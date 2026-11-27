package cnagent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"slices"
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
// A cntlr's converge and its read each start with one walk of
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
	// wcache is the write cache setting of the controller's path device,
	// "write back" or "write through", "" when unread (agent.CtrlRead
	// .WriteCache; CN10, The write cache of a leg's path).
	wcache string
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

// servingSide is the side of lp, a leg with one side, whose controller
// alone serves the leg on this node (CN28, RES_REASON_LEG_PATH_NOT_SOLE):
// every controller of the leg's subsystem that is not gone, deleting or
// dead has that side's address, and one of them reads live and ANA
// optimized, so IO to the leg goes to that side alone and no controller
// of another side can take it again; zero otherwise, and for a controller
// whose address did not read, which may be a path to anywhere (CN10).
func (v *subsysView) servingSide(lp *legPlan) uint64 {
	if v == nil || !v.found || len(lp.sides) != 1 {
		return 0
	}
	tr := lp.sides[0].GetNvmeTrConf()
	optimized := false
	for _, ctrl := range v.ctrls {
		if ctrl.addrUnknown != nil {
			return 0
		}
		if ctrl.gone || ctrl.state == "deleting" || ctrl.state == "dead" {
			continue
		}
		if ctrl.trAddr != tr.GetTrAddr() || ctrl.trSvcId != tr.GetTrSvcId() {
			// Another address: a live controller takes IO now, and one
			// that reconnects or resets can again, as long as that side's
			// disk node still exports, so neither leaves the side alone.
			return 0
		}
		optimized = optimized ||
			(ctrl.state == "live" && ctrl.anaState == "optimized")
	}
	if !optimized {
		return 0
	}
	return lp.sides[0].GetSideId()
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

// passWalk is the NVMe walk of one converge or read (CN10): where each
// subsystem entry of the node is, read at the start of the pass and shared by
// every lookup of it; a converge reads it again at the next lookup after it
// marks it stale. A read takes it before its rows, a converge right before its
// sweep, whose list of connections it is too (enumerateCn).
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
// other step of it, and a read names nothing on that answer. So does a
// listing of the matching entry's own directory that fails, whatever the
// failure: its
// controllers and namespace are unread; and a read of the namespace's nsid
// with no node picked, or of a controller's state, directory or path
// ana_state, that did not answer. A match is the answer even beside a
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
	return s.lookupSubsysOf(ctx, pw, nqn, nsIdx, false)
}

// lookupLegWcache is lookupSubsys that also reads each controller's path
// write cache (ctrlView.wcache), what only a primary's write cache step and
// its read of a marked leg use (CN10, The write cache of a leg's path).
func (s *CnAgentServer) lookupLegWcache(
	ctx context.Context,
	pw *passWalk,
	nqn string,
) (*subsysView, error) {
	return s.lookupSubsysOf(ctx, pw, nqn, sideNsid, true)
}

// lookupSubsysOf is lookupSubsys, with each path's write cache read when
// wcache is set.
func (s *CnAgentServer) lookupSubsysOf(
	ctx context.Context,
	pw *passWalk,
	nqn string,
	nsIdx uint32,
	wcache bool,
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
		NsIdx:      nsIdx,
		Rule:       agent.ReadTolerant,
		WriteCache: wcache,
	})
	switch {
	case err != nil:
		return nil, subsysUnknown(nqn, err)
	case read.DirErr != nil:
		return nil, subsysUnknown(nqn, read.DirErr)
	case read.DirGone:
		return nil, subsysUnknown(nqn,
			fmt.Errorf("list %s: %w", match.Dir, fs.ErrNotExist))
	case read.NsErr != nil && read.NsDev == "":
		// An nsid read that did not answer is no missing namespace; beside
		// a node it did pick, it is another nsid's, as the host keeps one
		// head per nsid.
		return nil, subsysUnknown(nqn, read.NsErr)
	}
	for _, ctrl := range read.Ctrls {
		// A state, directory or ana_state read that did not answer is no
		// path that is not live, nor an optimized one with no ANA state.
		// Unlike an address read that did not answer, which marks its own
		// controller unknown (unknownCtrl), it fails the lookup for every
		// side of the leg for the pass (CN10).
		for _, readErr := range []error{
			ctrl.StateErr, ctrl.DirErr, ctrl.AnaErr,
		} {
			if readErr != nil {
				return nil, subsysUnknown(nqn, readErr)
			}
		}
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
			wcache:      ctrl.WriteCache,
		})
	}
	return view, nil
}

// ---------------------------------------------------------------------------
// CN10 — leg side connections and the leg wrapper
// ---------------------------------------------------------------------------

// ensureLegs converges every leg of every group of every slice and of every
// clone's clone meta group — a repair's new legs included, at every stage
// of the repair, both roles. It reports
// which legs are **available** (cnagent.md CN12), which is what the assembly
// needs, and records each leg's ResInfo and a two-sided leg's path rows
// (legPathRows). A leg that failed to converge — its connect, a
// controller's address read that did not answer, its multipath namespace or
// its wrapper — goes to its row, and nothing here runs the converge again:
// a cntlr's read names each side a converge would connect and each wrapper
// a converge would change (sidesToConnect, wrapperName; cnagent.md CN30),
// and the worker re-sends the request on that name (dnv-worker.md RW4).
// budget is the pass's one CN10 wait budget, shared by every leg and by the
// clone sources after them.
func (s *CnAgentServer) ensureLegs(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	info *pb.CntlrInfo,
	budget *agent.WaitBudget,
) map[uint64]bool {
	available := make(map[uint64]bool, len(plan.legs))
	keepWcStale(st, plan)
	var fence fenceViews
	defer func() { s.fenceAfterLegs(ctx, st, plan, fence) }()
	for _, lp := range plan.legs {
		if lp.provisioning {
			// [D15]: every side is still zeroing, so the DN exports nothing —
			// there is no subsystem to connect to, no multipath namespace to
			// wrap and no device to probe. Deliberate, healthy, no action.
			// available is set explicitly, though the map's zero value reads
			// the same. A group with a provisioning leg_list leg is deferred,
			// but a repair's new leg's flag is read: the assembly and the add
			// take a new leg only once it is available (CN12), so never a
			// provisioning one.
			info.LegIdToLeg[lp.legId] = st.tracker.Provisioning(
				resKeyOf(resKeyLegFmt, lp.legId), lp.name, detailsProvisioning)
			s.legPathRows(ctx, st, plan, lp, nil, info)
			available[lp.legId] = false
			continue
		}
		view, err := s.ensureLeg(ctx, st, plan, lp, budget)
		fence.add(lp, view)
		// The path rows read the connect step's own view; a leg that failed
		// has none, and they look it up again (legPathRows).
		s.legPathRows(ctx, st, plan, lp, view, info)
		if err != nil {
			info.LegIdToLeg[lp.legId] = st.tracker.Err(
				resKeyOf(resKeyLegFmt, lp.legId), lp.name, err.Error())
			if plan.primary {
				s.keepLegProber(ctx, st, plan, lp)
			}
			continue
		}
		available[lp.legId] = view.available()
		if plan.primary {
			s.startLegProber(st, plan, lp)
		}
		info.LegIdToLeg[lp.legId] = s.legInfo(ctx, st, plan, lp, view, nil)
		markNotSole(st.tracker, info, lp)
	}
	return available
}

// keepLegProber starts a primary leg's prober (CN11) after a pass whose
// ensureLeg failed, when the leg has none, no sweep is disconnecting its
// subsystem, and its wrapper is there as the leg's whole-size dm-linear,
// resumed and writable: two dm reads, made only for a leg with no prober.
// The prober needs only the wrapper's path, takes no lock and needs no
// converge, so a leg whose wrapper an earlier pass built is probed whatever
// stopped this pass — a lookup that did not answer, a head number read that
// failed — and its row says how the leg serves, where it would otherwise
// read PENDING with no converge to come (cnagent.md CN11). A wrapper over a
// head the kernel replaced fails its probe, which is the leg's truth, and a
// read names that wrapper once a lookup answers (wrapperName). A wrapper
// that is absent, of another shape, suspended or read-only, or whose read
// did not answer, gets no prober: a probe through it would fail or hang for
// a reason the leg's paths do not have, and the early fail would count it
// (CN12).
func (s *CnAgentServer) keepLegProber(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	lp *legPlan,
) {
	s.mu.Lock()
	_, running := st.probers[lp.legId]
	s.mu.Unlock()
	if running || s.disconnectInFlight(lp.nqn) {
		return
	}
	shape, err := s.dmShapeOf(ctx, nil, lp.name, "linear", lp.sectors, nil,
		argCmpAny, true)
	if err != nil || shape != dmShapeWanted {
		return
	}
	s.startLegProber(st, plan, lp)
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
// Either way a later read names what a converge would still do: each side
// with no controller (sidesToConnect) and the wrapper (wrapperName;
// cnagent.md CN30). Each connect marks
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
	st *cntlrState,
	plan *cntlrPlan,
	lp *legPlan,
	budget *agent.WaitBudget,
) (*subsysView, error) {
	if s.disconnectInFlight(lp.nqn) {
		return nil, fmt.Errorf("%s", detailsDisconnectInFlight(lp.nqn))
	}
	// A primary reads its paths' write cache in this lookup (CN10, The
	// write cache of a leg's path), so its write cache step takes no lookup
	// of its own.
	view, err := s.lookupSubsysOf(ctx, plan.walk, lp.nqn, sideNsid,
		plan.primary)
	if err != nil {
		return nil, err
	}
	connected := false
	for _, side := range sidesToConnect(lp, view, plan.primary) {
		tr := side.GetNvmeTrConf()
		plan.walk.stale = true
		if err := s.connectWithin(ctx, plan, budget, agent.TrConf{
			TrType:  tr.GetTrType(),
			AdrFam:  tr.GetAdrFam(),
			TrAddr:  tr.GetTrAddr(),
			TrSvcId: tr.GetTrSvcId(),
		}, lp.nqn, plan.hostNqn(), plan.legFastIoFail); err != nil {
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
	if cut := pathsToCut(plan.primary, lp, view); len(cut) > 0 &&
		s.otherMemberInSync(ctx, lp) {
		s.cutPaths(ctx, lp, cut)
		plan.walk.stale = true
		if view, err = s.lookupSubsys(
			ctx, plan.walk, lp.nqn, sideNsid); err != nil {
			return nil, err
		}
	}
	s.disconnectDeadPaths(ctx, lp, view)
	// CN10: unknown, never unwanted — disconnectDeadPaths passed over it, and
	// the leg fails for the pass; a later read judges the leg again once the
	// address answers. After a connect this pass made, this is the re-read
	// view.
	if err := wrapperUnder(lp, view); err != nil {
		return nil, err
	}
	// CN10, The write cache of a leg's path: a primary scans again each path
	// it read while its disk node served it no data, before the wrapper's
	// step, so the wrapper and the array over it take the setting the disk
	// node reports.
	var stale []ctrlView
	if plan.primary {
		view, stale = s.freshenLegPaths(ctx, plan, lp, view, budget)
		if err := wrapperUnder(lp, view); err != nil {
			return nil, err
		}
	}
	headNo, err := s.legHeadNo(ctx, plan, lp, view)
	if err != nil {
		return nil, err
	}
	step, err := s.stepDmLinear(ctx, lp.name, lp.sectors, headNo, 0)
	if err != nil {
		// CN30: a create, reload or resume of the wrapper that failed is
		// named by the converge's own verdict, as a read names a wrapper
		// such a step would change (wrapperName); a read of the wrapper that
		// did not answer is left to the next read.
		if step != dmStepNone {
			lp.wrapperName = rowFailure(errRowDiffers, err.Error())
		}
		return nil, err
	}
	if plan.primary {
		if err := s.freshenWrapper(ctx, st, lp, view, stale, headNo,
			step); err != nil {
			return nil, err
		}
	}
	return view, nil
}

// legHeadNo is the number ensureLeg builds lp's wrapper over (CN10, The head
// is looked up again around its number): the number read by the head name view holds, taken
// only once a second lookup of the leg, made after that read in the pass's
// walk, names the same head. The steps between the lookup that named the
// head and the wrapper's — the other member's in-sync test before a cut, a
// dead path's disconnect, a primary's rescan and its pauses — can take
// seconds, and a head's name holds its subsystem's number, which the
// kernel frees with the leg's subsystem and gives to the next new
// controller: another cntlr's converge, which runs beside this one, or a
// dn agent on the machine can connect one meanwhile, and a number read by
// the old name would be that connection's head, another pool's leg. The
// lookup before the read and the one after it bound the read of the name;
// the number can still be freed between the second lookup and the
// wrapper's create (cnagent.md, Known limits). A head that changed, or a
// second lookup that fails, fails the leg for the pass, as any other step
// of it does. view itself is kept: the second lookup reads no path's write
// cache, which a primary's write cache step reads from view.
func (s *CnAgentServer) legHeadNo(
	ctx context.Context,
	plan *cntlrPlan,
	lp *legPlan,
	view *subsysView,
) (string, error) {
	headNo, err := s.blockDevNo(ctx, path.Base(view.nsDev))
	if err != nil {
		return "", err
	}
	again, err := s.lookupSubsys(ctx, plan.walk, lp.nqn, sideNsid)
	if err != nil {
		return "", err
	}
	if again.nsDev != view.nsDev {
		return "", fmt.Errorf("the head of %s changed from %q to %q "+
			"before its wrapper's step", lp.nqn, view.nsDev, again.nsDev)
	}
	return headNo, nil
}

// freshenWrapper is the rest of a primary's write cache step on lp (CN10,
// The write cache of a leg's path): a wrapper whose table is as wanted and
// that reads "write through" over a head that reads "write back" is loaded
// again (wcacheStale), and then the leg's mark is set (markLegWcache): a
// path that stalePaths found and that still reads "write through", or the
// wrapper, is stale, which names its wrapper, as a failed wrapper step does,
// so the worker sends the syncup again (CN30). A reload that fails is the
// leg's error.
func (s *CnAgentServer) freshenWrapper(
	ctx context.Context,
	st *cntlrState,
	lp *legPlan,
	view *subsysView,
	stale []ctrlView,
	headNo string,
	step dmStep,
) error {
	if step == dmStepNone && s.wcacheStale(ctx, lp.name, headNo) {
		if err := s.dm.Reload(ctx, lp.name,
			dmTable("linear", lp.sectors, linearArgs(headNo, 0))); err != nil {
			lp.wrapperName = rowFailure(errRowDiffers, err.Error())
			return err
		}
	}
	var still []ctrlView
	for _, was := range stale {
		if !pathsFresh(view, []ctrlView{was}) {
			still = append(still, was)
		}
	}
	details := legWcacheDetails(still, s.wcacheStale(ctx, lp.name, headNo))
	markLegWcache(st, lp, view, details)
	if details != "" {
		lp.wrapperName = rowFailure(errRowDiffers, details)
	}
	return nil
}

// keepWcStale sets the cntlr's write cache marks for a converge (CN10):
// none on a standby, which asks no target, and on a primary those of the
// legs the plan still holds, which each leg's step then sets or drops
// (freshenWrapper).
func keepWcStale(st *cntlrState, plan *cntlrPlan) {
	if !plan.primary {
		st.wcStale = nil
		return
	}
	keep := make(map[uint64]wcMark, len(st.wcStale))
	for _, lp := range plan.legs {
		if mark, ok := st.wcStale[lp.legId]; ok {
			keep[lp.legId] = mark
		}
	}
	st.wcStale = keep
}

// wrapperUnder is the test ensureLeg makes of a leg's lookup before the
// wrapper step (CN10), which a cntlr's read shares (wrapperName): every
// controller's address known, since an unknown one may be any side's path,
// and a multipath namespace head to build the wrapper over. It returns the
// error the leg fails with, nil when the step can run.
func wrapperUnder(lp *legPlan, view *subsysView) error {
	if err := view.unknownCtrl(); err != nil {
		return err
	}
	if view.nsDev == "" {
		return fmt.Errorf("no multipath namespace for %s", lp.nqn)
	}
	return nil
}

// wrapperStepOf is ensureLeg's steps up to the wrapper's, read and not
// taken, which a cntlr's read shares (wrapperName, readAvailable): the
// test of the lookup view (wrapperUnder), the head's number from /sys/block
// (blockDevNo), one file read in the agent's own process, and the wrapper's
// shape against the whole-size dm-linear over that number, read-only flag
// included, from the read's reading rd. It returns that shape and the step
// a converge would take on it (dmSingleStep), and false where the converge
// would fail the leg before that step — a wrapper of no size, which
// stepDmSingle refuses, a lookup with an unknown controller or no head, a
// number read that failed, the head's /sys/block entry not there among
// them — or the wrapper's read did not answer.
func (s *CnAgentServer) wrapperStepOf(
	ctx context.Context,
	rd *cnReading,
	lp *legPlan,
	view *subsysView,
) (dmShape, dmStep, bool) {
	if lp.sectors == 0 || wrapperUnder(lp, view) != nil {
		return dmShapeWanted, dmStepNone, false
	}
	headNo, err := s.blockDevNo(ctx, path.Base(view.nsDev))
	if err != nil {
		return dmShapeWanted, dmStepNone, false
	}
	shape, err := s.dmShapeOf(ctx, rd, lp.name, "linear", lp.sectors,
		linearArgs(headNo, 0), 0, true)
	if err != nil {
		return dmShapeWanted, dmStepNone, false
	}
	return shape, dmSingleStep(shape), true
}

// wrapperName is what a cntlr's read names lp's wrapper with (CN10, CN30):
// "absent" or "differs" while a converge would create, reload or resume it
// (wrapperStepOf), and nil otherwise. view is the leg's lookup, which
// answered. So a wrapper that maps another number than the head the lookup
// finds is named, as the converge reloads it, and so is a read-only one, as
// the converge reloads it and makes it writable. A step the converge would
// fail the leg at names nothing, and so does a read that did not answer. A
// read takes it only for a wanted leg that is not provisioning and whose
// subsystem no sweep is disconnecting (probeCntlr), the legs ensureLeg
// reaches the steps for.
func (s *CnAgentServer) wrapperName(
	ctx context.Context,
	rd *cnReading,
	lp *legPlan,
	view *subsysView,
) error {
	shape, step, ok := s.wrapperStepOf(ctx, rd, lp, view)
	if !ok || step == dmStepNone {
		return nil
	}
	return shapeFailure(shape, "linear")
}

// sideNsid is the single namespace id every side of a leg exports
// (architecture.md, Disk node).
const sideNsid = 1

// sidesToConnect is ensureLeg's connect test (CN10), which a cntlr's read
// names too (CN30): the provisioned sides of lp that view holds no
// controller for, in any state. A side not provisioned is left out ([D15]:
// it is still zeroing, so the DN has not built its per-CN export stack and a
// connect could only fail); the provisioned sides of the same leg connect
// normally, which is what keeps a migrating leg serving through its src side
// while its dst side provisions. A view with a controller whose address read
// did not answer gives none: that controller may be any side's path. On a
// primary, a leg of an md-raid1 group with two sides whose serving path is
// down (servingPathDown) gives none either (CN10, The cut of a migrating
// leg's path): the other side's path would come up inaccessible and be cut
// again, so it waits for the serving path to be live again or for the
// migration's end.
func sidesToConnect(lp *legPlan, view *subsysView, primary bool) []*pb.Side {
	if view.unknownCtrl() != nil {
		return nil
	}
	if cuttable(primary, lp) && servingPathDown(lp, view) {
		return nil
	}
	var out []*pb.Side
	for _, side := range lp.sides {
		tr := side.GetNvmeTrConf()
		if side.GetProvisioned() &&
			view.ctrlOf(tr.GetTrAddr(), tr.GetTrSvcId()) == nil {
			out = append(out, side)
		}
	}
	return out
}

// pathDown says whether a controller is a path that does not serve:
// connecting, as the kernel's reconnect loop leaves a path whose disk node
// stopped answering, or dead. A resetting one is not yet: the kernel's error
// recovery moves it on to live or to connecting within seconds; and a
// deleting one is on its way out, as a disconnect leaves it.
func pathDown(ctrl ctrlView) bool {
	return ctrl.state == "connecting" || ctrl.state == "dead"
}

// legCtrls are the controllers of view that are paths to a side of lp, by
// address; a gone controller or one whose address is unknown is none.
func legCtrls(lp *legPlan, view *subsysView) []ctrlView {
	if view == nil || !view.found {
		return nil
	}
	var out []ctrlView
	for _, ctrl := range view.ctrls {
		if ctrl.gone || ctrl.addrUnknown != nil {
			continue
		}
		for _, side := range lp.sides {
			tr := side.GetNvmeTrConf()
			if ctrl.trAddr == tr.GetTrAddr() &&
				ctrl.trSvcId == tr.GetTrSvcId() {
				out = append(out, ctrl)
				break
			}
		}
	}
	return out
}

// servingPathDown says whether a path of lp in view is down (pathDown)
// whose last ANA state, which a down controller keeps, was anything but
// inaccessible: the side that served, or was ready to. A down path that last
// read inaccessible was not serving when it went down, so the other side may
// serve and is connected as usual.
func servingPathDown(lp *legPlan, view *subsysView) bool {
	for _, ctrl := range legCtrls(lp, view) {
		if pathDown(ctrl) && ctrl.anaState != "inaccessible" {
			return true
		}
	}
	return false
}

// pathsToCut is CN10's cut of a migrating leg's path: on a primary, for a
// leg with two sides one of whose paths is down (pathDown) while every
// live path of the leg reads ANA inaccessible, the live paths, which the
// converge disconnects; none otherwise. Reason: the kernel counts a live
// inaccessible path as one IO may still go to, so with the serving path
// down a write to the leg waits without limit and md never fails the
// member, and the group's writes stop; with the live path cut, the leg
// behaves as a leg with one path, whose death md and the leg probe already
// handle (seen in the lab on the 7.0 kernel). Not on a standby, which holds
// no array, so nothing of it waits and its two paths keep it ready for a
// promotion; not for a leg of a group that is not md-raid1, which has no
// other leg to take the write, so a cut would turn the wait into IO errors
// to the hosts; not while a live path reads anything but inaccessible,
// which serves or is ready to; and not in the migration's gap, where both
// paths are live and inaccessible. The test reads the node alone.
func pathsToCut(primary bool, lp *legPlan, view *subsysView) []ctrlView {
	if !cuttable(primary, lp) || view.unknownCtrl() != nil {
		return nil
	}
	ctrls := legCtrls(lp, view)
	down := false
	var live []ctrlView
	for _, ctrl := range ctrls {
		switch {
		case pathDown(ctrl):
			down = true
		case ctrl.state == "live":
			if ctrl.anaState != "inaccessible" {
				return nil
			}
			live = append(live, ctrl)
		}
	}
	if !down {
		return nil
	}
	return live
}

// cuttable says whether CN10's cut can apply to lp at all: on a primary, a
// leg with two sides of an md-raid1 group.
func cuttable(primary bool, lp *legPlan) bool {
	return primary && lp.twoSided() && lp.grp != nil && lp.grp.raid1
}

// pathWatcher is CN10's watcher of one leg whose path the primary may cut
// (cuttable): a goroutine that looks the leg up in a walk of its own every
// probe interval and cuts what pathsToCut names, taking no lock, so a cut
// does not wait for a converge, which can wait for the cntlr's object lock
// (CN1) behind a background read whose status read of the leg's own pool
// waits behind a commit that waits on the leg (CN28, The reading;
// dnagent.md SH17). key is
// the leg's NQN and its sides' addresses, so a leg whose sides change gets
// a new watcher.
type pathWatcher struct {
	key    string
	cancel context.CancelFunc
}

// pathWatchKey is the key of lp's watcher.
func pathWatchKey(lp *legPlan) string {
	key := lp.nqn
	for _, side := range lp.sides {
		tr := side.GetNvmeTrConf()
		key += "|" + tr.GetTrAddr() + ":" + tr.GetTrSvcId()
	}
	return key
}

// keepPathWatchers keeps the cntlr's path watchers in step with plan: one
// for each leg the plan builds whose path the primary may cut (cuttable),
// started when missing or when the leg's key changed, and every other one
// stopped; a nil plan, the cntlr's drop, stops them all. It reads the plan
// alone.
func (s *CnAgentServer) keepPathWatchers(st *cntlrState, plan *cntlrPlan) {
	wanted := make(map[uint64]*legPlan)
	if plan != nil && plan.wantLeg {
		for _, lp := range plan.legs {
			if cuttable(plan.primary, lp) && !lp.provisioning {
				wanted[lp.legId] = lp
			}
		}
	}
	s.mu.Lock()
	var cancels []context.CancelFunc
	for legId, watcher := range st.cutters {
		if lp, ok := wanted[legId]; ok && pathWatchKey(lp) == watcher.key {
			delete(wanted, legId)
			continue
		}
		cancels = append(cancels, watcher.cancel)
		delete(st.cutters, legId)
	}
	interval := s.probeInterval
	for legId, lp := range wanted {
		ctx, cancel := context.WithCancel(s.rootCtx)
		st.cutters[legId] = &pathWatcher{key: pathWatchKey(lp),
			cancel: cancel}
		go s.pathWatchLoop(ctx, lp, interval)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// pathWatchLoop is one path watcher's loop (pathWatcher): every interval a
// lookup of the leg in a fresh walk, and the cut pathsToCut names.
func (s *CnAgentServer) pathWatchLoop(
	ctx context.Context,
	lp *legPlan,
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
		tickCtx := common.WithTraceId(ctx, common.NewTraceId())
		view, err := s.lookupSubsys(tickCtx, s.newPassWalk(tickCtx), lp.nqn,
			sideNsid)
		if err != nil {
			continue
		}
		if cut := pathsToCut(true, lp, view); len(cut) > 0 &&
			s.otherMemberInSync(tickCtx, lp) {
			s.cutPaths(tickCtx, lp, cut)
		}
	}
}

// otherMemberInSync says whether md holds a member of lp's group other than
// lp's own in sync (MdMember.inSync), read from sysfs in a walk of its own:
// the cut of lp's path (CN10) makes md fail lp's member, which only another
// member in sync can take the writes from; with none, the cut would end the
// wait in an IO error on the array's last member. A read that does not
// answer, and an array that does not run, say no.
func (s *CnAgentServer) otherMemberInSync(
	ctx context.Context,
	lp *legPlan,
) bool {
	if lp.grp == nil {
		return false
	}
	walk, err := s.md.Walk(ctx)
	if err != nil {
		return false
	}
	detail, err := s.md.Detail(ctx, walk, lp.grp.memberNames())
	if err != nil || detail == nil || !detail.Exists {
		return false
	}
	for _, member := range detail.Members {
		if member.DmName != lp.name && member.inSync() {
			return true
		}
	}
	return false
}

// cutPaths disconnects each controller of cut, by its device, and logs it;
// a disconnect that fails is logged, and the next converge tries again.
// Each device is looked up once more, in a fresh walk, just before its
// disconnect, and disconnected only while it is still a path of the leg at
// the same address: the path watcher and a converge can both cut, and the
// kernel gives a device name freed by one to the next controller connected.
func (s *CnAgentServer) cutPaths(
	ctx context.Context,
	lp *legPlan,
	cut []ctrlView,
) {
	for _, ctrl := range cut {
		view, err := s.lookupSubsys(ctx, s.newPassWalk(ctx), lp.nqn,
			sideNsid)
		if err != nil {
			continue
		}
		still := false
		for _, cur := range view.ctrls {
			if cur.name == ctrl.name && cur.trAddr == ctrl.trAddr &&
				cur.trSvcId == ctrl.trSvcId && !cur.gone {
				still = true
			}
		}
		if !still {
			continue
		}
		err = s.host.DisconnectDevice(ctx, ctrl.name)
		if err != nil {
			slog.ErrorContext(ctx, "cutting a leg path failed",
				slog.String("nqn", lp.nqn),
				slog.String("device", ctrl.name),
				slog.String("error", err.Error()))
			continue
		}
		slog.InfoContext(ctx, "leg path cut",
			slog.String("nqn", lp.nqn),
			slog.String("device", ctrl.name),
			slog.String("addr", ctrl.trAddr+":"+ctrl.trSvcId))
	}
}

// connectWithin is the CN10 connect of one side of a leg, and the CN18
// connect of one clone-source path: an attempt, and again after a
// CnConnectRetryPause whenever it failed, for as long as the pass's budget
// covers the pause. An attempt takes one of connectSlots, the agent's connect
// places, and then dials the side's address (NvmeHost.Dial, with
// CnConnectDialTimeout) and, once the dial answers, runs `nvme connect` with
// the fast IO fail timeout fastIoFailTmo; it gives the place back when it
// ends. The wait for a place is no attempt and is charged nothing: every
// failed attempt is charged its own elapsed time from the place on, its dial
// and its connect.
//
// The kernel runs one fabrics connect at a time on the machine, and a
// connect to an address that does not answer holds that turn for about three
// seconds, the SYNs it sends going unanswered, with every other connect of
// the node, a dn agent's or a host's on the same machine among them,
// waiting behind it. The dial meets such an address in the agent's own
// process instead and gives up at its timeout, which overdraws the budget at
// once, so the attempt is not retried; it marks the address dead in the
// pass's plan (deadAddrs), and every later attempt of the pass at that
// address, another leg's or a clone source's, fails at once with the dial's
// error, taking no place, dialling nothing and charging nothing. A dial the
// address refuses — a disk node whose port does not listen yet — is a failed
// attempt like a failed connect, retried in the budget.
//
// The error of a failed connect is never classified. The same failure is
// what a disk node that has not linked the export into its port yet answers
// when the port already listens for another export — rejected with "failed
// to write to nvme-fabrics device" — for as long as its SyncupSide takes
// after the worker's fan-out ([D16]: the sides-first hold is bounded, so a
// SyncupCntlr can still come before the side's export), and what a
// permanently rejected export answers; only the first goes away within a
// pass, and the budget is what caps how often the other is retried. The last
// attempt's error is the one returned, so a leg that stays unconnected reads
// as a failed dial or a failed connect.
func (s *CnAgentServer) connectWithin(
	ctx context.Context,
	plan *cntlrPlan,
	budget *agent.WaitBudget,
	tr agent.TrConf,
	nqn string,
	hostNqn string,
	fastIoFailTmo uint32,
) error {
	addr := agent.DialAddress(tr)
	for {
		if err := plan.deadAddrs[addr]; err != nil {
			return err
		}
		release, err := s.takeConnectSlot(ctx)
		if err != nil {
			return err
		}
		start := budget.Now()
		err = s.host.Dial(ctx, tr, common.CnConnectDialTimeout)
		dead := errors.Is(err, common.ErrDialTimeout)
		if err == nil {
			err = s.host.Connect(ctx, tr, nqn, hostNqn, fastIoFailTmo)
		}
		release()
		if err == nil {
			return nil
		}
		budget.Charge(budget.Now().Sub(start))
		if dead {
			plan.markDeadAddr(addr, err)
			return err
		}
		if !budget.Pause(ctx, common.CnConnectRetryPause) {
			return err
		}
	}
}

// takeConnectSlot waits for one of connectSlots and returns its release, or
// ctx's error when ctx ends first. It takes no lock and no OsClient slot
// while it waits; the caller's cntlr lock stays held.
func (s *CnAgentServer) takeConnectSlot(ctx context.Context) (func(), error) {
	slots := s.connectSlots
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// connectConcurrency is how many connect attempts of the agent run at once
// (connectWithin), across every cntlr's pass. The kernel runs one fabrics
// connect at a time, so more places would only queue in the kernel, each
// holding an OsClient slot; more than one leaves room for a dial to an
// address that does not answer beside the connects to live ones.
const connectConcurrency = 4

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
// step 3, and its error leads the dm-clone's row when that row is not OK,
// never the source's row (CN18).
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
// one walks. A lookup that did not answer ends the wait with its error
// (lookupSubsys), which fails the leg or the clone source for the pass.
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
				slog.InfoContext(ctx, msgHeadWaitSpent,
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

// msgHeadWaitSpent is the Info record of a head wait (awaitNsHead) that the
// pass's budget stopped (CN10), at Info since the next converge waits again
// (log.md R6): it names the subsystem, the head the last
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
	lookupErr error,
) *pb.ResInfo {
	key := resKeyOf(resKeyLegFmt, lp.legId)
	status, details := s.probeDmTarget(ctx, plan.rd, lp.name, "linear",
		lp.sectors, "", 0)
	if status != pb.ResStatus_RES_STATUS_OK {
		return st.tracker.Set(key, lp.name, status, details)
	}
	if plan.primary {
		// A leg whose path or wrapper keeps a stale write cache reads error
		// whatever its probe says (CN10): its flushes stop short of a disk
		// with a write cache.
		if details := st.wcStale[lp.legId].stale; details != "" {
			return st.tracker.Err(key, lp.name, details)
		}
		probeStatus, probeDetails := s.legProbeOutcome(st, lp)
		return st.tracker.Set(key, lp.name, probeStatus, probeDetails)
	}
	if lookupErr != nil {
		return st.tracker.Err(key, lp.name, lookupErr.Error())
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
// (`change`, `persistent-loss`, garbage); a transient bad read during a
// cutover's ana_grpid rewrites shows in the log alone, since a standby's leg
// row is logged and never recorded (dnv-worker.md HL2), save that two
// reports in a row with every leg of a group so, no group of a migrating
// leg among them, pass the standby over in the failover while another
// candidate stands (dnv-worker.md AR5); a persistent one shows there
// too, and in the primary's leg row (CN11) once the worker makes this
// standby the primary.
// A leg whose side_list holds two sides is
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
// (ensureLegs, or the level's MISSING rows in build), the read in
// probeCntlr. A row reads, in this order: the level's MISSING row
// (suppressedRow) while the sp_level suppresses legs (CN19), as the leg row
// does; PROVISIONING while its side is unprovisioned ([D15]: nothing is
// exported for it, so its missing controller is the desired state); ERROR
// with the leg row's details while a sweep's disconnect of the leg's
// subsystem is in flight (CN10's disconnect registry); otherwise it is
// pathHealth's verdict on the leg's view.
//
// view is the leg's view the caller already holds — the converge's, from its
// connect step — or nil, and the function then looks the leg up in the pass's
// walk, once, and only when a side needs it. It returns the view it used, nil
// when it has none, and the error of its lookup, so the probe's leg row and
// its verdict read the same lookup.
func (s *CnAgentServer) legPathRows(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	lp *legPlan,
	view *subsysView,
	info *pb.CntlrInfo,
) (*subsysView, error) {
	if !lp.twoSided() {
		s.legServingSide(ctx, plan, lp, view)
		return view, nil
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
			info.SideIdToPath[sideId] = suppressedRow(t, key, name)
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
	return view, lookupErr
}

// legServingSide is a primary's serving-side test of a leg with one side
// that carries a recopy mark (Leg.recopy_revision), as a forced migration
// cancel after the cutover leaves it (CN28, RES_REASON_LEG_PATH_NOT_SOLE),
// on both channels: the side whose controller alone serves the leg
// (subsysView.servingSide), from view, the converge's own, or else a lookup
// of the leg in the pass's walk that it keeps to itself, so the read's
// other steps over the leg see no view they did not take. It sets
// lp.servingSide, which the group steps of the same pass read (CN12, The
// recopy), and lp.notSole where the test found no such side or its lookup
// did not answer, which the leg row reads (markNotSole); neither on a
// standby, whose paths are not optimized by design and which copies
// nothing, nor for a leg with no mark, which a primary's read does not look
// up while its row reads OK, nor while the level suppresses legs, while the side is unprovisioned
// or while a sweep's disconnect of the leg's subsystem is in flight, each
// of which gives the leg row a status of its own.
func (s *CnAgentServer) legServingSide(
	ctx context.Context,
	plan *cntlrPlan,
	lp *legPlan,
	view *subsysView,
) {
	lp.servingSide, lp.notSole = 0, false
	if !plan.primary || lp.leg.GetRecopyRevision() == 0 || !plan.wantLeg ||
		len(lp.sides) != 1 || !lp.sides[0].GetProvisioned() ||
		s.disconnectInFlight(lp.nqn) {
		return
	}
	if view == nil {
		var err error
		view, err = s.lookupSubsys(ctx, plan.walk, lp.nqn, sideNsid)
		if err != nil {
			lp.notSole = true
			return
		}
	}
	lp.servingSide = view.servingSide(lp)
	lp.notSole = lp.servingSide == 0
}

// markNotSole gives the leg row of lp, a leg with one side whose side
// legServingSide did not find serving alone, RES_REASON_LEG_PATH_NOT_SOLE,
// its status and details kept, while the row reads RES_STATUS_OK (CN28): a
// row of another status already says the leg does not serve.
func markNotSole(t *agent.ResTracker, info *pb.CntlrInfo, lp *legPlan) {
	row := info.GetLegIdToLeg()[lp.legId]
	if !lp.notSole || row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
		return
	}
	info.LegIdToLeg[lp.legId] = t.SetReason(resKeyOf(resKeyLegFmt, lp.legId),
		row.GetResName(), row.GetStatus(), row.GetDetails(),
		pb.ResReason_RES_REASON_LEG_PATH_NOT_SOLE)
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
// with it every read of the cntlr (CN1), for the whole minute.
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
// and reads, whose OS calls would then be refused at the soft timeout
// and report ERROR rows on healthy objects.
func (s *CnAgentServer) startDisconnect(ctx context.Context, nqn string) {
	s.startInRegistry(ctx, nqn, func(bgCtx context.Context) {
		if err := s.host.Disconnect(bgCtx, nqn); err != nil {
			slog.ErrorContext(bgCtx, "nvme disconnect failed",
				slog.String("nqn", nqn),
				slog.String("error", err.Error()))
		}
	})
}

// startDisconnectCtrls is startDisconnect by controller: it disconnects each
// of ctrls by its device (`nvme disconnect --device`), one after another, in
// one goroutine that holds one slot, and never the whole NQN. It is the
// disconnect of a clone source the node-level sweep judges by its
// controllers' host NQN (CN21): the host keeps one subsystem per NQN, so a
// host stack on the same machine can hold a controller of the same
// subsystem with a host NQN of its own, which a disconnect by NQN would cut.
// The registry entry is the NQN's, as for startDisconnect, so a clone build
// of the same source neither adopts nor connects it while the goroutine
// runs (disconnectInFlight), and a second pass issues nothing more.
//
// The goroutine can run long after the pass read ctrls — it waits for a
// slot, and each disconnect before it can wait out the admin timeout — and
// the kernel gives a freed controller's number to the next controller
// connected, which may be a leg's or a host stack's. So right before each
// disconnect it reads the controller again (ownCtrlStill), as cutPaths
// does, and passes over one that is no longer a controller of dir's
// subsystem, nqn, opened with hostNqn.
func (s *CnAgentServer) startDisconnectCtrls(
	ctx context.Context,
	nqn string,
	dir string,
	hostNqn string,
	ctrls []string,
) {
	ctrls = append([]string(nil), ctrls...)
	s.startInRegistry(ctx, nqn, func(bgCtx context.Context) {
		for _, ctrl := range ctrls {
			if !s.ownCtrlStill(bgCtx, nqn, dir, hostNqn, ctrl) {
				slog.InfoContext(bgCtx, "controller passed over: no "+
					"longer the source's of this host NQN",
					slog.String("nqn", nqn),
					slog.String("device", ctrl))
				continue
			}
			if err := s.host.DisconnectDevice(bgCtx, ctrl); err != nil {
				slog.ErrorContext(bgCtx, "nvme disconnect failed",
					slog.String("nqn", nqn),
					slog.String("device", ctrl),
					slog.String("error", err.Error()))
			}
		}
	})
}

// ownCtrlStill reads, right before a disconnect, whether ctrl is still a
// controller of the subsystem entry dir, whose subsysnqn still reads nqn,
// and whose hostnqn reads hostNqn. A read that did not answer is a no: the
// disconnect of a controller nobody can name is never issued, and the next
// pass's check sets one going again if the connection is still there.
func (s *CnAgentServer) ownCtrlStill(
	ctx context.Context,
	nqn string,
	dir string,
	hostNqn string,
	ctrl string,
) bool {
	got, ok, err := s.host.ReadSubsysNqn(ctx, dir)
	if err != nil || !ok || got != nqn {
		return false
	}
	read, err := s.host.ReadSubsys(ctx, dir, agent.SubsysReadOpts{
		Ctrls:   agent.CtrlHostNqnEach,
		HostNqn: hostNqn,
		Rule:    agent.ReadStrict,
	})
	if err != nil {
		return false
	}
	return slices.Contains(read.HostNqnCtrls, ctrl)
}

// startInRegistry is the registry half of startDisconnect and
// startDisconnectCtrls: it registers nqn, unless a disconnect of it is in
// flight already, and runs run from a goroutine on rootCtx once a slot is
// free, the entry living exactly as long as the goroutine.
func (s *CnAgentServer) startInRegistry(
	ctx context.Context,
	nqn string,
	run func(bgCtx context.Context),
) {
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
		run(bgCtx)
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
// fails for the pass. Once the goroutine has gone, a cntlr's read names each
// side, or clone source endpoint, the subsystem holds no controller for
// (sidesToConnect, cloneSource.toName; cnagent.md CN30), the worker re-sends
// the request (dnv-worker.md RW4), and that converge takes a walk of its
// own, reads the subsystem's entry, directory and controllers at its lookup,
// and connects whatever is missing. The CN28 probe asks it too, so a read
// reports that step the way the converge does and names nothing for the
// subsystem while the disconnect runs.
func (s *CnAgentServer) disconnectInFlight(nqn string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.disconnecting[nqn]
	return ok
}
