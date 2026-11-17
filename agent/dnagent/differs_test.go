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

// The converge a migration destination's cutover brings checks the side
// device first; a `dmsetup table` of it that does not answer stops the pass
// there, before the destination connects to the old side, builds its
// dm-clone and serves through it (review item 4). That converge names the
// side device in its code, and the read after it names what the stop left
// undone, so the worker sends the syncup again and its converge finishes the
// cutover (SH31, DN16).
func TestACutoverStoppedAtTheSideDeviceIsNamed(t *testing.T) {
	srv, node := newTestServer(t)
	nf := srv.nf
	ctx := context.Background()
	names := newMigrDstNames()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	provisionSide(t, srv, 1, testSide)
	waits := migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)
	waits.MigrDstConf.CutoverPending = true
	if _, err := srv.SyncupSide(ctx, waits); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	sideDev := nf.DnSideName(testCluster, testDn, testSp, testSide)

	setHook(node, node.killCmd, "dmsetup table "+sideDev)
	reply, err := srv.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if servesThroughClone(t, srv, node, names) {
		t.Fatal("fixture is wrong: the cutover's converge did not stop " +
			"at the side device")
	}
	if got := reply.GetAgentReply(); got.GetCode() !=
		common.ReplyCodeLeftover ||
		!strings.Contains(got.GetDetails(), sideDev+": differs:") {
		t.Errorf("the cutover's converge answered %v, want the leftover "+
			"code naming the side device", got)
	}
	round, _ := checkSideNow(srv, ctx, &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn,
		SidePointer: sidePtr(testSide), Revision: 3,
	}, nil)
	if got := round.GetAgentReply(); got.GetCode() !=
		common.ReplyCodeLeftover {
		t.Errorf("the read after it answered %v, want the leftover code", got)
	}

	if reply, err = srv.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !servesThroughClone(t, srv, node, names) ||
		reply.GetAgentReply().GetCode() != 0 {
		t.Errorf("the syncup sent again: serves %v, code %v; want the "+
			"cutover finished with code zero",
			servesThroughClone(t, srv, node, names), reply.GetAgentReply())
	}
}

// A migration destination's dm-clone whose metadata is read-only, or failed,
// reads RES_STATUS_ERROR with its mode named (review item 22), on both
// channels, and the side's verdict does not name it: no converge brings a
// dm-clone back to read-write mode, and a reload of its table would reopen
// its metadata from its last commit and read the regions copied since from
// the old side again.
func TestADmCloneWithBrokenMetadataReadsErrorAndIsNotNamed(t *testing.T) {
	for _, mode := range []string{
		common.CloneMetadataReadOnly, common.CloneMetadataFail,
	} {
		t.Run(mode, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			names := newMigrDstNames()
			if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			provisionSide(t, srv, 1, testSide)
			if _, err := srv.SyncupSide(ctx, migrDstReq(2,
				pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if !servesThroughClone(t, srv, node, names) {
				t.Fatal("fixture is wrong: the destination does not serve " +
					"through its dm-clone")
			}
			node.mu.Lock()
			node.dms[names.clone].metaMode = mode
			node.mu.Unlock()

			reply, err := srv.SyncupSide(ctx, migrDstReq(2,
				pb.SpLevel_SP_LEVEL_READWRITE))
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			round, info := checkSideNow(srv, ctx, &pb.CheckSideRequest{
				ClusterId: testCluster, DnId: testDn,
				SidePointer: sidePtr(testSide), Revision: 2,
			}, nil)
			for _, ch := range []struct {
				name string
				row  *pb.ResInfo
				code *pb.AgentReply
			}{
				{"the converge", reply.GetSideInfo().GetMigrDstInfo().
					GetDmCloneInfo(), reply.GetAgentReply()},
				{"the read", info.GetMigrDstInfo().GetDmCloneInfo(),
					round.GetAgentReply()},
			} {
				if ch.row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
					!strings.HasPrefix(ch.row.GetDetails(),
						detailsCloneMetaMode+mode) {
					t.Errorf("%s's dm-clone row = %v/%q, want ERROR naming "+
						"the mode", ch.name, ch.row.GetStatus(),
						ch.row.GetDetails())
				}
				if ch.code.GetCode() != 0 {
					t.Errorf("%s answered %v, want code zero", ch.name,
						ch.code)
				}
			}
			assertNothingRunsBetweenCalls(t, node)
		})
	}
}

// A port transport attribute that differs while no subsystem is linked to
// the port is named by the DN's read (SH31, entry 4.20), since nvmet then
// takes the write, and the re-sent SyncupDn rewrites it. With a subsystem
// linked the port row alone says so: nvmet refuses the write then.
func TestADnPortSettingThatDiffersWithNothingLinkedReDrives(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	portPath := portPathOf(common.NvmetPortId)
	node.mu.Lock()
	want := node.files[portPath+"/addr_traddr"]
	node.files[portPath+"/addr_traddr"] = "192.168.10.99"
	node.mu.Unlock()

	dnRound := func() *pb.CheckDnReply {
		reply, _ := checkDnNow(srv, ctx, &pb.CheckDnRequest{
			ClusterId: testCluster, DnId: testDn, Revision: 1,
		}, nil)
		return reply
	}
	round := dnRound()
	if got := round.GetAgentReply(); got.GetCode() !=
		common.ReplyCodeLeftover || !strings.Contains(got.GetDetails(),
		`differs: addr_traddr is "192.168.10.99"`) {
		t.Fatalf("the DN's read answered %v, want the leftover code naming "+
			"the port", got)
	}
	if _, err := srv.SyncupDn(ctx, dnReq(1)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.mu.Lock()
	got := node.files[portPath+"/addr_traddr"]
	node.mu.Unlock()
	if got != want {
		t.Errorf("addr_traddr after the re-sent SyncupDn = %q, want %q",
			got, want)
	}
	if round = dnRound(); round.GetAgentReply().GetCode() != 0 {
		t.Errorf("the read after the rewrite answered %v, want code zero",
			round.GetAgentReply())
	}
}

// A configfs read that does not answer is never read as an empty value
// (review item 42): a syncup sent again at the same revision whose read of a
// live export's device_path does not answer stops that export's step, and
// does not disable the namespace of a live export, which the loose read's
// empty path, beside an enable that answered, made it do (SH16, SH17). The
// step's export is read again after it stopped, as a read reads it, and is
// found as wanted.
func TestAnUnansweredExportReadDisablesNothing(t *testing.T) {
	srv, node := newTestServer(t)
	nf := srv.nf
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	nqn := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0)
	nsPath := srv.nvmet.NsPath(nqn, sideNsid)

	node.Reset()
	setHook(node, node.killRead, nsPath+"/device_path")
	reply, err := srv.SyncupSide(ctx, sideReq(1, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	armed := node.killRead[nsPath+"/device_path"]
	node.mu.Unlock()
	if armed {
		t.Fatal("fixture is wrong: the device_path was never read")
	}
	for _, call := range node.Calls() {
		if strings.Contains(call, nsPath+"/enable=0") {
			t.Fatalf("the pass disabled the live export's namespace: %q",
				call)
		}
	}
	// The step that stopped is read again as a read reads it, and the
	// export is as wanted: an OK row, nothing named.
	if row := reply.GetSideInfo().GetCnIdToNvmeof()[testCn0]; row.
		GetStatus() != pb.ResStatus_RES_STATUS_OK ||
		reply.GetAgentReply().GetCode() != 0 {
		t.Errorf("the export's row = %v/%q, code %v; want OK and code zero",
			row.GetStatus(), row.GetDetails(), reply.GetAgentReply())
	}
}

// A migration source's step 1 moves every per-CN namespace to the
// inaccessible group before the fence suspends the dm-linears
// (architecture.md, Migration, src step 1). It reads the group alone, so a
// read of another attribute that fails moves it all the same, and a read of
// the group that fails tries the move: inaccessible is the safe direction.
func TestASourceStepOneMovesWhenANamespaceReadFails(t *testing.T) {
	srv, node := newTestServer(t)
	nf := srv.nf
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	nqn0 := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0)
	nqn1 := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn1)

	node.Reset()
	setHook(node, node.failRead, srv.nvmet.NsPath(nqn0, sideNsid)+
		"/device_uuid")
	setHook(node, node.failRead, srv.nvmet.NsPath(nqn1, sideNsid)+
		"/ana_grpid")
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	// The primary's dm-linear is the one the fence suspends; the standby's
	// is on its dm-error already.
	suspend := "cmd dmsetup suspend " + nf.DnLinearName(
		testCluster, testDn, testSp, testSide, testCn0)
	for _, nqn := range []string{nqn0, nqn1} {
		assertOrder(t, node,
			fmt.Sprintf("writedirect %s/subsystems/%s/namespaces/1/"+
				"ana_grpid=%d", agent.NvmetRoot, nqn,
				common.AnaGrpIdInaccessible),
			suspend)
	}
}

// A migration source whose link of its export fails because the node's
// nvmet port is not there reads that on the converge as the read does, "the
// port is not there", and its code names it not: the DN's verdict names the
// port, and the side is not sent again for the link alone (SH31).
func TestASourceExportOverAMissingPortIsNotNamed(t *testing.T) {
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
	node.cmdRmdir([]string{portPath})
	node.mu.Unlock()

	reply, err := srv.SyncupSide(ctx, migrSrcReq(2))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	row := reply.GetSideInfo().GetMigrSrcInfo().GetNvmeofInfo()
	if row.GetDetails() != detailsPortMissing {
		t.Fatalf("the source export's row is %v, want %q", row,
			detailsPortMissing)
	}
	if strings.Contains(reply.GetAgentReply().GetDetails(),
		row.GetResName()) {
		t.Errorf("the converge named the source export: %v",
			reply.GetAgentReply())
	}
}
