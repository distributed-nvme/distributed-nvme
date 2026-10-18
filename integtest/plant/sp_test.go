// Tests of the pool plan PutSp joins its five lists into: the wiring
// validation that refuses every dangling reference and duplicate id, the
// footprint and next_id it derives, and the spare-leg and slot-list rules.
// None of them needs etcd.
package plant

import (
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// smokeSpec is worker_test.sh's case S placement: two cntlrs, one slice, a
// raid1 meta group of 1 extent and a raid1 data group of 2, four sides on two
// DNs.
func smokeSpec() SpSpec {
	return SpSpec{
		Name:  "sp0",
		Id:    1,
		Slots: []uint32{0, 1, 2},
		Cntlrs: []CntlrSpec{
			{CntlrId: 1, CnId: 1, Slot: 0, Primary: true},
			{CntlrId: 2, CnId: 2, Slot: 1, Primary: false},
		},
		Slices: []SliceSpec{{SliceId: 1, SliceIdx: 0}},
		Groups: []GroupSpec{
			{SliceId: 1, GrpId: 2, Meta: true, ExtCnt: 1, Raid1: true},
			{SliceId: 1, GrpId: 5, Meta: false, ExtCnt: 2, Raid1: true},
		},
		Legs: []LegSpec{
			{GrpId: 2, LegId: 3, LegIdx: 0},
			{GrpId: 2, LegId: 4, LegIdx: 1},
			{GrpId: 5, LegId: 6, LegIdx: 0},
			{GrpId: 5, LegId: 7, LegIdx: 1},
		},
		Sides: []SideSpec{
			{LegId: 3, SideId: 8, DnId: 1, Slot: 0},
			{LegId: 4, SideId: 9, DnId: 2, Slot: 0},
			{LegId: 6, SideId: 10, DnId: 1, Slot: 0},
			{LegId: 7, SideId: 11, DnId: 2, Slot: 0},
		},
	}
}

func TestNewSpPlan(t *testing.T) {
	plan, err := newSpPlan(smokeSpec())
	if err != nil {
		t.Fatalf("newSpPlan failed: %v", err)
	}
	if !plan.raid1 {
		t.Fatalf("plan.raid1 = false")
	}
	// Σ ext_cnt over ALL groups is what one cntlr's CN reserves
	// (architecture.md, Storage pools).
	if plan.footprint != 3 {
		t.Fatalf("footprint = %d, want 3", plan.footprint)
	}
	// next_id must be past every id the caller assigned.
	if plan.nextId != 12 {
		t.Fatalf("next_id = %d, want 12", plan.nextId)
	}
	if plan.grpOfLeg[6] != 5 || plan.grpOfLeg[3] != 2 {
		t.Fatalf("grpOfLeg = %v", plan.grpOfLeg)
	}
	// A side of a data-group leg costs the data group's 2 extents.
	if plan.extOfLeg[6] != 2 || plan.extOfLeg[3] != 1 {
		t.Fatalf("extOfLeg = %v", plan.extOfLeg)
	}
	if len(plan.sides) != 4 || len(plan.legs) != 4 {
		t.Fatalf("plan has %d sides / %d legs", len(plan.sides), len(plan.legs))
	}
	if got := plan.slots; len(got) != 3 || got[2] != 2 {
		t.Fatalf("slots = %v, want the spec's 0,1,2", got)
	}
}

// TestNewSpPlanSlotsDefaultToAll pins the empty slot list: every cntlid slot
// a CN has, in order.
func TestNewSpPlanSlotsDefaultToAll(t *testing.T) {
	spec := smokeSpec()
	spec.Slots = nil
	plan, err := newSpPlan(spec)
	if err != nil {
		t.Fatalf("newSpPlan failed: %v", err)
	}
	if len(plan.slots) != common.CnCntlidSlotCnt {
		t.Fatalf("slots = %v, want %d of them", plan.slots,
			common.CnCntlidSlotCnt)
	}
	for i, slot := range plan.slots {
		if slot != uint32(i) {
			t.Fatalf("slots = %v, want 0 to %d in order", plan.slots,
				common.CnCntlidSlotCnt-1)
		}
	}
}

// TestNewSpPlanSpareLegs pins a spare leg: it shares the group's leg_idx
// space and costs its DN the group's extents like an active leg's side.
func TestNewSpPlanSpareLegs(t *testing.T) {
	spec := smokeSpec()
	spec.Legs = append(spec.Legs,
		LegSpec{GrpId: 5, LegId: 12, LegIdx: 2, Spare: true})
	spec.Sides = append(spec.Sides,
		SideSpec{LegId: 12, SideId: 13, DnId: 3, Slot: 0})
	plan, err := newSpPlan(spec)
	if err != nil {
		t.Fatalf("newSpPlan failed: %v", err)
	}
	if plan.grpOfLeg[12] != 5 || plan.extOfLeg[12] != 2 {
		t.Fatalf("the spare is in group %#x at %d extents, want 0x5 at 2",
			plan.grpOfLeg[12], plan.extOfLeg[12])
	}
	if plan.nextId != 14 {
		t.Fatalf("next_id = %d, want 14", plan.nextId)
	}
}

func TestNewSpPlanRejectsBadWiring(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(spec *SpSpec)
		want   string
	}{
		{
			name: "dangling group slice",
			mutate: func(spec *SpSpec) {
				spec.Groups[0] = GroupSpec{
					SliceId: 9, GrpId: 2, Meta: true, ExtCnt: 1, Raid1: true,
				}
			},
			want: "no slice with id",
		},
		{
			name: "dangling leg group",
			mutate: func(spec *SpSpec) {
				spec.Legs[0] = LegSpec{GrpId: 9, LegId: 3, LegIdx: 0}
			},
			want: "no group with id",
		},
		{
			name: "dangling side leg",
			mutate: func(spec *SpSpec) {
				spec.Sides[0] = SideSpec{LegId: 9, SideId: 8, DnId: 1}
			},
			want: "no leg with id",
		},
		{
			name: "duplicate side id",
			mutate: func(spec *SpSpec) {
				spec.Sides[1] = SideSpec{LegId: 4, SideId: 8, DnId: 2}
			},
			want: "side id 0x8 is used twice",
		},
		{
			name: "duplicate leg id",
			mutate: func(spec *SpSpec) {
				spec.Legs[1] = LegSpec{GrpId: 2, LegId: 3, LegIdx: 1}
			},
			want: "leg id 0x3 is used twice",
		},
		{
			name: "duplicate cntlr id",
			mutate: func(spec *SpSpec) {
				spec.Cntlrs[1] = CntlrSpec{CntlrId: 1, CnId: 2, Slot: 1}
			},
			want: "cntlr id 0x1 is used twice",
		},
		{
			name: "id zero",
			mutate: func(spec *SpSpec) {
				spec.Slices[0] = SliceSpec{SliceId: 0, SliceIdx: 0}
			},
			want: "never valid",
		},
		{
			name: "two primaries",
			mutate: func(spec *SpSpec) {
				spec.Cntlrs[1] = CntlrSpec{
					CntlrId: 2, CnId: 2, Slot: 1, Primary: true,
				}
			},
			want: "exactly one cntlr must be primary",
		},
		{
			name: "no primary",
			mutate: func(spec *SpSpec) {
				spec.Cntlrs[0] = CntlrSpec{CntlrId: 1, CnId: 1, Slot: 0}
			},
			want: "exactly one cntlr must be primary",
		},
		{
			name: "shared cntlid slot",
			mutate: func(spec *SpSpec) {
				spec.Cntlrs[1] = CntlrSpec{CntlrId: 2, CnId: 2, Slot: 0}
			},
			want: "already taken by cntlr",
		},
		{
			name: "slot outside the slot list",
			mutate: func(spec *SpSpec) {
				spec.Cntlrs[1] = CntlrSpec{CntlrId: 2, CnId: 2, Slot: 7}
			},
			want: "is not in the slot list",
		},
		{
			name: "side slot outside the slot list",
			mutate: func(spec *SpSpec) {
				spec.Sides[0].Slot = 7
			},
			want: "is not in the slot list",
		},
		{
			name: "two cntlrs on one cn",
			mutate: func(spec *SpSpec) {
				spec.Cntlrs[1] = CntlrSpec{CntlrId: 2, CnId: 1, Slot: 1}
			},
			want: "already hosts a cntlr of this sp",
		},
		{
			name: "mixed redundancy",
			mutate: func(spec *SpSpec) {
				spec.Groups[1] = GroupSpec{SliceId: 1, GrpId: 5, ExtCnt: 2}
			},
			want: "same redundancy",
		},
		{
			name: "two legs of one group on one dn",
			mutate: func(spec *SpSpec) {
				spec.Sides[1] = SideSpec{LegId: 4, SideId: 9, DnId: 1}
			},
			want: "already carries side",
		},
		{
			name: "duplicate leg_idx",
			mutate: func(spec *SpSpec) {
				spec.Legs[1] = LegSpec{GrpId: 2, LegId: 4, LegIdx: 0}
			},
			want: "already used by leg",
		},
		{
			name: "a spare's leg_idx taken by an active leg",
			mutate: func(spec *SpSpec) {
				spec.Legs = append(spec.Legs,
					LegSpec{GrpId: 2, LegId: 20, LegIdx: 0, Spare: true})
				spec.Sides = append(spec.Sides,
					SideSpec{LegId: 20, SideId: 21, DnId: 3})
			},
			want: "already used by leg",
		},
		{
			name: "duplicate slice_idx",
			mutate: func(spec *SpSpec) {
				spec.Slices = append(spec.Slices,
					SliceSpec{SliceId: 12, SliceIdx: 0})
			},
			want: "already used by slice",
		},
		{
			name: "group with no leg",
			mutate: func(spec *SpSpec) {
				spec.Groups = append(spec.Groups, GroupSpec{
					SliceId: 1, GrpId: 20, ExtCnt: 2, Raid1: true,
				})
			},
			want: "has no leg",
		},
		{
			name: "group whose only leg is a spare",
			mutate: func(spec *SpSpec) {
				spec.Groups = append(spec.Groups, GroupSpec{
					SliceId: 1, GrpId: 20, ExtCnt: 2, Raid1: true,
				})
				spec.Legs = append(spec.Legs,
					LegSpec{GrpId: 20, LegId: 21, LegIdx: 0, Spare: true})
				spec.Sides = append(spec.Sides,
					SideSpec{LegId: 21, SideId: 22, DnId: 1})
			},
			want: "no leg that is not a spare",
		},
		{
			name: "leg with no side",
			mutate: func(spec *SpSpec) {
				spec.Legs = append(spec.Legs,
					LegSpec{GrpId: 2, LegId: 20, LegIdx: 2})
			},
			want: "has no side",
		},
		{
			name: "spare leg in a redund_none pool",
			mutate: func(spec *SpSpec) {
				for i := range spec.Groups {
					spec.Groups[i].Raid1 = false
				}
				spec.Legs = append(spec.Legs,
					LegSpec{GrpId: 5, LegId: 20, LegIdx: 2, Spare: true})
				spec.Sides = append(spec.Sides,
					SideSpec{LegId: 20, SideId: 21, DnId: 3})
			},
			want: "a spare leg needs md-raid1",
		},
		{
			name: "group of no extent",
			mutate: func(spec *SpSpec) {
				spec.Groups[1].ExtCnt = 0
			},
			want: "ext_cnt must not be zero",
		},
		{
			name: "slot past the CN's slots",
			mutate: func(spec *SpSpec) {
				spec.Slots = []uint32{0, 1, common.CnCntlidSlotCnt}
			},
			want: "CnCntlidSlotCnt",
		},
		{
			name: "slot listed twice",
			mutate: func(spec *SpSpec) {
				spec.Slots = []uint32{0, 1, 1}
			},
			want: "appears twice",
		},
		{
			name: "fewer slots than cntlrs",
			mutate: func(spec *SpSpec) {
				spec.Slots = []uint32{0}
			},
			want: "is not in the slot list",
		},
		{
			name: "undefined level",
			mutate: func(spec *SpSpec) {
				spec.Level = pb.SpLevel(47)
			},
			want: "not a defined SpLevel",
		},
		{
			name: "no name",
			mutate: func(spec *SpSpec) {
				spec.Name = ""
			},
			want: "name is empty",
		},
		{
			name: "sp id zero",
			mutate: func(spec *SpSpec) {
				spec.Id = 0
			},
			want: "sp id 0 is never valid",
		},
		{
			name: "shard past the buckets",
			mutate: func(spec *SpSpec) {
				spec.Shard = common.ShardBucketSize
			},
			want: "out of range",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := smokeSpec()
			tc.mutate(&spec)
			err := spec.Check()
			if err == nil {
				t.Fatalf("Check succeeded, want %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestNewSpPlanRequiresEveryPart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(spec *SpSpec)
		want   string
	}{
		{"no cntlr", func(spec *SpSpec) { spec.Cntlrs = nil },
			"at least one cntlr is required"},
		{"no slice", func(spec *SpSpec) { spec.Slices = nil },
			"at least one slice is required"},
		{"no group", func(spec *SpSpec) { spec.Groups = nil },
			"at least one group is required"},
		// A slot list shorter than the cntlrs leaves two of them on one
		// slot, which the distinct-slot rule refuses (architecture.md,
		// cntlid slots).
		{"fewer slots than cntlrs", func(spec *SpSpec) {
			spec.Cntlrs = []CntlrSpec{
				{CntlrId: 1, CnId: 1, Slot: 0, Primary: true},
				{CntlrId: 2, CnId: 2, Slot: 0},
			}
			spec.Slots = []uint32{0}
		}, "cntlid slot 0 already taken by cntlr 0x1"},
	} {
		spec := smokeSpec()
		tc.mutate(&spec)
		_, err := newSpPlan(spec)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("a plan with %s = %v, want %q", tc.name, err, tc.want)
		}
	}
}
