package worker

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The fixture SP of §11 (§13: "the pending rule … with real §3.6 geometry")
// ---------------------------------------------------------------------------

const (
	// The geometry every reaction fixture is built with: 64 MiB extents,
	// 1 MiB pool blocks, 128-block bitmap chunks, md-raid1. model.GroupBlocks
	// turns them into the meta_blocks / data_blocks the AR6 pending rule does
	// its arithmetic on, so the numbers below are never hand-written.
	reactExtentSize  = uint64(64) << 20
	reactBlockSize   = uint64(1) << 20
	reactChunkBlocks = uint64(128)

	reactSliceId = uint64(10)
	reactMetaGrp = uint64(20)
	reactDataGrp = uint64(21)

	reactCntlrA = uint64(1)
	reactCntlrB = uint64(2)

	reactMetaLegA = uint64(100)
	reactMetaLegB = uint64(101)
	reactDataLegA = uint64(110)
	reactDataLegB = uint64(111)

	reactSideMetaA = uint64(200)
	reactSideMetaB = uint64(201)
	reactSideDataA = uint64(210)
	reactSideDataB = uint64(211)

	reactDnA = "rdn0:9520"
	reactDnB = "rdn1:9520"
	reactDnC = "rdn2:9520"
	reactDnD = "rdn3:9520"

	reactCnA = "rcn0:9620"
	reactCnB = "rcn1:9620"
	reactCnC = "rcn2:9620"

	// reactSpRevision is what the seeded SpRev reports, i.e. the "revision"
	// attribute of every `reaction applied` record.
	reactSpRevision = uint64(77)
)

// reactBdevConf is the fixture's SP-wide geometry.
func reactBdevConf() *pb.BdevConf {
	return &pb.BdevConf{
		DmPoolConf: &pb.DmPoolConf{
			DataBlockSize:   reactBlockSize,
			LowWaterMarkPct: 50,
		},
		RedundConf: &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{
					BitmapChunkBlockCnt: reactChunkBlocks,
				},
			},
		},
	}
}

// reactClusterConf is the cluster the fixture SP lives in: 64 MiB extents and
// the default batch sizes.
func reactClusterConf() *pb.ClusterConf {
	return &pb.ClusterConf{
		CreationEpoch: 1,
		DnBinConf:     &pb.DnBinConf{ExtentSize: reactExtentSize},
		BdevConf:      reactBdevConf(),
	}
}

// reactGroup builds one group with the real §3.6 block counts of extCnt
// extents and one single-sided leg per (leg_id, side_id, addr) triple.
func reactGroup(
	t *testing.T,
	grpId uint64,
	extCnt uint64,
	legIds []uint64,
	sideIds []uint64,
	addrs []string,
) *pb.Group {
	t.Helper()
	metaBlocks, dataBlocks, err := model.GroupBlocks(
		extCnt, reactExtentSize, reactBdevConf(),
	)
	if err != nil {
		t.Fatalf("group blocks: %v", err)
	}
	grp := &pb.Group{
		GrpId:      grpId,
		ExtCnt:     extCnt,
		MetaBlocks: metaBlocks,
		DataBlocks: dataBlocks,
	}
	for idx := range legIds {
		grp.LegList = append(grp.LegList, &pb.Leg{
			LegId:  legIds[idx],
			LegIdx: uint32(idx),
			SideList: []*pb.Side{{
				SideId:      sideIds[idx],
				AddrPort:    addrs[idx],
				Provisioned: true,
			}},
		})
	}
	return grp
}

// reactDataBlocks is the data_blocks of a group of extCnt extents, the unit
// the AR6 data pending rule sums.
func reactDataBlocks(t *testing.T, extCnt uint64) uint64 {
	t.Helper()
	_, dataBlocks, err := model.GroupBlocks(
		extCnt, reactExtentSize, reactBdevConf(),
	)
	if err != nil {
		t.Fatalf("group blocks: %v", err)
	}
	return dataBlocks
}

// reactMetaBlocks is the number of 4 KiB dm-thin METADATA blocks a meta group
// of extCnt extents contributes to the pool's metadata device.
func reactMetaBlocks(t *testing.T, extCnt uint64) uint64 {
	t.Helper()
	return reactDataBlocks(t, extCnt) * reactBlockSize / thinMetaBlockSize
}

// reactFixture is the SP every reaction test starts from: two cntlrs (one
// primary), one slice with one meta group (1 extent) and one data group (2
// extents), every leg healthy and every side provisioned.
func reactFixture(t *testing.T) *model.SpState {
	t.Helper()
	return &model.SpState{
		Rev: 9,
		Conf: &pb.SpConf{
			SpId:           testSpId,
			ShardCode:      testShard,
			NextId:         1000,
			BdevConf:       reactBdevConf(),
			CntlidSlotList: []uint32{0},
			SpLevel:        pb.SpLevel_SP_LEVEL_READWRITE,
			CntlrIdList:    []uint64{reactCntlrA, reactCntlrB},
			SliceIdList:    []uint64{reactSliceId},
			TdNameList:     []string{"td0"},
		},
		Cntlrs: map[uint64]*pb.Cntlr{
			reactCntlrA: {AddrPort: reactCnA, Primary: true},
			reactCntlrB: {AddrPort: reactCnB},
		},
		Slices: map[uint64]*pb.Slice{
			reactSliceId: {
				SliceIdx: 0,
				MetaGrpList: []*pb.Group{reactGroup(
					t, reactMetaGrp, 1,
					[]uint64{reactMetaLegA, reactMetaLegB},
					[]uint64{reactSideMetaA, reactSideMetaB},
					[]string{reactDnA, reactDnB},
				)},
				DataGrpList: []*pb.Group{reactGroup(
					t, reactDataGrp, 2,
					[]uint64{reactDataLegA, reactDataLegB},
					[]uint64{reactSideDataA, reactSideDataB},
					[]string{reactDnA, reactDnB},
				)},
			},
		},
		Tds:      []*pb.ThinDevice{{TdId: 500, DevId: 1}},
		TdNames:  []string{"td0"},
		DnByAddr: map[string]*pb.DnConf{},
		CnByAddr: map[string]*pb.CnConf{},
	}
}

// ---------------------------------------------------------------------------
// The fake model surface
// ---------------------------------------------------------------------------

// reactionCall is one mutation a pass ran, in call order.
type reactionCall struct {
	op          string
	oldId       uint64
	newId       uint64
	sliceId     uint64
	grpId       uint64
	isMeta      bool
	poolTotal   uint64
	legs        []model.Cand
	asPrimary   bool
	spareLegId  uint64
	targetLegId uint64
	now         uint64
}

// candQuery is one allocator scan a pass ran.
type candQuery struct {
	kind        string
	candExt     uint64
	candCnt     int
	requiredCnt int
	black       []string
	excludeLocs []string
	spCn        []string
}

// fakeReactionOps is the §13 stand-in for model: canned candidates, a canned
// error and a record of every scan and every mutation.
type fakeReactionOps struct {
	mu      sync.Mutex
	dnCands []model.Cand
	cnCands []model.Cand
	scanErr error
	opErr   error
	calls   []reactionCall
	queries []candQuery
}

func (o *fakeReactionOps) record(call reactionCall) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, call)
	return o.opErr
}

func (o *fakeReactionOps) allCalls() []reactionCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]reactionCall(nil), o.calls...)
}

func (o *fakeReactionOps) allQueries() []candQuery {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]candQuery(nil), o.queries...)
}

func (o *fakeReactionOps) findDnCandidates(
	ctx context.Context,
	cid uint64,
	cc *pb.ClusterConf,
	candExt uint64,
	candCnt int,
	requiredCnt int,
	black []string,
	excludeLocs []string,
) ([]model.Cand, error) {
	o.mu.Lock()
	o.queries = append(o.queries, candQuery{
		kind: "dn", candExt: candExt, candCnt: candCnt,
		requiredCnt: requiredCnt, black: black, excludeLocs: excludeLocs,
	})
	cands, err := o.dnCands, o.scanErr
	o.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return cands, nil
}

func (o *fakeReactionOps) findCnCandidates(
	ctx context.Context,
	cid uint64,
	candExt uint64,
	candCnt int,
	black []string,
	spCnAddrs []string,
) ([]model.Cand, error) {
	o.mu.Lock()
	o.queries = append(o.queries, candQuery{
		kind: "cn", candExt: candExt, candCnt: candCnt,
		black: black, spCn: spCnAddrs,
	})
	cands, err := o.cnCands, o.scanErr
	o.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return cands, nil
}

func (o *fakeReactionOps) failover(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	oldId uint64,
	newId uint64,
	now uint64,
) error {
	return o.record(reactionCall{
		op: "failover", oldId: oldId, newId: newId, now: now,
	})
}

func (o *fakeReactionOps) growSlice(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	sliceId uint64,
	isMeta bool,
	poolTotal uint64,
	cc *pb.ClusterConf,
	legs []model.Cand,
) (uint64, error) {
	err := o.record(reactionCall{
		op: "grow", sliceId: sliceId, isMeta: isMeta,
		poolTotal: poolTotal, legs: legs,
	})
	return 999, err
}

func (o *fakeReactionOps) replaceCntlr(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	oldId uint64,
	newCn model.Cand,
	asPrimary bool,
	now uint64,
) (uint64, error) {
	err := o.record(reactionCall{
		op: "replace", oldId: oldId, asPrimary: asPrimary, now: now,
		legs: []model.Cand{newCn},
	})
	return 555, err
}

func (o *fakeReactionOps) createSpareLeg(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	sliceId uint64,
	grpId uint64,
	dn model.Cand,
	cc *pb.ClusterConf,
) (uint64, error) {
	err := o.record(reactionCall{
		op: "create_spare", sliceId: sliceId, grpId: grpId,
		legs: []model.Cand{dn},
	})
	return 888, err
}

func (o *fakeReactionOps) switchSpareLeg(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	sliceId uint64,
	grpId uint64,
	spareLegId uint64,
	targetLegId uint64,
) error {
	return o.record(reactionCall{
		op: "switch_spare", sliceId: sliceId, grpId: grpId,
		spareLegId: spareLegId, targetLegId: targetLegId,
	})
}

// ---------------------------------------------------------------------------
// The harness
// ---------------------------------------------------------------------------

// reactHarness drives w.reactionPass synchronously over a fixture SpState:
// the pass is a pure function of the snapshot, the primary's CntlrInfo and
// now, so no goroutine and no clock stepping is needed to test it.
type reactHarness struct {
	t     *testing.T
	logs  *logCapture
	clk   *fakeClock
	store *fakeStore
	deps  *deps
	state *model.SpState
	ops   *fakeSpOps
	rops  *fakeReactionOps
	w     *spWorker
}

func newReactHarness(t *testing.T, state *model.SpState) *reactHarness {
	t.Helper()
	logs := captureLogs(t)
	clk := newFakeClock()
	store := newFakeStore()
	d := newTestDeps(testConfig(common.WorkerRoleSp), store, clk)
	d.conf.mu.Lock()
	d.conf.entries[testCid] = model.ResolveClusterConf(reactClusterConf())
	d.conf.mu.Unlock()
	store.seed(t, model.SpRevKey(testShard, testCid, testSpId), &pb.SpRev{
		Revision: reactSpRevision,
		SpName:   testSpName,
	})
	w := spTestWorker(d)
	ops := &fakeSpOps{state: state}
	rops := &fakeReactionOps{}
	w.ops = ops
	w.react = newReactor(rops)
	return &reactHarness{
		t: t, logs: logs, clk: clk, store: store, deps: d,
		state: state, ops: ops, rops: rops, w: w,
	}
}

// now is the pass's "now" — the fake clock's unix seconds.
func (h *reactHarness) now() uint64 {
	return h.clk.nowUnix()
}

// ago is an err_epoch that is seconds old at the current fake now.
func (h *reactHarness) ago(seconds uint64) uint64 {
	return h.now() - seconds
}

// pass runs one reaction pass.
func (h *reactHarness) pass() {
	h.t.Helper()
	h.w.reactionPass(context.Background())
}

// setPrimaryInfo installs the CntlrInfo a cntlr's child last reported (AR1).
func (h *reactHarness) setPrimaryInfo(cntlrId uint64, info *pb.CntlrInfo) {
	h.w.cntlrs[cntlrId] = &cntlrChild{driver: &cntlrDriver{lastInfo: info}}
}

// setPool installs one slice's thin-pool row on the primary.
func (h *reactHarness) setPool(sliceId uint64, status pb.ResStatus, details string) {
	h.t.Helper()
	child, ok := h.w.cntlrs[reactCntlrA]
	if !ok {
		h.setPrimaryInfo(reactCntlrA, &pb.CntlrInfo{})
		child = h.w.cntlrs[reactCntlrA]
	}
	info := child.driver.lastInfo
	if info.SliceIdToDmPool == nil {
		info.SliceIdToDmPool = map[uint64]*pb.ResInfo{}
	}
	info.SliceIdToDmPool[sliceId] = &pb.ResInfo{
		ResName: "pool", Status: status, Details: details,
	}
}

// setLegRow installs one leg's §3.6 probe row on the primary (AR8 readiness).
func (h *reactHarness) setLegRow(legId uint64, status pb.ResStatus) {
	h.t.Helper()
	child, ok := h.w.cntlrs[reactCntlrA]
	if !ok {
		h.setPrimaryInfo(reactCntlrA, &pb.CntlrInfo{})
		child = h.w.cntlrs[reactCntlrA]
	}
	info := child.driver.lastInfo
	if info.LegIdToLeg == nil {
		info.LegIdToLeg = map[uint64]*pb.ResInfo{}
	}
	info.LegIdToLeg[legId] = &pb.ResInfo{ResName: "leg", Status: status}
}

// slice is the fixture's one slice.
func (h *reactHarness) slice() *pb.Slice {
	return h.state.Slices[reactSliceId]
}

// dataGrp / metaGrp are the fixture's two groups.
func (h *reactHarness) dataGrp() *pb.Group {
	return h.slice().GetDataGrpList()[0]
}

func (h *reactHarness) metaGrp() *pb.Group {
	return h.slice().GetMetaGrpList()[0]
}

// legOf finds a leg of the fixture by id, in either list of either group.
func (h *reactHarness) legOf(legId uint64) *pb.Leg {
	h.t.Helper()
	for _, grp := range allGroups(h.slice()) {
		for _, list := range [][]*pb.Leg{
			grp.GetLegList(), grp.GetSpareLegList(),
		} {
			for _, leg := range list {
				if leg.GetLegId() == legId {
					return leg
				}
			}
		}
	}
	h.t.Fatalf("leg %d not in the fixture", legId)
	return nil
}

// dnCands hands the fake allocator a set of DN candidates.
func (h *reactHarness) dnCands(addrs ...string) {
	var cands []model.Cand
	for idx, addr := range addrs {
		cands = append(cands, model.Cand{
			AddrPort: addr,
			Location: fmt.Sprintf("rack%d", idx),
			FreeExt:  64,
		})
	}
	h.rops.dnCands = cands
}

// cnCands hands the fake allocator a set of CN candidates.
func (h *reactHarness) cnCands(addrs ...string) {
	var cands []model.Cand
	for idx, addr := range addrs {
		cands = append(cands, model.Cand{
			AddrPort: addr,
			Location: fmt.Sprintf("rack%d", idx),
			FreeExt:  64,
		})
	}
	h.rops.cnCands = cands
}

// ops assertions ------------------------------------------------------------

// wantOps asserts the exact sequence of mutations the passes ran.
func (h *reactHarness) wantOps(want ...string) []reactionCall {
	h.t.Helper()
	calls := h.rops.allCalls()
	got := make([]string, 0, len(calls))
	for _, call := range calls {
		got = append(got, call.op)
	}
	if len(got) != len(want) {
		h.t.Fatalf("ops = %v, want %v", got, want)
	}
	for idx := range want {
		if got[idx] != want[idx] {
			h.t.Fatalf("ops = %v, want %v", got, want)
		}
	}
	return calls
}

// wantApplied asserts the kinds of the §12 `reaction applied` records.
func (h *reactHarness) wantApplied(want ...string) []map[string]any {
	h.t.Helper()
	recs := h.logs.withMsg(msgReactionApplied)
	if len(recs) != len(want) {
		h.t.Fatalf("applied = %v, want %v", kindsOf(recs), want)
	}
	for idx, kind := range want {
		if recs[idx]["kind"] != kind {
			h.t.Fatalf("applied = %v, want %v", kindsOf(recs), want)
		}
	}
	return recs
}

// wantSkipped asserts one `reaction skipped` record with that kind and reason.
func (h *reactHarness) wantSkipped(kind string, reason string) {
	h.t.Helper()
	for _, rec := range h.logs.withMsg(msgReactionSkipped) {
		if rec["kind"] == kind && rec["reason"] == reason {
			return
		}
	}
	h.t.Fatalf(
		"no `reaction skipped` kind=%s reason=%s; got %v",
		kind, reason, h.logs.withMsg(msgReactionSkipped),
	)
}

// wantNoSkip asserts that no `reaction skipped` record was emitted at all.
func (h *reactHarness) wantNoSkip() {
	h.t.Helper()
	if recs := h.logs.withMsg(msgReactionSkipped); len(recs) != 0 {
		h.t.Fatalf("unexpected `reaction skipped`: %v", recs)
	}
}

func kindsOf(recs []map[string]any) []any {
	out := make([]any, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec["kind"])
	}
	return out
}

// poolLine renders a realistic `dmsetup status` thin-pool line.
func poolLine(usedMeta, totalMeta, usedData, totalData uint64) string {
	return fmt.Sprintf(
		"0 20971520 thin-pool 4 %d/%d %d/%d - rw discard_passdown "+
			"queue_if_no_space - 1024",
		usedMeta, totalMeta, usedData, totalData,
	)
}

// ---------------------------------------------------------------------------
// AR2 — priority and one action per pass
// ---------------------------------------------------------------------------

// TestReactionPriority pins AR2: with all four triggers armed at once, each
// pass runs exactly ONE action, and the one it runs is the highest-priority
// applicable reaction.
func TestReactionPriority(t *testing.T) {
	// A pool far over the watermark, an unhealthy standby and an unhealthy
	// leg are armed in every case; only the failover trigger is varied.
	armed := func(h *reactHarness) {
		total := reactDataBlocks(t, 2)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, total, total))
		h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(700)
		h.legOf(reactDataLegA).ErrEpoch = h.ago(1300)
		h.dnCands(reactDnC, reactDnD)
		h.cnCands(reactCnC)
	}

	t.Run("failover first", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		armed(h)
		// The standby has to be healthy for AR5 to have a candidate at all.
		h.state.Cntlrs[reactCntlrB].ErrEpoch = 0
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
		h.pass()
		calls := h.wantOps("failover")
		if calls[0].oldId != reactCntlrA || calls[0].newId != reactCntlrB {
			t.Fatalf("failover %d -> %d", calls[0].oldId, calls[0].newId)
		}
		recs := h.wantApplied(reactionFailover)
		if recs[0]["revision"] != float64(reactSpRevision) {
			t.Fatalf("revision = %v", recs[0]["revision"])
		}
	})

	t.Run("grow second", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		armed(h)
		h.pass()
		h.wantOps("grow")
		h.wantApplied(reactionGrowData)
	})

	t.Run("replace third", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		armed(h)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, 1, 1000))
		h.pass()
		calls := h.wantOps("replace")
		if calls[0].oldId != reactCntlrB || calls[0].asPrimary {
			t.Fatalf("replace = %+v", calls[0])
		}
		h.wantApplied(reactionReplaceCntlr)
	})

	t.Run("leg repair last", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		armed(h)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, 1, 1000))
		h.state.Cntlrs[reactCntlrB].ErrEpoch = 0
		h.pass()
		calls := h.wantOps("create_spare")
		if calls[0].grpId != reactDataGrp {
			t.Fatalf("spare on group %d", calls[0].grpId)
		}
		h.wantApplied(reactionSpareCreate)
	})
}

// TestReactionRunsOnTheCoordinatorTick pins AR1's cadence wiring: the pass is
// what the sp coordinator's cntlr_interval tick runs (RW20), not something a
// caller has to drive by hand.
func TestReactionRunsOnTheCoordinatorTick(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
	h.w.tick()
	h.wantOps("failover")
	h.wantApplied(reactionFailover)
}

// TestReactionQuietPassDoesNothing checks that a healthy SP produces neither a
// mutation nor a record: a pass is not a heartbeat.
func TestReactionQuietPassDoesNothing(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.pass()
	h.wantOps()
	h.wantApplied()
	h.wantNoSkip()
}

// ---------------------------------------------------------------------------
// AR3 — suppression
// ---------------------------------------------------------------------------

// TestReactionSuppressed pins AR3: nothing runs for a deleting SP or at
// sp_level >= SP_LEVEL_NO_THINPOOL, and the record is emitted once per
// transition rather than once per pass.
func TestReactionSuppressed(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(conf *pb.SpConf)
		suppressed bool
	}{
		{"deleting", func(c *pb.SpConf) { c.Deleting = true }, true},
		{"no_thinpool", func(c *pb.SpConf) {
			c.SpLevel = pb.SpLevel_SP_LEVEL_NO_THINPOOL
		}, true},
		{"no_migration", func(c *pb.SpConf) {
			c.SpLevel = pb.SpLevel_SP_LEVEL_NO_MIGRATION
		}, true},
		{"readonly", func(c *pb.SpConf) {
			c.SpLevel = pb.SpLevel_SP_LEVEL_READONLY
		}, false},
		{"no_clone", func(c *pb.SpConf) {
			c.SpLevel = pb.SpLevel_SP_LEVEL_NO_CLONE
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			tc.mutate(h.state.Conf)
			h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
			h.pass()
			h.pass()
			if tc.suppressed {
				h.wantOps()
				h.wantApplied()
				if got := len(
					h.logs.withMsg(msgReactionSuppressed),
				); got != 1 {
					t.Fatalf("suppressed records = %d, want 1", got)
				}
				return
			}
			h.wantOps("failover", "failover")
			if got := len(h.logs.withMsg(msgReactionSuppressed)); got != 0 {
				t.Fatalf("suppressed records = %d, want 0", got)
			}
		})
	}
}

// TestReactionDisabledCntlrIsHandsOff pins the other half of AR3: a disabled
// cntlr is never failed over TO and never replaced.
func TestReactionDisabledCntlrIsHandsOff(t *testing.T) {
	t.Run("never failed over to", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
		h.state.Cntlrs[reactCntlrB].Disabled = true
		h.cnCands(reactCnC)
		h.pass()
		// AR5 has no candidate; AR7 does not fire either, because the
		// primary has not been unhealthy for cntlr_unhealthy yet.
		h.wantOps()
		h.wantSkipped(reactionFailover, reasonNoCandidate)
	})

	t.Run("never replaced", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.state.Cntlrs[reactCntlrB].Disabled = true
		h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(700)
		h.cnCands(reactCnC)
		h.pass()
		h.wantOps()
		h.wantApplied()
	})
}

// ---------------------------------------------------------------------------
// AR5 — the failover triggers
// ---------------------------------------------------------------------------

// TestReactionDisabledPrimaryFailsOver pins AR5's second trigger: `disabled`
// on the PRIMARY fires on its own and immediately (architecture.md §8.6 —
// "disabling the current primary triggers the §10.4 primary re-election"),
// with no threshold to wait out. AR3's hands-off rule is unchanged in the
// other direction: a disabled cntlr is still never a candidate.
func TestReactionDisabledPrimaryFailsOver(t *testing.T) {
	t.Run("healthy disabled primary", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.state.Cntlrs[reactCntlrA].Disabled = true
		h.pass()
		calls := h.wantOps("failover")
		if calls[0].oldId != reactCntlrA || calls[0].newId != reactCntlrB {
			t.Fatalf("failover %d -> %d", calls[0].oldId, calls[0].newId)
		}
		h.wantApplied(reactionFailover)
		h.wantNoSkip()
		for _, cntlrId := range sortedIds(h.state.Conf.GetCntlrIdList()) {
			if got := h.state.Cntlrs[cntlrId].GetErrEpoch(); got != 0 {
				t.Fatalf(
					"cntlr %d err_epoch = %d; the trigger is the disabled "+
						"flag alone", cntlrId, got,
				)
			}
		}
	})

	// The no-candidate skip still does not end the pass (AR2's continue
	// list): with every other cntlr ineligible — disabled, or unhealthy —
	// the disabled primary stays put and the next reaction runs.
	for _, tc := range []struct {
		name   string
		mutate func(h *reactHarness, cntlr *pb.Cntlr)
	}{
		{"other disabled", func(h *reactHarness, cntlr *pb.Cntlr) {
			cntlr.Disabled = true
		}},
		{"other unhealthy", func(h *reactHarness, cntlr *pb.Cntlr) {
			cntlr.ErrEpoch = h.ago(10)
		}},
	} {
		t.Run("no candidate, "+tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.state.Cntlrs[reactCntlrA].Disabled = true
			tc.mutate(h, h.state.Cntlrs[reactCntlrB])
			// AR6 is armed so that the continuation is observable.
			total := reactDataBlocks(t, 2)
			h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
				poolLine(1, 1000, total, total))
			h.dnCands(reactDnC, reactDnD)
			h.pass()
			h.wantSkipped(reactionFailover, reasonNoCandidate)
			h.wantOps("grow")
			h.wantApplied(reactionGrowData)
		})
	}
}

// ---------------------------------------------------------------------------
// AR6 — the `dmsetup status` parser
// ---------------------------------------------------------------------------

// TestParseThinPoolStatus runs the AR6 parser over real lines and over
// garbage.
func TestParseThinPoolStatus(t *testing.T) {
	ok := []struct {
		name  string
		line  string
		usage poolUsage
	}{
		{
			name: "kernel line",
			line: "0 20971520 thin-pool 0 1128/32768 5344/163840 - rw " +
				"discard_passdown queue_if_no_space - 1024",
			usage: poolUsage{1128, 32768, 5344, 163840},
		},
		{
			name: "device name prefix",
			line: "dnv-0000000000000011-2-pool: 0 8192 thin-pool 5 " +
				"40/1024 600/1024 - rw discard_passdown " +
				"queue_if_no_space needs_check 512",
			usage: poolUsage{40, 1024, 600, 1024},
		},
		{
			name:  "trailing fields absent",
			line:  "0 8192 thin-pool 5 40/1024 600/1024",
			usage: poolUsage{40, 1024, 600, 1024},
		},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			usage, parsed := parseThinPoolStatus(tc.line)
			if !parsed || usage != tc.usage {
				t.Fatalf("parse = %+v %v, want %+v", usage, parsed, tc.usage)
			}
		})
	}

	bad := []struct {
		name string
		line string
	}{
		{"empty", ""},
		{"whitespace", "   \t "},
		{"failed pool", "0 20971520 thin-pool Fail"},
		{"other target", "0 8192 linear 8:16 2048"},
		{"truncated", "0 8192 thin-pool 5 40/1024"},
		{"not a number", "0 8192 thin-pool 5 x/1024 600/1024 - rw"},
		{"no slash", "0 8192 thin-pool 5 40 600 - rw"},
		{"negative", "0 8192 thin-pool 5 -1/1024 600/1024 - rw"},
		{"words", "the pool is fine, thanks"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if usage, parsed := parseThinPoolStatus(tc.line); parsed {
				t.Fatalf("parsed garbage %q as %+v", tc.line, usage)
			}
		})
	}
}

// TestReactionBadPoolLineLoggedOncePerChange pins AR6's "an unparsable line is
// skipped and logged once per change".
func TestReactionBadPoolLineLoggedOncePerChange(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, "thin-pool Fail")
	h.pass()
	h.pass()
	if got := len(h.logs.withMsg(msgPoolStatusUnparsable)); got != 1 {
		t.Fatalf("unparsable records = %d, want 1", got)
	}
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, "thin-pool Error")
	h.pass()
	if got := len(h.logs.withMsg(msgPoolStatusUnparsable)); got != 2 {
		t.Fatalf("unparsable records = %d, want 2", got)
	}
	// A line that parses drops the memo, so the same bad line reported again
	// is a change and is reported again.
	total := reactDataBlocks(t, 2)
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
		poolLine(1, 1000, 1, total))
	h.pass()
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, "thin-pool Error")
	h.pass()
	if got := len(h.logs.withMsg(msgPoolStatusUnparsable)); got != 3 {
		t.Fatalf("unparsable records = %d, want 3", got)
	}
	h.wantOps()
}

// ---------------------------------------------------------------------------
// AR6 — thresholds, the pending rule and the candidate rules
// ---------------------------------------------------------------------------

// TestReactionGrowOnlyForOkPool pins AR6's "an ERROR / PROVISIONING / absent
// pool is never grown", and the lwm > 100 kill switch.
func TestReactionGrowOnlyForOkPool(t *testing.T) {
	total := reactDataBlocks(t, 2)
	breach := poolLine(1, 1000, total, total)
	cases := []struct {
		name   string
		status pb.ResStatus
		lwm    uint32
		grow   bool
	}{
		{"ok", pb.ResStatus_RES_STATUS_OK, 50, true},
		{"error", pb.ResStatus_RES_STATUS_ERROR, 50, false},
		{"provisioning", pb.ResStatus_RES_STATUS_PROVISIONING, 50, false},
		{"missing", pb.ResStatus_RES_STATUS_MISSING, 50, false},
		{"lwm default", pb.ResStatus_RES_STATUS_OK, 0, true},
		{"lwm off", pb.ResStatus_RES_STATUS_OK, 101, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.state.Conf.BdevConf.DmPoolConf.LowWaterMarkPct = tc.lwm
			h.setPool(reactSliceId, tc.status, breach)
			h.dnCands(reactDnC, reactDnD)
			h.pass()
			if tc.grow {
				h.wantOps("grow")
				return
			}
			h.wantOps()
		})
	}
}

// TestReactionGrowNoPrimaryInfo checks that a pass whose primary has not
// reported yet grows nothing: AR6 reads the usage off the primary's own
// CntlrInfo and never guesses.
func TestReactionGrowNoPrimaryInfo(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.dnCands(reactDnC, reactDnD)
	h.pass()
	h.wantOps()
}

// TestReactionDataGrowPending pins AR6's stateless pending rule for the DATA
// unit: the first grow runs, no second one starts while the reported total is
// still the pre-grow total, and one does again as soon as the new group is
// visible in it.
func TestReactionDataGrowPending(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.dnCands(reactDnC, reactDnD)
	oneGrp := reactDataBlocks(t, 2)

	// One data group: the sum over "all but the last" is empty, so nothing is
	// pending and the breach grows.
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
		poolLine(1, 1000, oneGrp*80/100, oneGrp))
	h.pass()
	calls := h.wantOps("grow")
	if calls[0].isMeta || calls[0].sliceId != reactSliceId {
		t.Fatalf("grow = %+v, want a data grow of the fixture slice", calls[0])
	}
	if len(calls[0].legs) != 2 {
		t.Fatalf("grow legs = %v, want two", calls[0].legs)
	}
	if calls[0].legs[0].AddrPort == calls[0].legs[1].AddrPort {
		t.Fatalf("both legs on %s", calls[0].legs[0].AddrPort)
	}
	query := h.rops.allQueries()[0]
	if query.kind != "dn" || query.candExt != 2 {
		t.Fatalf("scan = %+v, want ext_cnt 2 of the first data group", query)
	}
	if query.candCnt != 2*common.DefaultAllocDnBatchSize {
		t.Fatalf("candCnt = %d", query.candCnt)
	}
	if query.requiredCnt != 2 {
		t.Fatalf("requiredCnt = %d, want one per leg", query.requiredCnt)
	}
	if len(query.black) != 0 {
		t.Fatalf("black = %v, want empty", query.black)
	}

	// The group lands in etcd but the pool still reports the old total: the
	// grow is pending and no second one starts.
	h.slice().DataGrpList = append(h.slice().DataGrpList, reactGroup(
		t, 22, 2, []uint64{300, 301}, []uint64{400, 401},
		[]string{reactDnC, reactDnD},
	))
	h.pass()
	h.wantOps("grow")
	h.wantSkipped(reactionGrowData, reasonGrowPending)

	// The new group becomes visible in the reported total: not pending any
	// more, and the usage still breaches.
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
		poolLine(1, 1000, 2*oneGrp*80/100, 2*oneGrp))
	h.pass()
	h.wantOps("grow", "grow")
}

// TestReactionMetaGrowPending pins the same rule for the METADATA unit, whose
// counts are in dm-thin's fixed 4 KiB blocks rather than the pool's data
// block size.
func TestReactionMetaGrowPending(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.dnCands(reactDnC, reactDnD)
	metaTotal := reactMetaBlocks(t, 1)
	dataTotal := reactDataBlocks(t, 2)

	// Metadata over the watermark, data well under it.
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
		poolLine(metaTotal*80/100, metaTotal, 1, dataTotal))
	h.pass()
	calls := h.wantOps("grow")
	if !calls[0].isMeta {
		t.Fatalf("grow = %+v, want a meta grow", calls[0])
	}
	// The §8.5 ladder: a slice whose meta groups total 1 extent grows by 1.
	if query := h.rops.allQueries()[0]; query.candExt != 1 {
		t.Fatalf("scan = %+v, want the ladder ext_cnt 1", query)
	}

	// The second meta group exists in etcd but not yet in the reported total.
	h.slice().MetaGrpList = append(h.slice().MetaGrpList, reactGroup(
		t, 23, 1, []uint64{310, 311}, []uint64{410, 411},
		[]string{reactDnC, reactDnD},
	))
	h.pass()
	h.wantOps("grow")
	h.wantSkipped(reactionGrowMeta, reasonGrowPending)

	// Visible now, and still over the watermark.
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
		poolLine(2*metaTotal*80/100, 2*metaTotal, 1, dataTotal))
	h.pass()
	calls = h.wantOps("grow", "grow")
	if !calls[1].isMeta {
		t.Fatalf("second grow = %+v, want a meta grow", calls[1])
	}
	// The ladder doubles: a slice at 2 meta extents grows by 2.
	queries := h.rops.allQueries()
	if last := queries[len(queries)-1]; last.candExt != 2 {
		t.Fatalf("scan = %+v, want the ladder ext_cnt 2", last)
	}
}

// TestReactionDataGrowsBeforeMeta pins AR6's "data is checked first when both
// breach".
func TestReactionDataGrowsBeforeMeta(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.dnCands(reactDnC, reactDnD)
	metaTotal := reactMetaBlocks(t, 1)
	dataTotal := reactDataBlocks(t, 2)
	h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, poolLine(
		metaTotal*90/100, metaTotal, dataTotal*90/100, dataTotal,
	))
	h.pass()
	calls := h.wantOps("grow")
	if calls[0].isMeta {
		t.Fatalf("grew metadata first")
	}
	h.wantApplied(reactionGrowData)
}

// TestReactionGrowNeedsOneCandidatePerLeg pins AR6's "fewer than legs
// candidates ⇒ reaction skipped": a RedundMdRaid1 group needs two distinct
// DNs, a RedundNone one needs a single DN.
func TestReactionGrowNeedsOneCandidatePerLeg(t *testing.T) {
	total := reactDataBlocks(t, 2)
	breach := poolLine(1, 1000, total, total)

	t.Run("raid1 with one candidate", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, breach)
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionGrowData, reasonNoCandidate)
	})

	t.Run("redund none with one candidate", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.state.Conf.BdevConf.RedundConf = &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundNone{
				RedundNone: &pb.RedundNone{},
			},
		}
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, breach)
		h.dnCands(reactDnC)
		h.pass()
		calls := h.wantOps("grow")
		if len(calls[0].legs) != 1 {
			t.Fatalf("legs = %v, want one", calls[0].legs)
		}
		if q := h.rops.allQueries()[0]; q.candCnt != common.DefaultAllocDnBatchSize {
			t.Fatalf("candCnt = %d, want one batch", q.candCnt)
		}
	})
}

// reactPendingDataGrow puts the fixture slice into AR6's PENDING data state:
// a second data group exists in etcd but the pool still reports the pre-grow
// total, exactly as it does while the CN defers the grow ([D15], §10.4). It
// returns the reported data total.
func reactPendingDataGrow(t *testing.T, h *reactHarness) uint64 {
	t.Helper()
	h.slice().DataGrpList = append(h.slice().DataGrpList, reactGroup(
		t, 22, 2, []uint64{300, 301}, []uint64{400, 401},
		[]string{reactDnC, reactDnD},
	))
	return reactDataBlocks(t, 2)
}

// TestReactionPendingGrowDoesNotBlockThePass pins AR6's "while pending, no
// grow OF THAT KIND starts" against AR2's one-action rule: a grow that is not
// APPLICABLE — pending, or capped by the §8.5 meta ladder — is not an action,
// so it holds back neither the pool's other grow kind nor AR7 and AR8.
//
// Without this the deferral is self-locking: the one reaction that can clear a
// grow the CN deferred on a provisioning leg is AR8 on that very group, and it
// is the reaction the pending grow would be disabling.
func TestReactionPendingGrowDoesNotBlockThePass(t *testing.T) {
	t.Run("meta grows while data is pending", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.dnCands(reactDnC, reactDnD)
		dataTotal := reactPendingDataGrow(t, h)
		metaTotal := reactMetaBlocks(t, 1)
		// Both kinds breach; only the data one is pending. Metadata filling
		// up puts the pool into needs_check, so it must not wait for the CN.
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, poolLine(
			metaTotal*80/100, metaTotal, dataTotal*80/100, dataTotal,
		))
		// An unhealthy leg is armed too: the walk continues past the pending
		// data grow, but AR2 still applies exactly ONE action.
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.pass()
		calls := h.wantOps("grow")
		if !calls[0].isMeta {
			t.Fatalf("grow = %+v, want the meta grow", calls[0])
		}
		h.wantSkipped(reactionGrowData, reasonGrowPending)
		h.wantApplied(reactionGrowMeta)
	})

	t.Run("replace cntlr while data is pending", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.cnCands(reactCnC)
		dataTotal := reactPendingDataGrow(t, h)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, dataTotal*80/100, dataTotal))
		h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(700)
		h.pass()
		calls := h.wantOps("replace")
		if calls[0].oldId != reactCntlrB {
			t.Fatalf("replace = %+v", calls[0])
		}
		h.wantSkipped(reactionGrowData, reasonGrowPending)
		h.wantApplied(reactionReplaceCntlr)
	})

	t.Run("leg repair while data is pending", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.dnCands(reactDnC)
		dataTotal := reactPendingDataGrow(t, h)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, dataTotal*80/100, dataTotal))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.pass()
		calls := h.wantOps("create_spare")
		if calls[0].grpId != reactDataGrp {
			t.Fatalf("create_spare = %+v, want the data group", calls[0])
		}
		h.wantSkipped(reactionGrowData, reasonGrowPending)
		h.wantApplied(reactionSpareCreate)
	})

	t.Run("leg repair while the meta ladder is capped", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.dnCands(reactDnC)
		// 256 × 64 MiB = the §8.5 16 GiB dm-thin metadata ceiling: the meta
		// grow can never run again for this slice.
		h.slice().MetaGrpList = []*pb.Group{reactGroup(
			t, reactMetaGrp, 256,
			[]uint64{reactMetaLegA, reactMetaLegB},
			[]uint64{reactSideMetaA, reactSideMetaB},
			[]string{reactDnA, reactDnB},
		)}
		metaTotal := reactMetaBlocks(t, 256)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK, poolLine(
			metaTotal*90/100, metaTotal, 1, reactDataBlocks(t, 2),
		))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.pass()
		h.wantOps("create_spare")
		h.wantSkipped(reactionGrowMeta, reasonMetaLadderCap)
		h.wantApplied(reactionSpareCreate)
	})
}

// TestReactionPreconditionSkips checks AR2's other half: an op that raises
// model.ErrPrecondition logs `reaction skipped` with the precondition's own
// reason, and the pass ends there.
func TestReactionPreconditionSkips(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
	h.rops.opErr = &model.ErrPrecondition{
		Op: "Failover", Reason: "old cntlr is not primary",
	}
	h.pass()
	h.wantOps("failover")
	h.wantApplied()
	h.wantSkipped(reactionFailover, "old cntlr is not primary")
}

// ---------------------------------------------------------------------------
// AR7 — cntlr replacement
// ---------------------------------------------------------------------------

// TestReactionReplaceCntlrBlackList pins AR7's black list and its spCnAddrs:
// the old CN is excluded even though the node itself is healthy, and the SP's
// other cntlrs' CNs are handed over as the §6.4 exclusion.
func TestReactionReplaceCntlrBlackList(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(700)
	h.cnCands(reactCnC)
	h.pass()
	calls := h.wantOps("replace")
	if calls[0].oldId != reactCntlrB || calls[0].asPrimary {
		t.Fatalf("replace = %+v", calls[0])
	}
	if calls[0].legs[0].AddrPort != reactCnC {
		t.Fatalf("new cn = %s", calls[0].legs[0].AddrPort)
	}
	query := h.rops.allQueries()[0]
	if query.kind != "cn" {
		t.Fatalf("query = %+v, want a cn scan", query)
	}
	// candExt is the SP footprint: 1 meta extent + 2 data extents.
	if query.candExt != 3 {
		t.Fatalf("candExt = %d, want the SP footprint 3", query.candExt)
	}
	if query.candCnt != common.DefaultAllocCnBatchSize {
		t.Fatalf("candCnt = %d", query.candCnt)
	}
	if len(query.black) != 1 || query.black[0] != reactCnB {
		t.Fatalf("black = %v, want the old cntlr's cn", query.black)
	}
	if len(query.spCn) != 1 || query.spCn[0] != reactCnA {
		t.Fatalf("spCnAddrs = %v, want the other cntlr's cn", query.spCn)
	}
	h.wantApplied(reactionReplaceCntlr)
}

// TestReactionReplaceSolePrimary pins §0 item 16: the primary of an SP with no
// failover candidate is replaced by a new PRIMARY, and AR5's no-candidate
// record is emitted first — the one skip that does not end the pass.
func TestReactionReplaceSolePrimary(t *testing.T) {
	state := reactFixture(t)
	state.Conf.CntlrIdList = []uint64{reactCntlrA}
	delete(state.Cntlrs, reactCntlrB)
	h := newReactHarness(t, state)
	h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(700)
	h.cnCands(reactCnC)
	h.pass()
	calls := h.wantOps("replace")
	if calls[0].oldId != reactCntlrA || !calls[0].asPrimary {
		t.Fatalf("replace = %+v, want the primary replaced as primary",
			calls[0])
	}
	if calls[0].now != h.now() {
		t.Fatalf("now = %d, want %d", calls[0].now, h.now())
	}
	h.wantSkipped(reactionFailover, reasonNoCandidate)
	h.wantApplied(reactionReplaceCntlr)
	if q := h.rops.allQueries()[0]; len(q.spCn) != 0 {
		t.Fatalf("spCnAddrs = %v, want empty for a sole cntlr", q.spCn)
	}
}

// TestReactionPrimaryWithCandidateIsNotReplaced checks the other side of AR7:
// while a failover candidate exists the unhealthy primary is failed over, not
// replaced, however long it has been unhealthy.
func TestReactionPrimaryWithCandidateIsNotReplaced(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(9000)
	h.cnCands(reactCnC)
	h.pass()
	h.wantOps("failover")
	h.wantApplied(reactionFailover)
}

// TestReactionReplaceNoCandidate pins AR7's "none ⇒ reaction skipped".
func TestReactionReplaceNoCandidate(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(700)
	h.pass()
	h.wantOps()
	h.wantSkipped(reactionReplaceCntlr, reasonNoCandidate)
}

// TestReactionCntlrThresholdNotReached checks AR4 on the replacement path: a
// cntlr unhealthy for less than cntlr_unhealthy is left alone.
func TestReactionCntlrThresholdNotReached(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(
		common.DefaultCntlrUnhealthy - 1,
	)
	h.cnCands(reactCnC)
	h.pass()
	h.wantOps()
	h.wantApplied()
}

// ---------------------------------------------------------------------------
// AR8 — leg repair
// ---------------------------------------------------------------------------

// TestReactionLegRepairCase1 pins AR8 case 1: the leg alone is unhealthy, and
// only the LONG threshold triggers a repair.
func TestReactionLegRepairCase1(t *testing.T) {
	t.Run("below the threshold", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(
			common.DefaultLegUnhealthy - 1,
		)
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps()
	})

	t.Run("at the threshold", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		h.dnCands(reactDnC)
		h.pass()
		calls := h.wantOps("create_spare")
		if calls[0].grpId != reactDataGrp || calls[0].sliceId != reactSliceId {
			t.Fatalf("create_spare = %+v", calls[0])
		}
		if calls[0].legs[0].AddrPort != reactDnC {
			t.Fatalf("spare on %s", calls[0].legs[0].AddrPort)
		}
		query := h.rops.allQueries()[0]
		if query.candExt != 2 {
			t.Fatalf("candExt = %d, want the group's ext_cnt", query.candExt)
		}
		if query.candCnt != common.DefaultAllocDnBatchSize {
			t.Fatalf("candCnt = %d", query.candCnt)
		}
		wantBlack := map[string]bool{reactDnA: true, reactDnB: true}
		if len(query.black) != 2 {
			t.Fatalf("black = %v, want both legs' DNs", query.black)
		}
		for _, addr := range query.black {
			if !wantBlack[addr] {
				t.Fatalf("black = %v, want both legs' DNs", query.black)
			}
		}
		h.wantApplied(reactionSpareCreate)
	})
}

// TestReactionLegRepairCase2 pins AR8 case 2: an unhealthy leg whose SIDE has
// been unhealthy for the short threshold is repaired long before
// leg_unhealthy — and a side alone, with the leg healthy, triggers nothing.
func TestReactionLegRepairCase2(t *testing.T) {
	t.Run("leg and side", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		leg := h.legOf(reactDataLegA)
		leg.ErrEpoch = h.ago(1)
		leg.SideList[0].ErrEpoch = h.ago(common.DefaultSideUnhealthy)
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps("create_spare")
	})

	t.Run("side only", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		leg := h.legOf(reactDataLegA)
		leg.SideList[0].ErrEpoch = h.ago(9000)
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps()
	})

	t.Run("side below the threshold", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		leg := h.legOf(reactDataLegA)
		leg.ErrEpoch = h.ago(1)
		leg.SideList[0].ErrEpoch = h.ago(common.DefaultSideUnhealthy - 1)
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps()
	})
}

// TestReactionLegRepairSmallestLegId pins AR8's "several unhealthy legs ⇒ the
// smallest leg_id first", across both group lists of the slice.
func TestReactionLegRepairSmallestLegId(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
	h.legOf(reactMetaLegB).ErrEpoch = h.ago(9000)
	h.dnCands(reactDnC)
	h.pass()
	calls := h.wantOps("create_spare")
	if calls[0].grpId != reactMetaGrp {
		t.Fatalf("repaired group %d, want the meta group (leg %d)",
			calls[0].grpId, reactMetaLegB)
	}
}

// TestReactionLegRepairSkips pins the three preconditions of AR8 that only
// log: a RedundNone group, a leg with two sides, and a full spare list.
func TestReactionLegRepairSkips(t *testing.T) {
	t.Run("redund none", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.state.Conf.BdevConf.RedundConf = &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundNone{
				RedundNone: &pb.RedundNone{},
			},
		}
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionSpareCreate, reasonRedundNone)
	})

	t.Run("two sides", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		leg := h.legOf(reactDataLegA)
		leg.ErrEpoch = h.ago(9000)
		leg.SideList = append(leg.SideList, &pb.Side{
			SideId: 999, AddrPort: reactDnD,
		})
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionSpareCreate, reasonTwoSides)
	})

	t.Run("spare list full", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		// Two parked legs: each keeps the err_epoch it was retired with, so
		// neither is ready nor pending, and the list is full.
		for idx := 0; idx < common.MaxSpareLegPerGrp; idx++ {
			h.dataGrp().SpareLegList = append(
				h.dataGrp().SpareLegList,
				&pb.Leg{
					LegId:    uint64(700 + idx),
					ErrEpoch: h.ago(9000),
					SideList: []*pb.Side{{
						SideId:      uint64(800 + idx),
						AddrPort:    reactDnC,
						Provisioned: true,
					}},
				},
			)
		}
		h.dnCands(reactDnD)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionSpareCreate, reasonSpareListFull)
	})
}

// TestReactionLegRepairWalksPastUnrepairableLegs pins AR8's per-leg
// preconditions as part of the CANDIDATE test rather than as reasons to end
// the pass: the smallest-leg_id ordering has to run over the legs that are
// actually repairable.
//
// Both conditions last: a two-sided leg has a user migration in flight (hours)
// and a full spare list is an operator event (§0 item 17). Ending the pass on
// either would leave every other group of the SP running degraded on a single
// md-raid1 member for that whole time — one more failure there is data loss.
func TestReactionLegRepairWalksPastUnrepairableLegs(t *testing.T) {
	t.Run("two sides does not hide a larger leg", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		// The SMALLEST unhealthy leg_id is the one under the migration.
		migrating := h.legOf(reactMetaLegA)
		migrating.ErrEpoch = h.ago(9000)
		migrating.SideList = append(migrating.SideList, &pb.Side{
			SideId: 999, AddrPort: reactDnD,
		})
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnC)
		h.pass()
		calls := h.wantOps("create_spare")
		if calls[0].grpId != reactDataGrp {
			t.Fatalf("create_spare = %+v, want the data group", calls[0])
		}
		if calls[0].legs[0].AddrPort != reactDnC {
			t.Fatalf("spare on %s", calls[0].legs[0].AddrPort)
		}
		h.wantSkipped(reactionSpareCreate, reasonTwoSides)
		h.wantApplied(reactionSpareCreate)
	})

	t.Run("full spare list does not hide another group", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		// The meta group holds two parked legs — each keeps the err_epoch it
		// was retired with, so neither is ready nor pending — and its own
		// active leg has failed. Only DeleteSpareLeg can unblock it.
		for idx := 0; idx < common.MaxSpareLegPerGrp; idx++ {
			h.metaGrp().SpareLegList = append(
				h.metaGrp().SpareLegList,
				&pb.Leg{
					LegId:    uint64(700 + idx),
					ErrEpoch: h.ago(9000),
					SideList: []*pb.Side{{
						SideId:      uint64(800 + idx),
						AddrPort:    reactDnC,
						Provisioned: true,
					}},
				},
			)
		}
		h.legOf(reactMetaLegA).ErrEpoch = h.ago(9000)
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnD)
		h.pass()
		calls := h.wantOps("create_spare")
		if calls[0].grpId != reactDataGrp {
			t.Fatalf("create_spare = %+v, want the data group", calls[0])
		}
		h.wantSkipped(reactionSpareCreate, reasonSpareListFull)
		h.wantApplied(reactionSpareCreate)
	})

	t.Run("still one action per pass", func(t *testing.T) {
		// Nothing blocks either leg: the walk stops at the first repairable
		// candidate, so AR2's invariant survives the scan being able to
		// continue.
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactMetaLegA).ErrEpoch = h.ago(9000)
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnC)
		h.pass()
		calls := h.wantOps("create_spare")
		if calls[0].grpId != reactMetaGrp {
			t.Fatalf("repaired group %d, want the smallest leg_id's group",
				calls[0].grpId)
		}
		h.wantNoSkip()
	})
}

// TestReactionSpareReadiness pins AR8 steps 1 and 2: a spare is switched in
// only when its side is provisioned AND the primary reports its leg OK;
// anything short of that is a pending spare the pass waits for.
func TestReactionSpareReadiness(t *testing.T) {
	const spareLegId = uint64(700)
	const spareSideId = uint64(800)

	// build installs one spare in the data group in the given shape.
	build := func(
		t *testing.T, provisioned bool, status pb.ResStatus, legErr uint64,
	) *reactHarness {
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.dataGrp().SpareLegList = append(h.dataGrp().SpareLegList, &pb.Leg{
			LegId:    spareLegId,
			LegIdx:   2,
			ErrEpoch: legErr,
			SideList: []*pb.Side{{
				SideId:      spareSideId,
				AddrPort:    reactDnC,
				Provisioned: provisioned,
			}},
		})
		if status != pb.ResStatus_RES_STATUS_UNKNOWN {
			h.setLegRow(spareLegId, status)
		}
		h.dnCands(reactDnD)
		return h
	}

	t.Run("ready", func(t *testing.T) {
		h := build(t, true, pb.ResStatus_RES_STATUS_OK, 0)
		h.pass()
		calls := h.wantOps("switch_spare")
		if calls[0].spareLegId != spareLegId ||
			calls[0].targetLegId != reactDataLegA {
			t.Fatalf("switch = %+v", calls[0])
		}
		if calls[0].grpId != reactDataGrp {
			t.Fatalf("switch on group %d", calls[0].grpId)
		}
		h.wantApplied(reactionSpareSwitch)
	})

	t.Run("not provisioned", func(t *testing.T) {
		h := build(t, false, pb.ResStatus_RES_STATUS_OK, 0)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionSpareSwitch, reasonSparePending)
	})

	t.Run("not reported ok", func(t *testing.T) {
		h := build(t, true, pb.ResStatus_RES_STATUS_PROVISIONING, 0)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionSpareSwitch, reasonSparePending)
	})

	t.Run("not reported at all", func(t *testing.T) {
		h := build(t, true, pb.ResStatus_RES_STATUS_UNKNOWN, 0)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionSpareSwitch, reasonSparePending)
	})

	t.Run("no primary info", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.dataGrp().SpareLegList = append(h.dataGrp().SpareLegList, &pb.Leg{
			LegId: spareLegId,
			SideList: []*pb.Side{{
				SideId: spareSideId, AddrPort: reactDnC, Provisioned: true,
			}},
		})
		h.dnCands(reactDnD)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionSpareSwitch, reasonSparePending)
	})

	t.Run("dead spare makes room for another", func(t *testing.T) {
		// A parked leg — err_epoch set, side provisioned — is neither ready
		// nor pending, so the group gets a second spare instead of waiting.
		h := build(t, true, pb.ResStatus_RES_STATUS_ERROR, uint64(1))
		h.pass()
		calls := h.wantOps("create_spare")
		wantBlack := map[string]bool{
			reactDnA: true, reactDnB: true, reactDnC: true,
		}
		for _, addr := range h.rops.allQueries()[0].black {
			if !wantBlack[addr] {
				t.Fatalf("black = %v", h.rops.allQueries()[0].black)
			}
		}
		if len(h.rops.allQueries()[0].black) != 3 {
			t.Fatalf("black = %v, want three DNs",
				h.rops.allQueries()[0].black)
		}
		if calls[0].legs[0].AddrPort != reactDnD {
			t.Fatalf("spare on %s", calls[0].legs[0].AddrPort)
		}
	})
}

// TestReactionParkedLegIsNeverRepaired pins §0 item 17: only leg_list legs are
// repaired, so the leg parked by a previous switch — err_epoch and all — is
// left alone.
func TestReactionParkedLegIsNeverRepaired(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.dataGrp().SpareLegList = append(h.dataGrp().SpareLegList, &pb.Leg{
		LegId:    700,
		ErrEpoch: h.ago(9000),
		SideList: []*pb.Side{{
			SideId: 800, AddrPort: reactDnC, Provisioned: true,
		}},
	})
	h.dnCands(reactDnD)
	h.pass()
	h.wantOps()
	h.wantApplied()
	h.wantNoSkip()
}

// TestReactionSpareCreateExcludesGroupLocations pins AR8 step 3's tier-1
// exclusion (§6.5): the scan carries the DNs of every leg and spare of the
// group AND their locations, read from the pass's own snapshot of the node
// records (MD3) rather than from a second etcd round-trip.
func TestReactionSpareCreateExcludesGroupLocations(t *testing.T) {
	build := func(t *testing.T, locA string, locB string) *reactHarness {
		t.Helper()
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.state.DnByAddr[reactDnA] = &pb.DnConf{Location: locA}
		h.state.DnByAddr[reactDnB] = &pb.DnConf{Location: locB}
		h.dnCands(reactDnD)
		return h
	}
	wantScan := func(t *testing.T, h *reactHarness, wantLocs []string) {
		t.Helper()
		queries := h.rops.allQueries()
		if len(queries) != 1 || queries[0].kind != "dn" {
			t.Fatalf("queries = %+v", queries)
		}
		wantBlack := []string{reactDnA, reactDnB}
		if fmt.Sprint(queries[0].black) != fmt.Sprint(wantBlack) {
			t.Errorf("black = %v, want %v", queries[0].black, wantBlack)
		}
		if fmt.Sprint(queries[0].excludeLocs) != fmt.Sprint(wantLocs) {
			t.Errorf("exclude_locs = %v, want %v",
				queries[0].excludeLocs, wantLocs)
		}
		// The tier-2 trigger is the ONE DN this step places, never the
		// oversampled candCnt: with dn_batch_size = 16 no realistic cluster
		// fills a batch out of one domain each, and tier 1 would be discarded
		// every time (§6.5).
		if queries[0].requiredCnt != 1 {
			t.Errorf("required_cnt = %d, want 1", queries[0].requiredCnt)
		}
	}

	t.Run("two named locations", func(t *testing.T) {
		h := build(t, "rack-left", "rack-right")
		h.pass()
		calls := h.wantOps("create_spare")
		if calls[0].legs[0].AddrPort != reactDnD {
			t.Fatalf("spare on %s", calls[0].legs[0].AddrPort)
		}
		wantScan(t, h, []string{"rack-left", "rack-right"})
	})

	t.Run("a missing dn_conf contributes no location", func(t *testing.T) {
		h := build(t, "rack-left", "rack-right")
		delete(h.state.DnByAddr, reactDnB)
		h.pass()
		h.wantOps("create_spare")
		wantScan(t, h, []string{"rack-left"})
	})

	t.Run("the default location is the addr_port", func(t *testing.T) {
		// The §8.2 default makes the exclusion degenerate to the DN black
		// list, which is AR8's behavior before the two tiers existed.
		h := build(t, reactDnA, reactDnB)
		h.pass()
		h.wantOps("create_spare")
		wantScan(t, h, []string{reactDnA, reactDnB})
	})
}

// TestReactionSpareCreateNoCandidate pins AR8 step 3's empty scan.
func TestReactionSpareCreateNoCandidate(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
	h.pass()
	h.wantOps()
	h.wantSkipped(reactionSpareCreate, reasonNoCandidate)
}

// ---------------------------------------------------------------------------
// Pass inputs
// ---------------------------------------------------------------------------

// TestReactionPassNeedsClusterConf checks that a pass whose cluster is not in
// the RW21 cache does nothing at all: every allocating reaction needs its
// extent_size and batch sizes.
func TestReactionPassNeedsClusterConf(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.deps.conf.mu.Lock()
	delete(h.deps.conf.entries, testCid)
	h.deps.conf.mu.Unlock()
	h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(10)
	h.pass()
	h.wantOps()
	h.wantApplied()
}

// TestReactionPassLoadFailure checks the two load outcomes of AR1: a deleted
// SP is silent, a transient failure is reported and retried on the next tick.
func TestReactionPassLoadFailure(t *testing.T) {
	t.Run("deleted", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.ops.setErr(model.ErrNotFound)
		h.pass()
		h.wantOps()
		if got := len(h.logs.withMsg(msgSpLoadFailed)); got != 0 {
			t.Fatalf("load failures = %d, want 0", got)
		}
	})

	t.Run("transient", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.ops.setErr(context.DeadlineExceeded)
		h.pass()
		h.wantOps()
		if got := len(h.logs.withMsg(msgSpLoadFailed)); got != 1 {
			t.Fatalf("load failures = %d, want 1", got)
		}
	})
}

// TestReactionScanFailure checks that a failed allocator scan ends the pass
// with an `op_failed` record rather than a mutation.
func TestReactionScanFailure(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
	h.rops.scanErr = context.DeadlineExceeded
	h.pass()
	h.wantOps()
	h.wantSkipped(reactionSpareCreate, reasonOpFailed)
}

// ---------------------------------------------------------------------------
// Unit-level helpers
// ---------------------------------------------------------------------------

// TestGrowPendingBoundaries pins the boundary AR6 is easiest to get wrong: the
// sum over "all groups but the last" is empty for zero and one group, so
// nothing is pending there whatever the pool reports.
func TestGrowPendingBoundaries(t *testing.T) {
	one := reactDataBlocks(t, 2)
	slice0 := &pb.Slice{}
	slice1 := &pb.Slice{DataGrpList: []*pb.Group{{DataBlocks: one}}}
	slice2 := &pb.Slice{DataGrpList: []*pb.Group{
		{DataBlocks: one}, {DataBlocks: one},
	}}
	cases := []struct {
		name  string
		slice *pb.Slice
		total uint64
		want  bool
	}{
		{"no group", slice0, 0, false},
		{"one group, nothing reported", slice1, 0, false},
		{"one group, reported", slice1, one, false},
		{"two groups, old total", slice2, one, true},
		{"two groups, one block more", slice2, one + 1, false},
		{"two groups, new total", slice2, 2 * one, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage := poolUsage{totalData: tc.total}
			got := growPending(tc.slice, false, usage, reactBlockSize)
			if got != tc.want {
				t.Fatalf("growPending = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPickDistinct pins AR6's growing black list: never two picks on one DN,
// and fewer than asked for when the scan returned fewer.
func TestPickDistinct(t *testing.T) {
	cands := []model.Cand{
		{AddrPort: reactDnA}, {AddrPort: reactDnB}, {AddrPort: reactDnC},
	}
	for run := 0; run < 32; run++ {
		picks := pickDistinct(cands, 2)
		if len(picks) != 2 {
			t.Fatalf("picks = %v, want two", picks)
		}
		if picks[0].AddrPort == picks[1].AddrPort {
			t.Fatalf("picks = %v, want distinct DNs", picks)
		}
	}
	if got := pickDistinct(cands[:1], 2); len(got) != 1 {
		t.Fatalf("picks = %v, want the one candidate there was", got)
	}
	if got := pickDistinct(nil, 2); len(got) != 0 {
		t.Fatalf("picks = %v, want none", got)
	}
}

// TestReached pins AR4's underflow-safe comparison.
func TestReached(t *testing.T) {
	cases := []struct {
		name      string
		now       uint64
		errEpoch  uint64
		threshold uint32
		want      bool
	}{
		{"healthy", 1000, 0, 5, false},
		{"below", 1000, 998, 5, false},
		{"exactly", 1000, 995, 5, true},
		{"above", 1000, 900, 5, true},
		{"zero threshold", 1000, 1000, 0, true},
		{"clock went backwards", 1000, 2000, 5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reached(tc.now, tc.errEpoch, tc.threshold)
			if got != tc.want {
				t.Fatalf("reached = %v, want %v", got, tc.want)
			}
		})
	}
}
