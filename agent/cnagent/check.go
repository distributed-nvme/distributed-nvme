package cnagent

import (
	"context"
	"errors"
	"io"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// CN24 instantiates the SH24-SH26 check loop with the §4.12 probes: rounds are
// worker-initiated, exactly one reply per received request, never an
// unsolicited send. The info rides along when show_info is set, on the first
// reply of the stream, and whenever the freshly probed info differs from the
// last one actually sent. One round takes the CN1 locks of the corresponding
// Get*Info and never mutates.

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
		reply, info := s.checkCnRound(ctx, req, lastSent)
		if info != nil {
			lastSent = info
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

func (s *CnAgentServer) checkCnRound(
	ctx context.Context,
	req *pb.CheckCnRequest,
	lastSent *pb.CnInfo,
) (*pb.CheckCnReply, *pb.CnInfo) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()

	st := s.getCn(cnKey(req.GetClusterId(), req.GetCnId()))
	if st == nil {
		// The stream stays open; the worker re-issues SyncupCn.
		return &pb.CheckCnReply{
			AgentReply: agent.UnknownObjectReply(
				"unknown cn %d", req.GetCnId()),
		}, nil
	}
	info := s.probeCn(ctx, st)
	reply := &pb.CheckCnReply{
		AgentReply: agent.OkReply(),
		Revision:   st.req.GetRevision(),
	}
	if !req.GetShowInfo() && lastSent != nil && proto.Equal(info, lastSent) {
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
		reply, info := s.checkCntlrRound(ctx, req, lastSent)
		if info != nil {
			lastSent = info
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

func (s *CnAgentServer) checkCntlrRound(
	ctx context.Context,
	req *pb.CheckCntlrRequest,
	lastSent *pb.CntlrInfo,
) (*pb.CheckCntlrReply, *pb.CntlrInfo) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()

	key := cntlrKey(req.GetClusterId(), req.GetCnId(),
		req.GetCntlrPointer().GetSpId(), req.GetCntlrPointer().GetCntlrId())
	unknown := &pb.CheckCntlrReply{
		AgentReply: agent.UnknownObjectReply(
			"unknown cntlr %s", cntlrPointerText(req.GetCntlrPointer())),
	}
	// The stream stays open; the worker re-issues SyncupCntlr.
	if s.getCntlr(key) == nil {
		return unknown, nil
	}
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()

	st := s.getCntlr(key)
	if st == nil {
		return unknown, nil
	}
	info := s.probeCntlr(ctx, st)
	reply := &pb.CheckCntlrReply{
		AgentReply: agent.OkReply(),
		Revision:   st.req.GetRevision(),
	}
	if !req.GetShowInfo() && lastSent != nil && proto.Equal(info, lastSent) {
		return reply, nil
	}
	reply.CntlrInfo = info
	return reply, info
}
