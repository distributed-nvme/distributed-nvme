package cnagent

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/agent"
)

// thinbm.go is the dm-thin metadata reader behind GetThinDeviceBm / GetLegBm
// (CN25-CN27) and the §11.4 geometry folds used by PushCloneBitmap (CN22) and
// by the §11.5 clone recovery. thin-provisioning-tools has a single role, so
// by the dnagent.md §1 split rule this wrapper is role code.

// ---------------------------------------------------------------------------
// thin_dump XML
// ---------------------------------------------------------------------------

// thinSuperblock is one thin_dump document. The dumper emits no XML
// declaration and no namespace; attributes are plain decimal.
//
// Two shapes must both parse: thin-provisioning-tools 0.9.x expands a shared
// btree subtree inline into every device, while 1.x factors it into a <def>
// that devices reach through <ref>. Dropping <def>/<ref> would silently lose
// mappings — and a lost mapping reads as "unmapped", which tells a caller it
// may skip copying live data — so both are decoded.
type thinSuperblock struct {
	XMLName       xml.Name     `xml:"superblock"`
	DataBlockSize uint64       `xml:"data_block_size,attr"`
	Defs          []thinDef    `xml:"def"`
	Devices       []thinDevice `xml:"device"`
}

type thinDef struct {
	Name    string              `xml:"name,attr"`
	Singles []thinSingleMapping `xml:"single_mapping"`
	Ranges  []thinRangeMapping  `xml:"range_mapping"`
}

type thinDevice struct {
	DevId        uint32              `xml:"dev_id,attr"`
	MappedBlocks uint64              `xml:"mapped_blocks,attr"`
	Singles      []thinSingleMapping `xml:"single_mapping"`
	Ranges       []thinRangeMapping  `xml:"range_mapping"`
	Refs         []thinRef           `xml:"ref"`
}

// single_mapping and range_mapping name their fields differently — a real
// trap: origin_block/data_block versus origin_begin/data_begin.
type thinSingleMapping struct {
	OriginBlock uint64 `xml:"origin_block,attr"`
	DataBlock   uint64 `xml:"data_block,attr"`
}

type thinRangeMapping struct {
	OriginBegin uint64 `xml:"origin_begin,attr"`
	DataBegin   uint64 `xml:"data_begin,attr"`
	Length      uint64 `xml:"length,attr"`
}

type thinRef struct {
	Name string `xml:"name,attr"`
}

// thinExtent is one run of mappings, normalized across the two element shapes.
type thinExtent struct {
	origin uint64
	data   uint64
	length uint64
}

func extentsOf(
	singles []thinSingleMapping,
	ranges []thinRangeMapping,
) []thinExtent {
	out := make([]thinExtent, 0, len(singles)+len(ranges))
	for _, m := range singles {
		out = append(out, thinExtent{m.OriginBlock, m.DataBlock, 1})
	}
	for _, m := range ranges {
		out = append(out, thinExtent{m.OriginBegin, m.DataBegin, m.Length})
	}
	return out
}

func (sb *thinSuperblock) defExtents() map[string][]thinExtent {
	if len(sb.Defs) == 0 {
		return nil
	}
	out := make(map[string][]thinExtent, len(sb.Defs))
	for _, def := range sb.Defs {
		out[def.Name] = extentsOf(def.Singles, def.Ranges)
	}
	return out
}

// extents returns every mapping of one device, resolving <ref> against the
// document's <def> blocks.
func (d *thinDevice) extents(defs map[string][]thinExtent) []thinExtent {
	out := extentsOf(d.Singles, d.Ranges)
	for _, ref := range d.Refs {
		out = append(out, defs[ref.Name]...)
	}
	return out
}

func parseThinDump(stdout string) (*thinSuperblock, error) {
	sb := &thinSuperblock{}
	if err := xml.Unmarshal([]byte(stdout), sb); err != nil {
		return nil, fmt.Errorf("thin_dump xml: %w", err)
	}
	return sb, nil
}

// ---------------------------------------------------------------------------
// The metadata snapshot (CN25)
// ---------------------------------------------------------------------------

// dumpThinMetadata reserves a dm-thin metadata snapshot, dumps it and
// **always** releases — on the success path and on every error path, because a
// leaked reservation blocks the next reserve and pins metadata blocks. A
// reserve that fails because one is already held is released and retried once.
func (s *CnAgentServer) dumpThinMetadata(
	ctx context.Context,
	sp *slicePlan,
) (*thinSuperblock, error) {
	if err := s.reserveMetadataSnap(ctx, sp); err != nil {
		return nil, err
	}
	defer s.releaseMetadataSnap(ctx, sp)

	metaPath := s.nf.DmPath(sp.poolMetaName)
	stdout, stderr, _, err := s.cmd.Run(ctx, "thin_dump",
		"--metadata-snap", metaPath)
	if err != nil {
		return nil, fmt.Errorf("thin_dump %s: %s", metaPath,
			firstNonEmpty(stderr, stdout, err.Error()))
	}
	sb, err := parseThinDump(stdout)
	if err != nil {
		return nil, err
	}
	// A dump the soft timeout truncated, or a shared subtree the parser
	// mishandled, both show up here — and a wrong bitmap is worse than no
	// bitmap (§8.9: bitmaps are an optimization, never a correctness input).
	defs := sb.defExtents()
	for i := range sb.Devices {
		var mapped uint64
		for _, extent := range sb.Devices[i].extents(defs) {
			mapped += extent.length
		}
		if mapped != sb.Devices[i].MappedBlocks {
			return nil, fmt.Errorf(
				"thin_dump device %d maps %d blocks, header says %d",
				sb.Devices[i].DevId, mapped, sb.Devices[i].MappedBlocks)
		}
	}
	return sb, nil
}

func (s *CnAgentServer) reserveMetadataSnap(
	ctx context.Context,
	sp *slicePlan,
) error {
	err := s.dm.Message(ctx, sp.poolFinalName, 0, "reserve_metadata_snap")
	if err == nil {
		return nil
	}
	// EBUSY: a reservation is already held — release it and try once more.
	if !strings.Contains(strings.ToLower(err.Error()), "busy") &&
		!strings.Contains(err.Error(), "already exists") {
		return err
	}
	s.releaseMetadataSnap(ctx, sp)
	return s.dm.Message(ctx, sp.poolFinalName, 0, "reserve_metadata_snap")
}

func (s *CnAgentServer) releaseMetadataSnap(
	ctx context.Context,
	sp *slicePlan,
) {
	if err := s.dm.Message(
		ctx, sp.poolFinalName, 0, "release_metadata_snap"); err != nil {
		// Releasing nothing is benign; anything else is worth a record.
		slog.WarnContext(ctx, "releasing the thin metadata snapshot failed",
			slog.String("pool", sp.poolFinalName),
			slog.String("error", err.Error()))
	}
}

// deviceMappings returns the extents of one thin device, or an error when the
// dump does not contain it. That assertion is load-bearing: thin_dump answers
// an unknown device with an empty document and exit 0, and reporting that as
// "nothing mapped" would tell the caller the whole volume is skippable.
func deviceMappings(
	sb *thinSuperblock,
	devId uint32,
) ([]thinExtent, error) {
	defs := sb.defExtents()
	for i := range sb.Devices {
		if sb.Devices[i].DevId == devId {
			return sb.Devices[i].extents(defs), nil
		}
	}
	return nil, fmt.Errorf("thin device %d not in the metadata snapshot", devId)
}

// ---------------------------------------------------------------------------
// Bit helpers — bit i lives at bitmap[i/8], mask 1<<(i%8) (LSB-first)
// ---------------------------------------------------------------------------

func bitmapBytes(bits uint64) uint64 {
	return (bits + 7) / 8
}

// allUnmapped is the wire-convention start point: every real bit 1
// ("unmapped/skippable"), every pad bit 0.
func allUnmapped(bits uint64) []byte {
	out := make([]byte, bitmapBytes(bits))
	for i := range out {
		out[i] = 0xff
	}
	if rest := bits % 8; rest != 0 && len(out) > 0 {
		out[len(out)-1] &= byte(1<<rest) - 1
	}
	return out
}

func clearBitRange(bitmap []byte, lo, hi uint64) {
	for ; lo < hi && lo%8 != 0; lo++ {
		bitmap[lo/8] &^= 1 << (lo % 8)
	}
	for ; hi > lo && hi%8 != 0; hi-- {
		bitmap[(hi-1)/8] &^= 1 << ((hi - 1) % 8)
	}
	for b := lo / 8; b < hi/8; b++ {
		bitmap[b] = 0
	}
}

func setBitRange(bitmap []byte, lo, hi uint64) {
	for ; lo < hi && lo%8 != 0; lo++ {
		bitmap[lo/8] |= 1 << (lo % 8)
	}
	for ; hi > lo && hi%8 != 0; hi-- {
		bitmap[(hi-1)/8] |= 1 << ((hi - 1) % 8)
	}
	for b := lo / 8; b < hi/8; b++ {
		bitmap[b] = 0xff
	}
}

func setBit(bitmap []byte, idx uint64) {
	if idx/8 < uint64(len(bitmap)) {
		bitmap[idx/8] |= 1 << (idx % 8)
	}
}

func clipRange(lo, hi, windowLo, windowHi uint64) (uint64, uint64) {
	if lo < windowLo {
		lo = windowLo
	}
	if hi > windowHi {
		hi = windowHi
	}
	return lo, hi
}

// ---------------------------------------------------------------------------
// CN26 — GetThinDeviceBm
// ---------------------------------------------------------------------------

// thinDeviceBitmap is the mapping bitmap of one thin volume over its virtual
// blocks: bit k = 1 iff block start+k is **unmapped**. Thin metadata answers
// "mapped = written", and this boundary is where the §11.4 wire convention
// inverts — exactly once.
func thinDeviceBitmap(
	extents []thinExtent,
	start uint64,
	count uint64,
) []byte {
	bitmap := allUnmapped(count)
	for _, extent := range extents {
		lo, hi := clipRange(extent.origin, extent.origin+extent.length,
			start, start+count)
		if lo < hi {
			clearBitRange(bitmap, lo-start, hi-start)
		}
	}
	return bitmap
}

// ---------------------------------------------------------------------------
// CN27 — GetLegBm
// ---------------------------------------------------------------------------

// legDataBitmap projects every thin device's pool-data mappings onto one data
// group's span: bit k = 1 iff no thin device of the pool maps pool-data block
// span_start+k. Every leg of the group mirrors those bytes, so the leg
// identity beyond its group never enters the math.
func legDataBitmap(
	sb *thinSuperblock,
	spanStart uint64,
	groupBlocks uint64,
	start uint64,
	count uint64,
) []byte {
	bitmap := allUnmapped(count)
	defs := sb.defExtents()
	eat := func(extent thinExtent) {
		dataLo := extent.data
		dataHi := extent.data + extent.length
		if dataHi <= spanStart || dataLo >= spanStart+groupBlocks {
			return
		}
		var grpLo uint64
		if dataLo > spanStart {
			grpLo = dataLo - spanStart
		}
		grpHi := dataHi - spanStart
		if grpHi > groupBlocks {
			grpHi = groupBlocks
		}
		lo, hi := clipRange(grpLo, grpHi, start, start+count)
		if lo < hi {
			clearBitRange(bitmap, lo-start, hi-start)
		}
	}
	for i := range sb.Devices {
		for _, extent := range sb.Devices[i].extents(defs) {
			eat(extent)
		}
	}
	return bitmap
}

// ---------------------------------------------------------------------------
// The §11.4 raid0 fold (CN22 and the §11.5 recovery)
// ---------------------------------------------------------------------------

// sliceBitmap is one underlying device's bitmap in the fold. present is false
// for a source chunk the agent has not received: an absent bit counts as
// written, so a region overlapping it is never discarded.
type sliceBitmap struct {
	present bool
	bits    []byte
}

func (b sliceBitmap) skippable(idx uint64) bool {
	if !b.present {
		return false
	}
	// A bit past the end of a present-but-short chunk is unknown, and unknown
	// resolves to written ([D8]: AppendCloneBitmap may still be growing it).
	if idx/8 >= uint64(len(b.bits)) {
		return false
	}
	return b.bits[idx/8]&(1<<(idx%8)) != 0
}

// raid0Geometry is one side of the §11.4 address mapping.
type raid0Geometry struct {
	sliceCnt   uint64
	stripeSize uint64
	blockSize  uint64
}

func (g raid0Geometry) valid() bool {
	return g.sliceCnt > 0 && g.stripeSize > 0 && g.blockSize > 0 &&
		g.blockSize%g.stripeSize == 0
}

// regionSkippable decides one dm-clone region. Region r covers logical bytes
// [r*regionSize, (r+1)*regionSize); §11.4 maps a byte offset to stripe chunk
// c = off/stripe_size on slice c mod slice_cnt at slice-local offset
// (c div slice_cnt)*stripe_size, and the source bit is that offset divided by
// the source block_size. Because block_size is a whole multiple of
// stripe_size, a chunk never straddles two bits and the mapping collapses to
//
//	slice = c mod slice_cnt        bit = c div (slice_cnt × block_size/stripe_size)
//
// — the bit index is independent of the slice, so one "cycle" of `cycle`
// consecutive chunks covers every slice at exactly one bit. Walking cycles
// instead of chunks makes the cost O(region/block_size × slice_cnt) rather
// than O(region/stripe_size).
//
// The region is skippable iff every covered (slice, bit) pair is present and
// set; anything unknown — an absent chunk, a bit past the end of a short one,
// an unusable geometry — counts as written, which can only cost an extra copy.
func regionSkippable(
	r uint64,
	regionSize uint64,
	g raid0Geometry,
	slices []sliceBitmap,
) bool {
	if !g.valid() || regionSize == 0 || uint64(len(slices)) < g.sliceCnt {
		return false
	}
	cycle := g.sliceCnt * (g.blockSize / g.stripeSize)
	chunkLo := r * regionSize / g.stripeSize
	chunkHi := ((r+1)*regionSize - 1) / g.stripeSize
	for bit := chunkLo / cycle; bit <= chunkHi/cycle; bit++ {
		lo, hi := bit*cycle, (bit+1)*cycle-1
		if lo < chunkLo {
			lo = chunkLo
		}
		if hi > chunkHi {
			hi = chunkHi
		}
		if hi-lo+1 >= g.sliceCnt {
			// The whole cycle is covered: every slice is touched at this bit.
			for slice := uint64(0); slice < g.sliceCnt; slice++ {
				if !slices[slice].skippable(bit) {
					return false
				}
			}
			continue
		}
		for chunk := lo; chunk <= hi; chunk++ {
			if !slices[chunk%g.sliceCnt].skippable(bit) {
				return false
			}
		}
	}
	return true
}

// foldRegions turns per-slice bitmaps into one device-order bitmap over the
// dm-clone's regions, bit r = 1 iff region r need not be copied. It is what
// agent.SkipRanges then coalesces into blkdiscard ranges (with
// shiftRegions = 0 — clones have no meta region, that shift is the dn's).
func foldRegions(
	regionCnt uint64,
	regionSize uint64,
	g raid0Geometry,
	slices []sliceBitmap,
) []byte {
	folded := make([]byte, bitmapBytes(regionCnt))
	for r := uint64(0); r < regionCnt; r++ {
		if regionSkippable(r, regionSize, g, slices) {
			setBit(folded, r)
		}
	}
	return folded
}

// ---------------------------------------------------------------------------
// Applying bitmaps to a dm-clone
// ---------------------------------------------------------------------------

// applyCloneChunks folds the source chunks this agent holds and blkdiscards
// the fully-skippable regions of the dm-clone (CN22). Re-applying is harmless:
// discarding an already-hydrated region is a no-op.
func (s *CnAgentServer) applyCloneChunks(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	cp *clonePlan,
) {
	set := st.chunks[cp.cloneId]
	if set == nil || set.Len() == 0 || cp.regionCnt == 0 {
		return
	}
	sliceCnt := uint64(cp.clone.GetSrcSliceCnt())
	if sliceCnt == 0 {
		return
	}
	slices := make([]sliceBitmap, sliceCnt)
	for i := uint64(0); i < sliceCnt; i++ {
		if bits, ok := set.Get(uint32(i)); ok {
			slices[i] = sliceBitmap{present: true, bits: bits}
		}
	}
	geometry := raid0Geometry{
		sliceCnt:   sliceCnt,
		stripeSize: cp.clone.GetSrcStripeSize(),
		blockSize:  cp.clone.GetSrcBlockSize(),
	}
	if !geometry.valid() {
		slog.ErrorContext(ctx, "clone source geometry is unusable",
			slog.Uint64("clone_id", cp.cloneId))
		return
	}
	folded := foldRegions(
		cp.regionCnt, plan.blockSize, geometry, slices)
	ranges := agent.SkipRanges(folded, 0, cp.regionCnt, plan.blockSize)
	if err := agent.ApplySkipRanges(
		ctx, s.dm, s.nf.DmPath(cp.finalName), ranges); err != nil {
		slog.ErrorContext(ctx, "applying clone bitmap failed",
			slog.String("dm", cp.finalName),
			slog.String("error", err.Error()))
	}
}

// applyDstBitmaps is the §11.5 recovery, B-side of §11.4: read the
// destination td's mapping bitmap from every slice pool and blkdiscard every
// region the destination already owns. "Mapped ⇔ already copied" holds because
// the dst td started empty ([D3]).
//
// It fails closed: every error is returned rather than logged, because a
// partially applied destination bitmap is exactly the §11.5 staleness hazard
// — the caller must not let the dm-clone serve or hydrate after one.
func (s *CnAgentServer) applyDstBitmaps(
	ctx context.Context,
	plan *cntlrPlan,
	cp *clonePlan,
) error {
	geometry := raid0Geometry{
		sliceCnt:   plan.sliceCnt,
		stripeSize: plan.stripeSize,
		blockSize:  plan.blockSize,
	}
	if cp.regionCnt == 0 || !geometry.valid() {
		return fmt.Errorf("destination geometry is unusable")
	}
	slices := make([]sliceBitmap, plan.sliceCnt)
	virtualBlocks := cp.dstTd.td.GetSize() / plan.sliceCnt / plan.blockSize
	for _, sp := range plan.slices {
		sb, err := s.dumpThinMetadata(ctx, sp)
		if err != nil {
			return fmt.Errorf("%s: %w", sp.poolFinalName, err)
		}
		extents, err := deviceMappings(sb, cp.dstTd.td.GetDevId())
		if err != nil {
			return fmt.Errorf("%s: %w", sp.poolFinalName, err)
		}
		mapped := make([]byte, bitmapBytes(virtualBlocks))
		for _, extent := range extents {
			lo, hi := clipRange(extent.origin, extent.origin+extent.length,
				0, virtualBlocks)
			if lo < hi {
				setBitRange(mapped, lo, hi)
			}
		}
		if uint64(sp.sliceIdx) >= plan.sliceCnt {
			return fmt.Errorf("slice_idx %d is outside the slice set",
				sp.sliceIdx)
		}
		slices[sp.sliceIdx] = sliceBitmap{present: true, bits: mapped}
	}
	folded := foldRegions(cp.regionCnt, plan.blockSize, geometry, slices)
	ranges := agent.SkipRanges(folded, 0, cp.regionCnt, plan.blockSize)
	return agent.ApplySkipRanges(
		ctx, s.dm, s.nf.DmPath(cp.finalName), ranges)
}
