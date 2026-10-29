package csi

import (
	"sort"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// volumeSteps is the "thin in eight steps" of csi.md, The volume model: a
// volume's first data group holds one part in volumeSteps of the volume,
// rounded up to whole extents, and auto-grow adds groups of that size until
// the slice covers the volume.
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

// capacityOf turns the cluster capacity call's records into the two numbers
// Kubernetes asks for (csi.md, Capacity). The available bytes are the
// smaller of the disk side and the controller side: the free extents of the
// disk nodes that are enabled, healthy and below their side ceiling, times
// the extent size, divided by the legs a group has; and the free extents of
// the controller nodes that are enabled, healthy and below their cntlr
// ceiling, times the extent size, divided by the cntlr count, since every
// cntlr charges its controller node the pool's footprint. The largest
// volume is bounded by the disk node that would carry its last leg: the
// legs-th largest free count less the meta group's extent, times the extent
// size, times volumeSteps, since the first data group is one part in
// volumeSteps of the volume; and never above the available bytes. Both are
// estimates, as the capacity call itself is.
func capacityOf(
	reply *pb.GetClusterCapacityReply,
	legs int,
	cntlrs uint32,
) (uint64, uint64) {
	extent := reply.GetExtentSize()
	if extent == 0 || legs <= 0 || cntlrs == 0 {
		return 0, 0
	}
	var dnFree []uint64
	dnSum := uint64(0)
	for _, dn := range reply.GetAddrPortToDn() {
		if dn.GetDisabled() || dn.GetErrEpoch() != 0 ||
			dn.GetSideCnt() >= common.MaxSideCntPerDn {
			continue
		}
		dnSum += dn.GetFreeExtCnt()
		dnFree = append(dnFree, dn.GetFreeExtCnt())
	}
	cnSum := uint64(0)
	for _, cn := range reply.GetAddrPortToCn() {
		if cn.GetDisabled() || cn.GetErrEpoch() != 0 ||
			cn.GetCntlrCnt() >= common.MaxCntlrCntPerCn {
			continue
		}
		cnSum += cn.GetFreeExtCnt()
	}
	available := dnSum * extent / uint64(legs)
	if ctrl := cnSum * extent / uint64(cntlrs); ctrl < available {
		available = ctrl
	}
	sort.Slice(dnFree, func(i, j int) bool { return dnFree[i] > dnFree[j] })
	maxVolume := uint64(0)
	if len(dnFree) >= legs {
		if k := dnFree[legs-1]; k > 1 {
			maxVolume = (k - 1) * extent * volumeSteps
		}
	}
	if maxVolume > available {
		maxVolume = available
	}
	return available, maxVolume
}
