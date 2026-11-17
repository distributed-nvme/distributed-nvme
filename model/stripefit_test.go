package model

import (
	"math/bits"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestFitStripeAtTheDefaults pins the stripe a pool that names none takes
// from a cluster that stored the default stripe (architecture.md, Common
// validation): the largest power of two at most the cluster's stripe whose
// product with the slice count is at most common.MaxNvmeTcpCmdBytes. The
// wanted values are written out, not computed, so that a formula that drifts
// fails here by number: 1 MiB up to four slices, 512 KiB up to eight, 256 KiB
// up to sixteen and 128 KiB up to the slice ceiling. A slice count of 0 is
// read as 1.
func TestFitStripeAtTheDefaults(t *testing.T) {
	const kib = uint64(1024)
	cluster := DefaultStripeSize(common.DefaultDmPoolDataBlockSize)
	if cluster != 1024*kib {
		t.Fatalf("the default cluster stripe is %d; the wanted values "+
			"below are written for 1 MiB", cluster)
	}
	for _, tc := range []struct {
		sliceCnt uint32
		want     uint64
	}{
		{0, 1024 * kib},
		{1, 1024 * kib},
		{2, 1024 * kib},
		{4, 1024 * kib},
		{5, 512 * kib},
		{8, 512 * kib},
		{9, 256 * kib},
		{16, 256 * kib},
		{17, 128 * kib},
		{common.MaxSliceCntPerSp, 128 * kib},
	} {
		if got := FitStripe(cluster, tc.sliceCnt); got != tc.want {
			t.Errorf("FitStripe(%d, %d) = %d, want %d",
				cluster, tc.sliceCnt, got, tc.want)
		}
	}
}

// TestFitStripeOverEveryGeometry holds FitStripe to its rule over every
// legal pool block, every stripe a cluster can store under that block and
// every slice count the gateway takes, 0 included: the result is a power of
// two, at most the cluster's stripe, at least common.MinDmRaid0StripeSize,
// divides the block, keeps its product with the slice count within
// common.MaxNvmeTcpCmdBytes, and is the largest that does — the cluster's
// stripe itself when that fits, so a stripe that already fits is never
// narrowed.
func TestFitStripeOverEveryGeometry(t *testing.T) {
	// The floor never fires inside the gateway's bounds: the bound spread
	// over the most slices is still a legal stripe.
	if common.MaxNvmeTcpCmdBytes/common.MaxSliceCntPerSp <
		common.MinDmRaid0StripeSize {
		t.Fatalf("MaxNvmeTcpCmdBytes / MaxSliceCntPerSp = %d is below "+
			"MinDmRaid0StripeSize %d: a pool at the slice ceiling has no "+
			"stripe that keeps the bound",
			common.MaxNvmeTcpCmdBytes/common.MaxSliceCntPerSp,
			common.MinDmRaid0StripeSize)
	}
	const bound = uint64(common.MaxNvmeTcpCmdBytes)
	checked := 0
	for block := uint64(common.MinDmPoolDataBlockSize); block <= common.MaxDmPoolDataBlockSize; block *= 2 {
		top := min(block, uint64(common.MaxDmRaid0StripeSize))
		for stripe := uint64(common.MinDmRaid0StripeSize); stripe <= top; stripe *= 2 {
			for sliceCnt := uint32(0); sliceCnt <= common.MaxSliceCntPerSp; sliceCnt++ {
				cnt := uint64(max(sliceCnt, 1))
				got := FitStripe(stripe, sliceCnt)
				checked++
				switch {
				case bits.OnesCount64(got) != 1:
					t.Errorf("block %d, stripe %d, %d slices: %d is not "+
						"a power of two", block, stripe, sliceCnt, got)
				case got > stripe:
					t.Errorf("block %d, stripe %d, %d slices: %d is "+
						"above the cluster's stripe", block, stripe,
						sliceCnt, got)
				case got < common.MinDmRaid0StripeSize:
					t.Errorf("block %d, stripe %d, %d slices: %d is "+
						"below MinDmRaid0StripeSize", block, stripe,
						sliceCnt, got)
				case block%got != 0:
					t.Errorf("block %d, stripe %d, %d slices: %d does "+
						"not divide the block", block, stripe, sliceCnt,
						got)
				case got*cnt > bound:
					t.Errorf("block %d, stripe %d, %d slices: %d x %d = "+
						"%d is over the bound %d", block, stripe,
						sliceCnt, got, cnt, got*cnt, bound)
				case got != stripe && 2*got*cnt <= bound:
					t.Errorf("block %d, stripe %d, %d slices: %d is not "+
						"the largest: %d fits too", block, stripe,
						sliceCnt, got, 2*got)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no geometry was checked")
	}
}

// TestResolveSpBdevConf pins the conf a pool stores from its merged request
// (architecture.md, Storage pools): ResolveBdevConf's, with a stripe the
// request did not name narrowed to the slice count and one it named kept as
// named. The gateway, not the resolve, refuses a named stripe whose product
// is over the bound, so the kept 1 MiB at five slices below is what that
// refusal judges.
func TestResolveSpBdevConf(t *testing.T) {
	const kib = uint64(1024)
	merged := func(stripe uint64) *pb.BdevConf {
		return &pb.BdevConf{
			DmPoolConf:  &pb.DmPoolConf{DataBlockSize: 1024 * kib},
			DmRaid0Conf: &pb.DmRaid0Conf{StripeSize: stripe},
		}
	}
	for _, tc := range []struct {
		name     string
		conf     *pb.BdevConf
		sliceCnt uint32
		named    bool
		want     uint64
	}{
		{"inherited 1 MiB at 5 slices", merged(1024 * kib), 5, false,
			512 * kib},
		{"inherited 1 MiB at 32 slices", merged(1024 * kib), 32, false,
			128 * kib},
		{"inherited 64 KiB at 32 slices fits", merged(64 * kib), 32, false,
			64 * kib},
		{"named 1 MiB at 5 slices is kept", merged(1024 * kib), 5, true,
			1024 * kib},
		{"both zero: the block's default, narrowed", merged(0), 32, false,
			128 * kib},
	} {
		got := ResolveSpBdevConf(tc.conf, tc.sliceCnt, tc.named)
		if stripe := got.GetDmRaid0Conf().GetStripeSize(); stripe != tc.want {
			t.Errorf("%s: stripe_size %d, want %d", tc.name, stripe, tc.want)
		}
		if got.GetDmPoolConf().GetLowWaterMarkPct() !=
			common.DefaultPoolLowWatermarkPct {
			t.Errorf("%s: the rest of ResolveBdevConf did not run: %v",
				tc.name, got)
		}
	}
}

// TestCloneOptimalIo pins the kernel's rule for the optimal IO size of a
// clone's dm-clone (architecture.md, Clones): the least common multiple of
// the source's and the destination raid0's sizes, kept when it is at least
// one region, the destination's block, and a whole number of regions, else
// the region. The first three rows are the lab's own (a five-slice pool of
// 512 KiB stripes under a 1 MiB block, as a clone destination).
func TestCloneOptimalIo(t *testing.T) {
	const kib = uint64(1024)
	const mib = 1024 * kib
	for _, tc := range []struct {
		name        string
		srcStripe   uint64
		srcSliceCnt uint32
		dstStripe   uint64
		dstSliceCnt uint32
		dstBlock    uint64
		want        uint64
	}{
		{"1 x 1 MiB into 5 x 512 KiB", mib, 1, 512 * kib, 5, mib, 5 * mib},
		{"2 x 1 MiB into 5 x 512 KiB", mib, 2, 512 * kib, 5, mib, 10 * mib},
		{"8 x 128 KiB into 5 x 512 KiB", 128 * kib, 8, 512 * kib, 5, mib,
			5 * mib},
		{"32 x 128 KiB into 1 x 1 MiB", 128 * kib, 32, mib, 1, mib, 4 * mib},
		{"1 x 1 MiB into 32 x 128 KiB", mib, 1, 128 * kib, 32, mib, 4 * mib},
		{"the same 5 x 512 KiB on both ends is not whole regions",
			512 * kib, 5, 512 * kib, 5, mib, mib},
		{"the same 4 x 1 MiB on both ends", mib, 4, mib, 4, mib, 4 * mib},
		{"a multiple below one region is the region", 64 * kib, 8,
			128 * kib, 1, 2 * mib, 2 * mib},
		{"a multiple not a whole number of regions is the region", mib, 3,
			mib, 1, 2 * mib, 2 * mib},
		{"a block over the bound is at least the block", 64 * kib, 1,
			128 * kib, 1, 8 * mib, 8 * mib},
		{"a zero side takes no part", 0, 0, 128 * kib, 32, mib, 4 * mib},
	} {
		got := CloneOptimalIo(tc.srcStripe, tc.srcSliceCnt, tc.dstStripe,
			tc.dstSliceCnt, tc.dstBlock)
		if got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}
