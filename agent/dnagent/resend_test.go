package dnagent

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The worker's re-send (dnv-worker.md RW4), and nothing between its calls
// ---------------------------------------------------------------------------

// workerRound plays one of the worker's rounds of the side held names, as
// dnv-worker.md RW4 has it, under traceId: the side's read, then a CheckSide
// round at held's revision, and, when the round's code is not zero or its
// revision is not held's, held sent again under the round's trace id — held
// is the request the worker holds for the side. It returns the round's reply
// and the re-send's, nil when the round sent nothing.
func workerRound(
	t *testing.T,
	srv *DnAgentServer,
	held *pb.SyncupSideRequest,
	traceId string,
) (*pb.CheckSideReply, *pb.SyncupSideReply) {
	t.Helper()
	ctx := common.WithTraceId(context.Background(), traceId)
	round, _ := checkSideNow(srv, ctx, &pb.CheckSideRequest{
		ClusterId: held.GetClusterId(), DnId: held.GetDnId(),
		SidePointer: held.GetSidePointer(), Revision: held.GetRevision(),
		ShowInfo: true, TraceId: traceId,
	}, nil)
	if round.GetAgentReply().GetCode() == 0 &&
		round.GetRevision() == held.GetRevision() {
		return round, nil
	}
	reply, err := srv.SyncupSide(ctx, held)
	if err != nil {
		t.Fatalf("%s: SyncupSide: %v", traceId, err)
	}
	return round, reply
}

// assertNamed fails unless reply's code is the leftover code and its details
// name every one of names (SH31): a row's resource name, which the row's
// text follows after ": ", or a leftover's, which follows its kind and ":"
// (agent.LeftoverReply).
func assertNamed(
	t *testing.T,
	what string,
	reply interface{ GetAgentReply() *pb.AgentReply },
	names ...string,
) {
	t.Helper()
	got := reply.GetAgentReply()
	if got.GetCode() != common.ReplyCodeLeftover {
		t.Errorf("%s: code %d (%s), want the leftover code naming %v", what,
			got.GetCode(), got.GetDetails(), names)
		return
	}
	for _, name := range names {
		details := got.GetDetails()
		if !strings.Contains(details, name+": ") &&
			!regexp.MustCompile(":"+regexp.QuoteMeta(name)+
				`(,|;|$| \[)`).MatchString(details) {
			t.Errorf("%s: details %q do not name %s", what, got.GetDetails(),
				name)
		}
	}
}

// assertNotNamed fails unless reply's code is zero: nothing a converge would
// act on is named.
func assertNotNamed(
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

// quietWait is how long a test leaves the agent alone before it reads what
// ran meanwhile: many times the pace at which a converge repeated in the
// tests whose agent re-ran a stopped build by itself.
const quietWait = 100 * time.Millisecond

// agentMadeCalls lists the calls the fake node ran under trace ids the test
// did not give: none of own, nor the empty id of a call the test made on a
// bare context. Such an id is one the agent made up itself (SH2) — a
// background converge's (reconvergeSide) or a round's read that carries no
// id — save the zeroing loop's batches, which run under ids of their own by
// design (DN9) and are left out: a read of the record, a discard.
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
		for _, call := range calls {
			if strings.HasPrefix(call, "readblock ") ||
				strings.HasPrefix(call, "discard ") ||
				strings.Contains(call, "blkdiscard") {
				continue
			}
			out[id] = append(out[id], call)
		}
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

// forgetTraces drops every call the fake node filed by trace id so far, for
// a test whose setup ran calls under ids the agent made itself — a read a
// round with no id started, a startup reconcile — before the part it checks.
func (f *fakeNode) forgetTraces() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.traced = nil
}

// assertNothingRunsBetweenCalls leaves the agent alone for quietWait and
// fails if anything ran meanwhile, or before, under an id the agent made up
// itself (agentMadeCalls): nothing converges a side again between the
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
