package dnagent

import (
	"context"
	"fmt"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The probe map of DN18. No function here creates, changes or removes an
// object of the node, as the background read behind the check rounds must
// not (DN16, SH29); reading a migration destination's dm-clone status still
// makes the kernel commit the clone's metadata (SH17). What a probe can
// change is the agent's own view of the disk (probeDn). A migration
// destination's two rows, and where its per-CN rows are judged to serve from,
// come from the rule its converge applies to what it reads (migrDstSeen,
// DN13).

func (s *DnAgentServer) probeDn(
	ctx context.Context,
	st *dnState,
) *pb.DnInfo {
	req := st.req.Load()
	// A row made after the read is cancelled moves no epoch. The read holds
	// only the node read lock, which the DN's next read may share before
	// this one has returned, so it writes through a view of its own
	// (agent.ResTracker.In).
	t := st.tracker.In(ctx)
	info := &pb.DnInfo{}

	size, err := s.dm.DiskSize(ctx, s.disk)
	info.DiskInfo = t.FromErr(resKeyDisk, s.disk, "", err)
	if err == nil {
		s.meta.SetDiskSize(size)
	}

	// Only the 4 KiB header is re-read: cheap enough for the health rounds
	// at any interval, and enough to catch a wiped, corrupt or foreign disk.
	// What it reads also moves DiskMeta's own view, in memory and nowhere
	// else: a header other than the one the loaded volume table came from
	// drops that table, so the next call re-reads the disk (DN18).
	details, err := s.meta.ProbeHeader(ctx, req.GetClusterId(),
		req.GetDnId(), req.GetExtentSize())
	info.MetaInfo = t.FromErr(resKeyMeta, s.disk, details, err)
	// The counts go after both reads, so they describe the table and the
	// size as this probe leaves them.
	s.addDiskCounts(info, req)

	portName := fmt.Sprintf("%d", s.port.PortId)
	ok, details, err := s.nvmet.ProbePort(ctx, s.port.PortId, s.port)
	switch {
	case err != nil:
		info.PortInfo = t.Err(resKeyPort, portName, err.Error())
	case !ok:
		info.PortInfo = t.Err(resKeyPort, portName, details)
	default:
		info.PortInfo = t.Ok(resKeyPort, portName, "")
	}
	return info
}

// addDiskCounts puts the disk counts (DN18) into info beside an OK meta row,
// and only there: a meta row that is not OK says that this pass's read could
// not vouch for the table in memory, though the table may still be this dn's,
// and DiskMeta.Counts applies the gates of its own. The DN converge
// (convergeDn) and the probe (probeDn) both call it right after they set the
// meta row.
func (s *DnAgentServer) addDiskCounts(
	info *pb.DnInfo,
	req *pb.SyncupDnRequest,
) {
	if info.GetMetaInfo().GetStatus() != pb.ResStatus_RES_STATUS_OK {
		return
	}
	info.DiskCounts = s.meta.Counts(
		req.GetClusterId(), req.GetDnId(), req.GetSidePointerList())
}

func (s *DnAgentServer) probeSide(
	ctx context.Context,
	st *sideState,
) *pb.SideInfo {
	req := st.req.Load()
	var extentSize uint64
	if dn := s.getDn(
		dnKey(req.GetClusterId(), req.GetDnId())); dn != nil {
		extentSize = dn.req.Load().GetExtentSize()
	}
	// The same refusal convergeSide makes (DN8), because this reads the SAME
	// stored request: a side whose length to zero is refused converges
	// nothing, so there is nothing provable to report about it. Read-only
	// either way (DN16, SH29): an empty SideInfo is what such a side looks
	// like.
	if err := validateZeroBytes(req.GetSideConf(), extentSize); err != nil {
		logInvalidSideConf(ctx, req, err)
		return &pb.SideInfo{}
	}
	plan := newSidePlan(s.nf, req, extentSize)
	info := &pb.SideInfo{}
	state, metaWait := s.probeSideDev(ctx, st, plan, info)
	if !plan.wantDm {
		return info
	}
	// The same gate the converge uses (DN9), so a probe can never
	// claim a stack the converge deliberately did not build.
	if state != sideDevReady {
		s.reportAboveSideDeferred(st, plan, info, metaWait)
		return info
	}
	s.probeAboveSideDev(ctx, st, plan, info)
	return info
}

// probeSideDev is the read-only half of the converge matrix of dnagent.md
// DN9 (DN18): it
// checks the side's allocation record, its zeroing progress and the aggregate
// dm-linear built from its extent runs, and reports whether the side is
// exportable.
//
// Unlike ensureSideDev it never allocates and never starts or stops the
// zeroing goroutine (DN16, SH29). That is why "record absent at
// provisioned = false" reports MISSING with empty details rather than the
// matrix's "zeroing 0/n MiB": with no record the only available length is
// the request's, and the etcd flag is a gate, never evidence (DN9). Nor does
// it take a migration destination's run in the clone-metadata area: it reads
// the area's refusal of the run (migrMetaRefusal), and reports a destination
// the area has no free run for as the converge does, PROVISIONING with the
// area's text, returning that refusal as ensureSideDev returns it.
func (s *DnAgentServer) probeSideDev(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
) (sideDevState, error) {
	t := st.tracker
	name := plan.sideDevName
	info.ZeroBytes = plan.conf.GetZeroBytes()
	rec, ok, err := s.meta.LookupSide(ctx, plan.spId, plan.sideId)
	if err != nil {
		// An unreadable or corrupt disk is an error, never "absent".
		info.SideDevInfo = t.Err(resKeySideDev, name, err.Error())
		return sideDevFailed, nil
	}
	if !ok {
		if plan.provisioned {
			info.SideDevInfo = t.Err(resKeySideDev, name, tagRecordMissing)
		} else {
			info.SideDevInfo = t.Missing(resKeySideDev, name, "")
		}
		return sideDevFailed, nil
	}
	// The counters are filled on every read of the side (SH29), whatever the
	// outcome below is: they are what the worker's provisioned-flip rule reads
	// (architecture.md, sp role). They come from the record, never from the
	// request — the disk is authoritative ([D13]).
	zeroed, zero := sideZeroedBytes(rec), sideZeroBytes(rec)
	info.ZeroedBytes, info.ZeroBytes = zeroed, zero
	if !sideFullyZeroed(rec) {
		// The device is judged on every read, provisioning or not: DN18 reads
		// this row off the volume-table record, its zeroed count and
		// `dmsetup table`, and a non-OK device wins. Skipping the check while
		// the count is short would let a side whose dm-linear could not be
		// built report healthy PROVISIONING for ever — and PROVISIONING never
		// feeds err_epoch (architecture.md, Live-state reporting), so nothing
		// would ever bump a revision and re-send the SyncupSide that is the
		// only thing able to rebuild it.
		if status, details := s.probeSideDm(ctx, plan, rec); status !=
			pb.ResStatus_RES_STATUS_OK {
			info.SideDevInfo = t.Set(resKeySideDev, name, status, details)
			return sideDevFailed, nil
		}
		// A migration destination that has not provisioned zeroes only once
		// it holds its run (DN13): the converge's refusal of the run, as far
		// as the loaded table shows it, is this row's verdict.
		if !plan.provisioned && plan.migrDst != nil {
			if refusal := s.migrMetaRefusal(ctx, plan); refusal != nil {
				if isNoFreeRun(refusal) {
					info.SideDevInfo = t.Provisioning(resKeySideDev, name,
						metaRoomDetails(refusal))
					return sideDevProvisioning, refusal
				}
				info.SideDevInfo = t.Err(
					resKeySideDev, name, refusal.Error())
				return sideDevFailed, nil
			}
		}
		// zeroErr is bound ONCE and then used: the zeroing loop clears it from
		// outside both DN1 locks (zeroing.go's success path runs after
		// release(), holding only s.mu), so testing the accessor and
		// re-reading it for the details would call .Error() on a nil error the
		// instant a retried batch succeeds — a panic in an RPC handler that no
		// interceptor recovers. ensureSideDev binds it exactly this way.
		zeroErr := s.zeroingErr(st)
		switch {
		case plan.provisioned:
			// The agent trusts its own count over the flag.
			info.SideDevInfo = t.Err(resKeySideDev, name, tagNotZeroed)
			return sideDevFailed, nil
		case zeroErr != nil:
			info.SideDevInfo = t.Err(resKeySideDev, name, zeroErr.Error())
		default:
			// Healthy, not ready: nothing is exported until the last byte to
			// zero is zeroed, and PROVISIONING says so without feeding
			// err_epoch.
			info.SideDevInfo = t.Provisioning(resKeySideDev, name,
				zeroingDetails(zeroed, zero))
		}
		return sideDevProvisioning, nil
	}
	status, details := s.probeSideDm(ctx, plan, rec)
	info.SideDevInfo = t.Set(resKeySideDev, name, status, details)
	switch {
	case status != pb.ResStatus_RES_STATUS_OK:
		return sideDevFailed, nil
	case !plan.provisioned:
		return sideDevProvisioning, nil
	default:
		return sideDevReady, nil
	}
}

// probeSideDm compares the live aggregate table against the record's runs —
// the same comparison the converge pass makes.
func (s *DnAgentServer) probeSideDm(
	ctx context.Context,
	plan *sidePlan,
	rec *pb.DnDiskTable_SideRecord,
) (pb.ResStatus, string) {
	name := plan.sideDevName
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if dev == nil {
		return pb.ResStatus_RES_STATUS_MISSING, ""
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	devNo, err := s.dm.DevNo(ctx, s.disk)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	runs := sideRunSectors(rec, plan.extentSize)
	if !sideDmConverged(targets, runs, devNo) {
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"table does not match the %d allocated extent run(s)", len(runs))
	}
	if dev.Suspended {
		return pb.ResStatus_RES_STATUS_OK, "suspended"
	}
	return pb.ResStatus_RES_STATUS_OK, ""
}

// probeAboveSideDev probes everything the side's desired state puts on top of
// the side device (the side's background read, SH29): a migration
// destination's two rows first, by the rule its converge applies
// (probeMigrDst, reportMigrDst), whose answer to where the per-CN stacks serve
// from the per-CN rows are judged by, and then the per-CN stacks and the
// migration source's rows (probeStacks).
func (s *DnAgentServer) probeAboveSideDev(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
) {
	cloneLive := false
	if plan.wantMigr {
		seen := s.probeMigrDst(ctx, plan)
		s.reportMigrDst(st, plan, info, seen)
		cloneLive = s.migrDstServing(ctx, plan, seen).cloneLive()
	}
	s.probeStacks(ctx, st, plan, info, cloneLive)
}

// probeMigrDst reads a migration destination as DN13's steps would find it,
// in their order, with nothing built: step (2) (migrMetaRead); then, where it
// holds, the stop for the cutover while the request carries cutover_pending,
// which reads nothing more, as the converge runs nothing more (runMigrDst);
// else the walk of the source connection, then, only over a source that
// serves, step (4) (migrCloneRead). Short of the stop for the cutover, the
// dm-clone's status line is read whenever the dm-clone is there, whatever
// stopped before it, because its row carries the line for the gateway's
// hydration checks (architecture.md, Live-state reporting).
func (s *DnAgentServer) probeMigrDst(
	ctx context.Context,
	plan *sidePlan,
) *migrDstSeen {
	seen := &migrDstSeen{}
	name := plan.migrFinalName()
	// present: step (4)'s read found the dm-clone as the step leaves it, so
	// the status read needs no probe of its own. Short of that a step
	// stopped, and its stop decides both rows, so a dm-clone whose probe
	// does not answer only goes without its status line.
	present := false
	if seen.metaErr = s.migrMetaRead(ctx, plan); seen.metaErr == nil {
		if plan.migrDst.GetCutoverPending() {
			seen.awaitingCutover = true
			return seen
		}
		state, err := s.host.ListSubsys(ctx, plan.srcNqnOfDst())
		seen.conn, seen.connErr, seen.connUnread = state, err, err != nil
		if seen.srcServes() {
			seen.cloneErr = s.migrCloneRead(ctx, plan, state.DevicePath)
			present = seen.cloneErr == nil
		}
	}
	if !present {
		if dev, err := s.dm.Info(ctx, name); err != nil || dev == nil {
			return seen
		}
	}
	seen.raw, seen.statusErr = s.dm.Status(ctx, name)
	seen.statusRead = seen.statusErr == nil
	return seen
}

// probeFencedLinear checks a per-CN dm-linear of a migration source inside
// the cutover grace window (architecture.md, Migration, src step 2) by the
// test phase 1 converges it to (ensureDmLinearSuspended): the device there
// and suspended, on whatever table it carries — its pre-fence one, or the
// dm-error of DN12 rule 1's known-limit second window — so the read a check
// round starts reads every window as the converge that holds it reports it.
func (s *DnAgentServer) probeFencedLinear(
	ctx context.Context,
	name string,
) (pb.ResStatus, string) {
	dev, err := s.dm.Info(ctx, name)
	switch {
	case err != nil:
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	case dev == nil:
		return pb.ResStatus_RES_STATUS_MISSING, ""
	case !dev.Suspended:
		return pb.ResStatus_RES_STATUS_ERROR,
			"not suspended inside the migration cutover grace window"
	}
	return pb.ResStatus_RES_STATUS_OK, fenceSuspendedDetails
}

// probeStacks probes the per-CN stacks (probeCnStacks) and the migration
// source's rows. It is shared by probeAboveSideDev and by a converge pass
// whose migration destination could not read where its per-CN stacks are
// (convergeSide's migrCloneUnread branch), which reports those rows instead
// of converging them. cloneLive is the destination rule's reading of the
// dm-clone (migrServing.cloneLive), false on a side that is no destination.
func (s *DnAgentServer) probeStacks(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
	cloneLive bool,
) {
	t := st.tracker
	s.probeCnStacks(ctx, st, plan, info, cloneLive)

	if plan.migrSrcDeferred {
		// The request carries dst_provisioned false (the destination not
		// provisioned yet, or the cutover still pending): this side is serving
		// exactly as if it had no migr_src_conf, and only the would-be rows
		// differ (architecture.md, Migration).
		s.reportMigrSrcDeferred(st, plan, info)
	}

	if plan.migrSrc != nil {
		srcInfo := &pb.SideInfo_MigrSrcInfo{}
		info.MigrSrcInfo = srcInfo
		name := plan.migrSrcName()
		status, details := s.probeDmTarget(
			ctx, name, "linear", plan.sectors, plan.sideDevPath)
		srcInfo.DmLinearInfo = t.Set(
			resKeyMigrSrcDm, name, status, details)
		if plan.wantExport {
			nqn := plan.migrSrcNqn()
			status, details = s.probeExport(ctx,
				agent.SubsysConf{
					Nqn: nqn,
					AllowedHosts: []string{s.nf.DnHostNqn(
						plan.clusterId, plan.migrSrc.GetDstDnId())},
				},
				agent.NsConf{
					Nqn:        nqn,
					Nsid:       sideNsid,
					DevicePath: s.nf.DmPath(name),
					AnaGrpId:   common.AnaGrpIdOptimized,
				})
			srcInfo.NvmeofInfo = t.Set(
				resKeyMigrSrcNvmeof, nqn, status, details)
		}
	}
}

// probeCnStacks probes the per-CN stacks alone: each CN's dm-error, dm-linear
// and, where the level has an export layer, export. Besides probeStacks, a
// converge pass of a migration source whose export build failed reports
// those rows through it (convergeSide), as it leaves the stacks where they
// are.
func (s *DnAgentServer) probeCnStacks(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
	cloneLive bool,
) {
	t := st.tracker
	// A read-only probe must not start the window (DN16/SH29), so it asks
	// whether one is already running rather than going through beginFence.
	fencing := plan.migrSrc != nil && s.inFence(st)

	info.CnIdToDmError = make(map[uint64]*pb.ResInfo, len(plan.cnIds))
	info.CnIdToDmLinear = make(map[uint64]*pb.ResInfo, len(plan.cnIds))
	for _, cnId := range plan.cnIds {
		errName := plan.errName(cnId)
		status, details := s.probeDmTarget(
			ctx, errName, "error", plan.sectors, "")
		info.CnIdToDmError[cnId] = t.Set(
			resKeyOf(resKeyDmErrorFmt, cnId), errName, status, details)

		linName := plan.linearName(cnId)
		// Inside the cutover grace window the linear is suspended on purpose,
		// on whatever table it carries, so the test is phase 1's: reporting
		// the window as a table mismatch would make a healthy cutover look
		// broken for a minute.
		if fencing {
			status, details = s.probeFencedLinear(ctx, linName)
		} else {
			status, details = s.probeDmTarget(ctx, linName, "linear",
				plan.sectors, plan.linearBacking(cnId, cloneLive))
		}
		info.CnIdToDmLinear[cnId] = t.Set(
			resKeyOf(resKeyDmLinearFmt, cnId), linName, status, details)
	}

	if plan.wantExport {
		uuid, nguid := plan.nsIdentity()
		cntlidMin, cntlidMax := plan.cntlidRange()
		info.CnIdToNvmeof = make(map[uint64]*pb.ResInfo, len(plan.cnIds))
		for _, cnId := range plan.cnIds {
			nqn := plan.sideNqn(cnId)
			status, details := s.probeExport(ctx,
				agent.SubsysConf{
					Nqn:          nqn,
					Serial:       fmt.Sprintf(common.IdKeyFmt, plan.legId),
					Model:        common.NvmeModel,
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
			info.CnIdToNvmeof[cnId] = t.Set(
				resKeyOf(resKeyNvmeofFmt, cnId), nqn, status, details)
		}
	}
}

// probeDmTarget checks one dm device against its desired single-target
// table. backingPath is empty for a dm-error device.
func (s *DnAgentServer) probeDmTarget(
	ctx context.Context,
	name string,
	targetType string,
	sectors uint64,
	backingPath string,
) (pb.ResStatus, string) {
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if dev == nil {
		return pb.ResStatus_RES_STATUS_MISSING, ""
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if len(targets) != 1 || targets[0].Type != targetType {
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"table is not a single %s target", targetType)
	}
	if sectors != 0 && targets[0].Length != sectors {
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"size is %d sectors, want %d", targets[0].Length, sectors)
	}
	if backingPath != "" {
		devNo, err := s.dm.DevNo(ctx, backingPath)
		if err != nil {
			return pb.ResStatus_RES_STATUS_ERROR, err.Error()
		}
		if len(targets[0].Args) < 1 || targets[0].Args[0] != devNo {
			return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
				"target is %v, want %s (%s)",
				targets[0].Args, devNo, backingPath)
		}
	}
	if dev.Suspended {
		return pb.ResStatus_RES_STATUS_OK, "suspended"
	}
	return pb.ResStatus_RES_STATUS_OK, ""
}

// probeExport checks one nvmet subsystem, its namespace and its port link.
func (s *DnAgentServer) probeExport(
	ctx context.Context,
	subsys agent.SubsysConf,
	ns agent.NsConf,
) (pb.ResStatus, string) {
	exists, err := s.nvmet.SubsysExists(ctx, subsys.Nqn)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if !exists {
		return pb.ResStatus_RES_STATUS_MISSING, ""
	}
	ok, details, err := s.nvmet.ProbeSubsystem(ctx, subsys)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if !ok {
		return pb.ResStatus_RES_STATUS_ERROR, details
	}
	state, err := s.nvmet.ProbeNamespace(ctx, ns.Nqn, ns.Nsid)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	switch {
	case !state.Exists:
		return pb.ResStatus_RES_STATUS_ERROR, "namespace missing"
	case !state.Enabled:
		return pb.ResStatus_RES_STATUS_ERROR, "namespace not enabled"
	case state.DevicePath != ns.DevicePath:
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"device_path is %q, want %q", state.DevicePath, ns.DevicePath)
	case state.AnaGrpId != ns.AnaGrpId:
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"ana_grpid is %d, want %d", state.AnaGrpId, ns.AnaGrpId)
	}
	linked, err := s.nvmet.PortLinked(ctx, s.port.PortId, ns.Nqn)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if !linked {
		return pb.ResStatus_RES_STATUS_ERROR, "not linked to the port"
	}
	return pb.ResStatus_RES_STATUS_OK, ""
}
