package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// errRoundTimeout is RW4 step 4's "no reply within the round timeout" (RW8).
var errRoundTimeout = errors.New("round timeout")

// errRoundAborted ends a round that a desired change overtook (RW6). The
// pending reply would answer the request built from the SUPERSEDED revision,
// so the round is given up together with its stream. It is NOT a health
// failure: the agent answered nothing wrong and a moved node (RW13) was never
// unreachable (HL1).
var errRoundAborted = errors.New("round aborted")

// desiredState is the mutable half of a rev value (architecture.md §5.5,
// RW2): the revision to reach plus the handle — addr_port for DnRev/CnRev,
// sp_name for SpRev. A put that changes neither is a no-op (RW3).
type desiredState struct {
	revision uint64
	handle   string
}

// replyState is what the generic loop needs from one Check*/Syncup* reply.
// The kind's *Info stays inside the driver that decoded it: the loop steers
// only on these fields, and the driver folds the info into health and the
// kind-specific consequences from its own observe (HL4).
type replyState struct {
	// revision is the agent's last fully applied revision for the object.
	revision uint64
	// code and details are the reply's AgentReply.
	code    uint32
	details string
	// infoPresent reports whether this reply carried an *Info at all. A
	// show_info = false reply carries one only when something changed, so
	// health is evaluated on the LATEST KNOWN info either way (HL5).
	infoPresent bool
}

// checkStream is one object's Check* bidirectional stream (RW4,
// architecture.md §9.7). recv blocks; the caller bounds it with the round
// timer and closes the stream when the timer wins (RW4 step 4).
type checkStream interface {
	send(revision uint64, showInfo bool) error
	recv() (*replyState, error)
	closeSend() error
}

// objDriver is the kind-specific half of the per-object loop (RW1). Every
// method runs on the revision worker's own goroutine, so an implementation
// needs no locking; the loop below is the same code for a DN, a CN, an sp
// side and an sp cntlr.
type objDriver interface {
	// logAttrs returns the object's ids as the §12 records that list "ids"
	// carry them (syncup result, syncup rejected, health changed).
	logAttrs() []slog.Attr
	// childAttrs returns the extra attributes an sp child's "revision worker
	// started"/"stopped" records carry (side_pointer / cntlr_pointer); nil
	// for a dn or a cn.
	childAttrs() []slog.Attr
	// addrPort is the agent endpoint the object is driven at. A change
	// re-syncs the object at its new endpoint (RW7, RW13).
	addrPort() string
	// interval is the kind's round period, read from the ClusterConf cache
	// every round (RW9, RW8).
	interval(cc *pb.ClusterConf) time.Duration
	// setDesired installs a new desired state (RW3). The loop has already
	// established that something changed.
	setDesired(d desiredState)
	// openStream opens the kind's Check* stream over conn (RW4 step 1).
	openStream(
		ctx context.Context,
		conn *grpc.ClientConn,
	) (checkStream, error)
	// syncup issues the kind's unary Syncup* (RW5). A nil reply with a nil
	// error means the driver deliberately sent nothing — a DN whose DnConf
	// is missing, say (RW13) — and the round retries next time.
	syncup(
		ctx context.Context,
		conn *grpc.ClientConn,
		cc *pb.ClusterConf,
	) (*replyState, error)
	// observe folds one reply — from a round or from a syncup, which are the
	// same thing here (HL4) — into health and the kind's own consequences
	// (the flips of RW18/RW19, the pushes of §10).
	observe(ctx context.Context, r *replyState)
	// unreachable folds a stream that cannot be opened, breaks or misses its
	// reply into health (HL1/HL2).
	unreachable(ctx context.Context)
}

// roundPeriod turns a stored health-check interval into the round period
// (RW9, RW8). A zero cannot reach it: the pass gate in run() refuses an
// invalid stored conf before the interval is ever read, and the gateway wrote
// a concrete value in the first place. The guard is kept only so that a bug
// upstream of it degrades into a slow loop rather than a hot one — it is a
// backstop against spinning, never a default anything is computed with.
func roundPeriod(seconds uint32) time.Duration {
	if seconds == 0 {
		seconds = common.DefaultHealthCheckInterval
	}
	return time.Duration(seconds) * time.Second
}

// revWorkerHandle is what a parent holds for one running revision worker
// (SW3): a coalescing desired-state update and a graceful stop.
type revWorkerHandle interface {
	// update delivers a new desired state (RW3). It never blocks.
	update(d desiredState)
	// stop cancels the worker and joins it (RW11, SW5).
	stop()
}

// revWorkerParams is everything a revision worker needs at construction.
type revWorkerParams struct {
	deps    *deps
	role    string
	shard   uint32
	cid     uint64
	id      uint64
	seed    string
	desired desiredState
}

// revWorker is the generic per-object loop of §8.1 (RW1-RW12), shared by the
// dn, cn, side and cntlr object kinds. One goroutine owns the object's
// Check* stream, its in-memory state and every RPC about it, which is what
// makes architecture.md §9.1's "one Syncup* at a time per object" and §9.7's
// "one stream per object" hold by construction.
type revWorker struct {
	deps   *deps
	driver objDriver
	role   string
	shard  uint32
	cid    uint64
	id     uint64
	seed   string

	// desiredCh has capacity one and is overwritten, so the loop only ever
	// sees the latest desired state (RW3).
	desiredCh chan desiredState
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}

	// State owned by the loop goroutine (RW2).
	desired      desiredState
	synced       uint64
	resyncWanted bool
	stream       *streamState
	conn         *grpc.ClientConn
	connAddr     string
	idleLogged   bool
	// confRefused memoizes the stored-conf error last logged, so a steady
	// invalid conf costs one Error record rather than one per round, and a
	// conf that changes from one invalid value to another still reports.
	confRefused string
}

// streamState is one open Check* stream plus the goroutine that turns its
// blocking recv into a channel the round timer can race (RW4 step 3).
type streamState struct {
	stream checkStream
	cancel context.CancelFunc
	recvCh chan recvResult
	done   chan struct{}
}

// recvResult is one reply or one stream error.
type recvResult struct {
	reply *replyState
	err   error
}

// startRevWorker builds a revision worker, hands it to newDriver so the
// kind-specific half can call back into it (BM6's wantResync,
// syncedRevision), and starts its goroutine.
func startRevWorker(
	p revWorkerParams,
	newDriver func(w *revWorker) objDriver,
) *revWorker {
	ctx, cancel := context.WithCancel(context.Background())
	w := &revWorker{
		deps:      p.deps,
		role:      p.role,
		shard:     p.shard,
		cid:       p.cid,
		id:        p.id,
		seed:      p.seed,
		desiredCh: make(chan desiredState, 1),
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		desired:   p.desired,
	}
	w.driver = newDriver(w)
	go w.run()
	return w
}

// update delivers a new desired state, overwriting an undelivered one (RW3).
// It is called by the single parent goroutine (the shard worker or the sp
// coordinator) and never blocks.
func (w *revWorker) update(d desiredState) {
	select {
	case <-w.desiredCh:
	default:
	}
	select {
	case w.desiredCh <- d:
	default:
	}
}

// stop cancels the worker and joins it (RW11). An in-flight unary call is
// allowed to finish first — its own deadline bounds the wait — so this can
// take up to common.DefaultWorkerSyncupTimeout.
func (w *revWorker) stop() {
	w.cancel()
	<-w.done
}

// wantResync marks the object for an equal-revision re-apply on the next
// round (BM6, RW4 step 6). A driver calls it when a Push*Bitmap failed.
func (w *revWorker) wantResync() {
	w.resyncWanted = true
}

// syncedRevision is the last revision the agent acknowledged (RW2); the
// bitmap pushes of §10 carry it.
func (w *revWorker) syncedRevision() uint64 {
	return w.synced
}

// desiredRevision is the revision the object is being driven to (RW2).
func (w *revWorker) desiredRevision() uint64 {
	return w.desired.revision
}

// run is the per-object loop (RW4). Its seven steps are round(); step 7 —
// re-arming the timer AFTER the round, so a slow round never queues a burst
// of catch-up rounds (RW8) — is wait().
func (w *revWorker) run() {
	defer close(w.done)
	slog.InfoContext(w.ctx, msgRevisionWorkerStarted, w.lifecycleAttrs()...)
	defer func() {
		w.cleanup()
		// The stop ctx is done by now; the record still has to come out.
		slog.InfoContext(
			context.WithoutCancel(w.ctx),
			msgRevisionWorkerStopped,
			w.lifecycleAttrs()...,
		)
	}()
	for {
		cc, ok := w.deps.conf.get(w.cid)
		if !ok {
			// RW9/SW6: an unknown cluster makes the loop idle — no stream, no
			// syncup, one record per idle period, a retry every
			// DefaultHealthCheckInterval seconds. The worker never guesses
			// defaults for a cluster it cannot read.
			w.idle()
			if !w.wait(
				common.DefaultHealthCheckInterval*time.Second, nil,
			) {
				return
			}
			continue
		}
		if err := model.ValidateClusterConf(cc); err != nil {
			// §7: the gateway stores concrete values, so a zero or an
			// out-of-range member here is corruption or foreign data, and
			// this object's pass is refused rather than computed with a
			// guessed geometry. The refusal reaches nothing: round() is
			// skipped, so no stream is opened, no Syncup* is sent and no
			// err_epoch is written, and wait() is passed a nil cc so a
			// revision bump arriving meanwhile is recorded (RW3) without
			// sending anything either. It retries every round, like the
			// unknown-cluster arm above, because an operator recreating the
			// cluster is what fixes it.
			w.refuseConf(err)
			if !w.wait(
				common.DefaultHealthCheckInterval*time.Second, nil,
			) {
				return
			}
			continue
		}
		w.idleLogged = false
		w.confRefused = ""
		interval := w.driver.interval(cc)
		w.round(cc, interval)
		if !w.wait(interval, cc) {
			return
		}
	}
}

// round runs one Check round (RW4 steps 1-6).
func (w *revWorker) round(cc *pb.ClusterConf, interval time.Duration) {
	ctx := newTraceCtx(w.ctx, w.seed)

	// RW6: a desired change that arrived while the previous round was still
	// running is applied — and synced — BEFORE this round is built, so step 2
	// never carries a superseded revision and step 1 opens its stream at the
	// endpoint the parent last named (RW13).
	w.drainDesired(cc)
	// Step 1: no stream => open one over the cached connection (RW7); a
	// failure to open counts as a broken stream.
	if w.stream == nil {
		if !w.openStream(ctx) {
			w.fail(ctx)
			return
		}
	}
	// Step 2.
	if err := w.stream.stream.send(w.desired.revision, false); err != nil {
		slog.InfoContext(ctx, "check stream send failed",
			append(w.idAttrs(), slog.String("error", err.Error()))...,
		)
		w.fail(ctx)
		return
	}
	// Steps 3 and 4. A reply that arrives after the round timeout MUST NOT be
	// mistaken for the next round's reply; step 4 is what makes that
	// impossible, because a missed reply closes the stream and the next round
	// opens a fresh one. Never reuse a stream across a round timeout.
	reply, err := w.recv(cc, interval)
	if err != nil {
		if errors.Is(err, errRoundAborted) {
			// RW6 overtook the round: the change has already been synced and
			// the superseded round's stream is gone. The next round opens a
			// fresh one — at the new endpoint when RW13 moved the object —
			// and nothing here is a health verdict.
			return
		}
		slog.InfoContext(ctx, "check round failed",
			append(w.idAttrs(), slog.String("error", err.Error()))...,
		)
		w.fail(ctx)
		return
	}
	// RW2: `synced` is the last revision the agent ACKNOWLEDGED — "the
	// revision of a code == 0 Syncup* reply OR of a Check* reply". A clean
	// round after a shard handoff, a worker restart or an agent restart is
	// such an acknowledgement even though it needs no Syncup*, and it is this
	// revision the §10 bitmap pushes carry (BM3).
	if reply.code == 0 {
		w.synced = reply.revision
	}
	// Step 5: process the reply's *Info if present (HL4/HL5), then re-sync
	// when the agent rejected the request or holds another revision — which
	// is also how the first sync after a shard handoff, a worker restart or
	// an agent restart happens, with no recovery step of its own.
	w.driver.observe(ctx, reply)
	needSyncup := reply.code != 0 || reply.revision != w.desired.revision
	// Step 6: an equal-revision re-apply after a failed push (BM6). Folded
	// into the same call so one round never issues two Syncup*.
	if w.resyncWanted {
		needSyncup = true
	}
	if needSyncup {
		w.syncup(ctx, cc)
	}
}

// fail is RW4 step 4: close the stream and report the object unreachable
// (HL1/HL2). A worker that is stopping reports nothing — a cancelled stop ctx
// is not a sick agent, and writing "unreachable" on the way out would restart
// the §11 threshold clock of a perfectly healthy object.
func (w *revWorker) fail(ctx context.Context) {
	w.dropStream()
	if w.ctx.Err() != nil {
		return
	}
	w.driver.unreachable(ctx)
}

// wait is RW4 step 7: it arms the round timer AFTER the round (RW8) and stays
// responsive to a desired change (RW6) and to a graceful stop (RW11). cc is
// nil while the loop idles (RW9), where a desired change is recorded but
// nothing is sent. It returns false when the worker must stop.
func (w *revWorker) wait(d time.Duration, cc *pb.ClusterConf) bool {
	timer := w.deps.clk.newTimer(d)
	defer timer.stop()
	for {
		select {
		case <-w.ctx.Done():
			return false
		case <-timer.C:
			return true
		case next := <-w.desiredCh:
			w.applyDesired(next, cc)
		}
	}
}

// applyDesired installs one desired change and, per RW6, issues its Syncup*
// AT ONCE — from wherever in the loop the change was picked up, the round
// included: RW9 lets an interval be an hour, so waiting for the round would
// leave a failover's SpRev bump (AR5) or a spare switch (AR8) unapplied that
// long. The round timer keeps running, so the cadence of RW8 is unaffected.
//
// cc is nil while the loop idles (RW9): the change is recorded, nothing is
// sent, and the next round syncs it once the cluster conf shows up.
func (w *revWorker) applyDesired(next desiredState, cc *pb.ClusterConf) {
	if next == w.desired {
		// RW3: a put that changes neither revision nor handle.
		return
	}
	w.desired = next
	w.driver.setDesired(next)
	if cc == nil {
		return
	}
	w.syncup(newTraceCtx(w.ctx, w.seed), cc)
}

// drainDesired applies a desired change that is already waiting, without
// blocking (RW3's channel holds at most the latest one).
func (w *revWorker) drainDesired(cc *pb.ClusterConf) {
	select {
	case next := <-w.desiredCh:
		w.applyDesired(next, cc)
	default:
	}
}

// syncup issues one Syncup* and folds its reply into the object's state
// (RW5).
func (w *revWorker) syncup(ctx context.Context, cc *pb.ClusterConf) {
	revision := w.desired.revision
	conn, err := w.connect()
	if err != nil {
		w.logSyncupResult(ctx, revision, 0, err)
		return
	}
	// RW11: the call's own deadline bounds it and the stop ctx is
	// deliberately NOT the RPC ctx, so a graceful stop lets an in-flight
	// Syncup* finish instead of cancelling it.
	rpcCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		common.DefaultWorkerSyncupTimeout*time.Second,
	)
	defer cancel()
	reply, err := w.driver.syncup(rpcCtx, conn, cc)
	if err != nil {
		// A gRPC error is logged and left to the next round (RW5, RW12).
		w.logSyncupResult(ctx, revision, 0, err)
		return
	}
	if reply == nil {
		// The driver sent nothing and said why (RW13's missing DnConf).
		return
	}
	w.logSyncupResult(ctx, revision, reply.code, nil)
	if reply.code != 0 {
		w.logSyncupRejected(ctx, revision, reply)
		// HL1/HL2: code != 0 neither sets nor clears health; observe is still
		// called so the driver can record what it learned.
		w.driver.observe(ctx, reply)
		return
	}
	w.synced = reply.revision
	w.resyncWanted = false
	w.driver.observe(ctx, reply)
}

// openStream opens the object's Check* stream (RW4 step 1) and starts the
// goroutine that feeds its replies to the round. It reports success.
func (w *revWorker) openStream(ctx context.Context) bool {
	conn, err := w.connect()
	if err != nil {
		slog.InfoContext(ctx, "check stream open failed",
			append(w.idAttrs(), slog.String("error", err.Error()))...,
		)
		return false
	}
	// The stream ctx descends from the round's trace ctx, which descends from
	// the worker ctx: a graceful stop kills the stream, and dropStream kills
	// it on its own (RW11).
	sctx, cancel := context.WithCancel(ctx)
	stream, err := w.driver.openStream(sctx, conn)
	if err != nil {
		cancel()
		slog.InfoContext(ctx, "check stream open failed",
			append(w.idAttrs(), slog.String("error", err.Error()))...,
		)
		return false
	}
	state := &streamState{
		stream: stream,
		cancel: cancel,
		recvCh: make(chan recvResult, 1),
		done:   make(chan struct{}),
	}
	go state.pump()
	w.stream = state
	return true
}

// recv waits at most interval for the round's reply (RW4 step 3, RW8) while
// staying responsive to a desired change (RW6): the parent's put must not sit
// in the channel until this round ends, which RW9 allows to be an hour away
// and RW5 a further DefaultWorkerSyncupTimeout on top.
func (w *revWorker) recv(
	cc *pb.ClusterConf,
	interval time.Duration,
) (*replyState, error) {
	timer := w.deps.clk.newTimer(interval)
	defer timer.stop()
	// The stream is captured: applying a desired change drops it, and the
	// round must not then read w.stream, which is nil by that point.
	stream := w.stream
	for {
		select {
		case <-w.ctx.Done():
			return nil, context.Canceled
		case res := <-stream.recvCh:
			if res.err != nil {
				return nil, fmt.Errorf("check stream: %w", res.err)
			}
			return res.reply, nil
		case <-timer.C:
			return nil, errRoundTimeout
		case next := <-w.desiredCh:
			// RW6: apply and sync AT ONCE — this reply may be a whole
			// interval away (RW9) and the parent's change must not wait for
			// it. The round then ends: its reply answers the request built
			// from the superseded revision, and RW4 step 4's rule that a
			// stream is never reused across an abandoned round holds here
			// too, so the stream goes with it (applyDesired has already
			// dropped it when RW13 moved the object). The next round opens a
			// fresh one, which §9.7 answers with the complete *Info again.
			w.applyDesired(next, cc)
			w.dropStream()
			return nil, errRoundAborted
		}
	}
}

// pump turns the stream's blocking recv into a channel (RW4 step 3). It exits
// as soon as the stream is dropped, so a reply that arrives after a round
// timeout is discarded with the stream rather than delivered to a later
// round.
func (s *streamState) pump() {
	for {
		reply, err := s.stream.recv()
		select {
		case s.recvCh <- recvResult{reply: reply, err: err}:
		case <-s.done:
			return
		}
		if err != nil {
			return
		}
	}
}

// dropStream closes the current stream: CloseSend, then cancel (RW11). The
// next round opens a fresh one (RW4 steps 1 and 4).
func (w *revWorker) dropStream() {
	if w.stream == nil {
		return
	}
	close(w.stream.done)
	_ = w.stream.stream.closeSend()
	w.stream.cancel()
	w.stream = nil
}

// connect returns the object's agent connection, acquiring it from the RW7
// cache on first use.
//
// A put whose only change is addr_port re-syncs the node at its NEW endpoint
// (RW13, §10.2): the stream and the connection reference are dropped and the
// loop continues at the new address. No delete ever reaches the agent.
func (w *revWorker) connect() (*grpc.ClientConn, error) {
	addr := w.driver.addrPort()
	if addr == "" {
		return nil, fmt.Errorf("worker %s: empty addr_port", w.role)
	}
	if w.conn != nil && w.connAddr == addr {
		return w.conn, nil
	}
	if w.conn != nil {
		w.dropStream()
		w.deps.conns.release(w.connAddr)
		w.conn = nil
		w.connAddr = ""
	}
	conn, err := w.deps.conns.acquire(addr)
	if err != nil {
		return nil, err
	}
	w.conn = conn
	w.connAddr = addr
	return conn, nil
}

// releaseConn drops this object's reference to its agent connection (RW7).
func (w *revWorker) releaseConn() {
	if w.conn == nil {
		return
	}
	w.deps.conns.release(w.connAddr)
	w.conn = nil
	w.connAddr = ""
}

// idle is the RW9 idle state: no stream, no syncup, and exactly one
// "cluster conf missing" record per idle period — not one per retry.
func (w *revWorker) idle() {
	w.quiesce()
	if w.idleLogged {
		return
	}
	w.idleLogged = true
	slog.InfoContext(w.ctx, msgClusterConfMissing,
		slog.Uint64("cluster_id", w.cid),
	)
}

// refuseConf is idle's twin for a cluster whose stored conf cannot be used
// (§7): the same quiesced state — stream dropped, RW7 connection reference
// released — but its own record, so the §14 grep for "cluster conf missing"
// keeps meaning "the cluster is not in the cache" and nothing else.
func (w *revWorker) refuseConf(err error) {
	w.quiesce()
	w.idleLogged = false
	if w.confRefused == err.Error() {
		return
	}
	w.confRefused = err.Error()
	attrs := append(w.lifecycleAttrs(), slog.String("error", err.Error()))
	slog.ErrorContext(w.ctx, msgInvalidStoredConf, attrs...)
}

// quiesce is the half idle and refuseConf share: the object drives nothing
// until its next round.
func (w *revWorker) quiesce() {
	w.dropStream()
	w.releaseConn()
}

// cleanup is the graceful-stop tail of RW11.
func (w *revWorker) cleanup() {
	w.dropStream()
	w.releaseConn()
}

// lifecycleAttrs are the attributes of the §12 "revision worker started" /
// "revision worker stopped" records.
func (w *revWorker) lifecycleAttrs() []any {
	attrs := []any{
		slog.String("role", w.role),
		slog.String("shard", shardCode(w.shard)),
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("id", w.id),
	}
	for _, attr := range w.driver.childAttrs() {
		attrs = append(attrs, attr)
	}
	return attrs
}

// idAttrs are the "ids" of the §12 syncup records.
func (w *revWorker) idAttrs() []any {
	attrs := make([]any, 0, 6)
	for _, attr := range w.driver.logAttrs() {
		attrs = append(attrs, attr)
	}
	return attrs
}

// logSyncupResult emits the §12 "syncup result" record, which every Syncup*
// reply or failure produces (RW5).
func (w *revWorker) logSyncupResult(
	ctx context.Context,
	revision uint64,
	code uint32,
	err error,
) {
	attrs := append(w.idAttrs(),
		slog.Uint64("revision", revision),
		slog.Uint64("code", uint64(code)),
	)
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	slog.InfoContext(ctx, msgSyncupResult, attrs...)
}

// logSyncupRejected emits the §12 "syncup rejected" record (RW5).
// common.ReplyCodeStaleRevision means the agent holds a revision newer than
// etcd's, which only an etcd restore can cause, and is logged at Error;
// everything else — an unknown object above all, which merely means the
// parent syncup has not landed yet — at Info.
func (w *revWorker) logSyncupRejected(
	ctx context.Context,
	revision uint64,
	reply *replyState,
) {
	attrs := append(w.idAttrs(),
		slog.Uint64("revision", revision),
		slog.Uint64("code", uint64(reply.code)),
		slog.String("details", reply.details),
	)
	if reply.code == common.ReplyCodeStaleRevision {
		slog.ErrorContext(ctx, msgSyncupRejected, attrs...)
		return
	}
	slog.InfoContext(ctx, msgSyncupRejected, attrs...)
}
