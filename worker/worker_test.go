package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestLongLivedContextsCarryASeedTraceId pins the trace ids of RW10 that
// last a lifetime rather than one unit of work: the vote layer's incarnation,
// each role's shard manager, each per-object loop, each sp coordinator and
// each bitmap pusher root their context at an id of the seed prefix and a
// fresh id, so a record one of them writes outside a unit of work names the
// incarnation that wrote it, and the units of work under a loop, a
// coordinator or a pusher still mint ids of their own: a round's syncup and
// an immediate syncup, a fan-out, a drain wake-up and a report's handling,
// and a push.
func TestLongLivedContextsCarryASeedTraceId(t *testing.T) {
	t.Run("incarnation and shard managers", func(t *testing.T) {
		synctest.Test(t, testIncarnationTraceIds)
	})
	t.Run("per-object loop", func(t *testing.T) {
		synctest.Test(t, testLoopTraceId)
	})
	t.Run("coordinator and children", testCoordinatorTraceIds)
	t.Run("pusher", testPusherTraceId)
}

// wantSeedTraceId fails unless id is seed's prefix, a dash and a fresh id.
func wantSeedTraceId(t *testing.T, what string, id string, seed string) {
	t.Helper()
	prefix := seedPrefix(seed) + "-"
	if !strings.HasPrefix(id, prefix) || len(id) <= len(prefix) {
		t.Fatalf("%s trace id is %q, want %q and a fresh id", what, id, prefix)
	}
}

// recTraceId is a captured record's trace id, "" when it carries none.
func recTraceId(rec map[string]any) string {
	id, _ := rec[common.TraceIdLogKey].(string)
	return id
}

func testIncarnationTraceIds(t *testing.T) {
	roles := []string{common.WorkerRoleDn, common.WorkerRoleCn}
	h := newVoteHarness(t, roles...)
	old := h.vote.currentSeed()
	first := incarnationTraceId(t, h, roles, old)

	// Once the grace window closes, each role's shard manager logs its
	// ownership under one id of its own.
	h.advance(h.deps.cfg.GraceTime)
	waitFor(t, "every shard of both roles owned", func() bool {
		return len(h.logs.withMsg(msgShardOwned)) ==
			len(roles)*common.ShardBucketSize
	})
	byRole := make(map[string]string)
	for _, rec := range h.logs.withMsg(msgShardOwned) {
		role, _ := rec["role"].(string)
		id := recTraceId(rec)
		wantSeedTraceId(t, "a shard owned record's", id, old)
		if prev, ok := byRole[role]; ok && prev != id {
			t.Fatalf("%s shard owned records under %q and %q, want one id",
				role, prev, id)
		}
		byRole[role] = id
	}
	if len(byRole) != len(roles) || byRole[roles[0]] == byRole[roles[1]] ||
		byRole[roles[0]] == first || byRole[roles[1]] == first {
		t.Fatalf("shard manager ids %v beside the incarnation's %q, want "+
			"one id per role and none the incarnation's", byRole, first)
	}

	// A fence ends the incarnation under its own id and starts the next one
	// under the new seed and a new id.
	err := h.store.Delete(context.Background(), workerRegKey(roles[0], old))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	waitFor(t, "fence", func() bool {
		return len(h.logs.withMsg(msgWorkerFenced)) >= 1
	})
	fenced := fenceRecord(t, h)
	if got := recTraceId(fenced); got != first {
		t.Fatalf("the fence record carries trace id %q, want the old "+
			"incarnation's %q", got, first)
	}
	newSeed, _ := fenced["new_seed"].(string)
	if second := incarnationTraceId(t, h, roles, newSeed); second == first {
		t.Fatalf("the new incarnation kept the old one's trace id %q", first)
	}
}

// incarnationTraceId waits until seed's incarnation has registered every
// role and observed its own registration of every role, and returns the one
// trace id all those records carry.
func incarnationTraceId(
	t *testing.T,
	h *voteHarness,
	roles []string,
	seed string,
) string {
	t.Helper()
	var recs []map[string]any
	waitFor(t, "the registrations and own observations of "+seed, func() bool {
		recs = recs[:0]
		for _, rec := range h.logs.withMsg(msgWorkerRegistered) {
			if rec["seed"] == seed {
				recs = append(recs, rec)
			}
		}
		for _, rec := range h.logs.withMsg(msgMembershipObserved) {
			if rec["seed"] == seed && rec["own"] == true {
				recs = append(recs, rec)
			}
		}
		return len(recs) >= 2*len(roles)
	})
	id := recTraceId(recs[0])
	wantSeedTraceId(t, "the incarnation's", id, seed)
	for _, rec := range recs[1:] {
		if got := recTraceId(rec); got != id {
			t.Fatalf("a %v record of %s carries trace id %q, want the "+
				"incarnation's one id %q", rec["msg"], seed, got, id)
		}
	}
	return id
}

func testLoopTraceId(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			return &pb.CheckDnReply{Revision: 0}
		},
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, dnTestConf())
	w := h.startDn(testAddr, 3)
	id, _ := common.TraceIdFromCtx(w.ctx)
	wantSeedTraceId(t, "the loop's", id, seedOf(1))

	waitFor(t, "the loop's start record and a syncup result", func() bool {
		return len(h.logs.withMsg(msgRevisionWorkerStarted)) >= 1 &&
			len(h.logs.withMsg(msgSyncupResult)) >= 1
	})
	if got := recTraceId(h.logs.withMsg(msgRevisionWorkerStarted)[0]); got != id {
		t.Fatalf("the start record carries trace id %q, want the loop's %q",
			got, id)
	}
	syncup := recTraceId(h.logs.withMsg(msgSyncupResult)[0])
	wantSeedTraceId(t, "the syncup's", syncup, seedOf(1))
	if syncup == id {
		t.Fatalf("the syncup ran under the loop's own trace id %q, want "+
			"one of its own", id)
	}

	// A desired change's immediate syncup (RW6) runs under an id of its own,
	// neither the loop's nor a round's. The fake clock stands still, so no
	// round starts meanwhile.
	w.update(desiredState{revision: 4, handle: testAddr})
	var immediate map[string]any
	waitFor(t, "the immediate syncup's result", func() bool {
		for _, rec := range h.logs.withMsg(msgSyncupResult) {
			if rev, _ := rec["revision"].(float64); rev == 4 {
				immediate = rec
				return true
			}
		}
		return false
	})
	got := recTraceId(immediate)
	wantSeedTraceId(t, "the immediate syncup's", got, seedOf(1))
	rounds := make(map[string]bool)
	stub.mu.Lock()
	for _, req := range stub.checkReqs {
		rounds[req.GetTraceId()] = true
	}
	stub.mu.Unlock()
	if got == id || rounds[got] {
		t.Fatalf("the immediate syncup ran under trace id %q, the loop's "+
			"(%q) or a round's (%v), want one of its own", got, id, rounds)
	}
	w.stop()
	stopped := h.logs.withMsg(msgRevisionWorkerStopped)
	if len(stopped) != 1 || recTraceId(stopped[0]) != id {
		t.Fatalf("stop records %v, want one under the loop's trace id %q",
			stopped, id)
	}
}

func testCoordinatorTraceIds(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	w := h.start()
	id, _ := common.TraceIdFromCtx(w.ctx)
	wantSeedTraceId(t, "the coordinator's", id, seedOf(3))

	// The coordinator and its five side and three cntlr children.
	waitFor(t, "every start record", func() bool {
		return len(h.logs.withMsg(msgRevisionWorkerStarted)) >= 9
	})
	children := make(map[string]bool)
	for _, rec := range h.logs.withMsg(msgRevisionWorkerStarted) {
		got := recTraceId(rec)
		_, side := rec["side_pointer"]
		_, cntlr := rec["cntlr_pointer"]
		if !side && !cntlr {
			if got != id {
				t.Fatalf("the coordinator's start record carries trace id "+
					"%q, want its own %q", got, id)
			}
			continue
		}
		wantSeedTraceId(t, "a child's", got, seedOf(3))
		if got == id || children[got] {
			t.Fatalf("a child's start record carries trace id %q, want an "+
				"id of the child's own", got)
		}
		children[got] = true
	}
	if len(children) != 8 {
		t.Fatalf("%d children's trace ids, want 5 sides + 3 cntlrs",
			len(children))
	}

	// The units of work of the coordinator's loop each mint an id of their
	// own: a fan-out and a drain wake-up, each logging the load the fake
	// refuses, and the handling of a report, logging its flip. The fake
	// clock stands still, so no tick runs a pass meanwhile.
	h.ops.setErr(errors.New("etcd down"))
	w.update(desiredState{revision: testSpRev + 1, handle: testSpName})
	w.armDrain()
	w.reportCh <- spReport{provisioned: &model.SideRef{
		SliceId: spSliceA, LegId: spLegMigr, SideId: spSideDst,
	}}
	waitFor(t, "two load failures and a flip", func() bool {
		return len(h.logs.withMsg(msgSpLoadFailed)) >= 2 &&
			len(h.logs.withMsg(msgFlipApplied)) >= 1
	})
	units := make(map[string]bool)
	for _, msg := range []string{msgSpLoadFailed, msgFlipApplied} {
		for _, rec := range h.logs.withMsg(msg) {
			got := recTraceId(rec)
			wantSeedTraceId(t, "a "+msg+" record's", got, seedOf(3))
			if got == id || children[got] {
				t.Fatalf("a %s record carries trace id %q, the coordinator's "+
					"or a child's, want one of its unit of work's own", msg, got)
			}
			if msg == msgSpLoadFailed {
				units[got] = true
			}
		}
	}
	if len(units) < 2 {
		t.Fatalf("the fan-out and the drain wake-up logged under %d trace "+
			"ids, want one each", len(units))
	}
}

func testPusherTraceId(t *testing.T) {
	logs := captureLogs(t)
	p := newTestPusher(t, bmTestDeps(t), newPushRecorder())
	id, _ := common.TraceIdFromCtx(p.ctx)
	wantSeedTraceId(t, "the pusher's", id, seedOf(4))

	p.submit(&bmPlan{
		resId: spCloneId, name: spCloneNm,
		parts: []model.BmChunk{{Idx: 0, ModRev: 10}},
	})
	waitFor(t, "the chunk pushed", func() bool {
		return len(logs.withMsg(msgBitmapPushed)) == 1
	})
	if got := recTraceId(logs.withMsg(msgBitmapPushed)[0]); got == id {
		t.Fatalf("the push ran under the pusher's own trace id %q, want "+
			"one of its own", id)
	}
}
