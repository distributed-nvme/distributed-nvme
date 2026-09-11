package cnagent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// CN18 — clones (fig. `090Clone`), primary only and only below
// SP_LEVEL_NO_CLONE.

// ensureClone runs the CN18 sequence for one clone and reports whether the
// source connection still needs the background retry. Every step captures its
// own failure into the clone's ResInfos and lets the pass continue (CN29).
func (s *CnAgentServer) ensureClone(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	cp *clonePlan,
	info *pb.CntlrInfo,
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
	srcDev, view, err := s.ensureCloneSource(ctx, plan, cp)
	if err != nil {
		info.CloneIdToTarget[cp.cloneId] = st.tracker.Err(
			tgtKey, cp.clone.GetSrcNqn(), err.Error())
		info.CloneIdToDmClone[cp.cloneId] = st.tracker.Missing(
			dmKey, cp.finalName, "source not connected")
		info.CloneIdToMeta[cp.cloneId] = s.cloneMetaInfo(
			ctx, st, plan, cp, metaKey)
		s.startConnectRetry(st, plan)
		return true
	}
	info.CloneIdToTarget[cp.cloneId] = st.tracker.Ok(
		tgtKey, cp.clone.GetSrcNqn(), pathStates(view))

	// (2) the dm-clone's metadata. This build is a §11.5 recovery whenever
	// that metadata does not survive — the volatile clone-metadata arena is
	// gone (CN reboot, failover to a CN that never ran the clone, tmpfs loss),
	// or the clone has never been built at all. The metadata **wrapper** alone
	// is not the test: a pass that created the wrapper and then failed to
	// build the dm-clone (an agent restart, a raid0 not up yet) leaves a
	// freshly discarded slot that dm-clone would format fresh, with nothing
	// hydrated — so an absent dm-clone device is a recovery too. On a first
	// build the destination td is empty by contract ([D3]), so the §11.5 read
	// costs one metadata snapshot and discards nothing.
	arena, err := s.planArena(ctx, plan)
	if err != nil {
		info.CloneIdToMeta[cp.cloneId] = st.tracker.Err(
			metaKey, metaName, err.Error())
		info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
			dmKey, cp.finalName, "metadata wrapper missing")
		return false
	}
	metaOk := s.cloneMetaConverged(ctx, arena, cp)
	dmDev, err := s.dm.Info(ctx, cp.finalName)
	if err != nil {
		info.CloneIdToMeta[cp.cloneId] = st.tracker.Err(
			metaKey, metaName, err.Error())
		info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
			dmKey, cp.finalName, err.Error())
		return false
	}
	recovery := !metaOk || dmDev == nil
	if recovery {
		// §11.5 step 1 comes first: nothing may serve the td while the
		// destination bitmaps are still being applied, or a read of an
		// already-copied (and possibly since-rewritten) region would be
		// fetched from the source again, returning stale data over the newer
		// local bytes.
		s.parkTdNsDevs(ctx, plan, cp.dstTd.tdId)
		s.removeDm(ctx, cp.finalName)
		// Only now, with nothing mapping it, may a mismatched wrapper be
		// replaced — and a *matching* one is left strictly alone: allocating
		// is what hole-punches the slot, and re-punching a live slot would
		// wipe a valid dm-clone superblock (CN18).
		if !metaOk {
			if err := s.ensureCloneMeta(ctx, plan, cp); err != nil {
				info.CloneIdToMeta[cp.cloneId] = st.tracker.Err(
					metaKey, metaName, err.Error())
				info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
					dmKey, cp.finalName, "metadata wrapper missing")
				return false
			}
		}
	}
	info.CloneIdToMeta[cp.cloneId] = st.tracker.Ok(metaKey, metaName, "")

	// (3) the dm-clone, always created with hydration off so every bitmap
	// lands before a single region is copied.
	created, err := s.ensureDmClone(ctx, plan, cp, srcDev)
	if err != nil {
		info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
			dmKey, cp.finalName, err.Error())
		return false
	}
	if knobErr := s.ensureHydrationKnobs(ctx, cp); knobErr != nil {
		slog.ErrorContext(ctx, "applying dm-clone hydration knobs failed",
			slog.String("dm", cp.finalName),
			slog.String("error", knobErr.Error()))
	}

	if created {
		// (4) destination bitmaps first on a recovery, then every locally
		// present source chunk. The destination bitmaps must be applied in
		// full before the dm-clone handles any IO (§11.5): a clone that
		// hydrates without them re-fetches regions the destination already
		// owns, overwriting newer local bytes with stale source bytes. A
		// partial read therefore fails the clone closed — the device goes,
		// so nothing can serve or hydrate through it, and the retry loop
		// re-runs the converge. Source chunks carry no such hazard (they
		// only ever cost an extra copy), so their failures stay logged.
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
	// never re-fetch a region the destination already owns.
	if err := s.enableHydration(ctx, cp); err != nil {
		info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
			dmKey, cp.finalName, err.Error())
		return false
	}
	// §9.5: the raw dm-clone status line carries hydration progress, which is
	// what DeleteClone's force check reads.
	raw, err := s.dm.Status(ctx, cp.finalName)
	if err != nil {
		info.CloneIdToDmClone[cp.cloneId] = st.tracker.Err(
			dmKey, cp.finalName, err.Error())
		return false
	}
	info.CloneIdToDmClone[cp.cloneId] = st.tracker.Ok(dmKey, cp.finalName, raw)
	// (6) the dst td's ns-devs move onto the dm-clone in CN16, which runs
	// after clones in the CN9 build order.
	return false
}

// ensureCloneSource connects to every entry of src_tr_conf_list and returns
// the source namespace device.
func (s *CnAgentServer) ensureCloneSource(
	ctx context.Context,
	plan *cntlrPlan,
	cp *clonePlan,
) (string, *subsysView, error) {
	nqn := cp.clone.GetSrcNqn()
	nsIdx := cp.clone.GetSrcNsIdx()
	view, err := s.readSubsys(ctx, nqn, nsIdx)
	if err != nil {
		return "", nil, err
	}
	connected := false
	for _, tr := range cp.clone.GetSrcTrConfList() {
		if view.ctrlOf(tr.GetTrAddr(), tr.GetTrSvcId()) != nil {
			continue
		}
		if err := s.host.Connect(ctx, agent.TrConf{
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
		if view, err = s.readSubsys(ctx, nqn, nsIdx); err != nil {
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

// cloneMetaInfo is the CN28 clone_id_to_meta row: the wrapper's own dm table
// read back out of the arena registry ([D14]).
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
	// no_discard_passdown is not optional here (CN18 step 3, Appendix A's
	// `2 no_hydration …`): CN22 and the §11.5 recovery mark regions hydrated
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
// td serves nothing while a clone is (re)built over it (§11.5 step 1).
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

// ---------------------------------------------------------------------------
// Teardown (CN18, strictly ordered)
// ---------------------------------------------------------------------------

// retireClone removes one clone stack. The order is load-bearing: the ns-devs
// come off the dm-clone first, the dm-clone goes **before** its source
// connection dies (dm-clone flushes through the source on removal and blocks
// without it), and only then the metadata wrapper and the connection. Removing
// the wrapper is what frees its units: they reappear as free in the next
// registry enumeration ([D14]).
func (s *CnAgentServer) retireClone(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	cp *clonePlan,
) {
	if cp.dstTd != nil {
		s.repointTdNsDevs(ctx, plan, cp.dstTd.tdId)
	}
	s.removeDm(ctx, cp.finalName)
	// The wrapper's removal is what frees its units, so it is a registry
	// mutation and runs under cloneMetaMu (R3.6) — the dm-clone above it is
	// already gone, so nothing maps it and the removal cannot fail EBUSY.
	s.removeCloneMetaDm(ctx, cp.metaDmName)
	// `--nqn` is safe here: every path of the source subsystem is being
	// retired with the clone.
	s.disconnect(ctx, cp.clone.GetSrcNqn())
	// Only a clone that actually left `clone_list` loses its chunks (SH7). A
	// clone the role or the sp_level merely suppresses keeps them
	// applied-by-file (CN19, CN22) — deleting them on every standby converge
	// would make the worker re-push them forever, and would leave a promoted
	// standby's §11.5 rebuild nothing to skip with.
	if plan.cloneById[cp.cloneId] == nil {
		s.dropCloneChunks(ctx, st, plan, cp.cloneId)
	}
	st.tracker.Drop(resKeyOf(resKeyCloneTgtFmt, cp.cloneId))
	st.tracker.Drop(resKeyOf(resKeyCloneDmFmt, cp.cloneId))
	st.tracker.Drop(resKeyOf(resKeyCloneMetaFmt, cp.cloneId))
}

// repointTdNsDevs moves every ns-dev of one td onto the backing the **new**
// plan wants — the raid0 when the cntlr still serves it (CN16 rule 5), the
// dm-error otherwise. It is what lets a dm-clone be removed: a device another
// dm table still maps cannot go.
func (s *CnAgentServer) repointTdNsDevs(
	ctx context.Context,
	plan *cntlrPlan,
	tdId uint64,
) {
	for _, np := range plan.namespaces {
		if np.td == nil || np.td.tdId != tdId {
			continue
		}
		if err := s.ensureNsDev(ctx, np); err != nil {
			slog.ErrorContext(ctx, "repointing a namespace device failed",
				slog.String("dm", np.devName),
				slog.String("error", err.Error()))
		}
	}
}

// probeCloneDm is the read-only view of a dm-clone: the raw status line rides
// into details (§9.5).
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
