package cnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// CN10/CN18 — the connect step waits, briefly and boundedly, for what it just
// asked for
// ---------------------------------------------------------------------------
//
// One wait budget per converge pass, CnConnectPassBudget, shared by every leg
// and every clone source, drawn on by exactly three things: a failed connect's
// own elapsed time, the CnConnectRetryPause before each in-pass retry, and the
// CnNsScanPause steps of the wait for the namespace head after a connect the
// pass made. Every test here runs the pass on a fake clock (withPassClock), so
// "how long" is exact: a failed connect takes no time unless the fake says so
// (connectTakes), and a pause moves the clock by exactly its length. On that
// clock a 1 s budget holds exactly ten 100 ms pauses — eleven attempts of a
// connect that keeps failing.

// withPassClock puts a server's CN10 pass budget on a fake clock: its sleep
// moves the clock on by exactly the pause, and a slow connect of the fake node
// moves the same clock (fakeNode.connectTakes).
func withPassClock(srv *CnAgentServer, node *fakeNode) *fakeClock {
	clock := withClock(srv)
	srv.sleep = func(ctx context.Context, d time.Duration) error {
		clock.advance(d)
		return ctx.Err()
	}
	node.mu.Lock()
	node.advance = clock.advance
	node.mu.Unlock()
	return clock
}

// failConnects refuses the first n connects to one endpoint and then lets
// them through; slowConnects makes every connect to it take d.
func failConnects(node *fakeNode, nqn, addr, svcId string, n int) {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.connectFail[connectKey(nqn, addr, svcId)] = n
}

func slowConnects(node *fakeNode, nqn, addr, svcId string, d time.Duration) {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.connectTakes[connectKey(nqn, addr, svcId)] = d
}

// deferNsHead makes the next subsystem created for nqn show its namespace head
// only on the nth listing of its directory.
func deferNsHead(node *fakeNode, nqn string, n int) {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.nsHeadAfter[nqn] = n
}

// connectsTo counts the recorded `nvme connect`s to one endpoint, failed ones
// included.
func connectsTo(node *fakeNode, nqn, addr, svcId string) int {
	n := 0
	for _, call := range node.callsMatching("cmd nvme connect ") {
		if strings.Contains(call, "--traddr "+addr+" ") &&
			strings.Contains(call, "--trsvcid "+svcId+" ") &&
			strings.Contains(call, "--nqn "+nqn+" ") {
			n++
		}
	}
	return n
}

// errorRows names every RES_STATUS_ERROR row of one reply: the rows that
// would stamp a primary's err_epoch, or — in the leg map — a leg's.
func errorRows(info *pb.CntlrInfo) []string {
	var out []string
	add := func(kind string, rows map[uint64]*pb.ResInfo) {
		for id, row := range rows {
			if row.GetStatus() == pb.ResStatus_RES_STATUS_ERROR {
				out = append(out, fmt.Sprintf("%s[%#x] %q", kind, id,
					row.GetDetails()))
			}
		}
	}
	add("ss", info.GetSsIdToSubsystem())
	add("ns", info.GetNsIdToNamespace())
	add("ns_dev", info.GetNsIdToDmLinear())
	add("raid0", info.GetTdIdToRaid0())
	add("td_error", info.GetTdIdToDmError())
	add("pool", info.GetSliceIdToDmPool())
	add("pool_meta", info.GetSliceIdToMeta())
	add("pool_data", info.GetSliceIdToData())
	add("grp", info.GetGrpIdToMdRaid())
	add("leg", info.GetLegIdToLeg())
	add("clone_tgt", info.GetCloneIdToTarget())
	add("clone_dm", info.GetCloneIdToDmClone())
	add("clone_meta", info.GetCloneIdToMeta())
	for tdId, thin := range info.GetTdIdToThinInfo() {
		add(fmt.Sprintf("thin[td %#x]", tdId), thin.GetSliceIdToDmThin())
	}
	return out
}

// growReq is the shape of the e2e react case's automatic grow: the fixture's
// primary with a second data group appended to its slice, the new group's one
// leg on testIp2:testSvcId2 and already provisioned — the pass right after the
// worker flips the new sides, which fans SyncupSide and SyncupCntlr out
// unordered, so the primary's connect can beat the disk node's export.
func growReq(revision uint64) *pb.SyncupCntlrRequest {
	req := cntlrReq(reqOpts{revision: revision, primary: true})
	grownDataGrp(req, true)
	return req
}

// syncupGrow builds the primary at revision 2 and then grows it at 3, with
// arm applied between the two — the only moment the new leg's connect and head
// can be faked, since the grow's pass is the one that connects it.
func syncupGrow(
	t *testing.T,
	srv *CnAgentServer,
	node *fakeNode,
	arm func(),
) *pb.SyncupCntlrReply {
	t.Helper()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	arm()
	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), growReq(3))
	if err != nil {
		t.Fatalf("grow: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("grow rejected: %v", reply.GetAgentReply())
	}
	return reply
}

// TestConnectRetriedWithinThePass pins the in-pass connect retry: a side whose
// disk node refuses the first connects — its export not linked into the port
// yet — is connected by the SAME pass, so the first reply is clean and no
// background retry is registered. On a primary that is the whole of the e2e
// react grow's failure: the grow's first report used to carry "pool concat
// missing", which stamped a settled primary's err_epoch and failed it over
// one reaction pass later.
func TestConnectRetriedWithinThePass(t *testing.T) {
	t.Run("standby leg", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.retryInterval = time.Hour
		clock := withPassClock(srv, node)
		nqn := legNqn(srv, testDataLeg)
		failConnects(node, nqn, testIp, testSvcId, 2)
		start := clock.Now()
		reply := syncupBoth(t, srv, reqOpts{revision: 2})

		assertOk(t, reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg],
			"standby leg data, connected on its third attempt")
		if got := connectsTo(node, nqn, testIp, testSvcId); got != 3 {
			t.Fatalf("%d connects to the data leg, want 3 (two refused, "+
				"then accepted)", got)
		}
		if got, want := clock.Now().Sub(start),
			2*common.CnConnectRetryPause; got != want {
			t.Fatalf("the pass waited %v, want the two retry pauses %v",
				got, want)
		}
		if retrying(t, srv) {
			t.Fatalf("a leg the pass connected registered the retry")
		}
	})
	t.Run("primary grow", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.retryInterval = time.Hour
		withPassClock(srv, node)
		nqn := legNqn(srv, testDataLeg2)
		reply := syncupGrow(t, srv, node, func() {
			failConnects(node, nqn, testIp2, testSvcId2, 1)
		})
		info := reply.GetCntlrInfo()
		if rows := errorRows(info); len(rows) != 0 {
			t.Fatalf("the grow's first reply carries ERROR rows %v", rows)
		}
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp2], "grown group")
		assertOk(t, info.GetSliceIdToData()[testSlice], "pool data concat")
		assertOk(t, info.GetSliceIdToDmPool()[testSlice], "pool")
		if got := connectsTo(node, nqn, testIp2, testSvcId2); got != 2 {
			t.Fatalf("%d connects to the grown leg, want 2", got)
		}
		if retrying(t, srv) {
			t.Fatalf("the grow registered the retry")
		}
	})
}

// TestNsHeadAwaitedAfterConnect pins the head wait: the kernel
// returns from `nvme connect` before its namespace scan has added the
// multipath head, and a single re-read right after the connect used to fail
// the leg with "no multipath namespace". The pass now re-reads until the head
// is there.
func TestNsHeadAwaitedAfterConnect(t *testing.T) {
	for _, n := range []int{2, 3} {
		t.Run(fmt.Sprintf("standby, head on read %d", n), func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			clock := withPassClock(srv, node)
			deferNsHead(node, legNqn(srv, testDataLeg), n)
			start := clock.Now()
			reply := syncupBoth(t, srv, reqOpts{revision: 2})

			assertOk(t, reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg],
				"standby leg data")
			if retrying(t, srv) {
				t.Fatalf("a head that appeared registered the retry")
			}
			want := time.Duration(n-1) * common.CnNsScanPause
			if got := clock.Now().Sub(start); got != want {
				t.Fatalf("the pass waited %v, want %d scan pause(s), %v",
					got, n-1, want)
			}
		})
	}
	t.Run("primary grow, head on read 3", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.retryInterval = time.Hour
		withPassClock(srv, node)
		reply := syncupGrow(t, srv, node, func() {
			deferNsHead(node, legNqn(srv, testDataLeg2), 3)
		})
		info := reply.GetCntlrInfo()
		if rows := errorRows(info); len(rows) != 0 {
			t.Fatalf("the grow's first reply carries ERROR rows %v", rows)
		}
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp2], "grown group")
		assertOk(t, info.GetSliceIdToDmPool()[testSlice], "pool")
		if retrying(t, srv) {
			t.Fatalf("the grow registered the retry")
		}
	})
}

// TestSlowFailedConnectIsNotRetried pins the charge of a failed connect's own
// elapsed time. A connect to a disk node whose VM is down takes about 3 s (the
// kernel's SYN retries, or CmdSoftTimeout) and spends the whole 1 s budget at
// once, so the pass does NOT try it again: the leg fails exactly as it did
// before the retry existed, and the background retry takes over.
func TestSlowFailedConnectIsNotRetried(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	clock := withPassClock(srv, node)
	nqn := legNqn(srv, testDataLeg)
	failConnects(node, nqn, testIp, testSvcId, 1000)
	slowConnects(node, nqn, testIp, testSvcId,
		common.CmdSoftTimeout*time.Second)
	start := clock.Now()
	reply := syncupBoth(t, srv, reqOpts{revision: 2})

	if got := connectsTo(node, nqn, testIp, testSvcId); got != 1 {
		t.Fatalf("%d connects to a dead disk node in one pass, want 1: a "+
			"failed connect that took the whole budget must not be retried",
			got)
	}
	assertErrorDetails(t, reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg],
		"connection refused", "leg data")
	if !retrying(t, srv) {
		t.Fatalf("a leg that did not connect registered no retry")
	}
	if got, want := clock.Now().Sub(start),
		common.CmdSoftTimeout*time.Second; got != want {
		t.Fatalf("the pass took %v, want the one connect's %v and no pause",
			got, want)
	}
}

// TestPassBudgetSharedByEveryLeg pins the ONE budget per pass: the meta leg
// (the plan's first) keeps failing and spends the whole budget on its own
// retries, so the data leg's failed connect gets no retry at all — although a
// retry would have succeeded. A budget per leg would multiply what a pass with
// several failing sides costs under the object lock the Check rounds need.
// The next pass is a new budget: a retry attempt retries again.
func TestPassBudgetSharedByEveryLeg(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	withPassClock(srv, node)
	metaNqn := legNqn(srv, testMetaLeg)
	dataNqn := legNqn(srv, testDataLeg)
	failConnects(node, metaNqn, testIp, testSvcId, 1000)
	failConnects(node, dataNqn, testIp, testSvcId, 1)
	reply := syncupBoth(t, srv, reqOpts{revision: 2})

	if got := connectsTo(node, metaNqn, testIp, testSvcId); got != 11 {
		t.Fatalf("%d connects to the meta leg, want 11: one, then a retry "+
			"after each of the ten 100 ms pauses a 1 s budget holds", got)
	}
	if got := connectsTo(node, dataNqn, testIp, testSvcId); got != 1 {
		t.Fatalf("%d connects to the data leg, want 1: the meta leg spent "+
			"the pass's one budget", got)
	}
	info := reply.GetCntlrInfo()
	assertErrorDetails(t, info.GetLegIdToLeg()[testMetaLeg],
		"connection refused", "leg meta")
	assertErrorDetails(t, info.GetLegIdToLeg()[testDataLeg],
		"connection refused", "leg data")
	if !retrying(t, srv) {
		t.Fatalf("legs that did not connect registered no retry")
	}

	// A background retry attempt is a pass of its own, with a budget of its
	// own: the data leg connects on its first try, and the meta leg gets its
	// eleven attempts again rather than none.
	node.Reset()
	retryAttempt(t, srv)
	if got := connectsTo(node, metaNqn, testIp, testSvcId); got != 11 {
		t.Fatalf("%d connects to the meta leg in the retry attempt, want "+
			"11: every pass has its own budget", got)
	}
	if got := connectsTo(node, dataNqn, testIp, testSvcId); got != 1 {
		t.Fatalf("%d connects to the data leg in the retry attempt, want 1",
			got)
	}
}

// TestNoHeadWaitWithoutAConnect pins the scope of the head wait: only a
// connect made in this pass is waited for. A subsystem that is already
// connected and has no head — the shape of a leg whose only path is ANA
// inaccessible, which never gets one — is judged on its one read, with no
// pause and no extra read, however long the budget is.
func TestNoHeadWaitWithoutAConnect(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	clock := withPassClock(srv, node)
	syncupBoth(t, srv, reqOpts{revision: 2})
	nqn := legNqn(srv, testDataLeg)
	node.mu.Lock()
	dir := fmt.Sprintf("%s/nvme-subsys%d", sysfsNvmeSubsysDir,
		node.subsystems[nqn].idx)
	node.mu.Unlock()
	listings := func() int {
		n := 0
		for _, call := range node.Calls() {
			if call == "cmd ls -1 "+dir {
				n++
			}
		}
		return n
	}

	// The control: the same pass with the head present.
	node.Reset()
	start := clock.Now()
	if _, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 3})); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	withHead := listings()
	if withHead == 0 {
		t.Fatalf("the pass never listed %s; the comparison is vacuous", dir)
	}

	node.dropNsHead(nqn)
	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 4}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	assertErrorDetails(t, reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg],
		"no multipath namespace", "leg data")
	assertNoCall(t, node, "cmd nvme connect")
	if got := listings(); got != withHead {
		t.Fatalf("the headless pass listed %s %d times, the headed one %d: "+
			"a subsystem this pass did not connect was waited for",
			dir, got, withHead)
	}
	if waited := clock.Now().Sub(start); waited != 0 {
		t.Fatalf("the passes waited %v, want none", waited)
	}
}

// TestCloneSourceConnectStep pins CN18's clone-source connect as the same
// connect step on the same rules: a refused-then-accepted source and a source
// whose namespace appears on the second read both build on the first reply.
func TestCloneSourceConnectStep(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(node *fakeNode)
	}{
		{"refused, then accepted", func(node *fakeNode) {
			failConnects(node, testSrcNqn, testIp2, testSvcId2, 1)
		}},
		{"namespace on read 2", func(node *fakeNode) {
			deferNsHead(node, testSrcNqn, 2)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			withPassClock(srv, node)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			tc.arm(node)
			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 3, primary: true,
					clones: []*pb.Clone{cloneOf()}}))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			info := reply.GetCntlrInfo()
			assertOk(t, info.GetCloneIdToTarget()[testClone], "clone target")
			assertOk(t, info.GetCloneIdToDmClone()[testClone], "dm-clone")
			if retrying(t, srv) {
				t.Fatalf("a source the pass connected registered the retry")
			}
		})
	}
}

// TestCloneSourceSharesThePassBudget pins that the clone source draws on the
// budget the legs drew on, not on one of its own: with the meta leg having
// spent it, the source's refused connect is not retried and the clone reports
// its step-1 failure, as before the retry existed.
func TestCloneSourceSharesThePassBudget(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	withPassClock(srv, node)
	failConnects(node, legNqn(srv, testMetaLeg), testIp, testSvcId, 1000)
	failConnects(node, testSrcNqn, testIp2, testSvcId2, 1)
	reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		clones: []*pb.Clone{cloneOf()}})

	if got := connectsTo(node, testSrcNqn, testIp2, testSvcId2); got != 1 {
		t.Fatalf("%d connects to the clone source, want 1: the legs spent "+
			"the pass's one budget", got)
	}
	info := reply.GetCntlrInfo()
	assertErrorDetails(t, info.GetCloneIdToTarget()[testClone],
		"connection refused", "clone target")
	if dm := info.GetCloneIdToDmClone()[testClone]; dm.GetStatus() !=
		pb.ResStatus_RES_STATUS_MISSING ||
		dm.GetDetails() != "source not connected" {
		t.Fatalf("dm-clone row %v %q, want MISSING \"source not connected\"",
			dm.GetStatus(), dm.GetDetails())
	}
	if !retrying(t, srv) {
		t.Fatalf("a source that did not connect registered no retry")
	}
}
