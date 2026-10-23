package cnagent

import (
	"fmt"
	"sort"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ResTracker keys. The node-level resource of the CN (CN5) first, then the
// per-cntlr ones — one format per CntlrInfo map, keyed by the map's own id so
// the SH14 epochs follow a resource across converges.
const (
	resKeyPort = "port"

	resKeySubsysFmt    = "subsystem/%016x"
	resKeyNamespaceFmt = "namespace/%016x"
	resKeyNsDevFmt     = "ns_dev/%016x"
	resKeyRaid0Fmt     = "raid0/%016x"
	resKeyTdErrorFmt   = "td_error/%016x"
	resKeyThinFmt      = "thin/%016x/%016x"
	resKeyPoolFmt      = "pool/%016x"
	resKeyPoolMetaFmt  = "pool_meta/%016x"
	resKeyPoolDataFmt  = "pool_data/%016x"
	resKeyGrpFmt       = "grp/%016x"
	resKeyLegFmt       = "leg/%016x"
	resKeyPathFmt      = "path/%016x"
	resKeyXferDmFmt    = "xfer_dm/%016x"
	resKeyXferSsFmt    = "xfer_ss/%016x"
	resKeyXferNsFmt    = "xfer_ns/%016x"
	resKeyCloneTgtFmt  = "clone_target/%016x"
	resKeyCloneDmFmt   = "clone_dm/%016x"
)

// detailsSpLevel is what a resource suppressed by the sp_level reports (CN19),
// shared through common with the worker's settle (dnv-worker.md HL2);
// detailsParked is the expected state of an effectively suspended ns-dev
// (CN28): **live**, its table a dm-linear over the td's dm-error
// (architecture.md, Namespace suspend semantics).
// detailsSuspended is what the generic single-device probes report for a device
// they find dm-suspended on the table they want. A converge and a probe of one
// cntlr take the same lock, so CN14's quiesce bracket is never observable from
// outside; what makes the state reachable is an interrupted pass, an agent
// killed inside that bracket, or a dmsetup command on the device that failed:
// a resume, or a suspend killed after it took effect. A reload whose load
// fails leaves its device suspended too (Dm.Reload fails closed), but on its
// old table, which these probes report as a table mismatch before they read
// the suspend bit — unless the reload was only clearing a read-only flag. No
// ns-dev is ever expected in it. None of the three details is a fault.
const (
	detailsSpLevel   = common.ResDetailsSpLevel
	detailsParked    = "parked"
	detailsSuspended = "suspended"
	// detailsProvisioning is what a provisioning-deferred resource reports:
	// the sides
	// under it have provisioned = false, so nothing is exported yet and
	// nothing is wrong (the CN9 provisioning gate). It carries no progress counter on
	// purpose — the dn's zeroing progress advances, and a details string that
	// changed every round would defeat the proto.Equal suppression of the CN24
	// check stream.
	detailsProvisioning = "provisioning"
	// detailsNsDevNotDesired is an ns-dev whose live table is not the CN16
	// backing its plan wants. The probe reports it, and so does the converge
	// for an ns-dev it held off its td's raid0 (ensureNsDev), so the two
	// channels do not flip the row against each other.
	detailsNsDevNotDesired = "table is not the desired namespace backing"
	// detailsNoController is a path row whose side's address no controller
	// of the leg's subsystem holds (CN28, legPathRows).
	detailsNoController = "no controller"
)

func resKeyOf(format string, id uint64) string {
	return fmt.Sprintf(format, id)
}

// thinResKey is the two-id key of one td's thin volume in one slice.
func thinResKey(tdId, sliceId uint64) string {
	return fmt.Sprintf(resKeyThinFmt, tdId, sliceId)
}

// equalFoldHex compares two hex identity strings the way configfs hands them
// back: nvmet normalizes both the case *and* the shape of
// device_uuid/device_nguid, always reading them back dash-separated (SH17).
// agent.SameNsId is the one definition of that comparison, shared with the dn
// role and with the converge side of this one (nvmet.go EnsureNamespace).
func equalFoldHex(got, want string) bool {
	return agent.SameNsId(got, want)
}

// ---------------------------------------------------------------------------
// The plan: the desired state of one cntlr, a pure function of its
// SyncupCntlrRequest. The same plan drives the converge pass (CN8 to CN20)
// and the read-only probe (CN28), which is what keeps the two from drifting.
// ---------------------------------------------------------------------------

type cntlrPlan struct {
	nf *common.NameFmt

	clusterId uint64
	cnId      uint64
	spId      uint64
	cntlrId   uint64

	req   *pb.SyncupCntlrRequest
	cntlr *pb.Cntlr
	level pb.SpLevel

	// primary is the *effective* role (CN9): a disabled cntlr converges the
	// standby shape, which already moves every namespace to inaccessible.
	primary  bool
	disabled bool

	blockSize         uint64
	stripeSize        uint64
	lowWaterPct       uint32
	raid1             bool
	bitmapChunkBlocks uint64

	sliceCnt  uint64
	slices    []*slicePlan
	sliceById map[uint64]*slicePlan
	grps      []*grpPlan
	grpById   map[uint64]*grpPlan
	legs      []*legPlan
	legById   map[uint64]*legPlan

	tds       []*tdPlan
	tdById    map[uint64]*tdPlan
	tdByDevId map[uint32]*tdPlan

	subsystems []*ssPlan
	namespaces []*nsPlan

	clones    []*clonePlan
	cloneById map[uint64]*clonePlan
	cloneByTd map[uint64]*clonePlan
	xfers     []*xferPlan
	xferById  map[uint64]*xferPlan

	// sp_level gates (CN19). Levels are desired state: raising one shrinks
	// the sweep's wanted set, so the layers it forbids are swept away;
	// lowering it rebuilds them in the build phase.
	wantAny   bool // level < SP_LEVEL_DISABLE
	wantLeg   bool // + level < SP_LEVEL_NO_SIDE
	wantGrp   bool // + primary, level < SP_LEVEL_NO_REDUND
	wantPool  bool // + primary, level < SP_LEVEL_NO_THINPOOL
	wantClone bool // + primary, level < SP_LEVEL_NO_CLONE
	readOnly  bool // primary, level >= SP_LEVEL_READONLY ([D11])

	// levelX is the level gate alone, without the role. The difference
	// matters only in the report: a resource a *standby* simply does not
	// have is left out of CntlrInfo, while one the level suppresses is
	// reported RES_STATUS_MISSING with details = "sp_level" (CN19).
	levelLeg   bool
	levelGrp   bool
	levelPool  bool
	levelClone bool

	// anyDeferred is true when any group or slice of this cntlr is
	// provisioning-deferred ([D15]): the pass-level summary of how far the
	// effective desired state falls short of the raw one. The td rule
	// deliberately does NOT use it — one deferred group of a many-group slice
	// leaves the pool serving at its effective size (a GrowSlice whose new
	// sides are still zeroing), and only a fully deferred *slice* defers the
	// tds above it (anySliceDeferred).
	anyDeferred bool

	// cloneHold is what this pass's sweep tells the build phase that
	// follows it (CN18, CN21): the raid0s a dm-clone the plan does not want
	// may still write into after the sweep — on a pass that removes,
	// because L3 did not run (the descent stopped above it) or did not
	// verify it gone; on one an unanswered listing stopped, because the dm
	// listing named it — or every raid0 of the cntlr, when the dm listing
	// did not answer or such a dm-clone's destination could not be read
	// (lingeringCloneHold). A dm-clone whose hydration is on goes on copying
	// into its destination raid0 until L3 removes it. sweepCntlr sets it on
	// a converge and nothing else does; the plan is built afresh for every
	// pass, so it never outlives the pass that learned it. It, the reading
	// and the walks after it are the only fields here that are not a pure
	// function of the request.
	cloneHold cloneHold

	// rd is the reading a cntlr's read took (CN28, readCntlr): the shared
	// helpers read dm facts and numbers from it (dmInfo, dmTable, dmDevNo),
	// and the verdict its listings. It is nil in a converge and in every other
	// plan builder, and the helpers then read each device on its own.
	rd *cnReading
	// walk is the pass's NVMe walk (CN10), shared by every lookup of the pass
	// and by its sweep's or its verdict's list of connections: the reading's
	// in a read, and in a converge the one convergeCntlr takes right before
	// its sweep. No other plan builder looks a subsystem up, and their plans
	// leave it nil: a lookup with no walk fails (lookupSubsys).
	walk *passWalk
	// mdWalks is the pass's md walk, shared by every group row and every
	// concat row of the pass, so a group's row and its concat rows read one
	// array (mdDevNo): in a converge taken at the first group that needs it
	// (build), in a read the walk view of the reading's md walk
	// (cnReading.mdWalks). No other plan builder reads an md array, and their
	// plans leave it nil.
	mdWalks *mdWalkOnce
}

// slicePlan is one slice of the SP: the two pool concats and the thin-pool
// built on the slice's groups (CN13).
type slicePlan struct {
	sliceId  uint64
	sliceIdx uint32
	slice    *pb.Slice

	metaGrps []*grpPlan
	dataGrps []*grpPlan

	// effMetaGrps / effDataGrps are the *effective* group lists ([D15]): each
	// list truncated at its first deferred group (effectiveGrps), never
	// filtered. They are what the pool concats are built from and probed
	// against, and what metaSectors / dataSectors / dataBlocks are summed over
	// — so a concat that has not grown into a still-provisioning group yet is
	// OK rather than a mismatch, it never re-bases a target that is already
	// live, and the grow completes on the pass where the group clears.
	effMetaGrps []*grpPlan
	effDataGrps []*grpPlan
	// deferred means one of the slice's two sides has no effective group left
	// — the initial CreateStoragePool shape, where every leg is provisioning.
	// Nothing is built for it: no concat, no thin-pool, no thin volume. Group
	// deferral alone would not do, because an empty concat is an error
	// (dmutil.go's "concat has no segments"), and [D15] promises PROVISIONING
	// throughout with no err_epoch.
	deferred bool

	poolMetaName  string
	poolDataName  string
	poolFinalName string

	// metaSectors / dataSectors are the concats' sizes; dataBlocks is what
	// the low_water_mark is a percentage of (CN13).
	metaSectors uint64
	dataSectors uint64
	dataBlocks  uint64
}

// grpPlan is one group: a RedundNone dm-linear over its single leg's data
// region, or a RedundMdRaid1 array over its member leg wrappers (CN12). It is
// a slice's group, or a clone's clone meta group (architecture.md,
// Terminology and object model), which the plan builds the same way.
type grpPlan struct {
	grpId uint64
	grp   *pb.Group
	// slice, isMeta and grpIdx place a slice's group in its slice's lists;
	// clone is the clone whose clone meta group this is. Exactly one of
	// slice and clone is set.
	slice  *slicePlan
	isMeta bool
	grpIdx uint32
	clone  *pb.Clone
	raid1  bool
	legs   []*legPlan // leg_list, ascending leg_idx — the md members
	spares []*legPlan // spare_leg_list; connected and probed, never members

	// deferred is the [D15] group gate: some leg of leg_list is provisioning, so
	// the group builds no md array and no CnGrpName, and is left out of the
	// pool concats and of the pool sizing. A provisioning *spare* never defers
	// a group — spares are not members (architecture.md, Spare legs).
	deferred bool

	// dmName is set for RedundNone only; mdDevName/mdArrayName for
	// RedundMdRaid1 only. devPath is the group device either way, the path
	// the mdadm commands name an md group's array by. A pool concat maps
	// the group by its number, which buildConcat reads: an md array's from
	// /sys/block (mdDevNo), never through the udev link at devPath.
	dmName      string
	mdDevName   string
	mdArrayName string
	devPath     string
	resName     string

	metaBlocks        uint64
	dataBlocks        uint64
	dataSectors       uint64
	dataOffsetSectors uint64
	bitmapChunkKiB    uint64
	dataOffsetKiB     uint64
}

// legNames are the dm names of the group's leg_list wrappers — the key
// Md.Detail finds the group's array by. Spares are left out: a spare is never
// a member (architecture.md, Spare legs), and a leg switched out into the
// spare list is an extra that reconcileMembers removes once CN12's in-sync
// guard lets it (inSyncGuard), found through the leg_list member that stays.
func (gp *grpPlan) legNames() []string {
	out := make([]string, 0, len(gp.legs))
	for _, lp := range gp.legs {
		out = append(out, lp.name)
	}
	return out
}

// hasLateMember reports whether a group's leg_list holds a member this pass
// may not use — not available (cnagent.md CN12: no path both live and
// optimized, or its connect, its multipath namespace or its wrapper failed,
// or a controller's address read did not answer, CN10). In an md group
// that leaves the group unassembled, assembled degraded without it, with its
// --add skipped by reconcileMembers, or running with md still holding it —
// its side died under the array (AR8's case), its path is otherwise no
// longer both live and optimized, or its converge failed this pass (CN10);
// reconcileMembers keeps a held leg_list member (CN12). In a RedundNone
// group it leaves the group's dm-linear unbuilt, and with it a pool not
// built yet and every layer above that pool, while a pool that serves when a
// grow adds the group keeps serving at its old size; or, where the dm-linear
// is there already, it keeps it as it is (ensureLinearGroup): a side that has
// not flipped to this CN yet exports dm-error to it (a non-optimized path).
// Only availability decides: a held member md has failed on a leg that is
// available again is not late — the read's verdict names it once its
// leg can take it, and the converge of the syncup the worker re-sends puts it
// back (putBackNames, reconcileMembers; cnagent.md CN12) — unless CN12's
// in-sync guard holds over its array (inSyncGuard): the read's row then reads
// ERROR, its verdict names nothing for the group and the converge puts
// nothing back, so when that member is the only one that stays nothing brings
// it back in sync and the guard does not let go by itself (cnagent.md CN12,
// Known limits). Spares are not
// members (architecture.md, Spare legs), so a spare is never late. A
// provisioning leg defers its whole group ([D15]), so the build never asks
// about one; it is excluded all the same rather than trusting the caller.
func (gp *grpPlan) hasLateMember(available map[uint64]bool) bool {
	for _, lp := range gp.legs {
		if !lp.provisioning && !available[lp.legId] {
			return true
		}
	}
	return false
}

// legPlan is one leg: its side connections and the cn-local dm-linear wrapper
// over the single nvme multipath namespace they share ([D1], CN10).
type legPlan struct {
	legId uint64
	leg   *pb.Leg
	grp   *grpPlan
	spare bool
	sides []*pb.Side
	// provisioning is the [D15] leg gate: every side of side_list has
	// provisioned = false, so the DN exports nothing — no connect is issued
	// and no wrapper is built. A mid-migration leg whose src side is
	// provisioned and whose dst side is not keeps serving and is NOT
	// provisioning.
	provisioning bool
	nqn          string
	name         string
	path         string
	// sectors is the whole leg (meta + data regions); healthOffset is the
	// last 4 KiB of the meta region, the health block (architecture.md, Group
	// on-leg layout: meta region, data region, health block).
	sectors      uint64
	healthOffset uint64
}

// tdPlan is one thin device: its per-slice thin volumes, its raid0 and its
// permanent dm-error reload target (CN14, CN15).
type tdPlan struct {
	tdId      uint64
	td        *pb.ThinDevice
	raid0Name string
	errorName string
	// deferred means some slice of this cntlr is provisioning-deferred: the
	// raid0 stripes across every slice's thin volume (CN15), so one deferred
	// slice defers the whole td — and, through CN16, its namespaces.
	deferred bool
	sectors  uint64
	// thinSectors is the virtual size of one slice's thin volume.
	thinSectors uint64
}

// ssPlan / nsPlan are the host-facing nvmet objects and the namespace backing
// devices of CN16.
type ssPlan struct {
	nqn        string
	ss         *pb.Subsystem
	ssId       uint64
	namespaces []*nsPlan
}

type nsPlan struct {
	ss    *ssPlan
	ns    *pb.Namespace
	nsId  uint64
	nsIdx int
	td    *tdPlan

	devName string
	sectors uint64

	// suspended is the *effective* suspend (architecture.md, Namespace
	// suspend semantics; CN16) — which is a **park**,
	// not a dm suspension: it makes backingName the td's dm-error (rule 1) and
	// anaGrpId inaccessible, and the device stays live ([D12]). backingName is
	// the dm device the ns-dev's table points at, flakey the [D11] read-only
	// wrapper over it, anaGrpId the fixed [D4] group.
	suspended   bool
	backingName string
	flakey      bool
	anaGrpId    int
	// clone is the clone whose dm-clone rule 5 made backingName, and nil
	// under every other rule. CN16 serves through that dm-clone only while
	// its live status shows hydration on and its metadata read-write
	// (nsDevNow).
	clone *clonePlan
	// deferred mirrors td.deferred, or, below SP_LEVEL_NO_CLONE (levelClone),
	// the deferral of a clone whose destination is the td: the fourth
	// conjunct of the CN16 ANA rule ([D15]). The ns-dev and the nvmet
	// namespace still exist — over the td's permanent dm-error — but nothing
	// under them can serve, so the namespace stays inaccessible and both rows
	// report PROVISIONING.
	deferred bool
}

// clonePlan is one dm-clone stack: the source connection and the dm-clone
// itself, whose metadata device is the clone's clone meta group's device
// (CN18).
type clonePlan struct {
	cloneId   uint64
	clone     *pb.Clone
	dstTd     *tdPlan
	finalName string
	// grp is the clone's clone meta group (buildCloneGrps), nil for a clone
	// record that carries none, for which nothing is built (CN18).
	grp *grpPlan

	sectors       uint64
	regionSectors uint64
	regionCnt     uint64

	// deferred means the destination td's backing chain is
	// provisioning-deferred, or the clone meta group is ([D15]): the
	// destination's raid0 or the group's device (its md array, or its
	// CnGrpName without redundancy) does not exist, so the clone builds no
	// dm-clone and connects to no source, and below SP_LEVEL_NO_CLONE its
	// rows read provisioning (reportCloneDeferred).
	deferred bool
}

// xferPlan is one transfer export (CN17).
type xferPlan struct {
	xferId    uint64
	xfer      *pb.Transfer
	finalName string
	nqn       string
	// ori is the origin namespace; nil when ori_nqn/ori_ns_idx resolve to
	// nothing in this request, which is an error of the xfer's own
	// resources and never of the pass (CN29).
	ori *nsPlan
	// deferred means the origin's td is provisioning-deferred ([D15]): its raid0
	// does not exist, so the transfer device carries an error table like a
	// standby's and its namespace stays inaccessible — never an optimized path
	// over a device that is not there.
	deferred bool
	sectors  uint64
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func newCntlrPlan(
	nf *common.NameFmt,
	req *pb.SyncupCntlrRequest,
) *cntlrPlan {
	ptr := req.GetCntlrPointer()
	cntlr := req.GetCntlr()
	level := req.GetSpLevel()
	bdev := req.GetBdevConf()

	p := &cntlrPlan{
		nf:                nf,
		clusterId:         req.GetClusterId(),
		cnId:              req.GetCnId(),
		spId:              ptr.GetSpId(),
		cntlrId:           ptr.GetCntlrId(),
		req:               req,
		cntlr:             cntlr,
		level:             level,
		disabled:          cntlr.GetDisabled(),
		primary:           cntlr.GetPrimary() && !cntlr.GetDisabled(),
		blockSize:         bdev.GetDmPoolConf().GetDataBlockSize(),
		stripeSize:        bdev.GetDmRaid0Conf().GetStripeSize(),
		lowWaterPct:       bdev.GetDmPoolConf().GetLowWaterMarkPct(),
		raid1:             bdev.GetRedundConf().GetRedundMdRaid1() != nil,
		bitmapChunkBlocks: bdev.GetRedundConf().GetRedundMdRaid1().GetBitmapChunkBlockCnt(),
		sliceById:         make(map[uint64]*slicePlan),
		grpById:           make(map[uint64]*grpPlan),
		legById:           make(map[uint64]*legPlan),
		tdById:            make(map[uint64]*tdPlan),
		tdByDevId:         make(map[uint32]*tdPlan),
		cloneById:         make(map[uint64]*clonePlan),
		cloneByTd:         make(map[uint64]*clonePlan),
		xferById:          make(map[uint64]*xferPlan),
	}
	p.wantAny = level < pb.SpLevel_SP_LEVEL_DISABLE
	p.levelLeg = p.wantAny && level < pb.SpLevel_SP_LEVEL_NO_SIDE
	p.levelGrp = p.wantAny && level < pb.SpLevel_SP_LEVEL_NO_REDUND
	p.levelPool = p.wantAny && level < pb.SpLevel_SP_LEVEL_NO_THINPOOL
	p.levelClone = p.wantAny && level < pb.SpLevel_SP_LEVEL_NO_CLONE
	p.wantLeg = p.levelLeg
	p.wantGrp = p.primary && p.levelGrp
	p.wantPool = p.primary && p.levelPool
	p.wantClone = p.primary && p.levelClone
	p.readOnly = p.primary && p.wantAny &&
		level >= pb.SpLevel_SP_LEVEL_READONLY

	p.buildSlices()
	p.buildCloneGrps()
	// [D15]: the effective desired state is computed once every group is
	// built, the clone meta groups included, since it defers a group by its
	// legs, and before the rest, because buildTds (tdPlan.deferred),
	// buildClones, buildSubsystems (the CN16 ANA rule) and buildXfers all
	// read it.
	p.computeEffective()
	p.buildTds()
	p.buildClones()
	p.buildSubsystems()
	p.buildXfers()
	return p
}

func (p *cntlrPlan) buildSlices() {
	idToSlice := p.req.GetIdToSlice()
	p.sliceCnt = uint64(len(idToSlice))
	for key, slice := range idToSlice {
		sliceId := parseIdKey(key)
		sp := &slicePlan{
			sliceId:  sliceId,
			sliceIdx: slice.GetSliceIdx(),
			slice:    slice,
			poolMetaName: p.nf.CnPoolMetaName(
				p.clusterId, p.cnId, p.spId, sliceId),
			poolDataName: p.nf.CnPoolDataName(
				p.clusterId, p.cnId, p.spId, sliceId),
			poolFinalName: p.nf.CnPoolFinalName(
				p.clusterId, p.cnId, p.spId, sliceId),
		}
		p.slices = append(p.slices, sp)
		p.sliceById[sliceId] = sp
	}
	sort.Slice(p.slices, func(i, j int) bool {
		if p.slices[i].sliceIdx != p.slices[j].sliceIdx {
			return p.slices[i].sliceIdx < p.slices[j].sliceIdx
		}
		return p.slices[i].sliceId < p.slices[j].sliceId
	})
	// The concat sizes are summed by computeEffective, over the *effective*
	// group lists ([D15]).
	for _, sp := range p.slices {
		sp.metaGrps = p.buildGrps(sp, sp.slice.GetMetaGrpList(), true)
		sp.dataGrps = p.buildGrps(sp, sp.slice.GetDataGrpList(), false)
	}
}

func (p *cntlrPlan) buildGrps(
	sp *slicePlan,
	groups []*pb.Group,
	isMeta bool,
) []*grpPlan {
	out := make([]*grpPlan, 0, len(groups))
	for grpIdx, grp := range groups {
		gp := p.newGrpPlan(grp)
		gp.slice = sp
		gp.isMeta = isMeta
		gp.grpIdx = uint32(grpIdx)
		if p.raid1 {
			gp.mdDevName = p.nf.CnMdDevName(p.clusterId, p.cnId, p.spId,
				sp.sliceIdx, gp.grpIdx, isMeta)
			gp.mdArrayName = p.nf.CnMdArrayName(
				p.spId, sp.sliceIdx, gp.grpIdx, isMeta)
		}
		p.addGrp(gp)
		out = append(out, gp)
	}
	return out
}

// buildCloneGrps builds the clone meta group of every clone in clone_list
// (architecture.md, Terminology and object model) as buildGrps builds a
// slice's group: its legs and spare legs, connected and wrapped on every
// cntlr (CN10), and on the primary its md array or, without redundancy,
// its CnGrpName dm-linear (CN12), under the md names of a clone meta group
// (architecture.md, md names), since a slice's md names carry the group's
// place in its slice. A clone with no group gets nothing. It runs before
// computeEffective, which marks a leg provisioning and defers a group with a
// provisioning leg_list leg ([D15]): built after it, a clone meta group
// whose sides still zero would be converged as if they were exported, and
// its leg and group rows would read ERROR, feeding health epochs and leg
// repair, where they read PROVISIONING.
func (p *cntlrPlan) buildCloneGrps() {
	for _, clone := range p.req.GetCloneList() {
		grp := clone.GetMetaGrp()
		if grp == nil {
			continue
		}
		gp := p.newGrpPlan(grp)
		gp.clone = clone
		if p.raid1 {
			gp.mdDevName = p.nf.CnCloneMdDevName(
				p.clusterId, p.cnId, p.spId, gp.grpId)
			gp.mdArrayName = p.nf.CnCloneMdArrayName(p.spId)
		}
		p.addGrp(gp)
	}
}

// newGrpPlan is the part of a group's plan that the group alone decides,
// for a slice's group and a clone meta group alike: its ids, its redundancy
// and its on-leg geometry (architecture.md, Group on-leg layout: meta
// region, data region, health block). The caller places it and, for an md
// group, names its array.
func (p *cntlrPlan) newGrpPlan(grp *pb.Group) *grpPlan {
	gp := &grpPlan{
		grpId:      grp.GetGrpId(),
		grp:        grp,
		raid1:      p.raid1,
		metaBlocks: grp.GetMetaBlocks(),
		dataBlocks: grp.GetDataBlocks(),
	}
	gp.dataSectors = grp.GetDataBlocks() * p.blockSize / agent.SectorSize
	gp.dataOffsetSectors =
		grp.GetMetaBlocks() * p.blockSize / agent.SectorSize
	gp.dataOffsetKiB = grp.GetMetaBlocks() * p.blockSize / 1024
	gp.bitmapChunkKiB = p.bitmapChunkBlocks * p.blockSize / 1024
	return gp
}

// addGrp finishes a group whose caller has placed it and named its md array:
// the group device's path and row name, its legs and spare legs, and its
// place in the plan's groups. A RedundNone group's device is CnGrpName,
// unique in the pool by its group id alone.
func (p *cntlrPlan) addGrp(gp *grpPlan) {
	if p.raid1 {
		gp.devPath = p.nf.MdPath(gp.mdDevName)
		gp.resName = gp.devPath
	} else {
		gp.dmName = p.nf.CnGrpName(p.clusterId, p.cnId, p.spId, gp.grpId)
		gp.devPath = p.nf.DmPath(gp.dmName)
		gp.resName = gp.dmName
	}
	gp.legs = p.buildLegs(gp, gp.grp.GetLegList(), false)
	gp.spares = p.buildLegs(gp, gp.grp.GetSpareLegList(), true)
	p.grps = append(p.grps, gp)
	p.grpById[gp.grpId] = gp
}

func (p *cntlrPlan) buildLegs(
	gp *grpPlan,
	legs []*pb.Leg,
	spare bool,
) []*legPlan {
	out := make([]*legPlan, 0, len(legs))
	for _, leg := range legs {
		lp := &legPlan{
			legId: leg.GetLegId(),
			leg:   leg,
			grp:   gp,
			spare: spare,
			sides: leg.GetSideList(),
			nqn: p.nf.SideToCnNqn(
				p.clusterId, p.spId, leg.GetLegId(), p.cnId),
			name: p.nf.CnLegName(
				p.clusterId, p.cnId, p.spId, leg.GetLegId()),
		}
		lp.path = p.nf.DmPath(lp.name)
		lp.sectors = (gp.metaBlocks + gp.dataBlocks) * p.blockSize /
			agent.SectorSize
		if metaBytes := gp.metaBlocks * p.blockSize; metaBytes >=
			common.LegHealthBlockSize {
			lp.healthOffset = metaBytes - common.LegHealthBlockSize
		}
		out = append(out, lp)
		p.legs = append(p.legs, lp)
		p.legById[lp.legId] = lp
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].leg.GetLegIdx() != out[j].leg.GetLegIdx() {
			return out[i].leg.GetLegIdx() < out[j].leg.GetLegIdx()
		}
		return out[i].legId < out[j].legId
	})
	return out
}

// computeEffective is the [D15] effective-desired-state pass. The raw desired
// state names resources whose DN sides are still being zeroed
// (architecture.md, Side provisioning protocol): such a
// side exports nothing at all, so anything built on top of it could only fail.
// The plan therefore carries both shapes — the raw one for the report, the
// effective one for converging and probing — and every resource the effective
// shape leaves out reports RES_STATUS_PROVISIONING instead of an error, so a
// freshly created SP never feeds err_epoch or the replacement flows of
// architecture.md, Automatic reactions.
//
//   - a leg is provisioning iff **every** side of side_list has
//     provisioned = false (a provisioned src beside an unprovisioned dst is a
//     serving, migrating leg);
//   - a group holding a provisioning leg_list leg is deferred: no md array,
//     no CnGrpName, out of the pool concats and out of the pool sizing;
//   - a deferred group defers every group *after* it in the same list, because
//     the list order is the concat's physical order (effectiveGrps);
//   - an unprovisioned spare defers only itself (spares never assemble);
//   - a slice with no effective group on one of its two sides is deferred
//     whole, because an empty concat is an error and not a shape;
//   - a deferred slice defers every td (the raid0 spans all slices) and so,
//     through CN16, every namespace's ANA.
func (p *cntlrPlan) computeEffective() {
	for _, lp := range p.legs {
		lp.provisioning = legProvisioning(lp.sides)
	}
	for _, gp := range p.grps {
		for _, lp := range gp.legs { // leg_list only — never gp.spares
			if lp.provisioning {
				gp.deferred = true
				p.anyDeferred = true
				break
			}
		}
	}
	for _, sp := range p.slices {
		sp.effMetaGrps = effectiveGrps(sp.metaGrps)
		sp.effDataGrps = effectiveGrps(sp.dataGrps)
		for _, gp := range sp.effMetaGrps {
			sp.metaSectors += gp.dataSectors
		}
		for _, gp := range sp.effDataGrps {
			sp.dataSectors += gp.dataSectors
			sp.dataBlocks += gp.dataBlocks
		}
		sp.deferred =
			(len(sp.metaGrps) > 0 && len(sp.effMetaGrps) == 0) ||
				(len(sp.dataGrps) > 0 && len(sp.effDataGrps) == 0)
		if sp.deferred {
			p.anyDeferred = true
		}
	}
}

// legProvisioning is the [D15] leg predicate. An empty side_list is deliberately
// *not* provisioning: a leg with no side is a malformed request, and CN10's
// existing "no multipath namespace" error must keep saying so rather than
// turning into a healthy PROVISIONING row.
func legProvisioning(sides []*pb.Side) bool {
	if len(sides) == 0 {
		return false
	}
	for _, side := range sides {
		if side.GetProvisioned() {
			return false
		}
	}
	return true
}

// effectiveGrps truncates one of a slice's two group lists at its first
// deferred group: **a deferred group defers every group after it in its list**.
// It is a prefix cut and never an order-preserving filter, because a group's
// index in the list is its physical place in the pool concat. Dropping a
// deferred group out of the middle would build the groups after it at the
// deferred one's offsets, and re-inserting the target when that group finally
// cleared would move every pool-data block dm-thin had allocated meanwhile —
// silent corruption reported as OK. Truncating instead is exactly what
// CN28 ("a not-yet-grown concat/pool is OK, not a mismatch; the
// grow completes when the group clears") and architecture.md, GrowSlice ("the
// concat and the pool keep their old, effective size") describe, and it is
// what makes
// the surviving groups' concat offsets — and so dataGrpSpanStart's CN27
// arithmetic — those of the live table.
//
// A group after the cut is still *built* (its own legs are provisioned and its
// md array is healthy); it is only not a concat target yet, so it reports OK
// and joins the concat on the pass where every group before it has cleared.
func effectiveGrps(grps []*grpPlan) []*grpPlan {
	out := make([]*grpPlan, 0, len(grps))
	for _, gp := range grps {
		if gp.deferred {
			break
		}
		out = append(out, gp)
	}
	return out
}

// effective reports whether a group is still one of its slice's concat
// targets. It is not the negation of gp.deferred: [D15]'s deferral is a prefix
// cut (effectiveGrps), so a group of its own is undeferred yet out of the
// effective state whenever an earlier group of the same list is deferred. A
// clone meta group is in no slice and no concat.
func (gp *grpPlan) effective() bool {
	if gp.slice == nil {
		return false
	}
	list := gp.slice.effDataGrps
	if gp.isMeta {
		list = gp.slice.effMetaGrps
	}
	for _, eff := range list {
		if eff == gp {
			return true
		}
	}
	return false
}

// anySliceDeferred: one deferred slice defers every td, because a td's raid0
// stripes across every slice's thin volume (CN15).
func (p *cntlrPlan) anySliceDeferred() bool {
	for _, sp := range p.slices {
		if sp.deferred {
			return true
		}
	}
	return false
}

// deferredFromErr reports one converged resource that may be
// provisioning-deferred ([D15]): a real failure still wins — a fault is a fault
// whatever the sides underneath are doing — while a deferred resource that
// converged without error is PROVISIONING rather than OK.
func deferredFromErr(
	t *agent.ResTracker,
	deferred bool,
	key string,
	resName string,
	details string,
	err error,
) *pb.ResInfo {
	if err == nil && deferred {
		return t.Provisioning(key, resName, detailsProvisioning)
	}
	return t.FromErr(key, resName, details, err)
}

// deferredSet is deferredFromErr's probe-side twin: a probe fault wins, an
// otherwise healthy deferred resource reports PROVISIONING.
func deferredSet(
	t *agent.ResTracker,
	deferred bool,
	key string,
	resName string,
	status pb.ResStatus,
	details string,
) *pb.ResInfo {
	if deferred && status == pb.ResStatus_RES_STATUS_OK {
		return t.Provisioning(key, resName, detailsProvisioning)
	}
	return t.Set(key, resName, status, details)
}

func (p *cntlrPlan) buildTds() {
	for _, td := range p.req.GetTdList() {
		tp := &tdPlan{
			tdId: td.GetTdId(),
			td:   td,
			raid0Name: p.nf.CnRaid0Name(
				p.clusterId, p.cnId, p.spId, td.GetTdId()),
			errorName: p.nf.CnErrorName(
				p.clusterId, p.cnId, p.spId, td.GetTdId()),
			deferred: p.anySliceDeferred(),
			sectors:  td.GetSize() / agent.SectorSize,
		}
		if p.sliceCnt > 0 {
			tp.thinSectors = td.GetSize() / p.sliceCnt / agent.SectorSize
		}
		p.tds = append(p.tds, tp)
		p.tdById[tp.tdId] = tp
		p.tdByDevId[td.GetDevId()] = tp
	}
}

func (p *cntlrPlan) buildClones() {
	for _, clone := range p.req.GetCloneList() {
		cp := &clonePlan{
			cloneId: clone.GetCloneId(),
			clone:   clone,
			dstTd:   p.tdById[clone.GetDstTdId()],
			finalName: p.nf.CnCloneFinalName(
				p.clusterId, p.cnId, p.spId, clone.GetCloneId()),
		}
		if grp := clone.GetMetaGrp(); grp != nil {
			if gp := p.grpById[grp.GetGrpId()]; gp != nil && gp.clone == clone {
				cp.grp = gp
			}
		}
		// [D15]: a clone whose destination td is missing entirely stays the CN18
		// error below; one whose destination or clone meta group is merely
		// still provisioning is deferred.
		cp.deferred = cp.dstTd == nil || cp.dstTd.deferred ||
			(cp.grp != nil && cp.grp.deferred)
		if cp.dstTd != nil {
			cp.sectors = cp.dstTd.sectors
			if p.blockSize != 0 {
				cp.regionSectors = p.blockSize / agent.SectorSize
				cp.regionCnt = cp.dstTd.td.GetSize() / p.blockSize
			}
		}
		p.clones = append(p.clones, cp)
		p.cloneById[cp.cloneId] = cp
		p.cloneByTd[clone.GetDstTdId()] = cp
	}
}

func (p *cntlrPlan) buildSubsystems() {
	for nqn, ss := range p.req.GetNqnToSubsystem() {
		sp := &ssPlan{nqn: nqn, ss: ss, ssId: ss.GetSsId()}
		p.subsystems = append(p.subsystems, sp)
	}
	sort.Slice(p.subsystems, func(i, j int) bool {
		return p.subsystems[i].nqn < p.subsystems[j].nqn
	})
	for _, sp := range p.subsystems {
		for _, ns := range sp.ss.GetNsList() {
			np := &nsPlan{
				ss:    sp,
				ns:    ns,
				nsId:  ns.GetNsId(),
				nsIdx: int(ns.GetNsIdx()),
				td:    p.tdById[ns.GetTdId()],
				devName: p.nf.CnNsDevName(
					p.clusterId, p.cnId, p.spId, ns.GetNsId()),
			}
			if np.td != nil {
				np.sectors = np.td.sectors
			}
			// deferred is read by nsBacking, so it is assigned first. Below
			// SP_LEVEL_NO_CLONE a clone whose clone meta group still
			// provisions defers the namespaces of its destination td as a
			// destination that still provisions does: rule 5's dm-clone
			// cannot be built yet (CN18). At that level and above the level
			// wins (CN9, CN19): no dm-clone is wanted, and rule 4 or an
			// earlier one backs those namespaces whatever the group does.
			np.deferred = np.td != nil && np.td.deferred
			if cp := p.cloneByTd[ns.GetTdId()]; np.td != nil && cp != nil &&
				cp.deferred && p.levelClone {
				np.deferred = true
			}
			np.suspended = p.effectiveSuspend(ns)
			np.backingName, np.flakey = p.nsBacking(np)
			if cp := p.cloneByTd[ns.GetTdId()]; cp != nil &&
				np.backingName == cp.finalName {
				np.clone = cp
			}
			np.anaGrpId = common.AnaGrpIdInaccessible
			// CN16's desired group: optimized iff primary ∧ not disabled
			// (folded into p.primary) ∧ not effectively suspended ∧ the
			// backing chain is not provisioning-deferred ([D15]) ∧ sp_level
			// below SP_LEVEL_DISABLE (wantAny). During initial provisioning
			// hosts queue on an inaccessible path instead of eating IO errors
			// from an error-backed ns-dev. The build moves a namespace to
			// optimized only in a pass whose ns-dev step left its ns-dev
			// where the rules want it, or held it on a device it still
			// serves through (nsAnaMove), and one parked while its rules
			// want a serving device waits inaccessible (nsAnaWanted).
			if p.primary && p.wantAny && !np.suspended && !np.deferred {
				np.anaGrpId = common.AnaGrpIdOptimized
			}
			sp.namespaces = append(sp.namespaces, np)
			p.namespaces = append(p.namespaces, np)
		}
	}
}

// effectiveSuspend is the effective-suspend rule of cnagent.md CN16: a
// namespace is suspended iff its
// stored flag says so **or** an auto_suspend transfer names it — unless an
// auto_resume clone targets its td, which overrides to not-suspended. That
// override is the flow of architecture.md, Transfer + clone = cross-SP live
// migration: the destination namespace is *created*
// suspended and serves anyway while the clone runs.
//
// The bit it returns is consumed twice, and neither consumer suspends
// anything: nsBacking rule 1 *parks* the ns-dev on the td's dm-error, and the
// ANA rule above keeps the namespace in the inaccessible group ([D12]).
func (p *cntlrPlan) effectiveSuspend(ns *pb.Namespace) bool {
	for _, clone := range p.req.GetCloneList() {
		if clone.GetAutoResume() && clone.GetDstTdId() == ns.GetTdId() {
			return false
		}
	}
	if ns.GetSuspended() {
		return true
	}
	for _, xfer := range p.req.GetXferList() {
		if !xfer.GetAutoSuspend() {
			continue
		}
		if xfer.GetOriNsIdx() != ns.GetNsIdx() {
			continue
		}
		if ss := p.req.GetNqnToSubsystem()[xfer.GetOriNqn()]; ss != nil {
			for _, candidate := range ss.GetNsList() {
				if candidate.GetNsId() == ns.GetNsId() {
					return true
				}
			}
		}
	}
	return false
}

// nsBacking is the CN16 backing state machine, evaluated in order — first
// match wins. It returns the dm name the ns-dev's table points at and whether
// the [D11] dm-flakey wrapper goes over it.
func (p *cntlrPlan) nsBacking(np *nsPlan) (string, bool) {
	if np.td == nil {
		return "", false
	}
	// 0. [D15]: the backing chain is provisioning-deferred — neither the raid0
	// nor a clone over it exists yet, or the clone's clone meta group does
	// not. The ns-dev is built on the td's permanent dm-error, and the ANA
	// rule above keeps the namespace inaccessible, so a host queues rather
	// than erroring.
	if np.deferred {
		return np.td.errorName, false
	}
	// 1. *parked*: the namespace is effectively suspended (architecture.md,
	// Namespace suspend semantics), so its
	// ns-dev is a live dm-linear over the td's dm-error — never under
	// dm-flakey, and never dm-suspended ([D12]). np.suspended is assigned
	// before this runs.
	if np.suspended {
		return np.td.errorName, false
	}
	// 2. standby or disabled cntlr; 3. no thin pools at this level.
	if !p.primary || p.level >= pb.SpLevel_SP_LEVEL_NO_THINPOOL {
		return np.td.errorName, false
	}
	clone, cloned := p.cloneByTd[np.td.tdId]
	// 4. a clone targets the td and the level suppresses clones: a raid0
	// with holes must never serve.
	if cloned && p.level >= pb.SpLevel_SP_LEVEL_NO_CLONE {
		return np.td.errorName, false
	}
	// 5. the clone backing — which the ns-dev takes only while the dm-clone
	// shows hydration on and its metadata read-write (nsDevNow); 6. the
	// plain raid0 — 7. under dm-flakey when the level is read-only.
	if cloned {
		return clone.finalName, p.readOnly
	}
	return np.td.raid0Name, p.readOnly
}

func (p *cntlrPlan) buildXfers() {
	for _, xfer := range p.req.GetXferList() {
		xp := &xferPlan{
			xferId: xfer.GetXferId(),
			xfer:   xfer,
			finalName: p.nf.CnXferFinalName(
				p.clusterId, p.cnId, p.spId, xfer.GetXferId()),
			nqn: p.nf.XferNqn(p.clusterId, p.spId, xfer.GetXferId()),
			ori: p.findNs(xfer.GetOriNqn(), xfer.GetOriNsIdx()),
		}
		if xp.ori != nil {
			xp.sectors = xp.ori.sectors
			// [D15]: the origin td's raid0 does not exist while the td
			// provisions, so the transfer device carries an error table and
			// its namespace stays inaccessible. The transfer maps that raid0
			// and no dm-clone, so a clone meta group that still provisions,
			// which defers the origin namespace (CN9), defers no transfer.
			xp.deferred = xp.ori.td != nil && xp.ori.td.deferred
		}
		p.xfers = append(p.xfers, xp)
		p.xferById[xp.xferId] = xp
	}
}

func (p *cntlrPlan) findNs(nqn string, nsIdx uint32) *nsPlan {
	for _, sp := range p.subsystems {
		if sp.nqn != nqn {
			continue
		}
		for _, np := range sp.namespaces {
			if np.ns.GetNsIdx() == nsIdx {
				return np
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Derived values shared by the converge pass and the probe
// ---------------------------------------------------------------------------

// cntlidRange is the cntlr's slot (architecture.md, cntlid slots), which
// every host-facing and every
// transfer subsystem of this cntlr carries. nvmet's range includes both
// ends, so the slot stops one short of the next slot's first id.
func (p *cntlrPlan) cntlidRange() (uint32, uint32) {
	min := uint32(common.CnCntlidSlotBase) +
		p.cntlr.GetCntlidSlot()*uint32(common.CnCntlidSlotStep)
	return min, min + uint32(common.CnCntlidSlotStep) - 1
}

func (p *cntlrPlan) hostNqn() string {
	return p.nf.CnHostNqn(p.clusterId, p.cnId)
}

// lowWaterMark is the dm thin-pool's event threshold (CN13): the pool fires
// when its usage passes low_water_mark_pct, so the mark itself is the
// complementary count of free blocks.
//
// pct > 100 means "auto-grow off" and passes 0 — no dm events at all. pct = 0
// is INVALID and does not reach here: the control plane resolves it to the
// default of architecture.md, Common validation, when it writes the conf, and
// both paths that reach this arithmetic
// sit behind an agent.ValidateBdevConf gate. poolArgs is this method's only
// caller, and poolArgs runs only from ensurePool (the converge, gated in
// syncupCntlr and convergeCntlr) and from probePool (the probe, gated in
// readCntlr). The other plan builders — PushCloneBitmap and the CN25 bitmap
// reads — compute no mark at all. This agent has no default of
// its own to apply, which is the point: a mark it invented would be one the
// pool was not sized for.
func (p *cntlrPlan) lowWaterMark(dataBlocks uint64) uint64 {
	pct := p.lowWaterPct
	if pct > 100 {
		return 0
	}
	return dataBlocks * uint64(100-pct) / 100
}

// thinName is the per-slice thin volume of one td.
func (p *cntlrPlan) thinName(tdId, sliceId uint64) string {
	return p.nf.CnThinDevName(p.clusterId, p.cnId, p.spId, tdId, sliceId)
}

// stripeSectors is the raid0 chunk (CN15).
func (p *cntlrPlan) stripeSectors() uint64 {
	return p.stripeSize / agent.SectorSize
}

// blockSectors is the thin-pool block (CN13).
func (p *cntlrPlan) blockSectors() uint64 {
	return p.blockSize / agent.SectorSize
}

// dataGrpSpanStart is the CN27 arithmetic: a data group's first pool-data
// block is the summed data_blocks of the groups before it in data_grp_list.
// The walk is over the *effective* list ([D15]), which is the live pool-data
// table: a group at or after the first provisioning-deferred one is no concat
// target at all and is reported not-found, so no caller is ever handed the
// span of a region that is another group's — or that does not exist yet.
func (sp *slicePlan) dataGrpSpanStart(grpId uint64) (uint64, bool) {
	var start uint64
	for _, gp := range sp.effDataGrps {
		if gp.grpId == grpId {
			return start, true
		}
		start += gp.dataBlocks
	}
	return 0, false
}

func parseIdKey(key string) uint64 {
	var id uint64
	if _, err := fmt.Sscanf(key, common.IdKeyFmt, &id); err != nil {
		return 0
	}
	return id
}
