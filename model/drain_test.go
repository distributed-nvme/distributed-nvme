package model

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The sp drain's own suite (dnv-worker.md §11.6 and §13, model half).
//
// Two things are deliberately pinned in BOTH directions here, because one
// direction alone passes a broken implementation:
//
//   - the SPD2 guards: an op that refused everything would pass a test that
//     only checked the refusals, and one that refused nothing would pass a test
//     that only checked the happy path, so every guard row is paired with a
//     "and the same op on a correctly latched SP commits";
//   - the slice-final STM: a batch that deleted the key without shrinking
//     slice_id_list would break every later LoadSp, and one that shrank the
//     list without deleting the key would strand a slice for ever, so the two
//     are asserted together and never separately.

// The wide-slice fixture. Its ids sit far above opsSlice()'s so a leaked id
// from one is obvious in the other's failure message.
const (
	drainDataGrpBase  = uint64(5000)
	drainMetaGrpBase  = uint64(5500)
	drainDataSideBase = uint64(7000)
	drainMetaSideBase = uint64(7500)
	drainLegBase      = uint64(6000)
	// One extent per group, so a DN's budget arithmetic is the group COUNT and
	// a miscounted pop shows up as an off-by-one rather than as a multiple.
	drainExtCnt = uint64(1)
)

// drainGrp builds one single-leg, single-side group of drainExtCnt extents.
func drainGrp(grpId uint64, sideId uint64, addrPort string) *pb.Group {
	return &pb.Group{
		GrpId:      grpId,
		ExtCnt:     drainExtCnt,
		MetaBlocks: 3,
		DataBlocks: 1021,
		LegList: []*pb.Leg{{
			LegId:    drainLegBase + sideId,
			SideList: []*pb.Side{opsSide(sideId, addrPort, true)},
		}},
	}
}

// putWideSlice replaces the fixture's slice with one carrying dataCnt data
// groups and metaCnt meta groups, every side on addrPort, and charges that DN
// for all of them exactly as a create would have: the pointers in
// `side_ptr_list`, the extents out of `free_ext_cnt`, the capacity key moved
// (§5.6).
//
// Every side on ONE DN is the point, not a shortcut: it is what makes the
// per-node accounting of a multi-group batch observable as a single write and a
// single DnRev bump.
func (e *opsEnv) putWideSlice(
	dataCnt int,
	metaCnt int,
	addrPort string,
) *pb.Slice {
	e.t.Helper()
	slice := &pb.Slice{SliceIdx: 0}
	var ptrs []*pb.SidePointer
	add := func(grpId uint64, sideId uint64, meta bool) {
		grp := drainGrp(grpId, sideId, addrPort)
		if meta {
			slice.MetaGrpList = append(slice.MetaGrpList, grp)
		} else {
			slice.DataGrpList = append(slice.DataGrpList, grp)
		}
		ptrs = append(ptrs, &pb.SidePointer{
			SpId:   opsSpId,
			LegId:  drainLegBase + sideId,
			SideId: sideId,
		})
	}
	for idx := 0; idx < metaCnt; idx++ {
		add(drainMetaGrpBase+uint64(idx), drainMetaSideBase+uint64(idx), true)
	}
	for idx := 0; idx < dataCnt; idx++ {
		add(drainDataGrpBase+uint64(idx), drainDataSideBase+uint64(idx), false)
	}
	mustPut(e.t, e.cli, SliceKey(e.cid, opsSpId, opsSliceId), slice)
	e.chargeDn(addrPort, ptrs, uint64(dataCnt+metaCnt)*drainExtCnt)
	return slice
}

// chargeDn rewrites one DN as a create that placed those sides would have left
// it, moving its capacity key with the free count it embeds (§5.6).
func (e *opsEnv) chargeDn(
	addrPort string,
	ptrs []*pb.SidePointer,
	extCnt uint64,
) {
	e.t.Helper()
	dn := e.dn(addrPort)
	oldFree := dn.GetFreeExtCnt()
	e.delKey(DnCapacityKey(
		e.cid, mustBin(e.t, e, oldFree), oldFree, addrPort))
	dn.SidePtrList = ptrs
	dn.FreeExtCnt = oldFree - extCnt
	mustPut(e.t, e.cli, DnConfKey(e.cid, addrPort), dn)
	newFree := dn.GetFreeExtCnt()
	mustPut(e.t, e.cli,
		DnCapacityKey(e.cid, mustBin(e.t, e, newFree), newFree, addrPort),
		&pb.DnCapacity{Location: addrPort})
}

// chargeCn charges one CN for one cntlr of the fixture SP: the pointer in
// `cntlr_ptr_list`, the SP's whole footprint out of `free_ext_cnt` (§6.5), the
// capacity key moved with the free count it embeds (§5.6).
func (e *opsEnv) chargeCn(addrPort string, cntlrId uint64) {
	e.t.Helper()
	cn := e.cn(addrPort)
	oldFree := cn.GetFreeExtCnt()
	e.delKey(CnCapacityKey(e.cid, oldFree, addrPort))
	cn.CntlrPtrList = append(cn.GetCntlrPtrList(),
		&pb.CntlrPointer{SpId: opsSpId, CntlrId: cntlrId})
	cn.FreeExtCnt = oldFree - opsFootprint
	mustPut(e.t, e.cli, CnConfKey(e.cid, addrPort), cn)
	mustPut(e.t, e.cli,
		CnCapacityKey(e.cid, cn.GetFreeExtCnt(), addrPort),
		&pb.CnCapacity{Location: addrPort})
}

// seedSpGlobal writes the cluster's SpGlobal with this SP's bucket slot taken,
// as the create the fixture stands for left it. newOpsEnv writes none — no
// other op reads it — and D3 applies GW12's deletion half to it.
func (e *opsEnv) seedSpGlobal() {
	e.t.Helper()
	bucket := make([]uint32, common.ShardBucketSize)
	bucket[opsShard] = 1
	mustPut(e.t, e.cli, SpGlobalKey(e.cid),
		&pb.SpGlobal{NextId: opsSpId + 1, ShardBucket: bucket})
}

// chargeFixture charges the fixture's nodes exactly as the create that built
// opsSlice() would have: each of dn-a and dn-b carries one leg's meta side and
// one leg's data side, opsFootprint extents in all, and each of cn-a and cn-b
// carries one cntlr and therefore the SP's whole footprint.
//
// newOpsEnv deliberately leaves the nodes UNcharged — every other op only reads
// them — so a drain test that asserts the budgets came back exactly has to put
// them where a create would have.
func (e *opsEnv) chargeFixture() {
	e.t.Helper()
	e.chargeDn(opsDnA, []*pb.SidePointer{
		{SpId: opsSpId, LegId: opsMetaLegA, SideId: opsMetaSideA},
		{SpId: opsSpId, LegId: opsDataLegA, SideId: opsDataSideA},
	}, opsFootprint)
	e.chargeDn(opsDnB, []*pb.SidePointer{
		{SpId: opsSpId, LegId: opsMetaLegB, SideId: opsMetaSideB},
		{SpId: opsSpId, LegId: opsDataLegB, SideId: opsDataSideB},
	}, opsFootprint)
	e.chargeCn(opsCnA, opsCntlrA)
	e.chargeCn(opsCnB, opsCntlrB)
	e.seedSpGlobal()
}

// latchForDrain puts the fixture SP in the state D2 and D3 run against: the
// flag set (SPD3's latch), the cntlr list already emptied (D1's committed
// effect) and the SpGlobal seeded so D3 has a bucket to release. It exists so
// that the slice tests cannot pass by accident on an SP whose cntlrs are still
// stacking it.
func (e *opsEnv) latchForDrain() {
	e.t.Helper()
	conf := e.spConf()
	conf.Deleting = true
	conf.CntlrIdList = nil
	mustPut(e.t, e.cli, SpConfKey(e.cid, opsSpName), conf)
	e.seedSpGlobal()
}

// grpIds is the id list of a group list, in stored order — what the tail-pop
// assertions compare against.
func grpIds(grps []*pb.Group) []uint64 {
	ids := make([]uint64, 0, len(grps))
	for _, grp := range grps {
		ids = append(ids, grp.GetGrpId())
	}
	return ids
}

// equalIds compares two id lists element by element.
func equalIds(got []uint64, want []uint64) bool {
	if len(got) != len(want) {
		return false
	}
	for idx := range got {
		if got[idx] != want[idx] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// D1 — DrainSpCntlrs (SPD9)
// ---------------------------------------------------------------------------

// TestDrainSpCntlrs is SPD9's whole effect in one transaction: every cntlr key
// gone, the list emptied, each cntlr's CN given the SP's WHOLE footprint back
// with its capacity key moved and its revision bumped exactly once, and one
// SpRev bump for the lot.
//
// The footprint is asserted as opsFootprint — the Σ ext_cnt of the fixture's
// groups — rather than as a per-cntlr share, because §6.5 charges every cntlr
// the whole SP and a release that returned a share would leave every CN of a
// multi-cntlr SP permanently short.
func TestDrainSpCntlrs(t *testing.T) {
	env := newOpsEnv(t)
	env.chargeFixture()
	env.setDeleting()
	before := struct{ spRev, cnARev, cnBRev uint64 }{
		env.spRev(), env.cnRev(opsCnA), env.cnRev(opsCnB),
	}

	removed, err := DrainSpCntlrs(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName)
	if err != nil {
		t.Fatalf("DrainSpCntlrs: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d cntlrs, want 2", removed)
	}
	for _, cntlrId := range []uint64{opsCntlrA, opsCntlrB} {
		if env.exists(CntlrKey(env.cid, opsSpId, cntlrId)) {
			t.Errorf("cntlr %d survived D1", cntlrId)
		}
	}
	conf := env.spConf()
	if len(conf.GetCntlrIdList()) != 0 {
		t.Errorf("cntlr_id_list = %v, want empty", conf.GetCntlrIdList())
	}
	// D1 touches nothing else about the SP: the slices are D2's.
	if len(conf.GetSliceIdList()) != 1 {
		t.Errorf("slice_id_list = %v, want the fixture's",
			conf.GetSliceIdList())
	}
	if !env.exists(SliceKey(env.cid, opsSpId, opsSliceId)) {
		t.Errorf("D1 deleted a slice key")
	}
	for _, tc := range []struct {
		addrPort string
		revStart uint64
	}{{opsCnA, before.cnARev}, {opsCnB, before.cnBRev}} {
		cn := env.cn(tc.addrPort)
		if cn.GetFreeExtCnt() != opsCnFree {
			t.Errorf("cn %q: free_ext_cnt %d, want the whole footprint back "+
				"(%d)", tc.addrPort, cn.GetFreeExtCnt(), opsCnFree)
		}
		if len(cn.GetCntlrPtrList()) != 0 {
			t.Errorf("cn %q: cntlr_ptr_list %v",
				tc.addrPort, cn.GetCntlrPtrList())
		}
		if !env.exists(CnCapacityKey(env.cid, opsCnFree, tc.addrPort)) {
			t.Errorf("cn %q: no capacity key for the restored free count",
				tc.addrPort)
		}
		if env.exists(CnCapacityKey(
			env.cid, opsCnFree-opsFootprint, tc.addrPort,
		)) {
			t.Errorf("cn %q: the charged capacity key survived", tc.addrPort)
		}
		if got := env.cnRev(tc.addrPort); got != tc.revStart+1 {
			t.Errorf("cn %q: revision %d, want %d (exactly one bump)",
				tc.addrPort, got, tc.revStart+1)
		}
	}
	if got := env.spRev(); got != before.spRev+1 {
		t.Errorf("SpRev: got %d, want %d (exactly one bump)",
			got, before.spRev+1)
	}
}

// TestDrainSpCntlrsBumpsASharedCnOnce is releaseSpCns's reason for existing:
// two cntlrs of one SP on one CN — a shape §6.4 forbids but nothing in the
// store enforces — give the footprint back TWICE and cost one write, one
// capacity-key maintenance and one CnRev bump.
func TestDrainSpCntlrsBumpsASharedCnOnce(t *testing.T) {
	env := newOpsEnv(t)
	env.setDeleting()
	// Both cntlrs onto cn-a.
	cntlr := env.cntlr(opsCntlrB)
	cntlr.AddrPort = opsCnA
	cntlr.NvmeTrConf = opsTrConf(opsCnA)
	mustPut(t, env.cli, CntlrKey(env.cid, opsSpId, opsCntlrB), cntlr)
	cn := env.cn(opsCnA)
	cn.FreeExtCnt = opsCnFree - 2*opsFootprint
	cn.CntlrPtrList = []*pb.CntlrPointer{
		{SpId: opsSpId, CntlrId: opsCntlrA},
		{SpId: opsSpId, CntlrId: opsCntlrB},
	}
	mustPut(t, env.cli, CnConfKey(env.cid, opsCnA), cn)
	env.delKey(CnCapacityKey(env.cid, opsCnFree, opsCnA))
	mustPut(t, env.cli,
		CnCapacityKey(env.cid, opsCnFree-2*opsFootprint, opsCnA),
		&pb.CnCapacity{Location: opsCnA})
	revBefore := env.cnRev(opsCnA)

	if _, err := DrainSpCntlrs(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
	); err != nil {
		t.Fatalf("DrainSpCntlrs: %v", err)
	}
	got := env.cn(opsCnA)
	if got.GetFreeExtCnt() != opsCnFree {
		t.Errorf("cn-a: free_ext_cnt %d, want %d", got.GetFreeExtCnt(), opsCnFree)
	}
	if len(got.GetCntlrPtrList()) != 0 {
		t.Errorf("cn-a: cntlr_ptr_list %v", got.GetCntlrPtrList())
	}
	if rev := env.cnRev(opsCnA); rev != revBefore+1 {
		t.Errorf("cn-a carried two cntlrs: revision %d, want %d (one bump)",
			rev, revBefore+1)
	}
	if !env.exists(CnCapacityKey(env.cid, opsCnFree, opsCnA)) {
		t.Errorf("cn-a: no capacity key for the restored free count")
	}
	if env.exists(CnCapacityKey(env.cid, opsCnFree-2*opsFootprint, opsCnA)) {
		t.Errorf("cn-a: the charged capacity key survived")
	}
}

// ---------------------------------------------------------------------------
// D2 — DrainSpSlice (SPD10, SPD11)
// ---------------------------------------------------------------------------

// TestDrainSpSliceBatchSizeAndOrder is SPD10's two rules at once, on a slice
// far wider than one batch: a batch removes at most common.MaxDelGrpPerTxn
// groups, and it takes them from the TAIL of data_grp_list first and only then
// from the tail of meta_grp_list.
//
// The order matters beyond determinism. The position of a group in its list and
// the leg_idx inside it are the md member order, so a head-pop would renumber
// every surviving group of a slice that is still being read — and a
// meta-before-data pop would leave a slice whose thin pool has data groups and
// no metadata to address them with, which is a worse intermediate state than
// the reverse.
func TestDrainSpSliceBatchSizeAndOrder(t *testing.T) {
	const dataCnt = common.MaxDelGrpPerTxn + 3
	const metaCnt = 4
	env := newOpsEnv(t)
	env.latchForDrain()
	before := env.putWideSlice(dataCnt, metaCnt, opsDnA)
	wantData := grpIds(before.GetDataGrpList())
	wantMeta := grpIds(before.GetMetaGrpList())
	revBefore := env.spRev()

	// Batch 1: the budget is spent entirely on the DATA tail, and no meta
	// group is touched even though the data list is not exhausted.
	removed, done, err := DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, opsSliceId, env.cc)
	if err != nil {
		t.Fatalf("DrainSpSlice batch 1: %v", err)
	}
	// A NON-final batch bumps too (SPD11). Every other bump assertion in this
	// file runs a batch that finishes its slice, so without this one a
	// DrainSpSlice that bumped only on `done` would go unnoticed — and the
	// bump is what re-fires the coordinator's fan-out, which is what stops the
	// side children of the groups the batch just released.
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("a non-final batch must bump SpRev: got %d, want %d",
			got, revBefore+1)
	}
	if removed != common.MaxDelGrpPerTxn {
		t.Errorf("batch 1 removed %d groups, want %d",
			removed, common.MaxDelGrpPerTxn)
	}
	if done {
		t.Errorf("batch 1 must not finish a %d-group slice", dataCnt+metaCnt)
	}
	slice := env.slice()
	if got := grpIds(slice.GetDataGrpList()); !equalIds(
		got, wantData[:dataCnt-common.MaxDelGrpPerTxn],
	) {
		t.Errorf("after batch 1 data_grp_list = %v, want the HEAD %v kept",
			got, wantData[:dataCnt-common.MaxDelGrpPerTxn])
	}
	if got := grpIds(slice.GetMetaGrpList()); !equalIds(got, wantMeta) {
		t.Errorf("batch 1 touched meta_grp_list: %v, want %v", got, wantMeta)
	}

	// Batch 2: the data remainder goes first, and only then does the budget
	// spill into the meta tail — within ONE transaction.
	removed, done, err = DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, opsSliceId, env.cc)
	if err != nil {
		t.Fatalf("DrainSpSlice batch 2: %v", err)
	}
	if removed != dataCnt-common.MaxDelGrpPerTxn+metaCnt {
		t.Errorf("batch 2 removed %d groups, want %d",
			removed, dataCnt-common.MaxDelGrpPerTxn+metaCnt)
	}
	if !done {
		t.Errorf("batch 2 emptied the slice and must report it done")
	}
	if env.exists(SliceKey(env.cid, opsSpId, opsSliceId)) {
		t.Errorf("the emptied slice key survived")
	}
}

// TestDrainSpSliceFinalRemovesKeyAndIdTogether is SPD11: the batch that empties
// a slice deletes the `slice` key AND the id from slice_id_list in the SAME
// transaction, and a batch that does not empty it does neither.
//
// Both halves are asserted because either one alone is a live bug. A key
// deleted with the id still listed breaks every later model.LoadSp, which
// fetches children by iterating the id lists; an id removed with the key still
// there strands a slice nothing will ever read again, holding the DN extents it
// describes for ever.
func TestDrainSpSliceFinalRemovesKeyAndIdTogether(t *testing.T) {
	env := newOpsEnv(t)
	env.latchForDrain()
	env.putWideSlice(common.MaxDelGrpPerTxn+1, 0, opsDnA)

	// The non-final batch: neither half moves.
	if _, done, err := DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, opsSliceId, env.cc); err != nil || done {
		t.Fatalf("batch 1: done=%v err=%v, want false/nil", done, err)
	}
	if !env.exists(SliceKey(env.cid, opsSpId, opsSliceId)) {
		t.Errorf("a non-final batch deleted the slice key")
	}
	if got := env.spConf().GetSliceIdList(); !equalIds(
		got, []uint64{opsSliceId},
	) {
		t.Errorf("a non-final batch changed slice_id_list to %v", got)
	}

	// The final batch: both halves move.
	if _, done, err := DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, opsSliceId, env.cc); err != nil || !done {
		t.Fatalf("batch 2: done=%v err=%v, want true/nil", done, err)
	}
	if env.exists(SliceKey(env.cid, opsSpId, opsSliceId)) {
		t.Errorf("the final batch left the slice key behind")
	}
	if got := env.spConf().GetSliceIdList(); len(got) != 0 {
		t.Errorf("the final batch left slice_id_list = %v", got)
	}
	// And the SP is now in D3's state, which is the only thing that makes the
	// drain terminate.
	if err := FinishSpDelete(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
	); err != nil {
		t.Fatalf("FinishSpDelete after the last slice: %v", err)
	}
}

// TestDrainSpSliceAccounting is the per-node half of §5.5 and §5.6 inside one
// batch: the DN carrying every side of the batch is written once, has its
// capacity key MOVED (old gone, new there) and its revision bumped exactly
// once, whatever the group count.
func TestDrainSpSliceAccounting(t *testing.T) {
	const grpCnt = 5
	env := newOpsEnv(t)
	env.latchForDrain()
	env.putWideSlice(grpCnt, 0, opsDnA)
	charged := opsDnFree - grpCnt*drainExtCnt
	if got := env.dn(opsDnA).GetFreeExtCnt(); got != charged {
		t.Fatalf("fixture dn-a: free %d, want %d", got, charged)
	}
	// A pointer of ANOTHER sp that collides on side_id. Side ids come from
	// SpConf.next_id, which starts at SpFirstId for every SP independently, so
	// two SPs' first sides are both side_id 1 — and one DN's side_ptr_list
	// carries entries from every SP that placed a leg on it. The drain walks
	// every side of its SP, so a release that matched on side_id alone would
	// strip the OTHER SP's pointers off every shared DN, leaving its sides
	// unreferenced to the orphan sweep and to DeleteDiskNode's "still has
	// sides" gate. Nothing else in this suite distinguishes the two
	// predicates: every other fixture puts one SP's sides on a DN.
	foreign := &pb.SidePointer{
		SpId:   opsSpId + 1,
		LegId:  drainLegBase + drainDataSideBase,
		SideId: drainDataSideBase,
	}
	dn := env.dn(opsDnA)
	dn.SidePtrList = append(dn.GetSidePtrList(), foreign)
	mustPut(t, env.cli, DnConfKey(env.cid, opsDnA), dn)
	before := struct{ spRev, dnRev uint64 }{env.spRev(), env.dnRev(opsDnA)}

	if _, _, err := DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, opsSliceId, env.cc); err != nil {
		t.Fatalf("DrainSpSlice: %v", err)
	}
	got := env.dn(opsDnA)
	if got.GetFreeExtCnt() != opsDnFree {
		t.Errorf("dn-a: free_ext_cnt %d, want %d",
			got.GetFreeExtCnt(), opsDnFree)
	}
	if len(got.GetSidePtrList()) != 1 ||
		got.GetSidePtrList()[0].GetSpId() != foreign.GetSpId() {
		t.Errorf("dn-a: side_ptr_list %v, want only the other SP's pointer",
			got.GetSidePtrList())
	}
	if got := env.dnRev(opsDnA); got != before.dnRev+1 {
		t.Errorf("dn-a carried %d sides: revision %d, want %d (one bump)",
			grpCnt, got, before.dnRev+1)
	}
	if !env.exists(DnCapacityKey(
		env.cid, mustBin(t, env, opsDnFree), opsDnFree, opsDnA)) {
		t.Errorf("dn-a: no capacity key for the restored free count")
	}
	if env.exists(DnCapacityKey(
		env.cid, mustBin(t, env, charged), charged, opsDnA)) {
		t.Errorf("dn-a: the charged capacity key survived")
	}
	if got := env.spRev(); got != before.spRev+1 {
		t.Errorf("SpRev: got %d, want %d (exactly one bump)",
			got, before.spRev+1)
	}
}

// TestDrainSpSliceReleasesSpareLegs pins that a spare leg's side is released
// like an active one (§8.12): it occupies a DN exactly the same way, so a walk
// that only visited leg_list would leak an extent per spare for ever.
func TestDrainSpSliceReleasesSpareLegs(t *testing.T) {
	env := newOpsEnv(t)
	env.latchForDrain()
	// The fixture slice plus a spare on dn-c, charged as CreateSpareLeg would
	// have charged it.
	env.addSpare(true)
	env.chargeDn(opsDnC, []*pb.SidePointer{{
		SpId: opsSpId, LegId: opsSpareLeg, SideId: opsSpareSide,
	}}, opsDataExtCnt)
	charged := opsDnFree - opsDataExtCnt
	revBefore := env.dnRev(opsDnC)

	if _, done, err := DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, opsSliceId, env.cc); err != nil || !done {
		t.Fatalf("DrainSpSlice: done=%v err=%v", done, err)
	}
	dn := env.dn(opsDnC)
	if dn.GetFreeExtCnt() != opsDnFree {
		t.Errorf("dn-c: free_ext_cnt %d, want the spare's %d extents back "+
			"(%d)", dn.GetFreeExtCnt(), opsDataExtCnt, opsDnFree)
	}
	if len(dn.GetSidePtrList()) != 0 {
		t.Errorf("dn-c: side_ptr_list %v", dn.GetSidePtrList())
	}
	if got := env.dnRev(opsDnC); got != revBefore+1 {
		t.Errorf("dn-c: revision %d, want %d", got, revBefore+1)
	}
	if env.exists(DnCapacityKey(
		env.cid, mustBin(t, env, charged), charged, opsDnC)) {
		t.Errorf("dn-c: the spare's capacity key survived")
	}
}

// TestDrainSpSliceRefusesWhileCntlrsRemain is SPD8's order made a precondition
// of the transaction: a caller that derived D2 from a stale snapshot must not
// start popping groups while cntlrs are still stacking the whole SP.
//
// Cntlrs-first is not cosmetic (SPD9). Every cntlr stacks the WHOLE SP on its
// CN and the coordinator keeps syncing during the drain, so a slices-first
// drain would make every CN reload its pool concats and disband md arrays on
// every batch, racing the DN export teardown each time, for stacks nothing will
// ever use.
func TestDrainSpSliceRefusesWhileCntlrsRemain(t *testing.T) {
	env := newOpsEnv(t)
	env.chargeFixture()
	env.setDeleting() // latched, but the cntlr list is untouched
	revBefore := env.spRev()
	_, _, err := DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, opsSliceId, env.cc)
	precondition := wantPrecondition(t, err, opDrainSpSlice)
	if precondition.Reason != "cntlrs not drained" {
		t.Errorf("Reason: got %q, want %q",
			precondition.Reason, "cntlrs not drained")
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("an aborted batch bumped SpRev to %d", got)
	}
	if !env.exists(SliceKey(env.cid, opsSpId, opsSliceId)) {
		t.Errorf("an aborted batch deleted the slice key")
	}
}

// TestDrainSliceRefusesAnInvalidStoredConf is the §7 gate DrainSpSlice needs
// for MaintainDnCapacity's sake, and the one the gateway's
// TestStoredClusterConfZeroIsRefusedByEveryReader used to reach through
// DeleteStoragePool's newDnLedger.
//
// A capacity key embeds the BIN INDEX the stored ladder yields, so a conf
// CreateCluster could not have written names a key nothing ever wrote: the
// release would leave the live key behind — still indexing a free count the DN
// no longer has, and still offered by the §6.3 scan — and write a new one under
// a bin its free count does not belong to. There is no right index to compute
// from such a conf, so the batch refuses instead of guessing.
func TestDrainSliceRefusesAnInvalidStoredConf(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(cc *pb.ClusterConf)
		reason string
	}{
		{"extent_size", func(cc *pb.ClusterConf) {
			cc.DnBinConf.ExtentSize = 0
		}, "invalid stored conf: dn_bin_conf.extent_size is zero"},
		{"dn_batch_size", func(cc *pb.ClusterConf) {
			cc.AllocConf.DnBatchSize = 0
		}, "invalid stored conf: alloc_conf.dn_batch_size 0 is outside [1, 1024]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			env.latchForDrain()
			env.putWideSlice(2, 0, opsDnA)
			revBefore := env.spRev()
			tc.mutate(env.cc)
			_, _, err := DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, opsSliceId, env.cc)
			precondition := wantPrecondition(t, err, opDrainSpSlice)
			if precondition.Reason != tc.reason {
				t.Errorf("Reason: got %q, want %q",
					precondition.Reason, tc.reason)
			}
			if got := env.spRev(); got != revBefore {
				t.Errorf("a refused batch bumped SpRev to %d", got)
			}
			if got := env.dn(opsDnA).GetFreeExtCnt(); got != opsDnFree-2 {
				t.Errorf("a refused batch credited dn-a: free %d", got)
			}
			// Repaired, the very same batch commits: the gate is the conf and
			// nothing else.
			env.cc = testClusterConf()
			env.cc.DnBinConf.ExtentSize = opsExtSize
			if _, _, err := DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, opsSliceId, env.cc); err != nil {
				t.Fatalf("DrainSpSlice with the conf repaired: %v", err)
			}
		})
	}
}

// TestDrainSpSliceAtTheCeiling is SPD14's PROOF, the half the arithmetic
// tripwire in gateway/txnbudget_test.go cannot do: one MAXIMUM-shape batch —
// MaxDelGrpPerTxn groups, each with MaxAllocLegPerGrp legs and
// MaxSpareLegPerGrp spares, every side on a DN of its own, so the batch touches
// the most distinct DNs a batch ever can — committed against the real etcd this
// package runs with --max-txn-ops=common.EtcdMaxTxnOps.
//
// The tripwire counts the ops the transaction is BELIEVED to issue; only etcd
// can say how many it actually receives, because the commit also carries one
// compare per key the STM read. If that grows — a new write in the batch, a
// change in how etcdutil builds the txn — this test fails with etcd's own "too
// many operations in txn request" while the tripwire stays green, but only
// once the true count passes EtcdMaxTxnOps: 486 leaves 26 ops of slack, so an
// op added PER DN (+80) is caught here and a single fixed one is not.
func TestDrainSpSliceAtTheCeiling(t *testing.T) {
	const legsPerGrp = common.MaxAllocLegPerGrp + common.MaxSpareLegPerGrp
	const dnCnt = common.MaxDelGrpPerTxn * legsPerGrp
	env := newOpsEnv(t)
	env.latchForDrain()

	slice := &pb.Slice{SliceIdx: 0}
	addrs := make([]string, 0, dnCnt)
	for grpIdx := 0; grpIdx < common.MaxDelGrpPerTxn; grpIdx++ {
		grp := &pb.Group{
			GrpId:      drainDataGrpBase + uint64(grpIdx),
			ExtCnt:     drainExtCnt,
			MetaBlocks: 3,
			DataBlocks: 1021,
		}
		for legIdx := 0; legIdx < legsPerGrp; legIdx++ {
			idx := grpIdx*legsPerGrp + legIdx
			addrPort := fmt.Sprintf("drain-dn-%03d:9000", idx)
			addrs = append(addrs, addrPort)
			// One DN per side and one side pointer per DN: the widest D2 batch
			// there is, which is what SPD13's budget is computed for.
			env.putDn(addrPort, uint64(900+idx), uint32(idx%256), opsDnFree)
			sideId := drainDataSideBase + uint64(idx)
			env.chargeDn(addrPort, []*pb.SidePointer{{
				SpId:   opsSpId,
				LegId:  drainLegBase + sideId,
				SideId: sideId,
			}}, drainExtCnt)
			leg := &pb.Leg{
				LegId:    drainLegBase + sideId,
				LegIdx:   uint32(legIdx),
				SideList: []*pb.Side{opsSide(sideId, addrPort, true)},
			}
			if legIdx < common.MaxAllocLegPerGrp {
				grp.LegList = append(grp.LegList, leg)
			} else {
				grp.SpareLegList = append(grp.SpareLegList, leg)
			}
		}
		slice.DataGrpList = append(slice.DataGrpList, grp)
	}
	mustPut(t, env.cli, SliceKey(env.cid, opsSpId, opsSliceId), slice)

	removed, done, err := DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, opsSliceId, env.cc)
	if err != nil {
		t.Fatalf("a maximum-shape batch (%d groups over %d disk nodes) did "+
			"not commit: %v (an \"too many operations in txn request\" here "+
			"means common.EtcdMaxTxnOps no longer covers one D2 batch)",
			common.MaxDelGrpPerTxn, dnCnt, err)
	}
	if removed != common.MaxDelGrpPerTxn || !done {
		t.Fatalf("removed=%d done=%v, want %d/true",
			removed, done, common.MaxDelGrpPerTxn)
	}
	for _, addrPort := range addrs {
		dn := env.dn(addrPort)
		if dn.GetFreeExtCnt() != opsDnFree {
			t.Fatalf("dn %q: free_ext_cnt %d, want %d",
				addrPort, dn.GetFreeExtCnt(), opsDnFree)
		}
		if len(dn.GetSidePtrList()) != 0 {
			t.Fatalf("dn %q: side_ptr_list %v", addrPort, dn.GetSidePtrList())
		}
	}
}

// ---------------------------------------------------------------------------
// D3 — FinishSpDelete (SPD12)
// ---------------------------------------------------------------------------

// TestFinishSpDelete is SPD12: the last three keys and GW12's deletion half, in
// one transaction that deliberately does NOT bump — it deletes the rev key,
// which is the shard worker's stop signal, so the drain terminates itself in
// the same transaction that finishes the job.
func TestFinishSpDelete(t *testing.T) {
	env := newOpsEnv(t)
	env.latchForDrain()
	conf := env.spConf()
	conf.SliceIdList = nil
	mustPut(t, env.cli, SpConfKey(env.cid, opsSpName), conf)
	env.delKey(SliceKey(env.cid, opsSpId, opsSliceId))

	if err := FinishSpDelete(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
	); err != nil {
		t.Fatalf("FinishSpDelete: %v", err)
	}
	for _, key := range []string{
		SpConfKey(env.cid, opsSpName),
		SpNameKey(env.cid, opsSpId),
		SpRevKey(opsShard, env.cid, opsSpId),
	} {
		if env.exists(key) {
			t.Errorf("%q survived D3", key)
		}
	}
	global := &pb.SpGlobal{}
	env.get(SpGlobalKey(env.cid), global)
	if global.GetShardBucket()[opsShard] != 0 {
		t.Errorf("shard bucket slot %d = %d, want 0 (GW12's deletion half)",
			opsShard, global.GetShardBucket()[opsShard])
	}
	if global.GetNextId() != opsSpId+1 {
		t.Errorf("next_id rewound to %d: a deleted sp_id must never come back",
			global.GetNextId())
	}
}

// TestFinishSpDeleteRefusesWhileAnythingSurvives is the guard that makes the
// drain safe under the accepted two-owner overlap: an owner whose snapshot is
// one step behind must not skip to the end and strand the keys the other owner
// is still popping.
//
// Both survivors are rowed, not one standing for the other: a guard that only
// looked at the cntlr list would let a slices-remaining SP through, and vice
// versa, and each of those leaks a different family of keys.
func TestFinishSpDeleteRefusesWhileAnythingSurvives(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(env *opsEnv)
		reason string
	}{
		{"cntlrs remain", func(env *opsEnv) {
			// Latched with the cntlr list untouched, and no slices, so only
			// the cntlr guard can refuse it.
			conf := env.spConf()
			conf.Deleting = true
			conf.SliceIdList = nil
			mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
			env.seedSpGlobal()
		}, "cntlrs remain"},
		{"slices remain", func(env *opsEnv) {
			env.latchForDrain()
		}, "slices remain"},
		{"sp rev not found", func(env *opsEnv) {
			// D3 READS the rev key it is about to delete, so a concurrent bump
			// fails this transaction's compare instead of being silently
			// overwritten by the delete. Its absence is a lost invariant key.
			env.latchForDrain()
			conf := env.spConf()
			conf.SliceIdList = nil
			mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
			env.delKey(SpRevKey(opsShard, env.cid, opsSpId))
		}, "sp rev not found"},
		{"sp global not found", func(env *opsEnv) {
			// Read before the first Del, so this refusal returns having staged
			// nothing — the bucket decrement and the key deletes are one unit.
			env.latchForDrain()
			conf := env.spConf()
			conf.SliceIdList = nil
			mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
			env.delKey(SpGlobalKey(env.cid))
		}, "sp global not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			tc.setup(env)
			err := FinishSpDelete(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName)
			precondition := wantPrecondition(t, err, opFinishSpDelete)
			if precondition.Reason != tc.reason {
				t.Errorf("Reason: got %q, want %q",
					precondition.Reason, tc.reason)
			}
			if !env.exists(SpConfKey(env.cid, opsSpName)) {
				t.Errorf("a refused D3 deleted sp_conf")
			}
			if !env.exists(SpNameKey(env.cid, opsSpId)) {
				t.Errorf("a refused D3 deleted sp_id_to_name")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SPD2 — the load-and-refuse guard, both directions
// ---------------------------------------------------------------------------

// TestDrainOpsRefuseAnUnlatchedSp is SPD2, mutation-tested across all three
// ops and all three conditions.
//
// It is the inverse of loadSpConfForOp's "sp deleting" refusal: normal ops
// require the flag CLEAR, drain ops require it SET. Every row is an ERROR and
// never a skip, and that is the whole point — a reconcile loop that reads
// absence as permission is how the wrong thing gets destroyed. An SpConf that
// is missing, or whose sp_id moved because the name was deleted and re-created
// under it, describes a DIFFERENT storage pool, and "carry on, the keys this op
// would remove are not there any more" is exactly the reasoning that removes
// someone else's.
//
// The `commits` half of each row is what stops the guard from being satisfied
// by an op that refuses everything.
func TestDrainOpsRefuseAnUnlatchedSp(t *testing.T) {
	ops := []struct {
		name string
		op   string
		run  func(env *opsEnv) error
		// ready puts the SP in the state this op would commit from, so the
		// positive half of the mutation test is the op's own happy path.
		ready func(env *opsEnv)
	}{
		{"DrainSpCntlrs", opDrainSpCntlrs, func(env *opsEnv) error {
			_, err := DrainSpCntlrs(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName)
			return err
		}, func(env *opsEnv) { env.chargeFixture(); env.setDeleting() }},
		{"DrainSpSlice", opDrainSpSlice, func(env *opsEnv) error {
			_, _, err := DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, opsSliceId, env.cc)
			return err
		}, func(env *opsEnv) { env.latchForDrain() }},
		{"FinishSpDelete", opFinishSpDelete, func(env *opsEnv) error {
			return FinishSpDelete(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName)
		}, func(env *opsEnv) {
			env.latchForDrain()
			conf := env.spConf()
			conf.SliceIdList = nil
			mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
		}},
	}
	guards := []struct {
		name   string
		break_ func(env *opsEnv)
		reason string
	}{
		{"not deleting", func(env *opsEnv) {
			conf := env.spConf()
			conf.Deleting = false
			mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
		}, "sp not deleting"},
		{"sp not found", func(env *opsEnv) {
			env.delKey(SpConfKey(env.cid, opsSpName))
		}, "sp not found"},
		{"sp id changed", func(env *opsEnv) {
			conf := env.spConf()
			conf.SpId = opsSpId + 1
			mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
		}, "sp id changed"},
	}
	for _, op := range ops {
		for _, guard := range guards {
			t.Run(op.name+"/"+guard.name, func(t *testing.T) {
				env := newOpsEnv(t)
				op.ready(env)
				guard.break_(env)
				err := op.run(env)
				precondition := wantPrecondition(t, err, op.op)
				if precondition.Reason != guard.reason {
					t.Errorf("Reason: got %q, want %q",
						precondition.Reason, guard.reason)
				}
			})
		}
		t.Run(op.name+"/commits when latched", func(t *testing.T) {
			// The other direction: a guard that refused unconditionally would
			// pass every row above and fail here.
			env := newOpsEnv(t)
			op.ready(env)
			if err := op.run(env); err != nil {
				t.Fatalf("%s on a correctly latched SP: %v", op.name, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Idempotence, concurrency and lost keys
// ---------------------------------------------------------------------------

// TestDrainStepsAreIdempotent is SPD8's "a step is an idempotent pop what is
// still there": the accepted transient two-owner overlap means both owners run
// the same step, and the loser must find nothing to do rather than an error to
// amplify.
//
// It is the ONE place absence is allowed to mean "someone else did it", and it
// is sound for a reason SPD2's refusals are not: what is absent here is the
// thing this step was going to remove, inside an SP this step has already
// proved is the right one and is latched.
func TestDrainStepsAreIdempotent(t *testing.T) {
	env := newOpsEnv(t)
	env.chargeFixture()
	env.setDeleting()
	if _, err := DrainSpCntlrs(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
	); err != nil {
		t.Fatalf("D1: %v", err)
	}
	revAfterD1 := env.spRev()
	removed, err := DrainSpCntlrs(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName)
	if err != nil {
		t.Fatalf("the loser's D1 must not fail: %v", err)
	}
	if removed != 0 {
		t.Errorf("the loser's D1 removed %d cntlrs, want 0", removed)
	}
	if got := env.spRev(); got != revAfterD1 {
		t.Errorf("a no-op D1 bumped SpRev to %d, want %d", got, revAfterD1)
	}

	if _, done, err := DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, opsSliceId, env.cc); err != nil || !done {
		t.Fatalf("D2: done=%v err=%v", done, err)
	}
	revAfterD2 := env.spRev()
	gone, done, err := DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, opsSliceId, env.cc)
	if err != nil {
		t.Fatalf("the loser's D2 must not fail: %v", err)
	}
	if gone != 0 || done {
		t.Errorf("the loser's D2 reported %d/%v, want 0/false", gone, done)
	}
	if got := env.spRev(); got != revAfterD2 {
		t.Errorf("a no-op D2 bumped SpRev to %d, want %d", got, revAfterD2)
	}
}

// TestDrainConcurrentDriversConverge is dnv-worker.md §13's "two concurrent
// drivers converging with exact ledgers", run for real: two goroutines drive
// the same drain to the end at the same time, and the SP must end up gone with
// neither driver reporting a failure outside the three tolerated reasons.
//
// They are named rather than blanket-tolerated: the loser of a step whose
// object is already gone is a no-op, not a failure at all (above). A D3 that
// runs while the other driver has not committed its last batch yet is "slices
// remain", and a driver whose SP the other's D3 deleted between its read and
// its call is "sp not found" — real refusals the next pass re-derives past.
// "cntlrs not drained" is the third reason tolerated and the defensive one:
// the drain only ever empties the cntlr list, so a driver that dispatched on
// an empty one cannot find it refilled. Anything else is a bug this test must
// fail on.
func TestDrainConcurrentDriversConverge(t *testing.T) {
	env := newOpsEnv(t)
	env.chargeFixture()
	env.setDeleting()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for driver := 0; driver < 2; driver++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for step := 0; step < 64; step++ {
				conf := &pb.SpConf{}
				found, err := env.cli.Get(
					env.ctx, SpConfKey(env.cid, opsSpName), conf)
				if err != nil {
					errs[idx] = err
					return
				}
				if !found {
					return
				}
				switch {
				case len(conf.GetCntlrIdList()) > 0:
					_, err = DrainSpCntlrs(env.ctx, env.cli, env.cid,
						opsShard, opsSpId, opsSpName)
				case len(conf.GetSliceIdList()) > 0:
					_, _, err = DrainSpSlice(env.ctx, env.cli, env.cid,
						opsShard, opsSpId, opsSpName, opsSliceId, env.cc)
				default:
					err = FinishSpDelete(env.ctx, env.cli, env.cid,
						opsShard, opsSpId, opsSpName)
				}
				if err == nil {
					continue
				}
				var pre *ErrPrecondition
				if errors.As(err, &pre) &&
					(pre.Reason == "slices remain" ||
						pre.Reason == "cntlrs not drained" ||
						pre.Reason == "sp not found") {
					// The two real refusals the next pass re-derives
					// past, and the defensive one (see the header).
					continue
				}
				errs[idx] = err
				return
			}
		}(driver)
	}
	wg.Wait()
	for idx, err := range errs {
		if err != nil {
			t.Errorf("driver %d: %v", idx, err)
		}
	}
	for _, key := range []string{
		SpConfKey(env.cid, opsSpName),
		SpNameKey(env.cid, opsSpId),
		SpRevKey(opsShard, env.cid, opsSpId),
		SliceKey(env.cid, opsSpId, opsSliceId),
		CntlrKey(env.cid, opsSpId, opsCntlrA),
		CntlrKey(env.cid, opsSpId, opsCntlrB),
	} {
		if env.exists(key) {
			t.Errorf("%q survived two concurrent drivers", key)
		}
	}
	// And the ledgers are exact, not merely non-negative: two drivers releasing
	// the same side twice is the failure this shape is most likely to produce.
	for _, addrPort := range []string{opsDnA, opsDnB} {
		if got := env.dn(addrPort).GetFreeExtCnt(); got != opsDnFree {
			t.Errorf("dn %q: free_ext_cnt %d, want exactly %d",
				addrPort, got, opsDnFree)
		}
	}
	for _, addrPort := range []string{opsCnA, opsCnB} {
		if got := env.cn(addrPort).GetFreeExtCnt(); got != opsCnFree {
			t.Errorf("cn %q: free_ext_cnt %d, want exactly %d",
				addrPort, got, opsCnFree)
		}
	}
}

// TestDrainToleratesALostNodeRecord pins the deliberate asymmetry between a
// DESCRIBING key and a RECEIVING ledger, which is the one place the drain does
// treat absence as "nothing to do".
//
//   - A Cntlr or a Slice key the SpConf lists but etcd does not have is a
//     REFUSAL: that record is what says which node reserved what, so without it
//     the release cannot be made exact, and a drain that carried on would
//     under-credit a node silently and for ever.
//   - A DnConf or CnConf the drain reaches through a stored Side or Cntlr is
//     SKIPPED: the key that describes the side is being deleted either way and
//     there is nothing left to give the extents back to. It is releaseCn's
//     stance, and it is also what keeps a delete from being un-finishable — the
//     gateway refuses over the same absence (TestReleasePathsAbortOnALostConfKey)
//     because there the caller can retry, whereas nothing else ever removes a
//     latched SP.
func TestDrainToleratesALostNodeRecord(t *testing.T) {
	t.Run("a lost cn_conf is skipped", func(t *testing.T) {
		env := newOpsEnv(t)
		env.chargeFixture()
		env.setDeleting()
		env.delKey(CnConfKey(env.cid, opsCnA))
		revBefore := env.cnRev(opsCnB)
		removed, err := DrainSpCntlrs(
			env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName)
		if err != nil {
			t.Fatalf("D1 over a lost cn_conf: %v", err)
		}
		if removed != 2 {
			t.Errorf("removed %d cntlrs, want 2", removed)
		}
		if env.exists(CntlrKey(env.cid, opsSpId, opsCntlrA)) {
			t.Errorf("the cntlr on the lost CN survived")
		}
		// The surviving CN is still credited exactly once.
		if got := env.cnRev(opsCnB); got != revBefore+1 {
			t.Errorf("cn-b: revision %d, want %d", got, revBefore+1)
		}
	})

	t.Run("a lost dn_conf is skipped", func(t *testing.T) {
		env := newOpsEnv(t)
		env.chargeFixture()
		env.latchForDrain()
		env.delKey(DnConfKey(env.cid, opsDnA))
		charged := opsDnFree - opsFootprint
		revBefore := env.dnRev(opsDnB)
		_, done, err := DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
			opsSpId, opsSpName, opsSliceId, env.cc)
		if err != nil {
			t.Fatalf("D2 over a lost dn_conf: %v", err)
		}
		if !done {
			t.Errorf("the batch must still empty the slice")
		}
		if env.exists(SliceKey(env.cid, opsSpId, opsSliceId)) {
			t.Errorf("the slice key survived")
		}
		// The other direction, as for the CN twin: the SURVIVING DN is still
		// credited exactly once. A releaser that gave up on the whole batch
		// when one node was missing would leave dn-b charged for ever.
		dn := env.dn(opsDnB)
		if dn.GetFreeExtCnt() != opsDnFree {
			t.Errorf("dn-b: free_ext_cnt %d, want %d (it was charged %d)",
				dn.GetFreeExtCnt(), opsDnFree, charged)
		}
		if len(dn.GetSidePtrList()) != 0 {
			t.Errorf("dn-b: side_ptr_list %v", dn.GetSidePtrList())
		}
		if got := env.dnRev(opsDnB); got != revBefore+1 {
			t.Errorf("dn-b: revision %d, want %d", got, revBefore+1)
		}
	})

	t.Run("a lost cntlr key refuses", func(t *testing.T) {
		env := newOpsEnv(t)
		env.chargeFixture()
		env.setDeleting()
		env.delKey(CntlrKey(env.cid, opsSpId, opsCntlrA))
		_, err := DrainSpCntlrs(
			env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName)
		precondition := wantPrecondition(t, err, opDrainSpCntlrs)
		if precondition.Reason != "cntlr not found" {
			t.Errorf("Reason: got %q, want %q",
				precondition.Reason, "cntlr not found")
		}
		if !env.exists(CntlrKey(env.cid, opsSpId, opsCntlrB)) {
			t.Errorf("a refused D1 deleted the other cntlr")
		}
	})

	t.Run("a lost slice key refuses", func(t *testing.T) {
		env := newOpsEnv(t)
		env.latchForDrain()
		env.delKey(SliceKey(env.cid, opsSpId, opsSliceId))
		_, _, err := DrainSpSlice(env.ctx, env.cli, env.cid, opsShard,
			opsSpId, opsSpName, opsSliceId, env.cc)
		precondition := wantPrecondition(t, err, opDrainSpSlice)
		if precondition.Reason != "slice not found" {
			t.Errorf("Reason: got %q, want %q",
				precondition.Reason, "slice not found")
		}
		if got := env.spConf().GetSliceIdList(); len(got) != 1 {
			t.Errorf("a refused batch shrank slice_id_list to %v", got)
		}
	})
}

// TestReleaseShardIsTheDeletionHalf pins model.ReleaseShard, which moved here
// from the gateway so that FinishSpDelete can apply GW12's deletion half from
// the worker: DeleteCluster's bucket-sum gate is only exact while every writer
// of a bucket agrees on the rule.
func TestReleaseShardIsTheDeletionHalf(t *testing.T) {
	bucket := make([]uint32, common.ShardBucketSize)
	bucket[3] = 2
	got := ReleaseShard(bucket, 3)
	if got[3] != 1 {
		t.Errorf("bucket[3] = %d, want 1", got[3])
	}
	if bucket[3] != 2 {
		t.Errorf("ReleaseShard mutated its argument")
	}
	// A zero slot is left alone rather than wrapped around, and a short bucket
	// is normalized exactly as the mint half normalizes it.
	if got := ReleaseShard(make([]uint32, common.ShardBucketSize), 7); got[7] != 0 {
		t.Errorf("a zero slot wrapped around to %d", got[7])
	}
	if got := ReleaseShard([]uint32{1, 2}, 1); len(got) != common.ShardBucketSize {
		t.Errorf("bucket length %d, want %d", len(got), common.ShardBucketSize)
	}
	// Out of range is a no-op, never a panic: a hand-edited global must not
	// crash the drain.
	if got := ReleaseShard(bucket, common.ShardBucketSize); len(got) !=
		common.ShardBucketSize {
		t.Errorf("an out-of-range shard changed the bucket length")
	}
}
