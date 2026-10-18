package worker

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/integtest/plant"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The whole-worker scenario cases of the reactions other than the failover:
// cntlr replacement (dnv-worker.md, AR7), leg repair (AR8) and thin-pool
// auto-grow (AR6), with AR2's pass rules, each played through the fakes of one
// planted cluster under one whole worker. A pass, the worker's evaluation of
// one pool's reactions, runs once a cntlr_interval, one second here; every
// "for a while" a case holds is two passes or more, counted by records or by
// the fakes' rounds, never by a sleep.

// ---------------------------------------------------------------------------
// Cntlr replacement (AR7)
// ---------------------------------------------------------------------------

// failCntlr makes every row of one cntlr read ERROR on its fake, so its
// child's verdicts are unhealthy and its health epoch is set (HL2).
func (c *stdCluster) failCntlr(t *testing.T, cntlrId uint64) {
	t.Helper()
	agent, key := c.cntlrAgent(t, cntlrId)
	setLever(t, agent, key, func(o *agentfake.Object) {
		o.Status = statusPtr(pb.ResStatus_RES_STATUS_ERROR)
	})
}

// TestScenReplaceCntlrWaitsForACnWithRoom pins AR7's scan: the old cntlr's
// CN is black-listed though it has room, the SP's other CN is excluded, so
// with the third CN full the replacement of an unhealthy standby finds no
// candidate, logs `reaction skipped` (no candidate) on every pass and
// replaces nothing; once the third CN has room the next pass replaces it
// there, and the new standby converges.
func TestScenReplaceCntlrWaitsForACnWithRoom(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	ctx := context.Background()
	if _, _, err := plant.SetCnFree(ctx, h.cli, c.cl, 3, 0); err != nil {
		t.Fatalf("SetCnFree: %v", err)
	}
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	c.failCntlr(t, 2)
	noRoom := and(reactionIs(reactionReplaceCntlr, reasonNoCandidate),
		numIs("old_cntlr_id", 2))
	// Two passes that found cntlr 2 due and no CN for it.
	h.waitCount(t, 20*time.Second, w, msgReactionSkipped, 2, noRoom)
	if n := h.countRecs(w, msgReactionApplied, reactionIs(reactionReplaceCntlr, "")); n != 0 {
		t.Fatalf("%d replacements applied with no CN with room", n)
	}
	if ids := c.load(t).Conf.GetCntlrIdList(); len(ids) != 2 {
		t.Fatalf("the pool lists cntlrs %v while no CN has room", ids)
	}

	if _, _, err := plant.SetCnFree(ctx, h.cli, c.cl, 3, c.spec.nodeFree); err != nil {
		t.Fatalf("SetCnFree: %v", err)
	}
	applied := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		and(reactionIs(reactionReplaceCntlr, ""), numIs("old_cntlr_id", 2)))
	if got := applied.str("addr_port"); got != stdCnAddr(3) {
		t.Errorf("the replacement went to %s, want cn 3 at %s", got, stdCnAddr(3))
	}
	newId := applied.num("new_cntlr_id")
	c.waitConverged(t)
	state := c.load(t)
	if _, ok := state.Cntlrs[2]; ok {
		t.Errorf("cntlr 2 is still in the pool after its replacement")
	}
	if cntlr := state.Cntlrs[newId]; cntlr == nil || cntlr.GetPrimary() ||
		cntlr.GetAddrPort() != stdCnAddr(3) {
		t.Errorf("cntlr %d after the replacement: %v, want a standby on cn 3",
			newId, cntlr)
	}
	if n := h.countRecs(w, msgReactionApplied, reactionIs(reactionReplaceCntlr, "")); n != 1 {
		t.Errorf("%d replacements applied, want 1", n)
	}
}

// TestScenReplaceCntlrCandidateChangedBetweenScanAndCommit pins AR7's stale
// pick (MD6): the CN the scan picked loses an extent between the scan and
// the commit, so model.ReplaceCntlr refuses the pick as `candidate changed`
// and the pass ends; the next pass scans again and replaces the cntlr on that
// CN at its new count. The case changes the CN from inside the scan's own
// record (onRecord), so the change lands between the two at every run.
func TestScenReplaceCntlrCandidateChangedBetweenScanAndCommit(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{keepWire: true})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	var fired atomic.Bool
	var scanTrace atomic.Value
	cnPrefix := model.CnCapacityPrefix(c.cl.Id)
	h.onRecord(func(_ context.Context, rec logRecord) {
		if rec.msg != "etcd range" || rec.str("prefix") != cnPrefix ||
			!fired.CompareAndSwap(false, true) {
			return
		}
		scanTrace.Store(rec.traceId)
		if _, _, err := plant.SetCnFree(context.Background(), h.cli, c.cl,
			3, c.spec.nodeFree-1); err != nil {
			t.Errorf("SetCnFree inside the scan: %v", err)
		}
	})
	c.failCntlr(t, 2)
	changed := h.waitRecord(t, 20*time.Second, w, msgReactionSkipped,
		reactionIs(reactionReplaceCntlr, model.ReasonCandidateChanged))
	if trace, _ := scanTrace.Load().(string); changed.traceId != trace {
		t.Errorf("the refusal came in pass %s, the changed scan in %s",
			changed.traceId, trace)
	}
	applied := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		reactionIs(reactionReplaceCntlr, ""))
	if applied.traceId == changed.traceId {
		t.Errorf("the replacement committed in the pass whose commit was refused")
	}
	if got := applied.str("addr_port"); got != stdCnAddr(3) {
		t.Errorf("the replacement went to %s, want cn 3", got)
	}
	// The pick was scanned again: cn 3 was charged from the count the change
	// left, one extent short of the planted one.
	footprint := uint64(1 + 2)
	if got, want := c.cnFree(t, 3), c.spec.nodeFree-1-footprint; got != want {
		t.Errorf("cn 3 has %d free extents, want %d", got, want)
	}
	if n := h.countRecs(w, msgReactionSkipped,
		reactionIs(reactionReplaceCntlr, model.ReasonCandidateChanged)); n != 1 {
		t.Errorf("%d candidate-changed refusals, want 1", n)
	}
}

// TestScenReplaceCntlrOfADeadCn pins AR7 against a dead CN: the standby on a
// CN whose agent stopped is replaced on the third CN once cntlr_unhealthy has
// run out, the replacement converges, the dead CN's record no longer lists
// the old cntlr, and the dead CN's agent, back with its files, gets the
// syncup that drops it, since an agent that was down keeps what it held
// until its next syncup (architecture.md, Teardown by sweep).
func TestScenReplaceCntlrOfADeadCn(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	cn2 := c.cnAgent(2)
	cn2.Stop()
	applied := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		and(reactionIs(reactionReplaceCntlr, ""), numIs("old_cntlr_id", 2)))
	if got := applied.str("addr_port"); got != stdCnAddr(3) {
		t.Errorf("the replacement went to %s, want cn 3", got)
	}
	newId := applied.num("new_cntlr_id")
	h.waitUntil(t, 20*time.Second, "the replacement on cn 3 to converge",
		func() (bool, string) {
			state := c.load(t)
			msg, _, _ := lastSyncup(c.cnAgent(3), agentfake.CntlrKey(stdSpId, newId))
			req, _ := msg.(*pb.SyncupCntlrRequest)
			if req == nil || req.GetRevision() != state.SpRevision {
				return false, fmt.Sprintf("cntlr %d's last syncup %v, SpRev %d",
					newId, req.GetRevision(), state.SpRevision)
			}
			if epoch := state.Cntlrs[newId].GetErrEpoch(); epoch != 0 {
				return false, fmt.Sprintf("cntlr %d err_epoch %d", newId, epoch)
			}
			return true, ""
		})
	for _, ptr := range c.cnConf(t, 2).GetCntlrPtrList() {
		if ptr.GetSpId() == stdSpId {
			t.Fatalf("cn 2's record still lists %v after the replacement", ptr)
		}
	}
	if err := cn2.Start(agentfake.KeepStore); err != nil {
		t.Fatalf("restart cn 2: %v", err)
	}
	h.waitUntil(t, 20*time.Second, "cn 2's syncup that drops cntlr 2",
		func() (bool, string) {
			msg, _, _ := lastSyncup(cn2, agentfake.CnKey)
			req, _ := msg.(*pb.SyncupCnRequest)
			if req == nil || req.GetRevision() != c.cnRev(t, 2) {
				return false, "no SyncupCn at cn 2's revision yet"
			}
			for _, ptr := range req.GetCntlrPointerList() {
				if ptr.GetSpId() == stdSpId {
					return false, fmt.Sprintf("the last SyncupCn lists %v", ptr)
				}
			}
			return true, ""
		})
	c.waitConverged(t)
}

// TestScenReplacementRefusedUnknownUntilItsCnSyncupLands pins RW5 for a
// replacement: the new cntlr's syncups reach its CN before the cn role's
// syncup lists it, and are refused as an unknown object, logged as
// `syncup rejected` and re-sent from the next rounds, with no health
// verdict meanwhile (HL1), until the CN's syncup lands; then the cntlr's
// syncup is accepted and it converges. The case holds the CN's syncup, so the
// refusals last as long as it wants.
func TestScenReplacementRefusedUnknownUntilItsCnSyncupLands(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	cn3 := c.cnAgent(3)
	setLever(t, cn3, agentfake.CnKey, func(o *agentfake.Object) { o.HangSyncup = true })
	defer setLever(t, cn3, agentfake.CnKey, func(o *agentfake.Object) { o.HangSyncup = false })
	c.failCntlr(t, 2)
	applied := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		reactionIs(reactionReplaceCntlr, ""))
	newId := applied.num("new_cntlr_id")
	ofNew := func(rec logRecord) bool {
		id, ok := cntlrIdOf(rec)
		return ok && id == newId
	}
	h.waitCount(t, 20*time.Second, w, msgSyncupRejected, 2,
		and(ofNew, numIs("code", common.ReplyCodeUnknownObject)))
	if epoch := c.cntlr(t, newId).GetErrEpoch(); epoch != 0 {
		t.Errorf("cntlr %d has err_epoch %d from refused syncups", newId, epoch)
	}
	for _, rec := range h.records(w, msgSyncupResult) {
		if ofNew(rec) && rec.num("code") == 0 && rec.str("error") == "" {
			t.Fatalf("cntlr %d's syncup was accepted while cn 3's syncup "+
				"was held: %v", newId, rec.attrs)
		}
	}

	setLever(t, cn3, agentfake.CnKey, func(o *agentfake.Object) { o.HangSyncup = false })
	cnApplied := h.waitRecord(t, 20*time.Second, w, msgSyncupResult,
		func(rec logRecord) bool {
			return isNodeRec(rec, "cn_id", 3) && rec.num("code") == 0 &&
				rec.str("error") == "" && rec.num("revision") == c.cnRev(t, 3)
		})
	accepted := h.waitRecord(t, 20*time.Second, w, msgSyncupResult,
		func(rec logRecord) bool {
			return ofNew(rec) && rec.num("code") == 0 && rec.str("error") == ""
		})
	if accepted.at.Before(cnApplied.at) {
		t.Errorf("cntlr %d's syncup was accepted %v before cn 3's syncup "+
			"listed it", newId, cnApplied.at.Sub(accepted.at))
	}
	c.waitConverged(t)
}

// ---------------------------------------------------------------------------
// Leg repair (AR8)
// ---------------------------------------------------------------------------

// failLeg makes the primary report one leg's row ERROR, the trigger of AR8's
// case 1 once leg_unhealthy has run out.
func (c *stdCluster) failLeg(t *testing.T, legId uint64) {
	t.Helper()
	agent, key := c.cntlrAgent(t, 1)
	setRow(t, agent, key, fmt.Sprintf("leg_id_to_leg.%d", legId),
		rowLever(pb.ResStatus_RES_STATUS_ERROR, "", true))
}

// spareIds are the leg and side ids AR8's create takes next (CreateSpareLeg
// takes the pool's next free id for the leg and the one after for its side).
func (c *stdCluster) spareIds(t *testing.T) (uint64, uint64) {
	t.Helper()
	next := c.nextSpId(t)
	return next, next + 1
}

// spareShape is the standard cluster with a fourth DN. The group's legs sit
// on dn 1 and dn 2, so dn 3 and dn 4 are both outside it: after the first
// spare takes one of them, a second spare still has a DN to go to, and a
// case's count of spare creates can catch one.
var spareShape = scenShape{extraDns: 1}

// spareDns are the DNs outside the group in spareShape.
var spareDns = []uint64{3, 4}

// spareDnOf is the DN of spareDns that a spare create record names, or 0.
func spareDnOf(rec logRecord) uint64 {
	for _, id := range spareDns {
		if rec.str("addr_port") == stdDnAddr(id) {
			return id
		}
	}
	return 0
}

// waitOneSpare waits, as waitCount does, for n records of w with msg that
// match accepts, and fails as soon as a second spare create is applied
// meanwhile (while names the moment for the message). A second spare can be
// ready at once and switched in, which ends the records waited for, so the
// count is checked inside the wait, where a failure names the second spare
// rather than a timeout.
func (h *workerHarness) waitOneSpare(
	t *testing.T,
	timeout time.Duration,
	w *harnessWorker,
	msg string,
	n int,
	match func(logRecord) bool,
	while string,
) {
	t.Helper()
	h.waitUntil(t, timeout, fmt.Sprintf("%d %q records", n, msg),
		func() (bool, string) {
			spares := h.countRecs(w, msgReactionApplied, reactionIs(reactionSpareCreate, ""))
			if spares != 1 {
				t.Fatalf("%d spares created %s, want 1", spares, while)
			}
			got := h.countRecs(w, msg, match)
			return got >= n, fmt.Sprintf("%d so far", got)
		})
}

// slowZeroing holds a side's zeroing at none done, or lets it finish.
func slowZeroing(t *testing.T, agent *agentfake.Agent, key string, slow bool) {
	t.Helper()
	setLever(t, agent, key, func(o *agentfake.Object) {
		if slow {
			o.ZeroedBytes = uint64Ptr(0)
		} else {
			o.ZeroedBytes = nil
		}
	})
}

// waitSwitched waits for AR8 step 1's switch of spare into leg's place and
// checks that the group then holds the spare in its leg list and the old leg
// parked in its spare list.
func (c *stdCluster) waitSwitched(t *testing.T, w *harnessWorker, legId, spareLegId uint64) {
	t.Helper()
	c.h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		and(reactionIs(reactionSpareSwitch, ""), and(numIs("leg_id", legId),
			numIs("spare_leg_id", spareLegId))))
	grp := c.dataGrp(t)
	if !slices.Contains(legIds(grp.GetLegList()), spareLegId) ||
		!slices.Contains(legIds(grp.GetSpareLegList()), legId) {
		t.Errorf("after the switch the group has legs %v and spares %v, want "+
			"leg %d in and leg %d parked", legIds(grp.GetLegList()),
			legIds(grp.GetSpareLegList()), spareLegId, legId)
	}
}

// TestScenLegRepairWaitsForASlowSpare pins AR8 steps 3, 2 and 1: a data leg
// the primary reports ERROR past leg_unhealthy gets one spare on a DN outside
// its group; while the spare's side zeroes, every pass waits for it
// (`reaction skipped`, spare_pending) and creates no second one, though
// another DN outside the group has room for it; once its zeroing ends the
// side is flipped provisioned (RW18) and the next pass switches the spare in
// and parks the leg.
func TestScenLegRepairWaitsForASlowSpare(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantScen(t, spareShape)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	spareLeg, spareSide := c.spareIds(t)
	spareKey := agentfake.SideKey(stdSpId, spareLeg, spareSide)
	for _, id := range spareDns {
		slowZeroing(t, c.dnAgent(id), spareKey, true)
	}
	c.failLeg(t, 6)
	created := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		and(reactionIs(reactionSpareCreate, ""), numIs("leg_id", 6)))
	spareDn := spareDnOf(created)
	if created.num("spare_leg_id") != spareLeg || spareDn == 0 {
		t.Fatalf("the spare is leg %d at %s, want leg %d on dn 3 or dn 4",
			created.num("spare_leg_id"), created.str("addr_port"), spareLeg)
	}
	// Three passes that find the spare zeroing: each waits for it.
	pending := and(reactionIs(reactionSpareSwitch, reasonSparePending),
		numIs("spare_leg_id", spareLeg))
	h.waitOneSpare(t, 20*time.Second, w, msgReactionSkipped, 3, pending,
		"while one zeroes")
	if _, _, side, ok := c.sideOf(t, spareSide); !ok || side.GetProvisioned() {
		t.Fatalf("the spare's side while it zeroes: found %t, %v", ok, side)
	}

	slowZeroing(t, c.dnAgent(spareDn), spareKey, false)
	c.waitSwitched(t, w, 6, spareLeg)
	if n := h.countRecs(w, msgReactionApplied, reactionIs(reactionSpareCreate, "")); n != 1 {
		t.Errorf("%d spares created for one repair, want 1", n)
	}
}

// TestScenLegRepairHoldsASpareWhoseDnDiesWhileZeroing pins AR8 step 3's hold
// (Known limits): a spare whose DN stops while the spare zeroes is no pending
// spare any more (its side's health epoch is set) and still unprovisioned,
// so every pass logs `reaction skipped` (spare_unprovisioned) and creates no
// second spare, though another DN outside the group is up and has room for
// it; once that DN is back and finishes the zeroing, the spare is switched in.
func TestScenLegRepairHoldsASpareWhoseDnDiesWhileZeroing(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantScen(t, spareShape)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	spareLeg, spareSide := c.spareIds(t)
	spareKey := agentfake.SideKey(stdSpId, spareLeg, spareSide)
	for _, id := range spareDns {
		slowZeroing(t, c.dnAgent(id), spareKey, true)
	}
	c.failLeg(t, 6)
	created := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		and(reactionIs(reactionSpareCreate, ""), numIs("spare_leg_id", spareLeg)))
	spareDn := spareDnOf(created)
	if spareDn == 0 {
		t.Fatalf("the spare went to %s, want dn 3 or dn 4", created.str("addr_port"))
	}
	h.waitRecord(t, 20*time.Second, w, msgReactionSkipped,
		and(reactionIs(reactionSpareSwitch, reasonSparePending),
			numIs("spare_leg_id", spareLeg)))

	dn := c.dnAgent(spareDn)
	dn.Stop()
	h.waitUntil(t, 20*time.Second, "the spare's side to carry a health epoch",
		func() (bool, string) {
			_, _, side, ok := c.sideOf(t, spareSide)
			return ok && side.GetErrEpoch() != 0, fmt.Sprintf("%v", side)
		})
	held := and(reactionIs(reactionSpareCreate, reasonSpareUnprovisioned),
		numIs("spare_leg_id", spareLeg))
	h.waitOneSpare(t, 20*time.Second, w, msgReactionSkipped, 2, held,
		"while the first one's DN is down")

	slowZeroing(t, dn, spareKey, false)
	if err := dn.Start(agentfake.KeepStore); err != nil {
		t.Fatalf("restart dn %d: %v", spareDn, err)
	}
	c.waitSwitched(t, w, 6, spareLeg)
	if n := h.countRecs(w, msgReactionApplied, reactionIs(reactionSpareCreate, "")); n != 1 {
		t.Errorf("%d spares created for one repair, want 1", n)
	}
}

// TestScenLegRepairWithTheSpareListFull pins AR8 step 4: a group whose spare
// list already holds MaxSpareLegPerGrp legs, both dead (the primary reports
// them ERROR past leg_unhealthy, as legs parked by earlier repairs), gets no
// spare and no switch for its failing leg; every pass logs
// `reaction skipped` (spare_list_full), since only an operator frees a slot.
func TestScenLegRepairWithTheSpareListFull(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantScen(t, scenShape{
		extraDns: 1,
		spares: []struct{ legId, sideId, dnId uint64 }{
			{legId: 12, sideId: 13, dnId: 3},
			{legId: 14, sideId: 15, dnId: 4},
		},
	})
	if common.MaxSpareLegPerGrp != 2 {
		t.Fatalf("the shape plants 2 spares, MaxSpareLegPerGrp is %d",
			common.MaxSpareLegPerGrp)
	}
	c.failLeg(t, 12)
	c.failLeg(t, 14)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	// The two spares are parked legs: provisioned, and dead by their leg's
	// epoch, older than leg_unhealthy, before the active leg fails.
	h.waitUntil(t, 30*time.Second, "both spares provisioned and past leg_unhealthy",
		func() (bool, string) {
			now := uint64(time.Now().Unix())
			for _, spare := range c.dataGrp(t).GetSpareLegList() {
				side := singleSide(spare)
				if side == nil || !side.GetProvisioned() {
					return false, fmt.Sprintf("spare %d not provisioned", spare.GetLegId())
				}
				if !reached(now, spare.GetErrEpoch(), c.spec.threshold.GetLegUnhealthy()) {
					return false, fmt.Sprintf("spare %d err_epoch %d",
						spare.GetLegId(), spare.GetErrEpoch())
				}
			}
			return true, ""
		})
	c.failLeg(t, 6)
	full := and(reactionIs(reactionSpareCreate, reasonSpareListFull), numIs("leg_id", 6))
	h.waitCount(t, 20*time.Second, w, msgReactionSkipped, 3, full)
	if n := h.countRecs(w, msgReactionApplied, nil); n != 0 {
		t.Errorf("%d reactions applied with the spare list full, want 0", n)
	}
	if n := h.countRecs(w, msgReactionSkipped, reactionIs(reactionSpareSwitch, reasonSparePending)); n != 0 {
		t.Errorf("%d passes waited for a dead parked leg", n)
	}
	grp := c.dataGrp(t)
	if got := legIds(grp.GetSpareLegList()); len(got) != 2 {
		t.Errorf("the spare list is %v, want the two planted spares", got)
	}
}

// TestScenLegRepairWaitsForADnWithRoom pins AR8 step 3's scan: with the one
// DN outside the group full (the group's DNs are black-listed), the repair
// finds no candidate and logs `reaction skipped` (no candidate) on every
// pass; once that DN has room, the next pass creates the spare there and it is
// switched in.
func TestScenLegRepairWaitsForADnWithRoom(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	ctx := context.Background()
	if _, _, err := plant.SetDnFree(ctx, h.cli, c.cl, 3, 0); err != nil {
		t.Fatalf("SetDnFree: %v", err)
	}
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	c.failLeg(t, 6)
	noRoom := and(reactionIs(reactionSpareCreate, reasonNoCandidate), numIs("leg_id", 6))
	h.waitCount(t, 20*time.Second, w, msgReactionSkipped, 2, noRoom)
	if n := h.countRecs(w, msgReactionApplied, nil); n != 0 {
		t.Fatalf("%d reactions applied with no DN for a spare, want 0", n)
	}

	if _, _, err := plant.SetDnFree(ctx, h.cli, c.cl, 3, c.spec.nodeFree); err != nil {
		t.Fatalf("SetDnFree: %v", err)
	}
	created := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		reactionIs(reactionSpareCreate, ""))
	if got := created.str("addr_port"); got != stdDnAddr(3) {
		t.Errorf("the spare went to %s, want dn 3", got)
	}
	c.waitSwitched(t, w, 6, created.num("spare_leg_id"))
}

// TestScenLegRepairWaitsForASpareThePrimaryNeverReports pins AR8 step 2's
// wait with no bound: the primary's syncups are held, so it never learns of
// the spare and reports no row of it; the spare is provisioned and carries no
// health epoch, so it stays pending, and every pass waits for it past
// leg_unhealthy without creating a second spare on the other DN outside the
// group; once the primary takes its syncup and reports the spare OK, the
// spare is switched in.
func TestScenLegRepairWaitsForASpareThePrimaryNeverReports(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantScen(t, spareShape)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	primary, primaryKey := c.cntlrAgent(t, 1)
	setLever(t, primary, primaryKey, func(o *agentfake.Object) { o.HangSyncup = true })
	defer setLever(t, primary, primaryKey, func(o *agentfake.Object) { o.HangSyncup = false })
	spareLeg, spareSide := c.spareIds(t)
	c.failLeg(t, 6)
	h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		and(reactionIs(reactionSpareCreate, ""), numIs("spare_leg_id", spareLeg)))
	h.waitUntil(t, 20*time.Second, "the spare's side provisioned",
		func() (bool, string) {
			_, _, side, ok := c.sideOf(t, spareSide)
			return ok && side.GetProvisioned(), fmt.Sprintf("%v", side)
		})
	pending := and(reactionIs(reactionSpareSwitch, reasonSparePending),
		numIs("spare_leg_id", spareLeg))
	from := h.countRecs(w, msgReactionSkipped, pending)
	// leg_unhealthy plus two passes, every one of them waiting: a spare with
	// no report has no bound.
	waits := int(c.spec.threshold.GetLegUnhealthy()) + 2
	h.waitOneSpare(t, 30*time.Second, w, msgReactionSkipped, from+waits, pending,
		"while the primary never reports the first")
	if grp := c.dataGrp(t); !slices.Contains(legIds(grp.GetLegList()), 6) {
		t.Fatalf("leg 6 left the leg list before the primary reported its spare")
	}

	setLever(t, primary, primaryKey, func(o *agentfake.Object) { o.HangSyncup = false })
	c.waitSwitched(t, w, 6, spareLeg)
}

// ---------------------------------------------------------------------------
// Thin-pool auto-grow (AR6)
// ---------------------------------------------------------------------------

// setPoolRow sets the primary's thin-pool row of the standard slice: a
// `dmsetup status` thin-pool line with used and total data blocks.
func (c *stdCluster) setPoolRow(t *testing.T, status pb.ResStatus, details string) {
	t.Helper()
	agent, key := c.cntlrAgent(t, 1)
	setRow(t, agent, key, fmt.Sprintf("slice_id_to_dm_pool.%d", stdSliceId),
		rowLever(status, details, true))
}

// dataBreach is a pool line whose data usage is past the low water mark of
// a pool with total data blocks.
func dataBreach(total uint64) string {
	return poolLine(1, 1000, total*95/100, total)
}

// growIds are the ids GrowSlice takes next: the group, then a leg and a side
// per leg.
func (c *stdCluster) growIds(t *testing.T) (uint64, [][2]uint64) {
	t.Helper()
	next := c.nextSpId(t)
	return next, [][2]uint64{{next + 1, next + 2}, {next + 3, next + 4}}
}

// TestScenGrowNeedsAnOkParsablePoolRow pins AR6's inputs: a pool row the
// primary reports ERROR is never grown, whatever its line says; an OK row
// whose line does not parse is not grown and is logged once per change of
// the line (`pool status unparsable`), not once per pass; an OK row whose
// line breaches the mark is grown. The thresholds are long enough that the
// ERROR row moves no cntlr.
func TestScenGrowNeedsAnOkParsablePoolRow(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, func(spec *stdSpec) {
		spec.threshold.PrimaryUnhealthy = 600
		spec.threshold.CntlrUnhealthy = 600
	})
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	total := c.dataGrp(t).GetDataBlocks()
	primary, primaryKey := c.cntlrAgent(t, 1)

	c.setPoolRow(t, pb.ResStatus_RES_STATUS_ERROR, dataBreach(total))
	h.waitUntil(t, 20*time.Second, "the primary's ERROR pool row to reach the worker",
		func() (bool, string) {
			epoch := c.cntlr(t, 1).GetErrEpoch()
			return epoch != 0, fmt.Sprintf("cntlr 1 err_epoch %d", epoch)
		})
	// Two rounds of the primary, a pass each, find the ERROR pool.
	h.waitRounds(t, primary, 2, primaryKey)
	growth := func(rec logRecord) bool {
		return strings.HasPrefix(rec.str("kind"), "grow_")
	}
	if n := h.countRecs(w, msgReactionApplied, growth) +
		h.countRecs(w, msgReactionSkipped, growth); n != 0 {
		t.Fatalf("%d grow records for an ERROR pool, want none", n)
	}

	unparsable := func(details string) func(logRecord) bool {
		return func(rec logRecord) bool { return rec.str("details") == details }
	}
	for _, line := range []string{"thin-pool garbage", "thin-pool 4 x/y"} {
		c.setPoolRow(t, pb.ResStatus_RES_STATUS_OK, line)
		h.waitRecord(t, 20*time.Second, w, msgPoolStatusUnparsable, unparsable(line))
		h.waitRounds(t, primary, 2, primaryKey)
		if n := h.countRecs(w, msgPoolStatusUnparsable, unparsable(line)); n != 1 {
			t.Fatalf("line %q logged unparsable %d times over two passes, "+
				"want once", line, n)
		}
	}
	if n := h.countRecs(w, msgReactionApplied, growth); n != 0 {
		t.Fatalf("%d grows of an unparsable pool, want none", n)
	}

	c.setPoolRow(t, pb.ResStatus_RES_STATUS_OK, dataBreach(total))
	h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		reactionIs(reactionGrowData, ""))
	if n := h.countRecs(w, msgReactionApplied, growth); n != 1 {
		t.Errorf("%d grows applied, want 1", n)
	}
}

// TestScenGrowStaysPendingWhileItsSidesZeroSlowly pins AR6's pending rule and
// half of AR2's rule on a refused grow: once a data grow commits, its new
// sides zero slowly and the pool reports the old total, so every pass finds
// the grow pending in its own check, logs `reaction skipped` (grow_pending),
// grows nothing more — and goes on: a standby due for replacement is
// replaced in such a pass. When the sides are provisioned and the pool
// reports the grown total, the grow is no longer pending: a breach of that
// total grows the slice once more.
func TestScenGrowStaysPendingWhileItsSidesZeroSlowly(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	total := c.dataGrp(t).GetDataBlocks()

	grpId, sides := c.growIds(t)
	for id := uint64(1); id <= uint64(c.spec.dnCnt); id++ {
		for _, ids := range sides {
			slowZeroing(t, c.dnAgent(id), agentfake.SideKey(stdSpId, ids[0], ids[1]), true)
		}
	}
	c.setPoolRow(t, pb.ResStatus_RES_STATUS_OK, dataBreach(total))
	grown := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		reactionIs(reactionGrowData, ""))
	if grown.num("grp_id") != grpId {
		t.Fatalf("the grow appended group %d, want %d", grown.num("grp_id"), grpId)
	}
	c.failCntlr(t, 2)
	replaced := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		reactionIs(reactionReplaceCntlr, ""))
	pendingInPass := h.countRecs(w, msgReactionSkipped, and(
		reactionIs(reactionGrowData, reasonGrowPending),
		func(rec logRecord) bool { return rec.traceId == replaced.traceId }))
	if pendingInPass != 1 {
		t.Errorf("the pass that replaced cntlr 2 found the grow pending %d "+
			"times, want once: the pending grow must not end the pass",
			pendingInPass)
	}
	for _, ids := range sides {
		if _, _, side, ok := c.sideOf(t, ids[1]); !ok || side.GetProvisioned() {
			t.Fatalf("new side %d while it zeroes: found %t, %v", ids[1], ok, side)
		}
	}
	h.waitCount(t, 20*time.Second, w, msgReactionSkipped, 3,
		reactionIs(reactionGrowData, reasonGrowPending))

	for id := uint64(1); id <= uint64(c.spec.dnCnt); id++ {
		for _, ids := range sides {
			slowZeroing(t, c.dnAgent(id), agentfake.SideKey(stdSpId, ids[0], ids[1]), false)
		}
	}
	h.waitUntil(t, 20*time.Second, "the new sides provisioned",
		func() (bool, string) {
			for _, ids := range sides {
				if _, _, side, ok := c.sideOf(t, ids[1]); !ok || !side.GetProvisioned() {
					return false, fmt.Sprintf("side %d: %v", ids[1], side)
				}
			}
			return true, ""
		})
	var grownTotal uint64
	for _, grp := range c.load(t).Slices[stdSliceId].GetDataGrpList() {
		grownTotal += grp.GetDataBlocks()
	}
	if n := h.countRecs(w, msgReactionApplied, reactionIs(reactionGrowData, "")); n != 1 {
		t.Fatalf("%d data grows applied before the pool reports the grown "+
			"total, want 1", n)
	}
	c.setPoolRow(t, pb.ResStatus_RES_STATUS_OK, dataBreach(grownTotal))
	again := h.waitCount(t, 20*time.Second, w, msgReactionApplied, 2,
		reactionIs(reactionGrowData, ""))
	if again[1].num("grp_id") == grpId {
		t.Errorf("the second grow names group %d again", grpId)
	}
}

// TestScenGrowWithNoDnEndsThePass pins the other half of AR2's rule for a
// grow: a grow whose scan finds fewer DNs with room than its legs logs
// `reaction skipped` (no candidate) and ends the pass, so a standby due for
// replacement is not replaced while it lasts; once a DN has room the grow
// commits, and a later pass, which finds the grow pending, replaces the
// standby.
func TestScenGrowWithNoDnEndsThePass(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	total := c.dataGrp(t).GetDataBlocks()
	ctx := context.Background()
	for _, id := range []uint64{2, 3} {
		if _, _, err := plant.SetDnFree(ctx, h.cli, c.cl, id, 0); err != nil {
			t.Fatalf("SetDnFree: %v", err)
		}
	}

	c.setPoolRow(t, pb.ResStatus_RES_STATUS_OK, dataBreach(total))
	noDn := reactionIs(reactionGrowData, reasonNoCandidate)
	h.waitRecord(t, 20*time.Second, w, msgReactionSkipped, noDn)
	c.failCntlr(t, 2)
	h.waitUntil(t, 20*time.Second, "cntlr 2 due for its replacement",
		func() (bool, string) {
			epoch := c.cntlr(t, 2).GetErrEpoch()
			return epoch != 0 && reached(uint64(time.Now().Unix()), epoch,
					c.spec.threshold.GetCntlrUnhealthy()+1),
				fmt.Sprintf("err_epoch %d", epoch)
		})
	// Two more passes in which cntlr 2 is due and the grow finds no DN.
	h.waitCount(t, 20*time.Second, w, msgReactionSkipped,
		h.countRecs(w, msgReactionSkipped, noDn)+2, noDn)
	if n := h.countRecs(w, "", reactionIs(reactionReplaceCntlr, "")); n != 0 {
		t.Fatalf("%d replace records while the grow found no DN: the grow's "+
			"empty scan must end the pass", n)
	}

	if _, _, err := plant.SetDnFree(ctx, h.cli, c.cl, 3, c.spec.nodeFree); err != nil {
		t.Fatalf("SetDnFree: %v", err)
	}
	grown := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		reactionIs(reactionGrowData, ""))
	replaced := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		reactionIs(reactionReplaceCntlr, ""))
	if replaced.traceId == grown.traceId {
		t.Errorf("the grow and the replacement ran in one pass")
	}
}

// TestScenGrowRefusedInsideItsTransactionEndsThePass pins both halves of
// AR2's rule on a refused grow in one run. A standby is due for replacement
// and no CN has room for it, so every pass that reaches AR7 logs its
// `reaction skipped` (no candidate). An operator's GrowSlice lands between
// the worker's scan for its own grow and that grow's transaction, which then
// finds the grow pending and refuses: that pass ends, with no AR7 record in
// it. The next pass finds the grow pending in its own check and goes on: its
// AR7 record is there.
func TestScenGrowRefusedInsideItsTransactionEndsThePass(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{keepWire: true})
	c := h.plantStd(t, nil)
	ctx := context.Background()
	if _, _, err := plant.SetCnFree(ctx, h.cli, c.cl, 3, 0); err != nil {
		t.Fatalf("SetCnFree: %v", err)
	}
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	total := c.dataGrp(t).GetDataBlocks()

	c.failCntlr(t, 2)
	replaceSkip := reactionIs(reactionReplaceCntlr, reasonNoCandidate)
	h.waitRecord(t, 20*time.Second, w, msgReactionSkipped, replaceSkip)

	dnRoot := commonPrefix(model.DnCapacityPrefix(c.cl.Id, 0),
		model.DnCapacityPrefix(c.cl.Id, 1))
	var fired atomic.Bool
	var scanTrace atomic.Value
	h.onRecord(func(_ context.Context, rec logRecord) {
		if rec.msg != "etcd range" || !strings.HasPrefix(rec.str("prefix"), dnRoot) ||
			!fired.CompareAndSwap(false, true) {
			return
		}
		scanTrace.Store(rec.traceId)
		if err := operatorGrow(c); err != nil {
			t.Errorf("the operator's GrowSlice inside the scan: %v", err)
		}
	})
	c.setPoolRow(t, pb.ResStatus_RES_STATUS_OK, dataBreach(total))
	refused := h.waitRecord(t, 20*time.Second, w, msgReactionSkipped,
		func(rec logRecord) bool {
			trace, _ := scanTrace.Load().(string)
			return trace != "" && rec.traceId == trace &&
				reactionIs(reactionGrowData, reasonGrowPending)(rec)
		})
	inPass := func(trace string) func(logRecord) bool {
		return func(rec logRecord) bool { return rec.traceId == trace }
	}
	next := h.waitRecord(t, 20*time.Second, w, msgReactionSkipped,
		func(rec logRecord) bool {
			return rec.traceId != refused.traceId && rec.at.After(refused.at) &&
				reactionIs(reactionGrowData, reasonGrowPending)(rec)
		})
	h.waitRecord(t, 20*time.Second, w, msgReactionSkipped,
		and(replaceSkip, inPass(next.traceId)))
	if n := h.countRecs(w, msgReactionSkipped,
		and(replaceSkip, inPass(refused.traceId))); n != 0 {
		t.Errorf("the pass whose grow transaction refused went on to AR7 "+
			"(%d records): an in-transaction refusal must end it", n)
	}
	if n := h.countRecs(w, msgReactionApplied, nil); n != 0 {
		t.Errorf("%d reactions applied, want none", n)
	}
}

// commonPrefix is the longest prefix two strings share.
func commonPrefix(a, b string) string {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return a[:n]
}

// operatorGrow is an operator's GrowSlice of the standard slice's data list,
// as the gateway runs it (architecture.md, GrowSlice): no usage gate, the
// legs on two DNs of the allocator's scan.
func operatorGrow(c *stdCluster) error {
	ctx := context.Background()
	cands, err := model.FindDnCandidates(ctx, c.h.cli, c.cl.Id, c.cl.Conf,
		2, 0, 8, nil, nil, nil)
	if err != nil {
		return err
	}
	picks := pickDistinct(cands, 2)
	if len(picks) < 2 {
		return fmt.Errorf("%d DNs with room, want 2", len(picks))
	}
	_, err = model.GrowSlice(ctx, c.h.cli, c.cl.Id, stdSpShard, stdSpId,
		c.sp.Ref.Name, 0, stdSliceId, false, math.MaxUint64, c.cl.Conf, picks)
	return err
}
