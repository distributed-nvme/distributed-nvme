package dnagent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

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
	// testExtCnt is deliberately larger than common.DnZeroBatchExtCnt (10), so
	// every side in this package provisions in three §9.4 batches — 10, 10, 5.
	// A fixture equal to the batch size would zero the whole side in one
	// command and hide every multi-batch offset bug (update_01.md U4).
	testExtCnt    = uint64(25)
	testBlockSize = uint64(1 << 20)
	testMigrId    = uint64(0x21)
	testSrcDn     = uint64(4)
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

	srv := startTestServer(t, node)
	node.Reset()
	return srv, node
}

// startTestServer builds a dn server over an existing fake node and reconciles
// it, the way cmd/dnv-agent does. rootCtx is cancellable and joined at test
// end, so the §9.4 zeroing goroutines can never outlive their test and mutate
// the fake under a later assertion — the unit-test shape of agent.Serve's
// cancel-then-WaitBackground shutdown.
func startTestServer(t *testing.T, node *fakeNode) *DnAgentServer {
	t.Helper()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	srv := NewDnAgentServer(node.osClient(), nf,
		common.DefaultLocalStorPrefix, testDisk, testTrConf())
	// The §11.2 cutover grace window is off unless a test asks for it: no
	// unit test can wait common.SuspendSeconds, and with it off a migration
	// source fences straight onto its dm-errors, which is the end state
	// every other test cares about. TestMigrationSourceFence covers the
	// window itself, and TestFenceWindowDefault pins the production value.
	srv.fenceWait = 0
	// Likewise for the §9.4 retry pace: DnZeroRetryInterval is 5 s, and
	// TestZeroingRetryIsPaced pins the production value.
	srv.zeroRetryInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		srv.WaitBackground()
	})
	if err := srv.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return srv
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

// sideReq is a *steady-state* request: side_conf.provisioned is true, which is
// what the sp-worker sets once the side has finished zeroing (update_01.md
// U4's flip rule). Almost every test wants that shape, so a side is brought
// there through syncupSideTwoPhase / syncupBoth rather than by hand.
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
			Provisioned:   true,
		},
	}
}

// unprovisionedSideReq is phase 1 of the U4 flow: a freshly created side whose
// flag the CP has not flipped yet. The agent allocates its extents, builds the
// aggregate dm-linear and zeroes it — and exports nothing.
func unprovisionedSideReq(
	revision uint64,
	sideId uint64,
	primary uint64,
	standbys []uint64,
	level pb.SpLevel,
) *pb.SyncupSideRequest {
	req := sideReq(revision, sideId, primary, standbys, level)
	req.SideConf.Provisioned = false
	return req
}

// syncupSideTwoPhase plays the sp-worker's provisioning flip around one
// request: sync it once with provisioned = false so the side is allocated and
// zeroed, wait for the background goroutine to finish, then sync the request
// as given. Both phases carry the same revision, which SH8 accepts.
func syncupSideTwoPhase(
	t *testing.T,
	srv *DnAgentServer,
	req *pb.SyncupSideRequest,
) *pb.SyncupSideReply {
	t.Helper()
	ctx := context.Background()
	sideId := req.GetSidePointer().GetSideId()
	// The flip is one-way: a side that has already provisioned is never sent
	// back through phase 1, which would retract its live exports.
	rec, ok, err := srv.meta.LookupSide(ctx, testSp, sideId)
	if err != nil {
		t.Fatalf("LookupSide: %v", err)
	}
	if !ok || !sideFullyZeroed(rec) {
		phase1 := proto.Clone(req).(*pb.SyncupSideRequest)
		phase1.SideConf.Provisioned = false
		if _, err := srv.SyncupSide(ctx, phase1); err != nil {
			t.Fatalf("SyncupSide (provisioning): %v", err)
		}
		waitZeroed(t, srv, sideId)
	}
	reply, err := srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupSide rejected: %v", reply.GetAgentReply())
	}
	return reply
}

// provisionSide runs the §9.4 phase-1 flow for a side that does not exist yet:
// the CP has not flipped its provisioned flag, so the agent allocates the
// side's extents, builds its aggregate dm-linear and zeroes it — and builds
// nothing above it. A migration destination provisions exactly this way before
// its migr_dst_conf takes effect (§11.2).
func provisionSide(
	t *testing.T,
	srv *DnAgentServer,
	revision uint64,
	sideId uint64,
) {
	t.Helper()
	if _, err := srv.SyncupSide(context.Background(), unprovisionedSideReq(
		revision, sideId, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide (provisioning): %v", err)
	}
	waitZeroed(t, srv, sideId)
}

// waitZeroed blocks until the side's record says every logical extent is
// zeroed — the unit-test twin of the integ suites' `wait-zeroed` helper.
func waitZeroed(t *testing.T, srv *DnAgentServer, sideId uint64) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, ok, err := srv.meta.LookupSide(ctx, testSp, sideId)
		if err != nil {
			t.Fatalf("LookupSide: %v", err)
		}
		if ok && sideFullyZeroed(rec) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("side %d never finished zeroing", sideId)
		}
		time.Sleep(time.Millisecond)
	}
}

// syncupBoth brings a DN and one side to a converged, exporting state.
func syncupBoth(
	t *testing.T,
	srv *DnAgentServer,
	revision uint64,
	sideId uint64,
) {
	t.Helper()
	dnReply, err := srv.SyncupDn(context.Background(), dnReq(revision, sideId))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if dnReply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn rejected: %v", dnReply.GetAgentReply())
	}
	syncupSideTwoPhase(t, srv, sideReq(revision, sideId, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
}

// assertOrder matches wanted as a *subsequence* of the recorded calls: each
// entry is found at or after the position of the previous one, so a sequence
// may legitimately name the same call shape twice (the two volume-table slot
// writes bracketing a zeroing batch, say).
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
// 5. Side provisioning protocol (§9.4, DN9)
// ---------------------------------------------------------------------------

func TestSideProvisioningProtocol(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	sideDevPath := nf.DmPath(sideDevName)

	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.Reset()
	// Phase 1 of the U4 flow: the CP has not flipped the flag yet.
	reply, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	// The RPC itself only allocates and builds: the record is persisted with
	// zeroed_bits all 0 (slot B — the format wrote slot A) and the aggregate
	// device is created. The zeroing is a background goroutine, so the reply
	// says PROVISIONING and nothing above the side device exists.
	assertOrder(t, node,
		fmt.Sprintf("writeblock %s off=%d",
			testDisk, common.DnTableSlotBOffset),
		"cmd dmsetup create "+sideDevName,
	)
	devInfo := reply.GetSideInfo().GetSideDevInfo()
	if devInfo.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING {
		t.Errorf("side_dev on the first pass = %v/%q, want PROVISIONING",
			devInfo.GetStatus(), devInfo.GetDetails())
	}
	for cnId, cnInfo := range reply.GetSideInfo().GetCnIdToDmLinear() {
		if cnInfo.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING ||
			cnInfo.GetDetails() != tagProvisioningWait {
			t.Errorf("cn %d dm_linear = %v/%q, want PROVISIONING/%q", cnId,
				cnInfo.GetStatus(), cnInfo.GetDetails(), tagProvisioningWait)
		}
	}

	// The device is a single run of testExtCnt extents at the data area.
	wantOffset := common.DnDataOffset / agent.SectorSize
	wantLen := testExtCnt * testExtentSize / agent.SectorSize
	wantTable := fmt.Sprintf("stdin=0 %d linear 253:0 %d",
		wantLen, wantOffset)
	if !node.hasCall("cmd dmsetup create " + sideDevName + " " + wantTable) {
		t.Errorf("side device table is not %q; calls:\n%s",
			wantTable, strings.Join(node.Calls(), "\n"))
	}

	waitZeroed(t, srv, testSide)
	// One command per batch, in order, at exactly the byte ranges the batch
	// cursor names: testExtCnt = 25 extents in DnZeroBatchExtCnt = 10 sized
	// batches is 10, 10 and 5.
	wantBatches := zerooutBatches(sideDevPath, testExtCnt)
	assertOrder(t, node, wantBatches...)
	if got := node.dms[sideDevName].zeroouts; len(got) != len(wantBatches) {
		t.Errorf("side zeroouts = %v, want %d batches", got, len(wantBatches))
	}
	// Zeroing is never recorded as a hydration-marking discard.
	if got := node.dms[sideDevName].discards; len(got) != 0 {
		t.Errorf("the side device took hydration discards: %v", got)
	}
	// Each batch persists its own bits: three zeroouts, three slot writes.
	for i, batch := range wantBatches {
		zeroIdx := node.indexOfCall(batch)
		if node.indexOfCallFrom(
			fmt.Sprintf("writeblock %s off=", testDisk), zeroIdx+1) < 0 {
			t.Errorf("batch %d did not persist its bits", i)
		}
	}
	// Nothing was exported while the side was provisioning.
	if node.hasCall("cmd mkdir -p " + agent.NvmetRoot + "/subsystems/") {
		t.Error("a provisioning side was exported")
	}

	// Phase 2: the worker flips the flag and the exports converge.
	node.Reset()
	reply, err = srv.SyncupSide(ctx,
		sideReq(1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := reply.GetSideInfo().GetSideDevInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Fatalf("side_dev after the flip = %v/%q",
			got.GetStatus(), got.GetDetails())
	}
	if !node.hasCall("cmd mkdir -p " + agent.NvmetRoot + "/subsystems/") {
		t.Error("the flip did not export the side")
	}
	for _, call := range node.Calls() {
		if strings.Contains(call, "blkdiscard") {
			t.Errorf("a zeroed side was re-zeroed: %s", call)
		}
	}
	sideInfo := reply.GetSideInfo()
	if sideInfo.GetZeroedExtCnt() != testExtCnt ||
		sideInfo.GetTotalExtCnt() != testExtCnt {
		t.Errorf("zeroed/total = %d/%d, want %d/%d",
			sideInfo.GetZeroedExtCnt(), sideInfo.GetTotalExtCnt(),
			testExtCnt, testExtCnt)
	}
}

// zerooutBatches is the exact command line of every §9.4 batch a side of
// extCnt extents produces, in order.
func zerooutBatches(sideDevPath string, extCnt uint64) []string {
	var out []string
	for from := uint64(0); from < extCnt; from += common.DnZeroBatchExtCnt {
		count := extCnt - from
		if count > common.DnZeroBatchExtCnt {
			count = common.DnZeroBatchExtCnt
		}
		out = append(out, fmt.Sprintf(
			"cmd blkdiscard --zeroout --offset %d --length %d %s",
			from*testExtentSize, count*testExtentSize, sideDevPath))
	}
	return out
}

// The converge matrix of update_01.md U4, one sub-test per row.
func TestSideProvisioningMatrix(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	ctx := context.Background()

	// Row 1: provisioned = false, no record. Allocate, build, start zeroing.
	t.Run("unprovisioned/absent", func(t *testing.T) {
		srv, node := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		reply, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		got := reply.GetSideInfo().GetSideDevInfo()
		if got.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING {
			t.Fatalf("side_dev = %v/%q, want PROVISIONING",
				got.GetStatus(), got.GetDetails())
		}
		if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); !ok {
			t.Error("row 1 did not allocate")
		}
		if _, ok := node.dms[sideDevName]; !ok {
			t.Error("row 1 did not build the side device")
		}
		waitZeroed(t, srv, testSide)
	})

	// Row 2: provisioned = false, partial bits. Keep converging the linear and
	// keep the goroutine; report the progress string with a real k.
	t.Run("unprovisioned/partial", func(t *testing.T) {
		srv, node := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		// Let exactly the first batch land and park the second one *inside*
		// its child. A failing batch would freeze the record too, but it would
		// also publish a zeroErr — ERROR wins over the progress string while
		// one is outstanding (ruling R4.14) — and a batch merely paced by
		// zeroRetryInterval keeps moving, so k would be racy and only its
		// prefix assertable. Blocking is what makes the whole formatted string
		// exact.
		batch1 := "cmd blkdiscard --zeroout --offset " + strconv.FormatUint(
			common.DnZeroBatchExtCnt*testExtentSize, 10)
		node.blockCmd(batch1)
		t.Cleanup(func() { node.releaseCmd(batch1) })
		if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if !waitFor(t, 5*time.Second, func() bool {
			rec, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide)
			return ok && sideZeroedCnt(rec) == common.DnZeroBatchExtCnt
		}) {
			t.Fatal("the first batch never landed")
		}
		reply, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		got := reply.GetSideInfo().GetSideDevInfo()
		// k/n in extents, k first: an operator reading "zeroing 25/10" would
		// be told a side that has zeroed nothing is nearly done.
		wantDetails := fmt.Sprintf(zeroingDetailsFmt,
			common.DnZeroBatchExtCnt, testExtCnt)
		if got.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING ||
			got.GetDetails() != wantDetails {
			t.Errorf("side_dev = %v/%q, want PROVISIONING/%q",
				got.GetStatus(), got.GetDetails(), wantDetails)
		}
		if reply.GetSideInfo().GetZeroedExtCnt() !=
			common.DnZeroBatchExtCnt {
			t.Errorf("zeroed_ext_cnt = %d, want %d",
				reply.GetSideInfo().GetZeroedExtCnt(),
				uint64(common.DnZeroBatchExtCnt))
		}
		if reply.GetSideInfo().GetTotalExtCnt() != testExtCnt {
			t.Errorf("total_ext_cnt = %d, want %d",
				reply.GetSideInfo().GetTotalExtCnt(), testExtCnt)
		}
		// The parked batch finishes the side once its child returns.
		node.releaseCmd(batch1)
		waitZeroed(t, srv, testSide)
	})

	// Row 3: provisioned = false, complete bits. The linear is ensured, the
	// goroutine is gone, and there are still no exports.
	t.Run("unprovisioned/complete", func(t *testing.T) {
		srv, node := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		waitZeroed(t, srv, testSide)
		node.Reset()
		reply, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		got := reply.GetSideInfo().GetSideDevInfo()
		if got.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("side_dev = %v/%q, want OK",
				got.GetStatus(), got.GetDetails())
		}
		st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
		srv.mu.Lock()
		zeroing := st.zeroing
		srv.mu.Unlock()
		if zeroing {
			t.Error("a fully zeroed side kept its zeroing goroutine")
		}
		if node.hasCall("cmd mkdir -p " + agent.NvmetRoot + "/subsystems/") {
			t.Error("an unflipped side was exported")
		}
		for cnId, cnInfo := range reply.GetSideInfo().GetCnIdToNvmeof() {
			if cnInfo.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING {
				t.Errorf("cn %d nvmeof = %v, want PROVISIONING",
					cnId, cnInfo.GetStatus())
			}
		}
	})

	// Row 4 is the steady state every other test in this package runs in, so
	// it only needs the export assertion.
	t.Run("provisioned/complete", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		if !node.hasCall("cmd mkdir -p " + agent.NvmetRoot + "/subsystems/") {
			t.Error("row 4 did not export the side")
		}
	})

	// Row 5: provisioned = true over incomplete bits. The agent trusts its own
	// bits over the flag: ERROR, no exports, and the goroutine keeps running
	// so the side self-heals.
	t.Run("provisioned/partial", func(t *testing.T) {
		srv, node := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		node.mu.Lock()
		node.failCmdAlways["blkdiscard --zeroout"] = "zeroout refused"
		node.mu.Unlock()
		if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		node.Reset()
		reply, err := srv.SyncupSide(ctx,
			sideReq(1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		got := reply.GetSideInfo().GetSideDevInfo()
		if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
			got.GetDetails() != tagNotZeroed {
			t.Errorf("side_dev = %v/%q, want ERROR/%q",
				got.GetStatus(), got.GetDetails(), tagNotZeroed)
		}
		if node.hasCall("cmd mkdir -p " + agent.NvmetRoot + "/subsystems/") {
			t.Error("a partially zeroed side was exported")
		}
		st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
		srv.mu.Lock()
		zeroing := st.zeroing
		srv.mu.Unlock()
		if !zeroing {
			t.Error("row 5 dropped the self-healing goroutine")
		}
		// And it does self-heal once the command works again.
		node.mu.Lock()
		delete(node.failCmdAlways, "blkdiscard --zeroout")
		node.mu.Unlock()
		waitZeroed(t, srv, testSide)
	})

	// Row 6: provisioned = true, no record. The data is gone; re-allocating
	// would present a zeroed impostor as the data-bearing leg.
	t.Run("provisioned/absent", func(t *testing.T) {
		srv, node := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		node.Reset()
		reply, err := srv.SyncupSide(ctx,
			sideReq(1, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		got := reply.GetSideInfo().GetSideDevInfo()
		if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
			got.GetDetails() != tagRecordMissing {
			t.Errorf("side_dev = %v/%q, want ERROR/%q",
				got.GetStatus(), got.GetDetails(), tagRecordMissing)
		}
		// No allocation write, and no side dm-linear.
		for _, call := range node.Calls() {
			if strings.HasPrefix(call, "writeblock") {
				t.Errorf("row 6 allocated: %s", call)
			}
		}
		if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); ok {
			t.Error("row 6 created an allocation record")
		}
		if _, ok := node.dms[sideDevName]; ok {
			t.Error("row 6 built the side device")
		}
		// total_ext_cnt is never omitted: with no record it comes from the
		// request (ruling R4.16).
		if reply.GetSideInfo().GetTotalExtCnt() != testExtCnt {
			t.Errorf("total_ext_cnt = %d, want %d",
				reply.GetSideInfo().GetTotalExtCnt(), testExtCnt)
		}
	})

	// A read-only probe never allocates, so "record absent at
	// provisioned = false" is MISSING with empty details, not the matrix's
	// "zeroing 0/n" (ruling R4.15).
	t.Run("probe/unprovisioned/absent", func(t *testing.T) {
		srv, node := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
			1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		waitZeroed(t, srv, testSide)
		clearRecord(t, srv, node)
		node.Reset()
		reply, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide),
		})
		if err != nil {
			t.Fatalf("GetSideInfo: %v", err)
		}
		got := reply.GetSideInfo().GetSideDevInfo()
		if got.GetStatus() != pb.ResStatus_RES_STATUS_MISSING ||
			got.GetDetails() != "" {
			t.Errorf("side_dev = %v/%q, want MISSING/\"\"",
				got.GetStatus(), got.GetDetails())
		}
		for _, call := range node.Mutations() {
			t.Errorf("a probe mutated the node: %s", call)
		}
	})

	// A probe over a re-allocated (all-not-zeroed) record at
	// provisioned = true reports the same hard error the converge does — and
	// still starts nothing.
	t.Run("probe/provisioned/partial", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		clearZeroed(t, srv, node)
		node.Reset()
		reply, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide),
		})
		if err != nil {
			t.Fatalf("GetSideInfo: %v", err)
		}
		got := reply.GetSideInfo().GetSideDevInfo()
		if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
			got.GetDetails() != tagNotZeroed {
			t.Errorf("side_dev = %v/%q, want ERROR/%q",
				got.GetStatus(), got.GetDetails(), tagNotZeroed)
		}
		if reply.GetSideInfo().GetZeroedExtCnt() != 0 ||
			reply.GetSideInfo().GetTotalExtCnt() != testExtCnt {
			t.Errorf("zeroed/total = %d/%d, want 0/%d",
				reply.GetSideInfo().GetZeroedExtCnt(),
				reply.GetSideInfo().GetTotalExtCnt(), testExtCnt)
		}
		st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
		srv.mu.Lock()
		zeroing := st.zeroing
		srv.mu.Unlock()
		if zeroing {
			t.Error("a read-only probe started the zeroing goroutine")
		}
		for _, call := range node.Mutations() {
			t.Errorf("a probe mutated the node: %s", call)
		}
	})
}

// A restart resumes zeroing where the last process stopped: the bits are in
// the volume table, and only the extents whose bits are unset are redone.
func TestZeroingResumesAfterRestart(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	sideDevPath := nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide))

	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	// Let exactly the first batch through, then stop the process.
	node.mu.Lock()
	node.failCmdAlways["blkdiscard --zeroout --offset "+
		strconv.FormatUint(common.DnZeroBatchExtCnt*testExtentSize, 10)] =
		"crash"
	node.mu.Unlock()
	if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		rec, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide)
		return ok && sideZeroedCnt(rec) == common.DnZeroBatchExtCnt
	}) {
		t.Fatal("the first batch never landed")
	}
	stopTestServer(t, srv)

	// A fresh process over the same disk and store.
	node.mu.Lock()
	delete(node.failCmdAlways, "blkdiscard --zeroout --offset "+
		strconv.FormatUint(common.DnZeroBatchExtCnt*testExtentSize, 10))
	node.mu.Unlock()
	node.Reset()
	restarted := startTestServer(t, node)
	waitZeroed(t, restarted, testSide)

	// Batch 0 is never redone; batches 1 and 2 are.
	batches := zerooutBatches(sideDevPath, testExtCnt)
	if node.hasCall(batches[0]) {
		t.Errorf("the restart re-zeroed an already zeroed batch: %s",
			batches[0])
	}
	assertOrder(t, node, batches[1:]...)
}

// The teardown cancels the zeroing goroutine and *waits* for it before the
// side device is removed: the `blkdiscard --zeroout` child holds that device
// open and `dmsetup remove` would fail EBUSY.
//
// The batch is held in flight by a **hard** gate — one the cancel does not
// release — because that is the only shape in which the wait is observable. A
// loop parked in zeroPace's timer, or one whose batch fails instantly, is
// drained by a bare cancel just as fast as by cancel-and-wait, so a test built
// on those passes with the `<-done` deleted and with the stop moved after the
// removal. Here the removal must not appear until the child returns
// (doc/dnagent.md §6 test 19, update_01.md U4).
func TestZeroingCancelledBeforeDeviceRemoval(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	const zeroout = "cmd blkdiscard --zeroout"
	removeCall := "cmd dmsetup remove " + sideDevName

	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	// The first batch enters its child and stays there until this test says
	// otherwise — the CmdSoftTimeout-to-CmdHardTimeout window of a real one.
	node.blockCmd(zeroout)
	t.Cleanup(func() { node.releaseCmd(zeroout) })
	if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		return node.hasCall(zeroout)
	}) {
		t.Fatal("the zeroing loop never ran a batch")
	}

	// The pointer leaves the DN's list: DN6 teardown. It blocks in
	// stopZeroing's wait, so it cannot run on this goroutine.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := srv.SyncupDn(ctx, dnReq(2)); err != nil {
			t.Errorf("SyncupDn: %v", err)
		}
	}()

	if waitFor(t, 200*time.Millisecond, func() bool {
		return node.hasCall(removeCall)
	}) {
		t.Fatal("the side device was removed while a zeroing batch still " +
			"held it open")
	}
	select {
	case <-done:
		t.Fatal("the teardown finished without waiting for the batch")
	default:
	}

	// The child returns; only now may the removal run.
	node.releaseCmd(zeroout)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the teardown never finished after the batch returned")
	}
	removeIdx := node.indexOfCall(removeCall)
	if removeIdx < 0 {
		t.Fatal("the side device was never removed")
	}
	if node.indexOfCallFrom(zeroout, removeIdx+1) >= 0 {
		t.Error("a zeroing batch ran after the side device was removed")
	}
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	if st != nil {
		t.Error("side state survived the teardown")
	}
}

// Every reply's counters come from the RECORD whenever one exists — including
// the paths that then fail (the DN9 ext-count mismatch, a disk the agent may
// not mutate). side_conf.ext_cnt is a gate, never evidence: the disk is
// authoritative ([D13], ruling R4.16). Only row 6 — no record at all — falls
// back to the request's number, which the matrix pins separately.
func TestAllocFailureReportsTheRecordsCounters(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)

	// The CP asks for a bigger side than the record holds: AllocSide refuses
	// (DN9) rather than re-allocating over live data.
	req := sideReq(2, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE)
	req.SideConf.ExtCnt = testExtCnt + 5
	node.Reset()
	reply, err := srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	got := reply.GetSideInfo().GetSideDevInfo()
	if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(got.GetDetails(), "allocated") {
		t.Fatalf("side_dev = %v/%q, want ERROR carrying the DN9 mismatch",
			got.GetStatus(), got.GetDetails())
	}
	if reply.GetSideInfo().GetTotalExtCnt() != testExtCnt ||
		reply.GetSideInfo().GetZeroedExtCnt() != testExtCnt {
		t.Errorf("zeroed/total = %d/%d, want %d/%d — the disk's numbers, not "+
			"the request's",
			reply.GetSideInfo().GetZeroedExtCnt(),
			reply.GetSideInfo().GetTotalExtCnt(), testExtCnt, testExtCnt)
	}
	// The read-only twin already answers from the record, and the two must
	// not contradict each other over identical on-disk state.
	infoReply, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	if infoReply.GetSideInfo().GetTotalExtCnt() !=
		reply.GetSideInfo().GetTotalExtCnt() ||
		infoReply.GetSideInfo().GetZeroedExtCnt() !=
			reply.GetSideInfo().GetZeroedExtCnt() {
		t.Errorf("probe reports %d/%d, converge %d/%d",
			infoReply.GetSideInfo().GetZeroedExtCnt(),
			infoReply.GetSideInfo().GetTotalExtCnt(),
			reply.GetSideInfo().GetZeroedExtCnt(),
			reply.GetSideInfo().GetTotalExtCnt())
	}
}

// §9.4's DN5 fail-fast: a disk whose write_zeroes_max_bytes reads 0 cannot
// meet the fast-Write-Zeroes assumption, so meta_info is an error that feeds
// err_epoch. An absent attribute is not a verdict.
func TestWriteZeroesFailFast(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	wzPath := "/sys/class/block/fake-disk/queue/write_zeroes_max_bytes"

	// Absent: not a verdict (the fake has no sysfs tree by default).
	reply, err := srv.SyncupDn(ctx, dnReq(1))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if got := reply.GetDnInfo().GetMetaInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("absent attribute = %v/%q, want OK",
			got.GetStatus(), got.GetDetails())
	}

	// A present 0 is.
	node.mu.Lock()
	node.files[wzPath] = "0\n"
	node.mu.Unlock()
	reply, err = srv.SyncupDn(ctx, dnReq(2))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	got := reply.GetDnInfo().GetMetaInfo()
	if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		got.GetDetails() != tagNoWriteZeroes {
		t.Fatalf("meta_info = %v/%q, want ERROR/%q",
			got.GetStatus(), got.GetDetails(), tagNoWriteZeroes)
	}
	// It reports, it does not gate: an already-populated DN keeps converging.
	infoReply, err := srv.GetDnInfo(ctx, &pb.GetDnInfoRequest{
		ClusterId: testCluster, DnId: testDn,
	})
	if err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	if got := infoReply.GetDnInfo().GetMetaInfo(); got.GetDetails() !=
		tagNoWriteZeroes {
		t.Errorf("the probe missed the fail-fast: %v/%q",
			got.GetStatus(), got.GetDetails())
	}

	// A non-zero value clears it again.
	node.mu.Lock()
	node.files[wzPath] = "2097152\n"
	node.mu.Unlock()
	reply, err = srv.SyncupDn(ctx, dnReq(3))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if got := reply.GetDnInfo().GetMetaInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("fast Write Zeroes = %v/%q, want OK",
			got.GetStatus(), got.GetDetails())
	}
}

// A failed batch publishes the killed command's output on side_dev_info and is
// retried no sooner than zeroRetryInterval later — never a hot loop.
func TestZeroingRetryIsPaced(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	srv.zeroRetryInterval = 200 * time.Millisecond

	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.mu.Lock()
	node.failCmdAlways["blkdiscard --zeroout"] = "fake: blkdiscard failed"
	node.mu.Unlock()
	if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		return len(node.callsMatching("cmd blkdiscard --zeroout")) >= 1
	}) {
		t.Fatal("the zeroing loop never ran a batch")
	}
	// Two retry intervals' worth of wall clock must not produce a hot loop.
	time.Sleep(500 * time.Millisecond)
	if got := len(node.callsMatching("cmd blkdiscard --zeroout")); got > 5 {
		t.Errorf("%d attempts in 500ms at a 200ms pace — the retry is a hot "+
			"loop", got)
	}

	// The outstanding failure is what side_dev_info reports (ruling R4.14).
	reply, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		2, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	got := reply.GetSideInfo().GetSideDevInfo()
	if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(got.GetDetails(), "blkdiscard") {
		t.Errorf("side_dev = %v/%q, want ERROR carrying the command output",
			got.GetStatus(), got.GetDetails())
	}

	// And the next successful batch clears it back to PROVISIONING.
	node.mu.Lock()
	delete(node.failCmdAlways, "blkdiscard --zeroout")
	node.mu.Unlock()
	waitZeroed(t, srv, testSide)
	reply, err = srv.SyncupSide(ctx, unprovisionedSideReq(
		3, testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := reply.GetSideInfo().GetSideDevInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("side_dev after the heal = %v/%q, want OK",
			got.GetStatus(), got.GetDetails())
	}
}

// TestZeroRetryIntervalDefault pins the production pace, which every other
// test shortens.
func TestZeroRetryIntervalDefault(t *testing.T) {
	srv := NewDnAgentServer(newFakeNode().osClient(),
		common.NewNameFmt(common.DefaultLocalStorPrefix),
		common.DefaultLocalStorPrefix, testDisk, testTrConf())
	if srv.zeroRetryInterval != common.DnZeroRetryInterval*time.Second {
		t.Errorf("zeroRetryInterval = %v, want %v", srv.zeroRetryInterval,
			common.DnZeroRetryInterval*time.Second)
	}
}

// stopTestServer plays process exit: stop every background goroutine and join
// them, the way agent.Serve does after GracefulStop.
func stopTestServer(t *testing.T, srv *DnAgentServer) {
	t.Helper()
	for _, key := range srv.allSideKeys() {
		st := srv.getSide(key)
		srv.stopMigrRetry(st)
		srv.stopZeroing(st)
	}
	srv.WaitBackground()
}

// clearRecord deletes the side's allocation record behind the agent's back —
// the lost-or-foreign-disk case — and makes the server re-read the disk.
func clearRecord(t *testing.T, srv *DnAgentServer, node *fakeNode) {
	t.Helper()
	other := newVerifiedMeta(t, node)
	if err := other.FreeSide(context.Background(), testSp, testSide); err != nil {
		t.Fatalf("FreeSide: %v", err)
	}
	srv.meta = newVerifiedMeta(t, node)
}

// clearZeroed puts the side's on-disk record back into the not-zeroed state —
// "somebody re-allocated the side behind our back" — and makes the server
// re-read the disk. It goes through a second, separately verified DiskMeta so
// it exercises the same API the agent does, and it is the invariant in
// action: a re-allocated record starts all-not-zeroed (update_01.md U4).
func clearZeroed(t *testing.T, srv *DnAgentServer, node *fakeNode) {
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
// frees its extents, and the next SyncupSide re-runs the §9.4 provisioning
// protocol and zeroes live data.
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
	if !sideFullyZeroed(after) {
		t.Error("the zeroed bits were lost across the restart")
	}
	if node.hasCall("cmd dmsetup remove " + nf.DnSideName(
		testCluster, testDn, testSp, testSide)) {
		t.Error("SyncupDn removed a live side device")
	}

	// And the re-sent SyncupSide reuses the allocation instead of re-zeroing
	// it.
	node.Reset()
	if _, err := srv2.SyncupSide(ctx, sideReq(1, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	for _, call := range node.Calls() {
		if strings.Contains(call, "blkdiscard") {
			t.Errorf("the recovered side was re-zeroed (data loss): %s", call)
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
	syncupSideTwoPhase(t, srv, sideReq(1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE))
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

	// A side that is not fully zeroed is never exported: the converge stops
	// before the nvmet layer and reports through the rest of the side's info.
	srv2, node2 := newTestServer(t)
	if _, err := srv2.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node2.mu.Lock()
	node2.failCmdAlways["blkdiscard --zeroout"] = "zeroout failed"
	node2.mu.Unlock()
	reply, err := srv2.SyncupSide(ctx, sideReq(1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := reply.GetSideInfo().GetSideDevInfo().GetStatus(); got !=
		pb.ResStatus_RES_STATUS_ERROR {
		t.Errorf("unzeroed side_dev = %v, want ERROR", got)
	}
	// Probing the exports is fine; creating one is not.
	if node2.hasCall("cmd mkdir -p " + agent.NvmetRoot + "/subsystems/") {
		t.Error("an unzeroed side was exported")
	}
	for _, call := range node2.Mutations() {
		if strings.Contains(call, "/namespaces/1/enable") {
			t.Errorf("an unzeroed side enabled a namespace: %s", call)
		}
	}
	// The per-CN stacks are still reported, so the operator sees the whole
	// picture (probeAboveSideDev on the not-ready path).
	if len(reply.GetSideInfo().GetCnIdToDmError()) == 0 {
		t.Error("the not-ready path reported nothing above the side device")
	}
}

// startPartialZeroing brings a side to the row-2 shape and *holds* it there:
// the first batch lands, the second is parked inside its child, so the record
// stays at DnZeroBatchExtCnt of testExtCnt and the loop publishes no zeroErr.
// The returned function releases the child.
func startPartialZeroing(
	t *testing.T,
	srv *DnAgentServer,
	node *fakeNode,
) func() {
	t.Helper()
	ctx := context.Background()
	batch1 := "cmd blkdiscard --zeroout --offset " + strconv.FormatUint(
		common.DnZeroBatchExtCnt*testExtentSize, 10)
	node.blockCmd(batch1)
	release := func() { node.releaseCmd(batch1) }
	t.Cleanup(release)
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if _, err := srv.SyncupSide(ctx, unprovisionedSideReq(
		1, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		rec, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide)
		return ok && sideZeroedCnt(rec) == common.DnZeroBatchExtCnt
	}) {
		t.Fatal("the first batch never landed")
	}
	return release
}

// A side whose dm-linear is missing or carries the wrong table must say so,
// even while it is still being zeroed: DN18 judges side_dev_info by "the
// volume-table record + its zeroed_bits + `dmsetup table`", and a probe that
// skips the device check reports healthy PROVISIONING for ever. PROVISIONING
// never feeds err_epoch (§9.5), so nothing would ever re-send the SyncupSide
// that is the only thing able to rebuild the device — the side would be
// bricked and silent.
func TestProbeReportsABrokenDeviceWhileZeroing(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	startPartialZeroing(t, srv, node)

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

	// A healthy provisioning side still reports its progress, unchanged.
	node.Reset()
	if got := getInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_PROVISIONING {
		t.Fatalf("side_dev while zeroing = %v/%q, want PROVISIONING",
			got.GetStatus(), got.GetDetails())
	}

	// A live table that does not match the record's extent runs.
	node.mu.Lock()
	node.dms[sideDevName].table = "0 8 linear 253:0 0"
	node.mu.Unlock()
	got := getInfo()
	if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(got.GetDetails(), "extent run") {
		t.Errorf("mismatched table while zeroing -> %v/%q, want ERROR/extent "+
			"run", got.GetStatus(), got.GetDetails())
	}

	// The device gone entirely — the ensureSideDm-failed shape, which no
	// PROVISIONING row would ever get retried.
	node.mu.Lock()
	delete(node.dms, sideDevName)
	node.mu.Unlock()
	if got := getInfo(); got.GetStatus() != pb.ResStatus_RES_STATUS_MISSING {
		t.Errorf("missing device while zeroing -> %v/%q, want MISSING",
			got.GetStatus(), got.GetDetails())
	}
	for _, call := range node.Mutations() {
		t.Errorf("a probe mutated the node: %s", call)
	}
}

// probeSideDev must bind the published batch error ONCE. setZeroingErr runs
// from the zeroing loop after release() — outside both DN1 locks — so a probe
// that tests the accessor and then re-reads it to format the details can see
// nil the second time and call .Error() on a nil error. That is a panic inside
// a gRPC handler, and nothing in this repo installs a recovery interceptor, so
// it takes the whole dn agent down with every side it hosts. `go test -race`
// cannot see it: both reads are correctly mutex-guarded, only the timing is
// wrong (ruling R4.14).
func TestProbeSideDevBindsTheZeroingErrorOnce(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	startPartialZeroing(t, srv, node)
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	if st == nil {
		t.Fatal("the side state vanished")
	}
	// The parked batch publishes nothing, so this test is the only writer.
	plan := newSidePlan(srv.nf, st.req, testExtentSize)

	stop := make(chan struct{})
	flipped := make(chan struct{})
	go func() {
		defer close(flipped)
		batchErr := fmt.Errorf("blkdiscard --zeroout: exit status 1")
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				srv.setZeroingErr(st, batchErr)
			} else {
				srv.setZeroingErr(st, nil)
			}
		}
	}()

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("probeSideDev panicked while the zeroing loop "+
					"cleared its error: %v", r)
			}
		}()
		for i := 0; i < 20000; i++ {
			srv.probeSideDev(ctx, st, plan, &pb.SideInfo{})
			if i%1000 == 0 {
				// The probe records three dm reads per round; recording all
				// of them would be the only thing this test measured.
				node.Reset()
			}
		}
	}()
	close(stop)
	<-flipped
	srv.setZeroingErr(st, nil)
}
