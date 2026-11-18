package cnagent

import (
	"context"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// A reserve_metadata_snap that did not answer — killed at the soft timeout,
// or its caller gone — may have run in the kernel, so the dump releases after
// it on a ctx the caller cannot cancel (CN25); held, the reservation would
// pin metadata blocks the auto-grow counts as used until the next reserve met
// EBUSY. A reserve the kernel refused for any cause but a snapshot already
// held holds nothing and gets no release.
func TestAReserveThatDidNotAnswerIsReleased(t *testing.T) {
	for _, tc := range []struct {
		name string
		// arm makes the reserve fail; cancel is the RPC's ctx's.
		arm     func(node *fakeNode, reserve string, cancel func())
		release bool
	}{
		{"killed after the kernel ran it", func(node *fakeNode,
			reserve string, _ func()) {
			node.killCmd[reserve] = true
		}, true},
		{"killed before it ran", func(node *fakeNode, reserve string,
			_ func()) {
			node.killCmdNoEffect[reserve] = true
		}, true},
		{"its caller gone while it ran", func(node *fakeNode,
			reserve string, cancel func()) {
			node.cancelCmd[reserve] = cancel
		}, true},
		{"refused by the kernel", func(node *fakeNode, reserve string,
			_ func()) {
			node.failCmd[reserve] = "device-mapper: message ioctl on " +
				"pool failed: Invalid argument"
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			scriptDump(srv, node, caseBDump)
			pool := poolName(srv)
			reserve := "dmsetup message " + pool + " 0 reserve_metadata_snap"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			node.mu.Lock()
			tc.arm(node, reserve, cancel)
			node.mu.Unlock()
			node.Reset()
			if _, err := srv.GetThinDeviceBm(ctx,
				&pb.GetThinDeviceBmRequest{
					ClusterId: testCluster, CnId: testCn, SpId: testSp,
					CntlrId: testCntlr, TdId: testTd, SliceIdx: 0,
				}); err == nil {
				t.Fatalf("GetThinDeviceBm over a failed reserve succeeded")
			}
			release := "cmd dmsetup message " + pool +
				" 0 release_metadata_snap"
			if got := node.hasCall(release); got != tc.release {
				t.Fatalf("release run: %v, want %v", got, tc.release)
			}
			node.mu.Lock()
			held := node.dms[pool].heldRoot
			node.mu.Unlock()
			if held {
				t.Fatalf("the pool still holds a metadata snapshot")
			}
		})
	}
}

// heldSnapshot reports whether the fixture pool holds a metadata snapshot.
func heldSnapshot(srv *CnAgentServer, node *fakeNode) bool {
	node.mu.Lock()
	defer node.mu.Unlock()
	return node.dms[poolName(srv)].heldRoot
}

// TestARestartReleasesAMetadataSnapshotNoProcessHolds: an agent that dies
// between a reserve_metadata_snap and its release — in an activation sweep,
// a sweep after a failed delete or a bitmap read — leaves the pool holding a
// metadata snapshot that pins its metadata blocks, and no reserve of a later
// process may ever come to meet it. Every reserve runs under the cntlr's
// object lock or inside its converge (CN1, CN14, CN25), so a converge that
// finds one held knows no live holder owns it: the restart's startup pass
// releases it, and changes nothing else on the node (CN2, CN13).
func TestARestartReleasesAMetadataSnapshotNoProcessHolds(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	node.holdMetaSnap(poolName(srv))

	node.Reset()
	reconcileForTest(t, newCnServer(node))
	if heldSnapshot(srv, node) {
		t.Fatalf("the restart left the pool holding a metadata snapshot")
	}
	release := "cmd dmsetup message " + poolName(srv) +
		" 0 release_metadata_snap"
	for _, call := range reconcileMutations(node.Mutations()) {
		if call != release {
			t.Errorf("the restart changed the node: %s", call)
		}
	}
}

// TestAReadNamesAHeldMetadataSnapshot: a read of a cntlr whose pool holds a
// metadata snapshot reports the pool row OK with RES_REASON_DIFFERS, its
// details still the raw status line, and its verdict names the row, so the
// worker sends the SyncupCntlr again (CN30); that converge releases the
// snapshot, and the next read is clean (CN13). A pool under needs_check
// writes no metadata, so its held snapshot is neither released nor named.
func TestAReadNamesAHeldMetadataSnapshot(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	node.holdMetaSnap(poolName(srv))

	verdict := checkCntlrVerdict(t, srv, 2)
	if verdict.GetCode() != common.ReplyCodeLeftover ||
		!strings.Contains(verdict.GetDetails(), poolName(srv)+": differs") {
		t.Fatalf("the read's verdict: %v, want the pool row named", verdict)
	}
	row := checkRound(t, srv, 2).GetSliceIdToDmPool()[testSlice]
	if row.GetStatus() != pb.ResStatus_RES_STATUS_OK ||
		row.GetReason() != pb.ResReason_RES_REASON_DIFFERS ||
		!strings.Contains(row.GetDetails(), "thin-pool") {
		t.Fatalf("the read's pool row: %v", row)
	}

	reply, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 2, primary: true}))
	if err != nil {
		t.Fatalf("the re-sent SyncupCntlr: %v", err)
	}
	if got := reply.GetAgentReply(); got.GetCode() != 0 {
		t.Fatalf("the re-sent SyncupCntlr answered %v, want code zero", got)
	}
	if heldSnapshot(srv, node) {
		t.Fatalf("the converge left the pool holding a metadata snapshot")
	}
	if verdict := checkCntlrVerdict(t, srv, 2); verdict.GetCode() != 0 {
		t.Fatalf("the read after the release: %v, want clean", verdict)
	}

	node.holdMetaSnap(poolName(srv))
	node.mu.Lock()
	node.dms[poolName(srv)].poolNeedsCheck = true
	node.mu.Unlock()
	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 2, primary: true})); err != nil {
		t.Fatalf("the SyncupCntlr under needs_check: %v", err)
	}
	assertNoCall(t, node, "release_metadata_snap")
	if row := checkRound(t, srv, 2).GetSliceIdToDmPool()[testSlice]; row.
		GetReason() != pb.ResReason_RES_REASON_POOL_NEEDS_CHECK {
		t.Fatalf("the read of a pool under needs_check: %v", row)
	}
}

// TestAReadOnlyPoolKeepsItsHeldMetadataSnapshot: a pool whose mode reads ro
// with no needs_check is out of metadata space, and dm-thin serves it no
// pool message, a release included, until the metadata auto-grow makes it
// writable again (dnv-worker.md AR6). Its held metadata snapshot is neither
// released nor named: a converge sends no release, its pool row and a
// read's read OK with no reason, and the read's verdict is clean (CN13).
// Naming it would bring a converge on every round whose release dm-thin
// refuses.
func TestAReadOnlyPoolKeepsItsHeldMetadataSnapshot(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	node.holdMetaSnap(poolName(srv))
	node.mu.Lock()
	node.dms[poolName(srv)].poolMode = "ro"
	node.mu.Unlock()

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 2, primary: true}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	assertNoCall(t, node, "release_metadata_snap")
	assertCodeZero(t, "the converge over a read-only pool", reply)
	for what, row := range map[string]*pb.ResInfo{
		"the converge's": reply.GetCntlrInfo().GetSliceIdToDmPool()[testSlice],
		"the read's":     checkRound(t, srv, 2).GetSliceIdToDmPool()[testSlice],
	} {
		if row.GetStatus() != pb.ResStatus_RES_STATUS_OK ||
			row.GetReason() != pb.ResReason_RES_REASON_NONE {
			t.Errorf("%s pool row: %v, want OK with no reason", what, row)
		}
	}
	if verdict := checkCntlrVerdict(t, srv, 2); verdict.GetCode() != 0 {
		t.Errorf("the read's verdict: %v, want clean", verdict)
	}
	if !heldSnapshot(srv, node) {
		t.Errorf("the read-only pool's snapshot is gone")
	}
}

// TestAPoolCreatedOverAHeldMetadataSnapshotReleasesIt: dm-thin keeps a held
// metadata snapshot in the pool's metadata, so a pool created again over it
// — on the primary a failover promotes, or after a reboot — comes up
// holding it. The converge that creates the pool releases it right after the
// create, so its pool row, its code and the read after it are clean, and no
// re-send comes for a snapshot already gone (CN13).
func TestAPoolCreatedOverAHeldMetadataSnapshotReleasesIt(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	if reply, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 3})); err != nil ||
		reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("demote: %v %v", reply.GetAgentReply(), err)
	}
	if dmPresent(node, poolName(srv)) {
		t.Fatalf("fixture: the demoted cntlr kept its pool")
	}
	node.holdMetaSnap(poolName(srv))

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 4, primary: true}))
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	assertCodeZero(t, "the converge that creates the pool", reply)
	if row := reply.GetCntlrInfo().GetSliceIdToDmPool()[testSlice]; row.
		GetReason() != pb.ResReason_RES_REASON_NONE {
		t.Errorf("the converge's pool row: %v, want no reason", row)
	}
	if heldSnapshot(srv, node) {
		t.Fatalf("the created pool still holds a metadata snapshot")
	}
	assertOrder(t, node, "cmd dmsetup create "+poolName(srv),
		"cmd dmsetup message "+poolName(srv)+" 0 release_metadata_snap")
	if verdict := checkCntlrVerdict(t, srv, 4); verdict.GetCode() != 0 {
		t.Errorf("the read after: %v, want clean", verdict)
	}
}
