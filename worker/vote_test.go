package worker

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Store with hooks
// ---------------------------------------------------------------------------

// voteHookStore wraps the in-memory fakeStore with the two hooks the VW8 cases
// need and nothing else: a one-shot gate that holds one Put until the test
// lets it through — a heartbeat frozen mid-tick, the SIGSTOP case of VW8(a) —
// and a callback that runs inside Delete, so a test can act while the vote
// loop is in the middle of the fence's own-registration cleanup. Disarmed it
// behaves exactly like the fakeStore it embeds.
type voteHookStore struct {
	*fakeStore

	mu        sync.Mutex
	gateKey   string
	gateArmed bool
	released  bool
	release   chan struct{}
	entered   chan struct{}
	left      chan struct{}
	onDelete  func(key string)
	ops       []string
}

func newVoteHookStore() *voteHookStore {
	return &voteHookStore{fakeStore: newFakeStore()}
}

// gatePut arms the one-shot gate on key: the next Put of it blocks until
// releasePut, or until its context ends — which is what a heartbeat put does
// when the fence cancels it. entered closes when the put arrives at the gate,
// left when it returns.
func (s *voteHookStore) gatePut(key string) (entered, left <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gateKey = key
	s.gateArmed = true
	s.released = false
	s.release = make(chan struct{})
	s.entered = make(chan struct{})
	s.left = make(chan struct{})
	return s.entered, s.left
}

// releasePut lets a gated put through. It is idempotent.
func (s *voteHookStore) releasePut() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.release == nil || s.released {
		return
	}
	s.released = true
	close(s.release)
}

// gateFor takes the armed gate for key; only the first put of it waits.
func (s *voteHookStore) gateFor(
	key string,
) (release chan struct{}, entered, left chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.gateArmed || s.gateKey != key {
		return nil, nil, nil
	}
	s.gateArmed = false
	return s.release, s.entered, s.left
}

// setOnDelete installs a callback that runs on the caller's goroutine right
// after every Delete.
func (s *voteHookStore) setOnDelete(fn func(key string)) {
	s.mu.Lock()
	s.onDelete = fn
	s.mu.Unlock()
}

// seedRaw writes a raw value without emitting a watch event, the way a value
// that no longer decodes as a WorkerReg would already sit in the registry.
func (s *voteHookStore) seedRaw(key string, value []byte) {
	s.fakeStore.mu.Lock()
	defer s.fakeStore.mu.Unlock()
	s.fakeStore.rev++
	s.fakeStore.kvs[key] = value
}

// opLog returns the successful writes in the order they happened.
func (s *voteHookStore) opLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ops...)
}

func (s *voteHookStore) record(op string) {
	s.mu.Lock()
	s.ops = append(s.ops, op)
	s.mu.Unlock()
}

func (s *voteHookStore) Put(
	ctx context.Context, key string, msg proto.Message,
) error {
	if release, entered, left := s.gateFor(key); release != nil {
		close(entered)
		defer close(left)
		select {
		case <-release:
		case <-ctx.Done():
			// The heartbeat context was cancelled while the put was in
			// flight, so nothing reaches etcd (VW8, CM5).
			return ctx.Err()
		}
	}
	if err := s.fakeStore.Put(ctx, key, msg); err != nil {
		return err
	}
	s.record("put " + key)
	return nil
}

func (s *voteHookStore) Delete(ctx context.Context, key string) error {
	err := s.fakeStore.Delete(ctx, key)
	s.record("delete " + key)
	s.mu.Lock()
	hook := s.onDelete
	s.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	return err
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// voteHarness runs a real vote worker over the in-memory registry and the
// fake clock. Registrations are delivered the way production delivers them —
// a store Put or Delete, which the store's watch turns into an event — so the
// tests exercise the scan, the watch pump and the state machines together.
type voteHarness struct {
	t     *testing.T
	clk   *fakeClock
	store *voteHookStore
	logs  *logCapture
	deps  *deps
	vote  *voteWorker

	cancel context.CancelFunc
	done   chan struct{}
}

func newVoteHarness(t *testing.T, roles ...string) *voteHarness {
	t.Helper()
	return newVoteHarnessOn(t, newVoteHookStore(), roles...)
}

// newVoteHarnessOn runs the vote worker over a store the caller has already
// prepared — keys that were in the registry before this worker started, or a
// hook it needs armed from the first tick.
func newVoteHarnessOn(
	t *testing.T,
	store *voteHookStore,
	roles ...string,
) *voteHarness {
	t.Helper()
	return newVoteHarnessCfg(t, testConfig(roles...), store)
}

// newVoteHarnessCfg runs the vote worker under a Config the caller chose. CM3
// makes a --vote-grace-time below the dead threshold legal — "a grace window
// shorter than the dead threshold is legal but pointless", warned about and
// never rejected — so §6 has to stay correct there too, and that is the range
// the own-key cases below need.
func newVoteHarnessCfg(
	t *testing.T,
	cfg Config,
	store *voteHookStore,
) *voteHarness {
	t.Helper()
	logs := captureLogs(t)
	clk := newFakeClock()
	d := newTestDeps(cfg, store.fakeStore, clk)
	d.store = store
	h := &voteHarness{
		t:     t,
		clk:   clk,
		store: store,
		logs:  logs,
		deps:  d,
		vote:  newVoteWorker(d, seedOf(1)),
		done:  make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		defer close(h.done)
		h.vote.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-h.done
	})
	// The first scan observes the worker's own registration (VW7).
	h.waitRecords(msgMembershipObserved, 1)
	h.settle()
	return h
}

// settle waits until the vote loop has drained everything the last stimulus
// produced.
func (h *voteHarness) settle() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	stable := 0
	for time.Now().Before(deadline) {
		before := h.vote.processed.Load()
		time.Sleep(time.Millisecond)
		if h.store.drained() && h.vote.processed.Load() == before {
			stable++
			if stable >= 3 {
				return
			}
			continue
		}
		stable = 0
	}
	h.t.Fatal("vote loop did not settle")
}

// advance moves the fake clock forward in steps of at most one vote interval,
// settling after each, the way production sees one heartbeat and its echo per
// interval rather than a single jump over several.
func (h *voteHarness) advance(d time.Duration) {
	h.t.Helper()
	step := h.deps.cfg.VoteInterval
	for d > 0 {
		chunk := step
		if d < chunk {
			chunk = d
		}
		h.clk.advance(chunk)
		h.settle()
		d -= chunk
	}
}

// beat puts a peer's registration, which its owner would do every interval.
func (h *voteHarness) beat(role string, seed string) {
	h.t.Helper()
	err := h.store.Put(
		context.Background(),
		workerRegKey(role, seed),
		&pb.WorkerReg{Epoch: h.clk.nowUnix()},
	)
	if err != nil {
		h.t.Fatalf("peer put: %v", err)
	}
	h.settle()
}

// beatUntilObserved re-puts a registration until the vote loop has logged an
// observed transition for it, which is what proves the incarnation the worker
// is running under RIGHT NOW has its registry watch delivering: after a fence
// (VW8) that watch is a new one and it opens asynchronously, so a single put
// aimed at the old one would simply be lost. Re-putting is free — a put of a
// key already observed live is a no-op for the state machines (VW3).
func (h *voteHarness) beatUntilObserved(role string, seed string) {
	h.t.Helper()
	waitFor(h.t, "an observed transition for "+seed, func() bool {
		err := h.store.Put(
			context.Background(),
			workerRegKey(role, seed),
			&pb.WorkerReg{Epoch: h.clk.nowUnix()},
		)
		if err != nil {
			h.t.Fatalf("peer put: %v", err)
		}
		for _, rec := range h.logs.withMsg(msgMembershipObserved) {
			if rec["role"] == role && rec["seed"] == seed {
				return true
			}
		}
		return false
	})
	h.settle()
}

// keepAlive beats a peer once per interval while the clock advances by d.
func (h *voteHarness) keepAlive(role string, seed string, d time.Duration) {
	h.t.Helper()
	step := h.deps.cfg.VoteInterval
	for d > 0 {
		chunk := step
		if d < chunk {
			chunk = d
		}
		h.beat(role, seed)
		h.advance(chunk)
		d -= chunk
	}
}

// waitClosed blocks until ch is closed, failing the test if that never
// happens.
func waitClosed(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func (h *voteHarness) waitRecords(msg string, n int) {
	h.t.Helper()
	waitFor(h.t, msg, func() bool { return len(h.logs.withMsg(msg)) >= n })
}

// committed returns the "membership committed" records of one seed.
func (h *voteHarness) committed(role string, seed string) []map[string]any {
	var out []map[string]any
	for _, rec := range h.logs.withMsg(msgMembershipCommitted) {
		if rec["role"] == role && rec["seed"] == seed {
			out = append(out, rec)
		}
	}
	return out
}

// observedStates returns the "state" attributes of one seed's observed
// transitions, in order.
func (h *voteHarness) observedStates(role string, seed string) []string {
	var out []string
	for _, rec := range h.logs.withMsg(msgMembershipObserved) {
		if rec["role"] == role && rec["seed"] == seed {
			state, _ := rec["state"].(string)
			out = append(out, state)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Tickets and ownership (VW9)
// ---------------------------------------------------------------------------

// TestVoteTicketGolden pins the VW9 ticket function against sha256 digests
// computed outside the package.
func TestVoteTicketGolden(t *testing.T) {
	seed := "00000001-0000-4000-8000-000000000001"
	cases := []struct {
		role  string
		shard uint32
		want  string
	}{
		{
			role:  common.WorkerRoleDn,
			shard: 0,
			want: "6365f9cffd6fadc98b660228c77ef5d4" +
				"caab4075d90747189b7376978eaeaef8",
		},
		{
			role:  common.WorkerRoleDn,
			shard: 255,
			want: "757557772c2b150c00b73e8b4d1e75e6" +
				"b9adc7876173a88fabe8500919a25935",
		},
		{
			role:  common.WorkerRoleCn,
			shard: 17,
			want: "362c04f4717c59fd5ed022d76e596fb2" +
				"3d85cecdfb3ffde35557e5caeb5cf1c6",
		},
	}
	for _, tc := range cases {
		ticket := voteTicket(seed, tc.role, tc.shard)
		if got := hex.EncodeToString(ticket[:]); got != tc.want {
			t.Fatalf("ticket(%s, %s, %d) = %s, want %s",
				seed, tc.role, tc.shard, got, tc.want)
		}
	}
	// The memoized per-registration tickets are the same function.
	entry := &regEntry{seed: seed}
	memo := entry.ticket(common.WorkerRoleDn, 0)
	if hex.EncodeToString(memo[:]) != cases[0].want {
		t.Fatalf("memoized ticket differs from voteTicket")
	}
}

// TestVoteOwnerLargestWins checks that ownerOf picks the largest ticket and
// that an empty membership owns nothing (VW9, VW11).
func TestVoteOwnerLargestWins(t *testing.T) {
	if owner := ownerOf(nil, common.WorkerRoleDn, 0); owner != "" {
		t.Fatalf("empty membership owns %q", owner)
	}
	members := []*regEntry{
		{seed: seedOf(1)},
		{seed: seedOf(2)},
		{seed: seedOf(3)},
	}
	for shard := uint32(0); shard < common.ShardBucketSize; shard++ {
		owner := ownerOf(members, common.WorkerRoleDn, shard)
		best := ""
		var bestTicket [32]byte
		for _, m := range members {
			ticket := voteTicket(m.seed, common.WorkerRoleDn, shard)
			if best == "" || string(ticket[:]) > string(bestTicket[:]) {
				best, bestTicket = m.seed, ticket
			}
		}
		if owner != best {
			t.Fatalf("shard %d owner %s, want %s", shard, owner, best)
		}
	}
}

// TestVoteOwnershipMovesOnlyToJoiner is the ~1/n movement property of VW9:
// adding a fourth member moves exactly the shards whose new largest ticket is
// the joiner's, and nothing else is reshuffled.
func TestVoteOwnershipMovesOnlyToJoiner(t *testing.T) {
	three := []*regEntry{
		{seed: seedOf(1)},
		{seed: seedOf(2)},
		{seed: seedOf(3)},
	}
	joiner := &regEntry{seed: seedOf(4)}
	four := append(append([]*regEntry(nil), three...), joiner)

	moved := 0
	for shard := uint32(0); shard < common.ShardBucketSize; shard++ {
		before := ownerOf(three, common.WorkerRoleSp, shard)
		after := ownerOf(four, common.WorkerRoleSp, shard)
		if before == after {
			continue
		}
		moved++
		if after != joiner.seed {
			t.Fatalf("shard %d moved from %s to %s, not to the joiner",
				shard, before, after)
		}
	}
	// sha256 is uniform, so a fourth member takes about a quarter. The bounds
	// are wide on purpose: this asserts "about 1/n", not an exact split.
	if moved < 30 || moved > 100 {
		t.Fatalf("joiner took %d of %d shards, want roughly a quarter",
			moved, common.ShardBucketSize)
	}
}

// ---------------------------------------------------------------------------
// Membership (VW3, VW5, VW6, VW7)
// ---------------------------------------------------------------------------

// TestVoteSymmetricStartup checks VW7: the effective membership starts empty,
// the worker's own key enters through an appear transition at scan time, and
// nothing is owned until one full grace window later.
func TestVoteSymmetricStartup(t *testing.T) {
	h := newVoteHarness(t, common.WorkerRoleDn)
	own := h.vote.currentSeed()

	if states := h.observedStates(common.WorkerRoleDn, own); len(states) != 1 ||
		states[0] != stateLive {
		t.Fatalf("own key observed %v, want one live appear", states)
	}
	// Just before the grace window ends nothing is committed and nothing is
	// owned.
	h.advance(h.deps.cfg.GraceTime - h.deps.cfg.VoteInterval)
	if got := len(h.committed(common.WorkerRoleDn, own)); got != 0 {
		t.Fatalf("committed %d times inside the grace window", got)
	}
	if got := len(h.logs.withMsg(msgShardOwned)); got != 0 {
		t.Fatalf("owned %d shards inside the grace window", got)
	}
	// One more interval closes it.
	h.advance(h.deps.cfg.VoteInterval)
	recs := h.committed(common.WorkerRoleDn, own)
	if len(recs) != 1 || recs[0]["state"] != stateMember {
		t.Fatalf("own commit = %v, want one member commit", recs)
	}
	waitFor(t, "all shards owned", func() bool {
		return len(h.logs.withMsg(msgShardOwned)) == common.ShardBucketSize
	})
}

// TestVoteDeadAtScanNeverEffective checks VW7's last clause: a key found by
// the scan whose owner is already dead never becomes effective — and is
// garbage-collected all the same, which is the stated purpose of the disappear
// timer VW7 arms for it.
func TestVoteDeadAtScanNeverEffective(t *testing.T) {
	store := newVoteHookStore()
	dead := seedOf(9)
	key := workerRegKey(common.WorkerRoleDn, dead)
	store.seed(t, key, &pb.WorkerReg{})
	h := newVoteHarnessOn(t, store, common.WorkerRoleDn)
	h.waitRecords(msgMembershipObserved, 2)
	h.settle()
	cfg := h.deps.cfg

	// The dead key appeared at scan time and disappears at scan + 2 x
	// interval, which cancels its pending appear.
	h.advance(2 * cfg.VoteInterval)
	if states := h.observedStates(common.WorkerRoleDn, dead); len(states) != 2 ||
		states[0] != stateLive || states[1] != stateDead {
		t.Fatalf("dead key observed %v, want live then dead", states)
	}
	// Well past the grace window it has still never been committed: nothing
	// about anybody's effective membership changed (VW5).
	h.advance(2 * cfg.GraceTime)
	if got := h.committed(common.WorkerRoleDn, dead); len(got) != 0 {
		t.Fatalf("dead-at-scan key committed %v", got)
	}
	// The worker itself, whose heartbeat keeps its key fresh, did commit, and
	// the never-effective key was never counted as a member.
	own := h.committed(common.WorkerRoleDn, h.vote.currentSeed())
	if len(own) != 1 {
		t.Fatalf("own commits = %v, want exactly one", own)
	}
	if cnt, _ := own[0]["member_cnt"].(float64); cnt != 1 {
		t.Fatalf("own commit member_cnt = %v, want 1", own[0]["member_cnt"])
	}
	// VW7's disappear timer still ran VW6's collection: without it nothing
	// ever removes the key of a worker that died inside its own grace window.
	waitFor(t, "gc delete of the dead-at-scan key", func() bool {
		for _, deleted := range h.store.deletedKeys() {
			if deleted == key {
				return true
			}
		}
		return false
	})
	if h.store.has(key) {
		t.Fatalf("%s survived the collection", key)
	}
}

// TestVoteAppearCommitDisappearGC walks the whole VW3/VW5/VW6 cycle for a
// peer: appear, commit member, stop heartbeating, disappear at the dead
// threshold, commit nonmember with the garbage-collecting Delete, and reappear
// as a fresh entry afterwards.
func TestVoteAppearCommitDisappearGC(t *testing.T) {
	h := newVoteHarness(t, common.WorkerRoleDn)
	peer := seedOf(7)
	key := workerRegKey(common.WorkerRoleDn, peer)

	h.beat(common.WorkerRoleDn, peer)
	h.keepAlive(common.WorkerRoleDn, peer, h.deps.cfg.GraceTime)
	recs := h.committed(common.WorkerRoleDn, peer)
	if len(recs) != 1 || recs[0]["state"] != stateMember {
		t.Fatalf("peer commit = %v, want one member commit", recs)
	}
	// Own and peer commit in the same grace window; whichever lands second
	// reports the full membership.
	maxCnt := 0.0
	for _, rec := range h.logs.withMsg(msgMembershipCommitted) {
		if cnt, ok := rec["member_cnt"].(float64); ok && cnt > maxCnt {
			maxCnt = cnt
		}
	}
	if maxCnt != 2 {
		t.Fatalf("largest member_cnt = %v, want 2", maxCnt)
	}

	// The peer crashes: its key stays, its puts stop, and 2 x interval later
	// every observer sees it disappear.
	h.advance(2 * h.deps.cfg.VoteInterval)
	if states := h.observedStates(common.WorkerRoleDn, peer); len(states) < 2 ||
		states[len(states)-1] != stateDead {
		t.Fatalf("peer observed %v, want a trailing dead", states)
	}
	if got := len(h.committed(common.WorkerRoleDn, peer)); got != 1 {
		t.Fatalf("peer committed %d times before its grace window closed", got)
	}

	// The grace window closes: nonmember, and the key is garbage-collected.
	h.advance(h.deps.cfg.GraceTime)
	recs = h.committed(common.WorkerRoleDn, peer)
	if len(recs) != 2 || recs[1]["state"] != stateNonmember {
		t.Fatalf("peer commits = %v, want member then nonmember", recs)
	}
	waitFor(t, "gc delete", func() bool {
		for _, deleted := range h.store.deletedKeys() {
			if deleted == key {
				return true
			}
		}
		return false
	})
	// VW6 drops the tracking entry, so a later put appears afresh.
	h.beat(common.WorkerRoleDn, peer)
	states := h.observedStates(common.WorkerRoleDn, peer)
	if states[len(states)-1] != stateLive {
		t.Fatalf("peer re-put observed %v, want a trailing live", states)
	}
}

// TestVoteFlappingNeverCommits checks VW5: a registration that flaps faster
// than the grace time never changes anybody's effective membership.
func TestVoteFlappingNeverCommits(t *testing.T) {
	h := newVoteHarness(t, common.WorkerRoleDn)
	peer := seedOf(5)
	key := workerRegKey(common.WorkerRoleDn, peer)

	for i := 0; i < 4; i++ {
		h.beat(common.WorkerRoleDn, peer)
		h.advance(h.deps.cfg.VoteInterval)
		if err := h.store.Delete(context.Background(), key); err != nil {
			t.Fatalf("delete: %v", err)
		}
		h.settle()
		h.advance(h.deps.cfg.VoteInterval)
	}
	h.advance(2 * h.deps.cfg.GraceTime)
	if got := h.committed(common.WorkerRoleDn, peer); len(got) != 0 {
		t.Fatalf("flapping key committed %v", got)
	}
	// It did flap: four appears and four disappears were observed.
	states := h.observedStates(common.WorkerRoleDn, peer)
	if len(states) != 8 {
		t.Fatalf("observed %v, want four appear/disappear pairs", states)
	}
}

// TestVoteReappearCancelsPendingCommit checks the cancel-on-transition rule of
// VW5: a member that disappears and comes back inside its grace window is never
// re-committed. The reappear cancels the pending nonmember timer and — per the
// VW7-over-VW5 resolution the implementation documents at observed() — DOES
// arm a timer of its own; the commit that timer fires is simply a no-op,
// because its target already equals the committed state, so no second
// "membership committed" record for the peer is ever emitted.
func TestVoteReappearCancelsPendingCommit(t *testing.T) {
	h := newVoteHarness(t, common.WorkerRoleDn)
	peer := seedOf(6)

	h.beat(common.WorkerRoleDn, peer)
	h.keepAlive(common.WorkerRoleDn, peer, h.deps.cfg.GraceTime)
	if got := len(h.committed(common.WorkerRoleDn, peer)); got != 1 {
		t.Fatalf("peer commits = %d, want 1", got)
	}
	// Stop beating: the deadline fires and a nonmember timer starts.
	h.advance(2 * h.deps.cfg.VoteInterval)
	// Come back well inside the grace window.
	h.beat(common.WorkerRoleDn, peer)
	h.keepAlive(common.WorkerRoleDn, peer, 2*h.deps.cfg.GraceTime)
	if got := h.committed(common.WorkerRoleDn, peer); len(got) != 1 {
		t.Fatalf("peer commits = %v, want only the original member commit", got)
	}
}

// TestVoteRoleIndependence checks VW10: each role has its own registry,
// entries, timers, effective set and shard workers.
func TestVoteRoleIndependence(t *testing.T) {
	h := newVoteHarness(t, common.WorkerRoleDn, common.WorkerRoleCn)
	peer := seedOf(8)

	h.beat(common.WorkerRoleDn, peer)
	h.keepAlive(common.WorkerRoleDn, peer, h.deps.cfg.GraceTime)

	if got := len(h.committed(common.WorkerRoleDn, peer)); got != 1 {
		t.Fatalf("dn peer commits = %d, want 1", got)
	}
	if got := len(h.committed(common.WorkerRoleCn, peer)); got != 0 {
		t.Fatalf("cn saw the dn-only peer commit %d times", got)
	}
	// The cn role owns every shard (one member); the dn role owns strictly
	// fewer, because the peer took some.
	own := h.vote.currentSeed()
	counts := map[string]int{}
	for _, rec := range h.logs.withMsg(msgShardOwned) {
		if rec["seed"] != own {
			continue
		}
		role, _ := rec["role"].(string)
		counts[role]++
	}
	waitFor(t, "cn owns everything", func() bool {
		n := 0
		for _, rec := range h.logs.withMsg(msgShardOwned) {
			if rec["role"] == common.WorkerRoleCn {
				n++
			}
		}
		return n == common.ShardBucketSize
	})
	counts = map[string]int{}
	released := map[string]int{}
	for _, rec := range h.logs.withMsg(msgShardOwned) {
		role, _ := rec["role"].(string)
		counts[role]++
	}
	for _, rec := range h.logs.withMsg(msgShardReleased) {
		role, _ := rec["role"].(string)
		released[role]++
	}
	if counts[common.WorkerRoleCn] != common.ShardBucketSize {
		t.Fatalf("cn owned %d shards, want %d",
			counts[common.WorkerRoleCn], common.ShardBucketSize)
	}
	if released[common.WorkerRoleCn] != 0 {
		t.Fatalf("cn released %d shards, want none",
			released[common.WorkerRoleCn])
	}
	dnHeld := counts[common.WorkerRoleDn] - released[common.WorkerRoleDn]
	if dnHeld <= 0 || dnHeld >= common.ShardBucketSize {
		t.Fatalf("dn holds %d shards, want a strict subset", dnHeld)
	}
}

// TestVoteNeverEffectiveKeyIsCollected is VW7's garbage collection for a
// registration that appears and dies INSIDE its own grace window, so it never
// becomes effective: the disappear still has to arm a timer whose commit runs
// VW6's Delete, because without a lease nothing else ever removes the key and
// every observer would keep a tracking entry for it forever.
func TestVoteNeverEffectiveKeyIsCollected(t *testing.T) {
	h := newVoteHarness(t, common.WorkerRoleDn)
	peer := seedOf(4)
	key := workerRegKey(common.WorkerRoleDn, peer)
	cfg := h.deps.cfg

	// One registration put, then the peer is never heard from again.
	h.beat(common.WorkerRoleDn, peer)
	h.advance(2*cfg.VoteInterval + cfg.GraceTime + cfg.VoteInterval)

	if states := h.observedStates(common.WorkerRoleDn, peer); len(states) != 2 ||
		states[0] != stateLive || states[1] != stateDead {
		t.Fatalf("peer observed %v, want live then dead", states)
	}
	// The commit is a no-op for membership (VW5): nothing was ever effective,
	// so nothing is logged and no member_cnt moved.
	if got := h.committed(common.WorkerRoleDn, peer); len(got) != 0 {
		t.Fatalf("never-effective key committed %v", got)
	}
	own := h.committed(common.WorkerRoleDn, h.vote.currentSeed())
	if len(own) != 1 {
		t.Fatalf("own commits = %v, want exactly one", own)
	}
	if cnt, _ := own[0]["member_cnt"].(float64); cnt != 1 {
		t.Fatalf("own commit member_cnt = %v, want 1", own[0]["member_cnt"])
	}
	// But the VW6 collection did run.
	waitFor(t, "gc delete", func() bool {
		for _, deleted := range h.store.deletedKeys() {
			if deleted == key {
				return true
			}
		}
		return false
	})
	if h.store.has(key) {
		t.Fatalf("%s leaked in the registry", key)
	}
	// VW6 dropped the tracking entry with it, so a later put appears afresh.
	h.beat(common.WorkerRoleDn, peer)
	states := h.observedStates(common.WorkerRoleDn, peer)
	if len(states) != 3 || states[2] != stateLive {
		t.Fatalf("peer re-put observed %v, want a trailing live", states)
	}
}

// TestVoteScanKeepsUndecodableValueLive is VW3 with VW4: a key FOUND by a scan
// is live, whatever its value holds — the vote layer never reads the stored
// epoch. Treating an undecodable value as an absent key would turn a healthy
// peer into a disappear, commit it nonmember, delete its registration and so
// fence it through VW8(c) over a field nobody reads.
func TestVoteScanKeepsUndecodableValueLive(t *testing.T) {
	// Field 1, varint, no payload: proto cannot decode it as a WorkerReg.
	corrupt := []byte{0x08}
	store := newVoteHookStore()
	scanned := seedOf(2)
	store.seedRaw(workerRegKey(common.WorkerRoleDn, scanned), corrupt)
	h := newVoteHarnessOn(t, store, common.WorkerRoleDn)
	h.waitRecords(msgMembershipObserved, 2)
	h.settle()

	if states := h.observedStates(common.WorkerRoleDn, scanned); len(states) != 1 ||
		states[0] != stateLive {
		t.Fatalf("scanned key observed %v, want one live appear", states)
	}

	// The same on the rescan of VW3's last paragraph: a peer that is live and
	// still present must not disappear because its value went bad.
	watched := seedOf(3)
	h.beat(common.WorkerRoleDn, watched)
	store.seedRaw(workerRegKey(common.WorkerRoleDn, watched), corrupt)
	h.store.breakWatches(errors.New("mvcc: required revision has been compacted"))
	waitFor(t, "the rescan's watch", func() bool {
		return h.store.watchCount() == 1
	})
	h.settle()
	if states := h.observedStates(common.WorkerRoleDn, watched); len(states) != 1 ||
		states[0] != stateLive {
		t.Fatalf("rescanned key observed %v, want one live appear", states)
	}
	for _, deleted := range h.store.deletedKeys() {
		if deleted == workerRegKey(common.WorkerRoleDn, watched) {
			t.Fatalf("a live peer's registration was collected")
		}
	}
}

// ---------------------------------------------------------------------------
// Self-fence (VW8)
// ---------------------------------------------------------------------------

// fenceRecord returns the single "worker fenced" record, failing otherwise.
func fenceRecord(t *testing.T, h *voteHarness) map[string]any {
	t.Helper()
	recs := h.logs.withMsg(msgWorkerFenced)
	if len(recs) == 0 {
		t.Fatalf("no worker fenced record")
	}
	return recs[0]
}

// TestVoteFenceHeartbeatStalled is VW8 (a): the heartbeat has not reached etcd
// for the dead threshold.
func TestVoteFenceHeartbeatStalled(t *testing.T) {
	h := newVoteHarness(t, common.WorkerRoleDn)
	old := h.vote.currentSeed()
	h.store.setPutErr(errors.New("etcd down"))
	h.advance(2 * h.deps.cfg.VoteInterval)
	waitFor(t, "fence", func() bool {
		return len(h.logs.withMsg(msgWorkerFenced)) >= 1
	})
	rec := fenceRecord(t, h)
	if rec["reason"] != fenceHeartbeatStalled {
		t.Fatalf("reason = %v, want %s", rec["reason"], fenceHeartbeatStalled)
	}
	if rec["old_seed"] != old {
		t.Fatalf("old_seed = %v, want %s", rec["old_seed"], old)
	}
	newSeedStr, _ := rec["new_seed"].(string)
	if newSeedStr == "" || newSeedStr == old {
		t.Fatalf("new_seed = %q, want a fresh seed", newSeedStr)
	}
	waitFor(t, "new incarnation", func() bool {
		return h.vote.currentSeed() == newSeedStr
	})
}

// TestVoteFenceHeartbeatStalledAfterFreeze is the case VW8(a) names and
// Appendix A walks at t=800: a process stopped by SIGSTOP (or a paused VM)
// resumes after more than the dead threshold and its catch-up put SUCCEEDS.
// The gap that fences it is the one measured against the last tick that
// reached etcd, so it MUST be evaluated before this tick's success is folded
// in; and the reason is heartbeat_stalled — the cause — not the stale watch
// the freeze also left behind (§14.11 case E step 7).
func TestVoteFenceHeartbeatStalledAfterFreeze(t *testing.T) {
	store := newVoteHookStore()
	h := newVoteHarnessOn(t, store, common.WorkerRoleDn)
	old := h.vote.currentSeed()
	interval := h.deps.cfg.VoteInterval

	// The freeze: the tick's put is held inside the store and no event
	// reaches the loop, exactly as while the process is stopped.
	entered, _ := store.gatePut(workerRegKey(common.WorkerRoleDn, old))
	h.store.setMuteEvents(true)
	h.clk.advance(interval)
	waitClosed(t, "the heartbeat put to reach the store", entered)
	// The monotonic clock runs on while the process is frozen (VW4).
	h.clk.advance(2 * interval)
	h.settle()
	if got := len(h.logs.withMsg(msgWorkerFenced)); got != 0 {
		t.Fatalf("fenced %d times while frozen, before any tick landed", got)
	}

	// SIGCONT: the pending tick's put lands and succeeds.
	store.releasePut()
	waitFor(t, "fence", func() bool {
		return len(h.logs.withMsg(msgWorkerFenced)) >= 1
	})
	rec := fenceRecord(t, h)
	if rec["reason"] != fenceHeartbeatStalled {
		t.Fatalf("reason = %v, want %s", rec["reason"], fenceHeartbeatStalled)
	}
	if rec["old_seed"] != old {
		t.Fatalf("old_seed = %v, want %s", rec["old_seed"], old)
	}
	newSeedStr, _ := rec["new_seed"].(string)
	if newSeedStr == "" || newSeedStr == old {
		t.Fatalf("new_seed = %q, want a fresh seed", newSeedStr)
	}
	waitFor(t, "new incarnation", func() bool {
		return h.vote.currentSeed() == newSeedStr
	})
}

// TestVoteRescanIsNotAWatchEcho is VW8(b)'s clock: only a put EVENT for this
// worker's own key is an echo. A Range result is not one, so a rescan — which
// this worker drives itself — must not refresh it, or a watch that has stopped
// delivering would be masked by every compaction rescan.
func TestVoteRescanIsNotAWatchEcho(t *testing.T) {
	h := newVoteHarness(t, common.WorkerRoleDn)
	interval := h.deps.cfg.VoteInterval

	// The watch stops delivering while the puts keep succeeding.
	h.store.setMuteEvents(true)
	h.advance(interval)
	// A compaction restarts the role's watch through a full rescan; the scan
	// finds this worker's own key.
	h.store.breakWatches(errors.New("mvcc: required revision has been compacted"))
	waitFor(t, "the rescan's watch", func() bool {
		return h.store.watchCount() == 1
	})
	h.settle()
	if got := len(h.logs.withMsg(msgWorkerFenced)); got != 0 {
		t.Fatalf("fenced %d times one interval after the last echo", got)
	}

	// One more interval and the last echo is 2 x interval old.
	h.advance(interval)
	waitFor(t, "fence", func() bool {
		return len(h.logs.withMsg(msgWorkerFenced)) >= 1
	})
	if rec := fenceRecord(t, h); rec["reason"] != fenceWatchStalled {
		t.Fatalf("reason = %v, want %s", rec["reason"], fenceWatchStalled)
	}
}

// TestVoteFenceWatchStalled is VW8 (b): the worker's own puts succeed but its
// own watch stops echoing them.
func TestVoteFenceWatchStalled(t *testing.T) {
	h := newVoteHarness(t, common.WorkerRoleDn)
	old := h.vote.currentSeed()
	h.store.setMuteEvents(true)
	h.advance(2 * h.deps.cfg.VoteInterval)
	waitFor(t, "fence", func() bool {
		return len(h.logs.withMsg(msgWorkerFenced)) >= 1
	})
	rec := fenceRecord(t, h)
	if rec["reason"] != fenceWatchStalled {
		t.Fatalf("reason = %v, want %s", rec["reason"], fenceWatchStalled)
	}
	if rec["old_seed"] != old || rec["new_seed"] == old {
		t.Fatalf("seeds = %v / %v", rec["old_seed"], rec["new_seed"])
	}
}

// TestVoteFenceKeyDeleted is VW8 (c): a peer committed this worker dead and
// deleted its key. The worker's own shutdown and fence deletes must NOT
// trigger it, which is why self-issued deletes are tracked.
func TestVoteFenceKeyDeleted(t *testing.T) {
	h := newVoteHarness(t, common.WorkerRoleDn)
	old := h.vote.currentSeed()
	err := h.store.Delete(
		context.Background(), workerRegKey(common.WorkerRoleDn, old),
	)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	waitFor(t, "fence", func() bool {
		return len(h.logs.withMsg(msgWorkerFenced)) >= 1
	})
	rec := fenceRecord(t, h)
	if rec["reason"] != fenceKeyDeleted {
		t.Fatalf("reason = %v, want %s", rec["reason"], fenceKeyDeleted)
	}
	newSeedStr, _ := rec["new_seed"].(string)
	if newSeedStr == old || newSeedStr == "" {
		t.Fatalf("new_seed = %q, want a fresh seed", newSeedStr)
	}
	waitFor(t, "new incarnation registers", func() bool {
		return h.store.has(workerRegKey(common.WorkerRoleDn, newSeedStr))
	})
}

// staleOwnKeyConfig is the CM3-legal configuration the own-key cases need: a
// grace window that closes before the next heartbeat tick can run VW8's checks,
// so a commit for the worker's OWN registration is reached first. CM3 calls a
// grace window below the dead threshold "legal but pointless" and only warns
// (cmd/dnv-worker); the shipped defaults (10/60) never reach it, which is why
// the rest of this file cannot see what it exposes.
func staleOwnKeyConfig() Config {
	return Config{
		Roles:        []string{common.WorkerRoleDn},
		VoteInterval: 10 * time.Second,
		GraceTime:    time.Second,
		Endpoints:    []string{"127.0.0.1:2379"},
	}
}

// staleOwnKey drives one worker to the moment its own registration would be
// committed nonmember: it owns every shard and every put still succeeds, but
// its own watch has stopped echoing them, so the liveness deadline of its OWN
// key (VW3) fires a full grace window before the first heartbeat tick that
// could apply VW8(b). It returns the harness and the seed it started under.
func staleOwnKey(t *testing.T) (*voteHarness, string) {
	t.Helper()
	store := newVoteHookStore()
	h := newVoteHarnessCfg(t, staleOwnKeyConfig(), store)
	own := h.vote.currentSeed()
	key := workerRegKey(common.WorkerRoleDn, own)

	// t=1: the own key's grace window closes and it takes every shard.
	h.advance(h.deps.cfg.GraceTime)
	waitFor(t, "all shards owned", func() bool {
		return len(h.logs.withMsg(msgShardOwned)) == common.ShardBucketSize
	})
	// Push the last own-key ECHO off the tick grid: the t=10 tick's put is
	// held and lands at t=15, so lastSeen = lastOwnEcho = 15 and the own key's
	// deadline (35) falls strictly before the first tick that can trip VW8(b)
	// (40).
	entered, left := store.gatePut(key)
	h.advance(9 * time.Second)
	waitClosed(t, "the held put to reach the store", entered)
	h.clk.advance(5 * time.Second)
	store.releasePut()
	waitClosed(t, "the held put to return", left)
	h.settle()

	// From here the watch delivers nothing while every put keeps succeeding.
	h.store.setMuteEvents(true)
	h.advance(5 * time.Second)  // t=20: a tick, no echo
	h.advance(10 * time.Second) // t=30: a tick, no echo
	h.advance(5 * time.Second)  // t=35: the own key's deadline fires
	h.advance(time.Second)      // t=36: its grace window would commit

	// Wait for that commit to have been APPLIED, whichever way it goes — a
	// fence (VW8) or the collection of the own key — so the caller never races
	// the loop draining a timer that has already fired.
	waitFor(t, "the own key's grace commit", func() bool {
		return len(h.logs.withMsg(msgWorkerFenced)) > 0 || !h.store.has(key)
	})
	return h, own
}

// TestVoteOwnKeyIsNeverSelfCollected pins the one registration VW6's garbage
// collection must never touch: this worker's own. VW6 collects "a key whose
// owner died" and VW8 says a worker the fleet gave up on is a NEW worker with a
// fresh seed; deleting the key this process's own heartbeat is still refreshing
// is neither. It would drop the worker out of its own effective set
// (member_cnt 0), release every shard and leave the next tick to re-create the
// key — a delete and an appear for a seed that never stopped running — with no
// "worker fenced" record, which is what §12 and §14 read a departure from.
func TestVoteOwnKeyIsNeverSelfCollected(t *testing.T) {
	h, own := staleOwnKey(t)

	waitFor(t, "fence", func() bool {
		return len(h.logs.withMsg(msgWorkerFenced)) >= 1
	})
	rec := fenceRecord(t, h)
	// The reason names the cause: the puts landed, the watch stopped echoing
	// them (VW8b).
	if rec["reason"] != fenceWatchStalled {
		t.Fatalf("reason = %v, want %s", rec["reason"], fenceWatchStalled)
	}
	if rec["old_seed"] != own {
		t.Fatalf("old_seed = %v, want %s", rec["old_seed"], own)
	}
	for _, c := range h.committed(common.WorkerRoleDn, own) {
		if c["state"] == stateNonmember {
			t.Fatalf("the worker committed ITSELF nonmember: %v", c)
		}
	}
	// It is a full VW8 rejoin, not a silent un-registration: the shards are
	// released, a fresh seed registers, and the old key is gone through the
	// fence's own cleanup.
	if got := len(h.logs.withMsg(msgShardReleased)); got != common.ShardBucketSize {
		t.Fatalf("released %d shards, want %d", got, common.ShardBucketSize)
	}
	newSeedStr, _ := rec["new_seed"].(string)
	if newSeedStr == "" || newSeedStr == own {
		t.Fatalf("new_seed = %q, want a fresh seed", newSeedStr)
	}
	waitFor(t, "the new incarnation to register", func() bool {
		return h.store.has(workerRegKey(common.WorkerRoleDn, newSeedStr))
	})
	if h.store.has(workerRegKey(common.WorkerRoleDn, own)) {
		t.Fatalf("the fenced seed is still registered")
	}
}

// TestVoteOwnKeyDeleteAlwaysFences is VW8(c) with nothing left that could
// swallow it. A worker that has itself deleted one of its own registrations
// earlier must still fence when a PEER deletes the key it is running under:
// Appendix B's split-brain bound assumes the peer the fleet gave up on rejoins
// with a fresh identity, and a missed (c) leaves two workers driving the same
// shards under seeds both of them believe are live.
func TestVoteOwnKeyDeleteAlwaysFences(t *testing.T) {
	h, _ := staleOwnKey(t)

	// The watch comes back. A peer registration is the probe: once it has been
	// observed, the incarnation the worker is running under NOW is delivering
	// events again, so the delete below cannot be lost against a watch that
	// has not opened yet.
	h.store.setMuteEvents(false)
	h.beatUntilObserved(common.WorkerRoleDn, seedOf(42))

	live := h.vote.currentSeed()
	liveKey := workerRegKey(common.WorkerRoleDn, live)
	// One put of that key, the way its own next heartbeat tick would refresh
	// it, so the observer has a live tracking entry for it either way.
	h.beat(common.WorkerRoleDn, live)
	before := len(h.logs.withMsg(msgWorkerFenced))

	// A peer commits this worker dead and garbage-collects its key (VW6).
	if err := h.store.Delete(context.Background(), liveKey); err != nil {
		t.Fatalf("peer delete: %v", err)
	}
	waitFor(t, "the VW8(c) fence", func() bool {
		return len(h.logs.withMsg(msgWorkerFenced)) > before
	})
	rec := h.logs.withMsg(msgWorkerFenced)[before]
	if rec["reason"] != fenceKeyDeleted {
		t.Fatalf("reason = %v, want %s", rec["reason"], fenceKeyDeleted)
	}
	if rec["old_seed"] != live {
		t.Fatalf("old_seed = %v, want %s", rec["old_seed"], live)
	}
}

// TestVoteDeferredFenceIsRetried is VW8's recovery when a fence cannot be
// applied because minting the new seed failed (VW1). Re-testing VW8's
// conditions only brings (a) and (b) back — checkFence reads lastOkPut and
// lastOwnEcho, never "a peer deleted my key" — and the caller that raised (c)
// has already consumed the delete event that carried it, so a dropped (c)
// fence would be lost for good.
//
// crypto/rand.Read does not fail today, so the deferral is driven directly.
// Every call below runs on this goroutine, exactly where the vote loop would
// run it, so the incarnation is touched by nobody else.
func TestVoteDeferredFenceIsRetried(t *testing.T) {
	logs := captureLogs(t)
	clk := newFakeClock()
	store := newVoteHookStore()
	d := newTestDeps(testConfig(common.WorkerRoleDn), store.fakeStore, clk)
	d.store = store
	v := newVoteWorker(d, seedOf(1))
	v.startIncarnation(seedOf(1))
	t.Cleanup(func() {
		if v.inc == nil {
			return
		}
		v.stopHeartbeat(v.inc)
		v.stopManagers(v.inc)
		v.discard(v.inc)
		v.inc = nil
	})
	inc := v.inc

	// Neither (a) nor (b) holds: the incarnation has just registered.
	if v.checkFence(inc, clk.now()) {
		t.Fatalf("fenced a healthy incarnation")
	}
	// VW8(c) fired and the mint failed.
	inc.deferFence(fenceKeyDeleted)
	if !v.checkFence(inc, clk.now()) {
		t.Fatalf("the deferred fence was never retried")
	}
	recs := logs.withMsg(msgWorkerFenced)
	if len(recs) != 1 {
		t.Fatalf("fenced %d times, want exactly 1", len(recs))
	}
	if recs[0]["reason"] != fenceKeyDeleted {
		t.Fatalf("reason = %v, want %s", recs[0]["reason"], fenceKeyDeleted)
	}
	if recs[0]["old_seed"] != seedOf(1) {
		t.Fatalf("old_seed = %v, want %s", recs[0]["old_seed"], seedOf(1))
	}
	// The rejoin is a clean slate (VW8): the new incarnation carries no
	// deferral of its own and does not fence again.
	if v.inc == nil || v.inc.seed == seedOf(1) {
		t.Fatalf("the fence did not start a new incarnation")
	}
	if v.inc.pendingFence != "" {
		t.Fatalf("the new incarnation inherited a deferred fence")
	}
	if v.checkFence(v.inc, clk.now()) {
		t.Fatalf("the new incarnation fenced immediately")
	}
}

// TestVoteFenceRejoinsWhileWatchesTearDown exercises the interleaving the §13
// harness used to dodge by muting the fake store: a fence cancels the old
// incarnation's watches (VW8) while its new incarnation is already putting its
// first registrations (VW2). Nothing may deliver an event into a channel that
// is being closed, so this is a -race test — un-muted, and with two roles, so
// two watches tear down against the rejoin's two puts.
func TestVoteFenceRejoinsWhileWatchesTearDown(t *testing.T) {
	store := newVoteHookStore()
	h := newVoteHarnessOn(t, store, common.WorkerRoleDn, common.WorkerRoleCn)
	old := h.vote.currentSeed()
	interval := h.deps.cfg.VoteInterval

	// A tick's put is held while the monotonic clock runs past the dead
	// threshold, so releasing it fences VW8(a) with every watch still open and
	// still delivering (Appendix A t=800).
	entered, _ := store.gatePut(workerRegKey(common.WorkerRoleDn, old))
	h.clk.advance(interval)
	waitClosed(t, "the heartbeat put to reach the store", entered)
	h.clk.advance(2 * interval)
	h.settle()
	store.releasePut()

	waitFor(t, "fence", func() bool {
		return len(h.logs.withMsg(msgWorkerFenced)) >= 1
	})
	newSeedStr, _ := fenceRecord(t, h)["new_seed"].(string)
	for _, role := range []string{common.WorkerRoleDn, common.WorkerRoleCn} {
		key := workerRegKey(role, newSeedStr)
		waitFor(t, "the new incarnation to register "+role, func() bool {
			return h.store.has(key)
		})
	}
	h.settle()
}

// TestVoteFenceReleasesShardsBeforeRejoin checks the §14 property: a fenced
// worker logs "shard released" for every shard it held BEFORE anything runs
// under the new seed.
func TestVoteFenceReleasesShardsBeforeRejoin(t *testing.T) {
	h := newVoteHarness(t, common.WorkerRoleDn)
	old := h.vote.currentSeed()
	// Own everything first.
	h.advance(h.deps.cfg.GraceTime)
	waitFor(t, "all shards owned", func() bool {
		return len(h.logs.withMsg(msgShardOwned)) == common.ShardBucketSize
	})
	// A peer deletes the key.
	err := h.store.Delete(
		context.Background(), workerRegKey(common.WorkerRoleDn, old),
	)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	waitFor(t, "all shards released", func() bool {
		return len(h.logs.withMsg(msgShardReleased)) == common.ShardBucketSize
	})
	order := h.logs.msgOrder(
		msgWorkerFenced, msgShardReleased, msgWorkerRegistered,
	)
	fencedAt := -1
	lastRelease := -1
	rejoinAt := -1
	registered := 0
	for i, msg := range order {
		switch msg {
		case msgWorkerFenced:
			fencedAt = i
		case msgShardReleased:
			lastRelease = i
		case msgWorkerRegistered:
			registered++
			if registered == 2 {
				rejoinAt = i
			}
		}
	}
	if fencedAt < 0 || lastRelease < 0 {
		t.Fatalf("missing records in %v", order)
	}
	if fencedAt > lastRelease {
		t.Fatalf("fence logged after the last release")
	}
	if rejoinAt >= 0 && rejoinAt < lastRelease {
		t.Fatalf("the new incarnation registered before the last release")
	}
}

// TestVoteFenceStopsHeartbeatBeforeDeletingRegs checks the order of VW8's
// fence procedure: the old incarnation's heartbeat is stopped and joined
// BEFORE its registrations are deleted. Otherwise a tick that is still in
// flight re-creates a key the fence has just deleted, and the peers see an
// appear for a seed that no longer exists.
func TestVoteFenceStopsHeartbeatBeforeDeletingRegs(t *testing.T) {
	store := newVoteHookStore()
	h := newVoteHarnessOn(t, store, common.WorkerRoleDn, common.WorkerRoleCn)
	old := h.vote.currentSeed()
	dnKey := workerRegKey(common.WorkerRoleDn, old)
	cnKey := workerRegKey(common.WorkerRoleCn, old)

	// A heartbeat tick is in flight, held inside its dn put.
	entered, left := store.gatePut(dnKey)
	h.clk.advance(h.deps.cfg.VoteInterval)
	waitClosed(t, "the heartbeat put to reach the store", entered)

	// When the fence deletes the dn registration, let that put through and
	// wait for it. With VW8's order the put was abandoned the moment the
	// heartbeat was cancelled, so nothing is written; without it, it lands
	// after the delete and resurrects the key.
	var once sync.Once
	store.setOnDelete(func(key string) {
		if key != dnKey {
			return
		}
		once.Do(func() {
			store.releasePut()
			select {
			case <-left:
			case <-time.After(5 * time.Second):
			}
		})
	})

	// A peer deletes the CN key: VW8(c) fences. The DN key is untouched, so
	// the hook above only ever runs for the fence's own cleanup.
	if err := h.store.Delete(context.Background(), cnKey); err != nil {
		t.Fatalf("peer delete: %v", err)
	}
	waitFor(t, "fence", func() bool {
		return len(h.logs.withMsg(msgWorkerFenced)) >= 1
	})
	newSeedStr, _ := fenceRecord(t, h)["new_seed"].(string)
	waitFor(t, "the new incarnation to register", func() bool {
		return h.store.has(workerRegKey(common.WorkerRoleDn, newSeedStr))
	})

	if h.store.has(dnKey) {
		t.Fatalf("%s was re-created after the fence deleted it", dnKey)
	}
	deleted := false
	for _, op := range store.opLog() {
		switch op {
		case "delete " + dnKey:
			deleted = true
		case "put " + dnKey:
			if deleted {
				t.Fatalf("the old heartbeat put %s after the fence "+
					"deleted it: %v", dnKey, store.opLog())
			}
		}
	}
	if !deleted {
		t.Fatalf("the fence never deleted %s: %v", dnKey, store.opLog())
	}
}

// TestVoteShutdownDeletesOwnRegistrations is CM5: a graceful stop deletes the
// worker's own registrations so peers start their grace windows at once.
func TestVoteShutdownDeletesOwnRegistrations(t *testing.T) {
	h := newVoteHarness(t, common.WorkerRoleDn, common.WorkerRoleCn)
	seed := h.vote.currentSeed()
	h.cancel()
	<-h.done
	for _, role := range []string{common.WorkerRoleDn, common.WorkerRoleCn} {
		key := workerRegKey(role, seed)
		if h.store.has(key) {
			t.Fatalf("%s still registered after shutdown", key)
		}
	}
}
