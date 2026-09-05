package worker

import (
	"context"
	"errors"
	"testing"

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

// TestConfCacheResolvesDefaults checks the RW21 defaults: the four intervals
// (0 => 5, clamped to [1, 3600]), extent_size (0 => DefaultDnExtSize), the
// dn_bin_conf shifts, and qos_ratio exactly as stored.
func TestConfCacheResolvesDefaults(t *testing.T) {
	h := newConfHarness(t)
	const name = "defaults"
	const epoch = uint64(42)
	cid := model.ClusterId(name, epoch)
	qos := &pb.QosRatio{Strict: true, BytesPerIops: 4096}

	err := h.store.Put(
		context.Background(),
		model.ClusterConfKey(name),
		&pb.ClusterConf{
			CreationEpoch: epoch,
			QosRatio:      qos,
			HealthCheckConf: &pb.HealthCheckConf{
				DnInterval: 0,
				CnInterval: 100000,
			},
		},
	)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	waitFor(t, "cluster cached", func() bool {
		_, ok := h.cache.get(cid)
		return ok
	})
	cc, _ := h.cache.get(cid)
	hc := cc.GetHealthCheckConf()
	if hc.GetDnInterval() != common.DefaultHealthCheckInterval {
		t.Fatalf("dn_interval = %d, want %d",
			hc.GetDnInterval(), common.DefaultHealthCheckInterval)
	}
	if hc.GetCnInterval() != common.MaxHealthCheckInterval {
		t.Fatalf("cn_interval = %d, want the %d clamp",
			hc.GetCnInterval(), common.MaxHealthCheckInterval)
	}
	if hc.GetSideInterval() != common.DefaultHealthCheckInterval ||
		hc.GetCntlrInterval() != common.DefaultHealthCheckInterval {
		t.Fatalf("side/cntlr intervals = %d/%d, want the default",
			hc.GetSideInterval(), hc.GetCntlrInterval())
	}
	bin := cc.GetDnBinConf()
	if bin.GetExtentSize() != common.DefaultDnExtSize {
		t.Fatalf("extent_size = %d, want %d",
			bin.GetExtentSize(), common.DefaultDnExtSize)
	}
	if bin.GetBin0Shift() != common.DefaultDnBin0Shift ||
		bin.GetBin3Shift() != common.DefaultDnBin3Shift {
		t.Fatalf("bin shifts = %d..%d, want the defaults",
			bin.GetBin0Shift(), bin.GetBin3Shift())
	}
	if !cc.GetQosRatio().GetStrict() ||
		cc.GetQosRatio().GetBytesPerIops() != 4096 {
		t.Fatalf("qos_ratio = %v, want it as stored", cc.GetQosRatio())
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
