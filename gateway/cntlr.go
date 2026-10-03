package gateway

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md, Cntlrs / gateway.md, Cntlrs and inspects: the
// three cntlr mutators and the two Inspect RPCs.
//
// A Cntlr is the SP's presence on one controller node: the record that makes
// a cn agent build the stack of architecture.md, Primary cntlr, for the pool
// and export its namespaces. The
// three mutators therefore all maintain the same three things together — the
// SpConf's `cntlr_id_list`, the CN's budget and pointer list, and the
// `nvme_tr_conf_list` of every CdcEntry of the SP — because a discovery entry
// that advertises a controller the SP no longer has (or hides one it does)
// sends hosts at a node that will refuse them.
//
// The two Inspect RPCs are the opposite shape: they write nothing, resolve
// everything the agent request needs in one Snapshot and make the single
// agent call strictly outside it (AG1; architecture.md, STM discipline).

// The op names the bump helpers and the agent-failure messages cite; they are
// the RPC names so a log line names something greppable (gateway.md, Log
// records).
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
// The id list is the existence check (architecture.md, Cntlrs: "NOT_FOUND id
// not in list") and the key is only then read: the list is what bounds the SP
// and what Create/DeleteCntlr maintain, so an id that is not in it names
// nothing even if a stale key were still lying around. The reverse — a listed
// id whose key is gone — is a lost invariant key and therefore an ABORTED
// (architecture.md, UNEXPECTED_ERROR → `ABORTED`), not a user-visible
// NOT_FOUND.
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
// its CN, the CNs that are already excluded from the draw, and their
// locations, which tier 1 of the draw excludes as well (architecture.md,
// Per-operation allocation). The STM holds the pool's cntlrs as it reads them
// against SpCnAddrs: one on a CN the plan did not hold sends the round back to
// be planned again.
type cntlrPlan struct {
	Cid       uint64
	Cc        *pb.ClusterConf
	ExtCnt    uint64
	SpCnAddrs []string
	SpCnLocs  []string
}

// planCreateCntlr is CreateCntlr's planning read (gateway.md, Cntlrs and
// inspects): the candidate scan needs a size and an exclusion list, and
// neither can be computed without reading the SP.
//
// The size is the SP's footprint — Σ ext_cnt over every group of every slice
// (architecture.md, Storage pools) — which is what one cntlr's CN reserves, so
// the slices must be read to size the scan at all. The exclusion list is the
// addr_port of every CN that already hosts a cntlr of this SP
// (architecture.md, Finding CN candidates), and the location of each of those
// CNs is the tier-1 exclusion of architecture.md, Per-operation allocation
// (cnLocations).
//
// The SP is read in one read-only Snapshot opened with openSp, not
// openSpRead: this is a MUTATOR's planning read, so GW5's deleting gate and
// then GW6's token, when one was sent, are checked here, before the scan this
// read feeds. Left to the deciding STM, both would come after the scan: with
// no CN left to draw, a stale client would hear RESOURCE_EXHAUSTED, computed
// against cntlrs it has not read, and hear it again after refreshing its
// token, and a pool whose deletion has begun would answer RESOURCE_EXHAUSTED
// rather than "is being deleted". The deciding STM checks both again (AG4);
// this read only fixes which refusal such a caller is given.
//
// The snapshot serves every read at one store revision, so a slice or cntlr
// its SpConf lists and the store lacks is a lost invariant key, never a torn
// read, and loadSlices and loadCntlrs answer it ABORTED. The locations are
// read after the snapshot, outside it, for the reasons cnLocations gives. The
// plan only shapes the scan: the STM resolves the SP again (architecture.md,
// STM discipline), recomputes the footprint from the slices it reads and
// re-checks the plan's cntlrs.
func planCreateCntlr(
	ctx context.Context,
	cli *etcdutil.Client,
	req *pb.CreateCntlrRequest,
) (cntlrPlan, error) {
	var plan cntlrPlan
	err := cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		plan = cntlrPlan{}
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		slices, err := loadSlices(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		cntlrs, err := loadCntlrs(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		addrs := make([]string, 0, len(cntlrs))
		for _, cntlr := range cntlrs {
			addrs = append(addrs, cntlr.GetAddrPort())
		}
		plan = cntlrPlan{
			Cid:       sc.Cid,
			Cc:        sc.Cc,
			ExtCnt:    spFootprint(slices),
			SpCnAddrs: addrs,
		}
		return nil
	})
	if err != nil {
		return cntlrPlan{}, mapStmErr(err)
	}
	locs, err := cnLocations(ctx, cli, plan.Cid, plan.SpCnAddrs)
	if err != nil {
		return cntlrPlan{}, err
	}
	plan.SpCnLocs = locs
	return plan, nil
}

// CreateCntlr is the CreateCntlr of architecture.md, Cntlrs: it adds one
// standby controller to an SP on a CN that hosts none of the SP's cntlrs yet.
// The sp-worker's next SyncupSide round tells every side about the new standby
// and the sides grow an export for it (architecture.md, Disk node), which is
// why the whole RPC is one SpRev bump away from being visible.
//
// It allocates, so it is a candidate unit (GW9): the CN scan runs outside the
// transaction and the transaction re-validates the pick. The scan needs a
// size, and a cntlr reserves the SP's whole footprint on its CN, so one round
// is planning read → scan → commit. The planning read opens the pool as the
// STM does, so a deleting pool and a stale token are refused before the scan
// (planCreateCntlr). Nothing else it produced is trusted: the STM recomputes
// the footprint from the slices IT reads and hands it to cnLedger.verifyPick,
// so a pick that no longer covers the current footprint — because the SP
// grew, or because the CN was charged by someone else — fails the unit as
// errCandidateChanged and the whole round runs again (GW9).
//
// The already-used cntlid slots are read from the SP's own cntlrs rather than
// tracked in a separate list: the Cntlr records are the only place a slot is
// stored, so they cannot disagree with anything (architecture.md, cntlid
// slots).
//
// The CN exclusion and the tier-1 locations the scan was given come from the
// pool's cntlrs as the plan read them, and a cntlr committed after that read
// would make both stale. A TOKEN-CARRYING caller never gets that far: whatever
// committed that cntlr — another CreateCntlr, the worker's AR7 replacement —
// bumped SpRev, which fails the token at the STM's openSp (GW6). GW6 is
// presence-based, so a token-LESS CreateCntlr is not serialized by it; the STM
// therefore compares the cntlrs it reads with the plan and re-plans on one the
// plan did not see (below), which keeps architecture.md, Finding CN
// candidates, and the tier 1 of architecture.md, Per-operation allocation,
// whole for every caller.
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
		plan, err := planCreateCntlr(ctx, s.cli, req)
		if err != nil {
			return err
		}
		cand, err := pickCn(
			ctx, s.cli, plan.Cid, plan.Cc, plan.ExtCnt,
			req.GetCnSelector(), nil, plan.SpCnAddrs, plan.SpCnLocs,
			opCreateCntlr)
		if err != nil {
			return err
		}
		return s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
			// Reassigned on every attempt: the id comes from the next_id
			// this attempt read, so a retry that reads a different next_id
			// must not reply the previous attempt's value (GW8).
			cntlrId = 0
			sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
				req.GetSpRev())
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
			// The pick was scanned against the pool's cntlrs as the round's
			// plan read them: their CNs for architecture.md, Finding CN
			// candidates, and their locations for the tier 1 of
			// architecture.md, Per-operation allocation. A cntlr committed
			// since — by another CreateCntlr, or the worker's AR7 replacement
			// — is missing from that plan, so the pick may sit in its failure
			// domain behind a capacity key verifyPick still finds, or on its
			// very CN, when the scan ran after that cntlr's charge. Only the
			// cntlrs this transaction reads can tell, so a cntlr on a CN the
			// plan did not hold makes the round a changed candidate (GW9) and
			// the next round plans from the pool as it now stands. Only a gain
			// is checked: a cntlr that has left since the plan frees room the
			// round did not count on, which GW9 never re-scans for. Only a
			// token-less request gets here with a gained cntlr: the gain's
			// SpRev bump fails a sent token at openSp above. The check runs
			// after the slot checks, so a request that can never commit is
			// refused rather than re-planned.
			for _, cntlr := range cntlrs {
				if !containsName(plan.SpCnAddrs, cntlr.GetAddrPort()) {
					return errCandidateChanged
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
			// that the election of architecture.md, Automatic reactions, may
			// later promote, and it is enabled
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

// DeleteCntlr is the DeleteCntlr of architecture.md, Cntlrs: it removes one
// cntlr record, after which the sides drop its export and the cn agent tears
// its stack down.
//
// It refuses a primary and refuses an enabled cntlr (`primary == true` or
// `disabled == false` ⇒ FAILED_PRECONDITION): disabling is what triggers the
// re-election of architecture.md, Automatic reactions, and takes the
// controller's namespaces ANA-inaccessible, so requiring the disable first
// means a failover has already happened by the
// time the record disappears — hosts have moved before their paths do.
//
// Everything CreateCntlr did is undone, item for item, so the two are
// auditable against each other: the id leaves the list, the key goes,
// the CN gets its pointer and its footprint back through one flush (one write,
// one capacity-key maintenance, one CnRev bump), the transport address leaves
// every CdcEntry, and SpRev bumps once. The footprint returned is the one
// recomputed from the SP's slices now, which is exactly what is reserved:
// GrowSlice charges its delta to every cntlr's CN (architecture.md,
// GrowSlice), so the reservation tracks the current geometry rather than the
// geometry at creation time.
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
			req.GetSpRev())
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

// UpdateCntlrEnabled is the UpdateCntlrEnabled of architecture.md, Cntlrs: the
// flag that takes a controller out of, or back into, service.
//
// Disabling removes the cntlr from primary eligibility and makes its
// namespaces ANA-inaccessible, so its CN must stop being advertised in the
// SP's CdcEntries at the same instant (architecture.md, Subsystems,
// namespaces) — a host that discovers a disabled controller finds only
// inaccessible paths there. Enabling puts the
// address back. Disabling the last enabled cntlr is allowed and stops IO;
// that is the operator's call to make. dnvctl does NOT warn about it in v1: the
// warning would need a pre-read, and dnvctl issues no RPC the operator did not
// type (dnvctl.md CT8). It is deferred until this reply carries the hint.
//
// Enabling a cntlr that is still the primary also marks it settling
// (dnv-worker.md HL2): its cn agent converged the standby shape while it was
// disabled (cnagent.md CN9) and now builds the primary stack from it, the work
// a promotion does, so AR5 must not judge that build by primary_unhealthy
// alone.
//
// A request that asks for the state already stored writes NOTHING and bumps
// NOTHING (GW6): a no-op that bumped SpRev would invalidate every client's
// token and make every agent re-sync for a change that did not happen. A token
// that was sent is still checked first (GW6), so a stale client hears ABORTED
// rather than a misleading OK.
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
			req.GetSpRev())
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
			// A re-enabled primary settles again (HL2), as a promoted one.
			if cntlr.GetPrimary() {
				cntlr.Settling = true
			}
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

// InspectCntlr is the InspectCntlr of architecture.md, Cntlrs: the live state
// of one cntlr as its cn agent sees it, which is how an operator watches a
// clone hydrate before calling DeleteClone (architecture.md, Clones).
//
// Phase 1 is a Snapshot and not a RunSTM because the RPC writes nothing: one
// store revision makes the Cntlr and the CN it names mutually consistent
// (architecture.md, STM discipline; GW8). The CnConf is read for its `cn_id`
// alone — the Cntlr stores the addr_port to dial, the agent addresses the node
// by id — and its absence is an ABORTED (architecture.md, UNEXPECTED_ERROR →
// `ABORTED`) rather than NOT_FOUND, because DeleteControllerNode refuses a CN
// whose `cntlr_ptr_list` is non-empty, so a live cntlr pointing at a missing
// CnConf is a lost invariant key.
//
// The reply is the agent's, whole: `applied_revision` (the revision of
// the last SyncupCntlr the agent accepted for this cntlr) and `cntlr_info`
// both come from the GetCntlrInfo reply (architecture.md, Cntlrs) — diff
// `applied_revision` against the SpRev token
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

// InspectSide is the InspectSide of architecture.md, Cntlrs: the live state of
// one side as its dn agent sees it, which is how an operator watches a
// migration hydrate before calling FinishMigration (architecture.md,
// Migrations).
//
// It is InspectCntlr's shape with the object named differently. The side is
// located by the bounded slice scan of findSide — every slice, every meta and
// data group, active legs and spare legs alike — because a side_id is unique
// inside the SP but carries no hint of which slice holds it (architecture.md,
// Cntlrs); an unknown id is NOT_FOUND.
//
// The agent request names the side by the full SidePointer, not by side_id
// alone: the dn agent keys its objects by (sp_id, leg_id, side_id), so the
// leg the scan found travels with the id. The DnConf is read for its `dn_id`
// alone and its absence is ABORTED for the same reason InspectCntlr's CnConf
// is — DeleteDiskNode refuses a DN that still carries side pointers.
//
// The reply is the agent's, whole: `applied_revision` (the revision of
// the last SyncupSide the agent accepted for this side) and `side_info`
// both come from the GetSideInfo reply (architecture.md, Cntlrs) — diff
// `applied_revision` against the SpRev token
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
