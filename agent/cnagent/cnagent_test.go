package cnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The fixture mirrors the case-S shape of cnagent_integtest.md §5/§6: one
// slice, one meta group and one data group, one td, one subsystem with one
// namespace. Tests that need raid1, snapshots, clones or transfers extend it.
const (
	testCluster    = uint64(0x1)
	testCn         = uint64(0x11)
	testCn2        = uint64(0x12)
	testSp         = uint64(0x3a1)
	testCntlr      = uint64(0x1)
	testSlice      = uint64(0x2)
	testMetaGrp    = uint64(0x3)
	testMetaLeg    = uint64(0x4)
	testMetaSide   = uint64(0x5)
	testDataGrp    = uint64(0x6)
	testDataLeg    = uint64(0x7)
	testDataSide   = uint64(0x8)
	testDataLeg2   = uint64(0x17)
	testDataSide2  = uint64(0x18)
	testDataGrp2   = uint64(0x26)
	testSpareLeg   = uint64(0x27)
	testSpareSide  = uint64(0x28)
	testDataGrp3   = uint64(0x36)
	testDataLeg3   = uint64(0x37)
	testDataSide3  = uint64(0x38)
	testSlice2     = uint64(0x40)
	testMetaGrpB   = uint64(0x43)
	testMetaLegB   = uint64(0x44)
	testMetaSideB  = uint64(0x45)
	testDataGrpB   = uint64(0x46)
	testDataLegB   = uint64(0x47)
	testDataSideB  = uint64(0x48)
	testTd         = uint64(0x9)
	testSs         = uint64(0xa)
	testNs         = uint64(0xb)
	testXfer       = uint64(0xc)
	testClone      = uint64(0xd)
	testClone2     = uint64(0x1d)
	testClone3     = uint64(0x2d)
	testSnapTd     = uint64(0xe)
	testSnapSs     = uint64(0xf)
	testSnapNs     = uint64(0x10)
	testCapacity   = uint64(1099511627776)
	testBlockSize  = uint64(1 << 20)
	testStripeSize = uint64(65536)
	testTdSize     = uint64(64 << 20)

	testNqn     = "nqn.2024-01.io.dnv-it:s:vol1"
	testSnapNqn = "nqn.2024-01.io.dnv-it:s:snap1"
	testHostNqn = "nqn.2024-01.io.dnv-it:host:0"
	testUuid    = "11111111-1111-4111-8111-111111111111"
	testNguid   = "11111111111141118111111111111111"

	testIp     = "192.168.10.12"
	testIp2    = "192.168.10.13"
	testSvcId  = "4200"
	testSvcId2 = "4201"
)

func testTrConf() *pb.NvmeTrConf {
	return &pb.NvmeTrConf{
		TrType:  "tcp",
		AdrFam:  "ipv4",
		TrAddr:  testIp,
		TrSvcId: testSvcId,
	}
}

func sideTrConf(addr, svcId string) *pb.NvmeTrConf {
	return &pb.NvmeTrConf{
		TrType:  "tcp",
		AdrFam:  "ipv4",
		TrAddr:  addr,
		TrSvcId: svcId,
	}
}

// newCnServer builds a server over one fake node. Every construction site in
// the suite goes through it, because it is also what keeps the CN11 probers
// off the real syscalls: Reconcile starts them for a stored primary and the
// loop fires on its own ticker, so a server left with the production
// directLegProbeIO would eventually open /dev/mapper/dnv-… for real
// (update_01.md U2).
func newCnServer(node *fakeNode) *CnAgentServer {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	srv := NewCnAgentServer(node.osClient(), nf,
		common.DefaultLocalStorPrefix, testCapacity, testTrConf())
	srv.probeIO = node.probeIO()
	return srv
}

// reconcileForTest runs the SH1 startup pass rooted at the *test's* context,
// which is what stops the background goroutines it starts. Reconcile publishes
// its ctx as the server's rootCtx (syncup_cn.go), and every CN11 prober and
// CN10/CN18 connect retry hangs off that — so a server rooted at
// context.Background() leaks a 5 s-ticker prober per leg past the end of the
// test that made it, and those goroutines then race the next test's fields
// (update_01.md U2 "stopped gracefully when the agent exits"; contract R2.1 —
// cancel, do not join). t.Context() is cancelled when the test returns, before
// its cleanups run.
func reconcileForTest(t *testing.T, srv *CnAgentServer) {
	t.Helper()
	if err := srv.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func newTestServer(t *testing.T) (*CnAgentServer, *fakeNode) {
	t.Helper()
	node := newFakeNode()
	node.dirs[agent.NvmetRoot] = true
	srv := newCnServer(node)
	reconcileForTest(t, srv)
	node.Reset()
	return srv, node
}

func cntlrPtr() *pb.CntlrPointer {
	return &pb.CntlrPointer{SpId: testSp, CntlrId: testCntlr}
}

func cnReq(revision uint64, withCntlr bool) *pb.SyncupCnRequest {
	req := &pb.SyncupCnRequest{
		ClusterId: testCluster,
		CnId:      testCn,
		Revision:  revision,
	}
	if withCntlr {
		req.CntlrPointerList = []*pb.CntlrPointer{cntlrPtr()}
	}
	return req
}

// legOf builds one leg with the given sides.
func legOf(legId uint64, sides ...*pb.Side) *pb.Leg {
	return &pb.Leg{LegId: legId, LegIdx: 0, SideList: sides}
}

// sideOf builds a side in the steady state every pre-U4 test means: its
// §9.4 zeroing finished and the worker flipped the gate, so the DN exports it
// and the CN converges the whole stack over it. Without the explicit flag
// proto3's default would make every existing fixture provisioning-deferred.
func sideOf(sideId uint64, addr, svcId string) *pb.Side {
	return &pb.Side{
		SideId:      sideId,
		AddrPort:    addr + ":29528",
		CntlidSlot:  0,
		NvmeTrConf:  sideTrConf(addr, svcId),
		Provisioned: true,
	}
}

// unprovisionedSideOf is sideOf's U4 twin: a side whose DN is still zeroing
// it, so nothing is exported for it yet.
func unprovisionedSideOf(sideId uint64, addr, svcId string) *pb.Side {
	side := sideOf(sideId, addr, svcId)
	side.Provisioned = false
	return side
}

type reqOpts struct {
	revision uint64
	primary  bool
	disabled bool
	level    pb.SpLevel
	raid1    bool
	// suspended is the stored Namespace.suspended flag.
	suspended bool
	tds       []*pb.ThinDevice
	subsys    map[string]*pb.Subsystem
	clones    []*pb.Clone
	xfers     []*pb.Transfer
	// extraSide adds a second side to every leg (a migrating leg, [D1]).
	extraSide bool
	// twoLegs gives the data group a second leg, so the §11.1.1 assembly
	// cases that need a real mirror have one.
	twoLegs bool
	// unprovisionedDataLeg makes every side of the data group's leg
	// provisioned = false, which defers the group and — since it is the
	// slice's only data group — the whole slice (U4).
	unprovisionedDataLeg bool
	// unprovisionedSpare gives the data group a spare leg whose sides are
	// unprovisioned. A spare defers only itself.
	unprovisionedSpare bool
	// unprovisionedExtraSide makes only the *second* side of each leg
	// unprovisioned — the mid-migration shape, where the leg keeps serving
	// through its provisioned side.
	unprovisionedExtraSide bool
	// twoSlices gives the SP a second slice, so a td's raid0 really stripes
	// across two thin volumes and a snapshot has more than one instant to be
	// torn between (update_02.md U1).
	twoSlices bool
}

func defaultTds() []*pb.ThinDevice {
	return []*pb.ThinDevice{{TdId: testTd, DevId: 1, Size: testTdSize}}
}

func defaultSubsys(suspended bool) map[string]*pb.Subsystem {
	return map[string]*pb.Subsystem{
		testNqn: {
			SsId:   testSs,
			Serial: fmt.Sprintf(common.IdKeyFmt, testSs),
			Model:  "dnv",
			NsList: []*pb.Namespace{{
				NsId:      testNs,
				NsIdx:     1,
				TdId:      testTd,
				DevUuid:   testUuid,
				DevNguid:  testNguid,
				Suspended: suspended,
			}},
		},
	}
}

// subsysForTd is defaultSubsys with the namespace pointed at another td, so a
// fixture whose td_list omits testTd — a snapshot whose origin the gateway
// has let go (ThinDeviceCreated.md U2-S2) — still describes a coherent cntlr.
func subsysForTd(tdId uint64) map[string]*pb.Subsystem {
	subsys := defaultSubsys(false)
	subsys[testNqn].NsList[0].TdId = tdId
	return subsys
}

// sliceOf is one slice's meta/data group pair. The fixture's first slice and
// the second one twoSlices adds are the same shape with different ids.
func sliceOf(
	sliceIdx uint32,
	metaGrpId uint64,
	metaLegs []*pb.Leg,
	dataGrpId uint64,
	dataLegs []*pb.Leg,
	dataSpares []*pb.Leg,
	metaBlocks uint64,
	dataBlocksMeta uint64,
	dataBlocksData uint64,
) *pb.Slice {
	return &pb.Slice{
		SliceIdx: sliceIdx,
		MetaGrpList: []*pb.Group{{
			GrpId:      metaGrpId,
			ExtCnt:     1,
			MetaBlocks: metaBlocks,
			DataBlocks: dataBlocksMeta,
			LegList:    metaLegs,
		}},
		DataGrpList: []*pb.Group{{
			GrpId:        dataGrpId,
			ExtCnt:       2,
			MetaBlocks:   metaBlocks,
			DataBlocks:   dataBlocksData,
			LegList:      dataLegs,
			SpareLegList: dataSpares,
		}},
	}
}

func cntlrReq(o reqOpts) *pb.SyncupCntlrRequest {
	redund := &pb.RedundConf{
		RedunKind: &pb.RedundConf_RedundNone{RedundNone: &pb.RedundNone{}},
	}
	metaBlocks, dataBlocksMeta, dataBlocksData := uint64(1), uint64(63),
		uint64(127)
	if o.raid1 {
		redund = &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{BitmapChunkBlockCnt: 128},
			},
		}
		metaBlocks, dataBlocksMeta, dataBlocksData = 3, 61, 125
	}
	metaSides := []*pb.Side{sideOf(testMetaSide, testIp, testSvcId)}
	dataSides := []*pb.Side{sideOf(testDataSide, testIp, testSvcId)}
	if o.unprovisionedDataLeg {
		dataSides = []*pb.Side{
			unprovisionedSideOf(testDataSide, testIp, testSvcId)}
	}
	if o.extraSide {
		metaSecond := sideOf(testMetaSide+0x100, testIp2, testSvcId2)
		dataSecond := sideOf(testDataSide+0x100, testIp2, testSvcId2)
		if o.unprovisionedExtraSide {
			metaSecond.Provisioned = false
			dataSecond.Provisioned = false
		}
		metaSides = append(metaSides, metaSecond)
		dataSides = append(dataSides, dataSecond)
	}
	dataLegs := []*pb.Leg{legOf(testDataLeg, dataSides...)}
	if o.twoLegs {
		second := legOf(testDataLeg2,
			sideOf(testDataSide2, testIp2, testSvcId2))
		second.LegIdx = 1
		dataLegs = append(dataLegs, second)
	}
	var dataSpares []*pb.Leg
	if o.unprovisionedSpare {
		spare := legOf(testSpareLeg,
			unprovisionedSideOf(testSpareSide, testIp2, testSvcId2))
		spare.LegIdx = 1
		dataSpares = []*pb.Leg{spare}
	}
	tds := o.tds
	if tds == nil {
		tds = defaultTds()
	}
	subsys := o.subsys
	if subsys == nil {
		subsys = defaultSubsys(o.suspended)
	}
	// Group and leg ids are cntlr-wide keys and a leg id generates the leg's
	// NQN, so the second slice needs fresh ones; slice_idx must stay dense.
	idToSlice := map[string]*pb.Slice{
		fmt.Sprintf(common.IdKeyFmt, testSlice): sliceOf(0,
			testMetaGrp, []*pb.Leg{legOf(testMetaLeg, metaSides...)},
			testDataGrp, dataLegs, dataSpares,
			metaBlocks, dataBlocksMeta, dataBlocksData),
	}
	if o.twoSlices {
		idToSlice[fmt.Sprintf(common.IdKeyFmt, testSlice2)] = sliceOf(1,
			testMetaGrpB, []*pb.Leg{legOf(testMetaLegB,
				sideOf(testMetaSideB, testIp2, testSvcId2))},
			testDataGrpB, []*pb.Leg{legOf(testDataLegB,
				sideOf(testDataSideB, testIp2, testSvcId2))},
			nil, metaBlocks, dataBlocksMeta, dataBlocksData)
	}
	return &pb.SyncupCntlrRequest{
		ClusterId:    testCluster,
		CnId:         testCn,
		CntlrPointer: cntlrPtr(),
		Revision:     o.revision,
		BdevConf: &pb.BdevConf{
			DmPoolConf: &pb.DmPoolConf{
				DataBlockSize:   testBlockSize,
				LowWaterMarkPct: 50,
			},
			DmRaid0Conf: &pb.DmRaid0Conf{StripeSize: testStripeSize},
			RedundConf:  redund,
		},
		SpLevel: o.level,
		Cntlr: &pb.Cntlr{
			AddrPort:   testIp + ":29529",
			NvmeTrConf: testTrConf(),
			CntlidSlot: 0,
			Primary:    o.primary,
			Disabled:   o.disabled,
		},
		IdToSlice:      idToSlice,
		TdList:         tds,
		NqnToSubsystem: subsys,
		CloneList:      o.clones,
		XferList:       o.xfers,
	}
}

// cnSyncup drives one SyncupCn: withCntlr false is the CN21 teardown of the
// fixture's only cntlr, true re-introduces its pointer.
func cnSyncup(
	t *testing.T,
	srv *CnAgentServer,
	revision uint64,
	withCntlr bool,
) {
	t.Helper()
	reply, err := srv.SyncupCn(context.Background(), cnReq(revision,
		withCntlr))
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCn rejected: %v", reply.GetAgentReply())
	}
}

// syncupBoth brings the CN base state up and converges one cntlr.
func syncupBoth(
	t *testing.T,
	srv *CnAgentServer,
	o reqOpts,
) *pb.SyncupCntlrReply {
	t.Helper()
	ctx := context.Background()
	cnReply, err := srv.SyncupCn(ctx, cnReq(o.revision, true))
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if cnReply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCn rejected: %v", cnReply.GetAgentReply())
	}
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(o))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCntlr rejected: %v", reply.GetAgentReply())
	}
	return reply
}

// ---------------------------------------------------------------------------
// Assertions
// ---------------------------------------------------------------------------

// assertOrder checks that the recorded calls contain the given fragments as a
// subsequence.
func assertOrder(t *testing.T, node *fakeNode, fragments ...string) {
	t.Helper()
	from := 0
	for i, fragment := range fragments {
		idx := node.indexOfCallFrom(fragment, from)
		if idx < 0 {
			t.Fatalf("call %q not found after %q (position %d)\ncalls:\n%s",
				fragment, previous(fragments, i), from,
				strings.Join(node.Calls(), "\n"))
		}
		from = idx + 1
	}
}

func previous(fragments []string, i int) string {
	if i == 0 {
		return "(start)"
	}
	return fragments[i-1]
}

func assertNoCall(t *testing.T, node *fakeNode, fragment string) {
	t.Helper()
	if node.hasCall(fragment) {
		t.Fatalf("unexpected call %q\ncalls:\n%s",
			fragment, strings.Join(node.Calls(), "\n"))
	}
}

// assertSysfsDeadlines is the update_02.md U2 regression guard: no read of
// the CN10 sysfs leg walk may reach the OsClient on a deadline-less ctx
// (SH15). The per-attribute guard keeps it from going vacuous if a later
// fixture change stops exercising one of the five reads.
func assertSysfsDeadlines(t *testing.T, node *fakeNode) {
	t.Helper()
	// Scoped to the walk's own reads: an unscoped "/ana_state" match would
	// also be satisfied by nvmet's configfs `ana_groups/{id}/ana_state`
	// writes, which are a different path family entirely.
	reads := node.callsMatching("read /sys/class/nvme")
	for _, suffix := range []string{
		"/subsysnqn", "/nsid", "/address", "/state", "/ana_state"} {
		found := false
		for _, call := range reads {
			if strings.HasSuffix(call, suffix) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("no sysfs read of %s: the deadline check is vacuous",
				suffix)
		}
	}
	if paths := node.SysfsNoDeadline(); len(paths) != 0 {
		t.Errorf("sysfs reads without an SH15 deadline: %v", paths)
	}
}

func assertOk(t *testing.T, info *pb.ResInfo, label string) {
	t.Helper()
	if info.GetStatus() != pb.ResStatus_RES_STATUS_OK {
		t.Fatalf("%s: status %v, details %q",
			label, info.GetStatus(), info.GetDetails())
	}
}

// assertErrorDetails is the row a failed converge leaves: RES_STATUS_ERROR
// whose details carry the node's own output, so an operator reads what the
// kernel said and not a paraphrase (§9.5).
func assertErrorDetails(
	t *testing.T,
	info *pb.ResInfo,
	want string,
	label string,
) {
	t.Helper()
	if info.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(info.GetDetails(), want) {
		t.Fatalf("%s: status %v, details %q, want ERROR containing %q",
			label, info.GetStatus(), info.GetDetails(), want)
	}
}

// assertOnlyPersisted is SH16: an equal-revision re-apply of a fully built
// cntlr issues no mutating command, and persisting the request is the one
// write SH5 mandates.
func assertOnlyPersisted(t *testing.T, node *fakeNode) {
	t.Helper()
	for _, call := range node.Mutations() {
		if strings.HasPrefix(call, "writeproto ") {
			continue
		}
		t.Fatalf("an equal-revision re-apply mutated: %q", call)
	}
}

// assertProvisioning is assertOk's U4 twin: the resource is deliberately not
// created yet and the row must say so with the fixed details string — never
// with a progress counter, which would re-send CntlrInfo on every check round.
func assertProvisioning(t *testing.T, info *pb.ResInfo, label string) {
	t.Helper()
	if info.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING {
		t.Fatalf("%s: status %v, details %q, want PROVISIONING",
			label, info.GetStatus(), info.GetDetails())
	}
	if info.GetDetails() != "provisioning" {
		t.Fatalf("%s: details %q, want %q", label, info.GetDetails(),
			"provisioning")
	}
}

// assertMissingSpLevel is the CN19 row: the operator's level says the resource
// must not exist, which is a stronger statement than U4's "it is coming" — so
// a resource that is both level-suppressed and provisioning-deferred reports
// MISSING/"sp_level", never PROVISIONING.
func assertMissingSpLevel(t *testing.T, info *pb.ResInfo, label string) {
	t.Helper()
	if info.GetStatus() != pb.ResStatus_RES_STATUS_MISSING ||
		info.GetDetails() != detailsSpLevel {
		t.Fatalf("%s: status %v, details %q, want MISSING/%q",
			label, info.GetStatus(), info.GetDetails(), detailsSpLevel)
	}
}

func names(srv *CnAgentServer) *common.NameFmt { return srv.nf }

func legName(srv *CnAgentServer, legId uint64) string {
	return names(srv).CnLegName(testCluster, testCn, testSp, legId)
}

func legNqn(srv *CnAgentServer, legId uint64) string {
	return names(srv).SideToCnNqn(testCluster, testSp, legId, testCn)
}

func grpName(srv *CnAgentServer, grpId uint64) string {
	return names(srv).CnGrpName(testCluster, testCn, testSp, grpId)
}

func poolName(srv *CnAgentServer) string {
	return poolNameOf(srv, testSlice)
}

func poolNameOf(srv *CnAgentServer, sliceId uint64) string {
	return names(srv).CnPoolFinalName(testCluster, testCn, testSp, sliceId)
}

func thinName(srv *CnAgentServer, tdId uint64) string {
	return thinNameOf(srv, tdId, testSlice)
}

func thinNameOf(srv *CnAgentServer, tdId, sliceId uint64) string {
	return names(srv).CnThinDevName(
		testCluster, testCn, testSp, tdId, sliceId)
}

func raid0Name(srv *CnAgentServer, tdId uint64) string {
	return names(srv).CnRaid0Name(testCluster, testCn, testSp, tdId)
}

func errorName(srv *CnAgentServer, tdId uint64) string {
	return names(srv).CnErrorName(testCluster, testCn, testSp, tdId)
}

func nsDevName(srv *CnAgentServer, nsId uint64) string {
	return names(srv).CnNsDevName(testCluster, testCn, testSp, nsId)
}

func cloneName(srv *CnAgentServer, cloneId uint64) string {
	return names(srv).CnCloneFinalName(
		testCluster, testCn, testSp, cloneId)
}

// cloneMetaName is the kind-`b` wrapper over one clone's slot in the
// clone-metadata arena ([D14]).
func cloneMetaName(srv *CnAgentServer, cloneId uint64) string {
	return names(srv).CnCloneMetaDmName(
		testCluster, testCn, testSp, cloneId)
}

// loopDev is the single loop device the CN5 base state attached to the arena
// file; the allocator re-learns it from `losetup --associated` every pass, so
// the tests read it the same way rather than assuming /dev/loop0.
func loopDev(t *testing.T, srv *CnAgentServer, node *fakeNode) string {
	t.Helper()
	devs := node.loops[srv.nf.CnTmpFilePath(testCluster, testCn)]
	if len(devs) != 1 {
		t.Fatalf("%d loop devices back the arena file, want 1", len(devs))
	}
	return devs[0]
}

func xferName(srv *CnAgentServer, xferId uint64) string {
	return names(srv).CnXferFinalName(testCluster, testCn, testSp, xferId)
}

func anaPath(nqn string, nsIdx int) string {
	return fmt.Sprintf("%s/subsystems/%s/namespaces/%d/ana_grpid",
		agent.NvmetRoot, nqn, nsIdx)
}

// ---------------------------------------------------------------------------
// §6.1 — fresh SyncupCn
// ---------------------------------------------------------------------------

func TestFreshSyncupCn(t *testing.T) {
	srv, node := newTestServer(t)
	reply, err := srv.SyncupCn(context.Background(), cnReq(1, false))
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	tmpfs := srv.nf.CnTmpfsPath(testCluster, testCn)
	file := srv.nf.CnTmpFilePath(testCluster, testCn)

	assertOrder(t, node,
		"cmd findmnt", "cmd mkdir -p "+tmpfs, "cmd mount -t tmpfs",
		"cmd stat --format %s "+file,
		"cmd truncate --size 1073741824 "+file,
		"cmd losetup --associated "+file,
		"cmd losetup --find --show "+file,
		"writedirect "+agent.NvmetRoot+"/ports/1/addr_trtype",
		"cmd mkdir -p "+agent.NvmetRoot+"/ports/1/ana_groups/2",
		"cmd mkdir -p "+agent.NvmetRoot+"/ports/1/ana_groups/3",
		"writedirect "+agent.NvmetRoot+"/ports/1/ana_groups/1/ana_state",
		"writedirect "+agent.NvmetRoot+"/ports/1/ana_groups/3/ana_state",
		"writeproto "+srv.nf.LocalCnPath(testCluster, testCn),
	)
	// [D14]: LVM is gone from the CN too — CN5 stops at the loop device and
	// the arena is carved by the slot allocator. This is the unit-test form
	// of the repo-wide "no LVM command anywhere" acceptance grep.
	for _, verb := range []string{
		"cmd pvcreate", "cmd vgcreate", "cmd vgs", "cmd lvcreate",
		"cmd lvremove", "cmd lvs", "cmd lvchange", "cmd pvs",
	} {
		assertNoCall(t, node, verb)
	}

	// CN6: QoS is accepted and programmed nowhere.
	for _, call := range node.Calls() {
		if strings.Contains(call, "io.max") ||
			strings.Contains(call, "cgroup") {
			t.Fatalf("QoS enforcement leaked into the converge: %q", call)
		}
	}

	assertOk(t, reply.GetCnInfo().GetTmpfsInfo(), "tmpfs")
	assertOk(t, reply.GetCnInfo().GetTmpFileInfo(), "tmp_file")
	assertOk(t, reply.GetCnInfo().GetLoopDevInfo(), "loop_dev")
	assertOk(t, reply.GetCnInfo().GetPortInfo(), "port")
}

// TestFailedLosetupNeverAttachesASecondLoop: `losetup --associated` failing is
// not the same fact as "no loop is associated". Treating the two alike lets
// a single timed-out probe attach a *second* loop to the arena file, and from
// then on every pass reports "2 loop devices …, want 1": no clone metadata
// can be allocated or probed on the CN until an operator runs `losetup -d`
// (update_01.md U3 Decision: a **single** loop device, re-learned every
// converge; §8 rejects loop sprawl outright).
func TestFailedLosetupNeverAttachesASecondLoop(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	file := srv.nf.CnTmpFilePath(testCluster, testCn)

	node.Reset()
	node.failCmd["losetup --associated"] =
		"losetup: cannot open /dev/loop-control"
	reply, err := srv.SyncupCn(context.Background(), cnReq(3, true))
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	loopInfo := reply.GetCnInfo().GetLoopDevInfo()
	if loopInfo.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("loop_dev_info is %v/%q, want ERROR",
			loopInfo.GetStatus(), loopInfo.GetDetails())
	}
	assertNoCall(t, node, "cmd losetup --find")
	if devs := node.loops[file]; len(devs) != 1 {
		t.Fatalf("%d loop devices back the arena file, want 1: %v",
			len(devs), devs)
	}
}

// ---------------------------------------------------------------------------
// §6.2 — the revision gate
// ---------------------------------------------------------------------------

func TestRevisionGate(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	node.Reset()
	reply, err := srv.SyncupCn(ctx, cnReq(1, true))
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != common.ReplyCodeStaleRevision {
		t.Fatalf("stale SyncupCn: code %d, want %d",
			reply.GetAgentReply().GetCode(), common.ReplyCodeStaleRevision)
	}
	if reply.GetRevision() != 2 {
		t.Fatalf("stale reply revision %d, want 2", reply.GetRevision())
	}
	if mutations := node.Mutations(); len(mutations) != 0 {
		t.Fatalf("a stale SyncupCn mutated: %v", mutations)
	}

	node.Reset()
	cntlrReply, err := srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 1, primary: true}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if cntlrReply.GetAgentReply().GetCode() !=
		common.ReplyCodeStaleRevision {
		t.Fatalf("stale SyncupCntlr: code %d",
			cntlrReply.GetAgentReply().GetCode())
	}
	if mutations := node.Mutations(); len(mutations) != 0 {
		t.Fatalf("a stale SyncupCntlr mutated: %v", mutations)
	}
}

func TestSyncupCntlrUnknownPointer(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()
	// SyncupCn without the pointer: the cntlr is unknown (CN8).
	if _, err := srv.SyncupCn(ctx, cnReq(1, false)); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	reply, err := srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 2, primary: true}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if reply.GetAgentReply().GetCode() != common.ReplyCodeUnknownObject {
		t.Fatalf("code %d, want %d", reply.GetAgentReply().GetCode(),
			common.ReplyCodeUnknownObject)
	}
}

// ---------------------------------------------------------------------------
// §6.3 — probe-first idempotency (SH16)
// ---------------------------------------------------------------------------

func TestIdempotentReapply(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 2, primary: true}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	for _, call := range node.Mutations() {
		// Persisting the request is the one write SH5 mandates.
		if strings.HasPrefix(call, "writeproto ") {
			continue
		}
		t.Fatalf("equal-revision re-apply mutated: %q\nall:\n%s",
			call, strings.Join(node.Mutations(), "\n"))
	}
}

// ---------------------------------------------------------------------------
// §6.5 — the primary converge order (CN9)
// ---------------------------------------------------------------------------

func TestPrimaryConvergeOrder(t *testing.T) {
	srv, node := newTestServer(t)
	reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	assertOrder(t, node,
		"cmd nvme connect",
		"cmd dmsetup create "+legName(srv, testMetaLeg),
		"cmd dmsetup create "+legName(srv, testDataLeg),
		"cmd dmsetup create "+grpName(srv, testMetaGrp),
		"cmd dmsetup create "+grpName(srv, testDataGrp),
		"cmd dmsetup create "+srv.nf.CnPoolMetaName(
			testCluster, testCn, testSp, testSlice),
		"cmd dmsetup create "+srv.nf.CnPoolDataName(
			testCluster, testCn, testSp, testSlice),
		"cmd dmsetup create "+poolName(srv),
		"cmd dmsetup message "+poolName(srv)+" 0 create_thin 1",
		"cmd dmsetup create "+thinName(srv, testTd),
		"cmd dmsetup create "+raid0Name(srv, testTd),
		"cmd dmsetup create "+nsDevName(srv, testNs),
		"cmd mkdir -p "+agent.NvmetRoot+"/subsystems/"+testNqn,
		"writedirect "+anaPath(testNqn, 1)+"=1",
	)
	// The ANA promotion is the last mutating step of the pass.
	last := node.indexOfCall("writedirect " + anaPath(testNqn, 1) + "=1")
	for i, call := range node.Calls() {
		if i <= last {
			continue
		}
		if strings.HasPrefix(call, "writeproto ") {
			continue
		}
		if isProbe(call) {
			continue
		}
		t.Fatalf("mutation after the ANA promotion: %q", call)
	}

	info := reply.GetCntlrInfo()
	assertOk(t, info.GetLegIdToLeg()[testMetaLeg], "leg meta")
	assertOk(t, info.GetLegIdToLeg()[testDataLeg], "leg data")
	assertOk(t, info.GetGrpIdToMdRaid()[testMetaGrp], "grp meta")
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	assertOk(t, info.GetSliceIdToMeta()[testSlice], "pool meta")
	assertOk(t, info.GetSliceIdToData()[testSlice], "pool data")
	assertOk(t, info.GetSliceIdToDmPool()[testSlice], "pool")
	assertOk(t, info.GetTdIdToRaid0()[testTd], "raid0")
	assertOk(t, info.GetTdIdToDmError()[testTd], "dm-error")
	assertOk(t,
		info.GetTdIdToThinInfo()[testTd].GetSliceIdToDmThin()[testSlice],
		"thin")
	assertOk(t, info.GetNsIdToDmLinear()[testNs], "ns-dev")
	assertOk(t, info.GetNsIdToNamespace()[testNs], "namespace")
	assertOk(t, info.GetSsIdToSubsystem()[testSs], "subsystem")

	// §9.5/CN28: the pool details are the raw dmsetup status line, which the
	// §10.4 auto-grow parses two used/total pairs out of.
	details := info.GetSliceIdToDmPool()[testSlice].GetDetails()
	if !strings.Contains(details, "thin-pool") ||
		strings.Count(details, "/") < 2 {
		t.Fatalf("pool details %q is not a raw status line", details)
	}
}

func isProbe(call string) bool {
	for _, prefix := range readOnlyPrefixes {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// §6.4 — the standby shape (§3.4)
// ---------------------------------------------------------------------------

func TestStandbyConverge(t *testing.T) {
	srv, node := newTestServer(t)
	reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: false})

	assertOrder(t, node,
		"cmd nvme connect",
		"cmd dmsetup create "+legName(srv, testMetaLeg),
		"cmd dmsetup create "+errorName(srv, testTd),
		"cmd dmsetup create "+nsDevName(srv, testNs),
		"cmd mkdir -p "+agent.NvmetRoot+"/subsystems/"+testNqn,
	)
	for _, forbidden := range []string{
		"cmd mdadm", "cmd dmsetup create " + poolName(srv),
		"cmd dmsetup create " + thinName(srv, testTd),
		"cmd dmsetup create " + raid0Name(srv, testTd),
		"create_thin",
	} {
		assertNoCall(t, node, forbidden)
	}
	// Every namespace sits in the inaccessible group and nothing was ever
	// promoted.
	assertNoCall(t, node, "writedirect "+anaPath(testNqn, 1)+"=1")

	info := reply.GetCntlrInfo()
	assertOk(t, info.GetNsIdToDmLinear()[testNs], "ns-dev")
	if _, ok := info.GetGrpIdToMdRaid()[testMetaGrp]; ok {
		t.Fatalf("a standby reported a group device")
	}
	// The ns-dev's table is the td's dm-error.
	table := node.dms[nsDevName(srv, testNs)].table
	errNo := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
	if !strings.Contains(table, "linear "+errNo) {
		t.Fatalf("standby ns-dev table %q is not on the dm-error", table)
	}
}

// ---------------------------------------------------------------------------
// §6.6 — failover (§11.1)
// ---------------------------------------------------------------------------

func TestFailoverRetireOrder(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, raid1: true})

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 3, primary: false, raid1: true}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	mdDev := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	assertOrder(t, node,
		"writedirect "+anaPath(testNqn, 1)+"=3",
		"cmd dmsetup reload "+nsDevName(srv, testNs),
		"cmd dmsetup remove "+raid0Name(srv, testTd),
		"cmd dmsetup remove "+thinName(srv, testTd),
		"cmd dmsetup remove "+poolName(srv),
		"cmd mdadm --stop "+mdDev,
	)
	// Standbys keep their legs: no leg NQN is disconnected.
	assertNoCall(t, node, "cmd nvme disconnect")
}

func TestFailoverBackToPrimaryAssembles(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, raid1: true})
	if _, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 3, primary: false, raid1: true})); err !=
		nil {
		t.Fatalf("demote: %v", err)
	}

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 4, primary: true, raid1: true}))
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	if !node.hasCall("cmd mdadm --assemble") {
		t.Fatalf("re-promotion did not assemble:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	assertNoCall(t, node, "cmd mdadm --create")
	assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testMetaGrp],
		"grp meta")
}

// ---------------------------------------------------------------------------
// §6.7 — §11.1.1 assembly cases and member reconciliation (CN12)
// ---------------------------------------------------------------------------

func TestGroupCreateAssumeClean(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, raid1: true})
	created := node.callsMatching("cmd mdadm --create")
	if len(created) != 2 {
		t.Fatalf("want 2 --create calls, got %d:\n%s",
			len(created), strings.Join(created, "\n"))
	}
	for _, call := range created {
		for _, want := range []string{
			"--assume-clean", "--bitmap internal", "--failfast",
			"--homehost any", "--run", "--level 1",
		} {
			if !strings.Contains(call, want) {
				t.Fatalf("--create is missing %q: %s", want, call)
			}
		}
	}
	assertNoCall(t, node, "--zero-superblock")
}

func TestGroupAssembleRefusalIsAnError(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, raid1: true})
	if _, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 3, primary: false, raid1: true})); err !=
		nil {
		t.Fatalf("demote: %v", err)
	}
	node.Reset()
	node.failCmdAlways["mdadm --assemble"] = "mdadm: refusing degraded start"
	reply, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 4, primary: true, raid1: true}))
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	info := reply.GetCntlrInfo().GetGrpIdToMdRaid()[testMetaGrp]
	if info.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("a refused assembly left status %v", info.GetStatus())
	}
}

// §11.1.1 case 1.2: exactly one member of a two-leg group carries a
// superblock — assemble with that one, then add the other.
func TestGroupAssembleThenAddMissingMember(t *testing.T) {
	srv, node := newTestServer(t)
	node.superblocks[srv.nf.DmPath(legName(srv, testDataLeg))] = true

	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, raid1: true, twoLegs: true})
	mdDev := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	for _, call := range node.callsMatching("cmd mdadm --create") {
		if strings.Contains(call, mdDev) {
			t.Fatalf("the data group was created, not assembled: %s", call)
		}
	}
	assertOrder(t, node,
		"cmd mdadm --assemble "+mdDev,
		"--add --failfast "+srv.nf.DmPath(legName(srv, testDataLeg2)),
	)
	if got := len(node.arrays[mdDev].members); got != 2 {
		t.Fatalf("the array has %d members, want 2", got)
	}
}

// §11.1.1 case 1.3: both members carry a superblock but mdadm leaves one out
// for stale metadata — the converge re-adds it, never zero-superblocks it.
func TestGroupReaddsLeftOutMember(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, raid1: true, twoLegs: true})
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: false, raid1: true, twoLegs: true})); err !=
		nil {
		t.Fatalf("demote: %v", err)
	}
	node.assembleDrop[srv.nf.DmPath(legName(srv, testDataLeg2))] = true

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 4, primary: true, raid1: true, twoLegs: true})); err !=
		nil {
		t.Fatalf("promote: %v", err)
	}
	assertOrder(t, node,
		"cmd mdadm --assemble",
		"--add --failfast "+srv.nf.DmPath(legName(srv, testDataLeg2)),
	)
	assertNoCall(t, node, "--zero-superblock")
	assertNoCall(t, node, "cmd mdadm --create")
}

// TestSwitchSpareLeg is the CN12 member reconciliation: a leg_list swapped
// with a spare is failed, removed and the promoted spare added — never
// zero-superblocked.
func TestSwitchSpareLeg(t *testing.T) {
	srv, node := newTestServer(t)
	spareLeg := uint64(0x77)
	spareSide := uint64(0x78)

	withSpare := func(revision uint64, promoted bool) *pb.SyncupCntlrRequest {
		req := cntlrReq(reqOpts{
			revision: revision, primary: true, raid1: true})
		slice := req.GetIdToSlice()[fmt.Sprintf(common.IdKeyFmt, testSlice)]
		grp := slice.GetDataGrpList()[0]
		member := legOf(testDataLeg, sideOf(testDataSide, testIp, testSvcId))
		spare := legOf(spareLeg, sideOf(spareSide, testIp2, testSvcId2))
		if promoted {
			grp.LegList = []*pb.Leg{spare}
			grp.SpareLegList = []*pb.Leg{member}
		} else {
			grp.LegList = []*pb.Leg{member}
			grp.SpareLegList = []*pb.Leg{spare}
		}
		return req
	}

	ctx := context.Background()
	if _, err := srv.SyncupCn(ctx, cnReq(2, true)); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if _, err := srv.SyncupCntlr(ctx, withSpare(2, false)); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	// A spare is connected, wrapped and probed but never an md member.
	if !node.hasCall("cmd dmsetup create " + legName(srv, spareLeg)) {
		t.Fatalf("the spare leg was not wrapped")
	}
	for _, call := range node.callsMatching("cmd mdadm --create") {
		if strings.Contains(call, legName(srv, spareLeg)) {
			t.Fatalf("a spare leg became an md member: %s", call)
		}
	}

	node.Reset()
	if _, err := srv.SyncupCntlr(ctx, withSpare(3, true)); err != nil {
		t.Fatalf("SwitchSpareLeg: %v", err)
	}
	assertOrder(t, node,
		"--fail "+srv.nf.DmPath(legName(srv, testDataLeg)),
		"--remove "+srv.nf.DmPath(legName(srv, testDataLeg)),
		"--add --failfast "+srv.nf.DmPath(legName(srv, spareLeg)),
	)
	assertNoCall(t, node, "--zero-superblock")
}

// ---------------------------------------------------------------------------
// §6.8 — namespace states (CN16)
// ---------------------------------------------------------------------------

func TestNamespaceSuspend(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, suspended: true})); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	assertOrder(t, node,
		"writedirect "+anaPath(testNqn, 1)+"=3",
		"cmd dmsetup suspend "+nsDevName(srv, testNs),
	)

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 4, primary: true})); err != nil {
		t.Fatalf("resume: %v", err)
	}
	assertOrder(t, node,
		"cmd dmsetup resume "+nsDevName(srv, testNs),
		"writedirect "+anaPath(testNqn, 1)+"=1",
	)
}

func TestTransferAutoSuspendRetiresOrigin(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	node.Reset()
	xfers := []*pb.Transfer{{
		XferId:       testXfer,
		OriNqn:       testNqn,
		OriNsIdx:     1,
		AllowedHosts: []string{testHostNqn},
		AutoSuspend:  true,
	}}
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, xfers: xfers}))
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	// CN9 fixes the build order (transfers before ns-devs), so the xfer
	// device is created between the origin's ANA retirement and its suspend;
	// what §11.3 makes load-bearing — ANA inaccessible *before* the suspend,
	// so no host IO is queued behind it — holds either way.
	assertOrder(t, node,
		"writedirect "+anaPath(testNqn, 1)+"=3",
		"cmd dmsetup create "+xferName(srv, testXfer),
		"cmd dmsetup suspend "+nsDevName(srv, testNs),
	)
	info := reply.GetCntlrInfo()
	assertOk(t, info.GetXferIdToDmLinear()[testXfer], "xfer dm")
	assertOk(t, info.GetXferIdToSubsystem()[testXfer], "xfer subsystem")
	assertOk(t, info.GetXferIdToNamespace()[testXfer], "xfer namespace")

	xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
	if got := node.files[agent.NvmetRoot+"/subsystems/"+xferNqn+
		"/attr_serial"]; got != fmt.Sprintf(common.IdKeyFmt, testXfer) {
		t.Fatalf("xfer attr_serial is %q", got)
	}
	// The xfer namespace carries the origin's identity, on the primary in
	// the optimized group.
	if got := node.files[anaPath(xferNqn, 1)]; got != "1" {
		t.Fatalf("xfer ana_grpid is %q, want 1", got)
	}
}

func TestReadOnlyLevelUsesFlakey(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true,
		level: pb.SpLevel_SP_LEVEL_READONLY})); err != nil {
		t.Fatalf("readonly: %v", err)
	}
	table := node.dms[nsDevName(srv, testNs)].table
	raid0No := node.devNo["/dev/mapper/"+raid0Name(srv, testTd)]
	want := fmt.Sprintf("flakey %s 0 0 1 1 error_writes", raid0No)
	if !strings.Contains(table, want) {
		t.Fatalf("ns-dev table is %q, want it to contain %q", table, want)
	}
	// Nothing else changes at this level.
	assertNoCall(t, node, "cmd dmsetup remove")
	assertNoCall(t, node, "cmd mdadm --stop")

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 4, primary: true})); err != nil {
		t.Fatalf("back to readwrite: %v", err)
	}
	table = node.dms[nsDevName(srv, testNs)].table
	if strings.Contains(table, "flakey") {
		t.Fatalf("ns-dev is still flakey: %q", table)
	}
}

func TestUpdateNamespaceDevIsOneReload(t *testing.T) {
	srv, node := newTestServer(t)
	tds := []*pb.ThinDevice{
		{TdId: testTd, DevId: 1, Size: testTdSize},
		{TdId: testSnapTd, DevId: 2, Size: testTdSize},
	}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: tds})

	node.Reset()
	subsys := defaultSubsys(false)
	subsys[testNqn].NsList[0].TdId = testSnapTd
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, tds: tds, subsys: subsys})); err != nil {
		t.Fatalf("repoint: %v", err)
	}
	reloads := node.callsMatching(
		"cmd dmsetup reload " + nsDevName(srv, testNs))
	if len(reloads) != 1 {
		t.Fatalf("want exactly one ns-dev reload, got %d:\n%s",
			len(reloads), strings.Join(reloads, "\n"))
	}
	// The nvmet device_path never changes.
	assertNoCall(t, node, "writedirect "+agent.NvmetRoot+"/subsystems/"+
		testNqn+"/namespaces/1/device_path")
}

// ---------------------------------------------------------------------------
// §6.11 — the sp_level ladder (CN19)
// ---------------------------------------------------------------------------

func TestSpLevelLadder(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	revision := uint64(2)
	resync := func(level pb.SpLevel) *pb.SyncupCntlrReply {
		t.Helper()
		revision++
		node.Reset()
		reply, err := srv.SyncupCntlr(context.Background(),
			cntlrReq(reqOpts{
				revision: revision, primary: true, level: level}))
		if err != nil {
			t.Fatalf("level %v: %v", level, err)
		}
		if reply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("level %v rejected: %v", level, reply.GetAgentReply())
		}
		return reply
	}

	reply := resync(pb.SpLevel_SP_LEVEL_NO_THINPOOL)
	info := reply.GetCntlrInfo()
	if got := info.GetSliceIdToDmPool()[testSlice]; got.GetStatus() !=
		pb.ResStatus_RES_STATUS_MISSING || got.GetDetails() != "sp_level" {
		t.Fatalf("pool at NO_THINPOOL: %v/%q", got.GetStatus(),
			got.GetDetails())
	}
	// Namespaces stay exported on error backing — a host sees IO errors,
	// not a vanished device.
	assertOk(t, info.GetNsIdToNamespace()[testNs], "namespace")
	table := node.dms[nsDevName(srv, testNs)].table
	errNo := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
	if !strings.Contains(table, "linear "+errNo) {
		t.Fatalf("ns-dev is not on the dm-error: %q", table)
	}

	beforeRedund := len(node.dms)
	resync(pb.SpLevel_SP_LEVEL_NO_REDUND)
	if _, ok := node.dms[grpName(srv, testMetaGrp)]; ok {
		t.Fatalf("a group device survived NO_REDUND")
	}
	if _, ok := node.dms[legName(srv, testMetaLeg)]; !ok {
		t.Fatalf("legs must stay connected and wrapped at NO_REDUND")
	}
	_ = beforeRedund

	// NO_MIGRATION has no CN-side behavior relative to NO_REDUND.
	node.Reset()
	resync(pb.SpLevel_SP_LEVEL_NO_MIGRATION)
	for _, call := range node.Mutations() {
		if strings.HasPrefix(call, "writeproto ") {
			continue
		}
		t.Fatalf("NO_MIGRATION mutated relative to NO_REDUND: %q", call)
	}

	resync(pb.SpLevel_SP_LEVEL_NO_SIDE)
	if _, ok := node.dms[legName(srv, testMetaLeg)]; ok {
		t.Fatalf("a leg wrapper survived NO_SIDE")
	}
	if !node.hasCall("cmd nvme disconnect") {
		t.Fatalf("NO_SIDE did not disconnect the legs")
	}
	if _, ok := node.dms[nsDevName(srv, testNs)]; !ok {
		t.Fatalf("host-facing ns-devs must remain at NO_SIDE")
	}

	resync(pb.SpLevel_SP_LEVEL_DISABLE)
	for name := range node.dms {
		t.Fatalf("dm device %s survived SP_LEVEL_DISABLE", name)
	}
	// The store files stay — desired state persists.
	path := srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr)
	if _, ok := node.protos[path]; !ok {
		t.Fatalf("SP_LEVEL_DISABLE deleted the cntlr state file")
	}

	// Lowering rebuilds.
	node.Reset()
	revision++
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: revision, primary: true})); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if _, ok := node.dms[raid0Name(srv, testTd)]; !ok {
		t.Fatalf("lowering the level did not rebuild the raid0")
	}
}

// ---------------------------------------------------------------------------
// Teardown (CN7/CN21)
// ---------------------------------------------------------------------------

func TestDeclarativeCntlrTeardown(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	node.Reset()
	reply, err := srv.SyncupCn(context.Background(), cnReq(3, false))
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	// The top-down chain. The leg rows are a subsequence over two legs, so
	// they cannot pin the per-leg order — the CN21 flip is asserted on the
	// meta leg's own two calls below.
	assertOrder(t, node,
		"cmd dmsetup reload "+nsDevName(srv, testNs),
		"cmd rmdir "+agent.NvmetRoot+"/subsystems/"+testNqn,
		"cmd dmsetup remove "+nsDevName(srv, testNs),
		"cmd dmsetup remove "+raid0Name(srv, testTd),
		"cmd dmsetup remove "+thinName(srv, testTd),
		"cmd dmsetup remove "+poolName(srv),
		"cmd dmsetup remove "+legName(srv, testMetaLeg),
		"cmd rm -f "+srv.nf.LocalCntlrPath(
			testCluster, testCn, testSp, testCntlr),
	)
	// CN21 (update_01.md U2 spec 4): one leg disconnects *before* its wrapper
	// is removed — a probe wedged on a pathless leg holds an open fd on the
	// wrapper, and only the disconnect errors its queued IO.
	metaNqn := srv.nf.SideToCnNqn(testCluster, testSp, testMetaLeg, testCn)
	disconnected := node.indexOfCall("cmd nvme disconnect --nqn " + metaNqn)
	unwrapped := node.indexOfCall(
		"cmd dmsetup remove " + legName(srv, testMetaLeg))
	if disconnected < 0 || unwrapped < 0 || disconnected > unwrapped {
		t.Fatalf("CN21 order: disconnect at %d, wrapper removal at %d\n%s",
			disconnected, unwrapped, strings.Join(node.Calls(), "\n"))
	}
	// CN14: a teardown deactivates, it never deletes thin device ids.
	assertNoCall(t, node, "0 delete ")
	for name := range node.dms {
		t.Fatalf("dm device %s survived the teardown", name)
	}
	// The base state outlives every cntlr: the tmpfs mount and its single
	// loop device stay, and the loop above already showed that every dm
	// device — the kind-`b` wrappers included — is gone.
	if got := node.mounts[srv.nf.CnTmpfsPath(testCluster, testCn)]; got !=
		"tmpfs" {
		t.Fatalf("the teardown unmounted the tmpfs (%q)", got)
	}
	if devs := node.loops[srv.nf.CnTmpFilePath(
		testCluster, testCn)]; len(devs) != 1 {
		t.Fatalf("the teardown detached the arena loop device: %v", devs)
	}
}

// ---------------------------------------------------------------------------
// §6.12 — check streams (CN24)
// ---------------------------------------------------------------------------

type fakeCheckCnStream struct {
	pb.ControllerNodeAgent_CheckCnServer
	ctx      context.Context
	requests []*pb.CheckCnRequest
	replies  []*pb.CheckCnReply
}

func (s *fakeCheckCnStream) Context() context.Context { return s.ctx }

func (s *fakeCheckCnStream) Recv() (*pb.CheckCnRequest, error) {
	if len(s.requests) == 0 {
		return nil, fmt.Errorf("EOF")
	}
	req := s.requests[0]
	s.requests = s.requests[1:]
	return req, nil
}

func (s *fakeCheckCnStream) Send(reply *pb.CheckCnReply) error {
	s.replies = append(s.replies, reply)
	return nil
}

func TestCheckCnRounds(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	node.Reset()
	lastSent := (*pb.CnInfo)(nil)
	reply, info := srv.checkCnRound(context.Background(),
		&pb.CheckCnRequest{ClusterId: testCluster, CnId: testCn,
			Revision: 2}, lastSent)
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("round 1 rejected: %v", reply.GetAgentReply())
	}
	if reply.GetCnInfo() == nil {
		t.Fatalf("the first reply of a stream must carry the info")
	}
	if reply.GetRevision() != 2 {
		t.Fatalf("revision %d, want 2", reply.GetRevision())
	}
	lastSent = info

	reply, _ = srv.checkCnRound(context.Background(),
		&pb.CheckCnRequest{ClusterId: testCluster, CnId: testCn,
			Revision: 2}, lastSent)
	if reply.GetCnInfo() != nil {
		t.Fatalf("an unchanged round must omit the info")
	}
	for _, call := range node.Mutations() {
		t.Fatalf("a check round mutated: %q", call)
	}

	// An unknown CN keeps the stream open with code 2.
	reply, _ = srv.checkCnRound(context.Background(),
		&pb.CheckCnRequest{ClusterId: testCluster, CnId: 0x99}, nil)
	if reply.GetAgentReply().GetCode() != common.ReplyCodeUnknownObject {
		t.Fatalf("unknown cn: code %d", reply.GetAgentReply().GetCode())
	}
}

func TestCheckCntlrRounds(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	node.Reset()
	req := &pb.CheckCntlrRequest{
		ClusterId: testCluster, CnId: testCn,
		CntlrPointer: cntlrPtr(), Revision: 2,
	}
	reply, info := srv.checkCntlrRound(context.Background(), req, nil)
	if reply.GetCntlrInfo() == nil {
		t.Fatalf("the first reply must carry the info")
	}
	reply2, _ := srv.checkCntlrRound(context.Background(), req, info)
	if reply2.GetCntlrInfo() != nil {
		t.Fatalf("an unchanged round must omit the info")
	}
	// show_info always fills it.
	req.ShowInfo = true
	reply3, _ := srv.checkCntlrRound(context.Background(), req, info)
	if reply3.GetCntlrInfo() == nil {
		t.Fatalf("show_info must fill the info")
	}
	for _, call := range node.Mutations() {
		t.Fatalf("a check round mutated: %q", call)
	}
	// A steady-state check round reports every resource OK. "No mutation" is
	// not enough on its own: a probe that misreads a healthy resource — say by
	// comparing a configfs identity attribute byte-wise (agent.SameNsId) —
	// writes nothing and still fails the CP's converge check.
	assertAllOk(t, reply.GetCntlrInfo())
}

// assertAllOk walks every ResInfo a CntlrInfo carries, the way the
// integration suite's `check-cntlr` assertion does.
func assertAllOk(t *testing.T, info *pb.CntlrInfo) {
	t.Helper()
	for id, res := range info.GetLegIdToLeg() {
		assertOk(t, res, fmt.Sprintf("leg %d", id))
	}
	for id, res := range info.GetGrpIdToMdRaid() {
		assertOk(t, res, fmt.Sprintf("md raid %d", id))
	}
	for id, res := range info.GetSliceIdToMeta() {
		assertOk(t, res, fmt.Sprintf("slice meta %d", id))
	}
	for id, res := range info.GetSliceIdToData() {
		assertOk(t, res, fmt.Sprintf("slice data %d", id))
	}
	for id, res := range info.GetSliceIdToDmPool() {
		assertOk(t, res, fmt.Sprintf("slice pool %d", id))
	}
	for id, res := range info.GetTdIdToRaid0() {
		assertOk(t, res, fmt.Sprintf("td raid0 %d", id))
	}
	for id, res := range info.GetTdIdToDmError() {
		assertOk(t, res, fmt.Sprintf("td dm-error %d", id))
	}
	for tdId, thin := range info.GetTdIdToThinInfo() {
		for sliceId, res := range thin.GetSliceIdToDmThin() {
			assertOk(t, res, fmt.Sprintf("thin %d/%d", tdId, sliceId))
		}
	}
	for id, res := range info.GetSsIdToSubsystem() {
		assertOk(t, res, fmt.Sprintf("subsystem %d", id))
	}
	for id, res := range info.GetNsIdToDmLinear() {
		assertOk(t, res, fmt.Sprintf("ns dm-linear %d", id))
	}
	for id, res := range info.GetNsIdToNamespace() {
		assertOk(t, res, fmt.Sprintf("namespace %d", id))
	}
}

// ---------------------------------------------------------------------------
// GetCnSize / GetCnInfo / GetCntlrInfo
// ---------------------------------------------------------------------------

func TestGetCnSize(t *testing.T) {
	srv, node := newTestServer(t)
	reply, err := srv.GetCnSize(context.Background(),
		&pb.GetCnSizeRequest{ClusterId: testCluster, CnId: 0})
	if err != nil {
		t.Fatalf("GetCnSize: %v", err)
	}
	if reply.GetSize() != testCapacity {
		t.Fatalf("size %d, want %d", reply.GetSize(), testCapacity)
	}
	// CN3: no locks, no store, no OS access.
	if calls := node.Calls(); len(calls) != 0 {
		t.Fatalf("GetCnSize touched the OS: %v", calls)
	}
}

func TestGetInfoUnknownObject(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()
	cnReply, err := srv.GetCnInfo(ctx,
		&pb.GetCnInfoRequest{ClusterId: testCluster, CnId: testCn})
	if err != nil {
		t.Fatalf("GetCnInfo: %v", err)
	}
	if cnReply.GetAgentReply().GetCode() != common.ReplyCodeUnknownObject ||
		cnReply.GetRevision() != 0 {
		t.Fatalf("unknown cn: %v rev %d",
			cnReply.GetAgentReply(), cnReply.GetRevision())
	}
	cntlrReply, err := srv.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	if cntlrReply.GetAgentReply().GetCode() !=
		common.ReplyCodeUnknownObject {
		t.Fatalf("unknown cntlr: %v", cntlrReply.GetAgentReply())
	}
}

// ---------------------------------------------------------------------------
// §6.17 — U4 provisioning deferral (update_01.md U4, [D15])
//
// A side that has not finished its §9.4 zeroing exports nothing, so every
// resource stacked on it is left out of the *effective* desired state: it is
// not built, and it reports RES_STATUS_PROVISIONING rather than an error, so a
// freshly created SP never feeds err_epoch or the §10.4 replacement flows.
// ---------------------------------------------------------------------------

// grownDataGrp appends a second data group to the fixture's only slice — the
// §8.5 GrowSlice shape. provisioned says whether the DN has already finished
// zeroing the new group's side.
func grownDataGrp(req *pb.SyncupCntlrRequest, provisioned bool) {
	appendDataGrp(req, testDataGrp2, testDataLeg2, testDataSide2, provisioned)
}

// grownDataGrp3 appends a *third* data group — a second GrowSlice on top of
// the second one, which is what makes the out-of-order clearing of U4's
// deferral observable.
func grownDataGrp3(req *pb.SyncupCntlrRequest, provisioned bool) {
	appendDataGrp(req, testDataGrp3, testDataLeg3, testDataSide3, provisioned)
}

func appendDataGrp(
	req *pb.SyncupCntlrRequest,
	grpId uint64,
	legId uint64,
	sideId uint64,
	provisioned bool,
) {
	slice := req.GetIdToSlice()[fmt.Sprintf(common.IdKeyFmt, testSlice)]
	side := sideOf(sideId, testIp2, testSvcId2)
	if !provisioned {
		side = unprovisionedSideOf(sideId, testIp2, testSvcId2)
	}
	first := slice.GetDataGrpList()[0]
	slice.DataGrpList = append(slice.DataGrpList, &pb.Group{
		GrpId:      grpId,
		ExtCnt:     first.GetExtCnt(),
		MetaBlocks: first.GetMetaBlocks(),
		DataBlocks: first.GetDataBlocks(),
		LegList:    []*pb.Leg{legOf(legId, side)},
	})
}

func TestProvisioningDeferredGroup(t *testing.T) {
	srv, node := newTestServer(t)
	reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		raid1: true, unprovisionedDataLeg: true})

	dataMd := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	metaMd := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, true))
	// Nothing at all is built over the provisioning leg: no connect, no
	// wrapper, no array — and no concat that would have to grow into it.
	assertNoCall(t, node, "--nqn "+legNqn(srv, testDataLeg))
	assertNoCall(t, node, "cmd dmsetup create "+legName(srv, testDataLeg))
	for _, call := range node.callsMatching("cmd mdadm") {
		if strings.Contains(call, dataMd) {
			t.Fatalf("a deferred group reached mdadm: %s", call)
		}
	}
	for _, name := range []string{
		srv.nf.CnPoolMetaName(testCluster, testCn, testSp, testSlice),
		srv.nf.CnPoolDataName(testCluster, testCn, testSp, testSlice),
		poolName(srv),
		thinName(srv, testTd),
		raid0Name(srv, testTd),
	} {
		assertNoCall(t, node, "cmd dmsetup create "+name)
	}
	// The meta group is untouched by the data group's deferral: its own leg
	// is provisioned, so it connects and assembles as usual.
	if !node.hasCall("cmd mdadm --create " + metaMd) {
		t.Fatalf("the meta group did not assemble:\n%s",
			strings.Join(node.Calls(), "\n"))
	}

	info := reply.GetCntlrInfo()
	assertProvisioning(t, info.GetLegIdToLeg()[testDataLeg], "leg data")
	assertProvisioning(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	assertProvisioning(t, info.GetSliceIdToMeta()[testSlice], "pool meta")
	assertProvisioning(t, info.GetSliceIdToData()[testSlice], "pool data")
	assertProvisioning(t, info.GetSliceIdToDmPool()[testSlice], "pool")
	assertProvisioning(t,
		info.GetTdIdToThinInfo()[testTd].GetSliceIdToDmThin()[testSlice],
		"thin")
	assertProvisioning(t, info.GetTdIdToRaid0()[testTd], "raid0")
	assertOk(t, info.GetLegIdToLeg()[testMetaLeg], "leg meta")
	assertOk(t, info.GetGrpIdToMdRaid()[testMetaGrp], "grp meta")
	// The td's dm-error and the host-facing subsystem are real resources and
	// stay OK — PROVISIONING marks only what is deferred.
	assertOk(t, info.GetTdIdToDmError()[testTd], "dm-error")
	assertOk(t, info.GetSsIdToSubsystem()[testSs], "subsystem")
	// The initial CreateStoragePool converges with no error anywhere, which
	// is what keeps err_epoch and the failover flapping out of it.
	for label, res := range map[string]*pb.ResInfo{
		"leg data": info.GetLegIdToLeg()[testDataLeg],
		"grp data": info.GetGrpIdToMdRaid()[testDataGrp],
		"pool":     info.GetSliceIdToDmPool()[testSlice],
		"raid0":    info.GetTdIdToRaid0()[testTd],
		"ns-dev":   info.GetNsIdToDmLinear()[testNs],
	} {
		if res.GetStatus() == pb.ResStatus_RES_STATUS_ERROR {
			t.Fatalf("%s reported ERROR: %q", label, res.GetDetails())
		}
	}
	// The read-only probe reports exactly the same shape (CN28).
	probe, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	probed := probe.GetCntlrInfo()
	assertProvisioning(t, probed.GetLegIdToLeg()[testDataLeg], "probed leg")
	assertProvisioning(t, probed.GetGrpIdToMdRaid()[testDataGrp], "probed grp")
	assertProvisioning(t, probed.GetSliceIdToDmPool()[testSlice], "probed pool")
	assertProvisioning(t, probed.GetTdIdToRaid0()[testTd], "probed raid0")
}

// TestServingPoolStaysOkDuringDeferredGrow is the §10.4 guard: a GrowSlice
// whose new group is still provisioning must leave the *serving* pool alone —
// OK at its effective (old) size, with the raw `dmsetup status` details the
// auto-grow parses. A PROVISIONING pool row would switch auto-grow off.
func TestServingPoolStaysOkDuringDeferredGrow(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	poolDataName := srv.nf.CnPoolDataName(
		testCluster, testCn, testSp, testSlice)
	oneGrpSectors := uint64(127) * testBlockSize / agent.SectorSize

	node.Reset()
	grow := cntlrReq(reqOpts{revision: 3, primary: true})
	grownDataGrp(grow, false)
	reply, err := srv.SyncupCntlr(context.Background(), grow)
	if err != nil {
		t.Fatalf("deferred grow: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	// The new group is not a concat target yet, so nothing reloads.
	assertNoCall(t, node, "cmd dmsetup reload "+poolDataName)
	assertNoCall(t, node, "cmd dmsetup reload "+poolName(srv))
	assertNoCall(t, node, "--nqn "+legNqn(srv, testDataLeg2))
	if got := len(strings.Split(node.dms[poolDataName].table, "\n")); got != 1 {
		t.Fatalf("the pool-data concat has %d targets, want 1:\n%s",
			got, node.dms[poolDataName].table)
	}
	wantPool := fmt.Sprintf("0 %d thin-pool", oneGrpSectors)
	if !strings.HasPrefix(node.dms[poolName(srv)].table, wantPool) {
		t.Fatalf("pool table %q, want it to start with %q",
			node.dms[poolName(srv)].table, wantPool)
	}

	info := reply.GetCntlrInfo()
	assertOk(t, info.GetSliceIdToMeta()[testSlice], "pool meta")
	assertOk(t, info.GetSliceIdToData()[testSlice], "pool data")
	assertOk(t, info.GetSliceIdToDmPool()[testSlice], "pool")
	assertProvisioning(t, info.GetGrpIdToMdRaid()[testDataGrp2], "grp grown")
	assertProvisioning(t, info.GetLegIdToLeg()[testDataLeg2], "leg grown")
	details := info.GetSliceIdToDmPool()[testSlice].GetDetails()
	if !strings.Contains(details, "thin-pool") ||
		strings.Count(details, "/") < 2 {
		t.Fatalf("pool details %q is not a raw status line", details)
	}

	// The worker flips the new side; the grow then completes on the very next
	// converge — the concat gains its target and the pool reloads onto it.
	node.Reset()
	flipped := cntlrReq(reqOpts{revision: 4, primary: true})
	grownDataGrp(flipped, true)
	reply, err = srv.SyncupCntlr(context.Background(), flipped)
	if err != nil {
		t.Fatalf("grow after the flip: %v", err)
	}
	if got := len(strings.Split(node.dms[poolDataName].table, "\n")); got != 2 {
		t.Fatalf("the pool-data concat has %d targets, want 2:\n%s",
			got, node.dms[poolDataName].table)
	}
	wantPool = fmt.Sprintf("0 %d thin-pool", 2*oneGrpSectors)
	if !strings.HasPrefix(node.dms[poolName(srv)].table, wantPool) {
		t.Fatalf("grown pool table %q, want it to start with %q",
			node.dms[poolName(srv)].table, wantPool)
	}
	assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp2],
		"grp grown")
	assertOk(t, reply.GetCntlrInfo().GetSliceIdToDmPool()[testSlice], "pool")
}

// concatDevNos reads back the backing device numbers of a dm concat, in table
// order — the one thing a pool concat may never permute, because a dm-thin
// block's physical home is its offset in this list.
func concatDevNos(t *testing.T, node *fakeNode, name string) []string {
	t.Helper()
	dm := node.dms[name]
	if dm == nil {
		t.Fatalf("no dm device %s", name)
	}
	var out []string
	for _, line := range strings.Split(dm.table, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 5 || fields[2] != "linear" {
			t.Fatalf("concat line %q is not a linear target", line)
		}
		out = append(out, fields[3])
	}
	return out
}

// TestOutOfOrderGrowDefersEveryLaterGroup is the U4 prefix rule: deferral cuts
// a group list at the *first* deferred group instead of filtering deferred
// groups out of the middle. Two GrowSlice appends whose sides finish zeroing
// out of order are the reachable trigger — no spare and no worker involved.
//
// An order-preserving filter would build the later group at the earlier one's
// concat offset, so every pool-data block dm-thin allocated in that window
// would physically move the moment the earlier group cleared and the target
// was re-inserted in the middle: silent corruption, reported OK by design.
// update_01.md U4 ("a not-yet-grown concat/pool is OK, not a mismatch; the
// grow completes when the group clears") and architecture.md §8.5 ("the concat
// and the pool keep their old, effective size") both describe a prefix.
func TestOutOfOrderGrowDefersEveryLaterGroup(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	poolDataName := srv.nf.CnPoolDataName(
		testCluster, testCn, testSp, testSlice)
	oneGrpSectors := uint64(127) * testBlockSize / agent.SectorSize
	g0No := node.devNo["/dev/mapper/"+grpName(srv, testDataGrp)]

	node.Reset()
	grow := cntlrReq(reqOpts{revision: 3, primary: true})
	grownDataGrp(grow, false) // the first appended group is still zeroing
	grownDataGrp3(grow, true) // the second one's side already flipped
	reply, err := srv.SyncupCntlr(context.Background(), grow)
	if err != nil {
		t.Fatalf("out-of-order grow: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}

	// The concat keeps its single target: the second appended group may not
	// take the first one's place.
	if got := concatDevNos(t, node, poolDataName); len(got) != 1 ||
		got[0] != g0No {
		t.Fatalf("pool-data concat is %v, want exactly [%s]", got, g0No)
	}
	wantPool := fmt.Sprintf("0 %d thin-pool", oneGrpSectors)
	if !strings.HasPrefix(node.dms[poolName(srv)].table, wantPool) {
		t.Fatalf("pool table %q, want it to start with %q",
			node.dms[poolName(srv)].table, wantPool)
	}
	assertNoCall(t, node, "cmd dmsetup reload "+poolDataName)

	info := reply.GetCntlrInfo()
	assertProvisioning(t, info.GetGrpIdToMdRaid()[testDataGrp2], "grp grown 1")
	// The second appended group's own leg is provisioned, so its group device
	// is built as usual — it simply is not a concat target yet.
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp3], "grp grown 2")
	assertOk(t, info.GetSliceIdToData()[testSlice], "pool data")
	assertOk(t, info.GetSliceIdToDmPool()[testSlice], "pool")

	// CN27: a group that is not a concat target has no pool-data span, so the
	// gateway read is refused rather than answered with another group's region.
	if _, err := srv.GetLegBm(context.Background(), &pb.GetLegBmRequest{
		ClusterId: testCluster, CnId: testCn, SpId: testSp,
		CntlrId: testCntlr, LegId: testDataLeg3,
	}); err == nil {
		t.Fatalf("GetLegBm answered for a group that is not a concat target")
	}

	// The first appended group clears: both grow in, in list order, and no
	// earlier target moves.
	node.Reset()
	flipped := cntlrReq(reqOpts{revision: 4, primary: true})
	grownDataGrp(flipped, true)
	grownDataGrp3(flipped, true)
	if _, err := srv.SyncupCntlr(context.Background(), flipped); err != nil {
		t.Fatalf("grow after the flip: %v", err)
	}
	want := []string{
		g0No,
		node.devNo["/dev/mapper/"+grpName(srv, testDataGrp2)],
		node.devNo["/dev/mapper/"+grpName(srv, testDataGrp3)],
	}
	got := concatDevNos(t, node, poolDataName)
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] ||
		got[2] != want[2] {
		t.Fatalf("pool-data concat is %v, want %v", got, want)
	}
	wantPool = fmt.Sprintf("0 %d thin-pool", 3*oneGrpSectors)
	if !strings.HasPrefix(node.dms[poolName(srv)].table, wantPool) {
		t.Fatalf("grown pool table %q, want it to start with %q",
			node.dms[poolName(srv)].table, wantPool)
	}
}

// TestConcatNeverShrinksWhenALiveGroupDefers is the other direction of the
// same rule: U4's deferral holds a *new* group out of the concat until it is
// ready — it may never take a serving one out. A live group whose leg_list
// gains an all-unprovisioned leg (§8.12's spare switch onto a fresh DN) must
// not shorten the pool-data concat under a live thin-pool: the concat's target
// list is the physical home of every block the pool has already allocated, so
// a shrink either remaps live data or leaves the pool suspended on a refused
// resume. The concat may only grow; anything else is reported, so §10.4 or an
// operator repairs the group.
func TestConcatNeverShrinksWhenALiveGroupDefers(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	poolDataName := srv.nf.CnPoolDataName(
		testCluster, testCn, testSp, testSlice)

	grow := cntlrReq(reqOpts{revision: 3, primary: true})
	grownDataGrp(grow, true)
	if _, err := srv.SyncupCntlr(context.Background(), grow); err != nil {
		t.Fatalf("grow: %v", err)
	}
	served := node.dms[poolDataName].table
	if len(concatDevNos(t, node, poolDataName)) != 2 {
		t.Fatalf("the grow did not land: %q", served)
	}

	// The serving group's leg is replaced by one whose sides have not been
	// zeroed yet.
	node.Reset()
	regressed := cntlrReq(reqOpts{revision: 4, primary: true})
	grownDataGrp(regressed, false)
	reply, err := srv.SyncupCntlr(context.Background(), regressed)
	if err != nil {
		t.Fatalf("regressed group: %v", err)
	}
	assertNoCall(t, node, "cmd dmsetup reload "+poolDataName)
	assertNoCall(t, node, "cmd dmsetup reload "+poolName(srv))
	if got := node.dms[poolDataName].table; got != served {
		t.Fatalf("the live concat was rewritten:\n%q\nwant:\n%q", got, served)
	}
	data := reply.GetCntlrInfo().GetSliceIdToData()[testSlice]
	if data.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(data.GetDetails(), "refusing to shrink") {
		t.Fatalf("slice_id_to_data is %v/%q, want the shrink refusal",
			data.GetStatus(), data.GetDetails())
	}
}

// TestDeferredTdNamespaceIsInaccessible is the CN16 conjunct U4 adds: while
// the backing chain provisions, the ns-dev sits on the td's dm-error and its
// namespace stays in the inaccessible group, so a host queues instead of
// eating IO errors.
func TestDeferredTdNamespaceIsInaccessible(t *testing.T) {
	srv, node := newTestServer(t)
	reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		unprovisionedDataLeg: true})

	assertNoCall(t, node, "writedirect "+anaPath(testNqn, 1)+"=1")
	if got := node.files[anaPath(testNqn, 1)]; got != "3" {
		t.Fatalf("ana_grpid is %q, want 3", got)
	}
	table := node.dms[nsDevName(srv, testNs)].table
	errNo := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
	if !strings.Contains(table, "linear "+errNo) {
		t.Fatalf("deferred ns-dev table %q is not on the td's dm-error", table)
	}
	info := reply.GetCntlrInfo()
	assertProvisioning(t, info.GetNsIdToDmLinear()[testNs], "ns-dev")
	assertProvisioning(t, info.GetNsIdToNamespace()[testNs], "namespace")

	// Once the side provisions, the namespace is promoted with no further
	// help — the effective state simply stops excluding it.
	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 3, primary: true})); err != nil {
		t.Fatalf("after the flip: %v", err)
	}
	if got := node.files[anaPath(testNqn, 1)]; got != "1" {
		t.Fatalf("ana_grpid is %q after the flip, want 1", got)
	}
}

// TestProvisioningCheckRoundIsStable is §6 test 18's check-stream clause: a
// deferred resource's details string is the fixed "provisioning" and carries no
// progress counter, so the second identical round of a CN24 stream suppresses
// the whole CntlrInfo (proto.Equal, SH26) and the round mutates nothing
// (CN23). A counter that advanced with the dn's zeroing would re-send the info
// on every round for a state the worker cannot act on.
func TestProvisioningCheckRoundIsStable(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, unprovisionedDataLeg: true})

	node.Reset()
	req := &pb.CheckCntlrRequest{
		ClusterId: testCluster, CnId: testCn,
		CntlrPointer: cntlrPtr(), Revision: 2,
	}
	reply, info := srv.checkCntlrRound(context.Background(), req, nil)
	if reply.GetCntlrInfo() == nil {
		t.Fatalf("the first reply must carry the info")
	}
	assertProvisioning(t, info.GetSliceIdToDmPool()[testSlice], "pool")
	assertProvisioning(t, info.GetLegIdToLeg()[testDataLeg], "leg data")

	reply2, _ := srv.checkCntlrRound(context.Background(), req, info)
	if reply2.GetCntlrInfo() != nil {
		t.Fatalf("an unchanged provisioning round re-sent the info: %v",
			reply2.GetCntlrInfo())
	}
	for _, call := range node.Mutations() {
		t.Fatalf("a check round of a provisioning cntlr mutated: %q", call)
	}
}

// TestSpLevelBeatsProvisioning is §6 test 18's precedence clause: a resource
// that is both level-suppressed and provisioning-deferred reports MISSING /
// "sp_level" and not PROVISIONING — the operator said it must not exist, which
// outranks "it is coming" (CN19, ruling R4.28). Both channels say so.
func TestSpLevelBeatsProvisioning(t *testing.T) {
	srv, _ := newTestServer(t)
	reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		unprovisionedDataLeg: true, level: pb.SpLevel_SP_LEVEL_DISABLE})

	info := reply.GetCntlrInfo()
	assertMissingSpLevel(t, info.GetNsIdToDmLinear()[testNs], "ns-dev")
	assertMissingSpLevel(t, info.GetNsIdToNamespace()[testNs], "namespace")
	assertMissingSpLevel(t, info.GetLegIdToLeg()[testDataLeg], "leg data")

	probe, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	probed := probe.GetCntlrInfo()
	assertMissingSpLevel(t, probed.GetNsIdToDmLinear()[testNs], "probed ns-dev")
	assertMissingSpLevel(t,
		probed.GetNsIdToNamespace()[testNs], "probed namespace")
	assertMissingSpLevel(t,
		probed.GetLegIdToLeg()[testDataLeg], "probed leg data")
}

// TestUnprovisionedSpareDefersOnlyItself: spares never assemble (§8.12), so an
// unprovisioned one holds nothing back.
func TestUnprovisionedSpareDefersOnlyItself(t *testing.T) {
	srv, node := newTestServer(t)
	reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		raid1: true, unprovisionedSpare: true})

	dataMd := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	if !node.hasCall("cmd mdadm --create " + dataMd) {
		t.Fatalf("the group did not assemble:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	assertNoCall(t, node, "--nqn "+legNqn(srv, testSpareLeg))
	assertNoCall(t, node, "cmd dmsetup create "+legName(srv, testSpareLeg))

	info := reply.GetCntlrInfo()
	assertProvisioning(t, info.GetLegIdToLeg()[testSpareLeg], "spare leg")
	assertOk(t, info.GetLegIdToLeg()[testDataLeg], "member leg")
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	assertOk(t, info.GetSliceIdToData()[testSlice], "pool data")
	assertOk(t, info.GetSliceIdToDmPool()[testSlice], "pool")
	assertOk(t, info.GetNsIdToDmLinear()[testNs], "ns-dev")
}

// TestMixedLegConnectsProvisionedSideOnly is the migration shape: a leg whose
// src side is provisioned and whose dst side is still zeroing keeps serving,
// and only the provisioned side is connected.
func TestMixedLegConnectsProvisionedSideOnly(t *testing.T) {
	srv, node := newTestServer(t)
	reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		extraSide: true, unprovisionedExtraSide: true})

	connects := 0
	for _, call := range node.callsMatching("cmd nvme connect") {
		if !strings.Contains(call, "--nqn "+legNqn(srv, testDataLeg)) {
			continue
		}
		connects++
		if !strings.Contains(call, "--traddr "+testIp+" ") {
			t.Fatalf("connected the unprovisioned side: %s", call)
		}
	}
	if connects != 1 {
		t.Fatalf("%d connects for the data leg, want 1:\n%s",
			connects, strings.Join(node.Calls(), "\n"))
	}
	info := reply.GetCntlrInfo()
	assertOk(t, info.GetLegIdToLeg()[testDataLeg], "leg data")
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	assertOk(t, info.GetSliceIdToDmPool()[testSlice], "pool")

	// A standby reports the same leg healthy, naming the side that has not
	// provisioned yet rather than calling its missing controller a fault.
	demoted, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: false, extraSide: true,
		unprovisionedExtraSide: true}))
	if err != nil {
		t.Fatalf("demote: %v", err)
	}
	leg := demoted.GetCntlrInfo().GetLegIdToLeg()[testDataLeg]
	assertOk(t, leg, "standby leg data")
	want := testIp2 + ":" + testSvcId2 + " provisioning"
	if !strings.Contains(leg.GetDetails(), want) {
		t.Fatalf("standby leg details %q, want it to contain %q",
			leg.GetDetails(), want)
	}
}
