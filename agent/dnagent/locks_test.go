package dnagent

import (
	"context"
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
