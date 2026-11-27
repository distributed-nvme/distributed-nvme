package dnagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/bits"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// syncupSide implements DN8-DN14. The node read lock and this side's object
// lock are held by the caller. A request the gates refuse, or whose save
// fails (SH5), stores no observation; a saved one stores its converge's,
// unless its call is cut off, before the caller lets go of the locks (SH28).
func (s *DnAgentServer) syncupSide(
	ctx context.Context,
	key string,
	req *pb.SyncupSideRequest,
) *pb.SyncupSideReply {
	st := s.getSide(key)
	var held *pb.SyncupSideRequest
	if st != nil {
		held = st.req.Load()
	}
	reply, extentSize := s.gateSide(ctx, req, held.GetRevision())
	if reply != nil {
		return reply
	}
	// The request held so far is read here, before the save and before req
	// replaces it, and nothing of the reading is stored: the converge the
	// cutover brings is the one whose request carries no cutover_pending
	// while the held one carried it for the same migration (DN13).
	connectBudget := common.DnMigrConnectPassBudget
	if bringsCutover(held, req) {
		connectBudget = common.DnMigrCutoverConnectBudget
	}
	// The side file is persisted as soon as the gates admit the request,
	// BEFORE it becomes the desired state and before the converge (SH5). A
	// converge can run long — a cutover, a finish, a failover's fence — and
	// a request the worker cancels inside it would skip a save placed after
	// it, so the next Reconcile would converge the request before this one
	// and undo what this pass did. A request whose save fails is refused at
	// the revision the agent holds, zero for a side it does not hold: it
	// does not become the desired state, a side the agent does not hold
	// stays unknown, and no converge runs, so the side file keeps the last
	// request the agent accepted and a restart undoes nothing the node
	// serves. Only when the directory fsync after the rename alone failed
	// does the file hold this request, which a restart converges and the
	// worker wants anyway. The worker sends the request again as SH5 says
	// (dnv-worker.md RW4, RW12).
	path := s.nf.LocalSidePath(req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer().GetSpId(), req.GetSidePointer().GetSideId())
	if err := s.store.Save(ctx, path, req); err != nil {
		slog.ErrorContext(ctx, "persisting side state failed",
			slog.String("path", path),
			slog.String("error", err.Error()))
		return &pb.SyncupSideReply{
			AgentReply: agent.SaveFailedReply(err),
			Revision:   held.GetRevision(),
		}
	}
	savedAt := s.now()
	// Stored before the converge builds anything, so no claim of this
	// request can be missing from another side's pass that sees what the
	// converge builds (collectClaims). The store is atomic because those
	// passes do not hold this side's object lock.
	if st == nil {
		st = newSideState(req)
	}
	st.req.Store(req)
	st.synced.Store(true)
	s.putSide(key, st)

	info, sweep := s.convergeSide(ctx, st, extentSize, connectBudget)
	converged := sweep.Reply()
	obs := storeObs(s, ctx, &st.obs, info, converged, req.GetRevision())
	return &pb.SyncupSideReply{
		AgentReply: converged,
		Revision:   req.GetRevision(),
		SideInfo:   info,
		BmInfo:     s.bitmapInfo(st),
		ObsStamp:   s.startup.Stamped(obs.Stamp(s.now(), time.Time{})),
		SaveTime:   agent.Timestamp(savedAt),
	}
}

// bringsCutover reports whether req is the request the worker's cutover
// brings a migration destination: its migr_dst_conf no longer carries
// cutover_pending, and held, the request this process held for the side
// before it, carried the flag for the same migration. The flag comes from
// the migration's record, which nothing but the worker's cutover changes
// once it is written (dnv-worker.md RW15, RW18), so a process gives the
// longer budget to at most one converge per cutover: once that converge's
// request has replaced the held one, the process holds no request with the
// flag. A request without the flag whose save fails is refused before it
// replaces the held one (SH5), so the request the worker sends again is
// still the one the cutover brings. A process that held no request with the
// flag when the request without it came gives the longer budget to none
// (DN13). The entrances that converge a request stored earlier — DN2's
// startup re-run and the DN12 fence timer — do not come through here, so
// neither gets it either.
func bringsCutover(held, req *pb.SyncupSideRequest) bool {
	was, dst := held.GetMigrDstConf(), req.GetMigrDstConf()
	return was.GetCutoverPending() && dst != nil &&
		!dst.GetCutoverPending() && dst.GetMigrId() == was.GetMigrId()
}

// gateSide is DN8's gating of a SyncupSide against stored, the revision of
// the request this process holds for the side, zero for a side it does not
// hold: the pointer, then the revision, then the side conf (sideGates). It
// returns the refusal, or nil and the extent size of the DN the side is
// sized by. It writes nothing but the side conf gate's Error record, so
// SyncupSide can run it under the node read lock before it takes the object
// lock of a side the process does not hold, and syncupSide runs it again
// under that lock.
func (s *DnAgentServer) gateSide(
	ctx context.Context,
	req *pb.SyncupSideRequest,
	stored uint64,
) (*pb.SyncupSideReply, uint64) {
	dn := s.getDn(dnKey(req.GetClusterId(), req.GetDnId()))
	reply, extentSize, confErr := sideGates(dn, req, stored)
	if confErr != nil {
		logInvalidSideConf(ctx, req, confErr)
	}
	return reply, extentSize
}

// sideGates is gateSide's gating against dn, the state of the side's DN, nil
// for a DN the process does not hold. It returns the refusal, or nil and the
// extent size, plus the side conf gate's error when that gate refused. It
// reads nothing but dn's request and writes nothing, so beginSideSyncup runs
// it under s.mu without a record.
func sideGates(
	dn *dnState,
	req *pb.SyncupSideRequest,
	stored uint64,
) (*pb.SyncupSideReply, uint64, error) {
	// DN8 gating: SyncupDn introduces the pointer first (architecture.md,
	// `service DiskNodeAgent`).
	if dn == nil || !pointerKnown(dn.req.Load(), req.GetSidePointer()) {
		return &pb.SyncupSideReply{
			AgentReply: agent.UnknownObjectReply(
				"side pointer %s not in the dn's list",
				sidePointerText(req.GetSidePointer())),
		}, 0, nil
	}
	if reject := agent.GateRevision(stored, req.GetRevision()); reject != nil {
		return &pb.SyncupSideReply{AgentReply: reject, Revision: stored}, 0, nil
	}
	// DN8's side conf gate: a length to zero the sp worker cannot have derived
	// from a stored group. The DN request cannot change under gateSide —
	// SyncupDn replaces it under the node write lock, and gateSide's callers
	// hold the read lock — so the gate and the converge see one extent size;
	// beginSideSyncup's run of it holds no node lock, and the run under the
	// locks is the one that decides. This is the
	// last point with literally zero side effects: refusing here skips the
	// desired-state promotion, the whole converge and the local-store Save
	// (syncupSide), so a bad request cannot be replayed by the next Reconcile
	// either. The stored revision is echoed back, not the request's, so the
	// worker sees the request was not accepted.
	extentSize := dn.req.Load().GetExtentSize()
	if err := validateZeroBytes(req.GetSideConf(), extentSize); err != nil {
		return &pb.SyncupSideReply{
			AgentReply: agent.InvalidConfReply("%v", err),
			Revision:   stored,
		}, 0, err
	}
	return nil, extentSize, nil
}

// validateZeroBytes is DN8's side conf gate. side_conf.zero_bytes is the
// length to zero the sp worker derives from the side's stored group and the
// pool's block size (architecture.md, Side provisioning protocol), so a
// length of zero, one that is not a whole multiple of common.DnZeroAlign, or
// one longer than the side's ext_cnt extents of extentSize bytes can only
// come from a bad stored conf. Its texts carry the "invalid stored conf"
// prefix of every other conf refusal, so one grep finds them all. The length
// bound is skipped where extentSize is itself unusable, which is the DN's own
// refusal to report, or where the side's byte size overflows, which no
// length can exceed.
//
// It lives here and not in agent/conf.go: those validators are copies of
// model's stored-conf rules, and this one has no model twin.
func validateZeroBytes(
	conf *pb.SyncupSideRequest_SideConf,
	extentSize uint64,
) error {
	zeroBytes := conf.GetZeroBytes()
	if zeroBytes == 0 {
		return fmt.Errorf("%s: side_conf.zero_bytes is zero",
			msgInvalidStoredConf)
	}
	if zeroBytes%common.DnZeroAlign != 0 {
		return fmt.Errorf("%s: side_conf.zero_bytes %d is not a multiple of %d",
			msgInvalidStoredConf, zeroBytes, common.DnZeroAlign)
	}
	if agent.ValidateExtentSize(extentSize) != nil {
		return nil
	}
	if hi, sideBytes := bits.Mul64(
		conf.GetExtCnt(), extentSize); hi == 0 && zeroBytes > sideBytes {
		return fmt.Errorf(
			"%s: side_conf.zero_bytes %d exceeds the side's %d bytes",
			msgInvalidStoredConf, zeroBytes, sideBytes)
	}
	return nil
}

// logInvalidSideConf is the one Error record a refusal by validateZeroBytes
// writes (architecture.md, Common validation), naming what an operator has to
// go look at — the RPC gate, the converge and the probe all write it alike.
func logInvalidSideConf(
	ctx context.Context,
	req *pb.SyncupSideRequest,
	err error,
) {
	ptr := req.GetSidePointer()
	slog.ErrorContext(ctx, msgInvalidStoredConf,
		slog.Uint64("cluster_id", req.GetClusterId()),
		slog.Uint64("dn_id", req.GetDnId()),
		slog.Uint64("sp_id", ptr.GetSpId()),
		slog.Uint64("side_id", ptr.GetSideId()),
		slog.String("error", err.Error()))
}

// convergeSide brings one side to its desired state: sweep away everything
// the desired state does not want, top-down, then build what it does want
// bottom-up — side device, dm, nvmet — probing first at every step (SH16).
// It returns the SideInfo and the sweep's verdict, which is what the reply's
// agent_reply carries. connectBudget is the budget of a migration
// destination's retries of its connect within the pass (connectMigrSrc):
// DnMigrCutoverConnectBudget in the converge the cutover brings, which only
// syncupSide can tell (bringsCutover), and DnMigrConnectPassBudget in every
// other (DN13).
func (s *DnAgentServer) convergeSide(
	ctx context.Context,
	st *sideState,
	extentSize uint64,
	connectBudget time.Duration,
) (*pb.SideInfo, *agent.SweepResult) {
	info, sweep, plan := s.convergeSideSteps(
		ctx, st, extentSize, connectBudget)
	if plan == nil {
		return info, sweep
	}
	// The converge's code names, beside its sweep's leftovers, every row of
	// its build a re-sent SyncupSide could cure (SH31), as a read's does: a
	// step that stopped in this pass is retried by the worker's next round
	// (dnv-worker.md RW4).
	s.rowVerdict(ctx, plan, info, sweep)
	sweep.Log(ctx, sideIdAttrs(plan)...)
	return info, sweep
}

// convergeSideSteps is convergeSide's sweep and build. It returns the plan it
// converged, nil when DN8's side conf gate refused the stored request, which
// converges nothing and so has no verdict.
func (s *DnAgentServer) convergeSideSteps(
	ctx context.Context,
	st *sideState,
	extentSize uint64,
	connectBudget time.Duration,
) (*pb.SideInfo, *agent.SweepResult, *sidePlan) {
	// The same refusal as syncupSide's (DN8), for the entrances that do not
	// come through it: the startup Reconcile, which converges from a stored
	// file, and reconvergeSide, the DN12 fence timer's converge, which
	// re-enters with the request the side already holds. Refusing
	// before the sweep touches nothing at all: a conf fault must not destroy
	// resources (architecture.md, Common agent rules), and nothing was
	// converged or enumerated, so the pass has no verdict to give either.
	req := st.req.Load()
	if err := validateZeroBytes(req.GetSideConf(), extentSize); err != nil {
		logInvalidSideConf(ctx, req, err)
		return &pb.SideInfo{}, &agent.SweepResult{}, nil
	}
	plan := newSidePlan(s.nf, req, extentSize)
	info := &pb.SideInfo{}
	// A row made after the call is cut off moves no epoch. Every pass of the
	// side — this converge from each of its entrances, and the side's read —
	// holds the side's object lock or the node write lock, so the helpers
	// write through st.tracker with this pass bound (agent.ResTracker.Bind).
	defer st.tracker.Bind(ctx)()

	// What to remove is derived by subtracting the desired state from what
	// the node actually holds, never from memory of a past converge. A side
	// whose request is still its stored file's, no SyncupSide of this
	// process accepted, removes nothing and reports what it would remove
	// (sideState.synced; sweepSide; DN2).
	sweep := s.sweepSide(ctx, st, plan, true, nil)

	state, metaWait := s.ensureSideDev(ctx, st, plan, info)
	if !plan.wantDm {
		// SP_LEVEL_DISABLE: only the side device, its allocation record and —
		// because zeroing is bottom-layer provisioning, below the level
		// ladder — its zeroing goroutine remain (DN11; architecture.md,
		// Side provisioning protocol), and, on a migration destination, its
		// run's record in the clone-metadata area (DN13). The sweep above has
		// already removed everything else, dm-clone included: at
		// this level nothing is wanted but the side device, so the per-CN
		// linears went first and stopped holding the clone open.
		return info, sweep, plan
	}
	if state != sideDevReady {
		// The whole per-CN stack is gated together (DN9 step 4): dm-error,
		// dm-linear, nvmet, migr-src and migr-dst. A side that is still
		// provisioning has nothing above it by design, and a side whose
		// zeroed count is short of its length must not export a fresh
		// impostor of the data.
		//
		// The DN12 fence is the one thing the gate may not skip: a window
		// opened by an earlier pass is a suspension already in place, and
		// [D12] bounds it at the window plus one converge whatever the side
		// device is doing. That holds for every window this process opened,
		// adopted or ended, as far as phase 2's reload succeeds (a reload
		// fails closed, dnagent.md,
		// OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`); DN12 rule 1's
		// known limit is a suspension it knows nothing of, which settleFence
		// cannot settle; a side whose stored request DN8's side conf gate
		// refuses never reaches this point, and a side of a DN whose stored
		// extent size is unusable reaches it with no extent size, so phase 2
		// builds nothing (dnagent.md, Known limits).
		s.settleFence(ctx, st, plan)
		s.reportAboveSideDeferred(st, plan, info, metaWait)
		return info, sweep, plan
	}

	// architecture.md, Migration: the source builds its migration source
	// export first, then runs src step 1, the suspend of src step 2, and
	// only then links the export to the port (DN12). A build that fails
	// stops the pass before step 1: its migration source rows carry the
	// failure, the per-CN stacks are left where they are and only
	// reported, judged as a check round's read judges them
	// (probeCnStacks), and the fence is settled as under the DN9 gate
	// above, for the same reason: [D12]'s bound does not wait for the next
	// syncup. Until a pass's build has held no pass has run steps 1 and 2,
	// so there the side serves on; once one has, a failed build undoes
	// nothing of them or of the port link.
	if plan.migrSrc != nil {
		if !s.buildMigrSrc(ctx, st, plan, info) {
			s.settleFence(ctx, st, plan)
			s.probeCnStacks(ctx, st, plan, info, false)
			return info, sweep, plan
		}
		// Src step 1: hand IO over before the dm-linears are suspended, and
		// later reloaded onto their dm-errors.
		if plan.wantExport {
			s.moveCnAnaGroups(ctx, plan, common.AnaGrpIdInaccessible)
		}
	}

	cloneLive := false
	if plan.wantMigr {
		serving := s.ensureMigrDst(ctx, st, plan, info, connectBudget)
		switch serving.state {
		case migrCloneUp:
			cloneLive = true
		case migrCloneUnread:
			// Nothing decides the per-CN stacks this pass, so they stay
			// where they are and are only reported, judged by the
			// dm-clone's presence as a check round's read judges them in
			// the same state (probeStacks); a later converge, which the
			// worker's re-send brings while a row of the side stays named
			// (rowVerdict), converges them. The fence is settled as under
			// the DN9 gate above, for the same reason: [D12]'s bound does
			// not wait for a probe.
			s.settleFence(ctx, st, plan)
			s.probeStacks(ctx, st, plan, info, serving.cloneLive())
			return info, sweep, plan
		}
	}
	primary := s.ensureCnDm(ctx, st, plan, info, cloneLive)
	switch {
	case plan.migrSrc != nil:
		// The link puts the export in the destination's reach, and the copy
		// may read a region before a write in flight lands on it, so it
		// waits until every per-CN dm-linear of the side is proven fenced
		// (DN12).
		if gap := s.sourceFenceGap(ctx, plan); len(gap) == 0 {
			s.linkMigrSrc(ctx, st, plan, info)
		} else {
			s.holdMigrSrcLink(ctx, st, plan, info, gap)
		}
	case plan.migrSrcDeferred:
		// Serving is untouched; only the reporting differs (architecture.md,
		// Migration).
		s.reportMigrSrcDeferred(st, plan, info)
	}
	if plan.wantExport {
		s.ensureCnExports(ctx, st, plan, info, cloneLive, primary)
	}
	return info, sweep, plan
}

// ---------------------------------------------------------------------------
// Side device — allocation + architecture.md, Side provisioning protocol (DN9)
// ---------------------------------------------------------------------------

// sideDevState is the outcome of one side-device converge — the three things
// the converge matrix (dnagent.md DN9) has to distinguish, which a bool
// cannot.
type sideDevState int

const (
	// sideDevFailed is a real fault: an unreadable disk, a table that will not
	// converge, or a request claiming provisioned = true over a side whose
	// record is missing, or whose short zeroed count the converge could not
	// write up to its length to zero (row 5's count write did not hold).
	// ERROR is reported and feeds err_epoch.
	sideDevFailed sideDevState = iota
	// sideDevProvisioning is healthy but not exportable yet: the side is being
	// zeroed, or it is zeroed and the CP has not flipped its flag. No
	// err_epoch (architecture.md, Live-state reporting).
	sideDevProvisioning
	// sideDevReady is zeroed *and* released by the CP: the per-CN stacks may
	// converge.
	sideDevReady
)

// ensureSideDev implements architecture.md, Side provisioning protocol (DN9):
// allocate the side's extents in the on-disk volume table, build the aggregate
// dm-linear that concatenates them, and keep the background zeroing goroutine
// running until the side's length to zero is zeroed.
//
// That length is side_conf.zero_bytes, which the sp worker derives from the
// side's group: the whole leg for a side of a slice's meta group, which holds
// the thin pool's metadata, and the meta region plus the first data block for
// a side of a data group or of a clone meta group. Those are the parts of a
// side that must read as zeros (architecture.md, Side provisioning protocol),
// and discard-reads-zeros is not a hardware guarantee, so `blkdiscard
// --zeroout` is what actually puts the zeros there; the rest of a data side
// is never zeroed, because dm-thin never lets a host read a block it has not
// written ([D15]). The counts live in the record because zeroed is a
// property of the side's *allocation*, not of the disk extent.
//
// The six rows of the converge matrix of dnagent.md DN9 (request provisioned
// × local state):
//
//	false / absent   allocate (count 0), build the linear, start the goroutine
//	false / partial  ensure the linear, keep the goroutine
//	false / complete linear ensured, goroutine stopped, still no exports
//	true  / complete the full DN10 export converge
//	true  / partial  take the count as complete: stop the goroutine, write
//	                 the count to its end, zero nothing, then as complete
//	true  / absent   never allocate; the data is gone
//
// A migration destination adds one step to the first two rows: it takes its
// run in the node's clone-metadata area before the goroutine starts
// (holdMigrMeta, DN13). While the area has no free run for it, the side
// starts no goroutine and reports PROVISIONING with the area's text; once
// units come free the side's read names the side (zeroingVerdict), so the
// worker's re-send takes the run. Any other refusal of the run is ERROR. One
// that a re-sent request meets the same way is named by nothing; a failed
// write is named by the read (zeroingVerdict).
//
// Allocation is permitted **only** at provisioned = false. At true a missing
// record means the data is gone (a lost or foreign disk); silently
// re-allocating would present a fresh impostor as the data-bearing leg, so it
// is a hard resource error that feeds err_epoch and the replacement flows.
//
// The table, not the local store, is authoritative for placement ([D13]): the
// lookup-or-allocate below is what makes a node that lost --local-store but
// kept its disk rebuild exactly the same device.
//
// The error it returns is the area's refusal while the destination waits for
// its run, and nil otherwise: the rows above the side device carry its text
// on the destination's migr_dst_info (reportAboveSideDeferred).
func (s *DnAgentServer) ensureSideDev(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
) (sideDevState, error) {
	t := st.tracker
	name := plan.sideDevName
	// zero_bytes is never omitted (DN9). Until a record exists the only
	// length available is the request's, which is why it is seeded here and
	// overwritten from the record below — the record always wins.
	info.ZeroBytes = plan.conf.GetZeroBytes()

	// A disk node whose stored extent size is unusable is refused itself
	// (DN2), so its table is never confirmed; the side reads that refusal,
	// the disk node's own, on the converge and on the read (probeSideDev),
	// rather than the table's refusal that follows from it.
	if err := agent.ValidateExtentSize(plan.extentSize); err != nil {
		info.SideDevInfo = t.Err(resKeySideDev, name, err.Error())
		return sideDevFailed, nil
	}
	// The confirmed lookup: on a disk this node has not confirmed as the
	// side's disk node's own, a record with the side's ids is the lookup's
	// refusal, and its counts reach no reply (DN5, DN9).
	rec, ok, err := s.meta.LookupConfirmedSide(ctx, plan.clusterId,
		plan.dnId, plan.spId, plan.sideId)
	if err != nil {
		// An unreadable, corrupt or unconfirmed disk is an error, never
		// "absent".
		info.SideDevInfo = t.Err(resKeySideDev, name, err.Error())
		return sideDevFailed, nil
	}
	if !ok && plan.provisioned {
		// Row 6. The detail string is byte-exact on purpose: runbooks and the
		// integ suites grep it.
		info.SideDevInfo = t.Err(resKeySideDev, name, tagRecordMissing)
		return sideDevFailed, nil
	}
	if ok {
		// The record exists on a confirmed disk, so it — not the request — is
		// what the counters report from here on, including on the failure
		// paths below: AllocSide can still refuse (a DN9 mismatch of the
		// extent count or of the length to zero, a load that fails, or a
		// table a header check dropped meanwhile that reloads as another
		// node's) and those replies carry this node's numbers from the
		// confirmed look, not a request value the agent can prove wrong
		// (DN9). They are overwritten with the identical
		// values once AllocSide hands the record back.
		info.ZeroedBytes = sideZeroedBytes(rec)
		info.ZeroBytes = sideZeroBytes(rec)
	}
	// Rows 1-5 all go through AllocSide: it returns the existing record —
	// re-checking DN9's invariants that its extent count and its length to
	// zero, both fixed at allocation, match the request — and allocates only
	// when there is none, a case row 6 has already taken off the table. Its
	// error is reported rather than discarded, because the matrix needs
	// "could not allocate" to be distinguishable from "allocated" and from
	// "must not allocate" (DN9).
	rec, err = s.meta.AllocSide(ctx, plan.clusterId, plan.dnId, plan.spId,
		plan.sideId, plan.conf.GetExtCnt(), plan.conf.GetZeroBytes())
	if err != nil {
		info.SideDevInfo = t.Err(resKeySideDev, name, err.Error())
		return sideDevFailed, nil
	}
	zeroed, zero := sideZeroedBytes(rec), sideZeroBytes(rec)
	info.ZeroedBytes, info.ZeroBytes = zeroed, zero

	if err := s.ensureSideDm(ctx, plan, rec); err != nil {
		info.SideDevInfo = t.Err(resKeySideDev, name, err.Error())
		return sideDevFailed, nil
	}
	if !sideFullyZeroed(rec) && plan.provisioned {
		// Row 5. The worker flips provisioned only on a whole count
		// (dnv-worker.md RW18), so a partial one under it is a record that
		// rolled back one save, or a hand edit: the count is taken as
		// complete and written to its end, and nothing is zeroed — zeroing
		// again would write over the pool's data the side holds since, and
		// refusing would have a leg repair replace a leg whose data is
		// intact. A loop that still runs from the side's provisioning is
		// stopped first. A write that does not hold reads "not zeroed", which
		// the verdict names (zeroingVerdict), so the re-send tries the write
		// again, never the zeroing.
		s.stopZeroing(st)
		if err := s.meta.SetSideZeroed(ctx, plan.clusterId, plan.dnId,
			plan.spId, plan.sideId, zeroed, zero); err != nil {
			slog.ErrorContext(ctx, msgZeroedCountWriteFailed,
				slog.Uint64("sp_id", plan.spId),
				slog.Uint64("side_id", plan.sideId),
				slog.String("error", err.Error()))
			info.SideDevInfo = t.Err(resKeySideDev, name, tagNotZeroed)
			return sideDevFailed, nil
		}
		slog.WarnContext(ctx, msgZeroedCountCompleted,
			slog.Uint64("sp_id", plan.spId),
			slog.Uint64("side_id", plan.sideId),
			slog.Uint64("zeroed_bytes", zeroed),
			slog.Uint64("zero_bytes", zero))
		info.ZeroedBytes = zero
	} else if !sideFullyZeroed(rec) {
		if !plan.provisioned && plan.migrDst != nil {
			// A migration destination takes its run in the clone-metadata
			// area before it zeroes, at every level (holdMigrMeta, DN13).
			// Without the run nothing zeroes, so the worker does not flip the
			// side and the source keeps serving. Nothing of the agent's own
			// runs this step again but an agent restart's reconcile (DN2):
			// the converge of a SyncupSide does, the re-send the side's read
			// asks for once the area could supply the run among them
			// (zeroingVerdict).
			if err := s.holdMigrMeta(ctx, plan); err != nil {
				if isNoFreeRun(err) {
					info.SideDevInfo = t.Provisioning(resKeySideDev, name,
						metaRoomDetails(err))
					return sideDevProvisioning, err
				}
				info.SideDevInfo = t.Err(resKeySideDev, name, err.Error())
				return sideDevFailed, nil
			}
		}
		// Row 2: the goroutine zeroes a side the gate holds back.
		s.startZeroing(st, plan)
		if zeroErr := s.zeroingErr(st); zeroErr != nil {
			// A batch that failed or was killed reports its output, and ERROR
			// wins while that failure is outstanding; the next successful
			// batch puts the row back to PROVISIONING (DN9).
			info.SideDevInfo = t.Err(resKeySideDev, name, zeroErr.Error())
			return sideDevProvisioning, nil
		}
		info.SideDevInfo = t.Provisioning(resKeySideDev, name,
			zeroingDetails(zeroed, zero))
		return sideDevProvisioning, nil
	}

	// Rows 3 and 4: the length to zero is zeroed. Cancel-and-wait rather
	// than a bare cancel — a straggler batch's child would otherwise still
	// hold the side device open (DN9).
	s.stopZeroing(st)
	status, details := s.probeSideDm(ctx, plan, rec)
	info.SideDevInfo = t.Set(resKeySideDev, name, status, details)
	switch {
	case status != pb.ResStatus_RES_STATUS_OK:
		return sideDevFailed, nil
	case !plan.provisioned:
		// Row 3: the side is ready, but the CP has not released it yet.
		return sideDevProvisioning, nil
	default:
		return sideDevReady, nil
	}
}

// ensureSideDm converges the aggregate dm-linear against the record's extent
// runs, probe-first. It is ensureDmLinear's multi-segment sibling: dmsetup's
// --table takes one line only, so the table travels through stdin. One that
// reads read-only is reloaded and made writable (makeWritable), though the
// side device's row does not read the flag (probeSideDm); a clear that fails
// is logged, and the step holds.
func (s *DnAgentServer) ensureSideDm(
	ctx context.Context,
	plan *sidePlan,
	rec *pb.DnDiskTable_SideRecord,
) error {
	name := plan.sideDevName
	devNo, err := s.dm.DevNo(ctx, s.disk)
	if err != nil {
		return err
	}
	runs := sideRunSectors(rec, plan.extentSize)
	if len(runs) == 0 {
		return fmt.Errorf("side size is 0 (extent_size or ext_cnt unset)")
	}
	table := agent.LinearRunsTable(runs, devNo)
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return err
	}
	if dev == nil {
		return s.dm.CreateMulti(ctx, name, table)
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		return err
	}
	if !sideDmConverged(targets, runs, devNo) || dev.ReadOnly {
		if err := s.dm.ReloadMulti(ctx, name, table); err != nil {
			return err
		}
		// A clear that fails is logged and fails nothing: the devices above
		// open the side device by its number and their bios only warn, so
		// the flag stops none of them, only the zeroing of a side still
		// zeroing (DN9), and the next converge, which finds it read-only
		// still, clears it again (DN10, A read-only device is made writable).
		if err := s.makeWritable(ctx, name, dev); err != nil {
			slog.ErrorContext(ctx, "clearing the read-only flag of a side "+
				"device failed",
				slog.String("name", name),
				slog.String("error", err.Error()))
		}
		return nil
	}
	if dev.Suspended {
		return s.dm.Resume(ctx, name)
	}
	return nil
}

// sideRunSectors converts a record's extent runs into dm table geometry: run
// r starts at byte DnDataOffset + r.start*extent_size and is r.count extents
// long. DnDataOffset is 1 MiB-aligned and the gateway takes only an extent
// size that is a whole number of common.DnExtSizeUnit (architecture.md,
// Common validation), so every run lands on whole sectors and the runs sum
// to the side. A size stored before that rule that is not a whole number of
// sectors can floor runs short, so that the runs of a side sum less than the
// side and its upper tables are longer than it (architecture.md, Known
// limits); no reader judges a stored size.
func sideRunSectors(
	rec *pb.DnDiskTable_SideRecord,
	extentSize uint64,
) []agent.ExtentRunSectors {
	if extentSize == 0 {
		return nil
	}
	out := make([]agent.ExtentRunSectors, 0, len(rec.GetRunList()))
	for _, run := range rec.GetRunList() {
		if run.GetCount() == 0 {
			continue
		}
		out = append(out, agent.ExtentRunSectors{
			OffsetSectors: (common.DnDataOffset +
				run.GetStart()*extentSize) / agent.SectorSize,
			LenSectors: run.GetCount() * extentSize / agent.SectorSize,
		})
	}
	return out
}

func sideDmConverged(
	targets []common.DmTarget,
	runs []agent.ExtentRunSectors,
	devNo string,
) bool {
	if len(targets) != len(runs) {
		return false
	}
	var start uint64
	for i, target := range targets {
		if target.Type != "linear" || target.Start != start ||
			target.Length != runs[i].LenSectors ||
			len(target.Args) != 2 || target.Args[0] != devNo ||
			target.Args[1] != strconv.FormatUint(
				runs[i].OffsetSectors, 10) {
			return false
		}
		start += runs[i].LenSectors
	}
	return true
}

// ---------------------------------------------------------------------------
// Per-CN dm stacks (DN10)
// ---------------------------------------------------------------------------

// ensureCnDm converges each CN's dm-error and dm-linear, in plan.cnIds'
// order: the other CNs first, the primary last. It reports what the
// primary's step left of its path (primaryPath), which decides its
// namespace's ANA group (servingAnaGrpId). The primary's path opens only
// once every other dm-linear of the side is fenced (ensurePrimaryLinear,
// DN10).
func (s *DnAgentServer) ensureCnDm(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
	cloneLive bool,
) primaryPath {
	// The src cutover (architecture.md, Migration, src step 2) fences the
	// per-CN linears in two phases: hold
	// them suspended for at least common.SuspendSeconds, then reload them
	// onto their dm-errors ([D12]). fencing is true only during phase 1.
	fencing := plan.migrSrc != nil && s.beginFence(st)

	primary := pathClosed
	info.CnIdToDmError = make(map[uint64]*pb.ResInfo, len(plan.cnIds))
	info.CnIdToDmLinear = make(map[uint64]*pb.ResInfo, len(plan.cnIds))
	for _, cnId := range plan.cnIds {
		errName := plan.errName(cnId)
		errKey := resKeyOf(resKeyDmErrorFmt, cnId)
		err := s.ensureDmError(ctx, errName, plan.sectors)
		info.CnIdToDmError[cnId] = st.tracker.FromErr(
			errKey, errName, "", err)

		linName := plan.linearName(cnId)
		linKey := resKeyOf(resKeyDmLinearFmt, cnId)
		backing := plan.linearBacking(cnId, cloneLive)
		if err != nil {
			info.CnIdToDmLinear[cnId] = st.tracker.Err(
				linKey, linName, "dm-error missing: "+err.Error())
			// The step is skipped, and a primary's dm-linear that already
			// serves goes on serving (DN10).
			if !fencing && plan.opensPath(cnId, backing) {
				primary = s.linearPath(ctx, plan, linName, backing)
			}
			continue
		}
		if fencing {
			// architecture.md, Migration, src step 2, phase 1: hold the device
			// suspended where it is. Its namespace is already
			// AnaGrpIdInaccessible, so this
			// only absorbs stragglers — and absorbing them is the point of
			// the window.
			err = s.ensureDmLinearSuspended(ctx, linName, plan.sectors,
				plan.preFenceBacking(cnId, cloneLive))
			info.CnIdToDmLinear[cnId] = st.tracker.Set(linKey, linName,
				fenceStatus(err), fenceDetails(err))
			continue
		}
		if err = s.finishFence(ctx, plan, linName, cnId); err != nil {
			info.CnIdToDmLinear[cnId] = st.tracker.Err(
				linKey, linName, err.Error())
			if plan.opensPath(cnId, backing) {
				primary = s.linearPath(ctx, plan, linName, backing)
			}
			continue
		}
		if plan.opensPath(cnId, backing) {
			path, gap, err := s.ensurePrimaryLinear(
				ctx, plan, linName, backing)
			primary = path
			switch {
			case err != nil:
				info.CnIdToDmLinear[cnId] = st.tracker.Err(
					linKey, linName, err.Error())
			case gap != "":
				info.CnIdToDmLinear[cnId] = st.tracker.Set(linKey, linName,
					pb.ResStatus_RES_STATUS_PENDING, detailsFenceUnproven+gap)
			default:
				info.CnIdToDmLinear[cnId] = st.tracker.Ok(
					linKey, linName, "")
			}
			continue
		}
		err = s.ensureDmLinear(ctx, linName, plan.sectors, backing)
		info.CnIdToDmLinear[cnId] = st.tracker.FromErr(
			linKey, linName, "", err)
	}
	if fencing {
		s.armFenceTimer(st, plan)
	}
	return primary
}

// detailsFenceUnproven starts the row of a primary's dm-linear whose path a
// pass keeps closed because another dm-linear of the side is not proven
// fenced (DN10); the dm-linears not fenced, or the read that did not answer,
// follow. The row reads RES_STATUS_PENDING, and the side's verdict names it
// (DN16), so the worker sends the syncup again.
const detailsFenceUnproven = "path closed until the fence is proven: "

// ensurePrimaryLinear converges the primary's dm-linear onto backing, the
// device the primary serves through, with the fence first (DN10): a
// dm-linear that serves there already (linearStand.serves) stays, whatever
// the other dm-linears read (a read that did not answer must not cut a
// serving primary from its leg). Any other change that would let IO reach
// backing — a create, a reload, the resume of one suspended there, the
// finish of a reload cut after its load — waits until every other dm-linear
// of the side is proven fenced (fenceGap). Until then the step changes
// nothing that serves: an absent dm-linear is created on the primary's own
// dm-error, and one whose live table is there stays there, resumed, unless
// a reload onto backing waits on it; any other is left as it is, one
// suspended on backing, or with its reload onto backing cut after its load,
// holding the primary's own IO for the pass that opens it. It reports what
// the step left of the path, and the fence's gap when that kept it closed.
func (s *DnAgentServer) ensurePrimaryLinear(
	ctx context.Context,
	plan *sidePlan,
	name string,
	backing string,
) (primaryPath, string, error) {
	servingNo, err := s.dm.DevNo(ctx, backing)
	if err != nil {
		return s.linearPath(ctx, plan, name, backing), "", err
	}
	l, err := s.readLinear(ctx, name)
	if err != nil {
		return pathUnread, "", err
	}
	if l.serves(plan, servingNo) {
		return pathOpen, "", nil
	}
	if gap := s.fenceGap(ctx, plan); gap != "" {
		if l.cutOnto(plan, servingNo) {
			return pathOpen, gap, nil
		}
		errPath := s.nf.DmPath(plan.errName(plan.primaryCnId))
		errNo, err := s.dm.DevNo(ctx, errPath)
		switch {
		case err != nil:
			return pathClosed, gap, err
		case l.dev == nil || linearMaps(l.live, plan.sectors, errNo):
			return pathClosed, gap, s.ensureDmLinear(
				ctx, name, plan.sectors, errPath)
		}
		return pathClosed, gap, nil
	}
	if err := s.ensureDmLinear(
		ctx, name, plan.sectors, backing); err != nil {
		return s.linearPath(ctx, plan, name, backing), "", err
	}
	return pathOpen, "", nil
}

// linearStand is where a per-CN dm-linear stands: absent (dev nil), or its
// state, its live table and, while it is suspended with a table waiting, the
// waiting one.
type linearStand struct {
	dev  *agent.DmDevInfo
	live []common.DmTarget
	next []common.DmTarget
}

// readLinear reads where the dm-linear name stands.
func (s *DnAgentServer) readLinear(
	ctx context.Context,
	name string,
) (linearStand, error) {
	dev, err := s.dm.Info(ctx, name)
	if err != nil || dev == nil {
		return linearStand{}, err
	}
	l := linearStand{dev: dev}
	if l.live, err = s.dm.Table(ctx, name); err != nil {
		return linearStand{}, err
	}
	if dev.Suspended && dev.Waiting {
		if l.next, err = s.dm.WaitingTable(ctx, name); err != nil {
			return linearStand{}, err
		}
	}
	return l, nil
}

// serves says the dm-linear serves through devNo: live, resumed and
// writable on it. Writable is the converge's test, not the kernel's: the
// reads, writes and discards of a namespace already enabled over a read-only
// dm-linear go on, since the block layer only warns, but the read names such
// a dm-linear and the converge's step for it is a reload and a clear of the
// flag (makeWritable), a change of the primary's dm-linear that waits for
// the fence check as any other does (DN10). Until a pass makes it writable,
// its path counts closed on both channels, so its namespace leaves the
// optimized group while the fence check or the clear fails.
func (l linearStand) serves(plan *sidePlan, devNo string) bool {
	return l.dev != nil && !l.dev.Suspended && !l.dev.ReadOnly &&
		linearMaps(l.live, plan.sectors, devNo)
}

// cutOnto says a reload of the dm-linear onto devNo was cut after its load:
// suspended, with a table onto devNo waiting, which the next pass makes live
// (DN12 rule 2).
func (l linearStand) cutOnto(plan *sidePlan, devNo string) bool {
	return l.dev != nil && l.dev.Suspended && l.dev.Waiting &&
		linearMaps(l.next, plan.sectors, devNo)
}

// path is the primary's path through devNo as l stands (primaryPath): open
// when the dm-linear serves through devNo, or will once a reload onto it cut
// after its load is finished, and closed otherwise. The primary's namespace
// takes the optimized group only over an open path (servingAnaGrpId): a
// host sent there is served, or waits for the finish, and never meets the
// dm-error's failures. One held suspended on devNo with no table waiting —
// a path the fence keeps closed, or a reload whose load failed — is closed:
// the cntlr counts an optimized path's leg as available and would read
// through it, and its reads would wait for as long as the fence is not
// proven. A reload cut after its load is the one exception: the pass that
// loaded it opened the path by DN10's rule — after the fence check held,
// or by a finished migration's repoint, which adds no writer — and its
// group is the record a migration destination's stopped pass reads its
// last decision from (migrCloneMapped), so while a later fence check fails
// its hosts wait in it until the check holds again.
func (l linearStand) path(plan *sidePlan, devNo string) primaryPath {
	if l.serves(plan, devNo) || l.cutOnto(plan, devNo) {
		return pathOpen
	}
	return pathClosed
}

// linearPath reads the primary's path through backing (linearStand.path).
// The answer is pathUnread only where a read that did not answer leaves it
// open: a dm-linear that is absent, suspended with no table waiting, or
// live on the primary's own dm-error with none waiting is closed whatever
// backing's number would say.
func (s *DnAgentServer) linearPath(
	ctx context.Context,
	plan *sidePlan,
	name string,
	backing string,
) primaryPath {
	l, err := s.readLinear(ctx, name)
	if err != nil {
		return pathUnread
	}
	if l.dev == nil || l.dev.Suspended && !l.dev.Waiting {
		return pathClosed
	}
	if devNo, err := s.dm.DevNo(ctx, backing); err == nil {
		return l.path(plan, devNo)
	}
	errNo, err := s.dm.DevNo(ctx, s.nf.DmPath(plan.errName(plan.primaryCnId)))
	if err == nil && !l.dev.Waiting &&
		linearMaps(l.live, plan.sectors, errNo) {
		return pathClosed
	}
	return pathUnread
}

// keptAnaGrpId is the group a namespace keeps (anaGrpKeep): the one it is
// in, and inaccessible for one that is not there yet or whose group does not
// answer, which the namespace's own step then reads again and reports.
func (s *DnAgentServer) keptAnaGrpId(ctx context.Context, nqn string) int {
	grpId, present, err := s.nvmet.NsAnaGrpId(ctx, nqn, sideNsid)
	if err != nil || !present {
		return common.AnaGrpIdInaccessible
	}
	return grpId
}

// fenceGap reads, from a fresh `dmsetup ls`, whether every per-CN dm-linear
// of the side but the primary's is fenced (DN10), the ones the desired state
// no longer names included: absent, or with its live table on its own CN's
// dm-error. A table waiting on another CN's suspended dm-linear does not
// count: nothing but its own CN's promotion finishes it (unfenceLinears),
// and a resume drops it (agent.Dm.Resume). It returns "" when that is
// proven, and otherwise what is not: each dm-linear that is not fenced, or
// whose read did not answer, which proves nothing.
func (s *DnAgentServer) fenceGap(ctx context.Context, plan *sidePlan) string {
	dms, err := s.dm.List(ctx)
	if err != nil {
		return "dmsetup ls: " + err.Error()
	}
	type other struct {
		name string
		cnId uint64
	}
	var others []other
	for name := range dms {
		dn, ok := common.ParseDmName(name)
		if !ok || dn.Kind != common.DmKindDnLinear ||
			dn.ClusterId != plan.clusterId || dn.NodeId != plan.dnId ||
			len(dn.Ids) != 3 || dn.Ids[0] != plan.spId ||
			dn.Ids[1] != plan.sideId || dn.Ids[2] == plan.primaryCnId {
			continue
		}
		others = append(others, other{name, dn.Ids[2]})
	}
	sort.Slice(others, func(i, j int) bool {
		return others[i].name < others[j].name
	})
	var gaps []string
	for _, o := range others {
		if why := s.linearUnfenced(ctx, plan, o.name, o.cnId); why != "" {
			gaps = append(gaps, o.name+": "+why)
		}
	}
	return strings.Join(gaps, "; ")
}

// linearUnfenced says why the per-CN dm-linear name of cnId is not proven
// fenced, or "" when it is: absent, or with every target of its live table on
// cnId's own dm-error, suspended or not. A dm-linear suspended on the side
// device is not fenced: its resume would let the IO it holds reach the
// side.
func (s *DnAgentServer) linearUnfenced(
	ctx context.Context,
	plan *sidePlan,
	name string,
	cnId uint64,
) string {
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return err.Error()
	}
	if dev == nil {
		return ""
	}
	errNo, err := s.dm.DevNo(ctx, s.nf.DmPath(plan.errName(cnId)))
	if err != nil {
		return err.Error()
	}
	live, err := s.dm.Table(ctx, name)
	if err != nil {
		return err.Error()
	}
	if !mapsOnly(live, errNo) {
		return "not on its dm-error"
	}
	return ""
}

// mapsOnly reports whether every target of a table is a linear target over
// devNo, and there is at least one.
func mapsOnly(targets []common.DmTarget, devNo string) bool {
	if len(targets) == 0 {
		return false
	}
	for _, target := range targets {
		if target.Type != "linear" || len(target.Args) == 0 ||
			target.Args[0] != devNo {
			return false
		}
	}
	return true
}

// unfenceLinears ends the suspension of every per-CN dm-linear of this side
// it finds suspended, onto a table the IO the linear holds may go to. A
// linear whose own dm-error waits is a fence cut after its load: that table
// is made live (Dm.FinishReload), whatever the CN is now, so the IO held at
// the fence fails (finishFence). Any other goes onto, for the primary, the
// device it serves through — the side device, or on a migration destination
// its dm-clone — and then its own dm-error; for any other CN its own
// dm-error alone, where the IO fails. It tries those targets in that order,
// and for each takes the live table when it maps the target, resumed with
// any waiting table dropped (Dm.Resume), and otherwise a waiting table that
// maps it, made live (Dm.FinishReload): a promotion cut after its load,
// whose held IO is then served. So a fenced linear of a migration that is
// over ends as the end of the window would have ended it, only without the
// dm-error swap the side no longer needs: a standby's held IO fails, the
// primary's reaches the device it serves through.
//
// Any other suspended linear is left suspended: one that is not the primary
// and whose live table maps the side device, with no waiting dm-error — a
// demotion whose load failed — whose resume would let the old primary's
// queued IO write the side's data; and one whose tables or whose targets'
// numbers cannot be read. The build phase reloads a wanted one onto the
// table it wants (ensureDmLinear), and the sweep retires one the desired
// state no longer names onto its dm-error before it removes it
// (retireIfSuspended); each reload fails what a fenced linear holds ([D12]).
// One neither reaches stays suspended and queues its IO, the closed side.
//
// The set of devices comes from the ENUMERATION, not from a remembered cn
// list: a linear built for a CN that has since left standby_id_list is
// exactly the one a remembered list would miss.
func (s *DnAgentServer) unfenceLinears(
	ctx context.Context,
	plan *sidePlan,
	actual *dnActual,
) {
	names := make([]string, 0)
	for name, dn := range actual.ours {
		if dn.Kind == common.DmKindDnLinear && len(dn.Ids) == 3 &&
			dn.Ids[0] == plan.spId && dn.Ids[1] == plan.sideId {
			names = append(names, name)
		}
	}
	// The primary's last, so its path opens only after every other has been
	// fenced (DN10).
	primary := plan.linearName(plan.primaryCnId)
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == primary) != (names[j] == primary) {
			return names[j] == primary
		}
		return names[i] < names[j]
	})
	for _, name := range names {
		dev, err := s.dm.Info(ctx, name)
		if err != nil || dev == nil || !dev.Suspended {
			continue
		}
		finish, resume, opens, reason := s.unfenceStep(
			ctx, plan, name, actual.ours[name].Ids[2], dev.Waiting)
		if opens {
			if gap := s.fenceGap(ctx, plan); gap != "" {
				finish, resume, reason = false, false,
					detailsFenceUnproven+gap
			}
		}
		switch {
		case finish:
			err = s.dm.FinishReload(ctx, name)
		case resume:
			err = s.dm.Resume(ctx, name)
		default:
			slog.InfoContext(ctx, "leaving a suspended dm-linear "+
				"for its reload",
				slog.String("name", name),
				slog.String("reason", reason))
			continue
		}
		if err != nil {
			slog.ErrorContext(ctx, "resuming a fenced dm-linear failed",
				slog.String("name", name),
				slog.String("error", err.Error()))
		}
	}
}

// waitsOnOwnError reports whether the table waiting on name, the per-CN
// dm-linear of cnId on this side, maps that CN's own dm-error: a fence whose
// reload was cut after its load.
func (s *DnAgentServer) waitsOnOwnError(
	ctx context.Context,
	plan *sidePlan,
	name string,
	cnId uint64,
) (bool, error) {
	next, err := s.dm.WaitingTable(ctx, name)
	if err != nil {
		return false, err
	}
	errNo, err := s.dm.DevNo(ctx, s.nf.DmPath(plan.errName(cnId)))
	if err != nil {
		return false, err
	}
	return linearMaps(next, plan.sectors, errNo), nil
}

// finishFence ends a fence cut after its load on the per-CN dm-linear of
// cnId, before the build phase converges it: when the linear is suspended
// with its own dm-error waiting, it makes that table live
// (Dm.FinishReload), which fails the IO the linear held at the fence. A
// resume of the live table instead would drop the dm-error and release that
// IO onto the side's data, though another CN may have written there since,
// as on a failback after a demotion whose resume did not run. A linear with
// a table waiting whose read does not answer is an error: the build leaves
// it suspended for this pass.
func (s *DnAgentServer) finishFence(
	ctx context.Context,
	plan *sidePlan,
	name string,
	cnId uint64,
) error {
	dev, err := s.dm.Info(ctx, name)
	if err != nil || dev == nil || !dev.Suspended || !dev.Waiting {
		return err
	}
	fenced, err := s.waitsOnOwnError(ctx, plan, name, cnId)
	if err != nil || !fenced {
		return err
	}
	return s.dm.FinishReload(ctx, name)
}

// unfenceStep is unfenceLinears' choice for the suspended linear name of
// cnId: finish its waiting table, resume its live one, or neither, with the
// reason. waiting says a table waits on it. opens says the table it makes
// live maps the device the primary serves through, which DN10's fence check
// then guards.
func (s *DnAgentServer) unfenceStep(
	ctx context.Context,
	plan *sidePlan,
	name string,
	cnId uint64,
	waiting bool,
) (finish bool, resume bool, opens bool, reason string) {
	// A fence in progress first: a dm-error waiting on the linear is a
	// reload onto it cut after its load, and the IO the linear held was
	// held at that fence, so it must fail, whatever the CN is now.
	if waiting {
		fenced, err := s.waitsOnOwnError(ctx, plan, name, cnId)
		if err != nil {
			return false, false, false, err.Error()
		}
		if fenced {
			return true, false, false, ""
		}
	}
	targets := []string{s.nf.DmPath(plan.errName(cnId))}
	if cnId == plan.primaryCnId {
		serving := plan.sideDevPath
		if plan.migrDst != nil {
			serving = s.nf.DmPath(plan.migrFinalName())
		}
		targets = []string{serving, targets[0]}
	}
	live, err := s.dm.Table(ctx, name)
	if err != nil {
		return false, false, false, err.Error()
	}
	var next []common.DmTarget
	if waiting {
		if next, err = s.dm.WaitingTable(ctx, name); err != nil {
			return false, false, false, err.Error()
		}
	}
	var unread error
	for i, target := range targets {
		devNo, err := s.dm.DevNo(ctx, target)
		if err != nil {
			// With a table waiting, a number that does not answer stops the
			// choice: a later target taken in its stead would drop that
			// table, which may map this one, unread. Without one, the next
			// target is tried, as for a dm-clone not built yet.
			if waiting {
				return false, false, false, err.Error()
			}
			unread = err
			continue
		}
		serving := cnId == plan.primaryCnId && i == 0
		switch {
		case linearMaps(live, plan.sectors, devNo):
			return false, true, serving, ""
		case waiting && linearMaps(next, plan.sectors, devNo):
			return true, false, serving, ""
		}
	}
	if unread != nil {
		return false, false, false, unread.Error()
	}
	return false, false, false,
		"neither table maps a device its IO may go to"
}

func fenceStatus(err error) pb.ResStatus {
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR
	}
	return pb.ResStatus_RES_STATUS_OK
}

func fenceDetails(err error) string {
	if err != nil {
		return err.Error()
	}
	return fenceSuspendedDetails
}

// fenceSuspendedDetails is what a per-CN dm-linear reports while it is inside
// the cutover grace window (architecture.md, Migration, src step 2) — an
// expected, time-bounded state, not a fault.
const fenceSuspendedDetails = "suspended (migration cutover grace window)"

// ensureDmLinearSuspended is phase 1 of the fence: the device must exist and
// be suspended, on whatever table it carries — its pre-fence one, except in
// DN12 rule 1's known-limit second window, which a restart opens over the
// dm-error phase 2 had already installed. It never swaps the table — the
// swap onto dm-error is phase 2, and doing it here would defeat the window
// by erroring the very IO the window exists to absorb.
func (s *DnAgentServer) ensureDmLinearSuspended(
	ctx context.Context,
	name string,
	sectors uint64,
	backingPath string,
) error {
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return err
	}
	if dev == nil {
		// Nothing was ever exported through it, so there is no in-flight IO
		// to absorb; build it and suspend it so the window still applies
		// uniformly across the side's CNs.
		if err := s.ensureDmLinear(
			ctx, name, sectors, backingPath); err != nil {
			return err
		}
		return s.dm.Suspend(ctx, name)
	}
	if dev.Suspended {
		return nil
	}
	return s.dm.Suspend(ctx, name)
}

// diskDevNo is the disk's number, the device ErrorWcTable names, read once
// and kept (diskNo); "" while no read of it has answered, which leaves the
// dm-error on the one-target table, logged.
func (s *DnAgentServer) diskDevNo(ctx context.Context) string {
	s.mu.Lock()
	no := s.diskNo
	s.mu.Unlock()
	if no != "" {
		return no
	}
	no, err := s.dm.DevNo(ctx, s.disk)
	if err != nil {
		slog.WarnContext(ctx, "the disk's number is unread, so a dm-error "+
			"reports no write cache",
			slog.String("disk", s.disk),
			slog.String("error", err.Error()))
		return ""
	}
	s.mu.Lock()
	s.diskNo = no
	s.mu.Unlock()
	return no
}

// ensureDmError converges a per-CN dm-error (DN10). Its table reports the
// write cache of the node's disk (agent.ErrorWcTable), so a cntlr's path
// that the node serves from it reads, when its controller node scans it, the
// write cache the side's own device has, a dm-linear over the same disk: a
// failover that puts that device under the path then changes nothing the
// controller node read (dnagent.md DN10, The dm-error reports the disk's
// write cache). A migration's cutover puts a dm-clone there, which reports a
// write cache whatever the disk's; the primary's rescan reads it (cnagent.md
// CN10). The one-target table of an older agent is loaded again into this
// one; with the disk's number unread it is the table wanted, and so is this
// one.
func (s *DnAgentServer) ensureDmError(
	ctx context.Context,
	name string,
	sectors uint64,
) error {
	if sectors == 0 {
		return fmt.Errorf("side size is 0 (extent_size or ext_cnt unset)")
	}
	diskNo := s.diskDevNo(ctx)
	table := agent.ErrorWcTable(sectors, diskNo)
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return err
	}
	if dev == nil {
		return s.dm.CreateMulti(ctx, name, table)
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		return err
	}
	switch agent.ErrorShapeOf(targets, sectors) {
	case agent.ErrorShapeWc:
	case agent.ErrorShapePlain:
		if diskNo != "" && sectors > 2*common.DmErrorWcSectors {
			return s.dm.ReloadMulti(ctx, name, table)
		}
	default:
		return s.dm.ReloadMulti(ctx, name, table)
	}
	// Suspended on the table it wants — an interrupted reload, or a failed
	// one whose old table is wanted again, since a reload fails closed
	// (dnagent.md, OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`) — it is
	// resumed ([D12]).
	if dev.Suspended {
		return s.dm.Resume(ctx, name)
	}
	return nil
}

// ensureDmLinear converges one dm-linear onto its desired backing device.
// A primary_cn_id change or a migration switch reaches the data plane as
// exactly this table reload plus an ana_grpid rewrite — nothing else (DN10).
//
// nvmet opens the backing device read-write; dnv never makes any DN device
// read-only ([D11]). One that reads read-only, which an operator's
// `blockdev --setro` makes, is reloaded and made writable (makeWritable).
func (s *DnAgentServer) ensureDmLinear(
	ctx context.Context,
	name string,
	sectors uint64,
	backingPath string,
) error {
	if sectors == 0 {
		return fmt.Errorf("side size is 0 (extent_size or ext_cnt unset)")
	}
	devNo, err := s.dm.DevNo(ctx, backingPath)
	if err != nil {
		return err
	}
	table := agent.LinearTable(sectors, devNo, 0)
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return err
	}
	if dev == nil {
		return s.dm.Create(ctx, name, table)
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		return err
	}
	if !linearMaps(targets, sectors, devNo) || dev.ReadOnly {
		if err := s.dm.Reload(ctx, name, table); err != nil {
			return err
		}
		return s.makeWritable(ctx, name, dev)
	}
	// A device found suspended — one a crash caught mid-reload, or one a
	// failed reload left suspended on the table that is wanted again (a
	// reload fails closed, dnagent.md,
	// OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`) — must converge back
	// to resumed.
	if dev.Suspended {
		return s.dm.Resume(ctx, name)
	}
	return nil
}

// makeWritable ends the reload a converge gives a device it found as dev:
// one that read read-only has the block layer's own read-only flag cleared
// (Dm.SetWritable), since dmsetup reads a device read-only while its table's
// mode or that flag says so, and a reload of a read-write table clears only
// the first (DN10, A read-only device is made writable). A clear that fails
// comes back as a writableError, which the caller decides by.
func (s *DnAgentServer) makeWritable(
	ctx context.Context,
	name string,
	dev *agent.DmDevInfo,
) error {
	if !dev.ReadOnly {
		return nil
	}
	if err := s.dm.SetWritable(ctx, s.nf.DmPath(name)); err != nil {
		return writableError{err}
	}
	return nil
}

// writableError is a clear of a device's read-only flag that failed after
// the reload before it went through (makeWritable): the device is there,
// resumed and on the table it wants. Its text is blockdev's error.
type writableError struct{ err error }

func (e writableError) Error() string { return e.err.Error() }

func (e writableError) Unwrap() error { return e.err }

// onlyClearFailed reports whether err is a failed clear of a read-only flag
// and nothing else (writableError).
func onlyClearFailed(err error) bool {
	var clear writableError
	return errors.As(err, &clear)
}

// linearMaps reports whether a live table is the one ensureDmLinear builds
// over devNo: a single "linear" target of the side's whole size, from
// sector 0 of the device.
func linearMaps(targets []common.DmTarget, sectors uint64, devNo string) bool {
	return len(targets) == 1 && targets[0].Type == "linear" &&
		targets[0].Length == sectors && len(targets[0].Args) == 2 &&
		targets[0].Args[0] == devNo && targets[0].Args[1] == "0"
}

// ---------------------------------------------------------------------------
// Per-CN nvmet exports (DN10)
// ---------------------------------------------------------------------------

func (s *DnAgentServer) ensureCnExports(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
	cloneLive bool,
	primary primaryPath,
) {
	uuid, nguid := plan.nsIdentity()
	cntlidMin, cntlidMax := plan.cntlidRange()
	info.CnIdToNvmeof = make(map[uint64]*pb.ResInfo, len(plan.cnIds))
	for _, cnId := range plan.cnIds {
		nqn := plan.sideNqn(cnId)
		key := resKeyOf(resKeyNvmeofFmt, cnId)
		subsys := agent.SubsysConf{
			Nqn:          nqn,
			Serial:       fmt.Sprintf(common.IdKeyFmt, plan.legId),
			Model:        common.NvmeModel,
			CntlidMin:    cntlidMin,
			CntlidMax:    cntlidMax,
			AllowedHosts: []string{plan.cnHostNqn(cnId)},
		}
		ns := agent.NsConf{
			Nqn:        nqn,
			Nsid:       sideNsid,
			DevicePath: s.nf.DmPath(plan.linearName(cnId)),
			Uuid:       uuid,
			Nguid:      nguid,
			AnaGrpId:   plan.servingAnaGrpId(cnId, cloneLive, primary),
		}
		if ns.AnaGrpId == anaGrpKeep {
			ns.AnaGrpId = s.keptAnaGrpId(ctx, nqn)
		}
		err := s.ensureExport(ctx, subsys, ns)
		if err == nil {
			info.CnIdToNvmeof[cnId] = st.tracker.Ok(key, nqn, "")
			continue
		}
		// A namespace the pass wants inaccessible is moved there even when
		// the export's step stopped before its own move — a read of another
		// attribute that did not answer, say — as no CN may be pointed at a
		// path that returns errors or that the fence holds closed (DN10).
		if ns.AnaGrpId == common.AnaGrpIdInaccessible {
			s.moveAnaGroup(ctx, nqn, ns.AnaGrpId)
		}
		// A step that failed is read as a read reads it, so an export that
		// serves and differs in its ANA group alone — its move failed —
		// reads as on the other channel, RES_STATUS_OK with the difference
		// (SH31), and one whose link failed because the port itself is not
		// there reads "the port is not there", which the side's verdict
		// leaves to the DN's; any other keeps the step's own error.
		status, details, reason := s.probeExport(ctx, subsys, ns)
		if status == pb.ResStatus_RES_STATUS_OK ||
			details == detailsPortMissing {
			info.CnIdToNvmeof[cnId] = st.tracker.SetReason(
				key, nqn, status, details, reason)
			continue
		}
		info.CnIdToNvmeof[cnId] = st.tracker.Err(key, nqn, err.Error())
	}
}

// ensureExport converges one subsystem, its single namespace and its port
// link. Every step probes first: re-linking a live port↔subsystem link or
// re-enabling a live namespace stalls host IO (SH16).
func (s *DnAgentServer) ensureExport(
	ctx context.Context,
	subsys agent.SubsysConf,
	ns agent.NsConf,
) error {
	if err := s.buildExport(ctx, subsys, ns); err != nil {
		return err
	}
	return s.nvmet.EnsurePortLink(
		ctx, s.port.PortId, subsys.Nqn, s.port.QidMax)
}

// buildExport converges one subsystem and its single namespace, probe-first
// (SH16), and leaves its port link alone: the kernel finds a subsystem for a
// connect only through the port's links, so an export built here is out of
// every host's reach until its link is made.
func (s *DnAgentServer) buildExport(
	ctx context.Context,
	subsys agent.SubsysConf,
	ns agent.NsConf,
) error {
	if err := s.nvmet.EnsureSubsystem(ctx, subsys); err != nil {
		return err
	}
	return s.nvmet.EnsureNamespace(ctx, ns)
}

// moveCnAnaGroups rewrites the ana_grpid of every existing per-CN namespace
// — the whole of an ANA transition ([D4]).
func (s *DnAgentServer) moveCnAnaGroups(
	ctx context.Context,
	plan *sidePlan,
	grpId int,
) {
	for _, cnId := range plan.cnIds {
		s.moveAnaGroup(ctx, plan.sideNqn(cnId), grpId)
	}
}

// moveAnaGroup moves the per-CN namespace of nqn into grpId, when it is
// there and in another group. The group alone is read, strictly. A read
// that did not answer leaves the move tried all the same: its callers move
// to inaccessible, the safe direction.
func (s *DnAgentServer) moveAnaGroup(
	ctx context.Context,
	nqn string,
	grpId int,
) {
	cur, present, err := s.nvmet.NsAnaGrpId(ctx, nqn, sideNsid)
	if err == nil && (!present || cur == grpId) {
		return
	}
	if err := s.nvmet.SetNsAnaGrpId(ctx, nqn, sideNsid, grpId); err != nil {
		slog.ErrorContext(ctx, "ana transition failed",
			slog.String("nqn", nqn),
			slog.Int("ana_grpid", grpId),
			slog.String("error", err.Error()))
	}
}

// ---------------------------------------------------------------------------
// Provisioning-deferred reporting (architecture.md, Side provisioning protocol)
// ---------------------------------------------------------------------------

// reportAboveSideDeferred fills every row above the side device with
// RES_STATUS_PROVISIONING. It probes nothing: the rows report that DN9's gate
// is closed, not what exists above the side device. Behind the closed gate
// nothing above the side device is created except by settleFence: its DN12
// phase 2 creates a migration source's per-CN dm-error and dm-linear where
// they are absent, even on a side whose gate never opened, since a level
// with no export layer ends the window (endFence).
// A gate that closes again on a side that already exports (DN9) removes
// nothing, because the sweep's wanted set ignores the gate, so the resources
// behind these rows can still exist and serve.
//
// PROVISIONING never feeds err_epoch (architecture.md, Live-state reporting),
// which is the point: one cause is
// reported once — on side_dev_info, as ERROR when it really is one — instead
// of multiplying a single fault across every per-CN stack (DN10).
//
// It fills exactly the keys probeAboveSideDev fills, so a side that becomes
// exportable later replaces them one for one.
//
// metaWait is the clone-metadata area's refusal while a migration destination
// waits for its run before zeroing (ensureSideDev, probeSideDev), and nil
// otherwise: its two migr_dst_info rows then carry the area's text in place
// of tagProvisioningWait, the same wait side_dev_info reports.
func (s *DnAgentServer) reportAboveSideDeferred(
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
	metaWait error,
) {
	t := st.tracker
	info.CnIdToDmError = make(map[uint64]*pb.ResInfo, len(plan.cnIds))
	info.CnIdToDmLinear = make(map[uint64]*pb.ResInfo, len(plan.cnIds))
	if plan.wantExport {
		info.CnIdToNvmeof = make(map[uint64]*pb.ResInfo, len(plan.cnIds))
	}
	for _, cnId := range plan.cnIds {
		errName := plan.errName(cnId)
		info.CnIdToDmError[cnId] = t.Provisioning(
			resKeyOf(resKeyDmErrorFmt, cnId), errName, tagProvisioningWait)
		linName := plan.linearName(cnId)
		info.CnIdToDmLinear[cnId] = t.Provisioning(
			resKeyOf(resKeyDmLinearFmt, cnId), linName, tagProvisioningWait)
		if plan.wantExport {
			nqn := plan.sideNqn(cnId)
			info.CnIdToNvmeof[cnId] = t.Provisioning(
				resKeyOf(resKeyNvmeofFmt, cnId), nqn, tagProvisioningWait)
		}
	}
	if plan.migrSrc != nil || plan.migrSrcDeferred {
		s.reportMigrSrcDeferred(st, plan, info)
	}
	if plan.wantMigr {
		// The destination provisions first, under this same protocol
		// (architecture.md, Migration): before it zeroes it takes only its
		// run's record in the clone-metadata area (holdMigrMeta), and behind
		// the closed gate no wrapper, source connection or dm-clone is built
		// for it. A gate that closes again removes none of them that a pass
		// built while it was open (the sweep's wanted set ignores the gate,
		// DN13), and these two rows do not probe them, so PROVISIONING here
		// does not mean that no dm-clone exists.
		details := tagProvisioningWait
		if metaWait != nil {
			details = metaRoomDetails(metaWait)
		}
		dstInfo := &pb.SideInfo_MigrDstInfo{}
		info.MigrDstInfo = dstInfo
		nqn := plan.srcNqnOfDst()
		dstInfo.TargetInfo = t.Provisioning(
			resKeyMigrDstTarget, nqn, details)
		dstInfo.DmCloneInfo = t.Provisioning(
			resKeyMigrDstClone, plan.migrFinalName(), details)
	}
}

// reportMigrSrcDeferred fills the migration-source rows a side would publish
// once the migration's cutover has started.
//
// `migr_src_conf.dst_provisioned = false` makes the source behave **exactly**
// as if migr_src_conf were absent (architecture.md, Migration): it keeps
// serving, it does not fence, and it exports nothing to the destination.
// Without that equivalence
// the source would fence the primary's path the moment the migration was
// created, and the leg would have no serving path for the whole zeroing
// window and the wait for the cutover. The only visible difference is right
// here — the would-be rows report PROVISIONING instead of being omitted:
// "waiting for the cutover" while the request carries cutover_pending on a
// deferred role, and "side provisioning" otherwise (DN12). The converge and
// the probe both report through it.
func (s *DnAgentServer) reportMigrSrcDeferred(
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
) {
	if plan.migrSrcRaw == nil {
		return
	}
	t := st.tracker
	details := tagProvisioningWait
	if plan.migrSrcDeferred && plan.migrSrcRaw.GetCutoverPending() {
		details = tagCutoverWait
	}
	// migrSrc is nil while the role is deferred, so the names come from a plan
	// with the raw conf re-attached.
	srcPlan := plan.withMigrSrc(plan.migrSrcRaw)
	srcInfo := &pb.SideInfo_MigrSrcInfo{}
	info.MigrSrcInfo = srcInfo
	srcInfo.DmLinearInfo = t.Provisioning(
		resKeyMigrSrcDm, srcPlan.migrSrcName(), details)
	if plan.wantExport {
		srcInfo.NvmeofInfo = t.Provisioning(
			resKeyMigrSrcNvmeof, srcPlan.migrSrcNqn(), details)
	}
}

// ---------------------------------------------------------------------------
// Teardown
// ---------------------------------------------------------------------------

func (s *DnAgentServer) removeExport(ctx context.Context, nqn string) {
	if err := s.nvmet.RemoveSubsystem(
		ctx, s.port.PortId, nqn); err != nil {
		slog.ErrorContext(ctx, "removing nvmet subsystem failed",
			slog.String("nqn", nqn),
			slog.String("error", err.Error()))
	}
}

// disconnect drops an nvme host connection, probing first so tearing down a
// side that never connected stays silent.
func (s *DnAgentServer) disconnect(ctx context.Context, nqn string) {
	state, err := s.host.ListSubsys(ctx, nqn)
	if err != nil {
		slog.ErrorContext(ctx, "probing nvme subsystems failed",
			slog.String("nqn", nqn),
			slog.String("error", err.Error()))
		return
	}
	// A subsystem with no controller (HasCtrl) is no connection — one the
	// kernel keeps after its last controller went, which it does while
	// something holds its multipath head open, or one that lists only
	// controllers already deleted: it has no controller left for
	// `nvme disconnect` to delete.
	if !state.HasCtrl() {
		return
	}
	if err := s.host.Disconnect(ctx, nqn); err != nil {
		slog.ErrorContext(ctx, "nvme disconnect failed",
			slog.String("nqn", nqn),
			slog.String("error", err.Error()))
	}
}

// removeDm removes a dm device if it exists and reports whether it is gone
// afterwards. The result matters for the [D13] records: an extent record must
// never be freed while a device still maps it, or the next allocation would
// hand those extents to another side while the old device is still live.
func (s *DnAgentServer) removeDm(ctx context.Context, name string) bool {
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		slog.ErrorContext(ctx, "probing dm device failed",
			slog.String("name", name),
			slog.String("error", err.Error()))
		return false
	}
	if dev == nil {
		return true
	}
	// `dmsetup remove` does not succeed on a suspended device. A per-CN
	// linear the DN12 fence suspended never gets here that way — the
	// sweep's pre-step has resumed it (a request that ends the source role)
	// or its P0 has put it on its dm-error — so what the agent itself leaves
	// suspended here is a device whose reload was cut off between its
	// suspend and its resume. Resume first; the queued IO drains against
	// whatever table is live.
	if dev.Suspended {
		if err := s.dm.Resume(ctx, name); err != nil {
			slog.ErrorContext(ctx, "resuming a suspended dm device failed",
				slog.String("name", name),
				slog.String("error", err.Error()))
			return false
		}
	}
	if err := s.dm.Remove(ctx, name); err != nil {
		slog.ErrorContext(ctx, "removing dm device failed",
			slog.String("name", name),
			slog.String("error", err.Error()))
		return false
	}
	return true
}
