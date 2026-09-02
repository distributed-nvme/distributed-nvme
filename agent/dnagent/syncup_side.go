package dnagent

import (
	"context"
	"fmt"
	"log/slog"
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
	// DN8 gating: SyncupDn introduces the pointer first (§9.2).
	dn := s.getDn(dnKey(req.GetClusterId(), req.GetDnId()))
	if dn == nil || !pointerKnown(dn.req, req.GetSidePointer()) {
		return &pb.SyncupSideReply{
			AgentReply: agent.UnknownObjectReply(
				"side pointer %s not in the dn's list",
				sidePointerText(req.GetSidePointer())),
		}
	}
	st := s.getSide(key)
	var stored uint64
	if st != nil {
		stored = st.req.GetRevision()
	}
	if reject := agent.GateRevision(stored, req.GetRevision()); reject != nil {
		return &pb.SyncupSideReply{AgentReply: reject, Revision: stored}
	}
	if st == nil {
		st = newSideState(req)
	}
	st.req = req
	s.putSide(key, st)

	info := s.convergeSide(ctx, st, dn.req.GetExtentSize())

	path := s.nf.LocalSidePath(req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer().GetSpId(), req.GetSidePointer().GetSideId())
	if err := s.store.Save(ctx, path, req); err != nil {
		slog.ErrorContext(ctx, "persisting side state failed",
			slog.String("path", path),
			slog.String("error", err.Error()))
	}
	return &pb.SyncupSideReply{
		AgentReply: agent.OkReply(),
		Revision:   req.GetRevision(),
		SideInfo:   info,
		BmInfo:     s.bitmapInfo(st),
	}
}

// convergeSide brings one side to its desired state: tear the layers the
// sp_level (or a finished migration) forbids down top-down, then build what
// is wanted bottom-up — side device, dm, nvmet — probing first at every step
// (SH16).
func (s *DnAgentServer) convergeSide(
	ctx context.Context,
	st *sideState,
	extentSize uint64,
) *pb.SideInfo {
	plan := newSidePlan(s.nf, st.req, extentSize)
	info := &pb.SideInfo{}

	s.teardownForbidden(ctx, st, plan)
	st.appliedCnIds = plan.cnIds
	st.appliedMigrSrc = plan.migrSrc
	st.appliedMigrDst = plan.migrDst

	sideDevReady := s.ensureSideDev(ctx, st, plan, info)
	if !plan.wantDm {
		// SP_LEVEL_DISABLE: only the side device and its allocation record
		// remain.
		return info
	}
	if !sideDevReady {
		// A side device that is missing or not yet discarded is never
		// exported (§9.4); report the rest as it currently stands.
		s.probeAboveSideDev(ctx, st, plan, info)
		return info
	}

	// §11.2 src step 1: hand IO over before the dm-linears are reloaded onto
	// their dm-errors.
	if plan.migrSrc != nil && plan.wantExport {
		s.moveCnAnaGroups(ctx, plan, common.AnaGrpIdInaccessible)
	}

	cloneLive := false
	if plan.wantMigr {
		cloneLive = s.ensureMigrDst(ctx, st, plan, info)
	}
	s.ensureCnDm(ctx, st, plan, info, cloneLive)
	if plan.migrSrc != nil {
		s.ensureMigrSrc(ctx, st, plan, info)
	}
	if plan.wantExport {
		s.ensureCnExports(ctx, st, plan, info, cloneLive)
	}
	return info
}

// ---------------------------------------------------------------------------
// Side device — allocation + the §9.4 trim protocol (DN9)
// ---------------------------------------------------------------------------

// ensureSideDev allocates the side's extents in the on-disk volume table,
// builds the aggregate dm-linear that concatenates them, and runs the trim
// protocol; it reports whether the side is ready to be exported.
//
// The table, not the local store, is authoritative for placement ([D13]): the
// lookup-or-allocate below is what makes a node that lost --local-store but
// kept its disk rebuild exactly the same device.
func (s *DnAgentServer) ensureSideDev(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
) bool {
	t := st.tracker
	name := plan.sideDevName
	rec, err := s.meta.AllocSide(
		ctx, plan.spId, plan.sideId, plan.conf.GetExtCnt())
	_ = err
	if err := s.ensureSideDm(ctx, plan, rec); err != nil {
		info.SideDevInfo = t.Err(resKeySideDev, name, err.Error())
		return false
	}
	if !rec.GetTrimmed() {
		// Steps 2-3 of §9.4 are redone on every restart until they stick:
		// the record is created untrimmed, the discard runs against the
		// assembled device, and only then does the flag flip.
		if err := s.dm.BlkDiscard(ctx, plan.sideDevPath); err != nil {
			info.SideDevInfo = t.Err(resKeySideDev, name, err.Error())
			return false
		}
		if err := s.meta.SetSideTrimmed(
			ctx, plan.spId, plan.sideId); err != nil {
			info.SideDevInfo = t.Err(resKeySideDev, name, err.Error())
			return false
		}
	}
	info.SideDevInfo = t.Ok(resKeySideDev, name, "")
	return true
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
	// The §11.2 src cutover fences the per-CN linears in two phases: hold
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
			// §11.2 src step 2, phase 1: hold the device suspended where it
			// is. Its namespace is already AnaGrpIdInaccessible, so this
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
// have done, only without the dm-error swap the side no longer needs.
func (s *DnAgentServer) unfenceLinears(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
) {
	for _, cnId := range unionIds(st.appliedCnIds, plan.cnIds) {
		name := plan.linearName(cnId)
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
// the §11.2 grace window — an expected, time-bounded state, not a fault.
const fenceSuspendedDetails = "suspended (migration cutover grace window)"

// ensureDmLinearSuspended is phase 1 of the fence: the device must exist,
// carry its pre-fence table, and be suspended. It never swaps the table —
// the swap onto dm-error is phase 2, and doing it here would defeat the
// window by erroring the very IO the window exists to absorb.
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
	// No dnv device is ever left suspended ([D12]).
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
	converged := len(targets) == 1 && targets[0].Type == "linear" &&
		targets[0].Length == sectors && len(targets[0].Args) == 2 &&
		targets[0].Args[0] == devNo && targets[0].Args[1] == "0"
	if !converged || dev.ReadOnly {
		return s.dm.Reload(ctx, name, table)
	}
	// A device an older (pre-[D12]) build left suspended, or one a crash
	// caught mid-reload, must converge back to resumed.
	if dev.Suspended {
		return s.dm.Resume(ctx, name)
	}
	return nil
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
	return s.nvmet.EnsurePortLink(ctx, common.NvmetPortId, subsys.Nqn)
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
// Teardown
// ---------------------------------------------------------------------------

// teardownForbidden removes, top-down, everything the current desired state
// no longer wants: layers the sp_level forbids, the endpoints of a finished
// migration, and the stacks of CNs that left the list.
func (s *DnAgentServer) teardownForbidden(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
) {
	for _, cnId := range removedIds(st.appliedCnIds, plan.cnIds) {
		s.removeExport(ctx, plan.sideNqn(cnId))
		s.removeDm(ctx, plan.linearName(cnId))
		s.removeDm(ctx, plan.errName(cnId))
		st.tracker.Drop(resKeyOf(resKeyNvmeofFmt, cnId))
		st.tracker.Drop(resKeyOf(resKeyDmLinearFmt, cnId))
		st.tracker.Drop(resKeyOf(resKeyDmErrorFmt, cnId))
	}
	if !plan.wantExport {
		for _, cnId := range plan.cnIds {
			s.removeExport(ctx, plan.sideNqn(cnId))
			st.tracker.Drop(resKeyOf(resKeyNvmeofFmt, cnId))
		}
	}
	// A migration role that ended is named by the *previously* applied
	// conf: the incoming request no longer carries it.
	if src := st.appliedMigrSrc; src != nil {
		if plan.migrSrc == nil {
			// The cutover was cancelled or finished: the window is over and
			// the linears go back to their normal targets, resumed.
			s.clearFence(st)
		}
		srcPlan := plan.withMigrSrc(src)
		if plan.migrSrc == nil || !plan.wantExport {
			s.removeExport(ctx, srcPlan.migrSrcNqn())
			st.tracker.Drop(resKeyMigrSrcNvmeof)
		}
		if plan.migrSrc == nil || !plan.wantDm {
			s.removeDm(ctx, srcPlan.migrSrcName())
			st.tracker.Drop(resKeyMigrSrcDm)
		}
	}
	if dst := st.appliedMigrDst; dst != nil && !plan.wantMigr {
		s.teardownMigrDst(ctx, st, plan.withMigrDst(dst))
	}
	if !plan.wantDm {
		for _, cnId := range plan.cnIds {
			s.removeDm(ctx, plan.linearName(cnId))
			s.removeDm(ctx, plan.errName(cnId))
			st.tracker.Drop(resKeyOf(resKeyDmLinearFmt, cnId))
			st.tracker.Drop(resKeyOf(resKeyDmErrorFmt, cnId))
		}
	}
}

// withMigrSrc / withMigrDst give a plan whose migration name helpers resolve,
// so a role that is being torn down can still be named.
func (p *sidePlan) withMigrSrc(
	conf *pb.SyncupSideRequest_MigrSrcConf,
) *sidePlan {
	clone := *p
	clone.migrSrc = conf
	return &clone
}

func (p *sidePlan) withMigrDst(
	conf *pb.SyncupSideRequest_MigrDstConf,
) *sidePlan {
	clone := *p
	clone.migrDst = conf
	return &clone
}

func removedIds(applied, want []uint64) []uint64 {
	keep := make(map[uint64]struct{}, len(want))
	for _, id := range want {
		keep[id] = struct{}{}
	}
	var out []uint64
	for _, id := range applied {
		if _, ok := keep[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// teardownSide removes a side completely, top-down (DN6): nvmet exports, the
// dm-clone, the migration-destination connection and its retry loop, the
// remaining dm devices, the dm-clone metadata wrapper and its slot, the side
// device and its allocation record — then the local files and the object lock
// (SH7).
func (s *DnAgentServer) teardownSide(
	ctx context.Context,
	key string,
	st *sideState,
) {
	plan := newSidePlan(s.nf, st.req, 0)
	if plan.migrSrc == nil && st.appliedMigrSrc != nil {
		plan = plan.withMigrSrc(st.appliedMigrSrc)
	}
	if plan.migrDst == nil && st.appliedMigrDst != nil {
		plan = plan.withMigrDst(st.appliedMigrDst)
	}

	// Unfence before anything else. A side torn down inside the §11.2 grace
	// window still has its per-CN dm-linears suspended, and the whole
	// teardown runs over them: disabling an nvmet namespace closes its
	// backing device, and `dmsetup remove` does not succeed on a suspended
	// one. Resuming first means every step below operates on live devices.
	s.clearFence(st)
	s.unfenceLinears(ctx, st, plan)

	for _, cnId := range unionIds(st.appliedCnIds, plan.cnIds) {
		s.removeExport(ctx, plan.sideNqn(cnId))
	}
	if plan.migrSrc != nil {
		s.removeExport(ctx, plan.migrSrcNqn())
	}

	s.stopMigrRetry(st)
	// Strictly top-down. The per-CN dm-linears go first because everything
	// below is one of their table targets — `dmsetup remove` on a device
	// another dm device still maps fails with EBUSY. The dm-clone then goes
	// before the disconnect that removes its source device: pulling the
	// source out from under a live dm-clone leaves in-flight hydration IO
	// with nowhere to go, and the remove blocks until the §7 hard timeout
	// (DN6).
	for _, cnId := range unionIds(st.appliedCnIds, plan.cnIds) {
		s.removeDm(ctx, plan.linearName(cnId))
	}
	if plan.migrDst != nil {
		s.removeDm(ctx, plan.migrFinalName())
		s.disconnect(ctx, plan.srcNqnOfDst())
	}
	if plan.migrSrc != nil {
		s.removeDm(ctx, plan.migrSrcName())
	}
	for _, cnId := range unionIds(st.appliedCnIds, plan.cnIds) {
		s.removeDm(ctx, plan.errName(cnId))
	}
	// A record is released only once its device is really gone: freeing it
	// while the device still maps those bytes would let the next allocation
	// hand them to another side. A record left behind is not a leak — the
	// DN6 orphan sweep retries it on the next node-level pass.
	if plan.migrDst != nil {
		if s.removeDm(ctx, plan.migrMetaDmName()) {
			if err := s.meta.FreeCloneMeta(ctx, plan.spId,
				plan.migrDst.GetMigrId()); err != nil {
				slog.ErrorContext(ctx,
					"freeing the clone-metadata slot failed",
					slog.String("error", err.Error()))
			}
		}
	}
	if s.removeDm(ctx, plan.sideDevName) {
		if err := s.meta.FreeSide(ctx, plan.spId, plan.sideId); err != nil {
			slog.ErrorContext(ctx, "freeing the side allocation failed",
				slog.String("error", err.Error()))
		}
	}

	paths := []string{s.nf.LocalSidePath(
		plan.clusterId, plan.dnId, plan.spId, plan.sideId)}
	paths = append(paths, s.chunkPaths(st)...)
	if err := s.store.Remove(ctx, paths...); err != nil {
		slog.ErrorContext(ctx, "removing side state files failed",
			slog.String("error", err.Error()))
	}
	s.dropSide(key)
	s.locks.DropObj(key)
}

func unionIds(a, b []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(a)+len(b))
	var out []uint64
	for _, list := range [][]uint64{a, b} {
		for _, id := range list {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

func (s *DnAgentServer) removeExport(ctx context.Context, nqn string) {
	if err := s.nvmet.RemoveSubsystem(
		ctx, common.NvmetPortId, nqn); err != nil {
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
	if !state.Found {
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
	// `dmsetup remove` does not succeed on a suspended device, and a side
	// torn down inside the §11.2 grace window still has its per-CN linears
	// suspended. Resume first; the queued IO drains against whatever table
	// is live, which for a fenced linear is its pre-fence one.
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
