package cnagent

import (
	"context"
	"log/slog"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Observations (dnagent.md SH28 to SH30)
//
// A CheckCn or CheckCntlr round and a GetCnInfo or GetCntlrInfo answer from
// the object's latest observation alone (answerCn, answerCntlr). The end of
// every converge stores one — a syncup's or the startup reconcile's — and so
// does the background read a round starts (refreshCn, refreshCntlr), which holds the
// CN1 locks a round once held and reads what a round once read. A syncup
// marks its converge as it arrives and cancels the read before it waits for a
// lock, unless a gate refuses it against the object's held request
// (markSyncup, markNodeSyncup); a SyncupCn newer than the CN's held request,
// or for a CN the agent does not hold, marks the node too and cancels every
// read, one at the held revision or one that only adds cntlrs (syncupCnAdds)
// only the CN's own. While the store's latest
// save failed, a syncup the gates admit marks its own object alone and
// cancels no read. No converge of the cn role runs in the background. No
// read starts while a mark of its object or of the node stands.
// ---------------------------------------------------------------------------

type (
	cnObs    = agent.Observation[*pb.CnInfo]
	cntlrObs = agent.Observation[*pb.CntlrInfo]
)

// answerCn is what a CheckCn round or a GetCnInfo of key answers from: the
// CN's latest observation and the start of a converge of it in flight (SH28),
// read in one hold of mu, in which a round (startRead) also begins the CN's
// read when SH29 allows one. known is false for a CN this process holds no
// request for. It takes no other lock and reads nothing of the node.
func (s *CnAgentServer) answerCn(
	roundCtx context.Context,
	key string,
	startRead bool,
) (obs cnObs, since time.Time, known bool) {
	s.mu.Lock()
	st := s.cns[key]
	if st == nil {
		s.mu.Unlock()
		return cnObs{}, time.Time{}, false
	}
	obs = st.obs.LatestLocked()
	since = s.marks.SinceLocked(key)
	var readCtx context.Context
	if startRead && s.mayReadLocked(key, st.obs.ReadingLocked()) {
		readCtx = st.obs.BeginReadLocked(s.readParent(roundCtx))
		s.reads.Add(1)
	}
	s.mu.Unlock()
	if readCtx != nil {
		go func() {
			defer s.reads.Done()
			s.refreshCn(readCtx, key, st)
		}()
	}
	return obs, since, true
}

// answerCntlr is answerCn for a cntlr: what a CheckCntlr round or a
// GetCntlrInfo of key answers from. It takes no lock of the CN1 mapping, so a
// round or an info read of a cntlr this process does not hold leaves no lock
// entry behind (dnagent.md, SH11), and none waits behind a converge.
func (s *CnAgentServer) answerCntlr(
	roundCtx context.Context,
	key string,
	startRead bool,
) (obs cntlrObs, since time.Time, known bool) {
	s.mu.Lock()
	st := s.cntlrs[key]
	if st == nil {
		s.mu.Unlock()
		return cntlrObs{}, time.Time{}, false
	}
	obs = st.obs.LatestLocked()
	since = s.marks.SinceLocked(key)
	var readCtx context.Context
	if startRead && s.mayReadLocked(key, st.obs.ReadingLocked()) {
		readCtx = st.obs.BeginReadLocked(s.readParent(roundCtx))
		s.reads.Add(1)
	}
	s.mu.Unlock()
	if readCtx != nil {
		go func() {
			defer s.reads.Done()
			s.refreshCntlr(readCtx, key, st)
		}()
	}
	return obs, since, true
}

// mayReadLocked reports whether a round may start a read of key (SH29): no
// read of it runs (reading), no converge of it and no node-level syncup is
// in flight (SH30), and the startup pass has ended (SH33), since a read
// would wait for the node lock the pass holds. Called with mu held.
func (s *CnAgentServer) mayReadLocked(key string, reading bool) bool {
	return s.roundReads && !reading && s.marks.QuietLocked(key) &&
		!s.startup.Starting()
}

// readParent is the ctx a round's read derives from: rootCtx, since the read
// lives with the agent and not with the stream, under the trace id of the
// round that starts it (SH29).
func (s *CnAgentServer) readParent(roundCtx context.Context) context.Context {
	traceId, ok := common.TraceIdFromCtx(roundCtx)
	if !ok {
		traceId = common.NewTraceId()
	}
	return common.WithTraceId(s.rootCtx, traceId)
}

// refreshCn is the CN's read (SH29). Under the node read lock it probes the
// base state and then takes the node-level read-only verdict: the same
// enumeration and the same comparison the sweep makes, with nothing of the
// node touched (CN23) — the one file it writes itself is the store's probe
// file, while the store refuses writes, removed once the write holds
// (agent.Store.Probe, CN28) — plus what of the base state the probe read wrong
// in a way a SyncupCn would cure (probeCn, dnagent.md SH31). It stores both as
// the CN's observation with the revision held when it took the lock, even when
// a listing did not answer, as the verdict then says. A read of a CN the agent
// no longer holds, or one cancelled before it took the lock, reads nothing; a
// cancelled one stores nothing, and the rows it makes once cancelled move no
// epoch (agent.ResTracker; probeCn writes through a view of its own, since the
// CN's next read may share the node read lock before this one has returned). A
// round starts it in the background (answerCn); unit tests call it themselves
// with a ctx of their own, which ends no read.
func (s *CnAgentServer) refreshCn(
	ctx context.Context,
	key string,
	st *cnState,
) {
	defer s.endRead(func() { st.obs.EndReadLocked(ctx) })
	if ctx.Err() != nil {
		return
	}
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	if s.getCn(key) != st || ctx.Err() != nil {
		return
	}
	rev := st.loadReq().GetRevision()
	info, redrive := s.probeCn(ctx, st)
	verdict := s.sweepCn(ctx, st, false, redrive)
	s.storeCnObs(ctx, st, rev, info, verdict.Reply())
}

// refreshCntlr is the cntlr's read (SH29): under the node read lock and the
// cntlr's object lock, one reading of the node, then the rows and the
// read-only verdict from it (readCntlr; CN23, CN28, CN30), stored as the
// cntlr's observation with the revision held when it took its locks. It
// takes the object lock only once it has found under the node read lock that
// the agent still holds st, so a read of a cntlr dropped meanwhile leaves no
// lock entry (dnagent.md, SH11): the drop runs under the node write lock, so
// the cntlr the check finds stays for the read. A cancelled read stores
// nothing, and the rows it makes once cancelled move no epoch: it binds its
// ctx to the cntlr's tracker under the object lock (agent.ResTracker.Bind). A
// round starts it in the background (answerCntlr), and unit tests call it
// themselves.
func (s *CnAgentServer) refreshCntlr(
	ctx context.Context,
	key string,
	st *cntlrState,
) {
	defer s.endRead(func() { st.obs.EndReadLocked(ctx) })
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	last := st.obs.LatestLocked().Time
	s.mu.Unlock()
	obsAge := s.now().Sub(last)
	start := time.Now()
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	if s.getCntlr(key) != st || ctx.Err() != nil {
		return
	}
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	if s.getCntlr(key) != st || ctx.Err() != nil {
		return
	}
	defer st.tracker.Bind(ctx)()
	rev := st.loadReq().GetRevision()
	locked := time.Now()
	readCtx, slotWait := common.WithSlotWait(ctx)
	info, verdict := s.readCntlr(readCtx, st)
	logReadTimes(ctx, st.loadReq(), obsAge, locked.Sub(start), slotWait(),
		time.Since(locked))
	s.storeCntlrObs(ctx, st, rev, info, verdict.Reply())
}

// msgCntlrReadTimes is the record of a cntlr's read's times (CN28).
const msgCntlrReadTimes = "cntlr read times"

// logReadTimes logs, at Debug, what a cntlr's report's age is made of while
// a node builds many cntlrs at once (CN28): the age of the cntlr's latest
// observation as its read starts, the time in which no read of it ran or
// stored, which a converge mark or a cancel makes (dnagent.md SH30); how
// long the read waited for its locks; how long its commands waited for a
// slot of the agent's OsClient; and how long it ran once it held its locks,
// slot waits included. A read cancelled before it held its locks logs
// nothing.
func logReadTimes(
	ctx context.Context,
	req *pb.SyncupCntlrRequest,
	obsAge, lockWait, slotWait, run time.Duration,
) {
	slog.DebugContext(ctx, msgCntlrReadTimes,
		slog.Uint64("cn_id", req.GetCnId()),
		slog.Uint64("sp_id", req.GetCntlrPointer().GetSpId()),
		slog.Uint64("cntlr_id", req.GetCntlrPointer().GetCntlrId()),
		slog.Int64("obs_age_ms", obsAge.Milliseconds()),
		slog.Int64("lock_wait_ms", lockWait.Milliseconds()),
		slog.Int64("slot_wait_ms", slotWait.Milliseconds()),
		slog.Int64("run_ms", run.Milliseconds()))
}

// endRead runs end, an EndReadLocked, under mu: on every path out of a read,
// so a read that stored nothing ends too. One that stored has ended already,
// in the same hold (storeCnObs, storeCntlrObs), and then end does nothing.
func (s *CnAgentServer) endRead(end func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	end()
}

// storeCnObs stores the CN's observation that a converge or a read under ctx
// made: its rows and code, rev the revision of the request it converged or
// held, its end now by the agent's clock and ctx's trace id (SH28). A ctx
// that is done stores nothing (agent.ObsSlot.WriteLocked). When ctx is the
// CN's running read, the read ends in the same hold of mu, so no round finds
// it ended before its observation is there. It returns the latest
// observation, the one a syncup reply's stamp gives.
func (s *CnAgentServer) storeCnObs(
	ctx context.Context,
	st *cnState,
	rev uint64,
	info *pb.CnInfo,
	reply *pb.AgentReply,
) cnObs {
	traceId, _ := common.TraceIdFromCtx(ctx)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st.obs.WriteLocked(ctx, cnObs{
		Info: info, Reply: reply, Revision: rev, Time: now, TraceId: traceId,
	})
	st.obs.EndReadLocked(ctx)
	return st.obs.LatestLocked()
}

// storeCntlrObs is storeCnObs for a cntlr.
func (s *CnAgentServer) storeCntlrObs(
	ctx context.Context,
	st *cntlrState,
	rev uint64,
	info *pb.CntlrInfo,
	reply *pb.AgentReply,
) cntlrObs {
	traceId, _ := common.TraceIdFromCtx(ctx)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st.obs.WriteLocked(ctx, cntlrObs{
		Info: info, Reply: reply, Revision: rev, Time: now, TraceId: traceId,
	})
	st.obs.EndReadLocked(ctx)
	return st.obs.LatestLocked()
}

// markSyncup marks a SyncupCntlr of key in flight as it arrives, then cancels
// the cntlr's running read, before the syncup waits for any lock (SH30): a
// round in between starts no read the object lock might be handed to first.
// The func it returns ends the mark; the syncup calls it once its locks have
// gone, after its converge has stored its observation or the call has ended
// without one.
//
// A request CN8's gates refuse against the request the agent holds for the
// cntlr — its pointer, its revision (dnagent.md SH8) or its conf
// (cntlrGates) — marks nothing and cancels nothing, and the func it returns
// ends nothing: the worker re-sends such a request every round — after an
// etcd restore, say, or over a bad stored conf — and were it to cancel each
// round's read, no read would ever end and the observation would go stale
// (dnv-worker.md HL7). The gates run here under mu, silently — they take no
// lock and read nothing of the node, so mu stays a leaf — and again under
// the locks, and that run decides: the held revision only grows while the
// agent holds the cntlr, so a stale request stays refused, the conf gate
// reads the request alone, and the CN request the pointer gate reads changes
// only under a SyncupCn, so the two runs differ only when another syncup
// lands in between, a SyncupCn or a newer SyncupCntlr of this cntlr, which
// costs that one syncup a wait for the read or one cancelled read. A cntlr
// the agent does not hold, or a request the gates admit, is marked as
// before.
//
// While the store's latest save failed (agent.Store.SaveFailing), a request
// the gates admit marks the cntlr and cancels nothing, for the same reason:
// a request refused for a failed save comes back every round too, at a
// newer revision, and at the held revision while the cntlr's rounds answer
// a non-zero code (dnagent.md SH5), and nothing knows before its save that
// it will be refused. Its object lock waits for the cntlr's read, which runs
// bounded commands (dnagent.md SH13).
func (s *CnAgentServer) markSyncup(
	key string,
	req *pb.SyncupCntlrRequest,
) func() {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.cntlrs[key]
	if st != nil {
		cn := s.cns[cnKey(req.GetClusterId(), req.GetCnId())]
		if reply, _ := cntlrGates(
			cn, req, st.loadReq().GetRevision()); reply != nil {
			return func() {}
		}
	}
	s.marks.BeginLocked(key, now)
	if st != nil && !s.store.SaveFailing() {
		st.obs.CancelReadLocked()
	}
	return func() { s.endMark(key, false) }
}

// markNodeSyncup marks a SyncupCn's converge of the CN key as it arrives,
// before the syncup waits for its node lock (SH30), and returns whether the
// request only adds cntlrs to the CN's held request (addsCntlrsOnly) and
// what ends the marks it made once the lock has gone, after the converge
// has stored its observation or the call has ended without one.
//
// A request of a revision newer than the one the agent holds for the CN, or
// for a CN it does not hold, marks the CN and the node and cancels every
// read of the agent: each read holds the node read lock the write lock waits
// for, and the node's mark keeps a round from starting a new one until the
// syncup ends.
//
// A request at the held revision is a re-send. The worker issues one on
// every round while the CN's code is not zero, as it is while a CN-level
// leftover lasts (dnv-worker.md RW4, RW12). It marks the CN and cancels the
// CN's own read alone. Were each re-send to cancel the cntlr reads and hold
// them off, a cntlr's read would end only between two re-sends, none would
// while a read takes longer than that, and the observations of cntlrs with
// no fault would go too old: their rounds would be missed rounds, which set
// their health epochs and lengthen their failover runs (dnv-worker.md HL7,
// AR10). Its write lock waits instead for the cntlr reads that hold the node
// read lock when it asks, each of which runs bounded commands (dnagent.md
// SH13); a read a round starts after that waits for the node read lock until
// the syncup ends, so it takes no lock ahead of the syncup.
//
// A newer request that only adds cntlrs (addsCntlrsOnly) marks the CN and
// cancels the CN's own read alone too, and runs under the node read lock
// (syncupCnAdds, cnagent.md CN1); the cancel keeps the CN's read, which
// shares that lock, from storing over the syncup's observation. A many-pools build brings one for each
// new cntlr, back to back; were each to mark the node, cancel every read
// and wait for the write lock behind the cntlr converges in flight, no read
// of a cntlr built already would start or end for as long as the build
// lasts, and those cntlrs' rounds would be missed rounds that fail their
// pools over (dnv-worker.md HL7, AR5).
//
// A request the revision gate (SH8) refuses against the CN's held request
// marks nothing and cancels nothing, for markSyncup's reason: the held
// revision only grows, so the gate refuses it again under the write lock.
// SyncupCn has no other gate.
//
// While the store's latest save failed (agent.Store.SaveFailing), a request
// the gate admits marks the CN alone and cancels nothing, whatever its
// revision, for markSyncup's reason: the worker re-sends a request refused
// for a failed save on every round at a newer revision, and at the held
// revision while the CN's rounds answer a non-zero code (dnagent.md SH5),
// and nothing knows before its save that it will be refused. Such a request
// runs under the write lock even when it only adds cntlrs, since it cancels
// no read of the CN that could then store over its observation. Were each
// newer re-send, or each one for a CN the agent does not hold, to mark the
// node and cancel every read of the agent, the observations of the agent's
// CNs and cntlrs would go too old (dnv-worker.md HL7), and were each
// re-send at the held revision to cancel the CN's own read, the CN's would.
// Its write lock waits for the reads that hold the node read lock when it
// asks, as a re-send's at the held revision does.
func (s *CnAgentServer) markNodeSyncup(
	req *pb.SyncupCnRequest,
) (adds bool, end func()) {
	key := cnKey(req.GetClusterId(), req.GetCnId())
	revision := req.GetRevision()
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.cns[key]
	var held uint64
	if st != nil {
		heldReq := st.loadReq()
		held = heldReq.GetRevision()
		if agent.GateRevision(held, revision) != nil {
			return false, func() {}
		}
		adds = addsCntlrsOnly(heldReq, req)
	}
	s.marks.BeginLocked(key, now)
	if s.store.SaveFailing() {
		return false, func() { s.endMark(key, false) }
	}
	if st != nil && (revision == held || adds) {
		st.obs.CancelReadLocked()
		return adds, func() { s.endMark(key, false) }
	}
	s.marks.BeginNodeLocked()
	for _, st := range s.cns {
		st.obs.CancelReadLocked()
	}
	for _, st := range s.cntlrs {
		st.obs.CancelReadLocked()
	}
	return false, func() { s.endMark(key, true) }
}

// addsCntlrsOnly reports whether req, a SyncupCn of the CN whose held
// request is held, only adds cntlrs to it (cnagent.md CN1): it is newer,
// keeps every cntlr pointer of held, adds at least one and carries the same
// qos_ratio. Such a request removes nothing from the node and changes
// nothing of its base state, so it needs no node write lock. One that adds
// nothing is no such request: it runs as every other newer request does.
func addsCntlrsOnly(held, req *pb.SyncupCnRequest) bool {
	if req.GetRevision() <= held.GetRevision() ||
		!proto.Equal(held.GetQosRatio(), req.GetQosRatio()) {
		return false
	}
	for _, ptr := range held.GetCntlrPointerList() {
		if !pointerKnown(req, ptr) {
			return false
		}
	}
	for _, ptr := range req.GetCntlrPointerList() {
		if !pointerKnown(held, ptr) {
			return true
		}
	}
	return false
}

func (s *CnAgentServer) endMark(key string, node bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks.EndLocked(key)
	if node {
		s.marks.EndNodeLocked()
	}
}
