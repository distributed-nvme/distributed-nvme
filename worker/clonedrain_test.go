package worker

import (
	"context"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The worker half of the clone drain (dnv-worker.md §11.7 and §13).
//
// Everything routed through the REAL reactionPass, as for the sp drain: CLD7's
// "alongside, not instead of" is a property of that pass, and a test that
// called drainClones directly would keep passing if the pass stopped calling
// it.

// latchClone marks one clone of the fixture as being deleted.
func (h *reactHarness) latchClone(name string) {
	h.t.Helper()
	clone, ok := h.state.Clones[name]
	if !ok {
		h.t.Fatalf("no clone %q in the fixture", name)
	}
	clone.Deleting = true
}

// withClone adds one clone with the given chunk pairs to a reaction fixture,
// whose SP carries none.
func (h *reactHarness) withClone(name string, id uint64, pairs [][2]uint32) {
	h.t.Helper()
	// reactFixture's SP carries no clone at all, so the two maps are nil.
	if h.state.Clones == nil {
		h.state.Clones = make(map[string]*pb.Clone)
	}
	if h.state.CloneBmIdx == nil {
		h.state.CloneBmIdx = make(map[string][]model.BmChunk)
	}
	h.state.Conf.CloneNameList = append(h.state.Conf.GetCloneNameList(), name)
	h.state.Clones[name] = &pb.Clone{CloneId: id, BmCnt: uint32(len(pairs))}
	chunks := make([]model.BmChunk, 0, len(pairs))
	for _, pair := range pairs {
		chunks = append(chunks, model.BmChunk{
			SliceIdx: pair[0], Idx: pair[1],
		})
	}
	h.state.CloneBmIdx[name] = chunks
}

// TestCloneDrainDerivation is CLD7's derivation half, all three states, plus
// the control that keeps a live clone out of it.
func TestCloneDrainDerivation(t *testing.T) {
	t.Run("chunks remain", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.withClone("c0", 700, [][2]uint32{{0, 0}, {1, 1}})
		h.latchClone("c0")
		h.pass()
		calls := h.wantOps("drain_clone_bm")
		if calls[0].cloneName != "c0" || calls[0].cloneId != 700 {
			t.Errorf("batch targeted %q/%d, want c0/700",
				calls[0].cloneName, calls[0].cloneId)
		}
		// The §12 record, whose attribute name is the operator's. chunk_cnt
		// and NOT bm_cnt: nothing else in the tree pins the worker's own
		// record — the shell suites read `wctl drain-clone`'s JSON, a
		// different producer — so without this the rename could go back, or a
		// second `bm_cnt` could appear beside it, with the whole suite green.
		recs := h.logs.withMsg(msgCloneDrainStep)
		if len(recs) != 1 {
			t.Fatalf("%q records = %d, want 1", msgCloneDrainStep, len(recs))
		}
		if recs[0]["chunk_cnt"] != float64(2) {
			t.Errorf("chunk_cnt = %v, want 2", recs[0]["chunk_cnt"])
		}
		if recs[0]["bm_cnt"] != nil {
			t.Errorf("bm_cnt = %v, want no such attribute (it is the Clone's "+
				"high-water, a different number)", recs[0]["bm_cnt"])
		}
	})

	t.Run("none remain", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.withClone("c0", 700, nil)
		// bm_cnt says 13 while the scan says zero, which is the TERMINAL state
		// of every real drain: AppendCloneBitmap only ever raised the
		// high-water and CLD8 forbids the batches from lowering it. Without
		// this line the fixture's counter and its keys agree in every case,
		// and a derivation that read `bm_cnt == 0` instead of the surviving
		// keys would pass the whole file while never finishing a real drain —
		// it would hand an empty batch to an op that commits nothing, and
		// CLD12's unconditional arm would spin on it for ever.
		h.state.Clones["c0"].BmCnt = 13
		h.latchClone("c0")
		h.pass()
		h.wantOps("finish_clone")
		if got := len(h.logs.withMsg(msgCloneDrainDone)); got != 1 {
			t.Errorf("%q records = %d, want 1", msgCloneDrainDone, got)
		}
	})

	t.Run("a live clone is never drained", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.withClone("c0", 700, [][2]uint32{{0, 0}})
		h.pass()
		h.wantOps()
		if got := len(h.logs.withMsg(msgCloneDrainStep)); got != 0 {
			t.Errorf("%q records = %d, want 0", msgCloneDrainStep, got)
		}
	})
}

// TestCloneDrainBatchIsCutAndOrdered is CLD8's two rules where they are
// applied: the batch handed to the op is at most MaxDelBmPerTxn chunks, and it
// is the LOWEST of them in (src_slice_idx, bm_idx) order.
//
// The fixture's chunks are planted out of order, so "the lowest" cannot be
// satisfied by "the first the scan happened to return" — the pair order is the
// rule, and it is the pair and not the bm_idx, because one bm_idx acknowledged
// on one source slice says nothing about the same bm_idx on another (U1).
func TestCloneDrainBatchIsCutAndOrdered(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	pairs := make([][2]uint32, 0, common.MaxDelBmPerTxn+3)
	// Descending, so an implementation that took the slice head-first without
	// sorting hands over the HIGHEST chunks.
	for idx := common.MaxDelBmPerTxn + 2; idx >= 0; idx-- {
		pairs = append(pairs, [2]uint32{uint32(idx / 16), uint32(idx % 16)})
	}
	h.withClone("c0", 700, pairs)
	h.latchClone("c0")
	h.pass()
	h.wantOps("drain_clone_bm")
	batches := h.rops.cloneBatches()
	if len(batches) != 1 {
		t.Fatalf("%d batches, want 1", len(batches))
	}
	batch := batches[0]
	if len(batch) != common.MaxDelBmPerTxn {
		t.Fatalf("batch of %d chunks, want %d",
			len(batch), common.MaxDelBmPerTxn)
	}
	for idx := 1; idx < len(batch); idx++ {
		prev, cur := batch[idx-1], batch[idx]
		if prev.SliceIdx > cur.SliceIdx ||
			(prev.SliceIdx == cur.SliceIdx && prev.Idx >= cur.Idx) {
			t.Fatalf("batch is not ascending at %d: %v then %v",
				idx, prev, cur)
		}
	}
	// The lowest of the whole set, not merely an ascending run of some of it.
	if batch[0].SliceIdx != 0 || batch[0].Idx != 0 {
		t.Errorf("batch starts at (%d, %d), want the lowest chunk (0, 0)",
			batch[0].SliceIdx, batch[0].Idx)
	}
}

// TestCloneDrainRunsAlongsideTheReactions is CLD7's cadence half, which is
// where it deliberately deviates from the sp drain's SPD6: the SP is healthy,
// so its reactions must keep running in the same pass that drains one of its
// clones.
//
// Both directions are asserted. A pass that drained INSTEAD of reacting would
// leave the failover unrun; one that reacted instead of draining would leave
// the batch unrun.
func TestCloneDrainRunsAlongsideTheReactions(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.withClone("c0", 700, [][2]uint32{{0, 0}})
	h.latchClone("c0")
	// An unhealthy primary with a healthy candidate: AR5's trigger.
	h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
	h.pass()
	h.wantOps("drain_clone_bm", "failover")
}

// TestCloneDrainIgnoresEveryPassGate pins all FOUR gates CLD7 puts the clone
// drain in front of. Each one, left in front of the drain, would strand a
// latched clone for as long as it held — permanently, the latch being one-way
// and nothing else ever removing those keys.
//
//   - the RW21 cluster-conf cache MISS: the drain reads no cluster conf, and
//     a cluster absent from a watch-populated cache is an ordinary startup
//     state.
//   - an INVALID cluster conf: the reactions refuse, the drain does not.
//   - sp_level: a doomed clone drains at any level, SPD6's rule scoped down to
//     the clone. At SP_LEVEL_NO_THINPOOL the reactions are suppressed and the
//     batch still runs.
//   - the SP's own bdev_conf: the drain reads no geometry, so a stored conf
//     that cannot be read must not be able to strand a clone.
func TestCloneDrainIgnoresEveryPassGate(t *testing.T) {
	// The two CLUSTER-level gates. Both are reachable in production — the RW21
	// cache is watch-populated, so a cluster can be absent at startup or after
	// its conf key is deleted, and §13 keeps an invalid conf in the cache
	// rather than dropping it — and behind either one EVERY latched clone of
	// EVERY SP in that cluster would be stranded, the latch being one-way.
	t.Run("with the cluster missing from the cache", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.withClone("c0", 700, [][2]uint32{{0, 0}})
		h.latchClone("c0")
		h.deps.conf.mu.Lock()
		delete(h.deps.conf.entries, testCid)
		h.deps.conf.mu.Unlock()
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
		h.pass()
		// The drain ran; the pass returned before building anything. There is
		// no record to assert here: a cache miss returns silently (the missing
		// cluster is logged by the revision worker, not by the pass).
		h.wantOps("drain_clone_bm")
		h.wantApplied()
	})

	t.Run("with an invalid cluster conf", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.withClone("c0", 700, [][2]uint32{{0, 0}})
		h.latchClone("c0")
		cc := reactClusterConf()
		cc.DnBinConf = nil
		setCachedConf(h.deps, testCid, cc)
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
		h.pass()
		h.wantOps("drain_clone_bm")
		if got := len(h.logs.withMsg(msgInvalidStoredConf)); got != 1 {
			t.Errorf("%q records = %d, want 1 (the REACTIONS still refuse)",
				msgInvalidStoredConf, got)
		}
	})

	t.Run("at a suppressed sp_level", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.withClone("c0", 700, [][2]uint32{{0, 0}})
		h.latchClone("c0")
		h.state.Conf.SpLevel = pb.SpLevel_SP_LEVEL_NO_THINPOOL
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
		h.pass()
		// The drain ran; the reactions did not.
		h.wantOps("drain_clone_bm")
		if got := len(h.logs.withMsg(msgReactionSuppressed)); got != 1 {
			t.Errorf("%q records = %d, want 1", msgReactionSuppressed, got)
		}
	})

	t.Run("with an unreadable bdev_conf", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.withClone("c0", 700, [][2]uint32{{0, 0}})
		h.latchClone("c0")
		h.state.Conf.BdevConf.DmRaid0Conf.StripeSize = 0
		h.pass()
		h.wantOps("drain_clone_bm")
		if got := len(h.logs.withMsg(msgInvalidStoredConf)); got != 1 {
			t.Errorf("%q records = %d, want 1 (the REACTIONS still refuse)",
				msgInvalidStoredConf, got)
		}
	})
}

// TestCloneDrainOnePerPassPerClone is CLD7's bound: one step per deleting
// clone per pass, and every deleting clone gets one — a pass that stopped at
// the first would leave the rest of a multi-clone teardown to the tick.
func TestCloneDrainOnePerPassPerClone(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.withClone("c0", 700, [][2]uint32{{0, 0}})
	h.withClone("c1", 701, nil)
	h.withClone("c2", 702, [][2]uint32{{1, 0}})
	h.latchClone("c0")
	h.latchClone("c2")
	h.pass()
	// c1 is live, so it is not drained at all; c0 and c2 each get exactly one
	// step, in clone_name_list order.
	calls := h.wantOps("drain_clone_bm", "drain_clone_bm")
	if calls[0].cloneName != "c0" || calls[1].cloneName != "c2" {
		t.Errorf("drained %q then %q, want c0 then c2",
			calls[0].cloneName, calls[1].cloneName)
	}
}

// TestCloneDrainFailedStepIsLogged is CLD10's retry half: a failed step commits
// nothing, logs `clone drain failed` with the step and the cause, and is
// retried unchanged on the next pass. There is no terminal-failure state.
//
// BOTH steps, because they fail through different call sites and the final one
// is the step whose silence would be invisible: a swallowed error there logs
// `clone drained` for a clone that is still in etcd, which is the one record
// §14's case G reads as "this clone is gone".
func TestCloneDrainFailedStepIsLogged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pairs   [][2]uint32
		op      string
		reason  string
		step    string
		wantOps []string
	}{
		{
			name: "a batch", pairs: [][2]uint32{{0, 0}},
			op: "DrainCloneBm", reason: "clone not deleting",
			step: cloneDrainStepBm,
			// Retried unchanged: the second pass derives the same step from
			// the same surviving keys.
			wantOps: []string{"drain_clone_bm", "drain_clone_bm"},
		},
		{
			name: "the final STM", pairs: nil,
			op: "FinishCloneDelete", reason: "sp_id changed",
			step:    cloneDrainStepFinal,
			wantOps: []string{"finish_clone", "finish_clone"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.withClone("c0", 700, tc.pairs)
			h.latchClone("c0")
			h.rops.cloneDrainErr = &model.ErrPrecondition{
				Op: tc.op, Reason: tc.reason,
			}
			h.pass()
			recs := h.logs.withMsg(msgCloneDrainFailed)
			if len(recs) != 1 {
				t.Fatalf("%q records = %d, want 1",
					msgCloneDrainFailed, len(recs))
			}
			if recs[0]["step"] != tc.step {
				t.Errorf("step = %v, want %q", recs[0]["step"], tc.step)
			}
			if recs[0]["reason"] != tc.reason {
				t.Errorf("reason = %v, want %q", recs[0]["reason"], tc.reason)
			}
			if recs[0]["clone_name"] != "c0" {
				t.Errorf("clone_name = %v, want c0", recs[0]["clone_name"])
			}
			if recs[0]["error"] == nil {
				t.Errorf("the record must carry the error text as well")
			}
			// The step did not commit, so the drain must not claim it did.
			if got := len(h.logs.withMsg(msgCloneDrainDone)); got != 0 {
				t.Errorf("%q records = %d, want 0", msgCloneDrainDone, got)
			}
			h.pass()
			h.wantOps(tc.wantOps...)
		})
	}
}

// TestCloneDrainArmsAfterACommittedBatch is CLD12's termination argument at the
// coordinator: a COMMITTED batch schedules the next pass from its own commit,
// and nothing else arms.
//
// Deliberately not "only on progress", the sp drain's rule (worker/drain.go's
// `removed > 0`): this op reports the batch it was handed, not keys it found,
// so a progress guard here would be dead code. The overlap loser commits a
// full-looking batch that removed nothing and arms — and still terminates,
// because its next pass re-scans, finds the winner's deletes and moves on.
//
// A step that did NOT commit does not arm: it changed nothing, and a retry
// comes with the next tick anyway. The final STM never arms either — there is
// no next step for that clone, and its SpRev bump re-fans the SP without it.
func TestCloneDrainArmsAfterACommittedBatch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pairs   [][2]uint32
		failed  bool
		armed   bool
		wantOps []string
	}{
		{"a committed batch", [][2]uint32{{0, 0}}, false, true,
			[]string{"drain_clone_bm"}},
		{"a batch that failed", [][2]uint32{{0, 0}}, true, false,
			[]string{"drain_clone_bm"}},
		{"the final STM", nil, false, false, []string{"finish_clone"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.w.drainCh = make(chan struct{}, 1)
			h.withClone("c0", 700, tc.pairs)
			h.latchClone("c0")
			if tc.failed {
				h.rops.cloneDrainErr = &model.ErrPrecondition{
					Op: "DrainCloneBm", Reason: "clone not found",
				}
			}
			h.pass()
			h.wantOps(tc.wantOps...)
			if armed := len(h.w.drainCh) == 1; armed != tc.armed {
				t.Errorf("armed = %v, want %v", armed, tc.armed)
			}
		})
	}
}

// TestCloneExclusionFromThePlans is CLD5: a latched clone is fully absent from
// every cntlr's clone_list and from the primary's chunk-push plans, at every
// sp_level, while a sibling clone of the same SP keeps converging.
//
// Full absence is deliberately distinct from level suppression. A
// level-suppressed clone (SP_LEVEL_NO_CLONE) keeps its local chunk files for a
// later rebuild; a deleting one must lose them, and the cn agent's
// removed-clone retire path drops them precisely when the clone id is absent
// from the plan. That retire is the whole teardown — zero agent changes — so
// an exclusion that only removed the chunk plans, or only the clone_list,
// would leave the CN's dm-clone standing for ever.
func TestCloneExclusionFromThePlans(t *testing.T) {
	for _, level := range []pb.SpLevel{
		pb.SpLevel_SP_LEVEL_READWRITE,
		pb.SpLevel_SP_LEVEL_NO_CLONE,
		pb.SpLevel_SP_LEVEL_NO_THINPOOL,
	} {
		t.Run(level.String(), func(t *testing.T) {
			captureLogs(t)
			state := spFixture()
			state.Conf.SpLevel = level
			// A second clone that is NOT being deleted, so "excluded" cannot
			// be satisfied by "no clone reaches the plan".
			state.Conf.CloneNameList = append(
				state.Conf.GetCloneNameList(), "clone1")
			state.Clones["clone1"] = &pb.Clone{CloneId: spCloneId + 1}
			state.CloneBmIdx["clone1"] = []model.BmChunk{{SliceIdx: 0, Idx: 0}}
			state.Clones[spCloneNm].Deleting = true

			w := spTestWorker(nil)
			plan := w.buildPlan(context.Background(), state)
			primary := plan.cntlrs[spCntlrPrimary]
			if primary == nil {
				t.Fatalf("no plan for the primary cntlr")
			}
			for _, clone := range primary.req.GetCloneList() {
				if clone.GetCloneId() == spCloneId {
					t.Errorf("the deleting clone reached clone_list")
				}
			}
			if len(primary.req.GetCloneList()) != 1 {
				t.Errorf("clone_list = %v, want only the live clone",
					primary.req.GetCloneList())
			}
			for _, cp := range primary.clones {
				if cp.id == spCloneId {
					t.Errorf("the deleting clone reached the chunk plans")
				}
			}
			if len(primary.clones) != 1 || primary.clones[0].name != "clone1" {
				t.Errorf("chunk plans = %v, want only the live clone",
					primary.clones)
			}
			// Every OTHER cntlr too: a standby carries the full state (RW16),
			// so an exclusion applied only to the primary would keep telling
			// the standbys about a clone that is going away.
			for cntlrId, cp := range plan.cntlrs {
				for _, clone := range cp.req.GetCloneList() {
					if clone.GetCloneId() == spCloneId {
						t.Errorf("cntlr %d still carries the deleting clone",
							cntlrId)
					}
				}
			}
		})
	}
}
