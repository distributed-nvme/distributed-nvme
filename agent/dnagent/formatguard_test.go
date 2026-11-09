package dnagent

import (
	"context"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// DN5's format gate reads the local store too: a blank header after a reboot
// has no live device over it, and a stored side at provisioned = true is the
// one thing left that says the disk held a table. Formatting it would take
// away the by-hand recovery of the old table and hand the old extents to the
// next side. Sides not provisioned, a store an operator cleared, and a DN's
// list of side pointers never block: the restore and re-add procedures, and
// a new disk's first SyncupDn, must still format.

// rebootBlank reboots the node with its header blanked, the way a stray
// write and then a reboot leave it, and starts a fresh server over it. It
// returns the server and the meta row the startup reconcile stored.
func rebootBlank(
	t *testing.T,
	srv *DnAgentServer,
	node *fakeNode,
	arm func(),
) (*DnAgentServer, *pb.ResInfo) {
	t.Helper()
	stopTestServer(t, srv)
	node.rebootKernel()
	node.corruptBlock(testDisk, common.DnHeaderOffset,
		make([]byte, common.DnHeaderSize))
	if arm != nil {
		arm()
	}
	node.Reset()
	srv = startTestServer(t, node)
	reply, err := srv.GetDnInfo(context.Background(),
		&pb.GetDnInfoRequest{ClusterId: testCluster, DnId: testDn})
	if err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	return srv, reply.GetDnInfo().GetMetaInfo()
}

// headerWritten reports whether the node saw a write of the disk's header.
func headerWritten(node *fakeNode) bool {
	return len(node.callsMatching("writeblock "+testDisk+" off=0 ")) > 0
}

func TestABlankHeaderAfterARebootIsNotFormattedOverAProvisionedSide(
	t *testing.T,
) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	sideDev := nf.DnSideName(testCluster, testDn, testSp, testSide)

	_, meta := rebootBlank(t, srv, node, nil)
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(meta.GetDetails(), "refusing to format") ||
		!strings.Contains(meta.GetDetails(), sideDev) {
		t.Fatalf("meta row %v, want ERROR refusing to format naming %s",
			meta, sideDev)
	}
	if headerWritten(node) {
		t.Fatal("the header was formatted over a provisioned side")
	}
}

func TestABlankHeaderAfterARebootIsFormattedWhenNoSideIsProvisioned(
	t *testing.T,
) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, srv *DnAgentServer, node *fakeNode) func()
	}{
		{"the store cleared of its sides", func(
			t *testing.T, srv *DnAgentServer, node *fakeNode,
		) func() {
			syncupBoth(t, srv, 1, testSide)
			return func() {
				node.mu.Lock()
				defer node.mu.Unlock()
				for path := range node.protos {
					if strings.HasPrefix(path,
						common.DefaultLocalStorPrefix+"/side-") {
						delete(node.protos, path)
					}
				}
			}
		}},
		{"a side not provisioned", func(
			t *testing.T, srv *DnAgentServer, node *fakeNode,
		) func() {
			if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(1,
				testSide, testCn0, nil,
				pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			return nil
		}},
		{"a list of side pointers with no side stored", func(
			t *testing.T, srv *DnAgentServer, node *fakeNode,
		) func() {
			if _, err := srv.SyncupDn(ctx,
				dnReq(1, testSide, testSide2)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			arm := tc.setup(t, srv, node)
			_, meta := rebootBlank(t, srv, node, arm)
			if meta.GetStatus() != pb.ResStatus_RES_STATUS_OK {
				t.Fatalf("meta row %v, want OK after a format", meta)
			}
			if !headerWritten(node) {
				t.Fatal("the blank header was not formatted")
			}
		})
	}
}

func TestABlankHeaderIsNotFormattedOverAnUnreadableSideFile(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	if _, err := srv.SyncupDn(context.Background(),
		dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	path := nf.LocalSidePath(testCluster, testDn, testSp, testSide)
	_, meta := rebootBlank(t, srv, node, func() {
		node.mu.Lock()
		// A truncated varint: no message decodes from it.
		node.protos[path] = []byte{0xff}
		node.mu.Unlock()
	})
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(meta.GetDetails(), "refusing to format") ||
		!strings.Contains(meta.GetDetails(), path) {
		t.Fatalf("meta row %v, want ERROR refusing to format naming %s",
			meta, path)
	}
	if headerWritten(node) {
		t.Fatal("the header was formatted over an unreadable side file")
	}
}

func TestABlankHeaderIsNotFormattedWhenTheStoreCannotBeListed(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	// No device maps the disk any more, so only the store says it held a
	// table.
	node.rebootKernel()
	node.corruptBlock(testDisk, common.DnHeaderOffset,
		make([]byte, common.DnHeaderSize))
	// The read that finds the header blank drops the loaded table (DN18), so
	// the SyncupDn below reads the disk again.
	checkDnNow(srv, context.Background(), &pb.CheckDnRequest{
		ClusterId: testCluster, DnId: testDn, Revision: 1,
	}, nil)
	// The startup listing answered; the gate's own listing does not.
	node.mu.Lock()
	node.failCmdAlways["list "+common.DefaultLocalStorPrefix] = "EIO"
	node.mu.Unlock()
	node.Reset()
	reply, err := srv.SyncupDn(context.Background(), dnReq(2, testSide))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	meta := reply.GetDnInfo().GetMetaInfo()
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(meta.GetDetails(), "refusing to format") {
		t.Fatalf("meta row %v, want ERROR refusing to format", meta)
	}
	if headerWritten(node) {
		t.Fatal("the header was formatted while the store could not be read")
	}
}

// TestABlankHeaderIsFormattedOverASideItsDnNoLongerLists: a provisioned side
// whose pointer left its DN's list while the delete of its state file
// failed. No one wants the side any more, so its leftover file does not keep
// a blank disk from its format until the next restart deletes it.
func TestABlankHeaderIsFormattedOverASideItsDnNoLongerLists(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	syncupBoth(t, srv, 1, testSide)
	path := nf.LocalSidePath(testCluster, testDn, testSp, testSide)
	node.mu.Lock()
	node.failCmdAlways["rm -f "+path] = "rm: Input/output error"
	node.mu.Unlock()
	if _, err := srv.SyncupDn(ctx, dnReq(2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.mu.Lock()
	_, kept := node.protos[path]
	node.mu.Unlock()
	if !kept {
		t.Fatal("fixture is wrong: the side's state file was deleted")
	}
	node.rebootKernel()
	node.corruptBlock(testDisk, common.DnHeaderOffset,
		make([]byte, common.DnHeaderSize))
	checkDnNow(srv, ctx, &pb.CheckDnRequest{
		ClusterId: testCluster, DnId: testDn, Revision: 2,
	}, nil)
	node.Reset()
	reply, err := srv.SyncupDn(ctx, dnReq(2))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if meta := reply.GetDnInfo().GetMetaInfo(); meta.GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Fatalf("meta row %v, want OK after a format", meta)
	}
	if !headerWritten(node) {
		t.Fatal("the blank header was not formatted")
	}
}

// TestABlankHeaderIsNotFormattedOverASideOfAnotherDn: a provisioned side
// stored for a DN this agent holds no request of, a store left from another
// node id, say. No list this agent holds can say the side was dropped, so it
// refuses the format as any stored provisioned side does.
func TestABlankHeaderIsNotFormattedOverASideOfAnotherDn(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	if _, err := srv.SyncupDn(ctx, dnReq(1)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	other := sideReq(1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE)
	other.DnId = testDn + 5
	path := nf.LocalSidePath(testCluster, testDn+5, testSp, testSide)
	if err := node.writeProto(ctx, path, other); err != nil {
		t.Fatalf("seeding the side state file: %v", err)
	}
	sideDev := nf.DnSideName(testCluster, testDn+5, testSp, testSide)
	_, meta := rebootBlank(t, srv, node, nil)
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(meta.GetDetails(), sideDev) {
		t.Fatalf("meta row %v, want ERROR refusing to format naming %s",
			meta, sideDev)
	}
	if headerWritten(node) {
		t.Fatal("the header was formatted over another DN's stored side")
	}
}
