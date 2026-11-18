package cnagent

import (
	"context"
	"errors"
	"log/slog"
	"path"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// CN12, The md check
//
// The sp worker asks the primary for an md check of one group at a time
// through the group's md_check.request_epoch (dnv-worker.md AR16). The
// primary starts md's own check by a write to md/sync_action, md runs it at
// its own speed settings, and the primary reports the request done once md
// reads idle again. The agent keeps in memory alone which request a check it
// started, or adopted running, was for (cntlrState.mdChecks), as it keeps the
// acted forced-start mark (cntlrState.forcedStarts).
// ---------------------------------------------------------------------------

// md/sync_action words the check reads and writes.
const (
	mdActionCheck = "check"
	mdActionIdle  = "idle"
)

// mdCheckRun is the md check this incarnation started, or adopted running,
// for one request of a group (Group.md_check.request_epoch), whether it
// started it, and whether it saw that check run to its end.
type mdCheckRun struct {
	epoch   uint64
	started bool
	done    bool
}

// SetSyncAction writes action into a running array's md/sync_action: a
// sysfs attribute, written in place like configfs (WriteFileDirect).
func (m *Md) SetSyncAction(ctx context.Context, dev, action string) error {
	return m.oc.WriteFileDirect(ctx,
		sysfsBlockDir+"/"+path.Base(dev)+"/md/sync_action", action)
}

// SetSyncMin writes sector into a running array's md/sync_min, where md's
// next requested check starts.
func (m *Md) SetSyncMin(ctx context.Context, dev, sector string) error {
	return m.oc.WriteFileDirect(ctx,
		sysfsBlockDir+"/"+path.Base(dev)+"/md/sync_min", sector)
}

// mdCheckAction is what the converge's md check does with one group's array
// (stepMdCheck), as mdCheckActionOf decides it.
type mdCheckAction int

const (
	// mdCheckNone writes nothing and changes no memory.
	mdCheckNone mdCheckAction = iota
	// mdCheckForget drops the memory: the array does not run, and a stopped
	// array ended any check md ran on it.
	mdCheckForget
	// mdCheckStopOld stops a check this incarnation started for a request
	// that is gone, md still running it, and then drops the memory.
	mdCheckStopOld
	// mdCheckStop stops a check md runs while the group does not suit one or
	// the array is degraded, and drops the memory once the stop is written.
	mdCheckStop
	// mdCheckAdopt takes a check md runs, which this incarnation has no
	// memory of, as the request's.
	mdCheckAdopt
	// mdCheckNote ends the request's check once md reads idle (noteMdCheck).
	mdCheckNote
	// mdCheckStart starts the request's check: sync_min back to 0, then
	// "check".
	mdCheckStart
)

// named reports whether a cntlr's read names a group's md check for the
// action (mdCheckDue): a start or a stop, which write to the array's md
// attributes, and an adoption, which only a converge makes. A note a read
// makes itself (noteMdCheck), and a memory dropped changes nothing a
// re-send would.
func (a mdCheckAction) named() bool {
	return a == mdCheckStopOld || a == mdCheckStop || a == mdCheckAdopt ||
		a == mdCheckStart
}

// mdCheckActionOf decides the converge's md check of one group of a primary
// from the array as read (detail), the group's request and the memory of
// the check (cntlrState.mdChecks), with nothing written (CN12, The md
// check). It is the one decision the converge acts on (stepMdCheck) and a
// cntlr's read names the group by (mdCheckDue). dropOld reports a memory of
// another request, which the converge drops before it acts. With a request
// in the group's record:
//
//   - while md checks the array, a check this incarnation has no memory of
//     is adopted as the request's; while the group stops suiting a check
//     (mdCheckSuits) or the array is degraded, the check is stopped (idle)
//     and forgotten once the stop is written, so that, while a request
//     stands, a repair's copy does not wait behind it, and it starts again
//     once the group suits a check;
//   - while md is idle, a check the request has ends (noteMdCheck); one it
//     has not is started — sync_min written back to 0, then "check" — when
//     the array runs whole with every member in sync and the group suits a
//     check.
//
// With no request, a check this incarnation started that md still runs is
// stopped; one it adopted is left to run, as one it never saw is; either way
// the memory goes. Another sync action of md (a resync, a recovery, a frozen
// array) starts and stops nothing.
func mdCheckActionOf(
	st *cntlrState,
	gp *grpPlan,
	detail *MdDetail,
) (action mdCheckAction, dropOld bool) {
	if detail == nil || !detail.Exists || !mdRunning(detail.State) {
		return mdCheckForget, false
	}
	req := gp.grp.GetMdCheck().GetRequestEpoch()
	run, have := st.mdChecks[gp.grpId]
	if have && run.epoch != req {
		if req == 0 && run.started && !run.done &&
			detail.SyncAction == mdActionCheck {
			return mdCheckStopOld, false
		}
		dropOld, have = true, false
	}
	if req == 0 {
		return mdCheckNone, dropOld
	}
	switch {
	case detail.SyncAction == mdActionCheck:
		if !mdCheckSuits(gp) || detail.Degraded != 0 {
			return mdCheckStop, dropOld
		}
		if !have {
			return mdCheckAdopt, dropOld
		}
	case detail.SyncAction != mdActionIdle:
	case have:
		return mdCheckNote, dropOld
	case mdCheckSuits(gp) && mdCheckable(detail):
		return mdCheckStart, dropOld
	}
	return mdCheckNone, dropOld
}

// stepMdCheck is the converge's md check of one group of a primary, from
// the array as the pass left it, which runs, its member reconciliation's
// failure aside (ensureGroup; CN12, The md check): it takes the action
// mdCheckActionOf decides. A write that fails is logged, and the
// memory changes only on a write that succeeded, so the decision stands and
// a cntlr's read names the group while it does (mdCheckDue): the worker's
// re-send brings the converge that writes again.
func (s *CnAgentServer) stepMdCheck(
	ctx context.Context,
	st *cntlrState,
	gp *grpPlan,
	detail *MdDetail,
) {
	action, dropOld := mdCheckActionOf(st, gp, detail)
	switch action {
	case mdCheckForget:
		delete(st.mdChecks, gp.grpId)
		return
	case mdCheckStopOld:
		// A stop that fails leaves the memory, so the decision stands.
		if s.setSyncAction(ctx, gp, detail, mdActionIdle) {
			delete(st.mdChecks, gp.grpId)
		}
		return
	}
	if dropOld {
		delete(st.mdChecks, gp.grpId)
	}
	req := gp.grp.GetMdCheck().GetRequestEpoch()
	switch action {
	case mdCheckStop:
		if s.setSyncAction(ctx, gp, detail, mdActionIdle) {
			delete(st.mdChecks, gp.grpId)
		}
	case mdCheckAdopt:
		st.mdChecks[gp.grpId] = mdCheckRun{epoch: req}
	case mdCheckNote:
		noteMdCheck(st, gp, detail)
	case mdCheckStart:
		if err := s.md.SetSyncMin(ctx, detail.Dev, "0"); err != nil {
			slog.ErrorContext(ctx, "md sync_min write failed",
				slog.Uint64("grp_id", gp.grpId),
				slog.String("array", detail.Dev),
				slog.String("error", err.Error()))
			return
		}
		if s.setSyncAction(ctx, gp, detail, mdActionCheck) {
			st.mdChecks[gp.grpId] = mdCheckRun{epoch: req, started: true}
		}
	}
}

// errMdCheckDue is the failure a cntlr's read names a group's md check by
// while a converge would start, stop or adopt it (mdCheckDue).
var errMdCheckDue = errors.New(
	"a converge starts, stops or adopts md's check of the group's array")

// mdCheckDue reports whether a cntlr's read names a group's md check (CN12,
// The md check; CN30): the group's row of the read reads OK, and the
// converge's decision (mdCheckActionOf) over the array the read found is a
// start, a stop or an adoption (mdCheckAction.named). The converge steps the
// check over every running array its pass read, a member reconciliation
// that failed included, but while the in-sync guard holds, whose row a read
// reads as ERROR too (ensureGroup), so the name is one a converge cures: it
// stands only while the converge's own read of the array does not answer —
// before its member reconciliation, which its group row reads, or after
// member commands that changed the array (arrayAfter), while its row reads
// the command's error — while the converge's in-sync guard holds where the
// read's does not (cnagent.md, Known limits), or while md refuses the write. A name only a
// read gives: the converge that acts answers no name for it, so a write md
// keeps refusing brings a converge every second round at most
// (dnv-worker.md RW4). Reason: a converge's pass can leave the check
// unstepped, as the delivering converge of a request whose array read did
// not answer does, and with no name the request would stand, with no
// converge to come, until the sp worker's MdCheckRequestBound ends it.
func mdCheckDue(
	st *cntlrState,
	gp *grpPlan,
	row *pb.ResInfo,
	detail *MdDetail,
) bool {
	if row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
		return false
	}
	action, _ := mdCheckActionOf(st, gp, detail)
	return action.named()
}

// forgetMdCheck drops the memory of a group's md check when the converge
// finds its array not running: a stopped array ended any check.
func forgetMdCheck(st *cntlrState, gp *grpPlan) {
	delete(st.mdChecks, gp.grpId)
}

// keepMdChecks drops the memory of the md check (cntlrState.mdChecks) of
// each group the request no longer lists for a primary at a level that
// builds groups, and of every group otherwise: a demotion or a level that
// stops the arrays stops any check. Every converge runs it, from the
// request alone.
func keepMdChecks(st *cntlrState, plan *cntlrPlan) {
	for grpId := range st.mdChecks {
		if !plan.primary || !plan.wantGrp || plan.grpById[grpId] == nil {
			delete(st.mdChecks, grpId)
		}
	}
}

// mdCheckSuits says whether the group suits a check from the request alone:
// no repair, no forced-start mark, no operator-in-charge flag, and no leg
// with two sides, which a check would read whole through the destination's
// dm-clone and so copy entirely. The mark is the group record's, so a stored
// request's counts too, though the converge does not act on it (CN8).
func mdCheckSuits(gp *grpPlan) bool {
	if gp.rep != nil || gp.grp.GetForcedStart() != nil ||
		gp.grp.GetOperatorInCharge() {
		return false
	}
	for _, lp := range gp.legs {
		if len(lp.sides) != 1 {
			return false
		}
	}
	return true
}

// mdStateReadonly is the array_state of an array md holds read-only, which
// counts as running (mdRunningStates).
const mdStateReadonly = "readonly"

// mdCheckable says whether a check may start on a group's running array: it
// runs whole, with two or more members, every one in sync, and is not held
// read-only. Reason for the last: md takes a "check" written to the
// sync_action of a read-only array and starts nothing, so md would read idle
// again at once and the start would be decided, and named by a read
// (mdCheckDue), on every pass. A read-auto array is not read-only for this:
// md makes it read-write on the write.
func mdCheckable(detail *MdDetail) bool {
	if detail.Degraded != 0 || len(detail.Members) < 2 ||
		detail.State == mdStateReadonly {
		return false
	}
	for _, member := range detail.Members {
		if !member.inSync() {
			return false
		}
	}
	return true
}

// setSyncAction writes action for the group's array and reports whether
// the write went through; a failure is logged.
func (s *CnAgentServer) setSyncAction(
	ctx context.Context,
	gp *grpPlan,
	detail *MdDetail,
	action string,
) bool {
	err := s.md.SetSyncAction(ctx, detail.Dev, action)
	if err != nil {
		slog.ErrorContext(ctx, "md sync_action write failed",
			slog.Uint64("grp_id", gp.grpId),
			slog.String("array", detail.Dev),
			slog.String("action", action),
			slog.String("error", err.Error()))
		return false
	}
	slog.InfoContext(ctx, "md sync_action written",
		slog.Uint64("grp_id", gp.grpId),
		slog.String("array", detail.Dev),
		slog.String("action", action),
		slog.Uint64("request_epoch", gp.grp.GetMdCheck().GetRequestEpoch()))
	return true
}

// noteMdCheck ends the check a group's request has once md reads idle on
// its running array: done when md ran it to its end — its last action the
// check, sync_min back at 0 and the array whole — and forgotten otherwise,
// md having stopped it (a member failed, say), so that a later converge
// starts it again from the array's start. Both channels run it, a read as
// well as a converge, since md ends a check with no syncup to start a
// converge; it changes the agent's memory alone, never the node.
func noteMdCheck(st *cntlrState, gp *grpPlan, detail *MdDetail) {
	run, have := st.mdChecks[gp.grpId]
	if !have || run.done || run.epoch !=
		gp.grp.GetMdCheck().GetRequestEpoch() || detail == nil ||
		!mdRunning(detail.State) || detail.SyncAction != mdActionIdle {
		return
	}
	if detail.LastSyncAction != mdActionCheck || detail.SyncMin != "0" ||
		detail.Degraded != 0 {
		delete(st.mdChecks, gp.grpId)
		return
	}
	run.done = true
	st.mdChecks[gp.grpId] = run
}

// mdCheckDone is the request a group's reading reports done (CN28): the
// group's request, when this incarnation's check for it has ended and the
// held request counts as delivered (cntlrState.reqFromRpc), and zero
// otherwise, the same on a read as on a converge.
func mdCheckDone(st *cntlrState, gp *grpPlan) uint64 {
	req := gp.grp.GetMdCheck().GetRequestEpoch()
	run, have := st.mdChecks[gp.grpId]
	if req == 0 || !have || !run.done || run.epoch != req ||
		!st.reqFromRpc {
		return 0
	}
	return req
}
