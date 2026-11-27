package dnagent

import (
	"context"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestZeroingCountsComeOnlyFromAConfirmedTable: a second agent over a disk
// whose header names another node finds there a record with its side's ids,
// zeroed to the end. Its side converge's reply and the side's probe read
// the foreign-disk refusal and carry zeroed bytes zero, so the worker's
// flip, which needs zeroed bytes equal to a non-zero length to zero
// (dnv-worker.md RW18), never runs; the owner's probe, before, reads the
// record's counts (DN5, DN9, DN18).
func TestZeroingCountsComeOnlyFromAConfirmedTable(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	_, ownInfo := checkSideNow(srv, traced("own-probe"),
		&pb.CheckSideRequest{ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide), Revision: 1, ShowInfo: true},
		nil)
	if ownInfo.GetZeroedBytes() != testZeroBytes ||
		ownInfo.GetZeroBytes() != testZeroBytes {
		t.Fatalf("the owner's probe reads %d of %d zeroed, want the "+
			"record's %d of %d", ownInfo.GetZeroedBytes(),
			ownInfo.GetZeroBytes(), testZeroBytes, testZeroBytes)
	}

	// A second agent claims the same device under another dn id.
	node.mu.Lock()
	node.protos = map[string][]byte{}
	node.mu.Unlock()
	srv2 := NewDnAgentServer(node.osClient(),
		common.NewNameFmt(common.DefaultLocalStorPrefix),
		common.DefaultLocalStorPrefix, testDisk, testTrConf(),
		common.NvmetPortId, 0)
	srv2.roundReads = false
	if err := srv2.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	other := testDn + 100
	dn := dnReq(1, testSide)
	dn.DnId = other
	if _, err := srv2.SyncupDn(ctx, dn); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	side := unprovisionedSideReq(1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)
	side.DnId = other
	reply, err := srv2.SyncupSide(ctx, side)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	check := func(label string, info *pb.SideInfo) {
		t.Helper()
		if row := info.GetSideDevInfo(); row.GetStatus() !=
			pb.ResStatus_RES_STATUS_ERROR ||
			!strings.Contains(row.GetDetails(), "foreign disk") {
			t.Errorf("%s: the side device row is %v, want the foreign disk",
				label, row)
		}
		if info.GetZeroedBytes() != 0 {
			t.Errorf("%s: zeroed bytes %d from a foreign table, want 0",
				label, info.GetZeroedBytes())
		}
	}
	check("the converge", reply.GetSideInfo())
	round, info := checkSideNow(srv2, traced("foreign-probe"),
		&pb.CheckSideRequest{ClusterId: testCluster, DnId: other,
			SidePointer: sidePtr(testSide), Revision: 1, ShowInfo: true},
		nil)
	if round.GetAgentReply().GetCode() == common.ReplyCodeUnknownObject {
		t.Fatalf("fixture is wrong: the probe's side is unknown")
	}
	check("the probe", info)
}
