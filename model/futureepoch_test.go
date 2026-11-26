package model

import (
	"fmt"
	"slices"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The reader's clock in the tests below, in unix seconds.
const futureNow = 1000

// futureSlice is the ops fixture's slice with a meta leg and a data side
// ahead of futureNow, a meta side behind it and the rest unset.
func futureSlice() *pb.Slice {
	slice := opsSlice()
	slice.MetaGrpList[0].LegList[0].ErrEpoch = 3000
	slice.MetaGrpList[0].LegList[1].SideList[0].ErrEpoch = 900
	slice.DataGrpList[0].LegList[1].SideList[0].ErrEpoch = 1500
	return slice
}

// futureClone is a clone whose clone meta group holds one leg ahead of
// futureNow and one side at it.
func futureClone() *pb.Clone {
	return &pb.Clone{MetaGrp: &pb.Group{
		GrpId: 2001,
		LegList: []*pb.Leg{
			{LegId: 2002, ErrEpoch: 5000, SideList: []*pb.Side{{SideId: 2003}}},
			{LegId: 2004, SideList: []*pb.Side{{SideId: 2005, ErrEpoch: futureNow}}},
		},
	}}
}

// futureRepair is a repair whose new leg is ahead of futureNow and whose
// side is behind it.
func futureRepair() *pb.Repair {
	return &pb.Repair{NewLegList: []*pb.Leg{{
		LegId:    3001,
		ErrEpoch: 1200,
		SideList: []*pb.Side{{SideId: 3002, ErrEpoch: 900}},
	}}}
}

// TestLowerFutureErrEpochs: the op writes its time over every health epoch
// later than it, in a cntlr, a slice's groups, a clone meta group and a
// repair's new leg, in one STM that bumps no revision; it leaves a zero, a
// past epoch and one equal to its time; a record that is gone is skipped; a
// second call writes nothing.
func TestLowerFutureErrEpochs(t *testing.T) {
	env := newOpsEnv(t)
	cid := env.cid
	cntlrA := env.cntlr(opsCntlrA)
	cntlrA.ErrEpoch = 2000
	mustPut(t, env.cli, CntlrKey(cid, opsSpId, opsCntlrA), cntlrA)
	cntlrB := env.cntlr(opsCntlrB)
	cntlrB.ErrEpoch = 500
	mustPut(t, env.cli, CntlrKey(cid, opsSpId, opsCntlrB), cntlrB)
	mustPut(t, env.cli, SliceKey(cid, opsSpId, opsSliceId), futureSlice())
	mustPut(t, env.cli, CloneKey(cid, opsSpId, "fclone"), futureClone())
	mustPut(t, env.cli, RepairKey(cid, opsSpId, "frep"), futureRepair())
	mustPut(t, env.cli, RetiredLegKey(cid, opsSpId, 4001), &pb.RetiredLeg{
		Side: &pb.Side{SideId: 4002, ErrEpoch: 1100},
	})
	refs := []EpochRecordRef{
		{Record: EpochRecordCntlr, Id: opsCntlrA},
		{Record: EpochRecordCntlr, Id: opsCntlrB},
		{Record: EpochRecordSlice, Id: opsSliceId},
		{Record: EpochRecordClone, Name: "fclone"},
		{Record: EpochRecordRepair, Name: "frep"},
		{Record: EpochRecordCntlr, Id: 999},
		{Record: EpochRecordClone, Name: "gone"},
		{Record: EpochRecordSlice, Id: opsSliceId},
	}
	spRev := env.spRev()
	lowered, err := LowerFutureErrEpochs(env.ctx, env.cli, cid, opsSpId,
		futureNow, refs)
	if err != nil || lowered != 5 {
		t.Fatalf("the write: lowered %d, err %v; want 5", lowered, err)
	}
	if env.spRev() != spRev {
		t.Errorf("SpRev moved to %d from %d: the write bumps nothing",
			env.spRev(), spRev)
	}
	if got := env.cntlr(opsCntlrA).GetErrEpoch(); got != futureNow {
		t.Errorf("cntlr A's epoch %d, want %d", got, futureNow)
	}
	if got := env.cntlr(opsCntlrB).GetErrEpoch(); got != 500 {
		t.Errorf("cntlr B's past epoch %d, want 500 kept", got)
	}
	slice := env.slice()
	meta, data := slice.GetMetaGrpList()[0], slice.GetDataGrpList()[0]
	for name, got := range map[string]uint64{
		"meta leg A":  meta.GetLegList()[0].GetErrEpoch(),
		"data side B": data.GetLegList()[1].GetSideList()[0].GetErrEpoch(),
	} {
		if got != futureNow {
			t.Errorf("%s's epoch %d, want %d", name, got, futureNow)
		}
	}
	if got := meta.GetLegList()[1].GetSideList()[0].GetErrEpoch(); got != 900 {
		t.Errorf("meta side B's past epoch %d, want 900 kept", got)
	}
	if got := data.GetLegList()[0].GetErrEpoch(); got != 0 {
		t.Errorf("data leg A's epoch %d, want 0 kept", got)
	}
	clone := env.clone("fclone").GetMetaGrp()
	if got := clone.GetLegList()[0].GetErrEpoch(); got != futureNow {
		t.Errorf("clone leg's epoch %d, want %d", got, futureNow)
	}
	if got := clone.GetLegList()[0].GetSideList()[0].GetErrEpoch(); got != 0 {
		t.Errorf("clone side's epoch %d, want 0 kept", got)
	}
	if got := clone.GetLegList()[1].GetSideList()[0].GetErrEpoch(); got != futureNow {
		t.Errorf("clone side at the reader's time %d, want it kept", got)
	}
	newLeg := env.repair("frep").GetNewLegList()[0]
	if newLeg.GetErrEpoch() != futureNow ||
		newLeg.GetSideList()[0].GetErrEpoch() != 900 {
		t.Errorf("repair's new leg %d and side %d, want %d and 900",
			newLeg.GetErrEpoch(), newLeg.GetSideList()[0].GetErrEpoch(),
			futureNow)
	}
	if got := env.retired(4001).GetSide().GetErrEpoch(); got != 1100 {
		t.Errorf("retired side's epoch %d, want 1100: no ref named it", got)
	}

	store := env.storeRev()
	lowered, err = LowerFutureErrEpochs(env.ctx, env.cli, cid, opsSpId,
		futureNow, refs)
	if err != nil || lowered != 0 || env.storeRev() != store {
		t.Fatalf("the second write: lowered %d, err %v; want no write",
			lowered, err)
	}
}

// TestFutureEpochRecords: the scan names each record of a loaded SP that
// holds a health epoch later than the reader's clock, once, in a stable
// order, with the largest step; a record whose epochs are all at or behind
// the clock, a retired side and a node record are not named.
func TestFutureEpochRecords(t *testing.T) {
	state := &SpState{
		Cntlrs: map[uint64]*pb.Cntlr{
			7: {ErrEpoch: 2000},
			3: {ErrEpoch: futureNow},
			5: {ErrEpoch: 500},
		},
		Slices: map[uint64]*pb.Slice{
			opsSliceId: futureSlice(),
			99:         opsSlice(),
		},
		Clones:  map[string]*pb.Clone{"fclone": futureClone()},
		Repairs: map[string]*pb.Repair{"frep": futureRepair()},
		RetiredLegs: map[uint64]*pb.RetiredLeg{
			4001: {Side: &pb.Side{ErrEpoch: 1100}},
			4003: {Side: &pb.Side{ErrEpoch: 800}},
		},
		DnByAddr: map[string]*pb.DnConf{opsDnA: {ErrEpoch: 9000}},
	}
	refs, step := FutureEpochRecords(state, futureNow)
	want := []EpochRecordRef{
		{Record: EpochRecordCntlr, Id: 7},
		{Record: EpochRecordSlice, Id: opsSliceId},
		{Record: EpochRecordClone, Name: "fclone"},
		{Record: EpochRecordRepair, Name: "frep"},
	}
	if !slices.Equal(refs, want) {
		t.Errorf("refs %v, want %v", refs, want)
	}
	if step != 4000 {
		t.Errorf("step %d, want 4000 (the clone leg's 5000)", step)
	}
	if refs, step := FutureEpochRecords(state, 9000); len(refs) != 0 ||
		step != 0 {
		t.Errorf("at a clock past every epoch: refs %v, step %d; want none",
			refs, step)
	}
}

// TestLowerFutureErrEpochsAtTheCeiling commits the write at its widest shape
// against the real etcd this package runs with
// --max-txn-ops=common.EtcdMaxTxnOps: every cntlr, every one of
// MaxSliceCntPerSp slices and MaxCloneCntPerSp clones in its first
// transaction, and repairs past one transaction's worth after them, each
// with an epoch ahead of the clock.
func TestLowerFutureErrEpochsAtTheCeiling(t *testing.T) {
	sliceCnt, cloneCnt := common.MaxSliceCntPerSp, common.MaxCloneCntPerSp
	// Repairs enough that the whole list in one transaction would pass
	// EtcdMaxTxnOps, so the test fails if the write is not split.
	repCnt := 2*LowerRecordsPerTxn + 100
	if 2*LowerRecordsPerTxn > common.EtcdMaxTxnOps {
		t.Fatalf("%d records need %d operations, over EtcdMaxTxnOps %d",
			LowerRecordsPerTxn, 2*LowerRecordsPerTxn, common.EtcdMaxTxnOps)
	}
	env := newOpsEnv(t)
	cid := env.cid
	refs := []EpochRecordRef{}
	var firstKeys []string
	for _, id := range []uint64{opsCntlrA, opsCntlrB} {
		cntlr := env.cntlr(id)
		cntlr.ErrEpoch = futureNow + 1
		mustPut(t, env.cli, CntlrKey(cid, opsSpId, id), cntlr)
		refs = append(refs, EpochRecordRef{Record: EpochRecordCntlr, Id: id})
		firstKeys = append(firstKeys, CntlrKey(cid, opsSpId, id))
	}
	for idx := 0; idx < sliceCnt; idx++ {
		sliceId := opsSliceId + uint64(idx)
		mustPut(t, env.cli, SliceKey(cid, opsSpId, sliceId), futureSlice())
		refs = append(refs, EpochRecordRef{Record: EpochRecordSlice, Id: sliceId})
		firstKeys = append(firstKeys, SliceKey(cid, opsSpId, sliceId))
	}
	for idx := 0; idx < cloneCnt; idx++ {
		name := fmt.Sprintf("ceil-clone-%02d", idx)
		cloneId := 2000 + 10*uint64(idx)
		grp := cloneGrpAt(cloneId+1, true, opsDnC, opsDnD)
		grp.LegList[0].ErrEpoch = futureNow + 2
		env.putGrpClone(name, cloneId, grp, false)
		refs = append(refs, EpochRecordRef{Record: EpochRecordClone, Name: name})
		firstKeys = append(firstKeys, CloneKey(cid, opsSpId, name))
	}
	if len(refs) > LowerRecordsPerTxn {
		t.Fatalf("%d cntlrs, slices and clones, over one transaction's %d",
			len(refs), LowerRecordsPerTxn)
	}
	for idx := 0; idx < repCnt; idx++ {
		name := fmt.Sprintf("ceil-rep-%04d", idx)
		mustPut(t, env.cli, RepairKey(cid, opsSpId, name), &pb.Repair{
			NewLegList: []*pb.Leg{{LegId: 5000 + uint64(idx),
				ErrEpoch: futureNow + 3}},
		})
		refs = append(refs, EpochRecordRef{Record: EpochRecordRepair, Name: name})
	}
	want := 2 + 2*sliceCnt + cloneCnt + repCnt
	lowered, err := LowerFutureErrEpochs(env.ctx, env.cli, cid, opsSpId,
		futureNow, refs)
	if err != nil || lowered != want {
		t.Fatalf("the write: lowered %d, err %v; want %d", lowered, err, want)
	}
	first := env.modRev(firstKeys[0])
	for _, key := range firstKeys {
		if got := env.modRev(key); got != first {
			t.Errorf("%s at mod revision %d, want %d: the cntlrs, slices and "+
				"clones took more than one transaction", key, got, first)
		}
	}
	if got := env.repair(fmt.Sprintf("ceil-rep-%04d", repCnt-1)).
		GetNewLegList()[0].GetErrEpoch(); got != futureNow {
		t.Errorf("the last repair's epoch %d, want %d", got, futureNow)
	}
}
