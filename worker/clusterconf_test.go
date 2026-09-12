package worker

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// confHarness runs a real ClusterConf cache (RW21) over the in-memory store.
type confHarness struct {
	t      *testing.T
	store  *fakeStore
	clk    *fakeClock
	cache  *confCache
	cancel context.CancelFunc
	done   chan struct{}
}

func newConfHarness(t *testing.T) *confHarness {
	t.Helper()
	captureLogs(t)
	store := newFakeStore()
	clk := newFakeClock()
	d := newTestDeps(testConfig(common.WorkerRoleDn), store, clk)
	ctx, cancel := context.WithCancel(context.Background())
	h := &confHarness{
		t: t, store: store, clk: clk, cache: d.conf,
		cancel: cancel, done: make(chan struct{}),
	}
	go func() {
		defer close(h.done)
		h.cache.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-h.done
	})
	waitFor(t, "cache watch", func() bool { return store.watchCount() > 0 })
	return h
}

// TestConfCacheKeyToIdDerivation checks that RW21 keys entries by
// ClusterId(name from the key, creation_epoch from the value).
func TestConfCacheKeyToIdDerivation(t *testing.T) {
	h := newConfHarness(t)
	const name = "prod"
	const epoch = uint64(1700000000)
	cid := model.ClusterId(name, epoch)

	err := h.store.Put(
		context.Background(),
		model.ClusterConfKey(name),
		&pb.ClusterConf{CreationEpoch: epoch},
	)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	waitFor(t, "cluster cached", func() bool {
		_, ok := h.cache.get(cid)
		return ok
	})
	if _, ok := h.cache.get(cid + 1); ok {
		t.Fatalf("an unrelated cluster id resolved")
	}
}

// TestConfCacheReturnsTheConfAsStored is the mirror image of the resolution
// the cache used to do: §7 makes the gateway resolve every defaultable member
// at WRITE time, so RW21 hands a reader the stored bytes and changes nothing.
//
// Three of the members below are values the cache's old resolver WOULD have
// rewritten on the way out — a dn_interval of 100000 (it clamped to 3600),
// absent side and cntlr intervals (it substituted 5), no dn_bin_conf at all
// (it substituted the 0/4/8/12 ladder and DefaultDnExtSize). The fourth, a
// low_water_mark_pct above 100, is the one value NOTHING ever rewrites, on
// either path: §7 gives it a meaning, "never grow this pool automatically".
// Every one of them comes back untouched. That is not a detail: it is what
// makes an INVALID stored conf reach a reader at all, and therefore what makes
// the refusals in revision.go, reaction.go, sprole.go and health.go
// expressible.
func TestConfCacheReturnsTheConfAsStored(t *testing.T) {
	h := newConfHarness(t)
	const name = "asstored"
	const epoch = uint64(42)
	cid := model.ClusterId(name, epoch)
	stored := &pb.ClusterConf{
		CreationEpoch: epoch,
		QosRatio:      &pb.QosRatio{Strict: true, BytesPerIops: 4096},
		HealthCheckConf: &pb.HealthCheckConf{
			DnInterval: 100000,
			CnInterval: 7,
		},
		BdevConf: &pb.BdevConf{
			DmPoolConf: &pb.DmPoolConf{LowWaterMarkPct: 150},
		},
	}

	err := h.store.Put(
		context.Background(), model.ClusterConfKey(name), stored,
	)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	waitFor(t, "cluster cached", func() bool {
		_, ok := h.cache.get(cid)
		return ok
	})
	cc, _ := h.cache.get(cid)
	if !proto.Equal(cc, stored) {
		t.Fatalf("cached conf =\n%v\nwant the stored\n%v", cc, stored)
	}
	// Spelled out for the three members a resolver would have been most
	// tempted by, so a reintroduced read-time default cannot hide behind a
	// message-level comparison someone later loosens.
	if got := cc.GetHealthCheckConf().GetDnInterval(); got != 100000 {
		t.Fatalf("dn_interval = %d, want the stored 100000 unclamped", got)
	}
	if cc.GetDnBinConf() != nil {
		t.Fatalf("dn_bin_conf = %v, want the stored absence", cc.GetDnBinConf())
	}
	if got := cc.GetBdevConf().GetDmPoolConf().GetLowWaterMarkPct(); got != 150 {
		t.Fatalf("low_water_mark_pct = %d, want the stored 150", got)
	}
	// A conf like this is exactly what model.ValidateClusterConf exists to
	// refuse: the cache caches it, the readers reject it (§7).
	if err := model.ValidateClusterConf(cc); err == nil {
		t.Fatalf("an unresolved stored conf validated")
	}
}

// TestConfCacheKeepsAnInvalidConf pins the cache's deliberate non-drop: an
// unusable conf stays in the cache, because dropping it would make a cluster
// whose conf went bad look exactly like a deleted one to every reader and send
// an operator chasing a phantom deletion (clusterconf.go, §7).
func TestConfCacheKeepsAnInvalidConf(t *testing.T) {
	h := newConfHarness(t)
	const name = "corrupt"
	const epoch = uint64(43)
	cid := model.ClusterId(name, epoch)

	// A ladder-less DnBinConf: valid protobuf, impossible from CreateCluster.
	bad := testClusterConf(func(cc *pb.ClusterConf) {
		cc.CreationEpoch = epoch
		cc.DnBinConf = nil
	})
	err := h.store.Put(context.Background(), model.ClusterConfKey(name), bad)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	waitFor(t, "cluster cached", func() bool {
		_, ok := h.cache.get(cid)
		return ok
	})
	cc, ok := h.cache.get(cid)
	if !ok {
		t.Fatalf("the cache dropped an invalid conf")
	}
	if err := model.ValidateClusterConf(cc); err == nil {
		t.Fatalf("the cache repaired the conf it was given")
	}
}

// TestConfCacheDelete checks that a delete drops the entry even though the
// event carries no value — the cache remembers the name -> id mapping (RW21).
func TestConfCacheDelete(t *testing.T) {
	h := newConfHarness(t)
	const name = "gone"
	const epoch = uint64(7)
	cid := model.ClusterId(name, epoch)
	key := model.ClusterConfKey(name)

	if err := h.store.Put(
		context.Background(), key, &pb.ClusterConf{CreationEpoch: epoch},
	); err != nil {
		t.Fatalf("put: %v", err)
	}
	waitFor(t, "cluster cached", func() bool {
		_, ok := h.cache.get(cid)
		return ok
	})
	if err := h.store.Delete(context.Background(), key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	waitFor(t, "cluster dropped", func() bool {
		_, ok := h.cache.get(cid)
		return !ok
	})
}

// TestConfCacheRescanAfterWatchError checks the SW4-style recovery: a
// cancelled watch makes the cache rescan and pick up what it missed.
func TestConfCacheRescanAfterWatchError(t *testing.T) {
	h := newConfHarness(t)
	const name = "compacted"
	const epoch = uint64(11)
	cid := model.ClusterId(name, epoch)

	// Write while the watch is down, then break it.
	h.store.seed(t, model.ClusterConfKey(name),
		&pb.ClusterConf{CreationEpoch: epoch})
	h.store.breakWatches(errors.New("etcdserver: mvcc: required revision has been compacted"))
	waitFor(t, "cluster picked up by the rescan", func() bool {
		_, ok := h.cache.get(cid)
		return ok
	})
	waitFor(t, "watch restarted", func() bool {
		return h.store.watchCount() > 0
	})
}

// TestConfCacheMissingClusterIdles is the reader half of RW9/SW6: a cluster
// the cache has never seen simply is not there.
func TestConfCacheMissingClusterIdles(t *testing.T) {
	h := newConfHarness(t)
	if _, ok := h.cache.get(12345); ok {
		t.Fatalf("an unknown cluster resolved")
	}
}
