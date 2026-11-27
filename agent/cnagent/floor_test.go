package cnagent

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestASkippedCntlrFileKeepsItsRevisionAsTheGate pins CN2's floor: at a
// restart whose read of the cn file fails once, the cntlr file is skipped
// with its CN, and the revision it carries stays the floor of CN8's revision
// gate until a request of the cntlr is accepted. A request older than the
// file is refused as stale, the file left as it was and no lock entry left
// behind, and one at the file's revision or above is taken. Without the
// floor the gate ran against zero, so an older request from a second worker
// passed, and its converge removed what it did not name.
func TestASkippedCntlrFileKeepsItsRevisionAsTheGate(t *testing.T) {
	const fileRev = 5
	key := cntlrKey(testCluster, testCn, testSp, testCntlr)
	restart := func(t *testing.T) (*CnAgentServer, *fakeNode, string) {
		t.Helper()
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: fileRev})
		cntlrPath := srv.nf.LocalCntlrPath(
			testCluster, testCn, testSp, testCntlr)
		if got := storedProto(t, node, cntlrPath).GetRevision(); got !=
			fileRev {
			t.Fatalf("cntlr file revision %d, want %d", got, fileRev)
		}
		node.mu.Lock()
		node.failProtoRead[srv.nf.LocalCnPath(testCluster, testCn)] = true
		node.mu.Unlock()
		node.Reset()

		restarted := newCnServer(node)
		restarted.probeInterval = time.Hour
		reconcileForTest(t, restarted)
		if !node.hasCall("readproto " + cntlrPath) {
			t.Fatalf("the restart never read the cntlr file")
		}
		if restarted.getCn(cnKey(testCluster, testCn)) != nil {
			t.Fatalf("the cn file loaded though its read failed")
		}
		if restarted.getCntlr(key) != nil {
			t.Fatalf("the cntlr of an unloaded cn was loaded")
		}
		if got := restarted.cntlrFloor(key); got != fileRev {
			t.Fatalf("floor %d after the restart, want %d", got, fileRev)
		}
		// The re-sent SyncupCn puts the pointer back (CN8's first gate).
		reply, err := restarted.SyncupCn(context.Background(),
			cnReq(fileRev, true))
		if err != nil || reply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("SyncupCn = %v, %v", reply.GetAgentReply(), err)
		}
		node.Reset()
		return restarted, node, cntlrPath
	}
	syncupAt := func(
		t *testing.T, srv *CnAgentServer, revision uint64,
	) *pb.AgentReply {
		t.Helper()
		reply, err := srv.SyncupCntlr(context.Background(),
			cntlrReq(reqOpts{revision: revision, primary: true}))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		return reply.GetAgentReply()
	}

	t.Run("an older request is refused", func(t *testing.T) {
		srv, node, cntlrPath := restart(t)
		locks := srv.locks.ObjCount()

		got := syncupAt(t, srv, fileRev-1)

		if got.GetCode() != common.ReplyCodeStaleRevision {
			t.Fatalf("SyncupCntlr below the skipped file's revision = "+
				"%v, want code %d", got, common.ReplyCodeStaleRevision)
		}
		if mutations := node.Mutations(); len(mutations) != 0 {
			t.Errorf("a refused request changed the node: %q", mutations)
		}
		if rev := storedProto(t, node, cntlrPath).GetRevision(); rev !=
			fileRev {
			t.Errorf("cntlr file revision %d after the refusal, want %d",
				rev, fileRev)
		}
		if srv.getCntlr(key) != nil {
			t.Errorf("a refused request was held")
		}
		if n := srv.locks.ObjCount(); n != locks {
			t.Errorf("a refused request left %d lock entries, want %d",
				n, locks)
		}
		// syncupCntlr, which runs every gate again under the object lock,
		// refuses it as well.
		inner := srv.syncupCntlr(context.Background(), key,
			cntlrReq(reqOpts{revision: fileRev - 1, primary: true}))
		if code := inner.GetAgentReply().GetCode(); code !=
			common.ReplyCodeStaleRevision {
			t.Errorf("syncupCntlr below the floor = code %d, want %d",
				code, common.ReplyCodeStaleRevision)
		}
	})

	t.Run("a request whose save fails keeps the floor", func(t *testing.T) {
		srv, node, cntlrPath := restart(t)
		fault := failSaves(srv, node, cntlrPath)

		reply, err := srv.SyncupCntlr(context.Background(),
			cntlrReq(reqOpts{revision: fileRev, primary: true}))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		if got := reply.GetAgentReply().GetCode(); got !=
			common.ReplyCodeSaveFailed {
			t.Fatalf("SyncupCntlr at the floor with a failing save = %v, "+
				"want code %d", reply.GetAgentReply(),
				common.ReplyCodeSaveFailed)
		}
		// Refused at the revision the process holds: none.
		if got := reply.GetRevision(); got != 0 {
			t.Errorf("the refused save's reply carries revision %d, want 0",
				got)
		}
		if got := srv.cntlrFloor(key); got != fileRev {
			t.Errorf("floor %d after a refused save, want %d", got, fileRev)
		}
		// The store disk takes writes again; an older request is still
		// refused.
		fault.mu.Lock()
		delete(fault.paths, cntlrPath)
		fault.mu.Unlock()
		if got := syncupAt(t, srv, fileRev-1); got.GetCode() !=
			common.ReplyCodeStaleRevision {
			t.Fatalf("SyncupCntlr below the floor after a refused save = "+
				"%v, want code %d", got, common.ReplyCodeStaleRevision)
		}
		if rev := storedProto(t, node, cntlrPath).GetRevision(); rev !=
			fileRev {
			t.Errorf("cntlr file revision %d, want %d", rev, fileRev)
		}
	})

	for _, revision := range []uint64{fileRev, fileRev + 1} {
		t.Run(fmt.Sprintf("a request at revision %d is taken", revision),
			func(t *testing.T) {
				srv, node, cntlrPath := restart(t)

				if got := syncupAt(t, srv, revision); got.GetCode() != 0 {
					t.Fatalf("SyncupCntlr at %d = %v, want code 0",
						revision, got)
				}
				if rev := storedProto(t, node, cntlrPath).GetRevision(); rev !=
					revision {
					t.Errorf("cntlr file revision %d, want %d", rev, revision)
				}
				// The held request gates from here on, and the floor ends.
				if got := srv.cntlrFloor(key); got != 0 {
					t.Errorf("floor %d once a request is held, want 0", got)
				}
				if got := syncupAt(t, srv, revision-1); got.GetCode() !=
					common.ReplyCodeStaleRevision {
					t.Errorf("SyncupCntlr below the held revision = %v, "+
						"want code %d", got, common.ReplyCodeStaleRevision)
				}
			})
	}
}

// TestACntlrFloorOnlyRises: two skipped files that name one cntlr leave the
// higher revision as its floor, whichever the pass reads first.
func TestACntlrFloorOnlyRises(t *testing.T) {
	srv := newCnServer(newFakeNode())
	key := cntlrKey(testCluster, testCn, testSp, testCntlr)
	srv.setCntlrFloor(key, 7)
	srv.setCntlrFloor(key, 3)
	if got := srv.cntlrFloor(key); got != 7 {
		t.Errorf("floor %d, want 7", got)
	}
	srv.setCntlrFloor(key, 9)
	if got := srv.cntlrFloor(key); got != 9 {
		t.Errorf("floor %d, want 9", got)
	}
}
