package dnagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The probe map of DN18. Every function here is read-only: Get*Info and the
// Check* streams must never mutate (DN16, SH25).

func (s *DnAgentServer) probeDn(
	ctx context.Context,
	st *dnState,
) *pb.DnInfo {
	req := st.req
	t := st.tracker
	info := &pb.DnInfo{}

	size, err := s.dm.DiskSize(ctx, s.disk)
	info.DiskInfo = t.FromErr(resKeyDisk, s.disk, "", err)
	if err == nil {
		s.meta.SetDiskSize(size)
	}

	// Only the 4 KiB header is re-read: cheap enough for the 5 s health
	// rounds, and enough to catch a wiped, corrupt or foreign disk.
	details, err := s.meta.ProbeHeader(ctx, req.GetClusterId(),
		req.GetDnId(), req.GetExtentSize())
	switch {
	case err != nil:
		info.MetaInfo = t.Err(resKeyMeta, s.disk, err.Error())
	default:
		// §9.4's DN5 fail-fast is re-checked every round, so a disk whose
		// queue limits changed under the agent surfaces without a re-sync.
		if wzDetails, ok := s.checkWriteZeroes(ctx); !ok {
			info.MetaInfo = t.Err(resKeyMeta, s.disk, wzDetails)
		} else {
			info.MetaInfo = t.Ok(resKeyMeta, s.disk, details)
		}
	}

	portName := fmt.Sprintf("%d", common.NvmetPortId)
	ok, details, err := s.nvmet.ProbePort(ctx, common.NvmetPortId, s.port)
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

func (s *DnAgentServer) probeSide(
	ctx context.Context,
	st *sideState,
) *pb.SideInfo {
	var extentSize uint64
	if dn := s.getDn(
		dnKey(st.req.GetClusterId(), st.req.GetDnId())); dn != nil {
		extentSize = dn.req.GetExtentSize()
	}
	plan := newSidePlan(s.nf, st.req, extentSize)
	info := &pb.SideInfo{}
	state := s.probeSideDev(ctx, st, plan, info)
	if !plan.wantDm {
		return info
	}
	// The same gate the converge uses (ruling R4.3), so a probe can never
	// claim a stack the converge deliberately did not build.
	if state != sideDevReady {
		s.reportAboveSideDeferred(st, plan, info)
		return info
	}
	s.probeAboveSideDev(ctx, st, plan, info)
	return info
}

// probeSideDev is the read-only half of the §9.4 converge matrix (DN18): it
// checks the side's allocation record, its zeroing progress and the aggregate
// dm-linear built from its extent runs, and reports whether the side is
// exportable.
//
// Unlike ensureSideDev it never allocates and never starts or stops the
// zeroing goroutine (DN16, SH25). That is why "record absent at
// provisioned = false" reports MISSING with empty details rather than the
// matrix's "zeroing 0/n": with no record the only available extent count is
// the request's, and the etcd flag is a gate, never evidence (ruling R4.15).
func (s *DnAgentServer) probeSideDev(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
) sideDevState {
	t := st.tracker
	name := plan.sideDevName
	info.TotalExtCnt = plan.conf.GetExtCnt()
	rec, ok, err := s.meta.LookupSide(ctx, plan.spId, plan.sideId)
	if err != nil {
		// An unreadable or corrupt disk is an error, never "absent".
		info.SideDevInfo = t.Err(resKeySideDev, name, err.Error())
		return sideDevFailed
	}
	if !ok {
		if plan.provisioned {
			info.SideDevInfo = t.Err(resKeySideDev, name, tagRecordMissing)
		} else {
			info.SideDevInfo = t.Missing(resKeySideDev, name, "")
		}
		return sideDevFailed
	}
	// The counters are filled on every round, whatever the outcome below is:
	// they are what the worker's provisioned-flip rule reads
	// (§10.3). They come from the record, never from the request — the disk is
	// authoritative ([D13]).
	zeroed, total := sideZeroedCnt(rec), sideExtCnt(rec)
	info.ZeroedExtCnt, info.TotalExtCnt = zeroed, total
	if zeroed < total {
		// The device is judged on every round, provisioning or not: DN18 reads
		// this row off "the volume-table record + its zeroed_bits +
		// `dmsetup table`", and a non-OK device wins. Skipping the check while
		// the bits are incomplete would let a side whose dm-linear could not be
		// built report healthy PROVISIONING for ever — and PROVISIONING never
		// feeds err_epoch (§9.5), so nothing would ever bump a revision and
		// re-send the SyncupSide that is the only thing able to rebuild it.
		if status, details := s.probeSideDm(ctx, plan, rec); status !=
			pb.ResStatus_RES_STATUS_OK {
			info.SideDevInfo = t.Set(resKeySideDev, name, status, details)
			return sideDevFailed
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
			// The agent trusts its own bits over the flag.
			info.SideDevInfo = t.Err(resKeySideDev, name, tagNotZeroed)
			return sideDevFailed
		case zeroErr != nil:
			info.SideDevInfo = t.Err(resKeySideDev, name, zeroErr.Error())
		default:
			// Healthy, not ready: nothing is exported until the last extent is
			// zeroed, and PROVISIONING says so without feeding err_epoch.
			info.SideDevInfo = t.Provisioning(resKeySideDev, name,
				fmt.Sprintf(zeroingDetailsFmt, zeroed, total))
		}
		return sideDevProvisioning
	}
	status, details := s.probeSideDm(ctx, plan, rec)
	info.SideDevInfo = t.Set(resKeySideDev, name, status, details)
	switch {
	case status != pb.ResStatus_RES_STATUS_OK:
		return sideDevFailed
	case !plan.provisioned:
		return sideDevProvisioning
	default:
		return sideDevReady
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
// the side device. It is shared by GetSideInfo/CheckSide and by a converge
// pass that could not get past the side device.
func (s *DnAgentServer) probeAboveSideDev(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
) {
	t := st.tracker
	cloneLive := false
	if plan.wantMigr {
		dev, err := s.dm.Info(ctx, plan.migrFinalName())
		cloneLive = err == nil && dev != nil
	}

	// A read-only probe must not start the window (DN16/SH25), so it asks
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
		// Inside the §11.2 grace window the linear is still on its pre-fence
		// target and suspended on purpose, so that — not dm-error — is what
		// the probe must expect; reporting the window as a table mismatch
		// would make a healthy cutover look broken for a minute.
		backing := plan.linearBacking(cnId, cloneLive)
		if fencing {
			backing = plan.preFenceBacking(cnId, cloneLive)
		}
		status, details = s.probeDmTarget(ctx, linName, "linear",
			plan.sectors, backing)
		if fencing && status == pb.ResStatus_RES_STATUS_OK {
			details = fenceSuspendedDetails
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
			info.CnIdToNvmeof[cnId] = t.Set(
				resKeyOf(resKeyNvmeofFmt, cnId), nqn, status, details)
		}
	}

	if plan.migrSrcDeferred {
		// The destination has not provisioned: this side is serving exactly as
		// if it had no migr_src_conf, and only the would-be rows differ
		// (§11.2).
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

	if plan.wantMigr {
		dstInfo := &pb.SideInfo_MigrDstInfo{}
		info.MigrDstInfo = dstInfo
		nqn := plan.srcNqnOfDst()
		state, err := s.host.ListSubsys(ctx, nqn)
		switch {
		case err != nil:
			dstInfo.TargetInfo = t.Err(resKeyMigrDstTarget, nqn, err.Error())
		case !state.Found:
			dstInfo.TargetInfo = t.Missing(resKeyMigrDstTarget, nqn, "")
		case !state.Live:
			dstInfo.TargetInfo = t.Err(resKeyMigrDstTarget, nqn,
				"paths: "+strings.Join(state.States, ","))
		default:
			dstInfo.TargetInfo = t.Ok(resKeyMigrDstTarget, nqn,
				"paths: "+strings.Join(state.States, ","))
		}
		cloneName := plan.migrFinalName()
		if !cloneLive {
			dstInfo.DmCloneInfo = t.Missing(
				resKeyMigrDstClone, cloneName, "")
		} else if raw, err := s.dm.Status(ctx, cloneName); err != nil {
			dstInfo.DmCloneInfo = t.Err(
				resKeyMigrDstClone, cloneName, err.Error())
		} else {
			dstInfo.DmCloneInfo = t.Ok(resKeyMigrDstClone, cloneName, raw)
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
	linked, err := s.nvmet.PortLinked(ctx, common.NvmetPortId, ns.Nqn)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if !linked {
		return pb.ResStatus_RES_STATUS_ERROR, "not linked to the port"
	}
	return pb.ResStatus_RES_STATUS_OK, ""
}
