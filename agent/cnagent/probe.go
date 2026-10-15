package cnagent

import (
	"context"
	"fmt"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The probe map of CN28: the rows of a check round and an info read, read
// from the node's reading (readCntlr, takeReading) and from the reads that
// stay outside it: the status of each thin pool and dm-clone; the configfs
// of the exports; the clone metadata area's loop device and its number; the
// attributes of the array the md walk matched to a group (Md.Detail) and
// that array's number (mdDevNo); the entry of each NVMe subsystem a lookup
// uses — its NQN once more, its namespace and its controllers
// (lookupSubsys); the own read of a dm name that a partial info listing, or
// the table listing, lacks (cnReading); and the fresh `dmsetup info` that
// confirms a device gone after its own status or table read failed. No
// function here creates, changes or removes an object of the node, as
// Get*Info and the Check* streams must not (CN23, SH25); a `dmsetup status`
// of a thin pool or a dm-clone still makes the kernel commit that target's
// metadata (dnagent.md SH17). In particular `reserve_metadata_snap` runs
// only inside `dumpThinMetadata` — the CN25 bitmap reads, CN14's thin id
// sweeps (the activation sweep and the sweep after a failed delete) and the
// dst-bitmap read of architecture.md, Clone crash recovery — never from here
// (CN29).

// probeCntlr writes the rows of one check round or info read from plan, whose
// reading readCntlr took (plan.rd, plan.walk, plan.mdWalks).
func (s *CnAgentServer) probeCntlr(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
) *pb.CntlrInfo {
	t := st.tracker
	info := newCntlrInfo()

	// Legs — the wrapper table plus the CN11 prober outcome on a primary,
	// transport liveness and ana_state per desired side on a standby.
	for _, lp := range plan.legs {
		key := resKeyOf(resKeyLegFmt, lp.legId)
		// CN19's suppression is evaluated first and wins over [D15]'s deferral:
		// the operator has said the resource must not exist, which is a
		// stronger statement than "it is coming".
		if !plan.wantLeg {
			info.LegIdToLeg[lp.legId] = t.Missing(key, lp.name, detailsSpLevel)
			continue
		}
		if lp.provisioning {
			info.LegIdToLeg[lp.legId] = t.Provisioning(
				key, lp.name, detailsProvisioning)
			continue
		}
		// ensureLeg's registry gate (CN10), answered the way the converge
		// answers it: that leg fails before it reads anything, so a row
		// judged from what the probe finds of a leg still being torn down
		// would flip against the converge's, and move its epoch, every
		// round the disconnect runs.
		if s.disconnectInFlight(lp.nqn) {
			info.LegIdToLeg[lp.legId] = t.Err(
				key, lp.name, detailsDisconnectInFlight(lp.nqn))
			continue
		}
		info.LegIdToLeg[lp.legId] = s.legInfo(ctx, st, plan, lp, nil)
	}

	// Groups — the reading's md walk serves all of them and the concat rows
	// below (Md.Detail, mdDevNo).
	for _, gp := range plan.grps {
		key := resKeyOf(resKeyGrpFmt, gp.grpId)
		switch {
		case plan.wantGrp && gp.deferred:
			info.GrpIdToMdRaid[gp.grpId] = t.Provisioning(
				key, gp.resName, detailsProvisioning)
		case plan.wantGrp:
			status, details := s.probeGroup(ctx, plan, gp)
			info.GrpIdToMdRaid[gp.grpId] = t.Set(
				key, gp.resName, status, details)
		case plan.primary:
			info.GrpIdToMdRaid[gp.grpId] = t.Missing(
				key, gp.resName, detailsSpLevel)
		}
	}

	// Pool concats and thin-pools.
	for _, sp := range plan.slices {
		metaKey := resKeyOf(resKeyPoolMetaFmt, sp.sliceId)
		dataKey := resKeyOf(resKeyPoolDataFmt, sp.sliceId)
		poolKey := resKeyOf(resKeyPoolFmt, sp.sliceId)
		if !plan.wantPool {
			if plan.primary {
				info.SliceIdToMeta[sp.sliceId] = t.Missing(
					metaKey, sp.poolMetaName, detailsSpLevel)
				info.SliceIdToData[sp.sliceId] = t.Missing(
					dataKey, sp.poolDataName, detailsSpLevel)
				info.SliceIdToDmPool[sp.sliceId] = t.Missing(
					poolKey, sp.poolFinalName, detailsSpLevel)
			}
			continue
		}
		if sp.deferred {
			s.reportSliceDeferred(st, sp, info)
			continue
		}
		// The concats are compared against the *effective* group lists ([D15]):
		// a pool that has not grown into a still-provisioning group is OK at
		// its current size, not a mismatch.
		status, details := s.probeDmConcat(
			ctx, plan, sp.poolMetaName, poolSegments(sp.effMetaGrps))
		info.SliceIdToMeta[sp.sliceId] = t.Set(
			metaKey, sp.poolMetaName, status, details)
		status, details = s.probeDmConcat(
			ctx, plan, sp.poolDataName, poolSegments(sp.effDataGrps))
		info.SliceIdToData[sp.sliceId] = t.Set(
			dataKey, sp.poolDataName, status, details)
		status, details = s.probePool(ctx, plan, sp)
		info.SliceIdToDmPool[sp.sliceId] = t.Set(
			poolKey, sp.poolFinalName, status, details)
	}

	// Thin volumes.
	for _, tp := range plan.tds {
		if !plan.primary {
			continue
		}
		// The torn mark is the converge's (tornMark, CN14); the rows carry
		// no reason (CN29): only a converge names a lost thin id.
		thinInfo := &pb.CntlrInfo_ThinInfo{
			SliceIdToDmThin: make(map[uint64]*pb.ResInfo, len(plan.slices)),
			Torn:            tornMark(st, tp),
		}
		for _, sp := range plan.slices {
			key := thinResKey(tp.tdId, sp.sliceId)
			name := plan.thinName(tp.tdId, sp.sliceId)
			if !plan.wantPool {
				thinInfo.SliceIdToDmThin[sp.sliceId] = t.Missing(
					key, name, detailsSpLevel)
				continue
			}
			if sp.deferred {
				thinInfo.SliceIdToDmThin[sp.sliceId] = t.Provisioning(
					key, name, detailsProvisioning)
				continue
			}
			status, details := s.probeThin(ctx, plan, tp, sp)
			thinInfo.SliceIdToDmThin[sp.sliceId] = t.Set(
				key, name, status, details)
		}
		info.TdIdToThinInfo[tp.tdId] = thinInfo
	}

	// Per-td raid0 and dm-error.
	for _, tp := range plan.tds {
		if plan.wantAny {
			status, details := s.probeDmArgs(
				ctx, plan.rd, tp.errorName, "error", tp.sectors, nil, 0)
			info.TdIdToDmError[tp.tdId] = t.Set(
				resKeyOf(resKeyTdErrorFmt, tp.tdId), tp.errorName,
				status, details)
		} else {
			// CN19: the converge reports the suppressed dm-error rather than
			// leaving it out (syncup_cntlr.go build()), so the probe must
			// too — a row that disappears one round after being reported
			// MISSING is exactly the omitted-vs-never-looked-at ambiguity
			// CN19 forbids.
			info.TdIdToDmError[tp.tdId] = t.Missing(
				resKeyOf(resKeyTdErrorFmt, tp.tdId), tp.errorName,
				detailsSpLevel)
		}
		raid0Key := resKeyOf(resKeyRaid0Fmt, tp.tdId)
		switch {
		case plan.wantPool && tp.deferred:
			info.TdIdToRaid0[tp.tdId] = t.Provisioning(
				raid0Key, tp.raid0Name, detailsProvisioning)
		case plan.wantPool:
			args, err := s.raid0Args(ctx, plan, tp)
			if err != nil {
				info.TdIdToRaid0[tp.tdId] = t.Err(
					raid0Key, tp.raid0Name, err.Error())
				break
			}
			status, details := s.probeDmArgs(
				ctx, plan.rd, tp.raid0Name, "striped", tp.sectors, args, 0)
			info.TdIdToRaid0[tp.tdId] = t.Set(
				raid0Key, tp.raid0Name, status, details)
		case plan.primary:
			info.TdIdToRaid0[tp.tdId] = t.Missing(
				raid0Key, tp.raid0Name, detailsSpLevel)
		}
	}

	// Clones: the rows of CN18's shared reading (cloneRows), from this
	// round's read of the source and of the dm-clone, so a check round
	// reports what the converge reports for the same node.
	for _, cp := range plan.clones {
		if !plan.wantClone {
			if plan.primary {
				s.reportCloneSuppressed(st, cp, info)
			}
			continue
		}
		if cp.dstTd == nil {
			// The same CN18 error the converge reports (reportCloneNoDst):
			// there is nothing to clone onto, so the surviving stack is not
			// probed and no metadata budget is invented from a zero region
			// count.
			s.reportCloneNoDst(st, cp, info)
			continue
		}
		// A clone whose dst is merely still provisioning is deferred and
		// builds nothing ([D15]).
		if cp.deferred {
			s.reportCloneDeferred(st, cp, info)
			continue
		}
		s.cloneRows(ctx, st, plan, cp, info,
			s.readCloneSource(ctx, plan, cp),
			func() cloneDmRead { return s.probeCloneDm(ctx, plan.rd, cp) }, "")
	}

	if !plan.wantAny {
		// SP_LEVEL_DISABLE. The transfer, ns-dev, namespace and subsystem rows
		// below are all suppressed — and CN19 wants that *said*, through the
		// very helper the converge uses, so the two channels report the same
		// CntlrInfo for the same stored request (architecture.md, Check
		// streams: show_info always fills the complete current info).
		s.reportSuppressed(st, plan, info)
		return info
	}

	// Transfers.
	for _, xp := range plan.xfers {
		dmKey := resKeyOf(resKeyXferDmFmt, xp.xferId)
		ssKey := resKeyOf(resKeyXferSsFmt, xp.xferId)
		nsKey := resKeyOf(resKeyXferNsFmt, xp.xferId)
		nsName := nsResName(xp.nqn, int(xp.xfer.GetOriNsIdx()))
		if xp.ori == nil || xp.ori.td == nil {
			details := fmt.Sprintf("origin namespace %s/%d not found",
				xp.xfer.GetOriNqn(), xp.xfer.GetOriNsIdx())
			info.XferIdToDmLinear[xp.xferId] = t.Err(
				dmKey, xp.finalName, details)
			info.XferIdToSubsystem[xp.xferId] = t.Err(ssKey, xp.nqn, details)
			info.XferIdToNamespace[xp.xferId] = t.Err(nsKey, nsName, details)
			continue
		}
		status, details := s.probeXferDm(ctx, plan, xp)
		info.XferIdToDmLinear[xp.xferId] = deferredSet(
			t, xp.deferred, dmKey, xp.finalName, status, details)
		status, details = s.probeExport(ctx, s.xferSubsysConf(plan, xp))
		info.XferIdToSubsystem[xp.xferId] = deferredSet(
			t, xp.deferred, ssKey, xp.nqn, status, details)
		status, details = s.probeNamespaceObject(
			ctx, s.xferNsConf(plan, xp, plan.xferAnaGrpId(xp)))
		info.XferIdToNamespace[xp.xferId] = deferredSet(
			t, xp.deferred, nsKey, nsName, status, details)
	}

	// Namespace devices, then the host-facing nvmet objects.
	for _, np := range plan.namespaces {
		status, details := s.probeNsDev(ctx, plan.rd, np)
		info.NsIdToDmLinear[np.nsId] = deferredSet(t, np.deferred,
			resKeyOf(resKeyNsDevFmt, np.nsId), np.devName, status, details)
	}
	for _, ssp := range plan.subsystems {
		status, details := s.probeExport(ctx, s.subsysConf(plan, ssp))
		info.SsIdToSubsystem[ssp.ssId] = t.Set(
			resKeyOf(resKeySubsysFmt, ssp.ssId), ssp.nqn, status, details)
		for _, np := range ssp.namespaces {
			// np.anaGrpId already carries [D15]'s fourth conjunct, so a deferred
			// namespace matches at ana_grpid = 3 and is healthy — it is only
			// not yet ready, which is what PROVISIONING says.
			nsStatus, nsDetails := s.probeNamespaceObject(
				ctx, s.nsConf(np, np.anaGrpId))
			info.NsIdToNamespace[np.nsId] = deferredSet(t, np.deferred,
				resKeyOf(resKeyNamespaceFmt, np.nsId),
				nsResName(ssp.nqn, np.nsIdx), nsStatus, nsDetails)
		}
	}
	return info
}

// cloneSourceLive reports whether every configured source endpoint has a live
// controller, the liveness half of the CN28 clone_id_to_target row
// (cloneTargetRow).
func cloneSourceLive(view *subsysView, cp *clonePlan) bool {
	for _, tr := range cp.clone.GetSrcTrConfList() {
		ctrl := view.ctrlOf(tr.GetTrAddr(), tr.GetTrSvcId())
		if ctrl == nil || ctrl.state != "live" {
			return false
		}
	}
	return true
}
