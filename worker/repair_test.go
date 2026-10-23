package worker

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The pass steps of a repair (AR11, AR12; architecture.md, Repair), and AR8
// beside a repair
// ---------------------------------------------------------------------------

const (
	// The reaction fixture's repair (withReactRepair): an operator's repair
	// of the data group whose bad leg is its first leg, with a fresh new leg
	// on a third DN.
	reactRepName = "r0"
	reactRepId   = uint64(900)
	reactNewLeg  = uint64(901)
	reactNewSide = uint64(902)

	// The reaction fixture's retired legs (withReactRetired).
	retireLegA  = uint64(950)
	retireSideA = uint64(951)
	retireLegB  = uint64(952)
	retireSideB = uint64(953)

	// reactRepSpRevision is the snapshot's SpRevision in the cases whose
	// ops take it, so a delete's expected revision names its source.
	reactRepSpRevision = uint64(31)
)

// withReactRepair gives the fixture's data group the repair r0 at stage, its
// abort flag as asked: bad leg A, unhealthy, and a fresh, provisioned new leg
// on dn C. It returns the repair.
func withReactRepair(
	h *reactHarness,
	stage pb.RepStage,
	tryToAbort bool,
) *pb.Repair {
	h.t.Helper()
	return withReactRepairOf(h, h.dataGrp(), reactDataLegA, stage, tryToAbort)
}

// withReactRepairOf is withReactRepair on a group of the fixture's slice,
// with badLegId as its bad leg.
func withReactRepairOf(
	h *reactHarness,
	grp *pb.Group,
	badLegId uint64,
	stage pb.RepStage,
	tryToAbort bool,
) *pb.Repair {
	h.t.Helper()
	rep := &pb.Repair{
		RepId:    reactRepId,
		GrpId:    grp.GetGrpId(),
		RepStage: stage,
		NewLegList: []*pb.Leg{{
			LegId:  reactNewLeg,
			LegIdx: 2,
			SideList: []*pb.Side{{
				SideId: reactNewSide, AddrPort: reactDnC, Provisioned: true,
			}},
		}},
		BadLegIdList: []uint64{badLegId},
		TryToAbort:   tryToAbort,
		SliceId:      reactSliceId,
	}
	if h.state.Repairs == nil {
		h.state.Repairs = map[string]*pb.Repair{}
	}
	h.state.Repairs[reactRepName] = rep
	h.state.Conf.RepairNameList = append(
		h.state.Conf.RepairNameList, reactRepName)
	grp.RepName = reactRepName
	h.legOf(badLegId).ErrEpoch = h.ago(9000)
	return rep
}

// setVerdict installs the primary's verdict on the repair repId
// (rep_id_to_repair_info).
func (h *reactHarness) setVerdict(repId uint64, started, abortConfirmed bool) {
	h.t.Helper()
	child, ok := h.w.cntlrs[reactCntlrA]
	if !ok {
		h.setPrimaryInfo(reactCntlrA, &pb.CntlrInfo{})
		child = h.w.cntlrs[reactCntlrA]
	}
	info := child.driver.lastInfo
	if info.RepIdToRepairInfo == nil {
		info.RepIdToRepairInfo = map[uint64]*pb.CntlrInfo_RepairInfo{}
	}
	info.RepIdToRepairInfo[repId] = &pb.CntlrInfo_RepairInfo{
		Started: started, AbortConfirmed: abortConfirmed,
	}
}

// withReactRetired gives a group of the fixture's slice a retired leg, its
// side on dn C, retired at epoch.
func withReactRetired(
	h *reactHarness,
	grp *pb.Group,
	legId uint64,
	sideId uint64,
	epoch uint64,
) *pb.RetiredLeg {
	h.t.Helper()
	retired := &pb.RetiredLeg{
		GrpId:  grp.GetGrpId(),
		LegIdx: 3,
		Side: &pb.Side{
			SideId: sideId, AddrPort: reactDnC, Provisioned: true,
		},
		RetiredEpoch: epoch,
		SliceId:      reactSliceId,
	}
	if h.state.RetiredLegs == nil {
		h.state.RetiredLegs = map[uint64]*pb.RetiredLeg{}
	}
	h.state.RetiredLegs[legId] = retired
	h.state.Conf.RetiredLegIdList = append(
		h.state.Conf.RetiredLegIdList, legId)
	grp.RetiredLegIdList = append(grp.RetiredLegIdList, legId)
	return retired
}

// wantStageRecord asserts one record msg that names the fixture repair and
// stage, and the reason when one is given.
func (h *reactHarness) wantStageRecord(
	msg string, stage pb.RepStage, reason string,
) {
	h.t.Helper()
	recs := h.logs.withMsg(msg)
	if len(recs) != 1 {
		h.t.Fatalf("%q records = %v, want one", msg, recs)
	}
	rec := recs[0]
	if rec["rep_name"] != reactRepName ||
		rec["rep_id"] != float64(reactRepId) ||
		rec["grp_id"] != float64(reactDataGrp) ||
		rec["stage"] != stage.String() ||
		rec["cntlr_id"] != float64(reactCntlrA) {
		h.t.Fatalf("record %v does not name repair %s (%d) of group %d "+
			"moving to %s on cntlr %d's report", rec, reactRepName,
			reactRepId, reactDataGrp, stage, reactCntlrA)
	}
	if reason != "" && rec["reason"] != reason {
		h.t.Fatalf("record %v, want reason %s", rec, reason)
	}
}

// wantNoRecord asserts that no record msg was logged.
func (h *reactHarness) wantNoRecord(msg string) {
	h.t.Helper()
	if recs := h.logs.withMsg(msg); len(recs) != 0 {
		h.t.Fatalf("unexpected %q records: %v", msg, recs)
	}
}

// TestRepairStageMovedFromTheVerdict pins AR11's stage move: a CREATED
// repair moves to STARTED on the current primary's verdict started, and to
// ABORTED on abort confirmed while the snapshot's record carries the abort
// flag, through the op that checks the rep_id the verdict named; a verdict
// that says neither, a repair past CREATED, a verdict under another rep_id
// and a primary that reported none move nothing, and an op's refusal is
// logged with the op.
func TestRepairStageMovedFromTheVerdict(t *testing.T) {
	t.Run("started", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
		h.setVerdict(reactRepId, true, false)
		h.pass()
		calls := h.wantOps("set_repair_stage")
		if calls[0].repName != reactRepName || calls[0].repId != reactRepId ||
			calls[0].stage != pb.RepStage_REP_STAGE_STARTED {
			t.Fatalf("stage move = %+v, want %s (%d) to STARTED",
				calls[0], reactRepName, reactRepId)
		}
		h.wantStageRecord(msgRepairStageMoved,
			pb.RepStage_REP_STAGE_STARTED, "")
		h.wantNoRecord(msgRepairStageSkipped)
	})

	t.Run("abort confirmed with the flag", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, true)
		h.setVerdict(reactRepId, false, true)
		h.pass()
		calls := h.wantOps("set_repair_stage")
		if calls[0].stage != pb.RepStage_REP_STAGE_ABORTED ||
			calls[0].repId != reactRepId {
			t.Fatalf("stage move = %+v, want ABORTED", calls[0])
		}
		h.wantStageRecord(msgRepairStageMoved,
			pb.RepStage_REP_STAGE_ABORTED, "")
	})

	t.Run("abort confirmed without the flag", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
		h.setVerdict(reactRepId, false, true)
		h.pass()
		h.wantOps()
		h.wantStageRecord(msgRepairStageSkipped,
			pb.RepStage_REP_STAGE_ABORTED, reasonAbortFlagClear)
	})

	t.Run("a verdict that says neither", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, true)
		h.setVerdict(reactRepId, false, false)
		h.pass()
		h.wantOps()
		h.wantNoRecord(msgRepairStageMoved)
		h.wantNoRecord(msgRepairStageSkipped)
	})

	for _, stage := range []pb.RepStage{
		pb.RepStage_REP_STAGE_STARTED, pb.RepStage_REP_STAGE_ABORTED,
	} {
		t.Run("a repair at "+stage.String(), func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			withReactRepair(h, stage, true)
			h.setVerdict(reactRepId, true, true)
			h.pass()
			h.wantOps()
			h.wantNoRecord(msgRepairStageMoved)
		})
	}

	// A repair finished, or cancelled, and created again under the name has
	// another rep_id: the row the primary still reports for the old one
	// moves nothing, whatever it says.
	t.Run("a stale row of another rep_id", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, true)
		h.setVerdict(reactRepId-1, true, true)
		h.pass()
		h.wantOps()
		h.wantNoRecord(msgRepairStageMoved)
		h.wantNoRecord(msgRepairStageSkipped)
	})

	t.Run("no report", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.inSyncAll = false
		withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
		h.pass()
		h.wantOps()
		h.wantNoRecord(msgRepairStageMoved)
	})

	// A cntlr whose child drives the standby plan is not the pass's primary,
	// so its verdict is never read (AR1).
	t.Run("a standby's report", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.inSyncAll = false
		withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
		h.w.cntlrs[reactCntlrA] = &cntlrChild{
			plan: &cntlrPlan{cntlrId: reactCntlrA},
			driver: &cntlrDriver{lastInfo: &pb.CntlrInfo{
				RepIdToRepairInfo: map[uint64]*pb.CntlrInfo_RepairInfo{
					reactRepId: {Started: true},
				},
			}},
		}
		h.pass()
		h.wantOps()
		h.wantNoRecord(msgRepairStageMoved)
	})

	t.Run("the op refuses", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
		h.setVerdict(reactRepId, true, false)
		h.rops.stageErr = &model.ErrPrecondition{
			Op: "SetRepairStage", Reason: "repair id changed",
		}
		h.pass()
		h.wantOps("set_repair_stage")
		h.wantStageRecord(msgRepairStageSkipped,
			pb.RepStage_REP_STAGE_STARTED, "repair id changed")
		if rec := h.logs.withMsg(msgRepairStageSkipped)[0]; rec["op"] !=
			"SetRepairStage" {
			t.Fatalf("refusal record %v does not name the op", rec)
		}
		h.wantNoRecord(msgRepairStageMoved)
	})

	t.Run("the op fails", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
		h.setVerdict(reactRepId, true, false)
		h.rops.stageErr = errors.New("etcd unavailable")
		h.pass()
		h.wantOps("set_repair_stage")
		h.wantStageRecord(msgRepairStageSkipped,
			pb.RepStage_REP_STAGE_STARTED, reasonOpFailed)
	})
}

// TestRepairAbortRaceBothOrders pins the two orders of the abort race
// (architecture.md, Repair): the primary added the new leg in the round the
// abort flag was set, so it reports started for a repair whose flag is set,
// and the worker moves it to STARTED, the started verdict winning over the
// flag even when the row says abort confirmed as well; or the flag reached
// the primary first, which never adds that new leg and confirms the abort,
// and the worker moves it to ABORTED.
func TestRepairAbortRaceBothOrders(t *testing.T) {
	cases := []struct {
		name                    string
		started, abortConfirmed bool
		want                    pb.RepStage
	}{
		{"the add first", true, false, pb.RepStage_REP_STAGE_STARTED},
		{"started wins over abort confirmed", true, true,
			pb.RepStage_REP_STAGE_STARTED},
		{"the flag first", false, true, pb.RepStage_REP_STAGE_ABORTED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, true)
			h.setVerdict(reactRepId, tc.started, tc.abortConfirmed)
			h.pass()
			calls := h.wantOps("set_repair_stage")
			if calls[0].stage != tc.want {
				t.Fatalf("moved to %s, want %s", calls[0].stage, tc.want)
			}
			h.wantStageRecord(msgRepairStageMoved, tc.want, "")
		})
	}
}

// TestRepairStageMovesAtEveryLevel pins AR11's placement in the pass: a stage
// move runs at every sp_level, the levels where reactions are suppressed
// included (AR3), where neither the retire timer nor a reaction runs; and a
// deleting pool runs its drain step and no stage move.
func TestRepairStageMovesAtEveryLevel(t *testing.T) {
	for _, level := range []pb.SpLevel{
		pb.SpLevel_SP_LEVEL_NO_THINPOOL, pb.SpLevel_SP_LEVEL_DISABLE,
	} {
		t.Run(level.String(), func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.state.Conf.SpLevel = level
			withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
			h.setVerdict(reactRepId, true, false)
			// An old retired leg the timer would delete, and leg B due for
			// a repair the reactions would start: neither runs here.
			h.setGrpRow(reactMetaGrp, pb.ResStatus_RES_STATUS_OK)
			withReactRetired(h, h.metaGrp(), retireLegA,
				retireSideA, h.ago(common.RetiredLegMaxAge))
			h.dnCands(reactDnD)
			h.pass()
			h.wantOps("set_repair_stage")
			h.wantStageRecord(msgRepairStageMoved,
				pb.RepStage_REP_STAGE_STARTED, "")
			h.wantNoRecord(msgRetiredLegDeleted)
			h.wantNoRecord(msgRetiredLegKept)
		})
	}

	t.Run("a deleting pool", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.state.Conf.Deleting = true
		withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
		h.setVerdict(reactRepId, true, false)
		h.pass()
		h.wantOps("drain_cntlrs")
		h.wantNoRecord(msgRepairStageMoved)
	})
}

// TestRepairPassStepsAreNotTheOneAction pins AR2's count against AR11 and
// AR12: a stage move and a reaction commit in one pass, and so do the retire
// timer's delete and a reaction, neither pass step being the pass's one
// action; but a stage move bumps SpRev, so the timer's delete in the same
// pass, which carries the snapshot's revision, is refused as stale, logged
// with the op's refusal, and commits on the next pass.
func TestRepairPassStepsAreNotTheOneAction(t *testing.T) {
	// The clone meta group's leg A is due for a spare create.
	withDueCloneLeg := func(h *reactHarness) {
		grp := withReactClone(t, h.state)
		cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnD)
	}
	// The meta group's retired leg is old on a whole group, in a report of
	// an observation at the snapshot's revision.
	withOldRetired := func(h *reactHarness) {
		h.state.SpRevision = reactRepSpRevision
		h.setGrpRow(reactMetaGrp, pb.ResStatus_RES_STATUS_OK)
		h.w.cntlrs[reactCntlrA].driver.infoRev = reactRepSpRevision
		withReactRetired(h, h.metaGrp(), retireLegA, retireSideA,
			h.ago(common.RetiredLegMaxAge))
	}

	t.Run("a stage move and a reaction", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
		h.setVerdict(reactRepId, true, false)
		withDueCloneLeg(h)
		h.pass()
		calls := h.wantOps("set_repair_stage", "create_clone_spare")
		if calls[1].grpId != reactCloneGrp {
			t.Fatalf("the create went to group %d, want %d",
				calls[1].grpId, reactCloneGrp)
		}
		h.wantStageRecord(msgRepairStageMoved,
			pb.RepStage_REP_STAGE_STARTED, "")
		h.wantApplied(reactionSpareCreate)
	})

	t.Run("the timer's delete and a reaction", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withOldRetired(h)
		withDueCloneLeg(h)
		h.pass()
		calls := h.wantOps("delete_retired_leg", "create_clone_spare")
		if calls[0].legIds[0] != retireLegA ||
			calls[1].grpId != reactCloneGrp {
			t.Fatalf("calls = %+v, want leg %d deleted and a create for "+
				"group %d", calls, retireLegA, reactCloneGrp)
		}
		if recs := h.logs.withMsg(msgRetiredLegDeleted); len(recs) != 1 {
			t.Fatalf("deleted records = %v, want one", recs)
		}
		h.wantApplied(reactionSpareCreate)
	})

	t.Run("a stage move makes the delete wait a pass", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		rep := withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
		h.setVerdict(reactRepId, true, false)
		withOldRetired(h)
		h.pass()
		h.wantOps("set_repair_stage", "delete_retired_leg")
		recs := h.logs.withMsg(msgRetiredLegKept)
		if len(recs) != 1 || recs[0]["reason"] != model.ReasonStaleRevision ||
			recs[0]["op"] != "DeleteRetiredLeg" ||
			recs[0]["leg_id"] != float64(retireLegA) {
			t.Fatalf("kept records = %v, want the stale delete of leg %d",
				recs, retireLegA)
		}
		h.wantNoRecord(msgRetiredLegDeleted)

		// The move commits; the primary reports an observation at its
		// revision.
		rep.RepStage = pb.RepStage_REP_STAGE_STARTED
		h.state.SpRevision = reactRepSpRevision + 1
		h.w.cntlrs[reactCntlrA].driver.infoRev = reactRepSpRevision + 1
		h.pass()
		calls := h.wantOps(
			"set_repair_stage", "delete_retired_leg", "delete_retired_leg")
		if calls[2].expectRev != reactRepSpRevision+1 {
			t.Fatalf("delete at revision %d, want %d", calls[2].expectRev,
				reactRepSpRevision+1)
		}
		if recs := h.logs.withMsg(msgRetiredLegDeleted); len(recs) != 1 {
			t.Fatalf("deleted records = %v, want one", recs)
		}
	})
}

// TestRepairStageSkipsADeletingClone pins AR11 on a clone meta group: a
// deleting clone's repair moves no stage, its verdict logged as skipped,
// since the clone drain's final transaction gives it back with the group
// (CLD9), and so does the repair of a clone the snapshot does not hold; a
// live clone's repair moves as a slice's group's does.
func TestRepairStageSkipsADeletingClone(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		name := "a live clone"
		if deleting {
			name = "a deleting clone"
		}
		t.Run(name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			withReactClone(t, h.state)
			h.state.Clones[reactCloneNm].Deleting = deleting
			h.state.Clones[reactCloneNm].MetaGrp.RepName = reactRepName
			h.state.Repairs = map[string]*pb.Repair{reactRepName: {
				RepId:        reactRepId,
				GrpId:        reactCloneGrp,
				RepStage:     pb.RepStage_REP_STAGE_CREATED,
				BadLegIdList: []uint64{reactCloneLegA},
				CloneName:    reactCloneNm,
			}}
			h.state.Conf.RepairNameList = []string{reactRepName}
			h.setVerdict(reactRepId, true, false)
			h.pass()
			if deleting {
				// The clone drain's final step runs in the same pass
				// (CLD7); the repair moves no stage.
				h.wantOps("finish_clone")
				recs := h.logs.withMsg(msgRepairStageSkipped)
				if len(recs) != 1 || recs[0]["reason"] != reasonCloneDeleting {
					t.Fatalf("skips = %v, want one %s", recs,
						reasonCloneDeleting)
				}
				return
			}
			calls := h.wantOps("set_repair_stage")
			if calls[0].stage != pb.RepStage_REP_STAGE_STARTED {
				t.Fatalf("moved to %s", calls[0].stage)
			}
		})
	}

	// A repair whose clone the snapshot does not hold is skipped the same
	// way: every repair op refuses it (MD6).
	t.Run("a clone the snapshot does not hold", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.state.Repairs = map[string]*pb.Repair{reactRepName: {
			RepId:        reactRepId,
			GrpId:        reactCloneGrp,
			RepStage:     pb.RepStage_REP_STAGE_CREATED,
			BadLegIdList: []uint64{reactCloneLegA},
			CloneName:    reactCloneNm,
		}}
		h.state.Conf.RepairNameList = []string{reactRepName}
		h.setVerdict(reactRepId, true, false)
		h.pass()
		h.wantOps()
		recs := h.logs.withMsg(msgRepairStageSkipped)
		if len(recs) != 1 || recs[0]["reason"] != reasonCloneDeleting {
			t.Fatalf("skips = %v, want one %s", recs, reasonCloneDeleting)
		}
	})
}

// retireCase is the shape TestRetireTimer starts each case from: the data
// group's retired leg A at the age, the group's row OK and whole with both
// legs in sync in a report of an observation at the snapshot's revision,
// and every leg and side of the group healthy.
func retireCase(t *testing.T) *reactHarness {
	h := newReactHarness(t, reactFixture(t))
	h.state.SpRevision = reactRepSpRevision
	h.setGrpRow(reactDataGrp, pb.ResStatus_RES_STATUS_OK)
	h.w.cntlrs[reactCntlrA].driver.infoRev = reactRepSpRevision
	h.setGrpInfo(reactDataGrp, true, reactDataLegA, reactDataLegB)
	withReactRetired(h, h.dataGrp(), retireLegA, retireSideA,
		h.ago(common.RetiredLegMaxAge))
	return h
}

// TestRetireTimer pins AR12 with the fake clock: a retired leg at least
// common.RetiredLegMaxAge old by its retired_epoch is deleted, with the
// snapshot's revision as the delete's expected revision and the cluster's
// stored conf, when its group has no repair, its row in the current
// primary's latest report reads OK, that report is of an observation at the
// snapshot's revision or later and sets its whole flag, and no leg of its
// leg_list nor that leg's side has an err_epoch; each of those conditions
// that fails keeps it, logged with its own reason; a younger leg is passed
// over with no record.
func TestRetireTimer(t *testing.T) {
	t.Run("deleted at the age", func(t *testing.T) {
		h := retireCase(t)
		h.pass()
		calls := h.wantOps("delete_retired_leg")
		if !slices.Equal(calls[0].legIds, []uint64{retireLegA}) ||
			calls[0].expectRev != reactRepSpRevision ||
			calls[0].cc == nil {
			t.Fatalf("delete = %+v, want leg %d at revision %d with the "+
				"cluster conf", calls[0], retireLegA, reactRepSpRevision)
		}
		recs := h.logs.withMsg(msgRetiredLegDeleted)
		if len(recs) != 1 || recs[0]["leg_id"] != float64(retireLegA) ||
			recs[0]["grp_id"] != float64(reactDataGrp) {
			t.Fatalf("deleted records = %v", recs)
		}
		h.wantNoRecord(msgRetiredLegKept)
	})

	// The clock decides: a second short of the age, nothing and no record;
	// once the clock has moved one second on, the next pass deletes it.
	t.Run("the clock reaches the age", func(t *testing.T) {
		h := retireCase(t)
		h.state.RetiredLegs[retireLegA].RetiredEpoch =
			h.ago(common.RetiredLegMaxAge - 1)
		h.pass()
		h.wantOps()
		h.wantNoRecord(msgRetiredLegKept)
		h.wantNoRecord(msgRetiredLegDeleted)
		h.clk.advance(time.Second)
		h.pass()
		h.wantOps("delete_retired_leg")
	})

	holds := []struct {
		name   string
		edit   func(h *reactHarness)
		reason string
	}{
		{"the group has a repair", func(h *reactHarness) {
			h.dataGrp().RepName = reactRepName
		}, reasonGrpHasRepair},
		{"the row reads ERROR", func(h *reactHarness) {
			h.setGrpRow(reactDataGrp, pb.ResStatus_RES_STATUS_ERROR)
		}, reasonGrpRowError},
		{"the row reads UNKNOWN", func(h *reactHarness) {
			h.setGrpRow(reactDataGrp, pb.ResStatus_RES_STATUS_UNKNOWN)
		}, reasonGrpRowUnknown},
		{"the report has no row", func(h *reactHarness) {
			delete(h.w.cntlrs[reactCntlrA].driver.lastInfo.GrpIdToMdRaid,
				reactDataGrp)
		}, reasonGrpRowAbsent},
		{"the report is of an older observation", func(h *reactHarness) {
			h.w.cntlrs[reactCntlrA].driver.infoRev = reactRepSpRevision - 1
		}, reasonGrpRowStale},
		{"the group is not whole", func(h *reactHarness) {
			h.setGrpInfo(reactDataGrp, false, reactDataLegA, reactDataLegB)
		}, reasonGrpNotWhole},
		{"the report has no reading of the group", func(h *reactHarness) {
			h.inSyncAll = false
			delete(h.w.cntlrs[reactCntlrA].driver.lastInfo.GrpIdToGrpInfo,
				reactDataGrp)
		}, reasonGrpNotWhole},
		{"a leg has an err_epoch", func(h *reactHarness) {
			h.legOf(reactDataLegB).ErrEpoch = h.ago(1)
		}, reasonGrpLegUnhealthy},
		{"a leg's side has an err_epoch", func(h *reactHarness) {
			h.legOf(reactDataLegA).SideList[0].ErrEpoch = h.ago(1)
		}, reasonGrpLegUnhealthy},
		{"the group is gone", func(h *reactHarness) {
			h.state.RetiredLegs[retireLegA].GrpId = 7777
		}, reasonGrpNotFound},
	}
	for _, tc := range holds {
		t.Run(tc.name, func(t *testing.T) {
			h := retireCase(t)
			tc.edit(h)
			h.pass()
			h.wantOps()
			recs := h.logs.withMsg(msgRetiredLegKept)
			if len(recs) != 1 || recs[0]["reason"] != tc.reason ||
				recs[0]["leg_id"] != float64(retireLegA) {
				t.Fatalf("kept records = %v, want one %s for leg %d",
					recs, tc.reason, retireLegA)
			}
			h.wantNoRecord(msgRetiredLegDeleted)
		})
	}

	// A spare S took leg A's place in leg_list, and A is parked with its
	// health epoch; the snapshot is one revision past the primary's latest
	// report, which still reads the group whole with A and B in sync. md
	// may not have started to copy onto S yet, so B may be the one leg in
	// sync, and the whole flag of that report is the old leg set's: the
	// leg waits for the primary's report of the switch's revision, and is
	// deleted once the report shows S in sync.
	t.Run("a switch the report does not show yet", func(t *testing.T) {
		const spareS, spareSide = uint64(700), uint64(800)
		h := retireCase(t)
		grp := h.dataGrp()
		legA := grp.LegList[0]
		legA.ErrEpoch = h.ago(9000)
		grp.LegList[0] = &pb.Leg{
			LegId: spareS, LegIdx: legA.GetLegIdx(),
			SideList: []*pb.Side{{
				SideId: spareSide, AddrPort: reactDnC, Provisioned: true,
			}},
		}
		grp.SpareLegList = append(grp.SpareLegList, legA)
		h.state.SpRevision = reactRepSpRevision + 1
		h.pass()
		h.wantOps()
		recs := h.logs.withMsg(msgRetiredLegKept)
		if len(recs) != 1 || recs[0]["reason"] != reasonGrpRowStale {
			t.Fatalf("kept records = %v, want one %s", recs,
				reasonGrpRowStale)
		}

		h.w.cntlrs[reactCntlrA].driver.infoRev = reactRepSpRevision + 1
		h.setGrpInfo(reactDataGrp, true, spareS, reactDataLegB)
		h.pass()
		h.wantOps("delete_retired_leg")
	})

	t.Run("no report", func(t *testing.T) {
		h := retireCase(t)
		h.inSyncAll = false
		delete(h.w.cntlrs, reactCntlrA)
		h.pass()
		h.wantOps()
		recs := h.logs.withMsg(msgRetiredLegKept)
		if len(recs) != 1 || recs[0]["reason"] != reasonGrpRowAbsent {
			t.Fatalf("kept records = %v, want one %s", recs,
				reasonGrpRowAbsent)
		}
	})

	t.Run("a deleting clone's retired leg", func(t *testing.T) {
		h := retireCase(t)
		grp := withReactClone(t, h.state)
		h.state.Clones[reactCloneNm].Deleting = true
		h.state.Conf.RetiredLegIdList = nil
		h.dataGrp().RetiredLegIdList = nil
		delete(h.state.RetiredLegs, retireLegA)
		retired := withReactRetired(h, grp, retireLegB, retireSideB,
			h.ago(common.RetiredLegMaxAge))
		retired.SliceId, retired.CloneName = 0, reactCloneNm
		h.setGrpRow(reactCloneGrp, pb.ResStatus_RES_STATUS_OK)
		h.pass()
		// The clone drain's final step runs in the same pass (CLD7) and
		// gives the retired leg back with the group (CLD9).
		h.wantOps("finish_clone")
		recs := h.logs.withMsg(msgRetiredLegKept)
		if len(recs) != 1 || recs[0]["reason"] != reasonCloneDeleting {
			t.Fatalf("kept records = %v, want one %s", recs,
				reasonCloneDeleting)
		}
	})

	t.Run("the op refuses", func(t *testing.T) {
		h := retireCase(t)
		h.rops.retireErr = &model.ErrPrecondition{
			Op: "DeleteRetiredLeg", Reason: model.ReasonStaleRevision,
		}
		h.pass()
		h.wantOps("delete_retired_leg")
		recs := h.logs.withMsg(msgRetiredLegKept)
		if len(recs) != 1 || recs[0]["reason"] != model.ReasonStaleRevision ||
			recs[0]["op"] != "DeleteRetiredLeg" {
			t.Fatalf("kept records = %v, want the op's refusal", recs)
		}
		h.wantNoRecord(msgRetiredLegDeleted)
	})
}

// TestRetireTimerDeletesOneAPass pins AR12's count: one deletion a pass, the
// oldest retired leg first by its retired_epoch whatever the list order, then
// by leg id, the next on the next pass; and a held old leg does not keep a
// younger one that may go from going.
func TestRetireTimerDeletesOneAPass(t *testing.T) {
	t.Run("the oldest first", func(t *testing.T) {
		h := retireCase(t)
		h.setGrpRow(reactMetaGrp, pb.ResStatus_RES_STATUS_OK)
		h.setGrpInfo(reactMetaGrp, true, reactMetaLegA, reactMetaLegB)
		// B, listed after A, retired a day earlier.
		withReactRetired(h, h.metaGrp(), retireLegB, retireSideB,
			h.ago(common.RetiredLegMaxAge+24*3600))
		h.pass()
		calls := h.wantOps("delete_retired_leg")
		if calls[0].legIds[0] != retireLegB {
			t.Fatalf("deleted leg %d first, want the older %d",
				calls[0].legIds[0], retireLegB)
		}
		// The delete commits.
		h.state.Conf.RetiredLegIdList = []uint64{retireLegA}
		delete(h.state.RetiredLegs, retireLegB)
		h.pass()
		calls = h.wantOps("delete_retired_leg", "delete_retired_leg")
		if calls[1].legIds[0] != retireLegA {
			t.Fatalf("second pass deleted leg %d, want %d",
				calls[1].legIds[0], retireLegA)
		}
	})

	// Finishes in different groups can leave the list out of leg-id order
	// at equal epochs: the lower leg id goes first.
	t.Run("equal epochs by leg id", func(t *testing.T) {
		h := retireCase(t)
		h.setGrpRow(reactMetaGrp, pb.ResStatus_RES_STATUS_OK)
		h.setGrpInfo(reactMetaGrp, true, reactMetaLegA, reactMetaLegB)
		h.state.Conf.RetiredLegIdList = nil
		h.dataGrp().RetiredLegIdList = nil
		delete(h.state.RetiredLegs, retireLegA)
		epoch := h.ago(common.RetiredLegMaxAge)
		withReactRetired(h, h.dataGrp(), retireLegB, retireSideB, epoch)
		withReactRetired(h, h.metaGrp(), retireLegA, retireSideA, epoch)
		h.pass()
		calls := h.wantOps("delete_retired_leg")
		if calls[0].legIds[0] != retireLegA {
			t.Fatalf("deleted leg %d first, want the lower id %d",
				calls[0].legIds[0], retireLegA)
		}
	})

	t.Run("a held leg passes the turn", func(t *testing.T) {
		h := retireCase(t)
		h.setGrpRow(reactMetaGrp, pb.ResStatus_RES_STATUS_OK)
		h.setGrpInfo(reactMetaGrp, false, reactMetaLegA, reactMetaLegB)
		withReactRetired(h, h.metaGrp(), retireLegB, retireSideB,
			h.ago(common.RetiredLegMaxAge+24*3600))
		h.pass()
		calls := h.wantOps("delete_retired_leg")
		if calls[0].legIds[0] != retireLegA {
			t.Fatalf("deleted leg %d, want %d past the held older one",
				calls[0].legIds[0], retireLegA)
		}
		recs := h.logs.withMsg(msgRetiredLegKept)
		if len(recs) != 1 || recs[0]["leg_id"] != float64(retireLegB) ||
			recs[0]["reason"] != reasonGrpNotWhole {
			t.Fatalf("kept records = %v, want leg %d not whole", recs,
				retireLegB)
		}
	})

	t.Run("a failed delete ends the step", func(t *testing.T) {
		h := retireCase(t)
		h.setGrpRow(reactMetaGrp, pb.ResStatus_RES_STATUS_OK)
		h.setGrpInfo(reactMetaGrp, true, reactMetaLegA, reactMetaLegB)
		withReactRetired(h, h.metaGrp(), retireLegB, retireSideB,
			h.ago(common.RetiredLegMaxAge))
		h.rops.retireErr = errors.New("etcd unavailable")
		h.pass()
		h.wantOps("delete_retired_leg")
	})
}

// TestLegRepairSkipsAGroupWithARepair pins AR8's skip of a group that has a
// repair: its due leg gets no switch and no create, the pass logs `reaction
// skipped` with the group's own reason on every pass, naming the repair, and
// goes on to the next candidate, in another group, which is repaired in the
// same pass. The repaired group's due leg comes first in leg-id order, so
// the skip runs before the other group's create.
func TestLegRepairSkipsAGroupWithARepair(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	withReactRepairOf(h, h.metaGrp(), reactMetaLegA,
		pb.RepStage_REP_STAGE_CREATED, false)
	// A ready spare in the repaired group would be switched in but for the
	// repair.
	h.metaGrp().SpareLegList = append(h.metaGrp().SpareLegList, &pb.Leg{
		LegId: 700, LegIdx: 3,
		SideList: []*pb.Side{{
			SideId: 800, AddrPort: reactDnD, Provisioned: true,
		}},
	})
	h.setLegRow(700, pb.ResStatus_RES_STATUS_OK)
	h.setGrpRow(reactMetaGrp, pb.ResStatus_RES_STATUS_OK)
	// The data group, which has no repair, has a due leg after it.
	h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
	h.dnCands(reactDnD)
	var want []string
	for pass := 1; pass <= 2; pass++ {
		h.pass()
		want = append(want, "create_spare")
		calls := h.wantOps(want...)
		if calls[pass-1].grpId != reactDataGrp {
			t.Fatalf("pass %d: created for group %d, want %d", pass,
				calls[pass-1].grpId, reactDataGrp)
		}
		recs := h.logs.withMsg(msgReactionSkipped)
		if len(recs) != pass {
			t.Fatalf("pass %d: skips = %v, want one a pass", pass, recs)
		}
		rec := recs[pass-1]
		if rec["kind"] != reactionSpareCreate ||
			rec["reason"] != reasonGrpHasRepair ||
			rec["rep_name"] != reactRepName ||
			rec["leg_id"] != float64(reactMetaLegA) {
			t.Fatalf("pass %d: skip %v, want %s for leg %d naming %s",
				pass, rec, reasonGrpHasRepair, reactMetaLegA, reactRepName)
		}
	}
}

// TestLegRepairCountsOnlyLegsInSync pins AR8's healthy-leg test: another leg
// of the group's leg_list counts only when it has no err_epoch AND the
// current primary's latest report holds it in sync. A healthy leg md still
// copies, a report that holds only the due leg in sync, and a report with no
// reading of the group all hold the group with no_healthy_leg, creating and
// switching nothing; a leg in sync lets the repair go on.
func TestLegRepairCountsOnlyLegsInSync(t *testing.T) {
	cases := []struct {
		name   string
		inSync []uint64
		read   bool
		want   bool
	}{
		{"the other leg in sync", []uint64{reactDataLegA, reactDataLegB},
			true, true},
		{"the other leg still copying", []uint64{reactDataLegA}, true, false},
		{"no leg in sync", nil, true, false},
		{"no reading of the group", nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.inSyncAll = false
			h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
			h.setGrpRow(reactDataGrp, pb.ResStatus_RES_STATUS_OK)
			if tc.read {
				h.setGrpInfo(reactDataGrp, true, tc.inSync...)
			}
			h.dnCands(reactDnD)
			h.pass()
			if tc.want {
				h.wantOps("create_spare")
				return
			}
			h.wantOps()
			h.wantOneSkip(reactionSpareCreate, reasonNoHealthyLeg,
				reactDataGrp, reactDataLegA, 0)
		})
	}

	// The report's in-sync list does not lift an err_epoch: a leg md holds
	// in sync whose probe fails is no healthy leg.
	t.Run("in sync with an err_epoch", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.inSyncAll = false
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.legOf(reactDataLegB).ErrEpoch = h.ago(1)
		h.setGrpRow(reactDataGrp, pb.ResStatus_RES_STATUS_OK)
		h.setGrpInfo(reactDataGrp, true, reactDataLegA, reactDataLegB)
		h.dnCands(reactDnD)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionSpareCreate, reasonNoHealthyLeg)
	})
}

// TestVerdictMemoKeepsRepairObjects pins AR10's memo against a repair's
// objects (architecture.md, Repair): a verdict on a repair's new leg, on its
// side or on a retired side is kept while the pass's snapshot lists the
// object, and dropped once the snapshot no longer does.
func TestVerdictMemoKeepsRepairObjects(t *testing.T) {
	state := spFixture()
	withSpRepair(state, false)
	withSpRetired(state)
	memo := newVerdictMemo()
	newSide := sideKey{legId: spRepNewLeg, sideId: spRepNewSide}
	retired := sideKey{legId: spRetiredLeg, sideId: spRetiredSide}
	memo.noteLeg(spRepNewLeg, true)
	memo.noteSide(newSide, true)
	memo.noteSide(retired, true)
	got := memo.snapshot(state)
	if !got.legs[spRepNewLeg] || !got.sides[newSide] || !got.sides[retired] {
		t.Fatalf("snapshot %+v lost a listed repair object's verdict", got)
	}
	got = memo.snapshot(spFixture())
	if got.legs[spRepNewLeg] || got.sides[newSide] || got.sides[retired] {
		t.Fatalf("snapshot %+v kept the verdicts of objects gone", got)
	}
}

// TestReseedHealthCoversRepairObjects pins the pass's half of HL3 on a
// repair's objects: the load's err_epoch of a new leg seeds the coordinator's
// monitor of it, and that of the new leg's side and of a retired side is
// offered to their children, so another observer's stamp is corrected at the
// owner's next clean verdict.
func TestReseedHealthCoversRepairObjects(t *testing.T) {
	captureLogs(t)
	ctx := context.Background()
	d := newTestDeps(
		testConfig(common.WorkerRoleSp), newFakeStore(), newFakeClock(),
	)
	hw := &fakeHealthWriter{}
	d.health = hw
	w := spTestWorker(d)
	newSideMon := newRepairSideMonitor(d, testCid, testSpId, testSpName,
		spRepName, spRepId, spRepNewSide)
	w.sides[sideKey{legId: spRepNewLeg, sideId: spRepNewSide}] =
		&sideChild{driver: &sideDriver{health: newSideMon}}
	retiredMon := newRetiredSideMonitor(d, testCid, testSpId, testSpName,
		spRetiredLeg, spRetiredSide)
	w.sides[sideKey{legId: spRetiredLeg, sideId: spRetiredSide}] =
		&sideChild{driver: &sideDriver{health: retiredMon}}
	legMon := newRepairLegMonitor(d, testCid, testSpId, testSpName,
		spRepName, spRepId, spRepNewLeg)
	w.legs[spRepNewLeg] = legMon
	for _, mon := range []*healthMonitor{newSideMon, retiredMon, legMon} {
		mon.observe(ctx, healthClean, "")
	}
	state := spFixture()
	withSpRepair(state, false)
	withSpRetired(state)
	state.Repairs[spRepName].NewLegList[0].ErrEpoch = 1
	state.Repairs[spRepName].NewLegList[0].SideList[0].ErrEpoch = 1
	state.RetiredLegs[spRetiredLeg].Side.ErrEpoch = 1
	w.reseedHealth(state, w.healthSeqs())
	for _, mon := range []*healthMonitor{newSideMon, retiredMon, legMon} {
		mon.observe(ctx, healthClean, "")
	}
	clears := map[string]int{}
	for _, write := range hw.all() {
		if write.epoch != 0 {
			continue
		}
		switch {
		case write.retiredLegId == spRetiredLeg:
			clears["retired side"]++
		case write.record == healthRecordSide && write.repName == spRepName:
			clears["new side"]++
		case write.record == healthRecordLeg && write.repName == spRepName:
			clears["new leg"]++
		}
	}
	for _, kind := range []string{"new side", "retired side", "new leg"} {
		if clears[kind] != 2 {
			t.Errorf("%d %s clears, want 2: the stamp was not offered",
				clears[kind], kind)
		}
	}
}

// TestHealthRepairMonitorsWriteToTheirRecords pins the HL2 monitors of a
// repair's new leg, of its side and of a retired side: each writes through
// the epoch write of the record that owns it, the repair by its name and
// rep_id or the retired leg by its leg id, with the pool's name, a set and a
// clear alike, and its record names that owner.
func TestHealthRepairMonitorsWriteToTheirRecords(t *testing.T) {
	logs := captureLogs(t)
	d, writer := healthTestDeps(t)
	ctx := context.Background()
	monitors := []*healthMonitor{
		newRepairLegMonitor(d, 1, 2, "sp", "r", 9, 5),
		newRepairSideMonitor(d, 1, 2, "sp", "r", 9, 6),
		newRetiredSideMonitor(d, 1, 2, "sp", 7, 8),
	}
	for _, mon := range monitors {
		mon.observe(ctx, healthErrorRow, "row")
	}
	for _, mon := range monitors {
		mon.observe(ctx, healthClean, "")
	}
	writes := writer.all()
	if len(writes) != 6 {
		t.Fatalf("writes = %+v, want a set and a clear of each", writes)
	}
	for i, want := range []healthWrite{
		{record: healthRecordLeg, repName: "r", repId: 9, objId: 5},
		{record: healthRecordSide, repName: "r", repId: 9, objId: 6},
		{record: healthRecordSide, retiredLegId: 7, objId: 8},
	} {
		for j, got := range []healthWrite{writes[i], writes[i+3]} {
			want.cid, want.spId, want.spName = 1, 2, "sp"
			epoch := got.epoch
			got.epoch = 0
			if got != want || (epoch != 0) != (j == 0) {
				t.Fatalf("write %+v (epoch %d), want %+v, set %v",
					got, epoch, want, j == 0)
			}
		}
	}
	for _, rec := range logs.withMsg(msgHealthChanged) {
		_, rep := rec["rep_name"]
		_, retired := rec["retired_leg_id"]
		if !rep && !retired {
			t.Fatalf("record without its owner: %v", rec)
		}
		if _, ok := rec["slice_id"]; ok {
			t.Fatalf("a repair object's record names a slice: %v", rec)
		}
	}
}

// ---------------------------------------------------------------------------
// A repair's sides and legs in the sp role (RW14-RW18, HL2, HL3)
// ---------------------------------------------------------------------------

const (
	// The sp fixture's repair (withSpRepair): an operator's repair of slice
	// B's data group, whose bad leg is spLegB, with a fresh new leg on dn C.
	spRepName    = "r0"
	spRepId      = uint64(3000)
	spRepNewLeg  = uint64(3001)
	spRepNewSide = uint64(3002)

	// The sp fixture's retired leg of the same group (withSpRetired), its
	// side on dn D.
	spRetiredLeg  = uint64(3010)
	spRetiredSide = uint64(3011)

	// A deleting clone's repair and retired leg (withDeletingCloneRepair),
	// on withDeletingClone's group.
	spGoneRepName     = "rc"
	spGoneRepId       = uint64(3100)
	spGoneNewLeg      = uint64(3101)
	spGoneNewSide     = uint64(3102)
	spGoneRetired     = uint64(3110)
	spGoneRetiredSide = uint64(3111)
)

// withSpRepair gives slice B's data group the repair r0, CREATED, with a
// fresh new leg on dn C whose side is provisioned as asked.
func withSpRepair(state *model.SpState, provisioned bool) *pb.Repair {
	rep := &pb.Repair{
		RepId:    spRepId,
		GrpId:    110,
		RepStage: pb.RepStage_REP_STAGE_CREATED,
		NewLegList: []*pb.Leg{{
			LegId:  spRepNewLeg,
			LegIdx: 1,
			SideList: []*pb.Side{{
				SideId:      spRepNewSide,
				AddrPort:    spDnC,
				CntlidSlot:  15000,
				Provisioned: provisioned,
			}},
		}},
		BadLegIdList: []uint64{spLegB},
		SliceId:      spSliceB,
	}
	if state.Repairs == nil {
		state.Repairs = map[string]*pb.Repair{}
	}
	state.Repairs[spRepName] = rep
	state.Conf.RepairNameList = append(state.Conf.RepairNameList, spRepName)
	state.Slices[spSliceB].DataGrpList[0].RepName = spRepName
	return rep
}

// withSpRetired gives slice B's data group the retired leg 3010, its side on
// dn D.
func withSpRetired(state *model.SpState) *pb.RetiredLeg {
	retired := &pb.RetiredLeg{
		GrpId:  110,
		LegIdx: 2,
		Side: &pb.Side{
			SideId:      spRetiredSide,
			AddrPort:    spDnD,
			CntlidSlot:  10000,
			Provisioned: true,
		},
		RetiredEpoch: 1,
		SliceId:      spSliceB,
	}
	if state.RetiredLegs == nil {
		state.RetiredLegs = map[uint64]*pb.RetiredLeg{}
	}
	state.RetiredLegs[spRetiredLeg] = retired
	state.Conf.RetiredLegIdList = append(
		state.Conf.RetiredLegIdList, spRetiredLeg)
	grp := state.Slices[spSliceB].DataGrpList[0]
	grp.RetiredLegIdList = append(grp.RetiredLegIdList, spRetiredLeg)
	return retired
}

// withDeletingCloneRepair gives withDeletingClone's group a repair, with an
// unprovisioned new leg on dn D, and a retired leg on dn B.
func withDeletingCloneRepair(state *model.SpState) {
	grp := state.Clones[spCloneGoneNm].MetaGrp
	grp.RepName = spGoneRepName
	if state.Repairs == nil {
		state.Repairs = map[string]*pb.Repair{}
	}
	state.Repairs[spGoneRepName] = &pb.Repair{
		RepId:    spGoneRepId,
		GrpId:    spCloneGoneGrp,
		RepStage: pb.RepStage_REP_STAGE_CREATED,
		NewLegList: []*pb.Leg{
			cloneLeg(spGoneNewLeg, spGoneNewSide, spDnD, false),
		},
		BadLegIdList: []uint64{spCloneGoneLeg},
		CloneName:    spCloneGoneNm,
	}
	state.Conf.RepairNameList = append(
		state.Conf.RepairNameList, spGoneRepName)
	if state.RetiredLegs == nil {
		state.RetiredLegs = map[uint64]*pb.RetiredLeg{}
	}
	state.RetiredLegs[spGoneRetired] = &pb.RetiredLeg{
		GrpId: spCloneGoneGrp,
		Side: &pb.Side{
			SideId: spGoneRetiredSide, AddrPort: spDnB, Provisioned: true,
		},
		CloneName: spCloneGoneNm,
	}
	state.Conf.RetiredLegIdList = append(
		state.Conf.RetiredLegIdList, spGoneRetired)
	grp.RetiredLegIdList = append(grp.RetiredLegIdList, spGoneRetired)
}

// TestSpRepairSidePlans pins RW14, RW15 and RW16 for a repair (architecture.md,
// Repair): the side of the repair's new leg gets a side request of its
// group's shape — the group's ext_cnt and length to zero, its own slot and
// provisioned flag, the primary's cn_id and the standby list — and a plan
// that names the repair, which the sides-first hold waits for; a retired
// side gets one that says provisioned, with its group's exact length to zero
// so nothing is zeroed, and a plan that names the retired leg, which the
// hold never waits for, though its record carries no err_epoch; the new leg
// is mapped to its repair for the leg rows; and every cntlr request carries
// the repair in name_to_repair. The new side's request, its owner and the
// cntlrs' name_to_repair hold at STARTED and ABORTED as at CREATED, and a
// retired side's request says provisioned when its record does not.
func TestSpRepairSidePlans(t *testing.T) {
	captureLogs(t)
	state := spFixture()
	withSpRepair(state, false)
	withSpRetired(state)
	w := spTestWorker(newTestDeps(
		testConfig(common.WorkerRoleSp), newFakeStore(), newFakeClock(),
	))
	plan := w.buildPlan(context.Background(), state)
	if plan.unresolved != 0 {
		t.Fatalf("plan left %d objects unresolved", plan.unresolved)
	}

	wantConf := func(
		t *testing.T, got *pb.SyncupSideRequest, dnId, legId, sideId uint64,
		slot uint32, provisioned bool,
	) {
		t.Helper()
		want := &pb.SyncupSideRequest{
			ClusterId: testCid,
			DnId:      dnId,
			SidePointer: &pb.SidePointer{
				SpId: testSpId, LegId: legId, SideId: sideId,
			},
			Revision: testSpRev,
			SideConf: &pb.SyncupSideRequest_SideConf{
				ExtCnt:        8,
				CntlidSlot:    slot,
				PrimaryCnId:   spCnIdA,
				StandbyIdList: []uint64{spCnIdB, spCnIdC},
				SpLevel:       pb.SpLevel_SP_LEVEL_READWRITE,
				Provisioned:   provisioned,
				ZeroBytes:     spDataZeroBytes,
			},
		}
		if !proto.Equal(got, want) {
			t.Fatalf("side request\n got %v\nwant %v", got, want)
		}
	}

	newKey := sideKey{legId: spRepNewLeg, sideId: spRepNewSide}
	newPlan := plan.sides[newKey]
	if newPlan == nil {
		t.Fatalf("no plan for the new leg's side")
	}
	wantConf(t, newPlan.req, spDnIdC, spRepNewLeg, spRepNewSide, 15000,
		false)
	wantRef := model.RepairSideRef{
		RepName: spRepName, RepId: spRepId,
		LegId: spRepNewLeg, SideId: spRepNewSide,
	}
	if newPlan.repair == nil || *newPlan.repair != wantRef ||
		newPlan.retired || newPlan.cloneDeleting || newPlan.clone != nil {
		t.Fatalf("new side plan = %+v, want the repair %+v", newPlan, wantRef)
	}
	if newPlan.owner() != (sideOwner{repName: spRepName, repId: spRepId}) {
		t.Fatalf("new side owner = %+v", newPlan.owner())
	}

	retiredKey := sideKey{legId: spRetiredLeg, sideId: spRetiredSide}
	retiredPlan := plan.sides[retiredKey]
	if retiredPlan == nil {
		t.Fatalf("no plan for the retired side")
	}
	wantConf(t, retiredPlan.req, spDnIdD, spRetiredLeg, spRetiredSide,
		10000, true)
	if !retiredPlan.retired || retiredPlan.repair != nil ||
		retiredPlan.owner() != (sideOwner{retired: true}) {
		t.Fatalf("retired side plan = %+v", retiredPlan)
	}
	if retiredPlan.errEpoch != 0 {
		t.Fatalf("the retired side's plan carries err_epoch %d",
			retiredPlan.errEpoch)
	}

	awaited := awaitedSides(plan.sides)
	if !awaited[newKey] {
		t.Errorf("the sides-first hold does not wait for the new side")
	}
	if awaited[retiredKey] {
		t.Errorf("the sides-first hold waits for the retired side")
	}
	if got := plan.legRepair[spRepNewLeg]; got !=
		(repairOwner{name: spRepName, id: spRepId}) {
		t.Errorf("new leg mapped to %+v", got)
	}
	if _, ok := plan.legSlice[spRepNewLeg]; ok {
		t.Errorf("the new leg is mapped to a slice before its finish")
	}
	if len(plan.cntlrs) != 3 {
		t.Fatalf("%d cntlr plans", len(plan.cntlrs))
	}
	for cntlrId, cp := range plan.cntlrs {
		got := cp.req.GetNameToRepair()
		if len(got) != 1 || !proto.Equal(got[spRepName], state.Repairs[spRepName]) {
			t.Errorf("cntlr %d request name_to_repair = %v, want r0",
				cntlrId, got)
		}
	}

	// A provisioned new side's request says provisioned, without which its
	// disk node, finding the record present and the zeroing complete,
	// would build no exports for it (dnagent.md, SyncupSide); and the side
	// is not reported for the repair flip again.
	t.Run("a provisioned new side", func(t *testing.T) {
		state := spFixture()
		withSpRepair(state, true)
		plan := w.buildPlan(context.Background(), state)
		newPlan := plan.sides[newKey]
		if newPlan == nil {
			t.Fatalf("no plan for the new leg's side")
		}
		wantConf(t, newPlan.req, spDnIdC, spRepNewLeg, spRepNewSide, 15000,
			true)
		d := &sideDriver{report: make(chan spReport, 1)}
		d.storeInfo(&pb.SideInfo{
			ZeroedBytes: spDataZeroBytes, ZeroBytes: spDataZeroBytes,
		})
		d.reportProvisioned(
			context.Background(), &replyState{code: 0}, newPlan)
		if len(d.report) != 0 {
			t.Fatalf("the provisioned new side was reported: %+v",
				<-d.report)
		}
	})

	// A retired side's request says provisioned whatever its record holds:
	// a bad leg that died before its first zeroing ended is retired with
	// its side unprovisioned, and a request that said so would have its
	// disk node zero the copy the retired leg is kept for (RW15).
	t.Run("a retired side its record leaves unprovisioned", func(t *testing.T) {
		state := spFixture()
		withSpRetired(state).Side.Provisioned = false
		plan := w.buildPlan(context.Background(), state)
		retiredPlan := plan.sides[retiredKey]
		if retiredPlan == nil {
			t.Fatalf("no plan for the retired side")
		}
		wantConf(t, retiredPlan.req, spDnIdD, spRetiredLeg, spRetiredSide,
			10000, true)
	})

	// RW14 and RW16 hold at every stage: past CREATED, the new side keeps
	// its request, its owner and its place in the sides-first hold, so a
	// bump during md's copy still reaches it, and every cntlr request
	// carries the repair as its record stands, by which the primary finds
	// the group's array with the new leg in it.
	for _, stage := range []pb.RepStage{
		pb.RepStage_REP_STAGE_STARTED, pb.RepStage_REP_STAGE_ABORTED,
	} {
		t.Run("a repair at "+stage.String(), func(t *testing.T) {
			state := spFixture()
			rep := withSpRepair(state, true)
			rep.RepStage = stage
			rep.TryToAbort = stage == pb.RepStage_REP_STAGE_ABORTED
			plan := w.buildPlan(context.Background(), state)
			newPlan := plan.sides[newKey]
			if newPlan == nil {
				t.Fatalf("no plan for the new leg's side")
			}
			wantConf(t, newPlan.req, spDnIdC, spRepNewLeg, spRepNewSide,
				15000, true)
			if newPlan.repair == nil || *newPlan.repair != wantRef ||
				newPlan.retired || newPlan.cloneDeleting {
				t.Fatalf("new side plan = %+v, want the repair %+v",
					newPlan, wantRef)
			}
			if !awaitedSides(plan.sides)[newKey] {
				t.Errorf("the sides-first hold does not wait for the new side")
			}
			if len(plan.cntlrs) != 3 {
				t.Fatalf("%d cntlr plans", len(plan.cntlrs))
			}
			for cntlrId, cp := range plan.cntlrs {
				got := cp.req.GetNameToRepair()[spRepName]
				if !proto.Equal(got, rep) {
					t.Errorf("cntlr %d request carries r0 as %v, want %v",
						cntlrId, got, rep)
				}
			}
		})
	}
}

// TestSpDeletingCloneRepairSides pins RW16 and CLD5 for a deleting clone's
// repair (architecture.md, Repair): the repair is left out of every cntlr's
// name_to_repair, while the sides of its new leg and of its group's retired
// leg keep their side requests until the clone drain's final transaction
// gives them back (CLD5, CLD9), with the clone meta group's length to zero;
// the new side's plan marks the clone being deleted, so the side is never
// reported for the repair flip.
func TestSpDeletingCloneRepairSides(t *testing.T) {
	captureLogs(t)
	state := spFixture()
	withSpRepair(state, false)
	withDeletingClone(state)
	withDeletingCloneRepair(state)
	w := spTestWorker(newTestDeps(
		testConfig(common.WorkerRoleSp), newFakeStore(), newFakeClock(),
	))
	plan := w.buildPlan(context.Background(), state)
	for cntlrId, cp := range plan.cntlrs {
		got := cp.req.GetNameToRepair()
		if _, ok := got[spGoneRepName]; ok {
			t.Errorf("cntlr %d request carries the deleting clone's repair",
				cntlrId)
		}
		if _, ok := got[spRepName]; !ok {
			t.Errorf("cntlr %d request lost the live repair", cntlrId)
		}
	}
	newPlan := plan.sides[sideKey{legId: spGoneNewLeg, sideId: spGoneNewSide}]
	if newPlan == nil || newPlan.repair == nil || !newPlan.cloneDeleting ||
		newPlan.req.GetSideConf().GetZeroBytes() != spCloneZeroBytes ||
		newPlan.req.GetSideConf().GetProvisioned() {
		t.Fatalf("the deleting clone's new side plan = %+v", newPlan)
	}
	retiredPlan := plan.sides[sideKey{
		legId: spGoneRetired, sideId: spGoneRetiredSide,
	}]
	if retiredPlan == nil || !retiredPlan.retired ||
		!retiredPlan.cloneDeleting ||
		retiredPlan.req.GetSideConf().GetZeroBytes() != spCloneZeroBytes ||
		!retiredPlan.req.GetSideConf().GetProvisioned() {
		t.Fatalf("the deleting clone's retired side plan = %+v", retiredPlan)
	}
}

// TestSpRepairGroupMissing pins the fan-out's answer to a repair or a
// retired leg whose group the load does not hold in the parent the record
// names: its side gets no request, a record names it, and the repair is left
// out of the cntlr requests; its new leg is still mapped to the repair for
// the leg rows. A side whose disk node the load cannot resolve is left idle
// and counted for the re-resolution, as any side is (RW14).
func TestSpRepairGroupMissing(t *testing.T) {
	logs := captureLogs(t)
	state := spFixture()
	withSpRepair(state, false).SliceId = 99
	withSpRetired(state).GrpId = 999
	w := spTestWorker(newTestDeps(
		testConfig(common.WorkerRoleSp), newFakeStore(), newFakeClock(),
	))
	plan := w.buildPlan(context.Background(), state)
	for _, key := range []sideKey{
		{legId: spRepNewLeg, sideId: spRepNewSide},
		{legId: spRetiredLeg, sideId: spRetiredSide},
	} {
		if _, ok := plan.sides[key]; ok {
			t.Errorf("side %+v planned with no group", key)
		}
	}
	for cntlrId, cp := range plan.cntlrs {
		if len(cp.req.GetNameToRepair()) != 0 {
			t.Errorf("cntlr %d carries a repair with no group", cntlrId)
		}
	}
	if _, ok := plan.legRepair[spRepNewLeg]; !ok {
		t.Errorf("the new leg is not mapped to its repair")
	}
	if recs := logs.withMsg(msgSpGrpMissing); len(recs) != 2 {
		t.Fatalf("%q records = %v, want one each", msgSpGrpMissing, recs)
	}

	unresolved := spFixture()
	withSpRepair(unresolved, false).NewLegList[0].SideList[0].AddrPort =
		"spdn9:9520"
	plan = w.buildPlan(context.Background(), unresolved)
	if plan.unresolved != 1 {
		t.Fatalf("unresolved = %d, want the new side's disk node",
			plan.unresolved)
	}
}

// TestReportProvisionedRepairSide pins RW18 for the side of a repair's new
// leg: its zeroing done is reported for the repair flip, naming the repair,
// unless its group is a deleting clone's; a retired side, whose request says
// provisioned, is never reported.
func TestReportProvisionedRepairSide(t *testing.T) {
	ref := model.RepairSideRef{
		RepName: spRepName, RepId: spRepId,
		LegId: spRepNewLeg, SideId: spRepNewSide,
	}
	done := &pb.SideInfo{
		ZeroedBytes: spDataZeroBytes, ZeroBytes: spDataZeroBytes,
	}
	cases := []struct {
		name string
		plan *sidePlan
		want bool
	}{
		{"a repair's new side", &sidePlan{repair: &ref}, true},
		{"a deleting clone's new side",
			&sidePlan{repair: &ref, cloneDeleting: true}, false},
		{"a retired side", &sidePlan{retired: true}, false},
	}
	for _, tc := range cases {
		d := &sideDriver{report: make(chan spReport, 1)}
		d.storeInfo(done)
		tc.plan.req = &pb.SyncupSideRequest{
			SideConf: &pb.SyncupSideRequest_SideConf{
				ZeroBytes: spDataZeroBytes,
			},
		}
		d.reportProvisioned(
			context.Background(), &replyState{code: 0}, tc.plan)
		if got := len(d.report) == 1; got != tc.want {
			t.Errorf("%s: reported %v, want %v", tc.name, got, tc.want)
			continue
		}
		if tc.want {
			rep := <-d.report
			if rep.repairProvisioned == nil || *rep.repairProvisioned != ref ||
				rep.provisioned != nil || rep.cloneProvisioned != nil {
				t.Errorf("%s: report = %+v, want the repair side %+v",
					tc.name, rep, ref)
			}
		}
	}
}

// sideInfos answers a side stub's rounds and syncups with each side's info
// by side id, and with no rows for a side it does not list, at the request's
// revision.
func sideInfos(
	stub *stubSideAgent,
	infos map[uint64]func() *pb.SideInfo,
) {
	info := func(sideId uint64) *pb.SideInfo {
		if build, ok := infos[sideId]; ok {
			return build()
		}
		return &pb.SideInfo{}
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.checkReply = func(req *pb.CheckSideRequest) *pb.CheckSideReply {
		return &pb.CheckSideReply{
			Revision: req.GetRevision(),
			SideInfo: info(req.GetSidePointer().GetSideId()),
		}
	}
	stub.syncupReply = func(req *pb.SyncupSideRequest) *pb.SyncupSideReply {
		return &pb.SyncupSideReply{
			Revision: req.GetRevision(),
			SideInfo: info(req.GetSidePointer().GetSideId()),
		}
	}
}

// sideDevError is a SideInfo whose side device reads ERROR (HL2).
func sideDevError() *pb.SideInfo {
	return &pb.SideInfo{SideDevInfo: resErr("side-dev", "io error")}
}

// TestSpRepairSideFlip pins RW18's repair flip end to end over a side child:
// the side of a repair's new leg whose disk node reports its zeroing done is
// flipped through model.FlipRepairProvisioned, one side per call, naming the
// repair by its name and rep_id, and the flip's record names the repair.
func TestSpRepairSideFlip(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	state := spFixture()
	withSpRepair(state, false)
	h.ops.setState(state)
	h.ops.provisionedRev = testSpRev + 1
	sideInfos(h.sides[spDnC], map[uint64]func() *pb.SideInfo{
		spRepNewSide: func() *pb.SideInfo {
			return &pb.SideInfo{
				ZeroedBytes: spDataZeroBytes, ZeroBytes: spDataZeroBytes,
			}
		},
	})
	h.start()
	waitFor(t, "the repair flip", func() bool {
		return len(h.ops.repairProvisionedCalls()) > 0
	})
	want := model.RepairSideRef{
		RepName: spRepName, RepId: spRepId,
		LegId: spRepNewLeg, SideId: spRepNewSide,
	}
	for _, ref := range h.ops.repairProvisionedCalls() {
		if ref != want {
			t.Fatalf("flipped %+v, want %+v", ref, want)
		}
	}
	waitFor(t, "the flip's record", func() bool {
		for _, rec := range h.logs.withMsg(msgFlipApplied) {
			if rec["rep_name"] == spRepName &&
				rec["rep_id"] == float64(spRepId) &&
				rec["side_id"] == float64(spRepNewSide) &&
				rec["revision"] == float64(testSpRev+1) {
				return true
			}
		}
		return false
	})
	if calls := h.ops.provisionedCalls(); len(calls) != 0 {
		for _, call := range calls {
			for _, ref := range call {
				if ref.SideId == spRepNewSide {
					t.Fatalf("the new side went through the slice flip")
				}
			}
		}
	}
}

// healthWriteOf reports whether the fake writer holds a write of record on
// object objId that pick accepts.
func healthWriteOf(
	hw *fakeHealthWriter,
	record string,
	objId uint64,
	pick func(healthWrite) bool,
) bool {
	for _, write := range hw.all() {
		if write.record == record && write.objId == objId && pick(write) {
			return true
		}
	}
	return false
}

// TestSpSideMovesBetweenRecords pins RW14's restart of a side child whose
// side moved to another record (architecture.md, Repair): a side keeps its leg id and side id
// when a finish moves it to another record, so the diff compares the record
// that owns it, and a side whose owner changed is restarted with a fresh
// monitor that writes to the new record. Before the finish the new leg's
// side writes its health into the repair and the new leg's row into the
// repair too; after it the same side writes into its slice, the new leg's
// row into the slice, and the bad leg's side, retired, into its retired leg;
// the sides whose owner did not change keep their children.
func TestSpSideMovesBetweenRecords(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	state := spFixture()
	withSpRepair(state, true)
	h.ops.setState(state)
	sideInfos(h.sides[spDnC], map[uint64]func() *pb.SideInfo{
		spRepNewSide: sideDevError,
	})
	sideInfos(h.sides[spDnA], map[uint64]func() *pb.SideInfo{
		spSideB: sideDevError,
	})
	legRows := func() *pb.CntlrInfo {
		return &pb.CntlrInfo{LegIdToLeg: map[uint64]*pb.ResInfo{
			spRepNewLeg: resErr("leg-new", "probe io error"),
		}}
	}
	primary := h.cntlrs[spCnA]
	primary.mu.Lock()
	primary.checkReply = func(req *pb.CheckCntlrRequest) *pb.CheckCntlrReply {
		return &pb.CheckCntlrReply{
			Revision: req.GetRevision(), CntlrInfo: legRows(),
		}
	}
	primary.syncupReply = func(req *pb.SyncupCntlrRequest) *pb.SyncupCntlrReply {
		return &pb.SyncupCntlrReply{
			Revision: req.GetRevision(), CntlrInfo: legRows(),
		}
	}
	primary.mu.Unlock()
	w := h.start()

	inRepair := func(write healthWrite) bool {
		return write.repName == spRepName && write.repId == spRepId &&
			write.epoch != 0
	}
	waitFor(t, "the repair's side and leg written in the repair", func() bool {
		return healthWriteOf(h.hw, healthRecordSide, spRepNewSide, inRepair) &&
			healthWriteOf(h.hw, healthRecordLeg, spRepNewLeg, inRepair)
	})

	// The finish: the new leg takes the bad leg's place in leg_list, and the
	// bad leg becomes a retired leg with its side as it is.
	finished := spFixture()
	grp := finished.Slices[spSliceB].DataGrpList[0]
	bad := grp.LegList[0]
	newLeg := proto.Clone(state.Repairs[spRepName].NewLegList[0]).(*pb.Leg)
	newLeg.LegIdx = bad.GetLegIdx()
	grp.LegList[0] = newLeg
	grp.RetiredLegIdList = []uint64{spLegB}
	finished.Conf.RetiredLegIdList = []uint64{spLegB}
	finished.RetiredLegs = map[uint64]*pb.RetiredLeg{spLegB: {
		GrpId: 110, LegIdx: bad.GetLegIdx(), Side: bad.GetSideList()[0],
		RetiredEpoch: 1, SliceId: spSliceB,
	}}
	finished.SpRevision = testSpRev + 1
	h.ops.setState(finished)
	w.update(desiredState{revision: testSpRev + 1, handle: testSpName})

	inSlice := func(write healthWrite) bool {
		return write.sliceId == spSliceB && write.repName == "" &&
			write.epoch != 0
	}
	inRetired := func(write healthWrite) bool {
		return write.retiredLegId == spLegB && write.spName == testSpName
	}
	waitFor(t, "the moved side and leg written in their new records",
		func() bool {
			return healthWriteOf(h.hw, healthRecordSide, spRepNewSide,
				inSlice) &&
				healthWriteOf(h.hw, healthRecordLeg, spRepNewLeg, inSlice) &&
				healthWriteOf(h.hw, healthRecordSide, spSideB, inRetired)
		})
	for _, sideId := range []uint64{spRepNewSide, spSideB} {
		if n := sideRecords(h.logs, msgRevisionWorkerStopped, sideId); n != 1 {
			t.Errorf("side %d: %d children stopped, want the one its move "+
				"restarted", sideId, n)
		}
	}
	for _, sideId := range []uint64{spSideMeta, spSideSrc, spSideDst} {
		if n := sideRecords(h.logs, msgRevisionWorkerStopped, sideId); n != 0 {
			t.Errorf("side %d: %d children stopped, want its child kept",
				sideId, n)
		}
	}
}

// TestSpRetiredSideMonitor pins HL2 for a retired side: its child's monitor
// writes the side's health into the retired leg's record, by its leg id and
// the pool's name, and the side's request says provisioned.
func TestSpRetiredSideMonitor(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	state := spFixture()
	withSpRetired(state)
	h.ops.setState(state)
	sideInfos(h.sides[spDnD], map[uint64]func() *pb.SideInfo{
		spRetiredSide: sideDevError,
	})
	h.start()
	waitFor(t, "the retired side written in its retired leg", func() bool {
		return healthWriteOf(h.hw, healthRecordSide, spRetiredSide,
			func(write healthWrite) bool {
				return write.retiredLegId == spRetiredLeg &&
					write.spName == testSpName && write.spId == testSpId &&
					write.epoch != 0
			})
	})
	for _, req := range h.sides[spDnD].syncups() {
		if req.GetSidePointer().GetSideId() != spRetiredSide {
			continue
		}
		if !req.GetSideConf().GetProvisioned() ||
			req.GetSidePointer().GetLegId() != spRetiredLeg {
			t.Fatalf("retired side request = %v", req)
		}
	}
}
