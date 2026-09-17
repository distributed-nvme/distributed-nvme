package model

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The clone drain of dnv-worker.md §11.7: the two worker-side ops that remove a
// LATCHED clone's bitmap chunks and then the clone itself.
//
// It is the sp drain's sibling (§11.6) and reuses its shapes on purpose — the
// same load-and-refuse guard, the same "derive the position from surviving
// state, never from a checkpoint", the same one-step-per-pass cadence. Three
// things differ, and each for a reason:
//
//   - It is LEDGER-FREE. Clone chunks are not budgeted to any node and the CN
//     arena units behind the metadata wrapper are agent-local, freed by the
//     retire path, so no `dn_*` or `cn_*` key is ever touched. That is what
//     makes a batch a constant 68 compares whatever the ceilings become.
//   - The SP OUTLIVES the clone, so the final STM bumps SpRev like every other
//     mutator instead of deleting a stop signal.
//   - The drain runs ALONGSIDE the SP's normal reaction pass rather than
//     instead of it (CLD7): the SP is healthy, and its other children must keep
//     converging while one of its clones goes away.
//
// What DeleteClone replaced: a single deciding STM that swept the whole
// rectangle of chunk keys — 512 deletes at today's 32×16 and, back when it was
// 256 at 16×16, the founding justification for the OLD EtcdMaxTxnOps = 512.
// Both ceilings are expected to grow and one of them has (MaxSliceCntPerSp
// doubled on 2026-09-17); incremental deletion makes growth change the batch
// COUNT and never the transaction's legality.

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
// It MUST NOT rewrite the Clone record (CLD7). The record carries no chunk
// count to maintain — the set of a clone's chunks IS the set of its chunk keys,
// which is what this drain derives its position from — and rewriting the record
// would both cost an op and invent a second copy of the truth.
//
// Physical effect: none. The CN dropped its local chunk files at retire — the
// first syncup after the latch, where the clone is already absent from the
// plan — agents never read etcd, and an excluded clone has no bitmap pusher, so
// the batch is pure bookkeeping removal.
func DrainCloneBm(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
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
		return BumpSpRev(s, opDrainCloneBm, shard, cid, spId)
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// FinishCloneDelete removes the Clone key and its `clone_name_list` entry in one
// STM and bumps SpRev (CLD9).
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
// After the commit the name is reusable and `DeleteStoragePool`'s clone
// precondition no longer sees it.
func FinishCloneDelete(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	cloneName string,
	cloneId uint64,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		conf, _, err := loadCloneForOp(
			s, opFinishCloneDelete, cid, spName, spId, cloneName, cloneId)
		if err != nil {
			return err
		}
		s.Del(CloneKey(cid, spId, cloneName))
		conf.CloneNameList = removeName(conf.GetCloneNameList(), cloneName)
		s.Put(SpConfKey(cid, spName), conf)
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
