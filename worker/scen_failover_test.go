package worker

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/integtest/plant"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The failover and takeover scenarios of the whole worker (dnv-worker.md,
// Integration test plan): each step of a failover played with a silence, a
// refusal or an error at that step, and the worker that owns the pool
// crashed at each moment of a failover, a cutover and a leg repair, with the
// worker that takes the pool's shard over finishing the work. A fake's lever
// holds each moment open, so no case sleeps. The harness and its API are in
// harness_test.go. The helpers of this file carry the prefix fo, so they
// stay apart from those of the package's other scenario files.

// foRoundFailed is the record of a round that failed (RW4 step 4):
// no reply in time, a broken stream, or an observation too old (HL7).
const foRoundFailed = "check round failed"

// foLongCntlrInterval is the cntlr interval, in seconds, of the cases that
// act inside the second wait of the hold (RW14), which lasts one cntlr
// interval: a second is too short a window to act in on a loaded machine.
const foLongCntlrInterval = 3

// foLongCntlrThreshold keeps cntlr replacement (AR7) out of a case's way: a
// cntlr whose controller node is down for the whole case is never due for
// replacement, while the primary still fails over at two seconds.
func foLongCntlrThreshold() *pb.EventThreshold {
	return &pb.EventThreshold{
		PrimaryUnhealthy: 2,
		CntlrUnhealthy:   60,
		SideUnhealthy:    1,
		LegUnhealthy:     2,
	}
}

// ---------------------------------------------------------------------------
// Levers and setters
// ---------------------------------------------------------------------------

// foLever changes one object's levers on one fake, failing the case on a
// refusal.
func foLever(t *testing.T, agent *agentfake.Agent, key string, fn func(*agentfake.Object)) {
	t.Helper()
	if err := agent.Update(key, fn); err != nil {
		t.Fatalf("lever on %s: %v", key, err)
	}
}

// foSetRow sets one row lever of an object.
func foSetRow(o *agentfake.Object, rowKey string, row agentfake.Row) {
	if o.Rows == nil {
		o.Rows = make(map[string]agentfake.Row)
	}
	o.Rows[rowKey] = row
}

// foStatusRow is a row lever that reads status; whenPrimary gates it on the
// cntlr's role.
func foStatusRow(status pb.ResStatus, whenPrimary bool) agentfake.Row {
	return agentfake.Row{Status: &status, WhenPrimary: whenPrimary}
}

// foDataGrpRow is the md row of the standard pool's data group, a row a cntlr
// is judged by (HL2) and of its own class.
func foDataGrpRow() string {
	return fmt.Sprintf("grp_id_to_md_raid.%d", stdDataGrpId)
}

// foFailPrimaryByRow makes cntlr 1's agent report its data group ERROR while it
// is the primary, a fault of its own (HL2), so the worker fails it over on
// its threshold and two unhealthy rounds (AR5, AR10). Once demoted, the row
// no longer bites and its rounds read clean.
func foFailPrimaryByRow(t *testing.T, c *stdCluster) {
	t.Helper()
	agent, key := c.cntlrAgent(t, 1)
	foLever(t, agent, key, func(o *agentfake.Object) {
		foSetRow(o, foDataGrpRow(), foStatusRow(pb.ResStatus_RES_STATUS_ERROR, true))
	})
}

// foSetDisabled writes a cntlr's disabled flag with one bump of the pool's
// revision through plant.SetCntlr; unlike the gateway's UpdateCntlrEnabled
// it leaves the discovery records as they are.
func foSetDisabled(t *testing.T, c *stdCluster, cntlrId uint64, disabled bool) {
	t.Helper()
	if _, _, err := plant.SetCntlr(context.Background(), c.h.cli, c.cl, c.sp.Ref,
		cntlrId, plant.CntlrFlags{Disabled: &disabled}); err != nil {
		t.Fatalf("disabled %t on cntlr %d: %v", disabled, cntlrId, err)
	}
}

// foSetCntlrInterval rewrites the cluster's cntlr interval before any worker
// starts, the conf a cluster planted with that interval would hold.
func foSetCntlrInterval(t *testing.T, c *stdCluster, seconds uint32) {
	t.Helper()
	key := model.ClusterConfKey(c.cl.Name)
	conf := &pb.ClusterConf{}
	c.mustGet(t, key, conf)
	conf.HealthCheckConf.CntlrInterval = seconds
	if err := c.h.cli.Put(context.Background(), key, conf); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// ---------------------------------------------------------------------------
// Records
// ---------------------------------------------------------------------------

// foCntlrIdOf is the cntlr_id of a cntlr child's record, read from its
// cntlr_pointer.
func foCntlrIdOf(rec logRecord) (uint64, bool) {
	v, ok := rec.attrs["cntlr_pointer"]
	if !ok || v.Kind() != slog.KindAny {
		return 0, false
	}
	ptr, ok := v.Any().(map[string]any)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseUint(fmt.Sprint(ptr["cntlr_id"]), 10, 64)
	return id, err == nil
}

// foIsSide and foIsCntlr select the records of one side child or one cntlr
// child.
func foIsSide(sideId uint64) func(logRecord) bool {
	return func(rec logRecord) bool {
		id, ok := sideIdOf(rec)
		return ok && id == sideId
	}
}

func foIsCntlr(cntlrId uint64) func(logRecord) bool {
	return func(rec logRecord) bool {
		id, ok := foCntlrIdOf(rec)
		return ok && id == cntlrId
	}
}

// foAccepted reports a "syncup result" record of an accepted reply: no
// error, and code zero or the leftover code (RW5).
func foAccepted(rec logRecord) bool {
	code := rec.num("code")
	return rec.str("error") == "" &&
		(code == 0 || code == uint64(common.ReplyCodeLeftover))
}

// foSyncedAt is w's first accepted "syncup result" record of the object pick
// selects, at revision rev or newer.
func foSyncedAt(
	h *workerHarness,
	w *harnessWorker,
	rev uint64,
	pick func(logRecord) bool,
) (logRecord, bool) {
	for _, rec := range h.records(w, msgSyncupResult) {
		if rec.num("revision") >= rev && foAccepted(rec) && pick(rec) {
			return rec, true
		}
	}
	return logRecord{}, false
}

// foWaitSidesSynced waits for w's accepted syncup of each side at rev or newer
// and returns the records, by side_id.
func foWaitSidesSynced(
	t *testing.T,
	h *workerHarness,
	w *harnessWorker,
	rev uint64,
	sideIds ...uint64,
) map[uint64]logRecord {
	t.Helper()
	found := make(map[uint64]logRecord)
	h.waitUntil(t, 20*time.Second, fmt.Sprintf("the syncups of sides %v at "+
		"revision %d", sideIds, rev), func() (bool, string) {
		for _, id := range sideIds {
			rec, ok := foSyncedAt(h, w, rev, foIsSide(id))
			if !ok {
				return false, fmt.Sprintf("none of side %d yet", id)
			}
			found[id] = rec
		}
		return true, ""
	})
	return found
}

// foWaitCntlrSynced waits for w's accepted syncup of a cntlr at rev or newer.
func foWaitCntlrSynced(
	t *testing.T,
	h *workerHarness,
	w *harnessWorker,
	cntlrId uint64,
	rev uint64,
	timeout time.Duration,
) logRecord {
	t.Helper()
	var found logRecord
	h.waitUntil(t, timeout, fmt.Sprintf("the accepted syncup of cntlr %d at "+
		"revision %d", cntlrId, rev), func() (bool, string) {
		rec, ok := foSyncedAt(h, w, rev, foIsCntlr(cntlrId))
		found = rec
		return ok, "none yet"
	})
	return found
}

// foWaitFailover waits for w's (nil: any worker's) "reaction applied" record of
// the failover from cntlr from to cntlr to.
func foWaitFailover(
	t *testing.T,
	h *workerHarness,
	w *harnessWorker,
	from, to uint64,
	timeout time.Duration,
) logRecord {
	t.Helper()
	return h.waitRecord(t, timeout, w, msgReactionApplied,
		func(rec logRecord) bool {
			return rec.str("kind") == reactionFailover &&
				rec.num("old_cntlr_id") == from && rec.num("new_cntlr_id") == to
		})
}

// foReactions are w's (nil: every worker's) "reaction applied" records of
// one kind.
func foReactions(h *workerHarness, w *harnessWorker, kind string) []logRecord {
	var out []logRecord
	for _, rec := range h.records(w, msgReactionApplied) {
		if rec.str("kind") == kind {
			out = append(out, rec)
		}
	}
	return out
}

// foRecordsAt are w's records of msg at one revision.
func foRecordsAt(h *workerHarness, w *harnessWorker, msg string, rev uint64) []logRecord {
	var out []logRecord
	for _, rec := range h.records(w, msg) {
		if rec.num("revision") == rev {
			out = append(out, rec)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// What the fakes received, and the discovery record
// ---------------------------------------------------------------------------

// foSyncupArrived reports whether a fake received a Syncup* of the object at
// rev or newer; primaryOnly takes only a SyncupCntlr that makes the cntlr the
// primary.
func foSyncupArrived(agent *agentfake.Agent, key string, rev uint64, primaryOnly bool) bool {
	for _, msg := range agent.Record(key).Syncups {
		switch req := msg.(type) {
		case *pb.SyncupSideRequest:
			if req.GetRevision() >= rev && !primaryOnly {
				return true
			}
		case *pb.SyncupCntlrRequest:
			if req.GetRevision() >= rev &&
				(!primaryOnly || req.GetCntlr().GetPrimary()) {
				return true
			}
		}
	}
	return false
}

// foListedByRule is the discovery record the listing rule gives the pool
// (architecture.md [D18]): in cntlr_id_list order, the transport of every
// enabled cntlr that is the primary or has no health epoch.
func foListedByRule(state *model.SpState) []*pb.NvmeTrConf {
	var out []*pb.NvmeTrConf
	for _, id := range state.Conf.GetCntlrIdList() {
		cntlr := state.Cntlrs[id]
		if !cntlr.GetDisabled() && (cntlr.GetPrimary() || cntlr.GetErrEpoch() == 0) {
			out = append(out, cntlr.GetNvmeTrConf())
		}
	}
	return out
}

// foCdcListsOnly reports whether the discovery record lists exactly the
// transports of the cntlrs ids, in that order, and whether that is what the
// listing rule gives the pool as it is stored now.
func foCdcListsOnly(t *testing.T, c *stdCluster, ids ...uint64) (bool, string) {
	t.Helper()
	state := c.load(t)
	var want []*pb.NvmeTrConf
	for _, id := range ids {
		cntlr, ok := state.Cntlrs[id]
		if !ok {
			return false, fmt.Sprintf("no cntlr %d in the pool", id)
		}
		want = append(want, cntlr.GetNvmeTrConf())
	}
	got := c.cdcEntry(t).GetNvmeTrConfList()
	if !trConfsEqual(got, want) {
		return false, fmt.Sprintf("the discovery record lists %v, want cntlrs %v",
			got, ids)
	}
	if rule := foListedByRule(state); !trConfsEqual(got, rule) {
		return false, fmt.Sprintf("the discovery record lists %v, the rule "+
			"gives %v", got, rule)
	}
	return true, ""
}

// foWaitConvergedNow waits until the cluster reads converged once
// (convergedNow), without the two later rounds waitConverged also waits for,
// which take too long at foLongCntlrInterval.
func foWaitConvergedNow(t *testing.T, c *stdCluster) {
	t.Helper()
	c.h.waitUntil(t, 40*time.Second, "the cluster to converge",
		func() (bool, string) {
			ok, why, _ := c.convergedNow(t)
			return ok, why
		})
}

// ---------------------------------------------------------------------------
// Two workers
// ---------------------------------------------------------------------------

// foStartPair starts two whole workers on every role and waits until they split
// every role's shards between them. It returns the worker that owns the
// pool's shard (VW9's ticket rule, read against its own records) and the
// other one, which takes the shard over when the first crashes.
func foStartPair(t *testing.T, h *workerHarness, c *stdCluster) (owner, peer *harnessWorker) {
	t.Helper()
	w1 := h.startWorker(t, "w1")
	w2 := h.startWorker(t, "w2")
	h.waitUntil(t, 30*time.Second, "w1 and w2 to split every shard",
		func() (bool, string) {
			for _, role := range w1.roles {
				n1, n2 := w1.ownedCount(role), w2.ownedCount(role)
				if n1 == 0 || n2 == 0 || n1+n2 != common.ShardBucketSize {
					return false, fmt.Sprintf("%s: w1 %d, w2 %d", role, n1, n2)
				}
			}
			return true, ""
		})
	owner = h.ticketOwner(common.WorkerRoleSp, stdSpShard, w1, w2)
	if got := c.spOwner(); got != owner {
		t.Fatalf("the pool's shard is owned by %v, the tickets give %s", got, owner.name)
	}
	peer = w1
	if owner == w1 {
		peer = w2
	}
	return owner, peer
}

// foWaitTakesPool waits until w owns the pool's shard by a "shard owned" record
// later than since, and returns that record's time.
func foWaitTakesPool(t *testing.T, h *workerHarness, w *harnessWorker, since time.Time) time.Time {
	t.Helper()
	var at time.Time
	h.waitUntil(t, 20*time.Second, fmt.Sprintf("%s to take the pool's shard", w.name),
		func() (bool, string) {
			for _, rec := range h.records(w, msgShardOwned) {
				shard, err := strconv.ParseUint(rec.str("shard"), 16, 32)
				if err == nil && rec.str("role") == common.WorkerRoleSp &&
					uint32(shard) == stdSpShard && rec.at.After(since) {
					at = rec.at
					return true, ""
				}
			}
			return false, "no shard owned record yet"
		})
	return at
}

// ---------------------------------------------------------------------------
// Failover steps
// ---------------------------------------------------------------------------

// TestFailoverDemotionUnansweredHoldsToTheTimer pins the first wait of the
// hold (RW22) when the old primary cannot answer: its controller node dies,
// the worker fails it over on its threshold and two missed rounds (AR5,
// AR10), the demotion it hands the old primary fails at once, nothing reports
// it applied, and the first wait runs to its timer, logged "sp demotion
// unsynced" naming the old primary. Only then are the sides told the
// failover's revision, and only after every side the new primary (RW14). The
// discovery record lists the new primary alone: the old one is an enabled
// standby with a health epoch (architecture.md [D18]).
func TestFailoverDemotionUnansweredHoldsToTheTimer(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, func(s *stdSpec) { s.threshold = foLongCntlrThreshold() })
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	c.cnAgent(1).Stop()
	applied := foWaitFailover(t, h, w, 1, 2, 20*time.Second)
	failRev := applied.num("revision")
	unsynced := h.waitRecord(t, 10*time.Second, w, msgSpDemotionUnsynced,
		func(rec logRecord) bool { return rec.num("revision") == failRev })
	if ids := unsynced.str("cntlr_ids"); ids != "[1]" {
		t.Errorf("the first wait ended naming cntlrs %s, want the old primary [1]", ids)
	}
	// The wait starts when the coordinator takes the failover's revision, a
	// moment after the commit, and only its timer ends it.
	took := unsynced.at.Sub(applied.at)
	t.Logf("the first wait ended %v after the failover's commit",
		took.Round(time.Millisecond))
	if took < h.tmo.demotionHold-250*time.Millisecond {
		t.Errorf("the first wait ended %v after the commit, want its whole "+
			"timer of %v", took, h.tmo.demotionHold)
	}
	demotionFailed := false
	for _, rec := range h.records(w, msgSyncupResult) {
		if foIsCntlr(1)(rec) && rec.num("revision") >= failRev &&
			rec.str("error") != "" && rec.at.Before(unsynced.at) {
			demotionFailed = true
		}
	}
	if !demotionFailed {
		t.Errorf("no failed demotion of cntlr 1 at revision %d before the "+
			"first wait ended", failRev)
	}

	var sideIds []uint64
	for _, side := range stdSides {
		sideIds = append(sideIds, side.sideId)
	}
	var lastSide time.Time
	for id, rec := range foWaitSidesSynced(t, h, w, failRev, sideIds...) {
		if rec.at.Before(unsynced.at) {
			t.Errorf("side %d was told revision %d %v before the first wait "+
				"ended", id, rec.num("revision"),
				unsynced.at.Sub(rec.at).Round(time.Millisecond))
		}
		if rec.at.After(lastSide) {
			lastSide = rec.at
		}
	}
	promoted := foWaitCntlrSynced(t, h, w, 2, failRev, 10*time.Second)
	if !promoted.at.After(lastSide) {
		t.Errorf("the new primary's promotion was answered %v before the "+
			"last side's syncup", lastSide.Sub(promoted.at).Round(time.Millisecond))
	}
	h.waitUntil(t, 10*time.Second, "the discovery record to list the new "+
		"primary alone", func() (bool, string) {
		if c.cntlr(t, 1).GetErrEpoch() == 0 {
			return false, "cntlr 1 has no health epoch"
		}
		return foCdcListsOnly(t, c, 2)
	})
}

// TestFailoverDemotionRefusedHoldsToTheTimer pins the first wait of the hold
// (RW22) when the old primary refuses its demotion: a refused reply reports
// nothing applied (RW5), so the first wait runs to its timer and names the
// old primary, and the sides are told the failover's revision only then.
// Once the agent accepts again, the old primary takes its demotion and the
// pool converges.
func TestFailoverDemotionRefusedHoldsToTheTimer(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	oldAgent, oldKey := c.cntlrAgent(t, 1)
	foLever(t, oldAgent, oldKey, func(o *agentfake.Object) {
		o.ReplyCode = common.ReplyCodeInvalidConf
	})

	// The refused rounds give no verdict (HL1), so the disable is what
	// fails the primary over (AR5).
	foSetDisabled(t, c, 1, true)
	applied := foWaitFailover(t, h, w, 1, 2, 20*time.Second)
	failRev := applied.num("revision")
	unsynced := h.waitRecord(t, 10*time.Second, w, msgSpDemotionUnsynced,
		func(rec logRecord) bool { return rec.num("revision") == failRev })
	if ids := unsynced.str("cntlr_ids"); ids != "[1]" {
		t.Errorf("the first wait ended naming cntlrs %s, want the old primary [1]", ids)
	}
	if took := unsynced.at.Sub(applied.at); took < h.tmo.demotionHold-250*time.Millisecond {
		t.Errorf("the first wait ended %v after the commit, want its whole "+
			"timer of %v", took, h.tmo.demotionHold)
	}
	refused := false
	for _, rec := range h.records(w, msgSyncupRejected) {
		if foIsCntlr(1)(rec) && rec.num("revision") >= failRev &&
			rec.num("code") == uint64(common.ReplyCodeInvalidConf) &&
			rec.at.Before(unsynced.at) {
			refused = true
		}
	}
	if !refused {
		t.Errorf("no refused demotion of cntlr 1 at revision %d before the "+
			"first wait ended", failRev)
	}
	var sideIds []uint64
	for _, side := range stdSides {
		sideIds = append(sideIds, side.sideId)
	}
	for id, rec := range foWaitSidesSynced(t, h, w, failRev, sideIds...) {
		if rec.at.Before(unsynced.at) {
			t.Errorf("side %d was told revision %d before the first wait ended",
				id, rec.num("revision"))
		}
	}

	foLever(t, oldAgent, oldKey, func(o *agentfake.Object) { o.ReplyCode = 0 })
	c.waitConverged(t)
}

// TestFailoverSideSilentUntilTheSecondWaitEnds pins the second wait of the
// hold (RW14) when one side never answers the failover's revision: the old
// primary reports its demotion, which ends the first wait at once (RW22); the
// sides are told the new revision and three of them answer; the fourth's
// syncup is held, so the new primary is handed its promotion only when the
// second wait's timer runs out, one cntlr interval after it began, logged "sp
// sides unsynced" with the one side that did not answer. The cluster's cntlr
// interval, the wait's bound, is long enough for the three other sides to
// answer inside it on a loaded machine.
func TestFailoverSideSilentUntilTheSecondWaitEnds(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	foSetCntlrInterval(t, c, foLongCntlrInterval)
	interval := foLongCntlrInterval * time.Second
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	foWaitConvergedNow(t, c)
	silent, silentKey := c.sideAgent(t, 8)
	foLever(t, silent, silentKey, func(o *agentfake.Object) { o.HangSyncup = true })

	foFailPrimaryByRow(t, c)
	applied := foWaitFailover(t, h, w, 1, 2, 40*time.Second)
	failRev := applied.num("revision")
	demoted := foWaitCntlrSynced(t, h, w, 1, failRev, 10*time.Second)
	unsynced := h.waitRecord(t, 10*time.Second, w, msgSpSidesUnsynced,
		func(rec logRecord) bool { return rec.num("revision") == failRev })
	// The timer's end is a unit of work of the pool's coordinator, so it
	// runs under a trace id of its own, not under the coordinator's
	// lifetime id that its start record carries (RW10).
	started := h.waitRecord(t, 10*time.Second, w, msgRevisionWorkerStarted,
		func(rec logRecord) bool {
			return rec.str("role") == common.WorkerRoleSp &&
				rec.num("cluster_id") == c.cl.Id && rec.num("id") == stdSpId &&
				rec.str("side_pointer") == "" && rec.str("cntlr_pointer") == ""
		})
	if unsynced.traceId == started.traceId {
		t.Errorf("the second wait's end was logged under the coordinator's "+
			"own trace id %q, want an id of its own", started.traceId)
	}
	if n := unsynced.num("side_cnt"); n != 1 {
		t.Errorf("the second wait ended with %d sides unsynced, want the "+
			"silent one", n)
	}
	if recs := foRecordsAt(h, w, msgSpDemotionUnsynced, failRev); len(recs) != 0 {
		t.Errorf("the first wait ran to its timer, though the old primary "+
			"reported its demotion: %v", recs[0].attrs)
	}
	// The second wait began once the demotion's report reached the
	// coordinator, after its record, and only its timer ended it.
	if took := unsynced.at.Sub(demoted.at); took < interval-100*time.Millisecond {
		t.Errorf("the second wait ended %v after the demotion was answered, "+
			"want its whole timer of %v", took, interval)
	}
	for id, rec := range foWaitSidesSynced(t, h, w, failRev, 9, 10, 11) {
		if !rec.at.Before(unsynced.at) {
			t.Errorf("side %d answered only after the second wait ended", id)
		}
	}
	if rec, ok := foSyncedAt(h, w, failRev, foIsSide(8)); ok {
		t.Errorf("the silent side 8 answered revision %d", rec.num("revision"))
	}
	promoted := foWaitCntlrSynced(t, h, w, 2, failRev, 10*time.Second)
	if promoted.at.Before(unsynced.at) {
		t.Errorf("the new primary's promotion was answered %v before the "+
			"second wait ended", unsynced.at.Sub(promoted.at).Round(time.Millisecond))
	}

	foLever(t, silent, silentKey, func(o *agentfake.Object) { o.HangSyncup = false })
	foWaitConvergedNow(t, c)
}

// TestFailoverSideDyingInTheSecondWaitIsStillAwaited pins the set of sides
// the second wait of the hold waits for (RW14): it is taken when the wait
// begins, so a side that was healthy then and whose disk node dies during
// the wait stays in it, though its health epoch is set meanwhile (HL2), and
// the new primary is handed its promotion only at the wait's timer. The
// cluster's cntlr interval, the wait's bound, is long enough for the side's
// epoch to land inside the wait.
func TestFailoverSideDyingInTheSecondWaitIsStillAwaited(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	foSetCntlrInterval(t, c, foLongCntlrInterval)
	interval := foLongCntlrInterval * time.Second
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	foWaitConvergedNow(t, c)
	dying, dyingKey := c.sideAgent(t, 8)
	foLever(t, dying, dyingKey, func(o *agentfake.Object) { o.HangSyncup = true })

	foFailPrimaryByRow(t, c)
	applied := foWaitFailover(t, h, w, 1, 2, 40*time.Second)
	failRev := applied.num("revision")
	demoted := foWaitCntlrSynced(t, h, w, 1, failRev, 10*time.Second)
	foWaitSidesSynced(t, h, w, failRev, 9, 10, 11)
	h.waitUntil(t, 10*time.Second, "side 8's held syncup at the failover's "+
		"revision", func() (bool, string) {
		return foSyncupArrived(dying, dyingKey, failRev, false), "none yet"
	})
	if recs := foRecordsAt(h, w, msgSpSidesUnsynced, failRev); len(recs) != 0 {
		t.Fatalf("the second wait ended before side 8's disk node died")
	}
	c.dnAgent(1).Stop()

	var epochAt time.Time
	h.waitUntil(t, 2*interval, "side 8's health epoch", func() (bool, string) {
		for _, slice := range c.load(t).Slices {
			for _, grp := range slice.GetMetaGrpList() {
				for _, leg := range grp.GetLegList() {
					for _, side := range leg.GetSideList() {
						if side.GetSideId() == 8 && side.GetErrEpoch() != 0 {
							epochAt = time.Now()
							return true, ""
						}
					}
				}
			}
		}
		return false, "no epoch yet"
	})
	unsynced := h.waitRecord(t, 2*interval, w, msgSpSidesUnsynced,
		func(rec logRecord) bool { return rec.num("revision") == failRev })
	if !epochAt.Before(unsynced.at) {
		t.Fatalf("side 8's health epoch came %v after the second wait ended: "+
			"the side did not die inside the wait",
			epochAt.Sub(unsynced.at).Round(time.Millisecond))
	}
	t.Logf("side 8's epoch came %v before the second wait ended",
		unsynced.at.Sub(epochAt).Round(time.Millisecond))
	if n := unsynced.num("side_cnt"); n != 1 {
		t.Errorf("the second wait ended with %d sides unsynced, want the dead "+
			"side 8", n)
	}
	if took := unsynced.at.Sub(demoted.at); took < interval-100*time.Millisecond {
		t.Errorf("the second wait ended %v after the demotion was answered, "+
			"want its whole timer of %v: the dead side was let go", took, interval)
	}
	promoted := foWaitCntlrSynced(t, h, w, 2, failRev, 10*time.Second)
	if promoted.at.Before(unsynced.at) {
		t.Errorf("the new primary's promotion was answered %v before the "+
			"second wait ended", unsynced.at.Sub(promoted.at).Round(time.Millisecond))
	}

	foLever(t, dying, dyingKey, func(o *agentfake.Object) { o.HangSyncup = false })
	if err := c.dnAgent(1).Start(agentfake.KeepStore); err != nil {
		t.Fatalf("restart dn 1: %v", err)
	}
	foWaitConvergedNow(t, c)
}

// TestFailoverBackWaitsWhileTheNewPrimarySettles pins AR5's hold on a
// settling primary: the new primary reports an error row from its promotion
// on, so it never reports its stack built and clean (HL2) and stays
// settling; with the old primary enabled again as a candidate, the failover
// back to it waits cntlr_unhealthy from the new primary's health epoch, not
// the shorter primary_unhealthy. The old primary, promoted again, settles.
func TestFailoverBackWaitsWhileTheNewPrimarySettles(t *testing.T) {
	t.Parallel()
	const primaryTh, cntlrTh = 2, 6
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, func(s *stdSpec) {
		s.threshold = &pb.EventThreshold{
			PrimaryUnhealthy: primaryTh,
			CntlrUnhealthy:   cntlrTh,
			SideUnhealthy:    1,
			LegUnhealthy:     2,
		}
	})
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	newAgent, newKey := c.cntlrAgent(t, 2)
	foLever(t, newAgent, newKey, func(o *agentfake.Object) {
		foSetRow(o, foDataGrpRow(), foStatusRow(pb.ResStatus_RES_STATUS_ERROR, true))
	})

	foSetDisabled(t, c, 1, true)
	foWaitFailover(t, h, w, 1, 2, 20*time.Second)
	foSetDisabled(t, c, 1, false)
	var epoch uint64
	h.waitUntil(t, 10*time.Second, "the new primary's health epoch",
		func() (bool, string) {
			epoch = c.cntlr(t, 2).GetErrEpoch()
			return epoch != 0, "none yet"
		})
	var back logRecord
	h.waitUntil(t, 30*time.Second, "the failover back to cntlr 1",
		func() (bool, string) {
			for _, rec := range foReactions(h, w, reactionFailover) {
				if rec.num("old_cntlr_id") == 2 && rec.num("new_cntlr_id") == 1 {
					back = rec
					return true, ""
				}
			}
			if cntlr := c.cntlr(t, 2); cntlr.GetPrimary() && !cntlr.GetSettling() {
				t.Fatalf("cntlr 2 settled although its every report has an " +
					"error row")
			}
			// The worker holds the failover back itself: one it tried early
			// would be refused by model.Failover's own check (MD6) and
			// logged as skipped.
			for _, rec := range h.records(w, msgReactionSkipped) {
				if rec.str("kind") == reactionFailover {
					t.Fatalf("a failover of the settling primary was tried and "+
						"skipped: %s", rec.str("reason"))
				}
			}
			return false, "cntlr 2 still primary"
		})
	t.Logf("the failover back came %ds after the new primary's epoch",
		back.at.Unix()-int64(epoch))
	// AR4: reached when now minus the epoch is at least the threshold, both
	// in whole seconds; the record comes after the pass's now.
	if back.at.Unix() < int64(epoch)+cntlrTh {
		t.Errorf("the failover back came %ds after the new primary's epoch, "+
			"want at least cntlr_unhealthy %ds: a settling primary is held to "+
			"the longer threshold", back.at.Unix()-int64(epoch), cntlrTh)
	}
	h.waitRecord(t, 10*time.Second, w, msgCntlrSettled, func(rec logRecord) bool {
		return foIsCntlr(1)(rec) && rec.at.After(back.at)
	})
}

// TestFailoverNewPrimaryRefusingItsSyncup pins what a refused promotion
// costs: the new primary's agent refuses every reply with a code, a refusal
// gives no health verdict (HL1, HL2) and adds to no run (AR10), so the new
// primary keeps the role with no health epoch, stays settling and stays in
// the discovery record (architecture.md [D18]); the promotion is sent again
// from every round (RW5) and no failover moves the role back, though the old
// primary is a candidate again. Once the agent accepts, the new primary
// settles.
func TestFailoverNewPrimaryRefusingItsSyncup(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	newAgent, newKey := c.cntlrAgent(t, 2)
	foLever(t, newAgent, newKey, func(o *agentfake.Object) {
		o.ReplyCode = common.ReplyCodeInvalidConf
	})

	foSetDisabled(t, c, 1, true)
	applied := foWaitFailover(t, h, w, 1, 2, 20*time.Second)
	failRev := applied.num("revision")
	foSetDisabled(t, c, 1, false)
	// Six refusals take about six rounds, past the threshold of two seconds
	// and the two rounds a failover back would need if a refusal counted.
	const refusals = 6
	h.waitUntil(t, 30*time.Second, fmt.Sprintf("%d refusals of the "+
		"promotion", refusals), func() (bool, string) {
		cntlr := c.cntlr(t, 2)
		if !cntlr.GetPrimary() || cntlr.GetErrEpoch() != 0 || !cntlr.GetSettling() {
			t.Fatalf("cntlr 2 while it refuses: primary %t, err_epoch %d, "+
				"settling %t; want primary, no epoch, settling",
				cntlr.GetPrimary(), cntlr.GetErrEpoch(), cntlr.GetSettling())
		}
		if n := len(foReactions(h, w, reactionFailover)); n != 1 {
			t.Fatalf("%d failovers applied, want only the first", n)
		}
		n := 0
		for _, rec := range h.records(w, msgSyncupRejected) {
			if foIsCntlr(2)(rec) && rec.num("revision") >= failRev {
				n++
			}
		}
		return n >= refusals, fmt.Sprintf("%d so far", n)
	})
	// The enable above is planted, and only the gateway's UpdateCntlrEnabled
	// rewrites the discovery record, so the record is read for the refusing
	// primary alone.
	got := c.cdcEntry(t).GetNvmeTrConfList()
	if !slices.ContainsFunc(got, func(conf *pb.NvmeTrConf) bool {
		return proto.Equal(conf, c.cntlr(t, 2).GetNvmeTrConf())
	}) {
		t.Errorf("the discovery record lists %v, without the refusing primary", got)
	}

	foLever(t, newAgent, newKey, func(o *agentfake.Object) { o.ReplyCode = 0 })
	h.waitRecord(t, 10*time.Second, w, msgCntlrSettled, foIsCntlr(2))
	c.waitConverged(t)
}

// TestFailoverEveryStandbyUnhealthyReplacesThePrimary pins AR7's sole-primary
// variant: the primary fails on its threshold while the one standby has a
// health epoch of its own, so AR5 finds no candidate, logs it and lets the
// pass go on (AR2), and AR7 replaces the primary itself once it has been
// unhealthy for cntlr_unhealthy: a new cntlr on the free controller node,
// created primary and settling (HL2), promoted there and settled. The
// standby's epoch is stamped after the primary's, so the primary is the first
// cntlr due for replacement.
func TestFailoverEveryStandbyUnhealthyReplacesThePrimary(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, func(s *stdSpec) {
		s.threshold = &pb.EventThreshold{
			PrimaryUnhealthy: 4,
			CntlrUnhealthy:   6,
			SideUnhealthy:    1,
			LegUnhealthy:     2,
		}
	})
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	foFailPrimaryByRow(t, c)
	h.waitUntil(t, 10*time.Second, "the primary's health epoch",
		func() (bool, string) { return c.cntlr(t, 1).GetErrEpoch() != 0, "none yet" })
	standby, standbyKey := c.cntlrAgent(t, 2)
	foLever(t, standby, standbyKey, func(o *agentfake.Object) {
		foSetRow(o, foDataGrpRow(), foStatusRow(pb.ResStatus_RES_STATUS_ERROR, false))
	})
	h.waitUntil(t, 10*time.Second, "the standby's health epoch",
		func() (bool, string) { return c.cntlr(t, 2).GetErrEpoch() != 0, "none yet" })
	if n := len(foReactions(h, w, reactionFailover)); n != 0 {
		t.Fatalf("a failover ran before the standby turned unhealthy")
	}

	replaced := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		func(rec logRecord) bool { return rec.str("kind") == reactionReplaceCntlr })
	if rec := replaced; rec.num("old_cntlr_id") != 1 || rec.str("primary") != "true" ||
		rec.str("addr_port") != stdCnAddr(3) {
		t.Fatalf("the replacement: old cntlr %d, primary %s, on %s; want the "+
			"primary cntlr 1 replaced on cn 3", rec.num("old_cntlr_id"),
			rec.str("primary"), rec.str("addr_port"))
	}
	noCandidate := false
	for _, rec := range h.records(w, msgReactionSkipped) {
		if rec.str("kind") == reactionFailover && rec.str("reason") == reasonNoCandidate &&
			rec.num("cntlr_id") == 1 && rec.at.Before(replaced.at) {
			noCandidate = true
		}
	}
	if !noCandidate {
		t.Errorf("no failover skipped for want of a candidate before the replacement")
	}
	newId := replaced.num("new_cntlr_id")
	state := c.load(t)
	if slices.Contains(state.Conf.GetCntlrIdList(), 1) {
		t.Errorf("cntlr 1 is still in the pool after its replacement")
	}
	if cntlr := state.Cntlrs[newId]; !cntlr.GetPrimary() || cntlr.GetAddrPort() != stdCnAddr(3) {
		t.Errorf("the replacement cntlr %d: primary %t on %s, want the primary "+
			"on cn 3", newId, cntlr.GetPrimary(), cntlr.GetAddrPort())
	}
	cn3, key3 := c.cnAgent(3), agentfake.CntlrKey(stdSpId, newId)
	h.waitUntil(t, 10*time.Second, "the replacement's promotion on cn 3",
		func() (bool, string) { return foSyncupArrived(cn3, key3, 0, true), "none yet" })
	h.waitRecord(t, 10*time.Second, w, msgCntlrSettled, foIsCntlr(newId))
	if n := len(foReactions(h, w, reactionFailover)); n != 0 {
		t.Errorf("%d failovers applied, want none: no standby was a candidate", n)
	}
	h.waitUntil(t, 10*time.Second, "the discovery record to list the "+
		"replacement", func() (bool, string) {
		got := c.cdcEntry(t).GetNvmeTrConfList()
		want := c.cntlr(t, newId).GetNvmeTrConf()
		for _, conf := range got {
			if proto.Equal(conf, want) {
				return trConfsEqual(got, foListedByRule(c.load(t))),
					fmt.Sprintf("%v is not what the rule gives", got)
			}
		}
		return false, fmt.Sprintf("%v without the replacement", got)
	})
}

// TestFailoverOldPrimaryReturns pins the old primary's return (HL2): its
// controller node dies, the worker fails it over and it stays an enabled
// standby with a health epoch, out of the discovery record (architecture.md
// [D18]). Its agent comes back on its kept files, so it answers clean at the
// revision before the failover and in the primary's shape; that reply clears
// nothing, and while the agent holds the syncup of the driven revision the
// epoch stays set and the record without it. Once the agent applies that
// syncup, its clean reply clears the epoch and the discovery record lists it
// again.
func TestFailoverOldPrimaryReturns(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, func(s *stdSpec) { s.threshold = foLongCntlrThreshold() })
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	cn1, key1 := c.cnAgent(1), agentfake.CntlrKey(stdSpId, 1)

	cn1.Stop()
	applied := foWaitFailover(t, h, w, 1, 2, 20*time.Second)
	failRev := applied.num("revision")
	foWaitCntlrSynced(t, h, w, 2, failRev, 10*time.Second)
	h.waitUntil(t, 10*time.Second, "the discovery record to drop the old "+
		"primary", func() (bool, string) {
		if c.cntlr(t, 1).GetErrEpoch() == 0 {
			return false, "cntlr 1 has no health epoch"
		}
		return foCdcListsOnly(t, c, 2)
	})

	foLever(t, cn1, key1, func(o *agentfake.Object) { o.HangSyncup = true })
	before := cn1.Record(key1)
	if err := cn1.Start(agentfake.KeepStore); err != nil {
		t.Fatalf("restart cn 1: %v", err)
	}
	// Two rounds answered after the restart and a held syncup: the first
	// round's clean reply at the old revision came and went.
	h.waitUntil(t, 20*time.Second, "two rounds and a held syncup of the "+
		"returned old primary", func() (bool, string) {
		if epoch := c.cntlr(t, 1).GetErrEpoch(); epoch == 0 {
			t.Fatalf("cntlr 1's health epoch cleared while its agent held " +
				"the revision before the failover")
		}
		if ok, why := foCdcListsOnly(t, c, 2); !ok {
			t.Fatalf("while cntlr 1's agent held its old revision: %s", why)
		}
		rec := cn1.Record(key1)
		rounds := len(rec.Rounds) - len(before.Rounds)
		syncups := len(rec.Syncups) - len(before.Syncups)
		return rounds >= 2 && syncups >= 1,
			fmt.Sprintf("%d rounds, %d syncups since the restart", rounds, syncups)
	})

	foLever(t, cn1, key1, func(o *agentfake.Object) { o.HangSyncup = false })
	h.waitUntil(t, 20*time.Second, "the old primary's health epoch to clear",
		func() (bool, string) {
			if c.cntlr(t, 1).GetErrEpoch() != 0 {
				return false, "still set"
			}
			if _, ok := foSyncedAt(h, w, failRev, foIsCntlr(1)); !ok {
				t.Fatalf("cntlr 1's health epoch cleared before it accepted " +
					"the syncup of the driven revision")
			}
			return true, ""
		})
	h.waitUntil(t, 10*time.Second, "the discovery record to list the old "+
		"primary again", func() (bool, string) { return foCdcListsOnly(t, c, 1, 2) })
}

// ---------------------------------------------------------------------------
// Takeovers
// ---------------------------------------------------------------------------

// TestTakeoverBeforeTheFailoverCommit pins AR10 across a takeover: the
// worker that owns the pool sets the failing primary's health epoch and
// crashes before its failover commits. The worker that takes the pool's shard
// over starts with an empty memo of its own verdicts, so though the stored
// epoch is past its threshold it fails the primary over only once two of its
// own rounds in a row read it unhealthy. From the crash on the old primary's
// agent refuses every reply, which gives no verdict (HL1) and neither adds to
// a run nor ends it: refusals alone fail nothing over; one held round, a
// missed round (HL2), is a run of one, which refusals after it keep; and the
// next missed round, the second with no clean answer between, fails the
// primary over. Each quiet stretch lasts three rounds, three passes of the
// reactions.
func TestTakeoverBeforeTheFailoverCommit(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	owner, peer := foStartPair(t, h, c)
	c.waitConverged(t)
	oldAgent, oldKey := c.cntlrAgent(t, 1)

	foFailPrimaryByRow(t, c)
	// The owner needs a second unhealthy round, a second after the epoch,
	// before it can fail over; the crash comes as the epoch lands.
	h.waitUntil(t, 10*time.Second, "the primary's health epoch",
		func() (bool, string) { return c.cntlr(t, 1).GetErrEpoch() != 0, "none yet" })
	crashedAt := time.Now()
	owner.crash(t)
	foLever(t, oldAgent, oldKey, func(o *agentfake.Object) {
		o.ReplyCode = common.ReplyCodeInvalidConf
	})
	if n := len(foReactions(h, owner, reactionFailover)); n != 0 {
		t.Fatalf("the owner failed over before its crash")
	}
	if got := c.primary(t); got != 1 {
		t.Fatalf("the primary is cntlr %d at the crash, want 1", got)
	}

	foWaitTakesPool(t, h, peer, crashedAt)
	quiet := func(what string) {
		t.Helper()
		from := len(oldAgent.Record(oldKey).Rounds)
		h.waitUntil(t, 20*time.Second, "three rounds of the old primary "+what,
			func() (bool, string) {
				if len(foReactions(h, peer, reactionFailover)) != 0 {
					t.Fatalf("the new owner failed the primary over %s", what)
				}
				n := len(oldAgent.Record(oldKey).Rounds) - from
				return n >= 3, fmt.Sprintf("%d so far", n)
			})
	}
	missed := func() []logRecord {
		var out []logRecord
		for _, rec := range h.records(peer, foRoundFailed) {
			if foIsCntlr(1)(rec) {
				out = append(out, rec)
			}
		}
		return out
	}
	quiet("on refused replies alone")
	foLever(t, oldAgent, oldKey, func(o *agentfake.Object) { o.HangRounds = 1 })
	h.waitUntil(t, 10*time.Second, "the held round to be missed",
		func() (bool, string) { return len(missed()) >= 1, "not yet" })
	quiet("after one missed round")

	before := len(missed())
	// A count that differs from the last one arms afresh.
	foLever(t, oldAgent, oldKey, func(o *agentfake.Object) { o.HangRounds = 0 })
	foLever(t, oldAgent, oldKey, func(o *agentfake.Object) { o.HangRounds = 1 })
	applied := foWaitFailover(t, h, peer, 1, 2, 20*time.Second)
	var total, more int
	for i, rec := range missed() {
		if rec.at.Before(applied.at) {
			total++
			if i >= before {
				more++
			}
		}
	}
	t.Logf("the new owner failed over %v after the crash, after %d missed "+
		"rounds of its own", applied.at.Sub(crashedAt).Round(time.Millisecond), total)
	if more < 1 || total < 2 {
		t.Errorf("the new owner failed over after %d missed rounds of its own, "+
			"%d of them after the quiet stretch; want the second missed round "+
			"to fail it over", total, more)
	}
	foWaitCntlrSynced(t, h, peer, 2, applied.num("revision"), 10*time.Second)
}

// TestTakeoverAfterTheCommitInTheFirstWait pins RW22's rule that nothing of
// the hold is persisted: the owner commits a failover and crashes inside its
// first wait, which the old primary's held demotion keeps open, before any
// side was told the failover's revision. The worker that takes the pool's
// shard over has no cntlr child it handed a primary plan, so it runs no
// first wait and fans the failover out in RW14's order: the sides first, and
// the cntlrs, the promotion included, only once its second wait ends, here
// at its timer, since one side's syncup is held as well. The cluster's
// cntlr interval, the bound of the new owner's second wait, is long enough
// for the three other sides to answer inside it on a loaded machine.
func TestTakeoverAfterTheCommitInTheFirstWait(t *testing.T) {
	t.Parallel()
	tmo := harnessTimeouts()
	// A first wait long enough to crash the owner inside it, and longer
	// than the new owner, which runs none, takes to tell the sides; it
	// costs no time, since the crash ends the owner's first wait.
	tmo.demotionHold = 6 * time.Second
	h := newWorkerHarness(t, harnessOpts{tmo: &tmo})
	c := h.plantStd(t, nil)
	foSetCntlrInterval(t, c, foLongCntlrInterval)
	owner, peer := foStartPair(t, h, c)
	foWaitConvergedNow(t, c)
	oldAgent, oldKey := c.cntlrAgent(t, 1)
	foLever(t, oldAgent, oldKey, func(o *agentfake.Object) { o.HangSyncup = true })
	held, heldKey := c.sideAgent(t, 8)
	foLever(t, held, heldKey, func(o *agentfake.Object) { o.HangSyncup = true })

	foFailPrimaryByRow(t, c)
	applied := foWaitFailover(t, h, owner, 1, 2, 40*time.Second)
	failRev := applied.num("revision")
	h.waitUntil(t, 10*time.Second, "the old primary's held demotion",
		func() (bool, string) {
			for _, msg := range oldAgent.Record(oldKey).Syncups {
				req := msg.(*pb.SyncupCntlrRequest)
				if req.GetRevision() >= failRev && !req.GetCntlr().GetPrimary() {
					return true, ""
				}
			}
			return false, "none yet"
		})
	crashedAt := time.Now()
	owner.crash(t)
	for _, side := range stdSides {
		agent, key := c.sideAgent(t, side.sideId)
		if foSyncupArrived(agent, key, failRev, false) {
			t.Fatalf("side %d was told revision %d inside the first wait",
				side.sideId, failRev)
		}
	}
	if recs := foRecordsAt(h, owner, msgSpDemotionUnsynced, failRev); len(recs) != 0 {
		t.Fatalf("the owner's first wait ended before its crash")
	}

	ownedAt := foWaitTakesPool(t, h, peer, crashedAt)
	unsynced := h.waitRecord(t, 20*time.Second, peer, msgSpSidesUnsynced,
		func(rec logRecord) bool { return rec.num("revision") == failRev })
	if n := unsynced.num("side_cnt"); n != 1 {
		t.Errorf("the new owner's second wait ended with %d sides unsynced, "+
			"want the held side 8", n)
	}
	if recs := foRecordsAt(h, peer, msgSpDemotionUnsynced, failRev); len(recs) != 0 {
		t.Errorf("the new owner ran a first wait of its own")
	}
	var sidesTook time.Duration
	for id, rec := range foWaitSidesSynced(t, h, peer, failRev, 9, 10, 11) {
		if !rec.at.Before(unsynced.at) {
			t.Errorf("side %d was told the failover only after the new "+
				"owner's second wait ended", id)
		}
		took := rec.at.Sub(ownedAt)
		if took >= tmo.demotionHold {
			t.Errorf("side %d was told the failover %v after the new owner "+
				"took the pool, a first wait's length", id, took)
		}
		sidesTook = max(sidesTook, took)
	}
	for _, rec := range h.records(peer, msgSyncupResult) {
		if id, ok := foCntlrIdOf(rec); ok && rec.num("revision") >= failRev &&
			rec.at.Before(unsynced.at) {
			t.Errorf("cntlr %d had a syncup at revision %d before the new "+
				"owner's second wait ended", id, rec.num("revision"))
		}
	}
	promoted := foWaitCntlrSynced(t, h, peer, 2, failRev, 10*time.Second)
	t.Logf("the new owner took the pool %v after the crash, told the sides "+
		"within %v and promoted cntlr 2 %v after it took the pool",
		ownedAt.Sub(crashedAt).Round(time.Millisecond),
		sidesTook.Round(time.Millisecond),
		promoted.at.Sub(ownedAt).Round(time.Millisecond))

	foLever(t, oldAgent, oldKey, func(o *agentfake.Object) { o.HangSyncup = false })
	foLever(t, held, heldKey, func(o *agentfake.Object) { o.HangSyncup = false })
	foWaitConvergedNow(t, c)
}

// TestTakeoverInTheSecondWait pins the second wait of the hold across a
// takeover (RW14): the owner's second wait is held open by one side's held
// syncup when the owner crashes, before the new primary was told anything.
// The worker that takes the pool's shard over holds its own first fan-out
// the same way, nothing of the hold being persisted: its second wait runs to
// its timer, and it then promotes the new primary, which settles under it.
func TestTakeoverInTheSecondWait(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	foSetCntlrInterval(t, c, foLongCntlrInterval)
	owner, peer := foStartPair(t, h, c)
	foWaitConvergedNow(t, c)
	held, heldKey := c.sideAgent(t, 8)
	foLever(t, held, heldKey, func(o *agentfake.Object) { o.HangSyncup = true })
	newAgent, newKey := c.cntlrAgent(t, 2)

	foFailPrimaryByRow(t, c)
	applied := foWaitFailover(t, h, owner, 1, 2, 40*time.Second)
	failRev := applied.num("revision")
	foWaitSidesSynced(t, h, owner, failRev, 9, 10, 11)
	h.waitUntil(t, 10*time.Second, "side 8's held syncup at the failover's "+
		"revision", func() (bool, string) {
		return foSyncupArrived(held, heldKey, failRev, false), "none yet"
	})
	crashedAt := time.Now()
	owner.crash(t)
	if recs := foRecordsAt(h, owner, msgSpSidesUnsynced, failRev); len(recs) != 0 {
		t.Fatalf("the owner's second wait ended before its crash")
	}
	if foSyncupArrived(newAgent, newKey, failRev, true) {
		t.Fatalf("the new primary was promoted inside the owner's second wait")
	}

	foWaitTakesPool(t, h, peer, crashedAt)
	unsynced := h.waitRecord(t, 30*time.Second, peer, msgSpSidesUnsynced,
		func(rec logRecord) bool { return rec.num("revision") == failRev })
	if n := unsynced.num("side_cnt"); n != 1 {
		t.Errorf("the new owner's second wait ended with %d sides unsynced, "+
			"want the held side 8", n)
	}
	promoted := foWaitCntlrSynced(t, h, peer, 2, failRev, 10*time.Second)
	if promoted.at.Before(unsynced.at) {
		t.Errorf("the new owner promoted cntlr 2 %v before its second wait "+
			"ended", unsynced.at.Sub(promoted.at).Round(time.Millisecond))
	}

	foLever(t, held, heldKey, func(o *agentfake.Object) { o.HangSyncup = false })
	h.waitRecord(t, 20*time.Second, peer, msgCntlrSettled, foIsCntlr(2))
	foWaitConvergedNow(t, c)
}

// TestTakeoverInACutoverWait pins RW18's cutover across a takeover: a
// migration's destination side is provisioned and reports the wait for the
// cutover, and the primary reports its path to the destination down, so the
// owner waits; it crashes, the path comes up meanwhile, and the worker that
// takes the pool's shard over, which was handed nothing, starts the cutover
// from its own first rounds.
func TestTakeoverInACutoverWait(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	migr, err := plant.PutMigr(context.Background(), h.cli, c.cl, c.sp.Ref,
		plant.MigrSpec{Name: "m0", SrcSide: 8, DstDn: 3, CutoverPending: true})
	if err != nil {
		t.Fatalf("PutMigr: %v", err)
	}
	pathRow := fmt.Sprintf("side_id_to_path.%d", migr.DstSide)
	primary, primaryKey := c.cntlrAgent(t, 1)
	foLever(t, primary, primaryKey, func(o *agentfake.Object) {
		foSetRow(o, pathRow, foStatusRow(pb.ResStatus_RES_STATUS_PROVISIONING, false))
	})
	owner, peer := foStartPair(t, h, c)
	foWaitConvergedNow(t, c)
	dst, dstKey := c.sideAgent(t, migr.DstSide)
	// The destination is provisioned and told its cutover is pending, which
	// its report answers with the wait for the cutover (dnagent.md DN13); the
	// primary has been told the provisioned destination and reports the path
	// down.
	h.waitUntil(t, 10*time.Second, "the owner to wait for the cutover",
		func() (bool, string) {
			rev := c.spRev(t)
			if !c.load(t).Migrs["m0"].GetCutoverPending() {
				t.Fatalf("the cutover ran with the primary's path down")
			}
			for _, msg := range dst.Record(dstKey).Syncups {
				req := msg.(*pb.SyncupSideRequest)
				if req.GetRevision() == rev && req.GetSideConf().GetProvisioned() &&
					req.GetMigrDstConf().GetCutoverPending() {
					return foSyncupArrived(primary, primaryKey, rev, true),
						"the primary has not been told the provisioned destination"
				}
			}
			return false, "the destination is not told it is provisioned yet"
		})
	crashedAt := time.Now()
	owner.crash(t)
	if recs := h.records(owner, msgFlipApplied); slices.ContainsFunc(recs,
		func(rec logRecord) bool { return rec.str("kind") == flipKindCutover }) {
		t.Fatalf("the owner ran the cutover before its crash")
	}
	foLever(t, primary, primaryKey, func(o *agentfake.Object) { delete(o.Rows, pathRow) })

	foWaitTakesPool(t, h, peer, crashedAt)
	cutover := h.waitRecord(t, 20*time.Second, peer, msgFlipApplied,
		func(rec logRecord) bool { return rec.str("kind") == flipKindCutover })
	t.Logf("the new owner ran the cutover %v after the crash",
		cutover.at.Sub(crashedAt).Round(time.Millisecond))
	if c.load(t).Migrs["m0"].GetCutoverPending() {
		t.Errorf("the migration's cutover is still pending after the new " +
			"owner's cutover")
	}
}

// TestTakeoverInARepairWait pins AR8's drive step across a takeover: the
// primary reports a leg failed, the owner creates a worker's repair with its
// new leg on the free disk node, whose agent holds the node syncup that
// introduces the new leg's side, so the side is refused as unknown and stays
// unprovisioned, and every pass holds the leg's group; the owner crashes.
// The worker that takes the pool's shard over finds the repair in etcd,
// holds the group in turn, creates no second repair, and finishes the repair
// once its side is provisioned and the primary reports it started.
func TestTakeoverInARepairWait(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	owner, peer := foStartPair(t, h, c)
	c.waitConverged(t)
	dn3 := c.dnAgent(3)
	foLever(t, dn3, agentfake.DnKey, func(o *agentfake.Object) { o.HangSyncup = true })
	primary, primaryKey := c.cntlrAgent(t, 1)
	foLever(t, primary, primaryKey, func(o *agentfake.Object) {
		foSetRow(o, "leg_id_to_leg.3", foStatusRow(pb.ResStatus_RES_STATUS_ERROR, true))
	})

	created := h.waitRecord(t, 20*time.Second, owner, msgReactionApplied,
		func(rec logRecord) bool { return rec.str("kind") == reactionRepairCreate })
	repName := created.str("rep_name")
	newLeg := created.num("new_leg_id")
	if created.num("leg_id") != 3 || created.str("addr_port") != stdDnAddr(3) {
		t.Fatalf("the repair: for leg %d on %s, want for leg 3 on dn 3",
			created.num("leg_id"), created.str("addr_port"))
	}
	isWait := func(rec logRecord) bool {
		return rec.str("kind") == reactionRepairCreate && rec.num("leg_id") == 3
	}
	h.waitRecord(t, 10*time.Second, owner, msgReactionSkipped, isWait)
	crashedAt := time.Now()
	owner.crash(t)

	foWaitTakesPool(t, h, peer, crashedAt)
	h.waitRecord(t, 20*time.Second, peer, msgReactionSkipped, isWait)
	foLever(t, dn3, agentfake.DnKey, func(o *agentfake.Object) { o.HangSyncup = false })
	h.waitUntil(t, 20*time.Second, "the new leg's side provisioned",
		func() (bool, string) {
			side := c.repair(t, repName).GetNewLegList()[0].GetSideList()[0]
			return side.GetProvisioned(), "not yet"
		})
	c.setVerdictLever(t, repName, agentfake.RepairVerdict{Started: true})
	finished := h.waitRecord(t, 20*time.Second, peer, msgReactionApplied,
		func(rec logRecord) bool {
			return rec.str("kind") == reactionRepairFinish &&
				rec.str("rep_name") == repName
		})
	if finished.num("retired_leg_id") != 3 {
		t.Errorf("the finish %v, want leg 3 retired", finished.attrs)
	}
	t.Logf("the new owner finished the repair %v after the crash",
		finished.at.Sub(crashedAt).Round(time.Millisecond))
	if n := len(foReactions(h, nil, reactionRepairCreate)); n != 1 {
		t.Errorf("%d repairs created, want the one the owner created", n)
	}
	state := c.load(t)
	if _, ok := state.RetiredLegs[3]; !ok {
		t.Errorf("leg 3 is no retired leg after the finish")
	}
	for _, grp := range state.Slices[stdSliceId].GetMetaGrpList() {
		legs := legIds(grp.GetLegList())
		if !slices.Contains(legs, newLeg) || slices.Contains(legs, 3) {
			t.Errorf("the meta group holds legs %v, want %d in place of 3",
				legs, newLeg)
		}
	}
}
