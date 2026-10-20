package gateway

import (
	"context"
	"fmt"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md, Clones, the destination side of a copy. A
// Clone names one thin device of this SP as the destination of a dm-clone
// whose source is a namespace somewhere else; the primary cntlr does the
// actual work — connect the source, build the dm-clone, reload the td's
// namespaces onto it (fig. `090Clone`). The gateway only ever writes desired
// state and bumps SpRev, and never waits for a cntlr to act on it.
//
// Three of the five RPCs are the plain SP-scoped shapes of gateway.md, The
// handler pattern. CreateClone is a candidate unit (GW9), because the clone's
// clone meta group takes extents on DNs. DeleteClone is the other exception:
// with force = false it must PROVE the copy finished before it may take the
// dm-clone away, and that proof lives only on the primary's CN — hence the
// two-phase AG4 shape.

// The op names the bump helper cites; they are the RPC names so a log line
// names something greppable.
const (
	opCreateClone       = "CreateClone"
	opDeleteClone       = "DeleteClone"
	opUpdateCloneTrConf = "UpdateCloneTrConf"
	opAppendCloneBitmap = "AppendCloneBitmap"
)

// loadClone reads one Clone of an already resolved SP. Every clone RPC but
// CreateClone opens with it, so "absent ⇒ NOT_FOUND" (GW7) is stated once.
func loadClone(
	stm etcdutil.STM,
	sc *spScope,
	cloneName string,
) (*pb.Clone, error) {
	clone := &pb.Clone{}
	if !stm.Get(model.CloneKey(sc.Cid, sc.SpId(), cloneName), clone) {
		return nil, errNotFound("clone %q not found", cloneName)
	}
	return clone, nil
}

// loadLiveClone is loadClone plus CLD1: a clone whose teardown has begun
// accepts no further changes. DeleteClone is the only RPC allowed to act on
// one, and it acts as a no-op (gateway.md, Clones) — so every other clone
// MUTATOR opens with this instead, and the two read paths (GetClone, and
// DeleteClone's own phase
// 1) keep using loadClone.
//
// The AppendCloneBitmap half is not cosmetic: without it a racing append could
// write a chunk key behind the drain, and the final STM's emptiness guard —
// which rests on "after the latch, no chunk key can ever appear again" — would
// stop being stable (CLD9).
//
// CreateClone needs no check of its own: the surviving Clone key keeps
// same-name creation failing ALREADY_EXISTS until the final STM.
func loadLiveClone(
	stm etcdutil.STM,
	sc *spScope,
	cloneName string,
) (*pb.Clone, error) {
	clone, err := loadClone(stm, sc, cloneName)
	if err != nil {
		return nil, err
	}
	if clone.GetDeleting() {
		return nil, errPrecondition("clone %q is being deleted", cloneName)
	}
	return clone, nil
}

// cloneMetaUnitsOf is the metadata units a clone onto td is charged on the CN
// of each cntlr (architecture.md, Metadata units): common.CloneMetaUnits of
// the destination td's size and the SP's stored data_block_size. The
// stored bdev_conf is validated first and refused ABORTED (GW11): a zero block
// size read back out of the store would size every clone at one unit.
// CreateClone stores the count on the record, so every later charge and
// release reads it there rather than computing it again.
func cloneMetaUnitsOf(conf *pb.SpConf, td *pb.ThinDevice) (uint64, error) {
	bdev := spBdevConf(conf)
	if err := model.ValidateBdevConf(bdev); err != nil {
		return 0, errAborted("%v", err)
	}
	return common.CloneMetaUnits(td.GetSize(), model.PoolBlockSize(bdev)), nil
}

// cloneGrpPlan is the shape of a new clone's clone meta group
// (architecture.md, Clones): Legs legs, one per pick, each one side of
// common.CloneMetaGrpExtCnt extents, laid out as MetaBlocks and DataBlocks
// (architecture.md, Group on-leg layout: meta region, data region, health
// block).
type cloneGrpPlan struct {
	Legs       int
	MetaBlocks uint64
	DataBlocks uint64
}

// planCloneGrp sizes a new clone's clone meta group from the stored confs and
// refuses a destination whose dm-clone metadata it cannot hold. CreateClone's
// planning read and its deciding STM both run it, so the two give one refusal
// the same words.
//
// The legs are the pool's own redundancy's (legCntOf, the gateway's one place
// that chooses a leg count, dnv-worker.md SPD1): a clone meta group is a group
// of the pool, held to the rule every group of it follows. Both stored confs
// are validated first and refused ABORTED (GW11), the cluster's extent size
// and the pool's block size being what the layout and the bound are computed
// from. model.GroupBlocks of one extent cannot fail on a conf that validates:
// every slice of the pool has a one-extent meta group CreateStoragePool laid
// out from these same two write-once confs, so a failure is a lost invariant,
// ABORTED as well.
//
// The bound: the metadata a copy of the destination takes,
// common.CloneMetaBytes of its size and the block, must fit the data region
// of one side, DataBlocks blocks, the md layout having taken MetaBlocks. A
// destination that does not fit is FAILED_PRECONDITION, a property of the
// request against the pool's own geometry that no capacity frees, and the
// message names the bound, the largest destination size whose metadata still
// fits.
func planCloneGrp(
	cc *pb.ClusterConf,
	conf *pb.SpConf,
	td *pb.ThinDevice,
	cloneName string,
	tdName string,
) (cloneGrpPlan, error) {
	if err := model.ValidateClusterConf(cc); err != nil {
		return cloneGrpPlan{}, errAborted("%v", err)
	}
	bdev := spBdevConf(conf)
	if err := model.ValidateBdevConf(bdev); err != nil {
		return cloneGrpPlan{}, errAborted("%v", err)
	}
	metaBlocks, dataBlocks, err := model.GroupBlocks(
		common.CloneMetaGrpExtCnt, cc.GetDnBinConf().GetExtentSize(), bdev)
	if err != nil {
		return cloneGrpPlan{}, errAborted("%v", err)
	}
	blockSize := model.PoolBlockSize(bdev)
	room := dataBlocks * blockSize
	need := common.CloneMetaBytes(td.GetSize(), blockSize)
	if need > room {
		if room < common.CloneMetaBaseBytes {
			return cloneGrpPlan{}, errPrecondition(
				"clone %q: one side of a clone meta group holds %d bytes "+
					"of data, less than the %d-byte base of any dm-clone "+
					"metadata", cloneName, room, common.CloneMetaBaseBytes)
		}
		// need > room means td.size / blockSize > room - base, so this
		// product is at most td.size and cannot overflow.
		largest := (room-common.CloneMetaBaseBytes+1)*blockSize - 1
		return cloneGrpPlan{}, errPrecondition(
			"clone %q: the dm-clone metadata of the %d-byte destination "+
				"%q at a %d-byte block takes %d bytes, over the %d bytes "+
				"of data one side of a clone meta group holds; the largest "+
				"destination it holds is %d bytes",
			cloneName, td.GetSize(), tdName, blockSize, need, room, largest)
	}
	return cloneGrpPlan{
		Legs:       legCntOf(bdev),
		MetaBlocks: metaBlocks,
		DataBlocks: dataBlocks,
	}, nil
}

// cloneCreateChecks is every refusal CreateClone judges from what it reads,
// in its order, behind the resolution and the token check openSp makes: the
// MaxCloneCntPerSp ceiling, ahead of the name so a pool at the ceiling hears
// the ceiling; the name free; the destination td by name; a destination no
// other clone names; and the clone meta group's plan and bound
// (planCloneGrp). The planning read and the deciding STM both run it, so a
// refusal reached in either has the same words, and the planning read
// answers every one of them ahead of the scan (GW9).
func cloneCreateChecks(
	stm etcdutil.STM,
	sc *spScope,
	req *pb.CreateCloneRequest,
) (*pb.ThinDevice, cloneGrpPlan, error) {
	if len(sc.Conf.GetCloneNameList()) >= common.MaxCloneCntPerSp {
		return nil, cloneGrpPlan{}, errExhausted(
			"storage pool %q holds %d clones, the maximum is %d",
			req.GetSpName(), len(sc.Conf.GetCloneNameList()),
			common.MaxCloneCntPerSp)
	}
	cloneKey := model.CloneKey(sc.Cid, sc.SpId(), req.GetCloneName())
	// Both halves are checked: the list is what every other RPC walks, the
	// key is what actually holds the record, and a create must not overwrite
	// either one if they ever disagree.
	if containsName(sc.Conf.GetCloneNameList(), req.GetCloneName()) ||
		stm.Get(cloneKey, &pb.Clone{}) {
		return nil, cloneGrpPlan{}, errExists(
			"clone %q already exists", req.GetCloneName())
	}
	td := &pb.ThinDevice{}
	tdKey := model.ThinDeviceKey(sc.Cid, sc.SpId(), req.GetDstTdName())
	if !stm.Get(tdKey, td) {
		return nil, cloneGrpPlan{}, errNotFound(
			"thin device %q not found", req.GetDstTdName())
	}
	// One td can be the destination of at most one clone: two dm-clones
	// writing the same raid0 would each believe they own its regions. The
	// scan is bounded by MaxCloneCntPerSp.
	for _, name := range sc.Conf.GetCloneNameList() {
		otherKey := model.CloneKey(sc.Cid, sc.SpId(), name)
		other := &pb.Clone{}
		if !stm.Get(otherKey, other) {
			return nil, cloneGrpPlan{}, errAborted(
				"clone key %q is missing", otherKey)
		}
		if other.GetDstTdId() == td.GetTdId() {
			return nil, cloneGrpPlan{}, errPrecondition(
				"thin device %q is already the destination of clone %q",
				req.GetDstTdName(), name)
		}
	}
	plan, err := planCloneGrp(
		sc.Cc, sc.Conf, td, req.GetCloneName(), req.GetDstTdName())
	if err != nil {
		return nil, cloneGrpPlan{}, err
	}
	return td, plan, nil
}

// CreateClone is the CreateClone of architecture.md, Clones: one Clone record
// carrying its clone meta group, one name in `clone_name_list`, one SpRev
// bump. Everything the primary needs to build the dm-clone is in that record,
// so the RPC calls no agent.
//
// It is a GW9 candidate unit, because the clone meta group is a new group of
// the pool and its sides take extents on DNs (architecture.md, Per-operation
// allocation). Each round plans from a read-only snapshot opened with openSp,
// so a stale token is ABORTED before anything else, and that snapshot runs
// every refusal of cloneCreateChecks, the bound included, ahead of the scan.
// The DN scan
// runs outside every transaction and places the group as architecture.md,
// Per-operation allocation, places a clone's — one DN per location among the
// legs, the black list empty and no location excluded, so the legs may land
// on DNs the pool's other groups already use — and fewer candidates than legs
// is RESOURCE_EXHAUSTED. The deciding STM runs the same checks again and
// drops the round as candidate changed when the cluster id or the leg count
// is no longer the one the scan was drawn for, or when a pick's capacity key
// has moved (dnLedger.verifyPick).
//
// One of the section's rules is deliberately NOT enforced here. The
// destination td MUST be empty, freshly created and never written ([D3]): a
// source chunk the cn agent discards marks its regions copied without a
// copy, so each such region then reads the destination's own blocks, which
// are zeros only in a thin device that was never written. The CP cannot see
// whether a td was written, so that stays a documented-unverifiable
// contract.
//
// The clone's metadata units (cloneMetaUnitsOf) are charged to the CN of
// EVERY cntlr of the SP, disabled ones included (architecture.md, Metadata
// units), though no CN takes a unit for the clone, which keeps its dm-clone
// metadata on its clone meta group. A CN short of the room refuses the
// create RESOURCE_EXHAUSTED before anything is written. The units are stored
// on the record, where every later charge and release reads them. The charge
// changes only the CNs' used units, so it rewrites each CN's record and
// capacity value and bumps no CnRev.
//
// The clone meta group's sides are charged to their DNs through the DN
// ledger, each DN written once with its pointer and its extents and bumped
// once, as CreateStoragePool charges a new group's; no CN is charged extents
// for it, since no cntlr's footprint counts it. Its ids come from the pool's
// counter after the clone's own, in GW15's order: the grp_id, then per pick
// a leg_id and its side_id, so a re-run of the closure on the same picks
// writes the same set (GW8).
func (s *Server) CreateClone(
	ctx context.Context,
	req *pb.CreateCloneRequest,
) (*pb.CreateCloneReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("clone_name", req.GetCloneName()); err != nil {
		return nil, err
	}
	if err := validateName("dst_td_name", req.GetDstTdName()); err != nil {
		return nil, err
	}
	if err := validateTrConfList(
		"src_tr_conf", req.GetSrcTrConf(),
	); err != nil {
		return nil, err
	}
	if err := validateNqn("src_nqn", req.GetSrcNqn()); err != nil {
		return nil, err
	}
	if err := validateCloneGeometry(
		req.GetSrcSliceCnt(), req.GetSrcStripeSize(), req.GetSrcBlockSize(),
	); err != nil {
		return nil, err
	}
	if err := validateDmCloneConf(
		"dm_clone_conf", req.GetDmCloneConf(),
	); err != nil {
		return nil, err
	}
	what := fmt.Sprintf("clone %q", req.GetCloneName())
	var cloneId uint64
	err := candidateUnit(ctx, func() error {
		// The plan: which cluster to scan and how many legs to place.
		var (
			planCid  uint64
			planCc   *pb.ClusterConf
			planLegs int
		)
		err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
			// openSp, not openSpRead: this is a MUTATOR's planning read, so
			// GW6 wants the token checked before any other state check.
			sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
				req.GetSpRev())
			if err != nil {
				return err
			}
			_, plan, err := cloneCreateChecks(stm, sc, req)
			if err != nil {
				return err
			}
			planCid = sc.Cid
			planCc = sc.Cc
			planLegs = plan.Legs
			return nil
		})
		if err != nil {
			return mapStmErr(err)
		}
		// A clone meta group's placement (architecture.md, Per-operation
		// allocation): no black list and no location excluded, the scan's
		// own one-DN-per-location rule spreading the legs. The request names
		// no DN selector.
		picks, err := pickDns(
			ctx, s.cli, planCid, planCc,
			dnPickPlan{
				ExtCnt:      common.CloneMetaGrpExtCnt,
				Legs:        planLegs,
				ExcludeLocs: nil,
			},
			nil, nil, what,
		)
		if err != nil {
			return err
		}
		return s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
			cloneId = 0
			sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
				req.GetSpRev())
			if err != nil {
				return err
			}
			td, plan, err := cloneCreateChecks(stm, sc, req)
			if err != nil {
				return err
			}
			if sc.Cid != planCid || plan.Legs != len(picks) {
				// The scan planned for another cluster or another leg
				// count; its picks describe a group this transaction is
				// not building.
				return errCandidateChanged
			}
			// The metadata room comes after every refusal of cloneCreateChecks,
			// so a request that breaks a rule of its own hears that rule, and
			// before any write: a CN short of room is RESOURCE_EXHAUSTED with
			// nothing written (architecture.md, Metadata units). One charge per
			// cntlr, each on the CN its record names, the ledger writing each
			// CN once.
			units, err := cloneMetaUnitsOf(sc.Conf, td)
			if err != nil {
				return err
			}
			cntlrs, err := loadCntlrs(stm, sc.Cid, sc.Conf)
			if err != nil {
				return err
			}
			cnl := newCnLedger(stm, sc.Cid)
			for _, cntlr := range cntlrs {
				err := cnl.chargeUnits(cntlr.GetAddrPort(), units, what)
				if err != nil {
					return err
				}
			}
			slots := sc.Conf.GetCntlidSlotList()
			if len(slots) == 0 {
				// CreateStoragePool stores a list and the slot list update
				// refuses an empty one, so this is unreachable; a side
				// cannot be written without a slot (architecture.md,
				// cntlid slots).
				return errPrecondition(
					"storage pool %q has an empty cntlid_slot_list",
					req.GetSpName())
			}
			// Every pick is verified and every charge staged before the
			// first Put, so a moved capacity key returns having written
			// nothing.
			dnl, err := newDnLedger(stm, sc.Cid, sc.Cc)
			if err != nil {
				return err
			}
			dns := make([]*pb.DnConf, 0, len(picks))
			for _, cand := range picks {
				dn, err := dnl.verifyPick(
					cand, common.CloneMetaGrpExtCnt, 0)
				if err != nil {
					return err
				}
				dns = append(dns, dn)
			}
			minter := newSpIdMinter(sc.Conf)
			cloneId = minter.mint()
			grp := &pb.Group{
				GrpId:      minter.mint(),
				ExtCnt:     common.CloneMetaGrpExtCnt,
				MetaBlocks: plan.MetaBlocks,
				DataBlocks: plan.DataBlocks,
			}
			for idx, cand := range picks {
				legId := minter.mint()
				sideId := minter.mint()
				grp.LegList = append(grp.LegList, &pb.Leg{
					LegId:  legId,
					LegIdx: uint32(idx),
					SideList: []*pb.Side{{
						SideId:     sideId,
						AddrPort:   cand.AddrPort,
						CntlidSlot: slots[0],
						NvmeTrConf: dns[idx].GetNvmeTrConf(),
						ErrEpoch:   0,
						// [D15]: a new side starts unprovisioned
						// (architecture.md, Side provisioning protocol).
						Provisioned: false,
					}},
				})
				err := dnl.charge(
					cand.AddrPort,
					&pb.SidePointer{
						SpId:   sc.SpId(),
						LegId:  legId,
						SideId: sideId,
					},
					common.CloneMetaGrpExtCnt)
				if err != nil {
					return err
				}
			}
			if err := minter.commit(sc.Conf, opCreateClone); err != nil {
				return err
			}
			stm.Put(
				model.CloneKey(sc.Cid, sc.SpId(), req.GetCloneName()),
				&pb.Clone{
					CloneId:       cloneId,
					SrcTrConfList: req.GetSrcTrConf(),
					SrcNqn:        req.GetSrcNqn(),
					SrcNsIdx:      req.GetSrcNsIdx(),
					SrcSliceCnt:   req.GetSrcSliceCnt(),
					SrcStripeSize: req.GetSrcStripeSize(),
					SrcBlockSize:  req.GetSrcBlockSize(),
					DstTdId:       td.GetTdId(),
					DmCloneConf:   req.GetDmCloneConf(),
					AutoResume:    req.GetAutoResume(),
					MetaUnits:     units,
					MetaGrp:       grp,
				})
			if err := cnl.flush(opCreateClone); err != nil {
				return err
			}
			if err := dnl.flush(opCreateClone); err != nil {
				return err
			}
			sc.Conf.CloneNameList = append(
				sc.Conf.CloneNameList, req.GetCloneName())
			stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), sc.Conf)
			return bumpSp(stm, opCreateClone, sc)
		})
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.CreateCloneReply{CloneId: cloneId}, nil
}

// clonePhase1 is what DeleteClone's read-only phase carries into its agent
// call. AG4 makes every field a hint and nothing more: phase 2 re-resolves all
// of it and re-checks the token, so for a token-carrying caller an interleaved
// mutation turns into ABORTED rather than into a decision taken on stale
// facts. A token-less caller keeps the re-resolution but not that guarantee
// (GW6 is presence-based).
type clonePhase1 struct {
	ClusterId uint64
	SpId      uint64
	CntlrId   uint64
	CnId      uint64
	AddrPort  string
	CloneId   uint64
	// Latched is CLD3's short-circuit: the clone was ALREADY being deleted
	// when phase 1 read it, so the RPC returns OK without the agent call and
	// without a deciding STM.
	Latched bool
}

// DeleteClone is the DeleteClone of architecture.md, Clones, the two-phase RPC
// of AG4: a read-only Snapshot, then — for force = false — one GetCntlrInfo
// against the primary's CN agent, then the deciding STM.
//
// force = false is a proof obligation, not a best effort. Removing the
// dm-clone while regions are still unhydrated would silently lose every
// byte that was never pulled from the source, so the RPC refuses unless the
// primary's `clone_id_to_dm_clone` row, in its agent's latest observation of
// the cntlr, shows the copy complete (checkCloneHydrated). An unreachable
// agent is FAILED_PRECONDITION for exactly the same reason (architecture.md,
// Clones): silence is not proof. force = true skips the whole check and is
// how an operator abandons a clone whose source is gone.
//
// The deciding STM also resumes the destination's namespaces: architecture.md,
// Transfer + clone = cross-SP live migration, has them
// suspended (created that way, or flipped) before the clone is made, and the
// clone record is the only thing that remembers why. The resume rides the
// LATCH (CLD4), not the final STM that removes the clone: clearing `suspended`
// in the same transaction that sets `deleting` makes it and CLD5's fan-out
// exclusion arrive in one SpRev bump, hence one syncup. Deferred to the end of
// the drain, the namespace would sit ANA-inaccessible for the whole teardown.
func (s *Server) DeleteClone(
	ctx context.Context,
	req *pb.DeleteCloneRequest,
) (*pb.DeleteCloneReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("clone_name", req.GetCloneName()); err != nil {
		return nil, err
	}
	var phase1 clonePhase1
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		phase1 = clonePhase1{}
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		// GW6 in PHASE 1, not only in the deciding STM: openSpRead skips the
		// check, and the table order of gateway.md, Clones, puts a stale token
		// ahead of every other answer this RPC can give — including the
		// `deleting` row, which decides here. AG4 still re-checks in phase 2;
		// this only makes a stale client see ABORTED before the agent call
		// rather than after it.
		if _, err := checkSpToken(
			stm, sc.Cid, sc.Conf, req.GetSpRev(),
		); err != nil {
			return err
		}
		clone, err := loadClone(stm, sc, req.GetCloneName())
		if err != nil {
			return err
		}
		phase1.CloneId = clone.GetCloneId()
		if clone.GetDeleting() {
			// CLD3: already latched ⇒ OK here, BEFORE the primary is resolved
			// and before any agent call. Two reasons, and the first is not an
			// optimization: after the latch the CN has retired the stack, so
			// `GetCntlrInfo` no longer reports the dm-clone and a hydration
			// check would wedge every repeat delete in FAILED_PRECONDITION for
			// ever. The second is SPD3's: a second bump would only force a
			// pointless re-resolve. The token check above has already run, so
			// a client that lost the race sees ABORTED rather than OK.
			phase1.Latched = true
			return nil
		}
		if req.GetForce() {
			// No agent call follows, so the primary need not even exist:
			// force is what deletes a clone whose cntlr is unreachable.
			return nil
		}
		cntlrs, err := loadCntlrs(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		cntlrId, cntlr, ok := primaryCntlr(sc.Conf, cntlrs)
		if !ok {
			return errPrecondition(
				"storage pool %q has no primary cntlr to prove hydration "+
					"with", req.GetSpName())
		}
		cn := &pb.CnConf{}
		cnKey := model.CnConfKey(sc.Cid, cntlr.GetAddrPort())
		if !stm.Get(cnKey, cn) {
			// GW7: NOT_FOUND is for an object the REQUEST named. This CN
			// is named by a live cntlr of the SP, and DeleteControllerNode
			// refuses a CN whose cntlr_ptr_list is non-empty, so its absence
			// is a lost invariant key — ABORTED (architecture.md,
			// UNEXPECTED_ERROR → `ABORTED`).
			return errAborted("cn_conf key %q is missing", cnKey)
		}
		phase1 = clonePhase1{
			ClusterId: sc.Cid,
			SpId:      sc.SpId(),
			CntlrId:   cntlrId,
			CnId:      cn.GetCnId(),
			AddrPort:  cntlr.GetAddrPort(),
			CloneId:   clone.GetCloneId(),
		}
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	if phase1.Latched {
		// CLD3: no agent call, no deciding STM, no writes.
		return &pb.DeleteCloneReply{CloneId: phase1.CloneId}, nil
	}
	if !req.GetForce() {
		if err := checkCloneHydrated(
			ctx, phase1, req.GetCloneName(),
		); err != nil {
			return nil, err
		}
	}
	var cloneId uint64
	err = s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		cloneId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		clone, err := loadClone(stm, sc, req.GetCloneName())
		if err != nil {
			return err
		}
		cloneId = clone.GetCloneId()
		if clone.GetDeleting() {
			// CLD3, the raced variant: the flag went up between phase 1 and
			// here. Same no-op — OK, no writes, no bump.
			return nil
		}
		if err := resumeCloneDstNs(stm, sc, clone.GetDstTdId()); err != nil {
			return err
		}
		// CLD4: the latch, and nothing else. The Clone key stays, its chunk
		// keys stay, and the name stays in clone_name_list — model.LoadSp
		// fetches clones by iterating that list, so a dangling name would
		// break every subsequent load.
		//
		// CLD6, SPD5's clone twin: no code path in any component writes
		// `deleting = false` on an existing Clone, so this is one-way across
		// restarts of every component. CLD9's emptiness argument depends on
		// it — an un-latch would let a chunk key appear after the scan that
		// found none — and so do CLD1's refusals.
		clone.Deleting = true
		stm.Put(model.CloneKey(sc.Cid, sc.SpId(), req.GetCloneName()), clone)
		return bumpSp(stm, opDeleteClone, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.DeleteCloneReply{CloneId: cloneId}, nil
}

// checkCloneHydrated is the between-the-phases agent call of DeleteClone
// (AG1, AG3): the primary's `clone_id_to_dm_clone` row carries the raw
// dm-clone status line, and hydrationComplete refuses to read anything but a
// finished copy out of it. GetCntlrInfo answers from the agent's latest
// observation of the cntlr and reads nothing of the node (architecture.md,
// Live-state reporting). That is still proof: a copy only advances, so an
// observation that lags the node shows less of it, and the refusal it gives
// is one the caller repeats.
//
// Both failure modes are FAILED_PRECONDITION rather than the usual ABORTED
// of AG3, because both mean the same thing to the caller: hydration could
// not be proven complete, so retry later or pass force (architecture.md,
// Clones).
func checkCloneHydrated(
	ctx context.Context,
	phase1 clonePhase1,
	cloneName string,
) error {
	details := ""
	callErr := withCnAgent(ctx, phase1.AddrPort,
		func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
			reply, err := client.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
				ClusterId: phase1.ClusterId,
				CnId:      phase1.CnId,
				CntlrPointer: &pb.CntlrPointer{
					SpId:    phase1.SpId,
					CntlrId: phase1.CntlrId,
				},
			})
			if err != nil {
				return err
			}
			details = reply.GetCntlrInfo().
				GetCloneIdToDmClone()[phase1.CloneId].GetDetails()
			return nil
		})
	if callErr != nil {
		return errPrecondition(
			"clone %q: hydration is unproven, controller node %q did not "+
				"answer: %v", cloneName, phase1.AddrPort, callErr)
	}
	if !hydrationComplete(details) {
		return errPrecondition(
			"clone %q has not finished hydrating", cloneName)
	}
	return nil
}

// resumeCloneDstNs clears `suspended` on every namespace of the SP backed by
// the clone's destination td (architecture.md, Clones: DeleteClone's Action).
// Only the subsystems that actually change are written back, so an SP with
// many subsystems produces the smallest write set that still describes the new
// desired state.
//
// A listed nqn whose Subsystem key is gone aborts the RPC (architecture.md,
// UNEXPECTED_ERROR → `ABORTED`): the SP has lost an invariant key, and
// half-resuming the namespaces of an SP is worse
// than refusing — the caller can retry, whereas a namespace left suspended
// with no clone to explain it never resumes on its own.
func resumeCloneDstNs(
	stm etcdutil.STM,
	sc *spScope,
	dstTdId uint64,
) error {
	for _, nqn := range sc.Conf.GetNqnList() {
		ssKey := model.SubsystemKey(sc.Cid, sc.SpId(), nqn)
		subsystem := &pb.Subsystem{}
		if !stm.Get(ssKey, subsystem) {
			return errAborted("subsystem key %q is missing", ssKey)
		}
		changed := false
		for _, ns := range subsystem.GetNsList() {
			if ns.GetTdId() != dstTdId || !ns.GetSuspended() {
				continue
			}
			ns.Suspended = false
			changed = true
		}
		if changed {
			stm.Put(ssKey, subsystem)
		}
	}
	return nil
}

// GetClone is the GetClone of architecture.md, Clones: one consistency read at
// a single store revision, no token and no bump.
func (s *Server) GetClone(
	ctx context.Context,
	req *pb.GetCloneRequest,
) (*pb.GetCloneReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("clone_name", req.GetCloneName()); err != nil {
		return nil, err
	}
	var clone *pb.Clone
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		clone = nil
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		found, err := loadClone(stm, sc, req.GetCloneName())
		if err != nil {
			return err
		}
		clone = found
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.GetCloneReply{Clone: clone}, nil
}

// UpdateCloneTrConf is the UpdateCloneTrConf of architecture.md, Clones: the
// source SP's cntlrs moved, so the addresses the primary reconnects to are
// replaced wholesale. The list is a replacement and never a merge — an address
// that is gone must stop being retried — and the bump is what tells the
// primary to reconnect.
func (s *Server) UpdateCloneTrConf(
	ctx context.Context,
	req *pb.UpdateCloneTrConfRequest,
) (*pb.UpdateCloneTrConfReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("clone_name", req.GetCloneName()); err != nil {
		return nil, err
	}
	if err := validateTrConfList(
		"src_tr_conf", req.GetSrcTrConf(),
	); err != nil {
		return nil, err
	}
	var cloneId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		cloneId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		clone, err := loadLiveClone(stm, sc, req.GetCloneName())
		if err != nil {
			return err
		}
		clone.SrcTrConfList = req.GetSrcTrConf()
		stm.Put(model.CloneKey(
			sc.Cid, sc.SpId(), req.GetCloneName()), clone)
		cloneId = clone.GetCloneId()
		return bumpSp(stm, opUpdateCloneTrConf, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.UpdateCloneTrConfReply{CloneId: cloneId}, nil
}

// AppendCloneBitmap is the AppendCloneBitmap of architecture.md, Clones: one
// chunk of the SOURCE bitmap, addressed by the PAIR (`src_slice_idx`,
// `bm_idx`).
//
// A chunk is addressed rather than allocated: `src_slice_idx` picks the source
// slice and `bm_idx` fixes the chunk's byte offset within that slice's bitmap
// at the FIXED quantum common.CloneBmChunkBytes — chunk (s, b) holds the bytes
// [b*C, b*C+len) of slice s's bitmap, len ≤ C. No chunk's meaning depends on
// any other chunk's existence or length, so a caller may append to any (s, b)
// at any time, in any order, and may leave chunks unsent entirely; an absent
// chunk reads as all-zero (= all-written) and a short chunk's missing tail
// reads as written, both the safe direction (architecture.md, Bitmap push
// protocol). WITHIN one chunk the page lands at the chunk's current length — a
// page is never a replacement, or paging would keep only the last one (the STM
// below) — which is why the pages of ONE chunk must arrive in slice-bitmap
// order.
//
// The Clone record carries no chunk count, and an append does not rewrite it:
// the set of a clone's chunks IS the set of its `CloneBitmap` keys, which is
// what the drain of dnv-worker.md, The clone drain, and `PushCloneBitmap` read
// (gateway.md, Clones). The clone key still enters this transaction's read set
// through `loadLiveClone` below, so the optimistic-concurrency guard is
// unchanged.
//
// The two index bounds are INVALID_ARGUMENT and not the RESOURCE_EXHAUSTED of
// GW7's Append*Bitmap row: they judge the indexes of THIS request against a
// geometry the clone was created with and against a compile-time constant. The
// chunk-overflow check in the STM IS that row's shape — a ceiling reached by
// previous appends, exactly like AppendMigrationBitmap's
// `bm_cnt ≥ MaxMigrBmCnt` — and is the one refusal here that is
// RESOURCE_EXHAUSTED. The
// stateless page cap is INVALID_ARGUMENT again, because a page longer than a
// whole chunk fits nowhere, whatever is already stored (architecture.md,
// Clones: AppendCloneBitmap's Errors).
//
// The bytes are stored exactly as sent: bitmaps are opaque to the gateway
// (GW14) — the 1 = never-written, LSB-first convention is the agents'
// and the callers'.
func (s *Server) AppendCloneBitmap(
	ctx context.Context,
	req *pb.AppendCloneBitmapRequest,
) (*pb.AppendCloneBitmapReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("clone_name", req.GetCloneName()); err != nil {
		return nil, err
	}
	if err := validateBitmap(req.GetBitmap()); err != nil {
		return nil, err
	}
	// Judged on this request alone, so it is here and NOT in validateBitmap:
	// that helper is shared with AppendMigrationBitmap, whose chunks carry no
	// byte cap of their own, and C is a clone chunk's capacity
	// (architecture.md, Clones).
	if len(req.GetBitmap()) > common.CloneBmChunkBytes {
		return nil, errInvalid(
			"bitmap is %d bytes, over the %d one clone bitmap chunk holds",
			len(req.GetBitmap()), common.CloneBmChunkBytes)
	}
	var cloneId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		cloneId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		clone, err := loadLiveClone(stm, sc, req.GetCloneName())
		if err != nil {
			return err
		}
		// src_slice_cnt is the whole slice bound: CreateClone already holds it
		// to MaxSliceCntPerSp (validateCloneGeometry), so a second constant
		// check here would judge nothing the geometry has not judged already.
		srcSliceIdx := req.GetSrcSliceIdx()
		if srcSliceIdx >= clone.GetSrcSliceCnt() {
			return errInvalid(
				"src_slice_idx %d is not below the clone's src_slice_cnt %d",
				srcSliceIdx, clone.GetSrcSliceCnt())
		}
		bmIdx := req.GetBmIdx()
		if bmIdx >= common.MaxCloneBmCnt {
			return errInvalid("bm_idx %d is not below %d",
				bmIdx, common.MaxCloneBmCnt)
		}
		// architecture.md, Clones, AppendCloneBitmap's Action: "append the
		// bytes to CloneBitmap key (src_slice_idx, bm_idx) (create if
		// absent)". The chunk grows in place; it is never replaced. Callers
		// page one source slice's bitmap through GetThinDeviceBitmap and hand
		// each page here, and the pages of one chunk concatenate into exactly
		// the bytes [b*C, b*C+len) that chunk holds of the slice's bitmap
		// (architecture.md, Bitmap push protocol; [D8]). Overwriting would
		// keep only the last page and place its bits at the chunk's own
		// offset, which PushCloneBitmap would then hand the primary as "these
		// blocks were never written" and the agent would blkdiscard regions
		// the source really wrote.
		//
		// The zero value of the read is what makes "create if absent" fall
		// out: an absent key decodes to an empty bitmap and the append is the
		// request's bytes alone. The same read is what the capacity check
		// needs — C bounds the STORED chunk, not one page — so a chunk that
		// previous appends already drove to its ceiling is RESOURCE_EXHAUSTED.
		bmKey := model.CloneBitmapKey(
			sc.Cid, sc.SpId(), req.GetCloneName(), srcSliceIdx, bmIdx)
		chunk := &pb.CloneBitmap{}
		stm.Get(bmKey, chunk)
		if len(chunk.GetBitmap())+len(req.GetBitmap()) >
			common.CloneBmChunkBytes {
			return errExhausted(
				"clone %q chunk (%d, %d) holds %d bytes, %d more would pass "+
					"the %d one chunk holds",
				req.GetCloneName(), srcSliceIdx, bmIdx,
				len(chunk.GetBitmap()), len(req.GetBitmap()),
				common.CloneBmChunkBytes)
		}
		chunk.Bitmap = append(chunk.GetBitmap(), req.GetBitmap()...)
		stm.Put(bmKey, chunk)
		// The Clone record itself is NOT rewritten: an append changes nothing
		// in it. `loadLiveClone` above already read the clone key, so it is in
		// this STM's read set and the conflict guard is the same as before.
		cloneId = clone.GetCloneId()
		return bumpSp(stm, opAppendCloneBitmap, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.AppendCloneBitmapReply{CloneId: cloneId}, nil
}
