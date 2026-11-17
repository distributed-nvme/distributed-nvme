package dnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A migration destination's run in the clone-metadata area (DN13)
// ---------------------------------------------------------------------------

// fillSp / fillMigr name the other migration whose run fills the area in
// these tests: another sp's, so no claim of the fixture side covers it.
const (
	fillSp   = testSp + 1
	fillMigr = testMigrId + 1
)

// testMigrMetaUnits is the fixture destination's run: 25 regions of one
// testBlockSize each over the base, two units.
var testMigrMetaUnits = common.CloneMetaUnits(
	testExtCnt*testExtentSize, testBlockSize)

// fillMetaArea has fillMigr hold every unit of the area. The record stays
// for as long as no SyncupDn runs: the node-level sweep frees a record no
// held side claims (DN6).
func fillMetaArea(t *testing.T, srv *DnAgentServer) {
	t.Helper()
	if _, err := srv.meta.AllocCloneMeta(context.Background(), fillSp,
		fillMigr, common.DnCloneMetaUnitCnt); err != nil {
		t.Fatalf("filling the clone-metadata area: %v", err)
	}
}

// freeMetaArea ends fillMigr: its units come free.
func freeMetaArea(t *testing.T, srv *DnAgentServer) {
	t.Helper()
	if err := srv.meta.FreeCloneMeta(
		context.Background(), fillSp, fillMigr); err != nil {
		t.Fatalf("FreeCloneMeta: %v", err)
	}
}

// waitingDstReq is a migration destination's request before the worker
// flips it: migrDstReq with provisioned = false.
func waitingDstReq(revision uint64, level pb.SpLevel) *pb.SyncupSideRequest {
	req := migrDstReq(revision, level)
	req.SideConf.Provisioned = false
	return req
}

// metaRoomText is the detail of the fixture destination's rows while the
// filled area has no run for it: the wait's words, then the allocator's own
// text (dnagent.md DN13).
func metaRoomText() string {
	return fmt.Sprintf("waiting for metadata room: allocating %d "+
		"clone-metadata units: no contiguous run of %d units in the "+
		"%d-unit area", testMigrMetaUnits, testMigrMetaUnits,
		common.DnCloneMetaUnitCnt)
}

// convergePasses counts the side converges since the node's last Reset:
// each lists the node's dm devices once, in its sweep.
func convergePasses(node *fakeNode) int {
	return len(node.callsMatching("cmd dmsetup ls"))
}

// checkSide runs the fixture side's read and then one CheckSide round of it,
// its info included, so the round answers what the node holds now (SH29).
func checkSide(
	ctx context.Context,
	srv *DnAgentServer,
) *pb.CheckSideReply {
	reply, _ := checkSideNow(srv, ctx, &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn,
		SidePointer: sidePtr(testSide), Revision: 1, ShowInfo: true,
	}, nil)
	return reply
}

// assertProvisioning checks one row's status and details.
func assertProvisioning(
	t *testing.T,
	what string,
	got *pb.ResInfo,
	details string,
) {
	t.Helper()
	if got.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING ||
		got.GetDetails() != details {
		t.Errorf("%s = %v/%q, want PROVISIONING/%q", what, got.GetStatus(),
			got.GetDetails(), details)
	}
}

// assertWaitingRows checks the rows of the fixture destination while it
// waits for its run at level: side_dev_info PROVISIONING with the area's
// text over the record's counters at 0 of n; below SP_LEVEL_NO_MIGRATION
// both migr_dst_info rows with the same text, and none at or above it; below
// SP_LEVEL_DISABLE every per-CN row PROVISIONING "side provisioning", and
// none at it.
func assertWaitingRows(
	t *testing.T,
	what string,
	info *pb.SideInfo,
	level pb.SpLevel,
) {
	t.Helper()
	assertProvisioning(t, what+" side_dev_info", info.GetSideDevInfo(),
		metaRoomText())
	if info.GetZeroedBytes() != 0 || info.GetZeroBytes() != testZeroBytes {
		t.Errorf("%s zeroed %d of %d bytes, want 0 of %d", what,
			info.GetZeroedBytes(), info.GetZeroBytes(), testZeroBytes)
	}
	dst := info.GetMigrDstInfo()
	switch {
	case level < pb.SpLevel_SP_LEVEL_NO_MIGRATION:
		assertProvisioning(t, what+" migr_dst target_info",
			dst.GetTargetInfo(), metaRoomText())
		assertProvisioning(t, what+" migr_dst dm_clone_info",
			dst.GetDmCloneInfo(), metaRoomText())
	case dst != nil:
		t.Errorf("%s has migr_dst_info rows at %v: %v", what, level, dst)
	}
	if level >= pb.SpLevel_SP_LEVEL_DISABLE {
		if n := len(info.GetCnIdToDmError()) + len(info.GetCnIdToDmLinear()) +
			len(info.GetCnIdToNvmeof()); n != 0 {
			t.Errorf("%s has %d per-CN rows at %v, want none", what, n, level)
		}
		return
	}
	for _, cnId := range []uint64{testCn0, testCn1} {
		assertProvisioning(t, fmt.Sprintf("%s dm_error/%d", what, cnId),
			info.GetCnIdToDmError()[cnId], tagProvisioningWait)
		assertProvisioning(t, fmt.Sprintf("%s dm_linear/%d", what, cnId),
			info.GetCnIdToDmLinear()[cnId], tagProvisioningWait)
		assertProvisioning(t, fmt.Sprintf("%s nvmeof/%d", what, cnId),
			info.GetCnIdToNvmeof()[cnId], tagProvisioningWait)
	}
}

// assertRunHeld checks that the fixture destination holds its run, and that
// the run's head is zeroed before any batch zeroes the side.
func assertRunHeld(t *testing.T, srv *DnAgentServer, node *fakeNode) {
	t.Helper()
	rec, ok, err := srv.meta.LookupCloneMeta(
		context.Background(), testSp, testMigrId)
	if err != nil || !ok || rec.GetUnitCount() != testMigrMetaUnits {
		t.Errorf("the destination's run = %v (found %v, %v), want %d units",
			rec, ok, err, testMigrMetaUnits)
	}
	head := node.indexOfCall(fmt.Sprintf("writeblock %s off=%d len=8192",
		testDisk, common.DnCloneMetaOffset+
			rec.GetUnitStart()*common.DnCloneMetaUnit))
	zeroout := node.indexOfCall("cmd blkdiscard --zeroout")
	if head < 0 || zeroout < 0 || head > zeroout {
		t.Errorf("the run's head was zeroed at call %d and the side's first "+
			"batch ran at call %d; want the head first", head, zeroout)
	}
}

// A destination's run is the shared formula over the moved side's bytes and
// the pool's block size (architecture.md, Metadata units) — the count the
// control plane charges the node by — and, for every block size of whole
// sectors, the count the side's sectors over the region's sectors give: the
// base plus one byte per region, rounded up to whole units. A block size
// under one sector sizes no run.
func TestMigrMetaUnits(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	planOf := func(extCnt, extentSize, blockSize uint64) *sidePlan {
		req := migrDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)
		req.SideConf.ExtCnt = extCnt
		req.MigrDstConf.BlockSize = blockSize
		return newSidePlan(nf, req, extentSize)
	}
	for _, tc := range []struct {
		name       string
		extCnt     uint64
		extentSize uint64
		blockSize  uint64
		want       uint64
	}{
		{"the fixture side", testExtCnt, testExtentSize, testBlockSize, 2},
		{"a side under one block", 1, 1 << 20, 2 << 20, 1},
		{"a side of one block", 2, 1 << 20, 2 << 20, 2},
		{"1 TiB at 1 MiB blocks", 1024, 1 << 30, 1 << 20, 2},
		{"1 TiB at 64 KiB blocks", 1024, 1 << 30, 64 << 10, 5},
		{"no extent size", testExtCnt, 0, testBlockSize, 1},
	} {
		got, err := migrMetaUnits(
			planOf(tc.extCnt, tc.extentSize, tc.blockSize))
		if err != nil || got != tc.want {
			t.Errorf("%s: migrMetaUnits = %d, %v; want %d", tc.name, got,
				err, tc.want)
		}
	}
	for _, blockSize := range []uint64{0, 1, agent.SectorSize - 1} {
		if got, err := migrMetaUnits(planOf(
			testExtCnt, testExtentSize, blockSize)); err == nil {
			t.Errorf("block size %d sized a run of %d units", blockSize, got)
		}
	}

	sectorUnits := func(extCnt, extentSize, blockSize uint64) uint64 {
		sectors := extCnt * extentSize / agent.SectorSize
		bytes := common.CloneMetaBaseBytes +
			sectors/(blockSize/agent.SectorSize)
		return (bytes + common.DnCloneMetaUnit - 1) / common.DnCloneMetaUnit
	}
	cases := 0
	for _, extentSize := range []uint64{1 << 20, 4 << 20, 64 << 20, 1 << 30} {
		for _, extCnt := range []uint64{
			1, 2, 3, 7, 25, 64, 255, 1024, 4096, 65536,
		} {
			for _, blockSize := range []uint64{
				agent.SectorSize, 3 * agent.SectorSize, 4 << 10, 64 << 10,
				192 << 10, 1 << 20, 2 << 20, 8 << 20,
			} {
				cases++
				want := sectorUnits(extCnt, extentSize, blockSize)
				got, err := migrMetaUnits(
					planOf(extCnt, extentSize, blockSize))
				if err != nil || got != want {
					t.Errorf("%d extents of %d at block size %d: "+
						"migrMetaUnits = %d, %v; the sectors give %d",
						extCnt, extentSize, blockSize, got, err, want)
				}
			}
		}
	}
	if cases != 320 {
		t.Fatalf("the grid ran %d cases, want 320", cases)
	}
}

// A migration destination takes its run in the clone-metadata area before it
// zeroes, at every level (DN13). While the area has no free run for it, it
// zeroes nothing — its counters stay at 0 of n, so the worker does not flip
// it and the source keeps serving — and side_dev_info reads PROVISIONING with
// the area's text, which sets no health epoch. Where the level emits them,
// both migr_dst_info rows carry the same text, and the per-CN rows keep
// "side provisioning". The check round reads the same rows at the same
// epochs and answers code 0, since a re-sent request would wait the same way
// (DN16), and nothing runs the converge again between the worker's calls.
// Once the units come free the side's read names a side nothing zeroes, and
// the re-send that brings takes the run and zeroes.
func TestMigrationDestinationWaitsForRoomBeforeZeroing(t *testing.T) {
	for _, level := range []pb.SpLevel{
		pb.SpLevel_SP_LEVEL_READWRITE,
		pb.SpLevel_SP_LEVEL_NO_MIGRATION,
		pb.SpLevel_SP_LEVEL_DISABLE,
	} {
		t.Run(level.String(), func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			fillMetaArea(t, srv)
			node.Reset()

			req := waitingDstReq(1, level)
			reply, err := srv.SyncupSide(ctx, req)
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if got := reply.GetAgentReply(); got.GetCode() != 0 {
				t.Fatalf("SyncupSide replied %v, want code 0", got)
			}
			info := reply.GetSideInfo()
			assertWaitingRows(t, "SyncupSide", info, level)

			// Nothing runs between the worker's calls, so nothing zeroes.
			assertNothingRunsBetweenCalls(t, node)
			if got := node.callsMatching("blkdiscard --zeroout"); len(got) != 0 {
				t.Errorf("a destination with no run zeroed: %v", got)
			}
			if _, ok, _ := srv.meta.LookupCloneMeta(
				ctx, testSp, testMigrId); ok {
				t.Error("a destination took a run in a full area")
			}

			// The check round reads the same rows at the same epochs, and
			// its verdict is clean.
			check := checkSide(ctx, srv)
			if got := check.GetAgentReply(); got.GetCode() != 0 {
				t.Errorf("CheckSide replied %v, want code 0", got)
			}
			if !proto.Equal(check.GetSideInfo(), info) {
				t.Errorf("the check round reports\n%v\nthe converge\n%v",
					check.GetSideInfo(), info)
			}

			// The other migration ends: the next round names the side, and
			// its re-send takes the run and zeroes.
			freeMetaArea(t, srv)
			round, resent := workerRound(t, srv, req, "round-1")
			assertNothingZeroing(t, "round-1", round)
			if resent == nil {
				t.Fatal("round-1 re-sent nothing")
			}
			waitZeroed(t, srv, testSide)
			assertRunHeld(t, srv, node)
		})
	}
}

// assertNothingZeroing fails unless reply names the fixture side as a side
// nothing zeroes, at 0 of its bytes (zeroingVerdict).
func assertNothingZeroing(
	t *testing.T,
	what string,
	reply interface{ GetAgentReply() *pb.AgentReply },
) {
	t.Helper()
	want := zeroingDetails(0, testZeroBytes) + ": nothing is zeroing the side"
	if got := reply.GetAgentReply(); got.GetCode() !=
		common.ReplyCodeLeftover || !strings.Contains(got.GetDetails(), want) {
		t.Errorf("%s: verdict %v, want a leftover naming %q", what, got,
			want)
	}
}

// At a level that suppresses the destination role the run is still taken
// before zeroing, and the side's read leaves the wait out only while the
// side device row it just made reads the area's refusal (readsRunRefusal):
// a read whose check of the side device does not answer reads that row in
// error, and a converge would act on the side device before the run, so the
// read names the side, and its re-send, which converges the side device and
// waits for the run again, is followed by a round that names nothing. Once
// the units come free the read names the side again, and the re-send takes
// the run and zeroes.
func TestAWaitingDestinationIsNamedWhileItsSideDeviceIsUnread(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	fillMetaArea(t, srv)
	req := waitingDstReq(1, pb.SpLevel_SP_LEVEL_NO_MIGRATION)
	reply, err := srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := reply.GetAgentReply(); got.GetCode() != 0 {
		t.Fatalf("fixture is wrong: the waiting destination replied %v", got)
	}

	// The next read's check of the side device is killed.
	stop := "dmsetup table " + srv.nf.DnSideName(
		testCluster, testDn, testSp, testSide)
	setHook(node, node.killCmdNoEffect, stop)
	round, resent := workerRound(t, srv, req, "round-1")
	node.mu.Lock()
	armed := node.killCmdNoEffect[stop]
	node.mu.Unlock()
	if armed {
		t.Fatalf("fixture is wrong: %q never ran", stop)
	}
	if got := round.GetSideInfo().GetSideDevInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("fixture is wrong: round-1's side_dev_info = %v", got)
	}
	assertNothingZeroing(t, "round-1", round)
	if resent == nil {
		t.Fatal("round-1 re-sent nothing")
	}
	assertWaitingRows(t, "round-1's re-send", resent.GetSideInfo(),
		pb.SpLevel_SP_LEVEL_NO_MIGRATION)
	round, resent = workerRound(t, srv, req, "round-2")
	assertNotNamed(t, "round-2", round)
	if resent != nil {
		t.Errorf("round-2 re-sent: %v", resent.GetAgentReply())
	}
	assertNothingRunsBetweenCalls(t, node, "round-1", "round-2")

	// The other migration ends.
	freeMetaArea(t, srv)
	round, resent = workerRound(t, srv, req, "round-3")
	assertNothingZeroing(t, "round-3", round)
	if resent == nil {
		t.Fatal("round-3 re-sent nothing")
	}
	waitZeroed(t, srv, testSide)
	assertRunHeld(t, srv, node)
}

// A destination whose side device reads suspended while its record is not
// fully zeroed and the area has no free run is named a side nothing zeroes
// (DN16): the area's refusal alone leaves out nothing, since the read's own
// side device row reads the suspended device, not the wait, and the
// converge a re-send brings resumes that device before it reaches the run.
// After the re-send the row reads the wait again, and a round names nothing.
func TestAWaitingDestinationWithASuspendedSideDeviceIsNamed(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	fillMetaArea(t, srv)
	req := waitingDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)
	if _, err := srv.SyncupSide(ctx, req); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := checkSide(ctx, srv).GetAgentReply(); got.GetCode() != 0 {
		t.Fatalf("fixture is wrong: the waiting destination's verdict = %v",
			got)
	}

	sideDev := srv.nf.DnSideName(testCluster, testDn, testSp, testSide)
	node.mu.Lock()
	node.dms[sideDev].suspended = true
	node.mu.Unlock()
	node.Reset()
	round, resent := workerRound(t, srv, req, "round-1")
	if got := round.GetSideInfo().GetSideDevInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR ||
		got.GetDetails() != detailsSuspended {
		t.Fatalf("fixture is wrong: round-1's side_dev_info = %v/%q, "+
			"want ERROR/%q", got.GetStatus(), got.GetDetails(),
			detailsSuspended)
	}
	assertNothingZeroing(t, "round-1", round)
	if resent == nil {
		t.Fatal("round-1 re-sent nothing")
	}
	if !tracedCall(node, "round-1", "cmd dmsetup resume "+sideDev) {
		t.Error("the re-send did not resume the side device")
	}
	if dmSuspended(node, sideDev) {
		t.Error("the side device is still suspended")
	}
	assertWaitingRows(t, "round-1's re-send", resent.GetSideInfo(),
		pb.SpLevel_SP_LEVEL_READWRITE)
	round, resent = workerRound(t, srv, req, "round-2")
	assertNotNamed(t, "round-2", round)
	if resent != nil {
		t.Errorf("round-2 re-sent: %v", resent.GetAgentReply())
	}
}

// A destination that waits for its run zeroes nothing by design, so the
// side verdict does not name it a side nothing zeroes (DN16): that leftover
// would have the worker re-send its SyncupSide every round only to repeat
// the wait. Once the area could supply the run and still nothing zeroes, the
// verdict names it, and the SyncupSide the worker then re-sends takes the
// run and zeroes.
func TestAWaitingDestinationIsNotALeftover(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	fillMetaArea(t, srv)
	req := waitingDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)
	if _, err := srv.SyncupSide(ctx, req); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := checkSide(ctx, srv).GetAgentReply(); got.GetCode() != 0 {
		t.Errorf("the verdict of a waiting destination = %v, want code 0",
			got)
	}

	freeMetaArea(t, srv)
	node.Reset()
	round, resent := workerRound(t, srv, req, "round-1")
	assertNothingZeroing(t, "the verdict once the area has room", round)
	if resent == nil {
		t.Fatal("round-1 re-sent nothing")
	}
	waitZeroed(t, srv, testSide)
	assertRunHeld(t, srv, node)
	if got := checkSide(ctx, srv).GetAgentReply(); got.GetCode() != 0 {
		t.Errorf("the verdict once zeroed = %v, want code 0", got)
	}
}

// A waiting destination whose pass stopped before the run's step — here its
// side device's first `dmsetup create` fails — is named a side nothing zeroes
// although the area still has no free run (DN16): its side device row reads
// the failure, not the area's wait, and a converge would build the device.
// The SyncupSide the worker then re-sends builds the side device and reaches
// the run's step, which waits, and the next verdict is clean. Once the units
// come free the verdict names the side again, and the re-send takes the run
// and zeroes.
func TestAWaitingDestinationStoppedBeforeItsRunIsALeftover(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	fillMetaArea(t, srv)
	sideDevName := srv.nf.DnSideName(testCluster, testDn, testSp, testSide)
	node.mu.Lock()
	node.failCmd["dmsetup create "+sideDevName] = "refused once"
	node.mu.Unlock()
	req := waitingDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)
	reply, err := srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := reply.GetSideInfo().GetSideDevInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("fixture is wrong: side_dev_info = %v, want ERROR", got)
	}

	// The worker re-sends the SyncupSide.
	round, resent := workerRound(t, srv, req, "round-1")
	assertNothingZeroing(t, "round-1", round)
	if resent == nil {
		t.Fatal("round-1 re-sent nothing")
	}
	assertWaitingRows(t, "the re-sent SyncupSide", resent.GetSideInfo(),
		pb.SpLevel_SP_LEVEL_READWRITE)
	round, resent = workerRound(t, srv, req, "round-2")
	assertNotNamed(t, "round-2", round)
	if resent != nil {
		t.Errorf("round-2 re-sent: %v", resent.GetAgentReply())
	}
	assertNothingRunsBetweenCalls(t, node, "round-1", "round-2")

	// The other migration ends.
	freeMetaArea(t, srv)
	round, resent = workerRound(t, srv, req, "round-3")
	assertNothingZeroing(t, "round-3", round)
	if resent == nil {
		t.Fatal("round-3 re-sent nothing")
	}
	waitZeroed(t, srv, testSide)
	assertRunHeld(t, srv, node)
}

// A refusal of the run other than "no free run" that a re-sent request may
// not meet — here the write of its head fails — is a fault, not a wait:
// side_dev_info reads ERROR with the write's error while the rows above it
// keep the gate's own text, nothing zeroes, and the verdict names a side
// nothing zeroes, since the read finds a run the area could supply. Nothing
// runs the converge again between the worker's calls; once the disk takes
// the write again, the next round's re-send takes the run and zeroes (DN13).
func TestMigrationDestinationRunErrorIsAnError(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.mu.Lock()
	node.failBlockSet, node.failBlockWrite = true, common.DnCloneMetaOffset
	node.mu.Unlock()
	node.Reset()

	req := waitingDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)
	reply, err := srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	info := reply.GetSideInfo()
	wantErr := fmt.Sprintf("zeroing the clone-metadata slot at %d: fake: "+
		"write block at %d failed", common.DnCloneMetaOffset,
		common.DnCloneMetaOffset)
	if got := info.GetSideDevInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR || got.GetDetails() != wantErr {
		t.Errorf("side_dev_info = %v/%q, want ERROR/%q", got.GetStatus(),
			got.GetDetails(), wantErr)
	}
	assertProvisioning(t, "migr_dst target_info",
		info.GetMigrDstInfo().GetTargetInfo(), tagProvisioningWait)
	assertProvisioning(t, "migr_dst dm_clone_info",
		info.GetMigrDstInfo().GetDmCloneInfo(), tagProvisioningWait)
	assertNothingRunsBetweenCalls(t, node)
	if got := node.callsMatching("blkdiscard --zeroout"); len(got) != 0 {
		t.Errorf("a destination whose run failed zeroed: %v", got)
	}
	if _, ok, _ := srv.meta.LookupCloneMeta(ctx, testSp, testMigrId); ok {
		t.Error("a run whose head was not zeroed was recorded")
	}
	assertNothingZeroing(t, "the verdict while the run fails",
		checkSide(ctx, srv))

	// The disk takes the write again.
	node.mu.Lock()
	node.failBlockSet = false
	node.mu.Unlock()
	round, resent := workerRound(t, srv, req, "round-1")
	assertNothingZeroing(t, "round-1", round)
	if resent == nil {
		t.Fatal("round-1 re-sent nothing")
	}
	waitZeroed(t, srv, testSide)
	assertRunHeld(t, srv, node)
}

// A destination whose request carries no usable block size sizes no run, and
// a re-sent request meets that refusal the same way, so neither channel
// names it (DN16), and no round re-sends it. Before it provisions,
// side_dev_info reads ERROR with the same text on the converge's reply and
// on the check round, so the rows do not trade between the two channels, and
// nothing zeroes; a side device a converge would act on first — here one
// left suspended — is named all the same, and the re-send that brings
// resumes it. Once provisioned, its two destination rows read the refusal at
// DN13 step (2), and are not named either.
func TestADestinationWithNoBlockSizeIsAnError(t *testing.T) {
	const want = "migr_dst_conf.block_size is 0"
	t.Run("before it provisions", func(t *testing.T) {
		srv, node := newTestServer(t)
		ctx := context.Background()
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		req := waitingDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)
		req.MigrDstConf.BlockSize = 0
		reply, err := srv.SyncupSide(ctx, req)
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		synced := reply.GetSideInfo().GetSideDevInfo()
		if synced.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
			synced.GetDetails() != want {
			t.Errorf("side_dev_info = %v/%q, want ERROR/%q",
				synced.GetStatus(), synced.GetDetails(), want)
		}
		assertNotNamed(t, "the converge", reply)
		round, resent := workerRound(t, srv, req, "round-1")
		if probed := round.GetSideInfo().GetSideDevInfo(); !proto.Equal(
			probed, synced) {
			t.Errorf("the check round reports side_dev_info %v, the "+
				"converge %v", probed, synced)
		}
		assertNotNamed(t, "round-1", round)
		if resent != nil {
			t.Errorf("round-1 re-sent: %v", resent.GetAgentReply())
		}
		assertNothingRunsBetweenCalls(t, node, "round-1")
		if got := node.callsMatching("blkdiscard --zeroout"); len(got) != 0 {
			t.Errorf("a destination with no run zeroed: %v", got)
		}

		// The side device is left suspended.
		sideDev := srv.nf.DnSideName(testCluster, testDn, testSp, testSide)
		node.mu.Lock()
		node.dms[sideDev].suspended = true
		node.mu.Unlock()
		round, resent = workerRound(t, srv, req, "round-2")
		if got := round.GetSideInfo().GetSideDevInfo(); got.GetDetails() !=
			detailsSuspended {
			t.Fatalf("fixture is wrong: round-2's side_dev_info = %v", got)
		}
		assertNothingZeroing(t, "round-2", round)
		if resent == nil {
			t.Fatal("round-2 re-sent nothing")
		}
		if dmSuspended(node, sideDev) {
			t.Error("the re-send left the side device suspended")
		}
		round, resent = workerRound(t, srv, req, "round-3")
		assertNotNamed(t, "round-3", round)
		if resent != nil {
			t.Errorf("round-3 re-sent: %v", resent.GetAgentReply())
		}
	})
	t.Run("once provisioned", func(t *testing.T) {
		srv, node := newTestServer(t)
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		req := migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)
		req.MigrDstConf.BlockSize = 0
		reply, err := srv.SyncupSide(ctx, req)
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		dst := reply.GetSideInfo().GetMigrDstInfo()
		if got := dst.GetDmCloneInfo(); got.GetStatus() !=
			pb.ResStatus_RES_STATUS_ERROR || got.GetDetails() != want {
			t.Errorf("dm_clone_info = %v/%q, want ERROR/%q",
				got.GetStatus(), got.GetDetails(), want)
		}
		if got := dst.GetTargetInfo(); got.GetStatus() !=
			pb.ResStatus_RES_STATUS_MISSING {
			t.Errorf("target_info = %v/%q, want MISSING", got.GetStatus(),
				got.GetDetails())
		}
		assertNotNamed(t, "the converge", reply)
		round, resent := workerRound(t, srv, req, "round-1")
		assertNotNamed(t, "round-1", round)
		if resent != nil {
			t.Errorf("round-1 re-sent: %v", resent.GetAgentReply())
		}
		assertNothingRunsBetweenCalls(t, node, "round-1")
	})
}

// Cancelling a migration drops the destination side's pointer from its DN's
// list (architecture.md, Migrations): the side's state goes, with its
// zeroing loop, and the node-level sweep frees its run's record
// along with the side's own, since with the side dropped no held side claims
// the run and no wrapper maps it (DN6). A destination that only waits for a
// run holds none, and leaves nothing running either.
func TestACancelledDestinationReleasesItsRun(t *testing.T) {
	ctx := context.Background()
	key := sideKey(testCluster, testDn, testSp, testSide)
	assertReleased := func(t *testing.T, srv *DnAgentServer, node *fakeNode) {
		t.Helper()
		if srv.getSide(key) != nil {
			t.Error("the side's state survived the cancel")
		}
		if _, ok, err := srv.meta.LookupCloneMeta(
			ctx, testSp, testMigrId); ok || err != nil {
			t.Errorf("the destination's run after the cancel: found %v, %v",
				ok, err)
		}
		if _, ok, err := srv.meta.LookupSide(
			ctx, testSp, testSide); ok || err != nil {
			t.Errorf("the side's record after the cancel: found %v, %v",
				ok, err)
		}
		if dmPresent(node, srv.nf.DnSideName(
			testCluster, testDn, testSp, testSide)) {
			t.Error("the side device survived the cancel")
		}
		joined := make(chan struct{})
		go func() {
			srv.WaitBackground()
			close(joined)
		}()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Fatal("a goroutine of the cancelled destination still runs")
		}
	}

	t.Run("holding its run", func(t *testing.T) {
		srv, node := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		// Every batch fails, so the side is still zeroing at the cancel.
		setFailAlways(node, "blkdiscard --zeroout", "stalled")
		if _, err := srv.SyncupSide(ctx, waitingDstReq(
			1, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if _, ok, _ := srv.meta.LookupCloneMeta(
			ctx, testSp, testMigrId); !ok {
			t.Fatal("fixture is wrong: the destination holds no run")
		}
		waitUntil(t, "a zeroing batch", func() bool {
			return node.hasCall("blkdiscard --zeroout")
		})

		if _, err := srv.SyncupDn(ctx, dnReq(2)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		assertReleased(t, srv, node)
		if got, want := srv.meta.Describe(), fmt.Sprintf(
			"free_meta_units=%d", common.DnCloneMetaUnitCnt); !strings.
			Contains(got, want) {
			t.Errorf("Describe = %q, want every unit free again (%s)",
				got, want)
		}
	})

	t.Run("waiting for a run", func(t *testing.T) {
		srv, node := newTestServer(t)
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		fillMetaArea(t, srv)
		if _, err := srv.SyncupSide(ctx, waitingDstReq(
			1, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}

		if _, err := srv.SyncupDn(ctx, dnReq(2)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		assertReleased(t, srv, node)
	})
}
