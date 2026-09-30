package dnagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
		// The steady state: the destination has finished provisioning, so the
		// source really does take on its role (§11.2). The false case is
		// exactly equivalent to having no migr_src_conf at all and is covered
		// by TestMigrationSourceDeferredUntilDestinationProvisions.
		DstProvisioned: true,
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
	// SyncupSide already carries migr_dst_conf. Under [D15] that side provisions
	// first — linear and zeroing only, no metadata slot, no connect, no
	// dm-clone — and only then does the worker flip its flag (§11.2).
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	provisionSide(t, srv, 1, testSide)
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
	// persisted, DN13) and its wrapper, connect, dm-clone, the per-CN stack
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
	// Every dnv dm-clone carries both features (DN13 step 4). The dn
	// hazard is after the §11.2 cutover: a skip-bitmap chunk `blkdiscard`ing
	// a region the host already hydrated must stay metadata-only.
	create := node.callsMatching("cmd dmsetup create " + cloneName)
	if len(create) != 1 ||
		!strings.Contains(create[0], "2 no_hydration no_discard_passdown") {
		t.Fatalf("dm-clone features are wrong: %v", create)
	}
	dnHostNqn := nf.DnHostNqn(testCluster, testDn)
	if !node.hasCall("--hostnqn " + dnHostNqn) {
		t.Error("nvme connect did not use the DN host nqn")
	}
	// The hostid is derived from the hostnqn, never left to the node-wide
	// /etc/nvme/hostid: the kernel keeps a 1:1 hostnqn<->hostid mapping and
	// rejects the second hostnqn under a known hostid with EINVAL, so an
	// implicit id makes this connect fail whenever anything else on the node
	// (another dnv identity, an unrelated NVMe-oF mount) claimed it first.
	if !node.hasCall("--hostid " + common.NvmeHostId(dnHostNqn)) {
		t.Error("nvme connect did not derive its hostid from the host nqn")
	}
	if !node.hasCall("--nqn " + srcNqn) {
		t.Error("nvme connect did not target the migration source nqn")
	}
	if !node.hasCall("--fast_io_fail_tmo 5 --ctrl-loss-tmo -1") {
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

// §11.2 under [D15]: a migration destination provisions before it does anything
// else — the aggregate dm-linear and the zeroing, and nothing above it: no
// clone-metadata slot, no connect, no dm-clone. Its migr_dst_info rows report
// PROVISIONING throughout.
// The §11.2 finish step: the request drops migr_dst_conf and the destination
// becomes a plain side. The dm-clone sits *under* the per-CN dm-linear, so the
// linear must be reloaded onto the plain side device **before** the clone is
// removed. Removing it first fails EBUSY and leaks the clone, its metadata
// wrapper and — because both sit on it — the side device itself, which no
// later empty side list can then remove either.
func TestMigrationDestinationFinishRepointsBeforeRemovingTheClone(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	provisionSide(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	metaName := nf.DnMigrMetaDmName(testCluster, testDn, testSp, testMigrId)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	sideName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	if _, ok := node.dms[cloneName]; !ok {
		t.Fatal("the migration destination did not build a dm-clone")
	}

	node.Reset()
	// The same side, with no migr_dst_conf: the finish.
	reply, err := srv.SyncupSide(ctx, sideReq(2, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	assertOrder(t, node,
		"cmd dmsetup reload "+linName,
		"cmd dmsetup remove "+cloneName,
		"cmd nvme disconnect",
		"cmd dmsetup remove "+metaName,
	)
	// Nothing of the migration is left, and the linear now serves the plain
	// side device.
	for _, name := range []string{cloneName, metaName} {
		if _, ok := node.dms[name]; ok {
			t.Errorf("%s survived the finish", name)
		}
	}
	if want := node.devNo[nf.DmPath(sideName)]; !strings.Contains(
		node.dms[linName].table, want) {
		t.Errorf("dm-linear table %q does not point at the side device (%s)",
			node.dms[linName].table, want)
	}
}

func TestMigrationDestinationProvisionsFirst(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}

	node.Reset()
	req := migrDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)
	req.SideConf.Provisioned = false
	reply, err := srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	dst := reply.GetSideInfo().GetMigrDstInfo()
	for _, row := range []struct {
		name string
		info *pb.ResInfo
	}{
		{"target", dst.GetTargetInfo()},
		{"dm_clone", dst.GetDmCloneInfo()},
	} {
		if row.info.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING ||
			row.info.GetDetails() != tagProvisioningWait {
			t.Errorf("migr_dst %s = %v/%q, want PROVISIONING/%q", row.name,
				row.info.GetStatus(), row.info.GetDetails(),
				tagProvisioningWait)
		}
	}
	// The side device exists; nothing above it does.
	if _, ok := node.dms[nf.DnSideName(
		testCluster, testDn, testSp, testSide)]; !ok {
		t.Error("the destination side device was not built")
	}
	for _, name := range []string{
		nf.DnMigrMetaDmName(testCluster, testDn, testSp, testMigrId),
		nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId),
		nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0),
	} {
		if _, ok := node.dms[name]; ok {
			t.Errorf("a provisioning destination built %s", name)
		}
	}
	if node.hasCall("cmd nvme connect") {
		t.Error("a provisioning destination connected to its source")
	}
	if _, ok, _ := srv.meta.LookupCloneMeta(ctx, testSp, testMigrId); ok {
		t.Error("a provisioning destination reserved a clone-metadata slot")
	}

	// After the flip the ordinary DN13 sequence runs.
	waitZeroed(t, srv, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if _, ok := node.dms[nf.DnMigrFinalName(
		testCluster, testDn, testSp, testMigrId)]; !ok {
		t.Error("the flip did not build the dm-clone")
	}
}

// [D11]: SP_LEVEL_READONLY never pauses hydration — it is infrastructure IO,
// not user IO. A destination converged straight at READONLY still ends up
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

// §11.2: `migr_src_conf.dst_provisioned = false` makes the source behave
// **exactly** as if migr_src_conf were absent — it keeps serving, it does not
// fence, and it exports nothing — differing only in reporting the would-be
// migr_src_info rows as PROVISIONING. Without the gate the source would fence
// the primary's path at migration start and the leg would have no serving path
// for the whole zeroing window.
func TestMigrationSourceDeferredUntilDestinationProvisions(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)

	srcName := nf.DnMigrSrcName(testCluster, testDn, testSp, testMigrId)
	srcNqn := nf.MigrSrcNqn(testCluster, testDn, testSp, testMigrId)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	sideNo := node.devNo[nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide))]

	node.Reset()
	req := migrSrcReq(2)
	req.MigrSrcConf.DstProvisioned = false
	reply, err := srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	// The side is byte-for-byte the side it was: no fence, no ANA handover,
	// no source export.
	for _, call := range node.Mutations() {
		if strings.HasPrefix(call, "writeproto") {
			continue // SH5: the request itself is re-persisted
		}
		t.Errorf("a deferred migration source mutated the dn: %s", call)
	}
	if node.dms[linName].suspended {
		t.Error("a deferred migration source fenced its per-CN dm-linear")
	}
	if !strings.Contains(node.dms[linName].table, sideNo) {
		t.Errorf("dm-linear table %q left the side device (%s)",
			node.dms[linName].table, sideNo)
	}
	if _, ok := node.dms[srcName]; ok {
		t.Error("a deferred migration source built its dm-linear")
	}
	if node.dirs[agent.NvmetRoot+"/subsystems/"+srcNqn] {
		t.Error("a deferred migration source exported itself")
	}
	// Only the reporting differs.
	src := reply.GetSideInfo().GetMigrSrcInfo()
	for _, row := range []struct {
		name string
		info *pb.ResInfo
	}{
		{"dm_linear", src.GetDmLinearInfo()},
		{"nvmeof", src.GetNvmeofInfo()},
	} {
		if row.info.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING ||
			row.info.GetDetails() != tagProvisioningWait {
			t.Errorf("migr_src %s = %v/%q, want PROVISIONING/%q", row.name,
				row.info.GetStatus(), row.info.GetDetails(),
				tagProvisioningWait)
		}
	}
	// The per-CN namespaces are still the primary's, optimized and serving.
	for cnId, info := range reply.GetSideInfo().GetCnIdToNvmeof() {
		if info.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("cn %d nvmeof = %v/%q, want OK",
				cnId, info.GetStatus(), info.GetDetails())
		}
	}
	// The read-only probe agrees.
	infoReply, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	if got := infoReply.GetSideInfo().GetMigrSrcInfo().GetDmLinearInfo(); got.
		GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING {
		t.Errorf("probed migr_src dm_linear = %v/%q, want PROVISIONING",
			got.GetStatus(), got.GetDetails())
	}

	// Once the destination provisions, the real §11.2 sequence runs.
	if _, err := srv.SyncupSide(ctx, migrSrcReq(3)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if _, ok := node.dms[srcName]; !ok {
		t.Error("the flip did not build the migration-source dm-linear")
	}
	if !node.dirs[agent.NvmetRoot+"/subsystems/"+srcNqn] {
		t.Error("the flip did not export the migration source")
	}
}

// A migration cancelled while it was still deferred built nothing — but it did
// register its migr_src_* rows, and those must go with the role. SH14 defines
// epoch as the unix second of the *last status change*, so a leaked entry
// silently hands a later deferral the dead migration's epoch: the worker would
// be told resources created a moment ago have been provisioning for as long as
// the agent has been up.
func TestDeferredMigrationSourceDropsItsRowsWhenCancelled(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)

	deferredReq := func(revision, migrId uint64) *pb.SyncupSideRequest {
		req := migrSrcReq(revision)
		req.MigrSrcConf.MigrId = migrId
		req.MigrSrcConf.DstProvisioned = false
		return req
	}
	first, err := srv.SyncupSide(ctx, deferredReq(2, testMigrId))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	firstSrc := first.GetSideInfo().GetMigrSrcInfo()
	if firstSrc.GetDmLinearInfo().GetStatus() !=
		pb.ResStatus_RES_STATUS_PROVISIONING {
		t.Fatalf("the first deferred migration did not report its rows: %v",
			firstSrc.GetDmLinearInfo())
	}

	// Cancelled while still deferred: the next request carries no
	// migr_src_conf at all.
	if _, err := srv.SyncupSide(ctx, sideReq(3, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}

	// A second migration, also deferred, reports the same status — so only a
	// dropped entry can give it a fresh epoch.
	waitForNextUnixSecond()
	second, err := srv.SyncupSide(ctx, deferredReq(4, testMigrId+1))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	secondSrc := second.GetSideInfo().GetMigrSrcInfo()
	for _, row := range []struct {
		name        string
		was, is     *pb.ResInfo
		wantPresent bool
	}{
		{"dm_linear", firstSrc.GetDmLinearInfo(),
			secondSrc.GetDmLinearInfo(), true},
		{"nvmeof", firstSrc.GetNvmeofInfo(),
			secondSrc.GetNvmeofInfo(), true},
	} {
		if row.is.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING {
			t.Errorf("migr_src %s = %v, want PROVISIONING",
				row.name, row.is.GetStatus())
			continue
		}
		if row.is.GetEpoch() <= row.was.GetEpoch() {
			t.Errorf("migr_src %s epoch = %d, want later than the cancelled "+
				"migration's %d — the tracker entry outlived its role",
				row.name, row.is.GetEpoch(), row.was.GetEpoch())
		}
	}
}

// waitForNextUnixSecond blocks until the unix second has moved on, so a
// re-registered ResInfo can be told apart from one that reused a stale entry's
// epoch (SH14: epoch is the unix second of the last status change).
func waitForNextUnixSecond() {
	start := time.Now().Unix()
	for time.Now().Unix() == start {
		time.Sleep(5 * time.Millisecond)
	}
}

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

// A node that lost its --local-store but kept its disk and its kernel state
// rebuilds every side its DN's pointer list names from what is there (DN8),
// and the SyncupDn that brings the list back comes before any side's own
// request. Its node-level sweep judged the migration objects by the claim
// rule alone, which reads held sides' requests — and none is held yet. A
// source's `d2` linear and its `:3:` export are claimed by nothing but that
// side's request, so the sweep took both out from under the destination's
// dm-clone: every read of a region not yet hydrated failed on the leg the host
// was using. A migration object names its sp, not a side, so while a side of
// that sp is known only by its pointer "no held side claims it" proves
// nothing, and the object waits for a pass that can prove it — the rule the
// clone-metadata record already follows (DN6).
func TestLostStoreKeepsAMigrationSourceUntilItsSideIsKnown(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	srcName := nf.DnMigrSrcName(testCluster, testDn, testSp, testMigrId)
	srcNqn := nf.MigrSrcNqn(testCluster, testDn, testSp, testMigrId)
	if !dmPresent(node, srcName) || !subsysPresent(node, srcNqn) {
		t.Fatal("the migration source was never built")
	}

	// The agent restarts over an empty --local-store; the disk and every
	// kernel object survive.
	stopTestServer(t, srv)
	node.mu.Lock()
	node.protos = map[string][]byte{}
	node.mu.Unlock()
	restarted := startTestServer(t, node)

	// The worker re-syncs the DN first. Its list names the side, whose own
	// request has not arrived.
	node.Reset()
	reply, err := restarted.SyncupDn(ctx, dnReq(1, testSide))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	for _, call := range node.Mutations() {
		if strings.Contains(call, srcName) || strings.Contains(call, srcNqn) {
			t.Errorf("the SyncupDn took the live migration source apart: %s",
				call)
		}
	}
	if !dmPresent(node, srcName) {
		t.Errorf("%s did not survive the SyncupDn", srcName)
	}
	if !subsysPresent(node, srcNqn) {
		t.Errorf("%s did not survive the SyncupDn", srcNqn)
	}
	// Kept is not left over. Naming it would have the worker re-send, every
	// round, a SyncupDn that must not remove it.
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("SyncupDn code = %d (%s), want 0",
			got, reply.GetAgentReply().GetDetails())
	}
	infoReply, err := restarted.GetDnInfo(ctx,
		&pb.GetDnInfoRequest{ClusterId: testCluster, DnId: testDn})
	if err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	if got := infoReply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("dn verdict = %d (%s), want 0",
			got, infoReply.GetAgentReply().GetDetails())
	}

	// The side's request arrives. Everything it wants is already there, so
	// its converge adopts what it finds and rebuilds nothing.
	node.Reset()
	sideReply, err := restarted.SyncupSide(ctx, migrSrcReq(2))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := sideReply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("SyncupSide code = %d (%s), want 0",
			got, sideReply.GetAgentReply().GetDetails())
	}
	for _, call := range node.Mutations() {
		if strings.HasPrefix(call, "writeproto") {
			continue // SH5: the request itself is persisted
		}
		t.Errorf("the side's SyncupSide rebuilt what the sweep kept: %s", call)
	}
	src := sideReply.GetSideInfo().GetMigrSrcInfo()
	for _, row := range []struct {
		name string
		info *pb.ResInfo
	}{
		{"dm_linear", src.GetDmLinearInfo()},
		{"nvmeof", src.GetNvmeofInfo()},
	} {
		if row.info.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("migr_src %s = %v/%q, want OK", row.name,
				row.info.GetStatus(), row.info.GetDetails())
		}
	}
}

// The destination's twin. Its dm-clone, the clone's metadata wrapper and its
// `:3:` connection to the source are claimed by nothing but the destination
// side's request either. The clone survived all the same — the side's per-CN
// linears map it, so its removal failed EBUSY and the layer stop rule held the
// connection — but the attempt named all three as leftovers, and a SyncupDn
// reporting them is re-sent every round for objects it must not remove. The
// connection needs the gate as much as the clone does: with the clone kept out
// of the chain and the connection left in, the dm-clone layer finds nothing of
// its own to remove and disconnects the source under the live clone.
func TestLostStoreKeepsAMigrationDestinationUntilItsSideIsKnown(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	provisionSide(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	metaName := nf.DnMigrMetaDmName(testCluster, testDn, testSp, testMigrId)
	srcNqn := nf.MigrSrcNqn(testCluster, testSrcDn, testSp, testMigrId)
	if !dmPresent(node, cloneName) || !dmPresent(node, metaName) ||
		!connPresent(node, srcNqn) {
		t.Fatal("the migration destination was never built")
	}

	stopTestServer(t, srv)
	node.mu.Lock()
	node.protos = map[string][]byte{}
	node.mu.Unlock()
	restarted := startTestServer(t, node)

	node.Reset()
	reply, err := restarted.SyncupDn(ctx, dnReq(1, testSide))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	for _, call := range node.Mutations() {
		if strings.Contains(call, cloneName) ||
			strings.Contains(call, metaName) ||
			strings.HasPrefix(call, "cmd nvme disconnect") {
			t.Errorf("the SyncupDn took the live migration destination "+
				"apart: %s", call)
		}
	}
	for _, name := range []string{cloneName, metaName} {
		if !dmPresent(node, name) {
			t.Errorf("%s did not survive the SyncupDn", name)
		}
	}
	if !connPresent(node, srcNqn) {
		t.Errorf("the connection to %s did not survive the SyncupDn", srcNqn)
	}
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("SyncupDn code = %d (%s), want 0",
			got, reply.GetAgentReply().GetDetails())
	}
	infoReply, err := restarted.GetDnInfo(ctx,
		&pb.GetDnInfoRequest{ClusterId: testCluster, DnId: testDn})
	if err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	if got := infoReply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("dn verdict = %d (%s), want 0",
			got, infoReply.GetAgentReply().GetDetails())
	}

	node.Reset()
	sideReply, err := restarted.SyncupSide(ctx,
		migrDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := sideReply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("SyncupSide code = %d (%s), want 0",
			got, sideReply.GetAgentReply().GetDetails())
	}
	for _, call := range node.Mutations() {
		if strings.HasPrefix(call, "writeproto") {
			continue // SH5: the request itself is persisted
		}
		t.Errorf("the side's SyncupSide rebuilt what the sweep kept: %s", call)
	}
	dst := sideReply.GetSideInfo().GetMigrDstInfo()
	for _, row := range []struct {
		name string
		info *pb.ResInfo
	}{
		{"target", dst.GetTargetInfo()},
		{"dm_clone", dst.GetDmCloneInfo()},
	} {
		if row.info.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("migr_dst %s = %v/%q, want OK", row.name,
				row.info.GetStatus(), row.info.GetDetails())
		}
	}
}

// The startup twin: the dn file survives, the source side's does not, so
// Reconcile's own removing sweepDn meets the side by its pointer alone.
func TestReconcileKeepsAMigrationSourceWhoseSideFileIsLost(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	srcName := nf.DnMigrSrcName(testCluster, testDn, testSp, testMigrId)
	srcNqn := nf.MigrSrcNqn(testCluster, testDn, testSp, testMigrId)
	if !dmPresent(node, srcName) || !subsysPresent(node, srcNqn) {
		t.Fatal("the migration source was never built")
	}
	stopTestServer(t, srv)
	node.mu.Lock()
	delete(node.protos, nf.LocalSidePath(testCluster, testDn, testSp, testSide))
	node.mu.Unlock()
	node.Reset()
	startTestServer(t, node)
	for _, call := range node.Mutations() {
		if strings.Contains(call, srcName) || strings.Contains(call, srcNqn) {
			t.Errorf("Reconcile took the live migration source apart: %s", call)
		}
	}
	if !dmPresent(node, srcName) || !subsysPresent(node, srcNqn) {
		t.Error("the migration source did not survive Reconcile")
	}
}

// Two sides of one sp may share a DN: growing a slice keeps a DN that already
// carries another group of the slice or of the sp allowed. A migration object
// names (sp, migr) and no side, so the object-level pass of EITHER side
// judges it, not only the pass of the side that plays the migration. After a
// lost store the other side's request can come back first, and its pass met
// the source's `d2` linear and `:3:` export with no stored request claiming
// them: it took both from under the destination's dm-clone and replied code
// 0, so nothing reported it. That pass has to wait for the same proof as the
// node-level one.
func TestLostStoreSiblingSideKeepsAMigrationSource(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	src := sweepSidePtr(testLeg, testSide)
	sibling := sweepSidePtr(testLeg+1, testSide2)
	if _, err := srv.SyncupDn(ctx, sweepDnReq(1, src, sibling)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	syncupSideTwoPhase(t, srv, sweepSideReq(1, src))
	syncupSideTwoPhase(t, srv, sweepSideReq(1, sibling))
	if _, err := srv.SyncupSide(ctx,
		sweepSrcReq(2, src, testMigrId)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	srcName := nf.DnMigrSrcName(testCluster, testDn, testSp, testMigrId)
	srcNqn := nf.MigrSrcNqn(testCluster, testDn, testSp, testMigrId)
	if !dmPresent(node, srcName) || !subsysPresent(node, srcNqn) {
		t.Fatal("the migration source was never built")
	}

	stopTestServer(t, srv)
	node.mu.Lock()
	node.protos = map[string][]byte{}
	node.mu.Unlock()
	restarted := startTestServer(t, node)
	if _, err := restarted.SyncupDn(ctx,
		sweepDnReq(1, src, sibling)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}

	// The sibling's request comes back first.
	node.Reset()
	reply, err := restarted.SyncupSide(ctx, sweepSideReq(1, sibling))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	for _, call := range node.Mutations() {
		if strings.Contains(call, srcName) || strings.Contains(call, srcNqn) {
			t.Errorf("the sibling's SyncupSide took the live migration "+
				"source apart: %s", call)
		}
	}
	if !dmPresent(node, srcName) || !subsysPresent(node, srcNqn) {
		t.Error("the migration source did not survive the sibling's pass")
	}
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("sibling SyncupSide code = %d (%s), want 0",
			got, reply.GetAgentReply().GetDetails())
	}
	infoReply, err := restarted.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sibling})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	if got := infoReply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("sibling verdict = %d (%s), want 0",
			got, infoReply.GetAgentReply().GetDetails())
	}

	// The source's own request then finds everything in place.
	node.Reset()
	srcReply, err := restarted.SyncupSide(ctx, sweepSrcReq(2, src, testMigrId))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := srcReply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("source SyncupSide code = %d (%s), want 0",
			got, srcReply.GetAgentReply().GetDetails())
	}
	for _, call := range node.Mutations() {
		if strings.HasPrefix(call, "writeproto") {
			continue // SH5: the request itself is persisted
		}
		t.Errorf("the source's SyncupSide rebuilt what was kept: %s", call)
	}
}

// The destination's twin of the sibling case. The sibling's pass tried
// `dmsetup remove` on the live dm-clone (EBUSY under the destination's per-CN
// linears) and replied with the clone, its metadata wrapper and its `:3:`
// connection as leftovers, a reply re-sent every round until the
// destination's own request came back.
func TestLostStoreSiblingSideKeepsAMigrationDestination(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	dst := sweepSidePtr(testLeg, testSide)
	sibling := sweepSidePtr(testLeg+1, testSide2)
	if _, err := srv.SyncupDn(ctx, sweepDnReq(1, dst, sibling)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	syncupSideTwoPhase(t, srv, sweepDstReq(1, dst, testMigrId))
	syncupSideTwoPhase(t, srv, sweepSideReq(1, sibling))
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	metaName := nf.DnMigrMetaDmName(testCluster, testDn, testSp, testMigrId)
	srcNqn := nf.MigrSrcNqn(testCluster, testSrcDn, testSp, testMigrId)
	if !dmPresent(node, cloneName) || !dmPresent(node, metaName) ||
		!connPresent(node, srcNqn) {
		t.Fatal("the migration destination was never built")
	}

	stopTestServer(t, srv)
	node.mu.Lock()
	node.protos = map[string][]byte{}
	node.mu.Unlock()
	restarted := startTestServer(t, node)
	if _, err := restarted.SyncupDn(ctx,
		sweepDnReq(1, dst, sibling)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}

	node.Reset()
	reply, err := restarted.SyncupSide(ctx, sweepSideReq(1, sibling))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	for _, call := range node.Mutations() {
		if strings.Contains(call, cloneName) ||
			strings.Contains(call, metaName) ||
			strings.HasPrefix(call, "cmd nvme disconnect") {
			t.Errorf("the sibling's SyncupSide took the live migration "+
				"destination apart: %s", call)
		}
	}
	for _, name := range []string{cloneName, metaName} {
		if !dmPresent(node, name) {
			t.Errorf("%s did not survive the sibling's pass", name)
		}
	}
	if !connPresent(node, srcNqn) {
		t.Errorf("the connection to %s did not survive the sibling's pass",
			srcNqn)
	}
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("sibling SyncupSide code = %d (%s), want 0",
			got, reply.GetAgentReply().GetDetails())
	}
	infoReply, err := restarted.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sibling})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	if got := infoReply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("sibling verdict = %d (%s), want 0",
			got, infoReply.GetAgentReply().GetDetails())
	}

	node.Reset()
	dstReply, err := restarted.SyncupSide(ctx,
		sweepDstReq(1, dst, testMigrId))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := dstReply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("destination SyncupSide code = %d (%s), want 0",
			got, dstReply.GetAgentReply().GetDetails())
	}
	for _, call := range node.Mutations() {
		if strings.HasPrefix(call, "writeproto") {
			continue // SH5: the request itself is persisted
		}
		t.Errorf("the destination's SyncupSide rebuilt what was kept: %s",
			call)
	}
}

// ---------------------------------------------------------------------------
// 7. PushMigrBitmap (DN15, SH21-SH23)
// ---------------------------------------------------------------------------

// pushReq is one chunk on its way to the destination side. A push carries no
// revision ([D13]): it is position-addressed data keyed by an id that is never
// reused, so there is nothing for the side's stored revision to be compared
// with.
func pushReq(bmIdx uint32, migrId uint64, bitmap []byte) *pb.PushMigrBitmapRequest {
	return &pb.PushMigrBitmapRequest{
		ClusterId:   testCluster,
		DnId:        testDn,
		SidePointer: sidePtr(testSide),
		MigrId:      migrId,
		BmIdx:       bmIdx,
		Bitmap:      bitmap,
	}
}

// TestPushHasNoRevisionGate is the dn twin of the cn agent's test of the same
// name: PushMigrBitmap carries no revision and the handler compares none, so a
// chunk the worker planned against a report the side's stored revision has
// since superseded is applied rather than discarded. The old gate refused
// exactly that, and refusing it only ever threw away work that was about to be
// redone. The refusal that survives is the object one — a migration the stored
// request does not name is ReplyCodeUnknownObject with a message the worker
// logs.
func TestPushHasNoRevisionGate(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	// Revision 2 is the report the push below is planned from...
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	// ...and the side moves on to 5 before the push arrives.
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(5, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("advancing the stored revision: %v", err)
	}

	// Nothing on the wire can carry a revision any more, so no later edit can
	// reintroduce the gate without changing the proto.
	if (&pb.PushMigrBitmapRequest{}).ProtoReflect().Descriptor().
		Fields().ByName("revision") != nil {
		t.Fatal("PushMigrBitmapRequest still carries a revision field")
	}

	node.Reset()
	reply, err := srv.PushMigrBitmap(ctx, pushReq(0, testMigrId, []byte{0x05}))
	if err != nil {
		t.Fatalf("PushMigrBitmap: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("push code = %d (%s), want it accepted",
			reply.GetAgentReply().GetCode(),
			reply.GetAgentReply().GetDetails())
	}
	// Persisted AND applied: the chunk reached the file and the dm-clone.
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	assertOrder(t, node,
		"writeproto "+nf.LocalMigrBmPath(
			testCluster, testDn, testSp, testMigrId, 0),
		"cmd blkdiscard --offset 3145728 --length 1048576 "+
			nf.DmPath(cloneName),
	)
	// And it is in the applied set the next reply reports, which is the only
	// thing that stops the worker re-pushing it.
	sideReply, err := srv.SyncupSide(ctx,
		migrDstReq(6, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if got := sideReply.GetBmInfo().GetBmIdxList(); len(got) != 1 ||
		got[0] != 0 {
		t.Fatalf("applied set = %v, want the pushed chunk 0", got)
	}

	// The object refusal is untouched, message and all.
	unknown, err := srv.PushMigrBitmap(ctx,
		pushReq(0, testMigrId+1, []byte{0x05}))
	if err != nil {
		t.Fatalf("unknown migration: %v", err)
	}
	if got := unknown.GetAgentReply().GetCode(); got !=
		common.ReplyCodeUnknownObject {
		t.Fatalf("unknown migration: code = %d, want %d",
			got, common.ReplyCodeUnknownObject)
	}
	if unknown.GetAgentReply().GetDetails() == "" {
		t.Fatal("the refusal carried no message for the worker to log")
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
		common.DefaultLocalStorPrefix, testDisk, testTrConf(),
		common.NvmetPortId)
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
		common.DefaultLocalStorPrefix, testDisk, testTrConf(),
		common.NvmetPortId)
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

// breakSideDev makes the side device unreadable the way one transient
// `dmsetup info` failure does: Dm.Info collapses a failed probe into "the
// device is absent", so the converge tries to re-create it, fails, and returns
// sideDevFailed — the DN9 side-device gate, taken by a side
// that is in fact serving.
func breakSideDev(node *fakeNode, sideDevName string) {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.failCmdAlways["dmsetup info --columns --noheadings -o attr "+
		sideDevName] = "no such device"
	node.failCmdAlways["dmsetup create "+sideDevName] = "device already exists"
}

// The window has to end even when the converge that ends it cannot get past
// the side device: [D12] promises no dnv device stays suspended for more than
// the window plus one converge (DN12 rule 1's known limit aside, and a reload
// whose load fails, which fails closed: dnagent.md §2.8), and a
// suspended dm target queues bios with no timeout, so the promise is the
// safety property — not a best effort that a transient `dmsetup info` failure
// of the side device may drop.
func TestFenceEndsEvenWhenTheSideDeviceIsBroken(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	errNo := node.devNo[nf.DmPath(
		nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0))]

	srv.fenceWait = 50 * time.Millisecond
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	suspended := node.dms[linName].suspended
	node.mu.Unlock()
	if !suspended {
		t.Fatal("the cutover did not suspend the primary's dm-linear")
	}

	// The side device goes unreadable before the timer fires, so the converge
	// that ends the window takes the DN9 gate.
	breakSideDev(node, sideDevName)

	if !waitFor(t, 5*time.Second, func() bool {
		node.mu.Lock()
		defer node.mu.Unlock()
		return !node.dms[linName].suspended
	}) {
		t.Fatal("the grace window ended with the per-CN dm-linears still " +
			"suspended and nothing left to re-arm")
	}
	// Same proof as in TestFenceAdoptedSettlesAtTheGate, counted rather than
	// looked up: syncupBoth's create is already on the record, so a second one
	// is what says the converge that ended the window found the side device
	// unreadable instead of simply converging past it.
	if n := len(node.callsMatching(
		"cmd dmsetup create " + sideDevName)); n < 2 {
		t.Errorf("the converge that ended the window never took the DN9 "+
			"gate: %d side-device creates, want 2", n)
	}
	node.mu.Lock()
	table := node.dms[linName].table
	node.mu.Unlock()
	if !strings.Contains(table, errNo) {
		t.Errorf("the window ended without fencing the linear onto its "+
			"dm-error: %q", table)
	}
}

// The same for the other half of the bookkeeping: when the source role ends,
// the linears go back into service from the sweep's own pre-step, rather than
// from an ensureCnDm the DN9 gate may never let run.
func TestFenceClearedOnARoleEndWithABrokenSideDevice(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	stbName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1)
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)

	srv.fenceWait = time.Hour
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !node.dms[linName].suspended {
		t.Fatal("the cutover did not suspend the primary's dm-linear")
	}

	breakSideDev(node, sideDevName)
	// The migration is cancelled while the side device is unreadable.
	if _, err := srv.SyncupSide(ctx, sideReq(3, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	for _, name := range []string{linName, stbName} {
		node.mu.Lock()
		suspended := node.dms[name].suspended
		node.mu.Unlock()
		if suspended {
			t.Errorf("%s stayed suspended after the source role ended", name)
		}
	}
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	if srv.inFence(st) {
		t.Error("the grace window outlived the migration source role")
	}
}

// A side torn down inside the window still tears down: `dmsetup remove` does
// not succeed on a suspended device, so the teardown retires it onto its
// dm-error first — the phase-2 reload, which fails the IO the window absorbed
// rather than replaying it onto a side that is going away.
func TestTeardownInsideTheFenceWindow(t *testing.T) {
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

	node.Reset()
	if _, err := srv.SyncupDn(ctx, dnReq(3)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	// The reload comes before the nvmet teardown, not just before the
	// remove: disabling a namespace waits for its in-flight IO, and a
	// suspended linear does not complete what it holds.
	assertOrder(t, node,
		"cmd dmsetup reload "+linName,
		"cmd dmsetup resume "+linName,
		"writedirect "+agent.NvmetRoot+"/subsystems/"+
			nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0)+
			"/namespaces/1/enable=0",
		"cmd dmsetup remove "+linName,
	)
	node.mu.Lock()
	released := append([]string(nil), node.releasedOnto[linName]...)
	node.mu.Unlock()
	if len(released) != 1 ||
		!strings.HasSuffix(released[0], " linear "+errNo+" 0") {
		t.Errorf("%s released its deferred IO against %q, want its "+
			"dm-error %s, once", linName, released, errNo)
	}
	if _, ok := node.dms[linName]; ok {
		t.Error("a suspended dm-linear survived the teardown")
	}
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); ok {
		t.Error("the side's extents were not freed")
	}
}

// A teardown inside the window whose primary export this pass cannot
// attribute: the read of its namespace's device_path does not answer, so the
// export stays out of the chain, and the linear under it is in the chain only
// as a linear to remove, not as one under an export to remove. P0 has to
// retire it all the same. Otherwise L2's removeDm finds it suspended and
// resumes it onto its pre-fence table, replaying what the window absorbed
// onto a side that is being deleted: writes the old primary is told
// succeeded, which the destination never gets.
func TestTeardownInsideTheFenceWindowOverAnUnreadExport(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	errNo := node.devNo[nf.DmPath(
		nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0))]
	nqn := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0)

	srv.fenceWait = time.Hour
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !node.dms[linName].suspended {
		t.Fatal("the cutover did not suspend the primary's dm-linear")
	}

	node.Reset()
	setHook(node, node.killRead,
		agent.NvmetRoot+"/subsystems/"+nqn+"/namespaces/1/device_path")
	reply, err := srv.SyncupDn(ctx, dnReq(3))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	// The premise: the pass could not attribute the export, so it kept it
	// and named the read that failed.
	if !subsysPresent(node, nqn) {
		t.Fatalf("%s went although its namespace could not be read", nqn)
	}
	if got, details := reply.GetAgentReply().GetCode(),
		reply.GetAgentReply().GetDetails(); got != common.ReplyCodeLeftover ||
		!strings.Contains(details, "nvmet "+nqn+" ns 1 device_path") {
		t.Errorf("code = %d (%s), want %d naming the unanswered "+
			"device_path read", got, details, common.ReplyCodeLeftover)
	}
	node.mu.Lock()
	released := append([]string(nil), node.releasedOnto[linName]...)
	node.mu.Unlock()
	if len(released) == 0 {
		t.Errorf("%s was never brought out of suspension", linName)
	}
	for _, tb := range released {
		if !strings.HasSuffix(tb, " linear "+errNo+" 0") {
			t.Errorf("%s released its deferred IO against %q, not its "+
				"dm-error %s", linName, tb, errNo)
		}
	}
	// The retire comes before L2's removal reaches the linear.
	assertOrder(t, node,
		"cmd dmsetup reload "+linName,
		"cmd dmsetup resume "+linName,
		"cmd dmsetup remove "+linName,
	)
}

// A level raise to SP_LEVEL_NO_SIDE inside the window takes the exports off
// linears the fence holds suspended. At that level the linears stay wanted and
// only the exports above them go, so a sweep that brought out of suspension
// only the linears it was about to REMOVE touched none of them, and L1
// disabled every namespace over a suspended device — a write that waits for
// the namespace's in-flight IO, which is what the window holds, so it does
// not return; it sits with the node read lock and the side's object lock
// held, and the next SyncupDn, every Check round and the fence timer itself
// queue behind it. The level change has to end the window the way its
// deadline would have: each linear put on its dm-error and resumed before its
// namespace is disabled, so the IO the window absorbed fails instead of
// replaying onto the side's data, and nothing is left suspended for the build
// phase or a later probe to find.
func TestNoSideInsideTheFenceWindow(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	cnIds := []uint64{testCn0, testCn1}

	srv.fenceWait = time.Hour
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	for _, cnId := range cnIds {
		name := nf.DnLinearName(testCluster, testDn, testSp, testSide, cnId)
		if !node.dms[name].suspended {
			t.Fatalf("the cutover did not suspend %s", name)
		}
	}

	node.Reset()
	req := migrSrcReq(3)
	req.SideConf.SpLevel = pb.SpLevel_SP_LEVEL_NO_SIDE
	reply, err := srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	wedged := append([]string(nil), node.wedgedDisables...)
	node.mu.Unlock()
	for _, path := range wedged {
		t.Errorf("namespace disabled over a suspended dm-linear, a write "+
			"that waits on the IO the device holds: %s", path)
	}

	for _, cnId := range cnIds {
		linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, cnId)
		errNo := node.devNo[nf.DmPath(
			nf.DnErrorName(testCluster, testDn, testSp, testSide, cnId))]
		nqn := nf.SideToCnNqn(testCluster, testSp, testLeg, cnId)
		disable := "writedirect " + agent.NvmetRoot + "/subsystems/" + nqn +
			"/namespaces/1/enable=0"

		// Phase 2: what the window deferred is released against the
		// dm-error, never replayed through the pre-fence table...
		node.mu.Lock()
		released := append([]string(nil), node.releasedOnto[linName]...)
		node.mu.Unlock()
		if len(released) == 0 {
			t.Errorf("%s was never brought out of suspension", linName)
		}
		for _, table := range released {
			if !strings.HasSuffix(table, " linear "+errNo+" 0") {
				t.Errorf("%s released its deferred IO against %q, not its "+
					"dm-error %s", linName, table, errNo)
			}
		}
		// ...and before the namespace above it is disabled.
		resume := node.indexOfCall("cmd dmsetup resume " + linName)
		if at := node.indexOfCall(disable); resume < 0 || at < 0 ||
			resume > at {
			t.Errorf("%s: resumed at %d, namespace disabled at %d; the "+
				"resume must come first:\n%s", linName, resume, at,
				strings.Join(node.Calls(), "\n"))
		}
		// The primary's linear gets there by exactly one reload; a
		// standby's pre-fence table already is its dm-error, so its phase 2
		// is the resume alone.
		reloads := node.callsMatching("cmd dmsetup reload " + linName)
		want := 0
		if cnId == testCn0 {
			want = 1
		}
		if len(reloads) != want {
			t.Errorf("%s reloaded %d times, want %d: %q",
				linName, len(reloads), want, reloads)
		}
		// The window is over: nothing re-suspended it, and it serves the
		// dm-error the level leaves it on.
		if node.dms[linName].suspended {
			t.Errorf("%s is still suspended after the level change", linName)
		}
		if !strings.Contains(node.dms[linName].table, errNo) {
			t.Errorf("%s is not on its dm-error: %q",
				linName, node.dms[linName].table)
		}
		if subsysPresent(node, nqn) {
			t.Errorf("SP_LEVEL_NO_SIDE kept the export %s", nqn)
		}
	}
	srcNqn := nf.MigrSrcNqn(testCluster, testDn, testSp, testMigrId)
	if subsysPresent(node, srcNqn) {
		t.Errorf("SP_LEVEL_NO_SIDE kept the migration-source export %s",
			srcNqn)
	}
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	if srv.inFence(st) {
		t.Error("the level change did not end the grace window")
	}
	if st.fenceTimer != nil {
		t.Error("the grace-window timer outlived the window")
	}

	// The pass completes, and the rows are the ones the level demands: the
	// dm layer serving, no export layer. A probe agrees — one still expecting
	// the pre-fence table would report the primary's linear as broken.
	probe, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	for _, got := range []struct {
		what  string
		reply *pb.AgentReply
		info  *pb.SideInfo
	}{
		{"SyncupSide", reply.GetAgentReply(), reply.GetSideInfo()},
		{"GetSideInfo", probe.GetAgentReply(), probe.GetSideInfo()},
	} {
		if got.reply.GetCode() != 0 {
			t.Errorf("%s code = %d (%s), want 0", got.what,
				got.reply.GetCode(), got.reply.GetDetails())
		}
		for _, cnId := range cnIds {
			for _, row := range []struct {
				name string
				info *pb.ResInfo
			}{
				{"dm_error", got.info.GetCnIdToDmError()[cnId]},
				{"dm_linear", got.info.GetCnIdToDmLinear()[cnId]},
			} {
				if row.info.GetStatus() != pb.ResStatus_RES_STATUS_OK ||
					strings.Contains(row.info.GetDetails(), "grace window") {
					t.Errorf("%s: cn %d %s = %v/%q, want OK outside the "+
						"window", got.what, cnId, row.name,
						row.info.GetStatus(), row.info.GetDetails())
				}
			}
		}
		if n := len(got.info.GetCnIdToNvmeof()); n != 0 {
			t.Errorf("%s reported %d export rows at SP_LEVEL_NO_SIDE",
				got.what, n)
		}
		src := got.info.GetMigrSrcInfo()
		if src.GetDmLinearInfo().GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("%s: migr_src dm_linear = %v/%q, want OK", got.what,
				src.GetDmLinearInfo().GetStatus(),
				src.GetDmLinearInfo().GetDetails())
		}
		if src.GetNvmeofInfo() != nil {
			t.Errorf("%s reported a migr_src export at SP_LEVEL_NO_SIDE: %v",
				got.what, src.GetNvmeofInfo())
		}
	}
}

// P0 is the proof L1 needs, so a linear it cannot bring out of suspension
// holds the whole descent: no namespace above a device that may still be
// suspended is disabled, and the exports are named as leftovers, which is what
// re-drives the pass. The build phase that follows still retires the linear —
// the window is over at this level — so the next pass takes the exports. A
// failed retire is never a bare resume: whatever releases the IO the window
// absorbed releases it against the dm-error, not the pre-fence table. The
// reload fails all three ways a command can: refused (it answered no), and
// both halves of "did not answer" — killed before it touched anything, and
// killed after its ioctl landed anyway.
func TestNoSideRetireFailureHoldsTheExports(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(node *fakeNode, key string)
	}{
		{"refused", func(node *fakeNode, key string) {
			node.failCmd[key] = "device-mapper: reload ioctl failed: " +
				"Invalid argument"
		}},
		{"killed before it ran", func(node *fakeNode, key string) {
			node.killCmdNoEffect[key] = true
		}},
		{"killed after it ran", func(node *fakeNode, key string) {
			node.killCmd[key] = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			linName := nf.DnLinearName(
				testCluster, testDn, testSp, testSide, testCn0)
			errNo := node.devNo[nf.DmPath(nf.DnErrorName(
				testCluster, testDn, testSp, testSide, testCn0))]
			nqns := []string{
				nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0),
				nf.SideToCnNqn(testCluster, testSp, testLeg, testCn1),
				nf.MigrSrcNqn(testCluster, testDn, testSp, testMigrId),
			}

			srv.fenceWait = time.Hour
			if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if !node.dms[linName].suspended {
				t.Fatal("the cutover did not suspend the primary's dm-linear")
			}

			node.Reset()
			node.mu.Lock()
			tc.fail(node, "dmsetup reload "+linName)
			node.mu.Unlock()
			req := migrSrcReq(3)
			req.SideConf.SpLevel = pb.SpLevel_SP_LEVEL_NO_SIDE
			reply, err := srv.SyncupSide(ctx, req)
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			node.mu.Lock()
			released := append([]string(nil), node.releasedOnto[linName]...)
			suspended := node.dms[linName].suspended
			table := node.dms[linName].table
			node.mu.Unlock()
			for _, tb := range released {
				if !strings.HasSuffix(tb, " linear "+errNo+" 0") {
					t.Errorf("%s released its deferred IO against %q, not "+
						"its dm-error %s: a failed retire must never "+
						"resume onto the pre-fence table", linName, tb, errNo)
				}
			}
			if suspended || !strings.Contains(table, errNo) {
				t.Errorf("the build phase did not retire %s after P0 "+
					"failed: suspended=%v table=%q", linName, suspended,
					table)
			}
			if node.hasCall("/enable=0") {
				t.Errorf("L1 ran over a linear P0 could not retire:\n%s",
					strings.Join(node.Calls(), "\n"))
			}
			for _, nqn := range nqns {
				if !subsysPresent(node, nqn) {
					t.Errorf("%s was removed although P0 held the descent", nqn)
				}
			}
			if got := reply.GetAgentReply().GetCode(); got !=
				common.ReplyCodeLeftover {
				t.Errorf("code = %d (%s), want %d: the held exports must "+
					"re-drive the pass", got,
					reply.GetAgentReply().GetDetails(),
					common.ReplyCodeLeftover)
			}

			node.Reset()
			req = migrSrcReq(4)
			req.SideConf.SpLevel = pb.SpLevel_SP_LEVEL_NO_SIDE
			reply, err = srv.SyncupSide(ctx, req)
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if got := reply.GetAgentReply().GetCode(); got != 0 {
				t.Errorf("re-driven pass: code = %d (%s), want 0", got,
					reply.GetAgentReply().GetDetails())
			}
			for _, nqn := range nqns {
				if subsysPresent(node, nqn) {
					t.Errorf("the re-driven pass kept %s", nqn)
				}
			}
			node.mu.Lock()
			wedged := append([]string(nil), node.wedgedDisables...)
			node.mu.Unlock()
			for _, path := range wedged {
				t.Errorf("namespace disabled over a suspended dm-linear: %s",
					path)
			}
		})
	}
}

// A P0 probe of a linear that does not answer proves nothing: the linear may
// still be suspended, so the descent stops before L1 exactly as it does for a
// failed reload, and the next pass, whose probe answers, retires the linear
// onto its dm-error and takes the exports. The kill is the always form
// because the pre-step's repoint probes the same linear first and would take
// a one-shot kill; it also keeps the build phase from retiring the linear in
// the same pass, so the retire this test sees is the next pass's P0.
func TestNoSideProbeFailureHoldsTheExports(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	errNo := node.devNo[nf.DmPath(
		nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0))]
	nqns := []string{
		nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0),
		nf.SideToCnNqn(testCluster, testSp, testLeg, testCn1),
		nf.MigrSrcNqn(testCluster, testDn, testSp, testMigrId),
	}

	srv.fenceWait = time.Hour
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !node.dms[linName].suspended {
		t.Fatal("the cutover did not suspend the primary's dm-linear")
	}

	node.Reset()
	probe := "dmsetup info --columns --noheadings -o attr " + linName
	setHook(node, node.killCmdNoEffectAlways, probe)
	req := migrSrcReq(3)
	req.SideConf.SpLevel = pb.SpLevel_SP_LEVEL_NO_SIDE
	reply, err := srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if node.hasCall("/enable=0") {
		t.Errorf("L1 ran over a linear P0 could not probe:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	for _, nqn := range nqns {
		if !subsysPresent(node, nqn) {
			t.Errorf("%s was removed although P0 held the descent", nqn)
		}
	}
	if got := reply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeLeftover {
		t.Errorf("code = %d (%s), want %d: the held exports must "+
			"re-drive the pass", got, reply.GetAgentReply().GetDetails(),
			common.ReplyCodeLeftover)
	}
	for _, nqn := range nqns {
		if !strings.Contains(reply.GetAgentReply().GetDetails(), nqn) {
			t.Errorf("the held export %s is not named as a leftover: %s",
				nqn, reply.GetAgentReply().GetDetails())
		}
	}
	node.mu.Lock()
	released := append([]string(nil), node.releasedOnto[linName]...)
	suspended := node.dms[linName].suspended
	node.mu.Unlock()
	if len(released) != 0 || !suspended {
		t.Errorf("%s left suspension in a pass that could not probe it: "+
			"released against %q, suspended=%v", linName, released, suspended)
	}

	clearHook(node, node.killCmdNoEffectAlways, probe)
	node.Reset()
	req = migrSrcReq(4)
	req.SideConf.SpLevel = pb.SpLevel_SP_LEVEL_NO_SIDE
	reply, err = srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("re-driven pass: code = %d (%s), want 0", got,
			reply.GetAgentReply().GetDetails())
	}
	node.mu.Lock()
	wedged := append([]string(nil), node.wedgedDisables...)
	released = append([]string(nil), node.releasedOnto[linName]...)
	node.mu.Unlock()
	for _, path := range wedged {
		t.Errorf("namespace disabled over a suspended dm-linear: %s", path)
	}
	if len(released) != 1 ||
		!strings.HasSuffix(released[0], " linear "+errNo+" 0") {
		t.Errorf("the re-driven pass released %s's deferred IO against %q, "+
			"want its dm-error %s, once", linName, released, errNo)
	}
	for _, nqn := range nqns {
		if subsysPresent(node, nqn) {
			t.Errorf("the re-driven pass kept %s", nqn)
		}
	}
}

// Lowering the level again, with the source role still standing, must not
// reopen a window the level ended. The exports come back over linears phase 2
// has already put on their dm-errors; a second window would suspend them there
// again, and a probe — which inside a window expects the pre-fence table —
// would report the primary's linear as broken for the whole of it.
func TestNoSideThenLevelDownOpensNoSecondWindow(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	cnIds := []uint64{testCn0, testCn1}

	srv.fenceWait = time.Hour
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	noSide := migrSrcReq(3)
	noSide.SideConf.SpLevel = pb.SpLevel_SP_LEVEL_NO_SIDE
	if _, err := srv.SyncupSide(ctx, noSide); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}

	// The level comes back down; the migration still stands.
	node.Reset()
	reply, err := srv.SyncupSide(ctx, migrSrcReq(4))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("code = %d (%s), want 0", got,
			reply.GetAgentReply().GetDetails())
	}
	suspends := node.callsMatching("cmd dmsetup suspend")
	if len(suspends) != 0 {
		t.Errorf("the level coming back down suspended %d times, want 0: %q",
			len(suspends), suspends)
	}
	for _, cnId := range cnIds {
		linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, cnId)
		errNo := node.devNo[nf.DmPath(
			nf.DnErrorName(testCluster, testDn, testSp, testSide, cnId))]
		node.mu.Lock()
		suspended := node.dms[linName].suspended
		table := node.dms[linName].table
		node.mu.Unlock()
		if suspended || !strings.Contains(table, errNo) {
			t.Errorf("%s: suspended=%v table=%q, want resumed on its "+
				"dm-error %s", linName, suspended, table, errNo)
		}
	}

	// A probe reads the rebuilt side as healthy, row by row: no window is
	// running, so it expects the dm-errors the linears are on, and the
	// exports, the migration source's included, are back.
	probe, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	if got := probe.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("GetSideInfo code = %d (%s), want 0", got,
			probe.GetAgentReply().GetDetails())
	}
	info := probe.GetSideInfo()
	if n := len(info.GetCnIdToNvmeof()); n != len(cnIds) {
		t.Errorf("GetSideInfo reported %d export rows, want %d",
			n, len(cnIds))
	}
	type row struct {
		name string
		info *pb.ResInfo
	}
	rows := []row{
		{"side_dev", info.GetSideDevInfo()},
		{"migr_src dm_linear", info.GetMigrSrcInfo().GetDmLinearInfo()},
		{"migr_src nvmeof", info.GetMigrSrcInfo().GetNvmeofInfo()},
	}
	for _, cnId := range cnIds {
		rows = append(rows,
			row{fmt.Sprintf("cn %d dm_error", cnId),
				info.GetCnIdToDmError()[cnId]},
			row{fmt.Sprintf("cn %d dm_linear", cnId),
				info.GetCnIdToDmLinear()[cnId]},
			row{fmt.Sprintf("cn %d nvmeof", cnId),
				info.GetCnIdToNvmeof()[cnId]})
	}
	for _, r := range rows {
		if r.info.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("GetSideInfo: %s = %v/%q, want OK", r.name,
				r.info.GetStatus(), r.info.GetDetails())
		}
	}
}

// The mark a level with no export layer leaves ends with the source role, as
// the restart mark does: the side's next migration is entitled to its whole
// window. A mark that outlived the role would skip that window and error the
// old primary's in-flight writes in the same pass that moved its namespaces
// away — what TestFenceWindowSurvivesAnUnrelatedAgentRestart pins for the
// restart mark.
func TestFenceEndedDoesNotOutliveTheRole(t *testing.T) {
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
	noSide := migrSrcReq(3)
	noSide.SideConf.SpLevel = pb.SpLevel_SP_LEVEL_NO_SIDE
	if _, err := srv.SyncupSide(ctx, noSide); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	// The migration is cancelled and the level lowered: the role ends.
	if _, err := srv.SyncupSide(ctx, sideReq(4, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}

	// The side's next migration.
	node.Reset()
	next := migrSrcReq(5)
	next.MigrSrcConf.MigrId = testMigrId + 1
	if _, err := srv.SyncupSide(ctx, next); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	suspended := node.dms[linName].suspended
	table := node.dms[linName].table
	node.mu.Unlock()
	if !suspended {
		t.Error("the next cutover skipped its grace window")
	}
	if reloads := node.callsMatching(
		"cmd dmsetup reload " + linName); len(reloads) != 0 {
		t.Errorf("phase 2 ran inside the next window: %q", reloads)
	}
	if !strings.Contains(table, sideNo) {
		t.Errorf("phase 1 moved the primary's table off the side device: %q",
			table)
	}
}

// A retire P0 could not finish, in a pass that then stops at the side-device
// gate. The level has ended the window and stopped its timer, so the gate's
// fence bookkeeping is what finishes phase 2 in this pass, as it does for a
// window that has elapsed: [D12] bounds a suspension at the window plus one
// converge, and the next one is whenever the worker re-drives the leftovers.
func TestNoSideRetireFailureAtTheGate(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	errNo := node.devNo[nf.DmPath(
		nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0))]

	srv.fenceWait = time.Hour
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !node.dms[linName].suspended {
		t.Fatal("the cutover did not suspend the primary's dm-linear")
	}

	breakSideDev(node, sideDevName)
	setHook(node, node.killCmdNoEffect, "dmsetup reload "+linName)
	node.Reset()
	req := migrSrcReq(3)
	req.SideConf.SpLevel = pb.SpLevel_SP_LEVEL_NO_SIDE
	reply, err := srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	// As in TestFenceAdoptedSettlesAtTheGate: a `dmsetup create` of the side
	// device is attempted only when the converge found it unreadable, so the
	// pass took the DN9 gate rather than retiring the linear in ensureCnDm.
	if !node.hasCall("cmd dmsetup create " + sideDevName) {
		t.Fatalf("the converge never took the DN9 gate:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	if node.hasCall("/enable=0") {
		t.Errorf("L1 ran over a linear P0 could not retire:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	if got := reply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeLeftover {
		t.Errorf("code = %d (%s), want %d", got,
			reply.GetAgentReply().GetDetails(), common.ReplyCodeLeftover)
	}
	node.mu.Lock()
	suspended := node.dms[linName].suspended
	table := node.dms[linName].table
	released := append([]string(nil), node.releasedOnto[linName]...)
	node.mu.Unlock()
	if suspended {
		t.Errorf("%s left suspended by a pass that stopped at the gate "+
			"after the level ended its window", linName)
	}
	if !strings.Contains(table, errNo) {
		t.Errorf("%s is not on its dm-error: %q", linName, table)
	}
	for _, tb := range released {
		if !strings.HasSuffix(tb, " linear "+errNo+" 0") {
			t.Errorf("%s released its deferred IO against %q, not its "+
				"dm-error %s", linName, tb, errNo)
		}
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
		common.DefaultLocalStorPrefix, testDisk, testTrConf(),
		common.NvmetPortId)
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

// ...and the converse: a restart of a side that was never fenced must not
// consume the *next* cutover's window. The adoption is a rule about the
// debris a previous process left suspended, and [D12] bounds how long a device
// stays suspended — it does not licence skipping the window that bound
// applies to.
func TestFenceWindowSurvivesAnUnrelatedAgentRestart(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	sideNo := node.devNo[nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide))]

	// A routine restart of a side that is simply serving: no migration, and
	// nothing of its own suspended.
	stopTestServer(t, srv)
	node.Reset()
	restarted := startTestServer(t, node)
	restarted.fenceWait = time.Hour

	// The migration is created only afterwards, so its cutover is entitled to
	// the whole common.SuspendSeconds window.
	if _, err := restarted.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	suspended := node.dms[linName].suspended
	table := node.dms[linName].table
	node.mu.Unlock()
	if !suspended {
		t.Error("the first cutover after an unrelated restart skipped the " +
			"grace window")
	}
	if node.hasCall("cmd dmsetup reload " + linName) {
		t.Error("phase 2 ran inside the window")
	}
	if !strings.Contains(table, sideNo) {
		t.Errorf("phase 1 moved the primary's table off the side device: %q",
			table)
	}
}

// The conjunction the two restart tests above leave open: an agent restart
// inside the window *and* a first converge that cannot get past the side
// device. DN12 rule 1 makes the adopted fence elapsed, rule 4 makes the DN9
// gate skip everything above the side device but never the fence — so the
// gate backstop has to finish phase 2 for a window this process never
// started.
func TestFenceAdoptedSettlesAtTheGate(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	stbName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1)
	sideDevName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	errNo := node.devNo[nf.DmPath(
		nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0))]
	stbErrNo := node.devNo[nf.DmPath(
		nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn1))]

	srv.fenceWait = time.Hour
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	for _, name := range []string{linName, stbName} {
		if !node.dms[name].suspended {
			t.Fatalf("the cutover did not suspend %s", name)
		}
	}

	// The kernel state survives the restart; the side device goes unreadable
	// before the restarted agent converges, so its first pass takes the gate.
	breakSideDev(node, sideDevName)
	node.Reset()
	restarted := NewDnAgentServer(node.osClient(),
		common.NewNameFmt(common.DefaultLocalStorPrefix),
		common.DefaultLocalStorPrefix, testDisk, testTrConf(),
		common.NvmetPortId)
	restarted.fenceWait = time.Hour
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Every mutation asserted below is also produced by the gate-OPEN path
	// through ensureCnDm, so the pass has to be proved gated or this test
	// silently degenerates into TestFenceNotRestartedAcrossAnAgentRestart.
	// A `dmsetup create` of the side device is attempted only when Dm.Info
	// collapsed breakSideDev's injected probe failure into "absent", and it
	// can only fail — the device is in fact there — so the converge returned
	// sideDevFailed and took the DN9 gate.
	if !node.hasCall("cmd dmsetup create " + sideDevName) {
		t.Fatalf("the converge never took the DN9 gate, so it proves nothing "+
			"about the adopted fence:\n%s", strings.Join(node.Calls(), "\n"))
	}

	// The same mutation set the elapsed-window case produces: the primary's
	// linear reloaded onto its dm-error, every per-CN linear resumed.
	if !node.hasCall("cmd dmsetup reload " + linName) {
		t.Fatalf("the gated pass left the adopted fence in phase 1:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	for _, want := range []struct {
		name  string
		devNo string
	}{{linName, errNo}, {stbName, stbErrNo}} {
		if node.dms[want.name].suspended {
			t.Errorf("%s outlived the adopted window suspended", want.name)
		}
		if !strings.Contains(node.dms[want.name].table, want.devNo) {
			t.Errorf("%s was resumed without being fenced onto its dm-error: %q",
				want.name, node.dms[want.name].table)
		}
	}
	// An adopted window is elapsed, never running, so there is nothing to wait
	// for: a timer here would take its deadline from a zero fenceAt.
	st := restarted.getSide(sideKey(testCluster, testDn, testSp, testSide))
	if restarted.inFence(st) {
		t.Error("the adopted fence reported a running window")
	}
	if st.fenceTimer != nil {
		t.Error("the gated pass armed a timer for an adopted fence")
	}
}

// TestMigrationConnectSucceedsFromTheRetryLoop pins the DN8 loop's one
// non-obvious property: the converge that finally connects must survive its
// own call to stopMigrRetry.
//
// The existing retry test above drives the second converge from an RPC, and
// an RPC-driven converge runs on the gRPC context — which stopMigrRetry's
// cancel cannot touch. That is why it passed for as long as the bug was
// there. The retry LOOP's converge is the only one that runs on the very
// context stopMigrRetry cancels, so it is the only shape in which "connect
// succeeded" and "cancel everything" happen in the same pass, in that order.
// Before the fix every OS call after the stopMigrRetry failed on the dead
// context: the dm-clone was never created, `retrying` was already false so
// nothing ticked again, and the side sat at `dm_clone: RES_STATUS_MISSING,
// target not connected` for ever while the controller it names was `live`.
// An e2e `copy` case measured exactly that on 2026-09-19, after one transient
// connect failure.
//
// The assertion is the dm-clone's existence, not a call order: what the bug
// destroyed was the REST of the converge, so the thing to pin is that the
// rest of it ran.
func TestMigrationConnectSucceedsFromTheRetryLoop(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	srv.migrRetryInterval = 5 * time.Millisecond
	syncupBoth(t, srv, 1, testSide)

	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)

	// One transient failure, exactly as the lab produced: the first connect
	// is refused, every later one succeeds.
	node.failCmd["nvme connect"] = "failed to write to nvme-fabrics device"
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	srv.mu.Lock()
	retrying := st.retrying
	srv.mu.Unlock()
	if !retrying {
		t.Fatal("fixture is wrong: no background retry was registered")
	}
	if dmPresent(node, cloneName) {
		t.Fatal("fixture is wrong: the dm-clone was built despite the " +
			"refused connect")
	}

	// No further RPC. The retry loop alone has to get there.
	deadline := time.Now().Add(5 * time.Second)
	for !dmPresent(node, cloneName) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !dmPresent(node, cloneName) {
		srv.mu.Lock()
		retrying = st.retrying
		srv.mu.Unlock()
		t.Fatalf("the retry loop connected but never built %s "+
			"(retrying = %v): the converge that called stopMigrRetry "+
			"cancelled its own context", cloneName, retrying)
	}
	srv.mu.Lock()
	retrying = st.retrying
	srv.mu.Unlock()
	if retrying {
		t.Error("the retry registration survived a successful connect")
	}
}

// ---------------------------------------------------------------------------
// DN13 step (3): the wait for the source namespace
// ---------------------------------------------------------------------------

// dnClock is a fake clock for the dn server's two seams: its sleep moves it
// on by exactly the duration asked for, and every call is counted.
type dnClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps int
}

func (c *dnClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *dnClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.sleeps++
	return ctx.Err()
}

// withDnClock puts the server, and the fake node's configfs stamps, on one
// fake clock.
func withDnClock(srv *DnAgentServer, node *fakeNode) *dnClock {
	clock := &dnClock{now: time.Unix(1<<30, 0)}
	srv.now = clock.Now
	srv.sleep = clock.Sleep
	node.mu.Lock()
	node.clock = clock.Now
	node.mu.Unlock()
	return clock
}

// TestMigrationDestinationAwaitsTheSourceNamespace pins DN13 step (3)'s wait:
// the kernel returns from `nvme connect` once the controller is live and only
// queues the scan that adds the namespace node, so the destination's single
// re-read used to find a controller and no namespace and fail the target
// "controller has no namespace" for a device milliseconds away. The pass now
// re-reads, in DnMigrDstNsPause steps, until the device is there — still with
// exactly one connect, so the dm-clone is built on the first reply and no DN8
// retry is registered.
func TestMigrationDestinationAwaitsTheSourceNamespace(t *testing.T) {
	for _, misses := range []int{1, 3} {
		t.Run(fmt.Sprintf("namespace after %d missed read(s)", misses),
			func(t *testing.T) {
				srv, node := newTestServer(t)
				ctx := context.Background()
				syncupBoth(t, srv, 1, testSide)
				clock := withDnClock(srv, node)
				srcNqn := srv.nf.MigrSrcNqn(
					testCluster, testSrcDn, testSp, testMigrId)
				node.mu.Lock()
				node.nsMisses[srcNqn] = misses
				node.mu.Unlock()
				start := clock.Now()

				node.Reset()
				reply, err := srv.SyncupSide(ctx,
					migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE))
				if err != nil {
					t.Fatalf("SyncupSide: %v", err)
				}
				info := reply.GetSideInfo().GetMigrDstInfo()
				if target := info.GetTargetInfo(); target.GetStatus() !=
					pb.ResStatus_RES_STATUS_OK {
					t.Fatalf("target_info = %v %q, want OK: the namespace "+
						"came %d read(s) after the connect",
						target.GetStatus(), target.GetDetails(), misses)
				}
				if dm := info.GetDmCloneInfo(); dm.GetStatus() !=
					pb.ResStatus_RES_STATUS_OK {
					t.Fatalf("dm_clone_info = %v %q, want OK",
						dm.GetStatus(), dm.GetDetails())
				}
				if n := len(node.callsMatching(
					"cmd nvme connect ")); n != 1 {
					t.Fatalf("%d connects, want exactly 1 (DN13)", n)
				}
				if got, want := clock.Now().Sub(start), time.Duration(
					misses)*common.DnMigrDstNsPause; got != want {
					t.Fatalf("the pass waited %v, want %d pause(s), %v",
						got, misses, want)
				}
				st := srv.getSide(sideKey(testCluster, testDn, testSp,
					testSide))
				if st.retrying {
					t.Fatalf("a connect whose namespace came registered " +
						"the DN8 retry")
				}
			})
	}

	// The wait is bounded: a namespace that never comes costs the pass
	// DnMigrDstNsWait and ends it exactly as the single re-read did — the
	// target reads "controller has no namespace" and the DN8 loop takes over.
	t.Run("namespace never comes", func(t *testing.T) {
		srv, node := newTestServer(t)
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		clock := withDnClock(srv, node)
		srcNqn := srv.nf.MigrSrcNqn(testCluster, testSrcDn, testSp, testMigrId)
		node.mu.Lock()
		node.nsMisses[srcNqn] = 1000
		node.mu.Unlock()
		start := clock.Now()

		reply, err := srv.SyncupSide(ctx,
			migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		target := reply.GetSideInfo().GetMigrDstInfo().GetTargetInfo()
		if target.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
			target.GetDetails() != "controller has no namespace" {
			t.Fatalf("target_info = %v %q, want ERROR "+
				"\"controller has no namespace\"",
				target.GetStatus(), target.GetDetails())
		}
		if got := clock.Now().Sub(start); got != common.DnMigrDstNsWait {
			t.Fatalf("the pass waited %v, want the bound %v", got,
				common.DnMigrDstNsWait)
		}
		st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
		if !st.retrying {
			t.Fatalf("no DN8 retry was registered")
		}
	})

	// A read that fails ends the wait at once: no pause, and the target
	// carries the read's error.
	t.Run("a failed read ends the wait", func(t *testing.T) {
		srv, node := newTestServer(t)
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		clock := withDnClock(srv, node)
		srcNqn := srv.nf.MigrSrcNqn(testCluster, testSrcDn, testSp, testMigrId)
		node.mu.Lock()
		node.nsMisses[srcNqn] = 1000
		node.mu.Unlock()
		// The source's subsystem is the only one this host holds, and it
		// exists only from the connect on: the read that fails is the
		// wait's.
		setHook(node, node.failReadAlways, "nvme-subsystem/nvme-subsys")
		start := clock.Now()

		reply, err := srv.SyncupSide(ctx,
			migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		target := reply.GetSideInfo().GetMigrDstInfo().GetTargetInfo()
		if target.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
			!strings.Contains(target.GetDetails(), "input/output error") {
			t.Fatalf("target_info = %v %q, want ERROR with the read's error",
				target.GetStatus(), target.GetDetails())
		}
		if n := len(node.callsMatching("cmd nvme connect ")); n != 1 {
			t.Fatalf("%d connects, want 1", n)
		}
		if waited := clock.Now().Sub(start); waited != 0 {
			t.Fatalf("the pass waited %v after a failed read, want none",
				waited)
		}
	})
}
