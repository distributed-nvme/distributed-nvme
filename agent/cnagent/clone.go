package cnagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// CN18 — clones (fig. `090Clone`), primary only and only below
// SP_LEVEL_NO_CLONE.

// detailsCloneMetaMissing is what `clone_id_to_dm_clone` reports, ERROR, when
// CN18 step 2's verdict finds that no metadata slot can be supplied at all
// (cloneMetaVerdict): the arena did not answer, or the run is larger than the
// whole arena. With no slot to map, the dm-clone is not merely absent, it
// cannot be built, which is why the row says it even where a dm-clone from an
// earlier pass is still up; that dm-clone's status line follows on a line of
// its own (reportCloneMetaErr).
const detailsCloneMetaMissing = "metadata wrapper missing"

// detailsCloneNotHydrating is what `clone_id_to_dm_clone` reports, ERROR and
// followed by the raw status line, for a dm-clone that is up and whose status
// does not show hydration on: a build or a recovery that stopped short of
// CN18 step 5, or a device under the clone's name that is not a dm-clone.
// CN16 does not serve through it (nsDevNow).
const detailsCloneNotHydrating = "hydration not enabled"

// reportCloneMetaErr fills the two rows of a clone whose CN18 step 2 verdict is
// an ERROR (cloneMetaVerdict): `clone_id_to_meta` ERROR with the verdict's own
// text and `clone_id_to_dm_clone` ERROR detailsCloneMetaMissing, followed by
// raw, the status line of a dm-clone that is still up, when the pass read one.
// note is a converge's failure, which leads both rows' details (cloneRows).
func (s *CnAgentServer) reportCloneMetaErr(
	st *cntlrState,
	cp *clonePlan,
	info *pb.CntlrInfo,
	details string,
	note string,
	raw string,
) {
	info.CloneIdToMeta[cp.cloneId] = st.tracker.Err(
		resKeyOf(resKeyCloneMetaFmt, cp.cloneId), cp.metaDmName,
		withNote(note, details))
	info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
		resKeyOf(resKeyCloneDmFmt, cp.cloneId), cp.finalName,
		agent.CloneStatusDetails(withNote(note, detailsCloneMetaMissing), raw))
}

// reportCloneNoRoom fills the two rows of a clone the arena has no free run
// for (CN18 step 2): `clone_id_to_meta` and `clone_id_to_dm_clone` both
// RES_STATUS_PROVISIONING, both carrying detailsWaitingForRoom of the
// allocator's error, and the dm row followed by raw, the status line of a
// dm-clone that still serves from an earlier pass, when the pass read one:
// the gateway's hydration checks read that line whatever the row's status
// (architecture.md, Live-state reporting). The clone is not broken, it waits
// for room, and PROVISIONING sets no health epoch. note is a converge's
// failure, which leads both rows' details (cloneRows).
func (s *CnAgentServer) reportCloneNoRoom(
	st *cntlrState,
	cp *clonePlan,
	info *pb.CntlrInfo,
	details string,
	note string,
	raw string,
) {
	details = withNote(note, details)
	info.CloneIdToMeta[cp.cloneId] = st.tracker.Provisioning(
		resKeyOf(resKeyCloneMetaFmt, cp.cloneId), cp.metaDmName, details)
	info.CloneIdToDmClone[cp.cloneId] = st.tracker.Provisioning(
		resKeyOf(resKeyCloneDmFmt, cp.cloneId), cp.finalName,
		agent.CloneStatusDetails(details, raw))
}

// reportCloneNoDst fills the three rows of a clone whose dst_td_id is not in
// td_list, ERROR on each, shared by the converge and the probe (CN18): there
// is nothing to clone onto, so nothing of the clone is read or built, and no
// metadata budget is made up from a zero region count.
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
	info.CloneIdToMeta[cp.cloneId] = st.tracker.Err(
		resKeyOf(resKeyCloneMetaFmt, cp.cloneId), cp.metaDmName, details)
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
// decide clone_id_to_target by (cloneTargetRow), and what gates CN18 step
// 2's verdict (ready).
type cloneSource struct {
	// inFlight: a sweep's disconnect of the source NQN is in flight
	// (disconnectInFlight), and the walk does not run.
	inFlight bool
	// view is the subsystem walk's last answer; nil when the walk did not
	// answer or did not run.
	view *subsysView
	// readErr is the walk that did not answer (errSubsysUnknown).
	readErr error
	// stepErr is a connect of the converge's CN18 step 1 that failed. The
	// probe connects nothing and never has one.
	stepErr error
}

// readCloneSource reads the source the way ensureCloneSource starts: the CN10
// disconnect registry first, and the subsystem walk only when no sweep's
// disconnect of the source is in flight.
func (s *CnAgentServer) readCloneSource(
	ctx context.Context,
	cp *clonePlan,
) cloneSource {
	if s.disconnectInFlight(cp.clone.GetSrcNqn()) {
		return cloneSource{inFlight: true}
	}
	view, err := s.readSubsys(ctx, cp.clone.GetSrcNqn(),
		cp.clone.GetSrcNsIdx())
	return cloneSource{view: view, readErr: err}
}

// ready reports whether CN18 step 1 holds on this read: no sweep's disconnect
// of the source is in flight, the walk answered, every configured endpoint
// has a controller, whatever its state, since the converge connects only the
// ones that are missing, and the source namespace device is there. It is not
// cloneSourceLive, which answers the target row; this answers "did the
// converge get past step 1", which is what makes a step 2 verdict comparable
// between the two channels at all. A read that still needs a connect is not
// ready: whether that connect would succeed is exactly what a read-only probe
// may not find out (CN23). A failed connect of the converge's is not asked:
// the probe never has one, and ensureCloneSource stops step 1 on it itself.
func (src cloneSource) ready(cp *clonePlan) bool {
	view := src.view
	if src.inFlight || src.readErr != nil || view == nil || !view.found ||
		view.nsDev == "" {
		return false
	}
	for _, tr := range cp.clone.GetSrcTrConfList() {
		if view.ctrlOf(tr.GetTrAddr(), tr.GetTrSvcId()) == nil {
			return false
		}
	}
	return true
}

// unready is the text of clone_id_to_dm_clone MISSING beside a source that is
// not ready: unknown when the walk did not answer (CN10), since the pass does
// not know whether the source is connected, and not connected otherwise.
func (src cloneSource) unready() string {
	if errors.Is(src.readErr, errSubsysUnknown) {
		return "source unknown: " + src.readErr.Error()
	}
	return "source not connected"
}

// cloneTargetRow is the clone_id_to_target row of one read of the source
// (CN28), the same for the converge and the probe. It reads ERROR only for
// this node's own reading: while a sweep's disconnect of the source is in
// flight, or when the walk did not answer. A fault of the source reads
// MISSING, as a source with no subsystem entry does: a configured endpoint
// with no live controller, its details the path states, or no source
// namespace. A MISSING row sets no health epoch, so the row does not move
// the role: moving the primary cannot repair the source, and the worker
// cannot tell a fault in this node's own path to the source from one at the
// source (CN18). A fault that fails CN18 step 1 still keeps the CN10 retry
// running, whose attempts can make check rounds miss (cnagent.md, Known
// limits). The row reads OK, with the path states, while every endpoint has
// a live controller and the namespace is there. A failed connect of the
// converge's decides nothing and leads the details of a row that is not OK.
func cloneTargetRow(cp *clonePlan, src cloneSource) (pb.ResStatus, string) {
	nqn := cp.clone.GetSrcNqn()
	note := ""
	if src.stepErr != nil {
		note = src.stepErr.Error()
	}
	view := src.view
	switch {
	case src.inFlight:
		return pb.ResStatus_RES_STATUS_ERROR, detailsDisconnectInFlight(nqn)
	case src.readErr != nil:
		return pb.ResStatus_RES_STATUS_ERROR,
			withNote(note, src.readErr.Error())
	case view == nil || !view.found:
		return pb.ResStatus_RES_STATUS_MISSING, note
	case !cloneSourceLive(view, cp):
		return pb.ResStatus_RES_STATUS_MISSING,
			withNote(note, pathStates(view))
	case view.nsDev == "":
		return pb.ResStatus_RES_STATUS_MISSING, withNote(note, fmt.Sprintf(
			"no namespace %d on %s", cp.clone.GetSrcNsIdx(), nqn))
	}
	return pb.ResStatus_RES_STATUS_OK, pathStates(view)
}

// cloneDmRow is the clone_id_to_dm_clone row of one read of the dm-clone
// (CN28), the same for the converge and the probe when CN18 step 2's verdict
// does not decide it (cloneRows): ERROR when the read did not answer, MISSING
// when the device is not there — saying why, unready, while the source is
// not ready — OK with the raw status line while that line shows hydration on,
// and ERROR detailsCloneNotHydrating followed by the line when it does not.
// The raw line rides in the details whenever the read took it, because the
// gateway's hydration checks read it (architecture.md, Live-state
// reporting).
func cloneDmRow(
	cp *clonePlan,
	src cloneSource,
	dm cloneDmRead,
) (pb.ResStatus, string) {
	switch {
	case dm.err != nil:
		return pb.ResStatus_RES_STATUS_ERROR, dm.err.Error()
	case !dm.present:
		if src.ready(cp) {
			return pb.ResStatus_RES_STATUS_MISSING, ""
		}
		return pb.ResStatus_RES_STATUS_MISSING, src.unready()
	case dm.hydrating:
		return pb.ResStatus_RES_STATUS_OK, dm.raw
	}
	return pb.ResStatus_RES_STATUS_ERROR,
		agent.CloneStatusDetails(detailsCloneNotHydrating, dm.raw)
}

// cloneRows writes the three rows of a clone whose destination td resolves
// and is not deferred, from one read of its source (src) and one of its
// dm-clone (readDm, called exactly once). It is the one function the
// converge (ensureClone, wherever its pass ends) and the probe (probeCntlr)
// decide those rows by, so the two channels report the same status for the
// same node, and the rows' epochs do not move between them (CN18, CN28): two
// readings of one device cannot both be right.
//
// The target row is cloneTargetRow's. For a source that is ready, CN18 step
// 2's verdict (cloneMetaVerdict) comes first: when it decides, the meta row
// and the dm row are its pair, PROVISIONING waiting for room
// (reportCloneNoRoom) or the ERROR pair (reportCloneMetaErr), the dm row
// followed by the status line of a dm-clone that is still up. A source that
// is not ready gets no verdict: the converge stops at step 1 and never asks
// whether the arena could supply a slot. Otherwise the dm row is cloneDmRow's
// and the meta row is the wrapper's own (probeCloneMeta).
//
// note is the failure of the converge step that stopped the build: the
// wrapper's allocation or replacement (ensureCloneMeta), step 3's converge
// of the dm-clone (ensureDmClone), step 4's destination bitmaps or the
// enable, each with the listings and reads it makes itself. It decides no
// status, and it leads the details of the dm row and the meta row whenever
// they are not OK (CN29). Every other call passes "": the probe's, and a
// converge's that step 1, the verdict or a reading ended. A failed connect
// rides in src instead (cloneTargetRow), and the pass's arena listing
// (planArena) or a readDm that does not answer is a reading the rows take
// themselves: the listing's error is the meta row's own details, and the
// read's error the dm row's where the verdict does not decide that row
// (cloneDmRow). What backs a namespace's ns-dev is not decided here
// (nsDevNow).
func (s *CnAgentServer) cloneRows(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	cp *clonePlan,
	info *pb.CntlrInfo,
	src cloneSource,
	readDm func() cloneDmRead,
	note string,
) {
	t := st.tracker
	status, details := cloneTargetRow(cp, src)
	info.CloneIdToTarget[cp.cloneId] = t.Set(
		resKeyOf(resKeyCloneTgtFmt, cp.cloneId), cp.clone.GetSrcNqn(),
		status, details)
	if src.ready(cp) {
		if status, details, decided := s.cloneMetaVerdict(
			ctx, plan, cp); decided {
			raw := readDm().rawLine()
			if status == pb.ResStatus_RES_STATUS_PROVISIONING {
				s.reportCloneNoRoom(st, cp, info, details, note, raw)
			} else {
				s.reportCloneMetaErr(st, cp, info, details, note, raw)
			}
			return
		}
	}
	status, details = cloneDmRow(cp, src, readDm())
	if status != pb.ResStatus_RES_STATUS_OK {
		details = withNote(note, details)
	}
	info.CloneIdToDmClone[cp.cloneId] = t.Set(
		resKeyOf(resKeyCloneDmFmt, cp.cloneId), cp.finalName, status, details)
	status, details = s.probeCloneMeta(ctx, plan, cp)
	if status != pb.ResStatus_RES_STATUS_OK {
		details = withNote(note, details)
	}
	info.CloneIdToMeta[cp.cloneId] = t.Set(
		resKeyOf(resKeyCloneMetaFmt, cp.cloneId), cp.metaDmName,
		status, details)
}

// ensureClone runs the CN18 sequence for one clone and reports which
// background retry the cntlr needs, and whether the build is finished.
// connectRetry asks for the CN10 retry: the source is not ready, the arena
// did not answer, a later step stopped the build before step 5 turned
// hydration on — step 2's wrapper allocation or replacement (ensureCloneMeta)
// among them, unless the arena had no free run — or the read after step 5
// did not show hydration on. waitRoom asks for CN18's slow retry
// (startCloneMetaRetry, which ensureClone registers itself, as it does the
// CN10 one): the arena answered and has no free run for the clone's
// metadata, so both of its step-2 rows read PROVISIONING (reportCloneNoRoom)
// and the clone waits for another one to leave the node. A run larger than
// the whole arena asks for neither and is left to its ERROR rows, which every
// check round reports again (cloneMetaVerdict). finished says the pass read
// the dm-clone hydrating after step 5; when a pass that finished the build
// fails to converge a rule-5 ns-dev of the td, CN16 registers the CN10 retry
// itself (build). A failed knob message or source chunk stops nothing and is
// only logged. CN16 serves the destination td through the dm-clone only
// while its status shows hydration on (rule 5, nsDevNow), and step 5 alone
// turns that on, so a build or a recovery that stops short of step 5 leaves
// the td parked until a later pass finishes it.
//
// The clone's rows are not the steps' own. Wherever the pass ends, cloneRows
// writes them from what it then reads of the node, the function the check
// round writes them by too, and the failure of a step that ended the pass
// only rides in their details (CN29). The pass's arena listing (planArena),
// step 2's read of the dm-clone and the read after step 5 are readings, not
// steps: when one of them ends the pass, the rows take that reading
// themselves, and no note rides with it. A listing or a read inside
// ensureCloneMeta, ensureDmClone, applyDstBitmaps or enableHydration is part
// of that step, and its error rides as the step's note. budget is the pass's
// one CN10 wait budget, the one its legs drew on.
func (s *CnAgentServer) ensureClone(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	cp *clonePlan,
	info *pb.CntlrInfo,
	budget *agent.WaitBudget,
) (connectRetry bool, waitRoom bool, finished bool) {
	if cp.dstTd == nil {
		s.reportCloneNoDst(st, cp, info)
		return false, false, false
	}
	if cp.deferred {
		// [D15]: the destination td's raid0 does not exist yet, so there is
		// nothing to clone onto — no metadata slot, no dm-clone and no source
		// connection.
		s.reportCloneDeferred(st, cp, info)
		return false, false, false
	}
	// fresh reads the dm-clone for the rows of a pass a step ends, after
	// whatever that step did to the device.
	fresh := func() cloneDmRead { return s.probeCloneDm(ctx, cp) }

	// (1) the source connection: one controller per source cntlr, all in one
	// subsystem, so the source's own ANA picks the serving path. A source
	// that is not ready ends the pass; a dm-clone an earlier pass built may
	// still serve, and its row says so.
	srcDev, src, ok := s.ensureCloneSource(ctx, plan, cp, budget)
	if !ok {
		s.cloneRows(ctx, st, plan, cp, info, src, fresh, "")
		s.startConnectRetry(st, plan)
		return true, false, false
	}

	// (2) the dm-clone's metadata. This build is a recovery (its triggers are
	// cnagent.md CN18's) whenever
	// that metadata does not survive — the volatile clone-metadata arena is
	// gone (CN reboot, failover to a CN that never ran the clone, tmpfs loss),
	// or the clone has never been built at all. The metadata **wrapper** alone
	// is not the test: a pass that created the wrapper and then failed to
	// build the dm-clone (an agent restart, a raid0 not up yet) leaves a
	// freshly discarded slot that dm-clone would format fresh, with nothing
	// hydrated — so an absent dm-clone device is a recovery too. Nor is a
	// dm-clone that is up: a pass killed between step 3 and step 5 leaves one
	// with hydration still off and step 4 perhaps unfinished, and so can a
	// pass that failed there without removing it; enabling hydration on it
	// would re-fetch every region the destination already owns. Only step 5
	// turns hydration on, so a dm-clone whose status does not show it on is
	// an unfinished build and gets the whole recovery again — re-applying the
	// bitmaps is idempotent.
	// A read that fails or does not answer, of a dm-clone that is there,
	// decides nothing: it ends the pass, removes and enables nothing, and
	// registers the retry; CN16 reads the status again and moves no ns-dev
	// onto a dm-clone it has not seen hydrating (nsDevNow). A dm-clone
	// confirmed gone after the `dmsetup info` found it reads as absent
	// (probeCloneDm), and the recovery runs.
	// On a first build the destination td is empty by contract ([D3]), so
	// the recovery's read (architecture.md, Clone crash recovery) costs one
	// metadata snapshot and discards nothing.
	arena, err := s.planArena(ctx, plan)
	if err != nil {
		// cloneRows writes the verdict's ERROR pair for an arena that did
		// not answer (cloneMetaVerdict).
		s.cloneRows(ctx, st, plan, cp, info, src, fresh, "")
		s.startConnectRetry(st, plan)
		return true, false, false
	}
	metaOk := s.cloneMetaConverged(ctx, arena, cp)
	// A build that must allocate or replace the wrapper first asks whether
	// the arena can supply a fresh run at all, counting the clone's own
	// mismatched run as free, and asks it before the dm-clone is parked or
	// removed (architecture.md, Clone crash recovery): parking a namespace
	// that may still serve through the old dm-clone, for a run that will not
	// come, is an outage for nothing. A clone the arena has no free run for
	// reads PROVISIONING on both rows and waits for CN18's slow retry; one
	// whose run can never fit reads the ERROR pair and waits for nothing. The
	// verdict is the probe's own (cloneMetaVerdict), and cloneRows writes its
	// pair for both channels.
	if !metaOk {
		if status, _, decided := s.cloneMetaVerdict(
			ctx, plan, cp); decided {
			s.cloneRows(ctx, st, plan, cp, info, src, fresh, "")
			if status == pb.ResStatus_RES_STATUS_PROVISIONING {
				s.startCloneMetaRetry(st, plan)
				return false, true, false
			}
			return false, false, false
		}
	}
	dm := s.probeCloneDm(ctx, cp)
	if dm.err != nil {
		s.cloneRows(ctx, st, plan, cp, info, src,
			func() cloneDmRead { return dm }, "")
		s.startConnectRetry(st, plan)
		return true, false, false
	}
	recovery := !metaOk || !dm.present || !dm.hydrating
	if recovery {
		// The park of the recovery build (cnagent.md CN18) comes first, for
		// the invariant of architecture.md, Clone crash recovery:
		// nothing may serve the td while the
		// destination bitmaps are still being applied, or a read of an
		// already-copied (and possibly since-rewritten) region would be
		// fetched from the source again, returning stale data over the newer
		// local bytes.
		s.parkTdNsDevs(ctx, plan, cp.dstTd.tdId)
		s.removeDm(ctx, cp.finalName)
		// Only now, once the dm-clone is gone, may a mismatched wrapper be
		// replaced (a dm-clone whose removal failed still maps it, and
		// ensureCloneMeta's removal of it then fails EBUSY and ends this
		// step) — and a *matching* one is left strictly alone: allocating is
		// what hole-punches the slot, and re-punching a live slot would wipe
		// a valid dm-clone superblock (CN18).
		if !metaOk {
			if err := s.ensureCloneMeta(ctx, plan, cp); err != nil {
				if isCloneMetaNoRun(err) {
					// The run was free when the verdict above read this
					// pass's enumeration, and another cntlr of the node took
					// it before ensureCloneMeta's own enumeration, which
					// replaces the pass's: the td is parked and the old
					// dm-clone is gone, and the clone waits for room, the
					// verdict on that enumeration saying so in its rows.
					s.cloneRows(ctx, st, plan, cp, info, src, fresh, "")
					s.startCloneMetaRetry(st, plan)
					return false, true, false
				}
				// Any other failure — the fresh listing, the removal of a
				// mismatched wrapper, the hole punch or the wrapper's
				// `dmsetup create` — registers the CN10 retry: a check round
				// cannot tell an absent wrapper in an arena with room from
				// one not built yet (cloneMetaVerdict), so once the dm-clone
				// is gone it reads the clone's rows MISSING and the parked
				// ns-dev OK (nsDevNow), no health epoch follows, and the
				// worker re-syncs on a revision or a reply code, never on a
				// row, so nothing else would run this pass again. The rows
				// read the arena afresh: a `dmsetup create` killed after its
				// ioctl ran leaves a wrapper the pass's enumeration does not
				// show.
				plan.arenaDone = false
				s.cloneRows(ctx, st, plan, cp, info, src, fresh, err.Error())
				s.startConnectRetry(st, plan)
				return true, false, false
			}
		}
	}

	// (3) the dm-clone, always created with hydration off so every bitmap
	// lands before a single region is copied. A failure here may still leave
	// a dm-clone behind with hydration off — a `dmsetup create` killed after
	// its ioctl ran — which CN16 does not serve through, and which the
	// retry's next pass recovers (step 2).
	created, err := s.ensureDmClone(ctx, plan, cp, srcDev)
	if err != nil {
		s.cloneRows(ctx, st, plan, cp, info, src, fresh, err.Error())
		s.startConnectRetry(st, plan)
		return true, false, false
	}
	if knobErr := s.ensureHydrationKnobs(ctx, cp); knobErr != nil {
		slog.ErrorContext(ctx, "applying dm-clone hydration knobs failed",
			slog.String("dm", cp.finalName),
			slog.String("error", knobErr.Error()))
	}

	if created || recovery {
		// (4) destination bitmaps first on a recovery, then every locally
		// present source chunk. The destination bitmaps must be applied in
		// full before the dm-clone handles any IO (architecture.md, Clone
		// crash recovery): a clone that
		// hydrates without them re-fetches regions the destination already
		// owns, overwriting newer local bytes with stale source bytes. A
		// partial apply — a read or a discard that failed — therefore fails
		// the clone closed: the device is removed, and even when that
		// removal fails (as it may for a stale dm-clone step 2 already could
		// not remove) its hydration stays off, so CN16 keeps the ns-devs
		// parked (nsDevNow): nothing can serve or hydrate through it, and the
		// retry loop re-runs the converge. Source chunks carry no such hazard
		// (they only ever cost an extra copy), so their failures stay logged.
		// A recovery runs this step even when step 3 created nothing: a
		// stale dm-clone that survived its removal in step 2 (its `dmsetup
		// remove` failed) is kept and converged by step 3 rather than
		// created, hydration still off, and needs every bitmap before step 5
		// just the same.
		if recovery {
			if err := s.applyDstBitmaps(ctx, plan, cp); err != nil {
				s.removeDm(ctx, cp.finalName)
				s.cloneRows(ctx, st, plan, cp, info, src, fresh,
					"destination bitmaps not applied: "+err.Error())
				s.startConnectRetry(st, plan)
				return true, false, false
			}
		}
		s.applyCloneChunks(ctx, st, plan, cp)
	}

	// (5) hydration is enabled only after step 4, so a recovered clone can
	// never re-fetch a region the destination already owns. When the enable
	// fails, hydration may still be off: CN16 then keeps the td parked, and
	// the retry's next pass runs the recovery again (step 2).
	if err := s.enableHydration(ctx, cp); err != nil {
		s.cloneRows(ctx, st, plan, cp, info, src, fresh, err.Error())
		s.startConnectRetry(st, plan)
		return true, false, false
	}
	// The read after step 5 is the rows' (architecture.md, Live-state
	// reporting: the raw status line carries hydration progress, which is
	// what DeleteClone's force check reads). One that does not show
	// hydration on — it did not answer, the device went, or the enable did
	// not take — leaves the build unfinished and registers the retry.
	dm = s.probeCloneDm(ctx, cp)
	s.cloneRows(ctx, st, plan, cp, info, src,
		func() cloneDmRead { return dm }, "")
	if dm.err != nil || !dm.present || !dm.hydrating {
		s.startConnectRetry(st, plan)
		return true, false, false
	}
	// (6) the dst td's ns-devs move onto the dm-clone in CN16, which runs
	// after clones in the CN9 build order and reads the status once more
	// before it does (nsDevNow).
	return false, false, true
}

// ensureCloneSource converges CN18 step 1, connecting every entry of
// src_tr_conf_list that has no controller, and returns the source namespace
// device, the source as the step last read it, and whether the step holds:
// the read is ready and no connect of the step failed. Its connect step is
// CN10's, on the same pass budget the legs drew on (CN18): a failed connect
// is tried again while the budget covers the pause, and after a connect this
// pass made the subsystem is re-read until the source namespace is there. A
// connect that fails ends the step, and the subsystem is read once more, so
// the rows say what that connect left behind. CN10's unknown-controller
// guard is not applied: a source controller whose address did not answer
// matches no entry, so an entry it alone serves is connected again — a
// duplicate the host refuses while that controller lives — and nothing is
// disconnected, since this step retires no path.
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
	src := s.readCloneSource(ctx, cp)
	if src.inFlight || src.readErr != nil {
		return "", src, false
	}
	connected := false
	for _, tr := range cp.clone.GetSrcTrConfList() {
		if src.view.ctrlOf(tr.GetTrAddr(), tr.GetTrSvcId()) != nil {
			continue
		}
		if err := s.connectWithin(ctx, budget, agent.TrConf{
			TrType:  tr.GetTrType(),
			AdrFam:  tr.GetAdrFam(),
			TrAddr:  tr.GetTrAddr(),
			TrSvcId: tr.GetTrSvcId(),
		}, nqn, plan.hostNqn()); err != nil {
			src.stepErr = err
			break
		}
		connected = true
	}
	switch {
	case src.stepErr != nil:
		src.view, src.readErr = s.readSubsys(ctx, nqn, nsIdx)
		return "", src, false
	case connected:
		src.view, src.readErr = s.awaitNsHead(ctx, budget, nqn, nsIdx)
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

// ensureDmClone converges the dm-clone and reports whether this pass created
// it. Only the four positional arguments are compared: the feature and core
// arguments dm-clone prints back reflect its live hydration state, and
// treating those as drift would reload the device onto `no_hydration` on every
// pass.
func (s *CnAgentServer) ensureDmClone(
	ctx context.Context,
	plan *cntlrPlan,
	cp *clonePlan,
	srcDev string,
) (bool, error) {
	if cp.regionSectors == 0 || cp.sectors == 0 {
		return false, fmt.Errorf("clone geometry is 0")
	}
	metaNo, err := s.dm.DevNo(ctx, cp.metaDmPath)
	if err != nil {
		return false, err
	}
	destNo, err := s.dm.DevNo(ctx, s.nf.DmPath(cp.dstTd.raid0Name))
	if err != nil {
		return false, err
	}
	srcNo, err := s.dm.DevNo(ctx, srcDev)
	if err != nil {
		return false, err
	}
	conf := cp.clone.GetDmCloneConf()
	// no_discard_passdown is not optional here (CN18 step 3 and [D7]: the
	// table's `2 no_hydration …`): CN22 and the recovery of architecture.md,
	// Clone crash recovery, mark regions hydrated
	// by `blkdiscard`ing them, and with passdown enabled dm-clone would remap
	// those discards to the destination raid0 — unmapping the very blocks the
	// destination already owns, and any host write that landed on a hydrated
	// region while `auto_resume` let the namespace serve.
	table := agent.CloneTable(cp.sectors, metaNo, destNo, srcNo,
		cp.regionSectors, true, true,
		conf.GetHydrationThreshold(), conf.GetHydrationBatchSize())

	dev, err := s.dm.Info(ctx, cp.finalName)
	if err != nil {
		return false, err
	}
	if dev == nil {
		if err := s.dm.Create(ctx, cp.finalName, table); err != nil {
			return false, err
		}
		return true, nil
	}
	targets, err := s.dm.Table(ctx, cp.finalName)
	if err != nil {
		return false, err
	}
	args := []string{metaNo, destNo, srcNo,
		fmt.Sprintf("%d", cp.regionSectors)}
	if dmTableMatches(targets, "clone", cp.sectors, args, len(args)) {
		if dev.Suspended {
			return false, s.dm.Resume(ctx, cp.finalName)
		}
		return false, nil
	}
	if err := s.dm.Reload(ctx, cp.finalName, table); err != nil {
		return false, err
	}
	return true, nil
}

// ensureHydrationKnobs applies hydration_threshold / hydration_batch_size,
// probe-first. It never touches the enable flag — that is step 5's job, and
// it must not run before the bitmaps are applied.
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
// so no sp_level ever pauses it ([D11]): this only ever enables.
func (s *CnAgentServer) enableHydration(
	ctx context.Context,
	cp *clonePlan,
) error {
	status, err := s.cloneStatus(ctx, cp)
	if err != nil {
		return err
	}
	if status.HydrationEnabled {
		return nil
	}
	return s.dm.Message(ctx, cp.finalName, 0, "enable_hydration")
}

// cloneHydrating reports whether the live dm-clone's status shows hydration
// on — the mark step 5 leaves, and nothing before it. A status that does not
// parse as a dm-clone's shows no such mark either, and neither does a
// dm-clone that does not exist. Absence is confirmed, never assumed: the
// status is read first, as on every pass, and only when that read fails does
// a `dmsetup info` ask whether the device is there at all, so a failed read
// of a dm-clone that is there, or one whose absence the info did not
// confirm, stays an error. CN16 reads it to decide whether the dm-clone may
// serve (nsDevNow): a clone whose dm-clone is gone — removed by a failed
// build, or never built while its clone waits for room in the arena — parks
// its td's ns-devs. CN18 step 2 and the clone's rows read the dm-clone
// through probeCloneDm, by the same rule.
func (s *CnAgentServer) cloneHydrating(
	ctx context.Context,
	cp *clonePlan,
) (bool, error) {
	raw, err := s.dm.Status(ctx, cp.finalName)
	if err != nil {
		if dev, infoErr := s.dm.Info(
			ctx, cp.finalName); infoErr == nil && dev == nil {
			return false, nil
		}
		return false, err
	}
	status, ok := agent.ParseCloneStatus(raw)
	return ok && status.HydrationEnabled, nil
}

func (s *CnAgentServer) cloneStatus(
	ctx context.Context,
	cp *clonePlan,
) (*agent.CloneStatus, error) {
	raw, err := s.dm.Status(ctx, cp.finalName)
	if err != nil {
		return nil, err
	}
	status, ok := agent.ParseCloneStatus(raw)
	if !ok {
		return nil, fmt.Errorf("unparsable dm-clone status %q", raw)
	}
	return status, nil
}

// parkTdNsDevs reloads every ns-dev backed by one td onto its dm-error, so the
// td serves nothing while a clone is (re)built over it (the park of the
// recovery build, cnagent.md CN18).
func (s *CnAgentServer) parkTdNsDevs(
	ctx context.Context,
	plan *cntlrPlan,
	tdId uint64,
) {
	for _, np := range plan.namespaces {
		if np.td == nil || np.td.tdId != tdId {
			continue
		}
		if err := s.parkNsDev(ctx, np); err != nil {
			slog.ErrorContext(ctx, "parking a namespace on dm-error failed",
				slog.String("dm", np.devName),
				slog.String("error", err.Error()))
		}
	}
}

// cloneDmRead is one read of a clone's dm-clone (probeCloneDm): whether the
// device is there, its raw status line, and whether that line shows hydration
// on, the mark CN18 step 5 leaves. err is a read that did not answer, of a
// dm-clone that is there or whose absence the read could not confirm.
type cloneDmRead struct {
	present   bool
	raw       string
	hydrating bool
	err       error
}

// rawLine is the status line the read took of a dm-clone that is there, and
// "" when it took none.
func (dm cloneDmRead) rawLine() string {
	if dm.err != nil || !dm.present {
		return ""
	}
	return dm.raw
}

// probeCloneDm reads a clone's dm-clone, for CN18 step 2's test of an
// unfinished build and for the clone's rows (cloneRows): `dmsetup info`, then
// `dmsetup status` of a device that is there. The raw status line rides into
// the dm row's details (architecture.md, Live-state reporting), and a status
// that does not parse as a dm-clone's shows no hydration. Absence is
// confirmed, never assumed, by cloneHydrating's rule: after a status read
// that fails, a `dmsetup info` that finds the device gone makes the read an
// absence, and anything else keeps it an error. Reading the status makes the
// kernel commit the clone's metadata (dnagent.md SH17).
func (s *CnAgentServer) probeCloneDm(
	ctx context.Context,
	cp *clonePlan,
) cloneDmRead {
	dev, err := s.dm.Info(ctx, cp.finalName)
	if err != nil {
		return cloneDmRead{err: err}
	}
	if dev == nil {
		return cloneDmRead{}
	}
	raw, err := s.dm.Status(ctx, cp.finalName)
	if err != nil {
		if gone, infoErr := s.dm.Info(
			ctx, cp.finalName); infoErr == nil && gone == nil {
			return cloneDmRead{}
		}
		return cloneDmRead{err: err}
	}
	status, ok := agent.ParseCloneStatus(raw)
	return cloneDmRead{
		present:   true,
		raw:       raw,
		hydrating: ok && status.HydrationEnabled,
	}
}
