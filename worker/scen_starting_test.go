package worker

import (
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// recRoundFailed is the record of a missed round (dnv-worker.md, Log
// records).
const recRoundFailed = "check round failed"

// TestStartingPrimaryIsNotFailedOver pins HL8: a primary whose agent answers
// that it is starting gives no verdict within the cluster's start hold, so it
// is not failed over though it starts for longer than the primary
// threshold; the CN's record logs the start, and its end once the agent
// answers from an observation again, and the pool goes on with no failover.
func TestStartingPrimaryIsNotFailedOver(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	hold := uint32(10)
	c := h.plantStd(t, func(s *stdSpec) { s.startHold = &hold })
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	agent, key := c.cntlrAgent(t, 1)
	start := time.Now()
	starting := func(o *agentfake.Object) { o.Starting = start }
	foLever(t, agent, key, starting)
	foLever(t, agent, agentfake.CnKey, starting)
	h.waitRecord(t, 10*time.Second, w, msgAgentStarting,
		func(rec logRecord) bool { return rec.num("cn_id") == 1 })
	// Twice the primary threshold and the rounds a failover needs.
	time.Sleep(5 * time.Second)
	if n := len(foReactions(h, w, reactionFailover)); n != 0 {
		t.Fatalf("%d failovers of a starting primary within its hold", n)
	}
	if epoch := c.cntlr(t, 1).GetErrEpoch(); epoch != 0 {
		t.Fatalf("a starting primary got a health epoch %d within its hold",
			epoch)
	}
	ended := func(o *agentfake.Object) { o.Starting = time.Time{} }
	foLever(t, agent, key, ended)
	foLever(t, agent, agentfake.CnKey, ended)
	h.waitRecord(t, 10*time.Second, w, msgAgentStarted,
		func(rec logRecord) bool { return rec.num("cn_id") == 1 })
	c.waitConverged(t)
	if n := len(foReactions(h, w, reactionFailover)); n != 0 {
		t.Fatalf("%d failovers after the start ended, want none", n)
	}
}

// TestStartingPastTheHoldIsAMissedRound pins HL8's bound: a primary whose
// agent answers starting from a start further back than the cluster's start
// hold misses its rounds, so it is failed over at the primary threshold as
// one that answers nothing.
func TestStartingPastTheHoldIsAMissedRound(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	hold := uint32(1)
	c := h.plantStd(t, func(s *stdSpec) { s.startHold = &hold })
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	agent, key := c.cntlrAgent(t, 1)
	foLever(t, agent, key, func(o *agentfake.Object) {
		o.Starting = time.Now().Add(-time.Minute)
	})
	foWaitFailover(t, h, w, 1, 2, 30*time.Second)
}

// TestRestartedAgentIsHeldPastItsDowntime pins HL8 on a restart: the rounds
// an agent's downtime misses start its cntlr's err_epoch and fill the run a
// failover needs (AR10), and the starting answers that follow hold the cntlr
// from AR5 and AR7 past both thresholds with that epoch standing, the
// primary first and then the standby; the first round answered after the
// start clears the epoch. The thresholds leave the downtime's two missed
// rounds well inside them.
func TestRestartedAgentIsHeldPastItsDowntime(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	hold := uint32(60)
	c := h.plantStd(t, func(s *stdSpec) {
		s.startHold = &hold
		s.threshold = &pb.EventThreshold{
			PrimaryUnhealthy: 6,
			CntlrUnhealthy:   6,
			SideUnhealthy:    1,
			LegUnhealthy:     2,
		}
	})
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	for _, cntlrId := range []uint64{1, 2} {
		agent, key := c.cntlrAgent(t, cntlrId)
		missed := func(rec logRecord) bool {
			id, ok := cntlrIdOf(rec)
			return ok && id == cntlrId
		}
		before := h.countRecs(w, recRoundFailed, missed)
		down := func(o *agentfake.Object) { o.Hang = true }
		foLever(t, agent, key, down)
		foLever(t, agent, agentfake.CnKey, down)
		// failoverRun missed rounds: the run a failover needs is full.
		h.waitCount(t, 10*time.Second, w, recRoundFailed,
			before+failoverRun, missed)
		if c.cntlr(t, cntlrId).GetErrEpoch() == 0 {
			t.Fatalf("cntlr %d: no epoch after its missed rounds", cntlrId)
		}
		start := time.Now()
		up := func(o *agentfake.Object) {
			o.Hang = false
			o.Starting = start
		}
		foLever(t, agent, key, up)
		foLever(t, agent, agentfake.CnKey, up)
		// Past both thresholds from the epoch, and a few passes.
		time.Sleep(9 * time.Second)
		if n := len(foReactions(h, w, reactionFailover)); n != 0 {
			t.Fatalf("cntlr %d: %d failovers of a starting cntlr", cntlrId, n)
		}
		if n := len(foReactions(h, w, reactionReplaceCntlr)); n != 0 {
			t.Fatalf("cntlr %d: %d replacements of a starting cntlr",
				cntlrId, n)
		}
		if c.cntlr(t, cntlrId).GetErrEpoch() == 0 {
			t.Fatalf("cntlr %d: the downtime's epoch is gone while it "+
				"starts; the case no longer tests a hold over an epoch",
				cntlrId)
		}
		ended := func(o *agentfake.Object) { o.Starting = time.Time{} }
		foLever(t, agent, key, ended)
		foLever(t, agent, agentfake.CnKey, ended)
		h.waitUntil(t, 10*time.Second, "the epoch cleared after the start",
			func() (bool, string) {
				return c.cntlr(t, cntlrId).GetErrEpoch() == 0, "epoch stands"
			})
	}
	c.waitConverged(t)
	if n := len(foReactions(h, w, reactionFailover)) +
		len(foReactions(h, w, reactionReplaceCntlr)); n != 0 {
		t.Fatalf("%d failovers or replacements after both starts, want none", n)
	}
}

// TestSideClockHoldsAStartingSide pins HL8 on the side clocks AR6 and AR8
// read: a side this coordinator judged unhealthy, its epoch past
// side_unhealthy, counts for a leg repair (AR8 case 2) and for the new leg
// of a repair (newLegDue) until its disk node's agent answers starting, and
// counts again once a round judges the side unhealthy; the memo keeps the
// hold until that verdict.
func TestSideClockHoldsAStartingSide(t *testing.T) {
	key := sideKey{legId: 11, sideId: 21}
	side := &pb.Side{SideId: 21, ErrEpoch: 100}
	leg := &pb.Leg{LegId: 11, ErrEpoch: 100, SideList: []*pb.Side{side}}
	memo := newVerdictMemo()
	memo.noteLeg(11, true)
	memo.noteSide(key, true)
	pass := func() *spPass {
		memo.mu.Lock()
		defer memo.mu.Unlock()
		judged := judgedSet{
			legs:          map[uint64]bool{11: memo.legs[11]},
			sides:         map[sideKey]bool{key: memo.sides[key]},
			sidesStarting: map[sideKey]bool{key: memo.sidesStarting[key]},
		}
		return &spPass{
			th:     &pb.EventThreshold{SideUnhealthy: 5, LegUnhealthy: 1000},
			now:    200,
			judged: judged,
		}
	}
	if !legNeedsRepair(pass(), leg) || !newLegDue(pass(), leg) {
		t.Fatalf("an unhealthy side past its threshold does not count")
	}
	memo.noteSideStarting(key)
	if legNeedsRepair(pass(), leg) || newLegDue(pass(), leg) {
		t.Fatalf("a side whose agent starts counts on the side clock")
	}
	memo.noteSide(key, true)
	if !legNeedsRepair(pass(), leg) || !newLegDue(pass(), leg) {
		t.Fatalf("a side judged again after its start does not count")
	}
}

// TestCrashLoopingAgentIsFailedOverPastTheHold pins HL8's start series: an
// agent that dies in each startup pass and starts again answers starting
// with a new start each time, between missed rounds. The rounds stay one
// series, whose hold is measured from its first start, so once the hold has
// passed from that start the primary is failed over, as one that does not
// answer, though no single start lasted the hold.
func TestCrashLoopingAgentIsFailedOverPastTheHold(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	hold := uint32(8)
	c := h.plantStd(t, func(s *stdSpec) {
		s.startHold = &hold
		s.threshold = &pb.EventThreshold{
			PrimaryUnhealthy: 2,
			CntlrUnhealthy:   600,
			SideUnhealthy:    1,
			LegUnhealthy:     2,
		}
	})
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	agent, key := c.cntlrAgent(t, 1)
	missed := func(rec logRecord) bool {
		id, ok := cntlrIdOf(rec)
		return ok && id == 1
	}
	first := time.Time{}
	for len(foReactions(h, w, reactionFailover)) == 0 {
		if !first.IsZero() && time.Since(first) > 30*time.Second {
			t.Fatalf("no failover %v after the first start, hold %ds",
				time.Since(first), hold)
		}
		before := h.countRecs(w, recRoundFailed, missed)
		down := func(o *agentfake.Object) { o.Hang = true }
		foLever(t, agent, key, down)
		foLever(t, agent, agentfake.CnKey, down)
		h.waitCount(t, 10*time.Second, w, recRoundFailed, before+1, missed)
		start := time.Now()
		if first.IsZero() {
			first = start
		}
		up := func(o *agentfake.Object) {
			o.Hang = false
			o.Starting = start
		}
		foLever(t, agent, key, up)
		foLever(t, agent, agentfake.CnKey, up)
		time.Sleep(3 * time.Second)
	}
	if got := time.Since(first); got < time.Duration(hold)*time.Second {
		t.Fatalf("failed over %v after the first start, inside the %ds hold",
			got, hold)
	}
	foWaitFailover(t, h, w, 1, 2, time.Second)
	if n := len(h.records(w, msgAgentRestarted)); n == 0 {
		t.Errorf("no %q record over the crash loop", msgAgentRestarted)
	}
}

// TestRefusalEndsWhenTheAgentHoldsTheRevision pins HL9's memory of a refused
// revision on one cntlr child: a SyncupCntlr refused with a failed save
// marks the revision the child sent, though the coordinator has installed a
// newer plan since; a round that answers below the driven revision keeps the
// mark; a round that answers, accepted, at the driven revision ends it,
// though no accepting Syncup* reply came back, as after a syncup cut by its
// deadline.
func TestRefusalEndsWhenTheAgentHoldsTheRevision(t *testing.T) {
	memo := newVerdictMemo()
	d := &cntlrDriver{cntlrId: 2, memo: memo}
	plan := func(rev uint64) *cntlrPlan {
		return &cntlrPlan{req: &pb.SyncupCntlrRequest{Revision: rev}}
	}
	refusing := func() bool {
		memo.mu.Lock()
		defer memo.mu.Unlock()
		return memo.refusing[2]
	}
	d.sentRev = 5
	d.noteRefusal(&replyState{code: common.ReplyCodeSaveFailed}, plan(6))
	if !refusing() || d.refusedRev != 5 {
		t.Fatalf("refused: memo %t, refusedRev %d; want the mark at the "+
			"sent revision 5", refusing(), d.refusedRev)
	}
	d.noteRefusal(&replyState{fromRound: true, revision: 4}, plan(6))
	if !refusing() {
		t.Fatal("a round below the driven revision ended the mark")
	}
	d.noteRefusal(&replyState{fromRound: true, revision: 6}, plan(6))
	if refusing() || d.refusedRev != 0 {
		t.Fatalf("a round at the driven revision: memo %t, refusedRev %d; "+
			"want the mark ended", refusing(), d.refusedRev)
	}
}

// TestGrowWithdrawalHoldsAStartingSide pins HL8 on AR6's pick of a grown
// group's unprovisioned sides (withdrawTarget): a side this coordinator
// judged unhealthy past side_unhealthy is picked until its disk node's agent
// answers starting, and again once a round judges it.
func TestGrowWithdrawalHoldsAStartingSide(t *testing.T) {
	side := &pb.Side{SideId: 21, ErrEpoch: 100, AddrPort: "dn:1"}
	key := sideKey{legId: 11, sideId: 21}
	slice := &pb.Slice{DataGrpList: []*pb.Group{
		{GrpId: 1, LegList: []*pb.Leg{{LegId: 10, SideList: []*pb.Side{
			{SideId: 20, Provisioned: true}}}}},
		{GrpId: 2, LegList: []*pb.Leg{{LegId: 11,
			SideList: []*pb.Side{side}}}},
	}}
	memo := newVerdictMemo()
	memo.noteSide(key, true)
	pass := func() *spPass {
		memo.mu.Lock()
		defer memo.mu.Unlock()
		return &spPass{
			state: &model.SpState{SpRevision: 3},
			th:    &pb.EventThreshold{SideUnhealthy: 5},
			now:   200,
			judged: judgedSet{
				sides:         map[sideKey]bool{key: memo.sides[key]},
				sidesStarting: map[sideKey]bool{key: memo.sidesStarting[key]},
			},
			infoRev: 3,
		}
	}
	picked := func() int {
		_, addrs := withdrawTarget(pass(), slice, false)
		return len(addrs)
	}
	if picked() != 1 {
		t.Fatal("an unhealthy side past its threshold is not picked")
	}
	memo.noteSideStarting(key)
	if picked() != 0 {
		t.Fatal("a side whose agent starts is picked")
	}
	memo.noteSide(key, true)
	if picked() != 1 {
		t.Fatal("a side judged again after its start is not picked")
	}
}

// TestAgentDyingAfterEachStartIsFailedOverPastTheHold pins HL8's start
// series across an ended start: the agent answers starting, then one round
// from its pass's observations, which the loop does not judge, then dies and
// starts again. With no round judged between, every start continues the one
// series, whose hold is measured from its first start, so once the hold has
// passed from that start the primary is failed over.
func TestAgentDyingAfterEachStartIsFailedOverPastTheHold(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	hold := uint32(8)
	c := h.plantStd(t, func(s *stdSpec) {
		s.startHold = &hold
		s.threshold = &pb.EventThreshold{
			PrimaryUnhealthy: 2,
			CntlrUnhealthy:   600,
			SideUnhealthy:    1,
			LegUnhealthy:     2,
		}
	})
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	agent, key := c.cntlrAgent(t, 1)
	missed := func(rec logRecord) bool {
		id, ok := cntlrIdOf(rec)
		return ok && id == 1
	}
	both := func(fn func(*agentfake.Object)) {
		foLever(t, agent, key, fn)
		foLever(t, agent, agentfake.CnKey, fn)
	}
	first := time.Time{}
	for len(foReactions(h, w, reactionFailover)) == 0 {
		if !first.IsZero() && time.Since(first) > 40*time.Second {
			t.Fatalf("no failover %v after the first start, hold %ds",
				time.Since(first), hold)
		}
		start := time.Now()
		if first.IsZero() {
			first = start
		}
		both(func(o *agentfake.Object) {
			o.Hang, o.ObsAge, o.Starting = false, 0, start
		})
		time.Sleep(2500 * time.Millisecond)
		// The start ends: rounds answer from the pass's observations, older
		// than the round that ends the start, so none is judged.
		both(func(o *agentfake.Object) {
			o.Starting, o.ObsAge = time.Time{}, 10*time.Second
		})
		time.Sleep(1500 * time.Millisecond)
		before := h.countRecs(w, recRoundFailed, missed)
		both(func(o *agentfake.Object) { o.Hang, o.ObsAge = true, 0 })
		h.waitCount(t, 10*time.Second, w, recRoundFailed, before+1, missed)
	}
	if got := time.Since(first); got < time.Duration(hold)*time.Second {
		t.Fatalf("failed over %v after the first start, inside the %ds hold",
			got, hold)
	}
	both(func(o *agentfake.Object) { o.Hang = false })
	foWaitFailover(t, h, w, 1, 2, time.Second)
}
