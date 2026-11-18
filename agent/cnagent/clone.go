package cnagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// CN18 — clones (fig. `090Clone`), primary only and only below
// SP_LEVEL_NO_CLONE.

// detailsCloneNotHydrating is what `clone_id_to_dm_clone` reports, ERROR and
// followed by the raw status line, for a dm-clone that is up and whose status
// does not show hydration on: a build that stopped short of CN18 step 5, or a
// device under the clone's name that is not a dm-clone. CN16 does not serve
// through it (nsDevNow).
const detailsCloneNotHydrating = "hydration not enabled"

// detailsCloneMetaReadOnly and detailsCloneMetaFailed are what
// `clone_id_to_dm_clone` reports, ERROR and followed by the raw status line,
// for a dm-clone whose metadata mode is read-only or failed (dm-clone leaves
// read-write mode when an operation on its metadata device fails); and
// detailsCloneMetaModeUnknown for one that shows hydration on and no
// metadata mode at all. CN16 does not serve through any of them, and nothing
// the agent does takes such a dm-clone back to read-write mode (CN18).
const (
	detailsCloneMetaReadOnly    = "metadata read-only"
	detailsCloneMetaFailed      = "metadata failed"
	detailsCloneMetaModeUnknown = "metadata mode not read-write"
)

// detailsCloneMetaGrpNotReady is what `clone_id_to_dm_clone` reports, MISSING,
// for a clone whose source is ready and whose dm-clone is absent while the
// clone meta group's row of the pass is not OK (CN18 step 2; cloneGrpReady
// says when). The group's own row carries the fault, so this row does not
// report it a second time. detailsCloneDstNotReady is what it reports,
// MISSING, beside a ready source and a ready group while the destination
// td's raid0 row of the pass is not OK (cloneDstReady), whose own row
// carries that fault the same way.
const (
	detailsCloneMetaGrpNotReady = "metadata group not ready"
	detailsCloneDstNotReady     = "destination raid0 not ready"
)

// reportCloneNoDst fills the two rows of a clone whose dst_td_id is not in
// td_list, ERROR on each, shared by the converge and the probe (CN18): there
// is nothing to clone onto, so nothing of the clone is read or built.
func (s *CnAgentServer) reportCloneNoDst(
	st *cntlrState,
	cp *clonePlan,
	info *pb.CntlrInfo,
) {
	details := fmt.Sprintf("destination td %d not found",
		cp.clone.GetDstTdId())
	info.CloneIdToTarget[cp.cloneId] = st.tracker.Err(
		resKeyOf(resKeyCloneTgtFmt, cp.cloneId), cp.clone.GetSrcNqn(), details)
	info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
		resKeyOf(resKeyCloneDmFmt, cp.cloneId), cp.finalName, details)
}

// reportCloneNoGrp fills the two rows of a clone record that carries no clone
// meta group, ERROR on each, shared by the converge and the probe (CN18): the
// dm-clone has no metadata device, so nothing of the clone is read or built.
func (s *CnAgentServer) reportCloneNoGrp(
	st *cntlrState,
	cp *clonePlan,
	info *pb.CntlrInfo,
) {
	const details = "no metadata group"
	info.CloneIdToTarget[cp.cloneId] = st.tracker.Err(
		resKeyOf(resKeyCloneTgtFmt, cp.cloneId), cp.clone.GetSrcNqn(), details)
	info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
		resKeyOf(resKeyCloneDmFmt, cp.cloneId), cp.finalName, details)
}

// withNote puts a converge's failure in front of a row's own details, on a
// line of its own. The failure decides nothing: the row's status is what the
// pass read of the node, and the failure only rides along (CN29).
func withNote(note, details string) string {
	switch {
	case note == "":
		return details
	case details == "":
		return note
	}
	return note + "\n" + details
}

// ---------------------------------------------------------------------------
// The shared rows (CN18, CN28)
// ---------------------------------------------------------------------------

// cloneSource is one read of a clone's source subsystem: what both channels
// decide clone_id_to_target by (cloneTargetRow), and what decides whether the
// dm-clone row of an absent dm-clone names the source (ready).
type cloneSource struct {
	// inFlight: a sweep's disconnect of the source NQN is in flight
	// (disconnectInFlight), and the lookup does not run.
	inFlight bool
	// view is the lookup's last answer (lookupSubsys); nil when the lookup
	// did not answer or did not run.
	view *subsysView
	// readErr is the lookup that did not answer (errSubsysUnknown).
	readErr error
	// stepErr is a connect of the converge's CN18 step 1 that failed. The
	// probe connects nothing and never has one.
	stepErr error
	// headPending: the converge's CN18 step 1 ran a connect of the source in
	// this pass, and the read after it (the head wait, awaitNsHead, the
	// budget spent; or the lookup after a connect that failed) finds a
	// controller at every endpoint and no source namespace head
	// (headToCome). The kernel adds the head after it made the controller,
	// so it may be a scan still in progress, a build in progress the
	// source-fault reason must not hide from the settle: the target row's
	// "no namespace" and the dm-clone row's source text then carry no reason
	// (cloneTargetRow, cloneDmRow). The probe connects nothing and never has
	// it, nor does a converge whose step 1 connects nothing; each reads a
	// head still to come with the reason, which CN18 states as a known limit.
	headPending bool
}

// readCloneSource reads the source the way ensureCloneSource starts: the CN10
// disconnect registry first, and the lookup in the pass's walk only when no
// sweep's disconnect of the source is in flight.
func (s *CnAgentServer) readCloneSource(
	ctx context.Context,
	plan *cntlrPlan,
	cp *clonePlan,
) cloneSource {
	if s.disconnectInFlight(cp.clone.GetSrcNqn()) {
		return cloneSource{inFlight: true}
	}
	view, err := s.lookupSubsys(ctx, plan.walk, cp.clone.GetSrcNqn(),
		cp.clone.GetSrcNsIdx())
	return cloneSource{view: view, readErr: err}
}

// ready reports whether CN18 step 1 holds on this read: no sweep's disconnect
// of the source is in flight, the lookup answered, every configured endpoint
// has a controller, whatever its state, since the converge connects only the
// ones that are missing, and the source namespace device is there. It is not
// cloneSourceLive, which answers the target row; this answers "did the
// converge get past step 1", which is what makes the two channels name the
// same cause for an absent dm-clone (cloneDmRow). A read that still needs a
// connect is not ready: whether that connect would succeed is exactly what a
// read-only probe may not find out (CN23). A failed connect of the converge's
// is not asked: the probe never has one, and ensureCloneSource stops step 1
// on it itself.
func (src cloneSource) ready(cp *clonePlan) bool {
	return src.atEveryEndpoint(cp) && src.view.nsDev != ""
}

// headToCome reports whether this read is one ready would take but for the
// source namespace head: what the read after a connect of the pass finds
// while the kernel has made a controller and not yet added the head
// (headPending).
func (src cloneSource) headToCome(cp *clonePlan) bool {
	return src.atEveryEndpoint(cp) && src.view.nsDev == ""
}

// atEveryEndpoint reports whether no sweep's disconnect of the source is in
// flight, the lookup answered and found the subsystem, and every configured
// endpoint has a controller, whatever its state: step 1 has no endpoint to
// connect (srcToConnect).
func (src cloneSource) atEveryEndpoint(cp *clonePlan) bool {
	view := src.view
	if src.inFlight || src.readErr != nil || view == nil || !view.found {
		return false
	}
	return len(srcToConnect(cp, view)) == 0
}

// srcToConnect is CN18 step 1's connect test, which the converge connects by
// (ensureCloneSource) and a cntlr's read names by (toName, CN30): the entries
// of src_tr_conf_list, in the list's order, that view holds no controller
// for, in any state. A controller whose "address" read did not answer
// matches no entry, so an entry it alone serves is one to connect.
func srcToConnect(cp *clonePlan, view *subsysView) []*pb.NvmeTrConf {
	var out []*pb.NvmeTrConf
	for _, tr := range cp.clone.GetSrcTrConfList() {
		if view.ctrlOf(tr.GetTrAddr(), tr.GetTrSvcId()) == nil {
			out = append(out, tr)
		}
	}
	return out
}

// toName is what a cntlr's read names of the source (CN18, CN30): each entry
// step 1 would connect (srcToConnect), while no sweep's disconnect of the
// source is in flight, the lookup answered and every controller's "address"
// read answered; nothing otherwise. The converge connects such an entry
// also while an address read does not answer (ensureCloneSource), but that
// controller may be the entry's own, which the host then refuses to connect
// again as a duplicate while it lives, so a name would bring a re-send on
// every second round (dnv-worker.md RW4) for a connect the host refuses.
func (src cloneSource) toName(cp *clonePlan) []*pb.NvmeTrConf {
	view := src.view
	if src.inFlight || src.readErr != nil || view == nil ||
		view.unknownCtrl() != nil {
		return nil
	}
	return srcToConnect(cp, view)
}

// unready is the text of clone_id_to_dm_clone MISSING beside a source that is
// not ready: unknown when the lookup did not answer (CN10), since the pass does
// not know whether the source is connected, and not connected otherwise.
// cloneDmRow says when the row carries RES_REASON_SOURCE_FAULT with it.
func (src cloneSource) unready() string {
	if errors.Is(src.readErr, errSubsysUnknown) {
		return "source unknown: " + src.readErr.Error()
	}
	return "source not connected"
}

// cloneTargetRow is the clone_id_to_target row of one read of the source
// (CN28), the same for the converge and the probe: its status, details and
// reason. It reads ERROR only for this node's own side: while a sweep's
// disconnect of the source is in flight, or when the walk or the lookup did
// not answer. A fault of the source reads MISSING, as a source with no
// subsystem entry does: a configured endpoint with no live controller, its
// details the path states, or no source namespace. A MISSING row sets no
// health epoch, so the row does not move the role: moving the primary cannot
// repair the source, and the worker cannot tell a fault in this node's own
// path to the source from one at the source (CN18). Each MISSING the rule
// gives carries RES_REASON_SOURCE_FAULT, which the worker's settle reads as
// built (dnv-worker.md HL2), but the converge's "no namespace" after a
// connect of its own pass, with a controller at every endpoint
// (headPending): that head may be a scan still in progress, a build in
// progress the settle waits for, and once the head is there a read names the
// dm-clone a converge would build (cloneDmName). No other row of it carries
// a reason. The row reads OK, with
// the path states, while every endpoint has a live controller and the
// namespace is there. A failed connect of the converge's decides no status
// and leads the details of a row that is not OK.
func cloneTargetRow(
	cp *clonePlan,
	src cloneSource,
) (pb.ResStatus, string, pb.ResReason) {
	nqn := cp.clone.GetSrcNqn()
	note := ""
	if src.stepErr != nil {
		note = src.stepErr.Error()
	}
	view := src.view
	switch {
	case src.inFlight:
		return pb.ResStatus_RES_STATUS_ERROR, detailsDisconnectInFlight(nqn),
			pb.ResReason_RES_REASON_NONE
	case src.readErr != nil:
		return pb.ResStatus_RES_STATUS_ERROR,
			withNote(note, src.readErr.Error()), pb.ResReason_RES_REASON_NONE
	case view == nil || !view.found:
		return pb.ResStatus_RES_STATUS_MISSING, note,
			pb.ResReason_RES_REASON_SOURCE_FAULT
	case !cloneSourceLive(view, cp):
		return pb.ResStatus_RES_STATUS_MISSING,
			withNote(note, pathStates(view)),
			pb.ResReason_RES_REASON_SOURCE_FAULT
	case view.nsDev == "":
		details := fmt.Sprintf("no namespace %d on %s",
			cp.clone.GetSrcNsIdx(), nqn)
		reason := pb.ResReason_RES_REASON_SOURCE_FAULT
		if src.headPending {
			reason = pb.ResReason_RES_REASON_NONE
		}
		return pb.ResStatus_RES_STATUS_MISSING, withNote(note, details),
			reason
	}
	return pb.ResStatus_RES_STATUS_OK, pathStates(view),
		pb.ResReason_RES_REASON_NONE
}

// cloneDmRow is the clone_id_to_dm_clone row of one read of the dm-clone
// (CN28), the same for the converge and the probe (cloneRows): its status,
// details and reason. It reads ERROR when the read did not answer; MISSING
// when the device is not there, saying why — unready while the source is not
// ready, with RES_REASON_SOURCE_FAULT, which the worker's settle reads as
// built (dnv-worker.md HL2), since the dm-clone is absent because this node
// does not read its source as ready; with no reason, though, while the clone
// meta group is not ready (grpReady) or the source's head is pending
// (headPending), since each is a build in progress the source's text must
// not hide from the settle; detailsCloneMetaGrpNotReady beside a ready source
// while the group is not, and detailsCloneDstNotReady beside a ready source
// and group while the destination raid0 is not (dstReady), each with no
// reason, as a MISSING with no details has none, the build's gate holding
// the dm-clone back (cloneDmGate) — and OK with the raw status line while
// the dm-clone serves
// (cloneDmRead.serves), and ERROR naming what it lacks (cloneDmRead.fault),
// followed by the line, when it does not: its metadata mode, then
// detailsSuspended for one suspended, then hydration off. The raw line rides
// in the details whenever the read took it, because the gateway's hydration
// checks read it (architecture.md, Live-state reporting).
func cloneDmRow(
	cp *clonePlan,
	src cloneSource,
	grpReady bool,
	dstReady bool,
	dm cloneDmRead,
) (pb.ResStatus, string, pb.ResReason) {
	const none = pb.ResReason_RES_REASON_NONE
	switch {
	case dm.err != nil:
		return pb.ResStatus_RES_STATUS_ERROR, dm.err.Error(), none
	case !dm.present:
		if !src.ready(cp) {
			reason := none
			if grpReady && !src.headPending {
				reason = pb.ResReason_RES_REASON_SOURCE_FAULT
			}
			return pb.ResStatus_RES_STATUS_MISSING, src.unready(), reason
		}
		if !grpReady {
			return pb.ResStatus_RES_STATUS_MISSING,
				detailsCloneMetaGrpNotReady, none
		}
		if !dstReady {
			return pb.ResStatus_RES_STATUS_MISSING,
				detailsCloneDstNotReady, none
		}
		return pb.ResStatus_RES_STATUS_MISSING, "", none
	}
	if fault := dm.fault(); fault != "" {
		return pb.ResStatus_RES_STATUS_ERROR,
			agent.CloneStatusDetails(fault, dm.raw), none
	}
	return pb.ResStatus_RES_STATUS_OK, dm.raw, none
}

// cloneGrpReady reports whether the clone meta group of cp is ready for the
// dm-clone (CN18 step 2): its row of this pass, which the build and the probe
// both write before the clone's rows (CN12, CN28), reads OK. A running array
// reads OK, degraded or not, unless a read of it failed, another array also
// holds legs of the group (MdWalk.Match), it holds a foreign member, CN12's
// in-sync guard keeps a member the group no longer keeps from being failed,
// on a converge and a read alike (inSyncGuard), or, on a converge, its member
// reconciliation failed otherwise (ensureGroup, probeGroup), each of which
// writes the row ERROR; an array that is not assembled or not
// running, and a RedundNone group whose device is not the dm-linear the group
// wants, do not read OK either.
func cloneGrpReady(cp *clonePlan, info *pb.CntlrInfo) bool {
	return cp.grp != nil && info.GetGrpIdToMdRaid()[cp.grp.grpId].
		GetStatus() == pb.ResStatus_RES_STATUS_OK
}

// cloneRows writes the two rows of a clone whose destination td resolves,
// that carries a clone meta group and that is not deferred, from one read of
// its source (src), the group's and the destination raid0's rows of the pass
// (cloneGrpReady, cloneDstReady) and one read of its dm-clone (readDm, called
// exactly once). It is the one function the
// converge (ensureClone, wherever its pass ends) and the probe (probeCntlr)
// decide those rows by, so the two channels report the same status for the
// same node, and the rows' epochs do not move between them (CN18, CN28): two
// reads of one device that disagree cannot both be right. The target row is
// cloneTargetRow's and the dm row cloneDmRow's.
//
// note is the failure of the converge step that stopped the build: step 3's
// converge of the dm-clone (ensureDmClone) or the enable, each with the
// listings and reads it makes itself. It decides no status, and it leads the
// details of the dm row whenever that row is not OK (CN29). Every other call
// passes "": the probe's, and a converge's that step 1, step 2 or a read
// ended. A failed connect rides in src instead (cloneTargetRow), and a readDm
// that does not answer is a read the row takes itself (cloneDmRow). What
// backs a namespace's ns-dev is not decided here (nsDevNow).
func (s *CnAgentServer) cloneRows(
	st *cntlrState,
	cp *clonePlan,
	info *pb.CntlrInfo,
	src cloneSource,
	readDm func() cloneDmRead,
	note string,
) {
	t := st.tracker
	status, details, reason := cloneTargetRow(cp, src)
	info.CloneIdToTarget[cp.cloneId] = t.SetReason(
		resKeyOf(resKeyCloneTgtFmt, cp.cloneId), cp.clone.GetSrcNqn(),
		status, details, reason)
	status, details, reason = cloneDmRow(cp, src, cloneGrpReady(cp, info),
		cloneDstReady(cp, info), readDm())
	if status != pb.ResStatus_RES_STATUS_OK {
		details = withNote(note, details)
	}
	info.CloneIdToDmClone[cp.cloneId] = t.SetReason(
		resKeyOf(resKeyCloneDmFmt, cp.cloneId), cp.finalName, status, details,
		reason)
}

// ensureClone runs the CN18 sequence for one clone. A pass stops at the first
// of these: the source is not ready, the build's gate (cloneDmGate) does not
// pass — the clone meta group's or the destination raid0's row is not OK, or
// the read of the dm-clone before step 3 did not answer or shows its
// metadata read-only or failed — a later step fails — step 3's converge of
// the dm-clone or the enable — or the read after the build does not show a
// dm-clone that serves: resumed, hydration on and its metadata read-write.
// Nothing here runs the converge again: a cntlr's read names each source
// endpoint a converge would connect (cloneSource.toName) and the dm-clone
// while a converge would change it (cloneDmName; CN18, CN30), and the
// worker re-sends the request on that name (dnv-worker.md RW4). A read that
// did not answer, a source whose head has not come, a group or a raid0 not
// ready and a metadata mode no converge changes name nothing for the clone.
// A failed knob message or source chunk stops nothing and is only logged.
// CN16 serves the destination td through the dm-clone only while it serves
// (rule 5, nsDevNow), so a build that stops short of step 5 leaves the td
// parked, and its namespaces inaccessible, until a later pass finishes it.
//
// The clone's rows are not the steps' own. Wherever the pass ends, cloneRows
// writes them from what it then reads of the node, the function the cntlr's
// read (refreshCntlr) writes them by too, and the failure of a step that
// ended the pass only rides in their details (CN29). The reads of the
// dm-clone before step 3 and after step 5 are reads, not steps: when one of
// them ends the pass, the rows take that read themselves, and no note rides
// with it. A listing or a read inside ensureDmClone or enableHydration is
// part of that step, and its error rides as the step's note. Step 3's
// create, reload or resume, or the enable's message, that fails names the
// dm-clone in the converge's own verdict (clonePlan.dmName), as a read names
// a dm-clone those steps would change (cloneDmName); a failed knob message
// is only logged, and a read inside them that did not answer is left to the
// next read. budget is the pass's one CN10 wait
// budget, the one its legs drew on.
func (s *CnAgentServer) ensureClone(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	cp *clonePlan,
	info *pb.CntlrInfo,
	budget *agent.WaitBudget,
) {
	if cp.dstTd == nil {
		s.reportCloneNoDst(st, cp, info)
		return
	}
	if cp.grp == nil {
		s.reportCloneNoGrp(st, cp, info)
		return
	}
	if cp.deferred {
		// [D15]: the destination td's raid0 or the clone meta group's device
		// (its md array, or its CnGrpName without redundancy) does not
		// exist, so there is nothing to clone onto or to keep the metadata
		// on — no dm-clone and no source connection.
		s.reportCloneDeferred(st, cp, info)
		return
	}
	// fresh reads the dm-clone for the rows of a pass a step ends, after
	// whatever that step did to the device.
	fresh := func() cloneDmRead { return s.probeCloneDm(ctx, nil, cp) }

	// (1) the source connection: one controller per source cntlr, all in one
	// subsystem, so the source's own ANA picks the serving path. A source
	// that is not ready ends the pass; a dm-clone an earlier pass built may
	// still serve, and its row says so.
	srcDev, src, ok := s.ensureCloneSource(ctx, plan, cp, budget)
	if !ok {
		s.cloneRows(st, cp, info, src, fresh, "")
		return
	}

	// (2) what the dm-clone is built over and the dm-clone as the build finds
	// it, which decide steps 3 to 5 through the gate a cntlr's read names the
	// dm-clone by too (cloneDmGate). The clone meta group's device, which
	// CN12 built earlier in this pass, and the destination td's raid0, which
	// CN15 did, each ready when its row of this pass reads OK: no dm-clone is
	// created over a group that is not assembled, nor over a running array
	// whose member reconciliation failed this pass, nor into a raid0 that is
	// not the stripe the td wants or that is suspended, and one an earlier
	// pass built is left as it is, its row read from its own status. Each
	// row says what is wrong with its device. A read of the dm-clone that
	// fails or does not answer, of a dm-clone that is there, decides nothing
	// either: it builds and enables nothing, and CN16 reads the status again
	// and moves no ns-dev onto a dm-clone it has not seen serve (nsDevNow). A
	// dm-clone confirmed gone after the `dmsetup info` found it reads as
	// absent (probeCloneDm), and step 3 creates it again. A dm-clone whose
	// metadata is read-only or failed is left as it is: no chunk lands and no
	// enable takes in that mode, so its row reads ERROR naming the mode, CN16
	// does not serve through it, and nothing here takes it back to read-write
	// mode (cnagent.md, Known limits). A dm-clone that serves serves on,
	// and the group's fault is re-driven as a slice's group's is (CN12): by
	// what a read names of the group.
	dm := s.probeCloneDm(ctx, nil, cp)
	if !cloneDmGate(cp, src, info, dm) {
		s.cloneRows(st, cp, info, src, func() cloneDmRead { return dm }, "")
		return
	}

	// (3) the dm-clone over the group's device, created with hydration off
	// so the source chunks land before a single region is copied. Over a
	// group that already holds the clone's metadata — a new primary, a node
	// reboot, a dm-clone a sweep removed — dm-clone loads it, and the
	// copy goes on from where it was. A failure here may still leave a
	// dm-clone behind with hydration off — a `dmsetup create` killed after
	// its ioctl ran — which CN16 does not serve through, and which a later
	// pass finishes (step 4).
	build, err := s.ensureDmClone(ctx, plan, cp, srcDev, dm)
	if err != nil {
		s.cloneRows(st, cp, info, src, fresh, err.Error())
		return
	}
	if knobErr := s.ensureHydrationKnobs(ctx, cp); knobErr != nil {
		slog.ErrorContext(ctx, "applying dm-clone hydration knobs failed",
			slog.String("dm", cp.finalName),
			slog.String("error", knobErr.Error()))
	}

	// (4) every locally present source chunk, on a pass whose step 3
	// created, resumed or reloaded the dm-clone, or that found it with
	// hydration off (cloneDmBuild.acts):
	// a build that stopped between steps 3 and 5 left it so, and the chunks
	// it held may not all have landed. Re-applying is idempotent: a chunk
	// marks hydrated only a region not copied yet whose source is unmapped,
	// and dm-clone ignores a discard of a region it has copied. A failed
	// chunk costs an extra copy only, so it is logged.
	if build.acts() {
		s.applyCloneChunks(ctx, st, plan, cp)
	}

	// (5) hydration is enabled only after step 4. When the enable fails,
	// hydration may still be off: CN16 then keeps the td parked, and a later
	// pass finishes the build.
	if sent, err := s.enableHydration(ctx, cp); err != nil {
		if sent {
			cp.dmName = rowFailure(errRowDiffers, err.Error())
		}
		s.cloneRows(st, cp, info, src, fresh, err.Error())
		return
	}
	// The read after step 5 is the rows' (architecture.md, Live-state
	// reporting: the raw status line carries hydration progress, which is
	// what DeleteClone's force check reads). One that does not show a
	// dm-clone that serves — it did not answer, the device went, the enable
	// did not take, or its metadata left read-write mode — leaves the build
	// unfinished, and the next read names the dm-clone where a converge
	// would change it.
	dm = s.probeCloneDm(ctx, nil, cp)
	s.cloneRows(st, cp, info, src, func() cloneDmRead { return dm }, "")
	// (6) the dst td's ns-devs move onto the dm-clone in CN16, which runs
	// after clones in the CN9 build order and reads the status once more
	// before it does (nsDevNow); their namespaces turn optimized in the same
	// pass, after the reload (build).
}

// cloneDstReady reports whether the destination td's raid0 is ready for the
// dm-clone (CN18 step 2): its row of this pass, which the build and the probe
// both write before the clone's rows (CN15, CN28), reads OK — there, the
// stripe the td wants, and resumed on a read; built without a failed step on
// a converge.
func cloneDstReady(cp *clonePlan, info *pb.CntlrInfo) bool {
	return cp.dstTd != nil && info.GetTdIdToRaid0()[cp.dstTd.tdId].
		GetStatus() == pb.ResStatus_RES_STATUS_OK
}

// cloneDmGate is the gate of CN18 steps 3 to 5 for a clone the request builds
// — its destination td resolves, it carries a clone meta group and it is not
// deferred — which the converge builds the dm-clone by (ensureClone) and a
// cntlr's read names it by (cloneDmName): the source reads ready
// (cloneSource.ready), the clone meta group's and the destination raid0's
// rows of the pass read OK (cloneGrpReady, cloneDstReady), and dm, the read
// of the dm-clone, answered and shows its metadata neither read-only nor
// failed. Past the gate, cloneDmBuild decides the steps.
func cloneDmGate(
	cp *clonePlan,
	src cloneSource,
	info *pb.CntlrInfo,
	dm cloneDmRead,
) bool {
	return src.ready(cp) && cloneGrpReady(cp, info) &&
		cloneDstReady(cp, info) && dm.err == nil && !dm.metaBroken()
}

// cloneDmBuild is what CN18 step 3's create, reload or resume and steps 4 and
// 5 do with a clone's dm-clone past the gate (cloneDmGate), the knob messages
// aside, decided from what the pass reads of it: table is the
// dm-clone step 3 wants, shape the dm-clone's against it (dmShapeOf, by its
// target, its size and its four positional arguments), and hydrating whether
// the read of the dm-clone before step 3 shows hydration on. The converge takes its steps
// (ensureDmClone, ensureClone) and a cntlr's read names the dm-clone by it
// (cloneDmName), so the read names the dm-clone exactly while a converge
// would change it.
type cloneDmBuild struct {
	table     string
	shape     dmShape
	hydrating bool
}

// step is step 3's command: a create of a dm-clone that is absent, a reload
// of one whose table is not the one step 3 wants, which loads it with
// hydration off, and a resume of one suspended on the table it wants
// (dmSingleStep). The read-only flag is not read (readCloneDmBuild passes
// dmShapeOf no readOnlyDiffers), so a dm-clone an operator's `blockdev
// --setro` made read-only is never reloaded or made writable
// (dmStepReloadWritable), nor named (CN18 step 3): the flag stops no host
// write, which reaches the dm-clone as a bio remapped through the ns-dev
// above it, and the block layer only warns once on such a bio
// (architecture.md, SpLevel); it refuses only IO through the dm-clone's own
// node, the source chunks' discards among it, whose failure costs only an
// extra copy (step 4).
func (b cloneDmBuild) step() dmStep {
	return dmSingleStep(b.shape)
}

// acts reports whether the build changes the dm-clone: step 3 runs a
// command, or the dm-clone was found with hydration off, which steps 4 and 5
// finish — the source chunks and the enable. Step 4 runs whenever the build
// acts (ensureClone), and step 5 enables hydration wherever it is off, as it
// is on a dm-clone step 3 created or reloaded.
func (b cloneDmBuild) acts() bool {
	return b.step() != dmStepNone || !b.hydrating
}

// name is what a verdict names the dm-clone with while the build acts on it
// (CN30), and nil otherwise: "absent" for one step 3 creates, "differs" with
// what differs for one it reloads or resumes (shapeFailure), and "differs"
// with detailsCloneNotHydrating for one only steps 4 and 5 finish.
func (b cloneDmBuild) name() error {
	switch {
	case !b.acts():
		return nil
	case b.step() != dmStepNone:
		return shapeFailure(b.shape, "clone")
	}
	return rowFailure(errRowDiffers, detailsCloneNotHydrating)
}

// readCloneDmBuild reads what CN18 step 3 decides by, from plan's reading in
// a cntlr's read and from the node's own answers in a converge (plan.rd is
// nil there): the dm-clone's table, whose metadata device is the clone meta
// group's device, named by its number by the rule the pool concats name a
// group by (grpDevNo), its destination the td's raid0 by its number
// (dmDevNo) and its source the source namespace head srcDev by its number in
// /sys/block (blockDevNo); and the dm-clone's shape against that table. Of
// the table only the target, its size and the four positional arguments are
// compared: `dmsetup table` prints the constructor's feature and core
// arguments back, never the live hydration state (`dmsetup status` shows
// that); the table's feature arguments are the create's own fixed pair and
// its core arguments the create's own copy of dm_clone_conf, which no
// request changes after the create, and the live knobs are converged by
// message from the status, so a compare of them would find nothing a reload
// could change (CN18). dm is the read of the dm-clone
// before step 3, whose hydration steps 4 and 5 go by. A geometry of 0 and a
// read that did not answer, a number's among them, are its error, on which
// no step runs.
func (s *CnAgentServer) readCloneDmBuild(
	ctx context.Context,
	plan *cntlrPlan,
	cp *clonePlan,
	srcDev string,
	dm cloneDmRead,
) (cloneDmBuild, error) {
	if cp.regionSectors == 0 || cp.sectors == 0 {
		return cloneDmBuild{}, fmt.Errorf("clone geometry is 0")
	}
	metaNo, err := s.grpDevNo(ctx, plan, cp.grp)
	if err != nil {
		return cloneDmBuild{}, err
	}
	destNo, err := s.dmDevNo(ctx, plan.rd, cp.dstTd.raid0Name)
	if err != nil {
		return cloneDmBuild{}, err
	}
	srcNo, err := s.blockDevNo(ctx, path.Base(srcDev))
	if err != nil {
		return cloneDmBuild{}, err
	}
	conf := cp.clone.GetDmCloneConf()
	// no_discard_passdown is not optional here (CN18 step 3 and [D7]: the
	// table's `2 no_hydration …`): CN22 marks regions hydrated by
	// discarding them, and with passdown enabled dm-clone would remap
	// those discards to the destination raid0 — unmapping any host write
	// that landed on a hydrated region while `auto_resume` let the namespace
	// serve.
	table := agent.CloneTable(cp.sectors, metaNo, destNo, srcNo,
		cp.regionSectors, true, true,
		conf.GetHydrationThreshold(), conf.GetHydrationBatchSize())
	args := []string{metaNo, destNo, srcNo,
		fmt.Sprintf("%d", cp.regionSectors)}
	shape, err := s.dmShapeOf(ctx, plan.rd, cp.finalName, "clone",
		cp.sectors, args, len(args), false)
	if err != nil {
		return cloneDmBuild{}, err
	}
	return cloneDmBuild{table: table, shape: shape, hydrating: dm.hydrating},
		nil
}

// cloneDmName is what a cntlr's read names the clone's dm-clone with (CN18,
// CN30), nil for nothing: the build's name (cloneDmBuild.name) past its gate
// (cloneDmGate), from the read's reading — the dm-clone's info and table, and
// the numbers dmDevNo reads there — and the reads outside it: the source
// head's number from /sys/block and, for an md clone meta group, the array's
// (CN28). src, info and dm are the read's source, rows and dm-clone read.
// A read that did not answer names nothing.
func (s *CnAgentServer) cloneDmName(
	ctx context.Context,
	plan *cntlrPlan,
	cp *clonePlan,
	src cloneSource,
	info *pb.CntlrInfo,
	dm cloneDmRead,
) error {
	if !cloneDmGate(cp, src, info, dm) {
		return nil
	}
	build, err := s.readCloneDmBuild(ctx, plan, cp, src.view.nsDev, dm)
	if err != nil {
		return nil
	}
	return build.name()
}

// ensureCloneSource converges CN18 step 1, connecting every entry of
// src_tr_conf_list that has no controller (srcToConnect, the test a cntlr's
// read names the entries by), and returns the source namespace
// device, the source as the step last read it, and whether the step holds:
// the read is ready and no connect of the step failed. Its connect step is
// CN10's, on the same pass budget the legs drew on (CN18): a failed connect
// is tried again while the budget covers the pause, and after a connect this
// pass made the subsystem is looked up again until the source namespace's
// head is complete (awaitNsHead). The step holds on a head that the budget
// left picked but not complete, since the step reads no number: the number
// is first read in step 3, and a read that fails there fails step 3
// (ensureDmClone). A wait that ended with no head fails the step. A connect
// that fails ends the step, and the subsystem is looked up once more, on a
// fresh walk, so the rows say what that connect left behind. Either read
// marks the source headPending when it finds a controller at every endpoint
// and no head: the step connected only endpoints its lookup found with none,
// so a controller found there after is one a connect made since that lookup
// — the pass's own, a failed one too, whose kill landed after the kernel
// made it (dnagent.md SH15), or, in a race, another cntlr's — whose head may
// be still to come; one whose address did not answer at that lookup, found
// once it answers, is marked the same way, on the side that holds the
// settle. CN10's unknown-controller guard is not applied: a source
// controller whose address did not answer matches no entry, so an entry it
// alone serves is connected again — a duplicate the host refuses while that
// controller lives — and nothing is disconnected, since this step retires
// no path.
//
// Every cntlr of the node may connect a source, and another may have
// connected this one since the pass's walk. So a source the walk holds no
// entry for, or whose entry fails the lookup's subsysnqn check, is looked up
// once more on a fresh walk before the step connects it (CN18), and a source
// that walk finds connected is adopted.
//
// A source a sweep is still disconnecting is neither read, adopted nor
// connected (disconnectInFlight): a dm-clone built on it would lose its
// source when that disconnect lands, and every read of a region not yet
// hydrated would fail until a later converge reloaded the table.
func (s *CnAgentServer) ensureCloneSource(
	ctx context.Context,
	plan *cntlrPlan,
	cp *clonePlan,
	budget *agent.WaitBudget,
) (string, cloneSource, bool) {
	nqn := cp.clone.GetSrcNqn()
	nsIdx := cp.clone.GetSrcNsIdx()
	src := s.readCloneSource(ctx, plan, cp)
	if src.inFlight || src.readErr != nil {
		return "", src, false
	}
	if !src.view.found {
		plan.walk.stale = true
		src = s.readCloneSource(ctx, plan, cp)
		if src.inFlight || src.readErr != nil {
			return "", src, false
		}
	}
	connected := false
	for _, tr := range srcToConnect(cp, src.view) {
		plan.walk.stale = true
		// The default fast IO fail timeout, never the pool's: one source
		// controller serves every clone of its source on this node, of
		// whichever pool (dnagent.md SH20).
		if err := s.connectWithin(ctx, plan, budget, agent.TrConf{
			TrType:  tr.GetTrType(),
			AdrFam:  tr.GetAdrFam(),
			TrAddr:  tr.GetTrAddr(),
			TrSvcId: tr.GetTrSvcId(),
		}, nqn, plan.hostNqn(), common.DefaultNvmeFastIoFailTmo); err != nil {
			src.stepErr = err
			break
		}
		connected = true
	}
	switch {
	case src.stepErr != nil:
		src.view, src.readErr = s.lookupSubsys(ctx, plan.walk, nqn, nsIdx)
		src.headPending = src.headToCome(cp)
		return "", src, false
	case connected:
		src.view, src.readErr = s.awaitNsHead(
			ctx, budget, plan.walk, nqn, nsIdx)
		src.headPending = src.headToCome(cp)
	}
	if !src.ready(cp) {
		return "", src, false
	}
	return src.view.nsDev, src, true
}

// pathStates renders one subsystem's controller states for ResInfo.details.
func pathStates(view *subsysView) string {
	if view == nil {
		return ""
	}
	states := make([]string, 0, len(view.ctrls))
	for _, ctrl := range view.ctrls {
		states = append(states, ctrl.state)
	}
	return "paths: " + strings.Join(states, ",")
}

// ensureDmClone is CN18 step 3: it reads the build (readCloneDmBuild) and
// takes its step (cloneDmBuild.step), and returns the build, which says
// whether steps 4 and 5 act (cloneDmBuild.acts): a pass whose step 3
// created, resumed or reloaded the dm-clone applies the stored chunks again
// (CN22), a resume because a chunk that came while the dm-clone was
// suspended sent no discard. A create, reload or resume that fails names
// the dm-clone in the converge's verdict (clonePlan.dmName); a read that did
// not answer, a number's among them, runs no command and names nothing.
func (s *CnAgentServer) ensureDmClone(
	ctx context.Context,
	plan *cntlrPlan,
	cp *clonePlan,
	srcDev string,
	dm cloneDmRead,
) (cloneDmBuild, error) {
	build, err := s.readCloneDmBuild(ctx, plan, cp, srcDev, dm)
	if err != nil {
		return build, err
	}
	if err := s.takeDmStep(
		ctx, cp.finalName, build.step(), build.table); err != nil {
		cp.dmName = rowFailure(errRowDiffers, err.Error())
		return build, err
	}
	return build, nil
}

// ensureHydrationKnobs applies hydration_threshold / hydration_batch_size,
// probe-first. It never touches the enable flag — that is step 5's job, and
// it must not run before the source chunks are applied.
func (s *CnAgentServer) ensureHydrationKnobs(
	ctx context.Context,
	cp *clonePlan,
) error {
	status, err := s.cloneStatus(ctx, cp)
	if err != nil {
		return err
	}
	conf := cp.clone.GetDmCloneConf()
	if threshold := conf.GetHydrationThreshold(); threshold != 0 &&
		status.Threshold != threshold {
		if err := s.dm.Message(ctx, cp.finalName, 0,
			fmt.Sprintf("hydration_threshold %d", threshold)); err != nil {
			return err
		}
	}
	if batch := conf.GetHydrationBatchSize(); batch != 0 &&
		status.BatchSize != batch {
		if err := s.dm.Message(ctx, cp.finalName, 0,
			fmt.Sprintf("hydration_batch_size %d", batch)); err != nil {
			return err
		}
	}
	return nil
}

// enableHydration is CN18 step 5. Hydration is infrastructure IO, not user IO,
// so no sp_level ever pauses it ([D11]): this only ever enables. It reports
// whether it sent the message: an error with none sent is its status read's.
func (s *CnAgentServer) enableHydration(
	ctx context.Context,
	cp *clonePlan,
) (bool, error) {
	status, err := s.cloneStatus(ctx, cp)
	if err != nil {
		return false, err
	}
	if status.HydrationEnabled {
		return false, nil
	}
	return true, s.dm.Message(ctx, cp.finalName, 0, "enable_hydration")
}

// cloneServes reports whether the live dm-clone serves (cloneDmRead.serves):
// its status shows hydration on, the mark step 5 leaves and nothing before
// it, and its metadata in read-write mode. A status that does not parse as a
// dm-clone's shows neither, and neither does a dm-clone that does not exist.
// Absence is confirmed, never assumed: the status is read first, as on every
// pass, and only when that read fails does a `dmsetup info` ask whether the
// device is there at all, so a failed read of a dm-clone that is there, or
// one whose absence the info did not confirm, stays an error. CN16 reads it
// to decide whether the dm-clone may serve (nsDevNow): a clone whose
// dm-clone is gone, or not built while its clone meta group is not ready,
// parks its td's ns-devs. It reads no suspend bit: the IO of an ns-dev over a
// dm-clone suspended on the table it wants waits in the dm-clone, and a park
// of that ns-dev would wait on it too, its reload's suspend flushing the
// ns-dev under the cntlr's lock until the soft timeout, while CN18 step 3's
// resume is what ends the wait. The clone's rows read the dm-clone through
// probeCloneDm, by the same rule and with the suspend bit.
func (s *CnAgentServer) cloneServes(
	ctx context.Context,
	cp *clonePlan,
) (bool, error) {
	raw, err := s.dm.CloneStatus(ctx, cp.finalName)
	if err != nil {
		if dev, infoErr := s.dm.Info(
			ctx, cp.finalName); infoErr == nil && dev == nil {
			return false, nil
		}
		return false, err
	}
	return cloneDmReadOf(raw).serves(), nil
}

func (s *CnAgentServer) cloneStatus(
	ctx context.Context,
	cp *clonePlan,
) (*common.CloneStatus, error) {
	raw, err := s.dm.CloneStatus(ctx, cp.finalName)
	if err != nil {
		return nil, err
	}
	status, ok := common.ParseCloneStatus(raw)
	if !ok {
		return nil, fmt.Errorf("unparsable dm-clone status %q", raw)
	}
	return status, nil
}

// cloneDmRead is one read of a clone's dm-clone (probeCloneDm): whether the
// device is there, whether its info shows it suspended, its raw status line,
// whether that line shows hydration on, the mark CN18 step 5 leaves, and the
// metadata mode it ends with (one of the common.CloneMetadata modes, "" when
// the line shows none). err is a read that did not answer, of a dm-clone
// that is there or whose absence the read could not confirm.
type cloneDmRead struct {
	present   bool
	suspended bool
	raw       string
	hydrating bool
	mode      string
	err       error
}

// cloneDmReadOf reads one status line of a dm-clone that is there: the line
// common.ParseCloneStatus reads, or the line of a dm-clone in fail mode
// (common.CloneFailed). Any other line shows no hydration and no mode. The
// suspend bit is the info's, which probeCloneDm sets beside it.
func cloneDmReadOf(raw string) cloneDmRead {
	read := cloneDmRead{present: true, raw: raw}
	if status, ok := common.ParseCloneStatus(raw); ok {
		read.hydrating = status.HydrationEnabled
		read.mode = status.MetadataMode
	} else if common.CloneFailed(raw) {
		read.mode = common.CloneMetadataFail
	}
	return read
}

// metaBroken reports whether the read shows the dm-clone's metadata in
// read-only or fail mode, which dm-clone enters when an operation on its
// metadata device fails, and which no step of the build changes (CN18).
func (dm cloneDmRead) metaBroken() bool {
	return dm.mode == common.CloneMetadataReadOnly ||
		dm.mode == common.CloneMetadataFail
}

// fault is what keeps a dm-clone that is there from serving, "" when
// nothing does: its metadata mode first, since no enable takes while it is
// read-only or failed and no step of the build changes it, then a dm-clone
// suspended (detailsSuspended), which serves no IO until CN18 step 3 resumes
// it, as a suspended pool, concat or raid0 reads (CN28), then hydration off,
// then a line that shows hydration on and no read-write mode.
func (dm cloneDmRead) fault() string {
	switch {
	case dm.mode == common.CloneMetadataReadOnly:
		return detailsCloneMetaReadOnly
	case dm.mode == common.CloneMetadataFail:
		return detailsCloneMetaFailed
	case dm.suspended:
		return detailsSuspended
	case !dm.hydrating:
		return detailsCloneNotHydrating
	case dm.mode != common.CloneMetadataReadWrite:
		return detailsCloneMetaModeUnknown
	}
	return ""
}

// serves reports whether the read shows a dm-clone that serves: there,
// resumed, with hydration on and its metadata read-write. CN16 goes by the
// status alone (cloneServes), which reads no suspend bit.
func (dm cloneDmRead) serves() bool {
	return dm.err == nil && dm.present && dm.fault() == ""
}

// probeCloneDm reads a clone's dm-clone, for the build's reads and for the
// clone's rows (cloneRows): `dmsetup info`, whose attr flags give the suspend
// bit, then `dmsetup status` of a device that is there. The info comes from
// rd, the reading of a cntlr's read, and from one `dmsetup info` of the
// device in a converge, which passes nil. The status is always the device's
// own. The raw status line rides into
// the dm row's details (architecture.md, Live-state reporting), and a status
// that does not parse as a dm-clone's shows no hydration (cloneDmReadOf).
// Absence is confirmed, never assumed, by cloneServes's rule: after a status
// read that fails, a fresh `dmsetup info` of the device, never the reading's,
// that finds it gone makes the read an absence, and anything else keeps it an
// error. The status read commits the clone's metadata only once the copy
// reads whole (Dm.CloneStatus; dnagent.md SH17).
func (s *CnAgentServer) probeCloneDm(
	ctx context.Context,
	rd *cnReading,
	cp *clonePlan,
) cloneDmRead {
	dev, err := s.dmInfo(ctx, rd, cp.finalName)
	if err != nil {
		return cloneDmRead{err: err}
	}
	if dev == nil {
		return cloneDmRead{}
	}
	raw, err := s.dm.CloneStatus(ctx, cp.finalName)
	if err != nil {
		if gone, infoErr := s.dm.Info(
			ctx, cp.finalName); infoErr == nil && gone == nil {
			return cloneDmRead{}
		}
		return cloneDmRead{err: err}
	}
	read := cloneDmReadOf(raw)
	read.suspended = dev.Suspended
	return read
}
