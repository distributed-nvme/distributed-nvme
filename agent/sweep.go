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
// The principle (architecture.md, Teardown by sweep): an agent decides what to
// REMOVE by comparing the desired state with the ACTUAL state of the node, and
// remembers nothing about past failures. Each pass enumerates what exists,
// subtracts what the desired state wants, removes the rest top-down, verifies
// every removal with a probe that cannot block on a dead remote, and
// recomputes "something is left" from scratch.
//
// SweepResult is the verdict of one such pass. It is a value that lives for
// the length of the pass and is then thrown away: nothing about it is stored,
// which is the whole point — a stored "pending" flag is memory of failure,
// and a teardown that remembers failure forgets an object that would not go.

// LeftoverKind labels a leftover in the reply's details and in the agent log,
// so an operator reading either can tell which enumeration found it.
// LeftoverKindMdMember, LeftoverKindMdBroken and LeftoverKindMdAbsent are
// the three labels no sweep's attribution produces. LeftoverKindMdMember
// names an md member that a cn agent's cntlr verdict reports so that the
// worker re-sends the SyncupCntlr whose converge fails it early for its dead
// leg, puts the member back, adds it to an array that lacks it, or removes a
// repair's member md holds without a place; LeftoverKindMdBroken names, by
// its node, a broken md array of a wanted group that the converge would stop
// and assemble again (cnagent.md CN12, The re-assembly; CN30);
// LeftoverKindMdAbsent names, by the group's row name, the absent md array
// of a wanted group that the converge's assembly would start (cnagent.md
// CN12, CN30).
const (
	LeftoverKindDm       = "dm"
	LeftoverKindMd       = "md"
	LeftoverKindMdMember = "md_member"
	LeftoverKindMdBroken = "md_broken"
	LeftoverKindMdAbsent = "md_absent"
	LeftoverKindNvmet    = "nvmet"
	LeftoverKindNvmetNs  = "nvmet_ns"
	LeftoverKindNvme     = "nvme"
	LeftoverKindRecord   = "record"
)

// MsgSweepLeftover is the record of log.md, Leftovers, naming everything one
// pass found that keeps its scope from being clean (SweepResult). One record
// per pass — every converge and every background read — so a lingering
// leftover shows up in the agent log again and again rather than once.
const MsgSweepLeftover = "sweep leftover"

// SweepResult is what one pass found: the leftovers — the objects still
// present that nothing wants and, under LeftoverKindMdMember,
// LeftoverKindMdBroken and LeftoverKindMdAbsent, the wanted md members,
// broken arrays and absent arrays a cntlr verdict names (cnagent.md CN30) —
// and the failures — each
// enumeration or read that did not answer, each removal that failed, and each
// other condition an agent reports the same way so that the worker re-sends
// the Syncup* whose converge acts on it (LeftoverReply).
type SweepResult struct {
	leftovers []string
	failures  []SweepFailure
}

// SweepFailure is one failure of a pass: What names what failed, an
// enumeration or the object a read, a removal or a condition is about, and
// Cause is the error's text. They are kept apart so the reply can cut the
// cause alone and keep the name whole (LeftoverReply).
type SweepFailure struct {
	What  string
	Cause string
}

// Clean is the whole definition of "this scope is done". An enumeration that
// did not answer is NOT clean: it cannot prove the node holds nothing.
func (r *SweepResult) Clean() bool {
	return len(r.leftovers) == 0 && len(r.failures) == 0
}

// Add records one object of kind that is present and unwanted, or, under
// LeftoverKindMdMember, an md member the converge would fail early, put
// back, add or remove for a repair, under LeftoverKindMdBroken, a broken
// array the converge would assemble again, or, under LeftoverKindMdAbsent,
// an absent array the converge would start.
func (r *SweepResult) Add(kind string, name string) {
	r.leftovers = append(r.leftovers, kind+":"+name)
}

// Fail records one failure. what names what failed: an enumeration, where
// there may be no object to name, or the object that a read, a removal or a
// condition is about.
func (r *SweepResult) Fail(what string, err error) {
	r.failures = append(r.failures, SweepFailure{What: what, Cause: err.Error()})
}

// Log emits MsgSweepLeftover when the pass was not clean. ids are the
// object's own log attributes.
func (r *SweepResult) Log(ctx context.Context, ids ...any) {
	if r.Clean() {
		return
	}
	sort.Strings(r.leftovers)
	failures := make([]string, 0, len(r.failures))
	for _, failure := range r.failures {
		failures = append(failures, failure.What+": "+failure.Cause)
	}
	attrs := append([]any{}, ids...)
	attrs = append(attrs,
		slog.Int("leftover_cnt", len(r.leftovers)),
		slog.String("leftovers", strings.Join(r.leftovers, " ")),
		slog.String("failures", strings.Join(failures, "; ")))
	slog.InfoContext(ctx, MsgSweepLeftover, attrs...)
}

// Reply is the AgentReply a converge or a background read of this scope
// stores as its observation's code, and a Syncup* reply carries: OK when the
// scope is clean, ReplyCodeLeftover otherwise.
func (r *SweepResult) Reply() *pb.AgentReply {
	if r.Clean() {
		return OkReply()
	}
	sort.Strings(r.leftovers)
	return LeftoverReply(r.leftovers, r.failures)
}
