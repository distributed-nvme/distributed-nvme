package gateway

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ForceStartGroup (architecture.md, Repairs; gateway.md, Repairs and retired
// legs), on the fixture of handler_vol_test.go with its primary moved to the
// package's fake agent, so the read of the primary's report between the
// planning snapshot and the op is a real call the test can count and steer.
// Every write is asserted against the store, every refusal by its code, its
// message and a store that did not move.

// fsCnId is the cn_id of the CN the fake primary sits on.
const fsCnId = uint64(802)

// fsPrimary moves the fixture's primary cntlr onto a fake agent, on a CN of
// its own, and returns the agent: volEnv's own cntlr addresses are not
// dialable, so without it the read of the report could not be observed.
func fsPrimary(env *volEnv) *fakeAgent {
	env.t.Helper()
	addr := fakeAddrPort(env.t, "cn")
	agent := startFakeAgent(env.t, addr, 1<<40)
	env.putCn(addr, fsCnId, 2)
	mustPut(env.t, env.cli, model.CntlrKey(env.cid, volSpId, volCntlrA),
		&pb.Cntlr{
			AddrPort:   addr,
			NvmeTrConf: volTrConf(addr),
			Primary:    true,
		})
	return agent
}

// fsReport installs the primary's report with the group's row in status, as
// the cn agent reads the group's array (cnagent.md CN28).
func fsReport(agent *fakeAgent, grpId uint64, resStatus pb.ResStatus) {
	agent.setInfo(nil, nil, nil, &pb.CntlrInfo{
		GrpIdToMdRaid: map[uint64]*pb.ResInfo{
			grpId: {Status: resStatus, Details: "the group's array"},
		},
	})
}

// fsRequest is the forced start of the data group from legId under the
// current token.
func fsRequest(env *volEnv, legId uint64) *pb.ForceStartGroupRequest {
	env.t.Helper()
	return &pb.ForceStartGroupRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		GrpId:       volDataGrpId,
		LegId:       legId,
	}
}

// fsCancel is the cancel of the data group's mark under the current token.
func fsCancel(env *volEnv) *pb.ForceStartGroupRequest {
	env.t.Helper()
	return &pb.ForceStartGroupRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		GrpId:       volDataGrpId,
		Cancel:      true,
	}
}

// fsMark is the forced-start mark of a group of the fixture slice, nil when
// none stands.
func fsMark(env *volEnv, grpId uint64) *pb.ForcedStart {
	env.t.Helper()
	return volGrpOf(env.t, env.slice(), grpId).GetForcedStart()
}

// fsPutMark plants a mark on a group of the fixture slice, as a write of one
// leaves it.
func fsPutMark(env *volEnv, grpId uint64, legId uint64) {
	env.t.Helper()
	slice := env.slice()
	volGrpOf(env.t, slice, grpId).ForcedStart = &pb.ForcedStart{
		LegId: legId, Revision: env.spRev(),
	}
	env.putSlice(slice)
}

// TestForceStartGroupWritesTheMark pins the call's action (architecture.md,
// Repairs): the mark names the leg and carries the SpRev revision the op's
// own transaction commits, which the reply returns, with one bump; the read
// asks the primary's agent about the primary cntlr of the pool; a second call
// replaces the mark; the cancel withdraws it with one bump and no agent call;
// and a cancel of a group with no mark writes nothing at all.
func TestForceStartGroupWritesTheMark(t *testing.T) {
	env := newVolEnv(t)
	agent := fsPrimary(env)
	fsReport(agent, volDataGrpId, pb.ResStatus_RES_STATUS_ERROR)
	before := env.spRev()
	reply, err := env.srv.ForceStartGroup(env.ctx, fsRequest(env, volDataLegA))
	if err != nil {
		t.Fatalf("ForceStartGroup: %v", err)
	}
	if reply.GetGrpId() != volDataGrpId || reply.GetRevision() != before+1 ||
		env.spRev() != before+1 {
		t.Errorf("reply %v, sp_rev %d, want group %d at revision %d",
			reply, env.spRev(), volDataGrpId, before+1)
	}
	if mark := fsMark(env, volDataGrpId); mark.GetLegId() != volDataLegA ||
		mark.GetRevision() != reply.GetRevision() {
		t.Errorf("the mark: got %v, want leg %d at %d", mark, volDataLegA,
			reply.GetRevision())
	}
	if mark := fsMark(env, volMetaGrpId); mark != nil {
		t.Errorf("the meta group carries a mark: %v", mark)
	}
	reqs := agent.cntlrInfoReqList()
	if len(reqs) != 1 {
		t.Fatalf("GetCntlrInfo calls = %d, want 1", len(reqs))
	}
	if reqs[0].GetClusterId() != env.cid || reqs[0].GetCnId() != fsCnId ||
		reqs[0].GetCntlrPointer().GetSpId() != volSpId ||
		reqs[0].GetCntlrPointer().GetCntlrId() != volCntlrA {
		t.Errorf("GetCntlrInfo asked for %v, want (%#x, %d, {%#x, %d})",
			reqs[0], env.cid, fsCnId, volSpId, volCntlrA)
	}

	reply, err = env.srv.ForceStartGroup(env.ctx, fsRequest(env, volDataLegB))
	if err != nil {
		t.Fatalf("the replace: %v", err)
	}
	if mark := fsMark(env, volDataGrpId); mark.GetLegId() != volDataLegB ||
		mark.GetRevision() != reply.GetRevision() ||
		reply.GetRevision() != before+2 {
		t.Errorf("the replaced mark: got %v, reply %v, want leg %d at %d",
			mark, reply, volDataLegB, before+2)
	}

	calls := agent.callCount("GetCntlrInfo")
	reply, err = env.srv.ForceStartGroup(env.ctx, fsCancel(env))
	if err != nil {
		t.Fatalf("the cancel: %v", err)
	}
	if reply.GetGrpId() != volDataGrpId || reply.GetRevision() != 0 {
		t.Errorf("the cancel's reply: %v, want group %d, revision 0", reply,
			volDataGrpId)
	}
	if mark := fsMark(env, volDataGrpId); mark != nil {
		t.Errorf("the cancel left the mark %v", mark)
	}
	if got := env.spRev(); got != before+3 {
		t.Errorf("sp_rev after the cancel: got %d, want %d", got, before+3)
	}
	dump := env.dump()
	if _, err := env.srv.ForceStartGroup(env.ctx, fsCancel(env)); err != nil {
		t.Fatalf("a cancel with no mark: %v", err)
	}
	env.wantNoWrite("a cancel with no mark", dump)
	if got := agent.callCount("GetCntlrInfo"); got != calls {
		t.Errorf("the cancels called the agent: %d calls, want %d", got,
			calls)
	}
}

// TestForceStartGroupLevelGate pins where the level gate of architecture.md,
// Repairs, starts: a write is taken at every level below SP_LEVEL_NO_REDUND,
// SP_LEVEL_NO_THINPOOL among them, the level where reactions stop and the
// operator writes the mark, with the mark and one bump; from
// SP_LEVEL_NO_REDUND up, where no group array is built, it is refused before
// the agent is asked and with nothing written; and a cancel withdraws a mark
// at every level, with one bump and no agent call.
func TestForceStartGroupLevelGate(t *testing.T) {
	for _, tc := range []struct {
		level pb.SpLevel
		code  codes.Code
	}{
		{pb.SpLevel_SP_LEVEL_READWRITE, codes.OK},
		{pb.SpLevel_SP_LEVEL_READONLY, codes.OK},
		{pb.SpLevel_SP_LEVEL_NO_CLONE, codes.OK},
		{pb.SpLevel_SP_LEVEL_NO_THINPOOL, codes.OK},
		{pb.SpLevel_SP_LEVEL_NO_REDUND, codes.FailedPrecondition},
		{pb.SpLevel_SP_LEVEL_NO_MIGRATION, codes.FailedPrecondition},
		{pb.SpLevel_SP_LEVEL_NO_SIDE, codes.FailedPrecondition},
		{pb.SpLevel_SP_LEVEL_DISABLE, codes.FailedPrecondition},
	} {
		t.Run(tc.level.String(), func(t *testing.T) {
			env := newVolEnv(t)
			agent := fsPrimary(env)
			fsReport(agent, volDataGrpId, pb.ResStatus_RES_STATUS_ERROR)
			conf := env.spConf()
			conf.SpLevel = tc.level
			env.putSpConf(conf)
			before := env.spRev()
			dump := env.dump()
			reply, err := env.srv.ForceStartGroup(
				env.ctx, fsRequest(env, volDataLegA))
			if tc.code == codes.OK {
				if err != nil {
					t.Fatalf("ForceStartGroup at %s: %v", tc.level, err)
				}
				if mark := fsMark(env, volDataGrpId); mark.GetLegId() !=
					volDataLegA || mark.GetRevision() != before+1 ||
					reply.GetRevision() != before+1 ||
					env.spRev() != before+1 {
					t.Errorf("the mark %v, reply %v, sp_rev %d: want leg "+
						"%d at revision %d", mark, reply, env.spRev(),
						volDataLegA, before+1)
				}
				if got := agent.callCount("GetCntlrInfo"); got != 1 {
					t.Errorf("GetCntlrInfo calls = %d, want 1", got)
				}
			} else {
				msg := volWantCode(t, err, tc.code)
				if want := fmt.Sprintf("storage pool %q is at %s, where no "+
					"group array is built", volSpName, tc.level); msg != want {
					t.Errorf("message:\n got %q\nwant %q", msg, want)
				}
				env.wantNoWrite("the refused ForceStartGroup", dump)
				if got := agent.callCount("GetCntlrInfo"); got != 0 {
					t.Errorf("GetCntlrInfo calls = %d, want 0", got)
				}
				fsPutMark(env, volDataGrpId, volDataLegA)
			}
			calls := agent.callCount("GetCntlrInfo")
			rev := env.spRev()
			if _, err := env.srv.ForceStartGroup(
				env.ctx, fsCancel(env)); err != nil {
				t.Fatalf("the cancel at %s: %v", tc.level, err)
			}
			if mark := fsMark(env, volDataGrpId); mark != nil {
				t.Errorf("the cancel at %s left the mark %v", tc.level, mark)
			}
			if got := env.spRev(); got != rev+1 {
				t.Errorf("sp_rev after the cancel: got %d, want %d", got,
					rev+1)
			}
			if got := agent.callCount("GetCntlrInfo"); got != calls {
				t.Errorf("the cancel called the agent: %d calls, want %d",
					got, calls)
			}
		})
	}
}

// TestForceStartGroupNamesARepairsNewLeg pins the second leg the mark may
// name (architecture.md, Repairs): the new leg of the group's open repair,
// but that of a CREATED repair whose abort flag is set; the abort is refused
// while the mark names that leg, goes through once the mark is withdrawn,
// and a mark naming that leg is refused while the repair stays CREATED; once
// the primary's started verdict moves the repair to STARTED, which the
// flag does not hold back, the mark may name that leg again.
func TestForceStartGroupNamesARepairsNewLeg(t *testing.T) {
	env := newVolEnv(t)
	agent := fsPrimary(env)
	fsReport(agent, volDataGrpId, pb.ResStatus_RES_STATUS_ERROR)
	created := repCreate(env, "rep-a", volDataLegA, volDnC)
	newLegId := created.GetNewLegId()
	if _, err := env.srv.ForceStartGroup(
		env.ctx, fsRequest(env, newLegId)); err != nil {
		t.Fatalf("ForceStartGroup of the repair's new leg: %v", err)
	}
	if mark := fsMark(env, volDataGrpId); mark.GetLegId() != newLegId {
		t.Errorf("the mark: got %v, want leg %d", mark, newLegId)
	}
	before := env.dump()
	_, err := repAbort(env, "rep-a")
	msg := volWantCode(t, err, codes.FailedPrecondition)
	if want := "SetRepairAbort: precondition failed: the group's " +
		"forced-start mark names the new leg"; msg != want {
		t.Errorf("the abort's message:\n got %q\nwant %q", msg, want)
	}
	env.wantNoWrite("the refused TryToAbortRepair", before)
	if _, err := env.srv.ForceStartGroup(env.ctx, fsCancel(env)); err != nil {
		t.Fatalf("the cancel: %v", err)
	}
	if _, err := repAbort(env, "rep-a"); err != nil {
		t.Fatalf("TryToAbortRepair after the cancel: %v", err)
	}
	before = env.dump()
	_, err = env.srv.ForceStartGroup(env.ctx, fsRequest(env, newLegId))
	msg = volWantCode(t, err, codes.FailedPrecondition)
	if want := fmt.Sprintf("leg %d is the new leg of the repair %q, which "+
		"is REP_STAGE_CREATED with its abort flag set: the forced start of "+
		"group %d keeps no such member", newLegId,
		"rep-a", volDataGrpId); msg != want {
		t.Errorf("message:\n got %q\nwant %q", msg, want)
	}
	env.wantNoWrite("the refused ForceStartGroup", before)
	rep := env.repair("rep-a")
	rep.RepStage = pb.RepStage_REP_STAGE_STARTED
	mustPut(env.t, env.cli, model.RepairKey(env.cid, volSpId, "rep-a"), rep)
	if _, err := env.srv.ForceStartGroup(
		env.ctx, fsRequest(env, newLegId)); err != nil {
		t.Fatalf("ForceStartGroup of a started repair's new leg, its abort "+
			"flag set: %v", err)
	}
	if mark := fsMark(env, volDataGrpId); mark.GetLegId() != newLegId {
		t.Errorf("the mark: got %v, want leg %d", mark, newLegId)
	}
}

// TestForceStartGroupGoesOnWithoutAnOkRow pins the readings of the
// primary's report that let the mark be written (architecture.md, Repairs):
// a group row in any status but OK, whatever the reply's code, an
// UnknownObject reply that carries a row among them.
func TestForceStartGroupGoesOnWithoutAnOkRow(t *testing.T) {
	broken := &pb.CntlrInfo{
		GrpIdToMdRaid: map[uint64]*pb.ResInfo{
			volDataGrpId: {
				Status:  pb.ResStatus_RES_STATUS_ERROR,
				Details: "array_state broken",
			},
		},
	}
	for _, tc := range []struct {
		name  string
		reply *pb.AgentReply
		info  *pb.CntlrInfo
	}{
		{"a broken array", nil, broken},
		{"a reply code beside the row", &pb.AgentReply{
			Code:    common.ReplyCodeUnknownObject,
			Details: "unknown cntlr",
		}, broken},
		{"no array", nil, &pb.CntlrInfo{
			GrpIdToMdRaid: map[uint64]*pb.ResInfo{
				volDataGrpId: {Status: pb.ResStatus_RES_STATUS_MISSING},
			},
		}},
		{"a row not read", nil, &pb.CntlrInfo{
			GrpIdToMdRaid: map[uint64]*pb.ResInfo{
				volDataGrpId: {Status: pb.ResStatus_RES_STATUS_UNKNOWN},
			},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			agent := fsPrimary(env)
			agent.setCntlrReply(tc.reply)
			agent.setInfo(nil, nil, nil, tc.info)
			reply, err := env.srv.ForceStartGroup(
				env.ctx, fsRequest(env, volDataLegA))
			if err != nil {
				t.Fatalf("ForceStartGroup: %v", err)
			}
			if mark := fsMark(env, volDataGrpId); mark.GetLegId() !=
				volDataLegA || mark.GetRevision() != reply.GetRevision() {
				t.Errorf("the mark: got %v, reply %v", mark, reply)
			}
		})
	}
}

// TestForceStartGroupRefusesAReportWithNoRow pins the reading that refuses
// as silence does (architecture.md, Repairs): a report that holds no row of
// the group — a reply with no info, the UnknownObject refusal with no info
// that an agent that does not hold the cntlr answers (cnagent.md CN23), and
// a report whose rows leave the group out, as a standby's does — proves
// nothing about the group's array, and is FAILED_PRECONDITION with nothing
// written.
func TestForceStartGroupRefusesAReportWithNoRow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply *pb.AgentReply
		info  *pb.CntlrInfo
	}{
		{"no info", nil, nil},
		{"an unknown cntlr", &pb.AgentReply{
			Code:    common.ReplyCodeUnknownObject,
			Details: "unknown cntlr",
		}, nil},
		{"no row for the group", nil, &pb.CntlrInfo{
			GrpIdToMdRaid: map[uint64]*pb.ResInfo{
				volMetaGrpId: {Status: pb.ResStatus_RES_STATUS_OK},
			},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			agent := fsPrimary(env)
			agent.setCntlrReply(tc.reply)
			agent.setInfo(nil, nil, nil, tc.info)
			before := env.dump()
			_, err := env.srv.ForceStartGroup(
				env.ctx, fsRequest(env, volDataLegA))
			msg := volWantCode(t, err, codes.FailedPrecondition)
			if want := fmt.Sprintf("group %d: its array is not proven "+
				"down, the primary's latest report holds no row of the "+
				"group", volDataGrpId); msg != want {
				t.Errorf("message:\n got %q\nwant %q", msg, want)
			}
			env.wantNoWrite("the refused ForceStartGroup", before)
		})
	}
}

// fsRefusal is one refusal of ForceStartGroup: the setup steers the fixture,
// whose primary is a fake agent reporting the data group's row OK, and the
// request; calls is how many times the call must have asked the agent.
type fsRefusal struct {
	name  string
	setup func(env *volEnv, agent *fakeAgent, req *pb.ForceStartGroupRequest)
	code  codes.Code
	msg   string
	calls int
}

// fsRefusals is every refusal of ForceStartGroup, in the order of
// architecture.md, Repairs. Each case also carries faults of the cases after
// it where the fixture allows — the level under a deleting pool and a stale
// token, a RedundNone pool and an unknown group under the level, no primary
// under a leg the mark may not name, a row that reads OK under every case —
// so a check that moved behind a later one answers with the later one's
// words and fails.
func fsRefusals() []fsRefusal {
	noPrimary := func(env *volEnv) {
		for _, cntlrId := range []uint64{volCntlrA, volCntlrB} {
			env.patchCntlr(cntlrId, func(cntlr *pb.Cntlr) {
				cntlr.Primary = false
			})
		}
	}
	redundNone := func(env *volEnv) {
		conf := env.spConf()
		conf.BdevConf.RedundConf = &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundNone{
				RedundNone: &pb.RedundNone{},
			},
		}
		env.putSpConf(conf)
	}
	return []fsRefusal{
		{
			name: "no sp_name",
			setup: func(_ *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				req.SpName = ""
			},
			code: codes.InvalidArgument,
			msg:  "sp_name must not be empty",
		},
		{
			name: "a leg with cancel",
			setup: func(_ *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				req.Cancel = true
			},
			code: codes.InvalidArgument,
			msg: fmt.Sprintf("leg_id %d is set with cancel: a cancel names "+
				"no leg", volDataLegA),
		},
		{
			name: "no leg without cancel",
			setup: func(_ *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				req.LegId = 0
			},
			code: codes.InvalidArgument,
			msg:  "leg_id must name the leg the group's array starts from",
		},
		{
			name: "unknown pool",
			setup: func(_ *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				req.SpName = "nosuch"
			},
			code: codes.NotFound,
			msg:  fmt.Sprintf("storage pool %q not found", "nosuch"),
		},
		{
			name: "a deleting pool",
			setup: func(env *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				conf := env.spConf()
				conf.Deleting = true
				conf.SpLevel = pb.SpLevel_SP_LEVEL_DISABLE
				env.putSpConf(conf)
			},
			code: codes.FailedPrecondition,
			msg:  fmt.Sprintf("storage pool %q is being deleted", volSpName),
		},
		{
			name: "a stale token",
			setup: func(env *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				req.SpRev = &pb.SpRev{Revision: env.spRev() + 9}
				conf := env.spConf()
				conf.SpLevel = pb.SpLevel_SP_LEVEL_DISABLE
				env.putSpConf(conf)
				req.GrpId = 9999
			},
			code: codes.Aborted,
			msg:  msgStaleRevision,
		},
		{
			name: "SP_LEVEL_NO_REDUND",
			setup: func(env *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				conf := env.spConf()
				conf.SpLevel = pb.SpLevel_SP_LEVEL_NO_REDUND
				env.putSpConf(conf)
				redundNone(env)
				req.GrpId = 9999
			},
			code: codes.FailedPrecondition,
			msg: fmt.Sprintf("storage pool %q is at SP_LEVEL_NO_REDUND, "+
				"where no group array is built", volSpName),
		},
		{
			name: "SP_LEVEL_DISABLE",
			setup: func(env *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				conf := env.spConf()
				conf.SpLevel = pb.SpLevel_SP_LEVEL_DISABLE
				env.putSpConf(conf)
			},
			code: codes.FailedPrecondition,
			msg: fmt.Sprintf("storage pool %q is at SP_LEVEL_DISABLE, "+
				"where no group array is built", volSpName),
		},
		{
			name: "RedundNone",
			setup: func(env *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				redundNone(env)
				req.GrpId = 9999
			},
			code: codes.InvalidArgument,
			msg: fmt.Sprintf("storage pool %q is RedundNone: its groups "+
				"have no array to start", volSpName),
		},
		{
			// A cancel skips the level, and still meets the RedundNone
			// refusal ahead of the group's look-up.
			name: "a cancel on a RedundNone pool",
			setup: func(env *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				conf := env.spConf()
				conf.SpLevel = pb.SpLevel_SP_LEVEL_DISABLE
				env.putSpConf(conf)
				redundNone(env)
				req.Cancel = true
				req.LegId = 0
				req.GrpId = 9999
			},
			code: codes.InvalidArgument,
			msg: fmt.Sprintf("storage pool %q is RedundNone: its groups "+
				"have no array to start", volSpName),
		},
		{
			name: "unknown group",
			setup: func(_ *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				req.GrpId = 9999
			},
			code: codes.NotFound,
			msg: fmt.Sprintf("group %d not found in storage pool %q", 9999,
				volSpName),
		},
		{
			name: "a leg the group does not hold",
			setup: func(env *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				req.LegId = 9999
				noPrimary(env)
			},
			code: codes.NotFound,
			msg: fmt.Sprintf("leg %d is neither in the leg_list of group %d "+
				"nor a new leg of its repair", 9999, volDataGrpId),
		},
		{
			name: "a leg of another group",
			setup: func(_ *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				req.LegId = volMetaLegA
			},
			code: codes.NotFound,
			msg: fmt.Sprintf("leg %d is neither in the leg_list of group %d "+
				"nor a new leg of its repair", volMetaLegA, volDataGrpId),
		},
		{
			// A group with an open repair takes a leg outside its leg_list
			// only as a new leg of that repair.
			name: "a leg the group does not hold, with a created repair",
			setup: func(env *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				repPutRepair(env, "rep-c", volDataGrpId, volDataLegB,
					pb.RepStage_REP_STAGE_CREATED, 801, volSide(802, volDnC))
				req.LegId = 9999
				noPrimary(env)
			},
			code: codes.NotFound,
			msg: fmt.Sprintf("leg %d is neither in the leg_list of group %d "+
				"nor a new leg of its repair", 9999, volDataGrpId),
		},
		{
			name: "a leg of another group, with a started repair",
			setup: func(env *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				repPutRepair(env, "rep-s", volDataGrpId, volDataLegB,
					pb.RepStage_REP_STAGE_STARTED, 801, volSide(802, volDnC))
				req.LegId = volMetaLegA
				noPrimary(env)
			},
			code: codes.NotFound,
			msg: fmt.Sprintf("leg %d is neither in the leg_list of group %d "+
				"nor a new leg of its repair", volMetaLegA, volDataGrpId),
		},
		{
			name: "the new leg of an aborting repair",
			setup: func(env *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				repPutRepair(env, "rep-z", volDataGrpId, volDataLegB,
					pb.RepStage_REP_STAGE_CREATED, 801, volSide(802, volDnC))
				rep := env.repair("rep-z")
				rep.TryToAbort = true
				mustPut(env.t, env.cli,
					model.RepairKey(env.cid, volSpId, "rep-z"), rep)
				req.LegId = 801
				noPrimary(env)
			},
			code: codes.FailedPrecondition,
			msg: fmt.Sprintf("leg %d is the new leg of the repair %q, "+
				"which is REP_STAGE_CREATED with its abort flag set: the "+
				"forced start of group %d keeps no such member",
				801, "rep-z", volDataGrpId),
		},
		{
			name: "the bad leg of a started repair",
			setup: func(env *volEnv, _ *fakeAgent,
				req *pb.ForceStartGroupRequest) {
				repPutRepair(env, "rep-s", volDataGrpId, volDataLegB,
					pb.RepStage_REP_STAGE_STARTED, 801, volSide(802, volDnC))
				req.LegId = volDataLegB
				noPrimary(env)
			},
			code: codes.FailedPrecondition,
			msg: fmt.Sprintf("leg %d is the bad leg of the repair %q, "+
				"which is REP_STAGE_STARTED: the leg left the array before "+
				"md began copying onto the new leg, and the finish retires "+
				"it, so the forced start of group %d may not name it",
				volDataLegB, "rep-s", volDataGrpId),
		},
		{
			name: "no primary",
			setup: func(env *volEnv, _ *fakeAgent,
				_ *pb.ForceStartGroupRequest) {
				noPrimary(env)
			},
			code: codes.FailedPrecondition,
			msg: fmt.Sprintf("storage pool %q has no primary cntlr whose "+
				"report could show that the array of group %d does not "+
				"run", volSpName, volDataGrpId),
		},
		{
			name: "an array that runs",
			setup: func(_ *volEnv, _ *fakeAgent,
				_ *pb.ForceStartGroupRequest) {
			},
			code: codes.FailedPrecondition,
			msg: fmt.Sprintf("group %d runs: the primary's latest report "+
				"reads its row RES_STATUS_OK", volDataGrpId),
			calls: 1,
		},
	}
}

// TestForceStartGroupRefusals pins every refusal of ForceStartGroup
// (architecture.md, Repairs), its code and its words, each against a fresh
// fixture, that it writes nothing anywhere in the cluster, and that only the
// last one, which the report answers, reaches the agent.
func TestForceStartGroupRefusals(t *testing.T) {
	for _, tc := range fsRefusals() {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			agent := fsPrimary(env)
			fsReport(agent, volDataGrpId, pb.ResStatus_RES_STATUS_OK)
			req := fsRequest(env, volDataLegA)
			tc.setup(env, agent, req)
			before := env.dump()
			_, err := env.srv.ForceStartGroup(env.ctx, req)
			if msg := volWantCode(t, err, tc.code); msg != tc.msg {
				t.Errorf("message:\n got %q\nwant %q", msg, tc.msg)
			}
			env.wantNoWrite("the refused ForceStartGroup", before)
			if got := agent.callCount("GetCntlrInfo"); got != tc.calls {
				t.Errorf("GetCntlrInfo calls = %d, want %d", got, tc.calls)
			}
		})
	}
}

// TestForceStartGroupRefusesASilentAgent pins the refusal of a primary
// whose agent does not answer (gateway.md AG3): FAILED_PRECONDITION, since
// silence proves nothing about the array, with nothing written.
func TestForceStartGroupRefusesASilentAgent(t *testing.T) {
	env := newVolEnv(t)
	agent := fsPrimary(env)
	agent.setFail(status.Error(codes.Unavailable, "the agent is down"))
	before := env.dump()
	_, err := env.srv.ForceStartGroup(env.ctx, fsRequest(env, volDataLegA))
	msg := volWantCode(t, err, codes.FailedPrecondition)
	want := fmt.Sprintf("group %d: its array is not proven down, "+
		"controller node %q did not answer: ", volDataGrpId, agent.AddrPort)
	if len(msg) < len(want) || msg[:len(want)] != want {
		t.Errorf("message:\n got %q\nwant the prefix %q", msg, want)
	}
	env.wantNoWrite("the refused ForceStartGroup", before)
	if got := agent.callCount("GetCntlrInfo"); got != 1 {
		t.Errorf("GetCntlrInfo calls = %d, want 1", got)
	}
}

// TestForceStartGroupAbortsOnALostCn pins the planning read's answer to a
// primary whose CN record is gone: a live cntlr names its CN, and
// DeleteControllerNode refuses a CN that hosts one, so the absence is a lost
// invariant key, ABORTED with the key named (architecture.md, UNEXPECTED_ERROR
// → `ABORTED`), with nothing written and no agent call, though the report
// would read the row OK.
func TestForceStartGroupAbortsOnALostCn(t *testing.T) {
	env := newVolEnv(t)
	agent := fsPrimary(env)
	fsReport(agent, volDataGrpId, pb.ResStatus_RES_STATUS_OK)
	cnKey := model.CnConfKey(env.cid, agent.AddrPort)
	if err := env.cli.Delete(env.ctx, cnKey); err != nil {
		t.Fatalf("Delete %s: %v", cnKey, err)
	}
	before := env.dump()
	_, err := env.srv.ForceStartGroup(env.ctx, fsRequest(env, volDataLegA))
	msg := volWantCode(t, err, codes.Aborted)
	if want := fmt.Sprintf("cn_conf key %q is missing", cnKey); msg != want {
		t.Errorf("message:\n got %q\nwant %q", msg, want)
	}
	env.wantNoWrite("the refused ForceStartGroup", before)
	if got := agent.callCount("GetCntlrInfo"); got != 0 {
		t.Errorf("GetCntlrInfo calls = %d, want 0", got)
	}
}

// TestForceStartGroupCancelSkipsTheGates pins the cancel's own path
// (architecture.md, Repairs): it names no leg and reads no report, so it
// withdraws a mark at a level where no group array is built and on a pool
// with no primary, with one bump and no agent call; the group's look-up
// still refuses an unknown group.
func TestForceStartGroupCancelSkipsTheGates(t *testing.T) {
	env := newVolEnv(t)
	agent := fsPrimary(env)
	agent.setFail(status.Error(codes.Unavailable, "the agent is down"))
	fsPutMark(env, volDataGrpId, volDataLegA)
	conf := env.spConf()
	conf.SpLevel = pb.SpLevel_SP_LEVEL_DISABLE
	env.putSpConf(conf)
	env.patchCntlr(volCntlrA, func(cntlr *pb.Cntlr) { cntlr.Primary = false })
	req := fsCancel(env)
	req.GrpId = 9999
	before := env.dump()
	_, err := env.srv.ForceStartGroup(env.ctx, req)
	if msg := volWantCode(t, err, codes.NotFound); msg != fmt.Sprintf(
		"group %d not found in storage pool %q", 9999, volSpName) {
		t.Errorf("the cancel of an unknown group: message %q", msg)
	}
	env.wantNoWrite("the refused cancel", before)
	rev := env.spRev()
	if _, err := env.srv.ForceStartGroup(env.ctx, fsCancel(env)); err != nil {
		t.Fatalf("the cancel at SP_LEVEL_DISABLE with no primary: %v", err)
	}
	if mark := fsMark(env, volDataGrpId); mark != nil {
		t.Errorf("the cancel left the mark %v", mark)
	}
	if got := env.spRev(); got != rev+1 {
		t.Errorf("sp_rev: got %d, want %d", got, rev+1)
	}
	if got := agent.callCount("GetCntlrInfo"); got != 0 {
		t.Errorf("the cancel called the agent %d times", got)
	}
}

// TestForceStartGroupOnACloneMetaGroup pins the look-up of architecture.md,
// Repairs: a grp_id no slice holds is found among the clones' clone meta
// groups, and the mark is written on the clone's record; a clone being
// deleted is refused before the leg and the report, a cancel included.
func TestForceStartGroupOnACloneMetaGroup(t *testing.T) {
	env := newVolEnv(t)
	agent := fsPrimary(env)
	grp := repCloneBadLeg(env)
	fsReport(agent, grp.GetGrpId(), pb.ResStatus_RES_STATUS_ERROR)
	legId := grp.GetLegList()[1].GetLegId()
	req := fsRequest(env, legId)
	req.GrpId = grp.GetGrpId()
	reply, err := env.srv.ForceStartGroup(env.ctx, req)
	if err != nil {
		t.Fatalf("ForceStartGroup on the clone meta group: %v", err)
	}
	mark := env.clone("clone-a").GetMetaGrp().GetForcedStart()
	if mark.GetLegId() != legId || mark.GetRevision() != reply.GetRevision() {
		t.Errorf("the clone's mark: got %v, want leg %d at %d", mark, legId,
			reply.GetRevision())
	}
	volLatchClone(env, "clone-a")
	calls := agent.callCount("GetCntlrInfo")
	for _, cancel := range []bool{false, true} {
		req := fsRequest(env, 9999)
		req.GrpId = grp.GetGrpId()
		if cancel {
			req.LegId, req.Cancel = 0, true
		}
		before := env.dump()
		_, err := env.srv.ForceStartGroup(env.ctx, req)
		msg := volWantCode(t, err, codes.FailedPrecondition)
		if want := `clone "clone-a" is being deleted`; msg != want {
			t.Errorf("cancel %v: message %q, want %q", cancel, msg, want)
		}
		env.wantNoWrite("the refused ForceStartGroup", before)
	}
	if got := agent.callCount("GetCntlrInfo"); got != calls {
		t.Errorf("the refusals called the agent: %d calls, want %d", got,
			calls)
	}
}

// TestForceStartGroupChecksTheTokenInItsOp pins the second token check of
// GW6 (AG4): the planning read passes the token, a token-less abort of
// another repair then bumps SpRev before the op's STM runs — for the write
// across its read of the report, for the cancel at once — and the call must
// answer ABORTED "stale revision" and write nothing.
func TestForceStartGroupChecksTheTokenInItsOp(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel %v", cancel), func(t *testing.T) {
			env := newVolEnv(t)
			agent := fsPrimary(env)
			fsReport(agent, volDataGrpId, pb.ResStatus_RES_STATUS_ERROR)
			fsPutMark(env, volDataGrpId, volDataLegB)
			repPutRepair(env, "rep-z", volMetaGrpId, volMetaLegA,
				pb.RepStage_REP_STAGE_CREATED, 801, volSide(802, volDnD))
			req := fsRequest(env, volDataLegA)
			if cancel {
				req = fsCancel(env)
			}
			tok := req.GetSpRev()
			var before map[string][]byte
			err := repRaceAtSpRev(t, env, func() error {
				_, err := env.srv.TryToAbortRepair(env.ctx,
					&pb.TryToAbortRepairRequest{
						ClusterName: env.cluster,
						SpName:      volSpName,
						RepName:     "rep-z",
					})
				before = env.dump()
				return err
			}, func(ctx context.Context) error {
				_, err := env.srv.ForceStartGroup(ctx, req)
				return err
			})
			if msg := volWantCode(t, err, codes.Aborted); msg !=
				msgStaleRevision {
				t.Errorf("message: got %q, want %q", msg, msgStaleRevision)
			}
			if rev := env.spRev(); rev != tok.GetRevision()+1 {
				t.Errorf("sp_rev: got %d, want the other writer's %d", rev,
					tok.GetRevision()+1)
			}
			env.wantNoWrite("the stale ForceStartGroup", before)
		})
	}
}

// TestForceStartGroupAnswersALegTakenAway pins the op's own leg check
// (model.ReasonLegNotNamable): the stage move to STARTED of the repair whose
// bad leg the call names, after the planning read has passed, reaches the
// client as FAILED_PRECONDITION in the gateway's words, and the op writes no
// mark.
func TestForceStartGroupAnswersALegTakenAway(t *testing.T) {
	env := newVolEnv(t)
	agent := fsPrimary(env)
	fsReport(agent, volDataGrpId, pb.ResStatus_RES_STATUS_ERROR)
	repCreate(env, "rep-a", volDataLegA, volDnC)
	err := repRaceAtSpRev(t, env, func() error {
		return model.SetRepairStage(env.ctx, env.cli, env.cid, volShard,
			volSpId, volSpName, "rep-a", env.repair("rep-a").GetRepId(),
			pb.RepStage_REP_STAGE_STARTED)
	}, func(ctx context.Context) error {
		req := fsRequest(env, volDataLegA)
		req.SpRev = nil
		_, err := env.srv.ForceStartGroup(ctx, req)
		return err
	})
	msg := volWantCode(t, err, codes.FailedPrecondition)
	if want := fmt.Sprintf("leg %d is no leg the forced start of group %d "+
		"may name any more: neither a leg of its leg_list, other than the "+
		"bad leg of a started repair, nor a new leg of its repair while "+
		"that repair is started or is created with its abort flag clear",
		volDataLegA, volDataGrpId); msg != want {
		t.Errorf("message:\n got %q\nwant %q", msg, want)
	}
	if mark := fsMark(env, volDataGrpId); mark != nil {
		t.Errorf("the refused op wrote the mark %v", mark)
	}
}

// TestForceStartGroupResolvesThePool pins the opening ForceStartGroup shares
// with the repair calls (GW4, GW5): a pool name that breaks the common name
// rule is INVALID_ARGUMENT before any read, and a cluster or a pool the
// store does not hold is NOT_FOUND, a cancel alike, with nothing written.
func TestForceStartGroupResolvesThePool(t *testing.T) {
	env := newVolEnv(t)
	before := env.dump()
	for _, cancel := range []bool{false, true} {
		req := &pb.ForceStartGroupRequest{
			ClusterName: env.cluster,
			SpName:      "sp a",
			GrpId:       volDataGrpId,
			LegId:       volDataLegA,
			Cancel:      cancel,
		}
		if cancel {
			req.LegId = 0
		}
		_, err := env.srv.ForceStartGroup(env.ctx, req)
		volWantCode(t, err, codes.InvalidArgument)
		req.SpName = "nosuch"
		_, err = env.srv.ForceStartGroup(env.ctx, req)
		if msg := volWantCode(t, err, codes.NotFound); msg !=
			fmt.Sprintf("storage pool %q not found", "nosuch") {
			t.Errorf("cancel %v, a missing pool: message %q", cancel, msg)
		}
		req.SpName, req.ClusterName = volSpName, "nosuch-cluster"
		_, err = env.srv.ForceStartGroup(env.ctx, req)
		if msg := volWantCode(t, err, codes.NotFound); msg !=
			fmt.Sprintf("cluster %q not found", "nosuch-cluster") {
			t.Errorf("cancel %v, a missing cluster: message %q", cancel, msg)
		}
	}
	env.wantNoWrite("the refused calls", before)
}
