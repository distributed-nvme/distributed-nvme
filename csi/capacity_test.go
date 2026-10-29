package csi

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestInitExtCnt pins the first data group's size: one part in volumeSteps
// of the volume, rounded up to whole extents, at least one.
func TestInitExtCnt(t *testing.T) {
	for _, tc := range []struct{ size, want uint64 }{
		{1, 1},
		{volumeSteps * fakeExtent, 1},
		{volumeSteps*fakeExtent + 1, 2},
		{10 << 30, 20},
		{0, 1},
	} {
		if got := initExtCnt(tc.size, fakeExtent); got != tc.want {
			t.Errorf("initExtCnt(%d) = %d, want %d", tc.size, got, tc.want)
		}
	}
}

// TestCapacityOf pins the arithmetic of csi.md, Capacity, on hand-made
// records.
func TestCapacityOf(t *testing.T) {
	dn := func(free uint64) *pb.DnCapacityEntry {
		return &pb.DnCapacityEntry{TotalExtCnt: 1024, FreeExtCnt: free}
	}
	cn := func(free uint64) *pb.CnCapacityEntry {
		return &pb.CnCapacityEntry{TotalExtCnt: 1024, FreeExtCnt: free}
	}
	reply := &pb.GetClusterCapacityReply{
		ExtentSize: fakeExtent,
		AddrPortToDn: map[string]*pb.DnCapacityEntry{
			"a": dn(500), "b": dn(30), "c": dn(10),
			"full":     {TotalExtCnt: 1024, FreeExtCnt: 1024, SideCnt: common.MaxSideCntPerDn},
			"disabled": {TotalExtCnt: 1024, FreeExtCnt: 1024, Disabled: true},
			"sick":     {TotalExtCnt: 1024, FreeExtCnt: 1024, ErrEpoch: 1},
		},
		AddrPortToCn: map[string]*pb.CnCapacityEntry{
			"x": cn(10000), "y": cn(10000),
			"busy": {TotalExtCnt: 1024, FreeExtCnt: 1024, CntlrCnt: common.MaxCntlrCntPerCn},
		},
	}
	// RAID1, two cntlrs: 540 free disk extents over 2 legs; the second
	// largest disk node has 30, less the meta group's one, times 8.
	avail, maxVol := capacityOf(reply, 2, 2)
	if want := 540 * fakeExtent / 2; avail != want {
		t.Errorf("available %d, want %d", avail, want)
	}
	if want := 29 * fakeExtent * volumeSteps; maxVol != want {
		t.Errorf("maximum volume %d, want %d", maxVol, want)
	}
	// No redundancy, one cntlr: the largest disk node bounds the volume,
	// capped by the available bytes.
	avail, maxVol = capacityOf(reply, 1, 1)
	if want := 540 * fakeExtent; avail != want || maxVol != want {
		t.Errorf("without redundancy: available %d, maximum %d, want %d",
			avail, maxVol, want)
	}
	// The controller side bounds when it is the smaller.
	reply.AddrPortToCn["x"].FreeExtCnt = 50
	reply.AddrPortToCn["y"].FreeExtCnt = 50
	avail, _ = capacityOf(reply, 2, 2)
	if want := 100 * fakeExtent / 2; avail != want {
		t.Errorf("available %d under a tight controller side, want %d", avail, want)
	}
	// Fewer eligible disk nodes than legs: no volume fits.
	reply.AddrPortToDn = map[string]*pb.DnCapacityEntry{"a": dn(500)}
	if _, maxVol = capacityOf(reply, 2, 2); maxVol != 0 {
		t.Errorf("maximum volume %d with one disk node for two legs", maxVol)
	}
	if avail, maxVol = capacityOf(&pb.GetClusterCapacityReply{}, 2, 2); avail != 0 || maxVol != 0 {
		t.Errorf("an empty reply gives %d and %d", avail, maxVol)
	}
}
