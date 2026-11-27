package model

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The keep flag of a retired leg (architecture.md, Retired legs): the
// operator's call (SetRetiredLegKeep), the forced start's keep of the group's
// other copies (keepGroupCopies), the finish of a repair whose bad legs are
// kept, the give-back at the bound that passes a kept leg over, and the
// cancel that gives a kept leg back kept.

// keepRetired sets the keep flag of a retired leg of the fixture SP
// directly in the store, as an earlier call would have.
func (e *opsEnv) keepRetired(legId uint64) {
	e.t.Helper()
	retired := e.retired(legId)
	retired.Keep = true
	mustPut(e.t, e.cli, RetiredLegKey(e.cid, opsSpId, legId), retired)
}

// TestSetRetiredLegKeep: the call sets and clears the flag, writes nothing
// for the value the leg holds, raises no revision, and refuses a leg the
// pool does not list with nothing written.
func TestSetRetiredLegKeep(t *testing.T) {
	env := newOpsEnv(t)
	env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide, opsDnC, 1,
		opsRetiredEpoch)
	set := func(legId uint64, keep bool) error {
		return SetRetiredLegKeep(env.ctx, env.cli, env.cid, opsShard,
			opsSpId, opsSpName, noExpectRev, legId, keep)
	}
	spRev := env.spRev()
	if err := set(opsRetiredLeg, true); err != nil {
		t.Fatalf("SetRetiredLegKeep true: %v", err)
	}
	if !env.retired(opsRetiredLeg).GetKeep() {
		t.Fatalf("the retired leg carries no keep after the set")
	}
	if got := env.spRev(); got != spRev {
		t.Errorf("SpRev %d after the set, want %d: no agent reads the flag",
			got, spRev)
	}
	before := env.storeRev()
	if err := set(opsRetiredLeg, true); err != nil {
		t.Fatalf("SetRetiredLegKeep true again: %v", err)
	}
	if got := env.storeRev(); got != before {
		t.Errorf("a set of the value the leg holds wrote: store revision "+
			"%d, want %d", got, before)
	}
	if err := set(opsRetiredLeg, false); err != nil {
		t.Fatalf("SetRetiredLegKeep false: %v", err)
	}
	if env.retired(opsRetiredLeg).GetKeep() {
		t.Fatalf("the retired leg keeps its flag after the clear")
	}
	before = env.storeRev()
	err := set(opsRetiredLeg2, true)
	env.wantNoWrite(err, opSetRetiredLegKeep, ReasonRetiredLegNotFound, before)
}

// TestForceStartGroupKeepsTheGroupsCopies: a mark's write sets keep on every
// retired leg of the group and keep_bad_legs on its open repair; a cancel
// clears neither; and an ABORTED repair, whose bad leg stays in the group,
// gets no keep_bad_legs.
func TestForceStartGroupKeepsTheGroupsCopies(t *testing.T) {
	t.Run("an open repair", func(t *testing.T) {
		env := newOpsEnv(t)
		env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide, opsDnC,
			1, opsRetiredEpoch)
		env.putDn(opsDnE, 704, 4, opsDnFree)
		env.createRepair(func() RepairNewLeg { return env.freshLeg(opsDnE) })
		env.mustForceStart(opsDataGrpId, opsDataLegA)
		if !env.retired(opsRetiredLeg).GetKeep() {
			t.Errorf("the group's retired leg carries no keep after the mark")
		}
		if !env.repair(opsRepName).GetKeepBadLegs() {
			t.Errorf("the open repair carries no keep_bad_legs after the mark")
		}
		if _, err := env.forceStart(opsDataGrpId, 0, true); err != nil {
			t.Fatalf("the cancel: %v", err)
		}
		if !env.retired(opsRetiredLeg).GetKeep() ||
			!env.repair(opsRepName).GetKeepBadLegs() {
			t.Errorf("the cancel cleared a keep flag")
		}
	})
	// A taken-back leg of a repair whose abort flag is set is no member the
	// forced start keeps, so it keeps its own data: the mark keeps it in
	// the repair, and the cancel gives it back kept.
	t.Run("a taken-back leg of an aborted repair", func(t *testing.T) {
		env := newOpsEnv(t)
		env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide, opsDnC,
			1, opsRetiredEpoch)
		repId, _ := env.createRepair(func() RepairNewLeg {
			return RepairNewLeg{RetiredLegId: opsRetiredLeg}
		})
		env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_ABORTED)
		env.mustForceStart(opsDataGrpId, opsDataLegA)
		rep := env.repair(opsRepName)
		if !rep.GetNewLegIdToKeep()[opsRetiredLeg] {
			t.Fatalf("the taken-back leg is not kept in the repair: %v", rep)
		}
		if _, err := CancelRepair(env.ctx, env.cli, env.cid, opsShard,
			opsSpId, opsSpName, noExpectRev, opsRepName, env.cc); err != nil {
			t.Fatalf("CancelRepair: %v", err)
		}
		if !env.retired(opsRetiredLeg).GetKeep() {
			t.Errorf("the cancel gave the taken-back leg back unkept")
		}
	})
	t.Run("an aborted repair", func(t *testing.T) {
		env := newOpsEnv(t)
		env.putDn(opsDnE, 704, 4, opsDnFree)
		repId, _ := env.createRepair(func() RepairNewLeg {
			return env.freshLeg(opsDnE)
		})
		env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_ABORTED)
		env.mustForceStart(opsDataGrpId, opsDataLegA)
		if env.repair(opsRepName).GetKeepBadLegs() {
			t.Errorf("an aborted repair got keep_bad_legs")
		}
	})
}

// TestFinishRepairRetiresAKeptBadLeg: the finish of a repair that carries
// keep_bad_legs retires its bad leg with keep; one without retires it
// without.
func TestFinishRepairRetiresAKeptBadLeg(t *testing.T) {
	for _, keep := range []bool{true, false} {
		env := newOpsEnv(t)
		env.putDn(opsDnE, 704, 4, opsDnFree)
		repId, _ := env.createRepair(func() RepairNewLeg {
			return env.freshLeg(opsDnE)
		})
		rep := env.repair(opsRepName)
		rep.KeepBadLegs = keep
		mustPut(t, env.cli, RepairKey(env.cid, opsSpId, opsRepName), rep)
		env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_STARTED)
		if _, err := FinishRepair(env.ctx, env.cli, env.cid, opsShard,
			opsSpId, opsSpName, noExpectRev, opsRepName, opsFinishNow,
			env.cc); err != nil {
			t.Fatalf("FinishRepair: %v", err)
		}
		if got := env.retired(opsDataLegB).GetKeep(); got != keep {
			t.Errorf("keep_bad_legs %v: the retired bad leg's keep is %v",
				keep, got)
		}
	}
}

// TestFinishRepairPassesAKeptLegOver: the finish of the sp worker's repair
// at the bound gives back the oldest retired leg that is not kept, the
// younger one when the oldest is kept, and gives back none and writes
// nothing when every one is kept (ReasonRetiredLegsKept), the repair staying
// STARTED.
func TestFinishRepairPassesAKeptLegOver(t *testing.T) {
	t.Run("the oldest kept", func(t *testing.T) {
		env := newOpsEnv(t)
		env.putRetiredBound()
		env.keepRetired(opsRetiredLeg)
		env.startWorkerRepair()
		res, err := FinishRepair(env.ctx, env.cli, env.cid, opsShard,
			opsSpId, opsSpName, noExpectRev, opsWorkerRepName, opsFinishNow,
			env.cc)
		if err != nil {
			t.Fatalf("FinishRepair: %v", err)
		}
		if res.GivenBackLegId != opsRetiredLeg2 {
			t.Errorf("gave back leg %d, want the younger %d: the oldest is "+
				"kept", res.GivenBackLegId, opsRetiredLeg2)
		}
		if !env.retired(opsRetiredLeg).GetKeep() {
			t.Errorf("the kept leg lost its flag")
		}
	})
	t.Run("every one kept", func(t *testing.T) {
		env := newOpsEnv(t)
		env.putRetiredBound()
		env.keepRetired(opsRetiredLeg)
		env.keepRetired(opsRetiredLeg2)
		env.startWorkerRepair()
		before := env.storeRev()
		_, err := FinishRepair(env.ctx, env.cli, env.cid, opsShard,
			opsSpId, opsSpName, noExpectRev, opsWorkerRepName, opsFinishNow,
			env.cc)
		env.wantNoWrite(err, opFinishRepair, ReasonRetiredLegsKept, before)
		if got := env.repair(opsWorkerRepName).GetRepStage(); got !=
			pb.RepStage_REP_STAGE_STARTED {
			t.Errorf("the repair is %v after the refusal, want STARTED", got)
		}
	})
}

// TestCancelRepairKeepsAKeptLeg: a kept retired leg taken back by a repair
// is remembered in the repair (new_leg_id_to_keep), and the cancel gives it
// back kept; a leg taken back without the flag comes back without it.
func TestCancelRepairKeepsAKeptLeg(t *testing.T) {
	for _, keep := range []bool{true, false} {
		env := newOpsEnv(t)
		env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide, opsDnC,
			1, opsRetiredEpoch)
		if keep {
			env.keepRetired(opsRetiredLeg)
		}
		repId, _ := env.createRepair(func() RepairNewLeg {
			return RepairNewLeg{RetiredLegId: opsRetiredLeg}
		})
		if got := env.repair(opsRepName).GetNewLegIdToKeep()[opsRetiredLeg]; got != keep {
			t.Errorf("keep %v: the repair remembers keep %v", keep, got)
		}
		env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_ABORTED)
		if _, err := CancelRepair(env.ctx, env.cli, env.cid, opsShard,
			opsSpId, opsSpName, noExpectRev, opsRepName, env.cc); err != nil {
			t.Fatalf("CancelRepair: %v", err)
		}
		if got := env.retired(opsRetiredLeg).GetKeep(); got != keep {
			t.Errorf("keep %v: the leg given back has keep %v", keep, got)
		}
	}
}

// TestDeleteRetiredLegRefusesAKeptLegForTheTimer: the retire timer's delete
// (refuseKept) refuses a kept leg in its own STM, with nothing written,
// since a keep written after the timer's snapshot raises no revision; the
// operator's delete deletes the same leg as asked.
func TestDeleteRetiredLegRefusesAKeptLegForTheTimer(t *testing.T) {
	env := newOpsEnv(t)
	env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide, opsDnC, 1,
		opsRetiredEpoch)
	env.keepRetired(opsRetiredLeg)
	before := env.storeRev()
	err := DeleteRetiredLeg(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, env.spRev(), opsRetiredLeg, env.cc, true)
	env.wantNoWrite(err, opDeleteRetiredLeg, ReasonRetiredLegKept, before)
	if err := DeleteRetiredLeg(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, noExpectRev, opsRetiredLeg, env.cc,
		false); err != nil {
		t.Fatalf("the operator's delete of a kept leg: %v", err)
	}
	if env.exists(RetiredLegKey(env.cid, opsSpId, opsRetiredLeg)) {
		t.Errorf("the operator's delete left the kept leg")
	}
}

// TestCancelRepairLeavesTheKeepOnTheLeg pins the keep carried with the leg
// (architecture.md, Retired legs): a forced start keeps the group's open
// repair's bad leg (keep_bad_legs); the repair's abort and cancel leave the
// keep on that leg in the group's leg_list; a later repair of the worker's
// of that leg finishes and retires it with keep, so the retire timer passes
// it over. The repair record carries keep_bad_legs alone, and its cancel
// leaves the keep on the leg. The new leg the finish puts in the bad leg's
// place carries no keep.
func TestCancelRepairLeavesTheKeepOnTheLeg(t *testing.T) {
	env := newOpsEnv(t)
	env.putDn(opsDnE, 704, 4, opsDnFree)
	repId, _ := env.createRepair(func() RepairNewLeg {
		return env.freshLeg(opsDnE)
	})
	env.mustForceStart(opsDataGrpId, opsDataLegA)
	env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_ABORTED)
	if _, err := CancelRepair(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, noExpectRev, opsRepName, env.cc); err != nil {
		t.Fatalf("CancelRepair: %v", err)
	}
	if !findLeg(env.slice(), opsDataLegB).GetKeep() {
		t.Fatalf("the cancel dropped the bad leg's keep")
	}
	if findLeg(env.slice(), opsDataLegA).GetKeep() {
		t.Fatalf("the cancel kept a leg that was no bad leg")
	}
	if _, err := env.forceStart(opsDataGrpId, 0, true); err != nil {
		t.Fatalf("the mark's cancel: %v", err)
	}
	env.startWorkerRepair()
	if _, err := FinishRepair(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, noExpectRev, opsWorkerRepName, opsFinishNow,
		env.cc); err != nil {
		t.Fatalf("FinishRepair: %v", err)
	}
	if !env.retired(opsDataLegB).GetKeep() {
		t.Fatalf("the worker's finish retired the kept leg without keep")
	}
	for _, leg := range findGroup(env.slice(), opsDataGrpId).GetLegList() {
		if leg.GetKeep() {
			t.Fatalf("leg %d in the group carries keep after the finish",
				leg.GetLegId())
		}
	}
}

// TestCancelRepairWithNoKeepLeavesNone: the cancel of a repair that carries
// no keep_bad_legs leaves no keep on its bad leg.
func TestCancelRepairWithNoKeepLeavesNone(t *testing.T) {
	env := newOpsEnv(t)
	env.putDn(opsDnE, 704, 4, opsDnFree)
	repId, _ := env.createRepair(func() RepairNewLeg {
		return env.freshLeg(opsDnE)
	})
	env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_ABORTED)
	if _, err := CancelRepair(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, noExpectRev, opsRepName, env.cc); err != nil {
		t.Fatalf("CancelRepair: %v", err)
	}
	if findLeg(env.slice(), opsDataLegB).GetKeep() {
		t.Fatalf("a cancel with no keep_bad_legs kept the bad leg")
	}
}
