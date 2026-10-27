package worker

import (
	"errors"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// AR13 — the forced-start clear, and AR8's hold on a group with a mark
// ---------------------------------------------------------------------------

// reactMarkRevision is the revision the fixture's forced-start marks carry
// (withMark): the SpRev revision their write committed.
const reactMarkRevision = uint64(40)

// withMark gives grp an operator's forced-start mark naming legId, carrying
// reactMarkRevision.
func withMark(grp *pb.Group, legId uint64) *pb.ForcedStart {
	grp.ForcedStart = &pb.ForcedStart{
		LegId: legId, Revision: reactMarkRevision,
	}
	return grp.ForcedStart
}

// reportAt makes the primary's latest report one of an observation at
// revision rev, and the snapshot's SpRevision rev too, so AR8's gate reads
// the report as current.
func (h *reactHarness) reportAt(rev uint64) {
	h.t.Helper()
	if _, ok := h.w.cntlrs[reactCntlrA]; !ok {
		h.setPrimaryInfo(reactCntlrA, &pb.CntlrInfo{})
	}
	h.w.cntlrs[reactCntlrA].driver.infoRev = rev
	h.state.SpRevision = rev
}

// setLegRow installs one leg's row (leg_id_to_leg) on the primary, which
// AR13 reads for every leg md holds in sync, the named one included.
func (h *reactHarness) setLegRow(legId uint64, status pb.ResStatus) {
	h.t.Helper()
	child, ok := h.w.cntlrs[reactCntlrA]
	if !ok {
		h.setPrimaryInfo(reactCntlrA, &pb.CntlrInfo{})
		child = h.w.cntlrs[reactCntlrA]
	}
	info := child.driver.lastInfo
	if info.LegIdToLeg == nil {
		info.LegIdToLeg = map[uint64]*pb.ResInfo{}
	}
	info.LegIdToLeg[legId] = &pb.ResInfo{ResName: "leg", Status: status}
}

// wantMarkRecord asserts exactly one record msg, naming the data group's
// mark on legId: the slice, the group, the leg, the mark's revision, the
// primary and its report's revision, and the reason when one is given. It
// returns the record.
func (h *reactHarness) wantMarkRecord(
	msg string, legId uint64, reason string,
) map[string]any {
	h.t.Helper()
	recs := h.logs.withMsg(msg)
	if len(recs) != 1 {
		h.t.Fatalf("%q records = %v, want one", msg, recs)
	}
	rec := recs[0]
	if rec["slice_id"] != float64(reactSliceId) ||
		rec["grp_id"] != float64(reactDataGrp) ||
		rec["leg_id"] != float64(legId) ||
		rec["mark_revision"] != float64(reactMarkRevision) ||
		rec["cntlr_id"] != float64(reactCntlrA) ||
		rec["report_revision"] != float64(reactMarkRevision) {
		h.t.Fatalf("record %v does not name the mark on leg %d of group %d "+
			"at revision %d on cntlr %d's report", rec, legId, reactDataGrp,
			reactMarkRevision, reactCntlrA)
	}
	if reason != "" && rec["reason"] != reason {
		h.t.Fatalf("record %v, want reason %s", rec, reason)
	}
	return rec
}

// TestForcedStartClearedFromTheReport pins AR13's clear: a mark the current
// primary's latest report shows done — the group's row OK, a leg held in
// sync whose own row reads OK, the named leg or another, a report of an
// observation at the mark's revision or later — is cleared through the op,
// handed the group's parent and id and the mark's leg and revision, and
// logged with the revision the op's own STM committed. A report older than
// the mark, a row that does not read OK, a row absent from a report that
// holds everything else the clear reads, every leg md holds in sync on a row
// that is absent or does not read OK — the named leg among them, as md holds
// a dead source in sync while it retries a copy — no report and a standby's
// report, though it holds all the clear reads, clear nothing and log
// nothing; an op that
// refuses or fails is logged as kept, and a mark the op skips, changed since
// the snapshot, logs nothing.
func TestForcedStartClearedFromTheReport(t *testing.T) {
	// due is the data group's mark on leg B in a report at the mark's
	// revision, which inSyncAll reads with the row OK and both legs in sync,
	// each on a leg row that reads OK.
	due := func(t *testing.T) *reactHarness {
		h := newReactHarness(t, reactFixture(t))
		withMark(h.dataGrp(), reactDataLegB)
		h.reportAt(reactMarkRevision)
		return h
	}

	t.Run("cleared", func(t *testing.T) {
		h := due(t)
		h.pass()
		calls := h.wantOps("clear_forced_start")
		want := model.ForcedStartRef{
			Parent: model.GrpParent{SliceId: reactSliceId},
			GrpId:  reactDataGrp, LegId: reactDataLegB,
			Revision: reactMarkRevision,
		}
		if calls[0].mark != want {
			t.Fatalf("clear = %+v, want %+v", calls[0].mark, want)
		}
		rec := h.wantMarkRecord(msgForcedStartCleared, reactDataLegB, "")
		// The fake's STM commits at the snapshot's revision plus one.
		if rec["revision"] != float64(reactMarkRevision+1) {
			t.Fatalf("record %v, want the clear's own revision %d", rec,
				reactMarkRevision+1)
		}
		h.wantNoRecord(msgForcedStartKept)
		h.wantNoSkip()
	})

	// A report of an observation at a revision after the mark's clears it
	// as well, whatever bumps the pool has seen since, and the op is still
	// handed the mark's own revision, the one ClearForcedStart matches the
	// stored mark by (MD6), never the report's or the snapshot's.
	t.Run("a later report", func(t *testing.T) {
		h := due(t)
		h.w.cntlrs[reactCntlrA].driver.infoRev = reactMarkRevision + 5
		h.state.SpRevision = reactMarkRevision + 9
		h.pass()
		calls := h.wantOps("clear_forced_start")
		if got := calls[0].mark.Revision; got != reactMarkRevision {
			t.Fatalf("clear = %+v, want the mark's revision %d, not the "+
				"report's %d or the snapshot's %d", calls[0].mark,
				reactMarkRevision, reactMarkRevision+5, reactMarkRevision+9)
		}
	})

	// A repair's new leg named and held in sync on a leg row that reads OK,
	// beside a leg_list leg in sync whose row does not: md lists the new leg
	// in the in-sync list beside the group's own legs, and the primary
	// reports its leg row as it does a leg_list leg's (cnagent.md CN28).
	t.Run("a repair's new leg", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
		withMark(h.dataGrp(), reactNewLeg)
		h.reportAt(reactMarkRevision)
		h.setGrpInfo(reactDataGrp, false, reactDataLegB, reactNewLeg)
		h.setLegRow(reactDataLegB, pb.ResStatus_RES_STATUS_ERROR)
		h.setLegRow(reactNewLeg, pb.ResStatus_RES_STATUS_OK)
		h.pass()
		calls := h.wantOps("clear_forced_start")
		if calls[0].mark.LegId != reactNewLeg {
			t.Fatalf("clear = %+v, want the new leg's mark", calls[0].mark)
		}
		h.wantMarkRecord(msgForcedStartCleared, reactNewLeg, "")
	})

	// The named leg out of sync while md holds leg A in sync on a leg whose
	// row reads OK: the array runs on a member that serves, so the mark is
	// done, as after its named leg died once the start was made, and the op
	// is handed the mark as it stands.
	t.Run("another leg in sync on a leg that serves", func(t *testing.T) {
		h := due(t)
		h.setGrpInfo(reactDataGrp, false, reactDataLegA)
		h.setLegRow(reactDataLegA, pb.ResStatus_RES_STATUS_OK)
		h.setLegRow(reactDataLegB, pb.ResStatus_RES_STATUS_ERROR)
		h.pass()
		calls := h.wantOps("clear_forced_start")
		if calls[0].mark.LegId != reactDataLegB ||
			calls[0].mark.Revision != reactMarkRevision {
			t.Fatalf("clear = %+v, want the mark on leg %d", calls[0].mark,
				reactDataLegB)
		}
		h.wantMarkRecord(msgForcedStartCleared, reactDataLegB, "")
	})

	for _, tc := range []struct {
		name string
		arm  func(h *reactHarness)
	}{
		{"a report older than the mark", func(h *reactHarness) {
			h.w.cntlrs[reactCntlrA].driver.infoRev = reactMarkRevision - 1
		}},
		{"the row not OK", func(h *reactHarness) {
			h.setGrpRow(reactDataGrp, pb.ResStatus_RES_STATUS_ERROR)
		}},
		// Everything else the clear reads is there: both legs in sync, each
		// on a leg row that reads OK; only the group's row is missing.
		{"the row absent", func(h *reactHarness) {
			h.inSyncAll = false
			h.setGrpInfo(reactDataGrp, true, reactDataLegA, reactDataLegB)
			h.setLegRow(reactDataLegA, pb.ResStatus_RES_STATUS_OK)
			h.setLegRow(reactDataLegB, pb.ResStatus_RES_STATUS_OK)
		}},
		{"another leg in sync, its row absent", func(h *reactHarness) {
			h.setGrpInfo(reactDataGrp, false, reactDataLegA)
		}},
		// md retries a copy whose only source died: it holds that dead
		// member in sync while the array reads running, so a member in sync
		// on a leg whose row does not read OK proves nothing, whether it is
		// another leg or the named one, which the operator names while it
		// is dead.
		{"the named leg not in sync, the leg in sync not serving",
			func(h *reactHarness) {
				h.setGrpInfo(reactDataGrp, false, reactDataLegA)
				h.setLegRow(reactDataLegA, pb.ResStatus_RES_STATUS_ERROR)
			}},
		{"the named leg in sync, not serving", func(h *reactHarness) {
			h.setGrpInfo(reactDataGrp, false, reactDataLegB)
			h.setLegRow(reactDataLegB, pb.ResStatus_RES_STATUS_ERROR)
		}},
		{"the named leg in sync, its row absent", func(h *reactHarness) {
			h.setGrpInfo(reactDataGrp, false, reactDataLegB)
		}},
		{"no report", func(h *reactHarness) {
			h.inSyncAll = false
			delete(h.w.cntlrs, reactCntlrA)
		}},
		// A cntlr whose child drives the standby plan is not the pass's
		// primary, so its report is never read (AR1), though it holds all
		// a primary's report needs for the clear.
		{"a standby's report", func(h *reactHarness) {
			h.inSyncAll = false
			h.w.cntlrs[reactCntlrA] = &cntlrChild{
				plan: &cntlrPlan{cntlrId: reactCntlrA},
				driver: &cntlrDriver{
					infoRev: reactMarkRevision,
					lastInfo: &pb.CntlrInfo{
						GrpIdToMdRaid: map[uint64]*pb.ResInfo{
							reactDataGrp: resOk("md"),
						},
						LegIdToLeg: map[uint64]*pb.ResInfo{
							reactDataLegA: resOk("leg"),
							reactDataLegB: resOk("leg"),
						},
						GrpIdToGrpInfo: map[uint64]*pb.CntlrInfo_GrpInfo{
							reactDataGrp: {
								Whole: true,
								InSyncLegIdList: []uint64{
									reactDataLegA, reactDataLegB,
								},
							},
						},
					},
				},
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := due(t)
			tc.arm(h)
			h.pass()
			h.wantOps()
			h.wantNoRecord(msgForcedStartCleared)
			h.wantNoRecord(msgForcedStartKept)
		})
	}

	t.Run("the op refuses", func(t *testing.T) {
		h := due(t)
		h.rops.markErr = &model.ErrPrecondition{
			Op: "ClearForcedStart", Reason: "sp not found",
		}
		h.pass()
		h.wantOps("clear_forced_start")
		rec := h.wantMarkRecord(msgForcedStartKept, reactDataLegB,
			"sp not found")
		if rec["op"] != "ClearForcedStart" {
			t.Fatalf("refusal record %v does not name the op", rec)
		}
		h.wantNoRecord(msgForcedStartCleared)
	})

	t.Run("the op fails", func(t *testing.T) {
		h := due(t)
		h.rops.markErr = errors.New("etcd unavailable")
		h.pass()
		h.wantOps("clear_forced_start")
		rec := h.wantMarkRecord(msgForcedStartKept, reactDataLegB,
			reasonOpFailed)
		if rec["error"] != "etcd unavailable" {
			t.Fatalf("failure record %v does not carry the error", rec)
		}
	})

	// The store holds another mark, or none, since the snapshot: the op
	// skips it, and the next pass reads the mark as it stands.
	t.Run("the op skips a mark changed since", func(t *testing.T) {
		h := due(t)
		h.rops.markSkip = true
		h.pass()
		h.wantOps("clear_forced_start")
		h.wantNoRecord(msgForcedStartCleared)
		h.wantNoRecord(msgForcedStartKept)
	})
}

// TestForcedStartClearWalksEveryGroup pins AR13's walk: the slices' groups in
// slice_id_list order, meta groups before data groups, then the clone meta
// groups in clone_name_list order, one clear per due mark, a clone's handed
// with the clone as its parent; a group without a mark costs nothing.
func TestForcedStartClearWalksEveryGroup(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	cloneGrp := withReactClone(t, h.state)
	withMark(cloneGrp, reactCloneLegA)
	withMark(h.dataGrp(), reactDataLegA)
	withMark(h.metaGrp(), reactMetaLegB)
	h.reportAt(reactMarkRevision)
	h.pass()
	calls := h.wantOps(
		"clear_forced_start", "clear_forced_start", "clear_forced_start")
	for idx, want := range []model.ForcedStartRef{
		{
			Parent: model.GrpParent{SliceId: reactSliceId},
			GrpId:  reactMetaGrp, LegId: reactMetaLegB,
			Revision: reactMarkRevision,
		},
		{
			Parent: model.GrpParent{SliceId: reactSliceId},
			GrpId:  reactDataGrp, LegId: reactDataLegA,
			Revision: reactMarkRevision,
		},
		{
			Parent: model.GrpParent{CloneName: reactCloneNm},
			GrpId:  reactCloneGrp, LegId: reactCloneLegA,
			Revision: reactMarkRevision,
		},
	} {
		if calls[idx].mark != want {
			t.Fatalf("clear %d = %+v, want %+v", idx, calls[idx].mark, want)
		}
	}
	recs := h.logs.withMsg(msgForcedStartCleared)
	if len(recs) != 3 {
		t.Fatalf("cleared records = %v, want three", recs)
	}
	wantCloneRecord(t, recs[2], reactCloneLegA)
	if _, ok := recs[2]["slice_id"]; ok {
		t.Fatalf("the clone's record %v names a slice", recs[2])
	}
}

// TestForcedStartClearSkipsADeletingClone pins AR13 on a deleting clone: its
// group's due mark is kept with clone_deleting and handed to no op, since
// the clone drain's final transaction gives the group back (CLD9), which
// runs in the same pass (CLD7).
func TestForcedStartClearSkipsADeletingClone(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	grp := withReactClone(t, h.state)
	h.state.Clones[reactCloneNm].Deleting = true
	withMark(grp, reactCloneLegB)
	h.reportAt(reactMarkRevision)
	h.pass()
	h.wantOps("finish_clone")
	recs := h.logs.withMsg(msgForcedStartKept)
	if len(recs) != 1 || recs[0]["reason"] != reasonCloneDeleting {
		t.Fatalf("kept records = %v, want one %s", recs, reasonCloneDeleting)
	}
	wantCloneRecord(t, recs[0], reactCloneLegB)
	h.wantNoRecord(msgForcedStartCleared)
}

// TestForcedStartClearedAtEveryLevel pins AR13's place in the pass: the
// clear runs at every sp_level, the levels where reactions are suppressed
// included (AR3), where neither the retire timer nor a reaction runs; and a
// deleting pool runs its drain step and no clear.
func TestForcedStartClearedAtEveryLevel(t *testing.T) {
	for _, level := range []pb.SpLevel{
		pb.SpLevel_SP_LEVEL_NO_THINPOOL, pb.SpLevel_SP_LEVEL_DISABLE,
	} {
		t.Run(level.String(), func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.state.Conf.SpLevel = level
			withMark(h.dataGrp(), reactDataLegB)
			h.reportAt(reactMarkRevision)
			// An old retired leg the timer would delete, and the meta
			// group's leg A due for a repair the reactions would start:
			// neither runs here.
			withReactRetired(h, h.metaGrp(), retireLegA, retireSideA,
				h.ago(common.RetiredLegMaxAge))
			h.legOf(reactMetaLegA).ErrEpoch = h.ago(9000)
			h.dnCands(reactDnD)
			h.pass()
			h.wantOps("clear_forced_start")
			h.wantMarkRecord(msgForcedStartCleared, reactDataLegB, "")
			h.wantNoRecord(msgRetiredLegDeleted)
			h.wantNoRecord(msgRetiredLegKept)
			h.wantApplied()
		})
	}

	t.Run("a deleting pool", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.state.Conf.Deleting = true
		withMark(h.dataGrp(), reactDataLegB)
		h.reportAt(reactMarkRevision)
		h.pass()
		h.wantOps("drain_cntlrs")
		h.wantNoRecord(msgForcedStartCleared)
	})
}

// TestForcedStartClearIsNotTheOneAction pins AR2's count against AR13: a
// clear and a reaction commit in one pass, the clear being no reaction; and
// a clear bumps SpRev, so the retire timer's delete in the same pass, which
// carries the snapshot's revision, is refused as stale and commits on the
// next pass.
func TestForcedStartClearIsNotTheOneAction(t *testing.T) {
	t.Run("a clear and a reaction", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withMark(h.metaGrp(), reactMetaLegA)
		h.reportAt(reactMarkRevision)
		grp := withReactClone(t, h.state)
		cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnD)
		h.pass()
		calls := h.wantOps("clear_forced_start", "create_repair")
		if calls[1].grpId != reactCloneGrp {
			t.Fatalf("the create went to group %d, want %d",
				calls[1].grpId, reactCloneGrp)
		}
		if recs := h.logs.withMsg(msgForcedStartCleared); len(recs) != 1 {
			t.Fatalf("cleared records = %v, want one", recs)
		}
		h.wantApplied(reactionRepairCreate)
	})

	t.Run("a clear makes the delete wait a pass", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withMark(h.dataGrp(), reactDataLegB)
		h.reportAt(reactMarkRevision)
		withReactRetired(h, h.metaGrp(), retireLegA, retireSideA,
			h.ago(common.RetiredLegMaxAge))
		h.pass()
		h.wantOps("clear_forced_start", "delete_retired_leg")
		recs := h.logs.withMsg(msgRetiredLegKept)
		if len(recs) != 1 || recs[0]["reason"] != model.ReasonStaleRevision ||
			recs[0]["op"] != "DeleteRetiredLeg" {
			t.Fatalf("kept records = %v, want the stale delete of leg %d",
				recs, retireLegA)
		}

		// The clear commits; the primary reports an observation at its
		// revision.
		h.dataGrp().ForcedStart = nil
		h.reportAt(reactMarkRevision + 1)
		h.pass()
		calls := h.wantOps(
			"clear_forced_start", "delete_retired_leg", "delete_retired_leg")
		if calls[2].expectRev != reactMarkRevision+1 {
			t.Fatalf("delete at revision %d, want %d", calls[2].expectRev,
				reactMarkRevision+1)
		}
		if recs := h.logs.withMsg(msgRetiredLegDeleted); len(recs) != 1 {
			t.Fatalf("deleted records = %v, want one", recs)
		}
	})
}

// TestLegRepairCreateHoldsWhileAForcedStartStands pins AR8's hold on a group
// with a forced-start mark, through to its release: with the data group's
// leg A due and leg B healthy, a mark naming leg B, which the primary does
// not hold in sync while it holds in sync only leg A, whose row does not
// read OK, keeps the mark standing and holds the create, the record naming
// the mark's leg; once the report holds leg B in sync on a leg row that
// reads OK the clear runs and the snapshot's mark still holds the create
// that pass; and
// the pass after the clear's commit creates the repair. The drive step is
// not held: a worker's STARTED repair of a group with a mark is finished.
func TestLegRepairCreateHoldsWhileAForcedStartStands(t *testing.T) {
	t.Run("the hold and its release", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnD)
		withMark(h.dataGrp(), reactDataLegB)
		h.reportAt(reactMarkRevision)
		h.setGrpInfo(reactDataGrp, false, reactDataLegA)
		h.setLegRow(reactDataLegA, pb.ResStatus_RES_STATUS_ERROR)
		h.pass()
		h.wantOps()
		h.wantNoScan()
		rec := h.wantOneSkip(reactionRepairCreate, reasonGrpForcedStart,
			reactDataGrp, reactDataLegA)
		if rec["forced_leg_id"] != float64(reactDataLegB) {
			t.Fatalf("hold %v does not name the mark's leg %d", rec,
				reactDataLegB)
		}
		h.wantNoRecord(msgForcedStartCleared)

		// The primary holds leg B in sync, its row OK: the clear runs, and
		// this pass's snapshot still holds the mark.
		h.setGrpInfo(reactDataGrp, true, reactDataLegA, reactDataLegB)
		h.setLegRow(reactDataLegB, pb.ResStatus_RES_STATUS_OK)
		h.pass()
		h.wantOps("clear_forced_start")
		if recs := h.logs.withMsg(msgReactionSkipped); len(recs) != 2 ||
			recs[1]["reason"] != reasonGrpForcedStart {
			t.Fatalf("skips = %v, want the hold again", recs)
		}

		// The clear commits; the primary reports an observation at its
		// revision, and the create runs.
		h.dataGrp().ForcedStart = nil
		h.reportAt(reactMarkRevision + 1)
		h.pass()
		calls := h.wantOps("clear_forced_start", "create_repair")
		if calls[1].grpId != reactDataGrp || calls[1].badLegId != reactDataLegA {
			t.Fatalf("create = %+v, want leg %d of group %d", calls[1],
				reactDataLegA, reactDataGrp)
		}
		h.wantApplied(reactionRepairCreate)
	})

	t.Run("the drive step runs", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		repName, _ := withWorkerRepair(h, pb.RepStage_REP_STAGE_STARTED, false)
		withMark(h.dataGrp(), reactNewLeg)
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
}
