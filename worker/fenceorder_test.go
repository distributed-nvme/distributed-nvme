package worker

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// fenceInfo is a primary's report whose reading of the fixture's data group
// 101 holds legs Migr and D in sync, last in slot 0 and the other in slot 1,
// and whose thin info completes the open td, so the coordinator runs the
// created flip in the drain that takes the report's views (RW19, RW24): the
// tests wait for that flip before they go on.
func fenceInfo(last, other uint64) *pb.CntlrInfo {
	inSync := []uint64{last, other}
	if other < last {
		inSync = []uint64{other, last}
	}
	info := thinInfoOnly()
	info.GrpIdToGrpInfo = map[uint64]*pb.CntlrInfo_GrpInfo{
		101: {
			Whole:             true,
			InSyncLegIdList:   inSync,
			InSyncLegIdToSlot: map[uint64]uint32{last: 0, other: 1},
		},
	}
	return info
}

// thinInfoOnly is a primary's report that completes the open td and reads no
// group: an array whose row did not read OK gets no reading (cnagent.md
// CN28).
func thinInfoOnly() *pb.CntlrInfo {
	ok := &pb.ResInfo{Status: pb.ResStatus_RES_STATUS_OK}
	return &pb.CntlrInfo{
		TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
			spTdOpen: {SliceIdToDmThin: map[uint64]*pb.ResInfo{
				spSliceA: ok,
				spSliceB: ok,
			}},
		},
	}
}

// fenceHarness drives the fixture SP whose primary reports, at each revision
// infoAt names, that info in its SyncupCntlr reply — by default leg Migr as
// the fence-last leg of group 101 (both its sides, on DNs B and C) at N —
// and answers every other syncup at once, but for the sides holds names: a
// side held from a revision holds every syncup at that revision or above
// until that side's gate at that revision is released.
type fenceHarness struct {
	*spHarness
	w     *spWorker
	holds map[uint64]uint64
	mu    sync.Mutex
	seq   int64
	gates map[gateKey]chan struct{}
	// sideIn and cntlrIn are each side's and each cntlr's first arrival at
	// each revision past N, by one counter.
	sideIn  map[uint64]map[uint64]int64
	cntlrIn map[uint64]map[uint64]int64
}

// gateKey is one side's gate at one revision.
type gateKey struct {
	sideId   uint64
	revision uint64
}

func newFenceHarness(
	t *testing.T,
	reporter string,
	holds map[uint64]uint64,
	infoAt map[uint64]*pb.CntlrInfo,
) *fenceHarness {
	return newFenceHarnessWith(t, reporter, holds, infoAt, nil)
}

// newFenceHarnessWith is newFenceHarness whose fixture prep changes before
// the coordinator's first load.
func newFenceHarnessWith(
	t *testing.T,
	reporter string,
	holds map[uint64]uint64,
	infoAt map[uint64]*pb.CntlrInfo,
	prep func(state *model.SpState),
) *fenceHarness {
	h := &fenceHarness{
		spHarness: newSpHarness(t),
		holds:     holds,
		gates:     make(map[gateKey]chan struct{}),
		sideIn:    make(map[uint64]map[uint64]int64),
		cntlrIn:   make(map[uint64]map[uint64]int64),
	}
	if infoAt == nil {
		infoAt = map[uint64]*pb.CntlrInfo{testSpRev: fenceInfo(spLegMigr, spLegD)}
	}
	h.addFixtureAgents()
	if prep != nil {
		state := spFixture()
		prep(state)
		h.ops.setState(state)
	}
	for _, stub := range h.sides {
		stub.syncupReply = func(req *pb.SyncupSideRequest) *pb.SyncupSideReply {
			rev := req.GetRevision()
			sideId := req.GetSidePointer().GetSideId()
			if rev > testSpRev {
				h.note(h.sideIn, sideId, rev)
				if from, ok := h.holds[sideId]; ok && rev >= from {
					<-h.gate(sideId, rev)
				}
			}
			return &pb.SyncupSideReply{Revision: rev}
		}
	}
	for addr, stub := range h.cntlrs {
		reports := addr == reporter
		stub.syncupReply = func(req *pb.SyncupCntlrRequest) *pb.SyncupCntlrReply {
			rev := req.GetRevision()
			if rev > testSpRev {
				h.note(h.cntlrIn, req.GetCntlrPointer().GetCntlrId(), rev)
			}
			if info, ok := infoAt[rev]; ok && reports {
				return &pb.SyncupCntlrReply{Revision: rev, CntlrInfo: info}
			}
			return &pb.SyncupCntlrReply{Revision: rev}
		}
	}
	h.w = h.start()
	// Registered after start, so it runs before the coordinator's stop,
	// which would otherwise wait for a held SyncupSide to time out.
	t.Cleanup(h.releaseAll)
	// A pass runs on the coordinator's tick once the clock moves.
	h.reactWith(h.w, &fakeReactionOps{})
	h.awaitFlips(1)
	return h
}

// awaitFlips waits until the coordinator has run n created flips: the last
// report that carries views has been taken by then.
func (h *fenceHarness) awaitFlips(n int) {
	h.t.Helper()
	waitFor(h.t, "the report's drain ran the created flip", func() bool {
		return len(h.ops.createdCalls()) >= n
	})
}

// gate is the gate a held side's syncups at revision wait on.
func (h *fenceHarness) gate(sideId, revision uint64) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := gateKey{sideId, revision}
	g, ok := h.gates[key]
	if !ok {
		g = make(chan struct{})
		h.gates[key] = g
	}
	return g
}

// release lets a held side answer its syncups at revision.
func (h *fenceHarness) release(sideId, revision uint64) {
	g := h.gate(sideId, revision)
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-g:
	default:
		close(g)
	}
}

func (h *fenceHarness) releaseAll() {
	for sideId := range h.holds {
		for _, revision := range []uint64{testSpRev + 1, testSpRev + 2, testSpRev + 3} {
			h.release(sideId, revision)
		}
	}
}

// heldD holds leg D's side, of the fence-last leg's group, from revision on.
func heldD(revision uint64) map[uint64]uint64 {
	return map[uint64]uint64{spSideD: revision}
}

func (h *fenceHarness) note(byId map[uint64]map[uint64]int64, id, revision uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	if byId[id] == nil {
		byId[id] = make(map[uint64]int64)
	}
	if _, seen := byId[id][revision]; !seen {
		byId[id][revision] = h.seq
	}
}

// sent is the first arrival of id at revision, and whether there is one.
func (h *fenceHarness) sent(
	byId map[uint64]map[uint64]int64, id, revision uint64,
) (int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	at, ok := byId[id][revision]
	return at, ok
}

// sentAny reports whether id has been sent any revision past N.
func (h *fenceHarness) sentAny(byId map[uint64]map[uint64]int64, id uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(byId[id]) > 0
}

// failOver fails the primary over at revision and waits until the sides of
// the fence round have the failover.
func (h *fenceHarness) failOver(revision uint64) {
	h.t.Helper()
	h.ops.setState(failoverFixture(revision))
	h.w.update(desiredState{revision: revision, handle: testSpName})
	h.awaitRoundSides(revision)
}

// awaitRoundSides waits until the fence round's sides — every side but the
// fence-last leg Migr's — have been sent revision.
func (h *fenceHarness) awaitRoundSides(revision uint64) {
	h.t.Helper()
	waitFor(h.t, "the fence round's sides sent the failover", func() bool {
		for _, sideId := range []uint64{spSideMeta, spSideB, spSideD} {
			if _, ok := h.sent(h.sideIn, sideId, revision); !ok {
				return false
			}
		}
		return true
	})
}

// settleNoLast waits a little real time and fails when a fence-last side has
// been sent a revision of from or above: an early release needs real time to
// reach a side.
func (h *fenceHarness) settleNoLast(from uint64, what string) {
	h.t.Helper()
	settle := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(settle) {
		time.Sleep(10 * time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, sideId := range []uint64{spSideSrc, spSideDst} {
		for revision := range h.sideIn[sideId] {
			if revision >= from {
				h.t.Fatalf("fence-last side %d was sent %d %s", sideId,
					revision, what)
			}
		}
	}
}

// awaitLast waits until both fence-last sides have been sent revision.
func (h *fenceHarness) awaitLast(revision uint64) {
	h.t.Helper()
	waitFor(h.t, "the fence-last sides sent the failover", func() bool {
		_, src := h.sent(h.sideIn, spSideSrc, revision)
		_, dst := h.sent(h.sideIn, spSideDst, revision)
		return src && dst
	})
}

// TestFailoverFencesTheLastLegLast checks RW24: when the demotion hold ends,
// every side but the fence-last leg's is handed the failover, and the
// fence-last leg's two sides, those of the migrating leg md holds in its
// lowest slot, wait until the other sides have reported the revision
// applied; the sides-first hold then keeps the other cntlrs back until the
// fence-last sides have reported too. The fake clock never moves, so only
// the side of leg D's reply ends the round, and only the fence-last source
// side's reply ends the sides-first hold.
func TestFailoverFencesTheLastLegLast(t *testing.T) {
	const bump = testSpRev + 1
	h := newFenceHarness(t, spCnA,
		map[uint64]uint64{spSideD: bump, spSideSrc: bump}, nil)
	h.failOver(bump)
	h.settleNoLast(bump, "before leg D's side reported the fence")

	h.release(spSideD, bump)
	h.awaitLast(bump)
	settle := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(settle) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := h.sent(h.cntlrIn, spCntlrStandby, bump); ok {
		t.Fatalf("the new primary was sent the failover before the " +
			"fence-last source side reported it")
	}
	h.release(spSideSrc, bump)
	waitFor(t, "the new primary sent the failover", func() bool {
		_, ok := h.sent(h.cntlrIn, spCntlrStandby, bump)
		return ok
	})
	for _, last := range []uint64{spSideSrc, spSideDst} {
		at, _ := h.sent(h.sideIn, last, bump)
		for _, other := range []uint64{spSideMeta, spSideB, spSideD} {
			if first, _ := h.sent(h.sideIn, other, bump); first > at {
				t.Fatalf("fence-last side %d (#%d) was sent the failover "+
					"before side %d (#%d)", last, at, other, first)
			}
		}
		if cntlr, _ := h.sent(h.cntlrIn, spCntlrStandby, bump); cntlr < at {
			t.Fatalf("the new primary (#%d) was sent the failover before "+
				"fence-last side %d (#%d)", cntlr, last, at)
		}
	}
	recs := h.logs.withMsg(msgSpFenceRoundEnded)
	if len(recs) != 1 {
		t.Fatalf("%d %q records, want one", len(recs), msgSpFenceRoundEnded)
	}
	if n, _ := recs[0]["last_side_cnt"].(float64); n != 2 {
		t.Fatalf("last_side_cnt = %v, want 2", recs[0]["last_side_cnt"])
	}
	if n, _ := recs[0]["side_cnt"].(float64); n != 0 {
		t.Fatalf("side_cnt = %v, want 0: every side reported", recs[0]["side_cnt"])
	}
}

// TestFenceRoundIsBounded checks RW24's bound: a side of the fence round that
// never reports holds the fence-last legs' sides for common.FenceRoundIntervals
// cntlr_intervals after the demotion hold's end and no longer, one interval
// included, and the record counts the side that did not report and gives
// the wait.
func TestFenceRoundIsBounded(t *testing.T) {
	const bump = testSpRev + 1
	bound := common.FenceRoundIntervals * roundInterval
	h := newFenceHarness(t, spCnA, heldD(bump), nil)
	h.failOver(bump)

	h.clk.advance(roundInterval + 250*time.Millisecond)
	h.settleNoLast(bump, "one interval into the round")
	h.clk.advance(bound - roundInterval - 500*time.Millisecond)
	h.settleNoLast(bump, "before the round's bound")
	h.clk.advance(250 * time.Millisecond)
	h.awaitLast(bump)
	recs := h.logs.withMsg(msgSpFenceRoundEnded)
	if len(recs) != 1 {
		t.Fatalf("%d %q records, want one", len(recs), msgSpFenceRoundEnded)
	}
	if n, _ := recs[0]["side_cnt"].(float64); n != 1 {
		t.Fatalf("side_cnt = %v, want 1: leg D's side", recs[0]["side_cnt"])
	}
	if ms, _ := recs[0]["wait_ms"].(float64); int64(ms) != bound.Milliseconds() {
		t.Fatalf("wait_ms = %v, want %d", recs[0]["wait_ms"],
			bound.Milliseconds())
	}
}

// silenceD makes leg D's disk node answer no check round from now on.
func (h *fenceHarness) silenceD() {
	stub := h.sides[spDnD]
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.checkReply = func(*pb.CheckSideRequest) *pb.CheckSideReply {
		return nil
	}
}

// awaitRoundFailed advances the fake clock one interval at a time, at most
// limit times, until leg D's side child has logged a check round with no
// answer, and reports whether it did.
func (h *fenceHarness) awaitRoundFailed(limit int) bool {
	h.t.Helper()
	failed := func() bool {
		for _, rec := range h.logs.withMsg("check round failed") {
			if id, _ := rec["dn_id"].(float64); uint64(id) == spDnIdD {
				return true
			}
		}
		return false
	}
	for range limit {
		h.clk.advance(roundInterval)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if failed() {
				return true
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	return failed()
}

// TestFenceRoundSkipsASideWithNoAnswer checks RW24's rule for a side whose
// latest check round got no answer: leg D's disk node stops answering
// before the failover, so the round does not wait for its side, though its
// syncup of the failover is held: the fence-last sides go with no move of
// the clock, and the record counts the side as not waited for.
func TestFenceRoundSkipsASideWithNoAnswer(t *testing.T) {
	const bump = testSpRev + 1
	h := newFenceHarness(t, spCnA, heldD(bump), nil)
	h.silenceD()
	if !h.awaitRoundFailed(3) {
		t.Fatalf("leg D's side child logged no check round with no answer")
	}
	h.failOver(bump)
	h.awaitLast(bump)
	recs := h.logs.withMsg(msgSpFenceRoundEnded)
	if len(recs) != 1 {
		t.Fatalf("%d %q records, want one", len(recs), msgSpFenceRoundEnded)
	}
	if n, _ := recs[0]["side_cnt"].(float64); n != 0 {
		t.Fatalf("side_cnt = %v, want 0", recs[0]["side_cnt"])
	}
	if n, _ := recs[0]["unanswered_cnt"].(float64); n != 1 {
		t.Fatalf("unanswered_cnt = %v, want 1: leg D's side",
			recs[0]["unanswered_cnt"])
	}
}

// TestFenceRoundEndsWhenASideGoesQuiet checks RW24's rule during a round:
// leg D's side answers its syncup of the failover without applying it, so
// the round waits for it; once its disk node stops answering and its child
// logs a check round with no answer, the round ends with that report, well
// before its bound.
func TestFenceRoundEndsWhenASideGoesQuiet(t *testing.T) {
	const bump = testSpRev + 1
	h := newFenceHarness(t, spCnA, nil, nil)
	stub := h.sides[spDnD]
	stub.mu.Lock()
	stub.syncupReply = func(req *pb.SyncupSideRequest) *pb.SyncupSideReply {
		if req.GetRevision() > testSpRev {
			h.note(h.sideIn, req.GetSidePointer().GetSideId(),
				req.GetRevision())
		}
		return &pb.SyncupSideReply{Revision: testSpRev}
	}
	stub.mu.Unlock()
	h.failOver(bump)
	h.settleNoLast(bump, "while leg D's side answered without the fence")
	h.silenceD()
	if !h.awaitRoundFailed(common.FenceRoundIntervals - 2) {
		t.Fatalf("leg D's side child logged no check round with no answer")
	}
	h.awaitLast(bump)
	recs := h.logs.withMsg(msgSpFenceRoundEnded)
	if len(recs) != 1 {
		t.Fatalf("%d %q records, want one", len(recs), msgSpFenceRoundEnded)
	}
	bound := common.FenceRoundIntervals * roundInterval
	if ms, _ := recs[0]["wait_ms"].(float64); int64(ms) >= bound.Milliseconds() {
		t.Fatalf("wait_ms = %v: the round ended by its bound %d",
			recs[0]["wait_ms"], bound.Milliseconds())
	}
}

// TestANewCoordinatorFencesTheStoredLegLast checks RW24's record: group 101's
// record names leg Migr as its fence-last leg and the primary's report reads
// no group, as after a worker restart before the primary's next report, so
// the coordinator's view comes from the record, and the failover fences leg
// Migr's sides last, after leg D's side has reported.
func TestANewCoordinatorFencesTheStoredLegLast(t *testing.T) {
	const bump = testSpRev + 1
	h := newFenceHarnessWith(t, spCnA, heldD(bump),
		map[uint64]*pb.CntlrInfo{testSpRev: thinInfoOnly()},
		func(state *model.SpState) {
			state.Slices[spSliceA].DataGrpList[0].FenceLastLegId = spLegMigr
		})
	h.failOver(bump)
	h.settleNoLast(bump, "before leg D's side reported the fence")
	h.release(spSideD, bump)
	h.awaitLast(bump)
	recs := h.logs.withMsg(msgSpFenceRoundEnded)
	if len(recs) != 1 {
		t.Fatalf("%d %q records, want one", len(recs), msgSpFenceRoundEnded)
	}
}

// TestAReadingChangeWritesTheFenceLastLeg checks RW24's write of the record:
// the primary's first reading of group 101 writes leg Migr once, a report
// with the same reading writes nothing, and a reading that names no
// fence-last leg writes zero.
func TestAReadingChangeWritesTheFenceLastLeg(t *testing.T) {
	const (
		bump  = testSpRev + 1
		bump2 = testSpRev + 2
	)
	noSlot := thinInfoOnly()
	noSlot.GrpIdToGrpInfo = map[uint64]*pb.CntlrInfo_GrpInfo{
		101: {Whole: true, InSyncLegIdList: []uint64{spLegMigr, spLegD}},
	}
	h := newFenceHarness(t, spCnA, nil, map[uint64]*pb.CntlrInfo{
		testSpRev: fenceInfo(spLegMigr, spLegD),
		bump:      fenceInfo(spLegMigr, spLegD),
		bump2:     noSlot,
	})
	ref := model.GrpRef{Parent: model.GrpParent{SliceId: spSliceA}, GrpId: 101}
	want := [][]model.FenceLastEdit{{{Ref: ref, LegId: spLegMigr}}}
	if got := h.ops.fenceLastCalls(); !slices.EqualFunc(got, want,
		slices.Equal) {
		t.Fatalf("after the first reading: writes %v, want %v", got, want)
	}
	for i, revision := range []uint64{bump, bump2} {
		// The load reads the record the first write wrote.
		next := spFixture()
		next.Slices[spSliceA].DataGrpList[0].FenceLastLegId = spLegMigr
		next.SpRevision = revision
		h.ops.setState(next)
		h.w.update(desiredState{revision: revision, handle: testSpName})
		h.awaitFlips(2 + i)
	}
	want = append(want, []model.FenceLastEdit{{Ref: ref}})
	waitFor(t, "the zero write", func() bool {
		return len(h.ops.fenceLastCalls()) >= len(want)
	})
	if got := h.ops.fenceLastCalls(); !slices.EqualFunc(got, want,
		slices.Equal) {
		t.Fatalf("writes %v, want %v", got, want)
	}
}

// TestSideNotesItsAnswers checks the side child's reports of RW24's mark: a
// check round with no answer reports the side quiet once, a second one
// reports nothing, and the next answer reports it answering again.
func TestSideNotesItsAnswers(t *testing.T) {
	ctx := context.Background()
	captureLogs(t)
	d := newTestDeps(
		testConfig(common.WorkerRoleSp), newFakeStore(), newFakeClock(),
	)
	d.health = &fakeHealthWriter{}
	w := spTestWorker(d)
	w.verdicts = newVerdictMemo()
	plan := spTestWorker(nil).buildPlan(ctx, spFixture()).
		sides[sideKey{legId: spLegD, sideId: spSideD}]
	driver := newSideDriver(w, nil, plan)
	answers := func() []bool {
		var out []bool
		for {
			select {
			case rep := <-w.reportCh:
				if rep.answer != nil {
					if rep.answer.driver != driver ||
						rep.answer.key.sideId != spSideD {
						t.Fatalf("an answer report of another side: %+v",
							rep.answer)
					}
					out = append(out, rep.answer.answered)
				}
			default:
				return out
			}
		}
	}
	driver.unreachable(ctx)
	driver.unreachable(ctx)
	if got := answers(); !slices.Equal(got, []bool{false}) {
		t.Fatalf("after two rounds with no answer: %v, want [false]", got)
	}
	driver.storeInfo(&pb.SideInfo{})
	driver.observe(ctx, &replyState{revision: testSpRev, infoPresent: true})
	if got := answers(); !slices.Equal(got, []bool{true}) {
		t.Fatalf("after an answer: %v, want [true]", got)
	}
	// A starting answer within the hold is an answer too: the node's kernel
	// may still serve the old primary.
	driver.unreachable(ctx)
	driver.starting(ctx)
	if got := answers(); !slices.Equal(got, []bool{false, true}) {
		t.Fatalf("a round with no answer, then a starting one: %v, want "+
			"[false true]", got)
	}
}

// TestStandbyReportNamesNoLastLeg checks RW24's gate: the views come only from
// a report of the cntlr the latest plan makes the enabled primary. Here only
// the standby reports a reading with slots, and the old primary none, so the
// failover runs no fence round: the would-be fence-last sides go with every
// other side, while leg D's side has not reported.
func TestStandbyReportNamesNoLastLeg(t *testing.T) {
	const bump = testSpRev + 1
	h := newFenceHarness(t, spCnB, heldD(bump), nil)
	h.failOver(bump)
	h.awaitLast(bump)
	if recs := h.logs.withMsg(msgSpFenceRoundEnded); len(recs) != 0 {
		t.Fatalf("%d %q records, want none", len(recs), msgSpFenceRoundEnded)
	}
}

// TestFanOutDuringFenceRoundKeepsLastHeld checks a fan-out of a newer
// revision inside RW24's round: the other sides are handed it at once, the
// fence-last sides stay held, and the round waits for the other sides at the
// newer revision, taking the sides it waits for from the newer load: leg D's
// side's report of the superseded revision does not end it, and leg B's
// side, which the first load found unhealthy and the newer one healthy, is
// waited for. The fence-last sides are first sent the newer revision, never
// the superseded one.
func TestFanOutDuringFenceRoundKeepsLastHeld(t *testing.T) {
	const (
		bump  = testSpRev + 1
		bump2 = testSpRev + 2
	)
	h := newFenceHarness(t, spCnA,
		map[uint64]uint64{spSideD: bump, spSideB: bump2}, nil)
	first := failoverFixture(bump)
	first.Slices[spSliceB].DataGrpList[0].LegList[0].SideList[0].ErrEpoch = 5
	h.ops.setState(first)
	h.w.update(desiredState{revision: bump, handle: testSpName})
	h.awaitRoundSides(bump)
	h.ops.setState(failoverFixture(bump2))
	h.w.update(desiredState{revision: bump2, handle: testSpName})
	waitFor(t, "DN A's sides sent the newer revision", func() bool {
		return sideSentAt(h.sides[spDnA], bump2)
	})

	// Leg D's side answers the superseded revision and is sent the newer
	// one, which it answers too: the round waits on for leg B's side.
	h.release(spSideD, bump)
	h.release(spSideD, bump2)
	waitFor(t, "leg D's side sent the newer revision", func() bool {
		_, ok := h.sent(h.sideIn, spSideD, bump2)
		return ok
	})
	h.settleNoLast(bump, "while leg B's side, healthy in the newer load, had not reported")

	h.release(spSideB, bump2)
	h.awaitLast(bump2)
	for _, sideId := range []uint64{spSideSrc, spSideDst} {
		if _, ok := h.sent(h.sideIn, sideId, bump); ok {
			t.Fatalf("fence-last side %d was sent the superseded revision %d",
				sideId, bump)
		}
	}
}

// TestUnreadGroupKeepsItsLastLeg checks RW24's per-group views: a later report
// of the primary that reads group 110 but not group 101, as one whose array's
// row read an error does not, leaves group 101's fence-last leg as the report
// before it named it, so the failover still fences leg Migr last.
func TestUnreadGroupKeepsItsLastLeg(t *testing.T) {
	const (
		bump  = testSpRev + 1
		bump2 = testSpRev + 2
	)
	readsB := thinInfoOnly()
	// Group 110's reading gives no slot, so it names no fence-last leg and
	// leg B's side stays in the round; the report still reads a group.
	readsB.GrpIdToGrpInfo = map[uint64]*pb.CntlrInfo_GrpInfo{
		110: {Whole: true, InSyncLegIdList: []uint64{spLegB}},
	}
	h := newFenceHarness(t, spCnA, heldD(bump2), map[uint64]*pb.CntlrInfo{
		testSpRev: fenceInfo(spLegMigr, spLegD),
		bump:      readsB,
	})
	// A bump that changes no role: the primary's reply at it reads group 110
	// and not group 101.
	next := spFixture()
	next.SpRevision = bump
	h.ops.setState(next)
	h.w.update(desiredState{revision: bump, handle: testSpName})
	h.awaitFlips(2)

	h.failOver(bump2)
	h.settleNoLast(bump2, "before leg D's side reported the fence")
	h.release(spSideD, bump2)
	h.awaitLast(bump2)
}

// TestFenceRoundWaitsForAnInSyncSideWithAnEpoch checks RW24's wait for a leg
// md holds in sync whatever its side's health record: leg D's side carries an
// err_epoch in the failover's load, which RW14's barrier would not wait for,
// but md holds leg D in sync by the primary's report, so the fence-last
// sides wait for its report.
func TestFenceRoundWaitsForAnInSyncSideWithAnEpoch(t *testing.T) {
	const bump = testSpRev + 1
	h := newFenceHarness(t, spCnA, heldD(bump), nil)
	next := failoverFixture(bump)
	next.Slices[spSliceA].DataGrpList[0].LegList[1].SideList[0].ErrEpoch = 5
	h.ops.setState(next)
	h.w.update(desiredState{revision: bump, handle: testSpName})
	h.awaitRoundSides(bump)
	h.settleNoLast(bump, "before leg D's side, unhealthy but in sync, reported")
	h.release(spSideD, bump)
	h.awaitLast(bump)
}

// TestFenceViewsOf checks RW24's reading of a report: per group, the in-sync
// leg of the lowest slot, a tie to the lower leg id, no fence-last leg for a
// group whose reading carries no slot, the in-sync legs, and nil for a reply
// that reads no group or carries no info.
func TestFenceViewsOf(t *testing.T) {
	if got := fenceViewsOf(nil); got != nil {
		t.Fatalf("fenceViewsOf(nil) = %v, want nil", got)
	}
	info := &pb.CntlrInfo{GrpIdToGrpInfo: map[uint64]*pb.CntlrInfo_GrpInfo{
		1: {InSyncLegIdList: []uint64{10, 11}, InSyncLegIdToSlot: map[uint64]uint32{10: 1, 11: 0}},
		2: {InSyncLegIdList: []uint64{20, 21}, InSyncLegIdToSlot: map[uint64]uint32{20: 3, 21: 3}},
		// A reading with no slots, as when md's slot of each member did not
		// read as a number.
		3: {InSyncLegIdList: []uint64{30, 31}},
		// A slot of a leg the list does not hold in sync counts for nothing.
		4: {InSyncLegIdList: []uint64{41}, InSyncLegIdToSlot: map[uint64]uint32{40: 0, 41: 1}},
	}}
	got := fenceViewsOf(info)
	want := map[uint64]uint64{1: 11, 2: 20, 3: 0, 4: 41}
	if len(got) != len(want) {
		t.Fatalf("fenceViewsOf = %v, want last legs %v", got, want)
	}
	for grpId, last := range want {
		if got[grpId].last != last {
			t.Fatalf("group %d: last leg %d, want %d", grpId, got[grpId].last, last)
		}
	}
	if inSync := got[3].inSync; len(inSync) != 2 || inSync[0] != 30 || inSync[1] != 31 {
		t.Fatalf("group 3: in sync %v, want [30 31]", inSync)
	}
	if got := fenceViewsOf(&pb.CntlrInfo{}); got != nil {
		t.Fatalf("fenceViewsOf(no reading) = %v, want nil: nothing to send", got)
	}
}

// TestACntlrRequestLeavesTheFenceLastLegOut checks RW16 with RW24: the group
// record's fence_last_leg_id, which the worker writes with no revision bump,
// is left out of every cntlr request, so a load at the same revision after
// that write keeps the cntlr children (keepChild), and the loaded state the
// requests are copied from keeps the field.
func TestACntlrRequestLeavesTheFenceLastLegOut(t *testing.T) {
	ctx := context.Background()
	captureLogs(t)
	state := spFixture()
	state.Slices[spSliceA].DataGrpList[0].FenceLastLegId = spLegMigr
	state.Clones[spCloneNm].MetaGrp = &pb.Group{GrpId: 777, FenceLastLegId: 7}
	plan := spTestWorker(nil).buildPlan(ctx, state)
	if len(plan.cntlrs) == 0 {
		t.Fatalf("fixture is wrong: the plan holds no cntlr")
	}
	for cntlrId, p := range plan.cntlrs {
		for _, slice := range p.req.GetIdToSlice() {
			for _, grp := range append(slices.Clone(slice.GetMetaGrpList()),
				slice.GetDataGrpList()...) {
				if grp.GetFenceLastLegId() != 0 {
					t.Errorf("cntlr %d's request carries group %d's "+
						"fence-last leg %d", cntlrId, grp.GetGrpId(),
						grp.GetFenceLastLegId())
				}
			}
		}
	}
	for cntlrId, p := range plan.cntlrs {
		for _, clone := range p.req.GetCloneList() {
			if got := clone.GetMetaGrp().GetFenceLastLegId(); got != 0 {
				t.Errorf("cntlr %d's request carries clone %d's fence-last "+
					"leg %d", cntlrId, clone.GetCloneId(), got)
			}
		}
	}
	if got := state.Slices[spSliceA].DataGrpList[0].GetFenceLastLegId(); got != spLegMigr {
		t.Errorf("the loaded state lost the field: %d", got)
	}
	if got := state.Clones[spCloneNm].GetMetaGrp().GetFenceLastLegId(); got != 7 {
		t.Errorf("the loaded clone lost the field: %d", got)
	}
	if got := plan.fenceGrps[101].stored; got != spLegMigr {
		t.Errorf("the plan's group 101 stores %d, want %d", got, spLegMigr)
	}
	bare := spFixture()
	bare.Clones[spCloneNm].MetaGrp = &pb.Group{GrpId: 777}
	bareReq := spTestWorker(nil).buildPlan(ctx, bare).cntlrs[spCntlrPrimary].req
	if !keepChild("a", "a", bareReq, plan.cntlrs[spCntlrPrimary].req) {
		t.Errorf("a load that differs only in the fence-last leg would " +
			"restart the cntlr child")
	}
}

// TestADroppedViewStaysDropped checks RW24's record against a view a report
// dropped: the primary's reading of group 101 names no fence-last leg, the
// zero write fails and is logged, and a later drain or load, while the
// record still holds leg Migr, seeds no view from it and writes the zero
// again; the failover after it runs no fence round.
func TestADroppedViewStaysDropped(t *testing.T) {
	const (
		bump  = testSpRev + 1
		bump2 = testSpRev + 2
		bump3 = testSpRev + 3
	)
	noSlot := thinInfoOnly()
	noSlot.GrpIdToGrpInfo = map[uint64]*pb.CntlrInfo_GrpInfo{
		101: {Whole: true, InSyncLegIdList: []uint64{spLegMigr, spLegD}},
	}
	stored := func(state *model.SpState) {
		state.Slices[spSliceA].DataGrpList[0].FenceLastLegId = spLegMigr
	}
	h := newFenceHarnessWith(t, spCnA, heldD(bump3), map[uint64]*pb.CntlrInfo{
		testSpRev: fenceInfo(spLegMigr, spLegD),
		bump:      noSlot,
		bump2:     thinInfoOnly(),
	}, stored)
	ref := model.GrpRef{Parent: model.GrpParent{SliceId: spSliceA}, GrpId: 101}
	if got := h.ops.fenceLastCalls(); len(got) != 0 {
		t.Fatalf("a reading equal to the record wrote %v", got)
	}
	h.ops.mu.Lock()
	h.ops.fenceLastErr = errors.New("etcd is away")
	h.ops.mu.Unlock()
	next := spFixture()
	stored(next)
	next.SpRevision = bump
	h.ops.setState(next)
	h.w.update(desiredState{revision: bump, handle: testSpName})
	h.awaitFlips(2)
	waitFor(t, "the failed write's record", func() bool {
		return len(h.logs.withMsg(msgSpFenceLastWriteFailed)) >= 1
	})
	h.ops.mu.Lock()
	h.ops.fenceLastErr = nil
	failed := len(h.ops.fenceLasts)
	h.ops.mu.Unlock()
	next = spFixture()
	stored(next)
	next.SpRevision = bump2
	h.ops.setState(next)
	h.w.update(desiredState{revision: bump2, handle: testSpName})
	h.awaitFlips(3)
	waitFor(t, "the zero written again", func() bool {
		return len(h.ops.fenceLastCalls()) > failed
	})
	calls := h.ops.fenceLastCalls()
	for i, call := range calls {
		if !slices.Equal(call, []model.FenceLastEdit{{Ref: ref}}) {
			t.Fatalf("write %d is %v, want the zero of group 101", i, call)
		}
	}
	failover := failoverFixture(bump3)
	h.ops.setState(failover)
	h.w.update(desiredState{revision: bump3, handle: testSpName})
	h.awaitLast(bump3)
	if recs := h.logs.withMsg(msgSpFenceRoundEnded); len(recs) != 0 {
		t.Fatalf("%d %q records: a dropped view came back from the record",
			len(recs), msgSpFenceRoundEnded)
	}
}

// TestARepairsNewLegIsAGroupLeg checks RW24's record for a repair's new leg:
// md holds it in sync once its copy ends, so the plan counts it among the
// group's legs, and a stored fence-last leg that names it is seeded.
func TestARepairsNewLegIsAGroupLeg(t *testing.T) {
	const newLeg = uint64(9001)
	captureLogs(t)
	state := spFixture()
	grp := state.Slices[spSliceA].DataGrpList[0]
	grp.RepName = "rep0"
	grp.FenceLastLegId = newLeg
	state.Repairs = map[string]*pb.Repair{"rep0": {
		RepId:   9000,
		GrpId:   grp.GetGrpId(),
		SliceId: spSliceA,
		NewLegList: []*pb.Leg{{LegId: newLeg, LegIdx: 2,
			SideList: []*pb.Side{{SideId: 9002, AddrPort: spDnC,
				Provisioned: true}}}},
	}}
	state.Conf.RepairNameList = []string{"rep0"}
	w := spTestWorker(nil)
	plan := w.buildPlan(context.Background(), state)
	if !slices.Contains(plan.fenceGrps[101].legs, newLeg) {
		t.Fatalf("group 101's legs %v lack the repair's new leg %d",
			plan.fenceGrps[101].legs, newLeg)
	}
	w.fenceLast = make(map[uint64]grpFenceView)
	w.fenceSaved = make(map[uint64]uint64)
	w.fenceRead = make(map[uint64]bool)
	w.seedFenceLast(plan)
	if got := w.fenceLast[101].last; got != newLeg {
		t.Errorf("the seeded view's fence-last leg is %d, want %d", got,
			newLeg)
	}
}
