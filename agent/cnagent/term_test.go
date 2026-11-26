package cnagent

import (
	"context"
	"strings"
	"testing"

	pb "github.com/distributed-nvme/distributed-nvme/pb"
)

// The term of a primary's stack (CN35; term.go).

// termOf is the term the fixture cntlr's stack was built under.
func termOf(srv *CnAgentServer) uint64 {
	return srv.getCntlr(cntlrKey(testCluster, testCn, testSp,
		testCntlr)).stackTerm
}

// TestAPrimaryUnderANewTermRemovesTheOldStack: a demotion whose sweep stops
// part way leaves the pool, and the standby's report says so; the role
// comes back under another term while the pool still cannot be removed, and
// the primary serves nothing; once the removal goes through, the primary
// builds anew under the new term (CN35).
func TestAPrimaryUnderANewTermRemovesTheOldStack(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, term: 10})
	if got := termOf(srv); got != 10 {
		t.Fatalf("the first build's term is %d, want 10", got)
	}
	pool := poolName(srv)
	node.mu.Lock()
	node.failCmdAlways["dmsetup remove "+pool] = "device-mapper: remove ioctl failed: Device or resource busy"
	node.mu.Unlock()
	reply := syncupBoth(t, srv, reqOpts{revision: 3, leftoverOk: true})
	if !reply.GetCntlrInfo().GetStackLeft() {
		t.Fatalf("the standby whose sweep left the pool does not say so")
	}
	if got := termOf(srv); got != 10 {
		t.Fatalf("a standby with a stack left forgot its term: %d", got)
	}

	node.Reset()
	reply = syncupBoth(t, srv, reqOpts{revision: 4, primary: true, term: 20,
		leftoverOk: true})
	ns := fenceFixtureNs(srv)
	if got := anaOfNs(node, ns); got != anaInaccessible {
		t.Fatalf("a primary over an earlier term's pool serves: group %s", got)
	}
	if reply.GetAgentReply().GetCode() == 0 {
		t.Fatalf("a primary that could not remove the old stack replied 0")
	}
	if got := termOf(srv); got != 10 {
		t.Fatalf("the term moved to %d before the old stack went", got)
	}

	node.mu.Lock()
	delete(node.failCmdAlways, "dmsetup remove "+pool)
	node.mu.Unlock()
	node.Reset()
	syncupBoth(t, srv, reqOpts{revision: 5, primary: true, term: 20})
	if !node.hasCall("cmd dmsetup remove " + pool) {
		t.Errorf("the old pool was not removed before the build")
	}
	if !node.hasCall("cmd dmsetup create " + pool) {
		t.Errorf("the pool was not made anew")
	}
	if got := termOf(srv); got != 20 {
		t.Errorf("the new build's term is %d, want 20", got)
	}
	if got := anaOfNs(node, ns); got != anaOptimized {
		t.Errorf("the new term's primary does not serve: group %s", got)
	}
}

// TestATermThatMatchesKeepsTheStack: a primary converge under the term its
// stack was built under, a restart's included, removes nothing; and a
// primary with no term, a pool's first primary or one an older build wrote,
// never tears down (CN35).
func TestATermThatMatchesKeepsTheStack(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, term: 10})
	pool := poolName(srv)
	node.Reset()
	restarted := newCnServer(node)
	reconcileForTest(t, restarted)
	if got := restarted.getCntlr(cntlrKey(testCluster, testCn, testSp,
		testCntlr)).stackTerm; got != 10 {
		t.Fatalf("the restart loaded the term %d, want 10", got)
	}
	if node.hasCall("cmd dmsetup remove " + pool) {
		t.Fatalf("the restart under the same term removed the pool")
	}

	srv2, node2 := newTestServer(t)
	syncupBoth(t, srv2, reqOpts{revision: 2, primary: true})
	node2.Reset()
	syncupBoth(t, srv2, reqOpts{revision: 3, primary: true})
	if node2.hasCall("cmd dmsetup remove " + poolName(srv2)) {
		t.Errorf("a primary with no term tore its stack down")
	}
}

// TestACleanStandbyForgetsItsTerm: a standby whose sweep left nothing
// forgets the term, and says no stack is left (CN35).
func TestACleanStandbyForgetsItsTerm(t *testing.T) {
	srv, _ := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, term: 10})
	reply := syncupBoth(t, srv, reqOpts{revision: 3})
	if reply.GetCntlrInfo().GetStackLeft() {
		t.Errorf("a clean standby says a stack is left")
	}
	if got := termOf(srv); got != 0 {
		t.Errorf("a clean standby keeps the term %d", got)
	}
}

// TestAFailedTeardownReadsUnhealthyAndIsNamed: a primary whose standby
// sweep cannot remove an earlier term's pool reports an error row for each
// pool, so it reads unhealthy and the failover can move the role; its read
// names it while the terms differ; and a standby's read reports the stack it
// left (CN35).
func TestAFailedTeardownReadsUnhealthyAndIsNamed(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, term: 10})
	pool := poolName(srv)
	node.mu.Lock()
	node.failCmdAlways["dmsetup remove "+pool] = "device-mapper: remove ioctl failed: Device or resource busy"
	node.mu.Unlock()
	syncupBoth(t, srv, reqOpts{revision: 3, leftoverOk: true})
	st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	info, _ := srv.readCntlr(context.Background(), st)
	if !info.GetStackLeft() {
		t.Errorf("a standby's read does not report the stack its sweep left")
	}
	opts := reqOpts{revision: 4, primary: true, term: 20, leftoverOk: true}
	reply := syncupBoth(t, srv, opts)
	row := reply.GetCntlrInfo().GetSliceIdToDmPool()[testSlice]
	if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(row.GetDetails(), "earlier term") {
		t.Fatalf("the pool row of a primary over an earlier term's stack "+
			"reads %v %q, want ERROR", row.GetStatus(), row.GetDetails())
	}
	round, _ := resendRound(t, srv, cntlrReq(opts), "term")
	assertCodeNames(t, "the read of a primary under another term", round,
		"term 10")
}

// TestTheOldStackGoesBeforeTheNewBuild: the old term's pool is removed
// before the new term's is made (CN35).
func TestTheOldStackGoesBeforeTheNewBuild(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, term: 10})
	pool := poolName(srv)
	node.mu.Lock()
	node.failCmdAlways["dmsetup remove "+pool] = "busy"
	node.mu.Unlock()
	syncupBoth(t, srv, reqOpts{revision: 3, leftoverOk: true})
	node.mu.Lock()
	delete(node.failCmdAlways, "dmsetup remove "+pool)
	node.mu.Unlock()
	node.Reset()
	syncupBoth(t, srv, reqOpts{revision: 4, primary: true, term: 20})
	assertOrder(t, node, "cmd dmsetup remove "+pool,
		"cmd dmsetup create "+pool)
}

// TestALostTermFileTakesTheServingStack: a primary whose term file is lost,
// and whose namespaces serve, takes its stack as it stands at the restart
// rather than tear it down (CN35).
func TestALostTermFileTakesTheServingStack(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, term: 10})
	path := srv.nf.LocalStackTermPath(testCluster, testCn, testSp, testCntlr)
	node.mu.Lock()
	if _, ok := node.protos[path]; !ok {
		node.mu.Unlock()
		t.Fatalf("fixture is wrong: no term file at %s", path)
	}
	delete(node.protos, path)
	node.mu.Unlock()
	node.Reset()
	restarted := newCnServer(node)
	reconcileForTest(t, restarted)
	if node.hasCall("cmd dmsetup remove " + poolName(srv)) {
		t.Errorf("a serving stack with a lost term file was torn down")
	}
	if got := restarted.getCntlr(cntlrKey(testCluster, testCn, testSp,
		testCntlr)).stackTerm; got != 10 {
		t.Errorf("the restart took the term %d, want 10", got)
	}
}

// TestAFailedTermSaveIsMadeAgain: a save of the term that fails keeps the
// term in memory, and the next primary converge under that term saves it
// again, so a restart reads it (CN35; saveTermAgain).
func TestAFailedTermSaveIsMadeAgain(t *testing.T) {
	srv, node := newTestServer(t)
	path := srv.nf.LocalStackTermPath(testCluster, testCn, testSp, testCntlr)
	fault := failSaves(srv, node, path)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, term: 10})
	if !node.hasCall(failedWrite(path)) {
		t.Fatalf("fixture is wrong: the term's save did not fail")
	}
	if got := termOf(srv); got != 10 {
		t.Fatalf("a failed save lost the term in memory: %d", got)
	}
	fault.mu.Lock()
	delete(fault.paths, path)
	fault.mu.Unlock()
	syncupBoth(t, srv, reqOpts{revision: 3, primary: true, term: 10})
	node.mu.Lock()
	_, saved := node.protos[path]
	node.mu.Unlock()
	if !saved {
		t.Fatalf("the next converge under the term did not save it again")
	}
	restarted := newCnServer(node)
	reconcileForTest(t, restarted)
	if got := restarted.getCntlr(cntlrKey(testCluster, testCn, testSp,
		testCntlr)).stackTerm; got != 10 {
		t.Errorf("the restart loaded the term %d, want 10", got)
	}
}
