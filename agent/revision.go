package agent

import (
	"fmt"
	"strings"

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

// InvalidConfReply and the codes above refuse a request. LeftoverReply is the
// one AgentReply that does not: the request was applied, the desired state is
// stored and every wanted object converged, but the node still holds objects
// the desired state does not want — or an enumeration of what exists did not
// answer, which is the same thing as far as the caller is concerned, because
// an unanswered enumeration cannot prove the node is clean.
//
// A leftover is the one per-resource outcome that cannot ride in the *Info
// rows (SH14, CN29, DN19): the rows are keyed by the ids of WANTED objects,
// and a leftover is by definition something nothing wanted names. It is never
// stored anywhere — the agent recomputes it by enumerating the node on every
// Syncup* and every Check*, so the worker's ordinary "re-sync while the code
// is non-zero" rule is the whole retry machinery.
//
// details names at most maxLeftoverNames objects so one stuck object cannot
// produce an unbounded reply; the agent log carries the full list every pass.
func LeftoverReply(leftovers []string, failures []string) *pb.AgentReply {
	var parts []string
	if len(leftovers) > 0 {
		shown := leftovers
		suffix := ""
		if len(shown) > maxLeftoverNames {
			shown = shown[:maxLeftoverNames]
			suffix = fmt.Sprintf(" [+%d more]",
				len(leftovers)-maxLeftoverNames)
		}
		parts = append(parts, fmt.Sprintf("leftover(%d): %s%s",
			len(leftovers), strings.Join(shown, ", "), suffix))
	}
	for _, failure := range failures {
		parts = append(parts, "enumeration failed: "+failure)
	}
	return &pb.AgentReply{
		Code:    common.ReplyCodeLeftover,
		Details: strings.Join(parts, "; "),
	}
}

// maxLeftoverNames bounds LeftoverReply's details.
const maxLeftoverNames = 8
