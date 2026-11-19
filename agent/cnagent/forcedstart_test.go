package cnagent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The forced start of cnagent.md CN12 and the take-back check of a repair's
// add, on the repair fixture's two-leg data group (repFix).

// withMark sets the data group's forced-start mark, naming legId, on req.
func withMark(req *pb.SyncupCntlrRequest, legId uint64) *pb.SyncupCntlrRequest {
	dataGrpOf(req).ForcedStart = &pb.ForcedStart{
		LegId: legId, Revision: req.GetRevision()}
	return req
}

// levelReq is the fixture's two-leg raid1 request with no repair at an
// sp_level, on the primary.
func levelReq(revision uint64, level pb.SpLevel) *pb.SyncupCntlrRequest {
	return cntlrReq(reqOpts{revision: revision, primary: true, raid1: true,
		twoLegs: true, level: level})
}

// takenBackReq is repairReq whose new leg was taken from a retired leg
// (repPlan.takenBack).
func takenBackReq(
	revision uint64,
	stage pb.RepStage,
	abort bool,
	opts ...repOpts,
) *pb.SyncupCntlrRequest {
	req := repairReq(revision, stage, abort, opts...)
	req.NameToRepair[repName].NewLegIdToRetiredEpoch =
		map[uint64]uint64{repNewLeg: 7}
	return req
}

// quietReq is the fixture's two-leg raid1 request with no repair and no
// subsystem, on the primary: no namespace waits for the pool (CN16), so a
// read's verdict names no namespace row beside the group's names.
func quietReq(revision uint64) *pb.SyncupCntlrRequest {
	return cntlrReq(reqOpts{revision: revision, primary: true, raid1: true,
		twoLegs: true, subsys: map[string]*pb.Subsystem{}})
}

// quietLevel is quietReq at the level that builds no thin pool, where no
// pool's concat holds a group's array open.
func quietLevel(revision uint64) *pb.SyncupCntlrRequest {
	return cntlrReq(reqOpts{revision: revision, primary: true,
		raid1: true, twoLegs: true,
		level:  pb.SpLevel_SP_LEVEL_NO_THINPOOL,
		subsys: map[string]*pb.Subsystem{}})
}

// downFix is the repair fixture with no array of the data group, as a
// failover leaves it on the new primary: converged as primary (newRepFix),
// then demoted by standby, whose sweep stops the arrays.
func downFix(t *testing.T, standby *pb.SyncupCntlrRequest) *repFix {
	t.Helper()
	f := newRepFix(t, allLegs...)
	f.syncup(t, standby)
	if !f.node.arrayGone(f.mdDev) {
		t.Fatalf("the demotion left the array")
	}
	f.node.Reset()
	return f
}

// downNoRepair is downFix with no repair at revision 3.
func downNoRepair(t *testing.T) *repFix {
	t.Helper()
	return downFix(t, cntlrReq(reqOpts{revision: 3, raid1: true,
		twoLegs: true}))
}

func (f *repFix) arrayName() string {
	return f.srv.nf.CnMdArrayName(testSp, 0, 0, false)
}

// assertRanOnce asserts one command of the fixture's node carried part.
func (f *repFix) assertRanOnce(t *testing.T, part string) {
	t.Helper()
	if calls := f.node.callsMatching(part); len(calls) != 1 {
		t.Fatalf("commands with %q: %v, want one", part, calls)
	}
}

// forced is the forced assembly of the data group's array from member with
// one superblock update.
func (f *repFix) forced(member, update string) string {
	return "cmd mdadm --assemble " + f.mdDev + " --metadata=1.2 --name " +
		f.arrayName() + " --run --force --update=" + update + " " + member
}

// legUuid is the identity of the array member's superblock names.
func (f *repFix) legUuid(member string) string {
	f.node.mu.Lock()
	defer f.node.mu.Unlock()
	return f.node.memberArray[member]
}

// assertNewIdentity checks that the data group's array runs under an
// identity other than old, the one its members' superblocks named before the
// forced start, and that every member it holds carries it.
func (f *repFix) assertNewIdentity(t *testing.T, old string) {
	t.Helper()
	f.node.mu.Lock()
	defer f.node.mu.Unlock()
	array := f.node.arrays[f.mdDev]
	if array == nil || array.uuid == "" || old == "" || array.uuid == old {
		t.Fatalf("the started array's identity is %v, want one other than "+
			"%q", array, old)
	}
	for _, member := range array.members {
		if got := f.node.memberArray[member]; got != array.uuid {
			t.Fatalf("member %s names array %q, want %q", member, got,
				array.uuid)
		}
	}
}

func zeroOf(member string) string {
	return "cmd mdadm --zero-superblock --metadata=1.2 " + member
}

// examineOf is the read of member's superblock area (Md.ReadMember), the
// reading every md decision of CN12 takes.
func examineOf(member string) string {
	return "readsb " + member + " off=4096 len=4096"
}

// examinedMembers are the members whose superblock area the calls read, in
// order.
func examinedMembers(node *fakeNode) []string {
	var out []string
	for _, call := range node.callsMatching("readsb /") {
		member, _, _ := strings.Cut(strings.TrimPrefix(call, "readsb "), " ")
		out = append(out, member)
	}
	return out
}

// forcedStartResent plays the worker's round whose read names the data
// group's forced start from legId (forcedStartNamed; cnagent.md CN30), under
// traceId: the round's verdict names it, and the round brings one re-send of
// held, whose reply it returns. The caller judges that converge by the
// commands it ran.
func (f *repFix) forcedStartResent(
	t *testing.T,
	held *pb.SyncupCntlrRequest,
	legId uint64,
	traceId string,
) *pb.SyncupCntlrReply {
	t.Helper()
	round, resent := resendRound(t, f.srv, held, traceId)
	if !strings.Contains(round.GetAgentReply().GetDetails(),
		forcedNamed(legId)) {
		t.Fatalf("%s: the round's verdict is code %d (%q), want the forced "+
			"start from leg %#x named", traceId,
			round.GetAgentReply().GetCode(),
			round.GetAgentReply().GetDetails(), legId)
	}
	if resent == nil {
		t.Fatalf("%s: the round that names the forced start brought no "+
			"re-send", traceId)
	}
	return resent
}

// assertForcedStartUnnamed checks that a read of the fixture cntlr at
// revision names no forced start of the data group, as for a refusal that
// lasts, and that nothing ran meanwhile, or ran before it since the test's
// last forgetTraces, under an id the agent made itself, own being the test's
// ids: no converge acts on the mark between the worker's calls.
func (f *repFix) assertForcedStartUnnamed(
	t *testing.T,
	revision uint64,
	label string,
	own ...string,
) {
	t.Helper()
	if _, reply := f.read(t, revision); strings.Contains(reply.GetDetails(),
		"forced start of group") {
		t.Fatalf("%s: the read names the forced start: %q", label,
			reply.GetDetails())
	}
	assertNothingRunsBetweenCalls(t, f.node, own...)
}

// assertForcedRecord checks the one forced start record the converge
// logged.
func assertForcedRecord(
	t *testing.T,
	logs *logCapture,
	legId uint64,
	update string,
	zeroed ...uint64,
) {
	t.Helper()
	recs := logs.msgRecords(t, msgForcedStart)
	if len(recs) != 1 {
		t.Fatalf("%d %q records, want 1", len(recs), msgForcedStart)
	}
	rec := recs[0]
	if rec["level"] != "WARN" || rec["grp_id"] != float64(testDataGrp) ||
		rec["leg_id"] != float64(legId) || rec["update"] != update ||
		rec["cntlr_id"] != float64(testCntlr) {
		t.Fatalf("the forced start record is %v, want leg %#x update %s",
			rec, legId, update)
	}
	got, _ := rec["zeroed"].([]any)
	if len(got) != len(zeroed) {
		t.Fatalf("the record's zeroed legs are %v, want %v", got, zeroed)
	}
	for i, legId := range zeroed {
		if got[i] != float64(legId) {
			t.Fatalf("the record's zeroed legs are %v, want %v", got, zeroed)
		}
	}
}

// TestForcedStartFromANamedLeg pins CN12's forced start: with no array of
// the group and the mark naming leg 2, the primary examines leg 2's
// superblock, zeroes every other member the forced start keeps, starts the
// array from leg 2 alone under a new identity, and the member reconciliation
// then adds the zeroed leg, in that order, with one Warn record; the assembly
// of a group without a mark is not run. With the array running the mark waits:
// a later converge zeroes, examines and assembles nothing.
func TestForcedStartFromANamedLeg(t *testing.T) {
	f := downNoRepair(t)
	oldUuid := f.legUuid(f.leg2)
	logs := captureLogs(t)
	info := f.syncup(t, withMark(noRepairReq(4), testDataLeg2))
	assertOrder(t, f.node,
		examineOf(f.leg2),
		zeroOf(f.leg1),
		f.forced(f.leg2, mdUpdateUuid),
		f.add(f.leg1))
	assertNoCall(t, f.node, zeroOf(f.leg2))
	assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev+
		" --metadata=1.2 --name "+f.arrayName()+" /dev/")
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	assertOk(t, info.GetSliceIdToDmPool()[testSlice], "pool")
	if members := f.members(); len(members) != 2 {
		t.Fatalf("the started array holds %v, want both legs", members)
	}
	assertForcedRecord(t, logs, testDataLeg2, mdUpdateUuid, testDataLeg)
	f.assertNewIdentity(t, oldUuid)

	f.node.Reset()
	info = f.syncup(t, withMark(noRepairReq(5), testDataLeg2))
	assertNoCall(t, f.node, "--zero-superblock")
	assertNoCall(t, f.node, "cmd mdadm --assemble")
	assertNoCall(t, f.node, examineOf(f.leg2))
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data, waiting")
	if n := len(logs.msgRecords(t, msgForcedStart)); n != 1 {
		t.Fatalf("%d forced start records after the waiting converge", n)
	}
}

// markOf is the request with the group's mark naming legId at a mark
// revision of its own, as the fan-out carries a mark written before the
// request's revision.
func markOf(
	req *pb.SyncupCntlrRequest,
	legId, revision uint64,
) *pb.SyncupCntlrRequest {
	dataGrpOf(req).ForcedStart = &pb.ForcedStart{
		LegId: legId, Revision: revision}
	return req
}

// actedRevision is the mark revision a report's reading of the data group
// names as the one the primary acted on, zero for none or no reading.
func actedRevision(info *pb.CntlrInfo) uint64 {
	return info.GetGrpIdToGrpInfo()[testDataGrp].GetForcedStartRevision()
}

// TestForcedStartReportsTheMarkItActedOn pins the acted mark (cnagent.md
// CN12, CN28): a forced start that starts the array makes the group's
// reading name the mark's revision, on the converge and on the read, for as
// long as the request carries a mark of that revision, while the array runs
// on with the mark waiting; a mark written since, or the clear, reads zero,
// and the memory goes with the clear, so the same revision back reads zero.
// A mark written over an array that runs was never acted on and reads zero,
// and so does the start a restart forgot.
func TestForcedStartReportsTheMarkItActedOn(t *testing.T) {
	t.Run("the start", func(t *testing.T) {
		f := downNoRepair(t)
		info := f.syncup(t, markOf(noRepairReq(4), testDataLeg2, 4))
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		if got := actedRevision(info); got != 4 {
			t.Fatalf("the converge that started the array reads acted "+
				"mark %d, want 4", got)
		}
		if read, _ := f.read(t, 4); actedRevision(read) != 4 {
			t.Fatalf("the read after the start reads acted mark %d, want 4",
				actedRevision(read))
		}
		info = f.syncup(t, markOf(noRepairReq(5), testDataLeg2, 4))
		if got := actedRevision(info); got != 4 {
			t.Fatalf("a converge of the same mark reads acted mark %d, "+
				"want 4", got)
		}
		info = f.syncup(t, markOf(noRepairReq(6), testDataLeg2, 6))
		if got := actedRevision(info); got != 0 {
			t.Fatalf("a mark written over the running array reads acted "+
				"mark %d, want 0", got)
		}
		info = f.syncup(t, noRepairReq(7))
		if got := actedRevision(info); got != 0 {
			t.Fatalf("the clear reads acted mark %d, want 0", got)
		}
		info = f.syncup(t, markOf(noRepairReq(8), testDataLeg2, 4))
		if got := actedRevision(info); got != 0 {
			t.Fatalf("the cleared mark's revision back reads acted mark %d, "+
				"want 0: the clear left the memory", got)
		}
	})
	t.Run("a mark over a running array", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		f.node.Reset()
		info := f.syncup(t, withMark(quietLevel(4), testDataLeg2))
		f.assertMarkLeftWaiting(t, f.leg2)
		if got := actedRevision(info); got != 0 {
			t.Fatalf("a mark the primary never acted on reads acted mark "+
				"%d, want 0", got)
		}
		if read, _ := f.read(t, 4); actedRevision(read) != 0 {
			t.Fatalf("the read reads acted mark %d, want 0",
				actedRevision(read))
		}
	})
	// A read can meet a request whose mark no converge has judged yet: the
	// memory of an older mark names nothing then, before keepForcedStarts
	// drops it.
	// A request that does not count as delivered names none either, on
	// the read as on its converge, which acts on no mark.
	t.Run("a newer mark before its converge", func(t *testing.T) {
		for _, tc := range []struct {
			mark      *pb.ForcedStart
			delivered bool
			want      uint64
		}{
			{&pb.ForcedStart{LegId: testDataLeg2, Revision: 4}, true, 4},
			{&pb.ForcedStart{LegId: testDataLeg2, Revision: 6}, true, 0},
			{nil, true, 0},
			{&pb.ForcedStart{LegId: testDataLeg2, Revision: 4}, false, 0},
		} {
			st := &cntlrState{
				forcedStarts: map[uint64]uint64{testDataGrp: 4},
				reqFromRpc:   tc.delivered,
			}
			gp := &grpPlan{grpId: testDataGrp, mark: tc.mark}
			if got := actedMark(st, gp); got != tc.want {
				t.Errorf("actedMark with the memory at 4, the mark %v and "+
					"delivered %v = %d, want %d", tc.mark, tc.delivered,
					got, tc.want)
			}
		}
	})
	t.Run("a restart forgets the start", func(t *testing.T) {
		f := downNoRepair(t)
		req := markOf(noRepairReq(4), testDataLeg2, 4)
		if got := actedRevision(f.syncup(t, req)); got != 4 {
			t.Fatalf("the start reads acted mark %d, want 4", got)
		}
		fresh := newCnServer(f.node)
		reconcileForTest(t, fresh)
		f.srv = fresh
		info := f.syncup(t, markOf(noRepairReq(5), testDataLeg2, 4))
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		if got := actedRevision(info); got != 0 {
			t.Fatalf("after a restart the running array reads acted mark "+
				"%d, want 0", got)
		}
	})
}

// TestForcedStartFromARepairNewLeg pins the forced start from the new leg
// of the group's open repair, taken from a retired leg: the bad leg, dead,
// is neither waited for nor zeroed, leg 2 is zeroed, the array starts from
// the new leg alone, leg 2 is added, and the repair's verdict says started.
func TestForcedStartFromARepairNewLeg(t *testing.T) {
	f := downFix(t, takenBackReq(3, pb.RepStage_REP_STAGE_CREATED, false,
		repOpts{standby: true}))
	f.badLegDead()
	f.node.setSuperblock(f.newLegDev, fakeSuperblock{
		role: "Active device 1", events: 40})
	info := f.syncup(t, withMark(
		takenBackReq(4, pb.RepStage_REP_STAGE_CREATED, false), repNewLeg))
	assertOrder(t, f.node,
		examineOf(f.newLegDev),
		zeroOf(f.leg2),
		f.forced(f.newLegDev, mdUpdateUuid),
		f.add(f.leg2))
	assertNoCall(t, f.node, zeroOf(f.leg1))
	assertNoCall(t, f.node, f.add(f.leg1))
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	assertVerdict(t, info, "the started repair", true, false)
	if got := verdict(info).GetRefusal(); got != "" {
		t.Fatalf("the started repair's refusal is %q", got)
	}
}

// TestForcedStartRefusesANamedLegThatIsNotFull pins the named leg's
// examine: a half-copied leg, a spare, a leg with no superblock and an
// examine that did not answer each leave the group ERROR and zero nothing.
// The first three last, so the read, whose dry run of the forced start
// meets the same refusal (forcedStartNamed), names nothing and nothing runs
// between the worker's calls. An examine that did not answer names nothing
// either while it does not answer; once it answers, the read names the
// forced start, and the re-send on that round starts the array.
func TestForcedStartRefusesANamedLegThatIsNotFull(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    func(f *repFix)
		want   string
		passes func(f *repFix)
	}{
		{"half-copied", func(f *repFix) {
			f.node.setSuperblock(f.leg2, fakeSuperblock{
				role: "Active device 1", recovery: "45056 sectors"})
		}, "not a full member: recovery offset 45056 sectors", nil},
		{"spare", func(f *repFix) {
			f.node.setSuperblock(f.leg2, fakeSuperblock{role: "spare"})
		}, "not a full member: device role spare", nil},
		{"no superblock", func(f *repFix) {
			f.node.mu.Lock()
			delete(f.node.superblocks, f.leg2)
			f.node.mu.Unlock()
		}, "not a full member: no md superblock", nil},
		{"examine killed", func(f *repFix) {
			f.node.mu.Lock()
			f.node.killCmdAlways[examineOf(f.leg2)] = true
			f.node.mu.Unlock()
		}, "not read", func(f *repFix) {
			f.node.mu.Lock()
			delete(f.node.killCmdAlways, examineOf(f.leg2))
			f.node.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := downNoRepair(t)
			tc.set(f)
			f.node.forgetTraces()
			held := withMark(quietReq(4), testDataLeg2)
			info := f.syncup(t, held)
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
				tc.want, "grp data")
			assertNoCall(t, f.node, "--zero-superblock")
			assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
			f.assertForcedStartUnnamed(t, 4, "the refused forced start")
			if tc.passes == nil {
				return
			}
			tc.passes(f)
			f.node.Reset()
			f.forcedStartResent(t, held, testDataLeg2, "examined")
			assertOrder(t, f.node,
				examineOf(f.leg2),
				zeroOf(f.leg1),
				f.forced(f.leg2, mdUpdateUuid),
				f.add(f.leg1))
			f.assertForcedStartUnnamed(t, 4, "after the start", "examined")
		})
	}
}

// TestForcedStartStopsAnArrayThatDoesNotRun pins the forced start's stop:
// a broken array of the group is stopped before anything is zeroed, at a
// level where no pool holds it, and the start goes on in the same pass; at
// a level that builds thin pools the slice's concat holds it, the stop
// fails, and the pass ends with nothing zeroed or assembled; md's holders
// listing shows the concat, so the read names nothing for as long as the
// concat holds, and nothing tries the stop again between the worker's calls.
// A running array is never stopped, nor one that holds a member the sweep
// would not call ours — no dm device, a dn agent's device, another sp's leg
// — which is the group's error (cnagent.md CN21). A stop whose verify meets
// the EBUSY array_state of an array md has marked deleted ends the pass with
// the group's error; the read after it finds the array gone and names the
// forced start, and the re-send on that round starts the array.
func TestForcedStartStopsAnArrayThatDoesNotRun(t *testing.T) {
	t.Run("no pool holds it", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, levelReq(3, pb.SpLevel_SP_LEVEL_NO_THINPOOL))
		f.node.setArrayState(f.mdDev, "broken")
		node := f.node.mdNode(f.mdDev)
		f.node.Reset()
		info := f.syncup(t, withMark(
			levelReq(4, pb.SpLevel_SP_LEVEL_NO_THINPOOL), testDataLeg2))
		assertOrder(t, f.node,
			"cmd mdadm --stop "+node,
			zeroOf(f.leg1),
			f.forced(f.leg2, mdUpdateUuid),
			f.add(f.leg1))
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	})
	t.Run("the pool's concat holds it", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.node.setArrayState(f.mdDev, "broken")
		f.node.forgetTraces()
		f.node.Reset()
		info := f.syncup(t, withMark(quietReq(3), testDataLeg2))
		if !f.node.hasCall("cmd mdadm --stop ") {
			t.Fatalf("no stop of the broken array")
		}
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"Cannot get exclusive access", "grp data")
		assertNoCall(t, f.node, "--zero-superblock")
		assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
		if f.node.arrayGone(f.mdDev) {
			t.Fatalf("the held array is gone")
		}
		f.node.Reset()
		f.assertForcedStartUnnamed(t, 3, "the array the concat holds")
		assertNoCall(t, f.node, "cmd mdadm --stop ")
	})
	t.Run("running", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.node.Reset()
		f.syncup(t, withMark(levelReq(3, pb.SpLevel_SP_LEVEL_NO_THINPOOL),
			testDataLeg2))
		assertNoCall(t, f.node, "cmd mdadm --stop "+f.node.mdNode(f.mdDev))
	})
	for _, tc := range []struct {
		name   string
		member func(f *repFix) string
		want   string
	}{
		{"it holds a member that is no dm device", func(*repFix) string {
			return "/dev/sdb1"
		}, "holds a foreign member"},
		{"it holds a dn agent's device", func(f *repFix) string {
			return "/dev/mapper/" + names(f.srv).DnLinearName(
				testCluster, 0xd1, testSp, 0x51, testCn)
		}, "which is no leg of this sp"},
		{"it holds another sp's leg", func(f *repFix) string {
			return "/dev/mapper/" + names(f.srv).CnLegName(
				testCluster, testCn, testSp+1, testDataLeg)
		}, "which is no leg of this sp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			f.syncup(t, levelReq(3, pb.SpLevel_SP_LEVEL_NO_THINPOOL))
			f.node.mu.Lock()
			array := f.node.arrays[f.mdDev]
			held := append(append([]string(nil), array.members...),
				tc.member(f))
			name := array.name
			f.node.mu.Unlock()
			f.node.seedArray(f.mdDev, name, held...)
			f.node.setArrayState(f.mdDev, "inactive")
			f.node.Reset()
			info := f.syncup(t, withMark(
				levelReq(4, pb.SpLevel_SP_LEVEL_NO_THINPOOL), testDataLeg2))
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
				tc.want, "grp data")
			assertNoCall(t, f.node, "cmd mdadm --stop ")
			assertNoCall(t, f.node, "--zero-superblock")
			assertNoCall(t, f.node, "cmd mdadm --assemble")
			if f.node.arrayGone(f.mdDev) {
				t.Fatalf("the array that holds a member that is not ours " +
					"is gone")
			}
		})
	}
	t.Run("the stop's verify does not answer", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		f.node.setArrayState(f.mdDev, "broken")
		node := f.node.mdNode(f.mdDev)
		f.node.mu.Lock()
		f.node.stopDeleting = true
		f.node.mu.Unlock()
		f.node.Reset()
		held := withMark(quietLevel(4), testDataLeg2)
		info := f.syncup(t, held)
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"array "+node+" stop not verified", "grp data")
		assertNoCall(t, f.node, "--zero-superblock")
		assertNoCall(t, f.node, "cmd mdadm --assemble")
		f.node.mu.Lock()
		f.node.stopDeleting = false
		f.node.mu.Unlock()
		f.node.forgetTraces()
		f.node.Reset()
		f.forcedStartResent(t, held, testDataLeg2, "named")
		assertOrder(t, f.node,
			zeroOf(f.leg1),
			f.forced(f.leg2, mdUpdateUuid),
			f.add(f.leg1))
		assertNoCall(t, f.node, "cmd mdadm --stop ")
		if !tracedCall(f.node, "named", zeroOf(f.leg1)) {
			t.Fatalf("the zero did not run under the round's trace id")
		}
		assertOk(t, probedCntlrInfo(t, f.srv).GetGrpIdToMdRaid()[testDataGrp],
			"grp data after the re-send")
		f.assertForcedStartUnnamed(t, 4, "after the start", "named")
	})
}

// TestForcedStartCountsAStopTheKernelCompleted pins the forced start's
// verify of its stop (stopGroupArray): a stop of the broken array killed at
// the soft timeout after the kernel completed it is read gone from sysfs, as
// the sweep reads one (Md.Gone), so the same pass goes on to the zero, the
// forced assembly and the add, the group's row OK, and the read after it
// names no forced start.
func TestForcedStartCountsAStopTheKernelCompleted(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, quietLevel(3))
	f.node.setArrayState(f.mdDev, "broken")
	node := f.node.mdNode(f.mdDev)
	f.node.mu.Lock()
	f.node.killCmd["cmd mdadm --stop "+node] = true
	f.node.mu.Unlock()
	f.node.Reset()
	info := f.syncup(t, withMark(quietLevel(4), testDataLeg2))
	assertOrder(t, f.node,
		"cmd mdadm --stop "+node,
		zeroOf(f.leg1),
		f.forced(f.leg2, mdUpdateUuid),
		f.add(f.leg1))
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	f.node.forgetTraces()
	f.assertForcedStartUnnamed(t, 4, "after the start")
}

// TestForcedStartLeavesASecondArrayFoundAfterTheStop pins the forced start's
// read after its stop (forcedStartPass): the group's broken array holds the
// named leg alone, and a second array of the group, inactive, holds the
// other leg, whose md/ listing did not answer when the pass walked, so the
// first read matched the broken array alone. After the stop the walk lists
// the second array, the read finds it, and the pass hands it to the test of
// an array that runs: nothing is zeroed or assembled beside it, and the
// group's row names it. The read after it finds the second array, which
// does not run, and names the forced start; the re-send on that round stops
// it in its turn and starts the array from the named leg.
func TestForcedStartLeavesASecondArrayFoundAfterTheStop(t *testing.T) {
	f, node, split := splitAfterTheStop(t, "inactive")
	held := withMark(quietLevel(4), testDataLeg2)
	info := f.syncup(t, held)
	if !f.node.hasCall("cmd mdadm --stop " + node) {
		t.Fatalf("no stop of the broken array, which the first read " +
			"matched alone")
	}
	assertNoCall(t, f.node, "cmd mdadm --stop "+split)
	assertNoCall(t, f.node, "--zero-superblock")
	assertNoCall(t, f.node, "cmd mdadm --assemble")
	assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
		"array "+split+" is inactive", "grp data")
	f.node.forgetTraces()
	f.node.Reset()
	f.forcedStartResent(t, held, testDataLeg2, "named")
	assertOrder(t, f.node,
		"cmd mdadm --stop "+split,
		zeroOf(f.leg1),
		f.forced(f.leg2, mdUpdateUuid),
		f.add(f.leg1))
	if !tracedCall(f.node, "named", "cmd mdadm --stop "+split) {
		t.Fatalf("the stop did not run under the round's trace id")
	}
	f.assertForcedStartUnnamed(t, 4, "after the start", "named")
}

// splitAfterTheStop is TestForcedStartLeavesASecondArrayFoundAfterTheStop's
// node, the second array in state: a split mirror, the group's broken array
// holding leg 2 alone and the second array leg 1, the second array's md/
// listing answering the sweep and the walk after the stop but not the
// build's first walk. node and split are the two arrays' kernel nodes.
func splitAfterTheStop(
	t *testing.T,
	state string,
) (f *repFix, node, split string) {
	t.Helper()
	f = newRepFix(t, allLegs...)
	f.syncup(t, quietLevel(3))
	f.node.mu.Lock()
	array := f.node.arrays[f.mdDev]
	array.members = slices.DeleteFunc(slices.Clone(array.members),
		func(member string) bool { return member == f.leg1 })
	array.degraded = 1
	f.node.publishArray(array)
	f.node.mu.Unlock()
	f.node.setArrayState(f.mdDev, "broken")
	node = f.node.mdNode(f.mdDev)
	f.node.seedArray("/dev/md/split", "split", f.leg1)
	f.node.setArrayState("/dev/md/split", state)
	split = f.node.mdNode("/dev/md/split")
	f.node.mu.Lock()
	f.node.killCmdNth["list "+sysfsBlockDir+"/"+
		strings.TrimPrefix(split, "/dev/")+"/md"] = 2
	f.node.mu.Unlock()
	f.node.Reset()
	return f, node, split
}

// TestForcedStartLeavesTheMarkOverASecondArrayThatRuns is
// TestForcedStartLeavesASecondArrayFoundAfterTheStop with the second array
// running (forcedStartPass; cnagent.md CN12): after the stop of the broken
// array the read finds the running one, and the member reconciliation runs
// over it as without a mark, nothing zeroed or assembled beside it, its add
// of leg 2 failing here. The pass ran no forced start, so it leaves the mark
// waiting, as a pass whose first read finds the array running does, and a
// read names no forced start while an array of the group runs
// (forcedStartNamed); nothing acts on the mark between the worker's calls.
func TestForcedStartLeavesTheMarkOverASecondArrayThatRuns(t *testing.T) {
	f, node, split := splitAfterTheStop(t, "clean")
	// Leg 2 carries the second array's identity, so that the reconciliation
	// over it takes leg 2 as its own member (legListAddRefusal).
	f.node.mu.Lock()
	delete(f.node.memberArray, f.leg2)
	f.node.mu.Unlock()
	f.syncup(t, withMark(quietLevel(4), testDataLeg2))
	if !f.node.hasCall("cmd mdadm --stop " + node) {
		t.Fatalf("no stop of the broken array, which the first read " +
			"matched alone")
	}
	assertNoCall(t, f.node, "cmd mdadm --stop "+split)
	assertNoCall(t, f.node, "--zero-superblock")
	assertNoCall(t, f.node, "cmd mdadm --assemble")
	if !f.node.hasCall(f.add(f.leg2)) {
		t.Fatalf("no member reconciliation over the second array")
	}
	f.node.forgetTraces()
	f.node.Reset()
	f.assertForcedStartUnnamed(t, 4, "the mark over the array that runs")
	assertNoCall(t, f.node, "--zero-superblock")
}

// TestForcedStartAfterAReadThatDidNotAnswer pins a pass whose read of the
// group's array, before any step of the forced start, does not answer while
// the mark stands (cnagent.md CN12): one member's state read of a broken
// array killed leaves the group ERROR, nothing stopped or zeroed. A read of
// the cntlr meets the same read and names nothing for the group (CN30), and
// nothing runs between the worker's calls. Once the read answers, a read
// names the forced start, and the re-send on that round stops the array and
// starts it from the named leg. With no mark the same read leaves the group
// ERROR too, and once it answers a read names the broken array, whose
// re-send assembles it again (CN12, The re-assembly).
func TestForcedStartAfterAReadThatDidNotAnswer(t *testing.T) {
	for _, marked := range []bool{true, false} {
		name := "with the mark"
		if !marked {
			name = "with no mark"
		}
		t.Run(name, func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			f.syncup(t, quietLevel(3))
			f.node.setArrayState(f.mdDev, "broken")
			node := f.node.mdNode(f.mdDev)
			state := "/dev-" + f.node.kernelName(f.leg1) + "/state"
			f.node.mu.Lock()
			f.node.killReadAlways[state] = true
			f.node.mu.Unlock()
			f.node.forgetTraces()
			f.node.Reset()
			held := quietLevel(4)
			if marked {
				held = withMark(held, testDataLeg2)
			}
			info := f.syncup(t, held)
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
				state, "grp data")
			assertNoCall(t, f.node, "cmd mdadm --stop ")
			assertNoCall(t, f.node, "--zero-superblock")
			if _, reply := f.read(t, 4); strings.Contains(
				reply.GetDetails(), "forced start of group") ||
				strings.Contains(reply.GetDetails(),
					agent.LeftoverKindMdBroken+":") {
				t.Fatalf("the read that did not answer names the group: %q",
					reply.GetDetails())
			}
			assertNothingRunsBetweenCalls(t, f.node)

			f.node.mu.Lock()
			delete(f.node.killReadAlways, state)
			f.node.mu.Unlock()
			f.node.Reset()
			if marked {
				f.forcedStartResent(t, held, testDataLeg2, "named")
				assertOrder(t, f.node,
					"cmd mdadm --stop "+node,
					zeroOf(f.leg1),
					f.forced(f.leg2, mdUpdateUuid),
					f.add(f.leg1))
			} else {
				round, resent := resendRound(t, f.srv, held, "named")
				if !strings.Contains(round.GetAgentReply().GetDetails(),
					agent.LeftoverKindMdBroken+":"+node) || resent == nil {
					t.Fatalf("the round once the read answers is %v, "+
						"re-sent %v; want the broken array named and a "+
						"re-send", round.GetAgentReply(), resent != nil)
				}
				assertOrder(t, f.node,
					"cmd mdadm --stop "+node,
					"cmd mdadm --assemble "+f.mdDev)
				assertNoCall(t, f.node, "--zero-superblock")
			}
			if !tracedCall(f.node, "named", "cmd mdadm --stop "+node) {
				t.Fatalf("the stop did not run under the round's trace id")
			}
			if _, reply := f.read(t, 4); reply.GetCode() != 0 {
				t.Fatalf("the read after the re-send: %v, want clean", reply)
			}
		})
	}
}

// TestForcedStartAfterAWalkThatDidNotAnswer is
// TestForcedStartAfterAReadThatDidNotAnswer for the md walk's half of the
// read (cnagent.md CN12): with no array of the group, the build's
// /sys/block listing killed while the sweep's answers leaves the group
// ERROR, nothing zeroed or assembled. The read after it, whose walk answers,
// names the forced start, and the re-send on that round starts the array
// from the named leg. With no mark the read names the absent array instead
// (absentArrayNamed), whose re-send assembles it.
func TestForcedStartAfterAWalkThatDidNotAnswer(t *testing.T) {
	for _, marked := range []bool{true, false} {
		name := "with the mark"
		if !marked {
			name = "with no mark"
		}
		t.Run(name, func(t *testing.T) {
			f := downNoRepair(t)
			// The sweep's listing of /sys/block comes first and answers; the
			// build's walk, the second, is killed. No array of the pool is
			// there, so no listing of an array's md/ comes between them.
			f.node.mu.Lock()
			f.node.killCmdNth["list "+sysfsBlockDir] = 2
			f.node.mu.Unlock()
			held := quietLevel(4)
			if marked {
				held = withMark(held, testDataLeg2)
			}
			info := f.syncup(t, held)
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
				sysfsBlockDir, "grp data")
			assertNoCall(t, f.node, "--zero-superblock")
			assertNoCall(t, f.node, examineOf(f.leg2))
			assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
			f.node.forgetTraces()
			f.node.Reset()
			if marked {
				f.forcedStartResent(t, held, testDataLeg2, "named")
				assertOrder(t, f.node,
					examineOf(f.leg2),
					zeroOf(f.leg1),
					f.forced(f.leg2, mdUpdateUuid),
					f.add(f.leg1))
			} else {
				round, resent := resendRound(t, f.srv, held, "named")
				if !strings.Contains(round.GetAgentReply().GetDetails(),
					f.absentNamed()) || resent == nil {
					t.Fatalf("the round after the walk is %v, re-sent %v; "+
						"want the absent array named and a re-send",
						round.GetAgentReply(), resent != nil)
				}
				assertNoCall(t, f.node, "--zero-superblock")
			}
			if !tracedCall(f.node, "named", "cmd mdadm --assemble "+f.mdDev) {
				t.Fatalf("the assembly did not run under the round's trace " +
					"id")
			}
			if _, reply := f.read(t, 4); reply.GetCode() != 0 {
				t.Fatalf("the read after the re-send: %v, want clean", reply)
			}
			assertNothingRunsBetweenCalls(t, f.node, "named")
		})
	}
}

// assertMarkLeftWaiting checks that a converge acted on no mark: it
// stopped, zeroed and assembled nothing, and did not examine the named leg,
// the one member the forced start examines.
func (f *repFix) assertMarkLeftWaiting(t *testing.T, named string) {
	t.Helper()
	assertNoCall(t, f.node, "cmd mdadm --stop ")
	assertNoCall(t, f.node, "--zero-superblock")
	assertNoCall(t, f.node, examineOf(named))
	assertNoCall(t, f.node, "cmd mdadm --assemble")
}

// TestForcedStartAfterAnArrayReadRunning pins a pass that reads the marked
// group's array running and so runs no forced start (cnagent.md CN12, the
// forced start, step 1), in the shape the e2e met: the open repair's bad
// leg, dead, is the one member md holds in sync, leg 2 is the half-copied
// member md keeps retrying the copy onto, and array_state reads
// write-pending, as md reads a broken array during each superblock write it
// makes to the dead member. The marked converge leaves the mark waiting, its
// group row OK with the dead bad leg in sync, and a read names no forced
// start while it reads the array running (forcedStartNamed), so nothing
// acts on the mark between the worker's calls. The read that reads the array
// broken names the forced start, and the re-send on that round stops the
// array and starts it from the named leg, the repair's new leg taken from a
// retired leg.
func TestForcedStartAfterAnArrayReadRunning(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, quietLevel(3))
	f.copying(f.leg2, "1")
	f.badLegDead()
	f.node.setArrayState(f.mdDev, "write-pending")
	f.node.setSuperblock(f.newLegDev, fakeSuperblock{
		role: "Active device 1", events: 40})
	node := f.node.mdNode(f.mdDev)
	f.node.forgetTraces()
	f.node.Reset()
	logs := captureLogs(t)
	held := withMark(takenBackReq(4,
		pb.RepStage_REP_STAGE_CREATED, false,
		repOpts{level: pb.SpLevel_SP_LEVEL_NO_THINPOOL, noSubsys: true}),
		repNewLeg)
	info := f.syncup(t, held)
	f.assertMarkLeftWaiting(t, f.newLegDev)
	f.assertMdCalls(t, "the converge over the array read running")
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	assertReading(t, info, "the array read running", false, testDataLeg)
	f.assertForcedStartUnnamed(t, 4, "the array read running")

	f.node.setArrayState(f.mdDev, "broken")
	f.node.Reset()
	f.forcedStartResent(t, held, repNewLeg, "named")
	assertOrder(t, f.node,
		"cmd mdadm --stop "+node,
		examineOf(f.newLegDev),
		zeroOf(f.leg2),
		f.forced(f.newLegDev, mdUpdateUuid),
		f.add(f.leg2))
	assertNoCall(t, f.node, zeroOf(f.leg1))
	assertNoCall(t, f.node, f.add(f.leg1))
	assertForcedRecord(t, logs, repNewLeg, mdUpdateUuid, testDataLeg2)
	if !tracedCall(f.node, "named", "cmd mdadm --stop "+node) {
		t.Fatalf("the stop did not run under the round's trace id")
	}
	f.assertForcedStartUnnamed(t, 4, "after the start", "named")
}

// TestAMarkOverARunningArrayIsNamedByNothing pins the mark over an array
// that runs (cnagent.md CN12, CN30): the marked converge of a clean array
// with every leg available reconciles it as with no mark, and a read names
// no forced start while an array of the group runs, so no SyncupCntlr comes
// for the mark and nothing acts on it between the worker's calls; the
// converge of the request without the mark, the sp worker's clear
// (dnv-worker.md AR13), acts on none either. The startup reconcile of the
// stored marked request over the same running array acts on no mark
// (dropForcedStartMarks), and the read names the stored mark (storedMarks),
// whose re-send delivers it; the converge of the delivered mark leaves it
// waiting over the running array, and the read after it names nothing.
func TestAMarkOverARunningArrayIsNamedByNothing(t *testing.T) {
	t.Run("delivered", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		f.node.forgetTraces()
		f.node.Reset()
		held := withMark(quietLevel(4), testDataLeg2)
		info := f.syncup(t, held)
		f.assertMarkLeftWaiting(t, f.leg2)
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		round, resent := resendRound(t, f.srv, held, "quiet")
		if round.GetAgentReply().GetCode() != 0 || resent != nil {
			t.Fatalf("the round over the running array is %v, re-sent %v; "+
				"want a clean verdict and no re-send",
				round.GetAgentReply(), resent != nil)
		}
		f.assertMarkLeftWaiting(t, f.leg2)
		assertNothingRunsBetweenCalls(t, f.node, "quiet")
		f.node.Reset()
		f.syncup(t, quietLevel(5))
		f.assertMarkLeftWaiting(t, f.leg2)
	})
	t.Run("a stored mark", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		req := withMark(quietLevel(4), testDataLeg2)
		path := f.srv.nf.LocalCntlrPath(testCluster, testCn, testSp,
			testCntlr)
		if err := f.srv.store.Save(f.ctx, path, req); err != nil {
			t.Fatalf("saving the marked request: %v", err)
		}
		f.node.Reset()
		fresh := newCnServer(f.node)
		reconcileForTest(t, fresh)
		f.srv = fresh
		f.assertMarkLeftWaiting(t, f.leg2)
		f.node.forgetTraces()
		f.node.Reset()
		round, resent := resendRound(t, f.srv, req, "stored")
		if !strings.Contains(round.GetAgentReply().GetDetails(),
			fmt.Sprintf("forced start of group %#x: %s", testDataGrp,
				errStoredMark)) || resent == nil {
			t.Fatalf("the round after the startup reconcile is %v, "+
				"re-sent %v; want the stored mark named and a re-send",
				round.GetAgentReply(), resent != nil)
		}
		f.assertMarkLeftWaiting(t, f.leg2)
		if _, reply := f.read(t, 4); reply.GetCode() != 0 {
			t.Fatalf("the read after the delivered mark: %v, want clean",
				reply)
		}
		assertNothingRunsBetweenCalls(t, f.node, "stored")
	})
}

// TestForcedStartOverARunningArrayWithAForeignMember pins a pass that reads
// the marked group's array running while it holds a member that is not a dm
// device (cnagent.md CN12, the forced start, step 1): the array is the
// group's error, as without a mark, and nothing is stopped, zeroed or
// assembled. A read names no forced start, whether md reads the array
// running or broken (MdDetail.strangerErr), so nothing acts on the mark
// between the worker's calls for as long as the member stays. Once that
// member is out of the array and md reads it broken, the read names the
// forced start, and the re-send on that round stops the array and starts it
// from the named leg.
func TestForcedStartOverARunningArrayWithAForeignMember(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, levelReq(3, pb.SpLevel_SP_LEVEL_NO_THINPOOL))
	f.node.mu.Lock()
	array := f.node.arrays[f.mdDev]
	members := slices.Clone(array.members)
	name := array.name
	f.node.mu.Unlock()
	f.node.seedArray(f.mdDev, name,
		append(slices.Clone(members), "/dev/sdb1")...)
	f.node.setArrayState(f.mdDev, "write-pending")
	f.node.forgetTraces()
	f.node.Reset()
	held := withMark(levelReq(4, pb.SpLevel_SP_LEVEL_NO_THINPOOL),
		testDataLeg2)
	info := f.syncup(t, held)
	assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
		"holds a foreign member", "grp data")
	f.assertMarkLeftWaiting(t, f.leg2)
	if f.node.arrayGone(f.mdDev) {
		t.Fatalf("the array that holds a member that is not ours is gone")
	}
	f.assertForcedStartUnnamed(t, 4, "the running array")
	f.node.setArrayState(f.mdDev, "broken")
	f.assertForcedStartUnnamed(t, 4, "the broken array")
	f.assertMarkLeftWaiting(t, f.leg2)

	// The operator takes the member out, and md reads the array broken.
	f.node.mu.Lock()
	array = f.node.arrays[f.mdDev]
	array.members = members
	f.node.publishArray(array)
	f.node.mu.Unlock()
	f.node.setArrayState(f.mdDev, "broken")
	node := f.node.mdNode(f.mdDev)
	f.node.Reset()
	f.forcedStartResent(t, held, testDataLeg2, "named")
	assertOrder(t, f.node,
		"cmd mdadm --stop "+node,
		zeroOf(f.leg1),
		f.forced(f.leg2, mdUpdateUuid),
		f.add(f.leg1))
	f.assertForcedStartUnnamed(t, 4, "after the start", "named")
}

// TestForcedStartActsOnlyOnADeliveredMark pins CN12's gate on the request a
// mark comes from: the startup reconcile of a stored request whose group
// carries a mark, with no array of the group, converges the group as with
// no mark — nothing is examined or zeroed and no forced assembly runs, the
// assembly of a group without a mark is tried instead — since the stored
// copy can carry a mark the worker has cleared since. Nothing converges
// that request again between the worker's calls. The read after it names
// the mark in its verdict (storedMarks; cnagent.md CN30), so the worker
// re-sends the SyncupCntlr; and that SyncupCntlr, at the same revision,
// starts the array from the named leg, after which a read names the mark no
// more.
func TestForcedStartActsOnlyOnADeliveredMark(t *testing.T) {
	f := downNoRepair(t)
	req := withMark(quietLevel(4), testDataLeg2)
	path := f.srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr)
	if err := f.srv.store.Save(f.ctx, path, req); err != nil {
		t.Fatalf("saving the marked request: %v", err)
	}
	// md refuses the assembly of a group without a mark, as it does in the
	// case the mark is for.
	plain := "cmd mdadm --assemble " + f.mdDev + " --metadata=1.2 --name " +
		f.arrayName() + " /dev/"
	f.node.mu.Lock()
	f.node.failCmdAlways[plain] = "mdadm: " + f.mdDev + " assembled from " +
		"1 drive - need all 2 to start it (use --run to insist)."
	f.node.mu.Unlock()
	f.node.Reset()
	fresh := newCnServer(f.node)
	reconcileForTest(t, fresh)
	f.srv = fresh
	assertStoredMarkLeftAlone := func(label string) {
		t.Helper()
		if !f.node.hasCall(plain) {
			t.Fatalf("%s: no assembly of the group as with no mark", label)
		}
		assertNoCall(t, f.node, "--zero-superblock")
		assertNoCall(t, f.node, " --run --force")
	}
	assertStoredMarkLeftAlone("the startup reconcile")
	f.node.forgetTraces()

	f.node.Reset()
	round, resent := resendRound(t, f.srv, req, "stored")
	if code := round.GetAgentReply(); code.GetCode() !=
		common.ReplyCodeLeftover || !strings.Contains(code.GetDetails(),
		fmt.Sprintf("forced start of group %#x: %s", testDataGrp,
			errStoredMark)) {
		t.Fatalf("the round's verdict is %v, want the stored mark named",
			code)
	}
	if resent == nil {
		t.Fatalf("the round that names the stored mark brought no re-send")
	}
	assertOrder(t, f.node,
		examineOf(f.leg2),
		zeroOf(f.leg1),
		f.forced(f.leg2, mdUpdateUuid),
		f.add(f.leg1))
	if !tracedCall(f.node, "stored", zeroOf(f.leg1)) {
		t.Fatalf("the zero did not run under the round's trace id")
	}
	assertOk(t, resent.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
		"grp data")
	if _, code := f.read(t, 4); strings.Contains(code.GetDetails(),
		"forced start of group") {
		t.Fatalf("the read after the SyncupCntlr names the mark: %v", code)
	}
	assertNothingRunsBetweenCalls(t, f.node, "stored")
}

// TestStoredMarkNamedOnlyWhereAConvergeBuilds pins the scope of the read's
// naming of a stored mark (storedMarks; cnagent.md CN30): only on a group a
// converge would build that is not deferred. A stored marked request on a
// standby, at SP_LEVEL_NO_REDUND, where no group is built, or whose marked
// group is deferred by a provisioning leg_list leg, gives a read whose
// verdict names no mark, so no SyncupCntlr is asked for; the primary's at a
// level that builds the group is named (TestForcedStartActsOnlyOnADeliveredMark).
func TestStoredMarkNamedOnlyWhereAConvergeBuilds(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts reqOpts
	}{
		{"a standby", reqOpts{raid1: true, twoLegs: true,
			level:  pb.SpLevel_SP_LEVEL_NO_THINPOOL,
			subsys: map[string]*pb.Subsystem{}}},
		{"SP_LEVEL_NO_REDUND", reqOpts{primary: true, raid1: true,
			twoLegs: true, level: pb.SpLevel_SP_LEVEL_NO_REDUND,
			subsys: map[string]*pb.Subsystem{}}},
		{"a deferred group", reqOpts{primary: true, raid1: true,
			twoLegs: true, unprovisionedDataLeg: true,
			level:  pb.SpLevel_SP_LEVEL_NO_THINPOOL,
			subsys: map[string]*pb.Subsystem{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := downNoRepair(t)
			opts := tc.opts
			opts.revision = 4
			req := withMark(cntlrReq(opts), testDataLeg2)
			path := f.srv.nf.LocalCntlrPath(testCluster, testCn, testSp,
				testCntlr)
			if err := f.srv.store.Save(f.ctx, path, req); err != nil {
				t.Fatalf("saving the marked request: %v", err)
			}
			f.node.Reset()
			fresh := newCnServer(f.node)
			reconcileForTest(t, fresh)
			f.srv = fresh
			assertNoCall(t, f.node, "--zero-superblock")
			assertNoCall(t, f.node, " --run --force")
			_, code := f.read(t, 4)
			if strings.Contains(code.GetDetails(), "forced start of group") ||
				code.GetCode() == common.ReplyCodeLeftover {
				t.Fatalf("the read's verdict is %v, want no mark named",
					code)
			}
		})
	}
}

// TestForcedStartRefusesAStartedRepairsBadLeg pins the one leg_list leg the
// mark may not name (isStartedBadLeg; architecture.md, Repair, Forced
// start): the bad leg of a STARTED repair, which left the array before md
// copied onto the new leg and which the finish retires. A mark naming it
// with no array of the group is the group's error, and nothing is examined,
// zeroed or assembled. The bad leg of a CREATED repair may be named
// (TestForcedStartLateMember).
func TestForcedStartRefusesAStartedRepairsBadLeg(t *testing.T) {
	f := downFix(t, repairReq(3, pb.RepStage_REP_STAGE_STARTED, false,
		repOpts{standby: true}))
	info := f.syncup(t, withMark(
		repairReq(4, pb.RepStage_REP_STAGE_STARTED, false,
			repOpts{noSubsys: true}), testDataLeg))
	assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
		"the bad leg of group", "grp data")
	assertNoCall(t, f.node, examineOf(f.leg1))
	assertNoCall(t, f.node, "--zero-superblock")
	assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
}

// TestForcedStartBesideAStartedRepair pins the bad leg's exception on a
// STARTED repair (forceStartGroup; cnagent.md CN12, steps 3 and 4): a mark
// naming leg 2 zeroes the repair's new leg alone and starts the array from
// leg 2, and the bad leg, which the finish retires with its data kept, is
// neither zeroed nor added, keeping its superblock while it serves, and is
// not waited for while it is dead. A new leg the primary that started the
// repair had added carries the array's superblock, half copied, and is
// zeroed; a new leg whose add never landed reads blank, and counts as zeroed
// with no zero run, since a zero named for the 1.2 format fails on a member
// with no superblock (Md.ZeroSuperblock). The record names the new leg
// zeroed either way.
func TestForcedStartBesideAStartedRepair(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dead  bool
		blank bool
	}{
		{"the bad leg serving", false, false},
		{"the bad leg dead", true, false},
		{"the new leg blank", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := downFix(t, repairReq(3, pb.RepStage_REP_STAGE_STARTED, false,
				repOpts{standby: true}))
			if !f.node.hasSuperblock(f.leg1) {
				t.Fatalf("the fixture's bad leg carries no superblock")
			}
			// The standby fixture never adds the new leg.
			if f.node.hasSuperblock(f.newLegDev) {
				t.Fatalf("the fixture's new leg carries a superblock")
			}
			if !tc.blank {
				f.node.setSuperblock(f.newLegDev, fakeSuperblock{
					role: "Active device 0", recovery: "1024 sectors"})
				f.node.sameArrayAs(f.newLegDev, f.leg2)
			}
			if tc.dead {
				f.badLegDead()
			}
			logs := captureLogs(t)
			info := f.syncup(t, withMark(
				repairReq(4, pb.RepStage_REP_STAGE_STARTED, false,
					repOpts{noSubsys: true}), testDataLeg2))
			if tc.blank {
				assertNoCall(t, f.node, "--zero-superblock")
				assertOrder(t, f.node,
					examineOf(f.leg2),
					examineOf(f.newLegDev),
					f.forced(f.leg2, mdUpdateUuid),
					f.add(f.newLegDev))
			} else {
				assertOrder(t, f.node,
					examineOf(f.leg2),
					zeroOf(f.newLegDev),
					f.forced(f.leg2, mdUpdateUuid),
					f.add(f.newLegDev))
			}
			assertForcedRecord(t, logs, testDataLeg2, mdUpdateUuid,
				repNewLeg)
			assertNoCall(t, f.node, zeroOf(f.leg1))
			assertNoCall(t, f.node, zeroOf(f.leg2))
			assertNoCall(t, f.node, f.add(f.leg1))
			assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
			if !f.node.hasSuperblock(f.leg1) {
				t.Fatalf("the forced start took the bad leg's superblock")
			}
		})
	}
}

// TestForcedStartZeroEndsThePass pins the order of the zero and the
// assembly: a zero that fails, or that did not answer, ends the pass for
// the group before any assembly. The read after it names the forced start
// (forcedStartNamed), and the re-send on that round tries again from the
// first step: a member a refused zero left as it was is zeroed again, and a
// member whose killed zero the kernel completed reads blank, which the pass
// counts as zeroed with no zero run, since a zero named for the 1.2 format
// fails on a member with no superblock (Md.ZeroSuperblock); the start goes
// through and its record names the member zeroed either way.
func TestForcedStartZeroEndsThePass(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(f *repFix)
		// reached says the failed zero reached the kernel.
		reached bool
	}{
		{"refused", func(f *repFix) {
			f.node.failCmd[zeroOf(f.leg1)] = "mdadm: Couldn't open " +
				f.leg1 + " for write - not zeroing"
		}, false},
		{"killed", func(f *repFix) {
			f.node.killCmd[zeroOf(f.leg1)] = true
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := downNoRepair(t)
			f.node.mu.Lock()
			tc.arm(f)
			f.node.mu.Unlock()
			held := withMark(quietReq(4), testDataLeg2)
			info := f.syncup(t, held)
			if row := info.GetGrpIdToMdRaid()[testDataGrp]; row.GetStatus() !=
				pb.ResStatus_RES_STATUS_ERROR {
				t.Fatalf("the failed zero's row is %v", row)
			}
			assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
			if f.node.hasSuperblock(f.leg1) == tc.reached {
				t.Fatalf("the failed zero reached the kernel: %v, want %v",
					!f.node.hasSuperblock(f.leg1), tc.reached)
			}
			f.node.forgetTraces()
			f.node.Reset()
			logs := captureLogs(t)
			f.forcedStartResent(t, held, testDataLeg2, "named")
			if tc.reached {
				assertNoCall(t, f.node, "--zero-superblock")
				assertOrder(t, f.node,
					examineOf(f.leg1),
					f.forced(f.leg2, mdUpdateUuid),
					f.add(f.leg1))
			} else {
				assertOrder(t, f.node,
					zeroOf(f.leg1),
					f.forced(f.leg2, mdUpdateUuid),
					f.add(f.leg1))
				if !tracedCall(f.node, "named", zeroOf(f.leg1)) {
					t.Fatalf("the zero did not run under the round's " +
						"trace id")
				}
			}
			assertForcedRecord(t, logs, testDataLeg2, mdUpdateUuid,
				testDataLeg)
			assertOk(t, probedCntlrInfo(t, f.srv).
				GetGrpIdToMdRaid()[testDataGrp], "grp data after the re-send")
			f.assertForcedStartUnnamed(t, 4, "after the start", "named")
		})
	}
}

// TestForcedStartFallsBackToForceNoBbl pins the second form of the forced
// assembly (forcedAssemble). When the kernel refuses the named leg for its
// bad-block log, the first run has already written the leg's new identity,
// so the same pass examines the leg again and assembles with the log
// dropped, the array starting under that identity and the record naming
// the update. A first run that left an inactive array has it stopped
// first; a stop that fails, or an array it left holding a member the sweep
// would not call ours, ends the pass with nothing more assembled. A first
// run that failed before its write leaves the old identity, so no second
// run comes, and the next pass, which the worker's re-send brings once a
// read names the forced start (forcedStartNamed), runs the first form
// again. Both runs failing is the group's error with both outputs; a run
// killed after the kernel started the array, of either form, is taken as
// started. The mark carries the force an older gateway's mark can carry,
// which passes the named leg's list as the override bad-blocks does
// (badBlocksRefusal, forcedStartPasses).
func TestForcedStartFallsBackToForceNoBbl(t *testing.T) {
	// refused scripts the named leg's bad-block log the kernel refuses.
	refused := func(f *repFix) {
		f.node.mu.Lock()
		f.node.bblRefused[f.leg2] = true
		f.node.mu.Unlock()
	}
	// leaves makes the refused run leave an inactive array holding the
	// named leg and also.
	leaves := func(f *repFix, also ...string) {
		f.node.mu.Lock()
		f.node.refusedLeaves = true
		f.node.refusedLeavesAlso = also
		f.node.mu.Unlock()
	}
	t.Run("the second form starts it", func(t *testing.T) {
		f := downNoRepair(t)
		oldUuid := f.legUuid(f.leg2)
		refused(f)
		logs := captureLogs(t)
		info := f.syncup(t, withForcedMark(noRepairReq(4), testDataLeg2))
		assertOrder(t, f.node,
			examineOf(f.leg2),
			zeroOf(f.leg1),
			f.forced(f.leg2, mdUpdateUuid),
			examineOf(f.leg2),
			f.forced(f.leg2, mdUpdateForceNoBbl),
			f.add(f.leg1))
		assertNoCall(t, f.node, "cmd mdadm --stop ")
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		assertForcedRecord(t, logs, testDataLeg2, mdUpdateUuid+","+mdUpdateForceNoBbl,
			testDataLeg)
		f.assertNewIdentity(t, oldUuid)
	})
	t.Run("the refused run leaves an inactive array", func(t *testing.T) {
		f := downNoRepair(t)
		refused(f)
		leaves(f)
		info := f.syncup(t, withForcedMark(noRepairReq(4), testDataLeg2))
		stops := f.node.callsMatching("cmd mdadm --stop ")
		if len(stops) != 1 {
			t.Fatalf("stops %v, want the one of what the refused run left",
				stops)
		}
		assertOrder(t, f.node,
			f.forced(f.leg2, mdUpdateUuid),
			stops[0],
			examineOf(f.leg2),
			f.forced(f.leg2, mdUpdateForceNoBbl),
			f.add(f.leg1))
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	})
	t.Run("the stop of what it left fails", func(t *testing.T) {
		f := downNoRepair(t)
		refused(f)
		leaves(f)
		f.node.mu.Lock()
		f.node.failCmd["cmd mdadm --stop "] =
			"mdadm: Cannot get exclusive access"
		f.node.mu.Unlock()
		logs := captureLogs(t)
		info := f.syncup(t, withForcedMark(noRepairReq(4), testDataLeg2))
		row := info.GetGrpIdToMdRaid()[testDataGrp]
		// The row keeps the head of the run's error and the stop's at its
		// end; the run's update, in the middle of its command line, the
		// row text cap takes out, so the calls show it.
		assertErrorDetails(t, row, "mdadm --assemble ", "grp data")
		assertErrorDetails(t, row, "Cannot get exclusive access", "grp data")
		f.assertRanOnce(t, "--update="+mdUpdateUuid)
		assertNoCall(t, f.node, "--update="+mdUpdateForceNoBbl)
		assertNoCall(t, f.node, f.add(f.leg1))
		if f.node.arrayGone(f.mdDev) {
			t.Fatalf("the array the refused run left is gone")
		}
		if n := len(logs.msgRecords(t, msgForcedStart)); n != 0 {
			t.Fatalf("%d forced start records for a start that failed", n)
		}
	})
	t.Run("what it left is foreign", func(t *testing.T) {
		f := downNoRepair(t)
		refused(f)
		leaves(f, "/dev/sdb1")
		info := f.syncup(t, withForcedMark(noRepairReq(4), testDataLeg2))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"holds a foreign member", "grp data")
		assertNoCall(t, f.node, "cmd mdadm --stop ")
		assertNoCall(t, f.node, "--update="+mdUpdateForceNoBbl)
		assertNoCall(t, f.node, f.add(f.leg1))
	})
	t.Run("what it left holds a dn agent's device", func(t *testing.T) {
		f := downNoRepair(t)
		refused(f)
		leaves(f, "/dev/mapper/"+names(f.srv).DnLinearName(
			testCluster, 0xd1, testSp, 0x51, testCn))
		info := f.syncup(t, withForcedMark(noRepairReq(4), testDataLeg2))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"which is no leg of this sp", "grp data")
		assertNoCall(t, f.node, "cmd mdadm --stop ")
		assertNoCall(t, f.node, "--update="+mdUpdateForceNoBbl)
		assertNoCall(t, f.node, f.add(f.leg1))
	})
	t.Run("the first run failed before its write", func(t *testing.T) {
		f := downNoRepair(t)
		oldUuid := f.legUuid(f.leg2)
		f.node.mu.Lock()
		f.node.failCmd["--update="+mdUpdateUuid] =
			"mdadm: cannot open " + f.leg2 + ": Device or resource busy"
		f.node.mu.Unlock()
		logs := captureLogs(t)
		info := f.syncup(t, withForcedMark(quietReq(4), testDataLeg2))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"Device or resource busy", "grp data")
		assertOrder(t, f.node,
			f.forced(f.leg2, mdUpdateUuid),
			examineOf(f.leg2))
		assertNoCall(t, f.node, "--update="+mdUpdateForceNoBbl)
		if got := f.legUuid(f.leg2); got != oldUuid {
			t.Fatalf("the failed run changed the leg's identity to %q", got)
		}
		if n := len(logs.msgRecords(t, msgForcedStart)); n != 0 {
			t.Fatalf("%d forced start records for a start that failed", n)
		}
		f.node.Reset()
		f.forcedStartResent(t, withForcedMark(quietReq(4), testDataLeg2),
			testDataLeg2, "named")
		// The first pass zeroed leg 1, which reads blank now and counts as
		// zeroed with no zero run.
		assertNoCall(t, f.node, "--zero-superblock")
		assertOrder(t, f.node,
			f.forced(f.leg2, mdUpdateUuid),
			f.add(f.leg1))
		assertNoCall(t, f.node, "--update="+mdUpdateForceNoBbl)
		assertOk(t, probedCntlrInfo(t, f.srv).GetGrpIdToMdRaid()[testDataGrp],
			"grp data after the re-send")
		f.assertNewIdentity(t, oldUuid)
	})
	t.Run("both fail", func(t *testing.T) {
		f := downNoRepair(t)
		refused(f)
		f.node.mu.Lock()
		f.node.failCmd["--update="+mdUpdateForceNoBbl] =
			"mdadm: Not enough devices to start the array."
		f.node.mu.Unlock()
		logs := captureLogs(t)
		info := f.syncup(t, withForcedMark(noRepairReq(4), testDataLeg2))
		row := info.GetGrpIdToMdRaid()[testDataGrp]
		// The row keeps the head of the first run's error and the end of
		// the second's; each run's update, in the middle of its command
		// line, the row text cap takes out, so the calls show them.
		assertErrorDetails(t, row, "mdadm --assemble ", "grp data")
		assertErrorDetails(t, row, "Not enough devices to start the array",
			"grp data")
		f.assertRanOnce(t, "--update="+mdUpdateUuid)
		f.assertRanOnce(t, "--update="+mdUpdateForceNoBbl)
		assertNoCall(t, f.node, f.add(f.leg1))
		if n := len(logs.msgRecords(t, msgForcedStart)); n != 0 {
			t.Fatalf("%d forced start records for a start that failed", n)
		}
	})
	t.Run("the first run killed after the start", func(t *testing.T) {
		f := downNoRepair(t)
		f.node.mu.Lock()
		f.node.killCmd["--update="+mdUpdateUuid] = true
		f.node.mu.Unlock()
		logs := captureLogs(t)
		info := f.syncup(t, withForcedMark(noRepairReq(4), testDataLeg2))
		assertNoCall(t, f.node, "--update="+mdUpdateForceNoBbl)
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		assertForcedRecord(t, logs, testDataLeg2, mdUpdateUuid, testDataLeg)
	})
	t.Run("the second run killed after the start", func(t *testing.T) {
		f := downNoRepair(t)
		oldUuid := f.legUuid(f.leg2)
		refused(f)
		f.node.mu.Lock()
		f.node.killCmd["--update="+mdUpdateForceNoBbl] = true
		f.node.mu.Unlock()
		logs := captureLogs(t)
		info := f.syncup(t, withForcedMark(noRepairReq(4), testDataLeg2))
		assertOrder(t, f.node,
			f.forced(f.leg2, mdUpdateForceNoBbl),
			f.add(f.leg1))
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		assertForcedRecord(t, logs, testDataLeg2, mdUpdateUuid+","+mdUpdateForceNoBbl,
			testDataLeg)
		f.assertNewIdentity(t, oldUuid)
	})
}

// TestForcedStartLeavesAnAbortingRepairsNewLeg pins the forced start's kept
// set: a new leg of a CREATED repair whose abort flag the request carries is
// a member the group keeps (memberNames) but none the forced start keeps
// unless the mark names it (forcedStartLegs): a mark naming leg 2 zeroes
// neither it nor the repair's bad leg, and a mark naming it starts the array
// from it, zeroing leg 2 and leaving the bad leg as it is, since after a
// forced start from it that did not start the array it can be the group's
// one full member.
func TestForcedStartLeavesAnAbortingRepairsNewLeg(t *testing.T) {
	t.Run("named", func(t *testing.T) {
		f := downFix(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, true,
			repOpts{standby: true}))
		f.badLegDead()
		f.node.setSuperblock(f.newLegDev, fakeSuperblock{events: 9})
		info := f.syncup(t, withMark(
			repairReq(4, pb.RepStage_REP_STAGE_CREATED, true), repNewLeg))
		assertOrder(t, f.node,
			examineOf(f.newLegDev),
			examineOf(f.leg2),
			zeroOf(f.leg2),
			f.forced(f.newLegDev, mdUpdateUuid),
			f.add(f.leg2))
		assertNoCall(t, f.node, zeroOf(f.leg1))
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	})
	t.Run("not zeroed", func(t *testing.T) {
		f := downFix(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, true,
			repOpts{standby: true}))
		f.node.setSuperblock(f.newLegDev, fakeSuperblock{events: 9})
		info := f.syncup(t, withMark(
			repairReq(4, pb.RepStage_REP_STAGE_CREATED, true), testDataLeg2))
		assertNoCall(t, f.node, "--zero-superblock")
		if !f.node.hasCall(f.forced(f.leg2, mdUpdateUuid)) {
			t.Fatalf("no forced assembly from leg 2")
		}
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		if !f.node.hasSuperblock(f.newLegDev) {
			t.Fatalf("the aborting repair's new leg lost its superblock")
		}
	})
}

// TestForcedStartKeepsAStartedRepairsNewLeg pins the other half of the
// forced start's kept set: a STARTED repair's new leg is a member the forced
// start keeps whatever its abort flag says, since a STARTED repair is never
// aborted and its finish makes that leg a leg_list leg. In the question-2
// case — the bad leg dead, leg 2 the in-sync leg and the new leg, taken from
// a retired leg, half-copied under the old array's far newer events count —
// a mark naming leg 2 zeroes the new leg, so the start's add of it is a fresh
// add with nothing to refuse, the verdict says started and carries no
// refusal, on the converge and on the read.
func TestForcedStartKeepsAStartedRepairsNewLeg(t *testing.T) {
	for _, tc := range []struct {
		name  string
		abort bool
	}{
		{"the abort flag clear", false},
		{"the abort flag set", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := downFix(t, takenBackReq(3, pb.RepStage_REP_STAGE_STARTED,
				tc.abort, repOpts{standby: true}))
			f.badLegDead()
			f.node.setSuperblock(f.leg2, fakeSuperblock{
				role: "Active device 1", events: 10})
			f.node.setSuperblock(f.newLegDev, fakeSuperblock{
				role: "Active device 0", recovery: "1024 sectors",
				events: 50})
			req := withMark(takenBackReq(4, pb.RepStage_REP_STAGE_STARTED,
				tc.abort, repOpts{noSubsys: true}), testDataLeg2)
			info := f.syncup(t, req)
			assertOrder(t, f.node,
				examineOf(f.leg2),
				zeroOf(f.newLegDev),
				f.forced(f.leg2, mdUpdateUuid),
				f.add(f.newLegDev))
			assertNoCall(t, f.node, zeroOf(f.leg1))
			assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
			assertVerdict(t, info, "the started repair", true, false)
			if got := verdict(info).GetRefusal(); got != "" {
				t.Fatalf("the converge's refusal is %q", got)
			}
			probeLegs(t, f.srv, repNewLeg)
			read, _ := f.read(t, 4)
			if got := verdict(read).GetRefusal(); got != "" {
				t.Fatalf("the read's refusal is %q", got)
			}
		})
	}
}

// TestForcedStartLevelBuildsNoGroup pins CN19 over the mark: at
// SP_LEVEL_NO_REDUND no group array is built, so a mark does nothing there.
func TestForcedStartLevelBuildsNoGroup(t *testing.T) {
	f := downNoRepair(t)
	info := f.syncup(t, withMark(
		levelReq(4, pb.SpLevel_SP_LEVEL_NO_REDUND), testDataLeg2))
	assertNoCall(t, f.node, "--zero-superblock")
	assertNoCall(t, f.node, "cmd mdadm --assemble")
	assertNoCall(t, f.node, "readsb ")
	assertMissingSpLevel(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
}

// TestForcedStartLateMember pins the forced start's wait for a member it
// keeps (cnagent.md CN12, CN30): while the mark stands and no array runs, a
// repair's new leg that does not serve, whether it is the named leg or a
// member to zero, leaves the group's error naming the wait, and a wait
// zeroes nothing, even a member that comes before the missing one. A read
// names no forced start while that leg does not serve (forcedStartNamed),
// and nothing runs between the worker's calls; once the leg's path is live
// again the read names the forced start, and the re-send on that round
// starts the array from the named leg. A provisioning new leg it waits for
// is named by nothing either: its provisioned flip raises the pool's
// revision (dnv-worker.md RW18), and the converge of that revision starts
// the array. With the array running a read names no forced start, whatever
// the new leg's state.
func TestForcedStartLateMember(t *testing.T) {
	for _, tc := range []struct {
		name  string
		named uint64
		dev   func(f *repFix) string
	}{
		{"a member to zero", testDataLeg2,
			func(f *repFix) string { return f.leg2 }},
		{"the named leg", repNewLeg,
			func(f *repFix) string { return f.newLegDev }},
		// The bad leg named: leg 2, available, comes before the dead new leg
		// among the members to zero, and is zeroed no more than the new leg
		// is, since every member is checked before the first zero.
		{"a member to zero after one that is there", testDataLeg,
			func(f *repFix) string { return f.leg1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := downFix(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false,
				repOpts{standby: true}))
			// A full member's superblock on the new leg, so that once it
			// serves the forced start can name it.
			f.node.setSuperblock(f.newLegDev, fakeSuperblock{
				role: "Active device 1", events: 40})
			f.legDead(repNewLeg, testIp, testSvcId)
			f.node.forgetTraces()
			held := withMark(repairReq(4, pb.RepStage_REP_STAGE_CREATED,
				false, repOpts{noSubsys: true}), tc.named)
			info := f.syncup(t, held)
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
				"forced start waits for leg 0x7b", "grp data")
			assertNoCall(t, f.node, "--zero-superblock")
			f.assertForcedStartUnnamed(t, 4, "the member that does not serve")
			assertNoCall(t, f.node, "--zero-superblock")

			f.node.setCtrlState(legNqn(f.srv, repNewLeg), testIp, testSvcId,
				"live")
			f.node.Reset()
			f.forcedStartResent(t, held, tc.named, "named")
			if !tracedCall(f.node, "named",
				f.forced(tc.dev(f), mdUpdateUuid)) {
				t.Fatalf("no forced start from the named leg under the "+
					"round's trace id:\n%s",
					strings.Join(f.node.Traced("named"), "\n"))
			}
			f.assertForcedStartUnnamed(t, 4, "after the start", "named")
		})
	}
	t.Run("a provisioning new leg", func(t *testing.T) {
		f := downFix(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false,
			repOpts{standby: true, unprovisioned: true}))
		f.node.forgetTraces()
		info := f.syncup(t, withMark(
			repairReq(4, pb.RepStage_REP_STAGE_CREATED, false,
				repOpts{unprovisioned: true, noSubsys: true}), testDataLeg2))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"forced start waits for leg 0x7b", "grp data")
		assertNoCall(t, f.node, "--zero-superblock")
		f.assertForcedStartUnnamed(t, 4, "the provisioning new leg")

		// The new leg's provisioned flip raises the pool's revision.
		f.node.Reset()
		f.syncup(t, withMark(repairReq(5, pb.RepStage_REP_STAGE_CREATED,
			false, repOpts{noSubsys: true}), testDataLeg2))
		if !f.node.hasCall(f.forced(f.leg2, mdUpdateUuid)) {
			t.Fatalf("the converge of the provisioned flip did not start "+
				"the array:\n%s", strings.Join(f.node.Calls(), "\n"))
		}
	})
	t.Run("running", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
		f.legDead(repNewLeg, testIp, testSvcId)
		f.node.forgetTraces()
		f.node.Reset()
		f.syncup(t, withMark(repairReq(4, pb.RepStage_REP_STAGE_CREATED,
			false, repOpts{noSubsys: true}), testDataLeg2))
		f.assertMarkLeftWaiting(t, f.leg2)
		f.assertForcedStartUnnamed(t, 4, "the running array")
		f.node.setCtrlState(legNqn(f.srv, repNewLeg), testIp, testSvcId,
			"live")
		f.assertForcedStartUnnamed(t, 4, "the running array, new leg back")
	})
}

// TestTakeBackCheck pins CN12's take-back check on the add of a new leg
// taken from a retired leg, in TestRepairAddsTheNewLeg's "bad leg does not
// serve" shape: a new leg whose events count is above that of the member
// that keeps the data is refused — no add, no fail or remove of the bad
// leg, the group's row OK, the refusal in the verdict — on the converge
// and, the same text, on the read, whose verdict names nothing; a count not
// above, a new leg with no superblock, and a fresh new leg go on as before,
// the last examined not at all. A STARTED repair's taken-back leg is
// checked the same way, refused when newer and added when not, and one that
// carries a superblock while no member keeps the data waits with no
// refusal.
func TestTakeBackCheck(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage pb.RepStage
		// fresh gives the request a fresh new leg, not taken back.
		fresh bool
		// probed are the legs whose probers complete a round first.
		probed []uint64
		// newSb is the new leg's superblock, nil for none.
		newSb  *fakeSuperblock
		refuse bool
		// want builds the mdadm commands against the array.
		want func(f *repFix) []string
		// examined are the members whose superblocks the converge reads.
		examined func(f *repFix) []string
	}{
		{"newer", pb.RepStage_REP_STAGE_CREATED, false, allLegs,
			&fakeSuperblock{role: "Active device 1", events: 50}, true, nil,
			func(f *repFix) []string { return []string{f.newLegDev, f.leg2} }},
		{"not newer", pb.RepStage_REP_STAGE_CREATED, false, allLegs,
			&fakeSuperblock{role: "Active device 1", events: 10}, false,
			func(f *repFix) []string {
				return []string{f.fail(f.leg1), f.remove(f.leg1),
					f.add(f.newLegDev)}
			},
			func(f *repFix) []string { return []string{f.newLegDev, f.leg2} }},
		{"no superblock", pb.RepStage_REP_STAGE_CREATED, false, allLegs,
			nil, false,
			func(f *repFix) []string {
				return []string{f.fail(f.leg1), f.remove(f.leg1),
					f.add(f.newLegDev)}
			},
			func(f *repFix) []string { return []string{f.newLegDev} }},
		{"fresh", pb.RepStage_REP_STAGE_CREATED, true, allLegs,
			&fakeSuperblock{role: "Active device 1", events: 50}, false,
			func(f *repFix) []string {
				return []string{f.fail(f.leg1), f.remove(f.leg1),
					f.add(f.newLegDev)}
			},
			func(f *repFix) []string { return nil }},
		{"started, nothing to compare with", pb.RepStage_REP_STAGE_STARTED,
			false, []uint64{testMetaLeg, testDataLeg},
			&fakeSuperblock{role: "Active device 1", events: 5}, false, nil,
			func(f *repFix) []string { return []string{f.newLegDev} }},
		{"started, not newer", pb.RepStage_REP_STAGE_STARTED, false, allLegs,
			&fakeSuperblock{role: "Active device 1", events: 5}, false,
			func(f *repFix) []string { return []string{f.add(f.newLegDev)} },
			func(f *repFix) []string { return []string{f.newLegDev, f.leg2} }},
		// The check runs on a STARTED repair's add as on a CREATED one's
		// (cnagent.md CN12: either add).
		{"started, newer", pb.RepStage_REP_STAGE_STARTED, false, allLegs,
			&fakeSuperblock{role: "Active device 1", events: 50}, true, nil,
			func(f *repFix) []string { return []string{f.newLegDev, f.leg2} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepFix(t, tc.probed...)
			f.badLegDead()
			f.node.setSuperblock(f.leg2, fakeSuperblock{
				role: "Active device 1", events: 10})
			if tc.newSb != nil {
				f.node.setSuperblock(f.newLegDev, *tc.newSb)
				f.node.sameArrayAs(f.newLegDev, f.leg2)
			}
			req := func(revision uint64) *pb.SyncupCntlrRequest {
				if tc.fresh {
					return repairReq(revision, tc.stage, false)
				}
				return takenBackReq(revision, tc.stage, false)
			}
			before := f.members()
			f.node.Reset()
			info := f.syncup(t, req(3))
			var want []string
			if tc.want != nil {
				want = tc.want(f)
			}
			f.assertMdCalls(t, "the converge", want...)
			if want == nil {
				if after := f.members(); !slices.Equal(after, before) {
					t.Fatalf("the array's members changed to %v, want %v",
						after, before)
				}
			}
			examined := examinedMembers(f.node)
			if wantExamined := tc.examined(f); !slices.Equal(examined,
				wantExamined) {
				t.Fatalf("the converge examined %v, want %v", examined,
					wantExamined)
			}
			assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
			refusal := verdict(info).GetRefusal()
			if tc.refuse != strings.Contains(refusal,
				"taken from a retired leg holds newer data") {
				t.Fatalf("the converge's refusal is %q, want refused %v",
					refusal, tc.refuse)
			}
			if !tc.refuse {
				return
			}
			if !strings.HasPrefix(refusal, "leg 0x7b ") ||
				!strings.HasSuffix(refusal, fmt.Sprintf(
					"its md events count 50 is above that of leg %#x",
					testDataLeg2)) {
				t.Fatalf("the refusal is %q", refusal)
			}
			assertVerdict(t, info, "the refused repair", false, false)
			// The read, once the new leg's row reads OK, reports the same
			// refusal and names nothing, so no syncup comes for it.
			probeLegs(t, f.srv, repNewLeg)
			read, reply := f.read(t, 3)
			if reply.GetCode() != 0 {
				t.Fatalf("the read of the refused repair: %v, want clean",
					reply)
			}
			assertOk(t, read.GetGrpIdToMdRaid()[testDataGrp], "grp data read")
			if got := verdict(read).GetRefusal(); got != refusal {
				t.Fatalf("the read's refusal is %q, the converge's %q", got,
					refusal)
			}
			assertVerdict(t, read, "the refused repair, read", false, false)
		})
	}
}

// TestTakeBackCheckRefusalBesideAChange pins the take-back refusal of a
// converge whose member reconciliation also changed the array, so that the
// converge reads the array again before it reports (ensureGroup): a STARTED
// repair's bad leg that md has failed out of its slot is removed
// (startedBadLegs), and its new leg, taken from a retired leg with a newer
// md events count, is refused; the converge's verdict carries the refusal,
// and the read's, once the new leg's row reads OK, the same one (cnagent.md
// CN12, CN28).
func TestTakeBackCheckRefusalBesideAChange(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.badLegDead()
	f.node.setSuperblock(f.leg2, fakeSuperblock{
		role: "Active device 1", events: 10})
	f.node.setSuperblock(f.newLegDev, fakeSuperblock{
		role: "Active device 1", events: 50})
	f.node.sameArrayAs(f.newLegDev, f.leg2)
	f.node.setMemberState(f.mdDev, f.leg1, "faulty")
	f.node.setMemberSlot(f.mdDev, f.leg1, mdSlotNone)
	f.node.Reset()
	info := f.syncup(t, takenBackReq(3, pb.RepStage_REP_STAGE_STARTED, false))
	f.assertMdCalls(t, "the converge", f.remove(f.leg1))
	if got, want := f.members(), []string{f.leg2}; !slices.Equal(got, want) {
		t.Fatalf("the array's members are %v, want %v", got, want)
	}
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	refusal := verdict(info).GetRefusal()
	if !strings.HasPrefix(refusal, "leg 0x7b taken from a retired leg "+
		"holds newer data") || !strings.HasSuffix(refusal, fmt.Sprintf(
		"its md events count 50 is above that of leg %#x", testDataLeg2)) {
		t.Fatalf("the converge's refusal is %q", refusal)
	}
	assertVerdict(t, info, "the refused repair", false, false)
	probeLegs(t, f.srv, repNewLeg)
	read, reply := f.read(t, 3)
	if reply.GetCode() != 0 {
		t.Fatalf("the read of the refused repair: %v, want clean", reply)
	}
	if got := verdict(read).GetRefusal(); got != refusal {
		t.Fatalf("the read's refusal is %q, the converge's %q", got, refusal)
	}
	assertVerdict(t, read, "the refused repair, read", false, false)
}

// TestTakeBackCheckExamineKilled pins the take-back check's examine that
// did not answer, of the new leg or of the member that keeps the data: on
// the converge it is the group's error and the bad leg stays; on the read it
// names nothing and reports no refusal.
func TestTakeBackCheckExamineKilled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		killed func(f *repFix) string
	}{
		{"the new leg's", func(f *repFix) string { return f.newLegDev }},
		{"the keeper's", func(f *repFix) string { return f.leg2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			f.badLegDead()
			f.node.setSuperblock(f.leg2, fakeSuperblock{
				role: "Active device 1", events: 10})
			f.node.setSuperblock(f.newLegDev, fakeSuperblock{events: 50})
			f.node.mu.Lock()
			f.node.killCmdAlways[examineOf(tc.killed(f))] = true
			f.node.mu.Unlock()
			f.node.Reset()
			info := f.syncup(t,
				takenBackReq(3, pb.RepStage_REP_STAGE_CREATED, false))
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
				"not read", "grp data")
			f.assertMdCalls(t, "the converge")
			probeLegs(t, f.srv, repNewLeg)
			read, reply := f.read(t, 3)
			if reply.GetCode() != 0 {
				t.Fatalf("the read with the examine killed: %v, want clean",
					reply)
			}
			if got := verdict(read).GetRefusal(); got != "" {
				t.Fatalf("the read's refusal is %q, want none", got)
			}
			assertVerdict(t, read, "the repair, read", false, false)
		})
	}
}

// TestTakeBackCheckKeeperWithoutSuperblock pins the take-back check's wait
// when the reading of the member that keeps the data finds no superblock:
// with nothing to compare the newer new leg with, the converge adds nothing,
// takes no bad leg out and reports no refusal, the group's row OK; the read
// names nothing and reports no refusal; both say why the repair waits.
func TestTakeBackCheckKeeperWithoutSuperblock(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.badLegDead()
	f.node.setSuperblock(f.newLegDev, fakeSuperblock{
		role: "Active device 1", events: 50})
	f.node.mu.Lock()
	delete(f.node.superblocks, f.leg2)
	f.node.mu.Unlock()
	before := f.members()
	f.node.Reset()
	info := f.syncup(t, takenBackReq(3, pb.RepStage_REP_STAGE_CREATED, false))
	f.assertMdCalls(t, "the converge")
	if after := f.members(); !slices.Equal(after, before) {
		t.Fatalf("the array's members changed to %v, want %v", after, before)
	}
	examined := examinedMembers(f.node)
	if want := []string{f.newLegDev, f.leg2}; !slices.Equal(examined, want) {
		t.Fatalf("the converge examined %v, want %v", examined, want)
	}
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	if got := verdict(info).GetRefusal(); got != "" {
		t.Fatalf("the converge's refusal is %q, want none", got)
	}
	assertVerdict(t, info, "the waiting repair", false, false)
	waits := fmt.Sprintf(waitsTakeBackKeeper, repNewLeg)
	wantWaits(t, info, "the waiting repair", waits)
	probeLegs(t, f.srv, repNewLeg)
	read, reply := f.read(t, 3)
	if reply.GetCode() != 0 {
		t.Fatalf("the read of the waiting repair: %v, want clean", reply)
	}
	if got := verdict(read).GetRefusal(); got != "" {
		t.Fatalf("the read's refusal is %q, want none", got)
	}
	assertVerdict(t, read, "the waiting repair, read", false, false)
	wantWaits(t, read, "the waiting repair, read", waits)
}

// TestReadMemberFromTheFake pins the reading of a member (Md.ReadMember)
// against the fake's rendering of md's layout: a device with no superblock
// reads none, a killed read and a read that fails are errors, never "no
// superblock", and the role, the copy position, the change count, the
// identity and the bad-block list read back as the test stamped them. The
// parse of areas md itself wrote is TestParseMdSuperblockLabAreas's.
func TestReadMemberFromTheFake(t *testing.T) {
	ctx := context.Background()
	const member = "/dev/mapper/dnv-leg-a"
	node := newFakeNode()
	node.devNo[member] = node.newDevNo()
	md := NewMd(node.osClient())
	if got := md.ReadMember(ctx, member); got.Err != nil || got.Sb != nil {
		t.Fatalf("a device without a superblock read as %+v", got)
	}
	node.setSuperblock(member, fakeSuperblock{
		role: "Active device 1", recovery: "45056 sectors", events: 4321,
		badBlocks: 2})
	got := md.ReadMember(ctx, member)
	if got.Err != nil || got.Sb == nil || got.Sb.Role != 1 ||
		!got.Sb.Recovering || got.Sb.RecoveryOffset != 45056 ||
		got.Sb.Events != 4321 || got.Sb.BadBlocks != 2 ||
		got.Sb.Uuid != fakeUuidText(fakeStampedUuid) {
		t.Fatalf("the reading is %+v, %v", got.Sb, got.Err)
	}
	if gap := got.Sb.notFullMember(); gap == "" {
		t.Fatalf("a half-copied member read as full")
	}
	node.setSuperblock(member, fakeSuperblock{
		role: "Active device 0", events: 12})
	if got = md.ReadMember(ctx, member); got.Err != nil ||
		got.Sb.notFullMember() != "" {
		t.Fatalf("a full member read as %+v, %v", got.Sb, got.Err)
	}
	node.failCmdAlways["readsb "+member] = ""
	if got = md.ReadMember(ctx, member); got.Err == nil ||
		!errors.Is(got.Err, errNotRead) {
		t.Fatalf("a read that fails read as %+v", got)
	}
	delete(node.failCmdAlways, "readsb "+member)
	node.killCmdAlways["readsb "+member] = true
	if got = md.ReadMember(ctx, member); got.Err == nil ||
		!errors.Is(got.Err, errNotRead) {
		t.Fatalf("a killed read read as %+v", got)
	}
}
