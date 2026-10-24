package worker

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/integtest/plant"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The whole-worker scenario cases of leg repair (dnv-worker.md, AR8): the
// worker's own repair of a leg that has been unhealthy too long, from its
// create to its finish or its cancel, played through the fakes of one
// planted cluster under one whole worker. The fakes' repairs lever stands for
// the primary's verdict on a repair, keyed by the repair's name, which the
// case reads off the create's record.

// repairShape is the standard cluster with a fourth DN. The data group's legs
// sit on dn 1 and dn 2, so dn 3 and dn 4 are both outside it: a second create
// still has a DN to go to, and a case's count of creates can catch one.
var repairShape = scenShape{extraDns: 1}

// repairDns are the DNs outside the data group in repairShape.
var repairDns = []uint64{3, 4}

// repairDnOf is the DN of repairDns that a create record names, or 0.
func repairDnOf(rec logRecord) uint64 {
	for _, id := range repairDns {
		if rec.str("addr_port") == stdDnAddr(id) {
			return id
		}
	}
	return 0
}

// nextRepairIds are the rep_id, the new leg's id and its side's id the next
// create takes: CreateRepair takes the pool's next three ids in that order.
func (c *stdCluster) nextRepairIds(t *testing.T) (uint64, uint64, uint64) {
	t.Helper()
	next := c.nextSpId(t)
	return next, next + 1, next + 2
}

// creates counts the worker's creates of a repair.
func (h *workerHarness) creates(w *harnessWorker) int {
	return h.countRecs(w, msgReactionApplied, reactionIs(reactionRepairCreate, ""))
}

// waitCreated waits for the worker's create of a repair of legId and returns
// its record, which names the repair by a name with the reserved prefix.
func (h *workerHarness) waitCreated(
	t *testing.T, w *harnessWorker, legId uint64,
) logRecord {
	t.Helper()
	rec := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		and(reactionIs(reactionRepairCreate, ""), numIs("leg_id", legId)))
	if !common.HasRepNameReservedPrefix(rec.str("rep_name")) {
		t.Fatalf("the create %v names no worker's repair", rec.attrs)
	}
	return rec
}

// waitOneCreate waits, as waitCount does, for n records of w with msg that
// match accepts, and fails as soon as a second create is applied meanwhile
// (while names the moment for the message).
func (h *workerHarness) waitOneCreate(
	t *testing.T,
	timeout time.Duration,
	w *harnessWorker,
	msg string,
	n int,
	match func(logRecord) bool,
	while string,
) {
	t.Helper()
	h.waitUntil(t, timeout, fmt.Sprintf("%d %q records", n, msg),
		func() (bool, string) {
			if got := h.creates(w); got != 1 {
				t.Fatalf("%d repairs created %s, want 1", got, while)
			}
			got := h.countRecs(w, msg, match)
			return got >= n, fmt.Sprintf("%d so far", got)
		})
}

// finishByVerdict sets the primary's verdict started on the worker's repair
// repName and waits for its stage move and then for the drive step's
// finish, which it returns.
func (c *stdCluster) finishByVerdict(
	t *testing.T, w *harnessWorker, repName string,
) logRecord {
	t.Helper()
	c.setVerdictLever(t, repName, agentfake.RepairVerdict{Started: true})
	c.h.waitRecord(t, 20*time.Second, w, msgRepairStageMoved,
		func(rec logRecord) bool {
			return rec.str("rep_name") == repName &&
				rec.str("stage") == pb.RepStage_REP_STAGE_STARTED.String()
		})
	return c.h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		func(rec logRecord) bool {
			return rec.str("kind") == reactionRepairFinish &&
				rec.str("rep_name") == repName
		})
}

// cancelByVerdict sets the primary's verdict abort confirmed on the
// worker's repair repName, whose abort flag the worker set, and waits for
// its stage move and then for the drive step's cancel.
func (c *stdCluster) cancelByVerdict(
	t *testing.T, w *harnessWorker, repName string,
) {
	t.Helper()
	c.setVerdictLever(t, repName,
		agentfake.RepairVerdict{AbortConfirmed: true})
	c.h.waitRecord(t, 20*time.Second, w, msgRepairStageMoved,
		func(rec logRecord) bool {
			return rec.str("rep_name") == repName &&
				rec.str("stage") == pb.RepStage_REP_STAGE_ABORTED.String()
		})
	c.h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		func(rec logRecord) bool {
			return rec.str("kind") == reactionRepairCancel &&
				rec.str("rep_name") == repName
		})
	if c.repair(t, repName) != nil {
		t.Fatalf("the repair %s outlived its cancel", repName)
	}
}

// TestScenLegRepairToFinish pins AR8 case 1 end to end: a data leg the
// primary reports ERROR past leg_unhealthy gets one worker's repair, its new
// leg on a DN outside the group; the new leg's side is zeroed and flipped
// provisioned in the repair (RW18); the primary's verdict started moves the
// repair to STARTED (AR11) and the next pass's drive step finishes it: the
// new leg takes the bad leg's place and the bad leg is a retired leg. No
// second repair is created, and the pool converges.
func TestScenLegRepairToFinish(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantScen(t, repairShape)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	repId, newLeg, newSide := c.nextRepairIds(t)
	c.failLeg(t, 6)
	created := h.waitCreated(t, w, 6)
	repName := created.str("rep_name")
	if created.num("rep_id") != repId || created.num("new_leg_id") != newLeg ||
		repairDnOf(created) == 0 {
		t.Fatalf("the create %v, want rep_id %d, new leg %d on dn 3 or dn 4",
			created.attrs, repId, newLeg)
	}
	h.waitRecord(t, 20*time.Second, w, msgFlipApplied,
		func(rec logRecord) bool {
			return rec.str("rep_name") == repName &&
				rec.num("side_id") == newSide
		})

	finished := c.finishByVerdict(t, w, repName)
	if finished.num("retired_leg_id") != 6 {
		t.Fatalf("the finish %v, want leg 6 retired", finished.attrs)
	}
	if _, ok := finished.attrs["given_back_leg_id"]; ok {
		t.Fatalf("the finish %v gave a retired leg back below the bound",
			finished.attrs)
	}
	state := c.load(t)
	if _, ok := state.RetiredLegs[6]; !ok {
		t.Fatalf("leg 6 is no retired leg after the finish")
	}
	if got := legIds(c.dataGrp(t).GetLegList()); !slices.Contains(got, newLeg) ||
		slices.Contains(got, 6) {
		t.Fatalf("the data group holds legs %v, want %d in place of 6",
			got, newLeg)
	}
	c.waitConverged(t)
	if n := h.creates(w); n != 1 {
		t.Errorf("%d repairs created for one leg, want 1", n)
	}
}

// TestScenLegRepairAbortedWhenTheBadLegReturns pins the try-to-abort's first
// trigger end to end: the bad leg of a worker's CREATED repair reads OK
// again before the copy starts, so the next pass sets the repair's abort
// flag; the primary confirms the abort, the stage moves to ABORTED (AR11),
// and the drive step cancels the repair, which gives its new leg's extents
// back to its DN. The leg, healthy, gets no second repair.
func TestScenLegRepairAbortedWhenTheBadLegReturns(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantScen(t, repairShape)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	before := map[uint64]uint64{}
	for _, id := range repairDns {
		before[id] = c.dnFree(t, id)
	}

	c.failLeg(t, 6)
	created := h.waitCreated(t, w, 6)
	repName := created.str("rep_name")
	newDn := repairDnOf(created)
	c.healLeg(t, 6)
	aborted := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		func(rec logRecord) bool {
			return rec.str("kind") == reactionRepairAbort &&
				rec.str("rep_name") == repName
		})
	if aborted.str("trigger") != triggerBadLegHealthy ||
		aborted.num("leg_id") != 6 {
		t.Fatalf("the try-to-abort %v, want trigger %s on leg 6",
			aborted.attrs, triggerBadLegHealthy)
	}
	if !c.repair(t, repName).GetTryToAbort() {
		t.Fatalf("the try-to-abort set no flag")
	}

	c.cancelByVerdict(t, w, repName)
	c.waitConverged(t)
	if got := c.dnFree(t, newDn); got != before[newDn] {
		t.Errorf("dn %d holds %d free extents after the cancel, want %d",
			newDn, got, before[newDn])
	}
	if n := h.creates(w); n != 1 {
		t.Errorf("%d repairs created, want 1", n)
	}
	if got := legIds(c.dataGrp(t).GetLegList()); !slices.Equal(got,
		[]uint64{6, 7}) {
		t.Errorf("the data group holds legs %v, want 6 and 7", got)
	}
}

// TestScenLegRepairAbortsANewLegWhoseDnDiesWhileZeroing pins the side clock
// of the try-to-abort (AR8): the new leg's DN stops while the new leg's side
// zeroes, so the side's health epoch is set while the leg, neither connected
// nor probed, has none; once that epoch is side_unhealthy old the worker
// tries to abort the repair, the primary confirms, and the drive step
// cancels it, giving the side back. The leg, still bad, gets a second repair
// on the other DN outside the group, the dead DN being out of allocation.
func TestScenLegRepairAbortsANewLegWhoseDnDiesWhileZeroing(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantScen(t, repairShape)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	_, newLeg, newSide := c.nextRepairIds(t)
	newKey := agentfake.SideKey(stdSpId, newLeg, newSide)
	for _, id := range repairDns {
		slowZeroing(t, c.dnAgent(id), newKey, true)
	}
	c.failLeg(t, 6)
	created := h.waitCreated(t, w, 6)
	repName := created.str("rep_name")
	deadDn := repairDnOf(created)
	if deadDn == 0 {
		t.Fatalf("the new leg went to %s, want dn 3 or dn 4",
			created.str("addr_port"))
	}

	dn := c.dnAgent(deadDn)
	dn.Stop()
	defer func() {
		slowZeroing(t, dn, newKey, false)
		if err := dn.Start(agentfake.KeepStore); err != nil {
			t.Errorf("restart dn %d: %v", deadDn, err)
		}
	}()
	h.waitUntil(t, 20*time.Second, "the new leg's side to carry a health "+
		"epoch", func() (bool, string) {
		_, side, ok := c.sideOf(t, newSide)
		return ok && side.GetErrEpoch() != 0, fmt.Sprintf("%v", side)
	})
	aborted := h.waitRecord(t, 30*time.Second, w, msgReactionApplied,
		func(rec logRecord) bool {
			return rec.str("kind") == reactionRepairAbort &&
				rec.str("rep_name") == repName
		})
	if aborted.str("trigger") != triggerNewLegUnhealthy ||
		aborted.num("leg_id") != newLeg {
		t.Fatalf("the try-to-abort %v, want trigger %s on leg %d",
			aborted.attrs, triggerNewLegUnhealthy, newLeg)
	}
	c.cancelByVerdict(t, w, repName)

	second := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		func(rec logRecord) bool {
			return rec.str("kind") == reactionRepairCreate &&
				rec.str("rep_name") != repName
		})
	if got := repairDnOf(second); got == 0 || got == deadDn {
		t.Fatalf("the second create %v, want the DN outside the group other "+
			"than dn %d", second.attrs, deadDn)
	}
}

// TestScenLegRepairWaitsForADnWithRoom pins the create's scan: with the one
// DN outside the group full (the group's DNs are black-listed), the repair
// finds no candidate and logs `reaction skipped` (no candidate) on every
// pass; once that DN has room, the next pass creates the repair there, and
// the primary's verdict lets the drive step finish it.
func TestScenLegRepairWaitsForADnWithRoom(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	ctx := context.Background()
	if _, _, err := plant.SetDnFree(ctx, h.cli, c.cl, 3, 0); err != nil {
		t.Fatalf("SetDnFree: %v", err)
	}
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	c.failLeg(t, 6)
	noRoom := and(reactionIs(reactionRepairCreate, reasonNoCandidate),
		numIs("leg_id", 6))
	h.waitCount(t, 20*time.Second, w, msgReactionSkipped, 2, noRoom)
	if n := h.countRecs(w, msgReactionApplied, nil); n != 0 {
		t.Fatalf("%d reactions applied with no DN for a new leg, want 0", n)
	}

	if _, _, err := plant.SetDnFree(ctx, h.cli, c.cl, 3, c.spec.nodeFree); err != nil {
		t.Fatalf("SetDnFree: %v", err)
	}
	created := h.waitCreated(t, w, 6)
	if got := created.str("addr_port"); got != stdDnAddr(3) {
		t.Errorf("the new leg went to %s, want dn 3", got)
	}
	c.finishByVerdict(t, w, created.str("rep_name"))
}

// TestScenLegRepairWaitsForANewLegThePrimaryNeverReports pins the Known
// limit of the try-to-abort: the primary's syncups are held, so it never
// learns of the repair and reports no row of its new leg, which then never
// carries a health epoch; the repair stays CREATED past leg_unhealthy, with
// no try-to-abort and no second create. Once the primary takes its syncup,
// its verdict lets the drive step finish the repair.
func TestScenLegRepairWaitsForANewLegThePrimaryNeverReports(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantScen(t, repairShape)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	primary, primaryKey := c.cntlrAgent(t, 1)
	setLever(t, primary, primaryKey, func(o *agentfake.Object) { o.HangSyncup = true })
	defer setLever(t, primary, primaryKey, func(o *agentfake.Object) { o.HangSyncup = false })
	_, _, newSide := c.nextRepairIds(t)
	c.failLeg(t, 6)
	created := h.waitCreated(t, w, 6)
	repName := created.str("rep_name")
	h.waitUntil(t, 20*time.Second, "the new leg's side provisioned",
		func() (bool, string) {
			_, side, ok := c.sideOf(t, newSide)
			return ok && side.GetProvisioned(), fmt.Sprintf("%v", side)
		})
	// Each pass holds leg 6's group: by its repair, or by the gate while the
	// primary's report is older than the create's revision.
	waiting := and(reactionIs(reactionRepairCreate, ""), numIs("leg_id", 6))
	from := h.countRecs(w, msgReactionSkipped, waiting)
	// leg_unhealthy plus two passes, every one of them waiting: a new leg
	// with no report has no clock.
	waits := int(c.spec.threshold.GetLegUnhealthy()) + 2
	h.waitOneCreate(t, 30*time.Second, w, msgReactionSkipped, from+waits,
		waiting, "while the primary never reports the new leg")
	if n := h.countRecs(w, msgReactionApplied,
		reactionIs(reactionRepairAbort, "")); n != 0 {
		t.Fatalf("%d try-to-aborts of a repair whose new leg has no report",
			n)
	}
	if got := c.repair(t, repName).GetRepStage(); got !=
		pb.RepStage_REP_STAGE_CREATED {
		t.Fatalf("the repair at %s, want CREATED", got)
	}

	setLever(t, primary, primaryKey, func(o *agentfake.Object) { o.HangSyncup = false })
	c.finishByVerdict(t, w, repName)
}

// TestScenLegRepairGivesBackTheOldestRetiredLeg pins the worker's repair at
// the retired-leg bound end to end: the data group holds
// MaxRetiredLegPerGrp retired legs, and its failed leg still gets a repair
// with a fresh new leg; the finish gives back the oldest retired leg, whose
// side's extents go back to its DN, names it in its record, and keeps the
// newer ones beside the leg it retires.
func TestScenLegRepairGivesBackTheOldestRetiredLeg(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantScen(t, repairShape)
	ctx := context.Background()
	now := uint64(time.Now().Unix())
	var retired []plant.RetiredLeg
	for idx := range common.MaxRetiredLegPerGrp {
		leg, err := plant.PutRetiredLeg(ctx, h.cli, c.cl, c.sp.Ref,
			plant.RetiredLegSpec{
				GrpId: stdDataGrpId, LegIdx: uint32(2 + idx), Dn: 3,
				Slot: 0, Epoch: now - uint64(100*(common.MaxRetiredLegPerGrp-idx)),
			})
		if err != nil {
			t.Fatalf("PutRetiredLeg: %v", err)
		}
		retired = append(retired, leg)
	}
	oldest := retired[0]
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	freeBefore := c.dnFree(t, 3)

	c.failLeg(t, 6)
	created := h.waitCreated(t, w, 6)
	finished := c.finishByVerdict(t, w, created.str("rep_name"))
	if finished.num("given_back_leg_id") != oldest.LegId ||
		finished.num("retired_leg_id") != 6 {
		t.Fatalf("the finish %v, want leg %d given back and leg 6 retired",
			finished.attrs, oldest.LegId)
	}
	state := c.load(t)
	if _, ok := state.RetiredLegs[oldest.LegId]; ok {
		t.Fatalf("the oldest retired leg %d outlived the finish",
			oldest.LegId)
	}
	for _, leg := range append(slices.Clone(retired[1:]),
		plant.RetiredLeg{LegId: 6}) {
		if _, ok := state.RetiredLegs[leg.LegId]; !ok {
			t.Errorf("retired leg %d is gone after the finish", leg.LegId)
		}
	}
	// dn 3 got back the oldest's extents, and held the new leg's only when
	// the new leg went there.
	want := freeBefore + c.dataGrp(t).GetExtCnt()
	if repairDnOf(created) == 3 {
		want -= c.dataGrp(t).GetExtCnt()
	}
	if got := c.dnFree(t, 3); got != want {
		t.Errorf("dn 3 holds %d free extents after the finish, want %d",
			got, want)
	}
}
