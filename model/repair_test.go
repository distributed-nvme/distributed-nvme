package model

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The repair operations on the MD6 fixture: a repair of the data group's leg
// B, its new leg fresh on dn-c or taken from a retired leg, and the clone
// meta group's twin of each through its parent. Every refusal is checked to
// have written nothing at all, by the store's revision.

// The repair fixture's ids: below opsNextId like every id of the fixture,
// and apart from the slice's and the clone's.
const (
	opsRepName      = "rep0"
	opsRetiredLeg   = uint64(461)
	opsRetiredSide  = uint64(462)
	opsRetiredLeg2  = uint64(463)
	opsRetiredSide2 = uint64(464)
	// opsBadEpoch is the err_epoch that makes leg B a bad leg, and
	// opsRetiredEpoch the time a planted retired leg left its group.
	opsBadEpoch     = uint64(4000)
	opsRetiredEpoch = uint64(5000)
	opsFinishNow    = uint64(9000)
	// opsWorkerEpoch is the time in the name of the sp worker's repair.
	opsWorkerEpoch = uint64(8000)
	// opsDnE is a fifth DN, outside the fixture, for a fresh new leg while
	// dn-c and dn-d hold the group's retired sides.
	opsDnE = "dn-e:9000"
)

// opsSliceParent is the parent of the fixture slice's groups.
var opsSliceParent = GrpParent{SliceId: opsSliceId}

// opsWorkerRepName is the name of the sp worker's repair of the data group,
// which carries the reserved prefix.
var opsWorkerRepName = common.WorkerRepName(
	opsSpId, opsDataGrpId, opsWorkerEpoch)

// setLegErr stamps one slice leg's err_epoch directly, bypassing the op.
func (e *opsEnv) setLegErr(legId uint64, epoch uint64) {
	e.t.Helper()
	slice := e.slice()
	findLeg(slice, legId).ErrEpoch = epoch
	mustPut(e.t, e.cli, SliceKey(e.cid, opsSpId, opsSliceId), slice)
}

// chargeSide charges one side to its DN as the create that placed it would
// have: its pointer appended to side_ptr_list, extCnt out of free_ext_cnt
// and the capacity key moved with the free count it embeds.
func (e *opsEnv) chargeSide(
	addrPort string,
	legId uint64,
	sideId uint64,
	extCnt uint64,
) {
	e.t.Helper()
	dn := e.dn(addrPort)
	oldFree := dn.GetFreeExtCnt()
	e.delKey(DnCapacityKey(e.cid, mustBin(e.t, e, oldFree), oldFree, addrPort))
	dn.SidePtrList = append(dn.GetSidePtrList(), &pb.SidePointer{
		SpId: opsSpId, LegId: legId, SideId: sideId,
	})
	dn.FreeExtCnt = oldFree - extCnt
	mustPut(e.t, e.cli, DnConfKey(e.cid, addrPort), dn)
	newFree := dn.GetFreeExtCnt()
	mustPut(e.t, e.cli,
		DnCapacityKey(e.cid, mustBin(e.t, e, newFree), newFree, addrPort),
		&pb.DnCapacity{Location: addrPort})
}

// putRetired plants one retired leg of the slice's group grpId as
// FinishRepair leaves one: the record, with a provisioned side on addrPort,
// its entries in the group's and the pool's lists, and the side charged to
// its DN with the group's ext_cnt.
func (e *opsEnv) putRetired(
	grpId uint64,
	legId uint64,
	sideId uint64,
	addrPort string,
	legIdx uint32,
	epoch uint64,
) *pb.RetiredLeg {
	e.t.Helper()
	slice := e.slice()
	grp := findGroup(slice, grpId)
	grp.RetiredLegIdList = append(grp.RetiredLegIdList, legId)
	mustPut(e.t, e.cli, SliceKey(e.cid, opsSpId, opsSliceId), slice)
	conf := e.spConf()
	conf.RetiredLegIdList = append(conf.RetiredLegIdList, legId)
	mustPut(e.t, e.cli, SpConfKey(e.cid, opsSpName), conf)
	retired := &pb.RetiredLeg{
		GrpId:        grpId,
		LegIdx:       legIdx,
		Side:         opsSide(sideId, addrPort, true),
		RetiredEpoch: epoch,
		SliceId:      opsSliceId,
	}
	mustPut(e.t, e.cli, RetiredLegKey(e.cid, opsSpId, legId), retired)
	e.chargeSide(addrPort, legId, sideId, grp.GetExtCnt())
	return retired
}

// repair reads one Repair of the fixture SP.
func (e *opsEnv) repair(name string) *pb.Repair {
	e.t.Helper()
	rep := &pb.Repair{}
	e.get(RepairKey(e.cid, opsSpId, name), rep)
	return rep
}

// retired reads one RetiredLeg of the fixture SP.
func (e *opsEnv) retired(legId uint64) *pb.RetiredLeg {
	e.t.Helper()
	retired := &pb.RetiredLeg{}
	e.get(RetiredLegKey(e.cid, opsSpId, legId), retired)
	return retired
}

// freshLeg is a fresh new leg on addrPort for the data group, planned now.
func (e *opsEnv) freshLeg(addrPort string) RepairNewLeg {
	e.t.Helper()
	return RepairNewLeg{
		Dn:         e.dnCand(addrPort),
		GrpDnAddrs: e.grpDnAddrs(opsDataGrpId),
	}
}

// createRepair marks leg B bad and creates opsRepName for it on the data
// group, failing the test on any error; it returns the rep_id and the new
// leg's id.
func (e *opsEnv) createRepair(newLeg func() RepairNewLeg) (uint64, uint64) {
	e.t.Helper()
	return e.createRepairNamed(opsRepName, newLeg)
}

// createRepairNamed is createRepair under the name repName.
func (e *opsEnv) createRepairNamed(
	repName string,
	newLeg func() RepairNewLeg,
) (uint64, uint64) {
	e.t.Helper()
	e.setLegErr(opsDataLegB, opsBadEpoch)
	repId, legId, err := CreateRepair(
		e.ctx, e.cli, e.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		repName, opsSliceParent, opsDataGrpId, opsDataLegB, newLeg(),
		e.cc,
	)
	if err != nil {
		e.t.Fatalf("CreateRepair %s: %v", repName, err)
	}
	return repId, legId
}

// toStage moves the repair name to stage through the ops: the abort flag
// first for ABORTED.
func (e *opsEnv) toStage(name string, repId uint64, stage pb.RepStage) {
	e.t.Helper()
	if stage == pb.RepStage_REP_STAGE_ABORTED {
		if _, err := SetRepairAbort(e.ctx, e.cli, e.cid, opsShard, opsSpId,
			opsSpName, noExpectRev, name); err != nil {
			e.t.Fatalf("SetRepairAbort: %v", err)
		}
	}
	if err := SetRepairStage(e.ctx, e.cli, e.cid, opsShard, opsSpId,
		opsSpName, name, repId, stage); err != nil {
		e.t.Fatalf("SetRepairStage %v: %v", stage, err)
	}
}

// wantNoWrite asserts that err is a precondition of op with reason and that
// the store's revision has not moved from before: the op wrote nothing.
func (e *opsEnv) wantNoWrite(err error, op string, reason string, before int64) {
	e.t.Helper()
	precondition := wantPrecondition(e.t, err, op)
	if precondition.Reason != reason {
		e.t.Errorf("Reason: got %q, want %q", precondition.Reason, reason)
	}
	if got := e.storeRev(); got != before {
		e.t.Errorf("the refusal wrote: store revision %d, want %d",
			got, before)
	}
}

// TestCreateRepair is a repair with a fresh new leg: three ids from next_id
// in the order rep_id, leg id, side id; the new leg on the pick, with the
// next free leg_idx, the first cntlid slot and an unprovisioned side; the
// group's rep_name and the pool's list; the pick charged the group's ext_cnt
// and pointed at the new leg; one SpRev bump and one DnRev bump.
func TestCreateRepair(t *testing.T) {
	env := newOpsEnv(t)
	before := struct{ spRev, dnCRev uint64 }{env.spRev(), env.dnRev(opsDnC)}
	repId, legId := env.createRepair(func() RepairNewLeg {
		return env.freshLeg(opsDnC)
	})
	if repId != opsNextId || legId != opsNextId+1 {
		t.Errorf("rep_id %d, new leg %d, want %d and %d from next_id",
			repId, legId, opsNextId, opsNextId+1)
	}
	if got := env.spConf().GetNextId(); got != opsNextId+3 {
		t.Errorf("next_id: got %d, want %d", got, opsNextId+3)
	}
	want := &pb.Repair{
		RepId:    opsNextId,
		GrpId:    opsDataGrpId,
		RepStage: pb.RepStage_REP_STAGE_CREATED,
		NewLegList: []*pb.Leg{{
			LegId:    opsNextId + 1,
			LegIdx:   2,
			SideList: []*pb.Side{opsSide(opsNextId+2, opsDnC, false)},
		}},
		BadLegIdList: []uint64{opsDataLegB},
		SliceId:      opsSliceId,
	}
	if got := env.repair(opsRepName); !proto.Equal(got, want) {
		t.Errorf("repair: got %v, want %v", got, want)
	}
	grp := findGroup(env.slice(), opsDataGrpId)
	if grp.GetRepName() != opsRepName {
		t.Errorf("rep_name: got %q, want %q", grp.GetRepName(), opsRepName)
	}
	if len(grp.GetLegList()) != 2 ||
		grp.GetLegList()[1].GetLegId() != opsDataLegB {
		t.Errorf("leg_list moved before the finish: %v", grp.GetLegList())
	}
	if got := env.spConf().GetRepairNameList(); !equalStrings(
		got, []string{opsRepName}) {
		t.Errorf("repair_name_list: got %v", got)
	}
	dn := env.dn(opsDnC)
	if dn.GetFreeExtCnt() != opsDnFree-opsDataExtCnt {
		t.Errorf("dn-c free_ext_cnt: got %d, want %d",
			dn.GetFreeExtCnt(), opsDnFree-opsDataExtCnt)
	}
	wantPtr := &pb.SidePointer{
		SpId: opsSpId, LegId: opsNextId + 1, SideId: opsNextId + 2,
	}
	if len(dn.GetSidePtrList()) != 1 ||
		!proto.Equal(dn.GetSidePtrList()[0], wantPtr) {
		t.Errorf("dn-c side_ptr_list: got %v, want %v",
			dn.GetSidePtrList(), wantPtr)
	}
	if got := env.dnRev(opsDnC); got != before.dnCRev+1 {
		t.Errorf("dn-c rev: got %d, want %d", got, before.dnCRev+1)
	}
	if got := env.spRev(); got != before.spRev+1 {
		t.Errorf("SpRev: got %d, want %d", got, before.spRev+1)
	}
}

// TestCreateRepairFromARetiredLeg is a repair whose new leg is a retired leg
// of the group: it keeps its leg id, its leg_idx and its side, the repair
// keeps its retired_epoch, its record and its two list entries go, one id
// is taken, and its DN, which already holds its pointer and its extents, is
// not touched. It is allowed at the retired-leg bound.
func TestCreateRepairFromARetiredLeg(t *testing.T) {
	env := newOpsEnv(t)
	retired := env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide,
		opsDnC, 1, opsRetiredEpoch)
	env.putRetired(opsDataGrpId, opsRetiredLeg2, opsRetiredSide2,
		opsDnD, 1, opsRetiredEpoch+1)
	dnC, dnCRev := env.dn(opsDnC), env.dnRev(opsDnC)
	revBefore := env.spRev()

	repId, legId := env.createRepair(func() RepairNewLeg {
		return RepairNewLeg{RetiredLegId: opsRetiredLeg}
	})
	if repId != opsNextId || legId != opsRetiredLeg {
		t.Errorf("rep_id %d, new leg %d, want %d and %d", repId, legId,
			opsNextId, opsRetiredLeg)
	}
	if got := env.spConf().GetNextId(); got != opsNextId+1 {
		t.Errorf("next_id: got %d, want %d", got, opsNextId+1)
	}
	rep := env.repair(opsRepName)
	wantLeg := &pb.Leg{
		LegId: opsRetiredLeg, LegIdx: 1,
		SideList: []*pb.Side{retired.GetSide()},
	}
	if len(rep.GetNewLegList()) != 1 ||
		!proto.Equal(rep.GetNewLegList()[0], wantLeg) {
		t.Errorf("new legs: got %v, want %v", rep.GetNewLegList(), wantLeg)
	}
	if got := rep.GetNewLegIdToRetiredEpoch(); len(got) != 1 ||
		got[opsRetiredLeg] != opsRetiredEpoch {
		t.Errorf("retired times: got %v, want %d for %d", got,
			opsRetiredEpoch, opsRetiredLeg)
	}
	if env.exists(RetiredLegKey(env.cid, opsSpId, opsRetiredLeg)) {
		t.Errorf("the retired leg's record survived its reuse")
	}
	if got := env.spConf().GetRetiredLegIdList(); !equalIds(
		got, []uint64{opsRetiredLeg2}) {
		t.Errorf("the pool's retired_leg_id_list: got %v", got)
	}
	grp := findGroup(env.slice(), opsDataGrpId)
	if got := grp.GetRetiredLegIdList(); !equalIds(
		got, []uint64{opsRetiredLeg2}) {
		t.Errorf("the group's retired_leg_id_list: got %v", got)
	}
	if got := env.dn(opsDnC); !proto.Equal(got, dnC) {
		t.Errorf("dn-c moved: got %v, want %v", got, dnC)
	}
	if got := env.dnRev(opsDnC); got != dnCRev {
		t.Errorf("dn-c rev: got %d, want %d", got, dnCRev)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d", got, revBefore+1)
	}
}

// TestCreateRepairTakesARetiredLegOnTheBadLegsDn: the retired side may share
// its DN with the bad leg, which leaves the group at the finish; only a leg
// that stays rules a DN out.
func TestCreateRepairTakesARetiredLegOnTheBadLegsDn(t *testing.T) {
	env := newOpsEnv(t)
	env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide, opsDnB, 1,
		opsRetiredEpoch)
	_, legId := env.createRepair(func() RepairNewLeg {
		return RepairNewLeg{RetiredLegId: opsRetiredLeg}
	})
	if legId != opsRetiredLeg {
		t.Errorf("new leg %d, want %d", legId, opsRetiredLeg)
	}
}

// repairCall is one CreateRepair call of a precondition case.
type repairCall struct {
	name      string
	parent    GrpParent
	grpId     uint64
	badLeg    uint64
	newLeg    RepairNewLeg
	expectRev uint64
}

func (c *repairCall) run(env *opsEnv) error {
	_, _, err := CreateRepair(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, c.expectRev,
		c.name, c.parent, c.grpId, c.badLeg, c.newLeg, env.cc,
	)
	return err
}

// TestCreateRepairPreconditions is every refusal of CreateRepair, each with
// nothing written: the call is leg B of the data group, marked bad, with a
// fresh new leg on dn-c planned before the case's setup, which changes the
// store or the call.
func TestCreateRepairPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(env *opsEnv, call *repairCall)
		reason string
	}{
		{"group is not md-raid1", func(env *opsEnv, _ *repairCall) {
			conf := env.spConf()
			conf.BdevConf.RedundConf = &pb.RedundConf{
				RedunKind: &pb.RedundConf_RedundNone{
					RedundNone: &pb.RedundNone{},
				},
			}
			mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
		}, "group is not md-raid1"},
		{"a listed name", func(env *opsEnv, _ *repairCall) {
			conf := env.spConf()
			conf.RepairNameList = []string{opsRepName}
			mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
		}, ReasonRepairExists},
		{"a key without its name", func(env *opsEnv, _ *repairCall) {
			mustPut(env.t, env.cli, RepairKey(env.cid, opsSpId, opsRepName),
				&pb.Repair{RepId: 1})
		}, ReasonRepairExists},
		{"slice not found", func(_ *opsEnv, call *repairCall) {
			call.parent = GrpParent{SliceId: 9999}
		}, "slice not found"},
		{"both parents", func(_ *opsEnv, call *repairCall) {
			call.parent = GrpParent{SliceId: opsSliceId, CloneName: "c"}
		}, "no single parent"},
		{"no parent", func(_ *opsEnv, call *repairCall) {
			call.parent = GrpParent{}
		}, "no single parent"},
		{"clone not found", func(_ *opsEnv, call *repairCall) {
			call.parent = GrpParent{CloneName: opsCloneName}
		}, "clone not found"},
		{"group not found", func(_ *opsEnv, call *repairCall) {
			call.grpId = 9999
		}, "group not found"},
		{"a group of another parent", func(_ *opsEnv, call *repairCall) {
			call.grpId = opsCloneGrpId
		}, "group not found"},
		{"clone deleting", func(env *opsEnv, call *repairCall) {
			env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(true), true)
			call.parent = GrpParent{CloneName: opsCloneName}
			call.grpId = opsCloneGrpId
			call.badLeg = opsCloneLegB
			call.newLeg = RepairNewLeg{
				Dn: env.dnCand(opsDnA), GrpDnAddrs: env.cloneGrpDnAddrs(),
			}
		}, "clone deleting"},
		{"a group with a repair", func(env *opsEnv, _ *repairCall) {
			slice := env.slice()
			findGroup(slice, opsDataGrpId).RepName = "other"
			mustPut(env.t, env.cli,
				SliceKey(env.cid, opsSpId, opsSliceId), slice)
		}, ReasonGrpHasRepair},
		{"bad leg not found", func(_ *opsEnv, call *repairCall) {
			call.badLeg = opsMetaLegB
		}, "bad leg not found"},
		{"a bad leg with two sides", func(env *opsEnv, _ *repairCall) {
			slice := env.slice()
			leg := findLeg(slice, opsDataLegB)
			leg.SideList = append(leg.SideList,
				opsSide(opsDataSideB+100, opsDnD, false))
			mustPut(env.t, env.cli,
				SliceKey(env.cid, opsSpId, opsSliceId), slice)
		}, "bad leg has no single side"},
		{"a healthy bad leg", func(env *opsEnv, _ *repairCall) {
			env.setLegErr(opsDataLegB, 0)
		}, "bad leg is healthy: use a migration"},
		{"retired leg not found", func(_ *opsEnv, call *repairCall) {
			call.newLeg = RepairNewLeg{RetiredLegId: opsRetiredLeg}
		}, ReasonRetiredLegNotFound},
		{"retired leg record missing", func(env *opsEnv, call *repairCall) {
			env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide,
				opsDnC, 1, opsRetiredEpoch)
			env.delKey(RetiredLegKey(env.cid, opsSpId, opsRetiredLeg))
			call.newLeg = RepairNewLeg{RetiredLegId: opsRetiredLeg}
		}, "retired leg record missing"},
		{"a retired leg of another group", func(env *opsEnv, call *repairCall) {
			env.putRetired(opsMetaGrpId, opsRetiredLeg, opsRetiredSide,
				opsDnC, 1, opsRetiredEpoch)
			call.newLeg = RepairNewLeg{RetiredLegId: opsRetiredLeg}
		}, "retired leg of another group"},
		// The record and the group's list are checked apart: each case
		// below trips one of the two alone.
		{"a retired leg the group does not list",
			func(env *opsEnv, call *repairCall) {
				env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide,
					opsDnC, 1, opsRetiredEpoch)
				slice := env.slice()
				grp := findGroup(slice, opsDataGrpId)
				grp.RetiredLegIdList = removeId(
					grp.GetRetiredLegIdList(), opsRetiredLeg)
				mustPut(env.t, env.cli,
					SliceKey(env.cid, opsSpId, opsSliceId), slice)
				call.newLeg = RepairNewLeg{RetiredLegId: opsRetiredLeg}
			}, "retired leg of another group"},
		{"a listed retired leg that names another group",
			func(env *opsEnv, call *repairCall) {
				retired := env.putRetired(opsDataGrpId, opsRetiredLeg,
					opsRetiredSide, opsDnC, 1, opsRetiredEpoch)
				retired.GrpId = opsMetaGrpId
				mustPut(env.t, env.cli,
					RetiredLegKey(env.cid, opsSpId, opsRetiredLeg), retired)
				call.newLeg = RepairNewLeg{RetiredLegId: opsRetiredLeg}
			}, "retired leg of another group"},
		{"an unhealthy retired side", func(env *opsEnv, call *repairCall) {
			retired := env.putRetired(opsDataGrpId, opsRetiredLeg,
				opsRetiredSide, opsDnC, 1, opsRetiredEpoch)
			retired.Side.ErrEpoch = 7
			mustPut(env.t, env.cli,
				RetiredLegKey(env.cid, opsSpId, opsRetiredLeg), retired)
			call.newLeg = RepairNewLeg{RetiredLegId: opsRetiredLeg}
		}, "retired side is unhealthy"},
		{"a retired side on a staying leg's dn",
			func(env *opsEnv, call *repairCall) {
				env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide,
					opsDnA, 1, opsRetiredEpoch)
				call.newLeg = RepairNewLeg{RetiredLegId: opsRetiredLeg}
			}, "retired side shares a dn with the group"},
		{"an operator's fresh leg at the retired-leg bound",
			func(env *opsEnv, call *repairCall) {
				env.putRetiredBound()
				call.newLeg = env.freshLeg(opsDnE)
			}, ReasonRetiredListFull},
		{"a gain the plan did not hold", func(env *opsEnv, _ *repairCall) {
			// A migration's destination on the leg that stays.
			slice := env.slice()
			leg := findLeg(slice, opsDataLegA)
			leg.SideList = append(leg.SideList,
				opsSide(opsDataSideA+100, opsDnD, false))
			mustPut(env.t, env.cli,
				SliceKey(env.cid, opsSpId, opsSliceId), slice)
		}, ReasonCandidateChanged},
		{"dn already in the group", func(env *opsEnv, call *repairCall) {
			call.newLeg = env.freshLeg(opsDnA)
		}, "dn already in the group"},
		{"group is empty", func(env *opsEnv, _ *repairCall) {
			slice := env.slice()
			findGroup(slice, opsDataGrpId).ExtCnt = 0
			mustPut(env.t, env.cli,
				SliceKey(env.cid, opsSpId, opsSliceId), slice)
		}, "group is empty"},
		{"the pick's dn_conf deleted", func(env *opsEnv, _ *repairCall) {
			env.delKey(DnConfKey(env.cid, opsDnC))
		}, ReasonCandidateChanged},
		{"an operator's name where no group array is built",
			func(env *opsEnv, _ *repairCall) {
				env.setLevel(pb.SpLevel_SP_LEVEL_NO_REDUND)
			}, reasonRepairLevelNoArray},
		{"a reserved name where reactions stop",
			func(env *opsEnv, call *repairCall) {
				env.setLevel(pb.SpLevel_SP_LEVEL_NO_THINPOOL)
				call.name = opsWorkerRepName
			}, reasonRepairLevelReactions},
		{"a reserved name with a retired leg",
			func(env *opsEnv, call *repairCall) {
				env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide,
					opsDnC, 1, opsRetiredEpoch)
				call.name = opsWorkerRepName
				call.newLeg = RepairNewLeg{RetiredLegId: opsRetiredLeg}
			}, reasonReservedNameRetiredLeg},
		{"sp deleting", func(env *opsEnv, _ *repairCall) {
			env.setDeleting()
		}, "sp deleting"},
		// The deleting gate (loadSpConfAnyLevel) comes before the level
		// gate (checkCreateRepairLevel).
		{"sp deleting at a level that refuses", func(env *opsEnv, _ *repairCall) {
			env.setLevel(pb.SpLevel_SP_LEVEL_DISABLE)
			env.setDeleting()
		}, "sp deleting"},
		{"stale revision", func(_ *opsEnv, call *repairCall) {
			call.expectRev = 99
		}, ReasonStaleRevision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			env.setLegErr(opsDataLegB, opsBadEpoch)
			call := &repairCall{
				name:   opsRepName,
				parent: opsSliceParent,
				grpId:  opsDataGrpId,
				badLeg: opsDataLegB,
				newLeg: env.freshLeg(opsDnC),
			}
			tc.setup(env, call)
			before := env.storeRev()
			env.wantNoWrite(call.run(env), opCreateRepair, tc.reason, before)
		})
	}
}

// putRetiredBound fills the data group's retired list to the bound with two
// retired legs, opsRetiredLeg on dn-c and opsRetiredLeg2 on dn-d one second
// younger, and adds dn-e for a fresh new leg.
func (e *opsEnv) putRetiredBound() {
	e.t.Helper()
	if common.MaxRetiredLegPerGrp != 2 {
		e.t.Fatalf("the fixture plants 2 retired legs, the bound is %d",
			common.MaxRetiredLegPerGrp)
	}
	e.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide, opsDnC, 1,
		opsRetiredEpoch)
	e.putRetired(opsDataGrpId, opsRetiredLeg2, opsRetiredSide2, opsDnD, 1,
		opsRetiredEpoch+1)
	e.putDn(opsDnE, 704, 4, opsDnFree)
}

// TestCreateRepairLevelGate pins the level gate by the kind of name
// (CreateRepairLevelGate) at each boundary: an operator's name is taken at
// SP_LEVEL_NO_THINPOOL and refused from SP_LEVEL_NO_REDUND up, where no group
// array is built; the sp worker's name, with the reserved prefix, is taken at
// SP_LEVEL_NO_CLONE and refused from SP_LEVEL_NO_THINPOOL up, where reactions
// stop. A create that is taken writes the repair; one that is refused writes
// nothing.
func TestCreateRepairLevelGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		repName string
		level   pb.SpLevel
		reason  string
	}{
		{"an operator's name at NO_THINPOOL", opsRepName,
			pb.SpLevel_SP_LEVEL_NO_THINPOOL, ""},
		{"an operator's name at NO_REDUND", opsRepName,
			pb.SpLevel_SP_LEVEL_NO_REDUND, reasonRepairLevelNoArray},
		{"an operator's name at DISABLE", opsRepName,
			pb.SpLevel_SP_LEVEL_DISABLE, reasonRepairLevelNoArray},
		{"a reserved name at READWRITE", opsWorkerRepName,
			pb.SpLevel_SP_LEVEL_READWRITE, ""},
		{"a reserved name at NO_CLONE", opsWorkerRepName,
			pb.SpLevel_SP_LEVEL_NO_CLONE, ""},
		{"a reserved name at NO_THINPOOL", opsWorkerRepName,
			pb.SpLevel_SP_LEVEL_NO_THINPOOL, reasonRepairLevelReactions},
		{"a reserved name at NO_REDUND", opsWorkerRepName,
			pb.SpLevel_SP_LEVEL_NO_REDUND, reasonRepairLevelReactions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			env.setLegErr(opsDataLegB, opsBadEpoch)
			env.setLevel(tc.level)
			call := &repairCall{
				name:   tc.repName,
				parent: opsSliceParent,
				grpId:  opsDataGrpId,
				badLeg: opsDataLegB,
				newLeg: env.freshLeg(opsDnC),
			}
			before := env.storeRev()
			err := call.run(env)
			if tc.reason != "" {
				env.wantNoWrite(err, opCreateRepair, tc.reason, before)
				return
			}
			if err != nil {
				t.Fatalf("CreateRepair at %s: %v", tc.level, err)
			}
			if got := findGroup(env.slice(), opsDataGrpId).GetRepName(); got !=
				tc.repName {
				t.Errorf("rep_name: got %q, want %q", got, tc.repName)
			}
			if got := env.repair(tc.repName).GetRepStage(); got !=
				pb.RepStage_REP_STAGE_CREATED {
				t.Errorf("stage: got %v, want CREATED", got)
			}
		})
	}
	for repName, want := range map[string]pb.SpLevel{
		opsRepName:       pb.SpLevel_SP_LEVEL_NO_REDUND,
		opsWorkerRepName: pb.SpLevel_SP_LEVEL_NO_THINPOOL,
	} {
		if got := CreateRepairLevelGate(repName); got != want {
			t.Errorf("CreateRepairLevelGate(%q) = %v, want %v",
				repName, got, want)
		}
	}
}

// TestCreateRepairReservedNameAtTheRetiredBound: the sp worker's repair takes
// a fresh new leg while its group holds MaxRetiredLegPerGrp retired legs,
// which an operator's create refuses (TestCreateRepairPreconditions). It
// writes what every fresh create writes — three ids, the new leg on the
// pick with the next leg_idx and an unprovisioned side, the group's rep_name,
// the pool's list, the pick charged, one DnRev and one SpRev bump — and
// leaves the retired legs, their lists and their DNs as they were: nothing
// is given back before the finish.
func TestCreateRepairReservedNameAtTheRetiredBound(t *testing.T) {
	env := newOpsEnv(t)
	env.putRetiredBound()
	retired := []*pb.RetiredLeg{
		env.retired(opsRetiredLeg), env.retired(opsRetiredLeg2),
	}
	dnC, dnD := env.dn(opsDnC), env.dn(opsDnD)
	dnCRev, dnDRev := env.dnRev(opsDnC), env.dnRev(opsDnD)
	spRev, dnERev := env.spRev(), env.dnRev(opsDnE)

	repId, legId := env.createRepairNamed(opsWorkerRepName,
		func() RepairNewLeg { return env.freshLeg(opsDnE) })
	if repId != opsNextId || legId != opsNextId+1 {
		t.Errorf("rep_id %d, new leg %d, want %d and %d from next_id",
			repId, legId, opsNextId, opsNextId+1)
	}
	want := &pb.Repair{
		RepId:    opsNextId,
		GrpId:    opsDataGrpId,
		RepStage: pb.RepStage_REP_STAGE_CREATED,
		NewLegList: []*pb.Leg{{
			LegId:    opsNextId + 1,
			LegIdx:   2,
			SideList: []*pb.Side{opsSide(opsNextId+2, opsDnE, false)},
		}},
		BadLegIdList: []uint64{opsDataLegB},
		SliceId:      opsSliceId,
	}
	if got := env.repair(opsWorkerRepName); !proto.Equal(got, want) {
		t.Errorf("repair: got %v, want %v", got, want)
	}
	wantRetired := []uint64{opsRetiredLeg, opsRetiredLeg2}
	grp := findGroup(env.slice(), opsDataGrpId)
	if grp.GetRepName() != opsWorkerRepName ||
		!equalIds(grp.GetRetiredLegIdList(), wantRetired) ||
		len(grp.GetLegList()) != 2 ||
		grp.GetLegList()[1].GetLegId() != opsDataLegB {
		t.Errorf("group: rep_name %q, retired legs %v, legs %v",
			grp.GetRepName(), grp.GetRetiredLegIdList(), grp.GetLegList())
	}
	conf := env.spConf()
	if !equalStrings(conf.GetRepairNameList(), []string{opsWorkerRepName}) ||
		!equalIds(conf.GetRetiredLegIdList(), wantRetired) ||
		conf.GetNextId() != opsNextId+3 {
		t.Errorf("pool: repairs %v, retired legs %v, next_id %d",
			conf.GetRepairNameList(), conf.GetRetiredLegIdList(),
			conf.GetNextId())
	}
	for i, legId := range wantRetired {
		if got := env.retired(legId); !proto.Equal(got, retired[i]) {
			t.Errorf("retired leg %d: got %v, want %v", legId, got,
				retired[i])
		}
	}
	dnE := env.dn(opsDnE)
	wantPtr := &pb.SidePointer{
		SpId: opsSpId, LegId: opsNextId + 1, SideId: opsNextId + 2,
	}
	if dnE.GetFreeExtCnt() != opsDnFree-opsDataExtCnt ||
		len(dnE.GetSidePtrList()) != 1 ||
		!proto.Equal(dnE.GetSidePtrList()[0], wantPtr) {
		t.Errorf("dn-e: free %d, pointers %v, want %d and %v",
			dnE.GetFreeExtCnt(), dnE.GetSidePtrList(),
			opsDnFree-opsDataExtCnt, wantPtr)
	}
	if got := env.dnRev(opsDnE); got != dnERev+1 {
		t.Errorf("dn-e rev: got %d, want %d", got, dnERev+1)
	}
	if !proto.Equal(env.dn(opsDnC), dnC) || !proto.Equal(env.dn(opsDnD), dnD) ||
		env.dnRev(opsDnC) != dnCRev || env.dnRev(opsDnD) != dnDRev {
		t.Errorf("a retired side's DN moved at the create")
	}
	if got := env.spRev(); got != spRev+1 {
		t.Errorf("SpRev: got %d, want %d", got, spRev+1)
	}
}

// TestFinishRepair: the new leg takes the bad leg's place in leg_list and its
// leg_idx; the bad leg becomes a RetiredLeg under its leg id with its side,
// the time the caller gives and the parent; the repair and its name go, the
// group's rep_name is cleared, the retired leg is listed in the group and in
// the pool, the bad side's DN keeps its pointer, and SpRev is bumped once.
func TestFinishRepair(t *testing.T) {
	env := newOpsEnv(t)
	env.chargeFixture()
	repId, legId := env.createRepair(func() RepairNewLeg {
		return env.freshLeg(opsDnC)
	})
	env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_STARTED)
	newLeg := env.repair(opsRepName).GetNewLegList()[0]
	badSide := findLeg(env.slice(), opsDataLegB).GetSideList()[0]
	dnB, revBefore := env.dn(opsDnB), env.spRev()

	res, err := FinishRepair(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, noExpectRev, opsRepName, opsFinishNow, env.cc)
	if err != nil {
		t.Fatalf("FinishRepair: %v", err)
	}
	if want := (FinishRepairResult{
		RepId: repId, RetiredLegId: opsDataLegB,
	}); res != want {
		t.Errorf("FinishRepair = %+v, want %+v", res, want)
	}
	grp := findGroup(env.slice(), opsDataGrpId)
	wantLeg := proto.Clone(newLeg).(*pb.Leg)
	wantLeg.LegIdx = 1
	if len(grp.GetLegList()) != 2 ||
		grp.GetLegList()[0].GetLegId() != opsDataLegA ||
		!proto.Equal(grp.GetLegList()[1], wantLeg) {
		t.Errorf("leg_list: got %v, want leg A then %v", grp.GetLegList(),
			wantLeg)
	}
	if grp.GetRepName() != "" {
		t.Errorf("rep_name: got %q, want it cleared", grp.GetRepName())
	}
	if got := grp.GetRetiredLegIdList(); !equalIds(got,
		[]uint64{opsDataLegB}) {
		t.Errorf("the group's retired_leg_id_list: got %v", got)
	}
	want := &pb.RetiredLeg{
		GrpId: opsDataGrpId, LegIdx: 1, Side: badSide,
		RetiredEpoch: opsFinishNow, SliceId: opsSliceId,
	}
	if got := env.retired(opsDataLegB); !proto.Equal(got, want) {
		t.Errorf("retired leg: got %v, want %v", got, want)
	}
	if env.exists(RepairKey(env.cid, opsSpId, opsRepName)) {
		t.Errorf("the repair survived its finish")
	}
	conf := env.spConf()
	if len(conf.GetRepairNameList()) != 0 ||
		!equalIds(conf.GetRetiredLegIdList(), []uint64{opsDataLegB}) {
		t.Errorf("the pool's lists: repairs %v, retired legs %v",
			conf.GetRepairNameList(), conf.GetRetiredLegIdList())
	}
	if got := env.dn(opsDnB); !proto.Equal(got, dnB) {
		t.Errorf("dn-b moved: the retired side keeps its pointer: %v", got)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d", got, revBefore+1)
	}
	if legId != wantLeg.GetLegId() {
		t.Errorf("the new leg in leg_list is %d, want %d",
			wantLeg.GetLegId(), legId)
	}
}

// opsRetiredPlant is one retired leg of the data group a give-back case
// plants, in the order of the group's list.
type opsRetiredPlant struct {
	legId  uint64
	sideId uint64
	addr   string
	epoch  uint64
}

// startWorkerRepair creates the sp worker's repair of leg B with a fresh new
// leg on dn-e and moves it to STARTED; it returns its rep_id.
func (e *opsEnv) startWorkerRepair() uint64 {
	e.t.Helper()
	repId, _ := e.createRepairNamed(opsWorkerRepName,
		func() RepairNewLeg { return e.freshLeg(opsDnE) })
	e.toStage(opsWorkerRepName, repId, pb.RepStage_REP_STAGE_STARTED)
	return repId
}

// TestFinishRepairGivesBackTheOldestRetiredLeg: the finish of the sp
// worker's repair whose group holds MaxRetiredLegPerGrp retired legs gives
// back the oldest one in the same transaction — the smallest retired_epoch,
// then the smallest leg id, whatever the order of the list — and names it in
// its result. Its record and its two list entries go and its side goes back
// to its DN as DeleteRetiredLeg gives one back: the pointer out, the group's
// ext_cnt back, the capacity key moved, one DnRev bump. The other retired leg
// and its DN stay as they were; the bad leg joins the lists after it, as
// every finish retires it; one SpRev bump.
func TestFinishRepairGivesBackTheOldestRetiredLeg(t *testing.T) {
	c := opsRetiredPlant{opsRetiredLeg, opsRetiredSide, opsDnC, 0}
	d := opsRetiredPlant{opsRetiredLeg2, opsRetiredSide2, opsDnD, 0}
	at := func(p opsRetiredPlant, epoch uint64) opsRetiredPlant {
		p.epoch = epoch
		return p
	}
	if c.legId >= d.legId {
		t.Fatalf("the cases need dn-c's retired leg %d below dn-d's %d",
			c.legId, d.legId)
	}
	for _, tc := range []struct {
		name   string
		plants []opsRetiredPlant
		gone   opsRetiredPlant
		kept   opsRetiredPlant
	}{
		{"the older one listed second, with the larger leg id",
			[]opsRetiredPlant{at(c, 6000), at(d, 5000)},
			at(d, 5000), at(c, 6000)},
		{"the older one listed first, with the larger leg id",
			[]opsRetiredPlant{at(d, 5000), at(c, 6000)},
			at(d, 5000), at(c, 6000)},
		{"equal times: the smaller leg id, listed second",
			[]opsRetiredPlant{at(d, 5000), at(c, 5000)},
			at(c, 5000), at(d, 5000)},
		{"equal times: the smaller leg id, listed first",
			[]opsRetiredPlant{at(c, 5000), at(d, 5000)},
			at(c, 5000), at(d, 5000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			for _, p := range tc.plants {
				env.putRetired(opsDataGrpId, p.legId, p.sideId, p.addr, 1,
					p.epoch)
			}
			env.putDn(opsDnE, 704, 4, opsDnFree)
			repId := env.startWorkerRepair()
			newLeg := env.repair(opsWorkerRepName).GetNewLegList()[0]
			badSide := findLeg(env.slice(), opsDataLegB).GetSideList()[0]
			kept := env.retired(tc.kept.legId)
			charged := opsDnFree - opsDataExtCnt
			keptDn, keptRev := env.dn(tc.kept.addr), env.dnRev(tc.kept.addr)
			goneRev := env.dnRev(tc.gone.addr)
			dnB, dnE := env.dn(opsDnB), env.dn(opsDnE)
			spRev := env.spRev()

			res, err := FinishRepair(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, noExpectRev, opsWorkerRepName,
				opsFinishNow, env.cc)
			if err != nil {
				t.Fatalf("FinishRepair: %v", err)
			}
			if want := (FinishRepairResult{
				RepId: repId, RetiredLegId: opsDataLegB,
				GivenBackLegId: tc.gone.legId,
			}); res != want {
				t.Errorf("FinishRepair = %+v, want %+v", res, want)
			}
			if env.exists(RetiredLegKey(env.cid, opsSpId, tc.gone.legId)) {
				t.Errorf("the given-back retired leg %d survived",
					tc.gone.legId)
			}
			if got := env.retired(tc.kept.legId); !proto.Equal(got, kept) {
				t.Errorf("the kept retired leg: got %v, want %v", got, kept)
			}
			wantBad := &pb.RetiredLeg{
				GrpId: opsDataGrpId, LegIdx: 1, Side: badSide,
				RetiredEpoch: opsFinishNow, SliceId: opsSliceId,
			}
			if got := env.retired(opsDataLegB); !proto.Equal(got, wantBad) {
				t.Errorf("the retired bad leg: got %v, want %v", got,
					wantBad)
			}
			wantList := []uint64{tc.kept.legId, opsDataLegB}
			grp := findGroup(env.slice(), opsDataGrpId)
			wantLeg := proto.Clone(newLeg).(*pb.Leg)
			wantLeg.LegIdx = 1
			if len(grp.GetLegList()) != 2 ||
				grp.GetLegList()[0].GetLegId() != opsDataLegA ||
				!proto.Equal(grp.GetLegList()[1], wantLeg) ||
				grp.GetRepName() != "" ||
				!equalIds(grp.GetRetiredLegIdList(), wantList) {
				t.Errorf("group: legs %v, rep_name %q, retired legs %v, "+
					"want leg A then %v, none and %v", grp.GetLegList(),
					grp.GetRepName(), grp.GetRetiredLegIdList(), wantLeg,
					wantList)
			}
			conf := env.spConf()
			if len(conf.GetRepairNameList()) != 0 ||
				!equalIds(conf.GetRetiredLegIdList(), wantList) {
				t.Errorf("pool: repairs %v, retired legs %v, want none "+
					"and %v", conf.GetRepairNameList(),
					conf.GetRetiredLegIdList(), wantList)
			}
			if env.exists(RepairKey(env.cid, opsSpId, opsWorkerRepName)) {
				t.Errorf("the repair survived its finish")
			}
			gone := env.dn(tc.gone.addr)
			if gone.GetFreeExtCnt() != opsDnFree ||
				len(gone.GetSidePtrList()) != 0 {
				t.Errorf("%s: free %d with pointers %v, want %d and none",
					tc.gone.addr, gone.GetFreeExtCnt(),
					gone.GetSidePtrList(), opsDnFree)
			}
			if env.exists(DnCapacityKey(env.cid, mustBin(t, env, charged),
				charged, tc.gone.addr)) ||
				!env.exists(DnCapacityKey(env.cid,
					mustBin(t, env, opsDnFree), opsDnFree, tc.gone.addr)) {
				t.Errorf("%s: the capacity key did not move back to %d",
					tc.gone.addr, opsDnFree)
			}
			if got := env.dnRev(tc.gone.addr); got != goneRev+1 {
				t.Errorf("%s rev: got %d, want %d", tc.gone.addr, got,
					goneRev+1)
			}
			if !proto.Equal(env.dn(tc.kept.addr), keptDn) ||
				env.dnRev(tc.kept.addr) != keptRev {
				t.Errorf("%s, the kept retired side's DN, moved",
					tc.kept.addr)
			}
			if !proto.Equal(env.dn(opsDnB), dnB) ||
				!proto.Equal(env.dn(opsDnE), dnE) {
				t.Errorf("the bad side's or the new side's DN moved")
			}
			if got := env.spRev(); got != spRev+1 {
				t.Errorf("SpRev: got %d, want %d", got, spRev+1)
			}
		})
	}
}

// TestFinishRepairGivesBackOnlyAtTheBound: the sp worker's repair below the
// bound, and an operator's repair whatever its group holds, finish without
// giving a retired leg back: the bad leg joins the lists and every retired
// leg, with its DN, stays. An operator's repair meets a group at the bound
// only in a planted state, since its create refuses one or takes a retired
// leg from it; the case pins that an operator's kept copies never go by a
// finish.
func TestFinishRepairGivesBackOnlyAtTheBound(t *testing.T) {
	for _, tc := range []struct {
		name    string
		finish  func(env *opsEnv) string
		retired []uint64
	}{
		{"the worker's repair below the bound", func(env *opsEnv) string {
			env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide,
				opsDnC, 1, opsRetiredEpoch)
			env.putDn(opsDnE, 704, 4, opsDnFree)
			env.startWorkerRepair()
			return opsWorkerRepName
		}, []uint64{opsRetiredLeg}},
		{"an operator's repair at the bound", func(env *opsEnv) string {
			env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide,
				opsDnC, 1, opsRetiredEpoch)
			env.putDn(opsDnE, 704, 4, opsDnFree)
			repId, _ := env.createRepair(func() RepairNewLeg {
				return env.freshLeg(opsDnE)
			})
			env.putRetired(opsDataGrpId, opsRetiredLeg2, opsRetiredSide2,
				opsDnD, 1, opsRetiredEpoch+1)
			env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_STARTED)
			return opsRepName
		}, []uint64{opsRetiredLeg, opsRetiredLeg2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			repName := tc.finish(env)
			before := map[uint64]*pb.RetiredLeg{}
			for _, legId := range tc.retired {
				before[legId] = env.retired(legId)
			}
			dnC, dnD := env.dn(opsDnC), env.dn(opsDnD)
			res, err := FinishRepair(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, noExpectRev, repName, opsFinishNow,
				env.cc)
			if err != nil {
				t.Fatalf("FinishRepair: %v", err)
			}
			if res.GivenBackLegId != 0 || res.RetiredLegId != opsDataLegB {
				t.Errorf("FinishRepair = %+v, want leg %d retired and none "+
					"given back", res, opsDataLegB)
			}
			for legId, want := range before {
				if got := env.retired(legId); !proto.Equal(got, want) {
					t.Errorf("retired leg %d: got %v, want %v", legId, got,
						want)
				}
			}
			wantList := append(append([]uint64{}, tc.retired...), opsDataLegB)
			if got := findGroup(env.slice(), opsDataGrpId).
				GetRetiredLegIdList(); !equalIds(got, wantList) {
				t.Errorf("the group's retired legs: got %v, want %v", got,
					wantList)
			}
			if got := env.spConf().GetRetiredLegIdList(); !equalIds(
				got, wantList) {
				t.Errorf("the pool's retired legs: got %v, want %v", got,
					wantList)
			}
			if !proto.Equal(env.dn(opsDnC), dnC) ||
				!proto.Equal(env.dn(opsDnD), dnD) {
				t.Errorf("a retired side's DN moved")
			}
		})
	}
}

// TestFinishRepairGiveBackRefusals: the give-back reads each retired leg the
// group lists, so a listed record that is missing, or one that names another
// group, refuses the finish, which writes nothing.
func TestFinishRepairGiveBackRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(env *opsEnv)
		reason string
	}{
		{"a listed retired leg's record missing", func(env *opsEnv) {
			env.delKey(RetiredLegKey(env.cid, opsSpId, opsRetiredLeg2))
		}, "retired leg record missing"},
		{"a listed retired leg the pool does not list", func(env *opsEnv) {
			conf := env.spConf()
			conf.RetiredLegIdList = removeId(
				conf.GetRetiredLegIdList(), opsRetiredLeg2)
			mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
		}, ReasonRetiredLegNotFound},
		{"a listed retired leg of another group", func(env *opsEnv) {
			retired := env.retired(opsRetiredLeg2)
			retired.GrpId = opsMetaGrpId
			mustPut(env.t, env.cli,
				RetiredLegKey(env.cid, opsSpId, opsRetiredLeg2), retired)
		}, "retired leg of another group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			env.putRetiredBound()
			env.startWorkerRepair()
			tc.setup(env)
			before := env.storeRev()
			_, err := FinishRepair(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, noExpectRev, opsWorkerRepName,
				opsFinishNow, env.cc)
			env.wantNoWrite(err, opFinishRepair, tc.reason, before)
		})
	}
}

// TestRepairOpsAtAnyLevel: every repair and retired-leg operation but the
// create takes a pool at a level where the operator is in charge, where the
// arrays still run (loadSpConfAnyLevel): the finish, the cancel, the abort,
// the delete of a retired leg and the provisioned flip each succeed at
// SP_LEVEL_DISABLE. TestSetRepairStage holds the stage move's case.
func TestRepairOpsAtAnyLevel(t *testing.T) {
	disable := func(env *opsEnv) { env.setLevel(pb.SpLevel_SP_LEVEL_DISABLE) }
	for _, tc := range []struct {
		name string
		run  func(env *opsEnv) error
	}{
		{"FinishRepair", func(env *opsEnv) error {
			repId, _ := env.createRepair(func() RepairNewLeg {
				return env.freshLeg(opsDnC)
			})
			env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_STARTED)
			disable(env)
			_, err := FinishRepair(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, noExpectRev, opsRepName, opsFinishNow,
				env.cc)
			return err
		}},
		{"CancelRepair", func(env *opsEnv) error {
			repId, _ := env.createRepair(func() RepairNewLeg {
				return env.freshLeg(opsDnC)
			})
			env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_ABORTED)
			disable(env)
			_, err := CancelRepair(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, noExpectRev, opsRepName, env.cc)
			return err
		}},
		{"SetRepairAbort", func(env *opsEnv) error {
			env.createRepair(func() RepairNewLeg {
				return env.freshLeg(opsDnC)
			})
			disable(env)
			_, err := SetRepairAbort(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, noExpectRev, opsRepName)
			if err == nil && !env.repair(opsRepName).GetTryToAbort() {
				env.t.Errorf("the abort flag is not set")
			}
			return err
		}},
		{"DeleteRetiredLeg", func(env *opsEnv) error {
			env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide,
				opsDnC, 1, opsRetiredEpoch)
			disable(env)
			return DeleteRetiredLeg(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, noExpectRev, opsRetiredLeg, env.cc)
		}},
		{"FlipRepairProvisioned", func(env *opsEnv) error {
			repId, legId := env.createRepair(func() RepairNewLeg {
				return env.freshLeg(opsDnC)
			})
			disable(env)
			flipped, _, err := FlipRepairProvisioned(env.ctx, env.cli,
				env.cid, opsShard, opsSpId, opsSpName, RepairSideRef{
					RepName: opsRepName, RepId: repId, LegId: legId,
					SideId: legId + 1,
				})
			if err == nil && !flipped {
				env.t.Errorf("the flip skipped the side")
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			if err := tc.run(env); err != nil {
				t.Fatalf("%s at SP_LEVEL_DISABLE: %v", tc.name, err)
			}
		})
	}
}

// TestFinishRepairPreconditions is every refusal of FinishRepair, each with
// nothing written.
func TestFinishRepairPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stage  pb.RepStage
		setup  func(env *opsEnv)
		repN   string
		expect uint64
		reason string
	}{
		{name: "a CREATED repair", stage: pb.RepStage_REP_STAGE_CREATED,
			reason: "repair stage is REP_STAGE_CREATED"},
		{name: "an ABORTED repair", stage: pb.RepStage_REP_STAGE_ABORTED,
			reason: "repair stage is REP_STAGE_ABORTED"},
		{name: "repair not found", stage: pb.RepStage_REP_STAGE_STARTED,
			repN: "nope", reason: ReasonRepairNotFound},
		{name: "repair record missing", stage: pb.RepStage_REP_STAGE_STARTED,
			setup: func(env *opsEnv) {
				env.delKey(RepairKey(env.cid, opsSpId, opsRepName))
			}, reason: "repair record missing"},
		{name: "a group that names another repair",
			stage: pb.RepStage_REP_STAGE_STARTED,
			setup: func(env *opsEnv) {
				slice := env.slice()
				findGroup(slice, opsDataGrpId).RepName = "other"
				mustPut(env.t, env.cli,
					SliceKey(env.cid, opsSpId, opsSliceId), slice)
			}, reason: "group does not name the repair"},
		{name: "bad leg not found", stage: pb.RepStage_REP_STAGE_STARTED,
			setup: func(env *opsEnv) {
				slice := env.slice()
				findLeg(slice, opsDataLegB).LegId = opsDataLegB + 100
				mustPut(env.t, env.cli,
					SliceKey(env.cid, opsSpId, opsSliceId), slice)
			}, reason: "bad leg not found"},
		{name: "a bad leg with two sides", stage: pb.RepStage_REP_STAGE_STARTED,
			setup: func(env *opsEnv) {
				slice := env.slice()
				leg := findLeg(slice, opsDataLegB)
				leg.SideList = append(leg.SideList,
					opsSide(opsDataSideB+100, opsDnD, false))
				mustPut(env.t, env.cli,
					SliceKey(env.cid, opsSpId, opsSliceId), slice)
			}, reason: "bad leg has no single side"},
		{name: "two bad legs", stage: pb.RepStage_REP_STAGE_STARTED,
			setup: func(env *opsEnv) {
				rep := env.repair(opsRepName)
				rep.BadLegIdList = append(rep.BadLegIdList, opsDataLegA)
				mustPut(env.t, env.cli,
					RepairKey(env.cid, opsSpId, opsRepName), rep)
			}, reason: "repair has no single bad leg"},
		{name: "no new leg", stage: pb.RepStage_REP_STAGE_STARTED,
			setup: func(env *opsEnv) {
				rep := env.repair(opsRepName)
				rep.NewLegList = nil
				mustPut(env.t, env.cli,
					RepairKey(env.cid, opsSpId, opsRepName), rep)
			}, reason: "repair has no single new leg"},
		{name: "sp deleting", stage: pb.RepStage_REP_STAGE_STARTED,
			setup:  func(env *opsEnv) { env.setDeleting() },
			reason: "sp deleting"},
		// The stored conf is gated whether the finish gives a side back
		// or not, as CancelRepair gates it.
		{name: "an invalid stored conf", stage: pb.RepStage_REP_STAGE_STARTED,
			setup:  func(env *opsEnv) { env.cc.DnBinConf.ExtentSize = 0 },
			reason: "invalid stored conf: dn_bin_conf.extent_size is zero"},
		{name: "stale revision", stage: pb.RepStage_REP_STAGE_STARTED,
			expect: 99, reason: ReasonStaleRevision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			repId, _ := env.createRepair(func() RepairNewLeg {
				return env.freshLeg(opsDnC)
			})
			if tc.stage != pb.RepStage_REP_STAGE_CREATED {
				env.toStage(opsRepName, repId, tc.stage)
			}
			if tc.setup != nil {
				tc.setup(env)
			}
			name := opsRepName
			if tc.repN != "" {
				name = tc.repN
			}
			before := env.storeRev()
			_, err := FinishRepair(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, tc.expect, name, opsFinishNow, env.cc)
			env.wantNoWrite(err, opFinishRepair, tc.reason, before)
		})
	}
}

// TestCancelRepairGivesAFreshLegBack: a fresh new leg's side goes back to its
// DN — the pointer out, the group's ext_cnt back, the capacity key moved, one
// DnRev bump — the repair and its name go, the group's rep_name is cleared,
// and SpRev is bumped once.
func TestCancelRepairGivesAFreshLegBack(t *testing.T) {
	env := newOpsEnv(t)
	repId, _ := env.createRepair(func() RepairNewLeg {
		return env.freshLeg(opsDnC)
	})
	env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_ABORTED)
	charged := opsDnFree - opsDataExtCnt
	dnCRev, revBefore := env.dnRev(opsDnC), env.spRev()

	got, err := CancelRepair(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, noExpectRev, opsRepName, env.cc)
	if err != nil {
		t.Fatalf("CancelRepair: %v", err)
	}
	if got != repId {
		t.Errorf("CancelRepair = %d, want %d", got, repId)
	}
	dn := env.dn(opsDnC)
	if dn.GetFreeExtCnt() != opsDnFree || len(dn.GetSidePtrList()) != 0 {
		t.Errorf("dn-c: free %d with pointers %v, want %d and none",
			dn.GetFreeExtCnt(), dn.GetSidePtrList(), opsDnFree)
	}
	if env.exists(DnCapacityKey(
		env.cid, mustBin(t, env, charged), charged, opsDnC)) {
		t.Errorf("dn-c: the charged capacity key survived")
	}
	if got := env.dnRev(opsDnC); got != dnCRev+1 {
		t.Errorf("dn-c rev: got %d, want %d", got, dnCRev+1)
	}
	if env.exists(RepairKey(env.cid, opsSpId, opsRepName)) {
		t.Errorf("the repair survived its cancel")
	}
	if len(env.spConf().GetRepairNameList()) != 0 {
		t.Errorf("repair_name_list: got %v", env.spConf().GetRepairNameList())
	}
	grp := findGroup(env.slice(), opsDataGrpId)
	if grp.GetRepName() != "" || len(grp.GetLegList()) != 2 ||
		grp.GetLegList()[1].GetLegId() != opsDataLegB {
		t.Errorf("group: rep_name %q, legs %v", grp.GetRepName(),
			grp.GetLegList())
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d", got, revBefore+1)
	}
}

// TestCancelRepairKeepsTheRetiredLeg: a new leg taken from a retired leg goes
// back as that retired leg with its old time, its leg_idx and its side, in
// the group's and the pool's lists, and its DN does not move: Cancel never
// destroys the kept copy.
func TestCancelRepairKeepsTheRetiredLeg(t *testing.T) {
	env := newOpsEnv(t)
	retired := env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide,
		opsDnC, 1, opsRetiredEpoch)
	repId, _ := env.createRepair(func() RepairNewLeg {
		return RepairNewLeg{RetiredLegId: opsRetiredLeg}
	})
	env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_ABORTED)
	dnC, dnCRev := env.dn(opsDnC), env.dnRev(opsDnC)

	if _, err := CancelRepair(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, noExpectRev, opsRepName, env.cc); err != nil {
		t.Fatalf("CancelRepair: %v", err)
	}
	if got := env.retired(opsRetiredLeg); !proto.Equal(got, retired) {
		t.Errorf("retired leg: got %v, want %v", got, retired)
	}
	if got := env.spConf().GetRetiredLegIdList(); !equalIds(
		got, []uint64{opsRetiredLeg}) {
		t.Errorf("the pool's retired_leg_id_list: got %v", got)
	}
	grp := findGroup(env.slice(), opsDataGrpId)
	if got := grp.GetRetiredLegIdList(); !equalIds(
		got, []uint64{opsRetiredLeg}) {
		t.Errorf("the group's retired_leg_id_list: got %v", got)
	}
	if got := env.dn(opsDnC); !proto.Equal(got, dnC) {
		t.Errorf("dn-c moved: got %v, want %v", got, dnC)
	}
	if got := env.dnRev(opsDnC); got != dnCRev {
		t.Errorf("dn-c rev: got %d, want %d", got, dnCRev)
	}
}

// TestCancelRepairOfAWorkersRepairKeepsTheRetiredLegs: the cancel of the sp
// worker's repair, aborted while its group holds MaxRetiredLegPerGrp retired
// legs, gives its fresh new leg back to dn-e as every cancel does and keeps
// every retired leg — the records, the group's and the pool's lists and the
// DNs of their sides — since only the finish gives one back
// (architecture.md, Repair, The worker's repairs).
func TestCancelRepairOfAWorkersRepairKeepsTheRetiredLegs(t *testing.T) {
	env := newOpsEnv(t)
	env.putRetiredBound()
	repId, _ := env.createRepairNamed(opsWorkerRepName,
		func() RepairNewLeg { return env.freshLeg(opsDnE) })
	env.toStage(opsWorkerRepName, repId, pb.RepStage_REP_STAGE_ABORTED)
	wantRetired := []uint64{opsRetiredLeg, opsRetiredLeg2}
	retired := []*pb.RetiredLeg{
		env.retired(opsRetiredLeg), env.retired(opsRetiredLeg2),
	}
	dnC, dnD := env.dn(opsDnC), env.dn(opsDnD)
	dnCRev, dnDRev := env.dnRev(opsDnC), env.dnRev(opsDnD)
	charged := opsDnFree - opsDataExtCnt
	dnERev := env.dnRev(opsDnE)

	got, err := CancelRepair(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, noExpectRev, opsWorkerRepName, env.cc)
	if err != nil {
		t.Fatalf("CancelRepair: %v", err)
	}
	if got != repId {
		t.Errorf("CancelRepair = %d, want %d", got, repId)
	}
	for i, legId := range wantRetired {
		if got := env.retired(legId); !proto.Equal(got, retired[i]) {
			t.Errorf("retired leg %d: got %v, want %v", legId, got,
				retired[i])
		}
	}
	grp := findGroup(env.slice(), opsDataGrpId)
	if got := grp.GetRetiredLegIdList(); !equalIds(got, wantRetired) {
		t.Errorf("the group's retired_leg_id_list: got %v, want %v", got,
			wantRetired)
	}
	if got := env.spConf().GetRetiredLegIdList(); !equalIds(
		got, wantRetired) {
		t.Errorf("the pool's retired_leg_id_list: got %v, want %v", got,
			wantRetired)
	}
	if !proto.Equal(env.dn(opsDnC), dnC) || !proto.Equal(env.dn(opsDnD), dnD) ||
		env.dnRev(opsDnC) != dnCRev || env.dnRev(opsDnD) != dnDRev {
		t.Errorf("a retired side's DN moved at the cancel")
	}
	dnE := env.dn(opsDnE)
	if dnE.GetFreeExtCnt() != opsDnFree || len(dnE.GetSidePtrList()) != 0 {
		t.Errorf("dn-e: free %d with pointers %v, want %d and none",
			dnE.GetFreeExtCnt(), dnE.GetSidePtrList(), opsDnFree)
	}
	if env.exists(DnCapacityKey(
		env.cid, mustBin(t, env, charged), charged, opsDnE)) {
		t.Errorf("dn-e: the charged capacity key survived")
	}
	if got := env.dnRev(opsDnE); got != dnERev+1 {
		t.Errorf("dn-e rev: got %d, want %d", got, dnERev+1)
	}
	if env.exists(RepairKey(env.cid, opsSpId, opsWorkerRepName)) {
		t.Errorf("the repair survived its cancel")
	}
	if grp.GetRepName() != "" || len(grp.GetLegList()) != 2 ||
		grp.GetLegList()[1].GetLegId() != opsDataLegB {
		t.Errorf("group: rep_name %q, legs %v", grp.GetRepName(),
			grp.GetLegList())
	}
}

// reuseNewLeg marks the stored repair's new leg as one taken from a retired
// leg, with that leg's old time, and lets edit change the leg's record.
func (e *opsEnv) reuseNewLeg(edit func(leg *pb.Leg)) {
	e.t.Helper()
	rep := e.repair(opsRepName)
	leg := rep.GetNewLegList()[0]
	rep.NewLegIdToRetiredEpoch = map[uint64]uint64{
		leg.GetLegId(): opsRetiredEpoch,
	}
	edit(leg)
	mustPut(e.t, e.cli, RepairKey(e.cid, opsSpId, opsRepName), rep)
}

// TestCancelRepairPreconditions is every refusal of CancelRepair, each with
// nothing written.
func TestCancelRepairPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stage  pb.RepStage
		setup  func(env *opsEnv)
		expect uint64
		reason string
	}{
		{name: "a CREATED repair", stage: pb.RepStage_REP_STAGE_CREATED,
			reason: "repair stage is REP_STAGE_CREATED"},
		{name: "a STARTED repair", stage: pb.RepStage_REP_STAGE_STARTED,
			reason: "repair stage is REP_STAGE_STARTED"},
		{name: "an invalid stored conf", stage: pb.RepStage_REP_STAGE_ABORTED,
			setup:  func(env *opsEnv) { env.cc.DnBinConf.ExtentSize = 0 },
			reason: "invalid stored conf: dn_bin_conf.extent_size is zero"},
		{name: "sp deleting", stage: pb.RepStage_REP_STAGE_ABORTED,
			setup:  func(env *opsEnv) { env.setDeleting() },
			reason: "sp deleting"},
		{name: "repair not found", stage: pb.RepStage_REP_STAGE_ABORTED,
			setup: func(env *opsEnv) {
				conf := env.spConf()
				conf.RepairNameList = nil
				mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
			}, reason: ReasonRepairNotFound},
		{name: "a group that names another repair",
			stage: pb.RepStage_REP_STAGE_ABORTED,
			setup: func(env *opsEnv) {
				slice := env.slice()
				findGroup(slice, opsDataGrpId).RepName = "other"
				mustPut(env.t, env.cli,
					SliceKey(env.cid, opsSpId, opsSliceId), slice)
			}, reason: "group does not name the repair"},
		{name: "a reused new leg with no side",
			stage: pb.RepStage_REP_STAGE_ABORTED,
			setup: func(env *opsEnv) {
				env.reuseNewLeg(func(leg *pb.Leg) { leg.SideList = nil })
			}, reason: "new leg has no single side"},
		{name: "a reused new leg with two sides",
			stage: pb.RepStage_REP_STAGE_ABORTED,
			setup: func(env *opsEnv) {
				env.reuseNewLeg(func(leg *pb.Leg) {
					leg.SideList = append(leg.SideList,
						opsSide(opsRetiredSide2, opsDnD, true))
				})
			}, reason: "new leg has no single side"},
		{name: "stale revision", stage: pb.RepStage_REP_STAGE_ABORTED,
			expect: 99, reason: ReasonStaleRevision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			repId, _ := env.createRepair(func() RepairNewLeg {
				return env.freshLeg(opsDnC)
			})
			if tc.stage != pb.RepStage_REP_STAGE_CREATED {
				env.toStage(opsRepName, repId, tc.stage)
			}
			if tc.setup != nil {
				tc.setup(env)
			}
			before := env.storeRev()
			_, err := CancelRepair(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, tc.expect, opsRepName, env.cc)
			env.wantNoWrite(err, opCancelRepair, tc.reason, before)
		})
	}
}

// TestSetRepairAbort: the flag is set with one SpRev bump and nothing else
// changes; a second call on the CREATED repair answers its rep_id and writes
// nothing; a repair in any other stage is refused with the stage.
func TestSetRepairAbort(t *testing.T) {
	env := newOpsEnv(t)
	repId, _ := env.createRepair(func() RepairNewLeg {
		return env.freshLeg(opsDnC)
	})
	want := env.repair(opsRepName)
	want.TryToAbort = true
	revBefore := env.spRev()
	got, err := SetRepairAbort(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, noExpectRev, opsRepName)
	if err != nil || got != repId {
		t.Fatalf("SetRepairAbort = %d, %v, want %d", got, err, repId)
	}
	// The flag alone moves: the stage, the legs and the parent stay.
	if got := env.repair(opsRepName); !proto.Equal(got, want) {
		t.Errorf("repair: got %v, want %v", got, want)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d", got, revBefore+1)
	}
	before := env.storeRev()
	got, err = SetRepairAbort(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, noExpectRev, opsRepName)
	if err != nil || got != repId {
		t.Fatalf("SetRepairAbort again = %d, %v, want %d", got, err, repId)
	}
	if got := env.storeRev(); got != before {
		t.Errorf("a second abort wrote: store revision %d, want %d",
			got, before)
	}
	for _, stage := range []pb.RepStage{
		pb.RepStage_REP_STAGE_STARTED, pb.RepStage_REP_STAGE_ABORTED,
	} {
		env := newOpsEnv(t)
		repId, _ := env.createRepair(func() RepairNewLeg {
			return env.freshLeg(opsDnC)
		})
		env.toStage(opsRepName, repId, stage)
		before := env.storeRev()
		_, err := SetRepairAbort(env.ctx, env.cli, env.cid, opsShard,
			opsSpId, opsSpName, noExpectRev, opsRepName)
		env.wantNoWrite(err, opSetRepairAbort,
			"repair stage is "+stage.String(), before)
	}
}

// TestSetRepairAbortPreconditions is every other refusal of SetRepairAbort.
func TestSetRepairAbortPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(env *opsEnv)
		repN   string
		expect uint64
		reason string
	}{
		{name: "repair not found", repN: "nope",
			reason: ReasonRepairNotFound},
		{name: "sp deleting", setup: func(env *opsEnv) { env.setDeleting() },
			reason: "sp deleting"},
		{name: "stale revision", expect: 99, reason: ReasonStaleRevision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			env.createRepair(func() RepairNewLeg {
				return env.freshLeg(opsDnC)
			})
			if tc.setup != nil {
				tc.setup(env)
			}
			name := opsRepName
			if tc.repN != "" {
				name = tc.repN
			}
			before := env.storeRev()
			_, err := SetRepairAbort(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, tc.expect, name)
			env.wantNoWrite(err, opSetRepairAbort, tc.reason, before)
		})
	}
}

// TestSetRepairStage: a CREATED repair moves to STARTED whatever its flag
// says, and to ABORTED only while its flag is set, and nothing but the stage
// changes; the move checks the rep_id the verdict named and the stage, and
// takes a pool at any level.
func TestSetRepairStage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(env *opsEnv, repId uint64) uint64
		stage  pb.RepStage
		reason string
	}{
		{name: "to STARTED", stage: pb.RepStage_REP_STAGE_STARTED},
		{name: "to STARTED with the flag set",
			setup: func(env *opsEnv, repId uint64) uint64 {
				if _, err := SetRepairAbort(env.ctx, env.cli, env.cid,
					opsShard, opsSpId, opsSpName, noExpectRev,
					opsRepName); err != nil {
					env.t.Fatalf("SetRepairAbort: %v", err)
				}
				return repId
			}, stage: pb.RepStage_REP_STAGE_STARTED},
		{name: "to ABORTED with the flag set",
			setup: func(env *opsEnv, repId uint64) uint64 {
				if _, err := SetRepairAbort(env.ctx, env.cli, env.cid,
					opsShard, opsSpId, opsSpName, noExpectRev,
					opsRepName); err != nil {
					env.t.Fatalf("SetRepairAbort: %v", err)
				}
				return repId
			}, stage: pb.RepStage_REP_STAGE_ABORTED},
		{name: "at SP_LEVEL_DISABLE",
			setup: func(env *opsEnv, repId uint64) uint64 {
				env.setLevel(pb.SpLevel_SP_LEVEL_DISABLE)
				return repId
			}, stage: pb.RepStage_REP_STAGE_STARTED},
		{name: "to ABORTED with the flag clear",
			stage: pb.RepStage_REP_STAGE_ABORTED, reason: "abort flag not set"},
		{name: "another rep_id",
			setup: func(_ *opsEnv, repId uint64) uint64 {
				return repId + 1
			}, stage: pb.RepStage_REP_STAGE_STARTED,
			reason: "repair id changed"},
		{name: "a STARTED repair",
			setup: func(env *opsEnv, repId uint64) uint64 {
				env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_STARTED)
				return repId
			}, stage: pb.RepStage_REP_STAGE_STARTED,
			reason: "repair stage is REP_STAGE_STARTED"},
		{name: "to CREATED", stage: pb.RepStage_REP_STAGE_CREATED,
			reason: "not a stage move"},
		{name: "sp deleting",
			setup: func(env *opsEnv, repId uint64) uint64 {
				env.setDeleting()
				return repId
			}, stage: pb.RepStage_REP_STAGE_STARTED, reason: "sp deleting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			repId, _ := env.createRepair(func() RepairNewLeg {
				return env.freshLeg(opsDnC)
			})
			verdictId := repId
			if tc.setup != nil {
				verdictId = tc.setup(env, repId)
			}
			want := env.repair(opsRepName)
			want.RepStage = tc.stage
			before, revBefore := env.storeRev(), env.spRev()
			err := SetRepairStage(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, opsRepName, verdictId, tc.stage)
			if tc.reason != "" {
				env.wantNoWrite(err, opSetRepairStage, tc.reason, before)
				return
			}
			if err != nil {
				t.Fatalf("SetRepairStage: %v", err)
			}
			// The stage alone moves: the flag, the legs and the parent stay.
			if got := env.repair(opsRepName); !proto.Equal(got, want) {
				t.Errorf("repair: got %v, want %v", got, want)
			}
			if got := env.spRev(); got != revBefore+1 {
				t.Errorf("SpRev: got %d, want %d", got, revBefore+1)
			}
		})
	}
}

// TestDeleteRetiredLeg: the side gives its DN back the group's ext_cnt and
// its pointer, the record and its two list entries go, and SpRev is bumped
// once; the other retired leg of the group stays.
func TestDeleteRetiredLeg(t *testing.T) {
	env := newOpsEnv(t)
	env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide, opsDnC, 1,
		opsRetiredEpoch)
	env.putRetired(opsDataGrpId, opsRetiredLeg2, opsRetiredSide2, opsDnD, 1,
		opsRetiredEpoch)
	dnCRev, revBefore := env.dnRev(opsDnC), env.spRev()
	if err := DeleteRetiredLeg(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, noExpectRev, opsRetiredLeg, env.cc); err != nil {
		t.Fatalf("DeleteRetiredLeg: %v", err)
	}
	dn := env.dn(opsDnC)
	if dn.GetFreeExtCnt() != opsDnFree || len(dn.GetSidePtrList()) != 0 {
		t.Errorf("dn-c: free %d with pointers %v, want %d and none",
			dn.GetFreeExtCnt(), dn.GetSidePtrList(), opsDnFree)
	}
	if got := env.dnRev(opsDnC); got != dnCRev+1 {
		t.Errorf("dn-c rev: got %d, want %d", got, dnCRev+1)
	}
	if env.exists(RetiredLegKey(env.cid, opsSpId, opsRetiredLeg)) {
		t.Errorf("the retired leg survived its delete")
	}
	if got := env.spConf().GetRetiredLegIdList(); !equalIds(
		got, []uint64{opsRetiredLeg2}) {
		t.Errorf("the pool's retired_leg_id_list: got %v", got)
	}
	if got := findGroup(env.slice(), opsDataGrpId).GetRetiredLegIdList(); !equalIds(got, []uint64{opsRetiredLeg2}) {
		t.Errorf("the group's retired_leg_id_list: got %v", got)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d", got, revBefore+1)
	}
}

// TestDeleteRetiredLegPreconditions is every refusal of DeleteRetiredLeg,
// each with nothing written.
func TestDeleteRetiredLegPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(env *opsEnv)
		legId  uint64
		expect uint64
		reason string
	}{
		{name: "retired leg not found", legId: opsRetiredLeg2,
			reason: ReasonRetiredLegNotFound},
		{name: "retired leg record missing", setup: func(env *opsEnv) {
			env.delKey(RetiredLegKey(env.cid, opsSpId, opsRetiredLeg))
		}, reason: "retired leg record missing"},
		{name: "a group that does not list it", setup: func(env *opsEnv) {
			slice := env.slice()
			findGroup(slice, opsDataGrpId).RetiredLegIdList = nil
			mustPut(env.t, env.cli,
				SliceKey(env.cid, opsSpId, opsSliceId), slice)
		}, reason: "group does not list the retired leg"},
		{name: "an invalid stored conf", setup: func(env *opsEnv) {
			env.cc.DnBinConf.ExtentSize = 0
		}, reason: "invalid stored conf: dn_bin_conf.extent_size is zero"},
		{name: "sp deleting", setup: func(env *opsEnv) { env.setDeleting() },
			reason: "sp deleting"},
		{name: "stale revision", expect: 99, reason: ReasonStaleRevision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide,
				opsDnC, 1, opsRetiredEpoch)
			if tc.setup != nil {
				tc.setup(env)
			}
			legId := opsRetiredLeg
			if tc.legId != 0 {
				legId = tc.legId
			}
			before := env.storeRev()
			err := DeleteRetiredLeg(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, opsSpName, tc.expect, legId, env.cc)
			env.wantNoWrite(err, opDeleteRetiredLeg, tc.reason, before)
		})
	}
}

// TestRepairEpochWrites: the epoch writes of a repair's new leg and side and
// of a retired side follow the rule every epoch write shares — set only onto
// zero, clear always, no write when nothing changes — and bump no revision;
// a stale rep_id, a leg or side the record does not hold, and a pool of
// another id are refused; a pool being deleted is not.
func TestRepairEpochWrites(t *testing.T) {
	env := newOpsEnv(t)
	repId, legId := env.createRepair(func() RepairNewLeg {
		return env.freshLeg(opsDnC)
	})
	sideId := legId + 1
	env.putRetired(opsDataGrpId, opsRetiredLeg, opsRetiredSide, opsDnD, 1,
		opsRetiredEpoch)
	revBefore := env.spRev()
	legEpoch := func() uint64 {
		return env.repair(opsRepName).GetNewLegList()[0].GetErrEpoch()
	}
	sideEpoch := func() uint64 {
		return env.repair(opsRepName).GetNewLegList()[0].GetSideList()[0].
			GetErrEpoch()
	}
	retiredEpoch := func() uint64 {
		return env.retired(opsRetiredLeg).GetSide().GetErrEpoch()
	}
	writes := []struct {
		name  string
		write func(epoch uint64) error
		read  func() uint64
		key   string
	}{
		{"leg", func(epoch uint64) error {
			return SetRepairLegErrEpoch(env.ctx, env.cli, env.cid, opsSpId,
				opsSpName, opsRepName, repId, legId, epoch)
		}, legEpoch, RepairKey(env.cid, opsSpId, opsRepName)},
		{"side", func(epoch uint64) error {
			return SetRepairSideErrEpoch(env.ctx, env.cli, env.cid, opsSpId,
				opsSpName, opsRepName, repId, sideId, epoch)
		}, sideEpoch, RepairKey(env.cid, opsSpId, opsRepName)},
		{"retired side", func(epoch uint64) error {
			return SetRetiredSideErrEpoch(env.ctx, env.cli, env.cid, opsSpId,
				opsSpName, opsRetiredLeg, opsRetiredSide, epoch)
		}, retiredEpoch, RetiredLegKey(env.cid, opsSpId, opsRetiredLeg)},
	}
	for _, w := range writes {
		if err := w.write(100); err != nil || w.read() != 100 {
			t.Fatalf("%s: set: %v, epoch %d", w.name, err, w.read())
		}
		modRev := env.modRev(w.key)
		if err := w.write(200); err != nil || w.read() != 100 {
			t.Errorf("%s: a second set: %v, epoch %d, want 100", w.name,
				err, w.read())
		}
		if got := env.modRev(w.key); got != modRev {
			t.Errorf("%s: a set onto a set epoch wrote", w.name)
		}
		if err := w.write(0); err != nil || w.read() != 0 {
			t.Errorf("%s: clear: %v, epoch %d", w.name, err, w.read())
		}
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("SpRev: got %d, want %d: an epoch write bumps nothing",
			got, revBefore)
	}
	for _, r := range []struct {
		name   string
		op     string
		err    error
		reason string
	}{
		{"another rep_id", opSetRepairLegErrEpoch,
			SetRepairLegErrEpoch(env.ctx, env.cli, env.cid, opsSpId,
				opsSpName, opsRepName, repId+1, legId, 1),
			"repair id changed"},
		{"a leg the repair does not hold", opSetRepairLegErrEpoch,
			SetRepairLegErrEpoch(env.ctx, env.cli, env.cid, opsSpId,
				opsSpName, opsRepName, repId, opsDataLegB, 1),
			"leg not found"},
		{"a side the repair does not hold", opSetRepairSideErrEpoch,
			SetRepairSideErrEpoch(env.ctx, env.cli, env.cid, opsSpId,
				opsSpName, opsRepName, repId, opsDataSideB, 1),
			"side not found"},
		{"another side of the retired leg", opSetRetiredSideErrEpoch,
			SetRetiredSideErrEpoch(env.ctx, env.cli, env.cid, opsSpId,
				opsSpName, opsRetiredLeg, opsRetiredSide+1, 1),
			"side not found"},
		{"a pool of another id", opSetRetiredSideErrEpoch,
			SetRetiredSideErrEpoch(env.ctx, env.cli, env.cid, opsSpId+1,
				opsSpName, opsRetiredLeg, opsRetiredSide, 1),
			"sp id changed"},
	} {
		precondition := wantPrecondition(t, r.err, r.op)
		if precondition.Reason != r.reason {
			t.Errorf("%s: Reason %q, want %q", r.name, precondition.Reason,
				r.reason)
		}
	}
	// A pool being deleted still takes the epoch writes, as the other epoch
	// writes read their record whatever its pool's state
	// (loadSpConfForHealth).
	env.setDeleting()
	for _, w := range writes {
		if err := w.write(300); err != nil || w.read() != 300 {
			t.Errorf("%s: set on a deleting pool: %v, epoch %d", w.name,
				err, w.read())
		}
		if err := w.write(0); err != nil || w.read() != 0 {
			t.Errorf("%s: clear on a deleting pool: %v, epoch %d", w.name,
				err, w.read())
		}
	}
}

// TestFlipRepairProvisioned: the flip of a fresh new leg's side sets it
// provisioned with one SpRev bump and reports the revision it committed; a
// new leg or a side the repair does not hold, a side already provisioned, a
// stale rep_id and a repair that is gone are skipped with nothing written; a
// pool being deleted is refused.
func TestFlipRepairProvisioned(t *testing.T) {
	env := newOpsEnv(t)
	repId, legId := env.createRepair(func() RepairNewLeg {
		return env.freshLeg(opsDnC)
	})
	ref := RepairSideRef{
		RepName: opsRepName, RepId: repId, LegId: legId, SideId: legId + 1,
	}
	// A new leg or a side the repair does not hold is skipped, never
	// refused, and leaves the repair's own side as it is: these run while
	// that side is not provisioned yet, so a flip of the wrong side shows.
	otherLeg := ref
	otherLeg.LegId = legId + 100
	otherSide := ref
	otherSide.SideId = legId + 100
	for name, r := range map[string]RepairSideRef{
		"a leg the repair does not hold":   otherLeg,
		"a side the new leg does not hold": otherSide,
	} {
		before := env.storeRev()
		flipped, revision, err := FlipRepairProvisioned(env.ctx, env.cli,
			env.cid, opsShard, opsSpId, opsSpName, r)
		if err != nil || flipped || revision != 0 {
			t.Errorf("%s: FlipRepairProvisioned = %v, %d, %v, want a skip",
				name, flipped, revision, err)
		}
		if got := env.storeRev(); got != before {
			t.Errorf("%s: a skipped flip wrote", name)
		}
	}
	revBefore := env.spRev()
	flipped, revision, err := FlipRepairProvisioned(env.ctx, env.cli,
		env.cid, opsShard, opsSpId, opsSpName, ref)
	if err != nil || !flipped || revision != revBefore+1 {
		t.Fatalf("FlipRepairProvisioned = %v, %d, %v, want true, %d",
			flipped, revision, err, revBefore+1)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d", got, revBefore+1)
	}
	side := env.repair(opsRepName).GetNewLegList()[0].GetSideList()[0]
	if !side.GetProvisioned() {
		t.Errorf("the side is not provisioned: %v", side)
	}
	stale := ref
	stale.RepId++
	gone := ref
	gone.RepName = "gone"
	for name, r := range map[string]RepairSideRef{
		"provisioned": ref, "stale rep_id": stale, "gone": gone,
	} {
		before := env.storeRev()
		flipped, revision, err := FlipRepairProvisioned(env.ctx, env.cli,
			env.cid, opsShard, opsSpId, opsSpName, r)
		if err != nil || flipped || revision != 0 {
			t.Errorf("%s: FlipRepairProvisioned = %v, %d, %v, want a skip",
				name, flipped, revision, err)
		}
		if got := env.storeRev(); got != before {
			t.Errorf("%s: a skipped flip wrote", name)
		}
	}
	env.setDeleting()
	before := env.storeRev()
	_, _, err = FlipRepairProvisioned(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, ref)
	env.wantNoWrite(err, opFlipRepairProvisioned, "sp deleting", before)
}

// TestRepairOfACloneMetaGroup is a repair through a clone parent: the clone's
// group carries the rep_name and the retired leg, the repair and the retired
// leg name the clone, and each operation writes the clone's record where a
// slice's group would have its slice's. A clone being deleted is refused by
// the abort, the stage move and the delete, and skipped by the flip.
func TestRepairOfACloneMetaGroup(t *testing.T) {
	env := newOpsEnv(t)
	env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(true), false)
	clone := env.clone(opsCloneName)
	findCloneLeg(clone, opsCloneLegB).ErrEpoch = opsBadEpoch
	mustPut(t, env.cli, CloneKey(env.cid, opsSpId, opsCloneName), clone)
	parent := GrpParent{CloneName: opsCloneName}
	repId, legId, err := CreateRepair(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, noExpectRev, opsRepName, parent, opsCloneGrpId,
		opsCloneLegB, RepairNewLeg{
			Dn: env.dnCand(opsDnA), GrpDnAddrs: env.cloneGrpDnAddrs(),
		}, env.cc)
	if err != nil {
		t.Fatalf("CreateRepair of the clone's group: %v", err)
	}
	rep := env.repair(opsRepName)
	if rep.GetCloneName() != opsCloneName || rep.GetSliceId() != 0 {
		t.Errorf("the repair's parent: slice %d, clone %q",
			rep.GetSliceId(), rep.GetCloneName())
	}
	if got := env.clone(opsCloneName).GetMetaGrp().GetRepName(); got !=
		opsRepName {
		t.Errorf("the clone's rep_name: got %q", got)
	}
	ref := RepairSideRef{
		RepName: opsRepName, RepId: repId, LegId: legId, SideId: legId + 1,
	}
	if flipped, _, err := FlipRepairProvisioned(env.ctx, env.cli, env.cid,
		opsShard, opsSpId, opsSpName, ref); err != nil || !flipped {
		t.Fatalf("FlipRepairProvisioned: %v, %v", flipped, err)
	}
	env.toStage(opsRepName, repId, pb.RepStage_REP_STAGE_STARTED)
	if _, err := FinishRepair(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, noExpectRev, opsRepName, opsFinishNow,
		env.cc); err != nil {
		t.Fatalf("FinishRepair: %v", err)
	}
	grp := env.clone(opsCloneName).GetMetaGrp()
	if grp.GetLegList()[1].GetLegId() != legId || grp.GetRepName() != "" ||
		!equalIds(grp.GetRetiredLegIdList(), []uint64{opsCloneLegB}) {
		t.Errorf("the clone's group after the finish: %v", grp)
	}
	retired := env.retired(opsCloneLegB)
	if retired.GetCloneName() != opsCloneName || retired.GetSliceId() != 0 {
		t.Errorf("the retired leg's parent: %v", retired)
	}

	// A second repair, then the clone latched.
	env.setLegErrClone(legId, opsBadEpoch)
	repId2, legId2, err := CreateRepair(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, noExpectRev, "rep1", parent, opsCloneGrpId,
		legId, RepairNewLeg{RetiredLegId: opsCloneLegB}, env.cc)
	if err != nil {
		t.Fatalf("CreateRepair from the clone's retired leg: %v", err)
	}
	// The reused side unprovisioned, so only the clone's latch can make the
	// flip skip it.
	rep1 := env.repair("rep1")
	rep1.GetNewLegList()[0].GetSideList()[0].Provisioned = false
	mustPut(t, env.cli, RepairKey(env.cid, opsSpId, "rep1"), rep1)
	clone = env.clone(opsCloneName)
	clone.Deleting = true
	mustPut(t, env.cli, CloneKey(env.cid, opsSpId, opsCloneName), clone)
	before := env.storeRev()
	_, err = SetRepairAbort(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, noExpectRev, "rep1")
	env.wantNoWrite(err, opSetRepairAbort, "clone deleting", before)
	err = SetRepairStage(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, "rep1", repId2, pb.RepStage_REP_STAGE_STARTED)
	env.wantNoWrite(err, opSetRepairStage, "clone deleting", before)
	flipped, _, err := FlipRepairProvisioned(env.ctx, env.cli, env.cid,
		opsShard, opsSpId, opsSpName, RepairSideRef{
			RepName: "rep1", RepId: repId2, LegId: legId2,
			SideId: opsCloneSideB,
		})
	if err != nil || flipped {
		t.Errorf("a deleting clone's flip: %v, %v, want a skip", flipped, err)
	}
	if got := env.storeRev(); got != before {
		t.Errorf("a deleting clone's flip wrote")
	}
}

// setLegErrClone stamps one leg of the fixture clone's group directly.
func (e *opsEnv) setLegErrClone(legId uint64, epoch uint64) {
	e.t.Helper()
	clone := e.clone(opsCloneName)
	findCloneLeg(clone, legId).ErrEpoch = epoch
	mustPut(e.t, e.cli, CloneKey(e.cid, opsSpId, opsCloneName), clone)
}

// TestGrpDnLegsCountTheRepairsNewLegs pins the placement helpers' disk-node
// set: a repair's new leg is in it, for the gain check and the address check
// alike, since it becomes a leg of the group at the finish.
func TestGrpDnLegsCountTheRepairsNewLegs(t *testing.T) {
	grp := opsSlice().GetDataGrpList()[0]
	rep := &pb.Repair{NewLegList: []*pb.Leg{{
		LegId: 9, SideList: []*pb.Side{opsSide(10, opsDnC, false)},
	}}}
	if grpHostsAddr(grp, nil, opsDnC) || !grpHostsAddr(grp, rep, opsDnC) {
		t.Errorf("grpHostsAddr does not count the repair's new leg alone")
	}
	planned := map[string]struct{}{opsDnA: {}, opsDnB: {}}
	if grpGainedDn(grp, nil, planned) || !grpGainedDn(grp, rep, planned) {
		t.Errorf("grpGainedDn does not count the repair's new leg alone")
	}
}

// TestRepairReasonsAreDistinct keeps the exported reasons apart from each
// other and from the ones the gateway maps already, so that a mapping by
// string never answers one for another.
func TestRepairReasonsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, reason := range []string{
		ReasonRepairExists, ReasonRepairNotFound, ReasonRetiredLegNotFound,
		ReasonRetiredListFull, ReasonGrpHasRepair, ReasonStaleRevision,
		ReasonCandidateChanged, ReasonNoIdLeft,
	} {
		if seen[reason] {
			t.Errorf("reason %q is used twice", reason)
		}
		seen[reason] = true
	}
	if common.MaxRetiredLegPerGrp < 1 {
		t.Errorf("MaxRetiredLegPerGrp %d leaves no room for a finish",
			common.MaxRetiredLegPerGrp)
	}
}

// TestRepairOpsRefuseADeletingClone: the finish, the cancel and the delete of
// a retired leg refuse a clone meta group's records once the clone is being
// deleted, with nothing written; the clone drain's final transaction gives
// them back.
func TestRepairOpsRefuseADeletingClone(t *testing.T) {
	env := newOpsEnv(t)
	env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(true), true)
	env.putCloneRepair("started", 2000)
	clone := env.clone(opsCloneName)
	clone.MetaGrp.RepName = "started"
	clone.MetaGrp.RetiredLegIdList = []uint64{opsRetiredLeg}
	mustPut(t, env.cli, CloneKey(env.cid, opsSpId, opsCloneName), clone)
	rep := env.repair("started")
	rep.RepStage = pb.RepStage_REP_STAGE_STARTED
	mustPut(t, env.cli, RepairKey(env.cid, opsSpId, "started"), rep)
	conf := env.spConf()
	conf.RetiredLegIdList = []uint64{opsRetiredLeg}
	mustPut(t, env.cli, SpConfKey(env.cid, opsSpName), conf)
	mustPut(t, env.cli, RetiredLegKey(env.cid, opsSpId, opsRetiredLeg),
		&pb.RetiredLeg{
			GrpId: opsCloneGrpId, LegIdx: 1,
			Side:      opsSide(opsRetiredSide, opsDnA, true),
			CloneName: opsCloneName,
		})
	before := env.storeRev()
	_, err := FinishRepair(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, noExpectRev, "started", opsFinishNow, env.cc)
	env.wantNoWrite(err, opFinishRepair, "clone deleting", before)
	rep.RepStage = pb.RepStage_REP_STAGE_ABORTED
	mustPut(t, env.cli, RepairKey(env.cid, opsSpId, "started"), rep)
	before = env.storeRev()
	_, err = CancelRepair(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, noExpectRev, "started", env.cc)
	env.wantNoWrite(err, opCancelRepair, "clone deleting", before)
	err = DeleteRetiredLeg(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, noExpectRev, opsRetiredLeg, env.cc)
	env.wantNoWrite(err, opDeleteRetiredLeg, "clone deleting", before)
}
