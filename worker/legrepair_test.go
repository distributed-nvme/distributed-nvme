package worker

import (
	"errors"
	"fmt"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// AR8 — leg repair over the worker's own repairs: the create, and the drive
// step that finishes, cancels and tries to abort them
// ---------------------------------------------------------------------------

// workerRepNameOf is the name the pass gives the worker's repair of the
// group grpId at the harness's now (common.WorkerRepName).
func (h *reactHarness) workerRepNameOf(grpId uint64) string {
	return common.WorkerRepName(testSpId, grpId, h.now())
}

// withRepair gives grp the repair repName at stage, its abort flag as asked:
// its bad leg badLegId, unhealthy, and a fresh, provisioned new leg on dn C
// whose leg id and side id follow repId. It returns the repair.
func withRepair(
	h *reactHarness,
	repName string,
	grp *pb.Group,
	badLegId uint64,
	stage pb.RepStage,
	tryToAbort bool,
	repId uint64,
) *pb.Repair {
	h.t.Helper()
	rep := &pb.Repair{
		RepId:    repId,
		GrpId:    grp.GetGrpId(),
		RepStage: stage,
		NewLegList: []*pb.Leg{{
			LegId:  repId + 1,
			LegIdx: 2,
			SideList: []*pb.Side{{
				SideId: repId + 2, AddrPort: reactDnC, Provisioned: true,
			}},
		}},
		BadLegIdList: []uint64{badLegId},
		TryToAbort:   tryToAbort,
		SliceId:      reactSliceId,
	}
	if h.state.Repairs == nil {
		h.state.Repairs = map[string]*pb.Repair{}
	}
	h.state.Repairs[repName] = rep
	h.state.Conf.RepairNameList = append(
		h.state.Conf.RepairNameList, repName)
	grp.RepName = repName
	h.legOf(badLegId).ErrEpoch = h.ago(9000)
	return rep
}

// withWorkerRepair is withRepair of the data group's leg A under the
// worker's name, rep_id reactRepId, so its new leg is reactNewLeg with side
// reactNewSide on dn C. It returns the repair's name and record.
func withWorkerRepair(
	h *reactHarness,
	stage pb.RepStage,
	tryToAbort bool,
) (string, *pb.Repair) {
	h.t.Helper()
	repName := common.WorkerRepName(testSpId, reactDataGrp, 1)
	rep := withRepair(h, repName, h.dataGrp(), reactDataLegA, stage,
		tryToAbort, reactRepId)
	return repName, rep
}

// wantOneSkip asserts that the passes logged exactly one `reaction skipped`,
// of that kind and reason, naming the slice's group and leg, and returns it.
func (h *reactHarness) wantOneSkip(
	kind string, reason string, grpId uint64, legId uint64,
) map[string]any {
	h.t.Helper()
	recs := h.logs.withMsg(msgReactionSkipped)
	if len(recs) != 1 {
		h.t.Fatalf("skips = %v, want one %s %s", recs, kind, reason)
	}
	rec := recs[0]
	if rec["kind"] != kind || rec["reason"] != reason ||
		rec["slice_id"] != float64(reactSliceId) ||
		rec["grp_id"] != float64(grpId) || rec["leg_id"] != float64(legId) {
		h.t.Fatalf("skip %v, want kind %s reason %s for leg %d of group %d",
			rec, kind, reason, legId, grpId)
	}
	return rec
}

// wantNoScan asserts that no pass ran an allocator scan.
func (h *reactHarness) wantNoScan() {
	h.t.Helper()
	if got := len(h.rops.allQueries()); got != 0 {
		h.t.Fatalf("scans = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// The create
// ---------------------------------------------------------------------------

// TestLegRepairCase1 pins AR8 case 1: the leg alone is unhealthy, and only
// the LONG threshold triggers a repair. The create is a worker's repair of
// the leg with a fresh new leg: its name built from the pool id, the group id
// and the pass's now, the group's slice as its parent, a scan of the group's
// ext_cnt with the stored batch and a black list of the group's DNs, which is
// the plan the op is handed, and the stored conf.
func TestLegRepairCase1(t *testing.T) {
	t.Run("below the threshold", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(
			common.DefaultLegUnhealthy - 1,
		)
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps()
		h.wantNoSkip()
	})

	t.Run("at the threshold", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		h.dnCands(reactDnC)
		h.pass()
		calls := h.wantOps("create_repair")
		call := calls[0]
		if call.grpId != reactDataGrp || call.sliceId != reactSliceId ||
			call.cloneName != "" || call.badLegId != reactDataLegA {
			t.Fatalf("create_repair = %+v", call)
		}
		if want := h.workerRepNameOf(reactDataGrp); call.repName != want {
			t.Fatalf("rep_name = %q, want %q", call.repName, want)
		}
		if !common.HasRepNameReservedPrefix(call.repName) {
			t.Fatalf("rep_name %q lacks the reserved prefix", call.repName)
		}
		if call.legs[0].AddrPort != reactDnC {
			t.Fatalf("new leg on %s", call.legs[0].AddrPort)
		}
		if call.cc.GetAllocConf().GetDnBatchSize() != reactDnBatch {
			t.Fatalf("create handed conf %v, want the stored one", call.cc)
		}
		query := h.rops.allQueries()[0]
		if query.candExt != 2 {
			t.Fatalf("candExt = %d, want the group's ext_cnt", query.candExt)
		}
		// The cluster's STORED alloc_conf.dn_batch_size (architecture.md,
		// Common validation), which is not the constant a substitution would
		// produce.
		if query.candCnt != reactDnBatch {
			t.Fatalf("candCnt = %d, want the stored dn_batch_size",
				query.candCnt)
		}
		want := fmt.Sprint([]string{reactDnA, reactDnB})
		if fmt.Sprint(query.black) != want ||
			fmt.Sprint(call.grpDn) != want {
			t.Fatalf("black = %v, plan = %v, want both legs' DNs %s",
				query.black, call.grpDn, want)
		}
		recs := h.wantApplied(reactionRepairCreate)
		rec := recs[0]
		if rec["rep_name"] != call.repName ||
			rec["rep_id"] != float64(887) ||
			rec["new_leg_id"] != float64(888) ||
			rec["addr_port"] != reactDnC ||
			rec["leg_id"] != float64(reactDataLegA) ||
			rec["grp_id"] != float64(reactDataGrp) ||
			rec["slice_id"] != float64(reactSliceId) {
			t.Fatalf("applied = %v", rec)
		}
		h.wantNoSkip()
	})
}

// TestLegRepairCase2 pins AR8 case 2: an unhealthy leg whose SIDE has been
// unhealthy for the short threshold is repaired long before leg_unhealthy —
// and a side alone, with the leg healthy, triggers nothing.
func TestLegRepairCase2(t *testing.T) {
	t.Run("leg and side", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		leg := h.legOf(reactDataLegA)
		leg.ErrEpoch = h.ago(1)
		leg.SideList[0].ErrEpoch = h.ago(common.DefaultSideUnhealthy)
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps("create_repair")
	})

	t.Run("side only", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		leg := h.legOf(reactDataLegA)
		leg.SideList[0].ErrEpoch = h.ago(9000)
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps()
	})

	t.Run("side below the threshold", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		leg := h.legOf(reactDataLegA)
		leg.ErrEpoch = h.ago(1)
		leg.SideList[0].ErrEpoch = h.ago(common.DefaultSideUnhealthy - 1)
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps()
	})
}

// TestLegRepairSmallestLegId pins AR8's "several unhealthy legs ⇒ the
// smallest leg_id first", across both group lists of the slice.
func TestLegRepairSmallestLegId(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
	h.legOf(reactMetaLegB).ErrEpoch = h.ago(9000)
	h.dnCands(reactDnC)
	h.pass()
	calls := h.wantOps("create_repair")
	if calls[0].grpId != reactMetaGrp || calls[0].badLegId != reactMetaLegB {
		t.Fatalf("repaired leg %d of group %d, want leg %d of the meta group",
			calls[0].badLegId, calls[0].grpId, reactMetaLegB)
	}
}

// TestLegRepairRedundNoneEndsThePass pins AR8's first condition: a RedundNone
// pool only logs, and since redundancy is pool-wide the pass ends there.
func TestLegRepairRedundNoneEndsThePass(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.state.Conf.BdevConf.RedundConf = &pb.RedundConf{
		RedunKind: &pb.RedundConf_RedundNone{
			RedundNone: &pb.RedundNone{},
		},
	}
	h.legOf(reactMetaLegA).ErrEpoch = h.ago(9000)
	h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
	h.dnCands(reactDnC)
	h.pass()
	h.wantOps()
	h.wantNoScan()
	h.wantOneSkip(reactionRepairCreate, reasonRedundNone,
		reactMetaGrp, reactMetaLegA)
}

// TestLegRepairCreateConditionsInOrder pins the order of AR8's create
// conditions: an md-raid1 pool, a leg with one side, no forced-start mark on
// the group, the group's row OK in a report of the snapshot's revision or
// later, the healthy-leg test, and no repair on the group. With every hold
// armed the first one is logged; each case lifts the hold the case before it
// logged, so the next one shows, and the last case creates. Each hold runs
// no scan and no op.
func TestLegRepairCreateConditionsInOrder(t *testing.T) {
	type hold struct {
		arm    func(h *reactHarness)
		reason string
	}
	holds := []hold{
		{func(h *reactHarness) {
			h.state.Conf.BdevConf.RedundConf = &pb.RedundConf{
				RedunKind: &pb.RedundConf_RedundNone{
					RedundNone: &pb.RedundNone{},
				},
			}
		}, reasonRedundNone},
		{func(h *reactHarness) {
			leg := h.legOf(reactDataLegA)
			leg.SideList = append(leg.SideList, &pb.Side{
				SideId: 999, AddrPort: reactDnD,
			})
		}, reasonTwoSides},
		{func(h *reactHarness) {
			// An operator's forced-start mark from leg B, which the
			// primary's report has not caught up with.
			withMark(h.dataGrp(), reactDataLegB)
		}, reasonGrpForcedStart},
		{func(h *reactHarness) {
			h.setGrpRow(reactDataGrp, pb.ResStatus_RES_STATUS_ERROR)
		}, reasonGrpRowError},
		{func(h *reactHarness) {
			// Leg B carries a fresh health epoch: it is no healthy leg, and
			// is not due itself.
			h.legOf(reactDataLegB).ErrEpoch = h.ago(1)
		}, reasonNoHealthyLeg},
		{func(h *reactHarness) {
			// An operator's repair of the group, whose bad leg is A.
			withReactRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
		}, reasonGrpHasRepair},
	}
	for first := range len(holds) + 1 {
		name := "create"
		if first < len(holds) {
			name = holds[first].reason
		}
		t.Run(name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
			h.dnCands(reactDnD)
			for _, armed := range holds[first:] {
				armed.arm(h)
			}
			h.pass()
			if first == len(holds) {
				h.wantOps("create_repair")
				h.wantNoSkip()
				return
			}
			h.wantOps()
			h.wantNoScan()
			h.wantOneSkip(reactionRepairCreate, holds[first].reason,
				reactDataGrp, reactDataLegA)
		})
	}
}

// TestLegRepairCreateHoldsOnlyItsGroup pins AR8's holds as part of the
// CANDIDATE test rather than reasons to end the pass: the smallest-leg_id
// ordering runs over the legs that can be repaired. Each hold here sits on
// the meta group, whose leg has the smaller id, and the data group's leg is
// still repaired in the same pass. Every one of them can last: a migration
// for hours, a stopped array until its legs come back, a group with no
// healthy leg until one answers again, an operator's repair or forced-start
// mark until the operator acts.
func TestLegRepairCreateHoldsOnlyItsGroup(t *testing.T) {
	for _, tc := range []struct {
		reason string
		arm    func(h *reactHarness)
	}{
		{reasonTwoSides, func(h *reactHarness) {
			leg := h.legOf(reactMetaLegA)
			leg.SideList = append(leg.SideList, &pb.Side{
				SideId: 999, AddrPort: reactDnD,
			})
		}},
		{reasonGrpForcedStart, func(h *reactHarness) {
			withMark(h.metaGrp(), reactMetaLegB)
		}},
		{reasonGrpRowError, func(h *reactHarness) {
			h.setGrpRow(reactMetaGrp, pb.ResStatus_RES_STATUS_ERROR)
		}},
		{reasonGrpRowAbsent, func(h *reactHarness) {
			h.inSyncAll = false
			h.setGrpInfo(reactMetaGrp, true, reactMetaLegA, reactMetaLegB)
			h.setGrpInfo(reactDataGrp, true, reactDataLegA, reactDataLegB)
			h.setGrpRow(reactDataGrp, pb.ResStatus_RES_STATUS_OK)
		}},
		{reasonNoHealthyLeg, func(h *reactHarness) {
			h.legOf(reactMetaLegB).ErrEpoch = h.ago(1)
		}},
		{reasonGrpHasRepair, func(h *reactHarness) {
			withReactRepairOf(h, h.metaGrp(), reactMetaLegA,
				pb.RepStage_REP_STAGE_CREATED, false)
		}},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.legOf(reactMetaLegA).ErrEpoch = h.ago(9000)
			h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
			h.dnCands(reactDnD)
			tc.arm(h)
			h.pass()
			calls := h.wantOps("create_repair")
			if calls[0].grpId != reactDataGrp {
				t.Fatalf("create_repair = %+v, want the data group",
					calls[0])
			}
			// The held group costs no scan: the one scan is the data
			// group's.
			if got := len(h.rops.allQueries()); got != 1 {
				t.Fatalf("scans = %d, want 1", got)
			}
			h.wantApplied(reactionRepairCreate)
			h.wantOneSkip(reactionRepairCreate, tc.reason,
				reactMetaGrp, reactMetaLegA)
		})
	}

	t.Run("still one action per pass", func(t *testing.T) {
		// Nothing holds either leg: the walk stops at the first candidate,
		// so AR2's invariant survives the scan being able to go on.
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactMetaLegA).ErrEpoch = h.ago(9000)
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnC)
		h.pass()
		calls := h.wantOps("create_repair")
		if calls[0].grpId != reactMetaGrp {
			t.Fatalf("repaired group %d, want the smallest leg_id's group",
				calls[0].grpId)
		}
		h.wantNoSkip()
	})
}

// TestLegRepairCreateNeedsTheGroupRunning pins AR8's gate: a repair is
// created only while the group's row in the current primary's latest report
// reads OK, the one status that says the group's array runs, whole or
// degraded (cnagent.md CN28). Any other status, or a report without the
// row, holds the create with a reason naming it, runs no scan and no op, and
// the scan goes on to the next candidate leg (AR2). The row is the current
// primary's: from a failover's hand-over until the promotion's reply the
// pass reads the new primary's standby report, which holds no group row, and
// never the old primary's last report.
func TestLegRepairCreateNeedsTheGroupRunning(t *testing.T) {
	build := func(t *testing.T) *reactHarness {
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnD)
		return h
	}

	t.Run("row OK releases the create", func(t *testing.T) {
		h := build(t)
		h.setGrpRow(reactDataGrp, pb.ResStatus_RES_STATUS_OK)
		h.pass()
		h.wantOps("create_repair")
		h.wantNoSkip()
	})

	for _, tc := range []struct {
		status pb.ResStatus
		reason string
	}{
		{pb.ResStatus_RES_STATUS_ERROR, reasonGrpRowError},
		{pb.ResStatus_RES_STATUS_MISSING, reasonGrpRowMissing},
		{pb.ResStatus_RES_STATUS_UNKNOWN, reasonGrpRowUnknown},
		{pb.ResStatus_RES_STATUS_PROVISIONING, reasonGrpRowProvisioning},
		{pb.ResStatus_RES_STATUS_PENDING, reasonGrpRowPending},
	} {
		t.Run("row "+tc.status.String()+" holds the create",
			func(t *testing.T) {
				h := build(t)
				h.setGrpRow(reactDataGrp, tc.status)
				h.pass()
				h.wantOps()
				h.wantNoScan()
				h.wantApplied()
				h.wantOneSkip(reactionRepairCreate, tc.reason,
					reactDataGrp, reactDataLegA)
			})
	}

	t.Run("a report without the row holds the create", func(t *testing.T) {
		h := build(t)
		h.inSyncAll = false
		h.setGrpInfo(reactDataGrp, true, reactDataLegA, reactDataLegB)
		h.pass()
		h.wantOps()
		h.wantNoScan()
		h.wantOneSkip(reactionRepairCreate, reasonGrpRowAbsent,
			reactDataGrp, reactDataLegA)
	})

	// A row the primary's dead stream marked UNKNOWN holds, though the
	// group's reading stays as the last report had it (markCntlrUnknown).
	t.Run("a row marked unknown", func(t *testing.T) {
		h := build(t)
		h.setGrpRow(reactDataGrp, pb.ResStatus_RES_STATUS_OK)
		h.setGrpInfo(reactDataGrp, true, reactDataLegA, reactDataLegB)
		markCntlrUnknown(h.w.cntlrs[reactCntlrA].driver.lastInfo)
		h.pass()
		h.wantOps()
		h.wantOneSkip(reactionRepairCreate, reasonGrpRowUnknown,
			reactDataGrp, reactDataLegA)
	})

	// The gate reads the target's own group.
	t.Run("the target's own group", func(t *testing.T) {
		h := build(t)
		h.setGrpRow(reactMetaGrp, pb.ResStatus_RES_STATUS_OK)
		h.setGrpRow(reactDataGrp, pb.ResStatus_RES_STATUS_ERROR)
		h.pass()
		h.wantOps()
		h.wantOneSkip(reactionRepairCreate, reasonGrpRowError,
			reactDataGrp, reactDataLegA)

		h = build(t)
		h.setGrpRow(reactMetaGrp, pb.ResStatus_RES_STATUS_ERROR)
		h.setGrpRow(reactDataGrp, pb.ResStatus_RES_STATUS_OK)
		h.pass()
		h.wantOps("create_repair")
	})

	t.Run("a clone meta group", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		grp := withReactClone(t, h.state)
		cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
		// The slice's groups run; the clone's group does not.
		h.setGrpRow(reactCloneGrp, pb.ResStatus_RES_STATUS_ERROR)
		h.dnCands(reactDnD)
		h.pass()
		h.wantOps()
		recs := h.logs.withMsg(msgReactionSkipped)
		if len(recs) != 1 || recs[0]["reason"] != reasonGrpRowError ||
			recs[0]["kind"] != reactionRepairCreate {
			t.Fatalf("skips = %v, want one grp_row_error", recs)
		}
		wantCloneRecord(t, recs[0], reactCloneLegA)
	})

	// The failover has committed and the release has handed cntlr B the
	// primary plan, but the promotion has not answered: B's latest report is
	// its standby's last, which holds no group row (cnagent.md CN12). The
	// old primary's last report, group row OK and leg B held in sync, is
	// still held by its child; a pass that read it would create.
	t.Run("a report absent after a failover", func(t *testing.T) {
		state := reactFixture(t)
		state.Cntlrs[reactCntlrA].Primary = false
		state.Cntlrs[reactCntlrB].Primary = true
		h := newReactHarness(t, state)
		h.inSyncAll = false
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnD)
		running := func() *pb.CntlrInfo {
			return &pb.CntlrInfo{
				GrpIdToMdRaid: map[uint64]*pb.ResInfo{
					reactMetaGrp: resOk("md"),
					reactDataGrp: resOk("md"),
				},
				GrpIdToGrpInfo: map[uint64]*pb.CntlrInfo_GrpInfo{
					reactDataGrp: {
						Whole: true,
						InSyncLegIdList: []uint64{
							reactDataLegA, reactDataLegB,
						},
					},
				},
			}
		}
		h.w.cntlrs[reactCntlrA] = &cntlrChild{
			plan:   &cntlrPlan{cntlrId: reactCntlrA},
			driver: &cntlrDriver{lastInfo: running()},
		}
		promoted := &cntlrChild{
			plan:   &cntlrPlan{cntlrId: reactCntlrB, primary: true},
			driver: &cntlrDriver{lastInfo: &pb.CntlrInfo{}},
		}
		h.w.cntlrs[reactCntlrB] = promoted
		h.pass()
		h.wantOps()
		h.wantOneSkip(reactionRepairCreate, reasonGrpRowAbsent,
			reactDataGrp, reactDataLegA)

		// The promotion answers: the new primary's array runs and holds
		// leg B in sync.
		promoted.driver.lastInfo = running()
		h.pass()
		calls := h.wantOps("create_repair")
		if calls[0].badLegId != reactDataLegA {
			t.Fatalf("create = %+v", calls[0])
		}
	})
}

// TestLegRepairCreateWaitsForTheReportOfTheSnapshot pins the revision test of
// AR8's gate: an OK group row in a report older than the snapshot's
// SpRevision holds the create (grp_row_stale) until the primary reports an
// observation of that revision. The case: the worker's repair of the data
// group's leg A has just finished — the new leg sits in A's place and the
// finish bumped SpRevision — and the other leg, B, is now due. The primary's
// latest report is still of an observation from before the finish, which
// describes the array before the new leg took A's place; its reading holds
// the new leg in sync, so the healthy-leg test would pass, and the gate
// alone holds the create.
func TestLegRepairCreateWaitsForTheReportOfTheSnapshot(t *testing.T) {
	const finishRev = uint64(20)
	state := reactFixture(t)
	state.SpRevision = finishRev
	h := newReactHarness(t, state)
	h.setGrpRow(reactDataGrp, pb.ResStatus_RES_STATUS_OK)
	h.setGrpInfo(reactDataGrp, true, reactDataLegA, reactDataLegB, reactNewLeg)
	driver := h.w.cntlrs[reactCntlrA].driver
	driver.infoRev = finishRev - 1
	grp := h.dataGrp()
	grp.LegList[0] = &pb.Leg{
		LegId: reactNewLeg, LegIdx: 0,
		SideList: []*pb.Side{{
			SideId: reactNewSide, AddrPort: reactDnC, Provisioned: true,
		}},
	}
	h.legOf(reactDataLegB).ErrEpoch = h.ago(9000)
	h.dnCands(reactDnD)

	h.pass()
	h.wantOps()
	h.wantNoScan()
	h.wantOneSkip(reactionRepairCreate, reasonGrpRowStale,
		reactDataGrp, reactDataLegB)

	// The primary reports the converge of the finish's syncup: the gate
	// reads its row, and the new leg, in sync, is B's healthy leg.
	driver.infoRev = finishRev
	h.pass()
	calls := h.wantOps("create_repair")
	if calls[0].badLegId != reactDataLegB {
		t.Fatalf("create = %+v, want leg B", calls[0])
	}
	if want := fmt.Sprint([]string{reactDnC, reactDnB}); fmt.Sprint(
		calls[0].grpDn) != want {
		t.Fatalf("plan = %v, want the group's legs now %s",
			calls[0].grpDn, want)
	}
}

// TestLegRepairNeedsAHealthyLeg pins AR8's healthy-leg test on a slice's
// group: a create runs only while another leg of the group's leg_list has no
// health epoch and is held in sync. md learns that a member is dead only
// from IO to it, so an idle array whose legs all died still runs and its row
// reads OK, which the gate lets through; the legs' health epochs come from
// the primary's probe of each leg and see what the row does not. With no
// healthy leg the pass logs no_healthy_leg, creates nothing for the group,
// and goes on to the next candidate; with one, the repair proceeds.
func TestLegRepairNeedsAHealthyLeg(t *testing.T) {
	// build makes both legs of the data group unhealthy, the first due for
	// repair, while the group's array still runs.
	build := func(t *testing.T) *reactHarness {
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.legOf(reactDataLegB).ErrEpoch = h.ago(1)
		h.dnCands(reactDnD)
		return h
	}

	t.Run("no healthy leg", func(t *testing.T) {
		h := build(t)
		h.pass()
		h.wantOps()
		h.wantNoScan()
		h.wantOneSkip(reactionRepairCreate, reasonNoHealthyLeg,
			reactDataGrp, reactDataLegA)
	})

	// Both legs due: each is refused on its own, and neither is repaired.
	t.Run("both legs due", func(t *testing.T) {
		h := build(t)
		h.legOf(reactDataLegB).ErrEpoch = h.ago(9000)
		h.pass()
		h.wantOps()
		recs := h.logs.withMsg(msgReactionSkipped)
		if len(recs) != 2 {
			t.Fatalf("skips = %v, want one per leg", recs)
		}
		for idx, legId := range []uint64{reactDataLegA, reactDataLegB} {
			if recs[idx]["reason"] != reasonNoHealthyLeg ||
				recs[idx]["leg_id"] != float64(legId) {
				t.Fatalf("skip %v, want no_healthy_leg for leg %d",
					recs[idx], legId)
			}
		}
	})

	// Once the other leg reads healthy again, the repair proceeds.
	t.Run("a leg healthy again", func(t *testing.T) {
		h := build(t)
		h.pass()
		h.wantOps()
		h.legOf(reactDataLegB).ErrEpoch = 0
		h.pass()
		calls := h.wantOps("create_repair")
		if calls[0].grpId != reactDataGrp {
			t.Fatalf("create_repair = %+v, want the data group", calls[0])
		}
		h.wantApplied(reactionRepairCreate)
	})
}

// TestLegRepairCountsOnlyLegsInSync pins AR8's healthy-leg test: another leg
// of the group's leg_list counts only when it has no err_epoch AND the
// current primary's latest report holds it in sync. A healthy leg md still
// copies, a report that holds only the due leg in sync, and a report with no
// reading of the group all hold the group with no_healthy_leg, creating
// nothing; a leg in sync lets the repair go on.
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
				h.wantOps("create_repair")
				return
			}
			h.wantOps()
			h.wantOneSkip(reactionRepairCreate, reasonNoHealthyLeg,
				reactDataGrp, reactDataLegA)
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
		h.wantSkipped(reactionRepairCreate, reasonNoHealthyLeg)
	})
}

// TestLegRepairAGroupWithARepairGetsNoOther pins AR8's last create condition:
// a group that has a repair gets no other, an operator's repair keeping the
// worker from creating one as the worker's own does. Its due leg is logged
// with the group's own reason on every pass, naming the repair, and the scan
// goes on to the next candidate, in another group, which is repaired in the
// same pass. The worker drives no operator's repair (AR9), so an operator's
// open repair keeps the worker from repairing the group until the operator
// finishes or cancels it.
func TestLegRepairAGroupWithARepairGetsNoOther(t *testing.T) {
	for _, tc := range []struct {
		name    string
		repName string
	}{
		{"an operator's repair", reactRepName},
		{"the worker's own repair",
			common.WorkerRepName(testSpId, reactMetaGrp, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			// The repair waits in CREATED with its bad leg still bad and no
			// verdict, so neither the stage move nor the drive step moves it.
			withRepair(h, tc.repName, h.metaGrp(), reactMetaLegA,
				pb.RepStage_REP_STAGE_CREATED, false, reactRepId)
			// The data group, which has no repair, has a due leg after it.
			h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
			h.dnCands(reactDnD)
			var want []string
			for pass := 1; pass <= 2; pass++ {
				h.pass()
				want = append(want, "create_repair")
				calls := h.wantOps(want...)
				if calls[pass-1].grpId != reactDataGrp {
					t.Fatalf("pass %d: created for group %d, want %d", pass,
						calls[pass-1].grpId, reactDataGrp)
				}
				recs := h.logs.withMsg(msgReactionSkipped)
				if len(recs) != pass {
					t.Fatalf("pass %d: skips = %v, want one a pass", pass,
						recs)
				}
				rec := recs[pass-1]
				if rec["kind"] != reactionRepairCreate ||
					rec["reason"] != reasonGrpHasRepair ||
					rec["rep_name"] != tc.repName ||
					rec["leg_id"] != float64(reactMetaLegA) {
					t.Fatalf("pass %d: skip %v, want %s for leg %d naming %s",
						pass, rec, reasonGrpHasRepair, reactMetaLegA,
						tc.repName)
				}
			}
		})
	}
}

// TestLegRepairCreateExcludesGroupLocations pins the tier-1 exclusion of the
// create's scan (architecture.md, Per-operation allocation): the scan
// carries the DNs of every leg of the group AND their locations, read from
// the pass's own snapshot of the node records (MD3) rather than from a
// second etcd round trip, and the op is handed the very black list the scan
// ran against.
func TestLegRepairCreateExcludesGroupLocations(t *testing.T) {
	build := func(t *testing.T, locA string, locB string) *reactHarness {
		t.Helper()
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.state.DnByAddr[reactDnA] = &pb.DnConf{Location: locA}
		h.state.DnByAddr[reactDnB] = &pb.DnConf{Location: locB}
		h.dnCands(reactDnD)
		return h
	}
	wantScan := func(t *testing.T, h *reactHarness, wantLocs []string) {
		t.Helper()
		queries := h.rops.allQueries()
		if len(queries) != 1 || queries[0].kind != "dn" {
			t.Fatalf("queries = %+v", queries)
		}
		wantBlack := []string{reactDnA, reactDnB}
		if fmt.Sprint(queries[0].black) != fmt.Sprint(wantBlack) {
			t.Errorf("black = %v, want %v", queries[0].black, wantBlack)
		}
		if fmt.Sprint(queries[0].excludeLocs) != fmt.Sprint(wantLocs) {
			t.Errorf("exclude_locs = %v, want %v",
				queries[0].excludeLocs, wantLocs)
		}
		// The tier-2 trigger is the ONE DN this step places, never the
		// oversampled candCnt (architecture.md, Per-operation allocation).
		if queries[0].requiredCnt != 1 {
			t.Errorf("required_cnt = %d, want 1", queries[0].requiredCnt)
		}
		calls := h.rops.allCalls()
		if len(calls) != 1 ||
			fmt.Sprint(calls[0].grpDn) != fmt.Sprint(queries[0].black) {
			t.Errorf("create plan = %v, want the scan's black list %v",
				calls, queries[0].black)
		}
	}

	t.Run("two named locations", func(t *testing.T) {
		h := build(t, "rack-left", "rack-right")
		h.pass()
		calls := h.wantOps("create_repair")
		if calls[0].legs[0].AddrPort != reactDnD {
			t.Fatalf("new leg on %s", calls[0].legs[0].AddrPort)
		}
		wantScan(t, h, []string{"rack-left", "rack-right"})
	})

	t.Run("a missing dn_conf contributes no location", func(t *testing.T) {
		h := build(t, "rack-left", "rack-right")
		delete(h.state.DnByAddr, reactDnB)
		h.pass()
		h.wantOps("create_repair")
		wantScan(t, h, []string{"rack-left"})
	})

	t.Run("the default location is the addr_port", func(t *testing.T) {
		h := build(t, reactDnA, reactDnB)
		h.pass()
		h.wantOps("create_repair")
		wantScan(t, h, []string{reactDnA, reactDnB})
	})
}

// TestLegRepairCreateNoCandidate pins the create's empty scan, which ends the
// pass.
func TestLegRepairCreateNoCandidate(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
	h.pass()
	h.wantOps()
	h.wantSkipped(reactionRepairCreate, reasonNoCandidate)
}

// TestLegRepairCreateAtTheRetiredBound pins the worker's repair at the
// retired-leg bound: a group that holds common.MaxRetiredLegPerGrp retired
// legs still gets its repair, with a fresh new leg — no hold, no wait for an
// operator — since the finish gives back the oldest retired leg; the
// retired sides' DNs are no part of the plan, a retired leg being no leg.
func TestLegRepairCreateAtTheRetiredBound(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	for idx := range common.MaxRetiredLegPerGrp {
		withReactRetired(h, h.dataGrp(), retireLegA+uint64(2*idx),
			retireSideA+uint64(2*idx), h.ago(60))
	}
	h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
	h.dnCands(reactDnD)
	h.pass()
	calls := h.wantOps("create_repair")
	if want := fmt.Sprint([]string{reactDnA, reactDnB}); fmt.Sprint(
		calls[0].grpDn) != want {
		t.Fatalf("plan = %v, want the legs' DNs %s", calls[0].grpDn, want)
	}
	h.wantApplied(reactionRepairCreate)
	h.wantNoSkip()
}

// TestLegRepairCreateRefusalEndsThePass pins how a pass meets a refusal of
// model.CreateRepair from inside its transaction (MD6, AR2): `candidate
// changed` when the group gained a DN since the snapshot or the pick went
// stale, or the group given a repair by another owner's create, or that
// owner's name taken when it built the same name in the same second. Like every
// ErrPrecondition of a create it is a `reaction skipped` carrying the op's
// reason and name that ends the pass, so no other group is tried; the next
// pass plans again from its own snapshot, with a fresh scan whose black
// list is the plan it hands over.
func TestLegRepairCreateRefusalEndsThePass(t *testing.T) {
	for _, reason := range []string{
		model.ReasonCandidateChanged, model.ReasonGrpHasRepair,
		model.ReasonRepairExists,
	} {
		t.Run(reason, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.legOf(reactMetaLegA).ErrEpoch = h.ago(9000)
			h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
			h.dnCands(reactDnC)
			h.rops.opErr = &model.ErrPrecondition{
				Op: "CreateRepair", Reason: reason,
			}
			h.pass()
			calls := h.wantOps("create_repair")
			if calls[0].grpId != reactMetaGrp {
				t.Fatalf("create_repair = %+v, want the meta group",
					calls[0])
			}
			h.wantApplied()
			h.wantSkippedBy(reactionRepairCreate, reason, "CreateRepair")

			h.rops.opErr = nil
			h.pass()
			calls = h.wantOps("create_repair", "create_repair")
			if calls[1].grpId != reactMetaGrp {
				t.Fatalf("retry = %+v, want the meta group again", calls[1])
			}
			queries := h.rops.allQueries()
			if len(queries) != 2 {
				t.Fatalf("scans = %d, want one per pass", len(queries))
			}
			if fmt.Sprint(calls[1].grpDn) != fmt.Sprint(queries[1].black) {
				t.Fatalf("retry plan = %v, want its own scan's %v",
					calls[1].grpDn, queries[1].black)
			}
			h.wantApplied(reactionRepairCreate)
		})
	}
}

// TestLegRepairRetiredAndNewLegsAreNoCandidates pins AR8's trigger scan: only
// leg_list legs are candidates. A repair's new leg with a health epoch, and
// a retired leg whose side has one, are no legs of their group and get no
// repair of their own.
func TestLegRepairRetiredAndNewLegsAreNoCandidates(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	withReactRepairOf(h, h.metaGrp(), reactMetaLegA,
		pb.RepStage_REP_STAGE_CREATED, true)
	h.legOf(reactNewLeg).ErrEpoch = h.ago(9000)
	retired := withReactRetired(h, h.dataGrp(), retireLegA, retireSideA,
		h.ago(60))
	retired.Side.ErrEpoch = h.ago(9000)
	h.dnCands(reactDnD)
	h.pass()
	// The meta group's bad leg is held by its repair; nothing else is due.
	h.wantOps()
	h.wantOneSkip(reactionRepairCreate, reasonGrpHasRepair,
		reactMetaGrp, reactMetaLegA)
}

// ---------------------------------------------------------------------------
// The drive step
// ---------------------------------------------------------------------------

// TestLegRepairFinishesAStartedRepair pins the drive step's finish: a
// STARTED worker's repair is finished at the pass's now under the stored
// conf, whatever its abort flag says, since the primary reported its new
// leg holding a place; the record names the repair and the leg it retired.
// The finish is the pass's one action: a due leg of another group waits for
// the next pass.
func TestLegRepairFinishesAStartedRepair(t *testing.T) {
	for _, flag := range []bool{false, true} {
		t.Run(fmt.Sprintf("abort flag %t", flag), func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			repName, _ := withWorkerRepair(h,
				pb.RepStage_REP_STAGE_STARTED, flag)
			h.rops.finishRes = model.FinishRepairResult{
				RepId: reactRepId, RetiredLegId: reactDataLegA,
			}
			h.legOf(reactMetaLegA).ErrEpoch = h.ago(9000)
			h.dnCands(reactDnD)
			h.pass()
			calls := h.wantOps("finish_repair")
			if calls[0].repName != repName || calls[0].now != h.now() ||
				calls[0].cc.GetAllocConf().GetDnBatchSize() != reactDnBatch {
				t.Fatalf("finish = %+v", calls[0])
			}
			rec := h.wantApplied(reactionRepairFinish)[0]
			if rec["rep_name"] != repName ||
				rec["rep_id"] != float64(reactRepId) ||
				rec["grp_id"] != float64(reactDataGrp) ||
				rec["retired_leg_id"] != float64(reactDataLegA) {
				t.Fatalf("applied = %v", rec)
			}
			if _, ok := rec["given_back_leg_id"]; ok {
				t.Fatalf("a finish that gave nothing back names a leg: %v",
					rec)
			}
			h.wantNoScan()
		})
	}
}

// TestLegRepairDrivesOneStepAPass pins AR2 over the drive step: with two
// worker's repairs due at once and both ops committing, the first step in
// repair_name_list order ends the pass, so the finish of the meta group's
// STARTED repair runs alone and the cancel of the data group's ABORTED one
// waits for the next pass.
func TestLegRepairDrivesOneStepAPass(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	metaRep := common.WorkerRepName(testSpId, reactMetaGrp, 1)
	dataRep := common.WorkerRepName(testSpId, reactDataGrp, 1)
	metaRec := withRepair(h, metaRep, h.metaGrp(), reactMetaLegA,
		pb.RepStage_REP_STAGE_STARTED, false, 900)
	withRepair(h, dataRep, h.dataGrp(), reactDataLegA,
		pb.RepStage_REP_STAGE_ABORTED, true, 910)
	h.pass()
	calls := h.wantOps("finish_repair")
	if calls[0].repName != metaRep {
		t.Fatalf("finish = %+v, want %s", calls[0], metaRep)
	}
	h.wantApplied(reactionRepairFinish)

	// The finish commits: the meta group's repair is gone and its new leg
	// sits in the bad leg's place.
	delete(h.state.Repairs, metaRep)
	h.state.Conf.RepairNameList = []string{dataRep}
	meta := h.metaGrp()
	meta.RepName = ""
	for idx, leg := range meta.GetLegList() {
		if leg.GetLegId() == reactMetaLegA {
			newLeg := metaRec.GetNewLegList()[0]
			newLeg.LegIdx = leg.GetLegIdx()
			meta.LegList[idx] = newLeg
		}
	}
	h.pass()
	calls = h.wantOps("finish_repair", "cancel_repair")
	if calls[1].repName != dataRep {
		t.Fatalf("cancel = %+v, want %s", calls[1], dataRep)
	}
	h.wantApplied(reactionRepairFinish, reactionRepairCancel)
}

// TestLegRepairFinishLogsTheGiveBack pins the record of a finish at the
// retired-leg bound: model.FinishRepair gave back the group's oldest retired
// leg to make room for the bad leg, and the record names it.
func TestLegRepairFinishLogsTheGiveBack(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	withWorkerRepair(h, pb.RepStage_REP_STAGE_STARTED, false)
	h.rops.finishRes = model.FinishRepairResult{
		RepId: reactRepId, RetiredLegId: reactDataLegA,
		GivenBackLegId: retireLegA,
	}
	h.pass()
	h.wantOps("finish_repair")
	rec := h.wantApplied(reactionRepairFinish)[0]
	if rec["given_back_leg_id"] != float64(retireLegA) ||
		rec["retired_leg_id"] != float64(reactDataLegA) {
		t.Fatalf("applied = %v, want the given-back leg %d", rec, retireLegA)
	}
}

// TestLegRepairCancelsAnAbortedRepair pins the drive step's cancel: an
// ABORTED worker's repair is cancelled under the stored conf, which gives
// its fresh new leg's side back; the cancel is the pass's one action.
func TestLegRepairCancelsAnAbortedRepair(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	repName, _ := withWorkerRepair(h, pb.RepStage_REP_STAGE_ABORTED, true)
	h.legOf(reactMetaLegA).ErrEpoch = h.ago(9000)
	h.dnCands(reactDnD)
	h.pass()
	calls := h.wantOps("cancel_repair")
	if calls[0].repName != repName ||
		calls[0].cc.GetAllocConf().GetDnBatchSize() != reactDnBatch {
		t.Fatalf("cancel = %+v", calls[0])
	}
	rec := h.wantApplied(reactionRepairCancel)[0]
	if rec["rep_name"] != repName || rec["rep_id"] != float64(reactRepId) {
		t.Fatalf("applied = %v", rec)
	}
}

// TestLegRepairAbortsWhenTheBadLegReturns pins the first trigger of the
// try-to-abort: a CREATED worker's repair whose bad leg reads healthy
// again, its err_epoch clear, is tried to abort at once, with no clock and
// no verdict of this coordinator; the record names the trigger and the leg.
// The try-to-abort is the pass's one action: the meta group's due leg waits
// for the next pass.
func TestLegRepairAbortsWhenTheBadLegReturns(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.judgeStored = false
	repName, _ := withWorkerRepair(h, pb.RepStage_REP_STAGE_CREATED, false)
	h.dnCands(reactDnD)
	h.pass()
	h.wantOps()
	h.wantApplied()

	h.legOf(reactDataLegA).ErrEpoch = 0
	h.legOf(reactMetaLegA).ErrEpoch = h.ago(9000)
	h.w.verdicts.noteLeg(reactMetaLegA, true)
	h.pass()
	h.wantNoScan()
	calls := h.wantOps("abort_repair")
	if calls[0].repName != repName {
		t.Fatalf("abort = %+v", calls[0])
	}
	rec := h.wantApplied(reactionRepairAbort)[0]
	if rec["trigger"] != triggerBadLegHealthy ||
		rec["leg_id"] != float64(reactDataLegA) ||
		rec["rep_name"] != repName {
		t.Fatalf("applied = %v", rec)
	}
}

// TestLegRepairAbortClocks pins the clocks of the try-to-abort (AR8, AR10):
// a new leg asks for an abort only once its health epoch is leg_unhealthy
// old, or its side's side_unhealthy old, each with this coordinator's own
// unhealthy verdict on it; another leg of the group asks only on AR8's own
// triggers. A fresh new leg reading ERROR from its provisioned flip until
// the primary's connect completes, which stamps its epoch at once, aborts
// nothing; an unprovisioned new leg, neither connected nor probed, aborts on
// its side's clock alone when its DN dies while it zeroes. Two triggers that
// hold at once name the first in AR8's order: the bad leg healthy again, then
// the new leg, then another leg.
func TestLegRepairAbortClocks(t *testing.T) {
	type tc struct {
		name    string
		set     func(h *reactHarness)
		trigger string
		legId   uint64
	}
	newSide := sideKey{legId: reactNewLeg, sideId: reactNewSide}
	sideB := sideKey{legId: reactDataLegB, sideId: reactSideDataB}
	for _, c := range []tc{
		{"a fresh new leg reading ERROR", func(h *reactHarness) {
			h.legOf(reactNewLeg).ErrEpoch = h.ago(1)
			h.w.verdicts.noteLeg(reactNewLeg, true)
		}, "", 0},
		{"a new leg below leg_unhealthy", func(h *reactHarness) {
			h.legOf(reactNewLeg).ErrEpoch = h.ago(
				common.DefaultLegUnhealthy - 1)
			h.w.verdicts.noteLeg(reactNewLeg, true)
		}, "", 0},
		{"a new leg at leg_unhealthy with no verdict", func(h *reactHarness) {
			h.legOf(reactNewLeg).ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		}, "", 0},
		{"a new leg at leg_unhealthy", func(h *reactHarness) {
			h.legOf(reactNewLeg).ErrEpoch = h.ago(common.DefaultLegUnhealthy)
			h.w.verdicts.noteLeg(reactNewLeg, true)
		}, triggerNewLegUnhealthy, reactNewLeg},
		{"a zeroing new leg's side at side_unhealthy", func(h *reactHarness) {
			side := h.legOf(reactNewLeg).SideList[0]
			side.Provisioned = false
			side.ErrEpoch = h.ago(common.DefaultSideUnhealthy)
			h.w.verdicts.noteSide(newSide, true)
		}, triggerNewLegUnhealthy, reactNewLeg},
		{"a zeroing new leg's side below side_unhealthy", func(h *reactHarness) {
			side := h.legOf(reactNewLeg).SideList[0]
			side.Provisioned = false
			side.ErrEpoch = h.ago(common.DefaultSideUnhealthy - 1)
			h.w.verdicts.noteSide(newSide, true)
		}, "", 0},
		{"a new leg's side with no verdict", func(h *reactHarness) {
			h.legOf(reactNewLeg).SideList[0].ErrEpoch = h.ago(
				common.DefaultSideUnhealthy)
		}, "", 0},
		{"the other leg reading ERROR", func(h *reactHarness) {
			h.legOf(reactDataLegB).ErrEpoch = h.ago(1)
			h.w.verdicts.noteLeg(reactDataLegB, true)
		}, "", 0},
		{"the other leg at leg_unhealthy with no verdict", func(h *reactHarness) {
			h.legOf(reactDataLegB).ErrEpoch = h.ago(common.DefaultLegUnhealthy)
		}, "", 0},
		{"the other leg at leg_unhealthy", func(h *reactHarness) {
			h.legOf(reactDataLegB).ErrEpoch = h.ago(common.DefaultLegUnhealthy)
			h.w.verdicts.noteLeg(reactDataLegB, true)
		}, triggerOtherLegUnhealthy, reactDataLegB},
		{"the other leg's side at side_unhealthy", func(h *reactHarness) {
			leg := h.legOf(reactDataLegB)
			leg.ErrEpoch = h.ago(1)
			leg.SideList[0].ErrEpoch = h.ago(common.DefaultSideUnhealthy)
			h.w.verdicts.noteLeg(reactDataLegB, true)
			h.w.verdicts.noteSide(sideB, true)
		}, triggerOtherLegUnhealthy, reactDataLegB},
		{"the other leg's side alone", func(h *reactHarness) {
			h.legOf(reactDataLegB).SideList[0].ErrEpoch = h.ago(9000)
			h.w.verdicts.noteSide(sideB, true)
		}, "", 0},
		{"the bad leg healthy and the new leg due", func(h *reactHarness) {
			h.legOf(reactDataLegA).ErrEpoch = 0
			h.legOf(reactNewLeg).ErrEpoch = h.ago(common.DefaultLegUnhealthy)
			h.w.verdicts.noteLeg(reactNewLeg, true)
		}, triggerBadLegHealthy, reactDataLegA},
		{"the new leg due and the other leg due", func(h *reactHarness) {
			h.legOf(reactNewLeg).ErrEpoch = h.ago(common.DefaultLegUnhealthy)
			h.w.verdicts.noteLeg(reactNewLeg, true)
			h.legOf(reactDataLegB).ErrEpoch = h.ago(common.DefaultLegUnhealthy)
			h.w.verdicts.noteLeg(reactDataLegB, true)
		}, triggerNewLegUnhealthy, reactNewLeg},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.judgeStored = false
			repName, _ := withWorkerRepair(h,
				pb.RepStage_REP_STAGE_CREATED, false)
			h.w.verdicts.noteLeg(reactDataLegA, true)
			h.dnCands(reactDnD)
			c.set(h)
			h.pass()
			if c.trigger == "" {
				h.wantOps()
				h.wantApplied()
				return
			}
			calls := h.wantOps("abort_repair")
			if calls[0].repName != repName {
				t.Fatalf("abort = %+v", calls[0])
			}
			rec := h.wantApplied(reactionRepairAbort)[0]
			if rec["trigger"] != c.trigger ||
				rec["leg_id"] != float64(c.legId) {
				t.Fatalf("applied = %v, want trigger %s on leg %d", rec,
					c.trigger, c.legId)
			}
		})
	}
}

// TestLegRepairDrivesOnlyWhatIsDue pins what the drive step leaves alone: an
// operator's repair at any stage (AR9); a CREATED worker's repair whose
// abort flag is set already, which waits for the primary's verdict; the
// repair of a deleting clone, which the clone drain gives back; and a
// repair whose stage this pass moved, which the next pass drives from its
// new stage.
func TestLegRepairDrivesOnlyWhatIsDue(t *testing.T) {
	for _, stage := range []pb.RepStage{
		pb.RepStage_REP_STAGE_CREATED,
		pb.RepStage_REP_STAGE_STARTED,
		pb.RepStage_REP_STAGE_ABORTED,
	} {
		t.Run("an operator's repair at "+stage.String(), func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			withReactRepair(h, stage, stage != pb.RepStage_REP_STAGE_CREATED)
			// Its bad leg reads healthy: a worker's repair would be tried
			// to abort.
			h.legOf(reactDataLegA).ErrEpoch = 0
			h.pass()
			h.wantOps()
			h.wantApplied()
		})
	}

	t.Run("the abort flag already set", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withWorkerRepair(h, pb.RepStage_REP_STAGE_CREATED, true)
		h.legOf(reactDataLegA).ErrEpoch = 0
		h.pass()
		h.wantOps()
		h.wantApplied()
	})

	t.Run("a deleting clone's repair", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		grp := withReactClone(t, h.state)
		h.state.Clones[reactCloneNm].Deleting = true
		repName := common.WorkerRepName(testSpId, reactCloneGrp, 1)
		h.state.Repairs = map[string]*pb.Repair{repName: {
			RepId: reactRepId, GrpId: reactCloneGrp,
			RepStage:     pb.RepStage_REP_STAGE_STARTED,
			BadLegIdList: []uint64{reactCloneLegA},
			NewLegList: []*pb.Leg{{
				LegId: reactNewLeg, LegIdx: 2,
				SideList: []*pb.Side{{
					SideId: reactNewSide, AddrPort: reactDnC,
				}},
			}},
			CloneName: reactCloneNm,
		}}
		h.state.Conf.RepairNameList = []string{repName}
		grp.RepName = repName
		h.pass()
		// The clone drain's final step runs in the same pass (CLD7); the
		// repair is not finished.
		h.wantOps("finish_clone")
		h.wantApplied()
	})

	// The primary reports the repair started in the pass where its bad leg
	// reads healthy again: the stage move commits, and the snapshot's
	// CREATED record, which would ask for a try-to-abort, is left alone.
	t.Run("a repair whose stage this pass moved", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		repName, rep := withWorkerRepair(h,
			pb.RepStage_REP_STAGE_CREATED, false)
		h.legOf(reactDataLegA).ErrEpoch = 0
		h.setVerdict(reactRepId, true, false)
		h.pass()
		calls := h.wantOps("set_repair_stage")
		if calls[0].repName != repName {
			t.Fatalf("move = %+v", calls[0])
		}
		h.wantApplied()

		// The move commits; the next pass finishes the repair.
		rep.RepStage = pb.RepStage_REP_STAGE_STARTED
		h.pass()
		h.wantOps("set_repair_stage", "finish_repair")
		h.wantApplied(reactionRepairFinish)
	})
}

// TestLegRepairDriveRefusalHoldsOnlyItsGroup pins the drive step's refusals:
// a model.ErrPrecondition holds only the repair it names, is logged with the
// op's reason and name, and the walk goes on to the next repair, which is
// driven in the same pass, and with no other repair due, on to the create in
// the same pass; a failure that is no refusal ends the pass, since its
// transaction may have committed.
func TestLegRepairDriveRefusalHoldsOnlyItsGroup(t *testing.T) {
	build := func(t *testing.T) (*reactHarness, string, string) {
		h := newReactHarness(t, reactFixture(t))
		metaRep := common.WorkerRepName(testSpId, reactMetaGrp, 1)
		dataRep := common.WorkerRepName(testSpId, reactDataGrp, 1)
		withRepair(h, metaRep, h.metaGrp(), reactMetaLegA,
			pb.RepStage_REP_STAGE_STARTED, false, 900)
		withRepair(h, dataRep, h.dataGrp(), reactDataLegA,
			pb.RepStage_REP_STAGE_ABORTED, true, 910)
		return h, metaRep, dataRep
	}

	t.Run("a refusal", func(t *testing.T) {
		h, metaRep, dataRep := build(t)
		h.rops.repairErr = map[string]error{
			metaRep: &model.ErrPrecondition{
				Op: "FinishRepair", Reason: "repair stage is REP_STAGE_CREATED",
			},
		}
		h.pass()
		calls := h.wantOps("finish_repair", "cancel_repair")
		if calls[0].repName != metaRep || calls[1].repName != dataRep {
			t.Fatalf("calls = %+v", calls)
		}
		h.wantSkippedBy(reactionRepairFinish,
			"repair stage is REP_STAGE_CREATED", "FinishRepair")
		h.wantApplied(reactionRepairCancel)
	})

	t.Run("a refusal, then the create", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		metaRep := common.WorkerRepName(testSpId, reactMetaGrp, 1)
		withRepair(h, metaRep, h.metaGrp(), reactMetaLegA,
			pb.RepStage_REP_STAGE_STARTED, false, 900)
		h.rops.repairErr = map[string]error{
			metaRep: &model.ErrPrecondition{
				Op: "FinishRepair", Reason: "repair stage is REP_STAGE_CREATED",
			},
		}
		// The data group, which has no repair, has a due leg.
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnD)
		h.pass()
		calls := h.wantOps("finish_repair", "create_repair")
		if calls[0].repName != metaRep || calls[1].grpId != reactDataGrp {
			t.Fatalf("calls = %+v", calls)
		}
		h.wantSkippedBy(reactionRepairFinish,
			"repair stage is REP_STAGE_CREATED", "FinishRepair")
		h.wantApplied(reactionRepairCreate)
	})

	t.Run("a failure", func(t *testing.T) {
		h, metaRep, _ := build(t)
		h.rops.repairErr = map[string]error{
			metaRep: errors.New("etcdserver: request timed out"),
		}
		h.pass()
		h.wantOps("finish_repair")
		h.wantSkipped(reactionRepairFinish, reasonOpFailed)
		h.wantApplied()
	})
}

// TestLegRepairDriveStepIsAReaction pins where the drive step runs: first in
// leg repair, so after the reactions of higher priority, the failover, the
// thin-pool grow and the cntlr replacement (AR2), and only where reactions
// run (AR3): at a level that suppresses reactions the stage move still runs
// and the worker's STARTED repair waits for the operator, who drives it with
// the gateway's calls.
func TestLegRepairDriveStepIsAReaction(t *testing.T) {
	t.Run("a failover first", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withWorkerRepair(h, pb.RepStage_REP_STAGE_STARTED, false)
		h.state.Cntlrs[reactCntlrA].ErrEpoch = h.ago(9000)
		h.pass()
		h.wantOps("failover")
		h.wantApplied(reactionFailover)
	})

	// The pool's data usage is far over the watermark: the grow runs first.
	t.Run("a grow first", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withWorkerRepair(h, pb.RepStage_REP_STAGE_STARTED, false)
		total := reactDataBlocks(t, 2)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, total, total))
		h.dnCands(reactDnC, reactDnD)
		h.pass()
		h.wantOps("grow")
		h.wantApplied(reactionGrowData)
	})

	// The standby has been unhealthy past the cntlr threshold: its
	// replacement runs first.
	t.Run("a replacement first", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withWorkerRepair(h, pb.RepStage_REP_STAGE_ABORTED, true)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(1, 1000, 1, 1000))
		h.state.Cntlrs[reactCntlrB].ErrEpoch = h.ago(700)
		h.cnCands(reactCnC)
		h.pass()
		h.wantOps("replace")
		h.wantApplied(reactionReplaceCntlr)
	})

	t.Run("the drive before the create", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		repName, _ := withWorkerRepair(h, pb.RepStage_REP_STAGE_ABORTED, true)
		h.legOf(reactMetaLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnD)
		h.pass()
		h.wantOps("cancel_repair")
		h.wantNoScan()

		// The cancel commits: the group is free and the next pass creates.
		delete(h.state.Repairs, repName)
		h.state.Conf.RepairNameList = nil
		h.dataGrp().RepName = ""
		h.pass()
		calls := h.wantOps("cancel_repair", "create_repair")
		if calls[1].grpId != reactMetaGrp {
			t.Fatalf("create = %+v, want the meta group", calls[1])
		}
	})

	t.Run("suppressed", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.state.Conf.SpLevel = pb.SpLevel_SP_LEVEL_NO_THINPOOL
		repName, _ := withWorkerRepair(h,
			pb.RepStage_REP_STAGE_CREATED, false)
		h.setVerdict(reactRepId, true, false)
		h.pass()
		calls := h.wantOps("set_repair_stage")
		if calls[0].repName != repName {
			t.Fatalf("move = %+v", calls[0])
		}
		h.state.Repairs[repName].RepStage = pb.RepStage_REP_STAGE_STARTED
		h.pass()
		h.wantOps("set_repair_stage")
		h.wantApplied()
	})
}

// TestLegRepairCloneMetaGroup pins leg repair on a clone meta group: the same
// create, its parent the clone by name, scanned with the group's own
// ext_cnt and a black list of its legs' DNs, the record naming the clone;
// and the same drive step.
func TestLegRepairCloneMetaGroup(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	grp := withReactClone(t, h.state)
	cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
	h.dnCands(reactDnD)
	h.pass()
	calls := h.wantOps("create_repair")
	call := calls[0]
	if call.cloneName != reactCloneNm || call.sliceId != 0 ||
		call.grpId != reactCloneGrp || call.badLegId != reactCloneLegA ||
		call.repName != h.workerRepNameOf(reactCloneGrp) {
		t.Fatalf("create = %+v", call)
	}
	query := h.rops.allQueries()[0]
	if query.candExt != grp.GetExtCnt() {
		t.Fatalf("candExt = %d, want the clone group's %d", query.candExt,
			grp.GetExtCnt())
	}
	var want []string
	for _, leg := range grp.GetLegList() {
		want = append(want, leg.GetSideList()[0].GetAddrPort())
	}
	if fmt.Sprint(call.grpDn) != fmt.Sprint(want) {
		t.Fatalf("plan = %v, want %v", call.grpDn, want)
	}
	wantCloneRecord(t, h.wantApplied(reactionRepairCreate)[0],
		reactCloneLegA)

	// The repair commits and the primary starts it: the drive step finishes
	// it.
	h.state.Repairs = map[string]*pb.Repair{call.repName: {
		RepId: 887, GrpId: reactCloneGrp,
		RepStage:     pb.RepStage_REP_STAGE_STARTED,
		BadLegIdList: []uint64{reactCloneLegA},
		NewLegList: []*pb.Leg{{
			LegId: 888, LegIdx: 2,
			SideList: []*pb.Side{{
				SideId: 889, AddrPort: reactDnD, Provisioned: true,
			}},
		}},
		CloneName: reactCloneNm,
	}}
	h.state.Conf.RepairNameList = []string{call.repName}
	grp.RepName = call.repName
	h.pass()
	calls = h.wantOps("create_repair", "finish_repair")
	if calls[1].repName != call.repName {
		t.Fatalf("finish = %+v", calls[1])
	}
}

// TestLegRepairAbortOnACloneMetaGroup pins the try-to-abort on a clone meta
// group: the drive step finds a CREATED worker's repair's group through the
// clone the repair names, so the clone's bad leg reading healthy again asks
// for the abort, naming that leg; a repair whose grp_id is not the clone's
// meta group's finds no group and asks for nothing.
func TestLegRepairAbortOnACloneMetaGroup(t *testing.T) {
	build := func(t *testing.T, grpId uint64) (*reactHarness, string) {
		t.Helper()
		h := newReactHarness(t, reactFixture(t))
		grp := withReactClone(t, h.state)
		repName := common.WorkerRepName(testSpId, reactCloneGrp, 1)
		h.state.Repairs = map[string]*pb.Repair{repName: {
			RepId: reactRepId, GrpId: grpId,
			RepStage:     pb.RepStage_REP_STAGE_CREATED,
			BadLegIdList: []uint64{reactCloneLegA},
			NewLegList: []*pb.Leg{{
				LegId: reactNewLeg, LegIdx: 2,
				SideList: []*pb.Side{{
					SideId: reactNewSide, AddrPort: reactDnC,
					Provisioned: true,
				}},
			}},
			CloneName: reactCloneNm,
		}}
		h.state.Conf.RepairNameList = []string{repName}
		grp.RepName = repName
		// The bad leg reads healthy again: its err_epoch is clear.
		cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = 0
		return h, repName
	}

	t.Run("the clone's meta group", func(t *testing.T) {
		h, repName := build(t, reactCloneGrp)
		h.pass()
		calls := h.wantOps("abort_repair")
		if calls[0].repName != repName {
			t.Fatalf("abort = %+v", calls[0])
		}
		rec := h.wantApplied(reactionRepairAbort)[0]
		if rec["trigger"] != triggerBadLegHealthy ||
			rec["leg_id"] != float64(reactCloneLegA) ||
			rec["rep_name"] != repName {
			t.Fatalf("applied = %v, want %s on leg %d", rec,
				triggerBadLegHealthy, reactCloneLegA)
		}
	})

	t.Run("a grp_id that is not the clone's", func(t *testing.T) {
		h, _ := build(t, reactCloneGrp+1)
		h.pass()
		h.wantOps()
		h.wantApplied()
	})
}

// TestLegRepairFromCreateToCancel pins a worker's repair from create
// to cancel through the pass alone, each step one pass: the create, the bad
// leg returning, the try-to-abort, the primary's abort confirmed and the
// stage move, and the cancel; then the group is free again.
func TestLegRepairFromCreateToCancel(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
	h.dnCands(reactDnD)
	h.pass()
	create := h.wantOps("create_repair")[0]

	// The create commits as model.CreateRepair writes it.
	rep := withRepair(h, create.repName, h.dataGrp(), reactDataLegA,
		pb.RepStage_REP_STAGE_CREATED, false, 887)
	h.legOf(reactDataLegA).ErrEpoch = 0
	h.pass()
	h.wantOps("create_repair", "abort_repair")

	rep.TryToAbort = true
	h.setVerdict(887, false, true)
	h.pass()
	h.wantOps("create_repair", "abort_repair", "set_repair_stage")

	rep.RepStage = pb.RepStage_REP_STAGE_ABORTED
	h.pass()
	h.wantOps("create_repair", "abort_repair", "set_repair_stage",
		"cancel_repair")
	h.wantApplied(reactionRepairCreate, reactionRepairAbort,
		reactionRepairCancel)
}
