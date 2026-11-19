package model

import (
	"cmp"
	"slices"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ClusterSpaceOf is the space answer of architecture.md, Clusters,
// GetClusterCapacity: what pools of legCnt legs a group, cntlrCnt cntlrs and
// sliceCnt slices can still place, computed from a cluster's disk node and
// controller node records, its stored conf and its pool count. The counts
// arrive resolved, each at least one, and cc has passed
// ValidateClusterConf: the gateway does both before it reads a record.
//
// A disk node counts when DnAllocatable holds, a controller node when
// CnAllocatable does: those are the nodes a scan can offer (architecture.md,
// Capacity index keys). Raw space is every record's extents, charged space
// what its pointers hold, its total less its free, and free space the free
// extents of the nodes that count; what the nodes that do not count hold
// free is raw less charged less free.
//
// A pool takes 2*sliceCnt groups, each with legCnt legs in distinct
// locations, every leg of the pool on a disk node of its own, and cntlrCnt
// cntlrs on controller nodes of their own, each charged the pool's whole
// footprint (architecture.md, Per-operation allocation). The limits are
// judged in CapacityLimit's order, each setting a placeable size and a
// largest first data group of zero: the pool ceiling; fewer controller
// nodes that count than cntlrs; fewer disk nodes that count than the
// pool's legs; and disk nodes in too few locations, where a location gives
// a pool at most one leg of each group, so at most min(its disk nodes,
// 2*sliceCnt) legs. Otherwise the placeable group space is the smaller of
// two shares (spread): on the disk side the free extents per location, a
// location holding at most one leg of each group, and on the controller side
// the free extents per controller node, a node holding at most one cntlr of
// each pool. The disk side wins a tie (DN_SPACE), else the budgets limit
// (CN_BUDGET).
//
// The largest first data group is the smaller of two bounds. On the disk
// side it is the largest size whose disk nodes, those with at least that
// many free extents, pass the location count above for every group of the
// pool at that size (largestDnGroup). With every disk node in a location of
// its own this is the worst case over the gateway's draws, where its
// earlier groups take the fullest disk nodes; where a location holds
// several, a draw can still leave a data group short of locations and the
// create refuses, so a repeat can build it. On the controller side the
// cntlrCnt-th fullest controller node must hold the footprint, sliceCnt
// times one meta extent and the data group. When either bound is zero no
// pool of the shape fits at all, so the placeable size is zero too and the
// limit names that side, the disk side first.
func ClusterSpaceOf(
	cc *pb.ClusterConf,
	dns []*pb.DnConf,
	cns []*pb.CnConf,
	poolCnt uint64,
	legCnt uint32,
	cntlrCnt uint32,
	sliceCnt uint32,
) *pb.ClusterSpace {
	extent := cc.GetDnBinConf().GetExtentSize()
	space := &pb.ClusterSpace{
		LegCnt:   legCnt,
		CntlrCnt: cntlrCnt,
		SliceCnt: sliceCnt,
		PoolCnt:  uint32(min(poolCnt, uint64(^uint32(0)))),
	}
	var dnRaw, dnCharged, dnFree uint64
	locFree := map[string]uint64{}
	locFrees := map[string][]uint64{}
	dnCnt := 0
	for _, dn := range dns {
		dnRaw += dn.GetTotalExtCnt()
		dnCharged += dn.GetTotalExtCnt() - min(dn.GetFreeExtCnt(),
			dn.GetTotalExtCnt())
		if !DnAllocatable(dn, cc.GetDnBinConf()) {
			continue
		}
		dnCnt++
		dnFree += dn.GetFreeExtCnt()
		locFree[dn.GetLocation()] += dn.GetFreeExtCnt()
		locFrees[dn.GetLocation()] = append(locFrees[dn.GetLocation()],
			dn.GetFreeExtCnt())
	}
	var cnRaw, cnCharged, cnFree uint64
	var cnFrees []uint64
	for _, cn := range cns {
		cnRaw += cn.GetTotalExtCnt()
		cnCharged += cn.GetTotalExtCnt() - min(cn.GetFreeExtCnt(),
			cn.GetTotalExtCnt())
		if !CnAllocatable(cn) {
			continue
		}
		cnFree += cn.GetFreeExtCnt()
		cnFrees = append(cnFrees, cn.GetFreeExtCnt())
	}
	space.DnRawBytes = mulSat(dnRaw, extent)
	space.DnChargedBytes = mulSat(dnCharged, extent)
	space.DnFreeBytes = mulSat(dnFree, extent)
	space.CnRawBytes = mulSat(cnRaw, extent)
	space.CnChargedBytes = mulSat(cnCharged, extent)
	space.CnFreeBytes = mulSat(cnFree, extent)

	grpCnt := 2 * int(sliceCnt)
	legs := int(legCnt)
	switch {
	case poolCnt >= common.MaxSpCntPerCluster:
		space.Limit = pb.CapacityLimit_CAPACITY_LIMIT_POOL_CEILING
		return space
	case len(cnFrees) < int(cntlrCnt):
		space.Limit = pb.CapacityLimit_CAPACITY_LIMIT_CN_COUNT
		return space
	case dnCnt < legs*grpCnt:
		space.Limit = pb.CapacityLimit_CAPACITY_LIMIT_DN_COUNT
		return space
	case locLegs(locFrees, 0, grpCnt) < legs*grpCnt:
		space.Limit = pb.CapacityLimit_CAPACITY_LIMIT_DN_LOCATIONS
		return space
	}
	locShares := make([]uint64, 0, len(locFree))
	for _, free := range locFree {
		locShares = append(locShares, free)
	}
	dnShare := spread(locShares, legs)
	cnShare := spread(slices.Clone(cnFrees), int(cntlrCnt))
	if dnShare <= cnShare {
		space.Limit = pb.CapacityLimit_CAPACITY_LIMIT_DN_SPACE
		space.PlaceableBytes = mulSat(dnShare, extent)
	} else {
		space.Limit = pb.CapacityLimit_CAPACITY_LIMIT_CN_BUDGET
		space.PlaceableBytes = mulSat(cnShare, extent)
	}
	slices.SortFunc(cnFrees, func(a, b uint64) int { return cmp.Compare(b, a) })
	cnGroup := uint64(0)
	if perSlice := cnFrees[cntlrCnt-1] / uint64(sliceCnt); perSlice > 1 {
		// The footprint is sliceCnt times one meta extent and the data
		// group.
		cnGroup = perSlice - 1
	}
	dnGroup := largestDnGroup(locFrees, legs, grpCnt)
	space.LargestInitExtCnt = min(dnGroup, cnGroup)
	// No first data group fits: no pool of the shape does either, whatever
	// the shares say, and the limit is the side with no room, the disk
	// side first as on a tie.
	switch {
	case dnGroup == 0:
		space.Limit = pb.CapacityLimit_CAPACITY_LIMIT_DN_SPACE
		space.PlaceableBytes = 0
	case cnGroup == 0:
		space.Limit = pb.CapacityLimit_CAPACITY_LIMIT_CN_BUDGET
		space.PlaceableBytes = 0
	}
	return space
}

// locLegs is the legs that the disk nodes with at least atLeast free
// extents can give one pool of grpCnt groups: a location gives at most one
// leg of each group, so at most min(its such disk nodes, grpCnt).
func locLegs(locFrees map[string][]uint64, atLeast uint64, grpCnt int) int {
	legs := 0
	for _, frees := range locFrees {
		cnt := 0
		for _, free := range frees {
			if free >= atLeast {
				cnt++
			}
		}
		legs += min(cnt, grpCnt)
	}
	return legs
}

// largestDnGroup is the largest free extent count v such that the disk
// nodes with at least v free give a pool of grpCnt groups legs*grpCnt legs
// under the location count of locLegs: the (legs*grpCnt)-th largest free
// count once each location keeps its grpCnt fullest disk nodes. Zero when
// there is none.
func largestDnGroup(locFrees map[string][]uint64, legs int, grpCnt int) uint64 {
	var kept []uint64
	for _, frees := range locFrees {
		sorted := slices.Clone(frees)
		slices.SortFunc(sorted, func(a, b uint64) int { return cmp.Compare(b, a) })
		kept = append(kept, sorted[:min(len(sorted), grpCnt)]...)
	}
	need := legs * grpCnt
	if need == 0 || len(kept) < need {
		return 0
	}
	slices.SortFunc(kept, func(a, b uint64) int { return cmp.Compare(b, a) })
	return kept[need-1]
}

// spread is the largest x with sum(min(v, x)) >= k*x over values: the most
// each of k parts can take when no value gives more than x to the parts
// together, as a location gives one leg of each group and a controller node
// one cntlr of each pool. It is the least, over j from 0 to k-1, of the
// values' sum less the j largest, over k-j; zero for fewer than k non-zero
// values. It sorts values in place.
func spread(values []uint64, k int) uint64 {
	if k <= 0 {
		return 0
	}
	slices.SortFunc(values, func(a, b uint64) int { return cmp.Compare(b, a) })
	rest := uint64(0)
	for _, value := range values {
		rest += value
	}
	best := ^uint64(0)
	for j := range k {
		if j > 0 {
			if j-1 < len(values) {
				rest -= values[j-1]
			}
		}
		best = min(best, rest/uint64(k-j))
	}
	return best
}
