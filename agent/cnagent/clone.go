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

// detailsCloneMetaMissing is what `clone_id_to_dm_clone` reports when CN18 step
// 2 could not supply the metadata slot (CN28): with no slot this pass can map,
// the dm-clone is not merely absent, it cannot be built at all — which is why
// the row says it even where a dm-clone from an earlier pass happens to still
// be up. Both channels say it — ensureClone below and probeCntlr's clone loop.
const detailsCloneMetaMissing = "metadata wrapper missing"

// ensureClone runs the CN18 sequence for one clone and reports whether the
// cntlr needs the CN10 background retry: the source did not connect, a later
// step stopped the build before step 5 turned hydration on, or the status
// read after that failed — except when ensureCloneMeta fails: its failures
// (the arena's refusal of the slot among them) are left to their rows. A
// failed knob message or source chunk stops nothing and is only logged. CN16
// serves the destination td through the dm-clone only while its status shows
// hydration on (rule 5, nsDevNow), and step 5 alone turns that on, so a build
// or a recovery that stops short of step 5 leaves the td parked until a later
// pass finishes it; when a pass that finished the build fails to converge a
// rule-5 ns-dev of the td, CN16 registers the retry itself
// (cloneBuiltThisPass). Every step captures its own failure into the clone's
// ResInfos and lets the pass continue (CN29). budget is the pass's one CN10
// wait budget, the one its legs drew on.
func (s *CnAgentServer) ensureClone(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	cp *clonePlan,
	info *pb.CntlrInfo,
	budget *agent.WaitBudget,
) bool {
	tgtKey := resKeyOf(resKeyCloneTgtFmt, cp.cloneId)
	dmKey := resKeyOf(resKeyCloneDmFmt, cp.cloneId)
	metaKey := resKeyOf(resKeyCloneMetaFmt, cp.cloneId)
	metaName := cp.metaDmName

	if cp.dstTd == nil {
		details := fmt.Sprintf("destination td %d not found",
			cp.clone.GetDstTdId())
		info.CloneIdToTarget[cp.cloneId] = st.tracker.Err(
			tgtKey, cp.clone.GetSrcNqn(), details)
		info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
			dmKey, cp.finalName, details)
		info.CloneIdToMeta[cp.cloneId] = st.tracker.Err(
			metaKey, metaName, details)
		return false
	}
	if cp.deferred {
		// [D15]: the destination td's raid0 does not exist yet, so there is
		// nothing to clone onto — no metadata slot, no dm-clone and no source
		// connection, the cn mirror of a still-zeroing migration destination.
		s.reportCloneDeferred(st, cp, info)
		return false
	}

	// (1) the source connection: one controller per source cntlr, all in one
	// subsystem, so the source's own ANA picks the serving path.
	srcDev, view, err := s.ensureCloneSource(ctx, plan, cp, budget)
	if err != nil {
		info.CloneIdToTarget[cp.cloneId] = st.tracker.Err(
			tgtKey, cp.clone.GetSrcNqn(), err.Error())
		// A walk that did not answer leaves the source unknown, not
		// unconnected (CN10), and the row must not say otherwise.
		details := "source not connected"
		if errors.Is(err, errSubsysUnknown) {
			details = "source unknown: " + err.Error()
		}
		info.CloneIdToDmClone[cp.cloneId] = st.tracker.Missing(
			dmKey, cp.finalName, details)
		info.CloneIdToMeta[cp.cloneId] = s.cloneMetaInfo(
			ctx, st, plan, cp, metaKey)
		s.startConnectRetry(st, plan)
		return true
	}
	info.CloneIdToTarget[cp.cloneId] = st.tracker.Ok(
		tgtKey, cp.clone.GetSrcNqn(), pathStates(view))

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
	// A status read that fails or does not answer decides nothing: it fails
	// the step like the `dmsetup info` before it, removes and enables
	// nothing, and registers the retry; CN16 reads the status again and
	// moves no ns-dev onto a dm-clone it has not seen hydrating (nsDevNow).
	// On a first build the destination td is empty by contract ([D3]), so
	// the recovery's read (architecture.md, Clone crash recovery) costs one
	// metadata snapshot and discards nothing.
	arena, err := s.planArena(ctx, plan)
	if err != nil {
		info.CloneIdToMeta[cp.cloneId] = st.tracker.Err(
			metaKey, metaName, err.Error())
		info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
			dmKey, cp.finalName, detailsCloneMetaMissing)
		s.startConnectRetry(st, plan)
		return true
	}
	metaOk := s.cloneMetaConverged(ctx, arena, cp)
	dmDev, err := s.dm.Info(ctx, cp.finalName)
	hydrating := false
	if err == nil && dmDev != nil {
		hydrating, err = s.cloneHydrating(ctx, cp)
	}
	if err != nil {
		info.CloneIdToMeta[cp.cloneId] = st.tracker.Err(
			metaKey, metaName, err.Error())
		info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
			dmKey, cp.finalName, err.Error())
		s.startConnectRetry(st, plan)
		return true
	}
	recovery := !metaOk || dmDev == nil || !hydrating
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
				info.CloneIdToMeta[cp.cloneId] = st.tracker.Err(
					metaKey, metaName, err.Error())
				info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
					dmKey, cp.finalName, detailsCloneMetaMissing)
				return false
			}
		}
	}
	info.CloneIdToMeta[cp.cloneId] = st.tracker.Ok(metaKey, metaName, "")

	// (3) the dm-clone, always created with hydration off so every bitmap
	// lands before a single region is copied. A failure here may still leave
	// a dm-clone behind with hydration off — a `dmsetup create` killed after
	// its ioctl ran — which CN16 does not serve through, and which the
	// retry's next pass recovers (step 2).
	created, err := s.ensureDmClone(ctx, plan, cp, srcDev)
	if err != nil {
		info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
			dmKey, cp.finalName, err.Error())
		s.startConnectRetry(st, plan)
		return true
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
				info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
					dmKey, cp.finalName,
					"destination bitmaps not applied: "+err.Error())
				s.startConnectRetry(st, plan)
				return true
			}
		}
		s.applyCloneChunks(ctx, st, plan, cp)
	}

	// (5) hydration is enabled only after step 4, so a recovered clone can
	// never re-fetch a region the destination already owns. When the enable
	// fails, hydration may still be off: CN16 then keeps the td parked, and
	// the retry's next pass runs the recovery again (step 2).
	if err := s.enableHydration(ctx, cp); err != nil {
		info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
			dmKey, cp.finalName, err.Error())
		s.startConnectRetry(st, plan)
		return true
	}
	// architecture.md, Live-state reporting: the raw dm-clone status line
	// carries hydration progress, which is what DeleteClone's force check
	// reads.
	raw, err := s.dm.Status(ctx, cp.finalName)
	if err != nil {
		info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
			dmKey, cp.finalName, err.Error())
		s.startConnectRetry(st, plan)
		return true
	}
	info.CloneIdToDmClone[cp.cloneId] = st.tracker.Ok(dmKey, cp.finalName, raw)
	// (6) the dst td's ns-devs move onto the dm-clone in CN16, which runs
	// after clones in the CN9 build order and reads the status once more
	// before it does (nsDevNow).
	return false
}

// cloneBuiltThisPass reports whether np is a rule-5 ns-dev whose clone this
// pass's CN18 took through step 5 and the status read after it. That is the
// one path on which ensureClone sets the clone's `clone_id_to_dm_clone` row
// OK, so the row is the answer: every other outcome of the pass leaves it
// ERROR, MISSING or PROVISIONING.
func cloneBuiltThisPass(info *pb.CntlrInfo, np *nsPlan) bool {
	if np.clone == nil {
		return false
	}
	return info.GetCloneIdToDmClone()[np.clone.cloneId].GetStatus() ==
		pb.ResStatus_RES_STATUS_OK
}

// ensureCloneSource connects to every entry of src_tr_conf_list and returns
// the source namespace device. Its connect step is CN10's, on the same pass
// budget the legs drew on (CN18): a failed connect is tried again while the
// budget covers the pause, and after a connect this pass made the subsystem
// is re-read until the source namespace is there. CN10's unknown-controller
// guard is not applied: a source controller whose address did not answer
// matches no entry, so an entry it alone serves is connected again — a
// duplicate the host refuses while that controller lives — and nothing is
// disconnected, since this step retires no path.
//
// A source a sweep is still disconnecting is neither adopted nor connected
// (disconnectInFlight): a dm-clone built on it would lose its source when
// that disconnect lands, and every read of a region not yet hydrated would
// fail until a later converge reloaded the table.
func (s *CnAgentServer) ensureCloneSource(
	ctx context.Context,
	plan *cntlrPlan,
	cp *clonePlan,
	budget *agent.WaitBudget,
) (string, *subsysView, error) {
	nqn := cp.clone.GetSrcNqn()
	nsIdx := cp.clone.GetSrcNsIdx()
	if s.disconnectInFlight(nqn) {
		return "", nil, fmt.Errorf("%s", detailsDisconnectInFlight(nqn))
	}
	view, err := s.readSubsys(ctx, nqn, nsIdx)
	if err != nil {
		return "", nil, err
	}
	connected := false
	for _, tr := range cp.clone.GetSrcTrConfList() {
		if view.ctrlOf(tr.GetTrAddr(), tr.GetTrSvcId()) != nil {
			continue
		}
		if err := s.connectWithin(ctx, budget, agent.TrConf{
			TrType:  tr.GetTrType(),
			AdrFam:  tr.GetAdrFam(),
			TrAddr:  tr.GetTrAddr(),
			TrSvcId: tr.GetTrSvcId(),
		}, nqn, plan.hostNqn()); err != nil {
			return "", nil, err
		}
		connected = true
	}
	if connected {
		if view, err = s.awaitNsHead(ctx, budget, nqn, nsIdx); err != nil {
			return "", nil, err
		}
	}
	if view.nsDev == "" {
		return "", nil, fmt.Errorf("no namespace %d on %s", nsIdx, nqn)
	}
	return view.nsDev, view, nil
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

// cloneMetaInfo is the ordinary CN28 clone_id_to_meta row: the wrapper's own dm
// table read back out of the arena registry ([D14]). probeCntlr skips it for a
// clone that is past CN18 step 1 and whose slot the arena cannot supply — that
// row is probeCloneArenaCannotSupply's, so both channels report the same
// refusal pair. ensureClone's step 1 failure branch above still reports it.
func (s *CnAgentServer) cloneMetaInfo(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	cp *clonePlan,
	metaKey string,
) *pb.ResInfo {
	status, details := s.probeCloneMeta(ctx, plan, cp)
	return st.tracker.Set(metaKey, cp.metaDmName, status, details)
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
// parse as a dm-clone's shows no such mark either; only a read that failed is
// an error. Step 2 reads it to find an unfinished build, and CN16 to decide
// whether the dm-clone may serve (nsDevNow).
func (s *CnAgentServer) cloneHydrating(
	ctx context.Context,
	cp *clonePlan,
) (bool, error) {
	raw, err := s.dm.Status(ctx, cp.finalName)
	if err != nil {
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

// probeCloneDm is the CN28 probe of a dm-clone: the raw status line rides
// into details (architecture.md, Live-state reporting). Reading that status
// makes the kernel commit the clone's metadata (dnagent.md SH17).
func (s *CnAgentServer) probeCloneDm(
	ctx context.Context,
	cp *clonePlan,
) (pb.ResStatus, string) {
	dev, err := s.dm.Info(ctx, cp.finalName)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if dev == nil {
		return pb.ResStatus_RES_STATUS_MISSING, ""
	}
	raw, err := s.dm.Status(ctx, cp.finalName)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	return pb.ResStatus_RES_STATUS_OK, raw
}
