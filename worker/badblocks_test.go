package worker

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// setRanges makes the primary's latest reading of the data group name md
// bad-block ranges on the legs given (leg_id_to_bad_blocks; cnagent.md
// CN12, The md check).
func setRanges(h *reactHarness, legIds ...uint64) {
	h.t.Helper()
	h.setGrpInfo(reactDataGrp, true, reactDataLegA, reactDataLegB)
	reading := h.w.cntlrs[reactCntlrA].driver.lastInfo.
		GrpIdToGrpInfo[reactDataGrp]
	reading.ForcedStartRevision = 0
	reading.LegIdToBadBlocks = map[uint64]uint32{}
	for _, legId := range legIds {
		reading.LegIdToBadBlocks[legId] = 1
	}
}

// checkDone makes the data group's md check request stand and the primary's
// reading name it done (md_check_done_epoch; dnv-worker.md AR16).
func checkDone(h *reactHarness) {
	h.t.Helper()
	req := h.ago(60)
	h.dataGrp().MdCheck = &pb.MdCheck{NextEpoch: h.ago(100),
		RequestEpoch: req}
	h.w.cntlrs[reactCntlrA].driver.lastInfo.
		GrpIdToGrpInfo[reactDataGrp].MdCheckDoneEpoch = req
}

// TestLegRepairOfALegWithRanges pins AR8 on md's bad-block ranges: a leg
// whose member carries ranges, with no health epoch and an OK row, is a
// candidate of its own and is repaired, the create saying so, while another
// leg is healthy, held in sync and clean of ranges, once the group's md
// check has run to its end; with every other leg carrying ranges too it is
// no candidate, with no record, since a copy would put the same ranges on
// the new leg; and a dead leg beside a leg with ranges is repaired from it
// as from any healthy leg, its ranges notwithstanding.
func TestLegRepairOfALegWithRanges(t *testing.T) {
	t.Run("the other leg clean", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		setRanges(h, reactDataLegA)
		checkDone(h)
		h.dnCands(reactDnD)
		h.pass()
		calls := h.wantOps("create_repair")
		if calls[0].badLegId != reactDataLegA || !calls[0].badBlocks {
			t.Fatalf("create_repair = %+v, want leg A for its ranges",
				calls[0])
		}
	})
	t.Run("the other leg carries ranges too", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		setRanges(h, reactDataLegA, reactDataLegB)
		h.dnCands(reactDnD)
		h.pass()
		h.wantOps()
		if recs := h.logs.withMsg(msgReactionSkipped); len(recs) != 0 {
			t.Fatalf("skips %v, want none: no leg is a candidate", recs)
		}
	})
	t.Run("a dead leg beside a leg with ranges", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		setRanges(h, reactDataLegB)
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnD)
		h.pass()
		calls := h.wantOps("create_repair")
		if calls[0].badLegId != reactDataLegA || calls[0].badBlocks {
			t.Fatalf("create_repair = %+v, want leg A for its death",
				calls[0])
		}
	})
}

// TestLegRepairOfRangesAborts pins AR8's try-to-abort of a repair of a leg
// for its ranges: while the reading names the ranges and another leg is
// clean, the bad leg's clear health epoch asks nothing; once the clean leg
// gains ranges too, or the reading names none on the bad leg, the repair is
// asked to abort as one whose bad leg came back; and a pass whose primary's
// report holds no current reading of the group's running array asks nothing,
// as it reads no ranges.
func TestLegRepairOfRangesAborts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ranges []uint64
		abort  bool
	}{
		{"ranges on the bad leg alone", []uint64{reactDataLegA}, false},
		{"ranges on both legs", []uint64{reactDataLegA, reactDataLegB}, true},
		{"no ranges", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.judgeStored = false
			withWorkerRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
			h.legOf(reactDataLegA).ErrEpoch = 0
			setRanges(h, tc.ranges...)
			h.dnCands(reactDnD)
			h.pass()
			if !tc.abort {
				h.wantOps()
				return
			}
			h.wantOps("abort_repair")
			rec := h.wantApplied(reactionRepairAbort)[0]
			if rec["trigger"] != triggerBadLegHealthy {
				t.Fatalf("applied = %v", rec)
			}
		})
	}
	for _, tc := range []struct {
		name string
		row  func(h *reactHarness)
	}{
		{"no report of the primary", func(h *reactHarness) {
			delete(h.w.cntlrs, reactCntlrA)
		}},
		{"an array that does not run", func(h *reactHarness) {
			h.setGrpRow(reactDataGrp, pb.ResStatus_RES_STATUS_ERROR)
		}},
		{"a reading older than the snapshot", func(h *reactHarness) {
			h.state.SpRevision++
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.judgeStored = false
			withWorkerRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
			h.legOf(reactDataLegA).ErrEpoch = 0
			h.dnCands(reactDnD)
			h.inSyncAll = false
			h.fillGrpInfo()
			tc.row(h)
			h.pass()
			h.wantOps()
		})
	}
}

// doneAgo records a check of the data group that the primary ran to its
// end age seconds ago (MdCheck.done_epoch), with no request standing.
func doneAgo(h *reactHarness, age uint64) {
	h.t.Helper()
	h.dataGrp().MdCheck = &pb.MdCheck{NextEpoch: h.now() + 1000,
		DoneEpoch: h.ago(age)}
}

// TestLegRepairOfRangesWaitsForTheMdCheck pins AR8's wait of a repair for
// md's bad-block ranges on the group's md check (dnv-worker.md AR8, AR16):
// with no check of the group that ended within common.MdCheckDoneFresh, no
// repair is created and the hold names the wait, and with no request
// standing in the pool the pass asks for the group's check, ahead of a due
// group; while a check runs nothing is asked; once the reading names the
// check done with the other leg still clean, the repair is created in the
// same pass and the schedule records the check's end; a fresh recorded
// check lets the create run, a stale one does not; with a range on the
// other leg at the check's end no repair is created. With the cluster's
// interval at zero no check runs and the repair does not wait; a clone meta
// group gets no check and its repair does not wait either; and a group with
// a repair logs that hold, not the wait.
func TestLegRepairOfRangesWaitsForTheMdCheck(t *testing.T) {
	t.Run("no check", func(t *testing.T) {
		h := mdCheckHarness(t, 30)
		setRanges(h, reactDataLegA)
		h.metaGrp().MdCheck = &pb.MdCheck{NextEpoch: h.ago(10)}
		h.dataGrp().MdCheck = &pb.MdCheck{NextEpoch: h.now() + 1000,
			DoneEpoch: h.ago(common.MdCheckDoneFresh)}
		h.dnCands(reactDnD)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionRepairCreate, reasonMdCheckWaits)
		edits := mdEdits(t, h)
		ask := editOf(edits, reactDataGrp)
		if ask == nil || ask.To.GetRequestEpoch() != h.now() ||
			ask.To.GetNextEpoch() != h.now()+1000 ||
			ask.To.GetDoneEpoch() != h.ago(common.MdCheckDoneFresh) {
			t.Fatalf("edits %+v, want the data group asked for now, its "+
				"times kept", edits)
		}
		if meta := editOf(edits, reactMetaGrp); meta != nil &&
			meta.To.GetRequestEpoch() != 0 {
			t.Fatalf("edits %+v, want the due meta group not asked for",
				edits)
		}
	})
	t.Run("a check runs", func(t *testing.T) {
		h := mdCheckHarness(t, 30)
		setRanges(h, reactDataLegA)
		h.metaGrp().MdCheck = &pb.MdCheck{NextEpoch: h.ago(10)}
		h.dataGrp().MdCheck = &pb.MdCheck{NextEpoch: h.ago(100),
			RequestEpoch: h.ago(60)}
		h.dnCands(reactDnD)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionRepairCreate, reasonMdCheckWaits)
		if edits := mdEdits(t, h); edits != nil {
			t.Fatalf("edits %+v while a check runs, want none: one "+
				"request at a time", edits)
		}
	})
	t.Run("the check done, the other leg clean", func(t *testing.T) {
		h := mdCheckHarness(t, 30)
		setRanges(h, reactDataLegA)
		checkDone(h)
		h.dnCands(reactDnD)
		h.pass()
		calls := h.wantOps("create_repair")
		if calls[0].badLegId != reactDataLegA || !calls[0].badBlocks {
			t.Fatalf("create_repair = %+v, want leg A for its ranges",
				calls[0])
		}
		end := editOf(mdEdits(t, h), reactDataGrp)
		if end == nil || end.To.GetRequestEpoch() != 0 ||
			end.To.GetDoneEpoch() != h.now() {
			t.Fatalf("edit %+v, want the request ended and its end "+
				"recorded", end)
		}
	})
	for _, tc := range []struct {
		name   string
		age    uint64
		create bool
	}{
		{"a fresh recorded check", common.MdCheckDoneFresh - 1, true},
		{"a stale recorded check", common.MdCheckDoneFresh, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := mdCheckHarness(t, 30)
			setRanges(h, reactDataLegA)
			doneAgo(h, tc.age)
			h.dnCands(reactDnD)
			h.pass()
			if tc.create {
				h.wantOps("create_repair")
				return
			}
			h.wantOps()
			h.wantSkipped(reactionRepairCreate, reasonMdCheckWaits)
		})
	}
	// A fresh check whose repair waits on something else, here no disk
	// node with room, asks for no new check: the group would otherwise be
	// asked for on every pass and starve the pool's other groups.
	t.Run("a fresh check, the repair held", func(t *testing.T) {
		h := mdCheckHarness(t, 30)
		setRanges(h, reactDataLegA)
		h.metaGrp().MdCheck = &pb.MdCheck{NextEpoch: h.now() + 1000}
		doneAgo(h, 100)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionRepairCreate, reasonNoCandidate)
		if edits := mdEdits(t, h); edits != nil {
			t.Fatalf("edits %+v, want no check asked for", edits)
		}
	})
	t.Run("the check done, the other leg with ranges", func(t *testing.T) {
		h := mdCheckHarness(t, 30)
		setRanges(h, reactDataLegA, reactDataLegB)
		checkDone(h)
		h.dnCands(reactDnD)
		h.pass()
		h.wantOps()
		end := editOf(mdEdits(t, h), reactDataGrp)
		if end == nil || end.To.GetRequestEpoch() != 0 ||
			end.To.GetDoneEpoch() != h.now() {
			t.Fatalf("edit %+v, want the done request ended", end)
		}
	})
	t.Run("the interval at zero", func(t *testing.T) {
		h := mdCheckHarness(t, 0)
		setRanges(h, reactDataLegA)
		h.dnCands(reactDnD)
		h.pass()
		h.wantOps("create_repair")
		if edits := mdEdits(t, h); edits != nil {
			t.Fatalf("edits %+v with the check off, want none", edits)
		}
	})
	t.Run("a clone meta group", func(t *testing.T) {
		h := mdCheckHarness(t, 30)
		withReactClone(t, h.state)
		h.setGrpInfo(reactCloneGrp, true, reactCloneLegA, reactCloneLegB)
		h.w.cntlrs[reactCntlrA].driver.lastInfo.
			GrpIdToGrpInfo[reactCloneGrp].LegIdToBadBlocks =
			map[uint64]uint32{reactCloneLegA: 1}
		h.setGrpRow(reactCloneGrp, pb.ResStatus_RES_STATUS_OK)
		h.dnCands(reactDnD)
		h.pass()
		calls := h.wantOps("create_repair")
		if calls[0].badLegId != reactCloneLegA || !calls[0].badBlocks {
			t.Fatalf("create_repair = %+v, want the clone's leg A for its "+
				"ranges", calls[0])
		}
	})
	// A leg due on AR8's own clocks is repaired by them, with no wait and
	// no check asked for on account of its ranges.
	t.Run("a leg due on its own clocks", func(t *testing.T) {
		h := mdCheckHarness(t, 30)
		setRanges(h, reactDataLegA)
		h.metaGrp().MdCheck = &pb.MdCheck{NextEpoch: h.now() + 1000}
		h.dataGrp().MdCheck = &pb.MdCheck{NextEpoch: h.now() + 1000}
		h.legOf(reactDataLegA).ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		h.w.verdicts.noteLeg(reactDataLegA, true)
		h.dnCands(reactDnD)
		h.pass()
		calls := h.wantOps("create_repair")
		if calls[0].badLegId != reactDataLegA || calls[0].badBlocks {
			t.Fatalf("create_repair = %+v, want leg A for its clocks",
				calls[0])
		}
		if edits := mdEdits(t, h); edits != nil {
			t.Fatalf("edits %+v, want no check asked for", edits)
		}
	})
	t.Run("a group with a repair", func(t *testing.T) {
		h := mdCheckHarness(t, 30)
		setRanges(h, reactDataLegA)
		h.dataGrp().RepName = "op-rep"
		h.dnCands(reactDnD)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionRepairCreate, reasonGrpHasRepair)
		for _, rec := range h.logs.withMsg(msgReactionSkipped) {
			if rec["reason"] == reasonMdCheckWaits {
				t.Fatalf("a group with a repair logs %v", rec)
			}
		}
	})
}
