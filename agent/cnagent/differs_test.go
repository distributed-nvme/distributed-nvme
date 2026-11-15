package cnagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// readCode is a check round of the fixture cntlr that answers from a read
// made now: the read runs, then the round answers its code.
func readCode(t *testing.T, srv *CnAgentServer, revision uint64) *pb.AgentReply {
	t.Helper()
	refreshTestCntlr(t, srv)
	reply, _ := srv.checkCntlrRound(context.Background(),
		&pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: revision,
		}, nil)
	return reply.GetAgentReply()
}

// A namespace whose move to the inaccessible group failed in a converge's
// first step (CN9 pre-step 1) serves on, in the group it had: its row reads
// OK with RES_REASON_DIFFERS, and the converge's own code names it, so the
// worker sends the syncup again and its converge moves it (review item 35).
func TestAFailedAnaMoveIsNamedAndRetried(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	write := anaPath(testNqn, 1) + "=3"

	node.failWriteAlways[write] = "configfs refused"
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 3, primary: true, suspended: true}))
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	delete(node.failWriteAlways, write)
	assertAna(t, node, testNqn, "1", "the failed move")
	row := reply.GetCntlrInfo().GetNsIdToNamespace()[testNs]
	if row.GetStatus() != pb.ResStatus_RES_STATUS_OK ||
		row.GetReason() != pb.ResReason_RES_REASON_DIFFERS {
		t.Errorf("namespace row = %v/%v/%q, want OK with "+
			"RES_REASON_DIFFERS", row.GetStatus(), row.GetReason(),
			row.GetDetails())
	}
	name := nsResName(testNqn, 1)
	if got := reply.GetAgentReply(); got.GetCode() !=
		common.ReplyCodeLeftover ||
		!strings.Contains(got.GetDetails(), name+": differs: ana_grpid") {
		t.Fatalf("the converge answered %v, want the leftover code naming "+
			"%s", got, name)
	}
	if got := readCode(t, srv, 3); got.GetCode() != common.ReplyCodeLeftover {
		t.Errorf("the read answered %v, want the leftover code", got)
	}

	if reply, err = srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 3, primary: true, suspended: true})); err != nil {
		t.Fatalf("park sent again: %v", err)
	}
	assertAna(t, node, testNqn, "3", "the syncup sent again")
	if got := reply.GetAgentReply(); got.GetCode() != 0 {
		t.Errorf("the syncup sent again answered %v, want code zero", got)
	}
}

// A suspended thin pool, concat or raid0 serves no IO: its row reads ERROR
// and the cntlr's read names it (review item 36), and the syncup sent again
// resumes it.
func TestASuspendedCntlrDeviceIsNamedAndResumed(t *testing.T) {
	for _, tc := range []struct {
		name string
		dev  func(srv *CnAgentServer) string
	}{
		{"the thin pool", func(srv *CnAgentServer) string {
			return poolName(srv)
		}},
		{"the data concat", func(srv *CnAgentServer) string {
			return srv.nf.CnPoolDataName(testCluster, testCn, testSp,
				testSlice)
		}},
		{"the raid0", func(srv *CnAgentServer) string {
			return raid0Name(srv, testTd)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			dev := tc.dev(srv)
			if node.dms[dev] == nil {
				t.Fatalf("fixture is wrong: no %s", dev)
			}
			node.dms[dev].suspended = true
			if got := readCode(t, srv, 2); got.GetCode() !=
				common.ReplyCodeLeftover ||
				!strings.Contains(got.GetDetails(), dev+": differs: "+
					"suspended") {
				t.Fatalf("the read answered %v, want the leftover code "+
					"naming %s", got, dev)
			}
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 2, primary: true}))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if node.dms[dev].suspended || reply.GetAgentReply().GetCode() != 0 {
				t.Errorf("after the syncup sent again: suspended %v, code "+
					"%v", node.dms[dev].suspended, reply.GetAgentReply())
			}
		})
	}
}

// A configfs read that does not answer is never read as an empty value
// (review item 42): the step that converges a namespace object stops on it,
// and a namespace that serves hosts is not moved to the inaccessible group,
// which the loose read's empty enable would have chosen.
func TestAnUnansweredNamespaceReadMovesNothing(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	assertAna(t, node, testNqn, "1", "the fixture")
	nsPath := agent.NvmetRoot + "/subsystems/" + testNqn + "/namespaces/1"

	node.Reset()
	node.killRead[nsPath+"/enable"] = true
	if _, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 2, primary: true,
			leftoverOk: true})); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if node.killRead[nsPath+"/enable"] {
		t.Fatal("fixture is wrong: the enable was never read")
	}
	assertNoCall(t, node, "writedirect "+anaPath(testNqn, 1)+"=3")
	assertAna(t, node, testNqn, "1", "the pass with the unanswered read")
}

// A converge whose call is cut off leaves the rest of its build undone and
// stores no observation; the read after it names what is missing (entry
// 4.21), so the worker sends the syncup again, whose converge builds it. The
// cntlr's first converge is cut off at the create of its namespace's device,
// which the kernel finishes: the subsystem the build makes after it is left
// absent.
func TestACutOffCntlrConvergeIsNamedByTheNextRead(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	ctx := context.Background()
	if reply, err := srv.SyncupCn(ctx, cnReq(2, true)); err != nil ||
		reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCn: %v %v", reply.GetAgentReply(), err)
	}
	subsys := agent.NvmetRoot + "/subsystems/" + testNqn
	req := cntlrReq(reqOpts{revision: 2, primary: true})
	cutOffSyncupCntlr(t, srv, node, req,
		"dmsetup create "+nsDevName(srv, testNs))
	if node.dirs[subsys] {
		t.Fatal("fixture is wrong: the cut-off converge made the subsystem")
	}

	got := readCode(t, srv, 2)
	if got.GetCode() != common.ReplyCodeLeftover ||
		!strings.Contains(got.GetDetails(), testNqn+": absent") {
		t.Fatalf("the read after the cut-off converge answered %v, want "+
			"the leftover code naming %s", got, testNqn)
	}
	if _, err := srv.SyncupCntlr(ctx, req); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if got := readCode(t, srv, 2); got.GetCode() != 0 {
		t.Errorf("the read after the syncup sent again answered %v, want "+
			"code zero", got)
	}
}

// A snapshot whose create keeps failing is not named: the pre-pass that
// makes it suspends its origin (CN14), and a re-send on every round would
// stall the origin's host IO on every round. The converge and the read
// answer code zero, and the next revision tries again, as before.
func TestAFailingSnapshotIsNotSentAgainEveryRound(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	node.failCmdAlways["message "+poolNameOf(srv, testSlice2)+
		" 0 create_snap"] = "device-mapper: message ioctl failed: " +
		"No space left on device"
	reply := syncupSnapshot(t, srv, node, reqOpts{
		revision: 3, primary: true, twoSlices: true, tds: snapTds()})
	if got := reply.GetAgentReply(); got.GetCode() != 0 {
		t.Errorf("the converge answered %v, want code zero", got)
	}
	if got := readCode(t, srv, 3); got.GetCode() != 0 {
		t.Errorf("the read answered %v, want code zero", got)
	}
}

// The snapshot's dm-error is one of the rows that wait with it: a re-send
// for it would run the same pre-pass that suspends the origin.
func TestAWaitingSnapshotsDmErrorIsNotNamed(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	node.failCmdAlways["message "+poolNameOf(srv, testSlice2)+
		" 0 create_snap"] = "device-mapper: message ioctl failed: " +
		"No space left on device"
	snapErr := errorName(srv, testSnapTd)
	node.failCmdAlways["dmsetup create "+snapErr] = "refused"
	reply := syncupSnapshot(t, srv, node, reqOpts{
		revision: 3, primary: true, twoSlices: true, tds: snapTds()})
	if row := reply.GetCntlrInfo().GetTdIdToDmError()[testSnapTd]; row.
		GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("fixture is wrong: the snapshot's dm-error row is %v", row)
	}
	if got := reply.GetAgentReply(); strings.Contains(got.GetDetails(),
		snapErr) {
		t.Errorf("the converge named the waiting snapshot's dm-error: %v",
			got)
	}
	if got := readCode(t, srv, 3); strings.Contains(got.GetDetails(),
		snapErr) {
		t.Errorf("the read named the waiting snapshot's dm-error: %v", got)
	}
}

// dropPort removes the node's nvmet port, its links and its attributes, as
// a port whose mkdir the kernel refused leaves it.
func dropPort(node *fakeNode) {
	portPath := agent.NvmetRoot + "/ports/1"
	node.mu.Lock()
	defer node.mu.Unlock()
	for path := range node.links {
		if strings.HasPrefix(path, portPath+"/") {
			delete(node.links, path)
		}
	}
	for path := range node.dirs {
		if path == portPath || strings.HasPrefix(path, portPath+"/") {
			delete(node.dirs, path)
		}
	}
	for path := range node.files {
		if strings.HasPrefix(path, portPath+"/") {
			delete(node.files, path)
		}
	}
}

// A subsystem's link fails while the node's nvmet port itself is not there:
// a SyncupCntlr cannot make the port, which the CN's verdict names, so the
// cntlr's verdict leaves the subsystem out on both channels.
func TestASubsystemOverAMissingPortIsNotNamed(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	dropPort(node)

	reply, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 2, primary: true}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	row := reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs]
	if row.GetDetails() != detailsPortMissing {
		t.Fatalf("fixture is wrong: subsystem row %v, want the port missing",
			row)
	}
	if strings.Contains(reply.GetAgentReply().GetDetails(), testNqn) {
		t.Errorf("the converge named the subsystem: %v",
			reply.GetAgentReply())
	}
	if got := readCode(t, srv, 2); strings.Contains(got.GetDetails(),
		testNqn) {
		t.Errorf("the read named the subsystem: %v", got)
	}
}

// A move to the inaccessible group is tried even when a read of the
// namespace does not answer: setAna reads the namespace's group alone, and a
// group read that does not answer leaves the move to inaccessible, the safe
// direction, tried all the same (review item 42).
func TestAMoveToInaccessibleIsTriedWhenAReadFails(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	nsPath := agent.NvmetRoot + "/subsystems/" + testNqn + "/namespaces/1"
	node.Reset()
	node.killRead[nsPath+"/device_uuid"] = true
	node.killRead[nsPath+"/ana_grpid"] = true
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, suspended: true,
		leftoverOk: true})); err != nil {
		t.Fatalf("park: %v", err)
	}
	assertAna(t, node, testNqn, "3", "the park with a failed read")
}

// A snapshot not yet created whose thin devices are all there makes no
// message and suspends no origin on a converge (CN14), so a row of it that
// differs is named like any other: here a suspended thin device, which the
// read names and the syncup sent again resumes, with no suspend of the
// origin and no create_snap.
func TestAnUncreatedSnapshotWithAllItsDevicesIsNamed(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	o := reqOpts{revision: 3, primary: true, twoSlices: true, tds: snapTds()}
	syncupSnapshot(t, srv, node, o)
	snapThin := thinNameOf(srv, testSnapTd, testSlice2)
	if node.dms[snapThin] == nil {
		t.Fatalf("fixture is wrong: no snapshot thin %s", snapThin)
	}
	node.dms[snapThin].suspended = true
	if got := readCode(t, srv, 3); got.GetCode() != common.ReplyCodeLeftover ||
		!strings.Contains(got.GetDetails(), snapThin) {
		t.Fatalf("the read answered %v, want the suspended %s named",
			got, snapThin)
	}

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(o))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if node.dms[snapThin].suspended {
		t.Errorf("the syncup sent again left %s suspended", snapThin)
	}
	if got := reply.GetAgentReply(); got.GetCode() != 0 {
		t.Errorf("the syncup sent again answered %v, want code zero", got)
	}
	for _, call := range node.Calls() {
		if strings.Contains(call, "create_snap") ||
			(strings.Contains(call, "dmsetup suspend") &&
				!strings.Contains(call, snapThin)) {
			t.Errorf("the syncup sent again made %q", call)
		}
	}
}

// A transfer's subsystem reads the missing port as a cntlr's subsystem does,
// and is not named either: a SyncupCntlr cannot make the port.
func TestATransferSubsystemOverAMissingPortIsNotNamed(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	o := reqOpts{revision: 2, primary: true, xfers: []*pb.Transfer{{
		XferId:       testXfer,
		OriNqn:       testNqn,
		OriNsIdx:     1,
		AllowedHosts: []string{testHostNqn},
		AutoSuspend:  true,
	}}}
	syncupBoth(t, srv, o)
	dropPort(node)
	xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)

	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(o))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	row := reply.GetCntlrInfo().GetXferIdToSubsystem()[testXfer]
	if row.GetDetails() != detailsPortMissing {
		t.Fatalf("the transfer's subsystem row is %v, want the port missing",
			row)
	}
	if strings.Contains(reply.GetAgentReply().GetDetails(), xferNqn) {
		t.Errorf("the converge named the transfer's subsystem: %v",
			reply.GetAgentReply())
	}
	if got := readCode(t, srv, 2); strings.Contains(got.GetDetails(),
		xferNqn) {
		t.Errorf("the read named the transfer's subsystem: %v", got)
	}
}
