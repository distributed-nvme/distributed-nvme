package dnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The read-only verdicts name what a re-sent Syncup* would build (DN16): a
// wanted side device, per-CN device or export of a side that is absent, a
// side record an allocation would make, and the agent's nvmet port. The
// worker re-sends on a Check reply's code, never on its rows (dnv-worker.md
// RW4), so a verdict that stayed clean would leave the object unbuilt until
// an unrelated revision or the agent's next start.

// worldRounds plays the worker's rounds: a CheckDn and a CheckSide, each
// re-sending its Syncup* when the reply's code is not zero (RW4). It returns
// the first side reply and the first DN reply.
func worldRounds(
	t *testing.T,
	srv *DnAgentServer,
	sideRequest *pb.SyncupSideRequest,
	rounds int,
) (*pb.AgentReply, *pb.AgentReply) {
	t.Helper()
	ctx := context.Background()
	var firstSide, firstDn *pb.AgentReply
	for round := 0; round < rounds; round++ {
		dnReply, _ := checkDnNow(srv, ctx, &pb.CheckDnRequest{
			ClusterId: testCluster, DnId: testDn,
			Revision: sideRequest.GetRevision(),
		}, nil)
		if firstDn == nil {
			firstDn = dnReply.GetAgentReply()
		}
		if dnReply.GetAgentReply().GetCode() != 0 {
			if _, err := srv.SyncupDn(ctx,
				dnReq(sideRequest.GetRevision(), testSide)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
		}
		sideReply, _ := checkSideNow(srv, ctx, &pb.CheckSideRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide),
			Revision:    sideRequest.GetRevision(),
		}, nil)
		if firstSide == nil {
			firstSide = sideReply.GetAgentReply()
		}
		if sideReply.GetAgentReply().GetCode() != 0 {
			if _, err := srv.SyncupSide(ctx, sideRequest); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
		}
	}
	return firstSide, firstDn
}

// TestAReadRebuildsASideAfterAReboot: a provisioned side,
// a reboot that clears every dm device and export, and a disk whose header
// answers no read while the startup reconcile runs, so the reconcile builds
// nothing of the side. Once the disk answers, the side's verdict names the
// absent side device, the re-sent SyncupSide builds the whole stack, and the
// rounds after it are clean.
func TestAReadRebuildsASideAfterAReboot(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	sideDev := nf.DnSideName(testCluster, testDn, testSp, testSide)
	syncupBoth(t, srv, 1, testSide)
	stopTestServer(t, srv)
	node.rebootKernel()
	header := fmt.Sprintf("readblock %s off=%d len=%d",
		testDisk, common.DnHeaderOffset, common.DnHeaderSize)
	setHook(node, node.killReadAlways, header)
	srv = startTestServer(t, node)
	clearHook(node, node.killReadAlways, header)
	if node.dms[sideDev] != nil {
		t.Fatal("the reconcile built the side device without its record")
	}

	firstSide, _ := worldRounds(t, srv, sideReq(1, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE), 3)
	if firstSide.GetCode() != common.ReplyCodeLeftover ||
		!strings.Contains(firstSide.GetDetails(), sideDev+": absent") {
		t.Fatalf("first side verdict = %v, want a leftover naming %s absent",
			firstSide, sideDev)
	}
	node.mu.Lock()
	built := node.dms[sideDev] != nil
	node.mu.Unlock()
	if !built {
		t.Fatal("the re-sent SyncupSide did not build the side device")
	}
	last, _ := checkSideNow(srv, context.Background(), &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
		Revision: 1,
	}, nil)
	if last.GetAgentReply().GetCode() != 0 {
		t.Fatalf("round after the rebuild = %v, want code 0",
			last.GetAgentReply())
	}
}

// TestAReadRebuildsAPerCnDeviceThatFailedToBuild is the same above a ready
// side device: a reboot after which the startup reconcile's create of one
// per-CN linear is refused once. The side's verdict names it, and the
// re-sent SyncupSide builds it.
func TestAReadRebuildsAPerCnDeviceThatFailedToBuild(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	linear := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1)
	syncupBoth(t, srv, 1, testSide)
	stopTestServer(t, srv)
	node.rebootKernel()
	node.mu.Lock()
	node.failCmd["dmsetup create "+linear] = "refused once"
	node.mu.Unlock()
	srv = startTestServer(t, node)
	if node.dms[linear] != nil {
		t.Fatal("the refused create left the linear in place")
	}
	firstSide, _ := worldRounds(t, srv, sideReq(1, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE), 3)
	if firstSide.GetCode() != common.ReplyCodeLeftover ||
		!strings.Contains(firstSide.GetDetails(), linear+": absent") {
		t.Fatalf("first side verdict = %v, want a leftover naming %s absent",
			firstSide, linear)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if node.dms[linear] == nil {
		t.Fatal("the re-sent SyncupSide did not build the linear")
	}
}

// TestAReadRebuildsASideRecordWhoseAllocationFailed: a side not provisioned
// yet whose allocation write fails once has no record and no device. The
// side's verdict names the absent record, since an allocation would succeed,
// and the re-sent SyncupSide allocates it and starts its zeroing.
func TestAReadRebuildsASideRecordWhoseAllocationFailed(t *testing.T) {
	ctx := context.Background()
	sideRequest := unprovisionedSideReq(1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)
	// The save writes one of the two table slots; the slot it writes is the
	// one whose failure leaves the side without a record.
	for _, slot := range []uint64{
		common.DnTableSlotAOffset, common.DnTableSlotBOffset,
	} {
		srv, node := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		node.mu.Lock()
		node.failBlockSet, node.failBlockWrite = true, slot
		node.mu.Unlock()
		if _, err := srv.SyncupSide(ctx, sideRequest); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		node.mu.Lock()
		node.failBlockSet = false
		node.mu.Unlock()
		if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); ok {
			continue
		}
		firstSide, _ := worldRounds(t, srv, sideRequest, 2)
		if firstSide.GetCode() != common.ReplyCodeLeftover ||
			!strings.Contains(firstSide.GetDetails(), "absent") {
			t.Fatalf("first side verdict = %v, want a leftover naming the "+
				"absent record", firstSide)
		}
		if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); !ok {
			t.Fatal("the re-sent SyncupSide did not allocate the record")
		}
		return
	}
	t.Fatal("no slot write failure left the side without a record")
}

// TestAReadDoesNotNameWhatAResendCannotBuild bounds the side verdict: a side
// whose allocation the disk refuses, for want of room, and a provisioned
// side whose record is gone (its row is an error, which feeds leg repair)
// both leave the verdict clean, since a re-sent SyncupSide would only repeat
// the refusal.
func TestAReadDoesNotNameWhatAResendCannotBuild(t *testing.T) {
	ctx := context.Background()
	t.Run("no room for the allocation", func(t *testing.T) {
		srv, _ := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		req := unprovisionedSideReq(1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE)
		req.SideConf.ExtCnt = 1 << 30
		req.SideConf.ZeroBytes = 1 << 30
		if _, err := srv.SyncupSide(ctx, req); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		reply, _ := checkSideNow(srv, ctx, &pb.CheckSideRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide), Revision: 1,
		}, nil)
		if reply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("verdict over a refused allocation = %v, want code 0",
				reply.GetAgentReply())
		}
	})
	t.Run("a provisioned side with no record", func(t *testing.T) {
		srv, _ := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		if _, err := srv.SyncupSide(ctx, sideReq(1, testSide, testCn0,
			[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		reply, info := checkSideNow(srv, ctx, &pb.CheckSideRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide), Revision: 1,
		}, nil)
		if info.GetSideDevInfo().GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
			t.Fatalf("side device row %v, want the record-missing error",
				info.GetSideDevInfo())
		}
		if reply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("verdict over a missing record = %v, want code 0",
				reply.GetAgentReply())
		}
	})
}

// TestAReadReDrivesAnAbsentPort is the port's rule (SH31) on the disk node: a
// CheckDn round that reads the agent's nvmet port or an ANA group of it
// absent names it, the re-sent SyncupDn makes it, and the round after is
// clean.
func TestAReadReDrivesAnAbsentPort(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"the port", ""},
		{"an ana group of the port", "/ana_groups/2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			portPath := agent.NvmetRoot + "/ports/1"
			node.mu.Lock()
			for path := range node.links {
				if strings.HasPrefix(path, portPath+"/subsystems/") {
					delete(node.links, path)
				}
			}
			node.cmdRmdir([]string{portPath + tc.dir})
			node.mu.Unlock()
			if tc.dir == "" && node.dirs[portPath] {
				t.Fatal("the port directory survived its rmdir")
			}
			reply, _ := checkDnNow(srv, ctx, &pb.CheckDnRequest{
				ClusterId: testCluster, DnId: testDn, Revision: 1,
			}, nil)
			if reply.GetAgentReply().GetCode() != common.ReplyCodeLeftover ||
				!strings.Contains(reply.GetAgentReply().GetDetails(),
					"nvmet port 1: absent") {
				t.Fatalf("check round = %v, want a leftover naming the port",
					reply.GetAgentReply())
			}
			// The side's exports read unlinked while the port is gone, which
			// only the SyncupDn cures: the side's verdict leaves them alone.
			if tc.dir == "" {
				sideReply, info := checkSideNow(srv, ctx,
					&pb.CheckSideRequest{
						ClusterId: testCluster, DnId: testDn,
						SidePointer: sidePtr(testSide), Revision: 1,
					}, nil)
				if row := info.GetCnIdToNvmeof()[testCn0]; row.GetDetails() !=
					detailsPortMissing {
					t.Fatalf("export row %v, want %q", row,
						detailsPortMissing)
				}
				if got := sideReply.GetAgentReply(); got.GetCode() != 0 {
					t.Fatalf("side verdict while the port is gone = %v, "+
						"want code 0", got)
				}
			}
			node.Reset()
			if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			if n := len(node.callsMatching("cmd mkdir -p " + portPath +
				tc.dir)); n == 0 {
				t.Fatalf("the re-sent SyncupDn did not make %s",
					portPath+tc.dir)
			}
			after, _ := checkDnNow(srv, ctx, &pb.CheckDnRequest{
				ClusterId: testCluster, DnId: testDn, Revision: 1,
			}, nil)
			if after.GetAgentReply().GetCode() != 0 {
				t.Fatalf("round after the rebuild = %v, want code 0",
					after.GetAgentReply())
			}
		})
	}
}

// TestAReadRebuildsAMigrationSourceExport: a migration source whose export is
// gone above a ready side, the per-CN stacks in place. The side's verdict
// names the export, and the re-sent SyncupSide builds it again.
func TestAReadRebuildsAMigrationSourceExport(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	srcNqn := nf.MigrSrcNqn(testCluster, testDn, testSp, testMigrId)
	subsysPath := agent.NvmetRoot + "/subsystems/" + srcNqn
	node.mu.Lock()
	gone := func(path string) bool {
		return path == subsysPath || strings.HasPrefix(path, subsysPath+"/") ||
			strings.HasSuffix(path, "/subsystems/"+srcNqn)
	}
	for path := range node.links {
		if gone(path) {
			delete(node.links, path)
		}
	}
	for path := range node.files {
		if gone(path) {
			delete(node.files, path)
		}
	}
	for path := range node.dirs {
		if gone(path) {
			delete(node.dirs, path)
		}
	}
	node.mu.Unlock()

	first, _ := checkSideNow(srv, ctx, &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
		Revision: 2,
	}, nil)
	if first.GetAgentReply().GetCode() != common.ReplyCodeLeftover ||
		!strings.Contains(first.GetAgentReply().GetDetails(),
			srcNqn+": absent") {
		t.Fatalf("side verdict = %v, want a leftover naming %s absent",
			first.GetAgentReply(), srcNqn)
	}
	node.Reset()
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if len(node.callsMatching("cmd mkdir -p "+subsysPath)) == 0 {
		t.Fatal("the re-sent SyncupSide did not build the export")
	}
	after, _ := checkSideNow(srv, ctx, &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
		Revision: 2,
	}, nil)
	if after.GetAgentReply().GetCode() != 0 {
		t.Fatalf("round after the rebuild = %v, want code 0",
			after.GetAgentReply())
	}
}

// TestAReadReDrivesAWrongAnaGroupState is the other half of the port's rule
// (SH31) on the disk node: an ANA group of the port in a state other than its
// fixed one, on a port whose transport attributes all match, is named, and the
// re-sent SyncupDn rewrites it. A transport attribute that differs is left to
// the row, beside a wrong group state too, and so is a probe that did not
// answer.
func TestAReadReDrivesAWrongAnaGroupState(t *testing.T) {
	ctx := context.Background()
	portPath := agent.NvmetRoot + "/ports/1"
	statePath := portPath + "/ana_groups/2/ana_state"
	details := fmt.Sprintf("ana group 2 is %q, want %q",
		agent.AnaStateOptimized, agent.AnaStateNonOptimized)
	check := func(srv *DnAgentServer) (*pb.AgentReply, *pb.DnInfo) {
		reply, info := checkDnNow(srv, ctx, &pb.CheckDnRequest{
			ClusterId: testCluster, DnId: testDn, Revision: 1,
		}, nil)
		return reply.GetAgentReply(), info
	}

	t.Run("its port's attributes match", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		node.mu.Lock()
		node.files[statePath] = agent.AnaStateOptimized
		node.mu.Unlock()
		reply, _ := check(srv)
		if reply.GetCode() != common.ReplyCodeLeftover ||
			!strings.Contains(reply.GetDetails(),
				"nvmet port 1: differs: "+details) {
			t.Fatalf("check round = %v, want a leftover naming %q",
				reply, details)
		}
		node.Reset()
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		if len(node.callsMatching("writedirect "+statePath+"="+
			agent.AnaStateNonOptimized)) != 1 {
			t.Fatalf("the re-sent SyncupDn did not rewrite %s", statePath)
		}
		if after, _ := check(srv); after.GetCode() != 0 {
			t.Fatalf("round after the rewrite = %v, want code 0", after)
		}
	})

	for _, tc := range []struct {
		name string
		arm  func(node *fakeNode)
		want string
	}{
		{"a transport attribute differs too", func(node *fakeNode) {
			node.files[statePath] = agent.AnaStateOptimized
			node.files[portPath+"/addr_traddr"] = "192.168.10.99"
		}, `addr_traddr is "192.168.10.99"`},
		{"the port directory's time read cut off", func(node *fakeNode) {
			node.killCmdAlways["mtime "+portPath] = true
		}, "mtime of " + portPath},
		{"an ana group read cut off", func(node *fakeNode) {
			node.killReadAlways[statePath] = true
		}, statePath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, 1, testSide)
			node.mu.Lock()
			tc.arm(node)
			node.mu.Unlock()
			reply, info := check(srv)
			if reply.GetCode() != 0 {
				t.Fatalf("check round = %v, want code 0", reply)
			}
			row := info.GetPortInfo()
			if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
				!strings.Contains(row.GetDetails(), tc.want) {
				t.Fatalf("port row %v, want an error containing %q",
					row, tc.want)
			}
		})
	}
}

// TestAReadDoesNotNameASideWhoseRecordDisagrees: a side whose record holds
// another extent count than its request, over a side device that is gone.
// AllocSide refuses that record on every pass, so the verdict leaves it to
// the converge's row.
func TestAReadDoesNotNameASideWhoseRecordDisagrees(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	req := sideReq(2, testSide, testCn0, []uint64{testCn1},
		pb.SpLevel_SP_LEVEL_READWRITE)
	req.SideConf.ExtCnt++
	req.SideConf.ZeroBytes = req.SideConf.GetExtCnt() * testExtentSize
	reply, err := srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !strings.Contains(reply.GetSideInfo().GetSideDevInfo().GetDetails(),
		"allocated") {
		t.Fatalf("side device row %v, want AllocSide's refusal",
			reply.GetSideInfo().GetSideDevInfo())
	}
	node.rebootKernel()
	check, info := checkSideNow(srv, ctx, &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
		Revision: 2,
	}, nil)
	if info.GetSideDevInfo().GetStatus() != pb.ResStatus_RES_STATUS_MISSING {
		t.Fatalf("fixture is wrong: side device row %v, want MISSING",
			info.GetSideDevInfo())
	}
	if got := check.GetAgentReply(); got.GetCode() != 0 {
		t.Fatalf("verdict over a record that disagrees = %v, want code 0", got)
	}
}

// TestAReadRebuildsAnExportWhosePortLinkFailed: a reboot after which the
// startup reconcile's mkdir of the port is refused once. The exports are
// built over a port that is not there, so each link fails. The DN's round
// names the port, its re-sent SyncupDn makes it, the side's round that
// follows names each export as not linked, and the re-sent SyncupSide links
// them.
func TestAReadRebuildsAnExportWhosePortLinkFailed(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	syncupBoth(t, srv, 1, testSide)
	stopTestServer(t, srv)
	node.rebootKernel()
	portPath := agent.NvmetRoot + "/ports/1"
	node.mu.Lock()
	node.failCmd["mkdir -p "+portPath] = "refused once"
	node.mu.Unlock()
	srv = startTestServer(t, node)

	linked := func(nqn string) bool {
		node.mu.Lock()
		defer node.mu.Unlock()
		_, ok := node.links[portPath+"/subsystems/"+nqn]
		return ok
	}
	cns := []uint64{testCn0, testCn1}
	for _, cnId := range cns {
		if linked(nf.SideToCnNqn(testCluster, testSp, testLeg, cnId)) {
			t.Fatal("fixture is wrong: an export linked under an absent port")
		}
	}
	firstSide, firstDn := worldRounds(t, srv, sideReq(1, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE), 3)
	if !strings.Contains(firstDn.GetDetails(), "nvmet port 1: absent") {
		t.Fatalf("first DN verdict = %v, want the port named", firstDn)
	}
	for _, cnId := range cns {
		nqn := nf.SideToCnNqn(testCluster, testSp, testLeg, cnId)
		if firstSide.GetCode() != common.ReplyCodeLeftover ||
			!strings.Contains(firstSide.GetDetails(),
				nqn+": absent: not linked to the port") {
			t.Fatalf("first side verdict = %v, want %s named as not linked",
				firstSide, nqn)
		}
		if !linked(nqn) {
			t.Fatalf("the re-sent SyncupSide did not link %s", nqn)
		}
	}
	last, _ := checkSideNow(srv, context.Background(), &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
		Revision: 1,
	}, nil)
	if last.GetAgentReply().GetCode() != 0 {
		t.Fatalf("round after the rebuild = %v, want code 0",
			last.GetAgentReply())
	}
}

// TestAReadDoesNotNameAWrongObject: an object that is there but differs
// from the wanted one reads ERROR, which feeds the side's health epoch, and
// the verdict leaves it to that row: it names absent objects only. So does a
// namespace whose enable read did not answer, which the loose probe reads as
// not enabled.
func TestAReadDoesNotNameAWrongObject(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	nqn := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn1)
	nsPath := agent.NvmetRoot + "/subsystems/" + nqn + "/namespaces/1"
	for _, tc := range []struct {
		name string
		arm  func(node *fakeNode)
		row  func(info *pb.SideInfo) *pb.ResInfo
	}{
		{"a dm-linear with another table", func(node *fakeNode) {
			linear := nf.DnLinearName(
				testCluster, testDn, testSp, testSide, testCn1)
			node.dms[linear].table = "0 8 linear 7:7 0"
		}, func(info *pb.SideInfo) *pb.ResInfo {
			return info.GetCnIdToDmLinear()[testCn1]
		}},
		{"an export in another ANA group", func(node *fakeNode) {
			node.files[nsPath+"/ana_grpid"] = "3"
		}, func(info *pb.SideInfo) *pb.ResInfo {
			return info.GetCnIdToNvmeof()[testCn1]
		}},
		{"an enable read that did not answer", func(node *fakeNode) {
			node.killReadAlways[nsPath+"/enable"] = true
		}, func(info *pb.SideInfo) *pb.ResInfo {
			return info.GetCnIdToNvmeof()[testCn1]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, 1, testSide)
			node.mu.Lock()
			tc.arm(node)
			node.mu.Unlock()
			reply, info := checkSideNow(srv, context.Background(),
				&pb.CheckSideRequest{
					ClusterId: testCluster, DnId: testDn,
					SidePointer: sidePtr(testSide), Revision: 1,
				}, nil)
			if row := tc.row(info); row.GetStatus() !=
				pb.ResStatus_RES_STATUS_ERROR {
				t.Fatalf("fixture is wrong: row %v, want ERROR", row)
			}
			if got := reply.GetAgentReply(); got.GetCode() != 0 {
				t.Fatalf("verdict = %v, want code 0", got)
			}
		})
	}
}

// TestAReadRebuildsAHalfBuiltExport: an export whose subsystem is there and
// whose namespace is off, or gone, is named with the part's words, and the
// re-sent SyncupSide builds the part again.
func TestAReadRebuildsAHalfBuiltExport(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	nqn := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn1)
	nsPath := agent.NvmetRoot + "/subsystems/" + nqn + "/namespaces/1"
	for _, tc := range []struct {
		name  string
		arm   func(node *fakeNode)
		words string
	}{
		{"its namespace off", func(node *fakeNode) {
			node.files[nsPath+"/enable"] = "0"
		}, detailsNsNotEnabled},
		{"its namespace gone", func(node *fakeNode) {
			for _, m := range []map[string]bool{node.dirs} {
				for path := range m {
					if path == nsPath || strings.HasPrefix(path, nsPath+"/") {
						delete(m, path)
					}
				}
			}
			for path := range node.files {
				if strings.HasPrefix(path, nsPath+"/") {
					delete(node.files, path)
				}
			}
		}, detailsNsMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			node.mu.Lock()
			tc.arm(node)
			node.mu.Unlock()
			check := func() *pb.AgentReply {
				reply, _ := checkSideNow(srv, ctx, &pb.CheckSideRequest{
					ClusterId: testCluster, DnId: testDn,
					SidePointer: sidePtr(testSide), Revision: 1,
				}, nil)
				return reply.GetAgentReply()
			}
			first := check()
			if first.GetCode() != common.ReplyCodeLeftover ||
				!strings.Contains(first.GetDetails(),
					nqn+": absent: "+tc.words) {
				t.Fatalf("side verdict = %v, want %s named with %q",
					first, nqn, tc.words)
			}
			if _, err := srv.SyncupSide(ctx, sideReq(1, testSide, testCn0,
				[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if after := check(); after.GetCode() != 0 {
				t.Fatalf("round after the rebuild = %v, want code 0", after)
			}
		})
	}
}

// TestAReadDoesNotNameASideOnAnotherNodesDisk: a side not provisioned, with
// no record, on a disk whose header names another node. Its row reads
// MISSING, but AllocSide refuses another node's disk, so the verdict leaves
// it alone.
func TestAReadDoesNotNameASideOnAnotherNodesDisk(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
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
	dnRequest := dnReq(1, testSide2)
	dnRequest.DnId = testDn + 100
	if _, err := srv2.SyncupDn(ctx, dnRequest); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	sideRequest := unprovisionedSideReq(1, testSide2, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)
	sideRequest.DnId = testDn + 100
	if _, err := srv2.SyncupSide(ctx, sideRequest); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	reply, info := checkSideNow(srv2, ctx, &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn + 100,
		SidePointer: sidePtr(testSide2), Revision: 1,
	}, nil)
	if info.GetSideDevInfo().GetStatus() != pb.ResStatus_RES_STATUS_MISSING {
		t.Fatalf("fixture is wrong: side device row %v, want MISSING",
			info.GetSideDevInfo())
	}
	if got := reply.GetAgentReply(); got.GetCode() != 0 {
		t.Fatalf("verdict on another node's disk = %v, want code 0", got)
	}
}
