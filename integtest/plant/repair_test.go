package plant

import (
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The etcd-backed tests of the repairs and retired legs the library plants:
// each read back through model as the worker reads them, the setters, the
// ids the records hold kept apart from later plants, and both drains giving
// back what the records charged.

// TestPutRepairAndRetiredLegReadBack plants a retired leg and a fresh repair
// of the small pool's meta group and reads both back, then drives every
// setter on them.
func TestPutRepairAndRetiredLegReadBack(t *testing.T) {
	ctx := testCtx(t)
	c := plantSmallCluster(t, ctx, true)
	cid, ref := c.cl.Id, c.sp.Ref
	revBefore := c.spRev(t, ctx)

	retired, err := PutRetiredLeg(ctx, c.cli, c.cl, ref, RetiredLegSpec{
		GrpId: 2, LegIdx: 1, Dn: 4, Epoch: 5000,
	})
	if err != nil {
		t.Fatalf("PutRetiredLeg: %v", err)
	}
	if retired.LegId != 12 || retired.SideId != 13 || retired.SliceId != 1 ||
		retired.CloneName != "" || retired.ExtCnt != 1 ||
		retired.SpRev != revBefore+1 {
		t.Errorf("PutRetiredLeg = %+v, want ids 12 and 13 from next_id in "+
			"slice 1, one extent, one bump", retired)
	}
	c.dnHolds(t, ctx, 4, nodeFree-1, nodeFree, []*pb.SidePointer{
		{SpId: ref.Id, LegId: 12, SideId: 13},
	}, retired.DnRevision)

	rep, err := PutRepair(ctx, c.cli, c.cl, ref, RepairSpec{
		Name: "rep0", GrpId: 2, BadLegId: 4, Dn: 5,
	})
	if err != nil {
		t.Fatalf("PutRepair: %v", err)
	}
	if rep.Id != 14 || rep.LegId != 15 || rep.SideId != 16 ||
		rep.SliceId != 1 || rep.Addr != dnAddr(5) {
		t.Errorf("PutRepair = %+v, want ids 14, 15 and 16 on dn 5", rep)
	}
	c.dnHolds(t, ctx, 5, nodeFree-1, nodeFree, []*pb.SidePointer{
		{SpId: ref.Id, LegId: 15, SideId: 16},
	}, rep.DnRevision)

	state := c.load(t, ctx)
	gotRetired := state.RetiredLegs[12]
	if gotRetired.GetGrpId() != 2 || gotRetired.GetLegIdx() != 1 ||
		gotRetired.GetRetiredEpoch() != 5000 ||
		gotRetired.GetSide().GetAddrPort() != dnAddr(4) ||
		!gotRetired.GetSide().GetProvisioned() {
		t.Errorf("retired leg 12 = %v", gotRetired)
	}
	gotRep := state.Repairs["rep0"]
	if gotRep.GetRepId() != 14 ||
		gotRep.GetRepStage() != pb.RepStage_REP_STAGE_CREATED ||
		len(gotRep.GetNewLegList()) != 1 ||
		gotRep.GetNewLegList()[0].GetLegIdx() != 2 ||
		gotRep.GetNewLegList()[0].GetSideList()[0].GetProvisioned() ||
		!equalU64(gotRep.GetBadLegIdList(), 4) {
		t.Errorf("repair rep0 = %v", gotRep)
	}
	grp := state.Slices[1].GetMetaGrpList()[0]
	if grp.GetRepName() != "rep0" || !equalU64(grp.GetRetiredLegIdList(), 12) {
		t.Errorf("group 2 = %v", grp)
	}
	for _, id := range []uint64{4, 5} {
		if state.DnByAddr[dnAddr(id)] == nil {
			t.Errorf("the load reads no record of dn %d", id)
		}
	}

	// The ids the records hold are claimed, a group holds one repair, and a
	// name is one repair's.
	for _, tc := range []struct {
		name string
		put  func() error
		want string
	}{
		{"a side id the repair holds", func() error {
			_, err := PutRetiredLeg(ctx, c.cli, c.cl, ref, RetiredLegSpec{
				GrpId: 5, SideId: 16, Dn: 4,
			})
			return err
		}, "side id 0x10 is already used"},
		{"a leg id the retired leg holds", func() error {
			_, err := PutRetiredLeg(ctx, c.cli, c.cl, ref, RetiredLegSpec{
				GrpId: 5, LegId: 12, Dn: 4,
			})
			return err
		}, "leg id 0xc is already used"},
		{"a second repair of the group", func() error {
			_, err := PutRepair(ctx, c.cli, c.cl, ref, RepairSpec{
				Name: "rep1", GrpId: 2, BadLegId: 3, Dn: 4,
			})
			return err
		}, "has the repair"},
		{"a name in use", func() error {
			_, err := PutRepair(ctx, c.cli, c.cl, ref, RepairSpec{
				Name: "rep0", GrpId: 5, BadLegId: 6, Dn: 4,
			})
			return err
		}, "already exists"},
		{"a bad leg the group does not hold", func() error {
			_, err := PutRepair(ctx, c.cli, c.cl, ref, RepairSpec{
				Name: "rep1", GrpId: 5, BadLegId: 3, Dn: 4,
			})
			return err
		}, "no leg 0x3 in its leg_list"},
	} {
		err := tc.put()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error with %q", tc.name, err, tc.want)
		}
	}

	if old, err := SetRepairLegEpoch(ctx, c.cli, c.cl, ref, "rep0", 15,
		7); err != nil || old != 0 {
		t.Errorf("SetRepairLegEpoch = %d, %v", old, err)
	}
	if old, err := SetRepairSideEpoch(ctx, c.cli, c.cl, ref, "rep0", 16,
		8); err != nil || old != 0 {
		t.Errorf("SetRepairSideEpoch = %d, %v", old, err)
	}
	if old, err := SetRetiredSideEpoch(ctx, c.cli, c.cl, ref, 12,
		9); err != nil || old != 0 {
		t.Errorf("SetRetiredSideEpoch = %d, %v", old, err)
	}
	if _, err := SetRepairSideEpoch(ctx, c.cli, c.cl, ref, "rep0", 13,
		9); err == nil {
		t.Errorf("SetRepairSideEpoch of a side the repair does not hold " +
			"succeeded")
	}
	flipped, spRev, err := SetRepairProvisioned(ctx, c.cli, c.cl, ref,
		"rep0", 15, 16)
	if err != nil || !flipped || spRev != c.spRev(t, ctx) {
		t.Errorf("SetRepairProvisioned = %v, %d, %v", flipped, spRev, err)
	}
	if again, _, err := SetRepairProvisioned(ctx, c.cli, c.cl, ref, "rep0",
		15, 16); err != nil || again {
		t.Errorf("a second SetRepairProvisioned = %v, %v", again, err)
	}
	repId, _, err := SetRepairAbort(ctx, c.cli, c.cl, ref, "rep0")
	if err != nil || repId != 14 {
		t.Errorf("SetRepairAbort = %d, %v", repId, err)
	}
	repId, spRev, err = SetRepairStage(ctx, c.cli, c.cl, ref, "rep0",
		pb.RepStage_REP_STAGE_ABORTED)
	if err != nil || repId != 14 || spRev != c.spRev(t, ctx) {
		t.Errorf("SetRepairStage = %d, %d, %v", repId, spRev, err)
	}
	state = c.load(t, ctx)
	gotRep = state.Repairs["rep0"]
	newLeg := gotRep.GetNewLegList()[0]
	if gotRep.GetRepStage() != pb.RepStage_REP_STAGE_ABORTED ||
		!gotRep.GetTryToAbort() || newLeg.GetErrEpoch() != 7 ||
		newLeg.GetSideList()[0].GetErrEpoch() != 8 ||
		!newLeg.GetSideList()[0].GetProvisioned() {
		t.Errorf("repair rep0 after the setters = %v", gotRep)
	}
	if got := state.RetiredLegs[12].GetSide().GetErrEpoch(); got != 9 {
		t.Errorf("retired side epoch %d, want 9", got)
	}
	mustGet(t, ctx, c.cli, model.RepairKey(cid, ref.Id, "rep0"), &pb.Repair{})
}

// TestPutRepairFromARetiredLeg plants a repair whose new leg is a retired
// leg of the group: the leg keeps its ids and its side, which stays charged,
// its record and list entries go, and the repair keeps its retired_epoch. A
// retired leg of another group is refused.
func TestPutRepairFromARetiredLeg(t *testing.T) {
	ctx := testCtx(t)
	c := plantSmallCluster(t, ctx, true)
	cid, ref := c.cl.Id, c.sp.Ref
	other, err := PutRetiredLeg(ctx, c.cli, c.cl, ref, RetiredLegSpec{
		GrpId: 5, LegIdx: 1, Dn: 5, Epoch: 4000,
	})
	if err != nil {
		t.Fatalf("PutRetiredLeg of group 5: %v", err)
	}
	retired, err := PutRetiredLeg(ctx, c.cli, c.cl, ref, RetiredLegSpec{
		GrpId: 2, LegIdx: 1, Dn: 4, Epoch: 5000,
	})
	if err != nil {
		t.Fatalf("PutRetiredLeg of group 2: %v", err)
	}
	if _, err := PutRepair(ctx, c.cli, c.cl, ref, RepairSpec{
		Name: "rep0", GrpId: 2, BadLegId: 4, RetiredLegId: other.LegId,
	}); err == nil || !strings.Contains(err.Error(), "is not group") {
		t.Errorf("a repair from another group's retired leg: %v", err)
	}
	dnRev := c.dnRev(t, ctx, 4)
	rep, err := PutRepair(ctx, c.cli, c.cl, ref, RepairSpec{
		Name: "rep0", GrpId: 2, BadLegId: 4, RetiredLegId: retired.LegId,
	})
	if err != nil {
		t.Fatalf("PutRepair: %v", err)
	}
	if rep.LegId != retired.LegId || rep.SideId != retired.SideId ||
		rep.Addr != dnAddr(4) || rep.DnRevision != 0 {
		t.Errorf("PutRepair = %+v, want the retired leg's ids on dn 4 and "+
			"no charge", rep)
	}
	mustAbsent(t, ctx, c.cli, model.RetiredLegKey(cid, ref.Id, retired.LegId),
		&pb.RetiredLeg{})
	state := c.load(t, ctx)
	if got := state.Conf.GetRetiredLegIdList(); !equalU64(got, other.LegId) {
		t.Errorf("the pool's retired legs: %v", got)
	}
	if got := state.Slices[1].GetMetaGrpList()[0].GetRetiredLegIdList(); len(got) != 0 {
		t.Errorf("group 2's retired legs: %v", got)
	}
	gotRep := state.Repairs["rep0"]
	if gotRep.GetNewLegIdToRetiredEpoch()[retired.LegId] != 5000 ||
		gotRep.GetNewLegList()[0].GetLegIdx() != 1 {
		t.Errorf("repair rep0 = %v", gotRep)
	}
	c.dnHolds(t, ctx, 4, nodeFree-1, nodeFree-1, []*pb.SidePointer{
		{SpId: ref.Id, LegId: retired.LegId, SideId: retired.SideId},
	}, dnRev)
}

// TestDrainsGiveBackRepairsAndRetiredLegs plants a repair and a retired leg
// on a slice's group and on a clone's, drains the clone and then the pool
// through DrainClone and DrainSp, and checks that every DN got back every
// extent and pointer the records took.
func TestDrainsGiveBackRepairsAndRetiredLegs(t *testing.T) {
	ctx := testCtx(t)
	c := plantSmallCluster(t, ctx, true)
	cid, ref := c.cl.Id, c.sp.Ref
	if _, err := PutRetiredLeg(ctx, c.cli, c.cl, ref, RetiredLegSpec{
		GrpId: 2, LegIdx: 1, Dn: 4, Epoch: 5000,
	}); err != nil {
		t.Fatalf("PutRetiredLeg: %v", err)
	}
	if _, err := PutRepair(ctx, c.cli, c.cl, ref, RepairSpec{
		Name: "rep0", GrpId: 2, BadLegId: 4, Dn: 5,
	}); err != nil {
		t.Fatalf("PutRepair: %v", err)
	}
	if _, err := PutTd(ctx, c.cli, c.cl, ref,
		TdSpec{Name: "td0", Size: 1 << 30}); err != nil {
		t.Fatalf("PutTd: %v", err)
	}
	clone, err := PutClone(ctx, c.cli, c.cl, ref, CloneSpec{
		Name: "c0", DstTd: "td0",
		Legs: []CloneLegSpec{{LegIdx: 0, DnId: 1}, {LegIdx: 1, DnId: 3}},
	})
	if err != nil {
		t.Fatalf("PutClone: %v", err)
	}
	cloneRetired, err := PutRetiredLeg(ctx, c.cli, c.cl, ref, RetiredLegSpec{
		GrpId: clone.GrpId, LegIdx: 1, Dn: 4, Epoch: 5000,
	})
	if err != nil {
		t.Fatalf("PutRetiredLeg of the clone's group: %v", err)
	}
	if cloneRetired.CloneName != "c0" || cloneRetired.SliceId != 0 {
		t.Errorf("the clone's retired leg = %+v", cloneRetired)
	}
	if _, err := PutRepair(ctx, c.cli, c.cl, ref, RepairSpec{
		Name: "rep1", GrpId: clone.GrpId, BadLegId: clone.LegIds[1], Dn: 5,
	}); err != nil {
		t.Fatalf("PutRepair of the clone's group: %v", err)
	}
	if _, err := SetCloneDeleting(ctx, c.cli, c.cl, ref, "c0"); err != nil {
		t.Fatalf("SetCloneDeleting: %v", err)
	}
	done, err := DrainClone(ctx, c.cli, c.cl, ref, "c0", 4096)
	if err != nil || !done.Deleted || done.GrpSideCnt != 4 {
		t.Fatalf("DrainClone = %+v, %v, want the group's four sides back",
			done, err)
	}
	mustAbsent(t, ctx, c.cli, model.RepairKey(cid, ref.Id, "rep1"),
		&pb.Repair{})
	mustAbsent(t, ctx, c.cli,
		model.RetiredLegKey(cid, ref.Id, cloneRetired.LegId),
		&pb.RetiredLeg{})

	if _, err := SetDeleting(ctx, c.cli, c.cl, ref); err != nil {
		t.Fatalf("SetDeleting: %v", err)
	}
	drain, err := DrainSp(ctx, c.cli, c.cl, ref, 4096)
	if err != nil || !drain.Deleted || drain.RepairCnt != 1 ||
		drain.RetiredLegCnt != 1 {
		t.Fatalf("DrainSp = %+v, %v, want one repair and one retired leg",
			drain, err)
	}
	for dnId := uint64(1); dnId <= 5; dnId++ {
		dn := c.dnConf(t, ctx, dnId)
		if dn.GetFreeExtCnt() != nodeFree || len(dn.GetSidePtrList()) != 0 {
			t.Errorf("dn %d after the drains = %v, want %d free and no side",
				dnId, dn, nodeFree)
		}
	}
}
