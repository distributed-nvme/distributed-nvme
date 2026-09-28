package cnagent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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
// directLegProbeIO would eventually open /dev/mapper/dnv-… for real.
func newCnServer(node *fakeNode) *CnAgentServer {
	return newCnServerOnPort(node, common.NvmetPortId)
}

// newCnServerOnPort is newCnServer with an explicit --nvmet-port-id. Every
// test but TestSyncupCnOnNonDefaultPort takes the default, which is what
// keeps the recorded call paths at ports/1.
func newCnServerOnPort(node *fakeNode, portId int) *CnAgentServer {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	srv := NewCnAgentServer(node.osClient(), nf,
		common.DefaultLocalStorPrefix, testCapacity, testTrConf(), portId)
	srv.probeIO = node.probeIO()
	// The CN10 pass budget's pauses do not really sleep here: a fixture whose
	// connect fails for good would otherwise spend the whole
	// CnConnectPassBudget of wall time on every pass. The budget still draws
	// each pause at its full length (agent.WaitBudget.Pause), so the pass
	// retries exactly as often as it would on a real clock. The tests that
	// pin the budget itself put the server on a fakeClock (withPassClock).
	srv.sleep = func(ctx context.Context, _ time.Duration) error {
		return ctx.Err()
	}
	return srv
}

// reconcileForTest runs the SH1 startup pass rooted at the *test's* context,
// which is what stops the background goroutines it starts. Reconcile publishes
// its ctx as the server's rootCtx (syncup_cn.go), and every CN11 prober and
// CN10/CN18 connect retry hangs off that — so a server rooted at
// context.Background() leaks a 5 s-ticker prober per leg past the end of the
// test that made it, and those goroutines then race the next test's fields
// (CN11: probers are stopped gracefully when the agent exits —
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
	return newTestServerOnPort(t, common.NvmetPortId)
}

func newTestServerOnPort(
	t *testing.T,
	portId int,
) (*CnAgentServer, *fakeNode) {
	t.Helper()
	node := newFakeNode()
	node.dirs[agent.NvmetRoot] = true
	srv := newCnServerOnPort(node, portId)
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

// sideOf builds a side in the steady state every pre-provisioning-gate test means: its
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

// unprovisionedSideOf is sideOf's [D15] twin: a side whose DN is still zeroing
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
	// slice's only data group — the whole slice ([D15]).
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
	// torn between (CN14's quiesce).
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

// assertSysfsDeadlines is the SH15 regression guard: no read of
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

// assertParked pins the §11.6 park: one ns-dev's table is a plain dm-linear
// over its td's `CnErrorName` and the device is **live**. Both halves are
// load-bearing. An assertion on the words "dmsetup suspend"/"dmsetup resume"
// would prove nothing either way, because `Dm.Reload` is suspend/load/resume
// and issues both on every park; the thing [D12] forbids is the device being
// *left* suspended, which is a final state, not a call.
func assertParked(
	t *testing.T,
	srv *CnAgentServer,
	node *fakeNode,
	nsId uint64,
	tdId uint64,
	label string,
) {
	t.Helper()
	dev := node.dms[nsDevName(srv, nsId)]
	if dev == nil {
		t.Fatalf("%s: the ns-dev does not exist", label)
	}
	if dev.suspended {
		t.Fatalf("%s: the parked ns-dev is dm-suspended ([D12])", label)
	}
	errNo := node.devNo["/dev/mapper/"+errorName(srv, tdId)]
	if errNo == "" {
		t.Fatalf("%s: the td's dm-error is gone", label)
	}
	want := agent.LinearTable(testTdSize/512, errNo, 0)
	if dev.table != want {
		t.Fatalf("%s: the ns-dev table is %q, want the park %q",
			label, dev.table, want)
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

// assertProvisioning is assertOk's [D15] twin: the resource is deliberately not
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

// assertPending is the CN11 row of a primary's leg whose prober has not
// completed a round: RES_STATUS_PENDING "health probe pending". It is what a
// converge reply carries here for a primary's freshly built legs: a prober's
// first round runs one CnLegProbeInterval of real time after it starts, and
// no test waits that long — probeAllLegs runs rounds by hand.
func assertPending(t *testing.T, info *pb.ResInfo, label string) {
	t.Helper()
	if info.GetStatus() != pb.ResStatus_RES_STATUS_PENDING ||
		info.GetDetails() != detailsProbePending {
		t.Fatalf("%s: status %v, details %q, want PENDING/%q",
			label, info.GetStatus(), info.GetDetails(), detailsProbePending)
	}
}

// assertMissingSpLevel is the CN19 row: the operator's level says the resource
// must not exist, which is a stronger statement than [D15]'s "it is coming" — so
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

// portPathOf is the configfs directory of one nvmet port, formatted the way
// agent.Nvmet.PortPath does. Expectations go through it rather than through a
// literal "ports/1" so the default is expressed as common.NvmetPortId and a
// non-default --nvmet-port-id can be asserted with the same strings.
func portPathOf(portId int) string {
	return fmt.Sprintf("%s/ports/%d", agent.NvmetRoot, portId)
}

// mentionsPort reports whether one recorded call names portPath. The path
// has to end where the call's next separator begins, which for this fake is
// one of: end of string, "/" (a child of the port), "=" (a write/writedirect
// of a port attribute) or " " (the next argument). Matching on portPath+"/"
// alone is not enough — it misses the two shapes a half-converted call site
// produces most often, `cmd ls -1 {portPath}` (every dirExists probe, so
// every ProbePort) and `cmd mkdir -p {portPath}` (EnsurePort's own mkdir).
// The separator test is what keeps ports/1 from matching ports/11.
func mentionsPort(call, portPath string) bool {
	rest := call
	for {
		i := strings.Index(rest, portPath)
		if i < 0 {
			return false
		}
		rest = rest[i+len(portPath):]
		if rest == "" || strings.HasPrefix(rest, "/") ||
			strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, " ") {
			return true
		}
	}
}

// assertDefaultPortUntouched is the negative half of
// TestSyncupCnOnNonDefaultPort: an agent on --nvmet-port-id 7 must neither
// create ports/1 nor name it in any call. It reports with t.Errorf and is
// called once per syncup round, because node.Reset() throws the previous
// round's calls away.
func assertDefaultPortUntouched(t *testing.T, node *fakeNode) {
	t.Helper()
	dflt := portPathOf(common.NvmetPortId)
	if node.dirs[dflt] {
		t.Errorf("%s was created by an agent on another port", dflt)
	}
	for _, call := range node.Calls() {
		if mentionsPort(call, dflt) {
			t.Errorf("a call touched the default port: %s", call)
		}
	}
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
		// The cn file carries the pointer list the node-level sweep
		// removes against, so it is persisted BEFORE anything is converged or
		// swept — a crash in the middle is then a startup sweep rather than a
		// rebuild against the old list.
		"writeproto "+srv.nf.LocalCnPath(testCluster, testCn),
		"cmd findmnt", "cmd mkdir -p "+tmpfs, "cmd mount -t tmpfs",
		"cmd stat --format %s "+file,
		"cmd truncate --size 1073741824 "+file,
		"cmd losetup --associated "+file,
		"cmd losetup --find --show "+file,
		"writedirect "+portPathOf(common.NvmetPortId)+"/addr_trtype",
		"cmd mkdir -p "+portPathOf(common.NvmetPortId)+"/ana_groups/2",
		"cmd mkdir -p "+portPathOf(common.NvmetPortId)+"/ana_groups/3",
		"writedirect "+portPathOf(common.NvmetPortId)+
			"/ana_groups/1/ana_state",
		"writedirect "+portPathOf(common.NvmetPortId)+
			"/ana_groups/3/ana_state",
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

// TestSyncupCnOnNonDefaultPort pins CM2's --nvmet-port-id (use_32_slices §5):
// a cn server built with port id 7 creates ports/7, writes its ANA states
// there, links the cntlr's host-facing subsystem and a transfer's subsystem
// into ports/7, reports "7" as port_info's res_name, reads the host-facing
// link back out of ports/7 on the probe path and unlinks from ports/7 on
// teardown — and never touches the default ports/1.
//
// The assertions are on the objects, not only on the recorded strings: both
// port links are read out of the fake's symlink table, before and after the
// teardown. The converge reply's subsystem row is written by
// ensureSubsystem's EnsurePortLink, so the READ path needs an assertion of
// its own: GetCntlrInfo, which is how a test reaches probeExport (the
// CheckCntlr stream of check.go is the only other caller).
func TestSyncupCnOnNonDefaultPort(t *testing.T) {
	const portId = 7
	if portId == common.NvmetPortId {
		t.Fatalf("this test needs a port id other than the default %d",
			common.NvmetPortId)
	}
	srv, node := newTestServerOnPort(t, portId)
	portPath := portPathOf(portId)

	// SyncupCn's own reply names the port. This is ensurePort's res_name,
	// which is a different site from probeCn's below, and syncupBoth throws
	// this reply away — so the call is made here rather than through it.
	cnSyncup, err := srv.SyncupCn(context.Background(), cnReq(2, true))
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	assertOk(t, cnSyncup.GetCnInfo().GetPortInfo(), "SyncupCn port")
	if got := cnSyncup.GetCnInfo().GetPortInfo().GetResName(); got != "7" {
		t.Errorf("SyncupCn port res_name = %q, want \"7\"", got)
	}

	reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	// Ahead of assertOrder, which is fatal: a regression that converges the
	// default port also drops the ports/7 calls assertOrder asks for, so with
	// the two the other way round this guard would never get to report.
	assertDefaultPortUntouched(t, node)

	assertOrder(t, node,
		"writedirect "+portPath+"/addr_trtype",
		"cmd mkdir -p "+portPath+"/ana_groups/2",
		"cmd mkdir -p "+portPath+"/ana_groups/3",
		"writedirect "+portPath+"/ana_groups/1/ana_state",
		"writedirect "+portPath+"/ana_groups/3/ana_state",
		"cmd mkdir -p "+agent.NvmetRoot+"/subsystems/"+testNqn,
	)

	// The subsystem's port link is an object in the fake's symlink table,
	// not just a recorded command.
	assertPortLink(t, node, portPath, testNqn)
	// Written by ensureSubsystem's EnsurePortLink, not by any probe.
	assertOk(t, reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
		"converged subsystem")

	cnReply, err := srv.GetCnInfo(context.Background(),
		&pb.GetCnInfoRequest{ClusterId: testCluster, CnId: testCn})
	if err != nil {
		t.Fatalf("GetCnInfo: %v", err)
	}
	assertOk(t, cnReply.GetCnInfo().GetPortInfo(), "port")
	if got := cnReply.GetCnInfo().GetPortInfo().GetResName(); got != "7" {
		t.Errorf("port res_name = %q, want \"7\"", got)
	}

	// The read path, which the converge above does not exercise: probeExport
	// asks PortLinked about s.port.PortId and reports "not linked to the
	// port" when the link is not under it, so this row is OK only from
	// ports/7.
	probeReply, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	assertOk(t, probeReply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
		"probed subsystem")

	// A transfer's subsystem is linked by ensureXfer, a second call site of
	// the same id.
	node.Reset()
	xferReply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, xfers: []*pb.Transfer{{
			XferId:       testXfer,
			OriNqn:       testNqn,
			OriNsIdx:     1,
			AllowedHosts: []string{testHostNqn},
			AutoSuspend:  true,
		}}}))
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	assertDefaultPortUntouched(t, node)
	assertOk(t, xferReply.GetCntlrInfo().GetXferIdToSubsystem()[testXfer],
		"xfer subsystem")
	xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
	assertPortLink(t, node, portPath, xferNqn)

	// Teardown unlinks from the same port. RemovePortLink asks PortLinked
	// about the id it was given and returns nil when the answer is "no", so
	// an agent that tore down against the default would report success,
	// delete the kernel-global subsystem and leave its ports/7 link behind.
	// Nothing but the symlink table can tell the two apart.
	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 4, primary: true,
		subsys: map[string]*pb.Subsystem{}})); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	assertDefaultPortUntouched(t, node)
	for _, nqn := range []string{testNqn, xferNqn} {
		link := portPath + "/subsystems/" + nqn
		if target, ok := node.links[link]; ok {
			t.Errorf("the port link %s -> %q survived teardown", link, target)
		}
	}
}

// assertPortLink reads one subsystem's port link back out of the fake's
// symlink table.
func assertPortLink(t *testing.T, node *fakeNode, portPath, nqn string) {
	t.Helper()
	link := portPath + "/subsystems/" + nqn
	target, ok := node.links[link]
	if !ok {
		t.Fatalf("no port link at %s; links: %v", link, node.links)
	}
	if want := agent.NvmetRoot + "/subsystems/" + nqn; target != want {
		t.Errorf("link %s -> %q, want %q", link, target, want)
	}
}

// TestFailedLosetupNeverAttachesASecondLoop: `losetup --associated` failing is
// not the same fact as "no loop is associated". Treating the two alike lets
// a single timed-out probe attach a *second* loop to the arena file, and from
// then on every pass reports "2 loop devices …, want 1": no clone metadata
// can be allocated or probed on the CN until an operator runs `losetup -d`
// (CN5: a **single** loop device, re-learned every converge; [D14] rejects
// loop sprawl outright).
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
	assertPending(t, info.GetLegIdToLeg()[testMetaLeg], "leg meta")
	assertPending(t, info.GetLegIdToLeg()[testDataLeg], "leg data")
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
	// Read before the flip: the sweep is about to stop this array, and a
	// stopped array has no sysfs node left to name.
	mdNode := node.mdNode(srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false)))
	if mdNode == "" {
		t.Fatalf("the fixture array has no sysfs node")
	}

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 3, primary: false, raid1: true}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	// The sweep stops the node SYSFS named — /dev/mdN — never the
	// /dev/md/<name> symlink udev may not have made, and never after an
	// `mdadm --detail`, whose member reads block until failfast on a leg
	// whose DN side has gone.
	assertOrder(t, node,
		"writedirect "+anaPath(testNqn, 1)+"=3",
		"cmd dmsetup reload "+nsDevName(srv, testNs),
		"cmd dmsetup remove "+raid0Name(srv, testTd),
		"cmd dmsetup remove "+thinName(srv, testTd),
		"cmd dmsetup remove "+poolName(srv),
		"cmd mdadm --stop "+mdNode,
	)
	assertNoCall(t, node, "cmd mdadm --detail")
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
	// The re-promotion reads its arrays from sysfs (CN12): a `mdadm
	// --detail` opens a member and can block on a dead one.
	assertNoCall(t, node, "cmd mdadm --detail")
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
	assertNoCall(t, node, "cmd mdadm --detail")
}

func TestGroupAssembleRefusalIsAnError(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
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
	assertNoCall(t, node, "cmd mdadm --detail")
	// A group's error is a row, and by itself registers no CN10 retry: with
	// every leg available, no member is late (CN12).
	if retrying(t, srv) {
		t.Fatalf("a refused assembly with every leg available registered " +
			"the retry")
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
	assertNoCall(t, node, "cmd mdadm --detail")
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
	assertNoCall(t, node, "cmd mdadm --detail")
}

// TestGroupFoundByItsHigherLeg is TestGroupReaddsLeftOutMember with the other
// leg left out: the assembly holds only the group's leg_idx 1 leg, which is
// case 2's shape whenever leg 0's side is the dead one. It pins that
// Md.Detail is keyed on the whole leg_list, not its first leg: keyed on leg 0
// alone, the assembled array reads "did not start", and every later pass
// reads it absent and assembles again over members the array holds.
func TestGroupFoundByItsHigherLeg(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	opts := func(revision uint64, primary bool) reqOpts {
		return reqOpts{revision: revision, primary: primary, raid1: true,
			twoLegs: true}
	}
	syncupBoth(t, srv, opts(2, true))
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(opts(3, false))); err != nil {
		t.Fatalf("demote: %v", err)
	}
	mdDev := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	leg0 := srv.nf.DmPath(legName(srv, testDataLeg))
	node.assembleDrop[leg0] = true

	node.Reset()
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts(4, true)))
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
		"grp data after the promotion")
	assertOrder(t, node,
		"cmd mdadm --assemble "+mdDev,
		"cmd mdadm "+mdDev+" --add --failfast "+leg0,
	)
	assertNoCall(t, node, "cmd mdadm --create")
	assertNoCall(t, node, "cmd mdadm --detail")

	node.Reset()
	_, info := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
		ClusterId: testCluster, CnId: testCn,
		CntlrPointer: cntlrPtr(), Revision: 4,
	}, nil)
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data, check round")
	node.Reset()
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(opts(4, true))); err != nil {
		t.Fatalf("re-converge: %v", err)
	}
	assertNoCall(t, node, "cmd mdadm --assemble")
}

// TestSwitchSpareLeg is the CN12 member reconciliation: a leg_list swapped
// with a spare is failed, removed and the promoted spare added — never
// zero-superblocked. The group is the product's two-leg mirror and one leg is
// switched, which is what SwitchSpareLeg does: the array is found through the
// leg that stays (Md.Detail keys on leg_list), and the switched-out leg is
// the extra it still holds.
//
// The switched-out member is taken the ways md holds it. In sync, as an
// operator's switch finds it; and faulty — md marks a member whose side died
// "faulty,failfast" on its first IO that errors yet still lists it under
// dev-*, so the extras loop, which walks every member sysfs lists, must take
// it whatever its state: an extra left in the array would have the promoted
// spare added beside it, where extras leave before promotions arrive. (The
// held set counting a faulty leg_list member is
// TestGroupHeldFaultyMemberStaysHeld's.) AR8's own shape is the faulty
// member whose leg is unavailable too — its side is dead, so its path is not
// both live and optimized (the controller reads connecting and keeps its
// last-known optimized ana_state), or its connect fails, as when the side's
// nvmet port is gone: a parked member is an extra whatever its leg's
// availability, unlike a leg_list member
// (TestGroupUnavailableLegMemberStaysWanted).
func TestSwitchSpareLeg(t *testing.T) {
	connecting := func(srv *CnAgentServer, node *fakeNode) {
		node.setCtrlState(srv.nf.SideToCnNqn(testCluster, testSp,
			testDataLeg, testCn), testIp, testSvcId, "connecting")
	}
	connectFails := func(srv *CnAgentServer, node *fakeNode) {
		nqn := srv.nf.SideToCnNqn(testCluster, testSp, testDataLeg, testCn)
		node.mu.Lock()
		defer node.mu.Unlock()
		subsys := node.subsystems[nqn]
		for _, ctrl := range slices.Clone(subsys.ctrls) {
			node.dropCtrl(subsys, ctrl)
		}
		node.failCmdAlways["--nqn "+nqn+" --hostnqn"] =
			"nvme connect: Connection refused"
	}
	for _, tc := range []struct {
		name  string
		state string // the switched-out member's dev-*/state; "" is in_sync
		// unavail makes the switched-out leg unavailable; nil leaves its
		// path live and optimized.
		unavail func(srv *CnAgentServer, node *fakeNode)
	}{
		{"in sync", "", nil},
		{"faulty", "faulty,failfast", nil},
		{"faulty, path connecting", "faulty,failfast", connecting},
		{"faulty, connect fails", "faulty,failfast", connectFails},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			if _, err := srv.SyncupCn(ctx, cnReq(2, true)); err != nil {
				t.Fatalf("SyncupCn: %v", err)
			}
			if _, err := srv.SyncupCntlr(
				ctx, switchSpareReq(2, false)); err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			// A spare is connected, wrapped and probed but never an md
			// member.
			if !node.hasCall(
				"cmd dmsetup create " + legName(srv, switchSpareLeg)) {
				t.Fatalf("the spare leg was not wrapped")
			}
			for _, call := range node.callsMatching("cmd mdadm --create") {
				if strings.Contains(call, legName(srv, switchSpareLeg)) {
					t.Fatalf("a spare leg became an md member: %s", call)
				}
			}
			if tc.state != "" {
				mdDev := srv.nf.MdPath(srv.nf.CnMdDevName(
					testCluster, testCn, testSp, 0, 0, false))
				node.setMdSync(mdDev, 1, "idle", "none")
				node.setMemberState(mdDev,
					srv.nf.DmPath(legName(srv, testDataLeg)), tc.state)
			}
			if tc.unavail != nil {
				tc.unavail(srv, node)
			}

			node.Reset()
			reply, err := srv.SyncupCntlr(ctx, switchSpareReq(3, true))
			if err != nil {
				t.Fatalf("SwitchSpareLeg: %v", err)
			}
			assertOrder(t, node,
				"--fail "+srv.nf.DmPath(legName(srv, testDataLeg)),
				"--remove "+srv.nf.DmPath(legName(srv, testDataLeg)),
				"--add --failfast "+
					srv.nf.DmPath(legName(srv, switchSpareLeg)),
			)
			assertNoCall(t, node, "--zero-superblock")
			assertNoCall(t, node, "cmd mdadm --detail")
			assertNoCall(t, node,
				"--remove "+srv.nf.DmPath(legName(srv, testDataLeg2)))
			assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
				"grp data after the switch")
		})
	}
}

// switchSpareLeg / switchSpareSide are the spare of switchSpareReq.
const (
	switchSpareLeg  = uint64(0x77)
	switchSpareSide = uint64(0x78)
)

// switchSpareReq is the two-leg raid1 request with a spare leg, before the
// switch (the spare in spare_leg_list) or after it (the spare promoted into
// testDataLeg's place, testDataLeg parked in spare_leg_list).
func switchSpareReq(revision uint64, promoted bool) *pb.SyncupCntlrRequest {
	req := cntlrReq(reqOpts{revision: revision, primary: true,
		raid1: true, twoLegs: true})
	slice := req.GetIdToSlice()[fmt.Sprintf(common.IdKeyFmt, testSlice)]
	grp := slice.GetDataGrpList()[0]
	member := grp.GetLegList()[0]
	spare := legOf(switchSpareLeg, sideOf(switchSpareSide, testIp2,
		testSvcId2))
	spare.LegIdx = 2
	if promoted {
		grp.LegList[0] = spare
		grp.SpareLegList = []*pb.Leg{member}
	} else {
		grp.SpareLegList = []*pb.Leg{spare}
	}
	return req
}

// TestSwitchSpareLegKilledVerb pins CN12's killed-verb rule for the switch:
// a `--fail`, `--remove` or `--add` that did not answer leaves the group row
// ERROR with the kill's error, whether the kernel completed the command or
// the tool was killed before touching anything. A killed `--fail` or
// `--remove` leaves the pass before the add loop — extras leave before
// promotions arrive, so the promoted spare is never added beside a member md
// may still hold — and the next converge completes the switch. An error
// swallowed here would read the row OK over a half-done switch.
func TestSwitchSpareLegKilledVerb(t *testing.T) {
	for _, verb := range []string{" --fail ", " --remove ", " --add "} {
		for _, noEffect := range []bool{true, false} {
			name := strings.TrimSpace(verb) + ", completed"
			if noEffect {
				name = strings.TrimSpace(verb) + ", no effect"
			}
			t.Run(name, func(t *testing.T) {
				srv, node := newTestServer(t)
				ctx := context.Background()
				if _, err := srv.SyncupCn(ctx, cnReq(2, true)); err != nil {
					t.Fatalf("SyncupCn: %v", err)
				}
				if _, err := srv.SyncupCntlr(
					ctx, switchSpareReq(2, false)); err != nil {
					t.Fatalf("SyncupCntlr: %v", err)
				}
				mdDev := srv.nf.MdPath(srv.nf.CnMdDevName(
					testCluster, testCn, testSp, 0, 0, false))

				node.mu.Lock()
				if noEffect {
					node.killCmdNoEffectAlways[verb] = true
				} else {
					node.killCmdAlways[verb] = true
				}
				node.mu.Unlock()
				node.Reset()
				reply, err := srv.SyncupCntlr(ctx, switchSpareReq(3, true))
				if err != nil {
					t.Fatalf("SwitchSpareLeg: %v", err)
				}
				row := reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]
				if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
					!strings.Contains(row.GetDetails(), "killed") {
					t.Fatalf("the switch with %s killed read %v %q, "+
						"want ERROR with the kill", strings.TrimSpace(verb),
						row.GetStatus(), row.GetDetails())
				}
				if !node.hasCall("cmd mdadm " + mdDev + verb) {
					t.Fatalf("the switch ran no %s; the case is vacuous",
						strings.TrimSpace(verb))
				}
				if verb != " --add " {
					assertNoCall(t, node, "--add --failfast")
				}
				assertNoCall(t, node, "cmd mdadm --detail")

				node.mu.Lock()
				clear(node.killCmdNoEffectAlways)
				clear(node.killCmdAlways)
				node.mu.Unlock()
				node.Reset()
				if reply, err = srv.SyncupCntlr(
					ctx, switchSpareReq(4, true)); err != nil {
					t.Fatalf("SyncupCntlr: %v", err)
				}
				assertOk(t,
					reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
					"grp data after the next converge")
				assertNoCall(t, node, "cmd mdadm --detail")
				got := slices.Sorted(slices.Values(
					node.arrays[mdDev].members))
				want := slices.Sorted(slices.Values([]string{
					srv.nf.DmPath(legName(srv, testDataLeg2)),
					srv.nf.DmPath(legName(srv, switchSpareLeg)),
				}))
				if !slices.Equal(got, want) {
					t.Fatalf("after the next converge the array holds %v, "+
						"want %v", got, want)
				}
			})
		}
	}
}

// TestGroupNeverCreatesBesideARunningArray is the both-legs-replaced shape
// (CN12, 2026-09-26): an array runs under the group's name holding none of
// the wrappers leg_list now names — both legs switched out while this cntlr
// was not converging, parked in spare_leg_list or already released. Md.Detail
// finds an array by its leg_list members only, so it reads the group as
// absent and the assembly runs; the fresh legs carry no superblock, which is
// case 1, and a create there would put a second array under the name the
// pool's concat resolves. The outcome must be an error that touches nothing:
// no create, and no fail, remove or add against the running array — which a
// Detail keyed on the parked spares too would have reached, failing and
// removing the only members holding the group's data. The refusal holds when
// the lsblk that asks whether an array runs under the name did not answer,
// too: read as "no", a kill would create beside the running array.
func TestGroupNeverCreatesBesideARunningArray(t *testing.T) {
	for _, tc := range []struct {
		name          string
		parked        bool
		killNameProbe bool
	}{
		{"parked", true, false},
		{"released", false, false},
		{"name probe killed", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{
				revision: 2, primary: true, raid1: true, twoLegs: true})
			mdDev := srv.nf.MdPath(
				srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
			held := append([]string(nil), node.arrays[mdDev].members...)

			req := cntlrReq(reqOpts{
				revision: 3, primary: true, raid1: true, twoLegs: true})
			grp := req.GetIdToSlice()[fmt.Sprintf(common.IdKeyFmt, testSlice)].
				GetDataGrpList()[0]
			fresh := legOf(0x71, sideOf(0x72, testIp, testSvcId))
			fresh2 := legOf(0x73, sideOf(0x74, testIp2, testSvcId2))
			fresh2.LegIdx = 1
			if tc.parked {
				grp.SpareLegList = grp.GetLegList()
				for i, leg := range grp.SpareLegList {
					leg.LegIdx = uint32(2 + i)
				}
			}
			grp.LegList = []*pb.Leg{fresh, fresh2}
			// Keyed on the md node: the dm wrappers' devno reads are lsblk
			// runs of the same form.
			want := "holding none of the group's legs"
			if tc.killNameProbe {
				node.mu.Lock()
				node.killCmdAlways["lsblk --nodeps --noheadings "+
					"--output MAJ:MIN "+mdDev] = true
				node.mu.Unlock()
				want = "signal: killed"
			}

			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(), req)
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			info := reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]
			if info.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
				!strings.Contains(info.GetDetails(), want) {
				t.Fatalf("the group reported %v %q, want ERROR %q",
					info.GetStatus(), info.GetDetails(), want)
			}
			assertNoCall(t, node, "cmd mdadm --create")
			assertNoCall(t, node, "cmd mdadm --detail")
			for _, verb := range []string{"--fail", "--remove", "--add"} {
				assertNoCall(t, node, "cmd mdadm "+mdDev+" "+verb)
			}
			array := node.arrays[mdDev]
			if array == nil || !slices.Equal(array.members, held) {
				t.Fatalf("the running array changed: %+v, want members %v",
					array, held)
			}
		})
	}
}

// TestGroupProbeWalksOnce pins the cost of the sysfs md read (CN12): a Check
// round lists /sys/block and each array's md/ directory once for all of its
// groups, not once per group. A listing is an `ls` — a process — and a walk
// per group lists every array of the node per group: at 32 slices, 64 groups
// over 64 arrays, 4160 listings a round where the whole round has 5 s. The
// round's other listing is the verdict's own (CN30), which enumerates the
// node exactly as the sweep does. Neither lists or reads any /sys/block entry
// that is not an array node: a CN's dm devices and nvme heads outnumber its
// arrays many times over.
func TestGroupProbeWalksOnce(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, raid1: true, twoSlices: true})
	var arrays []string
	for _, array := range node.arrays {
		arrays = append(arrays, array.node)
	}
	if len(arrays) != 4 {
		t.Fatalf("the fixture built %d arrays, want 4", len(arrays))
	}
	others := seedNonArrayEntries(node)
	node.Reset()
	_, info := srv.checkCntlrRound(context.Background(),
		&pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: 2,
		}, nil)
	if len(info.GetGrpIdToMdRaid()) != 4 {
		t.Fatalf("the round reported %d md rows, want 4",
			len(info.GetGrpIdToMdRaid()))
	}
	if got := countCalls(node, "cmd ls -1 "+sysfsBlockDir); got != 2 {
		t.Errorf("the round listed %s %d times, want 2 (the verdict's and "+
			"the md rows' one walk)", sysfsBlockDir, got)
	}
	for _, array := range arrays {
		dir := sysfsBlockDir + "/" + array + "/md"
		if got := countCalls(node, "cmd ls -1 "+dir); got != 2 {
			t.Errorf("the round listed %s %d times, want 2", dir, got)
		}
	}
	assertNoNonArrayRead(t, node, others, "the round")
}

// seedNonArrayEntries puts the block devices a CN really has beside its
// arrays at the top of the fake's /sys/block — the fake publishes arrays
// alone there otherwise — so a reader that stopped filtering for array nodes
// has something to list. dm-0 is given an md/ directory it never has on a
// real node, which a missing filter would list and record as an array.
func seedNonArrayEntries(node *fakeNode) []string {
	names := []string{"dm-0", "dm-7", "nvme0n1", "nvme0c0n1", "sda", "loop0"}
	node.mu.Lock()
	defer node.mu.Unlock()
	for _, name := range names {
		node.dirs[sysfsBlockDir+"/"+name] = true
	}
	node.dirs[sysfsBlockDir+"/dm-0/md"] = true
	return names
}

// assertNoNonArrayRead fails on any listing or read of, or under, a
// /sys/block entry seedNonArrayEntries planted.
func assertNoNonArrayRead(
	t *testing.T, node *fakeNode, names []string, label string,
) {
	t.Helper()
	for _, name := range names {
		dir := sysfsBlockDir + "/" + name
		for _, call := range node.Calls() {
			if call == "cmd ls -1 "+dir ||
				strings.HasPrefix(call, "cmd ls -1 "+dir+"/") ||
				strings.HasPrefix(call, "read "+dir+"/") {
				t.Errorf("%s touched %s, which is no array node: %s",
					label, dir, call)
			}
		}
	}
}

// countCalls counts the recorded calls that are exactly line — a listing of
// /sys/block is a prefix of every listing below it.
func countCalls(node *fakeNode, line string) int {
	n := 0
	for _, call := range node.Calls() {
		if call == line {
			n++
		}
	}
	return n
}

// TestGroupConvergeWalksOnce is TestGroupProbeWalksOnce for the converge
// (CN12): build hands every group of the pass the same walk too. An
// equal-revision SyncupCntlr over the same four arrays assembles nothing, so
// no Refresh runs, and it lists /sys/block and each array's md/ directory
// exactly twice — the sweep's ListArrays enumeration and the one walk the
// groups share — and no /sys/block entry that is not an array node. A walk
// per group would list each five times.
func TestGroupConvergeWalksOnce(t *testing.T) {
	srv, node := newTestServer(t)
	opts := reqOpts{revision: 2, primary: true, raid1: true, twoSlices: true}
	syncupBoth(t, srv, opts)
	var arrays []string
	for _, array := range node.arrays {
		arrays = append(arrays, array.node)
	}
	if len(arrays) != 4 {
		t.Fatalf("the fixture built %d arrays, want 4", len(arrays))
	}
	others := seedNonArrayEntries(node)
	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(opts))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	if len(reply.GetCntlrInfo().GetGrpIdToMdRaid()) != 4 {
		t.Fatalf("the converge reported %d md rows, want 4",
			len(reply.GetCntlrInfo().GetGrpIdToMdRaid()))
	}
	assertNoCall(t, node, "cmd mdadm")
	if got := countCalls(node, "cmd ls -1 "+sysfsBlockDir); got != 2 {
		t.Errorf("the converge listed %s %d times, want 2 (the sweep's "+
			"and the groups' one walk)", sysfsBlockDir, got)
	}
	for _, array := range arrays {
		dir := sysfsBlockDir + "/" + array + "/md"
		if got := countCalls(node, "cmd ls -1 "+dir); got != 2 {
			t.Errorf("the converge listed %s %d times, want 2", dir, got)
		}
	}
	assertNoNonArrayRead(t, node, others, "the converge")
}

// TestGroupAssemblyBesideAnArrayMidStop: after an assembly, Md.Refresh checks
// every array the pass's walk recorded, another sp's among them, and another
// cntlr may be stopping that one at that very moment (CN12). md unbinds a
// member by clearing its array pointer and removing its block link, and
// until it deletes the dev-* directory every attribute of the member's own
// reads ENODEV. Both cases here keep the block link: the member's state
// failing is the instant between md clearing the pointer and removing the
// link, and its block/dev failing too is a check that did not answer at all.
// That array is no business of this group: the check reads no attribute of
// the member's own, and a check that did not answer only makes Refresh walk
// that node again — the assembled groups read OK either way. The member with
// its block link gone is TestGroupBesideAnUnboundMember's.
func TestGroupAssemblyBesideAnArrayMidStop(t *testing.T) {
	const leg = "/dev/mapper/other-sp-leg"
	var listed []int
	for _, tc := range []struct {
		name string
		fail []string // the other array's member attributes that fail
	}{
		{"member state unreadable", []string{"/state"}},
		{"check unanswered", []string{"/state", "/block/dev"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			node.seedArray("/dev/md/other", "other", leg)
			for _, attr := range tc.fail {
				node.failReadAlways["/dev-"+node.kernelName(leg)+attr] = true
			}
			reply := syncupBoth(t, srv,
				reqOpts{revision: 2, primary: true, raid1: true})
			rows := reply.GetCntlrInfo().GetGrpIdToMdRaid()
			if len(rows) != 2 {
				t.Fatalf("the converge reported %d md rows, want 2",
					len(rows))
			}
			for grp, row := range rows {
				assertOk(t, row, fmt.Sprintf("grp %#x", grp))
			}
			other := node.mdNode("/dev/md/other")
			listed = append(listed, countCalls(node,
				"cmd ls -1 "+sysfsBlockDir+strings.TrimPrefix(other, "/dev")+
					"/md"))
		})
	}
	// Non-vacuity: with block/dev failing too, the check did not answer and
	// Refresh walked the other array again after each of the two assemblies.
	if len(listed) == 2 && listed[1] != listed[0]+2 {
		t.Errorf("the other array's md/ was listed %d and %d times, want "+
			"the second two more (the re-walks)", listed[0], listed[1])
	}
}

// TestGroupBesideAnUnboundMember pins the kernel's own shape of another sp's
// array mid-stop (CN12): md has unbound its member, so the member's block
// link is gone — dev-*/block/dev and dev-*/block/dm/name read ENOENT — and
// its dev-*/state reads ENODEV until md deletes the dev-* directory
// (fakeNode.unbindMember). The walk records that member with no dm name,
// which no group's names match, and never the array as unanswered; at this
// instant the sweep's ListArrays reads the array as foreign and leaves it
// alone. So a group not built yet — a new primary's first converge
// overlapping another sp's teardown — is created beside it with every md row
// OK and a clean reply, and a built group's Check round and converge read OK
// and run no mdadm. A walk that read the missing block/dev as "did not
// answer" would refuse that assembly, which, with every leg of the cntlr
// available as here, nothing re-drives (a leg_list member of any group of
// the cntlr that is not available registers the CN10 retry, whose next
// converge would try it again, CN12), and a ListArrays that did would fail
// the sweep's md enumeration at this instant too (later in the stop, once md
// marks the array deleted, its array_state reads EBUSY and ListArrays does
// fail that pass — CN12).
func TestGroupBesideAnUnboundMember(t *testing.T) {
	ctx := context.Background()
	const leg = "/dev/mapper/other-sp-leg"
	opts := reqOpts{revision: 2, primary: true, raid1: true}
	assertRows := func(t *testing.T, rows map[uint64]*pb.ResInfo, label string) {
		t.Helper()
		if len(rows) != 2 {
			t.Fatalf("%s reported %d md rows, want 2", label, len(rows))
		}
		for grp, row := range rows {
			assertOk(t, row, fmt.Sprintf("%s grp %#x", label, grp))
		}
	}
	unbound := func(t *testing.T, node *fakeNode) {
		t.Helper()
		node.seedArray("/dev/md/other", "other", leg)
		node.unbindMember("/dev/md/other", leg)
		node.mu.Lock()
		defer node.mu.Unlock()
		mdDir := sysfsBlockDir + "/" + node.arrays["/dev/md/other"].node + "/md"
		devDir := mdDir + "/dev-" + node.kernelName(leg)
		if !node.dirs[devDir] || node.dirs[devDir+"/block"] {
			t.Fatalf("the unbound member's dev-* directory must stay and " +
				"its block link go; the case is vacuous otherwise")
		}
	}

	t.Run("not built", func(t *testing.T) {
		srv, node := newTestServer(t)
		if reply, err := srv.SyncupCn(ctx, cnReq(2, true)); err != nil ||
			reply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("SyncupCn: %v %v", reply.GetAgentReply(), err)
		}
		unbound(t, node)
		node.Reset()
		reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		if reply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("the converge beside the unbound member replied %v, "+
				"want a clean reply", reply.GetAgentReply())
		}
		assertRows(t, reply.GetCntlrInfo().GetGrpIdToMdRaid(), "converge")
		if got := len(node.callsMatching("cmd mdadm --create")); got != 2 {
			t.Fatalf("the converge created %d arrays, want both groups'",
				got)
		}
		if node.arrayGone("/dev/md/other") {
			t.Fatalf("the sweep stopped the other sp's array")
		}
	})

	t.Run("built", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, opts)
		probeAllLegs(t, srv)
		unbound(t, node)
		node.Reset()
		check, info := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: 2,
		}, nil)
		if check.GetAgentReply().GetCode() != 0 {
			t.Fatalf("the Check round beside the unbound member replied "+
				"%v, want a clean verdict", check.GetAgentReply())
		}
		assertRows(t, info.GetGrpIdToMdRaid(), "check")
		reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		if reply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("the converge beside the unbound member replied %v, "+
				"want a clean reply", reply.GetAgentReply())
		}
		assertRows(t, reply.GetCntlrInfo().GetGrpIdToMdRaid(), "converge")
		assertNoCall(t, node, "cmd mdadm")
	})
}

// TestGroupBesideAnUnansweredArray pins that another sp's array never turns
// the md row of a group whose own array answered ERROR. The pass's walk reads
// every array on the node, and another array's md/ listing or member dm-name
// read can fail to answer (the fake kills the ls, or fails the read with an
// error that is not ENOENT); an md row counts toward cntlr health, so an
// error there would fail this sp's primary over for a read of another sp's
// array — the very fault a failover cannot fix. With the group's array built,
// a Check round and a converge read OK and run no mdadm; both reply a
// Leftover all the same, because the sweep's ListArrays keeps the strict
// rule (CN21) and the same fault fails its md enumeration — a reply code,
// never a row. With it not built yet, the unanswered array may be the
// group's own, so the converge neither creates nor assembles (ERROR) and,
// with every leg available, registers no CN10 retry — nothing re-drives that
// converge by itself (CN12); the test's next SyncupCntlr, once the array
// answers, assembles it.
func TestGroupBesideAnUnansweredArray(t *testing.T) {
	ctx := context.Background()
	const leg = "/dev/mapper/other-sp-leg"
	opts := reqOpts{revision: 2, primary: true, raid1: true}
	for _, tc := range []struct {
		name string
		kill func(node *fakeNode)
	}{
		{"member dm name", func(node *fakeNode) {
			node.failReadAlways["/dev-"+node.kernelName(leg)+
				"/block/dm/name"] = true
		}},
		{"md listing", func(node *fakeNode) {
			node.killCmdAlways["ls -1 "+sysfsBlockDir+strings.TrimPrefix(
				node.mdNode("/dev/md/other"), "/dev")+"/md"] = true
		}},
	} {
		t.Run(tc.name+", built", func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, opts)
			probeAllLegs(t, srv)
			node.seedArray("/dev/md/other", "other", leg)
			tc.kill(node)
			// The same fault under the sweep's strict rule (CN21): the md
			// enumeration of the Check verdict and of the converge's sweep
			// fails, which is a Leftover reply, never a row.
			assertMdLeftover := func(label string, reply *pb.AgentReply) {
				t.Helper()
				if reply.GetCode() != common.ReplyCodeLeftover ||
					!strings.Contains(reply.GetDetails(),
						"enumeration failed") ||
					!strings.Contains(reply.GetDetails(), "md arrays") {
					t.Fatalf("the %s replied %v, want a Leftover naming "+
						"the md enumeration", label, reply)
				}
			}
			node.Reset()
			check, info := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
				ClusterId: testCluster, CnId: testCn,
				CntlrPointer: cntlrPtr(), Revision: 2,
			}, nil)
			rows := info.GetGrpIdToMdRaid()
			if len(rows) != 2 {
				t.Fatalf("the Check round reported %d md rows, want 2",
					len(rows))
			}
			for grp, row := range rows {
				assertOk(t, row, fmt.Sprintf("check grp %#x", grp))
			}
			assertMdLeftover("Check round", check.GetAgentReply())
			reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			for grp, row := range reply.GetCntlrInfo().GetGrpIdToMdRaid() {
				assertOk(t, row, fmt.Sprintf("converge grp %#x", grp))
			}
			assertNoCall(t, node, "cmd mdadm")
			assertMdLeftover("converge", reply.GetAgentReply())
		})
		t.Run(tc.name+", not built", func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			// The node's own sweep refuses to run over an enumeration that
			// did not answer (CN21), so the cn is synced first.
			if reply, err := srv.SyncupCn(ctx, cnReq(2, true)); err != nil ||
				reply.GetAgentReply().GetCode() != 0 {
				t.Fatalf("SyncupCn: %v %v", reply.GetAgentReply(), err)
			}
			node.seedArray("/dev/md/other", "other", leg)
			tc.kill(node)
			reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			rows := reply.GetCntlrInfo().GetGrpIdToMdRaid()
			if len(rows) != 2 {
				t.Fatalf("the converge reported %d md rows, want 2",
					len(rows))
			}
			for grp, row := range rows {
				if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
					!strings.Contains(row.GetDetails(), "did not answer") {
					t.Fatalf("grp %#x beside an unanswered array, with "+
						"its own not built, read %v %q, want ERROR "+
						"naming it", grp, row.GetStatus(), row.GetDetails())
				}
			}
			assertNoCall(t, node, "cmd mdadm --create")
			assertNoCall(t, node, "cmd mdadm --assemble")
			if retrying(t, srv) {
				t.Fatalf("a group error with every leg available " +
					"registered the retry")
			}

			clear(node.failReadAlways)
			clear(node.killCmdAlways)
			if reply, err = srv.SyncupCntlr(ctx, cntlrReq(opts)); err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			for grp, row := range reply.GetCntlrInfo().GetGrpIdToMdRaid() {
				assertOk(t, row, fmt.Sprintf("answering grp %#x", grp))
			}
		})
	}
}

// TestGroupProbeIsSysfsOnly pins the trigger of the failover ping-pong at the
// probe itself (CN28). A primary's Check round over an array md has failed a
// member of (md/degraded 1, the member faulty) reports the md row from sysfs:
// OK with "degraded" in its details, and not one mdadm command — the old
// `mdadm --detail` loaded a superblock from a member, blocked on a dead one
// past the soft timeout, and turned the row ERROR, which counts toward cntlr
// health and failed the primary over. A rebuild reads in mdadm's own words
// with sysfs's progress. An array that is not running (inactive here, the
// other states in TestGroupProbeArrayStates) reads ERROR with its state as
// details, and a converge leaves it alone.
func TestGroupProbeIsSysfsOnly(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, raid1: true, twoLegs: true})
	probeAllLegs(t, srv)
	mdDev := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	node.setMdSync(mdDev, 1, "idle", "none")
	node.setMemberState(mdDev, srv.nf.DmPath(legName(srv, testDataLeg2)),
		"faulty,failfast")
	req := &pb.CheckCntlrRequest{
		ClusterId: testCluster, CnId: testCn,
		CntlrPointer: cntlrPtr(), Revision: 2,
	}
	round := func(label string) *pb.ResInfo {
		t.Helper()
		node.Reset()
		_, info := srv.checkCntlrRound(context.Background(), req, nil)
		for _, call := range node.Calls() {
			if strings.Contains(call, "cmd mdadm") {
				t.Fatalf("%s: the check round ran mdadm: %s", label, call)
			}
		}
		return info.GetGrpIdToMdRaid()[testDataGrp]
	}

	row := round("degraded")
	if row.GetStatus() != pb.ResStatus_RES_STATUS_OK ||
		row.GetDetails() != "clean, degraded" {
		t.Fatalf("a degraded array reported %v %q, want OK \"clean, "+
			"degraded\"", row.GetStatus(), row.GetDetails())
	}

	node.setMdSync(mdDev, 1, "recover", "32768 / 2093056")
	row = round("recovering")
	if row.GetStatus() != pb.ResStatus_RES_STATUS_OK ||
		row.GetDetails() != "clean, degraded, recovering (32768 / 2093056)" {
		t.Fatalf("a rebuilding array reported %v %q", row.GetStatus(),
			row.GetDetails())
	}

	node.setArrayState(mdDev, "inactive")
	row = round("inactive")
	if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		row.GetDetails() != "inactive" {
		t.Fatalf("an inactive array reported %v %q, want ERROR \"inactive\"",
			row.GetStatus(), row.GetDetails())
	}
	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 2, primary: true, raid1: true, twoLegs: true}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	info := reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]
	if info.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(info.GetDetails(), "inactive") {
		t.Fatalf("a converge over an inactive array reported %v %q",
			info.GetStatus(), info.GetDetails())
	}
	// Not one mdadm command: no --add, --fail or --remove against the
	// array, no assembly, and no probe of it either.
	assertNoCall(t, node, "cmd mdadm")
}

// TestGroupProbeArrayStates pins which array_state values are a running array
// (CN12, CN28), through the verdict and, every state but "clear", through the
// converge's gate. The six running ones read OK with the state and the
// "degraded" of a failed member. write-pending is the one the fault lives in
// — md shows it while a superblock write is stuck on a dead member, and the
// lab read it in exactly that window — so an ERROR there would bring the
// failover trigger back through the verdict. Every other state reads ERROR
// with the state as details, and "clear", which the lab never showed (a
// stopped array's /sys/block/mdN goes at once), reads MISSING like an absent
// array on the Check round. No converge is driven over "clear": md reads
// clear only for an array with no member at all, which no group's Detail can
// match. A converge reconciles a running array whatever its state — here the
// --add of the leg_list member it lacks — and runs no mdadm against an array
// in any other state it is driven over.
func TestGroupProbeArrayStates(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	opts := reqOpts{revision: 2, primary: true, raid1: true, twoLegs: true}
	syncupBoth(t, srv, opts)
	probeAllLegs(t, srv)
	mdDev := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	leg2 := srv.nf.DmPath(legName(srv, testDataLeg2))
	req := &pb.CheckCntlrRequest{
		ClusterId: testCluster, CnId: testCn,
		CntlrPointer: cntlrPtr(), Revision: 2,
	}
	const (
		ok      = pb.ResStatus_RES_STATUS_OK
		failed  = pb.ResStatus_RES_STATUS_ERROR
		missing = pb.ResStatus_RES_STATUS_MISSING
	)
	for _, tc := range []struct {
		state   string
		status  pb.ResStatus
		details string
	}{
		{"clean", ok, "clean, degraded"},
		{"active", ok, "active, degraded"},
		{"active-idle", ok, "active-idle, degraded"},
		{"write-pending", ok, "write-pending, degraded"},
		{"readonly", ok, "readonly, degraded"},
		{"read-auto", ok, "read-auto, degraded"},
		{"inactive", failed, "inactive"},
		{"suspended", failed, "suspended"},
		{"broken", failed, "broken"},
		{"no-such-state", failed, "no-such-state"},
		{"clear", missing, ""},
	} {
		// md failed and removed the second leg: the array lacks a leg_list
		// member it could re-add.
		node.mu.Lock()
		array := node.arrays[mdDev]
		array.members = slices.DeleteFunc(array.members,
			func(member string) bool { return member == leg2 })
		node.mu.Unlock()
		node.setMdSync(mdDev, 1, "idle", "none")
		node.setArrayState(mdDev, tc.state)

		node.Reset()
		_, info := srv.checkCntlrRound(ctx, req, nil)
		row := info.GetGrpIdToMdRaid()[testDataGrp]
		if row.GetStatus() != tc.status || row.GetDetails() != tc.details {
			t.Errorf("%s: the check round reported %v %q, want %v %q",
				tc.state, row.GetStatus(), row.GetDetails(), tc.status,
				tc.details)
		}
		assertNoCall(t, node, "cmd mdadm")
		if tc.status == missing {
			continue
		}

		node.Reset()
		reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
		if err != nil {
			t.Fatalf("%s: SyncupCntlr: %v", tc.state, err)
		}
		row = reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]
		added := node.hasCall("cmd mdadm " + mdDev + " --add --failfast " +
			leg2)
		switch {
		case tc.status == ok && (row.GetStatus() != ok || !added):
			t.Errorf("%s: the converge reported %v %q and added the "+
				"missing member: %v, want OK and an --add", tc.state,
				row.GetStatus(), row.GetDetails(), added)
		case tc.status == failed && (row.GetStatus() != failed ||
			!strings.Contains(row.GetDetails(), " is "+tc.state)):
			t.Errorf("%s: the converge reported %v %q, want ERROR naming "+
				"the state", tc.state, row.GetStatus(), row.GetDetails())
		case tc.status == failed && node.hasCall("cmd mdadm"):
			t.Errorf("%s: the converge ran mdadm on an array that is not "+
				"running: %v", tc.state, node.callsMatching("cmd mdadm"))
		}
	}
}

// TestGroupForeignMemberIsAnError: an array that holds the group's leg
// wrappers and a member that is not a dm device is not ours to reconcile
// (CN12), and its md row reads ERROR naming that member (CN28) — on a Check
// round and on a converge alike, with no mdadm run. Without the converge's
// guard the foreign member is an extra with no dm name, and reconcileMembers
// would --fail and --remove "/dev/mapper/".
func TestGroupForeignMemberIsAnError(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	opts := reqOpts{revision: 2, primary: true, raid1: true, twoLegs: true}
	syncupBoth(t, srv, opts)
	probeAllLegs(t, srv)
	mdDev := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	held := append(append([]string(nil), node.arrays[mdDev].members...),
		"/dev/sdb1")
	node.seedArray(mdDev, node.arrays[mdDev].name, held...)
	want := "foreign member " + node.devNo["/dev/sdb1"]
	check := func(label string, row *pb.ResInfo) {
		t.Helper()
		if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
			!strings.Contains(row.GetDetails(), want) {
			t.Fatalf("%s: the md row reported %v %q, want ERROR %q", label,
				row.GetStatus(), row.GetDetails(), want)
		}
		assertNoCall(t, node, "cmd mdadm")
	}

	node.Reset()
	_, info := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
		ClusterId: testCluster, CnId: testCn,
		CntlrPointer: cntlrPtr(), Revision: 2,
	}, nil)
	check("check round", info.GetGrpIdToMdRaid()[testDataGrp])

	node.Reset()
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	check("converge", reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp])
	if got := node.arrays[mdDev].members; !slices.Equal(got, held) {
		t.Fatalf("the array's members changed to %v, want %v", got, held)
	}
}

// TestGroupUnansweredSysfsReadIsAnError is "did not answer is not absent"
// (CN12, CN28) at the callers, where TestMdDetailFromSysfsOnly pins it at
// Md: a Check round whose walk left no array answering (the kill hits every
// array's member names), or whose read of the matched array did not answer,
// reports the md row ERROR, never MISSING, and a converge refuses with
// no mdadm at all. Read as absent, a killed member name or array_state would
// send the group into an assembly beside its running array — the shape a
// killed `mdadm --detail` once had. A member's block/dev, which only names a
// foreign member now that members are compared by dm name
// (TestGroupMembersComparedByName), keeps the same rule.
func TestGroupUnansweredSysfsReadIsAnError(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	opts := reqOpts{revision: 2, primary: true, raid1: true, twoLegs: true}
	syncupBoth(t, srv, opts)
	probeAllLegs(t, srv)
	req := &pb.CheckCntlrRequest{
		ClusterId: testCluster, CnId: testCn,
		CntlrPointer: cntlrPtr(), Revision: 2,
	}
	for _, key := range []string{
		// Md.Walk's member names: every array reads unanswered and none
		// matches, so Detail fails
		"/block/dm/name",
		"/md/degraded", // readDetail: the walk answers, Detail fails
		// readDetail's first read: absent would assemble beside the array
		"/md/array_state",
		// readDetail's member devno, which names a foreign member
		"/block/dev",
	} {
		node.mu.Lock()
		node.killReadAlways[key] = true
		node.mu.Unlock()

		node.Reset()
		_, info := srv.checkCntlrRound(ctx, req, nil)
		row := info.GetGrpIdToMdRaid()[testDataGrp]
		if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
			t.Errorf("%s killed: the check round reported %v %q, want ERROR",
				key, row.GetStatus(), row.GetDetails())
		}
		assertNoCall(t, node, "cmd mdadm")

		node.Reset()
		reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
		if err != nil {
			t.Fatalf("%s killed: SyncupCntlr: %v", key, err)
		}
		row = reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]
		if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
			t.Errorf("%s killed: the converge reported %v %q, want ERROR",
				key, row.GetStatus(), row.GetDetails())
		}
		assertNoCall(t, node, "cmd mdadm")

		node.mu.Lock()
		delete(node.killReadAlways, key)
		node.mu.Unlock()
	}
}

// TestGroupMembersComparedByName pins CN12's member comparison (amended
// 2026-09-26): the held set is the dm names sysfs gave the array's members,
// compared with the leg_list wrapper names, and reconcileMembers runs no
// lsblk. Keyed by device number, an lsblk of a leg_list wrapper that did not
// answer left that wrapper out of the wanted set, its in-sync member counted
// as an extra, and an equal-revision converge --failed and --removed it — the
// add loop skipped it for the same reason, and the mirror ran on one member
// until some unrelated converge.
func TestGroupMembersComparedByName(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	opts := reqOpts{revision: 2, primary: true, raid1: true, twoLegs: true}
	syncupBoth(t, srv, opts)
	mdDev := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	held := slices.Clone(node.arrays[mdDev].members)
	if len(held) != 2 {
		t.Fatalf("the converged array holds %v, want both legs", held)
	}
	for _, legId := range []uint64{testDataLeg, testDataLeg2} {
		lsblk := "lsblk --nodeps --noheadings --output MAJ:MIN " +
			srv.nf.DmPath(legName(srv, legId))
		node.mu.Lock()
		node.killCmdAlways[lsblk] = true
		node.mu.Unlock()

		node.Reset()
		reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
		if err != nil {
			t.Fatalf("leg %#x's lsblk killed: SyncupCntlr: %v", legId, err)
		}
		for _, verb := range []string{"--fail", "--remove", "--add"} {
			assertNoCall(t, node, "cmd mdadm "+mdDev+" "+verb)
		}
		assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
			fmt.Sprintf("grp data, leg %#x's lsblk killed", legId))
		if got := node.arrays[mdDev].members; !slices.Equal(got, held) {
			t.Fatalf("leg %#x's lsblk killed: the array's members changed "+
				"to %v, want %v", legId, got, held)
		}

		node.mu.Lock()
		delete(node.killCmdAlways, lsblk)
		node.mu.Unlock()
	}
}

// TestGroupUnavailableLegMemberStaysWanted pins the other half of CN12's
// member comparison: the wanted set is every leg_list wrapper name, available
// or not. A leg this pass cannot use — its path inaccessible or
// non-optimized, or its connect failing with the wrapper of an earlier pass
// still there — is never added, but the member md still holds for it stays
// wanted. Counted as an extra, an equal-revision converge would --fail and
// --remove an in-sync member because one side's path was briefly unusable,
// and run the mirror on the other. Each unusable leg registers the CN10
// retry all the same — a failed connect in ensureLegs, an unavailable path
// as a late member (CN12), which is availability alone; the failed connect's
// leg is late too, so TestFailedConnectRegistersTheRetryWithoutALateMember is
// what pins ensureLegs' own registration. The late verdict ORs over the
// group's whole leg_list, so a late first member beside an available later
// one registers the retry too ("non-optimized first member": leg 1 late,
// leg 2 available) — the leg-level twin of a late member in an earlier
// group, TestLateMembersRegisterTheRetry's "meta only". Only after the
// failed connect does an attempt change anything: it reconnects the side. A
// held member whose path is merely inaccessible needs no converge: nvme
// multipath queues its IO, so md keeps it, and the attempt after the path is
// optimized again runs no mdadm and only stops the retry. A non-optimized
// path is the side's dm-error, so the first write through it fails (a
// target-internal DNR error, not a path error), md fails the failfast
// member, and it stays failed (cnagent.md §7, "a member md failed stays
// failed"): neither this converge nor the retry re-adds it, and the attempt
// after the path is optimized again likewise runs no mdadm and only stops
// the retry. Until then each attempt is the cost cnagent.md §7 records.
func TestGroupUnavailableLegMemberStaysWanted(t *testing.T) {
	for _, tc := range []struct {
		name string
		// legId is the leg this pass cannot use.
		legId   uint64
		unavail func(srv *CnAgentServer, node *fakeNode)
		legErr  bool // legId's row reads ERROR: ensureLeg failed
	}{
		{"inaccessible", testDataLeg2, func(srv *CnAgentServer,
			node *fakeNode) {
			node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp,
				testDataLeg2, testCn), testIp2, testSvcId2, "inaccessible")
		}, false},
		{"non-optimized", testDataLeg2, func(srv *CnAgentServer,
			node *fakeNode) {
			node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp,
				testDataLeg2, testCn), testIp2, testSvcId2, "non-optimized")
		}, false},
		{"non-optimized first member", testDataLeg, func(srv *CnAgentServer,
			node *fakeNode) {
			node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp,
				testDataLeg, testCn), testIp, testSvcId, "non-optimized")
		}, false},
		{"connect fails", testDataLeg2, func(srv *CnAgentServer,
			node *fakeNode) {
			nqn := srv.nf.SideToCnNqn(testCluster, testSp, testDataLeg2,
				testCn)
			node.mu.Lock()
			defer node.mu.Unlock()
			subsys := node.subsystems[nqn]
			for _, ctrl := range slices.Clone(subsys.ctrls) {
				node.dropCtrl(subsys, ctrl)
			}
			node.failCmdAlways["--nqn "+nqn+" --hostnqn"] =
				"nvme connect: Connection refused"
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			ctx := context.Background()
			opts := reqOpts{
				revision: 2, primary: true, raid1: true, twoLegs: true}
			syncupBoth(t, srv, opts)
			mdDev := srv.nf.MdPath(
				srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
			held := slices.Clone(node.arrays[mdDev].members)
			if len(held) != 2 {
				t.Fatalf("the converged array holds %v, want both legs",
					held)
			}
			if retrying(t, srv) {
				t.Fatalf("a converge with every leg available registered " +
					"the retry")
			}
			tc.unavail(srv, node)

			node.Reset()
			reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			for _, verb := range []string{"--fail", "--remove", "--add"} {
				assertNoCall(t, node, "cmd mdadm "+mdDev+" "+verb)
			}
			assertOk(t,
				reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
				"grp data")
			legRow := reply.GetCntlrInfo().GetLegIdToLeg()[tc.legId]
			if tc.legErr != (legRow.GetStatus() ==
				pb.ResStatus_RES_STATUS_ERROR) {
				t.Fatalf("leg %#x's row read %v %q, want ERROR %v",
					tc.legId, legRow.GetStatus(), legRow.GetDetails(),
					tc.legErr)
			}
			if got := node.arrays[mdDev].members; !slices.Equal(got, held) {
				t.Fatalf("the array's members changed to %v, want %v",
					got, held)
			}
			if !retrying(t, srv) {
				t.Fatalf("an unusable leg_list leg registered no retry")
			}
		})
	}
}

// TestGroupHeldFaultyMemberStaysHeld pins the held set of CN12's member
// comparison: every member sysfs lists counts as held, whatever its
// dev-*/state. Leg 2's member reads "faulty,failfast" while its leg is
// available again, and an equal-revision converge runs no --fail, --remove
// or --add and reads OK — the known limit "a member md failed stays failed"
// (cnagent.md §7). A held set of in-sync members only would --add a member
// md still holds, which mdadm refuses (it opens the device O_EXCL, and md
// keeps its claim on a faulty member until the member is removed), and the
// group row would read ERROR on every converge; the follow-up that re-adds a
// faulty member must --remove it first. Nor is the faulty member late: only
// availability makes a member late (CN12), so the converge registers no CN10
// retry either.
func TestGroupHeldFaultyMemberStaysHeld(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	ctx := context.Background()
	opts := reqOpts{revision: 2, primary: true, raid1: true, twoLegs: true}
	syncupBoth(t, srv, opts)
	mdDev := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	held := slices.Clone(node.arrays[mdDev].members)
	if len(held) != 2 {
		t.Fatalf("the converged array holds %v, want both legs", held)
	}
	node.setMdSync(mdDev, 1, "idle", "none")
	node.setMemberState(mdDev, srv.nf.DmPath(legName(srv, testDataLeg2)),
		"faulty,failfast")
	detail, err := mdLookup(ctx, srv.md, legName(srv, testDataLeg2))
	if err != nil || !detail.Exists {
		t.Fatalf("the array read (%+v, %v)", detail, err)
	}
	faulty := false
	for _, member := range detail.Members {
		faulty = faulty || member.DmName == legName(srv, testDataLeg2) &&
			strings.Contains(member.State, "faulty")
	}
	if !faulty {
		t.Fatalf("leg 2's member read %+v, want it faulty; the case is "+
			"vacuous otherwise", detail.Members)
	}

	node.Reset()
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	for _, verb := range []string{"--fail", "--remove", "--add"} {
		assertNoCall(t, node, "cmd mdadm "+mdDev+" "+verb)
	}
	assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
		"grp data, leg 2 faulty")
	if got := node.arrays[mdDev].members; !slices.Equal(got, held) {
		t.Fatalf("the array's members changed to %v, want %v", got, held)
	}
	if retrying(t, srv) {
		t.Fatalf("a faulty member on an available leg registered the retry")
	}
}

// TestGroupNeverAddsAnUnavailableLeg pins the add loop's one guard now that
// members are compared by dm name (CN12): a leg_list member the array lacks
// is added only when its leg is available. Leg 2's wrapper is built but its
// path is non-optimized, so an --add would put a member on a path this pass
// may not use; the degraded array is left running on leg 1 and reads OK.
// reconcileMembers runs no lsblk for either leg. The late member registers
// the CN10 retry (CN12). Once the path is optimized again, the next converge
// adds it and, with no member late, stops the retry.
func TestGroupNeverAddsAnUnavailableLeg(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	ctx := context.Background()
	opts := reqOpts{revision: 2, primary: true, raid1: true, twoLegs: true}
	syncupBoth(t, srv, opts)
	mdDev := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	leg2 := srv.nf.DmPath(legName(srv, testDataLeg2))
	nqn2 := srv.nf.SideToCnNqn(testCluster, testSp, testDataLeg2, testCn)
	node.mu.Lock()
	array := node.arrays[mdDev]
	array.members = slices.DeleteFunc(slices.Clone(array.members),
		func(member string) bool { return member == leg2 })
	array.degraded = 1
	node.publishArray(array)
	node.mu.Unlock()
	node.setAnaState(nqn2, testIp2, testSvcId2, "non-optimized")

	node.Reset()
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	assertNoCall(t, node, "cmd mdadm "+mdDev+" --add")
	for _, legId := range []uint64{testDataLeg, testDataLeg2} {
		assertNoCall(t, node, "lsblk --nodeps --noheadings --output "+
			"MAJ:MIN "+srv.nf.DmPath(legName(srv, legId)))
	}
	assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
		"grp data, leg 2 unavailable")
	if !retrying(t, srv) {
		t.Fatalf("the unavailable member registered no retry")
	}

	// Non-vacuity: available again, the same converge adds it.
	node.setAnaState(nqn2, testIp2, testSvcId2, "optimized")
	node.Reset()
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(opts)); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if !node.hasCall("cmd mdadm " + mdDev + " --add --failfast " + leg2) {
		t.Fatalf("leg 2, available again, was not added")
	}
	if retrying(t, srv) {
		t.Fatalf("the retry outlived its last late member")
	}
}

// ---------------------------------------------------------------------------
// CN10/CN12 — a late leg_list member registers the background retry
// ---------------------------------------------------------------------------

// retrying reads the fixture cntlr's CN10 registration under s.mu, the lock
// that guards it.
func retrying(t *testing.T, srv *CnAgentServer) bool {
	t.Helper()
	st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	if st == nil {
		t.Fatalf("the fixture cntlr is not known")
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return st.retrying
}

// retryAttempt runs one attempt of the CN10 retry by hand: the call
// connectRetryLoop makes on each tick, on the ctx it makes it on. The tests
// that use it set a retryInterval no test outlives, so the loop itself never
// fires and every attempt is one the test ran.
func retryAttempt(t *testing.T, srv *CnAgentServer) {
	t.Helper()
	key := cntlrKey(testCluster, testCn, testSp, testCntlr)
	st := srv.getCntlr(key)
	if st == nil {
		t.Fatalf("the fixture cntlr is not known")
	}
	srv.reconvergeCntlr(srv.rootCtx, key, st)
}

// probedCntlrInfo is the fixture cntlr's GetCntlrInfo rows: how a test reads
// the outcome of a converge that, like a retry attempt, has no reply.
func probedCntlrInfo(t *testing.T, srv *CnAgentServer) *pb.CntlrInfo {
	t.Helper()
	reply, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	return reply.GetCntlrInfo()
}

// TestLateMembersRegisterTheRetry is link 1 of the failover ping-pong (CN12
// as amended 2026-09-26). A failover fans SyncupCntlr and the sides'
// SyncupSide out unordered ([D16]), so a promoted standby regularly reads its
// legs before the sides' ANA flips have reached its sysfs: the paths are live
// but still non-optimized, those legs are not available, and a group left
// with no available leg reports "no available leg" with no mdadm run.
// Nothing re-drives that converge — the worker re-syncs on a revision or a
// reply code, never on a row — so it registers the CN10 retry itself. A late
// member in any group registers it, not just one in the last group the pass
// converges: "meta only" leaves the meta group, which the pass converges
// before the data group, late and the data group whole. An attempt that
// still finds a member late keeps the retry, since the sides' flips can take
// longer than one CnConnectRetryInterval. Once the paths read optimized, the
// retry's next attempt assembles the late arrays and builds the stack above
// them, and, with no member late any more, stops the retry.
func TestLateMembersRegisterTheRetry(t *testing.T) {
	for _, tc := range []struct {
		name string
		// late are the legs whose paths read non-optimized until the flip,
		// lateGrps the groups that leaves with no available leg, and
		// wholeGrps the ones the promotion assembles all the same.
		late      []uint64
		lateGrps  []uint64
		wholeGrps []uint64
	}{
		{"meta and data", []uint64{testMetaLeg, testDataLeg},
			[]uint64{testMetaGrp, testDataGrp}, nil},
		{"meta only", []uint64{testMetaLeg},
			[]uint64{testMetaGrp}, []uint64{testDataGrp}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			ctx := context.Background()
			opts := func(revision uint64, primary bool) reqOpts {
				return reqOpts{revision: revision, primary: primary,
					raid1: true}
			}
			mdDevOf := map[uint64]string{
				testMetaGrp: srv.nf.MdPath(srv.nf.CnMdDevName(
					testCluster, testCn, testSp, 0, 0, true)),
				testDataGrp: srv.nf.MdPath(srv.nf.CnMdDevName(
					testCluster, testCn, testSp, 0, 0, false)),
			}
			assertAssembled := func(grpIds []uint64, label string) {
				t.Helper()
				assembled := node.callsMatching("cmd mdadm --assemble")
				ok := len(assembled) == len(grpIds)
				for _, grpId := range grpIds {
					ok = ok && len(node.callsMatching(
						"cmd mdadm --assemble "+mdDevOf[grpId]+" ")) == 1
				}
				if !ok {
					t.Fatalf("%s: ran %q, want one mdadm --assemble of "+
						"each of groups %v", label, assembled, grpIds)
				}
				assertNoCall(t, node, "cmd mdadm --create")
			}
			syncupBoth(t, srv, opts(2, true))
			if _, err := srv.SyncupCntlr(ctx,
				cntlrReq(opts(3, false))); err != nil {
				t.Fatalf("demote: %v", err)
			}
			setAna := func(state string) {
				for _, legId := range tc.late {
					node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp,
						legId, testCn), testIp, testSvcId, state)
				}
			}
			setAna("non-optimized")

			node.Reset()
			reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts(4, true)))
			if err != nil {
				t.Fatalf("promote: %v", err)
			}
			if reply.GetAgentReply().GetCode() != 0 {
				t.Fatalf("rejected: %v", reply.GetAgentReply())
			}
			rows := reply.GetCntlrInfo().GetGrpIdToMdRaid()
			for _, grpId := range tc.lateGrps {
				assertErrorDetails(t, rows[grpId], "no available leg",
					fmt.Sprintf("grp %d before the flip", grpId))
			}
			for _, grpId := range tc.wholeGrps {
				assertOk(t, rows[grpId],
					fmt.Sprintf("grp %d before the flip", grpId))
			}
			assertAssembled(tc.wholeGrps, "the promotion")
			if !retrying(t, srv) {
				t.Fatalf("a promotion that found a group with no available " +
					"leg registered no retry: nothing else would ever " +
					"assemble it")
			}

			// An attempt that still finds a member late keeps the retry: the
			// sides' flips can take longer than one CnConnectRetryInterval.
			node.Reset()
			retryAttempt(t, srv)
			assertAssembled(nil, "an attempt before the flip")
			if !retrying(t, srv) {
				t.Fatalf("an attempt that still found a member late " +
					"stopped the retry")
			}

			setAna("optimized")
			node.Reset()
			retryAttempt(t, srv)
			assertAssembled(tc.lateGrps, "the attempt after the flip")
			info := probedCntlrInfo(t, srv)
			assertOk(t, info.GetGrpIdToMdRaid()[testMetaGrp],
				"grp meta after the flip")
			assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp],
				"grp data after the flip")
			assertOk(t, info.GetTdIdToRaid0()[testTd], "raid0 after the flip")
			assertOk(t, info.GetNsIdToDmLinear()[testNs],
				"ns-dev after the flip")
			if retrying(t, srv) {
				t.Fatalf("the retry outlived its last late member")
			}
		})
	}
}

// TestLateMembersProbeMissingWithoutTd pins the agent half of the worker's
// settle (dnv-worker.md HL2, primaryShapeBuilt): a primary with no td whose
// promotion found its members late reads its groups and pool ERROR in the
// SyncupCntlr reply, but a Check round before the retry's first attempt
// probes those devices absent and reads them MISSING "", not ERROR. That
// reply, at the driven revision with an accepted code, then carries no ERROR
// and no PROVISIONING row outside the leg rows: only its MISSING rows say the
// stack is not built. A td would keep an ERROR row, its raid0 over the absent
// thins, which is why this SP has none.
func TestLateMembersProbeMissingWithoutTd(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	ctx := context.Background()
	opts := func(revision uint64, primary bool) reqOpts {
		return reqOpts{revision: revision, primary: primary, raid1: true,
			tds: []*pb.ThinDevice{}, subsys: map[string]*pb.Subsystem{}}
	}
	syncupBoth(t, srv, opts(2, true))
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(opts(3, false))); err != nil {
		t.Fatalf("demote: %v", err)
	}
	for _, legId := range []uint64{testMetaLeg, testDataLeg} {
		node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp, legId,
			testCn), testIp, testSvcId, "non-optimized")
	}
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts(4, true)))
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	for _, grpId := range []uint64{testMetaGrp, testDataGrp} {
		assertErrorDetails(t,
			reply.GetCntlrInfo().GetGrpIdToMdRaid()[grpId],
			"no available leg", fmt.Sprintf("grp %d in the promotion", grpId))
	}
	if !retrying(t, srv) {
		t.Fatalf("the promotion registered no retry")
	}

	check, info := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
		ClusterId: testCluster, CnId: testCn,
		CntlrPointer: cntlrPtr(), Revision: 4,
	}, nil)
	if code := check.GetAgentReply().GetCode(); (code != 0 &&
		code != common.ReplyCodeLeftover) || check.GetRevision() != 4 {
		t.Fatalf("check round: code %d, revision %d, want an accepted "+
			"code at revision 4", code, check.GetRevision())
	}
	for label, res := range map[string]*pb.ResInfo{
		"grp meta":  info.GetGrpIdToMdRaid()[testMetaGrp],
		"grp data":  info.GetGrpIdToMdRaid()[testDataGrp],
		"pool meta": info.GetSliceIdToMeta()[testSlice],
		"pool data": info.GetSliceIdToData()[testSlice],
		"pool":      info.GetSliceIdToDmPool()[testSlice],
	} {
		if res.GetStatus() != pb.ResStatus_RES_STATUS_MISSING ||
			res.GetDetails() != "" {
			t.Fatalf("probed %s: %v/%q, want MISSING/\"\"", label,
				res.GetStatus(), res.GetDetails())
		}
	}
	rows := []map[uint64]*pb.ResInfo{
		info.GetSsIdToSubsystem(), info.GetNsIdToNamespace(),
		info.GetNsIdToDmLinear(), info.GetTdIdToRaid0(),
		info.GetTdIdToDmError(), info.GetSliceIdToDmPool(),
		info.GetSliceIdToMeta(), info.GetSliceIdToData(),
		info.GetGrpIdToMdRaid(), info.GetXferIdToDmLinear(),
		info.GetXferIdToSubsystem(), info.GetXferIdToNamespace(),
		info.GetCloneIdToTarget(), info.GetCloneIdToDmClone(),
		info.GetCloneIdToMeta(),
	}
	for _, thin := range info.GetTdIdToThinInfo() {
		rows = append(rows, thin.GetSliceIdToDmThin())
	}
	for _, m := range rows {
		for id, res := range m {
			switch res.GetStatus() {
			case pb.ResStatus_RES_STATUS_ERROR,
				pb.ResStatus_RES_STATUS_PROVISIONING:
				t.Fatalf("probed row %d (%s): %v/%q, want neither ERROR "+
					"nor PROVISIONING", id, res.GetResName(),
					res.GetStatus(), res.GetDetails())
			}
		}
	}
}

// TestLateMemberRefusedStartIsAssembledByTheRetry is
// TestLateMembersRegisterTheRetry with one member of a two-leg group late
// after a clean demote, the shape a failover leaves ([D16]). The demote stops
// the array with both superblocks still counting both members, so case 2's
// start from the available member alone, without --run, is refused: the
// group reads ERROR with mdadm's own words, and the late member registers the
// retry. An attempt while it is still late is refused the same way, runs no
// --create and keeps the retry; the attempt after its path reads optimized
// assembles the array from both members, adds nothing and stops the retry.
// The fake has no Array State gate — it starts an array from any member that
// carries a superblock — so the refusal is injected, as
// TestGroupAssembleRefusalIsAnError does, for as long as the member is late;
// like mdadm, which stops an array whose start it refused, the injected
// refusal leaves no array.
func TestLateMemberRefusedStartIsAssembledByTheRetry(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	ctx := context.Background()
	opts := func(revision uint64, primary bool) reqOpts {
		return reqOpts{revision: revision, primary: primary, raid1: true,
			twoLegs: true}
	}
	syncupBoth(t, srv, opts(2, true))
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(opts(3, false))); err != nil {
		t.Fatalf("demote: %v", err)
	}
	mdDev := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	leg1 := srv.nf.DmPath(legName(srv, testDataLeg))
	leg2 := srv.nf.DmPath(legName(srv, testDataLeg2))
	nqn2 := srv.nf.SideToCnNqn(testCluster, testSp, testDataLeg2, testCn)
	node.setAnaState(nqn2, testIp2, testSvcId2, "non-optimized")
	start := "mdadm --assemble " + mdDev + " "
	node.mu.Lock()
	node.failCmdAlways[start] = "mdadm: " + mdDev + " assembled from 1 " +
		"drive - need 2 to start (use --run to insist)."
	node.mu.Unlock()

	node.Reset()
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts(4, true)))
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	assertErrorDetails(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
		"need 2 to start", "grp data, leg 2 late")
	assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testMetaGrp],
		"grp meta")
	started := node.callsMatching("cmd " + start)
	if len(started) != 1 || !strings.Contains(started[0], leg1) ||
		strings.Contains(started[0], leg2) {
		t.Fatalf("want one start of the data array from leg 1 alone, "+
			"got %q", started)
	}
	assertNoCall(t, node, "cmd mdadm --create")
	if !retrying(t, srv) {
		t.Fatalf("a refused start beside a late member registered no " +
			"retry: nothing else would ever assemble the array")
	}

	// An attempt that still finds the member late is refused the same way
	// and keeps the retry.
	node.Reset()
	retryAttempt(t, srv)
	if got := len(node.callsMatching("cmd " + start)); got != 1 {
		t.Fatalf("an attempt before the flip ran %d starts of the data "+
			"array, want 1", got)
	}
	assertNoCall(t, node, "cmd mdadm --create")
	if !retrying(t, srv) {
		t.Fatalf("an attempt that still found a member late stopped the " +
			"retry")
	}

	node.mu.Lock()
	delete(node.failCmdAlways, start)
	node.mu.Unlock()
	node.setAnaState(nqn2, testIp2, testSvcId2, "optimized")
	node.Reset()
	retryAttempt(t, srv)
	started = node.callsMatching("cmd " + start)
	if len(started) != 1 || !strings.Contains(started[0], leg1) ||
		!strings.Contains(started[0], leg2) {
		t.Fatalf("want one assembly of the data array from both legs, "+
			"got %q:\n%s", started, strings.Join(node.Calls(), "\n"))
	}
	assertNoCall(t, node, "cmd mdadm "+mdDev+" --add")
	if got := node.arrays[mdDev].members; !slices.Equal(got,
		[]string{leg1, leg2}) {
		t.Fatalf("the array holds %v, want both legs", got)
	}
	info := probedCntlrInfo(t, srv)
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data after the flip")
	assertOk(t, info.GetNsIdToDmLinear()[testNs], "ns-dev after the flip")
	if retrying(t, srv) {
		t.Fatalf("the retry outlived its last late member")
	}
}

// TestLateMemberIsAddedByTheRetry is the other shape of one late member in a
// two-leg group: md had failed and removed leg 2's member on the old primary
// before the demote stopped the array, so leg 1's superblock no longer
// counts it, and case 2's start from leg 1 alone is one mdadm allows
// (§11.1.1). The promotion starts the array degraded and reports the group
// OK, but leg 2 — late — still has to be added, and that --add is the
// retry's: an attempt while it is still late adds nothing and keeps the
// retry, and the attempt after its path reads optimized adds it and stops
// the retry. (The fake starts an array from any member that carries a
// superblock; the --fail and --remove are what make the fixture the shape
// that outcome belongs to.)
func TestLateMemberIsAddedByTheRetry(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	ctx := context.Background()
	opts := func(revision uint64, primary bool) reqOpts {
		return reqOpts{revision: revision, primary: primary, raid1: true,
			twoLegs: true}
	}
	syncupBoth(t, srv, opts(2, true))
	mdDev := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	leg1 := srv.nf.DmPath(legName(srv, testDataLeg))
	leg2 := srv.nf.DmPath(legName(srv, testDataLeg2))
	nqn2 := srv.nf.SideToCnNqn(testCluster, testSp, testDataLeg2, testCn)
	if err := srv.md.Fail(ctx, mdDev, leg2); err != nil {
		t.Fatalf("--fail leg 2: %v", err)
	}
	if err := srv.md.Remove(ctx, mdDev, leg2); err != nil {
		t.Fatalf("--remove leg 2: %v", err)
	}
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(opts(3, false))); err != nil {
		t.Fatalf("demote: %v", err)
	}
	node.setAnaState(nqn2, testIp2, testSvcId2, "non-optimized")

	node.Reset()
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts(4, true)))
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
		"grp data, leg 2 late")
	assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testMetaGrp],
		"grp meta")
	assembled := node.callsMatching("cmd mdadm --assemble " + mdDev + " ")
	if len(assembled) != 1 || !strings.Contains(assembled[0], leg1) ||
		strings.Contains(assembled[0], leg2) {
		t.Fatalf("want one assembly of the data array from leg 1 alone, "+
			"got %q", assembled)
	}
	assertNoCall(t, node, "cmd mdadm "+mdDev+" --add")
	if !retrying(t, srv) {
		t.Fatalf("an array assembled without a late member registered no " +
			"retry: nothing else would ever add it")
	}

	// An attempt that still finds the member late adds nothing and keeps
	// the retry.
	node.Reset()
	retryAttempt(t, srv)
	assertNoCall(t, node, "cmd mdadm "+mdDev+" --add")
	assertNoCall(t, node, "cmd mdadm --assemble")
	if !retrying(t, srv) {
		t.Fatalf("an attempt that still found a member late stopped the " +
			"retry")
	}

	node.setAnaState(nqn2, testIp2, testSvcId2, "optimized")
	node.Reset()
	retryAttempt(t, srv)
	if !node.hasCall("cmd mdadm " + mdDev + " --add --failfast " + leg2) {
		t.Fatalf("the retry did not add leg 2:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	assertNoCall(t, node, "cmd mdadm --assemble")
	if got := node.arrays[mdDev].members; !slices.Equal(got,
		[]string{leg1, leg2}) {
		t.Fatalf("the array holds %v, want both legs", got)
	}
	if retrying(t, srv) {
		t.Fatalf("the retry outlived its last late member")
	}
}

// TestLateRedundNoneLegRegistersTheRetry is link 1 for a RedundNone SP,
// redund_conf's default (CN12 as amended 2026-09-26). The group's dm-linear
// is built whatever its leg's availability, but the pool create above it
// reads the pool's metadata through the meta group's leg, and a side that
// has not flipped to this CN yet exports dm-error to it: the promotion's
// pool create fails, and only a later converge builds the pool and the
// stack above it. So an unavailable RedundNone leg registers the CN10 retry
// as an md member does; an attempt while it is still late keeps it, and the
// attempt after the paths read optimized stops it. The fake does not model
// dm-error IO — it builds the pool over a non-optimized leg — so this pins
// the registration, not the pool failure.
func TestLateRedundNoneLegRegistersTheRetry(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	if _, err := srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 3})); err != nil {
		t.Fatalf("demote: %v", err)
	}
	setAna := func(state string) {
		for _, legId := range []uint64{testMetaLeg, testDataLeg} {
			node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp, legId,
				testCn), testIp, testSvcId, state)
		}
	}
	setAna("non-optimized")

	node.Reset()
	reply, err := srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 4, primary: true}))
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if !node.hasCall("cmd dmsetup create " + grpName(srv, testDataGrp)) {
		t.Fatalf("the RedundNone group linear was not built")
	}
	assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
		"grp data, its leg late")
	if !retrying(t, srv) {
		t.Fatalf("a promotion over late RedundNone legs registered no " +
			"retry: nothing else would ever build its pool")
	}

	node.Reset()
	retryAttempt(t, srv)
	if !retrying(t, srv) {
		t.Fatalf("an attempt that still found a leg late stopped the retry")
	}

	setAna("optimized")
	node.Reset()
	retryAttempt(t, srv)
	info := probedCntlrInfo(t, srv)
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data after the flip")
	assertOk(t, info.GetSliceIdToDmPool()[testSlice], "pool after the flip")
	assertOk(t, info.GetNsIdToDmLinear()[testNs], "ns-dev after the flip")
	if retrying(t, srv) {
		t.Fatalf("the retry outlived its last late leg")
	}
}

// TestLateMemberRetryScope pins what never counts as a late member (CN12):
// each case holds an unavailable leg that TestLateMembersRegisterTheRetry
// shows would register the retry as a member of a wanted group. A standby
// wants no group (§3.4) — its non-optimized paths are its designed steady
// state — and nor does a primary whose sp_level suppresses its groups
// (CN19): at NO_SIDE, where no leg is wanted, every member would otherwise
// read unavailable. A deferred group builds no array at all ([D15]), even
// when its leg_list holds a provisioned member that is unavailable, and a
// spare is never a member (§8.12).
func TestLateMemberRetryScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup builds the cntlr with the unavailable leg in place.
		setup func(t *testing.T, srv *CnAgentServer, node *fakeNode)
	}{
		{"standby", func(t *testing.T, srv *CnAgentServer, node *fakeNode) {
			for _, leg := range []struct {
				legId       uint64
				addr, svcId string
			}{
				{testMetaLeg, testIp, testSvcId},
				{testDataLeg, testIp, testSvcId},
				{testDataLeg2, testIp2, testSvcId2},
			} {
				node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp,
					leg.legId, testCn), leg.addr, leg.svcId, "non-optimized")
			}
			reply := syncupBoth(t, srv, reqOpts{revision: 2, raid1: true,
				twoLegs: true})
			assertOk(t, reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg2],
				"standby leg 2, non-optimized")
			assertNoCall(t, node, "cmd mdadm")
		}},
		{"NO_SIDE primary", func(t *testing.T, srv *CnAgentServer,
			node *fakeNode) {
			reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				raid1: true, twoLegs: true,
				level: pb.SpLevel_SP_LEVEL_NO_SIDE})
			assertMissingSpLevel(t,
				reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
				"grp data")
		}},
		{"NO_REDUND primary", func(t *testing.T, srv *CnAgentServer,
			node *fakeNode) {
			node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp,
				testDataLeg2, testCn), testIp2, testSvcId2, "non-optimized")
			reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				raid1: true, twoLegs: true,
				level: pb.SpLevel_SP_LEVEL_NO_REDUND})
			assertMissingSpLevel(t,
				reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
				"grp data")
		}},
		{"deferred group", func(t *testing.T, srv *CnAgentServer,
			node *fakeNode) {
			node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp,
				testDataLeg2, testCn), testIp2, testSvcId2, "non-optimized")
			reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				raid1: true, twoLegs: true, unprovisionedDataLeg: true})
			info := reply.GetCntlrInfo()
			assertProvisioning(t, info.GetGrpIdToMdRaid()[testDataGrp],
				"grp data")
			// Leg 2 is provisioned, connected and wrapped: a member that is
			// not available, not a provisioning one.
			assertPending(t, info.GetLegIdToLeg()[testDataLeg2], "leg 2")
			assertOk(t, info.GetGrpIdToMdRaid()[testMetaGrp], "grp meta")
		}},
		{"spare", func(t *testing.T, srv *CnAgentServer, node *fakeNode) {
			node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp,
				switchSpareLeg, testCn), testIp2, testSvcId2, "non-optimized")
			cnSyncup(t, srv, 2, true)
			reply, err := srv.SyncupCntlr(context.Background(),
				switchSpareReq(2, false))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if !node.hasCall(
				"cmd dmsetup create " + legName(srv, switchSpareLeg)) {
				t.Fatalf("the spare leg was not wrapped")
			}
			assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
				"grp data")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			tc.setup(t, srv, node)
			if retrying(t, srv) {
				t.Fatalf("%s: an unavailable leg registered the retry",
					tc.name)
			}
		})
	}
}

// TestLateMemberRetryScopeWantedGroups pins the other edge of that scope
// (CN12): a late member of every group the pass builds counts, however little
// the pass builds above that group. A primary at NO_THINPOOL wants its groups
// but no pool (CN19): a group left unassembled reads ERROR, and without the
// retry it waits for the cntlr's next converge for some other reason. A group
// after [D15]'s prefix cut is not a concat target yet, but it is not deferred
// either: it is built, so its member counts all the same — only gp.deferred
// excludes a group, and effective() is not its negation.
func TestLateMemberRetryScopeWantedGroups(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup builds the cntlr with the late member in place.
		setup func(t *testing.T, srv *CnAgentServer, node *fakeNode)
	}{
		{"NO_THINPOOL primary", func(t *testing.T, srv *CnAgentServer,
			node *fakeNode) {
			node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp,
				testDataLeg2, testCn), testIp2, testSvcId2, "non-optimized")
			reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				raid1: true, twoLegs: true,
				level: pb.SpLevel_SP_LEVEL_NO_THINPOOL})
			assertErrorDetails(t,
				reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
				"only 1 of 2 legs available", "grp data, leg 2 late")
		}},
		{"group past the [D15] cut", func(t *testing.T, srv *CnAgentServer,
			node *fakeNode) {
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			if retrying(t, srv) {
				t.Fatalf("the fixture registered the retry before the grow")
			}
			node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp,
				testDataLeg3, testCn), testIp2, testSvcId2, "non-optimized")
			// TestOutOfOrderGrowDefersEveryLaterGroup's grow: group 2 is
			// deferred, which cuts group 3 out of the concat.
			grow := cntlrReq(reqOpts{revision: 3, primary: true})
			grownDataGrp(grow, false)
			grownDataGrp3(grow, true)
			reply, err := srv.SyncupCntlr(context.Background(), grow)
			if err != nil {
				t.Fatalf("out-of-order grow: %v", err)
			}
			info := reply.GetCntlrInfo()
			assertProvisioning(t, info.GetGrpIdToMdRaid()[testDataGrp2],
				"grp grown 1")
			// Built, not a concat target yet.
			assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp3],
				"grp grown 2, its leg late")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			tc.setup(t, srv, node)
			if !retrying(t, srv) {
				t.Fatalf("%s: a late member of a group the pass builds "+
					"registered no retry", tc.name)
			}
		})
	}
}

// TestFailedConnectRegistersTheRetryWithoutALateMember pins ensureLegs' own
// registration (CN10): a leg whose connect fails registers the retry whether
// or not it is a late member. Each case's failing leg is never one — a
// standby wants no group, and a spare is never a member — so the retry is
// ensureLegs' alone, and build must OR the late-member verdict into
// ensureLegs', not overwrite it.
func TestFailedConnectRegistersTheRetryWithoutALateMember(t *testing.T) {
	refuse := func(srv *CnAgentServer, node *fakeNode, legId uint64) {
		node.mu.Lock()
		defer node.mu.Unlock()
		node.failCmdAlways["--nqn "+srv.nf.SideToCnNqn(testCluster, testSp,
			legId, testCn)+" --hostnqn"] = "nvme connect: Connection refused"
	}
	for _, tc := range []struct {
		name  string
		legId uint64
		// setup refuses the leg's connect and converges the cntlr.
		setup func(t *testing.T, srv *CnAgentServer,
			node *fakeNode) *pb.SyncupCntlrReply
	}{
		{"standby", testDataLeg2, func(t *testing.T, srv *CnAgentServer,
			node *fakeNode) *pb.SyncupCntlrReply {
			refuse(srv, node, testDataLeg2)
			return syncupBoth(t, srv, reqOpts{revision: 2, raid1: true,
				twoLegs: true})
		}},
		{"spare", switchSpareLeg, func(t *testing.T, srv *CnAgentServer,
			node *fakeNode) *pb.SyncupCntlrReply {
			refuse(srv, node, switchSpareLeg)
			cnSyncup(t, srv, 2, true)
			reply, err := srv.SyncupCntlr(context.Background(),
				switchSpareReq(2, false))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
				"grp data")
			return reply
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			reply := tc.setup(t, srv, node)
			legRow := reply.GetCntlrInfo().GetLegIdToLeg()[tc.legId]
			if legRow.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
				t.Fatalf("leg %#x's row read %v %q, want ERROR: the "+
					"connect did not fail", tc.legId, legRow.GetStatus(),
					legRow.GetDetails())
			}
			if !retrying(t, srv) {
				t.Fatalf("a failed connect registered no retry: build must " +
					"OR the late-member verdict into ensureLegs', not " +
					"overwrite it")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// §6.8 — namespace states (CN16)
// ---------------------------------------------------------------------------

// TestNamespaceSuspend is §11.6 as amended 2026-09-16: an effectively
// suspended namespace is **parked**, never dm-suspended. Both directions pin
// the reload's own `--table` — the device the ns-dev ends up on — and the
// order against the ANA write, which is what keeps a host from ever reaching
// a table that has stopped serving.
func TestNamespaceSuspend(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, suspended: true})); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	errNo := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
	assertOrder(t, node,
		"writedirect "+anaPath(testNqn, 1)+"=3",
		"cmd dmsetup reload "+nsDevName(srv, testNs)+" --table "+
			agent.LinearTable(testTdSize/512, errNo, 0),
	)
	assertParked(t, srv, node, testNs, testTd, "suspend")

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 4, primary: true})); err != nil {
		t.Fatalf("resume: %v", err)
	}
	raid0No := node.devNo["/dev/mapper/"+raid0Name(srv, testTd)]
	assertOrder(t, node,
		"cmd dmsetup reload "+nsDevName(srv, testNs)+" --table "+
			agent.LinearTable(testTdSize/512, raid0No, 0),
		"writedirect "+anaPath(testNqn, 1)+"=1",
	)
	if dev := node.dms[nsDevName(srv, testNs)]; dev.suspended {
		t.Fatalf("the unparked ns-dev is dm-suspended")
	}
}

// TestParkIsIdempotent is SH16 for the park. The old suspend branch was
// trivially idempotent (`np.suspended && !dev.Suspended`); the park's
// idempotence rests on `parkNsDev`'s own `parked` predicate, which the retire
// phase reaches first for an effectively suspended namespace — so a drift
// there would reload a live, correct ns-dev on every converge round the worker
// drives. (`ensureNsDev`'s `nsDevTableMatches` is the second gate and is
// already satisfied by the time it runs here.)
func TestParkIsIdempotent(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, suspended: true})
	assertParked(t, srv, node, testNs, testTd, "fixture")

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, suspended: true})); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	for _, fragment := range []string{
		"cmd dmsetup reload " + nsDevName(srv, testNs),
		"cmd dmsetup suspend " + nsDevName(srv, testNs),
		"cmd dmsetup resume " + nsDevName(srv, testNs),
	} {
		assertNoCall(t, node, fragment)
	}
	assertParked(t, srv, node, testNs, testTd, "re-apply")
}

// TestParkedNamespaceProbe is the CN28 row of a parked ns-dev: `OK, parked`.
// A dm-suspended ns-dev is an ERROR whatever the plan says — nothing this
// build produces one, so finding one is a fault to report and not a steady
// state, which is the direction the old `!= dev.Suspended` comparison had
// backwards for an effectively suspended namespace.
func TestParkedNamespaceProbe(t *testing.T) {
	nsDevRow := func(t *testing.T, srv *CnAgentServer) *pb.ResInfo {
		t.Helper()
		reply, err := srv.GetCntlrInfo(context.Background(),
			&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
				CntlrPointer: cntlrPtr()})
		if err != nil {
			t.Fatalf("GetCntlrInfo: %v", err)
		}
		return reply.GetCntlrInfo().GetNsIdToDmLinear()[testNs]
	}

	// The other side of both carriers: a SERVING ns-dev reports OK with EMPTY
	// details. Without it, `nsDevDetails` and `probeNsDev` could return
	// "parked" unconditionally and every other assertion in the package would
	// still pass — assertOk compares the status only.
	t.Run("serving", func(t *testing.T) {
		srv, _ := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
		row := nsDevRow(t, srv)
		if row.GetStatus() != pb.ResStatus_RES_STATUS_OK ||
			row.GetDetails() != "" {
			t.Fatalf("probed serving ns-dev row: %v/%q",
				row.GetStatus(), row.GetDetails())
		}
		reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
			revision: 3, primary: true}))
		if err != nil {
			t.Fatalf("re-converge: %v", err)
		}
		row = reply.GetCntlrInfo().GetNsIdToDmLinear()[testNs]
		if row.GetStatus() != pb.ResStatus_RES_STATUS_OK ||
			row.GetDetails() != "" {
			t.Fatalf("converged serving ns-dev row: %v/%q",
				row.GetStatus(), row.GetDetails())
		}
	})

	t.Run("parked", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{
			revision: 2, primary: true, suspended: true})
		assertParked(t, srv, node, testNs, testTd, "fixture")
		row := nsDevRow(t, srv)
		if row.GetStatus() != pb.ResStatus_RES_STATUS_OK ||
			row.GetDetails() != "parked" {
			t.Fatalf("probed parked ns-dev row: %v/%q",
				row.GetStatus(), row.GetDetails())
		}
		// The converge reply says the same thing. It is a second code path
		// (`nsDevDetails`, not `probeNsDev`), so it needs its own pin —
		// without one, a converge that still reported "suspended" while the
		// probe said "parked" would go green.
		reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
			revision: 3, primary: true, suspended: true}))
		if err != nil {
			t.Fatalf("re-converge: %v", err)
		}
		row = reply.GetCntlrInfo().GetNsIdToDmLinear()[testNs]
		if row.GetStatus() != pb.ResStatus_RES_STATUS_OK ||
			row.GetDetails() != "parked" {
			t.Fatalf("converged parked ns-dev row: %v/%q",
				row.GetStatus(), row.GetDetails())
		}
	})

	// Rule 1 returns flakey = false unconditionally, and the read-only level is
	// the only thing that could make it true. Without this sub-case that
	// `false` is unobserved: turning it into `p.readOnly` leaves the package
	// green, and a parked ns-dev at SP_LEVEL_READONLY would quietly become a
	// dm-flakey table nothing in the tree describes.
	t.Run("parked is never under dm-flakey", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{
			revision: 2, primary: true, suspended: true,
			level: pb.SpLevel_SP_LEVEL_READONLY})
		assertParked(t, srv, node, testNs, testTd, "readonly + parked")
	})

	for _, tc := range []struct {
		name      string
		suspended bool
	}{
		{"suspended, plan says parked", true},
		{"suspended, plan says serving", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{
				revision: 2, primary: true, suspended: tc.suspended})
			node.dms[nsDevName(srv, testNs)].suspended = true
			row := nsDevRow(t, srv)
			if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
				row.GetDetails() != "unexpectedly suspended" {
				t.Fatalf("row: %v/%q", row.GetStatus(), row.GetDetails())
			}
		})
	}
}

// TestSuspendedNsDevFromAnOlderBuildIsResumed is the upgrade path (§1
// Compatibility): the three shapes in which an agent that never suspends can
// still meet a suspended ns-dev, and how each converges on the first pass.
func TestSuspendedNsDevFromAnOlderBuildIsResumed(t *testing.T) {
	// leftover re-creates what a pre-2026-09-16 agent (or an interrupted
	// reload) left behind, and returns the count of ns-dev reloads and resumes
	// the next converge issues.
	leftover := func(
		t *testing.T,
		suspended bool,
		table func(srv *CnAgentServer, node *fakeNode) string,
	) (*CnAgentServer, *fakeNode, int, int) {
		t.Helper()
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{
			revision: 2, primary: true, suspended: suspended})
		dev := node.dms[nsDevName(srv, testNs)]
		dev.table = table(srv, node)
		dev.suspended = true
		node.Reset()
		if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
			revision: 3, primary: true, suspended: suspended})); err != nil {
			t.Fatalf("converge: %v", err)
		}
		nsDev := nsDevName(srv, testNs)
		return srv, node,
			len(node.callsMatching("cmd dmsetup reload " + nsDev)),
			len(node.callsMatching("cmd dmsetup resume " + nsDev))
	}
	raid0Table := func(srv *CnAgentServer, node *fakeNode) string {
		return agent.LinearTable(testTdSize/512,
			node.devNo["/dev/mapper/"+raid0Name(srv, testTd)], 0)
	}
	errorTable := func(srv *CnAgentServer, node *fakeNode) string {
		return agent.LinearTable(testTdSize/512,
			node.devNo["/dev/mapper/"+errorName(srv, testTd)], 0)
	}

	// (a) Effectively suspended, still on the raid0 the older build left it
	// holding: one reload, onto the error backing, and the device ends live.
	t.Run("effectively suspended, raid0 table", func(t *testing.T) {
		srv, node, reloads, _ := leftover(t, true, raid0Table)
		if reloads != 1 {
			t.Fatalf("want exactly one ns-dev reload, got %d", reloads)
		}
		assertParked(t, srv, node, testNs, testTd, "older build, raid0 table")
	})

	// (b) Effectively suspended and already on the error table, but held
	// suspended: the park's own reload resumes it. A device left in this state
	// is what an interrupted `Reload` produces.
	t.Run("effectively suspended, error table", func(t *testing.T) {
		srv, node, reloads, _ := leftover(t, true, errorTable)
		if reloads != 1 {
			t.Fatalf("want exactly one ns-dev reload, got %d", reloads)
		}
		assertParked(t, srv, node, testNs, testTd, "older build, error table")
	})

	// (c) Serving, with the table it wants, suspended: a bare `dmsetup
	// resume`, no reload. This is the one remaining reason `ensureNsDev` looks
	// at `dev.Suspended` at all — delete that guard and this sub-case is the
	// only thing that notices.
	t.Run("serving, desired table", func(t *testing.T) {
		srv, node, reloads, resumes := leftover(t, false, raid0Table)
		if reloads != 0 {
			t.Fatalf("want no ns-dev reload, got %d", reloads)
		}
		if resumes != 1 {
			t.Fatalf("want exactly one bare ns-dev resume, got %d", resumes)
		}
		dev := node.dms[nsDevName(srv, testNs)]
		if dev.suspended || dev.table != raid0Table(srv, node) {
			t.Fatalf("ns-dev is %q, suspended=%v", dev.table, dev.suspended)
		}
	})

	// (d) The same guard one layer down, in `ensureDmSingle`: the td's raid0
	// found suspended with its correct table. The park ([D12]) made that
	// resume unconditional by deleting `keepSuspended`, and nothing else in
	// the package converges a suspended raid0, pool or thin volume — so
	// without this sub-case the guard could be dropped as dead and the next
	// converge after a killed agent would leave the stack wedged under a
	// live ns-dev.
	t.Run("a suspended raid0 is resumed", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
		raid0 := raid0Name(srv, testTd)
		node.dms[raid0].suspended = true
		// The asymmetry the park introduced, pinned on the fixture that
		// already exists: an ns-dev found suspended is an ERROR whatever
		// the plan says, while every OTHER dm device still probes OK with
		// `details = "suspended"`. Deliberate — only the ns-dev has a plan
		// state that used to expect it.
		probe, err := srv.GetCntlrInfo(context.Background(),
			&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
				CntlrPointer: cntlrPtr()})
		if err != nil {
			t.Fatalf("GetCntlrInfo: %v", err)
		}
		row := probe.GetCntlrInfo().GetTdIdToRaid0()[testTd]
		if row.GetStatus() != pb.ResStatus_RES_STATUS_OK ||
			row.GetDetails() != "suspended" {
			t.Fatalf("suspended raid0 row: %v/%q",
				row.GetStatus(), row.GetDetails())
		}
		node.Reset()
		if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
			revision: 3, primary: true})); err != nil {
			t.Fatalf("converge: %v", err)
		}
		if got := len(node.callsMatching(
			"cmd dmsetup resume " + raid0)); got != 1 {
			t.Fatalf("want exactly one bare raid0 resume, got %d", got)
		}
		assertNoCall(t, node, "cmd dmsetup reload "+raid0)
		if node.dms[raid0].suspended {
			t.Fatalf("the raid0 is still suspended")
		}
	})
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
	// The transfer-driven twin of TestNamespaceSuspend. Both steps are the
	// sweep's pre-steps (CN9): CN16 rule 1 makes the origin's backing the
	// td's dm-error, so pre-step 2 parks it — ahead of the build phase, which
	// is where the xfer device is created. What §11.3 makes
	// load-bearing is the first edge: ANA inaccessible *before* the device is
	// touched, so no host IO is behind the reload.
	errNo := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
	assertOrder(t, node,
		"writedirect "+anaPath(testNqn, 1)+"=3",
		"cmd dmsetup reload "+nsDevName(srv, testNs)+" --table "+
			agent.LinearTable(testTdSize/512, errNo, 0),
		"cmd dmsetup create "+xferName(srv, testXfer),
	)
	assertParked(t, srv, node, testNs, testTd, "auto_suspend origin")
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
// §6.26 — park before remove (CN9/CN21)
// ---------------------------------------------------------------------------

// TestRemovedSuspendedNamespaceIsParkedBeforeNvmetRemoval pins CN9's retire
// order for a namespace that *leaves* the desired state. CN21's rationale is
// that a dm-suspended device blocks both the nvmet disable above it and its
// own removal. Since 2026-09-16 (§11.6, [D12]) this agent never leaves one
// suspended, so the only way a teardown still meets one is an **older build's
// leftover** — which is exactly what the two ordering sub-cases fixture. The
// park — the reload onto the td's `CnErrorName`, whose internal resume is the
// whole point — therefore has to precede the nvmet removal, not follow it
// inside `removeDm`. The third sub-case is the steady state: an already
// parked namespace needs no reload at all.
func TestRemovedSuspendedNamespaceIsParkedBeforeNvmetRemoval(t *testing.T) {
	// Converge A: a primary serving one deliberately suspended namespace. It
	// ends *parked* — live on the td's dm-error — and its ana_grpid is already
	// `3` (CN16), so converge B rewrites nothing there.
	convergeA := func(t *testing.T) (*CnAgentServer, *fakeNode) {
		t.Helper()
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{
			revision: 2, primary: true, suspended: true})
		assertParked(t, srv, node, testNs, testTd, "converge A")
		node.Reset()
		return srv, node
	}
	// olderBuildLeftover puts the ns-dev back into the state a pre-2026-09-16
	// agent left it in: held `dmsetup suspend`ed, its table still the rule-6
	// raid0. Nothing this build does produces it; it is the upgrade path, and
	// the reason the park still has to come first.
	olderBuildLeftover := func(t *testing.T, srv *CnAgentServer,
		node *fakeNode) {
		t.Helper()
		raid0No := node.devNo["/dev/mapper/"+raid0Name(srv, testTd)]
		dev := node.dms[nsDevName(srv, testNs)]
		dev.table = agent.LinearTable(testTdSize/512, raid0No, 0)
		dev.suspended = true
	}
	nsPath := agent.NvmetRoot + "/subsystems/" + testNqn + "/namespaces/1"
	// assertParkedOnError pins the park's *target*, which an ordering
	// assertion on the reload's command prefix does not: a reload onto the
	// wrong backing device satisfies the order and still leaves the ns-dev
	// serving data. The pin is on the recorded `--table` of the reload itself
	// — `node.dms[nsDevName]` is gone by the time a sub-case asserts, the
	// removal being the very thing under test — and it names the td's
	// `CnErrorName` (CN9), the same device TestStandbyConverge
	// pins a live ns-dev against.
	assertParkedOnError := func(t *testing.T, srv *CnAgentServer,
		node *fakeNode) {
		t.Helper()
		errNo := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
		if errNo == "" {
			t.Fatalf("the td's dm-error is gone: nothing to park onto")
		}
		reloads := node.callsMatching(
			"cmd dmsetup reload " + nsDevName(srv, testNs))
		if len(reloads) == 0 {
			t.Fatalf("the ns-dev was never parked:\n%s",
				strings.Join(node.Calls(), "\n"))
		}
		for _, reload := range reloads {
			if !strings.Contains(reload, "linear "+errNo) {
				t.Fatalf("park is not onto the td's dm-error %s: %q",
					errNo, reload)
			}
		}
	}

	t.Run("namespace leaves ns_list", func(t *testing.T) {
		srv, node := convergeA(t)
		olderBuildLeftover(t, srv, node)
		subsys := defaultSubsys(false)
		subsys[testNqn].NsList = nil
		if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
			revision: 3, primary: true, subsys: subsys})); err != nil {
			t.Fatalf("remove namespace: %v", err)
		}
		assertOrder(t, node,
			"cmd dmsetup reload "+nsDevName(srv, testNs),
			"cmd dmsetup resume "+nsDevName(srv, testNs),
			"writedirect "+nsPath+"/enable=0",
			"cmd rmdir "+nsPath,
			"cmd dmsetup remove "+nsDevName(srv, testNs),
		)
		assertParkedOnError(t, srv, node)
		// One park call, not one per retire step: an ordering assertion stops
		// at its first match and cannot see a second one. The count is over
		// `parkNsDev`'s own `dmsetup table` probe, which every call makes
		// before it decides anything — counting reloads would prove nothing,
		// because `parkNsDev` is idempotent (td.go: already linear over the
		// errorName and resumed returns *before* `Reload`) and so a second
		// park emits no dmsetup command at all. `dmsetup info` is no counter
		// either: `removeDm` probes the same device below. It says nothing
		// about any other ns-dev — this fixture removes exactly one.
		parks := node.callsMatching(
			"cmd dmsetup table " + nsDevName(srv, testNs))
		if len(parks) != 1 {
			t.Fatalf("want exactly one ns-dev park, got %d:\n%s",
				len(parks), strings.Join(parks, "\n"))
		}
	})

	t.Run("subsystem leaves nqn_to_subsystem", func(t *testing.T) {
		srv, node := convergeA(t)
		olderBuildLeftover(t, srv, node)
		if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
			revision: 3, primary: true,
			subsys: map[string]*pb.Subsystem{}})); err != nil {
			t.Fatalf("remove subsystem: %v", err)
		}
		// removeExport tears the whole subsystem down (port link, ns disable,
		// rmdir ns, allowed hosts, rmdir subsystem), so the park has to be
		// ahead of *its* ns disable too.
		assertOrder(t, node,
			"cmd dmsetup reload "+nsDevName(srv, testNs),
			"cmd dmsetup resume "+nsDevName(srv, testNs),
			"writedirect "+nsPath+"/enable=0",
			"cmd rmdir "+agent.NvmetRoot+"/subsystems/"+testNqn,
			"cmd dmsetup remove "+nsDevName(srv, testNs),
		)
		assertParkedOnError(t, srv, node)
	})

	// The steady state after 2026-09-16: converge A already parked the ns-dev,
	// so the sweep's park pre-step finds the table it wants on a live device and
	// `parkNsDev` returns before it issues anything. The nvmet disable and the
	// removal then work on a device nobody ever suspended — which is the whole
	// point of the park, and what the two sub-cases above can no longer show.
	t.Run("an already parked namespace needs no reload", func(t *testing.T) {
		srv, node := convergeA(t)
		subsys := defaultSubsys(false)
		subsys[testNqn].NsList = nil
		if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
			revision: 3, primary: true, subsys: subsys})); err != nil {
			t.Fatalf("remove namespace: %v", err)
		}
		assertNoCall(t, node, "cmd dmsetup reload "+nsDevName(srv, testNs))
		assertNoCall(t, node, "cmd dmsetup suspend "+nsDevName(srv, testNs))
		assertNoCall(t, node, "cmd dmsetup resume "+nsDevName(srv, testNs))
		assertOrder(t, node,
			"writedirect "+nsPath+"/enable=0",
			"cmd rmdir "+nsPath,
			"cmd dmsetup remove "+nsDevName(srv, testNs),
		)
		if _, ok := node.dms[nsDevName(srv, testNs)]; ok {
			t.Fatalf("the ns-dev survived the removal")
		}
	})
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
		// The cntlr is FORGOTTEN first — file, chunks, memory entry —
		// and its resources are then found by name by the node-level sweep.
		// The old teardown removed the resources first and deleted the file
		// whether or not that worked, which is how a cntlr whose array would
		// not stop was forgotten with its devices still live.
		"cmd rm -f "+srv.nf.LocalCntlrPath(
			testCluster, testCn, testSp, testCntlr),
		"cmd dmsetup reload "+nsDevName(srv, testNs),
		"cmd rmdir "+agent.NvmetRoot+"/subsystems/"+testNqn,
		"cmd dmsetup remove "+nsDevName(srv, testNs),
		"cmd dmsetup remove "+raid0Name(srv, testTd),
		"cmd dmsetup remove "+thinName(srv, testTd),
		"cmd dmsetup remove "+poolName(srv),
		"cmd dmsetup remove "+legName(srv, testMetaLeg),
	)
	// CN21: one leg disconnects *before* its wrapper
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
	// Steady state includes a completed probe round per leg; before it the
	// legs read PENDING (CN11).
	probeAllLegs(t, srv)

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
// §6.17 — provisioning deferral ([D15])
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
// the second one, which is what makes the out-of-order clearing of [D15]'s
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
	assertPending(t, info.GetLegIdToLeg()[testMetaLeg], "leg meta")
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

// TestOutOfOrderGrowDefersEveryLaterGroup is the [D15] prefix rule: deferral cuts
// a group list at the *first* deferred group instead of filtering deferred
// groups out of the middle. Two GrowSlice appends whose sides finish zeroing
// out of order are the reachable trigger — no spare and no worker involved.
//
// An order-preserving filter would build the later group at the earlier one's
// concat offset, so every pool-data block dm-thin allocated in that window
// would physically move the moment the earlier group cleared and the target
// was re-inserted in the middle: silent corruption, reported OK by design.
// CN28 ("a not-yet-grown concat/pool is OK, not a mismatch; the
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
// same rule: [D15]'s deferral holds a *new* group out of the concat until it is
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

// TestDeferredTdNamespaceIsInaccessible is the CN16 conjunct [D15] adds: while
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
// outranks "it is coming" (CN19). Both channels say so.
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
	assertPending(t, info.GetLegIdToLeg()[testDataLeg], "member leg")
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
	assertPending(t, info.GetLegIdToLeg()[testDataLeg], "leg data")
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
