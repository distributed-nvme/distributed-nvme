package agent

import (
	"fmt"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// GateRevision returns nil when the request may be applied (incoming >=
// stored; equal means idempotent re-apply) and a rejection AgentReply for a
// stale revision (SH8).
func GateRevision(stored uint64, incoming uint64) *pb.AgentReply {
	if incoming < stored {
		return &pb.AgentReply{
			Code: common.ReplyCodeStaleRevision,
			Details: fmt.Sprintf(
				"stale revision %d < stored %d", incoming, stored),
		}
	}
	return nil
}

// UnknownObjectReply builds the SH9 rejection for an object the agent does
// not know; details names the missing pointer/id.
func UnknownObjectReply(format string, args ...any) *pb.AgentReply {
	return &pb.AgentReply{
		Code:    common.ReplyCodeUnknownObject,
		Details: fmt.Sprintf(format, args...),
	}
}

// OkReply is the AgentReply of an accepted request (code 0). Per-resource
// failures never travel here — they ride in the *Info (SH14, DN19).
func OkReply() *pb.AgentReply {
	return &pb.AgentReply{}
}
