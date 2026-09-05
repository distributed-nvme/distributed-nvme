// Package worker is dnv-worker's control-plane engine (dnv-worker.md): the
// vote layer that registers this process and computes shard ownership (§6),
// the shard workers that watch one revision prefix each (§7), the per-object
// revision workers that drive the agents through Syncup*/Check* (§8), the
// health bookkeeping (§9), the bitmap pushes (§10) and the automatic
// reactions (§11).
//
// It talks to etcd only through etcdutil and to the agents only as a gRPC
// client (layout.md §3); it never serves gRPC and never talks to hosts.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
)

// The normative msg strings of dnv-worker.md §12. The integration suite (§14)
// greps them, so they are constants and never formatted.
const (
	msgWorkerStarting   = "worker starting"
	msgWorkerStopping   = "worker stopping"
	msgWorkerRegistered = "worker registered"
	msgWorkerFenced     = "worker fenced"

	msgMembershipObserved  = "membership observed"
	msgMembershipCommitted = "membership committed"

	msgShardOwned    = "shard owned"
	msgShardReleased = "shard released"

	msgRevisionWorkerStarted = "revision worker started"
	msgRevisionWorkerStopped = "revision worker stopped"

	msgClusterConfMissing = "cluster conf missing"

	msgSyncupResult   = "syncup result"
	msgSyncupRejected = "syncup rejected"

	msgHealthChanged = "health changed"
)

// Config is everything cmd/dnv-worker passes to Run (CM1, CM4).
type Config struct {
	// Roles is the non-empty, duplicate-free subset of common.WorkerRoleDn,
	// WorkerRoleCn and WorkerRoleSp this process carries. Each role is
	// registered, voted and driven independently (VW10).
	Roles []string
	// VoteInterval is the registry heartbeat period (VW2). The dead
	// threshold is 2 x this and is never a constant of its own (§2.1).
	VoteInterval time.Duration
	// GraceTime is how long an observed membership transition must hold
	// before it is committed (VW5).
	GraceTime time.Duration
	// Endpoints are the etcd endpoints the client was built with. The worker
	// never dials them itself; they exist for the "worker starting" record
	// (CM6).
	Endpoints []string
}

// deadThreshold is 2 x the vote interval: a registration whose put has not
// been observed for this long is dead (VW3), and a worker whose own heartbeat
// or watch has been silent for this long fences itself (VW8).
func (c Config) deadThreshold() time.Duration {
	return 2 * c.VoteInterval
}

// ---------------------------------------------------------------------------
// Clock (testability)
// ---------------------------------------------------------------------------

// timerHandle is one pending single-shot timer. C fires once; stop cancels it
// and reports whether it had not fired yet.
type timerHandle struct {
	C    <-chan time.Time
	stop func() bool
}

// tickerHandle is one repeating timer.
type tickerHandle struct {
	C    <-chan time.Time
	stop func()
}

// clock is the package's single source of time. Every timer and every "now"
// of worker/ goes through it so that the unit tests of §13 can drive the vote
// grace windows, the liveness deadlines and the round timers deterministically
// instead of sleeping.
//
// Liveness is judged on monotonic readings (VW4): now() returns a time.Time
// carrying a monotonic component and callers compare with Sub, never with
// wall-clock arithmetic on a stored epoch. nowUnix() is the wall clock and is
// used only where a unix-seconds VALUE is stored (WorkerReg.epoch, err_epoch).
type clock interface {
	now() time.Time
	nowUnix() uint64
	newTimer(d time.Duration) *timerHandle
	newTicker(d time.Duration) *tickerHandle
	after(d time.Duration) <-chan time.Time
}

// realClock is the production clock.
type realClock struct{}

func (realClock) now() time.Time {
	return time.Now()
}

func (realClock) nowUnix() uint64 {
	return uint64(time.Now().Unix())
}

func (realClock) newTimer(d time.Duration) *timerHandle {
	t := time.NewTimer(d)
	return &timerHandle{C: t.C, stop: t.Stop}
}

func (realClock) newTicker(d time.Duration) *tickerHandle {
	t := time.NewTicker(d)
	return &tickerHandle{C: t.C, stop: t.Stop}
}

func (realClock) after(d time.Duration) <-chan time.Time {
	return time.After(d)
}

// ---------------------------------------------------------------------------
// Shared dependencies
// ---------------------------------------------------------------------------

// etcdStore is the etcd surface worker/ uses (EU2, EU3). *etcdutil.Client
// implements it as-is; the unit tests substitute in-memory fakes for the vote
// registry, the rev prefixes and the ClusterConf prefix. Everything else in
// the package that touches etcd does so through model, which takes the
// concrete client (deps.cli).
type etcdStore interface {
	Get(ctx context.Context, key string, msg proto.Message) (bool, error)
	Put(ctx context.Context, key string, msg proto.Message) error
	Delete(ctx context.Context, key string) error
	Range(ctx context.Context, prefix string) ([]etcdutil.KV, int64, error)
	Decode(ctx context.Context, kv etcdutil.KV, msg proto.Message) error
	WatchTyped(
		ctx context.Context,
		prefix string,
		fromRev int64,
		newMsg func() proto.Message,
	) (<-chan etcdutil.Event, <-chan error)
}

// deps is what every layer of the worker shares: one etcd client, one
// ClusterConf cache (RW21), one connection cache (RW7), one health writer
// (HL1/HL2) and one clock.
type deps struct {
	cfg    Config
	store  etcdStore
	cli    *etcdutil.Client
	conf   *confCache
	conns  *connCache
	health healthWriter
	clk    clock
	// kinds resolves a role's shard plumbing (SW1). It is a field so the §13
	// vote tests can substitute recording fakes for the real shard and
	// revision workers.
	kinds func(role string) (revKind, bool)
}

// ---------------------------------------------------------------------------
// Seed (VW1)
// ---------------------------------------------------------------------------

// newSeed mints an incarnation's seed (VW1): 16 bytes from crypto/rand with
// the RFC 4122 version (4) and variant (0b10) bits set, rendered canonically
// as lower-case 8-4-4-4-12 hex. Shaped by hand exactly like
// common.NvmeHostId, so the package needs no uuid dependency.
func newSeed() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("worker seed: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hexed[0:8], hexed[8:12], hexed[12:16], hexed[16:20], hexed[20:32]), nil
}

// seedPrefix is the first 8 characters of a seed, the identity half of every
// trace id this worker mints (RW10). The §14 suite matches
// trace_id | split("-")[0] against it.
func seedPrefix(seed string) string {
	if len(seed) < 8 {
		return seed
	}
	return seed[:8]
}

// newTraceCtx returns ctx under a fresh worker trace id (RW10):
// "{seed[:8]}-{common.NewTraceId()}", so an agent's log attributes every
// request to the worker incarnation that sent it.
func newTraceCtx(ctx context.Context, seed string) context.Context {
	return common.WithTraceId(ctx, seedPrefix(seed)+"-"+common.NewTraceId())
}

// ---------------------------------------------------------------------------
// Run (CM4, CM5)
// ---------------------------------------------------------------------------

// Run mints the incarnation seed, starts the process-wide ClusterConf cache
// (RW21) and the vote worker (§6), and blocks until ctx ends (CM4).
//
// On shutdown it performs CM5 in order: stop the heartbeat loop; delete this
// worker's own registrations best-effort so peers start their grace windows
// now rather than after the dead threshold; stop every shard worker
// gracefully and in parallel (SW5 -> RW11, which may take up to
// common.DefaultWorkerSyncupTimeout); close the cache watch; close the etcd
// client. It returns nil: nothing about a shutdown is an error.
func Run(ctx context.Context, cli *etcdutil.Client, cfg Config) error {
	seed, err := newSeed()
	if err != nil {
		return err
	}
	d := &deps{
		cfg:    cfg,
		store:  cli,
		cli:    cli,
		conns:  newConnCache(),
		health: &modelHealthWriter{cli: cli},
		clk:    realClock{},
		kinds:  kindFor,
	}
	d.conf = newConfCache(d)

	slog.InfoContext(ctx, msgWorkerStarting,
		slog.Any("roles", cfg.Roles),
		slog.String("seed", seed),
		slog.Any("endpoints", cfg.Endpoints),
		slog.Float64("vote_interval", cfg.VoteInterval.Seconds()),
		slog.Float64("grace_time", cfg.GraceTime.Seconds()),
	)

	// The cache and the vote layer live as long as ctx. The cache is stopped
	// after the vote layer, because a revision worker draining its last
	// syncup still reads the cluster conf (RW9).
	confCtx, confCancel := context.WithCancel(context.WithoutCancel(ctx))
	var confWg sync.WaitGroup
	confWg.Add(1)
	go func() {
		defer confWg.Done()
		d.conf.run(confCtx)
	}()

	vote := newVoteWorker(d, seed)
	// run returns only after CM5's first three steps are done.
	vote.run(ctx)

	stopSeed := vote.currentSeed()

	confCancel()
	confWg.Wait()

	if err := cli.Close(); err != nil {
		slog.ErrorContext(ctx, "etcd client close failed",
			slog.String("error", err.Error()),
		)
	}

	slog.InfoContext(ctx, msgWorkerStopping, slog.String("seed", stopSeed))
	return nil
}

// shutdownCtx bounds one best-effort etcd operation of the CM5 shutdown path.
// The parent ctx is already cancelled by then, so a plain child would be dead
// on arrival: the deletes run under a fresh ctx that keeps the parent's values
// (the trace id) but not its cancellation.
func shutdownCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(
		context.WithoutCancel(ctx),
		common.DefaultEtcdOpTimeout*time.Second,
	)
}

// shardCode renders a shard the way every dnv key and log record does
// (common.ShardCodeFmt).
func shardCode(shard uint32) string {
	return fmt.Sprintf(common.ShardCodeFmt, shard)
}

// workerRegKey is model.WorkerRegKey, wrapped so the vote worker reads the
// same way everywhere.
func workerRegKey(role string, seed string) string {
	return model.WorkerRegKey(role, seed)
}
