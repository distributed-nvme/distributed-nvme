package cnagent

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// ---------------------------------------------------------------------------
// CN34 — the fence watch: an old primary notices its fence
//
// A failover fences the old primary on every side: each disk node puts its
// export to that cntlr's controller node over a dm-error and moves it to
// the standby's ANA group, non-optimized (dnagent.md DN10). The old primary
// hears nothing of it while its network is cut or its agent is stopped, and
// its namespaces still read optimized: a host still on its path gets IO
// errors for its writes and, from a thin pool that maps no block for it,
// zeros for its reads of a block written through the new primary. A disk
// node gives a primary's path the standby group only when the role moved,
// so a primary that has served and that reads no leg path optimized and one
// non-optimized is fenced. The fence watch reads that, once a second for
// every primary of the node, and moves the cntlr's namespaces to the
// inaccessible group with no converge, so the hosts use the new primary's
// path.
// ---------------------------------------------------------------------------

// fenceWatch is one primary cntlr's watch (CN34): the cntlr's key and
// state, the key of its inputs, the legs and namespaces it reads and moves,
// the cntlr's pool devices, and whether it has seen the cntlr serve. Only
// armed changes after it is made; the watch loop and the converge that kept
// the watch (armFenceWatch) set it.
type fenceWatch struct {
	key    string
	st     *cntlrState
	inputs string
	legs   []fenceLeg
	nss    []fenceNs
	pools  []string
	armed  atomic.Bool
}

// fenceLeg is what the watch reads of one leg: its NQN and its provisioned
// sides' addresses.
type fenceLeg struct {
	nqn   string
	addrs map[string]bool
}

// fenceNs is one namespace the watch moves: a host-facing one or a
// transfer's, by its subsystem and index.
type fenceNs struct {
	nqn   string
	nsIdx int
	// devPath is the path of a host-facing namespace's own ns-dev, which
	// the watch reads under a shared NQN (ownsFenceNs); empty for a
	// transfer's, whose NQN no other pool names.
	devPath string
}

// ownsFenceNs says whether the watch may count or move ns: always, but
// under an NQN shared at this tick (CnAgentServer.nqnShared; CN16, A shared
// NQN), where only a namespace whose device is the cntlr's own ns-dev
// counts as served and moves, never another pool's. A read that does not
// answer says no there.
func (s *CnAgentServer) ownsFenceNs(ctx context.Context, ns fenceNs) bool {
	if ns.devPath == "" || !s.nqnShared(ns.nqn) {
		return true
	}
	path, ok, err := s.nvmet.NsDevicePath(ctx, ns.nqn, ns.nsIdx)
	return err == nil && ok && strings.TrimSpace(path) == ns.devPath
}

// fenceInputs are the legs a primary's plan builds, not provisioning, the
// namespaces of its host-facing subsystems and transfers, and its slices'
// pool devices; no legs for a plan that is no primary's or builds no leg.
func fenceInputs(plan *cntlrPlan) ([]fenceLeg, []fenceNs, []string) {
	if plan == nil || !plan.primary || !plan.wantLeg {
		return nil, nil, nil
	}
	var legs []fenceLeg
	for _, lp := range plan.legs {
		if lp.provisioning {
			continue
		}
		leg := fenceLeg{nqn: lp.nqn, addrs: map[string]bool{}}
		for _, side := range lp.sides {
			tr := side.GetNvmeTrConf()
			if side.GetProvisioned() {
				leg.addrs[tr.GetTrAddr()+":"+tr.GetTrSvcId()] = true
			}
		}
		legs = append(legs, leg)
	}
	var nss []fenceNs
	for _, np := range plan.namespaces {
		nss = append(nss, fenceNs{nqn: np.ss.nqn, nsIdx: np.nsIdx,
			devPath: plan.nf.DmPath(np.devName)})
	}
	for _, xp := range plan.xfers {
		nss = append(nss, fenceNs{nqn: xp.nqn,
			nsIdx: int(xp.xfer.GetOriNsIdx())})
	}
	var pools []string
	for _, sp := range plan.slices {
		pools = append(pools, sp.poolFinalName)
	}
	return legs, nss, pools
}

// fenceKey is the key of a watch's inputs: a plan whose legs, sides or
// namespaces change gets a new watch.
func fenceKey(legs []fenceLeg, nss []fenceNs) string {
	var parts []string
	for _, leg := range legs {
		addrs := make([]string, 0, len(leg.addrs))
		for addr := range leg.addrs {
			addrs = append(addrs, addr)
		}
		sort.Strings(addrs)
		parts = append(parts, leg.nqn+"|"+strings.Join(addrs, ","))
	}
	for _, ns := range nss {
		parts = append(parts, ns.nqn+"/"+strconv.Itoa(ns.nsIdx))
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

// keepFenceWatch keeps the cntlr's fence watch in step with plan (CN34):
// one while the plan is a primary's and builds legs, made anew, its arming
// kept, when its inputs change, and none otherwise, with the cntlr's fence
// flag dropped; a nil plan, the cntlr's drop, ends it. The node's watch
// loop starts with the first watch. It reads the plan alone.
func (s *CnAgentServer) keepFenceWatch(
	st *cntlrState,
	key string,
	plan *cntlrPlan,
) {
	legs, nss, pools := fenceInputs(plan)
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.fenceWatches[key]
	if len(legs) == 0 {
		delete(s.fenceWatches, key)
		st.fenced.Store(false)
		return
	}
	inputs := fenceKey(legs, nss)
	if cur != nil && cur.st == st && cur.inputs == inputs {
		return
	}
	w := &fenceWatch{key: key, st: st, inputs: inputs, legs: legs,
		nss: nss, pools: pools}
	if cur != nil && cur.st == st {
		w.armed.Store(cur.armed.Load())
	}
	s.fenceWatches[key] = w
	if !s.fenceLoopStarted {
		s.fenceLoopStarted = true
		go s.fenceWatchLoop(s.rootCtx, s.fenceInterval)
	}
}

// fenceWatchLoop is the node's watch loop (CN34): every interval, one NVMe
// walk of the node, and each watch's tick over it (fenceTick).
func (s *CnAgentServer) fenceWatchLoop(
	ctx context.Context,
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
		s.fenceTickAll(common.WithTraceId(ctx, common.NewTraceId()))
	}
}

// fenceTickAll is one tick of the watch loop over every watch.
func (s *CnAgentServer) fenceTickAll(ctx context.Context) {
	s.mu.Lock()
	watches := make([]*fenceWatch, 0, len(s.fenceWatches))
	for _, w := range s.fenceWatches {
		watches = append(watches, w)
	}
	s.mu.Unlock()
	if len(watches) == 0 {
		return
	}
	walk := s.newPassWalk(ctx)
	for _, w := range watches {
		s.fenceTick(ctx, w, walk)
	}
}

// fenceTick is one watch's tick over walk: a leg path optimized arms the
// watch and drops the cntlr's fence flag; a watch not armed yet arms once a
// namespace of the cntlr reads optimized (fenceServed over the namespaces
// alone; the converge arms it on a pool device, armFenceWatch); and an
// armed watch that reads the fence moves the namespaces (fenceNamespaces),
// on every tick while the fence reads so, so a move that failed is made
// again.
func (s *CnAgentServer) fenceTick(
	ctx context.Context,
	w *fenceWatch,
	walk *passWalk,
) {
	read := s.fencePicture(ctx, walk, w.legs)
	if read == fenceOpen {
		w.armed.Store(true)
		w.st.fenced.Store(false)
		return
	}
	// The tick reads the namespaces alone, which runs no command: a pool
	// device of the cntlr's own arms the watch from the converge that built
	// or found it (armFenceWatch).
	if !w.armed.Load() && s.fenceServed(ctx, w.nss, nil) {
		w.armed.Store(true)
	}
	if read == fenceShown && w.armed.Load() {
		s.fenceNamespaces(ctx, w)
	}
}

// The three answers of fencePicture.
type fenceRead int

const (
	// fenceNone: neither picture, or a read that did not answer.
	fenceNone fenceRead = iota
	// fenceOpen: a leg path is live and reads optimized: the cntlr serves.
	fenceOpen
	// fenceShown: no leg path reads optimized, and one is live and reads
	// non-optimized, the standby group, which a disk node gives a primary's
	// path only when the role moved.
	fenceShown
)

// fencePicture reads the legs in walk. The first live optimized path
// answers fenceOpen at once, so a serving primary reads one leg. A lookup
// that did not answer, or a controller whose address read did not, which
// may be any side's path (CN10), makes the answer fenceNone unless another
// leg answers fenceOpen. A path is a controller at one of the leg's sides'
// addresses.
func (s *CnAgentServer) fencePicture(
	ctx context.Context,
	walk *passWalk,
	legs []fenceLeg,
) fenceRead {
	unknown, standby := false, false
	for _, leg := range legs {
		view, err := s.lookupSubsys(ctx, walk, leg.nqn, sideNsid)
		if err != nil {
			unknown = true
			continue
		}
		for _, ctrl := range view.ctrls {
			if ctrl.addrUnknown != nil {
				unknown = true
				continue
			}
			if ctrl.gone || !leg.addrs[ctrl.trAddr+":"+ctrl.trSvcId] ||
				ctrl.state != "live" {
				continue
			}
			switch ctrl.anaState {
			case "optimized":
				return fenceOpen
			case "non-optimized":
				standby = true
			}
		}
	}
	if unknown || !standby {
		return fenceNone
	}
	return fenceShown
}

// armFenceWatch arms the fence watch of the cntlr key names, as this
// converge kept it (keepFenceWatch), once the cntlr is seen to have served
// (fenceServed): the converge reads its pool devices, which the watch's
// ticks do not, so a primary whose stack stands is armed after an agent's
// restart too, though its namespaces do not serve. A watch armed already is
// left. The read runs on the pass's context, so a pass cut off runs no
// command after the cut: a read that fails or is cut off leaves the watch
// unarmed until the next converge.
func (s *CnAgentServer) armFenceWatch(ctx context.Context, key string) {
	s.mu.Lock()
	w := s.fenceWatches[key]
	s.mu.Unlock()
	if w == nil || w.armed.Load() {
		return
	}
	if s.fenceServed(ctx, w.nss, w.pools) {
		w.armed.Store(true)
	}
}

// fenceServed says whether the cntlr has served: a namespace of nss in the
// optimized group, or a pool device of pools there, which a standby never
// holds (CN13) and which an agent's restart leaves in place. A read that
// does not answer says no.
func (s *CnAgentServer) fenceServed(
	ctx context.Context,
	nss []fenceNs,
	pools []string,
) bool {
	for _, ns := range nss {
		grpId, present, err := s.nvmet.NsAnaGrpId(ctx, ns.nqn, ns.nsIdx)
		if err == nil && present && grpId == common.AnaGrpIdOptimized &&
			s.ownsFenceNs(ctx, ns) {
			return true
		}
	}
	for _, pool := range pools {
		if dev, err := s.dm.Info(ctx, pool); err == nil && dev != nil {
			return true
		}
	}
	return false
}

// fenceNamespaces is the move of an armed watch that read the fence (CN34):
// under the node read lock and the cntlr's object lock, each taken only when
// free, so the move never waits behind a converge or a read and a held lock
// leaves it to the next tick, and only for a cntlr the agent still holds, it
// reads the legs again in a walk of its own, sets the cntlr's fence flag,
// which the plan of every later converge and read takes (applyFence), and
// moves each namespace that is not inaccessible to the inaccessible group.
// A move that fails is logged and made again at the next tick.
func (s *CnAgentServer) fenceNamespaces(ctx context.Context, w *fenceWatch) {
	if !s.locks.Node().TryRLock() {
		return
	}
	defer s.locks.Node().RUnlock()
	if ctx.Err() != nil || s.getCntlr(w.key) != w.st {
		return
	}
	objLock := s.locks.Obj(w.key)
	if !objLock.TryLock() {
		return
	}
	defer objLock.Unlock()
	if s.fencePicture(ctx, s.newPassWalk(ctx), w.legs) != fenceShown {
		return
	}
	w.st.fenced.Store(true)
	moved := 0
	for _, ns := range w.nss {
		grpId, present, err := s.nvmet.NsAnaGrpId(ctx, ns.nqn, ns.nsIdx)
		if err == nil && (!present || grpId == common.AnaGrpIdInaccessible) {
			continue
		}
		if !s.ownsFenceNs(ctx, ns) {
			continue
		}
		if s.moveDark(ctx, ns.nqn, ns.nsIdx) {
			moved++
		}
	}
	if moved > 0 {
		slog.WarnContext(ctx, msgFenceNoticed,
			slog.String("cntlr", w.key),
			slog.Int("legs", len(w.legs)),
			slog.Int("moved", moved))
	}
}

// msgFenceNoticed is the Warn record of a primary whose fence watch read
// its fence and moved its namespaces to the inaccessible group with no
// converge (CN34): its cntlr, its legs and how many namespaces moved.
const msgFenceNoticed = "fence noticed: namespaces moved to the inaccessible group"

// fenceViews is what a primary's converge read of its legs' paths in its
// leg step (ensureLegs), for its own read of the fence (fenceAfterLegs):
// a live optimized path, a live non-optimized one, and a leg whose lookup,
// or a controller's address read, did not answer.
type fenceViews struct {
	open, standby, unknown bool
}

// add takes one leg's lookup into the read; a leg that failed its step,
// view nil, is unknown.
func (f *fenceViews) add(lp *legPlan, view *subsysView) {
	if view == nil {
		f.unknown = true
		return
	}
	addrs := map[string]bool{}
	for _, side := range lp.sides {
		tr := side.GetNvmeTrConf()
		if side.GetProvisioned() {
			addrs[tr.GetTrAddr()+":"+tr.GetTrSvcId()] = true
		}
	}
	for _, ctrl := range view.ctrls {
		if ctrl.addrUnknown != nil {
			f.unknown = true
			continue
		}
		if ctrl.gone || !addrs[ctrl.trAddr+":"+ctrl.trSvcId] ||
			ctrl.state != "live" {
			continue
		}
		switch ctrl.anaState {
		case "optimized":
			f.open = true
		case "non-optimized":
			f.standby = true
		}
	}
}

// fenceAfterLegs is a primary's converge's own read of the fence (CN34),
// from the lookups of its leg step, before its build moves any namespace:
// a cntlr that reads the fence (fencePicture's rule) and has served
// (fenceServed) gets its fence flag set, and the pass's plan then wants
// every namespace inaccessible (applyFence). Reason: an agent that restarts
// after its watch moved the namespaces forgets the flag, and its startup
// converge would move them back to optimized before a watch reads
// anything. A new primary whose disk nodes have not opened its paths has
// not served, and is not judged.
func (s *CnAgentServer) fenceAfterLegs(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	read fenceViews,
) {
	if !plan.primary || plan.fenced || read.open || read.unknown ||
		!read.standby {
		return
	}
	_, nss, pools := fenceInputs(plan)
	if !s.fenceServed(ctx, nss, pools) {
		return
	}
	st.fenced.Store(true)
	applyFence(plan, st)
}

// applyFence makes plan want every namespace and transfer in the
// inaccessible group while the cntlr's fence flag is set (CN34), so a
// converge and a read of a fenced primary agree with the watch's move. The
// flag is read once, for the whole pass.
func applyFence(plan *cntlrPlan, st *cntlrState) {
	if !plan.primary || !st.fenced.Load() {
		return
	}
	plan.fenced = true
	for _, np := range plan.namespaces {
		np.anaGrpId = common.AnaGrpIdInaccessible
	}
}
