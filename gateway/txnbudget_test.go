package gateway

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// TestCloneDrainBatchBudget is CLD11's arithmetic tripwire, and the tripwire
// that REPLACED TestDeleteCloneTxnBudget: the rectangle transaction that one
// guarded — the whole src_slice_cnt x bm_cnt sweep in DeleteClone's deciding
// STM — does not exist any more. DeleteClone latches (§5.8) and the worker
// drains the chunk keys in batches of a constant size (dnv-worker.md §11.7).
//
// The arithmetic is one line, because chunk removal is LEDGER-FREE — no DN or
// CN accounting, pure point deletes. etcd caps a transaction at
// max(len(Compare), len(Success), len(Failure)), and etcdutil's
// serializable-snapshot STM compares every key it READ and every key it WROTE,
// so the COMPARE count is the bound:
//
//	reads    = 3                    (SpConf, Clone, SpRev)
//	writes   = MaxDelBmPerTxn + 1   (the dels, the SpRev put)
//	compares = MaxDelBmPerTxn + 4   <- what etcd checks
//
// and it is independent of every ceiling constant. That is the property worth
// guarding: growing MaxCloneBmCnt or MaxSliceCntPerSp now changes the batch
// COUNT and never the transaction's legality, which is what decoupled the
// deployment requirement from the clone shape.
func TestCloneDrainBatchBudget(t *testing.T) {
	const budget = common.MaxDelBmPerTxn + 4
	if budget > common.EtcdMaxTxnOps {
		t.Errorf(
			"one clone drain batch is %d compares (%d deletes + 4), over the %d "+
				"dnv requires etcd to allow: lower MaxDelBmPerTxn or raise "+
				"EtcdMaxTxnOps and the --max-txn-ops of every etcd that "+
				"serves dnv",
			budget, common.MaxDelBmPerTxn, common.EtcdMaxTxnOps)
	}
	// Prose, not a requirement (§6): 68 also fits etcd's DEFAULT cap, so no
	// clone transaction in the system needs the raised flag any more. The
	// deployment requirement stays EtcdMaxTxnOps for the SP drain's sake, and
	// this assertion is what would notice if the clone half stopped being
	// free of it.
	const etcdDefaultMaxTxnOps = 128
	if budget > etcdDefaultMaxTxnOps {
		t.Errorf("one clone drain batch is %d compares, over etcd's own default "+
			"cap of %d: the clone path is no longer free of the deployment "+
			"flag, and gateway.md §2.1's note must change with it",
			budget, etcdDefaultMaxTxnOps)
	}
}

// TestSpDrainBatchBudget is SPD14's arithmetic tripwire: the worst-case D2
// batch must fit inside the transaction size dnv requires of every etcd.
//
// etcd caps a transaction at max(len(Compare), len(Success), len(Failure)),
// and etcdutil's serializable-snapshot STM compares every key it READ and every
// key it WROTE, so the compare count binds. Per §6, with D the number of
// distinct DNs one batch touches:
//
//	reads    = 3 + 2D   (SpConf, Slice, SpRev; per DN DnConf + DnRev)
//	writes   = 3 + 4D   (Slice put/del, SpConf put, SpRev put;
//	                     per DN DnConf put, capacity del + put, DnRev put)
//	compares = 6 + 6D,  D <= MaxDelGrpPerTxn x (MaxAllocLegPerGrp +
//	                                            MaxSpareLegPerGrp)
//
// Every constant is NAMED (SPD1): widening the allocator's group shape, the
// spare ceiling or the batch size past the budget fails here, at once, instead
// of as an "etcd: too many operations in txn request" out of a drain in the
// field. Sides contribute one DN each because a deletable SP has no migrations
// and therefore no two-side legs.
//
// TestDrainSpSliceAtTheCeiling in model/drain_test.go is the PROOF this
// tripwire only approximates: it commits a maximum-shape batch against a real
// etcd started with --max-txn-ops=common.EtcdMaxTxnOps.
func TestSpDrainBatchBudget(t *testing.T) {
	const legs = common.MaxAllocLegPerGrp + common.MaxSpareLegPerGrp
	const dns = common.MaxDelGrpPerTxn * legs
	const budget = 6 + 6*dns
	if budget > common.EtcdMaxTxnOps {
		t.Errorf(
			"one sp drain batch is %d compares (6 + 6 x %d groups x (%d legs "+
				"+ %d spares)), over the %d dnv requires etcd to allow: lower "+
				"MaxDelGrpPerTxn or raise EtcdMaxTxnOps and the --max-txn-ops "+
				"of every etcd that serves dnv",
			budget, common.MaxDelGrpPerTxn, common.MaxAllocLegPerGrp,
			common.MaxSpareLegPerGrp, common.EtcdMaxTxnOps)
	}
}
