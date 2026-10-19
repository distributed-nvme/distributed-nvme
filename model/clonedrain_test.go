package model

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The clone drain's own suite (dnv-worker.md, The clone drain; model half).
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
		Deleting:    deleting,
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

// setCloneMetaUnits gives a planted clone its meta_units, as CreateClone
// writes them (architecture.md, Metadata units). No node is charged here.
func (e *opsEnv) setCloneMetaUnits(name string, units uint64) {
	e.t.Helper()
	clone := e.clone(name)
	clone.MetaUnits = units
	mustPut(e.t, e.cli, CloneKey(e.cid, opsSpId, name), clone)
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
// leaves the Clone record alone, and leaves SpRev alone too — no bump, and no
// rewrite of the same value.
//
// "Exactly the chunks it was handed" is the load-bearing half. An STM cannot
// range, so the set a batch may touch has to come in from the caller's snapshot
// scan; a batch that derived the set itself — from the rectangle the record's
// geometry implies, as a sweep over the rectangle would — would be back to a
// transaction whose size is the clone's shape.
//
// The SpRev half is the batch's other contract: a batch changes nothing a side
// or a cntlr is sent, the clone having left every request with the latch
// (CLD5), so a bump would re-send the SP's whole desired state once per batch
// for deletes no agent reads. The key's mod_revision is compared as well as
// its value, so a put of the same revision fails here too.
func TestDrainCloneBm(t *testing.T) {
	env := newOpsEnv(t)
	chunks := env.putDrainClone(true, [][2]uint32{
		{0, 0}, {0, 2}, {3, 1}, {5, 0},
	})
	before := env.clone(drainCloneName)
	revBefore := env.spRev()
	modBefore := env.modRev(SpRevKey(opsShard, env.cid, opsSpId))

	removed, err := DrainCloneBm(env.ctx, env.cli, env.cid, opsSpId,
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
	// CLD8: the record is untouched. proto.Equal against the pre-batch record
	// is the whole statement — the drain's choice of step consults nothing in
	// the record (CLD7), and a batch must write nothing into it (CLD8),
	// including a field added later.
	if got := env.clone(drainCloneName); !proto.Equal(got, before) {
		t.Errorf("the batch rewrote the Clone: got %v, want %v", got, before)
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("SpRev: got %d, want %d (a batch bumps nothing)",
			got, revBefore)
	}
	if got := env.modRev(SpRevKey(opsShard, env.cid, opsSpId)); got != modBefore {
		t.Errorf("SpRev mod_revision: got %d, want %d (a batch writes no "+
			"SpRev, not even its own value)", got, modBefore)
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
// op its entire scan would silently rebuild an unbounded sweep,
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

	_, err := DrainCloneBm(env.ctx, env.cli, env.cid, opsSpId,
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
	if _, err := DrainCloneBm(env.ctx, env.cli, env.cid, opsSpId,
		opsSpName, drainCloneName, drainCloneId,
		chunks[:common.MaxDelBmPerTxn]); err != nil {
		t.Fatalf("a batch of exactly MaxDelBmPerTxn must commit: %v", err)
	}
}

// TestDrainCloneBmIsIdempotent is the accepted two-owner overlap: a Del of a
// key another driver already removed is harmless, neither batch bumps SpRev
// (CLD8), and an empty batch is a no-op that writes nothing rather than a
// refusal.
func TestDrainCloneBmIsIdempotent(t *testing.T) {
	env := newOpsEnv(t)
	chunks := env.putDrainClone(true, [][2]uint32{{0, 0}, {1, 1}})
	revStart := env.spRev()
	if _, err := DrainCloneBm(env.ctx, env.cli, env.cid, opsSpId,
		opsSpName, drainCloneName, drainCloneId, chunks); err != nil {
		t.Fatalf("DrainCloneBm: %v", err)
	}
	revAfter := env.spRev()
	if revAfter != revStart {
		t.Errorf("SpRev after the winner's batch: got %d, want %d (a batch "+
			"bumps nothing)", revAfter, revStart)
	}

	// The loser of the race, handed the same batch its own snapshot found.
	removed, err := DrainCloneBm(env.ctx, env.cli, env.cid, opsSpId,
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
	if got := env.spRev(); got != revAfter {
		t.Errorf("SpRev after the loser's batch: got %d, want %d", got,
			revAfter)
	}

	// An EMPTY batch writes nothing at all.
	revBefore := env.spRev()
	removed, err = DrainCloneBm(env.ctx, env.cli, env.cid, opsSpId,
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

// TestDrainCloneBmReadsNoRevision is CLD8's other half: a batch does not read
// the pool's revision key, because the SpConf it loads already refuses a pool
// that is gone (CLD2) — the sp drain deletes the SpConf and the SpRev key in
// one transaction (SPD12), so no committed state holds the one without the
// other.
//
// Every step runs against a store that breaks that pairing on purpose. With
// the SpRev key gone and the SpConf kept, the batch commits and leaves the key
// absent: a batch that refused on the missing key, as BumpSpRev does, fails
// here, and so does one that wrote the key back. With the key holding bytes
// no SpRev decodes from, the batch commits and leaves the key's mod_revision
// alone: a Get of the key into a SpRev fails the batch on the decode, and
// any write moves the mod_revision. With the SpConf gone as well, the batch
// refuses with "sp not found" and deletes nothing: the guard a revision read
// would only have repeated still holds. The one read no step sees is a bare
// Rev of the key, which decodes no value and refuses nothing.
func TestDrainCloneBmReadsNoRevision(t *testing.T) {
	env := newOpsEnv(t)
	chunks := env.putDrainClone(true, [][2]uint32{{0, 0}, {1, 1}, {2, 2}})
	revKey := SpRevKey(opsShard, env.cid, opsSpId)
	env.delKey(revKey)

	removed, err := DrainCloneBm(env.ctx, env.cli, env.cid, opsSpId,
		opsSpName, drainCloneName, drainCloneId, chunks[:1])
	if err != nil {
		t.Fatalf("a batch with the SpRev key gone: %v (a batch must not "+
			"read the key)", err)
	}
	if removed != 1 {
		t.Errorf("removed %d chunks, want 1", removed)
	}
	if env.exists(CloneBitmapKey(env.cid, opsSpId, drainCloneName,
		chunks[0].SliceIdx, chunks[0].Idx)) {
		t.Errorf("chunk (%d, %d) survived the batch",
			chunks[0].SliceIdx, chunks[0].Idx)
	}
	if env.exists(revKey) {
		t.Errorf("the batch wrote the SpRev key back")
	}

	// Field 1 of a SpRev is the string sp_name, and these bytes are not
	// UTF-8, so a Get of the key into a SpRev fails on the decode.
	mustPut(t, env.cli, revKey, &pb.CloneBitmap{Bitmap: []byte{0xff, 0xfe}})
	modBefore := env.modRev(revKey)
	removed, err = DrainCloneBm(env.ctx, env.cli, env.cid, opsSpId,
		opsSpName, drainCloneName, drainCloneId, chunks[1:2])
	if err != nil {
		t.Fatalf("a batch with an undecodable SpRev value: %v (a batch "+
			"must not read the key)", err)
	}
	if removed != 1 {
		t.Errorf("removed %d chunks, want 1", removed)
	}
	if env.exists(CloneBitmapKey(env.cid, opsSpId, drainCloneName,
		chunks[1].SliceIdx, chunks[1].Idx)) {
		t.Errorf("chunk (%d, %d) survived the batch",
			chunks[1].SliceIdx, chunks[1].Idx)
	}
	if got := env.modRev(revKey); got != modBefore {
		t.Errorf("SpRev mod_revision: got %d, want %d (the batch wrote "+
			"the key)", got, modBefore)
	}

	env.delKey(SpConfKey(env.cid, opsSpName))
	_, err = DrainCloneBm(env.ctx, env.cli, env.cid, opsSpId,
		opsSpName, drainCloneName, drainCloneId, chunks[2:3])
	precondition := wantPrecondition(t, err, opDrainCloneBm)
	if precondition.Reason != "sp not found" {
		t.Errorf("Reason: got %q, want %q", precondition.Reason,
			"sp not found")
	}
	if got := env.drainCloneChunkCnt(); got != 1 {
		t.Errorf("%d chunk keys after the refused batch, want 1", got)
	}
}

// TestCloneDrainConcurrentDriversConverge is the design's "two concurrent
// drivers converge without error amplification" — CLD12's overlap-loser case
// and CLD8's idempotent pop — run for real: two goroutines drive the
// same clone's drain to the end at the same time, and the clone must end up
// gone with neither reporting a failure that is not one of the named benign
// races. The clone's metadata units and the extents of its clone meta group
// come back exactly once: the loser of the final STM finds the clone gone,
// so a second release, which would take other units off each CN and credit
// each DN twice, never happens. And the pool's revision moves exactly
// once over the whole drain, for the final STM (CLD12): ten or more batches
// from two drivers bump nothing (CLD8).
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
	// 3 units of the clone and 5 of another SP on each cntlr's CN.
	env.setCloneMetaUnits(drainCloneName, 3)
	env.setCnUsed(opsCnA, 3+5)
	env.setCnUsed(opsCnB, 3+5)
	grp := opsCloneGrp(true)
	env.setCloneGrp(drainCloneName, grp)
	env.chargeGrp(grp)
	dnRevs := map[string]uint64{
		opsDnC: env.dnRev(opsDnC), opsDnD: env.dnRev(opsDnD),
	}
	revBefore := env.spRev()
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
						drainCloneId, env.cc)
				} else {
					_, err = DrainCloneBm(env.ctx, env.cli, env.cid,
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
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d (one bump, the final STM's, "+
			"however many batches the two drivers committed)",
			got, revBefore+1)
	}
	// The SP is intact: two drivers rewriting one SpConf is the shape most
	// likely to lose an unrelated list.
	conf := env.spConf()
	if len(conf.GetSliceIdList()) != 1 || len(conf.GetCntlrIdList()) != 2 ||
		len(conf.GetTdNameList()) != 2 {
		t.Errorf("the concurrent drain damaged the SpConf: %v", conf)
	}
	for _, addrPort := range []string{opsCnA, opsCnB} {
		if got := env.cn(addrPort).GetUsedMetaUnits(); got != 5 {
			t.Errorf("%s used_meta_units: got %d, want exactly 5 — the "+
				"clone's 3 given back once", addrPort, got)
		}
	}
	for addrPort, rev := range dnRevs {
		dn := env.dn(addrPort)
		if dn.GetFreeExtCnt() != opsDnFree || len(dn.GetSidePtrList()) != 0 {
			t.Errorf("%s: free %d with pointers %v, want %d and none — "+
				"the group's side given back once", addrPort,
				dn.GetFreeExtCnt(), dn.GetSidePtrList(), opsDnFree)
		}
		if got := env.dnRev(addrPort); got != rev+1 {
			t.Errorf("%s rev: got %d, want %d (one bump)", addrPort, got,
				rev+1)
		}
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
		opsSpName, drainCloneName, drainCloneId, env.cc); err != nil {
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

// finishDrainClone runs the final STM on the fixture's drain clone.
func (e *opsEnv) finishDrainClone() error {
	return FinishCloneDelete(e.ctx, e.cli, e.cid, opsShard, opsSpId,
		opsSpName, drainCloneName, drainCloneId, e.cc)
}

// TestFinishCloneDeleteReleasesMetaUnits pins the units ledger the clone
// drain moves (architecture.md, Metadata units): the final STM gives the
// clone's meta_units back on the CN of every cntlr of the SP, and the counts
// other clones charged stay. The capacity values follow the records. Only a
// count moves, so no CN's free extents, pointers or revision move. The clone
// has no group here, so no DN key is touched at all; the DN ledger is
// TestFinishCloneDeleteGivesTheGroupBack's.
func TestFinishCloneDeleteReleasesMetaUnits(t *testing.T) {
	env := newOpsEnv(t)
	env.chargeFixture()
	env.putDrainClone(true, nil)
	env.setCloneMetaUnits(drainCloneName, 3)
	env.putMetaClone("other", 0x901, 5, false)
	env.setCnUsed(opsCnA, 3+5)
	env.setCnUsed(opsCnB, 3+5)
	type node struct {
		cn  *pb.CnConf
		rev uint64
	}
	cns := map[string]node{}
	for _, addrPort := range []string{opsCnA, opsCnB} {
		cns[addrPort] = node{env.cn(addrPort), env.cnRev(addrPort)}
	}
	dns := map[string]*pb.DnConf{}
	dnRevs := map[string]uint64{}
	for _, addrPort := range []string{opsDnA, opsDnB} {
		dns[addrPort] = env.dn(addrPort)
		dnRevs[addrPort] = env.dnRev(addrPort)
	}

	if err := env.finishDrainClone(); err != nil {
		t.Fatalf("FinishCloneDelete: %v", err)
	}
	for addrPort, before := range cns {
		cn := env.cn(addrPort)
		if cn.GetUsedMetaUnits() != 5 {
			t.Errorf("%s used_meta_units: got %d, want 5 (the other "+
				"clone's)", addrPort, cn.GetUsedMetaUnits())
		}
		if got := env.cnCapValue(addrPort).GetUsedMetaUnits(); got != 5 {
			t.Errorf("%s capacity value: got %d used, want 5", addrPort, got)
		}
		// Everything but the count is as it was.
		want := proto.Clone(before.cn).(*pb.CnConf)
		want.UsedMetaUnits = 5
		if !proto.Equal(cn, want) {
			t.Errorf("%s: got %v, want only the count moved: %v",
				addrPort, cn, want)
		}
		if got := env.cnRev(addrPort); got != before.rev {
			t.Errorf("%s rev: got %d, want %d (a count raises no revision)",
				addrPort, got, before.rev)
		}
	}
	for addrPort, before := range dns {
		if got := env.dn(addrPort); !proto.Equal(got, before) {
			t.Errorf("%s moved: %v", addrPort, got)
		}
		if got := env.dnRev(addrPort); got != dnRevs[addrPort] {
			t.Errorf("%s rev: got %d, want %d", addrPort, got,
				dnRevs[addrPort])
		}
	}
	if env.exists(CloneKey(env.cid, opsSpId, drainCloneName)) {
		t.Errorf("the clone key survived")
	}
}

// TestFinishCloneDeleteReleaseStopsAtZero pins the release's floor: a CN
// that holds fewer units than the clone's ends at zero, never at a count
// wrapped around, and a CN already at zero has nothing to give back and is
// not written at all.
func TestFinishCloneDeleteReleaseStopsAtZero(t *testing.T) {
	env := newOpsEnv(t)
	env.putDrainClone(true, nil)
	env.setCloneMetaUnits(drainCloneName, 3)
	env.setCnUsed(opsCnA, 1)
	cnBRev := env.modRev(CnConfKey(env.cid, opsCnB))

	if err := env.finishDrainClone(); err != nil {
		t.Fatalf("FinishCloneDelete: %v", err)
	}
	if got := env.cn(opsCnA).GetUsedMetaUnits(); got != 0 {
		t.Errorf("cn-a used_meta_units: got %d, want 0", got)
	}
	if got := env.cnCapValue(opsCnA).GetUsedMetaUnits(); got != 0 {
		t.Errorf("cn-a capacity value: got %d used, want 0", got)
	}
	if got := env.modRev(CnConfKey(env.cid, opsCnB)); got != cnBRev {
		t.Errorf("cn-b, already at zero, was rewritten: mod_revision %d, "+
			"want %d", got, cnBRev)
	}
}

// TestFinishCloneDeleteSkipsALostCn is releaseSpCns's stance on a RECEIVING
// ledger: a CN record that is gone has nothing left to give the units back
// to, so the release skips it — without writing it back into existence — and
// the clone still goes, its other CN credited.
func TestFinishCloneDeleteSkipsALostCn(t *testing.T) {
	env := newOpsEnv(t)
	env.putDrainClone(true, nil)
	env.setCloneMetaUnits(drainCloneName, 3)
	env.setCnUsed(opsCnA, 3)
	env.delKey(CnConfKey(env.cid, opsCnB))

	if err := env.finishDrainClone(); err != nil {
		t.Fatalf("FinishCloneDelete over a lost cn_conf: %v", err)
	}
	if env.exists(CloneKey(env.cid, opsSpId, drainCloneName)) {
		t.Errorf("the clone key survived")
	}
	if got := env.cn(opsCnA).GetUsedMetaUnits(); got != 0 {
		t.Errorf("cn-a used_meta_units: got %d, want 0", got)
	}
	if env.exists(CnConfKey(env.cid, opsCnB)) {
		t.Errorf("the lost cn_conf was written back")
	}
}

// TestFinishCloneDeleteRefusesALostCntlr is the DESCRIBING key's side of the
// same asymmetry, DrainSpCntlrs's: a listed Cntlr whose key is missing is
// what would say which CN holds the clone's units, so the release cannot be
// made exact and the op refuses, writing nothing — the clone stays latched,
// still listed, and every CN keeps its count.
func TestFinishCloneDeleteRefusesALostCntlr(t *testing.T) {
	env := newOpsEnv(t)
	env.putDrainClone(true, nil)
	env.setCloneMetaUnits(drainCloneName, 3)
	env.setCnUsed(opsCnA, 3)
	env.setCnUsed(opsCnB, 3)
	env.delKey(CntlrKey(env.cid, opsSpId, opsCntlrB))
	revBefore := env.spRev()

	err := env.finishDrainClone()
	precondition := wantPrecondition(t, err, opFinishCloneDelete)
	if precondition.Reason != "cntlr not found" {
		t.Errorf("Reason: got %q, want %q", precondition.Reason,
			"cntlr not found")
	}
	if !env.exists(CloneKey(env.cid, opsSpId, drainCloneName)) {
		t.Errorf("a refused final STM deleted the clone key")
	}
	if got := env.spConf().GetCloneNameList(); len(got) != 1 {
		t.Errorf("clone_name_list: got %v, want the name kept", got)
	}
	for _, addrPort := range []string{opsCnA, opsCnB} {
		if got := env.cn(addrPort).GetUsedMetaUnits(); got != 3 {
			t.Errorf("%s used_meta_units: got %d, want 3", addrPort, got)
		}
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("SpRev: got %d, want %d", got, revBefore)
	}
}

// TestFinishCloneDeleteReleasesTwiceOnASharedCn: two cntlrs of one SP on one
// CN — a shape architecture.md, Finding CN candidates, forbids but nothing in
// the store enforces — were each charged the clone, so the CN gets the units
// back twice, in its one write, and still no revision.
func TestFinishCloneDeleteReleasesTwiceOnASharedCn(t *testing.T) {
	env := newOpsEnv(t)
	cntlr := env.cntlr(opsCntlrB)
	cntlr.AddrPort = opsCnA
	cntlr.NvmeTrConf = opsTrConf(opsCnA)
	mustPut(t, env.cli, CntlrKey(env.cid, opsSpId, opsCntlrB), cntlr)
	env.putDrainClone(true, nil)
	env.setCloneMetaUnits(drainCloneName, 3)
	env.setCnUsed(opsCnA, 2*3+5)
	cnARev := env.cnRev(opsCnA)
	cnBMod := env.modRev(CnConfKey(env.cid, opsCnB))

	if err := env.finishDrainClone(); err != nil {
		t.Fatalf("FinishCloneDelete: %v", err)
	}
	if got := env.cn(opsCnA).GetUsedMetaUnits(); got != 5 {
		t.Errorf("cn-a used_meta_units: got %d, want 5 (the clone's 3 back "+
			"once per cntlr)", got)
	}
	if got := env.cnCapValue(opsCnA).GetUsedMetaUnits(); got != 5 {
		t.Errorf("cn-a capacity value: got %d used, want 5", got)
	}
	if got := env.cnRev(opsCnA); got != cnARev {
		t.Errorf("cn-a rev: got %d, want %d", got, cnARev)
	}
	if got := env.modRev(CnConfKey(env.cid, opsCnB)); got != cnBMod {
		t.Errorf("cn-b, hosting no cntlr of the SP, was rewritten")
	}
}

// TestFinishCloneDeleteWithoutUnitsReadsNoCn: a clone that holds no units — a
// record written before the counts existed reads zero — has nothing to give
// back, so the final STM reads no Cntlr and no CN at all. A lost Cntlr key,
// which a release would refuse over, does not stop it, and no CN record is
// rewritten.
func TestFinishCloneDeleteWithoutUnitsReadsNoCn(t *testing.T) {
	env := newOpsEnv(t)
	env.putDrainClone(true, nil)
	env.setCnUsed(opsCnA, 4)
	cnMods := map[string]int64{}
	for _, addrPort := range []string{opsCnA, opsCnB} {
		cnMods[addrPort] = env.modRev(CnConfKey(env.cid, addrPort))
	}
	env.delKey(CntlrKey(env.cid, opsSpId, opsCntlrB))

	if err := env.finishDrainClone(); err != nil {
		t.Fatalf("FinishCloneDelete of a clone with no units: %v", err)
	}
	if env.exists(CloneKey(env.cid, opsSpId, drainCloneName)) {
		t.Errorf("the clone key survived")
	}
	for addrPort, mod := range cnMods {
		if got := env.modRev(CnConfKey(env.cid, addrPort)); got != mod {
			t.Errorf("%s was rewritten for a clone with no units", addrPort)
		}
	}
	if got := env.cn(opsCnA).GetUsedMetaUnits(); got != 4 {
		t.Errorf("cn-a used_meta_units: got %d, want 4", got)
	}
}

// TestFinishCloneDeleteAtTheCeiling commits the final STM at its widest
// shape against the real etcd this package runs with
// --max-txn-ops=common.EtcdMaxTxnOps: MaxCntlrCntPerSp cntlrs, each on a CN
// of its own, so every units release is a read of the Cntlr and of the CN
// and a write of the CN and of its capacity key; and a clone meta group of
// MaxAllocLegPerGrp legs and MaxSpareLegPerGrp spares, each side on a DN of
// its own, so every side given back is a read and a write of its DN, of its
// DnRev, and a delete and a put of its capacity key. Every CN and DN is
// credited, and the whole release is one transaction with the clone's
// removal.
func TestFinishCloneDeleteAtTheCeiling(t *testing.T) {
	env := newOpsEnv(t)
	conf := env.spConf()
	addrs := []string{opsCnA, opsCnB}
	for idx := len(conf.GetCntlrIdList()); idx < common.MaxCntlrCntPerSp; idx++ {
		addrPort := fmt.Sprintf("ceil-cn-%d:9000", idx)
		cntlrId := opsCntlrB + uint64(idx)
		env.putCn(addrPort, uint64(810+idx), uint32(idx), opsCnFree)
		mustPut(t, env.cli, CntlrKey(env.cid, opsSpId, cntlrId), &pb.Cntlr{
			AddrPort:   addrPort,
			NvmeTrConf: opsTrConf(addrPort),
			CntlidSlot: uint32(idx),
		})
		conf.CntlrIdList = append(conf.CntlrIdList, cntlrId)
		addrs = append(addrs, addrPort)
	}
	mustPut(t, env.cli, SpConfKey(env.cid, opsSpName), conf)
	env.putDrainClone(true, nil)
	env.setCloneMetaUnits(drainCloneName, 3)
	for _, addrPort := range addrs {
		env.setCnUsed(addrPort, 3+5)
	}
	var dnAddrs []string
	for idx := 0; idx < common.MaxAllocLegPerGrp+common.MaxSpareLegPerGrp; idx++ {
		addrPort := fmt.Sprintf("ceil-dn-%d:9000", idx)
		env.putDn(addrPort, uint64(910+idx), uint32(idx), opsDnFree)
		dnAddrs = append(dnAddrs, addrPort)
	}
	grp := cloneGrpAt(opsCloneGrpId, true, dnAddrs...)
	grp.SpareLegList = grp.LegList[common.MaxAllocLegPerGrp:]
	grp.LegList = grp.LegList[:common.MaxAllocLegPerGrp]
	env.setCloneGrp(drainCloneName, grp)
	env.chargeGrp(grp)

	if err := env.finishDrainClone(); err != nil {
		t.Fatalf("a maximum-shape FinishCloneDelete (%d cntlrs on as many "+
			"CNs, %d sides on as many DNs) did not commit: %v",
			common.MaxCntlrCntPerSp, len(dnAddrs), err)
	}
	rev := env.modRev(SpConfKey(env.cid, opsSpName))
	for _, addrPort := range addrs {
		if got := env.cn(addrPort).GetUsedMetaUnits(); got != 5 {
			t.Errorf("%s used_meta_units: got %d, want 5", addrPort, got)
		}
		if got := env.modRev(CnConfKey(env.cid, addrPort)); got != rev {
			t.Errorf("%s: mod_revision %d, want the op's %d",
				addrPort, got, rev)
		}
	}
	for _, addrPort := range dnAddrs {
		dn := env.dn(addrPort)
		if dn.GetFreeExtCnt() != opsDnFree || len(dn.GetSidePtrList()) != 0 {
			t.Errorf("%s: free %d with pointers %v, want %d and none",
				addrPort, dn.GetFreeExtCnt(), dn.GetSidePtrList(), opsDnFree)
		}
		if got := env.modRev(DnConfKey(env.cid, addrPort)); got != rev {
			t.Errorf("%s: mod_revision %d, want the op's %d",
				addrPort, got, rev)
		}
	}
}

// TestFinishCloneDeleteGivesTheGroupBack pins the DN ledger of the final STM
// (dnv-worker.md CLD9): every side of the clone meta group, legs and spares,
// goes back to its DN as a slice drain batch gives a group's back — the
// pointer out, the group's ext_cnt back, the capacity key moved with the free
// count it embeds, one DnRev bump per DN — in the transaction that removes
// the clone. Only the clone's own pointers leave: the slice's sides on the
// same DN, and another SP's pointer that shares a side_id, stay. A DN the
// group does not use is not touched.
func TestFinishCloneDeleteGivesTheGroupBack(t *testing.T) {
	env := newOpsEnv(t)
	env.chargeFixture()
	env.putDrainClone(true, nil)
	grp := opsCloneGrp(true)
	// A spare on dn-a, which carries the slice's two sides of that DN.
	grp.SpareLegList = []*pb.Leg{{
		LegId:    opsCloneSpareLeg,
		LegIdx:   2,
		SideList: []*pb.Side{opsSide(opsCloneSpareSide, opsDnA, true)},
	}}
	env.setCloneGrp(drainCloneName, grp)
	env.chargeGrp(grp)
	// A pointer of ANOTHER sp that collides on side_id, for
	// TestDrainSpSliceAccounting's reason.
	foreign := &pb.SidePointer{
		SpId: opsSpId + 1, LegId: opsCloneLegA, SideId: opsCloneSideA,
	}
	dnC := env.dn(opsDnC)
	dnC.SidePtrList = append(dnC.GetSidePtrList(), foreign)
	mustPut(t, env.cli, DnConfKey(env.cid, opsDnC), dnC)
	type node struct {
		dn  *pb.DnConf
		rev uint64
	}
	before := map[string]node{}
	for _, addrPort := range []string{opsDnA, opsDnB, opsDnC, opsDnD} {
		before[addrPort] = node{env.dn(addrPort), env.dnRev(addrPort)}
	}
	cnB := env.cn(opsCnB)
	revBefore := env.spRev()

	if err := env.finishDrainClone(); err != nil {
		t.Fatalf("FinishCloneDelete: %v", err)
	}
	for addrPort, want := range map[string]struct {
		ptrs []*pb.SidePointer
	}{
		opsDnA: {before[opsDnA].dn.GetSidePtrList()[:2]},
		opsDnC: {[]*pb.SidePointer{foreign}},
		opsDnD: {nil},
	} {
		dn := env.dn(addrPort)
		wantFree := before[addrPort].dn.GetFreeExtCnt() +
			common.CloneMetaGrpExtCnt
		if dn.GetFreeExtCnt() != wantFree {
			t.Errorf("%s: free_ext_cnt %d, want %d", addrPort,
				dn.GetFreeExtCnt(), wantFree)
		}
		if len(dn.GetSidePtrList()) != len(want.ptrs) {
			t.Errorf("%s: side_ptr_list %v, want %v", addrPort,
				dn.GetSidePtrList(), want.ptrs)
		} else {
			for idx, ptr := range want.ptrs {
				if !proto.Equal(dn.GetSidePtrList()[idx], ptr) {
					t.Errorf("%s: side_ptr_list %v, want %v", addrPort,
						dn.GetSidePtrList(), want.ptrs)
					break
				}
			}
		}
		if got := env.dnRev(addrPort); got != before[addrPort].rev+1 {
			t.Errorf("%s rev: got %d, want %d (one bump)", addrPort, got,
				before[addrPort].rev+1)
		}
		oldFree := before[addrPort].dn.GetFreeExtCnt()
		if env.exists(DnCapacityKey(
			env.cid, mustBin(t, env, oldFree), oldFree, addrPort)) {
			t.Errorf("%s: the charged capacity key survived", addrPort)
		}
		if !env.exists(DnCapacityKey(
			env.cid, mustBin(t, env, wantFree), wantFree, addrPort)) {
			t.Errorf("%s: no capacity key for the restored free count",
				addrPort)
		}
	}
	if got := env.dn(opsDnB); !proto.Equal(got, before[opsDnB].dn) {
		t.Errorf("dn-b, outside the group, moved: %v", got)
	}
	if got := env.dnRev(opsDnB); got != before[opsDnB].rev {
		t.Errorf("dn-b rev: got %d, want %d", got, before[opsDnB].rev)
	}
	// The clone meta group is in no cntlr's footprint, so no CN extent
	// moves for it.
	if got := env.cn(opsCnB); !proto.Equal(got, cnB) {
		t.Errorf("cn-b moved: %v", got)
	}
	if env.exists(CloneKey(env.cid, opsSpId, drainCloneName)) {
		t.Errorf("the clone key survived")
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d (one bump)", got, revBefore+1)
	}
}

// TestFinishCloneDeleteSkipsALostDn is the RECEIVING ledger's stance of
// DrainSpSlice: a DN record that is gone has nothing left to credit, so the
// release skips it — without writing it back into existence — and the clone
// still goes, its other DN credited.
func TestFinishCloneDeleteSkipsALostDn(t *testing.T) {
	env := newOpsEnv(t)
	env.putDrainClone(true, nil)
	grp := opsCloneGrp(true)
	env.setCloneGrp(drainCloneName, grp)
	env.chargeGrp(grp)
	env.delKey(DnConfKey(env.cid, opsDnD))

	if err := env.finishDrainClone(); err != nil {
		t.Fatalf("FinishCloneDelete over a lost dn_conf: %v", err)
	}
	if env.exists(CloneKey(env.cid, opsSpId, drainCloneName)) {
		t.Errorf("the clone key survived")
	}
	if env.exists(DnConfKey(env.cid, opsDnD)) {
		t.Errorf("the lost dn_conf was written back")
	}
	if dn := env.dn(opsDnC); dn.GetFreeExtCnt() != opsDnFree ||
		len(dn.GetSidePtrList()) != 0 {
		t.Errorf("dn-c: free %d with pointers %v, want %d and none",
			dn.GetFreeExtCnt(), dn.GetSidePtrList(), opsDnFree)
	}
}

// TestFinishCloneDeleteRefusesAnInvalidStoredConf is the gate
// (architecture.md, Common validation) the final STM needs for
// MaintainDnCapacity's sake, TestDrainSliceRefusesAnInvalidStoredConf's
// reason: a capacity key embeds the bin index the stored ladder yields. The
// refusal writes nothing, a missing conf is refused like an invalid one, and
// the very same STM commits once the conf is repaired.
func TestFinishCloneDeleteRefusesAnInvalidStoredConf(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(env *opsEnv)
		reason string
	}{
		{"extent_size", func(env *opsEnv) {
			env.cc.DnBinConf.ExtentSize = 0
		}, "invalid stored conf: dn_bin_conf.extent_size is zero"},
		{"dn_batch_size", func(env *opsEnv) {
			env.cc.AllocConf.DnBatchSize = 0
		}, "invalid stored conf: alloc_conf.dn_batch_size 0 is outside [1, 1024]"},
		{"missing", func(env *opsEnv) {
			env.cc = nil
		}, "invalid stored conf: dn_bin_conf.extent_size is zero"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			env.putDrainClone(true, nil)
			grp := opsCloneGrp(true)
			env.setCloneGrp(drainCloneName, grp)
			env.chargeGrp(grp)
			tc.mutate(env)
			before := env.storeRev()
			precondition := wantPrecondition(t, env.finishDrainClone(),
				opFinishCloneDelete)
			if precondition.Reason != tc.reason {
				t.Errorf("Reason: got %q, want %q", precondition.Reason,
					tc.reason)
			}
			if got := env.storeRev(); got != before {
				t.Errorf("the refusal wrote: store revision %d, want %d",
					got, before)
			}
			env.cc = testClusterConf()
			env.cc.DnBinConf.ExtentSize = opsExtSize
			if err := env.finishDrainClone(); err != nil {
				t.Fatalf("FinishCloneDelete with the conf repaired: %v", err)
			}
			if got := env.dn(opsDnC).GetFreeExtCnt(); got != opsDnFree {
				t.Errorf("dn-c free_ext_cnt: got %d, want %d", got,
					opsDnFree)
			}
		})
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
			_, err := DrainCloneBm(env.ctx, env.cli, env.cid,
				opsSpId, opsSpName, drainCloneName, drainCloneId,
				[]BmChunk{{SliceIdx: 0, Idx: 0}})
			return err
		}},
		{"FinishCloneDelete", opFinishCloneDelete, func(env *opsEnv) error {
			return FinishCloneDelete(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, drainCloneName, drainCloneId, env.cc)
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

// TestCloneDrainLeavesTheSpAlone pins what the drain must NOT touch. Its
// batches are ledger-free — clone chunks are not budgeted to any node, and a
// batch bumps no SpRev, which is what makes it MaxDelBmPerTxn plus two
// compares whatever the ceilings become — and its final STM moves only what
// the clone itself was charged: its metadata units on its cntlrs' CNs
// (TestFinishCloneDeleteReleasesMetaUnits) and the extents of its own clone
// meta group on that group's DNs (TestFinishCloneDeleteGivesTheGroupBack).
// So no DN of the slice moves, no CN's free extents or revision move, the
// SP's own inventory is untouched but for the one name the final STM
// removes, and its revision moves once, for that final STM (CLD12). The run
// behind the metadata wrapper is agent-local, freed by the cn agent's sweep
// (cnagent.md CN21).
func TestCloneDrainLeavesTheSpAlone(t *testing.T) {
	env := newOpsEnv(t)
	env.chargeFixture()
	chunks := env.putDrainClone(true, [][2]uint32{{0, 0}, {2, 1}})
	env.setCloneMetaUnits(drainCloneName, 3)
	env.setCnUsed(opsCnA, 3)
	env.setCnUsed(opsCnB, 3)
	grp := opsCloneGrp(true)
	env.setCloneGrp(drainCloneName, grp)
	env.chargeGrp(grp)
	before := struct {
		dnA, dnB      *pb.DnConf
		dnARev, cnRev uint64
		cnA           uint64
		slice         *pb.Slice
	}{
		env.dn(opsDnA), env.dn(opsDnB), env.dnRev(opsDnA), env.cnRev(opsCnA),
		env.cn(opsCnA).GetFreeExtCnt(), env.slice(),
	}
	dnCRev := env.dnRev(opsDnC)
	revBefore := env.spRev()

	if _, err := DrainCloneBm(env.ctx, env.cli, env.cid, opsSpId,
		opsSpName, drainCloneName, drainCloneId, chunks); err != nil {
		t.Fatalf("DrainCloneBm: %v", err)
	}
	// The batch moves no node at all, the group's DNs included.
	if got := env.dnRev(opsDnC); got != dnCRev {
		t.Errorf("a batch bumped dn-c's DnRev: %d, want %d", got, dnCRev)
	}
	if err := FinishCloneDelete(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, drainCloneName, drainCloneId, env.cc); err != nil {
		t.Fatalf("FinishCloneDelete: %v", err)
	}
	if got := env.dn(opsDnA); !proto.Equal(got, before.dnA) {
		t.Errorf("dn-a, carrying only the slice's sides, moved: %v", got)
	}
	if got := env.dn(opsDnB); !proto.Equal(got, before.dnB) {
		t.Errorf("dn-b, carrying only the slice's sides, moved: %v", got)
	}
	if got := env.dnRev(opsDnA); got != before.dnARev {
		t.Errorf("the clone drain bumped a slice DN's DnRev: %d, want %d",
			got, before.dnARev)
	}
	if got := env.cn(opsCnA).GetFreeExtCnt(); got != before.cnA {
		t.Errorf("cn-a free_ext_cnt moved: %d, want %d", got, before.cnA)
	}
	if got := env.cnRev(opsCnA); got != before.cnRev {
		t.Errorf("the clone drain bumped a CnRev: %d, want %d",
			got, before.cnRev)
	}
	if got := env.slice(); !proto.Equal(got, before.slice) {
		t.Errorf("the clone drain changed the slice: %v", got)
	}
	// The two charges the clone itself held come back.
	if got := env.cn(opsCnA).GetUsedMetaUnits(); got != 0 {
		t.Errorf("cn-a used_meta_units: got %d, want the clone's 3 back",
			got)
	}
	if got := env.dn(opsDnC).GetFreeExtCnt(); got != opsDnFree {
		t.Errorf("dn-c free_ext_cnt: got %d, want the group's extent back "+
			"(%d)", got, opsDnFree)
	}
	conf := env.spConf()
	if len(conf.GetSliceIdList()) != 1 || len(conf.GetCntlrIdList()) != 2 {
		t.Errorf("the clone drain changed the SP's inventory: %v", conf)
	}
	if len(conf.GetCloneNameList()) != 0 {
		t.Errorf("clone_name_list: got %v, want empty",
			conf.GetCloneNameList())
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d (the batch bumps nothing, the "+
			"final STM once)", got, revBefore+1)
	}
}
