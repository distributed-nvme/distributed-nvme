package cnagent

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The fixture mirrors the smoke case's shape (cnagent_integtest.md, Cases): one
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
	testCapacity   = uint64(1099511627776)
	testBlockSize  = uint64(1 << 20)
	testStripeSize = uint64(65536)
	testTdSize     = uint64(64 << 20)

	testNqn     = "nqn.2024-01.io.dnv-it:s:vol1"
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
// zeroing (architecture.md, Side provisioning protocol) finished and the
// worker flipped the gate, so the DN exports it
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
	// twoLegs gives the data group a second leg, so the assembly cases of
	// cnagent.md CN12 that need a real mirror have one.
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
	// slices, when above 0, replaces the fixture's slices with that many
	// (manySlices): each one meta group and one data group, of two legs at
	// raid1 and of one leg otherwise, with fresh ids, and the legs' sides
	// alternating between the two disk node addresses. At raid1, 2 slices
	// hold 8 legs and 8 slices 32. twoSlices, twoLegs, extraSide and the
	// unprovisioned options shape only the fixture's own slices.
	slices int
}

// manySliceBase is where manySlices' ids start, above every other id of the
// fixtures; each slice takes manySliceStride of them.
const (
	manySliceBase   = uint64(0x1000)
	manySliceStride = uint64(0x10)
)

// manySlices is reqOpts.slices' slice map: slice i is id manySliceBase +
// i*manySliceStride, and its groups, legs and sides take the ids after it.
// Leg k of the whole request, counting in order, has its one side at testIp
// when k is even and at testIp2 when it is odd, so each raid1 group mirrors
// across the two addresses.
func manySlices(
	n int,
	raid1 bool,
	metaBlocks, dataBlocksMeta, dataBlocksData uint64,
) map[string]*pb.Slice {
	legsPerGrp := 1
	if raid1 {
		legsPerGrp = 2
	}
	out := make(map[string]*pb.Slice, n)
	legCnt := 0
	legsOf := func(first uint64) []*pb.Leg {
		var legs []*pb.Leg
		for j := 0; j < legsPerGrp; j++ {
			addr, svcId := testIp, testSvcId
			if legCnt%2 == 1 {
				addr, svcId = testIp2, testSvcId2
			}
			legCnt++
			legId := first + uint64(2*j)
			leg := legOf(legId, sideOf(legId+1, addr, svcId))
			leg.LegIdx = uint32(j)
			legs = append(legs, leg)
		}
		return legs
	}
	for i := 0; i < n; i++ {
		sliceId := manySliceBase + uint64(i)*manySliceStride
		metaGrp, dataGrp := sliceId+1, sliceId+8
		out[fmt.Sprintf(common.IdKeyFmt, sliceId)] = sliceOf(uint32(i),
			metaGrp, legsOf(metaGrp+1),
			dataGrp, legsOf(dataGrp+1), nil,
			metaBlocks, dataBlocksMeta, dataBlocksData)
	}
	return out
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
// has let go (architecture.md, Thin devices, DeleteThinDevice) — still
// describes a coherent cntlr.
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
	if o.slices > 0 {
		idToSlice = manySlices(o.slices, o.raid1,
			metaBlocks, dataBlocksMeta, dataBlocksData)
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

// assertSysfsDeadlines pins SH15: no read of
// the CN10 sysfs leg walk may reach the OsClient on a deadline-less ctx.
// The per-attribute guard keeps it from going vacuous if a later
// fixture change stops exercising one of the reads.
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

// assertParked pins the park of architecture.md, Namespace suspend semantics:
// one ns-dev's table is a plain dm-linear
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
// kernel said and not a paraphrase (architecture.md, Live-state reporting).
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

// assertProvisioningDetails is assertProvisioning for a row whose details say
// what it waits for: a clone the arena has no free run for reads
// "waiting for metadata room: " and the allocator's own text (CN18 step 2),
// fixed for as long as the arena stays as it is.
func assertProvisioningDetails(
	t *testing.T,
	info *pb.ResInfo,
	want string,
	label string,
) {
	t.Helper()
	if info.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING ||
		info.GetDetails() != want {
		t.Fatalf("%s: status %v, details %q, want PROVISIONING/%q",
			label, info.GetStatus(), info.GetDetails(), want)
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

// cloneMetaName is the kind-`cb` wrapper over one clone's slot in the
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
// Fresh SyncupCn (CN5, CN7)
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
		// rebuild against the previous list.
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
	// [D14]: the CN runs no LVM — CN5 stops at the loop device and the arena
	// is carved by the slot allocator. This is the unit-test form of the
	// repo-wide "no LVM command anywhere" acceptance grep.
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

// TestSyncupCnOnNonDefaultPort pins dnagent.md CM2's --nvmet-port-id:
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

	// Ahead of assertOrder, which is fatal: a fault that converges the
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

// arenaKill is how a base-state probe goes unanswered in the tests below:
// "once" kills only the first run of it, so the converge's second asking
// answers; "every time" kills every run, so neither does (CN5).
type arenaKill struct {
	name   string
	always bool
}

var arenaKills = []arenaKill{{"once", false}, {"every time", true}}

func (k arenaKill) arm(node *fakeNode, probe string) {
	if k.always {
		node.killCmdAlways[probe] = true
		return
	}
	node.killCmd[probe] = true
}

// TestKilledFindmntDoesNotRemountTheArena: a `findmnt` the soft timeout
// killed did not answer, and "did not answer" is not "nothing is mounted
// there" (CN5, SH15). Read as absence, it sends the converge into `mount`,
// and a mount over the live arena stacks a second, empty tmpfs on it: the
// arena file drops out of sight, the converge truncates a fresh one and
// attaches a second loop device to it, and every kind-cb wrapper still maps
// the first loop — so every clone on the CN reads a mismatched wrapper and is
// rebuilt (CN28), all for one probe that did not answer. Either of Mounted's
// two calls can be the one killed. The converge runs the probe once more,
// from its first call, and only once: killed once, the second asking answers
// and tmpfs_info reads OK; killed every time, tmpfs_info says which probe did
// not answer.
// Either way it mounts nothing, truncates nothing and keeps the one loop
// device. A check round does not ask again: a kill there reads ERROR, never
// MISSING.
func TestKilledFindmntDoesNotRemountTheArena(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe string
	}{
		{"the type probe",
			"findmnt --noheadings --output FSTYPE --target"},
		{"the mountpoint probe",
			"findmnt --noheadings --output TARGET --mountpoint"},
	} {
		for _, kill := range arenaKills {
			t.Run(tc.name+" killed "+kill.name, func(t *testing.T) {
				srv, node := newTestServer(t)
				syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
				probe := tc.probe + " " +
					srv.nf.CnTmpfsPath(testCluster, testCn)
				loop := loopDev(t, srv, node)

				node.Reset()
				kill.arm(node, probe)
				reply, err := srv.SyncupCn(context.Background(),
					cnReq(3, true))
				if err != nil {
					t.Fatalf("SyncupCn: %v", err)
				}
				if got := node.callsMatching("cmd mount "); len(got) != 0 {
					t.Errorf("a tmpfs was mounted over the live arena: %v",
						got)
				}
				if got := node.callsMatching("cmd truncate "); len(got) != 0 {
					t.Errorf("the arena file was truncated: %v", got)
				}
				if got := loopDev(t, srv, node); got != loop {
					t.Errorf("the arena file is behind %s now, was %s",
						got, loop)
				}
				if n := callCnt(node, "cmd "+probe); n != 2 {
					t.Errorf("%q ran %d times in the converge, want 2: "+
						"the one killed and one more asking", probe, n)
				}
				info := reply.GetCnInfo()
				if kill.always {
					assertErrorDetails(t, info.GetTmpfsInfo(), probe, "tmpfs")
				} else {
					assertOk(t, info.GetTmpfsInfo(), "tmpfs")
				}
				assertOk(t, info.GetTmpFileInfo(), "tmp_file")
				assertOk(t, info.GetLoopDevInfo(), "loop_dev")

				delete(node.killCmdAlways, probe)
				node.Reset()
				node.killCmd[probe] = true
				check, _ := srv.checkCnRound(context.Background(),
					&pb.CheckCnRequest{ClusterId: testCluster, CnId: testCn,
						Revision: 3}, nil)
				assertErrorDetails(t, check.GetCnInfo().GetTmpfsInfo(), probe,
					"checked tmpfs")
			})
		}
	}
}

// TestKilledStatDoesNotTruncateTheArenaFile is the backing file's half of the
// same rule (CN5, SH15): a `stat` that did not answer is not "no such file",
// and the converge must not answer it with a `truncate --size
// CnCloneMetaAreaSize` of the live arena file — a no-op at that size, but a
// resize in place of a file of any other size, which the converge otherwise
// leaves alone while anything uses it.
// The converge asks once more, and exactly once: killed once, tmp_file_info
// reads OK; killed every time, it names the probe. A check round reads the
// same kill as ERROR, never MISSING.
func TestKilledStatDoesNotTruncateTheArenaFile(t *testing.T) {
	for _, kill := range arenaKills {
		t.Run("killed "+kill.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			probe := "stat --format %s " +
				srv.nf.CnTmpFilePath(testCluster, testCn)
			loop := loopDev(t, srv, node)

			node.Reset()
			kill.arm(node, probe)
			reply, err := srv.SyncupCn(context.Background(), cnReq(3, true))
			if err != nil {
				t.Fatalf("SyncupCn: %v", err)
			}
			if got := node.callsMatching("cmd truncate "); len(got) != 0 {
				t.Errorf("the arena file was truncated: %v", got)
			}
			if got := loopDev(t, srv, node); got != loop {
				t.Errorf("the arena file is behind %s now, was %s", got, loop)
			}
			if n := callCnt(node, "cmd "+probe); n != 2 {
				t.Errorf("%q ran %d times in the converge, want 2: "+
					"the one killed and one more asking", probe, n)
			}
			info := reply.GetCnInfo()
			assertOk(t, info.GetTmpfsInfo(), "tmpfs")
			if kill.always {
				assertErrorDetails(t, info.GetTmpFileInfo(), probe, "tmp_file")
			} else {
				assertOk(t, info.GetTmpFileInfo(), "tmp_file")
			}
			assertOk(t, info.GetLoopDevInfo(), "loop_dev")

			delete(node.killCmdAlways, probe)
			node.Reset()
			node.killCmd[probe] = true
			check, _ := srv.checkCnRound(context.Background(),
				&pb.CheckCnRequest{ClusterId: testCluster, CnId: testCn,
					Revision: 3}, nil)
			assertErrorDetails(t, check.GetCnInfo().GetTmpFileInfo(), probe,
				"checked tmp_file")
		})
	}
}

// TestAskingAgainBuildsAnAbsentArena is the other side of the same asking
// (CN5): the base state a probe asked about may really be absent, and the
// converge that learns nothing from a kill must still build it once a second
// asking answers. A check round that reads the absent object names it in its
// verdict, and the worker re-sends the SyncupCn (CN30), but only a round
// later — so a converge that stopped at the first kill would leave the CN
// without a clone-metadata arena until then.
//
// After a reboot the tmpfs is gone while the cn file survives: the startup
// reconcile whose first `findmnt` is killed mounts the tmpfs exactly once,
// and the first check round reads the base state OK. On a fresh CN the arena
// file is not there yet: the first SyncupCn whose first `stat` of it is
// killed truncates it exactly once and attaches the one loop device.
func TestAskingAgainBuildsAnAbsentArena(t *testing.T) {
	t.Run("the tmpfs after a reboot", func(t *testing.T) {
		node := newFakeNode()
		node.dirs[agent.NvmetRoot] = true
		srv := newCnServer(node)
		if err := node.writeProto(context.Background(),
			srv.nf.LocalCnPath(testCluster, testCn),
			cnReq(2, false)); err != nil {
			t.Fatalf("seeding the cn file: %v", err)
		}
		probe := "findmnt --noheadings --output FSTYPE --target " +
			srv.nf.CnTmpfsPath(testCluster, testCn)
		node.killCmd[probe] = true
		reconcileForTest(t, srv)

		if n := callCnt(node, "cmd mount "); n != 1 {
			t.Fatalf("%d mounts at the startup reconcile, want exactly 1", n)
		}
		if n := callCnt(node, "cmd "+probe); n != 2 {
			t.Errorf("%q ran %d times, want 2: the one killed and one "+
				"more asking", probe, n)
		}
		node.Reset()
		check, _ := srv.checkCnRound(context.Background(),
			&pb.CheckCnRequest{ClusterId: testCluster, CnId: testCn,
				Revision: 2}, nil)
		if code := check.GetAgentReply().GetCode(); code != 0 {
			t.Errorf("check round code %d (%q), want 0",
				code, check.GetAgentReply().GetDetails())
		}
		info := check.GetCnInfo()
		assertOk(t, info.GetTmpfsInfo(), "checked tmpfs")
		assertOk(t, info.GetTmpFileInfo(), "checked tmp_file")
		assertOk(t, info.GetLoopDevInfo(), "checked loop_dev")
		if got := node.Mutations(); len(got) != 0 {
			t.Errorf("the check round mutated: %v", got)
		}
	})

	t.Run("the arena file on a fresh cn", func(t *testing.T) {
		srv, node := newTestServer(t)
		probe := "stat --format %s " +
			srv.nf.CnTmpFilePath(testCluster, testCn)
		node.killCmd[probe] = true
		reply, err := srv.SyncupCn(context.Background(), cnReq(2, false))
		if err != nil {
			t.Fatalf("SyncupCn: %v", err)
		}
		if n := callCnt(node, "cmd truncate "); n != 1 {
			t.Fatalf("%d truncates of the arena file, want exactly 1", n)
		}
		if n := callCnt(node, "cmd "+probe); n != 2 {
			t.Errorf("%q ran %d times, want 2: the one killed and one "+
				"more asking", probe, n)
		}
		info := reply.GetCnInfo()
		assertOk(t, info.GetTmpfsInfo(), "tmpfs")
		assertOk(t, info.GetTmpFileInfo(), "tmp_file")
		assertOk(t, info.GetLoopDevInfo(), "loop_dev")
		loopDev(t, srv, node)
	})
}

// TestUnconfirmedTmpfsCreatesNoArena: the arena file and its loop device are
// created only on a tmpfs the converge found or mounted at the path (CN5).
// Where it has neither — both askings of `findmnt` unanswered, or a refused
// `mount` — the mountpoint may be a bare directory: after a reboot that kept
// `/tmp`, or after the `mkdir -p` of that refused mount. A `stat` there
// answers "absent", and a `truncate` would put the arena file on the
// filesystem underneath and `losetup --find` attach the loop to it; the next
// converge mounts over that file, truncates a fresh one and attaches a second
// loop, and every clone built meanwhile on the first loop is rebuilt — the
// cascade a killed `findmnt` read as absence would cause. So the pass creates
// neither: the file and loop rows read ERROR, and the next SyncupCn, whose
// commands answer, builds all three exactly once.
func TestUnconfirmedTmpfsCreatesNoArena(t *testing.T) {
	assertNothingCreated := func(t *testing.T, node *fakeNode) {
		t.Helper()
		for _, verb := range []string{
			"cmd truncate ", "cmd losetup --find",
		} {
			if got := node.callsMatching(verb); len(got) != 0 {
				t.Errorf("without a confirmed tmpfs the pass ran %v", got)
			}
		}
	}
	assertBuiltOnce := func(
		t *testing.T,
		node *fakeNode,
		reply *pb.SyncupCnReply,
	) {
		t.Helper()
		for _, verb := range []string{
			"cmd mount ", "cmd truncate ", "cmd losetup --find",
		} {
			if n := callCnt(node, verb); n != 1 {
				t.Errorf("%q ran %d times, want exactly 1", verb, n)
			}
		}
		info := reply.GetCnInfo()
		assertOk(t, info.GetTmpfsInfo(), "tmpfs")
		assertOk(t, info.GetTmpFileInfo(), "tmp_file")
		assertOk(t, info.GetLoopDevInfo(), "loop_dev")
	}

	t.Run("both askings of findmnt killed after a reboot", func(t *testing.T) {
		node := newFakeNode()
		node.dirs[agent.NvmetRoot] = true
		srv := newCnServer(node)
		tmpfs := srv.nf.CnTmpfsPath(testCluster, testCn)
		file := srv.nf.CnTmpFilePath(testCluster, testCn)
		node.dirs[tmpfs] = true
		if err := node.writeProto(context.Background(),
			srv.nf.LocalCnPath(testCluster, testCn),
			cnReq(2, false)); err != nil {
			t.Fatalf("seeding the cn file: %v", err)
		}
		probe := "findmnt --noheadings --output FSTYPE --target " + tmpfs
		node.killCmdAlways[probe] = true
		reconcileForTest(t, srv)
		if got := node.callsMatching("cmd mount "); len(got) != 0 {
			t.Errorf("the startup reconcile mounted: %v", got)
		}
		assertNothingCreated(t, node)
		if _, ok := node.plain[file]; ok {
			t.Errorf("the arena file %s exists under the bare mountpoint", file)
		}

		node.Reset()
		reply, err := srv.SyncupCn(context.Background(), cnReq(2, false))
		if err != nil {
			t.Fatalf("SyncupCn: %v", err)
		}
		if got := node.callsMatching("cmd mount "); len(got) != 0 {
			t.Errorf("the SyncupCn mounted: %v", got)
		}
		assertNothingCreated(t, node)
		info := reply.GetCnInfo()
		assertErrorDetails(t, info.GetTmpfsInfo(), probe, "tmpfs")
		assertErrorDetails(t, info.GetTmpFileInfo(), "no tmpfs is confirmed",
			"tmp_file")
		assertErrorDetails(t, info.GetLoopDevInfo(), "no tmpfs is confirmed",
			"loop_dev")

		delete(node.killCmdAlways, probe)
		node.Reset()
		reply, err = srv.SyncupCn(context.Background(), cnReq(2, false))
		if err != nil {
			t.Fatalf("SyncupCn: %v", err)
		}
		assertBuiltOnce(t, node, reply)
		loopDev(t, srv, node)
	})

	t.Run("a refused mount", func(t *testing.T) {
		srv, node := newTestServer(t)
		tmpfs := srv.nf.CnTmpfsPath(testCluster, testCn)
		node.failCmd["mount -t tmpfs"] = "mount: permission denied"
		reply, err := srv.SyncupCn(context.Background(), cnReq(1, false))
		if err != nil {
			t.Fatalf("SyncupCn: %v", err)
		}
		if n := callCnt(node, "cmd mkdir -p "+tmpfs); n != 1 {
			t.Errorf("mkdir of the mountpoint ran %d times, want 1", n)
		}
		assertNothingCreated(t, node)
		info := reply.GetCnInfo()
		assertErrorDetails(t, info.GetTmpfsInfo(), "permission denied",
			"tmpfs")
		assertErrorDetails(t, info.GetTmpFileInfo(), "no tmpfs is confirmed",
			"tmp_file")
		assertErrorDetails(t, info.GetLoopDevInfo(), "no tmpfs is confirmed",
			"loop_dev")

		node.Reset()
		reply, err = srv.SyncupCn(context.Background(), cnReq(1, false))
		if err != nil {
			t.Fatalf("SyncupCn: %v", err)
		}
		assertBuiltOnce(t, node, reply)
		loopDev(t, srv, node)
	})
}

// checkCnOnce runs one CheckCn round of the fixture CN at revision 2.
func checkCnOnce(t *testing.T, srv *CnAgentServer) *pb.CheckCnReply {
	t.Helper()
	reply, _ := srv.checkCnRound(context.Background(), &pb.CheckCnRequest{
		ClusterId: testCluster, CnId: testCn, Revision: 2}, nil)
	return reply
}

// assertMissing checks one ResInfo reads RES_STATUS_MISSING.
func assertMissing(t *testing.T, info *pb.ResInfo, label string) {
	t.Helper()
	if info.GetStatus() != pb.ResStatus_RES_STATUS_MISSING {
		t.Fatalf("%s: status %v, details %q, want MISSING",
			label, info.GetStatus(), info.GetDetails())
	}
}

// assertReDriven checks a node-level reply is the leftover code that makes
// the worker re-send the SyncupCn, naming every one of names.
func assertReDriven(
	t *testing.T,
	reply *pb.AgentReply,
	label string,
	names ...string,
) {
	t.Helper()
	if reply.GetCode() != common.ReplyCodeLeftover {
		t.Fatalf("%s: code %d (%q), want %d: nothing would re-send the "+
			"SyncupCn that builds what is absent", label, reply.GetCode(),
			reply.GetDetails(), common.ReplyCodeLeftover)
	}
	for _, name := range names {
		if !strings.Contains(reply.GetDetails(), name+": absent") {
			t.Errorf("%s: details %q do not name %q as absent",
				label, reply.GetDetails(), name)
		}
	}
}

// TestAnAbsentBaseStateReDrivesTheSyncupCn pins the node-level half of CN30:
// a CheckCn round that reads a piece of the CN's base state absent — the
// clone-metadata arena's tmpfs, its file or its loop device, or the agent's
// nvmet port or one of its ANA groups — answers a verdict that is not clean,
// naming each, because only the converge of a SyncupCn builds them and the
// worker re-sends a SyncupCn on a Check reply's code, never on its rows
// (dnv-worker.md RW4). The rows alone read MISSING or ERROR, and a verdict
// that stayed clean would leave the CN without the piece until its next
// SyncupCn for some other reason or the agent's next start: after a reboot
// whose mount was refused, no clone could be built on the CN meanwhile. Each
// re-sent SyncupCn tries again, and the round after the one that builds the
// piece is clean.
func TestAnAbsentBaseStateReDrivesTheSyncupCn(t *testing.T) {
	t.Run("the arena after a reboot whose mount is refused",
		func(t *testing.T) {
			node := newFakeNode()
			node.dirs[agent.NvmetRoot] = true
			srv := newCnServer(node)
			if err := node.writeProto(context.Background(),
				srv.nf.LocalCnPath(testCluster, testCn),
				cnReq(2, false)); err != nil {
				t.Fatalf("seeding the cn file: %v", err)
			}
			tmpfs := srv.nf.CnTmpfsPath(testCluster, testCn)
			file := srv.nf.CnTmpFilePath(testCluster, testCn)
			absent := []string{"tmpfs " + tmpfs,
				"clone metadata arena file " + file,
				"loop device of " + file}
			node.failCmdAlways["mount -t tmpfs"] =
				"mount: cannot allocate memory"
			reconcileForTest(t, srv)
			if n := callCnt(node, "cmd mount "); n != 1 {
				t.Fatalf("%d mounts at the startup reconcile, want 1", n)
			}

			check := checkCnOnce(t, srv)
			assertReDriven(t, check.GetAgentReply(), "check round",
				absent...)
			info := check.GetCnInfo()
			assertMissing(t, info.GetTmpfsInfo(), "tmpfs")
			assertMissing(t, info.GetTmpFileInfo(), "tmp_file")
			assertMissing(t, info.GetLoopDevInfo(), "loop_dev")
			got, err := srv.GetCnInfo(context.Background(),
				&pb.GetCnInfoRequest{ClusterId: testCluster, CnId: testCn})
			if err != nil {
				t.Fatalf("GetCnInfo: %v", err)
			}
			assertReDriven(t, got.GetAgentReply(), "GetCnInfo", absent...)

			// The re-sent SyncupCn meets the same refusal. Nothing is left
			// that remembers the failure: the next round reads the arena
			// absent again and re-drives again. The SyncupCn's own reply
			// names nothing absent: its converge has just tried to build
			// the arena, and its rows say how that went.
			node.Reset()
			reply, err := srv.SyncupCn(context.Background(), cnReq(2, false))
			if err != nil {
				t.Fatalf("SyncupCn: %v", err)
			}
			if got := reply.GetAgentReply(); got.GetCode() != 0 ||
				strings.Contains(got.GetDetails(), ": absent") {
				t.Errorf("the re-sent SyncupCn replied %v, want code 0 "+
					"naming nothing absent", got)
			}
			if n := callCnt(node, "cmd mount "); n != 1 {
				t.Fatalf("the re-sent SyncupCn mounted %d times, want 1", n)
			}
			assertReDriven(t, checkCnOnce(t, srv).GetAgentReply(),
				"check round after a refused re-send", absent...)

			// Once the mount answers, the re-sent SyncupCn builds the arena
			// and the next round is clean.
			delete(node.failCmdAlways, "mount -t tmpfs")
			node.Reset()
			if _, err := srv.SyncupCn(context.Background(),
				cnReq(2, false)); err != nil {
				t.Fatalf("SyncupCn: %v", err)
			}
			for _, verb := range []string{
				"cmd mount ", "cmd truncate ", "cmd losetup --find",
			} {
				if n := callCnt(node, verb); n != 1 {
					t.Errorf("%q ran %d times, want exactly 1", verb, n)
				}
			}
			if got := checkCnOnce(t, srv).GetAgentReply(); got.GetCode() !=
				0 {
				t.Errorf("check round after the build = %v, want code 0",
					got)
			}
		})

	// The file and the loop device are created on a tmpfs the converge
	// finds as well as on one it mounts: under a live tmpfs they are
	// re-driven alone, and the re-sent SyncupCn mounts nothing.
	t.Run("the arena file and its loop device under a live tmpfs",
		func(t *testing.T) {
			srv, node := newTestServer(t)
			cnSyncup(t, srv, 2, false)
			file := srv.nf.CnTmpFilePath(testCluster, testCn)
			node.mu.Lock()
			delete(node.plain, file)
			delete(node.loops, file)
			node.mu.Unlock()

			check := checkCnOnce(t, srv)
			assertReDriven(t, check.GetAgentReply(), "check round",
				"clone metadata arena file "+file, "loop device of "+file)
			if details := check.GetAgentReply().GetDetails(); strings.Contains(
				details, "tmpfs ") {
				t.Errorf("details %q name the live tmpfs", details)
			}

			node.Reset()
			cnSyncup(t, srv, 2, false)
			for verb, want := range map[string]int{
				"cmd mount ": 0, "cmd truncate ": 1, "cmd losetup --find": 1,
			} {
				if n := callCnt(node, verb); n != want {
					t.Errorf("%q ran %d times, want %d", verb, n, want)
				}
			}
			if got := checkCnOnce(t, srv).GetAgentReply(); got.GetCode() !=
				0 {
				t.Errorf("check round after the build = %v, want code 0",
					got)
			}
		})

	for _, tc := range []struct {
		name string
		// dir is the configfs directory taken away, under the port.
		dir string
		// details is what the port row and the verdict say is missing.
		details string
	}{
		{"the port", "", "port directory missing"},
		{"an ana group of the port", "/ana_groups/2", "ana group 2 missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			cnSyncup(t, srv, 2, false)
			portPath := agent.NvmetRoot + "/ports/1"
			node.mu.Lock()
			node.cmdRmdir([]string{portPath + tc.dir})
			node.mu.Unlock()

			check := checkCnOnce(t, srv)
			assertReDriven(t, check.GetAgentReply(), "check round")
			if details := check.GetAgentReply().GetDetails(); !strings.Contains(
				details, "nvmet port 1: absent: "+tc.details) {
				t.Errorf("details %q do not name the port as absent", details)
			}
			assertErrorDetails(t, check.GetCnInfo().GetPortInfo(),
				tc.details, "port")

			node.Reset()
			cnSyncup(t, srv, 2, false)
			if n := countCalls(node, "cmd mkdir -p "+portPath+tc.dir); n != 1 {
				t.Errorf("the re-sent SyncupCn made %s %d times, want 1",
					portPath+tc.dir, n)
			}
			if got := checkCnOnce(t, srv).GetAgentReply(); got.GetCode() !=
				0 {
				t.Errorf("check round after the rebuild = %v, want code 0",
					got)
			}
		})
	}
}

// TestAnUnansweredBaseStateProbeDoesNotReDrive bounds the verdict above. A
// probe that did not answer proves nothing absent, and the next round asks
// again; a re-sent SyncupCn would not act on it either, since the converge
// creates nothing on an unanswered probe (CN5). So an unanswered `findmnt`
// after a reboot — the arena file and the loop device then read absent, but
// the converge creates them only on a tmpfs it found or mounted — and a port
// whose directory listing, ANA group read or transport attribute read was
// cut off read ERROR and leave the verdict clean. So does a mismatch the
// verdict leaves to its row: a transport attribute of the port that differs,
// which nvmet refuses to rewrite while a subsystem is linked to the port;
// and an arena file of another size whose loop device something holds, a
// second loop device, and a filesystem of another type mounted at the
// arena's path, with no arena file and no loop device on it, which the
// converge leaves as they are. TestAnArenaFileOfAnotherSizeIsReplaced
// covers the rest of the file of another size.
func TestAnUnansweredBaseStateProbeDoesNotReDrive(t *testing.T) {
	t.Run("every findmnt of the arena killed after a reboot",
		func(t *testing.T) {
			node := newFakeNode()
			node.dirs[agent.NvmetRoot] = true
			srv := newCnServer(node)
			if err := node.writeProto(context.Background(),
				srv.nf.LocalCnPath(testCluster, testCn),
				cnReq(2, false)); err != nil {
				t.Fatalf("seeding the cn file: %v", err)
			}
			probe := "findmnt --noheadings --output FSTYPE --target " +
				srv.nf.CnTmpfsPath(testCluster, testCn)
			node.killCmdAlways[probe] = true
			reconcileForTest(t, srv)

			check := checkCnOnce(t, srv)
			if got := check.GetAgentReply(); got.GetCode() != 0 {
				t.Errorf("check round = %v, want code 0", got)
			}
			info := check.GetCnInfo()
			assertErrorDetails(t, info.GetTmpfsInfo(), probe, "tmpfs")
			assertMissing(t, info.GetTmpFileInfo(), "tmp_file")
			assertMissing(t, info.GetLoopDevInfo(), "loop_dev")
		})

	portPath := agent.NvmetRoot + "/ports/1"
	file := common.NewNameFmt(common.DefaultLocalStorPrefix).CnTmpFilePath(
		testCluster, testCn)
	portRow := (*pb.CnInfo).GetPortInfo
	for _, tc := range []struct {
		name string
		arm  func(node *fakeNode)
		// row is the CnInfo row that must read ERROR containing want.
		row  func(*pb.CnInfo) *pb.ResInfo
		want string
	}{
		{"the port listing killed", func(node *fakeNode) {
			node.killCmdAlways["ls -1 "+portPath] = true
		}, portRow, "ls -1 " + portPath},
		{"an ana group read cut off", func(node *fakeNode) {
			node.killReadAlways[portPath+"/ana_groups/2/ana_state"] = true
		}, portRow, portPath + "/ana_groups/2/ana_state"},
		{"a port transport attribute read cut off", func(node *fakeNode) {
			node.killReadAlways[portPath+"/addr_trtype"] = true
		}, portRow, portPath + "/addr_trtype"},
		// The mismatches the verdict leaves to their rows: nvmet refuses an
		// addr_* write while a subsystem is linked to the port, and the
		// converge reports an arena file of another size whose loop device
		// something holds, or a second loop device, and leaves it as it is.
		{"a port transport attribute that differs", func(node *fakeNode) {
			node.files[portPath+"/addr_traddr"] = "192.168.10.99"
		}, portRow, `addr_traddr is "192.168.10.99"`},
		{"an arena file of another size whose loop device is held",
			func(node *fakeNode) {
				node.plain[file] = 4096
				node.loopHolders[node.loops[file][0]] = []string{"md127"}
			}, (*pb.CnInfo).GetTmpFileInfo, "size is 4096"},
		{"a second loop device", func(node *fakeNode) {
			node.loops[file] = append(node.loops[file], "/dev/loop99")
		}, (*pb.CnInfo).GetLoopDevInfo, "2 loop devices"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			cnSyncup(t, srv, 2, false)
			node.mu.Lock()
			tc.arm(node)
			node.mu.Unlock()

			check := checkCnOnce(t, srv)
			if got := check.GetAgentReply(); got.GetCode() != 0 {
				t.Errorf("check round = %v, want code 0", got)
			}
			assertErrorDetails(t, tc.row(check.GetCnInfo()), tc.want,
				tc.name)
		})
	}

	t.Run("a tmpfs of another type", func(t *testing.T) {
		srv, node := newTestServer(t)
		cnSyncup(t, srv, 2, false)
		tmpfs := srv.nf.CnTmpfsPath(testCluster, testCn)
		file := srv.nf.CnTmpFilePath(testCluster, testCn)
		node.mu.Lock()
		node.mounts[tmpfs] = "ext4"
		delete(node.plain, file)
		delete(node.loops, file)
		node.mu.Unlock()

		check := checkCnOnce(t, srv)
		if got := check.GetAgentReply(); got.GetCode() != 0 {
			t.Errorf("check round = %v, want code 0", got)
		}
		info := check.GetCnInfo()
		assertErrorDetails(t, info.GetTmpfsInfo(), "ext4", "tmpfs")
		assertMissing(t, info.GetTmpFileInfo(), "tmp_file")
		assertMissing(t, info.GetLoopDevInfo(), "loop_dev")
	})
}

// arenaFileTouched lists the calls of a pass that replace the arena file or
// detach a loop device of it.
func arenaFileTouched(node *fakeNode, file string) []string {
	var out []string
	for _, fragment := range []string{
		"cmd losetup --detach ", "cmd rm -f " + file, "cmd truncate ",
		"cmd losetup --find ",
	} {
		out = append(out, node.callsMatching(fragment)...)
	}
	return out
}

// assertArenaFileNamed checks a node-level verdict names the arena file as of
// another size, the leftover code that makes the worker re-send the SyncupCn
// whose converge replaces it (CN30).
func assertArenaFileNamed(t *testing.T, reply *pb.AgentReply, file string) {
	t.Helper()
	want := "clone metadata arena file " + file + ": of another size: " +
		fmt.Sprintf("4096 bytes, want %d", common.CnCloneMetaAreaSize)
	if reply.GetCode() != common.ReplyCodeLeftover ||
		!strings.Contains(reply.GetDetails(), want) {
		t.Fatalf("check round = %v, want code %d naming %q: nothing "+
			"would re-send the SyncupCn that replaces the file", reply,
			common.ReplyCodeLeftover, want)
	}
}

// TestAnArenaFileOfAnotherSizeIsReplaced pins CN5's replacement of an arena
// file of another size and the node-level verdict that drives it (CN30). Both
// channels judge the file by one test (arenaFileInUse). While nothing uses
// it, a check round names it, so the worker re-sends the SyncupCn, and that
// converge detaches the file's loop device, confirms the detach, removes the
// file, truncates it afresh and attaches a new loop device; the next round is
// clean. While something uses it — a clone-metadata wrapper that maps any
// of its loop devices, a wrapper whose table is not a run of the arena, a
// device that holds a loop device — or a read that would tell does not
// answer — the holders listing, the wrapper listing, the file's loop
// listing or a loop device's number — no round names it and the converge
// leaves it, the row saying why. A loop device open without a holder is
// named, and the converge that detaches it finds it still attached and
// removes nothing. Off a confirmed tmpfs nothing is named or touched.
func TestAnArenaFileOfAnotherSizeIsReplaced(t *testing.T) {
	// setup builds the CN's base state, makes the arena file 4096 bytes and
	// arms what the case wants under the fake's lock.
	setup := func(
		t *testing.T,
		arm func(srv *CnAgentServer, node *fakeNode, loop string),
	) (*CnAgentServer, *fakeNode, string, string) {
		t.Helper()
		srv, node := newTestServer(t)
		cnSyncup(t, srv, 2, false)
		file := srv.nf.CnTmpFilePath(testCluster, testCn)
		loop := loopDev(t, srv, node)
		node.mu.Lock()
		node.plain[file] = 4096
		arm(srv, node, loop)
		node.mu.Unlock()
		node.Reset()
		return srv, node, file, loop
	}
	syncupCn := func(t *testing.T, srv *CnAgentServer) *pb.CnInfo {
		t.Helper()
		reply, err := srv.SyncupCn(context.Background(), cnReq(2, false))
		if err != nil {
			t.Fatalf("SyncupCn: %v", err)
		}
		return reply.GetCnInfo()
	}

	t.Run("nothing uses it", func(t *testing.T) {
		srv, node, file, loop := setup(t,
			func(*CnAgentServer, *fakeNode, string) {})

		check := checkCnOnce(t, srv)
		assertArenaFileNamed(t, check.GetAgentReply(), file)
		assertErrorDetails(t, check.GetCnInfo().GetTmpFileInfo(),
			"size is 4096", "checked tmp_file")

		node.Reset()
		info := syncupCn(t, srv)
		assertOrder(t, node,
			"cmd losetup --detach "+loop,
			"cmd losetup --associated "+file,
			"cmd rm -f "+file,
			fmt.Sprintf("cmd truncate --size %d %s",
				common.CnCloneMetaAreaSize, file),
			"cmd losetup --find --show "+file)
		if n := len(node.callsMatching("cmd rm -f " + file)); n != 1 {
			t.Errorf("the file was removed %d times, want 1", n)
		}
		assertOk(t, info.GetTmpFileInfo(), "tmp_file")
		assertOk(t, info.GetLoopDevInfo(), "loop_dev")
		if got := node.plain[file]; got != common.CnCloneMetaAreaSize {
			t.Errorf("the file is %d bytes, want %d", got,
				common.CnCloneMetaAreaSize)
		}
		if got := loopDev(t, srv, node); got == loop {
			t.Errorf("the file is still behind the old loop device %s", got)
		}
		if got := checkCnOnce(t, srv).GetAgentReply(); got.GetCode() != 0 {
			t.Errorf("check round after the replacement = %v, want code 0",
				got)
		}
	})

	for _, tc := range []struct {
		name string
		arm  func(srv *CnAgentServer, node *fakeNode, loop string)
		// clean says the round has nothing else to name, so its code is 0;
		// an orphan wrapper, or a dm listing that does not answer, is named
		// by the sweep's own verdict.
		clean bool
		// why is what the converge's row says it left the file for, with
		// {loop} standing for the loop device's kernel name.
		why string
	}{
		{"a clone metadata wrapper maps its loop device",
			func(srv *CnAgentServer, node *fakeNode, loop string) {
				name := srv.nf.CnCloneMetaDmName(
					testCluster, testCn, testSp+1, testClone)
				node.dms[name] = &fakeDm{
					table: fmt.Sprintf("0 %d linear %s 0",
						cnCloneMetaUnitSectors, node.devNo[loop]),
					thinIds: map[uint32]bool{},
				}
				node.devNo["/dev/mapper/"+name] = node.newDevNo()
			}, false, "maps /dev/loop"},
		{"a clone metadata wrapper maps its second loop device",
			func(srv *CnAgentServer, node *fakeNode, loop string) {
				const second = "/dev/loop99"
				file := srv.nf.CnTmpFilePath(testCluster, testCn)
				node.loops[file] = append(node.loops[file], second)
				node.devNo[second] = node.newDevNo()
				node.publishBlockDev(strings.TrimPrefix(second, "/dev/"))
				name := srv.nf.CnCloneMetaDmName(
					testCluster, testCn, testSp+1, testClone)
				node.dms[name] = &fakeDm{
					table: fmt.Sprintf("0 %d linear %s 0",
						cnCloneMetaUnitSectors, node.devNo[second]),
					thinIds: map[uint32]bool{},
				}
				node.devNo["/dev/mapper/"+name] = node.newDevNo()
			}, false, "maps /dev/loop99"},
		{"the number of its loop device does not read",
			func(srv *CnAgentServer, node *fakeNode, loop string) {
				node.failReadAlways[sysfsBlockDir+"/"+
					strings.TrimPrefix(loop, "/dev/")+"/dev"] = true
			}, true, "read " + sysfsBlockDir + "/{loop}/dev: "},
		{"a clone metadata wrapper is not a run of the arena",
			func(srv *CnAgentServer, node *fakeNode, loop string) {
				name := srv.nf.CnCloneMetaDmName(
					testCluster, testCn, testSp+1, testClone)
				node.dms[name] = &fakeDm{
					table:   fmt.Sprintf("0 %d error", cnCloneMetaUnitSectors),
					thinIds: map[uint32]bool{},
				}
				node.devNo["/dev/mapper/"+name] = node.newDevNo()
			}, false, "is not a run of the arena"},
		{"a device holds its loop device",
			func(srv *CnAgentServer, node *fakeNode, loop string) {
				node.loopHolders[loop] = []string{"md127"}
			}, true, "is held by md127"},
		{"the holders listing does not answer",
			func(srv *CnAgentServer, node *fakeNode, loop string) {
				node.killCmdAlways["list "+sysfsBlockDir+"/"+
					strings.TrimPrefix(loop, "/dev/")+"/holders"] = true
			}, true, "list " + sysfsBlockDir + "/{loop}/holders"},
		{"the wrapper listing does not answer",
			func(srv *CnAgentServer, node *fakeNode, loop string) {
				node.killCmdAlways["cmd dmsetup ls"] = true
			}, false, "dmsetup ls"},
		// With no loop listing there is no loop device to match a wrapper
		// or read holders of, so nothing else would stop the replacement
		// under the wrapper that maps it.
		{"the loop listing does not answer",
			func(srv *CnAgentServer, node *fakeNode, loop string) {
				name := srv.nf.CnCloneMetaDmName(
					testCluster, testCn, testSp+1, testClone)
				node.dms[name] = &fakeDm{
					table: fmt.Sprintf("0 %d linear %s 0",
						cnCloneMetaUnitSectors, node.devNo[loop]),
					thinIds: map[uint32]bool{},
				}
				node.devNo["/dev/mapper/"+name] = node.newDevNo()
				node.killCmdAlways["losetup --associated"] = true
			}, false, "losetup --associated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node, file, loop := setup(t, tc.arm)
			why := strings.ReplaceAll(tc.why, "{loop}",
				strings.TrimPrefix(loop, "/dev/"))

			check := checkCnOnce(t, srv)
			reply := check.GetAgentReply()
			if strings.Contains(reply.GetDetails(),
				"clone metadata arena file") {
				t.Errorf("check round %v names the arena file: a re-sent "+
					"SyncupCn would leave it as it is", reply)
			}
			if tc.clean && reply.GetCode() != 0 {
				t.Errorf("check round = %v, want code 0", reply)
			}
			assertErrorDetails(t, check.GetCnInfo().GetTmpFileInfo(),
				"size is 4096", "checked tmp_file")

			node.Reset()
			info := syncupCn(t, srv)
			if touched := arenaFileTouched(node, file); len(touched) != 0 {
				t.Errorf("the converge touched the arena file: %q", touched)
			}
			assertErrorDetails(t, info.GetTmpFileInfo(), "not replaced: ",
				"tmp_file")
			assertErrorDetails(t, info.GetTmpFileInfo(), why, "tmp_file")
			if got := node.plain[file]; got != 4096 {
				t.Errorf("the file is %d bytes, want it left at 4096", got)
			}
		})
	}

	t.Run("its loop device is open without a holder", func(t *testing.T) {
		srv, node, file, loop := setup(t,
			func(_ *CnAgentServer, node *fakeNode, loop string) {
				node.loopOpen[loop] = true
			})

		assertArenaFileNamed(t, checkCnOnce(t, srv).GetAgentReply(), file)

		node.Reset()
		info := syncupCn(t, srv)
		assertOrder(t, node,
			"cmd losetup --detach "+loop,
			"cmd losetup --associated "+file)
		for _, fragment := range []string{
			"cmd rm -f " + file, "cmd truncate ", "cmd losetup --find ",
		} {
			assertNoCall(t, node, fragment)
		}
		assertErrorDetails(t, info.GetTmpFileInfo(),
			"replacing it: "+loop+" still attached after the detach",
			"tmp_file")
		if got := node.plain[file]; got != 4096 {
			t.Errorf("the file is %d bytes, want it left at 4096", got)
		}
		if got := loopDev(t, srv, node); got != loop {
			t.Errorf("the file is behind %s, want %s still", got, loop)
		}
	})

	t.Run("no tmpfs mounted", func(t *testing.T) {
		srv, _, file, _ := setup(t,
			func(srv *CnAgentServer, node *fakeNode, _ string) {
				delete(node.mounts, srv.nf.CnTmpfsPath(testCluster, testCn))
			})

		// The tmpfs is named absent; the file the bare path shows is not
		// named, since the converge's mount hides it.
		reply := checkCnOnce(t, srv).GetAgentReply()
		assertReDriven(t, reply, "check round",
			"tmpfs "+srv.nf.CnTmpfsPath(testCluster, testCn))
		if strings.Contains(reply.GetDetails(),
			"clone metadata arena file "+file) {
			t.Errorf("check round %v names the file the mount hides", reply)
		}
	})

	t.Run("off a confirmed tmpfs", func(t *testing.T) {
		srv, node, file, _ := setup(t,
			func(srv *CnAgentServer, node *fakeNode, _ string) {
				node.mounts[srv.nf.CnTmpfsPath(testCluster, testCn)] = "ext4"
			})

		check := checkCnOnce(t, srv)
		if got := check.GetAgentReply(); got.GetCode() != 0 {
			t.Errorf("check round = %v, want code 0", got)
		}

		node.Reset()
		info := syncupCn(t, srv)
		if touched := arenaFileTouched(node, file); len(touched) != 0 {
			t.Errorf("the converge touched the arena file: %q", touched)
		}
		assertErrorDetails(t, info.GetTmpFileInfo(),
			fmt.Sprintf("%s is 4096 bytes, want %d", file,
				common.CnCloneMetaAreaSize), "tmp_file")
		if strings.Contains(info.GetTmpFileInfo().GetDetails(), "replac") {
			t.Errorf("tmp_file %q: off a confirmed tmpfs the converge "+
				"does not try", info.GetTmpFileInfo().GetDetails())
		}
	})
}

// TestAWrongAnaGroupStateReDrivesTheSyncupCn pins the one piece of the base
// state that the node-level verdict re-drives although it is there (CN30): an
// ANA group of the port in a state other than its fixed one, on a port whose
// transport attributes all match. nvmet takes an ana_state write whatever is
// linked to the port, and EnsurePort rewrites a differing state once the
// attributes match, so a re-sent SyncupCn cures it — and a verdict that
// stayed clean would leave the group in the wrong state, which its
// namespaces report to the hosts, until the CN's next SyncupCn for some
// other reason or the agent's next start. A transport
// attribute that differs is not re-driven, not even beside a wrong group
// state: nvmet refuses every addr_* write while a subsystem is linked to the
// port (EACCES), when every re-send would fail on it the same way, and the
// probe, which reads the attributes first, never gets to the groups.
func TestAWrongAnaGroupStateReDrivesTheSyncupCn(t *testing.T) {
	portPath := agent.NvmetRoot + "/ports/1"
	statePath := portPath + "/ana_groups/2/ana_state"
	details := fmt.Sprintf("ana group 2 is %q, want %q",
		agent.AnaStateOptimized, agent.AnaStateNonOptimized)

	t.Run("its port's attributes match", func(t *testing.T) {
		srv, node := newTestServer(t)
		cnSyncup(t, srv, 2, false)
		node.mu.Lock()
		node.files[statePath] = agent.AnaStateOptimized
		node.mu.Unlock()

		check := checkCnOnce(t, srv)
		reply := check.GetAgentReply()
		if reply.GetCode() != common.ReplyCodeLeftover ||
			!strings.Contains(reply.GetDetails(),
				"nvmet port 1: differs: "+details) {
			t.Fatalf("check round code %d (%q), want %d naming %q: "+
				"nothing would re-send the SyncupCn that rewrites it",
				reply.GetCode(), reply.GetDetails(),
				common.ReplyCodeLeftover, details)
		}
		assertErrorDetails(t, check.GetCnInfo().GetPortInfo(), details,
			"port")

		node.Reset()
		cnSyncup(t, srv, 2, false)
		if n := countCalls(node, "writedirect "+statePath+"="+
			agent.AnaStateNonOptimized); n != 1 {
			t.Errorf("the re-sent SyncupCn wrote %s %d times, want 1",
				statePath, n)
		}
		if got := node.callsMatching("/ana_state="); len(got) != 1 {
			t.Errorf("ana_state writes %q, want the one above", got)
		}
		if got := checkCnOnce(t, srv).GetAgentReply(); got.GetCode() != 0 {
			t.Errorf("check round after the rewrite = %v, want code 0", got)
		}
	})

	t.Run("a transport attribute differs too", func(t *testing.T) {
		srv, node := newTestServer(t)
		cnSyncup(t, srv, 2, false)
		node.mu.Lock()
		node.files[statePath] = agent.AnaStateOptimized
		node.files[portPath+"/addr_traddr"] = "192.168.10.99"
		node.mu.Unlock()

		check := checkCnOnce(t, srv)
		if got := check.GetAgentReply(); got.GetCode() != 0 {
			t.Errorf("check round = %v, want code 0", got)
		}
		assertErrorDetails(t, check.GetCnInfo().GetPortInfo(),
			`addr_traddr is "192.168.10.99"`, "port")
	})
}

// ---------------------------------------------------------------------------
// The revision gate (CN4, CN8)
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
// Probe-first idempotency (SH16)
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
// The primary converge order (CN9)
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

	// CN28 (architecture.md, Live-state reporting): the pool details are the
	// raw dmsetup status line, which the thin-pool auto-grow (dnv-worker.md
	// AR6) parses two used/total pairs out of.
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
// The standby shape (architecture.md, Standby cntlr)
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
// Failover (architecture.md, Failover)
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
// The assembly cases and member reconciliation (cnagent.md CN12)
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

// TestGroupAssembleRefusalIsAnError: a promotion whose assembly md refuses
// reads the group's row ERROR. A group's error is a row, and by itself
// registers no CN10 retry: with every leg available, no member is late
// (CN12). A namespace over the group waits all the same — its ns-dev stays
// parked, its namespace inaccessible — and that registers the retry (CN16),
// a cause that never goes away included.
func TestGroupAssembleRefusalIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		// noTd: an SP with no td and no subsystem, so no namespace waits.
		noTd  bool
		retry bool
	}{
		{name: "under a namespace", retry: true},
		{name: "with no td", noTd: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			opts := func(revision uint64, primary bool) reqOpts {
				o := reqOpts{revision: revision, primary: primary,
					raid1: true}
				if tc.noTd {
					o.tds = []*pb.ThinDevice{}
					o.subsys = map[string]*pb.Subsystem{}
				}
				return o
			}
			syncupBoth(t, srv, opts(2, true))
			if _, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(opts(3, false))); err != nil {
				t.Fatalf("demote: %v", err)
			}
			node.Reset()
			node.failCmdAlways["mdadm --assemble"] =
				"mdadm: refusing degraded start"
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(opts(4, true)))
			if err != nil {
				t.Fatalf("promote: %v", err)
			}
			info := reply.GetCntlrInfo().GetGrpIdToMdRaid()[testMetaGrp]
			if info.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
				t.Fatalf("a refused assembly left status %v",
					info.GetStatus())
			}
			assertNoCall(t, node, "cmd mdadm --detail")
			if got := retrying(t, srv); got != tc.retry {
				t.Fatalf("the CN10 retry is %v after a refused assembly "+
					"with every leg available, want %v", got, tc.retry)
			}
			if !tc.noTd {
				assertNoCall(t, node, "writedirect "+anaPath(testNqn, 1)+"=1")
				if got := node.files[anaPath(testNqn, 1)]; got != "3" {
					t.Fatalf("ana_grpid is %q over the unbuilt stack, "+
						"want 3", got)
				}
			}
		})
	}
}

// Case 2 of cnagent.md CN12 with a member left out: exactly one member of a
// two-leg group carries a superblock — assemble with that one, then add the
// other.
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

// The stale-metadata re-add of case 2 (cnagent.md CN12): both members carry a
// superblock but mdadm leaves one out for stale metadata — the converge
// re-adds it, never zero-superblocks it.
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
// swallowed here would read the row OK over a half-done switch. What brings
// that converge is the check round's verdict: once the promoted spare's row
// reads OK it names the spare the array lacks (CN12, CN30), unless the
// killed command was an `--add` the kernel completed, which left nothing to
// add.
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
				probeAllLegs(t, srv)
				verdict := checkCntlrVerdict(t, srv, 3)
				if verb == " --add " && !noEffect {
					if verdict.GetCode() != 0 {
						t.Fatalf("the check round after a completed --add: "+
							"%v, want a clean verdict", verdict)
					}
				} else {
					assertNamesOnly(t, verdict, "the check round after "+
						"the half-done switch", legName(srv, switchSpareLeg))
				}
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
// (CN12): an array runs under the group's name holding none of the wrappers
// leg_list now names — both legs switched out while this cntlr was not
// converging, parked in spare_leg_list or already released. Md.Detail
// finds an array by its leg_list members only, so it reads the group as
// absent and the assembly runs; the fresh legs carry no superblock, which is
// case 1, and the create guard keeps a second array from being created under
// the name mdadm gives the group's array. The pool's concat finds the
// group's array by its legs as well (mdDevNo), so its rows read an error
// rather than take the running array's number, which
// TestConcatNumberComesFromTheMatchedArray pins. The outcome must be an
// error that touches nothing: no create, and no fail, remove or add against
// the running array — which a Detail keyed on the parked spares too would
// have reached, failing and removing the only members holding the group's
// data. The refusal holds when the lsblk that asks whether an array runs
// under the name did not answer, too: read as "no", a kill would create
// beside the running array.
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
			// The guard's lsblk of the name's node is the cn agent's one
			// lsblk: a dm device's number comes from `dmsetup info`, an
			// array's from /sys/block.
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

// TestConcatNumberComesFromTheMatchedArray pins where a pool concat takes an
// md group's number (CN12, CN13): /sys/block/<node>/dev of the array the
// pass's md walk matches by the group's legs (mdDevNo) — the array the
// group's own row reads — and never the /dev/md/<name> link udev makes. A
// link that is missing or points elsewhere therefore changes nothing: the
// round's concat rows read OK and a converge reloads no concat, where a
// number taken through the link would fail both rows, or remap the data
// concat onto the device the link names. A degraded array still matches,
// and the number is read apart from the array's other attributes, so a
// failed md/sync_completed read fails the group's row alone. A group whose
// legs two arrays hold, or no array holds while another array runs under
// its name, reads ERROR on its concat row, with one text on both channels
// that names the group, since the row is keyed by the slice. So does a group
// no array is seen to hold beside an array whose read did not answer, and
// the text names that array: the walk cannot tell that no array holds the
// legs.
func TestConcatNumberComesFromTheMatchedArray(t *testing.T) {
	ctx := context.Background()
	opts := reqOpts{revision: 2, primary: true, raid1: true, twoLegs: true}
	mdDevOf := func(srv *CnAgentServer, isMeta bool) string {
		return srv.nf.MdPath(srv.nf.CnMdDevName(
			testCluster, testCn, testSp, 0, 0, isMeta))
	}
	sameConcatRows := func(
		t *testing.T, converged, probed *pb.CntlrInfo, want string,
	) {
		t.Helper()
		for label, info := range map[string]*pb.CntlrInfo{
			"the converge": converged, "the check round": probed,
		} {
			assertOk(t, info.GetSliceIdToMeta()[testSlice],
				label+": the meta concat")
			row := info.GetSliceIdToData()[testSlice]
			if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
				row.GetDetails() != want {
				t.Errorf("%s: the data concat is %v %q, want ERROR %q",
					label, row.GetStatus(), row.GetDetails(), want)
			}
		}
	}

	for _, link := range []struct {
		name string
		// move changes the data group's /dev/md/<name> link, and leaves
		// the array's own /sys/block number as it is.
		move func(node *fakeNode, dataMd, metaMd string)
	}{
		{"the link is missing", func(node *fakeNode, dataMd, _ string) {
			delete(node.devNo, dataMd)
		}},
		{"the link points elsewhere",
			func(node *fakeNode, dataMd, metaMd string) {
				node.devNo[dataMd] = node.devNo[metaMd]
			}},
	} {
		t.Run(link.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, opts)
			dataMd, metaMd := mdDevOf(srv, false), mdDevOf(srv, true)
			poolData := srv.nf.CnPoolDataName(
				testCluster, testCn, testSp, testSlice)
			poolMeta := srv.nf.CnPoolMetaName(
				testCluster, testCn, testSp, testSlice)
			node.mu.Lock()
			if node.devNo[dataMd] == node.devNo[metaMd] {
				node.mu.Unlock()
				t.Fatalf("the two arrays share the number %s",
					node.devNo[dataMd])
			}
			// Each concat maps its own group's array, by the number the
			// array's /sys/block entry holds.
			for pool, md := range map[string]string{
				poolMeta: metaMd, poolData: dataMd,
			} {
				targets := dmTargets(node.dms[pool].table)
				if len(targets) != 1 ||
					len(strings.Fields(targets[0])) != 5 ||
					strings.Fields(targets[0])[3] != node.arrays[md].devNo {
					node.mu.Unlock()
					t.Fatalf("%s maps %v, want the array %s at %s", pool,
						targets, md, node.arrays[md].devNo)
				}
			}
			link.move(node, dataMd, metaMd)
			node.mu.Unlock()

			okRow := func(info *pb.ResInfo, label string) {
				t.Helper()
				if info.GetStatus() != pb.ResStatus_RES_STATUS_OK {
					t.Errorf("%s: status %v, details %q, want OK",
						label, info.GetStatus(), info.GetDetails())
				}
			}
			node.Reset()
			probed := checkRound(t, srv, 2)
			okRow(probed.GetSliceIdToMeta()[testSlice],
				"the check round: the meta concat")
			okRow(probed.GetSliceIdToData()[testSlice],
				"the check round: the data concat")

			node.Reset()
			reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			okRow(reply.GetCntlrInfo().GetSliceIdToData()[testSlice],
				"the converge: the data concat")
			for _, verb := range []string{"suspend", "reload"} {
				if node.hasCall("cmd dmsetup " + verb + " " + poolData) {
					t.Errorf("the converge ran dmsetup %s on the data "+
						"concat", verb)
				}
			}
		})
	}

	t.Run("two arrays hold the legs", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, opts)
		dataMd := mdDevOf(srv, false)
		leg2 := srv.nf.DmPath(legName(srv, testDataLeg2))
		// A split mirror: the group's array runs on leg 1, and a second
		// array, assembled from leg 2 alone, holds the other.
		node.mu.Lock()
		array := node.arrays[dataMd]
		array.members = slices.DeleteFunc(slices.Clone(array.members),
			func(member string) bool { return member == leg2 })
		array.degraded = 1
		node.publishArray(array)
		node.mu.Unlock()
		node.seedArray("/dev/md/split", "split", leg2)
		nodes := []string{node.mdNode(dataMd), node.mdNode("/dev/md/split")}
		slices.Sort(nodes)
		want := fmt.Sprintf("group %s: arrays %s and %s both hold legs "+
			"of the group", dataMd, nodes[0], nodes[1])

		node.Reset()
		reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		probed := checkRound(t, srv, 2)
		sameConcatRows(t, reply.GetCntlrInfo(), probed, want)
		for _, verb := range []string{"--fail", "--remove", "--add"} {
			assertNoCall(t, node, "cmd mdadm "+dataMd+" "+verb)
		}
	})

	t.Run("no array holds the legs", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, opts)
		dataMd := mdDevOf(srv, false)
		// TestGroupNeverCreatesBesideARunningArray's shape: both legs
		// switched out and parked, so the array that runs under the
		// group's name holds none of the legs leg_list names.
		next := reqOpts{revision: 3, primary: true, raid1: true, twoLegs: true}
		req := cntlrReq(next)
		grp := req.GetIdToSlice()[fmt.Sprintf(common.IdKeyFmt, testSlice)].
			GetDataGrpList()[0]
		grp.SpareLegList = grp.GetLegList()
		for i, leg := range grp.SpareLegList {
			leg.LegIdx = uint32(2 + i)
		}
		fresh := legOf(0x71, sideOf(0x72, testIp, testSvcId))
		fresh2 := legOf(0x73, sideOf(0x74, testIp2, testSvcId2))
		fresh2.LegIdx = 1
		grp.LegList = []*pb.Leg{fresh, fresh2}
		want := fmt.Sprintf("group %s: no array holds its legs", dataMd)

		node.Reset()
		reply, err := srv.SyncupCntlr(ctx, req)
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		if reply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("rejected: %v", reply.GetAgentReply())
		}
		probed := checkRound(t, srv, 3)
		sameConcatRows(t, reply.GetCntlrInfo(), probed, want)
		assertNoCall(t, node, "cmd mdadm --create")
		if node.arrays[dataMd] == nil {
			t.Fatalf("the array that runs under the group's name is gone")
		}
	})

	// noConcatChange fails on a converge that suspended or reloaded a concat.
	noConcatChange := func(t *testing.T, srv *CnAgentServer, node *fakeNode) {
		t.Helper()
		for _, pool := range []string{
			srv.nf.CnPoolMetaName(testCluster, testCn, testSp, testSlice),
			srv.nf.CnPoolDataName(testCluster, testCn, testSp, testSlice),
		} {
			for _, verb := range []string{"suspend", "reload"} {
				if node.hasCall("cmd dmsetup " + verb + " " + pool) {
					t.Errorf("the converge ran dmsetup %s on %s", verb, pool)
				}
			}
		}
	}

	t.Run("a degraded array", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.retryInterval = time.Hour
		syncupBoth(t, srv, opts)
		dataMd := mdDevOf(srv, false)
		leg2 := srv.nf.DmPath(legName(srv, testDataLeg2))
		nqn2 := srv.nf.SideToCnNqn(testCluster, testSp, testDataLeg2, testCn)
		// Leg 2 has left the data array, and its path is non-optimized, so
		// the converge leaves the array running on leg 1 alone
		// (TestGroupNeverAddsAnUnavailableLeg).
		node.mu.Lock()
		array := node.arrays[dataMd]
		array.members = slices.DeleteFunc(slices.Clone(array.members),
			func(member string) bool { return member == leg2 })
		array.degraded = 1
		node.publishArray(array)
		node.mu.Unlock()
		node.setAnaState(nqn2, testIp2, testSvcId2, "non-optimized")

		node.Reset()
		probed := checkRound(t, srv, 2)
		reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		assertNoCall(t, node, "cmd mdadm "+dataMd+" --add")
		for label, info := range map[string]*pb.CntlrInfo{
			"the converge": reply.GetCntlrInfo(), "the check round": probed,
		} {
			assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp],
				label+": the degraded group")
			assertOk(t, info.GetSliceIdToMeta()[testSlice],
				label+": the meta concat")
			assertOk(t, info.GetSliceIdToData()[testSlice],
				label+": the data concat")
		}
		noConcatChange(t, srv, node)
	})

	t.Run("a failed sync_completed read", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, opts)
		syncCompleted := sysfsBlockDir + node.mdNode(mdDevOf(srv, false))[len(
			"/dev"):] + "/md/sync_completed"
		node.mu.Lock()
		node.failReadAlways[syncCompleted] = true
		node.mu.Unlock()

		node.Reset()
		probed := checkRound(t, srv, 2)
		reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		for label, info := range map[string]*pb.CntlrInfo{
			"the converge": reply.GetCntlrInfo(), "the check round": probed,
		} {
			// The group's row reads the attribute; the concat rows read the
			// array's number alone.
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
				"read "+syncCompleted+": ", label+": the data group")
			assertOk(t, info.GetSliceIdToMeta()[testSlice],
				label+": the meta concat")
			assertOk(t, info.GetSliceIdToData()[testSlice],
				label+": the data concat")
		}
		assertOk(t, reply.GetCntlrInfo().GetSliceIdToDmPool()[testSlice],
			"the converge: the pool")
		noConcatChange(t, srv, node)
	})

	t.Run("an array did not answer", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, opts)
		dataMd := mdDevOf(srv, false)
		dataNode := node.mdNode(dataMd)
		listing := sysfsBlockDir + dataNode[len("/dev"):] + "/md"
		// The listing of the data array's md/ directory did not answer, so
		// the walk holds no array with the data group's legs and cannot
		// tell that none does; the meta group's array answers and matches.
		node.mu.Lock()
		node.killCmdAlways["list "+listing] = true
		node.mu.Unlock()
		want := fmt.Sprintf("group %s: no array holds legs of the group, "+
			"but %s did not answer: list %s: %v", dataMd, dataNode, listing,
			context.DeadlineExceeded)

		node.Reset()
		reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		probed := checkRound(t, srv, 2)
		sameConcatRows(t, reply.GetCntlrInfo(), probed, want)
	})
}

// TestFakeArrayKeepsItsOwnNumber pins the fake's two numbers of an md array,
// which TestConcatNumberComesFromTheMatchedArray pulls apart: the array's own
// (fakeArray.devNo, published at /sys/block/<node>/dev) and its
// /dev/md/<name> link's. With the link gone, the array is still held by the
// concat that maps its own number, so `mdadm --stop` is refused as the
// kernel refuses it, and a table naming that number still loads.
func TestFakeArrayKeepsItsOwnNumber(t *testing.T) {
	ctx := context.Background()
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, raid1: true, twoLegs: true})
	dataMd := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	node.mu.Lock()
	own := node.arrays[dataMd].devNo
	delete(node.devNo, dataMd)
	node.mu.Unlock()

	err := srv.md.Stop(ctx, dataMd)
	node.mu.Lock()
	stopped := node.arrays[dataMd] == nil
	node.mu.Unlock()
	if err == nil || stopped {
		t.Fatalf("mdadm --stop of the array the data concat maps gave %v "+
			"(stopped: %v)", err, stopped)
	}
	if err := srv.dm.Create(ctx, "fake-over-the-array",
		fmt.Sprintf("0 8 linear %s 0", own)); err != nil {
		t.Fatalf("a table over the array's own number %s: %v", own, err)
	}
}

// TestFakeDropsOneControllerOnly pins the fake's model of a subsystem
// leaving with its last controller: nvme1's and nvme-subsys1's directories
// and attributes go, and those of nvme10 and nvme-subsys10, whose names
// begin the same way, stay, with the namespace head behind them. A node with
// more than ten connections, as a cntlr at 32 slices holds, has both nvme1
// and nvme10, and a test that drops the first must keep the second.
func TestFakeDropsOneControllerOnly(t *testing.T) {
	node := newFakeNode()
	node.mu.Lock()
	defer node.mu.Unlock()
	for i := 0; i <= 10; i++ {
		node.plantSubsys(i, fmt.Sprintf("nqn.2024-01.io.test:%d", i),
			endpoint{testIp, testSvcId})
	}
	tenth := node.subsystems["nqn.2024-01.io.test:10"].ctrls[0].name
	if tenth != "nvme10" {
		t.Fatalf("fixture: nvme-subsys10's controller is %s", tenth)
	}
	dropSubsysLocked(node, "nqn.2024-01.io.test:1")
	for _, gone := range []string{
		sysfsNvmeSubsysDir + "/nvme-subsys1", sysfsNvmeCtrlDir + "/nvme1",
	} {
		if node.dirs[gone] {
			t.Errorf("%s is still there", gone)
		}
	}
	for _, dir := range []string{
		sysfsNvmeSubsysDir + "/nvme-subsys10",
		sysfsNvmeSubsysDir + "/nvme-subsys10/nvme10",
		sysfsNvmeSubsysDir + "/nvme-subsys10/nvme10n1",
		sysfsNvmeCtrlDir + "/nvme10",
		sysfsBlockDir + "/nvme10n1",
	} {
		if !node.dirs[dir] {
			t.Errorf("dropping nvme-subsys1 took %s", dir)
		}
	}
	for _, file := range []string{
		sysfsNvmeSubsysDir + "/nvme-subsys10/subsysnqn",
		sysfsNvmeCtrlDir + "/nvme10/address",
		sysfsNvmeCtrlDir + "/nvme10/state",
	} {
		if _, ok := node.files[file]; !ok {
			t.Errorf("dropping nvme-subsys1 took %s", file)
		}
	}
	if node.devNo["/dev/nvme10n1"] == "" {
		t.Errorf("dropping nvme-subsys1 took nvme10n1's number")
	}
}

// TestGroupRowReadsItsLegWrapperFirst pins the read order of a RedundNone
// group's row: its leg wrapper's number first (dmDevNo), then its own
// dm-linear, the order the converge reads them in. With both gone the row
// reads ERROR naming the wrapper, the device the group cannot be built
// without. Read the other way round it would read MISSING, which says only
// that the group is not built and sets no health epoch (dnv-worker.md,
// HL2).
func TestGroupRowReadsItsLegWrapperFirst(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	leg := legName(srv, testDataLeg)
	grp := grpName(srv, testDataGrp)
	node.mu.Lock()
	for _, name := range []string{grp, leg} {
		if node.dms[name] == nil {
			node.mu.Unlock()
			t.Fatalf("the fixture built no %s", name)
		}
		delete(node.dms, name)
		delete(node.devNo, "/dev/mapper/"+name)
	}
	node.mu.Unlock()

	row := checkRound(t, srv, 2).GetGrpIdToMdRaid()[testDataGrp]
	want := "dm device " + leg + " is not there"
	if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		row.GetDetails() != want {
		t.Fatalf("the group row is %v %q, want ERROR %q",
			row.GetStatus(), row.GetDetails(), want)
	}
}

// TestGroupProbeWalksOnce pins the cost of the sysfs md read (CN12): a Check
// round lists /sys/block and each array's md/ directory once, in its
// reading's one md walk, which serves all of its group rows, its concat rows
// and its verdict (CN28, CN30), not once per group and not once more for the
// verdict. A walk per group lists every array of the node per group: at 32
// slices, 64 groups over 64 arrays, 4160 listings a round where the whole
// round has one cntlr_interval. The walk lists no /sys/block entry that is
// not an array node, and the round reads nothing of one, its number
// included: a CN's dm devices and nvme heads outnumber its arrays many times
// over.
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
	if got := countCalls(node, "list "+sysfsBlockDir); got != 1 {
		t.Errorf("the round listed %s %d times, want 1 (its reading's md "+
			"walk)", sysfsBlockDir, got)
	}
	for _, array := range arrays {
		dir := sysfsBlockDir + "/" + array + "/md"
		if got := countCalls(node, "list "+dir); got != 1 {
			t.Errorf("the round listed %s %d times, want 1", dir, got)
		}
	}
	assertNoNonArrayRead(t, node, others, "the round")
}

// seedNonArrayEntries puts more of the block devices a CN really has beside
// its arrays at the top of the fake's /sys/block, which otherwise holds the
// arrays and the NVMe heads and loop devices the fake publishes a number
// for, so a reader that stopped filtering for array nodes has dm devices, a
// controller path and a disk to list too. dm-0 is given an md/ directory it
// never has on a real node, which a missing filter would list and record as
// an array.
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
// /sys/block entry seedNonArrayEntries planted, but the read of the number,
// /sys/block/<name>/dev, of an entry in mapped: a block device the pass maps
// by that number (blockDevNo).
func assertNoNonArrayRead(
	t *testing.T, node *fakeNode, names []string, label string,
	mapped ...string,
) {
	t.Helper()
	for _, name := range names {
		dir := sysfsBlockDir + "/" + name
		for _, call := range node.Calls() {
			if call == "read "+dir+"/dev" && slices.Contains(mapped, name) {
				continue
			}
			if call == "list "+dir ||
				strings.HasPrefix(call, "list "+dir+"/") ||
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
// groups share — and lists no /sys/block entry that is not an array node,
// nor reads anything of one but the number of a leg's NVMe head, which it
// maps (blockDevNo). A walk per group would list each five times.
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
	if got := countCalls(node, "list "+sysfsBlockDir); got != 2 {
		t.Errorf("the converge listed %s %d times, want 2 (the sweep's "+
			"and the groups' one walk)", sysfsBlockDir, got)
	}
	for _, array := range arrays {
		dir := sysfsBlockDir + "/" + array + "/md"
		if got := countCalls(node, "list "+dir); got != 2 {
			t.Errorf("the converge listed %s %d times, want 2", dir, got)
		}
	}
	// The fixture's first leg head is nvme0n1, an entry seedNonArrayEntries
	// plants as well; its number is the one read of such an entry the
	// converge makes.
	head := sysfsBlockDir + "/nvme0n1/dev"
	if countCalls(node, "read "+head) == 0 {
		t.Errorf("the converge never read %s, the number of the leg head "+
			"it maps", head)
	}
	assertNoNonArrayRead(t, node, others, "the converge", "nvme0n1")
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
				"list "+sysfsBlockDir+strings.TrimPrefix(other, "/dev")+
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
// read can fail to answer (the fake kills the listing or fails the read with
// an error that is not ENOENT); an md row counts toward cntlr health, so an
// error there would fail this sp's primary over for a read of another sp's
// array — the very fault a failover cannot fix. With the group's array built,
// a Check round and a converge read OK and run no mdadm; both reply a
// Leftover all the same, because their md enumeration keeps ListArrays'
// strict rule (CN21, CN30) and the same fault fails it — a reply code,
// never a row. With it not built yet, the unanswered array may be the
// group's own, so the converge neither creates nor assembles (ERROR); with
// every leg available the group registers no CN10 retry (CN12), but the
// namespace over the stack it leaves unbuilt waits inaccessible and
// registers it (CN16); the test's next SyncupCntlr, once the array answers,
// assembles it.
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
			node.killCmdAlways["list "+sysfsBlockDir+strings.TrimPrefix(
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
			if !retrying(t, srv) {
				t.Fatalf("a namespace waiting over an unbuilt stack " +
					"registered no retry")
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

// TestGroupProbeIsSysfsOnly pins the md probe (CN28). A primary's Check round
// over an array md has failed a member of (md/degraded 1, the member faulty)
// reports the md row from sysfs: OK with "degraded" in its details, and not
// one mdadm command, because an `mdadm --detail` loads a superblock from a
// member, blocks on a dead one past the soft timeout and would turn the row
// ERROR, which counts toward cntlr health and would fail the primary over. A
// rebuild reads in mdadm's own words
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
// lab read it in exactly that window — so an ERROR there would make the
// verdict a failover trigger. Every other state reads ERROR
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
// send the group into an assembly beside its running array. A member's
// block/dev, which only names a foreign member because members are compared
// by dm name (TestGroupMembersComparedByName), keeps the same rule.
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

// TestGroupMembersComparedByName pins CN12's member comparison: the held set
// is the dm names sysfs gave the array's members, compared with the leg_list
// wrapper names, and reconcileMembers reads no device number. Keyed by
// device number, a number read of a leg_list wrapper that did not answer —
// its `dmsetup info` with the number, killed here — would leave that wrapper
// out of the wanted set, its in-sync member would count as an extra, and an
// equal-revision converge would --fail and --remove it — the add loop would
// skip it for the same reason, and the mirror would run on one member until
// some unrelated converge.
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
		numberRead := dmNumberRead + legName(srv, legId)
		node.mu.Lock()
		node.killCmdAlways[numberRead] = true
		node.mu.Unlock()

		node.Reset()
		reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
		if err != nil {
			t.Fatalf("leg %#x's number read killed: SyncupCntlr: %v",
				legId, err)
		}
		for _, verb := range []string{"--fail", "--remove", "--add"} {
			assertNoCall(t, node, "cmd mdadm "+mdDev+" "+verb)
		}
		assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
			fmt.Sprintf("grp data, leg %#x's number read killed", legId))
		if got := node.arrays[mdDev].members; !slices.Equal(got, held) {
			t.Fatalf("leg %#x's number read killed: the array's members "+
				"changed to %v, want %v", legId, got, held)
		}

		node.mu.Lock()
		delete(node.killCmdAlways, numberRead)
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
// member, and neither this converge nor the retry puts it back while the
// path is not optimized; once it is and the leg's row reads OK, the verdict
// names the member and the converge of the syncup that brings puts it back
// (CN12, TestConvergePutsAFailedMemberBack). Until then each attempt is the
// cost cnagent.md, Known limits, records.
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
// available and its row reads OK, but md has not moved it out of its slot
// yet, which it does once no IO to the member is pending: an equal-revision
// converge runs no --fail, --remove or --add, reads OK, and the check round
// names nothing. A held set of in-sync members only would --add a member md
// still holds, which mdadm refuses (it opens the device O_EXCL, and md keeps
// its claim on a faulty member until the member is removed), and the group
// row would read ERROR on every converge; and a put-back that did not wait
// for the slot would --remove a member that may still have IO pending, which
// md refuses (TestConvergePutsAFailedMemberBack is the put-back itself). Nor
// is the faulty member late: only availability makes a member late (CN12),
// so the converge registers no CN10 retry either.
func TestGroupHeldFaultyMemberStaysHeld(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	ctx := context.Background()
	opts := reqOpts{revision: 2, primary: true, raid1: true, twoLegs: true}
	syncupBoth(t, srv, opts)
	probeAllLegs(t, srv)
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
			strings.Contains(member.State, "faulty") &&
			member.Slot != mdSlotNone
	}
	if !faulty {
		t.Fatalf("leg 2's member read %+v, want it faulty in its slot; the "+
			"case is vacuous otherwise", detail.Members)
	}
	if reply := checkCntlrVerdict(t, srv, 2); reply.GetCode() != 0 {
		t.Fatalf("a faulty member still in its slot was named: %v", reply)
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

// checkCntlrVerdict is the verdict of one check round of the fixture cntlr
// at a revision.
func checkCntlrVerdict(
	t *testing.T,
	srv *CnAgentServer,
	revision uint64,
) *pb.AgentReply {
	t.Helper()
	reply, _ := srv.checkCntlrRound(context.Background(),
		&pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: revision,
		}, nil)
	return reply.GetAgentReply()
}

// infoVerdict is the verdict of one GetCntlrInfo of the fixture cntlr.
func infoVerdict(t *testing.T, srv *CnAgentServer) *pb.AgentReply {
	t.Helper()
	reply, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	return reply.GetAgentReply()
}

// assertNamesOnly checks a verdict is the leftover code naming exactly the
// md members names, by dm name, and nothing else (cnagent.md CN30).
func assertNamesOnly(
	t *testing.T,
	reply *pb.AgentReply,
	label string,
	names ...string,
) {
	t.Helper()
	var want []string
	for _, name := range names {
		want = append(want, agent.LeftoverKindMdMember+":"+name)
	}
	wantDetails := fmt.Sprintf("leftover(%d): %s", len(want),
		strings.Join(want, ", "))
	if reply.GetCode() != common.ReplyCodeLeftover ||
		reply.GetDetails() != wantDetails {
		t.Fatalf("%s: code %d details %q, want code %d details %q",
			label, reply.GetCode(), reply.GetDetails(),
			common.ReplyCodeLeftover, wantDetails)
	}
}

// failOutOfSlot is what md leaves of a member it has failed once no IO to it
// is pending: "faulty,failfast", out of its slot, the array degraded.
func failOutOfSlot(node *fakeNode, mdDev, member string) {
	node.setMdSync(mdDev, 1, "idle", "none")
	node.setMemberState(mdDev, member, "faulty,failfast")
	node.setMemberSlot(mdDev, member, mdSlotNone)
}

// TestCheckRoundNamesAFailedMemberOnlyWhenItCanBePutBack pins the cntlr
// verdict's half of CN12's put-back (cnagent.md CN30): a check round and an
// info read name, with the leftover code, a leg_list member md has failed
// and moved out of its slot, on a leg whose path is live and optimized and
// whose row reads OK, and nothing else: no member still in its slot, no
// spare without a slot, none on a leg whose row is not OK — in error or
// before its first probe — or whose path is not optimized, whose
// controller's address is unknown or whose subsystem has no multipath
// namespace, none being recovered, and none of a standby, which wants no
// group. A drop changes no desired state, so without the name nothing would
// bring the converge that puts the member back; and a name on a leg the
// converge does not use would bring a syncup every round for nothing. Each
// case that names nothing also runs an equal-revision converge, which leaves
// the array as it is: the converge's own test reads the same row.
func TestCheckRoundNamesAFailedMemberOnlyWhenItCanBePutBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		// set puts leg 2's member, its leg and its row in the case's shape
		// on a converged primary whose legs have been probed, unless
		// unprobed.
		set   func(srv *CnAgentServer, node *fakeNode, mdDev, leg2 string)
		named bool
		// unprobed leaves every leg row PENDING: no probe has completed.
		unprobed bool
	}{
		{"failed out of its slot", func(srv *CnAgentServer, node *fakeNode,
			mdDev, leg2 string) {
			failOutOfSlot(node, mdDev, leg2)
		}, true, false},
		{"failed, still in its slot", func(srv *CnAgentServer,
			node *fakeNode, mdDev, leg2 string) {
			failOutOfSlot(node, mdDev, leg2)
			node.setMemberSlot(mdDev, leg2, "1")
		}, false, false},
		{"failed, row not OK", func(srv *CnAgentServer, node *fakeNode,
			mdDev, leg2 string) {
			failOutOfSlot(node, mdDev, leg2)
			st := srv.getCntlr(
				cntlrKey(testCluster, testCn, testSp, testCntlr))
			srv.mu.Lock()
			prober := st.probers[testDataLeg2]
			srv.mu.Unlock()
			prober.finish(fmt.Errorf("input/output error"))
		}, false, false},
		{"failed, path not optimized", func(srv *CnAgentServer,
			node *fakeNode, mdDev, leg2 string) {
			failOutOfSlot(node, mdDev, leg2)
			node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp,
				testDataLeg2, testCn), testIp2, testSvcId2, "non-optimized")
		}, false, false},
		{"recovering", func(srv *CnAgentServer, node *fakeNode,
			mdDev, leg2 string) {
			node.setMdSync(mdDev, 1, "recover", "100 / 200")
			node.setMemberState(mdDev, leg2, "spare,failfast")
		}, false, false},
		// A spare md holds without a slot: right after an --add, before md
		// gives it one, or one md cannot start recovering. The slot alone
		// would name it; only the faulty flag keeps it out.
		{"spare out of its slot", func(srv *CnAgentServer, node *fakeNode,
			mdDev, leg2 string) {
			node.setMdSync(mdDev, 1, "idle", "none")
			node.setMemberState(mdDev, leg2, "spare,failfast")
			node.setMemberSlot(mdDev, leg2, mdSlotNone)
		}, false, false},
		// After an agent restart or a promotion, before the first probe:
		// a row that is not OK is any row but OK, PENDING too.
		{"failed, row pending", func(srv *CnAgentServer, node *fakeNode,
			mdDev, leg2 string) {
			failOutOfSlot(node, mdDev, leg2)
		}, false, true},
		// The converge fails a leg on a controller whose address read does
		// not answer, while the other parts of the path still read live
		// and optimized.
		{"failed, a controller's address unknown", func(srv *CnAgentServer,
			node *fakeNode, mdDev, leg2 string) {
			failOutOfSlot(node, mdDev, leg2)
			ctrl := ctrlNameOf(node, legNqn(srv, testDataLeg2), testIp2,
				testSvcId2)
			if ctrl == "" {
				t.Fatalf("leg 2 has no controller; the case is vacuous")
			}
			node.mu.Lock()
			node.killReadAlways[sysfsNvmeCtrlDir+"/"+ctrl+"/address"] = true
			node.mu.Unlock()
		}, false, false},
		// The converge fails a leg whose subsystem has no multipath
		// namespace, while its controller's path still reads live and
		// optimized and the last probe passed.
		{"failed, no multipath namespace", func(srv *CnAgentServer,
			node *fakeNode, mdDev, leg2 string) {
			failOutOfSlot(node, mdDev, leg2)
			node.dropNsHead(legNqn(srv, testDataLeg2))
		}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			ctx := context.Background()
			opts := reqOpts{
				revision: 2, primary: true, raid1: true, twoLegs: true}
			syncupBoth(t, srv, opts)
			if !tc.unprobed {
				probeAllLegs(t, srv)
			}
			mdDev := srv.nf.MdPath(srv.nf.CnMdDevName(
				testCluster, testCn, testSp, 0, 0, false))
			leg2 := srv.nf.DmPath(legName(srv, testDataLeg2))
			if reply := checkCntlrVerdict(t, srv, 2); reply.GetCode() != 0 {
				t.Fatalf("the whole array's verdict: %v", reply)
			}
			tc.set(srv, node, mdDev, leg2)

			if tc.named {
				assertNamesOnly(t, checkCntlrVerdict(t, srv, 2),
					"check round", legName(srv, testDataLeg2))
				assertNamesOnly(t, infoVerdict(t, srv), "info read",
					legName(srv, testDataLeg2))
				return
			}
			if reply := checkCntlrVerdict(t, srv, 2); reply.GetCode() != 0 {
				t.Fatalf("check round: %v, want a clean verdict", reply)
			}
			if reply := infoVerdict(t, srv); reply.GetCode() != 0 {
				t.Fatalf("info read: %v, want a clean verdict", reply)
			}
			held := slices.Clone(node.arrays[mdDev].members)
			node.Reset()
			if _, err := srv.SyncupCntlr(ctx, cntlrReq(opts)); err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if got := node.callsMatching("cmd mdadm"); len(got) != 0 {
				t.Fatalf("the converge ran %v, want no mdadm", got)
			}
			if got := node.arrays[mdDev].members; !slices.Equal(got, held) {
				t.Fatalf("the array's members changed to %v, want %v",
					got, held)
			}
		})
	}
	t.Run("standby", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{
			revision: 2, primary: false, raid1: true, twoLegs: true})
		// A standby wants no group, so an array of the group's legs on its
		// node is a leftover of the md kind, never a member to put back.
		mdDev := srv.nf.MdPath(srv.nf.CnMdDevName(
			testCluster, testCn, testSp, 0, 0, false))
		leg2 := srv.nf.DmPath(legName(srv, testDataLeg2))
		node.seedArray(mdDev, "dnv-grp",
			srv.nf.DmPath(legName(srv, testDataLeg)), leg2)
		failOutOfSlot(node, mdDev, leg2)
		reply := checkCntlrVerdict(t, srv, 2)
		if !strings.Contains(reply.GetDetails(),
			agent.LeftoverKindMd+":"+node.mdNode(mdDev)) {
			t.Fatalf("the standby's verdict %v does not name the array; "+
				"the case is vacuous otherwise", reply)
		}
		if strings.Contains(reply.GetDetails(),
			agent.LeftoverKindMdMember+":") {
			t.Fatalf("the standby's verdict names a member: %v", reply)
		}
	})
}

// TestCheckRoundNamesNoMemberOfAnArrayThatIsNotRunning pins the "running
// array" part of the verdict's test (cnagent.md CN30): a member md has failed
// and moved out of its slot, on a leg that can take it, is named only while
// its group's array runs. An array that is not running — CN29's broken, where
// md charged an error to the last in-sync member, among them — and one that
// holds a foreign member read ERROR, and their converge refuses before it
// reaches the members (TestGroupProbeArrayStates,
// TestGroupForeignMemberIsAnError), so a name there would bring a syncup
// every round that puts nothing back; a missing one reads MISSING, and a
// stopped array lacks every member. Each case first sees the member named
// while the array runs, and then the group row read as the case wants, so it
// is not vacuous; a broken or foreign array may still make the verdict
// unclean for a reason of its own, so the case asks only that no member is
// named.
func TestCheckRoundNamesNoMemberOfAnArrayThatIsNotRunning(t *testing.T) {
	const (
		failed  = pb.ResStatus_RES_STATUS_ERROR
		missing = pb.ResStatus_RES_STATUS_MISSING
	)
	for _, tc := range []struct {
		name string
		// set takes the running array, leg 2's member failed out of its
		// slot, out of the running states.
		set    func(node *fakeNode, mdDev, leg2 string)
		status pb.ResStatus
	}{
		{"broken", func(node *fakeNode, mdDev, leg2 string) {
			node.setArrayState(mdDev, "broken")
		}, failed},
		{"inactive", func(node *fakeNode, mdDev, leg2 string) {
			node.setArrayState(mdDev, "inactive")
		}, failed},
		{"clear", func(node *fakeNode, mdDev, leg2 string) {
			node.setArrayState(mdDev, "clear")
		}, missing},
		// Stopped by hand while both legs serve: every leg_list member is
		// lacking, and none is named.
		{"stopped", func(node *fakeNode, mdDev, leg2 string) {
			node.mu.Lock()
			defer node.mu.Unlock()
			key, array := node.arrayByDev(mdDev)
			node.unpublishArray(array)
			delete(node.arrays, key)
		}, missing},
		{"foreign member", func(node *fakeNode, mdDev, leg2 string) {
			held := append(slices.Clone(node.arrays[mdDev].members),
				"/dev/sdb1")
			node.seedArray(mdDev, node.arrays[mdDev].name, held...)
			failOutOfSlot(node, mdDev, leg2)
		}, failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			ctx := context.Background()
			syncupBoth(t, srv, reqOpts{
				revision: 2, primary: true, raid1: true, twoLegs: true})
			probeAllLegs(t, srv)
			mdDev := srv.nf.MdPath(srv.nf.CnMdDevName(
				testCluster, testCn, testSp, 0, 0, false))
			leg2 := srv.nf.DmPath(legName(srv, testDataLeg2))
			failOutOfSlot(node, mdDev, leg2)
			assertNamesOnly(t, checkCntlrVerdict(t, srv, 2),
				"running array", legName(srv, testDataLeg2))
			tc.set(node, mdDev, leg2)

			reply, info := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
				ClusterId: testCluster, CnId: testCn,
				CntlrPointer: cntlrPtr(), Revision: 2,
			}, nil)
			infoReply, err := srv.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
				ClusterId: testCluster, CnId: testCn,
				CntlrPointer: cntlrPtr()})
			if err != nil {
				t.Fatalf("GetCntlrInfo: %v", err)
			}
			for _, read := range []struct {
				label string
				reply *pb.AgentReply
				row   *pb.ResInfo
			}{
				{"check round", reply.GetAgentReply(),
					info.GetGrpIdToMdRaid()[testDataGrp]},
				{"info read", infoReply.GetAgentReply(),
					infoReply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]},
			} {
				if read.row.GetStatus() != tc.status {
					t.Fatalf("%s: the md row reported %v %q, want %v; the "+
						"case is vacuous otherwise", read.label,
						read.row.GetStatus(), read.row.GetDetails(),
						tc.status)
				}
				if strings.Contains(read.reply.GetDetails(),
					agent.LeftoverKindMdMember+":") {
					t.Fatalf("%s: the verdict names a member of an array "+
						"that is not running: %v", read.label, read.reply)
				}
			}
		})
	}
}

// TestConvergePutsAFailedMemberBack pins CN12's put-back: the converge of the
// syncup the verdict brings removes the member md has failed and moved out of
// its slot and adds it back, in that order, one command each, and touches
// nothing else — no --fail, the other member and the meta array left alone.
// mdadm turns that --add into a re-add, which md resyncs from its write-intent
// bitmap. The removed member is no longer held, so the add comes in the same
// pass; an add loop that still counted it held would leave the array
// degraded. The member is not late (CN12), so no CN10 retry is registered,
// and the next check round reads clean.
func TestConvergePutsAFailedMemberBack(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	ctx := context.Background()
	opts := reqOpts{revision: 2, primary: true, raid1: true, twoLegs: true}
	syncupBoth(t, srv, opts)
	probeAllLegs(t, srv)
	mdDev := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	leg1 := srv.nf.DmPath(legName(srv, testDataLeg))
	leg2 := srv.nf.DmPath(legName(srv, testDataLeg2))
	failOutOfSlot(node, mdDev, leg2)
	assertNamesOnly(t, checkCntlrVerdict(t, srv, 2), "check round",
		legName(srv, testDataLeg2))

	node.Reset()
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	removeCall := "cmd mdadm " + mdDev + " --remove " + leg2
	addCall := "cmd mdadm " + mdDev + " --add --failfast " + leg2
	assertOrder(t, node, removeCall, addCall)
	mdadm := node.callsMatching("cmd mdadm")
	if !slices.Equal(mdadm, []string{removeCall, addCall}) {
		t.Fatalf("the converge ran %v, want exactly %q then %q",
			mdadm, removeCall, addCall)
	}
	assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
		"grp data after the put-back")
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("the converge's reply %v, want a clean verdict",
			reply.GetAgentReply())
	}
	if got := node.arrays[mdDev].members; !slices.Equal(got,
		[]string{leg1, leg2}) {
		t.Fatalf("the array holds %v, want %v", got, []string{leg1, leg2})
	}
	if retrying(t, srv) {
		t.Fatalf("the put-back registered the CN10 retry")
	}
	if verdict := checkCntlrVerdict(t, srv, 2); verdict.GetCode() != 0 {
		t.Fatalf("the check round after the put-back: %v", verdict)
	}
}

// TestCheckRoundNamesALackingMemberAndTheConvergeAddsIt is the put-back's
// other half (CN12, CN30): a running array that lacks a leg_list member on a
// leg that serves with its row OK — a remove that went through before its
// add failed, or a spare switch whose --remove was killed — is named by the
// verdict, so the worker's re-sent syncup brings the converge that adds it;
// nothing else re-drives it. Before its leg's first probe its row reads
// PENDING and the verdict names nothing, though the converge's add, which
// asks for availability alone, adds it all the same.
func TestCheckRoundNamesALackingMemberAndTheConvergeAddsIt(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	ctx := context.Background()
	opts := reqOpts{revision: 2, primary: true, raid1: true, twoLegs: true}
	syncupBoth(t, srv, opts)
	mdDev := srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, false))
	leg2 := srv.nf.DmPath(legName(srv, testDataLeg2))
	node.mu.Lock()
	array := node.arrays[mdDev]
	array.members = slices.DeleteFunc(slices.Clone(array.members),
		func(member string) bool { return member == leg2 })
	array.degraded = 1
	node.publishArray(array)
	node.mu.Unlock()
	if reply := checkCntlrVerdict(t, srv, 2); reply.GetCode() != 0 {
		t.Fatalf("a lacking member on a leg with a pending row was named: %v",
			reply)
	}
	probeAllLegs(t, srv)
	assertNamesOnly(t, checkCntlrVerdict(t, srv, 2), "check round",
		legName(srv, testDataLeg2))
	assertNamesOnly(t, infoVerdict(t, srv), "info read",
		legName(srv, testDataLeg2))

	node.Reset()
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(opts)); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	addCall := "cmd mdadm " + mdDev + " --add --failfast " + leg2
	if mdadm := node.callsMatching("cmd mdadm"); !slices.Equal(mdadm,
		[]string{addCall}) {
		t.Fatalf("the converge ran %v, want exactly %q", mdadm, addCall)
	}
	if verdict := checkCntlrVerdict(t, srv, 2); verdict.GetCode() != 0 {
		t.Fatalf("the check round after the add: %v", verdict)
	}
}

// TestARefusedPutBackRemoveLeavesTheAddUnrun pins the put-back's error rule
// (CN12): a --remove of the failed member that md refuses, or that is killed
// before it does anything, is the group's error for the pass, and the add is
// not run, since mdadm would refuse to add a member md still holds. Nothing
// is remembered: the next check round names the member again, so the
// worker's re-sent syncup tries again.
func TestARefusedPutBackRemoveLeavesTheAddUnrun(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(node *fakeNode, line string)
	}{
		{"refused", func(node *fakeNode, line string) {
			node.failCmd[line] = "mdadm: hot remove failed: " +
				"Device or resource busy"
		}},
		{"killed, no effect", func(node *fakeNode, line string) {
			node.killCmdNoEffect[line] = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			ctx := context.Background()
			opts := reqOpts{
				revision: 2, primary: true, raid1: true, twoLegs: true}
			syncupBoth(t, srv, opts)
			probeAllLegs(t, srv)
			mdDev := srv.nf.MdPath(srv.nf.CnMdDevName(
				testCluster, testCn, testSp, 0, 0, false))
			leg2 := srv.nf.DmPath(legName(srv, testDataLeg2))
			failOutOfSlot(node, mdDev, leg2)
			removeCall := "cmd mdadm " + mdDev + " --remove " + leg2
			tc.arm(node, removeCall)

			node.Reset()
			reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if got := countCalls(node, removeCall); got != 1 {
				t.Fatalf("the converge ran %q %d times, want once",
					removeCall, got)
			}
			assertNoCall(t, node, "cmd mdadm "+mdDev+" --add")
			assertNoCall(t, node, "cmd mdadm "+mdDev+" --fail")
			row := reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]
			if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
				t.Fatalf("grp data after a refused remove: %v %q, want ERROR",
					row.GetStatus(), row.GetDetails())
			}
			assertNamesOnly(t, checkCntlrVerdict(t, srv, 2),
				"check round after the refused remove",
				legName(srv, testDataLeg2))
		})
	}
}

// TestGroupNeverAddsAnUnavailableLeg pins the add loop's one guard now that
// members are compared by dm name (CN12): a leg_list member the array lacks
// is added only when its leg is available. Leg 2's wrapper is built but its
// path is non-optimized, so an --add would put a member on a path this pass
// may not use; the degraded array is left running on leg 1 and reads OK.
// reconcileMembers reads neither leg's number. The late member registers
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
		assertNoCall(t, node, dmNumberRead+legName(srv, legId))
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

// metaRetrying reads the fixture cntlr's registration for CN18's
// clone-metadata retry under s.mu, the lock that guards it.
func metaRetrying(t *testing.T, srv *CnAgentServer) bool {
	t.Helper()
	st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	if st == nil {
		t.Fatalf("the fixture cntlr is not known")
	}
	return metaRetryingOf(srv, st)
}

// metaRetryingOf is metaRetrying for a cntlr state the test holds, which it
// can still read once the cntlr has been dropped.
func metaRetryingOf(srv *CnAgentServer, st *cntlrState) bool {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return st.metaRetrying
}

// retryAttempt runs one attempt of a background retry by hand — the CN10
// retry's or CN18's clone-metadata retry's, which run the same attempt: the
// call connectRetryLoop makes on each tick, on the ctx it makes it on. The
// tests that use it set a retryInterval no test outlives, and a
// metaRetryInterval too where a clone waits for room, so the loops themselves
// never fire and every attempt is one the test ran.
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

// TestLateMembersRegisterTheRetry pins the late-member retry (CN12).
// The worker's sides-first hold is bounded ([D16]), so a promoted standby can
// still read its legs before the sides' ANA flips have reached its sysfs: the
// paths are live but still non-optimized, those legs are not available, and a
// group left with no available leg reports "no available leg" with no mdadm
// run. Nothing re-drives that converge — the worker re-syncs on a revision or a
// reply code, never on a row — so it registers the CN10 retry itself. A late
// member in any group registers it, not just one in the last group the pass
// converges: "meta only" leaves the meta group, which the pass converges before
// the data group, late and the data group whole. An attempt that still finds a
// member late keeps the retry, since the sides' flips can take longer than one
// CnConnectRetryInterval. Once the paths read optimized, the retry's next
// attempt assembles the late arrays and builds the stack above them, and, with
// no member late, stops the retry.
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
	// The start from one member is exactly the node path, the array name and
	// that member, as from both members: nothing such as --run, --force or
	// --scan rides along to override mdadm's own gate. The cn suite's
	// md-refusal stage builds the same command over devices of its own with
	// md_assemble_args (integtest/cnagent_test.sh).
	lone := "cmd " + start + "--name " +
		srv.nf.CnMdArrayName(testSp, 0, 0, false) + " " + leg1
	both := lone + " " + leg2
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
	if len(started) != 1 || started[0] != lone {
		t.Fatalf("want one start of the data array from leg 1 alone, "+
			"exactly %q, got %q", lone, started)
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
	if got := node.callsMatching("cmd " + start); len(got) != 1 ||
		got[0] != lone {
		t.Fatalf("an attempt before the flip ran the starts %q of the data "+
			"array, want exactly %q", got, lone)
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
	if len(started) != 1 || started[0] != both {
		t.Fatalf("want one assembly of the data array from both legs, "+
			"exactly %q, got %q:\n%s", both, started,
			strings.Join(node.Calls(), "\n"))
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
// (cnagent.md CN12). The promotion
// starts the array degraded and reports the group
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

// TestLateRedundNoneLegsWaitForTheRetry is the same for a RedundNone SP,
// redund_conf's default (CN12): a group's dm-linear is created only over an
// available leg, since the layers above do IO through it and a side that has
// not flipped to this CN yet exports dm-error to it. A promotion whose meta
// leg, data leg or both are late leaves each late group unbuilt ("no
// available leg"), builds no pool, raid0 or ns-dev serving device above it,
// and keeps the namespace inaccessible with its ns-dev parked, so the hosts
// wait rather than read IO errors from the late leg's blocks (CN16); "data
// only" is the case whose pool could be built, its metadata read through
// the meta leg that has flipped. The late member registers the CN10 retry; an
// attempt while a leg is still late keeps it, and the attempt after the
// paths read optimized builds the late groups and the stack, puts the ns-dev
// on the raid0, only then moves the namespace optimized, and stops it.
func TestLateRedundNoneLegsWaitForTheRetry(t *testing.T) {
	for _, tc := range []struct {
		name string
		// late are the legs whose paths read non-optimized until the flip,
		// lateGrps the groups that leaves unbuilt, and wholeGrps the ones
		// the promotion builds all the same.
		late      []uint64
		lateGrps  []uint64
		wholeGrps []uint64
	}{
		{"meta and data", []uint64{testMetaLeg, testDataLeg},
			[]uint64{testMetaGrp, testDataGrp}, nil},
		{"data only", []uint64{testDataLeg},
			[]uint64{testDataGrp}, []uint64{testMetaGrp}},
		{"meta only", []uint64{testMetaLeg},
			[]uint64{testMetaGrp}, []uint64{testDataGrp}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			ctx := context.Background()
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			if _, err := srv.SyncupCntlr(ctx,
				cntlrReq(reqOpts{revision: 3})); err != nil {
				t.Fatalf("demote: %v", err)
			}
			for _, grpId := range append(tc.lateGrps, tc.wholeGrps...) {
				if _, ok := node.dms[grpName(srv, grpId)]; ok {
					t.Fatalf("the standby kept group %d's dm-linear", grpId)
				}
			}
			assertAna(t, node, testNqn, "3", "the standby")
			setAna := func(state string) {
				for _, legId := range tc.late {
					node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp,
						legId, testCn), testIp, testSvcId, state)
				}
			}
			setAna("non-optimized")
			ana := "writedirect " + anaPath(testNqn, 1) + "="
			assertWaiting := func(label string) {
				t.Helper()
				for _, grpId := range tc.lateGrps {
					grp := grpName(srv, grpId)
					assertNoCall(t, node, "cmd dmsetup create "+grp)
					if _, ok := node.dms[grp]; ok {
						t.Fatalf("%s: group %d's dm-linear was built over "+
							"its late leg", label, grpId)
					}
				}
				for _, name := range []string{poolName(srv),
					raid0Name(srv, testTd)} {
					if _, ok := node.dms[name]; ok {
						t.Fatalf("%s: %s was built over a late leg",
							label, name)
					}
				}
				assertNoCall(t, node, ana+"1")
				assertAna(t, node, testNqn, "3", label)
				assertParked(t, srv, node, testNs, testTd, label)
				if !retrying(t, srv) {
					t.Fatalf("%s: a late RedundNone leg registered no "+
						"retry: nothing else would ever build its group",
						label)
				}
			}

			node.Reset()
			reply, err := srv.SyncupCntlr(ctx,
				cntlrReq(reqOpts{revision: 4, primary: true}))
			if err != nil {
				t.Fatalf("promote: %v", err)
			}
			if reply.GetAgentReply().GetCode() != 0 {
				t.Fatalf("rejected: %v", reply.GetAgentReply())
			}
			assertWaiting("the promotion")
			rows := reply.GetCntlrInfo().GetGrpIdToMdRaid()
			for _, grpId := range tc.lateGrps {
				assertErrorDetails(t, rows[grpId], "no available leg",
					fmt.Sprintf("grp %d before the flip", grpId))
			}
			for _, grpId := range tc.wholeGrps {
				if !node.hasCall("cmd dmsetup create " +
					grpName(srv, grpId)) {
					t.Fatalf("group %d's dm-linear, its leg available, "+
						"was not built", grpId)
				}
				assertOk(t, rows[grpId],
					fmt.Sprintf("grp %d before the flip", grpId))
			}
			assertOk(t, reply.GetCntlrInfo().GetNsIdToNamespace()[testNs],
				"the promotion's namespace row")

			node.Reset()
			retryAttempt(t, srv)
			assertWaiting("an attempt before the flip")

			setAna("optimized")
			node.Reset()
			retryAttempt(t, srv)
			raid0No := devNoOf(node, raid0Name(srv, testTd))
			if raid0No == "" {
				t.Fatalf("the attempt after the flip built no raid0")
			}
			for _, grpId := range tc.lateGrps {
				assertOrder(t, node,
					"cmd dmsetup create "+grpName(srv, grpId),
					"cmd dmsetup create "+poolName(srv),
					"cmd dmsetup create "+raid0Name(srv, testTd),
					"cmd dmsetup reload "+nsDevName(srv, testNs)+
						" --table "+nsDevTableOn(raid0No),
					ana+"1")
			}
			assertAna(t, node, testNqn, "1", "the attempt after the flip")
			info := probedCntlrInfo(t, srv)
			for _, grpId := range []uint64{testMetaGrp, testDataGrp} {
				assertOk(t, info.GetGrpIdToMdRaid()[grpId],
					fmt.Sprintf("grp %d after the flip", grpId))
			}
			assertOk(t, info.GetSliceIdToDmPool()[testSlice],
				"pool after the flip")
			assertOk(t, info.GetNsIdToDmLinear()[testNs],
				"ns-dev after the flip")
			assertOk(t, info.GetNsIdToNamespace()[testNs],
				"namespace after the flip")
			if retrying(t, srv) {
				t.Fatalf("the retry outlived its last late leg")
			}
		})
	}
}

// TestARedundNoneGroupKeepsItsDeviceOverALateLeg is the other half of that
// rule (CN12): a primary that serves keeps a RedundNone group's dm-linear
// that has the table it wants whatever its leg's availability — no
// teardown, no reload, no suspend — so it goes on serving through a short
// loss of the leg's path, its namespace optimized, while the late member
// registers the CN10 retry. A dm-linear that maps something else is not
// reloaded over a late leg either: the group reads "no available leg", and
// the attempt after the leg reads optimized again reloads it. Either way that
// attempt ends the retry.
func TestARedundNoneGroupKeepsItsDeviceOverALateLeg(t *testing.T) {
	for _, tc := range []struct {
		name  string
		legId uint64
		grpId uint64
		// drift gives the group's dm-linear a table it does not want.
		drift bool
	}{
		{"meta leg", testMetaLeg, testMetaGrp, false},
		{"data leg", testDataLeg, testDataGrp, false},
		{"data leg, its table drifted", testDataLeg, testDataGrp, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			assertAna(t, node, testNqn, "1", "the fixture")
			grp := grpName(srv, tc.grpId)
			want := node.dms[grp].table
			if tc.drift {
				// The same leg wrapper, one sector further in.
				fields := strings.Fields(want)
				offset, err := strconv.ParseUint(fields[len(fields)-1], 10, 64)
				if len(fields) != 5 || err != nil {
					t.Fatalf("fixture: the group table is %q", want)
				}
				fields[len(fields)-1] = strconv.FormatUint(offset+1, 10)
				node.dms[grp].table = strings.Join(fields, " ")
			}
			kept := node.dms[grp].table
			nqn := srv.nf.SideToCnNqn(testCluster, testSp, tc.legId, testCn)
			node.setAnaState(nqn, testIp, testSvcId, "non-optimized")

			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 3, primary: true}))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			for _, call := range node.Mutations() {
				if strings.Contains(call, grp) {
					t.Fatalf("the group's dm-linear was changed over its "+
						"late leg: %q", call)
				}
			}
			if dm := node.dms[grp]; dm == nil || dm.table != kept ||
				dm.suspended {
				t.Fatalf("the group's dm-linear is %+v, want it live on %q",
					dm, kept)
			}
			row := reply.GetCntlrInfo().GetGrpIdToMdRaid()[tc.grpId]
			if tc.drift {
				assertErrorDetails(t, row, "no available leg",
					"the drifted group over its late leg")
			} else {
				assertOk(t, row, "the kept group")
			}
			assertNoCall(t, node, "writedirect "+anaPath(testNqn, 1)+"=")
			assertAna(t, node, testNqn, "1", "the primary over a late leg")
			if !retrying(t, srv) {
				t.Fatalf("a late RedundNone leg registered no retry")
			}

			node.setAnaState(nqn, testIp, testSvcId, "optimized")
			node.Reset()
			retryAttempt(t, srv)
			if tc.drift {
				assertOrder(t, node, "cmd dmsetup reload "+grp)
			} else {
				assertNoCall(t, node, "cmd dmsetup reload "+grp)
			}
			if got := node.dms[grp].table; got != want {
				t.Fatalf("the group's dm-linear is %q after the flip, "+
					"want %q", got, want)
			}
			assertAna(t, node, testNqn, "1", "the attempt after the flip")
			if retrying(t, srv) {
				t.Fatalf("the retry outlived the late leg")
			}
		})
	}
}

// TestLateMemberRetryScope pins what never counts as a late member (CN12):
// each case holds an unavailable leg that TestLateMembersRegisterTheRetry
// shows would register the retry as a member of a wanted group. A standby
// wants no group (architecture.md, Standby cntlr) — its non-optimized paths
// are its designed steady
// state — and nor does a primary whose sp_level suppresses its groups
// (CN19): at NO_SIDE, where no leg is wanted, every member would otherwise
// read unavailable. A deferred group builds no array at all ([D15]), even
// when its leg_list holds a provisioned member that is unavailable, and a
// spare is never a member (architecture.md, Spare legs).
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
// either: the pass converges it, and its late leg leaves its dm-linear
// unbuilt, so its member counts all the same — only gp.deferred excludes a
// group, and effective() is not its negation.
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
			// Converged, not a concat target yet: its late leg leaves its
			// dm-linear unbuilt.
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp3],
				"no available leg", "grp grown 2, its leg late")
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

// TestUnansweredAddressReadKeepsTheController pins CN10's "unknown, never
// unwanted". A controller's sysfs `address` is the only thing that ties it to
// a side, so a read of it that did not answer — any error but ENOENT, such
// as the soft timeout spent waiting for an OsClient slot, which stalled sysfs
// reads can hold — leaves the pass not knowing which side, if any, that
// controller serves. Read as an empty address it would match no desired side,
// so the connect step would ask for its side again: a duplicate the host
// refuses (EALREADY, and so does the fake), spending the pass's connect budget
// on refused connects. And whenever a connect of the pass succeeded — the
// leg's controller had been lost, or another side had none — and the
// re-read after it met an address that did not answer, disconnectDeadPaths
// would retire that controller as a dead path: the live, only path of a
// healthy leg, on a primary an md member, on a migrating leg the serving src
// path.
// The pass must do neither: it disconnects nothing, connects nothing beside
// it, and fails the leg's converge with the read in its row, which registers
// the retry; the retry's next attempt, the read answering, finds the
// controllers where they were. Once the address stops answering, every read
// of it fails for the rest of the pass, and the cases fail it both ways the
// fake can: refused (failRead) and cut off at the soft timeout (killRead).
//
// The reconnected cases, on a standby and on a primary, are that re-read
// itself: the leg's controller was lost (a reconnect refused with DNR
// deletes it), the pass connects the side again, and the new controller's
// address is the one that does not answer. The migrating case is the other
// shape of it: the pass connects the leg's newly provisioned dst side, and
// the src controller's address stops answering while that connect runs.
func TestUnansweredAddressReadKeepsTheController(t *testing.T) {
	for _, tc := range []struct {
		name string
		// opts is the desired state the pass converges. The cntlr is first
		// built from it, and the pass is an equal-revision converge —
		// except for migrate.
		opts reqOpts
		// reconnect loses the leg's controller before the pass, whose
		// connect of the side then mints the one that does not answer.
		reconnect bool
		// migrate builds the cntlr one revision earlier with every dst side
		// still zeroing; the pass provisions them, and its connect of the
		// data leg's dst side is when the src address stops answering.
		migrate bool
		// killed cuts the read off at the soft timeout (killReadAlways)
		// rather than refusing it (failReadAlways).
		killed bool
		// settled is the leg row once the address answers: the standby's
		// transport verdict (CN11), or the primary's prober outcome, which
		// reads PENDING because no test waits a CnLegProbeInterval.
		settled func(t *testing.T, info *pb.ResInfo, label string)
	}{
		{name: "standby", opts: reqOpts{revision: 2}, settled: assertOk},
		{name: "primary",
			opts:   reqOpts{revision: 2, primary: true, raid1: true},
			killed: true, settled: assertPending},
		{name: "standby reconnected", opts: reqOpts{revision: 2},
			reconnect: true, killed: true, settled: assertOk},
		{name: "primary reconnected",
			opts:      reqOpts{revision: 2, primary: true, raid1: true},
			reconnect: true, settled: assertPending},
		{name: "standby migrating",
			opts:    reqOpts{revision: 3, extraSide: true},
			migrate: true, killed: true, settled: assertOk},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			built := tc.opts
			if tc.migrate {
				built.revision--
				built.unprovisionedExtraSide = true
			}
			syncupBoth(t, srv, built)
			nqn := legNqn(srv, testDataLeg)
			ctrlsOf := func() []fakeCtrl {
				node.mu.Lock()
				defer node.mu.Unlock()
				var out []fakeCtrl
				if subsys := node.subsystems[nqn]; subsys != nil {
					for _, ctrl := range subsys.ctrls {
						out = append(out, *ctrl)
					}
				}
				return out
			}
			ctrls := ctrlsOf()
			if len(ctrls) != 1 || ctrls[0].state != "live" {
				t.Fatalf("the data leg holds %+v, want one live controller",
					ctrls)
			}
			// ctrl is the controller whose address does not answer.
			ctrl, connects := ctrls[0].name, 0
			if tc.reconnect {
				node.mu.Lock()
				node.nvmeDisconnect([]string{"disconnect", "--device", ctrl})
				ctrl = fmt.Sprintf("nvme%d", node.nextCtrl)
				node.mu.Unlock()
				connects = 1
			}
			address := sysfsNvmeCtrlDir + "/" + ctrl + "/address"
			node.mu.Lock()
			hooks := node.failReadAlways
			if tc.killed {
				hooks = node.killReadAlways
			}
			if tc.migrate {
				// Every leg's dst side is connected, the meta leg's too.
				// The hook runs under the fake's lock.
				connects = 2
				node.duringConnect[connectKey(nqn, testIp2, testSvcId2)] =
					func() { hooks[address] = true }
			} else {
				hooks[address] = true
			}
			node.mu.Unlock()

			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(tc.opts))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if reply.GetAgentReply().GetCode() != 0 {
				t.Fatalf("rejected: %v", reply.GetAgentReply())
			}
			if got := node.callsMatching("cmd nvme disconnect"); len(got) != 0 {
				t.Fatalf("an unanswered address read disconnected a live "+
					"leg: %q", got)
			}
			if got := node.callsMatching("cmd nvme connect"); len(got) !=
				connects {
				t.Fatalf("the pass made %d connects, want %d: %q",
					len(got), connects, got)
			}
			assertNoCall(t, node, "cmd mdadm")
			assertErrorDetails(t,
				reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg], address,
				"leg data, its address unanswered")
			if !retrying(t, srv) {
				t.Fatalf("a leg whose controller is unknown registered no " +
					"retry: nothing else would re-run its converge")
			}
			// want is what the data leg holds after the pass: ctrl, and on
			// a migrating leg the dst side's new controller beside it.
			want := []string{ctrl}
			if tc.migrate {
				for _, held := range ctrlsOf() {
					if held.trAddr == testIp2 && held.trSvcId == testSvcId2 {
						want = append(want, held.name)
					}
				}
				if len(want) != 2 {
					t.Fatalf("the pass did not connect the dst side: %+v",
						ctrlsOf())
				}
			}
			slices.Sort(want)
			assertHolds := func(label string) {
				t.Helper()
				var held []string
				for _, ctrl := range ctrlsOf() {
					held = append(held, ctrl.name)
				}
				slices.Sort(held)
				if !slices.Equal(held, want) {
					t.Fatalf("%s: the data leg holds %q, want %q",
						label, held, want)
				}
			}
			assertHolds("the unanswered pass")

			node.mu.Lock()
			delete(hooks, address)
			delete(node.duringConnect, connectKey(nqn, testIp2, testSvcId2))
			node.mu.Unlock()
			node.Reset()
			retryAttempt(t, srv)
			for _, verb := range []string{
				"cmd nvme connect", "cmd nvme disconnect", "cmd mdadm"} {
				assertNoCall(t, node, verb)
			}
			if retrying(t, srv) {
				t.Fatalf("the retry outlived the unanswered read")
			}
			assertHolds("the retry's attempt")
			tc.settled(t, probedCntlrInfo(t, srv).GetLegIdToLeg()[testDataLeg],
				"leg data, its address answering")
		})
	}
}

// TestAbsentAddressIsAGoneController pins the other half of CN10's sysfs
// walk: an absent `address` is an answer. A fabrics controller has one for
// as long as its device exists, and its subsystem keeps listing it after the
// device was deleted, until the last reference to it drops, so a controller
// whose `address` is absent is gone: no side's path, and not unknown. The
// side it alone served reads unconnected and is connected again (no
// duplicate: the kernel's check passes over a controller being deleted);
// the gone controller is not disconnected, its device being deleted
// already; and the leg converges with no retry, on this pass and on the
// next, while the subsystem still lists it. Read as unknown, it would hold
// the leg in ERROR and its side unconnected for as long as the listing
// lasts; read as an empty address, it would draw a disconnect of a device
// that is no longer there.
func TestAbsentAddressIsAGoneController(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts reqOpts
		// settled is the leg row: the standby's transport verdict (CN11),
		// or the primary's prober outcome, PENDING in a converge reply.
		settled func(t *testing.T, info *pb.ResInfo, label string)
	}{
		{name: "standby", opts: reqOpts{revision: 2, extraSide: true},
			settled: assertOk},
		{name: "primary",
			opts: reqOpts{revision: 2, primary: true, raid1: true,
				extraSide: true},
			settled: assertPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			syncupBoth(t, srv, tc.opts)
			nqn := legNqn(srv, testDataLeg)
			gone := node.deleteCtrlDevice(nqn, testIp2, testSvcId2)
			if gone == "" {
				t.Fatalf("the data leg has no controller for its dst side")
			}
			for pass, label := range []string{"the pass", "the next pass"} {
				node.Reset()
				reply, err := srv.SyncupCntlr(context.Background(),
					cntlrReq(tc.opts))
				if err != nil {
					t.Fatalf("%s: SyncupCntlr: %v", label, err)
				}
				if reply.GetAgentReply().GetCode() != 0 {
					t.Fatalf("%s: rejected: %v", label, reply.GetAgentReply())
				}
				if !node.hasCall("read " + sysfsNvmeCtrlDir + "/" + gone +
					"/address") {
					t.Fatalf("%s never read the gone controller's address: "+
						"the subsystem no longer lists it", label)
				}
				assertNoCall(t, node, "cmd nvme disconnect")
				connects := 0
				if pass == 0 {
					connects = 1
				}
				got := node.callsMatching("cmd nvme connect")
				if len(got) != connects {
					t.Fatalf("%s made %d connects, want %d: %q",
						label, len(got), connects, got)
				}
				if connects == 1 &&
					(!strings.Contains(got[0], "--traddr "+testIp2+" ") ||
						!strings.Contains(got[0], "--nqn "+nqn+" ")) {
					t.Fatalf("%s connected %q, want the data leg's dst side",
						label, got[0])
				}
				assertNoCall(t, node, "cmd mdadm")
				tc.settled(t,
					reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg], label)
				if retrying(t, srv) {
					t.Fatalf("%s registered the retry for a gone controller",
						label)
				}
			}
		})
	}
}

// subsysDirOf is the sysfs directory of the subsystem the fake holds for
// nqn.
func subsysDirOf(t *testing.T, node *fakeNode, nqn string) string {
	t.Helper()
	node.mu.Lock()
	defer node.mu.Unlock()
	subsys := node.subsystems[nqn]
	if subsys == nil {
		t.Fatalf("the node holds no subsystem %s", nqn)
	}
	return fmt.Sprintf("%s/nvme-subsys%d", sysfsNvmeSubsysDir, subsys.idx)
}

// subsysnqnPath is the sysfs subsysnqn attribute of the subsystem the fake
// holds for nqn.
func subsysnqnPath(t *testing.T, node *fakeNode, nqn string) string {
	t.Helper()
	return subsysDirOf(t, node, nqn) + "/subsysnqn"
}

// TestUnansweredSubsystemWalkKeepsTheConnections pins CN10's sysfs walk
// from its top. A pass walks the node once — it lists
// /sys/class/nvme-subsystem and reads each entry's subsysnqn — and looks
// every leg and clone source up in that walk, and a listing that did not
// answer, or a subsysnqn read that failed, is unknown for the pass, as an
// unanswered address read is: the leg's (or the source's) converge fails
// naming the read, connects nothing and registers the retry, whose next
// attempt, the walk answering, finds the controllers where they were. A
// killed listing fails every lookup of the pass. Read as "not connected" —
// the listing's failure as no subsystem at all, the read's as some other
// subsystem's — it would make the connect step ask for every side of the
// leg again beside its live controllers (duplicates the host refuses,
// EALREADY, and so does the fake, spending the pass's connect budget), the
// clone source step do the same, and a standby's probe row say the leg had
// no controller. A match still wins beside a subsysnqn read that failed
// elsewhere: every controller of one NQN sits in the one subsystem the host
// keeps for it, so the leg whose own entry answered converges. A failed
// listing of the matching entry's own directory, which finds its
// controllers and namespace, fails the lookup. The clone's dm-clone row
// reads the dm-clone itself, which serves on whatever the walk says: OK,
// with its status line, in the converge and the check round alike (CN18).
//
// The reconnected case is the walk after a connect of the pass: the data
// leg's controller was lost, the pass connects its side, and the listing
// stops answering while that connect runs, so the walk the lookup after the
// connect makes is what meets the kill.
func TestUnansweredSubsystemWalkKeepsTheConnections(t *testing.T) {
	listing := "list " + sysfsNvmeSubsysDir
	for _, tc := range []struct {
		name string
		opts reqOpts
		// arm makes the walk fail for the rest of the pass and returns
		// what the failed rows must name. It takes the fake's lock itself.
		arm func(t *testing.T, srv *CnAgentServer, node *fakeNode) string
		// reconnect loses the data leg's controller first, and arms the
		// failure while the pass reconnects it.
		reconnect bool
		// failed are the legs the pass must fail; every other leg converges.
		failed []uint64
		// clone checks the clone source step instead of the legs.
		clone bool
		// settled is a leg's row once the walk answers: the standby's
		// transport verdict, or the primary's prober outcome, PENDING
		// because no test waits a CnLegProbeInterval.
		settled func(t *testing.T, info *pb.ResInfo, label string)
	}{
		{name: "standby, the listing killed", opts: reqOpts{revision: 2},
			arm: func(t *testing.T, _ *CnAgentServer, node *fakeNode) string {
				node.mu.Lock()
				defer node.mu.Unlock()
				node.killCmdAlways[listing] = true
				return listing
			},
			failed:  []uint64{testMetaLeg, testDataLeg},
			settled: assertOk},
		{name: "primary, the listing killed",
			opts: reqOpts{revision: 2, primary: true, raid1: true},
			arm: func(t *testing.T, _ *CnAgentServer, node *fakeNode) string {
				node.mu.Lock()
				defer node.mu.Unlock()
				node.killCmdAlways[listing] = true
				return listing
			},
			failed:  []uint64{testMetaLeg, testDataLeg},
			settled: assertPending},
		{name: "standby, its subsysnqn refused", opts: reqOpts{revision: 2},
			arm: func(t *testing.T, srv *CnAgentServer, node *fakeNode) string {
				path := subsysnqnPath(t, node, legNqn(srv, testDataLeg))
				node.mu.Lock()
				defer node.mu.Unlock()
				node.failReadAlways[path] = true
				return path
			},
			failed: []uint64{testDataLeg}, settled: assertOk},
		{name: "primary, another leg's subsysnqn cut off",
			opts: reqOpts{revision: 2, primary: true, raid1: true},
			arm: func(t *testing.T, srv *CnAgentServer, node *fakeNode) string {
				path := subsysnqnPath(t, node, legNqn(srv, testMetaLeg))
				node.mu.Lock()
				defer node.mu.Unlock()
				node.killReadAlways[path] = true
				return path
			},
			failed: []uint64{testMetaLeg}, settled: assertPending},
		{name: "standby reconnected, the listing killed after the connect",
			opts: reqOpts{revision: 2}, reconnect: true,
			arm: func(t *testing.T, srv *CnAgentServer, node *fakeNode) string {
				node.mu.Lock()
				defer node.mu.Unlock()
				// The hook runs under the fake's lock.
				node.duringConnect[connectKey(legNqn(srv, testDataLeg),
					testIp, testSvcId)] = func() {
					node.killCmdAlways[listing] = true
				}
				return listing
			},
			failed: []uint64{testDataLeg}, settled: assertOk},
		{name: "a clone source, its subsysnqn cut off",
			opts: reqOpts{revision: 2, primary: true,
				clones: []*pb.Clone{cloneOf()}},
			arm: func(t *testing.T, _ *CnAgentServer, node *fakeNode) string {
				path := subsysnqnPath(t, node, testSrcNqn)
				node.mu.Lock()
				defer node.mu.Unlock()
				node.killReadAlways[path] = true
				return path
			},
			clone: true},
		{name: "a clone source, its own subsystem's listing killed",
			opts: reqOpts{revision: 2, primary: true,
				clones: []*pb.Clone{cloneOf()}},
			arm: func(t *testing.T, _ *CnAgentServer, node *fakeNode) string {
				dir := subsysDirOf(t, node, testSrcNqn)
				node.mu.Lock()
				defer node.mu.Unlock()
				node.killCmdAlways["list "+dir] = true
				return "list " + dir
			},
			clone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			srv.probeInterval = time.Hour
			syncupBoth(t, srv, tc.opts)
			connects := 0
			if tc.reconnect {
				node.mu.Lock()
				ctrl := node.subsystems[legNqn(srv, testDataLeg)].ctrls[0]
				node.nvmeDisconnect([]string{
					"disconnect", "--device", ctrl.name})
				node.mu.Unlock()
				connects = 1
			}
			want := tc.arm(t, srv, node)

			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(tc.opts))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if code := reply.GetAgentReply().GetCode(); code != 0 &&
				code != common.ReplyCodeLeftover {
				t.Fatalf("rejected: %v", reply.GetAgentReply())
			}
			if got := node.callsMatching("cmd nvme connect"); len(got) !=
				connects {
				t.Fatalf("the pass made %d connects, want %d: a walk "+
					"that did not answer read as not connected: %q",
					len(got), connects, got)
			}
			for _, verb := range []string{
				"cmd nvme disconnect", "cmd mdadm"} {
				assertNoCall(t, node, verb)
			}
			info := reply.GetCntlrInfo()
			if tc.clone {
				assertErrorDetails(t,
					info.GetCloneIdToTarget()[testClone], want,
					"clone target, its walk unanswered")
				// Nor does the dm-clone row say the source is not
				// connected: the pass does not know whether it is, and the
				// dm-clone it reads serves on.
				dmRow := info.GetCloneIdToDmClone()[testClone]
				assertOk(t, dmRow, "clone dm row, its source's walk "+
					"unanswered")
				if _, ok := common.ParseCloneStatus(
					dmRow.GetDetails()); !ok {
					t.Fatalf("clone dm row details %q are not the "+
						"dm-clone's status line", dmRow.GetDetails())
				}
			}
			for _, lp := range []uint64{testMetaLeg, testDataLeg} {
				row := info.GetLegIdToLeg()[lp]
				if slices.Contains(tc.failed, lp) {
					assertErrorDetails(t, row, want, fmt.Sprintf(
						"leg %#x, its walk unanswered", lp))
				} else if row.GetStatus() == pb.ResStatus_RES_STATUS_ERROR {
					t.Fatalf("leg %#x read ERROR %q beside a walk that "+
						"failed on another subsystem", lp, row.GetDetails())
				}
			}
			if !retrying(t, srv) {
				t.Fatalf("a walk that did not answer registered no " +
					"retry: nothing else would re-run the converge")
			}
			// A check round in between reads the same where its row comes
			// from the walk: a standby's legs, a clone's source.
			if !tc.opts.primary || tc.clone {
				_, probed := srv.checkCntlrRound(context.Background(),
					&pb.CheckCntlrRequest{ClusterId: testCluster,
						CnId: testCn, CntlrPointer: cntlrPtr(),
						Revision: tc.opts.revision}, nil)
				if tc.clone {
					assertErrorDetails(t,
						probed.GetCloneIdToTarget()[testClone], want,
						"checked clone target")
					assertOk(t, probed.GetCloneIdToDmClone()[testClone],
						"checked clone dm row")
				}
				for _, lp := range tc.failed {
					assertErrorDetails(t, probed.GetLegIdToLeg()[lp], want,
						fmt.Sprintf("checked leg %#x", lp))
				}
			}

			node.mu.Lock()
			for _, hooks := range []map[string]bool{node.killCmdAlways,
				node.failReadAlways, node.killReadAlways} {
				for key := range hooks {
					delete(hooks, key)
				}
			}
			for key := range node.duringConnect {
				delete(node.duringConnect, key)
			}
			node.mu.Unlock()
			node.Reset()
			retryAttempt(t, srv)
			for _, verb := range []string{
				"cmd nvme connect", "cmd nvme disconnect", "cmd mdadm"} {
				assertNoCall(t, node, verb)
			}
			if retrying(t, srv) {
				t.Fatalf("the retry outlived the unanswered walk")
			}
			probed := probedCntlrInfo(t, srv)
			if tc.clone {
				assertOk(t, probed.GetCloneIdToTarget()[testClone],
					"clone target, its walk answering")
				return
			}
			for _, lp := range tc.failed {
				tc.settled(t, probed.GetLegIdToLeg()[lp], fmt.Sprintf(
					"leg %#x, its walk answering", lp))
			}
		})
	}
}

// TestAbsentSubsystemClassIsNoSubsystem is the other side of the walk's
// rule: an answer that something is absent is an answer.
// /sys/class/nvme-subsystem does not exist on a host that has not loaded
// nvme-core, which holds no fabrics controller either, and a listing that
// answers "no such directory" is "no subsystem at all", not a walk that did
// not answer. Nor is an entry whose subsysnqn read answers ENOENT: that is a
// subsystem gone between the listing and the read, some other NQN's, and it
// is passed over. A first converge on either host connects every side once
// and builds its legs.
func TestAbsentSubsystemClassIsNoSubsystem(t *testing.T) {
	gone := sysfsNvmeSubsysDir + "/nvme-subsys9"
	for _, tc := range []struct {
		name string
		arm  func(node *fakeNode)
		// read is a read the walk must have made, if any.
		read string
	}{
		{name: "no subsystem class at all", arm: func(node *fakeNode) {
			delete(node.dirs, sysfsNvmeSubsysDir)
			delete(node.dirs, sysfsNvmeCtrlDir)
		}},
		// Listed, with no subsysnqn: the fake answers its read ENOENT.
		{name: "an entry gone since the listing", arm: func(node *fakeNode) {
			node.dirs[gone] = true
		}, read: "read " + gone + "/subsysnqn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			node.mu.Lock()
			tc.arm(node)
			node.mu.Unlock()

			reply := syncupBoth(t, srv, reqOpts{revision: 2})
			if tc.read != "" && !node.hasCall(tc.read) {
				t.Fatalf("the walk never made %q", tc.read)
			}
			for _, lp := range []uint64{testMetaLeg, testDataLeg} {
				assertOk(t, reply.GetCntlrInfo().GetLegIdToLeg()[lp],
					fmt.Sprintf("leg %#x", lp))
				if n := callCnt(node, "--nqn "+legNqn(srv, lp)+" "); n != 1 {
					t.Errorf("leg %#x was connected %d times, want 1", lp, n)
				}
			}
			if retrying(t, srv) {
				t.Fatalf("an answer that the subsystem is absent " +
					"registered the retry")
			}
		})
	}
}

// dropSubsysLocked takes nqn's subsystem away with its controllers, which
// frees its entry number, the way the kernel does once nothing holds the
// subsystem. Called with node.mu held, from an afterList or afterRead hook.
func dropSubsysLocked(node *fakeNode, nqn string) {
	subsys := node.subsystems[nqn]
	for _, ctrl := range append([]*fakeCtrl(nil), subsys.ctrls...) {
		node.dropCtrl(subsys, ctrl)
	}
}

// TestAStaleWalkEntryReadsNoSubsystem pins the check a lookup makes before
// it uses its entry (CN10). A pass walks the node once, so the entry a leg's
// NQN was found in can be as old as the pass, and the kernel gives a freed
// number to the next new subsystem: here the data leg's subsystem goes right
// after the walk read its subsysnqn, and another cntlr of the node connects
// one of its own legs, which takes the freed number, with a controller at
// the data leg's side address, a second one at an address no side of the leg
// has, and a namespace head. The lookup reads the entry's subsysnqn again,
// finds the other NQN, and the leg has no subsystem: the check round reads
// its row ERROR "no controller", not OK over the other pool's path, and not
// unknown either, also beside a third entry whose subsysnqn read fails,
// since the walk found this NQN in this entry and the host keeps one
// subsystem per NQN ([D1]). An entry gone at the check, with nothing at its
// number, is no subsystem the same way. A converge connects the leg's side
// afresh and builds its wrapper over the new head, never over the other
// pool's namespace, and touches no controller of the other subsystem: read
// as the leg's, the other subsystem's second controller would be a dead path
// of the leg, which the converge disconnects. The converge's leg has no
// wrapper beforehand: nothing else holds a subsystem whose number the kernel
// frees.
func TestAStaleWalkEntryReadsNoSubsystem(t *testing.T) {
	const otherSp = testSp + 1
	for _, tc := range []struct {
		name string
		// gone drops the leg's subsystem and plants nothing at its number.
		gone bool
		// unread plants a third entry whose subsysnqn read fails.
		unread bool
		// converge runs a converge after dropping the leg's wrapper, and a
		// check round otherwise.
		converge bool
	}{
		{name: "check round"},
		{name: "check round, beside an entry that did not answer",
			unread: true},
		{name: "check round, the entry gone", gone: true},
		{name: "converge", converge: true},
		{name: "converge, the entry gone", gone: true, converge: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			opts := reqOpts{revision: 2}
			syncupBoth(t, srv, opts)
			nqn := legNqn(srv, testDataLeg)
			other := names(srv).SideToCnNqn(testCluster, otherSp, 0x99, testCn)
			dir := subsysDirOf(t, node, nqn)
			if tc.converge {
				dropWrapper(node, legName(srv, testDataLeg))
			}
			node.mu.Lock()
			idx := node.subsystems[nqn].idx
			unread := ""
			if tc.unread {
				third := names(srv).SideToCnNqn(
					testCluster, otherSp, 0x98, testCn)
				node.plantSubsys(node.nextSubsys, third,
					endpoint{testIp2, testSvcId2})
				unread = fmt.Sprintf("%s/nvme-subsys%d/subsysnqn",
					sysfsNvmeSubsysDir, node.subsystems[third].idx)
				node.failReadAlways[unread] = true
			}
			node.afterRead[dir+"/subsysnqn"] = func() {
				dropSubsysLocked(node, nqn)
				if !tc.gone {
					node.plantSubsys(idx, other, endpoint{testIp, testSvcId},
						endpoint{testIp2, testSvcId2})
				}
			}
			node.mu.Unlock()

			node.Reset()
			if !tc.converge {
				row := checkRound(t, srv, opts.revision).
					GetLegIdToLeg()[testDataLeg]
				assertErrorDetails(t, row, "no controller for "+nqn,
					"leg data, its entry gone or taken by another subsystem")
				if strings.Contains(row.GetDetails(),
					errSubsysUnknown.Error()) {
					t.Fatalf("leg data reads unknown: %q", row.GetDetails())
				}
				// The walk's read and the lookup's: the verdict lists the
				// connections from the same walk and reads none of its own.
				if n := countCalls(node, "read "+dir+"/subsysnqn"); n != 2 {
					t.Fatalf("the round read %s/subsysnqn %d times, want "+
						"2: the walk's read and the lookup's", dir, n)
				}
				if unread != "" {
					if n := countCalls(node, "read "+unread); n != 1 {
						t.Fatalf("the round read %s %d times, want 1: the "+
							"walk's", unread, n)
					}
				}
				return
			}
			reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(opts))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if code := reply.GetAgentReply().GetCode(); code != 0 {
				t.Fatalf("rejected: %v", reply.GetAgentReply())
			}
			assertOk(t, reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg],
				"leg data, connected afresh")
			if n := callCnt(node, "cmd nvme connect "); n != 1 ||
				callCnt(node, "--nqn "+nqn+" ") != 1 {
				t.Fatalf("the converge made %d connects, want the data "+
					"leg's one: %q", n, node.callsMatching("cmd nvme connect"))
			}
			disconnects := node.callsMatching("cmd nvme disconnect")
			node.mu.Lock()
			defer node.mu.Unlock()
			mine := node.subsystems[nqn]
			if mine == nil {
				t.Fatalf("the data leg has no subsystem after the converge")
			}
			newNo := node.devNo[fmt.Sprintf("/dev/nvme%dn1", mine.idx)]
			wrapper := node.dms[legName(srv, testDataLeg)]
			if newNo == "" || wrapper == nil ||
				!strings.Contains(wrapper.table, " "+newNo+" ") {
				t.Fatalf("the data leg's wrapper is %+v, want it over its "+
					"new head %s", wrapper, newNo)
			}
			if tc.gone {
				if len(disconnects) != 0 {
					t.Fatalf("the converge disconnected %q", disconnects)
				}
				return
			}
			held := node.subsystems[other]
			if held == nil || held.idx != idx || len(held.ctrls) != 2 {
				t.Fatalf("the other subsystem is %+v, want its two "+
					"controllers at nvme-subsys%d", held, idx)
			}
			for _, call := range disconnects {
				args := strings.Fields(call)
				if strings.Contains(call, other) ||
					slices.Contains(args, held.ctrls[0].name) ||
					slices.Contains(args, held.ctrls[1].name) {
					t.Fatalf("the converge touched the other subsystem: %q",
						call)
				}
			}
			if mine.idx == idx {
				t.Fatalf("the data leg's subsystem is %+v, want a new entry",
					mine)
			}
			otherNo := node.devNo[fmt.Sprintf("/dev/nvme%dn1", idx)]
			if otherNo == "" ||
				strings.Contains(wrapper.table, " "+otherNo+" ") {
				t.Fatalf("the data leg's wrapper is %+v, want it not over "+
					"the other pool's head %s", wrapper, otherNo)
			}
		})
	}
}

// TestAnEntryWhoseCheckDidNotAnswerIsUnknown pins the other half of the
// check a lookup makes before it uses its entry (CN10): the walk's read of
// the data leg's subsysnqn answers, and the lookup's second read of it does
// not, cut off at the soft timeout or failed with an I/O error. The pass does
// not know which NQN the entry names at the check, so the lookup did not
// answer: the check round reads the leg's row ERROR, unknown, naming the
// read, and the converge fails the leg the same way, connects nothing and
// registers the retry. Read as no subsystem, the round would say the leg has
// no controller, and the converge would connect the leg's side again beside
// its live controller, a duplicate the host refuses.
func TestAnEntryWhoseCheckDidNotAnswerIsUnknown(t *testing.T) {
	for _, converge := range []bool{false, true} {
		for _, cutOff := range []bool{true, false} {
			name := "check round"
			if converge {
				name = "converge"
			}
			if cutOff {
				name += ", cut off"
			} else {
				name += ", an I/O error"
			}
			t.Run(name, func(t *testing.T) {
				srv, node := newTestServer(t)
				srv.retryInterval = time.Hour
				withPassClock(srv, node)
				opts := reqOpts{revision: 2}
				syncupBoth(t, srv, opts)
				nqn := legNqn(srv, testDataLeg)
				path := subsysnqnPath(t, node, nqn)
				node.mu.Lock()
				// The walk's read answers and arms a one-shot fault of the
				// next read of the attribute: the lookup's check.
				node.afterRead[path] = func() {
					if cutOff {
						node.killRead[path] = true
					} else {
						node.failRead[path] = true
					}
				}
				node.mu.Unlock()

				node.Reset()
				var row *pb.ResInfo
				if converge {
					reply, err := srv.SyncupCntlr(context.Background(),
						cntlrReq(opts))
					if err != nil {
						t.Fatalf("SyncupCntlr: %v", err)
					}
					row = reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg]
				} else {
					row = checkRound(t, srv, opts.revision).
						GetLegIdToLeg()[testDataLeg]
				}
				assertErrorDetails(t, row, "read "+path,
					"leg data, its check did not answer")
				if !strings.Contains(row.GetDetails(),
					errSubsysUnknown.Error()) {
					t.Fatalf("leg data reads %q, want unknown",
						row.GetDetails())
				}
				node.mu.Lock()
				armed := node.killRead[path] || node.failRead[path]
				node.mu.Unlock()
				if armed {
					t.Fatalf("the fault of %s never fired", path)
				}
				// The walk's read and the check: the leg failed on its
				// own entry's check, not on the walk. A check round's
				// verdict lists the connections from the same walk.
				if n := countCalls(node, "read "+path); n != 2 {
					t.Fatalf("the %s read %s %d times, want 2", name, path, n)
				}
				if !converge {
					return
				}
				assertNoCall(t, node, "cmd nvme connect")
				if !retrying(t, srv) {
					t.Fatalf("an unknown leg registered no retry")
				}
			})
		}
	}
}

// TestAnEntryGoneBeforeItsListingIsUnknown pins the lookup's last step
// (CN10): the entry the walk found passes the subsysnqn check, and then its
// directory is gone before the lookup lists it. Its controllers and namespace
// are unread, so the pass does not know whether the NQN is connected, as for
// any failed listing of the matching entry's own directory: the leg's row
// reads unknown, naming the listing, and the converge connects nothing and
// registers the retry, whose attempt walks afresh, finds no entry and
// connects the leg.
func TestAnEntryGoneBeforeItsListingIsUnknown(t *testing.T) {
	for _, converge := range []bool{false, true} {
		name := "check round"
		if converge {
			name = "converge"
		}
		t.Run(name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			opts := reqOpts{revision: 2}
			syncupBoth(t, srv, opts)
			nqn := legNqn(srv, testDataLeg)
			dir := subsysDirOf(t, node, nqn)
			path := dir + "/subsysnqn"
			node.mu.Lock()
			// The walk's read arms the hook of the next one, the lookup's
			// check, after which the subsystem goes.
			node.afterRead[path] = func() {
				node.afterRead[path] = func() { dropSubsysLocked(node, nqn) }
			}
			node.mu.Unlock()

			node.Reset()
			var row *pb.ResInfo
			if converge {
				reply, err := srv.SyncupCntlr(context.Background(),
					cntlrReq(opts))
				if err != nil {
					t.Fatalf("SyncupCntlr: %v", err)
				}
				row = reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg]
			} else {
				row = checkRound(t, srv, opts.revision).
					GetLegIdToLeg()[testDataLeg]
			}
			assertErrorDetails(t, row, "list "+dir,
				"leg data, its entry gone before its listing")
			if !strings.Contains(row.GetDetails(), errSubsysUnknown.Error()) {
				t.Fatalf("leg data reads %q, want unknown", row.GetDetails())
			}
			if n := countCalls(node, "read "+path); n != 2 {
				t.Fatalf("%s read %s %d times before its listing, want 2",
					name, path, n)
			}
			if !converge {
				return
			}
			assertNoCall(t, node, "cmd nvme connect")
			if !retrying(t, srv) {
				t.Fatalf("an unknown leg registered no retry")
			}
			node.Reset()
			retryAttempt(t, srv)
			if got := connectsTo(node, nqn, testIp, testSvcId); got != 1 {
				t.Fatalf("the retry made %d connects of the data leg, "+
					"want 1", got)
			}
			assertOk(t, probedCntlrInfo(t, srv).GetLegIdToLeg()[testDataLeg],
				"leg data, connected by the retry")
		})
	}
}

// ---------------------------------------------------------------------------
// Namespace states (CN16)
// ---------------------------------------------------------------------------

// TestNamespaceSuspend is architecture.md, Namespace suspend semantics: an
// effectively suspended namespace is **parked**, never dm-suspended. Both
// directions pin
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

// TestParkIsIdempotent is SH16 for the park. The park's
// idempotence rests on `parkNsDev`'s own `parked` predicate, which CN9's
// pre-step 2 (the park of a planned ns-dev) reaches first for an effectively
// suspended namespace — so a drift there would reload a live, correct ns-dev
// on every converge round the worker drives. (`ensureNsDev`'s
// `nsDevTableMatches` is the second gate and is already satisfied by the time
// it runs here.)
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
// state, for an effectively suspended namespace too.
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

// TestLeftoverSuspendedNsDevIsResumed is the failure path (CN16): three
// shapes in which a pass can meet a suspended ns-dev, and how each converges
// on the first pass. A reload that was interrupted or failed (Dm.Reload fails
// closed) leaves an ns-dev suspended.
func TestLeftoverSuspendedNsDevIsResumed(t *testing.T) {
	// leftover re-creates what a reload that was interrupted or failed left
	// behind, and returns the count of ns-dev reloads and resumes the next
	// converge issues.
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

	// (a) Effectively suspended, still on the raid0 a failed park left it
	// holding: one reload, onto the error backing, and the device ends live.
	t.Run("effectively suspended, raid0 table", func(t *testing.T) {
		srv, node, reloads, _ := leftover(t, true, raid0Table)
		if reloads != 1 {
			t.Fatalf("want exactly one ns-dev reload, got %d", reloads)
		}
		assertParked(t, srv, node, testNs, testTd, "failed park, raid0 table")
	})

	// (b) Effectively suspended and already on the error table, but held
	// suspended: the park's own reload resumes it. A device left in this state
	// is what an interrupted `Reload` produces.
	t.Run("effectively suspended, error table", func(t *testing.T) {
		srv, node, reloads, _ := leftover(t, true, errorTable)
		if reloads != 1 {
			t.Fatalf("want exactly one ns-dev reload, got %d", reloads)
		}
		assertParked(t, srv, node, testNs, testTd,
			"interrupted reload, error table")
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
	// found suspended with its correct table. The park ([D12]) makes that
	// resume unconditional, and nothing else in
	// the package converges a suspended raid0, pool or thin volume — so
	// without this sub-case the guard could be dropped as dead and the next
	// converge after a killed agent would leave the stack wedged under a
	// live ns-dev.
	t.Run("a suspended raid0 is resumed", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
		raid0 := raid0Name(srv, testTd)
		node.dms[raid0].suspended = true
		// The park's asymmetry, pinned on the fixture that already exists:
		// an ns-dev found suspended is an ERROR whatever the plan says, while
		// every OTHER dm device still probes OK with `details = "suspended"`.
		// Deliberate — only the ns-dev has a plan state a suspend could be
		// mistaken for.
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
	// is where the xfer device is created. What architecture.md, Transfer +
	// clone = cross-SP live migration, makes
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
	// [D2] applied to the xfer (CN17): every cntlr presents the one model, so
	// a destination clone connecting through several of them sees one device.
	if got := node.files[agent.NvmetRoot+"/subsystems/"+xferNqn+
		"/attr_model"]; got != common.NvmeModel {
		t.Fatalf("xfer attr_model is %q, want %q", got, common.NvmeModel)
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
// cntlid slots (CN16/CN17; architecture.md, cntlid slots)
// ---------------------------------------------------------------------------

// cntlidSpan is the `[attr_cntlid_min, attr_cntlid_max]` one subsystem holds
// in configfs; nvmet hands out CNTLIDs from it with both ends included.
type cntlidSpan struct{ first, last uint64 }

// slotSpan is the range of architecture.md, cntlid slots, for one slot:
// `Base + s×Step` to
// `Base + s×Step + Step − 1`.
func slotSpan(slot int) cntlidSpan {
	first := uint64(common.CnCntlidSlotBase + slot*common.CnCntlidSlotStep)
	return cntlidSpan{first, first + common.CnCntlidSlotStep - 1}
}

// readCntlidSpan reads back the range a pass left in one subsystem.
func readCntlidSpan(t *testing.T, node *fakeNode, nqn string) cntlidSpan {
	t.Helper()
	var span cntlidSpan
	node.mu.Lock()
	defer node.mu.Unlock()
	for _, attr := range []struct {
		name string
		into *uint64
	}{
		{"attr_cntlid_min", &span.first},
		{"attr_cntlid_max", &span.last},
	} {
		path := agent.NvmetRoot + "/subsystems/" + nqn + "/" + attr.name
		if _, err := fmt.Sscan(node.files[path], attr.into); err != nil {
			t.Fatalf("%s is %q", path, node.files[path])
		}
	}
	return span
}

// TestCntlidSlotsAreDisjoint converges a primary on every cntlid slot, each
// on a fresh node, and reads back the range the pass wrote into configfs for
// the host-facing subsystem and for the transfer's, which carry the cntlr's
// one slot. nvmet hands out CNTLIDs from `[attr_cntlid_min,
// attr_cntlid_max]`, both ends included, and a host merges the cntlrs of one
// SP into one multipath device, so no two slots may share an id: slot s is
// `Base + s×Step` to `Base + s×Step + Step − 1`. A range that ended at
// `min + Step` handed its last id to the next slot as well.
func TestCntlidSlotsAreDisjoint(t *testing.T) {
	xfers := []*pb.Transfer{{
		XferId:       testXfer,
		OriNqn:       testNqn,
		OriNsIdx:     1,
		AllowedHosts: []string{testHostNqn},
	}}
	spans := make([]cntlidSpan, common.CnCntlidSlotCnt)
	for slot := range spans {
		srv, node := newTestServer(t)
		cnSyncup(t, srv, 2, true)
		req := cntlrReq(reqOpts{revision: 2, primary: true, xfers: xfers})
		req.Cntlr.CntlidSlot = uint32(slot)
		reply, err := srv.SyncupCntlr(context.Background(), req)
		if err != nil {
			t.Fatalf("slot %d: SyncupCntlr: %v", slot, err)
		}
		info := reply.GetCntlrInfo()
		assertOk(t, info.GetSsIdToSubsystem()[testSs], "subsystem")
		assertOk(t, info.GetXferIdToSubsystem()[testXfer], "xfer subsystem")
		spans[slot] = readCntlidSpan(t, node, testNqn)
		xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
		if got := readCntlidSpan(t, node, xferNqn); got != spans[slot] {
			t.Errorf("slot %d: the transfer's cntlids are %d-%d, the "+
				"subsystem's %d-%d", slot, got.first, got.last,
				spans[slot].first, spans[slot].last)
		}
	}
	for a := range spans {
		for b := a + 1; b < len(spans); b++ {
			if spans[a].last >= spans[b].first &&
				spans[b].last >= spans[a].first {
				t.Errorf("slot %d's cntlids %d-%d and slot %d's %d-%d "+
					"overlap", a, spans[a].first, spans[a].last,
					b, spans[b].first, spans[b].last)
			}
		}
	}
	// The slot table: slot 0 = 10000-14999 … slot 7 = 45000-49999.
	for slot, span := range spans {
		if want := slotSpan(slot); span != want {
			t.Errorf("slot %d's cntlids are %d-%d, want %d-%d",
				slot, span.first, span.last, want.first, want.last)
		}
	}
}

// TestCntlidRangeMovesBetweenSlots walks one live primary from slot to slot
// on ONE node and reads the range back after every pass, from the
// host-facing subsystem and from the transfer's. A live subsystem changes
// slot when a cntlr adopts one that an earlier cntlr on the same CN left
// behind: rotating a cntlr to a free slot (architecture.md, Transfer + clone
// = cross-SP live migration) deletes it and creates
// another, both requests name the same NQNs (the host-facing one is the
// user's own string), and the new cntlr's request keeps the leftover from
// the sweep, so its pass converges that subsystem under the new slot. Like
// nvmet, the fake refuses an attr_cntlid_min above the current
// attr_cntlid_max and an attr_cntlid_max below the current attr_cntlid_min,
// so once no two slots share an id a range that lies wholly above the live
// one must be written max first: min first, even a move up by one slot fails
// on every pass. The walk moves up by one, up by more, down by one and down
// by more, then repeats its last slot, and counts each bound's writes, so the
// max written first is not written a second time and an unchanged range
// writes neither.
func TestCntlidRangeMovesBetweenSlots(t *testing.T) {
	ctx := context.Background()
	xfers := []*pb.Transfer{{
		XferId:       testXfer,
		OriNqn:       testNqn,
		OriNsIdx:     1,
		AllowedHosts: []string{testHostNqn},
	}}
	srv, node := newTestServer(t)
	cnSyncup(t, srv, 2, true)
	nqns := []string{testNqn, srv.nf.XferNqn(testCluster, testSp, testXfer)}
	prev := -1
	for step, slot := range []int{0, 1, 3, 7, 6, 4, 0, 0} {
		label := fmt.Sprintf("slot %d -> %d", prev, slot)
		if prev < 0 {
			label = fmt.Sprintf("fresh node, slot %d", slot)
		}
		node.Reset()
		req := cntlrReq(reqOpts{
			revision: uint64(2 + step), primary: true, xfers: xfers})
		req.Cntlr.CntlidSlot = uint32(slot)
		reply, err := srv.SyncupCntlr(ctx, req)
		if err != nil {
			t.Fatalf("%s: SyncupCntlr: %v", label, err)
		}
		info := reply.GetCntlrInfo()
		assertOk(t, info.GetSsIdToSubsystem()[testSs], label+": subsystem")
		assertOk(t, info.GetXferIdToSubsystem()[testXfer],
			label+": xfer subsystem")
		for _, nqn := range nqns {
			if got, want := readCntlidSpan(t, node, nqn),
				slotSpan(slot); got != want {
				t.Errorf("%s: %s holds cntlids %d-%d, want %d-%d", label,
					nqn, got.first, got.last, want.first, want.last)
			}
			subsysPath := agent.NvmetRoot + "/subsystems/" + nqn
			minWrite := "writedirect " + subsysPath + "/attr_cntlid_min="
			maxWrite := "writedirect " + subsysPath + "/attr_cntlid_max="
			wantWrites := 1
			if slot == prev {
				wantWrites = 0
			}
			minCnt := len(node.callsMatching(minWrite))
			maxCnt := len(node.callsMatching(maxWrite))
			if minCnt != wantWrites || maxCnt != wantWrites {
				t.Errorf("%s: %s: %d attr_cntlid_min and %d attr_cntlid_max "+
					"writes, want %d of each\ncalls:\n%s", label, nqn,
					minCnt, maxCnt, wantWrites,
					strings.Join(node.Calls(), "\n"))
				continue
			}
			if wantWrites == 0 {
				continue
			}
			maxFirst := node.indexOfCall(maxWrite) < node.indexOfCall(minWrite)
			if wantMaxFirst := prev >= 0 && slot > prev; maxFirst !=
				wantMaxFirst {
				t.Errorf("%s: %s: attr_cntlid_max written first: %v, "+
					"want %v", label, nqn, maxFirst, wantMaxFirst)
			}
		}
		prev = slot
	}
}

// ---------------------------------------------------------------------------
// Park before remove (CN9/CN21)
// ---------------------------------------------------------------------------

// TestRemovedSuspendedNamespaceIsParkedBeforeNvmetRemoval pins CN21's order
// (the P0 park before L1's nvmet removal) for a namespace that *leaves* the
// desired state. CN21's rationale is that a dm-suspended device blocks the
// nvmet disable above it, and CN16's that it blocks its own removal. No pass
// of this agent leaves an ns-dev suspended unless a `dmsetup` command on it
// fails (architecture.md, Namespace suspend semantics; [D12]), yet a teardown
// can still meet one — the leftover of such a failure or of an interrupted
// reload (CN16) — and that leftover is
// exactly what the two ordering sub-cases fixture. The park — the reload onto
// the td's `CnErrorName`, whose internal resume is the whole point —
// therefore has to precede the nvmet removal, not follow it inside
// `removeDm`. The third sub-case is the steady state: an already parked
// namespace needs no reload at all.
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
	// olderBuildLeftover puts the ns-dev into the state a park whose load
	// failed leaves (Dm.Reload fails closed):
	// held `dmsetup suspend`ed, its table still the rule-6 raid0. That state
	// is why the park has to come first: the nvmet disable above a suspended
	// device does not complete.
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
		// One park, not one per layer: an ordering assertion stops at its
		// first match and cannot see a second one. The count is over
		// `parkByTable`'s own `dmsetup table` read (CN21's P0), which every
		// call that finds the device makes before it decides anything —
		// counting reloads would prove nothing, because `parkByTable` leaves
		// alone a device already linear over a kind-c5 dm-error (it only
		// resumes one that is suspended), so a second park issues no reload.
		// `dmsetup info` is no counter either: `removeDm` probes the same
		// device below. It says nothing about any other ns-dev — this
		// fixture removes exactly one.
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

	// The steady state: converge A already parked the ns-dev, so the chain's
	// P0 (`parkByTable`) finds it live and linear over a kind-c5 dm-error, and
	// changes nothing (no suspend, reload or resume). The nvmet disable and
	// the removal then work on a device nobody ever suspended — which is the
	// whole point of the park, and what the two sub-cases above cannot show.
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
// The sp_level ladder (CN19)
// ---------------------------------------------------------------------------

func TestSpLevelLadder(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	revision := uint64(2)
	send := func(level pb.SpLevel) *pb.SyncupCntlrReply {
		t.Helper()
		revision++
		node.Reset()
		reply, err := srv.SyncupCntlr(context.Background(),
			cntlrReq(reqOpts{
				revision: revision, primary: true, level: level}))
		if err != nil {
			t.Fatalf("level %v: %v", level, err)
		}
		return reply
	}
	resync := func(level pb.SpLevel) *pb.SyncupCntlrReply {
		t.Helper()
		reply := send(level)
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

	// CN10: the legs' disconnects run off the pass's locks, so the pass that
	// sets them going names the connections as leftovers.
	cnSweepOnlyDisconnects(t,
		send(pb.SpLevel_SP_LEVEL_NO_SIDE).GetAgentReply(), "NO_SIDE")
	awaitDisconnects(t, srv)
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
	// CN10: the legs' disconnects run off the pass's locks, so the pass that
	// sets them going names the two connections, and nothing else, as
	// leftovers.
	cnSweepOnlyDisconnects(t, reply.GetAgentReply(), "the teardown")
	for _, legId := range []uint64{testMetaLeg, testDataLeg} {
		cnSweepAssertDetails(t, reply.GetAgentReply(), legNqn(srv, legId),
			"the teardown")
	}
	// The top-down chain.
	assertOrder(t, node,
		// The cntlr is FORGOTTEN first — file, chunks, memory entry —
		// and its resources are then found by name by the node-level sweep.
		// Removing the resources first and deleting the file regardless would
		// forget a cntlr whose array would not stop, with its devices still
		// live.
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
	// CN21: each leg is disconnected exactly once, and the worker's re-sync
	// once the disconnects have returned finds nothing left.
	awaitDisconnects(t, srv)
	for _, legId := range []uint64{testMetaLeg, testDataLeg} {
		if n := len(node.callsMatching(
			"cmd nvme disconnect --nqn " + legNqn(srv, legId))); n != 1 {
			t.Fatalf("leg %#x was disconnected %d times", legId, n)
		}
	}
	reply, err = srv.SyncupCn(context.Background(), cnReq(3, false))
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("the re-sync: %v", reply.GetAgentReply())
	}
	// CN14: a teardown deactivates, it never deletes thin device ids.
	assertNoCall(t, node, "0 delete ")
	for name := range node.dms {
		t.Fatalf("dm device %s survived the teardown", name)
	}
	// The base state outlives every cntlr: the tmpfs mount and its single
	// loop device stay, and the loop above already showed that every dm
	// device — the kind-`cb` wrappers included — is gone.
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
// Check streams (CN24)
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
		return nil, io.EOF
	}
	req := s.requests[0]
	s.requests = s.requests[1:]
	return req, nil
}

func (s *fakeCheckCnStream) Send(reply *pb.CheckCnReply) error {
	s.replies = append(s.replies, reply)
	return nil
}

type fakeCheckCntlrStream struct {
	pb.ControllerNodeAgent_CheckCntlrServer
	ctx      context.Context
	requests []*pb.CheckCntlrRequest
	replies  []*pb.CheckCntlrReply
}

func (s *fakeCheckCntlrStream) Context() context.Context { return s.ctx }

func (s *fakeCheckCntlrStream) Recv() (*pb.CheckCntlrRequest, error) {
	if len(s.requests) == 0 {
		return nil, io.EOF
	}
	req := s.requests[0]
	s.requests = s.requests[1:]
	return req, nil
}

func (s *fakeCheckCntlrStream) Send(reply *pb.CheckCntlrReply) error {
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

// TestCheckRoundsCarryTheRequestTraceId pins CN24's per-round trace id: the
// stream's context names only the worker round that opened it, so every
// command and read of a round runs under the request's trace_id, and a round
// that names none keeps the stream's.
func TestCheckRoundsCarryTheRequestTraceId(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	probeAllLegs(t, srv)

	// The stream contexts are what the server interceptor hands the handler:
	// the trace id of the stream's metadata (grpc.md T2).
	check := func(streamId string, roundIds ...string) {
		t.Helper()
		cn := &fakeCheckCnStream{
			ctx: common.WithTraceId(context.Background(), "cn-"+streamId),
		}
		cntlr := &fakeCheckCntlrStream{
			ctx: common.WithTraceId(context.Background(), "cntlr-"+streamId),
		}
		for _, id := range roundIds {
			cn.requests = append(cn.requests, &pb.CheckCnRequest{
				ClusterId: testCluster, CnId: testCn, Revision: 2,
				TraceId: id,
			})
			cntlr.requests = append(cntlr.requests, &pb.CheckCntlrRequest{
				ClusterId: testCluster, CnId: testCn,
				CntlrPointer: cntlrPtr(), Revision: 2, TraceId: id,
			})
		}
		if err := srv.CheckCn(cn); err != nil {
			t.Fatalf("CheckCn: %v", err)
		}
		if err := srv.CheckCntlr(cntlr); err != nil {
			t.Fatalf("CheckCntlr: %v", err)
		}
		if len(cn.replies) != len(roundIds) ||
			len(cntlr.replies) != len(roundIds) {
			t.Fatalf("%d cn and %d cntlr replies to %d rounds",
				len(cn.replies), len(cntlr.replies), len(roundIds))
		}
	}

	check("stream", "round-1", "round-2")
	for _, id := range []string{"round-1", "round-2"} {
		if len(node.Traced(id)) == 0 {
			t.Errorf("no command or read ran under %s", id)
		}
	}
	for _, id := range []string{"cn-stream", "cntlr-stream"} {
		if got := node.Traced(id); len(got) != 0 {
			t.Errorf("a round naming its own id ran %d calls under the "+
				"stream's %s, the first %q", len(got), id, got[0])
		}
	}
	// An empty trace_id keeps the stream's id.
	check("stream", "")
	for _, id := range []string{"cn-stream", "cntlr-stream"} {
		if len(node.Traced(id)) == 0 {
			t.Errorf("a round naming no id ran nothing under the stream's %s",
				id)
		}
	}
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

// TestGetCntlrInfoReadsTheNodeFirst pins the order of a known cntlr's
// GetCntlrInfo (CN23): the reading first, then the rows and the read-only
// verdict from it, as a check round takes them (CN24), so a read answers what
// a round in the same state would. The reading's two whole-node dm listings,
// its NVMe walk and its md walk each run once, in that order — the info
// listing before the table listing, so a name the table listing lacks went
// away between the two — and before the pool's status, the first read of a
// row; the verdict lists no dm device, array or connection of its own, no
// `dmsetup ls` and no second reading. A check round is the control.
func TestGetCntlrInfoReadsTheNodeFirst(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	pool := "cmd dmsetup status --noflush " + poolName(srv)
	for _, entrance := range []struct {
		name string
		read func()
	}{
		{"GetCntlrInfo", func() { probedCntlrInfo(t, srv) }},
		{"a check round", func() { checkRound(t, srv, 2) }},
	} {
		t.Run(entrance.name, func(t *testing.T) {
			node.Reset()
			entrance.read()
			reading := []string{
				dmInfoListLine, dmTableListLine,
				"list " + sysfsNvmeSubsysDir, "list " + sysfsBlockDir,
			}
			for _, listing := range reading {
				if got := countCalls(node, listing); got != 1 {
					t.Errorf("%q ran %d times, want 1", listing, got)
				}
			}
			assertOrder(t, node, append(reading, pool)...)
			assertNoCall(t, node, "cmd dmsetup ls")
		})
	}
}

// ---------------------------------------------------------------------------
// Provisioning deferral ([D15])
//
// A side that has not finished its zeroing (architecture.md, Side provisioning
// protocol) exports nothing, so every
// resource stacked on it is left out of the *effective* desired state: it is
// not built, and it reports RES_STATUS_PROVISIONING rather than an error, so a
// freshly created SP never feeds err_epoch or the replacement flows of
// architecture.md, Automatic reactions.
// ---------------------------------------------------------------------------

// grownDataGrp appends a second data group to the fixture's only slice — the
// GrowSlice shape (architecture.md, GrowSlice). provisioned says whether the
// DN has already finished
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

// TestServingPoolStaysOkDuringDeferredGrow is the thin-pool auto-grow guard of
// dnv-worker.md AR6: a GrowSlice
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
// grow completes when the group clears") and architecture.md, GrowSlice ("the
// concat and the pool keep their old, effective size") both describe a prefix.
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
// gains an all-unprovisioned leg (the spare switch of architecture.md, Spare
// legs, onto a fresh DN) must
// not shorten the pool-data concat under a live thin-pool: the concat's target
// list is the physical home of every block the pool has already allocated, so
// a shrink either remaps live data or leaves the pool suspended on a refused
// resume. The concat may only grow; anything else is reported, so an automatic
// reaction (architecture.md, Automatic reactions) or an operator repairs the
// group.
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

// TestProvisioningCheckRoundIsStable is CN9's check-stream clause: a
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

// TestSpLevelBeatsProvisioning is CN9's precedence clause: a resource
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

// TestUnprovisionedSpareDefersOnlyItself: spares never assemble
// (architecture.md, Spare legs), so an
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
