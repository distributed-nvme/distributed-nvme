package cnagent

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// ---------------------------------------------------------------------------
// Teardown by sweep (architecture.md §9.8, cnagent.md CN21)
//
// Removal is derived by comparing the DESIRED state with the ACTUAL state of
// the node, never from a remembered plan and never from a remembered failure.
// Each pass enumerates what exists, subtracts what the desired state wants,
// removes the rest top-down, verifies each removal with a probe that cannot
// block on a dead remote, and recomputes "something is left" from scratch.
//
// That is what the old retire phase could not do. It computed what to remove
// as "the plan I applied last time minus the plan I am applying now", and
// then overwrote the applied plan whether or not the removals worked — so a
// removal that failed was forgotten together with the plan that named it, and
// nothing ever enumerated the object again. The leak the design started from
// (a killed `mdadm --detail` read as "the array is not there", the array left
// pinning its two leg wrappers for ever) is that shape, not a one-off bug.
//
// Two rules make the sweep safe rather than merely thorough:
//
//   - "gone" is always probe-verified, and a probe that DID NOT ANSWER is
//     "unknown", which counts as a leftover. A command killed at the soft
//     timeout may still have completed in the kernel, so only a fresh probe
//     can say (agent.Reported);
//   - within one object's chain the layers are strictly top-down, and the
//     descent STOPS at the first layer that left something behind. Removing a
//     leg out from under a live array is destructive on the migration path,
//     and a pass that simply re-runs next round costs nothing: by then the
//     failfast window has passed and the same order succeeds.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Enumeration
// ---------------------------------------------------------------------------

// cnActual is one snapshot of everything on the node this agent could own.
// It is taken fresh on every pass and thrown away afterwards: a snapshot is
// stale the instant it is printed, and the sweep's only use for it is to
// decide what to ATTEMPT — every removal re-probes its own object.
type cnActual struct {
	// dms is every dm device the node holds, name to "major:minor". The devno
	// can be empty for an entry the listing named and the kernel has already
	// dropped.
	dms map[string]string
	// ours is the subset whose name decodes as a dm name of this cluster and
	// this cn.
	ours map[string]common.DmName
	// byDevNo resolves a live table's device argument back to a dm name,
	// which is how a wanted device's backing is recognised as unwanted.
	byDevNo map[string]string
	// dmListed says the `dmsetup ls` behind dms/ours/byDevNo ANSWERED. When
	// it did not, those three maps are empty for want of an answer rather
	// than because the node holds nothing, and an empty map is the shape of
	// "there is nothing here" — so no removal may be derived from this
	// snapshot at all (architecture.md §9.8: "gone" is probe-verified, never
	// inferred). The pass reports what it can and re-drives.
	dmListed bool
	// arrays is every md array on the node, read from sysfs alone.
	arrays []MdArray
	// hostSubsys is every nvme subsystem this host holds a controller for.
	hostSubsys []agent.SubsysBrief
	// nvmetSubsys is every subsystem in the target's configfs tree, linked to
	// the port or not.
	nvmetSubsys []string
	// nvmetOwner is that tree attributed once per pass. Attribution costs
	// configfs reads per host-facing subsystem, and doing it again for every
	// sp chain would not only be wasteful but could give two chains
	// different answers about the same subsystem.
	nvmetOwner map[string]nvmetAttr
}

// nvmetAttr is one subsystem's attribution. ours is false for a subsystem
// this agent must never touch; unowned is true only when ours is true and no
// sp could be named for it.
type nvmetAttr struct {
	spId    uint64
	ours    bool
	unowned bool
}

// enumerateCn takes the snapshot. Every enumerator that did not answer is
// recorded as a failure and leaves its part of the snapshot empty — the sweep
// then removes nothing from that part and reports the failure, because an
// empty answer it cannot trust must never read as "there is nothing there".
func (s *CnAgentServer) enumerateCn(
	ctx context.Context,
	clusterId uint64,
	cnId uint64,
	res *agent.SweepResult,
) *cnActual {
	actual := &cnActual{
		dms:     make(map[string]string),
		ours:    make(map[string]common.DmName),
		byDevNo: make(map[string]string),
	}
	dms, err := s.dm.List(ctx)
	if err != nil {
		res.Fail("dmsetup ls", err)
	} else {
		actual.dmListed = true
		actual.dms = dms
		for name, devNo := range dms {
			if devNo != "" {
				actual.byDevNo[devNo] = name
			}
			dn, ok := common.ParseDmName(name)
			if !ok || dn.Role() != common.DmRoleCn ||
				dn.ClusterId != clusterId || dn.NodeId != cnId {
				continue
			}
			actual.ours[name] = dn
		}
	}
	arrays, err := s.md.ListArrays(ctx)
	if err != nil {
		res.Fail("md arrays", err)
	} else {
		actual.arrays = arrays
	}
	hostSubsys, err := s.host.ListAllSubsys(ctx)
	if err != nil {
		res.Fail("nvme subsystems", err)
	} else {
		actual.hostSubsys = hostSubsys
	}
	nvmetSubsys, err := s.nvmet.ListSubsystems(ctx)
	if err != nil {
		res.Fail("nvmet subsystems", err)
	} else {
		actual.nvmetSubsys = nvmetSubsys
	}
	actual.nvmetOwner = make(map[string]nvmetAttr, len(actual.nvmetSubsys))
	for _, nqn := range actual.nvmetSubsys {
		spId, ours, unowned := s.nvmetOwner(ctx, clusterId, cnId, nqn, res)
		actual.nvmetOwner[nqn] = nvmetAttr{
			spId: spId, ours: ours, unowned: unowned}
	}
	return actual
}

// dmsOfKind is the node's devices of one kind, whose sp id passes the filter,
// sorted so a pass is deterministic.
func (a *cnActual) dmsOfKind(
	kind common.DmKind,
	want func(dn common.DmName) bool,
) []string {
	var out []string
	for name, dn := range a.ours {
		if dn.Kind == kind && want(dn) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// arrayOwner attributes an md array to an sp. An array is ours only when
// EVERY member is a kind-c9 leg wrapper of this cluster and cn, all naming
// one sp. An array with a member that is not a dm device, or is a dm device
// of somebody else, is not attributable and is never stopped — that rule is
// what keeps a co-hosted dn agent's udev-assembled array untouched.
func arrayOwner(
	clusterId uint64,
	cnId uint64,
	array MdArray,
) (uint64, bool) {
	if array.Foreign || len(array.Members) == 0 {
		return 0, false
	}
	var spId uint64
	for i, member := range array.Members {
		dn, ok := common.ParseDmName(member)
		if !ok || dn.Kind != common.DmKindCnLeg ||
			dn.ClusterId != clusterId || dn.NodeId != cnId {
			return 0, false
		}
		if i == 0 {
			spId = dn.SpId()
		} else if dn.SpId() != spId {
			return 0, false
		}
	}
	return spId, true
}

// nvmetOwner attributes one subsystem in the target's configfs tree. It is
// called once per subsystem per pass, from enumerateCn.
//
// A dnv-format NQN carries its own ids. A HOST-FACING NQN is chosen by the
// user and carries nothing, so it is attributed by what its namespaces point
// at: a namespace whose device_path is a kind-c6 ns-dev of ours makes the
// subsystem that sp's. One with no attributable namespace at all is
// **unowned** and only the node-level sweep may remove it — which assumes at
// most one cn agent per kernel. A co-hosted dn agent is fine: its NQNs are
// all dnv-format kinds 2 and 3, which never reach that arm.
//
// ours is false for a subsystem this agent must never touch; unowned is true
// only when ours is true and no sp could be named.
func (s *CnAgentServer) nvmetOwner(
	ctx context.Context,
	clusterId uint64,
	cnId uint64,
	nqn string,
	res *agent.SweepResult,
) (spId uint64, ours bool, unowned bool) {
	if parts, ok := common.ParseNqn(nqn); ok {
		// A dnv-format NQN says whose it is. Kinds 2 and 3 are the dn role's
		// exports; kinds 0 and 1 are host NQNs and never name a subsystem.
		if parts.Kind == common.NqnKindXfer && parts.Ids[0] == clusterId {
			return parts.Ids[1], true, false
		}
		return 0, false, false
	}
	if common.IsDnvNqn(nqn) {
		// In our namespace but decoding to nothing: never touched. Treating
		// it as host-facing would let a malformed name get a subsystem
		// deleted.
		slog.DebugContext(ctx, "undecodable dnv nqn left alone",
			slog.String("nqn", nqn))
		return 0, false, false
	}
	// A host-facing NQN a stored cntlr still names settles it before anything
	// is read, and it has to come FIRST: a subsystem whose last namespace was
	// just deleted has nothing left to attribute it by, and the namespace
	// rule below would call it unowned and take the host's subsystem away
	// while `nqn_list` still names it.
	if spId, ok := s.hostFacingClaim(clusterId, cnId, nqn); ok {
		return spId, true, false
	}
	nsids, found, err := s.nvmet.ListNamespaces(ctx, nqn)
	if err != nil {
		res.Fail("nvmet namespaces of "+nqn, err)
		return 0, false, false
	}
	if !found {
		// Removed between the listing and this read.
		return 0, false, false
	}
	for _, nsid := range nsids {
		path, ok, err := s.nvmet.NsDevicePath(ctx, nqn, nsid)
		if err != nil {
			res.Fail(fmt.Sprintf("nvmet %s ns %d device_path", nqn, nsid), err)
			return 0, false, false
		}
		if !ok {
			continue
		}
		dn, ok := common.ParseDmName(strings.TrimPrefix(
			strings.TrimSpace(path), "/dev/mapper/"))
		if !ok || dn.Kind != common.DmKindCnNsDev {
			continue
		}
		if dn.ClusterId != clusterId || dn.NodeId != cnId {
			// A namespace backed by ANOTHER cn's ns-dev: foreign, not
			// unowned (`architecture.md` §9.8, "Several agents share one
			// kernel"). One cn agent per kernel is the ordinary deployment,
			// so this should not arise — but the unowned arm below REMOVES,
			// and a rule that has to be right only while an assumption holds
			// is the shape that takes a sibling's subsystem when it stops
			// holding.
			return 0, false, false
		}
		return dn.SpId(), true, false
	}
	return 0, true, true
}

// hostFacingClaim reports whether any cntlr of this cn still names one
// host-facing NQN in its stored request, and under which sp. It is the
// request-derived half of §9.8's attribution rule: the NQN is the user's own
// string and carries nothing, so "somebody here still wants it" is evidence
// in its own right — and the only evidence there is for a subsystem that
// currently has no namespace at all.
//
// A cntlr's request is stored (putCntlr) before its converge creates
// anything, so a subsystem cannot be built by a pass whose claim is not
// already visible here.
func (s *CnAgentServer) hostFacingClaim(
	clusterId uint64,
	cnId uint64,
	nqn string,
) (uint64, bool) {
	for _, key := range s.cntlrKeysOf(clusterId, cnId) {
		st := s.getCntlr(key)
		if st == nil {
			continue
		}
		if _, named := st.req.GetNqnToSubsystem()[nqn]; named {
			return st.req.GetCntlrPointer().GetSpId(), true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// The wanted set
// ---------------------------------------------------------------------------

// cnWanted is what the desired state wants to exist. It is derived from the
// plan alone, with every provisioning deferral removed: a deferred object is
// wanted even though the build phase has not built it yet, so a sweep can
// never remove what the next converge is about to create.
type cnWanted struct {
	dms      map[string]struct{}
	nvmetSs  map[string]struct{}
	nvmetNs  map[string]map[int]struct{}
	hostNqns map[string]struct{}
	// grpOfWrapper maps a wanted leg wrapper's name to its group, which is
	// how an md array — whose own name says nothing about the sp — is judged.
	grpOfWrapper map[string]*grpPlan
}

func newCnWanted() *cnWanted {
	return &cnWanted{
		dms:          make(map[string]struct{}),
		nvmetSs:      make(map[string]struct{}),
		nvmetNs:      make(map[string]map[int]struct{}),
		hostNqns:     make(map[string]struct{}),
		grpOfWrapper: make(map[string]*grpPlan),
	}
}

func (w *cnWanted) addDm(names ...string) {
	for _, name := range names {
		w.dms[name] = struct{}{}
	}
}

func (w *cnWanted) addNs(nqn string, nsid int) {
	set := w.nvmetNs[nqn]
	if set == nil {
		set = make(map[int]struct{})
		w.nvmetNs[nqn] = set
	}
	set[nsid] = struct{}{}
}

// cntlrWanted is the wanted set of one cntlr: exactly the objects the build
// phase would ensure for this plan with the [D15] deferrals removed.
//
// Every row is pinned against build() in syncup_cntlr.go, not against the
// design table, because build is what actually creates them:
//
//	c9 wrapper + :2: connection   wantLeg   (ensureLegs)
//	md array / ca group linear    wantGrp   (ensureGroup)
//	c0 c1 c2 pool devices         wantPool  (ensureSlice)
//	c3 thin volume                wantPool  (ensureThin)
//	c4 raid0                      wantPool  (ensureRaid0 — NOT wantAny: at
//	                                        SP_LEVEL_NO_THINPOOL the raid0
//	                                        has nothing to stripe over)
//	c5 per-td dm-error            wantAny   (ensureDmError; a standby keeps it)
//	c6 ns-dev + nvmet namespace   wantAny   (ensureNsDev / ensureSubsystem)
//	host-facing nvmet subsystem   wantAny   (ensureSubsystem)
//	c8 xfer + its subsystem/ns    wantAny   (ensureXfer)
//	c7 clone + cb wrapper + :4:   wantClone (ensureClone)
func cntlrWanted(plan *cntlrPlan) *cnWanted {
	w := newCnWanted()
	if plan.wantLeg {
		for _, lp := range plan.legs {
			w.addDm(lp.name)
			w.hostNqns[lp.nqn] = struct{}{}
		}
	}
	if plan.wantGrp {
		for _, gp := range plan.grps {
			if !gp.raid1 {
				w.addDm(gp.dmName)
			}
		}
		// grpOfWrapper is filled ONLY under wantGrp, because it is the whole
		// of arrayWanted: an md array carries no sp in its name, so "some
		// wanted group owns every member" is the only thing that makes it
		// wanted. A standby, or any level at or above SP_LEVEL_NO_REDUND,
		// wants no group — and therefore no array, which is exactly what a
		// primary→standby flip has to stop.
		for _, lp := range plan.legs {
			if lp.grp != nil {
				w.grpOfWrapper[lp.name] = lp.grp
			}
		}
	}
	if plan.wantPool {
		for _, sp := range plan.slices {
			w.addDm(sp.poolMetaName, sp.poolDataName, sp.poolFinalName)
		}
		for _, tp := range plan.tds {
			w.addDm(tp.raid0Name)
			for _, sp := range plan.slices {
				w.addDm(plan.thinName(tp.tdId, sp.sliceId))
			}
		}
	}
	if plan.wantAny {
		for _, tp := range plan.tds {
			w.addDm(tp.errorName)
		}
		for _, np := range plan.namespaces {
			w.addDm(np.devName)
			w.addNs(np.ss.nqn, np.nsIdx)
		}
		for _, ssp := range plan.subsystems {
			w.nvmetSs[ssp.nqn] = struct{}{}
		}
		for _, xp := range plan.xfers {
			w.addDm(xp.finalName)
			w.nvmetSs[xp.nqn] = struct{}{}
			w.addNs(xp.nqn, int(xp.xfer.GetOriNsIdx()))
		}
	}
	if plan.wantClone {
		for _, cp := range plan.clones {
			w.addDm(cp.finalName, cp.metaDmName)
			w.hostNqns[cp.clone.GetSrcNqn()] = struct{}{}
		}
	}
	return w
}

// arrayWanted judges one md array attributed to this plan's sp. It is wanted
// when every member is a wrapper of one raid1 group this plan still wants;
// anything else — a group that left the request, a role or level that wants
// no groups at all (grpOfWrapper is then empty), members from two groups —
// makes it unwanted and stops it.
func (w *cnWanted) arrayWanted(array MdArray) bool {
	var grp *grpPlan
	for _, member := range array.Members {
		gp := w.grpOfWrapper[member]
		if gp == nil || !gp.raid1 {
			return false
		}
		if grp == nil {
			grp = gp
		} else if grp != gp {
			return false
		}
	}
	return grp != nil
}

// ---------------------------------------------------------------------------
// The chain: one sp's unwanted objects, and the top-down removal of them
// ---------------------------------------------------------------------------

// cnNsRef is one nvmet namespace under a subsystem that stays.
type cnNsRef struct {
	nqn  string
	nsid int
}

// cnChain is everything of one sp that exists and is not wanted, grouped by
// the layer that removes it. Building a chain IS the comparison; running it is
// the removal. A node-level chain is the same thing with an empty wanted set.
type cnChain struct {
	spId uint64

	nvmetSs   []string  // host-facing and xfer subsystems
	nvmetNs   []cnNsRef // namespaces under a subsystem that stays
	nsDevs    []string  // c6
	xferDms   []string  // c8
	clones    []string  // c7
	cloneMeta []string  // cb
	// srcNqns is evaluated at L5, not when the chain is built: a clone-source
	// connection is unowned only once the dm-clone above it is GONE, and L3
	// is what removes it. Deciding earlier would read the clone this very
	// pass is about to remove as a live user of its own source and never
	// disconnect it.
	srcNqns   func() []string
	raid0s    []string // c4
	tdErrors  []string // c5
	thins     []string // c3
	pools     []string // c2
	poolMetas []string // c0
	poolDatas []string // c1
	arrays    []MdArray
	grpDms    []string // ca
	legNqns   []string // :2: side-to-cn connections
	legDms    []string // c9
}

// srcNqnList evaluates the chain's clone-source connections, once.
func (c *cnChain) srcNqnList() []string {
	if c.srcNqns == nil {
		return nil
	}
	return c.srcNqns()
}

// buildChain subtracts the wanted set from the snapshot, for one sp. A nil
// wanted set means "nothing of this sp is wanted", which is the node-level
// case for an sp whose pointer has left the cn's list.
func (s *CnAgentServer) buildChain(
	ctx context.Context,
	clusterId uint64,
	cnId uint64,
	spId uint64,
	actual *cnActual,
	wanted *cnWanted,
	res *agent.SweepResult,
) *cnChain {
	if wanted == nil {
		wanted = newCnWanted()
	}
	chain := &cnChain{spId: spId}
	ofSp := func(dn common.DmName) bool { return dn.SpId() == spId }
	unwantedDms := func(kind common.DmKind) []string {
		var out []string
		for _, name := range actual.dmsOfKind(kind, ofSp) {
			if _, ok := wanted.dms[name]; !ok {
				out = append(out, name)
			}
		}
		return out
	}
	chain.nsDevs = unwantedDms(common.DmKindCnNsDev)
	chain.xferDms = unwantedDms(common.DmKindCnXferFinal)
	chain.clones = unwantedDms(common.DmKindCnCloneFinal)
	chain.cloneMeta = unwantedDms(common.DmKindCnCloneMeta)
	chain.raid0s = unwantedDms(common.DmKindCnRaid0)
	chain.tdErrors = unwantedDms(common.DmKindCnError)
	chain.thins = unwantedDms(common.DmKindCnThinDev)
	chain.pools = unwantedDms(common.DmKindCnPoolFinal)
	chain.poolMetas = unwantedDms(common.DmKindCnPoolMeta)
	chain.poolDatas = unwantedDms(common.DmKindCnPoolData)
	chain.grpDms = unwantedDms(common.DmKindCnGrp)
	chain.legDms = unwantedDms(common.DmKindCnLeg)

	for _, array := range actual.arrays {
		owner, ok := arrayOwner(clusterId, cnId, array)
		if !ok || owner != spId {
			continue
		}
		if !wanted.arrayWanted(array) {
			chain.arrays = append(chain.arrays, array)
		}
	}

	for _, nqn := range actual.nvmetSubsys {
		attr := actual.nvmetOwner[nqn]
		if !attr.ours || attr.unowned || attr.spId != spId {
			continue
		}
		if _, keep := wanted.nvmetSs[nqn]; !keep {
			chain.nvmetSs = append(chain.nvmetSs, nqn)
			continue
		}
		// The subsystem stays; a namespace under it may still have to go.
		nsids, found, err := s.nvmet.ListNamespaces(ctx, nqn)
		if err != nil {
			res.Fail("nvmet namespaces of "+nqn, err)
			continue
		}
		if !found {
			continue
		}
		for _, nsid := range nsids {
			if _, keep := wanted.nvmetNs[nqn][nsid]; keep {
				continue
			}
			chain.nvmetNs = append(chain.nvmetNs, cnNsRef{nqn: nqn, nsid: nsid})
		}
	}

	for _, state := range actual.hostSubsys {
		parts, ok := common.ParseNqn(state.Nqn)
		if !ok || parts.Kind != common.NqnKindSideToCn {
			continue
		}
		// SideToCnNqn is (cluster, sp, leg, cn): it names the CN it is
		// exported to, which is how a connection of ours is told from one
		// another CN on this cluster holds.
		if parts.Ids[0] != clusterId || parts.Ids[1] != spId ||
			parts.Ids[3] != cnId {
			continue
		}
		if _, keep := wanted.hostNqns[state.Nqn]; !keep {
			chain.legNqns = append(chain.legNqns, state.Nqn)
		}
	}
	sort.Strings(chain.nvmetSs)
	sort.Strings(chain.legNqns)
	sort.Slice(chain.nvmetNs, func(i, j int) bool {
		if chain.nvmetNs[i].nqn != chain.nvmetNs[j].nqn {
			return chain.nvmetNs[i].nqn < chain.nvmetNs[j].nqn
		}
		return chain.nvmetNs[i].nsid < chain.nvmetNs[j].nsid
	})
	return chain
}

// removeDmVerified removes a dm device and re-probes it. The probe, not the
// exit status, decides: `dmsetup remove` may have been killed after the
// kernel had already completed it, and a killed command's status says nothing
// at all.
func (s *CnAgentServer) removeDmVerified(
	ctx context.Context,
	name string,
) bool {
	s.removeDm(ctx, name)
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		slog.ErrorContext(ctx, "verifying a dm removal failed",
			slog.String("name", name),
			slog.String("error", err.Error()))
		return false
	}
	return dev == nil
}

// removeCloneMetaDmVerified is removeDmVerified under cloneMetaMu: removing a
// kind-cb wrapper frees its units in the arena's next enumeration, so it is a
// registry mutation (CN18) and may not race another cntlr's allocation.
func (s *CnAgentServer) removeCloneMetaDmVerified(
	ctx context.Context,
	name string,
) bool {
	s.cloneMetaMu.Lock()
	defer s.cloneMetaMu.Unlock()
	s.removeDm(ctx, name)
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		slog.ErrorContext(ctx, "verifying a dm removal failed",
			slog.String("name", name),
			slog.String("error", err.Error()))
		return false
	}
	return dev == nil
}

// removeExportVerified removes an nvmet subsystem and re-probes configfs for
// it, so a killed `rmdir` cannot pass for a removal.
func (s *CnAgentServer) removeExportVerified(
	ctx context.Context,
	nqn string,
) bool {
	s.removeExport(ctx, nqn)
	exists, err := s.nvmet.SubsysExists(ctx, nqn)
	if err != nil {
		slog.ErrorContext(ctx, "verifying an nvmet removal failed",
			slog.String("nqn", nqn),
			slog.String("error", err.Error()))
		return false
	}
	return !exists
}

// runChain removes one sp's unwanted objects top-down and returns what is
// left. The layers are CN21's order; the `stuck` check between them is the
// stop rule: a layer that left anything behind ends the descent, and the layers
// below report their objects as leftovers WITHOUT touching them. Removing a
// leg out from under a live array would strand a migration's md superblocks,
// and a pass that simply re-runs next round costs nothing.
func (s *CnAgentServer) runChain(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	chain *cnChain,
	actual *cnActual,
	wanted *cnWanted,
	res *agent.SweepResult,
) {
	// P0, before every layer: an unwanted ns-dev is reloaded onto an error
	// table of its own size. Two things need that. Disabling the nvmet
	// namespace above it in L1 closes its backing device, which does not
	// complete on a SUSPENDED dm device; and the reload's own flushing
	// suspend is what completes the in-flight host IO instead of replaying it
	// at resume onto a stack that is about to be removed.
	//
	// The size comes from the device's own live table, so this needs neither
	// the td that used to back it — which may be leaving in the same pass —
	// nor any memory of what the ns-dev used to map.
	for _, name := range chain.nsDevs {
		s.parkByTable(ctx, name, actual)
	}
	stuck := false
	// Every layer's own leftovers are collected here, so a stuck layer can
	// report the layers below it untouched.
	layers := []struct {
		run    func() bool
		report func()
	}{
		{ // L1 — nvmet objects: the exports release the devices under them.
			run: func() bool {
				left := false
				for _, ref := range chain.nvmetNs {
					// An unwanted namespace goes ANA-inaccessible immediately
					// before it is removed, so a host that still holds a path
					// is told to stop using it rather than losing it under IO.
					s.setAnaLogged(ctx, ref.nqn, ref.nsid)
					if err := s.nvmet.RemoveNamespace(
						ctx, ref.nqn, ref.nsid); err != nil {
						slog.ErrorContext(ctx,
							"removing nvmet namespace failed",
							slog.String("nqn", ref.nqn),
							slog.Int("nsid", ref.nsid),
							slog.String("error", err.Error()))
					}
					gone, err := s.nvmet.NamespaceGone(ctx, ref.nqn, ref.nsid)
					if err != nil || !gone {
						res.Add(agent.LeftoverKindNvmetNs,
							fmt.Sprintf("%s/%d", ref.nqn, ref.nsid))
						left = true
					}
				}
				for _, nqn := range chain.nvmetSs {
					if !s.removeExportVerified(ctx, nqn) {
						res.Add(agent.LeftoverKindNvmet, nqn)
						left = true
					}
				}
				return left
			},
			report: func() {
				for _, ref := range chain.nvmetNs {
					res.Add(agent.LeftoverKindNvmetNs,
						fmt.Sprintf("%s/%d", ref.nqn, ref.nsid))
				}
				for _, nqn := range chain.nvmetSs {
					res.Add(agent.LeftoverKindNvmet, nqn)
				}
			},
		},
		{ // L2 — ns-devs and transfer devices.
			run:    func() bool { return s.removeDms(ctx, res, chain.nsDevs, chain.xferDms) },
			report: func() { s.reportDms(res, chain.nsDevs, chain.xferDms) },
		},
		{ // L3 — dm-clones, before their source connections die: dm-clone
			// flushes through the source on removal.
			run:    func() bool { return s.removeDms(ctx, res, chain.clones) },
			report: func() { s.reportDms(res, chain.clones) },
		},
		{ // L4 — clone-metadata wrappers. The chunk FILES of a clone that
			// left the request are swept separately, against the request
			// itself (sweepCloneChunks): a standby has no wrapper to key
			// them off.
			run: func() bool {
				left := false
				for _, name := range chain.cloneMeta {
					if !s.removeCloneMetaDmVerified(ctx, name) {
						res.Add(agent.LeftoverKindDm, name)
						left = true
					}
				}
				return left
			},
			report: func() { s.reportDms(res, chain.cloneMeta) },
		},
		{ // L5 — clone-source connections, once nothing maps them.
			run: func() bool {
				left := false
				for _, nqn := range chain.srcNqnList() {
					if !s.disconnectVerified(ctx, nqn) {
						res.Add(agent.LeftoverKindNvme, nqn)
						left = true
					}
				}
				return left
			},
			report: func() {
				for _, nqn := range chain.srcNqnList() {
					res.Add(agent.LeftoverKindNvme, nqn)
				}
			},
		},
		{ // L6 — per-td raid0 and dm-error.
			run:    func() bool { return s.removeDms(ctx, res, chain.raid0s, chain.tdErrors) },
			report: func() { s.reportDms(res, chain.raid0s, chain.tdErrors) },
		},
		{ // L7 — thin volumes, and the pool-side `delete` of their ids.
			run:    func() bool { return s.removeThins(ctx, chain, actual, wanted, res) },
			report: func() { s.reportDms(res, chain.thins) },
		},
		{ // L8 — thin-pools, then the concats under them.
			run: func() bool {
				left := s.removeDms(ctx, res, chain.pools)
				if st != nil && !left {
					for _, name := range chain.pools {
						if dn, ok := common.ParseDmName(name); ok {
							// The pool device's life ended, so its CN14
							// arming does too; a re-creation re-arms on its
							// own Create branch.
							delete(st.pendingSweep, dn.Ids[1])
						}
					}
				}
				// ONLY WHEN THE REMOVAL WAS VERIFIED. `left` means the
				// re-probe could not confirm the pool is gone, and dropping
				// the arming there would forget a live pool's pending thin-id
				// sweep: the next converge finds the device present, takes
				// ensurePool's probe-match path, and arms nothing — so
				// sweepThinIds never runs and every id deleted meanwhile
				// holds its data blocks for the life of the pool. Keeping an
				// arming too long costs one idempotent sweep; dropping one
				// too early cannot be repaired. This is the layer stop rule
				// applied to a STATE mutation rather than to a device: a
				// partly-succeeded removal is not a completed one.
				if left {
					// The concats stay untouched — the pool still maps them
					// — but they ARE present and unwanted, so they are named
					// like every other layer's leftovers.
					s.reportDms(res, chain.poolMetas, chain.poolDatas)
					return true
				}
				return s.removeDms(ctx, res, chain.poolMetas, chain.poolDatas)
			},
			report: func() {
				s.reportDms(res, chain.pools, chain.poolMetas, chain.poolDatas)
			},
		},
		{ // L9 — md arrays and RedundNone group linears.
			run: func() bool {
				left := false
				for _, array := range chain.arrays {
					if !s.stopArrayVerified(ctx, array) {
						res.Add(agent.LeftoverKindMd, array.Dev)
						left = true
					}
				}
				if s.removeDms(ctx, res, chain.grpDms) {
					left = true
				}
				return left
			},
			report: func() {
				for _, array := range chain.arrays {
					res.Add(agent.LeftoverKindMd, array.Dev)
				}
				s.reportDms(res, chain.grpDms)
			},
		},
		{ // L10 — legs: the probers, then the connections, then the wrappers.
			run: func() bool {
				left := false
				// A prober wedged on a pathless leg holds an open fd on the
				// wrapper, so it has to go before the removal; the disconnect
				// below is what releases one stuck in D state (CN11).
				if st != nil && plan != nil {
					s.stopLegProbers(st, wantedProbers(plan))
				}
				for _, nqn := range chain.legNqns {
					if !s.disconnectVerified(ctx, nqn) {
						res.Add(agent.LeftoverKindNvme, nqn)
						left = true
					}
				}
				// A wrapper is removed even when its own connection is stuck:
				// they are two different objects, and the connection is what
				// the next round retries.
				if s.removeDms(ctx, res, chain.legDms) {
					left = true
				}
				return left
			},
			report: func() {
				for _, nqn := range chain.legNqns {
					res.Add(agent.LeftoverKindNvme, nqn)
				}
				s.reportDms(res, chain.legDms)
			},
		},
	}
	for _, layer := range layers {
		if stuck {
			layer.report()
			continue
		}
		if layer.run() {
			stuck = true
		}
	}
}

// parkByTable stops an UNWANTED ns-dev serving, deriving everything it needs
// from the device's own live table — there is no plan for a namespace that
// left the desired state, and its td may be leaving in the same pass.
//
// The park's target is the CN16 one wherever the live table still says what
// it is: an ns-dev mapping a kind-c4 raid0 names its td in that raid0's own
// name, and the td's kind-c5 dm-error carries the same ids. Where it does not
// — a table this build never wrote, or an ns-dev over a dm-clone whose td
// cannot be read back — the device is reloaded onto an error target of its
// own size instead, which is the same thing one indirection shorter.
//
// A device already parked is left alone but RESUMED if it is suspended:
// disabling the nvmet namespace above it closes its backing device, and that
// does not complete on a suspended dm device.
func (s *CnAgentServer) parkByTable(
	ctx context.Context,
	name string,
	actual *cnActual,
) {
	dev, err := s.dm.Info(ctx, name)
	if err != nil || dev == nil {
		return
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		slog.ErrorContext(ctx, "reading a device table before removal failed",
			slog.String("dm", name),
			slog.String("error", err.Error()))
		return
	}
	var sectors uint64
	for _, target := range targets {
		sectors += target.Length
	}
	if sectors == 0 {
		return
	}
	resume := func() {
		if !dev.Suspended {
			return
		}
		if err := s.dm.Resume(ctx, name); err != nil {
			slog.ErrorContext(ctx, "resuming a parked device failed",
				slog.String("dm", name),
				slog.String("error", err.Error()))
		}
	}
	// The reload below is the whole park: its internal flushing suspend
	// completes the in-flight IO, and installing the new table before the
	// resume is what errors the stragglers instead of replaying them onto a
	// stack that is about to go.
	reload := func(table string) {
		if err := s.dm.Reload(ctx, name, table); err != nil {
			slog.ErrorContext(ctx, "parking a device before removal failed",
				slog.String("dm", name),
				slog.String("error", err.Error()))
		}
	}
	if len(targets) == 1 {
		if targets[0].Type == "error" && targets[0].Length == sectors {
			resume()
			return
		}
		if targets[0].Type == "linear" && len(targets[0].Args) > 0 {
			backing, ok := actual.ours[actual.byDevNo[targets[0].Args[0]]]
			if ok && backing.Kind == common.DmKindCnError {
				resume()
				return
			}
			if ok && backing.Kind == common.DmKindCnRaid0 {
				// c4 and c5 carry the same ids: [sp, td].
				errName := s.nf.CnErrorName(backing.ClusterId, backing.NodeId,
					backing.Ids[0], backing.Ids[1])
				errDev, err := s.dm.Info(ctx, errName)
				if err == nil && errDev != nil {
					devNo, err := s.dm.DevNo(ctx, s.nf.DmPath(errName))
					if err == nil {
						reload(agent.LinearTable(sectors, devNo, 0))
						return
					}
				}
			}
		}
	}
	reload(agent.ErrorTable(sectors))
}

// removeDms removes each named device and reports whether any is still there.
func (s *CnAgentServer) removeDms(
	ctx context.Context,
	res *agent.SweepResult,
	groups ...[]string,
) bool {
	left := false
	for _, names := range groups {
		for _, name := range names {
			if !s.removeDmVerified(ctx, name) {
				res.Add(agent.LeftoverKindDm, name)
				left = true
			}
		}
	}
	return left
}

func (s *CnAgentServer) reportDms(res *agent.SweepResult, groups ...[]string) {
	for _, names := range groups {
		for _, name := range names {
			res.Add(agent.LeftoverKindDm, name)
		}
	}
}

// removeThins is L7. A thin volume is only DEACTIVATED unless its slice's
// thin-pool survives the pass: the pool metadata lives on the DN legs, and a
// `delete` message against a pool that is itself going away is both pointless
// and unsendable. The dev_id comes from the live table — `0 N thin <pool
// devno> <dev_id>` — never from the request, because the table is what the
// kernel will act on.
func (s *CnAgentServer) removeThins(
	ctx context.Context,
	chain *cnChain,
	actual *cnActual,
	wanted *cnWanted,
	res *agent.SweepResult,
) bool {
	left := false
	for _, name := range chain.thins {
		poolName, devId := s.thinIdentity(ctx, name, actual)
		if !s.removeDmVerified(ctx, name) {
			res.Add(agent.LeftoverKindDm, name)
			left = true
			continue
		}
		if poolName == "" || devId == "" {
			continue
		}
		if _, poolWanted := wanted.dms[poolName]; !poolWanted {
			continue
		}
		s.deleteThinIdByName(ctx, poolName, devId)
	}
	return left
}

// thinIdentity reads a thin volume's pool and dev_id out of its live table.
// Both come back empty when the table cannot be read — the device may already
// be gone — which makes the caller skip the `delete` rather than guess an id.
func (s *CnAgentServer) thinIdentity(
	ctx context.Context,
	name string,
	actual *cnActual,
) (string, string) {
	targets, err := s.dm.Table(ctx, name)
	if err != nil || len(targets) != 1 || targets[0].Type != "thin" ||
		len(targets[0].Args) < 2 {
		return "", ""
	}
	return actual.byDevNo[targets[0].Args[0]], targets[0].Args[1]
}

// deleteThinIdByName is the CN14 deletion message addressed by name. The pool
// is probed first, so a pool that has meanwhile gone is silent.
func (s *CnAgentServer) deleteThinIdByName(
	ctx context.Context,
	poolName string,
	devId string,
) {
	pool, err := s.dm.Info(ctx, poolName)
	if err != nil || pool == nil {
		return
	}
	if err := s.dm.Message(
		ctx, poolName, 0, "delete "+devId); err != nil {
		slog.ErrorContext(ctx, "thin-pool delete message failed",
			slog.String("pool", poolName),
			slog.String("error", err.Error()))
	}
}

// stopArrayVerified stops one array and verifies it from sysfs. The node it
// stops is the one sysfs named — /dev/mdN, never /dev/md/<name>, which
// depends on udev having run — and nothing here runs `mdadm --detail`, whose
// member reads block until failfast on a leg whose DN side has gone.
func (s *CnAgentServer) stopArrayVerified(
	ctx context.Context,
	array MdArray,
) bool {
	if err := s.md.Stop(ctx, array.Dev); err != nil {
		slog.ErrorContext(ctx, "stopping md array failed",
			slog.String("array", array.Dev),
			slog.String("error", err.Error()))
	}
	gone, err := s.md.Gone(ctx, array.Dev)
	if err != nil {
		slog.ErrorContext(ctx, "verifying an md stop failed",
			slog.String("array", array.Dev),
			slog.String("error", err.Error()))
		return false
	}
	return gone
}

// disconnectVerified drops an nvme host connection and re-probes sysfs for
// it. "Gone" is the absence of any CONTROLLER, not the absence of the
// subsystem directory: the kernel keeps /sys/class/nvme-subsystem/nvme-subsysN
// around after its last controller is deleted, with its attributes readable
// and its namespace and controller nodes gone. Such a subsystem holds nothing
// open — no block device, no path — and waiting for the directory itself
// would report a leftover for ever and re-drive the worker every round.
func (s *CnAgentServer) disconnectVerified(
	ctx context.Context,
	nqn string,
) bool {
	s.disconnect(ctx, nqn)
	state, err := s.host.ListSubsys(ctx, nqn)
	if err != nil {
		slog.ErrorContext(ctx, "verifying an nvme disconnect failed",
			slog.String("nqn", nqn),
			slog.String("error", err.Error()))
		return false
	}
	return !state.Found || len(state.Paths) == 0
}

// sweepCloneChunks deletes the bitmap chunk files of every clone this cntlr
// no longer carries (SH7). It is a sweep of the LOCAL STORE against the
// request, not a side effect of removing the clone's wrapper: a standby
// builds no wrapper at all, so keying the deletion off one left a deleted
// clone's chunks on disk for ever and the worker re-pushing them.
//
// A clone the role or the sp_level merely SUPPRESSES keeps its chunks
// applied-by-file (CN19, CN22): deleting them on every standby converge would
// make the worker re-push them every round, and would leave a promoted
// standby's §11.5 rebuild nothing to skip with. The test is membership of
// clone_list, which is exactly "the clone still exists".
func (s *CnAgentServer) sweepCloneChunks(
	ctx context.Context,
	st *cntlrState,
) {
	ptr := st.req.GetCntlrPointer()
	var paths []string
	for cloneId := range st.chunks {
		if findClone(st.req, cloneId) != nil {
			continue
		}
		paths = append(paths, s.cloneChunkPathsOf(st,
			st.req.GetClusterId(), st.req.GetCnId(),
			ptr.GetSpId(), cloneId)...)
		delete(st.chunks, cloneId)
	}
	if len(paths) == 0 {
		return
	}
	sort.Strings(paths)
	if err := s.store.Remove(ctx, paths...); err != nil {
		slog.ErrorContext(ctx, "removing clone bitmap chunks failed",
			slog.String("error", err.Error()))
	}
}

// ---------------------------------------------------------------------------
// Cross-sp objects: the ones whose name carries no sp of ours
// ---------------------------------------------------------------------------

// srcNqnsInUse is the set of clone-source subsystems this CN still needs: the
// ones a stored cntlr names as a clone's src_nqn, plus the ones a live kind-c7
// dm-clone maps as its source device. A clone's source NQN belongs to the
// SOURCE sp, not to the cntlr that connects to it, so there is no sp field to
// attribute it by — "somebody here still wants it" is the only test there is.
//
// The stored-request half is what makes this safe to run outside the node
// write lock: a cntlr's request is stored (putCntlr) before its converge
// issues any `nvme connect`, so a source cannot be connected by a build whose
// request is not yet visible here.
func (s *CnAgentServer) srcNqnsInUse(
	ctx context.Context,
	clusterId uint64,
	cnId uint64,
	actual *cnActual,
	res *agent.SweepResult,
) map[string]struct{} {
	inUse := make(map[string]struct{})
	for _, key := range s.cntlrKeysOf(clusterId, cnId) {
		st := s.getCntlr(key)
		if st == nil {
			continue
		}
		for _, clone := range st.req.GetCloneList() {
			if nqn := clone.GetSrcNqn(); nqn != "" {
				inUse[nqn] = struct{}{}
			}
		}
	}
	// A dm-clone that is live right now holds its source open whatever any
	// request says; disconnecting under it would strand its hydration IO.
	srcDevNos := make(map[string]struct{})
	// unknown means at least one live kind-c7 device would not say what it
	// maps. Its source is then in the set we cannot enumerate, so NOTHING may
	// be called unused this pass: see the return below.
	unknown := false
	for name, dn := range actual.ours {
		if dn.Kind != common.DmKindCnCloneFinal {
			continue
		}
		targets, err := s.dm.Table(ctx, name)
		if err != nil {
			// The device may have gone — this runs at L5, after L3 removed
			// the unwanted clones — but if it did NOT, treating it as mapping
			// nothing could disconnect a live source. Absence is CONFIRMED,
			// never assumed.
			dev, infoErr := s.dm.Info(ctx, name)
			if infoErr == nil && dev == nil {
				continue
			}
			res.Fail("dm table of "+name, err)
			// Recording the failure is not the same as acting on it: nothing
			// downstream reads res.failures before removing, so the omission
			// alone would read as "this clone maps no source" — the exact
			// inverse of the rule three lines above.
			unknown = true
			continue
		}
		// CloneTable: `0 <sectors> clone <meta> <dest> <src> <region> …`.
		if len(targets) == 1 && targets[0].Type == "clone" &&
			len(targets[0].Args) >= 3 {
			srcDevNos[targets[0].Args[2]] = struct{}{}
		}
	}
	if len(srcDevNos) == 0 && !unknown {
		return inUse
	}
	for _, state := range actual.hostSubsys {
		parts, ok := common.ParseNqn(state.Nqn)
		if !ok || parts.Kind != common.NqnKindXfer {
			// Only a clone source can be the thing a dm-clone maps, and the
			// namespace node is read only for those: on a node holding a
			// leg per group that listing would otherwise be one forked `ls`
			// per leg, per pass.
			continue
		}
		if unknown {
			// A live clone whose table we could not read may be mapping this
			// one. Keeping it costs a round; disconnecting it strands a
			// hydration.
			inUse[state.Nqn] = struct{}{}
			continue
		}
		devPath, err := s.host.SubsysDevicePath(ctx, state)
		if err != nil || devPath == "" {
			// Unresolvable: keep it. A source a live clone might be using is
			// never disconnected on a guess.
			inUse[state.Nqn] = struct{}{}
			continue
		}
		devNo, err := s.dm.DevNo(ctx, devPath)
		if err != nil {
			inUse[state.Nqn] = struct{}{}
			continue
		}
		if _, used := srcDevNos[devNo]; used {
			inUse[state.Nqn] = struct{}{}
		}
	}
	return inUse
}

// unownedSrcNqns is every kind-4 connection this host holds for our cluster
// that srcNqnsInUse does not claim.
func (s *CnAgentServer) unownedSrcNqns(
	ctx context.Context,
	clusterId uint64,
	cnId uint64,
	actual *cnActual,
	res *agent.SweepResult,
) []string {
	inUse := s.srcNqnsInUse(ctx, clusterId, cnId, actual, res)
	var out []string
	for _, state := range actual.hostSubsys {
		parts, ok := common.ParseNqn(state.Nqn)
		if !ok || parts.Kind != common.NqnKindXfer ||
			parts.Ids[0] != clusterId {
			continue
		}
		if _, used := inUse[state.Nqn]; used {
			continue
		}
		out = append(out, state.Nqn)
	}
	sort.Strings(out)
	return out
}

// orphanCloneMetaNames is every kind-cb wrapper of this CN whose clone no
// stored cntlr wants any more. It is reconcileCloneMeta's rule, computed from
// the sweep's own snapshot so the verdict and the removal agree.
func (s *CnAgentServer) orphanCloneMetaNames(
	clusterId uint64,
	cnId uint64,
	actual *cnActual,
) []string {
	wanted := make(map[string]struct{})
	for _, key := range s.cntlrKeysOf(clusterId, cnId) {
		st := s.getCntlr(key)
		if st == nil {
			continue
		}
		spId := st.req.GetCntlrPointer().GetSpId()
		for _, clone := range st.req.GetCloneList() {
			wanted[s.nf.CnCloneMetaDmName(
				clusterId, cnId, spId, clone.GetCloneId())] = struct{}{}
		}
	}
	var out []string
	for name, dn := range actual.ours {
		if dn.Kind != common.DmKindCnCloneMeta {
			continue
		}
		if _, keep := wanted[name]; !keep {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// The two scopes
// ---------------------------------------------------------------------------

// sweepCntlr is the cntlr-level pass that replaced the retire phase. It runs
// in convergeCntlr in retire's place, under the node read lock and this
// cntlr's object lock, and it removes only objects attributed to this cntlr's
// sp — which is what lets two cntlrs of one CN converge concurrently.
//
// remove = false is the read-only VERDICT the Check rounds and Get*Info take:
// the same enumeration and the same comparison, with nothing touched (CN23).
func (s *CnAgentServer) sweepCntlr(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	remove bool,
) *agent.SweepResult {
	res := &agent.SweepResult{}
	actual := s.enumerateCn(ctx, plan.clusterId, plan.cnId, res)
	// AN ENUMERATION THAT DID NOT ANSWER LICENSES NO REMOVAL. Everything
	// below is "actual minus desired", and with the listing unanswered
	// `actual` is empty for want of an answer — which subtracts to "remove
	// nothing" in the safe direction but reads as "there is nothing left"
	// everywhere a REMOVAL is gated on absence: the layer stop rule never
	// fires, so lower layers run as though the ones above them had
	// succeeded, and the live-device tests that protect a shared object
	// (a dm-clone still mapping its source) lose the half of their evidence
	// that comes from the node. res.Fail has already made the verdict
	// non-OK, so the worker re-drives; this pass reports and touches
	// nothing.
	if !actual.dmListed {
		remove = false
	}
	wanted := cntlrWanted(plan)
	chain := s.buildChain(ctx, plan.clusterId, plan.cnId, plan.spId,
		actual, wanted, res)
	chain.srcNqns = func() []string {
		return s.unownedSrcNqns(ctx, plan.clusterId, plan.cnId, actual, res)
	}
	if !remove {
		s.reportChain(chain, res)
		res.Log(ctx, s.cntlrIdAttrs(plan)...)
		return res
	}
	s.cntlrPreSteps(ctx, plan, actual, wanted)
	s.runChain(ctx, st, plan, chain, actual, wanted, res)
	// The local store follows the same rule as the devices: a clone's chunk
	// files go when the clone leaves the request, and stay while it is only
	// suppressed. Last, so they go with the rest of the clone's state (SH7).
	s.sweepCloneChunks(ctx, st)
	// The ResInfo histories follow the same rule as the devices: whatever the
	// desired state no longer names is forgotten, so a later rebuild of the
	// same id reports a fresh epoch rather than the dead object's (SH14).
	st.tracker.Keep(cntlrResKeys(plan))
	res.Log(ctx, s.cntlrIdAttrs(plan)...)
	return res
}

// wantedProbers is the set of legs the CN11 health prober still covers: a
// standby runs none, a level at or above SP_LEVEL_NO_SIDE runs none, and a
// provisioning-deferred leg has no wrapper to probe ([D15]).
func wantedProbers(plan *cntlrPlan) map[uint64]struct{} {
	wanted := make(map[uint64]struct{})
	if !plan.primary || !plan.wantLeg {
		return wanted
	}
	for _, lp := range plan.legs {
		if lp.provisioning {
			continue
		}
		wanted[lp.legId] = struct{}{}
	}
	return wanted
}

// cntlrResKeys is every ResTracker key this plan's objects can use — the
// level-independent shape, because the build phase reports a level-suppressed
// resource MISSING under the same key (CN19). Anything else in the tracker
// belongs to an object that has left the desired state.
func cntlrResKeys(plan *cntlrPlan) map[string]struct{} {
	keys := make(map[string]struct{})
	add := func(key string) { keys[key] = struct{}{} }
	for _, lp := range plan.legs {
		add(resKeyOf(resKeyLegFmt, lp.legId))
	}
	for _, gp := range plan.grps {
		add(resKeyOf(resKeyGrpFmt, gp.grpId))
	}
	for _, sp := range plan.slices {
		add(resKeyOf(resKeyPoolFmt, sp.sliceId))
		add(resKeyOf(resKeyPoolMetaFmt, sp.sliceId))
		add(resKeyOf(resKeyPoolDataFmt, sp.sliceId))
	}
	for _, tp := range plan.tds {
		add(resKeyOf(resKeyRaid0Fmt, tp.tdId))
		add(resKeyOf(resKeyTdErrorFmt, tp.tdId))
		for _, sp := range plan.slices {
			add(thinResKey(tp.tdId, sp.sliceId))
		}
	}
	for _, np := range plan.namespaces {
		add(resKeyOf(resKeyNsDevFmt, np.nsId))
		add(resKeyOf(resKeyNamespaceFmt, np.nsId))
	}
	for _, ssp := range plan.subsystems {
		add(resKeyOf(resKeySubsysFmt, ssp.ssId))
	}
	for _, xp := range plan.xfers {
		add(resKeyOf(resKeyXferDmFmt, xp.xferId))
		add(resKeyOf(resKeyXferSsFmt, xp.xferId))
		add(resKeyOf(resKeyXferNsFmt, xp.xferId))
	}
	for _, cp := range plan.clones {
		add(resKeyOf(resKeyCloneTgtFmt, cp.cloneId))
		add(resKeyOf(resKeyCloneDmFmt, cp.cloneId))
		add(resKeyOf(resKeyCloneMetaFmt, cp.cloneId))
	}
	return keys
}

func (s *CnAgentServer) cntlrIdAttrs(plan *cntlrPlan) []any {
	return []any{
		slog.Uint64("cluster_id", plan.clusterId),
		slog.Uint64("cn_id", plan.cnId),
		slog.Uint64("sp_id", plan.spId),
		slog.Uint64("cntlr_id", plan.cntlrId),
	}
}

// cntlrPreSteps are the transitions the old retire phase computed from the
// previously applied plan and this one derives from the live tables instead.
// All three exist so the layers below them can remove anything at all.
func (s *CnAgentServer) cntlrPreSteps(
	ctx context.Context,
	plan *cntlrPlan,
	actual *cnActual,
	wanted *cnWanted,
) {
	// P1, ANA: every namespace the desired state wants inaccessible is moved
	// there before anything under it is touched (§11.1 old_primary step 1).
	// A provisioning-deferred namespace is already inaccessible by CN16's
	// fourth conjunct, so this covers a fresh SP's first converge too.
	for _, np := range plan.namespaces {
		if np.anaGrpId == common.AnaGrpIdInaccessible {
			s.setAnaLogged(ctx, np.ss.nqn, np.nsIdx)
		}
	}
	for _, xp := range plan.xfers {
		if plan.xferAnaGrpId(xp) == common.AnaGrpIdInaccessible {
			s.setAnaLogged(ctx, xp.nqn, int(xp.xfer.GetOriNsIdx()))
		}
	}
	// P2, park: an ns-dev OF THE PLAN whose LIVE table still maps something
	// this pass is about to remove is reloaded onto its td's dm-error first.
	// Of the plan, not of the wanted set: the loop below is over
	// plan.namespaces, and at SP_LEVEL_DISABLE the plan names every ns-dev
	// while the wanted set holds none of them — which is exactly the level
	// at which everything under them is about to be removed. The
	// reload's own flushing suspend is what completes the in-flight host IO,
	// and without it the removal below would fail EBUSY behind the ns-dev.
	//
	// The old retire phase computed this from the previous plan ("does this
	// namespace's OLD td still exist?"). The live table is the same answer
	// without the memory — and it is also right for a device an interrupted
	// pass left pointing somewhere the plan never described.
	for _, np := range plan.namespaces {
		if np.td == nil {
			continue
		}
		if np.backingName == np.td.errorName ||
			s.nsDevMapsUnwanted(ctx, np, actual, wanted) {
			s.parkNsDevLogged(ctx, np)
		}
	}
	// P3, demote: a transfer device this cntlr no longer serves carries an
	// error table of its own size, which is what makes it let go of the
	// origin td's raid0 (CN17) so L6 can remove it.
	for _, xp := range plan.xfers {
		if plan.xferServed(xp) {
			continue
		}
		s.demoteXfer(ctx, xp)
	}
}

// demoteXfer reloads a live transfer device onto an error table of its own
// size — the CN17 shape for a cntlr that does not serve it. The demotion is
// what makes the rest of the pass possible: a CnXferFinalName still mapping
// the origin td's raid0 holds it open, so the raid0's removal would fail
// EBUSY and take the thin volumes, the pool, the concats and `mdadm --stop`
// down with it (CN19's NO_THINPOOL row).
//
// The size comes from the plan when the origin namespace still resolves, and
// from the device's OWN LIVE TABLE when it does not — a transfer whose origin
// td was deleted in the same request has no plan size left. The old retire
// phase read that fallback off the previously applied plan, which is exactly
// the remembered state this design does without.
func (s *CnAgentServer) demoteXfer(ctx context.Context, xp *xferPlan) {
	dev, err := s.dm.Info(ctx, xp.finalName)
	if err != nil || dev == nil {
		return
	}
	sectors := xp.sectors
	if sectors == 0 {
		targets, err := s.dm.Table(ctx, xp.finalName)
		if err != nil {
			slog.ErrorContext(ctx, "reading a transfer device table failed",
				slog.String("dm", xp.finalName),
				slog.String("error", err.Error()))
			return
		}
		for _, target := range targets {
			sectors += target.Length
		}
	}
	if sectors == 0 {
		return
	}
	if err := s.ensureDmError(ctx, xp.finalName, sectors); err != nil {
		slog.ErrorContext(ctx, "demoting a transfer device failed",
			slog.String("dm", xp.finalName),
			slog.String("error", err.Error()))
	}
}

// nsDevMapsUnwanted reports whether one of the plan's ns-devs has a live
// table mapping a device this pass does not want. The ns-dev itself need not
// be wanted — at SP_LEVEL_DISABLE none of them is. The table is read through the snapshot's
// devno index, so the answer is about what the kernel holds, not about what
// any plan remembers.
func (s *CnAgentServer) nsDevMapsUnwanted(
	ctx context.Context,
	np *nsPlan,
	actual *cnActual,
	wanted *cnWanted,
) bool {
	targets, err := s.dm.Table(ctx, np.devName)
	if err != nil || len(targets) != 1 || len(targets[0].Args) == 0 {
		return false
	}
	backing, ok := actual.byDevNo[targets[0].Args[0]]
	if !ok {
		return false
	}
	if _, keep := wanted.dms[backing]; keep {
		return false
	}
	// A backing that is not one of ours at all (a foreign device) is not
	// something this sweep removes, so parking for it would be pointless.
	_, ours := actual.ours[backing]
	return ours
}

// reportChain is the read-only half: every unwanted object, named, with
// nothing touched.
func (s *CnAgentServer) reportChain(chain *cnChain, res *agent.SweepResult) {
	for _, ref := range chain.nvmetNs {
		res.Add(agent.LeftoverKindNvmetNs, fmt.Sprintf("%s/%d", ref.nqn, ref.nsid))
	}
	for _, nqn := range chain.nvmetSs {
		res.Add(agent.LeftoverKindNvmet, nqn)
	}
	s.reportDms(res, chain.nsDevs, chain.xferDms, chain.clones,
		chain.cloneMeta, chain.raid0s, chain.tdErrors, chain.thins,
		chain.pools, chain.poolMetas, chain.poolDatas, chain.grpDms,
		chain.legDms)
	for _, nqn := range chain.srcNqnList() {
		res.Add(agent.LeftoverKindNvme, nqn)
	}
	for _, array := range chain.arrays {
		res.Add(agent.LeftoverKindMd, array.Dev)
	}
	for _, nqn := range chain.legNqns {
		res.Add(agent.LeftoverKindNvme, nqn)
	}
}

// sweepCn is the node-level pass: everything of an sp whose pointer has left
// this CN's list, plus the objects no sp can be read off at all. It runs
// under the node write lock, so no cntlr converge, no Check round and no push
// runs beside it — which is what makes removing an UNOWNED object safe.
//
// An sp that IS in the pointer list is never touched here, even when it has
// no cntlr file: CN8 introduces the pointer before the SyncupCntlr, and after
// a lost --local-store the resources exist and must be re-adopted probe-first
// by that syncup rather than swept.
func (s *CnAgentServer) sweepCn(
	ctx context.Context,
	st *cnState,
	remove bool,
) *agent.SweepResult {
	res := &agent.SweepResult{}
	clusterId := st.req.GetClusterId()
	cnId := st.req.GetCnId()
	actual := s.enumerateCn(ctx, clusterId, cnId, res)
	// AN ENUMERATION THAT DID NOT ANSWER LICENSES NO REMOVAL. Everything
	// below is "actual minus desired", and with the listing unanswered
	// `actual` is empty for want of an answer — which subtracts to "remove
	// nothing" in the safe direction but reads as "there is nothing left"
	// everywhere a REMOVAL is gated on absence: the layer stop rule never
	// fires, so lower layers run as though the ones above them had
	// succeeded, and the live-device tests that protect a shared object
	// (a dm-clone still mapping its source) lose the half of their evidence
	// that comes from the node. res.Fail has already made the verdict
	// non-OK, so the worker re-drives; this pass reports and touches
	// nothing.
	if !actual.dmListed {
		remove = false
	}

	known := make(map[uint64]struct{})
	for _, ptr := range st.req.GetCntlrPointerList() {
		known[ptr.GetSpId()] = struct{}{}
	}
	unwantedSps := make(map[uint64]struct{})
	noteSp := func(spId uint64) {
		if _, ok := known[spId]; ok {
			return
		}
		unwantedSps[spId] = struct{}{}
	}
	for _, dn := range actual.ours {
		noteSp(dn.SpId())
	}
	for _, array := range actual.arrays {
		if owner, ok := arrayOwner(clusterId, cnId, array); ok {
			noteSp(owner)
		}
	}
	var unownedSs []string
	for _, nqn := range actual.nvmetSubsys {
		attr := actual.nvmetOwner[nqn]
		switch {
		case !attr.ours:
		case attr.unowned:
			unownedSs = append(unownedSs, nqn)
		default:
			noteSp(attr.spId)
		}
	}
	for _, state := range actual.hostSubsys {
		parts, ok := common.ParseNqn(state.Nqn)
		if ok && parts.Kind == common.NqnKindSideToCn &&
			parts.Ids[0] == clusterId && parts.Ids[3] == cnId {
			noteSp(parts.Ids[1])
		}
	}
	sort.Strings(unownedSs)

	spIds := make([]uint64, 0, len(unwantedSps))
	for spId := range unwantedSps {
		spIds = append(spIds, spId)
	}
	sort.Slice(spIds, func(i, j int) bool { return spIds[i] < spIds[j] })
	for _, spId := range spIds {
		chain := s.buildChain(ctx, clusterId, cnId, spId, actual, nil, res)
		if !remove {
			s.reportChain(chain, res)
			continue
		}
		s.runChain(ctx, nil, nil, chain, actual, newCnWanted(), res)
	}

	// Cross-sp items, each its own one-layer chain: they have no stack of
	// ours above or below them.
	for _, nqn := range unownedSs {
		if !remove {
			res.Add(agent.LeftoverKindNvmet, nqn)
			continue
		}
		if !s.removeExportVerified(ctx, nqn) {
			res.Add(agent.LeftoverKindNvmet, nqn)
		}
	}
	for _, nqn := range s.unownedSrcNqns(ctx, clusterId, cnId, actual, res) {
		if !remove {
			res.Add(agent.LeftoverKindNvme, nqn)
			continue
		}
		if !s.disconnectVerified(ctx, nqn) {
			res.Add(agent.LeftoverKindNvme, nqn)
		}
	}
	// The kind-cb wrappers are swept by reconcileCloneMeta, which already
	// worked this way before the design existed: it enumerates the arena and
	// removes every wrapper no stored cntlr's clone_list names. It is called
	// from here and nowhere else, so the node write lock this pass holds is
	// what makes its "every stored cntlr" set stable ([D14]).
	if remove {
		s.reconcileCloneMeta(ctx, clusterId, cnId)
	}
	for _, name := range s.orphanCloneMetaNames(clusterId, cnId, actual) {
		if remove {
			// The snapshot predates reconcileCloneMeta, so the verdict is
			// taken from a fresh probe rather than from it.
			if dev, err := s.dm.Info(ctx, name); err == nil && dev == nil {
				continue
			}
		}
		res.Add(agent.LeftoverKindDm, name)
	}
	res.Log(ctx,
		slog.Uint64("cluster_id", clusterId),
		slog.Uint64("cn_id", cnId))
	return res
}

// cntlrVerdict is the read-only cntlr-level comparison the Check rounds and
// GetCntlrInfo take. A cntlr whose stored conf is unusable is refused by §7
// before any converge runs, so it has no verdict either: reporting leftovers
// for a cntlr this agent deliberately did not build would name objects it is
// not allowed to remove.
func (s *CnAgentServer) cntlrVerdict(
	ctx context.Context,
	st *cntlrState,
) *agent.SweepResult {
	if agent.ValidateBdevConf(st.req.GetBdevConf()) != nil {
		return &agent.SweepResult{}
	}
	return s.sweepCntlr(ctx, st, newCntlrPlan(s.nf, st.req), false)
}
