package model

import (
	"slices"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// UnreachedGroups are the groups, by id and in ascending order, every leg
// of which reads RES_STATUS_ERROR in a cntlr's report (leg_id_to_leg) and
// has one side: a standby that reports so reaches no leg of the group and
// could build nothing of it as the primary, so the failover passes over it
// (dnv-worker.md AR5). A leg with no row is not counted as unreached, nor a
// leg with two sides, a migrating one, whose row reads ERROR while either
// side has no live path though the other may serve (a standby's transport
// probe, cnagent.md CN11); and an error on some legs of a group but not
// all is not enough: with one disk node down every standby of a RAID1 pool
// reads such a row and can still serve. grps are the pool's groups: the
// slices' meta and data groups and the clones' clone meta groups.
func UnreachedGroups(info *pb.CntlrInfo, grps []*pb.Group) []uint64 {
	rows := info.GetLegIdToLeg()
	var out []uint64
	for _, grp := range grps {
		legs := grp.GetLegList()
		if len(legs) == 0 {
			continue
		}
		all := true
		for _, leg := range legs {
			row, ok := rows[leg.GetLegId()]
			if !ok || row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
				len(leg.GetSideList()) > 1 {
				all = false
				break
			}
		}
		if all {
			out = append(out, grp.GetGrpId())
		}
	}
	slices.Sort(out)
	return out
}

// PoolGroups are the groups of a pool's slices, meta and data, and of its
// clones' clone meta groups, as UnreachedGroups reads them.
func PoolGroups(slices []*pb.Slice, clones []*pb.Clone) []*pb.Group {
	var out []*pb.Group
	for _, slice := range slices {
		out = append(out, slice.GetMetaGrpList()...)
		out = append(out, slice.GetDataGrpList()...)
	}
	for _, clone := range clones {
		if grp := clone.GetMetaGrp(); grp != nil {
			out = append(out, grp)
		}
	}
	return out
}
