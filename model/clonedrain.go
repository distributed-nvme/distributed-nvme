package model

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The clone drain (dnv-worker.md, The clone drain): the two worker-side ops
// that remove a LATCHED clone's bitmap chunks and then the clone itself.
//
// It is the sp drain's sibling (dnv-worker.md, The sp drain) and reuses its
// shapes on purpose — the same load-and-refuse guard, the same "derive the
// position from surviving state, never from a checkpoint", the same cadence of
// one step per pass or per wake-up of the drain. Three things differ, and each
// for a reason:
//
//   - Its batches are LEDGER-FREE. Clone chunks are not budgeted to any node,
//     so a batch touches no `dn_*` or `cn_*` key, and it neither reads nor
//     bumps the `sp_rev` key (CLD8): it reads the SpConf and the Clone and
//     deletes its chunks, which is what makes it a constant MaxDelBmPerTxn
//     plus two compares whatever the ceilings become. The final STM moves
//     one ledger: the sides of the clone's clone meta group go back to their
//     DNs exactly as an sp drain batch gives a slice's back, at most six
//     compares per distinct DN, and a clone meta group has at most
//     MaxAllocLegPerGrp legs and MaxSpareLegPerGrp spares of one side each,
//     so those bound them. No CN is touched: a CN holds no clone's metadata,
//     which is on the clone's clone meta group (cnagent.md CN18), and is
//     charged nothing for a clone. The final STM is the one step that needs
//     the cluster's stored conf, for the DN capacity keys' bins.
//   - The SP OUTLIVES the clone, so the final STM bumps SpRev like every other
//     mutator instead of deleting a stop signal.
//   - The drain runs ALONGSIDE the SP's normal reaction pass rather than
//     instead of it (CLD7): the SP is healthy, and its other children must keep
//     converging while one of its clones goes away. The wake-up a batch posts
//     once its call succeeds runs the next drain steps alone, at most one per
//     deleting clone, and the reactions keep their cntlr_interval pass.
//
// A single deciding STM sweeping the whole MaxSliceCntPerSp x MaxCloneBmCnt
// rectangle of chunk keys would grow with both ceilings; incremental deletion
// makes growth change the batch COUNT and never the transaction's legality.

// The Op names of the two clone-drain ops, the exported function names as
// everywhere else in this package.
const (
	opDrainCloneBm      = "DrainCloneBm"
	opFinishCloneDelete = "FinishCloneDelete"
)

// loadCloneForOp is CLD2: SPD2's load-and-refuse shape applied to a clone, and
// inverted exactly as it is there. Normal clone RPCs (CLD1) require the flag
// CLEAR; drain ops require it SET.
//
// It loads the SpConf first — the clone key is addressed by the SP's id, so an
// SP that was deleted and re-created under the same name makes every clone id
// the caller carries meaningless — and then the Clone. Every condition is an
// ERROR and never a skip: a reconcile loop that reads absence as permission is
// how the wrong thing gets destroyed.
//
// The SP's own `deleting` is refused too. It cannot happen — `DeleteStoragePool`
// requires an empty `clone_name_list` and a draining clone keeps its name there
// until the final STM — but the two drains must never be able to run on one SP
// at once, and a guard is cheaper than the argument that they cannot.
func loadCloneForOp(
	s etcdutil.STM,
	op string,
	cid uint64,
	spName string,
	spId uint64,
	cloneName string,
	cloneId uint64,
) (*pb.SpConf, *pb.Clone, error) {
	conf := &pb.SpConf{}
	if !s.Get(SpConfKey(cid, spName), conf) {
		return nil, nil, fail(op, "sp not found")
	}
	if conf.GetSpId() != spId {
		return nil, nil, fail(op, "sp id changed")
	}
	if conf.GetDeleting() {
		return nil, nil, fail(op, "sp deleting")
	}
	clone := &pb.Clone{}
	if !s.Get(CloneKey(cid, spId, cloneName), clone) {
		return nil, nil, fail(op, "clone not found")
	}
	if clone.GetCloneId() != cloneId {
		return nil, nil, fail(op, "clone id changed")
	}
	if !clone.GetDeleting() {
		return nil, nil, fail(op, "clone not deleting")
	}
	return conf, clone, nil
}

// DrainCloneBm deletes up to common.MaxDelBmPerTxn of a latched clone's bitmap
// chunk keys in one STM and returns the SIZE OF THE BATCH it was handed (CLD8)
// — not a count of keys that were still there. A Del is an idempotent pop, and
// telling the two apart would cost a Get per key, which is exactly the cost
// this shape avoids; the loser of an accepted two-owner overlap therefore
// reports a full batch and removes nothing.
//
// chunks are the addresses the caller's keys-only snapshot scan found
// (SpState.CloneBmIdx, MD3), and NOTHING else is deleted: an STM cannot range,
// so the set this op may touch has to come in from outside, and naming it
// exactly is what keeps a batch from removing a chunk an append wrote after the
// scan.
//
// It MUST NOT rewrite the Clone record (CLD8). The record carries no chunk
// count to maintain — the set of a clone's chunks IS the set of its chunk keys,
// which is what this drain derives its position from — and rewriting the record
// would both cost an op and invent a second copy of the truth.
//
// It neither reads nor bumps SpRev (CLD8). A batch changes nothing a side or a
// cntlr is sent — the latch took the clone, with its chunks, out of every
// cntlr's request (CLD5), and no side's request carries a clone's chunks — so
// a bump would only re-send the SP's whole desired state to every side and
// every cntlr once per batch. Nor does the pool's existence need a
// revision read: the SpConf load refuses a pool that is gone (CLD2), and the
// sp drain deletes the SpConf and the SpRev key in one transaction (SPD12).
// The pool's revision therefore moves once for the latch and once for the
// final STM, whatever the batch count (CLD12).
//
// Physical effect: none (CLD8). The CN drops its local chunk files in its sweep
// (cnagent.md CN21) once the clone has left the cntlr's request, which the
// latch's fan-out does (CLD5); agents never read etcd; and a push of the clone
// that finds a chunk key a batch deleted ends there, harmlessly (CLD5). So the
// batch is pure bookkeeping removal.
func DrainCloneBm(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	spId uint64,
	spName string,
	cloneName string,
	cloneId uint64,
	chunks []BmChunk,
) (int, error) {
	removed := 0
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		// A retried attempt (EU4) re-reads everything, so the count of the
		// abandoned one is dropped with it.
		removed = 0
		if _, _, err := loadCloneForOp(
			s, opDrainCloneBm, cid, spName, spId, cloneName, cloneId,
		); err != nil {
			return err
		}
		if len(chunks) == 0 {
			// The caller's scan found nothing to pop. Not a refusal: the final
			// STM is the step that belongs to that state, and this one simply
			// has no work — see the sp drain's DrainSpCntlrs for the same
			// reasoning about an accepted two-owner overlap.
			return nil
		}
		if len(chunks) > common.MaxDelBmPerTxn {
			// The caller's cut, re-applied inside the transaction: the batch
			// bound is what makes this op's size a constant, and a caller that
			// handed over the whole scan would silently rebuild the
			// unbounded sweep this design exists to remove.
			return fail(opDrainCloneBm, "batch too large")
		}
		for _, chunk := range chunks {
			s.Del(CloneBitmapKey(
				cid, spId, cloneName, chunk.SliceIdx, chunk.Idx))
		}
		removed = len(chunks)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// FinishCloneDelete removes the Clone key and its `clone_name_list` entry in one
// STM, gives the sides of the clone's clone meta group back to their DNs, and
// bumps SpRev (CLD9).
//
// The two go together and never separately, for the sp drain's slice-final
// reason: model.LoadSp fetches clones by iterating `clone_name_list`, so a
// dangling name would break every subsequent load, and a key left behind with
// the name gone would be unreachable for ever.
//
// The caller invokes it only when its snapshot scan found zero surviving chunk
// keys, and that emptiness is STABLE even though an STM cannot range to
// re-check it: after the latch commits, no chunk key can ever appear again —
// AppendCloneBitmap refuses on `deleting` (CLD1) inside an STM that reads the
// Clone, and an append that read the Clone BEFORE the latch fails its compare
// at commit because the latch rewrote that key — and batches only delete. So
// scan-emptiness at any revision at or after the scan's is permanent.
//
// Unlike the sp drain's D3 this bumps SpRev: the SP outlives the clone, so the
// bump is every mutator's normal epilogue rather than a stop-signal deletion.
// It is the drain's one bump after the latch's, its batches bumping none
// (CLD12). After the commit the name is reusable and `DeleteStoragePool`'s
// clone precondition no longer sees it.
//
// The same STM gives the clone's one charge back, here, in the step that
// removes the record it is read from, so it is given back exactly once: every
// side of the clone meta group's legs and spares goes back through the DN
// ledger exactly as DrainSpSlice gives a popped group's back — per distinct
// DN once, the pointer out, the group's ext_cnt back, the capacity key
// maintained and one DnRev bump. A DnConf that is gone is skipped, there
// being nothing left to credit, and a clone with no group gives nothing back,
// so a drain is never stranded on one. No CN is read or written: a clone
// charges none.
//
// The pointer's removal and the DnRev bump take a side out of its dn agent's
// next SyncupDn, which tears down the side the sp worker drove until this
// transaction (dnv-worker.md RW14), as it does any side whose pointer left,
// and nothing here waits on an agent (CLD10).
//
// cc is the cluster's stored conf, used as stored (architecture.md, Common
// validation): MaintainDnCapacity's key embeds the bin index its ladder
// yields, so it is gated at the top of the STM, ahead of the first read, as
// DrainSpSlice gates it, and a refusal returns having staged nothing. The
// gate holds for a clone with no group too, so the step has one contract.
func FinishCloneDelete(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	cloneName string,
	cloneId uint64,
	cc *pb.ClusterConf,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		if err := ValidateClusterConf(cc); err != nil {
			return fail(opFinishCloneDelete, err.Error())
		}
		conf, clone, err := loadCloneForOp(
			s, opFinishCloneDelete, cid, spName, spId, cloneName, cloneId)
		if err != nil {
			return err
		}
		rel := newDnReleaser(s, cid, cc)
		grp := clone.GetMetaGrp()
		for _, leg := range legsOf(grp) {
			for _, side := range leg.GetSideList() {
				rel.release(
					side.GetAddrPort(), spId,
					side.GetSideId(), grp.GetExtCnt(),
				)
			}
		}
		s.Del(CloneKey(cid, spId, cloneName))
		conf.CloneNameList = removeName(conf.GetCloneNameList(), cloneName)
		s.Put(SpConfKey(cid, spName), conf)
		if err := rel.flush(opFinishCloneDelete); err != nil {
			return err
		}
		return BumpSpRev(s, opFinishCloneDelete, shard, cid, spId)
	})
}

// removeName drops one entry from a name list, preserving the order of the
// rest. It is removeId's string twin; the gateway has its own copy, which is
// the one its handlers read.
func removeName(list []string, name string) []string {
	kept := make([]string, 0, len(list))
	for _, item := range list {
		if item == name {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}
