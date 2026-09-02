package dnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

const testMetaBlocks = uint64(3)

func migrDstReq(revision uint64, level pb.SpLevel) *pb.SyncupSideRequest {
	req := sideReq(revision, testSide, testCn0, []uint64{testCn1}, level)
	req.MigrDstConf = &pb.SyncupSideRequest_MigrDstConf{
		MigrId:        testMigrId,
		SrcSideId:     testSide2,
		SrcDnId:       testSrcDn,
		SrcNvmeTrConf: testTrConf(),
		BlockSize:     testBlockSize,
		MetaBlocks:    testMetaBlocks,
		DmCloneConf:   &pb.DmCloneConf{HydrationThreshold: 1, HydrationBatchSize: 1},
		BmCnt:         1,
	}
	return req
}

func migrSrcReq(revision uint64) *pb.SyncupSideRequest {
	req := sideReq(revision, testSide, testCn0, []uint64{testCn1},
		pb.SpLevel_SP_LEVEL_READWRITE)
	req.MigrSrcConf = &pb.SyncupSideRequest_MigrSrcConf{
		MigrId:    testMigrId,
		DstSideId: testSide2,
		DstDnId:   testSrcDn,
	}
	return req
}

// ---------------------------------------------------------------------------
// Migration destination (DN13)
// ---------------------------------------------------------------------------

func TestMigrationDestinationSequence(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	// A migration destination is a freshly allocated side: its very first
	// SyncupSide already carries migr_dst_conf.
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.Reset()
	reply, err := srv.SyncupSide(ctx,
		migrDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	metaName := nf.DnMigrMetaDmName(testCluster, testDn, testSp, testMigrId)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	srcNqn := nf.MigrSrcNqn(testCluster, testSrcDn, testSp, testMigrId)

	// Bottom-up: the clone-metadata slot (zeroed *before* its record is
	// persisted, [P6]) and its wrapper, connect, dm-clone, the per-CN stack
	// on top of it, then the exports with the primary's namespace optimized.
	assertOrder(t, node,
		fmt.Sprintf("writeblock %s off=%d len=8192",
			testDisk, common.DnCloneMetaOffset),
		fmt.Sprintf("writeblock %s off=%d",
			testDisk, common.DnTableSlotBOffset),
		"cmd dmsetup create "+metaName,
		"cmd nvme connect --transport tcp",
		"cmd dmsetup create "+cloneName,
		"cmd dmsetup create "+linName,
		fmt.Sprintf("writedirect %s/subsystems/%s/namespaces/1/ana_grpid=%d",
			agent.NvmetRoot,
			nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0),
			common.AnaGrpIdOptimized),
	)
	// The dm-clone's metadata device is the wrapper and its destination is
	// the side device.
	metaNo := node.devNo[nf.DmPath(metaName)]
	destNo := node.devNo[nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide))]
	cloneFields := strings.Fields(node.dms[cloneName].table)
	if len(cloneFields) < 6 || cloneFields[3] != metaNo ||
		cloneFields[4] != destNo {
		t.Errorf("dm-clone table %q does not use meta %s / dest %s",
			node.dms[cloneName].table, metaNo, destNo)
	}
	if !node.hasCall("--hostnqn " + nf.DnHostNqn(testCluster, testDn)) {
		t.Error("nvme connect did not use the DN host nqn")
	}
	if !node.hasCall("--nqn " + srcNqn) {
		t.Error("nvme connect did not target the migration source nqn")
	}
	if !node.hasCall("--fast-io-fail-tmo 5 --ctrl-loss-tmo -1") {
		t.Error("nvme connect missed the SH20 timeouts")
	}
	// The primary's dm-linear now points at the dm-clone.
	clonePath := nf.DmPath(cloneName)
	if want := node.devNo[clonePath]; !strings.Contains(
		node.dms[linName].table, want) {
		t.Errorf("dm-linear table %q does not point at the dm-clone (%s)",
			node.dms[linName].table, want)
	}
	// Created with hydration off, then enabled (§11.2 dst step 4/5).
	if node.dms[cloneName].noHydration {
		t.Error("hydration was never enabled")
	}
	if info := reply.GetSideInfo().GetMigrDstInfo(); info.GetDmCloneInfo().
		GetStatus() != pb.ResStatus_RES_STATUS_OK {
		t.Errorf("dm_clone_info = %v/%q", info.GetDmCloneInfo().GetStatus(),
			info.GetDmCloneInfo().GetDetails())
	}
	if details := reply.GetSideInfo().GetMigrDstInfo().GetDmCloneInfo().
		GetDetails(); !strings.Contains(details, "clone") {
		t.Errorf("dm_clone details %q does not carry the status line", details)
	}
}

// [P1]: hydration is infrastructure IO, not user IO, so SP_LEVEL_READONLY
// never pauses it. A destination converged straight at READONLY still ends up
// hydrating.
func TestMigrationDestinationHydrationEnabledAtReadOnly(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)

	node.Reset()
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READONLY)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if node.dms[cloneName].noHydration {
		t.Error("SP_LEVEL_READONLY left hydration disabled")
	}
	if !node.hasCall("cmd dmsetup message " + cloneName + " 0 enable_hydration") {
		t.Error("enable_hydration was never sent at SP_LEVEL_READONLY")
	}
	if node.hasCall("disable_hydration") {
		t.Error("disable_hydration must never be sent")
	}

	// Raising the level further keeps hydration on, too.
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_NO_REDUND)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if node.dms[cloneName].noHydration {
		t.Error("a CN-only level disabled hydration on the dn")
	}
}

func TestMigrationDestinationConnectFailureRetries(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)

	node.failCmd["nvme connect"] = "connect refused"
	reply, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	// The RPC still returns; the connect is retried in the background.
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	target := reply.GetSideInfo().GetMigrDstInfo().GetTargetInfo()
	if target.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Errorf("target_info = %v, want ERROR", target.GetStatus())
	}
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	if !st.retrying {
		t.Fatal("no background retry was registered")
	}
	// While the dm-clone is absent the destination keeps its exports on
	// dm-error with every namespace inaccessible.
	for cnId := range reply.GetSideInfo().GetCnIdToNvmeof() {
		nqn := srv.nf.SideToCnNqn(testCluster, testSp, testLeg, cnId)
		state, err := srv.nvmet.ProbeNamespace(ctx, nqn, sideNsid)
		if err != nil {
			t.Fatalf("ProbeNamespace: %v", err)
		}
		if state.AnaGrpId != common.AnaGrpIdInaccessible {
			t.Errorf("cn %d ana_grpid = %d, want inaccessible",
				cnId, state.AnaGrpId)
		}
	}

	// The next converge succeeds: it reloads the primary's dm-linear onto
	// the dm-clone and moves the namespaces off inaccessible (§11.2 dst
	// step 5) — the transition the retry loop exists to reach.
	node.Reset()
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if st.retrying {
		t.Error("retry registration survived a successful connect")
	}
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	assertOrder(t, node,
		"cmd dmsetup create "+
			nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId),
		"cmd dmsetup reload "+linName,
		fmt.Sprintf("writedirect %s/subsystems/%s/namespaces/1/ana_grpid=%d",
			agent.NvmetRoot,
			nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0),
			common.AnaGrpIdOptimized),
	)
	state, err := srv.nvmet.ProbeNamespace(ctx,
		nf.SideToCnNqn(testCluster, testSp, testLeg, testCn1), sideNsid)
	if err != nil {
		t.Fatalf("ProbeNamespace: %v", err)
	}
	if state.AnaGrpId != common.AnaGrpIdNonOptimized {
		t.Errorf("standby ana_grpid = %d, want non-optimized",
			state.AnaGrpId)
	}
}

// ---------------------------------------------------------------------------
// Migration source (DN12)
// ---------------------------------------------------------------------------

func TestMigrationSourceSequence(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)

	node.Reset()
	reply, err := srv.SyncupSide(ctx, migrSrcReq(2))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	srcName := nf.DnMigrSrcName(testCluster, testDn, testSp, testMigrId)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	errName := nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0)
	// (1) every namespace inaccessible, (2) reload every per-CN dm-linear
	// onto its dm-error ([D12]; newTestServer runs with the §11.2 grace
	// window off, so phase 2 happens at once — TestMigrationSourceFence
	// covers the window), (3) build + export.
	assertOrder(t, node,
		fmt.Sprintf("writedirect %s/subsystems/%s/namespaces/1/ana_grpid=%d",
			agent.NvmetRoot,
			nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0),
			common.AnaGrpIdInaccessible),
		"cmd dmsetup reload "+linName,
		"cmd dmsetup create "+srcName,
		"cmd mkdir -p "+agent.NvmetRoot+"/subsystems/"+
			nf.MigrSrcNqn(testCluster, testDn, testSp, testMigrId),
	)
	if node.dms[linName].suspended {
		t.Error("the per-CN dm-linear was left suspended ([D12] forbids it)")
	}
	if errNo := node.devNo[nf.DmPath(errName)]; !strings.Contains(
		node.dms[linName].table, errNo) {
		t.Errorf("dm-linear table %q is not fenced onto the dm-error (%s)",
			node.dms[linName].table, errNo)
	}
	hostNqn := nf.DnHostNqn(testCluster, testSrcDn)
	link := fmt.Sprintf("%s/subsystems/%s/allowed_hosts/%s", agent.NvmetRoot,
		nf.MigrSrcNqn(testCluster, testDn, testSp, testMigrId), hostNqn)
	if _, ok := node.links[link]; !ok {
		t.Errorf("the destination DN host is not in allowed_hosts")
	}
	if info := reply.GetSideInfo().GetMigrSrcInfo(); info.GetNvmeofInfo().
		GetStatus() != pb.ResStatus_RES_STATUS_OK {
		t.Errorf("migr_src nvmeof = %v/%q", info.GetNvmeofInfo().GetStatus(),
			info.GetNvmeofInfo().GetDetails())
	}

	// Cancelling the migration returns the side to normal service.
	if _, err := srv.SyncupSide(ctx, sideReq(3, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if node.dms[linName].suspended {
		t.Error("the per-CN dm-linear stayed suspended after the migration")
	}
	if sideNo := node.devNo[nf.DmPath(nf.DnSideName(
		testCluster, testDn, testSp, testSide))]; !strings.Contains(
		node.dms[linName].table, sideNo) {
		t.Errorf("dm-linear table %q did not go back onto the side device "+
			"(%s)", node.dms[linName].table, sideNo)
	}
	if _, ok := node.dms[srcName]; ok {
		t.Error("the migration-source dm-linear survived")
	}
	state, err := srv.nvmet.ProbeNamespace(ctx,
		nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0), sideNsid)
	if err != nil {
		t.Fatalf("ProbeNamespace: %v", err)
	}
	if state.AnaGrpId != common.AnaGrpIdOptimized {
		t.Errorf("primary ana_grpid = %d, want optimized", state.AnaGrpId)
	}
}

// ---------------------------------------------------------------------------
// 7. PushMigrBitmap (DN15, SH21-SH23)
// ---------------------------------------------------------------------------

func pushReq(bmIdx uint32, migrId uint64, bitmap []byte) *pb.PushMigrBitmapRequest {
	return &pb.PushMigrBitmapRequest{
		ClusterId:   testCluster,
		DnId:        testDn,
		SidePointer: sidePtr(testSide),
		Revision:    2,
		MigrId:      migrId,
		BmIdx:       bmIdx,
		Bitmap:      bitmap,
	}
}

func TestPushMigrBitmap(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	chunkPath := nf.LocalMigrBmPath(
		testCluster, testDn, testSp, testMigrId, 0)

	// Unknown migration id is rejected.
	reply, err := srv.PushMigrBitmap(ctx, pushReq(0, testMigrId+1, []byte{0x05}))
	if err != nil {
		t.Fatalf("PushMigrBitmap: %v", err)
	}
	if reply.GetAgentReply().GetCode() != common.ReplyCodeUnknownObject {
		t.Fatalf("code = %d, want ReplyCodeUnknownObject",
			reply.GetAgentReply().GetCode())
	}

	node.Reset()
	// bits 0 and 2 of the data region are skippable; the leg's 3 meta
	// blocks shift them onto dm-clone regions 3 and 5.
	reply, err = srv.PushMigrBitmap(ctx, pushReq(0, testMigrId, []byte{0x05}))
	if err != nil {
		t.Fatalf("PushMigrBitmap: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	// Persist before apply (§9.6 step 2).
	assertOrder(t, node,
		"writeproto "+chunkPath,
		"cmd blkdiscard --offset 3145728 --length 1048576 "+
			nf.DmPath(cloneName),
	)
	if got := node.dms[cloneName].discards; len(got) != 2 ||
		got[0] != "--offset 3145728 --length 1048576" ||
		got[1] != "--offset 5242880 --length 1048576" {
		t.Fatalf("discards = %v", got)
	}

	// The applied set rides in the next SyncupSide reply.
	sideReply, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	bmInfo := sideReply.GetBmInfo()
	if bmInfo.GetResId() != testMigrId ||
		len(bmInfo.GetBmIdxList()) != 1 || bmInfo.GetBmIdxList()[0] != 0 {
		t.Fatalf("bm_info = %v", bmInfo)
	}

	// A restart derives the applied set from the files on disk (SH21).
	restarted := NewDnAgentServer(node.osClient(),
		common.NewNameFmt(common.DefaultLocalStorPrefix),
		common.DefaultLocalStorPrefix, testDisk, testTrConf())
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	st := restarted.getSide(sideKey(testCluster, testDn, testSp, testSide))
	if st == nil {
		t.Fatal("the side did not survive the restart")
	}
	if got := restarted.bitmapInfo(st).GetBmIdxList(); len(got) != 1 ||
		got[0] != 0 {
		t.Fatalf("applied set after restart = %v", got)
	}
}

func TestPushMigrBitmapWithoutDmClone(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	// SP_LEVEL_NO_MIGRATION suppresses the dm-clone entirely.
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_NO_MIGRATION)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.Reset()
	reply, err := srv.PushMigrBitmap(ctx, pushReq(0, testMigrId, []byte{0x05}))
	if err != nil {
		t.Fatalf("PushMigrBitmap: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	if node.hasCall("cmd blkdiscard --offset") {
		t.Error("discarded without a dm-clone")
	}
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	if got := srv.bitmapInfo(st).GetBmIdxList(); len(got) != 1 {
		t.Fatalf("applied set = %v, want the chunk counted", got)
	}

	// Lowering the level rebuilds the dm-clone and re-applies the chunk.
	node.Reset()
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !node.hasCall("cmd blkdiscard --offset 3145728") {
		t.Error("the chunk was not re-applied when the dm-clone appeared")
	}
}

func TestPushMigrBitmapNonContiguousPrefix(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.Reset()
	// Chunk 1 alone is unplaceable: it is only interpretable behind chunk 0.
	if _, err := srv.PushMigrBitmap(
		ctx, pushReq(1, testMigrId, []byte{0xff})); err != nil {
		t.Fatalf("PushMigrBitmap: %v", err)
	}
	if node.hasCall("cmd blkdiscard --offset") {
		t.Error("applied a chunk with no contiguous prefix")
	}
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	if got := srv.bitmapInfo(st).GetBmIdxList(); len(got) != 1 ||
		got[0] != 1 {
		t.Fatalf("applied set = %v, want [1] (every file present)", got)
	}
}

func TestTeardownRemovesBitmapChunkFiles(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if _, err := srv.PushMigrBitmap(
		ctx, pushReq(0, testMigrId, []byte{0x05})); err != nil {
		t.Fatalf("PushMigrBitmap: %v", err)
	}
	chunkPath := nf.LocalMigrBmPath(
		testCluster, testDn, testSp, testMigrId, 0)
	if _, ok := node.protos[chunkPath]; !ok {
		t.Fatal("the chunk was not persisted")
	}
	if _, err := srv.SyncupDn(ctx, dnReq(3)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if _, ok := node.protos[chunkPath]; ok {
		t.Error("the chunk file survived the side teardown")
	}
	if _, ok := node.conns[nf.MigrSrcNqn(
		testCluster, testSrcDn, testSp, testMigrId)]; ok {
		t.Error("the migration connection survived the side teardown")
	}
}

// DN6: the dm-clone is removed before the disconnect that takes its source
// device away — otherwise in-flight hydration IO has nowhere to go and the
// dmsetup remove blocks until the hard timeout.
func TestTeardownRemovesDmCloneBeforeDisconnect(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	srcNqn := nf.MigrSrcNqn(testCluster, testSrcDn, testSp, testMigrId)

	// Path 1: the whole side leaves its DN's pointer list (DN6).
	t.Run("side teardown", func(t *testing.T) {
		srv, node := newTestServer(t)
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		if _, err := srv.SyncupSide(ctx,
			migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		node.Reset()
		if _, err := srv.SyncupDn(ctx, dnReq(3)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		// DN6 order: dm-clone, disconnect, the metadata wrapper, its slot's
		// release, then the side device and its record.
		metaName := nf.DnMigrMetaDmName(
			testCluster, testDn, testSp, testMigrId)
		assertOrder(t, node,
			"cmd dmsetup remove "+cloneName,
			"cmd nvme disconnect --nqn "+srcNqn,
			"cmd dmsetup remove "+metaName,
			"writeblock "+testDisk,
			"cmd dmsetup remove "+nf.DnSideName(
				testCluster, testDn, testSp, testSide),
		)
		if _, ok, _ := srv.meta.LookupCloneMeta(ctx, testSp, testMigrId); ok {
			t.Error("the clone-metadata record survived teardown")
		}
		if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); ok {
			t.Error("the side record survived teardown")
		}
	})

	// Path 2: the destination role ends while the side stays (DN11/DN13).
	t.Run("destination role ends", func(t *testing.T) {
		srv, node := newTestServer(t)
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		if _, err := srv.SyncupSide(ctx,
			migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		node.Reset()
		// FinishMigration drops migr_dst_conf; the side returns to normal.
		if _, err := srv.SyncupSide(ctx, sideReq(3, testSide, testCn0,
			[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		assertOrder(t, node,
			"cmd dmsetup remove "+cloneName,
			"cmd nvme disconnect --nqn "+srcNqn,
		)
		if _, ok := node.dms[cloneName]; ok {
			t.Error("the dm-clone survived the end of the migration")
		}
		if _, ok := node.conns[srcNqn]; ok {
			t.Error("the migration connection survived")
		}
		// The primary's dm-linear is reloaded straight onto the side device
		// (§8.11), and the metadata wrapper and its slot are gone.
		linName := nf.DnLinearName(
			testCluster, testDn, testSp, testSide, testCn0)
		sideNo := node.devNo[nf.DmPath(nf.DnSideName(
			testCluster, testDn, testSp, testSide))]
		if !strings.Contains(node.dms[linName].table, sideNo) {
			t.Errorf("dm-linear table %q does not point back at the "+
				"side device (%s)", node.dms[linName].table, sideNo)
		}
		metaName := nf.DnMigrMetaDmName(
			testCluster, testDn, testSp, testMigrId)
		if _, ok := node.dms[metaName]; ok {
			t.Error("the clone-metadata wrapper survived")
		}
		if _, ok, _ := srv.meta.LookupCloneMeta(ctx, testSp, testMigrId); ok {
			t.Error("the clone-metadata record survived")
		}
	})
}

// A hydration knob that will not apply is reported, but it must not cost the
// side its serving path: the dm-clone is live and reads through to the source,
// so demoting the primary's dm-linear back onto dm-error over it would turn a
// healthy side into an erroring one.
func TestHydrationKnobFailureKeepsTheClonePath(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)

	// Turn hydration off behind the agent's back and make the message that
	// would turn it back on fail.
	node.mu.Lock()
	node.dms[cloneName].noHydration = true
	node.failCmdAlways["dmsetup message "+cloneName] = "device busy"
	node.mu.Unlock()

	reply, err := srv.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	info := reply.GetSideInfo().GetMigrDstInfo().GetDmCloneInfo()
	if info.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Errorf("dm_clone_info = %v, want ERROR", info.GetStatus())
	}
	// The path stays on the dm-clone and the namespace stays optimized.
	cloneNo := node.devNo[nf.DmPath(cloneName)]
	if !strings.Contains(node.dms[linName].table, cloneNo) {
		t.Errorf("the primary's dm-linear was demoted off the dm-clone: %q",
			node.dms[linName].table)
	}
	state, err := srv.nvmet.ProbeNamespace(ctx,
		nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0), sideNsid)
	if err != nil {
		t.Fatalf("ProbeNamespace: %v", err)
	}
	if state.AnaGrpId != common.AnaGrpIdOptimized {
		t.Errorf("primary ana_grpid = %d, want optimized", state.AnaGrpId)
	}
}

// ---------------------------------------------------------------------------
// The §11.2 src-cutover grace window ([D12], common.SuspendSeconds)
// ---------------------------------------------------------------------------

// The production default is the contract: a migration source's per-CN
// dm-linears are held suspended for SuspendSeconds before they are retired.
func TestFenceWindowDefault(t *testing.T) {
	node := newFakeNode()
	srv := NewDnAgentServer(node.osClient(),
		common.NewNameFmt(common.DefaultLocalStorPrefix),
		common.DefaultLocalStorPrefix, testDisk, testTrConf())
	if want := common.SuspendSeconds * time.Second; srv.fenceWait != want {
		t.Errorf("fenceWait = %v, want %v", srv.fenceWait, want)
	}
	if common.SuspendSeconds != 60 {
		t.Errorf("SuspendSeconds = %d, want 60", common.SuspendSeconds)
	}
}

// Phase 1 holds the linears suspended **on their pre-fence tables**; phase 2
// reloads them onto their dm-errors and resumes. Swapping the table in phase 1
// would defeat the window by erroring the very IO it exists to absorb.
func TestMigrationSourceFence(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)

	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	stbName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1)
	errName := nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0)
	sideNo := node.devNo[nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide))]
	errNo := node.devNo[nf.DmPath(errName)]

	// --- phase 1: a window long enough that it cannot elapse mid-test.
	srv.fenceWait = time.Hour
	node.Reset()
	reply, err := srv.SyncupSide(ctx, migrSrcReq(2))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	// Namespaces inaccessible first, then the suspend — never a reload.
	assertOrder(t, node,
		fmt.Sprintf("writedirect %s/subsystems/%s/namespaces/1/ana_grpid=%d",
			agent.NvmetRoot,
			nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0),
			common.AnaGrpIdInaccessible),
		"cmd dmsetup suspend "+linName,
	)
	if node.hasCall("cmd dmsetup reload " + linName) {
		t.Error("phase 1 swapped the table instead of suspending in place")
	}
	for _, name := range []string{linName, stbName} {
		if !node.dms[name].suspended {
			t.Errorf("%s was not suspended by the cutover", name)
		}
	}
	// The primary's table is untouched: still the side device, so the
	// deferred IO has somewhere to have come from.
	if !strings.Contains(node.dms[linName].table, sideNo) {
		t.Errorf("phase 1 moved the primary's table off the side device: %q",
			node.dms[linName].table)
	}
	// The RPC returned rather than waiting out the window, and reports the
	// window as a healthy state.
	info := reply.GetSideInfo().GetCnIdToDmLinear()[testCn0]
	if info.GetStatus() != pb.ResStatus_RES_STATUS_OK {
		t.Errorf("dm_linear during the window = %v/%q, want OK",
			info.GetStatus(), info.GetDetails())
	}
	if !strings.Contains(info.GetDetails(), "grace window") {
		t.Errorf("dm_linear details = %q, want the grace-window note",
			info.GetDetails())
	}
	// A probe agrees, and does not mutate (DN16/SH25).
	node.Reset()
	if _, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	}); err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	if got := node.Mutations(); len(got) != 0 {
		t.Errorf("a probe inside the window mutated: %v", got)
	}
	// An extra converge inside the window is idempotent.
	node.Reset()
	if _, err := srv.SyncupSide(ctx, migrSrcReq(3)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	for _, call := range node.Mutations() {
		if strings.HasPrefix(call, "writeproto") {
			continue
		}
		t.Errorf("a re-converge inside the window mutated: %s", call)
	}

	// --- phase 2: the window elapses.
	srv.fenceWait = 0
	node.Reset()
	if _, err := srv.SyncupSide(ctx, migrSrcReq(4)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !node.hasCall("cmd dmsetup reload " + linName) {
		t.Fatalf("the window ended without a reload:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	for _, name := range []string{linName, stbName} {
		if node.dms[name].suspended {
			t.Errorf("%s outlived the grace window suspended", name)
		}
	}
	if !strings.Contains(node.dms[linName].table, errNo) {
		t.Errorf("the primary's table is not fenced onto its dm-error: %q",
			node.dms[linName].table)
	}
	if got := srv.getSide(sideKey(testCluster, testDn, testSp, testSide)).
		req; got.GetMigrSrcConf() == nil {
		t.Error("the source role vanished")
	}
}

// The window ends on its own: the RPC does not wait for it, so a timer has to
// run the converge that retires the linears.
func TestFenceTimerRetiresTheLinears(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	errNo := node.devNo[nf.DmPath(
		nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0))]

	srv.fenceWait = 30 * time.Millisecond
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	suspended := node.dms[linName].suspended
	node.mu.Unlock()
	if !suspended {
		t.Fatal("the cutover did not suspend the primary's dm-linear")
	}

	if !waitFor(t, 5*time.Second, func() bool {
		node.mu.Lock()
		defer node.mu.Unlock()
		return !node.dms[linName].suspended
	}) {
		t.Fatal("the grace window never ended on its own")
	}
	node.mu.Lock()
	table := node.dms[linName].table
	node.mu.Unlock()
	if !strings.Contains(table, errNo) {
		t.Errorf("the timer resumed the linear without fencing it: %q", table)
	}
}

// Cancelling the migration inside the window puts the side straight back into
// service — no leftover suspension, no leftover timer.
func TestFenceClearedWhenTheSourceRoleEnds(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	sideNo := node.devNo[nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide))]

	srv.fenceWait = time.Hour
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !node.dms[linName].suspended {
		t.Fatal("the cutover did not suspend the primary's dm-linear")
	}

	if _, err := srv.SyncupSide(ctx, sideReq(3, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if node.dms[linName].suspended {
		t.Error("cancelling the migration left the dm-linear suspended")
	}
	if !strings.Contains(node.dms[linName].table, sideNo) {
		t.Errorf("the dm-linear did not go back onto the side device: %q",
			node.dms[linName].table)
	}
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	if srv.inFence(st) {
		t.Error("the grace window outlived the migration source role")
	}
	if st.fenceTimer != nil {
		t.Error("the grace-window timer was not stopped")
	}
}

// A side torn down inside the window still tears down: `dmsetup remove` does
// not succeed on a suspended device, so the teardown resumes it first.
func TestTeardownInsideTheFenceWindow(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)

	srv.fenceWait = time.Hour
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !node.dms[linName].suspended {
		t.Fatal("the cutover did not suspend the primary's dm-linear")
	}

	node.Reset()
	if _, err := srv.SyncupDn(ctx, dnReq(3)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	// The resume comes before the nvmet teardown, not just before the
	// remove: disabling a namespace closes its backing device.
	assertOrder(t, node,
		"cmd dmsetup resume "+linName,
		"writedirect "+agent.NvmetRoot+"/subsystems/"+
			nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0)+
			"/namespaces/1/enable=0",
		"cmd dmsetup remove "+linName,
	)
	if _, ok := node.dms[linName]; ok {
		t.Error("a suspended dm-linear survived the teardown")
	}
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); ok {
		t.Error("the side's extents were not freed")
	}
}

// An agent restart inside the window must not open a second one: the linears
// it finds suspended are retired on the first converge.
func TestFenceNotRestartedAcrossAnAgentRestart(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	errNo := node.devNo[nf.DmPath(
		nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0))]

	srv.fenceWait = time.Hour
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !node.dms[linName].suspended {
		t.Fatal("the cutover did not suspend the primary's dm-linear")
	}

	// The kernel state survives; the agent process does not.
	restarted := NewDnAgentServer(node.osClient(),
		common.NewNameFmt(common.DefaultLocalStorPrefix),
		common.DefaultLocalStorPrefix, testDisk, testTrConf())
	restarted.fenceWait = time.Hour
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if node.dms[linName].suspended {
		t.Error("the restart started a second grace window")
	}
	if !strings.Contains(node.dms[linName].table, errNo) {
		t.Errorf("the reconcile did not retire the fenced linear: %q",
			node.dms[linName].table)
	}
}
