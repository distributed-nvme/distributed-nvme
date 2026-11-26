package cnagent

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// A primary's fence watch (CN34; fence.go). The tests drive the node's
// watch loop one tick at a time (fenceTickAll); the test servers' loop
// waits an hour between ticks.

// fenceFixtureNs is the fixture's host-facing namespace.
func fenceFixtureNs(srv *CnAgentServer) fenceNs {
	st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	_, nss, _ := fenceInputs(newCntlrPlan(srv.nf, st.loadReq()))
	return nss[0]
}

// anaOfNs reads the fake's ana_grpid of one namespace.
func anaOfNs(node *fakeNode, ns fenceNs) string {
	node.mu.Lock()
	defer node.mu.Unlock()
	return strings.TrimSpace(node.files[anaPath(ns.nqn, ns.nsIdx)])
}

// setLegPaths sets the one path of each of the fixture's legs to state.
func setLegPaths(srv *CnAgentServer, node *fakeNode, meta, data string) {
	node.setAnaState(legNqn(srv, testMetaLeg), testIp, testSvcId, meta)
	node.setAnaState(legNqn(srv, testDataLeg), testIp, testSvcId, data)
}

var (
	anaOptimized    = strconv.Itoa(common.AnaGrpIdOptimized)
	anaInaccessible = strconv.Itoa(common.AnaGrpIdInaccessible)
)

// TestAFencedPrimaryLeavesTheOptimizedGroup: a primary that served and whose
// leg paths read non-optimized, as a fence leaves them, moves its namespaces
// to the inaccessible group with no converge; a read and a converge after it
// keep them there and name nothing; and a leg path optimized again drops the
// flag (CN34).
func TestAFencedPrimaryLeavesTheOptimizedGroup(t *testing.T) {
	srv, node := newTestServer(t)
	opts := reqOpts{revision: 2, primary: true}
	syncupBoth(t, srv, opts)
	st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	ns := fenceFixtureNs(srv)
	ctx := context.Background()
	if got := anaOfNs(node, ns); got != anaOptimized {
		t.Fatalf("fixture is wrong: the namespace reads group %s", got)
	}
	setLegPaths(srv, node, "non-optimized", "non-optimized")
	srv.fenceTickAll(ctx)
	if got := anaOfNs(node, ns); got != anaInaccessible || !st.fenced.Load() {
		t.Fatalf("the fenced primary's namespace reads group %s, flag %v",
			got, st.fenced.Load())
	}
	held := cntlrReq(opts)
	round, resent := resendRound(t, srv, held, "fenced")
	assertCodeZero(t, "the read of a fenced primary", round)
	if resent != nil {
		t.Errorf("the read of a fenced primary brought a re-send")
	}
	if _, err := srv.SyncupCntlr(traced("again"), held); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if got := anaOfNs(node, ns); got != anaInaccessible {
		t.Errorf("a converge of the fenced primary moved the namespace to "+
			"group %s", got)
	}
	setLegPaths(srv, node, "optimized", "non-optimized")
	srv.fenceTickAll(ctx)
	if st.fenced.Load() {
		t.Errorf("the fence flag stayed set over an optimized path")
	}
}

// TestOnePathInTheStandbyGroupIsTheFence: with no leg path optimized, one
// live path in the standby group is the fence, whatever the other legs'
// paths are doing: after a network cut a host's path can come back before
// all of the node's own (CN34).
func TestOnePathInTheStandbyGroupIsTheFence(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	ns := fenceFixtureNs(srv)
	setLegPaths(srv, node, "non-optimized", "non-optimized")
	node.setCtrlState(legNqn(srv, testMetaLeg), testIp, testSvcId,
		"connecting")
	srv.fenceTickAll(context.Background())
	if got := anaOfNs(node, ns); got != anaInaccessible || !st.fenced.Load() {
		t.Fatalf("one standby path did not fence: group %s, flag %v", got,
			st.fenced.Load())
	}
}

// TestANewPrimaryIsNoFence: a standby promoted while its disk nodes have not
// opened its paths shows no optimized path and its standby paths, but it has
// not served: no namespace optimized, no pool. Neither its converge nor its
// watch judges it fenced, and once its paths open it serves (CN34).
func TestANewPrimaryIsNoFence(t *testing.T) {
	srv, node := newTestServer(t)
	setLegPaths(srv, node, "non-optimized", "non-optimized")
	syncupBoth(t, srv, reqOpts{revision: 2})
	syncupBoth(t, srv, reqOpts{revision: 3, primary: true, leftoverOk: true})
	st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	srv.fenceTickAll(context.Background())
	if st.fenced.Load() {
		t.Fatalf("a new primary whose paths are not open read as fenced")
	}
	setLegPaths(srv, node, "optimized", "optimized")
	syncupBoth(t, srv, reqOpts{revision: 4, primary: true})
	if got := anaOfNs(node, fenceFixtureNs(srv)); got != anaOptimized {
		t.Errorf("the new primary's namespace reads group %s once its paths "+
			"opened, want %s", got, anaOptimized)
	}
}

// TestARestartKeepsTheFence: an agent that restarts after its watch moved
// the namespaces forgets the flag; its startup converge reads the fence
// itself, as the cntlr's pools show it has served, and keeps the
// namespaces inaccessible (CN34).
func TestARestartKeepsTheFence(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	ns := fenceFixtureNs(srv)
	setLegPaths(srv, node, "non-optimized", "non-optimized")
	srv.fenceTickAll(context.Background())
	if got := anaOfNs(node, ns); got != anaInaccessible {
		t.Fatalf("fixture is wrong: the namespace reads group %s", got)
	}
	restarted := newCnServer(node)
	reconcileForTest(t, restarted)
	st := restarted.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	if got := anaOfNs(node, ns); got != anaInaccessible || !st.fenced.Load() {
		t.Errorf("the restart moved the fenced namespace to group %s, "+
			"flag %v", got, st.fenced.Load())
	}
}

// TestAHeldLockLeavesTheMoveToTheNextTick: the watch takes the cntlr's
// object lock only when it is free, so a tick behind a converge or a read
// moves nothing, and the next tick does (CN34).
func TestAHeldLockLeavesTheMoveToTheNextTick(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	ns := fenceFixtureNs(srv)
	key := cntlrKey(testCluster, testCn, testSp, testCntlr)
	setLegPaths(srv, node, "non-optimized", "non-optimized")
	lock := srv.locks.Obj(key)
	lock.Lock()
	srv.fenceTickAll(context.Background())
	lock.Unlock()
	if got := anaOfNs(node, ns); got != anaOptimized {
		t.Fatalf("a tick behind a held lock moved the namespace to %s", got)
	}
	srv.fenceTickAll(context.Background())
	if got := anaOfNs(node, ns); got != anaInaccessible {
		t.Errorf("the next tick left the namespace in group %s", got)
	}
}

// TestAStandbyHasNoFenceWatch: the watch exists only while the cntlr is a
// primary that builds legs, and the demotion drops the flag (CN34).
func TestAStandbyHasNoFenceWatch(t *testing.T) {
	srv, _ := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	key := cntlrKey(testCluster, testCn, testSp, testCntlr)
	st := srv.getCntlr(key)
	srv.mu.Lock()
	_, watched := srv.fenceWatches[key]
	srv.mu.Unlock()
	if !watched {
		t.Fatalf("a primary has no fence watch")
	}
	st.fenced.Store(true)
	syncupBoth(t, srv, reqOpts{revision: 3})
	srv.mu.Lock()
	_, watched = srv.fenceWatches[key]
	srv.mu.Unlock()
	if watched || st.fenced.Load() {
		t.Errorf("the demoted cntlr keeps its watch %v or its flag %v",
			watched, st.fenced.Load())
	}
}

// TestAWatchNotArmedRunsNoCommand: the watch of a primary that has not
// served, as a promotion whose disk nodes have not opened its paths, reads
// its namespaces alone at each tick and runs no command, so the agent runs
// none between two syncups (CN34).
func TestAWatchNotArmedRunsNoCommand(t *testing.T) {
	srv, node := newTestServer(t)
	setLegPaths(srv, node, "non-optimized", "non-optimized")
	syncupBoth(t, srv, reqOpts{revision: 2})
	syncupBoth(t, srv, reqOpts{revision: 3, primary: true, leftoverOk: true})
	w := fixtureWatch(srv)
	if w == nil || w.armed.Load() {
		t.Fatalf("fixture is wrong: want a watch not armed, have one %v",
			w != nil)
	}
	node.Reset()
	for range 3 {
		srv.fenceTickAll(context.Background())
	}
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "cmd ") {
			t.Errorf("a tick of a watch not armed ran %q", call)
		}
	}
	if w.armed.Load() {
		t.Errorf("the ticks armed a primary that has not served")
	}
}

// fixtureWatch is the fixture cntlr's fence watch, nil when it has none.
func fixtureWatch(srv *CnAgentServer) *fenceWatch {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.fenceWatches[cntlrKey(testCluster, testCn, testSp, testCntlr)]
}

// TestATickArmsOnAServingNamespace: a watch not armed arms at its tick once
// a namespace of the cntlr reads optimized, with no converge (CN34).
func TestATickArmsOnAServingNamespace(t *testing.T) {
	srv, node := newTestServer(t)
	setLegPaths(srv, node, "non-optimized", "non-optimized")
	syncupBoth(t, srv, reqOpts{revision: 2})
	syncupBoth(t, srv, reqOpts{revision: 3, primary: true, leftoverOk: true})
	w := fixtureWatch(srv)
	if w == nil || w.armed.Load() {
		t.Fatalf("fixture is wrong: want a watch not armed")
	}
	ns := fenceFixtureNs(srv)
	node.mu.Lock()
	node.files[anaPath(ns.nqn, ns.nsIdx)] = anaOptimized + "\n"
	node.mu.Unlock()
	srv.fenceTickAll(context.Background())
	if !w.armed.Load() {
		t.Errorf("a tick over an optimized namespace did not arm the watch")
	}
}

// TestARestartedPrimaryIsArmedByItsConverge: after a restart, the startup
// converge of a primary whose pool stands arms its watch, though its
// namespaces read inaccessible and no tick has run (CN34; armFenceWatch).
func TestARestartedPrimaryIsArmedByItsConverge(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	setLegPaths(srv, node, "non-optimized", "non-optimized")
	srv.fenceTickAll(context.Background())
	if got := anaOfNs(node, fenceFixtureNs(srv)); got != anaInaccessible {
		t.Fatalf("fixture is wrong: the namespace reads group %s", got)
	}
	restarted := newCnServer(node)
	reconcileForTest(t, restarted)
	if got := anaOfNs(node, fenceFixtureNs(srv)); got != anaInaccessible {
		t.Fatalf("the restart moved the fenced namespace to group %s", got)
	}
	w := fixtureWatch(restarted)
	if w == nil || !w.armed.Load() {
		t.Errorf("the restarted primary's watch is not armed: %v", w != nil)
	}
}
