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

const (
	testCluster    = uint64(0xebada5168620c5fe)
	testDn         = uint64(3)
	testSp         = uint64(0x11)
	testLeg        = uint64(0x15)
	testSide       = uint64(0x16)
	testSide2      = uint64(0x17)
	testCn0        = uint64(5)
	testCn1        = uint64(6)
	testDisk       = "/dev/fake-disk"
	testExtentSize = uint64(1 << 20)
	testExtCnt     = uint64(10)
	testBlockSize  = uint64(1 << 20)
	testMigrId     = uint64(0x21)
	testSrcDn      = uint64(4)
)

func testTrConf() *pb.NvmeTrConf {
	return &pb.NvmeTrConf{
		TrType:  "tcp",
		AdrFam:  "ipv4",
		TrAddr:  "192.168.0.20",
		TrSvcId: "4200",
	}
}

func newTestServer(t *testing.T) (*DnAgentServer, *fakeNode) {
	t.Helper()
	node := newFakeNode()
	// Must exceed DnDataOffset: at testExtentSize (1 MiB) that leaves 256
	// data extents.
	node.devSize[testDisk] = 512 << 20
	node.devNo[testDisk] = "253:0"
	node.dirs[agent.NvmetRoot] = true

	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	srv := NewDnAgentServer(node.osClient(), nf,
		common.DefaultLocalStorPrefix, testDisk, testTrConf())
	// The §11.2 cutover grace window is off unless a test asks for it: no
	// unit test can wait common.SuspendSeconds, and with it off a migration
	// source fences straight onto its dm-errors, which is the end state
	// every other test cares about. TestMigrationSourceFence covers the
	// window itself, and TestFenceWindowDefault pins the production value.
	srv.fenceWait = 0
	if err := srv.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	node.Reset()
	return srv, node
}

func sidePtr(sideId uint64) *pb.SidePointer {
	return &pb.SidePointer{SpId: testSp, LegId: testLeg, SideId: sideId}
}

func dnReq(revision uint64, sideIds ...uint64) *pb.SyncupDnRequest {
	ptrs := make([]*pb.SidePointer, 0, len(sideIds))
	for _, sideId := range sideIds {
		ptrs = append(ptrs, sidePtr(sideId))
	}
	return &pb.SyncupDnRequest{
		ClusterId:       testCluster,
		DnId:            testDn,
		Revision:        revision,
		SidePointerList: ptrs,
		ExtentSize:      testExtentSize,
	}
}

func sideReq(
	revision uint64,
	sideId uint64,
	primary uint64,
	standbys []uint64,
	level pb.SpLevel,
) *pb.SyncupSideRequest {
	return &pb.SyncupSideRequest{
		ClusterId:   testCluster,
		DnId:        testDn,
		SidePointer: sidePtr(sideId),
		Revision:    revision,
		SideConf: &pb.SyncupSideRequest_SideConf{
			ExtCnt:        testExtCnt,
			CntlidSlot:    1,
			PrimaryCnId:   primary,
			StandbyIdList: standbys,
			SpLevel:       level,
		},
	}
}

// syncupBoth brings a DN and one side to a converged state.
func syncupBoth(
	t *testing.T,
	srv *DnAgentServer,
	revision uint64,
	sideId uint64,
) {
	t.Helper()
	ctx := context.Background()
	dnReply, err := srv.SyncupDn(ctx, dnReq(revision, sideId))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if dnReply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn rejected: %v", dnReply.GetAgentReply())
	}
	sideReply, err := srv.SyncupSide(ctx,
		sideReq(revision, sideId, testCn0, []uint64{testCn1},
			pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if sideReply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupSide rejected: %v", sideReply.GetAgentReply())
	}
}

// assertOrder matches wanted as a *subsequence* of the recorded calls: each
// entry is found at or after the position of the previous one, so a sequence
// may legitimately name the same call shape twice (the two volume-table slot
// writes bracketing a trim, say).
func assertOrder(t *testing.T, node *fakeNode, wanted ...string) {
	t.Helper()
	previous := -1
	for _, want := range wanted {
		idx := node.indexOfCallFrom(want, previous+1)
		if idx < 0 {
			t.Fatalf("missing call %q after index %d in:\n%s",
				want, previous, strings.Join(node.Calls(), "\n"))
		}
		previous = idx
	}
}

// ---------------------------------------------------------------------------
// 1. Fresh SyncupDn (DN5)
// ---------------------------------------------------------------------------

func TestFreshSyncupDn(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	portPath := fmt.Sprintf("%s/ports/%d", agent.NvmetRoot, common.NvmetPortId)

	reply, err := srv.SyncupDn(context.Background(), dnReq(1))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	if reply.GetRevision() != 1 {
		t.Fatalf("revision = %d, want 1", reply.GetRevision())
	}

	assertOrder(t, node,
		fmt.Sprintf("readblock %s off=%d len=%d",
			testDisk, common.DnHeaderOffset, common.DnHeaderSize),
		fmt.Sprintf("writeblock %s off=%d",
			testDisk, common.DnTableSlotAOffset),
		fmt.Sprintf("writeblock %s off=%d", testDisk, common.DnHeaderOffset),
		"cmd mkdir -p "+portPath,
		"writedirect "+portPath+"/addr_trtype=tcp",
		"writedirect "+portPath+"/addr_traddr=192.168.0.20",
		"cmd mkdir -p "+portPath+"/ana_groups/2",
		"cmd mkdir -p "+portPath+"/ana_groups/3",
		"writedirect "+portPath+"/ana_groups/1/ana_state=optimized",
		"writedirect "+portPath+"/ana_groups/2/ana_state=non-optimized",
		"writedirect "+portPath+"/ana_groups/3/ana_state=inaccessible",
		"writeproto "+nf.LocalDnPath(testCluster, testDn),
	)
	// LVM is gone from the dn agent entirely ([D13]).
	for _, call := range node.Calls() {
		for _, banned := range []string{
			"pvcreate", "vgcreate", "lvcreate", "lvremove", "lvchange",
			"cmd lvs ", "cmd vgs ", "cmd pvs ",
		} {
			if strings.Contains(call, banned) {
				t.Errorf("an LVM command survived: %s", call)
			}
		}
	}

	for _, field := range []struct {
		name string
		info *pb.ResInfo
	}{
		{"disk", reply.GetDnInfo().GetDiskInfo()},
		{"meta", reply.GetDnInfo().GetMetaInfo()},
		{"port", reply.GetDnInfo().GetPortInfo()},
	} {
		if field.info.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("%s: status %v, details %q",
				field.name, field.info.GetStatus(), field.info.GetDetails())
		}
	}
	if got := reply.GetDnInfo().GetMetaInfo().GetResName(); got != testDisk {
		t.Errorf("meta res_name = %q, want %q", got, testDisk)
	}
	if got := reply.GetDnInfo().GetMetaInfo().GetDetails(); !strings.HasPrefix(
		got, "seq=1 sides=0") {
		t.Errorf("meta details = %q", got)
	}
	if got := reply.GetDnInfo().GetPortInfo().GetResName(); got != "1" {
		t.Errorf("port res_name = %q, want \"1\"", got)
	}
}

// ---------------------------------------------------------------------------
// 2. Revision gate (SH8)
// ---------------------------------------------------------------------------

func TestRevisionGate(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()

	if _, err := srv.SyncupDn(ctx, dnReq(5)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}

	node.Reset()
	reply, err := srv.SyncupDn(ctx, dnReq(4))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != common.ReplyCodeStaleRevision {
		t.Fatalf("code = %d, want ReplyCodeStaleRevision",
			reply.GetAgentReply().GetCode())
	}
	if reply.GetRevision() != 5 {
		t.Errorf("revision = %d, want the stored 5", reply.GetRevision())
	}
	if got := node.Mutations(); len(got) != 0 {
		t.Errorf("stale revision issued mutations: %v", got)
	}

	node.Reset()
	if _, err := srv.SyncupDn(ctx, dnReq(5)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if !node.hasCall("writeproto") {
		t.Error("equal revision did not re-persist")
	}
	for _, call := range node.Mutations() {
		if !strings.HasPrefix(call, "writeproto") {
			t.Errorf("equal revision mutated a converged dn: %s", call)
		}
	}

	node.Reset()
	reply, err = srv.SyncupDn(ctx, dnReq(6))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if reply.GetRevision() != 6 {
		t.Errorf("revision = %d, want 6", reply.GetRevision())
	}
}

// ---------------------------------------------------------------------------
// 3. Probe-first idempotency (SH16)
// ---------------------------------------------------------------------------

func TestSyncupSideIdempotent(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)

	node.Reset()
	reply, err := srv.SyncupSide(context.Background(),
		sideReq(1, testSide, testCn0, []uint64{testCn1},
			pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	for _, call := range node.Mutations() {
		if strings.HasPrefix(call, "writeproto") {
			continue // SH5: the request itself is re-persisted
		}
		t.Errorf("converged side issued a mutating call: %s", call)
	}
	if reply.GetSideInfo().GetSideDevInfo().GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("side_dev status = %v",
			reply.GetSideInfo().GetSideDevInfo().GetStatus())
	}
	for cnId, info := range reply.GetSideInfo().GetCnIdToNvmeof() {
		if info.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("cn %d nvmeof: %v %q",
				cnId, info.GetStatus(), info.GetDetails())
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Pointer diff (DN6, DN8)
// ---------------------------------------------------------------------------

func TestUnknownSidePointerRejected(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.Reset()
	reply, err := srv.SyncupSide(ctx,
		sideReq(1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != common.ReplyCodeUnknownObject {
		t.Fatalf("code = %d, want ReplyCodeUnknownObject",
			reply.GetAgentReply().GetCode())
	}
	if got := node.Mutations(); len(got) != 0 {
		t.Errorf("unknown pointer issued mutations: %v", got)
	}
}

func TestSyncupDnTearsDownRemovedSide(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	syncupBoth(t, srv, 1, testSide)

	node.Reset()
	if _, err := srv.SyncupDn(context.Background(), dnReq(2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	// Top-down: nvmet exports, then the per-CN dm devices, then the side
	// device, then its allocation record, then the files.
	assertOrder(t, node,
		"cmd rmdir "+agent.NvmetRoot+"/subsystems/"+
			nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0),
		"cmd dmsetup remove "+
			nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0),
		"cmd dmsetup remove "+
			nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0),
		"cmd dmsetup remove "+
			nf.DnSideName(testCluster, testDn, testSp, testSide),
		fmt.Sprintf("writeblock %s off=", testDisk),
		"cmd rm -f "+
			nf.LocalSidePath(testCluster, testDn, testSp, testSide),
	)
	if _, ok, _ := srv.meta.LookupSide(
		context.Background(), testSp, testSide); ok {
		t.Error("the allocation record survived teardown")
	}
	if srv.getSide(sideKey(testCluster, testDn, testSp, testSide)) != nil {
		t.Error("side state survived teardown")
	}
	if _, ok := node.protos[nf.LocalSidePath(
		testCluster, testDn, testSp, testSide)]; ok {
		t.Error("side state file survived teardown")
	}
}

// ---------------------------------------------------------------------------
// 5. Trim protocol (§9.4, DN9)
// ---------------------------------------------------------------------------

func TestTrimProtocol(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	sideDevPath := nf.DmPath(sideDevName)

	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.Reset()
	reply, err := srv.SyncupSide(ctx,
		sideReq(1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	// §9.4 on the [D13] layout: the record is persisted untrimmed, the
	// aggregate device is built, the whole device is discarded, and only
	// then does the trim flag reach the disk.
	// The record is persisted (slot B — the format wrote slot A), the
	// aggregate device is built, the whole device is discarded, and only then
	// does the trim flag reach the disk (slot A again).
	assertOrder(t, node,
		fmt.Sprintf("writeblock %s off=%d",
			testDisk, common.DnTableSlotBOffset),
		"cmd dmsetup create "+sideDevName,
		"cmd blkdiscard --force "+sideDevPath,
		fmt.Sprintf("writeblock %s off=%d",
			testDisk, common.DnTableSlotAOffset),
	)
	// The device is a single run of testExtCnt extents at the data area.
	wantOffset := common.DnDataOffset / agent.SectorSize
	wantLen := testExtCnt * testExtentSize / agent.SectorSize
	wantTable := fmt.Sprintf("stdin=0 %d linear 253:0 %d",
		wantLen, wantOffset)
	if !node.hasCall("cmd dmsetup create " + sideDevName + " " + wantTable) {
		t.Errorf("side device table is not %q; calls:\n%s",
			wantTable, strings.Join(node.Calls(), "\n"))
	}

	trimIdx := node.indexOfCall("cmd blkdiscard --force " + sideDevPath)
	nvmetIdx := node.indexOfCall(agent.NvmetRoot + "/subsystems/")
	if nvmetIdx >= 0 && nvmetIdx < trimIdx {
		t.Error("an nvmet command ran before the side device was trimmed")
	}

	// Clear the trim flag behind the agent's back: the probe must report the
	// error, and the next converge must redo steps 2-3.
	clearTrimmed(t, srv, node)
	infoReply, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	devInfo := infoReply.GetSideInfo().GetSideDevInfo()
	if devInfo.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		devInfo.GetDetails() != tagNotTrimmed {
		t.Fatalf("side_dev info = %v/%q, want ERROR/not_trimmed",
			devInfo.GetStatus(), devInfo.GetDetails())
	}

	node.Reset()
	if _, err := srv.SyncupSide(ctx,
		sideReq(1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertOrder(t, node,
		"cmd blkdiscard --force "+sideDevPath,
		fmt.Sprintf("writeblock %s off=", testDisk),
	)
	rec, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide)
	if !ok || !rec.GetTrimmed() {
		t.Error("the re-run did not re-set the trim flag")
	}
}

// clearTrimmed puts the side's on-disk record back into the untrimmed state —
// "somebody untrimmed the side behind our back" — and makes the server re-read
// the disk. It goes through a second, separately verified DiskMeta so it
// exercises the same API the agent does.
func clearTrimmed(t *testing.T, srv *DnAgentServer, node *fakeNode) {
	t.Helper()
	ctx := context.Background()
	other := newVerifiedMeta(t, node)
	if err := other.FreeSide(ctx, testSp, testSide); err != nil {
		t.Fatalf("FreeSide: %v", err)
	}
	if _, err := other.AllocSide(
		ctx, testSp, testSide, testExtCnt); err != nil {
		t.Fatalf("AllocSide: %v", err)
	}
	srv.meta = newVerifiedMeta(t, node)
}

// newVerifiedMeta opens the test disk the way the agent does: read the header,
// confirm the identity, learn the raw size.
func newVerifiedMeta(t *testing.T, node *fakeNode) *DiskMeta {
	t.Helper()
	meta := NewDiskMeta(node.osClient(), testDisk)
	meta.SetDiskSize(node.devSize[testDisk])
	if err := meta.EnsureFormatted(context.Background(),
		testCluster, testDn, testExtentSize); err != nil {
		t.Fatalf("EnsureFormatted: %v", err)
	}
	return meta
}

// ---------------------------------------------------------------------------
// 6. ANA moves ([D4], SH18)
// ---------------------------------------------------------------------------

func TestPrimaryFlipRewritesOnlyAnaGrpIds(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	syncupBoth(t, srv, 1, testSide)

	node.Reset()
	reply, err := srv.SyncupSide(context.Background(),
		sideReq(2, testSide, testCn1, []uint64{testCn0},
			pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}

	nqn0 := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0)
	nqn1 := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn1)
	wantWrites := map[string]bool{
		fmt.Sprintf("writedirect %s/subsystems/%s/namespaces/1/ana_grpid=%d",
			agent.NvmetRoot, nqn0, common.AnaGrpIdNonOptimized): false,
		fmt.Sprintf("writedirect %s/subsystems/%s/namespaces/1/ana_grpid=%d",
			agent.NvmetRoot, nqn1, common.AnaGrpIdOptimized): false,
	}
	for _, call := range node.Calls() {
		if !strings.HasPrefix(call, "writedirect ") {
			continue
		}
		if _, ok := wantWrites[call]; ok {
			wantWrites[call] = true
			continue
		}
		t.Errorf("unexpected configfs write: %s", call)
	}
	for call, seen := range wantWrites {
		if !seen {
			t.Errorf("missing ana transition: %s", call)
		}
	}
	// The two dm-linear tables are reloaded; nothing else about the
	// exports changes.
	for _, name := range []string{
		nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0),
		nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1),
	} {
		if !node.hasCall("cmd dmsetup reload " + name) {
			t.Errorf("dm-linear %s was not reloaded", name)
		}
	}
}

func TestNoWriteFileOnConfigfsAndNoAnaStateRewrite(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	syncupBoth(t, srv, 2, testSide)
	if _, err := srv.SyncupSide(context.Background(),
		sideReq(3, testSide, testCn1, []uint64{testCn0},
			pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	anaStateWrites := 0
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "write ") &&
			strings.Contains(call, "/sys/kernel/config") {
			t.Errorf("WriteFile used on configfs: %s", call)
		}
		if strings.HasPrefix(call, "writedirect ") &&
			strings.Contains(call, "/ana_state=") {
			anaStateWrites++
		}
	}
	// Exactly the three one-time writes of the port setup.
	if anaStateWrites != 3 {
		t.Errorf("ana_state writes = %d, want 3 (port setup only)",
			anaStateWrites)
	}
}

// ---------------------------------------------------------------------------
// 9. sp_level (DN11)
// ---------------------------------------------------------------------------

func TestSpLevels(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)

	nqn := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)

	// SP_LEVEL_NO_SIDE: exports gone, dm and the side device kept.
	if _, err := srv.SyncupSide(ctx, sideReq(2, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_NO_SIDE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if node.dirs[agent.NvmetRoot+"/subsystems/"+nqn] {
		t.Error("SP_LEVEL_NO_SIDE kept the subsystem")
	}
	if _, ok := node.dms[linName]; !ok {
		t.Error("SP_LEVEL_NO_SIDE removed the dm-linear")
	}
	if _, ok := node.dms[sideDevName]; !ok {
		t.Error("SP_LEVEL_NO_SIDE removed the side device")
	}

	// SP_LEVEL_DISABLE: only the side device and its record remain.
	if _, err := srv.SyncupSide(ctx, sideReq(3, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_DISABLE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if _, ok := node.dms[linName]; ok {
		t.Error("SP_LEVEL_DISABLE kept the dm-linear")
	}
	if _, ok := node.dms[sideDevName]; !ok {
		t.Error("SP_LEVEL_DISABLE removed the side device")
	}
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); !ok {
		t.Error("SP_LEVEL_DISABLE freed the allocation record")
	}

	// Lowering back rebuilds everything.
	reply, err := srv.SyncupSide(ctx, sideReq(4, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !node.dirs[agent.NvmetRoot+"/subsystems/"+nqn] {
		t.Error("lowering the level did not rebuild the subsystem")
	}
	if _, ok := node.dms[linName]; !ok {
		t.Error("lowering the level did not rebuild the dm-linear")
	}
	for cnId, info := range reply.GetSideInfo().GetCnIdToNvmeof() {
		if info.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("cn %d nvmeof after rebuild: %v %q",
				cnId, info.GetStatus(), info.GetDetails())
		}
	}
}

// TestReadOnlyLevelIsNoOpOnDn pins [P1]/[D11]: SP_LEVEL_READONLY — and every
// CN-only level below SP_LEVEL_NO_MIGRATION — has no DN-side behavior at all.
// Read-only is enforced on the CN's user-facing namespaces; the DN cannot
// enforce it (nvmet needs a writeable backing bdev, dm remaps bypass mid-stack
// read-only flags, and md metadata/resync writes must keep flowing).
func TestReadOnlyLevelIsNoOpOnDn(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)

	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)

	revision := uint64(1)
	for _, level := range []pb.SpLevel{
		pb.SpLevel_SP_LEVEL_READONLY,
		pb.SpLevel_SP_LEVEL_NO_CLONE,
		pb.SpLevel_SP_LEVEL_NO_THINPOOL,
		pb.SpLevel_SP_LEVEL_NO_REDUND,
	} {
		revision++
		node.Reset()
		reply, err := srv.SyncupSide(ctx,
			sideReq(revision, testSide, testCn0, []uint64{testCn1}, level))
		if err != nil {
			t.Fatalf("SyncupSide(%v): %v", level, err)
		}
		if reply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("%v rejected: %v", level, reply.GetAgentReply())
		}
		for _, call := range node.Mutations() {
			if strings.HasPrefix(call, "writeproto") {
				continue // SH5: the request itself is re-persisted
			}
			t.Errorf("%v mutated the dn: %s", level, call)
		}
		for cnId, info := range reply.GetSideInfo().GetCnIdToNvmeof() {
			if info.GetStatus() != pb.ResStatus_RES_STATUS_OK {
				t.Errorf("cn %d export at %v: %v %q",
					cnId, level, info.GetStatus(), info.GetDetails())
			}
		}
		if _, ok := node.dms[sideDevName]; !ok {
			t.Errorf("%v removed the side device", level)
		}
		if node.dms[linName].readOnly {
			t.Errorf("%v made the dm-linear read-only", level)
		}
		if node.dms[sideDevName].readOnly {
			t.Errorf("%v made the side device read-only", level)
		}
	}
}

// ---------------------------------------------------------------------------
// 12. The orphan sweep is pointer-list driven ([D13], [P4])
// ---------------------------------------------------------------------------

// [P4]: the on-disk volume table, not --local-store, is authoritative for
// extent placement — "a node that loses --local-store but keeps the disk
// recovers exactly as it did with LVM". The sweep must therefore never treat
// "no local state for this side" as proof that the side is gone: doing so
// frees its extents, and the next SyncupSide re-runs the §9.4 trim protocol
// and blkdiscards live data.
func TestLocalStoreLossKeepsSideAllocation(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	syncupBoth(t, srv, 1, testSide)

	before, ok, err := srv.meta.LookupSide(ctx, testSp, testSide)
	if err != nil || !ok {
		t.Fatalf("no record after the first converge: %v %v", ok, err)
	}
	wantRuns := fmt.Sprintf("%v", before.GetRunList())

	// The agent restarts with an empty --local-store; the disk is intact.
	node.mu.Lock()
	node.protos = map[string][]byte{}
	node.mu.Unlock()
	srv2 := NewDnAgentServer(node.osClient(), nf,
		common.DefaultLocalStorPrefix, testDisk, testTrConf())
	if err := srv2.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Reconcile has no DN state at all, so it may sweep nothing.
	if _, ok, _ := srv2.meta.LookupSide(ctx, testSp, testSide); !ok {
		t.Fatal("reconcile with an empty store freed the side's extents")
	}

	// The CP re-syncs. SyncupDn reintroduces the pointer (DN8 ordering); the
	// side's state has not arrived yet, which is exactly the window the old
	// sweep got wrong.
	node.Reset()
	if _, err := srv2.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	after, ok, err := srv2.meta.LookupSide(ctx, testSp, testSide)
	if err != nil || !ok {
		t.Fatalf("SyncupDn swept a side still in the pointer list: %v %v",
			ok, err)
	}
	if got := fmt.Sprintf("%v", after.GetRunList()); got != wantRuns {
		t.Errorf("extent runs moved across the restart: %s, want %s",
			got, wantRuns)
	}
	if !after.GetTrimmed() {
		t.Error("the trim flag was lost across the restart")
	}
	if node.hasCall("cmd dmsetup remove " + nf.DnSideName(
		testCluster, testDn, testSp, testSide)) {
		t.Error("SyncupDn removed a live side device")
	}

	// And the re-sent SyncupSide reuses the allocation instead of
	// re-trimming it.
	node.Reset()
	if _, err := srv2.SyncupSide(ctx, sideReq(1, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	for _, call := range node.Calls() {
		if strings.Contains(call, "blkdiscard") {
			t.Errorf("the recovered side was re-trimmed (data loss): %s", call)
		}
	}
}

// A side whose pointer really did leave the DN's list, and whose FreeSide
// never ran, IS swept — that is what the sweep exists for (the DN6 crash
// window).
func TestOrphanSweepFreesTornDownSide(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	syncupBoth(t, srv, 1, testSide)

	// Simulate the crash window: the side's local state and its resources
	// are gone, but the volume-table record survived.
	key := sideKey(testCluster, testDn, testSp, testSide)
	srv.dropSide(key)
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); !ok {
		t.Fatal("no record to orphan")
	}

	node.Reset()
	// An empty pointer list is the authority saying the side is gone.
	if _, err := srv.SyncupDn(ctx, dnReq(2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); ok {
		t.Error("the orphan record survived the sweep")
	}
	if !node.hasCall("cmd dmsetup remove " + nf.DnSideName(
		testCluster, testDn, testSp, testSide)) {
		t.Error("the orphan's side device was not removed")
	}
}

// A disk formatted for another node is refused, and — because a failed DN
// converge does not stop the side converges that follow (DN19) — nothing
// downstream may write to it either.
func TestForeignDiskIsNeverWritten(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)

	// A second agent claims the same device under a different dn_id.
	node.mu.Lock()
	node.protos = map[string][]byte{}
	node.mu.Unlock()
	srv2 := NewDnAgentServer(node.osClient(),
		common.NewNameFmt(common.DefaultLocalStorPrefix),
		common.DefaultLocalStorPrefix, testDisk, testTrConf())
	if err := srv2.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	node.Reset()
	req := dnReq(1, testSide)
	req.DnId = testDn + 100
	reply, err := srv2.SyncupDn(ctx, req)
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	meta := reply.GetDnInfo().GetMetaInfo()
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(meta.GetDetails(), "foreign disk") {
		t.Errorf("meta_info = %v/%q, want ERROR/foreign disk",
			meta.GetStatus(), meta.GetDetails())
	}

	// The side converge that follows must not allocate on the foreign disk.
	sideReply, err := srv2.SyncupSide(ctx, func() *pb.SyncupSideRequest {
		r := sideReq(1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE)
		r.DnId = testDn + 100
		return r
	}())
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := sideReply.GetSideInfo().GetSideDevInfo().GetStatus(); got ==
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("side_dev on a foreign disk reported OK")
	}
	for _, call := range node.Calls() {
		if strings.HasPrefix(call, "writeblock") {
			t.Errorf("a foreign disk was written: %s", call)
		}
		if strings.Contains(call, "dmsetup remove") {
			t.Errorf("a foreign disk's devices were removed: %s", call)
		}
	}
	// The original owner's record is intact on disk.
	fresh := NewDiskMeta(node.osClient(), testDisk)
	if _, ok, err := fresh.LookupSide(ctx, testSp, testSide); !ok ||
		err != nil {
		t.Errorf("the owning node's record was destroyed: %v %v", ok, err)
	}
}

// [D12]: outside the bounded §11.2 cutover window, no dnv device stays
// suspended. Whatever left one that way — a crash inside a reload's
// suspend/load/resume, or an older build — the next converge must resume it,
// for every dm kind the dn agent owns. (This side has no migr_src_conf, so no
// window applies.)
func TestConvergeResumesSuspendedDevices(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)

	names := []string{
		nf.DnSideName(testCluster, testDn, testSp, testSide),
		nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0),
		nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0),
	}
	node.mu.Lock()
	for _, name := range names {
		if node.dms[name] == nil {
			node.mu.Unlock()
			t.Fatalf("%s was never created", name)
		}
		node.dms[name].suspended = true
	}
	node.mu.Unlock()

	if _, err := srv.SyncupSide(ctx, sideReq(2, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	for _, name := range names {
		if node.dms[name].suspended {
			t.Errorf("%s was left suspended", name)
		}
	}
}

// A record is released only once its device is really gone: freeing extents
// that a live device still maps would let the next allocation hand the same
// bytes to a second side.
func TestFailedDeviceRemovalKeepsAllocation(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)

	node.mu.Lock()
	node.failCmdAlways["dmsetup remove "+sideDevName] = "device-mapper: busy"
	node.mu.Unlock()

	if _, err := srv.SyncupDn(ctx, dnReq(2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); !ok {
		t.Fatal("the extents were freed while a device still mapped them")
	}
	if _, ok := node.dms[sideDevName]; !ok {
		t.Fatal("the fake removed the device the test told it to refuse")
	}

	// The orphan sweep retries on the next node-level pass, and once the
	// removal succeeds the record goes with it.
	node.mu.Lock()
	delete(node.failCmdAlways, "dmsetup remove "+sideDevName)
	node.mu.Unlock()
	if _, err := srv.SyncupDn(ctx, dnReq(3)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if _, ok := node.dms[sideDevName]; ok {
		t.Error("the retry did not remove the side device")
	}
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); ok {
		t.Error("the sweep never retried the freed record")
	}
}

// A side whose extents are not contiguous still exports one flat device: the
// aggregate dm-linear concatenates the runs in order, each target's start
// following the previous target's length. This is the only place a
// multi-target table reaches dmsetup, and it travels on stdin because
// `--table` is single-line only.
func TestFragmentedSideBuildsMultiTargetTable(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide, testSide2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}

	// Carve a hole: allocate two neighbours, free the first, then ask for a
	// side that no single free run can hold.
	total := (node.devSize[testDisk] - common.DnDataOffset) / testExtentSize
	for i := uint64(0); i < total; i++ {
		if _, err := srv.meta.AllocSide(ctx, 0xaa, i, 1); err != nil {
			t.Fatalf("filling: %v", err)
		}
	}
	for i := uint64(1); i < total; i += 2 {
		if err := srv.meta.FreeSide(ctx, 0xaa, i); err != nil {
			t.Fatalf("freeing: %v", err)
		}
	}

	node.Reset()
	if _, err := srv.SyncupSide(ctx, sideReq(1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	rec, ok, err := srv.meta.LookupSide(ctx, testSp, testSide)
	if !ok || err != nil {
		t.Fatalf("no record: %v %v", ok, err)
	}
	if len(rec.GetRunList()) != int(testExtCnt) {
		t.Fatalf("runs = %v, want %d single-extent runs",
			rec.GetRunList(), testExtCnt)
	}

	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	table := node.dms[sideDevName].table
	lines := dmTargets(table)
	if len(lines) != int(testExtCnt) {
		t.Fatalf("table has %d targets, want %d:\n%s",
			len(lines), testExtCnt, table)
	}
	// Every target: contiguous starts, one extent long, and the disk offset
	// the record asks for.
	lenSectors := testExtentSize / agent.SectorSize
	for i, line := range lines {
		want := fmt.Sprintf("%d %d linear 253:0 %d",
			uint64(i)*lenSectors, lenSectors,
			(common.DnDataOffset+
				rec.GetRunList()[i].GetStart()*testExtentSize)/
				agent.SectorSize)
		if line != want {
			t.Errorf("target %d = %q, want %q", i, line, want)
		}
	}
	// The device is the right total size, and the per-CN linear on top sees
	// one flat address space.
	if got := node.devSize[nf.DmPath(sideDevName)]; got !=
		testExtCnt*testExtentSize {
		t.Errorf("side device size = %d, want %d",
			got, testExtCnt*testExtentSize)
	}
	// It went through stdin, not --table.
	if !node.hasCall("cmd dmsetup create " + sideDevName + " stdin=") {
		t.Errorf("the multi-target table did not travel on stdin:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	if node.hasCall("cmd dmsetup create " + sideDevName + " --table") {
		t.Error("a multi-target table was passed with --table")
	}

	// And the probe agrees the live table matches the record.
	reply, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	if got := reply.GetSideInfo().GetSideDevInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("side_dev_info = %v/%q", got.GetStatus(), got.GetDetails())
	}
}

// probeSideDev's two error paths: a side that has not been discarded yet is
// never exported, and a live table that disagrees with the record is an error.
func TestProbeSideDevErrorPaths(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)

	getInfo := func() *pb.ResInfo {
		t.Helper()
		reply, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide),
		})
		if err != nil {
			t.Fatalf("GetSideInfo: %v", err)
		}
		return reply.GetSideInfo().GetSideDevInfo()
	}

	// A table that no longer matches the record.
	node.mu.Lock()
	node.dms[sideDevName].table = "0 8 linear 253:0 0"
	node.mu.Unlock()
	got := getInfo()
	if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(got.GetDetails(), "extent run") {
		t.Errorf("mismatched table -> %v/%q, want ERROR/extent run",
			got.GetStatus(), got.GetDetails())
	}
	// The converge repairs it by reloading the aggregate table.
	node.Reset()
	if _, err := srv.SyncupSide(ctx, sideReq(2, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !node.hasCall("cmd dmsetup reload " + sideDevName) {
		t.Error("the mismatched side table was not reloaded")
	}
	if got := getInfo(); got.GetStatus() != pb.ResStatus_RES_STATUS_OK {
		t.Errorf("after repair: %v/%q", got.GetStatus(), got.GetDetails())
	}

	// The device gone entirely.
	node.mu.Lock()
	delete(node.dms, sideDevName)
	node.mu.Unlock()
	if got := getInfo(); got.GetStatus() != pb.ResStatus_RES_STATUS_MISSING {
		t.Errorf("missing device -> %v, want MISSING", got.GetStatus())
	}

	// An untrimmed side is never exported: the converge stops before the
	// nvmet layer and reports through the rest of the side's info.
	srv2, node2 := newTestServer(t)
	if _, err := srv2.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node2.mu.Lock()
	node2.failCmdAlways["blkdiscard --force"] = "discard failed"
	node2.mu.Unlock()
	reply, err := srv2.SyncupSide(ctx, sideReq(1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := reply.GetSideInfo().GetSideDevInfo().GetStatus(); got !=
		pb.ResStatus_RES_STATUS_ERROR {
		t.Errorf("undiscarded side_dev = %v, want ERROR", got)
	}
	// Probing the exports is fine; creating one is not.
	if node2.hasCall("cmd mkdir -p " + agent.NvmetRoot + "/subsystems/") {
		t.Error("an undiscarded side was exported")
	}
	for _, call := range node2.Mutations() {
		if strings.Contains(call, "/namespaces/1/enable") {
			t.Errorf("an undiscarded side enabled a namespace: %s", call)
		}
	}
	// The per-CN stacks are still reported, so the operator sees the whole
	// picture (probeAboveSideDev on the not-ready path).
	if len(reply.GetSideInfo().GetCnIdToDmError()) == 0 {
		t.Error("the not-ready path reported nothing above the side device")
	}
}
