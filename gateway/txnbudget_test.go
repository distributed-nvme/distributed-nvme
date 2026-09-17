package gateway

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The factors of one clone-drain batch, TRANSCRIBED BY HAND from what
// model.DrainCloneBm's STM does: loadCloneForOp reads SpConf and the Clone,
// BumpSpRev reads SpRev, and the writes are one Del per chunk plus the SpRev
// put.
//
// Transcribed is the operative word, and it bounds what any assertion in this
// file can be worth: the file imports nothing but testing and common, so a key
// added to or taken out of that STM changes nothing here and every check below
// stays green (verified by mutation). What the exact assertions do catch is a
// CEILING that moved — those operands really are read from common — and a
// half-done re-pin, a factor retyped without re-deriving the compare total,
// which is the moment to go and read the STM again. The transaction as etcd
// receives it is checked where it is committed: for the clone drain that is
// model/clonedrain_test.go's TestDrainCloneBmRefusesAnOversizedBatch, whose
// second half commits a batch of exactly MaxDelBmPerTxn against the real etcd
// that model's tests start with --max-txn-ops=common.EtcdMaxTxnOps — but, as
// for the sp batch below, only once the true count passes EtcdMaxTxnOps, and
// 68 leaves 444 ops of slack that neither test will notice.
const (
	cloneDrainReads       = 3 // SpConf, Clone, SpRev
	cloneDrainWritesFixed = 1 // the SpRev put; the dels are the batch itself
	// cloneDrainMaxDels is today's ceiling and cloneDrainMaxCompares the
	// worst-case compare count the factors above produce at it. They are
	// asserted separately so that a ceiling change and a retyped factor fail
	// with different messages.
	cloneDrainMaxDels     = 64
	cloneDrainMaxCompares = 68
)

// TestCloneDrainBatchBudget is CLD11's arithmetic tripwire, and the tripwire
// that REPLACED TestDeleteCloneTxnBudget: the rectangle transaction that one
// guarded — the whole rectangle of chunk keys swept in DeleteClone's deciding
// STM — does not exist any more. DeleteClone latches (§5.8) and the worker
// drains the chunk keys in batches of a constant size (dnv-worker.md §11.7).
//
// The arithmetic is one line, because chunk removal is LEDGER-FREE — no DN or
// CN accounting, pure point deletes. etcd caps a transaction at
// max(len(Compare), len(Success), len(Failure)), and etcdutil's
// serializable-snapshot STM compares every key it READ and every key it WROTE,
// so the COMPARE count is the bound:
//
//	reads    = cloneDrainReads
//	writes   = MaxDelBmPerTxn + cloneDrainWritesFixed
//	compares = their sum          <- what etcd checks
//
// and it is independent of every ceiling constant. That is the property worth
// guarding: growing MaxCloneBmCnt or MaxSliceCntPerSp now changes the batch
// COUNT and never the transaction's legality, which is what decoupled the
// deployment requirement from the clone shape.
func TestCloneDrainBatchBudget(t *testing.T) {
	const budget = common.MaxDelBmPerTxn +
		cloneDrainReads + cloneDrainWritesFixed

	// The CEILING, exactly. MaxDelBmPerTxn may well be raised one day — the
	// batch stays legal for a long way yet — but not by accident, and not
	// without re-pinning what it makes the batch cost.
	if common.MaxDelBmPerTxn != cloneDrainMaxDels {
		t.Errorf(
			"the clone-drain CEILING moved: common.MaxDelBmPerTxn is %d, "+
				"want the pinned %d. Re-pin cloneDrainMaxDels and "+
				"cloneDrainMaxCompares together, the latter at dels %d + "+
				"reads %d + fixed writes %d",
			common.MaxDelBmPerTxn, cloneDrainMaxDels,
			common.MaxDelBmPerTxn, cloneDrainReads, cloneDrainWritesFixed)
	}
	// The TRANSCRIPTION, evaluated at the PINNED ceiling so that a ceiling
	// change cannot reach it: it fires when one of the two factors above is
	// edited without re-deriving cloneDrainMaxCompares, and is blind to
	// anything that happens inside the STM itself.
	if compares := cloneDrainMaxDels + cloneDrainReads +
		cloneDrainWritesFixed; compares != cloneDrainMaxCompares {
		t.Errorf(
			"the clone-drain factors no longer add up: dels %d + reads %d "+
				"+ fixed writes %d = %d compares, want the pinned %d. Re-read "+
				"model.DrainCloneBm's STM against the two factors above and "+
				"re-pin cloneDrainMaxCompares once the one that moved is the "+
				"one you meant to move",
			cloneDrainMaxDels, cloneDrainReads, cloneDrainWritesFixed,
			compares, cloneDrainMaxCompares)
	}
	// The BUDGET. The two assertions above pin `budget` at
	// cloneDrainMaxCompares exactly; this one is what the batch must satisfy
	// even after a deliberate re-pin.
	if budget > common.EtcdMaxTxnOps {
		t.Errorf(
			"one clone drain batch is %d compares (dels %d + reads %d + "+
				"fixed writes %d), over the %d dnv requires etcd to allow: "+
				"lower MaxDelBmPerTxn or raise EtcdMaxTxnOps and the "+
				"--max-txn-ops of every etcd that serves dnv",
			budget, common.MaxDelBmPerTxn, cloneDrainReads,
			cloneDrainWritesFixed, common.EtcdMaxTxnOps)
	}
	// Prose, not a requirement (CLD11): 68 also fits etcd's DEFAULT cap, so no
	// clone transaction in the system needs the raised flag any more. The
	// deployment requirement stays EtcdMaxTxnOps for the sake of the two
	// transactions that do exceed the default — the SP drain's 486-compare D2
	// batch and CreateStoragePool's 503-compare maximum shape — and this
	// assertion is what would notice if the clone half stopped being free of
	// it.
	const etcdDefaultMaxTxnOps = 128
	if budget > etcdDefaultMaxTxnOps {
		t.Errorf("one clone drain batch is %d compares, over etcd's own default "+
			"cap of %d: the clone path is no longer free of the deployment "+
			"flag, and gateway.md §2.1's note must change with it",
			budget, etcdDefaultMaxTxnOps)
	}
}

// The factors of one sp-drain D2 batch, as model.DrainSpSlice's STM actually
// performs it. Fixed: loadSpConfForDrain reads SpConf, the batch reads the
// Slice and BumpSpRev reads SpRev; the worst-case write side is the final
// batch of a slice, which dels the Slice and puts the SpConf with the id gone,
// plus the SpRev put (a non-final batch puts the Slice instead and writes one
// fewer). Per DN: dnReleaser.flush reads DnConf and, through BumpDnRev, DnRev,
// and writes the DnConf, the dn_capacity del and put of MaintainDnCapacity and
// the DnRev.
//
// Like the clone factors above, these are transcribed by hand and nothing in
// this file reads the model package: a key added to or taken out of that STM
// is invisible here (verified by mutation). What meets the real transaction is
// TestDrainSpSliceAtTheCeiling, described below.
const (
	spDrainReadsFixed  = 3 // SpConf, Slice, SpRev
	spDrainWritesFixed = 3 // Slice del, SpConf put, SpRev put
	spDrainReadsPerDn  = 2 // DnConf, DnRev
	spDrainWritesPerDn = 4 // DnConf put, dn_capacity del + put, DnRev put
	// spDrainMaxDns is the most distinct DNs today's ceilings let one batch
	// touch, and spDrainMaxCompares the worst-case compare count the factors
	// above produce at it. Separate assertions, for the clone tripwire's
	// reason: a ceiling change and a retyped factor must not fail alike.
	spDrainMaxDns      = 80
	spDrainMaxCompares = 486
)

// TestSpDrainBatchBudget is SPD14's arithmetic tripwire: the worst-case D2
// batch must fit inside the transaction size dnv requires of every etcd.
//
// etcd caps a transaction at max(len(Compare), len(Success), len(Failure)),
// and etcdutil's serializable-snapshot STM compares every key it READ and every
// key it WROTE, so the compare count binds. Per SPD13, with D the number of
// distinct DNs one batch touches:
//
//	reads    = spDrainReadsFixed  + spDrainReadsPerDn  x D
//	writes   = spDrainWritesFixed + spDrainWritesPerDn x D
//	compares = their sum,
//	D <= MaxDelGrpPerTxn x (MaxAllocLegPerGrp + MaxSpareLegPerGrp)
//
// Every constant is NAMED (SPD1): widening the allocator's group shape, the
// spare ceiling or the batch size past the budget fails here, at once, instead
// of as an "etcd: too many operations in txn request" out of a drain in the
// field. Sides contribute one DN each because a deletable SP has no migrations
// and therefore no two-side legs.
//
// TestDrainSpSliceAtTheCeiling in model/drain_test.go is what this arithmetic
// cannot be, because nothing here reads the STM: it commits a maximum-shape
// batch against a real etcd started with --max-txn-ops=common.EtcdMaxTxnOps,
// so a read or a write ADDED to DrainSpSlice is caught there instead — but
// only once the true count passes EtcdMaxTxnOps, and 486 leaves 26 ops of
// slack that neither test will notice.
func TestSpDrainBatchBudget(t *testing.T) {
	const legs = common.MaxAllocLegPerGrp + common.MaxSpareLegPerGrp
	const dns = common.MaxDelGrpPerTxn * legs
	const perDn = spDrainReadsPerDn + spDrainWritesPerDn
	const budget = spDrainReadsFixed + spDrainWritesFixed + perDn*dns

	// The CEILINGS, exactly: all three of them at once, since only the
	// combination MaxDelGrpPerTxn x (MaxAllocLegPerGrp + MaxSpareLegPerGrp)
	// enters the budget.
	if dns != spDrainMaxDns {
		t.Errorf(
			"the sp-drain DN dimension moved: MaxDelGrpPerTxn %d x "+
				"(MaxAllocLegPerGrp %d + MaxSpareLegPerGrp %d) = %d DNs, "+
				"want the pinned %d. Re-pin spDrainMaxDns and "+
				"spDrainMaxCompares together",
			common.MaxDelGrpPerTxn, common.MaxAllocLegPerGrp,
			common.MaxSpareLegPerGrp, dns, spDrainMaxDns)
	}
	// The TRANSCRIPTION, evaluated at the PINNED DN count so that a ceiling
	// change cannot reach it: it fires when one of the four factors above is
	// edited without re-deriving spDrainMaxCompares, and is blind to anything
	// that happens inside the STM itself.
	if compares := spDrainReadsFixed + spDrainWritesFixed +
		perDn*spDrainMaxDns; compares != spDrainMaxCompares {
		t.Errorf(
			"the sp-drain factors no longer add up: fixed %d (reads %d + "+
				"writes %d) + per DN %d (reads %d + writes %d) x %d DNs = %d "+
				"compares, want the pinned %d. Re-read model.DrainSpSlice's "+
				"STM and dnReleaser.flush against the four factors above and "+
				"re-pin spDrainMaxCompares once the one that moved is the one "+
				"you meant to move",
			spDrainReadsFixed+spDrainWritesFixed, spDrainReadsFixed,
			spDrainWritesFixed, perDn, spDrainReadsPerDn, spDrainWritesPerDn,
			spDrainMaxDns, compares, spDrainMaxCompares)
	}
	// The BUDGET. The two assertions above pin `budget` at spDrainMaxCompares
	// exactly; this one is what the batch must satisfy even after a deliberate
	// re-pin, and it is the deployment requirement itself.
	if budget > common.EtcdMaxTxnOps {
		t.Errorf(
			"one sp drain batch is %d compares (fixed %d + per DN %d x %d "+
				"groups x (%d legs + %d spares)), over the %d dnv requires "+
				"etcd to allow: lower MaxDelGrpPerTxn or raise EtcdMaxTxnOps "+
				"and the --max-txn-ops of every etcd that serves dnv",
			budget, spDrainReadsFixed+spDrainWritesFixed, perDn,
			common.MaxDelGrpPerTxn, common.MaxAllocLegPerGrp,
			common.MaxSpareLegPerGrp, common.EtcdMaxTxnOps)
	}
}
