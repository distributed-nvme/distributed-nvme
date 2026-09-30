package dnagent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// 10. Lock mapping smoke (DN1): a SyncupSide stuck in a slow command holds
// the node read lock and its own object lock — so node-scoped reads and other
// sides run on, and only the same side waits.
func TestLockMapping(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()

	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide, testSide2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}

	gate := make(chan struct{})
	slow := "cmd dmsetup create " +
		nf.DnSideName(testCluster, testDn, testSp, testSide)
	node.mu.Lock()
	node.gate[slow] = gate
	node.mu.Unlock()

	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		_, _ = srv.SyncupSide(ctx, unprovisionedSideReq(1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE))
	}()
	if !waitFor(t, 2*time.Second, func() bool { return node.hasCall(slow) }) {
		t.Fatal("the slow command never started")
	}

	// A node-scoped CheckDn round only needs the node read lock.
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.checkDnRound(ctx, &pb.CheckDnRequest{
			ClusterId: testCluster, DnId: testDn, ShowInfo: true,
		}, nil)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a CheckDn round was blocked by a busy side")
	}

	// A different side takes a different object lock.
	otherDone := make(chan struct{})
	go func() {
		defer close(otherDone)
		_, _ = srv.SyncupSide(ctx, unprovisionedSideReq(1, testSide2, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE))
	}()
	select {
	case <-otherDone:
	case <-time.After(2 * time.Second):
		t.Fatal("a SyncupSide for another side was blocked")
	}

	// The same side must wait.
	sameDone := make(chan struct{})
	go func() {
		defer close(sameDone)
		_, _ = srv.SyncupSide(ctx, unprovisionedSideReq(1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE))
	}()
	select {
	case <-sameDone:
		t.Fatal("a second SyncupSide for the same side was not serialized")
	case <-time.After(150 * time.Millisecond):
	}

	close(gate)
	for _, ch := range []chan struct{}{blocked, sameDone} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("a SyncupSide never finished after the gate opened")
		}
	}
}

// SyncupDn takes the node write lock, so it waits for a busy side (SH10).
func TestSyncupDnWaitsForBusySide(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()

	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	gate := make(chan struct{})
	slow := "cmd dmsetup create " +
		nf.DnSideName(testCluster, testDn, testSp, testSide)
	node.mu.Lock()
	node.gate[slow] = gate
	node.mu.Unlock()

	sideDone := make(chan struct{})
	go func() {
		defer close(sideDone)
		_, _ = srv.SyncupSide(ctx, unprovisionedSideReq(1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE))
	}()
	if !waitFor(t, 2*time.Second, func() bool { return node.hasCall(slow) }) {
		t.Fatal("the slow command never started")
	}

	dnDone := make(chan struct{})
	go func() {
		defer close(dnDone)
		_, _ = srv.SyncupDn(ctx, dnReq(2, testSide))
	}()
	select {
	case <-dnDone:
		t.Fatal("SyncupDn ran while a side was converging")
	case <-time.After(150 * time.Millisecond):
	}
	close(gate)
	for _, ch := range []chan struct{}{sideDone, dnDone} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("a call never finished after the gate opened")
		}
	}
}

// TestAnotherSidesPassReadsARequestBeingReplaced: a SyncupSide replaces its
// side's request under that side's object lock, and other passes read it
// under locks that do not order them with that one: another side's CheckSide
// round builds its claims from every side's request under the node read lock
// and its own object lock, and a CheckDn round does the same under the node
// read lock alone. The replacement must therefore be safe on its own; under
// the race detector, a plain assignment fails this test with a DATA RACE on
// the request field.
func TestAnotherSidesPassReadsARequestBeingReplaced(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()
	// The second side sits in another leg: two sides of one leg on one DN
	// would share the leg's per-CN exports.
	side2Ptr := &pb.SidePointer{
		SpId: testSp, LegId: testLeg + 1, SideId: testSide2}
	dn := dnReq(1, testSide)
	dn.SidePointerList = append(dn.SidePointerList, side2Ptr)
	if _, err := srv.SyncupDn(ctx, dn); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	syncupSideTwoPhase(t, srv, sideReq(1, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	side2 := sideReq(1, testSide2, testCn0, []uint64{testCn1},
		pb.SpLevel_SP_LEVEL_READWRITE)
	side2.SidePointer = side2Ptr
	syncupSideTwoPhase(t, srv, side2)

	const rounds = 300
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for revision := uint64(2); revision < 2+rounds; revision++ {
			reply, err := srv.SyncupSide(ctx, sideReq(revision, testSide,
				testCn0, []uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
			if err != nil || reply.GetAgentReply().GetCode() != 0 {
				t.Errorf("SyncupSide at revision %d: %v %v", revision,
					reply.GetAgentReply(), err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			srv.checkSideRound(ctx, &pb.CheckSideRequest{
				ClusterId: testCluster, DnId: testDn,
				SidePointer: side2Ptr, Revision: 1,
			}, nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			srv.checkDnRound(ctx, &pb.CheckDnRequest{
				ClusterId: testCluster, DnId: testDn, Revision: 1,
			}, nil)
		}
	}()
	wg.Wait()

	reply, _ := srv.checkSideRound(ctx, &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn,
		SidePointer: sidePtr(testSide), Revision: 1 + rounds,
	}, nil)
	if got := reply.GetRevision(); got != 1+rounds {
		t.Errorf("the replaced side reports revision %d, want %d",
			got, 1+rounds)
	}
}
