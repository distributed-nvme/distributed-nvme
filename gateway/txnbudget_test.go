package gateway

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The factors of one clone-drain batch, TRANSCRIBED BY HAND from what
// model.DrainCloneBm's STM does: loadCloneForOp reads SpConf and the Clone,
// and the writes are one Del per chunk and nothing else — the batch neither
// reads nor bumps SpRev (dnv-worker.md CLD8).
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
// cloneDrainMaxCompares leaves slack that neither test will notice.
const (
	cloneDrainReads       = 2 // SpConf, Clone
	cloneDrainWritesFixed = 0 // none; the dels are the batch itself
	// cloneDrainMaxDels is the ceiling and cloneDrainMaxCompares the
	// worst-case compare count the factors above produce at it. They are
	// asserted separately so that a ceiling change and a retyped factor fail
	// with different messages.
	cloneDrainMaxDels     = 64
	cloneDrainMaxCompares = 66
)

// TestCloneDrainBatchBudget is CLD11's arithmetic tripwire: DeleteClone
// latches (gateway.md, Clones) and the worker drains the chunk keys in batches
// of a constant size (dnv-worker.md, The clone drain).
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
// guarding: growing MaxCloneBmCnt or MaxSliceCntPerSp changes the batch COUNT
// and never the transaction's legality, so the deployment requirement does not
// depend on the clone shape.
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
	// Prose, not a requirement (CLD11): the batch also fits etcd's DEFAULT
	// cap, and so does the drain's final step (TestFinishCloneDeleteTxnBudget
	// below), so no clone drain transaction needs the raised flag. The
	// deployment requirement stays EtcdMaxTxnOps for the sake of the
	// transactions that do exceed the default — among them CreateStoragePool's
	// maximum shape, which is the one the number is SIZED by
	// (TestCreateStoragePoolBudget below), the SP drain's D2 batch
	// (TestSpDrainBatchBudget below), the created flip's transaction
	// (TestFlipCreatedTxnBudget below) and the clone flip
	// (TestFlipCloneProvisionedTxnBudget below) — and this assertion is what
	// would notice if the clone drain's batches stopped being free of it.
	if budget > etcdDefaultTxnOps {
		t.Errorf("one clone drain batch is %d compares, over etcd's own default "+
			"cap of %d: the clone path is no longer free of the deployment "+
			"flag, and the note in gateway.md, Constants this "+
			"document owns, must change with it",
			budget, etcdDefaultTxnOps)
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
	// spDrainMaxDns is the most distinct DNs the ceilings let one batch
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
// only once the true count passes EtcdMaxTxnOps, and spDrainMaxCompares
// leaves slack that neither test will notice.
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
// (architecture.md, Per-operation allocation), so
//
//	D = spCreateGrpsPerSlice x MaxSliceCntPerSp x MaxAllocLegPerGrp
//
// init_ext_cnt never enters: it moves ExtCnt VALUES, not key counts. That is
// what makes this transaction tripwirable at all.
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
// growing black list is caught elsewhere, never here: the DN list's by
// TestCreateStoragePoolWriteSet's and TestCreateStoragePoolAtTheCeiling's
// distinct-node assertions, the CN list's by TestCntlrsSpreadAcrossLocations'
// tier-2 case and TestCreateStoragePoolRefusals' one-CN case. The tier-1
// location exclusion of architecture.md, Per-operation allocation, stands in
// for a lost CN list only while tier 1 still
// finds a CN outside the picks' locations: the tier-2 case's CNs share
// locations, and where every CN is its own location but fewer CNs have room
// than cntlr_cnt — the one-CN case — a lost list turns the RESOURCE_EXHAUSTED
// refusal into two cntlrs on one CN.
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
	// The ceiling and the compare count the factors produce at it — the count
	// common/constants.go's EtcdMaxTxnOps comment quotes. Pinned for the
	// reason the two tripwires above pin theirs, and for one more: the budget
	// below is an INEQUALITY with slack, so without this pin the ceiling could
	// move a step at a time under a green suite while that comment went stale.
	spCreateMaxSliceCnt = 32
	spCreateMaxCompares = 967
	// A second slice count at which the transcription is evaluated, out of
	// reach of a move of MaxSliceCntPerSp: it fires when a factor is retyped
	// or another ceiling moves. A model that cannot land on
	// spCreateOldCompares at spCreateOldSliceCnt slices is not a model of this
	// transaction, whatever it computes at the ceiling.
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

// TestCreateStoragePoolBudget is the create's arithmetic tripwire: the widest
// CreateStoragePool must fit inside the transaction size dnv requires of every
// etcd, and it — not the sp drain's D2 batch — is the transaction
// EtcdMaxTxnOps is sized by.
//
// Every factor is a NAMED constant, which is the whole point: widening the
// slice ceiling, the allocator's group shape or the cntlr ceiling past the
// budget fails here, at once, instead of as an "etcd: too many operations in
// txn request" out of a create in the field.
//
// The headroom is thin and it is a per-DN multiple. D is
// spCreateGrpsPerSlice x MaxSliceCntPerSp x MaxAllocLegPerGrp, so ONE more
// read or write per DN inside that STM costs D compares and takes the count
// over the budget in a single step. A reader who is about to add one should
// move EtcdMaxTxnOps and the --max-txn-ops of every etcd that serves dnv in
// the same change, not discover it later.
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
	// happened is what the second-slice-count arm below tells apart: it
	// stays green when only MaxSliceCntPerSp moved and fires alongside this
	// one when a factor was retyped or one of the other two ceilings moved.
	if common.MaxSliceCntPerSp != spCreateMaxSliceCnt ||
		budget != spCreateMaxCompares {
		t.Errorf(
			"the widest CreateStoragePool moved: MaxSliceCntPerSp %d gives "+
				"%d compares, want the pinned %d at %d slices. Re-pin "+
				"spCreateMaxSliceCnt and spCreateMaxCompares together, and "+
				"with them every carrier that quotes the count — "+
				"common/constants.go's EtcdMaxTxnOps comment (`grep -rn %d`)",
			common.MaxSliceCntPerSp, budget, spCreateMaxCompares,
			spCreateMaxSliceCnt, spCreateMaxCompares)
	}
	// The TRANSCRIPTION, evaluated at the second slice count so that
	// raising MaxSliceCntPerSp cannot reach it: it fires when one of the eight
	// factors above is edited without re-deriving what the model
	// produces, and — since it reads MaxAllocLegPerGrp and MaxCntlrCntPerSp
	// live — when one of the other two ceilings moves under it.
	if compares := spCreateCompares(spCreateOldSliceCnt); compares !=
		spCreateOldCompares {
		t.Errorf(
			"the CreateStoragePool factors do not reproduce the pinned "+
				"second shape: at %d slices, MaxAllocLegPerGrp %d and "+
				"MaxCntlrCntPerSp %d the model gives %d compares, want the "+
				"pinned %d. If you moved one of "+
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

// The factors of ONE committing DeleteThinDevice, transcribed by hand from the
// deciding STM of thindevice.go's decideDeleteThinDevice, with the caveat of
// every set above: nothing in this file reads that STM, so a key added to or
// taken out of it is invisible here. TestDeleteThinDeviceAtTheTdCeiling in
// gateway/handler_vol_test.go is the half that meets the real transaction —
// at the td ceiling, with no subsystem or clone in the pool.
//
// FIXED — openSp reads ClusterConf, SpConf and SpRev, and the target td is
// read; the writes are the td's del, the SpConf put and the SpRev put
// (BumpSpRev's read of SpRev is openSp's, already in the read set).
// PER SUBSYSTEM — tdNamespaceRef reads each Subsystem of nqn_list once; the
// namespaces are embedded in that value, so MaxNsCntPerSs never enters.
// PER CLONE — tdCloneRef reads each Clone of clone_name_list once.
// PER TD — nothing, and it is the factor this tripwire exists for. The walk
// for the target's uncreated snapshots is the PLAN's, one read-only Snapshot
// outside the transaction, and the deciding STM re-reads only the snapshots
// the plan named, after checking that it resolved the SP the plan walked and
// that SpRev has not moved since the plan read it. Every write to a td bumps
// SpRev, so a snapshot the plan named still blocks there and the STM refuses:
// no td but the target is ever in a committed read set. The identity check
// reads nothing new — the cluster and sp ids come from keys already counted
// under FIXED. A walk inside the STM would make this factor one per td and
// put a delete from a full pool past the budget before a single subsystem or
// clone.
const (
	tdDeleteReadsFixed    = 4 // ClusterConf, SpConf, SpRev, the target td
	tdDeleteWritesFixed   = 3 // td del, SpConf put, SpRev put
	tdDeleteReadsPerSs    = 1 // the Subsystem
	tdDeleteReadsPerClone = 1 // the Clone
	tdDeleteReadsPerTd    = 0 // the walk is the plan's, outside the STM
	// The two ceilings and the compare count the factors produce at them —
	// the count common/constants.go's EtcdMaxTxnOps comment quotes. Separate
	// assertions, for the clone tripwire's reason: a ceiling change and a
	// retyped factor must not fail alike.
	tdDeleteMaxSsCnt    = 4
	tdDeleteMaxCloneCnt = 64
	tdDeleteMaxCompares = 75
)

// TestDeleteThinDeviceBudget is DeleteThinDevice's arithmetic tripwire, and
// the reason it sits beside the create's: EtcdMaxTxnOps is SIZED by the widest
// CreateStoragePool, and a deciding STM that compared every td of the pool
// would be refused at the td ceiling. The plan/verify split keeps the td count
// out of that STM; this is what notices the count coming back, since one read
// per td takes the budget past EtcdMaxTxnOps in a single step.
func TestDeleteThinDeviceBudget(t *testing.T) {
	budget := tdDeleteReadsFixed + tdDeleteWritesFixed +
		tdDeleteReadsPerSs*common.MaxSsCntPerSp +
		tdDeleteReadsPerClone*common.MaxCloneCntPerSp +
		tdDeleteReadsPerTd*common.MaxTdCntPerSp
	create := spCreateCompares(common.MaxSliceCntPerSp)
	t.Logf("the widest committing DeleteThinDevice: %d subsystems, %d clones "+
		"and %d thin devices in the SP, %d compares; the widest "+
		"CreateStoragePool %d; EtcdMaxTxnOps %d",
		common.MaxSsCntPerSp, common.MaxCloneCntPerSp, common.MaxTdCntPerSp,
		budget, create, common.EtcdMaxTxnOps)

	// The CEILINGS, exactly: the two that enter the count. MaxTdCntPerSp is
	// deliberately not among them — raising it must stay free.
	if common.MaxSsCntPerSp != tdDeleteMaxSsCnt ||
		common.MaxCloneCntPerSp != tdDeleteMaxCloneCnt {
		t.Errorf(
			"the DeleteThinDevice ceilings moved: MaxSsCntPerSp %d and "+
				"MaxCloneCntPerSp %d, want the pinned %d and %d. Re-pin "+
				"tdDeleteMaxSsCnt, tdDeleteMaxCloneCnt and tdDeleteMaxCompares "+
				"together, and with them the count common/constants.go's "+
				"EtcdMaxTxnOps comment quotes",
			common.MaxSsCntPerSp, common.MaxCloneCntPerSp,
			tdDeleteMaxSsCnt, tdDeleteMaxCloneCnt)
	}
	// The TRANSCRIPTION, evaluated at the PINNED ceilings so that a ceiling
	// change cannot reach it: it fires when one of the five factors above is
	// edited without re-deriving tdDeleteMaxCompares — the per-td one above
	// all — and is blind to anything that happens inside the STM itself.
	if compares := tdDeleteReadsFixed + tdDeleteWritesFixed +
		tdDeleteReadsPerSs*tdDeleteMaxSsCnt +
		tdDeleteReadsPerClone*tdDeleteMaxCloneCnt +
		tdDeleteReadsPerTd*common.MaxTdCntPerSp; compares !=
		tdDeleteMaxCompares {
		t.Errorf(
			"the DeleteThinDevice factors no longer add up: fixed %d (reads "+
				"%d + writes %d) + %d per subsystem x %d + %d per clone x %d "+
				"+ %d per td x MaxTdCntPerSp %d = %d compares, want the "+
				"pinned %d. Re-read decideDeleteThinDevice's STM against the "+
				"five factors above; a per-td factor above zero means the "+
				"walk for uncreated snapshots is back inside the transaction",
			tdDeleteReadsFixed+tdDeleteWritesFixed, tdDeleteReadsFixed,
			tdDeleteWritesFixed, tdDeleteReadsPerSs, tdDeleteMaxSsCnt,
			tdDeleteReadsPerClone, tdDeleteMaxCloneCnt, tdDeleteReadsPerTd,
			common.MaxTdCntPerSp, compares, tdDeleteMaxCompares)
	}
	// The BUDGET, at the ceilings as they stand: the deployment requirement
	// itself.
	if budget > common.EtcdMaxTxnOps {
		t.Errorf(
			"the widest DeleteThinDevice is %d compares, over the %d dnv "+
				"requires etcd to allow: keep the td walk out of the deciding "+
				"STM, or lower MaxSsCntPerSp or MaxCloneCntPerSp",
			budget, common.EtcdMaxTxnOps)
	}
	// And the create stays the widest transaction, which is what
	// common/constants.go and gateway.md, Constants this document owns, say
	// sizes EtcdMaxTxnOps.
	if budget > create {
		t.Errorf(
			"the widest DeleteThinDevice is %d compares, wider than the "+
				"widest CreateStoragePool's %d: EtcdMaxTxnOps is no longer "+
				"sized by the create, and every carrier that says it is — "+
				"`grep -rn 'SIZED by'` — is wrong",
			budget, create)
	}
}

// mdNameMaxSliceIdx is the widest slice INDEX common/name_fmt.go's two md name
// formats can carry. Both CnMdDevName and CnMdArrayName fold the meta flag
// into bit 7 of the index — `if isMeta { sliceIdx |= 0x80 }` — and render the
// result "%02x", so meta-of-k and data-of-(k+128) are the same two hex digits.
const mdNameMaxSliceIdx = 0x7f

// TestSliceCeilingFitsTheMdNames is the create's OTHER ceiling check, and the
// reason it sits in this file with TestCreateStoragePoolBudget: raising
// MaxSliceCntPerSp runs into a second hard limit besides the etcd budget. The
// second is a NAME WIDTH, it binds at mdNameMaxSliceIdx + 1 slices, far above
// where the budget binds, and a breach of it is silent — which is why it needs
// an assertion rather than a check in the request path.
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

// The factors of ONE created-flip transaction, transcribed by hand from the
// STM model.FlipCreated commits for each MaxFlipCreatedPerTxn candidates: a
// Get of each candidate's td key — an absent key still costs its compare —
// and a Put of each one it flips, plus BumpSpRev's read and put of SpRev. The
// worst case is a transaction that flips every candidate it carries. Like the
// sets above these are transcribed, and nothing in this file reads that STM:
// model/ops_test.go's TestFlipCreatedAtTheTdCeiling is the half that commits
// it against a real etcd — but only once the true count passes
// EtcdMaxTxnOps, and flipCreatedMaxCompares leaves slack enough that one
// more key read or written per candidate still commits there. Nor does this
// file see the chunk FlipCreated really commits, only the constant: that same
// model test pins it, from the mod_revision of every td key it flipped.
const (
	flipCreatedReadsPerTd  = 1 // the td key
	flipCreatedWritesPerTd = 1 // the td key, created = true
	flipCreatedReadsFixed  = 1 // SpRev
	flipCreatedWritesFixed = 1 // SpRev
	// flipCreatedMaxTds is the ceiling and flipCreatedMaxCompares the
	// worst-case compare count the factors above produce at it. Separate
	// assertions, for the clone tripwire's reason: a ceiling change and a
	// retyped factor must not fail alike.
	flipCreatedMaxTds      = 256
	flipCreatedMaxCompares = 514
)

// TestFlipCreatedTxnBudget is the created flip's arithmetic tripwire (RW19):
// one transaction of model.FlipCreated must fit inside the transaction size
// dnv requires of every etcd. The candidate list the sp worker hands it holds
// every td one drain of its reports completed, which only MaxTdCntPerSp
// bounds, so what bounds the transaction is T = MaxFlipCreatedPerTxn:
//
//	reads    = flipCreatedReadsFixed  + flipCreatedReadsPerTd  x T
//	writes   = flipCreatedWritesFixed + flipCreatedWritesPerTd x T
//	compares = their sum          <- what etcd checks
//
// It is independent of MaxTdCntPerSp, which moves the transaction COUNT and
// never a transaction's legality. Unlike the clone drain's batch, the flip
// does not fit etcd's default cap: the created flip is one of the transactions
// the deployment flag exists for.
func TestFlipCreatedTxnBudget(t *testing.T) {
	const perTd = flipCreatedReadsPerTd + flipCreatedWritesPerTd
	const budget = flipCreatedReadsFixed + flipCreatedWritesFixed +
		perTd*common.MaxFlipCreatedPerTxn

	// The CEILING, exactly: it may be moved, but not by accident, and not
	// without re-pinning what it makes one transaction cost.
	if common.MaxFlipCreatedPerTxn != flipCreatedMaxTds {
		t.Errorf(
			"the created-flip CEILING moved: common.MaxFlipCreatedPerTxn is "+
				"%d, want the pinned %d. Re-pin flipCreatedMaxTds and "+
				"flipCreatedMaxCompares together, the latter at %d per td x "+
				"tds + %d fixed, and with them every carrier that quotes the "+
				"count — common/constants.go's MaxFlipCreatedPerTxn comment "+
				"(`grep -rn %d`)",
			common.MaxFlipCreatedPerTxn, flipCreatedMaxTds, perTd,
			flipCreatedReadsFixed+flipCreatedWritesFixed,
			flipCreatedMaxCompares)
	}
	// The TRANSCRIPTION, evaluated at the PINNED ceiling so that a ceiling
	// change cannot reach it: it fires when one of the four factors above is
	// edited without re-deriving flipCreatedMaxCompares, and is blind to
	// anything that happens inside the STM itself.
	if compares := flipCreatedReadsFixed + flipCreatedWritesFixed +
		perTd*flipCreatedMaxTds; compares != flipCreatedMaxCompares {
		t.Errorf(
			"the created-flip factors no longer add up: fixed %d (reads %d "+
				"+ writes %d) + per td %d (reads %d + writes %d) x %d tds = "+
				"%d compares, want the pinned %d. Re-read model.FlipCreated's "+
				"STM against the four factors above and re-pin "+
				"flipCreatedMaxCompares once the one that moved is the one "+
				"you meant to move",
			flipCreatedReadsFixed+flipCreatedWritesFixed,
			flipCreatedReadsFixed, flipCreatedWritesFixed, perTd,
			flipCreatedReadsPerTd, flipCreatedWritesPerTd, flipCreatedMaxTds,
			compares, flipCreatedMaxCompares)
	}
	// The BUDGET. The two assertions above pin `budget` at
	// flipCreatedMaxCompares exactly; this one is what a transaction must
	// satisfy even after a deliberate re-pin, and it is the deployment
	// requirement itself.
	if budget > common.EtcdMaxTxnOps {
		t.Errorf(
			"one created-flip transaction is %d compares (fixed %d + per td "+
				"%d x MaxFlipCreatedPerTxn %d), over the %d dnv requires etcd "+
				"to allow: lower MaxFlipCreatedPerTxn or raise EtcdMaxTxnOps "+
				"and the --max-txn-ops of every etcd that serves dnv",
			budget, flipCreatedReadsFixed+flipCreatedWritesFixed, perTd,
			common.MaxFlipCreatedPerTxn, common.EtcdMaxTxnOps)
	}
}

// etcdDefaultTxnOps is etcd's own default cap on the operations of one
// transaction: one past it commits only under the raised cap the deployment
// requirement EtcdMaxTxnOps sets.
const etcdDefaultTxnOps = 128

// txnTerm is one per-item factor of a transaction's compare count: what one
// item costs in compares — its reads plus its writes — and how many items the
// ceilings allow, read live from common and pinned beside it.
type txnTerm struct {
	item    string
	perItem int
	live    int
	pinned  int
}

// txnShape is one transaction's arithmetic tripwire at the ceilings, in the
// form of the tests above: its factors TRANSCRIBED BY HAND from its STM, with
// the same caveat — nothing here reads that STM, so a key added to or taken
// out of it is invisible until a factor is re-derived. Where a test commits
// the shape against a real etcd, the shape's own comment names it.
//
//	compares = fixedReads + fixedWrites + Σ perItem x items
//
// check asserts four things, each failing with its own message: every
// ceiling still has its pinned value; the factors at the pinned values still
// give maxCompares, the count common/constants.go's EtcdMaxTxnOps comment
// quotes for the transactions over etcd's default; the count at the live
// ceilings fits EtcdMaxTxnOps; and it is no wider than the widest
// CreateStoragePool, which is what EtcdMaxTxnOps is sized by. overDefault
// says on which side of etcd's default the count falls, which decides
// whether that comment lists the transaction among the ones that need the
// raised flag.
type txnShape struct {
	name        string
	stm         string
	fixedReads  int
	fixedWrites int
	terms       []txnTerm
	maxCompares int
	overDefault bool
}

// compares is the shape's count with every term at its live or its pinned
// item count.
func (shape txnShape) compares(live bool) int {
	total := shape.fixedReads + shape.fixedWrites
	for _, term := range shape.terms {
		items := term.pinned
		if live {
			items = term.live
		}
		total += term.perItem * items
	}
	return total
}

func (shape txnShape) check(t *testing.T) {
	t.Helper()
	budget := shape.compares(true)
	create := spCreateCompares(common.MaxSliceCntPerSp)
	t.Logf("the widest %s: %d compares; the widest CreateStoragePool %d; "+
		"etcd's default %d; EtcdMaxTxnOps %d", shape.name, budget, create,
		etcdDefaultTxnOps, common.EtcdMaxTxnOps)
	for _, term := range shape.terms {
		if term.live != term.pinned {
			t.Errorf("a %s ceiling moved: %d %ss at the ceilings, want "+
				"the pinned %d. Re-pin the term and maxCompares together, "+
				"and with them the count common/constants.go's "+
				"EtcdMaxTxnOps comment quotes", shape.name, term.live,
				term.item, term.pinned)
		}
	}
	if got := shape.compares(false); got != shape.maxCompares {
		t.Errorf("the %s factors no longer add up: %d compares at the "+
			"pinned ceilings, want the pinned %d. Re-read %s against the "+
			"factors and re-pin maxCompares once the one that moved is the "+
			"one you meant to move", shape.name, got, shape.maxCompares,
			shape.stm)
	}
	if budget > common.EtcdMaxTxnOps {
		t.Errorf("the widest %s is %d compares, over the %d dnv requires "+
			"etcd to allow: lower a ceiling it is counted by, or raise "+
			"EtcdMaxTxnOps and the --max-txn-ops of every etcd that serves "+
			"dnv", shape.name, budget, common.EtcdMaxTxnOps)
	}
	if over := budget > etcdDefaultTxnOps; over != shape.overDefault {
		t.Errorf("the widest %s is %d compares, which moved it across "+
			"etcd's default cap of %d: the list of transactions over that "+
			"default in common/constants.go's EtcdMaxTxnOps comment, and "+
			"in gateway.md, Constants this document owns, must change "+
			"with it", shape.name, budget, etcdDefaultTxnOps)
	}
	if budget > create {
		t.Errorf("the widest %s is %d compares, wider than the widest "+
			"CreateStoragePool's %d: EtcdMaxTxnOps is no longer sized by "+
			"the create, and every carrier that says it is — `grep -rn "+
			"'SIZED by'` — is wrong", shape.name, budget, create)
	}
}

// TestCreateCloneTxnBudget is CreateClone's tripwire: its STM reads every
// other clone of the pool for the one-destination rule and charges every
// leg's DN its clone meta group's side as a new group's legs are charged
// (dnLedger): a leg's DN is verified, written, re-indexed and bumped. It reads
// no cntlr and no CN. A clone meta group has the legs the pool's redundancy
// gives a group, at most MaxAllocLegPerGrp (SPD1 cites it here), each on a
// distinct DN.
func TestCreateCloneTxnBudget(t *testing.T) {
	txnShape{
		name: "CreateClone",
		stm:  "clone.go's CreateClone and dnLedger",
		// ClusterConf, SpConf, SpRev, the new Clone key (absent, which still
		// costs a compare) and the destination td.
		fixedReads: 5,
		// The Clone, SpConf and SpRev puts.
		fixedWrites: 3,
		terms: []txnTerm{
			// The one-destination walk: every clone the pool already has,
			// one short of the ceiling the create refuses at.
			{"other clone", 1, common.MaxCloneCntPerSp - 1, 63},
			// The DnConf, the scan's dn_capacity key and the DnRev read;
			// the DnConf put, the dn_capacity delete and put, and the DnRev
			// put.
			{"leg DN", 7, common.MaxAllocLegPerGrp, 2},
		},
		maxCompares: 85,
		overDefault: false,
	}.check(t)
}

// TestDeleteSpareLegTxnBudget is DeleteSpareLeg's tripwire at its widest, a
// spare of a clone meta group: the group is looked up in every slice first
// and only then in every clone (openGrpForSpareLeg), and the spare's one side
// gives its DN back its extent through the DN ledger. A spare of a slice's
// group reads no clone and is the narrower shape. A spare never has a
// second side here, since a spare under migration is refused.
func TestDeleteSpareLegTxnBudget(t *testing.T) {
	txnShape{
		name: "DeleteSpareLeg",
		stm:  "spareleg.go's DeleteSpareLeg, openGrpForSpareLeg and dnLedger",
		// ClusterConf, SpConf, SpRev, and the spare's DnConf and DnRev.
		fixedReads: 5,
		// The Clone, the DnConf, the dn_capacity delete and put, the DnRev
		// and the SpRev.
		fixedWrites: 6,
		terms: []txnTerm{
			{"slice", 1, common.MaxSliceCntPerSp, 32},
			{"clone", 1, common.MaxCloneCntPerSp, 64},
		},
		maxCompares: 107,
		overDefault: false,
	}.check(t)
}

// TestUpdateCntlidSlotListTxnBudget is UpdateStoragePoolCntlidSlotList's
// tripwire: a slot may leave the list only when no cntlr and no side uses
// it, so its STM reads every cntlr, every slice and every clone of the pool,
// the clones for the sides of their clone meta groups.
func TestUpdateCntlidSlotListTxnBudget(t *testing.T) {
	txnShape{
		name: "UpdateStoragePoolCntlidSlotList",
		stm:  "storagepool.go's UpdateStoragePoolCntlidSlotList",
		// ClusterConf, SpConf and SpRev.
		fixedReads: 3,
		// The SpConf and SpRev puts.
		fixedWrites: 2,
		terms: []txnTerm{
			{"cntlr", 1, common.MaxCntlrCntPerSp, 4},
			{"slice", 1, common.MaxSliceCntPerSp, 32},
			{"clone", 1, common.MaxCloneCntPerSp, 64},
		},
		maxCompares: 105,
		overDefault: false,
	}.check(t)
}

// TestCreateCntlrTxnBudget is CreateCntlr's tripwire: besides the pick, its
// STM reads the pool's slices for the footprint, its cntlrs for the slot and
// plan checks and its subsystems for the CdcEntry rewrite. It reads no clone,
// since a clone charges no CN.
func TestCreateCntlrTxnBudget(t *testing.T) {
	txnShape{
		name: "CreateCntlr",
		stm:  "cntlr.go's CreateCntlr, cnLedger and syncCdcEntries",
		// ClusterConf, SpConf, SpRev, the pick's CnConf, the scan's
		// cn_capacity key, and the CnRev the bump reads.
		fixedReads: 6,
		// The Cntlr, CnConf, cn_capacity delete and put, CnRev, SpConf and
		// SpRev.
		fixedWrites: 7,
		terms: []txnTerm{
			// The cntlrs the pool already has, one short of the ceiling the
			// create refuses at.
			{"other cntlr", 1, common.MaxCntlrCntPerSp - 1, 3},
			{"slice", 1, common.MaxSliceCntPerSp, 32},
			// The Subsystem and its CdcEntry read, the CdcEntry put.
			{"subsystem", 3, common.MaxSsCntPerSp, 4},
		},
		maxCompares: 60,
		overDefault: false,
	}.check(t)
}

// TestDeleteCntlrTxnBudget is DeleteCntlr's tripwire, CreateCntlr's mirror:
// it reads the pool's slices for the footprint the old CN gives back, and no
// clone.
func TestDeleteCntlrTxnBudget(t *testing.T) {
	txnShape{
		name: "DeleteCntlr",
		stm:  "cntlr.go's DeleteCntlr, cnLedger and syncCdcEntries",
		// ClusterConf, SpConf, SpRev, the target Cntlr, its CN's CnConf and
		// the CnRev the bump reads.
		fixedReads: 6,
		// The Cntlr delete, CnConf, cn_capacity delete and put, CnRev,
		// SpConf and SpRev.
		fixedWrites: 7,
		terms: []txnTerm{
			// The cntlrs the CdcEntry rewrite reads, the target's left out.
			{"other cntlr", 1, common.MaxCntlrCntPerSp - 1, 3},
			{"slice", 1, common.MaxSliceCntPerSp, 32},
			{"subsystem", 3, common.MaxSsCntPerSp, 4},
		},
		maxCompares: 60,
		overDefault: false,
	}.check(t)
}

// TestFinishMigrationTxnBudget is FinishMigration's tripwire for its deciding
// STM: besides the source DN it releases, it reads and writes the destination
// DN, which gets back the migration's metadata units — a units-only change,
// so one capacity put, no delete and no DnRev.
func TestFinishMigrationTxnBudget(t *testing.T) {
	txnShape{
		name: "FinishMigration",
		stm:  "migration.go's FinishMigration and dnLedger",
		// ClusterConf, SpConf, SpRev, the Migration, the source DnConf and
		// DnRev, and the destination DnConf.
		fixedReads: 7,
		// The source DnConf, dn_capacity delete and put and DnRev; the
		// destination DnConf and dn_capacity put; the Migration delete and
		// the Slice, SpConf and SpRev puts.
		fixedWrites: 10,
		terms: []txnTerm{
			{"slice", 1, common.MaxSliceCntPerSp, 32},
			// One delete per bitmap chunk.
			{"bitmap chunk", 1, common.MaxMigrBmCnt, 4},
		},
		maxCompares: 53,
		overDefault: false,
	}.check(t)
}

// TestReplaceCntlrTxnBudget is the tripwire of model.ReplaceCntlr (dnv-worker.md
// MD6): its STM reads every cntlr, every slice for the footprint and every
// subsystem for the CdcEntry rewrite, and no clone, since a clone charges no
// CN; at the ceilings it stays under etcd's default cap.
// model/ops_test.go's TestReplaceCntlrAtTheCeiling commits this shape against
// a real etcd.
func TestReplaceCntlrTxnBudget(t *testing.T) {
	txnShape{
		name: "ReplaceCntlr",
		stm:  "model.ReplaceCntlr's STM and releaseCn",
		// SpConf, the new CN's CnConf, the scan's cn_capacity key, the old
		// CN's CnConf, both CnRevs and SpRev.
		fixedReads: 7,
		// The old Cntlr delete; the old CN's CnConf, cn_capacity delete and
		// put and CnRev; the new Cntlr; the new CN's CnConf, cn_capacity
		// delete and put and CnRev; SpConf; SpRev.
		fixedWrites: 12,
		terms: []txnTerm{
			// Every listed Cntlr, the old one's included, read once.
			{"cntlr", 1, common.MaxCntlrCntPerSp, 4},
			{"slice", 1, common.MaxSliceCntPerSp, 32},
			{"subsystem", 3, common.MaxSsCntPerSp, 4},
		},
		maxCompares: 67,
		overDefault: false,
	}.check(t)
}

// TestFinishCloneDeleteTxnBudget is the tripwire of model.FinishCloneDelete,
// the clone drain's final step (dnv-worker.md CLD9, CLD11): it gives every
// side of the clone meta group back to its DN, through the DN ledger an sp
// drain batch uses, and reads no cntlr and no CN. A clone meta group has at most MaxAllocLegPerGrp
// legs and MaxSpareLegPerGrp spares, one side each — its legs are never
// migrated — on as many distinct DNs (SPD1 cites MaxAllocLegPerGrp here).
// model/clonedrain_test.go's TestFinishCloneDeleteAtTheCeiling commits this
// shape against a real etcd.
func TestFinishCloneDeleteTxnBudget(t *testing.T) {
	txnShape{
		name: "FinishCloneDelete",
		stm:  "model.FinishCloneDelete's STM and the dnReleaser",
		// SpConf, the Clone and SpRev.
		fixedReads: 3,
		// The Clone delete and the SpConf and SpRev puts.
		fixedWrites: 3,
		terms: []txnTerm{
			// The DnConf and DnRev read; the DnConf put, the dn_capacity
			// delete and put, and the DnRev put.
			{"dn", 6,
				common.MaxAllocLegPerGrp + common.MaxSpareLegPerGrp, 4},
		},
		maxCompares: 30,
		overDefault: false,
	}.check(t)
}

// TestFlipCloneProvisionedTxnBudget is the tripwire of
// model.FlipCloneProvisioned, the clone-keyed provisioned flip (dnv-worker.md
// MD6): one STM reads every Clone its refs name and writes every one it
// flips, and the refs of one call can name every clone of the pool, which
// takes it past etcd's default cap — one of the transactions the deployment
// flag exists for. model/clonegrp_test.go's
// TestFlipCloneProvisionedAtTheCloneCeiling commits this shape against a real
// etcd.
func TestFlipCloneProvisionedTxnBudget(t *testing.T) {
	txnShape{
		name: "FlipCloneProvisioned",
		stm:  "model.FlipCloneProvisioned's STM and bumpSpRevTo",
		// SpRev.
		fixedReads: 1,
		// SpRev.
		fixedWrites: 1,
		terms: []txnTerm{
			// The Clone read and put.
			{"clone", 2, common.MaxCloneCntPerSp, 64},
		},
		maxCompares: 130,
		overDefault: true,
	}.check(t)
}

// TestStartCutoverTxnBudget is the tripwire of model.StartCutover, the sp
// worker's cutover (dnv-worker.md RW18, MD6): it reads one record of each
// kind it re-checks and writes two, whatever the pool's shape, so no ceiling
// is a factor. model/ops_test.go's TestStartCutover commits it against a real
// etcd.
func TestStartCutoverTxnBudget(t *testing.T) {
	txnShape{
		name: "StartCutover",
		stm:  "model.StartCutover's STM and BumpSpRev",
		// The SpConf (its level), the Migration, the dst side's Slice, the
		// reporting Cntlr and SpRev.
		fixedReads: 5,
		// The Migration and SpRev puts.
		fixedWrites: 2,
		maxCompares: 7,
		overDefault: false,
	}.check(t)
}
