package model

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The forced start's operations on the MD6 fixture: the mark ForceStartGroup
// writes, replaces and withdraws on the data group, the sp worker's clear
// (ClearForcedStart, dnv-worker.md AR13), FinishRepair's clear of a mark
// that names the bad leg it retires, SetRepairAbort's refusal while a mark
// names the repair's new leg, and the clone meta group's twin of each
// through its parent. Every refusal
// and every skip is checked to have written nothing at all, by the store's
// revision.

// forceStart is ForceStartGroup on a group of the fixture slice with no
// expected revision.
func (e *opsEnv) forceStart(grpId, legId uint64, cancel bool) (uint64, error) {
	e.t.Helper()
	return ForceStartGroup(e.ctx, e.cli, e.cid, opsShard, opsSpId,
		opsSpName, noExpectRev, opsSliceParent, grpId, legId, cancel)
}

// mustForceStart is forceStart failing the test on an error.
func (e *opsEnv) mustForceStart(grpId, legId uint64) uint64 {
	e.t.Helper()
	revision, err := e.forceStart(grpId, legId, false)
	if err != nil {
		e.t.Fatalf("ForceStartGroup group %d leg %d: %v", grpId, legId, err)
	}
	return revision
}

// mark is the forced-start mark of a group of the fixture slice, nil when
// none stands.
func (e *opsEnv) mark(grpId uint64) *pb.ForcedStart {
	e.t.Helper()
	return findGroup(e.slice(), grpId).GetForcedStart()
}

// clearRef is the ref the sp worker hands ClearForcedStart for the mark that
// stands on a group of the fixture slice now.
func (e *opsEnv) clearRef(grpId uint64) ForcedStartRef {
	e.t.Helper()
	mark := e.mark(grpId)
	return ForcedStartRef{
		Parent:   opsSliceParent,
		GrpId:    grpId,
		LegId:    mark.GetLegId(),
		Revision: mark.GetRevision(),
	}
}

// clearMark is ClearForcedStart of ref on the fixture SP.
func (e *opsEnv) clearMark(ref ForcedStartRef) (bool, uint64, error) {
	e.t.Helper()
	return ClearForcedStart(e.ctx, e.cli, e.cid, opsShard, opsSpId,
		opsSpName, ref)
}

// TestForceStartGroup: the write puts the mark on the group with the named
// leg and the SpRev revision its own transaction commits, which it returns,
// in one bump and with nothing else of the slice changed; a second call
// replaces the mark, a call on the same leg included, each with a revision of
// its own; the cancel withdraws it in one bump and returns zero; and a cancel
// of a group with no mark writes nothing at all.
func TestForceStartGroup(t *testing.T) {
	env := newOpsEnv(t)
	want := env.slice()
	revBefore := env.spRev()
	revision := env.mustForceStart(opsDataGrpId, opsDataLegA)
	if revision != revBefore+1 || env.spRev() != revision {
		t.Fatalf("ForceStartGroup = revision %d, SpRev %d, want both %d",
			revision, env.spRev(), revBefore+1)
	}
	findGroup(want, opsDataGrpId).ForcedStart = &pb.ForcedStart{
		LegId: opsDataLegA, Revision: revision,
	}
	if got := env.slice(); !proto.Equal(got, want) {
		t.Errorf("slice: got %v, want %v", got, want)
	}
	if mark := env.mark(opsMetaGrpId); mark != nil {
		t.Errorf("the meta group carries a mark: %v", mark)
	}
	for _, legId := range []uint64{opsDataLegB, opsDataLegB} {
		before := env.spRev()
		got := env.mustForceStart(opsDataGrpId, legId)
		mark := env.mark(opsDataGrpId)
		if got != before+1 || mark.GetLegId() != legId ||
			mark.GetRevision() != got {
			t.Errorf("the replace with leg %d: revision %d, mark %v, want "+
				"leg %d at %d", legId, got, mark, legId, before+1)
		}
	}
	before := env.spRev()
	got, err := env.forceStart(opsDataGrpId, 0, true)
	if err != nil || got != 0 {
		t.Fatalf("the cancel = %d, %v, want 0, nil", got, err)
	}
	if mark := env.mark(opsDataGrpId); mark != nil {
		t.Errorf("the cancel left the mark %v", mark)
	}
	if env.spRev() != before+1 {
		t.Errorf("SpRev after the cancel: got %d, want %d",
			env.spRev(), before+1)
	}
	storeBefore := env.storeRev()
	got, err = env.forceStart(opsDataGrpId, 0, true)
	if err != nil || got != 0 {
		t.Fatalf("a cancel with no mark = %d, %v, want 0, nil", got, err)
	}
	if env.storeRev() != storeBefore {
		t.Errorf("a cancel with no mark wrote: store revision %d, want %d",
			env.storeRev(), storeBefore)
	}
}

// TestForceStartGroupTakesTheToken: a matching expected revision writes the
// mark, and the cancel takes one too; a stale one is refused first, on a
// write and on a cancel of a mark that stands alike.
func TestForceStartGroupTakesTheToken(t *testing.T) {
	env := newOpsEnv(t)
	if _, err := ForceStartGroup(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, env.spRev(), opsSliceParent, opsDataGrpId,
		opsDataLegA, false); err != nil {
		t.Fatalf("ForceStartGroup with the token: %v", err)
	}
	for _, cancel := range []bool{false, true} {
		before := env.storeRev()
		_, err := ForceStartGroup(env.ctx, env.cli, env.cid, opsShard,
			opsSpId, opsSpName, env.spRev()+9, opsSliceParent, opsDataGrpId,
			opsDataLegB, cancel)
		env.wantNoWrite(err, opForceStartGroup, ReasonStaleRevision, before)
	}
	if _, err := ForceStartGroup(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, env.spRev(), opsSliceParent, opsDataGrpId, 0,
		true); err != nil || env.mark(opsDataGrpId) != nil {
		t.Errorf("the cancel with the token: %v, mark %v", err,
			env.mark(opsDataGrpId))
	}
}

// TestForceStartGroupNamesARepairsNewLeg: the mark may name the new leg of
// the group's open repair while its abort flag is clear, and not once the
// flag is set on the CREATED repair, which takes the leg out of the members
// the forced start keeps; the abort itself waits for the mark's cancel
// (TestSetRepairAbortRefusedWhileAMarkNamesTheNewLeg). A STARTED repair's
// new leg may be named whatever its flag says (keepsNewLegs), and an
// ABORTED repair's may not.
func TestForceStartGroupNamesARepairsNewLeg(t *testing.T) {
	env := newOpsEnv(t)
	_, newLegId := env.createRepair(func() RepairNewLeg {
		return env.freshLeg(opsDnC)
	})
	revision := env.mustForceStart(opsDataGrpId, newLegId)
	if mark := env.mark(opsDataGrpId); mark.GetLegId() != newLegId ||
		mark.GetRevision() != revision {
		t.Errorf("the mark: got %v, want leg %d at %d", mark, newLegId,
			revision)
	}
	if _, err := env.forceStart(opsDataGrpId, 0, true); err != nil {
		t.Fatalf("the cancel: %v", err)
	}
	if _, err := SetRepairAbort(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, noExpectRev, opsRepName); err != nil {
		t.Fatalf("SetRepairAbort: %v", err)
	}
	before := env.storeRev()
	_, err := env.forceStart(opsDataGrpId, newLegId, false)
	env.wantNoWrite(err, opForceStartGroup, ReasonLegNotNamable, before)

	rep := env.repair(opsRepName)
	rep.RepStage = pb.RepStage_REP_STAGE_STARTED
	mustPut(env.t, env.cli, RepairKey(env.cid, opsSpId, opsRepName), rep)
	if mark := env.mark(opsDataGrpId); mark != nil {
		t.Fatalf("a mark stands before the started repair's: %v", mark)
	}
	revision = env.mustForceStart(opsDataGrpId, newLegId)
	if mark := env.mark(opsDataGrpId); mark.GetLegId() != newLegId ||
		mark.GetRevision() != revision {
		t.Errorf("the started repair's mark: got %v, want leg %d at %d",
			mark, newLegId, revision)
	}

	rep.RepStage = pb.RepStage_REP_STAGE_ABORTED
	mustPut(env.t, env.cli, RepairKey(env.cid, opsSpId, opsRepName), rep)
	before = env.storeRev()
	_, err = env.forceStart(opsDataGrpId, newLegId, false)
	env.wantNoWrite(err, opForceStartGroup, ReasonLegNotNamable, before)
}

// TestForceStartGroupPreconditions is every refusal of ForceStartGroup's
// write, each with nothing written: the call names leg A of the data group,
// and the case's setup changes the store or the call.
func TestForceStartGroupPreconditions(t *testing.T) {
	type call struct {
		expectRev uint64
		parent    GrpParent
		grpId     uint64
		legId     uint64
	}
	// withRepair gives the data group an open repair of leg B at stage and
	// names legId: a leg outside the leg_list is namable only as a new leg
	// of that repair.
	withRepair := func(stage pb.RepStage, legId uint64) func(*opsEnv, *call) {
		return func(env *opsEnv, c *call) {
			repId, _ := env.createRepair(func() RepairNewLeg {
				return env.freshLeg(opsDnC)
			})
			if stage != pb.RepStage_REP_STAGE_CREATED {
				env.toStage(opsRepName, repId, stage)
			}
			c.legId = legId
		}
	}
	for _, tc := range []struct {
		name   string
		setup  func(env *opsEnv, c *call)
		reason string
	}{
		{"stale revision", func(_ *opsEnv, c *call) {
			c.expectRev = 99
		}, ReasonStaleRevision},
		{"sp deleting", func(env *opsEnv, _ *call) {
			env.setDeleting()
		}, "sp deleting"},
		{"SP_LEVEL_NO_REDUND", func(env *opsEnv, _ *call) {
			env.setLevel(pb.SpLevel_SP_LEVEL_NO_REDUND)
		}, reasonRepairLevelNoArray},
		{"SP_LEVEL_DISABLE", func(env *opsEnv, _ *call) {
			env.setLevel(pb.SpLevel_SP_LEVEL_DISABLE)
		}, reasonRepairLevelNoArray},
		{"group is not md-raid1", func(env *opsEnv, _ *call) {
			conf := env.spConf()
			conf.BdevConf.RedundConf = &pb.RedundConf{
				RedunKind: &pb.RedundConf_RedundNone{
					RedundNone: &pb.RedundNone{},
				},
			}
			mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
		}, "group is not md-raid1"},
		{"slice not found", func(_ *opsEnv, c *call) {
			c.parent = GrpParent{SliceId: 9999}
		}, "slice not found"},
		{"no single parent", func(_ *opsEnv, c *call) {
			c.parent = GrpParent{}
		}, "no single parent"},
		{"group not found", func(_ *opsEnv, c *call) {
			c.grpId = 9999
		}, "group not found"},
		{"clone deleting", func(env *opsEnv, c *call) {
			env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(true), true)
			c.parent = GrpParent{CloneName: opsCloneName}
			c.grpId = opsCloneGrpId
			c.legId = opsCloneLegA
		}, "clone deleting"},
		{"a leg the group does not hold", func(_ *opsEnv, c *call) {
			c.legId = 9999
		}, ReasonLegNotNamable},
		{"a leg of another group", func(_ *opsEnv, c *call) {
			c.legId = opsMetaLegA
		}, ReasonLegNotNamable},
		{"a leg the group does not hold, with a CREATED repair",
			withRepair(pb.RepStage_REP_STAGE_CREATED, 9999),
			ReasonLegNotNamable},
		{"a leg of another group, with a CREATED repair",
			withRepair(pb.RepStage_REP_STAGE_CREATED, opsMetaLegA),
			ReasonLegNotNamable},
		{"a leg the group does not hold, with a STARTED repair",
			withRepair(pb.RepStage_REP_STAGE_STARTED, 9999),
			ReasonLegNotNamable},
		{"a leg of another group, with a STARTED repair",
			withRepair(pb.RepStage_REP_STAGE_STARTED, opsMetaLegA),
			ReasonLegNotNamable},
		// Leg B is the bad leg of a STARTED repair: it left the array
		// before md copied onto the new leg, and the finish retires it.
		{"the bad leg of a STARTED repair", func(env *opsEnv, c *call) {
			repId, _ := env.createRepair(func() RepairNewLeg {
				return env.freshLeg(opsDnC)
			})
			env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_STARTED)
			c.legId = opsDataLegB
		}, ReasonLegNotNamable},
		{"a group that names a repair the pool does not list",
			func(env *opsEnv, c *call) {
				slice := env.slice()
				findGroup(slice, opsDataGrpId).RepName = "other"
				mustPut(env.t, env.cli,
					SliceKey(env.cid, opsSpId, opsSliceId), slice)
				c.legId = 9999
			}, ReasonRepairNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			c := &call{
				parent: opsSliceParent,
				grpId:  opsDataGrpId,
				legId:  opsDataLegA,
			}
			tc.setup(env, c)
			before := env.storeRev()
			_, err := ForceStartGroup(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, c.expectRev, c.parent, c.grpId, c.legId,
				false)
			env.wantNoWrite(err, opForceStartGroup, tc.reason, before)
		})
	}
}

// TestForceStartGroupReadsTheGroupsRepair: a mark of a group with a repair
// reads the repair, whichever leg it names — a new leg is looked up there,
// and a leg of the leg_list is checked against the bad legs of a STARTED
// repair — so a group whose repair the pool lists but whose record is gone
// is refused as loadRepair refuses it, for both. The bad leg of a CREATED
// repair, and the group's other leg, are written.
func TestForceStartGroupReadsTheGroupsRepair(t *testing.T) {
	env := newOpsEnv(t)
	_, newLegId := env.createRepair(func() RepairNewLeg {
		return env.freshLeg(opsDnC)
	})
	env.mustForceStart(opsDataGrpId, opsDataLegB)
	env.mustForceStart(opsDataGrpId, opsDataLegA)
	env.delKey(RepairKey(env.cid, opsSpId, opsRepName))
	for _, legId := range []uint64{newLegId, opsDataLegA} {
		before := env.storeRev()
		_, err := env.forceStart(opsDataGrpId, legId, false)
		env.wantNoWrite(err, opForceStartGroup, "repair record missing",
			before)
	}
}

// TestForceStartGroupLevels: the write takes every level below
// SP_LEVEL_NO_REDUND, the levels where reactions stop among them, and the
// cancel takes every level, so a mark can be withdrawn where it cannot be
// written.
func TestForceStartGroupLevels(t *testing.T) {
	env := newOpsEnv(t)
	for _, level := range []pb.SpLevel{
		pb.SpLevel_SP_LEVEL_READWRITE, pb.SpLevel_SP_LEVEL_READONLY,
		pb.SpLevel_SP_LEVEL_NO_CLONE, pb.SpLevel_SP_LEVEL_NO_THINPOOL,
	} {
		env.setLevel(level)
		if _, err := env.forceStart(opsDataGrpId, opsDataLegA,
			false); err != nil {
			t.Errorf("ForceStartGroup at %v: %v", level, err)
		}
	}
	for _, level := range []pb.SpLevel{
		pb.SpLevel_SP_LEVEL_NO_REDUND, pb.SpLevel_SP_LEVEL_NO_SIDE,
		pb.SpLevel_SP_LEVEL_DISABLE,
	} {
		env.mustForceStart(opsMetaGrpId, opsMetaLegA)
		env.setLevel(level)
		if _, err := env.forceStart(opsMetaGrpId, 0, true); err != nil ||
			env.mark(opsMetaGrpId) != nil {
			t.Errorf("the cancel at %v: %v, mark %v", level, err,
				env.mark(opsMetaGrpId))
		}
		env.setLevel(pb.SpLevel_SP_LEVEL_READWRITE)
	}
}

// TestForceStartGroupCancelPreconditions: the cancel makes the checks the
// write makes but the level's and the leg's, so a RedundNone pool, a deleting
// pool and a deleting clone refuse it too, with nothing written.
func TestForceStartGroupCancelPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(env *opsEnv) (GrpParent, uint64)
		reason string
	}{
		{"group is not md-raid1", func(env *opsEnv) (GrpParent, uint64) {
			conf := env.spConf()
			conf.BdevConf.RedundConf = &pb.RedundConf{
				RedunKind: &pb.RedundConf_RedundNone{
					RedundNone: &pb.RedundNone{},
				},
			}
			mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
			return opsSliceParent, opsDataGrpId
		}, "group is not md-raid1"},
		{"sp deleting", func(env *opsEnv) (GrpParent, uint64) {
			env.setDeleting()
			return opsSliceParent, opsDataGrpId
		}, "sp deleting"},
		{"group not found", func(env *opsEnv) (GrpParent, uint64) {
			return opsSliceParent, 9999
		}, "group not found"},
		{"clone deleting", func(env *opsEnv) (GrpParent, uint64) {
			grp := opsCloneGrp(true)
			grp.ForcedStart = &pb.ForcedStart{
				LegId: opsCloneLegA, Revision: 1,
			}
			env.putGrpClone(opsCloneName, opsCloneId, grp, true)
			return GrpParent{CloneName: opsCloneName}, opsCloneGrpId
		}, "clone deleting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			env.mustForceStart(opsDataGrpId, opsDataLegA)
			parent, grpId := tc.setup(env)
			before := env.storeRev()
			_, err := ForceStartGroup(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, noExpectRev, parent, grpId, 0, true)
			env.wantNoWrite(err, opForceStartGroup, tc.reason, before)
		})
	}
}

// TestClearForcedStart: the clear of the mark that stands withdraws it in one
// SpRev bump and returns the revision it committed, at every level, those
// where no group array is built included; a ref the store no longer answers
// — a mark replaced, withdrawn, or naming another leg or revision, a group
// or a parent that is gone — is skipped with nothing written and no error;
// and a pool being deleted or recreated under another id is refused.
func TestClearForcedStart(t *testing.T) {
	env := newOpsEnv(t)
	env.mustForceStart(opsDataGrpId, opsDataLegA)
	env.setLevel(pb.SpLevel_SP_LEVEL_DISABLE)
	ref := env.clearRef(opsDataGrpId)
	revBefore := env.spRev()
	cleared, revision, err := env.clearMark(ref)
	if err != nil || !cleared || revision != revBefore+1 ||
		env.spRev() != revision {
		t.Fatalf("ClearForcedStart = %v, %d, %v, SpRev %d, want a clear "+
			"at %d", cleared, revision, err, env.spRev(), revBefore+1)
	}
	if mark := env.mark(opsDataGrpId); mark != nil {
		t.Errorf("the clear left the mark %v", mark)
	}
	env.setLevel(pb.SpLevel_SP_LEVEL_READWRITE)
	env.mustForceStart(opsDataGrpId, opsDataLegA)
	stale := env.clearRef(opsDataGrpId)
	env.mustForceStart(opsDataGrpId, opsDataLegB)
	live := env.clearRef(opsDataGrpId)
	otherLeg, otherRev, goneGrp, goneSlice := live, live, live, live
	otherLeg.LegId = opsDataLegA
	otherRev.Revision = live.Revision + 1
	goneGrp.GrpId = 9999
	goneSlice.Parent = GrpParent{SliceId: 9999}
	noMark := live
	noMark.GrpId = opsMetaGrpId
	for name, r := range map[string]ForcedStartRef{
		"a replaced mark":      stale,
		"another leg":          otherLeg,
		"another revision":     otherRev,
		"a group that is gone": goneGrp,
		"a slice that is gone": goneSlice,
		"a group with no mark": noMark,
		"a ref with no parent": {GrpId: opsDataGrpId, LegId: live.LegId},
		"a ref with no group":  {Parent: opsSliceParent},
	} {
		before := env.storeRev()
		cleared, revision, err := env.clearMark(r)
		if err != nil || cleared || revision != 0 {
			t.Errorf("%s: ClearForcedStart = %v, %d, %v, want a skip",
				name, cleared, revision, err)
		}
		if got := env.storeRev(); got != before {
			t.Errorf("%s: a skipped clear wrote", name)
		}
	}
	if mark := env.mark(opsDataGrpId); mark.GetLegId() != opsDataLegB {
		t.Errorf("the skips touched the live mark: %v", mark)
	}
	env.setDeleting()
	before := env.storeRev()
	_, _, err = env.clearMark(live)
	env.wantNoWrite(err, opClearForcedStart, "sp deleting", before)
}

// TestClearForcedStartRefusesAnotherPool: a pool recreated under the name
// with another sp id is refused, as every repair operation refuses it.
func TestClearForcedStartRefusesAnotherPool(t *testing.T) {
	env := newOpsEnv(t)
	env.mustForceStart(opsDataGrpId, opsDataLegA)
	ref := env.clearRef(opsDataGrpId)
	conf := env.spConf()
	conf.SpId = opsSpId + 1
	mustPut(t, env.cli, SpConfKey(env.cid, opsSpName), conf)
	before := env.storeRev()
	_, _, err := env.clearMark(ref)
	env.wantNoWrite(err, opClearForcedStart, "sp id changed", before)
}

// TestSetRepairAbortRefusedWhileAMarkNamesTheNewLeg: the abort flag would
// take the repair's new leg out of the members the forced start keeps, so
// the abort is refused, with nothing written, while the group's mark names
// that leg; once the mark is withdrawn the abort goes through in one SpRev
// bump, and a mark that names a leg of the leg_list neither holds the abort
// back nor is touched by it.
func TestSetRepairAbortRefusedWhileAMarkNamesTheNewLeg(t *testing.T) {
	env := newOpsEnv(t)
	_, newLegId := env.createRepair(func() RepairNewLeg {
		return env.freshLeg(opsDnC)
	})
	revision := env.mustForceStart(opsDataGrpId, newLegId)
	before := env.storeRev()
	_, err := SetRepairAbort(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, noExpectRev, opsRepName)
	env.wantNoWrite(err, opSetRepairAbort, reasonMarkNamesNewLeg, before)
	if mark := env.mark(opsDataGrpId); mark.GetLegId() != newLegId ||
		mark.GetRevision() != revision {
		t.Errorf("the refused abort touched the mark: %v", mark)
	}
	if env.repair(opsRepName).GetTryToAbort() {
		t.Errorf("the refused abort set the flag")
	}
	if _, err := env.forceStart(opsDataGrpId, 0, true); err != nil {
		t.Fatalf("the cancel: %v", err)
	}
	spRev := env.spRev()
	if _, err := SetRepairAbort(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, noExpectRev, opsRepName); err != nil {
		t.Fatalf("SetRepairAbort after the cancel: %v", err)
	}
	if !env.repair(opsRepName).GetTryToAbort() || env.spRev() != spRev+1 {
		t.Errorf("the abort: flag %v, SpRev %d, want set at %d",
			env.repair(opsRepName).GetTryToAbort(), env.spRev(), spRev+1)
	}

	env = newOpsEnv(t)
	env.createRepair(func() RepairNewLeg {
		return env.freshLeg(opsDnC)
	})
	revision = env.mustForceStart(opsDataGrpId, opsDataLegA)
	if _, err := SetRepairAbort(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, noExpectRev, opsRepName); err != nil {
		t.Fatalf("SetRepairAbort: %v", err)
	}
	if mark := env.mark(opsDataGrpId); mark.GetLegId() != opsDataLegA ||
		mark.GetRevision() != revision {
		t.Errorf("the abort touched a mark of the leg_list: %v", mark)
	}
}

// TestFinishRepairClearsAMarkNamingTheBadLeg: the finish retires the bad
// leg, so it withdraws a mark that names it in its own transaction; a mark
// that names the new leg stays, the leg keeping its id in the bad leg's
// place. Each mark is written while the repair is CREATED, as the bad leg
// of a STARTED repair may not be named.
func TestFinishRepairClearsAMarkNamingTheBadLeg(t *testing.T) {
	for _, tc := range []struct {
		name     string
		markLeg  func(newLegId uint64) uint64
		wantKept bool
	}{
		{"a mark of the bad leg", func(uint64) uint64 {
			return opsDataLegB
		}, false},
		{"a mark of the new leg", func(newLegId uint64) uint64 {
			return newLegId
		}, true},
		{"a mark of the leg that stays", func(uint64) uint64 {
			return opsDataLegA
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			repId, newLegId := env.createRepair(func() RepairNewLeg {
				return env.freshLeg(opsDnC)
			})
			legId := tc.markLeg(newLegId)
			revision := env.mustForceStart(opsDataGrpId, legId)
			env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_STARTED)
			if _, err := FinishRepair(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, noExpectRev, opsRepName, opsFinishNow,
				env.cc); err != nil {
				t.Fatalf("FinishRepair: %v", err)
			}
			mark := env.mark(opsDataGrpId)
			if !tc.wantKept {
				if mark != nil {
					t.Errorf("the finish left the mark %v", mark)
				}
				return
			}
			if mark.GetLegId() != legId || mark.GetRevision() != revision {
				t.Errorf("the mark after the finish: got %v, want leg %d "+
					"at %d", mark, legId, revision)
			}
		})
	}
}

// TestForcedStartOfACloneMetaGroup is the forced start through a clone
// parent: the mark is written on the clone's record where a slice's group
// would have its slice's, the worker's clear withdraws it there, and a
// deleting clone's group is refused by the write and skipped by the clear.
func TestForcedStartOfACloneMetaGroup(t *testing.T) {
	env := newOpsEnv(t)
	env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(true), false)
	parent := GrpParent{CloneName: opsCloneName}
	slice := env.slice()
	revision, err := ForceStartGroup(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, noExpectRev, parent, opsCloneGrpId,
		opsCloneLegB, false)
	if err != nil {
		t.Fatalf("ForceStartGroup of the clone's group: %v", err)
	}
	mark := env.clone(opsCloneName).GetMetaGrp().GetForcedStart()
	if mark.GetLegId() != opsCloneLegB || mark.GetRevision() != revision {
		t.Errorf("the clone's mark: got %v, want leg %d at %d", mark,
			opsCloneLegB, revision)
	}
	if got := env.slice(); !proto.Equal(got, slice) {
		t.Errorf("the clone's mark wrote the slice: %v", got)
	}
	ref := ForcedStartRef{
		Parent: parent, GrpId: opsCloneGrpId,
		LegId: opsCloneLegB, Revision: revision,
	}
	clone := env.clone(opsCloneName)
	clone.Deleting = true
	mustPut(t, env.cli, CloneKey(env.cid, opsSpId, opsCloneName), clone)
	before := env.storeRev()
	cleared, _, err := env.clearMark(ref)
	if err != nil || cleared || env.storeRev() != before {
		t.Errorf("a deleting clone's clear: %v, %v, want a skip", cleared,
			err)
	}
	_, err = ForceStartGroup(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, noExpectRev, parent, opsCloneGrpId, opsCloneLegA, false)
	env.wantNoWrite(err, opForceStartGroup, "clone deleting", before)
	clone.Deleting = false
	mustPut(t, env.cli, CloneKey(env.cid, opsSpId, opsCloneName), clone)
	cleared, revision, err = env.clearMark(ref)
	if err != nil || !cleared || revision != env.spRev() {
		t.Fatalf("the clone's clear = %v, %d, %v", cleared, revision, err)
	}
	if mark := env.clone(opsCloneName).GetMetaGrp().GetForcedStart(); mark !=
		nil {
		t.Errorf("the clear left the clone's mark %v", mark)
	}
}
