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
// steps of the wait for the namespace head after a connect the pass made — a
// CnNsScanPause while no head is picked, a CnNsHeadPause while the picked head
// lacks its ng device. Every test here runs the pass on a fake clock
// (withPassClock), so "how long" is exact: a failed connect takes no time
// unless the fake says so (connectTakes), and a pause moves the clock by
// exactly its length. On that clock the CnConnectPassBudget budget holds
// exactly CnConnectPassBudget / CnConnectRetryPause pauses — one more attempt
// than that of a connect that keeps failing.

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

// deferNsHead makes the next subsystem created for nqn show its whole
// namespace head, ng device included, only on the nth listing of its
// directory.
func deferNsHead(node *fakeNode, nqn string, n int) {
	stageNsHead(node, nqn, headStages{dir: n, nsid: n, block: n, ng: n})
}

// stageNsHead makes the next subsystem created for nqn show each step of its
// namespace head on the listing of its directory at names.
func stageNsHead(node *fakeNode, nqn string, at headStages) {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.nsHeadAt[nqn] = at
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
// worker flips the new sides. The worker's sides-first hold is bounded, so the
// primary's connect can still beat the disk node's export.
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
// background retry is registered. Without it a grow's first report would
// carry "pool concat missing" and stamp a settled primary's err_epoch.
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
// multipath head, so the pass re-reads until the head is complete instead of
// failing the leg on one read with "no multipath namespace". The first
// re-read after a connect step walks the node again, and each later one reads
// the new entry alone (CN10): the converge walks at its start and once after
// each connect step, however many re-reads the head takes.
func TestNsHeadAwaitedAfterConnect(t *testing.T) {
	walks := func(node *fakeNode) int {
		return countCalls(node, "list "+sysfsNvmeSubsysDir)
	}
	for _, n := range []int{2, 3} {
		t.Run(fmt.Sprintf("standby, head on read %d", n), func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			clock := withPassClock(srv, node)
			deferNsHead(node, legNqn(srv, testDataLeg), n)
			cnSyncup(t, srv, 2, true)
			node.Reset()
			start := clock.Now()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 2}))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if reply.GetAgentReply().GetCode() != 0 {
				t.Fatalf("SyncupCntlr rejected: %v", reply.GetAgentReply())
			}

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
			// The first build connects the meta leg, then the data leg.
			if got := walks(node); got != 3 {
				t.Fatalf("the first build listed %s %d times, want 3: at "+
					"its start and after each of its two connect steps",
					sysfsNvmeSubsysDir, got)
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
		if got := walks(node); got != 2 {
			t.Fatalf("the grow listed %s %d times, want 2: at its start and "+
				"after its one connect step", sysfsNvmeSubsysDir, got)
		}
	})
}

// headOf is the multipath namespace head the fake made for nqn's subsystem,
// as the kernel names it, and that subsystem's sysfs directory.
func headOf(t *testing.T, node *fakeNode, nqn string) (string, string) {
	t.Helper()
	node.mu.Lock()
	defer node.mu.Unlock()
	subsys := node.subsystems[nqn]
	if subsys == nil {
		t.Fatalf("the node holds no subsystem for %s", nqn)
	}
	nsIdx := uint32(1)
	if want, ok := node.nsIdxOf[nqn]; ok {
		nsIdx = want
	}
	return fmt.Sprintf("nvme%dn%d", subsys.idx, nsIdx),
		fmt.Sprintf("%s/nvme-subsys%d", sysfsNvmeSubsysDir, subsys.idx)
}

// finishNsHead makes every step of nqn's staged head that has not shown yet
// show on the next listing of its directory.
func finishNsHead(t *testing.T, node *fakeNode, nqn string) {
	t.Helper()
	_, dir := headOf(t, node, nqn)
	node.mu.Lock()
	defer node.mu.Unlock()
	head := node.pendingHeads[dir]
	if head == nil {
		t.Fatalf("the head of %s is not staged", nqn)
	}
	head.at = headStages{}
}

// numberReads counts the reads of a head's device number (blockDevNo).
func numberReads(node *fakeNode, head string) int {
	return countCalls(node, "read "+sysfsBlockDir+"/"+head+"/dev")
}

// neverInThePass is a step count no pass of a test reaches.
const neverInThePass = 1 << 20

// TestLegBuiltOnceItsHeadIsComplete pins the end of the head wait (CN10): the
// kernel adds a head in steps, and its /sys/block entry comes after the head
// is listed with its nsid, so the wait goes on until the head's ng device,
// the last step, is listed beside it. Here the /sys/block entry and the ng
// device come three listings after the head: the pass takes three short
// pauses, never a scan pause, reads the head's number once, after the head
// is complete, and builds the leg, so the reply is clean and no background
// retry is registered. A wait that ended at the first listing that picks the
// head would read the number before the kernel made it and fail the leg with
// "block device … is not there": in a grow of a running pool its group then
// cannot be made and the pool rows read ERROR, which stamp the primary's
// err_epoch.
func TestLegBuiltOnceItsHeadIsComplete(t *testing.T) {
	late := headStages{dir: 1, nsid: 1, block: 4, ng: 4}
	const shortPauses = 3
	t.Run("standby, first build", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.retryInterval = time.Hour
		clock := withPassClock(srv, node)
		nqn := legNqn(srv, testDataLeg)
		stageNsHead(node, nqn, late)
		start := clock.Now()
		reply := syncupBoth(t, srv, reqOpts{revision: 2})

		info := reply.GetCntlrInfo()
		if rows := errorRows(info); len(rows) != 0 {
			t.Fatalf("the first reply carries ERROR rows %v", rows)
		}
		assertOk(t, info.GetLegIdToLeg()[testDataLeg], "standby leg data")
		if retrying(t, srv) {
			t.Fatalf("a head that completed in the pass registered the retry")
		}
		if got, want := clock.Now().Sub(start),
			shortPauses*common.CnNsHeadPause; got != want {
			t.Fatalf("the pass waited %v, want %d short pauses, %v",
				got, shortPauses, want)
		}
		head, _ := headOf(t, node, nqn)
		if got := numberReads(node, head); got != 1 {
			t.Fatalf("the pass read %s's number %d times, want once",
				head, got)
		}
	})
	t.Run("primary grow", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.retryInterval = time.Hour
		clock := withPassClock(srv, node)
		nqn := legNqn(srv, testDataLeg2)
		var start time.Time
		reply := syncupGrow(t, srv, node, func() {
			stageNsHead(node, nqn, late)
			start = clock.Now()
		})
		info := reply.GetCntlrInfo()
		if rows := errorRows(info); len(rows) != 0 {
			t.Fatalf("the grow's first reply carries ERROR rows %v", rows)
		}
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp2], "grown group")
		assertOk(t, info.GetSliceIdToData()[testSlice], "pool data concat")
		assertOk(t, info.GetSliceIdToDmPool()[testSlice], "pool")
		if retrying(t, srv) {
			t.Fatalf("the grow registered the retry")
		}
		if got, want := clock.Now().Sub(start),
			shortPauses*common.CnNsHeadPause; got != want {
			t.Fatalf("the grow waited %v, want %d short pauses, %v",
				got, shortPauses, want)
		}
		head, _ := headOf(t, node, nqn)
		if got := numberReads(node, head); got != 1 {
			t.Fatalf("the grow read %s's number %d times, want once",
				head, got)
		}
	})
}

// TestHeadWaitWaitsForTheNgDevice pins the sign of a complete head (CN10):
// the ng device, not the /sys/block entry. Here the head is listed with its
// nsid and its /sys/block entry, and its ng device comes one listing later.
// The kernel lets the head be opened by its number only after its /sys/block
// entry, and adds the ng device after that, so the pass pauses once more,
// for one short pause, and reads the head's number only after the listing
// that shows the ng device. A wait that ended at the /sys/block entry, or at
// the head alone, would take no pause and read the number at once.
func TestHeadWaitWaitsForTheNgDevice(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	clock := withPassClock(srv, node)
	nqn := legNqn(srv, testDataLeg)
	stageNsHead(node, nqn, headStages{dir: 1, nsid: 1, block: 1, ng: 2})
	start := clock.Now()
	reply := syncupBoth(t, srv, reqOpts{revision: 2})

	assertOk(t, reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg],
		"standby leg data")
	if retrying(t, srv) {
		t.Fatalf("a head that completed in the pass registered the retry")
	}
	if got, want := clock.Now().Sub(start), common.CnNsHeadPause; got != want {
		t.Fatalf("the pass waited %v, want one short pause, %v", got, want)
	}
	head, dir := headOf(t, node, nqn)
	calls := node.Calls()
	listings := 0
	for i, call := range calls {
		switch call {
		case "list " + dir:
			listings++
		case "read " + sysfsBlockDir + "/" + head + "/dev":
			if listings < 2 {
				t.Fatalf("the pass read %s's number after %d listing(s) "+
					"of %s, before the one that shows its ng device: %q",
					head, listings, dir, calls[:i+1])
			}
		}
	}
	if got := numberReads(node, head); got != 1 {
		t.Fatalf("the pass read %s's number %d times, want once", head, got)
	}
}

// TestCloneSourceWaitsForACompleteHead pins CN18's use of the same head
// wait: a clone source the pass connected whose /sys/block entry and ng
// device come three listings after its head is waited for with three short
// pauses, and the clone builds on the first reply, reading the source's
// number once, with no background retry. A wait that ended at the first
// listing that picks the head would leave the dm-clone's number read to fail.
func TestCloneSourceWaitsForACompleteHead(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	clock := withPassClock(srv, node)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	stageNsHead(node, testSrcNqn, headStages{dir: 1, nsid: 1, block: 4, ng: 4})
	node.Reset()
	start := clock.Now()
	reply, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 3, primary: true,
			clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	info := reply.GetCntlrInfo()
	if rows := errorRows(info); len(rows) != 0 {
		t.Fatalf("the reply carries ERROR rows %v", rows)
	}
	assertOk(t, info.GetCloneIdToTarget()[testClone], "clone target")
	assertOk(t, info.GetCloneIdToDmClone()[testClone], "dm-clone")
	if retrying(t, srv) {
		t.Fatalf("a source whose head completed in the pass registered the " +
			"retry")
	}
	if got, want := clock.Now().Sub(start),
		3*common.CnNsHeadPause; got != want {
		t.Fatalf("the pass waited %v, want three short pauses, %v", got, want)
	}
	head, _ := headOf(t, node, testSrcNqn)
	if got := numberReads(node, head); got != 1 {
		t.Fatalf("the pass read %s's number %d times, want once", head, got)
	}
}

// TestHeadWaitStopsWhenTheBudgetIsSpent pins the kept case of the head wait
// (CN10): one budget per pass, so a head that is not complete when the
// budget no longer covers the next short pause is used as it is. The meta
// leg, the first in plan order, spends the budget but for two and a half
// short pauses: its first connect fails after a long run, which is charged,
// and the retry after one CnConnectRetryPause goes through. The data leg's
// head is listed with its nsid and never completes in the pass: the wait
// takes the two short pauses the budget covers, writes msgHeadWaitSpent
// naming the subsystem, the head and those pauses, and the pass reads the
// number, which is not there yet. The leg fails with that read's own error,
// as without the wait, and registers the background retry, whose pass
// builds the leg once the kernel has finished the head.
func TestHeadWaitStopsWhenTheBudgetIsSpent(t *testing.T) {
	capture := captureLogs(t)
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	withPassClock(srv, node)
	metaNqn := legNqn(srv, testMetaLeg)
	dataNqn := legNqn(srv, testDataLeg)
	left := 2*common.CnNsHeadPause + common.CnNsHeadPause/2
	failConnects(node, metaNqn, testIp, testSvcId, 1)
	slowConnects(node, metaNqn, testIp, testSvcId,
		common.CnConnectPassBudget-common.CnConnectRetryPause-left)
	stageNsHead(node, dataNqn, headStages{dir: 1, nsid: 1,
		block: neverInThePass, ng: neverInThePass})
	reply := syncupBoth(t, srv, reqOpts{revision: 2})

	head, _ := headOf(t, node, dataNqn)
	info := reply.GetCntlrInfo()
	assertOk(t, info.GetLegIdToLeg()[testMetaLeg],
		"leg meta, connected on its second attempt")
	assertErrorDetails(t, info.GetLegIdToLeg()[testDataLeg],
		"block device "+head+" is not there", "leg data")
	if !retrying(t, srv) {
		t.Fatalf("a leg whose head was not complete registered no retry")
	}
	recs := capture.msgRecords(t, msgHeadWaitSpent)
	if len(recs) != 1 {
		t.Fatalf("%d %q records, want 1: %v", len(recs), msgHeadWaitSpent,
			recs)
	}
	rec := recs[0]
	if rec["level"] != "WARN" || rec["nqn"] != dataNqn ||
		rec["head"] != "/dev/"+head || rec["head_pauses"] != float64(2) ||
		rec["scan_pauses"] != float64(0) {
		t.Fatalf("record %v, want WARN naming %s, /dev/%s, 2 short pauses "+
			"and no scan pause", rec, dataNqn, head)
	}

	finishNsHead(t, node, dataNqn)
	node.Reset()
	retryAttempt(t, srv)
	assertOk(t, probedCntlrInfo(t, srv).GetLegIdToLeg()[testDataLeg],
		"leg data, built by the retry's pass")
	if retrying(t, srv) {
		t.Fatalf("the retry's pass built the leg and left the retry " +
			"registered")
	}
}

// TestHeadWaitSpentBeforeAnyHeadIsListed pins the record of the kept case
// when no head was picked (CN10): the budget stops a wait that has seen no
// head at all, and the record says so. The meta leg spends the budget as in
// TestHeadWaitStopsWhenTheBudgetIsSpent, but for two and a half scan pauses;
// the data leg's subsystem lists no head in the pass. The wait takes the two
// scan pauses the budget covers, writes msgHeadWaitSpent with an empty head,
// two scan pauses and no short one, and the leg fails with "no multipath
// namespace", as without the wait, registering the background retry.
func TestHeadWaitSpentBeforeAnyHeadIsListed(t *testing.T) {
	capture := captureLogs(t)
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	withPassClock(srv, node)
	metaNqn := legNqn(srv, testMetaLeg)
	dataNqn := legNqn(srv, testDataLeg)
	left := 2*common.CnNsScanPause + common.CnNsScanPause/2
	failConnects(node, metaNqn, testIp, testSvcId, 1)
	slowConnects(node, metaNqn, testIp, testSvcId,
		common.CnConnectPassBudget-common.CnConnectRetryPause-left)
	stageNsHead(node, dataNqn, headStages{dir: neverInThePass,
		nsid: neverInThePass, block: neverInThePass, ng: neverInThePass})
	reply := syncupBoth(t, srv, reqOpts{revision: 2})

	info := reply.GetCntlrInfo()
	assertOk(t, info.GetLegIdToLeg()[testMetaLeg],
		"leg meta, connected on its second attempt")
	assertErrorDetails(t, info.GetLegIdToLeg()[testDataLeg],
		"no multipath namespace", "leg data")
	if !retrying(t, srv) {
		t.Fatalf("a leg with no head registered no retry")
	}
	recs := capture.msgRecords(t, msgHeadWaitSpent)
	if len(recs) != 1 {
		t.Fatalf("%d %q records, want 1: %v", len(recs), msgHeadWaitSpent,
			recs)
	}
	rec := recs[0]
	if rec["level"] != "WARN" || rec["nqn"] != dataNqn ||
		rec["head"] != "" || rec["scan_pauses"] != float64(2) ||
		rec["head_pauses"] != float64(0) {
		t.Fatalf("record %v, want WARN naming %s, no head, 2 scan pauses "+
			"and no short pause", rec, dataNqn)
	}
}

// TestHeadWaitEndedByItsCallerIsNotRecorded pins the other edge of the
// record (CN10): it tells a wait the budget stopped, so a wait that ends
// because the pass's caller went away writes none. The data leg's head is
// listed with its nsid and never completes in the pass, and the syncup's
// caller goes away during the wait's first short pause, with most of the
// budget left: the pause reports it, the wait stops, and no msgHeadWaitSpent
// is written.
func TestHeadWaitEndedByItsCallerIsNotRecorded(t *testing.T) {
	capture := captureLogs(t)
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	withPassClock(srv, node)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	movePassClock := srv.sleep
	shortPauses := 0
	srv.sleep = func(ctx context.Context, d time.Duration) error {
		if d == common.CnNsHeadPause {
			shortPauses++
			cancel()
		}
		return movePassClock(ctx, d)
	}
	dataNqn := legNqn(srv, testDataLeg)
	stageNsHead(node, dataNqn, headStages{dir: 1, nsid: 1,
		block: neverInThePass, ng: neverInThePass})
	if _, err := srv.SyncupCn(context.Background(),
		cnReq(2, true)); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if _, err := srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 2})); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}

	if shortPauses != 1 {
		t.Fatalf("the head wait took %d short pauses, want the one its "+
			"caller cut", shortPauses)
	}
	if recs := capture.msgRecords(t, msgHeadWaitSpent); len(recs) != 0 {
		t.Fatalf("a head wait its caller ended wrote %q: %v",
			msgHeadWaitSpent, recs)
	}
}

// TestHeadListedBeforeItsNsid pins the other edge of the head wait (CN10): a
// head listed before its nsid can be read is not picked, so the wait pauses
// a scan pause, not a short one, until a listing shows the head with its
// nsid, here complete on the third listing, and the pass builds the leg.
func TestHeadListedBeforeItsNsid(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	clock := withPassClock(srv, node)
	nqn := legNqn(srv, testDataLeg)
	stageNsHead(node, nqn, headStages{dir: 1, nsid: 3, block: 3, ng: 3})
	start := clock.Now()
	reply := syncupBoth(t, srv, reqOpts{revision: 2})

	assertOk(t, reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg],
		"standby leg data")
	if retrying(t, srv) {
		t.Fatalf("a head that completed in the pass registered the retry")
	}
	if got, want := clock.Now().Sub(start),
		2*common.CnNsScanPause; got != want {
		t.Fatalf("the pass waited %v, want two scan pauses, %v", got, want)
	}
}

// TestSlowFailedConnectIsNotRetried pins the charge of a failed connect's own
// elapsed time. A connect to a disk node whose VM is down takes about 3 s (the
// kernel's SYN retries, or CmdSoftTimeout) and spends the whole
// CnConnectPassBudget at once, so the pass does NOT try it again: the leg
// fails on its first attempt, and the background retry takes over.
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
			if call == "list "+dir {
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

// TestAConnectedHeadWithoutItsNgDeviceIsUsed pins the other half of the head
// wait's scope (CN10): only the wait after a connect this pass made judges
// the ng device. A subsystem that was already connected, whose head is listed
// with its nsid and its /sys/block entry but with no ng device beside it — a
// kernel that makes none, or one whose ng device did not register — is judged
// on its one read and used as it is: no pause, no extra listing, one read of
// its number, its rows OK and no msgHeadWaitSpent. Judged by the wait, such a
// head would spend the whole budget under the cntlr lock on every pass;
// judged as a head not there, it would fail for good. A leg and a clone
// source share the wait, so each gets the case.
func TestAConnectedHeadWithoutItsNgDeviceIsUsed(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts func(revision uint64) reqOpts
		nqn  func(srv *CnAgentServer) string
		ok   func(t *testing.T, info *pb.CntlrInfo)
	}{
		{"leg",
			func(revision uint64) reqOpts {
				return reqOpts{revision: revision}
			},
			func(srv *CnAgentServer) string {
				return legNqn(srv, testDataLeg)
			},
			func(t *testing.T, info *pb.CntlrInfo) {
				assertOk(t, info.GetLegIdToLeg()[testDataLeg],
					"standby leg data")
			}},
		{"clone source",
			func(revision uint64) reqOpts {
				return reqOpts{revision: revision, primary: true,
					clones: []*pb.Clone{cloneOf()}}
			},
			func(*CnAgentServer) string { return testSrcNqn },
			func(t *testing.T, info *pb.CntlrInfo) {
				assertOk(t, info.GetCloneIdToTarget()[testClone],
					"clone target")
				assertOk(t, info.GetCloneIdToDmClone()[testClone],
					"dm-clone")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := captureLogs(t)
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			clock := withPassClock(srv, node)
			syncupBoth(t, srv, tc.opts(2))
			head, dir := headOf(t, node, tc.nqn(srv))
			pass := func(revision uint64) *pb.CntlrInfo {
				t.Helper()
				node.Reset()
				reply, err := srv.SyncupCntlr(context.Background(),
					cntlrReq(tc.opts(revision)))
				if err != nil {
					t.Fatalf("SyncupCntlr: %v", err)
				}
				if reply.GetAgentReply().GetCode() != 0 {
					t.Fatalf("SyncupCntlr rejected: %v", reply.GetAgentReply())
				}
				info := reply.GetCntlrInfo()
				if rows := errorRows(info); len(rows) != 0 {
					t.Fatalf("the reply carries ERROR rows %v", rows)
				}
				tc.ok(t, info)
				assertNoCall(t, node, "cmd nvme connect")
				return info
			}

			// The control: the same pass with the head complete.
			start := clock.Now()
			pass(3)
			listings := countCalls(node, "list "+dir)
			reads := numberReads(node, head)
			if listings == 0 || reads == 0 {
				t.Fatalf("the pass listed %s %d times and read %s's number "+
					"%d times; the comparison is vacuous", dir, listings,
					head, reads)
			}

			ng := dir + "/" + ngDevOf(head)
			node.mu.Lock()
			listed := node.dirs[ng]
			delete(node.dirs, ng)
			node.mu.Unlock()
			if !listed {
				t.Fatalf("the node lists no %s; the case is vacuous", ng)
			}
			pass(4)
			if got := countCalls(node, "list "+dir); got != listings {
				t.Fatalf("the pass listed %s %d times, the control %d: a "+
					"subsystem this pass did not connect was waited for",
					dir, got, listings)
			}
			if got := numberReads(node, head); got != reads {
				t.Fatalf("the pass read %s's number %d times, the control %d",
					head, got, reads)
			}
			if waited := clock.Now().Sub(start); waited != 0 {
				t.Fatalf("the passes waited %v, want none", waited)
			}
			if retrying(t, srv) {
				t.Fatalf("a head used as it is registered the retry")
			}
			if recs := capture.msgRecords(t, msgHeadWaitSpent); len(recs) != 0 {
				t.Fatalf("a pass with no head wait wrote %q: %v",
					msgHeadWaitSpent, recs)
			}
		})
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

// TestAConvergeWalksAgainAfterAConnect pins the walk after a connect step
// (CN10): a converge walks the node once, and a connect step that ran a
// connect marks that walk stale, so the lookup after it walks again — the
// subsystem the connect made may have taken the number of an entry freed
// since the walk. The leg's second side lost its controller's device while
// its subsystem entry stays and keeps its number, so a lookup in the old
// walk would find the entry all the same: only the count of walks tells the
// two apart. The walk after the connect serves every later lookup of the
// pass: when the meta leg, the first in plan order, is the one connected
// again, the data leg's lookup after it does not walk once more. A converge
// that connects nothing walks once.
func TestAConvergeWalksAgainAfterAConnect(t *testing.T) {
	for _, tc := range []struct {
		name string
		leg  uint64
	}{
		{"the data leg, the last in plan order", testDataLeg},
		{"the meta leg, before the data leg", testMetaLeg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			opts := reqOpts{revision: 2, extraSide: true}
			syncupBoth(t, srv, opts)
			walks := func() int {
				return countCalls(node, "list "+sysfsNvmeSubsysDir)
			}

			node.Reset()
			if _, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(opts)); err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			assertNoCall(t, node, "cmd nvme connect")
			if got := walks(); got != 1 {
				t.Fatalf("a converge that connects nothing listed %s %d "+
					"times, want 1", sysfsNvmeSubsysDir, got)
			}

			nqn := legNqn(srv, tc.leg)
			if node.deleteCtrlDevice(nqn, testIp2, testSvcId2) == "" {
				t.Fatalf("the leg has no controller for its second side")
			}
			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(opts))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if got := connectsTo(node, nqn, testIp2, testSvcId2); got != 1 ||
				callCnt(node, "cmd nvme connect ") != 1 {
				t.Fatalf("the converge made %q, want one connect of the "+
					"leg's second side", node.callsMatching("cmd nvme connect"))
			}
			if got := walks(); got != 2 {
				t.Fatalf("a converge that connected a side listed %s %d "+
					"times, want 2: at its start and after the connect step",
					sysfsNvmeSubsysDir, got)
			}
			info := reply.GetCntlrInfo()
			assertOk(t, info.GetLegIdToLeg()[tc.leg],
				"the leg, its second side connected again")
			assertOk(t, info.GetLegIdToLeg()[testMetaLeg], "leg meta")
			assertOk(t, info.GetLegIdToLeg()[testDataLeg], "leg data")
		})
	}
}

// TestTheHeadWaitWalksAgainWhenItsEntryGoes pins the head wait's own walk
// (CN10): after a connect the wait reads the new entry alone, while the entry
// it found stands, and walks again once a lookup finds no entry, or finds an
// entry that fails the subsysnqn check. In both cases the data leg's new
// subsystem goes and comes back at another number. Gone right after the
// class listing of the walk that follows its connect, it is gone to that
// walk itself: the wait's first lookup finds no entry, and the one after the
// scan pause walks again and finds the new entry with its head. Gone after
// the wait's first lookup read the entry without its head, with another
// subsystem taking its number, it fails the next lookup's check, and the
// lookup after that walks again. Read in the old walk, the wait would look
// at the gone entry until the budget ran out and fail the leg with no
// multipath namespace.
func TestTheHeadWaitWalksAgainWhenItsEntryGoes(t *testing.T) {
	for _, tc := range []struct {
		name string
		// headAfter defers the head of the leg's first entry (deferNsHead).
		headAfter int
		// arm runs under the fake's lock while the data leg's connect runs,
		// and arms the change; it returns the listing whose hook makes it.
		arm func(srv *CnAgentServer, node *fakeNode, nqn string) string
		// pauses is how many scan pauses the wait takes.
		pauses int
	}{
		{"gone to the walk after the connect", 0,
			func(srv *CnAgentServer, node *fakeNode, nqn string) string {
				node.afterList[sysfsNvmeSubsysDir] = func() {
					dropSubsysLocked(node, nqn)
					node.plantSubsys(node.nextSubsys, nqn,
						endpoint{testIp, testSvcId})
				}
				return sysfsNvmeSubsysDir
			}, 1},
		{"its number taken after the first lookup", 2,
			func(srv *CnAgentServer, node *fakeNode, nqn string) string {
				other := names(srv).SideToCnNqn(
					testCluster, testSp+1, 0x97, testCn)
				idx := node.subsystems[nqn].idx
				dir := fmt.Sprintf("%s/nvme-subsys%d", sysfsNvmeSubsysDir, idx)
				node.afterList[dir] = func() {
					dropSubsysLocked(node, nqn)
					node.plantSubsys(idx, other,
						endpoint{testIp2, testSvcId2})
					node.plantSubsys(node.nextSubsys, nqn,
						endpoint{testIp, testSvcId})
				}
				return dir
			}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			clock := withPassClock(srv, node)
			nqn := legNqn(srv, testDataLeg)
			if tc.headAfter > 0 {
				deferNsHead(node, nqn, tc.headAfter)
			}
			listing := ""
			node.mu.Lock()
			node.duringConnect[connectKey(nqn, testIp, testSvcId)] = func() {
				listing = tc.arm(srv, node, nqn)
			}
			node.mu.Unlock()
			start := clock.Now()
			reply := syncupBoth(t, srv, reqOpts{revision: 2})

			assertOk(t, reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg],
				"leg data, its entry moved during the head wait")
			if got, want := clock.Now().Sub(start),
				time.Duration(tc.pauses)*common.CnNsScanPause; got != want {
				t.Fatalf("the pass waited %v, want %d scan pause(s), %v",
					got, tc.pauses, want)
			}
			node.mu.Lock()
			_, armed := node.afterList[listing]
			node.mu.Unlock()
			if listing == "" || armed {
				t.Fatalf("the change after the connect never ran (%q)",
					listing)
			}
			if retrying(t, srv) {
				t.Fatalf("a leg the pass connected registered the retry")
			}
		})
	}
}

// TestACloneSourceConnectedAfterTheWalkIsAdopted pins the walk again before
// a clone source's connect (CN18, its first step). Every cntlr of the node
// may connect a source, so a source that another cntlr connects after the
// converge's walk is no entry in it, or, when the source left and came back
// at another number meanwhile, an entry the kernel gave to another
// subsystem, which fails the lookup's subsysnqn check. In either case,
// before the converge connects the source, it walks once more, finds the
// source with a controller at every endpoint and adopts it: no connect of
// the source, which the host would refuse as a duplicate, and the clone
// builds. A converge of the built clone, whose source its walk holds, walks
// once.
func TestACloneSourceConnectedAfterTheWalkIsAdopted(t *testing.T) {
	var eps []endpoint
	for _, tr := range cloneOf().GetSrcTrConfList() {
		eps = append(eps, endpoint{tr.GetTrAddr(), tr.GetTrSvcId()})
	}
	for _, tc := range []struct {
		name string
		// arm, called with the fake's lock held, makes another cntlr
		// connect the source while the converge walks.
		arm func(srv *CnAgentServer, node *fakeNode)
	}{
		{"no entry in the walk", func(srv *CnAgentServer, node *fakeNode) {
			node.afterList[sysfsNvmeSubsysDir] = func() {
				node.plantSubsys(node.nextSubsys, testSrcNqn, eps...)
			}
		}},
		{"its entry fails the check", func(srv *CnAgentServer,
			node *fakeNode) {
			other := names(srv).SideToCnNqn(
				testCluster, testSp+1, 0x99, testCn)
			node.plantSubsys(node.nextSubsys, testSrcNqn, eps...)
			idx := node.subsystems[testSrcNqn].idx
			path := fmt.Sprintf("%s/nvme-subsys%d/subsysnqn",
				sysfsNvmeSubsysDir, idx)
			node.afterRead[path] = func() {
				dropSubsysLocked(node, testSrcNqn)
				node.plantSubsys(idx, other, endpoint{testIp, testSvcId})
				node.plantSubsys(node.nextSubsys, testSrcNqn, eps...)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			withPassClock(srv, node)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			node.mu.Lock()
			tc.arm(srv, node)
			node.mu.Unlock()

			opts := reqOpts{revision: 3, primary: true,
				clones: []*pb.Clone{cloneOf()}}
			for _, pass := range []struct {
				name  string
				walks int
			}{
				{"the converge that adopts the source", 2},
				{"a converge of the built clone", 1},
			} {
				node.Reset()
				reply, err := srv.SyncupCntlr(context.Background(),
					cntlrReq(opts))
				if err != nil {
					t.Fatalf("%s: SyncupCntlr: %v", pass.name, err)
				}
				if reply.GetAgentReply().GetCode() != 0 {
					t.Fatalf("%s: rejected: %v", pass.name,
						reply.GetAgentReply())
				}
				if got := callCnt(node, "--nqn "+testSrcNqn+" "); got != 0 {
					t.Fatalf("%s connected a source another cntlr had "+
						"connected: %q", pass.name,
						node.callsMatching("cmd nvme connect"))
				}
				if got := countCalls(node,
					"list "+sysfsNvmeSubsysDir); got != pass.walks {
					t.Fatalf("%s listed %s %d times, want %d: its walk, "+
						"and a walk again only before a source's connect",
						pass.name, sysfsNvmeSubsysDir, got, pass.walks)
				}
				info := reply.GetCntlrInfo()
				assertOk(t, info.GetCloneIdToTarget()[testClone],
					pass.name+": clone target")
				assertOk(t, info.GetCloneIdToDmClone()[testClone],
					pass.name+": dm-clone")
			}
		})
	}
}

// TestCloneSourceSharesThePassBudget pins that the clone source draws on the
// budget the legs drew on, not on one of its own: with the meta leg having
// spent it, the source's refused connect is not retried and the clone reports
// its step-1 failure, as before the retry existed. The target row reads what
// the refused connect left, no subsystem, MISSING as a check round reads it,
// with the connect's error in its details (CN18).
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
	assertMissingDetails(t, info.GetCloneIdToTarget()[testClone],
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
