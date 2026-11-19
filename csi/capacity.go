package csi

import (
	"math/bits"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// volumeSteps is the first data group's share of a volume (csi.md, The
// volume model): one part in volumeSteps, rounded up to whole extents;
// auto-grow adds data groups of that size until the slice's data covers the
// volume.
const volumeSteps = 8

// initExtCnt is the extents of a volume's first data group: one part in
// volumeSteps of its size, rounded up to whole extents, at least one.
func initExtCnt(size uint64, extentSize uint64) uint64 {
	part := (size + volumeSteps - 1) / volumeSteps
	ext := (part + extentSize - 1) / extentSize
	if ext == 0 {
		ext = 1
	}
	return ext
}

// capacityOf turns the gateway's space answer for a pool of the class's
// shape into the two numbers Kubernetes asks for (csi.md, Capacity): the
// available bytes are the answer's placeable bytes, and the largest volume is
// the one whose first data group, one part in volumeSteps of it, is the
// largest first data group the answer names, never above the available
// bytes. The arithmetic is the gateway's alone (architecture.md, Clusters,
// GetClusterCapacity); this only converts it to the volume model.
func capacityOf(space *pb.ClusterSpace, extentSize uint64) (uint64, uint64) {
	available := space.GetPlaceableBytes()
	hi, maxVolume := bits.Mul64(space.GetLargestInitExtCnt(),
		extentSize*volumeSteps)
	if hi != 0 || maxVolume > available {
		maxVolume = available
	}
	return available, maxVolume
}
