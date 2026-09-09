package gateway

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md §8.6 / gateway.md §5.5: the three cntlr
// mutators and the two Inspect RPCs.
//
// A Cntlr is the SP's presence on one controller node: the record that makes
// a cn agent build the §3.3 stack for the pool and export its namespaces. The
// three mutators therefore all maintain the same three things together — the
// SpConf's `cntlr_id_list`, the CN's budget and pointer list, and the
// `nvme_tr_conf_list` of every CdcEntry of the SP — because a discovery entry
// that advertises a controller the SP no longer has (or hides one it does)
// sends hosts at a node that will refuse them.
//
// The two Inspect RPCs are the opposite shape: they write nothing, resolve
// everything the agent request needs in one Snapshot and make the single
// agent call strictly outside it (AG1, §5.8).

// The op names the bump helpers and the agent-failure messages cite; they are
// the RPC names so a log line names something greppable (gateway.md §8).
const (
	opCreateCntlr        = "CreateCntlr"
	opDeleteCntlr        = "DeleteCntlr"
	opUpdateCntlrEnabled = "UpdateCntlrEnabled"
	opInspectCntlr       = "InspectCntlr"
	opInspectSide        = "InspectSide"
)

// resolveCntlr reads one Cntlr of the SP and returns its key alongside it,
// because every caller that finds the record also rewrites or deletes that
// exact key.
//
// The id list is the existence check (§8.6: "NOT_FOUND id not in list") and
// the key is only then read: the list is what bounds the SP and what
// Create/DeleteCntlr maintain, so an id that is not in it names nothing even
// if a stale key were still lying around. The reverse — a listed id whose key
// is gone — is a lost invariant key and therefore §5.9's ABORTED, not a
// user-visible NOT_FOUND.
func resolveCntlr(
	stm etcdutil.STM,
	sc *spScope,
	cntlrId uint64,
) (string, *pb.Cntlr, error) {
	if !containsId(sc.Conf.GetCntlrIdList(), cntlrId) {
		return "", nil, errNotFound("cntlr %d not found", cntlrId)
	}
	key := model.CntlrKey(sc.Cid, sc.SpId(), cntlrId)
	cntlr := &pb.Cntlr{}
	if !stm.Get(key, cntlr) {
		return "", nil, errAborted("cntlr key %q is missing", key)
	}
	return key, cntlr, nil
}

// cntlrPlan is what CreateCntlr prepares outside its STM (GW8): the cluster
// the scan runs in, the number of extents one cntlr of this SP reserves on
// its CN, and the CNs that are already excluded from the draw.
type cntlrPlan struct {
	Cid       uint64
	Cc        *pb.ClusterConf
	ExtCnt    uint64
	SpCnAddrs []string
}

// planCreateCntlr is CreateCntlr's "plain pre-reads for planning"
// (gateway.md §5.4): the candidate scan needs a size and an exclusion list,
// and neither can be computed without reading the SP.
//
// The size is the SP's footprint — Σ ext_cnt over every group of every slice
// (§8.4) — which is what one cntlr's CN reserves, so the slices must be read
// to size the scan at all. The exclusion list is the addr_port of every CN
// that already hosts a cntlr of this SP (§6.4).
//
// These are plain reads, not a transaction: they only shape the scan, and the
// STM re-reads all of it authoritatively (§5.8). They can therefore observe a
// torn world — an SpConf read before a concurrent DeleteStoragePool and a
// slice key read after it — which is why a listed key that is not there is
// ABORTED here too: the RPC cannot size a scan against half an SP, and the
// caller's retry will find the SP gone and hear NOT_FOUND.
func planCreateCntlr(
	ctx context.Context,
	cli *etcdutil.Client,
	clusterName string,
	spName string,
) (cntlrPlan, error) {
	name := clusterNameOf(clusterName)
	cc := &pb.ClusterConf{}
	found, err := cli.Get(ctx, model.ClusterConfKey(name), cc)
	if err != nil {
		return cntlrPlan{}, errAborted("%v", err)
	}
	if !found {
		return cntlrPlan{}, errNotFound("cluster %q not found", name)
	}
	cid := model.ClusterId(name, cc.GetCreationEpoch())
	conf := &pb.SpConf{}
	found, err = cli.Get(ctx, model.SpConfKey(cid, spName), conf)
	if err != nil {
		return cntlrPlan{}, errAborted("%v", err)
	}
	if !found {
		return cntlrPlan{}, errNotFound("storage pool %q not found", spName)
	}
	slices := make([]*pb.Slice, 0, len(conf.GetSliceIdList()))
	for _, sliceId := range conf.GetSliceIdList() {
		key := model.SliceKey(cid, conf.GetSpId(), sliceId)
		slice := &pb.Slice{}
		found, err := cli.Get(ctx, key, slice)
		if err != nil {
			return cntlrPlan{}, errAborted("%v", err)
		}
		if !found {
			return cntlrPlan{}, errAborted("slice key %q is missing", key)
		}
		slices = append(slices, slice)
	}
	addrs := make([]string, 0, len(conf.GetCntlrIdList()))
	for _, cntlrId := range conf.GetCntlrIdList() {
		key := model.CntlrKey(cid, conf.GetSpId(), cntlrId)
		cntlr := &pb.Cntlr{}
		found, err := cli.Get(ctx, key, cntlr)
		if err != nil {
			return cntlrPlan{}, errAborted("%v", err)
		}
		if !found {
			return cntlrPlan{}, errAborted("cntlr key %q is missing", key)
		}
		addrs = append(addrs, cntlr.GetAddrPort())
	}
	return cntlrPlan{
		Cid:       cid,
		Cc:        cc,
		ExtCnt:    spFootprint(slices),
		SpCnAddrs: addrs,
	}, nil
}

// CreateCntlr is architecture.md §8.6's CreateCntlr: it adds one standby
// controller to an SP on a CN that hosts none of the SP's cntlrs yet. The
// sp-worker's next SyncupSide round tells every side about the new standby
// and the sides grow an export for it (§3.1), which is why the whole RPC is
// one SpRev bump away from being visible.
//
// It allocates, so it is a candidate unit (GW9): the CN scan runs outside the
// transaction and the transaction re-validates the pick. The scan needs a
// size, and a cntlr reserves the SP's whole footprint on its CN, so one round
// is pre-read → scan → commit. Nothing the pre-read produced is trusted: the
// STM recomputes the footprint from the slices IT reads and hands it to
// cnLedger.verifyPick, so a pick that no longer covers the current footprint —
// because the SP grew, or because the CN was charged by someone else — fails
// the unit as errCandidateChanged and the whole round runs again (§0 #8).
//
// The already-used cntlid slots are read from the SP's own cntlrs rather than
// tracked in a separate list: the Cntlr records are the only place a slot is
// stored, so they cannot disagree with anything (§11.8).
//
// The exclusion list the scan was given cannot go stale in a way that
// matters: every cntlr of one SP is created under that SP's revision token
// (GW6), so two CreateCntlrs on the same SP are serialized and the loser is
// ABORTED before it can put a second cntlr of the SP on one CN.
func (s *Server) CreateCntlr(
	ctx context.Context,
	req *pb.CreateCntlrRequest,
) (*pb.CreateCntlrReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if req.GetCntlidSlot() >= common.CnCntlidSlotCnt {
		return nil, errInvalid("cntlid_slot %d is not below %d",
			req.GetCntlidSlot(), common.CnCntlidSlotCnt)
	}
	if err := validateNodeSelector(
		"cn_selector", req.GetCnSelector(),
	); err != nil {
		return nil, err
	}
	var cntlrId uint64
	err := candidateUnit(ctx, func() error {
		plan, err := planCreateCntlr(
			ctx, s.cli, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		cand, err := pickCn(
			ctx, s.cli, plan.Cid, plan.Cc, plan.ExtCnt,
			req.GetCnSelector(), nil, plan.SpCnAddrs, opCreateCntlr)
		if err != nil {
			return err
		}
		return s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
			// Reassigned on every attempt: the id comes from the next_id
			// this attempt read, so a retry that reads a different next_id
			// must not reply the previous attempt's value (GW8).
			cntlrId = 0
			sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
				req.GetSpRev().GetRevision())
			if err != nil {
				return err
			}
			conf := sc.Conf
			if len(conf.GetCntlrIdList()) >= common.MaxCntlrCntPerSp {
				return errExhausted(
					"cntlr count %d has reached the per-storage-pool "+
						"maximum %d",
					len(conf.GetCntlrIdList()), common.MaxCntlrCntPerSp)
			}
			slot := req.GetCntlidSlot()
			allowed := false
			for _, item := range conf.GetCntlidSlotList() {
				// At most CnCntlidSlotCnt (8) entries, so a linear scan is
				// the right shape.
				if item == slot {
					allowed = true
					break
				}
			}
			if !allowed {
				return errInvalid(
					"cntlid_slot %d is not in the storage pool's "+
						"cntlid_slot_list",
					slot)
			}
			cntlrs, err := loadCntlrs(stm, sc.Cid, conf)
			if err != nil {
				return err
			}
			for _, cntlr := range cntlrs {
				if cntlr.GetCntlidSlot() == slot {
					return errInvalid(
						"cntlid_slot %d is already used by another cntlr "+
							"of storage pool %q",
						slot, req.GetSpName())
				}
			}
			slices, err := loadSlices(stm, sc.Cid, conf)
			if err != nil {
				return err
			}
			extCnt := spFootprint(slices)
			ledger := newCnLedger(stm, sc.Cid)
			cn, err := ledger.verifyPick(cand, extCnt)
			if err != nil {
				return err
			}
			minter := newSpIdMinter(conf)
			cntlrId = minter.mint()
			// primary and disabled are both false: a new cntlr is a standby
			// that the §10.4 election may later promote, and it is enabled
			// from birth, which is what puts its CN into every CdcEntry
			// below.
			stm.Put(model.CntlrKey(sc.Cid, sc.SpId(), cntlrId), &pb.Cntlr{
				AddrPort:   cand.AddrPort,
				NvmeTrConf: cn.GetNvmeTrConf(),
				CntlidSlot: slot,
				Primary:    false,
				Disabled:   false,
			})
			conf.CntlrIdList = append(conf.GetCntlrIdList(), cntlrId)
			if err := ledger.charge(
				cand.AddrPort,
				&pb.CntlrPointer{SpId: sc.SpId(), CntlrId: cntlrId},
				extCnt,
			); err != nil {
				return err
			}
			if err := ledger.flush(opCreateCntlr); err != nil {
				return err
			}
			if err := addCdcTrConf(
				stm, sc, cn.GetNvmeTrConf(),
			); err != nil {
				return err
			}
			minter.commit(conf)
			stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), conf)
			return bumpSp(stm, opCreateCntlr, sc)
		})
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.CreateCntlrReply{CntlrId: cntlrId}, nil
}

// DeleteCntlr is architecture.md §8.6's DeleteCntlr: it removes one cntlr
// record, after which the sides drop its export and the cn agent tears its
// stack down.
//
// It refuses a primary and refuses an enabled cntlr (`primary == true` or
// `disabled == false` ⇒ FAILED_PRECONDITION): disabling is what triggers the
// §10.4 re-election and takes the controller's namespaces ANA-inaccessible,
// so requiring the disable first means a failover has already happened by the
// time the record disappears — hosts have moved before their paths do.
//
// Everything CreateCntlr did is undone, item for item, so the two are
// auditable against each other: the id leaves the list, the key goes,
// the CN gets its pointer and its footprint back through one flush (one write,
// one capacity-key maintenance, one CnRev bump), the transport address leaves
// every CdcEntry, and SpRev bumps once. The footprint returned is the one
// recomputed from the SP's slices now, which is exactly what is reserved:
// GrowSlice charges its delta to every cntlr's CN (§8.5), so the reservation
// tracks the current geometry rather than the geometry at creation time.
func (s *Server) DeleteCntlr(
	ctx context.Context,
	req *pb.DeleteCntlrRequest,
) (*pb.DeleteCntlrReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev().GetRevision())
		if err != nil {
			return err
		}
		conf := sc.Conf
		key, cntlr, err := resolveCntlr(stm, sc, req.GetCntlrId())
		if err != nil {
			return err
		}
		if cntlr.GetPrimary() {
			return errPrecondition(
				"cntlr %d is the primary of storage pool %q",
				req.GetCntlrId(), req.GetSpName())
		}
		if !cntlr.GetDisabled() {
			return errPrecondition(
				"cntlr %d is enabled; disable it first",
				req.GetCntlrId())
		}
		slices, err := loadSlices(stm, sc.Cid, conf)
		if err != nil {
			return err
		}
		conf.CntlrIdList = removeId(conf.GetCntlrIdList(), req.GetCntlrId())
		stm.Del(key)
		ledger := newCnLedger(stm, sc.Cid)
		if err := ledger.release(
			cntlr.GetAddrPort(), sc.SpId(), req.GetCntlrId(),
			spFootprint(slices),
		); err != nil {
			return err
		}
		if err := ledger.flush(opDeleteCntlr); err != nil {
			return err
		}
		// A disabled cntlr's address is already out of every entry, so this
		// normally changes nothing; it runs anyway because "reverse
		// everything CreateCntlr did" must not depend on another RPC having
		// run, and dropping an address that is not there is a no-op.
		if err := dropCdcTrConf(
			stm, sc, cntlr.GetNvmeTrConf(),
		); err != nil {
			return err
		}
		stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), conf)
		return bumpSp(stm, opDeleteCntlr, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.DeleteCntlrReply{CntlrId: req.GetCntlrId()}, nil
}

// UpdateCntlrEnabled is architecture.md §8.6's UpdateCntlrEnabled: the flag
// that takes a controller out of, or back into, service.
//
// Disabling removes the cntlr from primary eligibility and makes its
// namespaces ANA-inaccessible, so its CN must stop being advertised in the
// SP's CdcEntries at the same instant (§8.8) — a host that discovers a
// disabled controller finds only inaccessible paths there. Enabling puts the
// address back. Disabling the last enabled cntlr is allowed and stops IO;
// that is the operator's call to make, and dnvctl warns about it.
//
// A request that asks for the state already stored writes NOTHING and bumps
// NOTHING (§0 #17): a no-op that bumped SpRev would invalidate every client's
// token and make every agent re-sync for a change that did not happen. The
// token is still checked first (GW6), so a stale client hears ABORTED rather
// than a misleading OK.
func (s *Server) UpdateCntlrEnabled(
	ctx context.Context,
	req *pb.UpdateCntlrEnabledRequest,
) (*pb.UpdateCntlrEnabledReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev().GetRevision())
		if err != nil {
			return err
		}
		key, cntlr, err := resolveCntlr(stm, sc, req.GetCntlrId())
		if err != nil {
			return err
		}
		want := !req.GetEnabled()
		if cntlr.GetDisabled() == want {
			return nil
		}
		cntlr.Disabled = want
		if req.GetEnabled() {
			err = addCdcTrConf(stm, sc, cntlr.GetNvmeTrConf())
		} else {
			err = dropCdcTrConf(stm, sc, cntlr.GetNvmeTrConf())
		}
		if err != nil {
			return err
		}
		stm.Put(key, cntlr)
		return bumpSp(stm, opUpdateCntlrEnabled, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.UpdateCntlrEnabledReply{
		CntlrId: req.GetCntlrId(),
		Enabled: req.GetEnabled(),
	}, nil
}

// InspectCntlr is architecture.md §8.6's InspectCntlr: the live state of one
// cntlr as its cn agent sees it, which is how an operator watches a clone
// hydrate before calling DeleteClone (§8.9).
//
// Phase 1 is a Snapshot and not a RunSTM because the RPC writes nothing: one
// store revision makes the Cntlr and the CN it names mutually consistent
// (§5.8, GW8). The CnConf is read for its `cn_id` alone — the
// Cntlr stores the addr_port to dial, the agent addresses the node by id —
// and its absence is §5.9's ABORTED rather than NOT_FOUND, because
// DeleteControllerNode refuses a CN whose `cntlr_ptr_list` is non-empty, so a
// live cntlr pointing at a missing CnConf is a lost invariant key.
//
// The reply is the agent's, whole: `applied_revision` (the revision of
// the last SyncupCntlr the agent applied for this cntlr) and `cntlr_info`
// both come from the GetCntlrInfo reply (architecture.md §8.6;
// update_04.md U4) — diff `applied_revision` against the SpRev token
// GetStoragePool hands out to see how far the agent lags desired state.
//
// The agent's reply is passed through as it comes: only a transport failure
// or a non-OK gRPC status is this RPC's ABORTED, because AG3 exempts the
// Get*Info calls from the AgentReply.code convention — an agent that does not
// know the cntlr yet (nothing has synced it up) answers "unknown object" with
// no CntlrInfo, and that is a truthful description of the node's live state,
// not a failure of the call.
func (s *Server) InspectCntlr(
	ctx context.Context,
	req *pb.InspectCntlrRequest,
) (*pb.InspectCntlrReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	var (
		cid      uint64
		spId     uint64
		cnId     uint64
		addrPort string
	)
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		// Reassigned on entry so the closure stays a pure function of what
		// it reads (GW8), whatever a previous attempt left behind.
		cid, spId, cnId, addrPort = 0, 0, 0, ""
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		_, cntlr, err := resolveCntlr(stm, sc, req.GetCntlrId())
		if err != nil {
			return err
		}
		cnKey := model.CnConfKey(sc.Cid, cntlr.GetAddrPort())
		cn := &pb.CnConf{}
		if !stm.Get(cnKey, cn) {
			return errAborted("cn_conf key %q is missing", cnKey)
		}
		cid = sc.Cid
		spId = sc.SpId()
		cnId = cn.GetCnId()
		addrPort = cntlr.GetAddrPort()
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	var appliedRevision uint64
	var info *pb.CntlrInfo
	err = withCnAgent(ctx, addrPort,
		func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
			reply, err := client.GetCntlrInfo(
				ctx,
				&pb.GetCntlrInfoRequest{
					ClusterId: cid,
					CnId:      cnId,
					CntlrPointer: &pb.CntlrPointer{
						SpId:    spId,
						CntlrId: req.GetCntlrId(),
					},
				},
			)
			if err != nil {
				return err
			}
			appliedRevision = reply.GetRevision()
			info = reply.GetCntlrInfo()
			return nil
		})
	if err != nil {
		return nil, errAborted("%s: controller node %q: %v",
			opInspectCntlr, addrPort, err)
	}
	return &pb.InspectCntlrReply{
		AppliedRevision: appliedRevision,
		CntlrInfo:       info,
	}, nil
}

// InspectSide is architecture.md §8.6's InspectSide: the live state of one
// side as its dn agent sees it, which is how an operator watches a migration
// hydrate before calling FinishMigration (§8.11).
//
// It is InspectCntlr's shape with the object named differently. The side is
// located by the bounded slice scan of findSide — every slice, every meta and
// data group, active legs and spare legs alike — because a side_id is unique
// inside the SP but carries no hint of which slice holds it (§8.6); an
// unknown id is NOT_FOUND.
//
// The agent request names the side by the full SidePointer, not by side_id
// alone: the dn agent keys its objects by (sp_id, leg_id, side_id), so the
// leg the scan found travels with the id. The DnConf is read for its `dn_id`
// alone and its absence is ABORTED for the same reason InspectCntlr's CnConf
// is — DeleteDiskNode refuses a DN that still carries side pointers.
//
// The reply is the agent's, whole: `applied_revision` (the revision of
// the last SyncupSide the agent applied for this side) and `side_info`
// both come from the GetSideInfo reply (architecture.md §8.6;
// update_04.md U4) — diff `applied_revision` against the SpRev token
// GetStoragePool hands out to see how far the agent lags desired state.
// The agent's SideInfo is passed through exactly as InspectCntlr passes
// its CntlrInfo (AG3).
func (s *Server) InspectSide(
	ctx context.Context,
	req *pb.InspectSideRequest,
) (*pb.InspectSideReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	var (
		cid      uint64
		dnId     uint64
		addrPort string
		pointer  *pb.SidePointer
	)
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		cid, dnId, addrPort, pointer = 0, 0, "", nil
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		slices, err := loadSlices(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		loc, ok := findSide(sc.Conf, slices, req.GetSideId())
		if !ok {
			return errNotFound("side %d not found", req.GetSideId())
		}
		dnKey := model.DnConfKey(sc.Cid, loc.Side.GetAddrPort())
		dn := &pb.DnConf{}
		if !stm.Get(dnKey, dn) {
			return errAborted("dn_conf key %q is missing", dnKey)
		}
		cid = sc.Cid
		dnId = dn.GetDnId()
		addrPort = loc.Side.GetAddrPort()
		pointer = &pb.SidePointer{
			SpId:   sc.SpId(),
			LegId:  loc.Leg.GetLegId(),
			SideId: req.GetSideId(),
		}
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	var appliedRevision uint64
	var info *pb.SideInfo
	err = withDnAgent(ctx, addrPort,
		func(ctx context.Context, client pb.DiskNodeAgentClient) error {
			reply, err := client.GetSideInfo(
				ctx,
				&pb.GetSideInfoRequest{
					ClusterId:   cid,
					DnId:        dnId,
					SidePointer: pointer,
				},
			)
			if err != nil {
				return err
			}
			appliedRevision = reply.GetRevision()
			info = reply.GetSideInfo()
			return nil
		})
	if err != nil {
		return nil, errAborted("%s: disk node %q: %v",
			opInspectSide, addrPort, err)
	}
	return &pb.InspectSideReply{
		AppliedRevision: appliedRevision,
		SideInfo:        info,
	}, nil
}
