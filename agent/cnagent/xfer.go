package cnagent

import (
	"context"
	"fmt"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// CN17 — transfers (fig. `100Transfer`), built by both roles. The origin
// namespace's own retirement is not done here: it is the CN16
// effective-suspend rule, which an `auto_suspend` transfer feeds.

// xferServed reports whether this cntlr backs the transfer device with real
// data. A standby exports a plain dm-error table of the same size, and so does
// a primary once the level suppresses thin pools — or, per U4, one whose origin
// td is still provisioning-deferred and therefore has no raid0 to map.
func (p *cntlrPlan) xferServed(xp *xferPlan) bool {
	return p.primary && p.level < pb.SpLevel_SP_LEVEL_NO_THINPOOL &&
		!xp.deferred
}

// xferAnaGrpId is optimized on the serving primary and inaccessible
// everywhere else — the transfer has no suspend state of its own. A deferred
// transfer stays inaccessible for the CN16 reason (U4): promoting a path over
// an error table would hand the destination IO errors instead of a queue.
func (p *cntlrPlan) xferAnaGrpId(xp *xferPlan) int {
	if p.primary && p.wantAny && !xp.deferred {
		return common.AnaGrpIdOptimized
	}
	return common.AnaGrpIdInaccessible
}

func (s *CnAgentServer) xferSubsysConf(
	plan *cntlrPlan,
	xp *xferPlan,
) agent.SubsysConf {
	cntlidMin, cntlidMax := plan.cntlidRange()
	// [D2] applied to the xfer: every cntlr exports the same identity, so a
	// destination clone connecting through several of them sees one
	// multipath device.
	return agent.SubsysConf{
		Nqn:          xp.nqn,
		Serial:       fmt.Sprintf(common.IdKeyFmt, xp.xferId),
		Model:        xferModel,
		CntlidMin:    cntlidMin,
		CntlidMax:    cntlidMax,
		AllowedHosts: xp.xfer.GetAllowedHosts(),
	}
}

func (s *CnAgentServer) xferNsConf(
	plan *cntlrPlan,
	xp *xferPlan,
	anaGrpId int,
) agent.NsConf {
	return agent.NsConf{
		Nqn:        xp.nqn,
		Nsid:       int(xp.xfer.GetOriNsIdx()),
		DevicePath: s.nf.DmPath(xp.finalName),
		Uuid:       xp.ori.ns.GetDevUuid(),
		Nguid:      xp.ori.ns.GetDevNguid(),
		AnaGrpId:   anaGrpId,
	}
}

// ensureXfer converges one transfer's device and its nvmet export. An origin
// that resolves to nothing in this request leaves the xfer's own resources in
// error and the pass continues (CN29).
func (s *CnAgentServer) ensureXfer(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	xp *xferPlan,
	info *pb.CntlrInfo,
) {
	dmKey := resKeyOf(resKeyXferDmFmt, xp.xferId)
	ssKey := resKeyOf(resKeyXferSsFmt, xp.xferId)
	nsKey := resKeyOf(resKeyXferNsFmt, xp.xferId)
	if xp.ori == nil || xp.ori.td == nil {
		details := fmt.Sprintf("origin namespace %s/%d not found",
			xp.xfer.GetOriNqn(), xp.xfer.GetOriNsIdx())
		info.XferIdToDmLinear[xp.xferId] = st.tracker.Err(
			dmKey, xp.finalName, details)
		info.XferIdToSubsystem[xp.xferId] = st.tracker.Err(
			ssKey, xp.nqn, details)
		info.XferIdToNamespace[xp.xferId] = st.tracker.Err(
			nsKey, nsResName(xp.nqn, int(xp.xfer.GetOriNsIdx())), details)
		return
	}

	var err error
	if plan.xferServed(xp) {
		err = s.ensureDmLinear(ctx, xp.finalName, xp.sectors,
			s.nf.DmPath(xp.ori.td.raid0Name), 0)
	} else {
		err = s.ensureDmError(ctx, xp.finalName, xp.sectors)
	}
	// A deferred transfer's three rows report PROVISIONING (U4): the devices
	// are exactly what the effective desired state wants, and none of them can
	// carry data until the origin's chain clears.
	info.XferIdToDmLinear[xp.xferId] = deferredFromErr(
		st.tracker, xp.deferred, dmKey, xp.finalName, "", err)
	if err != nil {
		info.XferIdToSubsystem[xp.xferId] = st.tracker.Missing(
			ssKey, xp.nqn, "transfer device missing")
		info.XferIdToNamespace[xp.xferId] = st.tracker.Missing(
			nsKey, nsResName(xp.nqn, int(xp.xfer.GetOriNsIdx())),
			"transfer device missing")
		return
	}

	if err := s.nvmet.EnsureSubsystem(
		ctx, s.xferSubsysConf(plan, xp)); err != nil {
		info.XferIdToSubsystem[xp.xferId] = st.tracker.Err(
			ssKey, xp.nqn, err.Error())
		return
	}
	nsErr := s.ensureXferNamespace(ctx, plan, xp)
	info.XferIdToNamespace[xp.xferId] = deferredFromErr(
		st.tracker, xp.deferred,
		nsKey, nsResName(xp.nqn, int(xp.xfer.GetOriNsIdx())), "", nsErr)
	linkErr := s.nvmet.EnsurePortLink(ctx, common.NvmetPortId, xp.nqn)
	info.XferIdToSubsystem[xp.xferId] = deferredFromErr(
		st.tracker, xp.deferred, ssKey, xp.nqn, "", linkErr)
}

// ensureXferNamespace mirrors ensureNamespaceObject: the ANA group is left to
// the CN9 final pass so the build order stays "objects first, ANA last".
func (s *CnAgentServer) ensureXferNamespace(
	ctx context.Context,
	plan *cntlrPlan,
	xp *xferPlan,
) error {
	nsIdx := int(xp.xfer.GetOriNsIdx())
	state, err := s.nvmet.ProbeNamespace(ctx, xp.nqn, nsIdx)
	if err != nil {
		return err
	}
	anaGrpId := common.AnaGrpIdInaccessible
	if state.Exists && state.AnaGrpId != 0 {
		anaGrpId = state.AnaGrpId
	}
	return s.nvmet.EnsureNamespace(ctx, s.xferNsConf(plan, xp, anaGrpId))
}

// removeXfer tears one transfer down top-down: the nvmet export releases the
// dm device, then the device goes.
func (s *CnAgentServer) removeXfer(ctx context.Context, xp *xferPlan) {
	s.removeExport(ctx, xp.nqn)
	s.removeDm(ctx, xp.finalName)
}

func (s *CnAgentServer) dropXferKeys(st *cntlrState, xferId uint64) {
	st.tracker.Drop(resKeyOf(resKeyXferDmFmt, xferId))
	st.tracker.Drop(resKeyOf(resKeyXferSsFmt, xferId))
	st.tracker.Drop(resKeyOf(resKeyXferNsFmt, xferId))
}

// probeXferDm is the read-only view of a transfer device.
func (s *CnAgentServer) probeXferDm(
	ctx context.Context,
	plan *cntlrPlan,
	xp *xferPlan,
) (pb.ResStatus, string) {
	if plan.xferServed(xp) {
		return s.probeDmTarget(ctx, xp.finalName, "linear", xp.sectors,
			s.nf.DmPath(xp.ori.td.raid0Name), 0)
	}
	return s.probeDmArgs(ctx, xp.finalName, "error", xp.sectors, nil, 0)
}
