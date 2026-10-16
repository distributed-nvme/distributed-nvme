package cnagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// When a cntlr's request is saved (CN8; dnagent.md SH5)
// ---------------------------------------------------------------------------

// newTdReq is the fixture cntlr's request with td A, created, and td B,
// which the pass that takes it creates: its thin id and its thin volume.
func newTdReq(revision uint64) *pb.SyncupCntlrRequest {
	return cntlrReq(reqOpts{revision: revision, primary: true,
		tds: []*pb.ThinDevice{
			createdTd(testTd, sweepLiveDevId),
			{TdId: testSnapTd, DevId: sweepLiveDevId2, Size: testTdSize},
		}})
}

// syncupTdA brings the fixture cntlr up as a primary whose pool holds td A
// alone, at revision 2.
func syncupTdA(t *testing.T, srv *CnAgentServer, node *fakeNode) {
	t.Helper()
	node.holdThinIds(poolName(srv), sweepLiveDevId)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		tds: []*pb.ThinDevice{createdTd(testTd, sweepLiveDevId)}})
}

// storedCntlrRevision is the revision of the fixture cntlr's request in the
// local store, read from the fake node itself.
func storedCntlrRevision(
	t *testing.T,
	srv *CnAgentServer,
	node *fakeNode,
) uint64 {
	t.Helper()
	path := srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr)
	node.mu.Lock()
	raw, ok := node.protos[path]
	node.mu.Unlock()
	if !ok {
		t.Fatalf("no cntlr file at %s", path)
	}
	req := &pb.SyncupCntlrRequest{}
	if err := proto.Unmarshal(raw, req); err != nil {
		t.Fatalf("the cntlr file does not decode: %v", err)
	}
	return req.GetRevision()
}

// thinIdHeld reports whether the pool's metadata holds devId, read from the
// fake node under its lock: the cut-off test reads it beside the first
// server's probers.
func thinIdHeld(node *fakeNode, pool string, devId uint32) bool {
	node.mu.Lock()
	defer node.mu.Unlock()
	return node.thinPools[pool][devId]
}

// cutOffSyncupCntlr is the cn twin of the dn package's cutOffSyncupSide: one
// SyncupCntlr whose call the worker cancels while its pass is held at the
// first command whose line contains key. The held command runs to its end
// once let go; every command of the pass after it is refused on the
// cancelled ctx, and so is a save placed after them. It returns once the
// call has.
func cutOffSyncupCntlr(
	t *testing.T,
	srv *CnAgentServer,
	node *fakeNode,
	req *pb.SyncupCntlrRequest,
	key string,
) {
	t.Helper()
	node.blockCmd(key)
	t.Cleanup(func() { node.releaseCmd(key) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = srv.SyncupCntlr(ctx, req)
	}()
	waitFor(t, func() bool { return node.hasCall("cmd " + key) })
	cancel()
	node.releaseCmd(key)
	<-done
	calls := node.Calls()
	for _, call := range calls[node.indexOfCall("cmd "+key)+1:] {
		if strings.HasPrefix(call, "cmd ") {
			t.Fatalf("fixture is wrong: the cut-off pass ran %q after %q",
				call, key)
		}
	}
}

// A cntlr's request is saved as soon as the gates admit it: its file is the
// call's first write, before the converge's first mutating OS call (CN8;
// dnagent.md SH5). A save that fails is only logged: the converge runs all
// the same, and the reply carries the request's revision (CN20), as after a
// save that held.
func TestACntlrRequestIsSavedBeforeItsConverge(t *testing.T) {
	ctx := context.Background()
	t.Run("the save holds", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupTdA(t, srv, node)
		path := srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr)
		node.Reset()
		if _, err := srv.SyncupCntlr(ctx, newTdReq(3)); err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		muts := node.Mutations()
		if len(muts) < 2 || muts[0] != "writeproto "+path {
			t.Fatalf("want the cntlr file written before the pass's first "+
				"mutation, got:\n%s", strings.Join(muts, "\n"))
		}
		if got := storedCntlrRevision(t, srv, node); got != 3 {
			t.Errorf("the cntlr file holds revision %d, want 3", got)
		}
	})
	t.Run("the save fails", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupTdA(t, srv, node)
		path := srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr)
		oc := node.osClient()
		write := oc.WriteProtoFn
		oc.WriteProtoFn = func(
			ctx context.Context, p string, msg proto.Message,
		) error {
			if p == path {
				return errors.New("write " + p + ": no space left on device")
			}
			return write(ctx, p, msg)
		}
		srv.store = agent.NewStore(oc, common.DefaultLocalStorPrefix)
		node.Reset()
		reply, err := srv.SyncupCntlr(ctx, newTdReq(3))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		if got := reply.GetRevision(); got != 3 {
			t.Errorf("the reply carries revision %d, want 3", got)
		}
		if !node.hasCall("cmd dmsetup create " + thinName(srv, testSnapTd)) {
			t.Error("a failed save stopped the converge: td B's thin " +
				"volume was not built")
		}
		if got := storedCntlrRevision(t, srv, node); got != 2 {
			t.Errorf("the cntlr file holds revision %d, want the 2 before "+
				"the failed save", got)
		}
	})
}

// A restart after a cntlr pass the worker cut off keeps the thin volume
// that pass created. The cntlr's request was saved before its converge
// (dnagent.md SH5), so the restart converges the request that lists td B,
// never the one before it. That one lacks td B, and the startup reconcile's
// sweep removes the thin volume of a td its request does not list and,
// while the pool is wanted, deletes its thin id (CN21): td B's data is
// lost. The newer request, which the worker sends again, then makes the id
// anew and empty, as for any td not yet created (CN14), so nothing reports
// the loss. The pass is cut off after td B's thin id and thin volume were
// made, at the creation of its dm-error.
func TestARestartKeepsTheThinIdOfACutOffPass(t *testing.T) {
	srv, node := newTestServer(t)
	pool := poolName(srv)
	syncupTdA(t, srv, node)

	node.Reset()
	cutOffSyncupCntlr(t, srv, node, newTdReq(3),
		"dmsetup create "+errorName(srv, testSnapTd))
	if !thinIdHeld(node, pool, sweepLiveDevId2) ||
		!node.hasCall("cmd dmsetup create "+thinName(srv, testSnapTd)) {
		t.Fatal("fixture is wrong: the cut-off pass did not create td B's " +
			"thin id and thin volume")
	}

	// The process exits: its retries stop. The kernel state and the local
	// store survive.
	st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	srv.stopConnectRetry(st)
	srv.stopCloneMetaRetry(st)
	node.Reset()
	fresh := newCnServer(node)
	reconcileForTest(t, fresh)

	if n := callCnt(node, deleteMsg(pool, sweepLiveDevId2)); n != 0 {
		t.Fatalf("the restart deleted td B's thin id (%d messages)\n"+
			"calls:\n%s", n, strings.Join(node.Calls(), "\n"))
	}
	if !thinIdHeld(node, pool, sweepLiveDevId2) {
		t.Fatal("td B's thin id is gone from the pool metadata")
	}
	node.mu.Lock()
	_, built := node.dms[thinName(srv, testSnapTd)]
	node.mu.Unlock()
	if !built {
		t.Error("the restart removed td B's thin volume")
	}
	if !thinIdHeld(node, pool, sweepLiveDevId) {
		t.Error("td A's thin id is gone from the pool metadata")
	}
	if got := storedCntlrRevision(t, fresh, node); got != 3 {
		t.Errorf("the cntlr file holds revision %d, want 3", got)
	}
}
