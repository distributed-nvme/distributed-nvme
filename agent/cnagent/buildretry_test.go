package cnagent

import (
	"context"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// A failed export step and a failed group build go to a row, and the worker
// re-sends a SyncupCntlr only on a reply's code or a new revision, never on a
// row (dnv-worker.md RW4). So each registers the CN10 retry, whose attempt
// builds the object once its fault has cleared (CN10, CN12, CN16, CN17).

func TestAFailedBuildStepRegistersTheRetry(t *testing.T) {
	xfers := []*pb.Transfer{{
		XferId: testXfer, OriNqn: testNqn, OriNsIdx: 1, AutoSuspend: true,
	}}
	ssPath := agent.NvmetRoot + "/subsystems/" + testNqn
	for _, tc := range []struct {
		name string
		opts reqOpts
		// fail is the command line part that fails once.
		fail func(srv *CnAgentServer) string
		// built reports whether the node holds the object again.
		built func(srv *CnAgentServer, node *fakeNode) bool
	}{
		{"the subsystem's mkdir", reqOpts{revision: 2, primary: true},
			func(*CnAgentServer) string { return "mkdir -p " + ssPath },
			func(_ *CnAgentServer, node *fakeNode) bool {
				return node.dirs[ssPath]
			}},
		{"the namespace's mkdir", reqOpts{revision: 2, primary: true},
			func(*CnAgentServer) string {
				return "mkdir -p " + ssPath + "/namespaces/1"
			},
			func(_ *CnAgentServer, node *fakeNode) bool {
				return node.dirs[ssPath+"/namespaces/1"]
			}},
		{"the subsystem's port link", reqOpts{revision: 2, primary: true},
			func(*CnAgentServer) string { return "ln -s " + ssPath + " " },
			func(_ *CnAgentServer, node *fakeNode) bool {
				_, ok := node.links[agent.NvmetRoot+"/ports/1/subsystems/"+
					testNqn]
				return ok
			}},
		{"a transfer's subsystem mkdir",
			reqOpts{revision: 2, primary: true, xfers: xfers},
			func(srv *CnAgentServer) string {
				return "mkdir -p " + agent.NvmetRoot + "/subsystems/" +
					names(srv).XferNqn(testCluster, testSp, testXfer)
			},
			func(srv *CnAgentServer, node *fakeNode) bool {
				return node.dirs[agent.NvmetRoot+"/subsystems/"+
					names(srv).XferNqn(testCluster, testSp, testXfer)]
			}},
		{"a transfer's namespace mkdir",
			reqOpts{revision: 2, primary: true, xfers: xfers},
			func(srv *CnAgentServer) string {
				return "mkdir -p " + agent.NvmetRoot + "/subsystems/" +
					names(srv).XferNqn(testCluster, testSp, testXfer) +
					"/namespaces/1"
			},
			func(srv *CnAgentServer, node *fakeNode) bool {
				return node.dirs[agent.NvmetRoot+"/subsystems/"+
					names(srv).XferNqn(testCluster, testSp, testXfer)+
					"/namespaces/1"]
			}},
		// A standby wants its transfer inaccessible; its export's failed
		// step registers the retry all the same.
		{"a standby's transfer subsystem mkdir",
			reqOpts{revision: 2, primary: false, xfers: xfers},
			func(srv *CnAgentServer) string {
				return "mkdir -p " + agent.NvmetRoot + "/subsystems/" +
					names(srv).XferNqn(testCluster, testSp, testXfer)
			},
			func(srv *CnAgentServer, node *fakeNode) bool {
				return node.dirs[agent.NvmetRoot+"/subsystems/"+
					names(srv).XferNqn(testCluster, testSp, testXfer)]
			}},
		{"a transfer's port link",
			reqOpts{revision: 2, primary: true, xfers: xfers},
			func(srv *CnAgentServer) string {
				return "ln -s " + agent.NvmetRoot + "/subsystems/" +
					names(srv).XferNqn(testCluster, testSp, testXfer) + " "
			},
			func(srv *CnAgentServer, node *fakeNode) bool {
				_, ok := node.links[agent.NvmetRoot+"/ports/1/subsystems/"+
					names(srv).XferNqn(testCluster, testSp, testXfer)]
				return ok
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			node.mu.Lock()
			node.failCmd[tc.fail(srv)] = "refused once"
			node.mu.Unlock()
			syncupBoth(t, srv, tc.opts)
			if tc.built(srv, node) {
				t.Fatalf("fixture is wrong: the step did not fail")
			}
			if !retrying(t, srv) {
				t.Fatalf("the failed step registered no retry")
			}
			node.Reset()
			retryAttempt(t, srv)
			if !tc.built(srv, node) {
				t.Fatalf("the retry attempt did not build the object")
			}
			if retrying(t, srv) {
				t.Fatalf("the retry stayed registered after the build")
			}
		})
	}
}

// TestAFailedGroupBuildUnderAServingPoolRegistersTheRetry: a grow adds a
// group to a pool that serves, and the group's mdadm --create fails once.
// The pool serves on at its old size, so no namespace waits for the group
// and nothing else would register the retry; the group's failure does, and
// the attempt creates the array.
func TestAFailedGroupBuildUnderAServingPoolRegistersTheRetry(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, raid1: true})
	grow := cntlrReq(reqOpts{revision: 3, primary: true, raid1: true})
	grownDataGrp(grow, true)
	node.mu.Lock()
	node.failCmd["mdadm --create"] = "mdadm: create killed"
	node.mu.Unlock()
	reply, err := srv.SyncupCntlr(context.Background(), grow)
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	assertErrorDetails(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp2],
		"create killed", "the grown group")
	if !retrying(t, srv) {
		t.Fatalf("the failed create registered no retry")
	}
	node.Reset()
	retryAttempt(t, srv)
	if len(node.callsMatching("mdadm --create")) != 1 {
		t.Fatalf("the retry attempt made %q, want one create",
			node.callsMatching("mdadm --create"))
	}
	if retrying(t, srv) {
		t.Fatalf("the retry stayed registered after the create")
	}
}

// TestAFailedLinearGroupBuildRegistersTheRetry: a RedundNone grow's new group
// is a dm-linear over its leg, and its create fails once over an available
// leg. Under a pool that serves nothing else registers the retry; the
// group's failure does, and the attempt creates the device.
func TestAFailedLinearGroupBuildRegistersTheRetry(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	node.mu.Lock()
	node.failCmd["dmsetup create "+grpName(srv, testDataGrp2)] =
		"device-mapper: create ioctl failed"
	node.mu.Unlock()
	if _, err := srv.SyncupCntlr(context.Background(), growReq(3)); err != nil {
		t.Fatalf("grow: %v", err)
	}
	if !retrying(t, srv) {
		t.Fatalf("the failed dm-linear create registered no retry")
	}
	node.Reset()
	retryAttempt(t, srv)
	if !node.hasCall("dmsetup create " + grpName(srv, testDataGrp2)) {
		t.Fatalf("the retry attempt did not create the group's dm-linear")
	}
	if retrying(t, srv) {
		t.Fatalf("the retry stayed registered after the create")
	}
}

// TestAnArrayStateThePassLeavesAloneRegistersNoRetry bounds the build's
// retry: an array that does not run, and two arrays that both hold legs of
// the group, are states only an operator ends, so neither registers it.
func TestAnArrayStateThePassLeavesAloneRegistersNoRetry(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(f *repFix)
		want string
	}{
		{"an array that does not run", func(f *repFix) {
			f.node.setArrayState(f.mdDev, "inactive")
		}, "inactive"},
		{"two arrays that both hold legs of the group", func(f *repFix) {
			f.node.mu.Lock()
			name := f.node.arrays[f.mdDev].name
			f.node.mu.Unlock()
			f.node.seedArray("/dev/md/othername", name+"x", f.leg2)
		}, "both hold legs of the group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			f.syncup(t, quietLevel(3))
			tc.arm(f)
			f.node.Reset()
			info := f.syncup(t, quietLevel(4))
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
				tc.want, "grp data")
			if retrying(t, f.srv) {
				t.Fatalf("the state the pass leaves alone registered the " +
					"retry")
			}
		})
	}
}
