package cnagent

import (
	"context"
	"testing"

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
