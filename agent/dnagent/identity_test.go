package dnagent

import (
	"context"
	"fmt"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestSideExportIdentity pins the identity every per-CN export of a side
// presents (DN10): "attr_serial" is the leg id in IdKeyFmt and "attr_model"
// is common.NvmeModel, the model dnv sets (architecture.md, [D2]), so the two
// sides of a migrating leg, which export one NQN to a CN, carry the same
// pair. The converge writes the pair into each export, and the probe
// compares each export against the same pair, so the probe after the
// converge reads every export row OK.
func TestSideExportIdentity(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	dnReply, err := srv.SyncupDn(ctx, dnReq(1, testSide))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if dnReply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn rejected: %v", dnReply.GetAgentReply())
	}
	cnIds := []uint64{testCn0, testCn1}
	converged := syncupSideTwoPhase(t, srv, sideReq(1, testSide, cnIds[0],
		cnIds[1:], pb.SpLevel_SP_LEVEL_READWRITE)).GetSideInfo()
	probed, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	wantSerial := fmt.Sprintf(common.IdKeyFmt, testLeg)
	for _, cnId := range cnIds {
		for _, pass := range []struct {
			name string
			info *pb.SideInfo
		}{
			{"converge", converged},
			{"probe", probed.GetSideInfo()},
		} {
			row, ok := pass.info.GetCnIdToNvmeof()[cnId]
			if !ok || row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
				t.Errorf("cn %d: the %s's export row is %v %q (present %v)",
					cnId, pass.name, row.GetStatus(), row.GetDetails(), ok)
			}
		}
		subsys := agent.NvmetRoot + "/subsystems/" +
			nf.SideToCnNqn(testCluster, testSp, testLeg, cnId)
		node.mu.Lock()
		serial, serialOk := node.files[subsys+"/attr_serial"]
		model, modelOk := node.files[subsys+"/attr_model"]
		node.mu.Unlock()
		if !serialOk || serial != wantSerial {
			t.Errorf("cn %d: attr_serial is %q (written %v), want %q, the "+
				"leg id", cnId, serial, serialOk, wantSerial)
		}
		if !modelOk || model != common.NvmeModel {
			t.Errorf("cn %d: attr_model is %q (written %v), want %q",
				cnId, model, modelOk, common.NvmeModel)
		}
	}
}
