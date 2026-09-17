package gateway

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is the CreateStoragePool half of what model/drain_test.go's
// TestDrainSpSliceAtTheCeiling is for the sp drain: the one create that is
// committed against a real etcd at the widest shape the ceilings allow. It
// sits apart from handler_sp_test.go only because it is the txn-budget proof
// rather than another §8.4 behaviour, and it builds on that file's fixtures
// (sptNewEnv, sptSpec, walkSides) rather than a second set of its own.

// TestCreateStoragePoolAtTheCeiling is the PROOF that
// TestCreateStoragePoolBudget can only approximate: one MAXIMUM-shape create —
// MaxSliceCntPerSp slices, spCreateGrpsPerSlice groups per slice
// (planSpGroups), md-raid1 so each group takes MaxAllocLegPerGrp legs, every
// side on a DN of its own, and MaxCntlrCntPerSp cntlrs on that many distinct
// CNs — committed against the real etcd this package runs with
// --max-txn-ops=common.EtcdMaxTxnOps (gateway/etcdenv_test.go's startEtcd,
// which reads the constant and never a literal).
//
// The tripwire counts the ops the transaction is BELIEVED to issue; only etcd
// can say how many it actually receives, because the commit carries one
// compare per key the STM read as well as one per key it wrote. If that grows
// — a new read or write in the create, a change in how etcdutil builds the txn
// — this test fails with etcd's own "too many operations in txn request" while
// the tripwire stays green, but only once the true count passes
// EtcdMaxTxnOps: 967 leaves 57 ops of slack, so an op added PER DN (+128) is
// caught here and a single fixed one is not.
//
// Mutation-checked when it was written: this create commits against an etcd
// started with --max-txn-ops=967 and is REFUSED at 966, so at this shape the
// tripwire's 967 is the transaction's exact compare count and not merely an
// upper bound on it. (The four-writes-per-DN factor is an upper bound in
// general — a DN that falls out of model.DnAllocatable under its charge costs
// three — but no DN in this fixture does, and the same holds of the CN twin,
// which is what the last assertion below checks rather than assumes.)
//
// It also exercises §6.5's distinct-NODE property at the CEILING rather than
// at the two slices TestCreateStoragePoolWriteSet uses, on both sides: the DN
// black list grows across groups, so the spCreateGrpsPerSlice x
// MaxSliceCntPerSp x MaxAllocLegPerGrp sides sit on that many DISTINCT disk
// nodes, and the CN black list grows across cntlrs, so the MaxCntlrCntPerSp
// cntlrs sit on that many distinct controller nodes. The budget arithmetic
// cannot check either — sharing a node would make the transaction smaller, not
// larger — which is exactly why both are asserted here. (The placement rule
// itself is TestCreateStoragePoolWriteSet's; what is this test's own is that
// the two counts are still the ones the 967 was computed from.)
//
// Two fixture facts this test depends on, and they fail DIFFERENTLY. Getting
// the DN one wrong can only refuse the create. Getting the CN one wrong can
// refuse it OR let it commit a SMALLER transaction and say nothing, which is
// the worse of the two and the reason cnFree is passed explicitly.
//
//   - sptNewEnv gives every DN a DISTINCT Location (its own addr_port), and
//     model.FindDnCandidates keeps at most one candidate per location per
//     scan. DNs planted with a shared location would yield ONE candidate per
//     scan, PickRandom would return 1 < MaxAllocLegPerGrp, and the first
//     md-raid1 group would be refused RESOURCE_EXHAUSTED — a failure that has
//     nothing to do with the transaction size.
//   - one cntlr's CN reserves the WHOLE SP (§6.5): footprint is the sum of
//     ext_cnt over every group, i.e. slice_cnt x (1 + init_ext_cnt). Only one
//     of the two ways that can go wrong is a refusal. At the fixture's own
//     sptInitExt of 4 the footprint is 160 against the stock sptCnFree of 64,
//     no CN clears the scan's free-extent filter and pickCn returns
//     RESOURCE_EXHAUSTED ("no controller node with 160 free extents");
//     init_ext_cnt = 1 brings the footprint to exactly 64, and THAT one
//     refuses nothing. The scan admits free == footprint and so does
//     cnLedger.charge, every CN lands on free_ext_cnt 0,
//     model.CnAllocatable is false there, cnCapacityKeyOf implies no key and
//     MaintainCnCapacity issues its del with no put — four writes for that CN
//     where spCreateWritesPerCn counts five. The create still commits; it is
//     one compare per CN smaller than the shape this file exists to prove —
//     963 at today's MaxCntlrCntPerSp of 4.
//     So cnFree is passed as FOUR times the footprint, and the capacity-key
//     assertion at the end is what keeps the number honest — a silent shrink
//     is exactly what the exactness claim above cannot survive.
//     init_ext_cnt is 1 because it does not enter the op count at all — it
//     moves ExtCnt VALUES, not key counts — so the cheapest legal value is
//     also the widest transaction.
func TestCreateStoragePoolAtTheCeiling(t *testing.T) {
	const dnCnt = spCreateGrpsPerSlice * common.MaxSliceCntPerSp *
		common.MaxAllocLegPerGrp
	const cnCnt = common.MaxCntlrCntPerSp
	const initExt = uint64(1)
	const footprint = uint64(common.MaxSliceCntPerSp) * (1 + initExt)
	const cnFree = 4 * footprint
	const spName = "ceiling0"

	env := sptNewEnv(t, dnCnt, cnCnt, cnFree)
	spec := sptSpec{
		name:     spName,
		cntlrCnt: common.MaxCntlrCntPerSp,
		sliceCnt: common.MaxSliceCntPerSp,
		initExt:  initExt,
		raid1:    true,
	}
	reply, err := env.srv.CreateStoragePool(env.ctx, spec.req(env.name))
	if err != nil {
		t.Fatalf("a maximum-shape create (%d slices over %d disk nodes and "+
			"%d controller nodes) did not commit: %v (an \"etcd: too many "+
			"operations in txn request\" here means common.EtcdMaxTxnOps no "+
			"longer covers one CreateStoragePool, and the arithmetic in "+
			"TestCreateStoragePoolBudget under-counts it)",
			common.MaxSliceCntPerSp, dnCnt, cnCnt, err)
	}

	// Read the SP back through §8.4's own read path, not out of the fixture:
	// what the transaction committed is what a caller can see.
	get, err := env.srv.GetStoragePool(env.ctx, &pb.GetStoragePoolRequest{
		ClusterName: env.name,
		SpName:      spName,
	})
	if err != nil {
		t.Fatalf("GetStoragePool: %v", err)
	}
	conf := get.GetSpConf()
	if conf.GetSpId() != reply.GetSpId() {
		t.Fatalf("sp_id: create returned %d, the store holds %d",
			reply.GetSpId(), conf.GetSpId())
	}
	if len(get.GetSliceList()) != common.MaxSliceCntPerSp ||
		len(conf.GetSliceIdList()) != common.MaxSliceCntPerSp {
		t.Fatalf("slice_list has %d entries over %d slice ids, want %d of "+
			"each: a create that committed a shape narrower than the ceiling "+
			"proves nothing about the ceiling",
			len(get.GetSliceList()), len(conf.GetSliceIdList()),
			common.MaxSliceCntPerSp)
	}
	if len(get.GetCntlrList()) != common.MaxCntlrCntPerSp {
		t.Fatalf("cntlr_list has %d entries, want %d",
			len(get.GetCntlrList()), common.MaxCntlrCntPerSp)
	}

	// §6.5 at the ceiling: one side per leg, one DN per side, no DN twice.
	sides := env.walkSides(conf)
	if len(sides) != dnCnt {
		t.Fatalf("the SP has %d sides, want %d = %d groups per slice x %d "+
			"slices x %d legs", len(sides), dnCnt, spCreateGrpsPerSlice,
			common.MaxSliceCntPerSp, common.MaxAllocLegPerGrp)
	}
	seen := make(map[string]struct{}, len(sides))
	for _, side := range sides {
		seen[side.AddrPort] = struct{}{}
	}
	if len(seen) != dnCnt {
		t.Fatalf("the %d sides sit on %d distinct disk nodes, want %d: the "+
			"growing black list of §6.5 no longer puts every leg of the "+
			"WHOLE sp on a DN of its own, and the transaction this test "+
			"commits is smaller than the one the budget is computed for",
			len(sides), len(seen), dnCnt)
	}

	// §6.5's CN half. A cntlr that shares its CN with another costs 7 compares
	// rather than 8: the ledger's three reads and four writes — CnConf, the
	// capacity key, CnRev — happen once for that CN instead of twice, while
	// the Cntlr put is per cntlr_id and happens either way.
	//
	// Unlike the DN arm above this one sits behind a random pick with very
	// little room in it: MaxCntlrCntPerSp cntlrs drawn from the
	// MaxCntlrCntPerSp CNs the fixture plants still come out all-distinct in
	// 24 of the 256 equally likely draws, so a create that lost its cnBlack
	// list passes here about one run in ten. Mutation-checked when it was
	// written: dropping the cnBlack append let the FIRST run pass and then
	// failed 8 of 8 under -count=8, which is what that one in ten looks like.
	// The DN arm has no such gap — 128 picks out of 128 DNs are never all
	// distinct by accident.
	seenCn := make(map[string]struct{}, cnCnt)
	for _, cntlr := range get.GetCntlrList() {
		seenCn[cntlr.GetAddrPort()] = struct{}{}
	}
	if len(seenCn) != cnCnt {
		t.Fatalf("the %d cntlrs sit on %d distinct controller nodes, want "+
			"%d: the cnBlack list of §6.5 no longer puts every cntlr of the "+
			"sp on a CN of its own, and the transaction this test commits is "+
			"smaller than the one the budget is computed for",
			len(get.GetCntlrList()), len(seenCn), cnCnt)
	}

	// And the OTHER way a CN can cost 7 compares instead of 8, the one no
	// count above would notice: a CN charged down to free_ext_cnt 0 leaves
	// model.CnAllocatable, implies no capacity key, and MaintainCnCapacity
	// issues its del with no put. cnFree is four times the footprint for
	// exactly this reason, so each of these CNs must still HAVE a capacity
	// key, at the free count the charge left it with.
	for _, cntlr := range get.GetCntlrList() {
		addrPort := cntlr.GetAddrPort()
		key := model.CnCapacityKey(env.cid, cnFree-footprint, addrPort)
		if !env.exists(key) {
			t.Errorf("cn %q has no capacity key at free_ext_cnt %d after a "+
				"charge of the sp's %d-extent footprint: if the fixture's "+
				"cnFree is the footprint itself the charge still succeeds, "+
				"but the CN ends allocatable=false and its capacity-key PUT "+
				"is dropped — %d compares, not the %d this file proves",
				addrPort, cnFree-footprint, footprint,
				spCreateMaxCompares-cnCnt, spCreateMaxCompares)
		}
	}
}
