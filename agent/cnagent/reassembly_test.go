package cnagent

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The re-assembly of a broken array (cnagent.md CN12, The re-assembly;
// CN30): while no mark is in the group's record and no operator is in
// charge, the primary stops a broken array that nothing holds open and
// assembles it again from its members, once every member the group keeps
// serves and reads as a full member of one identity with no bad-block list.

// brokenIdle is the fixture with the data group's array broken at the level
// that builds no thin pool, where nothing holds the array open; it returns
// the array's md node.
func brokenIdle(t *testing.T) (*repFix, string) {
	t.Helper()
	f := newRepFix(t, allLegs...)
	f.syncup(t, quietLevel(3))
	f.node.setArrayState(f.mdDev, mdStateBroken)
	return f, f.node.mdNode(f.mdDev)
}

// TestABrokenArrayIsAssembledAgain: the read names the broken array, and the
// converge stops it and assembles it again from both legs, with no forced
// start, its row then reading OK over a running array.
func TestABrokenArrayIsAssembledAgain(t *testing.T) {
	f, node := brokenIdle(t)
	_, reply := f.read(t, 3)
	if reply.GetCode() != 4 ||
		!strings.Contains(reply.GetDetails(), "md_broken:"+node) {
		t.Errorf("the read's verdict: code %d (%s), want md_broken:%s",
			reply.GetCode(), reply.GetDetails(), node)
	}
	f.node.Reset()
	info := f.syncup(t, quietLevel(4))
	assertOrder(t, f.node, "cmd mdadm --stop "+node,
		"cmd mdadm --assemble "+f.mdDev)
	assertNoCall(t, f.node, "--zero-superblock")
	assertNoCall(t, f.node, "--run --force")
	for _, call := range f.node.callsMatching("cmd mdadm --assemble ") {
		for _, leg := range []string{f.leg1, f.leg2} {
			if !strings.Contains(call, leg) {
				t.Errorf("the assembly %q leaves out %s", call, leg)
			}
		}
	}
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	if got := f.members(); !slices.Equal(got,
		slices.Sorted(slices.Values([]string{f.leg1, f.leg2}))) {
		t.Errorf("the array holds %v, want both legs", got)
	}
	if _, reply := f.read(t, 4); reply.GetCode() != 0 {
		t.Errorf("a read after the re-assembly: code %d (%s), want none",
			reply.GetCode(), reply.GetDetails())
	}
}

// TestABrokenArrayIsLeftAsItIs: each condition that leaves the broken array
// as it is, with nothing stopped or assembled, the group's row naming why,
// the read naming nothing and nothing running between the worker's calls.
// A leg that does not serve is the one condition that ends by itself: once
// its path is live again the read names the array, and the re-send on that
// round assembles it again.
func TestABrokenArrayIsLeftAsItIs(t *testing.T) {
	for _, tc := range []struct {
		name string
		// req is the converge's request at revision 4; nil is quietLevel.
		req   func() *pb.SyncupCntlrRequest
		setup func(t *testing.T, f *repFix)
		why   string
	}{
		{"a leg does not serve", nil, func(t *testing.T, f *repFix) {
			f.legDead(testDataLeg2, testIp2, testSvcId2)
		}, "does not serve"},
		{"a half-copied member", nil, func(t *testing.T, f *repFix) {
			f.node.setSuperblock(f.leg2, fakeSuperblock{
				role: "Active device 1", recovery: "45056 sectors"})
		}, "not a full member: recovery offset"},
		{"a bad-block list", nil, func(t *testing.T, f *repFix) {
			f.node.setSuperblock(f.leg2, fakeSuperblock{
				role: "Active device 1", badBlocks: 2})
		}, "bad-block list of 2 entries"},
		{"two identities", nil, func(t *testing.T, f *repFix) {
			f.node.mu.Lock()
			f.node.memberArray[f.leg2] = f.node.newMdUuid()
			f.node.mu.Unlock()
		}, "members of two identities"},
		{"a superblock read that fails", nil, func(t *testing.T, f *repFix) {
			f.node.mu.Lock()
			f.node.failCmdAlways["readsb "+f.leg2] = "input/output error"
			f.node.mu.Unlock()
		}, "the superblock read of leg"},
		{"no member carries a superblock", nil, func(t *testing.T, f *repFix) {
			f.node.mu.Lock()
			f.node.superblocks[f.leg1] = false
			f.node.superblocks[f.leg2] = false
			f.node.mu.Unlock()
		}, "no member carries a superblock"},
		{"another sp's leg in it", nil, func(t *testing.T, f *repFix) {
			f.node.mu.Lock()
			array := f.node.arrays[f.mdDev]
			held := append(append([]string(nil), array.members...),
				"/dev/mapper/"+names(f.srv).CnLegName(
					testCluster, testCn, testSp+1, testDataLeg))
			name := array.name
			f.node.mu.Unlock()
			f.node.seedArray(f.mdDev, name, held...)
			f.node.setArrayState(f.mdDev, mdStateBroken)
		}, "which is no leg of this sp"},
		{"a leg's row reads ERROR", nil, func(t *testing.T, f *repFix) {
			failRounds(t, f.srv, testDataLeg2, 1)
		}, "its row reads ERROR"},
		{"the operator in charge", func() *pb.SyncupCntlrRequest {
			req := quietLevel(4)
			dataGrpOf(req).OperatorInCharge = true
			return req
		}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, node := brokenIdle(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			f.node.forgetTraces()
			req := quietLevel(4)
			if tc.req != nil {
				req = tc.req()
			}
			if tc.req == nil {
				if _, reply := f.read(t, 3); strings.Contains(
					reply.GetDetails(), "md_broken:") {
					t.Errorf("the read names the array: %s",
						reply.GetDetails())
				}
			}
			f.node.Reset()
			info := f.syncup(t, req)
			assertNoCall(t, f.node, "cmd mdadm --stop")
			assertNoCall(t, f.node, "cmd mdadm --assemble")
			row := info.GetGrpIdToMdRaid()[testDataGrp]
			if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
				!strings.Contains(row.GetDetails(), "is broken") ||
				!strings.Contains(row.GetDetails(), tc.why) {
				t.Errorf("the group's row: %v, want ERROR naming %q", row,
					tc.why)
			}
			if _, reply := f.read(t, 4); strings.Contains(
				reply.GetDetails(), "md_broken:") {
				t.Errorf("the read names the array: %s", reply.GetDetails())
			}
			assertNothingRunsBetweenCalls(t, f.node)
			if tc.name != "a leg does not serve" {
				return
			}
			f.node.setCtrlState(legNqn(f.srv, testDataLeg2), testIp2,
				testSvcId2, "live")
			f.node.Reset()
			round, resent := resendRound(t, f.srv, req, "named")
			if !strings.Contains(round.GetAgentReply().GetDetails(),
				agent.LeftoverKindMdBroken+":"+node) || resent == nil {
				t.Fatalf("the round once the leg serves is %v, re-sent %v; "+
					"want the broken array named and a re-send",
					round.GetAgentReply(), resent != nil)
			}
			assertOrder(t, f.node, "cmd mdadm --stop "+node,
				"cmd mdadm --assemble "+f.mdDev)
			if !tracedCall(f.node, "named", "cmd mdadm --stop "+node) {
				t.Fatalf("the stop did not run under the round's trace id")
			}
		})
	}
}

// TestABrokenArrayThePoolHoldsIsLeftAsItIs: at a level that builds thin
// pools the pool's concat holds the array open, so mdadm could not stop it:
// the pass leaves it as it is, the row naming the holder, and the read names
// nothing.
func TestABrokenArrayThePoolHoldsIsLeftAsItIs(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, quietReq(3))
	f.node.setArrayState(f.mdDev, mdStateBroken)
	if _, reply := f.read(t, 3); strings.Contains(reply.GetDetails(),
		"md_broken:") {
		t.Errorf("the read names the held array: %s", reply.GetDetails())
	}
	f.node.Reset()
	info := f.syncup(t, quietReq(4))
	assertNoCall(t, f.node, "cmd mdadm --stop")
	row := info.GetGrpIdToMdRaid()[testDataGrp]
	if !strings.Contains(row.GetDetails(), "held open by dm-") {
		t.Errorf("the group's row: %v, want the holder named", row)
	}
}

// TestABrokenArrayWhoseStopFailsIsNamedAgain: a stop that fails is the
// group's error, and the array stays broken, so the read after it names the
// broken array again (reassemblyNamed), and the re-send on that round stops
// and assembles the array once the stop goes through.
func TestABrokenArrayWhoseStopFailsIsNamedAgain(t *testing.T) {
	f, node := brokenIdle(t)
	f.node.mu.Lock()
	f.node.failCmd["mdadm --stop "+node] = "mdadm: stop refused"
	f.node.mu.Unlock()
	f.node.Reset()
	held := quietLevel(4)
	info := f.syncup(t, held)
	if row := info.GetGrpIdToMdRaid()[testDataGrp]; row.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("the group's row after the failed stop: %v", row)
	}
	f.node.forgetTraces()
	f.node.Reset()
	round, resent := resendRound(t, f.srv, held, "named")
	if !strings.Contains(round.GetAgentReply().GetDetails(),
		agent.LeftoverKindMdBroken+":"+node) || resent == nil {
		t.Fatalf("the round after the failed stop is %v, re-sent %v; want "+
			"the broken array named and a re-send", round.GetAgentReply(),
			resent != nil)
	}
	assertOrder(t, f.node, "cmd mdadm --stop "+node,
		"cmd mdadm --assemble "+f.mdDev)
	if !tracedCall(f.node, "named", "cmd mdadm --assemble "+f.mdDev) {
		t.Fatalf("the assembly did not run under the round's trace id")
	}
	if _, reply := f.read(t, 4); reply.GetCode() != 0 {
		t.Fatalf("the read after the re-send: %v, want clean", reply)
	}
	assertNothingRunsBetweenCalls(t, f.node, "named")
}

// TestABrokenArrayWaitsForAnOpenRepairsNewLeg: the new leg of the group's
// open repair is a member the group keeps, so the re-assembly waits while
// it does not serve, and the read names nothing.
func TestABrokenArrayWaitsForAnOpenRepairsNewLeg(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.newLegUnreachable()
	opts := repOpts{level: pb.SpLevel_SP_LEVEL_NO_THINPOOL}
	f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false, opts))
	f.node.setArrayState(f.mdDev, mdStateBroken)
	f.node.Reset()
	info := f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_CREATED, false,
		opts))
	assertNoCall(t, f.node, "cmd mdadm --stop")
	if row := info.GetGrpIdToMdRaid()[testDataGrp]; !strings.Contains(
		row.GetDetails(), fmt.Sprintf("leg %#x does not serve", repNewLeg)) {
		t.Errorf("the group's row: %v, want the new leg named", row)
	}
}

// TestABrokenArrayWithAMemberWithNoSuperblock: a leg that carries no
// superblock is left out of the assembly, which starts the array from the
// other leg, and the member reconciliation then adds it.
func TestABrokenArrayWithAMemberWithNoSuperblock(t *testing.T) {
	f, node := brokenIdle(t)
	f.node.mu.Lock()
	f.node.superblocks[f.leg2] = false
	f.node.mu.Unlock()
	f.node.Reset()
	info := f.syncup(t, quietLevel(4))
	assertOrder(t, f.node, "cmd mdadm --stop "+node,
		"cmd mdadm --assemble "+f.mdDev, f.add(f.leg2))
	for _, call := range f.node.callsMatching("cmd mdadm --assemble ") {
		if strings.Contains(call, f.leg2) {
			t.Errorf("the assembly %q takes the leg with no superblock",
				call)
		}
	}
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
}

// TestABrokenArrayWhoseHoldersDoNotRead: a listing of the holders that does
// not answer is the group's error, and the read, which meets the same
// listing, names nothing, so nothing runs between the worker's calls. Once
// the listing answers, the read names the broken array, and the re-send on
// that round assembles it again.
func TestABrokenArrayWhoseHoldersDoNotRead(t *testing.T) {
	f, node := brokenIdle(t)
	holders := "list " + sysfsBlockDir + "/" +
		strings.TrimPrefix(node, "/dev/") + "/holders"
	f.node.mu.Lock()
	f.node.failCmdAlways[holders] = "input/output error"
	f.node.mu.Unlock()
	f.node.forgetTraces()
	if _, reply := f.read(t, 3); strings.Contains(reply.GetDetails(),
		"md_broken:") {
		t.Errorf("the read names the array: %s", reply.GetDetails())
	}
	f.node.Reset()
	held := quietLevel(4)
	info := f.syncup(t, held)
	assertNoCall(t, f.node, "cmd mdadm --stop")
	if row := info.GetGrpIdToMdRaid()[testDataGrp]; row.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR {
		t.Errorf("the group's row: %v, want ERROR", row)
	}
	if _, reply := f.read(t, 4); strings.Contains(reply.GetDetails(),
		"md_broken:") {
		t.Errorf("the read after the converge names the array: %s",
			reply.GetDetails())
	}
	assertNothingRunsBetweenCalls(t, f.node)

	f.node.mu.Lock()
	delete(f.node.failCmdAlways, holders)
	f.node.mu.Unlock()
	f.node.Reset()
	round, resent := resendRound(t, f.srv, held, "named")
	if !strings.Contains(round.GetAgentReply().GetDetails(),
		agent.LeftoverKindMdBroken+":"+node) || resent == nil {
		t.Fatalf("the round once the listing answers is %v, re-sent %v; "+
			"want the broken array named and a re-send",
			round.GetAgentReply(), resent != nil)
	}
	assertOrder(t, f.node, "cmd mdadm --stop "+node,
		"cmd mdadm --assemble "+f.mdDev)
}

// TestABrokenArrayWhoseAssemblyFailsIsNamedAbsent: an assembly that fails
// after the stop is the group's error and leaves no array, so the read after
// it names the absent array (absentArrayNamed), and the re-send on that
// round assembles it by CN12's ordinary assembly, with no stop.
func TestABrokenArrayWhoseAssemblyFailsIsNamedAbsent(t *testing.T) {
	f, node := brokenIdle(t)
	f.node.mu.Lock()
	f.node.failCmd["mdadm --assemble "+f.mdDev] = "mdadm: assembly failed"
	f.node.mu.Unlock()
	f.node.Reset()
	held := quietLevel(4)
	info := f.syncup(t, held)
	assertOrder(t, f.node, "cmd mdadm --stop "+node,
		"cmd mdadm --assemble "+f.mdDev)
	if row := info.GetGrpIdToMdRaid()[testDataGrp]; row.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("the group's row after the failed assembly: %v", row)
	}
	f.node.forgetTraces()
	f.node.Reset()
	round, resent := resendRound(t, f.srv, held, "named")
	if !strings.Contains(round.GetAgentReply().GetDetails(),
		f.absentNamed()) || resent == nil {
		t.Fatalf("the round after the failed assembly is %v, re-sent %v; "+
			"want the absent array named and a re-send",
			round.GetAgentReply(), resent != nil)
	}
	assertNoCall(t, f.node, "cmd mdadm --stop")
	if !tracedCall(f.node, "named", "cmd mdadm --assemble "+f.mdDev) {
		t.Errorf("the re-send did not assemble the array under the "+
			"round's trace id:\n%s",
			strings.Join(f.node.Traced("named"), "\n"))
	}
	if _, reply := f.read(t, 4); reply.GetCode() != 0 {
		t.Fatalf("the read after the re-send: %v, want clean", reply)
	}
	assertNothingRunsBetweenCalls(t, f.node, "named")
}

// TestABrokenArrayWithATakenBackLeg: a new leg the open repair took back from
// a retired leg and that carries a superblock is let in by the take-back
// check alone, so the re-assembly leaves the array as it is; with no
// superblock on it, the array is assembled again.
func TestABrokenArrayWithATakenBackLeg(t *testing.T) {
	for _, carries := range []bool{true, false} {
		f := newRepFix(t, allLegs...)
		opts := repOpts{level: pb.SpLevel_SP_LEVEL_NO_THINPOOL}
		f.syncup(t, quietLevel(3))
		f.node.setArrayState(f.mdDev, mdStateBroken)
		if carries {
			f.node.setSuperblock(f.newLegDev, fakeSuperblock{
				role: "Active device 0", events: 5})
			f.node.sameArrayAs(f.newLegDev, f.leg1)
		}
		f.node.Reset()
		info := f.syncup(t, takenBackReq(4, pb.RepStage_REP_STAGE_CREATED,
			false, opts))
		stopped := f.node.hasCall("cmd mdadm --stop")
		if stopped == carries {
			t.Errorf("carries a superblock %v: the array stopped %v, row %v",
				carries, stopped, info.GetGrpIdToMdRaid()[testDataGrp])
		}
		if row := info.GetGrpIdToMdRaid()[testDataGrp]; carries &&
			!strings.Contains(row.GetDetails(), "taken back") {
			t.Errorf("the group's row: %v, want the taken-back leg named",
				row)
		}
	}
}
