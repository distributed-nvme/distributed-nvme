package worker

import (
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// stampRecorder records, per object id, the fake-clock offset from start of
// the first request each side or cntlr stub gets at a revision of at least
// from.
type stampRecorder struct {
	mu    sync.Mutex
	h     *spHarness
	start time.Time
	from  uint64
	first map[uint64]time.Duration
}

func newStampRecorder(h *spHarness, from uint64) *stampRecorder {
	return &stampRecorder{h: h, start: h.clk.now(), from: from,
		first: make(map[uint64]time.Duration)}
}

func (r *stampRecorder) note(id, revision uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, seen := r.first[id]; !seen && revision >= r.from {
		r.first[id] = r.h.clk.now().Sub(r.start)
	}
}

func (r *stampRecorder) snapshot() map[uint64]time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[uint64]time.Duration, len(r.first))
	for id, at := range r.first {
		out[id] = at
	}
	return out
}

// settleEmpty waits a little real time for an early request to arrive, and
// reports whether none did.
func (r *stampRecorder) settleEmpty() bool {
	settle := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(settle) {
		if len(r.snapshot()) != 0 {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return len(r.snapshot()) == 0
}

// TestDemotionHoldFromTheRecord pins RW22's demotion read from the cntlr
// record: a coordinator that starts after a failover's commit — a restart
// or a handoff — has no child it handed the old primary its primary plan,
// but the old primary's record names the demotion (demoted_revision). It
// hands the old primary its standby plan at once, with the field left out
// of the request, and holds the sides and the other cntlrs until the old
// primary reports or the demotion hold's timer ends; a bump inside the hold
// that still finds the record's demotion does not re-arm the timer; and the
// hold's end clears the record's demotion, once.
func TestDemotionHoldFromTheRecord(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	const bump = testSpRev + 1
	state := failoverFixture(testSpRev)
	state.Cntlrs[spCntlrPrimary].DemotedRevision = testSpRev
	h.ops.setState(state)
	sides := newStampRecorder(h, testSpRev)
	cntlrs := newStampRecorder(h, testSpRev)
	for _, stub := range h.sides {
		stub.syncupReply = func(req *pb.SyncupSideRequest) *pb.SyncupSideReply {
			sides.note(req.GetSidePointer().GetSideId(), req.GetRevision())
			return &pb.SyncupSideReply{Revision: req.GetRevision()}
		}
	}
	// The old primary's agent takes its demotion and never answers it.
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	for addr, stub := range h.cntlrs {
		old := addr == spCnA
		stub.syncupReply = func(req *pb.SyncupCntlrRequest) *pb.SyncupCntlrReply {
			if old {
				<-gate
				return &pb.SyncupCntlrReply{Revision: req.GetRevision()}
			}
			cntlrs.note(req.GetCntlrPointer().GetCntlrId(), req.GetRevision())
			return &pb.SyncupCntlrReply{Revision: req.GetRevision()}
		}
	}
	w := h.start()
	t.Cleanup(release)
	h.reactWith(w, &fakeReactionOps{})
	waitFor(t, "the old primary sent its standby plan", func() bool {
		for _, req := range h.cntlrs[spCnA].syncups() {
			if req.GetRevision() == testSpRev && !req.GetCntlr().GetPrimary() {
				if got := req.GetCntlr().GetDemotedRevision(); got != 0 {
					t.Fatalf("the request carries demoted_revision %d", got)
				}
				return true
			}
		}
		return false
	})

	// One second in, a bump that changes no role and still finds the
	// record's demotion: it replaces what is held and keeps the deadline.
	h.clk.advance(time.Second)
	next := failoverFixture(bump)
	next.Cntlrs[spCntlrPrimary].DemotedRevision = testSpRev
	h.ops.setState(next)
	w.update(desiredState{revision: bump, handle: testSpName})

	wait := h.deps.tmo.demotionHold
	h.clk.advance(wait - time.Second - 250*time.Millisecond)
	if !sides.settleEmpty() {
		t.Fatalf("sides were sent the failover inside the demotion hold: %v",
			sides.snapshot())
	}
	if early := cntlrs.snapshot(); len(early) != 0 {
		t.Fatalf("cntlrs were sent the failover inside the demotion hold: %v",
			early)
	}
	h.clk.advance(250 * time.Millisecond)
	waitFor(t, "every side and the two other cntlrs sent the failover",
		func() bool {
			return len(sides.snapshot()) == 5 && len(cntlrs.snapshot()) == 2
		})
	for id, at := range sides.snapshot() {
		if at != wait {
			t.Fatalf("side %d was first sent the failover %v in, want at "+
				"the demotion hold's deadline %v", id, at, wait)
		}
	}
	recs := h.logs.withMsg(msgSpDemotionUnsynced)
	if len(recs) != 1 {
		t.Fatalf("%d %q records, want the one release by the timer",
			len(recs), msgSpDemotionUnsynced)
	}
	if ids, _ := recs[0]["cntlr_ids"].([]any); len(ids) != 1 ||
		ids[0] != float64(spCntlrPrimary) {
		t.Fatalf("cntlr_ids = %v, want [%d]", recs[0]["cntlr_ids"],
			spCntlrPrimary)
	}
	waitFor(t, "the record's demotion cleared", func() bool {
		return len(h.ops.clearCalls()) > 0
	})
	if got := h.ops.clearCalls(); !slices.Equal(got, []demotionClear{
		{spCntlrPrimary, testSpRev}}) {
		t.Fatalf("clears %+v, want the old primary's at %d", got, testSpRev)
	}
}

// TestDemotionHoldFromTheRecordEndsOnTheReport: the hold read from the
// record ends as soon as the old primary reports its demotion applied, with
// no record of an unsynced demotion, and clears the record's demotion.
func TestDemotionHoldFromTheRecordEndsOnTheReport(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	state := failoverFixture(testSpRev)
	state.Cntlrs[spCntlrPrimary].DemotedRevision = testSpRev
	h.ops.setState(state)
	sides := newStampRecorder(h, testSpRev)
	for _, stub := range h.sides {
		stub.syncupReply = func(req *pb.SyncupSideRequest) *pb.SyncupSideReply {
			sides.note(req.GetSidePointer().GetSideId(), req.GetRevision())
			return &pb.SyncupSideReply{Revision: req.GetRevision()}
		}
	}
	h.start()
	waitFor(t, "every side sent the failover", func() bool {
		return len(sides.snapshot()) == 5
	})
	if recs := h.logs.withMsg(msgSpDemotionUnsynced); len(recs) != 0 {
		t.Fatalf("%d %q records, want none: the old primary answered",
			len(recs), msgSpDemotionUnsynced)
	}
	waitFor(t, "the record's demotion cleared", func() bool {
		return slices.Equal(h.ops.clearCalls(), []demotionClear{
			{spCntlrPrimary, testSpRev}})
	})
}

// TestReplacementOfThePrimaryHoldsTheSides pins RW22's hold on a cntlr
// replacement: one that deletes the primary, whose child was last handed a
// primary plan, holds the sides and the other cntlrs, the new primary
// included, for the demotion hold's whole time, nothing reporting for the
// deleted cntlr; one that deletes a standby holds nothing beyond RW14's
// sides-first hold, the sides told at once.
func TestReplacementOfThePrimaryHoldsTheSides(t *testing.T) {
	for _, tc := range []struct {
		name      string
		oldId     uint64
		asPrimary bool
	}{
		{"the primary", spCntlrPrimary, true},
		{"a standby", spCntlrStandby, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSpHarness(t)
			h.addFixtureAgents()
			h.addCntlr(replCnA)
			state := spFixture()
			state.Conf.NextId = replFirstId
			h.ops.setState(state)
			sides := newStampRecorder(h, testSpRev+1)
			for _, stub := range h.sides {
				stub.syncupReply = func(
					req *pb.SyncupSideRequest,
				) *pb.SyncupSideReply {
					sides.note(req.GetSidePointer().GetSideId(),
						req.GetRevision())
					return &pb.SyncupSideReply{Revision: req.GetRevision()}
				}
			}
			w := h.start()
			h.reactWith(w, &fakeReactionOps{})
			waitFor(t, "every cntlr synced at N", func() bool {
				for _, addr := range []string{spCnA, spCnB, spCnC} {
					if len(h.cntlrs[addr].syncups()) == 0 {
						return false
					}
				}
				return true
			})
			sides.start = h.clk.now()
			loads := h.ops.loadCnt()
			_, revision := h.ops.commitReplace(tc.oldId, replCnA, replCnIdA,
				tc.asPrimary)
			w.update(desiredState{revision: revision, handle: testSpName})
			waitFor(t, "the replacement's fan-out", func() bool {
				return h.ops.loadCnt() > loads
			})
			if !tc.asPrimary {
				waitFor(t, "every side sent the replacement at once",
					func() bool { return len(sides.snapshot()) == 5 })
				if recs := h.logs.withMsg(msgSpDemotionUnsynced); len(recs) != 0 {
					t.Fatalf("a replacement of a standby ran a demotion "+
						"hold: %v", recs)
				}
				return
			}
			wait := h.deps.tmo.demotionHold
			h.clk.advance(wait - 250*time.Millisecond)
			if !sides.settleEmpty() {
				t.Fatalf("sides were sent the replacement inside the "+
					"demotion hold: %v", sides.snapshot())
			}
			if got := len(h.cntlrs[replCnA].syncups()); got != 0 {
				t.Fatalf("the new primary was sent %d requests inside the "+
					"demotion hold", got)
			}
			// The hold's timer starts with the fan-out, which the test sees
			// begin but not end, so the clock is stepped on until the sides
			// are told, none of them before the hold's time.
			for step := 0; len(sides.snapshot()) < 5; step++ {
				if step == 40 {
					t.Fatalf("the sides were never sent the replacement: %v",
						sides.snapshot())
				}
				h.clk.advance(250 * time.Millisecond)
				settle := time.Now().Add(200 * time.Millisecond)
				for time.Now().Before(settle) && len(sides.snapshot()) < 5 {
					time.Sleep(time.Millisecond)
				}
			}
			for id, at := range sides.snapshot() {
				if at < wait {
					t.Fatalf("side %d was first sent the replacement %v in, "+
						"before the demotion hold's end %v", id, at, wait)
				}
			}
			recs := h.logs.withMsg(msgSpDemotionUnsynced)
			if len(recs) != 1 {
				t.Fatalf("%d %q records, want one", len(recs),
					msgSpDemotionUnsynced)
			}
			if ids, _ := recs[0]["cntlr_ids"].([]any); len(ids) != 1 ||
				ids[0] != float64(tc.oldId) {
				t.Fatalf("cntlr_ids = %v, want [%d]", recs[0]["cntlr_ids"],
					tc.oldId)
			}
		})
	}
}

// gateOldPrimary makes the old primary's agent (spCnA) take every syncup
// past the first revision and answer none until the returned release runs.
func gateOldPrimary(t *testing.T, h *spHarness) func() {
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	h.cntlrs[spCnA].syncupReply = func(
		req *pb.SyncupCntlrRequest,
	) *pb.SyncupCntlrReply {
		if req.GetRevision() > testSpRev {
			<-gate
		}
		return &pb.SyncupCntlrReply{Revision: req.GetRevision()}
	}
	return release
}

// noteSides records the first syncup of each side at a revision of at least
// from.
func noteSides(h *spHarness, from uint64) *stampRecorder {
	sides := newStampRecorder(h, from)
	for _, stub := range h.sides {
		stub.syncupReply = func(req *pb.SyncupSideRequest) *pb.SyncupSideReply {
			sides.note(req.GetSidePointer().GetSideId(), req.GetRevision())
			return &pb.SyncupSideReply{Revision: req.GetRevision()}
		}
	}
	return sides
}

// syncedAtN waits for every cntlr's first syncup at the harness's revision.
func syncedAtN(t *testing.T, h *spHarness) {
	t.Helper()
	waitFor(t, "every cntlr synced at N", func() bool {
		for _, addr := range []string{spCnA, spCnB, spCnC} {
			if len(h.cntlrs[addr].syncups()) == 0 {
				return false
			}
		}
		return true
	})
}

// TestOwnFailoverClearsTheRecord: the hold a coordinator runs for its own
// failover, here ended by its timer since the old primary never answers,
// clears the record's demotion once, at the failover's revision, so a later
// bump finds no demotion and runs no second hold: its sides are told at once.
func TestOwnFailoverClearsTheRecord(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	release := gateOldPrimary(t, h)
	w := h.start()
	// Registered after start, so it runs before the coordinator's stop,
	// which would otherwise wait for the held SyncupCntlr to time out.
	t.Cleanup(release)
	h.reactWith(w, &fakeReactionOps{})
	syncedAtN(t, h)
	failRev := h.ops.commitFailover(spCntlrPrimary, spCntlrStandby)
	sides := noteSides(h, failRev)
	w.update(desiredState{revision: failRev, handle: testSpName})
	waitFor(t, "the old primary sent its demotion", func() bool {
		for _, req := range h.cntlrs[spCnA].syncups() {
			if req.GetRevision() == failRev && !req.GetCntlr().GetPrimary() {
				return true
			}
		}
		return false
	})
	h.clk.advance(h.deps.tmo.demotionHold)
	waitFor(t, "every side sent the failover", func() bool {
		return len(sides.snapshot()) == 5
	})
	waitFor(t, "the record's demotion cleared", func() bool {
		return len(h.ops.clearCalls()) > 0
	})
	if got := h.ops.clearCalls(); !slices.Equal(got, []demotionClear{
		{spCntlrPrimary, failRev}}) {
		t.Fatalf("clears %+v, want the old primary's at %d", got, failRev)
	}

	bump := h.ops.commitBump()
	later := noteSides(h, bump)
	w.update(desiredState{revision: bump, handle: testSpName})
	waitFor(t, "every side sent the bump at once", func() bool {
		return len(later.snapshot()) == 5
	})
	if recs := h.logs.withMsg(msgSpDemotionUnsynced); len(recs) != 1 {
		t.Fatalf("%d %q records, want the failover's one", len(recs),
			msgSpDemotionUnsynced)
	}
}

// TestFailedClearHoldsNoMore: a clear that fails is logged, and the record
// keeps its demotion; a later bump, whose fan-out finds the old primary's
// report at that revision already, runs no second hold.
func TestFailedClearHoldsNoMore(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	h.ops.clearErr = errors.New("etcd down")
	w := h.start()
	h.reactWith(w, &fakeReactionOps{})
	syncedAtN(t, h)
	failRev := h.ops.commitFailover(spCntlrPrimary, spCntlrStandby)
	sides := noteSides(h, failRev)
	w.update(desiredState{revision: failRev, handle: testSpName})
	waitFor(t, "every side sent the failover", func() bool {
		return len(sides.snapshot()) == 5
	})
	waitFor(t, "the failed clear logged", func() bool {
		return len(h.logs.withMsg(msgSpDemotionClearFailed)) == 1
	})

	bump := h.ops.commitBump()
	later := noteSides(h, bump)
	w.update(desiredState{revision: bump, handle: testSpName})
	waitFor(t, "every side sent the bump at once", func() bool {
		return len(later.snapshot()) == 5
	})
	if n := len(h.ops.clearCalls()); n != 1 {
		t.Fatalf("%d clears, want the one at the failover's hold", n)
	}
}

// TestNoDemotionForAPrimaryNotDeleted: a primary the plan leaves idle, its
// CN's record missing (RW14), and one a drain takes away, are no demotion
// (RW22): the sides are told at once, and no hold runs.
func TestNoDemotionForAPrimaryNotDeleted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(state *model.SpState)
	}{
		{"its CN's record missing", func(state *model.SpState) {
			delete(state.CnByAddr, spCnA)
		}},
		{"a drain", func(state *model.SpState) {
			state.Conf.Deleting = true
			state.Conf.CntlrIdList = nil
			state.Cntlrs = map[uint64]*pb.Cntlr{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSpHarness(t)
			h.addFixtureAgents()
			w := h.start()
			h.reactWith(w, &fakeReactionOps{})
			syncedAtN(t, h)
			loads := h.ops.loadCnt()
			next := spFixture()
			next.SpRevision = testSpRev + 1
			next.CnByAddr = maps.Clone(next.CnByAddr)
			next.Conf = proto.Clone(next.Conf).(*pb.SpConf)
			tc.apply(next)
			h.ops.setState(next)
			w.update(desiredState{revision: testSpRev + 1,
				handle: testSpName})
			waitFor(t, "the fan-out", func() bool {
				return h.ops.loadCnt() > loads
			})
			h.clk.advance(h.deps.tmo.demotionHold + time.Second)
			time.Sleep(100 * time.Millisecond)
			if recs := h.logs.withMsg(msgSpDemotionUnsynced); len(recs) != 0 {
				t.Fatalf("a demotion hold ran: %v", recs)
			}
		})
	}
}

// TestANewerDemotionRearmsTheHold: while the hold read from the record
// waits for a demotion, a fan-out whose record names a newer one — the cntlr
// promoted and demoted again between two fan-outs — is a new demotion: the
// hold waits for that revision and re-arms its timer, and its end clears
// the newer value.
func TestANewerDemotionRearmsTheHold(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	const bump = testSpRev + 2
	state := failoverFixture(testSpRev)
	state.Cntlrs[spCntlrPrimary].DemotedRevision = testSpRev
	h.ops.setState(state)
	sides := noteSides(h, testSpRev)
	release := gateOldPrimary(t, h)
	// The gate holds syncups past testSpRev only: hold the first one too.
	gate := make(chan struct{})
	releaseFirst := sync.OnceFunc(func() { close(gate) })
	h.cntlrs[spCnA].syncupReply = func(
		req *pb.SyncupCntlrRequest,
	) *pb.SyncupCntlrReply {
		<-gate
		return &pb.SyncupCntlrReply{Revision: req.GetRevision()}
	}
	w := h.start()
	t.Cleanup(release)
	t.Cleanup(releaseFirst)
	h.reactWith(w, &fakeReactionOps{})
	waitFor(t, "the old primary sent its standby plan", func() bool {
		return len(h.cntlrs[spCnA].syncups()) > 0
	})

	h.clk.advance(time.Second)
	loads := h.ops.loadCnt()
	next := failoverFixture(bump)
	next.Cntlrs[spCntlrPrimary].DemotedRevision = bump
	h.ops.setState(next)
	w.update(desiredState{revision: bump, handle: testSpName})
	waitFor(t, "the fan-out of the newer demotion", func() bool {
		return h.ops.loadCnt() > loads
	})
	// The fan-out's plan work after its load takes no time on the fake
	// clock; give it real time to finish before the clock moves.
	time.Sleep(50 * time.Millisecond)
	wait := h.deps.tmo.demotionHold
	// The first deadline passes: the re-armed hold still holds the sides.
	h.clk.advance(wait - time.Second + 250*time.Millisecond)
	if !sides.settleEmpty() {
		t.Fatalf("sides were sent the failover at the first demotion's "+
			"deadline: %v", sides.snapshot())
	}
	h.clk.advance(time.Second)
	waitFor(t, "every side sent the failover", func() bool {
		return len(sides.snapshot()) == 5
	})
	waitFor(t, "the newer demotion cleared", func() bool {
		return len(h.ops.clearCalls()) > 0
	})
	if got := h.ops.clearCalls(); !slices.Equal(got, []demotionClear{
		{spCntlrPrimary, bump}}) {
		t.Fatalf("clears %+v, want the newer demotion's at %d", got, bump)
	}
}
