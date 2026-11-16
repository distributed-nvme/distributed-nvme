package worker

import (
	"errors"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// setWaits installs the primary's verdict on the fixture repair, neither
// started nor abort confirmed, with text as its waits (cnagent.md CN12, Why
// a repair waits).
func (h *reactHarness) setWaits(text string) {
	h.t.Helper()
	h.setVerdict(reactRepId, false, false)
	h.w.cntlrs[reactCntlrA].driver.lastInfo.RepIdToRepairInfo[reactRepId].
		Waits = text
}

// wantWaitRecords asserts the reasons of every `repair waits` record so far,
// in order, and that each names the fixture repair.
func (h *reactHarness) wantWaitRecords(stage pb.RepStage, want ...string) {
	h.t.Helper()
	recs := h.logs.withMsg(msgRepairWaits)
	if len(recs) != len(want) {
		h.t.Fatalf("repair waits records %v, want reasons %q", recs, want)
	}
	for idx, rec := range recs {
		if rec["waits"] != want[idx] || rec["rep_name"] != reactRepName {
			h.t.Fatalf("repair waits record %d: %v, want reason %q", idx,
				rec, want[idx])
		}
	}
	if len(recs) > 0 && recs[len(recs)-1]["stage"] != stage.String() {
		h.t.Fatalf("the last repair waits record %v, want stage %s",
			recs[len(recs)-1], stage)
	}
}

// TestRepairWaitsLoggedOncePerChange pins AR17's memory: the primary's
// reason for holding a repair back is logged once, not once per pass, and
// again only when the text, the repair's stage or its rep_id changes; a
// pass that finds no reason, or that has no report of the primary, keeps
// the last record, so a text that comes back after it is not logged again.
func TestRepairWaitsLoggedOncePerChange(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	rep := withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
	created := pb.RepStage_REP_STAGE_CREATED
	const x, y = "new leg 0x385 is not available",
		"bad leg 0x6e holds its place in the array and serves"
	h.setWaits(x)
	h.pass()
	h.pass()
	h.wantWaitRecords(created, x)
	h.setWaits(y)
	h.pass()
	h.pass()
	h.wantWaitRecords(created, x, y)
	h.setWaits("")
	h.pass()
	h.setWaits(y)
	h.pass()
	h.wantWaitRecords(created, x, y)
	primary := h.w.cntlrs[reactCntlrA]
	info := primary.driver.lastInfo
	primary.driver.lastInfo = nil
	h.pass()
	primary.driver.lastInfo = info
	h.pass()
	h.wantWaitRecords(created, x, y)
	// The repair created again under the same name, a new rep_id.
	rep.RepId = reactRepId + 50
	h.setVerdict(rep.RepId, false, false)
	primary.driver.lastInfo.RepIdToRepairInfo[rep.RepId].Waits = y
	h.pass()
	h.wantWaitRecords(created, x, y, y)
	rep.RepStage = pb.RepStage_REP_STAGE_STARTED
	h.pass()
	h.wantWaitRecords(pb.RepStage_REP_STAGE_STARTED, x, y, y, y)
}

// TestRepairWaitsReason pins AR17's reason and reach: a CREATED repair with
// no verdict, or one with no waits text, waits for its group's row when that
// reads other than OK, the row's status and details its reason; a STARTED
// repair is never held by the row; a verdict that says started or abort
// confirmed logs nothing, even when the move it asks for failed or was
// refused; the record runs at a level where reactions stop; and a deleting
// clone's repair logs nothing.
func TestRepairWaitsReason(t *testing.T) {
	broken := func(h *reactHarness) {
		h.setGrpRow(reactDataGrp, pb.ResStatus_RES_STATUS_ERROR)
		h.w.cntlrs[reactCntlrA].driver.lastInfo.
			GrpIdToMdRaid[reactDataGrp].Details = "array broken"
	}
	t.Run("a created repair, the row in error", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
		broken(h)
		h.pass()
		h.wantWaitRecords(pb.RepStage_REP_STAGE_CREATED,
			"the group's row reads RES_STATUS_ERROR: array broken")
	})
	t.Run("a started repair, the row in error", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withReactRepair(h, pb.RepStage_REP_STAGE_STARTED, false)
		broken(h)
		h.pass()
		h.wantWaitRecords(pb.RepStage_REP_STAGE_STARTED)
	})
	t.Run("a level where reactions stop", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
		h.state.Conf.SpLevel = pb.SpLevel_SP_LEVEL_NO_THINPOOL
		h.setWaits("new leg 0x385 is not available")
		h.pass()
		h.wantWaitRecords(pb.RepStage_REP_STAGE_CREATED,
			"new leg 0x385 is not available")
	})
	t.Run("an abort confirmed on a clear flag", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
		broken(h)
		h.setVerdict(reactRepId, false, true)
		h.pass()
		if recs := h.logs.withMsg(msgRepairStageSkipped); len(recs) != 1 ||
			recs[0]["reason"] != reasonAbortFlagClear {
			t.Fatalf("stage skips %v, want one %s", recs,
				reasonAbortFlagClear)
		}
		h.wantWaitRecords(pb.RepStage_REP_STAGE_CREATED)
	})
	t.Run("a deleting clone's repair", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withReactClone(t, h.state)
		h.state.Clones[reactCloneNm].Deleting = true
		h.state.Clones[reactCloneNm].MetaGrp.RepName = reactRepName
		h.state.Repairs = map[string]*pb.Repair{reactRepName: {
			RepId:        reactRepId,
			GrpId:        reactCloneGrp,
			RepStage:     pb.RepStage_REP_STAGE_CREATED,
			BadLegIdList: []uint64{reactCloneLegA},
			CloneName:    reactCloneNm,
		}}
		h.state.Conf.RepairNameList = []string{reactRepName}
		h.setWaits("new leg 0x385 is not available")
		h.pass()
		h.wantWaitRecords(pb.RepStage_REP_STAGE_CREATED)
	})
	t.Run("a verdict that starts the repair", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
		broken(h)
		h.setVerdict(reactRepId, true, false)
		// The move fails, so the pass did not move the repair.
		h.rops.stageErr = errors.New("etcd down")
		h.pass()
		h.wantWaitRecords(pb.RepStage_REP_STAGE_CREATED)
	})
}
