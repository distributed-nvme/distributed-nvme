package agentfake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Observation stamps (dnagent.md SH28)
// ---------------------------------------------------------------------------

// observation is one object's latest observation (dnagent.md SH28): when it
// ended, the trace id it was made under, and what a reply that answers from
// it carries — the code, the revision and the *Info — so that a reply that
// repeats it repeats all of it, as an agent whose reads stall does
// (Object.ObsAge). Once stored it is never changed.
type observation struct {
	time     time.Time
	traceId  string
	code     uint32
	details  string
	revision uint64
	info     proto.Message
}

// answer is what one reply carries about its object: the code, the
// revision, the *Info, nil for none, and the stamp.
type answer struct {
	code     uint32
	details  string
	revision uint64
	info     proto.Message
	stamp    *pb.ObsStamp
	// noObs marks a round that answers from no observation (Object.ObsNone):
	// its stream's change-only rule passes it by, as an agent's stream
	// compares the *Info it carries against the last one it sent
	// (dnagent.md SH26).
	noObs bool
}

// observeLocked stores a new observation of an object, made at t under
// traceId and carrying what ans carries, and returns its time: t, moved
// later than the object's previous observation and than after, so that two
// observations never share a time and equal times keep meaning the same
// observation.
func (a *Agent) observeLocked(
	key, traceId string, t, after time.Time, ans *answer,
) time.Time {
	t = t.Round(0)
	if last := a.observed[key]; last != nil && !t.After(last.time) {
		t = last.time.Add(time.Nanosecond)
	}
	if !t.After(after) {
		t = after.Add(time.Nanosecond)
	}
	a.observed[key] = &observation{
		time:     t,
		traceId:  traceId,
		code:     ans.code,
		details:  ans.details,
		revision: ans.revision,
		info:     ans.info,
	}
	return t
}

// obsStamp is the ObsStamp of a reply that answers at answer from the
// observation made at obs under traceId: the answer time never before the
// observation's, and no observation for a zero obs.
func obsStamp(obs time.Time, traceId string, answer time.Time) *pb.ObsStamp {
	if answer.Before(obs) {
		answer = obs
	}
	stamp := &pb.ObsStamp{AnswerTime: agent.Timestamp(answer)}
	if !obs.IsZero() {
		stamp.ObsTime = agent.Timestamp(obs)
		stamp.ObsTraceId = traceId
	}
	return stamp
}

// readLocked answers one Check round (round) or one info read of an object
// under traceId and the observation levers. listed is whether the object's
// parent lists it, and info derives the object's *Info, told whether the
// reply knows the object; it returns nil when the reply carries none.
//
// A reply that refuses its object as unknown — one its parent does not list,
// or one a forced ReplyCodeUnknownObject refuses so — has no observation,
// whatever the levers say. For an object it knows, ObsNone answers as an
// agent that holds the object and no observation of it yet (dnagent.md
// SH28): code zero, revision zero, no *Info and the answer time alone.
// ObsAge answers from a new observation that ended that long before the
// answer, or, while that would reach back past the latest observation the
// fake gave of the object, repeats that one whole — its time, trace id,
// code, revision and *Info — and spends no leftover round, since the agent
// read nothing. Every other answer is a new observation made now, and a
// later repeat repeats it.
func (a *Agent) readLocked(
	key string, listed, round bool, traceId string,
	info func(known bool) proto.Message,
) answer {
	now := time.Now().Round(0)
	known := listed &&
		a.forcedCodeLocked(key) != common.ReplyCodeUnknownObject
	ob := a.objLocked(key)
	obsAt := now
	if known && ob != nil {
		if ob.ObsNone {
			return answer{stamp: obsStamp(time.Time{}, "", now), noObs: true}
		}
		if ob.ObsAge > 0 {
			obsAt = now.Add(-ob.ObsAge)
			if last := a.observed[key]; last != nil &&
				!obsAt.After(last.time) {
				return answer{
					code:     last.code,
					details:  last.details,
					revision: last.revision,
					info:     last.info,
					stamp:    obsStamp(last.time, last.traceId, now),
				}
			}
		}
	}
	var ans answer
	if round {
		result := a.checkGateLocked(key, listed)
		ans.code, ans.details, ans.revision =
			result.code, result.details, result.revision
	} else {
		ans.code, ans.details = a.infoCodeLocked(key)
		ans.revision = a.revisionLocked(key)
	}
	ans.info = info(known)
	if !known {
		ans.stamp = obsStamp(time.Time{}, "", now)
		return ans
	}
	obs := a.observeLocked(key, traceId, obsAt, time.Time{}, &ans)
	ans.stamp = obsStamp(obs, traceId, now)
	return ans
}

// syncupStampLocked is the stamp of a Syncup* reply under ctx's trace id,
// with the save time: saved is whether the call saved the request, and the
// save time it returns is when, unset otherwise. A reply whose code refuses
// the request carries neither, as a live agent's does (architecture.md,
// Live-state reporting): a code the worker reads as accepted is zero or
// ReplyCodeLeftover (dnagent.md SH9), which the reply_code lever can force
// without the fake saving anything. An accepted reply's converge makes a new
// observation, after the save, whatever the observation levers say; it
// carries the reply's code and *Info and the stored revision, what a round
// that answers from it carries.
func (a *Agent) syncupStampLocked(
	ctx context.Context, key string, code uint32, details string,
	info proto.Message, saved bool,
) (*pb.ObsStamp, *timestamppb.Timestamp) {
	if code != 0 && code != common.ReplyCodeLeftover {
		return nil, nil
	}
	now := time.Now().Round(0)
	var saveAt time.Time
	if saved {
		saveAt = now
	}
	traceId := callTraceId(ctx)
	obs := a.observeLocked(key, traceId, now, saveAt, &answer{
		code:     code,
		details:  details,
		revision: a.revisionLocked(key),
		info:     info,
	})
	return obsStamp(obs, traceId, time.Now().Round(0)),
		agent.Timestamp(saveAt)
}

// ---------------------------------------------------------------------------
// The gates (dnv-worker.md, Integration test plan, The fake agent)
// ---------------------------------------------------------------------------

// The details of the replies whose code a lever gives.
const (
	detailsForcedCode = "behavior.json reply_code"
	detailsLeftover   = "behavior.json leftover_rounds"
)

func agentReply(code uint32, details string) *pb.AgentReply {
	return &pb.AgentReply{Code: code, Details: details}
}

// forcedCodeLocked applies the reply_code lever, which forces
// agent_reply.code on every reply of an object. A forced non-zero code also
// suppresses the apply: an agent that answers "rejected" must not have
// stored the request, or the worker suite's "clear => the push succeeds"
// would have nothing left to push.
func (a *Agent) forcedCodeLocked(key string) uint32 {
	if ob := a.objLocked(key); ob != nil {
		return ob.ReplyCode
	}
	return 0
}

// leftoverLocked reports whether the leftover_rounds lever still stands for
// an object: it has rounds left to answer with the leftover code.
func (a *Agent) leftoverLocked(key string) bool {
	return a.leftRounds[key] != 0
}

// gateSyncupLocked is the revision gate: a Syncup* with a revision lower than
// the stored one is ReplyCodeStaleRevision, anything else applies
// (dnv-worker.md, Integration test plan, The fake agent). It returns the
// reply's code and details and whether to apply: a forced code applies
// nothing, and a standing leftover applies the request and answers
// ReplyCodeLeftover, as a real agent's converge that applied the request and
// still holds residue does.
func (a *Agent) gateSyncupLocked(
	key string, revision uint64,
) (uint32, string, bool) {
	if code := a.forcedCodeLocked(key); code != 0 {
		return code, detailsForcedCode, false
	}
	if obj := a.state[key]; obj != nil && revision < obj.Revision {
		return common.ReplyCodeStaleRevision, fmt.Sprintf(
			"stale revision %d < stored %d", revision, obj.Revision), false
	}
	if a.leftoverLocked(key) {
		return common.ReplyCodeLeftover, detailsLeftover, true
	}
	return 0, "", true
}

// syncupRevisionLocked is the revision of a Syncup* reply: the stored one, or
// for an accepted reply (code zero or ReplyCodeLeftover) the one the
// syncup_revision lever echoes in its place.
func (a *Agent) syncupRevisionLocked(key string, code uint32) uint64 {
	if code == 0 || code == common.ReplyCodeLeftover {
		if ob := a.objLocked(key); ob != nil && ob.SyncupRevision != nil {
			return *ob.SyncupRevision
		}
	}
	return a.revisionLocked(key)
}

// infoCodeLocked is the code of an info read's reply: the forced one, or
// ReplyCodeLeftover while a leftover stands, which an info read reports
// without spending a round of it.
func (a *Agent) infoCodeLocked(key string) (uint32, string) {
	if code := a.forcedCodeLocked(key); code != 0 {
		return code, ""
	}
	if a.leftoverLocked(key) {
		return common.ReplyCodeLeftover, detailsLeftover
	}
	return 0, ""
}

// gatePushLocked is the gate of the Push* RPCs. A push carries no revision:
// its only rejections are the forced code and an object its node's last
// syncup does not list or an id the object's last request does not name
// (dnv-worker.md, Integration test plan, The fake agent).
func (a *Agent) gatePushLocked(
	key string, resId uint64, known bool,
) (uint32, string) {
	if code := a.forcedCodeLocked(key); code != 0 {
		return code, detailsForcedCode
	}
	if !known {
		return common.ReplyCodeUnknownObject, fmt.Sprintf(
			"id %d is not in %s's last request", resId, key)
	}
	return 0, ""
}

// sideKnownLocked implements the fake's ordering rule (dnv-worker.md,
// Integration test plan, The fake agent): a side pointer absent from the DN's
// last SyncupDn.side_pointer_list is unknown, so its
// SyncupSide/CheckSide/PushMigrBitmap is refused with
// ReplyCodeUnknownObject. The worker's independent dn and sp roles must
// survive exactly this.
func (a *Agent) sideKnownLocked(ptr *pb.SidePointer) bool {
	req, _ := a.requestLocked(DnKey).(*pb.SyncupDnRequest)
	if req == nil {
		return false
	}
	want := sideObjKey(ptr)
	for _, known := range req.GetSidePointerList() {
		if sideObjKey(known) == want {
			return true
		}
	}
	return false
}

// cntlrKnownLocked is sideKnownLocked's cn twin: a cntlr absent from the CN's
// last SyncupCn.cntlr_pointer_list is unknown.
func (a *Agent) cntlrKnownLocked(ptr *pb.CntlrPointer) bool {
	req, _ := a.requestLocked(CnKey).(*pb.SyncupCnRequest)
	if req == nil {
		return false
	}
	want := cntlrObjKey(ptr)
	for _, known := range req.GetCntlrPointerList() {
		if cntlrObjKey(known) == want {
			return true
		}
	}
	return false
}

// migrKnownLocked reports whether a migr_id appears in the side's last
// accepted request, in either the source or the destination role.
func (a *Agent) migrKnownLocked(key string, migrId uint64) bool {
	req, _ := a.requestLocked(key).(*pb.SyncupSideRequest)
	if req == nil {
		return false
	}
	if src := req.GetMigrSrcConf(); src != nil && src.GetMigrId() == migrId {
		return true
	}
	if dst := req.GetMigrDstConf(); dst != nil && dst.GetMigrId() == migrId {
		return true
	}
	return false
}

// cloneKnownLocked reports whether a clone_id appears in the cntlr's last
// accepted request.
func (a *Agent) cloneKnownLocked(key string, cloneId uint64) bool {
	req, _ := a.requestLocked(key).(*pb.SyncupCntlrRequest)
	if req == nil {
		return false
	}
	for _, clone := range req.GetCloneList() {
		if clone.GetCloneId() == cloneId {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The Check* streams (dnv-worker.md, Integration test plan, The fake agent;
// architecture.md, Check streams)
// ---------------------------------------------------------------------------

// checkResult is what one Check* round decides under the lock, before the
// handler sends its single reply.
type checkResult struct {
	code     uint32
	details  string
	revision uint64
}

// checkGateLocked resolves a Check* round for one object: the gate's code and
// the object's stored revision (architecture.md, Check streams — a reply
// whose revision differs from the request's is what makes the worker re-issue
// the object's Syncup*). A round of a known object answered while a leftover
// stands answers ReplyCodeLeftover and spends one of the leftover's rounds.
func (a *Agent) checkGateLocked(key string, known bool) checkResult {
	if !known {
		return checkResult{
			code:    common.ReplyCodeUnknownObject,
			details: key + " is not in its parent's last pointer list",
		}
	}
	result := checkResult{revision: a.revisionLocked(key)}
	if code := a.forcedCodeLocked(key); code != 0 {
		result.code = code
		result.details = detailsForcedCode
		return result
	}
	if a.leftoverLocked(key) {
		result.code = common.ReplyCodeLeftover
		result.details = detailsLeftover
		a.leftRounds[key]--
		if a.leftRounds[key] == 0 {
			delete(a.leftRounds, key)
		}
	}
	return result
}

// infoTracker implements the change-only delivery of architecture.md, Check
// streams: show_info = true always sends the *Info, show_info = false sends
// it only when it changed since the previous reply ON THIS STREAM, and the
// first reply on a fresh stream always carries it.
type infoTracker struct {
	sent bool
	last []byte
}

func (t *infoTracker) include(showInfo bool, info proto.Message) bool {
	var fingerprint []byte
	if info != nil && info.ProtoReflect().IsValid() {
		fingerprint, _ = proto.MarshalOptions{Deterministic: true}.
			Marshal(info)
	}
	if !t.sent || showInfo || !bytes.Equal(fingerprint, t.last) {
		t.sent = true
		t.last = fingerprint
		return true
	}
	return false
}

// waitForRound applies the stream levers (dnv-worker.md, Integration test
// plan, The fake agent) between a Check* request and its reply, and reports
// whether the stream must be dropped.
//
// hang holds this object's round — and only this object's, the lock is never
// held while waiting — for as long as the lever is set, so the worker misses
// the reply and times the round out. It returns as soon as the stream's
// context is cancelled (the worker closes the stream on that timeout), so no
// goroutine is leaked per round, and it also returns once the lever is
// cleared, so a fake whose stream the worker happens to keep open cannot stay
// wedged: every lever change wakes it (notifyLocked).
//
// hang_rounds is hang counted: each of the next that many rounds of the
// object after the levers are written is held until its stream ends, and the
// rounds after them are answered — so a case misses exactly the rounds it
// asks for, whatever its own polls cost. Clearing the lever does not release
// a round it holds: the worker's round timeout ends it. drop_stream wins over
// both.
//
// delay_rounds_ms answers a round that is neither held nor hung that many
// milliseconds late — a late answer, still in time while the delay stays
// under the worker's round timeout. A stream that ends during the delay gets
// no reply.
func (a *Agent) waitForRound(
	ctx context.Context, key string,
) (bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		a.mu.Lock()
		hang, drop := false, false
		var delay time.Duration
		if ob := a.objLocked(key); ob != nil {
			hang, drop = ob.Hang, ob.DropStream
			delay = ob.DelayRounds
		}
		held := !drop && a.heldRounds[key] != 0
		if held {
			a.heldRounds[key]--
		}
		changed := a.changed
		a.mu.Unlock()
		if drop {
			return true, nil
		}
		if held {
			<-ctx.Done()
			return false, ctx.Err()
		}
		if !hang {
			if delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return false, ctx.Err()
				case <-timer.C:
				}
			}
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-changed:
		}
	}
}

// hangUnary applies the `hang` lever to a UNARY request, the way waitForRound
// applies it to a Check* round.
//
// It exists for the dnv-gateway integration suite (gateway.md AG2 and AG3):
// the gateway bounds every agent call by common.DefaultGatewayAgentTimeout,
// and the only way to prove that bound is a listening agent that never
// answers — distinct from a closed port, which fails instantly. The worker
// never calls any of the unary RPCs this guards.
//
// The lock is never held while waiting, so one hung request cannot block the
// rest of the agent, and the wait ends as soon as the caller's context does
// (which is what the gateway's timeout cancels) or the lever is cleared.
func (a *Agent) hangUnary(ctx context.Context, key string) error {
	return a.holdUnary(ctx, key, func(ob *Object) bool {
		return ob.Hang
	})
}

// hangSyncup applies the `hang_syncup` lever to one object's Syncup*, the way
// hang holds its Check rounds: an agent that takes the request and never
// answers it, as a stopped cn agent does with the demotion of dnv-worker.md
// RW22. The server interceptor logs the request on arrival, before this runs,
// so its record times it exactly. A held request ends unapplied when the
// caller's context does — the worker's per-call deadline (RW5) — and is
// applied as usual once the lever is cleared. hang alone never holds a
// Syncup*.
func (a *Agent) hangSyncup(ctx context.Context, key string) error {
	return a.holdUnary(ctx, key, func(ob *Object) bool {
		return ob.HangSyncup
	})
}

// holdUnary is the wait of hangUnary and hangSyncup: it holds one unary
// request of an object while lever reports true for the object's levers,
// re-read on every lever change with the lock released. The caller's end
// comes first: a call whose caller gave up, or which a Stop ended, by the
// time its lever is cleared ends as its caller's, never released.
func (a *Agent) holdUnary(
	ctx context.Context, key string, lever func(*Object) bool,
) error {
	for {
		if err := ctx.Err(); err != nil {
			return status.FromContextError(err).Err()
		}
		a.mu.Lock()
		hold := false
		if ob := a.objLocked(key); ob != nil {
			hold = lever(ob)
		}
		changed := a.changed
		a.mu.Unlock()
		if !hold {
			return nil
		}
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-changed:
		}
	}
}

// dropStreamError is what drop_stream closes a Check* stream with.
func dropStreamError() error {
	return status.Error(codes.Unavailable, "behavior.json drop_stream")
}

// ---------------------------------------------------------------------------
// service DiskNodeAgent
// ---------------------------------------------------------------------------

// GetDnSize replies the configured size (the worker never calls it).
func (a *Agent) GetDnSize(
	ctx context.Context, req *pb.GetDnSizeRequest,
) (*pb.GetDnSizeReply, error) {
	gen, err := a.arrive(ctx, MethodGetDnSize, DnKey, recvNone, req)
	if err != nil {
		return nil, err
	}
	if err := a.hangUnary(ctx, DnKey); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.liveLocked(ctx, gen); err != nil {
		return nil, err
	}
	return &pb.GetDnSizeReply{Size: a.sizeLocked()}, nil
}

// SyncupDn implements the revision gate for the "dn" object (dnv-worker.md,
// Integration test plan, The fake agent) and the Full sync rule of
// architecture.md, Common agent rules: side pointers that disappear from the
// list are dropped, as the real agent deletes their local files.
func (a *Agent) SyncupDn(
	ctx context.Context, req *pb.SyncupDnRequest,
) (*pb.SyncupDnReply, error) {
	gen, err := a.arrive(ctx, MethodSyncupDn, DnKey, recvSyncup, req)
	if err != nil {
		return nil, err
	}
	if err := a.hangSyncup(ctx, DnKey); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.liveLocked(ctx, gen); err != nil {
		return nil, err
	}
	a.reloadStateLocked(ctx)

	code, details, apply := a.gateSyncupLocked(DnKey, req.GetRevision())
	if apply {
		a.applyLocked(ctx, DnKey, req.GetRevision(), req)
		keep := make(map[string]bool, len(req.GetSidePointerList()))
		for _, ptr := range req.GetSidePointerList() {
			keep[sideObjKey(ptr)] = true
		}
		a.pruneLocked(sideObjPrefix, keep)
		a.saveStateLocked(ctx)
	}
	info := a.dnInfoLocked()
	stamp, saveTime := a.syncupStampLocked(
		ctx, DnKey, code, details, info, apply)
	return &pb.SyncupDnReply{
		AgentReply: agentReply(code, details),
		Revision:   a.syncupRevisionLocked(DnKey, code),
		DnInfo:     info,
		ObsStamp:   stamp,
		SaveTime:   saveTime,
	}, nil
}

// SyncupSide implements the ordering gate (a side pointer absent from the
// last SyncupDn is ReplyCodeUnknownObject) followed by the revision gate
// (dnv-worker.md, Integration test plan, The fake agent).
func (a *Agent) SyncupSide(
	ctx context.Context, req *pb.SyncupSideRequest,
) (*pb.SyncupSideReply, error) {
	key := sideObjKey(req.GetSidePointer())
	gen, err := a.arrive(ctx, MethodSyncupSide, key, recvSyncup, req)
	if err != nil {
		return nil, err
	}
	if err := a.hangSyncup(ctx, key); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.liveLocked(ctx, gen); err != nil {
		return nil, err
	}
	a.reloadStateLocked(ctx)

	if !a.sideKnownLocked(req.GetSidePointer()) {
		return &pb.SyncupSideReply{
			AgentReply: agentReply(common.ReplyCodeUnknownObject,
				key+" is not in the last SyncupDn.side_pointer_list"),
		}, nil
	}
	code, details, apply := a.gateSyncupLocked(key, req.GetRevision())
	if apply {
		a.applyLocked(ctx, key, req.GetRevision(), req)
		a.saveStateLocked(ctx)
	}
	info := a.sideInfoLocked(key)
	stamp, saveTime := a.syncupStampLocked(
		ctx, key, code, details, info, apply)
	return &pb.SyncupSideReply{
		AgentReply: agentReply(code, details),
		Revision:   a.syncupRevisionLocked(key, code),
		SideInfo:   info,
		BmInfo:     a.sideBmInfoLocked(key),
		ObsStamp:   stamp,
		SaveTime:   saveTime,
	}, nil
}

// PushMigrBitmap records one migration-bitmap chunk behind the push gate
// (dnv-worker.md, Integration test plan, The fake agent); the applied set
// derived from the recorded chunks is what the next SyncupSide reply reports
// as bm_info (architecture.md, Bitmap push protocol).
func (a *Agent) PushMigrBitmap(
	ctx context.Context, req *pb.PushMigrBitmapRequest,
) (*pb.PushMigrBitmapReply, error) {
	key := sideObjKey(req.GetSidePointer())
	gen, err := a.arrive(ctx, MethodPushMigrBitmap, key, recvPush, req)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.liveLocked(ctx, gen); err != nil {
		return nil, err
	}
	a.reloadStateLocked(ctx)

	known := a.sideKnownLocked(req.GetSidePointer()) &&
		a.migrKnownLocked(key, req.GetMigrId())
	code, details := a.gatePushLocked(key, req.GetMigrId(), known)
	if code == 0 {
		a.state[key].putChunk(req.GetMigrId(),
			migrChunkKey(req.GetBmIdx()), len(req.GetBitmap()))
		a.saveStateLocked(ctx)
	}
	return &pb.PushMigrBitmapReply{
		AgentReply: agentReply(code, details),
	}, nil
}

// GetDnInfo returns the same DnInfo the CheckDn stream reports.
func (a *Agent) GetDnInfo(
	ctx context.Context, req *pb.GetDnInfoRequest,
) (*pb.GetDnInfoReply, error) {
	gen, err := a.arrive(ctx, MethodGetDnInfo, DnKey, recvInfoRead, req)
	if err != nil {
		return nil, err
	}
	if err := a.hangUnary(ctx, DnKey); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.liveLocked(ctx, gen); err != nil {
		return nil, err
	}
	a.reloadStateLocked(ctx)
	ans := a.readLocked(DnKey, true, false, callTraceId(ctx),
		func(bool) proto.Message { return a.dnInfoLocked() })
	info, _ := ans.info.(*pb.DnInfo)
	return &pb.GetDnInfoReply{
		AgentReply: agentReply(ans.code, ans.details),
		Revision:   ans.revision,
		DnInfo:     info,
		ObsStamp:   ans.stamp,
	}, nil
}

// GetSideInfo returns the same SideInfo the CheckSide stream reports.
func (a *Agent) GetSideInfo(
	ctx context.Context, req *pb.GetSideInfoRequest,
) (*pb.GetSideInfoReply, error) {
	key := sideObjKey(req.GetSidePointer())
	gen, err := a.arrive(ctx, MethodGetSideInfo, key, recvInfoRead, req)
	if err != nil {
		return nil, err
	}
	if err := a.hangUnary(ctx, key); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.liveLocked(ctx, gen); err != nil {
		return nil, err
	}
	a.reloadStateLocked(ctx)

	if !a.sideKnownLocked(req.GetSidePointer()) {
		return &pb.GetSideInfoReply{
			AgentReply: agentReply(common.ReplyCodeUnknownObject,
				key+" is not in the last SyncupDn.side_pointer_list"),
			ObsStamp: obsStamp(time.Time{}, "", time.Now().Round(0)),
		}, nil
	}
	ans := a.readLocked(key, true, false, callTraceId(ctx),
		func(bool) proto.Message { return a.sideInfoLocked(key) })
	info, _ := ans.info.(*pb.SideInfo)
	return &pb.GetSideInfoReply{
		AgentReply: agentReply(ans.code, ans.details),
		Revision:   ans.revision,
		SideInfo:   info,
		ObsStamp:   ans.stamp,
	}, nil
}

// CheckDn serves the DN health stream: one reply per request, never an
// unsolicited message, with the *Info delivered per the change-only rule of
// architecture.md, Check streams.
func (a *Agent) CheckDn(
	stream grpc.BidiStreamingServer[pb.CheckDnRequest, pb.CheckDnReply],
) error {
	ctx := stream.Context()
	track, err := a.openStream(ctx, MethodCheckDn)
	if err != nil {
		return err
	}
	defer a.endStream(track)
	tracker := &infoTracker{}
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := a.arriveRound(ctx, track, DnKey, req); err != nil {
			return err
		}
		drop, err := a.waitForRound(
			agent.CheckRoundCtx(ctx, req.GetTraceId()), DnKey)
		if err != nil {
			return err
		}
		if drop {
			return dropStreamError()
		}
		a.mu.Lock()
		if err := a.liveLocked(ctx, track.gen); err != nil {
			a.mu.Unlock()
			return err
		}
		a.reloadStateLocked(ctx)
		ans := a.readLocked(DnKey, true, true, req.GetTraceId(),
			func(bool) proto.Message { return a.dnInfoLocked() })
		a.mu.Unlock()

		reply := &pb.CheckDnReply{
			AgentReply: agentReply(ans.code, ans.details),
			Revision:   ans.revision,
			ObsStamp:   ans.stamp,
		}
		info, _ := ans.info.(*pb.DnInfo)
		if !ans.noObs && tracker.include(req.GetShowInfo(), info) {
			reply.DnInfo = info
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

// CheckSide serves one side's health stream; a side pointer absent from the
// last SyncupDn is refused with ReplyCodeUnknownObject on every round
// (dnv-worker.md, Integration test plan, The fake agent).
func (a *Agent) CheckSide(
	stream grpc.BidiStreamingServer[pb.CheckSideRequest, pb.CheckSideReply],
) error {
	ctx := stream.Context()
	track, err := a.openStream(ctx, MethodCheckSide)
	if err != nil {
		return err
	}
	defer a.endStream(track)
	tracker := &infoTracker{}
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		key := sideObjKey(req.GetSidePointer())
		if err := a.arriveRound(ctx, track, key, req); err != nil {
			return err
		}
		drop, err := a.waitForRound(
			agent.CheckRoundCtx(ctx, req.GetTraceId()), key)
		if err != nil {
			return err
		}
		if drop {
			return dropStreamError()
		}
		a.mu.Lock()
		if err := a.liveLocked(ctx, track.gen); err != nil {
			a.mu.Unlock()
			return err
		}
		a.reloadStateLocked(ctx)
		ans := a.readLocked(key, a.sideKnownLocked(req.GetSidePointer()), true,
			req.GetTraceId(), func(known bool) proto.Message {
				if !known {
					return (*pb.SideInfo)(nil)
				}
				return a.sideInfoLocked(key)
			})
		a.mu.Unlock()

		reply := &pb.CheckSideReply{
			AgentReply: agentReply(ans.code, ans.details),
			Revision:   ans.revision,
			ObsStamp:   ans.stamp,
		}
		info, _ := ans.info.(*pb.SideInfo)
		if !ans.noObs && tracker.include(req.GetShowInfo(), info) {
			reply.SideInfo = info
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

// ---------------------------------------------------------------------------
// service ControllerNodeAgent
// ---------------------------------------------------------------------------

// GetCnSize replies the configured size (the worker never calls it).
func (a *Agent) GetCnSize(
	ctx context.Context, req *pb.GetCnSizeRequest,
) (*pb.GetCnSizeReply, error) {
	gen, err := a.arrive(ctx, MethodGetCnSize, CnKey, recvNone, req)
	if err != nil {
		return nil, err
	}
	if err := a.hangUnary(ctx, CnKey); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.liveLocked(ctx, gen); err != nil {
		return nil, err
	}
	return &pb.GetCnSizeReply{Size: a.sizeLocked()}, nil
}

// SyncupCn implements the revision gate for the "cn" object (dnv-worker.md,
// Integration test plan, The fake agent) and drops the cntlrs that disappeared
// from the pointer list (the Full sync rule of architecture.md, Common agent
// rules).
func (a *Agent) SyncupCn(
	ctx context.Context, req *pb.SyncupCnRequest,
) (*pb.SyncupCnReply, error) {
	gen, err := a.arrive(ctx, MethodSyncupCn, CnKey, recvSyncup, req)
	if err != nil {
		return nil, err
	}
	if err := a.hangSyncup(ctx, CnKey); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.liveLocked(ctx, gen); err != nil {
		return nil, err
	}
	a.reloadStateLocked(ctx)

	code, details, apply := a.gateSyncupLocked(CnKey, req.GetRevision())
	if apply {
		a.applyLocked(ctx, CnKey, req.GetRevision(), req)
		keep := make(map[string]bool, len(req.GetCntlrPointerList()))
		for _, ptr := range req.GetCntlrPointerList() {
			keep[cntlrObjKey(ptr)] = true
		}
		a.pruneLocked(cntlrObjPrefix, keep)
		a.saveStateLocked(ctx)
	}
	info := a.cnInfoLocked()
	stamp, saveTime := a.syncupStampLocked(
		ctx, CnKey, code, details, info, apply)
	return &pb.SyncupCnReply{
		AgentReply: agentReply(code, details),
		Revision:   a.syncupRevisionLocked(CnKey, code),
		CnInfo:     info,
		ObsStamp:   stamp,
		SaveTime:   saveTime,
	}, nil
}

// SyncupCntlr implements the ordering gate (a cntlr absent from the last
// SyncupCn is ReplyCodeUnknownObject) followed by the revision gate
// (dnv-worker.md, Integration test plan, The fake agent).
func (a *Agent) SyncupCntlr(
	ctx context.Context, req *pb.SyncupCntlrRequest,
) (*pb.SyncupCntlrReply, error) {
	key := cntlrObjKey(req.GetCntlrPointer())
	gen, err := a.arrive(ctx, MethodSyncupCntlr, key, recvSyncup, req)
	if err != nil {
		return nil, err
	}
	if err := a.hangSyncup(ctx, key); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.liveLocked(ctx, gen); err != nil {
		return nil, err
	}
	a.reloadStateLocked(ctx)

	if !a.cntlrKnownLocked(req.GetCntlrPointer()) {
		return &pb.SyncupCntlrReply{
			AgentReply: agentReply(common.ReplyCodeUnknownObject,
				key+" is not in the last SyncupCn.cntlr_pointer_list"),
		}, nil
	}
	code, details, apply := a.gateSyncupLocked(key, req.GetRevision())
	if apply {
		a.applyLocked(ctx, key, req.GetRevision(), req)
		a.saveStateLocked(ctx)
	}
	info := a.cntlrInfoLocked(key)
	stamp, saveTime := a.syncupStampLocked(
		ctx, key, code, details, info, apply)
	return &pb.SyncupCntlrReply{
		AgentReply: agentReply(code, details),
		Revision:   a.syncupRevisionLocked(key, code),
		CntlrInfo:  info,
		BmInfoList: a.cntlrBmInfoListLocked(key),
		ObsStamp:   stamp,
		SaveTime:   saveTime,
	}, nil
}

// PushCloneBitmap records one clone-bitmap chunk behind the push gate
// (dnv-worker.md, Integration test plan, The fake agent), at the
// (src_slice_idx, bm_idx) pair the request addresses it by (architecture.md,
// Bitmap push protocol); the applied set is reported as the chunk_id_list of
// the next SyncupCntlr reply's bm_info_list.
func (a *Agent) PushCloneBitmap(
	ctx context.Context, req *pb.PushCloneBitmapRequest,
) (*pb.PushCloneBitmapReply, error) {
	key := cntlrObjKey(req.GetCntlrPointer())
	gen, err := a.arrive(ctx, MethodPushCloneBitmap, key, recvPush, req)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.liveLocked(ctx, gen); err != nil {
		return nil, err
	}
	a.reloadStateLocked(ctx)

	known := a.cntlrKnownLocked(req.GetCntlrPointer()) &&
		a.cloneKnownLocked(key, req.GetCloneId())
	code, details := a.gatePushLocked(key, req.GetCloneId(), known)
	if code == 0 {
		a.state[key].putChunk(req.GetCloneId(),
			cloneChunkKey(req.GetSrcSliceIdx(), req.GetBmIdx()),
			len(req.GetBitmap()))
		a.saveStateLocked(ctx)
	}
	return &pb.PushCloneBitmapReply{
		AgentReply: agentReply(code, details),
	}, nil
}

// GetCnInfo returns the same CnInfo the CheckCn stream reports.
func (a *Agent) GetCnInfo(
	ctx context.Context, req *pb.GetCnInfoRequest,
) (*pb.GetCnInfoReply, error) {
	gen, err := a.arrive(ctx, MethodGetCnInfo, CnKey, recvInfoRead, req)
	if err != nil {
		return nil, err
	}
	if err := a.hangUnary(ctx, CnKey); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.liveLocked(ctx, gen); err != nil {
		return nil, err
	}
	a.reloadStateLocked(ctx)
	ans := a.readLocked(CnKey, true, false, callTraceId(ctx),
		func(bool) proto.Message { return a.cnInfoLocked() })
	info, _ := ans.info.(*pb.CnInfo)
	return &pb.GetCnInfoReply{
		AgentReply: agentReply(ans.code, ans.details),
		Revision:   ans.revision,
		CnInfo:     info,
		ObsStamp:   ans.stamp,
	}, nil
}

// GetCntlrInfo returns the same CntlrInfo the CheckCntlr stream reports.
func (a *Agent) GetCntlrInfo(
	ctx context.Context, req *pb.GetCntlrInfoRequest,
) (*pb.GetCntlrInfoReply, error) {
	key := cntlrObjKey(req.GetCntlrPointer())
	gen, err := a.arrive(ctx, MethodGetCntlrInfo, key, recvInfoRead, req)
	if err != nil {
		return nil, err
	}
	if err := a.hangUnary(ctx, key); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.liveLocked(ctx, gen); err != nil {
		return nil, err
	}
	a.reloadStateLocked(ctx)

	if !a.cntlrKnownLocked(req.GetCntlrPointer()) {
		return &pb.GetCntlrInfoReply{
			AgentReply: agentReply(common.ReplyCodeUnknownObject,
				key+" is not in the last SyncupCn.cntlr_pointer_list"),
			ObsStamp: obsStamp(time.Time{}, "", time.Now().Round(0)),
		}, nil
	}
	ans := a.readLocked(key, true, false, callTraceId(ctx),
		func(bool) proto.Message { return a.cntlrInfoLocked(key) })
	info, _ := ans.info.(*pb.CntlrInfo)
	return &pb.GetCntlrInfoReply{
		AgentReply: agentReply(ans.code, ans.details),
		Revision:   ans.revision,
		CntlrInfo:  info,
		ObsStamp:   ans.stamp,
	}, nil
}

// GetThinDeviceBm replies an empty bitmap (the worker never calls it).
func (a *Agent) GetThinDeviceBm(
	ctx context.Context, req *pb.GetThinDeviceBmRequest,
) (*pb.GetThinDeviceBmReply, error) {
	_, err := a.arrive(ctx, MethodGetThinDeviceBm, CnKey, recvNone, req)
	if err != nil {
		return nil, err
	}
	if err := a.hangUnary(ctx, CnKey); err != nil {
		return nil, err
	}
	return &pb.GetThinDeviceBmReply{}, nil
}

// GetLegBm replies an empty bitmap (the worker never calls it).
func (a *Agent) GetLegBm(
	ctx context.Context, req *pb.GetLegBmRequest,
) (*pb.GetLegBmReply, error) {
	_, err := a.arrive(ctx, MethodGetLegBm, CnKey, recvNone, req)
	if err != nil {
		return nil, err
	}
	if err := a.hangUnary(ctx, CnKey); err != nil {
		return nil, err
	}
	return &pb.GetLegBmReply{}, nil
}

// CheckCn serves the CN health stream, one reply per request.
func (a *Agent) CheckCn(
	stream grpc.BidiStreamingServer[pb.CheckCnRequest, pb.CheckCnReply],
) error {
	ctx := stream.Context()
	track, err := a.openStream(ctx, MethodCheckCn)
	if err != nil {
		return err
	}
	defer a.endStream(track)
	tracker := &infoTracker{}
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := a.arriveRound(ctx, track, CnKey, req); err != nil {
			return err
		}
		drop, err := a.waitForRound(
			agent.CheckRoundCtx(ctx, req.GetTraceId()), CnKey)
		if err != nil {
			return err
		}
		if drop {
			return dropStreamError()
		}
		a.mu.Lock()
		if err := a.liveLocked(ctx, track.gen); err != nil {
			a.mu.Unlock()
			return err
		}
		a.reloadStateLocked(ctx)
		ans := a.readLocked(CnKey, true, true, req.GetTraceId(),
			func(bool) proto.Message { return a.cnInfoLocked() })
		a.mu.Unlock()

		reply := &pb.CheckCnReply{
			AgentReply: agentReply(ans.code, ans.details),
			Revision:   ans.revision,
			ObsStamp:   ans.stamp,
		}
		info, _ := ans.info.(*pb.CnInfo)
		if !ans.noObs && tracker.include(req.GetShowInfo(), info) {
			reply.CnInfo = info
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

// CheckCntlr serves one cntlr's health stream; a cntlr absent from the last
// SyncupCn is refused with ReplyCodeUnknownObject on every round
// (dnv-worker.md, Integration test plan, The fake agent).
func (a *Agent) CheckCntlr(
	stream grpc.BidiStreamingServer[pb.CheckCntlrRequest, pb.CheckCntlrReply],
) error {
	ctx := stream.Context()
	track, err := a.openStream(ctx, MethodCheckCntlr)
	if err != nil {
		return err
	}
	defer a.endStream(track)
	tracker := &infoTracker{}
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		key := cntlrObjKey(req.GetCntlrPointer())
		if err := a.arriveRound(ctx, track, key, req); err != nil {
			return err
		}
		drop, err := a.waitForRound(
			agent.CheckRoundCtx(ctx, req.GetTraceId()), key)
		if err != nil {
			return err
		}
		if drop {
			return dropStreamError()
		}
		a.mu.Lock()
		if err := a.liveLocked(ctx, track.gen); err != nil {
			a.mu.Unlock()
			return err
		}
		a.reloadStateLocked(ctx)
		ans := a.readLocked(key, a.cntlrKnownLocked(req.GetCntlrPointer()), true,
			req.GetTraceId(), func(known bool) proto.Message {
				if !known {
					return (*pb.CntlrInfo)(nil)
				}
				return a.cntlrInfoLocked(key)
			})
		a.mu.Unlock()

		reply := &pb.CheckCntlrReply{
			AgentReply: agentReply(ans.code, ans.details),
			Revision:   ans.revision,
			ObsStamp:   ans.stamp,
		}
		info, _ := ans.info.(*pb.CntlrInfo)
		if !ans.noObs && tracker.include(req.GetShowInfo(), info) {
			reply.CntlrInfo = info
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}
