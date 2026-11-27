package worker

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestRecopyClearedFromTheReport pins AR13's recopy clear: a recopy mark on
// a leg of the group's leg_list that the current primary's latest report
// shows done — a report of an observation at the mark's revision or later
// whose reading of the group names the mark's revision as the recopy the
// primary ran on the leg and holds the leg in sync — is cleared through the
// op, handed the group's parent and id, the leg and the mark's revision,
// and logged with the revision the op's own STM committed. A report that
// names no recopy of the leg or another revision, one that does not hold
// the leg in sync, as while md still copies it, and one older than the
// mark clear nothing and log nothing.
func TestRecopyClearedFromTheReport(t *testing.T) {
	due := func(t *testing.T, acted uint64, inSync []uint64,
		reportRev uint64) *reactHarness {
		h := newReactHarness(t, reactFixture(t))
		for _, leg := range h.dataGrp().GetLegList() {
			if leg.GetLegId() == reactDataLegA {
				leg.RecopyRevision = reactMarkRevision
			}
		}
		h.reportAt(reportRev)
		h.w.cntlrs[reactCntlrA].driver.lastInfo.GrpIdToGrpInfo =
			map[uint64]*pb.CntlrInfo_GrpInfo{reactDataGrp: {
				InSyncLegIdList: inSync,
				LegIdToRecopyRevision: map[uint64]uint64{
					reactDataLegA: acted},
			}}
		return h
	}
	both := []uint64{reactDataLegA, reactDataLegB}
	t.Run("a pool without RAID", func(t *testing.T) {
		// Nothing copies the leg; the mark clears once the primary reads
		// the leg served by its own side alone (no reason on its row).
		for _, tc := range []struct {
			name   string
			reason pb.ResReason
			want   bool
		}{
			{"served alone", pb.ResReason_RES_REASON_NONE, true},
			{"not alone", pb.ResReason_RES_REASON_LEG_PATH_NOT_SOLE, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := due(t, 0, nil, reactMarkRevision)
				h.state.Conf.BdevConf.RedundConf = &pb.RedundConf{}
				h.setLegRow(reactDataLegA, pb.ResStatus_RES_STATUS_OK)
				row := h.w.cntlrs[reactCntlrA].driver.lastInfo.
					LegIdToLeg[reactDataLegA]
				row.Reason = tc.reason
				h.pass()
				if !tc.want {
					h.wantNoRecord(msgRecopyCleared)
					return
				}
				calls := h.wantOps("clear_recopy")
				if calls[0].badLegId != reactDataLegA {
					t.Fatalf("clear = %+v, want leg %d", calls[0],
						reactDataLegA)
				}
			})
		}
	})
	t.Run("cleared", func(t *testing.T) {
		h := due(t, reactMarkRevision, both, reactMarkRevision)
		h.pass()
		calls := h.wantOps("clear_recopy")
		if calls[0].grpId != reactDataGrp ||
			calls[0].sliceId != reactSliceId ||
			calls[0].badLegId != reactDataLegA ||
			calls[0].now != reactMarkRevision {
			t.Fatalf("clear = %+v, want leg %d of group %d at revision %d",
				calls[0], reactDataLegA, reactDataGrp, reactMarkRevision)
		}
		recs := h.logs.withMsg(msgRecopyCleared)
		if len(recs) != 1 || recs[0]["leg_id"] != float64(reactDataLegA) ||
			recs[0]["revision"] != float64(reactMarkRevision+1) {
			t.Fatalf("%q records = %v, want one naming leg %d with the "+
				"clear's own revision", msgRecopyCleared, recs,
				reactDataLegA)
		}
	})
	for _, tc := range []struct {
		name      string
		acted     uint64
		inSync    []uint64
		reportRev uint64
	}{
		{"no recopy reported", 0, both, reactMarkRevision},
		{"a recopy of another mark", reactMarkRevision - 1, both,
			reactMarkRevision},
		{"the leg not in sync", reactMarkRevision,
			[]uint64{reactDataLegB}, reactMarkRevision},
		{"a report older than the mark", reactMarkRevision, both,
			reactMarkRevision - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := due(t, tc.acted, tc.inSync, tc.reportRev)
			h.pass()
			h.wantOps()
			h.wantNoRecord(msgRecopyCleared)
			h.wantNoRecord(msgRecopyKept)
		})
	}
}
