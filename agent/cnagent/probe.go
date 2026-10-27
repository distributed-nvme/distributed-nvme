package cnagent

import (
	"context"
	"fmt"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The probe map of CN28: the rows of a cntlr's read (refreshCntlr), read
// from the node's reading (readCntlr, takeReading) and from the reads that
// stay outside it: the status of each thin pool and dm-clone; the configfs
// of the exports; the attributes of the array the md walk matched to a group
// (Md.Detail) and
// that array's number (mdDevNo); the entry of each NVMe subsystem a lookup
// uses — its NQN once more, its namespace and its controllers
// (lookupSubsys); the own read of a dm name that a partial info listing, or
// the table listing, lacks (cnReading); the fresh `dmsetup info` that
// confirms a device gone after its own status or table read failed; and the
// take-back check's `mdadm --examine` of a repair's new leg taken from a
// retired leg and of the member that keeps the data (takeBackStep, through
// putBackNames), which reads those member devices. No
// function here creates, changes or removes an object of the node, as a
// cntlr's background read must not (CN23; dnagent.md SH29). A thin pool's
// `dmsetup status` carries the noflush flag and commits nothing, though it
// waits behind a commit already running on that pool; a dm-clone's still
// makes the kernel commit the clone's metadata (dnagent.md SH17). In
// particular `reserve_metadata_snap`, a pool message that commits the pool,
// runs only inside `dumpThinMetadata` — the CN25 bitmap reads and CN14's thin
// id sweeps (the activation sweep and the sweep after a failed delete) —
// never from here (CN29).

// probeCntlr writes the rows of one read of the cntlr from plan, whose
// reading readCntlr took (plan.rd, plan.walk, plan.mdWalks), and beside them
// what it reads of each group's running array (reportGrpReading). It also
// returns the dm names of the md members the converge would put back, add or
// remove for a repair, which the verdict names (putBackNames, cnagent.md
// CN30): they are read from the arrays and the leg rows the probe has just
// read, and from a lookup of each such member's leg in the call's walk.
func (s *CnAgentServer) probeCntlr(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
) (*pb.CntlrInfo, []string) {
	t := st.tracker
	info := newCntlrInfo()
	var putBack []string

	// Legs — the wrapper table plus the CN11 prober outcome on a primary,
	// transport liveness and ana_state per desired side on a standby. A
	// two-sided leg's path rows come first (legPathRows), and the lookup they
	// take serves a standby's leg row too.
	for _, lp := range plan.legs {
		key := resKeyOf(resKeyLegFmt, lp.legId)
		view := s.legPathRows(ctx, st, plan, lp, nil, info)
		// CN19's suppression is evaluated first and wins over [D15]'s deferral:
		// the operator has said the resource must not exist, which is a
		// stronger statement than "it is coming".
		if !plan.wantLeg {
			info.LegIdToLeg[lp.legId] = suppressedRow(t, key, lp.name)
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
		// would flip against the converge's, and move its epoch, on every
		// read while the disconnect runs.
		if s.disconnectInFlight(lp.nqn) {
			info.LegIdToLeg[lp.legId] = t.Err(
				key, lp.name, detailsDisconnectInFlight(lp.nqn))
			continue
		}
		info.LegIdToLeg[lp.legId] = s.legInfo(ctx, st, plan, lp, view)
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
			status, details, detail := s.probeGroup(ctx, plan, gp)
			info.GrpIdToMdRaid[gp.grpId] = t.Set(
				key, gp.resName, status, details)
			names, refusal := s.putBackNames(
				ctx, plan, gp, detail, info.LegIdToLeg)
			putBack = append(putBack, names...)
			reportGrpReading(info, gp, detail, refusal)
		case plan.primary:
			info.GrpIdToMdRaid[gp.grpId] = suppressedRow(t, key, gp.resName)
		}
	}

	// Pool concats and thin-pools.
	for _, sp := range plan.slices {
		metaKey := resKeyOf(resKeyPoolMetaFmt, sp.sliceId)
		dataKey := resKeyOf(resKeyPoolDataFmt, sp.sliceId)
		poolKey := resKeyOf(resKeyPoolFmt, sp.sliceId)
		if !plan.wantPool {
			if plan.primary {
				info.SliceIdToMeta[sp.sliceId] = suppressedRow(t,
					metaKey, sp.poolMetaName)
				info.SliceIdToData[sp.sliceId] = suppressedRow(t,
					dataKey, sp.poolDataName)
				info.SliceIdToDmPool[sp.sliceId] = suppressedRow(t,
					poolKey, sp.poolFinalName)
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
		// The torn mark is the converge's (tornMark, CN14); no row carries
		// the lost thin id's reason (CN29): only a converge names one.
		thinInfo := &pb.CntlrInfo_ThinInfo{
			SliceIdToDmThin: make(map[uint64]*pb.ResInfo, len(plan.slices)),
			Torn:            tornMark(st, tp),
		}
		for _, sp := range plan.slices {
			key := thinResKey(tp.tdId, sp.sliceId)
			name := plan.thinName(tp.tdId, sp.sliceId)
			if !plan.wantPool {
				thinInfo.SliceIdToDmThin[sp.sliceId] = suppressedRow(t,
					key, name)
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
			// too — a row that disappears one read after being reported
			// MISSING is exactly the omitted-vs-never-looked-at ambiguity
			// CN19 forbids.
			info.TdIdToDmError[tp.tdId] = suppressedRow(t,
				resKeyOf(resKeyTdErrorFmt, tp.tdId), tp.errorName)
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
			info.TdIdToRaid0[tp.tdId] = suppressedRow(t, raid0Key, tp.raid0Name)
		}
	}

	// Clones: the rows of CN18's shared reading (cloneRows), from this
	// read's look at the source and at the dm-clone, so a read reports what
	// the converge reports for the same node.
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
			// probed.
			s.reportCloneNoDst(st, cp, info)
			continue
		}
		if cp.grp == nil {
			// The same CN18 error the converge reports (reportCloneNoGrp).
			s.reportCloneNoGrp(st, cp, info)
			continue
		}
		// A clone whose dst or clone meta group is merely still provisioning
		// is deferred and builds nothing ([D15]).
		if cp.deferred {
			s.reportCloneDeferred(st, cp, info)
			continue
		}
		s.cloneRows(st, cp, info, s.readCloneSource(ctx, plan, cp),
			func() cloneDmRead { return s.probeCloneDm(ctx, plan.rd, cp) }, "")
	}

	if !plan.wantAny {
		// SP_LEVEL_DISABLE. The transfer, ns-dev, namespace and subsystem rows
		// below are all suppressed — and CN19 wants that *said*, through the
		// very helper the converge uses, so the two channels report the same
		// CntlrInfo for the same stored request (architecture.md, Check
		// streams: show_info always fills the complete current info).
		s.reportSuppressed(st, plan, info)
		return info, putBack
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
		// The group CN17 wants for this reading's table of the transfer
		// device, as the converge compares it (xferRowAnaGrpId).
		status, details = s.probeNamespaceObject(ctx, s.xferNsConf(
			plan, xp, s.xferRowAnaGrpId(ctx, plan.rd, plan, xp)))
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
			// The group CN16 wants for this reading's table of the ns-dev,
			// as the converge compares it (nsRowAnaGrpId).
			nsStatus, nsDetails := s.probeNamespaceObject(
				ctx, s.nsConf(np, s.nsRowAnaGrpId(ctx, plan.rd, np)))
			info.NsIdToNamespace[np.nsId] = deferredSet(t, np.deferred,
				resKeyOf(resKeyNamespaceFmt, np.nsId),
				nsResName(ssp.nqn, np.nsIdx), nsStatus, nsDetails)
		}
	}
	return info, putBack
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
