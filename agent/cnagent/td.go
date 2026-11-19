package cnagent

import (
	"context"
	"fmt"
	"strconv"
	"strings"

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
		devNo, err := s.dmDevNo(
			ctx, plan.rd, plan.thinName(tp.tdId, sp.sliceId))
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
	targets []common.DmTarget,
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

// nsDevNow is np with the backing CN16 installs on this node in this pass.
// It is np itself under every rule but 5: a dm-clone may serve only once
// CN18 step 5 has turned its hydration on, which follows step 4's source
// chunks and nothing before them, and only while its metadata is read-write,
// so a rule-5 ns-dev whose dm-clone's live status does not show both — a
// build that has not finished, whatever stopped it, or a dm-clone whose
// metadata went read-only or failed — gets the td's dm-error instead: the
// table of rule 1's park, live and never under dm-flakey, while its namespace
// waits in the inaccessible group (build). A dm-clone that does not exist
// shows no hydration either, so its td's ns-devs park with their rows OK —
// whether a sweep removed it, its build has not got that far, or its clone
// meta group is not ready (CN18 step 2). Nothing is remembered: the status
// is read on every call. A read that fails or does not answer, of a dm-clone
// that is there or whose absence is not confirmed (cloneServes), is an
// error, which licenses neither the move onto the dm-clone nor a park — the
// ns-dev keeps the table it has.
func (s *CnAgentServer) nsDevNow(
	ctx context.Context,
	np *nsPlan,
) (*nsPlan, error) {
	if np.clone == nil {
		return np, nil
	}
	serves, err := s.cloneServes(ctx, np.clone)
	if err != nil {
		return nil, err
	}
	if serves {
		return np, nil
	}
	parked := *np
	parked.backingName = np.td.errorName
	parked.flakey = false
	return &parked, nil
}

// wantsServing reports whether the CN16 rules want np's ns-dev on a serving
// device — rule 5's dm-clone or rule 6's raid0, under rule 7's dm-flakey at
// the read-only level — rather than on the td's dm-error, which rules 0-4
// select.
func (np *nsPlan) wantsServing() bool {
	return np.td != nil && np.backingName != "" &&
		np.backingName != np.td.errorName
}

// nsAnaWanted is the ANA group CN16 wants a namespace in, given whether its
// ns-dev's live table maps the td's dm-error (parked): inaccessible when the
// rules want the ns-dev on a serving device and it is parked — a host then
// queues its IO rather than reads IO errors from an optimized path — and the
// desired group, np.anaGrpId, otherwise, a table that could not be read
// included. The build's ANA move takes it for what the ns-dev step did
// (nsAnaMove), and both channels' namespace rows compare the group with it
// for the ns-dev's live table (nsRowAnaGrpId).
func nsAnaWanted(np *nsPlan, parked bool) int {
	if parked && np.wantsServing() {
		return common.AnaGrpIdInaccessible
	}
	return np.anaGrpId
}

// nsAnaMove is the group the build's last loop moves a namespace whose
// desired group is optimized to, and false where the namespace keeps the
// group it has (CN16): an ns-dev the step put on what its rules want takes
// the desired group, and one parked while its rules want a serving device
// takes inaccessible. A failed step that did not find the ns-dev parked
// keeps the group, since a failed read never moves an optimized namespace
// back: the ns-dev may still serve, and the move would pause its hosts for
// nothing. So does a failed step that found it on the dm-error its rules
// select. A hold that serves (CN18) takes the desired group: its ns-dev
// serves on from the table it keeps, and P2 may have moved its namespace to
// inaccessible before a park whose load failed, which would pause its hosts
// for nothing until the hold ends. The move goes by what the step did, so it
// counts as parked a held ns-dev whose dm-error number cannot be read
// (heldNsDev), which the namespace row, read from the live table, does not
// (nsRowAnaGrpId): that row then reads ERROR on both channels.
func nsAnaMove(np *nsPlan, outcome nsDevOutcome) (int, bool) {
	switch outcome {
	case nsDevFailed:
		return 0, false
	case nsDevFailedParked:
		if !np.wantsServing() {
			return 0, false
		}
	}
	return nsAnaWanted(np, outcome.parked()), true
}

// ensureNsDev converges one namespace's own dm-linear (architecture.md,
// Primary cntlr, step 5) onto the
// CN16 backing, and says what it did (nsDevOutcome), which decides the
// namespace's ANA move (build). An effectively suspended namespace needs no
// step of its own
// here: it is **parked**, and rule 1 of the backing state machine has already
// made its backing the td's dm-error, so the ordinary reload below installs
// the park — live, never dm-suspended (architecture.md, Namespace suspend
// semantics; [D12]). The ANA move to
// `inaccessible` that must precede it has already happened in the sweep's P1
// pre-step (CN9), which runs before anything else in the pass and on every
// converge, one whose sweep an unanswered listing stopped included (CN21).
//
// The hold is the one case in which it does not converge: while a dm-clone
// the plan does not want may still write into its td's raid0 (lingering,
// the pass's cloneHold, CN18) — the sweep's L3 did not run, or did not
// verify that dm-clone gone, or the pass cannot say which raid0s such a
// dm-clone writes into — an ns-dev goes onto that raid0 only if it is on it
// already. That dm-clone goes on hydrating into the raid0 until L3 removes
// it, and a region it has not copied yet would overwrite a write a host made
// to the raid0 directly. An existing ns-dev keeps its live table — one over
// the dm-clone serves on through it (nsDevHeldServing), a parked one stays
// parked (nsDevHeldParked) — and a new one starts parked on the td's
// dm-error. The caller reports a held ns-dev's row ERROR, which the
// converge's verdict and a read's both name (CN30), so the worker re-sends
// the request; it moves a serving namespace to its desired group and a
// parked one inaccessible (nsAnaMove). The first pass that leaves no such
// dm-clone live — its listing names none, or its L3 removed it, parking
// first an ns-dev still over it — reloads it. The ns-devs of a td whose raid0 no such
// dm-clone writes into converge as usual.
//
// A step that fails, whatever read or command failed, reads the ns-dev's
// live table afterwards (failedNsDev) — the number of a raid0 that is not
// there yet included, which is how a promotion that came before the sides'
// switch-over ([D16]) fails — so the build can tell an ns-dev parked while
// its rules want a serving device from one that may still serve.
func (s *CnAgentServer) ensureNsDev(
	ctx context.Context,
	np *nsPlan,
	lingering cloneHold,
) (nsDevOutcome, error) {
	outcome, err := s.convergeNsDev(ctx, np, lingering)
	if err != nil {
		return s.failedNsDev(ctx, np), err
	}
	return outcome, nil
}

// convergeNsDev is ensureNsDev's step. Its outcome means nothing when it
// returns an error.
func (s *CnAgentServer) convergeNsDev(
	ctx context.Context,
	np *nsPlan,
	lingering cloneHold,
) (nsDevOutcome, error) {
	if np.backingName == "" {
		return nsDevFailed, fmt.Errorf("namespace has no thin device")
	}
	if np.sectors == 0 {
		return nsDevFailed, fmt.Errorf("namespace size is 0")
	}
	// A rule-5 ns-dev gets the park's table while its dm-clone does not
	// serve, and its namespace waits in the inaccessible group.
	now, err := s.nsDevNow(ctx, np)
	if err != nil {
		return nsDevFailed, err
	}
	outcome := nsDevServing
	switch {
	case now.backingName != np.backingName:
		outcome = nsDevAwaiting
	case !np.wantsServing():
		outcome = nsDevOnError
	}
	np = now
	hold := np.td != nil && np.backingName == np.td.raid0Name &&
		lingering.holds(np.td.raid0Name)
	// The td's dm-error is ensured again here, probe-first: the build's td
	// loop ensures it earlier in the pass (CN15), and this retries it on a
	// pass in which that step failed.
	if np.td != nil && np.backingName == np.td.errorName {
		if err := s.ensureDmError(
			ctx, np.td.errorName, np.td.sectors); err != nil {
			return nsDevFailed, err
		}
	}
	devNo, err := s.dmDevNo(ctx, nil, np.backingName)
	if err != nil {
		return nsDevFailed, err
	}
	table := nsDevTable(np, devNo)
	dev, err := s.dm.Info(ctx, np.devName)
	if err != nil {
		return nsDevFailed, err
	}
	if dev == nil {
		if hold {
			// Created parked, on the td's dm-error.
			if err := s.ensureDmError(
				ctx, np.td.errorName, np.td.sectors); err != nil {
				return nsDevFailed, err
			}
			errNo, err := s.dmDevNo(ctx, nil, np.td.errorName)
			if err != nil {
				return nsDevFailed, err
			}
			outcome = nsDevHeldParked
			table = agent.LinearTable(np.sectors, errNo, 0)
		}
		if err := s.dm.Create(ctx, np.devName, table); err != nil {
			return nsDevFailed, err
		}
		dev = &agent.DmDevInfo{}
	} else {
		targets, tableErr := s.dm.Table(ctx, np.devName)
		if tableErr != nil {
			return nsDevFailed, tableErr
		}
		if !nsDevTableMatches(targets, np, devNo) || dev.ReadOnly {
			if hold && !onDevice(targets, devNo) {
				outcome = s.heldNsDev(ctx, np, targets)
			} else {
				if outcome == nsDevAwaiting {
					// Rule 5's fallback parks an ns-dev whose namespace the
					// desired state wants optimized: the namespace goes
					// inaccessible before the reload, as before every such
					// park (parkNsDev).
					s.setAnaLogged(ctx, np.ss.nqn, np.nsIdx)
				}
				// Reload is suspend, load, resume: it leaves the device
				// live, or fails and returns here before the guard below —
				// a failed load leaving the device suspended on purpose
				// (Dm.Reload fails closed) — so the guard needs no case
				// for this branch.
				if err := s.dm.Reload(ctx, np.devName, table); err != nil {
					return nsDevFailed, err
				}
				if dev.ReadOnly {
					// A reload leaves the block layer's own read-only flag
					// set (CN10, A read-only device is made writable).
					if err := s.dm.SetWritable(
						ctx, s.nf.DmPath(np.devName)); err != nil {
						return nsDevFailed, err
					}
				}
				dev = &agent.DmDevInfo{}
			}
		}
	}
	// Nothing here suspends the ns-dev but the Reload above, and it leaves the
	// device suspended only when one of its commands fails. A device found
	// suspended and not reloaded — its table already the one this pass wants,
	// or held — is one a reload that was interrupted or failed left behind,
	// or one a chunk's apply paused and could not resume
	// (agent.ApplySkipRanges), and it is resumed (CN16).
	if dev.Suspended {
		return outcome, s.dm.Resume(ctx, np.devName)
	}
	return outcome, nil
}

// failedNsDev is the outcome of an ns-dev step that failed:
// nsDevFailedParked when the ns-dev's live table, read after the failure,
// maps the td's dm-error (nsDevParked), and nsDevFailed otherwise — a table
// or a dm-error number that cannot be read included, since what is not known
// moves no namespace (nsAnaMove).
func (s *CnAgentServer) failedNsDev(
	ctx context.Context,
	np *nsPlan,
) nsDevOutcome {
	if np.td != nil && s.nsDevParked(ctx, nil, np) {
		return nsDevFailedParked
	}
	return nsDevFailed
}

// nsDevParked reports whether np's ns-dev's live table maps the td's dm-error
// (nsDevParkedTable): the table from rd, the reading of a cntlr's read, or
// with no reading from one `dmsetup table` of the ns-dev. An
// ns-dev that is not there, or a table that cannot be read, answers false.
func (s *CnAgentServer) nsDevParked(
	ctx context.Context,
	rd *cnReading,
	np *nsPlan,
) bool {
	if rd != nil {
		// A reading looks a table up only for a device its info lookup
		// found (cnReading.table). With no reading, the table read of a
		// device that is not there fails, which answers the same.
		if dev, err := rd.info(ctx, np.devName); err != nil || dev == nil {
			return false
		}
	}
	targets, err := s.dmTable(ctx, rd, np.devName)
	return err == nil && s.nsDevParkedTable(ctx, rd, np, targets)
}

// nsRowAnaGrpId is the group a namespace row compares the namespace's
// "ana_grpid" with, on both channels alike (CN28): nsAnaWanted for whether
// the ns-dev's live table maps the td's dm-error (nsDevParked) — read from a
// read's reading, or by a converge after its ANA loop. So the two
// channels decide the row by one rule from what the node holds, whatever the
// converge's ns-dev step did.
func (s *CnAgentServer) nsRowAnaGrpId(
	ctx context.Context,
	rd *cnReading,
	np *nsPlan,
) int {
	if np.anaGrpId == common.AnaGrpIdInaccessible || !np.wantsServing() {
		// The table cannot change the group nsAnaWanted answers, so it is
		// not read.
		return np.anaGrpId
	}
	return nsAnaWanted(np, s.nsDevParked(ctx, rd, np))
}

// nsDevParkedTable reports whether a live ns-dev table maps the td's
// dm-error: a single target over it, or a single error target. The
// dm-error's number comes from rd, the reading of a cntlr's read, and with
// no reading from one `dmsetup info` of it (dmDevNo); a number
// that cannot be read answers false.
func (s *CnAgentServer) nsDevParkedTable(
	ctx context.Context,
	rd *cnReading,
	np *nsPlan,
	targets []common.DmTarget,
) bool {
	if len(targets) == 1 && targets[0].Type == "error" {
		return true
	}
	if np.td == nil {
		return false
	}
	errNo, err := s.dmDevNo(ctx, rd, np.td.errorName)
	return err == nil && onDevice(targets, errNo)
}

// nsDevOutcome is what ensureNsDev did with one ns-dev, which decides its
// namespace's ANA move (nsAnaMove).
type nsDevOutcome int

const (
	// nsDevFailed: the step failed, and the ns-dev's live table is not known
	// to map the td's dm-error — it maps something else, or it could not be
	// read.
	nsDevFailed nsDevOutcome = iota
	// nsDevFailedParked: the step failed, and the ns-dev's live table maps
	// the td's dm-error (failedNsDev).
	nsDevFailedParked
	// nsDevServing: the ns-dev maps its serving device in this pass — the
	// raid0, or the dm-clone that serves, under dm-flakey at the read-only
	// level — put there or found there.
	nsDevServing
	// nsDevOnError: the rules select the td's dm-error (CN16 rules 0-4), and
	// the ns-dev maps it.
	nsDevOnError
	// nsDevAwaiting: rule 5's dm-clone does not serve, so the ns-dev maps
	// the td's dm-error instead (nsDevNow) — a clone build that has not
	// finished, or a dm-clone whose metadata is read-only or failed.
	nsDevAwaiting
	// nsDevHeldServing: held (CN18) on a live table that maps data — the
	// dm-clone the plan dropped, typically — so its namespace takes its
	// desired group (nsAnaMove).
	nsDevHeldServing
	// nsDevHeldParked: held on, or created on, a table that maps none — the
	// td's dm-error — so its namespace goes inaccessible: a host queues its
	// IO rather than take errors on an optimized path.
	nsDevHeldParked
)

// held reports whether ensureNsDev held the ns-dev off its td's raid0
// (CN18).
func (o nsDevOutcome) held() bool {
	return o == nsDevHeldServing || o == nsDevHeldParked
}

// parked reports whether the step left the ns-dev on the td's dm-error as
// far as the step knows, which decides the group the ANA move takes
// (nsAnaMove): a held ns-dev whose dm-error number cannot be read counts as
// parked (heldNsDev).
func (o nsDevOutcome) parked() bool {
	switch o {
	case nsDevFailedParked, nsDevOnError, nsDevAwaiting, nsDevHeldParked:
		return true
	}
	return false
}

// heldNsDev tells a held ns-dev that is parked from one that serves, by its
// live table. A dm-error it cannot resolve counts as parked: the namespace
// then goes inaccessible for the pass, which costs its hosts a wait and
// never an IO error.
func (s *CnAgentServer) heldNsDev(
	ctx context.Context,
	np *nsPlan,
	targets []common.DmTarget,
) nsDevOutcome {
	if len(targets) == 1 && targets[0].Type == "error" {
		return nsDevHeldParked
	}
	errNo, err := s.dmDevNo(ctx, nil, np.td.errorName)
	if err != nil || onDevice(targets, errNo) {
		return nsDevHeldParked
	}
	return nsDevHeldServing
}

// onDevice reports whether a live ns-dev table is a single target over one
// device: a dm-linear, or the [D11] dm-flakey wrapper, whose first argument
// is that device either way.
func onDevice(targets []common.DmTarget, devNo string) bool {
	return len(targets) == 1 && len(targets[0].Args) > 0 &&
		targets[0].Args[0] == devNo
}

// parkNsDev reloads one ns-dev onto its td's dm-error. It is CN9's
// pre-step 2, the park of a planned ns-dev, which performs the old primary's
// reload of architecture.md, Failover; the park of an unwanted ns-dev is
// CN21's P0, parkByTable.
// Where a removal follows, the ns-dev must stop mapping whatever is about to
// be removed under it, and the reload's own flushing suspend is what
// completes the in-flight IO on the old table. The reload also resumes a
// device a reload that was interrupted or failed left suspended — when its
// own commands succeed: one whose load fails leaves the device suspended
// (Dm.Reload fails closed).
//
// A namespace whose desired group is optimized while its rules want its
// ns-dev on a serving device — P2's park of an ns-dev mapping a device the
// pass removes — goes inaccessible before the reload (CN16), and so does one
// whose ns-dev the park finds parked already. Its hosts then queue their IO
// through the park rather than read IO errors from an optimized path; the
// build moves it back in the pass that puts the ns-dev on its serving
// device, or that finds it still serving after a park whose load failed
// (CN18's hold). A move that fails is logged and the park goes on: the park
// guards the data, the move only spares the hosts IO errors. Rules 3 and 4
// park onto the dm-error they select, with no move.
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
	devNo, err := s.dmDevNo(ctx, nil, np.td.errorName)
	if err != nil {
		return err
	}
	targets, err := s.dm.Table(ctx, np.devName)
	if err != nil {
		return err
	}
	parked := len(targets) == 1 && targets[0].Type == "linear" &&
		len(targets[0].Args) == 2 && targets[0].Args[0] == devNo
	if np.anaGrpId != common.AnaGrpIdInaccessible && np.wantsServing() {
		s.setAnaLogged(ctx, np.ss.nqn, np.nsIdx)
	}
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
// moving its ANA group: an enabled namespace keeps the group it has and the
// CN9 final pass promotes it, and one that is not enabled — a fresh one, or
// one a step left behind between its `mkdir` and its enable — gets
// `inaccessible` before EnsureNamespace enables it, never the kernel's
// default group. Folding the
// ANA write in here would both break the build's "objects first, ANA last"
// order (CN9's ANA rewrite to optimized comes last, as ensureXferNamespace
// keeps it too) and make an idempotent re-apply write `ana_grpid` twice.
func (s *CnAgentServer) ensureNamespaceObject(
	ctx context.Context,
	np *nsPlan,
) error {
	state, err := s.nvmet.ProbeNamespace(ctx, np.ss.nqn, np.nsIdx)
	if err != nil {
		return err
	}
	conf := s.nsConf(np, objectAnaGrpId(state))
	if conf.Disables(state) {
		if err := s.disableSafe(ctx, state.DevicePath); err != nil {
			return fmt.Errorf("namespace %s/%d is not disabled: %w",
				np.ss.nqn, np.nsIdx, err)
		}
	}
	return s.nvmet.EnsureNamespace(ctx, conf)
}

// disableSafe returns nil when the disable of an enabled namespace over
// devicePath may run: the disable waits, unkillable, for every request in
// flight on the namespace, and a suspended dm device never completes one,
// so a dm device that reads suspended, or whose read did not answer, makes
// it an error. A
// namespace whose id the request gave to another namespace can still sit
// over an unwanted ns-dev the sweep's park step could not prove out of
// suspension (CN21); the step fails, its row names it, and the converge
// the worker's re-send brings parks that ns-dev first. A device that is no
// dm device, or is gone, is no such wait.
func (s *CnAgentServer) disableSafe(ctx context.Context, devicePath string) error {
	name, ok := strings.CutPrefix(strings.TrimSpace(devicePath), "/dev/mapper/")
	if !ok || name == "" {
		return nil
	}
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return fmt.Errorf("its device %s did not read: %w", name, err)
	}
	if dev != nil && dev.Suspended {
		return fmt.Errorf("its device %s is suspended, and its disable "+
			"would wait for that device's IO", name)
	}
	return nil
}

// objectAnaGrpId is the group a namespace object's step writes: the one an
// enabled namespace has, and `inaccessible` for one that is absent or not
// enabled, which EnsureNamespace writes before its enable. A namespace that
// serves hosts is thus never moved by its object's step, and one that does
// not yet serve starts where a host queues.
func objectAnaGrpId(state *agent.NsState) int {
	if state.Exists && state.Enabled && state.AnaGrpId != 0 {
		return state.AnaGrpId
	}
	return common.AnaGrpIdInaccessible
}

// ensureSubsystem converges one host-facing subsystem, its namespaces and its
// port link. A namespace that left `ns_list` while the subsystem stays is not
// its business: CN21's L1 removes it, inaccessible first, and only from an
// enumeration that answered — and nothing does under a subsystem whose NQN
// decodes as a dnv kind other than a transfer of this cluster, which the
// sweep never attributes, claimed or not, while one that carries the dnv
// prefix but decodes to nothing is attributed by the held request that names
// it (nvmetOwner; cnagent.md CN16). Each namespace's step leaves its error in
// nsErrs, from which reportNamespaces writes the namespace rows after the
// build's ANA loop; a subsystem whose own step fails steps none of its
// namespaces, and they get no row. A step that failed goes to a row, and the
// converge's verdict and a read's both name it (cntlrRowVerdict; CN16, CN30),
// so the worker's re-send runs it again (dnv-worker.md RW4).
func (s *CnAgentServer) ensureSubsystem(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	sp *ssPlan,
	info *pb.CntlrInfo,
	nsErrs map[uint64]error,
) {
	ssKey := resKeyOf(resKeySubsysFmt, sp.ssId)
	if err := s.nvmet.EnsureSubsystem(
		ctx, s.subsysConf(plan, sp)); err != nil {
		info.SsIdToSubsystem[sp.ssId] = st.tracker.Err(
			ssKey, sp.nqn, err.Error())
		return
	}
	for _, np := range sp.namespaces {
		nsErrs[np.nsId] = s.ensureNamespaceObject(ctx, np)
	}
	err := s.nvmet.EnsurePortLink(ctx, s.port.PortId, sp.nqn, s.port.QidMax)
	info.SsIdToSubsystem[sp.ssId] = s.linkRow(
		ctx, ssKey, sp.nqn, s.subsysConf(plan, sp), st, err)
}

// linkRow is a subsystem's row after its port link step: OK when the step
// held, the read's "the port is not there" when the node's port itself is
// gone, which a SyncupCntlr cannot make and the cntlr's verdict leaves out
// (cntlrRowVerdict), and the step's error otherwise.
func (s *CnAgentServer) linkRow(
	ctx context.Context,
	key string,
	nqn string,
	conf agent.SubsysConf,
	st *cntlrState,
	err error,
) *pb.ResInfo {
	if err != nil {
		if _, details := s.probeExport(ctx, conf); details ==
			detailsPortMissing {
			return st.tracker.Err(key, nqn, detailsPortMissing)
		}
	}
	return st.tracker.FromErr(key, nqn, "", err)
}

// reportNamespaces writes a converge's namespace rows after its ANA loop
// (CN28), one per namespace ensureSubsystem stepped: the step's error wins,
// and otherwise the namespace is read and compared as a read compares
// it, its group with nsRowAnaGrpId's for the ns-dev's live table, so the two
// channels report the same row for the same node. The namespace
// object of a provisioning-deferred backing chain is created and correct,
// but it is inaccessible and no host can do IO through it — PROVISIONING,
// not OK ([D15]); its subsystem's row is untouched, since the subsystem
// itself is fully converged.
func (s *CnAgentServer) reportNamespaces(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	info *pb.CntlrInfo,
	nsErrs map[uint64]error,
) {
	for _, np := range plan.namespaces {
		err, stepped := nsErrs[np.nsId]
		if !stepped {
			continue
		}
		key := resKeyOf(resKeyNamespaceFmt, np.nsId)
		name := nsResName(np.ss.nqn, np.nsIdx)
		if err != nil {
			info.NsIdToNamespace[np.nsId] = deferredFromErr(
				st.tracker, np.deferred, key, name, "", err)
			continue
		}
		status, details, reason := s.probeNamespaceObject(
			ctx, s.nsConf(np, s.nsRowAnaGrpId(ctx, nil, np)))
		info.NsIdToNamespace[np.nsId] = deferredSetReason(
			st.tracker, np.deferred, key, name, status, details, reason)
	}
}

func nsResName(nqn string, nsIdx int) string {
	return fmt.Sprintf("%s/%d", nqn, nsIdx)
}

// setAna performs one ANA transition, probe-first: a namespace that is absent
// or already in the target group is left alone (SH16, [D4]). It reads the
// namespace's group alone, strictly (Nvmet.NsAnaGrpId).
func (s *CnAgentServer) setAna(
	ctx context.Context,
	nqn string,
	nsIdx int,
	grpId int,
) error {
	cur, present, err := s.nvmet.NsAnaGrpId(ctx, nqn, nsIdx)
	switch {
	case err != nil && grpId != common.AnaGrpIdInaccessible:
		return err
	case err == nil && (!present || cur == grpId):
		return nil
	}
	// A read that did not answer leaves a move to inaccessible tried all the
	// same: that is the safe direction, and skipping it would leave a host
	// sending IO to a path the pass is about to take away.
	return s.nvmet.SetNsAnaGrpId(ctx, nqn, nsIdx, grpId)
}

// ---------------------------------------------------------------------------
// Probes
// ---------------------------------------------------------------------------

func (s *CnAgentServer) probeNsDev(
	ctx context.Context,
	rd *cnReading,
	np *nsPlan,
) (pb.ResStatus, string) {
	dev, err := s.dmInfo(ctx, rd, np.devName)
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
	// rule-5 ns-dev parked because its dm-clone does not serve is where CN16
	// wants it, and reports as the converge did.
	np, err = s.nsDevNow(ctx, np)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	devNo, err := s.dmDevNo(ctx, rd, np.backingName)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	targets, err := s.dmTable(ctx, rd, np.devName)
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
	linked, portThere, err := s.nvmet.PortLinkedState(
		ctx, s.port.PortId, conf.Nqn)
	switch {
	case err != nil:
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	case !portThere:
		// The CN's verdict names the port (dnagent.md SH31), whose SyncupCn
		// makes it; a SyncupCntlr could only fail its link until then, so
		// the cntlr's verdict leaves this row out (cntlrRowVerdict).
		return pb.ResStatus_RES_STATUS_ERROR, detailsPortMissing
	case !linked:
		return pb.ResStatus_RES_STATUS_ERROR, "not linked to the port"
	}
	return pb.ResStatus_RES_STATUS_OK, ""
}

// detailsPortMissing is a subsystem's row while the node's nvmet port itself
// is not there (probeExport).
const detailsPortMissing = "the port is not there"

// probeNamespaceObject checks one nvmet namespace against its desired
// identity, backing device and ANA group, and returns its row's status,
// details and reason. A namespace whose ANA group alone differs while the
// wanted group is not optimized (agent.AnaServesAsWanted) reads
// RES_STATUS_OK with RES_REASON_DIFFERS and the difference in its details,
// which the cntlr's verdict names (CN30); one wanted optimized and not in
// that group leaves the primary without its optimized path and reads
// RES_STATUS_ERROR.
func (s *CnAgentServer) probeNamespaceObject(
	ctx context.Context,
	conf agent.NsConf,
) (pb.ResStatus, string, pb.ResReason) {
	none := pb.ResReason_RES_REASON_NONE
	state, err := s.nvmet.ProbeNamespace(ctx, conf.Nqn, conf.Nsid)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error(), none
	}
	switch {
	case !state.Exists:
		return pb.ResStatus_RES_STATUS_MISSING, "", none
	case !state.Enabled:
		return pb.ResStatus_RES_STATUS_ERROR, "namespace not enabled", none
	case state.DevicePath != conf.DevicePath:
		details := fmt.Sprintf("device_path is %q, want %q",
			state.DevicePath, conf.DevicePath)
		return pb.ResStatus_RES_STATUS_ERROR, details, none
	case conf.Uuid != "" && !equalFoldHex(state.Uuid, conf.Uuid):
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"device_uuid is %q, want %q", state.Uuid, conf.Uuid), none
	case conf.Nguid != "" && !equalFoldHex(state.Nguid, conf.Nguid):
		return pb.ResStatus_RES_STATUS_ERROR, fmt.Sprintf(
			"device_nguid is %q, want %q", state.Nguid, conf.Nguid), none
	case state.AnaGrpId != conf.AnaGrpId:
		details := fmt.Sprintf("ana_grpid is %d, want %d",
			state.AnaGrpId, conf.AnaGrpId)
		if !agent.AnaServesAsWanted(state.AnaGrpId, conf.AnaGrpId) {
			return pb.ResStatus_RES_STATUS_ERROR, details, none
		}
		return pb.ResStatus_RES_STATUS_OK, details,
			pb.ResReason_RES_REASON_DIFFERS
	}
	return pb.ResStatus_RES_STATUS_OK, "", none
}
