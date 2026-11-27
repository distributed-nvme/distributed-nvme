package cnagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Teardown by sweep (architecture.md, Teardown by sweep; cnagent.md CN21)
//
// Removal is derived by comparing the DESIRED state with the ACTUAL state of
// the node, never from a remembered plan and never from a remembered failure.
// Each pass enumerates what exists, subtracts what the desired state wants,
// removes the rest top-down, verifies each removal with a probe that cannot
// block on a dead remote, and recomputes "something is left" from scratch.
// A removal derived from a remembered plan would forget a failed removal
// together with the plan that named it, and nothing would enumerate the
// object again: an unanswered sysfs listing of the arrays read as "the array
// is not there" would leave the array pinning its leg wrappers for ever.
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
//     failfast window has passed and the same order succeeds, unless a path
//     stays live while none can serve, which holds the IO without limit
//     (osclient.md, Exported raw helpers and the probe-IO carve-out).
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
	// dropped, or whose numbers did not parse.
	dms map[string]string
	// ours is the subset whose name decodes as a dm name of this cluster and
	// this cn.
	ours map[string]common.DmName
	// byDevNo resolves a live table's device argument back to a dm name,
	// which is how a wanted device's backing is recognised as unwanted.
	byDevNo map[string]string
	// dmListed says the dm listing behind dms/ours/byDevNo ANSWERED: the
	// pass's own `dmsetup ls`, or in a read the reading's `dmsetup info`
	// listing, partial or not (CN30). When it did not, those three maps are
	// empty for want of an answer rather than because the node holds nothing,
	// and an empty map is the shape of "there is nothing here" — so no removal
	// may be derived from this snapshot at all (architecture.md, Teardown by
	// sweep: "gone" is probe-verified, never inferred). The pass reports what
	// it can and re-drives.
	dmListed bool
	// arrays is every md array on the node, read from sysfs alone.
	arrays []MdArray
	// mdListed is dmListed for arrays: the /sys/block enumeration answered,
	// for every array of it under ListArrays' strict rule (in a read, the
	// reading's md walk read that way, MdScan.Arrays).
	// Its silence costs the most: with arrays empty for want of an answer,
	// L9 would find no array to stop and read clean, and L10 would
	// disconnect unwanted legs from under a live one.
	mdListed bool
	// hostSubsys is every nvme subsystem entry this host holds, including
	// one the kernel keeps with no controller (disconnectVerified).
	hostSubsys []agent.SubsysBrief
	// hostListed is dmListed for hostSubsys: the sysfs subsystem walk
	// answered.
	hostListed bool
	// hostWalk is the walk hostSubsys came from, the pass's one walk of the
	// host's subsystems: each check that a removed connection is gone finds
	// its entry there and reads that entry alone (disconnectVerified), so a
	// pass lists /sys/class/nvme-subsystem once however many connections it
	// checks. nil when the walk's listing did not answer.
	hostWalk *agent.SubsysWalk
	// nvmetSubsys is every subsystem in the target's configfs tree, linked to
	// the port or not.
	nvmetSubsys []string
	// nvmetListed is dmListed for nvmetSubsys: the listing of the configfs
	// subsystems tree answered, in the agent's own process (dnagent.md
	// SH18); one that failed is no empty tree.
	nvmetListed bool
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

// enumerateCn takes the snapshot. Each of its four listings that did not
// answer is recorded as a failure and leaves its part of the snapshot empty —
// the sweep then removes nothing from the node and reports the failure,
// because an empty answer it cannot trust must never read as "there is
// nothing there", and every removal gated on absence would read it exactly
// so (listed).
//
// Where the listings come from depends on the pass. A read passes its reading
// (rd) and its walk (pw): the dm devices are the reading's `dmsetup info`
// listing, which answers even when partial — only this cntlr's own converges,
// the node's syncup and the startup reconcile create or remove its devices,
// and none of them runs beside the read, so a device a partial listing lacks
// is someone else's (CN30) — and a `dmsetup table` listing that did not answer
// fails the verdict too; the arrays are the reading's md walk under
// ListArrays' rule (MdScan.Arrays); the connections are the walk's. A converge
// passes no reading and its own walk, which its build's lookups share: it
// lists the dm devices with the strict `dmsetup ls` and the arrays with
// ListArrays. The node-level passes pass neither, and take a walk of their own
// (newPassWalk). Every list of connections fails on any subsysnqn read that
// did not answer (SubsysWalk.Briefs).
//
// node says the pass is a node-level one, which alone judges a host-facing
// subsystem with no namespace device by its age (nvmetOwner).
func (s *CnAgentServer) enumerateCn(
	ctx context.Context,
	clusterId uint64,
	cnId uint64,
	rd *cnReading,
	pw *passWalk,
	node bool,
	res *agent.SweepResult,
) *cnActual {
	actual := &cnActual{
		dms:     make(map[string]string),
		ours:    make(map[string]common.DmName),
		byDevNo: make(map[string]string),
	}
	dms, err := s.listDms(ctx, rd, res)
	if err == nil {
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
	arrays, err := s.listArrays(ctx, rd)
	if err != nil {
		res.Fail("md arrays", err)
	} else {
		actual.mdListed = true
		actual.arrays = arrays
	}
	if pw == nil {
		pw = s.newPassWalk(ctx)
	}
	hostSubsys, err := s.hostSubsys(pw)
	if err != nil {
		res.Fail("nvme subsystems", err)
	} else {
		actual.hostListed = true
		actual.hostSubsys = hostSubsys
		actual.hostWalk = pw.walk
	}
	nvmetSubsys, err := s.nvmet.ListSubsystems(ctx)
	if err != nil {
		res.Fail("nvmet subsystems", err)
	} else {
		actual.nvmetListed = true
		actual.nvmetSubsys = nvmetSubsys
	}
	actual.nvmetOwner = make(map[string]nvmetAttr, len(actual.nvmetSubsys))
	links := &portLinks{}
	for _, nqn := range actual.nvmetSubsys {
		spId, ours, unowned := s.nvmetOwner(
			ctx, clusterId, cnId, nqn, node, links, res)
		actual.nvmetOwner[nqn] = nvmetAttr{
			spId: spId, ours: ours, unowned: unowned}
	}
	return actual
}

// listDms is the dm part of enumerateCn, name to number, with each listing
// that did not answer recorded in res under its own name: with no reading
// one `dmsetup ls`, strict; with a reading its `dmsetup info` listing, which
// answers when partial, and its `dmsetup table` listing, whose failure fails
// the verdict but leaves the names standing. The error is the name
// listing's.
func (s *CnAgentServer) listDms(
	ctx context.Context,
	rd *cnReading,
	res *agent.SweepResult,
) (map[string]string, error) {
	if rd == nil {
		dms, err := s.dm.List(ctx)
		if err != nil {
			res.Fail("dmsetup ls", err)
		}
		return dms, err
	}
	if rd.infoErr != nil {
		res.Fail("dmsetup info", rd.infoErr)
	}
	if rd.tableErr != nil {
		res.Fail("dmsetup table", rd.tableErr)
	}
	if rd.infoErr != nil {
		return nil, rd.infoErr
	}
	dms := make(map[string]string, len(rd.infoList.Devs))
	for name, dev := range rd.infoList.Devs {
		dms[name] = dev.DevNo
	}
	return dms, nil
}

// listArrays is the md part of enumerateCn under ListArrays' rule: the
// reading's md walk when the pass took a reading, a ListArrays of its own
// when it did not.
func (s *CnAgentServer) listArrays(
	ctx context.Context,
	rd *cnReading,
) ([]MdArray, error) {
	if rd == nil {
		return s.md.ListArrays(ctx)
	}
	if rd.mdErr != nil {
		return nil, rd.mdErr
	}
	return rd.md.Arrays()
}

// hostSubsys is the list of connections of enumerateCn, from the pass's
// walk: a converge's own, a read's in its reading, or the one a node-level
// pass takes.
func (s *CnAgentServer) hostSubsys(pw *passWalk) ([]agent.SubsysBrief, error) {
	if pw.err != nil {
		return nil, pw.err
	}
	return pw.walk.Briefs()
}

// listed says every one of the four listings behind the snapshot answered.
// A part that did not is empty for want of an answer, which is the shape of
// "there is nothing here" to every removal gated on absence — the layer stop
// rule first of all — so a snapshot with any part missing licenses no removal
// at all (architecture.md, Teardown by sweep).
func (a *cnActual) listed() bool {
	return a.dmListed && a.mdListed && a.hostListed && a.nvmetListed
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
// what keeps a co-hosted dn agent's udev-assembled array untouched. CN12's
// forced start stops a group's array only under the same rule
// (MdDetail.strangerErr).
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
		legSp, ok := legWrapperSp(clusterId, cnId, member)
		if !ok {
			return 0, false
		}
		if i == 0 {
			spId = legSp
		} else if legSp != spId {
			return 0, false
		}
	}
	return spId, true
}

// legWrapperSp is the sp a dm name names when it is a kind-c9 leg wrapper of
// this cluster and cn, and false for any other name: the member test of
// arrayOwner.
func legWrapperSp(clusterId, cnId uint64, name string) (uint64, bool) {
	dn, ok := common.ParseDmName(name)
	if !ok || dn.Kind != common.DmKindCnLeg ||
		dn.ClusterId != clusterId || dn.NodeId != cnId {
		return 0, false
	}
	return dn.SpId(), true
}

// nvmetOwner attributes one subsystem in the target's configfs tree. It is
// called once per subsystem per pass, from enumerateCn, in every scope: each
// cntlr's converge and read, and the node's passes (node). Its reads come in
// one order for every caller, and a step that decides reads nothing after
// it (cnagent.md CN21).
//
// A dnv-format NQN carries its own ids. One that carries dnv's prefix and
// decodes to nothing is judged by a claim alone: a held request that names it
// makes it that cntlr's sp's, and unclaimed it is never touched — it may be a
// newer build's subsystem over a wanted ns-dev after a rollback, and the
// answer of any read could only be used to remove it. A HOST-FACING NQN is
// chosen by the user and carries nothing. A claim settles it first; then what
// its namespaces point at: a namespace whose device_path is a kind-c6 ns-dev
// of ours makes the subsystem that sp's. dnv exports a host namespace only
// over an ns-dev, so a namespace over any other device, another cn's ns-dev
// included, makes the subsystem foreign.
//
// One with no namespace device at all is no pool's at cntlr level, and
// nothing more of it is read there. At node level its allowed hosts and its
// port links are listed next (emptySubsysOwner). With a host or a port link
// its attr_model decides, with no age: common.NvmeModel, which every
// host-facing subsystem dnv makes carries, makes it **unowned** — what a
// deletion or a teardown leaves once its namespaces have gone — anything
// else foreign (a model read that did not answer is not dnv's, and is named
// as a failure). With neither, its age decides and no attribute file of it
// is read: younger than common.CnSubsysOrphanGrace it is left alone, older
// it exports nothing and is unowned whatever its model — what a converge cut
// off between its `mkdir` and its model write leaves. On a kernel that
// stamps the directory on every lookup of an attribute file, a model read
// on each cntlr read would keep a directory young for good, and one on each
// node read would keep a linked one young for the SyncupCn it brings. Only
// the node-level sweep removes an unowned subsystem — which assumes at most
// one cn agent per kernel. A co-hosted dn agent is fine: its NQNs are all
// dnv-format kinds 2 and 3, which never reach that arm.
//
// ours is false for a subsystem this agent must never touch, or that is no
// pool's at cntlr level; unowned is true only when ours is true and no sp
// could be named.
func (s *CnAgentServer) nvmetOwner(
	ctx context.Context,
	clusterId uint64,
	cnId uint64,
	nqn string,
	node bool,
	links *portLinks,
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
	// An NQN a stored cntlr still names settles it before anything is read,
	// and it has to come FIRST: a subsystem whose last namespace was just
	// deleted has nothing left to attribute it by, and the namespace rule
	// below would call it unowned and take the host's subsystem away while
	// `nqn_to_subsystem` still names it.
	if spId, ok := s.hostFacingClaim(clusterId, cnId, nqn); ok {
		return spId, true, false
	}
	if common.IsDnvNqn(nqn) {
		// In our namespace but decoding to nothing, and claimed by no held
		// request: never touched. Treating it as host-facing would let a
		// malformed name get a subsystem deleted.
		slog.DebugContext(ctx, "undecodable dnv nqn left alone",
			slog.String("nqn", nqn))
		return 0, false, false
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
		path = strings.TrimSpace(path)
		if !ok || path == "" || path == agent.NsDevicePathUnset {
			// A namespace whose device was never written names nothing:
			// what a build cut off between its namespace mkdir and its
			// device_path write leaves.
			continue
		}
		dn, ok := common.ParseDmName(strings.TrimPrefix(path, "/dev/mapper/"))
		if !ok || dn.Kind != common.DmKindCnNsDev {
			// A namespace over a device that is no ns-dev: dnv exports a
			// host namespace over nothing else, so the subsystem is foreign.
			return 0, false, false
		}
		if dn.ClusterId != clusterId || dn.NodeId != cnId {
			// A namespace backed by ANOTHER cn's ns-dev: foreign, not
			// unowned (`architecture.md`, Teardown by sweep, "Several agents
			// share one kernel"). One cn agent per kernel is the ordinary
			// deployment,
			// so this should not arise — but the unowned arm below REMOVES,
			// and a rule that has to be right only while an assumption holds
			// is the shape that takes a sibling's subsystem when it stops
			// holding.
			return 0, false, false
		}
		return dn.SpId(), true, false
	}
	if !node {
		// No namespace device: no pool's, and only the node-level scope
		// judges it further. A cntlr pass reads nothing more of it, so its
		// reads every round leave the directory's age alone.
		return 0, false, false
	}
	return s.emptySubsysOwner(ctx, nqn, links, res)
}

// emptySubsysOwner is the node-level half of nvmetOwner for a subsystem no
// held request claims and with no namespace device. Its allowed hosts and
// its port links are listed first, in the agent's own process; a listing
// looks up no attribute file, so it leaves the directory's time as it is.
//
// With a host or a port link, the model alone decides, with no age:
// common.NvmeModel makes it unowned, anything else foreign. A dnv build in
// flight never reaches here, since a cntlr's request is stored (storeReq,
// putCntlr) before its converge makes anything, so the subsystem is
// claimed; and the age could not decide anyway: on a kernel that stamps
// the directory on an attribute lookup, the model read of each node read
// would make it young again for the SyncupCn that read brings, for good.
//
// With neither, its age decides, and the model is never read: younger than
// common.CnSubsysOrphanGrace it is left alone, as it may be a build between
// its `mkdir` and its first namespace whose request is not this agent's to
// see (another program's); older, it exports nothing and is unowned
// whatever its model. Nothing of such a subsystem looks up an attribute
// file, so a directory nothing touches ages.
func (s *CnAgentServer) emptySubsysOwner(
	ctx context.Context,
	nqn string,
	links *portLinks,
	res *agent.SweepResult,
) (spId uint64, ours bool, unowned bool) {
	hosts, found, err := s.nvmet.ListAllowedHosts(ctx, nqn)
	if err != nil {
		res.Fail("nvmet allowed hosts of "+nqn, err)
		return 0, false, false
	}
	if !found {
		// Removed between the listing and this read.
		return 0, false, false
	}
	linked := len(hosts) > 0
	if !linked {
		byNqn, err := s.portLinksOf(ctx, links)
		if err != nil {
			res.Fail("nvmet port links", err)
			return 0, false, false
		}
		linked = byNqn[nqn]
	}
	if linked {
		model, ok, err := s.nvmet.SubsysModel(ctx, nqn)
		if err != nil {
			res.Fail("nvmet "+nqn+" attr_model", err)
			return 0, false, false
		}
		if !ok || model != common.NvmeModel {
			return 0, false, false
		}
		return 0, true, true
	}
	mtime, present, err := s.nvmet.SubsysMtime(ctx, nqn)
	if err != nil {
		res.Fail("nvmet subsystem age of "+nqn, err)
		return 0, false, false
	}
	if !present {
		return 0, false, false
	}
	if s.now().Sub(mtime) <= common.CnSubsysOrphanGrace {
		return 0, false, false
	}
	// Old, and empty: no namespace device, no host, no port. It exports
	// nothing, whoever made it.
	return 0, true, true
}

// portLinks is the lazily built "which subsystems are linked to a port"
// set of one pass, built from the ports directory and each port's links,
// listed in the agent's own process, the first time emptySubsysOwner needs
// it, so a pass with no unclaimed subsystem without namespace device and
// host lists no port. A listing that
// fails is the pass's error, never a port with no link (dnagent.md SH18).
type portLinks struct {
	done   bool
	err    error
	linked map[string]bool
}

// portLinksOf is the pass's set of subsystems linked to any port of the
// kernel, this agent's or another's.
func (s *CnAgentServer) portLinksOf(
	ctx context.Context,
	links *portLinks,
) (map[string]bool, error) {
	if links.done {
		return links.linked, links.err
	}
	links.done = true
	links.linked = make(map[string]bool)
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
			links.linked[nqn] = true
		}
	}
	return links.linked, nil
}

// hostFacingClaim reports whether any cntlr of this cn still names one
// host-facing NQN, or one that carries dnv's prefix and decodes to nothing,
// in its stored request, and under which sp. It is the request-derived half
// of the attribution rule (cnagent.md CN21): such an NQN carries no ids of
// ours, so "somebody here still wants it" is evidence in its own right — and
// the only evidence there is for a subsystem that currently has no namespace
// at all.
//
// A cntlr's request is stored (storeReq, and putCntlr for a new cntlr)
// before its converge creates anything, so a subsystem cannot be built by a
// pass whose claim is not already visible here.
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
		req := st.loadReq()
		if _, named := req.GetNqnToSubsystem()[nqn]; named {
			return req.GetCntlrPointer().GetSpId(), true
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
//	c5 per-td dm-error            wantAny   (ensureTdError; a standby keeps it)
//	c6 ns-dev + nvmet namespace   wantAny   (ensureNsDev / ensureSubsystem)
//	host-facing nvmet subsystem   wantAny   (ensureSubsystem)
//	c8 xfer + its subsystem/ns    wantAny   (ensureXfer)
//	c7 clone + :4:                wantClone (ensureClone)
//
// The legs and groups are the plan's, a clone's clone meta group among them
// (buildCloneGrps), so a clone meta group's legs and its array or group
// linear are wanted under the same gates as a slice's, for as long as its
// clone is in the request.
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
			w.addDm(cp.finalName)
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

	nvmetSs []string  // host-facing and xfer subsystems
	nvmetNs []cnNsRef // namespaces under a subsystem that stays
	nsDevs  []string  // c6
	xferDms []string  // c8
	clones  []string  // c7
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

// chainOutcome is what runChain tells its caller beside the leftovers it adds
// to the pass's result.
type chainOutcome struct {
	// lingeringClones are the chain's dm-clones that may still be live after
	// the pass: every one of them when L3 did not run, because the descent
	// stopped above it, and the ones whose removal L3 did not verify
	// otherwise. A cntlr's build holds the ns-devs of their destination
	// raid0s off those raid0s (lingeringCloneHold, CN18).
	lingeringClones []string
	// failedDeletes are the pools, in the order L7 met them and each once,
	// that refused or did not answer a delete message of L7 this pass. A
	// cntlr's converge sweeps their ids in the same pass
	// (sweepAfterFailedDeletes, CN14).
	failedDeletes []string
}

// runChain removes one sp's unwanted objects top-down and reports what is
// left in res. The layers are CN21's order; the `stuck` check between them is
// the stop rule: a layer that left anything behind ends the descent, and the
// layers below report their objects as leftovers WITHOUT touching them.
// Removing a leg out from under a live array would strand a migration's md
// superblocks, and a pass that simply re-runs next round costs nothing.
//
// plan is the cntlr's plan in a cntlr's converge, from which L7 reads the
// slices it discards on (removeThins), and nil in a node-level pass, whose
// wanted set holds no pool, so that L7 discards nothing.
func (s *CnAgentServer) runChain(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	chain *cnChain,
	actual *cnActual,
	wanted *cnWanted,
	res *agent.SweepResult,
) chainOutcome {
	var out chainOutcome
	// P0, before every layer, the park step: every unwanted ns-dev and
	// transfer device is reloaded onto an error table of its own size. Two
	// things need that. Disabling the nvmet namespace above it in L1 waits,
	// unkillable, for every request in flight on the namespace, which a
	// SUSPENDED dm device never completes; and the reload's own flushing
	// suspend is what completes the in-flight host IO instead of replaying it
	// at resume onto a stack that is about to be removed (CN16, CN17).
	//
	// It runs in three steps (parkStep): every unwanted namespace moves to
	// the inaccessible ANA group, so nvmet refuses new host IO to it; every
	// suspended device of ours below an unwanted ns-dev or transfer device
	// is resumed, so the park's flushing suspend does not wait for IO held
	// below it; and each such device is parked. A device the park cannot
	// prove out of suspension STOPS the chain before L1, as a layer that left
	// something behind does: L1's disable over it would hold the pass, and
	// its locks, in the kernel for as long as the device stays suspended, and
	// no later pass could reach the reload that would release it. The stopped
	// chain names the device, and the converge the worker's re-send brings
	// parks it again.
	//
	// The size comes from the device's own live table, so this needs neither
	// the td that backed it — which may be leaving in the same pass — nor any
	// memory of what the ns-dev mapped before.
	stuck := !s.parkStep(ctx, chain, actual)
	// cloneLayerRan is set by L3 alone: before it runs, every dm-clone of
	// the chain may still be live.
	cloneLayerRan := false
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
					// The park step has moved it ANA-inaccessible, so a host
					// that still holds a path is told to stop using it rather
					// than losing it under IO.
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
			run: func() bool {
				cloneLayerRan = true
				for _, name := range chain.clones {
					if !s.removeDmVerified(ctx, name) {
						res.Add(agent.LeftoverKindDm, name)
						out.lingeringClones = append(
							out.lingeringClones, name)
					}
				}
				return len(out.lingeringClones) > 0
			},
			report: func() { s.reportDms(res, chain.clones) },
		},
		{ // L5 — clone-source connections, once nothing maps them.
			run: func() bool {
				left := false
				for _, nqn := range chain.srcNqnList() {
					if !s.disconnectVerified(ctx, actual, nqn) {
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
		{ // L7 — thin volumes, each discarded first where its id is
			// deleted, and the pool-side `delete` of their ids.
			run: func() bool {
				return s.removeThins(
					ctx, plan, chain, actual, wanted, res, &out)
			},
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
				// ensurePool's probe-match path, and arms nothing — so the
				// activation sweep never runs, and every id deleted meanwhile
				// can hold its data blocks for the life of the pool. Keeping an
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
		{ // L10 — legs: the connections, then the wrappers.
			run: func() bool {
				left := false
				// Their probers are cancelled already — by the converge's
				// trim before its sweep (convergeCntlr), or at node level by
				// the drop step (CN7) — but one still wedged on its leg
				// holds an open fd on the wrapper; the disconnect set
				// going below is what releases one stuck in D state (CN11).
				for _, nqn := range chain.legNqns {
					if !s.disconnectVerified(ctx, actual, nqn) {
						res.Add(agent.LeftoverKindNvme, nqn)
						left = true
					}
				}
				// A wrapper is removed without waiting for its connection,
				// whose disconnect runs off the pass or is stuck: they are
				// two different objects. One a wedged prober still holds
				// fails EBUSY until that disconnect has errored the prober's
				// IO, so it normally goes on a later pass.
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
	if !cloneLayerRan {
		out.lingeringClones = chain.clones
	}
	return out
}

// cloneHold is what a converge's build may not put an ns-dev onto (CN18): the
// raid0s a dm-clone the plan does not want may still write into after this
// pass's sweep. That dm-clone goes on hydrating into its destination until
// L3 removes it, and a region it has not copied yet would overwrite a write
// a host made to the raid0 directly. all holds every raid0 of the cntlr, for
// a pass that cannot say which ones such a dm-clone writes into.
type cloneHold struct {
	all bool
	// dests are the dm names of the raid0s a lingering dm-clone writes into.
	dests map[string]struct{}
}

// holds reports whether the build may not put an ns-dev onto raid0Name.
func (h cloneHold) holds(raid0Name string) bool {
	if h.all {
		return true
	}
	_, ok := h.dests[raid0Name]
	return ok
}

// lingeringCloneHold reads which raid0s the named unwanted dm-clones write
// into, each from its live table's destination device (CloneTable's second
// argument), resolved to a dm name through the pass's dm listing. Only what
// it can read narrows the hold. When the dm listing did not answer, nothing
// rules out a dm-clone the listing would have named, and when a dm-clone's
// table does not answer, is not one dm-clone target, or names a device the
// listing does not hold, its destination is unknown: the hold is then the
// whole cntlr's, as one dm-clone cannot be told from another. A dm-clone
// whose table read fails and which a `dmsetup info` then confirms gone holds
// nothing. It reads only, and only what the converge already found
// unwanted, so a pass with no lingering dm-clone reads nothing here.
func (s *CnAgentServer) lingeringCloneHold(
	ctx context.Context,
	names []string,
	actual *cnActual,
) cloneHold {
	if !actual.dmListed {
		return cloneHold{all: true}
	}
	hold := cloneHold{dests: make(map[string]struct{}, len(names))}
	for _, name := range names {
		targets, err := s.dm.Table(ctx, name)
		if err != nil {
			if dev, infoErr := s.dm.Info(
				ctx, name); infoErr == nil && dev == nil {
				continue
			}
			return cloneHold{all: true}
		}
		if len(targets) != 1 || targets[0].Type != "clone" ||
			len(targets[0].Args) < 2 {
			return cloneHold{all: true}
		}
		dest, ok := actual.byDevNo[targets[0].Args[1]]
		if !ok {
			return cloneHold{all: true}
		}
		hold.dests[dest] = struct{}{}
	}
	return hold
}

// parkStep is the chain's park step (P0): the ANA move of every unwanted
// namespace, the release of the devices below, then the park of each
// unwanted ns-dev and transfer device. It reports whether every one of those
// devices is now known out of suspension (parkByTable); every one is
// attempted whatever the others gave.
func (s *CnAgentServer) parkStep(
	ctx context.Context,
	chain *cnChain,
	actual *cnActual,
) bool {
	s.unwantedNsAna(ctx, chain)
	s.releaseStacks(ctx, chain, actual)
	ok := true
	for _, names := range [][]string{chain.nsDevs, chain.xferDms} {
		for _, name := range names {
			if !s.parkByTable(ctx, name, actual) {
				ok = false
			}
		}
	}
	return ok
}

// unwantedNsAna moves every unwanted namespace of the chain to the
// inaccessible ANA group before the park: one dropped under a subsystem that
// stays, and every namespace of an unwanted subsystem (one that left the
// request, or every one at SP_LEVEL_DISABLE), listed for it. nvmet refuses
// IO to an inaccessible group, so from the move on no new host IO reaches
// the ns-dev the park suspends — the order the planned park has too
// (parkNsDev). A move or a listing that fails is logged and stops nothing:
// the park still completes or errors the IO in flight, and its own result
// is what the chain stops on.
func (s *CnAgentServer) unwantedNsAna(ctx context.Context, chain *cnChain) {
	for _, ref := range chain.nvmetNs {
		s.setAnaLogged(ctx, ref.nqn, ref.nsid)
	}
	for _, nqn := range chain.nvmetSs {
		nsids, _, err := s.nvmet.ListNamespaces(ctx, nqn)
		if err != nil {
			slog.ErrorContext(ctx, "listing the namespaces of a subsystem "+
				"before its park failed",
				slog.String("nqn", nqn),
				slog.String("error", err.Error()))
			continue
		}
		for _, nsid := range nsids {
			s.setAnaLogged(ctx, nqn, nsid)
		}
	}
}

// releaseStacks is the park step's second part, as the dn's releaseStacks:
// below every unwanted ns-dev and transfer device of the chain it walks the
// live tables down and resumes every suspended device of ours it finds, on
// whatever table is live, children before their parent. Nothing on a
// controller node stays suspended across passes on purpose (architecture.md,
// Namespace suspend semantics), so a device of ours found suspended — a raid0
// or a thin volume — was left so by a reload or a quiesce that failed or was
// cut off, and the IO it holds came through devices that still map it: a bare
// resume releases it where it was going, and without it the park's flushing
// suspend would wait for that IO until the soft timeout killed it. One
// `dmsetup info` listing of the node comes first, and when it answered whole
// and reads no device of ours suspended but the ones the step parks, nothing
// is walked: the common case costs one command whatever the stacks' depth. A
// read or a resume that fails is logged; the park's own result is what the
// chain stops on.
func (s *CnAgentServer) releaseStacks(
	ctx context.Context,
	chain *cnChain,
	actual *cnActual,
) {
	starts := make(map[string]bool, len(chain.nsDevs)+len(chain.xferDms))
	for _, names := range [][]string{chain.nsDevs, chain.xferDms} {
		for _, name := range names {
			starts[name] = true
		}
	}
	if len(starts) == 0 {
		return
	}
	listing, err := s.dm.ListInfo(ctx)
	if err == nil && !listing.Partial {
		suspended := false
		for name, dev := range listing.Devs {
			if _, ours := actual.ours[name]; ours && dev.Suspended &&
				!starts[name] {
				suspended = true
				break
			}
		}
		if !suspended {
			return
		}
	}
	seen := make(map[string]bool)
	for _, names := range [][]string{chain.nsDevs, chain.xferDms} {
		for _, name := range names {
			s.releaseBelow(ctx, name, actual, starts, seen)
		}
	}
}

// releaseBelow walks name's live table down and resumes every suspended
// device of ours on the way, children before their parent, so a device's
// released IO meets no suspended device below it; name itself is resumed
// too unless it is one the park step parks.
func (s *CnAgentServer) releaseBelow(
	ctx context.Context,
	name string,
	actual *cnActual,
	starts map[string]bool,
	seen map[string]bool,
) {
	if seen[name] {
		return
	}
	seen[name] = true
	if _, listed := actual.dms[name]; !listed {
		return
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		slog.ErrorContext(ctx, "reading a dm table below a park failed",
			slog.String("dm", name),
			slog.String("error", err.Error()))
		return
	}
	for _, target := range targets {
		for _, arg := range target.Args {
			below, found := actual.byDevNo[arg]
			if !found {
				continue
			}
			if _, ours := actual.ours[below]; !ours {
				continue
			}
			s.releaseBelow(ctx, below, actual, starts, seen)
		}
	}
	if _, ours := actual.ours[name]; !ours || starts[name] {
		return
	}
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		slog.ErrorContext(ctx, "probing a dm device below a park failed",
			slog.String("dm", name),
			slog.String("error", err.Error()))
		return
	}
	if dev == nil || !dev.Suspended {
		return
	}
	if err := s.dm.Resume(ctx, name); err != nil {
		slog.ErrorContext(ctx, "resuming a dm device below a park failed",
			slog.String("dm", name),
			slog.String("error", err.Error()))
	}
}

// parkByTable stops an UNWANTED ns-dev or transfer device serving, deriving
// everything it needs from the device's own live table — there is no plan
// for a namespace that left the desired state, and its td may be leaving in
// the same pass.
//
// An ns-dev's park target is the CN16 one where the live table still says
// what it is: an ns-dev that is a dm-linear over a kind-c4 raid0 names its td
// in that raid0's own name, and the td's kind-c5 dm-error, when it reads
// present, carries the same ids. Anywhere else — a table this build never
// wrote, the dm-flakey of a read-only ns-dev, an ns-dev over a dm-clone whose
// td cannot be read back, a raid0 whose td's dm-error is missing or does not
// read, or a transfer device, whose shape off service is an error table of
// its size (CN17) — the device is reloaded onto an error target of its own
// size instead, which is the same thing one indirection shorter. When the load onto the td's dm-error fails, the plain error table
// is loaded at once: an error target names no device, so a dm-error that went
// or changed its number since the read cannot fail it.
//
// A device already parked is left alone but RESUMED if it is suspended:
// disabling the nvmet namespace above it waits for its in-flight IO, which a
// suspended dm device never completes.
//
// It reports whether the device is now known out of suspension: absent,
// never suspended, or its reload or resume went through. A `dmsetup info`
// that did not answer is none of those, and neither is a suspended device
// whose table did not read, as its size is then unknown: a bare resume would
// send the IO it holds onto the stack being retired (Dm.Reload fails
// closed).
func (s *CnAgentServer) parkByTable(
	ctx context.Context,
	name string,
	actual *cnActual,
) bool {
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		slog.ErrorContext(ctx, "probing a device before its park failed",
			slog.String("dm", name),
			slog.String("error", err.Error()))
		return false
	}
	if dev == nil {
		return true
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		slog.ErrorContext(ctx, "reading a device table before removal failed",
			slog.String("dm", name),
			slog.String("error", err.Error()))
		return !dev.Suspended
	}
	var sectors uint64
	for _, target := range targets {
		sectors += target.Length
	}
	if sectors == 0 {
		return !dev.Suspended
	}
	resume := func() bool {
		if !dev.Suspended {
			return true
		}
		if err := s.dm.Resume(ctx, name); err != nil {
			slog.ErrorContext(ctx, "resuming a parked device failed",
				slog.String("dm", name),
				slog.String("error", err.Error()))
			return false
		}
		return true
	}
	// The reload below is the whole park: its internal flushing suspend
	// completes the in-flight IO, and installing the new table before the
	// resume is what errors the stragglers instead of replaying them onto a
	// stack that is about to go.
	reload := func(table string) bool {
		if err := s.dm.Reload(ctx, name, table); err != nil {
			slog.ErrorContext(ctx, "parking a device before removal failed",
				slog.String("dm", name),
				slog.String("error", err.Error()))
			return false
		}
		return true
	}
	errorTable := agent.ErrorTable(sectors)
	own, parsed := common.ParseDmName(name)
	nsDev := parsed && own.Kind == common.DmKindCnNsDev
	if len(targets) == 1 {
		if targets[0].Type == "error" && targets[0].Length == sectors {
			return resume()
		}
		if nsDev && targets[0].Type == "linear" &&
			len(targets[0].Args) > 0 {
			backing, ok := actual.ours[actual.byDevNo[targets[0].Args[0]]]
			if ok && backing.Kind == common.DmKindCnError {
				return resume()
			}
			if ok && backing.Kind == common.DmKindCnRaid0 {
				// c4 and c5 carry the same ids: [sp, td].
				errName := s.nf.CnErrorName(backing.ClusterId, backing.NodeId,
					backing.Ids[0], backing.Ids[1])
				errDev, err := s.dm.Info(ctx, errName)
				if err == nil && errDev != nil {
					devNo, err := s.dmDevNo(ctx, nil, errName)
					if err == nil {
						if reload(agent.LinearTable(sectors, devNo, 0)) {
							return true
						}
						// The load failed: the plain error table at once.
						return reload(errorTable)
					}
				}
			}
		}
	}
	return reload(errorTable)
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
// kernel will act on. It records in out the pools whose delete message
// failed, for the sweep that follows a failed delete
// (sweepAfterFailedDeletes).
//
// A volume whose id the layer deletes is discarded first, before its
// removal, so the disk nodes learn that its blocks are free (discardThin);
// the delete alone changes only the pool's metadata. Only there: a volume
// removed with its pool unwanted — a demotion, a level that drops the pool, a
// whole sp leaving — is deactivated, and its blocks are the data the next
// primary attaches. The discard is best effort, and nothing that becomes of
// it stops the removal or the delete.
func (s *CnAgentServer) removeThins(
	ctx context.Context,
	plan *cntlrPlan,
	chain *cnChain,
	actual *cnActual,
	wanted *cnWanted,
	res *agent.SweepResult,
	out *chainOutcome,
) bool {
	left := false
	discard := newThinDiscard(plan)
	for _, name := range chain.thins {
		id := s.thinIdentity(ctx, name, actual)
		_, poolWanted := wanted.dms[id.pool]
		deletes := id.pool != "" && id.devId != "" && poolWanted
		if deletes {
			s.discardThin(ctx, discard, name, id)
		}
		if !s.removeDmVerified(ctx, name) {
			res.Add(agent.LeftoverKindDm, name)
			left = true
			continue
		}
		if !deletes {
			continue
		}
		if s.deleteThinIdByName(ctx, id.pool, id.devId) != nil &&
			!slices.Contains(out.failedDeletes, id.pool) {
			out.failedDeletes = append(out.failedDeletes, id.pool)
		}
	}
	return left
}

// thinId is what a thin volume's live table says of it: its pool's dm name,
// its dev_id and its length in sectors. pool and devId are both empty when
// the table cannot be read.
type thinId struct {
	pool    string
	devId   string
	sectors uint64
}

// thinIdentity reads a thin volume's pool and dev_id out of its live table.
// Both come back empty when the table cannot be read — the device may already
// be gone — which makes the caller skip the `delete` rather than guess an id.
func (s *CnAgentServer) thinIdentity(
	ctx context.Context,
	name string,
	actual *cnActual,
) thinId {
	targets, err := s.dm.Table(ctx, name)
	if err != nil || len(targets) != 1 || targets[0].Type != "thin" ||
		len(targets[0].Args) < 2 {
		return thinId{}
	}
	return thinId{
		pool:    actual.byDevNo[targets[0].Args[0]],
		devId:   targets[0].Args[1],
		sectors: targets[0].Length,
	}
}

// thinDiscard is L7's discard state for one pass (discardThin): the chunks
// the pass may still send, the one `dmsetup info` listing of the node it
// reads suspended devices from, and each slice's answer, read once in the
// pass whatever the number of volumes the slice's pool loses.
type thinDiscard struct {
	plan *cntlrPlan
	// chunks is what is left of the pass's CnThinDiscardPassChunks.
	chunks int
	// listing is the node's `dmsetup info` listing, read at the first
	// volume that has something to discard; listErr is why there is none
	// to read from, a listing that failed or answered partial.
	listed  bool
	listing *agent.DmInfoListing
	listErr error
	// sliceWhy is each slice's reason to take no discard, "" for one that
	// may take it.
	sliceWhy map[uint64]string
}

func newThinDiscard(plan *cntlrPlan) *thinDiscard {
	return &thinDiscard{
		plan:     plan,
		chunks:   common.CnThinDiscardPassChunks,
		sliceWhy: make(map[uint64]string),
	}
}

// discardThin discards one thin volume whose id L7 is about to delete
// (cnagent.md CN14, CN21), in chunks of CnThinDiscardChunkBytes rounded down
// to whole pool blocks, through one DiscardRanges call. The range ends at the
// end of the volume's highest mapped block as its status reads it, or at the
// volume's last whole block, and its chunks go top first, down toward the
// first sector. dm-thin frees a block, and passes the discard down to the
// pool's data device, only where no other volume maps it, so a snapshot keeps
// the blocks it shares.
//
// The top-first order is what lets a later pass go on: the status names only
// the highest mapped sector, which a pass's top chunks lower, so the next pass
// that finds the volume — the re-send after a removal that failed — starts
// below where this one stopped. A volume that ends inside a pool block keeps
// that block once it is written: dm-thin unmaps only the blocks a discard
// covers whole. So the range stops at the last whole block, the part-block
// is counted as left, and a volume whose status reads only that block mapped
// gets no discard at all, which keeps a re-send from opening it again for
// nothing. Such a block also keeps the volume's highest sector at its end, so
// a re-send after a pass the budget cut starts again at the last whole block.
//
// It sends nothing while a leg of the slice has two sides, as the pool then
// passes no discard down to its data device (CN13), and nothing where the
// discard could wait in the kernel, which it would do under the cntlr's CN1
// locks: when a leg of the slice's groups has
// a disconnect in flight, or its lookup in the pass's walk fails or finds no
// live, optimized path (a discard into a dead leg waits out the leg fast IO
// fail timeout), or when the volume, its pool or a dm device under the pool
// reads suspended (sliceDiscardWhy); nor when the pool's status does not read
// read-write, as dm-thin completes a discard on a read-only pool and unmaps
// nothing. The pass sends at most CnThinDiscardPassChunks chunks over all its
// volumes, so a pass's discards hold the locks for a bounded count of
// BLKDISCARDs. Best effort: whatever it leaves — skipped, cut by the budget,
// failed, or a mapped part-block — is logged with its bytes and why, and once
// the volume is removed nothing discards it again, since the delete that
// follows ends the volume and nothing remembers what was left.
func (s *CnAgentServer) discardThin(
	ctx context.Context,
	td *thinDiscard,
	name string,
	id thinId,
) {
	stayed := func(bytes uint64, why string) {
		slog.InfoContext(ctx, "thin volume not discarded in full",
			slog.String("dm", name),
			slog.String("pool", id.pool),
			slog.Uint64("bytes_left", bytes),
			slog.String("reason", why))
	}
	raw, err := s.dm.StatusNoFlush(ctx, name)
	if err != nil {
		stayed(id.sectors*agent.SectorSize, "status read: "+err.Error())
		return
	}
	st, ok := common.ParseThinStatus(raw)
	if !ok {
		stayed(id.sectors*agent.SectorSize, "status does not parse: "+raw)
		return
	}
	if !st.Mapped {
		return
	}
	sp := td.sliceOfPool(id.pool)
	if sp == nil || td.plan.blockSize < agent.SectorSize {
		stayed(st.MappedSectors*agent.SectorSize,
			"no slice of the request names the pool")
		return
	}
	block := td.plan.blockSize / agent.SectorSize
	whole := id.sectors / block * block
	// part is the volume's last part-block, in sectors, when the status
	// reads it mapped: a discard never unmaps it.
	var part uint64
	if st.HighestSector+1 > whole {
		part = id.sectors - whole
	}
	partWhy := "the volume's last block is partial, and no discard unmaps it"
	end := min(st.HighestSector+1, whole) * agent.SectorSize
	if end == 0 || part > 0 && st.MappedSectors <= block {
		if part > 0 {
			stayed(part*agent.SectorSize, partWhy)
		}
		return
	}
	left := end + part*agent.SectorSize
	if td.chunks <= 0 {
		stayed(left, "the pass's discard budget is spent")
		return
	}
	if why := s.sliceDiscardWhy(ctx, td, sp); why != "" {
		stayed(left, why)
		return
	}
	if dev := td.listing.Devs[name]; dev != nil && dev.Suspended {
		stayed(left, "the volume is suspended")
		return
	}
	chunk := uint64(common.CnThinDiscardChunkBytes)
	if bytes := td.plan.blockSize; bytes <= chunk {
		chunk -= chunk % bytes
	}
	var ranges []common.DiscardRange
	for hi := end; hi > 0 && len(ranges) < td.chunks; {
		lo := uint64(0)
		if hi > chunk {
			lo = hi - chunk
		}
		ranges = append(ranges,
			common.DiscardRange{Offset: lo, Length: hi - lo})
		hi = lo
	}
	began := time.Now()
	done, err := s.oc.DiscardRanges(ctx, s.nf.DmPath(name), ranges)
	sent := done
	if err != nil {
		sent++
	}
	td.chunks -= sent
	var discarded uint64
	for _, r := range ranges[:done] {
		discarded += r.Length
	}
	left -= discarded
	slog.InfoContext(ctx, "thin volume discarded",
		slog.String("dm", name),
		slog.String("pool", id.pool),
		slog.Uint64("bytes", discarded),
		slog.Int("chunks", done),
		slog.Int64("duration_ms", time.Since(began).Milliseconds()))
	switch {
	case err != nil:
		stayed(left, "discard: "+err.Error())
	case discarded < end:
		stayed(left, "the pass's discard budget is spent")
	case part > 0:
		stayed(left, partWhy)
	}
}

// sliceOfPool is the plan's slice whose pool is the dm device pool, nil for
// none.
func (td *thinDiscard) sliceOfPool(pool string) *slicePlan {
	if td.plan == nil {
		return nil
	}
	for _, sp := range td.plan.slices {
		if sp.poolFinalName == pool {
			return sp
		}
	}
	return nil
}

// sliceDiscardWhy is the reason a slice takes no discard in this pass, ""
// for one that may (discardThin), read once a pass for each slice, the
// node's `dmsetup info` listing once for all of them. Every leg of the
// slice's groups that carries IO — the groups' legs and an open repair's new
// legs, a provisioning one excepted, as nothing reaches it — needs a path
// both live and optimized in the pass's walk and no disconnect in flight;
// the pool, its two concats, a RedundNone group's dm-linear and every such
// leg's wrapper need to read not suspended in the listing; and the pool's
// status needs to read read-write.
func (s *CnAgentServer) sliceDiscardWhy(
	ctx context.Context,
	td *thinDiscard,
	sp *slicePlan,
) string {
	if why, ok := td.sliceWhy[sp.sliceId]; ok {
		return why
	}
	why := s.readSliceDiscardWhy(ctx, td, sp)
	td.sliceWhy[sp.sliceId] = why
	return why
}

func (s *CnAgentServer) readSliceDiscardWhy(
	ctx context.Context,
	td *thinDiscard,
	sp *slicePlan,
) string {
	// CN13: while a leg of the slice has two sides the pool passes no
	// discard down to its data device, so a discard would free the blocks
	// in the pool alone, which the delete does anyway.
	if sp.keepsDiscards() {
		return "the pool keeps discards from its legs while a leg of the " +
			"slice has two sides"
	}
	devs := []string{sp.poolFinalName, sp.poolMetaName, sp.poolDataName}
	for _, gp := range slices.Concat(sp.metaGrps, sp.dataGrps) {
		if !gp.raid1 && gp.dmName != "" {
			devs = append(devs, gp.dmName)
		}
		legs := gp.legs
		if rp := gp.openRep(); rp != nil {
			legs = slices.Concat(legs, rp.newLegs)
		}
		for _, lp := range legs {
			if lp.provisioning {
				continue
			}
			if s.disconnectInFlight(lp.nqn) {
				return fmt.Sprintf("leg %016x is disconnecting", lp.legId)
			}
			view, err := s.lookupSubsys(ctx, td.plan.walk, lp.nqn, sideNsid)
			if err != nil {
				return fmt.Sprintf("leg %016x: %v", lp.legId, err)
			}
			if !view.available() {
				return fmt.Sprintf("leg %016x has no live, optimized path",
					lp.legId)
			}
			devs = append(devs, lp.name)
		}
	}
	if !td.listed {
		td.listed = true
		td.listing, td.listErr = s.dm.ListInfo(ctx)
		if td.listErr == nil && td.listing.Partial {
			td.listing, td.listErr = nil,
				errors.New("the dm listing answered partial")
		}
	}
	if td.listErr != nil {
		return "dm listing: " + td.listErr.Error()
	}
	for _, dev := range devs {
		if info := td.listing.Devs[dev]; info != nil && info.Suspended {
			return "dm device " + dev + " is suspended"
		}
	}
	raw, err := s.dm.StatusNoFlush(ctx, sp.poolFinalName)
	if err != nil {
		return "pool status read: " + err.Error()
	}
	if st, ok := common.ParseThinPoolStatus(raw); !ok || st.ReadOnly ||
		st.NeedsCheck {
		return "the pool is not read-write: " + raw
	}
	return ""
}

// deleteThinIdByName is the CN14 deletion message addressed by name. The pool
// is probed first, so a pool that has meanwhile gone, or whose probe does not
// answer, is silent: no message was sent, and nil comes back. A message that
// fails, refused or unanswered, is logged and its error returned.
func (s *CnAgentServer) deleteThinIdByName(
	ctx context.Context,
	poolName string,
	devId string,
) error {
	pool, err := s.dm.Info(ctx, poolName)
	if err != nil || pool == nil {
		return nil
	}
	if err := s.dm.Message(
		ctx, poolName, 0, "delete "+devId); err != nil {
		slog.ErrorContext(ctx, "thin-pool delete message failed",
			slog.String("pool", poolName),
			slog.String("error", err.Error()))
		return err
	}
	return nil
}

// sweepAfterFailedDeletes is CN14's sweep after a failed delete: each pool
// that refused, or did not answer, an L7 delete message of this pass has its
// thin ids swept in the same pass (sweepThinIds), under a request that
// counts as delivered only, as the activation sweep is (the caller's
// reqFromRpc gate). A
// delete that fails after its volume is gone is otherwise never sent again:
// no later pass lists the volume, and only a sweep of the pool's ids finds an
// id that has no dm device. The sweep deletes no id that a thin volume of
// this sp still maps: every one the pass's snapshot listed that is still
// there keeps the id its live table names in its pool (listedThinIds). Those
// tables are read once in the pass, for every such pool at once, and only
// once a pool's dump is in (sweepThinIds runs keep after its dump), so a pool
// that refuses its reserve — one in read-only or fail mode refuses every
// message — costs no table read. A table read that fails while its volume is
// still there stops every one of these sweeps, since that volume may map an
// id in any of the pools. Nothing is remembered: a sweep that fails too
// leaves the id to the next sweep of that pool's ids that gets through, and a
// sweep that succeeds also ends a pending activation sweep of its slice.
func (s *CnAgentServer) sweepAfterFailedDeletes(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	actual *cnActual,
	out chainOutcome,
) {
	var (
		listed  bool
		keeps   map[string]map[uint32]struct{}
		keepErr error
	)
	for _, poolName := range out.failedDeletes {
		var sp *slicePlan
		for _, candidate := range plan.slices {
			if candidate.poolFinalName == poolName {
				sp = candidate
				break
			}
		}
		if sp == nil {
			// removeThins messages only a pool the plan wants, so a slice
			// of the plan names it.
			continue
		}
		if keepErr != nil {
			slog.ErrorContext(ctx, msgThinSweepFailed,
				slog.String("pool", poolName),
				slog.String("error", keepErr.Error()))
			continue
		}
		s.sweepThinIds(ctx, st, plan, sp,
			func() (map[uint32]struct{}, error) {
				if !listed {
					listed = true
					keeps, keepErr = s.listedThinIds(
						ctx, plan, actual, out.failedDeletes)
				}
				return keeps[poolName], keepErr
			})
	}
}

// listedThinIds is, for each pool of pools, the set of thin ids in that pool
// that the thin volumes of this sp still map: every one the pass's snapshot
// listed, read once from its live table, keeps its id in the set of the pool
// its table's device resolves to through the snapshot's dm listing. A volume
// whose table names a device the listing does not resolve keeps its id in
// every set, since its pool is unknown; one that is not one thin target maps
// no id. A volume whose table read fails is skipped only when a probe then
// finds it gone, as one L7 removed is; otherwise its id is unknown, and the
// error stops every pool's sweep.
func (s *CnAgentServer) listedThinIds(
	ctx context.Context,
	plan *cntlrPlan,
	actual *cnActual,
	pools []string,
) (map[string]map[uint32]struct{}, error) {
	keeps := make(map[string]map[uint32]struct{}, len(pools))
	for _, pool := range pools {
		keeps[pool] = make(map[uint32]struct{})
	}
	ofSp := func(dn common.DmName) bool { return dn.SpId() == plan.spId }
	for _, name := range actual.dmsOfKind(common.DmKindCnThinDev, ofSp) {
		targets, err := s.dm.Table(ctx, name)
		if err != nil {
			if dev, infoErr := s.dm.Info(
				ctx, name); infoErr == nil && dev == nil {
				continue
			}
			return nil, fmt.Errorf("reading the thin id of %s: %w", name, err)
		}
		if len(targets) != 1 || targets[0].Type != "thin" ||
			len(targets[0].Args) < 2 {
			continue
		}
		pool, known := actual.byDevNo[targets[0].Args[0]]
		if _, swept := keeps[pool]; known && !swept {
			continue
		}
		devId, err := strconv.ParseUint(targets[0].Args[1], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%s maps the thin id %q: %w",
				name, targets[0].Args[1], err)
		}
		if known {
			keeps[pool][uint32(devId)] = struct{}{}
			continue
		}
		for _, keep := range keeps {
			keep[uint32(devId)] = struct{}{}
		}
	}
	return keeps, nil
}

// stopArrayVerified stops one array and verifies it from sysfs. The node it
// stops is the one sysfs named — /dev/mdN (or /dev/md_<name>), never
// /dev/md/<name>, which depends on udev having run — and nothing here runs
// `mdadm --detail`, whose member reads block until failfast on a leg whose DN
// side has gone.
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

// disconnectVerified reports whether an nvme host connection is gone, and
// sets its disconnect going when it is not. The probe is all the pass does
// under its locks: the `nvme disconnect` itself runs off them
// (startDisconnect), because a controller delete whose target vanished
// mid-delete does not return for the kernel's admin timeout, and so a
// connection that still has a controller is a leftover of THIS pass however
// fast its disconnect turns out to be — a later pass's probe is what finds
// it gone. A disconnect already in flight is not issued a second time.
//
// The probe reads the connection's entry of the pass's own walk alone
// (walkedEntry), so the pass lists /sys/class/nvme-subsystem once however
// many connections it checks.
//
// "Gone" is the absence of any CONTROLLER, not the absence of the
// subsystem directory (architecture.md, Teardown by sweep): the kernel keeps
// /sys/class/nvme-subsystem/nvme-subsysN after its last controller is deleted
// for as long as something, such as a leg wrapper or a dm-clone, holds its
// multipath head open, with its attributes readable and its namespace and
// controller nodes gone. Such a subsystem is no connection — no block device,
// no path — so reading the directory itself as present would stop the descent
// below its layer for as long as that holder stays, and disconnecting it
// again would be a command per pass. Nor is a controller the subsystem still
// lists after its device was deleted, until the last reference to it drops
// (HasCtrl; CN10): nothing of it is left to read or to delete. Read as
// present, it would hold the descent the same way and set a whole-NQN
// disconnect going on every pass; for a clone source, each one would reopen a
// window like the one cnagent.md, Known limits, describes: a build whose
// request is stored after the pass's read finds the deleted controller at no
// address, connects the source afresh and loses that controller to the
// disconnect.
func (s *CnAgentServer) disconnectVerified(
	ctx context.Context,
	actual *cnActual,
	nqn string,
) bool {
	dir, found, err := s.walkedEntry(ctx, actual, nqn)
	if err == nil && found {
		var read *agent.SubsysRead
		read, err = s.host.ReadSubsys(ctx, dir, agent.SubsysReadOpts{
			Rule: agent.ReadStrict,
		})
		if err == nil && !read.State(nqn).HasCtrl() {
			found = false
		}
	}
	if err != nil {
		slog.ErrorContext(ctx, "verifying an nvme disconnect failed",
			slog.String("nqn", nqn),
			slog.String("error", err.Error()))
		return false
	}
	if !found {
		return true
	}
	s.startDisconnect(ctx, nqn)
	return false
}

// disconnectOwnCtrlsVerified is disconnectVerified for a clone source the
// node-level pass judges by its controllers' host NQN (nodeUnownedSrcs):
// the connection is gone once no controller of it reads this cn's host NQN,
// and the disconnect it sets going takes those controllers one by one, by
// their devices (startDisconnectCtrls), never the whole NQN — the host keeps
// one subsystem per NQN, so a host stack on the same machine can hold a
// controller of the same subsystem with its own host NQN, and a disconnect
// by NQN would cut it.
func (s *CnAgentServer) disconnectOwnCtrlsVerified(
	ctx context.Context,
	clusterId uint64,
	cnId uint64,
	actual *cnActual,
	nqn string,
) bool {
	hostNqn := s.nf.CnHostNqn(clusterId, cnId)
	dir, found, err := s.walkedEntry(ctx, actual, nqn)
	var ctrls []string
	if err == nil && found {
		var read *agent.SubsysRead
		read, err = s.host.ReadSubsys(ctx, dir, agent.SubsysReadOpts{
			Ctrls:   agent.CtrlHostNqnEach,
			HostNqn: hostNqn,
			Rule:    agent.ReadStrict,
		})
		if err == nil {
			ctrls = read.HostNqnCtrls
		}
	}
	if err != nil {
		slog.ErrorContext(ctx, "verifying an nvme disconnect failed",
			slog.String("nqn", nqn),
			slog.String("error", err.Error()))
		return false
	}
	if len(ctrls) == 0 {
		return true
	}
	s.startDisconnectCtrls(ctx, nqn, dir, hostNqn, ctrls)
	return false
}

// walkedEntry is the directory of nqn's entry in the pass's walk, read again
// for its subsysnqn: the walk can be as old as the pass, and the kernel gives
// a freed entry's number to the next new subsystem, so an entry gone since
// the walk, or whose subsysnqn now reads another NQN, is no entry of nqn's
// (found false), as an NQN the walk did not find is not. A pass with no walk,
// an entry of the walk whose read did not answer where none matched, or a
// read again that did not answer, is an error.
func (s *CnAgentServer) walkedEntry(
	ctx context.Context,
	actual *cnActual,
	nqn string,
) (dir string, found bool, err error) {
	if actual.hostWalk == nil {
		return "", false, fmt.Errorf("subsystem %s %w: no NVMe walk in "+
			"this pass", nqn, errSubsysUnknown)
	}
	match, unread := actual.hostWalk.Find(nqn)
	if match == nil {
		if unread != nil {
			return "", false, unread.Err
		}
		return "", false, nil
	}
	got, ok, err := s.host.ReadSubsysNqn(ctx, match.Dir)
	if err != nil {
		return "", false, err
	}
	if !ok || got != nqn {
		return "", false, nil
	}
	return match.Dir, true, nil
}

// sweepCloneChunks deletes the bitmap chunk files of every clone this cntlr
// no longer carries (SH7). It is a sweep of the LOCAL STORE against the
// request, not a side effect of removing the clone's dm-clone: a standby
// builds no dm-clone at all, so keying the deletion off one would leave a
// deleted clone's chunks on disk for ever and the worker re-pushing them.
//
// A clone the role or the sp_level merely SUPPRESSES keeps its chunks
// applied-by-file (CN19, CN22): deleting them on every standby converge would
// make the worker re-push them every round, and would leave a promoted
// standby's build of the dm-clone (CN18 step 4) no chunk to apply. The test
// is membership of clone_list, which is exactly "the clone still exists".
//
// st.chunks mirrors the files, so a clone's entry goes only once its files
// have. A removal that did not happen keeps them indexed, the converge that
// tried names each as a leftover record, and so does every verdict
// (converge false) until one goes: that is what re-drives the pass that
// removes them. Dropping the entry first would forget files still on disk —
// nothing but a restart's orphan sweep lists the store again — and no reply
// would ever name them.
func (s *CnAgentServer) sweepCloneChunks(
	ctx context.Context,
	st *cntlrState,
	res *agent.SweepResult,
	converge bool,
) {
	req := st.loadReq()
	ptr := req.GetCntlrPointer()
	var gone []uint64
	var paths []string
	for cloneId := range st.chunks {
		if findClone(req, cloneId) != nil {
			continue
		}
		gone = append(gone, cloneId)
		paths = append(paths, s.cloneChunkPathsOf(st,
			req.GetClusterId(), req.GetCnId(),
			ptr.GetSpId(), cloneId)...)
	}
	sort.Strings(paths)
	if converge {
		err := s.store.Remove(ctx, paths...)
		if err == nil {
			for _, cloneId := range gone {
				delete(st.chunks, cloneId)
			}
			return
		}
		slog.ErrorContext(ctx, "removing clone bitmap chunks failed",
			slog.String("error", err.Error()))
	}
	for _, path := range paths {
		res.Add(agent.LeftoverKindRecord, path)
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
// write lock: a cntlr's request is stored (storeReq, and putCntlr for a new
// cntlr) before its converge issues any `nvme connect`, so a source cannot be
// connected by a build whose request is not yet visible here. The disconnect
// of a source found unused can land after the pass's locks are released
// (startDisconnect), and a build that finds it still in flight neither adopts
// nor connects that source (disconnectInFlight). A build whose request is
// stored after this read, and whose step 1 runs before the disconnect is
// registered, still adopts the source (cnagent.md Known limits).
//
// The live half reads the namespace node of every transfer connection, and
// of each connection in also: the other clone sources the node-level pass
// judges (nodeUnownedSrcs).
//
// A live dm-clone's table comes from rd, the reading of a cntlr's read, and
// from one `dmsetup table` of the device in every other pass (dmTable). The
// `dmsetup info` that confirms such a device gone after its table read failed
// is always fresh, never the reading's.
func (s *CnAgentServer) srcNqnsInUse(
	ctx context.Context,
	clusterId uint64,
	cnId uint64,
	actual *cnActual,
	rd *cnReading,
	also map[string]struct{},
	res *agent.SweepResult,
) map[string]struct{} {
	inUse := s.requestedSrcNqns(clusterId, cnId)
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
		targets, err := s.dmTable(ctx, rd, name)
		if err != nil {
			// The device may have gone — this runs at L5, after L3 removed
			// the unwanted clones — but if it did NOT, treating it as mapping
			// nothing could disconnect a live source. Absence is CONFIRMED,
			// never assumed, by a fresh read.
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
		_, judged := also[state.Nqn]
		if !judged && (!ok || parts.Kind != common.NqnKindXfer) {
			// Only a clone source can be the thing a dm-clone maps, and the
			// namespace node is read only for those: on a node holding a
			// leg per group that read would otherwise be one listing per
			// leg, per pass.
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
		devNo, err := s.blockDevNo(ctx, path.Base(devPath))
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

// requestedSrcNqns is the stored-request half of srcNqnsInUse: every clone
// source NQN a cntlr this process holds names.
func (s *CnAgentServer) requestedSrcNqns(
	clusterId uint64,
	cnId uint64,
) map[string]struct{} {
	inUse := make(map[string]struct{})
	for _, key := range s.cntlrKeysOf(clusterId, cnId) {
		st := s.getCntlr(key)
		if st == nil {
			continue
		}
		for _, clone := range st.loadReq().GetCloneList() {
			if nqn := clone.GetSrcNqn(); nqn != "" {
				inUse[nqn] = struct{}{}
			}
		}
	}
	return inUse
}

// unownedSrcNqns is every kind-4 connection this host holds for our cluster
// that srcNqnsInUse does not claim, with rd as srcNqnsInUse takes it: the
// clone sources every scope judges, by their NQN alone.
func (s *CnAgentServer) unownedSrcNqns(
	ctx context.Context,
	clusterId uint64,
	cnId uint64,
	actual *cnActual,
	rd *cnReading,
	res *agent.SweepResult,
) []string {
	inUse := s.srcNqnsInUse(ctx, clusterId, cnId, actual, rd, nil, res)
	return xferSrcsUnused(clusterId, actual, inUse)
}

// xferSrcsUnused is the transfer connections of our cluster that inUse does
// not hold, sorted.
func xferSrcsUnused(
	clusterId uint64,
	actual *cnActual,
	inUse map[string]struct{},
) []string {
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

// hostNqnJudged says whether the node-level pass judges a connection by its
// controllers' host NQN: one whose NQN is in no dnv namespace — another
// pool's subsystem, whose NQN its user chose — or is a transfer of another
// cluster. Every other dnv-format NQN is passed over with no read: a
// transfer of our cluster is judged by its NQN, a leg is its sp's chain's,
// and the cn opens no other dnv kind, so a migration source a co-hosted dn
// agent connects costs nothing; and one in the dnv namespace that decodes
// to nothing may be a newer build's after a rollback, which this build
// never touches (IsDnvNqn).
func hostNqnJudged(clusterId uint64, nqn string) bool {
	if parts, ok := common.ParseNqn(nqn); ok {
		return parts.Kind == common.NqnKindXfer && parts.Ids[0] != clusterId
	}
	return !common.IsDnvNqn(nqn)
}

// nodeSrcs is what the node-level pass judges of the clone sources: the
// transfer connections of our cluster no one uses (unownedSrcNqns' rule), and
// the other connections of this cn's whose clone has gone (foreign).
type nodeSrcs struct {
	xfers   []string
	foreign []string
}

// nodeUnownedSrcs is the node-level pass's whole judgment of the clone
// sources (cnagent.md CN21). Beside the transfer connections of our
// cluster, judged by their NQN as in every scope, it judges each connection
// hostNqnJudged names by its controllers' host NQN, the mark the kernel
// keeps of who opened a controller: one of whose controllers reads this
// cn's host NQN is a clone source of ours, since the cn opens nothing else
// with it but its legs, and is unowned when no held request names it as a
// clone's src_nqn and no live dm-clone maps its namespace device. A
// connection a held request names costs no read; every other one it judges
// costs a listing of its entry and a hostnqn read per controller up to the
// first that reads ours, in every node pass, the node's reads included:
// only a machine that is also a host, or that a clone of another pool or
// cluster reads through, holds such connections. A hostnqn read that did
// not answer leaves the connection alone for the pass and is named as a
// failure. A cntlr's pass judges none of these: the reads would come in
// every cntlr read, every round.
func (s *CnAgentServer) nodeUnownedSrcs(
	ctx context.Context,
	clusterId uint64,
	cnId uint64,
	actual *cnActual,
	res *agent.SweepResult,
) nodeSrcs {
	requested := s.requestedSrcNqns(clusterId, cnId)
	hostNqn := s.nf.CnHostNqn(clusterId, cnId)
	ownConns := make(map[string]struct{})
	for _, state := range actual.hostSubsys {
		if !hostNqnJudged(clusterId, state.Nqn) {
			continue
		}
		if _, named := requested[state.Nqn]; named {
			continue
		}
		held, err := s.host.HeldWithHostNqn(ctx, state, hostNqn)
		if err != nil {
			res.Fail("nvme host nqn of "+state.Nqn, err)
			continue
		}
		if held {
			ownConns[state.Nqn] = struct{}{}
		}
	}
	inUse := s.srcNqnsInUse(ctx, clusterId, cnId, actual, nil, ownConns, res)
	out := nodeSrcs{xfers: xferSrcsUnused(clusterId, actual, inUse)}
	for nqn := range ownConns {
		if _, used := inUse[nqn]; !used {
			out.foreign = append(out.foreign, nqn)
		}
	}
	sort.Strings(out.foreign)
	return out
}

// ---------------------------------------------------------------------------
// The two scopes
// ---------------------------------------------------------------------------

// sweepCntlr is the cntlr-level pass of CN21. It runs in convergeCntlr,
// under the CN1 locks of convergeCntlr's caller: the node read lock and this
// cntlr's object lock in SyncupCntlr, the node write lock in the startup
// Reconcile. It removes only objects attributed to this
// cntlr's sp — which is what lets two cntlrs of one CN converge concurrently.
//
// remove = false is the read-only VERDICT a cntlr's read takes (readCntlr):
// the same attribution and the same comparison, with nothing touched (CN23),
// over the listings of the read's reading (plan.rd, plan.walk; CN30). A
// converge passes no reading and lists the node itself (enumerateCn).
//
// putBack are the conditions the verdict names beside the leftovers: each
// md member, by dm name, the converge would fail early, put back, add or
// remove for a repair (putBackNames), and each broken or inactive array, by
// its node, the converge would assemble again (reassemblyNamed; CN30), each absent
// array, by its group's row name, the converge's assembly would start
// (absentArrayNamed), and, as failures, each forced start the converge
// would carry through, or the stop of an array it begins with
// (forcedStartNamed), each group whose md check the
// converge would start, stop or adopt (mdCheckDue), and each uncreated
// snapshot whose create message the converge would send (snapshotDue):
// what the call's
// probe read, so nothing here reads them again. A
// converge passes none: it has just tried to put them back, and its rows say
// how that went. The verdict also names each forced-start mark of a request
// that does not count as delivered (cntlrState.reqFromRpc; storedMarks;
// CN12, CN30).
//
// noCtrl are the sides of wanted legs, and the entries of the clones'
// src_tr_conf_list, with no controller that the converge would connect
// (sidesToConnect, srcToConnect; CN30), named as failures for the same
// reason: the kernel deletes a controller whose reconnect nvmet refused for
// good, and only a converge connects it again; and, marked cut, the sides
// whose live path the converge would cut (pathsToCut, CN10), which only a
// converge does either. A converge passes none.
func (s *CnAgentServer) sweepCntlr(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	remove bool,
	read *pb.CntlrInfo,
	putBack []verdictName,
	noCtrl []legSide,
) *agent.SweepResult {
	res := &agent.SweepResult{}
	actual := s.enumerateCn(
		ctx, plan.clusterId, plan.cnId, plan.rd, plan.walk, false, res)
	// converge is what the caller asked for; from here on, remove is what
	// the snapshot licenses.
	converge := remove
	// AN ENUMERATION THAT DID NOT ANSWER LICENSES NO REMOVAL. Everything
	// below is "actual minus desired", and with a listing unanswered its
	// part of `actual` is empty for want of an answer — which subtracts to
	// "remove nothing" in the safe direction but reads as "there is nothing
	// left" everywhere a REMOVAL is gated on absence: the layer stop rule
	// never fires, so lower layers run as though the ones above them had
	// succeeded (an unanswered md enumeration leaves L9 nothing to stop and
	// lets L10 disconnect unwanted legs under a live array), and the
	// live-device tests that protect a shared object (a dm-clone still
	// mapping its source) lose the half of their evidence that comes from
	// the node. res.Fail has already made the verdict non-OK, so the worker
	// re-drives; this pass reports and removes nothing from the node.
	if !actual.listed() {
		remove = false
	}
	wanted := cntlrWanted(plan)
	chain := s.buildChain(ctx, plan.clusterId, plan.cnId, plan.spId,
		actual, wanted, res)
	chain.srcNqns = func() []string {
		return s.unownedSrcNqns(
			ctx, plan.clusterId, plan.cnId, actual, plan.rd, res)
	}
	if converge {
		// P1 runs on every converge, a gated one too: it decides from the
		// plan alone and removes nothing, and the build phase after this sweep
		// parks on its strength whether or not the sweep ran — ensureNsDev
		// reloads a demoted or suspended namespace's ns-dev onto the td's
		// dm-error and ensureXfer demotes an unserved transfer device, and
		// neither moves an ANA group. Skipped here, such a namespace would
		// be served from an error table while its path still reads
		// optimized, until some later pass moved it; and a pass with nothing
		// left to remove has no leftover to make the worker drive one.
		s.cntlrAnaPreStep(ctx, plan)
	}
	// The build phase after the sweep must not act on the strength of a
	// removal that did not happen. While a dm-clone the plan does not want
	// is live, it may be hydrating into its destination raid0, and a host
	// writing that raid0 directly could have its write overwritten by a
	// region the clone had not copied yet, so the build puts no ns-dev onto
	// such a raid0 it is not already on (ensureNsDev, CN18). On a pass that
	// removes, the chain says which dm-clones may be left: all of them when
	// L3 did not run, the ones it did not verify gone otherwise. On a
	// stopped sweep no L3 runs: every one the dm listing names is left, and
	// when that listing did not answer, any may be. lingeringCloneHold
	// turns them into the raid0s they write into. The fact is this pass's,
	// read off this snapshot and this chain; the next pass learns its own.
	if remove {
		s.cntlrPreSteps(ctx, plan, actual, wanted)
		out := s.runChain(ctx, st, plan, chain, actual, wanted, res)
		plan.cloneHold = s.lingeringCloneHold(
			ctx, out.lingeringClones, actual)
		// CN14: a delete message that failed is not sent again by any later
		// pass, so this one sweeps that pool's ids, under the activation
		// sweep's own gate, and remembers nothing.
		if st.reqFromRpc && len(out.failedDeletes) > 0 {
			s.sweepAfterFailedDeletes(ctx, st, plan, actual, out)
		}
	} else {
		s.reportChain(chain, res)
	}
	if converge && !remove {
		plan.cloneHold = s.lingeringCloneHold(ctx, chain.clones, actual)
	}
	// The local store follows the same rule as the devices: a clone's chunk
	// files go when the clone leaves the request, and stay while it is only
	// suppressed. That is decided from the request alone, so an unanswered
	// listing gives it no reason to wait, and it runs on every converge. It
	// comes after the chain, so that on a pass that removes, the files go
	// with the rest of the clone's state (SH7). The verdict runs it too, to
	// name what a converge could not remove.
	s.sweepCloneChunks(ctx, st, res, converge)
	if converge {
		// The ResInfo histories follow the same rule as the devices:
		// whatever the desired state no longer names is forgotten, so a
		// later rebuild of the same id reports a fresh epoch rather than the
		// dead object's (SH14). From the request alone as well, so this too
		// runs on every converge.
		st.tracker.Keep(cntlrResKeys(plan))
	}
	for _, vn := range putBack {
		if vn.err != nil {
			res.Fail(vn.name, vn.err)
			continue
		}
		res.Add(vn.kind, vn.name)
	}
	for _, ls := range noCtrl {
		tr := ls.tr
		why := errNoController
		if ls.cut {
			why = errPathToCut
		}
		res.Fail(ls.nqn+" at "+tr.GetTrAddr()+":"+tr.GetTrSvcId(), why)
	}
	if !converge && !st.reqFromRpc {
		storedMarks(plan, res)
	}
	if !converge {
		// A converge names its rows once its build has made them, and logs
		// then (convergeCntlr).
		cntlrRowVerdict(plan, read, s.snapsLackingADevice(ctx, plan), true,
			res)
		res.Log(ctx, s.cntlrIdAttrs(plan)...)
	}
	return res
}

// cntlrRowVerdict is what a cntlr's verdict names beyond its sweep's
// leftovers: each row of the pass, of a dm device or nvmet object the
// cntlr's converge builds from devices of its own node, that is not as the
// desired state wants it while everything it is built on is (CN30;
// dnagent.md SH31). Both a converge and a read take it, each from the rows it
// just made, so nothing about a failed step is remembered and the two
// channels name the same node alike. The worker re-sends a Syncup* on a
// Check reply's code alone, never on its rows (dnv-worker.md RW4), so
// without the name a step that stopped once — a converge cut off by its
// call's deadline, an ANA move that failed, a resume that failed — would stay
// so with code zero until something else moved the cntlr's revision.
//
// The rows are the concats, thin pools, raid0s, the thin volume's dm-error
// and thin devices, ns-devs, namespaces, subsystems, and a transfer's
// dm-linear, subsystem and namespace; and the leg wrappers, the RedundNone
// groups' dm-linears and the dm-clones that the pass's own test of them
// names (legPlan.wrapperName, grpPlan.linearName, clonePlan.dmName). It
// names a row that reads
// RES_STATUS_MISSING with no reason, as "absent"; one that reads
// RES_STATUS_ERROR, or RES_STATUS_OK with RES_REASON_DIFFERS (a namespace
// whose ANA group differs, or a thin pool short of its metadata device or
// holding a metadata snapshot), as
// "differs", a subsystem whose nvmet connections could not be read, or that
// a lockdown the lockdown file shows keeps from being ended, among them,
// and a subsystem row that reads RES_STATUS_PENDING, a stray connection
// still there (CN33), as "pending", the first line of the row's
// details after it. It names a row only while every row it is built on
// reads RES_STATUS_OK (rowsUnder): a stack over a group whose array does not
// run waits for that array, which the verdict's md names cover (the absent
// array, a member to put back, a broken or inactive array, a forced
// start), and over a
// broken layer of its own only that
// layer is named, the lowest, whose converge builds the rest. It names no row
// the level suppresses (RES_REASON_SP_LEVEL), no RES_STATUS_PROVISIONING row,
// the stack a deferred build holds back on purpose, no thin row whose pool
// has lost its thin id (RES_REASON_THIN_ID_LOST) or holds a thin id the
// agent does not adopt for a new volume (RES_REASON_THIN_ID_NOT_ADOPTED), no
// pool row that reads needs_check (RES_REASON_POOL_NEEDS_CHECK) or failed
// (RES_REASON_POOL_FAILED), and no row of a transfer whose origin the
// request no longer names, none of which a converge cures, save a new volume
// refused because its header read failed: a later converge reads the
// headers again and can then build it, and a read names that volume absent
// (probeThin), so the re-send comes every second round at most. A
// subsystem row the cut leaves in error, whose nvmet connections cannot be
// read, or that a lockdown the lockdown file shows keeps from being ended
// (cutErrorRow, CN33), is a name only a read gives (onRead):
// a converge, which would only meet the same fact again, leaves it
// unnamed, so the round after it runs a read, which names it and counts
// it, and the worker's threshold reaction can fail the primary over.
// A transfer's dm-linear rests on its origin's raid0 only where the
// transfer is served (xferServed): elsewhere it is an error table, which
// rests on nothing. The rest of the rows of legs, groups and clones, and
// those of paths and arrays, rest on a remote end, and keep the rules the
// verdict has for them (sides and clone source endpoints to connect, md
// members to put back): a new syncup does not cure a disk node that is gone.
// A dm-clone's own test rests on its source, its clone meta group's row and
// its destination raid0's row (cloneDmGate). A nil info names nothing.
func cntlrRowVerdict(
	plan *cntlrPlan,
	info *pb.CntlrInfo,
	snapsWaiting map[uint64]bool,
	onRead bool,
	res *agent.SweepResult,
) {
	if info == nil {
		return
	}
	name := func(row *pb.ResInfo, under ...*pb.ResInfo) {
		if row != nil && rowsUnder(under) {
			nameCntlrRow(row, res)
		}
	}
	// A leg's wrapper, a RedundNone group's dm-linear and a dm-clone are
	// named by their own test, which the pass ran (wrapperName,
	// linearGroupName, cloneDmName), and not by their rows: those also carry
	// what a remote end does, the leg's prober or transport, the leg's
	// availability and the clone's source, and read neither the read-only
	// flag nor, for the wrapper and the dm-clone, the numbers their tables
	// map.
	for _, lp := range plan.legs {
		if lp.wrapperName != nil {
			res.Fail(lp.name, lp.wrapperName)
		}
	}
	for _, gp := range plan.grps {
		if gp.linearName != nil {
			res.Fail(gp.resName, gp.linearName)
		}
	}
	for _, cp := range plan.clones {
		if cp.dmName != nil {
			res.Fail(cp.finalName, cp.dmName)
		}
	}
	for _, sp := range plan.slices {
		var metaGrps, dataGrps []*pb.ResInfo
		for _, gp := range sp.effMetaGrps {
			metaGrps = append(metaGrps, info.GetGrpIdToMdRaid()[gp.grpId])
		}
		for _, gp := range sp.effDataGrps {
			dataGrps = append(dataGrps, info.GetGrpIdToMdRaid()[gp.grpId])
		}
		meta := info.GetSliceIdToMeta()[sp.sliceId]
		data := info.GetSliceIdToData()[sp.sliceId]
		name(meta, metaGrps...)
		name(data, dataGrps...)
		name(info.GetSliceIdToDmPool()[sp.sliceId], meta, data)
	}
	for _, tp := range plan.tds {
		// A snapshot not yet created that still lacks a device in a slice
		// is made by CN14's pre-pass, which suspends its origin; a re-send
		// on every round that its create fails would stall the origin's
		// host IO on every round. So its rows, its dm-error's among them,
		// are not named (snapsLackingADevice), and a read names the
		// snapshot itself while the pre-pass would send a message
		// (snapshotDue), a name only a read gives, which brings a converge
		// every second round at most (dnv-worker.md RW4).
		if snapsWaiting[tp.tdId] {
			continue
		}
		name(info.GetTdIdToDmError()[tp.tdId])
		thins := info.GetTdIdToThinInfo()[tp.tdId].GetSliceIdToDmThin()
		var under []*pb.ResInfo
		for _, sp := range plan.slices {
			name(thins[sp.sliceId], info.GetSliceIdToDmPool()[sp.sliceId])
			under = append(under, thins[sp.sliceId])
		}
		name(info.GetTdIdToRaid0()[tp.tdId], under...)
	}
	for _, ssp := range plan.subsystems {
		subsys := info.GetSsIdToSubsystem()[ssp.ssId]
		if onRead || !cutErrorRow(subsys) {
			name(subsys)
		}
		for _, np := range ssp.namespaces {
			nsDev := info.GetNsIdToDmLinear()[np.nsId]
			name(nsDev, nsDevUnder(np, info))
			name(info.GetNsIdToNamespace()[np.nsId], nsDev, subsys)
		}
	}
	for _, xp := range plan.xfers {
		// A transfer whose origin namespace or thin device the request no
		// longer names cannot be built by any converge of this request.
		if xp.ori == nil || xp.ori.td == nil {
			continue
		}
		// A transfer device rests on its origin's raid0 only where it maps
		// it (xferServed); elsewhere it is an error table, which rests on
		// nothing, at a level that suppresses the raid0 too.
		var under *pb.ResInfo
		if plan.xferServed(xp) {
			under = info.GetTdIdToRaid0()[xp.ori.td.tdId]
		}
		dm := info.GetXferIdToDmLinear()[xp.xferId]
		subsys := info.GetXferIdToSubsystem()[xp.xferId]
		name(dm, under)
		if onRead || !cutErrorRow(subsys) {
			name(subsys)
		}
		name(info.GetXferIdToNamespace()[xp.xferId], dm, subsys)
	}
}

// snapsLackingADevice lists the snapshot tds not yet created of which a
// slice that is not deferred has no thin device, or one whose read does not
// answer: a converge of them runs CN14's pre-pass, which suspends the
// origin's raid0 to make the missing ids (snapshotPrePass), so the cntlr's
// verdict leaves their rows out (cntlrRowVerdict), and a read names the
// snapshot alone, while the pre-pass would send a message (snapshotDue). A
// snapshot whose devices
// are all there makes no message and suspends no origin, so its rows are
// named like any other. Only a primary builds thin devices.
func (s *CnAgentServer) snapsLackingADevice(
	ctx context.Context,
	plan *cntlrPlan,
) map[uint64]bool {
	waiting := map[uint64]bool{}
	if !plan.primary {
		return waiting
	}
	for _, tp := range plan.tds {
		if tp.td.GetOriId() == 0 || tp.td.GetCreated() {
			continue
		}
		for _, sp := range plan.slices {
			if sp.deferred {
				continue
			}
			dev, err := s.dmInfo(ctx, plan.rd,
				plan.thinName(tp.tdId, sp.sliceId))
			if err != nil || dev == nil {
				waiting[tp.tdId] = true
				break
			}
		}
	}
	return waiting
}

// nsDevUnder is the row of what an ns-dev's backing is: the td's raid0, the
// td's dm-error of a park, or a clone's dm-clone; nil, which counts as
// there, for a backing that has no row.
func nsDevUnder(np *nsPlan, info *pb.CntlrInfo) *pb.ResInfo {
	switch {
	case np.td == nil:
		return nil
	case np.backingName == np.td.raid0Name:
		return info.GetTdIdToRaid0()[np.td.tdId]
	case np.backingName == np.td.errorName:
		return info.GetTdIdToDmError()[np.td.tdId]
	case np.clone != nil && np.backingName == np.clone.finalName:
		return info.GetCloneIdToDmClone()[np.clone.cloneId]
	}
	return nil
}

// rowsUnder reports whether every row in under reads RES_STATUS_OK; a nil
// row is one with no row of its own, which counts as there.
func rowsUnder(under []*pb.ResInfo) bool {
	for _, row := range under {
		if row != nil && row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			return false
		}
	}
	return true
}

// nameCntlrRow records row in res when cntlrRowVerdict names it: "absent",
// "differs" or "pending", with the first line of its details.
func nameCntlrRow(row *pb.ResInfo, res *agent.SweepResult) {
	var err error
	switch row.GetStatus() {
	case pb.ResStatus_RES_STATUS_MISSING:
		if row.GetReason() != pb.ResReason_RES_REASON_NONE {
			return
		}
		err = errRowAbsent
	case pb.ResStatus_RES_STATUS_ERROR:
		if row.GetReason() == pb.ResReason_RES_REASON_THIN_ID_LOST ||
			row.GetReason() == pb.ResReason_RES_REASON_THIN_ID_NOT_ADOPTED ||
			row.GetReason() == pb.ResReason_RES_REASON_POOL_NEEDS_CHECK ||
			row.GetReason() == pb.ResReason_RES_REASON_POOL_FAILED ||
			row.GetReason() == pb.ResReason_RES_REASON_POOL_META_DAMAGED ||
			row.GetDetails() == detailsPortMissing ||
			strings.HasPrefix(row.GetDetails(), detailsConcatNoShrink) {
			return
		}
		err = errRowDiffers
	case pb.ResStatus_RES_STATUS_OK:
		if row.GetReason() != pb.ResReason_RES_REASON_DIFFERS {
			return
		}
		err = errRowDiffers
	case pb.ResStatus_RES_STATUS_PENDING:
		// A subsystem with a stray connection still listed after the cut
		// or at a read (CN33), the one row the verdict reads that can be
		// pending: the re-send cuts again.
		err = errRowPending
	default:
		return
	}
	res.Fail(row.GetResName(), rowFailure(err, row.GetDetails()))
}

// rowFailure is the failure a cntlr's verdict names a row with: why, the
// first line of details after it where there is one.
func rowFailure(why error, details string) error {
	if first, _, _ := strings.Cut(details, "\n"); first != "" {
		return fmt.Errorf("%w: %s", why, first)
	}
	return why
}

// shapeFailure is the failure a cntlr's verdict names a single-target dm
// device of shape with, as nameCntlrRow names the row its probe writes
// (shapeRow): "absent" for one that is not there, and "differs" with the
// row's details otherwise.
func shapeFailure(shape dmShape, targetType string) error {
	if shape == dmShapeAbsent {
		return errRowAbsent
	}
	_, details := shapeRow(shape, targetType)
	return rowFailure(errRowDiffers, details)
}

// errRowAbsent, errRowDiffers and errRowPending are the failures
// cntlrRowVerdict records for the rows it names.
var (
	errRowAbsent  = errors.New("absent")
	errRowDiffers = errors.New("differs")
	errRowPending = errors.New("pending")
)

// sortedIds lists the keys of m in ascending order.
func sortedIds[V any](m map[uint64]V) []uint64 {
	ids := make([]uint64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// errNoController is the failure a cntlr's verdict records for a side of a
// wanted leg that the converge would connect (sweepCntlr); errPathToCut the
// one it records for a side whose path the converge would cut (pathsToCut).
var (
	errNoController = errors.New("no controller")
	errPathToCut    = errors.New("path to cut")
)

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
		for _, key := range pathResKeys(lp) {
			add(key)
		}
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

// cntlrAnaPreStep is P1, the first of the pre-steps. It is the one of them
// that runs on every converge, whether or not the snapshot licensed any
// removal: it decides from the plan alone and removes nothing, and the build
// phase parks on its strength (sweepCntlr).
func (s *CnAgentServer) cntlrAnaPreStep(ctx context.Context, plan *cntlrPlan) {
	// P1, ANA: every namespace the desired state wants inaccessible is moved
	// there before anything under it is touched (architecture.md, Failover,
	// old_primary step 1).
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
}

// cntlrPreSteps are the other two of those transitions, which this pass
// derives from the plan and the live tables instead of from a remembered
// plan. Both exist only so the layers below them can remove anything at all,
// so they run only on a pass whose snapshot licenses removal, after P1.
func (s *CnAgentServer) cntlrPreSteps(
	ctx context.Context,
	plan *cntlrPlan,
	actual *cnActual,
	wanted *cnWanted,
) {
	// P2, park: an ns-dev OF THE PLAN whose LIVE table still maps something
	// this pass is about to remove is reloaded onto its td's dm-error first,
	// which the park creates when it is missing. Of the plan, not of the
	// wanted set: below SP_LEVEL_DISABLE an ns-dev the desired state still
	// wants can map a device it no longer does — a raid0 at
	// SP_LEVEL_NO_THINPOOL, a dm-clone the level suppresses — and P0 parks
	// only unwanted ns-devs. The reload's own flushing suspend is what
	// completes the in-flight host IO, and without it the removal below
	// would fail EBUSY behind the ns-dev.
	//
	// Not at SP_LEVEL_DISABLE. The wanted set holds no ns-dev there, so P0
	// parks every one of them by its own live table; and the park target
	// would be an unwanted `c5` created after the sweep's enumeration, in no
	// chain — the pass would remove the ns-dev parked on it, reply clean, and
	// leave that device for the next round to find.
	//
	// The live table answers "does this ns-dev still map something unwanted?"
	// with no memory of a previous plan — and it is also right for a device
	// an interrupted pass left pointing somewhere the plan never described.
	//
	// The park of an ns-dev whose rules want a serving device — one still
	// mapping a dm-clone the plan dropped, say — moves its namespace
	// inaccessible first (parkNsDev), so its hosts queue rather than read IO
	// errors; the park of rules 0-4 moves nothing, since P1 has moved every
	// namespace the desired state wants inaccessible, and rules 3 and 4 keep
	// theirs optimized over the dm-error they select.
	for _, np := range plan.namespaces {
		if np.td == nil || !plan.wantAny {
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
		// A failure is logged inside, and the build's ensureXfer meets the
		// device again.
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
// td was deleted in the same request has no plan size left, and a previously
// applied plan is exactly the remembered state this design does without.
//
// It logs what fails and returns it, nil for a device that is not there or
// whose table sizes nothing: ensureXfer's demotion of a transfer whose
// origin is gone moves the transfer's ANA group only when the device is
// where CN17 wants it (xferAnaMove).
func (s *CnAgentServer) demoteXfer(ctx context.Context, xp *xferPlan) error {
	dev, err := s.dm.Info(ctx, xp.finalName)
	if err != nil || dev == nil {
		return err
	}
	sectors := xp.sectors
	if sectors == 0 {
		targets, err := s.dm.Table(ctx, xp.finalName)
		if err != nil {
			slog.ErrorContext(ctx, "reading a transfer device table failed",
				slog.String("dm", xp.finalName),
				slog.String("error", err.Error()))
			return err
		}
		for _, target := range targets {
			sectors += target.Length
		}
	}
	if sectors == 0 {
		return nil
	}
	if err := s.ensureDmError(ctx, xp.finalName, sectors); err != nil {
		slog.ErrorContext(ctx, "demoting a transfer device failed",
			slog.String("dm", xp.finalName),
			slog.String("error", err.Error()))
		return err
	}
	return nil
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
		chain.raid0s, chain.tdErrors, chain.thins, chain.pools,
		chain.poolMetas, chain.poolDatas, chain.grpDms, chain.legDms)
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
// this CN's list, plus the objects no sp can be read off at all. A removing
// pass (remove) runs under the node write lock, so no cntlr converge, no
// read and no push runs beside it — which is what makes judging an UNOWNED
// object safe; the read-only verdict, of a CN's read (refreshCn) or of a
// SyncupCn that only adds cntlrs (syncupCnAdds), runs under the node read
// lock and removes nothing. A
// disconnect it sets going (startDisconnect) can land after the lock is
// released; the connect steps' registry check (disconnectInFlight) keeps a
// converge from adopting that connection meanwhile.
//
// The sp-scoped chain of an sp that IS in the pointer list is never touched
// here, even when it has no cntlr file: CN8 introduces the pointer before the
// SyncupCntlr, and after a lost --local-store the resources exist and must be
// re-adopted probe-first by that syncup rather than swept. The steps for
// unowned subsystems and source connections are keyed on the stored requests
// and on what the kernel shows instead, so an unowned host-facing subsystem
// of such an sp, with no cntlr in memory to claim it, or an unmapped source,
// is removed (CN21). Those steps are this scope's alone: an old host-facing
// subsystem with no namespace device (nvmetOwner) and a clone source judged
// by its controllers' host NQN (nodeUnownedSrcs), disconnected controller
// by controller (disconnectOwnCtrlsVerified).
//
// redrive is what of the base state the caller's own probe read wrong in a
// way a SyncupCn's converge would cure (probeCn): a piece of it absent, or an
// ANA group in a state other than its fixed one. The read-only verdict of a
// CN's read (refreshCn) or of a SyncupCn that only adds cntlrs
// (syncupCnAdds) reports each as a failure, so the verdict is
// not clean and the worker re-sends the SyncupCn whose converge builds or
// rewrites it (CN30); nothing else would, since the worker re-syncs on a
// reply's code and never on its rows. The removing passes pass none: their
// converge has just tried to build the base state, and its rows say how that
// went.
func (s *CnAgentServer) sweepCn(
	ctx context.Context,
	st *cnState,
	remove bool,
	redrive []agent.BaseRedrive,
) *agent.SweepResult {
	res := &agent.SweepResult{}
	req := st.loadReq()
	clusterId := req.GetClusterId()
	cnId := req.GetCnId()
	actual := s.enumerateCn(ctx, clusterId, cnId, nil, nil, true, res)
	// AN ENUMERATION THAT DID NOT ANSWER LICENSES NO REMOVAL. Everything
	// below is "actual minus desired", and with a listing unanswered its
	// part of `actual` is empty for want of an answer — which subtracts to
	// "remove nothing" in the safe direction but reads as "there is nothing
	// left" everywhere a REMOVAL is gated on absence: the layer stop rule
	// never fires, so lower layers run as though the ones above them had
	// succeeded (an unanswered md enumeration leaves L9 nothing to stop and
	// lets L10 disconnect unwanted legs under a live array), and the
	// live-device tests that protect a shared object (a dm-clone still
	// mapping its source) lose the half of their evidence that comes from
	// the node. res.Fail has already made the verdict non-OK, so the worker
	// re-drives; this pass reports and touches nothing.
	if !actual.listed() {
		remove = false
	}

	known := make(map[uint64]struct{})
	for _, ptr := range req.GetCntlrPointerList() {
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
	srcs := s.nodeUnownedSrcs(ctx, clusterId, cnId, actual, res)
	for _, nqn := range srcs.xfers {
		if !remove {
			res.Add(agent.LeftoverKindNvme, nqn)
			continue
		}
		if !s.disconnectVerified(ctx, actual, nqn) {
			res.Add(agent.LeftoverKindNvme, nqn)
		}
	}
	for _, nqn := range srcs.foreign {
		if !remove {
			res.Add(agent.LeftoverKindNvme, nqn)
			continue
		}
		if !s.disconnectOwnCtrlsVerified(
			ctx, clusterId, cnId, actual, nqn) {
			res.Add(agent.LeftoverKindNvme, nqn)
		}
	}
	for _, piece := range redrive {
		res.Fail(piece.What, piece.Err)
	}
	res.Log(ctx,
		slog.Uint64("cluster_id", clusterId),
		slog.Uint64("cn_id", cnId))
	return res
}
