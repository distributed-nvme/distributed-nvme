package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// errRoundTimeout is RW4 step 4's "no reply within the round timeout" (RW8).
var errRoundTimeout = errors.New("round timeout")

// errStaleObservation is HL7's cause of a failed round: a reply that came in
// time but answers from an observation older than ObsMaxAgeIntervals of the
// object's intervals, or from none at all.
var errStaleObservation = errors.New("observation too old")

// errRoundAborted ends a round that a desired change overtook (RW6). The
// pending reply would answer the request built from the SUPERSEDED revision,
// so the round is given up together with its stream. It is NOT a health
// failure: the agent answered nothing wrong and a moved node (RW13) was never
// unreachable (HL1).
var errRoundAborted = errors.New("round aborted")

// desiredState is the mutable half of a rev value (architecture.md, Revision
// keys and the sync fan-out; RW2): the revision to reach plus the handle —
// addr_port for DnRev/CnRev, sp_name for SpRev. A put that changes neither is a
// no-op (RW3).
type desiredState struct {
	revision uint64
	handle   string
}

// replyState is what the generic loop needs from one Check*/Syncup* reply.
// The kind's *Info stays inside the driver that decoded it: the loop steers
// only on these fields, and the driver folds the info into health and the
// kind-specific consequences from its own observe (HL4).
type replyState struct {
	// revision is the reply's revision. A round's is the revision of the
	// observation it answers from, the one the agent held when that reading
	// started (dnagent.md SH28), so a Syncup* whose converge stored nothing
	// leaves the rounds at the older one; a Syncup* reply's is that of the
	// request the agent holds (architecture.md, Live-state reporting).
	revision uint64
	// code and details are the reply's AgentReply.
	code    uint32
	details string
	// infoPresent reports whether this reply carried an *Info at all. A
	// show_info = false reply carries one only when something changed, so
	// health is evaluated on the LATEST KNOWN info either way (HL5).
	infoPresent bool
	// fromRound reports whether the reply answered a Check* round rather
	// than a Syncup*: AR10's run of unhealthy health verdicts, which AR5's
	// threshold trigger asks two of, and RW13's run of disagreeing count
	// compares count rounds only, and of those only a new observation
	// (newObs).
	fromRound bool

	// The reply's stamp of the observation it answers from (dnagent.md
	// SH28), every time by the node's clock and zero when unset: obsTime is
	// when the reading ended, zero while the agent holds no observation of
	// the object; answerTime is the agent's clock when it answered;
	// convergeStart is the start of a converge of the object in flight then;
	// obsTraceId is the trace id of the call whose converge or read made the
	// observation.
	obsTime       time.Time
	answerTime    time.Time
	convergeStart time.Time
	obsTraceId    string
	// saveTime is when the agent saved the request a Syncup* reply answers
	// to its local store, zero on a round's reply and on a Syncup* reply
	// that saved nothing.
	saveTime time.Time
	// newObs reports whether a round's observation is later than every one
	// the loop has seen for the object (RW23). The loop sets it, on a
	// round's reply only, so a Syncup* reply's observation never counts.
	newObs bool
}

// newReplyState is the replyState of one reply, built by each driver's fold:
// the reply's AgentReply and revision, whether it carried an *Info, its
// observation stamp, and its save time — nil on a round's reply.
func newReplyState(
	agentReply *pb.AgentReply,
	revision uint64,
	infoPresent bool,
	stamp *pb.ObsStamp,
	saveTime *timestamppb.Timestamp,
) *replyState {
	return &replyState{
		revision:      revision,
		code:          agentReply.GetCode(),
		details:       agentReply.GetDetails(),
		infoPresent:   infoPresent,
		obsTime:       pbTime(stamp.GetObsTime()),
		answerTime:    pbTime(stamp.GetAnswerTime()),
		convergeStart: pbTime(stamp.GetConvergeStart()),
		obsTraceId:    stamp.GetObsTraceId(),
		saveTime:      pbTime(saveTime),
	}
}

// pbTime is a protobuf time as a time.Time, the zero time when it is unset,
// which AsTime alone would turn into the Unix epoch.
func pbTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

// ageFrom is the age HL7 judges: the agent's answer time less the later of
// the observation's time and from, the answer time of the first round after
// a pause in the loop's rounds (revWorker.askFrom), so that a reply with no
// observation counts from from alone. Every time is by the node's clock, so
// no clock difference between this host and the node enters it. It is known
// only when the reply carries an answer time; a negative age reads 0.
func (r *replyState) ageFrom(from time.Time) (age time.Duration, known bool) {
	if r.answerTime.IsZero() {
		return 0, false
	}
	start := r.obsTime
	if start.Before(from) {
		start = from
	}
	return max(r.answerTime.Sub(start), 0), true
}

// checkStream is one object's Check* bidirectional stream (RW4,
// architecture.md, Check streams). recv blocks; the caller bounds it with the
// round timer and closes the stream when the timer wins (RW4 step 4). send puts
// the round's trace id into the request (RW10): the stream's metadata carries
// only the id of the round that opened it.
type checkStream interface {
	send(traceId string, revision uint64, showInfo bool) error
	recv() (*replyState, error)
	closeSend() error
}

// objDriver is the kind-specific half of the per-object loop (RW1). Every
// method runs on the revision worker's own goroutine, so an implementation
// needs no locking; the loop below is the same code for a DN, a CN, an sp
// side and an sp cntlr.
type objDriver interface {
	// logAttrs returns the object's ids as the records that list "ids" carry
	// them (syncup result, syncup rejected, health changed; dnv-worker.md, Log
	// records).
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
	// is missing, say (RW13) — and the round retries next time. An error is
	// a failed syncup, the call's or the failed read of the record the
	// request is built from (RW13), and is logged as its result.
	syncup(
		ctx context.Context,
		conn *grpc.ClientConn,
		cc *pb.ClusterConf,
	) (*replyState, error)
	// observe folds one reply — from a round or from a syncup, which are the
	// same thing here (HL4) but for the runs of rounds in a row, which count
	// a round's new observation only (replyState.fromRound, newObs; RW23) —
	// into health and the kind's own consequences (the flips of RW18/RW19,
	// the pushes of BM1-BM6). A round whose observation is too old never
	// reaches it, and neither does one whose observation ended before the
	// first round after a pause in the loop's rounds, or that has none
	// (HL7).
	observe(ctx context.Context, r *replyState)
	// unreachable folds a stream that cannot be opened, breaks or misses its
	// reply, or a reply whose observation is too old (HL7), into health
	// (HL1/HL2). Only a round reaches it (fail).
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
	// after is the graceful stop of the object's previous worker when its
	// parent has not seen it return yet (childStops): closed once it has,
	// nil when there is none to wait for (RW1).
	after <-chan struct{}
}

// awaitPredecessor holds a worker until the previous worker of its object has
// stopped (RW1). A parent that goes on running does not wait for the stop of
// a child it stopped (childStops), so a successor can be started while its
// predecessor still finishes an in-flight call; it drives nothing — it does
// not even log "revision worker started" — until that stop has returned. A
// worker stopped while it waits still returns only after its predecessor has,
// so a chain of restarts of one object stays in order too. It reports whether
// the worker may run.
func awaitPredecessor(ctx context.Context, after <-chan struct{}) bool {
	if after == nil {
		return true
	}
	select {
	case <-after:
	case <-ctx.Done():
		<-after
	}
	return ctx.Err() == nil
}

// revWorker is the generic per-object loop (RW1-RW12), shared by the dn, cn,
// side and cntlr object kinds. One goroutine owns the object's Check* stream,
// its in-memory state and every RPC about it, which is what makes the "one
// Syncup* at a time per object" of architecture.md, Common agent rules, and the
// "one stream per object" of architecture.md, Check streams, hold by
// construction.
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
	// after is revWorkerParams.after: the loop starts once it is closed.
	after <-chan struct{}

	// State owned by the loop goroutine (RW2).
	desired    desiredState
	stream     *streamState
	conn       *grpc.ClientConn
	connAddr   string
	idleLogged bool
	// confRefused memoizes the stored-conf error last logged, so a steady
	// invalid conf costs one Error record rather than one per round, and a
	// conf that changes from one invalid value to another still reports.
	confRefused string
	// obsSeen is the latest observation time a round's or a Syncup* reply
	// carried for the object (RW23), and obsAddr the endpoint it was seen
	// at: a move to another endpoint forgets it (connect). It lives here, on
	// the loop goroutine, not in fold, which runs on the stream's pump
	// goroutine for a round's reply.
	obsSeen time.Time
	obsAddr string
	// syncupObs is the observation time the latest Syncup* reply carried,
	// kept until the next round's reply is judged or a round fails
	// (staleRound, fail). That round answers from it as a rule, a whole
	// interval after the reply (RW4 step 6) with no read of the object in
	// between, since the Syncup* cancelled the one the round before it
	// started (dnagent.md SH30): an age past one interval of the loop's own
	// making, which the late record leaves out (HL7).
	syncupObs time.Time
	// askFrom is the agent's answer time on the first round the loop judged
	// after a pause in its rounds of the object: its start, an idle or a
	// refused conf (quiesce), or a move to another endpoint (connect). An
	// observation's age is counted from no earlier than askFrom (HL7), since
	// no round asked the agent to read during the pause. It is by the node's
	// clock; zero until that first round.
	askFrom time.Time
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
// kind-specific half can call back into it, and starts its goroutine. The
// loop's own trace id (RW10) is on the records it writes outside a round or a
// syncup, which mint their own: its start, stop and idle records among them.
func startRevWorker(
	p revWorkerParams,
	newDriver func(w *revWorker) objDriver,
) *revWorker {
	ctx, cancel := context.WithCancel(newTraceCtx(context.Background(), p.seed))
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
		after:     p.after,
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
// take up to the syncup timeout of deps (common.DefaultWorkerSyncupTimeout in
// production), which is why a parent that keeps running issues it off its own
// goroutine (childStops).
func (w *revWorker) stop() {
	w.cancel()
	<-w.done
}

// run is the per-object loop (RW4). Of its six steps, round() is 1-5, and
// step 6 — re-arming the timer AFTER the round, so a slow round never queues a
// burst of catch-up rounds (RW8) — is wait().
func (w *revWorker) run() {
	defer close(w.done)
	if !awaitPredecessor(w.ctx, w.after) {
		return
	}
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
			// architecture.md, Common validation: the gateway stores concrete
			// values, so a zero or an out-of-range member here is corruption or
			// foreign data, and this object's pass is refused rather than
			// computed with a guessed geometry. The refusal reaches nothing:
			// round() is skipped, so no stream is opened, no Syncup* is sent
			// and no err_epoch is written, and wait() is passed a nil cc so a
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

// round runs one Check round (RW4 steps 1-5).
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
	// Step 2, under this round's trace id (RW10).
	traceId, _ := common.TraceIdFromCtx(ctx)
	if err := w.stream.stream.send(
		traceId, w.desired.revision, false,
	); err != nil {
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
	if w.staleRound(ctx, reply, interval) {
		return
	}
	// Step 5: process the reply's *Info if present (HL4/HL5), then re-sync
	// when the agent rejected the request, reported leftovers, or holds
	// another revision — which is also how the first sync after a shard
	// handoff, a worker restart or an agent restart happens, with no recovery
	// step of its own.
	//
	// The leftover case is what re-drives a sweep: the observation's code is
	// written by every converge and every background read of the object
	// (dnagent.md SH28, SH29), so as long as the node still holds something
	// the desired state does not want, every round issues a Syncup* that
	// sweeps again. No backoff (RW12), and no flag anywhere — the code itself
	// is the state.
	w.seeRound(reply)
	// An observation that ended before the first round after a pause in the
	// loop's rounds (askFrom), or none at all, is one no round asked for: it
	// carries no health verdict and drives nothing but the re-sync below,
	// until staleRound finds it too old (HL7). The read that round started
	// brings one the next round judges.
	if !reply.obsTime.Before(w.askFrom) ||
		reply.code == common.ReplyCodeUnknownObject {
		w.driver.observe(ctx, reply)
	}
	needSyncup := reply.code != 0 || reply.revision != w.desired.revision
	if needSyncup {
		w.syncup(ctx, cc, reply.obsTraceId)
	}
}

// staleRound judges the age of the observation a round's reply answers from
// (HL7) and reports whether the round failed on it. The age counts from no
// earlier than the first round after a pause in the loop's rounds (askFrom):
// its start, an idle or a move. No round asked the agent to read during the
// pause, so an observation that aged then tells nothing of the agent. A gap
// between two rounds of a running loop is no pause: a round that waited out
// a long Syncup*, behind a converge that holds the object's reads off, must
// find the observation as old as it is. The reply came in time, but the
// observation may not have: one older than an interval is logged as a late
// observation, but for the round right after a Syncup* answering from that
// reply's observation (syncupObs); one older than ObsMaxAgeIntervals of them,
// or none at all once the pause lies that far behind, is a missed round in
// every way (fail). The stream
// drops, because the agent leaves an unchanged *Info out on a kept stream and
// the unknown marking would then read clean, and no Syncup* is issued on the
// reply's revision or code: the next round's fresh stream brings them again.
//
// A reply that refuses the object as unknown is not judged: the agent holds
// no state of the object and so no observation of it, and its answer is its
// state as it answers. Judged, it would fail every round, and the re-sync of
// RW4 step 5 that makes the object known — an agent's first SyncupDn, a
// side's first SyncupSide — would never be issued.
func (w *revWorker) staleRound(
	ctx context.Context,
	reply *replyState,
	interval time.Duration,
) bool {
	afterSyncup := !w.syncupObs.IsZero() && reply.obsTime.Equal(w.syncupObs)
	w.syncupObs = time.Time{}
	if reply.code == common.ReplyCodeUnknownObject {
		return false
	}
	bound := common.ObsMaxAgeIntervals * interval
	if w.askFrom.IsZero() {
		w.askFrom = reply.answerTime
	}
	age, known := reply.ageFrom(w.askFrom)
	stale := !known || age > bound
	// The round right after a Syncup* that answers from that reply's
	// observation is late by the loop's own wait (syncupObs), so it is
	// logged late only when it is stale; a later round still answering from
	// it is late in earnest, a read that has not ended within an interval.
	if (!known || age > interval) && (stale || !afterSyncup) {
		w.logLateObservation(ctx, reply, interval, stale, age, known)
	}
	if !stale {
		return false
	}
	attrs := append(w.idAttrs(),
		slog.String("error", errStaleObservation.Error()),
	)
	if known {
		attrs = append(attrs, slog.Int64("age_ms", age.Milliseconds()))
	}
	slog.InfoContext(ctx, "check round failed", attrs...)
	w.fail(ctx)
	return true
}

// seeRound marks a round's reply r, one not judged stale (HL7), before
// observe sees it: a round's, and new when its observation is later than
// every one the loop has seen for the object (RW23), which it then sees too.
func (w *revWorker) seeRound(r *replyState) {
	r.fromRound = true
	r.newObs = r.obsTime.After(w.obsSeen)
	w.seeObservation(r)
}

// seeObservation raises the latest observation time the loop has seen for
// the object to r's when r's is later (RW23).
func (w *revWorker) seeObservation(r *replyState) {
	if r.obsTime.After(w.obsSeen) {
		w.obsSeen = r.obsTime
	}
}

// fail is RW4 step 4: close the stream and report the object unreachable
// (HL1/HL2). A worker that is stopping reports nothing — a cancelled stop ctx
// is not a sick agent, and writing "unreachable" on the way out would restart
// the threshold clock of the automatic reactions (AR4) of a perfectly healthy
// object. A failed round also ends the late record's leave for the round
// after a Syncup* (syncupObs).
func (w *revWorker) fail(ctx context.Context) {
	w.syncupObs = time.Time{}
	w.dropStream()
	if w.ctx.Err() != nil {
		return
	}
	w.driver.unreachable(ctx)
}

// wait is RW4 step 6: it arms the round timer AFTER the round (RW8) and stays
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
	w.syncup(newTraceCtx(w.ctx, w.seed), cc, "")
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
// (RW5). obsTraceId is the trace id of the observation a round's reply
// answered from when the round issues it (RW4 step 5), which its syncup
// result record names (RW10); empty for an immediate syncup (RW6).
func (w *revWorker) syncup(
	ctx context.Context,
	cc *pb.ClusterConf,
	obsTraceId string,
) {
	revision := w.desired.revision
	conn, err := w.connect()
	if err != nil {
		w.logSyncupResult(ctx, revision, obsTraceId, nil, err)
		return
	}
	// RW11: the call's own deadline bounds it and the stop ctx is
	// deliberately NOT the RPC ctx, so a graceful stop lets an in-flight
	// Syncup* finish instead of cancelling it.
	rpcCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), w.deps.tmo.syncup,
	)
	defer cancel()
	reply, err := w.driver.syncup(rpcCtx, conn, cc)
	if err != nil {
		// A gRPC error, or a failed read of what the request is built from
		// (RW13), is logged and left to the next round (RW5, RW12).
		w.logSyncupResult(ctx, revision, obsTraceId, nil, err)
		return
	}
	if reply == nil {
		// The driver sent nothing and said why (RW13's missing DnConf).
		return
	}
	// RW23: the reply's observation is seen, so a round that answers from it
	// afterwards is no new one, but it never counts itself: newObs stays
	// false. Nor is it judged by its age (HL7), and the next round, which
	// answers from it as a rule, is not logged late on it (syncupObs).
	w.seeObservation(reply)
	w.syncupObs = reply.obsTime
	w.logSyncupResult(ctx, revision, obsTraceId, reply, nil)
	if !accepted(reply.code) {
		w.logSyncupRejected(ctx, revision, reply)
		// HL1/HL2: a rejection neither sets nor clears health; observe is
		// still called so the driver can record what it learned.
		w.driver.observe(ctx, reply)
		return
	}
	if reply.code == common.ReplyCodeLeftover {
		// An accepted request with residue: the desired state is stored and
		// the wanted objects converged, but the node still holds objects it
		// does not want, or an enumeration did not answer. RW4 step 5 re-
		// issues the Syncup* every round while the code persists, so the
		// record is what makes a leftover each re-sync still finds visible
		// in the worker log (logSyncupLeftover).
		w.logSyncupLeftover(ctx, revision, reply)
	}
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
// and RW5 a further syncup timeout of deps on top.
func (w *revWorker) recv(
	cc *pb.ClusterConf,
	interval time.Duration,
) (*replyState, error) {
	timer := w.deps.clk.newTimer(interval)
	defer timer.stop()
	// The stream is captured: a desired change drops it, and the round must
	// not then read w.stream, which is nil by that point.
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
			// it. The round ends: its reply answers the request built from
			// the superseded revision, and RW4 step 4's rule that a stream
			// is never reused across an abandoned round holds here too, so
			// the stream goes with it, before the Syncup* is sent. The round
			// itself holds no lock the Syncup* needs: the agent answers it
			// from memory, and the Syncup* cancels the object's background
			// read itself before it waits for its locks (dnagent.md SH30).
			// applyDesired's own drop when RW13 moved the object finds no
			// stream left. The next round opens a fresh one, which the agent
			// answers with the complete *Info again (architecture.md, Check
			// streams).
			w.dropStream()
			w.applyDesired(next, cc)
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
// (RW13; architecture.md, dn / cn roles): the stream and the connection
// reference are dropped and the loop continues at the new address. No delete
// ever reaches the agent. The latest observation time seen is forgotten with
// the old endpoint (RW23): the node there is another node with another
// clock.
func (w *revWorker) connect() (*grpc.ClientConn, error) {
	addr := w.driver.addrPort()
	if addr == "" {
		return nil, fmt.Errorf("worker %s: empty addr_port", w.role)
	}
	if addr != w.obsAddr {
		w.obsSeen = time.Time{}
		w.askFrom = time.Time{}
		w.obsAddr = addr
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
// (architecture.md, Common validation): the same quiesced state — stream
// dropped, RW7 connection reference released — but its own record, so an
// operator's grep for "cluster conf missing" keeps meaning "the cluster is not
// in the cache" and nothing else (RW9).
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
	// No round asks the agent to read while the loop idles, so the first
	// round after it counts an observation's age from its own answer (HL7).
	w.askFrom = time.Time{}
}

// cleanup is the graceful-stop tail of RW11.
func (w *revWorker) cleanup() {
	w.dropStream()
	w.releaseConn()
}

// lifecycleAttrs are the attributes of the "revision worker started" /
// "revision worker stopped" records (dnv-worker.md, Log records).
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

// idAttrs are the "ids" of the syncup records (dnv-worker.md, Log records).
func (w *revWorker) idAttrs() []any {
	attrs := make([]any, 0, 6)
	for _, attr := range w.driver.logAttrs() {
		attrs = append(attrs, attr)
	}
	return attrs
}

// logSyncupResult emits the "syncup result" record (dnv-worker.md, Log
// records), which every Syncup* reply (reply) or failure (err, reply nil)
// produces (RW5). The record of a round's re-send names the trace id of the
// observation the round answered from (obsTraceId, RW10), and one whose reply
// carries both a save time and an observation time gives the time from the
// save to that observation, the converge's length.
func (w *revWorker) logSyncupResult(
	ctx context.Context,
	revision uint64,
	obsTraceId string,
	reply *replyState,
	err error,
) {
	var code uint32
	if reply != nil {
		code = reply.code
	}
	attrs := append(w.idAttrs(),
		slog.Uint64("revision", revision),
		slog.Uint64("code", uint64(code)),
	)
	if obsTraceId != "" {
		attrs = append(attrs, slog.String("obs_trace_id", obsTraceId))
	}
	if reply != nil && !reply.saveTime.IsZero() && !reply.obsTime.IsZero() {
		attrs = append(attrs, slog.Int64("converge_ms",
			reply.obsTime.Sub(reply.saveTime).Milliseconds()))
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	slog.InfoContext(ctx, msgSyncupResult, attrs...)
}

// logLateObservation emits the "late observation" record (HL7; dnv-worker.md,
// Log records) of a round staleRound logs late: one whose age HL7 judges
// (ageFrom) is more than one interval, or unknown for want of an answer time.
// A reply with no observation counts its age from the first round after a
// pause (askFrom), so it is logged only once that round lies more than an
// interval behind. The record names whether the round is stale, no_obs when
// the reply has no observation, the age judged when the reply carries an
// answer time, the interval, the observation's trace id and, while a
// converge of the object runs, how long it has run.
func (w *revWorker) logLateObservation(
	ctx context.Context,
	reply *replyState,
	interval time.Duration,
	stale bool,
	age time.Duration,
	known bool,
) {
	attrs := append(w.idAttrs(), slog.Bool("stale", stale))
	if reply.obsTime.IsZero() {
		attrs = append(attrs, slog.Bool("no_obs", true))
	}
	if known {
		attrs = append(attrs, slog.Int64("age_ms", age.Milliseconds()))
	}
	attrs = append(attrs,
		slog.Int64("interval_ms", interval.Milliseconds()),
		slog.String("obs_trace_id", reply.obsTraceId),
	)
	if !reply.convergeStart.IsZero() && !reply.answerTime.IsZero() {
		attrs = append(attrs, slog.Int64("converge_ms",
			reply.answerTime.Sub(reply.convergeStart).Milliseconds()))
	}
	slog.InfoContext(ctx, msgLateObservation, attrs...)
}

// logSyncupRejected emits the "syncup rejected" record (RW5; dnv-worker.md, Log
// records). common.ReplyCodeStaleRevision means the agent holds a revision
// newer than etcd's, which only an etcd restore can cause, and is logged at
// Error; everything else — an unknown object above all, which merely means the
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

// logSyncupLeftover emits the "syncup leftover" record (log.md, Leftovers): the
// request was accepted and stored, and the agent's details name what the node
// still holds that the desired state does not want, or what kept the agent from
// proving the node clean (an enumeration that did not answer, say). It is Info,
// not Error: a leftover is normal for as long as a dead remote's failfast
// window lasts, and on the pass of a cn teardown whose sweep disconnects
// anything — the cn agent's sweep sets each `nvme disconnect` it issues, of a
// leg or a clone source, going off its pass and names the connection a leftover
// until a later pass finds it gone (a removing pass once it has no controller,
// a background read once its sysfs subsystem directory, which can outlive the
// last controller, is gone), so that pass's SyncupCntlr/SyncupCn reply carries
// the code, and the first round answering from an observation that no longer
// finds the connection, with nothing else left, answers code 0 and ends the
// re-sync.
// A background read only computes the observation's code and removes nothing
// (dnagent.md SH29), and the agent registers nothing for a leftover: its
// background converges (the cn's connect retry, the dn's connect retry and
// fence timer) sweep only while one is registered or
// armed for a reason of its own. So the sweep a leftover brings back is the
// one in the re-sync RW4 step 5 issues on that code, and a leftover that
// re-sync still finds is logged every round. One only a read's verdict names
// (a subsystem directory whose last controller is gone, above) is not: the
// re-sync's removing pass counts it gone, so its reply does not carry it, and
// what names it is the agent's own `sweep leftover` record of each read's
// verdict.
func (w *revWorker) logSyncupLeftover(
	ctx context.Context,
	revision uint64,
	reply *replyState,
) {
	slog.InfoContext(ctx, msgSyncupLeftover, append(w.idAttrs(),
		slog.Uint64("revision", revision),
		slog.String("details", reply.details),
	)...)
}
