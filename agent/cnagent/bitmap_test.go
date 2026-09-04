package cnagent

import (
	"context"
	"encoding/hex"
	"testing"

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
	tds := []*pb.ThinDevice{
		{TdId: testTd, DevId: 1, Size: testTdSize},
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
			slices: []sliceBitmap{{present: true,
				bits: []byte{0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}}},
			want: "00000000ffffffff",
		},
		{
			name: "an absent chunk counts as written",
			geometry: raid0Geometry{
				sliceCnt: 2, stripeSize: 65536, blockSize: 1 << 20},
			regionCnt: 8,
			region:    1 << 20,
			slices: []sliceBitmap{
				{present: true, bits: []byte{0xff}},
				{present: false},
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
				{present: true, bits: []byte{0x0f}},
				{present: true, bits: []byte{0x0f}},
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
						slices[i] = sliceBitmap{
							present: next()%8 != 0, bits: bits}
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
