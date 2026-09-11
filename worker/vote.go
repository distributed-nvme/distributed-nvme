package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The "reason" attribute of the §12 "worker fenced" record (VW8).
const (
	fenceHeartbeatStalled = "heartbeat_stalled"
	fenceWatchStalled     = "watch_stalled"
	fenceKeyDeleted       = "key_deleted"
)

// The "state" attributes of the §12 membership records (VW3, VW6).
const (
	stateLive      = "live"
	stateDead      = "dead"
	stateMember    = "member"
	stateNonmember = "nonmember"
)

// ---------------------------------------------------------------------------
// Messages funnelled into the vote loop
// ---------------------------------------------------------------------------
//
// The vote layer is one goroutine (the loop below) plus short-lived helpers
// that do the blocking etcd work. Everything the loop reacts to arrives on one
// of these four channels, so the state machines of VW3, VW5, VW6 and VW9 run
// serialized and need no locking, while a slow scan, a slow put or a slow
// shard handoff can never stall the heartbeat and fence the worker by
// accident.

// hbMsg is one heartbeat tick's outcome (VW2).
type hbMsg struct {
	gen uint64
	// tickAt is the monotonic time at which the ticker FIRED, sampled before
	// the puts: VW2 records "the monotonic time of the last TICK at which
	// every role's put succeeded", so a put that blocks for its whole op
	// timeout cannot backdate the gap the NEXT tick measures for VW8(a).
	tickAt time.Time
	// at is the monotonic time after this tick's puts returned — the "now" of
	// the VW8 checks the tick performs.
	at time.Time
	// ok is true when EVERY configured role's put succeeded on this tick
	// (VW8a's lastOkPut).
	ok bool
}

// watchMsg is one event of a role's registry watch, or its end (VW3).
type watchMsg struct {
	gen      uint64
	watchGen uint64
	role     string
	event    etcdutil.Event
	ended    bool
	err      error
}

// scanMsg is one role's registry scan result (VW3, VW7).
type scanMsg struct {
	gen  uint64
	role string
	kvs  []etcdutil.KV
	rev  int64
	err  error
}

// timerKind distinguishes the vote layer's three timers.
type timerKind int

const (
	// timerDeadline is a registration's liveness deadline at lastSeen +
	// 2 x interval, re-armed by every put (VW3).
	timerDeadline timerKind = iota
	// timerGrace is a registration's pending grace window (VW5).
	timerGrace
	// timerRescan retries a failed registry scan one vote interval later —
	// never a hot loop.
	timerRescan
)

// timerMsg is one fired vote timer.
type timerMsg struct {
	gen  uint64
	kind timerKind
	role string
	seed string
	// tgen is the timer's own generation: a timer cancelled by a transition
	// that fires anyway is discarded here (VW5's cancel-on-transition rule).
	tgen uint64
}

// pendingTimer is one armed timer plus the goroutine that funnels its firing
// into the loop. stop cancels both.
type pendingTimer struct {
	handle  *timerHandle
	cancel  chan struct{}
	stopped bool
}

// stop cancels the timer. It is idempotent and nil-safe, and is only ever
// called from the vote loop goroutine.
func (p *pendingTimer) stop() {
	if p == nil || p.stopped {
		return
	}
	p.stopped = true
	close(p.cancel)
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

// regEntry is one observed registration (VW3, VW5).
type regEntry struct {
	seed string
	// lastSeen is the MONOTONIC time of the last put this observer saw (the
	// scan time for a key found by a scan). The stored WorkerReg.epoch is
	// never compared with local time (VW4).
	lastSeen time.Time
	// live is the observed state of VW3.
	live bool
	// effective is the committed state of VW5/VW6: true = member.
	effective bool

	deadline    *pendingTimer
	deadlineGen uint64

	grace       *pendingTimer
	graceGen    uint64
	graceTarget bool

	// tickets memoizes this registration's 256 VW9 tickets for the role it
	// belongs to, so an ownership recompute never re-hashes them.
	tickets [][sha256.Size]byte
}

// stopDeadline cancels the registration's liveness deadline (VW3).
func (e *regEntry) stopDeadline() {
	e.deadline.stop()
	e.deadline = nil
}

// stopGrace cancels the registration's pending grace timer (VW5).
func (e *regEntry) stopGrace() {
	e.grace.stop()
	e.grace = nil
}

// ticket returns sha256("{seed}-{role}-{shard}") (VW9), memoized.
func (e *regEntry) ticket(role string, shard uint32) [sha256.Size]byte {
	if e.tickets == nil {
		e.tickets = make([][sha256.Size]byte, common.ShardBucketSize)
		for s := 0; s < common.ShardBucketSize; s++ {
			e.tickets[s] = voteTicket(e.seed, role, uint32(s))
		}
	}
	return e.tickets[shard]
}

// voteTicket is the VW9 ticket function: sha256 of
// fmt.Sprintf("%s-%s-%02x", seed, role, shard), 32 raw bytes.
func voteTicket(seed string, role string, shard uint32) [sha256.Size]byte {
	return sha256.Sum256([]byte(fmt.Sprintf(
		"%s-%s-"+common.ShardCodeFmt, seed, role, shard,
	)))
}

// ownerOf returns the seed of the effective member holding the largest ticket
// for (role, shard) under bytes.Compare (VW9). A tie is a sha256 collision and
// is broken by the larger seed string; it is never expected. An empty
// membership owns nothing (VW11).
func ownerOf(members []*regEntry, role string, shard uint32) string {
	best := ""
	var bestTicket [sha256.Size]byte
	for _, entry := range members {
		ticket := entry.ticket(role, shard)
		if best == "" {
			best, bestTicket = entry.seed, ticket
			continue
		}
		switch cmp := bytes.Compare(ticket[:], bestTicket[:]); {
		case cmp > 0, cmp == 0 && entry.seed > best:
			best, bestTicket = entry.seed, ticket
		}
	}
	return best
}

// roleState is one role's independent vote state (VW10): its own registry
// prefix, watch, tracking entries, timers, effective set and shard workers.
type roleState struct {
	role    string
	entries map[string]*regEntry
	mgr     *shardManager

	watchCancel context.CancelFunc
	watchGen    uint64

	scanPending bool
	rescan      *pendingTimer
}

// entryFor returns the tracking entry of a seed, creating it on first sight.
func (rs *roleState) entryFor(seed string) *regEntry {
	if entry, ok := rs.entries[seed]; ok {
		return entry
	}
	entry := &regEntry{seed: seed}
	rs.entries[seed] = entry
	return entry
}

// members returns the role's effective membership in seed order (VW6, VW9).
func (rs *roleState) members() []*regEntry {
	out := make([]*regEntry, 0, len(rs.entries))
	for _, entry := range rs.entries {
		if entry.effective {
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seed < out[j].seed })
	return out
}

// stopWatch cancels the role's registry watch (VW3).
func (rs *roleState) stopWatch() {
	if rs.watchCancel == nil {
		return
	}
	rs.watchCancel()
	rs.watchCancel = nil
}

// incarnation is one seed's worth of vote state (VW1): a process start and
// every rejoin after a fence mints a new one, and nothing is persisted.
type incarnation struct {
	seed string
	// gen discards messages that were in flight when the incarnation ended.
	gen uint64

	ctx      context.Context
	cancel   context.CancelFunc
	hbCtx    context.Context
	hbCancel context.CancelFunc
	// hbDone is closed when the heartbeat loop has returned. The fence and
	// the shutdown join it before deleting this seed's registrations, so no
	// put of this incarnation can still be in flight and resurrect a key
	// that was just deleted (VW8, CM5).
	hbDone chan struct{}
	wg     sync.WaitGroup

	roles map[string]*roleState

	// lastOkPut is the monotonic time of the last tick at which EVERY role's
	// put succeeded (VW8a).
	lastOkPut time.Time
	// lastOwnEcho is the monotonic time of the last own-key put EVENT this
	// observer saw come back on its own registry watch (VW8b). A Range result
	// is NOT a watch event and never refreshes it: (b) exists precisely to
	// catch a watch that has stopped delivering while everything else still
	// looks healthy, and a rescan is driven by this worker, not by the watch
	// it distrusts. Refreshing it from a scan would hide exactly the case
	// (b) is the backstop for.
	lastOwnEcho time.Time

	// pendingFence is the reason of a fence VW8 demanded that could NOT be
	// applied, because minting the new seed failed (VW1). checkFence retries it
	// on every following heartbeat tick: (a) and (b) are conditions that still
	// hold and would fire again on their own, but (c) is an EVENT — the delete
	// has already been consumed by the time the mint fails — so without this
	// the fence would be lost for good.
	pendingFence string

	regMu      sync.Mutex
	registered map[string]bool
}

// markRegistered reports whether this is the FIRST successful put of a role,
// which is what the §12 "worker registered" record marks (VW2).
func (inc *incarnation) markRegistered(role string) bool {
	inc.regMu.Lock()
	defer inc.regMu.Unlock()
	if inc.registered[role] {
		return false
	}
	inc.registered[role] = true
	return true
}

// deferFence remembers a fence VW8 demanded that could not be applied, so that
// the next heartbeat tick retries it (VW8). The first cause wins: it is the one
// that made the worker leave.
func (inc *incarnation) deferFence(reason string) {
	if inc.pendingFence == "" {
		inc.pendingFence = reason
	}
}

// ---------------------------------------------------------------------------
// The vote worker (§6)
// ---------------------------------------------------------------------------

// voteWorker is the per-process vote layer (§6). It owns the seed, the
// heartbeat loop, one registry watch per role, the per-registration state
// machines, the effective membership of every role, the ownership computation
// and the lifecycle of the shard workers.
type voteWorker struct {
	deps  *deps
	roles []string

	hbCh    chan hbMsg
	watchCh chan watchMsg
	scanCh  chan scanMsg
	timerCh chan timerMsg
	// stopped is closed when the loop returns, so a timer that fires during
	// teardown never blocks its funnel goroutine forever.
	stopped chan struct{}

	nextGen uint64
	inc     *incarnation

	// processed counts the messages the loop has handled. It is an
	// observation point for the §13 vote tests, which must know a stimulus
	// has been applied before they advance the fake clock; nothing in
	// production reads it.
	processed atomic.Uint64

	mu   sync.Mutex
	seed string
}

// newVoteWorker builds the vote layer around one freshly minted seed (VW1).
func newVoteWorker(d *deps, seed string) *voteWorker {
	return &voteWorker{
		deps:    d,
		roles:   append([]string(nil), d.cfg.Roles...),
		hbCh:    make(chan hbMsg, 1),
		watchCh: make(chan watchMsg),
		scanCh:  make(chan scanMsg, 1),
		timerCh: make(chan timerMsg),
		stopped: make(chan struct{}),
		seed:    seed,
	}
}

// currentSeed is the seed of the live incarnation, which the §12
// "worker stopping" record carries (CM5/CM6).
func (v *voteWorker) currentSeed() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.seed
}

// setSeed publishes a new incarnation's seed.
func (v *voteWorker) setSeed(seed string) {
	v.mu.Lock()
	v.seed = seed
	v.mu.Unlock()
}

// run drives the vote layer until ctx ends, then performs CM5's first three
// steps in order: stop the heartbeat loop, delete this worker's own
// registrations, stop every shard worker gracefully and in parallel.
func (v *voteWorker) run(ctx context.Context) {
	defer close(v.stopped)
	v.startIncarnation(v.currentSeed())
	for {
		select {
		case <-ctx.Done():
			v.shutdown(ctx)
			return
		case msg := <-v.hbCh:
			v.onHeartbeat(msg)
		case msg := <-v.watchCh:
			v.onWatch(msg)
		case msg := <-v.scanCh:
			v.onScan(msg)
		case msg := <-v.timerCh:
			v.onTimer(msg)
		}
		v.processed.Add(1)
	}
}

// startIncarnation registers under a seed and starts observing (§6.2-§6.4).
// The FIRST put of every role happens here, before the observers scan, so the
// worker's own registration is part of its own first scan or its own first
// watch events (VW2/VW3).
func (v *voteWorker) startIncarnation(seed string) {
	v.nextGen++
	ctx, cancel := context.WithCancel(context.Background())
	hbCtx, hbCancel := context.WithCancel(ctx)
	now := v.deps.clk.now()
	inc := &incarnation{
		seed:        seed,
		gen:         v.nextGen,
		ctx:         ctx,
		cancel:      cancel,
		hbCtx:       hbCtx,
		hbCancel:    hbCancel,
		hbDone:      make(chan struct{}),
		roles:       make(map[string]*roleState, len(v.roles)),
		lastOkPut:   now,
		lastOwnEcho: now,
		registered:  make(map[string]bool),
	}
	for _, role := range v.roles {
		kind, ok := v.deps.kinds(role)
		if !ok {
			slog.ErrorContext(ctx, "unknown worker role",
				slog.String("role", role),
			)
			continue
		}
		inc.roles[role] = &roleState{
			role:    role,
			entries: make(map[string]*regEntry),
			mgr:     startShardManager(v.deps, kind, seed),
		}
	}
	v.inc = inc
	v.setSeed(seed)

	// VW2: the first put round, before any observation.
	if v.putAll(inc) {
		inc.lastOkPut = v.deps.clk.now()
	}

	inc.wg.Add(1)
	go v.heartbeat(inc)

	for _, rs := range inc.roles {
		v.startScan(inc, rs)
	}
}

// heartbeat re-puts every role's registration on one ticker (VW2).
func (v *voteWorker) heartbeat(inc *incarnation) {
	defer inc.wg.Done()
	// The fence and the shutdown wait for this (VW8, CM5): once it is closed
	// no put of this incarnation is in flight any more.
	defer close(inc.hbDone)
	ticker := v.deps.clk.newTicker(v.deps.cfg.VoteInterval)
	defer ticker.stop()
	for {
		select {
		case <-inc.hbCtx.Done():
			return
		case <-ticker.C:
			tickAt := v.deps.clk.now()
			ok := v.putAll(inc)
			select {
			case v.hbCh <- hbMsg{
				gen:    inc.gen,
				tickAt: tickAt,
				at:     v.deps.clk.now(),
				ok:     ok,
			}:
			case <-inc.hbCtx.Done():
				return
			}
		}
	}
}

// putAll re-puts the registration of every configured role and reports
// whether all of them succeeded (VW2, VW8a). Each put is bounded by
// common.DefaultEtcdOpTimeout inside etcdutil (EU5); a failed put is logged
// by its "etcd put" record and retried at the next tick.
func (v *voteWorker) putAll(inc *incarnation) bool {
	allOk := true
	for _, role := range v.roles {
		reg := &pb.WorkerReg{Epoch: v.deps.clk.nowUnix()}
		err := v.deps.store.Put(
			inc.hbCtx, workerRegKey(role, inc.seed), reg,
		)
		if err != nil {
			allOk = false
			continue
		}
		if inc.markRegistered(role) {
			slog.InfoContext(inc.hbCtx, msgWorkerRegistered,
				slog.String("role", role),
				slog.String("seed", inc.seed),
			)
		}
	}
	return allOk
}

// onHeartbeat applies the self-fence checks of VW8 (a) and (b), which VW8
// requires on every tick, and only THEN folds the tick's outcome into the
// incarnation.
//
// The order is the rule: VW8(a) asks how long this worker's heartbeat has been
// absent from etcd, so it is evaluated against the value lastOkPut had BEFORE
// this tick. Folding a successful put in first would make (a) true only on a
// tick whose put failed, and the case VW8(a) calls out by name — a process
// stopped by SIGSTOP or a paused VM, whose resume tick's put SUCCEEDS after a
// long monotonic gap (Appendix A, t=800) — would be undetectable.
func (v *voteWorker) onHeartbeat(msg hbMsg) {
	inc := v.inc
	if inc == nil || msg.gen != inc.gen {
		return
	}
	if v.checkFence(inc, msg.at) {
		// The incarnation is gone; nothing of it may be touched any more.
		return
	}
	if msg.ok {
		inc.lastOkPut = msg.tickAt
	}
}

// ---------------------------------------------------------------------------
// Observation (VW3, VW7)
// ---------------------------------------------------------------------------

// startScan scans one role's registry prefix (VW3). The watch is cancelled
// first: the scan's revision is what the new watch resumes from.
func (v *voteWorker) startScan(inc *incarnation, rs *roleState) {
	if rs.scanPending {
		return
	}
	rs.scanPending = true
	rs.stopWatch()
	gen, role := inc.gen, rs.role
	inc.wg.Add(1)
	go func() {
		defer inc.wg.Done()
		kvs, rev, err := v.deps.store.Range(
			inc.ctx, model.WorkerRegPrefix(role),
		)
		select {
		case v.scanCh <- scanMsg{
			gen: gen, role: role, kvs: kvs, rev: rev, err: err,
		}:
		case <-inc.ctx.Done():
		}
	}()
}

// onScan folds one scan result into the role (VW3, VW7) and restarts the
// watch at the scan's revision + 1.
func (v *voteWorker) onScan(msg scanMsg) {
	inc := v.inc
	if inc == nil || msg.gen != inc.gen {
		return
	}
	rs := inc.roles[msg.role]
	if rs == nil {
		return
	}
	rs.scanPending = false
	if msg.err != nil {
		// The "etcd range" record carries the error. Retried one vote
		// interval later, never in a hot loop.
		rs.rescan = v.armTimer(
			inc.gen, timerRescan, rs.role, "", 0, v.deps.cfg.VoteInterval,
		)
		return
	}
	v.applyScan(inc, rs, msg.kvs)
	v.startWatch(inc, rs, msg.rev+1)
}

// applyScan applies one scan to a role's tracking entries (VW3, VW7): a key
// present gets lastSeen = now and becomes live (an APPEAR transition only if
// it was not already observed live); a key that WAS live but is absent from
// the scan transitions to dead.
//
// The worker's own key is tracked exactly like everyone else's, which is what
// makes VW7 hold: the effective membership starts empty and every key of the
// first scan — the worker's own included — enters through an appear
// transition, so a fresh worker drives nothing for one full grace window.
func (v *voteWorker) applyScan(
	inc *incarnation,
	rs *roleState,
	kvs []etcdutil.KV,
) {
	now := v.deps.clk.now()
	present := make(map[string]bool, len(kvs))
	for _, kv := range kvs {
		role, seed, ok := model.ParseWorkerRegKey(kv.Key)
		if !ok || role != rs.role {
			slog.InfoContext(inc.ctx, "worker reg key malformed",
				slog.String("role", rs.role),
				slog.String("key", kv.Key),
			)
			continue
		}
		// The stored value is deliberately NOT decoded. VW3 says a key FOUND
		// by a scan is live, and VW4 makes its epoch informational: the vote
		// layer never reads it. Skipping a key whose value failed to decode
		// would treat a key that IS present as absent, turn a healthy peer
		// into a disappear, commit it nonmember, delete its registration and
		// so fence it (VW8c) over a field nobody reads.
		present[seed] = true
		entry := rs.entryFor(seed)
		entry.lastSeen = now
		if !entry.live {
			entry.live = true
			v.observed(inc, rs, entry, true)
		}
		v.armDeadline(inc, rs, entry)
	}
	for seed, entry := range rs.entries {
		if present[seed] || !entry.live {
			continue
		}
		entry.live = false
		entry.stopDeadline()
		v.observed(inc, rs, entry, false)
	}
}

// startWatch opens a role's registry watch at fromRev (VW3) and funnels its
// events into the loop.
func (v *voteWorker) startWatch(
	inc *incarnation,
	rs *roleState,
	fromRev int64,
) {
	rs.stopWatch()
	rs.watchGen++
	ctx, cancel := context.WithCancel(inc.ctx)
	rs.watchCancel = cancel
	evCh, errCh := v.deps.store.WatchTyped(
		ctx, model.WorkerRegPrefix(rs.role), fromRev,
		func() proto.Message { return &pb.WorkerReg{} },
	)
	gen, watchGen, role := inc.gen, rs.watchGen, rs.role
	inc.wg.Add(1)
	go func() {
		defer inc.wg.Done()
		for event := range evCh {
			select {
			case v.watchCh <- watchMsg{
				gen: gen, watchGen: watchGen, role: role, event: event,
			}:
			case <-ctx.Done():
				return
			}
		}
		// EU3: the error, if any, is buffered before both channels close, so
		// this never blocks.
		err := <-errCh
		select {
		case v.watchCh <- watchMsg{
			gen: gen, watchGen: watchGen, role: role, ended: true, err: err,
		}:
		case <-ctx.Done():
		}
	}()
}

// onWatch folds one registry watch event into the role (VW3) and checks the
// self-fence conditions on every own-key event (VW8).
func (v *voteWorker) onWatch(msg watchMsg) {
	inc := v.inc
	if inc == nil || msg.gen != inc.gen {
		return
	}
	rs := inc.roles[msg.role]
	if rs == nil || msg.watchGen != rs.watchGen {
		return
	}
	if msg.ended {
		if msg.err != nil {
			slog.InfoContext(inc.ctx, "worker reg watch restarting",
				slog.String("role", rs.role),
				slog.String("error", msg.err.Error()),
				slog.Bool("compacted", etcdutil.IsCompacted(msg.err)),
			)
		}
		if inc.ctx.Err() != nil {
			return
		}
		// After ErrCompacted or any watch error the role rescans (VW3).
		v.startScan(inc, rs)
		return
	}
	role, seed, ok := model.ParseWorkerRegKey(msg.event.Key)
	if !ok || role != rs.role {
		slog.InfoContext(inc.ctx, "worker reg key malformed",
			slog.String("role", rs.role),
			slog.String("key", msg.event.Key),
		)
		return
	}
	now := v.deps.clk.now()
	own := seed == inc.seed
	// VW8 is checked on every own-key event, and (a) is checked FIRST. A
	// worker that was stopped (SIGSTOP, a paused VM) comes back to a peer's
	// VW6 garbage-collecting delete of its key and to its own catch-up put at
	// the same moment; the fence must then name the cause — its heartbeat
	// stalled for the dead threshold — and not the consequence (Appendix A
	// t=800, §14.11 case E step 7).
	if own && v.checkHeartbeatStall(inc, now) {
		return
	}
	switch msg.event.Type {
	case etcdutil.EventPut:
		entry := rs.entryFor(seed)
		entry.lastSeen = now
		if own {
			inc.lastOwnEcho = now
		}
		if !entry.live {
			entry.live = true
			v.observed(inc, rs, entry, true)
		}
		v.armDeadline(inc, rs, entry)
	case etcdutil.EventDelete:
		if own {
			// VW8c: a peer committed this worker dead (VW6).
			//
			// VW8(c) says "a delete event for its own key that THIS PROCESS
			// did not issue", and every delete this process issues for its own
			// keys comes from deleteOwnRegs — on this goroutine, from the fence
			// or from the shutdown, both of which discard the incarnation
			// before the loop can select again, so the event is dropped by the
			// watch teardown and by the generation check above. An own-key
			// delete that reaches HERE was therefore always somebody else's,
			// and no bookkeeping of self-issued deletes is needed to tell them
			// apart. Keeping one would be worse than useless: a credit taken
			// for a delete whose event never arrives outlives it and swallows
			// the next, genuine, peer delete — exactly the split-brain VW8(c)
			// exists to close (Appendix B).
			v.fence(inc, fenceKeyDeleted)
			return
		}
		entry, known := rs.entries[seed]
		if !known {
			return
		}
		entry.stopDeadline()
		if entry.live {
			entry.live = false
			v.observed(inc, rs, entry, false)
		}
	}
	if own {
		v.checkFence(inc, now)
	}
}

// armDeadline (re-)arms a registration's liveness deadline at lastSeen +
// 2 x interval (VW3). Every put re-arms it; firing with no put in between
// makes the registration dead.
func (v *voteWorker) armDeadline(
	inc *incarnation,
	rs *roleState,
	entry *regEntry,
) {
	entry.stopDeadline()
	entry.deadlineGen++
	// The deadline sits at lastSeen + 2 x interval; lastSeen is "now" at
	// every call site today, but computing it from the stored monotonic
	// reading is what VW3 says and keeps a future caller honest.
	remaining := entry.lastSeen.
		Add(v.deps.cfg.deadThreshold()).
		Sub(v.deps.clk.now())
	if remaining < 0 {
		remaining = 0
	}
	entry.deadline = v.armTimer(
		inc.gen, timerDeadline, rs.role, entry.seed, entry.deadlineGen,
		remaining,
	)
}

// onTimer folds one fired vote timer into the loop.
func (v *voteWorker) onTimer(msg timerMsg) {
	inc := v.inc
	if inc == nil || msg.gen != inc.gen {
		return
	}
	rs := inc.roles[msg.role]
	if rs == nil {
		return
	}
	switch msg.kind {
	case timerRescan:
		if rs.rescan == nil {
			return
		}
		rs.rescan = nil
		v.startScan(inc, rs)
	case timerDeadline:
		entry := rs.entries[msg.seed]
		if entry == nil || entry.deadline == nil ||
			entry.deadlineGen != msg.tgen {
			return
		}
		entry.deadline = nil
		if !entry.live {
			return
		}
		entry.live = false
		v.observed(inc, rs, entry, false)
	case timerGrace:
		entry := rs.entries[msg.seed]
		if entry == nil || entry.grace == nil || entry.graceGen != msg.tgen {
			return
		}
		entry.grace = nil
		if v.commit(inc, rs, entry) {
			// The incarnation is gone; nothing of it may be touched any more.
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Grace, commit and ownership (VW5, VW6, VW9)
// ---------------------------------------------------------------------------

// observed records one OBSERVED transition of a registration (VW3's log
// record) and applies the grace rule of VW5: cancel the registration's pending
// timer and start a new one of --vote-grace-time with the target this
// transition implies — member when the registration is now observed live,
// nonmember otherwise.
//
// VW5 (as amended) mandates arming the grace timer on EVERY observed
// transition — even when target already equals effective(k) — and VW7 relies
// on it: the disappear of a key that never became effective "starts a
// disappear timer whose commit is a no-op except for the VW6 garbage
// collection". Arming only on target != effective would leave the
// registration of a worker that appeared and died inside its own grace
// window with no timer, hence no commit, hence no VW6 Delete —
// and VW6's collection is the ONLY thing that ever removes a key whose owner
// is gone, because there is no lease ([D17]). The key would leak in etcd for
// good and every observer would keep a tracking entry for it for good, once
// per fence and per process restart.
//
// So a timer is armed on every transition and commit() is idempotent instead:
// a commit whose target already equals the committed state changes no
// membership, logs nothing and recomputes no ownership — VW5's guarantee that
// a registration flapping faster than the grace time never changes anybody's
// effective membership and never delays anybody else's commit holds exactly as
// before — and performs only the VW6 collection when that target is nonmember.
func (v *voteWorker) observed(
	inc *incarnation,
	rs *roleState,
	entry *regEntry,
	live bool,
) {
	state := stateDead
	if live {
		state = stateLive
	}
	slog.InfoContext(inc.ctx, msgMembershipObserved,
		slog.String("role", rs.role),
		slog.String("seed", entry.seed),
		slog.String("state", state),
		slog.Bool("own", entry.seed == inc.seed),
	)
	entry.stopGrace()
	entry.graceGen++
	entry.graceTarget = live
	entry.grace = v.armTimer(
		inc.gen, timerGrace, rs.role, entry.seed, entry.graceGen,
		v.deps.cfg.GraceTime,
	)
}

// commit commits a registration's pending target (VW6): set the effective
// state, log it, and on nonmember also garbage-collect the key — the only
// thing that ever removes a registration whose owner died, since there is no
// lease — and drop the tracking entry entirely, so a later put creates a fresh
// entry with an appear transition. Ownership is recomputed afterwards (VW9).
//
// It is idempotent, because VW7 arms a timer for targets that are already the
// committed state (see observed): such a commit is "a no-op except for the VW6
// garbage collection". It therefore neither logs "membership committed" nor
// recomputes ownership — nothing about the membership changed, and §13 keeps
// the promise that a flapping key never commits — while the collection of a
// nonmember target still runs.
//
// The ONE registration it never collects is this worker's own: that key's owner
// is alive and still heartbeating, so the commit takes VW8's exit instead
// (fenceSelfStale). It reports whether the worker fenced, after which the
// caller must not touch the old incarnation any more.
func (v *voteWorker) commit(
	inc *incarnation,
	rs *roleState,
	entry *regEntry,
) bool {
	if entry.seed == inc.seed && !entry.graceTarget {
		return v.fenceSelfStale(inc, v.deps.clk.now())
	}
	changed := entry.effective != entry.graceTarget
	entry.effective = entry.graceTarget
	if !entry.effective {
		entry.stopDeadline()
		delete(rs.entries, entry.seed)
	}
	if changed {
		state := stateNonmember
		if entry.effective {
			state = stateMember
		}
		slog.InfoContext(inc.ctx, msgMembershipCommitted,
			slog.String("role", rs.role),
			slog.String("seed", entry.seed),
			slog.String("state", state),
			// The entry is already out of the map above, so a nonmember
			// commit reports the membership that remains.
			slog.Int("member_cnt", len(rs.members())),
		)
	}
	if !entry.effective {
		v.deleteAsync(inc, workerRegKey(rs.role, entry.seed))
	}
	if changed {
		v.recomputeOwnership(inc, rs)
	}
	return false
}

// fenceSelfStale takes VW8's exit for the one commit VW6's garbage collection
// must never perform: a nonmember target for this worker's OWN registration.
//
// VW6 defines the collection as removing "a key whose owner died", and VW8 says
// a worker the fleet considers dead is a NEW worker that mints a fresh seed.
// Deleting the key this process's own heartbeat is still refreshing is neither:
// it would un-register a live worker, drop it out of its own effective set,
// release every shard with no "worker fenced" record, and let the next tick
// re-create the key — peers would see a delete and an appear for a seed that
// never stopped running. It also defeats the §12 observability contract, where
// "worker fenced" is what an operator greps to see a worker leave. So the
// commit is abandoned and VW8's single "the fleet gave up on me" path runs
// instead. It is reachable whenever the grace window is short enough to close
// before the next heartbeat tick's VW8 check, which CM3 explicitly permits
// (a grace window below the dead threshold is "legal but pointless").
//
// The reason names the CAUSE, not the consequence. VW8 (a) and (b) are re-tested
// first, in that order — this observer stopped seeing its own key because its
// heartbeat stopped reaching etcd, or because its watch stopped echoing its
// puts — which is also what keeps §14.11 case E step 7 true: a resumed
// SIGSTOPped worker fences as heartbeat_stalled whichever of its expired timers
// the loop drains first. Only when neither holds is the key genuinely gone from
// the registry without this process deleting it — VW8(c)'s fact, learned from a
// rescan (VW3) instead of from a delete event.
func (v *voteWorker) fenceSelfStale(inc *incarnation, now time.Time) bool {
	if v.checkFence(inc, now) {
		return true
	}
	v.fence(inc, fenceKeyDeleted)
	return true
}

// deleteAsync issues one best-effort registry delete off the loop goroutine
// (VW6): it is garbage collection, and every observer that commits the same
// registration dead issues it, so failures need no handling beyond the
// "etcd delete" record. The key is always a PEER's — this worker's own
// registration is never collected (commit, fenceSelfStale).
func (v *voteWorker) deleteAsync(inc *incarnation, key string) {
	inc.wg.Add(1)
	go func() {
		defer inc.wg.Done()
		_ = v.deps.store.Delete(inc.ctx, key)
	}()
}

// recomputeOwnership recomputes owned(role) from the effective membership and
// hands it to the role's shard manager (VW9, VW11).
func (v *voteWorker) recomputeOwnership(inc *incarnation, rs *roleState) {
	members := rs.members()
	owned := make(map[uint32]bool)
	for shard := uint32(0); shard < common.ShardBucketSize; shard++ {
		if ownerOf(members, rs.role, shard) == inc.seed {
			owned[shard] = true
		}
	}
	if rs.mgr != nil {
		rs.mgr.want(owned)
	}
}

// ---------------------------------------------------------------------------
// Self-fence (VW8)
// ---------------------------------------------------------------------------

// checkHeartbeatStall applies VW8(a) alone: the worker's heartbeat has not
// reached etcd for the dead threshold, so its peers are about to (or already
// do) consider it dead. It is judged on monotonic readings, which is what
// catches a process that was stopped (SIGSTOP, a paused VM): the clock
// advanced while it was frozen, so the resume tick measures the whole gap
// even though its own put succeeds (VW4, Appendix A t=800).
//
// It reports whether the worker fenced, after which the caller must not touch
// the old incarnation any more.
func (v *voteWorker) checkHeartbeatStall(
	inc *incarnation,
	now time.Time,
) bool {
	if now.Sub(inc.lastOkPut) < v.deps.cfg.deadThreshold() {
		return false
	}
	v.fence(inc, fenceHeartbeatStalled)
	return true
}

// checkFence applies VW8 (a) and then (b), after retrying a fence a failed seed
// mint had deferred. It is called on every heartbeat tick, on every own-key
// watch event and before this worker's own registration would be collected
// (fenceSelfStale); (c) is applied where the delete arrives. It reports whether
// the worker fenced, after which the caller must not touch the old incarnation
// any more.
func (v *voteWorker) checkFence(inc *incarnation, now time.Time) bool {
	// A fence VW8 already demanded but could not apply — the seed mint failed
	// (VW1) — is retried before anything else. (a) and (b) are conditions and
	// would fire again by themselves; (c) is an event whose delete has already
	// been consumed, so only this brings it back.
	if inc.pendingFence != "" {
		v.fence(inc, inc.pendingFence)
		return true
	}
	// (a) first: it is the root condition, and its reason is the one the
	// SIGSTOP case must log.
	if v.checkHeartbeatStall(inc, now) {
		return true
	}
	// (b) the worker's own put is not echoed by its own watch while puts
	// report success — the "while puts report success" half is implied here,
	// because (a) has just established that a put landed recently.
	if now.Sub(inc.lastOwnEcho) >= v.deps.cfg.deadThreshold() {
		v.fence(inc, fenceWatchStalled)
		return true
	}
	return false
}

// fence stops driving everything and rejoins as a fresh identity (VW8): there
// is no "resume with the old seed" path — a worker that lost etcd for the dead
// threshold is a new worker, exactly like a restart.
//
// The shard workers are stopped and JOINED before the new incarnation starts,
// so every "shard released" of the old seed is logged before anything is
// driven under the new one (§14).
func (v *voteWorker) fence(inc *incarnation, reason string) {
	seed, err := newSeed()
	if err != nil {
		// Nothing is torn down, and the fence is REMEMBERED. Re-testing the
		// VW8 conditions would only bring (a) and (b) back: checkFence looks at
		// lastOkPut and lastOwnEcho, never at "a peer deleted my key" — that
		// one is an event, and the caller has already consumed it — so a (c)
		// fence dropped here would be lost for good. Every following heartbeat
		// tick now retries it (checkFence).
		inc.deferFence(reason)
		slog.ErrorContext(inc.ctx, "worker fence deferred",
			slog.String("reason", reason),
			slog.String("error", err.Error()),
		)
		return
	}
	slog.InfoContext(inc.ctx, msgWorkerFenced,
		slog.String("reason", reason),
		slog.String("old_seed", inc.seed),
		slog.String("new_seed", seed),
	)
	// The old heartbeat goes FIRST, exactly as on CM5's shutdown path: it is
	// still re-putting this seed's registrations, and a tick that lands
	// during or after deleteOwnRegs would resurrect a key the fleet has
	// already given up on — peers would then observe an appear for a seed
	// that no longer exists.
	v.stopHeartbeat(inc)
	v.stopManagers(inc)
	v.deleteOwnRegs(inc.ctx, inc)
	v.discard(inc)
	v.inc = nil
	// VW7 applies to the new incarnation: nothing is driven until one full
	// grace window after its first successful put.
	v.startIncarnation(seed)
}

// shutdown performs CM5's first three steps, in order.
func (v *voteWorker) shutdown(ctx context.Context) {
	inc := v.inc
	if inc == nil {
		return
	}
	// 1. Stop the heartbeat loop.
	v.stopHeartbeat(inc)
	// 2. Delete this worker's own registrations, best effort, so peers start
	//    their grace windows now rather than after the dead threshold.
	v.deleteOwnRegs(ctx, inc)
	// 3. Stop every shard worker gracefully and in parallel (SW5 -> RW11).
	v.stopManagers(inc)
	v.discard(inc)
	v.inc = nil
}

// stopHeartbeat cancels the incarnation's heartbeat loop and JOINS it, so no
// put of this seed can still be in flight when the caller deletes its
// registrations (VW8's fence procedure, CM5 step 1). Every put is bounded by
// common.DefaultEtcdOpTimeout inside etcdutil (EU5), so the join is bounded
// too.
func (v *voteWorker) stopHeartbeat(inc *incarnation) {
	inc.hbCancel()
	<-inc.hbDone
}

// deleteOwnRegs deletes this incarnation's registrations, best effort, one
// Delete per role (CM5, VW8). Each runs under a FRESH context bounded by
// common.DefaultEtcdOpTimeout: on the shutdown path the parent ctx is already
// cancelled, so a plain child would be dead on arrival.
//
// This is the ONLY place that deletes an own registration, and both callers —
// the fence and the shutdown — run it on the loop goroutine and discard the
// incarnation immediately afterwards, so the delete events it produces are
// never processed. That is what lets VW8(c) fence on every own-key delete event
// it sees without tracking which deletes this process issued (onWatch).
func (v *voteWorker) deleteOwnRegs(parent context.Context, inc *incarnation) {
	for _, role := range v.roles {
		key := workerRegKey(role, inc.seed)
		ctx, cancel := shutdownCtx(parent)
		_ = v.deps.store.Delete(ctx, key)
		cancel()
	}
}

// stopManagers stops every role's shard workers gracefully and IN PARALLEL
// (VW8, CM5): each may take up to common.DefaultWorkerSyncupTimeout to let an
// in-flight syncup finish (RW11).
func (v *voteWorker) stopManagers(inc *incarnation) {
	var wg sync.WaitGroup
	for _, rs := range inc.roles {
		if rs.mgr == nil {
			continue
		}
		wg.Add(1)
		go func(m *shardManager) {
			defer wg.Done()
			m.stop()
		}(rs.mgr)
	}
	wg.Wait()
}

// discard throws away every tracking entry, timer, watch and effective set of
// an incarnation (VW8) and joins its helper goroutines.
func (v *voteWorker) discard(inc *incarnation) {
	for _, rs := range inc.roles {
		rs.stopWatch()
		rs.rescan.stop()
		rs.rescan = nil
		for _, entry := range rs.entries {
			entry.stopDeadline()
			entry.stopGrace()
		}
		rs.entries = make(map[string]*regEntry)
		rs.mgr = nil
	}
	inc.hbCancel()
	inc.cancel()
	inc.wg.Wait()
}

// armTimer arms one vote timer and funnels its firing into the loop.
func (v *voteWorker) armTimer(
	gen uint64,
	kind timerKind,
	role string,
	seed string,
	tgen uint64,
	d time.Duration,
) *pendingTimer {
	handle := v.deps.clk.newTimer(d)
	pending := &pendingTimer{handle: handle, cancel: make(chan struct{})}
	msg := timerMsg{gen: gen, kind: kind, role: role, seed: seed, tgen: tgen}
	go func() {
		select {
		case <-handle.C:
			select {
			case v.timerCh <- msg:
			case <-pending.cancel:
			case <-v.stopped:
			}
		case <-pending.cancel:
			handle.stop()
		}
	}()
	return pending
}

// ---------------------------------------------------------------------------
// Shard manager (VW9)
// ---------------------------------------------------------------------------

// shardManager applies one role's owned-shard set on its own goroutine, so
// that a graceful handoff (SW5 -> RW11, up to
// common.DefaultWorkerSyncupTimeout) never stalls the vote loop's heartbeat
// and watch handling — which would fence the worker for no reason.
type shardManager struct {
	deps *deps
	kind revKind
	seed string

	wantCh chan map[uint32]bool
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	running map[uint32]*shardWorker
}

// startShardManager starts one role's shard manager.
func startShardManager(d *deps, kind revKind, seed string) *shardManager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &shardManager{
		deps:    d,
		kind:    kind,
		seed:    seed,
		wantCh:  make(chan map[uint32]bool, 1),
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		running: make(map[uint32]*shardWorker),
	}
	go m.run()
	return m
}

// want hands the manager a new owned set, overwriting an unapplied one: only
// the latest ownership matters (VW9). It never blocks, and has exactly one
// producer — the vote loop.
func (m *shardManager) want(owned map[uint32]bool) {
	select {
	case <-m.wantCh:
	default:
	}
	select {
	case m.wantCh <- owned:
	default:
	}
}

// stop stops every shard worker of the role gracefully and joins them (SW5).
func (m *shardManager) stop() {
	m.cancel()
	<-m.done
}

func (m *shardManager) run() {
	defer close(m.done)
	for {
		select {
		case <-m.ctx.Done():
			m.stopShards(m.shardList())
			return
		case owned := <-m.wantCh:
			m.apply(owned)
		}
	}
}

// apply diffs the owned set against the running shard workers (VW9): newly
// owned shards start one and log "shard owned", shards no longer owned are
// stopped gracefully and log "shard released" after the join.
func (m *shardManager) apply(owned map[uint32]bool) {
	starting := make([]uint32, 0, len(owned))
	for shard := range owned {
		if _, ok := m.running[shard]; ok {
			continue
		}
		starting = append(starting, shard)
	}
	sort.Slice(starting, func(i, j int) bool {
		return starting[i] < starting[j]
	})
	for _, shard := range starting {
		m.running[shard] = startShardWorker(
			m.deps, m.kind, shard, m.seed,
		)
		slog.InfoContext(m.ctx, msgShardOwned,
			slog.String("role", m.kind.role),
			slog.String("shard", shardCode(shard)),
			slog.String("seed", m.seed),
		)
	}
	var gone []uint32
	for shard := range m.running {
		if !owned[shard] {
			gone = append(gone, shard)
		}
	}
	m.stopShards(gone)
}

// shardList returns every running shard, ascending.
func (m *shardManager) shardList() []uint32 {
	shards := make([]uint32, 0, len(m.running))
	for shard := range m.running {
		shards = append(shards, shard)
	}
	return shards
}

// stopShards stops a set of shard workers in parallel, joins them, and only
// then logs "shard released" for each — so a shard shows as released when
// nothing is driving it any more (VW9, SW5).
func (m *shardManager) stopShards(shards []uint32) {
	if len(shards) == 0 {
		return
	}
	sort.Slice(shards, func(i, j int) bool { return shards[i] < shards[j] })
	var wg sync.WaitGroup
	for _, shard := range shards {
		worker := m.running[shard]
		delete(m.running, shard)
		if worker == nil {
			continue
		}
		wg.Add(1)
		go func(w *shardWorker) {
			defer wg.Done()
			w.stop()
		}(worker)
	}
	wg.Wait()
	// The manager ctx is cancelled on the teardown path; the record still has
	// to come out.
	ctx := context.WithoutCancel(m.ctx)
	for _, shard := range shards {
		slog.InfoContext(ctx, msgShardReleased,
			slog.String("role", m.kind.role),
			slog.String("shard", shardCode(shard)),
			slog.String("seed", m.seed),
		)
	}
}
