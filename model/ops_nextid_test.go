package model

import (
	"math"
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

// TestSpNextIdReservesZero pins the id floor (architecture.md, Globals: id
// allocation + shard buckets): per-SP
// sub-object ids come from SpConf.next_id, which starts at SpFirstId, so 0 is
// not a legal id — it is the reserved "none" sentinel failoverCandidate returns
// and the reaction pass compares against.
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

// TestCreateRepairNeverMintsIdZero is the same rule at the repair's mint: a
// fresh new leg takes the rep_id, its leg id and its side id from the same
// counter, all above the floor, and the counter comes out past them.
func TestCreateRepairNeverMintsIdZero(t *testing.T) {
	env := newOpsEnv(t)
	resetNextId(env, 0)

	repId, legId := env.createRepair(func() RepairNewLeg {
		return env.freshLeg(opsDnC)
	})
	if repId != SpFirstId || legId != SpFirstId+1 {
		t.Fatalf("rep_id %d, leg_id %d, want %d and %d (0 is the none "+
			"sentinel)", repId, legId, SpFirstId, SpFirstId+1)
	}
	side := env.repair(opsRepName).GetNewLegList()[0].GetSideList()[0]
	if side.GetSideId() != SpFirstId+2 {
		t.Errorf("side_id: got %d, want %d", side.GetSideId(), SpFirstId+2)
	}
	if got := env.spConf().GetNextId(); got != SpFirstId+3 {
		t.Errorf("next_id: got %d, want %d", got, SpFirstId+3)
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
		opsCntlrB, env.cnCand(opsCnC), []string{opsCnA}, false, now,
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

// TestTakeSpIdsAtTheTop pins the top of a pool's counter (architecture.md,
// Globals: id allocation + shard buckets): the largest 64-bit value is never
// an id, so a counter one below it has one id left and a counter at it has
// none. A run that fits leaves next_id just past it, at most at the largest
// value; a run that does not is ReasonNoIdLeft and leaves the conf as it was.
func TestTakeSpIdsAtTheTop(t *testing.T) {
	const top = uint64(math.MaxUint64)
	for _, tc := range []struct {
		name   string
		nextId uint64
		n      uint64
		ok     bool
	}{
		{name: "the last id", nextId: top - 1, n: 1, ok: true},
		{name: "the last two ids", nextId: top - 2, n: 2, ok: true},
		{name: "two ids with one left", nextId: top - 1, n: 2},
		{name: "one id with none left", nextId: top, n: 1},
		{name: "nothing with none left", nextId: top, n: 0, ok: true},
		{name: "a run past the top", nextId: top - 5, n: top},
		{name: "an unset counter", nextId: 0, n: 3, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := &pb.SpConf{NextId: tc.nextId}
			first, err := TakeSpIds(conf, "TestOp", tc.n)
			if !tc.ok {
				precondition := wantPrecondition(t, err, "TestOp")
				if precondition.Reason != ReasonNoIdLeft {
					t.Errorf("Reason: got %q, want %q",
						precondition.Reason, ReasonNoIdLeft)
				}
				if conf.GetNextId() != tc.nextId {
					t.Errorf("the refusal moved next_id from %d to %d",
						tc.nextId, conf.GetNextId())
				}
				return
			}
			if err != nil {
				t.Fatalf("TakeSpIds: %v", err)
			}
			want := SpNextId(&pb.SpConf{NextId: tc.nextId})
			if first != want {
				t.Errorf("first id: got %d, want %d", first, want)
			}
			if got := conf.GetNextId(); got != want+tc.n {
				t.Errorf("next_id: got %d, want %d", got, want+tc.n)
			}
		})
	}
}

// storeRev is the etcd store's revision. No test of this package runs in
// parallel with another, so an op that leaves it where it was has committed
// nothing at all.
func (e *opsEnv) storeRev() int64 {
	e.t.Helper()
	_, rev, err := e.cli.RangeKeys(e.ctx, SpConfKey(e.cid, opsSpName))
	if err != nil {
		e.t.Fatalf("RangeKeys: %v", err)
	}
	return rev
}

// TestOpsTakeTheirIdsUpToTheTop is the same rule at the ops that mint ids
// (dnv-worker.md, MD6): with exactly the ids an op takes left below the
// largest 64-bit value, the op commits them, its first id the counter's, and
// leaves next_id at that value; with one fewer left it is refused with
// ReasonNoIdLeft and writes nothing.
func TestOpsTakeTheirIdsUpToTheTop(t *testing.T) {
	for _, tc := range []struct {
		name  string
		op    string
		n     uint64
		setup func(env *opsEnv)
		run   func(env *opsEnv) (uint64, error)
	}{
		{
			// One grp_id plus a leg_id/side_id pair per leg.
			name: "GrowSlice", op: opGrowSlice, n: 5,
			run: func(env *opsEnv) (uint64, error) {
				legs := []Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)}
				return GrowSlice(
					env.ctx, env.cli, env.cid, opsShard, opsSpId,
					opsSpName, noExpectRev, opsSliceId, false,
					opsNotPending, env.cc, legs,
				)
			},
		},
		{
			name: "ReplaceCntlr", op: opReplaceCntlr, n: 1,
			setup: func(env *opsEnv) {
				env.setCntlrErr(opsCntlrB, 1000)
			},
			run: func(env *opsEnv) (uint64, error) {
				now := uint64(1000 + common.DefaultCntlrUnhealthy)
				return ReplaceCntlr(
					env.ctx, env.cli, env.cid, opsShard, opsSpId,
					opsSpName, opsCntlrB, env.cnCand(opsCnC),
					[]string{opsCnA}, false, now,
				)
			},
		},
		{
			// The rep_id, the fresh new leg's leg_id and its side's
			// side_id.
			name: "CreateRepair fresh", op: opCreateRepair, n: 3,
			setup: func(env *opsEnv) {
				env.setLegErr(opsDataLegB, opsBadEpoch)
			},
			run: func(env *opsEnv) (uint64, error) {
				repId, _, err := CreateRepair(
					env.ctx, env.cli, env.cid, opsShard, opsSpId,
					opsSpName, noExpectRev, opsRepName, opsSliceParent,
					opsDataGrpId, opsDataLegB, env.freshLeg(opsDnC),
					env.cc,
				)
				return repId, err
			},
		},
		{
			// The rep_id alone: the retired leg keeps its ids.
			name: "CreateRepair from a retired leg", op: opCreateRepair, n: 1,
			setup: func(env *opsEnv) {
				env.setLegErr(opsDataLegB, opsBadEpoch)
				env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide,
					opsDnC, 1, opsRetiredEpoch)
			},
			run: func(env *opsEnv) (uint64, error) {
				repId, _, err := CreateRepair(
					env.ctx, env.cli, env.cid, opsShard, opsSpId,
					opsSpName, noExpectRev, opsRepName, opsSliceParent,
					opsDataGrpId, opsDataLegB,
					RepairNewLeg{RetiredLegId: opsRetiredLeg}, env.cc,
				)
				return repId, err
			},
		},
	} {
		t.Run(tc.name+"/the last ids", func(t *testing.T) {
			env := newOpsEnv(t)
			first := uint64(math.MaxUint64) - tc.n
			resetNextId(env, first)
			if tc.setup != nil {
				tc.setup(env)
			}
			id, err := tc.run(env)
			if err != nil {
				t.Fatalf("%s: %v", tc.op, err)
			}
			if id != first {
				t.Errorf("first id: got %d, want %d", id, first)
			}
			if got := env.spConf().GetNextId(); got != math.MaxUint64 {
				t.Errorf("next_id: got %d, want %d", got,
					uint64(math.MaxUint64))
			}
		})
		t.Run(tc.name+"/one id short", func(t *testing.T) {
			env := newOpsEnv(t)
			resetNextId(env, uint64(math.MaxUint64)-tc.n+1)
			if tc.setup != nil {
				tc.setup(env)
			}
			before := env.storeRev()
			_, err := tc.run(env)
			precondition := wantPrecondition(t, err, tc.op)
			if precondition.Reason != ReasonNoIdLeft {
				t.Errorf("Reason: got %q, want %q",
					precondition.Reason, ReasonNoIdLeft)
			}
			if got := env.storeRev(); got != before {
				t.Errorf("the refusal wrote: store revision %d, want %d",
					got, before)
			}
		})
	}
}
