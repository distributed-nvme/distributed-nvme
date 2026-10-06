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
		ctx, tp.raid0Name, "striped", tp.sectors, args, 0)
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

// nsDevNow is np with the backing CN16 installs on this node now. It is np
// itself under every rule but 5: a dm-clone may serve only once CN18 step 5
// has turned its hydration on, which follows step 4's bitmaps and nothing
// before them (architecture.md, Clone crash recovery), so a rule-5 ns-dev
// whose dm-clone's live status does
// not show hydration on — a build or a recovery that has not finished,
// whatever stopped it — gets the td's dm-error instead: the table of rule
// 1's park, live and never under dm-flakey, though with no ANA move.
// Nothing is remembered: the status is read on every call. A read that
// fails or does not answer is an error, which licenses neither the move onto
// the dm-clone nor a park — the ns-dev keeps the table it has.
func (s *CnAgentServer) nsDevNow(
	ctx context.Context,
	np *nsPlan,
) (*nsPlan, error) {
	if np.clone == nil {
		return np, nil
	}
	hydrating, err := s.cloneHydrating(ctx, np.clone)
	if err != nil {
		return nil, err
	}
	if hydrating {
		return np, nil
	}
	parked := *np
	parked.backingName = np.td.errorName
	parked.flakey = false
	return &parked, nil
}

// ensureNsDev converges one namespace's own dm-linear (architecture.md,
// Primary cntlr, step 5) onto the
// CN16 backing. An effectively suspended namespace needs no step of its own
// here: it is **parked**, and rule 1 of the backing state machine has already
// made its backing the td's dm-error, so the ordinary reload below installs
// the park — live, never dm-suspended (architecture.md, Namespace suspend
// semantics; [D12]). The ANA move to
// `inaccessible` that must precede it has already happened in the sweep's P1
// pre-step (CN9), which runs before anything else in the pass and on every
// converge, one whose sweep an unanswered listing stopped included (CN21).
//
// The hold is the one case in which it does not converge: while a dm-clone
// the plan does not want may still be live (cloneMayLinger, CN18) — the
// sweep's L3 did not run, or it left one — an ns-dev goes onto its td's
// raid0 only if it is on it already. That dm-clone goes on hydrating into the
// raid0 until L3 removes it, and a region it has not copied yet would
// overwrite a write a host made to the raid0 directly. An existing ns-dev
// keeps its live table — one over the dm-clone serves on through it
// (nsDevHeldServing), a parked one stays parked (nsDevHeldParked) — and a
// new one starts parked on the td's dm-error. The caller reports a held
// ns-dev's row ERROR, registers the CN10 retry, keeps a serving namespace in
// the ANA group it has and moves a parked one inaccessible; the first pass
// that leaves no such dm-clone live — its listing names none, or its L3
// removed it, parking first an ns-dev still over it — reloads it.
func (s *CnAgentServer) ensureNsDev(
	ctx context.Context,
	np *nsPlan,
	cloneMayLinger bool,
) (held nsDevHold, err error) {
	if np.backingName == "" {
		return nsDevConverged, fmt.Errorf("namespace has no thin device")
	}
	if np.sectors == 0 {
		return nsDevConverged, fmt.Errorf("namespace size is 0")
	}
	// A rule-5 ns-dev gets the park's table while its dm-clone does not show
	// hydration on, with no ANA move: its namespace stays optimized, as it
	// does through a recovery's own park (CN18 step 2).
	np, err = s.nsDevNow(ctx, np)
	if err != nil {
		return nsDevConverged, err
	}
	hold := cloneMayLinger && np.td != nil &&
		np.backingName == np.td.raid0Name
	// The td's dm-error is ensured again here, probe-first: the build's td
	// loop ensures it earlier in the pass (CN15), and this retries it on a
	// pass in which that step failed.
	if np.td != nil && np.backingName == np.td.errorName {
		if err := s.ensureDmError(
			ctx, np.td.errorName, np.td.sectors); err != nil {
			return nsDevConverged, err
		}
	}
	devNo, err := s.dm.DevNo(ctx, s.nf.DmPath(np.backingName))
	if err != nil {
		return nsDevConverged, err
	}
	table := nsDevTable(np, devNo)
	dev, err := s.dm.Info(ctx, np.devName)
	if err != nil {
		return nsDevConverged, err
	}
	if dev == nil {
		if hold {
			// Created parked, on the td's dm-error.
			if err := s.ensureDmError(
				ctx, np.td.errorName, np.td.sectors); err != nil {
				return nsDevConverged, err
			}
			errNo, err := s.dm.DevNo(ctx, s.nf.DmPath(np.td.errorName))
			if err != nil {
				return nsDevConverged, err
			}
			held = nsDevHeldParked
			table = agent.LinearTable(np.sectors, errNo, 0)
		}
		if err := s.dm.Create(ctx, np.devName, table); err != nil {
			return held, err
		}
		dev = &agent.DmDevInfo{}
	} else {
		targets, tableErr := s.dm.Table(ctx, np.devName)
		if tableErr != nil {
			return nsDevConverged, tableErr
		}
		if !nsDevTableMatches(targets, np, devNo) || dev.ReadOnly {
			if hold && !onDevice(targets, devNo) {
				held = s.heldNsDev(ctx, np, targets)
			} else {
				// Reload is suspend, load, resume: it leaves the device
				// live, or fails and returns here before the guard below —
				// a failed load leaving the device suspended on purpose
				// (Dm.Reload fails closed) — so the guard needs no case
				// for this branch.
				if err := s.dm.Reload(ctx, np.devName, table); err != nil {
					return nsDevConverged, err
				}
				dev = &agent.DmDevInfo{}
			}
		}
	}
	// Nothing here suspends the ns-dev but the Reload above, and it leaves the
	// device suspended only when one of its commands fails. A device found
	// suspended and not reloaded — its table already the one this pass wants,
	// or held — is one an **older build** deliberately held suspended for
	// a namespace suspend (architecture.md, Namespace suspend semantics), or
	// one a reload that was interrupted or failed left behind;
	// either way it is resumed, which is the whole of the upgrade path.
	if dev.Suspended {
		return held, s.dm.Resume(ctx, np.devName)
	}
	return held, nil
}

// nsDevHold is what ensureNsDev did with an ns-dev it held off its td's
// raid0, which decides the namespace's ANA move (build).
type nsDevHold int

const (
	// nsDevConverged: not held — the ns-dev is on the backing its plan
	// wants, or the converge failed and its error says why.
	nsDevConverged nsDevHold = iota
	// nsDevHeldServing: held on a live table that maps data — the dm-clone
	// the plan dropped, typically — so its namespace keeps its ANA group.
	nsDevHeldServing
	// nsDevHeldParked: held on, or created on, a table that maps none — the
	// td's dm-error — so its namespace goes inaccessible: a host queues its
	// IO rather than take errors on an optimized path.
	nsDevHeldParked
)

// heldNsDev tells a held ns-dev that is parked from one that serves, by its
// live table. A dm-error it cannot resolve counts as parked: the namespace
// then goes inaccessible for the pass, which costs its hosts a wait and
// never an IO error.
func (s *CnAgentServer) heldNsDev(
	ctx context.Context,
	np *nsPlan,
	targets []agent.DmTarget,
) nsDevHold {
	if len(targets) == 1 && targets[0].Type == "error" {
		return nsDevHeldParked
	}
	errNo, err := s.dm.DevNo(ctx, s.nf.DmPath(np.td.errorName))
	if err != nil || onDevice(targets, errNo) {
		return nsDevHeldParked
	}
	return nsDevHeldServing
}

// onDevice reports whether a live ns-dev table is a single target over one
// device: a dm-linear, or the [D11] dm-flakey wrapper, whose first argument
// is that device either way.
func onDevice(targets []agent.DmTarget, devNo string) bool {
	return len(targets) == 1 && len(targets[0].Args) > 0 &&
		targets[0].Args[0] == devNo
}

// parkNsDev reloads one ns-dev onto its td's dm-error. It is CN9's
// pre-step 2, the park of a planned ns-dev, which performs old_primary step 3
// of architecture.md, Failover, and it is the park of a clone recovery
// (CN18 step 2); the park of an unwanted ns-dev is CN21's P0, parkByTable.
// Where a removal follows, the ns-dev must stop mapping whatever is about to
// be removed under it, and the reload's own flushing suspend is what
// completes the in-flight IO on the old table. The reload also resumes a
// device an **older build** left deliberately suspended (or a reload that
// was interrupted or failed left behind) — when its own commands succeed:
// one whose load fails leaves the device suspended (Dm.Reload fails closed).
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

// subsysConf is one host-facing subsystem's nvmet conf. Its allowed hosts are
// the record's list verbatim, and an empty list admits no host
// (architecture.md, Primary cntlr, step 6).
func (s *CnAgentServer) subsysConf(
	plan *cntlrPlan,
	sp *ssPlan,
) agent.SubsysConf {
	cntlidMin, cntlidMax := plan.cntlidRange()
	return agent.SubsysConf{
		Nqn:          sp.nqn,
		Serial:       sp.ss.GetSerial(),
		Model:        sp.ss.GetModel(),
		CntlidMin:    cntlidMin,
		CntlidMax:    cntlidMax,
		AllowedHosts: sp.ss.GetAllowedHosts(),
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
// ANA write in here would both break the sweep-then-build ordering (CN9) and
// make an idempotent re-apply write `ana_grpid` twice.
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
// port link. A namespace that left `ns_list` while the subsystem stays is not
// its business: CN21's L1 removes it, inaccessible first, and only from an
// enumeration that answered — and nothing does under a subsystem whose NQN
// carries the dnv prefix but decodes to nothing, which the sweep never
// attributes (cnagent.md CN16).
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
	for _, np := range sp.namespaces {
		nsKey := resKeyOf(resKeyNamespaceFmt, np.nsId)
		err := s.ensureNamespaceObject(ctx, np)
		// The namespace object of a provisioning-deferred backing chain is
		// created and correct, but it is inaccessible and no host can do IO
		// through it — PROVISIONING, not OK ([D15]). The subsystem row above is
		// untouched: the subsystem itself is fully converged.
		info.NsIdToNamespace[np.nsId] = deferredFromErr(
			st.tracker, np.deferred,
			nsKey, nsResName(sp.nqn, np.nsIdx), "", err)
	}
	err := s.nvmet.EnsurePortLink(ctx, s.port.PortId, sp.nqn)
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
	// The backing the converge installs, read the same way (nsDevNow): a
	// rule-5 ns-dev parked because its dm-clone does not show hydration on is
	// where CN16 wants it, and reports as the converge did.
	np, err = s.nsDevNow(ctx, np)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
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
		return pb.ResStatus_RES_STATUS_ERROR, detailsNsDevNotDesired
	}
	// No ns-dev is ever expected dm-suspended, whatever the plan says: an
	// effectively suspended one is *parked* — live, on the td's dm-error,
	// which the table check above has already confirmed (CN28;
	// architecture.md, Namespace suspend semantics).
	if dev.Suspended {
		return pb.ResStatus_RES_STATUS_ERROR, "unexpectedly suspended"
	}
	if np.suspended {
		return pb.ResStatus_RES_STATUS_OK, detailsParked
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
	linked, err := s.nvmet.PortLinked(ctx, s.port.PortId, conf.Nqn)
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
