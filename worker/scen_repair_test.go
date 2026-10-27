package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/integtest/plant"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The whole-worker scenario cases of an operator's repair, forced start and
// take-over of a group (architecture.md, Repair; Repairs; dnv-worker.md,
// AR8, AR11, AR12, AR13), played through the fakes of one planted cluster
// under one whole worker: the fakes' repairs lever stands for the primary's
// verdict, their grps lever for its reading of a group, and the model
// operations the gateway runs stand for the operator's calls.

// stdRepName is the repair the cases plant on the standard data group: its
// bad leg is leg 6, whose side 10 is on dn 1, and its fresh new leg goes to
// dn 3, which holds nothing of the pool.
const stdRepName = "r0"

// putStdRepair plants the repair stdRepName with an unprovisioned new leg
// on dn 3, as CreateRepair leaves it.
func (c *stdCluster) putStdRepair(t *testing.T) plant.Repair {
	t.Helper()
	rep, err := plant.PutRepair(context.Background(), c.h.cli, c.cl, c.sp.Ref,
		plant.RepairSpec{
			Name: stdRepName, GrpId: stdDataGrpId, BadLegId: 6, Dn: 3,
			Slot: 0,
		})
	if err != nil {
		t.Fatalf("PutRepair: %v", err)
	}
	return rep
}

// repair reads the repair repName, nil when the pool holds none.
func (c *stdCluster) repair(t *testing.T, repName string) *pb.Repair {
	t.Helper()
	return c.load(t).Repairs[repName]
}

// setVerdictLever sets the primary cntlr 1's verdict on the repair repName.
func (c *stdCluster) setVerdictLever(
	t *testing.T, repName string, verdict agentfake.RepairVerdict,
) {
	t.Helper()
	agent, key := c.cntlrAgent(t, 1)
	setLever(t, agent, key, func(o *agentfake.Object) {
		if o.Repairs == nil {
			o.Repairs = make(map[string]agentfake.RepairVerdict)
		}
		o.Repairs[repName] = verdict
	})
}

// TestScenOperatorRepairToFinish pins a repair's path to its finish through a
// whole worker: the new leg's side is driven and zeroed on its disk node and
// flipped provisioned in the repair (RW18), the new leg is carried to every
// cntlr, the primary's verdict started moves the repair to STARTED (AR11),
// which every cntlr and the new leg's side are handed at the move's
// revision (RW14, RW16), and once the operator finishes it, the new leg's
// side is driven as a side of its slice and the bad leg's side, retired,
// keeps its request on its disk node, provisioned, with no cntlr connecting
// it; the group's due leg gets no repair of the worker's while the
// operator's is open, since a group has one repair at a time (AR8).
func TestScenOperatorRepairToFinish(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	ctx := context.Background()

	rep := c.putStdRepair(t)
	h.waitUntil(t, 20*time.Second, "the new side provisioned in the repair",
		func() (bool, string) {
			r := c.repair(t, stdRepName)
			side := r.GetNewLegList()[0].GetSideList()[0]
			return side.GetProvisioned(), "not yet"
		})
	h.waitRecord(t, 20*time.Second, w, msgFlipApplied,
		func(rec logRecord) bool {
			return rec.str("rep_name") == stdRepName &&
				rec.num("side_id") == rep.SideId
		})
	h.waitUntil(t, 20*time.Second, "every cntlr carries the repair",
		func() (bool, string) {
			for cntlrId := uint64(1); cntlrId <= 2; cntlrId++ {
				agent, key := c.cntlrAgent(t, cntlrId)
				req, _ := agent.Stored(key).(*pb.SyncupCntlrRequest)
				got := req.GetNameToRepair()[stdRepName]
				if got.GetRepId() != rep.Id ||
					!got.GetNewLegList()[0].GetSideList()[0].GetProvisioned() {
					return false, fmt.Sprintf("cntlr %d keeps %v", cntlrId, got)
				}
			}
			return true, ""
		})

	// The bad leg reads ERROR on the primary: its group has a repair, so leg
	// repair leaves it to the repair.
	agent, key := c.cntlrAgent(t, 1)
	setRow(t, agent, key, fmt.Sprintf("leg_id_to_leg.%d", 6),
		rowLever(pb.ResStatus_RES_STATUS_ERROR, "probe io error", true))
	h.waitCount(t, 20*time.Second, w, msgReactionSkipped, 2,
		reactionIs(reactionRepairCreate, reasonGrpHasRepair))

	c.setVerdictLever(t, stdRepName, agentfake.RepairVerdict{Started: true})
	h.waitRecord(t, 20*time.Second, w, msgRepairStageMoved,
		func(rec logRecord) bool {
			return rec.str("rep_name") == stdRepName &&
				rec.num("rep_id") == rep.Id &&
				rec.str("stage") == pb.RepStage_REP_STAGE_STARTED.String()
		})
	if got := c.repair(t, stdRepName).GetRepStage(); got !=
		pb.RepStage_REP_STAGE_STARTED {
		t.Fatalf("repair at %s after the move", got)
	}
	// The move's bump reaches every cntlr, which carries the repair at its
	// new stage (RW16), and the new leg's side, which keeps its request
	// past CREATED (RW14).
	h.waitUntil(t, 20*time.Second, "the move carried to the cntlrs and "+
		"the new side", func() (bool, string) {
		for cntlrId := uint64(1); cntlrId <= 2; cntlrId++ {
			agent, key := c.cntlrAgent(t, cntlrId)
			req, _ := agent.Stored(key).(*pb.SyncupCntlrRequest)
			got := req.GetNameToRepair()[stdRepName]
			if got == nil ||
				got.GetRepStage() != pb.RepStage_REP_STAGE_STARTED {
				return false, fmt.Sprintf("cntlr %d keeps %v", cntlrId, got)
			}
		}
		spRev := c.load(t).SpRevision
		newReq, _ := c.dnAgent(3).Stored(agentfake.SideKey(
			stdSpId, rep.LegId, rep.SideId)).(*pb.SyncupSideRequest)
		if newReq.GetRevision() != spRev {
			return false, fmt.Sprintf("the new side keeps %v at SpRev %d",
				newReq, spRev)
		}
		return true, ""
	})

	// The operator finishes the repair.
	clearRow(t, agent, key, fmt.Sprintf("leg_id_to_leg.%d", 6))
	res, err := model.FinishRepair(ctx, h.cli, c.cl.Id, stdSpShard,
		stdSpId, c.sp.Ref.Name, 0, stdRepName, uint64(time.Now().Unix()),
		c.cl.Conf)
	if err != nil {
		t.Fatalf("FinishRepair: %v", err)
	}
	if res.RetiredLegId != 6 {
		t.Fatalf("retired leg %d, want the bad leg 6", res.RetiredLegId)
	}
	c.waitConverged(t)
	state := c.load(t)
	if _, ok := state.RetiredLegs[6]; !ok {
		t.Fatalf("the bad leg is no retired leg after the finish")
	}
	retiredReq, _ := c.dnAgent(1).Stored(
		agentfake.SideKey(stdSpId, 6, 10)).(*pb.SyncupSideRequest)
	if retiredReq.GetRevision() != state.SpRevision ||
		!retiredReq.GetSideConf().GetProvisioned() {
		t.Fatalf("the retired side keeps %v, want a provisioned request "+
			"at SpRev %d", retiredReq, state.SpRevision)
	}
	newReq, _ := c.dnAgent(3).Stored(
		agentfake.SideKey(stdSpId, rep.LegId, rep.SideId)).(*pb.SyncupSideRequest)
	if newReq.GetRevision() != state.SpRevision {
		t.Fatalf("the new leg's side keeps %v at SpRev %d", newReq,
			state.SpRevision)
	}
	for cntlrId := uint64(1); cntlrId <= 2; cntlrId++ {
		agent, key := c.cntlrAgent(t, cntlrId)
		req, _ := agent.Stored(key).(*pb.SyncupCntlrRequest)
		if len(req.GetNameToRepair()) != 0 {
			t.Fatalf("cntlr %d still carries a repair after the finish",
				cntlrId)
		}
	}
}

// TestScenOperatorRepairToCancel pins a repair's abort through a whole
// worker: the operator's abort sets the flag, the primary's verdict abort
// confirmed moves the repair to ABORTED (AR11), and the operator's cancel
// gives the new leg's side back to its disk node.
func TestScenOperatorRepairToCancel(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	ctx := context.Background()
	freeBefore := c.dnConf(t, 3).GetFreeExtCnt()

	rep := c.putStdRepair(t)
	if _, err := model.SetRepairAbort(ctx, h.cli, c.cl.Id, stdSpShard,
		stdSpId, c.sp.Ref.Name, 0, stdRepName); err != nil {
		t.Fatalf("SetRepairAbort: %v", err)
	}
	c.setVerdictLever(t, stdRepName,
		agentfake.RepairVerdict{AbortConfirmed: true})
	h.waitRecord(t, 20*time.Second, w, msgRepairStageMoved,
		func(rec logRecord) bool {
			return rec.num("rep_id") == rep.Id &&
				rec.str("stage") == pb.RepStage_REP_STAGE_ABORTED.String()
		})
	if _, err := model.CancelRepair(ctx, h.cli, c.cl.Id, stdSpShard,
		stdSpId, c.sp.Ref.Name, 0, stdRepName, c.cl.Conf); err != nil {
		t.Fatalf("CancelRepair: %v", err)
	}
	c.waitConverged(t)
	if c.repair(t, stdRepName) != nil {
		t.Fatalf("the repair outlived its cancel")
	}
	if got := c.dnConf(t, 3).GetFreeExtCnt(); got != freeBefore {
		t.Fatalf("dn 3 holds %d free extents, want %d back", got, freeBefore)
	}
}

// TestScenRetireTimer pins AR12 through a whole worker: an old retired leg
// of a group the primary reads not whole is kept, logged with the hold; once
// the primary reads the group whole, with its rows OK and its legs healthy,
// the timer deletes the retired leg and its side's extents go back to its
// disk node.
func TestScenRetireTimer(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	agent, key := c.cntlrAgent(t, 1)
	notWhole := false
	setLever(t, agent, key, func(o *agentfake.Object) {
		o.Grps = map[uint64]agentfake.GrpReading{
			stdMetaGrpId: {Whole: &notWhole},
		}
	})
	freeBefore := c.dnConf(t, 3).GetFreeExtCnt()
	retired, err := plant.PutRetiredLeg(context.Background(), h.cli, c.cl,
		c.sp.Ref, plant.RetiredLegSpec{
			GrpId: stdMetaGrpId, LegIdx: 2, Dn: 3, Slot: 0,
			Epoch: uint64(time.Now().Unix()) - common.RetiredLegMaxAge - 60,
		})
	if err != nil {
		t.Fatalf("PutRetiredLeg: %v", err)
	}
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	h.waitCount(t, 20*time.Second, w, msgRetiredLegKept, 2,
		func(rec logRecord) bool {
			return rec.num("leg_id") == retired.LegId &&
				rec.str("reason") == reasonGrpNotWhole
		})
	if _, ok := c.load(t).RetiredLegs[retired.LegId]; !ok {
		t.Fatalf("the timer deleted a retired leg of a group not whole")
	}
	setLever(t, agent, key, func(o *agentfake.Object) { o.Grps = nil })
	h.waitRecord(t, 20*time.Second, w, msgRetiredLegDeleted,
		numIs("leg_id", retired.LegId))
	c.waitConverged(t)
	if _, ok := c.load(t).RetiredLegs[retired.LegId]; ok {
		t.Fatalf("the retired leg outlived the timer's delete")
	}
	if got := c.dnConf(t, 3).GetFreeExtCnt(); got != freeBefore {
		t.Fatalf("dn 3 holds %d free extents, want %d back", got, freeBefore)
	}
}

// requestMark is the forced-start mark of the group grpId in a cntlr's
// request, among its slices' groups; nil when the request holds none.
func requestMark(req *pb.SyncupCntlrRequest, grpId uint64) *pb.ForcedStart {
	for _, slice := range req.GetIdToSlice() {
		for _, grp := range allGroups(slice) {
			if grp.GetGrpId() == grpId {
				return grp.GetForcedStart()
			}
		}
	}
	return nil
}

// TestScenForcedStartHoldsLegRepairUntilItsClear pins an operator's forced
// start through a whole worker: the mark the gateway's operation writes
// reaches every cntlr in its group's slice at the mark's revision (RW14);
// while the primary holds no leg in sync the mark stands, and leg repair
// holds the group's due leg, the one the mark names, with grp_forced_start,
// creating no repair, though the group's row reads OK, the hold coming
// before the healthy-leg test (AR8); once the primary holds the other leg in
// sync on a leg that serves, the named leg still out of sync, a report of an
// observation after the mark's revision, the pool bumped in between, clears
// the mark (AR13), the op handed the mark's own revision, whose bump hands
// every cntlr the group without it, and leg repair then creates its repair
// of the due leg.
func TestScenForcedStartHoldsLegRepairUntilItsClear(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantScen(t, repairShape)
	agent, key := c.cntlrAgent(t, 1)
	// The primary holds no leg in sync, so a mark naming leg 6 stands.
	setLever(t, agent, key, func(o *agentfake.Object) {
		o.Grps = map[uint64]agentfake.GrpReading{
			stdDataGrpId: {InSync: &[]uint64{}},
		}
	})
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	ctx := context.Background()

	revision, spRev, err := plant.ForceStartGroup(ctx, h.cli, c.cl, c.sp.Ref,
		stdDataGrpId, 6, false)
	if err != nil {
		t.Fatalf("ForceStartGroup: %v", err)
	}
	if revision == 0 || spRev != revision {
		t.Fatalf("the mark at %d, the pool at %d: want the mark's own "+
			"revision", revision, spRev)
	}
	h.waitUntil(t, 20*time.Second, "every cntlr carries the mark",
		func() (bool, string) {
			for cntlrId := uint64(1); cntlrId <= 2; cntlrId++ {
				agent, key := c.cntlrAgent(t, cntlrId)
				req, _ := agent.Stored(key).(*pb.SyncupCntlrRequest)
				mark := requestMark(req, stdDataGrpId)
				if mark.GetLegId() != 6 || mark.GetRevision() != revision {
					return false, fmt.Sprintf("cntlr %d keeps %v", cntlrId,
						mark)
				}
			}
			return true, ""
		})

	// Leg 6 fails on the primary and is due: the mark holds its repair,
	// ahead of the healthy-leg test, which leg 7 out of sync would hold.
	c.failLeg(t, 6)
	h.waitCount(t, 20*time.Second, w, msgReactionSkipped, 2,
		and(reactionIs(reactionRepairCreate, reasonGrpForcedStart),
			numIs("leg_id", 6)))
	if n := h.creates(w); n != 0 {
		t.Fatalf("%d repairs created while the mark stands", n)
	}
	if n := h.countRecs(w, msgReactionSkipped,
		reactionIs(reactionRepairCreate, reasonNoHealthyLeg)); n != 0 {
		t.Fatalf("%d passes reached the healthy-leg test while the mark "+
			"stands", n)
	}
	if n := h.countRecs(w, msgForcedStartCleared, nil); n != 0 {
		t.Fatalf("%d clears while the primary holds no leg in sync", n)
	}
	if mark := c.dataGrp(t).GetForcedStart(); mark.GetLegId() != 6 {
		t.Fatalf("the data group's mark is %v while the start is not done",
			mark)
	}

	// The pool's revision moves past the mark's before the primary holds
	// leg 7 in sync, so the report that does is of a later revision than
	// the mark's, which the clear's op must not be handed in its place.
	bumped, err := plant.BumpRev(ctx, h.cli, c.cl, plant.RevSp,
		stdSpShard, stdSpId)
	if err != nil {
		t.Fatalf("bump the pool: %v", err)
	}
	h.waitUntil(t, 20*time.Second, "the primary carries the bumped revision",
		func() (bool, string) {
			req, _ := agent.Stored(key).(*pb.SyncupCntlrRequest)
			return req.GetRevision() >= bumped, fmt.Sprintf(
				"the primary at revision %d", req.GetRevision())
		})

	// The primary holds leg 7 in sync, on a leg whose row reads OK, and leg
	// 6 still out of sync: the array runs on a member that serves, so the
	// mark is cleared, and leg 6 gets its repair.
	setLever(t, agent, key, func(o *agentfake.Object) {
		o.Grps = map[uint64]agentfake.GrpReading{
			stdDataGrpId: {InSync: &[]uint64{7}},
		}
	})
	cleared := h.waitRecord(t, 20*time.Second, w, msgForcedStartCleared,
		and(numIs("grp_id", stdDataGrpId), numIs("leg_id", 6)))
	if cleared.num("mark_revision") != revision ||
		cleared.num("report_revision") < bumped ||
		cleared.num("revision") <= bumped {
		t.Fatalf("the clear %v, want the mark at %d cleared from a report "+
			"at %d or after, at a revision of its own", cleared.attrs,
			revision, bumped)
	}
	if mark := c.dataGrp(t).GetForcedStart(); mark != nil {
		t.Fatalf("the mark %v outlived its clear", mark)
	}
	h.waitCreated(t, w, 6)
	h.waitUntil(t, 20*time.Second, "every cntlr carries the group with no "+
		"mark", func() (bool, string) {
		for cntlrId := uint64(1); cntlrId <= 2; cntlrId++ {
			agent, key := c.cntlrAgent(t, cntlrId)
			req, _ := agent.Stored(key).(*pb.SyncupCntlrRequest)
			if req.GetRevision() < cleared.num("revision") {
				return false, fmt.Sprintf("cntlr %d at revision %d",
					cntlrId, req.GetRevision())
			}
			if mark := requestMark(req, stdDataGrpId); mark != nil {
				return false, fmt.Sprintf("cntlr %d keeps %v", cntlrId, mark)
			}
		}
		return true, ""
	})
}

// TestScenOperatorInChargeHoldsLegRepairUntilCleared pins an operator's
// take-over of a group through a whole worker (architecture.md, Repairs):
// while the data group's operator_in_charge flag, written by the operation
// the gateway runs, stands, leg repair holds the group's due leg with
// grp_operator_in_charge, creating no repair, though every other condition
// of the create holds (AR8); once the operator clears the flag the worker
// creates its repair of the leg. With the flag set again over that CREATED
// repair, the drive step still runs: the bad leg reads healthy again, so the
// worker tries to abort its repair, and once the primary confirms the abort
// it cancels it, and no second repair is created.
func TestScenOperatorInChargeHoldsLegRepairUntilCleared(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantScen(t, repairShape)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	ctx := context.Background()
	setFlag := func(on bool) {
		t.Helper()
		if _, err := plant.SetOperatorInCharge(ctx, h.cli, c.cl, c.sp.Ref,
			stdDataGrpId, on); err != nil {
			t.Fatalf("SetOperatorInCharge(%t): %v", on, err)
		}
		if got := c.dataGrp(t).GetOperatorInCharge(); got != on {
			t.Fatalf("the data group's flag is %t, want %t", got, on)
		}
	}

	// Leg 6 fails on the primary and is due: the pass's own hold, which
	// names no op, holds its repair.
	setFlag(true)
	c.failLeg(t, 6)
	h.waitCount(t, 20*time.Second, w, msgReactionSkipped, 2,
		and(reactionIs(reactionRepairCreate, reasonGrpOperatorInCharge),
			and(numIs("leg_id", 6), func(rec logRecord) bool {
				_, op := rec.attrs["op"]
				return !op
			})))
	if n := h.creates(w); n != 0 {
		t.Fatalf("%d repairs created while the flag stands", n)
	}
	if rep := c.dataGrp(t).GetRepName(); rep != "" {
		t.Fatalf("the data group names the repair %s under the flag", rep)
	}

	// The operator gives the group back: the worker repairs leg 6.
	setFlag(false)
	created := h.waitCreated(t, w, 6)
	repName := created.str("rep_name")

	// The flag again, over the worker's CREATED repair: leg 6 reads healthy
	// again, and the drive step tries to abort the repair and, once the
	// primary confirms, cancels it.
	setFlag(true)
	c.healLeg(t, 6)
	aborted := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		func(rec logRecord) bool {
			return rec.str("kind") == reactionRepairAbort &&
				rec.str("rep_name") == repName
		})
	if aborted.str("trigger") != triggerBadLegHealthy {
		t.Fatalf("the try-to-abort %v, want trigger %s", aborted.attrs,
			triggerBadLegHealthy)
	}
	c.cancelByVerdict(t, w, repName)
	if n := h.creates(w); n != 1 {
		t.Errorf("%d repairs created, want 1", n)
	}
	if !c.dataGrp(t).GetOperatorInCharge() {
		t.Errorf("the flag went with the cancel")
	}
}

// TestScenTakeBackRefusalIsLogged pins AR11's record of the primary's refusal
// of a take-back through a whole worker: an operator's repair whose new leg
// is a retired leg of its group, whose add the primary's verdict refuses,
// stays CREATED, and every pass logs `repair stage skipped` with
// take_back_refused and the verdict's text; once the verdict says started,
// the repair moves to STARTED.
func TestScenTakeBackRefusalIsLogged(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	ctx := context.Background()
	retired, err := plant.PutRetiredLeg(ctx, h.cli, c.cl, c.sp.Ref,
		plant.RetiredLegSpec{
			GrpId: stdDataGrpId, LegIdx: 2, Dn: 3, Slot: 0,
			Epoch: uint64(time.Now().Unix()),
		})
	if err != nil {
		t.Fatalf("PutRetiredLeg: %v", err)
	}
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	rep, err := plant.PutRepair(ctx, h.cli, c.cl, c.sp.Ref, plant.RepairSpec{
		Name: stdRepName, GrpId: stdDataGrpId, BadLegId: 6,
		RetiredLegId: retired.LegId,
	})
	if err != nil {
		t.Fatalf("PutRepair: %v", err)
	}
	refusal := fmt.Sprintf("leg %#x taken from a retired leg holds newer "+
		"data than the array", retired.LegId)
	c.setVerdictLever(t, stdRepName, agentfake.RepairVerdict{Refusal: refusal})
	h.waitCount(t, 20*time.Second, w, msgRepairStageSkipped, 2,
		func(rec logRecord) bool {
			return rec.str("rep_name") == stdRepName &&
				rec.num("rep_id") == rep.Id &&
				rec.str("reason") == reasonTakeBackRefused &&
				rec.str("refusal") == refusal
		})
	if got := c.repair(t, stdRepName).GetRepStage(); got !=
		pb.RepStage_REP_STAGE_CREATED {
		t.Fatalf("a refused repair at %s", got)
	}
	if n := h.countRecs(w, msgRepairStageMoved, nil); n != 0 {
		t.Fatalf("%d stage moves on a refused repair", n)
	}

	c.setVerdictLever(t, stdRepName, agentfake.RepairVerdict{Started: true})
	h.waitRecord(t, 20*time.Second, w, msgRepairStageMoved,
		func(rec logRecord) bool {
			return rec.num("rep_id") == rep.Id &&
				rec.str("stage") == pb.RepStage_REP_STAGE_STARTED.String()
		})
}
