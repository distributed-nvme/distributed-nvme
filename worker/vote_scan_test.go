package worker

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestVoteFailedScanRetriesAfterAnInterval is VW3's failed scan: a scan of
// the registry that fails is tried again one vote interval later, never in a
// loop, and the role's watch restarts only from a scan that worked. Here the
// rescan after a broken watch fails once: no watch opens, nothing is
// observed, and a registration put meanwhile is found only by the retry, one
// interval after the failure and not before.
func TestVoteFailedScanRetriesAfterAnInterval(t *testing.T) {
	synctest.Test(t, testVoteFailedScanRetriesAfterAnInterval)
}

func testVoteFailedScanRetriesAfterAnInterval(t *testing.T) {
	h := newVoteHarness(t, common.WorkerRoleDn)
	interval := h.deps.cfg.VoteInterval
	h.store.setRangeErr(errors.New("etcd down"))
	h.store.breakWatches(errors.New("mvcc: required revision has been compacted"))
	h.settle()
	if n := h.store.watchCount(); n != 0 {
		t.Fatalf("%d watches open after a failed scan, want none: the "+
			"watch restarts only from a scan that worked", n)
	}

	// The registry answers again, and a peer's registration lands with no
	// event the vote worker could see: only a scan finds it.
	h.store.setRangeErr(nil)
	peer := seedOf(2)
	h.store.seed(t, workerRegKey(common.WorkerRoleDn, peer),
		&pb.WorkerReg{Epoch: h.clk.nowUnix()})
	h.advance(interval - time.Second)
	if n := h.store.watchCount(); n != 0 {
		t.Fatalf("%d watches open %v after the failed scan, want none: "+
			"the scan was retried before one vote interval", n,
			interval-time.Second)
	}
	if states := h.observedStates(common.WorkerRoleDn, peer); len(states) != 0 {
		t.Fatalf("peer observed %v before the retry", states)
	}

	h.advance(time.Second)
	waitFor(t, "the retried scan's watch", func() bool {
		return h.store.watchCount() == 1
	})
	h.settle()
	if states := h.observedStates(common.WorkerRoleDn, peer); len(states) != 1 ||
		states[0] != stateLive {
		t.Fatalf("peer observed %v after the retry, want one live appear",
			states)
	}
}
