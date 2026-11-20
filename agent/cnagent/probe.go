package cnagent

import (
	"context"
	"fmt"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The probe map of CN28: the rows of a cntlr's read (refreshCntlr), read
// from the node's reading (readCntlr, takeReading) and from the reads that
// stay outside it: the status of each thin pool and dm-clone; the configfs
// of the exports; the attributes of the array the md walk matched to a group
// (Md.Detail) and
// that array's number (mdDevNo); on a primary, the md walk and array read of
// CN10's cut test (otherMemberInSync), taken afresh for each leg whose paths
// pathsToCut names, its answer deciding whether the verdict names them; the
// entry of each NVMe subsystem a lookup
// uses — its NQN once more, its namespace and its controllers
// (lookupSubsys) — and the number in /sys/block of a looked-up leg's
// multipath head, which the wrapper's name compares (wrapperName), and of a
// clone's source head and md clone meta group's array, which the dm-clone's
// name compares (cloneDmName); the own
// read of a dm name that a partial info listing, or
// the table listing, lacks (cnReading); the fresh `dmsetup info` that
// confirms a device gone after its own status or table read failed; and the
// superblock reads (Md.ReadMember) of the take-back check, of a repair's new
// leg taken from a retired leg and of the member that keeps the data
// (takeBackStep), and of the identity check, of a leg_list member the array
// lacks (legListAddRefusal), both through putBackNames, the re-assembly's
// listing of a broken array's holders and reads of every member the group
// keeps (reassemblyNamed), the reads of CN12's case selection for a group
// with no array, with its lsblk of the group's name for a create
// (absentArrayNamed, planAssembly), and the forced start's listing of the
// holders of an array of the group that does not run and reads of the
// members it keeps (forcedStartNamed, planForcedStart), which read those
// member devices. No
// function here creates, changes or removes an object of the node, as a
// cntlr's background read must not (CN23; dnagent.md SH29). A thin pool's
// `dmsetup status` carries the noflush flag and commits nothing, though it
// waits behind a commit already running on that pool; so does a dm-clone's,
// but one that finds the copy whole reads once more without the flag, which
// makes the kernel commit the clone's metadata (Dm.CloneStatus, dnagent.md
// SH17). In
// particular `reserve_metadata_snap`, a pool message that commits the pool,
// runs only inside `dumpThinMetadata` — the CN25 bitmap reads and CN14's thin
// id sweeps (the activation sweep and the sweep after a failed delete) —
// never from here (CN29).

// legSide is one endpoint of an NVMe subsystem, by the subsystem's NQN and
// the endpoint's transport, that a cntlr's verdict names (probeCntlr,
// sweepCntlr): a side of a leg, or an entry of a clone's src_tr_conf_list,
// with no controller that the converge would connect (sidesToConnect,
// srcToConnect), or, with cut set, a side whose path the converge would cut
// (pathsToCut).
type legSide struct {
	nqn string
	tr  *pb.NvmeTrConf
	cut bool
}

// probeCntlr writes the rows of one read of the cntlr from plan, whose
// reading readCntlr took (plan.rd, plan.walk, plan.mdWalks), and beside them
// what it reads of each group's running array (reportGrpReading). It also
// returns the dm names of the md members the converge would put back, add or
// remove for a repair, which the verdict names (putBackNames, cnagent.md
// CN30): they are read from the arrays and the leg rows the probe has just
// read, and from a lookup in the call's walk of each such member's leg and
// of the legs that could be its copy source. And
// it returns each provisioned side of a wanted leg that the converge would
// connect, the leg's lookup having answered (sidesToConnect, CN30), and each
// side whose live path the converge would cut (pathsToCut, CN10), which the
// verdict names too, and each entry of a clone source's src_tr_conf_list
// that the converge would connect (cloneSource.toName, CN30). It marks in
// the plan each leg wrapper, each RedundNone group's dm-linear and each
// dm-clone the converge would change (wrapperName, linearGroupName,
// cloneDmName), which the verdict names (cntlrRowVerdict). Among the md
// conditions it returns are also each absent array the converge's
// assembly would start (absentArrayNamed) and each forced start the
// converge would carry through, or the stop of an array it begins with
// (forcedStartNamed), the second as a failure,
// and as failures too each uncreated snapshot whose create message the
// converge's pre-pass would send (snapshotDue) and each group whose md check
// the converge would start, stop or adopt (mdCheckDue).
func (s *CnAgentServer) probeCntlr(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
) (*pb.CntlrInfo, []verdictName, []legSide) {
	t := st.tracker
	info := newCntlrInfo()
	var putBack []verdictName
	var noCtrl []legSide

	// Legs — the wrapper table plus the CN11 prober outcome on a primary,
	// transport liveness and ana_state per desired side on a standby. A
	// two-sided leg's path rows come first (legPathRows), and the lookup they
	// take serves a standby's leg row too.
	for _, lp := range plan.legs {
		key := resKeyOf(resKeyLegFmt, lp.legId)
		view, lookupErr := s.legPathRows(ctx, st, plan, lp, nil, info)
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
		// The sides the converge would connect (CN30), from one lookup of the
		// leg; a lookup that did not answer names none. A standby's leg row
		// reads the same lookup and says so. A primary's row is its prober's
		// (legInfo), and a one-sided leg whose row reads OK has a live path
		// through its one side, so it is not looked up: a primary with
		// thousands of legs would otherwise read each one's controllers on
		// every read.
		var row *pb.ResInfo
		if plan.primary {
			row = s.legInfo(ctx, st, plan, lp, view, lookupErr)
		}
		if view == nil && lookupErr == nil && (!plan.primary ||
			row.GetStatus() != pb.ResStatus_RES_STATUS_OK) {
			view, lookupErr = s.lookupSubsys(ctx, plan.walk, lp.nqn, sideNsid)
		}
		if lookupErr == nil && view != nil {
			lp.readView = view
			for _, side := range sidesToConnect(lp, view, plan.primary) {
				noCtrl = append(noCtrl, legSide{nqn: lp.nqn,
					tr: side.GetNvmeTrConf()})
			}
			// The leg's wrapper, which the verdict names while a converge
			// would create, reload or resume it (CN30). A primary's
			// one-sided leg whose row reads OK, not looked up, has a
			// wrapper of the leg's target and size there and resumed,
			// through which its last probe passed, so the head it maps
			// serves.
			lp.wrapperName = s.wrapperName(ctx, plan.rd, lp, view)
			cut := pathsToCut(plan.primary, lp, view)
			if len(cut) > 0 && !s.otherMemberInSync(ctx, lp) {
				cut = nil
			}
			for _, ctrl := range cut {
				for _, side := range lp.sides {
					tr := side.GetNvmeTrConf()
					if ctrl.trAddr == tr.GetTrAddr() &&
						ctrl.trSvcId == tr.GetTrSvcId() {
						noCtrl = append(noCtrl, legSide{nqn: lp.nqn,
							tr: tr, cut: true})
					}
				}
			}
		}
		if !plan.primary {
			row = s.legInfo(ctx, st, plan, lp, view, lookupErr)
		}
		info.LegIdToLeg[lp.legId] = row
	}

	// Groups — the reading's md walk serves all of them and the concat rows
	// below (Md.Detail, mdDevNo).
	var dead map[uint64]bool
	if plan.wantGrp {
		dead = s.deadLegs(st)
	}
	for _, gp := range plan.grps {
		key := resKeyOf(resKeyGrpFmt, gp.grpId)
		switch {
		case plan.wantGrp && gp.deferred:
			info.GrpIdToMdRaid[gp.grpId] = t.Provisioning(
				key, gp.resName, detailsProvisioning)
		case plan.wantGrp:
			status, details, detail, found := s.probeGroup(ctx, plan, gp)
			info.GrpIdToMdRaid[gp.grpId] = t.Set(
				key, gp.resName, status, details)
			if !gp.raid1 {
				// The verdict names a RedundNone group's dm-linear by the
				// group's row while a converge would change it (CN30).
				gp.linearName = s.linearGroupName(ctx, plan, gp)
			}
			names, refusals := s.putBackNames(
				ctx, plan, gp, detail, info.LegIdToLeg, dead)
			for _, name := range names {
				putBack = append(putBack, verdictName{
					kind: agent.LeftoverKindMdMember, name: name})
			}
			if broken := brokenArray(found); broken != nil &&
				s.reassemblyNamed(ctx, plan, gp, broken, info.LegIdToLeg) {
				putBack = append(putBack, verdictName{
					kind: agent.LeftoverKindMdBroken, name: broken.Dev})
			}
			// An absent array the converge's assembly would start, and a
			// forced start the converge would carry through, or the stop
			// of an array it begins with (CN30).
			named, conflict := s.absentArrayNamed(ctx, st, plan, gp, found)
			if named {
				putBack = append(putBack, verdictName{
					kind: agent.LeftoverKindMdAbsent, name: gp.resName})
			}
			// CN12, The identity check: the legs of another identity than
			// the running array's, and those of two identities an
			// assembly of the absent array would meet, marked as the
			// converge marks them.
			markLegRows(t, info, refusals.otherIdentity,
				pb.ResReason_RES_REASON_OTHER_IDENTITY)
			if conflict != nil {
				markLegRows(t, info, conflict.legIds,
					pb.ResReason_RES_REASON_IDENTITY_UNDECIDED)
			}
			if why := s.forcedStartNamed(
				ctx, st, plan, gp, found); why != nil {
				putBack = append(putBack, verdictName{
					name: fmt.Sprintf("forced start of group %#x", gp.grpId),
					err:  why})
			}
			noteMdCheck(st, gp, detail)
			if mdCheckDue(st, gp, info.GrpIdToMdRaid[gp.grpId], detail) {
				putBack = append(putBack, verdictName{
					name: fmt.Sprintf("md check of group %#x", gp.grpId),
					err:  errMdCheckDue})
			}
			reportGrpReading(info, gp, detail, refusals, actedMark(st, gp),
				mdCheckDone(st, gp))
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
		status, details, reason := s.probePool(ctx, plan, sp)
		info.SliceIdToDmPool[sp.sliceId] = t.SetReason(
			poolKey, sp.poolFinalName, status, details, reason)
	}

	// An uncreated snapshot whose create message the converge's pre-pass
	// would send, by the pool rows above (CN14, CN30).
	for _, tp := range plan.tds {
		if s.snapshotDue(ctx, plan, tp, info) {
			putBack = append(putBack, verdictName{
				name: fmt.Sprintf("snapshot td %#x", tp.tdId),
				err:  errSnapshotDue})
		}
	}

	// Thin volumes.
	for _, tp := range plan.tds {
		if !plan.primary {
			// A torn mark is reported in any role, in an entry with no rows,
			// as the converge reports it (tornMark, CN14).
			if tornMark(st, tp) {
				info.TdIdToThinInfo[tp.tdId] = &pb.CntlrInfo_ThinInfo{
					SliceIdToDmThin: map[uint64]*pb.ResInfo{},
					Torn:            true,
				}
			}
			continue
		}
		// The torn mark is the converge's (tornMark, CN14); no row carries
		// the lost thin id's reason, nor the reason of a thin id the agent
		// does not adopt (CN29): only a converge names either.
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
	// the converge reports for the same node; and from the same look, what
	// the verdict names (CN30): the source's endpoints step 1 would connect
	// (cloneSource.toName) and the dm-clone while step 3's create, reload or
	// resume, or steps 4 and 5, would act on it (cloneDmName).
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
		src := s.readCloneSource(ctx, plan, cp)
		dm := s.probeCloneDm(ctx, plan.rd, cp)
		s.cloneRows(st, cp, info, src, func() cloneDmRead { return dm }, "")
		for _, tr := range src.toName(cp) {
			noCtrl = append(noCtrl, legSide{nqn: cp.clone.GetSrcNqn(), tr: tr})
		}
		cp.dmName = s.cloneDmName(ctx, plan, cp, src, info, dm)
	}

	if !plan.wantAny {
		// SP_LEVEL_DISABLE. The transfer, ns-dev, namespace and subsystem rows
		// below are all suppressed — and CN19 wants that *said*, through the
		// very helper the converge uses, so the two channels report the same
		// CntlrInfo for the same stored request (architecture.md, Check
		// streams: show_info always fills the complete current info).
		s.reportSuppressed(st, plan, info)
		return info, putBack, noCtrl
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
			xp.safe = true
			continue
		}
		// The subsystem first: whether it is safe decides the device's row
		// of a namespace its move could not make inaccessible (CN33).
		ssStatus, ssDetails, safe := s.readExport(
			ctx, s.xferSubsysConf(plan, xp))
		xp.safe = safe
		status, details := s.probeXferDm(ctx, plan, xp)
		nsIdx := int(xp.xfer.GetOriNsIdx())
		if status != pb.ResStatus_RES_STATUS_OK && plan.xferServed(xp) &&
			!xp.safe {
			// As the converge's device step decides it (ensureXfer): off
			// the linear over the origin, not suspended on it, and its
			// namespace not inaccessible.
			off, err := s.xferOffOrigin(ctx, plan.rd, xp)
			if err == nil && off && s.isDark(ctx, xp.nqn, nsIdx) != nil {
				status, details = pb.ResStatus_RES_STATUS_ERROR,
					notDarkDetails(xp.nqn, nsIdx)
			}
		}
		info.XferIdToDmLinear[xp.xferId] = deferredSet(
			t, xp.deferred, dmKey, xp.finalName, status, details)
		info.XferIdToSubsystem[xp.xferId] = deferredSet(
			t, xp.deferred, ssKey, xp.nqn, ssStatus, ssDetails)
		// The group CN17 wants for this reading's table of the transfer
		// device, as the converge compares it (xferRowAnaGrpId).
		status, details, reason := s.probeNamespaceObject(ctx, s.xferNsConf(
			plan, xp, s.xferRowAnaGrpId(ctx, plan.rd, plan, xp)))
		info.XferIdToNamespace[xp.xferId] = deferredSetReason(
			t, xp.deferred, nsKey, nsName, status, details, reason)
	}

	// Whether each subsystem is safe, read first (readExport, CN33): it
	// decides the rows of its ns-devs and namespaces.
	type ssRow struct {
		status  pb.ResStatus
		details string
	}
	ssRows := make(map[uint64]ssRow, len(plan.subsystems))
	for _, ssp := range plan.subsystems {
		var row ssRow
		row.status, row.details, ssp.safe = s.readExport(
			ctx, s.subsysConf(plan, ssp))
		ssRows[ssp.ssId] = row
	}
	// Namespace devices, then the host-facing nvmet objects.
	for _, np := range plan.namespaces {
		status, details := s.probeNsDev(ctx, plan.rd, np)
		info.NsIdToDmLinear[np.nsId] = deferredSetReason(t, np.deferred,
			resKeyOf(resKeyNsDevFmt, np.nsId), np.devName, status, details,
			nsDevReason(status, details))
	}
	for _, ssp := range plan.subsystems {
		// The subsystem's row, read above with whether it is safe; its
		// namespaces' rows want the inaccessible group while it is not.
		row := ssRows[ssp.ssId]
		info.SsIdToSubsystem[ssp.ssId] = t.Set(
			resKeyOf(resKeySubsysFmt, ssp.ssId), ssp.nqn, row.status,
			row.details)
		for _, np := range ssp.namespaces {
			// np.anaGrpId already carries [D15]'s fourth conjunct, so a deferred
			// namespace matches at ana_grpid = 3 and is healthy — it is only
			// not yet ready, which is what PROVISIONING says.
			// The group CN16 wants for this reading's table of the ns-dev,
			// as the converge compares it (nsRowAnaGrpId).
			nsStatus, nsDetails, nsReason := s.probeNamespaceObject(
				ctx, s.nsConf(np, s.nsRowAnaGrpId(ctx, plan.rd, np)))
			info.NsIdToNamespace[np.nsId] = deferredSetReason(t,
				np.deferred, resKeyOf(resKeyNamespaceFmt, np.nsId),
				nsResName(ssp.nqn, np.nsIdx), nsStatus, nsDetails, nsReason)
		}
	}
	return info, putBack, noCtrl
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

// verdictName is one condition a read's verdict names beside the leftovers
// (CN30): its leftover kind and its name, or, with err set, a failure, err
// naming the cause after the name (forcedStartNamed, mdCheckDue,
// snapshotDue).
type verdictName struct {
	kind string
	name string
	err  error
}

// reassemblyNamed reports whether a read's verdict names the group's broken
// array (CN30): the converge would try CN12's re-assembly (reassembles) and
// its refusal finds nothing against it, read with the read's leg rows and,
// only then, legServes for a member's leg, as copySourceLeg's test. A read
// that did not answer names nothing; the next read reads again.
func (s *CnAgentServer) reassemblyNamed(
	ctx context.Context,
	plan *cntlrPlan,
	gp *grpPlan,
	broken *MdDetail,
	legRows map[uint64]*pb.ResInfo,
) bool {
	if !reassembles(gp, broken) {
		return false
	}
	why, err := s.reassemblyRefusal(ctx, plan, gp, broken,
		func(lp *legPlan) bool {
			return legRows[lp.legId].GetStatus() !=
				pb.ResStatus_RES_STATUS_ERROR && s.legServes(ctx, plan, lp)
		})
	return err == nil && why == ""
}
