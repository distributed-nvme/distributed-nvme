package gateway

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md §8.11 / gateway.md §5.10: the five RPCs that
// move one leg's side from one disk node to another while the volume keeps
// serving (§11.2).
//
// The whole feature rests on one temporary shape: between CreateMigration and
// Finish/Cancel the leg owns TWO sides, the source and the destination, both
// exporting the same NQN so every CN sees them as two paths of one multipath
// namespace. Every rule below follows from that — the destination must land on
// a disk node no leg of the group already uses and, whenever the cluster has
// one to offer, in a failure domain the group does not already occupy either
// (a destination in the domain the migration was meant to leave repairs
// nothing; §6.5's two tiers: the domain exclusion yields rather than refuse
// the migration altogether), it must take a cntlid slot the source does not
// hold (otherwise the two controllers a CN aggregates could pick the same
// CNTLID and the kernel would refuse the second path, §11.8), and exactly one
// of the two sides survives: Finish keeps the destination, Cancel keeps the
// source.

// The op names the ledger flushes and the bump helpers cite; they are the RPC
// names so a log record names something greppable. GetMigration writes
// nothing and therefore needs none.
const (
	opCreateMigration       = "CreateMigration"
	opFinishMigration       = "FinishMigration"
	opCancelMigration       = "CancelMigration"
	opAppendMigrationBitmap = "AppendMigrationBitmap"
)

// migrMaxLegSideCnt is how many sides one leg may own: its own, plus the
// destination of at most ONE migration (§8.11). A leg already at this count is
// mid-migration, so CreateMigration refuses it — two concurrent migrations of
// one leg would give the CN three paths to reconcile and leave no unambiguous
// survivor for FinishMigration to keep.
const migrMaxLegSideCnt = 2

// migrSideIdx is the position of one side inside a leg's side_list, or -1.
// FinishMigration uses it to assert that the two sides of a migration really
// do share one leg before it removes either of them.
func migrSideIdx(leg *pb.Leg, sideId uint64) int {
	for idx, side := range leg.GetSideList() {
		if side.GetSideId() == sideId {
			return idx
		}
	}
	return -1
}

// migrDropSide removes one side from a leg's side_list, which is how both
// terminal RPCs end a migration: Finish drops the source, Cancel the
// destination, and the survivor is the leg's only side again.
//
// Every caller has already located that side in this very leg, so a side that
// is not there is impossible; the guard is what keeps that impossibility from
// becoming a slice-index panic inside a gRPC handler.
func migrDropSide(leg *pb.Leg, sideId uint64) {
	idx := migrSideIdx(leg, sideId)
	if idx < 0 {
		return
	}
	sideList := leg.GetSideList()
	leg.SideList = append(sideList[:idx], sideList[idx+1:]...)
}

// migrDstCntlidSlot is the destination side's cntlid slot [D-I]: the first
// entry of the SP's cntlid_slot_list that differs from the source side's.
//
// §11.8 makes this the one slot constraint a side has. Sides of different legs
// may share slots freely — their subsystem NQNs differ — but the two sides of
// ONE leg are aggregated by every CN, so they must occupy different CNTLID
// ranges. An SP whose list offers no second value cannot migrate this leg at
// all; that is a standing property of its own configuration, hence
// FAILED_PRECONDITION rather than a candidate problem.
func migrDstCntlidSlot(conf *pb.SpConf, srcSlot uint32) (uint32, error) {
	for _, slot := range conf.GetCntlidSlotList() {
		if slot != srcSlot {
			return slot, nil
		}
	}
	return 0, errPrecondition(
		"cntlid_slot_list holds no slot other than the source side's %d",
		srcSlot)
}

// dropMigration is the teardown both FinishMigration and CancelMigration
// perform once they have decided which side survives: the Migration record,
// every chunk of its bitmap and its entry in the SP's name list disappear
// together, because a migration that no longer owns two sides can never be
// resumed.
//
// The chunks are deleted one key at a time from bm_idx 0 to bm_cnt-1: an STM
// cannot range (EU4), and bm_cnt is exactly the number of chunks
// AppendMigrationBitmap ever wrote, since every append takes the next index
// and a written chunk is immutable (§8.11).
//
// The caller still writes the SpConf and bumps SpRev — this helper only
// mutates the conf in memory, so one transaction keeps one write per key.
func dropMigration(
	stm etcdutil.STM,
	sc *spScope,
	migrName string,
	bmCnt uint32,
) {
	for idx := uint32(0); idx < bmCnt; idx++ {
		stm.Del(model.MigrBitmapKey(sc.Cid, sc.SpId(), migrName, idx))
	}
	stm.Del(model.MigrationKey(sc.Cid, sc.SpId(), migrName))
	sc.Conf.MigrNameList = removeName(sc.Conf.GetMigrNameList(), migrName)
}

// migrSrcLocation locates a CreateMigration source side and asserts its leg
// can still take a destination (§8.11's two errors).
//
// The same walk runs twice — once to plan the allocation and once inside the
// deciding transaction — and shares one implementation so the two can never
// disagree about what they refuse. findSide covers spare legs as well as
// active ones, which §8.11's "any leg of the SP" asks for: a spare's side sits
// on a disk node exactly like an active one and can be moved off it.
func migrSrcLocation(
	conf *pb.SpConf,
	slices []*pb.Slice,
	spName string,
	srcSideId uint64,
) (sliceLocation, error) {
	loc, ok := findSide(conf, slices, srcSideId)
	if !ok {
		return sliceLocation{}, errNotFound(
			"side %d is not in any leg of storage pool %q", srcSideId, spName)
	}
	if len(loc.Leg.GetSideList()) >= migrMaxLegSideCnt {
		return sliceLocation{}, errPrecondition(
			"leg %d already has %d sides: a migration is running on it",
			loc.Leg.GetLegId(), len(loc.Leg.GetSideList()))
	}
	return loc, nil
}

// getMigration reads one migration of the SP by name inside the caller's
// transaction; an absent record is NOT_FOUND (GW7).
//
// The record and its migr_name_list entry are written and deleted in the same
// transaction, so either one answers "does it exist". The four RPCs that
// address an existing migration read the record because they need its fields;
// CreateMigration tests the list instead, because that is also what the
// MaxMigrCntPerSp ceiling counts.
func getMigration(
	stm etcdutil.STM,
	sc *spScope,
	migrName string,
) (*pb.Migration, error) {
	migr := &pb.Migration{}
	if !stm.Get(model.MigrationKey(sc.Cid, sc.SpId(), migrName), migr) {
		return nil, errNotFound("migration %q not found", migrName)
	}
	return migr, nil
}

// CreateMigration is architecture.md §8.11's CreateMigration: it allocates one
// destination disk node and hangs a second, unprovisioned Side off the source
// side's leg.
//
// The destination is written `provisioned: false` ([D15]) and nothing else
// happens yet: the dst DN runs only the §9.4 zeroing protocol, and
// `migr_src_conf.dst_provisioned = false` keeps the source serving untouched
// meanwhile (§11.2 phase 0). That is why this RPC allocates and writes but
// starts no data movement — the sp-worker's later flip does.
//
// Shape (gateway.md §5.10): plain pre-reads to plan, then the GW9 candidate
// unit. The plan — the group's ext_cnt and the black list — is read once
// through a read-only snapshot so the SpConf and the slices it indexes are
// coherent; nothing from it is trusted, since the deciding STM re-locates the
// side and re-verifies the pick's capacity key.
func (s *Server) CreateMigration(
	ctx context.Context,
	req *pb.CreateMigrationRequest,
) (*pb.CreateMigrationReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("migr_name", req.GetMigrName()); err != nil {
		return nil, err
	}
	if err := validateNodeSelector(
		"dn_selector", req.GetDnSelector(),
	); err != nil {
		return nil, err
	}
	if err := validateDmCloneConf(
		"dm_clone_conf", req.GetDmCloneConf(),
	); err != nil {
		return nil, err
	}
	// The plan: which cluster to scan, how big the destination must be, and
	// which disk nodes — and failure domains — it must avoid. grpDnAddrs names
	// every DN the group already occupies through an active or a spare leg and
	// seeds the black list; grpDnLocations turns the same set into §6.5's
	// tier-1 exclusion, so the destination leaves the failure domain the
	// migration is meant to leave, and a cluster with no other domain still
	// places through tier 2 rather than refusing.
	var (
		planCid uint64
		planCc  *pb.ClusterConf
		planExt uint64
		planGrp *pb.Group
	)
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		// openSp, not openSpRead: this is a MUTATOR's planning read, so
		// GW6 wants the token — when one was sent — checked before any other
		// state check: a stale client must see ABORTED "stale revision",
		// never a NOT_FOUND or a FAILED_PRECONDITION computed against a slice
		// list it has not read. The deciding STM checks it again (AG4); this
		// one only fixes which refusal a stale caller is given.
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		slices, err := loadSlices(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		loc, err := migrSrcLocation(
			sc.Conf, slices, req.GetSpName(), req.GetSrcSideId())
		if err != nil {
			return err
		}
		planCid = sc.Cid
		planCc = sc.Cc
		planExt = loc.Grp.GetExtCnt()
		planGrp = loc.Grp
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	planBlack := grpDnAddrs(planGrp)
	// Outside the snapshot, and once for the whole candidate unit: a location
	// cannot change under either (§8.2).
	planLocs, err := grpDnLocations(ctx, s.cli, planCid, planGrp)
	if err != nil {
		return nil, err
	}
	var migrId uint64
	err = candidateUnit(ctx, func() error {
		picks, err := pickDns(
			ctx, s.cli, planCid, planCc,
			dnPickPlan{ExtCnt: planExt, Legs: 1, ExcludeLocs: planLocs},
			req.GetDnSelector(), planBlack, "migration destination",
		)
		if err != nil {
			return err
		}
		cand := picks[0]
		return s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
			migrId = 0
			sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
				req.GetSpRev())
			if err != nil {
				return err
			}
			if len(sc.Conf.GetMigrNameList()) >= common.MaxMigrCntPerSp {
				return errExhausted(
					"storage pool %q already has %d migrations, "+
						"the maximum is %d",
					req.GetSpName(), len(sc.Conf.GetMigrNameList()),
					common.MaxMigrCntPerSp)
			}
			if containsName(
				sc.Conf.GetMigrNameList(), req.GetMigrName(),
			) {
				return errExists(
					"migration %q already exists", req.GetMigrName())
			}
			// The topology is re-verified inside the transaction, not
			// carried over from the plan: a side that moved between the two
			// reads is the plain NOT_FOUND / FAILED_PRECONDITION of §8.11
			// again, never GW9's candidate-changed retry — re-scanning disk
			// nodes would not bring a side back.
			slices, err := loadSlices(stm, sc.Cid, sc.Conf)
			if err != nil {
				return err
			}
			loc, err := migrSrcLocation(
				sc.Conf, slices, req.GetSpName(), req.GetSrcSideId())
			if err != nil {
				return err
			}
			slot, err := migrDstCntlidSlot(
				sc.Conf, loc.Side.GetCntlidSlot())
			if err != nil {
				return err
			}
			// The size charged is the one this transaction read, not the one
			// the scan was run for: it is what the destination must actually
			// hold, and a pick too small for it fails the verify below.
			extCnt := loc.Grp.GetExtCnt()
			ledger := newDnLedger(stm, sc.Cid, sc.Cc)
			dn, err := ledger.verifyPick(cand, extCnt)
			if err != nil {
				return err
			}
			minter := newSpIdMinter(sc.Conf)
			migrId = minter.mint()
			dstSideId := minter.mint()
			minter.commit(sc.Conf)
			loc.Leg.SideList = append(loc.Leg.GetSideList(), &pb.Side{
				SideId:      dstSideId,
				AddrPort:    cand.AddrPort,
				CntlidSlot:  slot,
				NvmeTrConf:  dn.GetNvmeTrConf(),
				ErrEpoch:    0,
				Provisioned: false,
			})
			ptr := &pb.SidePointer{
				SpId:   sc.SpId(),
				LegId:  loc.Leg.GetLegId(),
				SideId: dstSideId,
			}
			if err := ledger.charge(
				cand.AddrPort, ptr, extCnt,
			); err != nil {
				return err
			}
			if err := ledger.flush(opCreateMigration); err != nil {
				return err
			}
			stm.Put(
				model.MigrationKey(
					sc.Cid, sc.SpId(), req.GetMigrName()),
				&pb.Migration{
					MigrId:      migrId,
					SrcSideId:   req.GetSrcSideId(),
					DstSideId:   dstSideId,
					DmCloneConf: req.GetDmCloneConf(),
					BmCnt:       0,
				},
			)
			sc.Conf.MigrNameList = append(
				sc.Conf.GetMigrNameList(), req.GetMigrName())
			stm.Put(
				model.SliceKey(sc.Cid, sc.SpId(), loc.SliceId), loc.Slice)
			stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), sc.Conf)
			return bumpSp(stm, opCreateMigration, sc)
		})
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.CreateMigrationReply{MigrId: migrId}, nil
}

// FinishMigration is architecture.md §8.11's FinishMigration: the destination
// side becomes the leg's only side and the source is released.
//
// It is two-phase (AG4) for the same reason DeleteClone is: `force == false`
// must PROVE the dm-clone has finished hydrating before the source disappears,
// and only the destination DN's agent knows that — so one read-only STM
// resolves what the call needs, the GetSideInfo happens strictly between the
// transactions (AG1), and the deciding STM re-runs the whole resolution plus
// the token check. Nothing from phase 1 is trusted in phase 2: any interleaved
// mutation bumped SpRev, so a token that was sent subsumes the staleness of
// everything phase 1 saw. A caller that sent none gets the re-resolution but
// not that subsumption — GW6 is presence-based (§0 #7), so an interleaved
// mutation stays invisible to it.
//
// An unreachable agent is FAILED_PRECONDITION, not ABORTED (AG3): the caller
// cannot prove hydration is done, which is exactly the precondition §8.11
// states. `force == true` skips the whole judgement and finishes regardless.
func (s *Server) FinishMigration(
	ctx context.Context,
	req *pb.FinishMigrationRequest,
) (*pb.FinishMigrationReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("migr_name", req.GetMigrName()); err != nil {
		return nil, err
	}
	var (
		phaseCid  uint64
		phaseDnId uint64
		phaseAddr string
		phasePtr  *pb.SidePointer
	)
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		migr, err := getMigration(stm, sc, req.GetMigrName())
		if err != nil {
			return err
		}
		phaseCid = sc.Cid
		if req.GetForce() {
			return nil
		}
		slices, err := loadSlices(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		loc, ok := findSide(sc.Conf, slices, migr.GetDstSideId())
		if !ok {
			// A live Migration always owns both sides: Cancel removes the
			// destination and the record together, Finish the source and the
			// record together. A destination that is gone while the record
			// stands is a lost invariant, which is §5.9's ABORTED.
			return errAborted(
				"migration %q destination side %d is in no leg",
				req.GetMigrName(), migr.GetDstSideId())
		}
		dn := &pb.DnConf{}
		dnKey := model.DnConfKey(sc.Cid, loc.Side.GetAddrPort())
		if !stm.Get(dnKey, dn) {
			// GW7: the request named a migration, not this DN — the DN is
			// named by the destination side, and DeleteDiskNode refuses a DN
			// whose side_ptr_list is non-empty. Its absence is a lost
			// invariant key, which is §5.9's ABORTED.
			return errAborted("dn_conf key %q is missing", dnKey)
		}
		phaseDnId = dn.GetDnId()
		phaseAddr = loc.Side.GetAddrPort()
		phasePtr = &pb.SidePointer{
			SpId:   sc.SpId(),
			LegId:  loc.Leg.GetLegId(),
			SideId: loc.Side.GetSideId(),
		}
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	if !req.GetForce() {
		details := ""
		callErr := withDnAgent(ctx, phaseAddr,
			func(ctx context.Context, client pb.DiskNodeAgentClient) error {
				reply, err := client.GetSideInfo(ctx, &pb.GetSideInfoRequest{
					ClusterId:   phaseCid,
					DnId:        phaseDnId,
					SidePointer: phasePtr,
				})
				if err != nil {
					return err
				}
				details = reply.GetSideInfo().GetMigrDstInfo().
					GetDmCloneInfo().GetDetails()
				return nil
			})
		if callErr != nil {
			return nil, errPrecondition(
				"migration %q: disk node %q did not report hydration: %v",
				req.GetMigrName(), phaseAddr, callErr)
		}
		if !hydrationComplete(details) {
			return nil, errPrecondition(
				"migration %q has not finished hydrating", req.GetMigrName())
		}
	}
	var migrId uint64
	err = s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		migrId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		migr, err := getMigration(stm, sc, req.GetMigrName())
		if err != nil {
			return err
		}
		migrId = migr.GetMigrId()
		slices, err := loadSlices(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		loc, ok := findSide(sc.Conf, slices, migr.GetSrcSideId())
		if !ok {
			return errAborted(
				"migration %q source side %d is in no leg",
				req.GetMigrName(), migr.GetSrcSideId())
		}
		if migrSideIdx(loc.Leg, migr.GetDstSideId()) < 0 {
			// The two sides must share one leg: they export the same NQN and
			// the CN aggregates them as the paths of one namespace. If they
			// do not, the leg is not in the shape §8.11 finishes.
			return errAborted(
				"migration %q sides %d and %d are not in one leg",
				req.GetMigrName(), migr.GetSrcSideId(), migr.GetDstSideId())
		}
		// The source's extents go back to its DN and its pointer disappears,
		// which is what makes the src agent tear the side down on its next
		// syncup. One flush = one DnConf write, one capacity key, one DnRev
		// bump (§5.5, §5.6).
		ledger := newDnLedger(stm, sc.Cid, sc.Cc)
		if err := ledger.release(
			loc.Side.GetAddrPort(), sc.SpId(), loc.Side.GetSideId(),
			loc.Grp.GetExtCnt(),
		); err != nil {
			return err
		}
		if err := ledger.flush(opFinishMigration); err != nil {
			return err
		}
		migrDropSide(loc.Leg, migr.GetSrcSideId())
		dropMigration(stm, sc, req.GetMigrName(), migr.GetBmCnt())
		stm.Put(model.SliceKey(sc.Cid, sc.SpId(), loc.SliceId), loc.Slice)
		stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), sc.Conf)
		return bumpSp(stm, opFinishMigration, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.FinishMigrationReply{MigrId: migrId}, nil
}

// CancelMigration is architecture.md §8.11's CancelMigration: the mirror
// rollback of CreateMigration, in one STM.
//
// It is a single transaction where FinishMigration needs two because it
// answers a question no agent has to be consulted about: whatever the
// destination has copied is thrown away, so there is nothing to prove. The
// source side is left exactly as it was and returns to normal service on its
// next SyncupSide; the destination DN sees its pointer disappear and tears the
// stack down, including the migration's local bitmap files (§9.6).
func (s *Server) CancelMigration(
	ctx context.Context,
	req *pb.CancelMigrationRequest,
) (*pb.CancelMigrationReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("migr_name", req.GetMigrName()); err != nil {
		return nil, err
	}
	var migrId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		migrId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		migr, err := getMigration(stm, sc, req.GetMigrName())
		if err != nil {
			return err
		}
		migrId = migr.GetMigrId()
		slices, err := loadSlices(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		loc, ok := findSide(sc.Conf, slices, migr.GetDstSideId())
		if !ok {
			return errAborted(
				"migration %q destination side %d is in no leg",
				req.GetMigrName(), migr.GetDstSideId())
		}
		// The destination's extents return to its DN and its pointer goes,
		// which is the whole rollback: the source side was never touched, so
		// it needs nothing done to it here (§8.11).
		ledger := newDnLedger(stm, sc.Cid, sc.Cc)
		if err := ledger.release(
			loc.Side.GetAddrPort(), sc.SpId(), loc.Side.GetSideId(),
			loc.Grp.GetExtCnt(),
		); err != nil {
			return err
		}
		if err := ledger.flush(opCancelMigration); err != nil {
			return err
		}
		migrDropSide(loc.Leg, migr.GetDstSideId())
		dropMigration(stm, sc, req.GetMigrName(), migr.GetBmCnt())
		stm.Put(model.SliceKey(sc.Cid, sc.SpId(), loc.SliceId), loc.Slice)
		stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), sc.Conf)
		return bumpSp(stm, opCancelMigration, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.CancelMigrationReply{MigrId: migrId}, nil
}

// GetMigration is architecture.md §8.11's GetMigration: one read-only
// transaction (§5.8), so the SpConf that resolves the key and the Migration it
// addresses are read at one store revision. It carries no token — a read never
// needs one — and the Migration is replied verbatim, bm_cnt included, which is
// what tells a caller how many bitmap chunks it has already appended.
func (s *Server) GetMigration(
	ctx context.Context,
	req *pb.GetMigrationRequest,
) (*pb.GetMigrationReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("migr_name", req.GetMigrName()); err != nil {
		return nil, err
	}
	var migr *pb.Migration
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		migr, err = getMigration(stm, sc, req.GetMigrName())
		return err
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.GetMigrationReply{Migr: migr}, nil
}

// AppendMigrationBitmap is architecture.md §8.11's AppendMigrationBitmap: it
// stores one more chunk of the optional "never written, skippable" bitmap the
// destination uses to avoid copying blocks that hold nothing (§11.4).
//
// The chunk lands at bm_idx = the CURRENT bm_cnt and the count then advances,
// which is the whole append rule: a written chunk is immutable, so the index
// is a consequence of how many chunks exist and never a request field. The
// bytes are stored verbatim (GW14, [D-J]) — the gateway never inspects or
// rewrites a bit; the destination agent is what shifts them by the leg's
// meta_blocks and blkdiscards the fully skippable regions.
func (s *Server) AppendMigrationBitmap(
	ctx context.Context,
	req *pb.AppendMigrationBitmapRequest,
) (*pb.AppendMigrationBitmapReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("migr_name", req.GetMigrName()); err != nil {
		return nil, err
	}
	if err := validateBitmap(req.GetBitmap()); err != nil {
		return nil, err
	}
	var migrId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		migrId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		migr, err := getMigration(stm, sc, req.GetMigrName())
		if err != nil {
			return err
		}
		migrId = migr.GetMigrId()
		bmIdx := migr.GetBmCnt()
		if bmIdx >= common.MaxMigrBmCnt {
			return errExhausted(
				"migration %q already has %d bitmap chunks, "+
					"the maximum is %d",
				req.GetMigrName(), bmIdx, common.MaxMigrBmCnt)
		}
		stm.Put(
			model.MigrBitmapKey(
				sc.Cid, sc.SpId(), req.GetMigrName(), bmIdx),
			&pb.MigrBitmap{Bitmap: req.GetBitmap()},
		)
		migr.BmCnt = bmIdx + 1
		stm.Put(
			model.MigrationKey(sc.Cid, sc.SpId(), req.GetMigrName()), migr)
		return bumpSp(stm, opAppendMigrationBitmap, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.AppendMigrationBitmapReply{MigrId: migrId}, nil
}
