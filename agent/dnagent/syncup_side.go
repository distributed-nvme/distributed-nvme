package dnagent

import (
	"context"
	"fmt"
	"log/slog"
	"math/bits"
	"strconv"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// syncupSide implements DN8-DN14. The node read lock and this side's object
// lock are held by the caller.
func (s *DnAgentServer) syncupSide(
	ctx context.Context,
	key string,
	req *pb.SyncupSideRequest,
) *pb.SyncupSideReply {
	// DN8 gating: SyncupDn introduces the pointer first (architecture.md,
	// `service DiskNodeAgent`).
	dn := s.getDn(dnKey(req.GetClusterId(), req.GetDnId()))
	if dn == nil || !pointerKnown(dn.req.Load(), req.GetSidePointer()) {
		return &pb.SyncupSideReply{
			AgentReply: agent.UnknownObjectReply(
				"side pointer %s not in the dn's list",
				sidePointerText(req.GetSidePointer())),
		}
	}
	st := s.getSide(key)
	var stored uint64
	if st != nil {
		stored = st.req.Load().GetRevision()
	}
	if reject := agent.GateRevision(stored, req.GetRevision()); reject != nil {
		return &pb.SyncupSideReply{AgentReply: reject, Revision: stored}
	}
	// DN8's side conf gate: a length to zero the sp worker cannot have derived
	// from a stored group. The DN request cannot change under this RPC —
	// SyncupDn replaces it under the node write lock, and this holds the read
	// lock — so the gate and the converge below see one extent size. This is
	// the last point with literally zero side effects: refusing here skips
	// the desired-state promotion below, the whole converge and the
	// local-store Save, so a bad request cannot be replayed by the next
	// Reconcile either. The stored revision is echoed back, not the
	// request's, so the worker sees the request was not accepted.
	extentSize := dn.req.Load().GetExtentSize()
	if err := validateZeroBytes(req.GetSideConf(), extentSize); err != nil {
		logInvalidSideConf(ctx, req, err)
		return &pb.SyncupSideReply{
			AgentReply: agent.InvalidConfReply("%v", err),
			Revision:   stored,
		}
	}
	// Stored before the converge builds anything, so no claim of this
	// request can be missing from another side's pass that sees what the
	// converge builds (collectClaims). The store is atomic because those
	// passes do not hold this side's object lock.
	if st == nil {
		st = newSideState(req)
	}
	st.req.Store(req)
	s.putSide(key, st)

	info, sweep := s.convergeSide(ctx, st, extentSize)

	path := s.nf.LocalSidePath(req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer().GetSpId(), req.GetSidePointer().GetSideId())
	if err := s.store.Save(ctx, path, req); err != nil {
		slog.ErrorContext(ctx, "persisting side state failed",
			slog.String("path", path),
			slog.String("error", err.Error()))
	}
	return &pb.SyncupSideReply{
		AgentReply: sweep.Reply(),
		Revision:   req.GetRevision(),
		SideInfo:   info,
		BmInfo:     s.bitmapInfo(st),
	}
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
// agent_reply carries.
func (s *DnAgentServer) convergeSide(
	ctx context.Context,
	st *sideState,
	extentSize uint64,
) (*pb.SideInfo, *agent.SweepResult) {
	// The same refusal as syncupSide's (DN8), for the entrances that do not
	// come through it: the startup Reconcile, which converges from a stored
	// file, and reconvergeSide — the DN13 retry and the DN12 fence timer —
	// which re-enter with the request the side already holds. Refusing
	// before the sweep touches nothing at all: a conf fault must not destroy
	// resources (architecture.md, Common agent rules), and nothing was
	// converged or enumerated, so the pass has no verdict to give either.
	req := st.req.Load()
	if err := validateZeroBytes(req.GetSideConf(), extentSize); err != nil {
		logInvalidSideConf(ctx, req, err)
		return &pb.SideInfo{}, &agent.SweepResult{}
	}
	plan := newSidePlan(s.nf, req, extentSize)
	info := &pb.SideInfo{}

	// What to remove is derived by subtracting the desired state from what
	// the node actually holds, never from memory of a past converge.
	sweep := s.sweepSide(ctx, st, plan, true)

	state := s.ensureSideDev(ctx, st, plan, info)
	if !plan.wantDm {
		// SP_LEVEL_DISABLE: only the side device, its allocation record and —
		// because zeroing is bottom-layer provisioning, below the level
		// ladder — its zeroing goroutine remain (DN11; architecture.md,
		// Side provisioning protocol). The sweep above has already removed
		// everything else, dm-clone included: at
		// this level nothing is wanted but the side device, so the per-CN
		// linears went first and stopped holding the clone open.
		return info, sweep
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
		s.reportAboveSideDeferred(st, plan, info)
		return info, sweep
	}

	// architecture.md, Migration, src step 1: hand IO over before the
	// dm-linears are reloaded onto their dm-errors.
	if plan.migrSrc != nil && plan.wantExport {
		s.moveCnAnaGroups(ctx, plan, common.AnaGrpIdInaccessible)
	}

	cloneLive, dstHeld := false, false
	if plan.wantMigr {
		var clone migrCloneState
		clone, dstHeld = s.ensureMigrDst(ctx, st, plan, info)
		switch clone {
		case migrCloneUp:
			cloneLive = true
		case migrCloneUnread:
			// Nothing decides the per-CN stacks this pass, so they stay
			// where they are and are only reported, as a check round would
			// report them; the retry the destination registered converges
			// them. The fence is settled as under the DN9 gate above, for
			// the same reason: [D12]'s bound does not wait for a probe.
			s.settleFence(ctx, st, plan)
			dstInfo := info.MigrDstInfo
			s.probeAboveSideDev(ctx, st, plan, info)
			info.MigrDstInfo = dstInfo
			return info, sweep
		}
	}
	s.ensureCnDm(ctx, st, plan, info, cloneLive)
	switch {
	case plan.migrSrc != nil:
		s.ensureMigrSrc(ctx, st, plan, info)
	case plan.migrSrcDeferred:
		// Serving is untouched; only the reporting differs (architecture.md,
		// Migration).
		s.reportMigrSrcDeferred(st, plan, info)
	}
	if plan.wantExport {
		s.ensureCnExports(ctx, st, plan, info, cloneLive)
	}
	if dstHeld {
		// DN13 step (5) has run: whether the destination still needs its
		// retry is decided last, so no OS call of this pass follows its
		// deregistration.
		s.settleMigrRetry(st, plan, info)
	}
	return info, sweep
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
	// record is missing or whose zeroed count is short of its length to zero.
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
// side's group: the whole leg for a side of a meta group, which holds the thin
// pool's metadata, and the meta region plus the first data block for a side
// of a data group. Those are the parts of a side that must read as zeros
// (architecture.md, Side provisioning protocol), and discard-reads-zeros is
// not a hardware guarantee, so `blkdiscard --zeroout` is what actually puts
// the zeros there; the rest of a data side is never zeroed, because dm-thin
// never lets a host read a block it has not written ([D15]). The counts live
// in the record because zeroed is a property of the side's *allocation*, not
// of the disk extent.
//
// The six rows of the converge matrix of dnagent.md DN9 (request provisioned
// × local state):
//
//	false / absent   allocate (count 0), build the linear, start the goroutine
//	false / partial  ensure the linear, keep the goroutine
//	false / complete linear ensured, goroutine stopped, still no exports
//	true  / complete the full DN10 export converge
//	true  / partial  refuse the exports, keep the goroutine (it self-heals)
//	true  / absent   never allocate; the data is gone
//
// Allocation is permitted **only** at provisioned = false. At true a missing
// record means the data is gone (a lost or foreign disk); silently
// re-allocating would present a fresh impostor as the data-bearing leg, so it
// is a hard resource error that feeds err_epoch and the replacement flows.
//
// The table, not the local store, is authoritative for placement ([D13]): the
// lookup-or-allocate below is what makes a node that lost --local-store but
// kept its disk rebuild exactly the same device.
func (s *DnAgentServer) ensureSideDev(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
) sideDevState {
	t := st.tracker
	name := plan.sideDevName
	// zero_bytes is never omitted (DN9). Until a record exists the only
	// length available is the request's, which is why it is seeded here and
	// overwritten from the record below — the record always wins.
	info.ZeroBytes = plan.conf.GetZeroBytes()

	rec, ok, err := s.meta.LookupSide(ctx, plan.spId, plan.sideId)
	if err != nil {
		// An unreadable or corrupt disk is an error, never "absent".
		info.SideDevInfo = t.Err(resKeySideDev, name, err.Error())
		return sideDevFailed
	}
	if !ok && plan.provisioned {
		// Row 6. The detail string is byte-exact on purpose: runbooks and the
		// integ suites grep it.
		info.SideDevInfo = t.Err(resKeySideDev, name, tagRecordMissing)
		return sideDevFailed
	}
	if ok {
		// The record exists, so it — not the request — is what the counters
		// report from here on, including on the failure paths below: AllocSide
		// can still refuse (a DN9 mismatch of the extent count or of the
		// length to zero, or a disk this agent may not mutate) and those
		// replies must carry the disk's numbers, not a request value the agent
		// can prove wrong (DN9). They are overwritten with the identical
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
	rec, err = s.meta.AllocSide(ctx, plan.spId, plan.sideId,
		plan.conf.GetExtCnt(), plan.conf.GetZeroBytes())
	if err != nil {
		info.SideDevInfo = t.Err(resKeySideDev, name, err.Error())
		return sideDevFailed
	}
	zeroed, zero := sideZeroedBytes(rec), sideZeroBytes(rec)
	info.ZeroedBytes, info.ZeroBytes = zeroed, zero

	if err := s.ensureSideDm(ctx, plan, rec); err != nil {
		info.SideDevInfo = t.Err(resKeySideDev, name, err.Error())
		return sideDevFailed
	}
	if !sideFullyZeroed(rec) {
		// Rows 2 and 5. The goroutine runs on both sides of the gate: at
		// provisioned = true it is what self-heals a side whose count was lost
		// or never finished.
		s.startZeroing(st, plan)
		if plan.provisioned {
			info.SideDevInfo = t.Err(resKeySideDev, name, tagNotZeroed)
			return sideDevFailed
		}
		if zeroErr := s.zeroingErr(st); zeroErr != nil {
			// A batch that failed or was killed reports its output, and ERROR
			// wins while that failure is outstanding; the next successful
			// batch puts the row back to PROVISIONING (DN9).
			info.SideDevInfo = t.Err(resKeySideDev, name, zeroErr.Error())
			return sideDevProvisioning
		}
		info.SideDevInfo = t.Provisioning(resKeySideDev, name,
			zeroingDetails(zeroed, zero))
		return sideDevProvisioning
	}

	// Rows 3 and 4: the length to zero is zeroed. Cancel-and-wait rather
	// than a bare cancel — a straggler batch's child would otherwise still
	// hold the side device open (DN9).
	s.stopZeroing(st)
	status, details := s.probeSideDm(ctx, plan, rec)
	info.SideDevInfo = t.Set(resKeySideDev, name, status, details)
	switch {
	case status != pb.ResStatus_RES_STATUS_OK:
		return sideDevFailed
	case !plan.provisioned:
		// Row 3: the side is ready, but the CP has not released it yet.
		return sideDevProvisioning
	default:
		return sideDevReady
	}
}

// ensureSideDm converges the aggregate dm-linear against the record's extent
// runs, probe-first. It is ensureDmLinear's multi-segment sibling: dmsetup's
// --table takes one line only, so the table travels through stdin.
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
		return s.dm.ReloadMulti(ctx, name, table)
	}
	if dev.Suspended {
		return s.dm.Resume(ctx, name)
	}
	return nil
}

// sideRunSectors converts a record's extent runs into dm table geometry: run
// r starts at byte DnDataOffset + r.start*extent_size and is r.count extents
// long. DnDataOffset is 1 MiB-aligned, so every legal extent size lands the
// runs on whole sectors.
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
	targets []agent.DmTarget,
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

func (s *DnAgentServer) ensureCnDm(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
	cloneLive bool,
) {
	// The src cutover (architecture.md, Migration, src step 2) fences the
	// per-CN linears in two phases: hold
	// them suspended for at least common.SuspendSeconds, then reload them
	// onto their dm-errors ([D12]). fencing is true only during phase 1.
	fencing := plan.migrSrc != nil && s.beginFence(st)

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
		if err != nil {
			info.CnIdToDmLinear[cnId] = st.tracker.Err(
				linKey, linName, "dm-error missing: "+err.Error())
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
		backing := plan.linearBacking(cnId, cloneLive)
		err = s.ensureDmLinear(ctx, linName, plan.sectors, backing)
		info.CnIdToDmLinear[cnId] = st.tracker.FromErr(
			linKey, linName, "", err)
	}
	if fencing {
		s.armFenceTimer(st, plan)
	}
}

// unfenceLinears resumes every per-CN dm-linear this side left suspended.
// The queued IO drains against whatever table is live — for a fenced linear
// its pre-fence one — which is the same thing the end of the window would
// have done, only without the dm-error swap the side no longer needs. A
// linear that a primary flip's failed reload left suspended on its old
// table still needs that swap, and resuming it here releases the old
// primary's queued IO onto the side's data ahead of any retry of the
// reload: a known limit (a reload fails closed; dnagent.md, Known limits).
//
// The set of devices comes from the ENUMERATION, not from a remembered cn
// list: a linear built for a CN that has since left standby_id_list is
// exactly the one a remembered list would miss, and leaving it suspended
// would queue bios with no timeout ([D12]).
func (s *DnAgentServer) unfenceLinears(
	ctx context.Context,
	plan *sidePlan,
	actual *dnActual,
) {
	ofSide := func(dn common.DmName) bool {
		return dn.Ids[0] == plan.spId && dn.Ids[1] == plan.sideId
	}
	for _, name := range actual.dmsOfKind(common.DmKindDnLinear, ofSide) {
		dev, err := s.dm.Info(ctx, name)
		if err != nil || dev == nil || !dev.Suspended {
			continue
		}
		if err := s.dm.Resume(ctx, name); err != nil {
			slog.ErrorContext(ctx, "resuming a fenced dm-linear failed",
				slog.String("name", name),
				slog.String("error", err.Error()))
		}
	}
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

func (s *DnAgentServer) ensureDmError(
	ctx context.Context,
	name string,
	sectors uint64,
) error {
	if sectors == 0 {
		return fmt.Errorf("side size is 0 (extent_size or ext_cnt unset)")
	}
	table := agent.ErrorTable(sectors)
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
	if len(targets) != 1 || targets[0].Type != "error" ||
		targets[0].Length != sectors {
		return s.dm.Reload(ctx, name, table)
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
// read-only ([D11]).
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
		return s.dm.Reload(ctx, name, table)
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

// linearMaps reports whether a live table is the one ensureDmLinear builds
// over devNo: a single "linear" target of the side's whole size, from
// sector 0 of the device.
func linearMaps(targets []agent.DmTarget, sectors uint64, devNo string) bool {
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
) {
	uuid, nguid := plan.nsIdentity()
	cntlidMin, cntlidMax := plan.cntlidRange()
	info.CnIdToNvmeof = make(map[uint64]*pb.ResInfo, len(plan.cnIds))
	for _, cnId := range plan.cnIds {
		nqn := plan.sideNqn(cnId)
		key := resKeyOf(resKeyNvmeofFmt, cnId)
		err := s.ensureExport(ctx,
			agent.SubsysConf{
				Nqn:          nqn,
				Serial:       fmt.Sprintf(common.IdKeyFmt, plan.legId),
				Model:        nsModel,
				CntlidMin:    cntlidMin,
				CntlidMax:    cntlidMax,
				AllowedHosts: []string{plan.cnHostNqn(cnId)},
			},
			agent.NsConf{
				Nqn:        nqn,
				Nsid:       sideNsid,
				DevicePath: s.nf.DmPath(plan.linearName(cnId)),
				Uuid:       uuid,
				Nguid:      nguid,
				AnaGrpId:   plan.anaGrpId(cnId, cloneLive),
			})
		info.CnIdToNvmeof[cnId] = st.tracker.FromErr(key, nqn, "", err)
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
	if err := s.nvmet.EnsureSubsystem(ctx, subsys); err != nil {
		return err
	}
	if err := s.nvmet.EnsureNamespace(ctx, ns); err != nil {
		return err
	}
	return s.nvmet.EnsurePortLink(ctx, s.port.PortId, subsys.Nqn)
}

// moveCnAnaGroups rewrites the ana_grpid of every existing per-CN namespace
// — the whole of an ANA transition ([D4]).
func (s *DnAgentServer) moveCnAnaGroups(
	ctx context.Context,
	plan *sidePlan,
	grpId int,
) {
	for _, cnId := range plan.cnIds {
		nqn := plan.sideNqn(cnId)
		state, err := s.nvmet.ProbeNamespace(ctx, nqn, sideNsid)
		if err != nil || !state.Exists || state.AnaGrpId == grpId {
			continue
		}
		if err := s.nvmet.SetNsAnaGrpId(
			ctx, nqn, sideNsid, grpId); err != nil {
			slog.ErrorContext(ctx, "ana transition failed",
				slog.String("nqn", nqn),
				slog.Int("ana_grpid", grpId),
				slog.String("error", err.Error()))
		}
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
func (s *DnAgentServer) reportAboveSideDeferred(
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
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
		// (architecture.md, Migration): behind the closed gate no metadata
		// slot, source connection or dm-clone is built for it. A gate that
		// closes again removes none of them that a pass built while it was
		// open (the sweep's wanted set ignores the gate, DN13), and these two
		// rows do not probe them, so PROVISIONING here does not mean that no
		// dm-clone exists.
		dstInfo := &pb.SideInfo_MigrDstInfo{}
		info.MigrDstInfo = dstInfo
		nqn := plan.srcNqnOfDst()
		dstInfo.TargetInfo = t.Provisioning(
			resKeyMigrDstTarget, nqn, tagProvisioningWait)
		dstInfo.DmCloneInfo = t.Provisioning(
			resKeyMigrDstClone, plan.migrFinalName(), tagProvisioningWait)
	}
}

// reportMigrSrcDeferred fills the migration-source rows a side would publish
// once its destination has provisioned.
//
// `migr_src_conf.dst_provisioned = false` makes the source behave **exactly**
// as if migr_src_conf were absent (architecture.md, Migration): it keeps
// serving, it does not fence, and it exports nothing to the destination.
// Without that equivalence
// the source would fence the primary's path the moment the migration was
// created, and the leg would have no serving path for the whole zeroing
// window. The only visible difference is right here — the would-be rows report
// PROVISIONING instead of being omitted.
func (s *DnAgentServer) reportMigrSrcDeferred(
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
) {
	if plan.migrSrcRaw == nil {
		return
	}
	t := st.tracker
	// migrSrc is nil while the role is deferred, so the names come from a plan
	// with the raw conf re-attached.
	srcPlan := plan.withMigrSrc(plan.migrSrcRaw)
	srcInfo := &pb.SideInfo_MigrSrcInfo{}
	info.MigrSrcInfo = srcInfo
	srcInfo.DmLinearInfo = t.Provisioning(
		resKeyMigrSrcDm, srcPlan.migrSrcName(), tagProvisioningWait)
	if plan.wantExport {
		srcInfo.NvmeofInfo = t.Provisioning(
			resKeyMigrSrcNvmeof, srcPlan.migrSrcNqn(), tagProvisioningWait)
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
