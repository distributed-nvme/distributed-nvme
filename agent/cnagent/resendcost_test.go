package cnagent

import (
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// What a fault costs when only the worker's re-send runs a converge again
// (cnagent.md CN30; dnv-worker.md RW4)
// ---------------------------------------------------------------------------

// costRoundInterval is the pace of the rounds these cases play: the worker's
// interval, shortened so a case need not wait out the production one.
const costRoundInterval = 200 * time.Millisecond

// lateLegFix builds the fixture primary's two-leg md-raid1 pool at revision 1
// through a fake worker whose rounds start their reads (dnagent.md SH29),
// and then kills leg 2's disk node for good: the host keeps its controller
// reconnecting, md fails the member, and the array runs degraded. It
// returns the request the worker then holds, at revision 2, which no
// converge has met yet, and forgets the calls filed so far by trace id.
func lateLegFix(t *testing.T) (*CnAgentServer, *fakeNode, *fakeWorker,
	*pb.SyncupCntlrRequest) {
	t.Helper()
	srv, node := newTestServer(t)
	srv.roundReads = true
	w := newFakeWorker(t, srv)
	if reply := w.syncupCn(cnReq(1, true)); reply.GetAgentReply().
		GetCode() != 0 {
		t.Fatalf("SyncupCn: %v", reply.GetAgentReply())
	}
	opts := reqOpts{revision: 1, primary: true, raid1: true, twoLegs: true}
	if reply := w.syncupCntlr(cntlrReq(opts)); reply.GetAgentReply().
		GetCode() != 0 {
		t.Fatalf("SyncupCntlr: %v", reply.GetAgentReply())
	}
	mdDev := srv.nf.MdPath(srv.nf.CnMdDevName(
		testCluster, testCn, testSp, 0, 0, false))
	leg2 := srv.nf.DmPath(legName(srv, testDataLeg2))
	node.setCtrlState(legNqn(srv, testDataLeg2), testIp2, testSvcId2,
		"connecting")
	node.setMemberState(mdDev, leg2, "faulty,failfast")
	node.setMdSync(mdDev, 1, mdActionIdle, "none")
	joinReads(t, srv)
	node.forgetTraces()
	opts.revision = 2
	return srv, node, w, cntlrReq(opts)
}

// TestALateLegOfADeadDiskNodeCostsNoSyncups: leg 2's disk node is dead for
// good — its controller reconnecting, md having failed its member — when a
// new revision reaches the primary. That one SyncupCntlr converges it, and
// no converge can make the leg serve, so nothing names it (cnagent.md CN12,
// CN30): over twenty rounds every read answers code 0 at the revision, the
// worker re-sends nothing, and no command runs under an id the agent made
// itself. A converge the agent ran again by itself for the late member
// would show as such commands.
func TestALateLegOfADeadDiskNodeCostsNoSyncups(t *testing.T) {
	srv, node, w, held := lateLegFix(t)
	reply := w.syncupCntlrTraced(held, "syncup")
	assertCodeZero(t, "the syncup that meets the late leg", reply)
	if row := reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg2]; row.
		GetStatus() == pb.ResStatus_RES_STATUS_OK {
		t.Fatalf("the case is vacuous: leg 2 reads OK: %v", row)
	}
	own := []string{"syncup"}
	syncups := 1
	for i := 0; i < 20; i++ {
		time.Sleep(10 * time.Millisecond)
		id := fmt.Sprintf("round-%d", i)
		own = append(own, id)
		round, resent := w.resendRound(held, id)
		joinReads(t, srv)
		if resent != nil {
			syncups++
		}
		assertCodeZero(t, id, round)
		if round.GetRevision() != 2 {
			t.Errorf("%s answered revision %d, want 2", id,
				round.GetRevision())
		}
		if len(node.Traced(id)) == 0 {
			t.Errorf("%s started no read", id)
		}
	}
	if syncups != 1 {
		t.Errorf("%d SyncupCntlr calls, want the one that met the late leg",
			syncups)
	}
	assertNothingRunsBetweenCalls(t, node, own...)
}

// TestAStalledConvergeAgesNoReport plays the rounds of a primary whose leg 2
// is late for good, one worker interval apart. Nothing converges the cntlr
// between the worker's calls: each round starts the cntlr's read (dnagent.md
// SH29), the read ends before the next round, and so each round answers from
// the read the round before it started, an observation at most one interval
// plus that read's time old. Beside a converge that hangs in a command, a
// round answers at once from the last read, with the converge's start, and
// starts no read (dnagent.md SH30); once that converge ends, no converge
// follows it. A converge the agent ran again by itself would hold the
// cntlr's lock, keep the rounds' reads from running and leave rounds
// answering its own observations.
func TestAStalledConvergeAgesNoReport(t *testing.T) {
	srv, node, w, held := lateLegFix(t)
	assertCodeZero(t, "the syncup that meets the late leg",
		w.syncupCntlrTraced(held, "syncup"))
	ptr := held.GetCntlrPointer()

	own := []string{"syncup", "beside-the-hang", "after-the-hang"}
	prev, readTook := "", time.Duration(0)
	for i := 0; i < 5; i++ {
		time.Sleep(costRoundInterval)
		id := fmt.Sprintf("round-%d", i)
		own = append(own, id)
		round := w.promptRound(ptr, 2, id)
		started := time.Now()
		joinReads(t, srv)
		took := time.Since(started)
		if len(node.Traced(id)) == 0 {
			t.Errorf("%s started no read", id)
		}
		stamp := round.GetObsStamp()
		if prev != "" {
			age := stamp.GetAnswerTime().AsTime().Sub(
				stamp.GetObsTime().AsTime())
			if stamp.GetObsTraceId() != prev ||
				age > costRoundInterval+readTook+costRoundInterval/2 {
				t.Errorf("%s answered from %q, %v old; want %q's read, "+
					"at most %v plus its %v old", id,
					stamp.GetObsTraceId(), age, prev, costRoundInterval,
					readTook)
			}
		}
		assertCodeZero(t, id, round)
		prev, readTook = id, took
	}
	if calls := agentMadeCalls(node, own...); len(calls) > 0 {
		t.Errorf("calls ran under ids the agent made itself: %v", calls)
	}

	// A new revision's converge hangs in a command; a round beside it
	// answers at once from the last read and starts none.
	gate := metaLegAttr(srv)
	release := openGate(t, node, gate)
	next := proto.Clone(held).(*pb.SyncupCntlrRequest)
	next.Revision = 3
	node.Reset()
	done := w.asyncSyncupCntlr(next)
	waitUntil(t, "the converge's hang", func() bool {
		return node.hasCall(gate)
	})
	round := w.promptRound(ptr, 3, "beside-the-hang")
	if got := round.GetObsStamp(); got.GetObsTraceId() != prev ||
		got.GetConvergeStart() == nil {
		t.Errorf("the round beside the hang = %v; want %q's read and the "+
			"converge's start", got, prev)
	}
	if calls := node.Traced("beside-the-hang"); len(calls) != 0 {
		t.Errorf("the round beside the hang started a read: %v", calls)
	}
	release()
	assertCodeZero(t, "the converge that hung",
		w.awaitCntlr(done, "the converge that hung"))
	w.promptRound(ptr, 3, "after-the-hang")
	joinReads(t, srv)
	assertNothingRunsBetweenCalls(t, node, own...)
}

// TestADeletedControllerOfADeadDiskNodeCostsAReSendEveryOtherRound: leg 2's
// disk node is dead and the kernel deleted its controller, as after a
// reconnect nvmet refused with do-not-retry or a CN reboot, so only a
// converge's connect could bring it back (cnagent.md CN10). A read names the
// side with no controller (CN30) and the worker re-sends on that round
// (dnv-worker.md RW4); the converge's connect fails, and its own reply does
// not name the side, so the round after answers from that converge's
// observation, code 0, starts a read, and the round after that answers the
// read's name again. The worker re-sends every other round at most, each
// re-send making the connect under its round's trace id, and nothing runs
// between the worker's calls.
func TestADeletedControllerOfADeadDiskNodeCostsAReSendEveryOtherRound(
	t *testing.T,
) {
	srv, node, w, held := lateLegFix(t)
	nqn2 := legNqn(srv, testDataLeg2)
	dropLegCtrl(t, node, nqn2, testIp2, testSvcId2)
	node.mu.Lock()
	node.failCmdAlways["--nqn "+nqn2+" --hostnqn"] =
		"nvme connect: Connection refused"
	node.mu.Unlock()
	assertCodeZero(t, "the syncup that meets the dead disk node",
		w.syncupCntlrTraced(held, "syncup"))

	const rounds = 12
	side := noCtrlName(nqn2, testIp2, testSvcId2)
	own := []string{"syncup"}
	resends, lastResent := 0, -2
	for i := 0; i < rounds; i++ {
		time.Sleep(10 * time.Millisecond)
		id := fmt.Sprintf("round-%d", i)
		own = append(own, id)
		round, resent := w.resendRound(held, id)
		joinReads(t, srv)
		if resent == nil {
			continue
		}
		resends++
		if lastResent == i-1 {
			t.Errorf("%s re-sent right after the re-send of round %d",
				id, i-1)
		}
		lastResent = i
		assertCodeNames(t, id, round, side)
		if connectsUnder(node, id, nqn2) == 0 {
			t.Errorf("the re-send of %s made no connect of the side under "+
				"its id", id)
		}
		if code := resent.GetAgentReply().GetCode(); code != 0 {
			t.Errorf("the re-send of %s replied code %d (%s), want 0: "+
				"its converge names no side", id, code,
				resent.GetAgentReply().GetDetails())
		}
	}
	// The name stands: from the first round whose read names it, every
	// other round re-sends.
	if resends < rounds/2-1 || resends > (rounds+1)/2 {
		t.Errorf("%d re-sends over %d rounds, want one every other round",
			resends, rounds)
	}
	assertNothingRunsBetweenCalls(t, node, own...)
}

// TestARowThatKeepsFailingIsReSentEveryRound pins the cost of a row both
// channels name while its step keeps failing: the port link of the
// cntlr's subsystem, which configfs keeps refusing. The converge's own reply
// names the subsystem (CN16, CN30), so every round answers from the last
// re-send's observation, names it, and re-sends at once, and no round
// answers from a read: each re-send cancels the read its round started, or
// stores its observation after it (dnagent.md SH30). The observation each
// round answers from is the previous round's re-send's, at most one
// interval plus that converge's time old, and nothing runs between the
// worker's calls.
func TestARowThatKeepsFailingIsReSentEveryRound(t *testing.T) {
	srv, node := newTestServer(t)
	srv.roundReads = true
	w := newFakeWorker(t, srv)
	if reply := w.syncupCn(cnReq(1, true)); reply.GetAgentReply().
		GetCode() != 0 {
		t.Fatalf("SyncupCn: %v", reply.GetAgentReply())
	}
	held := cntlrReq(reqOpts{revision: 1, primary: true})
	if reply := w.syncupCntlr(held); reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCntlr: %v", reply.GetAgentReply())
	}
	ssPath := agent.NvmetRoot + "/subsystems/" + testNqn
	node.mu.Lock()
	delete(node.links, agent.NvmetRoot+"/ports/1/subsystems/"+testNqn)
	node.failCmdAlways["ln -s "+ssPath+" "] = "ln: Operation not permitted"
	node.mu.Unlock()
	joinReads(t, srv)
	node.forgetTraces()

	ptr := held.GetCntlrPointer()
	own := []string{"first"}
	first := w.promptRound(ptr, 1, "first")
	joinReads(t, srv)
	assertCodeZero(t, "the round before the read", first)
	prevId := "first"
	var prevObs time.Time
	for i := 0; i < 6; i++ {
		time.Sleep(costRoundInterval)
		id := fmt.Sprintf("round-%d", i)
		own = append(own, id)
		round, resent := w.resendRound(held, id)
		joinReads(t, srv)
		assertCodeNames(t, id, round, testNqn)
		if resent == nil {
			t.Fatalf("%s brought no re-send", id)
		}
		assertCodeNames(t, "the re-send of "+id, resent, testNqn)
		stamp := round.GetObsStamp()
		if i > 0 {
			age := stamp.GetAnswerTime().AsTime().Sub(
				stamp.GetObsTime().AsTime())
			if stamp.GetObsTraceId() != prevId ||
				!stamp.GetObsTime().AsTime().Equal(prevObs) ||
				age > costRoundInterval+costRoundInterval/2 {
				t.Errorf("%s answered from %q at %v, %v old; want the "+
					"re-send of %s at %v, at most %v old", id,
					stamp.GetObsTraceId(), stamp.GetObsTime().AsTime(), age,
					prevId, prevObs, costRoundInterval)
			}
		}
		prevId = id
		prevObs = resent.GetObsStamp().GetObsTime().AsTime()
	}
	assertNothingRunsBetweenCalls(t, node, own...)
}
