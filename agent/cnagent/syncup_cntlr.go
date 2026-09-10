package cnagent

import (
	"context"
	"log/slog"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// syncupCntlr implements CN8-CN20. The node read lock and this cntlr's object
// lock are held by the caller.
func (s *CnAgentServer) syncupCntlr(
	ctx context.Context,
	key string,
	req *pb.SyncupCntlrRequest,
) *pb.SyncupCntlrReply {
	// CN8 gating: SyncupCn introduces the pointer first (§9.1).
	cn := s.getCn(cnKey(req.GetClusterId(), req.GetCnId()))
	if cn == nil || !pointerKnown(cn.req, req.GetCntlrPointer()) {
		return &pb.SyncupCntlrReply{
			AgentReply: agent.UnknownObjectReply(
				"cntlr pointer %s not in the cn's list",
				cntlrPointerText(req.GetCntlrPointer())),
		}
	}
	st := s.getCntlr(key)
	var stored uint64
	if st != nil {
		stored = st.req.GetRevision()
	}
	if reject := agent.GateRevision(stored, req.GetRevision()); reject != nil {
		return &pb.SyncupCntlrReply{AgentReply: reject, Revision: stored}
	}
	if st == nil {
		st = newCntlrState(req)
	}
	st.req = req
	// U3: this request came through the revision gate, so GateRevision makes
	// it the newest desired state this cntlr has ever accepted and its
	// td_list is authoritative — the one copy the activation sweep may
	// delete against (update_05.md U3 point 4).
	st.reqFromRpc = true
	s.putCntlr(key, st)

	info := s.convergeCntlr(ctx, st)

	ptr := req.GetCntlrPointer()
	path := s.nf.LocalCntlrPath(req.GetClusterId(), req.GetCnId(),
		ptr.GetSpId(), ptr.GetCntlrId())
	if err := s.store.Save(ctx, path, req); err != nil {
		slog.ErrorContext(ctx, "persisting cntlr state failed",
			slog.String("path", path),
			slog.String("error", err.Error()))
	}
	return &pb.SyncupCntlrReply{
		AgentReply: agent.OkReply(),
		Revision:   req.GetRevision(),
		CntlrInfo:  info,
		BmInfoList: s.bitmapInfoList(st),
	}
}

// convergeCntlr is the CN9 converge pass: one retire phase top-down, then one
// build phase bottom-up. That phase order is what implements §11.1 without
// special cases — a primary→standby flip is nothing but "the desired set shrank
// to the standby shape", and standby→primary is "it grew".
//
// `migr_list` is carried in the request and read by nothing: the CN's whole
// part in a migration is that a leg's side_list temporarily holds two sides
// (CN10); the field stays reserved for a future consumer.
func (s *CnAgentServer) convergeCntlr(
	ctx context.Context,
	st *cntlrState,
) *pb.CntlrInfo {
	plan := newCntlrPlan(s.nf, st.req)
	info := newCntlrInfo()
	s.retire(ctx, st, plan, info)
	s.build(ctx, st, plan, info)
	st.applied = plan
	return info
}

func newCntlrInfo() *pb.CntlrInfo {
	return &pb.CntlrInfo{
		SsIdToSubsystem:   make(map[uint64]*pb.ResInfo),
		NsIdToNamespace:   make(map[uint64]*pb.ResInfo),
		NsIdToDmLinear:    make(map[uint64]*pb.ResInfo),
		TdIdToRaid0:       make(map[uint64]*pb.ResInfo),
		TdIdToDmError:     make(map[uint64]*pb.ResInfo),
		TdIdToThinInfo:    make(map[uint64]*pb.CntlrInfo_ThinInfo),
		SliceIdToDmPool:   make(map[uint64]*pb.ResInfo),
		SliceIdToMeta:     make(map[uint64]*pb.ResInfo),
		SliceIdToData:     make(map[uint64]*pb.ResInfo),
		GrpIdToMdRaid:     make(map[uint64]*pb.ResInfo),
		LegIdToLeg:        make(map[uint64]*pb.ResInfo),
		XferIdToDmLinear:  make(map[uint64]*pb.ResInfo),
		XferIdToSubsystem: make(map[uint64]*pb.ResInfo),
		XferIdToNamespace: make(map[uint64]*pb.ResInfo),
		CloneIdToTarget:   make(map[uint64]*pb.ResInfo),
		CloneIdToDmClone:  make(map[uint64]*pb.ResInfo),
		CloneIdToMeta:     make(map[uint64]*pb.ResInfo),
	}
}

// ---------------------------------------------------------------------------
// Retire phase, top-down (CN9)
// ---------------------------------------------------------------------------

// retire removes, top-down, everything the new desired state (level-adjusted,
// CN19) no longer wants. The step order **is** §11.1 old_primary steps 1-3:
// ANA first, then the ns-dev reloads onto dm-error (whose internal suspend
// flushes the in-flight IO), then the nvmet and dm removals, `mdadm --stop`,
// and outbound disconnects last.
func (s *CnAgentServer) retire(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	info *pb.CntlrInfo,
) {
	old := st.applied
	if !plan.wantAny {
		// SP_LEVEL_DISABLE: every cntlr-scoped object goes, but the store
		// files and the in-memory desired state stay (CN19) — which is the
		// one difference from the CN21 teardown below.
		if old != nil {
			s.teardownCntlrResources(ctx, st, old)
		}
		s.teardownCntlrResources(ctx, st, plan)
		s.dropAllResKeys(st, old)
		s.dropAllResKeys(st, plan)
		return
	}

	// (1) Every namespace leaving service moves to the inaccessible group.
	// U4 needs no case of its own here: a provisioning-deferred namespace's
	// anaGrpId is already inaccessible (CN16's fourth conjunct), so this loop
	// parks it on the very first converge of a fresh SP.
	for _, np := range plan.namespaces {
		if np.anaGrpId == common.AnaGrpIdInaccessible {
			s.setAnaLogged(ctx, np.ss.nqn, np.nsIdx)
		}
	}
	for _, xp := range plan.xfers {
		if plan.xferAnaGrpId(xp) == common.AnaGrpIdInaccessible {
			s.setAnaLogged(ctx, xp.nqn, int(xp.xfer.GetOriNsIdx()))
		}
	}
	for _, np := range removedNamespaces(old, plan) {
		s.setAnaLogged(ctx, np.ss.nqn, np.nsIdx)
	}
	for _, xp := range removedXfers(old, plan) {
		s.setAnaLogged(ctx, xp.nqn, int(xp.xfer.GetOriNsIdx()))
	}

	// (2) Every ns-dev that must stop serving is reloaded onto its dm-error.
	// A provisioning-deferred namespace is covered by the same test, because
	// CN16 rule 0 makes its backing the td's errorName (U4). The reload's
	// internal suspend is what flushes the in-flight IO. A
	// namespace whose *old* td is leaving `td_list` is parked too, even when
	// its new backing is a live raid0: its table still maps the departing
	// td's raid0, and the removal below would fail EBUSY behind it.
	for _, np := range plan.namespaces {
		if (np.td != nil && np.backingName == np.td.errorName) ||
			mapsRemovedTd(old, plan, np) {
			s.parkNsDevLogged(ctx, np)
		}
	}

	// (3) nvmet objects that must go entirely, then the ns-devs under them.
	// The ns-devs come off before the transfer and clone stacks they may
	// still map — the retire phase is strictly top-down.
	for _, ssp := range removedSubsystems(old, plan) {
		s.removeExport(ctx, ssp.nqn)
		st.tracker.Drop(resKeyOf(resKeySubsysFmt, ssp.ssId))
	}
	for _, np := range removedNamespaces(old, plan) {
		if plan.ssByNqn(np.ss.nqn) == nil {
			continue // its whole subsystem is gone already
		}
		if err := s.nvmet.RemoveNamespace(
			ctx, np.ss.nqn, np.nsIdx); err != nil {
			slog.ErrorContext(ctx, "removing nvmet namespace failed",
				slog.String("nqn", np.ss.nqn),
				slog.String("error", err.Error()))
		}
	}
	for _, np := range removedNamespaces(old, plan) {
		s.removeDm(ctx, np.devName)
		st.tracker.Drop(resKeyOf(resKeyNsDevFmt, np.nsId))
		st.tracker.Drop(resKeyOf(resKeyNamespaceFmt, np.nsId))
	}

	// (4) transfers: first demote every device this cntlr no longer serves,
	// then remove the ones that left `xfer_list`. The demotion is what makes
	// the rest of the retire possible — a CnXferFinalName still mapping the
	// origin td's raid0 holds it open, so the raid0's removal in step (6)
	// would fail EBUSY and take the thin volumes, the pool, the concats and
	// `mdadm --stop` down with it (CN17, CN19's NO_THINPOOL row).
	for _, xp := range plan.xfers {
		if plan.xferServed(xp) {
			continue
		}
		s.demoteXfer(ctx, old, xp)
	}
	for _, xp := range retiredXfers(old, plan) {
		s.removeXfer(ctx, xp)
		s.dropXferKeys(st, xp.xferId)
	}

	// (5) clones, in the strict CN18 order (ns-devs off the dm-clone, the
	// dm-clone before its source connection dies, then the metadata wrapper).
	for _, cp := range retiredClones(old, plan) {
		s.retireClone(ctx, st, plan, cp)
	}

	// (6) per-td devices. A td that left td_list is **deleted**: its thin
	// volume ids go back to the pool. Everything else is only deactivated.
	for _, tp := range removedTds(old, plan) {
		s.removeDm(ctx, tp.raid0Name)
		s.removeDm(ctx, tp.errorName)
		for _, sp := range plan.slices {
			s.removeDm(ctx, plan.thinName(tp.tdId, sp.sliceId))
			if plan.wantPool {
				s.deleteThinId(ctx, sp, tp.td.GetDevId())
			}
		}
		s.dropTdKeys(st, plan, tp)
	}
	if !plan.wantPool {
		for _, tp := range unionTds(old, plan) {
			s.removeDm(ctx, tp.raid0Name)
			for _, sp := range unionSlices(old, plan) {
				s.removeDm(ctx, plan.thinName(tp.tdId, sp.sliceId))
			}
		}
	}
	// (7) pool concats and thin-pools.
	for _, sp := range retiredSlices(old, plan) {
		s.removeDm(ctx, sp.poolFinalName)
		s.removeDm(ctx, sp.poolMetaName)
		s.removeDm(ctx, sp.poolDataName)
		st.tracker.Drop(resKeyOf(resKeyPoolFmt, sp.sliceId))
		st.tracker.Drop(resKeyOf(resKeyPoolMetaFmt, sp.sliceId))
		st.tracker.Drop(resKeyOf(resKeyPoolDataFmt, sp.sliceId))
		// The pool device's life ends here, so its arming does too: a later
		// re-creation re-arms on its own Create branch, and dropping the
		// entry keeps the map from carrying dead slices (update_05.md U3).
		delete(st.pendingSweep, sp.sliceId)
	}

	// (8) group devices — `mdadm --stop` for arrays, `dmsetup remove` for
	// RedundNone linears.
	for _, gp := range retiredGrps(old, plan) {
		s.removeGroup(ctx, gp)
		st.tracker.Drop(resKeyOf(resKeyGrpFmt, gp.grpId))
	}

	// (9) probers, then the outbound disconnects, then the leg wrappers, last
	// — the CN21 order of update_01.md U2 spec 4 (removeLeg).
	wantedProbers := make(map[uint64]struct{})
	if plan.primary && plan.wantLeg {
		for _, lp := range plan.legs {
			if lp.provisioning {
				continue // U4: no wrapper to probe, so no prober
			}
			wantedProbers[lp.legId] = struct{}{}
		}
	}
	s.stopLegProbers(st, wantedProbers)
	for _, lp := range retiredLegs(old, plan) {
		s.removeLeg(ctx, lp)
		st.tracker.Drop(resKeyOf(resKeyLegFmt, lp.legId))
	}
}

// setAnaLogged moves a namespace to the inaccessible group, probe-first.
func (s *CnAgentServer) setAnaLogged(
	ctx context.Context,
	nqn string,
	nsIdx int,
) {
	if err := s.setAna(
		ctx, nqn, nsIdx, common.AnaGrpIdInaccessible); err != nil {
		slog.ErrorContext(ctx, "ana transition failed",
			slog.String("nqn", nqn),
			slog.String("error", err.Error()))
	}
}

func (s *CnAgentServer) parkNsDevLogged(ctx context.Context, np *nsPlan) {
	if err := s.parkNsDev(ctx, np); err != nil {
		slog.ErrorContext(ctx, "parking a namespace on dm-error failed",
			slog.String("dm", np.devName),
			slog.String("error", err.Error()))
	}
}

// demoteXfer reloads a live transfer device onto an error table of its own
// size — the CN17 shape for a cntlr that does not serve it. The size falls
// back to the previous plan's when the origin namespace has meanwhile gone,
// so a transfer whose origin td was deleted in the same request still lets go
// of that td's raid0.
func (s *CnAgentServer) demoteXfer(
	ctx context.Context,
	old *cntlrPlan,
	xp *xferPlan,
) {
	dev, err := s.dm.Info(ctx, xp.finalName)
	if err != nil || dev == nil {
		return
	}
	sectors := xp.sectors
	if sectors == 0 && old != nil {
		if prev := old.xferById[xp.xferId]; prev != nil {
			sectors = prev.sectors
		}
	}
	if sectors == 0 {
		return
	}
	if err := s.ensureDmError(ctx, xp.finalName, sectors); err != nil {
		slog.ErrorContext(ctx, "demoting a transfer device failed",
			slog.String("dm", xp.finalName),
			slog.String("error", err.Error()))
	}
}

// mapsRemovedTd reports whether a surviving namespace's live table still maps
// a td that is leaving `td_list` — an `UpdateNamespaceDev` repoint and the
// origin's deletion arriving in one full sync.
func mapsRemovedTd(old, plan *cntlrPlan, np *nsPlan) bool {
	if old == nil {
		return false
	}
	for _, prev := range old.namespaces {
		if prev.nsId != np.nsId {
			continue
		}
		return prev.td != nil && plan.tdById[prev.td.tdId] == nil
	}
	return false
}

func (s *CnAgentServer) dropTdKeys(
	st *cntlrState,
	plan *cntlrPlan,
	tp *tdPlan,
) {
	st.tracker.Drop(resKeyOf(resKeyRaid0Fmt, tp.tdId))
	st.tracker.Drop(resKeyOf(resKeyTdErrorFmt, tp.tdId))
	for _, sp := range plan.slices {
		st.tracker.Drop(thinResKey(tp.tdId, sp.sliceId))
	}
}

// ---------------------------------------------------------------------------
// Build phase, bottom-up (CN9) — §11.1 new_primary steps 1-4
// ---------------------------------------------------------------------------

func (s *CnAgentServer) build(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	info *pb.CntlrInfo,
) {
	retryNeeded := false

	// Legs (CN10).
	var available map[uint64]bool
	if plan.wantLeg {
		available, retryNeeded = s.ensureLegs(ctx, st, plan, info)
	} else {
		for _, lp := range plan.legs {
			info.LegIdToLeg[lp.legId] = st.tracker.Missing(
				resKeyOf(resKeyLegFmt, lp.legId), lp.name, detailsSpLevel)
		}
	}

	// Groups (CN12) — a standby has none (§3.4).
	if plan.wantGrp {
		for _, gp := range plan.grps {
			if gp.deferred {
				// U4: a leg of leg_list is still provisioning, so there is no
				// md array and no CnGrpName to build, and the group is not a
				// pool concat target either.
				info.GrpIdToMdRaid[gp.grpId] = st.tracker.Provisioning(
					resKeyOf(resKeyGrpFmt, gp.grpId), gp.resName,
					detailsProvisioning)
				continue
			}
			err := s.ensureGroup(ctx, gp, available)
			info.GrpIdToMdRaid[gp.grpId] = st.tracker.FromErr(
				resKeyOf(resKeyGrpFmt, gp.grpId), gp.resName, "", err)
		}
	} else if plan.primary {
		for _, gp := range plan.grps {
			info.GrpIdToMdRaid[gp.grpId] = st.tracker.Missing(
				resKeyOf(resKeyGrpFmt, gp.grpId), gp.resName, detailsSpLevel)
		}
	}

	// Per-slice pools (CN13) and thin volumes (CN14).
	poolReady := make(map[uint64]bool, len(plan.slices))
	if plan.wantPool {
		for _, sp := range plan.slices {
			if sp.deferred {
				// U4: every group on one side of this slice is deferred — the
				// initial CreateStoragePool shape. Nothing is built, and
				// nothing is wrong: an empty concat would be an error, and
				// PROVISIONING must never feed err_epoch.
				s.reportSliceDeferred(st, sp, info)
				continue
			}
			poolReady[sp.sliceId] = s.ensureSlice(ctx, st, plan, sp, info)
		}
		// U3: CN14's activation sweep, for every slice whose pool device this
		// incarnation just created (update_05.md U3 points 2 and 4). A
		// startup reconcile arms and skips — its persisted request may lag
		// the pool, and deleting against a lagging td_list would destroy a
		// live td; the first post-boot SyncupCntlr then finds the flag still
		// set — its own ensurePool probe-matches the surviving device and
		// arms nothing — and sweeps with the fresh, revision-gated td_list.
		// Sweeping here, before the pre-pass and the thin loop below, also
		// frees the strays' blocks before anything allocates, and a stray can
		// never collide with a new id (dev_ids are never reused).
		for _, sp := range plan.slices {
			if !poolReady[sp.sliceId] {
				continue
			}
			if st.reqFromRpc && st.pendingSweep[sp.sliceId] {
				s.sweepThinIds(ctx, st, plan, sp)
			}
		}
	} else if plan.primary {
		for _, sp := range plan.slices {
			info.SliceIdToMeta[sp.sliceId] = st.tracker.Missing(
				resKeyOf(resKeyPoolMetaFmt, sp.sliceId), sp.poolMetaName,
				detailsSpLevel)
			info.SliceIdToData[sp.sliceId] = st.tracker.Missing(
				resKeyOf(resKeyPoolDataFmt, sp.sliceId), sp.poolDataName,
				detailsSpLevel)
			info.SliceIdToDmPool[sp.sliceId] = st.tracker.Missing(
				resKeyOf(resKeyPoolFmt, sp.sliceId), sp.poolFinalName,
				detailsSpLevel)
		}
	}
	// U1: the snapshot-message pre-pass. Every needed slice's `create_snap`
	// goes out inside ONE suspension of the origin td's raid0, so the
	// per-slice snapshots all describe the same instant instead of one per
	// message. It has to sit exactly here: after the loop that fills
	// poolReady, and before the thin loop — only the *messages* belong inside
	// the window, because a snapshot's content is fixed at message time and
	// its thin devices are built afterwards by the unchanged loop below.
	if plan.wantPool {
		// plan.wantPool implies plan.primary, so no role test is needed.
		for _, tp := range plan.tds {
			// U4-S1: a created snapshot's ids are in every slice pool
			// already, so there is nothing to message and nothing to
			// quiesce for it — the thin loop's `dmsetup create` is the
			// whole job.
			if tp.td.GetOriId() == 0 || tp.td.GetCreated() {
				continue
			}
			s.snapshotPrePass(ctx, plan, tp, poolReady)
		}
	}
	for _, tp := range plan.tds {
		if !plan.primary {
			continue
		}
		thinInfo := &pb.CntlrInfo_ThinInfo{
			SliceIdToDmThin: make(map[uint64]*pb.ResInfo, len(plan.slices)),
		}
		for _, sp := range plan.slices {
			key := thinResKey(tp.tdId, sp.sliceId)
			name := plan.thinName(tp.tdId, sp.sliceId)
			switch {
			case !plan.wantPool:
				thinInfo.SliceIdToDmThin[sp.sliceId] = st.tracker.Missing(
					key, name, detailsSpLevel)
			case sp.deferred:
				// U4: no pool, so no thin volume in this slice.
				thinInfo.SliceIdToDmThin[sp.sliceId] = st.tracker.Provisioning(
					key, name, detailsProvisioning)
			case !poolReady[sp.sliceId]:
				thinInfo.SliceIdToDmThin[sp.sliceId] = st.tracker.Missing(
					key, name, "thin pool missing")
			default:
				err := s.ensureThin(ctx, plan, tp, sp)
				thinInfo.SliceIdToDmThin[sp.sliceId] = st.tracker.FromErr(
					key, name, "", err)
			}
		}
		info.TdIdToThinInfo[tp.tdId] = thinInfo
	}

	// Per-td raid0 and the permanent dm-error reload target (CN15).
	for _, tp := range plan.tds {
		if plan.wantAny {
			err := s.ensureDmError(ctx, tp.errorName, tp.sectors)
			info.TdIdToDmError[tp.tdId] = st.tracker.FromErr(
				resKeyOf(resKeyTdErrorFmt, tp.tdId), tp.errorName, "", err)
		} else {
			info.TdIdToDmError[tp.tdId] = st.tracker.Missing(
				resKeyOf(resKeyTdErrorFmt, tp.tdId), tp.errorName,
				detailsSpLevel)
		}
		switch {
		case plan.wantPool && tp.deferred:
			// U4: a slice's thin volume does not exist, so the stripe cannot
			// be assembled. The dm-error above is built all the same — it is
			// what backs the td's ns-devs meanwhile (CN16).
			info.TdIdToRaid0[tp.tdId] = st.tracker.Provisioning(
				resKeyOf(resKeyRaid0Fmt, tp.tdId), tp.raid0Name,
				detailsProvisioning)
		case plan.wantPool:
			err := s.ensureRaid0(ctx, plan, tp)
			info.TdIdToRaid0[tp.tdId] = st.tracker.FromErr(
				resKeyOf(resKeyRaid0Fmt, tp.tdId), tp.raid0Name, "", err)
		case plan.primary:
			info.TdIdToRaid0[tp.tdId] = st.tracker.Missing(
				resKeyOf(resKeyRaid0Fmt, tp.tdId), tp.raid0Name,
				detailsSpLevel)
		}
	}

	// Clones (CN18), then transfers (CN17).
	if plan.wantClone {
		for _, cp := range plan.clones {
			if s.ensureClone(ctx, st, plan, cp, info) {
				retryNeeded = true
			}
		}
	} else if plan.primary {
		for _, cp := range plan.clones {
			s.reportCloneSuppressed(st, cp, info)
		}
	}
	if plan.wantAny {
		for _, xp := range plan.xfers {
			s.ensureXfer(ctx, st, plan, xp, info)
		}
	}

	// ns-devs and the host-facing nvmet objects (CN16).
	if plan.wantAny {
		for _, np := range plan.namespaces {
			err := s.ensureNsDev(ctx, np)
			// A deferred namespace is built exactly as the effective plan
			// wants it — on the td's dm-error — but nothing under it can
			// serve, so it reports PROVISIONING rather than OK (U4).
			info.NsIdToDmLinear[np.nsId] = deferredFromErr(
				st.tracker, np.deferred,
				resKeyOf(resKeyNsDevFmt, np.nsId), np.devName,
				nsDevDetails(np), err)
		}
		for _, ssp := range plan.subsystems {
			s.ensureSubsystem(ctx, st, plan, ssp, info)
		}

		// ANA rewrites to optimized last — §11.1 new_primary step 4.
		for _, np := range plan.namespaces {
			if np.anaGrpId == common.AnaGrpIdInaccessible {
				continue
			}
			if err := s.setAna(
				ctx, np.ss.nqn, np.nsIdx, np.anaGrpId); err != nil {
				slog.ErrorContext(ctx, "ana transition failed",
					slog.String("nqn", np.ss.nqn),
					slog.String("error", err.Error()))
			}
		}
		for _, xp := range plan.xfers {
			grpId := plan.xferAnaGrpId(xp)
			if grpId == common.AnaGrpIdInaccessible {
				continue
			}
			if err := s.setAna(ctx, xp.nqn,
				int(xp.xfer.GetOriNsIdx()), grpId); err != nil {
				slog.ErrorContext(ctx, "ana transition failed",
					slog.String("nqn", xp.nqn),
					slog.String("error", err.Error()))
			}
		}
	} else {
		// SP_LEVEL_DISABLE. Everything cntlr-scoped is gone, and CN19 wants
		// that said rather than left out: a worker cannot tell an omitted
		// resource from one the agent never looked at.
		s.reportSuppressed(st, plan, info)
	}

	if !retryNeeded {
		s.stopConnectRetry(st)
	}
}

// snapshotPrePass sends one snapshot td's per-slice `create_snap` messages
// inside a single quiesce of the origin td's raid0 (CN14, update_02.md U1).
// Suspending the raid0 flushes every in-flight host write and holds new ones
// for the duration of the messages, which is what makes the per-slice
// snapshots point-in-time with respect to each other; each message keeps its
// own nested per-slice origin-thin suspend, which is dm-thin's own documented
// requirement and a separate thing.
//
// It owns *every* message of an uncreated snapshot (U4-S3): ensureThin never
// messages a td with ori_id != 0, so a slice this pass declines — or one
// whose message failed — simply waits for the next converge, which is still
// driven by the same `created == false`. A failed message is tolerated
// exactly as before: the pool may already hold the dev_id after a crashed
// earlier pass, and the `dmsetup create` that follows decides the outcome.
func (s *CnAgentServer) snapshotPrePass(
	ctx context.Context,
	plan *cntlrPlan,
	tp *tdPlan,
	poolReady map[uint64]bool,
) {
	var need []*slicePlan
	for _, sp := range plan.slices {
		// poolReady only ever holds an entry for a non-deferred slice under
		// plan.wantPool, so it already subsumes the deferred and
		// pool-missing arms of the thin loop's switch. It is deliberately
		// NOT tp.deferred: that flag is plan-global (anySliceDeferred), and
		// gating the messages on it would drop create_snap for every ready
		// slice whenever one sibling slice is still provisioning.
		if !poolReady[sp.sliceId] {
			continue
		}
		// The same trigger ensureThin uses. An Info error means "skip":
		// ensureThin fails that slice with the same error anyway.
		//
		// U4-S3: there is no second filter on the origin's own thin device.
		// The gateway refuses a snapshot of an origin that is not materialized
		// in every slice pool (§8.7), so `create_snap` can no longer be
		// inverted with the origin's `create_thin` — and whether *this* CN has
		// built the origin's dm device is irrelevant to a message the pool
		// metadata answers.
		dev, err := s.dm.Info(ctx, plan.thinName(tp.tdId, sp.sliceId))
		if err != nil || dev != nil {
			continue
		}
		need = append(need, sp)
	}
	if len(need) == 0 {
		// SH16: an equal-revision re-apply must not suspend anything. Every
		// converge round would otherwise stall the origin's host IO.
		return
	}
	// ori_id is the origin's **dev_id**, not its td_id. An origin absent from
	// the plan means there is nothing to quiesce — not that the messages are
	// somebody else's job. suspendSnapOrigin itself returns "" for an origin
	// whose raid0 is not live, which covers a fresh primary and a raid0
	// somebody else holds suspended. A deferred origin is NOT one of those:
	// the deferred arm of the raid0 loop below suppresses the converge, not
	// the device, so a raid0 an earlier revision built is still live and is
	// quiesced like any other.
	if origin := plan.tdByDevId[tp.td.GetOriId()]; origin != nil {
		if raid0 := s.suspendSnapOrigin(ctx, origin); raid0 != "" {
			defer func() {
				// [D12]: the resume runs on every path out of the sequence —
				// a failed message, a timeout, an early return. It also has
				// to survive a cancelled RPC ctx, or a client that
				// disconnects mid-pass leaves the origin's raid0 suspended,
				// stalling its host IO and wedging in D state any scanner
				// that opens it.
				rctx := context.WithoutCancel(ctx)
				if err := s.dm.Resume(rctx, raid0); err != nil {
					slog.ErrorContext(ctx,
						"resuming the snapshot origin raid0 failed",
						slog.String("raid0", raid0),
						slog.String("error", err.Error()))
				}
			}()
		}
	}
	for _, sp := range need {
		if err := s.createSnapId(ctx, plan, tp, sp); err != nil {
			slog.ErrorContext(ctx, "thin-pool create message failed",
				slog.String("pool", sp.poolFinalName),
				slog.String("thin", plan.thinName(tp.tdId, sp.sliceId)),
				slog.String("error", err.Error()))
		}
	}
}

// suspendSnapOrigin quiesces the origin td's raid0 and returns its name, or
// "" when it did not suspend it — no live raid0 (a standby, or a stack this
// cntlr has not built yet), or one somebody else already holds suspended.
// Liveness is the whole test: a td the plan defers keeps whatever raid0 an
// earlier revision built, and that one is quiesced. "" is the caller's
// signal that it owns no resume: un-suspending a device another operation is
// holding would be silently destructive.
func (s *CnAgentServer) suspendSnapOrigin(
	ctx context.Context,
	origin *tdPlan,
) string {
	dev, err := s.dm.Info(ctx, origin.raid0Name)
	if err != nil || dev == nil || dev.Suspended {
		return ""
	}
	if err := s.dm.Suspend(ctx, origin.raid0Name); err != nil {
		slog.ErrorContext(ctx, "suspending the snapshot origin raid0 failed",
			slog.String("raid0", origin.raid0Name),
			slog.String("error", err.Error()))
		return ""
	}
	return origin.raid0Name
}

// reportSuppressed fills the CntlrInfo entries the SP_LEVEL_DISABLE branch of
// the build phase skips, so every suppressed resource reads the same way
// (CN19: RES_STATUS_MISSING, details = "sp_level").
func (s *CnAgentServer) reportSuppressed(
	st *cntlrState,
	plan *cntlrPlan,
	info *pb.CntlrInfo,
) {
	t := st.tracker
	for _, xp := range plan.xfers {
		nsName := nsResName(xp.nqn, int(xp.xfer.GetOriNsIdx()))
		info.XferIdToDmLinear[xp.xferId] = t.Missing(
			resKeyOf(resKeyXferDmFmt, xp.xferId), xp.finalName,
			detailsSpLevel)
		info.XferIdToSubsystem[xp.xferId] = t.Missing(
			resKeyOf(resKeyXferSsFmt, xp.xferId), xp.nqn, detailsSpLevel)
		info.XferIdToNamespace[xp.xferId] = t.Missing(
			resKeyOf(resKeyXferNsFmt, xp.xferId), nsName, detailsSpLevel)
	}
	for _, np := range plan.namespaces {
		info.NsIdToDmLinear[np.nsId] = t.Missing(
			resKeyOf(resKeyNsDevFmt, np.nsId), np.devName, detailsSpLevel)
		info.NsIdToNamespace[np.nsId] = t.Missing(
			resKeyOf(resKeyNamespaceFmt, np.nsId),
			nsResName(np.ss.nqn, np.nsIdx), detailsSpLevel)
	}
	for _, ssp := range plan.subsystems {
		info.SsIdToSubsystem[ssp.ssId] = t.Missing(
			resKeyOf(resKeySubsysFmt, ssp.ssId), ssp.nqn, detailsSpLevel)
	}
}

// reportSliceDeferred fills the three pool rows of a provisioning-deferred
// slice, shared by the converge pass and the probe (U4). A *serving* slice
// never comes here: its dm_pool row must keep carrying the raw `dmsetup
// status` line the §10.4 auto-grow parses.
func (s *CnAgentServer) reportSliceDeferred(
	st *cntlrState,
	sp *slicePlan,
	info *pb.CntlrInfo,
) {
	t := st.tracker
	info.SliceIdToMeta[sp.sliceId] = t.Provisioning(
		resKeyOf(resKeyPoolMetaFmt, sp.sliceId), sp.poolMetaName,
		detailsProvisioning)
	info.SliceIdToData[sp.sliceId] = t.Provisioning(
		resKeyOf(resKeyPoolDataFmt, sp.sliceId), sp.poolDataName,
		detailsProvisioning)
	info.SliceIdToDmPool[sp.sliceId] = t.Provisioning(
		resKeyOf(resKeyPoolFmt, sp.sliceId), sp.poolFinalName,
		detailsProvisioning)
}

func nsDevDetails(np *nsPlan) string {
	if np.suspended {
		return detailsSuspended
	}
	return ""
}

// reportCloneDeferred fills the three rows of a clone whose destination td is
// provisioning-deferred (U4), shared by the converge pass and the probe.
func (s *CnAgentServer) reportCloneDeferred(
	st *cntlrState,
	cp *clonePlan,
	info *pb.CntlrInfo,
) {
	info.CloneIdToTarget[cp.cloneId] = st.tracker.Provisioning(
		resKeyOf(resKeyCloneTgtFmt, cp.cloneId), cp.clone.GetSrcNqn(),
		detailsProvisioning)
	info.CloneIdToDmClone[cp.cloneId] = st.tracker.Provisioning(
		resKeyOf(resKeyCloneDmFmt, cp.cloneId), cp.finalName,
		detailsProvisioning)
	info.CloneIdToMeta[cp.cloneId] = st.tracker.Provisioning(
		resKeyOf(resKeyCloneMetaFmt, cp.cloneId), cp.metaDmName,
		detailsProvisioning)
}

func (s *CnAgentServer) reportCloneSuppressed(
	st *cntlrState,
	cp *clonePlan,
	info *pb.CntlrInfo,
) {
	info.CloneIdToTarget[cp.cloneId] = st.tracker.Missing(
		resKeyOf(resKeyCloneTgtFmt, cp.cloneId), cp.clone.GetSrcNqn(),
		detailsSpLevel)
	info.CloneIdToDmClone[cp.cloneId] = st.tracker.Missing(
		resKeyOf(resKeyCloneDmFmt, cp.cloneId), cp.finalName, detailsSpLevel)
	info.CloneIdToMeta[cp.cloneId] = st.tracker.Missing(
		resKeyOf(resKeyCloneMetaFmt, cp.cloneId), cp.metaDmName,
		detailsSpLevel)
}

// ---------------------------------------------------------------------------
// CN21 — cntlr teardown
// ---------------------------------------------------------------------------

// teardownCntlr is used by CN7 (pointer removed), CN2 (orphan file) and
// CN19's SP_LEVEL_DISABLE. Strictly top-down, resuming every suspended ns-dev
// by reloading it onto its CnErrorName **before** anything else: a suspended
// device blocks both the nvmet disable above it and its own removal.
//
// Thin volumes are only **deactivated** — no `delete` messages — because the
// pool metadata lives on the DN legs and the next hosting CN must find the
// thin volumes intact (CN14).
func (s *CnAgentServer) teardownCntlr(
	ctx context.Context,
	key string,
	st *cntlrState,
) {
	plan := st.applied
	if plan == nil {
		plan = newCntlrPlan(s.nf, st.req)
	}
	s.teardownCntlrResources(ctx, st, plan)

	paths := []string{s.nf.LocalCntlrPath(
		plan.clusterId, plan.cnId, plan.spId, plan.cntlrId)}
	paths = append(paths, s.allChunkPaths(st, plan)...)
	if err := s.store.Remove(ctx, paths...); err != nil {
		slog.ErrorContext(ctx, "removing cntlr state files failed",
			slog.String("error", err.Error()))
	}
	s.dropCntlr(key)
	s.locks.DropObj(key)
}

// teardownCntlrResources removes every cntlr-scoped resource, strictly
// top-down, and leaves the local store alone. CN21 adds the file deletion on
// top; the SP_LEVEL_DISABLE row of CN19 uses it bare, because there the
// desired state must persist. The tail is the CN21 leg order of update_01.md
// U2 spec 4: cancel the probers, disconnect, then remove the wrappers
// (removeLeg).
func (s *CnAgentServer) teardownCntlrResources(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
) {
	for _, np := range plan.namespaces {
		s.parkNsDevLogged(ctx, np)
	}
	for _, ssp := range plan.subsystems {
		s.removeExport(ctx, ssp.nqn)
	}
	for _, xp := range plan.xfers {
		s.removeExport(ctx, xp.nqn)
	}
	for _, np := range plan.namespaces {
		s.removeDm(ctx, np.devName)
	}
	for _, xp := range plan.xfers {
		s.removeDm(ctx, xp.finalName)
	}
	s.stopConnectRetry(st)
	for _, cp := range plan.clones {
		s.removeDm(ctx, cp.finalName)
		// Under cloneMetaMu like every other registry mutation (R3.6): this
		// teardown runs on one cntlr while another cntlr of the same CN may be
		// enumerating and allocating.
		s.removeCloneMetaDm(ctx, cp.metaDmName)
		s.disconnect(ctx, cp.clone.GetSrcNqn())
	}
	for _, tp := range plan.tds {
		s.removeDm(ctx, tp.raid0Name)
		s.removeDm(ctx, tp.errorName)
	}
	for _, tp := range plan.tds {
		for _, sp := range plan.slices {
			s.removeDm(ctx, plan.thinName(tp.tdId, sp.sliceId))
		}
	}
	for _, sp := range plan.slices {
		s.removeDm(ctx, sp.poolFinalName)
		s.removeDm(ctx, sp.poolMetaName)
		s.removeDm(ctx, sp.poolDataName)
	}
	// Every pool device of this cntlr is gone, so no slice is sweep-pending
	// any more; a re-creation re-arms on its own Create branch (update_05.md
	// U3).
	clear(st.pendingSweep)
	for _, gp := range plan.grps {
		s.removeGroup(ctx, gp)
	}
	s.stopLegProbers(st, nil)
	for _, lp := range plan.legs {
		s.removeLeg(ctx, lp)
	}
}

// dropAllResKeys forgets every resource history of one shape, so a later
// rebuild reports a fresh epoch (SH14).
func (s *CnAgentServer) dropAllResKeys(st *cntlrState, plan *cntlrPlan) {
	if plan == nil {
		return
	}
	t := st.tracker
	for _, lp := range plan.legs {
		t.Drop(resKeyOf(resKeyLegFmt, lp.legId))
	}
	for _, gp := range plan.grps {
		t.Drop(resKeyOf(resKeyGrpFmt, gp.grpId))
	}
	for _, sp := range plan.slices {
		t.Drop(resKeyOf(resKeyPoolFmt, sp.sliceId))
		t.Drop(resKeyOf(resKeyPoolMetaFmt, sp.sliceId))
		t.Drop(resKeyOf(resKeyPoolDataFmt, sp.sliceId))
	}
	for _, tp := range plan.tds {
		s.dropTdKeys(st, plan, tp)
	}
	for _, np := range plan.namespaces {
		t.Drop(resKeyOf(resKeyNsDevFmt, np.nsId))
		t.Drop(resKeyOf(resKeyNamespaceFmt, np.nsId))
	}
	for _, ssp := range plan.subsystems {
		t.Drop(resKeyOf(resKeySubsysFmt, ssp.ssId))
	}
	for _, xp := range plan.xfers {
		s.dropXferKeys(st, xp.xferId)
	}
	for _, cp := range plan.clones {
		t.Drop(resKeyOf(resKeyCloneTgtFmt, cp.cloneId))
		t.Drop(resKeyOf(resKeyCloneDmFmt, cp.cloneId))
		t.Drop(resKeyOf(resKeyCloneMetaFmt, cp.cloneId))
	}
}

// ---------------------------------------------------------------------------
// Old-vs-new diffs used by the retire phase
// ---------------------------------------------------------------------------

func (p *cntlrPlan) ssByNqn(nqn string) *ssPlan {
	for _, ssp := range p.subsystems {
		if ssp.nqn == nqn {
			return ssp
		}
	}
	return nil
}

func removedNamespaces(old, plan *cntlrPlan) []*nsPlan {
	if old == nil {
		return nil
	}
	keep := make(map[uint64]struct{}, len(plan.namespaces))
	for _, np := range plan.namespaces {
		keep[np.nsId] = struct{}{}
	}
	var out []*nsPlan
	for _, np := range old.namespaces {
		if _, ok := keep[np.nsId]; !ok {
			out = append(out, np)
		}
	}
	return out
}

func removedSubsystems(old, plan *cntlrPlan) []*ssPlan {
	if old == nil {
		return nil
	}
	keep := make(map[string]struct{}, len(plan.subsystems))
	for _, ssp := range plan.subsystems {
		keep[ssp.nqn] = struct{}{}
	}
	var out []*ssPlan
	for _, ssp := range old.subsystems {
		if _, ok := keep[ssp.nqn]; !ok {
			out = append(out, ssp)
		}
	}
	return out
}

func removedXfers(old, plan *cntlrPlan) []*xferPlan {
	if old == nil {
		return nil
	}
	var out []*xferPlan
	for _, xp := range old.xfers {
		if plan.xferById[xp.xferId] == nil {
			out = append(out, xp)
		}
	}
	return out
}

// retiredXfers are the transfers whose resources must go: removed from the
// request, or suppressed because nothing cntlr-scoped survives this level.
func retiredXfers(old, plan *cntlrPlan) []*xferPlan {
	out := removedXfers(old, plan)
	if plan.wantAny {
		return out
	}
	seen := make(map[uint64]struct{}, len(out))
	for _, xp := range out {
		seen[xp.xferId] = struct{}{}
	}
	for _, xp := range plan.xfers {
		if _, ok := seen[xp.xferId]; !ok {
			out = append(out, xp)
		}
	}
	return out
}

func retiredClones(old, plan *cntlrPlan) []*clonePlan {
	var out []*clonePlan
	seen := make(map[uint64]struct{})
	add := func(cp *clonePlan) {
		if _, ok := seen[cp.cloneId]; ok {
			return
		}
		seen[cp.cloneId] = struct{}{}
		out = append(out, cp)
	}
	if old != nil {
		for _, cp := range old.clones {
			if !plan.wantClone || plan.cloneById[cp.cloneId] == nil {
				add(cp)
			}
		}
	}
	if !plan.wantClone {
		for _, cp := range plan.clones {
			add(cp)
		}
	}
	return out
}

func removedTds(old, plan *cntlrPlan) []*tdPlan {
	if old == nil {
		return nil
	}
	var out []*tdPlan
	for _, tp := range old.tds {
		if plan.tdById[tp.tdId] == nil {
			out = append(out, tp)
		}
	}
	return out
}

func unionTds(old, plan *cntlrPlan) []*tdPlan {
	out := append([]*tdPlan(nil), plan.tds...)
	if old == nil {
		return out
	}
	for _, tp := range old.tds {
		if plan.tdById[tp.tdId] == nil {
			out = append(out, tp)
		}
	}
	return out
}

func unionSlices(old, plan *cntlrPlan) []*slicePlan {
	out := append([]*slicePlan(nil), plan.slices...)
	if old == nil {
		return out
	}
	for _, sp := range old.slices {
		if plan.sliceById[sp.sliceId] == nil {
			out = append(out, sp)
		}
	}
	return out
}

// retiredSlices are the pools that must go: their slice left the request, or
// the level/role no longer wants thin pools at all.
func retiredSlices(old, plan *cntlrPlan) []*slicePlan {
	if plan.wantPool {
		if old == nil {
			return nil
		}
		var out []*slicePlan
		for _, sp := range old.slices {
			if plan.sliceById[sp.sliceId] == nil {
				out = append(out, sp)
			}
		}
		return out
	}
	return unionSlices(old, plan)
}

func retiredGrps(old, plan *cntlrPlan) []*grpPlan {
	if plan.wantGrp {
		if old == nil {
			return nil
		}
		var out []*grpPlan
		for _, gp := range old.grps {
			if plan.grpById[gp.grpId] == nil {
				out = append(out, gp)
			}
		}
		return out
	}
	out := append([]*grpPlan(nil), plan.grps...)
	if old == nil {
		return out
	}
	for _, gp := range old.grps {
		if plan.grpById[gp.grpId] == nil {
			out = append(out, gp)
		}
	}
	return out
}

func retiredLegs(old, plan *cntlrPlan) []*legPlan {
	if plan.wantLeg {
		if old == nil {
			return nil
		}
		var out []*legPlan
		for _, lp := range old.legs {
			if plan.legById[lp.legId] == nil {
				out = append(out, lp)
			}
		}
		return out
	}
	out := append([]*legPlan(nil), plan.legs...)
	if old == nil {
		return out
	}
	for _, lp := range old.legs {
		if plan.legById[lp.legId] == nil {
			out = append(out, lp)
		}
	}
	return out
}
