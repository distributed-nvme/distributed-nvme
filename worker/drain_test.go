package worker

import (
	"context"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The worker half of the sp drain (dnv-worker.md, The sp drain).
//
// Everything here runs through reactHarness, i.e. through the REAL
// reactionPass or the REAL drainTick, the drain's wake-up: the split AR3 makes
// (SPD6) is a property of those two, not of a helper, so a test that called
// drainStep directly would keep passing if either stopped routing to it.

// latch is what DeleteStoragePool's STM leaves behind (SPD4): the deleting
// flag set and no other conf change. Everything below starts from it.
func (h *reactHarness) latch() {
	h.t.Helper()
	h.state.Conf.Deleting = true
}

// drainTick runs one drain wake-up, which is what run() does with the token a
// drain step posts (SPD6, CLD7).
func (h *reactHarness) drainTick() {
	h.t.Helper()
	h.w.drainTick(context.Background())
}

// drainPaths are the two ways a drain step is reached: the reaction pass and
// the drain's own wake-up (SPD6, CLD7). A test of what a step does runs
// through both, so a step that behaves on one path and not on the other
// fails.
var drainPaths = []struct {
	name string
	run  func(h *reactHarness)
}{
	{"pass", (*reactHarness).pass},
	{"wake-up", (*reactHarness).drainTick},
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
// `deleting` is not a suppression reason (SPD6 keeps it out of AR3), so the
// drain step runs on a latched SP at SP_LEVEL_NO_THINPOOL, and — the second
// half, and the one a reader is most likely to get wrong — NO `reaction
// suppressed` record is emitted, because nothing was suppressed.
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

// TestDrainRunsAheadOfTheSpGates is AR1's order for a latched SP: its drain
// step runs ahead of the SP's own two gates, its bdev_conf and its groups'
// lengths to zero, because the drain reads no geometry, and either gate in
// front of it would leave an SP whose stored geometry cannot be read
// undeletable, the latch being one-way (SPD5). The step runs and nothing is
// refused; an unhealthy primary shows that the pass did not fall through to
// the reactions either.
func TestDrainRunsAheadOfTheSpGates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(h *reactHarness)
	}{
		{
			name: "an unreadable bdev_conf",
			corrupt: func(h *reactHarness) {
				h.state.Conf.BdevConf.DmRaid0Conf.StripeSize = 0
			},
		},
		{
			name: "a group with no length to zero",
			corrupt: func(h *reactHarness) {
				h.dataGrp().MetaBlocks = 0
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.latch()
			h.rops.drainCntlrCnt = 2
			h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
			tc.corrupt(h)
			h.pass()
			h.wantOps("drain_cntlrs")
			h.wantApplied()
			if got := len(h.logs.withMsg(msgInvalidStoredConf)); got != 0 {
				t.Errorf("%q records = %d, want 0: the drain reads no "+
					"geometry", msgInvalidStoredConf, got)
			}
		})
	}
}

// TestDrainFailedStepIsLogged is SPD6 and its record (dnv-worker.md, Log
// records): a step refused before its commit changes nothing, logs
// `sp drain failed` with the phase and the cause, and is simply retried on
// the next tick. There is no terminal-failure state and no status field on
// the SpConf (SPD6) — a delete that gave up would just strand garbage. The
// RECORD does carry `error`, asserted below.
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

// TestDrainArmsItsOwnTick is SPD6's self-tick: a step whose call succeeded and
// that moved the SP posts the drain's wake-up AT ONCE, instead of waiting for
// the cntlr_interval ticker.
//
// Three properties, and all three matter:
//
//   - a step that PROGRESSED arms, which is what makes a 16-slice drain take
//     sixteen sub-second transactions rather than sixteen tick periods;
//   - a step that removed NOTHING does not, so the idempotent no-op an accepted
//     two-owner overlap produces cannot spin a wake-up that can only find the
//     same nothing (the RW12 tick picks it up instead);
//   - the token is coalesced, so a burst of steps can never queue wake-ups up.
//
// The channel is read directly rather than through run(): run()'s arm is one
// select case that turns a token into drainTick, which TestDrainSelfTickIsWired
// pins on a live coordinator.
func TestDrainArmsItsOwnTick(t *testing.T) {
	w := &spWorker{drainCh: make(chan struct{}, 1)}
	ctx := context.Background()
	captureLogs(t)

	w.drainStepped(ctx, drainPhaseSlice, false, nil...)
	select {
	case <-w.drainCh:
		t.Fatalf("a step that removed nothing armed a wake-up")
	default:
	}

	w.drainStepped(ctx, drainPhaseSlice, true, nil...)
	w.drainStepped(ctx, drainPhaseSlice, true, nil...)
	select {
	case <-w.drainCh:
	default:
		t.Fatalf("a step that progressed did not arm a wake-up")
	}
	select {
	case <-w.drainCh:
		t.Fatalf("two steps queued two wake-ups; the token must coalesce")
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
// a drainTick that can only find the same nothing and arm again — a hot loop
// on the coordinator goroutine with no tick pacing it. Every row runs through
// the pass and through the wake-up, because the wake-up is the path a
// self-continuing drain takes after its first step.
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
			// signal; arming would wake a drainTick with nothing left to
			// load.
			h.state.Conf.CntlrIdList = nil
			h.state.Conf.SliceIdList = nil
		}, false},
	} {
		for _, via := range drainPaths {
			t.Run(tc.name+"/"+via.name, func(t *testing.T) {
				h := newReactHarness(t, reactFixture(t))
				h.latch()
				h.w.drainCh = make(chan struct{}, 1)
				tc.prepare(h)
				via.run(h)
				armed := len(h.w.drainCh) == 1
				if armed != tc.armed {
					t.Errorf("armed = %v, want %v", armed, tc.armed)
				}
			})
		}
	}
}

// TestDrainSelfTickIsWired is SPD6's other half: run() must actually TURN an
// armed token into the drain's wake-up (drainTick) — the drain step alone, not
// a reaction pass — and startSpWorker must give the coordinator a channel to
// arm in the first place.
//
// Without it, deleting either the `case <-w.drainCh` arm or the channel's
// allocation leaves every test green and the drain still finishing — one step
// per cntlr_interval instead of one per step that posts a wake-up, which the
// integration suite's 65-second budget cannot see either. And a case that ran
// reactionPass instead would re-evaluate every reaction and re-seed every
// health memo once per step that posts a wake-up.
//
// The fixture tells the two apart through AR3: its SP sits at
// SP_LEVEL_NO_THINPOOL with a latched clone, and passes every gate of AR1
// (testClusterConf, spFixture's bdev_conf and groups), so a reaction pass
// logs `reaction suppressed` after its clone drain step, and the wake-up logs
// no such record. The fan-out loads ErrNotFound and so builds no child; the
// later loads succeed. The clone batch fails on purpose: a failed step posts
// no token, so the wake-up does not feed itself, and the second token, posted
// once the first wake-up has made its call, is taken only after that wake-up
// has returned. The test stops the coordinator, which joins run(), before it
// counts the records, so every record both wake-ups log is in by then.
func TestDrainSelfTickIsWired(t *testing.T) {
	h := newSpHarness(t)
	state := spFixture()
	state.Clones[spCloneNm].Deleting = true
	state.Conf.SpLevel = pb.SpLevel_SP_LEVEL_NO_THINPOOL
	h.ops.setState(state)
	h.ops.setErr(model.ErrNotFound)
	w := h.start()
	if w.drainCh == nil {
		t.Fatalf("startSpWorker left drainCh nil: armDrain is inert")
	}
	if cap(w.drainCh) != 1 {
		t.Errorf("drainCh capacity %d, want 1 (one pending wake-up, "+
			"coalesced)", cap(w.drainCh))
	}
	waitFor(t, "the fan-out's load", func() bool {
		return h.ops.loadCnt() >= 1
	})
	h.ops.setErr(nil)
	rops := &fakeReactionOps{cloneDrainErr: &model.ErrPrecondition{
		Op: "DrainCloneBm", Reason: "clone not found",
	}}
	h.reactWith(w, rops)

	w.armDrain()
	waitFor(t, "run() to turn the token into a clone drain step", func() bool {
		return len(rops.allCalls()) >= 1
	})
	w.armDrain()
	waitFor(t, "run() to turn the second token into a step", func() bool {
		return len(rops.allCalls()) >= 2
	})
	// The second wake-up logs its record after its call returns: join run()
	// before counting.
	w.stop()
	calls := rops.allCalls()
	if len(calls) != 2 || calls[0].op != "drain_clone_bm" ||
		calls[1].op != "drain_clone_bm" {
		t.Fatalf("ops = %v, want two clone batches, one per token", calls)
	}
	if got := len(h.logs.withMsg(msgReactionSuppressed)); got != 0 {
		t.Errorf("%q records = %d, want 0: the token ran a reaction pass, "+
			"not the drain alone", msgReactionSuppressed, got)
	}
	if got := len(h.logs.withMsg(msgCloneDrainFailed)); got != 2 {
		t.Errorf("%q records = %d, want 2", msgCloneDrainFailed, got)
	}
}

// TestDrainTickRunsTheDrainAlone is the wake-up's half of SPD6 and CLD7: the
// token a step posts runs the next drain step and nothing else — no reaction,
// no candidate scan, no AR3 record, and no pass built (newPass), whose AR10
// snapshot would prune the verdict memo.
//
// Every row but one carries a trigger the pass acts on, and runs the same
// fixture through the pass afterwards as its control: an unhealthy primary
// with a healthy candidate is AR5's failover, a suppressed sp_level is AR3's
// record, and a verdict on a cntlr the SP no longer lists is what newPass
// prunes, in a latched SP's pass as in a live SP's. The row of a latched SP
// beside AR5's trigger has no control, since a latched SP's pass runs no
// reaction either (SPD6): it only proves that the wake-up adds no reaction,
// AR5's being the one armed.
func TestDrainTickRunsTheDrainAlone(t *testing.T) {
	t.Run("a deleting clone beside AR5's trigger", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.withClone("c0", 700, [][2]uint32{{0, 0}})
		h.latchClone("c0")
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
		h.drainTick()
		h.wantOps("drain_clone_bm")
		if got := h.rops.allQueries(); len(got) != 0 {
			t.Errorf("the wake-up ran candidate scans: %v", got)
		}
		h.wantApplied()
		h.wantNoSkip()
		// The control: the pass drains AND reacts.
		h.pass()
		h.wantOps("drain_clone_bm", "drain_clone_bm", "failover")
	})

	t.Run("a deleting clone at a suppressed sp_level", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.withClone("c0", 700, [][2]uint32{{0, 0}})
		h.latchClone("c0")
		h.state.Conf.SpLevel = pb.SpLevel_SP_LEVEL_NO_THINPOOL
		h.drainTick()
		h.wantOps("drain_clone_bm")
		if got := len(h.logs.withMsg(msgReactionSuppressed)); got != 0 {
			t.Errorf("%q records = %d, want 0", msgReactionSuppressed, got)
		}
		h.pass()
		if got := len(h.logs.withMsg(msgReactionSuppressed)); got != 1 {
			t.Errorf("%q records after the pass = %d, want 1",
				msgReactionSuppressed, got)
		}
	})

	t.Run("a latched SP beside AR5's trigger", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.latch()
		h.rops.drainCntlrCnt = 2
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
		h.drainTick()
		h.wantOps("drain_cntlrs")
		h.wantApplied()
		h.wantNoSkip()
	})

	for _, tc := range []struct {
		name    string
		prepare func(h *reactHarness)
		op      string
	}{
		{"no pass is built for a deleting clone", func(h *reactHarness) {
			h.withClone("c0", 700, [][2]uint32{{0, 0}})
			h.latchClone("c0")
		}, "drain_clone_bm"},
		{"no pass is built for a latched SP", func(h *reactHarness) {
			h.latch()
			h.rops.drainCntlrCnt = 2
		}, "drain_cntlrs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			tc.prepare(h)
			// A verdict on a cntlr the snapshot does not list: newPass drops
			// it (verdictMemo.snapshot), and nothing else does.
			const gone = uint64(99)
			h.w.verdicts.noteCntlr(gone, true)
			held := func() bool {
				h.w.verdicts.mu.Lock()
				defer h.w.verdicts.mu.Unlock()
				return h.w.verdicts.cntlrs[gone]
			}
			h.drainTick()
			h.wantOps(tc.op)
			if !held() {
				t.Errorf("the wake-up pruned AR10's memo: it built a pass")
			}
			h.pass()
			if held() {
				t.Fatalf("the control failed: the pass kept a verdict on a " +
					"cntlr its snapshot does not list")
			}
		})
	}
}

// TestDrainTickReseedsNoHealth pins the wake-up's other half of "alone": it
// re-seeds no health memo (HL3). The pass offers each child the err_epoch its
// load read (TestReactionPassReseedsHealthEveryLoad); a wake-up offers
// nothing, since a drain step calls no agent and the cntlr_interval pass is
// the load that keeps the memos in step with the records.
//
// The stamp another observer left on the standby's record is the probe: had
// the wake-up offered it, the clean verdict after it would be a transition
// and write a clear. The pass after it is the control, for both drains.
func TestDrainTickReseedsNoHealth(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(h *reactHarness)
		op      string
	}{
		{"the clone drain", func(h *reactHarness) {
			h.withClone("c0", 700, [][2]uint32{{0, 0}})
			h.latchClone("c0")
		}, "drain_clone_bm"},
		{"the sp drain", func(h *reactHarness) {
			h.latch()
			h.rops.drainCntlrCnt = 2
		}, "drain_cntlrs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newReactHarness(t, reactFixture(t))
			tc.prepare(h)
			hw := &fakeHealthWriter{}
			h.deps.health = hw
			standby := newCntlrMonitor(h.deps, testCid, testSpId, reactCntlrB)
			h.w.cntlrs[reactCntlrB] = &cntlrChild{
				driver: &cntlrDriver{health: standby},
			}
			standby.observe(ctx, healthClean, "")
			if got := len(hw.all()); got != 1 {
				t.Fatalf("epoch writes after the first verdict = %d, want 1",
					got)
			}
			h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(1)

			h.drainTick()
			h.wantOps(tc.op)
			standby.observe(ctx, healthClean, "")
			if got := len(hw.all()); got != 1 {
				t.Fatalf("epoch writes after the wake-up = %d, want 1: the "+
					"wake-up offered the standby the record it loaded", got)
			}

			h.pass()
			h.wantOps(tc.op, tc.op)
			standby.observe(ctx, healthClean, "")
			if got := len(hw.all()); got != 2 {
				t.Fatalf("epoch writes after the pass = %d, want 2: the "+
					"control failed, the pass did not offer the stamp", got)
			}
		})
	}
}

// TestDrainTickGates pins the gates the wake-up keeps, each the pass's own
// (SPD6, CLD7). A latched SP's step stays behind the cluster-conf gate,
// because a D2 batch maintains DN capacity keys whose bins come from the
// ladder: a cluster missing from the cache runs nothing and logs nothing, an
// invalid conf runs nothing and logs `invalid stored conf` once, and a usable
// conf runs the step and clears that refusal's memo. The invalid conf's row
// runs through the pass as well, whose deleting branch keeps the same gate
// and the same clear. The SP's own two gates stay out of its way. A live SP's
// clone drain batch takes no gate at all, the cluster-conf gate included —
// and unlike the pass, whose reactions still refuse a bad conf, the wake-up
// then logs no refusal either (the final step's own conf check is
// TestCloneDrainFinalStepTakesTheClusterConf's). Nor does it clear the
// pass's refusal memo, which the wake-up clears only for a latched SP's
// step, so a steady bad conf costs one record however many wake-ups fall
// between two passes (dnv-worker.md, Log records).
func TestDrainTickGates(t *testing.T) {
	t.Run("sp drain/with the cluster missing from the cache", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.latch()
		h.rops.drainCntlrCnt = 2
		h.deps.conf.mu.Lock()
		delete(h.deps.conf.entries, testCid)
		h.deps.conf.mu.Unlock()
		h.drainTick()
		h.wantOps()
		if got := len(h.logs.withMsg(msgInvalidStoredConf)); got != 0 {
			t.Errorf("%q records = %d, want 0", msgInvalidStoredConf, got)
		}
	})

	for _, via := range drainPaths {
		t.Run("sp drain/with an invalid cluster conf/"+via.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.latch()
			h.rops.drainCntlrCnt = 2
			bad := reactClusterConf()
			bad.DnBinConf = nil
			setCachedConf(h.deps, testCid, bad)
			via.run(h)
			via.run(h)
			h.wantOps()
			if got := len(h.logs.withMsg(msgInvalidStoredConf)); got != 1 {
				t.Fatalf("%q records = %d, want 1: refused, once",
					msgInvalidStoredConf, got)
			}
			// A usable conf runs the step and clears the memo, so the same
			// refusal is recorded again when it comes back.
			setCachedConf(h.deps, testCid, reactClusterConf())
			via.run(h)
			h.wantOps("drain_cntlrs")
			setCachedConf(h.deps, testCid, bad)
			via.run(h)
			h.wantOps("drain_cntlrs")
			if got := len(h.logs.withMsg(msgInvalidStoredConf)); got != 2 {
				t.Errorf("%q records = %d, want 2: the step that ran did "+
					"not clear the refusal's memo", msgInvalidStoredConf, got)
			}
		})
	}

	for _, tc := range []struct {
		name    string
		corrupt func(h *reactHarness)
	}{
		{"with an unreadable bdev_conf", func(h *reactHarness) {
			h.state.Conf.BdevConf.DmRaid0Conf.StripeSize = 0
		}},
		{"with a group with no length to zero", func(h *reactHarness) {
			h.dataGrp().MetaBlocks = 0
		}},
	} {
		t.Run("sp drain/"+tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.latch()
			h.rops.drainCntlrCnt = 2
			tc.corrupt(h)
			h.drainTick()
			h.wantOps("drain_cntlrs")
			if got := len(h.logs.withMsg(msgInvalidStoredConf)); got != 0 {
				t.Errorf("%q records = %d, want 0", msgInvalidStoredConf, got)
			}
		})
	}

	for _, tc := range []struct {
		name    string
		corrupt func(h *reactHarness)
		// passRecords is what the pass logs for the same fixture, the
		// control: its reactions still refuse.
		passRecords int
	}{
		{"with the cluster missing from the cache", func(h *reactHarness) {
			h.deps.conf.mu.Lock()
			delete(h.deps.conf.entries, testCid)
			h.deps.conf.mu.Unlock()
		}, 0},
		{"with an invalid cluster conf", func(h *reactHarness) {
			cc := reactClusterConf()
			cc.DnBinConf = nil
			setCachedConf(h.deps, testCid, cc)
		}, 1},
		{"with an unreadable bdev_conf", func(h *reactHarness) {
			h.state.Conf.BdevConf.DmRaid0Conf.StripeSize = 0
		}, 1},
		{"with a group with no length to zero", func(h *reactHarness) {
			h.dataGrp().MetaBlocks = 0
		}, 1},
	} {
		t.Run("clone drain/"+tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.withClone("c0", 700, [][2]uint32{{0, 0}})
			h.latchClone("c0")
			tc.corrupt(h)
			h.drainTick()
			h.wantOps("drain_clone_bm")
			if got := len(h.logs.withMsg(msgInvalidStoredConf)); got != 0 {
				t.Errorf("%q records = %d, want 0: the clone drain takes no "+
					"gate", msgInvalidStoredConf, got)
			}
			h.pass()
			h.wantOps("drain_clone_bm", "drain_clone_bm")
			if got := len(h.logs.withMsg(msgInvalidStoredConf)); got != tc.passRecords {
				t.Errorf("%q records after the pass = %d, want %d",
					msgInvalidStoredConf, got, tc.passRecords)
			}
			// A wake-up between two passes leaves the pass's refusal memo
			// as it is, so the next pass records nothing new.
			h.drainTick()
			h.pass()
			h.wantOps("drain_clone_bm", "drain_clone_bm", "drain_clone_bm",
				"drain_clone_bm")
			if got := len(h.logs.withMsg(msgInvalidStoredConf)); got != tc.passRecords {
				t.Errorf("%q records after a wake-up and a pass = %d, want "+
					"%d: the wake-up cleared the pass's refusal memo",
					msgInvalidStoredConf, got, tc.passRecords)
			}
		})
	}
}

// TestDrainTickLoadFailure is AR1's two load outcomes on the wake-up's load:
// an SP that is gone is silent, a transient failure is recorded, and neither
// runs a step — the next tick's pass retries (RW12).
func TestDrainTickLoadFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		records int
	}{
		{"deleted", model.ErrNotFound, 0},
		{"transient", context.DeadlineExceeded, 1},
	} {
		for _, drain := range []struct {
			name    string
			prepare func(h *reactHarness)
		}{
			{"clone drain", func(h *reactHarness) {
				h.withClone("c0", 700, [][2]uint32{{0, 0}})
				h.latchClone("c0")
			}},
			{"sp drain", func(h *reactHarness) {
				h.latch()
				h.rops.drainCntlrCnt = 2
			}},
		} {
			t.Run(tc.name+"/"+drain.name, func(t *testing.T) {
				h := newReactHarness(t, reactFixture(t))
				drain.prepare(h)
				h.ops.setErr(tc.err)
				h.drainTick()
				h.wantOps()
				if got := len(h.logs.withMsg(msgSpLoadFailed)); got != tc.records {
					t.Errorf("%q records = %d, want %d",
						msgSpLoadFailed, got, tc.records)
				}
			})
		}
	}
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
