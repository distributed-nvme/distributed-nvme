package worker

import (
	"sync"
	"testing"
	"time"

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
// never reports holds the fence-last legs' sides for one cntlr_interval
// after the demotion hold's end and no longer, and the record counts the
// side that did not report and gives the wait.
func TestFenceRoundIsBounded(t *testing.T) {
	const bump = testSpRev + 1
	h := newFenceHarness(t, spCnA, heldD(bump), nil)
	h.failOver(bump)

	h.clk.advance(roundInterval - 250*time.Millisecond)
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
	if ms, _ := recs[0]["wait_ms"].(float64); int64(ms) != roundInterval.Milliseconds() {
		t.Fatalf("wait_ms = %v, want %d", recs[0]["wait_ms"],
			roundInterval.Milliseconds())
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
		// An older cn agent's reading: no slots.
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
