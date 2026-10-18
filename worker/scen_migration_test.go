package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/integtest/plant"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The whole-worker scenario cases of a migration (dnv-worker.md, RW15 and
// RW18's cutover) and of its bitmap push (dnv-worker.md, BM3 and BM6), each
// on the standard cluster with a migration planted after it
// converged: side 10 of leg 6, on dn 1, moves to a new side on dn 3.

// stdMigrName is the planted migration's name.
const stdMigrName = "m0"

// stdMigr is a migration planted on the standard cluster, and the key of
// its destination side on dn 3's fake.
type stdMigr struct {
	plant.Migr
	dstKey string
}

// migrIds are the ids PutMigr is handed: the migration's and its new side's,
// the pool's next free two, so a case can set the levers of the new side
// before the worker hears of it.
func (c *stdCluster) migrIds(t *testing.T) (uint64, uint64) {
	t.Helper()
	next := c.nextSpId(t)
	return next, next + 1
}

// putStdMigr plants the migration of side 10 to dn 3 (CreateMigration's
// shape: cutover pending), with the ids migrIds gives.
func (c *stdCluster) putStdMigr(t *testing.T, migrId, dstSide uint64) stdMigr {
	t.Helper()
	migr, err := plant.PutMigr(context.Background(), c.h.cli, c.cl, c.sp.Ref,
		plant.MigrSpec{
			Name: stdMigrName, Id: migrId, SrcSide: 10, DstSide: dstSide,
			DstDn: 3, CutoverPending: true,
		})
	if err != nil {
		t.Fatalf("PutMigr: %v", err)
	}
	if migr.DstSide != dstSide || migr.LegId != 6 {
		t.Fatalf("the migration's new side is %d on leg %d, want %d on leg 6",
			migr.DstSide, migr.LegId, dstSide)
	}
	return stdMigr{Migr: migr, dstKey: agentfake.SideKey(stdSpId, 6, dstSide)}
}

// cutoverPending reads the migration's cutover_pending.
func (c *stdCluster) cutoverPending(t *testing.T) bool {
	t.Helper()
	migr, ok := c.load(t).Migrs[stdMigrName]
	if !ok {
		t.Fatalf("no migration %q in the pool", stdMigrName)
	}
	return migr.GetCutoverPending()
}

// isCutover matches the `flip applied` or `sp flip failed` record of the
// migration's cutover.
func isCutover(rec logRecord) bool {
	return rec.str("kind") == flipKindCutover && rec.str("migr_name") == stdMigrName
}

// isFlipOf matches the provisioned flip of one side.
func isFlipOf(sideId uint64) func(logRecord) bool {
	return func(rec logRecord) bool {
		return rec.str("kind") == flipKindProvisioned && rec.num("side_id") == sideId
	}
}

// pathLever holds a cntlr's path row to a side at status, while the cntlr
// is primary or always.
func (c *stdCluster) pathLever(
	t *testing.T,
	cntlrId uint64,
	sideId uint64,
	status pb.ResStatus,
	whenPrimary bool,
) {
	t.Helper()
	agent, key := c.cntlrAgent(t, cntlrId)
	setRow(t, agent, key, fmt.Sprintf("side_id_to_path.%d", sideId),
		rowLever(status, "", whenPrimary))
}

// clearPathLever drops it.
func (c *stdCluster) clearPathLever(t *testing.T, cntlrId uint64, sideId uint64) {
	t.Helper()
	agent, key := c.cntlrAgent(t, cntlrId)
	clearRow(t, agent, key, fmt.Sprintf("side_id_to_path.%d", sideId))
}

// waitNoCutover holds for n rounds of a cntlr and fails the case on any
// cutover record meanwhile, or on a cleared cutover_pending.
func (c *stdCluster) waitNoCutover(t *testing.T, w *harnessWorker, cntlrId uint64, n int, why string) {
	t.Helper()
	agent, key := c.cntlrAgent(t, cntlrId)
	c.h.waitRounds(t, agent, n, key)
	if k := c.h.countRecs(w, msgFlipApplied, isCutover) +
		c.h.countRecs(w, msgSpFlipFailed, isCutover); k != 0 {
		t.Fatalf("%d cutover records %s", k, why)
	}
	if !c.cutoverPending(t) {
		t.Fatalf("the cutover started %s", why)
	}
}

// TestScenMigrationDestinationThatNeverZeroes pins RW18's provisioned flip
// and RW15's source conf for a migration whose destination does not finish
// its zeroing: the destination is never flipped provisioned, so no cutover
// starts and the source is told the destination is not provisioned and the
// cutover pending, which keeps it serving; once the zeroing ends, the flip
// and then the cutover follow, and the source is told both.
func TestScenMigrationDestinationThatNeverZeroes(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	migrId, dstSide := c.migrIds(t)
	dn3 := c.dnAgent(3)
	slowZeroing(t, dn3, agentfake.SideKey(stdSpId, 6, dstSide), true)
	m := c.putStdMigr(t, migrId, dstSide)
	h.waitRecord(t, 20*time.Second, w, msgSyncupResult, func(rec logRecord) bool {
		id, ok := sideIdOf(rec)
		return ok && id == dstSide && rec.num("code") == 0 && rec.str("error") == ""
	})
	// Three rounds of the destination report it zeroing.
	h.waitRounds(t, dn3, 3, m.dstKey)
	if _, _, side, ok := c.sideOf(t, dstSide); !ok || side.GetProvisioned() {
		t.Fatalf("the destination while it zeroes: found %t, %v", ok, side)
	}
	if n := h.countRecs(w, msgFlipApplied, isFlipOf(dstSide)) +
		h.countRecs(w, msgFlipApplied, isCutover); n != 0 {
		t.Fatalf("%d flips of a destination that has not zeroed", n)
	}
	if !c.cutoverPending(t) {
		t.Fatalf("the cutover started before the destination zeroed")
	}
	srcAgent, srcKey := c.sideAgent(t, 10)
	msg, _, _ := lastSyncup(srcAgent, srcKey)
	src := msg.(*pb.SyncupSideRequest).GetMigrSrcConf()
	if src.GetMigrId() != migrId || src.GetDstProvisioned() || !src.GetCutoverPending() {
		t.Fatalf("the source's conf while the destination zeroes: %v, want the "+
			"destination not provisioned and the cutover pending", src)
	}

	slowZeroing(t, dn3, m.dstKey, false)
	h.waitRecord(t, 20*time.Second, w, msgFlipApplied, isFlipOf(dstSide))
	cutover := h.waitRecord(t, 20*time.Second, w, msgFlipApplied, isCutover)
	if cutover.num("cntlr_id") != 1 {
		t.Errorf("the cutover acted on cntlr %d's report, want the primary's",
			cutover.num("cntlr_id"))
	}
	h.waitUntil(t, 20*time.Second, "the source told the cutover started",
		func() (bool, string) {
			msg, _, _ := lastSyncup(srcAgent, srcKey)
			req := msg.(*pb.SyncupSideRequest)
			src := req.GetMigrSrcConf()
			return req.GetRevision() == c.spRev(t) && src.GetDstProvisioned() &&
				!src.GetCutoverPending(), fmt.Sprintf("%v", src)
		})
}

// TestScenCutoverWaitsOutAMissedDestinationRound pins RW18's read of the
// destination: a destination whose rounds go unanswered has its rows marked
// UNKNOWN, so it reads no wait for the cutover, and the cutover does not start
// though the primary reports a live path to it, until a later round of the
// destination reports the wait again.
func TestScenCutoverWaitsOutAMissedDestinationRound(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	migrId, dstSide := c.migrIds(t)
	c.pathLever(t, 1, dstSide, pb.ResStatus_RES_STATUS_PROVISIONING, true)
	m := c.putStdMigr(t, migrId, dstSide)
	h.waitRecord(t, 20*time.Second, w, msgFlipApplied, isFlipOf(dstSide))
	dn3 := c.dnAgent(3)
	h.waitRounds(t, dn3, 2, m.dstKey)

	setLever(t, dn3, m.dstKey, func(o *agentfake.Object) { o.Hang = true })
	defer setLever(t, dn3, m.dstKey, func(o *agentfake.Object) { o.Hang = false })
	h.waitUntil(t, 20*time.Second, "a missed round of the destination",
		func() (bool, string) {
			_, _, side, ok := c.sideOf(t, dstSide)
			return ok && side.GetErrEpoch() != 0, fmt.Sprintf("%v", side)
		})
	c.clearPathLever(t, 1, dstSide)
	c.waitNoCutover(t, w, 1, 3, "while the destination's rounds go unanswered")

	resumed := time.Now()
	setLever(t, dn3, m.dstKey, func(o *agentfake.Object) { o.Hang = false })
	cutover := h.waitRecord(t, 20*time.Second, w, msgFlipApplied, isCutover)
	if cutover.at.Before(resumed) {
		t.Errorf("the cutover started before the destination answered again")
	}
	if c.cutoverPending(t) {
		t.Errorf("cutover_pending still set after the cutover")
	}
}

// TestScenCutoverWaitsForThePrimarysPath pins RW18's path condition and
// HL2's path rows: while the primary reports its path to the destination
// ERROR, the cutover waits and the primary gets no health epoch, since a
// path row sets and clears nothing; once the path reads OK, the cutover
// starts on the primary's report.
func TestScenCutoverWaitsForThePrimarysPath(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	migrId, dstSide := c.migrIds(t)
	c.pathLever(t, 1, dstSide, pb.ResStatus_RES_STATUS_ERROR, true)
	c.putStdMigr(t, migrId, dstSide)
	h.waitRecord(t, 20*time.Second, w, msgFlipApplied, isFlipOf(dstSide))
	c.waitNoCutover(t, w, 1, 3, "while the primary's path is in error")
	if epoch := c.cntlr(t, 1).GetErrEpoch(); epoch != 0 {
		t.Fatalf("the primary has err_epoch %d from a path row", epoch)
	}

	c.clearPathLever(t, 1, dstSide)
	cutover := h.waitRecord(t, 20*time.Second, w, msgFlipApplied, isCutover)
	if cutover.num("cntlr_id") != 1 {
		t.Errorf("the cutover acted on cntlr %d's report, want the primary's",
			cutover.num("cntlr_id"))
	}
}

// TestScenCutoverAfterAFailoverWaitsForTheNewPrimary pins RW18 across a
// failover: a standby's live path counts for nothing, so the cutover waits
// while the primary's path is down; after a failover it waits for the new
// primary's own report, here held down while it is primary, and starts on
// that report once it reads OK, naming the new primary.
func TestScenCutoverAfterAFailoverWaitsForTheNewPrimary(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	migrId, dstSide := c.migrIds(t)
	c.pathLever(t, 1, dstSide, pb.ResStatus_RES_STATUS_PROVISIONING, false)
	c.pathLever(t, 2, dstSide, pb.ResStatus_RES_STATUS_PROVISIONING, true)
	c.putStdMigr(t, migrId, dstSide)
	h.waitRecord(t, 20*time.Second, w, msgFlipApplied, isFlipOf(dstSide))
	c.waitNoCutover(t, w, 2, 3, "on a standby's live path")

	disabled := true
	if _, _, err := plant.SetCntlr(context.Background(), h.cli, c.cl, c.sp.Ref, 1,
		plant.CntlrFlags{Disabled: &disabled}); err != nil {
		t.Fatalf("disable cntlr 1: %v", err)
	}
	h.waitUntil(t, 20*time.Second, "cntlr 2's promotion applied on its fake",
		func() (bool, string) {
			if c.primary(t) != 2 {
				return false, "cntlr 1 still primary"
			}
			for _, rec := range h.records(w, msgSyncupResult) {
				id, ok := cntlrIdOf(rec)
				if ok && id == 2 && rec.num("code") == 0 && rec.str("error") == "" &&
					rec.num("revision") >= c.spRev(t) {
					return true, ""
				}
			}
			return false, "no accepted promotion yet"
		})
	c.waitNoCutover(t, w, 2, 3, "while the new primary's path is down")

	c.clearPathLever(t, 2, dstSide)
	cutover := h.waitRecord(t, 20*time.Second, w, msgFlipApplied, isCutover)
	if cutover.num("cntlr_id") != 2 {
		t.Errorf("the cutover acted on cntlr %d's report, want the new "+
			"primary's, cntlr 2", cutover.num("cntlr_id"))
	}
}

// TestScenCutoverRefusedByThePoolLevel pins RW18's level rule: the
// coordinator does not read the pool's level, so at SP_LEVEL_NO_MIGRATION
// model.StartCutover refuses every attempt, which is logged as
// `sp flip failed` and tried again on a later report, the migration kept;
// once the level is lowered, the cutover commits.
func TestScenCutoverRefusedByThePoolLevel(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	ctx := context.Background()

	if _, err := plant.SetLevel(ctx, h.cli, c.cl, c.sp.Ref,
		pb.SpLevel_SP_LEVEL_NO_MIGRATION); err != nil {
		t.Fatalf("SetLevel: %v", err)
	}
	migrId, dstSide := c.migrIds(t)
	c.putStdMigr(t, migrId, dstSide)
	h.waitRecord(t, 20*time.Second, w, msgFlipApplied, isFlipOf(dstSide))
	// Two refused attempts: tried again on a later report.
	h.waitCount(t, 20*time.Second, w, msgSpFlipFailed, 2, isCutover)
	if n := h.countRecs(w, msgFlipApplied, isCutover); n != 0 {
		t.Fatalf("%d cutovers committed at SP_LEVEL_NO_MIGRATION", n)
	}
	if !c.cutoverPending(t) {
		t.Fatalf("the cutover started at SP_LEVEL_NO_MIGRATION")
	}

	if _, err := plant.SetLevel(ctx, h.cli, c.cl, c.sp.Ref,
		pb.SpLevel_SP_LEVEL_READWRITE); err != nil {
		t.Fatalf("SetLevel: %v", err)
	}
	h.waitRecord(t, 20*time.Second, w, msgFlipApplied, isCutover)
	if c.cutoverPending(t) {
		t.Errorf("cutover_pending still set after the cutover")
	}
}

// TestScenMigrationBitmapPushTimesOut pins BM3's deadline, the push timeout
// of the worker's seam (the case's, not the production one), and BM6: a
// bitmap push to the destination that its agent holds fails at that deadline
// with `bitmap push failed`, ends its plan and is not sent again while no
// syncup reply of the destination re-plans it; the next syncup reply, from a
// bump of the pool, re-plans it and the chunk is delivered. The destination's
// zeroing is held, so no flip moves the pool meanwhile. The push timeout is
// set apart from the syncup and etcd timeouts, so the case fails when the push
// waits on either of them instead of its own.
func TestScenMigrationBitmapPushTimesOut(t *testing.T) {
	t.Parallel()
	tmo := harnessTimeouts()
	tmo.push = 2*tmo.syncup + time.Second
	h := newWorkerHarness(t, harnessOpts{tmo: &tmo})
	hold := newCallHold(agentfake.MethodPushMigrBitmap)
	c := h.plantScen(t, scenShape{
		dnCfg: map[uint64]agentfake.Config{3: hold.config()},
	})
	defer hold.open()
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	ctx := context.Background()

	migrId, dstSide := c.migrIds(t)
	dn3 := c.dnAgent(3)
	slowZeroing(t, dn3, agentfake.SideKey(stdSpId, 6, dstSide), true)
	m := c.putStdMigr(t, migrId, dstSide)
	settled := func(rev uint64) func(logRecord) bool {
		return func(rec logRecord) bool {
			id, ok := sideIdOf(rec)
			return ok && id == dstSide && rec.num("code") == 0 &&
				rec.str("error") == "" && rec.num("revision") >= rev
		}
	}
	h.waitRecord(t, 20*time.Second, w, msgSyncupResult, settled(m.SpRev))
	bm, err := plant.PutMigrBitmap(ctx, h.cli, c.cl, c.sp.Ref, plant.MigrBitmapSpec{
		Migr: stdMigrName, BmIdx: 0, Data: []byte{0xff},
	})
	if err != nil {
		t.Fatalf("PutMigrBitmap: %v", err)
	}
	h.waitUntil(t, 20*time.Second, "the push of chunk 0 to reach dn 3",
		func() (bool, string) {
			return len(hold.arrivals()) == 1, fmt.Sprintf("%d pushes", len(hold.arrivals()))
		})
	sentAt := hold.arrivals()[0]
	failed := h.waitRecord(t, 3*h.tmo.push+10*time.Second, w, msgBitmapPushFailed,
		func(rec logRecord) bool {
			return rec.str("kind") == bmKindMigr && rec.num("migr_id") == migrId &&
				rec.num("bm_idx") == 0 && rec.str("error") != ""
		})
	took := failed.at.Sub(sentAt)
	t.Logf("the held push failed %v after it reached the fake: %s",
		took.Round(time.Millisecond), failed.str("error"))
	// took counts from the push's arrival at the fake, and the worker's
	// deadline starts before it, so a stall between the two shortens took.
	// The lower bound sits halfway between the case's other timeouts and its
	// push timeout: a push that waits on either of the others still fails
	// it, and a stall of up to half the gap does not. The upper bound only
	// has to stay far below the production timeout; a loaded machine may
	// delay the failure's record by seconds.
	other := max(h.tmo.syncup, h.tmo.etcdOp)
	lo := other + (h.tmo.push-other)/2
	if took < lo || took > h.tmo.push+10*time.Second {
		t.Errorf("the held push failed %v after it reached the fake, want about "+
			"the case's push timeout %v (at least %v)", took, h.tmo.push, lo)
	}

	// Two rounds of the destination, which answer at its revision and so
	// bring no syncup: nothing re-plans the push (BM6).
	h.waitRounds(t, dn3, 2, m.dstKey)
	if n := len(hold.arrivals()); n != 1 {
		t.Fatalf("%d pushes after the failure with no syncup reply, want "+
			"the one", n)
	}
	if n := h.countRecs(w, msgSyncupResult, settled(bm.SpRev+1)); n != 0 {
		t.Fatalf("a syncup of the destination after the push's own: the "+
			"case cannot tell a re-plan from a re-send (%d)", n)
	}

	hold.open()
	if _, err := plant.BumpRev(ctx, h.cli, c.cl, plant.RevSp, stdSpShard, stdSpId); err != nil {
		t.Fatalf("BumpRev: %v", err)
	}
	h.waitRecord(t, 20*time.Second, w, msgBitmapPushed, func(rec logRecord) bool {
		return rec.str("kind") == bmKindMigr && rec.num("migr_id") == migrId &&
			rec.num("bm_idx") == 0 && rec.num("code") == 0
	})
	if n := len(dn3.Record(m.dstKey).Pushes); n != 1 {
		t.Errorf("dn 3 applied %d pushes of the destination, want 1", n)
	}
}
