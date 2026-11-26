package cnagent

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestAStartingAgentAnswersStarting pins dnagent.md SH33 on the cn agent: a
// server Prepare began answers, until its startup pass ends, every round
// and every info read with the starting code and the agent's start time, and refuses a Syncup*, a push, a bitmap read
// and the inspect call; once Reconcile ends, the same requests are answered
// as SH25 and SH28 say, the start time still on the stamp.
func TestAStartingAgentAnswersStarting(t *testing.T) {
	node := newFakeNode()
	node.dirs[agent.NvmetRoot] = true
	srv := newCnServerOnPort(node, common.NvmetPortId)
	ctx := t.Context()
	srv.Prepare(ctx)
	wantStarting := func(what string, reply *pb.AgentReply, stamp *pb.ObsStamp) {
		t.Helper()
		if reply.GetCode() != common.ReplyCodeStarting {
			t.Fatalf("%s: code %d, want %d (starting)", what, reply.GetCode(),
				common.ReplyCodeStarting)
		}
		if stamp != nil && (stamp.GetAgentStartTime() == nil ||
			stamp.GetObsTime() != nil) {
			t.Fatalf("%s: stamp %v, want the start time and no observation",
				what, stamp)
		}
	}
	round, _ := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr(),
		Revision: 3}, nil)
	wantStarting("a cntlr round", round.GetAgentReply(), round.GetObsStamp())
	cnRound, _ := srv.checkCnRound(ctx, &pb.CheckCnRequest{
		ClusterId: testCluster, CnId: testCn, Revision: 2}, nil)
	wantStarting("a cn round", cnRound.GetAgentReply(), cnRound.GetObsStamp())
	info, _ := srv.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr()})
	wantStarting("an info read", info.GetAgentReply(), info.GetObsStamp())
	syncup, _ := srv.SyncupCn(ctx, cnReq(2, true))
	wantStarting("a SyncupCn", syncup.GetAgentReply(), syncup.GetObsStamp())
	cntlr, _ := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{revision: 3}))
	wantStarting("a SyncupCntlr", cntlr.GetAgentReply(), cntlr.GetObsStamp())
	push, _ := srv.PushCloneBitmap(ctx, &pb.PushCloneBitmapRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr()})
	wantStarting("a push", push.GetAgentReply(), nil)
	if _, err := srv.GetLegBm(ctx, &pb.GetLegBmRequest{
		ClusterId: testCluster, CnId: testCn}); status.Code(err) !=
		codes.Unavailable {
		t.Fatalf("a leg bitmap read: %v, want Unavailable", err)
	}
	if _, err := srv.InspectGrp(ctx, &pb.InspectGrpRequest{
		ClusterId: testCluster, CnId: testCn}); status.Code(err) !=
		codes.Unavailable {
		t.Fatalf("an inspect call: %v, want Unavailable", err)
	}
	cnInfo, _ := srv.GetCnInfo(ctx, &pb.GetCnInfoRequest{
		ClusterId: testCluster, CnId: testCn})
	wantStarting("a cn info read", cnInfo.GetAgentReply(),
		cnInfo.GetObsStamp())
	if _, err := srv.GetThinDeviceBm(ctx, &pb.GetThinDeviceBmRequest{
		ClusterId: testCluster, CnId: testCn, SpId: testSp,
		CntlrId: testCntlr, TdId: testTd}); status.Code(err) !=
		codes.Unavailable {
		t.Fatalf("a thin device bitmap read: %v, want Unavailable", err)
	}
	if calls := node.Calls(); startCalls(node) != 0 {
		t.Fatalf("a starting agent touched the node before its pass: %v",
			calls)
	}

	if err := srv.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	round, _ = srv.checkCntlrRound(context.Background(),
		&pb.CheckCntlrRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: 3}, nil)
	if round.GetAgentReply().GetCode() != common.ReplyCodeUnknownObject {
		t.Fatalf("after the pass: code %d, want unknown object",
			round.GetAgentReply().GetCode())
	}
	if round.GetObsStamp().GetAgentStartTime() == nil {
		t.Fatalf("after the pass: the stamp lost the start time")
	}
	syncup, _ = srv.SyncupCn(ctx, cnReq(2, true))
	if code := syncup.GetAgentReply().GetCode(); code != 0 {
		t.Fatalf("a SyncupCn after the pass: code %d, want 0", code)
	}
}

// TestAStartingAgentHoldsAKnownCntlr pins SH33 on a cntlr the cn agent holds
// while it starts: a round answers starting and begins no read, though the
// object is known, and one whose pass ends between its reading and its
// answer still answers starting, so no round answers a known object from an
// observation it read while the agent started. Once the start has ended, a
// round answers from the object's observation and begins a read.
func TestAStartingAgentHoldsAKnownCntlr(t *testing.T) {
	node := newFakeNode()
	node.dirs[agent.NvmetRoot] = true
	srv := newCnServerOnPort(node, common.NvmetPortId)
	srv.roundReads = true
	ctx := t.Context()
	srv.Prepare(ctx)
	req := cntlrReq(reqOpts{revision: 3})
	key := cntlrKey(testCluster, testCn, req.GetCntlrPointer().GetSpId(),
		req.GetCntlrPointer().GetCntlrId())
	srv.putCntlr(key, newCntlrState(req))
	reading := func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return srv.cntlrs[key].obs.ReadingLocked()
	}
	round := func() *pb.CheckCntlrReply {
		reply, _ := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr(),
			Revision: 3}, nil)
		return reply
	}
	if code := round().GetAgentReply().GetCode(); code !=
		common.ReplyCodeStarting || reading() {
		t.Fatalf("a round of a known cntlr while starting: code %d, read "+
			"begun %t; want starting and no read", code, reading())
	}
	// The pass ends between the round's reading of its clock and its
	// answer.
	now := srv.now
	srv.now = func() time.Time {
		srv.startup.End()
		return now()
	}
	if code := round().GetAgentReply().GetCode(); code !=
		common.ReplyCodeStarting {
		t.Fatalf("a round the pass's end overtook: code %d, want starting",
			code)
	}
	srv.now = now
	// The node lock held keeps the read the next round begins waiting, so
	// the test sees it run.
	srv.locks.Node().Lock()
	reply := round()
	begun := reading()
	srv.locks.Node().Unlock()
	if code := reply.GetAgentReply().GetCode(); code ==
		common.ReplyCodeStarting {
		t.Fatal("a round after the start answered starting")
	}
	if !begun {
		t.Fatal("a round after the start began no read")
	}
}
