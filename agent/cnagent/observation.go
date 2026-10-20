package cnagent

import (
	"context"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Observations (dnagent.md SH28 to SH30)
//
// A CheckCn or CheckCntlr round and a GetCnInfo or GetCntlrInfo answer from
// the object's latest observation alone (answerCn, answerCntlr). The end of
// every converge stores one — a syncup's, the startup reconcile's, a
// connect-retry attempt's — and so does the
// background read a round starts (refreshCn, refreshCntlr), which holds the
// CN1 locks a round once held and reads what a round once read. A syncup
// marks its converge as it arrives and cancels the read before it waits for a
// lock, unless a gate refuses it against the object's held request
// (markSyncup, markNodeSyncup); a SyncupCn newer than the CN's held request,
// or for a CN the agent does not hold, marks the node too and cancels every
// read, one at the held revision only the CN's own. A background converge
// marks its object before it waits for the object lock (markConverge). No
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
// read of it runs (reading), and no converge of it and no node-level syncup
// is in flight (SH30). Called with mu held.
func (s *CnAgentServer) mayReadLocked(key string, reading bool) bool {
	return s.roundReads && !reading && s.marks.QuietLocked(key)
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
// enumeration and the same comparison the sweep makes, with nothing touched
// (CN23), plus what of the base state the probe read wrong in a way a
// SyncupCn would cure (probeCn, CN30). It stores both as the CN's observation
// with the revision held when it took the lock, even when a listing did not
// answer, as the verdict then says. A read of a CN the agent no longer holds,
// or one cancelled before it took the lock, reads nothing; a cancelled one
// stores nothing, and the rows it makes once cancelled move no epoch
// (agent.ResTracker; probeCn writes through a view of its own, since the CN's
// next read may share the node read lock before this one has returned). A
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
	info, verdict := s.readCntlr(ctx, st)
	s.storeCntlrObs(ctx, st, rev, info, verdict.Reply())
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
// The func it returns ends the mark; the syncup calls it once its converge
// has stored its observation and its locks have gone.
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
	if st != nil {
		st.obs.CancelReadLocked()
	}
	return func() { s.endMark(key, false) }
}

// markNodeSyncup marks a SyncupCn's converge of the CN key as it arrives,
// before the syncup waits for the node write lock (SH30), and returns what
// ends the marks it made once the converge has stored its observation and the
// lock has gone.
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
// A request the revision gate (SH8) refuses against the CN's held request
// marks nothing and cancels nothing, for markSyncup's reason: the held
// revision only grows, so the gate refuses it again under the write lock.
// SyncupCn has no other gate.
func (s *CnAgentServer) markNodeSyncup(key string, revision uint64) func() {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.cns[key]
	if st != nil {
		held := st.loadReq().GetRevision()
		if agent.GateRevision(held, revision) != nil {
			return func() {}
		}
		if revision == held {
			s.marks.BeginLocked(key, now)
			st.obs.CancelReadLocked()
			return func() { s.endMark(key, false) }
		}
	}
	s.marks.BeginNodeLocked()
	s.marks.BeginLocked(key, now)
	for _, st := range s.cns {
		st.obs.CancelReadLocked()
	}
	for _, st := range s.cntlrs {
		st.obs.CancelReadLocked()
	}
	return func() { s.endMark(key, true) }
}

// markConverge marks a background converge of the cntlr key in flight (SH30),
// a retry attempt that has found its cntlr and is about to wait for its
// object lock. It cancels nothing: the attempt waits for a read as it waits
// for another converge.
func (s *CnAgentServer) markConverge(key string) func() {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks.BeginLocked(key, now)
	return func() { s.endMark(key, false) }
}

func (s *CnAgentServer) endMark(key string, node bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks.EndLocked(key)
	if node {
		s.marks.EndNodeLocked()
	}
}
