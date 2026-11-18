package cnagent

import (
	"context"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// A failed export step and a failed group build go to a row. Only a
// converge builds the object again, and the worker brings one only on a
// reply's code or a new revision (dnv-worker.md RW4). So a converge's reply
// names a failed export step, and a cntlr's read names the step's object, the
// group's absent array or a RedundNone group's dm-linear while a converge
// would build it (cnagent.md CN12, CN16, CN17, CN30); the one re-send the
// worker sends on that round builds it, and the read after it names
// nothing.

// TestAFailedExportStepIsNamedAndResent: a step of a namespace's or a
// transfer's export — the subsystem, its namespace or its port link —
// fails while the fault stands. The converge's reply names the step's
// object, the read names it while the fault stands and after it clears, a
// re-send while it stands builds nothing, and the re-send after it builds
// the object under the round's trace id; the read after that names
// nothing.
func TestAFailedExportStepIsNamedAndResent(t *testing.T) {
	xfers := []*pb.Transfer{{
		XferId: testXfer, OriNqn: testNqn, OriNsIdx: 1, AutoSuspend: true,
	}}
	ssPath := agent.NvmetRoot + "/subsystems/" + testNqn
	xferNqn := func(srv *CnAgentServer) string {
		return names(srv).XferNqn(testCluster, testSp, testXfer)
	}
	for _, tc := range []struct {
		name string
		opts reqOpts
		// fail is the command line part that fails while the fault stands.
		fail func(srv *CnAgentServer) string
		// named is the name of the row the verdicts name.
		named func(srv *CnAgentServer) string
		// built reports whether the node holds the object again.
		built func(srv *CnAgentServer, node *fakeNode) bool
	}{
		{"the subsystem's mkdir", reqOpts{revision: 2, primary: true},
			func(*CnAgentServer) string { return "mkdir -p " + ssPath },
			func(*CnAgentServer) string { return testNqn },
			func(_ *CnAgentServer, node *fakeNode) bool {
				return node.dirs[ssPath]
			}},
		{"the namespace's mkdir", reqOpts{revision: 2, primary: true},
			func(*CnAgentServer) string {
				return "mkdir -p " + ssPath + "/namespaces/1"
			},
			func(*CnAgentServer) string { return nsResName(testNqn, 1) },
			func(_ *CnAgentServer, node *fakeNode) bool {
				return node.dirs[ssPath+"/namespaces/1"]
			}},
		{"the subsystem's port link", reqOpts{revision: 2, primary: true},
			func(*CnAgentServer) string { return "ln -s " + ssPath + " " },
			func(*CnAgentServer) string { return testNqn },
			func(_ *CnAgentServer, node *fakeNode) bool {
				_, ok := node.links[agent.NvmetRoot+"/ports/1/subsystems/"+
					testNqn]
				return ok
			}},
		{"a transfer's subsystem mkdir",
			reqOpts{revision: 2, primary: true, xfers: xfers},
			func(srv *CnAgentServer) string {
				return "mkdir -p " + agent.NvmetRoot + "/subsystems/" +
					xferNqn(srv)
			},
			xferNqn,
			func(srv *CnAgentServer, node *fakeNode) bool {
				return node.dirs[agent.NvmetRoot+"/subsystems/"+xferNqn(srv)]
			}},
		{"a transfer's namespace mkdir",
			reqOpts{revision: 2, primary: true, xfers: xfers},
			func(srv *CnAgentServer) string {
				return "mkdir -p " + agent.NvmetRoot + "/subsystems/" +
					xferNqn(srv) + "/namespaces/1"
			},
			func(srv *CnAgentServer) string {
				return nsResName(xferNqn(srv), 1)
			},
			func(srv *CnAgentServer, node *fakeNode) bool {
				return node.dirs[agent.NvmetRoot+"/subsystems/"+
					xferNqn(srv)+"/namespaces/1"]
			}},
		// A standby wants its transfer inaccessible; its export's failed
		// step is named all the same.
		{"a standby's transfer subsystem mkdir",
			reqOpts{revision: 2, primary: false, xfers: xfers},
			func(srv *CnAgentServer) string {
				return "mkdir -p " + agent.NvmetRoot + "/subsystems/" +
					xferNqn(srv)
			},
			xferNqn,
			func(srv *CnAgentServer, node *fakeNode) bool {
				return node.dirs[agent.NvmetRoot+"/subsystems/"+xferNqn(srv)]
			}},
		{"a transfer's port link",
			reqOpts{revision: 2, primary: true, xfers: xfers},
			func(srv *CnAgentServer) string {
				return "ln -s " + agent.NvmetRoot + "/subsystems/" +
					xferNqn(srv) + " "
			},
			xferNqn,
			func(srv *CnAgentServer, node *fakeNode) bool {
				_, ok := node.links[agent.NvmetRoot+"/ports/1/subsystems/"+
					xferNqn(srv)]
				return ok
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			fail := tc.fail(srv)
			named := tc.named(srv) + ": "
			node.mu.Lock()
			node.failCmdAlways[fail] = "refused"
			node.mu.Unlock()
			opts := tc.opts
			opts.leftoverOk = true
			held := cntlrReq(tc.opts)
			assertCodeNames(t, "the converge", syncupBoth(t, srv, opts),
				named)
			if tc.built(srv, node) {
				t.Fatalf("fixture is wrong: the step did not fail")
			}

			round, resent := resendRound(t, srv, held, "fault-stands")
			assertCodeNames(t, "the read while the fault stands", round,
				named)
			if resent == nil {
				t.Fatalf("the round that names the step brought no re-send")
			}
			assertCodeNames(t, "the re-send while the fault stands", resent,
				named)
			if tc.built(srv, node) {
				t.Fatalf("the object was built while the fault stands")
			}

			node.mu.Lock()
			delete(node.failCmdAlways, fail)
			node.mu.Unlock()
			round, resent = resendRound(t, srv, held, "fault-cleared")
			assertCodeNames(t, "the read after the fault", round, named)
			if resent == nil {
				t.Fatalf("the round after the fault brought no re-send")
			}
			assertCodeZero(t, "the re-send after the fault", resent)
			if !tc.built(srv, node) {
				t.Fatalf("the re-send did not build the object")
			}
			if !tracedCall(node, "fault-cleared", fail) {
				t.Errorf("the step did not run under the round's trace id")
			}
			round, resent = resendRound(t, srv, held, "after-the-cure")
			assertCodeZero(t, "the read after the re-send", round)
			if resent != nil {
				t.Errorf("the round after the cure re-sent the request")
			}
		})
	}
}

// TestAFailedGroupBuildUnderAServingPoolIsNamedAndResent: a grow adds a
// group to a pool that serves, and the group's mdadm --create fails once.
// The pool serves on at its old size, so no namespace waits for the group.
// The converge's reply does not name the group's absent array, a read-only
// name; the read after it does, since every leg of the group serves and
// none carries a superblock (absentArrayNamed), and the one re-send creates
// the array; the read after it names nothing.
func TestAFailedGroupBuildUnderAServingPoolIsNamedAndResent(t *testing.T) {
	srv, node := newTestServer(t)
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
	row := reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp2]
	assertErrorDetails(t, row, "create killed", "the grown group")
	absent := agent.LeftoverKindMdAbsent + ":" + row.GetResName()
	if strings.Contains(reply.GetAgentReply().GetDetails(), absent) {
		t.Fatalf("the converge's own reply names the absent array: %v",
			reply.GetAgentReply())
	}

	node.Reset()
	round, resent := resendRound(t, srv, grow, "names-the-array")
	assertCodeNames(t, "the read after the failed create", round, absent)
	if resent == nil {
		t.Fatalf("the round that names the array brought no re-send")
	}
	if got := tracedCalls(node, "names-the-array", "mdadm --create"); len(
		got) != 1 {
		t.Fatalf("the re-send made %q, want one create", got)
	}
	round, resent = resendRound(t, srv, grow, "after-the-create")
	assertCodeZero(t, "the read after the create", round)
	if resent != nil {
		t.Errorf("the round after the create re-sent the request")
	}
}

// TestAFailedLinearGroupBuildIsNamedAndResent: a RedundNone grow's new group
// is a dm-linear over its leg, and its create fails once over a leg that
// serves. The converge's reply and the read after it both name the
// dm-linear (linearGroupStep), the one re-send creates it, and the read
// after it names nothing.
func TestAFailedLinearGroupBuildIsNamedAndResent(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	grp2 := grpName(srv, testDataGrp2)
	node.mu.Lock()
	node.failCmd["dmsetup create "+grp2] = "device-mapper: create ioctl failed"
	node.mu.Unlock()
	grow := growReq(3)
	reply, err := srv.SyncupCntlr(context.Background(), grow)
	if err != nil {
		t.Fatalf("grow: %v", err)
	}
	assertCodeNames(t, "the converge", reply, grp2+": ")
	if dmPresent(node, grp2) {
		t.Fatalf("the case is vacuous: the create went through")
	}

	node.Reset()
	round, resent := resendRound(t, srv, grow, "names-the-linear")
	assertCodeNames(t, "the read after the failed create", round, grp2+": ")
	if resent == nil {
		t.Fatalf("the round that names the dm-linear brought no re-send")
	}
	if got := tracedCalls(node, "names-the-linear",
		"dmsetup create "+grp2); len(got) != 1 {
		t.Fatalf("the re-send made %q, want one create of the dm-linear",
			got)
	}
	round, resent = resendRound(t, srv, grow, "after-the-create")
	assertCodeZero(t, "the read after the create", round)
	if resent != nil {
		t.Errorf("the round after the create re-sent the request")
	}
}

// TestAnArrayStateThePassLeavesAloneIsNotNamed bounds the names: an array
// that does not run, and two arrays that both hold legs of the group, are
// states only an operator ends, so no read names the group, and the worker
// re-sends nothing for it.
func TestAnArrayStateThePassLeavesAloneIsNotNamed(t *testing.T) {
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
			held := quietLevel(4)
			info := f.syncup(t, held)
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
				tc.want, "grp data")
			round, resent := resendRound(t, f.srv, held, "left-alone")
			for _, part := range []string{agent.LeftoverKindMdAbsent,
				agent.LeftoverKindMdBroken, "forced start of group"} {
				if strings.Contains(round.GetAgentReply().GetDetails(),
					part) {
					t.Fatalf("the read names the group: %v",
						round.GetAgentReply())
				}
			}
			assertCodeZero(t, "the read", round)
			if resent != nil {
				t.Errorf("the round re-sent the request for a state only " +
					"an operator ends")
			}
		})
	}
}
