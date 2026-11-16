package model

import (
	"fmt"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// TestMarkGroupsArrayMade: the op sets the flag on every group its refs
// name that does not carry it, the slice's and a clone meta group's, in one
// STM with one SpRev bump, and returns the count and the revision; a second
// call over the same groups writes nothing at all; a ref to a group or a
// parent that is gone, and one to a deleting clone's group, is skipped.
func TestMarkGroupsArrayMade(t *testing.T) {
	env := newOpsEnv(t)
	env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(true), false)
	clone := GrpParent{CloneName: opsCloneName}
	refs := []GrpRef{
		{Parent: opsSliceParent, GrpId: opsMetaGrpId},
		{Parent: opsSliceParent, GrpId: opsDataGrpId},
		{Parent: clone, GrpId: opsCloneGrpId},
		{Parent: opsSliceParent, GrpId: 999},
		{Parent: GrpParent{SliceId: 999}, GrpId: opsDataGrpId},
	}
	before := env.spRev()
	set, revision, err := MarkGroupsArrayMade(env.ctx, env.cli, env.cid,
		opsShard, opsSpId, opsSpName, refs)
	if err != nil || set != 3 || revision != before+1 ||
		env.spRev() != before+1 {
		t.Fatalf("the mark: set %d, revision %d, err %v, SpRev %d; want 3 "+
			"at %d", set, revision, err, env.spRev(), before+1)
	}
	for _, grpId := range []uint64{opsMetaGrpId, opsDataGrpId} {
		if !findGroup(env.slice(), grpId).GetArrayMade() {
			t.Errorf("group %d carries no flag", grpId)
		}
	}
	if !env.clone(opsCloneName).GetMetaGrp().GetArrayMade() {
		t.Errorf("the clone meta group carries no flag")
	}
	store := env.storeRev()
	set, revision, err = MarkGroupsArrayMade(env.ctx, env.cli, env.cid,
		opsShard, opsSpId, opsSpName, refs)
	if err != nil || set != 0 || revision != 0 || env.storeRev() != store {
		t.Fatalf("the second mark: set %d, revision %d, err %v; want no "+
			"write", set, revision, err)
	}

	env = newOpsEnv(t)
	env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(true), true)
	store = env.storeRev()
	set, _, err = MarkGroupsArrayMade(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, []GrpRef{{Parent: clone, GrpId: opsCloneGrpId}})
	if err != nil || set != 0 || env.storeRev() != store {
		t.Fatalf("a deleting clone's group: set %d, err %v; want a skip",
			set, err)
	}
}

// TestMarkGroupsArrayMadeAtTheCeiling commits the flag's write at its widest
// shape against the real etcd this package runs with
// --max-txn-ops=common.EtcdMaxTxnOps: both groups of each of
// MaxSliceCntPerSp slices and the meta group of each of MaxCloneCntPerSp
// clones, every parent read and written in one transaction with the SpConf
// and SpRev keys. That is over etcd's default transaction cap, which makes
// the write one of the transactions the deployment flag exists for
// (dnv-worker.md, Constants this document owns); the shape is a pool whose
// records come from before the flag existed, all flagged in its first pass.
func TestMarkGroupsArrayMadeAtTheCeiling(t *testing.T) {
	const etcdDefaultTxnOps = 128
	sliceCnt, cloneCnt := common.MaxSliceCntPerSp, common.MaxCloneCntPerSp
	if compares := 3 + 2*sliceCnt + 2*cloneCnt; compares <= etcdDefaultTxnOps ||
		compares > common.EtcdMaxTxnOps {
		t.Fatalf("%d slices and %d clones no longer reproduce a write over "+
			"etcd's default: %d compares against the default %d and "+
			"EtcdMaxTxnOps %d", sliceCnt, cloneCnt, compares,
			etcdDefaultTxnOps, common.EtcdMaxTxnOps)
	}
	env := newOpsEnv(t)
	var refs []GrpRef
	var keys []string
	for idx := 0; idx < sliceCnt; idx++ {
		sliceId := opsSliceId + uint64(idx)
		if idx > 0 {
			mustPut(t, env.cli, SliceKey(env.cid, opsSpId, sliceId),
				opsSlice())
		}
		parent := GrpParent{SliceId: sliceId}
		refs = append(refs, GrpRef{Parent: parent, GrpId: opsMetaGrpId},
			GrpRef{Parent: parent, GrpId: opsDataGrpId})
		keys = append(keys, SliceKey(env.cid, opsSpId, sliceId))
	}
	for idx := 0; idx < cloneCnt; idx++ {
		name := fmt.Sprintf("ceil-clone-%02d", idx)
		cloneId := 2000 + 10*uint64(idx)
		env.putGrpClone(name, cloneId,
			cloneGrpAt(cloneId+1, true, opsDnC, opsDnD), false)
		refs = append(refs, GrpRef{
			Parent: GrpParent{CloneName: name}, GrpId: cloneId + 1,
		})
		keys = append(keys, CloneKey(env.cid, opsSpId, name))
	}
	before := env.spRev()

	set, revision, err := MarkGroupsArrayMade(env.ctx, env.cli, env.cid,
		opsShard, opsSpId, opsSpName, refs)
	if err != nil {
		t.Fatalf("MarkGroupsArrayMade over %d slices and %d clones: %v "+
			"(etcd's \"too many operations in txn request\" here means "+
			"one transaction carried more of them than "+
			"common.EtcdMaxTxnOps admits)", sliceCnt, cloneCnt, err)
	}
	if set != len(refs) {
		t.Errorf("set %d groups, want all %d", set, len(refs))
	}
	if revision != before+1 || env.spRev() != before+1 {
		t.Errorf("SpRev: committed %d, stored %d, want %d (one bump)",
			revision, env.spRev(), before+1)
	}
	// One transaction wrote every parent: they share one mod revision.
	want := env.modRev(keys[0])
	for _, key := range keys {
		if got := env.modRev(key); got != want {
			t.Errorf("%s at mod revision %d, want %d: the write took "+
				"more than one transaction", key, got, want)
		}
	}
	for idx := 0; idx < cloneCnt; idx++ {
		name := fmt.Sprintf("ceil-clone-%02d", idx)
		if !env.clone(name).GetMetaGrp().GetArrayMade() {
			t.Errorf("%s: the meta group carries no flag", name)
		}
	}
}
