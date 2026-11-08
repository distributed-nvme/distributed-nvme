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

// SaveFailedReply refuses a request the gates admitted whose save to the
// local store failed (SH5): details carry err, the save's error.
func SaveFailedReply(err error) *pb.AgentReply {
	return &pb.AgentReply{
		Code:    common.ReplyCodeSaveFailed,
		Details: fmt.Sprintf("saving the request failed: %v", err),
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
// an unanswered enumeration cannot prove the node is clean. The agents also
// report a few conditions through it so that the worker re-sends the Syncup*
// whose converge acts on them (architecture.md, Teardown by sweep): on a dn a
// disk identity not yet confirmed, a side with bytes left to zero and
// nothing zeroing it that is not a migration destination waiting for a free
// run in the clone-metadata area (dnagent.md DN16), or, from a converge
// alone, a side or clone-metadata record its pass could not free, which a
// read never attempts (dnagent.md DN19); on a cn a piece of the
// node's base state that the CN's background read probed absent, or an ANA
// group it read in a state other than its fixed one on a port whose
// transport attributes match, and a forced-start mark of a cntlr's request
// that does not count as delivered (cnagent.md CN8, CN12, CN30). Each
// arrives among the failures, rendered like an enumeration that did not
// answer. One condition arrives among the
// leftovers instead: on a cntlr, an md member that the cntlr's background
// read names as one its converge would put back or add, under
// LeftoverKindMdMember (cnagent.md CN30). Only a read's verdict names these
// conditions, save two of the dn's: the unconfirmed disk identity, which the
// converge's own code names too, since the record step that reports it runs
// in the converge, and the record a pass could not free, which only a
// converge's code names, since a read frees nothing (dnagent.md DN16, DN19);
// a converge's code names none of the others.
//
// A leftover is the one per-resource outcome that cannot ride in the *Info
// rows (SH14, CN29, DN19): the rows are keyed by the ids of WANTED objects,
// and a leftover is by definition something nothing wanted names — the md
// member above is the one wanted object listed among them. It is never
// stored as work owed — the agent recomputes it by enumerating the node in
// every converge and every background read, and keeps it only as the code of
// the object's observation (dnagent.md SH28, SH29), so the worker's ordinary
// "re-sync while the code is non-zero" rule is the whole retry machinery for
// a leftover.
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
