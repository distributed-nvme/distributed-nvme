package dnagent

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
// Teardown by sweep — the dn half (architecture.md §9.8, dnagent.md DN6)
//
// Same principle as the cn's: what to remove is derived by subtracting the
// desired state from what the node actually holds, and nothing about a failed
// removal is remembered. The dn's version of the bug the design started from
// is worse than a leak: teardownForbidden and retireMigrDst diffed against
// sideState.appliedCnIds / appliedMigrSrc / appliedMigrDst, and every one of
// those fields was overwritten on the way through the converge — so a per-CN
// stack whose removal failed was forgotten, while the ALLOCATION RECORD under
// it could still be freed by a probe that read a killed `dmsetup info` as
// "the device is gone". Freeing a record whose device still maps those
// extents hands them to the next side: a corruption path, not a leak.
//
// Two dn-specific rules on top of the shared ones:
//
//   - a record is released only after the sweep has VERIFIED its device is
//     gone, and only for a side or migration the authoritative pointer lists
//     prove unwanted (the sweepOrphanRecords proof, unchanged);
//   - migration objects are keyed by (sp, migr) and belong to no side, so
//     they are judged by a claim rule over every locally stored side rather
//     than by an sp id in the name. That is what lets a FINISHED migration's
//     dm-clone go in the pass that repoints the linear off it, without any
//     "applied destination" memory.
// ---------------------------------------------------------------------------

// dnActual is one snapshot of everything on the node this agent could own,
// taken fresh on every pass and thrown away afterwards.
type dnActual struct {
	dms     map[string]string
	ours    map[string]common.DmName
	byDevNo map[string]string
	// dmListed says the `dmsetup ls` behind the three maps above ANSWERED;
	// see the cn twin. A dn removal also gates an on-disk allocation record,
	// so deriving one from an unanswered enumeration is the corruption path,
	// not merely a leak.
	dmListed bool
	// hostSubsys is every nvme subsystem this host holds a controller for —
	// on the dn that means the migration-source connections a destination
	// side makes.
	hostSubsys []agent.SubsysBrief
	// nvmetSubsys is every subsystem in the target's configfs tree.
	nvmetSubsys []string
}

func (s *DnAgentServer) enumerateDn(
	ctx context.Context,
	clusterId uint64,
	dnId uint64,
	res *agent.SweepResult,
) *dnActual {
	actual := &dnActual{
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
			if !ok || dn.Role() != common.DmRoleDn ||
				dn.ClusterId != clusterId || dn.NodeId != dnId {
				continue
			}
			actual.ours[name] = dn
		}
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
	return actual
}

// dmsOfKind is the node's devices of one kind that pass the filter, sorted so
// a pass is deterministic.
func (a *dnActual) dmsOfKind(
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

// ---------------------------------------------------------------------------
// Claims: who still wants a side, and who still wants a migration
// ---------------------------------------------------------------------------

// sideClaims is what the locally stored sides still want. It is recomputed on
// every pass from the requests themselves — the one authoritative copy of the
// desired state this agent holds — and never from anything the converge left
// behind.
type sideClaims struct {
	// exports are the :2: subsystem NQNs some wanted side still exports.
	exports map[string]struct{}
	// migrSrcDm / migrSrcNqn are the (sp, migr) pairs some stored side still
	// plays the SOURCE of, SPLIT BY THE GATE THAT KEEPS EACH OBJECT. The
	// source has two, and they are not wanted at the same levels: the `d2`
	// linear under wantDm, its `:3:` export under wantExport. A single claim
	// set would have to pick one, and either choice is wrong at
	// SP_LEVEL_NO_SIDE — the level whose whole point is that the sp is off
	// the network, and which wants the linear while wanting no export at all.
	// A claim is not a statement that an object exists; it is a statement
	// that somebody still WANTS it, so it has to carry that somebody's gate.
	migrSrcDm  map[[2]uint64]struct{}
	migrSrcNqn map[[2]uint64]struct{}
	// migrDst is the same for the destination role, whose objects share one
	// gate (wantMigr).
	migrDst map[[2]uint64]struct{}
}

// collectClaims walks every side this agent holds state for. A side's request
// is stored (putSide) before its converge builds anything, so nothing can be
// exported or connected by a side whose claim is not already visible here —
// which is what makes it safe to run this outside the node write lock.
func (s *DnAgentServer) collectClaims(
	clusterId uint64,
	dnId uint64,
) *sideClaims {
	claims := &sideClaims{
		exports:    make(map[string]struct{}),
		migrSrcDm:  make(map[[2]uint64]struct{}),
		migrSrcNqn: make(map[[2]uint64]struct{}),
		migrDst:    make(map[[2]uint64]struct{}),
	}
	for _, key := range s.sideKeysOf(clusterId, dnId) {
		st := s.getSide(key)
		if st == nil {
			continue
		}
		// extentSize is irrelevant: only names and gates are read here, and
		// both are pure functions of the request.
		plan := newSidePlan(s.nf, st.req, 0)
		if plan.wantExport {
			for _, cnId := range plan.cnIds {
				claims.exports[plan.sideNqn(cnId)] = struct{}{}
			}
		}
		if plan.migrSrc != nil {
			key := [2]uint64{plan.spId, plan.migrSrc.GetMigrId()}
			if plan.wantDm {
				claims.migrSrcDm[key] = struct{}{}
			}
			if plan.wantExport {
				claims.migrSrcNqn[key] = struct{}{}
			}
		}
		if plan.wantMigr && plan.migrDst != nil {
			claims.migrDst[[2]uint64{
				plan.spId, plan.migrDst.GetMigrId()}] = struct{}{}
		}
	}
	return claims
}

// ---------------------------------------------------------------------------
// The wanted set of one side
// ---------------------------------------------------------------------------

// dnWanted is what one side's desired state wants to exist. Every row is
// pinned against convergeSide, not against a design table:
//
//	d4 side device        always   (ensureSideDev — every level, DN11)
//	d0 error, d1 linear   wantDm   (ensureCnDm, one pair per cn in cnIds)
//	:2: export            wantExport (ensureCnExports)
//	d2 migr-src linear    migrSrc != nil && wantDm   (ensureMigrSrc)
//	:3: export            migrSrc != nil && wantExport
//	d3 clone, d5 wrapper,
//	:3: connection        wantMigr (ensureMigrDst)
//
// migrSrc here is the EFFECTIVE one: a source conf whose destination has not
// provisioned is exactly equivalent to no source conf at all (§11.2), so a
// deferred source wants nothing and the side keeps serving unchanged.
type dnWanted struct {
	dms      map[string]struct{}
	nvmetSs  map[string]struct{}
	hostNqns map[string]struct{}
}

func sideWanted(plan *sidePlan) *dnWanted {
	w := &dnWanted{
		dms:      make(map[string]struct{}),
		nvmetSs:  make(map[string]struct{}),
		hostNqns: make(map[string]struct{}),
	}
	w.dms[plan.sideDevName] = struct{}{}
	for _, cnId := range plan.cnIds {
		if plan.wantDm {
			w.dms[plan.errName(cnId)] = struct{}{}
			w.dms[plan.linearName(cnId)] = struct{}{}
		}
		if plan.wantExport {
			w.nvmetSs[plan.sideNqn(cnId)] = struct{}{}
		}
	}
	if plan.migrSrc != nil {
		if plan.wantDm {
			w.dms[plan.migrSrcName()] = struct{}{}
		}
		if plan.wantExport {
			w.nvmetSs[plan.migrSrcNqn()] = struct{}{}
		}
	}
	if plan.wantMigr {
		w.dms[plan.migrFinalName()] = struct{}{}
		w.dms[plan.migrMetaDmName()] = struct{}{}
		w.hostNqns[plan.srcNqnOfDst()] = struct{}{}
	}
	return w
}

// sideResKeys is every ResTracker key this side's objects can use. Anything
// else in the tracker belongs to an object that has left the desired state,
// and leaving it there would let the NEXT object with the same id inherit a
// dead one's epoch (SH14).
func sideResKeys(plan *sidePlan) map[string]struct{} {
	keys := map[string]struct{}{resKeySideDev: {}}
	for _, cnId := range plan.cnIds {
		keys[resKeyOf(resKeyDmErrorFmt, cnId)] = struct{}{}
		keys[resKeyOf(resKeyDmLinearFmt, cnId)] = struct{}{}
		keys[resKeyOf(resKeyNvmeofFmt, cnId)] = struct{}{}
	}
	// The RAW source conf is what registers the migr_src_* rows: a source
	// role reports from the moment migr_src_conf appears, deferred or not
	// (§11.2).
	if plan.migrSrcRaw != nil {
		keys[resKeyMigrSrcDm] = struct{}{}
		keys[resKeyMigrSrcNvmeof] = struct{}{}
	}
	if plan.migrDst != nil {
		keys[resKeyMigrDstTarget] = struct{}{}
		keys[resKeyMigrDstClone] = struct{}{}
	}
	return keys
}

// ---------------------------------------------------------------------------
// The chain
// ---------------------------------------------------------------------------

// dnChain is everything of one scope that exists and is not wanted, grouped
// by the layer that removes it (DN6's order, top-down).
type dnChain struct {
	exports  []string // :2: and :3: nvmet subsystems
	linears  []string // d1
	clones   []string // d3
	srcConns []string // :3: nvme host connections
	migrSrcs []string // d2
	errors   []string // d0
	metas    []string // d5
	sideDevs []string // d4
}

// removeDmVerified removes a dm device and RE-PROBES it. The probe, not the
// exit status, is the evidence: `dmsetup remove` may have been killed after
// the kernel completed it, and — the reason this matters more here than
// anywhere else — the answer gates meta.FreeSide / meta.FreeCloneMeta.
func (s *DnAgentServer) removeDmVerified(
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

func (s *DnAgentServer) removeExportVerified(
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

// disconnectVerified drops an nvme host connection and re-probes sysfs. As on
// the cn side, "gone" is the absence of any CONTROLLER: the kernel keeps the
// subsystem directory after its last controller is deleted, and it then holds
// nothing open.
func (s *DnAgentServer) disconnectVerified(
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

func (s *DnAgentServer) removeDms(
	ctx context.Context,
	res *agent.SweepResult,
	names []string,
) bool {
	left := false
	for _, name := range names {
		if !s.removeDmVerified(ctx, name) {
			res.Add(agent.LeftoverKindDm, name)
			left = true
		}
	}
	return left
}

func reportDms(res *agent.SweepResult, groups ...[]string) {
	for _, names := range groups {
		for _, name := range names {
			res.Add(agent.LeftoverKindDm, name)
		}
	}
}

// runChain removes one scope's unwanted objects top-down. The descent stops
// at the first layer that left something behind: pulling a source out from
// under a live dm-clone strands its hydration IO, and a pass that simply
// re-runs next round costs nothing.
//
// gate decides, per record, whether this pass has the proof needed to release
// it. A record is never freed on the strength of a removal alone: the device
// must be VERIFIED gone and the authoritative pointer lists must show the
// owner is gone too, because freeing extents a live device still maps hands
// them to the next side.
func (s *DnAgentServer) runChain(
	ctx context.Context,
	chain *dnChain,
	res *agent.SweepResult,
	gate recordGate,
) {
	// P0, before every layer: a suspended per-CN linear is resumed. L1
	// disables the nvmet namespace above it, and that write closes the
	// backing device — which does not complete on a suspended dm device. The
	// §11.2 cutover fence leaves exactly such devices behind, and a side torn
	// down inside the grace window is torn down over them ([D12]).
	for _, name := range chain.linears {
		s.resumeIfSuspended(ctx, name)
	}
	stuck := false
	layers := []struct {
		run    func() bool
		report func()
	}{
		{ // L1 — the exports, which is what releases the devices under them.
			run: func() bool {
				left := false
				for _, nqn := range chain.exports {
					if !s.removeExportVerified(ctx, nqn) {
						res.Add(agent.LeftoverKindNvmet, nqn)
						left = true
					}
				}
				return left
			},
			report: func() {
				for _, nqn := range chain.exports {
					res.Add(agent.LeftoverKindNvmet, nqn)
				}
			},
		},
		{ // L2 — the per-CN linears: everything below is one of their table
			// targets, and `dmsetup remove` on a device another dm device
			// still maps fails EBUSY.
			run:    func() bool { return s.removeDms(ctx, res, chain.linears) },
			report: func() { reportDms(res, chain.linears) },
		},
		{ // L3 — the destination dm-clone, and only then the source
			// connection it hydrates through (DN6).
			run: func() bool {
				if s.removeDms(ctx, res, chain.clones) {
					// The source stays connected — pulling it from under a
					// live dm-clone strands the clone's hydration IO — but
					// it is present and unwanted, so it is named.
					for _, nqn := range chain.srcConns {
						res.Add(agent.LeftoverKindNvme, nqn)
					}
					return true
				}
				left := false
				for _, nqn := range chain.srcConns {
					if !s.disconnectVerified(ctx, nqn) {
						res.Add(agent.LeftoverKindNvme, nqn)
						left = true
					}
				}
				return left
			},
			report: func() {
				reportDms(res, chain.clones)
				for _, nqn := range chain.srcConns {
					res.Add(agent.LeftoverKindNvme, nqn)
				}
			},
		},
		{ // L4 — the migration-source linear.
			run:    func() bool { return s.removeDms(ctx, res, chain.migrSrcs) },
			report: func() { reportDms(res, chain.migrSrcs) },
		},
		{ // L5 — the per-CN dm-errors.
			run:    func() bool { return s.removeDms(ctx, res, chain.errors) },
			report: func() { reportDms(res, chain.errors) },
		},
		{ // L6 — the clone-metadata wrapper, and its record once the device
			// is verifiably gone.
			run: func() bool {
				left := false
				for _, name := range chain.metas {
					if !s.removeDmVerified(ctx, name) {
						res.Add(agent.LeftoverKindDm, name)
						left = true
						continue
					}
					s.freeCloneMetaOf(ctx, name, gate, res)
				}
				return left
			},
			report: func() { reportDms(res, chain.metas) },
		},
		{ // L7 — the side device, and its allocation record once the device
			// is verifiably gone.
			run: func() bool {
				left := false
				for _, name := range chain.sideDevs {
					if !s.removeDmVerified(ctx, name) {
						res.Add(agent.LeftoverKindDm, name)
						left = true
						continue
					}
					s.freeSideOf(ctx, name, gate, res)
				}
				return left
			},
			report: func() { reportDms(res, chain.sideDevs) },
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

func reportChain(chain *dnChain, res *agent.SweepResult) {
	for _, nqn := range chain.exports {
		res.Add(agent.LeftoverKindNvmet, nqn)
	}
	reportDms(res, chain.linears, chain.clones, chain.migrSrcs, chain.errors,
		chain.metas, chain.sideDevs)
	for _, nqn := range chain.srcConns {
		res.Add(agent.LeftoverKindNvme, nqn)
	}
}

// recordGate is the proof half of a record release, supplied by the scope
// that has it. A scope with no proof passes a gate that always says no, and
// the device is still removed — the record simply waits for a pass that can
// prove it is an orphan.
type recordGate struct {
	side      func(spId, sideId uint64) bool
	cloneMeta func(spId, migrId uint64) bool
}

func (g recordGate) freeSide(spId, sideId uint64) bool {
	return g.side != nil && g.side(spId, sideId)
}

func (g recordGate) freeCloneMeta(spId, migrId uint64) bool {
	return g.cloneMeta != nil && g.cloneMeta(spId, migrId)
}

// resumeIfSuspended resumes one dm device. A suspended dm target queues bios
// with no timeout and no error path, so anything that touches it blocks in
// uninterruptible D state; nothing above it can be disabled and it cannot
// itself be removed.
func (s *DnAgentServer) resumeIfSuspended(ctx context.Context, name string) {
	dev, err := s.dm.Info(ctx, name)
	if err != nil || dev == nil || !dev.Suspended {
		return
	}
	if err := s.dm.Resume(ctx, name); err != nil {
		slog.ErrorContext(ctx, "resuming a suspended dm device failed",
			slog.String("name", name),
			slog.String("error", err.Error()))
	}
}

// freeSideOf releases the allocation record of a side device that has just
// been verified gone. The name carries the ids, so no plan is needed.
func (s *DnAgentServer) freeSideOf(
	ctx context.Context,
	name string,
	gate recordGate,
	res *agent.SweepResult,
) {
	dn, ok := common.ParseDmName(name)
	if !ok || dn.Kind != common.DmKindDnSide {
		return
	}
	if !gate.freeSide(dn.Ids[0], dn.Ids[1]) {
		return
	}
	if err := s.meta.FreeSide(ctx, dn.Ids[0], dn.Ids[1]); err != nil {
		slog.ErrorContext(ctx, "freeing the side allocation failed",
			slog.String("error", err.Error()))
		res.Fail("free side record "+name, err)
	}
}

func (s *DnAgentServer) freeCloneMetaOf(
	ctx context.Context,
	name string,
	gate recordGate,
	res *agent.SweepResult,
) {
	dn, ok := common.ParseDmName(name)
	if !ok || dn.Kind != common.DmKindDnMigrMeta {
		return
	}
	if !gate.freeCloneMeta(dn.Ids[0], dn.Ids[1]) {
		return
	}
	if err := s.meta.FreeCloneMeta(ctx, dn.Ids[0], dn.Ids[1]); err != nil {
		slog.ErrorContext(ctx, "freeing the clone-metadata slot failed",
			slog.String("error", err.Error()))
		res.Fail("free clone metadata record "+name, err)
	}
}

// ---------------------------------------------------------------------------
// The two scopes
// ---------------------------------------------------------------------------

// sweepSide is the side-level pass that replaced teardownForbidden and
// retireMigrDst. It runs in convergeSide in their place, under the node read
// lock and this side's object lock.
//
// remove = false is the read-only verdict the Check rounds and GetSideInfo
// take: same enumeration, same comparison, nothing touched.
func (s *DnAgentServer) sweepSide(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	remove bool,
) *agent.SweepResult {
	res := &agent.SweepResult{}
	actual := s.enumerateDn(ctx, plan.clusterId, plan.dnId, res)
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
	claims := s.collectClaims(plan.clusterId, plan.dnId)
	wanted := sideWanted(plan)
	chain := s.buildSideChain(ctx, plan, actual, claims, wanted, res)
	if !remove {
		reportChain(chain, res)
		res.Log(ctx, sideIdAttrs(plan)...)
		return res
	}
	s.sweepMigrChunks(ctx, st, plan)
	s.sidePreSteps(ctx, st, plan, actual, wanted)
	// A SIDE record is never freed here: this side is in the pointer list by
	// construction, so its own device is never unwanted, and no other side's
	// device is in this chain. A CLONE-METADATA record can be, under exactly
	// the proof sweepOrphanRecords uses — which is what lets a finished
	// migration release its slot in the pass that removes its wrapper,
	// instead of waiting for the next SyncupDn.
	_, _, identified := s.meta.Identity()
	s.runChain(ctx, chain, res, s.cloneMetaGate(identified))
	st.tracker.Keep(sideResKeys(plan))
	res.Log(ctx, sideIdAttrs(plan)...)
	return res
}

// cloneMetaGate is the proof a clone-metadata slot must pass before its
// record is released: no side claims the migration, and every side of that sp
// which this node may host is one whose local state the agent actually holds.
// The slot lives in the [D13] on-disk table, so releasing one early is a
// table-level mistake, not a local one. Without the second half a side
// we have not heard from yet could still own the slot, and freeing it would
// strand an in-flight migration whose hydration resumes from disk (§11.2).
func (s *DnAgentServer) cloneMetaGate(identified bool) recordGate {
	known, haveState, ok := s.knownSides()
	if !ok || !identified {
		return recordGate{}
	}
	claimed := s.claimedMigrs()
	return recordGate{
		cloneMeta: func(spId, migrId uint64) bool {
			if _, held := claimed[[2]uint64{spId, migrId}]; held {
				return false
			}
			return s.spFullyKnown(spId, known, haveState)
		},
	}
}

// sweepMigrChunks deletes the bitmap chunk files of a migration this side no
// longer plays the destination of (SH21). It is a sweep of the LOCAL STORE
// against the request, the dn twin of the cn's sweepCloneChunks: the files
// used to be dropped only by ensureMigrDst, which a side whose migr_dst_conf
// has gone never reaches — so a finished migration left its chunks on disk
// until the next startup reconcile happened to notice them.
//
// A destination role the LEVEL merely suppresses keeps its chunks: they stay
// applied-by-file, and deleting them would make the worker re-push every one
// when the level comes back down.
func (s *DnAgentServer) sweepMigrChunks(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
) {
	if st.chunkMigrId == 0 {
		return
	}
	if plan.migrDst != nil && plan.migrDst.GetMigrId() == st.chunkMigrId {
		return
	}
	s.dropChunks(ctx, st)
}

func sideIdAttrs(plan *sidePlan) []any {
	return []any{
		slog.Uint64("cluster_id", plan.clusterId),
		slog.Uint64("dn_id", plan.dnId),
		slog.Uint64("sp_id", plan.spId),
		slog.Uint64("side_id", plan.sideId),
	}
}

// buildSideChain subtracts this side's wanted set from the snapshot. The
// per-CN devices are attributed by the (sp, side) in their own names; the
// migration devices carry (sp, migr) instead and belong to no side at all, so
// they are judged by the claim rule — which is what lets a FINISHED
// migration's clone go in this pass rather than waiting for a SyncupDn.
func (s *DnAgentServer) buildSideChain(
	ctx context.Context,
	plan *sidePlan,
	actual *dnActual,
	claims *sideClaims,
	wanted *dnWanted,
	res *agent.SweepResult,
) *dnChain {
	chain := &dnChain{}
	ofSide := func(dn common.DmName) bool {
		return dn.Ids[0] == plan.spId && dn.Ids[1] == plan.sideId
	}
	unwantedOfSide := func(kind common.DmKind) []string {
		var out []string
		for _, name := range actual.dmsOfKind(kind, ofSide) {
			if _, keep := wanted.dms[name]; !keep {
				out = append(out, name)
			}
		}
		return out
	}
	chain.linears = unwantedOfSide(common.DmKindDnLinear)
	chain.errors = unwantedOfSide(common.DmKindDnError)
	chain.sideDevs = unwantedOfSide(common.DmKindDnSide)

	// Migration devices of this sp that no stored side claims.
	ofSp := func(dn common.DmName) bool { return dn.Ids[0] == plan.spId }
	unclaimedMigr := func(
		kind common.DmKind,
		claimed map[[2]uint64]struct{},
	) []string {
		var out []string
		for _, name := range actual.dmsOfKind(kind, ofSp) {
			if _, keep := wanted.dms[name]; keep {
				continue
			}
			dn := actual.ours[name]
			if _, held := claimed[[2]uint64{dn.Ids[0], dn.Ids[1]}]; held {
				continue
			}
			out = append(out, name)
		}
		return out
	}
	chain.clones = unclaimedMigr(common.DmKindDnMigrFinal, claims.migrDst)
	chain.metas = unclaimedMigr(common.DmKindDnMigrMeta, claims.migrDst)
	chain.migrSrcs = unclaimedMigr(common.DmKindDnMigrSrc, claims.migrSrcDm)

	s.collectExports(ctx, plan.clusterId, plan.dnId, plan.spId, plan.sideId,
		actual, claims, wanted, chain, &portLinks{}, res)
	s.collectSrcConns(ctx, plan.clusterId, plan.dnId, plan.spId,
		actual, claims, wanted, chain, res)
	return chain
}

// exportOwner is how a :2: export is attributed. Its NQN carries
// (cluster, sp, leg, cn) and names neither a side nor a dn — both sides of a
// migrating leg export this same NQN ([D1]) — so the evidence has to come
// from somewhere else entirely.
type exportOwner int

const (
	// exportForeign: the export demonstrably belongs to somebody else. On a
	// node running several dn agents that is the common case, and taking one
	// of those would pull a healthy leg's path out from under its owner.
	exportForeign exportOwner = iota
	// exportOurs: its namespace names one of OUR per-CN linears, so the side
	// it belongs to is known exactly — by name, not by anything remembered.
	// This is what makes the rule survive a lost --local-store: a side still
	// in the DN's pointer list keeps its export although no local state
	// claims it, because such a side must be REBUILT from its record (DN8).
	exportOurs
	// exportOrphan: no namespace to read, and the subsystem is linked to our
	// port or to no port at all. It exports nothing and holds nothing open —
	// this is what our own half-finished RemoveSubsystem leaves behind, and
	// leaving it would leak it for ever.
	exportOrphan
)

// portLinks is the lazily built "which port is this subsystem linked to" map.
// It is only ever needed for a subsystem with no namespace to attribute it
// by, which in a healthy steady state never happens — so the listing it costs
// is not paid on an ordinary pass.
type portLinks struct {
	done bool
	err  error
	// byNqn maps a subsystem to the ports it is linked to.
	byNqn map[string][]int
}

func (s *DnAgentServer) portLinksOf(
	ctx context.Context,
	links *portLinks,
) (map[string][]int, error) {
	if links.done {
		return links.byNqn, links.err
	}
	links.done = true
	links.byNqn = make(map[string][]int)
	ports, err := s.nvmet.ListPorts(ctx)
	if err != nil {
		links.err = err
		return nil, err
	}
	for _, portId := range ports {
		linked, err := s.nvmet.ListPortSubsystems(ctx, portId)
		if err != nil {
			links.err = err
			return nil, err
		}
		for _, nqn := range linked {
			links.byNqn[nqn] = append(links.byNqn[nqn], portId)
		}
	}
	return links.byNqn, nil
}

// classifyExport attributes one :2: export. It reads the export's namespace
// first — the cheap and usually conclusive evidence — and falls back to the
// port links only when there is no namespace to read.
func (s *DnAgentServer) classifyExport(
	ctx context.Context,
	clusterId uint64,
	dnId uint64,
	nqn string,
	links *portLinks,
	res *agent.SweepResult,
) (owner exportOwner, spId uint64, sideId uint64) {
	nsids, found, err := s.nvmet.ListNamespaces(ctx, nqn)
	if err != nil {
		res.Fail("nvmet namespaces of "+nqn, err)
		return exportForeign, 0, 0
	}
	if !found {
		// Removed between the listing and this read.
		return exportForeign, 0, 0
	}
	sawNs := false
	for _, nsid := range nsids {
		sawNs = true
		path, present, err := s.nvmet.NsDevicePath(ctx, nqn, nsid)
		if err != nil {
			res.Fail(fmt.Sprintf("nvmet %s ns %d device_path", nqn, nsid), err)
			return exportForeign, 0, 0
		}
		if !present {
			continue
		}
		dn, parsed := common.ParseDmName(strings.TrimSpace(
			strings.TrimPrefix(strings.TrimSpace(path), "/dev/mapper/")))
		if !parsed || dn.Kind != common.DmKindDnLinear ||
			dn.ClusterId != clusterId {
			continue
		}
		if dn.NodeId != dnId {
			// A per-CN linear of a SIBLING dn agent on this kernel. Its
			// export is that agent's, and nothing here may touch it.
			return exportForeign, 0, 0
		}
		return exportOurs, dn.Ids[0], dn.Ids[1]
	}
	if sawNs {
		// It has namespaces, but none of them names a device we can read an
		// owner off. Somebody else's, or a shape this build did not write.
		return exportForeign, 0, 0
	}
	byNqn, err := s.portLinksOf(ctx, links)
	if err != nil {
		res.Fail("nvmet port links", err)
		return exportForeign, 0, 0
	}
	for _, portId := range byNqn[nqn] {
		if portId != s.port.PortId {
			// Linked to a sibling agent's port: its half-built export.
			return exportForeign, 0, 0
		}
	}
	return exportOrphan, 0, 0
}

// collectExports adds the unwanted nvmet subsystems of one sp to a chain.
func (s *DnAgentServer) collectExports(
	ctx context.Context,
	clusterId uint64,
	dnId uint64,
	spId uint64,
	sideId uint64,
	actual *dnActual,
	claims *sideClaims,
	wanted *dnWanted,
	chain *dnChain,
	links *portLinks,
	res *agent.SweepResult,
) {
	for _, nqn := range actual.nvmetSubsys {
		parts, ok := common.ParseNqn(nqn)
		if !ok {
			// Either a cn agent's host-facing subsystem on a co-hosted node,
			// or a stranger's. Never ours.
			continue
		}
		switch parts.Kind {
		case common.NqnKindSideToCn:
			if parts.Ids[0] != clusterId || parts.Ids[1] != spId {
				continue
			}
			if _, keep := wanted.nvmetSs[nqn]; keep {
				continue
			}
			// The side-level scope judges its OWN side's exports and nothing
			// else: another side's export is its own SyncupSide's business,
			// and this pass holds only the node READ lock.
			owner, ownerSp, ownerSide := s.classifyExport(
				ctx, clusterId, dnId, nqn, links, res)
			switch owner {
			case exportForeign:
				continue
			case exportOurs:
				if ownerSp != spId || ownerSide != sideId {
					continue
				}
			case exportOrphan:
				// No namespace names it, so it belongs to no side this pass
				// can judge — but a live side may still be building it, and
				// its request is the one thing that says so.
				if _, held := claims.exports[nqn]; held {
					continue
				}
			}
		case common.NqnKindMigrSrc:
			// (cluster, dn, sp, migr) — the dn id is the exporting one, so
			// this really is ours, and the NQN names the migration directly.
			if parts.Ids[0] != clusterId || parts.Ids[1] != dnId ||
				parts.Ids[2] != spId {
				continue
			}
			if _, keep := wanted.nvmetSs[nqn]; keep {
				continue
			}
			if _, held := claims.migrSrcNqn[[2]uint64{
				parts.Ids[2], parts.Ids[3]}]; held {
				continue
			}
		default:
			continue
		}
		chain.exports = append(chain.exports, nqn)
	}
	sort.Strings(chain.exports)
}

// collectSrcConns adds the unwanted :3: host connections of one sp. The dn id
// inside a MigrSrcNqn is the SOURCE dn's, and the nvme host namespace is per
// KERNEL, so the host NQN the controller was opened with is what says which
// agent on this node holds it.
func (s *DnAgentServer) collectSrcConns(
	ctx context.Context,
	clusterId uint64,
	dnId uint64,
	spId uint64,
	actual *dnActual,
	claims *sideClaims,
	wanted *dnWanted,
	chain *dnChain,
	res *agent.SweepResult,
) {
	hostNqn := s.nf.DnHostNqn(clusterId, dnId)
	for _, state := range actual.hostSubsys {
		parts, ok := common.ParseNqn(state.Nqn)
		if !ok || parts.Kind != common.NqnKindMigrSrc ||
			parts.Ids[0] != clusterId || parts.Ids[2] != spId {
			continue
		}
		if _, keep := wanted.hostNqns[state.Nqn]; keep {
			continue
		}
		if _, held := claims.migrDst[[2]uint64{
			parts.Ids[2], parts.Ids[3]}]; held {
			continue
		}
		ours, err := s.host.HeldWithHostNqn(ctx, state, hostNqn)
		if err != nil {
			res.Fail("nvme host nqn of "+state.Nqn, err)
			continue
		}
		if !ours {
			continue
		}
		chain.srcConns = append(chain.srcConns, state.Nqn)
	}
	sort.Strings(chain.srcConns)
}

// sidePreSteps are the transitions derived from the live tables that make the
// layers below able to remove anything.
func (s *DnAgentServer) sidePreSteps(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	actual *dnActual,
	wanted *dnWanted,
) {
	// The destination role this side no longer plays keeps no retry loop.
	if !plan.wantMigr {
		s.stopMigrRetry(st)
	}
	if plan.migrSrc == nil {
		// The fence only ever applies while a source role is live, so a side
		// that is not one has no window — clearing unconditionally needs no
		// memory of whether one was ever started.
		s.clearFence(st)
		// A linear this side left suspended inside a window that is now over
		// must be resumed, or it stays suspended until something else happens
		// to converge it — and a suspended dm target queues bios with no
		// timeout ([D12]). The set comes from the enumeration, not from a
		// remembered cn list.
		s.unfenceLinears(ctx, plan, actual)
	}
	// P-DN1, repoint: a wanted per-CN linear whose LIVE table still maps a
	// device this pass is about to remove — a finished migration's dm-clone —
	// is reloaded onto the backing the plan wants first. Without it the
	// clone's removal fails EBUSY under the linear and the whole chain waits
	// a round for the build phase to repoint it.
	//
	// A fenced (suspended) wanted linear is left alone: that suspension is
	// deliberate, and ending it is the fence's own job.
	if !plan.wantDm {
		return
	}
	for _, cnId := range plan.cnIds {
		name := plan.linearName(cnId)
		dev, err := s.dm.Info(ctx, name)
		if err != nil || dev == nil || dev.Suspended {
			continue
		}
		if !s.dmMapsUnwanted(ctx, name, actual, wanted) {
			continue
		}
		if err := s.ensureDmLinear(ctx, name, plan.sectors,
			plan.linearBacking(cnId, false)); err != nil {
			slog.ErrorContext(ctx, "repointing a per-cn dm-linear failed",
				slog.String("name", name),
				slog.String("error", err.Error()))
		}
	}
}

// dmMapsUnwanted reports whether a device's live table maps one of ours that
// the desired state does not want.
func (s *DnAgentServer) dmMapsUnwanted(
	ctx context.Context,
	name string,
	actual *dnActual,
	wanted *dnWanted,
) bool {
	targets, err := s.dm.Table(ctx, name)
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
	_, ours := actual.ours[backing]
	return ours
}

// sweepDn is the node-level pass: everything of a side whose pointer has left
// this DN's list, the migration objects no stored side claims, the exports no
// stored side claims, and the allocation records the authoritative lists
// prove orphaned.
//
// It runs under the node write lock, so neither the DN set nor the side set
// can move under it. A side that IS in the pointer list is never touched here
// even when its side file is absent: after a lost --local-store the side must
// be REBUILT from its record, and sweeping it would free the extents and send
// the next SyncupSide through the §9.4 provisioning protocol again, zeroing
// live data.
func (s *DnAgentServer) sweepDn(
	ctx context.Context,
	st *dnState,
	remove bool,
) *agent.SweepResult {
	res := &agent.SweepResult{}
	clusterId := st.req.GetClusterId()
	dnId := st.req.GetDnId()
	actual := s.enumerateDn(ctx, clusterId, dnId, res)
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
	claims := s.collectClaims(clusterId, dnId)

	// knownSides is the authoritative proof: the union of every synced DN's
	// side_pointer_list and every side this agent holds state for. It is the
	// same set sweepOrphanRecords frees against, so the device sweep and the
	// record sweep can never disagree about which sides may still be hosted
	// here. With no DN synced at all nothing is authoritative and nothing is
	// swept.
	known, _, ok := s.knownSides()
	if !ok {
		res.Log(ctx,
			slog.Uint64("cluster_id", clusterId),
			slog.Uint64("dn_id", dnId))
		return res
	}

	chain := &dnChain{}
	for name, dn := range actual.ours {
		switch dn.Kind {
		case common.DmKindDnError, common.DmKindDnLinear, common.DmKindDnSide:
			if _, live := known[[2]uint64{dn.Ids[0], dn.Ids[1]}]; live {
				continue
			}
			switch dn.Kind {
			case common.DmKindDnError:
				chain.errors = append(chain.errors, name)
			case common.DmKindDnLinear:
				chain.linears = append(chain.linears, name)
			default:
				chain.sideDevs = append(chain.sideDevs, name)
			}
		case common.DmKindDnMigrFinal:
			if _, held := claims.migrDst[[2]uint64{
				dn.Ids[0], dn.Ids[1]}]; !held {
				chain.clones = append(chain.clones, name)
			}
		case common.DmKindDnMigrMeta:
			if _, held := claims.migrDst[[2]uint64{
				dn.Ids[0], dn.Ids[1]}]; !held {
				chain.metas = append(chain.metas, name)
			}
		case common.DmKindDnMigrSrc:
			if _, held := claims.migrSrcDm[[2]uint64{
				dn.Ids[0], dn.Ids[1]}]; !held {
				chain.migrSrcs = append(chain.migrSrcs, name)
			}
		}
	}
	sort.Strings(chain.errors)
	sort.Strings(chain.linears)
	sort.Strings(chain.sideDevs)
	sort.Strings(chain.clones)
	sort.Strings(chain.metas)
	sort.Strings(chain.migrSrcs)

	// hosted is every sp this node holds evidence of: a dm device of ours, or
	// a pointer in an authoritative list. A :2: NQN carries no dn id — both
	// sides of a migrating leg export the same one ([D1]) — so without this
	// an agent sharing a VM with others would read the namespaces of every
	// one of THEIR exports on every pass, which on a lab node running
	// dozens of dn agents is the whole configfs tree per round.
	//
	// It loses nothing: an export this agent could have left behind belongs
	// to a side whose devices are still there (the layers remove the export
	// FIRST and stop the descent when it will not go), so its sp is in
	// actual.ours by construction.
	links := &portLinks{}
	hosted := make(map[uint64]struct{})
	for _, dn := range actual.ours {
		hosted[dn.Ids[0]] = struct{}{}
	}
	for key := range known {
		hosted[key[0]] = struct{}{}
	}
	for _, nqn := range actual.nvmetSubsys {
		parts, ok := common.ParseNqn(nqn)
		if !ok {
			continue
		}
		switch parts.Kind {
		case common.NqnKindSideToCn:
			if parts.Ids[0] != clusterId {
				continue
			}
			if _, ours := hosted[parts.Ids[1]]; !ours {
				continue
			}
			// A side whose state this agent holds and whose plan still
			// exports this NQN settles it without reading anything: that
			// request IS the desired state.
			if _, held := claims.exports[nqn]; held {
				continue
			}
			owner, ownerSp, ownerSide := s.classifyExport(
				ctx, clusterId, dnId, nqn, links, res)
			switch owner {
			case exportForeign:
				continue
			case exportOurs:
				if _, live := known[[2]uint64{ownerSp, ownerSide}]; live {
					continue
				}
			case exportOrphan:
				// Ours, half-removed, exporting nothing: it goes.
			}
		case common.NqnKindMigrSrc:
			if parts.Ids[0] != clusterId || parts.Ids[1] != dnId {
				continue
			}
			if _, held := claims.migrSrcNqn[[2]uint64{
				parts.Ids[2], parts.Ids[3]}]; held {
				continue
			}
		default:
			continue
		}
		chain.exports = append(chain.exports, nqn)
	}
	sort.Strings(chain.exports)

	hostNqn := s.nf.DnHostNqn(clusterId, dnId)
	for _, state := range actual.hostSubsys {
		parts, ok := common.ParseNqn(state.Nqn)
		if !ok || parts.Kind != common.NqnKindMigrSrc ||
			parts.Ids[0] != clusterId {
			continue
		}
		if _, held := claims.migrDst[[2]uint64{
			parts.Ids[2], parts.Ids[3]}]; held {
			continue
		}
		// The dn id inside a MigrSrcNqn is the SOURCE dn's, and the nvme host
		// namespace is per KERNEL: on a node running several dn agents this
		// listing shows every one of their connections. The host NQN the
		// controller was opened with is what says whose it is.
		ours, err := s.host.HeldWithHostNqn(ctx, state, hostNqn)
		if err != nil {
			res.Fail("nvme host nqn of "+state.Nqn, err)
			continue
		}
		if !ours {
			continue
		}
		chain.srcConns = append(chain.srcConns, state.Nqn)
	}
	sort.Strings(chain.srcConns)

	if !remove {
		reportChain(chain, res)
		s.sweepOrphanRecords(ctx, res, false)
		res.Log(ctx,
			slog.Uint64("cluster_id", clusterId),
			slog.Uint64("dn_id", dnId))
		return res
	}
	// A side named only by a record still has its §9.4 zeroing child holding
	// the side device open, and `dmsetup remove` on a device with an open fd
	// fails EBUSY.
	for _, name := range chain.sideDevs {
		if dn, ok := common.ParseDmName(name); ok {
			s.stopZeroingOf(clusterId, dnId, dn.Ids[0], dn.Ids[1])
		}
	}
	// The side records this chain can free are exactly the ones `known`
	// already proved orphaned: the chain holds a d4 only when its (sp, side)
	// is absent from every authoritative list and from every local state.
	// The identity check is the same one sweepOrphanRecords makes — an
	// unformatted, unreadable or foreign disk holds no table this node may
	// write.
	_, _, identified := s.meta.Identity()
	s.runChain(ctx, chain, res, recordGate{
		side:      func(uint64, uint64) bool { return identified },
		cloneMeta: s.cloneMetaGate(identified).cloneMeta,
	})
	s.sweepOrphanRecords(ctx, res, true)
	res.Log(ctx,
		slog.Uint64("cluster_id", clusterId),
		slog.Uint64("dn_id", dnId))
	return res
}

// ---------------------------------------------------------------------------
// The read-only verdicts
// ---------------------------------------------------------------------------

// dnVerdict is the node-level comparison the CheckDn rounds and GetDnInfo
// take. A DN whose stored extent size is unusable converges nothing and
// sweeps nothing (§7), so it has no verdict either: naming leftovers for a
// node this agent deliberately did not touch would report objects it is not
// allowed to remove.
func (s *DnAgentServer) dnVerdict(
	ctx context.Context,
	st *dnState,
) *agent.SweepResult {
	if agent.ValidateExtentSize(st.req.GetExtentSize()) != nil {
		return &agent.SweepResult{}
	}
	return s.sweepDn(ctx, st, false)
}

// sideVerdict is the side-level comparison the CheckSide rounds and
// GetSideInfo take. The side's extent size comes from its DN, and a side
// whose DN is unknown or unusable has nothing to compare against.
func (s *DnAgentServer) sideVerdict(
	ctx context.Context,
	st *sideState,
) *agent.SweepResult {
	dn := s.getDn(dnKey(st.req.GetClusterId(), st.req.GetDnId()))
	if dn == nil ||
		agent.ValidateExtentSize(dn.req.GetExtentSize()) != nil {
		return &agent.SweepResult{}
	}
	plan := newSidePlan(s.nf, st.req, dn.req.GetExtentSize())
	return s.sweepSide(ctx, st, plan, false)
}
