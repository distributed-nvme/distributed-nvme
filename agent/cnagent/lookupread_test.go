package cnagent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// A leg's lookup fails naming a read that did not answer — the namespace's
// nsid, a controller's state, its directory listing, its path's ana_state —
// for every side of the leg (CN10): taken as absent, an nsid read would fail
// the leg with "no multipath namespace", and a state read would read the path
// as not live. A file that is gone still reads absent, which the agent
// package's tests of the host read pin. The converge connects nothing on
// that answer, and a read names nothing for the leg, neither a side to
// connect nor its wrapper (cnagent.md CN30), so the worker re-sends nothing
// for it: the next read whose lookup answers judges the leg again.
func TestALegLookupFailsNamingAReadThatDidNotAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(node *fakeNode, nqn string) string
		// listing: path is a directory whose listing is killed, not a file
		// whose read is.
		listing bool
	}{
		{"the namespace's nsid", func(node *fakeNode, nqn string) string {
			node.mu.Lock()
			defer node.mu.Unlock()
			subsys := node.subsystems[nqn]
			return fmt.Sprintf("%s/nvme-subsys%d/nvme%dn1/nsid",
				sysfsNvmeSubsysDir, subsys.idx, subsys.idx)
		}, false},
		{"a controller's state", func(node *fakeNode, nqn string) string {
			node.mu.Lock()
			defer node.mu.Unlock()
			return sysfsNvmeCtrlDir + "/" +
				node.subsystems[nqn].ctrls[0].name + "/state"
		}, false},
		{"a path's ana_state", func(node *fakeNode, nqn string) string {
			node.mu.Lock()
			defer node.mu.Unlock()
			ctrl := node.subsystems[nqn].ctrls[0]
			return sysfsNvmeCtrlDir + "/" + ctrl.name + "/" + ctrl.pathDev +
				"/ana_state"
		}, false},
		{"the second controller's state", func(node *fakeNode,
			nqn string) string {
			node.mu.Lock()
			defer node.mu.Unlock()
			return sysfsNvmeCtrlDir + "/" +
				node.subsystems[nqn].ctrls[1].name + "/state"
		}, false},
		{"a controller's directory listing", func(node *fakeNode,
			nqn string) string {
			node.mu.Lock()
			defer node.mu.Unlock()
			return sysfsNvmeCtrlDir + "/" + node.subsystems[nqn].ctrls[0].name
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			// Two sides per leg, so a case can read the second controller.
			opts := reqOpts{revision: 2, extraSide: true}
			syncupBoth(t, srv, opts)
			nqn := legNqn(srv, testDataLeg)
			path := tc.path(node, nqn)
			node.mu.Lock()
			if tc.listing {
				node.killCmdAlways["list "+path] = true
			} else {
				if _, ok := node.files[path]; !ok {
					node.mu.Unlock()
					t.Fatalf("fixture is wrong: the node has no %s", path)
				}
				node.killReadAlways[path] = true
			}
			node.mu.Unlock()

			row := checkRound(t, srv, 2).GetLegIdToLeg()[testDataLeg]
			if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
				!strings.Contains(row.GetDetails(), path) ||
				!strings.Contains(row.GetDetails(), errSubsysUnknown.Error()) {
				t.Fatalf("the read's leg row is %v, want ERROR naming %s "+
					"as unknown", row, path)
			}
			node.Reset()
			reply, err := srv.SyncupCntlr(t.Context(), cntlrReq(opts))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			row = reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg]
			if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
				!strings.Contains(row.GetDetails(), path) {
				t.Fatalf("the converge's leg row is %v, want ERROR naming %s",
					row, path)
			}
			assertNoCall(t, node, "cmd nvme connect")
			round, resent := resendRound(t, srv, cntlrReq(opts), "unanswered")
			if details := round.GetAgentReply().GetDetails(); strings.
				Contains(details, nqn) ||
				strings.Contains(details, legName(srv, testDataLeg)) {
				t.Fatalf("the read names the leg on an unanswered read: %v",
					round.GetAgentReply())
			}
			assertCodeZero(t, "the read", round)
			if resent != nil {
				t.Fatalf("the round re-sent the request on an unanswered read")
			}
		})
	}
}
