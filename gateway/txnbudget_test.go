package gateway

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestDeleteCloneTxnBudget is the tripwire under §8.9's chunk sweep.
// DeleteClone deletes the whole src_slice_cnt x bm_cnt rectangle of chunk keys
// in ONE transaction — an STM cannot range, so there is no way to split it —
// and the worst case is MaxSliceCntPerSp x MaxCloneBmCnt deletes plus the
// Clone key, the SpConf, the SpRev and the reads it already does. etcd caps
// a transaction at --max-txn-ops, whose DEFAULT is 128, so every etcd serving
// dnv is required to run with common.EtcdMaxTxnOps and every test launcher
// passes it (gateway/etcdenv_test.go and its three siblings).
//
// The assertion is arithmetic on constants and needs no etcd, which is the
// point: raising either cap is a one-line change in common/constants.go whose
// cost lands on a deployment flag nobody would think to re-check, and it fails
// here at once instead of as an "etcd: too many operations in txn request" out
// of a DeleteClone in the field. The 8 is the non-chunk ops, rounded up — it
// is deliberately an UNDER-estimate of the real per-transaction overhead, and
// TestDeleteCloneAtTheChunkCeiling below is what actually proves the budget,
// against a real etcd, so this tripwire only has to fire EARLY, never exactly.
func TestDeleteCloneTxnBudget(t *testing.T) {
	const budget = common.MaxSliceCntPerSp*common.MaxCloneBmCnt + 8
	if budget > common.EtcdMaxTxnOps {
		t.Errorf(
			"DeleteClone's worst case is %d ops (%d slices x %d chunks + 8), "+
				"over the %d dnv requires etcd to allow: raise EtcdMaxTxnOps "+
				"and the --max-txn-ops of every etcd that serves dnv",
			budget, common.MaxSliceCntPerSp, common.MaxCloneBmCnt,
			common.EtcdMaxTxnOps)
	}
}

// TestDeleteCloneAtTheChunkCeiling is the tripwire above turned into a PROOF:
// it fills a clone's whole MaxSliceCntPerSp x MaxCloneBmCnt rectangle — every
// chunk key DeleteClone can ever have to sweep — and deletes it, against the
// real etcd this package runs with `--max-txn-ops=common.EtcdMaxTxnOps`.
//
// The arithmetic tripwire cannot do this. It counts the ops DeleteClone's
// transaction was BELIEVED to issue; only etcd can say how many it actually
// receives, because the deciding STM also carries resumeCloneDstNs's subsystem
// puts, the SpConf put, the SpRev bump and one comparison per key the STM
// read. If any of those grows — a new write in the deciding STM, a wider
// resumeCloneDstNs, a change in how etcdutil builds the txn — this test fails
// with etcd's own "too many operations in txn request" while the tripwire
// stays green.
func TestDeleteCloneAtTheChunkCeiling(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	if _, err := env.srv.CreateClone(env.ctx, &pb.CreateCloneRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		CloneName:   "clone-max",
		SrcTrConf:   []*pb.NvmeTrConf{volTrConf(volCnA)},
		SrcNqn:      volSrcNqn,
		SrcNsIdx:    1,
		// The ceiling itself: every source slice the geometry allows, so the
		// sweep's rectangle is as wide as it can ever be.
		SrcSliceCnt:   common.MaxSliceCntPerSp,
		SrcStripeSize: 64 * 1024,
		SrcBlockSize:  1024 * 1024,
		DstTdName:     "dst",
		DmCloneConf:   &pb.DmCloneConf{HydrationThreshold: 2},
		AutoResume:    true,
	}); err != nil {
		t.Fatalf("CreateClone: %v", err)
	}
	// One byte per chunk: this test is about the transaction's OP COUNT, not
	// its byte size, and 256 one-byte chunks keep it quick.
	for sliceIdx := uint32(0); sliceIdx < common.MaxSliceCntPerSp; sliceIdx++ {
		for bmIdx := uint32(0); bmIdx < common.MaxCloneBmCnt; bmIdx++ {
			_, err := env.srv.AppendCloneBitmap(
				env.ctx, &pb.AppendCloneBitmapRequest{
					ClusterName: env.cluster,
					SpName:      volSpName,
					SpRev:       env.token(),
					CloneName:   "clone-max",
					SrcSliceIdx: sliceIdx,
					BmIdx:       bmIdx,
					Bitmap:      []byte{0xff},
				})
			if err != nil {
				t.Fatalf("AppendCloneBitmap (%d, %d): %v",
					sliceIdx, bmIdx, err)
			}
		}
	}
	if got := env.clone("clone-max").GetBmCnt(); got != common.MaxCloneBmCnt {
		t.Fatalf("bm_cnt: got %d, want %d", got, common.MaxCloneBmCnt)
	}
	// force: the clone has no primary cntlr to prove hydration with, and the
	// proof is not what this test is about.
	if _, err := env.srv.DeleteClone(env.ctx, &pb.DeleteCloneRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		CloneName:   "clone-max",
		Force:       true,
	}); err != nil {
		t.Fatalf("DeleteClone over the full %dx%d rectangle: %v "+
			"(an \"too many operations in txn request\" here means "+
			"common.EtcdMaxTxnOps no longer covers the sweep)",
			common.MaxSliceCntPerSp, common.MaxCloneBmCnt, err)
	}
	// Every one of the 256 keys is gone, not just the ones a narrower sweep
	// would have reached.
	for sliceIdx := uint32(0); sliceIdx < common.MaxSliceCntPerSp; sliceIdx++ {
		for bmIdx := uint32(0); bmIdx < common.MaxCloneBmCnt; bmIdx++ {
			key := model.CloneBitmapKey(
				env.cid, volSpId, "clone-max", sliceIdx, bmIdx)
			if env.exists(key, &pb.CloneBitmap{}) {
				t.Fatalf("chunk (%d, %d) survived the delete",
					sliceIdx, bmIdx)
			}
		}
	}
}

// TestSpDrainBatchBudget is SPD14's arithmetic tripwire: the worst-case D2
// batch must fit inside the transaction size dnv requires of every etcd.
//
// Per §6, with D the number of distinct DNs one batch touches:
//
//	compares = 3 + 2D   (SpConf, Slice, SpRev; per DN DnConf + DnRev)
//	ops      = 3 + 4D   (Slice put/del, SpConf put, SpRev put;
//	                     per DN DnConf put, capacity del + put, DnRev put)
//	total    = 6 + 6D,  D <= MaxDelGrpPerTxn x (MaxAllocLegPerGrp +
//	                                            MaxSpareLegPerGrp)
//
// Every constant is NAMED (SPD1): widening the allocator's group shape, the
// spare ceiling or the batch size past the budget fails here, at once, instead
// of as an "etcd: too many operations in txn request" out of a drain in the
// field. Sides contribute one DN each because a deletable SP has no migrations
// and therefore no two-side legs.
//
// TestDrainSpSliceAtTheCeiling in model/drain_test.go is the PROOF this
// tripwire only approximates: it commits a maximum-shape batch against a real
// etcd started with --max-txn-ops=common.EtcdMaxTxnOps.
func TestSpDrainBatchBudget(t *testing.T) {
	const legs = common.MaxAllocLegPerGrp + common.MaxSpareLegPerGrp
	const dns = common.MaxDelGrpPerTxn * legs
	const budget = 6 + 6*dns
	if budget > common.EtcdMaxTxnOps {
		t.Errorf(
			"one sp drain batch is %d ops (6 + 6 x %d groups x (%d legs + %d "+
				"spares)), over the %d dnv requires etcd to allow: lower "+
				"MaxDelGrpPerTxn or raise EtcdMaxTxnOps and the --max-txn-ops "+
				"of every etcd that serves dnv",
			budget, common.MaxDelGrpPerTxn, common.MaxAllocLegPerGrp,
			common.MaxSpareLegPerGrp, common.EtcdMaxTxnOps)
	}
}
