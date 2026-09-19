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

	info, sweep := s.convergeSide(ctx, st, dn.req.GetExtentSize())

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
	plan := newSidePlan(s.nf, st.req, extentSize)
	info := &pb.SideInfo{}

	// The sweep replaced teardownForbidden and retireMigrDst, and with them
	// the applied* fields they diffed against. Those fields were memory of a
	// past converge that the converge itself then overwrote, so a removal
	// that failed was forgotten; what to remove is now derived by subtracting
	// this plan from what the node actually holds.
	sweep := s.sweepSide(ctx, st, plan, true)

	state := s.ensureSideDev(ctx, st, plan, info)
	if !plan.wantDm {
		// SP_LEVEL_DISABLE: only the side device, its allocation record and —
		// because zeroing is bottom-layer provisioning, like the trim it
		// replaced — its zeroing goroutine remain (DN11, §9.4). The sweep
		// above has already removed everything else, dm-clone included: at
		// this level nothing is wanted but the side device, so the per-CN
		// linears went first and stopped holding the clone open.
		return info, sweep
	}
	if state != sideDevReady {
		// The whole per-CN stack is gated together (DN9 step 4): dm-error,
		// dm-linear, nvmet, migr-src and migr-dst. A side that is still
		// provisioning has nothing above it by design, and a side whose bits
		// are incomplete must not export a zeroed impostor of the data.
		//
		// The §11.2 fence is the one thing the gate may not skip: a window
		// opened by an earlier pass is a suspension already in place, and
		// [D12] bounds it at the window plus one converge whatever the side
		// device is doing.
		s.settleFence(ctx, st, plan)
		s.reportAboveSideDeferred(st, plan, info)
		return info, sweep
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
	switch {
	case plan.migrSrc != nil:
		s.ensureMigrSrc(ctx, st, plan, info)
	case plan.migrSrcDeferred:
		// Serving is untouched; only the reporting differs (§11.2).
		s.reportMigrSrcDeferred(st, plan, info)
	}
	if plan.wantExport {
		s.ensureCnExports(ctx, st, plan, info, cloneLive)
	}
	return info, sweep
}

// ---------------------------------------------------------------------------
// Side device — allocation + the §9.4 provisioning protocol (DN9)
// ---------------------------------------------------------------------------

// sideDevState is the outcome of one side-device converge — the three things
// the §9.4 matrix has to distinguish, which a bool cannot.
type sideDevState int

const (
	// sideDevFailed is a real fault: an unreadable disk, a table that will not
	// converge, or a request claiming provisioned = true over a side whose
	// data is missing or not fully zeroed. ERROR is reported and feeds
	// err_epoch.
	sideDevFailed sideDevState = iota
	// sideDevProvisioning is healthy but not exportable yet: the side is being
	// zeroed, or it is zeroed and the CP has not flipped its flag. No
	// err_epoch (§9.5).
	sideDevProvisioning
	// sideDevReady is zeroed *and* released by the CP: the per-CN stacks may
	// converge.
	sideDevReady
)

// ensureSideDev implements the §9.4 side provisioning protocol (DN9): allocate
// the side's extents in the on-disk volume table, build the aggregate
// dm-linear that concatenates them, and keep the background zeroing goroutine
// running until every logical extent is zeroed.
//
// Zeroing is whole-side and mandatory: dnv is multi-tenant and
// discard-reads-zeros is not a hardware guarantee, so `blkdiscard --zeroout`
// is what actually funds "a fresh side reads as zeros" ([D15]). The bits live
// in the record because zeroed is a property of the side's *allocation*, not
// of the disk extent.
//
// The six rows of the §9.4 converge matrix (request provisioned × local state):
//
//	false / absent   allocate (bits 0), build the linear, start the goroutine
//	false / partial  ensure the linear, keep the goroutine
//	false / complete linear ensured, goroutine stopped, still no exports
//	true  / complete the full DN10 export converge
//	true  / partial  refuse the exports, keep the goroutine (it self-heals)
//	true  / absent   never allocate; the data is gone
//
// Allocation is permitted **only** at provisioned = false. At true a missing
// record means the data is gone (a lost or foreign disk); silently
// re-allocating would present a zeroed impostor as the data-bearing leg, so it
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
	// total_ext_cnt is never omitted (DN9). Until a record exists the
	// only number available is the request's, which is why it is seeded here
	// and overwritten from the record below — the record always wins.
	info.TotalExtCnt = plan.conf.GetExtCnt()

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
		// can still refuse (a DN9 ext-count mismatch, or a disk this agent may
		// not mutate) and those replies must carry the disk's numbers, not a
		// request value the agent can prove wrong (DN9). They are overwritten
		// with the identical values once AllocSide hands the record back.
		info.ZeroedExtCnt, info.TotalExtCnt = sideZeroedCnt(rec), sideExtCnt(rec)
	}
	// Rows 1-5 all go through AllocSide: it returns the existing record —
	// re-checking DN9's ext-count invariant, which a resize would violate —
	// and allocates only when there is none, a case row 6 has already taken
	// off the table. Its error is reported rather than discarded, because the
	// matrix needs "could not allocate" to be distinguishable from "allocated"
	// and from "must not allocate" (DN9).
	rec, err = s.meta.AllocSide(
		ctx, plan.spId, plan.sideId, plan.conf.GetExtCnt())
	if err != nil {
		info.SideDevInfo = t.Err(resKeySideDev, name, err.Error())
		return sideDevFailed
	}
	zeroed, total := sideZeroedCnt(rec), sideExtCnt(rec)
	info.ZeroedExtCnt, info.TotalExtCnt = zeroed, total

	if err := s.ensureSideDm(ctx, plan, rec); err != nil {
		info.SideDevInfo = t.Err(resKeySideDev, name, err.Error())
		return sideDevFailed
	}
	if zeroed < total {
		// Rows 2 and 5. The goroutine runs on both sides of the gate: at
		// provisioned = true it is what self-heals a side whose bits were lost
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
			fmt.Sprintf(zeroingDetailsFmt, zeroed, total))
		return sideDevProvisioning
	}

	// Rows 3 and 4: fully zeroed. Cancel-and-wait rather than a bare cancel —
	// a straggler batch's child would otherwise still hold the side device
	// open (§9.4).
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
// Provisioning-deferred reporting (§9.4)
// ---------------------------------------------------------------------------

// reportAboveSideDeferred fills every row above the side device with
// RES_STATUS_PROVISIONING. It probes nothing, because there is nothing to look
// at: those resources are deliberately not created while the side underneath
// them is not exportable.
//
// PROVISIONING never feeds err_epoch (§9.5), which is the point: one cause is
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
		// The destination provisions first, under this same protocol: linear
		// and zeroing only, no metadata slot, no connect, no dm-clone
		// (§11.2).
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
// as if migr_src_conf were absent (§11.2): it keeps serving, it does not
// fence, and it exports nothing to the destination. Without that equivalence
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
	// A subsystem the kernel has kept after its last controller went holds
	// nothing open; disconnecting it again would be a command per round.
	if !state.Found || len(state.Paths) == 0 {
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
