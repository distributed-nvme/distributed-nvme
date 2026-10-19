package gateway

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is the gateway's half of architecture.md, Per-operation
// allocation: the per-operation candidate compositions that run OUTSIDE every
// STM (model.FindDnCandidatesAntiAffine / FindCnCandidatesAntiAffine +
// PickRandom), the in-STM re-validation of a pick
// that makes a scan outside a transaction safe (GW9), and the DN/CN
// bookkeeping ledgers that keep "one write and at most one revision bump per
// node per STM" (architecture.md, Revision keys and the sync fan-out) true
// however many sides, cntlrs or clones one transaction touches. A node whose
// only change is its used metadata units is written and re-indexed but not
// bumped (architecture.md, Metadata units).

// ---------------------------------------------------------------------------
// Candidate scans (architecture.md, Per-operation allocation)
// ---------------------------------------------------------------------------

// legCntOf is the number of legs one group of this SP has: RedundMdRaid1 two,
// RedundNone one (architecture.md, Per-operation allocation).
//
// The md-raid1 arm is common.MaxAllocLegPerGrp and not a literal 2 (SPD1):
// this is where the allocator's widest group shape is chosen, and the sp-drain
// batch budget is tripwired against that constant, so a wider shape has to
// move the constant and fails the budget test instead of a deployment.
func legCntOf(bdev *pb.BdevConf) int {
	if bdev.GetRedundConf().GetRedundMdRaid1() != nil {
		return common.MaxAllocLegPerGrp
	}
	return 1
}

// isMdRaid1 reports whether this SP's groups are md-raid1 pairs.
func isMdRaid1(bdev *pb.BdevConf) bool {
	return bdev.GetRedundConf().GetRedundMdRaid1() != nil
}

// dnPickPlan is one group's DN allocation request: how many extents each leg
// needs, how many legs the group has, the failure domains tier 1 of the
// scan of architecture.md, Per-operation allocation, keeps out, and the free
// metadata units each candidate's metadata area must still have. ExcludeLocs
// is empty for the DN scans that section leaves on
// the plain scan (CreateStoragePool's, GrowSlice's and CreateClone's). MetaUnits is zero for
// every scan but CreateMigration's, whose one destination holds a dm-clone
// run in its node's metadata area (architecture.md, Metadata units).
type dnPickPlan struct {
	ExtCnt      uint64
	Legs        int
	ExcludeLocs []string
	MetaUnits   uint64
}

// pickDns draws Legs distinct DNs for one group (architecture.md,
// Per-operation allocation): scan dn_batch_size × Legs candidates with at
// least ExtCnt free extents each, then pick Legs of them at random.
//
// black is the growing exclusion list — the request's NodeSelector black list
// plus every DN already picked in this operation — which is what puts every
// leg of one group (and, for CreateStoragePool, every leg of the whole SP) on
// a distinct DN. plan.ExcludeLocs is the two-tier half of the same rule: tier
// 1 keeps the candidates out of the group's existing failure domains and tier
// 2 rescans without that exclusion when tier 1 yields fewer than plan.Legs —
// the DNs this group must actually place, never the oversampled scan width —
// so a cluster with too few domains still places rather than refusing. Too few
// candidates is RESOURCE_EXHAUSTED, never a partial allocation.
//
// plan.MetaUnits makes the scan skip every DN whose capacity value leaves it
// fewer free metadata units (architecture.md, Metadata units). When that skip
// is what left the pick short, the refusal says so: the same scan run again
// without the units would have found enough DNs, so the shortfall is room in
// the metadata areas and not extents, and the message names "metadata room".
// The second scan runs only on that refusal path. Only CreateMigration asks
// for units, for its one destination, so a short pick there means no DN at
// all.
func pickDns(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	cc *pb.ClusterConf,
	plan dnPickPlan,
	selector *pb.NodeSelector,
	black []string,
	what string,
) ([]model.Cand, error) {
	// The cluster's stored batch size (architecture.md, Common validation). A
	// zero would silently make the scan width zero and turn every allocation
	// into RESOURCE_EXHAUSTED, so the stored conf is checked rather than
	// defaulted.
	if err := model.ValidateClusterConf(cc); err != nil {
		return nil, errAborted("%v", err)
	}
	batch := int(cc.GetAllocConf().GetDnBatchSize())
	scanBlack := append(
		append([]string(nil), selector.GetBlackList()...), black...)
	// The tier bool is deliberately dropped: a tier-2 placement is visible in
	// the stored topology, and gateway.md, Log records (LG1 to LG3), gains no
	// record for it.
	scan := func(metaUnits uint64) ([]model.Cand, error) {
		cands, _, err := model.FindDnCandidatesAntiAffine(
			ctx, cli, cid, cc,
			plan.ExtCnt,
			metaUnits,
			plan.Legs*batch,
			plan.Legs,
			scanBlack,
			selector.GetWhiteList(),
			plan.ExcludeLocs,
		)
		if err != nil {
			return nil, errAborted("%v", err)
		}
		return cands, nil
	}
	cands, err := scan(plan.MetaUnits)
	if err != nil {
		return nil, err
	}
	picks := model.PickRandom(cands, plan.Legs)
	if len(picks) < plan.Legs {
		if plan.MetaUnits > 0 {
			byExtents, err := scan(0)
			if err != nil {
				return nil, err
			}
			if len(byExtents) >= plan.Legs {
				return nil, errExhausted(
					"%s: no disk node with %d free extents has metadata "+
						"room for %d units",
					what, plan.ExtCnt, plan.MetaUnits)
			}
		}
		return nil, errExhausted(
			"%s: %d disk nodes with %d free extents, need %d",
			what, len(picks), plan.ExtCnt, plan.Legs)
	}
	return picks, nil
}

// pickCn draws exactly one CN with extCnt free extents and metaUnits free
// metadata units (architecture.md, Per-operation allocation; Metadata units).
// spCnAddrs names the CNs already hosting a cntlr
// of this SP, which model excludes so that two cntlrs of one SP never share a
// CN (architecture.md, Finding CN candidates). excludeLocs is the
// two-tier half of the rule: tier 1 keeps the pick out of the locations the
// SP's other cntlrs occupy, and tier 2 drops that exclusion when tier 1 finds
// no CN, so too few failure domains alone never make it refuse.
//
// metaUnits is the SP's clone units for CreateCntlr, whose new cntlr's CN is
// charged every clone of the SP since the cntlr may become the primary and
// build them there, and zero for CreateStoragePool, whose new SP has no
// clone. An empty pick that the units' skip caused answers the "metadata
// room" refusal, as in pickDns.
func pickCn(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	cc *pb.ClusterConf,
	extCnt uint64,
	metaUnits uint64,
	selector *pb.NodeSelector,
	black []string,
	spCnAddrs []string,
	excludeLocs []string,
	what string,
) (model.Cand, error) {
	// As in pickDns: the stored batch size, validated rather than defaulted.
	if err := model.ValidateClusterConf(cc); err != nil {
		return model.Cand{}, errAborted("%v", err)
	}
	batch := int(cc.GetAllocConf().GetCnBatchSize())
	scanBlack := append(
		append([]string(nil), selector.GetBlackList()...), black...)
	// The tier bool is dropped, as in pickDns.
	scan := func(units uint64) ([]model.Cand, error) {
		cands, _, err := model.FindCnCandidatesAntiAffine(
			ctx, cli, cid,
			extCnt,
			units,
			batch,
			scanBlack,
			selector.GetWhiteList(),
			spCnAddrs,
			excludeLocs,
		)
		if err != nil {
			return nil, errAborted("%v", err)
		}
		return cands, nil
	}
	cands, err := scan(metaUnits)
	if err != nil {
		return model.Cand{}, err
	}
	picks := model.PickRandom(cands, 1)
	if len(picks) < 1 {
		if metaUnits > 0 {
			byExtents, err := scan(0)
			if err != nil {
				return model.Cand{}, err
			}
			if len(byExtents) > 0 {
				return model.Cand{}, errExhausted(
					"%s: no controller node with %d free extents has "+
						"metadata room for %d units",
					what, extCnt, metaUnits)
			}
		}
		return model.Cand{}, errExhausted(
			"%s: no controller node with %d free extents", what, extCnt)
	}
	return picks[0], nil
}

// candAddrs is the addr_port list of a pick set, for the growing black list.
func candAddrs(cands []model.Cand) []string {
	addrs := make([]string, 0, len(cands))
	for _, cand := range cands {
		addrs = append(addrs, cand.AddrPort)
	}
	return addrs
}

// ---------------------------------------------------------------------------
// The DN ledger
// ---------------------------------------------------------------------------

// dnLedger accumulates one STM's DN bookkeeping so that a DN this transaction
// touches more than once — a pick verified and then charged, or a side list
// that names it twice — is read once, written once, has its capacity key
// maintained once and its revision bumped at most once (architecture.md,
// Revision keys and the sync fan-out; architecture.md, Capacity index keys).
//
// Every record it hands out is the one THIS transaction read, which is what
// makes model.MaintainDnCapacity's delete target exact: a capacity key embeds
// free_ext_cnt, so it can only be removed by the transaction that still knows
// the count it was written with.
//
// bump holds the DNs a side was charged to or released from: a DnRev bump is
// how such a change reaches the dn agent. A DN whose only change is its used
// metadata units (chargeUnits, releaseUnits) is written and re-indexed, its
// capacity value carrying the new count, but not bumped: nothing the node is
// sent depends on the count (architecture.md, Metadata units).
type dnLedger struct {
	s     etcdutil.STM
	cid   uint64
	cc    *pb.ClusterConf
	old   map[string]*pb.DnConf
	cur   map[string]*pb.DnConf
	bump  map[string]bool
	order []string
}

// newDnLedger opens a ledger over the cluster's STORED conf, or refuses that
// conf (architecture.md, Common validation; GW11).
//
// The gate sits in the constructor rather than in each handler because every
// DN this ledger writes goes through model.MaintainDnCapacity, which shifts
// dn_bin_conf as stored. A capacity key embeds the bin index it was written
// under, and that index comes from the ladder: computed from a conf
// CreateCluster could not have written, it is a different index, so the key a
// release means to delete survives — still indexing a free count the node no
// longer has — and the one it writes lands under a bin its free count does not
// belong to. There is no right index to compute from such a conf, and guessing
// one is the resolve-at-read this rule removed (architecture.md, Common
// validation), so the ledger refuses with the same errAborted the allocating
// paths already give (architecture.md, UNEXPECTED_ERROR → `ABORTED`).
//
// Six handlers build a ledger — CreateStoragePool, CreateClone,
// DeleteSpareLeg, and CreateMigration, FinishMigration and CancelMigration —
// and each builds it before staging its first write, so a refusal here
// returns having written nothing rather than relying on the transaction being
// abandoned (EU4). DeleteStoragePool builds none: it latches, and the
// worker's drain returns the extents (dnv-worker.md, The sp drain); nor does
// DeleteClone, whose drain gives the clone meta group back (dnv-worker.md,
// The clone drain). Three of the six reach a conf gate before this one
// anyway (CreateStoragePool's own, CreateClone's in planCloneGrp,
// CreateMigration's in its plan, through migrMetaUnitsOf and pickDns); the
// other three have none, which is what this covers.
func newDnLedger(
	s etcdutil.STM,
	cid uint64,
	cc *pb.ClusterConf,
) (*dnLedger, error) {
	if err := model.ValidateClusterConf(cc); err != nil {
		return nil, errAborted("%v", err)
	}
	return &dnLedger{
		s:    s,
		cid:  cid,
		cc:   cc,
		old:  make(map[string]*pb.DnConf),
		cur:  make(map[string]*pb.DnConf),
		bump: make(map[string]bool),
	}, nil
}

// tryGet reads one DN, caching both the record as stored and the copy being
// mutated, and reports whether it exists. Callers that address a DN through a
// stored Side use get, which turns absence into ABORTED; verifyPick uses
// this form, because for a PICK absence is not an error at all — it is a
// candidate that moved (GW9).
func (l *dnLedger) tryGet(addrPort string) (*pb.DnConf, bool) {
	if cur, ok := l.cur[addrPort]; ok {
		return cur, true
	}
	dn := &pb.DnConf{}
	if !l.s.Get(model.DnConfKey(l.cid, addrPort), dn) {
		return nil, false
	}
	l.old[addrPort] = dn
	l.cur[addrPort] = proto.Clone(dn).(*pb.DnConf)
	l.order = append(l.order, addrPort)
	return l.cur[addrPort], true
}

// get is tryGet with absence made an error. Every addr_port it is asked about
// comes out of a stored Side, so its record must exist; a missing one is a
// broken invariant, not a race.
func (l *dnLedger) get(addrPort string) (*pb.DnConf, error) {
	dn, found := l.tryGet(addrPort)
	if !found {
		// GW7: NOT_FOUND is for an object the REQUEST named; a DN named only
		// by a stored Side is a lost invariant key — ABORTED (architecture.md,
		// UNEXPECTED_ERROR → `ABORTED`).
		return nil, errAborted("dn_conf for %q is missing", addrPort)
	}
	return dn, nil
}

// verifyPick is GW9's exact test on one allocator pick: the DN still exists
// and is allocatable, it still has room for extCnt extents, the capacity key
// the scan saw is still there, and the DN's metadata area still has metaUnits
// free units. The key embeds free_ext_cnt, so its presence proves the DN's
// free extents have not moved since the scan. It proves nothing about the
// units: a write that changes only a node's used units rewrites the key's
// value where it is, so the units are read from the record this transaction
// holds (model.DnFreeMetaUnits; architecture.md, Metadata units). Any failure
// is errCandidateChanged and the caller re-runs the whole unit, whose next
// scan skips a DN that has filled up.
func (l *dnLedger) verifyPick(
	cand model.Cand,
	extCnt uint64,
	metaUnits uint64,
) (*pb.DnConf, error) {
	// A DN that was DELETED between the scan and this transaction is a moved
	// candidate like any other, not a NOT_FOUND for the client: the request
	// named no disk node, the allocator picked this one, and GW9 says the
	// answer to a pick that is gone is "re-scan", never a refusal.
	dn, found := l.tryGet(cand.AddrPort)
	if !found {
		return nil, errCandidateChanged
	}
	if !model.DnAllocatable(dn, l.cc.GetDnBinConf()) {
		return nil, errCandidateChanged
	}
	if dn.GetFreeExtCnt() < extCnt {
		return nil, errCandidateChanged
	}
	capKey := model.DnCapacityKey(
		l.cid, cand.BinIdx, cand.FreeExt, cand.AddrPort)
	if !l.s.Get(capKey, &pb.DnCapacity{}) {
		return nil, errCandidateChanged
	}
	if model.DnFreeMetaUnits(dn) < metaUnits {
		return nil, errCandidateChanged
	}
	return dn, nil
}

// charge is the DN bookkeeping of one side allocation (architecture.md,
// Capacity index keys; architecture.md, Storage pools): the side
// pointer goes in and extCnt leaves the budget.
func (l *dnLedger) charge(
	addrPort string,
	ptr *pb.SidePointer,
	extCnt uint64,
) error {
	dn, err := l.get(addrPort)
	if err != nil {
		return err
	}
	if dn.GetFreeExtCnt() < extCnt {
		return errExhausted(
			"disk node %q has %d free extents, need %d",
			addrPort, dn.GetFreeExtCnt(), extCnt)
	}
	dn.SidePtrList = append(dn.SidePtrList, ptr)
	dn.FreeExtCnt -= extCnt
	l.bump[addrPort] = true
	return nil
}

// release is charge's reverse: the pointer goes out and extCnt returns.
func (l *dnLedger) release(
	addrPort string,
	spId uint64,
	sideId uint64,
	extCnt uint64,
) error {
	dn, err := l.get(addrPort)
	if err != nil {
		return err
	}
	kept := make([]*pb.SidePointer, 0, len(dn.GetSidePtrList()))
	for _, ptr := range dn.GetSidePtrList() {
		if ptr.GetSpId() == spId && ptr.GetSideId() == sideId {
			continue
		}
		kept = append(kept, ptr)
	}
	dn.SidePtrList = kept
	dn.FreeExtCnt += extCnt
	l.bump[addrPort] = true
	return nil
}

// chargeUnits is the DN bookkeeping of one migration destination's dm-clone
// run (architecture.md, Metadata units): units more of the DN's metadata area
// are used. A DN whose area lacks the room is RESOURCE_EXHAUSTED, the message
// led by what; CreateMigration's verifyPick answers the same shortfall as
// candidate changed first, so this refusal is defensive, as charge's is. It
// marks no revision bump, and zero units read and write nothing.
func (l *dnLedger) chargeUnits(
	addrPort string,
	units uint64,
	what string,
) error {
	if units == 0 {
		return nil
	}
	dn, err := l.get(addrPort)
	if err != nil {
		return err
	}
	if free := model.DnFreeMetaUnits(dn); free < units {
		return errExhausted(
			"%s: disk node %q has no metadata room: "+
				"%d free metadata units, need %d",
			what, addrPort, free, units)
	}
	dn.UsedMetaUnits += units
	return nil
}

// releaseUnits is chargeUnits' reverse (architecture.md, Metadata units): the
// DN's used units drop by units and stop at zero (model.ReleaseMetaUnits), so
// a count that is already short never wraps around into one that refuses
// every later charge. Like chargeUnits it marks no revision bump, and zero
// units read and write nothing: a Migration whose record carries no
// meta_units leaves its destination DN exactly as it was.
func (l *dnLedger) releaseUnits(addrPort string, units uint64) error {
	if units == 0 {
		return nil
	}
	dn, err := l.get(addrPort)
	if err != nil {
		return err
	}
	dn.UsedMetaUnits = model.ReleaseMetaUnits(dn.GetUsedMetaUnits(), units)
	return nil
}

// flush writes every touched DN once and maintains its capacity key per
// architecture.md, Capacity index keys, the value carrying the DN's used
// metadata units, and bumps the revision of every DN a side was charged to or
// released from, once (architecture.md, Revision keys and the sync fan-out).
// A DN whose only change is its used units is written and re-indexed but not
// bumped (architecture.md, Metadata units). Nodes are flushed in first-touch
// order so one transaction's writes are deterministic.
func (l *dnLedger) flush(op string) error {
	for _, addrPort := range l.order {
		cur := l.cur[addrPort]
		l.s.Put(model.DnConfKey(l.cid, addrPort), cur)
		model.MaintainDnCapacity(
			l.s, l.cid, addrPort, l.cc, l.old[addrPort], cur)
		if !l.bump[addrPort] {
			continue
		}
		if err := bumpDn(l.s, op, l.cid, cur); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// The CN ledger
// ---------------------------------------------------------------------------

// cnLedger is dnLedger's CN twin: one read, one write, one capacity-key
// maintenance and at most one CnRev bump per CN per transaction
// (architecture.md, Revision keys and the sync fan-out; architecture.md,
// Capacity index keys). Its bump set holds the CNs a cntlr was charged to or
// released from; a CN whose only change is its used metadata units — every
// CN CreateClone charges — is written and re-indexed but not bumped
// (architecture.md, Metadata units). It takes no ClusterConf — CN capacity
// keys carry no bin index (architecture.md, Finding CN candidates).
type cnLedger struct {
	s     etcdutil.STM
	cid   uint64
	old   map[string]*pb.CnConf
	cur   map[string]*pb.CnConf
	bump  map[string]bool
	order []string
}

func newCnLedger(s etcdutil.STM, cid uint64) *cnLedger {
	return &cnLedger{
		s:    s,
		cid:  cid,
		old:  make(map[string]*pb.CnConf),
		cur:  make(map[string]*pb.CnConf),
		bump: make(map[string]bool),
	}
}

// tryGet is dnLedger.tryGet's CN twin: absence is reported, not raised.
func (l *cnLedger) tryGet(addrPort string) (*pb.CnConf, bool) {
	if cur, ok := l.cur[addrPort]; ok {
		return cur, true
	}
	cn := &pb.CnConf{}
	if !l.s.Get(model.CnConfKey(l.cid, addrPort), cn) {
		return nil, false
	}
	l.old[addrPort] = cn
	l.cur[addrPort] = proto.Clone(cn).(*pb.CnConf)
	l.order = append(l.order, addrPort)
	return l.cur[addrPort], true
}

// get is dnLedger.get's CN twin, with the same GW7 reading of an absence.
func (l *cnLedger) get(addrPort string) (*pb.CnConf, error) {
	cn, found := l.tryGet(addrPort)
	if !found {
		// GW7: a CN named only by a stored Cntlr is not an object the request
		// named, so its absence is a lost invariant key — ABORTED
		// (architecture.md, UNEXPECTED_ERROR → `ABORTED`).
		return nil, errAborted("cn_conf for %q is missing", addrPort)
	}
	return cn, nil
}

// verifyPick is GW9 for a CN pick. CN capacity keys carry no bin field, so the
// key is rebuilt from the free count the scan saw. As in dnLedger.verifyPick,
// the key proves the free extents and nothing about the units, so the
// metaUnits a cntlr's CN must hold for the SP's clones are checked on the
// record (model.CnFreeMetaUnits; architecture.md, Metadata units).
func (l *cnLedger) verifyPick(
	cand model.Cand,
	extCnt uint64,
	metaUnits uint64,
) (*pb.CnConf, error) {
	// Deleted between the scan and here is a moved candidate, not a refusal
	// (GW9) — see dnLedger.verifyPick.
	cn, found := l.tryGet(cand.AddrPort)
	if !found {
		return nil, errCandidateChanged
	}
	if !model.CnAllocatable(cn) {
		return nil, errCandidateChanged
	}
	if cn.GetFreeExtCnt() < extCnt {
		return nil, errCandidateChanged
	}
	capKey := model.CnCapacityKey(l.cid, cand.FreeExt, cand.AddrPort)
	if !l.s.Get(capKey, &pb.CnCapacity{}) {
		return nil, errCandidateChanged
	}
	if model.CnFreeMetaUnits(cn) < metaUnits {
		return nil, errCandidateChanged
	}
	return cn, nil
}

// charge reserves the SP's footprint on one CN and records the cntlr pointer.
func (l *cnLedger) charge(
	addrPort string,
	ptr *pb.CntlrPointer,
	extCnt uint64,
) error {
	cn, err := l.get(addrPort)
	if err != nil {
		return err
	}
	if cn.GetFreeExtCnt() < extCnt {
		return errExhausted(
			"controller node %q has %d free extents, need %d",
			addrPort, cn.GetFreeExtCnt(), extCnt)
	}
	cn.CntlrPtrList = append(cn.CntlrPtrList, ptr)
	cn.FreeExtCnt -= extCnt
	l.bump[addrPort] = true
	return nil
}

// release is charge's reverse.
func (l *cnLedger) release(
	addrPort string,
	spId uint64,
	cntlrId uint64,
	extCnt uint64,
) error {
	cn, err := l.get(addrPort)
	if err != nil {
		return err
	}
	kept := make([]*pb.CntlrPointer, 0, len(cn.GetCntlrPtrList()))
	for _, ptr := range cn.GetCntlrPtrList() {
		if ptr.GetSpId() == spId && ptr.GetCntlrId() == cntlrId {
			continue
		}
		kept = append(kept, ptr)
	}
	cn.CntlrPtrList = kept
	cn.FreeExtCnt += extCnt
	l.bump[addrPort] = true
	return nil
}

// chargeUnits is dnLedger.chargeUnits' CN twin (architecture.md, Metadata
// units): units more of the CN's metadata area are used, for a clone's run
// (CreateClone, once per cntlr of the SP) or for every clone of the SP
// (CreateCntlr). A CN whose area lacks the room is RESOURCE_EXHAUSTED, the
// message led by what. It marks no revision bump, and zero units read and
// write nothing.
func (l *cnLedger) chargeUnits(
	addrPort string,
	units uint64,
	what string,
) error {
	if units == 0 {
		return nil
	}
	cn, err := l.get(addrPort)
	if err != nil {
		return err
	}
	if free := model.CnFreeMetaUnits(cn); free < units {
		return errExhausted(
			"%s: controller node %q has no metadata room: "+
				"%d free metadata units, need %d",
			what, addrPort, free, units)
	}
	cn.UsedMetaUnits += units
	return nil
}

// releaseUnits is chargeUnits' reverse, dnLedger.releaseUnits' CN twin: the
// CN's used units drop by units and stop at zero (model.ReleaseMetaUnits),
// with no revision bump marked, and zero units read and write nothing.
func (l *cnLedger) releaseUnits(addrPort string, units uint64) error {
	if units == 0 {
		return nil
	}
	cn, err := l.get(addrPort)
	if err != nil {
		return err
	}
	cn.UsedMetaUnits = model.ReleaseMetaUnits(cn.GetUsedMetaUnits(), units)
	return nil
}

// flush writes every touched CN once and maintains its capacity key, the value
// carrying the CN's used metadata units, and bumps the revision of every CN a
// cntlr was charged to or released from, once. A CN whose only change is its
// used units is written and re-indexed but not bumped (architecture.md,
// Metadata units).
func (l *cnLedger) flush(op string) error {
	for _, addrPort := range l.order {
		cur := l.cur[addrPort]
		l.s.Put(model.CnConfKey(l.cid, addrPort), cur)
		model.MaintainCnCapacity(l.s, l.cid, addrPort, l.old[addrPort], cur)
		if !l.bump[addrPort] {
			continue
		}
		if err := bumpCn(l.s, op, l.cid, cur); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// SP geometry helpers
// ---------------------------------------------------------------------------

// loadSlices reads every slice the SpConf lists, in list order, inside the
// caller's STM. A listed slice that does not exist is an ABORTED
// (architecture.md, UNEXPECTED_ERROR → `ABORTED`): an SP
// whose slice list points at nothing has lost an invariant key, and a
// footprint computed from a partial list would undercharge a node.
func loadSlices(
	s etcdutil.STM,
	cid uint64,
	conf *pb.SpConf,
) ([]*pb.Slice, error) {
	slices := make([]*pb.Slice, 0, len(conf.GetSliceIdList()))
	for _, sliceId := range conf.GetSliceIdList() {
		key := model.SliceKey(cid, conf.GetSpId(), sliceId)
		slice := &pb.Slice{}
		if !s.Get(key, slice) {
			return nil, errAborted("slice key %q is missing", key)
		}
		slices = append(slices, slice)
	}
	return slices, nil
}

// loadCntlrs reads every cntlr the SpConf lists, in list order.
func loadCntlrs(
	s etcdutil.STM,
	cid uint64,
	conf *pb.SpConf,
) ([]*pb.Cntlr, error) {
	cntlrs := make([]*pb.Cntlr, 0, len(conf.GetCntlrIdList()))
	for _, cntlrId := range conf.GetCntlrIdList() {
		key := model.CntlrKey(cid, conf.GetSpId(), cntlrId)
		cntlr := &pb.Cntlr{}
		if !s.Get(key, cntlr) {
			return nil, errAborted("cntlr key %q is missing", key)
		}
		cntlrs = append(cntlrs, cntlr)
	}
	return cntlrs, nil
}

// loadClones reads every clone the SpConf lists, in list order, latched ones
// included. A listed clone whose key is missing is an ABORTED
// (architecture.md, UNEXPECTED_ERROR → `ABORTED`), as a listed slice's is in
// loadSlices: a unit sum computed from a partial list would charge a node
// less, or give it back less, than the SP's clones hold there, and a walk of
// the clones' clone meta groups over a partial list would miss a group, a
// leg or a side the pool holds.
func loadClones(
	s etcdutil.STM,
	cid uint64,
	conf *pb.SpConf,
) ([]*pb.Clone, error) {
	clones := make([]*pb.Clone, 0, len(conf.GetCloneNameList()))
	for _, name := range conf.GetCloneNameList() {
		key := model.CloneKey(cid, conf.GetSpId(), name)
		clone := &pb.Clone{}
		if !s.Get(key, clone) {
			return nil, errAborted("clone key %q is missing", key)
		}
		clones = append(clones, clone)
	}
	return clones, nil
}

// allGroups is every group of a slice, meta groups first — the order every
// walk over a slice's groups uses.
func allGroups(slice *pb.Slice) []*pb.Group {
	grps := make([]*pb.Group, 0,
		len(slice.GetMetaGrpList())+len(slice.GetDataGrpList()))
	grps = append(grps, slice.GetMetaGrpList()...)
	grps = append(grps, slice.GetDataGrpList()...)
	return grps
}

// allLegs is every leg of a group, active legs first and spare legs after —
// both carry sides that occupy a DN (architecture.md, Spare legs).
func allLegs(grp *pb.Group) []*pb.Leg {
	legs := make([]*pb.Leg, 0,
		len(grp.GetLegList())+len(grp.GetSpareLegList()))
	legs = append(legs, grp.GetLegList()...)
	legs = append(legs, grp.GetSpareLegList()...)
	return legs
}

// spFootprint is Σ ext_cnt over ALL groups of ALL slices — meta and data
// alike — which is what one cntlr's CN reserves for the SP (architecture.md,
// Storage pools; architecture.md, Cntlrs).
func spFootprint(slices []*pb.Slice) uint64 {
	total := uint64(0)
	for _, slice := range slices {
		for _, grp := range allGroups(slice) {
			total += grp.GetExtCnt()
		}
	}
	return total
}

// spCloneMetaUnits is Σ meta_units over EVERY clone of the SP, latched ones
// included: what the CN of each cntlr of the SP holds in its metadata area for
// the SP's clones, and so what CreateCntlr charges the new cntlr's CN and
// DeleteCntlr gives the old one back (architecture.md, Metadata units). A
// latched clone counts until model.FinishCloneDelete releases its units on
// the CN of every cntlr the SP has at that moment: a sum that left it out
// would give that release units a new cntlr's CN was never charged, and
// strand on a deleted cntlr's CN units nothing gives back. It is the gateway
// twin of model's sum, as spFootprint is, with the gateway's own refusal of a
// missing clone in loadClones.
func spCloneMetaUnits(clones []*pb.Clone) uint64 {
	total := uint64(0)
	for _, clone := range clones {
		total += clone.GetMetaUnits()
	}
	return total
}

// grpDnAddrs is every DN a group already occupies through an active leg or a
// spare leg: the black-list seed of CreateMigration and CreateSpareLeg
// (architecture.md, Per-operation allocation), and the plan each one's
// deciding transaction checks the group against for a DN it gained since.
func grpDnAddrs(grp *pb.Group) []string {
	var addrs []string
	for _, leg := range allLegs(grp) {
		for _, side := range leg.GetSideList() {
			addrs = append(addrs, side.GetAddrPort())
		}
	}
	return addrs
}

// grpDnLocations is the failure domain of every DN grpDnAddrs names: the tier-1
// exclusion of CreateMigration and CreateSpareLeg (architecture.md,
// Per-operation allocation), which is about LOCATIONS and not merely about the
// DNs the black list already holds.
//
// One plain DnConf read per DISTINCT addr_port, outside every STM like the
// capacity scan it feeds. Reading them before the transaction is sound because
// `location` is immutable — CreateDiskNode defaults it to addr_port and
// UpdateDiskNodeDisabled is the only later DN mutator (architecture.md, Disk
// nodes) — so a location read here cannot have gone stale by the time the op
// re-validates the pick, which is also why that re-validation stays
// address-based. A DN whose conf is gone contributes no location: it is
// black-listed by address anyway, and inventing one would exclude a domain
// nothing occupies.
func grpDnLocations(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	grp *pb.Group,
) ([]string, error) {
	addrs := grpDnAddrs(grp)
	locs := make([]string, 0, len(addrs))
	seen := make(map[string]struct{}, len(addrs))
	for _, addrPort := range addrs {
		if _, ok := seen[addrPort]; ok {
			continue
		}
		seen[addrPort] = struct{}{}
		dn := &pb.DnConf{}
		found, err := cli.Get(ctx, model.DnConfKey(cid, addrPort), dn)
		if err != nil {
			return nil, errAborted("%v", err)
		}
		if !found {
			continue
		}
		locs = append(locs, dn.GetLocation())
	}
	return locs, nil
}

// cnLocations is the failure domain of every CN addrs names: the tier-1
// exclusion of CreateCntlr (architecture.md, Per-operation allocation), which
// keeps a new cntlr out of the LOCATIONS the SP's cntlrs occupy and not merely
// off their CNs.
//
// grpDnLocations' CN twin. Reading it before the transaction is sound for two
// reasons together. `location` is immutable — CreateControllerNode
// defaults it to addr_port and UpdateControllerNodeDisabled is the only later
// CN mutator (architecture.md, Controller nodes) — so no location read here
// goes stale. And CreateCntlr's STM drops the pick when the SP, as it reads
// it, has a cntlr on a CN outside addrs: a cntlr committed after this read is
// the only way the set of domains to exclude can grow, and immutability alone
// says nothing about that. Both checks are therefore address-based. A CN whose
// conf is gone contributes no location: it is excluded by address anyway.
func cnLocations(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	addrs []string,
) ([]string, error) {
	locs := make([]string, 0, len(addrs))
	seen := make(map[string]struct{}, len(addrs))
	for _, addrPort := range addrs {
		if _, ok := seen[addrPort]; ok {
			continue
		}
		seen[addrPort] = struct{}{}
		cn := &pb.CnConf{}
		found, err := cli.Get(ctx, model.CnConfKey(cid, addrPort), cn)
		if err != nil {
			return nil, errAborted("%v", err)
		}
		if !found {
			continue
		}
		locs = append(locs, cn.GetLocation())
	}
	return locs, nil
}

// sliceLocation is where a slice walk found something: the slice, its id and
// the group that holds the object.
type sliceLocation struct {
	SliceId uint64
	Slice   *pb.Slice
	Grp     *pb.Group
	Leg     *pb.Leg
	Side    *pb.Side
	IsSpare bool
}

// findSide locates one side by id across every slice, group, leg and spare leg
// of the SP (architecture.md, Cntlrs, InspectSide: the scan is bounded by the
// SP's slices, groups and legs).
func findSide(
	conf *pb.SpConf,
	slices []*pb.Slice,
	sideId uint64,
) (sliceLocation, bool) {
	for idx, slice := range slices {
		for _, grp := range allGroups(slice) {
			for _, leg := range grp.GetLegList() {
				for _, side := range leg.GetSideList() {
					if side.GetSideId() == sideId {
						return sliceLocation{
							SliceId: conf.GetSliceIdList()[idx],
							Slice:   slice,
							Grp:     grp,
							Leg:     leg,
							Side:    side,
						}, true
					}
				}
			}
			for _, leg := range grp.GetSpareLegList() {
				for _, side := range leg.GetSideList() {
					if side.GetSideId() == sideId {
						return sliceLocation{
							SliceId: conf.GetSliceIdList()[idx],
							Slice:   slice,
							Grp:     grp,
							Leg:     leg,
							Side:    side,
							IsSpare: true,
						}, true
					}
				}
			}
		}
	}
	return sliceLocation{}, false
}

// findLeg locates one leg by id across every slice and group of the SP, in
// both the active and the spare list.
func findLeg(
	conf *pb.SpConf,
	slices []*pb.Slice,
	legId uint64,
) (sliceLocation, bool) {
	for idx, slice := range slices {
		for _, grp := range allGroups(slice) {
			for _, leg := range grp.GetLegList() {
				if leg.GetLegId() == legId {
					return sliceLocation{
						SliceId: conf.GetSliceIdList()[idx],
						Slice:   slice,
						Grp:     grp,
						Leg:     leg,
					}, true
				}
			}
			for _, leg := range grp.GetSpareLegList() {
				if leg.GetLegId() == legId {
					return sliceLocation{
						SliceId: conf.GetSliceIdList()[idx],
						Slice:   slice,
						Grp:     grp,
						Leg:     leg,
						IsSpare: true,
					}, true
				}
			}
		}
	}
	return sliceLocation{}, false
}

// findGrp locates one group by id across every slice of the SP.
func findGrp(
	conf *pb.SpConf,
	slices []*pb.Slice,
	grpId uint64,
) (sliceLocation, bool) {
	for idx, slice := range slices {
		for _, grp := range allGroups(slice) {
			if grp.GetGrpId() == grpId {
				return sliceLocation{
					SliceId: conf.GetSliceIdList()[idx],
					Slice:   slice,
					Grp:     grp,
				}, true
			}
		}
	}
	return sliceLocation{}, false
}

// cloneLocation is where a clone walk found something: the clone, under the
// name that keys its record, and its clone meta group with the leg and side
// that hold the object (architecture.md, Terminology and object model).
type cloneLocation struct {
	CloneName string
	Clone     *pb.Clone
	Grp       *pb.Group
	Leg       *pb.Leg
	Side      *pb.Side
	IsSpare   bool
}

// The clone twins of the three slice walks above. A clone meta group belongs
// to no slice, so every lookup that takes a group, leg or side id walks the
// slices first and, only when no slice holds the id, the clones, through
// these: separate functions, as every clone-keyed path is, so no slice walk
// ever sees a clone. Ids are unique in the SP, so a hit in either walk is the
// object. clones is what loadClones returns, in clone_name_list order, and a
// clone with no group holds no group, leg or side and is passed over, so the
// proto getters' zero ids never match one.

// findCloneGrp locates one group by id among the clones' clone meta groups.
func findCloneGrp(
	conf *pb.SpConf,
	clones []*pb.Clone,
	grpId uint64,
) (cloneLocation, bool) {
	for idx, clone := range clones {
		grp := clone.GetMetaGrp()
		if grp == nil || grp.GetGrpId() != grpId {
			continue
		}
		return cloneLocation{
			CloneName: conf.GetCloneNameList()[idx],
			Clone:     clone,
			Grp:       grp,
		}, true
	}
	return cloneLocation{}, false
}

// findCloneLeg locates one leg by id among the clones' clone meta groups, in
// both the active and the spare list.
func findCloneLeg(
	conf *pb.SpConf,
	clones []*pb.Clone,
	legId uint64,
) (cloneLocation, bool) {
	for idx, clone := range clones {
		grp := clone.GetMetaGrp()
		if grp == nil {
			continue
		}
		for _, leg := range allLegs(grp) {
			if leg.GetLegId() != legId {
				continue
			}
			return cloneLocation{
				CloneName: conf.GetCloneNameList()[idx],
				Clone:     clone,
				Grp:       grp,
				Leg:       leg,
				IsSpare:   spareLegOf(grp, legId) != nil,
			}, true
		}
	}
	return cloneLocation{}, false
}

// findCloneSide locates one side by id among the clones' clone meta groups,
// active legs and spare legs alike.
func findCloneSide(
	conf *pb.SpConf,
	clones []*pb.Clone,
	sideId uint64,
) (cloneLocation, bool) {
	for idx, clone := range clones {
		grp := clone.GetMetaGrp()
		if grp == nil {
			continue
		}
		for _, leg := range allLegs(grp) {
			for _, side := range leg.GetSideList() {
				if side.GetSideId() != sideId {
					continue
				}
				return cloneLocation{
					CloneName: conf.GetCloneNameList()[idx],
					Clone:     clone,
					Grp:       grp,
					Leg:       leg,
					Side:      side,
					IsSpare:   spareLegOf(grp, leg.GetLegId()) != nil,
				}, true
			}
		}
	}
	return cloneLocation{}, false
}
