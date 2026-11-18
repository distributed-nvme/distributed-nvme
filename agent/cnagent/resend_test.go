package cnagent

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The worker's re-send (dnv-worker.md RW4), and nothing between its calls
// ---------------------------------------------------------------------------

// resendRound plays one of the worker's rounds of the fixture cntlr, as
// dnv-worker.md RW4 has it, under traceId: the cntlr's read the round before
// it started, then a CheckCntlr round at held's revision, which answers from
// that read, and, when the round's code is not zero or its revision is not
// held's, held sent again under the round's trace id. held is the request
// the worker holds for the cntlr. It returns the round's reply and the
// re-send's, nil when the round sent nothing.
func resendRound(
	t *testing.T,
	srv *CnAgentServer,
	held *pb.SyncupCntlrRequest,
	traceId string,
) (*pb.CheckCntlrReply, *pb.SyncupCntlrReply) {
	t.Helper()
	key := cntlrKey(testCluster, testCn, testSp, testCntlr)
	st := srv.getCntlr(key)
	if st == nil {
		t.Fatalf("%s: the fixture cntlr is not known", traceId)
	}
	srv.refreshCntlr(traced(traceId), key, st)
	return resendRoundOnly(t, srv, held, traceId)
}

// resendRoundOnly is resendRound with no read before the round: the round
// answers from the latest observation, a converge's included.
func resendRoundOnly(
	t *testing.T,
	srv *CnAgentServer,
	held *pb.SyncupCntlrRequest,
	traceId string,
) (*pb.CheckCntlrReply, *pb.SyncupCntlrReply) {
	t.Helper()
	round := roundCntlr(srv, traceId, held.GetRevision())
	if round.GetAgentReply().GetCode() == 0 &&
		round.GetRevision() == held.GetRevision() {
		return round, nil
	}
	reply, err := srv.SyncupCntlr(traced(traceId), held)
	if err != nil {
		t.Fatalf("%s: the re-sent SyncupCntlr: %v", traceId, err)
	}
	return round, reply
}

// syncupCntlrTraced is syncupCntlr under traceId, as the worker sends a
// re-send under the trace id of the round that brought it (dnv-worker.md
// RW4).
func (w *fakeWorker) syncupCntlrTraced(
	req *pb.SyncupCntlrRequest,
	traceId string,
) *pb.SyncupCntlrReply {
	w.t.Helper()
	ctx, cancel := w.callCtx()
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, common.TraceIdMetadataKey,
		traceId)
	reply, err := w.client.SyncupCntlr(ctx, req)
	if err != nil {
		w.t.Fatalf("%s: SyncupCntlr %s rev %d: %v", traceId,
			cntlrPointerText(req.GetCntlrPointer()), req.GetRevision(), err)
	}
	return reply
}

// resendRound is one of the worker's rounds of the cntlr held names, under
// traceId, as dnv-worker.md RW4 has it: a CheckCntlr round at held's
// revision, then, when the round's code is not zero or its revision is not
// held's, held sent again under the round's trace id. It returns the
// round's reply and the re-send's, nil when the round sent nothing.
func (w *fakeWorker) resendRound(
	held *pb.SyncupCntlrRequest,
	traceId string,
) (*pb.CheckCntlrReply, *pb.SyncupCntlrReply) {
	w.t.Helper()
	round := w.promptRound(held.GetCntlrPointer(), held.GetRevision(),
		traceId)
	if round.GetAgentReply().GetCode() == 0 &&
		round.GetRevision() == held.GetRevision() {
		return round, nil
	}
	return round, w.syncupCntlrTraced(held, traceId)
}

// quietWait is how long a test leaves the agent alone before it reads what
// ran meanwhile: many times the pace at which a converge repeated in the
// tests whose agent re-ran a stopped build by itself.
const quietWait = 100 * time.Millisecond

// agentMadeCalls lists the calls the fake node ran under trace ids the test
// did not give: none of own, nor the empty id of a call the test made on a
// bare context. Such an id is one the agent made up itself (dnagent.md SH2):
// a converge it ran by itself, a round's read that carried no id, a
// sweep's background disconnect from a pass with no id, or a path watcher's
// tick (CN10). A test that keeps the probe interval long has no tick.
func agentMadeCalls(node *fakeNode, own ...string) map[string][]string {
	known := map[string]bool{"": true}
	for _, id := range own {
		known[id] = true
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	out := make(map[string][]string)
	for id, calls := range node.traced {
		if known[id] {
			continue
		}
		out[id] = append(out[id], calls...)
	}
	return out
}

// tracedCall reports whether a call that holds substr ran under traceId.
func tracedCall(node *fakeNode, traceId, substr string) bool {
	for _, call := range node.Traced(traceId) {
		if strings.Contains(call, substr) {
			return true
		}
	}
	return false
}

// tracedCalls lists the calls that hold substr and ran under traceId.
func tracedCalls(node *fakeNode, traceId, substr string) []string {
	var out []string
	for _, call := range node.Traced(traceId) {
		if strings.Contains(call, substr) {
			out = append(out, call)
		}
	}
	return out
}

// forgetTraces drops every call the fake node filed by trace id so far, for
// a test whose setup ran calls under ids the agent made itself — a read a
// round with no id started, the startup reconcile — before the part it
// checks.
func (f *fakeNode) forgetTraces() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.traced = nil
}

// assertNothingRunsBetweenCalls leaves the agent alone for quietWait and
// fails if anything ran meanwhile, or before, under an id the agent made up
// itself (agentMadeCalls): nothing converges a cntlr again between the
// worker's calls.
func assertNothingRunsBetweenCalls(
	t *testing.T,
	node *fakeNode,
	own ...string,
) {
	t.Helper()
	time.Sleep(quietWait)
	if calls := agentMadeCalls(node, own...); len(calls) > 0 {
		t.Errorf("calls ran under ids the agent made itself: %v", calls)
	}
}

// assertCodeNames fails unless reply's code is the leftover code and its
// details hold every one of names.
func assertCodeNames(
	t *testing.T,
	what string,
	reply interface{ GetAgentReply() *pb.AgentReply },
	names ...string,
) {
	t.Helper()
	got := reply.GetAgentReply()
	if got.GetCode() != common.ReplyCodeLeftover {
		t.Errorf("%s: code %d (%s), want the leftover code naming %q", what,
			got.GetCode(), got.GetDetails(), names)
		return
	}
	for _, name := range names {
		if !strings.Contains(got.GetDetails(), name) {
			t.Errorf("%s: details %q do not name %q", what, got.GetDetails(),
				name)
		}
	}
}

// assertCodeZero fails unless reply's code is zero: nothing a converge
// would act on is named.
func assertCodeZero(
	t *testing.T,
	what string,
	reply interface{ GetAgentReply() *pb.AgentReply },
) {
	t.Helper()
	if got := reply.GetAgentReply(); got.GetCode() != 0 {
		t.Errorf("%s: code %d (%s), want 0", what, got.GetCode(),
			got.GetDetails())
	}
}
