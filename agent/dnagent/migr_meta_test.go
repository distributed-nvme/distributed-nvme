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
// text, the rule both agents share.
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
// epochs and answers code 0, and the DN8 retry takes the run, and then
// zeroes, with no RPC once the units come free.
func TestMigrationDestinationWaitsForRoomBeforeZeroing(t *testing.T) {
	for _, level := range []pb.SpLevel{
		pb.SpLevel_SP_LEVEL_READWRITE,
		pb.SpLevel_SP_LEVEL_NO_MIGRATION,
		pb.SpLevel_SP_LEVEL_DISABLE,
	} {
		t.Run(level.String(), func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.migrRetryInterval = 5 * time.Millisecond
			ctx := context.Background()
			if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			fillMetaArea(t, srv)
			node.Reset()

			reply, err := srv.SyncupSide(ctx, waitingDstReq(1, level))
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if got := reply.GetAgentReply(); got.GetCode() != 0 {
				t.Fatalf("SyncupSide replied %v, want code 0", got)
			}
			info := reply.GetSideInfo()
			assertWaitingRows(t, "SyncupSide", info, level)
			if !migrRetrying(srv) {
				t.Error("no background retry was registered")
			}

			// The retry's passes come and go, and none of them zeroes.
			waitUntil(t, "three retry passes", func() bool {
				return convergePasses(node) >= 4
			})
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

			// The other migration ends; no RPC follows.
			freeMetaArea(t, srv)
			waitZeroed(t, srv, testSide)
			waitUntil(t, "the retry to end", func() bool {
				return !migrRetrying(srv)
			})
			assertRunHeld(t, srv, node)
		})
	}
}

// At a level that suppresses the destination role, the run is still taken
// before zeroing, so the retry that waits for it stays registered through
// every pass, a pass that stops before the run included: the sweep's
// pre-step leaves it to the run's own step (sidePreSteps). Were the pre-step
// to stop it, a pass whose side device did not converge would end the wait,
// and nothing but an RPC would take the run once units came free.
func TestAWaitingDestinationKeepsItsRetryWhenAPassStopsEarly(t *testing.T) {
	srv, node := newTestServer(t)
	srv.migrRetryInterval = 5 * time.Millisecond
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	fillMetaArea(t, srv)
	if _, err := srv.SyncupSide(ctx, waitingDstReq(
		1, pb.SpLevel_SP_LEVEL_NO_MIGRATION)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !migrRetrying(srv) {
		t.Fatal("fixture is wrong: the waiting destination has no retry")
	}

	// One retry pass stops at the side device, before the run.
	stop := "dmsetup table " + srv.nf.DnSideName(
		testCluster, testDn, testSp, testSide)
	setHook(node, node.killCmdNoEffect, stop)
	waitUntil(t, "a pass to stop at the side device", func() bool {
		node.mu.Lock()
		defer node.mu.Unlock()
		return !node.killCmdNoEffect[stop]
	})
	node.Reset()
	waitUntil(t, "the retry to run on", func() bool {
		return convergePasses(node) >= 2
	})

	// The other migration ends; no RPC follows.
	freeMetaArea(t, srv)
	waitZeroed(t, srv, testSide)
	waitUntil(t, "the retry to end", func() bool {
		return !migrRetrying(srv)
	})
}

// A destination that waits for its run zeroes nothing by design, so the
// side verdict does not name it a side nothing zeroes (DN16): that leftover
// would have the worker re-send its SyncupSide every round only to repeat
// the wait its own retry covers. Once the area could supply the run and
// still nothing zeroes — the retry has not run yet — the verdict names it
// again, and the SyncupSide the worker then re-sends takes the run and
// zeroes.
func TestAWaitingDestinationIsNotALeftover(t *testing.T) {
	srv, node := newTestServer(t)
	// No retry pass runs: every pass here is the test's.
	srv.migrRetryInterval = time.Hour
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
	wantLeftover := zeroingDetails(0, testZeroBytes) +
		": nothing is zeroing the side"
	if got := checkSide(ctx, srv).GetAgentReply(); got.GetCode() !=
		common.ReplyCodeLeftover ||
		!strings.Contains(got.GetDetails(), wantLeftover) {
		t.Errorf("the verdict once the area has room = %v, want a leftover "+
			"naming %q", got, wantLeftover)
	}

	node.Reset()
	if _, err := srv.SyncupSide(ctx, req); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	waitZeroed(t, srv, testSide)
	assertRunHeld(t, srv, node)
	if migrRetrying(srv) {
		t.Error("the pass that took the run left the retry registered")
	}
	if got := checkSide(ctx, srv).GetAgentReply(); got.GetCode() != 0 {
		t.Errorf("the verdict once zeroed = %v, want code 0", got)
	}
}

// The exemption above rests on the DN8 retry, and only the run's own step
// registers it while the side has not provisioned, so a destination whose
// pass stopped before that step — here its side device's first `dmsetup
// create` fails — has none, and nothing would take the run once units came
// free. The verdict names it a side nothing zeroes although the area still
// has no free run (DN16); the SyncupSide the worker then re-sends reaches the
// run's step, which registers the retry, and the next verdict is clean. The
// retry takes the run and zeroes with no further RPC once the units come
// free.
func TestAWaitingDestinationWithNoRetryIsALeftover(t *testing.T) {
	srv, node := newTestServer(t)
	srv.migrRetryInterval = 5 * time.Millisecond
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
	if migrRetrying(srv) {
		t.Fatal("fixture is wrong: a pass that stopped at the side device " +
			"registered the retry")
	}
	wantLeftover := zeroingDetails(0, testZeroBytes) +
		": nothing is zeroing the side"
	if got := checkSide(ctx, srv).GetAgentReply(); got.GetCode() !=
		common.ReplyCodeLeftover ||
		!strings.Contains(got.GetDetails(), wantLeftover) {
		t.Errorf("the verdict with no retry registered = %v, want a "+
			"leftover naming %q", got, wantLeftover)
	}

	// The worker re-sends the SyncupSide.
	reply, err = srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertWaitingRows(t, "the re-sent SyncupSide", reply.GetSideInfo(),
		pb.SpLevel_SP_LEVEL_READWRITE)
	if !migrRetrying(srv) {
		t.Error("the pass that reached the run's step registered no retry")
	}
	if got := checkSide(ctx, srv).GetAgentReply(); got.GetCode() != 0 {
		t.Errorf("the verdict of a waiting destination with its retry = "+
			"%v, want code 0", got)
	}

	// The other migration ends; no RPC follows.
	freeMetaArea(t, srv)
	waitZeroed(t, srv, testSide)
	waitUntil(t, "the retry to end", func() bool {
		return !migrRetrying(srv)
	})
	assertRunHeld(t, srv, node)
}

// A refusal of the run other than "no free run" — here the write of its head
// fails — is a fault, not a wait: side_dev_info reads ERROR with the write's
// error while the rows above it keep the gate's own text, nothing zeroes,
// and the verdict names a side nothing zeroes. The DN8 retry is registered
// all the same, and it takes the run and zeroes with no RPC once the disk
// takes the write again (DN13).
func TestMigrationDestinationRunErrorIsAnError(t *testing.T) {
	srv, node := newTestServer(t)
	srv.migrRetryInterval = 5 * time.Millisecond
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.mu.Lock()
	node.failBlockSet, node.failBlockWrite = true, common.DnCloneMetaOffset
	node.mu.Unlock()
	node.Reset()

	reply, err := srv.SyncupSide(ctx,
		waitingDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE))
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
	if !migrRetrying(srv) {
		t.Error("no background retry was registered")
	}
	waitUntil(t, "two retry passes", func() bool {
		return convergePasses(node) >= 3
	})
	if got := node.callsMatching("blkdiscard --zeroout"); len(got) != 0 {
		t.Errorf("a destination whose run failed zeroed: %v", got)
	}
	if _, ok, _ := srv.meta.LookupCloneMeta(ctx, testSp, testMigrId); ok {
		t.Error("a run whose head was not zeroed was recorded")
	}
	wantLeftover := zeroingDetails(0, testZeroBytes) +
		": nothing is zeroing the side"
	if got := checkSide(ctx, srv).GetAgentReply(); got.GetCode() !=
		common.ReplyCodeLeftover ||
		!strings.Contains(got.GetDetails(), wantLeftover) {
		t.Errorf("the verdict while the run fails = %v, want a leftover "+
			"naming %q", got, wantLeftover)
	}

	// The disk takes the write again; no RPC follows.
	node.mu.Lock()
	node.failBlockSet = false
	node.mu.Unlock()
	waitZeroed(t, srv, testSide)
	waitUntil(t, "the retry to end", func() bool {
		return !migrRetrying(srv)
	})
	assertRunHeld(t, srv, node)
}

// A destination whose request carries no usable block size sizes no run:
// side_dev_info reads ERROR with the same text on the converge's reply and on
// the check round, so the rows do not trade between the two channels; the
// retry is registered, nothing zeroes, and the verdict names a side nothing
// zeroes.
func TestADestinationWithNoBlockSizeIsAnError(t *testing.T) {
	srv, node := newTestServer(t)
	// No retry pass runs: every pass here is the test's.
	srv.migrRetryInterval = time.Hour
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
	const want = "migr_dst_conf.block_size is 0"
	synced := reply.GetSideInfo().GetSideDevInfo()
	if synced.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		synced.GetDetails() != want {
		t.Errorf("side_dev_info = %v/%q, want ERROR/%q", synced.GetStatus(),
			synced.GetDetails(), want)
	}
	if !migrRetrying(srv) {
		t.Error("no background retry was registered")
	}
	check := checkSide(ctx, srv)
	if probed := check.GetSideInfo().GetSideDevInfo(); !proto.Equal(
		probed, synced) {
		t.Errorf("the check round reports side_dev_info %v, the converge %v",
			probed, synced)
	}
	if got := check.GetAgentReply(); got.GetCode() !=
		common.ReplyCodeLeftover ||
		!strings.Contains(got.GetDetails(), "nothing is zeroing the side") {
		t.Errorf("the verdict = %v, want a leftover naming a side nothing "+
			"zeroes", got)
	}
	if got := node.callsMatching("blkdiscard --zeroout"); len(got) != 0 {
		t.Errorf("a destination with no run zeroed: %v", got)
	}
}

// Cancelling a migration drops the destination side's pointer from its DN's
// list (architecture.md, Migrations): the side's state goes, with its
// zeroing loop and its retry, and the node-level sweep frees its run's record
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
		srv.migrRetryInterval = 5 * time.Millisecond
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
		srv.migrRetryInterval = 5 * time.Millisecond
		if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		fillMetaArea(t, srv)
		if _, err := srv.SyncupSide(ctx, waitingDstReq(
			1, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if !migrRetrying(srv) {
			t.Fatal("fixture is wrong: the waiting destination has no retry")
		}

		if _, err := srv.SyncupDn(ctx, dnReq(2)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		assertReleased(t, srv, node)
	})
}
