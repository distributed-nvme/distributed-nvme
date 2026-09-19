package cnagent

import (
	"context"
	"log/slog"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// msgInvalidStoredConf is the §7 refusal record: a conf member the control
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
	// §7: the control plane stores concrete geometry, so a zero member here
	// is a conf this agent must not build against — data_block_size and
	// stripe_size become the dm thin-pool's and raid0's own arguments, and
	// bitmap_chunk_block_cnt the md bitmap's. This is the last point with
	// literally zero side effects: refusing here skips the desired-state
	// promotion below, the whole converge (whose sweep alone rewrites ANA
	// states, reloads ns-dev linears and removes dm devices) and the
	// local-store Save, so a bad request cannot even be replayed by the next
	// Reconcile. The stored revision is echoed back, not the request's, so
	// the worker sees the request was not accepted.
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
	if st == nil {
		st = newCntlrState(req)
	}
	st.req = req
	// This request came through the revision gate, so GateRevision makes
	// it the newest desired state this cntlr has ever accepted and its
	// td_list is authoritative — the one copy the activation sweep may
	// delete against (CN14).
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

// convergeCntlr is the CN9 converge pass: one sweep top-down, then one build
// phase bottom-up. That phase order is what implements §11.1 without special
// cases — a primary→standby flip is nothing but "the desired set shrank to
// the standby shape", and standby→primary is "it grew".
//
// The sweep replaced a retire phase that diffed the plan it applied last time
// against this one. That diff could name a removal only once: a removal that
// failed was forgotten together with the old plan, and these are exactly the
// flows — a level change, a spare switch, a finished migration — where the
// remote end is dead and a removal DOES fail. The sweep derives the same work
// from what the node actually holds, so a failed removal is simply found
// again next pass.
//
// `migr_list` is carried in the request and read by nothing: the CN's whole
// part in a migration is that a leg's side_list temporarily holds two sides
// (CN10); the field stays reserved for a future consumer.
func (s *CnAgentServer) convergeCntlr(
	ctx context.Context,
	st *cntlrState,
) (*pb.CntlrInfo, *agent.SweepResult) {
	// The same §7 refusal as syncupCntlr's, for the two entrances that do not
	// come through it: the startup Reconcile, which converges from a file an
	// older build may have persisted with zeros, and the background connect
	// retry, which re-enters with the request it already holds. Refusing
	// before newCntlrPlan touches nothing at all: the sweep is name-driven
	// and needs no plan of this cntlr to find its objects later.
	if err := agent.ValidateBdevConf(st.req.GetBdevConf()); err != nil {
		ptr := st.req.GetCntlrPointer()
		slog.ErrorContext(ctx, msgInvalidStoredConf,
			slog.Uint64("cluster_id", st.req.GetClusterId()),
			slog.Uint64("cn_id", st.req.GetCnId()),
			slog.Uint64("sp_id", ptr.GetSpId()),
			slog.Uint64("cntlr_id", ptr.GetCntlrId()),
			slog.String("error", err.Error()))
		// Nothing was converged and nothing enumerated, so the pass has no
		// verdict to give: the reply's code is the §7 refusal, not this.
		return newCntlrInfo(), &agent.SweepResult{}
	}
	plan := newCntlrPlan(s.nf, st.req)
	info := newCntlrInfo()
	sweep := s.sweepCntlr(ctx, st, plan, true)
	if !plan.wantAny {
		// SP_LEVEL_DISABLE: the sweep's wanted set is empty, so every
		// cntlr-scoped object has just gone, but the store file and the
		// in-memory desired state stay (CN19). What the sweep cannot do is
		// stop the background connect retry or forget the resource
		// histories, because neither is an object on the node.
		s.stopConnectRetry(st)
		s.dropAllResKeys(st, plan)
	}
	s.build(ctx, st, plan, info)
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
				// [D15]: a leg of leg_list is still provisioning, so there is no
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
				// [D15]: no pool, so no thin volume in this slice.
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
			// serve, so it reports PROVISIONING rather than OK ([D15]).
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
// inside a single quiesce of the origin td's raid0 (CN14).
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
// slice, shared by the converge pass and the probe ([D15]). A *serving* slice
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
