package dnagent

import (
	"context"
	"errors"
	"io"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The Check* streams follow SH24-SH26: rounds are worker-initiated, exactly
// one reply per received request, never an unsolicited send. A round answers
// from the object's latest observation (SH28) and takes no lock of the SH10 to
// SH13 hierarchy; the info rides along when show_info is set, on the first
// reply of the stream with an observation to carry, and whenever the
// observation's info differs from the last one actually sent. Each round runs under the trace id
// its own request carries (agent.CheckRoundCtx), and so does the background
// read it starts (SH29), which refreshDn and refreshSide are.

// streamEnded reports whether a Recv error is the normal end of a stream.
func streamEnded(ctx context.Context, err error) bool {
	return errors.Is(err, io.EOF) || ctx.Err() != nil
}

func (s *DnAgentServer) CheckDn(
	stream pb.DiskNodeAgent_CheckDnServer,
) error {
	ctx := stream.Context()
	var lastSent *pb.DnInfo
	for {
		req, err := stream.Recv()
		if err != nil {
			if streamEnded(ctx, err) {
				return nil
			}
			return err
		}
		reply, info := s.checkDnRound(
			agent.CheckRoundCtx(ctx, req.GetTraceId()), req, lastSent)
		if info != nil {
			lastSent = info
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

// checkDnRound answers one round from the DN's latest observation and starts
// a read of the DN when SH29 allows one. It returns the reply plus the info it
// carries (nil when the info was omitted).
func (s *DnAgentServer) checkDnRound(
	ctx context.Context,
	req *pb.CheckDnRequest,
	lastSent *pb.DnInfo,
) (*pb.CheckDnReply, *pb.DnInfo) {
	key := dnKey(req.GetClusterId(), req.GetDnId())
	s.mu.Lock()
	st := s.dns[key]
	if st == nil {
		s.mu.Unlock()
		// The stream stays open; the worker re-issues SyncupDn.
		return &pb.CheckDnReply{
			AgentReply: agent.UnknownObjectReply(
				"unknown dn %d", req.GetDnId()),
			ObsStamp: noObsStamp[*pb.DnInfo](s.now()),
		}, nil
	}
	obs, since, readCtx := roundObsLocked(s, ctx, key, &st.obs)
	s.mu.Unlock()
	if readCtx != nil {
		go func() {
			defer s.bg.Done()
			defer s.reads.Done()
			s.refreshDn(readCtx, key, st)
		}()
	}
	// The verdict is the observation's code: the one the last read or
	// converge of the DN found, so a leftover that has since gone stops
	// being reported once a read has found it gone, and one that is still
	// there keeps driving the worker's re-sync until the next SyncupDn
	// sweeps it away.
	reply := &pb.CheckDnReply{
		AgentReply: obs.Reply,
		Revision:   obs.Revision,
		ObsStamp:   obs.Stamp(s.now(), since),
	}
	info := obs.Info
	if info == nil ||
		(!req.GetShowInfo() && lastSent != nil && proto.Equal(info, lastSent)) {
		return reply, nil
	}
	reply.DnInfo = info
	return reply, info
}

func (s *DnAgentServer) CheckSide(
	stream pb.DiskNodeAgent_CheckSideServer,
) error {
	ctx := stream.Context()
	var lastSent *pb.SideInfo
	for {
		req, err := stream.Recv()
		if err != nil {
			if streamEnded(ctx, err) {
				return nil
			}
			return err
		}
		reply, info := s.checkSideRound(
			agent.CheckRoundCtx(ctx, req.GetTraceId()), req, lastSent)
		if info != nil {
			lastSent = info
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

// checkSideRound answers one round from the side's latest observation and
// starts a read of the side when SH29 allows one. It never creates a lock
// entry: the round takes no lock, and the read takes the side's lock only once
// it has found the side under the node read lock (refreshSide).
func (s *DnAgentServer) checkSideRound(
	ctx context.Context,
	req *pb.CheckSideRequest,
	lastSent *pb.SideInfo,
) (*pb.CheckSideReply, *pb.SideInfo) {
	key := sideKey(req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer().GetSpId(), req.GetSidePointer().GetSideId())
	s.mu.Lock()
	st := s.sides[key]
	if st == nil {
		s.mu.Unlock()
		// The stream stays open; the worker re-issues SyncupSide.
		return &pb.CheckSideReply{
			AgentReply: agent.UnknownObjectReply(
				"unknown side %s", sidePointerText(req.GetSidePointer())),
			ObsStamp: noObsStamp[*pb.SideInfo](s.now()),
		}, nil
	}
	obs, since, readCtx := roundObsLocked(s, ctx, key, &st.obs)
	s.mu.Unlock()
	if readCtx != nil {
		go func() {
			defer s.bg.Done()
			defer s.reads.Done()
			s.refreshSide(readCtx, key, st)
		}()
	}
	reply := &pb.CheckSideReply{
		// The side-level verdict. A dn's own Check reports node-level
		// leftovers only; each object drives its own Syncup*.
		AgentReply: obs.Reply,
		Revision:   obs.Revision,
		ObsStamp:   obs.Stamp(s.now(), since),
	}
	info := obs.Info
	if info == nil ||
		(!req.GetShowInfo() && lastSent != nil && proto.Equal(info, lastSent)) {
		return reply, nil
	}
	reply.SideInfo = info
	return reply, info
}

// noObsStamp is the stamp of a reply about an object the agent does not hold:
// no observation, only the agent's clock as it answers (SH25).
func noObsStamp[T proto.Message](now time.Time) *pb.ObsStamp {
	return (&agent.Observation[T]{}).Stamp(now, time.Time{})
}

// roundObsLocked is what a check round of the object key answers from: the
// object's latest observation and the start of a converge of it in flight,
// zero when none is (SH28, SH30). When SH29 allows a read — rounds may start
// reads (roundReads), none of the object runs, no converge of it and no
// SyncupDn that marks the node (beginDnSyncup) is in flight, and the server
// still starts background work — it
// begins one in the same hold of s.mu, enrolled in bg, and returns its ctx,
// which the caller runs the read on in the background; nil otherwise. The read
// runs on rootCtx, so it outlives the round and its stream, under the round's
// trace id, or an id of its own when the round carries none (SH29). The caller
// holds s.mu.
func roundObsLocked[T proto.Message](
	s *DnAgentServer,
	roundCtx context.Context,
	key string,
	slot *agent.ObsSlot[T],
) (agent.Observation[T], time.Time, context.Context) {
	obs := slot.LatestLocked()
	since := s.marks.SinceLocked(key)
	if !s.roundReads || slot.ReadingLocked() ||
		!s.marks.QuietLocked(key) || s.bgClosedLocked() {
		return obs, since, nil
	}
	traceId, ok := common.TraceIdFromCtx(roundCtx)
	if !ok {
		traceId = common.NewTraceId()
	}
	readCtx := slot.BeginReadLocked(common.WithTraceId(s.rootCtx, traceId))
	s.enrollLocked()
	s.reads.Add(1)
	return obs, since, readCtx
}

// writeObsLocked stores what a converge or a read of one object found as the
// object's latest observation (SH28): info and reply, the revision the
// converge converged or the read started under, the agent's clock now and
// ctx's trace id. Nothing is stored when ctx is done: a cancelled read and a
// converge whose call was cut off store nothing. The caller holds s.mu.
func writeObsLocked[T proto.Message](
	s *DnAgentServer,
	ctx context.Context,
	slot *agent.ObsSlot[T],
	info T,
	reply *pb.AgentReply,
	revision uint64,
) {
	traceId, _ := common.TraceIdFromCtx(ctx)
	slot.WriteLocked(ctx, agent.Observation[T]{
		Info:     info,
		Reply:    reply,
		Revision: revision,
		Time:     s.now(),
		TraceId:  traceId,
	})
}

// storeObs is writeObsLocked for the end of a converge, which runs before the
// converge lets go of its locks (SH28). It returns the object's latest
// observation, the one a syncup's reply stamps: the one just stored, or the
// one before it when ctx is done.
func storeObs[T proto.Message](
	s *DnAgentServer,
	ctx context.Context,
	slot *agent.ObsSlot[T],
	info T,
	reply *pb.AgentReply,
	revision uint64,
) agent.Observation[T] {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeObsLocked(s, ctx, slot, info, reply, revision)
	return slot.LatestLocked()
}

// endRead ends the read run under ctx (EndReadLocked), on every path out of
// it. A read that stored its observation has ended already, and a read a test
// ran with a ctx no round began ends nothing.
func endRead[T proto.Message](
	s *DnAgentServer,
	ctx context.Context,
	slot *agent.ObsSlot[T],
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot.EndReadLocked(ctx)
}

// refreshDn is the background read of one DN (SH29): under the node read lock,
// as a converge of it holds the write lock, it probes the DN and then takes the
// read-only verdict (DN16), and stores both as the DN's observation with the
// revision the DN held when the read took its lock. A DN the agent no longer
// holds as st, or a read cancelled before it got the lock, reads nothing; a
// cancelled one stores nothing, and the rows it makes once cancelled move no
// epoch (agent.ResTracker; probeDn writes through a view of its own).
//
// The probe runs first: its read of the disk can be the first that answers,
// which is what confirms DN5's identity, and a verdict taken before it would
// report the volume table unconfirmed beside a meta row that reads OK.
func (s *DnAgentServer) refreshDn(
	ctx context.Context,
	key string,
	st *dnState,
) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	defer endRead(s, ctx, &st.obs)
	if s.getDn(key) != st || ctx.Err() != nil {
		return
	}
	revision := st.req.Load().GetRevision()
	info := s.probeDn(ctx, st)
	reply := s.dnVerdict(ctx, st).Reply()
	s.mu.Lock()
	defer s.mu.Unlock()
	writeObsLocked(s, ctx, &st.obs, info, reply, revision)
	st.obs.EndReadLocked(ctx)
}

// refreshSide is the background read of one side (SH29). It checks the side
// under the node read lock before it takes the side's object lock, so a read
// of a side dropped meanwhile — the drop runs under the node write lock and
// forgets the side's lock entry (dropSideState) — creates no entry
// (dnagent.md, Concurrency — `locks.go`); it checks the side and its ctx again
// once it holds that lock, which a SyncupSide that cancelled it may have held.
// Then it probes the side and takes the read-only verdict, in that order: what
// the probe reads can move the agent's own view the verdict reads — a read of
// the disk that answers loads the volume table the side's record is read from
// (DN16) — and stores both as the side's observation with the revision the
// side held when the read took its locks. A cancelled read stores nothing,
// and the rows it makes once cancelled move no epoch: it binds its ctx to the
// side's tracker under the object lock (agent.ResTracker.Bind).
func (s *DnAgentServer) refreshSide(
	ctx context.Context,
	key string,
	st *sideState,
) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	defer endRead(s, ctx, &st.obs)
	if s.getSide(key) != st || ctx.Err() != nil {
		return
	}
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	if s.getSide(key) != st || ctx.Err() != nil {
		return
	}
	// A row made after the read is cancelled moves no epoch: the object lock
	// keeps every other pass of the side out (agent.ResTracker.Bind).
	defer st.tracker.Bind(ctx)()
	revision := st.req.Load().GetRevision()
	info := s.probeSide(ctx, st)
	reply := s.sideVerdict(ctx, st).Reply()
	s.mu.Lock()
	defer s.mu.Unlock()
	writeObsLocked(s, ctx, &st.obs, info, reply, revision)
	st.obs.EndReadLocked(ctx)
}

// beginSideConverge marks a background attempt's converge of the side key in
// flight from now on (SH30), once the attempt has found the side under the
// node read lock and before it waits for the object lock. It cancels
// nothing: the attempt waits for a read as it waits for another converge.
// endConverge ends the mark.
func (s *DnAgentServer) beginSideConverge(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks.BeginLocked(key, s.now())
}

// beginSideSyncup marks a SyncupSide's converge of the side key in flight as
// it arrives, before it waits for any lock, and cancels the side's running
// read in the same hold of s.mu, so no round in between starts another that
// the object lock could go to first (SH30). It reports whether it marked;
// endConverge ends the mark.
//
// A request DN8's gates refuse against the request the agent holds for the
// side — its revision (SH8), its pointer, or its side conf — marks nothing
// and cancels nothing: the worker re-sends such a request every round —
// after an etcd restore, say, or over a bad stored conf — and were it to
// cancel each round's read, no read would ever end and the observation
// would go stale (dnv-worker.md HL7). The gates run here under s.mu,
// silently, and again under the locks, and that run decides: the held
// revision only grows while the agent holds the side, so a stale request
// stays refused, and the DN request the other gates read changes only under
// a SyncupDn, so the two runs differ only when another syncup lands in
// between, a SyncupDn or a newer SyncupSide of this side, which costs that
// one syncup a wait for the read or one cancelled read. A side the agent
// does not hold, or a request the gates admit, is marked as before.
func (s *DnAgentServer) beginSideSyncup(
	key string,
	req *pb.SyncupSideRequest,
) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.sides[key]
	if st != nil {
		dn := s.dns[dnKey(req.GetClusterId(), req.GetDnId())]
		if reply, _, _ := sideGates(
			dn, req, st.req.Load().GetRevision()); reply != nil {
			return false
		}
	}
	s.marks.BeginLocked(key, s.now())
	if st != nil {
		st.obs.CancelReadLocked()
	}
	return true
}

// beginDnSyncup marks a SyncupDn's converge as it arrives, before the syncup
// waits for the node write lock (SH30), and returns what ends the marks it
// made once the converge has stored its observation, nil when it made none.
//
// A request of a revision newer than the one the agent holds for the DN, or
// for a DN it does not hold, marks the DN and the node and cancels every
// read of the agent: each read holds the node read lock the write lock waits
// for, and the node's mark keeps a round from starting a new one until the
// syncup ends.
//
// A request at the held revision is a re-send. The worker issues one on
// every round while the DN's code is not zero, as it is while a leftover
// lasts (dnv-worker.md RW4, RW12). It marks the DN and cancels the DN's own
// read alone. Were each re-send to cancel the reads of the sides and hold
// their reads off, a side's read would end only between two re-sends, none
// would while a read takes longer than that, and the observations of sides
// with no fault would go too old (dnv-worker.md HL7). Its write lock waits
// instead for the side reads that hold the node read lock when it asks, each
// of which runs bounded commands (SH13); a read a round starts after that
// waits for the node read lock until the syncup ends, so it takes no lock
// ahead of the syncup.
//
// A request DN4's gates refuse — the revision gate (SH8) against the held
// request, or the conf gate — marks nothing and cancels nothing, for
// beginSideSyncup's reason. The held revision only grows and the conf gate
// reads the request alone, so they refuse it again under the write lock.
func (s *DnAgentServer) beginDnSyncup(
	key string,
	req *pb.SyncupDnRequest,
) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var held *pb.SyncupDnRequest
	if st := s.dns[key]; st != nil {
		held = st.req.Load()
	}
	if agent.GateRevision(held.GetRevision(), req.GetRevision()) != nil ||
		agent.ValidateExtentSize(req.GetExtentSize()) != nil {
		return nil
	}
	s.marks.BeginLocked(key, s.now())
	if held != nil && req.GetRevision() == held.GetRevision() {
		s.dns[key].obs.CancelReadLocked()
		return func() { s.endConverge(key) }
	}
	s.marks.BeginNodeLocked()
	for _, st := range s.dns {
		st.obs.CancelReadLocked()
	}
	for _, st := range s.sides {
		st.obs.CancelReadLocked()
	}
	return func() { s.endDnSyncup(key) }
}

// endConverge ends one mark beginSideConverge, beginSideSyncup or a re-sent
// SyncupDn's beginDnSyncup made, once the converge has stored its
// observation.
func (s *DnAgentServer) endConverge(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks.EndLocked(key)
}

// endDnSyncup ends the two marks beginDnSyncup made for a newer request.
func (s *DnAgentServer) endDnSyncup(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks.EndLocked(key)
	s.marks.EndNodeLocked()
}
