package worker

// ---------------------------------------------------------------------------
// The whole-worker harness (dnv-worker.md, Integration test plan)
// ---------------------------------------------------------------------------
//
// The harness runs whole workers — the vote layer, the shard and revision
// workers, the sp coordinators and their reactions, all through run, the
// entry behind Run — inside the test process, against a real etcd of the
// harness's own, a fleet of integtest/agentfake fakes and a cluster planted
// through integtest/plant. It runs on the real clock with the shortest
// intervals that stay robust on a loaded machine (harnessVoteInterval and the
// rest below); a case that waits always waits for a condition, with a
// deadline and a message (waitUntil), never for a fixed time.
//
// What a case uses:
//
//	h := newWorkerHarness(t, harnessOpts{})  private etcd, fleet, etcd client
//	h.cli, h.fleet, h.tmo                    the case's client, the fakes, the timeouts
//	c := h.plantStd(t, nil)                  the standard cluster and its fakes
//	cl := h.putCluster(t, spec)              a cluster for a shape of the case's own
//	w := h.startWorker(t, "w1")              a whole worker on every role
//	w.stop(t)                                orderly stop (CM5), joined
//	w.crash(t)                               no CM5: its registrations stay
//	w.join(t)                                wait for a crashed worker to wind down
//	w2 := h.restart(t, w)                    a new incarnation, a new seed
//	h.waitUntil(t, d, what, cond)            poll cond until d runs out
//	h.waitOwnsAll(t, w)                      w owns every shard of its roles
//	h.ownerOf(role, shard)                   the running owner, by its records
//	h.ticketOwner(role, shard, ws...)        the owner VW9 gives among ws
//	h.records(w, msg), h.waitRecord(...)     the log records routed to w
//	rec.str(k), rec.num(k), sideIdOf(rec)    a record's attributes
//	c.waitConverged(t)                       every object converged (below)
//	c.load(t), c.cntlr(t, id), c.primary(t), c.cdcEntry(t), c.spRev(t),
//	c.dnConf(t, id), c.dnRev(t, id), c.cnConf(t, id), c.cnRev(t, id)
//	c.dnAgent(id), c.cnAgent(id), c.sideAgent(t, sideId), c.cntlrAgent(t, id)
//	c.spOwner(), c.dnOwner(id), c.cnOwner(id)
//	h.onRecord(f)                            run f inside each worker record's log call
//
// Every harness is safe beside other harnesses, so its cases call t.Parallel:
// the etcd is the harness's own, because the registrations, the revision
// prefixes and the cluster confs a worker watches are global keys, and the
// log records reach each case through the one handler harnessRouter installs.
// Its cleanup joins every worker it started, so nothing of a case logs into
// a later one, and fails the case on a fence it did not allow.

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/integtest/plant"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The harness's timers: the worker suite's shape (dnv-worker.md, Integration
// test plan, Topology), shortened. A check round every second, the shortest
// interval a stored conf holds; a vote interval below a second, with a grace
// window past twice it (CM3); and the four fixed timeouts at a second or two.
const (
	harnessVoteInterval = 500 * time.Millisecond
	harnessGraceTime    = 1500 * time.Millisecond
	// harnessInterval is every health-check interval of a planted cluster,
	// in seconds.
	harnessInterval = 1
	// harnessStopBound is how long a worker may take to return once it is
	// stopped or crashed: a stop waits out an in-flight call (RW11), a crash
	// each CM5 delete on its closed client.
	harnessStopBound = 20 * time.Second
	// harnessDumpCnt is how many of its last records a failed case logs.
	harnessDumpCnt = 300
	// harnessPoll is how often waitUntil asks its condition.
	harnessPoll = 25 * time.Millisecond
)

// harnessTimeouts are the four fixed timeouts every harness worker runs with.
func harnessTimeouts() timeouts {
	return timeouts{
		syncup:       2 * time.Second,
		push:         2 * time.Second,
		demotionHold: time.Second,
		etcdOp:       2 * time.Second,
	}
}

// harnessOpts are a harness's settings; the zero value is the harness's
// defaults.
type harnessOpts struct {
	// voteInterval and graceTime replace the vote timers when not zero.
	voteInterval time.Duration
	graceTime    time.Duration
	// tmo replaces harnessTimeouts when not nil.
	tmo *timeouts
	// keepWire keeps the records of the gRPC interceptors and of etcdutil,
	// which the harness drops by default: a case reads what an agent got
	// from its fake's Record, and what etcd holds from etcd.
	keepWire bool
	// loudFleet serves the fakes behind the real server interceptors, whose
	// log records every message; with keepWire those records are routed too.
	loudFleet bool
	// allowFence lets a worker fence itself (VW8) without failing the case.
	allowFence bool
}

// workerHarness is one harness: its etcd, its fleet, its workers and the log
// records routed to it.
type workerHarness struct {
	t        *testing.T
	opts     harnessOpts
	vote     time.Duration
	grace    time.Duration
	tmo      timeouts
	endpoint string
	// cli is the case's own etcd client, never a worker's.
	cli      *etcdutil.Client
	fleet    *agentfake.Fleet
	stopEtcd func()

	mu      sync.Mutex
	workers []*harnessWorker
	recs    []logRecord
	// hook is the case's onRecord function, nil when none is set.
	hook func(ctx context.Context, rec logRecord)
}

// workerState is where a harness worker is in its life.
type workerState int

const (
	workerRunning workerState = iota
	workerStopped
	workerCrashed
)

// harnessWorker is one worker incarnation of a harness: one call of run
// under one first seed, with its own etcd client and its own agent
// connections.
type harnessWorker struct {
	h     *workerHarness
	name  string
	roles []string
	// seed is the first seed; a fence adds the next ones to seeds.
	seed   string
	cli    *etcdutil.Client
	conns  *workerConns
	cancel context.CancelFunc
	done   chan struct{}

	// Guarded by h.mu.
	state workerState
	seeds []string
	owned map[string]map[uint32]bool
}

// newWorkerHarness starts a harness: its own etcd, from the binary the
// package's etcd runs (EU7), an etcd client for the case, and an empty fleet.
func newWorkerHarness(t *testing.T, opts harnessOpts) *workerHarness {
	t.Helper()
	endpoint, stop, err := startEtcd(findEtcdBin())
	if err != nil {
		t.Fatalf("harness etcd: %v", err)
	}
	cli, err := etcdutil.New(context.Background(), []string{endpoint}, time.Second)
	if err != nil {
		stop()
		t.Fatalf("harness etcd client: %v", err)
	}
	h := &workerHarness{
		t:        t,
		opts:     opts,
		vote:     harnessVoteInterval,
		grace:    harnessGraceTime,
		tmo:      harnessTimeouts(),
		endpoint: endpoint,
		cli:      cli,
		stopEtcd: stop,
		fleet: agentfake.NewFleet(agentfake.FleetConfig{
			Quiet: !opts.loudFleet,
			// A connection to a stopped fake retries within half a second,
			// not after gRPC's default backoff of up to two minutes, so a
			// restarted fake is reached again inside one round or two.
			DialOptions: []grpc.DialOption{
				grpc.WithConnectParams(grpc.ConnectParams{
					Backoff: backoff.Config{
						BaseDelay:  50 * time.Millisecond,
						Multiplier: 1.6,
						Jitter:     0.2,
						MaxDelay:   500 * time.Millisecond,
					},
					MinConnectTimeout: time.Second,
				}),
			},
		}),
	}
	if opts.voteInterval != 0 {
		h.vote = opts.voteInterval
	}
	if opts.graceTime != 0 {
		h.grace = opts.graceTime
	}
	if opts.tmo != nil {
		h.tmo = *opts.tmo
	}
	harnessRouter.install()
	t.Cleanup(h.close)
	return h
}

// close is the harness's cleanup: every running worker stops in order, a
// fence the case did not allow fails it, and the fleet, the client and the
// etcd go, in that order, since a stopping worker still deletes its
// registrations.
func (h *workerHarness) close() {
	t := h.t
	h.mu.Lock()
	workers := slices.Clone(h.workers)
	h.mu.Unlock()
	for _, w := range workers {
		w.setState(workerStopped)
		w.cancel()
	}
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-w.done:
			case <-time.After(harnessStopBound):
				t.Errorf("worker %s did not return within %v of its stop",
					w.name, harnessStopBound)
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		h.dump(t)
	}
	if !h.opts.allowFence {
		for _, rec := range h.records(nil, msgWorkerFenced) {
			t.Errorf("worker %s fenced itself (reason %q, old seed %s): "+
				"a case that means it sets allowFence",
				rec.workerName(), rec.str("reason"), rec.str("old_seed"))
		}
	}
	h.fleet.Close()
	if err := h.cli.Close(); err != nil {
		t.Errorf("harness etcd client close: %v", err)
	}
	h.stopEtcd()
	harnessRouter.forget(h)
}

// startWorker starts one whole worker carrying roles, every role when none
// is given, under a new seed and an etcd client of its own. Its Run-level
// records carry a trace id under its seed prefix, as cmd/dnv-worker's carry
// one of their own (CM4), so they reach the worker's records too.
func (h *workerHarness) startWorker(
	t *testing.T,
	name string,
	roles ...string,
) *harnessWorker {
	t.Helper()
	if len(roles) == 0 {
		roles = []string{
			common.WorkerRoleDn, common.WorkerRoleCn, common.WorkerRoleSp,
		}
	}
	cli, err := etcdutil.New(
		context.Background(), []string{h.endpoint}, time.Second,
	)
	if err != nil {
		t.Fatalf("worker %s etcd client: %v", name, err)
	}
	w := &harnessWorker{
		h:     h,
		name:  name,
		roles: roles,
		cli:   cli,
		conns: &workerConns{fleet: h.fleet},
		done:  make(chan struct{}),
		owned: make(map[string]map[uint32]bool),
	}
	w.seed = harnessRouter.claimSeed(t, w)
	h.mu.Lock()
	h.workers = append(h.workers, w)
	h.mu.Unlock()
	ctx, cancel := context.WithCancel(common.WithTraceId(
		context.Background(), seedPrefix(w.seed)+"-"+common.NewTraceId(),
	))
	w.cancel = cancel
	cfg := Config{
		Roles:        roles,
		VoteInterval: h.vote,
		GraceTime:    h.grace,
		Endpoints:    []string{h.endpoint},
	}
	env := runEnv{clk: realClock{}, dial: w.conns.dial, tmo: h.tmo}
	go func() {
		defer close(w.done)
		_ = run(ctx, cli, cfg, w.seed, env)
	}()
	return w
}

// restart starts a new incarnation of a stopped or crashed worker: the same
// name and roles, a new seed, as a restarted process gets (VW1).
func (h *workerHarness) restart(
	t *testing.T,
	w *harnessWorker,
) *harnessWorker {
	t.Helper()
	if w.running() {
		t.Fatalf("restart of worker %s, which still runs", w.name)
	}
	return h.startWorker(t, w.name, w.roles...)
}

// stop is the orderly stop of CM5: the context ends, the worker deletes its
// registrations and joins its shard workers, and run returns.
func (w *harnessWorker) stop(t *testing.T) {
	t.Helper()
	w.cancel()
	w.join(t)
	w.setState(workerStopped)
}

// crash stops the worker as a killed process stops: its etcd client is
// closed, every agent connection it opened is closed and every later dial
// refused, and only then is its context cancelled. Its CM5 deletes then fail,
// so its registrations stay in etcd until a peer finds them dead by the dead
// threshold and collects them (VW3, VW6), and no call it makes after the
// crash reaches an agent or etcd. Its records from the crash on are dropped:
// a killed process logs nothing. The client closes first because a call the
// cut ends wakes its loop at once: a side child whose held syncup the cut
// ends goes on to a round, finds its agent unreachable and writes the side's
// health epoch, which no killed process writes, and with the client still
// open that write would land.
//
// crash does not wait for run to return: on a closed client each CM5 delete
// takes up to the etcd operation timeout to fail, so the dead worker's
// goroutines wind down for a few seconds more, reaching nothing. The harness joins them at
// its cleanup; join waits for them earlier.
func (w *harnessWorker) crash(t *testing.T) {
	t.Helper()
	w.setState(workerCrashed)
	_ = w.cli.Close()
	w.conns.cutAll()
	w.cancel()
}

// join waits for run to return.
func (w *harnessWorker) join(t *testing.T) {
	t.Helper()
	select {
	case <-w.done:
	case <-time.After(harnessStopBound):
		t.Fatalf("worker %s did not return within %v of its stop",
			w.name, harnessStopBound)
	}
}

func (w *harnessWorker) setState(state workerState) {
	w.h.mu.Lock()
	defer w.h.mu.Unlock()
	if w.state == workerRunning {
		w.state = state
	}
}

func (w *harnessWorker) running() bool {
	w.h.mu.Lock()
	defer w.h.mu.Unlock()
	return w.state == workerRunning
}

// currentSeeds are every seed of the incarnation: the first and those its
// fences minted (VW8).
func (w *harnessWorker) currentSeeds() []string {
	w.h.mu.Lock()
	defer w.h.mu.Unlock()
	return slices.Clone(w.seeds)
}

// workerConns is a worker's dial of agent connections (RW7) over the fleet,
// with every connection it built remembered, so a crash can close them.
type workerConns struct {
	fleet *agentfake.Fleet

	mu     sync.Mutex
	cutOff bool
	conns  []*grpc.ClientConn
}

func (c *workerConns) dial(addrPort string) (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cutOff {
		return nil, fmt.Errorf("dial %s: the worker crashed", addrPort)
	}
	conn, err := c.fleet.Dial(addrPort)
	if err != nil {
		return nil, err
	}
	c.conns = append(c.conns, conn)
	return conn, nil
}

// cutAll closes every connection and refuses every later dial, which ends
// every call and stream in flight at once, as a killed process's sockets do.
func (c *workerConns) cutAll() {
	c.mu.Lock()
	c.cutOff = true
	conns := c.conns
	c.conns = nil
	c.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// ---------------------------------------------------------------------------
// Waits
// ---------------------------------------------------------------------------

// waitUntil asks cond every harnessPoll until it holds, and fails the case
// once timeout has passed, with what it waited for and cond's last word on
// why it did not hold yet.
func (h *workerHarness) waitUntil(
	t *testing.T,
	timeout time.Duration,
	what string,
	cond func() (bool, string),
) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(harnessPoll)
	defer ticker.Stop()
	for {
		ok, why := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s: %s", timeout, what, why)
		}
		<-ticker.C
	}
}

// ---------------------------------------------------------------------------
// Log records
// ---------------------------------------------------------------------------

// logRecord is one record routed to a harness: worker is the incarnation it
// came from, nil for a record the router could tie to the harness alone.
type logRecord struct {
	at      time.Time
	level   slog.Level
	msg     string
	traceId string
	worker  *harnessWorker
	attrs   map[string]slog.Value
}

// str is an attribute's value as text, "" when the record has none.
func (r logRecord) str(key string) string {
	v, ok := r.attrs[key]
	if !ok {
		return ""
	}
	return v.String()
}

// num is an attribute's value as a number, 0 when it has none.
func (r logRecord) num(key string) uint64 {
	v, ok := r.attrs[key]
	if !ok {
		return 0
	}
	switch v.Kind() {
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindInt64:
		return uint64(v.Int64())
	}
	n, _ := strconv.ParseUint(v.String(), 10, 64)
	return n
}

// sideIdOf is the side_id of a side child's record (a round, syncup or
// lifecycle record of one side), read from its side_pointer.
func sideIdOf(rec logRecord) (uint64, bool) {
	v, ok := rec.attrs["side_pointer"]
	if !ok || v.Kind() != slog.KindAny {
		return 0, false
	}
	ptr, ok := v.Any().(map[string]any)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseUint(fmt.Sprint(ptr["side_id"]), 10, 64)
	return id, err == nil
}

// dump logs the last harnessDumpCnt records of a failed case, for its
// reader.
func (h *workerHarness) dump(t *testing.T) {
	recs := h.records(nil, "")
	if len(recs) > harnessDumpCnt {
		recs = recs[len(recs)-harnessDumpCnt:]
	}
	t.Logf("the last %d records of the harness:", len(recs))
	for _, rec := range recs {
		keys := make([]string, 0, len(rec.attrs))
		for key := range rec.attrs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var attrs strings.Builder
		for _, key := range keys {
			fmt.Fprintf(&attrs, " %s=%s", key, rec.attrs[key])
		}
		t.Logf("  %s %s %q%s", rec.at.Format("15:04:05.000"),
			rec.workerName(), rec.msg, attrs.String())
	}
}

func (r logRecord) workerName() string {
	if r.worker == nil {
		return "(none)"
	}
	return r.worker.name
}

// records returns, in arrival order, the records of w whose msg is msg; a nil
// w takes every record of the harness, a worker's or not, and an empty msg
// every msg.
func (h *workerHarness) records(w *harnessWorker, msg string) []logRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []logRecord
	for _, rec := range h.recs {
		if (w == nil || rec.worker == w) && (msg == "" || rec.msg == msg) {
			out = append(out, rec)
		}
	}
	return out
}

// waitRecord waits for the first record of w (nil: any) with msg that match
// accepts (nil: any) and returns it.
func (h *workerHarness) waitRecord(
	t *testing.T,
	timeout time.Duration,
	w *harnessWorker,
	msg string,
	match func(logRecord) bool,
) logRecord {
	t.Helper()
	var found logRecord
	h.waitUntil(t, timeout, fmt.Sprintf("a %q record", msg),
		func() (bool, string) {
			for _, rec := range h.records(w, msg) {
				if match == nil || match(rec) {
					found = rec
					return true, ""
				}
			}
			return false, "none yet"
		})
	return found
}

// file appends one routed record and keeps the worker's shard ownership
// (VW9) from its shard owned and shard released records.
func (h *workerHarness) file(rec logRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if w := rec.worker; w != nil {
		if w.state == workerCrashed {
			return
		}
		if rec.msg == msgShardOwned || rec.msg == msgShardReleased {
			shard, err := strconv.ParseUint(rec.str("shard"), 16, 32)
			if err == nil {
				role := rec.str("role")
				if w.owned[role] == nil {
					w.owned[role] = make(map[uint32]bool)
				}
				if rec.msg == msgShardOwned {
					w.owned[role][uint32(shard)] = true
				} else {
					delete(w.owned[role], uint32(shard))
				}
			}
		}
	}
	h.recs = append(h.recs, rec)
}

// logRouter is the one slog handler of every whole-worker case: it files
// each record under the harness and the worker incarnation it belongs to.
//
// A worker's record is its own when its trace id starts with the worker's
// seed prefix (RW10), and every record a worker writes carries such an id: a
// unit of work's own, a round's, syncup's, push's, flip's, pass's or drain
// step's, or outside one the id its incarnation, a shard manager, a loop or a
// coordinator holds for its life, so the membership records of its own seed
// and of its peers', the fence, the ownership records, the watch restarts,
// the malformed keys and the loops' lifecycle, idle and invalid stored conf
// records are the worker's too. The harness's Run-level records carry the id
// startWorker mints, and every etcdutil and gRPC record the id of the call it
// logs. A worker fenced record joins its new seed to the incarnation.
//
// A record with no worker's trace id, the case's own or a fake's, is filed by
// what it names: under the worker whose own seed it names (ownSeedMsgs, the
// old seed of worker fenced, membership observed with own set), else under
// the harness of a seed it names, else under the harness of its cluster_id.
// A wire record of no worker is dropped, and so is one that names neither.
type logRouter struct {
	installMu sync.Mutex

	mu        sync.Mutex
	bySeed    map[string]*harnessWorker
	byCluster map[uint64]*workerHarness
}

// harnessRouter is the process's one router.
var harnessRouter = &logRouter{
	bySeed:    make(map[string]*harnessWorker),
	byCluster: make(map[uint64]*workerHarness),
}

// install makes the router the default handler unless it is already. Go
// runs every parallel top-level test only after the last sequential one has
// ended, so no captureLogs case runs beside a parallel harness; one that runs
// after a sequential harness case puts the router back at its cleanup.
func (r *logRouter) install() {
	r.installMu.Lock()
	defer r.installMu.Unlock()
	if h, ok := slog.Default().Handler().(*routeHandler); ok && h.r == r {
		return
	}
	slog.SetDefault(slog.New(&routeHandler{r: r}))
}

// claimSeed mints a seed whose prefix no other worker of the process holds
// and files it under w.
func (r *logRouter) claimSeed(t *testing.T, w *harnessWorker) string {
	t.Helper()
	for {
		seed, err := newSeed()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		r.mu.Lock()
		if _, taken := r.bySeed[seedPrefix(seed)]; !taken {
			r.bySeed[seedPrefix(seed)] = w
			r.mu.Unlock()
			w.h.mu.Lock()
			w.seeds = append(w.seeds, seed)
			w.h.mu.Unlock()
			return seed
		}
		r.mu.Unlock()
	}
}

// addCluster files the records of a cluster under h.
func (r *logRouter) addCluster(cid uint64, h *workerHarness) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byCluster[cid] = h
}

// forget drops every seed and cluster of h.
func (r *logRouter) forget(h *workerHarness) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for prefix, w := range r.bySeed {
		if w.h == h {
			delete(r.bySeed, prefix)
		}
	}
	for cid, owner := range r.byCluster {
		if owner == h {
			delete(r.byCluster, cid)
		}
	}
}

func (r *logRouter) worker(seedOrPrefix string) *harnessWorker {
	if seedOrPrefix == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bySeed[seedPrefix(seedOrPrefix)]
}

func (r *logRouter) cluster(cid uint64) *workerHarness {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byCluster[cid]
}

// ownSeedMsgs are the records whose seed attribute names the worker that logs
// them, by which one that reaches the router with no worker's trace id is
// filed.
var ownSeedMsgs = map[string]bool{
	msgWorkerStarting:   true,
	msgWorkerStopping:   true,
	msgWorkerRegistered: true,
	msgShardOwned:       true,
	msgShardReleased:    true,
}

// wireMsg reports the records of the gRPC interceptors and of etcdutil.
func wireMsg(msg string) bool {
	return strings.HasPrefix(msg, "grpc ") || strings.HasPrefix(msg, "etcd ")
}

// route files one record (logRouter).
func (r *logRouter) route(ctx context.Context, rec slog.Record, h *routeHandler) {
	traceId, _ := common.TraceIdFromCtx(ctx)
	var w *harnessWorker
	if prefix, _, minted := strings.Cut(traceId, "-"); minted {
		w = r.worker(prefix)
	}
	if wireMsg(rec.Message) && (w == nil || !w.h.opts.keepWire) {
		return
	}
	out := logRecord{
		at:      rec.Time,
		level:   rec.Level,
		msg:     rec.Message,
		traceId: traceId,
		attrs:   make(map[string]slog.Value, rec.NumAttrs()+len(h.attrs)),
	}
	for _, a := range h.attrs {
		out.attrs[a.Key] = a.Value.Resolve()
	}
	rec.Attrs(func(a slog.Attr) bool {
		key := a.Key
		if h.group != "" {
			key = h.group + "." + key
		}
		out.attrs[key] = a.Value.Resolve()
		return true
	})
	if w == nil {
		switch {
		case ownSeedMsgs[rec.Message]:
			w = r.worker(out.str("seed"))
		case rec.Message == msgWorkerFenced:
			w = r.worker(out.str("old_seed"))
		case rec.Message == msgMembershipObserved && out.str("own") == "true":
			w = r.worker(out.str("seed"))
		}
	}
	if w != nil {
		if rec.Message == msgWorkerFenced {
			r.addFenceSeed(w, out.str("new_seed"))
		}
		out.worker = w
		w.h.file(out)
		w.h.runHook(ctx, out)
		return
	}
	if peer := r.worker(out.str("seed")); peer != nil {
		peer.h.file(out)
		return
	}
	if owner := r.cluster(out.num("cluster_id")); owner != nil {
		owner.file(out)
	}
}

// onRecord sets f to run for every record of one of the harness's workers,
// after the record is filed, on the goroutine that logged it and inside that
// log call, with the call's ctx: a case acts there at the moment the worker
// reaches one step of a call, between a scan and its commit say, which no
// wait could hit. The wire records reach f only under keepWire, and a
// crashed worker's records never do. f must return, must not wait for the
// worker that logged, and must use a ctx of its own for its own calls, so
// that their records carry no trace id of the worker's; nil clears it.
func (h *workerHarness) onRecord(f func(ctx context.Context, rec logRecord)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hook = f
}

// runHook runs the onRecord function on one filed record of a worker.
func (h *workerHarness) runHook(ctx context.Context, rec logRecord) {
	h.mu.Lock()
	hook := h.hook
	crashed := rec.worker.state == workerCrashed
	h.mu.Unlock()
	if hook != nil && !crashed {
		hook(ctx, rec)
	}
}

// addFenceSeed files the seed a fence minted under the fenced incarnation
// (VW8): the fence logs it before the new incarnation's first record.
func (r *logRouter) addFenceSeed(w *harnessWorker, seed string) {
	if seed == "" {
		return
	}
	r.mu.Lock()
	r.bySeed[seedPrefix(seed)] = w
	r.mu.Unlock()
	w.h.mu.Lock()
	w.seeds = append(w.seeds, seed)
	w.h.mu.Unlock()
}

// routeHandler is the slog.Handler over the router, Info and up, as the
// worker's own default level is (log.md R6).
type routeHandler struct {
	r     *logRouter
	attrs []slog.Attr
	group string
}

func (h *routeHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h *routeHandler) Handle(ctx context.Context, rec slog.Record) error {
	h.r.route(ctx, rec, h)
	return nil
}

func (h *routeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := *h
	out.attrs = append(slices.Clone(h.attrs), attrs...)
	return &out
}

func (h *routeHandler) WithGroup(name string) slog.Handler {
	out := *h
	if out.group != "" {
		name = out.group + "." + name
	}
	out.group = name
	return &out
}

// ---------------------------------------------------------------------------
// Ownership (VW9)
// ---------------------------------------------------------------------------

// ownedCount is how many shards of role w owns by its own records.
func (w *harnessWorker) ownedCount(role string) int {
	w.h.mu.Lock()
	defer w.h.mu.Unlock()
	return len(w.owned[role])
}

// ownerOf is the running worker of the harness whose records say it owns
// shard of role now, nil when none does.
func (h *workerHarness) ownerOf(role string, shard uint32) *harnessWorker {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, w := range h.workers {
		if w.state == workerRunning && w.owned[role][shard] {
			return w
		}
	}
	return nil
}

// ticketOwner is the worker among ws whose current seed holds the largest
// ticket for role and shard (VW9): the owner once ws are exactly the
// committed members of the role.
func (h *workerHarness) ticketOwner(
	role string,
	shard uint32,
	ws ...*harnessWorker,
) *harnessWorker {
	entries := make([]*regEntry, 0, len(ws))
	bySeed := make(map[string]*harnessWorker, len(ws))
	for _, w := range ws {
		seeds := w.currentSeeds()
		seed := seeds[len(seeds)-1]
		entries = append(entries, &regEntry{seed: seed})
		bySeed[seed] = w
	}
	return bySeed[ownerOf(entries, role, shard)]
}

// waitOwnsAll waits until w's records say it owns every shard of each of its
// roles.
func (h *workerHarness) waitOwnsAll(t *testing.T, w *harnessWorker) {
	t.Helper()
	h.waitUntil(t, 10*h.grace+10*time.Second,
		fmt.Sprintf("worker %s to own every shard", w.name),
		func() (bool, string) {
			for _, role := range w.roles {
				if n := w.ownedCount(role); n != common.ShardBucketSize {
					return false, fmt.Sprintf("%d %s shards of %d",
						n, role, common.ShardBucketSize)
				}
			}
			return true, ""
		})
}

// ---------------------------------------------------------------------------
// The standard cluster
// ---------------------------------------------------------------------------

// stdSpec shapes the standard cluster; defaultStdSpec is the shape a case
// gets when it edits nothing.
type stdSpec struct {
	// dnCnt and cnCnt are the nodes planted; the pool uses dn 1 and 2 and
	// cn 1 and 2, the others hold nothing.
	dnCnt int
	cnCnt int
	// nodeFree is every node's free extent count before the pool.
	nodeFree uint64
	// threshold is the pool's event_threshold, in seconds.
	threshold *pb.EventThreshold
	// thinOk sets the fakes' lever that reports a cntlr's thin rows OK while
	// it is primary, as a cn agent whose thin devices exist reports them.
	thinOk bool
	// sharedRacks puts dn i and cn i into one location, "rack<i>", as a rack
	// that holds a disk node and a controller node; by default every node
	// is a location of its own.
	sharedRacks bool
}

// defaultStdSpec is the standard cluster: three disk nodes, three controller
// nodes, event thresholds of one and two seconds — the primary's at twice the
// cntlr interval, the leg's past the side's, as the gateway requires of a
// stored pool (architecture.md, Common validation).
func defaultStdSpec() stdSpec {
	return stdSpec{
		dnCnt:    3,
		cnCnt:    3,
		nodeFree: 64,
		threshold: &pb.EventThreshold{
			PrimaryUnhealthy: 2,
			CntlrUnhealthy:   2,
			SideUnhealthy:    1,
			LegUnhealthy:     2,
		},
		thinOk: true,
	}
}

// location is the location of the standard cluster's node id of kind ("dn"
// or "cn").
func (spec stdSpec) location(kind string, id uint64) string {
	if spec.sharedRacks {
		return fmt.Sprintf("rack%d", id)
	}
	return fmt.Sprintf("%s-rack%d", kind, id)
}

// The standard cluster's shards: each node and the pool in a shard of its
// own, so two workers split them by their tickets.
func stdDnShard(id uint64) uint32 { return uint32(id) }
func stdCnShard(id uint64) uint32 { return 0x10 + uint32(id) }

const stdSpShard = uint32(0x20)

// The standard pool's ids: one slice, a meta group and a data group of two
// RAID1 legs each, every leg's side on dn 1 or dn 2.
const (
	stdSpId      = uint64(1)
	stdSliceId   = uint64(1)
	stdMetaGrpId = uint64(2)
	stdDataGrpId = uint64(5)
)

// stdSides are the standard pool's sides: leg, side and the dn holding it.
var stdSides = []struct{ legId, sideId, dnId uint64 }{
	{3, 8, 1}, {4, 9, 2}, {6, 10, 1}, {7, 11, 2},
}

// harnessClusterSeq names the clusters of the process apart.
var harnessClusterSeq atomic.Uint64

// stdCluster is one planted standard cluster and what its plant wrote.
type stdCluster struct {
	h    *workerHarness
	spec stdSpec
	cl   plant.Cluster
	// dns[i] and cns[i] are dn_id and cn_id i+1.
	dns []plant.Dn
	cns []plant.Cn
	sp  plant.Sp
	td  plant.Td
	ss  plant.Ss
}

func stdDnAddr(id uint64) string { return fmt.Sprintf("10.1.0.1:%d", 29600+id) }
func stdCnAddr(id uint64) string { return fmt.Sprintf("10.1.0.1:%d", 29700+id) }

// putCluster plants a cluster under a name of its own, the spec's name
// replaced and every interval it leaves zero at harnessInterval, and files
// the records that name its cluster_id under h (logRouter). A case that
// plants a cluster of its own shape plants it through here.
func (h *workerHarness) putCluster(t *testing.T, spec plant.ClusterSpec) plant.Cluster {
	t.Helper()
	spec.Name = fmt.Sprintf("wh%d", harnessClusterSeq.Add(1))
	for _, interval := range []*uint32{
		&spec.DnInterval, &spec.CnInterval, &spec.SideInterval, &spec.CntlrInterval,
	} {
		if *interval == 0 {
			*interval = harnessInterval
		}
	}
	cl, err := plant.PutCluster(context.Background(), h.cli, spec)
	if err != nil {
		t.Fatalf("PutCluster: %v", err)
	}
	harnessRouter.addCluster(cl.Id, h)
	return cl
}

// plantStd adds the fakes of the standard cluster to the fleet and plants
// it: the cluster at harnessInterval, the nodes, the pool, its thin device
// and its subsystem. edit, when not nil, changes the spec first.
func (h *workerHarness) plantStd(t *testing.T, edit func(*stdSpec)) *stdCluster {
	t.Helper()
	ctx := context.Background()
	spec := defaultStdSpec()
	if edit != nil {
		edit(&spec)
	}
	c := &stdCluster{h: h, spec: spec}
	for id := uint64(1); id <= uint64(spec.dnCnt); id++ {
		if _, err := h.fleet.Add(stdDnAddr(id), agentfake.Dn, agentfake.Config{}); err != nil {
			t.Fatalf("fake dn %d: %v", id, err)
		}
	}
	for id := uint64(1); id <= uint64(spec.cnCnt); id++ {
		if _, err := h.fleet.Add(stdCnAddr(id), agentfake.Cn, agentfake.Config{}); err != nil {
			t.Fatalf("fake cn %d: %v", id, err)
		}
	}
	cl := h.putCluster(t, plant.ClusterSpec{ExtentSize: common.MinDnExtSize})
	c.cl = cl
	var err error
	var dnSpecs []plant.DnSpec
	for id := uint64(1); id <= uint64(spec.dnCnt); id++ {
		dnSpecs = append(dnSpecs, plant.DnSpec{
			Id: id, Shard: stdDnShard(id), Addr: stdDnAddr(id),
			Location: spec.location("dn", id), FreeExt: spec.nodeFree,
		})
	}
	if c.dns, err = plant.PutDns(ctx, h.cli, cl, dnSpecs); err != nil {
		t.Fatalf("PutDns: %v", err)
	}
	var cnSpecs []plant.CnSpec
	for id := uint64(1); id <= uint64(spec.cnCnt); id++ {
		cnSpecs = append(cnSpecs, plant.CnSpec{
			Id: id, Shard: stdCnShard(id), Addr: stdCnAddr(id),
			Location: spec.location("cn", id), FreeExt: spec.nodeFree,
		})
	}
	if c.cns, err = plant.PutCns(ctx, h.cli, cl, cnSpecs); err != nil {
		t.Fatalf("PutCns: %v", err)
	}
	spSpec := plant.SpSpec{
		Name:      "sp0",
		Id:        stdSpId,
		Shard:     stdSpShard,
		Slots:     []uint32{0, 1, 2},
		Threshold: spec.threshold,
		Cntlrs: []plant.CntlrSpec{
			{CntlrId: 1, CnId: 1, Slot: 0, Primary: true},
			{CntlrId: 2, CnId: 2, Slot: 1},
		},
		Slices: []plant.SliceSpec{{SliceId: stdSliceId, SliceIdx: 0}},
		Groups: []plant.GroupSpec{
			{SliceId: stdSliceId, GrpId: stdMetaGrpId, Meta: true, ExtCnt: 1, Raid1: true},
			{SliceId: stdSliceId, GrpId: stdDataGrpId, ExtCnt: 2, Raid1: true},
		},
		Legs: []plant.LegSpec{
			{GrpId: stdMetaGrpId, LegId: 3, LegIdx: 0},
			{GrpId: stdMetaGrpId, LegId: 4, LegIdx: 1},
			{GrpId: stdDataGrpId, LegId: 6, LegIdx: 0},
			{GrpId: stdDataGrpId, LegId: 7, LegIdx: 1},
		},
	}
	for _, side := range stdSides {
		spSpec.Sides = append(spSpec.Sides, plant.SideSpec{
			LegId: side.legId, SideId: side.sideId, DnId: side.dnId,
		})
	}
	if c.sp, err = plant.PutSp(ctx, h.cli, cl, spSpec); err != nil {
		t.Fatalf("PutSp: %v", err)
	}
	if spec.thinOk {
		for cntlrId, cnId := range map[uint64]uint64{1: 1, 2: 2} {
			err := c.cnAgent(cnId).Update(agentfake.CntlrKey(stdSpId, cntlrId),
				func(o *agentfake.Object) { o.ThinOk = true })
			if err != nil {
				t.Fatalf("thin_ok on cntlr %d: %v", cntlrId, err)
			}
		}
	}
	if c.td, err = plant.PutTd(ctx, h.cli, cl, c.sp.Ref, plant.TdSpec{
		Name: "td0", Size: 1 << 30,
	}); err != nil {
		t.Fatalf("PutTd: %v", err)
	}
	if c.ss, err = plant.PutSs(ctx, h.cli, cl, c.sp.Ref, plant.SsSpec{
		Nqn:        "nqn.2026-10.io.dnv-wh:ss0",
		Serial:     "s0",
		Namespaces: []plant.NsSpec{{NsIdx: 1, TdId: c.td.Id}},
	}); err != nil {
		t.Fatalf("PutSs: %v", err)
	}
	return c
}

func (c *stdCluster) dnAgent(id uint64) *agentfake.Agent {
	return c.h.fleet.Agent(stdDnAddr(id))
}

func (c *stdCluster) cnAgent(id uint64) *agentfake.Agent {
	return c.h.fleet.Agent(stdCnAddr(id))
}

// poolGrps is every group of a load whose sides the pool's coordinator
// drives (dnv-worker.md RW14): the meta and then the data groups of every
// slice, in slice_id_list order, and then the clone meta group of every
// clone, in clone_name_list order, a clone being deleted included and a
// clone with no group adding none. The harness's walks over a pool's legs and
// sides go through it, so none of them leaves the clone meta groups out.
func poolGrps(state *model.SpState) []*pb.Group {
	var grps []*pb.Group
	for _, sliceId := range state.Conf.GetSliceIdList() {
		slice := state.Slices[sliceId]
		grps = append(grps, slice.GetMetaGrpList()...)
		grps = append(grps, slice.GetDataGrpList()...)
	}
	for _, name := range state.Conf.GetCloneNameList() {
		if grp := state.Clones[name].GetMetaGrp(); grp != nil {
			grps = append(grps, grp)
		}
	}
	return grps
}

// sideAgent is the fake that holds a side by the pool's record, and the
// side's object key there.
func (c *stdCluster) sideAgent(t *testing.T, sideId uint64) (*agentfake.Agent, string) {
	t.Helper()
	for _, grp := range poolGrps(c.load(t)) {
		for _, leg := range append(slices.Clone(grp.GetLegList()), grp.GetSpareLegList()...) {
			for _, side := range leg.GetSideList() {
				if side.GetSideId() == sideId {
					return c.h.fleet.Agent(side.GetAddrPort()),
						agentfake.SideKey(stdSpId, leg.GetLegId(), sideId)
				}
			}
		}
	}
	t.Fatalf("no side %d in the pool", sideId)
	return nil, ""
}

// cntlrAgent is the fake of the CN that holds a cntlr by its record, and the
// cntlr's object key there.
func (c *stdCluster) cntlrAgent(t *testing.T, cntlrId uint64) (*agentfake.Agent, string) {
	t.Helper()
	return c.h.fleet.Agent(c.cntlr(t, cntlrId).GetAddrPort()),
		agentfake.CntlrKey(stdSpId, cntlrId)
}

// load reads the pool through model.LoadSp, as the coordinator does.
func (c *stdCluster) load(t *testing.T) *model.SpState {
	t.Helper()
	state, err := model.LoadSp(context.Background(), c.h.cli, c.cl.Id, c.sp.Ref.Name)
	if err != nil {
		t.Fatalf("LoadSp: %v", err)
	}
	return state
}

// cntlr reads one cntlr's record.
func (c *stdCluster) cntlr(t *testing.T, cntlrId uint64) *pb.Cntlr {
	t.Helper()
	cntlr := &pb.Cntlr{}
	c.mustGet(t, model.CntlrKey(c.cl.Id, stdSpId, cntlrId), cntlr)
	return cntlr
}

// primary is the cntlr_id the pool's records name primary, 0 when none does.
func (c *stdCluster) primary(t *testing.T) uint64 {
	t.Helper()
	state := c.load(t)
	for _, id := range state.Conf.GetCntlrIdList() {
		if state.Cntlrs[id].GetPrimary() {
			return id
		}
	}
	return 0
}

// cdcEntry reads the subsystem's discovery record (architecture.md [D18]).
func (c *stdCluster) cdcEntry(t *testing.T) *pb.CdcEntry {
	t.Helper()
	entry := &pb.CdcEntry{}
	c.mustGet(t, c.ss.CdcKey, entry)
	return entry
}

func (c *stdCluster) spRev(t *testing.T) uint64 {
	t.Helper()
	rev := &pb.SpRev{}
	c.mustGet(t, model.SpRevKey(stdSpShard, c.cl.Id, stdSpId), rev)
	return rev.GetRevision()
}

func (c *stdCluster) dnConf(t *testing.T, id uint64) *pb.DnConf {
	t.Helper()
	conf := &pb.DnConf{}
	c.mustGet(t, model.DnConfKey(c.cl.Id, stdDnAddr(id)), conf)
	return conf
}

func (c *stdCluster) dnRev(t *testing.T, id uint64) uint64 {
	t.Helper()
	rev := &pb.DnRev{}
	c.mustGet(t, model.DnRevKey(stdDnShard(id), c.cl.Id, id), rev)
	return rev.GetRevision()
}

func (c *stdCluster) cnConf(t *testing.T, id uint64) *pb.CnConf {
	t.Helper()
	conf := &pb.CnConf{}
	c.mustGet(t, model.CnConfKey(c.cl.Id, stdCnAddr(id)), conf)
	return conf
}

func (c *stdCluster) cnRev(t *testing.T, id uint64) uint64 {
	t.Helper()
	rev := &pb.CnRev{}
	c.mustGet(t, model.CnRevKey(stdCnShard(id), c.cl.Id, id), rev)
	return rev.GetRevision()
}

func (c *stdCluster) mustGet(t *testing.T, key string, msg proto.Message) {
	t.Helper()
	found, err := c.h.cli.Get(context.Background(), key, msg)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	if !found {
		t.Fatalf("get %s: not found", key)
	}
}

// spOwner, dnOwner and cnOwner are the running workers whose records say
// they own the pool's and the nodes' shards.
func (c *stdCluster) spOwner() *harnessWorker {
	return c.h.ownerOf(common.WorkerRoleSp, stdSpShard)
}

func (c *stdCluster) dnOwner(id uint64) *harnessWorker {
	return c.h.ownerOf(common.WorkerRoleDn, stdDnShard(id))
}

func (c *stdCluster) cnOwner(id uint64) *harnessWorker {
	return c.h.ownerOf(common.WorkerRoleCn, stdCnShard(id))
}

// ---------------------------------------------------------------------------
// Convergence
// ---------------------------------------------------------------------------

// objProgress is one object's fake and the count of its syncups and rounds
// when the cluster read as converged.
type objProgress struct {
	name    string
	agent   *agentfake.Agent
	key     string
	syncups int
	rounds  int
}

// convergence is what waitConverged saw.
type convergence struct {
	// at is when every object first read as converged.
	at time.Time
	// spRev, dnRevs and cnRevs are the revisions it converged at.
	spRev  uint64
	dnRevs map[uint64]uint64
	cnRevs map[uint64]uint64
	objs   []objProgress
}

// lastSyncup is the newest Syncup* request a fake recorded about an object:
// the last one it received, refused and held ones included.
func lastSyncup(agent *agentfake.Agent, key string) (proto.Message, int, int) {
	rec := agent.Record(key)
	if len(rec.Syncups) == 0 {
		return nil, 0, len(rec.Rounds)
	}
	return rec.Syncups[len(rec.Syncups)-1], len(rec.Syncups), len(rec.Rounds)
}

// storedSyncup is the Syncup* request a fake keeps for an object, the last
// one it saved, with the counts of the syncups and rounds it received;
// convergedNow alone reads it. A fake records a request as it arrives,
// before it accepts, holds or refuses it, so a case that clears a refusal,
// releases a held syncup or restarts a fake on empty files would read
// converged early from the record (lastSyncup). The request is read before
// the counts: a syncup that lands between the two reads is counted and not
// judged, which only leaves it out of the check that no syncup follows
// convergence, while the other order could judge a syncup it did not count
// and fail that check.
func storedSyncup(agent *agentfake.Agent, key string) (proto.Message, int, int) {
	msg := agent.Stored(key)
	rec := agent.Record(key)
	return msg, len(rec.Syncups), len(rec.Rounds)
}

// sidePtrSet and cntlrPtrSet render pointer lists for comparison.
func sidePtrSet(list []*pb.SidePointer) string {
	var out []string
	for _, p := range list {
		out = append(out, fmt.Sprintf("%d:%d:%d", p.GetSpId(), p.GetLegId(), p.GetSideId()))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func cntlrPtrSet(list []*pb.CntlrPointer) string {
	var out []string
	for _, p := range list {
		out = append(out, fmt.Sprintf("%d:%d", p.GetSpId(), p.GetCntlrId()))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// convergedNow reads etcd and the fakes once and reports whether the cluster
// has converged, as waitConverged defines it, short of the rounds after it:
// every object's fake keeps a syncup at the revision its key holds with the
// planted content (storedSyncup), every side — of the slices' groups and of
// the clones' clone meta groups alike (poolGrps) — is provisioned, the thin
// device created, the primary settled, and no health epoch is set. It returns
// why not, and every object's fake with its counts.
func (c *stdCluster) convergedNow(t *testing.T) (bool, string, convergence) {
	t.Helper()
	conv := convergence{
		dnRevs: make(map[uint64]uint64),
		cnRevs: make(map[uint64]uint64),
	}
	why := ""
	note := func(format string, args ...any) {
		if why == "" {
			why = fmt.Sprintf(format, args...)
		}
	}
	for id := uint64(1); id <= uint64(c.spec.dnCnt); id++ {
		name := fmt.Sprintf("dn %d", id)
		rev, conf := c.dnRev(t, id), c.dnConf(t, id)
		conv.dnRevs[id] = rev
		agent := c.dnAgent(id)
		msg, syncups, rounds := storedSyncup(agent, agentfake.DnKey)
		conv.objs = append(conv.objs, objProgress{name, agent, agentfake.DnKey, syncups, rounds})
		req, _ := msg.(*pb.SyncupDnRequest)
		switch {
		case req == nil:
			note("%s: keeps no SyncupDn yet", name)
		case req.GetRevision() != rev:
			note("%s: keeps a SyncupDn at revision %d, DnRev %d", name, req.GetRevision(), rev)
		case sidePtrSet(req.GetSidePointerList()) != sidePtrSet(conf.GetSidePtrList()):
			note("%s: keeps a SyncupDn listing sides %q, the record %q", name,
				sidePtrSet(req.GetSidePointerList()), sidePtrSet(conf.GetSidePtrList()))
		case conf.GetErrEpoch() != 0:
			note("%s: err_epoch %d", name, conf.GetErrEpoch())
		}
	}
	for id := uint64(1); id <= uint64(c.spec.cnCnt); id++ {
		name := fmt.Sprintf("cn %d", id)
		rev, conf := c.cnRev(t, id), c.cnConf(t, id)
		conv.cnRevs[id] = rev
		agent := c.cnAgent(id)
		msg, syncups, rounds := storedSyncup(agent, agentfake.CnKey)
		conv.objs = append(conv.objs, objProgress{name, agent, agentfake.CnKey, syncups, rounds})
		req, _ := msg.(*pb.SyncupCnRequest)
		switch {
		case req == nil:
			note("%s: keeps no SyncupCn yet", name)
		case req.GetRevision() != rev:
			note("%s: keeps a SyncupCn at revision %d, CnRev %d", name, req.GetRevision(), rev)
		case cntlrPtrSet(req.GetCntlrPointerList()) != cntlrPtrSet(conf.GetCntlrPtrList()):
			note("%s: keeps a SyncupCn listing cntlrs %q, the record %q", name,
				cntlrPtrSet(req.GetCntlrPointerList()), cntlrPtrSet(conf.GetCntlrPtrList()))
		case conf.GetErrEpoch() != 0:
			note("%s: err_epoch %d", name, conf.GetErrEpoch())
		}
	}
	state := c.load(t)
	conv.spRev = state.SpRevision
	for _, grp := range poolGrps(state) {
		for _, leg := range append(slices.Clone(grp.GetLegList()), grp.GetSpareLegList()...) {
			if leg.GetErrEpoch() != 0 {
				note("leg %d: err_epoch %d", leg.GetLegId(), leg.GetErrEpoch())
			}
			for _, side := range leg.GetSideList() {
				name := fmt.Sprintf("side %d", side.GetSideId())
				agent := c.h.fleet.Agent(side.GetAddrPort())
				key := agentfake.SideKey(stdSpId, leg.GetLegId(), side.GetSideId())
				msg, syncups, rounds := storedSyncup(agent, key)
				conv.objs = append(conv.objs, objProgress{name, agent, key, syncups, rounds})
				req, _ := msg.(*pb.SyncupSideRequest)
				switch {
				case !side.GetProvisioned():
					note("%s: not provisioned", name)
				case side.GetErrEpoch() != 0:
					note("%s: err_epoch %d", name, side.GetErrEpoch())
				case req == nil:
					note("%s: keeps no SyncupSide yet", name)
				case req.GetRevision() != state.SpRevision:
					note("%s: keeps a SyncupSide at revision %d, SpRev %d", name,
						req.GetRevision(), state.SpRevision)
				case !req.GetSideConf().GetProvisioned():
					note("%s: keeps a SyncupSide that says not provisioned", name)
				}
			}
		}
	}
	for _, cntlrId := range state.Conf.GetCntlrIdList() {
		name := fmt.Sprintf("cntlr %d", cntlrId)
		cntlr := state.Cntlrs[cntlrId]
		agent := c.h.fleet.Agent(cntlr.GetAddrPort())
		key := agentfake.CntlrKey(stdSpId, cntlrId)
		msg, syncups, rounds := storedSyncup(agent, key)
		conv.objs = append(conv.objs, objProgress{name, agent, key, syncups, rounds})
		req, _ := msg.(*pb.SyncupCntlrRequest)
		switch {
		case cntlr.GetErrEpoch() != 0:
			note("%s: err_epoch %d", name, cntlr.GetErrEpoch())
		case cntlr.GetSettling():
			note("%s: settling", name)
		case req == nil:
			note("%s: keeps no SyncupCntlr yet", name)
		case req.GetRevision() != state.SpRevision:
			note("%s: keeps a SyncupCntlr at revision %d, SpRev %d", name,
				req.GetRevision(), state.SpRevision)
		case req.GetCntlr().GetPrimary() != cntlr.GetPrimary():
			note("%s: keeps a SyncupCntlr with primary %t, the record %t", name,
				req.GetCntlr().GetPrimary(), cntlr.GetPrimary())
		}
	}
	for i, td := range state.Tds {
		if !td.GetCreated() {
			note("td %s: not created", state.TdNames[i])
		}
	}
	return why == "", why, conv
}

// waitConverged waits until the cluster has converged and returns what it
// converged at. Converged means: every revision key stays where it is; every
// object's fake keeps a syncup, the last one it saved, at the revision its
// key holds, with the planted content (a dn's side list and a cn's cntlr list
// as their records hold them, a side provisioned, a cntlr in the role its
// record gives it); every side is provisioned, the thin device created, the
// primary settled (HL2), and no health epoch is set; and after that every
// object's fake gets two more check rounds and no syncup, so the rounds
// answer the revision the worker wants (RW4 step 5).
func (c *stdCluster) waitConverged(t *testing.T) convergence {
	t.Helper()
	var conv convergence
	c.h.waitUntil(t, 30*time.Second, "the standard cluster to converge",
		func() (bool, string) {
			ok, why, now := c.convergedNow(t)
			conv = now
			return ok, why
		})
	conv.at = time.Now()
	c.h.waitUntil(t, 10*time.Second, "two more rounds of every object",
		func() (bool, string) {
			for _, obj := range conv.objs {
				rec := obj.agent.Record(obj.key)
				if len(rec.Syncups) != obj.syncups {
					t.Fatalf("%s got syncup %d after the cluster converged: %v",
						obj.name, len(rec.Syncups), rec.Syncups[len(rec.Syncups)-1])
				}
				if len(rec.Rounds) < obj.rounds+2 {
					return false, fmt.Sprintf("%s: %d rounds since, of 2",
						obj.name, len(rec.Rounds)-obj.rounds)
				}
			}
			return true, ""
		})
	ok, why, after := c.convergedNow(t)
	if !ok {
		t.Fatalf("the cluster stopped reading converged: %s", why)
	}
	if after.spRev != conv.spRev {
		t.Fatalf("SpRev moved from %d to %d after the cluster converged",
			conv.spRev, after.spRev)
	}
	for id, rev := range conv.dnRevs {
		if after.dnRevs[id] != rev {
			t.Fatalf("dn %d's revision moved from %d to %d", id, rev, after.dnRevs[id])
		}
	}
	for id, rev := range conv.cnRevs {
		if after.cnRevs[id] != rev {
			t.Fatalf("cn %d's revision moved from %d to %d", id, rev, after.cnRevs[id])
		}
	}
	return conv
}
