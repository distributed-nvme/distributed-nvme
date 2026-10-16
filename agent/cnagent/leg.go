package cnagent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The pass's NVMe walk and the leg lookup (CN10)
//
// The cn agent reads the connections of its legs and of its clone sources
// from sysfs, through the walk both agents share (agent.NvmeHost:
// WalkSubsys, ReadSubsysNqn, ReadSubsys; dnagent.md SH20), rather than from
// `nvme list-subsys`, for two measured reasons (CN10, CN12):
//
//   - `nvme list-subsys -o json` emits **no** `ANAState` unless it is given a
//     namespace block device, so the availability test of cnagent.md CN12
//     cannot be evaluated from it; and with a device argument it answers an
//     all-inaccessible namespace with an *empty* subsystem list, which is
//     indistinguishable from "not connected".
//   - sysfs exposes the controller `state` next to the path `ana_state`,
//     which the test needs together: a dead path keeps its last-known ANA
//     state, so "available" is `state == live && ana_state == optimized`,
//     never ANA alone.
//
// A cntlr's converge, check round and info read each start with one walk of
// the node: the listing of /sys/class/nvme-subsystem and each entry's
// subsysnqn say where each subsystem is (passWalk). A lookup (lookupSubsys)
// reads its own entry alone: its subsysnqn again, then its directory and its
// controllers. Only a converge walks again: a connect step that ran a
// connect marks the walk stale, so the next lookup of the pass walks again
// first; and a clone source the walk did not find is looked up once more on
// a fresh walk before it is connected (ensureCloneSource).
// ---------------------------------------------------------------------------

// subsysView is what the agent needs to know about one connected subsystem.
type subsysView struct {
	found bool
	// nsDev is the multipath namespace device, e.g. "/dev/nvme0n1".
	nsDev string
	// ngListed says the lookup's listing held nsDev's ng device too, which
	// the kernel adds last, once the head can be opened by its number
	// (agent.SubsysRead.NgListed). Only the head wait after a connect of the
	// pass judges it (awaitNsHead).
	ngListed bool
	ctrls    []ctrlView
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

// available is the path half of CN12's availability: a path that is both
// live and optimized. ensureLegs counts a leg available only when its
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

// errSubsysUnknown marks every failure of lookupSubsys (CN10), so a caller
// can tell "this pass does not know whether the NQN is connected" from its
// step's other failures, such as a refused connect. Its text is the middle
// of that failure's: "subsystem <nqn> is unknown: <the read's error>".
var errSubsysUnknown = errors.New("is unknown")

// subsysUnknown is a lookup of nqn that did not answer, naming the read.
func subsysUnknown(nqn string, err error) error {
	return fmt.Errorf("subsystem %s %w: %w", nqn, errSubsysUnknown, err)
}

// passWalk is the NVMe walk of one converge, check round or info read
// (CN10): where each subsystem entry of the node is, read at the start of
// the pass and shared by every lookup of it; a converge reads it again at
// the next lookup after it marks it stale. A check round and an info read
// take it before their rows, a converge right before its sweep, whose list
// of connections it is too (enumerateCn).
type passWalk struct {
	walk *agent.SubsysWalk
	// err is the listing of /sys/class/nvme-subsystem that did not answer:
	// every lookup of the pass is unknown.
	err error
	// stale is set before each connect of a connect step: the kernel may
	// give the subsystem that connect makes the number of an entry freed
	// since the walk, so the next lookup walks again first, whether or not
	// the connect worked.
	stale bool
}

// newPassWalk takes a pass's walk.
func (s *CnAgentServer) newPassWalk(ctx context.Context) *passWalk {
	pw := &passWalk{}
	s.rewalk(ctx, pw)
	return pw
}

// rewalk walks the node again for the pass.
func (s *CnAgentServer) rewalk(ctx context.Context, pw *passWalk) {
	pw.walk, pw.err = s.host.WalkSubsys(ctx)
	pw.stale = false
}

// lookupSubsys is the leg lookup of CN10: the view of the subsystem nqn
// names, found in the pass's walk, with the namespace node nsIdx picks (0:
// the first). An absent subsystem is "not connected", never an error, and so
// is an absent /sys/class/nvme-subsystem, which nvme-core creates when it
// loads: a host holds no fabrics controller before it has. A lookup that did
// not answer is not "not connected" (CN10): a listing of that directory that
// did not answer, or a subsysnqn read that failed where no other entry
// matched, leaves the pass not knowing whether this NQN is connected, and
// each caller acts on "not connected" — the connect steps ask for every side
// again beside the live controllers, which the host refuses, and the probe
// rows read the connection missing. So the lookup fails for the pass, naming
// the read, and its caller fails the leg or the clone source as it fails any
// other step of it, which registers the retry. So does a listing of the
// matching entry's own directory that fails, whatever the failure: its
// controllers and namespace are unread. A match is the answer even beside a
// subsysnqn read that failed: the host joins every controller of one NQN to
// the one subsystem it keeps for it ([D1]), so the entry that did not answer
// is not this NQN's.
//
// The walk can be as old as the pass, and the kernel gives a freed entry's
// number to the next new subsystem, which another cntlr or a dn agent on the
// machine may connect meanwhile. So the lookup reads the entry's subsysnqn
// again just before it reads the entry's directory and controllers. An entry
// gone since the walk, or whose subsysnqn reads another NQN, is no
// subsystem, as a subsystem gone since the listing is: also beside another
// entry whose read failed, because the walk found this NQN in this entry and
// the host keeps one subsystem per NQN. Without the check a converge could
// build a leg's wrapper over another pool's namespace. That read failing in
// any other way did not answer.
//
// A pass with no walk has no lookup: that is an unknown error, never a walk
// of its own, so a caller that lacks one shows as an ERROR row.
func (s *CnAgentServer) lookupSubsys(
	ctx context.Context,
	pw *passWalk,
	nqn string,
	nsIdx uint32,
) (*subsysView, error) {
	if pw == nil {
		return nil, fmt.Errorf("subsystem %s %w: no NVMe walk in this pass",
			nqn, errSubsysUnknown)
	}
	if pw.stale {
		s.rewalk(ctx, pw)
	}
	if pw.err != nil {
		return nil, subsysUnknown(nqn, pw.err)
	}
	match, unread := pw.walk.Find(nqn)
	if match == nil {
		if unread != nil {
			return nil, subsysUnknown(nqn, unread.Err)
		}
		return &subsysView{}, nil
	}
	got, ok, err := s.host.ReadSubsysNqn(ctx, match.Dir)
	if err != nil {
		return nil, subsysUnknown(nqn, err)
	}
	if !ok || got != nqn {
		return &subsysView{}, nil
	}
	read, err := s.host.ReadSubsys(ctx, match.Dir, agent.SubsysReadOpts{
		NsIdx: nsIdx,
		Rule:  agent.ReadTolerant,
	})
	switch {
	case err != nil:
		return nil, subsysUnknown(nqn, err)
	case read.DirErr != nil:
		return nil, subsysUnknown(nqn, read.DirErr)
	case read.DirGone:
		return nil, subsysUnknown(nqn,
			fmt.Errorf("list %s: %w", match.Dir, fs.ErrNotExist))
	}
	view := &subsysView{found: true, nsDev: read.NsDev,
		ngListed: read.NgListed}
	for _, ctrl := range read.Ctrls {
		// A gone controller has nothing but its name read (ReadTolerant).
		view.ctrls = append(view.ctrls, ctrlView{
			name:        ctrl.Name,
			trAddr:      ctrl.TrAddr,
			trSvcId:     ctrl.TrSvcId,
			state:       ctrl.State,
			anaState:    ctrl.AnaState,
			addrUnknown: ctrl.AddrErr,
			gone:        ctrl.AddrGone,
		})
	}
	return view, nil
}

// ---------------------------------------------------------------------------
// CN10 — leg side connections and the leg wrapper
// ---------------------------------------------------------------------------

// ensureLegs converges every leg of every group of every slice — spare legs
// included, both roles. It reports which legs are **available** (cnagent.md
// CN12), which is what the assembly needs, records each leg's ResInfo and a
// two-sided leg's path rows (legPathRows), and says
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
			s.legPathRows(ctx, st, plan, lp, nil, info)
			available[lp.legId] = false
			continue
		}
		view, err := s.ensureLeg(ctx, plan, lp, budget)
		// The path rows read the connect step's own view; a leg that failed
		// has none, and they look it up again (legPathRows).
		s.legPathRows(ctx, st, plan, lp, view, info)
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
// The leg is looked up in the pass's walk (lookupSubsys). The connect step
// waits, briefly and boundedly, for what it has just asked for (CN10): a
// failed connect is tried again within the pass (connectWithin), and after a
// connect this pass made the subsystem is read again until the kernel has
// finished adding the multipath namespace head (awaitNsHead). Both draw on
// budget, the pass's one wait budget. Once it is spent, a connect that fails
// fails the leg at once with its own error, and a head wait returns what
// its last lookup read: a picked head that is not complete is used as it is,
// and a step after the wait that fails fails the leg with its own error.
// Either way ensureLegs registers the background retry. Each connect marks
// the pass's walk stale first, so the lookup after it walks again: the
// subsystem it made may have taken the number of an entry freed since the
// walk.
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
	view, err := s.lookupSubsys(ctx, plan.walk, lp.nqn, sideNsid)
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
		plan.walk.stale = true
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
			ctx, budget, plan.walk, lp.nqn, sideNsid); err != nil {
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
	headNo, err := s.blockDevNo(ctx, path.Base(view.nsDev))
	if err != nil {
		return nil, err
	}
	if err := s.ensureDmLinear(
		ctx, lp.name, lp.sectors, headNo, 0); err != nil {
		return nil, err
	}
	return view, nil
}

// sideNsid is the single namespace id every side of a leg exports
// (architecture.md, Disk node).
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
// worker's fan-out ([D16]: the sides-first hold is bounded, so a SyncupCntlr
// can still come before the side's export), and what a permanently rejected
// export or a dead disk node answers; only the first goes away within a pass,
// and the budget is what caps how often the other two are retried. The last
// attempt's error is the one returned, so a leg that stays unconnected reads
// as a failed connect.
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

// awaitNsHead looks one subsystem up again after a connect this pass made,
// until its multipath namespace head is complete or the pass's budget no
// longer covers the next pause (CN10). The kernel returns from `nvme connect`
// as soon as the controller is live and only QUEUES the namespace scan, which
// adds the hidden per-path disk first and the head after it; a single read
// right after the connect can therefore miss a head that is milliseconds
// away. The head itself is added in steps: its directory, its nsid, its
// /sys/block entry, then what lets it be opened by its number, and its ng
// device last. So a lookup that has picked the head counts it only once the
// ng device is listed beside it: before that, the number read can find no
// /sys/block entry, and a table naming the number can fail to open it.
//
// The pause between two lookups is CnNsScanPause while no head is picked —
// none listed, or none whose nsid reads the one asked for — and the far
// shorter CnNsHeadPause while one is picked whose ng device is not listed
// yet. Once the budget does not cover the next pause the wait stops, writes
// msgHeadWaitSpent and returns what the last lookup read, and a picked head
// is then used as it is. A leg reads its number next, and a read that
// fails fails the leg with its own error. A clone source passes CN18's step
// 1 on it, since that step reads no number: the source's number is first
// read in step 3, for the dm-clone's table, so a read that fails there fails
// step 3, and its error leads the dm-clone's and the wrapper's rows that
// are not OK, never the source's row (CN18).
//
// Only a connect made in this pass is waited for: a subsystem that was
// already connected is judged on the one read, whatever it shows — no head,
// as a leg whose only path is ANA inaccessible never gets one, or a head
// with no ng device listed.
//
// The connect left pw stale, so the first lookup walks again. Each later one
// reads that entry alone — its subsysnqn, its directory and its controllers
// — while the entry it found stands; a lookup that finds no entry, or whose
// entry fails the subsysnqn check, marks the walk stale again, so the next
// one walks.
func (s *CnAgentServer) awaitNsHead(
	ctx context.Context,
	budget *agent.WaitBudget,
	pw *passWalk,
	nqn string,
	nsIdx uint32,
) (*subsysView, error) {
	scanPauses, headPauses := 0, 0
	for {
		view, err := s.lookupSubsys(ctx, pw, nqn, nsIdx)
		if err != nil || (view.nsDev != "" && view.ngListed) {
			return view, err
		}
		if !view.found {
			pw.stale = true
		}
		pause := common.CnNsScanPause
		if view.nsDev != "" {
			pause = common.CnNsHeadPause
		}
		if !budget.Pause(ctx, pause) {
			if ctx.Err() == nil {
				slog.WarnContext(ctx, msgHeadWaitSpent,
					slog.String("nqn", nqn),
					slog.String("head", view.nsDev),
					slog.Int("scan_pauses", scanPauses),
					slog.Int("head_pauses", headPauses))
			}
			return view, nil
		}
		if view.nsDev != "" {
			headPauses++
		} else {
			scanPauses++
		}
	}
}

// msgHeadWaitSpent is the Warn record of a head wait (awaitNsHead) that the
// pass's budget stopped (CN10): it names the subsystem, the head the last
// lookup picked ("" when none) and the pauses the wait took of each kind. A
// leg whose number read then fails on a head that was not complete, or a
// clone whose step 3 fails on its source's, is the kept case of the budget,
// which this record tells from a head wait that ended too early.
const msgHeadWaitSpent = "head wait budget spent"

// newPassBudget is one converge pass's CN10 wait budget: created once per
// convergeCntlr and handed down to every leg and clone-source connect of that
// pass, never kept across passes or shared between cntlrs.
func (s *CnAgentServer) newPassBudget() *agent.WaitBudget {
	return agent.NewWaitBudget(common.CnConnectPassBudget, s.now, s.sleep)
}

// disconnectDeadPaths retires a controller of the leg NQN whose transport
// matches no desired side — the src side after FinishMigration, whose
// controller died with DNR and will never reconnect. It is dropped by
// **device**, never by NQN: the surviving side shares that NQN ([D1];
// cnagent.md, `NvmeHost.DisconnectDevice`).
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

// legInfo is the CN28 leg report, shared by the converge pass and the probe:
// on a primary the wrapper table plus the CN11 block-probe outcome, on a
// standby the wrapper table plus transport liveness and ana_state per desired
// side (architecture.md, Group on-leg layout: meta region, data region,
// health block — a standby's path terminates in the side's dm-error, so block
// IO through it can never succeed).
func (s *CnAgentServer) legInfo(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	lp *legPlan,
	view *subsysView,
) *pb.ResInfo {
	key := resKeyOf(resKeyLegFmt, lp.legId)
	status, details := s.probeDmTarget(ctx, plan.rd, lp.name, "linear",
		lp.sectors, "", 0)
	if status != pb.ResStatus_RES_STATUS_OK {
		return st.tracker.Set(key, lp.name, status, details)
	}
	if plan.primary {
		probeStatus, probeDetails := s.legProbeOutcome(st, lp)
		return st.tracker.Set(key, lp.name, probeStatus, probeDetails)
	}
	if view == nil {
		var err error
		view, err = s.lookupSubsys(ctx, plan.walk, lp.nqn, sideNsid)
		if err != nil {
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

// legPathRows writes the path rows of one leg (CN28): for a leg whose
// side_list holds two sides — a migration's — one row per side, keyed by
// side id and named by the side's address, which says whether this node
// holds a live controller to that address. The worker's cutover reads the
// primary's row for the migration's dst side (dnv-worker.md RW18); no health
// rule judges these rows (dnv-worker.md HL2). A leg with one side has none.
//
// Every cntlr writes them, primary and standby, on both channels, and this is
// the one function that does: the converge after each leg's connect step
// (ensureLegs, or the level's MISSING rows in build), the check round and the
// info read in probeCntlr. A row reads, in this order: MISSING "sp_level"
// while the sp_level suppresses legs (CN19), as the leg row does;
// PROVISIONING while its side is unprovisioned ([D15]: nothing is exported
// for it, so its missing controller is the desired state); ERROR with the
// leg row's details while a sweep's disconnect of the leg's subsystem is in
// flight (CN10's disconnect registry); otherwise it is pathHealth's verdict
// on the leg's view.
//
// view is the leg's view the caller already holds — the converge's, from its
// connect step — or nil, and the function then looks the leg up in the pass's
// walk, once, and only when a side needs it. It returns the view it used, nil
// when it has none, so the probe's standby leg row reads the same lookup.
func (s *CnAgentServer) legPathRows(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	lp *legPlan,
	view *subsysView,
	info *pb.CntlrInfo,
) *subsysView {
	if !lp.twoSided() {
		return view
	}
	t := st.tracker
	var lookupErr error
	looked := view != nil
	for _, side := range lp.sides {
		sideId := side.GetSideId()
		key := resKeyOf(resKeyPathFmt, sideId)
		tr := side.GetNvmeTrConf()
		name := fmt.Sprintf("%s:%s", tr.GetTrAddr(), tr.GetTrSvcId())
		switch {
		case !plan.wantLeg:
			info.SideIdToPath[sideId] = t.Missing(key, name, detailsSpLevel)
			continue
		case !side.GetProvisioned():
			info.SideIdToPath[sideId] = t.Provisioning(
				key, name, detailsProvisioning)
			continue
		case s.disconnectInFlight(lp.nqn):
			info.SideIdToPath[sideId] = t.Err(
				key, name, detailsDisconnectInFlight(lp.nqn))
			continue
		}
		if !looked {
			view, lookupErr = s.lookupSubsys(
				ctx, plan.walk, lp.nqn, sideNsid)
			looked = true
		}
		status, details := pathHealth(view, lookupErr, tr)
		info.SideIdToPath[sideId] = t.Set(key, name, status, details)
	}
	return view
}

// twoSided reports whether the leg's side_list holds two sides: a
// migration's leg (CN10), the only kind that reports path rows.
func (lp *legPlan) twoSided() bool {
	return len(lp.sides) == 2
}

// pathResKeys is the ResTracker key of each path row one leg reports
// (legPathRows): one per side of a two-sided leg, none otherwise. A side
// that leaves the leg, or a leg that leaves the plan, leaves the wanted keys
// with it (cntlrResKeys), so its row's history goes.
func pathResKeys(lp *legPlan) []string {
	if !lp.twoSided() {
		return nil
	}
	keys := make([]string, 0, len(lp.sides))
	for _, side := range lp.sides {
		keys = append(keys, resKeyOf(resKeyPathFmt, side.GetSideId()))
	}
	return keys
}

// pathHealth is a provisioned side's path row (CN28, legPathRows): OK with
// the controller's state and ana_state when a live controller of the leg's
// subsystem has the side's address; ERROR with the same details when that
// controller is not live; MISSING "no controller" when none has it. The ANA
// state does not count: while a migration's cutover is pending its dst side
// exports inaccessible, and the cutover asks only for a live path
// (dnv-worker.md RW18). The ana_state is read from the path device, which the
// kernel's scan adds after the connect returns, so a fresh path may read
// "live/" for a moment. A lookup that did not answer is ERROR naming the
// read, never MISSING; so is a subsystem holding a controller whose address
// read did not answer when no controller has the side's address: that
// controller may be the side's (CN10: unknown, never unwanted). A gone
// controller has no address and is no side's path.
func pathHealth(
	view *subsysView,
	lookupErr error,
	tr *pb.NvmeTrConf,
) (pb.ResStatus, string) {
	if lookupErr != nil {
		return pb.ResStatus_RES_STATUS_ERROR, lookupErr.Error()
	}
	ctrl := view.ctrlOf(tr.GetTrAddr(), tr.GetTrSvcId())
	if ctrl == nil {
		if err := view.unknownCtrl(); err != nil {
			return pb.ResStatus_RES_STATUS_ERROR, err.Error()
		}
		return pb.ResStatus_RES_STATUS_MISSING, detailsNoController
	}
	details := ctrl.state + "/" + ctrl.anaState
	if ctrl.state != "live" {
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
// kernel does — so issued inline it would hold the cntlr's object lock, and
// with it every CheckCntlr round of the cntlr (CN1), for the whole minute.
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
// The goroutine carries the pass's trace id, or a fresh one when the pass had
// none — the command is that pass's own, only not waited for — and takes
// none of the CN1 locks. It is not joined at exit (`dnagent.md` SH27): the
// child holds nothing a restarted agent needs, and a delete already in the
// kernel finishes whether or not anybody waits for it.
//
// At most disconnectConcurrency of them run at once. The rest wait for a
// slot and stay registered while they wait, so a queued disconnect keeps the
// connect steps off its subsystem and is not issued twice either. Uncapped,
// one L10 of many legs or one pool drain would set them all going together,
// and each delete a vanished target stalls holds an OsClient slot for the
// admin timeout. Enough of those would leave no slot for the node's converges
// and Check rounds, whose OS calls would then be refused at the soft timeout
// and report ERROR rows on healthy objects.
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
// connect steps ask it before they look the subsystem up, because the delete
// outlives the locks of the pass that set it going: the controller they
// would find at a wanted address — and adopt, since they connect only the
// addresses that have none — is about to go, taking the device a leg wrapper
// or a dm-clone would be built on with it. A step that finds one in flight
// fails for the pass, which registers the CN10/CN18 connect retry, and the
// first pass after the goroutine has gone takes a walk of its own, reads the
// subsystem's entry, directory and controllers at its lookup, and connects
// whatever is missing. The CN28 probe asks it too, so the Check round
// reports that step the way the converge does.
func (s *CnAgentServer) disconnectInFlight(nqn string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.disconnecting[nqn]
	return ok
}

// ---------------------------------------------------------------------------
// CN10/CN18 background connect retry and CN18's clone-metadata retry (the
// DN13 pattern)
// ---------------------------------------------------------------------------

// startConnectRetry registers a cntlr for the CN10 background retry: a leg or
// a clone source failed to converge (CN10/CN18), a clone recovery's
// destination bitmaps were not applied or another later step of a clone
// failed (CN18 says which), a group's leg_list holds a member that is not
// available (CN12), the build failed to move the ANA group of a namespace
// or a transfer (CN16, CN17), or the build left the device of a namespace
// or a transfer whose desired group is optimized off what the desired state
// wants — held off its td's raid0 (CN18), waiting on the dm-error, or after
// a failed step — whatever the cause (CN16, CN17), but for its clone's room
// in the arena alone (waitsForRoom). A goroutine
// re-runs the whole converge every CnConnectRetryInterval seconds under the
// CN1 locks until a pass registers none of these (build's stopConnectRetry)
// or the cntlr is torn down. The RPC itself retries a connect, or waits for
// its namespace head, only as far as the pass's CnConnectPassBudget allows:
// an in-pass retry, or a step of that wait, starts only while the budget
// still covers its pause (connectWithin, awaitNsHead). An arena that has no
// free run for a clone's metadata is not one of its causes: that clone, and
// a namespace of its destination td that waits only for it, wait for CN18's
// slower clone-metadata retry (startCloneMetaRetry), and a run larger than
// the whole arena registers neither retry.
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

// startCloneMetaRetry registers a cntlr for CN18's clone-metadata retry: one
// of its clones found the arena with no free run for its metadata (CN18 step
// 2), so both of the clone's rows read PROVISIONING and nothing of it is
// built until a run comes free. A run comes free when a clone of another
// cntlr of the node goes, which no request of this cntlr says, so the cntlr
// asks again itself. A goroutine re-runs the whole converge every
// metaRetryInterval (CnCloneMetaRetryInterval seconds) under the CN1 locks
// until a pass leaves no clone waiting (build's stopCloneMetaRetry: the clone
// was built, left clone_list, or is suppressed by the level or a demotion),
// the cntlr is disabled (SP_LEVEL_DISABLE) or it is torn down
// (dropCntlrState). It runs through connectRetryLoop, the one copy of the
// DN13 pattern, and it is not the CN10 retry because of what each attempt
// costs: a whole converge under the object lock the cntlr's check rounds
// need, which at the CN10 pace would make a large pool's check rounds miss —
// and a clone that waits for room is not broken, so the longer interval
// loses nothing. A namespace of the clone's destination td that waits only
// for the clone registers no CN10 retry either: the attempt of this one that
// finds room builds the clone and moves the namespace to optimized (CN16).
// The two retries may run at once; each attempt is a whole converge, and a
// second one in a row is harmless.
func (s *CnAgentServer) startCloneMetaRetry(
	st *cntlrState,
	plan *cntlrPlan,
) {
	key := cntlrKey(plan.clusterId, plan.cnId, plan.spId, plan.cntlrId)
	s.mu.Lock()
	if st.metaRetrying {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.rootCtx)
	st.metaRetrying = true
	st.metaCancel = cancel
	interval := s.metaRetryInterval
	s.mu.Unlock()
	go s.connectRetryLoop(ctx, key, st, interval)
}

func (s *CnAgentServer) stopCloneMetaRetry(st *cntlrState) {
	s.mu.Lock()
	cancel := st.metaCancel
	st.metaCancel = nil
	st.metaRetrying = false
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// connectRetryLoop is the loop of both background retries, the CN10 connect
// retry and CN18's clone-metadata retry: each tick re-runs the cntlr's whole
// converge (reconvergeCntlr), until ctx — the registration's own — is
// cancelled.
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
		// reason dnagent's migrRetryLoop carries in full: the pass that ends
		// a retry calls stopConnectRetry or stopCloneMetaRetry, which cancels
		// exactly that retry's loop's ctx, so an attempt running on it would
		// cancel ITSELF and abandon the rest of the pass. The rule rests on
		// rootCtx alone, never on where a stop call happens to sit: this
		// loop is written as the DN13 pattern, and the two agents must not
		// differ in it.
		s.reconvergeCntlr(s.rootCtx, key, st)
		if ctx.Err() != nil || s.rootCtx.Err() != nil {
			return
		}
	}
}

// reconvergeCntlr re-runs one cntlr's converge from a background goroutine
// under the CN1 locks. Every attempt is its own traceable operation (SH2/CN2).
//
// The cntlr is checked under the node read lock and before its object lock
// is taken: a cntlr dropped since the attempt was scheduled — the drop runs
// under the node write lock and forgets the cntlr's lock entry
// (dropCntlrState) — gets no new entry from an attempt that converges
// nothing (dnagent.md, SH11). Nothing that drops or replaces a held cntlr
// runs under the read lock — a SyncupCntlr makes a new state only for a key
// that has none — so the state the check finds stays for the attempt.
func (s *CnAgentServer) reconvergeCntlr(
	ctx context.Context,
	key string,
	st *cntlrState,
) {
	attemptCtx := common.WithTraceId(ctx, common.NewTraceId())
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	if s.getCntlr(key) != st {
		return
	}
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	s.convergeCntlr(attemptCtx, st)
}
