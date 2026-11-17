package cnagent

import (
	"context"
	"log/slog"
	"path"
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

// stepMdCheck is the converge's md check of one group of a primary, from
// the array as the pass left it, which runs (CN12, The md check). With a
// request in the group's record:
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
// array) starts and stops nothing. A write that fails is logged and tried
// again by the next converge; the memory changes only on a write that
// succeeded.
func (s *CnAgentServer) stepMdCheck(
	ctx context.Context,
	st *cntlrState,
	gp *grpPlan,
	detail *MdDetail,
) {
	if detail == nil || !detail.Exists || !mdRunning(detail.State) {
		// A stopped array ended any check md ran on it.
		delete(st.mdChecks, gp.grpId)
		return
	}
	req := gp.grp.GetMdCheck().GetRequestEpoch()
	run, have := st.mdChecks[gp.grpId]
	if have && run.epoch != req {
		if req == 0 && run.started && !run.done &&
			detail.SyncAction == mdActionCheck &&
			!s.setSyncAction(ctx, gp, detail, mdActionIdle) {
			// The stop failed: the memory stays, so the next converge
			// tries it again.
			return
		}
		delete(st.mdChecks, gp.grpId)
		have = false
	}
	if req == 0 {
		return
	}
	switch {
	case detail.SyncAction == mdActionCheck:
		if !mdCheckSuits(gp) || detail.Degraded != 0 {
			if s.setSyncAction(ctx, gp, detail, mdActionIdle) {
				delete(st.mdChecks, gp.grpId)
			}
			return
		}
		if !have {
			st.mdChecks[gp.grpId] = mdCheckRun{epoch: req}
		}
	case detail.SyncAction != mdActionIdle:
	case have:
		noteMdCheck(st, gp, detail)
	case mdCheckSuits(gp) && mdCheckable(detail):
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

// mdCheckable says whether a check may start on a group's running array: it
// runs whole, with two or more members, every one in sync.
func mdCheckable(detail *MdDetail) bool {
	if detail.Degraded != 0 || len(detail.Members) < 2 {
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
