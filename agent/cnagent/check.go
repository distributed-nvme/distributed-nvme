package cnagent

import (
	"context"
	"errors"
	"io"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// CN24 instantiates the SH24-SH26 check loop: rounds are worker-initiated,
// exactly one reply per received request, never an unsolicited send. A round
// answers from the object's latest observation (dnagent.md SH28): it takes no
// lock of the CN1 mapping, runs no OS call and changes nothing, and then
// starts the object's background read when SH29 allows one (observation.go).
// The read runs under the trace id the round's own request carries
// (agent.CheckRoundCtx). The info rides along when show_info is set, on the
// first reply of the stream, and whenever the observation's info differs
// from the last one actually sent; an object with no observation yet has no
// info to send.

// streamEnded reports whether a Recv error is the normal end of a stream.
func streamEnded(ctx context.Context, err error) bool {
	return errors.Is(err, io.EOF) || ctx.Err() != nil
}

func (s *CnAgentServer) CheckCn(
	stream pb.ControllerNodeAgent_CheckCnServer,
) error {
	ctx := stream.Context()
	var lastSent *pb.CnInfo
	for {
		req, err := stream.Recv()
		if err != nil {
			if streamEnded(ctx, err) {
				return nil
			}
			return err
		}
		reply, info := s.checkCnRound(
			agent.CheckRoundCtx(ctx, req.GetTraceId()), req, lastSent)
		if info != nil {
			lastSent = info
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

// checkCnRound answers one CheckCn round from the CN's observation
// (dnagent.md SH28). When a read made it, its code is the node-level
// read-only verdict (CN30); when a converge made it — a SyncupCn or the
// startup reconcile — its code is the one the converge's own reply carries,
// that of its node-level sweep, which removes. A leftover that has since
// gone stops being reported once a later read or converge stores its
// absence, and one that is still there keeps driving the worker's re-sync
// until it goes — a background disconnect finishing, or the next SyncupCn
// sweeping it away or building or rewriting what the probe read wrong.
func (s *CnAgentServer) checkCnRound(
	ctx context.Context,
	req *pb.CheckCnRequest,
	lastSent *pb.CnInfo,
) (*pb.CheckCnReply, *pb.CnInfo) {
	obs, since, known := s.answerCn(ctx,
		cnKey(req.GetClusterId(), req.GetCnId()), true)
	now := s.now()
	if !known {
		// The stream stays open; the worker re-issues SyncupCn.
		return &pb.CheckCnReply{
			AgentReply: unknownCn(req.GetCnId()),
			ObsStamp:   (&cnObs{}).Stamp(now, time.Time{}),
		}, nil
	}
	reply := &pb.CheckCnReply{
		AgentReply: obs.Reply,
		Revision:   obs.Revision,
		ObsStamp:   obs.Stamp(now, since),
	}
	info := obs.Info
	if info == nil ||
		!req.GetShowInfo() && lastSent != nil && proto.Equal(info, lastSent) {
		return reply, nil
	}
	reply.CnInfo = info
	return reply, info
}

func (s *CnAgentServer) CheckCntlr(
	stream pb.ControllerNodeAgent_CheckCntlrServer,
) error {
	ctx := stream.Context()
	var lastSent *pb.CntlrInfo
	for {
		req, err := stream.Recv()
		if err != nil {
			if streamEnded(ctx, err) {
				return nil
			}
			return err
		}
		reply, info := s.checkCntlrRound(
			agent.CheckRoundCtx(ctx, req.GetTraceId()), req, lastSent)
		if info != nil {
			lastSent = info
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

// checkCntlrRound answers one CheckCntlr round from the cntlr's observation.
// Its code is the cntlr-level verdict: a cn's own Check reports node-level
// leftovers only, and each object drives its own Syncup*.
func (s *CnAgentServer) checkCntlrRound(
	ctx context.Context,
	req *pb.CheckCntlrRequest,
	lastSent *pb.CntlrInfo,
) (*pb.CheckCntlrReply, *pb.CntlrInfo) {
	key := cntlrKey(req.GetClusterId(), req.GetCnId(),
		req.GetCntlrPointer().GetSpId(), req.GetCntlrPointer().GetCntlrId())
	obs, since, known := s.answerCntlr(ctx, key, true)
	now := s.now()
	if !known {
		// The stream stays open; the worker re-issues SyncupCntlr.
		return &pb.CheckCntlrReply{
			AgentReply: unknownCntlr(req.GetCntlrPointer()),
			ObsStamp:   (&cntlrObs{}).Stamp(now, time.Time{}),
		}, nil
	}
	reply := &pb.CheckCntlrReply{
		AgentReply: obs.Reply,
		Revision:   obs.Revision,
		ObsStamp:   obs.Stamp(now, since),
	}
	info := obs.Info
	if info == nil ||
		!req.GetShowInfo() && lastSent != nil && proto.Equal(info, lastSent) {
		return reply, nil
	}
	reply.CntlrInfo = info
	return reply, info
}
