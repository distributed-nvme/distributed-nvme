package cnagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// §6.13 — the CN1 lock mapping: a SyncupCntlr blocked in a slow command
// blocks a same-cntlr SyncupCntlr, but neither a CheckCn round (node read
// lock only) nor another cntlr's converge (a different object lock).

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

	// A node-scoped read does get through: it takes the node read lock only.
	done := make(chan struct{})
	go func() {
		defer close(done)
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

// TestCloneMetaAllocatorSerialized pins the one cn lock that IS held across OS
// calls (cloneMetaMu, [D14]). The clone-metadata registry is the kernel's dm
// table set, and SyncupCntlr holds only the node *read* lock, so two cntlrs of
// the same CN converge at once: without one critical section around
// enumerate → discard → create both passes read the same run as free and,
// because their wrappers have different names, neither `dmsetup create` fails
// — two dm-clones would silently share one metadata range.
func TestCloneMetaAllocatorSerialized(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	loop := loopDev(t, srv, node)

	// Two plans of the same CN, one per SP, each wanting its own slot.
	planA := newCntlrPlan(srv.nf, cntlrReq(reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}}))
	cpA := planA.cloneById[testClone]
	reqB := cntlrReq(reqOpts{revision: 2, primary: true,
		clones: []*pb.Clone{cloneOfTd(testClone2, testSrcNqn2, testTd)}})
	reqB.CntlrPointer = cntlrPtr2()
	planB := newCntlrPlan(srv.nf, reqB)
	cpB := planB.cloneById[testClone2]

	gate := make(chan struct{})
	node.gate["cmd dmsetup create "+cpA.metaDmName] = gate
	firstDone := make(chan error, 1)
	go func() { firstDone <- srv.ensureCloneMeta(ctx, planA, cpA) }()
	waitFor(t, func() bool {
		return node.hasCall("cmd dmsetup create " + cpA.metaDmName)
	})

	secondDone := make(chan error, 1)
	go func() { secondDone <- srv.ensureCloneMeta(ctx, planB, cpB) }()
	select {
	case <-secondDone:
		t.Fatalf("a second allocation ran inside the first's critical section")
	case <-time.After(50 * time.Millisecond):
	}
	// In particular it has not hole-punched the run the first one is taking:
	// that punch is what wipes a slot's previous contents.
	if punches := node.callsMatching(
		"cmd blkdiscard --offset 0 --length 8388608 " +
			loop); len(punches) != 1 {
		t.Fatalf("the free run was punched %d times: %v", len(punches), punches)
	}

	close(gate)
	if err := <-firstDone; err != nil {
		t.Fatalf("first allocation: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second allocation: %v", err)
	}
	// The second enumeration saw the first wrapper, so the runs are disjoint.
	if got, want := node.dms[cpB.metaDmName].table,
		wrapperTable(node, loop, 2); got != want {
		t.Fatalf("the second slot is %q, want %q", got, want)
	}
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
	pushBitmap(t, srv, 2, hexBytes(t, testSkipHex))
	bmPath := srv.nf.LocalCloneBmPath(
		testCluster, testCn, testSp, testClone, 0)

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
