package cnagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// A leg's controller that the kernel deleted, as after a reconnect nvmet
// refused with do-not-retry, comes back only from a converge's connect
// (CN10), and the worker brings a converge only on a check reply's code
// (dnv-worker.md RW4). So a cntlr's read names each provisioned side of a
// wanted leg that has no controller, where the converge would connect it.

// dropLegCtrl takes away the controller of one side of a leg the way the
// kernel ends it: device, link and all.
func dropLegCtrl(
	t *testing.T,
	node *fakeNode,
	nqn, addr, svcId string,
) {
	t.Helper()
	node.mu.Lock()
	defer node.mu.Unlock()
	subsys, ok := node.subsystems[nqn]
	if !ok {
		t.Fatalf("no subsystem %s", nqn)
	}
	for _, ctrl := range subsys.ctrls {
		if ctrl.trAddr == addr && ctrl.trSvcId == svcId {
			node.dropCtrl(subsys, ctrl)
			return
		}
	}
	t.Fatalf("no controller of %s at %s:%s", nqn, addr, svcId)
}

// noCtrlName is how a verdict names a side with no controller.
func noCtrlName(nqn, addr, svcId string) string {
	return nqn + " at " + addr + ":" + svcId + ": no controller"
}

func TestACheckRoundNamesALegSideWithNoController(t *testing.T) {
	for _, tc := range []struct {
		name    string
		primary bool
		// window: the controller's device is gone while its subsystem
		// still lists it, as when the leg's wrapper holds the namespace
		// head open; otherwise the controller is gone with its entry.
		window bool
	}{
		{"primary", true, false},
		{"standby", false, false},
		{"primary, its device gone, its entry kept", true, true},
		{"standby, its device gone, its entry kept", false, true},
	} {
		primary := tc.primary
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			opts := reqOpts{revision: 2, primary: primary}
			syncupBoth(t, srv, opts)
			nqn := legNqn(srv, testDataLeg)
			if tc.window {
				if node.deleteCtrlDevice(nqn, testIp, testSvcId) == "" {
					t.Fatalf("no controller of %s at %s:%s", nqn,
						testIp, testSvcId)
				}
			} else {
				dropLegCtrl(t, node, nqn, testIp, testSvcId)
			}

			reply := checkCntlrVerdict(t, srv, opts.revision)
			want := noCtrlName(nqn, testIp, testSvcId)
			if reply.GetCode() != common.ReplyCodeLeftover ||
				!strings.Contains(reply.GetDetails(), want) {
				t.Fatalf("verdict = %v, want a leftover naming %q",
					reply, want)
			}

			node.Reset()
			if _, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(opts)); err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if got := connectsTo(node, nqn, testIp, testSvcId); got != 1 {
				t.Fatalf("the re-sent SyncupCntlr made %d connects of the "+
					"side, want 1", got)
			}
			if after := checkCntlrVerdict(t, srv, opts.revision); strings.
				Contains(after.GetDetails(), "no controller") {
				t.Fatalf("verdict after the reconnect = %v, want no side "+
					"named", after)
			}
		})
	}
}

// TestACheckRoundDoesNotNameASideTheConvergeWouldNotConnect bounds the
// verdict: a controller that is there in any state, a side not provisioned,
// a leg whose lookup did not answer, a leg the level suppresses and a leg
// whose disconnect is in flight are each left alone, as the converge leaves
// them.
func TestACheckRoundDoesNotNameASideTheConvergeWouldNotConnect(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts reqOpts
		// pre runs before the first converge, arm after it.
		pre func(node *fakeNode)
		arm func(t *testing.T, srv *CnAgentServer, node *fakeNode, nqn string)
	}{
		{"a controller that is connecting", reqOpts{revision: 2}, nil,
			func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				nqn string) {
				node.setCtrlState(nqn, testIp, testSvcId, "connecting")
			}},
		// The DN exports nothing for a side not provisioned, so a connect
		// to it would be refused.
		{"a side not provisioned",
			reqOpts{revision: 2, extraSide: true, unprovisionedExtraSide: true},
			func(node *fakeNode) {
				node.mu.Lock()
				defer node.mu.Unlock()
				node.failCmdAlways["--traddr "+testIp2+" "] =
					"nvme connect: Connection refused"
			}, nil},
		{"a lookup that did not answer", reqOpts{revision: 2}, nil,
			func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				nqn string) {
				dropLegCtrl(t, node, nqn, testIp, testSvcId)
				// The read's walk does not answer, so every lookup fails.
				node.mu.Lock()
				node.killCmdAlways["list "+sysfsNvmeSubsysDir] = true
				node.mu.Unlock()
			}},
		{"a leg the level suppresses",
			reqOpts{revision: 2, level: pb.SpLevel_SP_LEVEL_NO_SIDE}, nil,
			func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				nqn string) {
			}},
		{"a leg whose disconnect is in flight", reqOpts{revision: 2}, nil,
			func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				nqn string) {
				dropLegCtrl(t, node, nqn, testIp, testSvcId)
				srv.mu.Lock()
				srv.disconnecting[nqn] = struct{}{}
				srv.mu.Unlock()
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			if tc.pre != nil {
				tc.pre(node)
			}
			syncupBoth(t, srv, tc.opts)
			nqn := legNqn(srv, testDataLeg)
			if tc.arm != nil {
				tc.arm(t, srv, node, nqn)
			}
			reply := checkCntlrVerdict(t, srv, tc.opts.revision)
			if strings.Contains(reply.GetDetails(), "no controller") {
				t.Fatalf("verdict = %v, want no side named", reply)
			}
		})
	}
}

// TestAPrimaryLegRowKeepsItsProberWhenItsLookupFails: the read looks up a
// primary's legs for the sides it names (CN30), and a lookup that did not
// answer names none; the primary's leg row stays its prober's verdict
// (CN11), never the lookup's error.
func TestAPrimaryLegRowKeepsItsProberWhenItsLookupFails(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	before := checkRound(t, srv, 2).GetLegIdToLeg()[testDataLeg]
	node.mu.Lock()
	node.killCmdAlways["list "+sysfsNvmeSubsysDir] = true
	node.mu.Unlock()
	after := checkRound(t, srv, 2).GetLegIdToLeg()[testDataLeg]
	if after.GetStatus() != before.GetStatus() ||
		after.GetDetails() != before.GetDetails() {
		t.Fatalf("the primary's leg row went from %v to %v when its "+
			"lookup did not answer", before, after)
	}
}

// TestAPrimaryLooksUpAOneSidedLegOnlyWhenItsRowIsNotOk pins the cost of the
// names above on a primary: a one-sided leg whose prober's row reads OK has a
// live path through its one side, so the read takes no lookup of it. A
// controller that goes after that probe is named once the leg's next probe
// has failed and its row no longer reads OK.
func TestAPrimaryLooksUpAOneSidedLegOnlyWhenItsRowIsNotOk(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	opts := reqOpts{revision: 2, primary: true}
	syncupBoth(t, srv, opts)
	probeLegs(t, srv, testMetaLeg, testDataLeg)
	nqn := legNqn(srv, testDataLeg)
	entry := "list " + subsysDirOf(t, node, nqn)
	node.Reset()
	if reply := checkCntlrVerdict(t, srv, 2); reply.GetCode() != 0 {
		t.Fatalf("verdict over probed legs = %v, want code 0", reply)
	}
	if n := countCalls(node, entry); n != 0 {
		t.Fatalf("the read listed a probed one-sided leg's entry %d "+
			"times, want 0", n)
	}

	dropLegCtrl(t, node, nqn, testIp, testSvcId)
	if reply := checkCntlrVerdict(t, srv, 2); strings.Contains(
		reply.GetDetails(), "no controller") {
		t.Fatalf("verdict = %v before the leg's probe failed, want the "+
			"side not named yet", reply)
	}
	st := legState(t, srv)
	srv.mu.Lock()
	var prober *legProber
	for _, p := range st.probers {
		if p.legId == testDataLeg {
			prober = p
		}
	}
	srv.mu.Unlock()
	// The probe's IO through the leg's wrapper fails, as it does once the
	// leg's only path is gone.
	node.mu.Lock()
	devNo := node.devNo[prober.path]
	delete(node.devNo, prober.path)
	node.mu.Unlock()
	srv.runLegProbe(context.Background(), prober, testCn)
	node.mu.Lock()
	node.devNo[prober.path] = devNo
	node.mu.Unlock()
	reply := checkCntlrVerdict(t, srv, 2)
	if !strings.Contains(reply.GetDetails(),
		noCtrlName(nqn, testIp, testSvcId)) {
		t.Fatalf("verdict after the failed probe = %v, want the side named",
			reply)
	}
}
