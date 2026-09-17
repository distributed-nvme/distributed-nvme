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
// 68 leaves 956 ops of slack that neither test will notice.
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
	// transactions that do exceed the default — CreateStoragePool's
	// 967-compare maximum shape, which is the one the number is now SIZED by
	// (TestCreateStoragePoolBudget below), and the SP drain's 486-compare D2
	// batch — and this assertion is what would notice if the clone half
	// stopped being free of it.
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
// only once the true count passes EtcdMaxTxnOps, and 486 leaves 538 ops of
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

// The factors of ONE CreateStoragePool transaction, transcribed by hand from
// what the STM at the end of storagepool.go's CreateStoragePool and the two
// ledgers of alloc.go actually perform, with the same caveat as the two sets
// above: nothing in this file reads that STM, so a key added to or taken out
// of it is invisible here. TestCreateStoragePoolAtTheCeiling in
// gateway/spceiling_test.go is the half that meets the real transaction.
//
// FIXED — resolveCluster reads ClusterConf, the duplicate-name check reads
// SpConf (ABSENT, which still costs a compare: an absent read emits
// ModRevision(key) = 0) and the id mint reads SpGlobal; the puts are SpConf,
// the sp_id_to_name key, SpRev and SpGlobal.
// PER SLICE — one Slice put.
// PER DN — dnLedger.verifyPick reads DnConf and the exact dn_capacity key the
// scan saw; dnLedger.flush puts DnConf, dels and puts the dn_capacity key
// through model.MaintainDnCapacity, and bumps DnRev, which is a read and a put
// of its own (model.BumpDnRev). Three reads, four writes.
// PER CN — cnLedger.verifyPick, cnLedger.flush, model.MaintainCnCapacity and
// model.BumpCnRev are the exact twins of those three reads and four writes,
// plus that cntlr's own Cntlr put: eight and not seven, because cntlrs and CNs
// are one to one (every CN pick is black-listed, so two cntlrs of one SP never
// share a CN).
//
// D, the number of distinct DNs, is a product of ceilings and nothing else:
// planSpGroups emits spCreateGrpsPerSlice groups per slice, each group takes
// legCntOf legs — MaxAllocLegPerGrp for md-raid1, the widest SP there is — and
// the growing dnBlack list puts every leg of the WHOLE SP on a DN of its own
// (§6.5), so
//
//	D = spCreateGrpsPerSlice x MaxSliceCntPerSp x MaxAllocLegPerGrp
//
// init_ext_cnt never enters: it moves ExtCnt VALUES, not key counts. That is
// what makes this transaction tripwirable at all, and it is why the sentence
// that used to say its size was the REQUEST's is gone from
// common/constants.go.
//
// Two places where a careless reading of the above goes wrong, and how this
// file resolves them — both re-derived from the code and from the etcd client
// in the module cache rather than taken on trust:
//
//   - Four writes per DN is an UPPER bound, not an identity. A DN that falls
//     out of model.DnAllocatable under its charge gets no new capacity key, so
//     MaintainDnCapacity issues the del alone and that DN costs three. The
//     budget wants the worst case, so four is the right factor — but a test
//     that counted the transaction's DISTINCT keys rather than its reads plus
//     its writes would be counting something else and would come out lower.
//   - etcd caps a transaction at max(len(Compare), len(Success), len(Failure)),
//     which on its own leaves open which of the three lists binds. For an
//     etcdutil STM it is always the compare list, and not by a margin:
//     concurrency/stm.go's commit() issues
//     If(conflicts).Then(wset.puts()).Else(rset.gets()) — one op per written
//     key and one per read key — while conflicts is
//     append(rset.cmps(), wset.cmps(...)) with NO dedup across the two sets.
//     So len(Compare) = len(Success) + len(Failure) EXACTLY, a key both read
//     and written is compared twice, and reads + writes is the only number
//     that has to fit.
//
// What this does NOT guard is placement. The count assumes all D DNs and all C
// CNs are distinct; two sides landing on one DN, or two cntlrs on one CN,
// would make the transaction SMALLER, not larger, so a regression in either
// growing black list is caught by TestCreateStoragePoolWriteSet and by
// TestCreateStoragePoolAtTheCeiling's two distinct-node assertions, never
// here.
const (
	spCreateReadsFixed  = 3 // ClusterConf, SpConf (absent), SpGlobal
	spCreateWritesFixed = 4 // SpConf, sp_id_to_name, SpRev, SpGlobal
	// spCreateGrpsPerSlice is planSpGroups' shape: one meta group of one
	// extent and one data group of init_ext_cnt, per slice.
	spCreateGrpsPerSlice   = 2
	spCreateWritesPerSlice = 1 // the Slice put
	spCreateReadsPerDn     = 3 // DnConf, the scan's dn_capacity key, DnRev
	spCreateWritesPerDn    = 4 // DnConf, dn_capacity del + put, DnRev
	spCreateReadsPerCn     = 3 // the DN three, CN-keyed
	spCreateWritesPerCn    = 5 // the DN four CN-keyed, plus the Cntlr put
	// TODAY's ceiling and the compare count the factors produce at it — the
	// 967 the rest of the tree quotes. Pinned for the reason the two tripwires
	// above pin theirs, and for one more: the budget below is an INEQUALITY,
	// and with MaxAllocLegPerGrp and MaxCntlrCntPerSp where they are
	// spCreateCompares(S) is 39 + 29S, which clears EtcdMaxTxnOps for every
	// slice ceiling from 1 through 33. Without this pin the ceiling could move
	// a step at a time under a green suite while every carrier of the 967 —
	// common/constants.go's EtcdMaxTxnOps comment, spceiling_test.go's header
	// and the doc prose that `grep -rn 967` turns up — went stale.
	spCreateMaxSliceCnt = 32
	spCreateMaxCompares = 967
	// The HISTORICAL anchor: the shape this model has to reproduce to be
	// believed. While MaxSliceCntPerSp was 16 the widest create was 503
	// compares, which is why the old EtcdMaxTxnOps of 512 was exactly tight —
	// 9 ops of slack, and already the narrower margin of the two bounded
	// transactions. A model that cannot land on 503 at 16 slices is not a
	// model of this transaction, whatever it computes at 32.
	spCreateOldSliceCnt = 16
	spCreateOldCompares = 503
)

// spCreateCompares is the closed form of the factors above: the compare count
// one CreateStoragePool commits for an SP of sliceCnt slices at the widest
// shape the ceilings allow (md-raid1 legs, MaxCntlrCntPerSp cntlrs, every one
// of them on a CN of its own). C below is MaxCntlrCntPerSp.
//
//	D        = spCreateGrpsPerSlice x sliceCnt x MaxAllocLegPerGrp
//	reads    = spCreateReadsFixed  + spCreateReadsPerDn  x D
//	           + spCreateReadsPerCn x C
//	writes   = spCreateWritesFixed + spCreateWritesPerSlice x sliceCnt
//	           + spCreateWritesPerDn x D + spCreateWritesPerCn x C
//	compares = their sum          <- what etcd checks
func spCreateCompares(sliceCnt int) int {
	dns := spCreateGrpsPerSlice * sliceCnt * common.MaxAllocLegPerGrp
	cns := common.MaxCntlrCntPerSp
	reads := spCreateReadsFixed +
		spCreateReadsPerDn*dns + spCreateReadsPerCn*cns
	writes := spCreateWritesFixed + spCreateWritesPerSlice*sliceCnt +
		spCreateWritesPerDn*dns + spCreateWritesPerCn*cns
	return reads + writes
}

// TestCreateStoragePoolBudget is the create's arithmetic tripwire, the one
// common/constants.go used to say could not be written: the widest
// CreateStoragePool must fit inside the transaction size dnv requires of every
// etcd, and it — not the sp drain's D2 batch — is the transaction
// EtcdMaxTxnOps is now sized by. It was already the larger of the two while
// MaxSliceCntPerSp was 16 (503 against the drain's 486); at 32 it is the one
// the old 512 could not have held.
//
// Every factor is a NAMED constant, which is the whole point: widening the
// slice ceiling, the allocator's group shape or the cntlr ceiling past the
// budget fails here, at once, instead of as an "etcd: too many operations in
// txn request" out of a create in the field.
//
// The headroom is thin and it is a per-DN multiple. D is
// spCreateGrpsPerSlice x MaxSliceCntPerSp x MaxAllocLegPerGrp = 128 at the
// current ceilings, so ONE more read or write per DN inside that STM costs 128
// compares and takes 967 to 1095, over the budget in a single step. A reader
// who is about to add one should move EtcdMaxTxnOps and the --max-txn-ops of
// every etcd that serves dnv in the same change, not discover it later.
func TestCreateStoragePoolBudget(t *testing.T) {
	budget := spCreateCompares(common.MaxSliceCntPerSp)
	dns := spCreateGrpsPerSlice * common.MaxSliceCntPerSp *
		common.MaxAllocLegPerGrp
	t.Logf("the widest CreateStoragePool: %d slices x %d groups x %d legs = "+
		"%d disk nodes, %d cntlrs, %d compares, EtcdMaxTxnOps %d",
		common.MaxSliceCntPerSp, spCreateGrpsPerSlice,
		common.MaxAllocLegPerGrp, dns, common.MaxCntlrCntPerSp, budget,
		common.EtcdMaxTxnOps)

	// The CEILING and what it costs, pinned in ONE arm because — unlike the
	// two tripwires above — they cannot usefully fail apart here: what the
	// tree quotes is the compare count, not the slice ceiling, and both a
	// moved ceiling and a retyped factor make it wrong. Which of the two
	// happened is what the HISTORY arm below tells apart: it stays green when
	// only MaxSliceCntPerSp moved and fires alongside this one when a factor
	// was retyped or one of the other two ceilings moved.
	if common.MaxSliceCntPerSp != spCreateMaxSliceCnt ||
		budget != spCreateMaxCompares {
		t.Errorf(
			"the widest CreateStoragePool moved: MaxSliceCntPerSp %d gives "+
				"%d compares, want the pinned %d at %d slices. Re-pin "+
				"spCreateMaxSliceCnt and spCreateMaxCompares together, and "+
				"with them every carrier that quotes the count — "+
				"common/constants.go's EtcdMaxTxnOps comment, "+
				"gateway/spceiling_test.go's header and the doc prose "+
				"`grep -rn %d` turns up",
			common.MaxSliceCntPerSp, budget, spCreateMaxCompares,
			spCreateMaxSliceCnt, spCreateMaxCompares)
	}
	// The TRANSCRIPTION, evaluated at the HISTORICAL slice count so that
	// raising MaxSliceCntPerSp cannot reach it: it fires when one of the eight
	// factors above is edited without re-deriving what the model has always
	// produced, and — since it reads MaxAllocLegPerGrp and MaxCntlrCntPerSp
	// live — when one of the other two ceilings moves under it.
	if compares := spCreateCompares(spCreateOldSliceCnt); compares !=
		spCreateOldCompares {
		t.Errorf(
			"the CreateStoragePool factors no longer reproduce history: at "+
				"%d slices, MaxAllocLegPerGrp %d and MaxCntlrCntPerSp %d the "+
				"model gives %d compares, want the %d that made the old "+
				"EtcdMaxTxnOps of 512 exactly tight. If you moved one of "+
				"those two ceilings, re-derive spCreateOldCompares at it; "+
				"otherwise a factor was retyped — re-read CreateStoragePool's "+
				"STM and the dnLedger/cnLedger flushes against the eight "+
				"above",
			spCreateOldSliceCnt, common.MaxAllocLegPerGrp,
			common.MaxCntlrCntPerSp, compares, spCreateOldCompares)
	}
	// The BUDGET, at the ceilings as they stand: the deployment requirement
	// itself.
	if budget > common.EtcdMaxTxnOps {
		t.Errorf(
			"the widest CreateStoragePool is %d compares (fixed %d + %d per "+
				"slice x MaxSliceCntPerSp %d + %d per DN x %d DNs + %d per CN "+
				"x MaxCntlrCntPerSp %d, with %d DNs = %d groups per slice x "+
				"MaxSliceCntPerSp %d x MaxAllocLegPerGrp %d), over the %d dnv "+
				"requires etcd to allow: lower MaxSliceCntPerSp, "+
				"MaxAllocLegPerGrp or MaxCntlrCntPerSp, or raise "+
				"EtcdMaxTxnOps and the --max-txn-ops of every etcd that "+
				"serves dnv",
			budget, spCreateReadsFixed+spCreateWritesFixed,
			spCreateWritesPerSlice, common.MaxSliceCntPerSp,
			spCreateReadsPerDn+spCreateWritesPerDn, dns,
			spCreateReadsPerCn+spCreateWritesPerCn, common.MaxCntlrCntPerSp,
			dns, spCreateGrpsPerSlice, common.MaxSliceCntPerSp,
			common.MaxAllocLegPerGrp, common.EtcdMaxTxnOps)
	}
}

// mdNameMaxSliceIdx is the widest slice INDEX common/name_fmt.go's two md name
// formats can carry. Both CnMdDevName and CnMdArrayName fold the meta flag
// into bit 7 of the index — `if isMeta { sliceIdx |= 0x80 }` — and render the
// result "%02x", so meta-of-k and data-of-(k+128) are the same two hex digits.
const mdNameMaxSliceIdx = 0x7f

// TestSliceCeilingFitsTheMdNames is the create's OTHER ceiling check, and the
// reason it sits next to TestCreateStoragePoolBudget: raising
// MaxSliceCntPerSp runs into a second hard limit besides the etcd budget, and
// only the budget had a tripwire. The second is a NAME WIDTH, it binds at 128
// slices where the budget binds at 33, and a breach of it is silent — which is
// why it has never been hit and why it needs an assertion rather than a check
// in the request path.
//
// A slice index is `Slice.slice_idx`, and gateway/storagepool.go's
// CreateStoragePool is the only thing in the tree that assigns one: it numbers
// the slices 0..slice_cnt-1 at create, and no later path (GrowSlice included)
// adds a slice. So the widest index that can reach a name formatter is
// MaxSliceCntPerSp - 1, and the formats admit it up to mdNameMaxSliceIdx.
//
// Past that, meta-slice k and data-slice k+128 produce one md device name and
// one mdadm array name between them. Nothing parses either name back, so
// nothing would return an error: two groups of one SP would simply converge
// onto the same /dev/md device on the same CN. The constant is what would have
// to move for that to happen, which is why the guard is here and not in the
// request path — MaxSliceCntPerSp cannot move without this test being read.
func TestSliceCeilingFitsTheMdNames(t *testing.T) {
	if common.MaxSliceCntPerSp-1 > mdNameMaxSliceIdx {
		t.Errorf(
			"MaxSliceCntPerSp is %d, so slice indexes run to %d, past the %d "+
				"common/name_fmt.go's md names can carry: CnMdDevName and "+
				"CnMdArrayName OR the meta flag into bit 7 of the index and "+
				"print it %%02x, so meta-slice k and data-slice k+%d would "+
				"render the same name and nothing parses one back to notice. "+
				"Widening the two formats renames the md device and the mdadm "+
				"array of every SP that already exists — a migration, not a "+
				"constant edit",
			common.MaxSliceCntPerSp, common.MaxSliceCntPerSp-1,
			mdNameMaxSliceIdx, mdNameMaxSliceIdx+1)
	}
}
