package model

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The partial mark of a taken-back leg (architecture.md, Retired legs): the
// worker's write once md held the leg (SetNewLegsPartial), the cancel that
// gives the leg back partial, the take-back that carries the mark into a new
// repair, and the forced start that refuses to name a partial leg unless
// forced.

// takeBack creates the operator's repair of leg B that takes the fixture's
// retired leg back, and returns its rep_id.
func (e *opsEnv) takeBack() uint64 {
	e.t.Helper()
	repId, _ := e.createRepair(func() RepairNewLeg {
		return RepairNewLeg{RetiredLegId: opsRetiredLeg}
	})
	return repId
}

// markPartial is SetNewLegsPartial marking legIds on the fixture's repair.
func (e *opsEnv) markPartial(repId uint64, legIds ...uint64) (int, error) {
	e.t.Helper()
	return SetNewLegsPartial(e.ctx, e.cli, e.cid, opsSpId, opsSpName,
		opsRepName, repId, legIds, nil)
}

// TestSetNewLegsPartial: the op marks a taken-back new leg, raises no
// revision, writes nothing for a leg already marked, a leg the repair did
// not take back, a stale rep_id or a name the pool does not list, and
// clears a mark, a clear of no mark writing nothing.
func TestSetNewLegsPartial(t *testing.T) {
	env := newOpsEnv(t)
	env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide, opsDnC, 1,
		opsRetiredEpoch)
	repId := env.takeBack()
	spRev := env.spRev()
	set, err := env.markPartial(repId, opsRetiredLeg)
	if err != nil || set != 1 {
		t.Fatalf("SetNewLegsPartial = %d, %v, want 1", set, err)
	}
	if !env.repair(opsRepName).GetNewLegIdToPartial()[opsRetiredLeg] {
		t.Fatalf("the repair carries no partial mark for the leg")
	}
	if got := env.spRev(); got != spRev {
		t.Errorf("SpRev %d after the mark, want %d", got, spRev)
	}
	before := env.storeRev()
	for _, tc := range []struct {
		name   string
		repId  uint64
		legIds []uint64
	}{
		{"marked already", repId, []uint64{opsRetiredLeg}},
		{"no taken-back leg", repId, []uint64{opsDataLegA}},
		{"another rep_id", repId + 1, []uint64{opsRetiredLeg}},
	} {
		set, err := env.markPartial(tc.repId, tc.legIds...)
		if err != nil || set != 0 {
			t.Errorf("%s: SetNewLegsPartial = %d, %v, want a skip",
				tc.name, set, err)
		}
	}
	set, err = SetNewLegsPartial(env.ctx, env.cli, env.cid, opsSpId,
		opsSpName, "no-such-repair", repId, []uint64{opsRetiredLeg}, nil)
	if err != nil || set != 0 {
		t.Errorf("an unlisted name: SetNewLegsPartial = %d, %v", set, err)
	}
	if got := env.storeRev(); got != before {
		t.Errorf("a skip wrote: store revision %d, want %d", got, before)
	}
	clear := func() (int, error) {
		return SetNewLegsPartial(env.ctx, env.cli, env.cid, opsSpId,
			opsSpName, opsRepName, repId, nil, []uint64{opsRetiredLeg})
	}
	if set, err := clear(); err != nil || set != 1 {
		t.Fatalf("the clear = %d, %v, want 1", set, err)
	}
	if env.repair(opsRepName).GetNewLegIdToPartial()[opsRetiredLeg] {
		t.Fatalf("the clear left the mark")
	}
	before = env.storeRev()
	if set, err := clear(); err != nil || set != 0 ||
		env.storeRev() != before {
		t.Errorf("a clear of no mark = %d, %v and wrote", set, err)
	}
}

// TestPartialRidesTheCancelAndTheTakeBack: a cancel gives a marked leg back
// as a retired leg with partial set, and a repair that takes it back again
// carries the mark; an unmarked leg comes back unmarked.
func TestPartialRidesTheCancelAndTheTakeBack(t *testing.T) {
	for _, partial := range []bool{true, false} {
		env := newOpsEnv(t)
		env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide, opsDnC,
			1, opsRetiredEpoch)
		repId := env.takeBack()
		if partial {
			if _, err := env.markPartial(repId, opsRetiredLeg); err != nil {
				t.Fatalf("SetNewLegsPartial: %v", err)
			}
		}
		env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_ABORTED)
		if _, err := CancelRepair(env.ctx, env.cli, env.cid, opsShard,
			opsSpId, opsSpName, noExpectRev, opsRepName, env.cc); err != nil {
			t.Fatalf("CancelRepair: %v", err)
		}
		if got := env.retired(opsRetiredLeg).GetPartial(); got != partial {
			t.Fatalf("partial %v: the leg given back reads partial %v",
				partial, got)
		}
		env.takeBack()
		if got := env.repair(opsRepName).GetNewLegIdToPartial()[opsRetiredLeg]; got != partial {
			t.Errorf("partial %v: the new take-back carries partial %v",
				partial, got)
		}
	}
}

// TestForceStartGroupRefusesAPartialLeg: a mark naming a taken-back new leg
// that carries the partial mark is refused with nothing written, unless
// forced; the same leg unmarked is named with no force.
func TestForceStartGroupRefusesAPartialLeg(t *testing.T) {
	force := func(env *opsEnv, on bool) error {
		_, err := ForceStartGroup(env.ctx, env.cli, env.cid, opsShard,
			opsSpId, opsSpName, noExpectRev, opsSliceParent, opsDataGrpId,
			opsRetiredLeg, false, on)
		return err
	}
	env := newOpsEnv(t)
	env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide, opsDnC, 1,
		opsRetiredEpoch)
	repId := env.takeBack()
	if _, err := env.markPartial(repId, opsRetiredLeg); err != nil {
		t.Fatalf("SetNewLegsPartial: %v", err)
	}
	before := env.storeRev()
	env.wantNoWrite(force(env, false), opForceStartGroup, ReasonLegPartial,
		before)
	if err := force(env, true); err != nil {
		t.Fatalf("the forced mark on the partial leg: %v", err)
	}
	if got := env.mark(opsDataGrpId).GetLegId(); got != opsRetiredLeg {
		t.Errorf("the mark names leg %d, want %d", got, opsRetiredLeg)
	}

	env = newOpsEnv(t)
	env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide, opsDnC, 1,
		opsRetiredEpoch)
	env.takeBack()
	if err := force(env, false); err != nil {
		t.Fatalf("a mark on the unmarked taken-back leg: %v", err)
	}
}
