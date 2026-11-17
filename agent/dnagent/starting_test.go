package dnagent

import (
	"context"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestAStartingAgentAnswersStarting pins SH33 on the dn agent: a server
// Prepare began answers, until its startup pass ends, every round and every
// info read with the starting code and the agent's start time, and refuses a Syncup* and a push; once Reconcile
// ends, the same requests are answered as SH25 and SH28 say.
func TestAStartingAgentAnswersStarting(t *testing.T) {
	node := newFakeNode()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	srv := NewDnAgentServer(node.osClient(), nf,
		common.DefaultLocalStorPrefix, testDisk, testTrConf(),
		common.NvmetPortId, 0)
	srv.roundReads = false
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		srv.WaitBackground()
	})
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
	dnRound, _ := srv.checkDnRound(ctx, &pb.CheckDnRequest{
		ClusterId: testCluster, DnId: testDn, Revision: 2}, nil)
	wantStarting("a dn round", dnRound.GetAgentReply(), dnRound.GetObsStamp())
	sideRound, _ := srv.checkSideRound(ctx, &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
		Revision: 3}, nil)
	wantStarting("a side round", sideRound.GetAgentReply(),
		sideRound.GetObsStamp())
	info, _ := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide)})
	wantStarting("an info read", info.GetAgentReply(), info.GetObsStamp())
	dnInfo, _ := srv.GetDnInfo(ctx, &pb.GetDnInfoRequest{
		ClusterId: testCluster, DnId: testDn})
	wantStarting("a dn info read", dnInfo.GetAgentReply(),
		dnInfo.GetObsStamp())
	dn, _ := srv.SyncupDn(ctx, dnReq(2, testSide))
	wantStarting("a SyncupDn", dn.GetAgentReply(), dn.GetObsStamp())
	side, _ := srv.SyncupSide(ctx, sideReq(3, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE))
	wantStarting("a SyncupSide", side.GetAgentReply(), side.GetObsStamp())
	push, _ := srv.PushMigrBitmap(ctx, &pb.PushMigrBitmapRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide)})
	wantStarting("a push", push.GetAgentReply(), nil)
	if calls := node.Calls(); len(calls) != 0 {
		t.Fatalf("a starting agent touched the node before its pass: %v",
			calls)
	}

	if err := srv.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	dnRound, _ = srv.checkDnRound(ctx, &pb.CheckDnRequest{
		ClusterId: testCluster, DnId: testDn, Revision: 2}, nil)
	if code := dnRound.GetAgentReply().GetCode(); code !=
		common.ReplyCodeUnknownObject {
		t.Fatalf("after the pass: code %d, want unknown object", code)
	}
	if dnRound.GetObsStamp().GetAgentStartTime() == nil {
		t.Fatalf("after the pass: the stamp lost the start time")
	}
	dn, _ = srv.SyncupDn(ctx, dnReq(2, testSide))
	if code := dn.GetAgentReply().GetCode(); code ==
		common.ReplyCodeStarting {
		t.Fatalf("a SyncupDn after the pass: still starting")
	}
}

// TestAStartingAgentHoldsAKnownSide pins SH33 on a side the dn agent holds
// while it starts: a round answers starting and begins no read, though the
// side is known; once the start has ended, a round answers from the side's
// observation and begins a read.
func TestAStartingAgentHoldsAKnownSide(t *testing.T) {
	node := newFakeNode()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	srv := NewDnAgentServer(node.osClient(), nf,
		common.DefaultLocalStorPrefix, testDisk, testTrConf(),
		common.NvmetPortId, 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		srv.WaitBackground()
	})
	srv.Prepare(ctx)
	key := sideKey(testCluster, testDn, testSp, testSide)
	srv.putSide(key, newSideState(sideReq(3, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)))
	reading := func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return srv.sides[key].obs.ReadingLocked()
	}
	round := func() *pb.CheckSideReply {
		reply, _ := srv.checkSideRound(ctx, &pb.CheckSideRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide), Revision: 3}, nil)
		return reply
	}
	if code := round().GetAgentReply().GetCode(); code !=
		common.ReplyCodeStarting || reading() {
		t.Fatalf("a round of a known side while starting: code %d, read "+
			"begun %t; want starting and no read", code, reading())
	}
	srv.startup.End()
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
