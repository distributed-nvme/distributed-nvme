package worker

import (
	"context"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The worker half of the sp drain (dnv-worker.md §11.6, §13).
//
// Everything here runs through reactHarness, i.e. through the REAL
// reactionPass: the split AR3 makes (SPD6) is a property of that pass, not of a
// helper, so a test that called drainStep directly would keep passing if the
// pass stopped routing to it.

// latch is what DeleteStoragePool's STM leaves behind (§3): the flag set and
// nothing else. Everything below starts from it.
func (h *reactHarness) latch() {
	h.t.Helper()
	h.state.Conf.Deleting = true
}

// TestDrainDerivation is SPD8, all four states, in the order the derivation
// takes them. The phases are asserted through the OP SEQUENCE rather than
// through the log, because the sequence is what an owner actually commits:
// cntlrs first, then one slice at a time, then the final keys.
//
// The fourth state — not latched — is the control. Without it every row here
// would still pass on a pass that ran the drain unconditionally, which would
// tear down every healthy SP in the cluster.
func TestDrainDerivation(t *testing.T) {
	t.Run("cntlrs remain", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.latch()
		h.rops.drainCntlrCnt = 2
		h.pass()
		h.wantOps("drain_cntlrs")
		h.wantApplied()
		h.wantNoSkip()
	})

	t.Run("slices remain", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.latch()
		// D1 has committed: no cntlrs, and a second slice id listed BEFORE the
		// lower one, so "the lowest listed id" cannot be satisfied by "the
		// first listed id".
		h.state.Conf.CntlrIdList = nil
		h.state.Conf.SliceIdList = []uint64{reactSliceId + 5, reactSliceId}
		h.rops.drainGrpCnt = 1
		h.pass()
		calls := h.wantOps("drain_slice")
		if calls[0].sliceId != reactSliceId {
			t.Errorf("batch targeted slice %d, want the LOWEST listed (%d)",
				calls[0].sliceId, reactSliceId)
		}
	})

	t.Run("nothing remains", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.latch()
		h.state.Conf.CntlrIdList = nil
		h.state.Conf.SliceIdList = nil
		h.pass()
		h.wantOps("finish_delete")
		if got := len(h.logs.withMsg(msgSpDrainDone)); got != 1 {
			t.Errorf("%q records = %d, want 1", msgSpDrainDone, got)
		}
	})

	t.Run("not latched", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		// A perfectly healthy SP: no drain op may run, at all.
		h.pass()
		h.wantOps()
		if got := len(h.logs.withMsg(msgSpDrainStep)); got != 0 {
			t.Errorf("%q records = %d, want 0", msgSpDrainStep, got)
		}
	})
}

// TestDrainRunsAtEverySpLevel is SPD6's "regardless of sp_level suppression,
// because a doomed SP must drain at any level".
//
// AR3 used to make `deleting` a suppression reason, so a latched SP at
// SP_LEVEL_NO_THINPOOL would have been suppressed twice over. The split means
// neither suppression applies: the drain step runs, and — the second half, and
// the one a reader is most likely to get wrong — NO `reaction suppressed`
// record is emitted, because nothing was suppressed.
func TestDrainRunsAtEverySpLevel(t *testing.T) {
	for _, level := range []pb.SpLevel{
		pb.SpLevel_SP_LEVEL_READWRITE,
		pb.SpLevel_SP_LEVEL_READONLY,
		pb.SpLevel_SP_LEVEL_NO_CLONE,
		pb.SpLevel_SP_LEVEL_NO_MIGRATION,
		pb.SpLevel_SP_LEVEL_NO_THINPOOL,
		pb.SpLevel_SP_LEVEL_NO_SIDE,
		pb.SpLevel_SP_LEVEL_DISABLE,
	} {
		t.Run(level.String(), func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.latch()
			h.state.Conf.SpLevel = level
			h.rops.drainCntlrCnt = 2
			// An unhealthy primary as well: a pass that fell through to the
			// reactions instead of the drain would run a failover here, which
			// is exactly what SPD6 forbids for a latched SP.
			h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
			h.pass()
			h.wantOps("drain_cntlrs")
			if got := len(h.logs.withMsg(msgReactionSuppressed)); got != 0 {
				t.Errorf("%q records = %d, want 0: a drain is not a suppression",
					msgReactionSuppressed, got)
			}
			h.wantApplied()
		})
	}
}

// TestDrainFailedStepIsLogged is §0 #9 and §7: a failed step commits nothing,
// logs `sp drain failed` with the phase and the cause, and is simply retried on
// the next tick. There is no terminal-failure state and no error field — a
// delete that gave up would just strand garbage.
//
// The `reason` attribute is asserted separately from `error`, because an
// SPD2 refusal is a model.ErrPrecondition whose Reason is the whole diagnosis
// ("sp not deleting", "cntlr not found"), and flattening it into the error text
// would make the one thing an operator greps for unfindable.
func TestDrainFailedStepIsLogged(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.latch()
	h.rops.drainErr = &model.ErrPrecondition{
		Op: "DrainSpCntlrs", Reason: "cntlr not found",
	}
	h.pass()
	h.wantOps("drain_cntlrs")
	recs := h.logs.withMsg(msgSpDrainFailed)
	if len(recs) != 1 {
		t.Fatalf("%q records = %d, want 1", msgSpDrainFailed, len(recs))
	}
	if recs[0]["phase"] != drainPhaseCntlrs {
		t.Errorf("phase = %v, want %q", recs[0]["phase"], drainPhaseCntlrs)
	}
	if recs[0]["reason"] != "cntlr not found" {
		t.Errorf("reason = %v, want %q", recs[0]["reason"], "cntlr not found")
	}
	if recs[0]["error"] == nil {
		t.Errorf("the record must carry the error text as well")
	}
	if got := len(h.logs.withMsg(msgSpDrainStep)); got != 0 {
		t.Errorf("a failed step logged %q", msgSpDrainStep)
	}
	// Retried on the next pass, with no backoff and no memo of the failure
	// (RW12): the next pass re-derives the same step from the same SpConf.
	h.pass()
	h.wantOps("drain_cntlrs", "drain_cntlrs")
}

// TestDrainStepRecordsProgress pins the non-normative `sp drain step` record
// that makes a multi-batch drain visible: the phase, the target and how much
// the step actually removed.
func TestDrainStepRecordsProgress(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.latch()
	h.state.Conf.CntlrIdList = nil
	h.rops.drainGrpCnt = 7
	h.rops.drainDone = true
	h.pass()
	recs := h.logs.withMsg(msgSpDrainStep)
	if len(recs) != 1 {
		t.Fatalf("%q records = %d, want 1", msgSpDrainStep, len(recs))
	}
	if recs[0]["phase"] != drainPhaseSlice {
		t.Errorf("phase = %v, want %q", recs[0]["phase"], drainPhaseSlice)
	}
	if recs[0]["slice_id"] != float64(reactSliceId) {
		t.Errorf("slice_id = %v, want %d", recs[0]["slice_id"], reactSliceId)
	}
	if recs[0]["grp_cnt"] != float64(7) {
		t.Errorf("grp_cnt = %v, want 7", recs[0]["grp_cnt"])
	}
	if recs[0]["slice_done"] != true {
		t.Errorf("slice_done = %v, want true", recs[0]["slice_done"])
	}
}

// TestDrainArmsItsOwnTick is §0 #8's mechanism: a committed step that moved the
// SP schedules the next pass AT ONCE, instead of waiting for the
// cntlr_interval ticker.
//
// Three properties, and all three matter:
//
//   - a step that PROGRESSED arms, which is what makes a 16-slice drain take
//     sixteen sub-second transactions rather than sixteen tick periods;
//   - a step that removed NOTHING does not, so the idempotent no-op an accepted
//     two-owner overlap produces cannot spin a pass that can only find the same
//     nothing (the RW12 tick picks it up instead);
//   - the token is coalesced, so a burst of steps can never queue passes up.
//
// The channel is read directly rather than through run(): run()'s arm is one
// select case that turns a token into reactionPass, and a live coordinator
// would need a whole etcd to reach a drain op.
func TestDrainArmsItsOwnTick(t *testing.T) {
	w := &spWorker{drainCh: make(chan struct{}, 1)}
	ctx := context.Background()
	captureLogs(t)

	w.drainStepped(ctx, drainPhaseSlice, false, nil...)
	select {
	case <-w.drainCh:
		t.Fatalf("a step that removed nothing armed the next pass")
	default:
	}

	w.drainStepped(ctx, drainPhaseSlice, true, nil...)
	w.drainStepped(ctx, drainPhaseSlice, true, nil...)
	select {
	case <-w.drainCh:
	default:
		t.Fatalf("a step that progressed did not arm the next pass")
	}
	select {
	case <-w.drainCh:
		t.Fatalf("two steps queued two passes; the token must coalesce")
	default:
	}

	// A coordinator assembled by hand — every plan-only fixture in this
	// package — has no loop to wake and no channel, and arming must be inert
	// rather than a nil-channel block.
	bare := &spWorker{}
	bare.drainStepped(ctx, drainPhaseFinal, true, nil...)
}

// TestDrainStepArmsOnlyOnProgress pins the value drainStep COMPUTES, which
// TestDrainArmsItsOwnTick does not reach: that test calls drainStepped
// directly, so a drainStep that passed a literal `true` would leave it green.
//
// The mutation this catches is not a missed optimisation. Under the accepted
// two-owner overlap the loser's step removes nothing; arming on it would wake
// a pass that can only find the same nothing and arm again — a hot loop on the
// coordinator goroutine with no tick pacing it.
func TestDrainStepArmsOnlyOnProgress(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(h *reactHarness)
		armed   bool
	}{
		{"D1 removed cntlrs", func(h *reactHarness) {
			h.rops.drainCntlrCnt = 2
		}, true},
		{"D1 removed nothing", func(h *reactHarness) {
			h.rops.drainCntlrCnt = 0
		}, false},
		{"D2 removed groups", func(h *reactHarness) {
			h.state.Conf.CntlrIdList = nil
			h.rops.drainGrpCnt = 3
		}, true},
		{"D2 emptied the slice without popping", func(h *reactHarness) {
			// removed == 0 but done == true: the batch found the slice already
			// empty and committed its removal, which IS progress.
			h.state.Conf.CntlrIdList = nil
			h.rops.drainGrpCnt = 0
			h.rops.drainDone = true
		}, true},
		{"D2 removed nothing", func(h *reactHarness) {
			h.state.Conf.CntlrIdList = nil
			h.rops.drainGrpCnt = 0
		}, false},
		{"D3 never arms", func(h *reactHarness) {
			// The last step deletes SpRev, which is the shard worker's stop
			// signal; arming would wake a pass with nothing left to load.
			h.state.Conf.CntlrIdList = nil
			h.state.Conf.SliceIdList = nil
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.latch()
			h.w.drainCh = make(chan struct{}, 1)
			tc.prepare(h)
			h.pass()
			armed := len(h.w.drainCh) == 1
			if armed != tc.armed {
				t.Errorf("armed = %v, want %v", armed, tc.armed)
			}
		})
	}
}

// TestDrainSelfTickIsWired is §0 #8's other half: run() must actually TURN an
// armed token into a pass, and startSpWorker must give the coordinator a
// channel to arm in the first place.
//
// Without it, deleting either the `case <-w.drainCh` arm or the channel's
// allocation leaves every test green and the drain still finishing — one step
// per cntlr_interval instead of one per commit, which the integration suite's
// 65-second budget cannot see either.
//
// The fixture makes loadSp report ErrNotFound, so the pass this wakes returns
// immediately: what is under test is the wiring, not what the pass then does,
// and the harness's reactor holds a nil etcd client that a real drain op would
// dereference.
func TestDrainSelfTickIsWired(t *testing.T) {
	h := newSpHarness(t)
	h.ops.setErr(model.ErrNotFound)
	w := h.start()
	if w.drainCh == nil {
		t.Fatalf("startSpWorker left drainCh nil: armDrain is inert")
	}
	if cap(w.drainCh) != 1 {
		t.Errorf("drainCh capacity %d, want 1 (one pending pass, coalesced)",
			cap(w.drainCh))
	}
	w.armDrain()
	waitFor(t, "run() to consume the armed drain token", func() bool {
		return len(w.drainCh) == 0
	})
}

// TestDrainZeroCntlrSidesAreIdle is SPD7: D1 leaves an SP with no cntlr at all
// — a shape CreateStoragePool can never produce — and the fan-out must tolerate
// it.
//
// "Tolerate" is idle and not "send primary_cn_id = 0". agent/dnagent's cnIdsOf
// puts the primary at the head of the export list unconditionally, so an empty
// cntlr set would have every DN of the doomed SP BUILD a dm-error, a dm-linear,
// a subsystem and a namespace for the CN numbered 0 — new garbage, created
// while the SP is being torn down.
//
// The cntlr half of SPD7 is the same assertion from the other side: zero cntlr
// plans, and no error.
func TestDrainZeroCntlrSidesAreIdle(t *testing.T) {
	logs := captureLogs(t)
	state := spFixture()
	state.Conf.Deleting = true
	state.Conf.CntlrIdList = nil
	state.Cntlrs = map[uint64]*pb.Cntlr{}
	w := spTestWorker(nil)
	plan := w.buildPlan(context.Background(), state)

	if len(plan.sides) != 0 {
		t.Errorf("%d side children for an SP with no cntlr, want 0",
			len(plan.sides))
	}
	if len(plan.cntlrs) != 0 {
		t.Errorf("%d cntlr children for an SP with no cntlr, want 0",
			len(plan.cntlrs))
	}
	// Nothing is UNRESOLVED — there is nothing left to resolve — so the
	// coordinator must not arm a re-resolution tick for it.
	if plan.unresolved != 0 {
		t.Errorf("unresolved = %d, want 0", plan.unresolved)
	}
	// The legs are still indexed, which is what keeps the HL2 leg rows landing
	// on their slice for as long as any cntlr still reports them.
	if len(plan.legSlice) == 0 {
		t.Errorf("the leg index must survive a cntlr-less fan-out")
	}
	recs := logs.withMsg(msgSpSidesIdle)
	if len(recs) != 1 {
		t.Fatalf("%q records = %d, want 1", msgSpSidesIdle, len(recs))
	}
	if recs[0]["reason"] != "no cntlr" {
		t.Errorf("reason = %v, want %q", recs[0]["reason"], "no cntlr")
	}
}
