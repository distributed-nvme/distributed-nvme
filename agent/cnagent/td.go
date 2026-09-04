package cnagent

import (
	"context"
	"fmt"
	"strconv"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// CN15 — per-td devices
// ---------------------------------------------------------------------------

// raid0Args is the dm-striped table of one td across its per-slice thin
// volumes, in slice_idx order.
func (s *CnAgentServer) raid0Args(
	ctx context.Context,
	plan *cntlrPlan,
	tp *tdPlan,
) ([]string, error) {
	if plan.stripeSectors() == 0 {
		return nil, fmt.Errorf("dm_raid0_conf.stripe_size is 0")
	}
	args := []string{
		strconv.Itoa(len(plan.slices)),
		strconv.FormatUint(plan.stripeSectors(), 10),
	}
	for _, sp := range plan.slices {
		devNo, err := s.dm.DevNo(
			ctx, s.nf.DmPath(plan.thinName(tp.tdId, sp.sliceId)))
		if err != nil {
			return nil, err
		}
		args = append(args, devNo, "0")
	}
	return args, nil
}

func (s *CnAgentServer) ensureRaid0(
	ctx context.Context,
	plan *cntlrPlan,
	tp *tdPlan,
) error {
	args, err := s.raid0Args(ctx, plan, tp)
	if err != nil {
		return err
	}
	return s.ensureDmSingle(
		ctx, tp.raid0Name, "striped", tp.sectors, args, 0, false)
}

// ---------------------------------------------------------------------------
// CN16 — namespace backing devices
// ---------------------------------------------------------------------------

// nsDevTableMatches compares a live ns-dev table with the CN16 backing.
// dm-flakey reproduces its feature arguments in a kernel-version-dependent
// order, so the read-only case compares the four positional arguments and
// insists the `error_writes` feature is present rather than demanding an exact
// argument list — a probe that reported drift here would reload a live,
// correct table on every pass.
func nsDevTableMatches(
	targets []agent.DmTarget,
	np *nsPlan,
	devNo string,
) bool {
	if len(targets) != 1 || targets[0].Length != np.sectors {
		return false
	}
	target := targets[0]
	if np.flakey {
		if target.Type != "flakey" || len(target.Args) < 5 {
			return false
		}
		if target.Args[0] != devNo || target.Args[1] != "0" ||
			target.Args[2] != "0" || target.Args[3] != "1" {
			return false
		}
		for _, arg := range target.Args[4:] {
			if arg == "error_writes" {
				return true
			}
		}
		return false
	}
	return target.Type == "linear" && len(target.Args) == 2 &&
		target.Args[0] == devNo && target.Args[1] == "0"
}

func nsDevTable(np *nsPlan, devNo string) string {
	if np.flakey {
		return agent.FlakeyErrorWritesTable(np.sectors, devNo)
	}
	return agent.LinearTable(np.sectors, devNo, 0)
}

// ensureNsDev converges one namespace's own dm-linear (§3.3 step 5) onto the
// CN16 backing, then applies the §11.6 suspend state. Suspending is the third
// and last deliberate suspension in dnv; the ANA move to `inaccessible` that
// must precede it has already happened in the retire phase (CN9).
func (s *CnAgentServer) ensureNsDev(
	ctx context.Context,
	np *nsPlan,
) error {
	if np.backingName == "" {
		return fmt.Errorf("namespace has no thin device")
	}
	if np.sectors == 0 {
		return fmt.Errorf("namespace size is 0")
	}
	// The td's dm-error is the one backing that may not exist yet when a
	// namespace is pointed at it — the retire phase reaches here before the
	// build phase creates it.
	if np.td != nil && np.backingName == np.td.errorName {
		if err := s.ensureDmError(
			ctx, np.td.errorName, np.td.sectors); err != nil {
			return err
		}
	}
	devNo, err := s.dm.DevNo(ctx, s.nf.DmPath(np.backingName))
	if err != nil {
		return err
	}
	table := nsDevTable(np, devNo)
	dev, err := s.dm.Info(ctx, np.devName)
	if err != nil {
		return err
	}
	if dev == nil {
		if err := s.dm.Create(ctx, np.devName, table); err != nil {
			return err
		}
		dev = &agent.DmDevInfo{}
	} else {
		targets, tableErr := s.dm.Table(ctx, np.devName)
		if tableErr != nil {
			return tableErr
		}
		if !nsDevTableMatches(targets, np, devNo) || dev.ReadOnly {
			// Reload always resumes; the suspend state is re-applied below.
			if err := s.dm.Reload(ctx, np.devName, table); err != nil {
				return err
			}
			dev = &agent.DmDevInfo{}
		}
	}
	if np.suspended && !dev.Suspended {
		return s.dm.Suspend(ctx, np.devName)
	}
	if !np.suspended && dev.Suspended {
		return s.dm.Resume(ctx, np.devName)
	}
	return nil
}

// parkNsDev reloads one ns-dev onto its td's dm-error. It is both the §11.1
// old_primary step 3 and the "resume before teardown" of CN21: the reload
// resumes a deliberately suspended device, which a suspended device's own
// removal (and the nvmet disable above it) requires.
func (s *CnAgentServer) parkNsDev(
	ctx context.Context,
	np *nsPlan,
) error {
	if np.td == nil {
		return nil
	}
	dev, err := s.dm.Info(ctx, np.devName)
	if err != nil || dev == nil {
		return err
	}
	if err := s.ensureDmError(ctx, np.td.errorName, np.td.sectors); err != nil {
		return err
	}
	devNo, err := s.dm.DevNo(ctx, s.nf.DmPath(np.td.errorName))
	if err != nil {
		return err
	}
	targets, err := s.dm.Table(ctx, np.devName)
	if err != nil {
		return err
	}
	parked := len(targets) == 1 && targets[0].Type == "linear" &&
		len(targets[0].Args) == 2 && targets[0].Args[0] == devNo
	if parked && !dev.Suspended {
		return nil
	}
	return s.dm.Reload(
		ctx, np.devName, agent.LinearTable(np.sectors, devNo, 0))
}

// ---------------------------------------------------------------------------
// CN16 — host-facing nvmet objects
// ---------------------------------------------------------------------------

func (s *CnAgentServer) subsysConf(
	plan *cntlrPlan,
	sp *ssPlan,
) agent.SubsysConf {
	cntlidMin, cntlidMax := plan.cntlidRange()
	hosts := sp.ss.GetAllowedHosts()
	return agent.SubsysConf{
		Nqn:          sp.nqn,
		Serial:       sp.ss.GetSerial(),
		Model:        sp.ss.GetModel(),
		CntlidMin:    cntlidMin,
		CntlidMax:    cntlidMax,
		AllowedHosts: hosts,
		AllowAnyHost: len(hosts) == 0,
	}
}

func (s *CnAgentServer) nsConf(np *nsPlan, anaGrpId int) agent.NsConf {
	return agent.NsConf{
		Nqn:        np.ss.nqn,
		Nsid:       np.nsIdx,
		DevicePath: s.nf.DmPath(np.devName),
		Uuid:       np.ns.GetDevUuid(),
		Nguid:      np.ns.GetDevNguid(),
		AnaGrpId:   anaGrpId,
	}
}

// ensureNamespaceObject creates or converges one nvmet namespace **without**
// moving its ANA group: an existing namespace keeps the group it has and the
// CN9 final pass promotes it, a fresh one starts `inaccessible`. Folding the
// ANA write in here would both break the retire→build ordering and make an
// idempotent re-apply write `ana_grpid` twice.
func (s *CnAgentServer) ensureNamespaceObject(
	ctx context.Context,
	np *nsPlan,
) error {
	state, err := s.nvmet.ProbeNamespace(ctx, np.ss.nqn, np.nsIdx)
	if err != nil {
		return err
	}
	anaGrpId := common.AnaGrpIdInaccessible
	if state.Exists && state.AnaGrpId != 0 {
		anaGrpId = state.AnaGrpId
	}
	return s.nvmet.EnsureNamespace(ctx, s.nsConf(np, anaGrpId))
}

// ensureSubsystem converges one host-facing subsystem, its namespaces and its
// port link, and drops namespaces that left `ns_list` while the subsystem
// itself stays.
func (s *CnAgentServer) ensureSubsystem(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	sp *ssPlan,
	info *pb.CntlrInfo,
) {
	ssKey := resKeyOf(resKeySubsysFmt, sp.ssId)
	if err := s.nvmet.EnsureSubsystem(
		ctx, s.subsysConf(plan, sp)); err != nil {
		info.SsIdToSubsystem[sp.ssId] = st.tracker.Err(
			ssKey, sp.nqn, err.Error())
		return
	}
	wanted := make(map[int]struct{}, len(sp.namespaces))
	for _, np := range sp.namespaces {
		wanted[np.nsIdx] = struct{}{}
	}
	if nsids, ok, err := s.nvmet.ListNamespaces(ctx, sp.nqn); err == nil &&
		ok {
		for _, nsid := range nsids {
			if _, keep := wanted[nsid]; keep {
				continue
			}
			if err := s.nvmet.RemoveNamespace(
				ctx, sp.nqn, nsid); err != nil {
				info.SsIdToSubsystem[sp.ssId] = st.tracker.Err(
					ssKey, sp.nqn, err.Error())
				return
			}
		}
	}
	for _, np := range sp.namespaces {
		nsKey := resKeyOf(resKeyNamespaceFmt, np.nsId)
		err := s.ensureNamespaceObject(ctx, np)
		// The namespace object of a provisioning-deferred backing chain is
		// created and correct, but it is inaccessible and no host can do IO
		// through it — PROVISIONING, not OK (U4). The subsystem row above is
		// untouched: the subsystem itself is fully converged.
		info.NsIdToNamespace[np.nsId] = deferredFromErr(
			st.tracker, np.deferred,
			nsKey, nsResName(sp.nqn, np.nsIdx), "", err)
	}
	err := s.nvmet.EnsurePortLink(ctx, common.NvmetPortId, sp.nqn)
	info.SsIdToSubsystem[sp.ssId] = st.tracker.FromErr(ssKey, sp.nqn, "", err)
}

func nsResName(nqn string, nsIdx int) string {
	return fmt.Sprintf("%s/%d", nqn, nsIdx)
}

// setAna performs one ANA transition, probe-first: a namespace that is absent
// or already in the target group is left alone (SH16, [D4]).
func (s *CnAgentServer) setAna(
	ctx context.Context,
	nqn string,
	nsIdx int,
	grpId int,
) error {
	state, err := s.nvmet.ProbeNamespace(ctx, nqn, nsIdx)
	if err != nil {
		return err
	}
	if !state.Exists || state.AnaGrpId == grpId {
		return nil
	}
	return s.nvmet.SetNsAnaGrpId(ctx, nqn, nsIdx, grpId)
}

// ---------------------------------------------------------------------------
// Probes
// ---------------------------------------------------------------------------

func (s *CnAgentServer) probeNsDev(
	ctx context.Context,
	np *nsPlan,
) (pb.ResStatus, string) {
	dev, err := s.dm.Info(ctx, np.devName)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if dev == nil {
		return pb.ResStatus_RES_STATUS_MISSING, ""
	}
	if np.backingName == "" {
		return pb.ResStatus_RES_STATUS_ERROR, "namespace has no thin device"
	}
	devNo, err := s.dm.DevNo(ctx, s.nf.DmPath(np.backingName))
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	targets, err := s.dm.Table(ctx, np.devName)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if !nsDevTableMatches(targets, np, devNo) {
		return pb.ResStatus_RES_STATUS_ERROR,
			"table is not the desired namespace backing"
	}
	// An effectively suspended ns-dev is *expected* suspended (CN28).
	if np.suspended != dev.Suspended {
		if dev.Suspended {
			return pb.ResStatus_RES_STATUS_ERROR, "unexpectedly suspended"
		}
		return pb.ResStatus_RES_STATUS_ERROR, "not suspended"
	}
	if dev.Suspended {
		return pb.ResStatus_RES_STATUS_OK, detailsSuspended
	}
	return pb.ResStatus_RES_STATUS_OK, ""
}

// probeExport checks one nvmet subsystem and its port link.
func (s *CnAgentServer) probeExport(
	ctx context.Context,
	conf agent.SubsysConf,
) (pb.ResStatus, string) {
	exists, err := s.nvmet.SubsysExists(ctx, conf.Nqn)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if !exists {
		return pb.ResStatus_RES_STATUS_MISSING, ""
	}
	ok, details, err := s.nvmet.ProbeSubsystem(ctx, conf)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if !ok {
		return pb.ResStatus_RES_STATUS_ERROR, details
	}
	linked, err := s.nvmet.PortLinked(ctx, common.NvmetPortId, conf.Nqn)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if !linked {
		return pb.ResStatus_RES_STATUS_ERROR, "not linked to the port"
	}
	return pb.ResStatus_RES_STATUS_OK, ""
}

// probeNamespaceObject checks one nvmet namespace against its desired
// identity, backing device and ANA group.
func (s *CnAgentServer) probeNamespaceObject(
	ctx context.Context,
	conf agent.NsConf,
) (pb.ResStatus, string) {
	state, err := s.nvmet.ProbeNamespace(ctx, conf.Nqn, conf.Nsid)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	switch {
	case !state.Exists:
		return pb.ResStatus_RES_STATUS_MISSING, ""
	case !state.Enabled:
		return pb.ResStatus_RES_STATUS_ERROR, "namespace not enabled"
	case state.DevicePath != conf.DevicePath:
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"device_path is %q, want %q", state.DevicePath, conf.DevicePath)
	case conf.Uuid != "" && !equalFoldHex(state.Uuid, conf.Uuid):
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"device_uuid is %q, want %q", state.Uuid, conf.Uuid)
	case conf.Nguid != "" && !equalFoldHex(state.Nguid, conf.Nguid):
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"device_nguid is %q, want %q", state.Nguid, conf.Nguid)
	case state.AnaGrpId != conf.AnaGrpId:
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"ana_grpid is %d, want %d", state.AnaGrpId, conf.AnaGrpId)
	}
	return pb.ResStatus_RES_STATUS_OK, ""
}
