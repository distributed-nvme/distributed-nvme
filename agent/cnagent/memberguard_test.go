package cnagent

import (
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The guards of cnagent.md CN12 that decide from the reading of each member
// (Md.ReadMember): the forced start's refusals, its proof of each zero and
// its start that drops a bad-block list; the assembly's one identity; and
// the identity check of a taken-back leg's add and of a leg_list member's
// add.

// withForcedMark is withMark with the operator's force on the mark.
func withForcedMark(
	req *pb.SyncupCntlrRequest,
	legId uint64,
) *pb.SyncupCntlrRequest {
	req = withMark(req, legId)
	dataGrpOf(req).ForcedStart.Force = true
	return req
}

// withRepairForce sets the operator's force on the request's repair.
func withRepairForce(req *pb.SyncupCntlrRequest) *pb.SyncupCntlrRequest {
	req.NameToRepair[repName].Force = true
	return req
}

// identity sets the identity dev's superblock carries.
func (f *repFix) identity(dev, id string) {
	f.node.mu.Lock()
	defer f.node.mu.Unlock()
	f.node.memberArray[dev] = id
}

// TestForcedStartRefusesAnOlderNamedLeg pins CN12's refusal of a named leg
// whose change count is below that of a full member of the same identity
// that the start would zero (olderNamedLeg): nothing is zeroed or
// assembled, the row names both legs, and the pass registers the retry.
// With the mark's force the start goes on. A higher count on a member of
// another identity, or on one that is not full, is not compared.
func TestForcedStartRefusesAnOlderNamedLeg(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		f := downNoRepair(t)
		f.node.setSuperblock(f.leg2, fakeSuperblock{events: 10})
		f.node.setSuperblock(f.leg1, fakeSuperblock{events: 50})
		info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"leg 0x7 is a full member with md change count 50, above "+
				"the 10 of named leg 0x17", "grp data")
		assertNoCall(t, f.node, "--zero-superblock")
		assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
		if !retrying(t, f.srv) {
			t.Fatalf("the refused forced start registered no retry")
		}
	})
	t.Run("forced", func(t *testing.T) {
		f := downNoRepair(t)
		f.node.setSuperblock(f.leg2, fakeSuperblock{events: 10})
		f.node.setSuperblock(f.leg1, fakeSuperblock{events: 50})
		info := f.syncup(t, withForcedMark(quietReq(4), testDataLeg2))
		assertOrder(t, f.node,
			examineOf(f.leg2),
			examineOf(f.leg1),
			zeroOf(f.leg1),
			f.forced(f.leg2, mdUpdateUuid),
			f.add(f.leg1))
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	})
	for _, tc := range []struct {
		name string
		set  func(f *repFix)
	}{
		{"another identity", func(f *repFix) {
			f.identity(f.leg1, "older")
		}},
		{"not full", func(f *repFix) {
			f.node.setSuperblock(f.leg1, fakeSuperblock{
				recovery: "1024 sectors", events: 50})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := downNoRepair(t)
			f.node.setSuperblock(f.leg2, fakeSuperblock{events: 10})
			f.node.setSuperblock(f.leg1, fakeSuperblock{events: 50})
			tc.set(f)
			info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
			assertOrder(t, f.node,
				zeroOf(f.leg1),
				f.forced(f.leg2, mdUpdateUuid))
			assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		})
	}
}

// TestForcedStartRefusesAMemberThatIsNotRead pins CN12's refusal of a
// member the forced start would zero whose reading did not answer or failed:
// a member whose disk returns errors is never taken for one with no
// superblock, and nothing is zeroed or assembled.
func TestForcedStartRefusesAMemberThatIsNotRead(t *testing.T) {
	f := downNoRepair(t)
	f.node.mu.Lock()
	f.node.failCmdAlways["readsb "+f.leg1] = ""
	f.node.mu.Unlock()
	info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
	assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
		"forced start: member leg 0x7", "grp data")
	assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
		"not read", "grp data")
	assertNoCall(t, f.node, "--zero-superblock")
	assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
	if !retrying(t, f.srv) {
		t.Fatalf("the refused forced start registered no retry")
	}
}

// TestForcedStartProvesEachZero pins CN12's read back of each zero: mdadm
// answers a zero whose read of the member fails with success and writes
// nothing, so a member that still shows a superblock after its zero, or
// whose read back fails, is the group's error, and no assembly follows.
func TestForcedStartProvesEachZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(f *repFix)
		want string
	}{
		{"no effect", func(f *repFix) {
			f.node.zeroNoEffect[f.leg1] = true
		}, "still carries a superblock"},
		{"read back fails", func(f *repFix) {
			f.node.killCmdNth["readsb "+f.leg1+" off=4096"] = 2
		}, "the zero of leg 0x7 is not proven"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := downNoRepair(t)
			f.node.mu.Lock()
			tc.set(f)
			f.node.mu.Unlock()
			info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
				tc.want, "grp data")
			if !f.node.hasCall(zeroOf(f.leg1)) {
				t.Fatalf("no zero of leg 1")
			}
			assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
			if !retrying(t, f.srv) {
				t.Fatalf("the unproven zero registered no retry")
			}
		})
	}
}

// TestForcedStartDropsABadBlockList pins CN12's start of a named leg whose
// reading shows a bad-block list the kernel takes: the first run starts the
// array under its new identity with the list, so the pass stops it and
// starts it again with the list dropped, keeping the new identity; the
// record names both updates. A stop that fails is the group's error.
func TestForcedStartDropsABadBlockList(t *testing.T) {
	t.Run("dropped", func(t *testing.T) {
		f := downNoRepair(t)
		oldUuid := f.legUuid(f.leg2)
		f.node.setSuperblock(f.leg2, fakeSuperblock{badBlocks: 2})
		logs := captureLogs(t)
		info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
		assertOrder(t, f.node,
			examineOf(f.leg2),
			zeroOf(f.leg1),
			f.forced(f.leg2, mdUpdateUuid),
			"cmd mdadm --stop ",
			f.forced(f.leg2, mdUpdateForceNoBbl),
			f.add(f.leg1))
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		assertForcedRecord(t, logs, testDataLeg2,
			mdUpdateUuid+","+mdUpdateForceNoBbl, testDataLeg)
		f.assertNewIdentity(t, oldUuid)
		f.node.mu.Lock()
		left := f.node.sbFacts[f.leg2].badBlocks
		f.node.mu.Unlock()
		if left != 0 {
			t.Fatalf("leg 2 keeps %d bad blocks", left)
		}
	})
	t.Run("the stop fails", func(t *testing.T) {
		f := downNoRepair(t)
		f.node.setSuperblock(f.leg2, fakeSuperblock{badBlocks: 2})
		f.node.mu.Lock()
		f.node.failCmdAlways["cmd mdadm --stop "] =
			"mdadm: Cannot get exclusive access"
		f.node.mu.Unlock()
		info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"its stop to drop the list failed", "grp data")
		assertNoCall(t, f.node, "--update="+mdUpdateForceNoBbl)
		// The first run started the array, so the mark was acted on though
		// the pass ended in an error: the next converge finds the array
		// running, leaves the mark waiting and names it as acted
		// (cnagent.md CN28), for the worker's clear.
		f.node.mu.Lock()
		delete(f.node.failCmdAlways, "cmd mdadm --stop ")
		f.node.mu.Unlock()
		info = f.syncup(t, markOf(quietReq(5), testDataLeg2, 4))
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		if got := actedRevision(info); got != 4 {
			t.Fatalf("the converge after the failed stop reads acted mark "+
				"%d, want 4", got)
		}
	})
}

// TestAssemblyHandsMdOneIdentity pins CN12's assembly of members of two
// identities (assemblyIdentity): the leg_list legs other than an open
// repair's bad leg decide, then the repair's new legs, then its bad leg,
// and a member of another identity is not handed to mdadm, which would
// refuse the whole assembly; two identities among the legs that decide are
// the group's error, with no assembly and no retry.
func TestAssemblyHandsMdOneIdentity(t *testing.T) {
	assembled := func(f *repFix) string {
		calls := f.node.callsMatching("cmd mdadm --assemble " + f.mdDev)
		if len(calls) != 1 {
			t.Fatalf("assemblies %v, want one", calls)
		}
		return calls[0]
	}
	t.Run("the bad leg of another identity", func(t *testing.T) {
		f := downFix(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false,
			repOpts{standby: true}))
		f.identity(f.leg1, "older")
		info := f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_CREATED,
			false))
		if call := assembled(f); !strings.HasSuffix(call, " "+f.leg2) {
			t.Fatalf("the assembly is %q, want leg 2 alone", call)
		}
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	})
	t.Run("a new leg of another identity", func(t *testing.T) {
		f := downFix(t, repairReq(3, pb.RepStage_REP_STAGE_STARTED, false,
			repOpts{standby: true}))
		f.node.setSuperblock(f.newLegDev, fakeSuperblock{events: 9})
		info := f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_STARTED,
			false))
		if call := assembled(f); strings.Contains(call, f.newLegDev) ||
			!strings.Contains(call, f.leg1) ||
			!strings.Contains(call, f.leg2) {
			t.Fatalf("the assembly is %q, want both legs of leg_list "+
				"alone", call)
		}
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	})
	t.Run("an aborted repair's bad leg of another identity", func(t *testing.T) {
		f := downFix(t, repairReq(3, pb.RepStage_REP_STAGE_ABORTED, true,
			repOpts{standby: true}))
		f.identity(f.leg1, "older")
		info := f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_ABORTED,
			true))
		if call := assembled(f); !strings.HasSuffix(call, " "+f.leg2) {
			t.Fatalf("the assembly is %q, want leg 2 alone", call)
		}
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		if got := info.GetGrpIdToGrpInfo()[testDataGrp].
			GetRefusal(); !strings.Contains(got, "leg 0x7 carries another") {
			t.Fatalf("the group's refusal is %q", got)
		}
	})
	t.Run("two identities in leg_list", func(t *testing.T) {
		f := downNoRepair(t)
		f.identity(f.leg1, "older")
		info := f.syncup(t, quietReq(4))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"members of two identities", "grp data")
		assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
		assertNoCall(t, f.node, "cmd mdadm --create "+f.mdDev)
		if retrying(t, f.srv) {
			t.Fatalf("two identities registered a retry")
		}
	})
}

// TestTakeBackRefusesAnotherIdentity pins CN12's take-back check on a leg
// of another identity, as a retired leg from before a forced start carries:
// its add is refused whatever its change count, and the refusal names it,
// on the converge and on the read; the repair's force lets it in.
func TestTakeBackRefusesAnotherIdentity(t *testing.T) {
	setup := func(t *testing.T) *repFix {
		f := newRepFix(t, allLegs...)
		f.badLegDead()
		f.node.setSuperblock(f.leg2, fakeSuperblock{
			role: "Active device 1", events: 10})
		f.node.setSuperblock(f.newLegDev, fakeSuperblock{
			role: "Active device 1", events: 5})
		f.node.Reset()
		return f
	}
	t.Run("refused", func(t *testing.T) {
		f := setup(t)
		info := f.syncup(t, takenBackReq(3, pb.RepStage_REP_STAGE_CREATED,
			false))
		f.assertMdCalls(t, "the converge")
		refusal := verdict(info).GetRefusal()
		if !strings.Contains(refusal, "leg 0x7b taken from a retired leg "+
			"carries another array's superblock") {
			t.Fatalf("the converge's refusal is %q", refusal)
		}
		assertVerdict(t, info, "the refused repair", false, false)
		probeLegs(t, f.srv, repNewLeg)
		read, reply := f.read(t, 3)
		if reply.GetCode() != 0 {
			t.Fatalf("the read of the refused repair: %v, want clean",
				reply)
		}
		if got := verdict(read).GetRefusal(); got != refusal {
			t.Fatalf("the read's refusal is %q, the converge's %q", got,
				refusal)
		}
	})
	t.Run("forced", func(t *testing.T) {
		f := setup(t)
		info := f.syncup(t, withRepairForce(takenBackReq(3,
			pb.RepStage_REP_STAGE_CREATED, false)))
		f.assertMdCalls(t, "the converge", f.fail(f.leg1),
			f.remove(f.leg1), f.add(f.newLegDev))
		if got := verdict(info).GetRefusal(); got != "" {
			t.Fatalf("the forced repair's refusal is %q", got)
		}
	})
}

// TestAddRefusesALegListMemberOfAnotherIdentity pins CN12's identity check
// on the add of a leg_list member the running array lacks
// (legListAddRefusal): a member of another identity, as a forced start from
// another leg leaves an aborted repair's bad leg, is not added, and the
// group's reading names it on the converge and on the read, whose verdict
// does not name the member; the force of the repair whose bad leg it is
// lets it in, and without the force, or once the repair is gone, it stays
// out.
func TestAddRefusesALegListMemberOfAnotherIdentity(t *testing.T) {
	setup := func(t *testing.T) *repFix {
		f := newRepFix(t, allLegs...)
		f.node.mu.Lock()
		array := f.node.arrays[f.mdDev]
		var kept []string
		for _, member := range array.members {
			if member != f.leg1 {
				kept = append(kept, member)
			}
		}
		array.members = kept
		array.degraded = 1
		f.node.publishArray(array)
		f.node.memberArray[f.leg1] = "older"
		f.node.mu.Unlock()
		f.node.Reset()
		return f
	}
	refused := func(t *testing.T, f *repFix, info *pb.CntlrInfo) string {
		t.Helper()
		f.assertMdCalls(t, "the converge")
		refusal := info.GetGrpIdToGrpInfo()[testDataGrp].GetRefusal()
		if !strings.Contains(refusal, "leg 0x7 carries another array's "+
			"superblock") {
			t.Fatalf("the group's refusal is %q", refusal)
		}
		return refusal
	}
	for _, tc := range []struct {
		name string
		req  func(revision uint64) *pb.SyncupCntlrRequest
	}{
		{"no repair", func(revision uint64) *pb.SyncupCntlrRequest {
			return noRepairReq(revision)
		}},
		{"an aborted repair without force",
			func(revision uint64) *pb.SyncupCntlrRequest {
				return repairReq(revision, pb.RepStage_REP_STAGE_ABORTED,
					true)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			info := f.syncup(t, tc.req(3))
			refusal := refused(t, f, info)
			read, reply := f.read(t, 3)
			if strings.Contains(reply.GetDetails(), legName(f.srv,
				testDataLeg)) {
				t.Fatalf("the read names the refused member: %v", reply)
			}
			if got := read.GetGrpIdToGrpInfo()[testDataGrp].
				GetRefusal(); got != refusal {
				t.Fatalf("the read's refusal is %q, the converge's %q",
					got, refusal)
			}
		})
	}
	t.Run("the aborted repair's force", func(t *testing.T) {
		f := setup(t)
		info := f.syncup(t, withRepairForce(repairReq(3,
			pb.RepStage_REP_STAGE_ABORTED, true)))
		f.assertMdCalls(t, "the converge", f.add(f.leg1))
		if got := info.GetGrpIdToGrpInfo()[testDataGrp].
			GetRefusal(); got != "" {
			t.Fatalf("the group's refusal is %q, want none", got)
		}
	})
}

// TestPutBackMakesTheIdentityCheck pins CN12's identity check on a member
// md has failed and moved out of its slot: the converge's put-back removes
// it and its add makes the check, and the read makes the same check before
// it names the member, so a member of another identity is named by
// neither channel and both report the refusal.
func TestPutBackMakesTheIdentityCheck(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.node.setMemberState(f.mdDev, f.leg1, "faulty")
	f.node.setMemberSlot(f.mdDev, f.leg1, mdSlotNone)
	f.identity(f.leg1, "older")
	f.node.Reset()
	read, reply := f.read(t, 2)
	if strings.Contains(reply.GetDetails(), legName(f.srv, testDataLeg)) {
		t.Fatalf("the read names the member of another identity: %v", reply)
	}
	refusal := read.GetGrpIdToGrpInfo()[testDataGrp].GetRefusal()
	if !strings.Contains(refusal, "leg 0x7 carries another") {
		t.Fatalf("the read's refusal is %q", refusal)
	}
	info := f.syncup(t, noRepairReq(3))
	f.assertMdCalls(t, "the converge", f.remove(f.leg1))
	if got := info.GetGrpIdToGrpInfo()[testDataGrp].
		GetRefusal(); got != refusal {
		t.Fatalf("the converge's refusal is %q, the read's %q", got, refusal)
	}
}

// TestNoCreateOnceTheArrayRan pins CN12's refusal of case 1's create for a
// group whose array has run (Group.array_made): with every leg available
// and none carrying a superblock, as a forced start cut short after its
// zeroes leaves them, the group's row reads ERROR, no array is created, and
// the retry is registered, whose converge takes a leg with a superblock once
// one is available; the same group without the flag is created.
func TestNoCreateOnceTheArrayRan(t *testing.T) {
	for _, made := range []bool{true, false} {
		f := downNoRepair(t)
		f.node.mu.Lock()
		delete(f.node.superblocks, f.leg1)
		delete(f.node.superblocks, f.leg2)
		f.node.mu.Unlock()
		req := quietReq(4)
		dataGrpOf(req).ArrayMade = made
		info := f.syncup(t, req)
		if !made {
			if !f.node.hasCall("cmd mdadm --create " + f.mdDev) {
				t.Fatalf("a group with no flag got no create")
			}
			continue
		}
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"has run before", "grp data")
		assertNoCall(t, f.node, "cmd mdadm --create "+f.mdDev)
		if !retrying(t, f.srv) {
			t.Fatalf("the refused create registered no retry")
		}
	}
}
