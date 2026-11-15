package dnagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestASourceLinksItsExportOnlyOnceItsFenceHeld is DN12's gate on step 3: a
// migration source whose suspend of the primary's per-CN dm-linear, which
// still maps the side device, fails in phase 1 links no export, its export
// row reads pending naming the dm-linear, on the converge and on a check
// round's read alike, and the pass answers the leftover code; the next pass,
// whose suspend holds, links it. The standby's dm-linear sits on its own
// dm-error, so a failed suspend of it holds nothing back.
func TestASourceLinksItsExportOnlyOnceItsFenceHeld(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	srcNqn := nf.MigrSrcNqn(testCluster, testDn, testSp, testMigrId)
	link := "ln -s " + agent.NvmetRoot + "/subsystems/" + srcNqn
	primaryLin := nf.DnLinearName(testCluster, testDn, testSp, testSide,
		testCn0)

	srv.fenceWait = time.Hour
	node.mu.Lock()
	node.failCmd["dmsetup suspend "+primaryLin] = "refused"
	node.mu.Unlock()
	node.Reset()
	reply, err := srv.SyncupSide(ctx, migrSrcReq(2))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if node.hasCall(link) {
		t.Fatal("the export was linked while a suspend of the fence failed")
	}
	row := reply.GetSideInfo().GetMigrSrcInfo().GetNvmeofInfo()
	if row.GetStatus() != pb.ResStatus_RES_STATUS_PENDING ||
		!strings.Contains(row.GetDetails(), detailsLinkHeld+primaryLin) {
		t.Fatalf("the export's row is %v, want pending naming %s",
			row, primaryLin)
	}
	if code := reply.GetAgentReply().GetCode(); code !=
		common.ReplyCodeLeftover {
		t.Fatalf("the pass answered code %d, want the leftover code %d",
			code, common.ReplyCodeLeftover)
	}
	// A check round's read reports the held export as the converge does.
	got, err := sideInfoNow(srv, ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	readRow := got.GetSideInfo().GetMigrSrcInfo().GetNvmeofInfo()
	if readRow.GetStatus() != pb.ResStatus_RES_STATUS_PENDING ||
		!strings.Contains(readRow.GetDetails(), detailsLinkHeld+primaryLin) {
		t.Fatalf("the read reports the export as %v, want pending naming %s",
			readRow, primaryLin)
	}

	node.Reset()
	reply, err = srv.SyncupSide(ctx, migrSrcReq(2))
	if err != nil {
		t.Fatalf("SyncupSide again: %v", err)
	}
	if !node.hasCall(link) {
		t.Fatal("the export was not linked once every suspend held")
	}
	if got := reply.GetSideInfo().GetMigrSrcInfo().GetNvmeofInfo().
		GetStatus(); got != pb.ResStatus_RES_STATUS_OK {
		t.Fatalf("the export's row reads %v, want OK", got)
	}
}

// TestASourceLeftoverLinearHoldsTheLink: a per-CN dm-linear of a CN the
// request no longer names, still live on the side device because its
// removal failed, holds the export's link as a failed suspend does (DN12):
// it can still write the side while the destination copies. Once it is gone
// the link is made, and a later pass whose fence step fails leaves the
// linked export linked.
func TestASourceLeftoverLinearHoldsTheLink(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	const leftCn = uint64(7)
	srcNqn := nf.MigrSrcNqn(testCluster, testDn, testSp, testMigrId)
	link := "ln -s " + agent.NvmetRoot + "/subsystems/" + srcNqn
	leftover := nf.DnLinearName(testCluster, testDn, testSp, testSide,
		leftCn)
	sideNo := node.devNo[nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide))]
	// A CN the request no longer names left its dm-linear on the side
	// device, and its removal fails.
	node.mu.Lock()
	node.dms[leftover] = &fakeDm{table: "0 51200 linear " + sideNo + " 0"}
	node.devNo[nf.DmPath(leftover)] = node.newDevNo()
	node.failCmdAlways["dmsetup remove "+leftover] = "busy"
	node.mu.Unlock()

	srv.fenceWait = time.Hour
	node.Reset()
	reply, err := srv.SyncupSide(ctx, migrSrcReq(2))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if node.hasCall(link) {
		t.Fatal("the export was linked beside a leftover dm-linear on the " +
			"side device")
	}
	row := reply.GetSideInfo().GetMigrSrcInfo().GetNvmeofInfo()
	if row.GetStatus() != pb.ResStatus_RES_STATUS_PENDING ||
		!strings.Contains(row.GetDetails(), leftover) {
		t.Fatalf("the export's row is %v, want pending naming %s",
			row, leftover)
	}

	node.mu.Lock()
	delete(node.failCmdAlways, "dmsetup remove "+leftover)
	node.mu.Unlock()
	node.Reset()
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !node.hasCall(link) {
		t.Fatal("the export was not linked once the leftover was gone")
	}

	// A later pass that finds a dm-linear not proven fenced again keeps the
	// export an earlier pass linked, and its row reads as the link reads.
	node.mu.Lock()
	node.dms[leftover] = &fakeDm{table: "0 51200 linear " + sideNo + " 0"}
	node.devNo[nf.DmPath(leftover)] = node.newDevNo()
	node.failCmdAlways["dmsetup remove "+leftover] = "busy"
	node.mu.Unlock()
	node.Reset()
	reply, err = srv.SyncupSide(ctx, migrSrcReq(2))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	_, stillLinked := node.links[portPathOf(common.NvmetPortId)+
		"/subsystems/"+srcNqn]
	node.mu.Unlock()
	if !stillLinked {
		t.Fatal("the linked export was unlinked")
	}
	if got := reply.GetSideInfo().GetMigrSrcInfo().GetNvmeofInfo().
		GetStatus(); got != pb.ResStatus_RES_STATUS_OK {
		t.Fatalf("the linked export reads %v, want OK", got)
	}
}
