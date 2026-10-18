package worker

import (
	"fmt"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
)

// The whole-worker scenario cases of an agent restart (dnv-worker.md, The
// per-object loop, RW4 step 5 and RW5): a dn agent killed and started again
// on its kept local files, and one started again on empty files.

// dn1Objects are dn 1's object keys on its fake: the node and its two sides.
func dn1Objects() []string {
	keys := []string{agentfake.DnKey}
	for _, side := range stdSides {
		if side.dnId == 1 {
			keys = append(keys, agentfake.SideKey(stdSpId, side.legId, side.sideId))
		}
	}
	return keys
}

// waitDn1Down stops dn 1's fake and waits until the worker has seen it
// down: dn 1 and both its sides carry a health epoch.
func (c *stdCluster) waitDn1Down(t *testing.T) {
	t.Helper()
	c.dnAgent(1).Stop()
	c.h.waitUntil(t, 20*time.Second, "dn 1 and its sides seen down",
		func() (bool, string) {
			if epoch := c.dnConf(t, 1).GetErrEpoch(); epoch == 0 {
				return false, "dn 1 has no health epoch"
			}
			for _, side := range stdSides {
				if side.dnId != 1 {
					continue
				}
				if _, _, s, ok := c.sideOf(t, side.sideId); !ok || s.GetErrEpoch() == 0 {
					return false, fmt.Sprintf("side %d has no health epoch", side.sideId)
				}
			}
			return true, ""
		})
}

// TestScenAgentRestartWithItsFilesGetsNoSyncup pins RW4 step 5 for an agent
// restart: a dn agent killed and started again on its kept files answers its
// rounds at the revision the worker wants, for the node and for each of its
// sides, so the worker sends it no syncup at all, and its health epochs
// clear on its clean rounds.
func TestScenAgentRestartWithItsFilesGetsNoSyncup(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	dn1 := c.dnAgent(1)
	stoppedAt := time.Now()
	c.waitDn1Down(t)
	t.Logf("dn 1 seen down %v after its stop", time.Since(stoppedAt).Round(time.Millisecond))
	before := map[string]int{}
	for _, key := range dn1Objects() {
		before[key] = len(dn1.Record(key).Syncups)
	}
	if err := dn1.Start(agentfake.KeepStore); err != nil {
		t.Fatalf("restart dn 1: %v", err)
	}
	// Three rounds of every object of dn 1 after the restart.
	startedAt := time.Now()
	h.waitRounds(t, dn1, 3, dn1Objects()...)
	t.Logf("three rounds of each object %v after the start", time.Since(startedAt).Round(time.Millisecond))
	for _, key := range dn1Objects() {
		if n := len(dn1.Record(key).Syncups) - before[key]; n != 0 {
			t.Errorf("%s got %d syncups after a restart with its files kept, "+
				"want none", key, n)
		}
	}
	c.waitConverged(t)
	t.Logf("converged %v after the start", time.Since(startedAt).Round(time.Millisecond))
}

// TestScenAgentRestartWithEmptyFilesResyncs pins RW4 step 5 and RW5 for an
// agent started again on empty files: it answers at no revision, so the worker
// re-sends the node's syncup; its sides are unknown objects to it until that
// syncup lists them, so every side syncup meanwhile is refused as unknown,
// logged as `syncup rejected` and re-sent from the next round, and the first
// side syncup accepted comes after the node's. The case holds the node's
// syncup for a few rounds, so the refusals last as long as it wants.
func TestScenAgentRestartWithEmptyFilesResyncs(t *testing.T) {
	t.Parallel()
	h := newWorkerHarness(t, harnessOpts{})
	c := h.plantStd(t, nil)
	w := h.startWorker(t, "w1")
	h.waitOwnsAll(t, w)
	c.waitConverged(t)

	dn1 := c.dnAgent(1)
	c.waitDn1Down(t)
	// The node's syncup is held from the agent's start on.
	setLever(t, dn1, agentfake.DnKey, func(o *agentfake.Object) { o.HangSyncup = true })
	defer setLever(t, dn1, agentfake.DnKey, func(o *agentfake.Object) { o.HangSyncup = false })
	restartedAt := time.Now()
	if err := dn1.Start(agentfake.DropStore); err != nil {
		t.Fatalf("restart dn 1: %v", err)
	}
	onDn1 := func(rec logRecord) bool {
		id, ok := sideIdOf(rec)
		return ok && rec.num("dn_id") == 1 && id != 0 && rec.at.After(restartedAt)
	}
	for _, side := range stdSides {
		if side.dnId != 1 {
			continue
		}
		sideId := side.sideId
		h.waitCount(t, 20*time.Second, w, msgSyncupRejected, 2,
			func(rec logRecord) bool {
				id, _ := sideIdOf(rec)
				return onDn1(rec) && id == sideId &&
					rec.num("code") == common.ReplyCodeUnknownObject
			})
	}
	accepted := func(rec logRecord) bool {
		return rec.num("code") == 0 && rec.str("error") == "" && rec.at.After(restartedAt)
	}
	if n := h.countRecs(w, msgSyncupResult, and(onDn1, accepted)); n != 0 {
		t.Fatalf("%d side syncups accepted while dn 1's own syncup was held", n)
	}

	setLever(t, dn1, agentfake.DnKey, func(o *agentfake.Object) { o.HangSyncup = false })
	dnApplied := h.waitRecord(t, 20*time.Second, w, msgSyncupResult,
		func(rec logRecord) bool { return isNodeRec(rec, "dn_id", 1) && accepted(rec) })
	for _, side := range stdSides {
		if side.dnId != 1 {
			continue
		}
		sideId := side.sideId
		first := h.waitRecord(t, 20*time.Second, w, msgSyncupResult,
			func(rec logRecord) bool {
				id, _ := sideIdOf(rec)
				return onDn1(rec) && id == sideId && accepted(rec)
			})
		if first.at.Before(dnApplied.at) {
			t.Errorf("side %d's syncup was accepted %v before dn 1's syncup "+
				"listed it", sideId, dnApplied.at.Sub(first.at))
		}
	}
	c.waitConverged(t)
}
