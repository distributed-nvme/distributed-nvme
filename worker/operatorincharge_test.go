package worker

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// AR8 — leg repair under a group's operator_in_charge flag (architecture.md,
// Repairs)
// ---------------------------------------------------------------------------

// TestLegRepairCreateHoldsWhileTheOperatorIsInCharge pins AR8's hold on a
// group whose operator_in_charge flag is set, through to its release: with
// the data group's leg A due, the flag holds the create on every pass, with
// no scan and no op, the record naming the group and the leg and no op; the
// pass after the operator clears the flag creates the repair.
func TestLegRepairCreateHoldsWhileTheOperatorIsInCharge(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
	h.dnCands(reactDnD)
	h.dataGrp().OperatorInCharge = true
	h.pass()
	h.wantOps()
	h.wantNoScan()
	rec := h.wantOneSkip(reactionRepairCreate, reasonGrpOperatorInCharge,
		reactDataGrp, reactDataLegA)
	if _, has := rec["op"]; has {
		t.Fatalf("hold %v names an op: the pass's own check held it", rec)
	}

	// The flag lasts: the next pass holds the create again.
	h.pass()
	h.wantOps()
	if recs := h.logs.withMsg(msgReactionSkipped); len(recs) != 2 ||
		recs[1]["reason"] != reasonGrpOperatorInCharge {
		t.Fatalf("skips = %v, want the hold again", recs)
	}

	// The operator clears the flag: the next pass creates.
	h.dataGrp().OperatorInCharge = false
	h.pass()
	calls := h.wantOps("create_repair")
	if calls[0].grpId != reactDataGrp || calls[0].badLegId != reactDataLegA ||
		calls[0].repName != h.workerRepNameOf(reactDataGrp) {
		t.Fatalf("create = %+v, want the worker's repair of leg %d of "+
			"group %d", calls[0], reactDataLegA, reactDataGrp)
	}
	h.wantApplied(reactionRepairCreate)
}

// TestLegRepairCreateRefusalUnderTheFlagHoldsOnlyItsGroup pins the second
// lock: model.CreateRepair refuses the worker's create of a group whose flag
// a pass's snapshot does not yet hold (ReasonGrpOperatorInCharge). The
// refusal is logged as the pass's own hold is, with the op's name beside
// it, and, unlike every other refusal of the create, holds that group only:
// the scan goes on, and the data group's due leg is repaired in the same
// pass.
func TestLegRepairCreateRefusalUnderTheFlagHoldsOnlyItsGroup(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.legOf(reactMetaLegA).ErrEpoch = h.ago(9000)
	h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
	h.dnCands(reactDnD)
	metaRep := h.workerRepNameOf(reactMetaGrp)
	h.rops.repairErr = map[string]error{
		metaRep: &model.ErrPrecondition{
			Op: "CreateRepair", Reason: model.ReasonGrpOperatorInCharge,
		},
	}
	h.pass()
	calls := h.wantOps("create_repair", "create_repair")
	if calls[0].repName != metaRep || calls[0].grpId != reactMetaGrp ||
		calls[1].grpId != reactDataGrp ||
		calls[1].badLegId != reactDataLegA {
		t.Fatalf("creates = %+v, want the meta group's refused and the "+
			"data group's next", calls)
	}
	h.wantSkippedBy(reactionRepairCreate, reasonGrpOperatorInCharge,
		"CreateRepair")
	skips := h.logs.withMsg(msgReactionSkipped)
	if len(skips) != 1 || skips[0]["grp_id"] != float64(reactMetaGrp) ||
		skips[0]["leg_id"] != float64(reactMetaLegA) ||
		skips[0]["rep_name"] != metaRep {
		t.Fatalf("skips = %v, want the meta group's refusal alone", skips)
	}
	rec := h.wantApplied(reactionRepairCreate)[0]
	if rec["grp_id"] != float64(reactDataGrp) {
		t.Fatalf("applied = %v, want the data group's create", rec)
	}
	if got := len(h.rops.allQueries()); got != 2 {
		t.Fatalf("scans = %d, want one per create", got)
	}
}

// TestLegRepairUnderTheFlagDrivesTheWorkersRepair pins what the flag does not
// stop (architecture.md, Repairs): the drive step still finishes a STARTED
// worker's repair of the group, cancels an ABORTED one and tries to abort a
// CREATED one whose bad leg reads healthy again; failover and the thin-pool
// grow run ahead of leg repair as ever (AR2); and the retire timer deletes
// an old retired leg of the group.
func TestLegRepairUnderTheFlagDrivesTheWorkersRepair(t *testing.T) {
	t.Run("the finish", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		repName, _ := withWorkerRepair(h, pb.RepStage_REP_STAGE_STARTED, false)
		h.dataGrp().OperatorInCharge = true
		h.rops.finishRes = model.FinishRepairResult{
			RepId: reactRepId, RetiredLegId: reactDataLegA,
		}
		h.pass()
		calls := h.wantOps("finish_repair")
		if calls[0].repName != repName {
			t.Fatalf("finish = %+v, want %s", calls[0], repName)
		}
		h.wantApplied(reactionRepairFinish)
	})

	t.Run("the cancel", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		repName, _ := withWorkerRepair(h, pb.RepStage_REP_STAGE_ABORTED, true)
		h.dataGrp().OperatorInCharge = true
		h.pass()
		calls := h.wantOps("cancel_repair")
		if calls[0].repName != repName {
			t.Fatalf("cancel = %+v, want %s", calls[0], repName)
		}
		h.wantApplied(reactionRepairCancel)
	})

	t.Run("the try-to-abort", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		repName, _ := withWorkerRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
		h.dataGrp().OperatorInCharge = true
		h.legOf(reactDataLegA).ErrEpoch = 0
		h.pass()
		calls := h.wantOps("abort_repair")
		if calls[0].repName != repName {
			t.Fatalf("abort = %+v, want %s", calls[0], repName)
		}
		rec := h.wantApplied(reactionRepairAbort)[0]
		if rec["trigger"] != triggerBadLegHealthy {
			t.Fatalf("applied = %v, want the bad leg's return", rec)
		}
	})

	t.Run("a failover", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.dataGrp().OperatorInCharge = true
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(9000)
		h.pass()
		h.wantOps("failover")
		h.wantApplied(reactionFailover)
	})

	// The pool's data usage is far over the watermark.
	t.Run("a grow", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.dataGrp().OperatorInCharge = true
		total := reactDataBlocks(t, 2)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, total, total))
		h.dnCands(reactDnC, reactDnD)
		h.pass()
		h.wantOps("grow")
		h.wantApplied(reactionGrowData)
	})

	t.Run("the retire timer", func(t *testing.T) {
		h := retireCase(t)
		h.dataGrp().OperatorInCharge = true
		h.pass()
		h.wantOps("delete_retired_leg")
		if recs := h.logs.withMsg(msgRetiredLegDeleted); len(recs) != 1 ||
			recs[0]["leg_id"] != float64(retireLegA) {
			t.Fatalf("deleted records = %v, want leg %d", recs, retireLegA)
		}
	})
}

// TestLegRepairOperatorTakeOver pins the worker's side of an operator's
// take-over of a group (architecture.md, Repairs), pass by pass: the flag is
// set over the worker's CREATED repair and the operator aborts it, which
// leaves the repair to the primary's verdict and AR11's stage move; the
// drive step cancels the ABORTED repair; from then on the flag holds the
// create of the group's still due leg, also while the operator's own repair
// stands, which the worker drives not at all (AR9); and once the operator
// has finished it and cleared the flag, a leg of the group that goes bad
// later gets the worker's repair.
func TestLegRepairOperatorTakeOver(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.dnCands(reactDnD)
	repName, rep := withWorkerRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
	h.dataGrp().OperatorInCharge = true

	// The operator's abort committed: the flag is set, and the repair waits
	// for the primary's verdict.
	rep.TryToAbort = true
	h.pass()
	h.wantOps()
	if recs := h.logs.withMsg(msgReactionSkipped); len(recs) != 1 ||
		recs[0]["reason"] != reasonGrpOperatorInCharge {
		t.Fatalf("skips = %v, want the flag's hold", recs)
	}

	// The primary confirms the abort: AR11 moves it, and the next pass's
	// drive step cancels it.
	h.setVerdict(reactRepId, false, true)
	h.pass()
	h.wantOps("set_repair_stage")
	rep.RepStage = pb.RepStage_REP_STAGE_ABORTED
	h.pass()
	h.wantOps("set_repair_stage", "cancel_repair")
	h.wantApplied(reactionRepairCancel)

	// The cancel committed: the group has no repair, and its leg A is still
	// due, so only the flag holds the create.
	delete(h.state.Repairs, repName)
	h.state.Conf.RepairNameList = nil
	h.dataGrp().RepName = ""
	h.pass()
	h.wantOps("set_repair_stage", "cancel_repair")
	if recs := h.logs.withMsg(msgReactionSkipped); len(recs) != 3 ||
		recs[2]["reason"] != reasonGrpOperatorInCharge {
		t.Fatalf("skips = %v, want the flag's hold", recs)
	}

	// The operator's own repair of leg A stands: the flag, checked ahead of
	// the group's repair, still names the hold, and the worker drives
	// nothing of an operator's repair (AR9).
	withReactRepair(h, pb.RepStage_REP_STAGE_STARTED, false)
	h.pass()
	h.wantOps("set_repair_stage", "cancel_repair")
	if recs := h.logs.withMsg(msgReactionSkipped); len(recs) != 4 ||
		recs[3]["reason"] != reasonGrpOperatorInCharge {
		t.Fatalf("skips = %v, want the flag's hold", recs)
	}

	// The operator finishes it, so the group's legs are healthy, and clears
	// the flag; leg B goes bad later, and the worker repairs it.
	delete(h.state.Repairs, reactRepName)
	h.state.Conf.RepairNameList = nil
	h.dataGrp().RepName = ""
	h.dataGrp().OperatorInCharge = false
	h.legOf(reactDataLegA).ErrEpoch = 0
	h.legOf(reactDataLegB).ErrEpoch = h.ago(common.DefaultLegUnhealthy)
	h.pass()
	calls := h.wantOps("set_repair_stage", "cancel_repair", "create_repair")
	if calls[2].grpId != reactDataGrp || calls[2].badLegId != reactDataLegB {
		t.Fatalf("create = %+v, want leg %d of group %d", calls[2],
			reactDataLegB, reactDataGrp)
	}
}
