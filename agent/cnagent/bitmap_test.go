package cnagent

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// §6.14 — the dm-thin metadata reader (CN25-CN27). The fixture is the case-B
// pool of cnagent_integtest.md §12: a 64-block td whose virtual blocks
// {0,5,6,7} are written, landing on pool-data blocks 0..3.
const caseBDump = `<superblock uuid="" time="0" transaction="0" flags="0" ` +
	`version="2" data_block_size="2048" nr_data_blocks="0">
  <device dev_id="1" mapped_blocks="4" transaction="0" creation_time="0" snap_time="0">
    <single_mapping origin_block="0" data_block="0" time="0"/>
    <range_mapping origin_begin="5" data_begin="1" length="3" time="0"/>
  </device>
</superblock>
`

// caseBSharedDump is the same state as thin-provisioning-tools 1.x emits it
// once a snapshot shares the origin's btree leaf: the mappings move into a
// <def> the devices reach through <ref>. Losing them would report mapped
// blocks as unmapped — telling a caller it may skip copying live data.
const caseBSharedDump = `<superblock uuid="" time="1" transaction="1" ` +
	`version="2" data_block_size="2048" nr_data_blocks="0">
  <def name="7">
    <single_mapping origin_block="0" data_block="0" time="0"/>
    <range_mapping origin_begin="5" data_begin="1" length="3" time="0"/>
  </def>
  <device dev_id="1" mapped_blocks="4" transaction="0" creation_time="0" snap_time="1">
    <ref name="7"/>
  </device>
  <device dev_id="2" mapped_blocks="4" transaction="1" creation_time="1" snap_time="1">
    <ref name="7"/>
  </device>
</superblock>
`

func scriptDump(srv *CnAgentServer, node *fakeNode, dump string) {
	node.thinDumps[srv.nf.DmPath(srv.nf.CnPoolMetaName(
		testCluster, testCn, testSp, testSlice))] = dump
}

func tdBm(
	t *testing.T,
	srv *CnAgentServer,
	start, count uint64,
) string {
	t.Helper()
	reply, err := srv.GetThinDeviceBm(context.Background(),
		&pb.GetThinDeviceBmRequest{
			ClusterId: testCluster, CnId: testCn, SpId: testSp,
			CntlrId: testCntlr, TdId: testTd, SliceIdx: 0,
			StartBlock: start, BlockCnt: count,
		})
	if err != nil {
		t.Fatalf("GetThinDeviceBm: %v", err)
	}
	return hex.EncodeToString(reply.GetBitmap())
}

func legBm(
	t *testing.T,
	srv *CnAgentServer,
	legId, start, count uint64,
) string {
	t.Helper()
	reply, err := srv.GetLegBm(context.Background(), &pb.GetLegBmRequest{
		ClusterId: testCluster, CnId: testCn, SpId: testSp,
		CntlrId: testCntlr, LegId: legId,
		StartBlock: start, BlockCnt: count,
	})
	if err != nil {
		t.Fatalf("GetLegBm: %v", err)
	}
	return hex.EncodeToString(reply.GetBitmap())
}

func TestThinDeviceBitmap(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	scriptDump(srv, node, caseBDump)

	// Bit k = 1 iff virtual block start+k is unmapped — the single §11.4
	// wire inversion.
	if got := tdBm(t, srv, 0, 0); got != "1effffffffffffff" {
		t.Fatalf("full td bitmap is %q, want 1effffffffffffff", got)
	}
	// A one-byte window proves the paging math end to end: bit 0 = block 4
	// unmapped, bits 1-3 = blocks 5,6,7 written, bits 4-7 = blocks 8..11.
	if got := tdBm(t, srv, 4, 8); got != "f1" {
		t.Fatalf("window [4,12) is %q, want f1", got)
	}

	// The reserve/dump/release ordering, and the release on every path.
	pool := poolName(srv)
	assertOrder(t, node,
		"cmd dmsetup message "+pool+" 0 reserve_metadata_snap",
		"cmd thin_dump --metadata-snap ",
		"cmd dmsetup message "+pool+" 0 release_metadata_snap",
	)
	if node.dms[pool].heldRoot {
		t.Fatalf("the metadata snapshot leaked")
	}
}

func TestThinDeviceBitmapSharedSubtree(t *testing.T) {
	srv, node := newTestServer(t)
	// The gateway only ever publishes a snapshot whose origin is already
	// materialized (ThinDeviceCreated.md U2-S1), so the origin carries
	// `created` and its dev_id is in the pool before this cntlr converges.
	node.holdThinIds(poolName(srv), 1)
	tds := []*pb.ThinDevice{
		{TdId: testTd, DevId: 1, Size: testTdSize, Created: true},
		{TdId: testSnapTd, DevId: 2, OriId: 1, Size: testTdSize},
	}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: tds})
	scriptDump(srv, node, caseBSharedDump)

	if got := tdBm(t, srv, 0, 0); got != "1effffffffffffff" {
		t.Fatalf("a <def>/<ref> dump lost its mappings: %q", got)
	}
}

func TestLegBitmap(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	scriptDump(srv, node, caseBDump)

	// The data group's 127 blocks, with pool-data blocks 0..3 mapped. The
	// trailing 0x7f is the proof that the pad bit is zeroed.
	want := "f0ffffffffffffffffffffffffffff7f"
	if got := legBm(t, srv, testDataLeg, 0, 0); got != want {
		t.Fatalf("data-leg bitmap is %q, want %q", got, want)
	}
	// A meta-group leg is never skippable: 63 blocks, all zero, and no
	// metadata snapshot is taken at all.
	node.Reset()
	if got := legBm(t, srv, testMetaLeg, 0, 0); got != "0000000000000000" {
		t.Fatalf("meta-leg bitmap is %q, want all-zero", got)
	}
	assertNoCall(t, node, "reserve_metadata_snap")
}

func TestBitmapReadRejectsNonPrimary(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: false})
	scriptDump(srv, node, caseBDump)
	if _, err := srv.GetThinDeviceBm(context.Background(),
		&pb.GetThinDeviceBmRequest{
			ClusterId: testCluster, CnId: testCn, SpId: testSp,
			CntlrId: testCntlr, TdId: testTd, SliceIdx: 0,
		}); err == nil {
		t.Fatalf("a standby must not serve GetThinDeviceBm")
	}
	if _, err := srv.GetLegBm(context.Background(), &pb.GetLegBmRequest{
		ClusterId: testCluster, CnId: testCn, SpId: testSp,
		CntlrId: testCntlr, LegId: testDataLeg,
	}); err == nil {
		t.Fatalf("a standby must not serve GetLegBm")
	}
}

// TestBitmapReadMissingDevice pins the highest-severity failure mode of the
// whole feature: thin_dump answers an unknown dev_id with an empty document
// and exit 0, and reporting that as "nothing mapped" would hand the caller an
// all-ones bitmap — "skip the whole volume".
func TestBitmapReadMissingDevice(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	scriptDump(srv, node, `<superblock uuid="" time="0" transaction="0" `+
		`version="2" data_block_size="2048" nr_data_blocks="0">
</superblock>
`)
	if _, err := srv.GetThinDeviceBm(context.Background(),
		&pb.GetThinDeviceBmRequest{
			ClusterId: testCluster, CnId: testCn, SpId: testSp,
			CntlrId: testCntlr, TdId: testTd, SliceIdx: 0,
		}); err == nil {
		t.Fatalf("a dump without the device must fail the RPC")
	}
}

// TestReserveRetriesOnceWhenHeld: a reservation left behind by a killed reader
// is released and re-taken (CN25).
func TestReserveRetriesOnceWhenHeld(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	scriptDump(srv, node, caseBDump)
	node.dms[poolName(srv)].heldRoot = true

	node.Reset()
	if got := tdBm(t, srv, 0, 0); got != "1effffffffffffff" {
		t.Fatalf("bitmap after the retry is %q", got)
	}
	pool := poolName(srv)
	assertOrder(t, node,
		"cmd dmsetup message "+pool+" 0 reserve_metadata_snap",
		"cmd dmsetup message "+pool+" 0 release_metadata_snap",
		"cmd dmsetup message "+pool+" 0 reserve_metadata_snap",
		"cmd thin_dump ",
		"cmd dmsetup message "+pool+" 0 release_metadata_snap",
	)
}

// ---------------------------------------------------------------------------
// The §11.4 raid0 fold (CN22)
// ---------------------------------------------------------------------------

// chunked is one slice's bitmap held as the single chunk 0 — the shape every
// clone whose bitmap fits in one CloneBmChunkBytes arrives in.
func chunked(bits ...byte) sliceBitmap {
	return sliceBitmap{chunks: map[uint32][]byte{0: bits}}
}

func TestFoldRegions(t *testing.T) {
	tests := []struct {
		name      string
		geometry  raid0Geometry
		regionCnt uint64
		region    uint64
		slices    []sliceBitmap
		want      string
	}{
		{
			name: "single slice, whole tail skippable",
			geometry: raid0Geometry{
				sliceCnt: 1, stripeSize: 65536, blockSize: 1 << 20},
			regionCnt: 64,
			region:    1 << 20,
			slices: []sliceBitmap{
				chunked(0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff)},
			want: "00000000ffffffff",
		},
		{
			name: "an absent chunk counts as written",
			geometry: raid0Geometry{
				sliceCnt: 2, stripeSize: 65536, blockSize: 1 << 20},
			regionCnt: 8,
			region:    1 << 20,
			slices: []sliceBitmap{
				chunked(0xff),
				{},
			},
			want: "00",
		},
		{
			name: "two slices, both set",
			geometry: raid0Geometry{
				sliceCnt: 2, stripeSize: 65536, blockSize: 1 << 20},
			regionCnt: 8,
			region:    1 << 20,
			slices: []sliceBitmap{
				chunked(0x0f),
				chunked(0x0f),
			},
			// Region r covers source bits r/2 of both slices (block_size is
			// 16 stripes, slice_cnt 2 ⇒ 32 chunks per source bit), so the
			// first 8 regions all fall inside bits 0-3.
			want: "ff",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hex.EncodeToString(foldRegions(
				tc.regionCnt, tc.region, tc.geometry, tc.slices))
			if got != tc.want {
				t.Fatalf("fold is %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAllUnmappedPadsWithZeros(t *testing.T) {
	if got := hex.EncodeToString(allUnmapped(127)); got !=
		"ffffffffffffffffffffffffffffff7f" {
		t.Fatalf("allUnmapped(127) is %q", got)
	}
	if got := hex.EncodeToString(allUnmapped(64)); got !=
		"ffffffffffffffff" {
		t.Fatalf("allUnmapped(64) is %q", got)
	}
}

// regionSkippableNaive is the §11.4 address mapping applied literally, one
// fine-grained offset at a time. It is the reference the cycle-walking
// implementation is differentially tested against: the two must agree for
// every geometry the §8.9 validation admits.
func regionSkippableNaive(
	r uint64,
	regionSize uint64,
	g raid0Geometry,
	slices []sliceBitmap,
) bool {
	const step = 4096
	for off := r * regionSize; off < (r+1)*regionSize; off += step {
		chunk := off / g.stripeSize
		slice := chunk % g.sliceCnt
		local := (chunk/g.sliceCnt)*g.stripeSize + off%g.stripeSize
		if !slices[slice].skippable(local / g.blockSize) {
			return false
		}
	}
	return true
}

// TestRegionSkippableMatchesTheAddressMapping sweeps the geometries §11.4
// allows (1 <= slice_cnt <= 16, stripe_size = i x 4 KiB, block_size = k x
// stripe_size) against pseudo-random per-slice bitmaps.
func TestRegionSkippableMatchesTheAddressMapping(t *testing.T) {
	rng := uint64(0x2545f4914f6cdd1d)
	next := func() uint64 {
		rng ^= rng << 13
		rng ^= rng >> 7
		rng ^= rng << 17
		return rng
	}
	sliceCnts := []uint64{1, 2, 3, 4, 16}
	stripeSizes := []uint64{4 << 10, 16 << 10, 64 << 10}
	blockMultiples := []uint64{1, 2, 16}
	regionMultiples := []uint64{1, 2, 4}

	for _, sliceCnt := range sliceCnts {
		for _, stripeSize := range stripeSizes {
			for _, blockMul := range blockMultiples {
				blockSize := stripeSize * blockMul
				g := raid0Geometry{
					sliceCnt:   sliceCnt,
					stripeSize: stripeSize,
					blockSize:  blockSize,
				}
				for _, regionMul := range regionMultiples {
					regionSize := blockSize * regionMul
					slices := make([]sliceBitmap, sliceCnt)
					for i := range slices {
						bits := make([]byte, 8)
						for j := range bits {
							bits[j] = byte(next())
						}
						// Every bit these geometries reach lives in chunk 0;
						// one slice in eight has no chunk at all.
						if next()%8 == 0 {
							slices[i] = sliceBitmap{}
							continue
						}
						slices[i] = chunked(bits...)
					}
					for r := uint64(0); r < 24; r++ {
						want := regionSkippableNaive(
							r, regionSize, g, slices)
						got := regionSkippable(r, regionSize, g, slices)
						if got != want {
							t.Fatalf("slice_cnt=%d stripe=%d block=%d "+
								"region=%d r=%d: got %v, want %v",
								sliceCnt, stripeSize, blockSize,
								regionSize, r, got, want)
						}
					}
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The chunk view of a source slice (CN22)
// ---------------------------------------------------------------------------

const testChunkBits = uint64(common.CloneBmChunkBytes) * 8

func assertSkippable(
	t *testing.T,
	b sliceBitmap,
	idx uint64,
	want bool,
) {
	t.Helper()
	if got := b.skippable(idx); got != want {
		t.Fatalf("bit %d is skippable=%v, want %v", idx, got, want)
	}
}

// TestSliceBitmapChunkBoundary pins the one arithmetic the fold cannot get
// wrong: bit 8C−1 is the last bit of chunk 0 and bit 8C is the first bit of
// chunk 1, so a slice whose chunk 0 is full still reads nothing of chunk 1.
func TestSliceBitmapChunkBoundary(t *testing.T) {
	full := make([]byte, common.CloneBmChunkBytes)
	full[len(full)-1] = 0x80 // bit 8C−1, the chunk's very last bit
	b := sliceBitmap{chunks: map[uint32][]byte{0: full}}

	assertSkippable(t, b, testChunkBits-1, true)
	assertSkippable(t, b, testChunkBits-2, false)
	// Chunk 1 is absent, so the first bit past the boundary reads as written
	// even though the chunk below it is complete.
	assertSkippable(t, b, testChunkBits, false)

	b.chunks[1] = []byte{0x01}
	assertSkippable(t, b, testChunkBits, true)
	assertSkippable(t, b, testChunkBits+1, false)
	// Adding chunk 1 moved nothing in chunk 0 — positions are fixed by the
	// chunk index alone.
	assertSkippable(t, b, testChunkBits-1, true)
}

// TestShortChunkTailReadsAsWritten: a chunk that AppendCloneBitmap has not
// finished growing is present but short, and every bit past its length is
// unknown — which resolves to written ([D8]), never to skippable.
func TestShortChunkTailReadsAsWritten(t *testing.T) {
	b := sliceBitmap{chunks: map[uint32][]byte{0: {0xff}}}
	assertSkippable(t, b, 0, true)
	assertSkippable(t, b, 7, true)
	// Byte 1 of a one-byte chunk: within the chunk's span, past its length.
	assertSkippable(t, b, 8, false)
	assertSkippable(t, b, testChunkBits-1, false)
}

// TestAbsentMiddleChunkKeepsLaterChunksInPlace is the case that distinguishes
// §9.6's self-positioning from a concatenation model. Chunks 0 and 2 are
// present and chunk 1 is missing: chunk 2's bits must stay at their own
// offset, 16C bits in. A concatenation would have slid them down behind chunk
// 0 — reporting bit 8 as skippable and bit 16C as absent, both wrong, and the
// first of the two is the unsafe direction (a written region discarded).
func TestAbsentMiddleChunkKeepsLaterChunksInPlace(t *testing.T) {
	b := sliceBitmap{chunks: map[uint32][]byte{
		0: {0xff},
		2: {0xff},
	}}
	assertSkippable(t, b, 0, true)
	// Not chunk 0's second byte and not chunk 2 slid down behind it.
	assertSkippable(t, b, 8, false)
	// The whole of the absent chunk 1 reads as written.
	assertSkippable(t, b, testChunkBits, false)
	assertSkippable(t, b, 2*testChunkBits-1, false)
	// Chunk 2 is exactly where its bm_idx puts it.
	assertSkippable(t, b, 2*testChunkBits, true)
	assertSkippable(t, b, 2*testChunkBits+7, true)
	assertSkippable(t, b, 2*testChunkBits+8, false)
}

// TestChunksOfCutsAtChunkBoundaries covers the three shapes the §11.5
// recovery hands chunksOf: shorter than C, exactly C, and one byte past it.
func TestChunksOfCutsAtChunkBoundaries(t *testing.T) {
	const size = common.CloneBmChunkBytes
	for _, tc := range []struct {
		name    string
		length  int
		wantLen []int
	}{
		{"shorter than a chunk", 3, []int{3}},
		{"exactly one chunk", size, []int{size}},
		{"one byte past a chunk", size + 1, []int{size, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks := chunksOf(make([]byte, tc.length))
			if len(chunks) != len(tc.wantLen) {
				t.Fatalf("%d chunks, want %d", len(chunks), len(tc.wantLen))
			}
			for idx, want := range tc.wantLen {
				got, ok := chunks[uint32(idx)]
				if !ok || len(got) != want {
					t.Fatalf("chunk %d is %d bytes/%v, want %d bytes",
						idx, len(got), ok, want)
				}
			}
		})
	}
	if got := chunksOf(nil); len(got) != 0 {
		t.Fatalf("an empty bitmap yielded %d chunks", len(got))
	}
}

// TestDstBitmapLongerThanAChunkIsSplit is the §11.5 regression pin: applyDst
// Bitmaps builds its per-slice bitmaps locally, so it must cut them at C
// before handing them to the fold. Wrapping the whole bitmap as chunk 0
// instead leaves every bit past the first MiB addressed to a chunk that does
// not exist — and unknown reads as written, silently stopping the skip at 1
// MiB of bitmap on a large slice.
func TestDstBitmapLongerThanAChunkIsSplit(t *testing.T) {
	mapped := make([]byte, common.CloneBmChunkBytes+1)
	for i := range mapped {
		mapped[i] = 0xff
	}
	b := sliceBitmap{chunks: chunksOf(mapped)}
	assertSkippable(t, b, 0, true)
	assertSkippable(t, b, testChunkBits-1, true)
	// The byte past C: only a split bitmap puts it in chunk 1, where the fold
	// can reach it.
	assertSkippable(t, b, testChunkBits, true)
	assertSkippable(t, b, testChunkBits+7, true)
	assertSkippable(t, b, testChunkBits+8, false)

	// And through regionSkippable, the entry point foldRegions uses: with
	// stripe = block = region = 1 MiB the region index IS the source bit
	// index, so region 8C is the first region past the first chunk.
	g := raid0Geometry{sliceCnt: 1, stripeSize: 1 << 20, blockSize: 1 << 20}
	slices := []sliceBitmap{b}
	if !regionSkippable(testChunkBits, 1<<20, g, slices) {
		t.Fatalf("region %d is not skippable: the bitmap past the first "+
			"chunk was not reachable", testChunkBits)
	}
	if regionSkippable(testChunkBits+8, 1<<20, g, slices) {
		t.Fatalf("region %d is skippable past the end of the bitmap",
			testChunkBits+8)
	}
}
