package worker

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
	"github.com/distributed-nvme/distributed-nvme/integtest/plant"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The fault schedule (dnv-worker.md, Integration test plan, The fault
// schedule): one whole-worker case whose faults a seed draws. It runs only
// when the environment variable faultSeedEnv names a seed, a list of seeds
// split by commas, or a range "a-b", each seed a parallel subtest, so make
// test skips it; faultStepsEnv sets the number of steps. A run logs the seed
// and every step it took, so a failure can be read and the seed run again:
// the seed fixes the draws, not the timing of the real clock and the real
// etcd, nor AR1's random first-pass wait.
//
// The cluster is the standard one with five disk nodes and four controller
// nodes, its pool sp0 and a second pool sp1 planted beside it on other
// nodes, both converged under three whole workers. Each step draws one
// action and acts on what the state offers: a worker crashed, stopped or
// started again; a fault set or cleared on a fake, of the kinds a real agent
// gives — every row of a cntlr ERROR, the primary's data group row or a
// leg's row ERROR, a side's rows ERROR, an object that answers no round, an
// agent whose saves fail, an agent stopped and started again on its files;
// a cntlr disabled or enabled; a pool's level moved between the levels where
// reactions run and one where they stop; an operator's repair created,
// aborted, finished or cancelled, the primary's verdicts on a repair given;
// and a migration created, cancelled or finished. After each step the four
// rules of faultcheck_test.go are read for every write since the last. At
// the end every fault is cleared, every worker, agent and cntlr is back,
// every repair and migration ended, and both pools must converge.

const (
	// faultSeedEnv names the seeds; unset, the case is skipped.
	faultSeedEnv = "DNV_FAULT_SEED"
	// faultStepsEnv overrides faultSteps.
	faultStepsEnv = "DNV_FAULT_STEPS"
	// faultSteps is a run's number of steps.
	faultSteps = 60
	// faultCoverSeeds is the number of seeds from which a run fails on a
	// kind of action none of its steps took effect for: the 20 seeds this
	// case is run on take every kind several times, where a few seeds may
	// draw a rare chain of steps not at all.
	faultCoverSeeds = 20
	// faultWorkerCnt workers run at the start; a step never crashes or stops
	// the last one running.
	faultWorkerCnt = 3
	faultDnCnt     = 5
	faultCnCnt     = 4
	// The second pool's id and shard.
	faultSp1Id    = uint64(2)
	faultSp1Shard = uint32(0x21)
)

// TestWholeWorkerFaultSchedule runs the fault schedule for each seed
// faultSeedEnv names.
func TestWholeWorkerFaultSchedule(t *testing.T) {
	raw := os.Getenv(faultSeedEnv)
	if raw == "" {
		t.Skipf("%s is not set: the fault schedule runs on demand only", faultSeedEnv)
	}
	t.Parallel()
	seeds, err := parseFaultSeeds(raw)
	if err != nil {
		t.Fatalf("%s=%q: %v", faultSeedEnv, raw, err)
	}
	steps := faultSteps
	if s := os.Getenv(faultStepsEnv); s != "" {
		if steps, err = strconv.Atoi(s); err != nil || steps < 1 {
			t.Fatalf("%s=%q is no step count", faultStepsEnv, s)
		}
	}
	// The steps of each kind that found a target and those whose act took
	// effect, over every seed, logged once every seed has ended: a kind that
	// never took effect is named, since a run that never makes an action
	// happen proves nothing of it, and a run of faultCoverSeeds seeds or
	// more fails on one.
	var mu sync.Mutex
	found := make(map[string]int)
	took := make(map[string]int)
	t.Cleanup(func() {
		var never []string
		for _, k := range faultKinds {
			if took[k.name] == 0 {
				never = append(never, k.name)
			}
		}
		t.Logf("%d seeds: steps that found a target, by kind: %v; steps "+
			"that took effect: %v; kinds that never took effect: %v",
			len(seeds), found, took, never)
		if len(never) > 0 && len(seeds) >= faultCoverSeeds {
			t.Errorf("%d seeds took no step of %v", len(seeds), never)
		}
	})
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			t.Parallel()
			fs := newFaultSched(t, seed)
			fs.run(t, steps)
			mu.Lock()
			defer mu.Unlock()
			for name, n := range fs.found {
				found[name] += n
			}
			for name, n := range fs.took {
				took[name] += n
			}
		})
	}
}

// parseFaultSeeds reads "7", "1,5,9" or "1-20".
func parseFaultSeeds(raw string) ([]uint64, error) {
	var seeds []uint64
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.ParseUint(lo, 10, 64)
		if err != nil {
			return nil, err
		}
		b := a
		if isRange {
			if b, err = strconv.ParseUint(hi, 10, 64); err != nil {
				return nil, err
			}
		}
		if b < a {
			return nil, fmt.Errorf("range %q runs backwards", part)
		}
		for s := a; s <= b; s++ {
			seeds = append(seeds, s)
		}
	}
	return seeds, nil
}

// faultObj is one object on one fake: its fake's address and its key there.
type faultObj struct {
	addr string
	key  string
}

// faultLever is one standing fault: the objects it was set on and how to
// take it off.
type faultLever struct {
	desc  string
	objs  []faultObj
	clear func(o *agentfake.Object)
}

// faultSched is one run of the schedule.
type faultSched struct {
	seed  uint64
	rng   *rand.Rand
	h     *workerHarness
	c     *stdCluster
	pools []plant.SpRef
	check *faultCheck
	// workers holds the current incarnation of each worker by its slot.
	workers []*harnessWorker
	// levers are the standing faults by their description; stopped the
	// fakes stopped, by address.
	levers  map[string]*faultLever
	stopped map[string]bool
	// verdicts are the cntlr objects that hold a repair verdict, and
	// thinOk those armed with the thin_ok lever (armThin).
	verdicts map[faultObj]bool
	thinOk   map[faultObj]bool
	// log is every step taken, with its outcome; found counts the steps of
	// each kind that found a target, took those whose act took effect, and
	// refused is set by an act whose op or scan refused it (outcome).
	log     []string
	found   map[string]int
	took    map[string]int
	refused bool
	// coin is the step's draw beyond its pick, for an act that makes a
	// choice of its own.
	coin uint64
}

// newFaultSched plants the cluster, starts the workers and waits until both
// pools converged.
func newFaultSched(t *testing.T, seed uint64) *faultSched {
	t.Helper()
	// A worker may fence itself under the schedule's load (VW8): that is a
	// safe answer, not a broken rule, so the harness allows it.
	h := newWorkerHarness(t, harnessOpts{allowFence: true})
	c := h.plantStd(t, func(s *stdSpec) {
		s.dnCnt = faultDnCnt
		s.cnCnt = faultCnCnt
	})
	fs := &faultSched{
		seed:     seed,
		rng:      rand.New(rand.NewPCG(seed, seed^0x5bd1e995)),
		h:        h,
		c:        c,
		levers:   make(map[string]*faultLever),
		stopped:  make(map[string]bool),
		verdicts: make(map[faultObj]bool),
		thinOk:   make(map[faultObj]bool),
		found:    make(map[string]int),
		took:     make(map[string]int),
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("seed %d: the steps taken:\n%s", seed, strings.Join(fs.log, "\n"))
		}
	})
	fs.check = startFaultCheck(t, h)
	fs.pools = []plant.SpRef{c.sp.Ref, fs.plantSp1(t)}
	fs.armThin(t)
	for i := range faultWorkerCnt {
		fs.workers = append(fs.workers, h.startWorker(t, fmt.Sprintf("w%d", i+1)))
	}
	fs.waitShared(t)
	fs.waitConverged(t)
	return fs
}

// plantSp1 plants the second pool: the standard pool's shape, its cntlrs on
// cn 2, the primary, and cn 3, its legs on dn 2 and dn 3, with a thin device
// and a subsystem of its own.
func (fs *faultSched) plantSp1(t *testing.T) plant.SpRef {
	t.Helper()
	ctx := context.Background()
	c := fs.c
	spec := plant.SpSpec{
		Name:      "sp1",
		Id:        faultSp1Id,
		Shard:     faultSp1Shard,
		Slots:     []uint32{0, 1, 2},
		Threshold: c.spec.threshold,
		Cntlrs: []plant.CntlrSpec{
			{CntlrId: 1, CnId: 2, Slot: 0, Primary: true},
			{CntlrId: 2, CnId: 3, Slot: 1},
		},
		Slices: []plant.SliceSpec{{SliceId: stdSliceId, SliceIdx: 0}},
		Groups: []plant.GroupSpec{
			{SliceId: stdSliceId, GrpId: stdMetaGrpId, Meta: true, ExtCnt: 1, Raid1: true},
			{SliceId: stdSliceId, GrpId: stdDataGrpId, ExtCnt: 2, Raid1: true},
		},
		Legs: []plant.LegSpec{
			{GrpId: stdMetaGrpId, LegId: 3, LegIdx: 0},
			{GrpId: stdMetaGrpId, LegId: 4, LegIdx: 1},
			{GrpId: stdDataGrpId, LegId: 6, LegIdx: 0},
			{GrpId: stdDataGrpId, LegId: 7, LegIdx: 1},
		},
		Sides: []plant.SideSpec{
			{LegId: 3, SideId: 8, DnId: 2}, {LegId: 4, SideId: 9, DnId: 3},
			{LegId: 6, SideId: 10, DnId: 2}, {LegId: 7, SideId: 11, DnId: 3},
		},
	}
	sp, err := plant.PutSp(ctx, fs.h.cli, c.cl, spec)
	if err != nil {
		t.Fatalf("PutSp sp1: %v", err)
	}
	td, err := plant.PutTd(ctx, fs.h.cli, c.cl, sp.Ref, plant.TdSpec{
		Name: "td0", Size: 1 << 30,
	})
	if err != nil {
		t.Fatalf("PutTd sp1: %v", err)
	}
	if _, err := plant.PutSs(ctx, fs.h.cli, c.cl, sp.Ref, plant.SsSpec{
		Nqn:        "nqn.2026-10.io.dnv-wh:ss1",
		Serial:     "s1",
		Namespaces: []plant.NsSpec{{NsIdx: 1, TdId: td.Id}},
	}); err != nil {
		t.Fatalf("PutSs sp1: %v", err)
	}
	return sp.Ref
}

// ---------------------------------------------------------------------------
// Reading the state
// ---------------------------------------------------------------------------

// load reads one pool as the coordinator does.
func (fs *faultSched) load(t *testing.T, ref plant.SpRef) *model.SpState {
	t.Helper()
	state, err := model.LoadSp(context.Background(), fs.h.cli, fs.c.cl.Id, ref.Name)
	if err != nil {
		t.Fatalf("LoadSp %s: %v", ref.Name, err)
	}
	return state
}

// cntlrObjs are the fake objects of a pool's cntlrs as its record holds
// them.
func cntlrObjs(ref plant.SpRef, state *model.SpState) []faultObj {
	var out []faultObj
	for _, id := range state.Conf.GetCntlrIdList() {
		out = append(out, faultObj{
			addr: state.Cntlrs[id].GetAddrPort(),
			key:  agentfake.CntlrKey(ref.Id, id),
		})
	}
	return out
}

// primaryOf is a pool's primary cntlr_id, 0 when none is.
func primaryOf(state *model.SpState) uint64 {
	for _, id := range state.Conf.GetCntlrIdList() {
		if state.Cntlrs[id].GetPrimary() {
			return id
		}
	}
	return 0
}

// sliceLegs are the legs of a pool's slices with the group of each.
func sliceLegs(state *model.SpState) []struct {
	grp *pb.Group
	leg *pb.Leg
} {
	var out []struct {
		grp *pb.Group
		leg *pb.Leg
	}
	for _, sliceId := range state.Conf.GetSliceIdList() {
		slice := state.Slices[sliceId]
		for _, grp := range slices.Concat(slice.GetMetaGrpList(), slice.GetDataGrpList()) {
			for _, leg := range grp.GetLegList() {
				out = append(out, struct {
					grp *pb.Group
					leg *pb.Leg
				}{grp, leg})
			}
		}
	}
	return out
}

// grpDns are the disk nodes of a group by the rule of architecture.md,
// Per-operation allocation: every side of its legs and of its repair's new
// leg.
func grpDns(state *model.SpState, grp *pb.Group) []string {
	var out []string
	for _, leg := range grp.GetLegList() {
		for _, side := range leg.GetSideList() {
			out = append(out, side.GetAddrPort())
		}
	}
	if rep := state.Repairs[grp.GetRepName()]; rep != nil {
		for _, leg := range rep.GetNewLegList() {
			for _, side := range leg.GetSideList() {
				out = append(out, side.GetAddrPort())
			}
		}
	}
	return out
}

// faultDnAddrs and faultCnAddrs are every node's fake.
func faultDnAddrs() []string {
	var out []string
	for id := uint64(1); id <= faultDnCnt; id++ {
		out = append(out, stdDnAddr(id))
	}
	return out
}

func faultCnAddrs() []string {
	var out []string
	for id := uint64(1); id <= faultCnCnt; id++ {
		out = append(out, stdCnAddr(id))
	}
	return out
}

// ---------------------------------------------------------------------------
// The run
// ---------------------------------------------------------------------------

// run takes the steps and then settles the cluster.
func (fs *faultSched) run(t *testing.T, steps int) {
	t.Helper()
	t.Logf("seed %d: %d steps", fs.seed, steps)
	for step := 1; step <= steps; step++ {
		// Every step draws the same three numbers, whatever the state, so a
		// seed draws one sequence of actions: the kind, the pick among the
		// targets the state offers, and the time to the next step.
		kind := fs.drawKind()
		pick := fs.rng.Uint64()
		gap := time.Duration(200+fs.rng.IntN(2300)) * time.Millisecond
		fs.armThin(t)
		what := fs.act(t, kind, pick)
		line := fmt.Sprintf("step %d: %s: %s", step, kind.name, what)
		fs.log = append(fs.log, line)
		t.Logf("seed %d %s", fs.seed, line)
		// The gap is drawn, not a wait for an outcome: the rules hold at
		// every instant, so the schedule acts at the moments it draws.
		time.Sleep(gap)
		if found := fs.check.sync(t, fs.h); len(found) > 0 {
			t.Fatalf("seed %d, by step %d (%s), the rules broke:\n%s", fs.seed,
				step, line, strings.Join(found, "\n"))
		}
	}
	fs.settle(t)
	if found := fs.check.sync(t, fs.h); len(found) > 0 {
		t.Fatalf("seed %d, while settling, the rules broke:\n%s", fs.seed,
			strings.Join(found, "\n"))
	}
	writes, rev := fs.check.read()
	reactions := make(map[string]int)
	for _, rec := range fs.h.records(nil, msgReactionApplied) {
		reactions[rec.str("kind")]++
	}
	t.Logf("seed %d: settled after %d steps; %d writes checked, to store "+
		"revision %d; steps that found a target %v; steps that took effect "+
		"%v; reactions applied %v; %d fences",
		fs.seed, steps, writes, rev, fs.found, fs.took, reactions,
		len(fs.h.records(nil, msgWorkerFenced)))
}

// faultKind is one kind of action, with its weight in the draw and what it
// does on the state as it is.
type faultKind struct {
	name   string
	weight int
	// targets lists what the action can act on now, each with its act.
	targets func(fs *faultSched, t *testing.T) []faultTarget
}

// faultTarget is one target of an action: what it is and its act, which
// returns the outcome to log.
type faultTarget struct {
	desc string
	act  func(t *testing.T) string
}

// faultKinds are the schedule's actions, in a fixed order.
var faultKinds = []faultKind{
	{"crash a worker", 3, (*faultSched).crashTargets},
	{"stop a worker", 2, (*faultSched).stopTargets},
	{"start a worker", 4, (*faultSched).startTargets},
	{"fail a cntlr", 3, (*faultSched).failCntlrTargets},
	{"fail the primary's data group", 3, (*faultSched).failPrimaryTargets},
	{"fail a leg", 4, (*faultSched).failLegTargets},
	{"fail a side", 2, (*faultSched).failSideTargets},
	{"hang an object", 2, (*faultSched).hangTargets},
	{"refuse saves", 2, (*faultSched).refuseTargets},
	{"clear a fault", 8, (*faultSched).clearTargets},
	{"stop an agent", 2, (*faultSched).stopAgentTargets},
	{"start an agent", 3, (*faultSched).startAgentTargets},
	{"disable a cntlr", 2, (*faultSched).disableTargets},
	{"enable a cntlr", 3, (*faultSched).enableTargets},
	{"set a level", 2, (*faultSched).levelTargets},
	{"create a repair", 6, (*faultSched).createRepairTargets},
	{"abort a repair", 4, (*faultSched).abortRepairTargets},
	{"give a verdict", 5, (*faultSched).verdictTargets},
	{"finish a repair", 5, (*faultSched).finishRepairTargets},
	{"cancel a repair", 6, (*faultSched).cancelRepairTargets},
	{"create a migration", 3, (*faultSched).createMigrTargets},
	{"cancel a migration", 5, (*faultSched).cancelMigrTargets},
	{"finish a migration", 4, (*faultSched).finishMigrTargets},
}

// drawKind draws a kind by weight.
func (fs *faultSched) drawKind() faultKind {
	total := 0
	for _, k := range faultKinds {
		total += k.weight
	}
	n := fs.rng.IntN(total)
	for _, k := range faultKinds {
		if n < k.weight {
			return k
		}
		n -= k.weight
	}
	return faultKinds[len(faultKinds)-1]
}

// act runs one step: the pick among the targets the kind offers now, or
// nothing when it offers none.
func (fs *faultSched) act(t *testing.T, kind faultKind, pick uint64) string {
	t.Helper()
	targets := kind.targets(fs, t)
	if len(targets) == 0 {
		return "nothing to act on"
	}
	target := targets[pick%uint64(len(targets))]
	fs.coin = pick >> 32
	fs.refused = false
	out := target.act(t)
	fs.found[kind.name]++
	if !fs.refused {
		fs.took[kind.name]++
	}
	if out == "" {
		return target.desc
	}
	return target.desc + ": " + out
}

// outcome is a model op's outcome as the step's log reads it: a refusal
// is an outcome, as a gateway answers one, not a failure of the case, and
// a step that took no effect.
func (fs *faultSched) outcome(err error) string {
	if err != nil {
		fs.refused = true
		return "refused: " + err.Error()
	}
	return "done"
}

// ---------------------------------------------------------------------------
// Workers
// ---------------------------------------------------------------------------

func (fs *faultSched) running() []int {
	var out []int
	for i, w := range fs.workers {
		if w.running() {
			out = append(out, i)
		}
	}
	return out
}

func (fs *faultSched) crashTargets(t *testing.T) []faultTarget {
	run := fs.running()
	if len(run) < 2 {
		return nil
	}
	var out []faultTarget
	for _, i := range run {
		w := fs.workers[i]
		out = append(out, faultTarget{w.name, func(t *testing.T) string {
			w.crash(t)
			return ""
		}})
	}
	return out
}

func (fs *faultSched) stopTargets(t *testing.T) []faultTarget {
	run := fs.running()
	if len(run) < 2 {
		return nil
	}
	var out []faultTarget
	for _, i := range run {
		w := fs.workers[i]
		out = append(out, faultTarget{w.name, func(t *testing.T) string {
			w.stop(t)
			return ""
		}})
	}
	return out
}

func (fs *faultSched) startTargets(t *testing.T) []faultTarget {
	var out []faultTarget
	for i, w := range fs.workers {
		if w.running() {
			continue
		}
		out = append(out, faultTarget{w.name, func(t *testing.T) string {
			// A crashed worker's goroutines may still wind down; the new
			// incarnation does not wait for them, as a restarted process
			// does not.
			fs.workers[i] = fs.h.restart(t, w)
			return ""
		}})
	}
	return out
}

// waitShared waits until the running workers own every shard of every role
// between them.
func (fs *faultSched) waitShared(t *testing.T) {
	t.Helper()
	fs.h.waitUntil(t, 60*time.Second, "the running workers to own every shard",
		func() (bool, string) {
			for _, role := range fs.workers[0].roles {
				n := 0
				for _, i := range fs.running() {
					n += fs.workers[i].ownedCount(role)
				}
				if n != common.ShardBucketSize {
					return false, fmt.Sprintf("%s: %d of %d shards", role, n,
						common.ShardBucketSize)
				}
			}
			return true, ""
		})
}

// ---------------------------------------------------------------------------
// Faults on the fakes
// ---------------------------------------------------------------------------

// armThin sets the thin_ok lever on every cntlr of both pools that does not
// carry it yet, a cntlr made by a replacement included, so that each
// primary reports its thin rows OK and RW19 can flip its thin devices
// created. The fake reports them OK even while a slice waits for its sides
// or the level holds no pool, where the cn agent reports PROVISIONING or
// MISSING (cnagent.md CN14, CN19): the created flip may then come early,
// which none of the four rules reads.
func (fs *faultSched) armThin(t *testing.T) {
	t.Helper()
	for _, ref := range fs.pools {
		for _, obj := range cntlrObjs(ref, fs.load(t, ref)) {
			if fs.thinOk[obj] {
				continue
			}
			setLever(t, fs.h.fleet.Agent(obj.addr), obj.key,
				func(o *agentfake.Object) { o.ThinOk = true })
			fs.thinOk[obj] = true
		}
	}
}

// setFault sets a standing fault on objs, under a description no standing
// fault holds.
func (fs *faultSched) setFault(
	t *testing.T,
	desc string,
	objs []faultObj,
	set, clear func(o *agentfake.Object),
) {
	t.Helper()
	for _, obj := range objs {
		setLever(t, fs.h.fleet.Agent(obj.addr), obj.key, set)
	}
	fs.levers[desc] = &faultLever{desc: desc, objs: objs, clear: clear}
}

// faultTargetsOf offers a fault on each object of objs that carries none of
// this kind: the description names the kind and the object.
func (fs *faultSched) faultTargetsOf(
	kind string,
	objs map[string][]faultObj,
	set, clear func(o *agentfake.Object),
) []faultTarget {
	var names []string
	for name := range objs {
		names = append(names, name)
	}
	slices.Sort(names)
	var out []faultTarget
	for _, name := range names {
		desc := kind + " " + name
		if fs.levers[desc] != nil {
			continue
		}
		list := objs[name]
		out = append(out, faultTarget{desc, func(t *testing.T) string {
			fs.setFault(t, desc, list, set, clear)
			return ""
		}})
	}
	return out
}

func (fs *faultSched) failCntlrTargets(t *testing.T) []faultTarget {
	objs := make(map[string][]faultObj)
	for _, ref := range fs.pools {
		for _, obj := range cntlrObjs(ref, fs.load(t, ref)) {
			objs[fmt.Sprintf("%s %s", ref.Name, obj.key)] = []faultObj{obj}
		}
	}
	return fs.faultTargetsOf("error rows on", objs,
		func(o *agentfake.Object) { o.Status = statusPtr(pb.ResStatus_RES_STATUS_ERROR) },
		func(o *agentfake.Object) { o.Status = nil })
}

// failPrimaryTargets set the data group's md row ERROR on a pool's current
// primary while it is the primary, a fault of its own (HL2) that stops
// biting once it is demoted.
func (fs *faultSched) failPrimaryTargets(t *testing.T) []faultTarget {
	objs := make(map[string][]faultObj)
	row := fmt.Sprintf("grp_id_to_md_raid.%d", stdDataGrpId)
	for _, ref := range fs.pools {
		state := fs.load(t, ref)
		id := primaryOf(state)
		if id == 0 {
			continue
		}
		objs[fmt.Sprintf("%s cntlr %d", ref.Name, id)] = []faultObj{{
			addr: state.Cntlrs[id].GetAddrPort(), key: agentfake.CntlrKey(ref.Id, id),
		}}
	}
	return fs.faultTargetsOf("data group row on", objs,
		func(o *agentfake.Object) {
			if o.Rows == nil {
				o.Rows = make(map[string]agentfake.Row)
			}
			o.Rows[row] = rowLever(pb.ResStatus_RES_STATUS_ERROR, "", true)
		},
		func(o *agentfake.Object) { delete(o.Rows, row) })
}

// failLegTargets set one leg's row ERROR on every cntlr of its pool while
// it is the primary, so the row bites on whichever cntlr the pool's primary
// is.
func (fs *faultSched) failLegTargets(t *testing.T) []faultTarget {
	var out []faultTarget
	for _, ref := range fs.pools {
		state := fs.load(t, ref)
		objs := cntlrObjs(ref, state)
		for _, gl := range sliceLegs(state) {
			row := fmt.Sprintf("leg_id_to_leg.%d", gl.leg.GetLegId())
			out = append(out, fs.faultTargetsOf("leg row",
				map[string][]faultObj{fmt.Sprintf("%s leg %d", ref.Name,
					gl.leg.GetLegId()): objs},
				func(o *agentfake.Object) {
					if o.Rows == nil {
						o.Rows = make(map[string]agentfake.Row)
					}
					o.Rows[row] = rowLever(pb.ResStatus_RES_STATUS_ERROR, "", true)
				},
				func(o *agentfake.Object) { delete(o.Rows, row) })...)
		}
	}
	return out
}

// sideObjs are the fake objects of every side of a pool's slices.
func sideObjs(ref plant.SpRef, state *model.SpState) map[string][]faultObj {
	objs := make(map[string][]faultObj)
	for _, gl := range sliceLegs(state) {
		for _, side := range gl.leg.GetSideList() {
			objs[fmt.Sprintf("%s side %d", ref.Name, side.GetSideId())] = []faultObj{{
				addr: side.GetAddrPort(),
				key:  agentfake.SideKey(ref.Id, gl.leg.GetLegId(), side.GetSideId()),
			}}
		}
	}
	return objs
}

func (fs *faultSched) failSideTargets(t *testing.T) []faultTarget {
	objs := make(map[string][]faultObj)
	for _, ref := range fs.pools {
		for name, list := range sideObjs(ref, fs.load(t, ref)) {
			objs[name] = list
		}
	}
	return fs.faultTargetsOf("error rows on", objs,
		func(o *agentfake.Object) { o.Status = statusPtr(pb.ResStatus_RES_STATUS_ERROR) },
		func(o *agentfake.Object) { o.Status = nil })
}

// hangTargets hold an object's rounds: a node, a side or a cntlr whose
// agent stops answering it.
func (fs *faultSched) hangTargets(t *testing.T) []faultTarget {
	objs := make(map[string][]faultObj)
	for _, addr := range faultDnAddrs() {
		objs["dn "+addr] = []faultObj{{addr, agentfake.DnKey}}
	}
	for _, addr := range faultCnAddrs() {
		objs["cn "+addr] = []faultObj{{addr, agentfake.CnKey}}
	}
	for _, ref := range fs.pools {
		state := fs.load(t, ref)
		for name, list := range sideObjs(ref, state) {
			objs[name] = list
		}
		for _, obj := range cntlrObjs(ref, state) {
			objs[fmt.Sprintf("%s %s", ref.Name, obj.key)] = []faultObj{obj}
		}
	}
	return fs.faultTargetsOf("rounds held on", objs,
		func(o *agentfake.Object) { o.Hang = true },
		func(o *agentfake.Object) { o.Hang = false })
}

// refuseTargets make a side's or a cntlr's agent refuse its syncups as a
// failed save does (HL9).
func (fs *faultSched) refuseTargets(t *testing.T) []faultTarget {
	objs := make(map[string][]faultObj)
	for _, ref := range fs.pools {
		state := fs.load(t, ref)
		for name, list := range sideObjs(ref, state) {
			objs[name] = list
		}
		for _, obj := range cntlrObjs(ref, state) {
			objs[fmt.Sprintf("%s %s", ref.Name, obj.key)] = []faultObj{obj}
		}
	}
	return fs.faultTargetsOf("failed saves on", objs,
		func(o *agentfake.Object) { o.SyncupCode = common.ReplyCodeSaveFailed },
		func(o *agentfake.Object) { o.SyncupCode = 0 })
}

func (fs *faultSched) clearTargets(t *testing.T) []faultTarget {
	var descs []string
	for desc := range fs.levers {
		descs = append(descs, desc)
	}
	slices.Sort(descs)
	var out []faultTarget
	for _, desc := range descs {
		lever := fs.levers[desc]
		out = append(out, faultTarget{desc, func(t *testing.T) string {
			fs.clearLever(t, lever)
			return ""
		}})
	}
	return out
}

func (fs *faultSched) clearLever(t *testing.T, lever *faultLever) {
	t.Helper()
	for _, obj := range lever.objs {
		setLever(t, fs.h.fleet.Agent(obj.addr), obj.key, lever.clear)
	}
	delete(fs.levers, lever.desc)
}

func (fs *faultSched) stopAgentTargets(t *testing.T) []faultTarget {
	var out []faultTarget
	for _, addr := range slices.Concat(faultDnAddrs(), faultCnAddrs()) {
		if fs.stopped[addr] {
			continue
		}
		out = append(out, faultTarget{addr, func(t *testing.T) string {
			fs.h.fleet.Agent(addr).Stop()
			fs.stopped[addr] = true
			return ""
		}})
	}
	return out
}

func (fs *faultSched) startAgentTargets(t *testing.T) []faultTarget {
	var out []faultTarget
	for _, addr := range slices.Concat(faultDnAddrs(), faultCnAddrs()) {
		if !fs.stopped[addr] {
			continue
		}
		out = append(out, faultTarget{addr, func(t *testing.T) string {
			fs.startAgent(t, addr)
			return ""
		}})
	}
	return out
}

// startAgent starts a stopped fake on its files, as an agent restarted on
// its local files.
func (fs *faultSched) startAgent(t *testing.T, addr string) {
	t.Helper()
	if err := fs.h.fleet.Agent(addr).Start(agentfake.KeepStore); err != nil {
		t.Fatalf("start the fake at %s: %v", addr, err)
	}
	delete(fs.stopped, addr)
}

// ---------------------------------------------------------------------------
// Cntlrs and levels
// ---------------------------------------------------------------------------

func (fs *faultSched) cntlrFlagTargets(t *testing.T, disabled bool) []faultTarget {
	var out []faultTarget
	for _, ref := range fs.pools {
		state := fs.load(t, ref)
		for _, id := range state.Conf.GetCntlrIdList() {
			if state.Cntlrs[id].GetDisabled() == disabled {
				continue
			}
			out = append(out, faultTarget{fmt.Sprintf("%s cntlr %d", ref.Name, id),
				func(t *testing.T) string {
					_, _, err := plant.SetCntlr(context.Background(), fs.h.cli,
						fs.c.cl, ref, id, plant.CntlrFlags{Disabled: &disabled})
					return fs.outcome(err)
				}})
		}
	}
	return out
}

func (fs *faultSched) disableTargets(t *testing.T) []faultTarget {
	return fs.cntlrFlagTargets(t, true)
}

func (fs *faultSched) enableTargets(t *testing.T) []faultTarget {
	return fs.cntlrFlagTargets(t, false)
}

// faultLevels are the levels a step sets: two where reactions run, and one
// where they stop (AR3).
var faultLevels = []pb.SpLevel{
	pb.SpLevel_SP_LEVEL_READWRITE,
	pb.SpLevel_SP_LEVEL_READONLY,
	pb.SpLevel_SP_LEVEL_NO_THINPOOL,
}

func (fs *faultSched) levelTargets(t *testing.T) []faultTarget {
	var out []faultTarget
	for _, ref := range fs.pools {
		now := fs.load(t, ref).Conf.GetSpLevel()
		for _, level := range faultLevels {
			if level == now {
				continue
			}
			out = append(out, faultTarget{fmt.Sprintf("%s to %s", ref.Name, level),
				func(t *testing.T) string {
					_, err := plant.SetLevel(context.Background(), fs.h.cli,
						fs.c.cl, ref, level)
					return fs.outcome(err)
				}})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Repairs
// ---------------------------------------------------------------------------

// createRepairTargets are an operator's repairs, through the op the
// gateway's CreateRepair runs, of a leg whose health epoch is set, its fresh
// new leg on a disk node the scan of architecture.md, Per-operation
// allocation, picks outside the group. A group that holds a repair already
// is offered too: an operator can send that create, and the op must refuse
// it, since a group has one repair at most (architecture.md, Repairs).
func (fs *faultSched) createRepairTargets(t *testing.T) []faultTarget {
	var out []faultTarget
	ctx := context.Background()
	for _, ref := range fs.pools {
		state := fs.load(t, ref)
		for _, gl := range sliceLegs(state) {
			grp, leg := gl.grp, gl.leg
			if leg.GetErrEpoch() == 0 || len(leg.GetSideList()) != 1 {
				continue
			}
			sliceId := uint64(0)
			for _, id := range state.Conf.GetSliceIdList() {
				for _, g := range slices.Concat(state.Slices[id].GetMetaGrpList(),
					state.Slices[id].GetDataGrpList()) {
					if g.GetGrpId() == grp.GetGrpId() {
						sliceId = id
					}
				}
			}
			out = append(out, faultTarget{
				fmt.Sprintf("%s leg %d", ref.Name, leg.GetLegId()),
				func(t *testing.T) string {
					black := grpDns(state, grp)
					var locs []string
					for _, addr := range black {
						locs = append(locs, state.DnByAddr[addr].GetLocation())
					}
					cands, _, err := model.FindDnCandidatesAntiAffine(ctx, fs.h.cli,
						fs.c.cl.Id, fs.c.cl.Conf, grp.GetExtCnt(), 0, 1, 1,
						black, nil, locs)
					if err != nil {
						fs.refused = true
						return "no candidate: " + err.Error()
					}
					if len(cands) == 0 {
						fs.refused = true
						return "no candidate"
					}
					name := fmt.Sprintf("op%d", len(fs.log)+1)
					_, _, err = model.CreateRepair(ctx, fs.h.cli, fs.c.cl.Id,
						ref.Shard, ref.Id, ref.Name, 0, name,
						model.GrpParent{SliceId: sliceId}, grp.GetGrpId(),
						leg.GetLegId(), model.RepairNewLeg{
							Dn: cands[0], GrpDnAddrs: black,
						}, fs.c.cl.Conf)
					return name + " on " + cands[0].AddrPort + " " + fs.outcome(err)
				}})
		}
	}
	return out
}

// repairTargets offers each repair of both pools that keep accepts.
func (fs *faultSched) repairTargets(
	t *testing.T,
	keep func(state *model.SpState, rep *pb.Repair) bool,
	act func(ref plant.SpRef, state *model.SpState, name string, rep *pb.Repair) string,
) []faultTarget {
	var out []faultTarget
	for _, ref := range fs.pools {
		state := fs.load(t, ref)
		for _, name := range state.Conf.GetRepairNameList() {
			rep := state.Repairs[name]
			if rep == nil || !keep(state, rep) {
				continue
			}
			out = append(out, faultTarget{fmt.Sprintf("%s repair %s", ref.Name, name),
				func(t *testing.T) string { return act(ref, state, name, rep) }})
		}
	}
	return out
}

// abortRepairTargets are an operator's aborts of a CREATED repair, a
// worker's or an operator's; the primary confirms the abort at once, as one
// whose new leg holds no place yet does (cnagent.md CN12), so the worker
// moves the repair to ABORTED (AR11), where an operator's repair waits for
// the operator's cancel and a worker's for the drive step's (AR8).
func (fs *faultSched) abortRepairTargets(t *testing.T) []faultTarget {
	return fs.repairTargets(t,
		func(_ *model.SpState, rep *pb.Repair) bool {
			return rep.GetRepStage() == pb.RepStage_REP_STAGE_CREATED && !rep.GetTryToAbort()
		},
		func(ref plant.SpRef, state *model.SpState, name string, _ *pb.Repair) string {
			_, err := model.SetRepairAbort(context.Background(), fs.h.cli,
				fs.c.cl.Id, ref.Shard, ref.Id, ref.Name, 0, name)
			if err == nil {
				fs.setVerdict(t, ref, state, name,
					agentfake.RepairVerdict{AbortConfirmed: true})
			}
			return fs.outcome(err)
		})
}

// newLegReady says whether a repair's new legs have every side provisioned,
// before which no real primary can start the copy.
func newLegReady(rep *pb.Repair) bool {
	for _, leg := range rep.GetNewLegList() {
		for _, side := range leg.GetSideList() {
			if !side.GetProvisioned() {
				return false
			}
		}
	}
	return true
}

// verdictTargets give the primary's verdict on a CREATED repair: abort
// confirmed for one whose flag is set, started for one whose new leg is
// provisioned. The verdict is set on every cntlr of the pool, since only
// the primary reports one (agentfake.Object.Repairs).
func (fs *faultSched) verdictTargets(t *testing.T) []faultTarget {
	return fs.repairTargets(t,
		func(_ *model.SpState, rep *pb.Repair) bool {
			return rep.GetRepStage() == pb.RepStage_REP_STAGE_CREATED &&
				(rep.GetTryToAbort() || newLegReady(rep))
		},
		func(ref plant.SpRef, state *model.SpState, name string, rep *pb.Repair) string {
			verdict := agentfake.RepairVerdict{Started: true}
			if rep.GetTryToAbort() {
				verdict = agentfake.RepairVerdict{AbortConfirmed: true}
			}
			fs.setVerdict(t, ref, state, name, verdict)
			if verdict.Started {
				return "started"
			}
			return "abort confirmed"
		})
}

// setVerdict sets a verdict on a repair on every cntlr of its pool.
func (fs *faultSched) setVerdict(
	t *testing.T,
	ref plant.SpRef,
	state *model.SpState,
	name string,
	verdict agentfake.RepairVerdict,
) {
	t.Helper()
	for _, obj := range cntlrObjs(ref, state) {
		setLever(t, fs.h.fleet.Agent(obj.addr), obj.key, func(o *agentfake.Object) {
			if o.Repairs == nil {
				o.Repairs = make(map[string]agentfake.RepairVerdict)
			}
			o.Repairs[name] = verdict
		})
		fs.verdicts[obj] = true
	}
}

// finishRepairTargets are an operator's finishes, through the op the
// gateway's FinishRepair runs, of a STARTED repair, or of a CREATED one whose
// new leg is provisioned and whose abort no one asked: then the primary
// first answers that the copy started, and the step waits for the worker to
// move the repair to STARTED (AR11) before it finishes it. A worker's
// repair races the worker's own drive step, which finishes it at its next
// pass (AR8): one of the two is refused.
func (fs *faultSched) finishRepairTargets(t *testing.T) []faultTarget {
	return fs.repairTargets(t,
		func(_ *model.SpState, rep *pb.Repair) bool {
			switch rep.GetRepStage() {
			case pb.RepStage_REP_STAGE_STARTED:
				return true
			case pb.RepStage_REP_STAGE_CREATED:
				return !rep.GetTryToAbort() && newLegReady(rep)
			}
			return false
		},
		func(ref plant.SpRef, state *model.SpState, name string, rep *pb.Repair) string {
			if rep.GetRepStage() == pb.RepStage_REP_STAGE_CREATED {
				fs.setVerdict(t, ref, state, name,
					agentfake.RepairVerdict{Started: true})
				if why := fs.awaitStage(t, ref, name,
					pb.RepStage_REP_STAGE_STARTED); why != "" {
					fs.refused = true
					return "started by the primary, " + why
				}
			}
			_, err := model.FinishRepair(context.Background(), fs.h.cli,
				fs.c.cl.Id, ref.Shard, ref.Id, ref.Name, 0, name,
				uint64(time.Now().Unix()), fs.c.cl.Conf)
			return fs.outcome(err)
		})
}

// cancelRepairTargets are an operator's cancels, through the op the
// gateway's CancelRepair runs, of an ABORTED repair, or of a CREATED one
// whose abort was asked: then the primary first confirms the abort, and the
// step waits for the worker to move the repair to ABORTED (AR11) before it
// cancels it. A worker's repair races the worker's own drive step, which
// cancels it at its next pass (AR8): one of the two is refused.
func (fs *faultSched) cancelRepairTargets(t *testing.T) []faultTarget {
	return fs.repairTargets(t,
		func(_ *model.SpState, rep *pb.Repair) bool {
			switch rep.GetRepStage() {
			case pb.RepStage_REP_STAGE_ABORTED:
				return true
			case pb.RepStage_REP_STAGE_CREATED:
				return rep.GetTryToAbort()
			}
			return false
		},
		func(ref plant.SpRef, state *model.SpState, name string, rep *pb.Repair) string {
			if rep.GetRepStage() == pb.RepStage_REP_STAGE_CREATED {
				fs.setVerdict(t, ref, state, name,
					agentfake.RepairVerdict{AbortConfirmed: true})
				if why := fs.awaitStage(t, ref, name,
					pb.RepStage_REP_STAGE_ABORTED); why != "" {
					fs.refused = true
					return "abort confirmed by the primary, " + why
				}
			}
			_, err := model.CancelRepair(context.Background(), fs.h.cli,
				fs.c.cl.Id, ref.Shard, ref.Id, ref.Name, 0, name, fs.c.cl.Conf)
			return fs.outcome(err)
		})
}

// awaitStage waits, for a few passes, until the worker moves a repair to
// stage; "" once it has, else what the repair is instead. A repair the
// worker cannot move — its pool's coordinator not running, its primary
// unreachable — stays where it is, an outcome of the step, not a failure.
func (fs *faultSched) awaitStage(
	t *testing.T, ref plant.SpRef, name string, stage pb.RepStage,
) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rep := fs.load(t, ref).Repairs[name]
		switch {
		case rep == nil:
			return "but the repair is gone"
		case rep.GetRepStage() == stage:
			return ""
		case time.Now().After(deadline):
			return fmt.Sprintf("but the repair stays %s", rep.GetRepStage())
		}
		time.Sleep(10 * harnessPoll)
	}
}

// ---------------------------------------------------------------------------
// Migrations
// ---------------------------------------------------------------------------

// createMigrTargets are migrations, in the shape CreateMigration writes, of
// the one side of a leg whose group has no repair and whose legs carry no
// health epoch and no standing leg fault, to a running disk node outside
// the group (architecture.md, Migrations). The gateway's own checks are
// played here, since plant.PutMigr makes none. Every other one, by the
// step's coin, has its destination zero slowly, as a slow disk does: a
// standing fault until a step clears it, which holds the destination
// unprovisioned and so the cutover pending, the one state a cancel takes.
func (fs *faultSched) createMigrTargets(t *testing.T) []faultTarget {
	var out []faultTarget
	for _, ref := range fs.pools {
		state := fs.load(t, ref)
		if len(state.Conf.GetMigrNameList()) >= common.MaxMigrCntPerSp {
			continue
		}
		for _, gl := range sliceLegs(state) {
			grp, leg := gl.grp, gl.leg
			if grp.GetRepName() != "" || len(leg.GetSideList()) != 1 {
				continue
			}
			calm := true
			for _, other := range grp.GetLegList() {
				desc := fmt.Sprintf("leg row %s leg %d", ref.Name, other.GetLegId())
				if other.GetErrEpoch() != 0 || fs.levers[desc] != nil {
					calm = false
				}
			}
			if !calm {
				continue
			}
			used := grpDns(state, grp)
			var dst uint64
			for id := uint64(1); id <= faultDnCnt; id++ {
				addr := stdDnAddr(id)
				if !slices.Contains(used, addr) && !fs.stopped[addr] {
					dst = id
					break
				}
			}
			if dst == 0 {
				continue
			}
			src := leg.GetSideList()[0]
			slot := uint32(0)
			for _, s := range state.Conf.GetCntlidSlotList() {
				if s != src.GetCntlidSlot() {
					slot = s
					break
				}
			}
			legId := leg.GetLegId()
			out = append(out, faultTarget{
				fmt.Sprintf("%s side %d to dn %d", ref.Name, src.GetSideId(), dst),
				func(t *testing.T) string {
					name := fmt.Sprintf("m%d", len(fs.log)+1)
					// The ids PutMigr takes, the pool's next two, so the slow
					// zeroing is set before the worker hears of the side; the
					// create names the next_id they come from, and a write
					// that took them since makes PutMigr refuse it
					// (MigrSpec.ExpectNextId).
					next := model.SpNextId(state.Conf)
					dstKey := agentfake.SideKey(ref.Id, legId, next+1)
					slow := fs.coin%2 == 0
					if slow {
						fs.setFault(t, fmt.Sprintf("slow zeroing on %s side %d",
							ref.Name, next+1),
							[]faultObj{{stdDnAddr(dst), dstKey}},
							func(o *agentfake.Object) { o.ZeroedBytes = uint64Ptr(0) },
							func(o *agentfake.Object) { o.ZeroedBytes = nil })
					}
					_, err := plant.PutMigr(context.Background(), fs.h.cli, fs.c.cl,
						ref, plant.MigrSpec{
							Name: name, Id: next, SrcSide: src.GetSideId(),
							DstSide: next + 1, DstDn: dst, DstSlot: slot,
							CutoverPending: true, ExpectNextId: next,
						})
					if slow && err != nil {
						fs.clearLever(t, fs.levers[fmt.Sprintf(
							"slow zeroing on %s side %d", ref.Name, next+1)])
					} else if slow {
						name += ", zeroing slowly,"
					}
					return name + " " + fs.outcome(err)
				}})
		}
	}
	return out
}

// migrTargets offers each migration of both pools whose cutover is pending
// (pending true) or has started.
func (fs *faultSched) migrTargets(t *testing.T, pending bool) []faultTarget {
	var out []faultTarget
	for _, ref := range fs.pools {
		state := fs.load(t, ref)
		for _, name := range state.Conf.GetMigrNameList() {
			migr := state.Migrs[name]
			if migr == nil || migr.GetCutoverPending() != pending {
				continue
			}
			out = append(out, faultTarget{fmt.Sprintf("%s migration %s", ref.Name, name),
				func(t *testing.T) string {
					return fs.outcome(fs.endMigr(ref, name, migr, pending))
				}})
		}
	}
	return out
}

// endMigr cancels a migration whose cutover is pending or finishes one whose
// cutover has started, through the model ends a gateway's call and leg
// repair run (MD6).
func (fs *faultSched) endMigr(ref plant.SpRef, name string, migr *pb.Migration, cancel bool) error {
	ctx := context.Background()
	if cancel {
		return model.CancelMigration(ctx, fs.h.cli, fs.c.cl.Id, ref.Shard, ref.Id,
			ref.Name, name, migr.GetMigrId(), fs.c.cl.Conf)
	}
	return model.FinishMigration(ctx, fs.h.cli, fs.c.cl.Id, ref.Shard, ref.Id,
		ref.Name, name, migr.GetMigrId(), fs.c.cl.Conf)
}

func (fs *faultSched) cancelMigrTargets(t *testing.T) []faultTarget {
	return fs.migrTargets(t, true)
}

func (fs *faultSched) finishMigrTargets(t *testing.T) []faultTarget {
	return fs.migrTargets(t, false)
}

// ---------------------------------------------------------------------------
// Settling
// ---------------------------------------------------------------------------

// settle clears every fault and brings everything back: the workers, the
// fakes, the cntlrs' flags and the pools' levels; ends every repair and
// migration as an operator would; and waits until both pools converge.
func (fs *faultSched) settle(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for i, w := range fs.workers {
		if !w.running() {
			fs.workers[i] = fs.h.restart(t, w)
		}
	}
	for _, addr := range slices.Concat(faultDnAddrs(), faultCnAddrs()) {
		if fs.stopped[addr] {
			fs.startAgent(t, addr)
		}
	}
	var descs []string
	for desc := range fs.levers {
		descs = append(descs, desc)
	}
	slices.Sort(descs)
	for _, desc := range descs {
		fs.clearLever(t, fs.levers[desc])
	}
	for _, ref := range fs.pools {
		state := fs.load(t, ref)
		for _, id := range state.Conf.GetCntlrIdList() {
			if state.Cntlrs[id].GetDisabled() {
				enabled := false
				if _, _, err := plant.SetCntlr(ctx, fs.h.cli, fs.c.cl, ref, id,
					plant.CntlrFlags{Disabled: &enabled}); err != nil {
					t.Fatalf("enable %s cntlr %d: %v", ref.Name, id, err)
				}
			}
		}
		if state.Conf.GetSpLevel() != pb.SpLevel_SP_LEVEL_READWRITE {
			if _, err := plant.SetLevel(ctx, fs.h.cli, fs.c.cl, ref,
				pb.SpLevel_SP_LEVEL_READWRITE); err != nil {
				t.Fatalf("level of %s: %v", ref.Name, err)
			}
		}
	}
	fs.waitShared(t)
	// Every repair and migration ends: the primary's verdicts as a real
	// primary would give them once its new leg serves, the operator's
	// finish and cancel, which race the worker's own drive step and leg
	// repair, and a migration cancelled while its cutover is pending or
	// finished once it has started.
	deadline := time.Now().Add(120 * time.Second)
	for {
		open := fs.endOpen(t)
		if len(open) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after 120s waiting for every repair and "+
				"migration to end: %s", strings.Join(open, "; "))
		}
		// A pass of the operator's calls every few polls, not every poll:
		// each pass may run a model op on every open repair.
		time.Sleep(10 * harnessPoll)
	}
	for obj := range fs.verdicts {
		setLever(t, fs.h.fleet.Agent(obj.addr), obj.key,
			func(o *agentfake.Object) { o.Repairs = nil })
	}
	fs.waitConverged(t)
}

// endOpen is one pass of settle's ends: it acts once on every open repair
// and migration and returns what was open.
func (fs *faultSched) endOpen(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	fs.armThin(t)
	open := []string{}
	for _, ref := range fs.pools {
		state := fs.load(t, ref)
		for _, name := range state.Conf.GetRepairNameList() {
			rep := state.Repairs[name]
			if rep == nil {
				continue
			}
			open = append(open, fmt.Sprintf("%s repair %s at %s", ref.Name,
				name, rep.GetRepStage()))
			switch rep.GetRepStage() {
			case pb.RepStage_REP_STAGE_CREATED:
				if rep.GetTryToAbort() {
					fs.setVerdict(t, ref, state, name,
						agentfake.RepairVerdict{AbortConfirmed: true})
				} else if newLegReady(rep) {
					fs.setVerdict(t, ref, state, name,
						agentfake.RepairVerdict{Started: true})
				}
			case pb.RepStage_REP_STAGE_STARTED:
				_, _ = model.FinishRepair(ctx, fs.h.cli, fs.c.cl.Id, ref.Shard,
					ref.Id, ref.Name, 0, name, uint64(time.Now().Unix()),
					fs.c.cl.Conf)
			case pb.RepStage_REP_STAGE_ABORTED:
				_, _ = model.CancelRepair(ctx, fs.h.cli, fs.c.cl.Id, ref.Shard,
					ref.Id, ref.Name, 0, name, fs.c.cl.Conf)
			}
		}
		for _, name := range state.Conf.GetMigrNameList() {
			migr := state.Migrs[name]
			if migr == nil {
				continue
			}
			open = append(open, fmt.Sprintf("%s migration %s", ref.Name, name))
			_ = fs.endMigr(ref, name, migr, migr.GetCutoverPending())
		}
	}
	return open
}

// ---------------------------------------------------------------------------
// Convergence of both pools
// ---------------------------------------------------------------------------

// sp1Converged is convergedNow's walk over the second pool: every side
// provisioned, with no health epoch, and its fake keeping a syncup at SpRev
// that says so; every leg with no epoch and every group flagged as one whose
// array has run (AR15); every cntlr with no epoch, settled, its fake keeping
// a syncup at SpRev in the role its record gives; the thin device created.
// It also wants no repair and no migration in either pool.
func (fs *faultSched) sp1Converged(t *testing.T) (bool, string, uint64, []objProgress) {
	t.Helper()
	ref := fs.pools[1]
	state := fs.load(t, ref)
	why := ""
	note := func(format string, args ...any) {
		if why == "" {
			why = fmt.Sprintf(format, args...)
		}
	}
	var objs []objProgress
	for _, r := range fs.pools {
		s := state
		if r != ref {
			s = fs.load(t, r)
		}
		if n := len(s.Conf.GetRepairNameList()) + len(s.Conf.GetMigrNameList()); n != 0 {
			note("%s: %d repairs and migrations open", r.Name, n)
		}
	}
	for _, grp := range poolGrps(state) {
		if !grp.GetArrayMade() {
			note("%s group %d: array_made not set", ref.Name, grp.GetGrpId())
		}
		for _, leg := range grp.GetLegList() {
			if leg.GetErrEpoch() != 0 {
				note("%s leg %d: err_epoch %d", ref.Name, leg.GetLegId(), leg.GetErrEpoch())
			}
			for _, side := range leg.GetSideList() {
				name := fmt.Sprintf("%s side %d", ref.Name, side.GetSideId())
				agent := fs.h.fleet.Agent(side.GetAddrPort())
				key := agentfake.SideKey(ref.Id, leg.GetLegId(), side.GetSideId())
				msg, syncups, rounds := storedSyncup(agent, key)
				objs = append(objs, objProgress{name, agent, key, syncups, rounds})
				req, _ := msg.(*pb.SyncupSideRequest)
				switch {
				case !side.GetProvisioned():
					note("%s: not provisioned", name)
				case side.GetErrEpoch() != 0:
					note("%s: err_epoch %d", name, side.GetErrEpoch())
				case req.GetRevision() != state.SpRevision:
					note("%s: keeps a SyncupSide at revision %d, SpRev %d", name,
						req.GetRevision(), state.SpRevision)
				case !req.GetSideConf().GetProvisioned():
					note("%s: keeps a SyncupSide that says not provisioned", name)
				}
			}
		}
	}
	for _, id := range state.Conf.GetCntlrIdList() {
		name := fmt.Sprintf("%s cntlr %d", ref.Name, id)
		cntlr := state.Cntlrs[id]
		agent := fs.h.fleet.Agent(cntlr.GetAddrPort())
		key := agentfake.CntlrKey(ref.Id, id)
		msg, syncups, rounds := storedSyncup(agent, key)
		objs = append(objs, objProgress{name, agent, key, syncups, rounds})
		req, _ := msg.(*pb.SyncupCntlrRequest)
		switch {
		case cntlr.GetErrEpoch() != 0:
			note("%s: err_epoch %d", name, cntlr.GetErrEpoch())
		case cntlr.GetSettling():
			note("%s: settling", name)
		case req.GetRevision() != state.SpRevision:
			note("%s: keeps a SyncupCntlr at revision %d, SpRev %d", name,
				req.GetRevision(), state.SpRevision)
		case req.GetCntlr().GetPrimary() != cntlr.GetPrimary():
			note("%s: keeps a SyncupCntlr with primary %t, the record %t", name,
				req.GetCntlr().GetPrimary(), cntlr.GetPrimary())
		}
	}
	for i, td := range state.Tds {
		if !td.GetCreated() {
			note("%s td %s: not created", ref.Name, state.TdNames[i])
		}
	}
	return why == "", why, state.SpRevision, objs
}

// waitConverged is the standard cluster's waitConverged over both pools:
// the nodes and sp0 by convergedNow, sp1 by sp1Converged, then two more
// rounds of every object of both with no syncup, and no revision moved. A
// break of that convergence fails the case, with every reaction applied
// and every health change written since the wait began, for its reader.
func (fs *faultSched) waitConverged(t *testing.T) {
	t.Helper()
	since := time.Now()
	why := fs.convergeOnce(t)
	if why == "" {
		return
	}
	var events []string
	for _, msg := range []string{msgReactionApplied, msgHealthChanged} {
		for _, rec := range fs.h.records(nil, msg) {
			if rec.at.After(since) {
				events = append(events, fmt.Sprintf("%s %s %q %v",
					rec.at.Format("15:04:05.000"), rec.workerName(), rec.msg,
					rec.attrs))
			}
		}
	}
	t.Fatalf("seed %d: %s; reactions and health changes since the wait "+
		"began:\n%s", fs.seed, why, strings.Join(events, "\n"))
}

// convergeOnce is one wait of waitConverged: "" once both pools stayed
// converged, else what broke it.
func (fs *faultSched) convergeOnce(t *testing.T) string {
	t.Helper()
	var conv convergence
	var sp1Rev uint64
	var sp1Objs []objProgress
	fs.h.waitUntil(t, 90*time.Second, "both pools to converge",
		func() (bool, string) {
			ok, why, now := fs.c.convergedNow(t)
			conv = now
			if !ok {
				return false, why
			}
			ok, why, sp1Rev, sp1Objs = fs.sp1Converged(t)
			return ok, why
		})
	objs := slices.Concat(conv.objs, sp1Objs)
	broken := ""
	fs.h.waitUntil(t, 20*time.Second, "two more rounds of every object",
		func() (bool, string) {
			for _, obj := range objs {
				rec := obj.agent.Record(obj.key)
				if len(rec.Syncups) != obj.syncups {
					var last proto.Message
					if n := len(rec.Syncups); n > 0 {
						last = rec.Syncups[n-1]
					}
					broken = fmt.Sprintf("%s got syncup %d after both pools "+
						"converged: %v", obj.name, len(rec.Syncups), last)
					return true, ""
				}
				if len(rec.Rounds) < obj.rounds+2 {
					return false, fmt.Sprintf("%s: %d rounds since, of 2",
						obj.name, len(rec.Rounds)-obj.rounds)
				}
			}
			return true, ""
		})
	if broken != "" {
		return broken
	}
	ok, why, after := fs.c.convergedNow(t)
	if !ok {
		return "the cluster stopped reading converged: " + why
	}
	ok, why, sp1After, _ := fs.sp1Converged(t)
	if !ok {
		return "sp1 stopped reading converged: " + why
	}
	if after.spRev != conv.spRev || sp1After != sp1Rev {
		return fmt.Sprintf("a pool's revision moved after both converged: "+
			"sp0 %d to %d, sp1 %d to %d", conv.spRev, after.spRev, sp1Rev,
			sp1After)
	}
	for id, rev := range conv.dnRevs {
		if after.dnRevs[id] != rev {
			return fmt.Sprintf("dn %d's revision moved from %d to %d", id, rev,
				after.dnRevs[id])
		}
	}
	for id, rev := range conv.cnRevs {
		if after.cnRevs[id] != rev {
			return fmt.Sprintf("cn %d's revision moved from %d to %d", id, rev,
				after.cnRevs[id])
		}
	}
	return ""
}
