package agent

import (
	"context"
	"sort"
)

// Bitmap conventions (architecture.md, Bitmap push protocol and
// raid0 bitmap math):
//
//   - Every Gateway/etcd/Push*Bitmap bitmap is "1 = unwritten/skippable";
//     thin-pool metadata and the raid0 bitmap math formulas are
//     "written/copied = 1". The inversion happens exactly once, where thin-pool
//     metadata is read — so
//     the chunks that reach an agent are already in wire convention and a set
//     bit means "this region need not be copied".
//   - Bits are addressed LSB-first inside each byte: bit i lives in
//     bitmap[i/8] at mask 1<<(i%8). When the logical bit count is not a
//     multiple of 8 the trailing pad bits of the last byte are 0.
//   - Chunks are byte-aligned, so migration's bit-level concatenation
//     (chunk k starts at the summed bit length of chunks 0…k−1) is plain byte
//     concatenation.
const bitsPerByte = 8

// BitmapBitCount is the number of bits a chunk carries.
//
// It is for **wire chunks only**, where every bit of every byte is meaningful
// by construction (chunks are byte-aligned; architecture.md,
// raid0 bitmap math). It must never be used as
// the bit count of a bitmap whose logical length is not a multiple of 8:
// there it would count the trailing pad bits as real ones.
func BitmapBitCount(bitmap []byte) uint64 {
	return uint64(len(bitmap)) * bitsPerByte
}

// BitmapBit reads one bit of a bitmap; out-of-range bits read as 0.
func BitmapBit(bitmap []byte, idx uint64) bool {
	byteIdx := idx / bitsPerByte
	if byteIdx >= uint64(len(bitmap)) {
		return false
	}
	return bitmap[byteIdx]&(1<<(idx%bitsPerByte)) != 0
}

// ChunkSet holds the migration bitmap chunks of one migration that the agent
// loaded from disk or received since. bm_idx is the append sequence, so the
// set is interpretable only as a ContiguousPrefix (SH23). The applied set
// reported through BitmapInfo.bm_idx_list is always derived from it, so it
// survives restarts (SH21). Clone chunks are addressed by the pair
// (src_slice_idx, bm_idx) and live in CloneChunkSet below.
type ChunkSet struct {
	chunks map[uint32][]byte
}

func NewChunkSet() *ChunkSet {
	return &ChunkSet{chunks: make(map[uint32][]byte)}
}

func (c *ChunkSet) Put(bmIdx uint32, bitmap []byte) {
	c.chunks[bmIdx] = bitmap
}

func (c *ChunkSet) Delete(bmIdx uint32) {
	delete(c.chunks, bmIdx)
}

// Get reads one chunk by its bm_idx. It places nothing: a migration chunk is
// interpretable only inside the ContiguousPrefix its lower neighbours build
// (SH23).
func (c *ChunkSet) Get(bmIdx uint32) ([]byte, bool) {
	bitmap, ok := c.chunks[bmIdx]
	return bitmap, ok
}

func (c *ChunkSet) Len() int {
	return len(c.chunks)
}

// Indexes lists every chunk present, ascending — the applied set of SH21.
func (c *ChunkSet) Indexes() []uint32 {
	out := make([]uint32, 0, len(c.chunks))
	for bmIdx := range c.chunks {
		out = append(out, bmIdx)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ContiguousPrefix concatenates the longest run of chunks starting at
// bm_idx 0. Migration chunks are interpretable only as such a prefix: chunk
// k's first bit sits at the summed bit length of chunks 0…k−1, so a gap makes
// every later chunk unplaceable (SH23). The worker's ascending, one-in-flight
// push makes gaps unreachable in practice; the applied set (Indexes) still
// reports every chunk in the set, not only this prefix.
func (c *ChunkSet) ContiguousPrefix() []byte {
	var out []byte
	for bmIdx := uint32(0); ; bmIdx++ {
		chunk, ok := c.chunks[bmIdx]
		if !ok {
			return out
		}
		out = append(out, chunk...)
	}
}

// CloneChunkKey addresses one clone bitmap chunk: the source slice it
// describes and its index within that slice's bitmap (architecture.md,
// Bitmap push protocol).
type CloneChunkKey struct {
	SliceIdx uint32
	BmIdx    uint32
}

// CloneChunkSet holds the clone bitmap chunks an agent currently has on disk
// for one clone. Chunk (s, b) carries bytes [b*CloneBmChunkBytes, …) of source
// slice s's bitmap, so every chunk is **self-positioned** by its key alone
// (SH22): chunks may arrive in any order, a chunk may be missing entirely, and
// one may keep growing in place up to its fixed capacity ([D8]). There is
// deliberately no ContiguousPrefix analogue — concatenating clone chunks would
// place them at the wrong offsets; the fold of architecture.md,
// raid0 bitmap math, reads them in place instead.
// The applied set reported through BitmapInfo.chunk_id_list is always derived
// from this set, so it survives restarts (SH21).
type CloneChunkSet struct {
	chunks map[CloneChunkKey][]byte
}

func NewCloneChunkSet() *CloneChunkSet {
	return &CloneChunkSet{chunks: make(map[CloneChunkKey][]byte)}
}

func (c *CloneChunkSet) Put(key CloneChunkKey, bitmap []byte) {
	c.chunks[key] = bitmap
}

func (c *CloneChunkSet) Get(key CloneChunkKey) ([]byte, bool) {
	bitmap, ok := c.chunks[key]
	return bitmap, ok
}

func (c *CloneChunkSet) Delete(key CloneChunkKey) {
	delete(c.chunks, key)
}

func (c *CloneChunkSet) Len() int {
	return len(c.chunks)
}

// Ids lists every chunk present, ascending by (SliceIdx, BmIdx) — the applied
// set of SH21. The order is deterministic only: clone chunks are
// order-independent, so nothing downstream may depend on it for placement.
func (c *CloneChunkSet) Ids() []CloneChunkKey {
	out := make([]CloneChunkKey, 0, len(c.chunks))
	for key := range c.chunks {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SliceIdx != out[j].SliceIdx {
			return out[i].SliceIdx < out[j].SliceIdx
		}
		return out[i].BmIdx < out[j].BmIdx
	})
	return out
}

// SkipRange is one contiguous byte range of a dm-clone device that may be
// marked hydrated without copying.
type SkipRange struct {
	Offset uint64
	Length uint64
}

// SkipRanges turns a wire bitmap into coalesced dm-clone byte ranges.
//
// Bit i of the bitmap describes region shiftRegions+i of the device — the dn
// shift by the leg's meta_blocks, whose blocks are never skippable because
// the md superblock and write-intent bitmap must be copied verbatim
// (architecture.md, Migrations, and
// Group on-leg layout: meta region, data region, health block). regionSize is
// the dm-clone region (= the SP's block_size) and
// regionCnt bounds the device, so a bitmap longer than the device discards
// nothing beyond it.
func SkipRanges(
	bitmap []byte,
	shiftRegions uint64,
	regionCnt uint64,
	regionSize uint64,
) []SkipRange {
	if regionSize == 0 {
		return nil
	}
	var ranges []SkipRange
	limit := shiftRegions + BitmapBitCount(bitmap)
	if limit > regionCnt {
		limit = regionCnt
	}
	runStart := uint64(0)
	runLen := uint64(0)
	flush := func() {
		if runLen > 0 {
			ranges = append(ranges, SkipRange{
				Offset: runStart * regionSize,
				Length: runLen * regionSize,
			})
			runLen = 0
		}
	}
	for region := shiftRegions; region < limit; region++ {
		if !BitmapBit(bitmap, region-shiftRegions) {
			flush()
			continue
		}
		if runLen == 0 {
			runStart = region
		}
		runLen++
	}
	flush()
	return ranges
}

// ApplySkipRanges blkdiscards every skippable range of a dm-clone device,
// which is how a region is marked "already hydrated" without copying it.
// Re-applying is harmless: discarding an already-hydrated region is a no-op.
func ApplySkipRanges(
	ctx context.Context,
	dm *Dm,
	dev string,
	ranges []SkipRange,
) error {
	for _, r := range ranges {
		if err := dm.BlkDiscardRange(ctx, dev, r.Offset, r.Length); err != nil {
			return err
		}
	}
	return nil
}
