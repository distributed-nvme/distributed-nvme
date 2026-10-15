package cnagent

import (
	"context"
	"log/slog"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// msgInvalidStoredConf is the refusal record of architecture.md, Common
// validation: a conf member the control
// plane cannot have written reached this agent, and the converge it would have
// driven did not happen. The string is shared with the dn role and with
// dnv-worker's own refusal so one grep finds every one of them.
const msgInvalidStoredConf = "invalid stored conf"

// syncupCntlr implements CN8-CN20. The node read lock and this cntlr's object
// lock are held by the caller.
func (s *CnAgentServer) syncupCntlr(
	ctx context.Context,
	key string,
	req *pb.SyncupCntlrRequest,
) *pb.SyncupCntlrReply {
	st := s.getCntlr(key)
	var stored uint64
	if st != nil {
		stored = st.loadReq().GetRevision()
	}
	if reply := s.gateCntlr(ctx, req, stored); reply != nil {
		return reply
	}
	if st == nil {
		st = newCntlrState(req)
	}
	// Other goroutines read the request without this cntlr's object lock
	// (cntlrState.req): the store is atomic, and what it replaces is never
	// modified.
	st.storeReq(req)
	// This request came through the revision gate, so GateRevision makes
	// it the newest desired state this cntlr has ever accepted and its
	// td_list is authoritative — the one copy CN14's thin id sweeps may
	// delete against. Only this cntlr's converges read it, each under the
	// object lock held here or the node write lock.
	st.reqFromRpc = true
	s.putCntlr(key, st)

	info, sweep := s.convergeCntlr(ctx, st)

	ptr := req.GetCntlrPointer()
	path := s.nf.LocalCntlrPath(req.GetClusterId(), req.GetCnId(),
		ptr.GetSpId(), ptr.GetCntlrId())
	if err := s.store.Save(ctx, path, req); err != nil {
		slog.ErrorContext(ctx, "persisting cntlr state failed",
			slog.String("path", path),
			slog.String("error", err.Error()))
	}
	return &pb.SyncupCntlrReply{
		AgentReply: sweep.Reply(),
		Revision:   req.GetRevision(),
		CntlrInfo:  info,
		BmInfoList: s.bitmapInfoList(st),
	}
}

// gateCntlr is CN8's gating of a SyncupCntlr against stored, the revision of
// the request this process holds for the cntlr, zero for a cntlr it does not
// hold: the pointer, then the revision, then the conf. It returns the
// refusal, or nil for a request the converge may take. It writes nothing but
// the conf refusal's Error record, so SyncupCntlr runs it under the node read
// lock before it takes the object lock of a cntlr this process does not
// hold, and a refused request leaves no lock entry behind (dnagent.md, SH11);
// syncupCntlr runs it again under that lock. The CN request the pointer gate
// reads cannot change under the node read lock, since SyncupCn replaces it
// under the write lock.
func (s *CnAgentServer) gateCntlr(
	ctx context.Context,
	req *pb.SyncupCntlrRequest,
	stored uint64,
) *pb.SyncupCntlrReply {
	// CN8 gating: SyncupCn introduces the pointer first (architecture.md,
	// Common agent rules).
	cn := s.getCn(cnKey(req.GetClusterId(), req.GetCnId()))
	if cn == nil || !pointerKnown(cn.loadReq(), req.GetCntlrPointer()) {
		return &pb.SyncupCntlrReply{
			AgentReply: agent.UnknownObjectReply(
				"cntlr pointer %s not in the cn's list",
				cntlrPointerText(req.GetCntlrPointer())),
		}
	}
	if reject := agent.GateRevision(stored, req.GetRevision()); reject != nil {
		return &pb.SyncupCntlrReply{AgentReply: reject, Revision: stored}
	}
	// architecture.md, Common validation: the control plane stores concrete
	// geometry, so a zero member here
	// is a conf this agent must not build against — data_block_size and
	// stripe_size become the dm thin-pool's and raid0's own arguments, and
	// bitmap_chunk_block_cnt the md bitmap's. This is the last point with
	// literally zero side effects: refusing here skips the desired-state
	// promotion, the whole converge (whose sweep alone rewrites ANA states,
	// reloads ns-dev linears and removes dm devices) and the local-store
	// Save (syncupCntlr), so a bad request cannot even be replayed by the
	// next Reconcile. The stored revision is echoed back, not the request's,
	// so the worker sees the request was not accepted.
	if err := agent.ValidateBdevConf(req.GetBdevConf()); err != nil {
		ptr := req.GetCntlrPointer()
		slog.ErrorContext(ctx, msgInvalidStoredConf,
			slog.Uint64("cluster_id", req.GetClusterId()),
			slog.Uint64("cn_id", req.GetCnId()),
			slog.Uint64("sp_id", ptr.GetSpId()),
			slog.Uint64("cntlr_id", ptr.GetCntlrId()),
			slog.String("error", err.Error()))
		return &pb.SyncupCntlrReply{
			AgentReply: agent.InvalidConfReply("%v", err),
			Revision:   stored,
		}
	}
	return nil
}

// convergeCntlr is the CN9 converge pass: one sweep top-down, then one build
// phase bottom-up. That phase order is what implements architecture.md,
// Failover, without special
// cases — a primary→standby flip is nothing but "the desired set shrank to
// the standby shape", and standby→primary is "it grew".
//
// The sweep derives its work from what the node actually holds, never from
// the plan applied last time, so a removal that fails — and in a level
// change, a spare switch or a finished migration the remote end is dead and
// a removal DOES fail — is simply found again next pass.
//
// The CN's whole part in a migration is that a leg's side_list temporarily
// holds two sides (CN10).
func (s *CnAgentServer) convergeCntlr(
	ctx context.Context,
	st *cntlrState,
) (*pb.CntlrInfo, *agent.SweepResult) {
	// The same refusal (architecture.md, Common validation) as syncupCntlr's,
	// for the entrances that do not come through it: the startup Reconcile,
	// which converges from a file that may hold zeros, and the two background
	// retries, the connect retry and CN18's clone-metadata retry, which
	// re-enter with the request they already hold. Refusing before
	// newCntlrPlan touches nothing at all: the sweep is name-driven and needs
	// no plan of this cntlr to find its objects later.
	req := st.loadReq()
	if err := agent.ValidateBdevConf(req.GetBdevConf()); err != nil {
		ptr := req.GetCntlrPointer()
		slog.ErrorContext(ctx, msgInvalidStoredConf,
			slog.Uint64("cluster_id", req.GetClusterId()),
			slog.Uint64("cn_id", req.GetCnId()),
			slog.Uint64("sp_id", ptr.GetSpId()),
			slog.Uint64("cntlr_id", ptr.GetCntlrId()),
			slog.String("error", err.Error()))
		// Nothing was converged and nothing enumerated, so the pass has no
		// verdict to give: the reply's code is the refusal of
		// architecture.md, Common validation, not this.
		return newCntlrInfo(), &agent.SweepResult{}
	}
	plan := newCntlrPlan(s.nf, req)
	// CN14: the torn marks follow the request alone, on every converge.
	keepTornMarks(st, plan)
	info := newCntlrInfo()
	// CN10: the pass's one wait budget, made here and nowhere else, so every
	// entrance — the RPC, the startup reconcile, a background retry attempt
	// — gets its own and no pass can spend another's.
	budget := s.newPassBudget()
	// CN11: the probers are this process's goroutines, not objects of the
	// node, so the sweep does not own them. Every converge trims them,
	// before its sweep, to the legs its plan still probes: the trim decides
	// from the plan alone and removes nothing from the node, so a sweep
	// that an unanswered listing stops, or whose descent stops above L10,
	// is no reason for it to wait. Inside L10 it did wait on every layer
	// above, and a demoted primary's probers went on probing through the
	// standby's legs, and a departed leg's through its wrapper, until some
	// later pass got that far. Coming first, it also comes before L10's
	// disconnect, which is what releases a prober wedged on a pathless leg.
	s.stopLegProbers(st, wantedProbers(plan))
	// CN10: the pass's NVMe walk, the sweep's list of connections and where
	// the build's lookups find their entries until the build marks it stale.
	plan.walk = s.newPassWalk(ctx)
	sweep := s.sweepCntlr(ctx, st, plan, true)
	if !plan.wantAny {
		// SP_LEVEL_DISABLE: the sweep's wanted set is empty, so every
		// cntlr-scoped object has just gone, but the store file and the
		// in-memory desired state stay (CN19). What the sweep cannot do is
		// stop the background retries or forget the resource histories,
		// because none of them is an object on the node.
		s.stopConnectRetry(st)
		s.stopCloneMetaRetry(st)
		s.dropAllResKeys(st, plan)
	}
	s.build(ctx, st, plan, info, budget)
	return info, sweep
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
// Build phase, bottom-up (CN9) — architecture.md, Failover, new_primary
// steps 1-4
// ---------------------------------------------------------------------------

func (s *CnAgentServer) build(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	info *pb.CntlrInfo,
	budget *agent.WaitBudget,
) {
	retryNeeded := false
	// metaWait: a clone of this pass waits for a free run in the arena, the
	// one cause of CN18's clone-metadata retry (startCloneMetaRetry).
	metaWait := false

	// Legs (CN10).
	var available map[uint64]bool
	if plan.wantLeg {
		available, retryNeeded = s.ensureLegs(ctx, st, plan, info, budget)
	} else {
		for _, lp := range plan.legs {
			info.LegIdToLeg[lp.legId] = st.tracker.Missing(
				resKeyOf(resKeyLegFmt, lp.legId), lp.name, detailsSpLevel)
		}
	}

	// Groups (CN12) — a standby has none (architecture.md, Standby cntlr).
	// One /sys/block walk serves every group of the pass (Md.Walk), and the
	// pool concats after them take each md group's number from the same walk
	// (mdDevNo), which an assembly here brings up to date (Md.Refresh).
	plan.mdWalks = &mdWalkOnce{md: s.md}
	late := false
	if plan.wantGrp {
		for _, gp := range plan.grps {
			if gp.deferred {
				// [D15]: a leg of leg_list is still provisioning, so there is no
				// md array and no CnGrpName to build, and the group is not a
				// pool concat target either.
				info.GrpIdToMdRaid[gp.grpId] = st.tracker.Provisioning(
					resKeyOf(resKeyGrpFmt, gp.grpId), gp.resName,
					detailsProvisioning)
				continue
			}
			err := s.ensureGroup(ctx, plan, gp, available)
			info.GrpIdToMdRaid[gp.grpId] = st.tracker.FromErr(
				resKeyOf(resKeyGrpFmt, gp.grpId), gp.resName, "", err)
			if gp.hasLateMember(available) {
				late = true
			}
		}
	} else if plan.primary {
		for _, gp := range plan.grps {
			info.GrpIdToMdRaid[gp.grpId] = st.tracker.Missing(
				resKeyOf(resKeyGrpFmt, gp.grpId), gp.resName, detailsSpLevel)
		}
	}
	if late {
		// CN12: a group's leg_list member that is not available this pass is
		// the same class of transient as a connect that failed — the side's
		// ANA flip has not reached this CN's sysfs yet (the worker's
		// sides-first hold is bounded, so a promotion's SyncupCntlr can still
		// come before the sides' flips, [D16]), its DN is rebooting, its path
		// is mid-reconnect — and nothing else re-drives the converge that
		// would assemble it, add it or build the pool over it: the worker
		// re-syncs on a revision or a reply code, never on a row. So it
		// registers the CN10 retry, which re-runs the whole converge until
		// every such member is available. There is no record of its own: the
		// rows already report it — an md group's error, or "degraded" once a
		// probe reads the array, the error of a layer above a RedundNone
		// group, and the member's own leg row.
		s.startConnectRetry(st, plan)
		retryNeeded = true
	}

	// Per-slice pools (CN13) and thin volumes (CN14).
	poolReady := make(map[uint64]bool, len(plan.slices))
	if plan.wantPool {
		for _, sp := range plan.slices {
			if sp.deferred {
				// [D15]: every group on one side of this slice is deferred — the
				// initial CreateStoragePool shape. Nothing is built, and
				// nothing is wrong: an empty concat would be an error, and
				// PROVISIONING must never feed err_epoch.
				s.reportSliceDeferred(st, sp, info)
				continue
			}
			poolReady[sp.sliceId] = s.ensureSlice(ctx, st, plan, sp, info)
		}
		// CN14's activation sweep, for every slice whose pool device this
		// incarnation just created. A
		// startup reconcile arms and skips — its persisted request may lag
		// the pool, and deleting against a lagging td_list would destroy a
		// live td; the first post-boot SyncupCntlr then finds the flag still
		// set — its own ensurePool probe-matches the surviving device and
		// arms nothing — and sweeps with the fresh, revision-gated td_list.
		// Sweeping here, before the pre-pass and the thin loop below, also
		// frees the strays' blocks before anything allocates, and a stray can
		// never collide with a new id (dev_ids are never reused;
		// architecture.md, Known limits, names the etcd restore that can
		// break that).
		for _, sp := range plan.slices {
			if !poolReady[sp.sliceId] {
				continue
			}
			if st.reqFromRpc && st.pendingSweep[sp.sliceId] {
				s.sweepThinIds(ctx, st, plan, sp, nil)
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
	// CN14: the snapshot-message pre-pass. Every needed slice's `create_snap`
	// goes out inside ONE suspension of the origin td's raid0, so the
	// per-slice snapshots all describe the same instant instead of one per
	// message. It has to sit exactly here: after the loop that fills
	// poolReady, and before the thin loop — only the *messages* belong inside
	// the window, because a snapshot's content is fixed at message time and
	// its thin devices are built afterwards by the unchanged loop below.
	if plan.wantPool {
		// plan.wantPool implies plan.primary, so no role test is needed.
		for _, tp := range plan.tds {
			// CN14, case 1: a created snapshot's ids are in every slice pool
			// already, so there is nothing to message and nothing to
			// quiesce for it — the thin loop's `dmsetup create` is the
			// whole job.
			if tp.td.GetOriId() == 0 || tp.td.GetCreated() {
				continue
			}
			if !s.snapshotPrePass(ctx, plan, tp, poolReady) {
				continue
			}
			// CN14's torn mark: the run that tore the snapshot sets it, and
			// only the request clears it (keepTornMarks).
			if !st.tornTds[tp.tdId] {
				slog.WarnContext(ctx, msgSnapshotTorn, append(
					s.cntlrIdAttrs(plan),
					slog.Uint64("td_id", tp.tdId),
					slog.Uint64("dev_id", uint64(tp.td.GetDevId())))...)
			}
			st.tornTds[tp.tdId] = true
		}
	}
	for _, tp := range plan.tds {
		if !plan.primary {
			continue
		}
		thinInfo := &pb.CntlrInfo_ThinInfo{
			SliceIdToDmThin: make(map[uint64]*pb.ResInfo, len(plan.slices)),
			Torn:            tornMark(st, tp),
		}
		for _, sp := range plan.slices {
			key := thinResKey(tp.tdId, sp.sliceId)
			name := plan.thinName(tp.tdId, sp.sliceId)
			switch {
			case !plan.wantPool:
				thinInfo.SliceIdToDmThin[sp.sliceId] = st.tracker.Missing(
					key, name, detailsSpLevel)
			case sp.deferred:
				// [D15]: no pool, so no thin volume in this slice.
				thinInfo.SliceIdToDmThin[sp.sliceId] = st.tracker.Provisioning(
					key, name, detailsProvisioning)
			case !poolReady[sp.sliceId]:
				thinInfo.SliceIdToDmThin[sp.sliceId] = st.tracker.Missing(
					key, name, "thin pool missing")
			default:
				err := s.ensureThin(ctx, plan, tp, sp)
				thinInfo.SliceIdToDmThin[sp.sliceId] = thinRowOf(
					st, tp, key, name, err)
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
			// [D15]: a slice's thin volume does not exist, so the stripe cannot
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

	// Clones (CN18), then transfers (CN17). finished: the clones whose
	// dm-clone this pass read hydrating after CN18 step 5.
	finished := make(map[uint64]bool, len(plan.clones))
	if plan.wantClone {
		for _, cp := range plan.clones {
			connectRetry, waitRoom, done := s.ensureClone(
				ctx, st, plan, cp, info, budget)
			if connectRetry {
				retryNeeded = true
			}
			if waitRoom {
				metaWait = true
			}
			finished[cp.cloneId] = done
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
		// held: the namespaces whose ns-dev ensureNsDev kept off a raid0 a
		// dm-clone the plan does not want may still be hydrating into
		// (plan.cloneHold).
		held := make(map[uint64]nsDevHold)
		for _, np := range plan.namespaces {
			nsHeld, err := s.ensureNsDev(ctx, np, plan.cloneHold)
			if nsHeld != nsDevConverged {
				held[np.nsId] = nsHeld
				// A held ns-dev is not where its plan wants it, and nothing
				// else is sure to re-drive the pass that puts it there: a
				// hold that an unanswered `dmsetup ls` alone caused leaves
				// the Check verdict, which lists the node from the round's
				// own reading, clean, and the worker re-syncs on a revision
				// or a reply code, never on a row. So the hold registers the
				// CN10 retry, whose next attempt lists afresh and, with no
				// dm-clone left to wait for, reloads the ns-dev.
				s.startConnectRetry(st, plan)
				retryNeeded = true
			}
			if err != nil && np.clone != nil && finished[np.clone.cloneId] {
				// Rule 5 after a build this pass finished: hydration is on,
				// so whatever still keeps the td's hosts off the dm-clone is
				// this ns-dev — left on a recovery's park (CN18 step 2),
				// whose namespace stays optimized, or not created yet. CN18
				// registered no retry for the clone, and the worker re-syncs
				// on a revision or a reply code, never on a row, so this
				// registers the CN10 retry: its next attempt reads the
				// status and the table afresh.
				s.startConnectRetry(st, plan)
				retryNeeded = true
			}
			key := resKeyOf(resKeyNsDevFmt, np.nsId)
			if err == nil && nsHeld != nsDevConverged {
				// In the probe's words, so the converge and the Check
				// round report the held ns-dev alike.
				info.NsIdToDmLinear[np.nsId] = st.tracker.Err(
					key, np.devName, detailsNsDevNotDesired)
				continue
			}
			// A deferred namespace is built exactly as the effective plan
			// wants it — on the td's dm-error — but nothing under it can
			// serve, so it reports PROVISIONING rather than OK ([D15]).
			info.NsIdToDmLinear[np.nsId] = deferredFromErr(
				st.tracker, np.deferred, key, np.devName,
				nsDevDetails(np), err)
		}
		for _, ssp := range plan.subsystems {
			s.ensureSubsystem(ctx, st, plan, ssp, info)
		}

		// ANA rewrites to optimized last — architecture.md, Failover,
		// new_primary step 4. A held
		// namespace that serves keeps the group it has and goes on serving
		// from the table it keeps. A held one that is parked goes
		// inaccessible rather than optimized over the dm-error: it may have
		// been optimized until this pass's P2 parked it for an L3 that then
		// left the dm-clone, and a host should queue, not take IO errors.
		for _, np := range plan.namespaces {
			grpId := np.anaGrpId
			switch held[np.nsId] {
			case nsDevHeldServing:
				continue
			case nsDevHeldParked:
				grpId = common.AnaGrpIdInaccessible
			default:
				if grpId == common.AnaGrpIdInaccessible {
					continue
				}
			}
			if err := s.setAna(
				ctx, np.ss.nqn, np.nsIdx, grpId); err != nil {
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
	// The pass that leaves no clone waiting for room ends CN18's
	// clone-metadata retry: the clone was built, left clone_list, or is
	// suppressed by the level or a demotion, and a request that undoes any
	// of those comes with a SyncupCntlr of its own.
	if !metaWait {
		s.stopCloneMetaRetry(st)
	}
}

// snapshotPrePass sends one snapshot td's per-slice `create_snap` messages
// inside a single quiesce of the origin td's raid0 (CN14).
// Suspending the raid0 flushes every in-flight host write and holds new ones
// for the duration of the messages, which is what makes the per-slice
// snapshots point-in-time with respect to each other; each message keeps its
// own nested per-slice origin-thin suspend, which is dm-thin's own documented
// requirement and a separate thing.
//
// It owns *every* message of an uncreated snapshot (CN14): ensureThin never
// messages a td with ori_id != 0, so a slice this pass declines — or one
// whose message failed — simply waits for the next converge, which is still
// driven by the same `created == false`. A failed message is tolerated: the
// pool may already hold the dev_id after a crashed earlier pass, and the
// `dmsetup create` that follows decides the outcome.
//
// It reports whether this run tore the snapshot (CN14's torn mark): whether
// one slice's message made the id while another slice already held it — its
// snapshot device is there, or its message was refused with EEXIST
// (agent.IsExists), the id being in its pool already. The slices of the two
// kinds then date from different instants, since the ones already held were
// made by an earlier run, in another quiesce or in none. The snapshot device
// of every slice that is not deferred is read, one whose pool is not ready
// this pass included, since a device there holds its id whatever the state
// of its pool; only a ready pool is sent a message. A slice the run cannot
// read, or whose message fails otherwise, counts as neither; so does one
// with no device whose pool is not ready. A run that makes every id, or
// none, tears nothing.
func (s *CnAgentServer) snapshotPrePass(
	ctx context.Context,
	plan *cntlrPlan,
	tp *tdPlan,
	poolReady map[uint64]bool,
) bool {
	var need []*slicePlan
	// had: a slice holds the id from before this run.
	had := false
	for _, sp := range plan.slices {
		// [D15]: a deferred slice has no pool, so no snapshot device.
		if sp.deferred {
			continue
		}
		// The same trigger ensureThin uses. An Info error means "skip":
		// ensureThin fails that slice with the same error anyway, or the
		// thin loop reports its pool missing.
		//
		// CN14: there is no second filter on the origin's own thin device.
		// The gateway refuses a snapshot of an origin that is not materialized
		// in every slice pool (architecture.md, Thin devices), so `create_snap`
		// cannot be inverted with the origin's `create_thin` — and whether
		// *this* CN has built the origin's dm device is irrelevant to a message
		// the pool metadata answers.
		dev, err := s.dm.Info(ctx, plan.thinName(tp.tdId, sp.sliceId))
		if err != nil {
			continue
		}
		if dev != nil {
			// The snapshot device attaches the id, so its pool holds it,
			// ready this pass or not: the id was made by an earlier run.
			had = true
			continue
		}
		// poolReady only ever holds an entry for a non-deferred slice under
		// plan.wantPool, so it also subsumes the pool-missing arm of the
		// thin loop's switch. It is deliberately NOT tp.deferred: that flag
		// is plan-global (anySliceDeferred), and gating the messages on it
		// would drop create_snap for every ready slice whenever one sibling
		// slice is still provisioning.
		if !poolReady[sp.sliceId] {
			continue
		}
		need = append(need, sp)
	}
	if len(need) == 0 {
		// SH16: an equal-revision re-apply must not suspend anything. Every
		// converge round would otherwise stall the origin's host IO.
		return false
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
	// made: a message of this run made the id in its slice's pool.
	made := false
	for _, sp := range need {
		err := s.createSnapId(ctx, plan, tp, sp)
		if err == nil {
			made = true
			continue
		}
		if agent.IsExists(err) {
			had = true
		}
		slog.ErrorContext(ctx, "thin-pool create message failed",
			slog.String("pool", sp.poolFinalName),
			slog.String("thin", plan.thinName(tp.tdId, sp.sliceId)),
			slog.String("error", err.Error()))
	}
	return made && had
}

// msgSnapshotTorn is the Warn record of the converge whose snapshot pre-pass
// tore a snapshot and set its torn mark (CN14). The mark is reported until
// the created flip records it; the only remedy is to delete and re-create
// the snapshot (architecture.md, Thin devices).
const msgSnapshotTorn = "snapshot torn"

// tornMark is the torn mark a cntlr's report carries in one td's thin info,
// the same on the converge and the check round (CN14, CN28): set while the
// cntlr holds a mark for the td and its request does not list it created
// yet. A created td reads false here even before a converge has dropped its
// mark (keepTornMarks), so a check round never reports one either.
func tornMark(st *cntlrState, tp *tdPlan) bool {
	return !tp.td.GetCreated() && st.tornTds[tp.tdId]
}

// keepTornMarks drops the torn marks of the tds the request no longer lists,
// or lists created: the created flip has recorded the mark in the thin device
// record by then (dnv-worker.md RW19), and a td that left td_list has nothing
// to mark. Every converge runs it, from the request alone; a pass that makes
// no id clears nothing else, so the mark stays reported through any number
// of re-applies until the flip.
func keepTornMarks(st *cntlrState, plan *cntlrPlan) {
	for tdId := range st.tornTds {
		if tp := plan.tdById[tdId]; tp == nil || tp.td.GetCreated() {
			delete(st.tornTds, tdId)
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
// slice, shared by the converge pass and the probe ([D15]). A *serving* slice
// never comes here: its dm_pool row must keep carrying the raw `dmsetup
// status` line the thin-pool auto-grow (dnv-worker.md AR6)
// parses.
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
		return detailsParked
	}
	return ""
}

// reportCloneDeferred fills the three rows of a clone whose destination td is
// provisioning-deferred ([D15]), shared by the converge pass and the probe.
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
