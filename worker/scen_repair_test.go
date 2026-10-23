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

// The whole-worker scenario cases of an operator's repair (architecture.md,
// Repair; dnv-worker.md, AR11, AR12), played through the fakes of one planted
// cluster under one whole worker: the fakes' repairs lever stands for the
// primary's verdict, their grps lever for its reading of a group, and the
// model operations the gateway runs stand for the operator's calls.

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
// it; the group's due leg gets no spare while the repair is open (AR8).
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
		reactionIs(reactionSpareCreate, reasonGrpHasRepair))

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
	_, retiredId, err := model.FinishRepair(ctx, h.cli, c.cl.Id, stdSpShard,
		stdSpId, c.sp.Ref.Name, 0, stdRepName, uint64(time.Now().Unix()))
	if err != nil {
		t.Fatalf("FinishRepair: %v", err)
	}
	if retiredId != 6 {
		t.Fatalf("retired leg %d, want the bad leg 6", retiredId)
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
