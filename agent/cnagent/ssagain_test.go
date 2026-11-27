package cnagent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The ids of the subsystem made again under testNqn, and of its namespace.
const (
	againSs = uint64(0x5a)
	againNs = uint64(0x5b)
)

// subsysMadeAgain is defaultSubsys deleted and created again under its NQN:
// a new ss_id, so a new serial, and a new namespace over the same volume.
func subsysMadeAgain() map[string]*pb.Subsystem {
	subsys := defaultSubsys(false)
	ss := subsys[testNqn]
	ss.SsId = againSs
	ss.Serial = fmt.Sprintf(common.IdKeyFmt, againSs)
	ss.NsList[0].NsId = againNs
	return subsys
}

// TestASubsystemMadeAgainIsBuilt is CN16's subsystem made again: once a host
// has identified the kernel subsystem, nvmet refuses its serial, and a
// subsystem deleted and created again under its NQN carries another one.
// The converge moves the old subsystem's namespace inaccessible before the
// park of its old ns-dev, removes the subsystem, and builds it again in the
// same pass with the new serial and the new namespace, which serves; the
// rows read OK and a second converge removes the subsystem no more.
func TestASubsystemMadeAgainIsBuilt(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	subsysDir := agent.NvmetRoot + "/subsystems/" + testNqn
	nsPath := subsysDir + "/namespaces/1"
	optimized := strconv.Itoa(common.AnaGrpIdOptimized)
	assertAna(t, node, testNqn, optimized, "the first subsystem's namespace")
	node.mu.Lock()
	node.frozenSerial[subsysDir] = true
	node.mu.Unlock()

	node.Reset()
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{revision: 3,
		primary: true, subsys: subsysMadeAgain(), leftoverOk: true}))
	if err != nil {
		t.Fatalf("SyncupCntlr of the subsystem made again: %v", err)
	}
	info := reply.GetCntlrInfo()
	assertOk(t, info.GetSsIdToSubsystem()[againSs],
		"the subsystem made again")
	assertOk(t, info.GetNsIdToNamespace()[againNs],
		"the namespace of the subsystem made again")
	want := fmt.Sprintf(common.IdKeyFmt, againSs)
	if got := strings.TrimSpace(node.fileOf(subsysDir + "/attr_serial")); got != want {
		t.Fatalf("the serial is %q, want the new %q", got, want)
	}
	newDev := srv.nf.DmPath(names(srv).CnNsDevName(testCluster, testCn,
		testSp, againNs))
	if got := strings.TrimSpace(node.fileOf(nsPath + "/device_path")); got != newDev {
		t.Fatalf("the namespace's device is %q, want the new %q", got, newDev)
	}
	assertAna(t, node, testNqn, optimized,
		"the namespace of the subsystem made again")
	dark := node.indexOfCall("writedirect " + nsPath + "/ana_grpid=" +
		strconv.Itoa(common.AnaGrpIdInaccessible))
	removed := -1
	for i, call := range node.Calls() {
		if strings.HasPrefix(call, "cmd rmdir ") &&
			strings.HasSuffix(call, " "+subsysDir) {
			removed = i
			break
		}
	}
	park := node.indexOfCall("cmd dmsetup suspend " + nsDevName(srv, testNs))
	if dark < 0 || park < 0 || dark > park {
		t.Fatalf("the old namespace did not move inaccessible before its "+
			"ns-dev's park: move at %d, park at %d", dark, park)
	}
	if dark < 0 || removed < 0 || dark > removed {
		t.Fatalf("the old namespace did not move inaccessible before the "+
			"subsystem's removal: move at %d, removal at %d", dark, removed)
	}

	node.Reset()
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{revision: 3,
		primary: true, subsys: subsysMadeAgain()})); err != nil {
		t.Fatalf("SyncupCntlr again: %v", err)
	}
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "cmd rmdir ") &&
			strings.HasSuffix(call, " "+subsysDir) {
			t.Fatalf("a second converge removed the subsystem again")
		}
	}
}

// TestASubsystemMadeAgainWhoseMoveFailsStays: when the old subsystem's
// namespace neither moves inaccessible nor goes (its disable refused, so the
// sweep cannot take it out), the converge removes no subsystem, the
// subsystem's row reads the error naming the namespace, and its namespace
// stays over the old device.
func TestASubsystemMadeAgainWhoseMoveFailsStays(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	subsysDir := agent.NvmetRoot + "/subsystems/" + testNqn
	nsPath := subsysDir + "/namespaces/1"
	node.mu.Lock()
	node.frozenSerial[subsysDir] = true
	node.failWriteAlways[nsPath+"/ana_grpid="+
		strconv.Itoa(common.AnaGrpIdInaccessible)] = "refused"
	node.failWriteAlways[nsPath+"/enable=0"] = "refused"
	node.mu.Unlock()

	reply, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{revision: 3,
		primary: true, subsys: subsysMadeAgain(), leftoverOk: true}))
	if err != nil {
		t.Fatalf("SyncupCntlr of the subsystem made again: %v", err)
	}
	row := reply.GetCntlrInfo().GetSsIdToSubsystem()[againSs]
	if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(row.GetDetails(), "did not move to inaccessible") {
		t.Fatalf("the subsystem row is %v, want ERROR naming the move", row)
	}
	if !node.dirs[subsysDir] || node.hasCall("cmd rmdir "+subsysDir) {
		t.Fatalf("the subsystem was removed though its namespace's move " +
			"failed")
	}
	oldDev := srv.nf.DmPath(nsDevName(srv, testNs))
	if got := strings.TrimSpace(node.fileOf(nsPath + "/device_path")); got != oldDev {
		t.Fatalf("the namespace's device is %q, want the old %q", got, oldDev)
	}
}

// TestASubsystemMadeAgainWhoseRemovalFailsStaysDark: when the old
// subsystem's namespace moves inaccessible but its disable is refused, so
// neither the sweep nor the removal can take it out, the old subsystem
// stays and is not safe in the pass: no ANA move makes the old namespace
// serve over its parked ns-dev, and the subsystem's row reads the removal's
// error.
func TestASubsystemMadeAgainWhoseRemovalFailsStaysDark(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	subsysDir := agent.NvmetRoot + "/subsystems/" + testNqn
	nsPath := subsysDir + "/namespaces/1"
	node.mu.Lock()
	node.frozenSerial[subsysDir] = true
	node.failWriteAlways[nsPath+"/enable=0"] = "refused"
	node.mu.Unlock()

	reply, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{revision: 3,
		primary: true, subsys: subsysMadeAgain(), leftoverOk: true}))
	if err != nil {
		t.Fatalf("SyncupCntlr of the subsystem made again: %v", err)
	}
	row := reply.GetCntlrInfo().GetSsIdToSubsystem()[againSs]
	if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(row.GetDetails(), `enable = "0": refused`) {
		t.Fatalf("the subsystem row is %v, want ERROR naming the refused "+
			"disable (the row's cap cuts its middle)", row)
	}
	assertAna(t, node, testNqn, strconv.Itoa(common.AnaGrpIdInaccessible),
		"the old namespace under the subsystem that stays")
}

// TestASubsystemMadeAgainOverASuspendedDevice: an old namespace whose device
// stays suspended, its park cut short, is never disabled: the removal stops
// before it, the old subsystem stays, and the row names the device.
func TestASubsystemMadeAgainOverASuspendedDevice(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	subsysDir := agent.NvmetRoot + "/subsystems/" + testNqn
	nsPath := subsysDir + "/namespaces/1"
	oldDev := nsDevName(srv, testNs)
	node.mu.Lock()
	node.frozenSerial[subsysDir] = true
	node.dms[oldDev].suspended = true
	node.failCmdAlways["dmsetup resume "+oldDev] = "resume refused"
	node.mu.Unlock()

	node.Reset()
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{revision: 3,
		primary: true, subsys: subsysMadeAgain(), leftoverOk: true}))
	if err != nil {
		t.Fatalf("SyncupCntlr of the subsystem made again: %v", err)
	}
	row := reply.GetCntlrInfo().GetSsIdToSubsystem()[againSs]
	if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(row.GetDetails(), "is suspended, and its disable") {
		t.Fatalf("the subsystem row is %v, want ERROR naming the "+
			"suspended device (the row's cap cuts its middle)", row)
	}
	if node.hasCall("writedirect " + nsPath + "/enable=0") {
		t.Fatalf("the namespace over the suspended device was disabled")
	}
	if !node.dirs[subsysDir] {
		t.Fatalf("the subsystem went though its namespace stays")
	}
}
