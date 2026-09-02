package dnagent

import (
	"context"
	"errors"
	"io"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The Check* streams follow SH24-SH26: rounds are worker-initiated, exactly
// one reply per received request, never an unsolicited send. The info rides
// along when show_info is set, on the first reply of the stream, and whenever
// the freshly probed info differs from the last one actually sent.

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
		reply, info := s.checkDnRound(ctx, req, lastSent)
		if info != nil {
			lastSent = info
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

// checkDnRound runs one round under the DN1 node read lock and returns the
// reply plus the info it carries (nil when the info was omitted).
func (s *DnAgentServer) checkDnRound(
	ctx context.Context,
	req *pb.CheckDnRequest,
	lastSent *pb.DnInfo,
) (*pb.CheckDnReply, *pb.DnInfo) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()

	st := s.getDn(dnKey(req.GetClusterId(), req.GetDnId()))
	if st == nil {
		// The stream stays open; the worker re-issues SyncupDn.
		return &pb.CheckDnReply{
			AgentReply: agent.UnknownObjectReply(
				"unknown dn %d", req.GetDnId()),
		}, nil
	}
	info := s.probeDn(ctx, st)
	reply := &pb.CheckDnReply{
		AgentReply: agent.OkReply(),
		Revision:   st.req.GetRevision(),
	}
	if !req.GetShowInfo() && lastSent != nil && proto.Equal(info, lastSent) {
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
		reply, info := s.checkSideRound(ctx, req, lastSent)
		if info != nil {
			lastSent = info
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

func (s *DnAgentServer) checkSideRound(
	ctx context.Context,
	req *pb.CheckSideRequest,
	lastSent *pb.SideInfo,
) (*pb.CheckSideReply, *pb.SideInfo) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()

	key := sideKey(req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer().GetSpId(), req.GetSidePointer().GetSideId())
	unknown := &pb.CheckSideReply{
		AgentReply: agent.UnknownObjectReply(
			"unknown side %s", sidePointerText(req.GetSidePointer())),
	}
	// The stream stays open; the worker re-issues SyncupSide.
	if s.getSide(key) == nil {
		return unknown, nil
	}
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()

	st := s.getSide(key)
	if st == nil {
		return unknown, nil
	}
	info := s.probeSide(ctx, st)
	reply := &pb.CheckSideReply{
		AgentReply: agent.OkReply(),
		Revision:   st.req.GetRevision(),
	}
	if !req.GetShowInfo() && lastSent != nil && proto.Equal(info, lastSent) {
		return reply, nil
	}
	reply.SideInfo = info
	return reply, info
}
