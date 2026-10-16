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

// xferAnaGrpId is the transfer's desired group: optimized on a primary below
// SP_LEVEL_DISABLE whose transfer is not deferred — whether or not that
// primary serves data — and inaccessible everywhere else; the transfer has no
// suspend state of its own. So where such a primary does not serve it on
// purpose (SP_LEVEL_NO_THINPOOL ≤ level < SP_LEVEL_DISABLE, or an origin that
// no longer resolves, CN29), the transfer namespace stays optimized over an
// error table and the destination reads IO errors: an operator's choice or a
// permanent loss, which a wait would never end. A deferred transfer is the
// one held inaccessible, for the CN16 reason ([D15]): its data is still to
// come, and promoting a path over an error table would hand the destination
// IO errors instead of a queue. The build moves a transfer to this group only
// in a pass that put its transfer device where the desired state wants it
// (xferAnaMove), and a transfer that should serve waits inaccessible while
// its device is an error table (xferAnaWanted).
func (p *cntlrPlan) xferAnaGrpId(xp *xferPlan) int {
	if p.primary && p.wantAny && !xp.deferred {
		return common.AnaGrpIdOptimized
	}
	return common.AnaGrpIdInaccessible
}

// xferAnaWanted is the ANA group CN17 wants a transfer namespace in, given
// whether its transfer device's live table is an error table (onError):
// inaccessible when the transfer should serve (xferServed) and its device is
// an error table — a clone reading through it then waits rather than reads IO
// errors — and xferAnaGrpId otherwise, a table that could not be read
// included. The build's transfer ANA move takes it for what the device step
// did (xferAnaMove), and both channels' transfer namespace rows compare the
// group with it for the device's live table (xferRowAnaGrpId).
func (p *cntlrPlan) xferAnaWanted(xp *xferPlan, onError bool) int {
	if onError && p.xferServed(xp) {
		return common.AnaGrpIdInaccessible
	}
	return p.xferAnaGrpId(xp)
}

// xferAnaMove is the group the build's transfer ANA loop moves a transfer
// whose desired group is optimized to, and false where the transfer keeps
// the group it has (CN17): a device the step put where the desired state
// wants takes the desired group, and a served transfer whose device step
// failed on an error table takes inaccessible. Any other failed step keeps
// the group, as a failed ns-dev step does (nsAnaMove).
func (p *cntlrPlan) xferAnaMove(xp *xferPlan, outcome xferDevOutcome) (
	int, bool,
) {
	switch {
	case outcome == xferDevPut:
		return p.xferAnaWanted(xp, false), true
	case outcome == xferDevFailedParked && p.xferServed(xp):
		return p.xferAnaWanted(xp, true), true
	}
	return 0, false
}

// xferDevOutcome is what ensureXfer's device step did with one transfer
// device, which decides the transfer's ANA move (xferAnaMove) and whether it
// waits for the CN10 retry (build).
type xferDevOutcome int

const (
	// xferDevFailed: the device step failed, and the device's live table is
	// not known to be an error table — it is something else, or it could not
	// be read.
	xferDevFailed xferDevOutcome = iota
	// xferDevFailedParked: the device step failed, and the device's live
	// table is an error table (xferDevOnError).
	xferDevFailedParked
	// xferDevPut: the step put the device where the desired state wants it,
	// or found it there — the linear over the origin's raid0 where the
	// transfer is served, the error table where it is not on purpose
	// (xferServed), a missing origin's demotion included.
	xferDevPut
)

// xferRows says which of a transfer's export rows the build writes after its
// transfer ANA loop (reportXferExport).
type xferRows int

const (
	// xferRowsNone: ensureXfer wrote every row the transfer gets in the
	// pass — all three, or, when the subsystem's step failed, the device's
	// and the subsystem's alone.
	xferRowsNone xferRows = iota
	// xferRowsNamespace: ensureXfer converged the export, and its namespace
	// row comes from the namespace step's error or a read of the namespace.
	xferRowsNamespace
	// xferRowsExport: the device step failed, so ensureXfer left the export
	// alone, and its subsystem and namespace rows are what a read of them
	// finds, as in a check round.
	xferRowsExport
)

// xferStep is what ensureXfer did with one transfer: its device step's
// outcome, the rows left to the build, and the namespace step's error.
type xferStep struct {
	outcome xferDevOutcome
	rows    xferRows
	nsErr   error
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
		Model:        common.NvmeModel,
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

// ensureXfer converges one transfer's device and its nvmet export, and says
// what it did (xferStep), which decides the transfer's ANA move and the rows
// the build writes after its transfer ANA loop. An origin that resolves to
// nothing in this request leaves the xfer's own resources in error and the
// pass continues (CN29) — with the device still demoted onto an error table
// of its own live size, so that it lets go of the departed td's raid0 on a
// pass whose sweep an unanswered listing stopped too (CN17). A device step
// that fails leaves the export alone, and reads the device's live table
// afterwards, so the build can tell a transfer that should serve but sits on
// an error table — a promotion that came before the sides' switch-over
// ([D16]) fails on the origin's raid0 — from one that may still serve.
func (s *CnAgentServer) ensureXfer(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	xp *xferPlan,
	info *pb.CntlrInfo,
) xferStep {
	dmKey := resKeyOf(resKeyXferDmFmt, xp.xferId)
	ssKey := resKeyOf(resKeyXferSsFmt, xp.xferId)
	nsKey := resKeyOf(resKeyXferNsFmt, xp.xferId)
	if xp.ori == nil || xp.ori.td == nil {
		// The build's copy of pre-step 3, which a stopped sweep does not
		// run: the plan cannot size the device, so demoteXfer sizes it from
		// its live table. After a pre-step 3 that ran it finds the device
		// demoted already and reloads nothing.
		outcome := xferDevPut
		if err := s.demoteXfer(ctx, xp); err != nil {
			outcome = xferDevFailed
		}
		details := fmt.Sprintf("origin namespace %s/%d not found",
			xp.xfer.GetOriNqn(), xp.xfer.GetOriNsIdx())
		info.XferIdToDmLinear[xp.xferId] = st.tracker.Err(
			dmKey, xp.finalName, details)
		info.XferIdToSubsystem[xp.xferId] = st.tracker.Err(
			ssKey, xp.nqn, details)
		info.XferIdToNamespace[xp.xferId] = st.tracker.Err(
			nsKey, nsResName(xp.nqn, int(xp.xfer.GetOriNsIdx())), details)
		return xferStep{outcome: outcome}
	}

	var err error
	if plan.xferServed(xp) {
		var raid0No string
		raid0No, err = s.dmDevNo(ctx, nil, xp.ori.td.raid0Name)
		if err == nil {
			err = s.ensureDmLinear(ctx, xp.finalName, xp.sectors, raid0No, 0)
		}
	} else {
		err = s.ensureDmError(ctx, xp.finalName, xp.sectors)
	}
	// A deferred transfer's three rows report PROVISIONING ([D15]): the devices
	// are exactly what the effective desired state wants, and none of them can
	// carry data until the origin's chain clears.
	info.XferIdToDmLinear[xp.xferId] = deferredFromErr(
		st.tracker, xp.deferred, dmKey, xp.finalName, "", err)
	if err != nil {
		outcome := xferDevFailed
		if s.xferDevOnError(ctx, nil, xp) {
			outcome = xferDevFailedParked
		}
		return xferStep{outcome: outcome, rows: xferRowsExport}
	}

	if err := s.nvmet.EnsureSubsystem(
		ctx, s.xferSubsysConf(plan, xp)); err != nil {
		info.XferIdToSubsystem[xp.xferId] = st.tracker.Err(
			ssKey, xp.nqn, err.Error())
		return xferStep{outcome: xferDevPut}
	}
	nsErr := s.ensureXferNamespace(ctx, plan, xp)
	linkErr := s.nvmet.EnsurePortLink(ctx, s.port.PortId, xp.nqn)
	info.XferIdToSubsystem[xp.xferId] = deferredFromErr(
		st.tracker, xp.deferred, ssKey, xp.nqn, "", linkErr)
	return xferStep{outcome: xferDevPut, rows: xferRowsNamespace, nsErr: nsErr}
}

// reportXferExport writes the export rows ensureXfer left to the build, after
// its transfer ANA loop (CN28), as reportNamespaces writes the namespaces':
// the namespace step's error wins, and otherwise the namespace is read and
// its group compared with xferRowAnaGrpId's for the device's live table; an
// export a failed device step left alone has its subsystem read too. So the
// two channels report the same rows for the same node.
func (s *CnAgentServer) reportXferExport(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	xp *xferPlan,
	step xferStep,
	info *pb.CntlrInfo,
) {
	if step.rows == xferRowsNone {
		return
	}
	t := st.tracker
	if step.rows == xferRowsExport {
		status, details := s.probeExport(ctx, s.xferSubsysConf(plan, xp))
		info.XferIdToSubsystem[xp.xferId] = deferredSet(t, xp.deferred,
			resKeyOf(resKeyXferSsFmt, xp.xferId), xp.nqn, status, details)
	}
	nsKey := resKeyOf(resKeyXferNsFmt, xp.xferId)
	nsName := nsResName(xp.nqn, int(xp.xfer.GetOriNsIdx()))
	if step.nsErr != nil {
		info.XferIdToNamespace[xp.xferId] = deferredFromErr(
			t, xp.deferred, nsKey, nsName, "", step.nsErr)
		return
	}
	status, details := s.probeNamespaceObject(ctx, s.xferNsConf(plan, xp,
		s.xferRowAnaGrpId(ctx, nil, plan, xp)))
	info.XferIdToNamespace[xp.xferId] = deferredSet(
		t, xp.deferred, nsKey, nsName, status, details)
}

// xferDevOnError reports whether a transfer device's live table is an error
// table, the shape CN17 gives a transfer device that does not serve. It reads
// from rd, the reading of a check round or an info read, and with no reading
// the device's own `dmsetup info` and `dmsetup table`; a device that is not
// there, or a read that fails, answers false.
func (s *CnAgentServer) xferDevOnError(
	ctx context.Context,
	rd *cnReading,
	xp *xferPlan,
) bool {
	dev, err := s.dmInfo(ctx, rd, xp.finalName)
	if err != nil || dev == nil {
		return false
	}
	targets, err := s.dmTable(ctx, rd, xp.finalName)
	return err == nil && len(targets) == 1 && targets[0].Type == "error"
}

// xferRowAnaGrpId is the group a transfer namespace row compares the
// namespace's "ana_grpid" with, on both channels alike (CN28): xferAnaWanted
// for whether the transfer device's live table is an error table
// (xferDevOnError) — read from a check round's reading, or by a converge
// after its transfer ANA loop.
func (s *CnAgentServer) xferRowAnaGrpId(
	ctx context.Context,
	rd *cnReading,
	plan *cntlrPlan,
	xp *xferPlan,
) int {
	if !plan.xferServed(xp) {
		// The table cannot change the group xferAnaWanted answers, so it
		// is not read.
		return plan.xferAnaGrpId(xp)
	}
	return plan.xferAnaWanted(xp, s.xferDevOnError(ctx, rd, xp))
}

// ensureXferNamespace mirrors ensureNamespaceObject: the ANA group is left to
// the CN9 final pass so the build order stays "objects first, ANA last", and
// a namespace that is not enabled gets `inaccessible` before its enable.
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
	return s.nvmet.EnsureNamespace(
		ctx, s.xferNsConf(plan, xp, objectAnaGrpId(state)))
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
		return s.probeDmTarget(ctx, plan.rd, xp.finalName, "linear",
			xp.sectors, xp.ori.td.raid0Name, 0)
	}
	return s.probeDmArgs(
		ctx, plan.rd, xp.finalName, "error", xp.sectors, nil, 0)
}
