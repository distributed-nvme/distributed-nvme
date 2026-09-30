package cnagent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// sysfs is where the kernel publishes one directory per nvme subsystem the
// host holds. The cn agent reads leg state from there rather than from
// `nvme list-subsys`, for two measured reasons (CN10, CN12):
//
//   - `nvme list-subsys -o json` emits **no** `ANAState` unless it is given a
//     namespace block device, so the §11.1.1 availability test cannot be
//     evaluated from it; and with a device argument it answers an
//     all-inaccessible namespace with an *empty* subsystem list, which is
//     indistinguishable from "not connected".
//   - sysfs exposes the controller `state` next to the path `ana_state`,
//     which the test needs together: a dead path keeps its last-known ANA
//     state, so "available" is `state == live && ana_state == optimized`,
//     never ANA alone.
const (
	sysfsNvmeSubsysDir = "/sys/class/nvme-subsystem"
	sysfsNvmeCtrlDir   = "/sys/class/nvme"
)

var (
	// Inside a subsystem directory, `nvme0` is a controller, `nvme0n1` the
	// multipath namespace; inside a controller directory, `nvme0c1n1` is that
	// controller's own (hidden) path device, the only place ana_state lives.
	nvmeCtrlEntryPattern = regexp.MustCompile(`^nvme\d+$`)
	nvmeNsEntryPattern   = regexp.MustCompile(`^nvme\d+n\d+$`)
	nvmePathEntryPattern = regexp.MustCompile(`^nvme\d+c\d+n\d+$`)
)

// ---------------------------------------------------------------------------
// The sysfs view of one leg's subsystem
// ---------------------------------------------------------------------------

// subsysView is what the agent needs to know about one connected subsystem.
type subsysView struct {
	found bool
	// nsDev is the multipath namespace device, e.g. "/dev/nvme0n1".
	nsDev string
	ctrls []ctrlView
}

// ctrlView is one controller (one side's path) of a subsystem.
type ctrlView struct {
	// name is the controller device, e.g. "nvme0" — what
	// `nvme disconnect --device` takes.
	name     string
	trAddr   string
	trSvcId  string
	state    string // live | connecting | resetting | deleting | dead
	anaState string // optimized | non-optimized | inaccessible
	// addrUnknown is the error of an `address` read that did not answer —
	// any failure but ENOENT — and nil otherwise. trAddr/trSvcId are then
	// empty and match no side, yet the controller may be a desired side's
	// live path as easily as a dead one: it is unknown, never unwanted
	// (CN10).
	addrUnknown error
	// gone is an absent `address`: a fabrics controller has one for as long
	// as its device exists, and its subsystem keeps listing it after the
	// device was deleted, until the last reference to it drops. A gone
	// controller is no side's path: it matches no side — a side it alone
	// served reads unconnected and is connected again — and it is neither
	// unknown nor disconnected (CN10).
	gone bool
}

func (v *subsysView) ctrlOf(trAddr, trSvcId string) *ctrlView {
	if v == nil {
		return nil
	}
	for i := range v.ctrls {
		if v.ctrls[i].trAddr == trAddr && v.ctrls[i].trSvcId == trSvcId {
			return &v.ctrls[i]
		}
	}
	return nil
}

// unknownCtrl names the first controller whose address read did not answer,
// and the read; nil when every one answered (CN10). A gone controller
// answered: it is not unknown.
func (v *subsysView) unknownCtrl() error {
	if v == nil {
		return nil
	}
	for _, ctrl := range v.ctrls {
		if ctrl.addrUnknown != nil {
			return fmt.Errorf("controller %s is unknown: %w",
				ctrl.name, ctrl.addrUnknown)
		}
	}
	return nil
}

// available is the path half of CN12's availability (§11.1.1): a path that is
// both live and optimized. ensureLegs counts a leg available only when its
// CN10 converge succeeded too. A `non-optimized` path means the side
// currently exports dm-error and cannot be used; a path that is merely
// `connecting` keeps its last-known ANA state, which is why the controller
// state gates it.
//
// This is deliberately *not* transportHealth's CN11 rule: this is the
// primary's assembly-time gate, "is this path usable now", which
// `non-optimized` rightly fails, while the standby's transportHealth row
// answers "could this path serve a promote", which `non-optimized` rightly
// passes.
func (v *subsysView) available() bool {
	if v == nil || !v.found {
		return false
	}
	for _, ctrl := range v.ctrls {
		if ctrl.state == "live" && ctrl.anaState == "optimized" {
			return true
		}
	}
	return false
}

// readSubsys walks sysfs for one subsystem NQN. An absent subsystem is "not
// connected", never an error.
func (s *CnAgentServer) readSubsys(
	ctx context.Context,
	nqn string,
	nsIdx uint32,
) (*subsysView, error) {
	entries, err := s.listDir(ctx, sysfsNvmeSubsysDir)
	if err != nil {
		// No nvme subsystem at all yet.
		return &subsysView{}, nil
	}
	for _, entry := range entries {
		dir := sysfsNvmeSubsysDir + "/" + entry
		data, readErr := s.readSysfs(ctx, dir+"/subsysnqn")
		if readErr != nil || strings.TrimSpace(data) != nqn {
			continue
		}
		view := &subsysView{found: true}
		inner, listErr := s.listDir(ctx, dir)
		if listErr != nil {
			return nil, fmt.Errorf("listing %s: %w", dir, listErr)
		}
		for _, name := range inner {
			switch {
			case nvmeNsEntryPattern.MatchString(name):
				if view.nsDev != "" {
					break
				}
				if s.nsidMatches(ctx, dir+"/"+name, nsIdx) {
					view.nsDev = "/dev/" + name
				}
			case nvmeCtrlEntryPattern.MatchString(name):
				view.ctrls = append(view.ctrls, s.readCtrl(ctx, name))
			}
		}
		return view, nil
	}
	return &subsysView{}, nil
}

func (s *CnAgentServer) nsidMatches(
	ctx context.Context,
	nsDir string,
	nsIdx uint32,
) bool {
	if nsIdx == 0 {
		return true
	}
	nsid, err := s.readSysfs(ctx, nsDir+"/nsid")
	if err != nil {
		return false
	}
	return strings.TrimSpace(nsid) == strconv.FormatUint(uint64(nsIdx), 10)
}

// readCtrl reads one controller's transport, liveness and — from its own
// hidden path device, the only place it exists — the ANA state.
func (s *CnAgentServer) readCtrl(
	ctx context.Context,
	name string,
) ctrlView {
	ctrlDir := sysfsNvmeCtrlDir + "/" + name
	ctrl := ctrlView{name: name}
	address, err := s.readSysfs(ctx, ctrlDir+"/address")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// CN10: the device is deleted and nothing else of it is left to
		// read; only the subsystem's link to it is.
		ctrl.gone = true
		return ctrl
	case err != nil:
		// CN10: the failure is recorded, not left as an empty address,
		// which matches no side and so reads as a dead path to retire.
		ctrl.addrUnknown = fmt.Errorf("read %s/address: %w", ctrlDir, err)
	default:
		ctrl.trAddr, ctrl.trSvcId = agent.ParseNvmeAddress(
			strings.TrimSpace(address))
	}
	if state, err := s.readSysfs(ctx, ctrlDir+"/state"); err == nil {
		ctrl.state = strings.TrimSpace(state)
	}
	entries, err := s.listDir(ctx, ctrlDir)
	if err != nil {
		return ctrl
	}
	for _, entry := range entries {
		if !nvmePathEntryPattern.MatchString(entry) {
			continue
		}
		if ana, readErr := s.readSysfs(
			ctx, ctrlDir+"/"+entry+"/ana_state"); readErr == nil {
			ctrl.anaState = strings.TrimSpace(ana)
			break
		}
	}
	return ctrl
}

// listDir lists a directory; a failure means "absent".
func (s *CnAgentServer) listDir(
	ctx context.Context,
	path string,
) ([]string, error) {
	stdout, _, _, err := s.cmd.Run(ctx, "ls", "-1", path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(stdout, "\n") {
		if entry := strings.TrimSpace(line); entry != "" {
			out = append(out, entry)
		}
	}
	return out, nil
}

// readSysfs reads one sysfs attribute under the §7 soft timeout (SH15). This
// walk calls the OsClient directly rather than through an `agent` OS wrapper,
// so the bound every other OS touch gets for free has to be applied here
// explicitly (SH15): the /sys/class/nvme* tree can stall while a
// controller is mid-reset or being torn down, which is exactly when the walk
// runs. listDir above is already bounded, through cmd.Run.
func (s *CnAgentServer) readSysfs(
	ctx context.Context,
	path string,
) (string, error) {
	cctx, cancel := agent.CmdCtx(ctx)
	defer cancel()
	return s.oc.ReadFile(cctx, path)
}

// ---------------------------------------------------------------------------
// CN10 — leg side connections and the leg wrapper
// ---------------------------------------------------------------------------

// ensureLegs converges every leg of every group of every slice — spare legs
// included, both roles. It reports which legs are **available** (§11.1.1),
// which is what CN12 assembly needs, records each leg's ResInfo, and says
// whether any leg failed to converge — its connect, a controller's address
// read that did not answer, its multipath namespace or its wrapper — which
// registers the cntlr for the background retry (build registers it too for a
// group's leg_list member that is not available, CN12). budget is the pass's
// one CN10 wait budget, shared by every leg and by the clone sources after
// them.
func (s *CnAgentServer) ensureLegs(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	info *pb.CntlrInfo,
	budget *agent.WaitBudget,
) (map[uint64]bool, bool) {
	available := make(map[uint64]bool, len(plan.legs))
	retryNeeded := false
	for _, lp := range plan.legs {
		if lp.provisioning {
			// [D15]: every side is still zeroing, so the DN exports nothing —
			// there is no subsystem to connect to, no multipath namespace to
			// wrap and no device to probe. Deliberate, healthy, no action.
			// available is set explicitly, but the map's zero value reads the
			// same: nothing consults this entry — a group with a provisioning
			// leg_list leg is deferred, and a spare's flag is never read.
			info.LegIdToLeg[lp.legId] = st.tracker.Provisioning(
				resKeyOf(resKeyLegFmt, lp.legId), lp.name, detailsProvisioning)
			available[lp.legId] = false
			continue
		}
		view, err := s.ensureLeg(ctx, plan, lp, budget)
		if err != nil {
			info.LegIdToLeg[lp.legId] = st.tracker.Err(
				resKeyOf(resKeyLegFmt, lp.legId), lp.name, err.Error())
			s.startConnectRetry(st, plan)
			retryNeeded = true
			continue
		}
		available[lp.legId] = view.available()
		if plan.primary {
			s.startLegProber(st, plan, lp)
		}
		info.LegIdToLeg[lp.legId] = s.legInfo(ctx, st, plan, lp, view)
	}
	return available, retryNeeded
}

// ensureLeg connects every side of one leg and builds its wrapper. All sides
// share one subsystem NQN, so the kernel merges them into a single multipath
// namespace ([D1]); the wrapper is a whole-device dm-linear over it, sized
// from the desired state and never from probing the device.
//
// The connect step waits, briefly and boundedly, for what it has just asked
// for (CN10): a failed connect is tried again within the pass
// (connectWithin), and after a connect this pass made the subsystem is
// re-read until the kernel has added the multipath namespace head
// (awaitNsHead). Both draw on budget, the pass's one wait budget; once it is
// spent the leg fails as it always did, with the same error, and ensureLegs
// registers the background retry.
//
// A leg whose subsystem a sweep is still disconnecting fails before any of
// that (disconnectInFlight): its controllers are being deleted, so there is
// nothing to adopt, and a controller connected beside them could go with
// them, since the disconnect is by NQN.
func (s *CnAgentServer) ensureLeg(
	ctx context.Context,
	plan *cntlrPlan,
	lp *legPlan,
	budget *agent.WaitBudget,
) (*subsysView, error) {
	if s.disconnectInFlight(lp.nqn) {
		return nil, fmt.Errorf("%s", detailsDisconnectInFlight(lp.nqn))
	}
	view, err := s.readSubsys(ctx, lp.nqn, sideNsid)
	if err != nil {
		return nil, err
	}
	// CN10: a controller whose address read did not answer may be any
	// side's path, so no side is connected beside it; the leg fails below.
	unknown := view.unknownCtrl()
	connected := false
	for _, side := range lp.sides {
		if !side.GetProvisioned() {
			// [D15]: this side is still zeroing, so the DN has not built its
			// per-CN export stack and a connect could only fail. The
			// provisioned sides of the same leg connect normally — that is
			// what keeps a migrating leg serving through its src side while
			// its dst side provisions.
			continue
		}
		tr := side.GetNvmeTrConf()
		if unknown != nil ||
			view.ctrlOf(tr.GetTrAddr(), tr.GetTrSvcId()) != nil {
			continue
		}
		if err := s.connectWithin(ctx, budget, agent.TrConf{
			TrType:  tr.GetTrType(),
			AdrFam:  tr.GetAdrFam(),
			TrAddr:  tr.GetTrAddr(),
			TrSvcId: tr.GetTrSvcId(),
		}, lp.nqn, plan.hostNqn()); err != nil {
			return nil, err
		}
		connected = true
	}
	if connected {
		if view, err = s.awaitNsHead(
			ctx, budget, lp.nqn, sideNsid); err != nil {
			return nil, err
		}
	}
	s.disconnectDeadPaths(ctx, lp, view)
	// CN10: unknown, never unwanted — disconnectDeadPaths passed over it, and
	// the leg fails for the pass, which registers the retry that reads it
	// again. After a connect this pass made, this is the re-read view.
	if err := view.unknownCtrl(); err != nil {
		return nil, err
	}
	if view.nsDev == "" {
		return nil, fmt.Errorf("no multipath namespace for %s", lp.nqn)
	}
	if err := s.ensureDmLinear(
		ctx, lp.name, lp.sectors, view.nsDev, 0); err != nil {
		return nil, err
	}
	return view, nil
}

// sideNsid is the single namespace id every side of a leg exports (§3.1).
const sideNsid = 1

// connectWithin is the CN10 connect of one side of a leg, and the CN18
// connect of one clone-source path: `nvme connect`, and again after a
// CnConnectRetryPause whenever it failed, for as long as the pass's budget
// covers the pause. Every failed attempt is charged its own elapsed time, so
// a connect to a disk node whose VM is down — about 3 s, the kernel's SYN
// retries or CmdSoftTimeout — spends the whole budget at once and is never
// retried in the pass.
//
// The error is never classified. The same failure is what a disk node that
// has not linked the export into its port yet answers — refused at TCP by a
// port that does not listen yet, rejected by one that does ("failed to write
// to nvme-fabrics device") — for as long as its SyncupSide takes after the
// worker's unordered fan-out ([D16]), and what a permanently rejected export
// or a dead disk node answers; only the first goes away within a pass, and
// the budget is what caps how often the other two are retried. The last
// attempt's error is the one returned, so a leg that stays unconnected reads
// exactly as it did before the retry existed.
func (s *CnAgentServer) connectWithin(
	ctx context.Context,
	budget *agent.WaitBudget,
	tr agent.TrConf,
	nqn string,
	hostNqn string,
) error {
	for {
		start := budget.Now()
		err := s.host.Connect(ctx, tr, nqn, hostNqn)
		if err == nil {
			return nil
		}
		budget.Charge(budget.Now().Sub(start))
		if !budget.Pause(ctx, common.CnConnectRetryPause) {
			return err
		}
	}
}

// awaitNsHead re-reads one subsystem after a connect this pass made, until
// its multipath namespace head is there or the pass's budget no longer
// covers another CnNsScanPause (CN10). The kernel returns from `nvme connect`
// as soon as the controller is live and only QUEUES the namespace scan, which
// adds the hidden per-path disk first and the head after it; a single re-read
// right after the connect can therefore miss a head that is milliseconds
// away. Only a connect made in this pass is waited for: a subsystem that was
// already connected and has no head — a leg whose only path is ANA
// inaccessible never gets one — is judged on the one read, as before.
func (s *CnAgentServer) awaitNsHead(
	ctx context.Context,
	budget *agent.WaitBudget,
	nqn string,
	nsIdx uint32,
) (*subsysView, error) {
	for {
		view, err := s.readSubsys(ctx, nqn, nsIdx)
		if err != nil || view.nsDev != "" {
			return view, err
		}
		if !budget.Pause(ctx, common.CnNsScanPause) {
			return view, nil
		}
	}
}

// newPassBudget is one converge pass's CN10 wait budget: created once per
// convergeCntlr and handed down to every leg and clone-source connect of that
// pass, never kept across passes or shared between cntlrs.
func (s *CnAgentServer) newPassBudget() *agent.WaitBudget {
	return agent.NewWaitBudget(common.CnConnectPassBudget, s.now, s.sleep)
}

// disconnectDeadPaths retires a controller of the leg NQN whose transport
// matches no desired side — the src side after FinishMigration, whose
// controller died with DNR and will never reconnect. It is dropped by
// **device**, never by NQN: the surviving side shares that NQN ([D1], §2.3).
// A controller whose address read did not answer is passed over: its empty
// transport matches no side, but it is unknown, never unwanted (CN10). A gone
// one is passed over too: its device is already deleted.
// `nvme disconnect --device` is not idempotent — a second call reports "Did
// not find device" — so a failure here is logged, never fatal.
func (s *CnAgentServer) disconnectDeadPaths(
	ctx context.Context,
	lp *legPlan,
	view *subsysView,
) {
	if view == nil || !view.found {
		return
	}
	for _, ctrl := range view.ctrls {
		wanted := false
		// An unprovisioned side stays "wanted" ([D15]): it was never connected,
		// so it cannot own a controller here — and were one to exist it would
		// be this leg's, never a dead path to retire.
		for _, side := range lp.sides {
			tr := side.GetNvmeTrConf()
			if ctrl.trAddr == tr.GetTrAddr() &&
				ctrl.trSvcId == tr.GetTrSvcId() {
				wanted = true
				break
			}
		}
		if wanted || ctrl.name == "" || ctrl.gone || ctrl.addrUnknown != nil {
			continue
		}
		if err := s.host.DisconnectDevice(ctx, ctrl.name); err != nil {
			slog.ErrorContext(ctx, "disconnecting a dead leg path failed",
				slog.String("nqn", lp.nqn),
				slog.String("device", ctrl.name),
				slog.String("error", err.Error()))
		}
	}
}

// nsDeviceOf finds a subsystem's namespace device — the clone source of CN18
// step 1, matched by src_nqn plus nsid.
func (s *CnAgentServer) nsDeviceOf(
	ctx context.Context,
	nqn string,
	nsIdx uint32,
) (string, error) {
	view, err := s.readSubsys(ctx, nqn, nsIdx)
	if err != nil {
		return "", err
	}
	return view.nsDev, nil
}

// legInfo is the CN28 leg report, shared by the converge pass and the probe:
// on a primary the wrapper table plus the CN11 block-probe outcome, on a
// standby the wrapper table plus transport liveness and ana_state per desired
// side (§3.6 as amended — a standby's path terminates in the side's dm-error,
// so block IO through it can never succeed).
func (s *CnAgentServer) legInfo(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	lp *legPlan,
	view *subsysView,
) *pb.ResInfo {
	key := resKeyOf(resKeyLegFmt, lp.legId)
	status, details := s.probeDmTarget(ctx, lp.name, "linear", lp.sectors,
		"", 0)
	if status != pb.ResStatus_RES_STATUS_OK {
		return st.tracker.Set(key, lp.name, status, details)
	}
	if plan.primary {
		probeStatus, probeDetails := s.legProbeOutcome(st, lp)
		return st.tracker.Set(key, lp.name, probeStatus, probeDetails)
	}
	if view == nil {
		var err error
		if view, err = s.readSubsys(ctx, lp.nqn, sideNsid); err != nil {
			return st.tracker.Err(key, lp.name, err.Error())
		}
	}
	transportStatus, transportDetails := transportHealth(lp, view)
	return st.tracker.Set(key, lp.name, transportStatus, transportDetails)
}

// transportHealth is the standby's leg report (CN11): one live controller per
// desired side, and — on a leg with exactly one desired side — an ana_state
// that could serve a promote. The healthy set is
// `optimized` or `non-optimized`, not `optimized` alone: the DN grants the
// optimized group to the primary CN alone, so every standby's path sits in
// the non-optimized group over the side's dm-error backing and that *is* its
// designed steady state, while `optimized` appears on a standby in the
// post-flip pre-promote window — this function cannot know the promote phase,
// so both pass. What fails the row is an *unpromotable* path: `inaccessible`
// (the DN parked the side or handed it over) or any state the DN never sets
// (`change`, `persistent-loss`, garbage); err_epoch absorbs a transient read
// during a cutover's ana_grpid rewrites, and a persistent one correctly ages
// toward AR8 leg repair. A leg whose side_list holds two sides is
// mid-migration (CN10) and exempt: its ANA is wrong-by-design for the whole
// hydration (dst inaccessible until cutover, src after) and the phase is the
// DN's knowledge, not this CN's, so such a leg keeps the live-only check.
func transportHealth(
	lp *legPlan,
	view *subsysView,
) (pb.ResStatus, string) {
	if view == nil || !view.found {
		return pb.ResStatus_RES_STATUS_ERROR, "no controller for " + lp.nqn
	}
	var reports []string
	ok := true
	for _, side := range lp.sides {
		tr := side.GetNvmeTrConf()
		if !side.GetProvisioned() {
			// [D15]: nothing is exported for this side yet, so its missing
			// controller is the desired state and not a fault.
			reports = append(reports, fmt.Sprintf("%s:%s %s",
				tr.GetTrAddr(), tr.GetTrSvcId(), detailsProvisioning))
			continue
		}
		ctrl := view.ctrlOf(tr.GetTrAddr(), tr.GetTrSvcId())
		if ctrl == nil {
			reports = append(reports, fmt.Sprintf("%s:%s missing",
				tr.GetTrAddr(), tr.GetTrSvcId()))
			ok = false
			continue
		}
		report := fmt.Sprintf("%s:%s %s/%s",
			tr.GetTrAddr(), tr.GetTrSvcId(), ctrl.state, ctrl.anaState)
		switch {
		case ctrl.state != "live":
			ok = false
		case len(lp.sides) == 1 &&
			ctrl.anaState != agent.AnaStateOptimized &&
			ctrl.anaState != agent.AnaStateNonOptimized:
			// CN11: a single-sided leg must hold a promotable path.
			// non-optimized is the standby's designed steady state (the DN
			// grants optimized to the primary CN alone) and optimized the
			// pre-promote window, so both pass; inaccessible — or a state
			// the DN never sets — cannot serve a promote and fails the row.
			// A two-sided leg (a migration, CN10) is exempt: its ANA is
			// wrong-by-design for the hydration and the phase is the DN's
			// knowledge, not this CN's.
			report += " unpromotable"
			ok = false
		}
		reports = append(reports, report)
	}
	details := strings.Join(reports, ",")
	if !ok {
		return pb.ResStatus_RES_STATUS_ERROR, details
	}
	return pb.ResStatus_RES_STATUS_OK, details
}

// startDisconnect issues the CN21 sweep's `nvme disconnect --nqn` from a
// goroutine on rootCtx and returns at once (CN10's disconnect registry). The
// pass that calls it holds its CN1 locks, and a controller delete can outlast
// every one of them: when the target vanishes mid-delete the kernel cannot
// enter error recovery on a DELETING controller, so the controller's shutdown
// command waits out the admin timeout (60 s), and `nvme disconnect` waits
// with it in an uninterruptible write of the controller's `delete_controller`
// attribute. No SH15 signal ends that child — SIGTERM at the soft timeout and
// SIGKILL at the hard one are delivered, and it still returns only when the
// kernel does — so issued inline it held the cntlr's object lock, and with it
// every CheckCntlr round of the cntlr (CN1), for the whole minute.
//
// The registry is bookkeeping of work in progress, never memory of work that
// failed: an entry lives exactly as long as its goroutine, and it does two
// things. It keeps a later pass from issuing a second disconnect of a
// subsystem whose first one is still in the kernel, and it keeps the connect
// steps off a controller that is being deleted under them
// (disconnectInFlight). Whether the subsystem is still a leftover is never
// read from it: the caller reports one for as long as the node's own
// enumeration shows a controller, and a disconnect that failed is issued
// again by the first pass that finds the controller still there after the
// goroutine has gone.
//
// The goroutine carries the pass's trace id — the command is that pass's own,
// only not waited for — and takes none of the CN1 locks. It is not joined at
// exit (`dnagent.md` SH27): the child holds nothing a restarted agent needs,
// and a delete already in the kernel finishes whether or not anybody waits
// for it.
//
// At most disconnectConcurrency of them run at once. The rest wait for a
// slot and stay registered while they wait, so a queued disconnect keeps the
// connect steps off its subsystem and is not issued twice either. Inline, a
// pass issued its disconnects one at a time; uncapped, one L10 of many legs
// or one pool drain would set them all going together, and each delete a
// vanished target stalls holds an OsClient slot for the admin timeout. Enough
// of those would leave no slot for the node's converges and Check rounds,
// whose OS calls would then be refused at the soft timeout and report ERROR
// rows on healthy objects.
func (s *CnAgentServer) startDisconnect(ctx context.Context, nqn string) {
	s.mu.Lock()
	if _, inFlight := s.disconnecting[nqn]; inFlight {
		s.mu.Unlock()
		return
	}
	s.disconnecting[nqn] = struct{}{}
	s.mu.Unlock()
	traceId, ok := common.TraceIdFromCtx(ctx)
	if !ok {
		traceId = common.NewTraceId()
	}
	bgCtx := common.WithTraceId(s.rootCtx, traceId)
	slots := s.disconnectSlots
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.disconnecting, nqn)
			s.mu.Unlock()
		}()
		select {
		case slots <- struct{}{}:
		case <-bgCtx.Done():
			return
		}
		defer func() { <-slots }()
		if err := s.host.Disconnect(bgCtx, nqn); err != nil {
			slog.ErrorContext(bgCtx, "nvme disconnect failed",
				slog.String("nqn", nqn),
				slog.String("error", err.Error()))
		}
	}()
}

// disconnectConcurrency is how many of the sweep's background disconnects run
// at once (startDisconnect): a quarter of the node's OsClient slots, so a
// batch of deletes stuck in the kernel leaves the rest to everything else.
const disconnectConcurrency = common.DefaultOsClientLimit / 4

// detailsDisconnectInFlight is the ResInfo details of a leg or a clone source
// whose connect step the registry stopped. The converge and the CN28 probe
// report the same string, so the two channels do not flip the row against
// each other while the disconnect runs.
func detailsDisconnectInFlight(nqn string) string {
	return fmt.Sprintf("a sweep's disconnect of %s is still in flight", nqn)
}

// disconnectInFlight reports whether a sweep's background disconnect of nqn
// is still running, or waiting for a slot (CN10's disconnect registry). The
// connect steps ask it before they read the subsystem, because the pass's
// locks no longer cover the delete: the controller they would find at a
// wanted address — and adopt, since they connect only the addresses that
// have none — is about to go, taking the device a leg wrapper or a dm-clone
// would be built on with it. A step that finds one in flight fails for the
// pass, which registers the CN10/CN18 connect retry, and the first pass after
// the goroutine has gone reads the subsystem afresh and connects whatever is
// missing. The CN28 probe asks it too, so the Check round reports that step
// the way the converge does.
func (s *CnAgentServer) disconnectInFlight(nqn string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.disconnecting[nqn]
	return ok
}

// ---------------------------------------------------------------------------
// CN10/CN18 background connect retry (the DN13 pattern)
// ---------------------------------------------------------------------------

// startConnectRetry registers a cntlr for the CN10 background retry: a leg or
// a clone source failed to converge (CN10/CN18), a clone recovery's
// destination bitmaps were not applied or another later step of a clone
// failed (CN18 says which), a group's leg_list holds a member that is not
// available (CN12), or the build held an ns-dev off its td's raid0 while a
// dm-clone the plan does not want may still be live (CN18). A goroutine
// re-runs the whole converge every CnConnectRetryInterval seconds under the
// CN1 locks until a pass registers none of these (build's stopConnectRetry)
// or the cntlr is torn down. The RPC itself retries a connect, or waits for
// its namespace head, only as far as the pass's CnConnectPassBudget allows:
// an in-pass retry, or a step of that wait, starts only while the budget
// still covers its pause (connectWithin, awaitNsHead).
func (s *CnAgentServer) startConnectRetry(
	st *cntlrState,
	plan *cntlrPlan,
) {
	key := cntlrKey(plan.clusterId, plan.cnId, plan.spId, plan.cntlrId)
	s.mu.Lock()
	if st.retrying {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.rootCtx)
	st.retrying = true
	st.cancel = cancel
	interval := s.retryInterval
	s.mu.Unlock()
	go s.connectRetryLoop(ctx, key, st, interval)
}

func (s *CnAgentServer) stopConnectRetry(st *cntlrState) {
	s.mu.Lock()
	cancel := st.cancel
	st.cancel = nil
	st.retrying = false
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *CnAgentServer) connectRetryLoop(
	ctx context.Context,
	key string,
	st *cntlrState,
	interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// The attempt runs on rootCtx, never on this loop's ctx, for the
		// reason dnagent's migrRetryLoop carries in full: a converge that
		// finally connects calls stopConnectRetry, which cancels exactly
		// this loop's ctx, so an attempt running on it would cancel ITSELF
		// and abandon the rest of the pass.
		//
		// Unlike the dn's, this one was not yet producing a failure — do not
		// go looking for one. Both of this agent's stopConnectRetry sites
		// sit where nothing that matters follows them: one is the last
		// statement of convergeCntlr, the other is the SP_LEVEL_DISABLE arm,
		// whose sweep has already run and whose build has nothing to do. It
		// is the SHAPE that is wrong — correct only by statement order, one
		// edit away from the dn's stall — and the two agents must not differ
		// here, because this loop is written as "the DN13 pattern" and
		// whichever copy a reader meets first is the one they will follow.
		s.reconvergeCntlr(s.rootCtx, key, st)
		if ctx.Err() != nil || s.rootCtx.Err() != nil {
			return
		}
	}
}

// reconvergeCntlr re-runs one cntlr's converge from a background goroutine
// under the CN1 locks. Every attempt is its own traceable operation (SH2/CN2).
func (s *CnAgentServer) reconvergeCntlr(
	ctx context.Context,
	key string,
	st *cntlrState,
) {
	if s.getCntlr(key) != st {
		return
	}
	attemptCtx := common.WithTraceId(ctx, common.NewTraceId())
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	if s.getCntlr(key) != st {
		return
	}
	s.convergeCntlr(attemptCtx, st)
}
