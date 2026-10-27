package model

import (
	"context"
	"slices"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The forced start of a group's array: the mark an operator writes on an
// md-raid1 group so that its primary starts the array from one leg the
// operator names (architecture.md, Repairs; Repair). The mark is the
// group's forced_start, the named leg and the SpRev revision its write
// committed; it rides to every cntlr in the group's parent record, a slice
// or a clone, so a write raises SpRev. ForceStartGroup writes, replaces and
// withdraws it for the gateway's call; ClearForcedStart is the sp worker's
// clear from the primary's report (dnv-worker.md AR13); FinishRepair clears
// a mark that names the bad leg it retires, and SetRepairAbort refuses while
// the mark names the repair's new leg (forcedStartNames).

// The Op names of the forced start's operations.
const (
	opForceStartGroup  = "ForceStartGroup"
	opClearForcedStart = "ClearForcedStart"
)

// ReasonLegNotNamable is the Reason ForceStartGroup carries for a leg the
// mark may not name: one in neither the group's leg_list nor the new legs of
// the repair it names, a new leg of an ABORTED repair or of a CREATED one
// whose abort flag is set, which is no member the forced start keeps
// (keepsNewLegs), or the bad leg of a STARTED repair, which the finish
// retires (architecture.md, Repairs). It is exported for the tests and for
// the gateway's switch on model reasons: the gateway names the leg with its
// own code from its planning read, and meets this only when a write since
// that read took the leg away.
const ReasonLegNotNamable = "leg not namable"

// ForceStartGroup writes the forced-start mark on the group grpId held by
// parent, naming legId, and returns the revision the mark carries: the SpRev
// revision its own transaction commits, so a report of an observation made
// before the write can never clear it. A mark already on the group is
// replaced. With cancel the mark is withdrawn instead and the result is
// zero; a group that carries none is answered as done and written to not at
// all, so a repeated cancel raises no revision.
//
// It checks the token first (checkSpRev) and takes a pool that is not being
// deleted (loadSpConfAnyLevel), a group of an md-raid1 pool whose parent
// still holds it and is not a deleting clone (loadGrpOf). A write is refused
// at an sp_level at or above SP_LEVEL_NO_REDUND, where no group array is
// built; a cancel runs at every level, since a mark that stands acts once
// the level is lowered. The leg must be one the mark may name
// (ReasonLegNotNamable). The gateway's planning read refuses the rest of the
// call's cases, the group's row among them, which no transaction sees.
func ForceStartGroup(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	expectRev uint64,
	parent GrpParent,
	grpId uint64,
	legId uint64,
	cancel bool,
) (uint64, error) {
	revision := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		revision = 0
		if err := checkSpRev(
			s, opForceStartGroup, shard, cid, spId, expectRev,
		); err != nil {
			return err
		}
		conf, err := loadSpConfAnyLevel(
			s, opForceStartGroup, cid, spName, spId)
		if err != nil {
			return err
		}
		if !cancel && conf.GetSpLevel() >= pb.SpLevel_SP_LEVEL_NO_REDUND {
			return fail(opForceStartGroup, reasonRepairLevelNoArray)
		}
		if !isMdRaid1(conf) {
			return fail(opForceStartGroup, "group is not md-raid1")
		}
		holder, grp, err := loadGrpOf(
			s, opForceStartGroup, cid, spId, parent, grpId)
		if err != nil {
			return err
		}
		if cancel {
			if grp.GetForcedStart() == nil {
				return nil
			}
			grp.ForcedStart = nil
			holder.put(s)
			return BumpSpRev(s, opForceStartGroup, shard, cid, spId)
		}
		namable, err := forcedStartNamable(
			s, opForceStartGroup, cid, conf, grp, legId)
		if err != nil {
			return err
		}
		if !namable {
			return fail(opForceStartGroup, ReasonLegNotNamable)
		}
		// STM.Put marshals at the call, so the revision goes into the mark
		// before the parent is staged.
		revision, err = bumpSpRevTo(s, opForceStartGroup, shard, cid, spId)
		if err != nil {
			return err
		}
		grp.ForcedStart = &pb.ForcedStart{LegId: legId, Revision: revision}
		holder.put(s)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return revision, nil
}

// forcedStartNamable reports whether a forced-start mark of grp may name
// legId, read inside the caller's STM: a leg of the group's leg_list but the
// bad leg of a STARTED repair, or a new leg of the group's open repair but
// one of a CREATED repair whose abort flag is set (keepsNewLegs). The bad
// leg of a STARTED repair left the array before
// md began copying onto the new leg, so it is older than the array, and the
// finish retires it, so an array started from it alone would be left with
// no full member the group keeps (architecture.md, Repair, Forced start). A
// group that names a repair is checked against that repair whichever leg
// the mark names; one that names a repair the pool does not list, or whose
// record is missing, is refused as loadRepair refuses it.
func forcedStartNamable(
	s etcdutil.STM,
	op string,
	cid uint64,
	conf *pb.SpConf,
	grp *pb.Group,
	legId uint64,
) (bool, error) {
	inLegList := legIdxIn(grp.GetLegList(), legId) >= 0
	if grp.GetRepName() == "" {
		return inLegList, nil
	}
	rep, err := loadRepair(s, op, cid, conf, grp.GetRepName())
	if err != nil {
		return false, err
	}
	if inLegList {
		return !startedBadLeg(rep, legId), nil
	}
	return newLegOf(rep, legId) != nil && keepsNewLegs(rep), nil
}

// keepsNewLegs reports whether the forced start keeps rep's new legs
// (architecture.md, Repair, Forced start): those of an open repair but a
// CREATED one whose abort flag is set, which the abort takes out of the
// group. A STARTED repair's new legs are kept whatever the flag says, since
// no abort takes a STARTED repair back and its finish makes them leg_list
// legs; an ABORTED repair is open no more.
func keepsNewLegs(rep *pb.Repair) bool {
	switch rep.GetRepStage() {
	case pb.RepStage_REP_STAGE_STARTED:
		return true
	case pb.RepStage_REP_STAGE_CREATED:
		return !rep.GetTryToAbort()
	}
	return false
}

// startedBadLeg reports whether legId is a bad leg of rep while rep is
// STARTED, the one leg_list leg a forced-start mark may not name
// (forcedStartNamable).
func startedBadLeg(rep *pb.Repair, legId uint64) bool {
	return rep.GetRepStage() == pb.RepStage_REP_STAGE_STARTED &&
		slices.Contains(rep.GetBadLegIdList(), legId)
}

// forcedStartNames reports whether grp carries a forced-start mark that
// names legId. FinishRepair clears such a mark for the repair's bad leg,
// which the finish retires, and SetRepairAbort refuses the abort flag while
// such a mark names the repair's new leg, which the flag would take out of
// the members the forced start keeps. A mark that names a leg the forced
// start no longer keeps can never be acted on, and while the group's array
// does not run no report clears it (dnv-worker.md AR13), so it would be the
// group's error on every pass and hold leg repair off the group until the
// operator withdrew it; and an abort that withdrew it could leave the group
// with no member a mark may name (SetRepairAbort).
func forcedStartNames(grp *pb.Group, legId uint64) bool {
	mark := grp.GetForcedStart()
	return mark != nil && mark.GetLegId() == legId
}

// ForcedStartRef names the mark the sp worker clears (ClearForcedStart): the
// group by its parent and id, and the mark by the leg it named and the
// revision it carried, which prove that the mark is still the one the
// primary's report answered.
type ForcedStartRef struct {
	Parent   GrpParent
	GrpId    uint64
	LegId    uint64
	Revision uint64
}

// ClearForcedStart withdraws the forced-start mark ref names once the
// primary's report shows the forced start done: the group's row reads OK and
// md holds in sync a leg whose own row reads OK, the named leg or another,
// in a report of an observation made at the mark's revision or later
// (dnv-worker.md AR13). It reports whether it wrote
// and the revision its STM committed, 0 when it wrote nothing, and raises
// SpRev when it writes, so that every cntlr is handed the group without the
// mark: one that stayed would start the array again from the named leg at
// the group's next fault.
//
// It takes no expected revision, the worker converging on what etcd holds,
// and a pool at any level that is not being deleted (loadSpConfAnyLevel). A
// ref that is stale is skipped, never refused, as FlipRepairProvisioned
// skips one: a parent or a group that is gone, a clone being deleted, whose
// group goes back in the clone drain's final transaction (dnv-worker.md
// CLD9), and a mark that is gone or is not the one ref names, replaced or
// withdrawn since.
func ClearForcedStart(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	ref ForcedStartRef,
) (bool, uint64, error) {
	cleared, revision := false, uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		cleared, revision = false, 0
		if _, err := loadSpConfAnyLevel(
			s, opClearForcedStart, cid, spName, spId,
		); err != nil {
			return err
		}
		holder, err := loadGrpHolder(
			s, opClearForcedStart, cid, spId, ref.Parent)
		if err != nil {
			return nil
		}
		grp := holder.group(ref.GrpId)
		if grp == nil || holder.deleting() {
			return nil
		}
		mark := grp.GetForcedStart()
		if mark == nil || mark.GetLegId() != ref.LegId ||
			mark.GetRevision() != ref.Revision {
			return nil
		}
		grp.ForcedStart = nil
		holder.put(s)
		revision, err = bumpSpRevTo(
			s, opClearForcedStart, shard, cid, spId)
		cleared = err == nil
		return err
	})
	if err != nil {
		return false, 0, err
	}
	return cleared, revision, nil
}
