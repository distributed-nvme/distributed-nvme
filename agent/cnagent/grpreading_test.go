package cnagent

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestGrpReadingCarriesInSyncSlots checks CN28's group reading: every leg
// whose member md holds in sync carries md's slot of it, and no other leg
// does — not a member md copies onto, not one it has failed, and not a leg
// md holds nothing on. The sp worker fences the leg of the lowest slot last
// in a failover (dnv-worker.md RW24).
func TestGrpReadingCarriesInSyncSlots(t *testing.T) {
	gp := &grpPlan{grpId: 101, raid1: true}
	legs := []*legPlan{
		{legId: 11, name: "dnv-leg-11", grp: gp},
		{legId: 12, name: "dnv-leg-12", grp: gp},
		{legId: 13, name: "dnv-leg-13", grp: gp},
		{legId: 14, name: "dnv-leg-14", grp: gp},
		// Leg 15 has no member: md holds nothing on it.
		{legId: 15, name: "dnv-leg-15", grp: gp},
	}
	gp.legs = legs
	detail := &MdDetail{
		State:    "clean",
		Degraded: 1,
		Members: []MdMember{
			// Slot 1 in sync, slot 0 in sync: the fence-last leg is leg 12.
			{DmName: "dnv-leg-11", State: "in_sync,failfast", Slot: "1"},
			{DmName: "dnv-leg-12", State: "in_sync,failfast", Slot: "0"},
			// A member md copies onto holds a place but is not in sync.
			{DmName: "dnv-leg-13", State: "failfast", Slot: "2"},
			// Leg 14's member md failed out of its slot.
			{DmName: "dnv-leg-14", State: "faulty,failfast", Slot: "none"},
		},
	}
	info := &pb.CntlrInfo{GrpIdToGrpInfo: make(map[uint64]*pb.CntlrInfo_GrpInfo)}
	reportGrpReading(info, gp, detail, grpRefusals{}, 0, 0)
	got := info.GetGrpIdToGrpInfo()[101]
	if got == nil {
		t.Fatalf("no reading of the group: %v", info.GetGrpIdToGrpInfo())
	}
	if inSync := got.GetInSyncLegIdList(); len(inSync) != 2 || inSync[0] != 11 || inSync[1] != 12 {
		t.Fatalf("in sync = %v, want [11 12]", inSync)
	}
	want := map[uint64]uint32{11: 1, 12: 0}
	slots := got.GetInSyncLegIdToSlot()
	if len(slots) != len(want) {
		t.Fatalf("in-sync slots = %v, want %v", slots, want)
	}
	for legId, slot := range want {
		if have, ok := slots[legId]; !ok || have != slot {
			t.Fatalf("in-sync slots = %v, want %v", slots, want)
		}
	}
}
