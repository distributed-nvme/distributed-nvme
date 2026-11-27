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

// withForcedMark is withMark with the force on the mark that a mark an
// older gateway wrote can carry, which passes every kind of refusal.
func withForcedMark(
	req *pb.SyncupCntlrRequest,
	legId uint64,
) *pb.SyncupCntlrRequest {
	req = withMark(req, legId)
	dataGrpOf(req).ForcedStart.Force = true
	return req
}

// forcedMarkOf is markOf with the force of withForcedMark.
func forcedMarkOf(
	req *pb.SyncupCntlrRequest,
	legId, revision uint64,
) *pb.SyncupCntlrRequest {
	req = markOf(req, legId, revision)
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
// assembled, and the row names both legs. The refusal lasts, so the read,
// whose dry run meets it too (forcedStartNamed), names nothing, and nothing
// runs between the worker's calls. With the force an older gateway's mark
// carries the start goes on. A higher count on a member of
// another identity, or on one that is not full, is not compared.
func TestForcedStartRefusesAnOlderNamedLeg(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		f := downNoRepair(t)
		f.node.setSuperblock(f.leg2, fakeSuperblock{events: 10})
		f.node.setSuperblock(f.leg1, fakeSuperblock{events: 50})
		f.node.forgetTraces()
		info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"leg 0x7 is a full member with md change count 50, above "+
				"the 10 of named leg 0x17", "grp data")
		assertNoCall(t, f.node, "--zero-superblock")
		assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
		f.assertForcedStartUnnamed(t, 4, "the older named leg")
		assertNoCall(t, f.node, "--zero-superblock")
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
// superblock, and nothing is zeroed or assembled. The read meets the same
// failure and names nothing (forcedStartNamed), so nothing runs between the
// worker's calls while it lasts; once the member reads, the read names the
// forced start, and the re-send on that round starts the array.
func TestForcedStartRefusesAMemberThatIsNotRead(t *testing.T) {
	f := downNoRepair(t)
	f.node.mu.Lock()
	f.node.failCmdAlways["readsb "+f.leg1] = ""
	f.node.mu.Unlock()
	f.node.forgetTraces()
	held := withMark(quietReq(4), testDataLeg2)
	info := f.syncup(t, held)
	assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
		"forced start: member leg 0x7", "grp data")
	assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
		"not read", "grp data")
	assertNoCall(t, f.node, "--zero-superblock")
	assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
	f.assertForcedStartUnnamed(t, 4, "the member that is not read")

	f.node.mu.Lock()
	delete(f.node.failCmdAlways, "readsb "+f.leg1)
	f.node.mu.Unlock()
	f.node.Reset()
	f.forcedStartResent(t, held, testDataLeg2, "named")
	assertOrder(t, f.node,
		zeroOf(f.leg1),
		f.forced(f.leg2, mdUpdateUuid),
		f.add(f.leg1))
	f.assertForcedStartUnnamed(t, 4, "after the start", "named")
}

// TestForcedStartProvesEachZero pins CN12's read back of each zero: a zero
// that answered success proves nothing by itself, so a member that still
// shows a superblock after its zero, as a write that never reached the leg
// leaves it, or whose read back fails, is the group's error, and no assembly
// follows. The read after it names the forced start, whose dry run reads no
// zero (forcedStartNamed), and the re-send on that round starts the array:
// it zeroes again a member that still shows a superblock, the zero proven
// this time, and counts as zeroed with no zero run a member the first zero
// did leave blank.
func TestForcedStartProvesEachZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(f *repFix)
		want string
		// blank says the first zero left the member blank.
		blank bool
	}{
		{"no effect", func(f *repFix) {
			f.node.zeroNoEffect[f.leg1] = true
		}, "still carries a superblock", false},
		{"read back fails", func(f *repFix) {
			f.node.killCmdNth["readsb "+f.leg1+" off=4096"] = 2
		}, "the zero of leg 0x7 is not proven", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := downNoRepair(t)
			f.node.mu.Lock()
			tc.set(f)
			f.node.mu.Unlock()
			held := withMark(quietReq(4), testDataLeg2)
			info := f.syncup(t, held)
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
				tc.want, "grp data")
			if !f.node.hasCall(zeroOf(f.leg1)) {
				t.Fatalf("no zero of leg 1")
			}
			assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)

			f.node.mu.Lock()
			delete(f.node.zeroNoEffect, f.leg1)
			f.node.mu.Unlock()
			f.node.forgetTraces()
			f.node.Reset()
			f.forcedStartResent(t, held, testDataLeg2, "named")
			if tc.blank {
				assertNoCall(t, f.node, "--zero-superblock")
				assertOrder(t, f.node,
					f.forced(f.leg2, mdUpdateUuid),
					f.add(f.leg1))
			} else {
				assertOrder(t, f.node,
					zeroOf(f.leg1),
					f.forced(f.leg2, mdUpdateUuid),
					f.add(f.leg1))
			}
			f.assertForcedStartUnnamed(t, 4, "after the start", "named")
		})
	}
}

// TestForcedStartDropsABadBlockList pins CN12's start of a named leg whose
// reading shows a bad-block list the kernel takes: the first run starts the
// array under its new identity with the list, so the pass stops it and
// starts it again with the list dropped, keeping the new identity; the
// record names both updates. A stop that fails is the group's error. The
// mark carries the force an older gateway's mark can carry, which passes the
// named leg's list as the override bad-blocks does (badBlocksRefusal,
// forcedStartPasses).
func TestForcedStartDropsABadBlockList(t *testing.T) {
	t.Run("dropped", func(t *testing.T) {
		f := downNoRepair(t)
		oldUuid := f.legUuid(f.leg2)
		f.node.setSuperblock(f.leg2, fakeSuperblock{badBlocks: 2})
		logs := captureLogs(t)
		info := f.syncup(t, withForcedMark(quietReq(4), testDataLeg2))
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
		logs := captureLogs(t)
		info := f.syncup(t, withForcedMark(quietReq(4), testDataLeg2))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"its stop to drop the list failed", "grp data")
		recs := logs.msgRecords(t, msgForcedStartListKept)
		if len(recs) != 1 || recs[0]["level"] != "WARN" ||
			recs[0]["grp_id"] != float64(testDataGrp) ||
			recs[0]["leg_id"] != float64(testDataLeg2) {
			t.Fatalf("the %q records are %v, want one naming the group "+
				"and leg 2", msgForcedStartListKept, recs)
		}
		assertNoCall(t, f.node, "--update="+mdUpdateForceNoBbl)
		// The first run started the array with the named leg's list, and
		// the stop that would drop the list failed: the start is not done,
		// so the next converge finds the array running, leaves the mark
		// waiting and names no acted mark (cnagent.md CN12, CN28), and the
		// worker keeps the mark.
		f.node.mu.Lock()
		delete(f.node.failCmdAlways, "cmd mdadm --stop ")
		f.node.mu.Unlock()
		info = f.syncup(t, forcedMarkOf(quietReq(5), testDataLeg2, 4))
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		if got := actedRevision(info); got != 0 {
			t.Fatalf("the converge after the failed stop reads acted mark "+
				"%d, want none", got)
		}
		if read, _ := f.read(t, 5); actedRevision(read) != 0 {
			t.Fatalf("the read after the failed stop reads acted mark %d, "+
				"want none", actedRevision(read))
		}
		// The operator's way back: the level step down stops the array,
		// and the step up acts on the standing mark again, whose stop now
		// goes through and whose second run drops the list.
		f.syncup(t, forcedMarkOf(levelReq(6, pb.SpLevel_SP_LEVEL_NO_REDUND),
			testDataLeg2, 4))
		if !f.node.arrayGone(f.mdDev) {
			t.Fatalf("the level step down left the array")
		}
		f.node.Reset()
		f.syncup(t, forcedMarkOf(quietReq(7), testDataLeg2, 4))
		if calls := f.node.callsMatching(
			"--update=" + mdUpdateForceNoBbl); len(calls) != 1 {
			t.Fatalf("the start after the level step: %v, want one run "+
				"that drops the list", calls)
		}
		info = f.syncup(t, forcedMarkOf(quietReq(8), testDataLeg2, 4))
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		if got := actedRevision(info); got != 4 {
			t.Fatalf("the converge after the level step reads acted mark "+
				"%d, want 4", got)
		}
	})
	t.Run("a second start's stop fails", func(t *testing.T) {
		// A mark acted on stands, as until the worker's clear; the level
		// steps down, the list is back on the named leg, and the start the
		// step up brings fails its stop: that start is not done, though
		// an earlier one of the same mark was, so the reading names no
		// acted mark.
		f := downNoRepair(t)
		f.node.setSuperblock(f.leg2, fakeSuperblock{badBlocks: 2})
		f.syncup(t, withForcedMark(quietReq(4), testDataLeg2))
		info := f.syncup(t, forcedMarkOf(quietReq(5), testDataLeg2, 4))
		if got := actedRevision(info); got != 4 {
			t.Fatalf("the first start reads acted mark %d, want 4", got)
		}
		f.syncup(t, forcedMarkOf(levelReq(6, pb.SpLevel_SP_LEVEL_NO_REDUND),
			testDataLeg2, 4))
		if !f.node.arrayGone(f.mdDev) {
			t.Fatalf("the level step down left the array")
		}
		f.node.setSuperblock(f.leg2, fakeSuperblock{badBlocks: 2})
		f.node.mu.Lock()
		f.node.failCmdAlways["cmd mdadm --stop "] =
			"mdadm: Cannot get exclusive access"
		f.node.mu.Unlock()
		info = f.syncup(t, forcedMarkOf(quietReq(7), testDataLeg2, 4))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"its stop to drop the list failed", "grp data")
		f.node.mu.Lock()
		delete(f.node.failCmdAlways, "cmd mdadm --stop ")
		f.node.mu.Unlock()
		info = f.syncup(t, forcedMarkOf(quietReq(8), testDataLeg2, 4))
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		if got := actedRevision(info); got != 0 {
			t.Fatalf("the converge after the second start's failed stop "+
				"reads acted mark %d, want none", got)
		}
	})
}

// TestForcedStartRefusesANamedLegWithABadBlockList pins CN12's refusal of
// a named leg whose reading shows a bad-block list (badBlocksRefusal): with
// no override nothing is zeroed or assembled, the row names the leg and the
// list's length, the dry run refuses alike, and the read names no forced
// start, so nothing runs between the worker's calls; with the override
// bad-blocks the dry run names the drop, and the start goes on under a mark
// that passes it, here the force of a mark an older gateway wrote.
func TestForcedStartRefusesANamedLegWithABadBlockList(t *testing.T) {
	f := downNoRepair(t)
	f.node.setSuperblock(f.leg2, fakeSuperblock{badBlocks: 3})
	f.node.forgetTraces()
	info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
	assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
		"named leg 0x17 carries a bad-block list of 3 entries", "grp data")
	assertNoCall(t, f.node, "--zero-superblock")
	assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
	f.assertForcedStartUnnamed(t, 4, "a named leg with a bad-block list")
	assertNoCall(t, f.node, "--zero-superblock")

	f.node.Reset()
	dry := f.inspect(t, testDataLeg2).GetDryRun()
	if !strings.Contains(dry.GetRefusal(), "carries a bad-block list") {
		t.Fatalf("the dry run with no override: %v, want the refusal", dry)
	}
	dry = f.inspect(t, testDataLeg2,
		pb.ForcedStartOverride_FORCED_START_OVERRIDE_BAD_BLOCKS).GetDryRun()
	if dry.GetRefusal() != "" ||
		dry.GetUpdate() != mdUpdateUuid+","+mdUpdateForceNoBbl {
		t.Fatalf("the dry run with the override bad-blocks: %v, want the "+
			"list dropped", dry)
	}
	assertNoCall(t, f.node, "cmd mdadm")

	info = f.syncup(t, withForcedMark(quietReq(5), testDataLeg2))
	assertOrder(t, f.node,
		zeroOf(f.leg1),
		f.forced(f.leg2, mdUpdateUuid),
		f.forced(f.leg2, mdUpdateForceNoBbl))
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
}

// TestAssemblyHandsMdOneIdentity pins CN12's assembly of members of two
// identities (assemblyIdentity): the leg_list legs other than an open
// repair's bad leg decide, then the repair's new legs, then its bad leg,
// and a member of another identity is not handed to mdadm, which would
// refuse the whole assembly; two identities among the legs that decide are
// the group's error, with no assembly, and the read, whose dry run of the
// assembly meets the same refusal (absentArrayNamed), names nothing, so
// nothing runs between the worker's calls.
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
		f.node.forgetTraces()
		info := f.syncup(t, quietReq(4))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"members of two identities", "grp data")
		assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
		assertNoCall(t, f.node, "cmd mdadm --create "+f.mdDev)
		if _, reply := f.read(t, 4); strings.Contains(reply.GetDetails(),
			f.absentNamed()) {
			t.Fatalf("the read names the absent array of two identities: "+
				"%q", reply.GetDetails())
		}
		assertNothingRunsBetweenCalls(t, f.node)
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
			f.remove(f.leg1), f.addFresh(f.newLegDev))
		if got := verdict(info).GetRefusal(); got != "" {
			t.Fatalf("the forced repair's refusal is %q", got)
		}
	})
}

// TestTakeBackAddsAFreshMember pins CN12's add of a repair's new leg taken
// back from a retired leg: md must copy it whole, since its write record can
// lack regions written while the leg was away, so the primary adds it with
// mdadm's --add-spare, which never re-adds a member from that record; a new
// leg that was never a retired leg keeps the plain add.
func TestTakeBackAddsAFreshMember(t *testing.T) {
	setup := func(t *testing.T) *repFix {
		f := newRepFix(t, allLegs...)
		f.badLegDead()
		f.node.setSuperblock(f.leg2, fakeSuperblock{
			role: "Active device 1", events: 50})
		f.node.setSuperblock(f.newLegDev, fakeSuperblock{
			role: "Active device 1", events: 10})
		f.node.sameArrayAs(f.newLegDev, f.leg2)
		f.node.Reset()
		return f
	}
	t.Run("taken back", func(t *testing.T) {
		f := setup(t)
		info := f.syncup(t, takenBackReq(3, pb.RepStage_REP_STAGE_CREATED,
			false))
		f.assertMdCalls(t, "the converge", f.fail(f.leg1),
			f.remove(f.leg1), f.addFresh(f.newLegDev))
		if got := verdict(info).GetRefusal(); got != "" {
			t.Fatalf("the take-back's refusal is %q", got)
		}
	})
	t.Run("new", func(t *testing.T) {
		f := setup(t)
		f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
		f.assertMdCalls(t, "the converge", f.fail(f.leg1),
			f.remove(f.leg1), f.add(f.newLegDev))
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

// legReason is the reason of legId's row in info, NONE for a row it lacks.
func legReason(info *pb.CntlrInfo, legId uint64) pb.ResReason {
	return info.GetLegIdToLeg()[legId].GetReason()
}

// TestTheIdentityReasonOnTheLegRow pins CN12's reasons on a leg's row: a
// leg_list member the running array does not add for another identity reads
// RES_REASON_OTHER_IDENTITY on its row, whose status stays the probe's, on
// the converge and on the read, and the verdict does not name it, while the
// leg of the array's identity carries no reason; the row is marked only
// while it reads OK, on both channels alike, and a converge whose
// reconciliation fails after the check, or whose read of the array after
// its member changes fails, still marks it; legs of two
// identities an assembly of the absent array meets read
// RES_REASON_IDENTITY_UNDECIDED, a reason of their own, on both channels.
func TestTheIdentityReasonOnTheLegRow(t *testing.T) {
	// olderLeg1 takes leg 1's member out of the running array and gives its
	// superblock another identity.
	olderLeg1 := func(f *repFix) {
		f.node.mu.Lock()
		defer f.node.mu.Unlock()
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
	}
	t.Run("the running array's case", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		olderLeg1(f)
		info := f.syncup(t, noRepairReq(3))
		read, reply := f.read(t, 3)
		for label, got := range map[string]*pb.CntlrInfo{
			"converge": info, "read": read} {
			row := got.GetLegIdToLeg()[testDataLeg]
			if row.GetReason() != pb.ResReason_RES_REASON_OTHER_IDENTITY ||
				row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
				t.Fatalf("%s: leg 1's row = %v, want OK with the other "+
					"identity's reason", label, row)
			}
			if r := legReason(got, testDataLeg2); r !=
				pb.ResReason_RES_REASON_NONE {
				t.Fatalf("%s: leg 2's reason = %v, want none", label, r)
			}
		}
		if strings.Contains(reply.GetDetails(), legName(f.srv, testDataLeg)) {
			t.Fatalf("the read names the leg of another identity: %v", reply)
		}
	})
	t.Run("a leg row that does not read OK", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		olderLeg1(f)
		failRounds(t, f.srv, testDataLeg, 1)
		info := f.syncup(t, noRepairReq(3))
		read, _ := f.read(t, 3)
		for label, got := range map[string]*pb.CntlrInfo{
			"converge": info, "read": read} {
			row := got.GetLegIdToLeg()[testDataLeg]
			if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
				row.GetReason() != pb.ResReason_RES_REASON_NONE {
				t.Fatalf("%s: leg 1's row = %v, want the probe's ERROR "+
					"with no reason", label, row)
			}
		}
		assertNoCall(t, f.node, f.add(f.leg1))
	})
	t.Run("a converge that fails after the check", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		olderLeg1(f)
		// A third leg_list member after leg 1, fresh, whose add fails: the
		// reconciliation fails after leg 1's check.
		req := noRepairReq(3)
		third := legOf(repNewLeg, sideOf(repNewSide, testIp, testSvcId))
		third.LegIdx = 2
		dataGrpOf(req).LegList = append(dataGrpOf(req).LegList, third)
		f.node.mu.Lock()
		f.node.failCmdAlways["--add --failfast "+f.newLegDev] =
			"mdadm: add new device failed for " + f.newLegDev
		f.node.mu.Unlock()
		info := f.syncup(t, req)
		if row := info.GetGrpIdToMdRaid()[testDataGrp]; row.GetStatus() !=
			pb.ResStatus_RES_STATUS_ERROR {
			t.Fatalf("fixture: the group's row = %v, want the add's ERROR",
				row)
		}
		if !f.node.hasCall(f.add(f.newLegDev)) {
			t.Fatalf("fixture: no add of the third leg ran")
		}
		if r := legReason(info, testDataLeg); r !=
			pb.ResReason_RES_REASON_OTHER_IDENTITY {
			t.Fatalf("leg 1's reason = %v after the failed add, want the "+
				"other identity's", r)
		}
	})
	t.Run("an array read again that fails", func(t *testing.T) {
		// The third leg's add goes through, and the md listing of the
		// array after it is killed, so the array read again gives no
		// detail: the converge keeps leg 1's mark, as the read has it.
		withThird := func() *pb.SyncupCntlrRequest {
			req := noRepairReq(3)
			third := legOf(repNewLeg, sideOf(repNewSide, testIp, testSvcId))
			third.LegIdx = 2
			dataGrpOf(req).LegList = append(dataGrpOf(req).LegList, third)
			return req
		}
		// listingsBeforeAdd counts the md listings of the array a converge
		// runs before the third leg's add.
		listingsBeforeAdd := func(t *testing.T) (string, int) {
			f := newRepFix(t, allLegs...)
			olderLeg1(f)
			f.node.mu.Lock()
			key := "list /sys/block/" + f.node.arrays[f.mdDev].node + "/md"
			f.node.mu.Unlock()
			f.node.Reset()
			f.syncup(t, withThird())
			before, after, added := 0, 0, false
			for _, call := range f.node.Calls() {
				if strings.Contains(call, f.add(f.newLegDev)) {
					added = true
				}
				if strings.Contains(call, key) {
					if added {
						after++
					} else {
						before++
					}
				}
			}
			if !added || after == 0 {
				t.Fatalf("fixture: no add of the third leg, or no md " +
					"listing after it")
			}
			return key, before
		}
		key, before := listingsBeforeAdd(t)
		f := newRepFix(t, allLegs...)
		olderLeg1(f)
		f.node.mu.Lock()
		f.node.killCmdNth[key] = before + 1
		f.node.mu.Unlock()
		f.node.Reset()
		info := f.syncup(t, withThird())
		if !f.node.hasCall(f.add(f.newLegDev)) {
			t.Fatalf("fixture: no add of the third leg ran")
		}
		if got := info.GetGrpIdToGrpInfo()[testDataGrp]; got != nil {
			t.Fatalf("fixture: the converge read the array again: %v", got)
		}
		read, _ := f.read(t, 3)
		for label, got := range map[string]*pb.CntlrInfo{
			"converge": info, "read": read} {
			if r := legReason(got, testDataLeg); r !=
				pb.ResReason_RES_REASON_OTHER_IDENTITY {
				t.Fatalf("%s: leg 1's reason = %v, want the other "+
					"identity's", label, r)
			}
		}
	})
	t.Run("the assembly's case", func(t *testing.T) {
		f := downNoRepair(t)
		f.identity(f.leg1, "older")
		info := f.syncup(t, quietReq(4))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"members of two identities", "grp data")
		read, _ := f.read(t, 4)
		for label, got := range map[string]*pb.CntlrInfo{
			"converge": info, "read": read} {
			for _, legId := range []uint64{testDataLeg, testDataLeg2} {
				if r := legReason(got, legId); r !=
					pb.ResReason_RES_REASON_IDENTITY_UNDECIDED {
					t.Fatalf("%s: leg %#x's reason = %v, want the "+
						"undecided identity's", label, legId, r)
				}
			}
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
// zeroes leaves them, the group's row reads ERROR and no array is created.
// The read, whose dry run of the assembly meets the same refusal
// (absentArrayNamed), names nothing, and nothing runs between the worker's
// calls. Once a leg with a superblock serves — one whose superblock records
// its peer failed, which mdadm starts alone — the read names the absent
// array, and the re-send on that round assembles it from that leg, its peer
// added after. The same group without the flag is created.
func TestNoCreateOnceTheArrayRan(t *testing.T) {
	for _, made := range []bool{true, false} {
		f := downNoRepair(t)
		f.node.mu.Lock()
		delete(f.node.superblocks, f.leg1)
		delete(f.node.superblocks, f.leg2)
		f.node.mu.Unlock()
		f.node.forgetTraces()
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
		if _, reply := f.read(t, 4); strings.Contains(reply.GetDetails(),
			f.absentNamed()) {
			t.Fatalf("the read names the absent array of a group whose "+
				"array ran, with no superblock: %q", reply.GetDetails())
		}
		assertNothingRunsBetweenCalls(t, f.node)

		f.node.setSuperblock(f.leg2, fakeSuperblock{
			role: "Active device 1", events: 10, arrayState: ".A"})
		f.node.Reset()
		round, resent := resendRound(t, f.srv, req, "named")
		if !strings.Contains(round.GetAgentReply().GetDetails(),
			f.absentNamed()) || resent == nil {
			t.Fatalf("the round once leg 2 carries a superblock is %v, "+
				"re-sent %v; want the absent array named and a re-send",
				round.GetAgentReply(), resent != nil)
		}
		assertNoCall(t, f.node, "cmd mdadm --create "+f.mdDev)
		if !tracedCall(f.node, "named", "cmd mdadm --assemble "+f.mdDev) {
			t.Fatalf("no assembly under the round's trace id:\n%s",
				strings.Join(f.node.Traced("named"), "\n"))
		}
		assertOk(t, resent.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
			"grp data after the re-send")
	}
}

// TestRepairOfALegOfAnotherIdentityAddsTheNewLeg pins the agent's half of an
// operator's repair of a leg_list leg the running array does not take back
// for another identity (architecture.md, Repairs; CN12): the bad leg is
// outside the array, so the converge of a CREATED repair adds the new leg
// and takes nothing out — md holds nothing on the bad leg — and the
// verdict reads started once the new leg holds a place, as the sp worker's
// stage move needs; the bad leg's superblock is left as it is.
func TestRepairOfALegOfAnotherIdentityAddsTheNewLeg(t *testing.T) {
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
	info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
	f.assertMdCalls(t, "the converge", f.add(f.newLegDev))
	assertVerdict(t, info, "the repair", true, false)
	if got := f.legUuid(f.leg1); got != "older" {
		t.Fatalf("the bad leg's identity is %q, want it left as it was", got)
	}
}
