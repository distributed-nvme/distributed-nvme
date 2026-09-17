package model

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The sp drain of dnv-worker.md §11.6: the three worker-side ops
// that tear a LATCHED storage pool down in bounded steps.
//
// DeleteStoragePool no longer tears anything down. It commits `deleting = true`
// plus one SpRev bump and returns (SPD3/SPD4), because the one-shot teardown it
// replaced was unbounded in the DN dimension — already about 532 writes at the
// then-maximum 16-slice shape, over the EtcdMaxTxnOps of the time, and the
// slice ceiling has doubled since — and made unbounded for good by GrowSlice,
// so no single transaction could ever be proven legal. What the one-shot really
// guaranteed was not atomicity but AGREEMENT: DN and CN budgets must never
// disagree with the keys that describe them. That is preserved here by
// construction rather than by a single STM — every batch releases budget in the
// same transaction that shrinks the describing key, so at every commit boundary
// the keys and the budgets agree exactly (SPD13).
//
// The three phases, in the order SPD8 derives them from the SpConf alone:
//
//	D1  DrainSpCntlrs   every cntlr, in one STM, BEFORE any slice work
//	D2  DrainSpSlice    one batch of up to MaxDelGrpPerTxn groups of one slice
//	D3  FinishSpDelete  sp_conf, sp_id_to_name, sp_rev and the shard bucket
//
// Cntlrs go first deliberately, inverting the naive slices-first order (SPD9):
// every cntlr stacks the WHOLE SP on its CN and the coordinator keeps syncing
// during the drain, so slices-first would make every CN reload pool concats and
// disband md arrays on every batch — racing the DN export teardown each time —
// for stacks nothing will ever use. Cntlrs-first tears each CN stack down
// exactly once through the pointer diff, frees CN capacity immediately, and
// leaves the remainder of the drain as pure DN accounting.
//
// Nothing here waits on, calls or verifies an agent (SPD13). Etcd emptiness MAY
// outrun physical teardown; the DN orphan sweep and the CN wrapper sweep are the
// crash-window backstops, exactly as for every other mutation.

// The Op names of the three drain ops. They are the exported function names, so
// that a log record names something greppable.
const (
	opDrainSpCntlrs  = "DrainSpCntlrs"
	opDrainSpSlice   = "DrainSpSlice"
	opFinishSpDelete = "FinishSpDelete"
)

// loadSpConfForDrain is SPD2: the inverse of loadSpConfForOp's "sp deleting"
// refusal. Normal ops require the flag CLEAR, drain ops require it SET, and both
// share the load-and-refuse structure — the SP exists, it is the one the caller
// addressed, and it is in the state this op is allowed to act on.
//
// Every one of the three conditions is an ERROR and never a skip. A reconcile
// loop that reads absence as permission is how the wrong thing gets destroyed:
// an SpConf that is missing, or whose sp_id moved because the name was deleted
// and re-created, describes a different storage pool, and "carry on, the keys
// this op would remove are not there any more" is precisely the reasoning that
// removes someone else's.
//
// sp_level is deliberately NOT consulted (SPD6): AR3's suppression exists so an
// operator can take charge of a degraded SP, and a doomed SP must drain at any
// level.
func loadSpConfForDrain(
	s etcdutil.STM,
	op string,
	cid uint64,
	spName string,
	spId uint64,
) (*pb.SpConf, error) {
	conf := &pb.SpConf{}
	if !s.Get(SpConfKey(cid, spName), conf) {
		return nil, fail(op, "sp not found")
	}
	if conf.GetSpId() != spId {
		return nil, fail(op, "sp id changed")
	}
	if !conf.GetDeleting() {
		return nil, fail(op, "sp not deleting")
	}
	return conf, nil
}

// ---------------------------------------------------------------------------
// D1 — DrainSpCntlrs (SPD9)
// ---------------------------------------------------------------------------

// DrainSpCntlrs removes EVERY cntlr of a latched SP in one STM and returns how
// many it removed (SPD9).
//
// Effects: every `cntlr` key deleted; SpConf put with an empty cntlr_id_list;
// per distinct CN the full ledger release — CnConf put with the pointer gone and
// the SP footprint back, its capacity key maintained (§5.6) and one CnRev bump;
// finally one SpRev bump, whose watch event schedules the next drain step.
//
// It deliberately skips DeleteCntlr's disabled-first and non-primary
// preconditions: those exist to protect host IO and discovery, and a deletable
// SP has an empty nqn_list — no subsystems, hence no CdcEntry keys, no host
// paths and no CDC bookkeeping to drop.
//
// Budget: at most MaxCntlrCntPerSp = 4 CNs, well under EtcdMaxTxnOps even though
// the footprint calculation re-reads every slice.
//
// A cntlr key the SpConf lists but etcd does not have is a REFUSAL and not a
// skip, for spFootprint's reason: the record is what says which CN reserved the
// footprint, so without it the release cannot be made exact. (A CnConf that is
// gone is the other way round — see releaseSpCns.) The cost is that such an SP
// stays latched, the latch being one-way; the benefit is that the refusal
// repeats on every tick as `sp drain failed` naming the key to restore, which
// is a louder and more recoverable state than a silent under-credit.
func DrainSpCntlrs(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
) (int, error) {
	removed := 0
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		// A retried attempt (EU4) re-reads everything, so the count of the
		// abandoned one is dropped with it.
		removed = 0
		conf, err := loadSpConfForDrain(s, opDrainSpCntlrs, cid, spName, spId)
		if err != nil {
			return err
		}
		cntlrIds := conf.GetCntlrIdList()
		if len(cntlrIds) == 0 {
			// Another owner's D1 got there first (SPD8). A drain step is an
			// idempotent "pop what is still there", so an empty list is nothing
			// to do rather than a refusal — SPD2's three conditions above are
			// the only refusals, and a second `sp drain failed` record per pass
			// of an accepted two-owner overlap would be pure noise.
			return nil
		}
		// Every cntlr reserved the SP's WHOLE footprint, so every one of them
		// returns it (§6.5). It is computed from the slices as they are NOW,
		// which is what makes a grown SP release exactly what it charged — and
		// is the second reason D1 runs before any group is popped (SPD9).
		footprint, err := spFootprint(s, opDrainSpCntlrs, cid, conf)
		if err != nil {
			return err
		}
		cntlrs := make([]*pb.Cntlr, 0, len(cntlrIds))
		for _, cntlrId := range cntlrIds {
			cntlr := &pb.Cntlr{}
			if !s.Get(CntlrKey(cid, spId, cntlrId), cntlr) {
				return fail(opDrainSpCntlrs, "cntlr not found")
			}
			cntlrs = append(cntlrs, cntlr)
		}
		// --- effects: everything above has to have been read first, so that a
		// refusal returns without having staged a write (EU4) ---
		for _, cntlrId := range cntlrIds {
			s.Del(CntlrKey(cid, spId, cntlrId))
		}
		err = releaseSpCns(
			s, opDrainSpCntlrs, cid, spId, cntlrIds, cntlrs, footprint,
		)
		if err != nil {
			return err
		}
		conf.CntlrIdList = nil
		s.Put(SpConfKey(cid, spName), conf)
		removed = len(cntlrIds)
		return BumpSpRev(s, opDrainSpCntlrs, shard, cid, spId)
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// releaseSpCns is chargeSpCns's inverse: every cntlr of the SP gives the SP's
// whole footprint back to its CN, with one CnConf write, one capacity-key
// maintenance and one CnRev bump per DISTINCT CN (§5.5, §5.6). A CN hosting two
// cntlrs of one SP is not supposed to exist (§6.4), but if one did it would have
// reserved the footprint twice, so the credit is accumulated per CN and applied
// once — which is also what keeps MaintainCnCapacity's delete target exact.
//
// A CN record that is gone is SKIPPED rather than raised, exactly as releaseCn
// does it: the cntlr key is deleted either way, and there is nothing left to
// give the extents back to.
func releaseSpCns(
	s etcdutil.STM,
	op string,
	cid uint64,
	spId uint64,
	cntlrIds []uint64,
	cntlrs []*pb.Cntlr,
	footprint uint64,
) error {
	var order []string
	old := make(map[string]*pb.CnConf)
	cur := make(map[string]*pb.CnConf)
	for idx, cntlr := range cntlrs {
		addrPort := cntlr.GetAddrPort()
		newCn, ok := cur[addrPort]
		if !ok {
			stored := &pb.CnConf{}
			if !s.Get(CnConfKey(cid, addrPort), stored) {
				continue
			}
			old[addrPort] = stored
			newCn = proto.Clone(stored).(*pb.CnConf)
			cur[addrPort] = newCn
			order = append(order, addrPort)
		}
		newCn.CntlrPtrList = removeCntlrPtr(
			newCn.GetCntlrPtrList(), spId, cntlrIds[idx],
		)
		newCn.FreeExtCnt += footprint
	}
	// First-touch order, so one transaction's writes are deterministic.
	for _, addrPort := range order {
		newCn := cur[addrPort]
		s.Put(CnConfKey(cid, addrPort), newCn)
		MaintainCnCapacity(s, cid, addrPort, old[addrPort], newCn)
		if err := BumpCnRev(s, op, cid, newCn); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// D2 — DrainSpSlice (SPD10, SPD11)
// ---------------------------------------------------------------------------

// DrainSpSlice removes up to common.MaxDelGrpPerTxn groups of ONE slice in one
// STM and reports how many it removed and whether the slice is now gone
// (SPD10, SPD11).
//
// Groups are popped from the TAIL of data_grp_list first and then, if budget
// remains, from the tail of meta_grp_list. Tail-popping is not a detail: the
// position of a group in its list and the leg_idx inside it are the md member
// order, so a surviving group or leg must never be renumbered — and it also
// means the lists never reindex under a concurrent reader.
//
// A batch MUST NOT touch a second slice, even when the first has fewer remaining
// groups than the budget: SPD13's transaction budget is derived for one slice,
// and the slice-final STM below is what keeps the SpConf's id list and the keys
// it points at in step.
//
// For every removed group, each leg's and each spare leg's sides are released
// through the DN ledger — per distinct DN once per STM: DnConf put with the
// pointer gone and the extents back, dn_capacity del + put, one DnRev bump.
//
// If both group lists are empty after the pop, the SAME transaction deletes the
// `slice` key and removes the id from slice_id_list; otherwise it puts the
// shrunken Slice. There is no "empty slice" intermediate state, and no dangling
// id: model.LoadSp fetches children by iterating the id lists, so a slice id
// pointing at nothing would break every subsequent load (SPD10, SPD11).
//
// Every commit therefore leaves a LOADABLE SP whose id lists exactly match its
// keys and whose DN budgets exactly match its sides (SPD13).
func DrainSpSlice(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	sliceId uint64,
	cc *pb.ClusterConf,
) (int, bool, error) {
	removed := 0
	done := false
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		removed, done = 0, false
		// The cluster's stored ladder, used as stored (§7): every DnConf this
		// batch writes goes through MaintainDnCapacity, whose key embeds the bin
		// index the ladder yields, so a conf CreateCluster could not have
		// written would leave the live capacity key behind and write a new one
		// under a bin its free count does not belong to. The gate sits ahead of
		// the first read for the usual reason — a refusal returns having staged
		// nothing.
		if err := ValidateClusterConf(cc); err != nil {
			return fail(opDrainSpSlice, err.Error())
		}
		conf, err := loadSpConfForDrain(s, opDrainSpSlice, cid, spName, spId)
		if err != nil {
			return err
		}
		if len(conf.GetCntlrIdList()) != 0 {
			// SPD8's order made a precondition of the transaction (AR2's shape):
			// a caller that derived D2 from a stale snapshot must not start
			// popping groups while cntlrs are still stacking the whole SP.
			return fail(opDrainSpSlice, "cntlrs not drained")
		}
		if !containsId(conf.GetSliceIdList(), sliceId) {
			// Another owner's batch already emptied and removed this slice.
			// Nothing to do, and — see DrainSpCntlrs — not a refusal.
			return nil
		}
		sliceKey := SliceKey(cid, spId, sliceId)
		slice := &pb.Slice{}
		if !s.Get(sliceKey, slice) {
			return fail(opDrainSpSlice, "slice not found")
		}
		rel := newDnReleaser(s, cid, cc)
		budget := common.MaxDelGrpPerTxn
		dataGrps, budget, dataCnt := popGrps(
			rel, spId, slice.GetDataGrpList(), budget,
		)
		metaGrps, _, metaCnt := popGrps(
			rel, spId, slice.GetMetaGrpList(), budget,
		)
		removed = dataCnt + metaCnt
		// --- effects ---
		if len(dataGrps) == 0 && len(metaGrps) == 0 {
			// SPD11: the key and the id list entry go together, never
			// separately.
			s.Del(sliceKey)
			conf.SliceIdList = removeId(conf.GetSliceIdList(), sliceId)
			s.Put(SpConfKey(cid, spName), conf)
			done = true
		} else {
			slice.DataGrpList = dataGrps
			slice.MetaGrpList = metaGrps
			s.Put(sliceKey, slice)
		}
		if err := rel.flush(opDrainSpSlice); err != nil {
			return err
		}
		return BumpSpRev(s, opDrainSpSlice, shard, cid, spId)
	})
	if err != nil {
		return 0, false, err
	}
	return removed, done, nil
}

// popGrps removes up to budget groups from the TAIL of grps, releasing every
// side of every active leg and every spare leg of each one through rel (§8.12: a
// spare occupies a DN exactly like an active leg). It returns what is left of
// the list, the budget that is left, and how many groups it popped.
func popGrps(
	rel *dnReleaser,
	spId uint64,
	grps []*pb.Group,
	budget int,
) ([]*pb.Group, int, int) {
	popped := 0
	for budget > 0 && len(grps) > 0 {
		grp := grps[len(grps)-1]
		for _, leg := range legsOf(grp) {
			for _, side := range leg.GetSideList() {
				rel.release(
					side.GetAddrPort(), spId,
					side.GetSideId(), grp.GetExtCnt(),
				)
			}
		}
		grps = grps[:len(grps)-1]
		budget--
		popped++
	}
	return grps, budget, popped
}

// legsOf is every leg of ONE group — active legs first, spares after. Both carry
// sides that occupy a DN (§8.12), so both are released.
func legsOf(grp *pb.Group) []*pb.Leg {
	legs := make([]*pb.Leg, 0,
		len(grp.GetLegList())+len(grp.GetSpareLegList()))
	legs = append(legs, grp.GetLegList()...)
	return append(legs, grp.GetSpareLegList()...)
}

// dnReleaser is the release-only twin of the gateway's dnLedger: it accumulates
// one STM's DN bookkeeping so that a DN carrying several sides of the batch is
// read once, written once, has its capacity key maintained once and its revision
// bumped exactly once (§5.5, §5.6).
//
// Every record it hands out is the one THIS transaction read, which is what
// makes MaintainDnCapacity's delete target exact: a capacity key embeds
// free_ext_cnt, so it can only be removed by the transaction that still knows
// the count it was written with.
type dnReleaser struct {
	s     etcdutil.STM
	cid   uint64
	cc    *pb.ClusterConf
	old   map[string]*pb.DnConf
	cur   map[string]*pb.DnConf
	order []string
}

// newDnReleaser opens a releaser over the cluster's STORED conf. The caller
// gates that conf (§7) before the first read; see DrainSpSlice.
func newDnReleaser(
	s etcdutil.STM,
	cid uint64,
	cc *pb.ClusterConf,
) *dnReleaser {
	return &dnReleaser{
		s:   s,
		cid: cid,
		cc:  cc,
		old: make(map[string]*pb.DnConf),
		cur: make(map[string]*pb.DnConf),
	}
}

// release takes one side's pointer out of its DN and gives extCnt extents back.
//
// A DnConf that is gone is skipped, for releaseCn's reason: the key that
// describes the side is being deleted either way, and there is nothing left to
// credit. It is the mirror image of the "slice not found" refusal above — a
// DESCRIBING key that is missing is a refusal, a RECEIVING ledger that is
// missing is not.
func (r *dnReleaser) release(
	addrPort string,
	spId uint64,
	sideId uint64,
	extCnt uint64,
) {
	newDn, ok := r.cur[addrPort]
	if !ok {
		stored := &pb.DnConf{}
		if !r.s.Get(DnConfKey(r.cid, addrPort), stored) {
			return
		}
		r.old[addrPort] = stored
		newDn = proto.Clone(stored).(*pb.DnConf)
		r.cur[addrPort] = newDn
		r.order = append(r.order, addrPort)
	}
	newDn.SidePtrList = removeSidePtr(newDn.GetSidePtrList(), spId, sideId)
	newDn.FreeExtCnt += extCnt
}

// flush writes every touched DN once, maintains its capacity key per §5.6 and
// bumps its revision once (§5.5). Nodes are flushed in first-touch order, so one
// transaction's writes are deterministic.
func (r *dnReleaser) flush(op string) error {
	for _, addrPort := range r.order {
		newDn := r.cur[addrPort]
		r.s.Put(DnConfKey(r.cid, addrPort), newDn)
		MaintainDnCapacity(
			r.s, r.cid, addrPort, r.cc, r.old[addrPort], newDn,
		)
		if err := BumpDnRev(r.s, op, r.cid, newDn); err != nil {
			return err
		}
	}
	return nil
}

// removeSidePtr drops one (sp_id, side_id) pointer from a DN's list, preserving
// the order of the rest. Side ids come from SpConf.next_id and are unique within
// the SP, so the leg id is not needed to address one.
func removeSidePtr(
	list []*pb.SidePointer,
	spId uint64,
	sideId uint64,
) []*pb.SidePointer {
	kept := make([]*pb.SidePointer, 0, len(list))
	for _, ptr := range list {
		if ptr.GetSpId() == spId && ptr.GetSideId() == sideId {
			continue
		}
		kept = append(kept, ptr)
	}
	return kept
}

// ---------------------------------------------------------------------------
// D3 — FinishSpDelete (SPD12)
// ---------------------------------------------------------------------------

// FinishSpDelete removes the SP's last keys in one STM (SPD12): `sp_conf`,
// `sp_id_to_name` and `sp_rev`, plus GW12's deletion half on the cluster's
// SpGlobal so that DeleteCluster's bucket-sum gate stays exact.
//
// It refuses while any cntlr or any slice survives. That guard is what makes the
// drain safe under the accepted two-owner overlap: an owner whose snapshot is
// one step behind cannot skip to the end and strand the keys the other owner is
// still popping.
//
// It is the one drain STM that does NOT bump SpRev — it DELETES the key, and
// that delete is already the shard worker's stop signal for the sp coordinator
// (dnv-worker.md SW3; the prose carrier is architecture.md §8.4). The drain
// therefore terminates itself in the same transaction that finishes the job.
// After the commit the name is reusable.
func FinishSpDelete(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		conf, err := loadSpConfForDrain(s, opFinishSpDelete, cid, spName, spId)
		if err != nil {
			return err
		}
		if len(conf.GetCntlrIdList()) != 0 {
			return fail(opFinishSpDelete, "cntlrs remain")
		}
		if len(conf.GetSliceIdList()) != 0 {
			return fail(opFinishSpDelete, "slices remain")
		}
		// Read, so that a concurrent bump of the key this STM deletes fails the
		// transaction's compare rather than being overwritten by the delete.
		revKey := SpRevKey(shard, cid, spId)
		if !s.Get(revKey, &pb.SpRev{}) {
			return fail(opFinishSpDelete, "sp rev not found")
		}
		globalKey := SpGlobalKey(cid)
		global := &pb.SpGlobal{}
		if !s.Get(globalKey, global) {
			// Read before the first Del so this refusal, like every other one,
			// returns without having staged a write.
			return fail(opFinishSpDelete, "sp global not found")
		}
		s.Del(SpConfKey(cid, spName))
		s.Del(SpNameKey(cid, spId))
		s.Del(revKey)
		// GW12: the bucket shrinks, next_id never rewinds — a deleted sp_id must
		// never come back, so agents may assume it never does (§5.4).
		global.ShardBucket = ReleaseShard(global.GetShardBucket(), shard)
		s.Put(globalKey, global)
		return nil
	})
}
