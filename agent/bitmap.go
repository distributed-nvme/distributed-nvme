package agent

import (
	"context"
	"sort"
)

// Bitmap conventions (architecture.md §9.6, §11.4):
//
//   - Every Gateway/etcd/Push*Bitmap bitmap is "1 = unwritten/skippable";
//     thin-pool metadata and the §11.4 formulas are "written/copied = 1". The
//     inversion happens exactly once, where thin-pool metadata is read — so
//     the chunks that reach an agent are already in wire convention and a set
//     bit means "this region need not be copied".
//   - Bits are addressed LSB-first inside each byte: bit i lives in
//     bitmap[i/8] at mask 1<<(i%8).
//   - Chunks are byte-aligned, so migration's bit-level concatenation
//     (chunk k starts at the summed bit length of chunks 0…k−1) is plain byte
//     concatenation.
const bitsPerByte = 8

// BitmapBitCount is the number of bits a chunk carries.
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

// ChunkSet holds the bitmap chunks an agent currently has on disk for one
// migration/clone. The applied set reported through BitmapInfo.bm_idx_list is
// always derived from it, so it survives restarts (SH21).
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

// Get is the self-positioned access a clone needs (bm_idx = source slice).
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
// push makes gaps unreachable in practice; the applied set still reports
// every file present.
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
// the md superblock and write-intent bitmap must be copied verbatim (§8.11,
// §3.6). regionSize is the dm-clone region (= the SP's block_size) and
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
