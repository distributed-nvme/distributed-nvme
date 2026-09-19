package agent

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Teardown by sweep, the part both roles share.
//
// The principle (architecture.md §9.8): an agent decides what to REMOVE by
// comparing the desired state with the ACTUAL state of the node, and
// remembers nothing about past failures. Each pass enumerates what exists,
// subtracts what the desired state wants, removes the rest top-down, verifies
// every removal with a probe that cannot block on a dead remote, and
// recomputes "something is left" from scratch.
//
// SweepResult is the verdict of one such pass. It is a value that lives for
// the length of the pass and is then thrown away: nothing about it is stored,
// which is the whole point — a stored "pending" flag is memory of failure,
// and memory of failure is what let the old teardowns forget an object that
// would not go.

// LeftoverKind labels a leftover in the reply's details and in the agent log,
// so an operator reading either can tell which enumeration found it.
const (
	LeftoverKindDm      = "dm"
	LeftoverKindMd      = "md"
	LeftoverKindNvmet   = "nvmet"
	LeftoverKindNvmetNs = "nvmet_ns"
	LeftoverKindNvme    = "nvme"
	LeftoverKindRecord  = "record"
)

// MsgSweepLeftover is the §12 record naming everything one pass found that
// the desired state does not want. One record per pass, so a lingering
// leftover shows up in the agent log every round rather than once.
const MsgSweepLeftover = "sweep leftover"

// SweepResult is what one pass found: the objects still present that nothing
// wants, and the enumerations that did not answer.
type SweepResult struct {
	leftovers []string
	failures  []string
}

// Clean is the whole definition of "this scope is done". An enumeration that
// did not answer is NOT clean: it cannot prove the node holds nothing.
func (r *SweepResult) Clean() bool {
	return len(r.leftovers) == 0 && len(r.failures) == 0
}

// Add records one object of kind that is present and unwanted.
func (r *SweepResult) Add(kind string, name string) {
	r.leftovers = append(r.leftovers, kind+":"+name)
}

// Fail records an enumeration that did not answer. what names the
// enumeration, not the object — there may be no object to name.
func (r *SweepResult) Fail(what string, err error) {
	r.failures = append(r.failures, what+": "+err.Error())
}

// Log emits MsgSweepLeftover when the pass was not clean. ids are the
// object's own log attributes.
func (r *SweepResult) Log(ctx context.Context, ids ...any) {
	if r.Clean() {
		return
	}
	sort.Strings(r.leftovers)
	attrs := append([]any{}, ids...)
	attrs = append(attrs,
		slog.Int("leftover_cnt", len(r.leftovers)),
		slog.String("leftovers", strings.Join(r.leftovers, " ")),
		slog.String("failures", strings.Join(r.failures, "; ")))
	slog.InfoContext(ctx, MsgSweepLeftover, attrs...)
}

// Reply is the AgentReply every Syncup*, Check* and Get*Info of this scope
// carries: OK when the scope is clean, ReplyCodeLeftover otherwise.
func (r *SweepResult) Reply() *pb.AgentReply {
	if r.Clean() {
		return OkReply()
	}
	sort.Strings(r.leftovers)
	return LeftoverReply(r.leftovers, r.failures)
}
