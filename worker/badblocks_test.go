package worker

import (
	"testing"

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

// TestLegRepairOfALegWithRanges pins AR8 on md's bad-block ranges: a leg
// whose member carries ranges, with no health epoch and an OK row, is a
// candidate of its own and is repaired, the create saying so, while another
// leg is healthy, held in sync and clean of ranges; with every other leg
// carrying ranges too it is no candidate, with no record, since a copy would
// put the same ranges on the new leg; and a dead leg beside a leg with ranges is
// repaired from it as from any healthy leg, its ranges notwithstanding.
func TestLegRepairOfALegWithRanges(t *testing.T) {
	t.Run("the other leg clean", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		setRanges(h, reactDataLegA)
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
