package cnagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The CN1 lock mapping: a SyncupCntlr blocked in a slow command
// blocks a same-cntlr SyncupCntlr, but neither a CN's read (node read lock
// only) nor another cntlr's converge (a different object lock).

const (
	testCntlr2 = uint64(0x2)
	// A second SP, so cntlr 2's every derived name differs from cntlr 1's
	// and the two converges genuinely share nothing but the node lock.
	testSp2 = uint64(0x3b1)
)

func cntlrPtr2() *pb.CntlrPointer {
	return &pb.CntlrPointer{SpId: testSp2, CntlrId: testCntlr2}
}

func cnReq2(revision uint64) *pb.SyncupCnRequest {
	return &pb.SyncupCnRequest{
		ClusterId: testCluster,
		CnId:      testCn,
		Revision:  revision,
		CntlrPointerList: []*pb.CntlrPointer{
			cntlrPtr(), cntlrPtr2(),
		},
	}
}

// cntlrReq2 is a second cntlr, of a second SP, on this CN: every derived name
// differs, so only the node lock is shared.
func cntlrReq2(revision uint64) *pb.SyncupCntlrRequest {
	req := cntlrReq(reqOpts{revision: revision, primary: false})
	req.CntlrPointer = cntlrPtr2()
	req.NqnToSubsystem = map[string]*pb.Subsystem{
		testNqn + "2": req.GetNqnToSubsystem()[testNqn],
	}
	return req
}

func TestLockSmoke(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.SyncupCn(ctx, cnReq2(2)); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if _, err := srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 2, primary: false})); err != nil {
		t.Fatalf("cntlr 1: %v", err)
	}

	// Gate the next pass inside a probe of cntlr 1's leg wrapper.
	gate := make(chan struct{})
	node.gate["dmsetup info --columns --noheadings -o attr "+
		legName(srv, testMetaLeg)] = gate

	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		if _, err := srv.SyncupCntlr(ctx,
			cntlrReq(reqOpts{revision: 3, primary: false})); err != nil {
			t.Errorf("gated converge: %v", err)
		}
	}()
	// Let the gated pass reach the gate.
	waitFor(t, func() bool {
		return node.hasCall("cmd dmsetup info --columns --noheadings -o attr " +
			legName(srv, testMetaLeg))
	})

	// A same-cntlr converge must not get through.
	sameCntlr := make(chan struct{})
	go func() {
		defer close(sameCntlr)
		_, _ = srv.SyncupCntlr(ctx,
			cntlrReq(reqOpts{revision: 4, primary: false}))
	}()
	select {
	case <-sameCntlr:
		t.Fatalf("a same-cntlr SyncupCntlr ran while one was in flight")
	case <-time.After(50 * time.Millisecond):
	}

	// A round and an info read of the cntlr whose converge holds its object
	// lock answer at once: they take no lock (dnagent.md SH28).
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: 2}, nil)
		if _, err := srv.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()}); err != nil {
			t.Errorf("GetCntlrInfo: %v", err)
		}
	}()
	select {
	case <-answered:
	case <-time.After(2 * time.Second):
		t.Fatalf("a CheckCntlr round or a GetCntlrInfo waited for the " +
			"object lock its cntlr's converge holds")
	}

	// A node-scoped read does get through: it takes the node read lock only.
	// The round itself takes no lock at all (dnagent.md SH28); the CN's read
	// it starts is what takes the node read lock.
	done := make(chan struct{})
	go func() {
		defer close(done)
		key := cnKey(testCluster, testCn)
		srv.refreshCn(ctx, key, srv.getCn(key))
		reply, _ := srv.checkCnRound(ctx, &pb.CheckCnRequest{
			ClusterId: testCluster, CnId: testCn, Revision: 2}, nil)
		if reply.GetAgentReply().GetCode() != 0 {
			t.Errorf("CheckCn round rejected: %v", reply.GetAgentReply())
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("a CheckCn round blocked behind another cntlr's converge")
	}

	// So does another cntlr's converge: a different object lock.
	other := make(chan struct{})
	go func() {
		defer close(other)
		if _, err := srv.SyncupCntlr(ctx, cntlrReq2(2)); err != nil {
			t.Errorf("other cntlr: %v", err)
		}
	}()
	select {
	case <-other:
	case <-time.After(2 * time.Second):
		t.Fatalf("another cntlr's converge blocked")
	}

	close(gate)
	<-blocked
	<-sameCntlr
}

// TestAStuckDisconnectDoesNotHoldTheCntlrRead pins the CN21 sweep's
// disconnect off the object lock (CN10's disconnect registry). A controller
// delete whose target vanishes mid-delete waits out the kernel's admin
// timeout (60 s) in an uninterruptible sysfs write, and no SH15 signal ends
// it: the child returns when the kernel does. Run under the cntlr's object
// lock, that child would hold every read of the cntlr — which takes the same
// lock (CN1, dnagent.md SH29) — for the whole minute, and the worker's
// SyncupCntlr with it, until its own 60 s deadline.
//
// The disconnect is parked with blockCmd, which ignores ctx exactly as such a
// child ignores its signals, and the level drop to SP_LEVEL_NO_SIDE wants
// neither of the standby's two legs. The pass must return with both
// connections named as leftovers, a read and the Check round after it must
// get through, a second pass must not issue a second disconnect for a
// subsystem whose first one is still in flight — the count, not only the
// order — and once the kernel lets go, the next pass's own enumeration finds
// both gone.
func TestAStuckDisconnectDoesNotHoldTheCntlrRead(t *testing.T) {
	srv, node := newTestServer(t)
	withPassClock(srv, node)
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{revision: 2})
	nqns := []string{legNqn(srv, testMetaLeg), legNqn(srv, testDataLeg)}
	const disconnect = "cmd nvme disconnect --nqn "
	node.blockCmd(disconnect)
	t.Cleanup(func() { node.releaseCmd(disconnect) })
	disconnectsOf := func(nqn string) int {
		return len(node.callsMatching(disconnect + nqn))
	}
	noSide := cntlrReq(reqOpts{
		revision: 3, level: pb.SpLevel_SP_LEVEL_NO_SIDE})
	// A syncup is run off the test goroutine, so a pass that waits for the
	// parked child fails the test instead of hanging it.
	syncup := func(label string) *pb.SyncupCntlrReply {
		t.Helper()
		type result struct {
			reply *pb.SyncupCntlrReply
			err   error
		}
		done := make(chan result, 1)
		go func() {
			reply, err := srv.SyncupCntlr(ctx, noSide)
			done <- result{reply, err}
		}()
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatalf("%s: %v", label, got.err)
			}
			return got.reply
		case <-time.After(2 * time.Second):
			t.Fatalf("%s waited for a stuck nvme disconnect", label)
		}
		return nil
	}
	// The round's read holds the object lock now (dnagent.md SH29), so it is
	// the read that must get through, and the round after it answers what
	// it read.
	check := func(label string) *pb.CheckCntlrReply {
		t.Helper()
		done := make(chan *pb.CheckCntlrReply, 1)
		go func() {
			srv.refreshCntlr(ctx, cntlrKey(testCluster, testCn, testSp,
				testCntlr), srv.getCntlr(cntlrKey(testCluster, testCn,
				testSp, testCntlr)))
			reply, _ := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
				ClusterId: testCluster, CnId: testCn,
				CntlrPointer: cntlrPtr(), Revision: 3}, nil)
			done <- reply
		}()
		select {
		case reply := <-done:
			return reply
		case <-time.After(2 * time.Second):
			t.Fatalf("%s blocked behind a stuck nvme disconnect", label)
		}
		return nil
	}
	leftovers := func(reply *pb.AgentReply, label string) {
		t.Helper()
		cnSweepAssertCode(t, reply, common.ReplyCodeLeftover, label)
		for _, nqn := range nqns {
			cnSweepAssertDetails(t, reply, nqn, label)
		}
	}

	node.Reset()
	first := make(chan *pb.SyncupCntlrReply, 1)
	go func() {
		reply, _ := srv.SyncupCntlr(ctx, noSide)
		first <- reply
	}()
	waitFor(t, func() bool { return node.hasCall(disconnect) })
	leftovers(check("a CheckCntlr round").GetAgentReply(),
		"the Check round beside a stuck disconnect")
	select {
	case reply := <-first:
		leftovers(reply.GetAgentReply(), "the pass that disconnects")
	case <-time.After(2 * time.Second):
		t.Fatalf("the SyncupCntlr pass waited for a stuck nvme disconnect")
	}
	// Both legs' deletes are in the kernel at once, and the wrappers went in
	// the same pass: they are other objects, and nothing holds them.
	waitFor(t, func() bool { return len(node.callsMatching(disconnect)) == 2 })
	for _, legId := range []uint64{testMetaLeg, testDataLeg} {
		if _, ok := node.dms[legName(srv, legId)]; ok {
			t.Fatalf("leg %#x's wrapper waited for its connection", legId)
		}
	}

	// The worker's re-sync while both are still in the kernel: the
	// subsystems are leftovers because the node still holds their
	// controllers, and neither is disconnected a second time.
	leftovers(syncup("the re-sync").GetAgentReply(),
		"the re-sync beside a stuck disconnect")
	for _, nqn := range nqns {
		if got := disconnectsOf(nqn); got != 1 {
			t.Fatalf("%d disconnects of %s while one was in flight",
				got, nqn)
		}
	}

	// The kernel lets go. The next pass knows only what the node holds.
	node.releaseCmd(disconnect)
	waitFor(t, func() bool {
		node.mu.Lock()
		defer node.mu.Unlock()
		return len(node.subsystems) == 0
	})
	cnSweepAssertCode(t, check("the Check round after").GetAgentReply(), 0,
		"the Check round once the kernel let go")
	cnSweepAssertCode(t, syncup("the last re-sync").GetAgentReply(), 0,
		"the re-sync once the kernel let go")
	for _, nqn := range nqns {
		if got := disconnectsOf(nqn); got != 1 {
			t.Fatalf("%s was disconnected %d times", nqn, got)
		}
	}
}

// TestAFailedDisconnectIsIssuedAgain pins the other half of CN10's disconnect
// registry: it is bookkeeping of work in progress, never memory of work that
// failed. A background disconnect that fails leaves the registry with its
// goroutine; the connection stays a leftover only because the node still
// holds its controller, and the next pass issues the disconnect again.
func TestAFailedDisconnectIsIssuedAgain(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2})
	nqn := legNqn(srv, testMetaLeg)
	disconnect := "cmd nvme disconnect --nqn " + nqn
	node.failCmd[disconnect] = "Failed to disconnect"
	pass := func(label string) *pb.AgentReply {
		t.Helper()
		reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(
			reqOpts{revision: 3, level: pb.SpLevel_SP_LEVEL_NO_SIDE}))
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		awaitDisconnects(t, srv)
		return reply.GetAgentReply()
	}

	node.Reset()
	cnSweepOnlyDisconnects(t, pass("the pass that disconnects"),
		"the pass that disconnects")
	if _, ok := node.subsystems[nqn]; !ok {
		t.Fatalf("the failed disconnect took the connection anyway")
	}
	// The worker's re-sync: the node still holds the controller, and nothing
	// remembers that the first attempt failed.
	reply := pass("the re-sync")
	cnSweepOnlyDisconnects(t, reply, "the re-sync")
	cnSweepAssertDetails(t, reply, nqn, "the re-sync")
	if got := len(node.callsMatching(disconnect)); got != 2 {
		t.Fatalf("%d disconnects of %s, want the failed one and one more",
			got, nqn)
	}
	cnSweepAssertCode(t, pass("the last re-sync"), 0, "the last re-sync")
	if got := len(node.callsMatching(disconnect)); got != 2 {
		t.Fatalf("%s was disconnected %d times", nqn, got)
	}
}

// TestAStuckDisconnectDoesNotHoldTheNodeLock is the node-level twin of
// TestAStuckDisconnectDoesNotHoldTheCntlrRead: a SyncupCn that drops the
// cntlr's pointer — the pool drain's shape, under the node WRITE lock, whose
// stall would hold every object of the node — with both leg disconnects
// parked. The pass must reply beside the parked children, the CN's read and
// a CheckCn round must get through, and once the kernel lets go the re-sync
// is clean with each leg disconnected exactly once.
func TestAStuckDisconnectDoesNotHoldTheNodeLock(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2})
	nqns := []string{legNqn(srv, testMetaLeg), legNqn(srv, testDataLeg)}
	const disconnect = "cmd nvme disconnect --nqn "
	node.blockCmd(disconnect)
	t.Cleanup(func() { node.releaseCmd(disconnect) })
	done := make(chan *pb.SyncupCnReply, 1)
	go func() {
		reply, _ := srv.SyncupCn(context.Background(), cnReq(3, false))
		done <- reply
	}()
	select {
	case reply := <-done:
		cnSweepOnlyDisconnects(t, reply.GetAgentReply(), "the drop")
		for _, nqn := range nqns {
			cnSweepAssertDetails(t, reply.GetAgentReply(), nqn, "the drop")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("SyncupCn held the node write lock across a stuck " +
			"nvme disconnect")
	}
	// The CN's read, which the round starts, holds the node read lock.
	checked := make(chan *pb.CheckCnReply, 1)
	go func() {
		key := cnKey(testCluster, testCn)
		srv.refreshCn(context.Background(), key, srv.getCn(key))
		reply, _ := srv.checkCnRound(context.Background(), &pb.CheckCnRequest{
			ClusterId: testCluster, CnId: testCn, Revision: 3}, nil)
		checked <- reply
	}()
	select {
	case reply := <-checked:
		cnSweepOnlyDisconnects(t, reply.GetAgentReply(),
			"CheckCn beside a stuck disconnect")
	case <-time.After(2 * time.Second):
		t.Fatalf("a CheckCn round blocked behind a stuck nvme disconnect")
	}
	node.releaseCmd(disconnect)
	awaitDisconnects(t, srv)
	reply, err := srv.SyncupCn(context.Background(), cnReq(3, false))
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	cnSweepAssertCode(t, reply.GetAgentReply(), 0,
		"the re-sync once the kernel let go")
	for _, nqn := range nqns {
		if n := len(node.callsMatching(disconnect + nqn)); n != 1 {
			t.Fatalf("%s disconnected %d times", nqn, n)
		}
	}
}

// TestADisconnectOutlivesTheRpcCtx pins the background disconnect's ctx:
// rootCtx, never the pass's, which gRPC cancels the moment the handler
// returns. On the pass's ctx a disconnect the reply outran would be refused
// at its start or signalled before its write, issued again by the next pass
// and killed again the same way, so the teardown need never complete. The
// disconnect is held on the fake's ctx-honouring gate — unlike blockCmd, it
// returns as soon as its ctx is cancelled, as a child the signals can still
// end does.
func TestADisconnectOutlivesTheRpcCtx(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2})
	nqn := legNqn(srv, testMetaLeg)
	disconnect := "cmd nvme disconnect --nqn " + nqn
	gate := make(chan struct{})
	node.mu.Lock()
	node.gate[disconnect] = gate
	node.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 3, level: pb.SpLevel_SP_LEVEL_NO_SIDE})); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	waitFor(t, func() bool { return node.hasCall(disconnect) })
	cancel() // what gRPC does when the handler returns
	close(gate)
	awaitDisconnects(t, srv)
	node.mu.Lock()
	_, still := node.subsystems[nqn]
	node.mu.Unlock()
	if still {
		t.Fatalf("the background disconnect died with the RPC's ctx")
	}
}

// TestARecreatedCloneNeverAdoptsADyingSource pins the connect steps' half of
// CN10's disconnect registry. The disconnect a sweep sets going can complete
// after the pass's locks are released, so no lock keeps a later converge off
// the connection it is deleting: a clone re-created on the same source while
// the previous clone's disconnect is still in the kernel finds a controller at
// every wanted address, connects nothing, and builds its dm-clone on a
// namespace device about to vanish — every read of a region not yet hydrated
// then fails until a later converge reloads the table. While that disconnect
// runs, the step must neither adopt nor connect; the connect retry it
// registers connects afresh once the disconnect has returned — exactly once.
func TestARecreatedCloneNeverAdoptsADyingSource(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = 20 * time.Millisecond
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	oldSrc := connectedNsDevNo(t, srv, node, testSrcNqn)
	disconnect := "cmd nvme disconnect --nqn " + testSrcNqn
	node.blockCmd(disconnect)
	t.Cleanup(func() { node.releaseCmd(disconnect) })
	node.Reset()
	// DeleteClone: the sweep sets the source's disconnect going, and the
	// delete sits in the kernel.
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 3, primary: true})); err != nil {
		t.Fatalf("delete clone: %v", err)
	}
	waitFor(t, func() bool { return node.hasCall(disconnect) })
	// A new clone of the same source while that delete is in flight.
	second := cloneOfTd(testClone+1, testSrcNqn, testTd)
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 4, primary: true,
		clones: []*pb.Clone{second}})); err != nil {
		t.Fatalf("create clone: %v", err)
	}
	node.mu.Lock()
	_, built := node.dms[cloneName(srv, testClone+1)]
	node.mu.Unlock()
	if built {
		t.Fatalf("the new clone was built on a connection being deleted")
	}
	if n := connectsTo(node, testSrcNqn, testIp2, testSvcId2); n != 0 {
		t.Fatalf("%d connects of a source whose disconnect is in flight", n)
	}
	node.releaseCmd(disconnect)
	waitFor(t, func() bool {
		node.mu.Lock()
		defer node.mu.Unlock()
		dm := node.dms[cloneName(srv, testClone+1)]
		_, connected := node.subsystems[testSrcNqn]
		return dm != nil && connected &&
			!strings.Contains(dm.table, " "+oldSrc+" ")
	})
	if n := connectsTo(node, testSrcNqn, testIp2, testSvcId2); n != 1 {
		t.Fatalf("%d connects of the source once its disconnect returned, "+
			"want 1", n)
	}
}

// TestARestoredLegNeverAdoptsADyingConnection is the leg twin of
// TestARecreatedCloneNeverAdoptsADyingSource: SP_LEVEL_NO_SIDE sets both
// legs' disconnects going, and a level lowered back while they are still in
// the kernel wants the legs again. Adopting the controllers it finds would
// build the wrappers on multipath devices about to vanish, so each leg fails
// for the pass — no connect, no wrapper, an ERROR row naming the disconnect —
// and the connect retry builds both over fresh connections once the
// disconnects have returned, each leg connected exactly once.
func TestARestoredLegNeverAdoptsADyingConnection(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = 20 * time.Millisecond
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{revision: 2})
	legIds := []uint64{testMetaLeg, testDataLeg}
	oldDev := make(map[uint64]string, len(legIds))
	for _, legId := range legIds {
		oldDev[legId] = connectedNsDevNo(t, srv, node, legNqn(srv, legId))
	}
	const disconnect = "cmd nvme disconnect --nqn "
	node.blockCmd(disconnect)
	t.Cleanup(func() { node.releaseCmd(disconnect) })
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 3, level: pb.SpLevel_SP_LEVEL_NO_SIDE})); err != nil {
		t.Fatalf("NO_SIDE: %v", err)
	}
	waitFor(t, func() bool { return len(node.callsMatching(disconnect)) == 2 })
	connectsOf := func(legId uint64) int {
		n := 0
		for _, call := range node.callsMatching("cmd nvme connect ") {
			if strings.Contains(call, "--nqn "+legNqn(srv, legId)+" ") {
				n++
			}
		}
		return n
	}

	node.Reset()
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{revision: 4}))
	if err != nil {
		t.Fatalf("the level lowered back: %v", err)
	}
	for _, legId := range legIds {
		row := reply.GetCntlrInfo().GetLegIdToLeg()[legId]
		if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
			!strings.Contains(row.GetDetails(), "still in flight") {
			t.Fatalf("leg %#x reports %v/%q beside its own disconnect",
				legId, row.GetStatus(), row.GetDetails())
		}
		node.mu.Lock()
		_, built := node.dms[legName(srv, legId)]
		node.mu.Unlock()
		if built {
			t.Fatalf("leg %#x's wrapper was built on a connection being "+
				"deleted", legId)
		}
		if n := connectsOf(legId); n != 0 {
			t.Fatalf("%d connects of leg %#x while its disconnect is in "+
				"flight", n, legId)
		}
	}
	// The read answers the registry gate the way the converge does, and the
	// Check round after it carries what it read (dnagent.md SH29): the same
	// row, not one judged from the half-torn-down leg it finds — the two
	// channels would otherwise flip it, and its epoch, every round.
	refreshTestCntlr(t, srv)
	_, probed := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
		ClusterId: testCluster, CnId: testCn,
		CntlrPointer: cntlrPtr(), Revision: 4,
	}, nil)
	for _, legId := range legIds {
		converged := reply.GetCntlrInfo().GetLegIdToLeg()[legId]
		row := probed.GetLegIdToLeg()[legId]
		if row.GetStatus() != converged.GetStatus() ||
			row.GetDetails() != converged.GetDetails() {
			t.Fatalf("the Check round reports leg %#x %v/%q beside its "+
				"disconnect, the converge %v/%q", legId, row.GetStatus(),
				row.GetDetails(), converged.GetStatus(),
				converged.GetDetails())
		}
	}

	node.releaseCmd(disconnect)
	waitFor(t, func() bool {
		node.mu.Lock()
		defer node.mu.Unlock()
		for _, legId := range legIds {
			dm := node.dms[legName(srv, legId)]
			_, connected := node.subsystems[legNqn(srv, legId)]
			if dm == nil || !connected ||
				strings.Contains(dm.table, " "+oldDev[legId]+" ") {
				return false
			}
		}
		return true
	})
	for _, legId := range legIds {
		if n := connectsOf(legId); n != 1 {
			t.Fatalf("%d connects of leg %#x once its disconnect returned, "+
				"want 1", n, legId)
		}
	}
}

// TestTheCntlrReadMirrorsTheDisconnectGate pins the read-only half of
// ensureCloneSource's registry gate. A converge whose clone source is still
// being disconnected stops at CN18 step 1 and reports the dm-clone MISSING
// "source not connected", so the cntlr's read, which the Check round
// answers from (dnagent.md SH29), must read the source as not ready too,
// however connected it still looks, and name it ahead of the clone's step
// 2: the two channels would flip the row, and its epoch, against each
// other every round. The clone meta group is made not ready after the
// converge, so a read that judged step 2 first would name the group.
func TestTheCntlrReadMirrorsTheDisconnectGate(t *testing.T) {
	srv, node := newTestServer(t)
	// The failed step registers the connect retry, which the test runs by
	// hand only.
	srv.retryInterval = time.Hour
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	disconnect := "cmd nvme disconnect --nqn " + testSrcNqn
	node.blockCmd(disconnect)
	t.Cleanup(func() { node.releaseCmd(disconnect) })
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 3, primary: true})); err != nil {
		t.Fatalf("delete clone: %v", err)
	}
	waitFor(t, func() bool { return node.hasCall(disconnect) })
	second := cloneOfTd(testClone+1, testSrcNqn, testTd)
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 4, primary: true, clones: []*pb.Clone{second}}))
	if err != nil {
		t.Fatalf("create clone: %v", err)
	}
	converged := reply.GetCntlrInfo().GetCloneIdToDmClone()[testClone+1]
	if converged.GetStatus() != pb.ResStatus_RES_STATUS_MISSING {
		t.Fatalf("the converge reports the dm-clone %v/%q, want MISSING at "+
			"step 1", converged.GetStatus(), converged.GetDetails())
	}

	grp := second.GetMetaGrp()
	if grp == nil {
		grp = cloneGrpOf(second.GetCloneId(), false)
	}
	cloneGrpNotReady(srv, node, grp.GetGrpId(),
		grp.GetLegList()[0].GetLegId())

	refreshTestCntlr(t, srv)
	_, probed := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
		ClusterId: testCluster, CnId: testCn,
		CntlrPointer: cntlrPtr(), Revision: 4,
	}, nil)
	dm := probed.GetCloneIdToDmClone()[testClone+1]
	if dm.GetStatus() != converged.GetStatus() ||
		dm.GetDetails() != "source not connected" {
		t.Fatalf("the Check round reports the dm-clone %v/%q while its "+
			"source disconnects, the converge %v/%q", dm.GetStatus(),
			dm.GetDetails(), converged.GetStatus(), converged.GetDetails())
	}
	// The target row too: the converge reports it ERROR naming the
	// disconnect, and the dying controller still reads live.
	convergedTgt := reply.GetCntlrInfo().GetCloneIdToTarget()[testClone+1]
	tgt := probed.GetCloneIdToTarget()[testClone+1]
	if tgt.GetStatus() != convergedTgt.GetStatus() ||
		tgt.GetDetails() != convergedTgt.GetDetails() {
		t.Fatalf("the Check round reports the clone target %v/%q while its "+
			"source disconnects, the converge %v/%q", tgt.GetStatus(),
			tgt.GetDetails(), convergedTgt.GetStatus(),
			convergedTgt.GetDetails())
	}
}

// TestABackgroundDisconnectCarriesThePassTraceId pins CN10's "carries the
// pass's trace id, or a fresh one when the pass had none" for a pass that
// has one: the disconnect is one command of that pass, and its `os command`
// record is how a stall is read back out of the log — a record of
// `nvme disconnect` whose previous record of the same trace id is the admin
// timeout older.
func TestABackgroundDisconnectCarriesThePassTraceId(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2})
	node.mu.Lock()
	node.traceOf = make(map[string]string)
	node.mu.Unlock()
	const tid = "a1b2c3d4e5f60718"
	ctx := common.WithTraceId(context.Background(), tid)
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 3, level: pb.SpLevel_SP_LEVEL_NO_SIDE})); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	awaitDisconnects(t, srv)
	for _, legId := range []uint64{testMetaLeg, testDataLeg} {
		line := "cmd nvme disconnect --nqn " + legNqn(srv, legId)
		node.mu.Lock()
		got, ok := node.traceOf[line]
		node.mu.Unlock()
		if !ok || got != tid {
			t.Fatalf("%s ran under trace id %q (seen %v), want the pass's %q",
				line, got, ok, tid)
		}
	}
}

// TestTheBackgroundDisconnectsRunAFewAtATime pins CN10's cap on the
// registry's disconnects. Off the
// pass, one L10 of many legs or one pool drain would set them all going at
// once, and each delete a vanished target stalls keeps an OsClient slot for
// the kernel's admin timeout — enough of them, and the node's converges and
// Check rounds are refused a slot and report ERROR rows on healthy objects.
// The fixture has two legs, so the cap is shrunk to one: the second
// disconnect must wait, registered, for the first to return — not be issued
// beside it, and not be issued twice by the re-sync meanwhile.
func TestTheBackgroundDisconnectsRunAFewAtATime(t *testing.T) {
	srv, node := newTestServer(t)
	if got := cap(srv.disconnectSlots); got != common.DefaultOsClientLimit/4 {
		t.Fatalf("%d background disconnects may run at once, want a "+
			"quarter of the node's %d OsClient slots", got,
			common.DefaultOsClientLimit)
	}
	srv.disconnectSlots = make(chan struct{}, 1)
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{revision: 2})
	nqns := []string{legNqn(srv, testMetaLeg), legNqn(srv, testDataLeg)}
	const disconnect = "cmd nvme disconnect --nqn "
	node.blockCmd(disconnect)
	t.Cleanup(func() { node.releaseCmd(disconnect) })
	noSide := cntlrReq(reqOpts{
		revision: 3, level: pb.SpLevel_SP_LEVEL_NO_SIDE})
	pass := func(label string) *pb.AgentReply {
		t.Helper()
		reply, err := srv.SyncupCntlr(ctx, noSide)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		return reply.GetAgentReply()
	}
	leftovers := func(reply *pb.AgentReply, label string) {
		t.Helper()
		cnSweepOnlyDisconnects(t, reply, label)
		for _, nqn := range nqns {
			cnSweepAssertDetails(t, reply, nqn, label)
		}
	}

	node.Reset()
	leftovers(pass("the pass that disconnects"), "the pass that disconnects")
	waitFor(t, func() bool { return node.hasCall(disconnect) })
	// The parked disconnect holds the one slot, so the other cannot have
	// got past it; both are registered all the same.
	if n := len(srv.disconnectSlots); n != 1 {
		t.Fatalf("%d slots held with a disconnect parked in the kernel, "+
			"want 1", n)
	}
	for _, nqn := range nqns {
		if !srv.disconnectInFlight(nqn) {
			t.Fatalf("%s left the registry while its disconnect waited", nqn)
		}
	}
	leftovers(pass("the re-sync"), "the re-sync")
	if n := len(node.callsMatching(disconnect)); n != 1 {
		t.Fatalf("%d disconnects issued with one slot, want 1", n)
	}

	node.releaseCmd(disconnect)
	awaitDisconnects(t, srv)
	for _, nqn := range nqns {
		if n := len(node.callsMatching(disconnect + nqn)); n != 1 {
			t.Fatalf("%s was disconnected %d times, want 1", nqn, n)
		}
	}
	if n := len(srv.disconnectSlots); n != 0 {
		t.Fatalf("%d slots still held once every disconnect returned", n)
	}
	cnSweepAssertCode(t, pass("the last re-sync"), 0, "the last re-sync")
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition never became true")
}

// ---------------------------------------------------------------------------
// Reconcile / restart (CN2)
// ---------------------------------------------------------------------------

// TestReconcileReapplies is the case-D shape: a fresh server over the same
// store re-converges every stored object and, on an already-converged node,
// issues no mutating command at all.
func TestReconcileReapplies(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	node.Reset()
	fresh := newCnServer(node)
	reconcileForTest(t, fresh)
	for _, call := range node.Mutations() {
		t.Fatalf("the reconcile of a converged node mutated: %q\nall:\n%s",
			call, strings.Join(node.Mutations(), "\n"))
	}
	// The revision survived, so a stale re-sync is still rejected.
	reply, err := fresh.SyncupCn(context.Background(), cnReq(1, true))
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != common.ReplyCodeStaleRevision {
		t.Fatalf("the revision did not survive the restart: %v",
			reply.GetAgentReply())
	}
}

// TestReconcileTearsDownOrphanCntlr: a cntlr file whose pointer left the
// stored SyncupCnRequest is torn down on start (CN2).
func TestReconcileTearsDownOrphanCntlr(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	// Rewrite the stored CN request without the pointer, the way a crash
	// between the pointer removal and the teardown leaves it.
	if err := node.writeProto(context.Background(),
		srv.nf.LocalCnPath(testCluster, testCn), cnReq(3, false)); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	node.Reset()
	fresh := newCnServer(node)
	reconcileForTest(t, fresh)
	for name := range node.dms {
		t.Fatalf("dm device %s survived the orphan teardown", name)
	}
	path := srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr)
	if _, ok := node.protos[path]; ok {
		t.Fatalf("the orphan cntlr file was not deleted")
	}
}

// TestReconcileDropsOrphanChunks: a clone-bm file whose clone is not in the
// stored request is removed on start.
func TestReconcileDropsOrphanChunks(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	pushBitmap(t, srv, hexBytes(t, testSkipHex))
	bmPath := srv.nf.LocalCloneBmPath(
		testCluster, testCn, testSp, testClone, 0, 0)

	// The clone is deleted while the agent is down: rewrite the stored
	// request without it.
	if err := node.writeProto(context.Background(),
		srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr),
		cntlrReq(reqOpts{revision: 3, primary: true})); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	fresh := newCnServer(node)
	reconcileForTest(t, fresh)
	if _, ok := node.protos[bmPath]; ok {
		t.Fatalf("the orphan chunk file was not removed")
	}
}

// TestReconcileNeverLoadsAnInterruptedWrite: at a restart the committed cn
// and cntlr files are the only truth (dnagent.md SH6). A save whose process
// died between writing its temp file and renaming it leaves
// `{name}.tmp-{random}` behind, which holds an OLDER request once later saves
// have renamed newer ones over the name, and it sorts right after the
// committed file. Loaded, the older cn request here, which lacks the cntlr's
// pointer, would drop the cntlr — its file deleted, its devices swept — and
// the older cntlr request would replace the newer one. Neither temp file is
// read, both go in one rm, and that rm is the only change the restart makes.
func TestReconcileNeverLoadsAnInterruptedWrite(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	ctx := context.Background()
	cnPath := srv.nf.LocalCnPath(testCluster, testCn)
	cntlrPath := srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr)
	cnStray := cnPath + common.AtomicWriteTmpInfix + "1234567890"
	cntlrStray := cntlrPath + common.AtomicWriteTmpInfix + "987654321"
	if err := node.writeProto(ctx, cnStray, cnReq(1, false)); err != nil {
		t.Fatalf("planting the cn temp file: %v", err)
	}
	if err := node.writeProto(ctx, cntlrStray,
		cntlrReq(reqOpts{revision: 1})); err != nil {
		t.Fatalf("planting the cntlr temp file: %v", err)
	}
	node.mu.Lock()
	committed := map[string]string{
		cnPath:    string(node.protos[cnPath]),
		cntlrPath: string(node.protos[cntlrPath]),
	}
	node.mu.Unlock()
	node.Reset()

	fresh := newCnServer(node)
	reconcileForTest(t, fresh)

	// The rounds answer from the observations the reconcile stored for the
	// requests it loaded (dnagent.md SH28).
	cnReply, _ := fresh.checkCnRound(ctx, &pb.CheckCnRequest{
		ClusterId: testCluster, CnId: testCn, Revision: 2,
	}, nil)
	if got := cnReply.GetRevision(); got != 2 {
		t.Errorf("the cn came back at revision %d, want the committed 2",
			got)
	}
	cntlrReply, _ := fresh.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr(),
		Revision: 2,
	}, nil)
	if got := cntlrReply.GetAgentReply(); got.GetCode() != 0 ||
		cntlrReply.GetRevision() != 2 {
		t.Errorf("CheckCntlr after the restart = %v at revision %d, want "+
			"0 at the committed 2", got, cntlrReply.GetRevision())
	}
	node.mu.Lock()
	after := make(map[string]string, len(node.protos))
	for path, raw := range node.protos {
		after[path] = string(raw)
	}
	node.mu.Unlock()
	for path, raw := range committed {
		if got, ok := after[path]; !ok {
			t.Errorf("%s was deleted", path)
		} else if got != raw {
			t.Errorf("%s was rewritten", path)
		}
	}
	for _, stray := range []string{cnStray, cntlrStray} {
		if node.hasCall("readproto " + stray) {
			t.Errorf("%s was read", stray)
		}
		if _, ok := after[stray]; ok {
			t.Errorf("%s survived the restart", stray)
		}
	}
	mutations := node.Mutations()
	if len(mutations) != 1 ||
		!strings.HasPrefix(mutations[0], "cmd rm -f ") ||
		!strings.Contains(mutations[0], " "+cnStray) ||
		!strings.Contains(mutations[0], " "+cntlrStray) {
		t.Errorf("the restart changed the node by\n%s\nwant exactly one "+
			"rm naming both temp files", strings.Join(mutations, "\n"))
	}
}

// TestAConvergeDoesNotRaceAnotherCntlrsReads pins the request pointers
// against the reads other goroutines make of them. A SyncupCntlr stores the
// cntlr's new request under the node read lock and that cntlr's object lock,
// and the claim reads of every other pass read it holding neither that
// object lock nor the node write lock: another cntlr's converge or read
// attributes each host-facing subsystem by asking every cntlr of the CN
// whether its request still names it, and the node-level verdict of a CN's
// read reads every cntlr's clone list too. Run
// beside one another they must not race, which is what the race detector
// checks here; without it the test proves nothing. The detector reports a
// pair only when the two accesses happen to interleave between the locks
// both sides take, so one converge rarely shows it: three hundred beside
// four reader loops is enough to report an unguarded pointer on every run.
func TestAConvergeDoesNotRaceAnotherCntlrsReads(t *testing.T) {
	if !raceEnabled {
		t.Skip("proves nothing without -race")
	}
	srv, _ := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.SyncupCn(ctx, cnReq2(2)); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	for _, req := range []*pb.SyncupCntlrRequest{
		cntlrReq(reqOpts{revision: 2}), cntlrReq2(2),
	} {
		reply, err := srv.SyncupCntlr(ctx, req)
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		if reply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("SyncupCntlr rejected: %v", reply.GetAgentReply())
		}
	}

	const rounds = 300
	done := make(chan struct{})
	writer := make(chan struct{})
	go func() {
		defer close(writer)
		for i := uint64(0); i < rounds; i++ {
			if _, err := srv.SyncupCntlr(ctx,
				cntlrReq(reqOpts{revision: 3 + i})); err != nil {
				t.Errorf("SyncupCntlr: %v", err)
				return
			}
		}
	}()
	// The claim reads are made by the reads a round starts (dnagent.md
	// SH29), so the rounds here start theirs in the background, beside the
	// syncups that mark and cancel them, and the readers also run one in
	// the foreground before each round: a round starts none while one runs
	// or a converge of its object is marked. The test joins the background
	// reads before it returns.
	srv.roundReads = true
	defer srv.reads.Wait()
	key2 := cntlrKey(testCluster, testCn, testSp2, testCntlr2)
	cn := cnKey(testCluster, testCn)
	readers := []func(){
		func() {
			srv.refreshCntlr(ctx, key2, srv.getCntlr(key2))
			srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
				ClusterId: testCluster, CnId: testCn,
				CntlrPointer: cntlrPtr2(), Revision: 2,
			}, nil)
		},
		func() {
			srv.refreshCn(ctx, cn, srv.getCn(cn))
			srv.checkCnRound(ctx, &pb.CheckCnRequest{
				ClusterId: testCluster, CnId: testCn, Revision: 2,
			}, nil)
		},
		func() {
			if _, err := srv.GetCnInfo(ctx, &pb.GetCnInfoRequest{
				ClusterId: testCluster, CnId: testCn,
			}); err != nil {
				t.Errorf("GetCnInfo: %v", err)
			}
		},
		func() {
			if _, err := srv.SyncupCntlr(ctx, cntlrReq2(2)); err != nil {
				t.Errorf("SyncupCntlr of the other cntlr: %v", err)
			}
		},
	}
	finished := make(chan struct{}, len(readers))
	for _, read := range readers {
		go func() {
			defer func() { finished <- struct{}{} }()
			for {
				select {
				case <-done:
					return
				default:
				}
				read()
			}
		}()
	}
	<-writer
	close(done)
	for range readers {
		<-finished
	}
}

// TestARefusedCallLeavesNoLockEntry: a call refused for a cntlr the agent
// does not hold creates no lock entry for it (dnagent.md, SH11): only the
// teardown of a held cntlr drops an entry, so one a refusal created would
// stay for the life of the process. Each call is checked twice: the set holds
// as many entries after it as before, and the call does not wait for an
// entry another call holds. The admitted SyncupCntlr at the end creates its
// cntlr's entry, which is what the count can see.
func TestARefusedCallLeavesNoLockEntry(t *testing.T) {
	ctx := context.Background()
	key := cntlrKey(testCluster, testCn, testSp, testCntlr)
	for _, tc := range []struct {
		name string
		// listed says whether the CN's list names the cntlr.
		listed bool
		call   func(srv *CnAgentServer) uint32
		want   uint32
		// entries is how many entries the call adds.
		entries int
	}{
		{"a SyncupCntlr for a pointer the cn does not list", false,
			func(srv *CnAgentServer) uint32 {
				reply, _ := srv.SyncupCntlr(ctx,
					cntlrReq(reqOpts{revision: 2, primary: true}))
				return reply.GetAgentReply().GetCode()
			}, common.ReplyCodeUnknownObject, 0},
		{"a SyncupCntlr whose conf CN8 refuses, for a cntlr never held",
			true, func(srv *CnAgentServer) uint32 {
				reply, _ := srv.SyncupCntlr(ctx, zeroConfReq(
					reqOpts{revision: 2, primary: true},
					func(conf *pb.BdevConf) {
						conf.DmPoolConf.DataBlockSize = 0
					}))
				return reply.GetAgentReply().GetCode()
			}, common.ReplyCodeInvalidConf, 0},
		{"a PushCloneBitmap for a cntlr never held", true,
			func(srv *CnAgentServer) uint32 {
				reply, _ := srv.PushCloneBitmap(ctx,
					&pb.PushCloneBitmapRequest{
						ClusterId: testCluster, CnId: testCn,
						CntlrPointer: cntlrPtr(), CloneId: testClone,
						Bitmap: []byte{0xff},
					})
				return reply.GetAgentReply().GetCode()
			}, common.ReplyCodeUnknownObject, 0},
		{"a GetCntlrInfo for a cntlr never held", true,
			func(srv *CnAgentServer) uint32 {
				reply, _ := srv.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
					ClusterId: testCluster, CnId: testCn,
					CntlrPointer: cntlrPtr(),
				})
				return reply.GetAgentReply().GetCode()
			}, common.ReplyCodeUnknownObject, 0},
		{"a CheckCntlr round for a cntlr never held", true,
			func(srv *CnAgentServer) uint32 {
				reply, _ := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
					ClusterId: testCluster, CnId: testCn,
					CntlrPointer: cntlrPtr(), Revision: 2, ShowInfo: true,
				}, nil)
				return reply.GetAgentReply().GetCode()
			}, common.ReplyCodeUnknownObject, 0},
		{"an admitted SyncupCntlr", true, func(srv *CnAgentServer) uint32 {
			reply, _ := srv.SyncupCntlr(ctx,
				cntlrReq(reqOpts{revision: 2, primary: false}))
			return reply.GetAgentReply().GetCode()
		}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newServer := func() *CnAgentServer {
				srv, _ := newTestServer(t)
				cnSyncup(t, srv, 1, tc.listed)
				return srv
			}

			srv := newServer()
			before := srv.locks.ObjCount()
			if code := tc.call(srv); code != tc.want {
				t.Fatalf("code %d, want %d", code, tc.want)
			}
			if got := srv.locks.ObjCount(); got != before+tc.entries {
				t.Errorf("%d lock entries after the call, want %d",
					got, before+tc.entries)
			}
			if tc.entries != 0 {
				return
			}

			srv = newServer()
			lock := srv.locks.Obj(key)
			lock.Lock()
			defer lock.Unlock()
			got := make(chan uint32, 1)
			go func() { got <- tc.call(srv) }()
			select {
			case code := <-got:
				if code != tc.want {
					t.Errorf("code %d, want %d", code, tc.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the refusal waited for the cntlr's object lock")
			}
		})
	}
}

// TestABackgroundAttemptOnADroppedCntlrLeavesNoLockEntry: a background
// attempt scheduled for a cntlr that a drop takes away before the attempt
// gets the node read lock — a connect retry racing the cntlr's pointer
// leaving its CN's list — creates no lock entry: the
// attempt checks the cntlr under the node read lock, which the drop's write
// lock excludes, before it takes the object lock (reconvergeCntlr).
func TestABackgroundAttemptOnADroppedCntlrLeavesNoLockEntry(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: false})
	key := cntlrKey(testCluster, testCn, testSp, testCntlr)
	st := srv.getCntlr(key)
	if st == nil {
		t.Fatal("the fixture converge left no cntlr state")
	}

	srv.locks.Node().Lock()
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		close(started)
		srv.reconvergeCntlr(ctx, key, st)
		close(done)
	}()
	<-started
	// The attempt reaches the node lock and waits behind the write lock.
	time.Sleep(50 * time.Millisecond)
	srv.dropCntlrState(ctx, key, st)
	before := srv.locks.ObjCount()
	node.Reset()
	srv.locks.Node().Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the attempt never returned")
	}
	if got := srv.locks.ObjCount(); got != before {
		t.Errorf("%d lock entries after the attempt, want %d", got, before)
	}
	if calls := node.Calls(); len(calls) != 0 {
		t.Errorf("the attempt on a dropped cntlr ran %q", calls)
	}
}
