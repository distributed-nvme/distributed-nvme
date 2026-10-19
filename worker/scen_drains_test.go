package worker

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/integtest/plant"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The whole-worker scenario cases of the two drains (dnv-worker.md, The sp
// drain, SPD1 to SPD14, and The clone drain, CLD1 to CLD12). A pool that is
// drained is planted without its thin device and subsystem, as
// DeleteStoragePool latches only a pool that holds none (SPD4).

// latchStd latches the standard pool as DeleteStoragePool does (SPD3).
func (c *stdCluster) latchStd(t *testing.T) {
	t.Helper()
	latch, err := plant.SetDeleting(context.Background(), c.h.cli, c.cl, c.sp.Ref)
	if err != nil || !latch.Latched {
		t.Fatalf("SetDeleting: latched %t, %v", latch.Latched, err)
	}
}

// waitDrained waits for the pool's keys to be gone and checks what the
// drain gave back: every node's free extent count as planted and no pointer
// of the pool left on any node record (SPD13's budget agreement).
func (c *stdCluster) waitDrained(t *testing.T, timeout time.Duration) {
	t.Helper()
	ctx := context.Background()
	c.h.waitUntil(t, timeout, "the pool's keys to be gone", func() (bool, string) {
		conf := model.SpConfKey(c.cl.Id, c.sp.Ref.Name)
		found, err := c.h.cli.Get(ctx, conf, &pb.SpConf{})
		if err != nil || found {
			return false, fmt.Sprintf("%s still there (%v)", conf, err)
		}
		rev := model.SpRevKey(stdSpShard, c.cl.Id, stdSpId)
		found, err = c.h.cli.Get(ctx, rev, &pb.SpRev{})
		if err != nil || found {
			return false, fmt.Sprintf("%s still there (%v)", rev, err)
		}
		return true, ""
	})
	for id := uint64(1); id <= uint64(c.spec.dnCnt); id++ {
		conf := c.dnConf(t, id)
		if conf.GetFreeExtCnt() != c.spec.nodeFree || len(conf.GetSidePtrList()) != 0 {
			t.Errorf("dn %d after the drain: %d free extents of %d, pointers %v",
				id, conf.GetFreeExtCnt(), c.spec.nodeFree, conf.GetSidePtrList())
		}
	}
	for id := uint64(1); id <= uint64(c.spec.cnCnt); id++ {
		conf := c.cnConf(t, id)
		if conf.GetFreeExtCnt() != c.spec.nodeFree || len(conf.GetCntlrPtrList()) != 0 {
			t.Errorf("cn %d after the drain: %d free extents of %d, pointers %v",
				id, conf.GetFreeExtCnt(), c.spec.nodeFree, conf.GetCntlrPtrList())
		}
	}
}

// drainEnds checks that the drain logged its end exactly once and never a
// failed step, over every worker of the harness.
func (c *stdCluster) drainEnds(t *testing.T) logRecord {
	t.Helper()
	done := c.h.records(nil, msgSpDrainDone)
	if len(done) != 1 {
		t.Fatalf("%d %q records, want exactly 1", len(done), msgSpDrainDone)
	}
	if failed := c.h.records(nil, msgSpDrainFailed); len(failed) != 0 {
		t.Fatalf("%d %q records, want none: %v", len(failed), msgSpDrainFailed,
			failed[0].attrs)
	}
	return done[0]
}

// TestScenPoolDrainDoesNotWaitForAgents pins SPD13: no drain step waits on,
// calls or verifies an agent. A pool is latched while one of its CNs' agents
// is down and one of its DNs' agents answers every round with a leftover
// that never clears; the drain runs to its end all the same, gives every
// budget back and logs `sp drained` once, while that CN is still down and
// that DN still answers leftover. The CN, back with its files, then gets the
// syncup with no cntlr of the pool.
func TestScenPoolDrainDoesNotWaitForAgents(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantScen(t, scenShape{noTdSs: true})
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	cn2, dn2 := c.cnAgent(2), c.dnAgent(2)
	cn2.Stop()
	setLever(t, dn2, agentfake.DnKey, func(o *agentfake.Object) {
		o.LeftoverRounds = 1 << 30
	})
	defer setLever(t, dn2, agentfake.DnKey, func(o *agentfake.Object) { o.LeftoverRounds = 0 })
	h.waitUntil(t, 20*time.Second, "cn 2 down and dn 2 answering leftover",
		func() (bool, string) {
			if c.cnConf(t, 2).GetErrEpoch() == 0 {
				return false, "cn 2 has no health epoch yet"
			}
			if h.countRecs(w, msgSyncupLeftover, func(rec logRecord) bool {
				return isNodeRec(rec, "dn_id", 2)
			}) == 0 {
				return false, "no leftover of dn 2 yet"
			}
			return true, ""
		})

	c.latchStd(t)
	c.waitDrained(t, 20*time.Second)
	h.waitRecord(t, 10*time.Second, w, msgSpDrainDone, nil)
	c.drainEnds(t)
	if n := h.countRecs(w, msgSpDrainStep, func(rec logRecord) bool {
		return rec.str("phase") == drainPhaseCntlrs
	}); n != 1 {
		t.Errorf("%d cntlr steps, want 1 (SPD9)", n)
	}
	leftovers := h.countRecs(w, msgSyncupLeftover, func(rec logRecord) bool {
		return isNodeRec(rec, "dn_id", 2)
	})
	// dn 2 still answers with its leftover after the drain's end: the drain
	// did not wait for it to clear.
	h.waitCount(t, 20*time.Second, w, msgSyncupLeftover, leftovers+1,
		func(rec logRecord) bool { return isNodeRec(rec, "dn_id", 2) })

	if err := cn2.Start(agentfake.KeepStore); err != nil {
		t.Fatalf("restart cn 2: %v", err)
	}
	h.waitUntil(t, 20*time.Second, "cn 2's syncup with no cntlr of the pool",
		func() (bool, string) {
			msg, _, _ := lastSyncup(cn2, agentfake.CnKey)
			req, _ := msg.(*pb.SyncupCnRequest)
			if req == nil || req.GetRevision() != c.cnRev(t, 2) {
				return false, "no SyncupCn at cn 2's revision yet"
			}
			return len(req.GetCntlrPointerList()) == 0,
				fmt.Sprintf("it lists %v", req.GetCntlrPointerList())
		})
}

// TestScenPoolDrainFinalStepLogsItsEndOnce pins the end of SPD12's last step
// on a whole worker: its commit deletes the pool's keys, the revision key
// among them, that delete stops the pool's coordinator (SW3), and the drain
// logs `sp drained` exactly once and no `sp drain failed`. The stop can cancel
// the step's own call only inside one etcd round trip, which this case cannot
// open on cue, so TestDrainFinalStepSurvivesTheStopItCauses pins the context
// the step runs under.
func TestScenPoolDrainFinalStepLogsItsEndOnce(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantScen(t, scenShape{noTdSs: true})
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	c.latchStd(t)
	c.waitDrained(t, 20*time.Second)
	// The coordinator's stop, which the last step's own delete brings
	// (SW3), has run: its record, the one of the pool's that names no side
	// and no cntlr, is w1's by the coordinator's own trace id (RW10).
	h.waitRecord(t, 10*time.Second, w, msgRevisionWorkerStopped,
		func(rec logRecord) bool {
			return rec.str("role") == common.WorkerRoleSp &&
				rec.num("cluster_id") == c.cl.Id && rec.num("id") == stdSpId &&
				rec.str("side_pointer") == "" && rec.str("cntlr_pointer") == ""
		})
	done := c.drainEnds(t)
	if done.num("sp_id") != stdSpId || done.worker != w {
		t.Errorf("the drained record names sp %d of worker %s",
			done.num("sp_id"), done.workerName())
	}
}

// TestScenCloneDrainPushToADeletedChunkIsHarmless pins CLD5 and BM6: a push
// of a clone's chunk from the plan the primary drove before the clone's
// latch can find its chunk key already deleted by the drain's batch; that
// push ends with `bitmap push failed`, the clone drains to its end all the
// same, no push of it follows, and the pool converges with the clone gone.
// The case holds the first push at the primary's fake, and the primary's
// syncups, so the plan from before the latch is the one that runs when the
// batch has deleted the chunks.
func TestScenCloneDrainPushToADeletedChunkIsHarmless(t *testing.T) {
	t.Parallel()
	tmo := harnessTimeouts()
	// Far past the case's own waits, so that only hold.open() ends the held
	// push: a push that failed by its deadline would end its plan (BM6)
	// before any push of it could find its chunk gone. The case's waits and
	// the deferred hold.open() still bound it.
	tmo.push = 60 * time.Second
	h := newWorkerHarness(t, harnessOpts{tmo: &tmo})
	hold := newCallHold(agentfake.MethodPushCloneBitmap)
	c := h.plantScen(t, scenShape{
		cnCfg: map[uint64]agentfake.Config{1: hold.config()},
	})
	defer hold.open()
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	ctx := context.Background()

	clone, err := plant.PutClone(ctx, h.cli, c.cl, c.sp.Ref, plant.CloneSpec{
		Name: "c0", DstTd: "td0", SrcSliceCnt: 1,
		SrcNqn: "nqn.2026-10.io.dnv-wh:src", SrcNsIdx: 1,
		SrcStripeSize: 1 << 20, SrcBlockSize: 1 << 16,
	})
	if err != nil {
		t.Fatalf("PutClone: %v", err)
	}
	for idx := uint32(0); idx < 3; idx++ {
		if _, err := plant.PutCloneBitmap(ctx, h.cli, c.cl, c.sp.Ref, plant.CloneBitmapSpec{
			Clone: "c0", SrcSliceIdx: 0, BmIdx: idx, Data: []byte{byte(idx + 1)},
		}); err != nil {
			t.Fatalf("PutCloneBitmap: %v", err)
		}
	}
	h.waitUntil(t, 20*time.Second, "the first push of the clone at cn 1",
		func() (bool, string) {
			return len(hold.arrivals()) == 1, fmt.Sprintf("%d pushes", len(hold.arrivals()))
		})
	primary, primaryKey := c.cntlrAgent(t, 1)
	setLever(t, primary, primaryKey, func(o *agentfake.Object) { o.HangSyncup = true })
	defer setLever(t, primary, primaryKey, func(o *agentfake.Object) { o.HangSyncup = false })

	if _, err := plant.SetCloneDeleting(ctx, h.cli, c.cl, c.sp.Ref, "c0"); err != nil {
		t.Fatalf("SetCloneDeleting: %v", err)
	}
	h.waitRecord(t, 20*time.Second, w, msgCloneDrainDone, nil)
	keys, _, err := h.cli.RangeKeys(ctx, model.CloneBitmapPrefix(c.cl.Id, stdSpId, "c0"))
	if err != nil || len(keys) != 0 {
		t.Fatalf("the clone's chunk keys after its drain: %v, %v", keys, err)
	}
	if len(hold.arrivals()) != 1 {
		t.Fatalf("%d pushes reached cn 1 before the hold ended, want 1",
			len(hold.arrivals()))
	}

	hold.open()
	ofClone := func(rec logRecord) bool {
		return rec.str("kind") == bmKindClone && rec.num("clone_id") == clone.Id
	}
	h.waitRecord(t, 20*time.Second, w, msgBitmapPushFailed, ofClone)
	setLever(t, primary, primaryKey, func(o *agentfake.Object) { o.HangSyncup = false })
	c.waitConverged(t)
	// Each plan the primary's replies before the latch submitted ends at its
	// first chunk that is gone (BM6); none of them reaches the agent again.
	for _, rec := range h.records(w, msgBitmapPushFailed) {
		if ofClone(rec) && rec.str("error") != "chunk not found" {
			t.Errorf("a push of the clone failed with %q, want only the "+
				"chunk's absence", rec.str("error"))
		}
	}
	if n := len(hold.arrivals()); n != 1 {
		t.Errorf("%d pushes of the clone reached cn 1, want only the one held", n)
	}
	if n := h.countRecs(w, msgCloneDrainDone, nil); n != 1 {
		t.Errorf("%d %q records, want 1", n, msgCloneDrainDone)
	}
	if n := h.countRecs(w, msgCloneDrainFailed, nil); n != 0 {
		t.Errorf("%d %q records, want none", n, msgCloneDrainFailed)
	}
	if names := c.load(t).Conf.GetCloneNameList(); len(names) != 0 {
		t.Errorf("the pool still lists clones %v", names)
	}
	msg, _, _ := lastSyncup(primary, primaryKey)
	if clones := msg.(*pb.SyncupCntlrRequest).GetCloneList(); len(clones) != 0 {
		t.Errorf("the primary's last syncup still carries clones %v", clones)
	}
}

// TestScenPoolDrainAcrossATakeover pins SPD8's derivation across a takeover:
// the worker that owns the pool commits the drain's cntlr step and is then
// killed, inside that step's own record, before any later step; the other
// worker takes the pool's shard and, deriving the next step from the pool's
// record alone, drains the slices and the last keys, so the drain ends in
// one `sp drained`, by the new owner, with no step run twice.
func TestScenPoolDrainAcrossATakeover(t *testing.T) {
	t.Parallel()
	tmo := harnessTimeouts()
	tmo.etcdOp = 500 * time.Millisecond
	h := newWorkerHarness(t, harnessOpts{tmo: &tmo})
	c := h.plantScen(t, scenShape{noTdSs: true})
	w1 := h.startWorker(t, "w1")
	w2 := h.startWorker(t, "w2")
	h.waitUntil(t, 20*time.Second, "w1 and w2 to share every shard",
		func() (bool, string) {
			for _, role := range w1.roles {
				n1, n2 := w1.ownedCount(role), w2.ownedCount(role)
				if n1 == 0 || n2 == 0 || n1+n2 != common.ShardBucketSize {
					return false, fmt.Sprintf("%s: w1 %d, w2 %d", role, n1, n2)
				}
			}
			return true, ""
		})
	c.waitConverged(t)
	owner := c.spOwner()
	if owner == nil {
		t.Fatalf("no worker owns the pool's shard")
	}
	heir := w1
	if owner == w1 {
		heir = w2
	}

	var killed atomic.Bool
	h.onRecord(func(_ context.Context, rec logRecord) {
		if rec.worker == owner && rec.msg == msgSpDrainStep &&
			rec.str("phase") == drainPhaseCntlrs && killed.CompareAndSwap(false, true) {
			owner.crash(t)
		}
	})
	c.latchStd(t)
	h.waitUntil(t, 20*time.Second, "the owner's crash at its cntlr step",
		func() (bool, string) { return killed.Load(), "not yet" })
	h.waitUntil(t, 20*time.Second, "the heir to own the pool's shard",
		func() (bool, string) { return c.spOwner() == heir, "not yet" })
	c.waitDrained(t, 20*time.Second)
	h.waitRecord(t, 10*time.Second, heir, msgSpDrainDone, nil)
	done := c.drainEnds(t)
	if done.worker != heir {
		t.Errorf("the drain ended under %s, want the heir %s",
			done.workerName(), heir.name)
	}
	steps := func(w *harnessWorker, phase string) int {
		return h.countRecs(w, msgSpDrainStep, func(rec logRecord) bool {
			return rec.str("phase") == phase
		})
	}
	if steps(owner, drainPhaseCntlrs) != 1 || steps(owner, drainPhaseSlice) != 0 {
		t.Errorf("the owner logged %d cntlr and %d slice steps, want 1 and 0",
			steps(owner, drainPhaseCntlrs), steps(owner, drainPhaseSlice))
	}
	if steps(heir, drainPhaseCntlrs) != 0 || steps(heir, drainPhaseSlice) == 0 {
		t.Errorf("the heir logged %d cntlr and %d slice steps, want none and "+
			"some", steps(heir, drainPhaseCntlrs), steps(heir, drainPhaseSlice))
	}
}
