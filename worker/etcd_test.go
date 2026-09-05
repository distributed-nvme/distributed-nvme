package worker

import (
	"context"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The etcd-backed half of the §13 list (EU7): everything here goes through
// the real etcdutil client and the real model ops, and skips cleanly when no
// etcd binary is available.

// TestHealthWritesThroughModel checks HL1/HL3 against real etcd: the monitor
// writes only on a transition, model.SetDnErrEpoch never restarts the
// threshold clock, and two owners observing the same transition write once.
func TestHealthWritesThroughModel(t *testing.T) {
	cli := newTestClient(t)
	captureLogs(t)
	ctx := context.Background()
	cid := testClusterId()
	const addr = "dn0:9520"

	err := cli.Put(ctx, model.DnConfKey(cid, addr), &pb.DnConf{
		DnId:        11,
		TotalExtCnt: 100,
		FreeExtCnt:  100,
	})
	if err != nil {
		t.Fatalf("seed DnConf: %v", err)
	}

	clk := newFakeClock()
	d := newTestDeps(testConfig(common.WorkerRoleDn), newFakeStore(), clk)
	d.cli = cli
	d.health = &modelHealthWriter{cli: cli}
	d.conf.entries[cid] = model.ResolveClusterConf(&pb.ClusterConf{})

	owner := newDnMonitor(d, cid, 11, func() string { return addr })
	// A second owner of the same DN, the accepted overlap of §0 item 4.
	peer := newDnMonitor(d, cid, 11, func() string { return addr })

	owner.observe(ctx, healthUnreachable, "")
	stored := &pb.DnConf{}
	if _, err := cli.Get(ctx, model.DnConfKey(cid, addr), stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	first := stored.GetErrEpoch()
	if first == 0 {
		t.Fatalf("err_epoch not set")
	}

	// The peer observes the same transition later; the STM re-read makes its
	// write a no-op, so the threshold clock does not restart (HL3, MD6).
	clk.advance(30 * time.Second)
	peer.observe(ctx, healthErrorRow, "disk")
	if _, err := cli.Get(ctx, model.DnConfKey(cid, addr), stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.GetErrEpoch() != first {
		t.Fatalf("err_epoch moved from %d to %d", first, stored.GetErrEpoch())
	}

	// Recovery clears it.
	owner.observe(ctx, healthClean, "")
	if _, err := cli.Get(ctx, model.DnConfKey(cid, addr), stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.GetErrEpoch() != 0 {
		t.Fatalf("err_epoch = %d after recovery", stored.GetErrEpoch())
	}
}

// TestConfCacheAgainstEtcd runs the RW21 cache over the real client: scan,
// watch, key -> id derivation and delete.
func TestConfCacheAgainstEtcd(t *testing.T) {
	cli := newTestClient(t)
	captureLogs(t)
	ctx := context.Background()
	name := "etcdcache"
	const epoch = uint64(1234567)
	cid := model.ClusterId(name, epoch)
	key := model.ClusterConfKey(name)
	t.Cleanup(func() { _ = cli.Delete(context.Background(), key) })

	d := newTestDeps(
		testConfig(common.WorkerRoleDn), newFakeStore(), newFakeClock(),
	)
	d.store = cli
	d.cli = cli
	cache := newConfCache(d)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.run(runCtx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	err := cli.Put(ctx, key, &pb.ClusterConf{
		CreationEpoch: epoch,
		QosRatio:      &pb.QosRatio{BytesPerIops: 512},
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	waitFor(t, "cluster cached", func() bool {
		_, ok := cache.get(cid)
		return ok
	})
	cc, _ := cache.get(cid)
	if cc.GetHealthCheckConf().GetDnInterval() !=
		common.DefaultHealthCheckInterval {
		t.Fatalf("dn_interval = %d", cc.GetHealthCheckConf().GetDnInterval())
	}
	if cc.GetQosRatio().GetBytesPerIops() != 512 {
		t.Fatalf("qos_ratio = %v", cc.GetQosRatio())
	}
	if err := cli.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	waitFor(t, "cluster dropped", func() bool {
		_, ok := cache.get(cid)
		return !ok
	})
}

// TestVoteWorkerAgainstEtcd is the end-to-end smoke of §6 over real etcd: the
// worker registers, observes its own key in its own first scan, drives
// nothing for one grace window, then owns every shard of its role, and
// deletes its registration on a graceful stop (CM5).
func TestVoteWorkerAgainstEtcd(t *testing.T) {
	cli := newTestClient(t)
	logs := captureLogs(t)

	cfg := Config{
		Roles:        []string{common.WorkerRoleDn},
		VoteInterval: 100 * time.Millisecond,
		GraceTime:    400 * time.Millisecond,
		Endpoints:    []string{testEndpoint},
	}
	d := &deps{
		cfg:    cfg,
		store:  cli,
		cli:    cli,
		conns:  newConnCache(),
		health: &fakeHealthWriter{},
		clk:    realClock{},
		kinds:  kindFor,
	}
	d.conf = newConfCache(d)

	seed, err := newSeed()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	vote := newVoteWorker(d, seed)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		vote.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = cli.Delete(
			context.Background(),
			workerRegKey(common.WorkerRoleDn, seed),
		)
	})

	key := workerRegKey(common.WorkerRoleDn, seed)
	waitFor(t, "registration in etcd", func() bool {
		reg := &pb.WorkerReg{}
		found, err := cli.Get(context.Background(), key, reg)
		return err == nil && found && reg.GetEpoch() > 0
	})
	// The etcd put and its 'worker registered' record are not atomic, so the
	// wait above can win the race against the log append. Wait for the record
	// too, then assert it appears exactly once (VW2 logs only the FIRST
	// successful put of a role).
	waitFor(t, "worker registered record", func() bool {
		return len(logs.withMsg(msgWorkerRegistered)) >= 1
	})
	if got := len(logs.withMsg(msgWorkerRegistered)); got != 1 {
		t.Fatalf("%d worker registered records, want 1", got)
	}
	waitFor(t, "own key observed live", func() bool {
		for _, rec := range logs.withMsg(msgMembershipObserved) {
			if rec["seed"] == seed && rec["state"] == stateLive &&
				rec["own"] == true {
				return true
			}
		}
		return false
	})
	waitFor(t, "membership committed", func() bool {
		for _, rec := range logs.withMsg(msgMembershipCommitted) {
			if rec["seed"] == seed && rec["state"] == stateMember {
				return true
			}
		}
		return false
	})
	waitFor(t, "every shard owned", func() bool {
		return len(logs.withMsg(msgShardOwned)) == common.ShardBucketSize
	})
	if got := len(logs.withMsg(msgWorkerFenced)); got != 0 {
		t.Fatalf("the worker fenced itself: %v", logs.withMsg(msgWorkerFenced))
	}

	cancel()
	<-done
	reg := &pb.WorkerReg{}
	found, err := cli.Get(context.Background(), key, reg)
	if err != nil {
		t.Fatalf("get after shutdown: %v", err)
	}
	if found {
		t.Fatalf("registration survived a graceful stop")
	}
	if got := len(logs.withMsg(msgShardReleased)); got != common.ShardBucketSize {
		t.Fatalf("%d shard released records, want %d",
			got, common.ShardBucketSize)
	}
}
