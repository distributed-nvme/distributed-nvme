package worker

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The cutover beside RW18's flip: the side requests' flags, the three
// conditions, the STM's refusal and the record (dnv-worker.md, Log records)
// ---------------------------------------------------------------------------

// spCutoverRef is what the coordinator builds for the fixture's migration
// once its cutover waits (cutoverRefs).
var spCutoverRef = model.CutoverRef{
	MigrName:  spMigrName,
	MigrId:    spMigrId,
	SliceId:   spSliceA,
	LegId:     spLegMigr,
	SrcSideId: spSideSrc,
	DstSideId: spSideDst,
}

// spDstKey is the fixture migration's dst side.
var spDstKey = sideKey{legId: spLegMigr, sideId: spSideDst}

// cutoverFixture is spFixture with its migration waiting for the cutover:
// cutover_pending set and, when dstProvisioned, the dst side provisioned.
func cutoverFixture(dstProvisioned bool) *model.SpState {
	state := spFixture()
	state.Migrs[spMigrName].CutoverPending = true
	dstSide(state).Provisioned = dstProvisioned
	return state
}

// dstSide is the fixture migration's dst side in a state.
func dstSide(state *model.SpState) *pb.Side {
	return state.Slices[spSliceA].DataGrpList[0].LegList[0].SideList[1]
}

// awaitingSideInfo is a dst side's report while it waits for the cutover
// (dnagent.md DN18): both migration rows PROVISIONING with the reason.
func awaitingSideInfo() *pb.SideInfo {
	row := func() *pb.ResInfo {
		return &pb.ResInfo{
			Status:  pb.ResStatus_RES_STATUS_PROVISIONING,
			Details: "waiting for the cutover",
			Reason:  pb.ResReason_RES_REASON_AWAITING_CUTOVER,
		}
	}
	return &pb.SideInfo{
		MigrDstInfo: &pb.SideInfo_MigrDstInfo{
			TargetInfo:  row(),
			DmCloneInfo: row(),
		},
	}
}

// pathInfo is a cntlr report whose path rows read OK for the live sides and
// MISSING for the others of the fixture's migration leg.
func pathInfo(live ...uint64) *pb.CntlrInfo {
	rows := map[uint64]*pb.ResInfo{
		spSideSrc: resStatus("path-src", pb.ResStatus_RES_STATUS_MISSING),
		spSideDst: resStatus("path-dst", pb.ResStatus_RES_STATUS_MISSING),
	}
	for _, sideId := range live {
		rows[sideId] = resOk("path")
	}
	return &pb.CntlrInfo{SideIdToPath: rows}
}

// TestSpSideRequestsCarryTheCutoverFlag pins RW15's migration confs around
// the cutover: both sides carry the migration's cutover_pending, and the src
// side is told the dst side is provisioned only once it is provisioned AND
// the cutover is no longer pending, so the src side serves on until the
// cutover. The plan lists the migration for the cutover only while the flag
// is set and the dst side is provisioned.
func TestSpSideRequestsCarryTheCutoverFlag(t *testing.T) {
	cases := []struct {
		name           string
		pending        bool
		dstProvisioned bool
		wantDstProv    bool
		wantCutover    bool
	}{
		{"pending, the dst side zeroing", true, false, false, false},
		{"pending, the dst side provisioned", true, true, false, true},
		{"the cutover started", false, true, true, false},
		{"the cutover started, the dst side zeroing", false, false, false,
			false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captureLogs(t)
			state := cutoverFixture(tc.dstProvisioned)
			state.Migrs[spMigrName].CutoverPending = tc.pending
			plan := spTestWorker(nil).buildPlan(context.Background(), state)

			src := plan.sides[sideKey{legId: spLegMigr, sideId: spSideSrc}]
			wantSrc := &pb.SyncupSideRequest_MigrSrcConf{
				MigrId:         spMigrId,
				DstSideId:      spSideDst,
				DstDnId:        spDnIdC,
				DstProvisioned: tc.wantDstProv,
				CutoverPending: tc.pending,
			}
			if got := src.req.GetMigrSrcConf(); !proto.Equal(got, wantSrc) {
				t.Errorf("migr_src_conf =\n%v\nwant\n%v", got, wantSrc)
			}
			dst := plan.sides[spDstKey]
			wantDst := &pb.SyncupSideRequest_MigrDstConf{
				MigrId:        spMigrId,
				SrcSideId:     spSideSrc,
				SrcDnId:       spDnIdB,
				SrcNvmeTrConf: srcTrConf(),
				BlockSize:     testBlockSize,
				MetaBlocks:    5,
				DmCloneConf: &pb.DmCloneConf{
					HydrationThreshold: common.DefaultMigrThreshold,
					HydrationBatchSize: common.DefaultMigrBatchSize,
				},
				BmCnt:          2,
				CutoverPending: tc.pending,
			}
			if got := dst.req.GetMigrDstConf(); !proto.Equal(got, wantDst) {
				t.Errorf("migr_dst_conf =\n%v\nwant\n%v", got, wantDst)
			}
			if got := dst.req.GetSideConf().GetProvisioned(); got != tc.dstProvisioned {
				t.Errorf("the dst side's provisioned = %v, want %v", got,
					tc.dstProvisioned)
			}
			var want []model.CutoverRef
			if tc.wantCutover {
				want = []model.CutoverRef{spCutoverRef}
			}
			if !slices.Equal(plan.cutovers, want) {
				t.Errorf("plan cutovers = %+v, want %+v", plan.cutovers, want)
			}
		})
	}
}

// cutoverWorker is a coordinator outside any loop with the fixture
// migration's cutover pending in its latest load, a primary and a standby
// cntlr child, and the dst side's child, waiting as awaiting says. The store
// holds SpRev at revision, a bump after any the cutover's STM commits, which
// the record must not read back.
func cutoverWorker(
	t *testing.T,
	awaiting bool,
	revision uint64,
) (*spWorker, *fakeSpOps, *logCapture) {
	t.Helper()
	logs := captureLogs(t)
	store := newFakeStore()
	store.seed(t, model.SpRevKey(testShard, testCid, testSpId), &pb.SpRev{
		Revision: revision, SpName: testSpName,
	})
	d := newTestDeps(testConfig(common.WorkerRoleSp), store, newFakeClock())
	d.health = &fakeHealthWriter{}
	w := spTestWorker(d)
	ops := &fakeSpOps{state: cutoverFixture(true)}
	w.ops = ops
	w.cutovers = []model.CutoverRef{spCutoverRef}
	w.cntlrs[spCntlrPrimary] = &cntlrChild{plan: &cntlrPlan{
		cntlrId: spCntlrPrimary, primary: true, req: cntlrReq(false),
	}}
	w.cntlrs[spCntlrStandby] = &cntlrChild{plan: &cntlrPlan{
		cntlrId: spCntlrStandby, req: cntlrReq(false),
	}}
	side := &sideDriver{}
	side.setAwaiting(awaiting)
	w.sides[spDstKey] = &sideChild{driver: side}
	return w, ops, logs
}

// cntlrReq is the part of a cntlr plan's request the cutover reads: the
// Cntlr record's disabled flag.
func cntlrReq(disabled bool) *pb.SyncupCntlrRequest {
	return &pb.SyncupCntlrRequest{Cntlr: &pb.Cntlr{Disabled: disabled}}
}

// livePaths is a path report of the fixture's migration leg at revision.
func livePaths(revision uint64, live ...uint64) *pathReport {
	return newPathReport(revision, pathInfo(live...))
}

// TestSpCutoverConditions pins the three conditions of the cutover beside
// RW18's flip, read by the coordinator (applyCutovers): the migration is in
// the latest load's list (its flag set, its dst side provisioned), the dst
// side's latest report waits for the cutover, and the cntlr the plan the
// coordinator last decided makes the primary, and not disabled, reported a
// live path to the dst side, in the newest report of the drain. A standby's
// path counts for nothing, nor does a disabled primary's, in a held plan or
// not; a held plan's new primary counts, its demoted one does not.
func TestSpCutoverConditions(t *testing.T) {
	cases := []struct {
		name     string
		awaiting bool
		// noSide drops the dst side's child; noLoad empties the list.
		noSide, noLoad bool
		// held, when set, is the plan RW14's barrier holds: the standby
		// promoted and the primary demoted.
		held    bool
		reports []spReport
		// want is the cntlr the cutover is started for, 0 for none.
		want uint64
		// disabled marks the cntlr the plan makes primary disabled: the
		// primary's child plan, or the held plan's new primary.
		disabled bool
	}{
		{"the primary's live path and the side's wait", true, false, false,
			false, []spReport{{
				cntlrId: spCntlrPrimary, paths: livePaths(7, spSideDst),
			}}, spCntlrPrimary, false},
		{"a standby's live path", true, false, false, false, []spReport{{
			cntlrId: spCntlrStandby, paths: livePaths(7, spSideDst),
		}}, 0, false},
		{"the primary's path to the src side only", true, false, false, false,
			[]spReport{{
				cntlrId: spCntlrPrimary, paths: livePaths(7, spSideSrc),
			}}, 0, false},
		{"the side does not wait", false, false, false, false, []spReport{{
			cntlrId: spCntlrPrimary, paths: livePaths(7, spSideDst),
		}}, 0, false},
		{"no child for the dst side", true, true, false, false, []spReport{{
			cntlrId: spCntlrPrimary, paths: livePaths(7, spSideDst),
		}}, 0, false},
		{"no cutover pending in the latest load", true, false, true, false,
			[]spReport{{
				cntlrId: spCntlrPrimary, paths: livePaths(7, spSideDst),
			}}, 0, false},
		{"a report with no info of its own", true, false, false, false,
			[]spReport{{cntlrId: spCntlrPrimary}}, 0, false},
		{"a live report the drain's newer one replaces", true, false, false,
			false, []spReport{
				{cntlrId: spCntlrPrimary, paths: livePaths(7, spSideDst)},
				{cntlrId: spCntlrPrimary, paths: livePaths(8)},
			}, 0, false},
		{"a newer live report of the drain", true, false, false, false,
			[]spReport{
				{cntlrId: spCntlrPrimary, paths: livePaths(7)},
				{cntlrId: spCntlrPrimary, paths: livePaths(8, spSideDst)},
			}, spCntlrPrimary, false},
		{"the primary a held plan demotes", true, false, false, true,
			[]spReport{{
				cntlrId: spCntlrPrimary, paths: livePaths(7, spSideDst),
			}}, 0, false},
		{"the standby a held plan promotes", true, false, false, true,
			[]spReport{{
				cntlrId: spCntlrStandby, paths: livePaths(7, spSideDst),
			}}, spCntlrStandby, false},
		{"a disabled primary's live path", true, false, false, false,
			[]spReport{{
				cntlrId: spCntlrPrimary, paths: livePaths(7, spSideDst),
			}}, 0, true},
		{"a disabled primary a held plan promotes", true, false, false, true,
			[]spReport{{
				cntlrId: spCntlrStandby, paths: livePaths(7, spSideDst),
			}}, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, ops, _ := cutoverWorker(t, tc.awaiting, testSpRev+1)
			if tc.noSide {
				delete(w.sides, spDstKey)
			}
			if tc.noLoad {
				w.cutovers = nil
			}
			if tc.disabled {
				w.cntlrs[spCntlrPrimary].plan.req = cntlrReq(true)
			}
			if tc.held {
				w.held = &cntlrHold{
					revision: testSpRev + 1,
					plans: map[uint64]*cntlrPlan{
						spCntlrPrimary: {
							cntlrId: spCntlrPrimary, req: cntlrReq(false),
						},
						spCntlrStandby: {
							cntlrId: spCntlrStandby, primary: true,
							req: cntlrReq(tc.disabled),
						},
					},
					// A side the barrier waits for, short of the held
					// revision, keeps the hold through handleReports' own
					// release check.
					awaited: map[sideKey]bool{spDstKey: true},
				}
				w.sides[spDstKey].synced = testSpRev
			}
			for _, rep := range tc.reports[1:] {
				w.reportCh <- rep
			}
			w.handleReports(tc.reports[0])
			calls := ops.cutoverCalls()
			if tc.want == 0 {
				if len(calls) != 0 {
					t.Fatalf("cutover started: %+v", calls)
				}
				return
			}
			want := cutoverCall{spName: testSpName, ref: spCutoverRef,
				cntlrId: tc.want}
			if len(calls) != 1 || calls[0] != want {
				t.Fatalf("cutover calls = %+v, want one %+v", calls, want)
			}
		})
	}
}

// TestSpCutoverRecord pins the cutover's records (dnv-worker.md, Log
// records): a refused STM logs "sp flip failed" with the kind cutover, keeps
// the migration, and is tried again on the next report; a committed one
// logs "flip applied" with the kind cutover, naming the migration, its two
// sides, the reporting primary, the revision of the reply it acted on and
// the revision its own STM committed — never a later one the store holds
// by the time the record is written — and leaves the list, so a later
// report starts nothing.
func TestSpCutoverRecord(t *testing.T) {
	const committed = testSpRev + 2
	w, ops, logs := cutoverWorker(t, true, committed+1)
	ops.cutoverRev = committed
	report := func(revision uint64) {
		w.handleReports(spReport{
			cntlrId: spCntlrPrimary, paths: livePaths(revision, spSideDst),
		})
	}

	ops.setCutoverErr(errors.New("StartCutover: precondition failed: " +
		"cntlr not primary"))
	report(testSpRev)
	failed := logs.withMsg(msgSpFlipFailed)
	if len(failed) != 1 || failed[0]["kind"] != flipKindCutover ||
		failed[0]["migr_name"] != spMigrName {
		t.Fatalf("sp flip failed records = %v, want one of kind %q",
			failed, flipKindCutover)
	}
	if got := len(logs.withMsg(msgFlipApplied)); got != 0 {
		t.Fatalf("%d flip applied records for a refused cutover", got)
	}
	if !slices.Equal(w.cutovers, []model.CutoverRef{spCutoverRef}) {
		t.Fatalf("a refused cutover left the list: %+v", w.cutovers)
	}

	ops.setCutoverErr(nil)
	report(testSpRev + 1)
	if got := len(ops.cutoverCalls()); got != 2 {
		t.Fatalf("%d cutover calls, want the refused one tried again", got)
	}
	applied := logs.withMsg(msgFlipApplied)
	if len(applied) != 1 {
		t.Fatalf("%d flip applied records, want 1", len(applied))
	}
	want := map[string]any{
		"kind":            flipKindCutover,
		"cluster_id":      float64(testCid),
		"sp_id":           float64(testSpId),
		"migr_name":       spMigrName,
		"migr_id":         float64(spMigrId),
		"src_side_id":     float64(spSideSrc),
		"dst_side_id":     float64(spSideDst),
		"cntlr_id":        float64(spCntlrPrimary),
		"report_revision": float64(testSpRev + 1),
		"revision":        float64(committed),
	}
	for key, value := range want {
		if applied[0][key] != value {
			t.Errorf("flip applied %s = %v, want %v", key, applied[0][key],
				value)
		}
	}
	if len(w.cutovers) != 0 {
		t.Fatalf("the started cutover is still listed: %+v", w.cutovers)
	}
	report(testSpRev + 2)
	if got := len(ops.cutoverCalls()); got != 2 {
		t.Fatalf("%d cutover calls after it started, want 2", got)
	}
}

// TestCntlrPathReportIsTheReplysOwn pins where a cntlr child reads the path
// rows the cutover acts on: the reply's own info (ownInfo), never the latest
// known state. A round's reply that leaves its info out as unchanged repeats
// what its stream last carried, even when a Syncup*'s info with a live path,
// from before a failover, say, is the latest known; a Syncup* reply that
// carried no info reports no path at all. A standby reports none, and nor
// does a disabled primary, whose agent runs the standby shape (cnagent.md
// CN9).
func TestCntlrPathReportIsTheReplysOwn(t *testing.T) {
	ctx := context.Background()
	setupPlan := func(t *testing.T, plan *cntlrPlan) (*cntlrDriver,
		*spWorker) {
		t.Helper()
		captureLogs(t)
		d := newTestDeps(
			testConfig(common.WorkerRoleSp), newFakeStore(), newFakeClock(),
		)
		d.health = &fakeHealthWriter{}
		w := spTestWorker(d)
		w.verdicts = newVerdictMemo()
		return newCntlrDriver(w, nil, plan), w
	}
	setup := func(t *testing.T, primary bool) (*cntlrDriver, *spWorker) {
		t.Helper()
		return setupPlan(t, settlePlan(primary, false))
	}
	// paths observes one reply and returns the path report the child sent,
	// nil when it sent none or one without paths.
	paths := func(t *testing.T, driver *cntlrDriver, w *spWorker,
		r *replyState) *pathReport {
		t.Helper()
		driver.observe(ctx, r)
		select {
		case rep := <-w.reportCh:
			return rep.paths
		default:
			return nil
		}
	}

	t.Run("a round's own info", func(t *testing.T) {
		driver, w := setup(t, true)
		info := pathInfo(spSideDst)
		driver.storeInfo(info, 0)
		driver.setStreamInfo(info)
		got := paths(t, driver, w, &replyState{
			revision: 7, infoPresent: true, fromRound: true,
		})
		if !got.live(spSideDst) || got.revision != 7 {
			t.Fatalf("path report = %+v, want the dst side live at 7", got)
		}
	})

	t.Run("a round repeating its stream after a Syncup", func(t *testing.T) {
		driver, w := setup(t, true)
		// The stream carried no live path; a Syncup*'s info since — taken
		// before a failover, say — holds one, and is the latest known.
		driver.setStreamInfo(pathInfo())
		stale := pathInfo(spSideDst)
		driver.storeInfo(stale, 0)
		driver.syncupInfo = stale
		got := paths(t, driver, w, &replyState{revision: 7, fromRound: true})
		if got == nil || got.live(spSideDst) {
			t.Fatalf("path report = %+v, want the stream's, with no live "+
				"path", got)
		}
	})

	t.Run("a Syncup that carried no info", func(t *testing.T) {
		driver, w := setup(t, true)
		info := pathInfo(spSideDst)
		driver.storeInfo(info, 0)
		driver.setStreamInfo(info)
		driver.syncupInfo = nil
		if got := paths(t, driver, w, &replyState{revision: 7}); got != nil {
			t.Fatalf("path report = %+v, want none", got)
		}
	})

	t.Run("a Syncup's own info", func(t *testing.T) {
		driver, w := setup(t, true)
		driver.setStreamInfo(pathInfo())
		info := pathInfo(spSideDst)
		driver.storeInfo(info, 0)
		driver.syncupInfo = info
		got := paths(t, driver, w, &replyState{revision: 7, infoPresent: true})
		if !got.live(spSideDst) {
			t.Fatalf("path report = %+v, want the dst side live", got)
		}
	})

	t.Run("a standby", func(t *testing.T) {
		driver, w := setup(t, false)
		info := pathInfo(spSideDst)
		driver.storeInfo(info, 0)
		driver.setStreamInfo(info)
		got := paths(t, driver, w, &replyState{
			revision: 7, infoPresent: true, fromRound: true,
		})
		if got != nil {
			t.Fatalf("a standby sent path report %+v", got)
		}
	})

	t.Run("a disabled primary", func(t *testing.T) {
		plan := settlePlan(true, false)
		plan.req.Cntlr = &pb.Cntlr{Primary: true, Disabled: true}
		driver, w := setupPlan(t, plan)
		info := pathInfo(spSideDst)
		driver.storeInfo(info, 0)
		driver.setStreamInfo(info)
		got := paths(t, driver, w, &replyState{
			revision: 7, infoPresent: true, fromRound: true,
		})
		if got != nil {
			t.Fatalf("a disabled primary sent path report %+v", got)
		}
	})
}

// TestSideAwaitsCutover pins condition (2) on one side child: the target row
// PROVISIONING with the reason, read from the latest SideInfo at each reply,
// and cleared when the stream dies and the rows read UNKNOWN.
func TestSideAwaitsCutover(t *testing.T) {
	ctx := context.Background()
	captureLogs(t)
	d := newTestDeps(
		testConfig(common.WorkerRoleSp), newFakeStore(), newFakeClock(),
	)
	d.health = &fakeHealthWriter{}
	w := spTestWorker(d)
	w.verdicts = newVerdictMemo()
	plan := spTestWorker(nil).buildPlan(ctx, cutoverFixture(true)).
		sides[spDstKey]
	driver := newSideDriver(w, nil, plan)
	reply := &replyState{revision: testSpRev, infoPresent: true}

	noReason := awaitingSideInfo()
	noReason.MigrDstInfo.TargetInfo.Reason = pb.ResReason_RES_REASON_NONE
	ok := awaitingSideInfo()
	ok.MigrDstInfo.TargetInfo.Status = pb.ResStatus_RES_STATUS_OK
	for _, c := range []struct {
		name string
		info *pb.SideInfo
		want bool
	}{
		{"the row with the reason", awaitingSideInfo(), true},
		{"PROVISIONING with no reason", noReason, false},
		{"the reason on a row no longer PROVISIONING", ok, false},
		{"no migration row", &pb.SideInfo{}, false},
	} {
		driver.storeInfo(c.info)
		driver.observe(ctx, reply)
		if got := driver.awaitsCutover(); got != c.want {
			t.Errorf("%s: awaitsCutover = %v, want %v", c.name, got, c.want)
		}
	}
	driver.storeInfo(awaitingSideInfo())
	driver.observe(ctx, reply)
	driver.unreachable(ctx)
	if driver.awaitsCutover() {
		t.Fatalf("a side whose stream died still waits for the cutover")
	}
}

// cutoverRounds counts the Check rounds each stub answered, per endpoint.
type cutoverRounds struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *cutoverRounds) add(addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = make(map[string]int)
	}
	c.counts[addr]++
}

// atLeast reports whether every listed endpoint answered n rounds.
func (c *cutoverRounds) atLeast(n int, addrs ...string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, addr := range addrs {
		if c.counts[addr] < n {
			return false
		}
	}
	return true
}

// cutoverAgents answers the fixture's agents for the cutover tests: the dst
// side (dn2) with side, each cntlr with its path rows, on Syncups and on
// rounds at the driven revision alike. A cntlr in roundPaths answers its
// rounds with those rows instead; with nil, it answers them with no info and
// no revision, as an agent that holds none, so each is followed by a
// SyncupCntlr.
type cutoverAgents struct {
	mu         sync.Mutex
	side       *pb.SideInfo
	paths      map[string]*pb.CntlrInfo
	roundPaths map[string]*pb.CntlrInfo
	rounds     cutoverRounds
}

func (a *cutoverAgents) setSide(info *pb.SideInfo) {
	a.mu.Lock()
	a.side = info
	a.mu.Unlock()
}

func (a *cutoverAgents) setPaths(addr string, info *pb.CntlrInfo) {
	a.mu.Lock()
	a.paths[addr] = info
	a.mu.Unlock()
}

func (a *cutoverAgents) sideInfo() *pb.SideInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return proto.Clone(a.side).(*pb.SideInfo)
}

func (a *cutoverAgents) cntlrInfo(addr string) *pb.CntlrInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return proto.Clone(a.paths[addr]).(*pb.CntlrInfo)
}

func (a *cutoverAgents) setRoundPaths(addr string, info *pb.CntlrInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.roundPaths == nil {
		a.roundPaths = make(map[string]*pb.CntlrInfo)
	}
	a.roundPaths[addr] = info
}

// roundReply is a cntlr's answer to one round at revision.
func (a *cutoverAgents) roundReply(
	addr string,
	revision uint64,
) *pb.CheckCntlrReply {
	a.mu.Lock()
	info, ok := a.roundPaths[addr]
	a.mu.Unlock()
	if !ok {
		return &pb.CheckCntlrReply{
			Revision: revision, CntlrInfo: a.cntlrInfo(addr),
		}
	}
	if info == nil {
		return &pb.CheckCntlrReply{}
	}
	return &pb.CheckCntlrReply{
		Revision: revision, CntlrInfo: proto.Clone(info).(*pb.CntlrInfo),
	}
}

// serveCutover wires the stubs of h to a.
func serveCutover(h *spHarness, a *cutoverAgents) {
	dst := h.sides[spDnC]
	dst.syncupReply = func(req *pb.SyncupSideRequest) *pb.SyncupSideReply {
		return &pb.SyncupSideReply{
			Revision: req.GetRevision(), SideInfo: a.sideInfo(),
		}
	}
	dst.checkReply = func(req *pb.CheckSideRequest) *pb.CheckSideReply {
		a.rounds.add(spDnC)
		return &pb.CheckSideReply{
			Revision: req.GetRevision(), SideInfo: a.sideInfo(),
		}
	}
	for addr, stub := range h.cntlrs {
		stub.syncupReply = func(
			req *pb.SyncupCntlrRequest,
		) *pb.SyncupCntlrReply {
			return &pb.SyncupCntlrReply{
				Revision: req.GetRevision(), CntlrInfo: a.cntlrInfo(addr),
			}
		}
		stub.checkReply = func(
			req *pb.CheckCntlrRequest,
		) *pb.CheckCntlrReply {
			a.rounds.add(addr)
			return a.roundReply(addr, req.GetRevision())
		}
	}
}

// newCutoverAgents answers with the dst side waiting and no live path.
func newCutoverAgents() *cutoverAgents {
	return &cutoverAgents{
		side: awaitingSideInfo(),
		paths: map[string]*pb.CntlrInfo{
			spCnA: pathInfo(), spCnB: pathInfo(), spCnC: pathInfo(),
		},
	}
}

// TestSpCutoverRunsOnThePrimarysPath is the cutover end to end: the dst side
// reports that it waits, the primary reports a live path to it, and the
// coordinator starts the cutover once, for the primary, with the record
// naming the reply's revision. Either channel's own info counts: a
// SyncupCntlr reply's, its rounds carrying no info at all, and a round's,
// its SyncupCntlr reply showing no live path.
func TestSpCutoverRunsOnThePrimarysPath(t *testing.T) {
	t.Run("from the SyncupCntlr reply", inBubble(func(t *testing.T) {
		testSpCutoverRunsOnThePrimarysPath(t, false)
	}))
	t.Run("from a round", inBubble(func(t *testing.T) {
		testSpCutoverRunsOnThePrimarysPath(t, true)
	}))
}

func testSpCutoverRunsOnThePrimarysPath(t *testing.T, fromRound bool) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	h.ops.setState(cutoverFixture(true))
	h.ops.cutoverRev = testSpRev + 1
	h.store.seed(t, model.SpRevKey(testShard, testCid, testSpId),
		&pb.SpRev{Revision: testSpRev + 1, SpName: testSpName})
	agents := newCutoverAgents()
	if fromRound {
		agents.setPaths(spCnA, pathInfo(spSideSrc))
		agents.setRoundPaths(spCnA, pathInfo(spSideSrc, spSideDst))
	} else {
		agents.setPaths(spCnA, pathInfo(spSideSrc, spSideDst))
		agents.setRoundPaths(spCnA, nil)
	}
	serveCutover(h, agents)
	h.start()

	h.advanceUntil("the cutover and two more rounds", roundInterval,
		func() bool {
			return len(h.ops.cutoverCalls()) > 0 &&
				agents.rounds.atLeast(3, spCnA, spDnC)
		})
	calls := h.ops.cutoverCalls()
	want := cutoverCall{spName: testSpName, ref: spCutoverRef,
		cntlrId: spCntlrPrimary}
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("cutover calls = %+v, want one %+v", calls, want)
	}
	applied := h.logs.withMsg(msgFlipApplied)
	if len(applied) != 1 || applied[0]["kind"] != flipKindCutover {
		t.Fatalf("flip applied records = %v, want one cutover", applied)
	}
	if got := applied[0]["report_revision"]; got != float64(testSpRev) {
		t.Fatalf("report_revision = %v, want %d", got, testSpRev)
	}
	if got := applied[0]["revision"]; got != float64(testSpRev+1) {
		t.Fatalf("revision = %v, want %d", got, testSpRev+1)
	}
}

// TestSpCutoverIgnoresAStandbysPath: a standby's live path to the dst side
// starts nothing while the primary reports none.
func TestSpCutoverIgnoresAStandbysPath(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSpHarness(t)
		h.addFixtureAgents()
		h.ops.setState(cutoverFixture(true))
		agents := newCutoverAgents()
		agents.setPaths(spCnA, pathInfo(spSideSrc))
		agents.setPaths(spCnB, pathInfo(spSideSrc, spSideDst))
		serveCutover(h, agents)
		h.start()

		h.advanceUntil("three rounds of every cntlr and the dst side",
			roundInterval, func() bool {
				return agents.rounds.atLeast(3, spCnA, spCnB, spCnC, spDnC)
			})
		if calls := h.ops.cutoverCalls(); len(calls) != 0 {
			t.Fatalf("cutover started on a standby's path: %+v", calls)
		}
	})
}

// TestSpCutoverWaitsForTheSidesReason: the primary's live path starts
// nothing until the dst side reports the reason; a PROVISIONING row with no
// reason, or the row's text alone, is no wait for the cutover (cnagent.md
// CN29). Once the side reports it, the primary's next report starts it.
func TestSpCutoverWaitsForTheSidesReason(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSpHarness(t)
		h.addFixtureAgents()
		h.ops.setState(cutoverFixture(true))
		agents := newCutoverAgents()
		noReason := awaitingSideInfo()
		noReason.MigrDstInfo.TargetInfo.Reason = pb.ResReason_RES_REASON_NONE
		agents.setSide(noReason)
		agents.setPaths(spCnA, pathInfo(spSideSrc, spSideDst))
		serveCutover(h, agents)
		h.start()

		h.advanceUntil("three rounds of the primary and the dst side",
			roundInterval, func() bool {
				return agents.rounds.atLeast(3, spCnA, spDnC)
			})
		if calls := h.ops.cutoverCalls(); len(calls) != 0 {
			t.Fatalf("cutover started without the side's reason: %+v", calls)
		}
		agents.setSide(awaitingSideInfo())
		h.advanceUntil("the cutover", roundInterval, func() bool {
			return len(h.ops.cutoverCalls()) > 0
		})
		want := cutoverCall{spName: testSpName, ref: spCutoverRef,
			cntlrId: spCntlrPrimary}
		if calls := h.ops.cutoverCalls(); calls[0] != want {
			t.Fatalf("cutover call = %+v, want %+v", calls[0], want)
		}
	})
}

// TestSpCutoverWaitsForTheProvisionedFlip: a dst side the load shows still
// zeroing gets no cutover, whatever the reports say; the fan-out of the load
// that shows it provisioned lists it, and the next reports start it.
func TestSpCutoverWaitsForTheProvisionedFlip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSpHarness(t)
		h.addFixtureAgents()
		h.ops.setState(cutoverFixture(false))
		agents := newCutoverAgents()
		agents.setPaths(spCnA, pathInfo(spSideSrc, spSideDst))
		serveCutover(h, agents)
		w := h.start()

		h.advanceUntil("three rounds of the primary and the dst side",
			roundInterval, func() bool {
				return agents.rounds.atLeast(3, spCnA, spDnC)
			})
		if calls := h.ops.cutoverCalls(); len(calls) != 0 {
			t.Fatalf("cutover started before the provisioned flip: %+v",
				calls)
		}
		next := cutoverFixture(true)
		next.SpRevision = testSpRev + 1
		h.ops.setState(next)
		w.update(desiredState{revision: testSpRev + 1, handle: testSpName})
		h.advanceUntil("the cutover", roundInterval, func() bool {
			return len(h.ops.cutoverCalls()) > 0
		})
	})
}

// TestSpCutoverWaitsForTheNewPrimary: only the primary's path counts, so a
// cutover the old primary's reports never allowed waits through a failover
// for the new primary's own report, and starts for it.
func TestSpCutoverWaitsForTheNewPrimary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSpHarness(t)
		h.addFixtureAgents()
		h.ops.setState(cutoverFixture(true))
		agents := newCutoverAgents()
		agents.setPaths(spCnA, pathInfo(spSideSrc))
		agents.setPaths(spCnB, pathInfo(spSideSrc, spSideDst))
		serveCutover(h, agents)
		w := h.start()

		h.advanceUntil("three rounds of both cntlrs and the dst side",
			roundInterval, func() bool {
				return agents.rounds.atLeast(3, spCnA, spCnB, spDnC)
			})
		if calls := h.ops.cutoverCalls(); len(calls) != 0 {
			t.Fatalf("cutover started before the failover: %+v", calls)
		}
		bump := h.ops.commitFailover(spCntlrPrimary, spCntlrStandby)
		w.update(desiredState{revision: bump, handle: testSpName})
		h.advanceUntil("the cutover", roundInterval, func() bool {
			return len(h.ops.cutoverCalls()) > 0
		})
		want := cutoverCall{spName: testSpName, ref: spCutoverRef,
			cntlrId: spCntlrStandby}
		for _, call := range h.ops.cutoverCalls() {
			if call != want {
				t.Fatalf("cutover call = %+v, want %+v", call, want)
			}
		}
	})
}
