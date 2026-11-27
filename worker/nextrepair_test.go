package worker

import (
	"errors"
	"slices"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestLegRepairCancelJoinsTheNextCreate pins AR8's joined cancel and
// create: the cancel of a worker's ABORTED repair whose new leg is due on a
// repair clock, while its bad leg still needs a repair — on its clocks or
// for md's bad-block ranges once the group's md check is done — creates the
// group's next repair in the same transaction, the scan black-listing that new leg's disk node; with no new
// leg due — a young health epoch included — a bad leg that came back,
// another leg due, a hold of the create other than a stale report, no disk
// node left, or a refusal of the create other than a changed candidate, the
// cancel runs alone. While the primary's report is older than the snapshot,
// the cancel waits and the walk goes on to the next repair; a scan that
// fails ends the pass.
func TestLegRepairCancelJoinsTheNextCreate(t *testing.T) {
	build := func(t *testing.T) (*reactHarness, *pb.Repair) {
		h := newReactHarness(t, reactFixture(t))
		_, rep := withWorkerRepair(h, pb.RepStage_REP_STAGE_ABORTED, true)
		h.legOf(reactDataLegA).ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		h.w.verdicts.noteLeg(reactDataLegA, true)
		h.dnCands(reactDnC, reactDnD)
		h.rops.dnBlackFilter = true
		return h, rep
	}
	t.Run("the new leg due", func(t *testing.T) {
		h, rep := build(t)
		rep.NewLegList[0].ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		h.state.DnByAddr[reactDnA] = &pb.DnConf{Location: "rack0"}
		h.state.DnByAddr[reactDnB] = &pb.DnConf{Location: "rack1"}
		h.pass()
		calls := h.wantOps("cancel_and_create_repair")
		if calls[0].badLegId != reactDataLegA ||
			calls[0].legs[0].AddrPort != reactDnD {
			t.Fatalf("joined call %+v, want leg A's next repair on dn D",
				calls[0])
		}
		q := h.rops.queries[len(h.rops.queries)-1]
		if !slices.Contains(q.black, reactDnC) {
			t.Fatalf("scan black list %v, want the aborted new leg's dn",
				q.black)
		}
		// The group's locations, from the node records the pass reads for
		// the scan (AR1, passNodes).
		if !slices.Contains(q.excludeLocs, "rack0") ||
			!slices.Contains(q.excludeLocs, "rack1") {
			t.Fatalf("scan excluded %q, want the group's two racks",
				q.excludeLocs)
		}
		recs := h.wantApplied(reactionRepairCancel, reactionRepairCreate)
		if recs[1]["slice_id"] != float64(reactSliceId) ||
			recs[1]["leg_id"] != float64(reactDataLegA) ||
			recs[1]["addr_port"] != reactDnD {
			t.Fatalf("the create's record %v", recs[1])
		}
	})
	t.Run("the new leg's side due", func(t *testing.T) {
		h, rep := build(t)
		rep.NewLegList[0].SideList[0].ErrEpoch = h.ago(
			common.DefaultSideUnhealthy)
		h.pass()
		h.wantOps("cancel_and_create_repair")
	})
	t.Run("the bad leg's ranges", func(t *testing.T) {
		h, rep := build(t)
		rep.NewLegList[0].ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		h.legOf(reactDataLegA).ErrEpoch = 0
		setRanges(h, reactDataLegA)
		checkDone(h)
		h.pass()
		calls := h.wantOps("cancel_and_create_repair")
		if calls[0].badLegId != reactDataLegA || !calls[0].badBlocks {
			t.Fatalf("joined call %+v, want leg A for its ranges", calls[0])
		}
	})
	// A repair for the bad leg's ranges waits for the group's md check
	// (dnv-worker.md AR8, A leg with bad-block ranges), so with no done
	// check the cancel runs alone.
	t.Run("the bad leg's ranges, no done check", func(t *testing.T) {
		h, rep := build(t)
		cc := reactClusterConf()
		cc.MdCheckConf = &pb.MdCheckConf{IntervalDays: 30}
		setCachedConf(h.deps, testCid, cc)
		rep.NewLegList[0].ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		h.legOf(reactDataLegA).ErrEpoch = 0
		setRanges(h, reactDataLegA)
		h.pass()
		h.wantOps("cancel_repair")
	})
	// A second worker's repair, STARTED, after the aborted one in
	// repair_name_list: the drive step finishes it only when the walk goes
	// on past the aborted one.
	started := func(h *reactHarness) {
		withRepair(h, common.WorkerRepName(testSpId, reactMetaGrp, 2),
			h.metaGrp(), reactMetaLegA, pb.RepStage_REP_STAGE_STARTED, false,
			reactRepId+100)
	}
	t.Run("a report older than the snapshot", func(t *testing.T) {
		h, rep := build(t)
		rep.NewLegList[0].ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		started(h)
		h.state.SpRevision = 1 << 40
		h.pass()
		h.wantOps("finish_repair")
		h.wantSkipped(reactionRepairCancel, reasonGrpRowStale)
	})
	t.Run("the scan fails", func(t *testing.T) {
		h, rep := build(t)
		rep.NewLegList[0].ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		started(h)
		h.rops.scanErr = errors.New("scan down")
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionRepairCreate, reasonOpFailed)
	})
	t.Run("a third leg healthy beside another leg due", func(t *testing.T) {
		h, rep := build(t)
		rep.NewLegList[0].ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		h.legOf(reactDataLegB).ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		grp := h.dataGrp()
		grp.LegList = append(grp.LegList, &pb.Leg{LegId: 5555, LegIdx: 3,
			SideList: []*pb.Side{{SideId: 5556, AddrPort: "rdn4:9520",
				Provisioned: true}}})
		h.setGrpInfo(reactDataGrp, false, 5555)
		h.pass()
		h.wantOps("cancel_repair")
	})
	t.Run("the create refused", func(t *testing.T) {
		h, rep := build(t)
		rep.NewLegList[0].ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		h.rops.repairErr = map[string]error{rep0Name(): &model.ErrPrecondition{
			Op: "CreateRepair", Reason: "no cntlid slot"}}
		h.pass()
		h.wantOps("cancel_and_create_repair", "cancel_repair")
	})
	t.Run("the candidate changed", func(t *testing.T) {
		h, rep := build(t)
		rep.NewLegList[0].ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		h.rops.repairErr = map[string]error{rep0Name(): &model.ErrPrecondition{
			Op: "CreateRepair", Reason: model.ReasonCandidateChanged}}
		h.pass()
		h.wantOps("cancel_and_create_repair")
	})
	for _, tc := range []struct {
		name string
		set  func(h *reactHarness, rep *pb.Repair)
	}{
		{"the new leg healthy", func(*reactHarness, *pb.Repair) {}},
		{"a young health epoch", func(h *reactHarness, rep *pb.Repair) {
			rep.NewLegList[0].ErrEpoch = h.ago(1)
		}},
		{"the bad leg back", func(h *reactHarness, rep *pb.Repair) {
			rep.NewLegList[0].ErrEpoch = h.ago(common.DefaultLegUnhealthy)
			h.legOf(reactDataLegA).ErrEpoch = 0
		}},
		{"no other disk node", func(h *reactHarness, rep *pb.Repair) {
			rep.NewLegList[0].ErrEpoch = h.ago(common.DefaultLegUnhealthy)
			h.dnCands(reactDnC)
		}},
		{"another leg due", func(h *reactHarness, rep *pb.Repair) {
			rep.NewLegList[0].ErrEpoch = h.ago(common.DefaultLegUnhealthy)
			h.legOf(reactDataLegB).ErrEpoch = h.ago(
				common.DefaultLegUnhealthy)
		}},
		{"the operator in charge", func(h *reactHarness, rep *pb.Repair) {
			rep.NewLegList[0].ErrEpoch = h.ago(common.DefaultLegUnhealthy)
			h.dataGrp().OperatorInCharge = true
		}},
		{"no healthy leg", func(h *reactHarness, rep *pb.Repair) {
			rep.NewLegList[0].ErrEpoch = h.ago(common.DefaultLegUnhealthy)
			h.legOf(reactDataLegB).ErrEpoch = h.ago(1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, rep := build(t)
			tc.set(h, rep)
			h.pass()
			h.wantOps("cancel_repair")
		})
	}
}

// rep0Name is the name withWorkerRepair gives the data group's repair.
func rep0Name() string {
	return common.WorkerRepName(testSpId, reactDataGrp, 1)
}
