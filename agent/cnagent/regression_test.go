package cnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Regressions for the defects an adversarial review of the CN9 retire phase,
// the CN18 clone paths and the CN12/CN13 wrappers turned up. Each one failed
// before its fix; the fake node models dm/md holders, so an out-of-order
// teardown now surfaces as a real EBUSY rather than only on a node.

// A transfer's dm-linear maps the origin td's raid0. If the retire phase does
// not demote it to an error table, `dmsetup remove` of that raid0 fails EBUSY
// and the whole cascade behind it — thin volumes, pool, concats,
// `mdadm --stop` — fails with it, leaving a demoted cntlr with its arrays
// still assembled while the new primary assembles the same ones.
func TestFailoverWithTransferReleasesTheStack(t *testing.T) {
	srv, node := newTestServer(t)
	xfers := []*pb.Transfer{{
		XferId:      testXfer,
		OriNqn:      testNqn,
		OriNsIdx:    1,
		AutoSuspend: true,
	}}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, xfers: xfers})
	if _, ok := node.dms[raid0Name(srv, testTd)]; !ok {
		t.Fatalf("the fixture never built the raid0")
	}

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: false, xfers: xfers})); err != nil {
		t.Fatalf("demote: %v", err)
	}
	// The transfer device is demoted before anything under it is removed.
	assertOrder(t, node,
		"cmd dmsetup reload "+xferName(srv, testXfer),
		"cmd dmsetup remove "+raid0Name(srv, testTd),
	)
	for _, name := range []string{
		raid0Name(srv, testTd),
		thinName(srv, testTd),
		poolName(srv),
		srv.nf.CnPoolMetaName(testCluster, testCn, testSp, testSlice),
		srv.nf.CnPoolDataName(testCluster, testCn, testSp, testSlice),
		grpName(srv, testMetaGrp),
	} {
		if _, ok := node.dms[name]; ok {
			t.Fatalf("%s survived the demotion", name)
		}
	}
	// A standby keeps the transfer exported, error-backed (§3.4).
	table := node.dms[xferName(srv, testXfer)].table
	if !strings.Contains(table, " error") {
		t.Fatalf("the standby's transfer device is %q, want an error table",
			table)
	}
}

// CN22/CN19: a clone the role or the level merely suppresses keeps its chunk
// files. Deleting them made the worker re-push forever and left a promoted
// standby's §11.5 rebuild nothing to skip with.
func TestSuppressedCloneKeepsItsChunks(t *testing.T) {
	srv, node := newTestServer(t)
	// A standby that nonetheless carries the clone in its desired state.
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: false, clones: []*pb.Clone{cloneOf()}})
	pushBitmap(t, srv, 2, hexBytes(t, testSkipHex))
	bmPath := srv.nf.LocalCloneBmPath(
		testCluster, testCn, testSp, testClone, 0)
	if _, ok := node.protos[bmPath]; !ok {
		t.Fatalf("the chunk was not persisted")
	}

	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: false, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if _, ok := node.protos[bmPath]; !ok {
		t.Fatalf("a standby converge deleted the clone's chunk file")
	}
	if len(reply.GetBmInfoList()) != 1 ||
		len(reply.GetBmInfoList()[0].GetBmIdxList()) != 1 {
		t.Fatalf("the applied set was dropped: %v", reply.GetBmInfoList())
	}

	// Deleting the clone from clone_list does remove them (SH7).
	if _, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 4, primary: false})); err != nil {
		t.Fatalf("delete clone: %v", err)
	}
	if _, ok := node.protos[bmPath]; ok {
		t.Fatalf("a deleted clone kept its chunk file")
	}
}

// §11.5 keys off missing dm-clone *metadata*, not off the metadata wrapper: a
// pass that created the wrapper and then failed to build the dm-clone leaves a
// freshly discarded slot that dm-clone would format fresh, with nothing
// hydrated.
func TestCloneRecoveryWhenOnlyTheDmCloneVanished(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	loop := loopDev(t, srv, node)

	// The wrapper survives; the dm-clone does not.
	removeCloneDevice(node, srv)

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true,
		clones: []*pb.Clone{cloneOf()}})); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	pool := poolName(srv)
	assertOrder(t, node,
		"cmd dmsetup message "+pool+" 0 reserve_metadata_snap",
		"cmd thin_dump --metadata-snap ",
		"cmd dmsetup message "+pool+" 0 release_metadata_snap",
		"cmd dmsetup message "+cloneName(srv, testClone)+
			" 0 enable_hydration",
	)
	// A wrapper that is present and matches is reused as-is: "allocate"
	// strictly means "a new unit range was chosen", so the surviving slot is
	// never re-created and — decisively — never re-hole-punched, which would
	// wipe the valid dm-clone superblock it carries (CN18).
	assertNoCall(t, node, "cmd dmsetup create "+cloneMetaName(srv, testClone))
	assertNoCall(t, node, "cmd blkdiscard --offset 0 --length 8388608 "+loop)
}

// removeCloneDevice drops the dm-clone behind the agent's back, the way a
// crash between the wrapper's creation and `dmsetup create` leaves it.
func removeCloneDevice(node *fakeNode, srv *CnAgentServer) {
	name := cloneName(srv, testClone)
	delete(node.dms, name)
	delete(node.devNo, "/dev/mapper/"+name)
	delete(node.devSize, "/dev/mapper/"+name)
}

// §11.5: the destination bitmaps must be applied in full before the dm-clone
// handles any IO. A read that fails must therefore leave nothing able to serve
// or hydrate — not enable hydration and report OK.
func TestCloneRecoveryFailsClosed(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	removeCloneDevice(node, srv)

	node.Reset()
	node.failCmdAlways["thin_dump"] = "thin_dump: out of time"
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	info := reply.GetCntlrInfo().GetCloneIdToDmClone()[testClone]
	if info.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("a failed destination-bitmap read reported %v/%q",
			info.GetStatus(), info.GetDetails())
	}
	assertNoCall(t, node, "0 enable_hydration")
	if _, ok := node.dms[cloneName(srv, testClone)]; ok {
		t.Fatalf("the dm-clone was left able to serve and hydrate")
	}
	// Nothing serves the td: the ns-dev stays on the td's dm-error.
	table := node.dms[nsDevName(srv, testNs)].table
	errNo := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
	if !strings.Contains(table, "linear "+errNo) {
		t.Fatalf("the ns-dev is not parked: %q", table)
	}
}

// §11.1.1 puts "one leg available" in case 2 (assemble, let mdadm decide),
// never in case 1: creating with --assume-clean over the available subset
// would resync the unavailable survivor's data away.
func TestGroupNeverCreatesOverASubsetOfLegs(t *testing.T) {
	srv, node := newTestServer(t)
	// The second data leg's side exports dm-error, so its path is
	// non-optimized and the leg is not available.
	node.anaOf[srv.nf.SideToCnNqn(testCluster, testSp, testDataLeg2, testCn)+
		"|"+testIp2+":"+testSvcId2] = "non-optimized"

	reply := syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, raid1: true, twoLegs: true})
	mdDev := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	for _, call := range node.callsMatching("cmd mdadm --create") {
		if strings.Contains(call, mdDev) {
			t.Fatalf("created an array over a subset of legs: %s", call)
		}
	}
	info := reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]
	if info.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("the group reported %v, want ERROR", info.GetStatus())
	}
}

// CN13: a GrowSlice that appends only meta groups changes nothing in the
// thin-pool's own table, and dm-thin picks up a resized metadata device only
// in pool_preresume — so the pool must be reloaded anyway.
func TestGrowSliceReloadsThePool(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	grown := cntlrReq(reqOpts{revision: 3, primary: true})
	slice := grown.GetIdToSlice()[fmt.Sprintf(common.IdKeyFmt, testSlice)]
	second := &pb.Group{
		GrpId:      testMetaGrp + 0x100,
		ExtCnt:     1,
		MetaBlocks: 1,
		DataBlocks: 63,
		LegList: []*pb.Leg{legOf(testMetaLeg+0x100,
			sideOf(testMetaSide+0x200, testIp2, testSvcId2))},
	}
	slice.MetaGrpList = append(slice.MetaGrpList, second)

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), grown); err != nil {
		t.Fatalf("grow: %v", err)
	}
	metaName := srv.nf.CnPoolMetaName(
		testCluster, testCn, testSp, testSlice)
	assertOrder(t, node,
		"cmd dmsetup reload "+metaName,
		"cmd dmsetup reload "+poolName(srv),
	)
}

// CN16: `attr_allow_any_host = 1` is refused by nvmet while explicit host
// links remain, so emptying `allowed_hosts` must unlink first.
func TestAllowAnyHostIsWrittenAfterTheUnlink(t *testing.T) {
	srv, node := newTestServer(t)
	withHost := defaultSubsys(false)
	withHost[testNqn].AllowedHosts = []string{testHostNqn}
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, subsys: withHost})
	linkPath := "/sys/kernel/config/nvmet/subsystems/" + testNqn +
		"/allowed_hosts/" + testHostNqn
	if _, ok := node.links[linkPath]; !ok {
		t.Fatalf("the host was never linked")
	}

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true})); err != nil {
		t.Fatalf("empty the host list: %v", err)
	}
	attrPath := "/sys/kernel/config/nvmet/subsystems/" + testNqn +
		"/attr_allow_any_host"
	assertOrder(t, node,
		"cmd rm -f "+linkPath,
		"writedirect "+attrPath+"=1",
	)
	if node.files[attrPath] != "1" {
		t.Fatalf("attr_allow_any_host is %q, want 1", node.files[attrPath])
	}
	if _, ok := node.links[linkPath]; ok {
		t.Fatalf("the retired host link survived")
	}
}

// ---------------------------------------------------------------------------
// Probe / converge agreement (CN19, CN23, CN28)
//
// The two channels answer the same question about the same stored request, so
// a row they disagree on makes the worker's err_epoch and its proto.Equal
// suppression flap on a state that is not changing.
// ---------------------------------------------------------------------------

// TestDisabledCntlrProbeReportsTheSuppressedRows: at SP_LEVEL_DISABLE the
// converge reports every cntlr-scoped resource RES_STATUS_MISSING /
// "sp_level" rather than leaving it out, because "a worker cannot tell an
// omitted resource from one the agent never looked at" (CN19) and because
// show_info promises the complete current CntlrInfo (§9.7). A probe that
// returned early skipped seven row families the converge fills.
func TestDisabledCntlrProbeReportsTheSuppressedRows(t *testing.T) {
	srv, _ := newTestServer(t)
	xfers := []*pb.Transfer{{
		XferId:   testXfer,
		OriNqn:   testNqn,
		OriNsIdx: 1,
	}}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, xfers: xfers})

	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, xfers: xfers,
		level: pb.SpLevel_SP_LEVEL_DISABLE}))
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	converged := reply.GetCntlrInfo()
	for label, res := range map[string]*pb.ResInfo{
		"td dm-error": converged.GetTdIdToDmError()[testTd],
		"ns-dev":      converged.GetNsIdToDmLinear()[testNs],
		"namespace":   converged.GetNsIdToNamespace()[testNs],
		"subsystem":   converged.GetSsIdToSubsystem()[testSs],
		"xfer dm":     converged.GetXferIdToDmLinear()[testXfer],
		"xfer subsys": converged.GetXferIdToSubsystem()[testXfer],
		"xfer ns":     converged.GetXferIdToNamespace()[testXfer],
	} {
		if res.GetStatus() != pb.ResStatus_RES_STATUS_MISSING ||
			res.GetDetails() != "sp_level" {
			t.Fatalf("converged %s is %v/%q, want MISSING/sp_level",
				label, res.GetStatus(), res.GetDetails())
		}
	}

	probe, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	// Identical, epochs included: nothing changed between the two passes, so
	// a row that moves is a row the two paths disagree about.
	if !proto.Equal(converged, probe.GetCntlrInfo()) {
		t.Fatalf("the probe disagrees with the converge at SP_LEVEL_DISABLE:\n"+
			"converge:\n%v\nprobe:\n%v", converged, probe.GetCntlrInfo())
	}
}

// TestCloneWithUnresolvableDstTdAgrees: a clone whose dst_td_id is not in
// td_list is the CN18 error the converge reports on all three rows. The probe
// used to fall through to the live-source / dm-clone / wrapper probes and
// answer MISSING (or OK for a stack that outlived the td), and — with
// region_cnt = 0 — invent a one-unit metadata budget and report a size
// mismatch that describes nothing on the node. Every interleaved converge and
// check round then flipped the rows and bumped their epochs.
func TestCloneWithUnresolvableDstTdAgrees(t *testing.T) {
	srv, _ := newTestServer(t)
	// Phase 1: the clone is built over its own destination td, so its metadata
	// wrapper really exists when the td leaves td_list.
	clone := cloneOfTd(testClone, testSrcNqn, testSnapTd)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: twoTds(),
		clones: []*pb.Clone{clone}})

	// Phase 2: the destination td is dropped while the clone stays.
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{clone}}))
	if err != nil {
		t.Fatalf("drop the destination td: %v", err)
	}
	details := fmt.Sprintf("destination td %d not found", testSnapTd)
	converged := reply.GetCntlrInfo()
	assertCloneRows(t, converged, "converged", details)

	probe, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	assertCloneRows(t, probe.GetCntlrInfo(), "probed", details)
	// No flap: the epoch of a row whose status never changed must not move.
	if got, want := probe.GetCntlrInfo().GetCloneIdToMeta()[testClone].
		GetEpoch(), converged.GetCloneIdToMeta()[testClone].GetEpoch(); got !=
		want {
		t.Fatalf("clone_id_to_meta epoch moved %d -> %d", want, got)
	}
}

func assertCloneRows(
	t *testing.T,
	info *pb.CntlrInfo,
	label string,
	details string,
) {
	t.Helper()
	for row, res := range map[string]*pb.ResInfo{
		"clone_id_to_target":   info.GetCloneIdToTarget()[testClone],
		"clone_id_to_dm_clone": info.GetCloneIdToDmClone()[testClone],
		"clone_id_to_meta":     info.GetCloneIdToMeta()[testClone],
	} {
		if res.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
			res.GetDetails() != details {
			t.Fatalf("%s %s is %v/%q, want ERROR/%q",
				label, row, res.GetStatus(), res.GetDetails(), details)
		}
	}
}
