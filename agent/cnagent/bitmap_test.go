package cnagent

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The dm-thin metadata reader (CN25-CN27). The fixture is the case-B pool of
// cnagent_integtest.md, Cases (thinbm): a 64-block td whose virtual blocks
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

	// Bit k = 1 iff virtual block start+k is unmapped — the single wire
	// inversion of architecture.md, raid0 bitmap math.
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

// TestThinDumpGoesThroughAFile: the dump's XML never reaches stdout. The
// production client logs a command's stdout in full in its `os command`
// record (log.md), and a dump is one element per mapped run — tens of MB on a
// fragmented slice, once per slice per bitmap read. thin_dump writes it to a
// file instead (`-o`), whose read logs a truncated excerpt, and the file is
// removed after the read — and on both paths where the dump did not answer
// too, since a thin_dump that was killed may already have written it: one the
// soft timeout killed, and one killed because the caller's ctx was cancelled,
// whose `rm` must not ride that dead ctx. Nor may that caller's metadata
// snapshot release: a reservation left held pins the pool's metadata blocks
// until some later reserve on the pool meets it, which may never come.
func TestThinDumpGoesThroughAFile(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	scriptDump(srv, node, caseBDump)

	node.Reset()
	if got := tdBm(t, srv, 0, 0); got != "1effffffffffffff" {
		t.Fatalf("full td bitmap is %q, want 1effffffffffffff", got)
	}
	outs := node.stdouts["thin_dump"]
	if len(outs) != 1 {
		t.Fatalf("thin_dump ran %d times, want 1", len(outs))
	}
	if outs[0] != "" {
		t.Fatalf("thin_dump's os command record carries %d bytes of stdout, "+
			"want none", len(outs[0]))
	}
	dumps := node.callsMatching("cmd thin_dump --metadata-snap ")
	args := strings.Fields(dumps[0])
	if len(args) != 6 || args[4] != "-o" {
		t.Fatalf("thin_dump does not write to a file: %q", dumps[0])
	}
	file := args[5]
	if dir := "/run/dnv-thin-dump"; filepath.Dir(file) != dir {
		t.Fatalf("the dump file %s is not in the private directory %s",
			file, dir)
	}
	pool := poolName(srv)
	assertOrder(t, node,
		"cmd thin_dump --metadata-snap ",
		"read "+file,
		"cmd rm -f "+file,
		"cmd dmsetup message "+pool+" 0 release_metadata_snap",
	)
	if n := len(node.callsMatching("read " + file)); n != 1 {
		t.Fatalf("the dump file was read %d times, want 1", n)
	}
	if _, ok := node.files[file]; ok {
		t.Fatalf("the dump file %s was left behind", file)
	}

	node.Reset()
	node.killCmd["thin_dump"] = true
	if _, err := srv.GetThinDeviceBm(context.Background(),
		&pb.GetThinDeviceBmRequest{
			ClusterId: testCluster, CnId: testCn, SpId: testSp,
			CntlrId: testCntlr, TdId: testTd, SliceIdx: 0,
		}); err == nil {
		t.Fatalf("a dump that did not answer must fail the RPC")
	}
	assertNoCall(t, node, "read "+file)
	if _, ok := node.files[file]; ok {
		t.Fatalf("a killed dump left its file %s behind", file)
	}

	// The caller goes away while thin_dump runs (a client that gave up, a
	// gateway deadline): exec kills the dump, which has written the file.
	node.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node.cancelCmd["cmd thin_dump "] = cancel
	if _, err := srv.GetThinDeviceBm(ctx,
		&pb.GetThinDeviceBmRequest{
			ClusterId: testCluster, CnId: testCn, SpId: testSp,
			CntlrId: testCntlr, TdId: testTd, SliceIdx: 0,
		}); err == nil {
		t.Fatalf("a dump whose caller went away must fail the RPC")
	}
	assertNoCall(t, node, "read "+file)
	if n := len(node.callsMatching("cmd rm -f " + file)); n != 1 {
		t.Fatalf("the dump file was removed %d times after a cancelled "+
			"dump, want 1", n)
	}
	release := "cmd dmsetup message " + pool + " 0 release_metadata_snap"
	if n := len(node.callsMatching(release)); n != 1 {
		t.Fatalf("the metadata snapshot was released %d times after a "+
			"cancelled dump, want 1", n)
	}
	assertOrder(t, node, "cmd rm -f "+file, release)
	if _, ok := node.files[file]; ok {
		t.Fatalf("a dump whose caller went away left its file %s behind",
			file)
	}
}

// TestThinDumpDirIsPrivate: the agent writes the dump as root, through a
// thin_dump that opens its output with a plain create-and-truncate. Under a
// fixed name in a world-writable directory another local user could create
// that file first, own it through the dump, and rewrite the document before
// the agent reads it back — and a destination bitmap read from a forged
// document discards regions of a dm-clone that were never copied. So the file
// lives in a directory the agent owns with mode 0700, checked before every
// dump: one that is anything else — another user's directory, one others can
// write, a symlink — fails the read before the pool is touched. That
// directory sits in /run, which only root can write, not in the temp
// directory: in /tmp any local user could create the name first, and the
// check would then fail every dump on the node, a clone recovery's included,
// until an operator removed it.
func TestThinDumpDirIsPrivate(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	scriptDump(srv, node, caseBDump)
	dir := "/run/dnv-thin-dump"
	pool := poolName(srv)

	node.Reset()
	if got := tdBm(t, srv, 0, 0); got != "1effffffffffffff" {
		t.Fatalf("full td bitmap is %q, want 1effffffffffffff", got)
	}
	assertOrder(t, node,
		"cmd mkdir -p -m 0700 "+dir,
		"cmd stat --format %u %a %F "+dir,
		"cmd dmsetup message "+pool+" 0 reserve_metadata_snap",
		"cmd thin_dump --metadata-snap ",
	)

	euid := os.Geteuid()
	for _, tc := range []struct {
		name string
		stat string
	}{
		{"another user's directory", fmt.Sprintf("%d 700 directory", euid+1)},
		{"a directory others can write", fmt.Sprintf("%d 777 directory", euid)},
		{"a symlink", fmt.Sprintf("%d 777 symbolic link", euid+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node.Reset()
			node.dirStat[dir] = tc.stat
			defer delete(node.dirStat, dir)
			if _, err := srv.GetThinDeviceBm(context.Background(),
				&pb.GetThinDeviceBmRequest{
					ClusterId: testCluster, CnId: testCn, SpId: testSp,
					CntlrId: testCntlr, TdId: testTd, SliceIdx: 0,
				}); err == nil {
				t.Fatalf("a dump into %s (%s) must fail the RPC", dir, tc.stat)
			}
			assertNoCall(t, node, "cmd thin_dump ")
			assertNoCall(t, node, "reserve_metadata_snap")
		})
	}
}

func TestThinDeviceBitmapSharedSubtree(t *testing.T) {
	srv, node := newTestServer(t)
	// The gateway only ever publishes a snapshot whose origin is already
	// materialized (architecture.md, Thin devices), so the origin carries
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
// The raid0 fold of architecture.md, raid0 bitmap math (CN22)
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

// regionSkippableNaive is the address mapping of architecture.md, raid0 bitmap
// math, applied literally, one fine-grained offset at a time. It is the
// reference the cycle-walking
// implementation is differentially tested against: the two must agree for
// every geometry the CreateClone validation (architecture.md, Clones) admits.
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

// TestRegionSkippableMatchesTheAddressMapping sweeps the geometries
// architecture.md, raid0 bitmap math, allows
// (1 <= slice_cnt <= common.MaxSliceCntPerSp, stripe_size = i x 4 KiB,
// block_size = k x stripe_size) against pseudo-random per-slice bitmaps.
// CreateClone is what admits them (gateway/validate.go validateCloneGeometry),
// so the widest shape swept here is written as that constant and not as the
// number it happens to hold.
func TestRegionSkippableMatchesTheAddressMapping(t *testing.T) {
	rng := uint64(0x2545f4914f6cdd1d)
	next := func() uint64 {
		rng ^= rng << 13
		rng ^= rng >> 7
		rng ^= rng << 17
		return rng
	}
	// The ceiling is read from common, so the sweep follows it up; 16 stays
	// as a middle value once the ceiling is past it. Every entry is a real
	// geometry here, not a label: the naive reference below walks the whole
	// region 4 KiB at a time, so a wider slice_cnt genuinely re-derives the
	// chunk -> (slice, local offset) mapping for more slices.
	sliceCnts := []uint64{1, 2, 3, 4, 16, common.MaxSliceCntPerSp}
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
// the self-positioning of architecture.md, Bitmap push protocol, from a
// concatenation model. Chunks 0 and 2 are
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

// TestChunksOfCutsAtChunkBoundaries covers the three shapes the recovery of
// architecture.md, Clone crash recovery, hands chunksOf: shorter than C,
// exactly C, and one byte past it.
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

// TestDstBitmapLongerThanAChunkIsSplit pins architecture.md, Clone crash
// recovery: applyDst
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
