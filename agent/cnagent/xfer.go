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
// a primary once the level suppresses thin pools — or, per [D15], one whose origin
// td is still provisioning-deferred and therefore has no raid0 to map — or one
// whose origin no longer resolves in this request (CN29): the plan cannot say
// what it maps, and a linear still mapping a td that left the request would
// hold that td's raid0 open against L6's removal. The CN9 pre-step then sizes
// the error table from the device's own live table (demoteXfer).
func (p *cntlrPlan) xferServed(xp *xferPlan) bool {
	return p.primary && p.level < pb.SpLevel_SP_LEVEL_NO_THINPOOL &&
		!xp.deferred && xp.ori != nil && xp.ori.td != nil
}

// xferAnaGrpId is optimized on a primary below SP_LEVEL_DISABLE whose transfer
// is not deferred — whether or not that primary serves data — and inaccessible
// everywhere else; the transfer has no suspend state of its own. So where such
// a primary does not serve it (SP_LEVEL_NO_THINPOOL ≤ level < SP_LEVEL_DISABLE,
// or an origin that no longer resolves, CN29), the transfer namespace stays
// optimized over an error table and the destination reads IO errors. A
// deferred transfer is the one held inaccessible, for the CN16 reason ([D15]):
// its data is still to come, and promoting a path over an error table would
// hand the destination IO errors instead of a queue.
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
// error and the pass continues (CN29) — with the device still demoted onto an
// error table of its own live size, so that it lets go of the departed td's
// raid0 on a pass whose sweep an unanswered listing stopped too (CN17).
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
		// The build's copy of pre-step 3, which a stopped sweep does not
		// run: the plan cannot size the device, so demoteXfer sizes it from
		// its live table. After a pre-step 3 that ran it finds the device
		// demoted already and reloads nothing.
		s.demoteXfer(ctx, xp)
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
	// A deferred transfer's three rows report PROVISIONING ([D15]): the devices
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
	linkErr := s.nvmet.EnsurePortLink(ctx, s.port.PortId, xp.nqn)
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
