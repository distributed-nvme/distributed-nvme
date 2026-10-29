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

// dnFloorOf is the free extents a disk node needs to be offered at all: the
// level of the cluster's smallest bin (architecture.md, DN bins); a disk
// node below it holds no capacity key, and no scan finds it.
func dnFloorOf(conf *pb.DnBinConf) uint64 {
	shift := conf.GetBin0Shift()
	if shift >= 64 {
		return ^uint64(0)
	}
	return uint64(1) << shift
}

// poolCntOf is the pools a cluster holds, those being deleted included: the
// sum of its shard buckets, which the gateway holds to its pool ceiling.
func poolCntOf(global *pb.SpGlobal) uint64 {
	var n uint64
	for _, c := range global.GetShardBucket() {
		n += uint64(c)
	}
	return n
}

// capacityOf turns the cluster capacity call's records into the two numbers
// Kubernetes asks for (csi.md, Capacity). A disk node counts when it is
// enabled, healthy, below its side ceiling and has dnFloor free extents; a
// controller node when it is enabled, healthy, below its cntlr ceiling and
// has an extent free. A pool puts every leg on a disk node of its own and
// every cntlr on a controller node of its own, charged with the whole
// pool's extents (architecture.md, Per-operation allocation), so a class
// that needs more disk nodes or controller nodes than count gets nothing.
// Otherwise the available bytes are the smaller of the disk side and the
// controller side: the free extents of the disk nodes that count, times the
// extent size, divided by the legs a group has; and the free extents of the
// controller nodes that count, times the extent size, divided by the cntlr
// count. The largest volume is the one whose first data group, one part in
// volumeSteps of it, fits whichever disk nodes the meta group takes, since
// the gateway places the meta group first and at random: of the
// poolGrpCnt*legs disk nodes with the most free extents, at least legs stay
// for the data group, so the bound is the (poolGrpCnt*legs)-th largest free
// count, the worst case, which can be well below what a create builds. It also fits
// the controller node that would carry the last cntlr, the cntlrs-th
// largest free count less the meta group's extent; and it is never above
// the available bytes. Both are estimates, as the capacity call itself is.
func capacityOf(
	reply *pb.GetClusterCapacityReply,
	legs int,
	cntlrs uint32,
	dnFloor uint64,
) (uint64, uint64) {
	extent := reply.GetExtentSize()
	if extent == 0 || legs <= 0 || cntlrs == 0 {
		return 0, 0
	}
	if dnFloor == 0 {
		dnFloor = 1
	}
	var dnFree, cnFree []uint64
	dnSum, cnSum := uint64(0), uint64(0)
	for _, dn := range reply.GetAddrPortToDn() {
		if dn.GetDisabled() || dn.GetErrEpoch() != 0 ||
			dn.GetSideCnt() >= common.MaxSideCntPerDn || dn.GetFreeExtCnt() < dnFloor {
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
	dataExt := dnFree[poolGrpCnt*legs-1]
	if cnExt := cnFree[cntlrs-1] - 1; cnExt < dataExt {
		dataExt = cnExt
	}
	maxVolume := dataExt * extent * volumeSteps
	if maxVolume > available {
		maxVolume = available
	}
	return available, maxVolume
}
