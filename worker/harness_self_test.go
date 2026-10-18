package worker

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/integtest/plant"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The harness's own tests: what the scenario cases rely on in crash, stop
// and the shortened timeouts. They test the harness, not a scenario. The push
// timeout of the seam waits for a case that holds a bitmap push.

// TestHarnessCrashLeavesRegistrations tests the harness, not a scenario:
// crash stops a worker as a killed one stops, so its registrations stay in
// etcd until its peer finds it dead by the dead threshold, commits it a
// nonmember and collects them (VW3, VW6), and the peer takes over every shard
// of it (VW9); stop is CM5's orderly stop, after which the worker's
// registrations are gone at once.
func TestHarnessCrashLeavesRegistrations(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	w1 := h.startWorker(t, "w1")
	w2 := h.startWorker(t, "w2")
	roles := w1.roles
	h.waitUntil(t, 20*time.Second, "w1 and w2 to share every shard",
		func() (bool, string) {
			for _, role := range roles {
				n1, n2 := w1.ownedCount(role), w2.ownedCount(role)
				if n1 == 0 || n2 == 0 || n1+n2 != common.ShardBucketSize {
					return false, fmt.Sprintf("%s: w1 %d, w2 %d", role, n1, n2)
				}
			}
			return true, ""
		})
	ctx := context.Background()
	regsOf := func(w *harnessWorker) []string {
		var found []string
		for _, role := range roles {
			key := model.WorkerRegKey(role, w.seed)
			ok, err := h.cli.Get(ctx, key, &pb.WorkerReg{})
			if err != nil {
				t.Fatalf("get %s: %v", key, err)
			}
			if ok {
				found = append(found, key)
			}
		}
		return found
	}
	// The deletes of the registry keys, with the time each reached the
	// case, tell who deleted w1's keys: its own CM5 would delete them before
	// any peer commits it a nonmember, a peer's collection only after.
	deletedAt := watchDeletes(t, h, roles)

	crashedAt := time.Now()
	w1.crash(t)
	if regs := regsOf(w1); len(regs) != len(roles) {
		t.Fatalf("after the crash w1 has registrations %v, want all %d",
			regs, len(roles))
	}
	h.waitOwnsAll(t, w2)
	t.Logf("w2 owns every shard %v after the crash",
		time.Since(crashedAt).Round(time.Millisecond))
	h.waitUntil(t, 10*time.Second, "w2 to collect w1's registrations",
		func() (bool, string) {
			regs := regsOf(w1)
			return len(regs) == 0, fmt.Sprintf("%v still there", regs)
		})
	for _, role := range roles {
		key := model.WorkerRegKey(role, w1.seed)
		at, ok := deletedAt(key)
		if !ok {
			t.Fatalf("no delete of %s reached the watch", key)
		}
		var committed []logRecord
		for _, rec := range h.records(nil, msgMembershipCommitted) {
			if rec.str("seed") == w1.seed && rec.str("role") == role &&
				rec.str("state") == stateNonmember {
				committed = append(committed, rec)
			}
		}
		if len(committed) == 0 {
			t.Fatalf("no %s commit of w1 as a nonmember", role)
		}
		if at.Before(committed[0].at) {
			t.Errorf("%s was deleted %v before w2 committed w1 a nonmember: "+
				"the crash ran its orderly stop", key,
				committed[0].at.Sub(at).Round(time.Millisecond))
		}
	}

	w2.stop(t)
	if regs := regsOf(w2); len(regs) != 0 {
		t.Errorf("after the orderly stop w2 still has registrations %v", regs)
	}
}

// TestHarnessCrashEndsCallsInFlight tests the harness, not a scenario: a
// crash ends the worker's calls in flight as a killed process's sockets end
// them, so a Syncup* an agent holds at the crash applies nothing once the
// agent lets it go, and the next incarnation finds the agent short of its
// revision and sends the syncup again (RW4 step 5). An orderly stop lets such
// a call finish instead (RW11).
func TestHarnessCrashEndsCallsInFlight(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	dn3 := c.dnAgent(3)
	hang := func(on bool) {
		t.Helper()
		if err := dn3.Update(agentfake.DnKey,
			func(o *agentfake.Object) { o.HangSyncup = on }); err != nil {
			t.Fatalf("hang_syncup %t on dn 3: %v", on, err)
		}
	}
	hang(true)
	w1 := h.startWorker(t, "w1", common.WorkerRoleDn)
	h.waitOwnsAll(t, w1)
	h.waitUntil(t, 10*time.Second, "w1's held SyncupDn of dn 3 beside its "+
		"open check stream", func() (bool, string) {
		rec := dn3.Record(agentfake.DnKey)
		return len(rec.Syncups) == 1 && rec.Streams > rec.StreamEnds,
			fmt.Sprintf("%d syncups, %d of %d streams ended",
				len(rec.Syncups), rec.StreamEnds, rec.Streams)
	})
	w1.crash(t)
	// The cut reaches the fake's server on a goroutine of its own, which
	// ends every call of the cut connection together. The lever is cleared
	// only once w1's check stream on that connection has ended, so the held
	// syncup has ended too, and clearing the lever cannot let it apply.
	h.waitUntil(t, 10*time.Second, "dn 3's server to end w1's calls",
		func() (bool, string) {
			rec := dn3.Record(agentfake.DnKey)
			return rec.StreamEnds == rec.Streams,
				fmt.Sprintf("%d of %d streams ended", rec.StreamEnds, rec.Streams)
		})
	hang(false)
	w2 := h.restart(t, w1)
	h.waitOwnsAll(t, w2)
	h.waitUntil(t, 10*time.Second, "w2's SyncupDn of dn 3, which the crash "+
		"left at no revision", func() (bool, string) {
		n := len(dn3.Record(agentfake.DnKey).Syncups)
		return n >= 2, fmt.Sprintf("%d syncups", n)
	})
}

// watchDeletes watches the registry of every role from now on and returns a
// lookup of when the delete of a key reached the case. The watches end, and
// their goroutines are joined, at the case's cleanup.
func watchDeletes(
	t *testing.T,
	h *workerHarness,
	roles []string,
) func(key string) (time.Time, bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	var mu sync.Mutex
	deleted := make(map[string]time.Time)
	for _, role := range roles {
		prefix := model.WorkerRegPrefix(role)
		_, rev, err := h.cli.Range(ctx, prefix)
		if err != nil {
			t.Fatalf("range %s: %v", prefix, err)
		}
		events, _ := h.cli.WatchTyped(ctx, prefix, rev+1,
			func() proto.Message { return &pb.WorkerReg{} })
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ev := range events {
				if ev.Type != etcdutil.EventDelete {
					continue
				}
				mu.Lock()
				if _, seen := deleted[ev.Key]; !seen {
					deleted[ev.Key] = time.Now()
				}
				mu.Unlock()
			}
		}()
	}
	return func(key string) (time.Time, bool) {
		mu.Lock()
		defer mu.Unlock()
		at, ok := deleted[key]
		return at, ok
	}
}

// TestHarnessShortensTheSyncupTimeout tests the harness's seam, not a
// scenario: the worker runs with the harness's syncup timeout in place of the
// production one, so a Syncup* its agent never answers fails at the harness's
// syncup timeout (RW5), and a stop during it waits that long at most for the
// call to finish (RW11). With the production timeout each would take a
// minute.
func TestHarnessShortensTheSyncupTimeout(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	// dn 3 holds nothing of the pool; it answers its rounds and never its
	// syncup.
	if err := c.dnAgent(3).Update(agentfake.DnKey,
		func(o *agentfake.Object) { o.HangSyncup = true }); err != nil {
		t.Fatalf("hang_syncup on dn 3: %v", err)
	}
	w := h.startWorker(t, "w1", common.WorkerRoleDn)
	h.waitOwnsAll(t, w)
	var sentAt time.Time
	h.waitUntil(t, 10*time.Second, "dn 3's first SyncupDn", func() (bool, string) {
		if len(c.dnAgent(3).Record(agentfake.DnKey).Syncups) == 0 {
			return false, "none yet"
		}
		sentAt = time.Now()
		return true, ""
	})
	failed := h.waitRecord(t, 3*h.tmo.syncup+5*time.Second, w, msgSyncupResult,
		func(rec logRecord) bool {
			return rec.num("dn_id") == 3 && rec.str("error") != ""
		})
	took := failed.at.Sub(sentAt)
	t.Logf("dn 3's hung syncup failed %v after it reached the fake: %s",
		took.Round(time.Millisecond), failed.str("error"))
	if took < h.tmo.syncup-time.Second || took > 2*h.tmo.syncup+2*time.Second {
		t.Errorf("the hung syncup failed %v after it was sent, want about "+
			"the harness's syncup timeout %v", took, h.tmo.syncup)
	}
	// The next round sends it again, and a stop now waits for that call.
	h.waitUntil(t, 10*time.Second, "dn 3's second SyncupDn", func() (bool, string) {
		n := len(c.dnAgent(3).Record(agentfake.DnKey).Syncups)
		return n >= 2, fmt.Sprintf("%d syncups", n)
	})
	stopAt := time.Now()
	w.stop(t)
	stopTook := time.Since(stopAt)
	t.Logf("the stop during a hung syncup took %v", stopTook.Round(time.Millisecond))
	if stopTook > 2*h.tmo.syncup+2*time.Second {
		t.Errorf("the stop took %v, want at most about the syncup timeout %v",
			stopTook, h.tmo.syncup)
	}
}

// TestHarnessShortensTheDemotionHold tests the harness's seam, not a
// scenario: the worker's demotion hold (RW22) lasts the harness's hold, not
// the production one. cn 1's fake holds every syncup of cntlr 1, so the old
// primary never reports its demotion applied and the hold ends by its timer:
// the "sp demotion unsynced" record names cntlr 1 about one harness hold
// after the failover's commit, and only then are the sides sent the
// failover's revision.
func TestHarnessShortensTheDemotionHold(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)
	oldAgent, oldKey := c.cntlrAgent(t, 1)
	setHang := func(on bool) error {
		return oldAgent.Update(oldKey,
			func(o *agentfake.Object) { o.HangSyncup = on })
	}
	if err := setHang(true); err != nil {
		t.Fatalf("hang_syncup on cntlr 1: %v", err)
	}
	// Cleared at the end, so the stop at cleanup waits for no held call.
	defer func() { _ = setHang(false) }()
	disabled := true
	if _, _, err := plant.SetCntlr(context.Background(), h.cli, c.cl, c.sp.Ref, 1,
		plant.CntlrFlags{Disabled: &disabled}); err != nil {
		t.Fatalf("disable cntlr 1: %v", err)
	}

	applied := h.waitRecord(t, 20*time.Second, w, msgReactionApplied,
		func(rec logRecord) bool { return rec.str("kind") == reactionFailover })
	failRev := applied.num("revision")
	unsynced := h.waitRecord(t, 10*time.Second, w, msgSpDemotionUnsynced, nil)
	took := unsynced.at.Sub(applied.at)
	t.Logf("the demotion hold ended %v after the failover's commit at revision %d",
		took.Round(time.Millisecond), failRev)
	if ids := unsynced.str("cntlr_ids"); ids != "[1]" ||
		unsynced.num("revision") != failRev {
		t.Errorf("the hold ended at revision %d naming cntlrs %s, want the "+
			"failover's %d naming cntlr 1", unsynced.num("revision"), ids, failRev)
	}
	// The hold starts when the coordinator takes the failover's revision,
	// a moment after the commit, and ends by its timer.
	hold := h.tmo.demotionHold
	round := harnessInterval * time.Second
	if took < hold-250*time.Millisecond || took > hold+round {
		t.Errorf("the hold ended %v after the commit, want about the "+
			"harness's hold %v and at most one round more (production %v)",
			took, hold, common.DemotionHoldTimeout*time.Second)
	}

	sideSent := make(map[uint64]logRecord)
	h.waitUntil(t, 10*time.Second, "every side's syncup at the failover's "+
		"revision", func() (bool, string) {
		for _, rec := range h.records(w, msgSyncupResult) {
			id, ok := sideIdOf(rec)
			if _, seen := sideSent[id]; ok && !seen && rec.num("revision") >= failRev {
				sideSent[id] = rec
			}
		}
		for _, side := range stdSides {
			if _, ok := sideSent[side.sideId]; !ok {
				return false, fmt.Sprintf("none for side %d yet", side.sideId)
			}
		}
		return true, ""
	})
	var slowest time.Duration
	for _, side := range stdSides {
		rec := sideSent[side.sideId]
		slowest = max(slowest, rec.at.Sub(unsynced.at))
		if rec.at.Before(unsynced.at) {
			t.Errorf("side %d had its syncup at revision %d %v before the "+
				"hold ended", side.sideId, rec.num("revision"),
				unsynced.at.Sub(rec.at).Round(time.Millisecond))
		}
		if after := rec.at.Sub(unsynced.at); after > round {
			t.Errorf("side %d had its syncup at the failover's revision %v "+
				"after the hold ended, want within a round", side.sideId,
				after.Round(time.Millisecond))
		}
	}
	t.Logf("the last side's syncup at the failover's revision came %v after "+
		"the hold ended", slowest.Round(time.Millisecond))
}

// TestHarnessShortensTheEtcdOpTimeout tests the harness's seam, not a
// scenario: each delete of a stopping worker's registrations is bounded by
// the harness's etcd operation timeout (CM5), here shorter than the
// harness's default. A delete on the closed client of a crashed worker does
// not fail at once, since the etcd client retries it for a few seconds of
// its own, so each ends at that bound, and the crashed worker's run returns
// about one bound per role after the crash. With the production timeout the
// client's own retries end each delete, seconds later.
func TestHarnessShortensTheEtcdOpTimeout(t *testing.T) {
	t.Parallel()
	tmo := harnessTimeouts()
	tmo.etcdOp = 500 * time.Millisecond
	h := newWorkerHarness(t, harnessOpts{tmo: &tmo})
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	crashedAt := time.Now()
	w.crash(t)
	w.join(t)
	took := time.Since(crashedAt)
	bound := time.Duration(len(w.roles)) * tmo.etcdOp
	t.Logf("the crashed worker's run returned %v after the crash, its %d "+
		"deletes bounded by %v each", took.Round(time.Millisecond),
		len(w.roles), tmo.etcdOp)
	// The slack covers the wind-down's other steps on a loaded machine; the
	// production timeout costs seconds per role.
	if took > bound+2*time.Second {
		t.Errorf("the crashed worker's run returned %v after the crash, want "+
			"at most about %v, one etcd operation timeout per role", took, bound)
	}
}
