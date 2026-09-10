package cnagent

import (
	"context"
	"fmt"
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

// available is the §11.1.1 test: a leg is available iff it has an optimized
// path. A `non-optimized` path means the side currently exports dm-error and
// cannot be used; a path that is merely `connecting` keeps its last-known ANA
// state, which is why the controller state gates it.
//
// This is deliberately *not* update_05.md U2's rule: this is the primary's
// assembly-time gate, "is this path usable now", which `non-optimized`
// rightly fails, while the standby's transportHealth row answers "could this
// path serve a promote", which `non-optimized` rightly passes.
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
	if address, err := s.readSysfs(ctx, ctrlDir+"/address"); err == nil {
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
// explicitly (update_02.md U2): the /sys/class/nvme* tree can stall while a
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
// whether any connect failed — which is what registers the cntlr for the
// background retry.
func (s *CnAgentServer) ensureLegs(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	info *pb.CntlrInfo,
) (map[uint64]bool, bool) {
	available := make(map[uint64]bool, len(plan.legs))
	retryNeeded := false
	for _, lp := range plan.legs {
		if lp.provisioning {
			// U4: every side is still zeroing, so the DN exports nothing —
			// there is no subsystem to connect to, no multipath namespace to
			// wrap and no device to probe. Deliberate, healthy, no action.
			// available is set explicitly rather than left at its zero value,
			// so the §11.1.1 case-1 guard refuses `--assume-clean` by rule.
			info.LegIdToLeg[lp.legId] = st.tracker.Provisioning(
				resKeyOf(resKeyLegFmt, lp.legId), lp.name, detailsProvisioning)
			available[lp.legId] = false
			continue
		}
		view, err := s.ensureLeg(ctx, plan, lp)
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
func (s *CnAgentServer) ensureLeg(
	ctx context.Context,
	plan *cntlrPlan,
	lp *legPlan,
) (*subsysView, error) {
	view, err := s.readSubsys(ctx, lp.nqn, sideNsid)
	if err != nil {
		return nil, err
	}
	connected := false
	for _, side := range lp.sides {
		if !side.GetProvisioned() {
			// U4: this side is still zeroing, so the DN has not built its
			// per-CN export stack and a connect could only fail. The
			// provisioned sides of the same leg connect normally — that is
			// what keeps a migrating leg serving through its src side while
			// its dst side provisions.
			continue
		}
		tr := side.GetNvmeTrConf()
		if view.ctrlOf(tr.GetTrAddr(), tr.GetTrSvcId()) != nil {
			continue
		}
		if err := s.host.Connect(ctx, agent.TrConf{
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
		if view, err = s.readSubsys(ctx, lp.nqn, sideNsid); err != nil {
			return nil, err
		}
	}
	s.disconnectDeadPaths(ctx, lp, view)
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

// disconnectDeadPaths retires a controller of the leg NQN whose transport
// matches no desired side — the src side after FinishMigration, whose
// controller died with DNR and will never reconnect. It is dropped by
// **device**, never by NQN: the surviving side shares that NQN ([D1], §2.3).
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
		// An unprovisioned side stays "wanted" (U4): it was never connected,
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
		if wanted || ctrl.name == "" {
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
// that could serve a promote (update_05.md U2). The healthy set is
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
			// U4: nothing is exported for this side yet, so its missing
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
			// U2: a single-sided leg must hold a promotable path.
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

// removeLeg tears one leg down: the connection first, then the wrapper
// (update_01.md U2 spec 4, CN21). A probe wedged on a pathless leg holds an
// open fd on the wrapper, so `dmsetup remove` before the disconnect fails
// EBUSY; deleting the controllers errors the queued IO, the fd closes, and the
// removal then succeeds. A whole-NQN disconnect is fine here — every path of
// the leg is being retired.
func (s *CnAgentServer) removeLeg(ctx context.Context, lp *legPlan) {
	s.disconnect(ctx, lp.nqn)
	s.removeDm(ctx, lp.name)
}

// disconnect drops an nvme host connection, probing first so tearing down
// something that never connected stays silent.
func (s *CnAgentServer) disconnect(ctx context.Context, nqn string) {
	view, err := s.readSubsys(ctx, nqn, 0)
	if err != nil {
		slog.ErrorContext(ctx, "probing nvme subsystems failed",
			slog.String("nqn", nqn),
			slog.String("error", err.Error()))
		return
	}
	if !view.found {
		return
	}
	if err := s.host.Disconnect(ctx, nqn); err != nil {
		slog.ErrorContext(ctx, "nvme disconnect failed",
			slog.String("nqn", nqn),
			slog.String("error", err.Error()))
	}
}

// ---------------------------------------------------------------------------
// CN10/CN18 background connect retry (the DN13 pattern)
// ---------------------------------------------------------------------------

// startConnectRetry registers a cntlr whose outbound connect failed — a leg
// side or a clone source. A goroutine re-runs the whole converge every
// CnConnectRetryInterval seconds under the CN1 locks until it succeeds or the
// cntlr is torn down; the RPC itself never blocks on a connect.
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
		s.reconvergeCntlr(ctx, key, st)
		if ctx.Err() != nil {
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
