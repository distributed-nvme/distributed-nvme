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

// poolGrpCnt is the groups a volume's pool of one slice is made of: its
// meta group and its data group.
const poolGrpCnt = 2

// capacityOf turns the cluster capacity call's records into the two numbers
// Kubernetes asks for (csi.md, Capacity). A pool puts every leg on a disk
// node of its own and every cntlr on a controller node of its own, charged
// with the whole pool's extents (architecture.md, Per-operation
// allocation), so a class that needs more eligible disk nodes or controller
// nodes than the cluster has gets nothing; an eligible node is enabled,
// healthy, below its ceiling and has an extent free. Otherwise the available
// bytes are the smaller of the disk side and the controller side: the free
// extents of the eligible disk nodes, times the extent size, divided by the
// legs a group has; and the free extents of the eligible controller nodes,
// times the extent size, divided by the cntlr count. The largest volume is
// the one whose first data group, one part in volumeSteps of it, still fits
// the disk node that would carry its last data leg, the legs-th largest free
// count, since the meta group's legs sit on other disk nodes, and the
// controller node that would carry its last cntlr, the cntlrs-th largest
// free count less the meta group's extent; and it is never above the
// available bytes. Both are estimates, as the capacity call itself is.
func capacityOf(
	reply *pb.GetClusterCapacityReply,
	legs int,
	cntlrs uint32,
) (uint64, uint64) {
	extent := reply.GetExtentSize()
	if extent == 0 || legs <= 0 || cntlrs == 0 {
		return 0, 0
	}
	var dnFree, cnFree []uint64
	dnSum, cnSum := uint64(0), uint64(0)
	for _, dn := range reply.GetAddrPortToDn() {
		if dn.GetDisabled() || dn.GetErrEpoch() != 0 ||
			dn.GetSideCnt() >= common.MaxSideCntPerDn || dn.GetFreeExtCnt() == 0 {
			continue
		}
		dnSum += dn.GetFreeExtCnt()
		dnFree = append(dnFree, dn.GetFreeExtCnt())
	}
	for _, cn := range reply.GetAddrPortToCn() {
		if cn.GetDisabled() || cn.GetErrEpoch() != 0 ||
			cn.GetCntlrCnt() >= common.MaxCntlrCntPerCn || cn.GetFreeExtCnt() == 0 {
			continue
		}
		cnSum += cn.GetFreeExtCnt()
		cnFree = append(cnFree, cn.GetFreeExtCnt())
	}
	if len(dnFree) < poolGrpCnt*legs || len(cnFree) < int(cntlrs) {
		return 0, 0
	}
	available := dnSum * extent / uint64(legs)
	if ctrl := cnSum * extent / uint64(cntlrs); ctrl < available {
		available = ctrl
	}
	sort.Slice(dnFree, func(i, j int) bool { return dnFree[i] > dnFree[j] })
	sort.Slice(cnFree, func(i, j int) bool { return cnFree[i] > cnFree[j] })
	dataExt := dnFree[legs-1]
	if cnExt := cnFree[cntlrs-1] - 1; cnExt < dataExt {
		dataExt = cnExt
	}
	maxVolume := dataExt * extent * volumeSteps
	if maxVolume > available {
		maxVolume = available
	}
	return available, maxVolume
}
