package model

import (
	"fmt"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// TestSetFenceLastLegs: the op writes each group's fence-last leg, the
// slice's and a clone meta group's, in one STM that bumps no revision, and
// returns the count; a second call with the same legs writes nothing; zero
// clears a leg; a ref to a group or a parent that is gone, and one to a
// deleting clone's group, is skipped.
func TestSetFenceLastLegs(t *testing.T) {
	env := newOpsEnv(t)
	env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(true), false)
	clone := GrpParent{CloneName: opsCloneName}
	edits := []FenceLastEdit{
		{Ref: GrpRef{Parent: opsSliceParent, GrpId: opsMetaGrpId}, LegId: 11},
		{Ref: GrpRef{Parent: opsSliceParent, GrpId: opsDataGrpId}, LegId: 12},
		{Ref: GrpRef{Parent: clone, GrpId: opsCloneGrpId}, LegId: 13},
		{Ref: GrpRef{Parent: opsSliceParent, GrpId: 999}, LegId: 14},
		{Ref: GrpRef{Parent: GrpParent{SliceId: 999}, GrpId: opsDataGrpId},
			LegId: 15},
	}
	before := env.spRev()
	set, err := SetFenceLastLegs(env.ctx, env.cli, env.cid, opsSpId, edits)
	if err != nil || set != 3 {
		t.Fatalf("the write: set %d, err %v; want 3", set, err)
	}
	if env.spRev() != before {
		t.Errorf("SpRev moved to %d from %d: the write bumps nothing",
			env.spRev(), before)
	}
	if got := findGroup(env.slice(), opsMetaGrpId).GetFenceLastLegId(); got != 11 {
		t.Errorf("meta group's fence-last leg %d, want 11", got)
	}
	if got := findGroup(env.slice(), opsDataGrpId).GetFenceLastLegId(); got != 12 {
		t.Errorf("data group's fence-last leg %d, want 12", got)
	}
	if got := env.clone(opsCloneName).GetMetaGrp().GetFenceLastLegId(); got != 13 {
		t.Errorf("clone meta group's fence-last leg %d, want 13", got)
	}
	store := env.storeRev()
	set, err = SetFenceLastLegs(env.ctx, env.cli, env.cid, opsSpId, edits)
	if err != nil || set != 0 || env.storeRev() != store {
		t.Fatalf("the second write: set %d, err %v; want no write", set, err)
	}
	set, err = SetFenceLastLegs(env.ctx, env.cli, env.cid, opsSpId,
		[]FenceLastEdit{{Ref: edits[0].Ref}})
	if err != nil || set != 1 ||
		findGroup(env.slice(), opsMetaGrpId).GetFenceLastLegId() != 0 {
		t.Fatalf("the clear: set %d, err %v, leg %d; want 1 and 0", set, err,
			findGroup(env.slice(), opsMetaGrpId).GetFenceLastLegId())
	}

	env = newOpsEnv(t)
	env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(true), true)
	store = env.storeRev()
	set, err = SetFenceLastLegs(env.ctx, env.cli, env.cid, opsSpId,
		[]FenceLastEdit{{Ref: GrpRef{Parent: clone, GrpId: opsCloneGrpId},
			LegId: 13}})
	if err != nil || set != 0 || env.storeRev() != store {
		t.Fatalf("a deleting clone's group: set %d, err %v; want a skip",
			set, err)
	}
}

// TestSetFenceLastLegsAtTheCeiling commits the write at its widest shape
// against the real etcd this package runs with
// --max-txn-ops=common.EtcdMaxTxnOps: both groups of each of
// MaxSliceCntPerSp slices and the meta group of each of MaxCloneCntPerSp
// clones, every parent read and written in one transaction, as a worker's
// first readings of a large pool write them.
func TestSetFenceLastLegsAtTheCeiling(t *testing.T) {
	sliceCnt, cloneCnt := common.MaxSliceCntPerSp, common.MaxCloneCntPerSp
	if ops := 2*sliceCnt + 2*cloneCnt; ops > common.EtcdMaxTxnOps {
		t.Fatalf("%d slices and %d clones need %d operations, over "+
			"EtcdMaxTxnOps %d", sliceCnt, cloneCnt, ops,
			common.EtcdMaxTxnOps)
	}
	env := newOpsEnv(t)
	var edits []FenceLastEdit
	var keys []string
	for idx := 0; idx < sliceCnt; idx++ {
		sliceId := opsSliceId + uint64(idx)
		if idx > 0 {
			mustPut(t, env.cli, SliceKey(env.cid, opsSpId, sliceId),
				opsSlice())
		}
		parent := GrpParent{SliceId: sliceId}
		edits = append(edits,
			FenceLastEdit{Ref: GrpRef{Parent: parent, GrpId: opsMetaGrpId},
				LegId: 7},
			FenceLastEdit{Ref: GrpRef{Parent: parent, GrpId: opsDataGrpId},
				LegId: 8})
		keys = append(keys, SliceKey(env.cid, opsSpId, sliceId))
	}
	for idx := 0; idx < cloneCnt; idx++ {
		name := fmt.Sprintf("ceil-clone-%02d", idx)
		cloneId := 2000 + 10*uint64(idx)
		env.putGrpClone(name, cloneId,
			cloneGrpAt(cloneId+1, true, opsDnC, opsDnD), false)
		edits = append(edits, FenceLastEdit{
			Ref:   GrpRef{Parent: GrpParent{CloneName: name}, GrpId: cloneId + 1},
			LegId: 9,
		})
		keys = append(keys, CloneKey(env.cid, opsSpId, name))
	}
	set, err := SetFenceLastLegs(env.ctx, env.cli, env.cid, opsSpId, edits)
	if err != nil {
		t.Fatalf("SetFenceLastLegs over %d slices and %d clones: %v",
			sliceCnt, cloneCnt, err)
	}
	if set != len(edits) {
		t.Errorf("set %d groups, want all %d", set, len(edits))
	}
	want := env.modRev(keys[0])
	for _, key := range keys {
		if got := env.modRev(key); got != want {
			t.Errorf("%s at mod revision %d, want %d: the write took "+
				"more than one transaction", key, got, want)
		}
	}
}
