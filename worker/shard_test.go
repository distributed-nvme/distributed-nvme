package worker

import (
	"context"
	"errors"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A recording revision worker, so the shard tests exercise §7 alone
// ---------------------------------------------------------------------------

// fakeRevWorker records what a shard worker told one revision worker.
type fakeRevWorker struct {
	mu      sync.Mutex
	params  revWorkerParams
	updates []desiredState
	stopped int
}

func (w *fakeRevWorker) update(d desiredState) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.updates = append(w.updates, d)
}

func (w *fakeRevWorker) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped++
}

func (w *fakeRevWorker) snapshot() ([]desiredState, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]desiredState(nil), w.updates...), w.stopped
}

// revRecorder builds fakeRevWorkers and remembers them by object key.
type revRecorder struct {
	mu      sync.Mutex
	workers map[revObjKey]*fakeRevWorker
	starts  []revObjKey
}

func newRevRecorder() *revRecorder {
	return &revRecorder{workers: make(map[revObjKey]*fakeRevWorker)}
}

func (r *revRecorder) newWorker(p revWorkerParams) revWorkerHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := revObjKey{cid: p.cid, id: p.id}
	w := &fakeRevWorker{params: p}
	r.workers[key] = w
	r.starts = append(r.starts, key)
	return w
}

func (r *revRecorder) get(cid uint64, id uint64) *fakeRevWorker {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.workers[revObjKey{cid: cid, id: id}]
}

func (r *revRecorder) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.starts)
}

// recordingDnKind is the dn kind with a recording revision-worker
// constructor.
func recordingDnKind(rec *revRecorder) revKind {
	kind := dnKind()
	kind.newWorker = rec.newWorker
	return kind
}

// shardHarness runs one real shard worker over the in-memory store.
type shardHarness struct {
	t     *testing.T
	store *fakeStore
	clk   *fakeClock
	rec   *revRecorder
	deps  *deps
	sw    *shardWorker
}

const testShard = uint32(0x2a)

func newShardHarness(t *testing.T) *shardHarness {
	t.Helper()
	captureLogs(t)
	store := newFakeStore()
	clk := newFakeClock()
	rec := newRevRecorder()
	d := newTestDeps(testConfig(common.WorkerRoleDn), store, clk)
	return &shardHarness{t: t, store: store, clk: clk, rec: rec, deps: d}
}

func (h *shardHarness) start() {
	h.t.Helper()
	h.sw = startShardWorker(
		h.deps, recordingDnKind(h.rec), testShard, seedOf(1),
	)
	h.t.Cleanup(h.sw.stop)
	waitFor(h.t, "shard watch", func() bool { return h.store.watchCount() > 0 })
}

func (h *shardHarness) putRev(cid uint64, dnId uint64, rev *pb.DnRev) {
	h.t.Helper()
	err := h.store.Put(
		context.Background(), model.DnRevKey(testShard, cid, dnId), rev,
	)
	if err != nil {
		h.t.Fatalf("put rev: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestShardScanThenWatch checks SW2: the initial Range starts one revision
// worker per key with the value's (revision, handle), and the watch that
// follows starts one for a new key.
func TestShardScanThenWatch(t *testing.T) {
	h := newShardHarness(t)
	h.store.seed(t, model.DnRevKey(testShard, 1, 10),
		&pb.DnRev{AddrPort: "dn0:9520", Revision: 7})
	h.store.seed(t, model.DnRevKey(testShard, 1, 11),
		&pb.DnRev{AddrPort: "dn1:9520", Revision: 3})
	h.start()

	waitFor(t, "two workers", func() bool { return h.rec.startCount() == 2 })
	first := h.rec.get(1, 10)
	if first.params.desired != (desiredState{revision: 7, handle: "dn0:9520"}) {
		t.Fatalf("initial desired = %+v", first.params.desired)
	}
	if first.params.role != common.WorkerRoleDn ||
		first.params.shard != testShard || first.params.seed != seedOf(1) {
		t.Fatalf("params = %+v", first.params)
	}

	h.putRev(2, 20, &pb.DnRev{AddrPort: "dn2:9520", Revision: 1})
	waitFor(t, "third worker", func() bool { return h.rec.startCount() == 3 })
	third := h.rec.get(2, 20)
	if third.params.desired != (desiredState{revision: 1, handle: "dn2:9520"}) {
		t.Fatalf("new key desired = %+v", third.params.desired)
	}
}

// TestShardCoalescing checks SW3/RW3: a put that changes neither the revision
// nor the handle is a no-op, and one that changes either is an update on the
// existing revision worker rather than a restart.
func TestShardCoalescing(t *testing.T) {
	h := newShardHarness(t)
	h.store.seed(t, model.DnRevKey(testShard, 1, 10),
		&pb.DnRev{AddrPort: "dn0:9520", Revision: 7})
	h.start()
	waitFor(t, "worker", func() bool { return h.rec.startCount() == 1 })
	worker := h.rec.get(1, 10)

	// Identical put: nothing happens.
	h.putRev(1, 10, &pb.DnRev{AddrPort: "dn0:9520", Revision: 7})
	// A revision bump: one update.
	h.putRev(1, 10, &pb.DnRev{AddrPort: "dn0:9520", Revision: 8})
	// An endpoint move: one more update, same worker (RW13).
	h.putRev(1, 10, &pb.DnRev{AddrPort: "dn9:9520", Revision: 8})

	waitFor(t, "two updates", func() bool {
		updates, _ := worker.snapshot()
		return len(updates) == 2
	})
	updates, stopped := worker.snapshot()
	want := []desiredState{
		{revision: 8, handle: "dn0:9520"},
		{revision: 8, handle: "dn9:9520"},
	}
	for i := range want {
		if updates[i] != want[i] {
			t.Fatalf("update %d = %+v, want %+v", i, updates[i], want[i])
		}
	}
	if stopped != 0 {
		t.Fatalf("worker stopped %d times on an endpoint change", stopped)
	}
	if h.rec.startCount() != 1 {
		t.Fatalf("started %d workers, want 1", h.rec.startCount())
	}
}

// TestShardDeleteStops checks SW3: a delete stops the revision worker
// gracefully and forgets it.
func TestShardDeleteStops(t *testing.T) {
	h := newShardHarness(t)
	key := model.DnRevKey(testShard, 1, 10)
	h.store.seed(t, key, &pb.DnRev{AddrPort: "dn0:9520", Revision: 7})
	h.start()
	waitFor(t, "worker", func() bool { return h.rec.startCount() == 1 })
	worker := h.rec.get(1, 10)

	if err := h.store.Delete(context.Background(), key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	waitFor(t, "worker stopped", func() bool {
		_, stopped := worker.snapshot()
		return stopped == 1
	})
	// Forgotten: a later put starts a new one.
	h.putRev(1, 10, &pb.DnRev{AddrPort: "dn0:9520", Revision: 9})
	waitFor(t, "restarted", func() bool { return h.rec.startCount() == 2 })
}

// TestShardRescanDiffAfterCompaction checks SW4: after a compaction the shard
// rescans and diffs — starting unknown keys, updating changed ones and
// stopping the ones that are gone — then restarts the watch.
func TestShardRescanDiffAfterCompaction(t *testing.T) {
	h := newShardHarness(t)
	keepKey := model.DnRevKey(testShard, 1, 10)
	goneKey := model.DnRevKey(testShard, 1, 11)
	h.store.seed(t, keepKey, &pb.DnRev{AddrPort: "dn0:9520", Revision: 7})
	h.store.seed(t, goneKey, &pb.DnRev{AddrPort: "dn1:9520", Revision: 1})
	h.start()
	waitFor(t, "two workers", func() bool { return h.rec.startCount() == 2 })
	keep := h.rec.get(1, 10)
	gone := h.rec.get(1, 11)

	// Change the store behind the watch's back, then break the watch.
	h.store.seed(t, keepKey, &pb.DnRev{AddrPort: "dn0:9520", Revision: 12})
	h.store.mu.Lock()
	delete(h.store.kvs, goneKey)
	h.store.mu.Unlock()
	h.store.seed(t, model.DnRevKey(testShard, 3, 30),
		&pb.DnRev{AddrPort: "dn3:9520", Revision: 2})
	h.store.breakWatches(errors.New(
		"etcdserver: mvcc: required revision has been compacted",
	))

	waitFor(t, "rescan diff applied", func() bool {
		updates, _ := keep.snapshot()
		_, stopped := gone.snapshot()
		return len(updates) == 1 && stopped == 1 && h.rec.startCount() == 3
	})
	updates, _ := keep.snapshot()
	if updates[0] != (desiredState{revision: 12, handle: "dn0:9520"}) {
		t.Fatalf("rescan update = %+v", updates[0])
	}
	waitFor(t, "watch restarted", func() bool { return h.store.watchCount() > 0 })
}

// TestShardFailingRescanRetries checks SW4's "never a hot loop": a failing
// rescan is retried one vote interval later.
func TestShardFailingRescanRetries(t *testing.T) {
	h := newShardHarness(t)
	h.store.setRangeErr(errors.New("etcd down"))
	h.sw = startShardWorker(
		h.deps, recordingDnKind(h.rec), testShard, seedOf(1),
	)
	t.Cleanup(h.sw.stop)

	waitFor(t, "retry timer armed", func() bool {
		return h.clk.waiterCount() > 0
	})
	if h.store.watchCount() != 0 {
		t.Fatalf("a watch was opened after a failed scan")
	}
	h.store.setRangeErr(nil)
	h.store.seed(t, model.DnRevKey(testShard, 1, 10),
		&pb.DnRev{AddrPort: "dn0:9520", Revision: 7})
	h.clk.advance(h.deps.cfg.VoteInterval)
	waitFor(t, "rescan succeeded", func() bool {
		return h.rec.startCount() == 1 && h.store.watchCount() > 0
	})
}

// TestShardRejectsMalformedKeys checks SW2: a key this shard's kind cannot
// parse, or one belonging to another shard, is logged and skipped.
func TestShardRejectsMalformedKeys(t *testing.T) {
	h := newShardHarness(t)
	prefix := model.DnRevPrefix(testShard)
	// A key under the prefix with too few fields, and one whose id is not
	// hex.
	h.store.seed(t, prefix+"zzzz", &pb.DnRev{AddrPort: "x:1", Revision: 1})
	h.store.seed(t, prefix+"0000000000000001 zzzzzzzzzzzzzzzz",
		&pb.DnRev{AddrPort: "x:1", Revision: 1})
	h.store.seed(t, model.DnRevKey(testShard, 1, 10),
		&pb.DnRev{AddrPort: "dn0:9520", Revision: 7})
	h.start()

	waitFor(t, "the one good key", func() bool {
		return h.rec.startCount() == 1
	})
	if h.rec.get(1, 10) == nil {
		t.Fatalf("the well-formed key did not start a worker")
	}
}

// TestShardKindsMatchTheirPrefixes pins the three kinds of SW1 to the model
// key functions and message types.
func TestShardKindsMatchTheirPrefixes(t *testing.T) {
	cases := []struct {
		role   string
		prefix string
		msg    proto.Message
	}{
		{common.WorkerRoleDn, model.DnRevPrefix(3), &pb.DnRev{}},
		{common.WorkerRoleCn, model.CnRevPrefix(3), &pb.CnRev{}},
		{common.WorkerRoleSp, model.SpRevPrefix(3), &pb.SpRev{}},
	}
	for _, tc := range cases {
		kind, ok := kindFor(tc.role)
		if !ok {
			t.Fatalf("no kind for role %s", tc.role)
		}
		if got := kind.prefix(3); got != tc.prefix {
			t.Fatalf("%s prefix = %q, want %q", tc.role, got, tc.prefix)
		}
		if got := kind.newMsg(); got.ProtoReflect().Descriptor().FullName() !=
			tc.msg.ProtoReflect().Descriptor().FullName() {
			t.Fatalf("%s message = %T, want %T", tc.role, got, tc.msg)
		}
	}
	if _, ok := kindFor("nope"); ok {
		t.Fatalf("an unknown role resolved to a kind")
	}
	// SpRev's handle is the sp_name, not an addr_port.
	spKindValue, _ := kindFor(common.WorkerRoleSp)
	desired, ok := spKindValue.desired(&pb.SpRev{SpName: "sp0", Revision: 4})
	if !ok || desired != (desiredState{revision: 4, handle: "sp0"}) {
		t.Fatalf("sp desired = %+v, ok = %v", desired, ok)
	}
}
