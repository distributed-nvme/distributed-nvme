package model

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The operator-in-charge flag on the MD6 fixture: SetGrpOperatorInCharge's
// write and clear of the data group's flag, its refusals, its levels and its
// clone meta group's twin through the parent, and the flag's one effect in
// model, CreateRepair's refusal of the sp worker's repair under it
// (architecture.md, Repairs). Every refusal and every write of an equal value
// is checked to have written nothing at all, by the store's revision, and
// every write to have left SpRev where it was.

// setInCharge is SetGrpOperatorInCharge on a group of the fixture slice with
// no expected revision.
func (e *opsEnv) setInCharge(grpId uint64, on bool) error {
	e.t.Helper()
	return SetGrpOperatorInCharge(e.ctx, e.cli, e.cid, opsShard, opsSpId,
		opsSpName, noExpectRev, opsSliceParent, grpId, on)
}

// mustSetInCharge is setInCharge failing the test on an error.
func (e *opsEnv) mustSetInCharge(grpId uint64, on bool) {
	e.t.Helper()
	if err := e.setInCharge(grpId, on); err != nil {
		e.t.Fatalf("SetGrpOperatorInCharge group %d to %v: %v", grpId, on,
			err)
	}
}

// inCharge is the operator_in_charge flag of a group of the fixture slice.
func (e *opsEnv) inCharge(grpId uint64) bool {
	e.t.Helper()
	return findGroup(e.slice(), grpId).GetOperatorInCharge()
}

// putOperatorInCharge sets the flag of a group of the fixture slice directly,
// bypassing the op, and names repName as its repair when it is not empty.
func (e *opsEnv) putOperatorInCharge(grpId uint64, repName string) {
	e.t.Helper()
	slice := e.slice()
	grp := findGroup(slice, grpId)
	grp.OperatorInCharge = true
	if repName != "" {
		grp.RepName = repName
	}
	mustPut(e.t, e.cli, SliceKey(e.cid, opsSpId, opsSliceId), slice)
}

// TestSetGrpOperatorInCharge: the write sets the group's flag with nothing
// else of the slice changed, and the clear clears it, neither raising SpRev;
// a write of the value the group already holds, set or clear, writes nothing
// at all.
func TestSetGrpOperatorInCharge(t *testing.T) {
	env := newOpsEnv(t)
	want := env.slice()
	revBefore := env.spRev()
	env.mustSetInCharge(opsDataGrpId, true)
	findGroup(want, opsDataGrpId).OperatorInCharge = true
	if got := env.slice(); !proto.Equal(got, want) {
		t.Errorf("slice: got %v, want %v", got, want)
	}
	if env.inCharge(opsMetaGrpId) {
		t.Errorf("the meta group carries the flag")
	}
	if env.spRev() != revBefore {
		t.Errorf("SpRev after the write: got %d, want %d", env.spRev(),
			revBefore)
	}
	storeBefore := env.storeRev()
	env.mustSetInCharge(opsDataGrpId, true)
	if env.storeRev() != storeBefore {
		t.Errorf("a repeated write wrote: store revision %d, want %d",
			env.storeRev(), storeBefore)
	}
	env.mustSetInCharge(opsDataGrpId, false)
	if env.inCharge(opsDataGrpId) {
		t.Errorf("the clear left the flag")
	}
	if env.spRev() != revBefore {
		t.Errorf("SpRev after the clear: got %d, want %d", env.spRev(),
			revBefore)
	}
	storeBefore = env.storeRev()
	env.mustSetInCharge(opsDataGrpId, false)
	if env.storeRev() != storeBefore {
		t.Errorf("a repeated clear wrote: store revision %d, want %d",
			env.storeRev(), storeBefore)
	}
}

// TestSetGrpOperatorInChargeTakesTheToken: a matching expected revision
// writes the flag, and, since no write of the flag raises SpRev, the same
// token clears it again; a stale one is refused first, on a write and on a
// clear alike, and so is one on a write that would change nothing.
func TestSetGrpOperatorInChargeTakesTheToken(t *testing.T) {
	env := newOpsEnv(t)
	token := env.spRev()
	if err := SetGrpOperatorInCharge(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, token, opsSliceParent, opsDataGrpId,
		true); err != nil || !env.inCharge(opsDataGrpId) {
		t.Fatalf("the write with the token: %v, flag %v", err,
			env.inCharge(opsDataGrpId))
	}
	for _, on := range []bool{true, false} {
		before := env.storeRev()
		err := SetGrpOperatorInCharge(env.ctx, env.cli, env.cid, opsShard,
			opsSpId, opsSpName, token+9, opsSliceParent, opsDataGrpId, on)
		env.wantNoWrite(err, opSetGrpOperatorInCharge, ReasonStaleRevision,
			before)
	}
	if err := SetGrpOperatorInCharge(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, token, opsSliceParent, opsDataGrpId,
		false); err != nil || env.inCharge(opsDataGrpId) {
		t.Errorf("the clear with the same token: %v, flag %v", err,
			env.inCharge(opsDataGrpId))
	}
}

// TestSetGrpOperatorInChargePreconditions is every refusal of
// SetGrpOperatorInCharge, each with nothing written: the call sets the data
// group's flag, and the case's setup changes the store or the call. The
// token comes first, then the pool's checks, then the redundancy, then the
// group's look-up.
func TestSetGrpOperatorInChargePreconditions(t *testing.T) {
	type call struct {
		expectRev uint64
		spName    string
		parent    GrpParent
		grpId     uint64
	}
	redundNone := func(env *opsEnv) {
		conf := env.spConf()
		conf.BdevConf.RedundConf = &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundNone{
				RedundNone: &pb.RedundNone{},
			},
		}
		mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
	}
	for _, tc := range []struct {
		name   string
		setup  func(env *opsEnv, c *call)
		reason string
	}{
		{"stale revision", func(_ *opsEnv, c *call) {
			c.expectRev = 99
		}, ReasonStaleRevision},
		{"stale revision on a deleting pool", func(env *opsEnv, c *call) {
			env.setDeleting()
			c.expectRev = 99
		}, ReasonStaleRevision},
		{"sp not found", func(_ *opsEnv, c *call) {
			c.spName = "no-such-pool"
		}, "sp not found"},
		{"sp id changed", func(env *opsEnv, _ *call) {
			conf := env.spConf()
			conf.SpId = opsSpId + 1
			mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
		}, "sp id changed"},
		{"sp deleting", func(env *opsEnv, _ *call) {
			env.setDeleting()
		}, "sp deleting"},
		{"sp deleting with no redundancy", func(env *opsEnv, _ *call) {
			redundNone(env)
			env.setDeleting()
		}, "sp deleting"},
		{"group is not md-raid1", func(env *opsEnv, _ *call) {
			redundNone(env)
		}, "group is not md-raid1"},
		{"no redundancy and a group that is gone",
			func(env *opsEnv, c *call) {
				redundNone(env)
				c.grpId = 9999
			}, "group is not md-raid1"},
		{"slice not found", func(_ *opsEnv, c *call) {
			c.parent = GrpParent{SliceId: 9999}
		}, "slice not found"},
		{"both parents", func(_ *opsEnv, c *call) {
			c.parent = GrpParent{SliceId: opsSliceId, CloneName: "c"}
		}, "no single parent"},
		{"no parent", func(_ *opsEnv, c *call) {
			c.parent = GrpParent{}
		}, "no single parent"},
		{"clone not found", func(_ *opsEnv, c *call) {
			c.parent = GrpParent{CloneName: opsCloneName}
		}, "clone not found"},
		{"group not found", func(_ *opsEnv, c *call) {
			c.grpId = 9999
		}, "group not found"},
		{"a group of another parent", func(_ *opsEnv, c *call) {
			c.grpId = opsCloneGrpId
		}, "group not found"},
		{"clone deleting", func(env *opsEnv, c *call) {
			env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(true), true)
			c.parent = GrpParent{CloneName: opsCloneName}
			c.grpId = opsCloneGrpId
		}, "clone deleting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			c := &call{
				spName: opsSpName,
				parent: opsSliceParent,
				grpId:  opsDataGrpId,
			}
			tc.setup(env, c)
			before := env.storeRev()
			err := SetGrpOperatorInCharge(env.ctx, env.cli, env.cid, opsShard,
				opsSpId, c.spName, c.expectRev, c.parent, c.grpId, true)
			env.wantNoWrite(err, opSetGrpOperatorInCharge, tc.reason, before)
		})
	}
}

// TestSetGrpOperatorInChargeAtEveryLevel: the write and the clear take every
// sp_level, the levels where reactions stop and those where no group array is
// built included, so an operator sets the flag before lowering a raised
// level.
func TestSetGrpOperatorInChargeAtEveryLevel(t *testing.T) {
	env := newOpsEnv(t)
	for _, level := range []pb.SpLevel{
		pb.SpLevel_SP_LEVEL_READWRITE, pb.SpLevel_SP_LEVEL_READONLY,
		pb.SpLevel_SP_LEVEL_NO_CLONE, pb.SpLevel_SP_LEVEL_NO_THINPOOL,
		pb.SpLevel_SP_LEVEL_NO_REDUND, pb.SpLevel_SP_LEVEL_NO_MIGRATION,
		pb.SpLevel_SP_LEVEL_NO_SIDE, pb.SpLevel_SP_LEVEL_DISABLE,
	} {
		env.setLevel(level)
		if err := env.setInCharge(opsDataGrpId, true); err != nil ||
			!env.inCharge(opsDataGrpId) {
			t.Errorf("the write at %v: %v, flag %v", level, err,
				env.inCharge(opsDataGrpId))
		}
		if err := env.setInCharge(opsDataGrpId, false); err != nil ||
			env.inCharge(opsDataGrpId) {
			t.Errorf("the clear at %v: %v, flag %v", level, err,
				env.inCharge(opsDataGrpId))
		}
	}
}

// TestSetGrpOperatorInChargeOfACloneMetaGroup is the flag through a clone
// parent: the write puts it on the clone's record where a slice's group would
// have its slice's, leaves the slice alone, raises no SpRev, and the clear
// takes it off again.
func TestSetGrpOperatorInChargeOfACloneMetaGroup(t *testing.T) {
	env := newOpsEnv(t)
	env.putGrpClone(opsCloneName, opsCloneId, opsCloneGrp(true), false)
	parent := GrpParent{CloneName: opsCloneName}
	slice := env.slice()
	revBefore := env.spRev()
	if err := SetGrpOperatorInCharge(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, noExpectRev, parent, opsCloneGrpId,
		true); err != nil {
		t.Fatalf("the write of the clone's group: %v", err)
	}
	if !env.clone(opsCloneName).GetMetaGrp().GetOperatorInCharge() {
		t.Errorf("the clone's group does not carry the flag")
	}
	if got := env.slice(); !proto.Equal(got, slice) {
		t.Errorf("the clone's flag wrote the slice: %v", got)
	}
	if env.spRev() != revBefore {
		t.Errorf("SpRev after the clone's write: got %d, want %d",
			env.spRev(), revBefore)
	}
	if err := SetGrpOperatorInCharge(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, noExpectRev, parent, opsCloneGrpId,
		false); err != nil {
		t.Fatalf("the clear of the clone's group: %v", err)
	}
	if env.clone(opsCloneName).GetMetaGrp().GetOperatorInCharge() {
		t.Errorf("the clear left the clone's flag")
	}
}

// TestCreateRepairUnderTheOperatorInChargeFlag: under the group's flag a
// create of the sp worker's repair is refused with nothing written, while an
// operator's repair of the same leg is created; once the flag is cleared the
// worker's repair is created again.
func TestCreateRepairUnderTheOperatorInChargeFlag(t *testing.T) {
	env := newOpsEnv(t)
	env.setLegErr(opsDataLegB, opsBadEpoch)
	env.mustSetInCharge(opsDataGrpId, true)
	worker := &repairCall{
		name:   opsWorkerRepName,
		parent: opsSliceParent,
		grpId:  opsDataGrpId,
		badLeg: opsDataLegB,
		newLeg: env.freshLeg(opsDnC),
	}
	before := env.storeRev()
	env.wantNoWrite(worker.run(env), opCreateRepair,
		ReasonGrpOperatorInCharge, before)
	env.createRepair(func() RepairNewLeg {
		return env.freshLeg(opsDnC)
	})
	if got := findGroup(env.slice(), opsDataGrpId); got.GetRepName() !=
		opsRepName || !got.GetOperatorInCharge() {
		t.Errorf("the operator's create under the flag: rep_name %q, flag "+
			"%v, want %q and the flag kept", got.GetRepName(),
			got.GetOperatorInCharge(), opsRepName)
	}

	env = newOpsEnv(t)
	env.setLegErr(opsDataLegB, opsBadEpoch)
	env.mustSetInCharge(opsDataGrpId, true)
	worker.newLeg = env.freshLeg(opsDnC)
	before = env.storeRev()
	env.wantNoWrite(worker.run(env), opCreateRepair,
		ReasonGrpOperatorInCharge, before)
	env.mustSetInCharge(opsDataGrpId, false)
	if err := worker.run(env); err != nil {
		t.Fatalf("the worker's create once the flag is cleared: %v", err)
	}
	if got := findGroup(env.slice(), opsDataGrpId).GetRepName(); got !=
		opsWorkerRepName {
		t.Errorf("rep_name: got %q, want %q", got, opsWorkerRepName)
	}
}

// TestWorkersRepairMovesOnUnderTheFlag: the flag stops the create alone. A
// worker's repair that exists when the flag is set is aborted, moved and
// cancelled, or moved and finished, by the same ops as without it, and each
// leaves the flag standing for the operator to clear.
func TestWorkersRepairMovesOnUnderTheFlag(t *testing.T) {
	for _, stage := range []pb.RepStage{
		pb.RepStage_REP_STAGE_ABORTED, pb.RepStage_REP_STAGE_STARTED,
	} {
		t.Run(stage.String(), func(t *testing.T) {
			env := newOpsEnv(t)
			repId, _ := env.createRepairNamed(opsWorkerRepName,
				func() RepairNewLeg { return env.freshLeg(opsDnC) })
			env.mustSetInCharge(opsDataGrpId, true)
			env.toStage(opsWorkerRepName, repId, stage)
			var err error
			if stage == pb.RepStage_REP_STAGE_ABORTED {
				_, err = CancelRepair(env.ctx, env.cli, env.cid, opsShard,
					opsSpId, opsSpName, noExpectRev, opsWorkerRepName,
					env.cc)
			} else {
				_, err = FinishRepair(env.ctx, env.cli, env.cid, opsShard,
					opsSpId, opsSpName, noExpectRev, opsWorkerRepName,
					opsFinishNow, env.cc)
			}
			if err != nil {
				t.Fatalf("the worker's repair under the flag: %v", err)
			}
			grp := findGroup(env.slice(), opsDataGrpId)
			if grp.GetRepName() != "" || !grp.GetOperatorInCharge() {
				t.Errorf("after the end of the repair: rep_name %q, flag "+
					"%v, want none and the flag kept", grp.GetRepName(),
					grp.GetOperatorInCharge())
			}
		})
	}
}
