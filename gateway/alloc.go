package gateway

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is the §6.5 half of the gateway: the per-operation candidate
// compositions that run OUTSIDE every STM (model.FindDnCandidatesAntiAffine /
// FindCnCandidates + PickRandom), the in-STM re-validation of a pick that
// makes a scan outside a transaction safe (GW9), and the DN/CN bookkeeping
// ledgers that keep "one write and one revision bump per node per STM" (§5.5)
// true however many sides or cntlrs one transaction touches.

// ---------------------------------------------------------------------------
// Candidate scans (§6.5)
// ---------------------------------------------------------------------------

// legCntOf is the number of legs one group of this SP has: RedundMdRaid1 two,
// RedundNone one (§6.5).
func legCntOf(bdev *pb.BdevConf) int {
	if bdev.GetRedundConf().GetRedundMdRaid1() != nil {
		return 2
	}
	return 1
}

// isMdRaid1 reports whether this SP's groups are md-raid1 pairs.
func isMdRaid1(bdev *pb.BdevConf) bool {
	return bdev.GetRedundConf().GetRedundMdRaid1() != nil
}

// dnPickPlan is one group's DN allocation request: how many extents each leg
// needs, how many legs the group has, and the failure domains tier 1 of the
// §6.5 scan keeps out. ExcludeLocs is empty for the operations §6.5 leaves on
// the plain scan (CreateStoragePool, GrowSlice).
type dnPickPlan struct {
	ExtCnt      uint64
	Legs        int
	ExcludeLocs []string
}

// pickDns draws Legs distinct DNs for one group (§6.5): scan
// dn_batch_size × Legs candidates with at least ExtCnt free extents each, then
// pick Legs of them at random.
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
	batch := int(model.ResolveAllocConf(cc.GetAllocConf()).GetDnBatchSize())
	// The tier bool is deliberately dropped: a tier-2 placement is visible in
	// the stored topology, and §8's LG table gains no record for it.
	cands, _, err := model.FindDnCandidatesAntiAffine(
		ctx, cli, cid, cc,
		plan.ExtCnt,
		plan.Legs*batch,
		plan.Legs,
		append(append([]string(nil), selector.GetBlackList()...), black...),
		selector.GetWhiteList(),
		plan.ExcludeLocs,
	)
	if err != nil {
		return nil, errAborted("%v", err)
	}
	picks := model.PickRandom(cands, plan.Legs)
	if len(picks) < plan.Legs {
		return nil, errExhausted(
			"%s: %d disk nodes with %d free extents, need %d",
			what, len(picks), plan.ExtCnt, plan.Legs)
	}
	return picks, nil
}

// pickCn draws exactly one CN with extCnt free extents (§6.5). spCnAddrs
// names the CNs already hosting a cntlr of this SP, which model excludes so
// that two cntlrs of one SP never share a CN (§6.4).
func pickCn(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	cc *pb.ClusterConf,
	extCnt uint64,
	selector *pb.NodeSelector,
	black []string,
	spCnAddrs []string,
	what string,
) (model.Cand, error) {
	batch := int(model.ResolveAllocConf(cc.GetAllocConf()).GetCnBatchSize())
	cands, err := model.FindCnCandidates(
		ctx, cli, cid,
		extCnt,
		batch,
		append(append([]string(nil), selector.GetBlackList()...), black...),
		selector.GetWhiteList(),
		spCnAddrs,
	)
	if err != nil {
		return model.Cand{}, errAborted("%v", err)
	}
	picks := model.PickRandom(cands, 1)
	if len(picks) < 1 {
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

// dnLedger accumulates one STM's DN bookkeeping so that a DN touched by
// several sides — two legs of the same SP, or every side of an SP being torn
// down — is read once, written once, has its capacity key maintained once and
// its revision bumped exactly once (§5.5, §5.6).
//
// Every record it hands out is the one THIS transaction read, which is what
// makes model.MaintainDnCapacity's delete target exact: a capacity key embeds
// free_ext_cnt, so it can only be removed by the transaction that still knows
// the count it was written with.
type dnLedger struct {
	s     etcdutil.STM
	cid   uint64
	cc    *pb.ClusterConf
	old   map[string]*pb.DnConf
	cur   map[string]*pb.DnConf
	order []string
}

func newDnLedger(s etcdutil.STM, cid uint64, cc *pb.ClusterConf) *dnLedger {
	return &dnLedger{
		s:   s,
		cid: cid,
		cc:  cc,
		old: make(map[string]*pb.DnConf),
		cur: make(map[string]*pb.DnConf),
	}
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
		// by a stored Side is a lost invariant key — §5.9's ABORTED.
		return nil, errAborted("dn_conf for %q is missing", addrPort)
	}
	return dn, nil
}

// verifyPick is GW9's exact test on one allocator pick: the DN still exists
// and is allocatable, it still has room, and the capacity key the scan saw is
// still there. The key embeds free_ext_cnt, so its presence proves the DN has
// not been touched since the scan; its absence is errCandidateChanged and the
// caller re-runs the whole unit.
func (l *dnLedger) verifyPick(
	cand model.Cand,
	extCnt uint64,
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
	return dn, nil
}

// charge is the DN bookkeeping of one side allocation (§5.6, §8.4): the side
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
	return nil
}

// flush writes every touched DN once, maintains its capacity key per §5.6 and
// bumps its revision once (§5.5). Nodes are flushed in first-touch order so
// one transaction's writes are deterministic.
func (l *dnLedger) flush(op string) error {
	for _, addrPort := range l.order {
		cur := l.cur[addrPort]
		l.s.Put(model.DnConfKey(l.cid, addrPort), cur)
		model.MaintainDnCapacity(
			l.s, l.cid, addrPort, l.cc, l.old[addrPort], cur)
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
// maintenance and one CnRev bump per CN per transaction (§5.5, §5.6). It takes
// no ClusterConf — CN capacity keys carry no bin index (§6.4).
type cnLedger struct {
	s     etcdutil.STM
	cid   uint64
	old   map[string]*pb.CnConf
	cur   map[string]*pb.CnConf
	order []string
}

func newCnLedger(s etcdutil.STM, cid uint64) *cnLedger {
	return &cnLedger{
		s:   s,
		cid: cid,
		old: make(map[string]*pb.CnConf),
		cur: make(map[string]*pb.CnConf),
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
		// named, so its absence is a lost invariant key — §5.9's ABORTED.
		return nil, errAborted("cn_conf for %q is missing", addrPort)
	}
	return cn, nil
}

// verifyPick is GW9 for a CN pick. CN capacity keys carry no bin field, so the
// key is rebuilt from the free count the scan saw.
func (l *cnLedger) verifyPick(
	cand model.Cand,
	extCnt uint64,
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
	return nil
}

// flush writes every touched CN once, maintains its capacity key and bumps its
// revision once.
func (l *cnLedger) flush(op string) error {
	for _, addrPort := range l.order {
		cur := l.cur[addrPort]
		l.s.Put(model.CnConfKey(l.cid, addrPort), cur)
		model.MaintainCnCapacity(l.s, l.cid, addrPort, l.old[addrPort], cur)
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
// caller's STM. A listed slice that does not exist is §5.9's ABORTED: an SP
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
// both carry sides that occupy a DN (§8.12).
func allLegs(grp *pb.Group) []*pb.Leg {
	legs := make([]*pb.Leg, 0,
		len(grp.GetLegList())+len(grp.GetSpareLegList()))
	legs = append(legs, grp.GetLegList()...)
	legs = append(legs, grp.GetSpareLegList()...)
	return legs
}

// spFootprint is Σ ext_cnt over ALL groups of ALL slices — meta and data
// alike — which is what one cntlr's CN reserves for the SP (§8.4, §8.6).
func spFootprint(slices []*pb.Slice) uint64 {
	total := uint64(0)
	for _, slice := range slices {
		for _, grp := range allGroups(slice) {
			total += grp.GetExtCnt()
		}
	}
	return total
}

// grpDnAddrs is every DN a group already occupies through an active leg or a
// spare leg: the black-list seed of CreateMigration and CreateSpareLeg (§6.5).
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
// exclusion of CreateMigration and CreateSpareLeg (§6.5), which is about
// LOCATIONS and not merely about the DNs the black list already holds.
//
// One plain DnConf read per DISTINCT addr_port, outside every STM like the
// capacity scan it feeds. Reading them before the transaction is sound because
// `location` is immutable in v1 — CreateDiskNode defaults it to addr_port and
// UpdateDiskNodeDisabled is the only later DN mutator (§8.2) — so a location
// read here cannot have gone stale by the time the op re-validates the pick,
// which is also why that re-validation stays address-based. A DN whose conf is
// gone contributes no location: it is black-listed by address anyway, and
// inventing one would exclude a domain nothing occupies.
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
// of the SP (§8.6: the scan is bounded by MaxSliceCntPerSp × groups ×
// MaxLegPerGrp).
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

// sliceOf finds a slice by id in a loaded list.
func sliceOf(conf *pb.SpConf, slices []*pb.Slice, sliceId uint64) *pb.Slice {
	for idx, id := range conf.GetSliceIdList() {
		if id == sliceId && idx < len(slices) {
			return slices[idx]
		}
	}
	return nil
}
