package cnagent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

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
// lock are held by the caller. A request the gates refuse, or whose save
// fails (dnagent.md SH5), stores no observation; a saved one stores its
// converge's, unless its call is cut off, before the caller lets go of the
// locks (dnagent.md SH28).
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
	// The cntlr file is persisted as soon as the gates admit the request,
	// before it becomes the desired state and before the converge, for
	// dnagent.md SH5's reason: a request the worker cancels inside a long
	// converge would skip a save placed after it, and the next Reconcile
	// would converge the request before this one — whose td_list lacks a
	// thin volume this pass may have created, and whose sweep would then
	// delete that volume's thin id. A request whose save fails is refused at
	// the revision the agent holds, zero for a cntlr it does not hold, for
	// the same reason: it does not become the desired state, a cntlr the
	// agent does not hold stays unknown, and no converge runs, so the cntlr
	// file keeps the last request the agent accepted and a restart undoes
	// nothing the node serves. Only when the directory fsync after the
	// rename alone failed does the file hold this request, which a restart
	// converges and the worker wants anyway. The held request stays the one
	// the call found. The worker sends the request again as dnagent.md SH5
	// says (dnv-worker.md RW4, RW12).
	ptr := req.GetCntlrPointer()
	path := s.nf.LocalCntlrPath(req.GetClusterId(), req.GetCnId(),
		ptr.GetSpId(), ptr.GetCntlrId())
	if err := s.store.Save(ctx, path, req); err != nil {
		slog.ErrorContext(ctx, "persisting cntlr state failed",
			slog.String("path", path),
			slog.String("error", err.Error()))
		// CN8: a refused request newer than the held one makes the held
		// request count as delivered no more, as if the startup reconcile
		// had loaded it, until a SyncupCntlr whose save holds sets
		// reqFromRpc again, even one at the held revision, such as an old
		// owner's during an ownership change (cnagent.md, Known limits).
		// The refused request may be the sp worker's clear of a
		// forced-start mark (dnv-worker.md AR13) or the operator's
		// withdrawal of it, so a mark the held request carries may be one
		// the worker no longer wants, and a start from it would zero the
		// other members the forced start keeps, one of which may hold newer
		// data. So no converge acts on a mark of the held request
		// (dropForcedStartMarks), and the cntlr's reads name its marks as
		// stored (storedMarks), not as a forced start a converge would carry
		// through (forcedStartNamed), and report none acted on (actedMark):
		// the name brings the worker's re-send of the refused request, and
		// only a SyncupCntlr whose save holds brings a converge that acts on
		// a mark. CN14's thin id sweeps, behind the same gate, do not run
		// from the held request either. A refused request at the held
		// revision leaves reqFromRpc as it is: the clear and the withdrawal
		// of a mark each raise SpRev, so a request at the held revision
		// clears no mark the held request carries. A cntlr the agent does
		// not hold has no state, and nothing changes. The object lock this
		// call holds orders the write before every later converge and read
		// of the cntlr (cntlrState.reqFromRpc).
		if st != nil && req.GetRevision() > stored {
			st.reqFromRpc = false
		}
		return &pb.SyncupCntlrReply{
			AgentReply: agent.SaveFailedReply(err),
			Revision:   stored,
		}
	}
	saveAt := s.now()
	if st == nil {
		st = newCntlrState(req)
	}
	// Other goroutines read the request without this cntlr's object lock
	// (cntlrState.req): the store is atomic, and what it replaces is never
	// modified.
	st.storeReq(req)
	// This request came through the revision gate and its save held, so
	// GateRevision makes it the newest desired state this cntlr has ever
	// accepted, and it counts as delivered (CN8) until a newer request is
	// refused for a failed save (above): a converge acts on its
	// forced-start marks (CN12), and CN14's thin id sweeps, behind the same
	// gate, may run from it. Only this cntlr's converges and reads read
	// reqFromRpc, each under the object lock held here or the node write
	// lock.
	st.reqFromRpc = true
	s.putCntlr(key, st)

	info, sweep := s.convergeCntlr(ctx, st)
	code := sweep.Reply()
	// The converge's observation, stored under the object lock before the
	// reply is built (dnagent.md SH28): the next round answers from it.
	obs := s.storeCntlrObs(ctx, st, req.GetRevision(), info, code)
	return &pb.SyncupCntlrReply{
		AgentReply: code,
		Revision:   req.GetRevision(),
		CntlrInfo:  info,
		BmInfoList: s.bitmapInfoList(st),
		ObsStamp:   s.startup.Stamped(obs.Stamp(s.now(), time.Time{})),
		SaveTime:   agent.Timestamp(saveAt),
	}
}

// gateCntlr is CN8's gating of a SyncupCntlr against stored, the revision of
// the request this process holds for the cntlr, zero for a cntlr it does not
// hold: the pointer, then the revision, then the conf (cntlrGates). It
// returns the refusal, or nil for a request the converge may take. It writes
// nothing but the conf refusal's Error record, so SyncupCntlr runs it under
// the node read lock before it takes the object lock of a cntlr this process
// does not hold, and a refused request leaves no lock entry behind
// (dnagent.md, SH11); syncupCntlr runs it again under that lock. The CN
// request the pointer gate reads cannot change under the node read lock,
// since SyncupCn replaces it under the write lock.
func (s *CnAgentServer) gateCntlr(
	ctx context.Context,
	req *pb.SyncupCntlrRequest,
	stored uint64,
) *pb.SyncupCntlrReply {
	cn := s.getCn(cnKey(req.GetClusterId(), req.GetCnId()))
	reply, confErr := cntlrGates(cn, req, stored)
	if confErr != nil {
		ptr := req.GetCntlrPointer()
		slog.ErrorContext(ctx, msgInvalidStoredConf,
			slog.Uint64("cluster_id", req.GetClusterId()),
			slog.Uint64("cn_id", req.GetCnId()),
			slog.Uint64("sp_id", ptr.GetSpId()),
			slog.Uint64("cntlr_id", ptr.GetCntlrId()),
			slog.String("error", confErr.Error()))
	}
	return reply
}

// cntlrGates is gateCntlr's gating against cn, the state of the cntlr's CN,
// nil for a CN the process does not hold. It returns the refusal, or nil,
// plus the conf gate's error when that gate refused. It reads nothing but
// cn's request, takes no lock and writes nothing, so markSyncup runs it under
// mu without a record.
func cntlrGates(
	cn *cnState,
	req *pb.SyncupCntlrRequest,
	stored uint64,
) (*pb.SyncupCntlrReply, error) {
	// CN8 gating: SyncupCn introduces the pointer first (architecture.md,
	// Common agent rules).
	if cn == nil || !pointerKnown(cn.loadReq(), req.GetCntlrPointer()) {
		return &pb.SyncupCntlrReply{
			AgentReply: agent.UnknownObjectReply(
				"cntlr pointer %s not in the cn's list",
				cntlrPointerText(req.GetCntlrPointer())),
		}, nil
	}
	if reject := agent.GateRevision(stored, req.GetRevision()); reject != nil {
		return &pb.SyncupCntlrReply{AgentReply: reject, Revision: stored}, nil
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
	// so the worker sees the request was not accepted. The same gate holds
	// the request's NQNs to the gateway's rule (validateCntlrConf).
	if err := validateCntlrConf(req); err != nil {
		return &pb.SyncupCntlrReply{
			AgentReply: agent.InvalidConfReply("%v", err),
			Revision:   stored,
		}, err
	}
	return nil, nil
}

// validateCntlrConf is the conf gate of a cntlr request (architecture.md,
// Common validation), the same at its three entrances — the SyncupCntlr's
// gates (cntlrGates), the converge of a stored request (convergeCntlr) and a
// cntlr's read (readCntlr): the bdev conf's geometry is concrete
// (agent.ValidateBdevConf), and every NQN the agent builds a path or a
// connect from passes the gateway's rule (agent.ValidateCntlrNqns), so no
// pass, a read's included, builds a configfs path or a connect's options
// from an NQN the gateway would refuse.
func validateCntlrConf(req *pb.SyncupCntlrRequest) error {
	if err := agent.ValidateBdevConf(req.GetBdevConf()); err != nil {
		return err
	}
	return agent.ValidateCntlrNqns(req)
}

// convergeCntlr is the CN9 converge pass: one sweep top-down, then one build
// phase bottom-up. That phase order is what implements architecture.md,
// Failover, without special
// cases — a primary→standby flip is nothing but "the desired set shrank to
// the standby shape", and standby→primary is "it grew".
//
// The sweep derives its work from what the node actually holds, never from
// the plan applied last time, so a removal that fails — and in a level
// change or a finished migration or repair the remote end is dead and a
// removal DOES fail — is simply found again next pass.
//
// The CN's whole part in a migration is that a leg's side_list temporarily
// holds two sides (CN10).
func (s *CnAgentServer) convergeCntlr(
	ctx context.Context,
	st *cntlrState,
) (*pb.CntlrInfo, *agent.SweepResult) {
	// The same refusal (architecture.md, Common validation) as syncupCntlr's,
	// for the entrance that does not come through it: the startup Reconcile,
	// which converges from a file that may hold zeros. Refusing before
	// newCntlrPlan touches nothing at all: the sweep is name-driven and needs
	// no plan of this cntlr to find its objects later.
	req := st.loadReq()
	if err := validateCntlrConf(req); err != nil {
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
	// CN12: a forced-start mark acts only from a request that counts as
	// delivered (cntlrState.reqFromRpc). Any other held request is the
	// stored copy the startup reconcile loaded, or one whose newer
	// successor was refused for a failed save (syncupCntlr, dnagent.md
	// SH5): it can still carry a mark the worker has cleared since, whose
	// clear this agent has not accepted (the clear never arrived, say, or
	// its save failed), and a start from that mark's leg would zero the
	// member with the newest data. The startup reconcile converges the
	// first kind, every group as with no mark, and the cntlr's read names
	// the mark of either kind, so the worker re-sends the SyncupCntlr
	// (storedMarks).
	// CN12: the memory of the marks this incarnation acted on follows the
	// request's marks, read before a request that does not count as
	// delivered drops them from the plan, which only stops this pass acting.
	keepForcedStarts(st, plan)
	keepMdChecks(st, plan)
	if !st.reqFromRpc {
		plan.dropForcedStartMarks()
	}
	// A row made after the call is cut off moves no epoch. Every converge of
	// the cntlr holds its object lock or the node write lock, so the helpers
	// write through st.tracker with this pass bound (agent.ResTracker.Bind).
	defer st.tracker.Bind(ctx)()
	// CN14: the torn marks follow the request alone, on every converge, and
	// so does their file in the local store.
	s.keepTornMarks(ctx, st, plan)
	info := newCntlrInfo()
	// CN10: the pass's one wait budget, made here and nowhere else, so each
	// entrance — the RPC or the startup reconcile — gets its own and no pass
	// can spend another's.
	budget := s.newPassBudget()
	// CN11: the probers are this process's goroutines, not objects of the
	// node, so the sweep does not own them. Every converge trims them,
	// before its sweep, to the legs its plan still probes: the trim decides
	// from the plan alone and removes nothing from the node, so a sweep
	// that an unanswered listing stops, or whose descent stops above L10,
	// is no reason for it to wait. Inside L10 it would wait on every layer
	// above, and a demoted primary's probers would go on probing through the
	// standby's legs, and a departed leg's through its wrapper, until some
	// later pass got that far. Coming first, it also comes before L10's
	// disconnect, which is what releases a prober still wedged on a leg the
	// sweep removes.
	s.stopLegProbers(st, wantedProbers(plan))
	// CN10: the path watchers follow the plan from the request alone too.
	s.keepPathWatchers(st, plan)
	// CN10: the pass's NVMe walk, the sweep's list of connections and where
	// the build's lookups find their entries until the build marks it stale.
	plan.walk = s.newPassWalk(ctx)
	sweep := s.sweepCntlr(ctx, st, plan, true, nil, nil, nil)
	if !plan.wantAny {
		// SP_LEVEL_DISABLE: the sweep's wanted set is empty, so every
		// cntlr-scoped object has just gone, but the store file and the
		// in-memory desired state stay (CN19). What the sweep cannot do is
		// forget the resource histories, which are no object on the node.
		s.dropAllResKeys(st, plan)
	}
	s.build(ctx, st, plan, info, budget)
	// The converge's code names, beside its sweep's leftovers, every row of
	// its build a re-sent SyncupCntlr could cure (CN30), as a read's does,
	// but an uncreated snapshot's, which only a read names (snapshotDue):
	// a step that stopped in this pass runs again in the converge of the
	// request the worker re-sends on that code (dnv-worker.md RW4). Nothing
	// in this agent runs the converge again by itself.
	cntlrRowVerdict(plan, info, s.snapsLackingADevice(ctx, plan), sweep)
	sweep.Log(ctx, s.cntlrIdAttrs(plan)...)
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
		SideIdToPath:      make(map[uint64]*pb.ResInfo),
		RepIdToRepairInfo: make(map[uint64]*pb.CntlrInfo_RepairInfo),
		GrpIdToGrpInfo:    make(map[uint64]*pb.CntlrInfo_GrpInfo),
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
	// Legs (CN10).
	var available map[uint64]bool
	if plan.wantLeg {
		available = s.ensureLegs(ctx, st, plan, info, budget)
	} else {
		for _, lp := range plan.legs {
			info.LegIdToLeg[lp.legId] = suppressedRow(st.tracker,
				resKeyOf(resKeyLegFmt, lp.legId), lp.name)
			s.legPathRows(ctx, st, plan, lp, nil, info)
		}
	}

	// Groups (CN12) — a standby has none (architecture.md, Standby cntlr).
	// One /sys/block walk serves every group of the pass (Md.Walk), and the
	// pool concats after them take each md group's number from the same walk
	// (mdDevNo), which an assembly here brings up to date (Md.Refresh).
	plan.mdWalks = &mdWalkOnce{md: s.md}
	if plan.wantGrp {
		dead := s.deadLegs(st)
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
			detail, running, refusals, err := s.ensureGroup(
				ctx, plan, gp, available, info.LegIdToLeg, dead)
			// CN28: an OK row carries the array's state line, as a read's
			// does, when the pass read the array as it left it.
			line := ""
			if err == nil && detail != nil {
				line = mdStateLine(detail)
			}
			info.GrpIdToMdRaid[gp.grpId] = st.tracker.FromErr(
				resKeyOf(resKeyGrpFmt, gp.grpId), gp.resName, line, err)
			// CN12, The identity check: the legs whose add the running
			// array refused for another identity, and the legs of two
			// identities the assembly met, each leg row's status and
			// details kept.
			markLegRows(st.tracker, info, refusals.otherIdentity,
				pb.ResReason_RES_REASON_OTHER_IDENTITY)
			var conflict *identityConflict
			if errors.As(err, &conflict) {
				markLegRows(st.tracker, info, conflict.legIds,
					pb.ResReason_RES_REASON_IDENTITY_UNDECIDED)
			}
			if !running {
				// CN12, The md check: an array the pass left not running
				// holds no check of this incarnation's.
				forgetMdCheck(st, gp)
			}
			// CN28: the array's reading and the repair's verdict, from the
			// array as the pass left it, for a group whose row reads OK.
			// CN12: the forced start that ran its assembly acted on the
			// mark, whether or not the rest of the pass went through.
			if gp.forcedRan {
				st.forcedStarts[gp.grpId] = gp.mark.GetRevision()
			}
			if err == nil {
				// CN12, The md check: started, stopped or adopted from
				// the array as the pass left it.
				s.stepMdCheck(ctx, st, gp, detail)
				reportGrpReading(info, gp, detail, refusals,
					actedMark(st, gp), mdCheckDone(st, gp))
			} else if running && detail != nil {
				// CN12, The md check: a member reconciliation that failed
				// over a running array still steps the check, over the
				// array as the pass last read it, so a read's name of the
				// check (mdCheckDue) is one a converge cures whatever made
				// the reconciliation fail. md starts no check while a
				// resync or a recovery is due or runs, running that sync in
				// its place, and refuses a check while a sync runs; and the
				// step stops a running check once the array reads
				// degraded, so a check beside a failed member command holds
				// no copy back past this step.
				s.stepMdCheck(ctx, st, gp, detail)
			}
		}
	} else if plan.primary {
		for _, gp := range plan.grps {
			info.GrpIdToMdRaid[gp.grpId] = suppressedRow(st.tracker,
				resKeyOf(resKeyGrpFmt, gp.grpId), gp.resName)
		}
	}
	// Nothing here runs the converge again for a group the pass left short:
	// a member that is not available, a build that failed, a forced start
	// that waits or failed. A cntlr's read names each group a converge would
	// act on — an absent array the assembly would start, a member to put
	// back, a broken array to assemble again, a forced start it would carry
	// through, an md check it would start, stop or adopt, a RedundNone
	// group's dm-linear it would change (CN12, CN30) — and the worker
	// re-sends the request on that name (dnv-worker.md RW4).
	// A late member whose controller the kernel keeps reconnecting is named
	// by nothing, since no converge can make it serve.

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
		// incarnation just created. It runs only from a request that counts
		// as delivered (cntlrState.reqFromRpc), the gate CN12 puts on the
		// forced-start marks for a reason of its own, so a startup
		// reconcile arms and skips; the first post-boot SyncupCntlr whose
		// save holds then finds the flag still set — its own ensurePool
		// probe-matches the surviving device and arms nothing — and sweeps
		// with the revision-gated td_list.
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
			info.SliceIdToMeta[sp.sliceId] = suppressedRow(st.tracker,
				resKeyOf(resKeyPoolMetaFmt, sp.sliceId), sp.poolMetaName)
			info.SliceIdToData[sp.sliceId] = suppressedRow(st.tracker,
				resKeyOf(resKeyPoolDataFmt, sp.sliceId), sp.poolDataName)
			info.SliceIdToDmPool[sp.sliceId] = suppressedRow(st.tracker,
				resKeyOf(resKeyPoolFmt, sp.sliceId), sp.poolFinalName)
		}
	}
	// CN14: the snapshot-message pre-pass. Every needed slice's `create_snap`
	// goes out inside ONE suspension of the origin td's raid0, so the
	// per-slice snapshots all describe the same instant instead of one per
	// message. It has to sit exactly here: after the loop that builds the
	// pools and writes their rows, which decide the slices it sends to
	// (poolReady, poolTakesMessages), and before the thin loop — only the
	// *messages* belong inside
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
			if !s.snapshotPrePass(ctx, plan, tp, info, poolReady) {
				continue
			}
			// CN14's torn mark: the run that tore the snapshot sets it, and
			// only the request clears it (keepTornMarks). A new mark is
			// saved at once, before the thin loop, so an agent restart
			// keeps it.
			if st.tornTds[tp.tdId] {
				continue
			}
			slog.WarnContext(ctx, msgSnapshotTorn, append(
				s.cntlrIdAttrs(plan),
				slog.Uint64("td_id", tp.tdId),
				slog.Uint64("dev_id", uint64(tp.td.GetDevId())))...)
			st.tornTds[tp.tdId] = true
			s.saveTornMarks(ctx, st)
		}
	}
	for _, tp := range plan.tds {
		if !plan.primary {
			// CN14: a cntlr in any other role builds no thin volume and
			// reports a torn mark it holds in an entry with no rows, so the
			// worker reads a mark a demoted primary saved.
			if tornMark(st, tp) {
				info.TdIdToThinInfo[tp.tdId] = &pb.CntlrInfo_ThinInfo{
					SliceIdToDmThin: map[uint64]*pb.ResInfo{},
					Torn:            true,
				}
			}
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
				thinInfo.SliceIdToDmThin[sp.sliceId] = suppressedRow(
					st.tracker, key, name)
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
			info.TdIdToDmError[tp.tdId] = suppressedRow(st.tracker,
				resKeyOf(resKeyTdErrorFmt, tp.tdId), tp.errorName)
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
			info.TdIdToRaid0[tp.tdId] = suppressedRow(st.tracker,
				resKeyOf(resKeyRaid0Fmt, tp.tdId), tp.raid0Name)
		}
	}

	// Clones (CN18), then transfers (CN17). A clone's build reads its clone
	// meta group's row and its destination raid0's row, which the group and
	// td loops above wrote.
	if plan.wantClone {
		for _, cp := range plan.clones {
			s.ensureClone(ctx, st, plan, cp, info, budget)
		}
	} else if plan.primary {
		for _, cp := range plan.clones {
			s.reportCloneSuppressed(st, cp, info)
		}
	}
	// xferSteps: what ensureXfer did with each transfer, which decides its
	// ANA move and the rows written after the transfer ANA loop.
	xferSteps := make(map[uint64]xferStep, len(plan.xfers))
	if plan.wantAny {
		for _, xp := range plan.xfers {
			xferSteps[xp.xferId] = s.ensureXfer(ctx, st, plan, xp, info)
		}
	}

	// ns-devs and the host-facing nvmet objects (CN16).
	if plan.wantAny {
		// outcomes: what ensureNsDev did with each namespace's ns-dev, which
		// decides the namespace's ANA move and the group its row is compared
		// with.
		outcomes := make(map[uint64]nsDevOutcome, len(plan.namespaces))
		for _, np := range plan.namespaces {
			outcome, err := s.ensureNsDev(ctx, np, plan.cloneHold)
			outcomes[np.nsId] = outcome
			key := resKeyOf(resKeyNsDevFmt, np.nsId)
			if err == nil && outcome.held() {
				// In the probe's words, so the converge and a read
				// report the held ns-dev alike.
				info.NsIdToDmLinear[np.nsId] = st.tracker.Err(
					key, np.devName, detailsNsDevNotDesired)
				continue
			}
			// A deferred namespace is built exactly as the effective plan
			// wants it — on the td's dm-error — but nothing under it can
			// serve, so it reports PROVISIONING rather than OK ([D15]).
			if err == nil && !np.deferred {
				// Built as wanted; a parked one says so by its reason.
				details := nsDevDetails(np)
				info.NsIdToDmLinear[np.nsId] = st.tracker.SetReason(key,
					np.devName, pb.ResStatus_RES_STATUS_OK, details,
					nsDevReason(pb.ResStatus_RES_STATUS_OK, details))
				continue
			}
			info.NsIdToDmLinear[np.nsId] = deferredFromErr(
				st.tracker, np.deferred, key, np.devName,
				nsDevDetails(np), err)
		}
		// nsErrs: each namespace object's step, for the rows written after
		// the ANA loop (reportNamespaces).
		nsErrs := make(map[uint64]error, len(plan.namespaces))
		for _, ssp := range plan.subsystems {
			s.ensureSubsystem(ctx, st, plan, ssp, info, nsErrs)
		}

		// ANA rewrites to optimized last — architecture.md, Failover,
		// new_primary step 4 — and only over what the desired state wants
		// (CN16): a namespace moves to optimized in a pass whose ns-dev step
		// put its ns-dev on its serving device, or on the dm-error rules 3
		// and 4 select (nsAnaMove). One whose ns-dev sits on the td's
		// dm-error while its rules want a serving device — the stack of a
		// promotion that came before the sides' switch-over not built yet
		// ([D16]), a clone build not finished, a hold that parked
		// it, a failed step that found it parked — goes inaccessible, and its
		// hosts queue their IO rather than read IO errors. A held namespace
		// that serves takes its desired group and goes on serving from the
		// table it keeps: P2 may have moved it inaccessible before a park
		// whose load failed. A failed step that did not find its ns-dev
		// parked keeps the group: a failed read never moves an optimized
		// namespace back. A namespace the desired state wants inaccessible
		// was moved there by CN9's P1 before anything under it was touched;
		// one whose move failed there stays in the group it had, over a
		// ns-dev the pass may park, as a demotion's does; its row reads OK
		// with RES_REASON_DIFFERS, and the converge's own verdict names it
		// (cntlrRowVerdict), so the worker sends the SyncupCntlr again and
		// its P1 moves it. A namespace left short of its desired group waits
		// for what a read names: its ns-dev while a converge would change it,
		// or the layer under it that a converge would build or change (the
		// group's array, the group's dm-linear, the dm-clone, an uncreated
		// snapshot whose create message the pre-pass would send; CN30). A
		// wait on a layer no converge cures — a pool that needs a check or
		// has failed — is named by nothing and costs nothing until a new
		// revision.
		for _, np := range plan.namespaces {
			if np.anaGrpId == common.AnaGrpIdInaccessible {
				continue
			}
			outcome := outcomes[np.nsId]
			grpId, move := nsAnaMove(np, outcome)
			if !move {
				continue
			}
			if err := s.setAna(
				ctx, np.ss.nqn, np.nsIdx, grpId); err != nil {
				slog.ErrorContext(ctx, "ana transition failed",
					slog.String("nqn", np.ss.nqn),
					slog.String("error", err.Error()))
				// A move that fails leaves the namespace short of the group
				// its ns-dev step earned. Its row reads the group it has
				// against the one its ns-dev's table earns (reportNamespaces),
				// which both channels name, so the worker's re-send moves it
				// again.
			}
		}
		// The transfers' ANA, by the same rule (CN17): a transfer moves to
		// optimized only in a pass whose device step put its transfer device
		// where the desired state wants it — the linear over the origin's
		// raid0 where it is served, the error table where it is not on
		// purpose (xferAnaMove). A served transfer whose device step failed
		// goes inaccessible when its device is an error table, and keeps its
		// group otherwise. A failed export step, a device left short and a
		// move that failed are each a row both channels name (CN17, CN30),
		// as for the namespaces.
		for _, xp := range plan.xfers {
			step := xferSteps[xp.xferId]
			if plan.xferAnaGrpId(xp) == common.AnaGrpIdInaccessible {
				continue
			}
			grpId, move := plan.xferAnaMove(xp, step.outcome)
			if !move {
				continue
			}
			if err := s.setAna(ctx, xp.nqn,
				int(xp.xfer.GetOriNsIdx()), grpId); err != nil {
				slog.ErrorContext(ctx, "ana transition failed",
					slog.String("nqn", xp.nqn),
					slog.String("error", err.Error()))
			}
		}
		// The namespace rows, and the transfer rows ensureXfer left, come
		// after the ANA loops, compared as a read compares them
		// (CN28).
		s.reportNamespaces(ctx, st, plan, info, nsErrs)
		for _, xp := range plan.xfers {
			s.reportXferExport(ctx, st, plan, xp, xferSteps[xp.xferId], info)
		}
	} else {
		// SP_LEVEL_DISABLE. Everything cntlr-scoped is gone, and CN19 wants
		// that said rather than left out: a worker cannot tell an omitted
		// resource from one the agent never looked at.
		s.reportSuppressed(st, plan, info)
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
// whose message failed — waits for the next converge, which is still driven
// by the same `created == false`, and a cntlr's read names the snapshot
// while that converge would send a message (snapshotDue), so the worker's
// re-send brings it. A failed message is tolerated: the
// pool may already hold the dev_id after a crashed earlier pass, and the
// `dmsetup create` that follows decides the outcome.
//
// It reports whether this run tore the snapshot (CN14's torn mark): whether
// one slice's message made the id while another slice already held it — its
// snapshot device is there, or its message was refused with EEXIST
// (agent.IsExists), the id being in its pool already. The slices of the two
// kinds then date from different instants, since the ones already held were
// made by an earlier run, in another quiesce or in none. A message that
// never answered (agent.Reported: the soft timeout killed it) counts as one
// that made the id, since the kernel may have carried it out and the thin
// loop of the same pass then builds the device over it, after which no run
// messages that slice again; reason for no read of the id: when such a
// message made nothing, a later run makes the id beside the others, which
// tears the snapshot as well, so the mark is false only for a run whose
// quiesce failed and two or more of whose killed messages made nothing — a
// snapshot flagged as possibly inconsistent, the safe side. The snapshot
// device of every slice that is not deferred is read, one whose pool takes
// no message this pass included, since a device there holds its id
// whatever the state of its pool; only a pool that takes the message is
// sent one (poolTakesMessages). A slice the run cannot read, or whose
// message fails otherwise — or is never sent, after a failed read or
// suspend of the slice's origin thin volume — counts as neither; so does one
// with no device whose pool takes no message. A run that makes every id, or
// none, tears nothing — but for a run whose quiesce of the origin's raid0
// failed (suspendSnapOrigin): its messages still go, and when they make ids
// in two or more slices, those may date from different instants too, so the
// run tears the snapshot.
// Reason: a run that sent no message would wait for a re-send that never
// comes, as the verdict leaves a waiting snapshot's rows unnamed
// (snapsLackingADevice), and naming them would stall the origin's host IO
// every second round. One id made alone tears nothing: a later run that
// makes another beside it marks the tear by the first rule.
func (s *CnAgentServer) snapshotPrePass(
	ctx context.Context,
	plan *cntlrPlan,
	tp *tdPlan,
	info *pb.CntlrInfo,
	poolReady map[uint64]bool,
) bool {
	need, had := s.snapMessageSlices(ctx, nil, plan, tp,
		func(sp *slicePlan) bool {
			return poolReady[sp.sliceId] && poolTakesMessages(
				info.GetSliceIdToDmPool()[sp.sliceId])
		})
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
	// unquiesced: the origin's raid0 may be live and the run could not
	// suspend it, so its messages go unquiesced.
	unquiesced := false
	if origin := plan.tdByDevId[tp.td.GetOriId()]; origin != nil {
		raid0, failed := s.suspendSnapOrigin(ctx, origin)
		unquiesced = failed
		if raid0 != "" {
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
	// made: how many slices' pools a message of this run made the id in, or
	// may have, its message never answering.
	made := 0
	for _, sp := range need {
		sent, err := s.createSnapId(ctx, plan, tp, sp)
		switch {
		case err == nil:
			made++
			continue
		case agent.IsExists(err):
			had = true
		case sent && !commandRefused(err):
			made++
		}
		slog.ErrorContext(ctx, "thin-pool create message failed",
			slog.String("pool", sp.poolFinalName),
			slog.String("thin", plan.thinName(tp.tdId, sp.sliceId)),
			slog.String("error", err.Error()))
	}
	return made > 0 && had || unquiesced && made >= 2
}

// snapMessageSlices is CN14's choice of the slices of an uncreated snapshot
// td whose pool the pre-pass sends a `create_snap` (snapshotPrePass), which a
// cntlr's read names the snapshot by too (snapshotDue), so the read names it
// exactly while a converge would send a message. A slice is chosen when it
// is not deferred, its snapshot thin device reads absent, and its pool takes
// the message (takes): on a converge, a pool its pass built (ensureSlice)
// whose row takes messages (poolTakesMessages); on a read, the same row test
// beside the slice's two concat rows reading OK and the pool device not
// reading read-only (readPoolTakesMessages). had
// reports a slice whose thin device is there, which holds the id from before
// this run. The device is read from rd, the reading of a cntlr's read, or
// with no reading its own `dmsetup info`; a slice whose read did not answer
// counts as neither.
//
// CN14: there is no second filter on the origin's own thin device. The
// gateway refuses a snapshot of an origin that is not materialized in every
// slice pool (architecture.md, Thin devices), so `create_snap` cannot be
// inverted with the origin's `create_thin` — and whether *this* CN has built
// the origin's dm device is irrelevant to a message the pool metadata
// answers. Nor is it tp.deferred: that flag is plan-global
// (anySliceDeferred), and gating the messages on it would drop create_snap
// for every ready slice whenever one sibling slice is still provisioning.
func (s *CnAgentServer) snapMessageSlices(
	ctx context.Context,
	rd *cnReading,
	plan *cntlrPlan,
	tp *tdPlan,
	takes func(*slicePlan) bool,
) (need []*slicePlan, had bool) {
	for _, sp := range plan.slices {
		// [D15]: a deferred slice has no pool, so no snapshot device.
		if sp.deferred {
			continue
		}
		dev, err := s.dmInfo(ctx, rd, plan.thinName(tp.tdId, sp.sliceId))
		if err != nil {
			continue
		}
		if dev != nil {
			// The snapshot device attaches the id, so its pool holds it,
			// ready this pass or not: the id was made by an earlier run.
			had = true
			continue
		}
		if !takes(sp) {
			continue
		}
		need = append(need, sp)
	}
	return need, had
}

// poolTakesMessages is CN14's test of a slice's pool row of a pass, the
// converge's or a read's, for the pre-pass's `create_snap`: the row reads
// OK, and the pool's status line, which the row carries, shows the mode
// "rw" or "out_of_data_space". dm-thin refuses every message to a pool
// read-only for want of metadata space or for any other reason, which the
// line shows as "ro" and whose row can read OK (poolHealth), and to one that
// has failed or needs a check, whose row reads ERROR.
func poolTakesMessages(pool *pb.ResInfo) bool {
	if pool.GetStatus() != pb.ResStatus_RES_STATUS_OK {
		return false
	}
	switch thinPoolMode(pool.GetDetails()) {
	case "rw", "out_of_data_space":
		return true
	}
	return false
}

// readPoolTakesMessages is poolTakesMessages as a cntlr's read can test a
// slice's pool, by info, the read's rows, and rd, its reading: the slice's
// metadata and data concat rows read OK beside the pool row, and the pool
// device does not read read-only, in place of the converge's own test that
// it built the pool (ensureSlice). Reason for the concat rows: a concat a
// grow left unbuilt, over a late leg say, keeps the old pool's table, so the
// read's pool row reads OK while the converge builds no pool over the
// concat. A concat the converge builds a pool over whose block-layer
// read-only flag it failed to clear reads ERROR on a converge but OK on a
// read, which does not read the flag (CN28), so only the read tests the
// concat rows. Reason for the pool's flag: the converge reloads a pool that
// reads read-only and clears its flag (ensurePool), and a clear that fails
// fails the pool's step, so the pass sends no message there, while the
// read's pool row does not read the flag; the device's read, from the
// reading, does. So a pool read-only by that flag alone, whose clear a
// converge would make before it messages, gets no snapshot name, and the
// snapshot waits for a converge something else brings (cnagent.md, Known
// limits). A read that did not answer takes no message.
func (s *CnAgentServer) readPoolTakesMessages(
	ctx context.Context,
	rd *cnReading,
	info *pb.CntlrInfo,
	sp *slicePlan,
) bool {
	if info.GetSliceIdToMeta()[sp.sliceId].GetStatus() !=
		pb.ResStatus_RES_STATUS_OK ||
		info.GetSliceIdToData()[sp.sliceId].GetStatus() !=
			pb.ResStatus_RES_STATUS_OK ||
		!poolTakesMessages(info.GetSliceIdToDmPool()[sp.sliceId]) {
		return false
	}
	dev, err := s.dmInfo(ctx, rd, sp.poolFinalName)
	return err == nil && dev != nil && !dev.ReadOnly
}

// thinPoolMode is the mode word of a thin pool's `dmsetup status` line: rw,
// ro or out_of_data_space, the fifth field after the `thin-pool` token
// (common.ParseThinPoolStatus), "" for a line that carries none.
func thinPoolMode(raw string) string {
	fields := strings.Fields(raw)
	for idx, field := range fields {
		if field == "thin-pool" {
			if len(fields) > idx+5 {
				return fields[idx+5]
			}
			return ""
		}
	}
	return ""
}

// errSnapshotDue is the failure a cntlr's read names an uncreated snapshot
// by while a converge would send its `create_snap` (snapshotDue).
var errSnapshotDue = errors.New(
	"not made in every slice, and a converge sends its create message")

// snapshotDue reports whether a cntlr's read names the uncreated snapshot td
// tp (CN14, CN30): a primary at a level that builds pools, the td a snapshot
// not yet created, and a slice the converge's pre-pass would send a message
// to (snapMessageSlices) by the rows of info, the read's
// (readPoolTakesMessages). A read-only name:
// the converge's verdict leaves the snapshot's rows out
// (snapsLackingADevice), so a create that keeps failing brings a converge,
// and with it one suspension of the origin's raid0, every second round at
// most (dnv-worker.md RW4), while a pool that takes no message, or a device
// read that did not answer, names nothing.
func (s *CnAgentServer) snapshotDue(
	ctx context.Context,
	plan *cntlrPlan,
	tp *tdPlan,
	info *pb.CntlrInfo,
) bool {
	if !plan.wantPool || tp.td.GetOriId() == 0 || tp.td.GetCreated() {
		return false
	}
	need, _ := s.snapMessageSlices(ctx, plan.rd, plan, tp,
		func(sp *slicePlan) bool {
			return s.readPoolTakesMessages(ctx, plan.rd, info, sp)
		})
	return len(need) > 0
}

// msgSnapshotTorn is the Warn record of the converge whose snapshot pre-pass
// tore a snapshot and set its torn mark (CN14). The mark is reported until
// the request shows the worker recorded it; the only remedy is to delete and
// re-create the snapshot (architecture.md, Thin devices).
const msgSnapshotTorn = "snapshot torn"

// msgTornSaveFailed is the Error record of a save or removal of a cntlr's
// torn marks' file that failed (saveTornMarks). The marks stay in memory and
// are reported.
const msgTornSaveFailed = "saving the torn marks failed"

// tornMark is the torn mark a cntlr's report carries in one td's thin info,
// the same on the converge and the read, in any role (CN14, CN28): set while
// the cntlr holds a mark for the td and its request does not list it torn
// yet, created or not. A td the request lists torn reads false here even
// before a converge has dropped its mark (keepTornMarks), so a read never
// reports one either.
func tornMark(st *cntlrState, tp *tdPlan) bool {
	return !tp.td.GetTorn() && st.tornTds[tp.tdId]
}

// keepTornMarks drops the torn marks of the tds the request no longer lists,
// or lists torn: the worker has recorded the mark in the thin device record
// by then (dnv-worker.md RW19), and a td that left td_list has nothing to
// mark. A td the request lists created keeps its mark: a primary whose node
// died after the tear can come back after the new primary's created flip,
// and its saved mark is then the only record of the tear. Every converge
// runs it, from the request alone; a pass that makes no id clears nothing
// else, so the mark stays reported through any number of re-applies until
// the request shows it. It writes the marks' file again, or removes it, when
// it dropped a mark.
func (s *CnAgentServer) keepTornMarks(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
) {
	dropped := false
	for tdId := range st.tornTds {
		if tp := plan.tdById[tdId]; tp == nil || tp.td.GetTorn() {
			delete(st.tornTds, tdId)
			dropped = true
		}
	}
	if dropped {
		s.saveTornMarks(ctx, st)
	}
}

// saveTornMarks writes the cntlr's torn marks into their file in the local
// store (CN14; architecture.md, Agent local-store paths), or removes the
// file when the cntlr holds no mark. A save goes through agent.Store.Save,
// like a request's, so a disk that refuses it marks the node's store row
// (CN28). A failure is logged and changes nothing else: the marks stay in
// memory and are reported, and the next write of the file, at another tear
// or a dropped mark, holds every mark the cntlr then holds. Nothing
// remembers the failure: a removal that failed leaves a file whose marks the
// request already shows, and the startup reconcile that loads them has its
// first converge drop them and remove the file again.
func (s *CnAgentServer) saveTornMarks(ctx context.Context, st *cntlrState) {
	req := st.loadReq()
	ptr := req.GetCntlrPointer()
	path := s.nf.LocalTornPath(req.GetClusterId(), req.GetCnId(),
		ptr.GetSpId(), ptr.GetCntlrId())
	var err error
	if len(st.tornTds) == 0 {
		err = s.store.Remove(ctx, path)
	} else {
		err = s.store.Save(ctx, path, &pb.TornMarks{
			ClusterId:    req.GetClusterId(),
			CnId:         req.GetCnId(),
			CntlrPointer: ptr,
			TdIdList:     sortedIds(st.tornTds),
		})
	}
	if err != nil {
		slog.ErrorContext(ctx, msgTornSaveFailed,
			slog.String("path", path),
			slog.String("error", err.Error()))
	}
}

// keepForcedStarts drops the memory of a forced start (cntlrState.
// forcedStarts) of each group whose request carries no mark, or a mark of
// another revision, and of each group the request no longer lists: the
// worker has cleared or the operator withdrawn the mark the start acted on,
// or written a new one, which this incarnation has not acted on. Every
// converge runs it, from the request alone.
func keepForcedStarts(st *cntlrState, plan *cntlrPlan) {
	for grpId, revision := range st.forcedStarts {
		if gp := plan.grpById[grpId]; gp == nil ||
			gp.mark.GetRevision() != revision {
			delete(st.forcedStarts, grpId)
		}
	}
}

// actedMark is the revision a group's reading reports as the mark this
// incarnation acted on (CN28): the revision of the group's mark in the plan,
// when this incarnation's forced start acted on a mark of that revision and
// the held request counts as delivered (cntlrState.reqFromRpc), and zero
// otherwise, the same on a read as on a converge. A mark written or replaced
// since reads zero before any converge has dropped the memory
// (keepForcedStarts), and a request that does not count as delivered,
// whose converge acts on no mark (dropForcedStartMarks), reports none on
// either channel.
func actedMark(st *cntlrState, gp *grpPlan) uint64 {
	revision := gp.mark.GetRevision()
	if revision == 0 || !st.reqFromRpc ||
		st.forcedStarts[gp.grpId] != revision {
		return 0
	}
	return revision
}

// suspendSnapOrigin quiesces the origin td's raid0 and returns its name, or
// "" when it did not suspend it. Of those, it reports failed when the raid0
// may be live and was not suspended — its info read did not answer, or its
// suspend failed — and not when there is nothing to quiesce: no live raid0
// (a standby, or a stack this cntlr has not built yet), or one somebody else
// already holds suspended. Liveness is the whole test: a td the plan defers
// keeps whatever raid0 an earlier revision built, and that one is quiesced.
// "" is the caller's signal that it owns no resume: un-suspending a device
// another operation is holding would be silently destructive. A failed
// quiesce lets the messages go and may tear the snapshot (snapshotPrePass).
func (s *CnAgentServer) suspendSnapOrigin(
	ctx context.Context,
	origin *tdPlan,
) (string, bool) {
	dev, err := s.dm.Info(ctx, origin.raid0Name)
	if err != nil {
		slog.ErrorContext(ctx, "reading the snapshot origin raid0 failed",
			slog.String("raid0", origin.raid0Name),
			slog.String("error", err.Error()))
		return "", true
	}
	if dev == nil || dev.Suspended {
		return "", false
	}
	if err := s.dm.Suspend(ctx, origin.raid0Name); err != nil {
		slog.ErrorContext(ctx, "suspending the snapshot origin raid0 failed",
			slog.String("raid0", origin.raid0Name),
			slog.String("error", err.Error()))
		return "", true
	}
	return origin.raid0Name, false
}

// suppressedRow is the row of a resource the sp_level suppresses, on the
// converge and the read alike (CN19, CN28): RES_STATUS_MISSING with the
// details detailsSpLevel, for people, and RES_REASON_SP_LEVEL, which the
// worker's settle reads (dnv-worker.md HL2). The reason comes from the level
// the pass's plan reads, never from the text (CN29).
func suppressedRow(t *agent.ResTracker, key, resName string) *pb.ResInfo {
	return t.MissingReason(key, resName, detailsSpLevel,
		pb.ResReason_RES_REASON_SP_LEVEL)
}

// reportSuppressed fills the CntlrInfo entries the SP_LEVEL_DISABLE branch of
// the build phase skips, so every suppressed resource reads the same way
// (CN19: suppressedRow).
func (s *CnAgentServer) reportSuppressed(
	st *cntlrState,
	plan *cntlrPlan,
	info *pb.CntlrInfo,
) {
	t := st.tracker
	for _, xp := range plan.xfers {
		nsName := nsResName(xp.nqn, int(xp.xfer.GetOriNsIdx()))
		info.XferIdToDmLinear[xp.xferId] = suppressedRow(t,
			resKeyOf(resKeyXferDmFmt, xp.xferId), xp.finalName)
		info.XferIdToSubsystem[xp.xferId] = suppressedRow(t,
			resKeyOf(resKeyXferSsFmt, xp.xferId), xp.nqn)
		info.XferIdToNamespace[xp.xferId] = suppressedRow(t,
			resKeyOf(resKeyXferNsFmt, xp.xferId), nsName)
	}
	for _, np := range plan.namespaces {
		info.NsIdToDmLinear[np.nsId] = suppressedRow(t,
			resKeyOf(resKeyNsDevFmt, np.nsId), np.devName)
		info.NsIdToNamespace[np.nsId] = suppressedRow(t,
			resKeyOf(resKeyNamespaceFmt, np.nsId),
			nsResName(np.ss.nqn, np.nsIdx))
	}
	for _, ssp := range plan.subsystems {
		info.SsIdToSubsystem[ssp.ssId] = suppressedRow(t,
			resKeyOf(resKeySubsysFmt, ssp.ssId), ssp.nqn)
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

// nsDevReason is the reason of an ns-dev row: RES_REASON_PARKED beside the
// details of a parked ns-dev that reads OK, which the gateway's
// GetThinDeviceBitmap reads (CN28; architecture.md, Bitmap reads), and
// none otherwise.
func nsDevReason(status pb.ResStatus, details string) pb.ResReason {
	if status == pb.ResStatus_RES_STATUS_OK && details == detailsParked {
		return pb.ResReason_RES_REASON_PARKED
	}
	return pb.ResReason_RES_REASON_NONE
}

// reportCloneDeferred fills the two rows of a clone whose destination td or
// clone meta group is provisioning-deferred ([D15]), shared by the converge
// pass and the probe.
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
}

func (s *CnAgentServer) reportCloneSuppressed(
	st *cntlrState,
	cp *clonePlan,
	info *pb.CntlrInfo,
) {
	info.CloneIdToTarget[cp.cloneId] = suppressedRow(st.tracker,
		resKeyOf(resKeyCloneTgtFmt, cp.cloneId), cp.clone.GetSrcNqn())
	info.CloneIdToDmClone[cp.cloneId] = suppressedRow(st.tracker,
		resKeyOf(resKeyCloneDmFmt, cp.cloneId), cp.finalName)
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
		for _, key := range pathResKeys(lp) {
			t.Drop(key)
		}
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
	}
}
