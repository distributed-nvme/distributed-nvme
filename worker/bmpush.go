package worker

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
)

// The bitmap pushes of dnv-worker.md §10 (BM1-BM6), the worker half of
// architecture.md §9.6: the skip bitmaps of §8.9/§8.11 reach the data plane
// exclusively through the unary PushMigrBitmap / PushCloneBitmap RPCs, never
// through a Syncup*.
//
// One pusher belongs to one sp child (a side or a cntlr, §8.4) and sequences
// that child's chunks: one push in flight per migration / clone, ascending
// bm_idx, the next part only after a code == 0 reply (BM3). Different
// migrations / clones of one child push independently and may run
// concurrently toward the same agent, which is what §9.6 step 4 allows and
// step 3 bounds.

// msgBitmapPushed is the §12 record of one delivered chunk (BM3).
const msgBitmapPushed = "bitmap pushed"

// msgBitmapPushFailed is the non-normative companion of msgBitmapPushed: a
// push that never produced an AgentReply has no code to report, so it is
// logged under its own msg rather than as a "bitmap pushed" with an invented
// code 0.
const msgBitmapPushFailed = "bitmap push failed"

// The "kind" attribute of the §12 "bitmap pushed" record (BM1).
const (
	bmKindMigr  = "migr"
	bmKindClone = "clone"
)

// errPusherStopped is what a pusher reports once its child is stopping.
var errPusherStopped = errors.New("bitmap pusher stopped")

// bmPart is one chunk on its way to an agent (BM3).
type bmPart struct {
	// resId is the migr_id or the clone_id the chunk belongs to.
	resId uint64
	// name is the migration / clone name, i.e. the etcd key suffix the chunk
	// value is read at (BM1: values are read one at a time, when pushed).
	name string
	// bmIdx is the chunk index: the append sequence for a migration, the
	// source slice_idx for a clone (§9.6).
	bmIdx uint32
	// revision is the object's SYNCED revision (BM3).
	revision uint64
	bitmap   []byte
}

// bmPlan is the work one Syncup* reply's diff produced for ONE migration or
// clone (BM2): the chunks the agent does not hold — ascending by bm_idx, which
// is what makes a migration's chunk concatenation interpretable (§9.6) — plus
// the revision every push of the batch carries.
type bmPlan struct {
	resId    uint64
	name     string
	revision uint64
	parts    []model.BmChunk
}

// bmMemoKey is the BM5 memo's key: one chunk of one clone / migration.
type bmMemoKey struct {
	resId uint64
	bmIdx uint32
}

// bmPusherParams is everything a pusher needs at construction. fetch and
// deliver are the two kind-specific halves — where the chunk comes from and
// which RPC carries it — so that BM1-BM6 are implemented once for both kinds
// and the §13 tests can drive them without an agent.
type bmPusherParams struct {
	deps *deps
	seed string
	// kind is bmKindMigr or bmKindClone, the §12 "kind" attribute.
	kind string
	// addrPort is the agent this child drives; BM4's target rule is enforced
	// by the caller, which submits a plan only for the destination side's DN
	// resp. the primary cntlr's CN.
	addrPort string
	// idAttr is the name of the resource-id attribute of the §12 record:
	// "migr_id" or "clone_id".
	idAttr string
	// ids are the object's own ids, as the §12 records carry them.
	ids []slog.Attr
	// fetch reads one chunk's VALUE out of etcd (BM1). It reports found =
	// false when the key is gone.
	fetch func(
		ctx context.Context,
		name string,
		bmIdx uint32,
	) ([]byte, bool, error)
	// deliver issues the kind's Push*Bitmap and returns the AgentReply's
	// code and details.
	deliver func(
		ctx context.Context,
		conn *grpc.ClientConn,
		part bmPart,
	) (uint32, string, error)
}

// bmPusher is the push engine of one sp child (§10). Its public surface is
// three calls from the child's loop goroutine — missing (BM2/BM5), submit
// (BM3) and takeFailed (BM6) — plus stop, which the coordinator makes after
// joining the child's loop.
type bmPusher struct {
	deps     *deps
	seed     string
	kind     string
	addrPort string
	idAttr   string
	ids      []slog.Attr
	fetch    func(
		ctx context.Context,
		name string,
		bmIdx uint32,
	) ([]byte, bool, error)
	deliver func(
		ctx context.Context,
		conn *grpc.ClientConn,
		part bmPart,
	) (uint32, string, error)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu sync.Mutex
	// running holds the res_ids a runner goroutine is working on, and next
	// the newest plan submitted while one was running: that is BM3's "one in
	// flight per migration/clone", with the later diff superseding the
	// earlier one rather than queueing behind it.
	running map[uint64]bool
	next    map[uint64]*bmPlan
	// memo is BM5: per (res_id, bm_idx) the etcd mod_revision of the chunk
	// last pushed. In-memory and lost on a handoff, as [D8] accepts.
	memo     map[bmMemoKey]int64
	conn     *grpc.ClientConn
	connHeld bool
	stopped  bool
	failed   bool
}

// newBmPusher builds one child's push engine (§10).
func newBmPusher(p bmPusherParams) *bmPusher {
	ctx, cancel := context.WithCancel(context.Background())
	return &bmPusher{
		deps:     p.deps,
		seed:     p.seed,
		kind:     p.kind,
		addrPort: p.addrPort,
		idAttr:   p.idAttr,
		ids:      p.ids,
		fetch:    p.fetch,
		deliver:  p.deliver,
		ctx:      ctx,
		cancel:   cancel,
		running:  make(map[uint64]bool),
		next:     make(map[uint64]*bmPlan),
		memo:     make(map[bmMemoKey]int64),
	}
}

// missing is the BM2 diff of one migration / clone, refined by BM5.
//
// chunks are the indexes etcd holds (SpState.MigrBmIdx / CloneBmIdx, MD3) and
// applied is the agent's acknowledged set (BitmapInfo.bm_idx_list). A chunk
// the agent does not list is missing. A chunk it does list is re-pushed only
// when THIS worker pushed it before and its etcd mod_revision has advanced
// since — AppendCloneBitmap may grow a chunk that keeps its index (BM5).
//
// architecture.md [D8] describes the same memo by the byte length last
// pushed; dnv-worker.md BM5 is the normative rule for this implementation and
// uses the mod_revision, which needs no chunk value to decide.
//
// The result is ascending by bm_idx (BM3).
func (p *bmPusher) missing(
	resId uint64,
	chunks []model.BmChunk,
	applied []uint32,
) []model.BmChunk {
	have := make(map[uint32]struct{}, len(applied))
	for _, idx := range applied {
		have[idx] = struct{}{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]model.BmChunk, 0, len(chunks))
	for _, chunk := range chunks {
		if _, ok := have[chunk.Idx]; !ok {
			out = append(out, chunk)
			continue
		}
		pushed, ok := p.memo[bmMemoKey{resId: resId, bmIdx: chunk.Idx}]
		if ok && chunk.ModRev > pushed {
			out = append(out, chunk)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Idx < out[j].Idx })
	return out
}

// submit hands one migration's / clone's missing chunks to the engine (BM3).
// A plan for a res_id that is already being pushed replaces the pending one
// instead of starting a second runner, so exactly one push per res_id is ever
// in flight; res_ids run independently of each other.
func (p *bmPusher) submit(plan *bmPlan) {
	if plan == nil || len(plan.parts) == 0 {
		return
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	if p.running[plan.resId] {
		p.next[plan.resId] = plan
		p.mu.Unlock()
		return
	}
	p.running[plan.resId] = true
	p.wg.Add(1)
	p.mu.Unlock()
	go p.run(plan)
}

// takeFailed reports and clears the BM6 flag: a push that failed or was
// rejected asks the child's loop for an equal-revision re-apply, whose reply
// restarts the diff. There is no push-specific timer.
func (p *bmPusher) takeFailed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	failed := p.failed
	p.failed = false
	return failed
}

// stop ends the engine and joins its runners. An in-flight push is allowed to
// finish — its own DefaultWorkerPushTimeout deadline bounds the wait, exactly
// as RW11 bounds an in-flight Syncup* — and the connection reference is given
// back afterwards.
func (p *bmPusher) stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	p.next = make(map[uint64]*bmPlan)
	p.mu.Unlock()
	p.cancel()
	p.wg.Wait()
	p.mu.Lock()
	held := p.connHeld
	p.connHeld = false
	p.conn = nil
	p.mu.Unlock()
	if held {
		p.deps.conns.release(p.addrPort)
	}
}

// run is one res_id's runner: it pushes a plan's parts one at a time and then
// picks up whatever newer plan arrived meanwhile (BM3).
func (p *bmPusher) run(plan *bmPlan) {
	defer p.wg.Done()
	for plan != nil {
		p.pushPlan(plan)
		p.mu.Lock()
		next, ok := p.next[plan.resId]
		if !ok || p.stopped {
			delete(p.running, plan.resId)
			delete(p.next, plan.resId)
			p.mu.Unlock()
			return
		}
		delete(p.next, plan.resId)
		p.mu.Unlock()
		plan = next
	}
}

// pushPlan delivers one plan's chunks in ascending bm_idx, one at a time
// (BM3). Every failure aborts the rest of the plan and raises the BM6 flag:
// the next round's equal-revision Syncup* reply produces a fresh diff, so
// nothing is lost by giving up here — and nothing would be retried without
// it, because a diff only ever comes from a Syncup* reply.
func (p *bmPusher) pushPlan(plan *bmPlan) {
	conn, err := p.connect()
	if err != nil {
		if !errors.Is(err, errPusherStopped) {
			// RW10: a push that never got as far as a chunk is still a push,
			// so its record carries a trace id of this worker's incarnation
			// like every other one.
			p.setFailed()
			slog.InfoContext(
				newTraceCtx(p.ctx, p.seed), msgBitmapPushFailed, append(
					p.attrs(plan.resId, 0), slog.String("error", err.Error()),
				)...,
			)
		}
		return
	}
	for _, chunk := range plan.parts {
		if p.ctx.Err() != nil {
			return
		}
		if !p.pushOne(conn, plan, chunk) {
			return
		}
	}
}

// pushOne reads one chunk's value (BM1) and delivers it (BM3). It reports
// whether the plan may continue.
func (p *bmPusher) pushOne(
	conn *grpc.ClientConn,
	plan *bmPlan,
	chunk model.BmChunk,
) bool {
	ctx := newTraceCtx(p.ctx, p.seed)
	bitmap, found, err := p.fetch(ctx, plan.name, chunk.Idx)
	if err != nil || !found {
		// The chunk index came from the coordinator's snapshot, so a value
		// that cannot be read now is either an etcd failure or a chunk
		// deleted with its object. Both raise the BM6 flag: without it this
		// child would never diff again until the next revision bump.
		p.setFailed()
		attrs := p.attrs(plan.resId, chunk.Idx)
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
		} else {
			attrs = append(attrs, slog.String("error", "chunk not found"))
		}
		slog.InfoContext(ctx, msgBitmapPushFailed, attrs...)
		return false
	}
	rpcCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		common.DefaultWorkerPushTimeout*time.Second,
	)
	defer cancel()
	code, details, err := p.deliver(rpcCtx, conn, bmPart{
		resId:    plan.resId,
		name:     plan.name,
		bmIdx:    chunk.Idx,
		revision: plan.revision,
		bitmap:   bitmap,
	})
	if err != nil {
		p.setFailed()
		slog.InfoContext(ctx, msgBitmapPushFailed, append(
			p.attrs(plan.resId, chunk.Idx),
			slog.String("error", err.Error()),
		)...)
		return false
	}
	attrs := append(p.attrs(plan.resId, chunk.Idx),
		slog.Uint64("code", uint64(code)),
	)
	slog.InfoContext(ctx, msgBitmapPushed, attrs...)
	if code != 0 {
		// A stale revision, or an introducing Syncup* the agent has not
		// applied yet (BM6). The §12 record above carries only the code, so
		// the agent's own explanation is logged next to it.
		slog.InfoContext(ctx, msgBitmapPushFailed, append(
			p.attrs(plan.resId, chunk.Idx),
			slog.Uint64("code", uint64(code)),
			slog.String("details", details),
		)...)
		p.setFailed()
		return false
	}
	p.memoize(plan.resId, chunk)
	return true
}

// memoize records the mod_revision of a chunk that reached its agent (BM5).
func (p *bmPusher) memoize(resId uint64, chunk model.BmChunk) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.memo[bmMemoKey{resId: resId, bmIdx: chunk.Idx}] = chunk.ModRev
}

// setFailed raises the BM6 flag.
func (p *bmPusher) setFailed() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failed = true
}

// connect takes this pusher's own reference to the child's agent connection
// (RW7). The pushes run off the child's loop goroutine, so they may not share
// the loop's reference; the endpoint never changes under a child, because a
// moved side or cntlr is a restarted child (RW14).
func (p *bmPusher) connect() (*grpc.ClientConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return nil, errPusherStopped
	}
	if p.connHeld {
		return p.conn, nil
	}
	conn, err := p.deps.conns.acquire(p.addrPort)
	if err != nil {
		return nil, err
	}
	p.conn = conn
	p.connHeld = true
	return conn, nil
}

// attrs are the §12 "bitmap pushed" attributes of one chunk: kind, the
// object's ids, the resource id and bm_idx.
func (p *bmPusher) attrs(resId uint64, bmIdx uint32) []any {
	attrs := make([]any, 0, len(p.ids)+4)
	attrs = append(attrs, slog.String("kind", p.kind))
	for _, attr := range p.ids {
		attrs = append(attrs, attr)
	}
	return append(attrs,
		slog.Uint64(p.idAttr, resId),
		slog.Uint64("bm_idx", uint64(bmIdx)),
	)
}
