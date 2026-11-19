package csi

import (
	"testing"

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

// TestCapacityOf pins the conversion of csi.md, Capacity: the available
// bytes are the placeable bytes, and the largest volume is volumeSteps times
// the largest first data group, never above them.
func TestCapacityOf(t *testing.T) {
	for _, tc := range []struct {
		label                string
		space                *pb.ClusterSpace
		wantAvail, wantLarge uint64
	}{
		{"the first data group bounds the volume",
			&pb.ClusterSpace{PlaceableBytes: 1 << 40, LargestInitExtCnt: 10},
			1 << 40, 10 * fakeExtent * volumeSteps},
		{"the placeable bytes bound the volume",
			&pb.ClusterSpace{PlaceableBytes: 30 * fakeExtent, LargestInitExtCnt: 10},
			30 * fakeExtent, 30 * fakeExtent},
		{"a product past uint64 is the placeable bytes",
			&pb.ClusterSpace{PlaceableBytes: 1 << 50, LargestInitExtCnt: 1 << 40},
			1 << 50, 1 << 50},
		{"nothing placeable", &pb.ClusterSpace{}, 0, 0},
	} {
		avail, large := capacityOf(tc.space, fakeExtent)
		if avail != tc.wantAvail || large != tc.wantLarge {
			t.Errorf("%s: available %d, largest %d, want %d and %d",
				tc.label, avail, large, tc.wantAvail, tc.wantLarge)
		}
	}
}
