package model

import (
	"slices"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestUnreachedGroups pins the test the failover passes a standby over by
// (dnv-worker.md AR5): a group every leg of which reads ERROR counts, one
// with an error on one leg only does not, a leg with no row is not
// unreached, nor a migrating leg with two sides, and a clone meta group
// counts as a slice's group does.
func TestUnreachedGroups(t *testing.T) {
	grp := func(id uint64, legs ...uint64) *pb.Group {
		g := &pb.Group{GrpId: id}
		for _, legId := range legs {
			g.LegList = append(g.LegList, &pb.Leg{LegId: legId})
		}
		return g
	}
	errRow := &pb.ResInfo{Status: pb.ResStatus_RES_STATUS_ERROR}
	okRow := &pb.ResInfo{Status: pb.ResStatus_RES_STATUS_OK}
	poolSlices := []*pb.Slice{{
		MetaGrpList: []*pb.Group{grp(10, 1, 2)},
		DataGrpList: []*pb.Group{grp(20, 3, 4), grp(30, 5, 6)},
	}}
	clones := []*pb.Clone{{MetaGrp: grp(40, 7, 8)}}
	// A migrating leg, with two sides, reads ERROR while either side has no
	// live path: it does not count as unreached.
	migr := grp(50, 9)
	migr.LegList[0].SideList = []*pb.Side{{SideId: 1}, {SideId: 2}}
	poolSlices[0].DataGrpList = append(poolSlices[0].DataGrpList, migr)
	info := &pb.CntlrInfo{LegIdToLeg: map[uint64]*pb.ResInfo{
		1: errRow, 2: errRow, // group 10: every leg
		3: errRow, 4: okRow, // group 20: one leg
		5: errRow,            // group 30: leg 6 has no row
		7: errRow, 8: errRow, // the clone meta group
		9: errRow, // group 50: a migrating leg
	}}
	got := UnreachedGroups(info, PoolGroups(poolSlices, clones))
	if want := []uint64{10, 40}; !slices.Equal(got, want) {
		t.Fatalf("unreached groups %v, want %v", got, want)
	}
	if got := UnreachedGroups(nil, PoolGroups(poolSlices, clones)); len(got) != 0 {
		t.Fatalf("a report with no info reads groups %v unreached", got)
	}
}
