package model

import (
	"errors"
	"sync"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The clone drain's own suite (dnv-worker.md §11.7, model half).
//
// Both of its guards are mutation-tested in BOTH directions, for the sp
// sibling's reason: an op that refused everything would pass a test that only
// checked the refusals, and one that refused nothing would pass a test that
// only checked the happy path.

const (
	drainCloneName = "clone-drain"
	drainCloneId   = uint64(0x900)
)

// putDrainClone plants one clone of the fixture SP with the given chunk pairs,
// latched or not, and returns the pairs it wrote.
func (e *opsEnv) putDrainClone(
	deleting bool,
	pairs [][2]uint32,
) []BmChunk {
	e.t.Helper()
	conf := e.spConf()
	if !containsName(conf.GetCloneNameList(), drainCloneName) {
		conf.CloneNameList = append(conf.GetCloneNameList(), drainCloneName)
		mustPut(e.t, e.cli, SpConfKey(e.cid, opsSpName), conf)
	}
	mustPut(e.t, e.cli, CloneKey(e.cid, opsSpId, drainCloneName), &pb.Clone{
		CloneId:     drainCloneId,
		SrcNqn:      "nqn.2024-01.io.dnv:src",
		SrcSliceCnt: common.MaxSliceCntPerSp,
		DstTdId:     601,
		// bm_cnt as AppendCloneBitmap's high-water would have left it: the
		// drain must not read it, and must not rewrite it either.
		BmCnt:    uint32(len(pairs)),
		Deleting: deleting,
	})
	chunks := make([]BmChunk, 0, len(pairs))
	for _, pair := range pairs {
		mustPut(e.t, e.cli,
			CloneBitmapKey(e.cid, opsSpId, drainCloneName, pair[0], pair[1]),
			&pb.CloneBitmap{Bitmap: []byte{byte(pair[1])}})
		chunks = append(chunks, BmChunk{SliceIdx: pair[0], Idx: pair[1]})
	}
	return chunks
}

// clone reads one Clone of the fixture SP.
func (e *opsEnv) clone(name string) *pb.Clone {
	e.t.Helper()
	clone := &pb.Clone{}
	e.get(CloneKey(e.cid, opsSpId, name), clone)
	return clone
}

// containsName reports whether list already holds name.
func containsName(list []string, name string) bool {
	for _, item := range list {
		if item == name {
			return true
		}
	}
	return false
}

// drainCloneChunkCnt counts the surviving chunk keys of the fixture clone.
func (e *opsEnv) drainCloneChunkCnt() int {
	e.t.Helper()
	keys, _, err := e.cli.RangeKeys(
		e.ctx, CloneBitmapPrefix(e.cid, opsSpId, drainCloneName))
	if err != nil {
		e.t.Fatalf("RangeKeys: %v", err)
	}
	return len(keys)
}

// TestDrainCloneBm is CLD8: the batch deletes exactly the chunks it was handed,
// leaves the Clone record alone, and bumps SpRev once.
//
// "Exactly the chunks it was handed" is the load-bearing half. An STM cannot
// range, so the set a batch may touch has to come in from the caller's snapshot
// scan; a batch that derived the set itself — from the record's
// src_slice_cnt x bm_cnt rectangle, as the sweep this replaced did — would be
// back to a transaction whose size is the clone's shape.
func TestDrainCloneBm(t *testing.T) {
	env := newOpsEnv(t)
	chunks := env.putDrainClone(true, [][2]uint32{
		{0, 0}, {0, 2}, {3, 1}, {5, 0},
	})
	before := env.clone(drainCloneName)
	revBefore := env.spRev()

	removed, err := DrainCloneBm(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, drainCloneName, drainCloneId, chunks[:2])
	if err != nil {
		t.Fatalf("DrainCloneBm: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d chunks, want 2", removed)
	}
	for _, chunk := range chunks[:2] {
		if env.exists(CloneBitmapKey(env.cid, opsSpId, drainCloneName,
			chunk.SliceIdx, chunk.Idx)) {
			t.Errorf("chunk (%d, %d) survived the batch",
				chunk.SliceIdx, chunk.Idx)
		}
	}
	for _, chunk := range chunks[2:] {
		if !env.exists(CloneBitmapKey(env.cid, opsSpId, drainCloneName,
			chunk.SliceIdx, chunk.Idx)) {
			t.Errorf("chunk (%d, %d) was NOT handed to the batch and must "+
				"survive it", chunk.SliceIdx, chunk.Idx)
		}
	}
	// CLD8: the record is untouched — bm_cnt above all, which the drain does
	// not read and must not maintain (CLD7).
	if got := env.clone(drainCloneName); got.GetBmCnt() != before.GetBmCnt() ||
		got.GetDeleting() != true {
		t.Errorf("the batch rewrote the Clone: got %v, want %v", got, before)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d (exactly one bump)",
			got, revBefore+1)
	}
	// The clone is still listed: only the final STM removes the name.
	if got := env.spConf().GetCloneNameList(); len(got) != 1 {
		t.Errorf("clone_name_list: got %v, want the name to survive", got)
	}
}

// TestDrainCloneBmRefusesAnOversizedBatch pins the bound INSIDE the op, not
// only in the caller that cuts the slice.
//
// The bound is what makes a batch's size a constant independent of every
// ceiling, which is the whole point of the design: a caller that handed the
// op its entire scan would silently rebuild the unbounded sweep this replaced,
// and no arithmetic tripwire would notice, because the tripwire counts
// MaxDelBmPerTxn and not what was actually passed.
func TestDrainCloneBmRefusesAnOversizedBatch(t *testing.T) {
	env := newOpsEnv(t)
	pairs := make([][2]uint32, 0, common.MaxDelBmPerTxn+1)
	for idx := 0; idx <= common.MaxDelBmPerTxn; idx++ {
		pairs = append(pairs, [2]uint32{uint32(idx / 16), uint32(idx % 16)})
	}
	chunks := env.putDrainClone(true, pairs)
	revBefore := env.spRev()

	_, err := DrainCloneBm(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, drainCloneName, drainCloneId, chunks)
	precondition := wantPrecondition(t, err, opDrainCloneBm)
	if precondition.Reason != "batch too large" {
		t.Errorf("Reason: got %q, want %q",
			precondition.Reason, "batch too large")
	}
	if got := env.drainCloneChunkCnt(); got != len(chunks) {
		t.Errorf("a refused batch deleted %d chunks",
			len(chunks)-got)
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("a refused batch bumped SpRev to %d", got)
	}
	// Exactly at the bound it commits: the refusal is the bound and not an
	// off-by-one around it.
	if _, err := DrainCloneBm(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, drainCloneName, drainCloneId,
		chunks[:common.MaxDelBmPerTxn]); err != nil {
		t.Fatalf("a batch of exactly MaxDelBmPerTxn must commit: %v", err)
	}
}

// TestDrainCloneBmIsIdempotent is the accepted two-owner overlap: a Del of a
// key another driver already removed is harmless, and an empty batch is a
// no-op that writes and bumps nothing rather than a refusal.
func TestDrainCloneBmIsIdempotent(t *testing.T) {
	env := newOpsEnv(t)
	chunks := env.putDrainClone(true, [][2]uint32{{0, 0}, {1, 1}})
	if _, err := DrainCloneBm(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, drainCloneName, drainCloneId, chunks); err != nil {
		t.Fatalf("DrainCloneBm: %v", err)
	}
	revAfter := env.spRev()

	// The loser of the race, handed the same batch its own snapshot found.
	removed, err := DrainCloneBm(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, drainCloneName, drainCloneId, chunks)
	if err != nil {
		t.Fatalf("the loser's batch must not fail: %v", err)
	}
	// It still reports what it was asked to delete — a Del is a pop, and the
	// op cannot tell an absent key from one it removed without a read per key,
	// which is exactly the cost this design avoids.
	if removed != len(chunks) {
		t.Errorf("removed %d, want %d", removed, len(chunks))
	}
	if got := env.spRev(); got != revAfter+1 {
		t.Errorf("SpRev: got %d, want %d", got, revAfter+1)
	}

	// An EMPTY batch writes nothing at all.
	revBefore := env.spRev()
	removed, err = DrainCloneBm(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, drainCloneName, drainCloneId, nil)
	if err != nil {
		t.Fatalf("an empty batch must not fail: %v", err)
	}
	if removed != 0 {
		t.Errorf("an empty batch removed %d", removed)
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("an empty batch bumped SpRev to %d", got)
	}
}

// TestCloneDrainConcurrentDriversConverge is the design's "two concurrent
// drivers converge without error amplification" — CLD12's overlap-loser case
// and CLD8's idempotent pop — run for real: two goroutines drive the
// same clone's drain to the end at the same time, and the clone must end up
// gone with neither reporting a failure that is not one of the named benign
// races.
//
// The races are named rather than blanket-tolerated. A batch whose keys another
// driver already removed is an idempotent pop; a final STM that runs while the
// other driver has not committed its last batch is a real refusal the next
// derivation passes; and a driver whose read raced the other's final STM sees
// the clone gone. Anything else is a bug this must fail on — a loser that
// corrupted `clone_name_list`, say, or one that deleted a chunk key of a
// DIFFERENT clone.
func TestCloneDrainConcurrentDriversConverge(t *testing.T) {
	env := newOpsEnv(t)
	pairs := make([][2]uint32, 0, 40)
	for idx := 0; idx < 40; idx++ {
		pairs = append(pairs, [2]uint32{uint32(idx / 8), uint32(idx % 8)})
	}
	env.putDrainClone(true, pairs)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for driver := 0; driver < 2; driver++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for step := 0; step < 64; step++ {
				clone := &pb.Clone{}
				found, err := env.cli.Get(env.ctx,
					CloneKey(env.cid, opsSpId, drainCloneName), clone)
				if err != nil {
					errs[idx] = err
					return
				}
				if !found {
					return
				}
				keys, _, err := env.cli.RangeKeys(env.ctx,
					CloneBitmapPrefix(env.cid, opsSpId, drainCloneName))
				if err != nil {
					errs[idx] = err
					return
				}
				chunks := make([]BmChunk, 0, len(keys))
				for _, entry := range keys {
					sliceIdx, bmIdx, ok := ParseCloneBmKey(entry.Key)
					if !ok {
						continue
					}
					chunks = append(chunks,
						BmChunk{SliceIdx: sliceIdx, Idx: bmIdx})
				}
				// Deliberately small, so the two drivers interleave many
				// times rather than racing once.
				if len(chunks) > 4 {
					chunks = chunks[:4]
				}
				if len(chunks) == 0 {
					err = FinishCloneDelete(env.ctx, env.cli, env.cid,
						opsShard, opsSpId, opsSpName, drainCloneName,
						drainCloneId)
				} else {
					_, err = DrainCloneBm(env.ctx, env.cli, env.cid, opsShard,
						opsSpId, opsSpName, drainCloneName, drainCloneId,
						chunks)
				}
				if err == nil {
					continue
				}
				var pre *ErrPrecondition
				if errors.As(err, &pre) && pre.Reason == "clone not found" {
					// The other driver's final STM committed between this
					// driver's read and its call.
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
	if env.exists(CloneKey(env.cid, opsSpId, drainCloneName)) {
		t.Errorf("the clone survived two concurrent drivers")
	}
	if got := env.drainCloneChunkCnt(); got != 0 {
		t.Errorf("%d chunk keys survived two concurrent drivers", got)
	}
	if got := env.spConf().GetCloneNameList(); len(got) != 0 {
		t.Errorf("clone_name_list: got %v, want empty", got)
	}
	// The SP is intact: two drivers rewriting one SpConf is the shape most
	// likely to lose an unrelated list.
	conf := env.spConf()
	if len(conf.GetSliceIdList()) != 1 || len(conf.GetCntlrIdList()) != 2 ||
		len(conf.GetTdNameList()) != 2 {
		t.Errorf("the concurrent drain damaged the SpConf: %v", conf)
	}
}

// TestFinishCloneDelete is CLD9: the Clone key and its clone_name_list entry go
// together, and SpRev is bumped — unlike the sp drain's D3, which deletes the
// rev key because the SP is going away. Here the SP outlives the clone, so the
// bump is every mutator's normal epilogue.
//
// Both halves are asserted together and never separately: model.LoadSp fetches
// clones by iterating clone_name_list, so a key removed with the name still
// listed would break every subsequent load, and a name removed with the key
// still there would strand it for ever.
func TestFinishCloneDelete(t *testing.T) {
	env := newOpsEnv(t)
	env.putDrainClone(true, nil)
	revBefore := env.spRev()

	if err := FinishCloneDelete(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, drainCloneName, drainCloneId); err != nil {
		t.Fatalf("FinishCloneDelete: %v", err)
	}
	if env.exists(CloneKey(env.cid, opsSpId, drainCloneName)) {
		t.Errorf("the clone key survived")
	}
	if got := env.spConf().GetCloneNameList(); len(got) != 0 {
		t.Errorf("clone_name_list: got %v, want empty", got)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d (the SP outlives the clone)",
			got, revBefore+1)
	}
	if !env.exists(SpRevKey(opsShard, env.cid, opsSpId)) {
		t.Errorf("the clone drain must NOT delete the SP's rev key")
	}
}

// TestCloneDrainOpsRefuseALiveClone is CLD2, mutation-tested across both ops
// and every condition of the shared guard.
//
// It is the inverse of CLD1's refusal, exactly as SPD2 is the inverse of
// loadSpConfForOp's: normal clone RPCs require the flag CLEAR, drain ops
// require it SET. Every row is an ERROR and never a skip — a reconcile loop
// that reads absence as permission is how the wrong thing gets destroyed — and
// each op is paired with the direction that proves the guard is not simply
// "refuse everything".
func TestCloneDrainOpsRefuseALiveClone(t *testing.T) {
	ops := []struct {
		name string
		op   string
		run  func(env *opsEnv) error
	}{
		{"DrainCloneBm", opDrainCloneBm, func(env *opsEnv) error {
			_, err := DrainCloneBm(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, drainCloneName, drainCloneId,
				[]BmChunk{{SliceIdx: 0, Idx: 0}})
			return err
		}},
		{"FinishCloneDelete", opFinishCloneDelete, func(env *opsEnv) error {
			return FinishCloneDelete(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, drainCloneName, drainCloneId)
		}},
	}
	guards := []struct {
		name   string
		break_ func(env *opsEnv)
		reason string
	}{
		{"clone not deleting", func(env *opsEnv) {
			clone := env.clone(drainCloneName)
			clone.Deleting = false
			mustPut(env.t, env.cli,
				CloneKey(env.cid, opsSpId, drainCloneName), clone)
		}, "clone not deleting"},
		{"clone not found", func(env *opsEnv) {
			env.delKey(CloneKey(env.cid, opsSpId, drainCloneName))
		}, "clone not found"},
		{"clone id changed", func(env *opsEnv) {
			clone := env.clone(drainCloneName)
			clone.CloneId = drainCloneId + 1
			mustPut(env.t, env.cli,
				CloneKey(env.cid, opsSpId, drainCloneName), clone)
		}, "clone id changed"},
		{"sp not found", func(env *opsEnv) {
			env.delKey(SpConfKey(env.cid, opsSpName))
		}, "sp not found"},
		{"sp id changed", func(env *opsEnv) {
			conf := env.spConf()
			conf.SpId = opsSpId + 1
			mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
		}, "sp id changed"},
		{"sp deleting", func(env *opsEnv) {
			// Impossible by precondition — DeleteStoragePool needs an empty
			// clone_name_list, and a draining clone keeps its name there — but
			// guarded anyway, so the two drains can never run on one SP.
			env.setDeleting()
		}, "sp deleting"},
	}
	for _, op := range ops {
		for _, guard := range guards {
			t.Run(op.name+"/"+guard.name, func(t *testing.T) {
				env := newOpsEnv(t)
				env.putDrainClone(true, [][2]uint32{{0, 0}})
				guard.break_(env)
				err := op.run(env)
				precondition := wantPrecondition(t, err, op.op)
				if precondition.Reason != guard.reason {
					t.Errorf("Reason: got %q, want %q",
						precondition.Reason, guard.reason)
				}
				// Nothing was written: the chunk key and the clone name are
				// the two things these ops remove.
				if guard.reason != "sp not found" &&
					guard.reason != "sp id changed" {
					if got := env.drainCloneChunkCnt(); got != 1 {
						t.Errorf("a refused op deleted the chunk key")
					}
				}
			})
		}
		t.Run(op.name+"/commits on a latched clone", func(t *testing.T) {
			env := newOpsEnv(t)
			env.putDrainClone(true, [][2]uint32{{0, 0}})
			if err := op.run(env); err != nil {
				t.Fatalf("%s on a latched clone: %v", op.name, err)
			}
		})
	}
}

// TestCloneDrainLeavesTheSpAlone pins what the drain must NOT touch: it is
// ledger-free, so no DN or CN key moves, and the SP's own inventory is
// untouched but for the one name the final STM removes.
//
// Clone chunks are not budgeted to any node and the CN arena units behind the
// metadata wrapper are agent-local, freed by the retire path — which is what
// makes a batch a constant 68 ops whatever the ceilings become.
func TestCloneDrainLeavesTheSpAlone(t *testing.T) {
	env := newOpsEnv(t)
	env.chargeFixture()
	chunks := env.putDrainClone(true, [][2]uint32{{0, 0}, {2, 1}})
	before := struct {
		dnA, dnB, cnA uint64
		dnARev        uint64
		cnARev        uint64
		slice         *pb.Slice
	}{
		env.dn(opsDnA).GetFreeExtCnt(), env.dn(opsDnB).GetFreeExtCnt(),
		env.cn(opsCnA).GetFreeExtCnt(),
		env.dnRev(opsDnA), env.cnRev(opsCnA), env.slice(),
	}

	if _, err := DrainCloneBm(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, drainCloneName, drainCloneId, chunks); err != nil {
		t.Fatalf("DrainCloneBm: %v", err)
	}
	if err := FinishCloneDelete(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, drainCloneName, drainCloneId); err != nil {
		t.Fatalf("FinishCloneDelete: %v", err)
	}
	if got := env.dn(opsDnA).GetFreeExtCnt(); got != before.dnA {
		t.Errorf("dn-a free_ext_cnt moved: %d, want %d", got, before.dnA)
	}
	if got := env.dn(opsDnB).GetFreeExtCnt(); got != before.dnB {
		t.Errorf("dn-b free_ext_cnt moved: %d, want %d", got, before.dnB)
	}
	if got := env.cn(opsCnA).GetFreeExtCnt(); got != before.cnA {
		t.Errorf("cn-a free_ext_cnt moved: %d, want %d", got, before.cnA)
	}
	if got := env.dnRev(opsDnA); got != before.dnARev {
		t.Errorf("the clone drain bumped a DnRev: %d, want %d",
			got, before.dnARev)
	}
	if got := env.cnRev(opsCnA); got != before.cnARev {
		t.Errorf("the clone drain bumped a CnRev: %d, want %d",
			got, before.cnARev)
	}
	conf := env.spConf()
	if len(conf.GetSliceIdList()) != 1 || len(conf.GetCntlrIdList()) != 2 {
		t.Errorf("the clone drain changed the SP's inventory: %v", conf)
	}
	if len(conf.GetCloneNameList()) != 0 {
		t.Errorf("clone_name_list: got %v, want empty",
			conf.GetCloneNameList())
	}
}
