package model

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// resetNextId rewrites the fixture SP's next_id, which is how an SpConf that
// was written without one — proto3, so the counter reads back as its zero
// value — reaches the ops.
func resetNextId(e *opsEnv, nextId uint64) {
	e.t.Helper()
	conf := e.spConf()
	conf.NextId = nextId
	mustPut(e.t, e.cli, SpConfKey(e.cid, opsSpName), conf)
}

// TestSpNextIdReservesZero pins the id floor of architecture.md §5.4: per-SP
// sub-object ids come from SpConf.next_id, which "starts at 1", so 0 is not a
// legal id — it is the reserved "none" sentinel failoverCandidate returns and
// the reaction pass compares against.
func TestSpNextIdReservesZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		conf *pb.SpConf
		want uint64
	}{
		{name: "nil conf", conf: nil, want: SpFirstId},
		{name: "unset counter", conf: &pb.SpConf{}, want: SpFirstId},
		{name: "explicit zero", conf: &pb.SpConf{NextId: 0}, want: SpFirstId},
		{name: "one", conf: &pb.SpConf{NextId: 1}, want: 1},
		{name: "past one", conf: &pb.SpConf{NextId: 1000}, want: 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SpNextId(tc.conf); got != tc.want {
				t.Errorf("SpNextId: got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestCreateSpareLegNeverMintsIdZero is the same rule at the op that mints leg
// and side ids: an SpConf whose next_id is the proto3 zero must still produce
// ids >= 1, and the counter must come out past them.
func TestCreateSpareLegNeverMintsIdZero(t *testing.T) {
	env := newOpsEnv(t)
	resetNextId(env, 0)

	legId, err := CreateSpareLeg(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, opsDataGrpId, env.dnCand(opsDnC), env.cc,
	)
	if err != nil {
		t.Fatalf("CreateSpareLeg: %v", err)
	}
	if legId < SpFirstId {
		t.Fatalf("leg_id: got %d, want >= %d (0 is the none sentinel)",
			legId, SpFirstId)
	}
	grp := findGroup(env.slice(), opsDataGrpId)
	spares := grp.GetSpareLegList()
	if len(spares) != 1 {
		t.Fatalf("spares: got %d, want 1", len(spares))
	}
	sides := spares[0].GetSideList()
	if len(sides) != 1 {
		t.Fatalf("sides: got %d, want 1", len(sides))
	}
	if sides[0].GetSideId() < SpFirstId {
		t.Errorf("side_id: got %d, want >= %d",
			sides[0].GetSideId(), SpFirstId)
	}
	// Two ids were consumed, both above the floor.
	if got := env.spConf().GetNextId(); got != SpFirstId+2 {
		t.Errorf("next_id: got %d, want %d", got, SpFirstId+2)
	}
}

// TestReplaceCntlrNeverMintsIdZero is AR7's mint: a cntlr with id 0 would be
// invisible to AR5's election (failoverCandidate returns 0 for "no candidate"
// and worker/reaction.go tests the plan's candidate against 0), so a healthy,
// enabled, non-primary cntlr could exist while AR7 still took its sole-primary
// replacement path.
func TestReplaceCntlrNeverMintsIdZero(t *testing.T) {
	env := newOpsEnv(t)
	resetNextId(env, 0)
	env.setCntlrErr(opsCntlrB, 1000)
	now := uint64(1000 + common.DefaultCntlrUnhealthy)

	newId, err := ReplaceCntlr(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrB, env.cnCand(opsCnC), false, now,
	)
	if err != nil {
		t.Fatalf("ReplaceCntlr: %v", err)
	}
	if newId < SpFirstId {
		t.Fatalf("cntlr_id: got %d, want >= %d (0 is the none sentinel)",
			newId, SpFirstId)
	}
	if got := env.spConf().GetNextId(); got != newId+1 {
		t.Errorf("next_id: got %d, want %d", got, newId+1)
	}
	// The replacement is exactly the kind of cntlr AR5 has to be able to elect:
	// healthy, enabled, not primary. With id 0 it would look like "no
	// candidate" to every reader of failoverCandidate.
	fresh := env.cntlr(newId)
	if fresh.GetPrimary() || fresh.GetDisabled() || fresh.GetErrEpoch() != 0 {
		t.Fatalf("the new cntlr must be a healthy, enabled standby: %v", fresh)
	}
}

// TestGrowSliceNeverMintsIdZero is the third mint site: a grown group takes a
// grp_id and one leg_id/side_id pair per leg, all from the same counter.
func TestGrowSliceNeverMintsIdZero(t *testing.T) {
	env := newOpsEnv(t)
	resetNextId(env, 0)

	legs := []Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)}
	grpId, err := GrowSlice(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, false, opsNotPending, env.cc, legs,
	)
	if err != nil {
		t.Fatalf("GrowSlice: %v", err)
	}
	if grpId < SpFirstId {
		t.Fatalf("grp_id: got %d, want >= %d (0 is the none sentinel)",
			grpId, SpFirstId)
	}
	grp := findGroup(env.slice(), grpId)
	if grp == nil {
		t.Fatalf("the grown group %d is not in the slice", grpId)
	}
	for _, leg := range grp.GetLegList() {
		if leg.GetLegId() < SpFirstId {
			t.Errorf("leg_id: got %d, want >= %d",
				leg.GetLegId(), SpFirstId)
		}
		for _, side := range leg.GetSideList() {
			if side.GetSideId() < SpFirstId {
				t.Errorf("side_id: got %d, want >= %d",
					side.GetSideId(), SpFirstId)
			}
		}
	}
	// One grp_id plus a leg_id/side_id pair per leg.
	if got := env.spConf().GetNextId(); got != SpFirstId+5 {
		t.Errorf("next_id: got %d, want %d", got, SpFirstId+5)
	}
}
