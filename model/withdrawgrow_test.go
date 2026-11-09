package model

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestWithdrawGrowUndoesTheGrow pins WithdrawGrow's effects (MD6;
// dnv-worker.md AR6): after a grow whose sides never provisioned, the take
// back leaves the slice, both picked DNs and both CNs as they were before the
// grow, but for one revision bump each, and bumps SpRev.
func TestWithdrawGrowUndoesTheGrow(t *testing.T) {
	env := newOpsEnv(t)
	dnC, dnD := &pb.DnConf{}, &pb.DnConf{}
	cnA, cnB := &pb.CnConf{}, &pb.CnConf{}
	env.get(DnConfKey(env.cid, opsDnC), dnC)
	env.get(DnConfKey(env.cid, opsDnD), dnD)
	env.get(CnConfKey(env.cid, opsCnA), cnA)
	env.get(CnConfKey(env.cid, opsCnB), cnB)
	sliceBefore := env.slice()
	grpId, err := GrowSlice(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, false, opsNotPending, env.cc,
		[]Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)},
	)
	if err != nil {
		t.Fatalf("GrowSlice: %v", err)
	}
	// One leg's side provisioned, the other's not: the disk node of the
	// second died while it zeroed, so no cntlr built the group.
	grown := env.slice()
	grown.GetDataGrpList()[1].GetLegList()[0].GetSideList()[0].Provisioned = true
	mustPut(t, env.cli, SliceKey(env.cid, opsSpId, opsSliceId), grown)
	spRev := env.spRev()
	// The pool still reports the slice's old total: the grow is pending.
	pending := sliceBefore.GetDataGrpList()[0].GetDataBlocks()
	got, err := WithdrawGrow(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, false, grpId, pending, env.cc,
	)
	if err != nil {
		t.Fatalf("WithdrawGrow: %v", err)
	}
	if got != grpId {
		t.Errorf("grp_id: got %d, want %d", got, grpId)
	}
	if !proto.Equal(env.slice(), sliceBefore) {
		t.Errorf("slice after the withdrawal:\n got %v\nwant %v",
			env.slice(), sliceBefore)
	}
	for _, tc := range []struct {
		addr string
		want *pb.DnConf
	}{{opsDnC, dnC}, {opsDnD, dnD}} {
		after := &pb.DnConf{}
		env.get(DnConfKey(env.cid, tc.addr), after)
		if after.GetFreeExtCnt() != tc.want.GetFreeExtCnt() {
			t.Errorf("%s free_ext_cnt: got %d, want %d", tc.addr,
				after.GetFreeExtCnt(), tc.want.GetFreeExtCnt())
		}
		if len(after.GetSidePtrList()) != len(tc.want.GetSidePtrList()) {
			t.Errorf("%s side pointers: got %v, want %v", tc.addr,
				after.GetSidePtrList(), tc.want.GetSidePtrList())
		}
	}
	for _, tc := range []struct {
		addr string
		want *pb.CnConf
	}{{opsCnA, cnA}, {opsCnB, cnB}} {
		after := &pb.CnConf{}
		env.get(CnConfKey(env.cid, tc.addr), after)
		if after.GetFreeExtCnt() != tc.want.GetFreeExtCnt() {
			t.Errorf("%s free_ext_cnt: got %d, want %d", tc.addr,
				after.GetFreeExtCnt(), tc.want.GetFreeExtCnt())
		}
	}
	if env.spRev() != spRev+1 {
		t.Errorf("sp_rev: got %d, want %d", env.spRev(), spRev+1)
	}
}

// TestWithdrawGrowPreconditions pins WithdrawGrow's refusals, each writing
// nothing: a group that is not the newest, a grow no longer pending, every
// side provisioned, and a group a repair, a retired leg, a forced-start mark
// or an operator acts on.
func TestWithdrawGrowPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(env *opsEnv, grp *pb.Group)
		grpId  func(grp *pb.Group) uint64
		total  func(env *opsEnv, before *pb.Slice) uint64
		reason string
	}{
		{name: "not the newest group",
			grpId:  func(grp *pb.Group) uint64 { return grp.GetGrpId() + 99 },
			reason: "not the newest group"},
		{name: "grow not pending",
			total:  func(*opsEnv, *pb.Slice) uint64 { return opsNotPending },
			reason: "grow not pending"},
		{name: "no leg provisioning",
			edit: func(_ *opsEnv, grp *pb.Group) {
				for _, leg := range grp.GetLegList() {
					leg.GetSideList()[0].Provisioned = true
				}
			},
			reason: "no leg provisioning"},
		{name: "a leg with two sides",
			edit: func(_ *opsEnv, grp *pb.Group) {
				leg := grp.GetLegList()[0]
				leg.SideList = append(leg.SideList, &pb.Side{
					SideId: 9999, AddrPort: opsDnC,
				})
			},
			reason: "a leg has two sides"},
		{name: "a repair",
			edit:   func(_ *opsEnv, grp *pb.Group) { grp.RepName = "r0" },
			reason: ReasonGrpHasRepair},
		{name: "a retired leg",
			edit: func(_ *opsEnv, grp *pb.Group) {
				grp.RetiredLegIdList = []uint64{9999}
			},
			reason: "group has a retired leg"},
		{name: "a forced-start mark",
			edit: func(_ *opsEnv, grp *pb.Group) {
				grp.ForcedStart = &pb.ForcedStart{LegId: 1, Revision: 1}
			},
			reason: "group has a forced-start mark"},
		{name: "an operator in charge",
			edit:   func(_ *opsEnv, grp *pb.Group) { grp.OperatorInCharge = true },
			reason: ReasonGrpOperatorInCharge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			before := env.slice()
			grpId, err := GrowSlice(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
				noExpectRev, opsSliceId, false, opsNotPending, env.cc,
				[]Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)},
			)
			if err != nil {
				t.Fatalf("GrowSlice: %v", err)
			}
			slice := env.slice()
			grp := slice.GetDataGrpList()[len(slice.GetDataGrpList())-1]
			if tc.edit != nil {
				tc.edit(env, grp)
				mustPut(t, env.cli, SliceKey(env.cid, opsSpId, opsSliceId), slice)
			}
			id := grpId
			if tc.grpId != nil {
				id = tc.grpId(grp)
			}
			total := before.GetDataGrpList()[0].GetDataBlocks()
			if tc.total != nil {
				total = tc.total(env, before)
			}
			rev := env.storeRev()
			_, err = WithdrawGrow(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
				noExpectRev, opsSliceId, false, id, total, env.cc,
			)
			var pre *ErrPrecondition
			if !errors.As(err, &pre) || pre.Reason != tc.reason {
				t.Fatalf("err = %v, want the refusal %q", err, tc.reason)
			}
			if env.storeRev() != rev {
				t.Errorf("a refusal wrote")
			}
		})
	}
}

// TestWithdrawGrowOfAMetaGroup pins the kind: a metadata group is withdrawn
// off the metadata list, and the data list is left as it is.
func TestWithdrawGrowOfAMetaGroup(t *testing.T) {
	env := newOpsEnv(t)
	sliceBefore := env.slice()
	grpId, err := GrowSlice(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, true, opsNotPending, env.cc,
		[]Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)},
	)
	if err != nil {
		t.Fatalf("GrowSlice: %v", err)
	}
	if _, err := WithdrawGrow(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, true, grpId, 0, env.cc,
	); err != nil {
		t.Fatalf("WithdrawGrow: %v", err)
	}
	if !proto.Equal(env.slice(), sliceBefore) {
		t.Errorf("slice after the withdrawal:\n got %v\nwant %v",
			env.slice(), sliceBefore)
	}
}

// TestWithdrawGrowRefusesASliceWithNoGrownGroup pins the first guard: a list
// of one group holds no grown group to withdraw.
func TestWithdrawGrowRefusesASliceWithNoGrownGroup(t *testing.T) {
	env := newOpsEnv(t)
	grp := env.slice().GetDataGrpList()[0]
	_, err := WithdrawGrow(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, false, grp.GetGrpId(), 0, env.cc,
	)
	var pre *ErrPrecondition
	if !errors.As(err, &pre) || pre.Reason != "no grown group" {
		t.Fatalf("err = %v, want the refusal \"no grown group\"", err)
	}
}
