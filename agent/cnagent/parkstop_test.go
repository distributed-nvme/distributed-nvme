package cnagent

import (
	"context"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The park step (cnagent.md CN21's P0)
//
// L1's disable of an nvmet namespace waits, unkillable, for every request in
// flight on it, and a suspended dm device under the namespace never
// completes one, so a pass that ran L1 over a suspended ns-dev or transfer
// device would hold its locks in the kernel for as long as the device stays
// suspended. The park step moves every unwanted namespace inaccessible,
// resumes the devices of ours below, parks each unwanted ns-dev and transfer
// device, and stops the chain before L1 while any of them is not known out
// of suspension. The stand-in node does not wedge; these tests pin the order
// and the stop, and the lab pins the kernel's half.
// ---------------------------------------------------------------------------

// noNsReq is the fixture's request with the namespace dropped from the
// subsystem that stays.
func noNsReq(revision uint64) reqOpts {
	noNs := defaultSubsys(false)
	noNs[testNqn].NsList = nil
	return reqOpts{revision: revision, primary: true, subsys: noNs}
}

// syncupCntlrReply sends one request and returns its reply whatever its code.
func syncupCntlrReply(t *testing.T, srv *CnAgentServer, o reqOpts) *pb.AgentReply {
	t.Helper()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(o))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	return reply.GetAgentReply()
}

// TestTheParkStepMovesAnaFirst pins the order of the park step for a
// namespace dropped under a subsystem that stays and for a subsystem that
// leaves whole: the namespace moves to the inaccessible group, then its
// ns-dev is parked, then L1 disables it. A pass with nothing of ours
// suspended reads one `dmsetup info` listing and walks no table below the
// park.
func TestTheParkStepMovesAnaFirst(t *testing.T) {
	for _, tc := range []struct {
		name string
		to   reqOpts
	}{
		{"a namespace dropped", noNsReq(3)},
		{"a subsystem dropped", reqOpts{revision: 3, primary: true,
			subsys: map[string]*pb.Subsystem{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			nsDev := nsDevName(srv, testNs)
			nsPath := agent.NvmetRoot + "/subsystems/" + testNqn +
				"/namespaces/1"

			node.Reset()
			reply := syncupCntlrReply(t, srv, tc.to)
			cnSweepAssertCode(t, reply, 0, "the pass that removes")
			assertOrder(t, node,
				"writedirect "+anaPath(testNqn, 1)+"=3",
				"cmd dmsetup suspend "+nsDev,
				"cmd dmsetup reload "+nsDev,
				"cmd dmsetup resume "+nsDev,
				"writedirect "+nsPath+"/enable=0",
				"cmd dmsetup remove "+nsDev)
			listing := node.indexOfCall("cmd dmsetup info --columns " +
				"--noheadings --separator / -o name,major,minor,attr")
			park := node.indexOfCall("cmd dmsetup suspend " + nsDev)
			if listing < 0 || listing > park {
				t.Fatalf("no dmsetup info listing before the park")
			}
			for _, call := range node.Calls()[listing:park] {
				if strings.HasPrefix(call, "cmd dmsetup table ") &&
					call != "cmd dmsetup table "+nsDev {
					t.Fatalf("a table below the park was read with nothing "+
						"suspended: %q", call)
				}
			}
		})
	}
}

// TestAParkWhoseLoadFails covers the park's failures. A load onto the td's
// dm-error that is refused is followed at once by the plain error table, and
// the chain goes on. Both loads refused, or a `dmsetup info` of the ns-dev
// that does not answer, leave the device not known out of suspension, and
// the chain stops before L1: no disable, no rmdir; the reply names the ns-dev
// and the namespace, and the next converge, the hook gone, parks and removes
// them.
func TestAParkWhoseLoadFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(node *fakeNode, nsDev string)
		// stops says the first pass stops before L1.
		stops bool
	}{
		{"one load refused", func(node *fakeNode, nsDev string) {
			node.failCmd["dmsetup reload "+nsDev] =
				"device-mapper: reload ioctl on " + nsDev +
					" failed: No such device or address"
		}, false},
		{"both loads refused", func(node *fakeNode, nsDev string) {
			node.failCmdAlways["dmsetup reload "+nsDev] =
				"device-mapper: reload ioctl on " + nsDev +
					" failed: Cannot allocate memory"
		}, true},
		{"its dmsetup info does not answer", func(node *fakeNode, nsDev string) {
			node.killCmdNoEffectAlways["dmsetup info --columns "+
				"--noheadings -o attr "+nsDev] = true
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			nsDev := nsDevName(srv, testNs)
			nsPath := agent.NvmetRoot + "/subsystems/" + testNqn +
				"/namespaces/1"

			node.Reset()
			node.mu.Lock()
			tc.arm(node, nsDev)
			node.mu.Unlock()
			reply := syncupCntlrReply(t, srv, noNsReq(3))
			if !tc.stops {
				cnSweepAssertCode(t, reply, 0, "the pass")
				reloads := node.callsMatching("cmd dmsetup reload " + nsDev)
				if len(reloads) != 2 || !strings.HasSuffix(reloads[1],
					" error") {
					t.Fatalf("reloads %q, want the refused one and then the "+
						"plain error table", reloads)
				}
				assertOrder(t, node, reloads[1],
					"writedirect "+nsPath+"/enable=0",
					"cmd rmdir "+nsPath)
				return
			}
			cnSweepAssertCode(t, reply, common.ReplyCodeLeftover,
				"the stopped pass")
			cnSweepAssertDetails(t, reply,
				agent.LeftoverKindDm+":"+nsDev, "the stopped pass")
			cnSweepAssertDetails(t, reply,
				agent.LeftoverKindNvmetNs+":"+testNqn+"/1", "the stopped pass")
			assertNoCall(t, node, "writedirect "+nsPath+"/enable=0")
			assertNoCall(t, node, "cmd rmdir "+nsPath)
			if !node.dirs[nsPath] {
				t.Fatalf("the namespace was removed by a stopped pass")
			}
			if got := node.fileOf(anaPath(testNqn, 1)); got != "3" {
				t.Fatalf("ana_grpid is %q after the stopped pass, want 3",
					got)
			}

			// The re-send, the hook gone.
			node.mu.Lock()
			node.failCmdAlways = make(map[string]string)
			node.killCmdNoEffectAlways = make(map[string]bool)
			node.mu.Unlock()
			node.Reset()
			reply = syncupCntlrReply(t, srv, noNsReq(3))
			cnSweepAssertCode(t, reply, 0, "the re-sent pass")
			assertOrder(t, node,
				"cmd dmsetup reload "+nsDev,
				"cmd dmsetup resume "+nsDev,
				"writedirect "+nsPath+"/enable=0",
				"cmd rmdir "+nsPath,
				"cmd dmsetup remove "+nsDev)
		})
	}
}

// TestTheParkResumesASuspendedDeviceBelow pins the release before the park:
// a device of ours below an unwanted ns-dev, here its raid0, that a failed
// reload or quiesce left suspended holds the ns-dev's IO, and the park's
// flushing suspend would wait for it. The raid0 is resumed, on its live
// table, before the ns-dev is suspended.
func TestTheParkResumesASuspendedDeviceBelow(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	nsDev := nsDevName(srv, testNs)
	raid0 := raid0Name(srv, testTd)
	node.mu.Lock()
	node.dms[raid0].suspended = true
	node.mu.Unlock()

	node.Reset()
	reply := syncupCntlrReply(t, srv, noNsReq(3))
	cnSweepAssertCode(t, reply, 0, "the pass")
	assertOrder(t, node,
		"cmd dmsetup table "+raid0,
		"cmd dmsetup resume "+raid0,
		"cmd dmsetup suspend "+nsDev)
	if node.dms[raid0].suspended {
		t.Fatalf("the raid0 is still suspended")
	}
	for _, call := range node.callsMatching("cmd dmsetup reload " + raid0) {
		t.Fatalf("the raid0 was reloaded: %q", call)
	}
}

// TestAnUnwantedTransferDeviceIsParked pins the park of a transfer device
// that left the request: one a failed demote left suspended on its old table
// holds a remote clone's reads, and the transfer subsystem's removal waits on
// them as an ns-dev's does. The transfer's namespace moves inaccessible, the
// device is parked onto an error table of its size, and only then does L1
// disable the namespace; a park refused stops the chain as for an ns-dev.
func TestAnUnwantedTransferDeviceIsParked(t *testing.T) {
	xfers := []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn, OriNsIdx: 1}}
	for _, refused := range []bool{false, true} {
		name := "parked"
		if refused {
			name = "its park refused"
		}
		t.Run(name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				xfers: xfers})
			xfer := xferName(srv, testXfer)
			xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
			nsPath := agent.NvmetRoot + "/subsystems/" + xferNqn +
				"/namespaces/1"
			node.mu.Lock()
			if !strings.Contains(node.dms[xfer].table, " linear ") {
				node.mu.Unlock()
				t.Fatalf("fixture: the transfer device does not serve: %q",
					node.dms[xfer].table)
			}
			node.dms[xfer].suspended = true
			if refused {
				node.failCmdAlways["dmsetup reload "+xfer] =
					"device-mapper: reload ioctl on " + xfer +
						" failed: Cannot allocate memory"
			}
			node.mu.Unlock()

			node.Reset()
			reply := syncupCntlrReply(t, srv,
				reqOpts{revision: 3, primary: true})
			if refused {
				cnSweepAssertCode(t, reply, common.ReplyCodeLeftover,
					"the stopped pass")
				cnSweepAssertDetails(t, reply, agent.LeftoverKindDm+":"+xfer,
					"the stopped pass")
				assertNoCall(t, node, "writedirect "+nsPath+"/enable=0")
				assertOrder(t, node,
					"writedirect "+anaPath(xferNqn, 1)+"=3",
					"cmd dmsetup reload "+xfer)
				return
			}
			cnSweepAssertCode(t, reply, 0, "the pass")
			assertOrder(t, node,
				"writedirect "+anaPath(xferNqn, 1)+"=3",
				"cmd dmsetup reload "+xfer,
				"cmd dmsetup resume "+xfer,
				"writedirect "+nsPath+"/enable=0",
				"cmd dmsetup remove "+xfer)
			parked := node.callsMatching("cmd dmsetup reload " + xfer)
			if !strings.HasSuffix(parked[0], " error") {
				t.Fatalf("the transfer device was parked onto %q, want its "+
					"error table", parked[0])
			}
		})
	}
}
