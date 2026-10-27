package gateway

import (
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// UpdateGroupOperatorInCharge (architecture.md, Repairs; gateway.md, Repairs
// and retired legs), on the fixture of handler_vol_test.go. Every write is
// asserted against the store and an SpRev that did not move, and every
// refusal and every write of the value a group already holds by a store
// whose revision did not move, a refusal by its code and its message too.

// oicRequest is the write of the data group's flag to on under the current
// token.
func oicRequest(env *volEnv, on bool) *pb.UpdateGroupOperatorInChargeRequest {
	env.t.Helper()
	return &pb.UpdateGroupOperatorInChargeRequest{
		ClusterName:      env.cluster,
		SpName:           volSpName,
		SpRev:            env.token(),
		GrpId:            volDataGrpId,
		OperatorInCharge: on,
	}
}

// oicFlag is the operator_in_charge flag of a group of the fixture slice.
func oicFlag(env *volEnv, grpId uint64) bool {
	env.t.Helper()
	return volGrpOf(env.t, env.slice(), grpId).GetOperatorInCharge()
}

// oicPutFlag plants the data group's flag, as a write of it leaves it.
func oicPutFlag(env *volEnv, on bool) {
	env.t.Helper()
	slice := env.slice()
	volGrpOf(env.t, slice, volDataGrpId).OperatorInCharge = on
	env.putSlice(slice)
}

// volStoreRev is the store's revision, which etcd moves only for a
// transaction that wrote: a put of a value a key already holds moves it,
// which a comparison of the values does not see.
func volStoreRev(env *volEnv) int64 {
	env.t.Helper()
	_, rev, err := env.cli.Range(env.ctx, common.DnvPrefix)
	if err != nil {
		env.t.Fatalf("Range: %v", err)
	}
	return rev
}

// oicWantNoWrite asserts that nothing was written since before and
// storeBefore were read: no key changed and the store's revision stands.
func oicWantNoWrite(
	env *volEnv,
	what string,
	before map[string][]byte,
	storeBefore int64,
) {
	env.t.Helper()
	env.wantNoWrite(what, before)
	if got := volStoreRev(env); got != storeBefore {
		env.t.Errorf("%s wrote: store revision %d, want %d", what, got,
			storeBefore)
	}
}

// TestUpdateGroupOperatorInChargeWritesTheFlag pins the call's action
// (architecture.md, Repairs): the set puts the data group's flag in one
// transaction with nothing else of the slice changed and no SpRev bump, so
// the same token then clears it; a set or a clear of the value the group
// already holds writes nothing at all.
func TestUpdateGroupOperatorInChargeWritesTheFlag(t *testing.T) {
	env := newVolEnv(t)
	want := env.slice()
	tok := env.token()
	storeBefore := volStoreRev(env)
	reply, err := env.srv.UpdateGroupOperatorInCharge(
		env.ctx, oicRequest(env, true))
	if err != nil {
		t.Fatalf("UpdateGroupOperatorInCharge: %v", err)
	}
	if reply.GetGrpId() != volDataGrpId {
		t.Errorf("reply grp_id: got %d, want %d", reply.GetGrpId(),
			volDataGrpId)
	}
	volGrpOf(t, want, volDataGrpId).OperatorInCharge = true
	if got := env.slice(); !proto.Equal(got, want) {
		t.Errorf("slice:\n got %v\nwant %v", got, want)
	}
	if got := volStoreRev(env); got != storeBefore+1 {
		t.Errorf("store revision: got %d, want %d (one transaction)", got,
			storeBefore+1)
	}
	if got := env.spRev(); got != tok.GetRevision() {
		t.Errorf("sp_rev: got %d, want %d (the write bumps nothing)", got,
			tok.GetRevision())
	}
	for _, on := range []bool{true, false} {
		req := oicRequest(env, on)
		req.SpRev = tok
		if _, err := env.srv.UpdateGroupOperatorInCharge(
			env.ctx, req); err != nil {
			t.Fatalf("the write of %v with the first token: %v", on, err)
		}
		if got := oicFlag(env, volDataGrpId); got != on {
			t.Errorf("the flag after the write of %v: %v", on, got)
		}
		if oicFlag(env, volMetaGrpId) {
			t.Errorf("the meta group carries the flag")
		}
		before, storeBefore := env.dump(), volStoreRev(env)
		if _, err := env.srv.UpdateGroupOperatorInCharge(
			env.ctx, req); err != nil {
			t.Fatalf("a repeated write of %v: %v", on, err)
		}
		oicWantNoWrite(env, fmt.Sprintf("a repeated write of %v", on),
			before, storeBefore)
	}
	if got := env.spRev(); got != tok.GetRevision() {
		t.Errorf("sp_rev after the clear: got %d, want %d", got,
			tok.GetRevision())
	}
}

// TestUpdateGroupOperatorInChargeAtEveryLevel pins the call's levels
// (architecture.md, Repairs): the set and the clear are taken at every
// sp_level, the levels where reactions stop and those where no group array
// is built among them, each with no SpRev bump.
func TestUpdateGroupOperatorInChargeAtEveryLevel(t *testing.T) {
	env := newVolEnv(t)
	for _, level := range []pb.SpLevel{
		pb.SpLevel_SP_LEVEL_READWRITE, pb.SpLevel_SP_LEVEL_READONLY,
		pb.SpLevel_SP_LEVEL_NO_CLONE, pb.SpLevel_SP_LEVEL_NO_THINPOOL,
		pb.SpLevel_SP_LEVEL_NO_REDUND, pb.SpLevel_SP_LEVEL_NO_MIGRATION,
		pb.SpLevel_SP_LEVEL_NO_SIDE, pb.SpLevel_SP_LEVEL_DISABLE,
	} {
		conf := env.spConf()
		conf.SpLevel = level
		env.putSpConf(conf)
		rev := env.spRev()
		for _, on := range []bool{true, false} {
			if _, err := env.srv.UpdateGroupOperatorInCharge(
				env.ctx, oicRequest(env, on)); err != nil {
				t.Fatalf("the write of %v at %s: %v", on, level, err)
			}
			if got := oicFlag(env, volDataGrpId); got != on {
				t.Errorf("the flag after the write of %v at %s: %v", on,
					level, got)
			}
		}
		if got := env.spRev(); got != rev {
			t.Errorf("sp_rev at %s: got %d, want %d", level, got, rev)
		}
	}
}

// oicRefusal is one refusal of UpdateGroupOperatorInCharge: the setup steers
// the fixture and the request.
type oicRefusal struct {
	name  string
	setup func(env *volEnv, req *pb.UpdateGroupOperatorInChargeRequest)
	code  codes.Code
	msg   string
}

// oicRefusals is every refusal of UpdateGroupOperatorInCharge but the
// deleting clone's, which TestUpdateGroupOperatorInChargeOnACloneMetaGroup
// holds, in the order of architecture.md, Repairs. Each case from the
// deleting pool on also carries the faults of the cases after it: the
// deleting pool a stale token, a RedundNone pool and an unknown group; both
// token cases a RedundNone pool and an unknown group; the RedundNone pool an
// unknown group. So a check that moved behind a later one answers with the
// later one's words and fails.
func oicRefusals() []oicRefusal {
	redundNone := func(env *volEnv) {
		conf := env.spConf()
		conf.BdevConf.RedundConf = &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundNone{
				RedundNone: &pb.RedundNone{},
			},
		}
		env.putSpConf(conf)
	}
	return []oicRefusal{
		{
			name: "no sp_name",
			setup: func(_ *volEnv, req *pb.UpdateGroupOperatorInChargeRequest) {
				req.SpName = ""
			},
			code: codes.InvalidArgument,
			msg:  "sp_name must not be empty",
		},
		{
			name: "a bad sp_name",
			setup: func(_ *volEnv, req *pb.UpdateGroupOperatorInChargeRequest) {
				req.SpName = "sp a"
			},
			code: codes.InvalidArgument,
			msg: fmt.Sprintf("sp_name %q does not match %s", "sp a",
				common.ValidStrPattern),
		},
		{
			name: "a bad cluster_name",
			setup: func(_ *volEnv, req *pb.UpdateGroupOperatorInChargeRequest) {
				req.ClusterName = "cluster a"
			},
			code: codes.InvalidArgument,
			msg: fmt.Sprintf("cluster_name %q does not match %s",
				"cluster a", common.ValidStrPattern),
		},
		{
			name: "unknown cluster",
			setup: func(_ *volEnv, req *pb.UpdateGroupOperatorInChargeRequest) {
				req.ClusterName = "nosuch-cluster"
			},
			code: codes.NotFound,
			msg:  fmt.Sprintf("cluster %q not found", "nosuch-cluster"),
		},
		{
			name: "unknown pool",
			setup: func(_ *volEnv, req *pb.UpdateGroupOperatorInChargeRequest) {
				req.SpName = "nosuch"
			},
			code: codes.NotFound,
			msg:  fmt.Sprintf("storage pool %q not found", "nosuch"),
		},
		{
			name: "a deleting pool",
			setup: func(env *volEnv, req *pb.UpdateGroupOperatorInChargeRequest) {
				redundNone(env)
				conf := env.spConf()
				conf.Deleting = true
				conf.SpLevel = pb.SpLevel_SP_LEVEL_DISABLE
				env.putSpConf(conf)
				req.SpRev = &pb.SpRev{Revision: env.spRev() + 9}
				req.GrpId = 9999
			},
			code: codes.FailedPrecondition,
			msg:  fmt.Sprintf("storage pool %q is being deleted", volSpName),
		},
		{
			name: "a stale token",
			setup: func(env *volEnv, req *pb.UpdateGroupOperatorInChargeRequest) {
				redundNone(env)
				req.SpRev = &pb.SpRev{Revision: env.spRev() + 9}
				req.GrpId = 9999
			},
			code: codes.Aborted,
			msg:  msgStaleRevision,
		},
		{
			// GW6 keys on the message's presence: a zero revision never
			// matches, so it is refused where an absent token is not.
			name: "a present zero token",
			setup: func(env *volEnv, req *pb.UpdateGroupOperatorInChargeRequest) {
				redundNone(env)
				req.SpRev = &pb.SpRev{}
				req.GrpId = 9999
			},
			code: codes.Aborted,
			msg:  msgStaleRevision,
		},
		{
			name: "RedundNone",
			setup: func(env *volEnv, req *pb.UpdateGroupOperatorInChargeRequest) {
				redundNone(env)
				req.GrpId = 9999
			},
			code: codes.InvalidArgument,
			msg: fmt.Sprintf("storage pool %q is RedundNone: its groups "+
				"have no repair to take over", volSpName),
		},
		{
			name: "unknown group",
			setup: func(_ *volEnv, req *pb.UpdateGroupOperatorInChargeRequest) {
				req.GrpId = 9999
			},
			code: codes.NotFound,
			msg: fmt.Sprintf("group %d not found in storage pool %q", 9999,
				volSpName),
		},
	}
}

// TestUpdateGroupOperatorInChargeRefusals pins every refusal of
// UpdateGroupOperatorInCharge (architecture.md, Repairs), its code and its
// words, each against a fresh fixture, for a set over a clear flag and a
// clear over a set one, and that it writes nothing anywhere in the cluster.
func TestUpdateGroupOperatorInChargeRefusals(t *testing.T) {
	for _, tc := range oicRefusals() {
		for _, on := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/%v", tc.name, on), func(t *testing.T) {
				env := newVolEnv(t)
				oicPutFlag(env, !on)
				req := oicRequest(env, on)
				tc.setup(env, req)
				before, storeBefore := env.dump(), volStoreRev(env)
				_, err := env.srv.UpdateGroupOperatorInCharge(env.ctx, req)
				if msg := volWantCode(t, err, tc.code); msg != tc.msg {
					t.Errorf("message:\n got %q\nwant %q", msg, tc.msg)
				}
				oicWantNoWrite(env, "the refused UpdateGroupOperatorInCharge",
					before, storeBefore)
			})
		}
	}
}

// TestUpdateGroupOperatorInChargeOnACloneMetaGroup pins the look-up of
// architecture.md, Repairs: a grp_id no slice holds is found among the
// clones' clone meta groups, and the flag is written on the clone's record,
// the slice untouched and no SpRev bumped; a clone being deleted is refused
// FAILED_PRECONDITION, a set and a clear alike, with nothing written.
func TestUpdateGroupOperatorInChargeOnACloneMetaGroup(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	volCreateClone(env, "clone-a", "dst")
	grpId := env.clone("clone-a").GetMetaGrp().GetGrpId()
	slice := env.slice()
	rev := env.spRev()
	req := oicRequest(env, true)
	req.GrpId = grpId
	reply, err := env.srv.UpdateGroupOperatorInCharge(env.ctx, req)
	if err != nil {
		t.Fatalf("UpdateGroupOperatorInCharge on the clone meta group: %v",
			err)
	}
	if reply.GetGrpId() != grpId {
		t.Errorf("reply grp_id: got %d, want %d", reply.GetGrpId(), grpId)
	}
	if !env.clone("clone-a").GetMetaGrp().GetOperatorInCharge() {
		t.Errorf("the clone's group does not carry the flag")
	}
	if got := env.slice(); !proto.Equal(got, slice) {
		t.Errorf("the clone's flag wrote the slice: %v", got)
	}
	if got := env.spRev(); got != rev {
		t.Errorf("sp_rev: got %d, want %d", got, rev)
	}
	volLatchClone(env, "clone-a")
	for _, on := range []bool{true, false} {
		req := oicRequest(env, on)
		req.GrpId = grpId
		before, storeBefore := env.dump(), volStoreRev(env)
		_, err := env.srv.UpdateGroupOperatorInCharge(env.ctx, req)
		msg := volWantCode(t, err, codes.FailedPrecondition)
		if want := `clone "clone-a" is being deleted`; msg != want {
			t.Errorf("the write of %v: message %q, want %q", on, msg, want)
		}
		oicWantNoWrite(env, "the refused UpdateGroupOperatorInCharge",
			before, storeBefore)
	}
}

// TestOperatorsRepairUnderTheFlag pins what the flag leaves to the operator
// (architecture.md, Repairs): under it an operator's repair of the group is
// created, and its finish leaves the flag standing, since only this call
// clears it.
func TestOperatorsRepairUnderTheFlag(t *testing.T) {
	env := newVolEnv(t)
	if _, err := env.srv.UpdateGroupOperatorInCharge(
		env.ctx, oicRequest(env, true)); err != nil {
		t.Fatalf("UpdateGroupOperatorInCharge: %v", err)
	}
	repCreate(env, "rep-a", volDataLegA, volDnC)
	if got := volGrpOf(t, env.slice(), volDataGrpId); got.GetRepName() !=
		"rep-a" || !got.GetOperatorInCharge() {
		t.Errorf("after the create: rep_name %q, flag %v, want %q and the "+
			"flag kept", got.GetRepName(), got.GetOperatorInCharge(), "rep-a")
	}
	repMoveStage(env, "rep-a", pb.RepStage_REP_STAGE_STARTED)
	if _, err := repFinish(env, "rep-a"); err != nil {
		t.Fatalf("FinishRepair: %v", err)
	}
	if !oicFlag(env, volDataGrpId) {
		t.Errorf("the finish cleared the flag")
	}
}
