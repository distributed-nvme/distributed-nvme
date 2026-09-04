package cnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The clone fixture mirrors cnagent_integtest.md §13: a 64 MiB destination td
// whose source wrote only its first 32 MiB, so the pushed chunk
// `00000000ffffffff` (wire: 1 = never written = skippable) must coalesce into
// exactly one 32 MiB blkdiscard at offset 32 MiB.
const (
	testSrcNqn = "nqn.2024-01.io.dnv:4:" +
		"0000000000000001:00000000000003d1:000000000000000c"
	// The allocator tests run two and three clones at once; each has its own
	// source subsystem, so retiring one never disturbs another's connection.
	testSrcNqn2 = testSrcNqn + "2"
	testSrcNqn3 = testSrcNqn + "3"
	testSkipHex = "00000000ffffffff"
)

func cloneOf() *pb.Clone {
	return &pb.Clone{
		CloneId:       testClone,
		SrcTrConfList: []*pb.NvmeTrConf{sideTrConf(testIp2, testSvcId2)},
		SrcNqn:        testSrcNqn,
		SrcNsIdx:      1,
		SrcSliceCnt:   1,
		SrcStripeSize: testStripeSize,
		SrcBlockSize:  testBlockSize,
		DstTdId:       testTd,
		DmCloneConf: &pb.DmCloneConf{
			HydrationThreshold: 1, HydrationBatchSize: 1},
		AutoResume: true,
		BmCnt:      1,
	}
}

// cloneOfTd is a second (or third) clone: its own id, its own source
// subsystem and its own destination td, so several clones can be converged at
// once without sharing a connection or a raid0.
func cloneOfTd(cloneId uint64, srcNqn string, dstTdId uint64) *pb.Clone {
	clone := cloneOf()
	clone.CloneId = cloneId
	clone.SrcNqn = srcNqn
	clone.DstTdId = dstTdId
	return clone
}

func pushBitmap(
	t *testing.T,
	srv *CnAgentServer,
	revision uint64,
	bitmap []byte,
) {
	t.Helper()
	reply, err := srv.PushCloneBitmap(context.Background(),
		&pb.PushCloneBitmapRequest{
			ClusterId:    testCluster,
			CnId:         testCn,
			CntlrPointer: cntlrPtr(),
			Revision:     revision,
			CloneId:      testClone,
			BmIdx:        0,
			Bitmap:       bitmap,
		})
	if err != nil {
		t.Fatalf("PushCloneBitmap: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("PushCloneBitmap rejected: %v", reply.GetAgentReply())
	}
}

func hexBytes(t *testing.T, text string) []byte {
	t.Helper()
	out := make([]byte, len(text)/2)
	for i := range out {
		var value uint32
		if _, err := fmt.Sscanf(text[2*i:2*i+2], "%02x", &value); err != nil {
			t.Fatalf("bad hex %q: %v", text, err)
		}
		out[i] = byte(value)
	}
	return out
}

// ---------------------------------------------------------------------------
// §6.9 — clone build, §11.5 recovery and teardown (CN18)
// ---------------------------------------------------------------------------

func TestCloneBuild(t *testing.T) {
	srv, node := newTestServer(t)
	// Stage 1: the clone is declared but gated by the level, so the bitmap
	// push cannot race the dm-clone (the CN19 staged gate).
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()},
		level: pb.SpLevel_SP_LEVEL_NO_CLONE})); err != nil {
		t.Fatalf("gated clone: %v", err)
	}
	assertNoCall(t, node, "cmd nvme connect --transport tcp --traddr "+
		testIp2+" --trsvcid "+testSvcId2)

	// Stage 2: the chunk lands while nothing serves it.
	node.Reset()
	pushBitmap(t, srv, 3, hexBytes(t, testSkipHex))
	assertNoCall(t, node, "cmd blkdiscard")
	bmPath := srv.nf.LocalCloneBmPath(
		testCluster, testCn, testSp, testClone, 0)
	if _, ok := node.protos[bmPath]; !ok {
		t.Fatalf("the chunk was not persisted at %s", bmPath)
	}

	// Stage 3: enabling the clone runs the whole CN18 sequence.
	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 4, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	// The fixture's 64 MiB td over a 1 MiB block gives 64 regions, so the
	// CN18 step 2 budget is ceil((4 MiB + 64) / 4 MiB) = 2 units = 8 MiB =
	// 16384 sectors, first-fit at unit 0 of an empty arena.
	metaDm := cloneMetaName(srv, testClone)
	loop := loopDev(t, srv, node)
	assertOrder(t, node,
		"cmd nvme connect --transport tcp --traddr "+testIp2,
		"cmd blkdiscard --offset 0 --length 8388608 "+loop,
		"cmd dmsetup create "+metaDm,
		"cmd dmsetup create "+cloneName(srv, testClone),
		"cmd blkdiscard --offset 33554432 --length 33554432",
		"cmd dmsetup message "+cloneName(srv, testClone)+
			" 0 enable_hydration",
		"cmd dmsetup reload "+nsDevName(srv, testNs),
	)
	// The wrapper is a plain linear over the arena's first slot: the dm-clone
	// reads its superblock from sector 0 and takes no offset, which is the
	// whole reason the wrapper exists ([D14]).
	if got, want := node.dms[metaDm].table,
		"0 16384 linear "+node.devNo[loop]+" 0"; got != want {
		t.Fatalf("wrapper table is %q, want %q", got, want)
	}
	// The table is created with hydration off, always — and with discard
	// passdown off, without which every `blkdiscard` below would be remapped
	// to the destination raid0 and unmap blocks it already owns (CN18 step 3).
	create := node.callsMatching(
		"cmd dmsetup create " + cloneName(srv, testClone))
	if len(create) != 1 ||
		!strings.Contains(create[0], "2 no_hydration no_discard_passdown") {
		t.Fatalf("dm-clone features are wrong: %v", create)
	}
	// Exactly one coalesced hydration discard: skip bits 32..63 → 32 MiB at
	// 32 MiB. The arena hole-punch is the other `blkdiscard` of this pass and
	// is counted separately — the two are on different devices and mean
	// completely different things (update_01.md U3 §4.3).
	discards := node.callsMatching("cmd blkdiscard --offset 33554432")
	if len(discards) != 1 {
		t.Fatalf("want 1 blkdiscard, got %d: %v", len(discards), discards)
	}
	punches := node.callsMatching(
		"cmd blkdiscard --offset 0 --length 8388608 " + loop)
	if len(punches) != 1 {
		t.Fatalf("want 1 arena hole punch, got %d: %v", len(punches), punches)
	}
	// The ns-dev now sits on the dm-clone, serving despite the stored
	// suspended flag — the auto_resume override of CN16.
	table := node.dms[nsDevName(srv, testNs)].table
	cloneNo := node.devNo["/dev/mapper/"+cloneName(srv, testClone)]
	if !strings.Contains(table, "linear "+cloneNo) {
		t.Fatalf("ns-dev table %q is not on the dm-clone", table)
	}

	info := reply.GetCntlrInfo()
	assertOk(t, info.GetCloneIdToTarget()[testClone], "clone target")
	assertOk(t, info.GetCloneIdToDmClone()[testClone], "dm-clone")
	assertOk(t, info.GetCloneIdToMeta()[testClone], "clone meta")

	// CN20: bm_info_list reports the applied set, derived from the files.
	if len(reply.GetBmInfoList()) != 1 ||
		reply.GetBmInfoList()[0].GetResId() != testClone ||
		len(reply.GetBmInfoList()[0].GetBmIdxList()) != 1 ||
		reply.GetBmInfoList()[0].GetBmIdxList()[0] != 0 {
		t.Fatalf("bm_info_list is %v", reply.GetBmInfoList())
	}
}

func TestCloneAutoResumeOverridesSuspended(t *testing.T) {
	srv, node := newTestServer(t)
	// The destination namespace is created suspended (§11.3) and serves
	// anyway while the clone runs.
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, suspended: true,
		clones: []*pb.Clone{cloneOf()}})
	if node.dms[nsDevName(srv, testNs)].suspended {
		t.Fatalf("auto_resume did not override the suspended flag")
	}
	if got := node.files[anaPath(testNqn, 1)]; got != "1" {
		t.Fatalf("ana_grpid is %q, want 1 (optimized)", got)
	}

	// auto_resume = false leaves it effectively suspended.
	clone := cloneOf()
	clone.AutoResume = false
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, suspended: true,
		clones: []*pb.Clone{clone}})); err != nil {
		t.Fatalf("auto_resume=false: %v", err)
	}
	if !node.dms[nsDevName(srv, testNs)].suspended {
		t.Fatalf("a suspended dst namespace must stay suspended")
	}
}

// TestCloneRecovery is the §11.5 rebuild: the volatile metadata wrapper is
// gone, so the destination thin bitmaps are read and applied before hydration
// is ever enabled, with the ns-devs parked on dm-error throughout.
func TestCloneRecovery(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})

	// The tmpfs arena did not survive: drop the wrapper (and the dm-clone
	// that mapped it) behind the agent's back, exactly as a CN reboot does.
	metaDm := cloneMetaName(srv, testClone)
	loop := loopDev(t, srv, node)
	removeCloneDevice(node, srv)
	delete(node.dms, metaDm)
	delete(node.devNo, "/dev/mapper/"+metaDm)
	delete(node.devSize, "/dev/mapper/"+metaDm)

	// The destination has blocks 0..3 mapped ⇒ regions 0..3 are already
	// copied and must be discarded before hydration starts.
	metaPath := srv.nf.DmPath(srv.nf.CnPoolMetaName(
		testCluster, testCn, testSp, testSlice))
	node.thinDumps[metaPath] = `<superblock uuid="" time="0" transaction="0" ` +
		`flags="0" version="2" data_block_size="2048" nr_data_blocks="0">
  <device dev_id="1" mapped_blocks="4" transaction="0" creation_time="0" snap_time="0">
    <range_mapping origin_begin="0" data_begin="0" length="4" time="0"/>
  </device>
</superblock>
`

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true,
		clones: []*pb.Clone{cloneOf()}})); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	pool := poolName(srv)
	assertOrder(t, node,
		"cmd dmsetup reload "+nsDevName(srv, testNs),
		"cmd blkdiscard --offset 0 --length 8388608 "+loop,
		"cmd dmsetup create "+metaDm,
		"cmd dmsetup create "+cloneName(srv, testClone),
		"cmd dmsetup message "+pool+" 0 reserve_metadata_snap",
		"cmd thin_dump --metadata-snap "+metaPath,
		"cmd dmsetup message "+pool+" 0 release_metadata_snap",
		"cmd blkdiscard --offset 0 --length 4194304",
		"cmd dmsetup message "+cloneName(srv, testClone)+
			" 0 enable_hydration",
		"cmd dmsetup reload "+nsDevName(srv, testNs),
	)
	// Parking comes before the metadata snapshot is even taken (§11.5 step
	// 1): nothing may serve the td while the bitmaps are being applied.
	park := node.indexOfCall("cmd dmsetup reload " + nsDevName(srv, testNs))
	snap := node.indexOfCall(
		"cmd dmsetup message " + pool + " 0 reserve_metadata_snap")
	if park > snap {
		t.Fatalf("the ns-dev was not parked before the snapshot")
	}
}

func TestCloneMetadataSnapAlwaysReleased(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	metaPath := srv.nf.DmPath(srv.nf.CnPoolMetaName(
		testCluster, testCn, testSp, testSlice))
	node.Reset()
	node.failCmdAlways["thin_dump"] = "thin_dump: corrupt metadata"

	_, err := srv.GetThinDeviceBm(context.Background(),
		&pb.GetThinDeviceBmRequest{
			ClusterId: testCluster, CnId: testCn, SpId: testSp,
			CntlrId: testCntlr, TdId: testTd, SliceIdx: 0,
		})
	if err == nil {
		t.Fatalf("a failing thin_dump must fail the RPC")
	}
	pool := poolName(srv)
	assertOrder(t, node,
		"cmd dmsetup message "+pool+" 0 reserve_metadata_snap",
		"cmd thin_dump --metadata-snap "+metaPath,
		"cmd dmsetup message "+pool+" 0 release_metadata_snap",
	)
	if node.dms[pool].heldRoot {
		t.Fatalf("the metadata snapshot leaked")
	}
}

func TestCloneTeardownOrder(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	pushBitmap(t, srv, 2, hexBytes(t, testSkipHex))

	node.Reset()
	// DeleteClone: the clone leaves clone_list and the namespace's stored
	// suspended flag goes back to false.
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true})); err != nil {
		t.Fatalf("delete clone: %v", err)
	}
	assertOrder(t, node,
		"cmd dmsetup reload "+nsDevName(srv, testNs),
		"cmd dmsetup remove "+cloneName(srv, testClone),
		"cmd dmsetup remove "+cloneMetaName(srv, testClone),
		"cmd nvme disconnect --nqn "+testSrcNqn,
		"cmd rm -f "+srv.nf.LocalCloneBmPath(
			testCluster, testCn, testSp, testClone, 0),
	)
	// The ns-dev is back on the raid0, all data local.
	table := node.dms[nsDevName(srv, testNs)].table
	raid0No := node.devNo["/dev/mapper/"+raid0Name(srv, testTd)]
	if !strings.Contains(table, "linear "+raid0No) {
		t.Fatalf("ns-dev table %q is not back on the raid0", table)
	}
}

// ---------------------------------------------------------------------------
// §6.9 — the clone-metadata slot allocator (CN18 step 2, [D14])
// ---------------------------------------------------------------------------

// twoTds is the fixture for the multi-clone allocator cases: a second
// destination td, so two clones can be built at once.
func twoTds() []*pb.ThinDevice {
	return []*pb.ThinDevice{
		{TdId: testTd, DevId: 1, Size: testTdSize},
		{TdId: testSnapTd, DevId: 2, Size: testTdSize},
	}
}

// wrapperTable is the kind-`b` table one clone's slot must carry: a single
// linear over the arena's loop device at its own unit offset.
func wrapperTable(node *fakeNode, loop string, unitStart uint64) string {
	return fmt.Sprintf("0 %d linear %s %d",
		2*cnCloneMetaUnitSectors, node.devNo[loop],
		unitStart*cnCloneMetaUnitSectors)
}

// TestCloneMetaFirstFitAndRecycling pins the allocator: contiguous units,
// first fit over the free runs, and a run freed by a retired clone handed out
// again — hole-punched **before** anything is created, because a freed unit
// still holds the previous clone's valid dm-clone superblock.
func TestCloneMetaFirstFitAndRecycling(t *testing.T) {
	srv, node := newTestServer(t)
	first := cloneOf()
	second := cloneOfTd(testClone2, testSrcNqn2, testSnapTd)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: twoTds(),
		clones: []*pb.Clone{first, second}})
	loop := loopDev(t, srv, node)

	// 2 units each (CN18 step 2), so the second clone lands directly after
	// the first: units [0,2) and [2,4).
	firstDm := cloneMetaName(srv, testClone)
	secondDm := cloneMetaName(srv, testClone2)
	if got, want := node.dms[firstDm].table,
		wrapperTable(node, loop, 0); got != want {
		t.Fatalf("first wrapper table is %q, want %q", got, want)
	}
	if got, want := node.dms[secondDm].table,
		wrapperTable(node, loop, 2); got != want {
		t.Fatalf("second wrapper table is %q, want %q", got, want)
	}
	if !node.hasCall(
		"cmd blkdiscard --offset 8388608 --length 8388608 " + loop) {
		t.Fatalf("the second slot was not hole-punched:\n%s",
			strings.Join(node.Calls(), "\n"))
	}

	// Retiring the first clone removes its wrapper, so its units are free
	// again in the next enumeration — there is no allocation table to update.
	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, tds: twoTds(),
		clones: []*pb.Clone{second}})); err != nil {
		t.Fatalf("retire the first clone: %v", err)
	}
	if _, ok := node.dms[firstDm]; ok {
		t.Fatalf("the retired clone kept its metadata wrapper")
	}

	// A third clone first-fits back into the recycled run.
	node.Reset()
	third := cloneOfTd(testClone3, testSrcNqn3, testTd)
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 4, primary: true, tds: twoTds(),
		clones: []*pb.Clone{second, third}})); err != nil {
		t.Fatalf("third clone: %v", err)
	}
	thirdDm := cloneMetaName(srv, testClone3)
	assertOrder(t, node,
		"cmd blkdiscard --offset 0 --length 8388608 "+loop,
		"cmd dmsetup create "+thirdDm,
	)
	if got, want := node.dms[thirdDm].table,
		wrapperTable(node, loop, 0); got != want {
		t.Fatalf("recycled wrapper table is %q, want %q", got, want)
	}
	// The surviving clone's slot is left strictly alone.
	if got, want := node.dms[secondDm].table,
		wrapperTable(node, loop, 2); got != want {
		t.Fatalf("surviving wrapper table is %q, want %q", got, want)
	}
	assertNoCall(t, node, "cmd dmsetup create "+secondDm)
}

// TestCloneMetaArenaExhaustion is the old lvcreate-ENOSPC equivalent: with no
// contiguous run left the clone's metadata and dm-clone rows go ERROR and
// nothing is built, while the pass itself carries on (CN29).
func TestCloneMetaArenaExhaustion(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	loop := loopDev(t, srv, node)

	// A kind-`b` wrapper claiming every unit of the arena, planted behind the
	// agent's back (a SyncupCn would sweep it as an orphan).
	filler := srv.nf.CnCloneMetaDmName(testCluster, testCn, testSp, 0x999)
	node.dms[filler] = &fakeDm{
		table: fmt.Sprintf("0 %d linear %s 0",
			cnCloneMetaUnitCnt*cnCloneMetaUnitSectors, node.devNo[loop]),
		thinIds: map[uint32]bool{},
	}
	node.devNo["/dev/mapper/"+filler] = "253:200"

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("exhausted arena: %v", err)
	}
	meta := reply.GetCntlrInfo().GetCloneIdToMeta()[testClone]
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(meta.GetDetails(),
			"no contiguous run of 2 clone-metadata units in the "+
				"256-unit arena") {
		t.Fatalf("clone_id_to_meta is %v/%q",
			meta.GetStatus(), meta.GetDetails())
	}
	dm := reply.GetCntlrInfo().GetCloneIdToDmClone()[testClone]
	if dm.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		dm.GetDetails() != "metadata wrapper missing" {
		t.Fatalf("clone_id_to_dm_clone is %v/%q",
			dm.GetStatus(), dm.GetDetails())
	}
	assertNoCall(t, node, "cmd dmsetup create "+cloneName(srv, testClone))
	// The source connection is fine, so its row is not dragged down with it.
	assertOk(t, reply.GetCntlrInfo().GetCloneIdToTarget()[testClone],
		"clone target")
}

// TestCloneMetaOrphanWrapperSwept is the CN2 reconcile bullet: a kind-`b`
// wrapper no stored cntlr's clone_list names is an orphan and its units go
// back to the arena. The sweep runs from the two passes that hold the node
// write lock, so a live clone's wrapper is never mistaken for one.
func TestCloneMetaOrphanWrapperSwept(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	loop := loopDev(t, srv, node)

	orphan := srv.nf.CnCloneMetaDmName(testCluster, testCn, testSp, 0x999)
	node.dms[orphan] = &fakeDm{
		table:   wrapperTable(node, loop, 8),
		thinIds: map[uint32]bool{},
	}
	node.devNo["/dev/mapper/"+orphan] = "253:201"

	node.Reset()
	if _, err := srv.SyncupCn(
		context.Background(), cnReq(3, true)); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	assertOrder(t, node, "cmd dmsetup ls", "cmd dmsetup remove "+orphan)
	if _, ok := node.dms[orphan]; ok {
		t.Fatalf("the orphan wrapper survived the sweep")
	}
	if _, ok := node.dms[cloneMetaName(srv, testClone)]; !ok {
		t.Fatalf("the sweep removed a live clone's wrapper")
	}
}

// TestCloneMetaWrapperMismatchRebuilds is the tmpfs-remounted-under-a-live-
// agent case: the wrapper is still there but no longer backed by the currently
// probed loop device. CN28 reports ERROR and the converge repairs it through
// the §11.5 rebuild — removing the dm-clone first, so the wrapper's own
// removal cannot fail EBUSY.
func TestCloneMetaWrapperMismatchRebuilds(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	loop := loopDev(t, srv, node)
	metaDm := cloneMetaName(srv, testClone)

	stale := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
	node.dms[metaDm].table = fmt.Sprintf("0 %d linear %s 0",
		2*cnCloneMetaUnitSectors, stale)

	info, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{
			ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr(),
		})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	meta := info.GetCntlrInfo().GetCloneIdToMeta()[testClone]
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(meta.GetDetails(), "backed by "+stale) {
		t.Fatalf("clone_id_to_meta is %v/%q",
			meta.GetStatus(), meta.GetDetails())
	}

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	assertOrder(t, node,
		"cmd dmsetup remove "+cloneName(srv, testClone),
		"cmd dmsetup remove "+metaDm,
		"cmd blkdiscard --offset 0 --length 8388608 "+loop,
		"cmd dmsetup create "+metaDm,
		"cmd dmsetup create "+cloneName(srv, testClone),
	)
	if got, want := node.dms[metaDm].table,
		wrapperTable(node, loop, 0); got != want {
		t.Fatalf("rebuilt wrapper table is %q, want %q", got, want)
	}
	assertOk(t, reply.GetCntlrInfo().GetCloneIdToMeta()[testClone],
		"clone meta")
}

// TestCloneMetaRegistrySurvivesRestart: the registry *is* the kernel's dm
// table set, so a restarted agent re-derives the used map from it and never
// double-allocates — the reason the CN allocator needs none of diskmeta.go's
// header/CRC/A-B machinery.
func TestCloneMetaRegistrySurvivesRestart(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	loop := loopDev(t, srv, node)
	metaDm := cloneMetaName(srv, testClone)
	table := node.dms[metaDm].table

	node.Reset()
	fresh := newCnServer(node)
	reconcileForTest(t, fresh)
	assertNoCall(t, node, "cmd dmsetup create "+metaDm)
	assertNoCall(t, node, "cmd blkdiscard --offset 0 --length 8388608 "+loop)
	if node.dms[metaDm].table != table {
		t.Fatalf("the wrapper was rebuilt: %q, want %q",
			node.dms[metaDm].table, table)
	}
}

// ---------------------------------------------------------------------------
// §6.10 — PushCloneBitmap (CN22)
// ---------------------------------------------------------------------------

func TestPushCloneBitmapGates(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})

	unknown := func(req *pb.PushCloneBitmapRequest, label string) {
		t.Helper()
		reply, err := srv.PushCloneBitmap(ctx, req)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if reply.GetAgentReply().GetCode() !=
			common.ReplyCodeUnknownObject {
			t.Fatalf("%s: code %d, want %d", label,
				reply.GetAgentReply().GetCode(),
				common.ReplyCodeUnknownObject)
		}
	}
	unknown(&pb.PushCloneBitmapRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr(),
		Revision: 2, CloneId: 0x999, BmIdx: 0, Bitmap: []byte{0xff},
	}, "unknown clone")
	unknown(&pb.PushCloneBitmapRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr(),
		Revision: 2, CloneId: testClone, BmIdx: 1, Bitmap: []byte{0xff},
	}, "bm_idx >= src_slice_cnt")
	unknown(&pb.PushCloneBitmapRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr(),
		Revision: 2, CloneId: testClone,
		BmIdx: common.MaxCloneBmCnt, Bitmap: []byte{0xff},
	}, "bm_idx >= MaxCloneBmCnt")

	stale, err := srv.PushCloneBitmap(ctx, &pb.PushCloneBitmapRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr(),
		Revision: 1, CloneId: testClone, BmIdx: 0, Bitmap: []byte{0xff},
	})
	if err != nil {
		t.Fatalf("stale push: %v", err)
	}
	if stale.GetAgentReply().GetCode() != common.ReplyCodeStaleRevision {
		t.Fatalf("stale push: code %d", stale.GetAgentReply().GetCode())
	}
}

func TestPushCloneBitmapPersistBeforeApply(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})

	node.Reset()
	pushBitmap(t, srv, 2, hexBytes(t, testSkipHex))
	bmPath := srv.nf.LocalCloneBmPath(
		testCluster, testCn, testSp, testClone, 0)
	assertOrder(t, node,
		"writeproto "+bmPath,
		"cmd blkdiscard --offset 33554432 --length 33554432",
	)

	// A grown chunk overwrites the file and re-applies ([D8]).
	node.Reset()
	grown := append(hexBytes(t, testSkipHex), 0xff)
	pushBitmap(t, srv, 2, grown)
	assertOrder(t, node, "writeproto "+bmPath, "cmd blkdiscard --offset")

	// An identical re-push is neither rewritten nor re-applied.
	node.Reset()
	pushBitmap(t, srv, 2, grown)
	assertNoCall(t, node, "writeproto "+bmPath)
	assertNoCall(t, node, "cmd blkdiscard")
}

// TestPushCloneBitmapSurvivesRestart is SH21: the applied set is derived from
// the files on disk, so a fresh server over the same store reports it back
// unchanged and never needs a re-push.
func TestPushCloneBitmapSurvivesRestart(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	pushBitmap(t, srv, 2, hexBytes(t, testSkipHex))

	fresh := newCnServer(node)
	reconcileForTest(t, fresh)
	reply, err := fresh.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if len(reply.GetBmInfoList()) != 1 ||
		len(reply.GetBmInfoList()[0].GetBmIdxList()) != 1 {
		t.Fatalf("the applied set did not survive the restart: %v",
			reply.GetBmInfoList())
	}
}

// TestPushCloneBitmapWithoutDmClone: a chunk that arrives while the dm-clone
// is level-suppressed still counts as applied, and is re-applied when the
// dm-clone is built (CN22, CN18 step 4).
func TestPushCloneBitmapWithoutDmClone(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()},
		level: pb.SpLevel_SP_LEVEL_NO_CLONE})

	node.Reset()
	pushBitmap(t, srv, 2, hexBytes(t, testSkipHex))
	assertNoCall(t, node, "cmd blkdiscard")

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if len(reply.GetBmInfoList()[0].GetBmIdxList()) != 1 {
		t.Fatalf("the chunk was not reported applied")
	}
	if !node.hasCall("cmd blkdiscard --offset 33554432 --length 33554432") {
		t.Fatalf("the chunk was not re-applied on the rebuild")
	}
}

// TestWrapperEnumerationSurvivesAVanishedWrapper: the kind-`b` name list comes
// from a `dmsetup ls` snapshot that is stale the instant it is printed —
// SyncupCntlr holds only the node *read* lock, so another cntlr's retire or
// SP_LEVEL_DISABLE teardown can remove a wrapper between the `ls` and the
// `dmsetup table` of that name. Failing the whole CN-wide enumeration on it
// flipped a healthy, serving clone of an unrelated cntlr to RES_STATUS_ERROR,
// and ERROR — unlike PROVISIONING — feeds err_epoch and the §10.2/§10.4
// reactions ([D14], update_01.md U3 spec 3).
func TestWrapperEnumerationSurvivesAVanishedWrapper(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})

	// Another cntlr's wrapper, listed by `ls` and gone by the time its table
	// is read.
	node.lsGhosts = append(node.lsGhosts,
		srv.nf.CnCloneMetaDmName(testCluster, testCn, testSp+1, 0x999))

	probe, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	assertOk(t, probe.GetCntlrInfo().GetCloneIdToMeta()[testClone],
		"clone meta")
	assertOk(t, probe.GetCntlrInfo().GetCloneIdToDmClone()[testClone],
		"clone dm")

	// The converge is not disturbed either — and it must not rebuild the live
	// wrapper, whose slot carries a serving dm-clone's superblock.
	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	assertOk(t, reply.GetCntlrInfo().GetCloneIdToMeta()[testClone],
		"converged clone meta")
	assertNoCall(t, node, "cmd blkdiscard")
	assertNoCall(t, node, "cmd dmsetup remove "+cloneMetaName(srv, testClone))
}

// TestWrapperEnumerationFailsOnALiveWrapper is the other half: a `dmsetup
// table` that fails for a wrapper that is still *there* is a real failure and
// must stay fatal. Skipping it would report its units as free, and the
// allocator's discard-first hole punch would then wipe a live dm-clone's
// superblock — the exact corruption the discard-first order exists to prevent.
func TestWrapperEnumerationFailsOnALiveWrapper(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})

	metaDm := cloneMetaName(srv, testClone)
	node.failCmdAlways["dmsetup table "+metaDm] = "fake: transient failure"
	probe, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	meta := probe.GetCntlrInfo().GetCloneIdToMeta()[testClone]
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(meta.GetDetails(),
			"clone-metadata arena unavailable") {
		t.Fatalf("clone_id_to_meta is %v/%q, want the arena error",
			meta.GetStatus(), meta.GetDetails())
	}
}

// TestCloneWrapperRemovalTakesTheArenaLock: removing a kind-`b` wrapper is a
// mutation of the allocator's registry — the dm table set itself — so it
// belongs inside cloneMetaMu with the enumerate → discard → create section it
// races (ruling R3.6). Without it a retire on one cntlr can delete a wrapper
// in the middle of another cntlr's allocation.
func TestCloneWrapperRemovalTakesTheArenaLock(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	metaDm := cloneMetaName(srv, testClone)

	// Stand in for the concurrent allocator: hold the leaf lock and let the
	// retire run.
	srv.cloneMetaMu.Lock()
	retired := make(chan struct{})
	var retireErr error
	go func() {
		defer close(retired)
		_, retireErr = srv.SyncupCntlr(context.Background(),
			cntlrReq(reqOpts{revision: 3, primary: true}))
	}()
	select {
	case <-retired:
		srv.cloneMetaMu.Unlock()
		t.Fatalf("the wrapper removal ran outside cloneMetaMu:\n%s",
			strings.Join(node.Calls(), "\n"))
	case <-time.After(100 * time.Millisecond):
	}
	srv.cloneMetaMu.Unlock()

	select {
	case <-retired:
	case <-time.After(10 * time.Second):
		t.Fatalf("the retire never completed after the lock was released")
	}
	if retireErr != nil {
		t.Fatalf("retire: %v", retireErr)
	}
	if _, ok := node.dms[metaDm]; ok {
		t.Fatalf("the retired clone kept its metadata wrapper")
	}
}

// TestWrapperLengthIsComparedRaw: CN28 checks "table length matches the
// computed size" (update_01.md U3 spec 3). Comparing a unit count rounded down
// from that length instead made the check fail *open* over a whole unit's
// worth of lengths — the one malformed-table case that reported OK.
func TestWrapperLengthIsComparedRaw(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	loop := loopDev(t, srv, node)
	metaDm := cloneMetaName(srv, testClone)

	// One sector past the budgeted two units — a hand-written reload, or a
	// partially applied table.
	node.dms[metaDm].table = fmt.Sprintf("0 %d linear %s 0",
		2*cnCloneMetaUnitSectors+1, node.devNo[loop])

	probe, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	meta := probe.GetCntlrInfo().GetCloneIdToMeta()[testClone]
	want := fmt.Sprintf("table is %d sectors, want %d",
		2*cnCloneMetaUnitSectors+1, 2*cnCloneMetaUnitSectors)
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		meta.GetDetails() != want {
		t.Fatalf("clone_id_to_meta is %v/%q, want ERROR/%q",
			meta.GetStatus(), meta.GetDetails(), want)
	}
}

// TestMisSizedWrapperClaimsItsPartialUnit: the used-unit map is a *footprint*,
// so a wrapper whose table runs one sector into the next unit still claims that
// unit. Rounding down instead handed the tail unit out again, and Alloc's
// discard-first hole punch — which is correct and mandatory for a recycled
// unit — would then zero a range the live wrapper still maps.
func TestMisSizedWrapperClaimsItsPartialUnit(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	loop := loopDev(t, srv, node)

	// Another SP's wrapper on this CN, one sector longer than a whole unit.
	// It is planted behind the agent's back, so this cntlr's converge cannot
	// repair it and has to allocate around it.
	filler := srv.nf.CnCloneMetaDmName(testCluster, testCn, testSp+1, 0x999)
	node.dms[filler] = &fakeDm{
		table: fmt.Sprintf("0 %d linear %s 0",
			cnCloneMetaUnitSectors+1, node.devNo[loop]),
		thinIds: map[uint32]bool{},
	}
	node.devNo["/dev/mapper/"+filler] = "253:200"

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true,
		clones: []*pb.Clone{cloneOf()}})); err != nil {
		t.Fatalf("allocate beside the mis-sized wrapper: %v", err)
	}
	// Units 0 and 1 are both claimed by the filler, so the clone starts at 2.
	if got, want := node.dms[cloneMetaName(srv, testClone)].table,
		wrapperTable(node, loop, 2); got != want {
		t.Fatalf("wrapper table is %q, want %q", got, want)
	}
	punch := fmt.Sprintf("cmd blkdiscard --offset %d ", common.CnCloneMetaUnit)
	if node.hasCall(punch) {
		t.Fatalf("the hole punch hit a unit the live wrapper maps:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
}
