package cnagent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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
// subsystem, on the primary: no namespace waits for the pool and registers
// its own retry (CN16), so a test reads the late-member retry alone.
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

// forced is the forced assembly of the data group's array from member with
// one superblock update.
func (f *repFix) forced(member, update string) string {
	return "cmd mdadm --assemble " + f.mdDev + " --name " + f.arrayName() +
		" --run --force --update=" + update + " " + member
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
	return "cmd mdadm --zero-superblock " + member
}

// examineOf is the human-readable `mdadm --examine` of member, which the
// assembly's `--examine --export` never matches.
func examineOf(member string) string {
	return "cmd mdadm --examine " + member
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
	assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev+" --name "+
		f.arrayName()+" /dev/")
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
// examine that did not answer each leave the group ERROR and zero nothing,
// and each registers the retry, as every error of the forced start but a
// wait does (forcedStartRetries): an examine killed at the soft timeout
// passes, and a read of the absent array brings no converge.
func TestForcedStartRefusesANamedLegThatIsNotFull(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(f *repFix)
		want string
	}{
		{"half-copied", func(f *repFix) {
			f.node.setSuperblock(f.leg2, fakeSuperblock{
				role: "Active device 1", recovery: "45056 sectors"})
		}, "not a full member: recovery offset 45056 sectors"},
		{"spare", func(f *repFix) {
			f.node.setSuperblock(f.leg2, fakeSuperblock{role: "spare"})
		}, "not a full member: device role spare"},
		{"no superblock", func(f *repFix) {
			f.node.mu.Lock()
			delete(f.node.superblocks, f.leg2)
			f.node.mu.Unlock()
		}, "not a full member: no md superblock"},
		{"examine killed", func(f *repFix) {
			f.node.mu.Lock()
			f.node.killCmdAlways[examineOf(f.leg2)] = true
			f.node.mu.Unlock()
		}, "mdadm --examine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := downNoRepair(t)
			tc.set(f)
			info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
				tc.want, "grp data")
			assertNoCall(t, f.node, "--zero-superblock")
			assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
			if !retrying(t, f.srv) {
				t.Fatalf("the refused forced start registered no retry")
			}
		})
	}
}

// TestForcedStartStopsAnArrayThatDoesNotRun pins the forced start's stop:
// a broken array of the group is stopped before anything is zeroed, at a
// level where no pool holds it, and the start goes on in the same pass; at
// a level that builds thin pools the slice's concat holds it, the stop
// fails, and the pass ends with nothing zeroed or assembled, the retry it
// registers trying the stop again for as long as the concat holds. A running
// array is never stopped, nor one that holds a member the sweep would not
// call ours — no dm device, a dn agent's device, another sp's leg — which is
// the group's error (cnagent.md CN21). A stop whose verify meets the EBUSY
// array_state of an array md has marked deleted ends the pass with the
// group's error and registers the retry, whose converge finds the array
// gone and starts it.
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
		if !retrying(t, f.srv) {
			t.Fatalf("the stop the concat refused registered no retry")
		}
		f.node.Reset()
		retryAttempt(t, f.srv)
		if !f.node.hasCall("cmd mdadm --stop ") {
			t.Fatalf("the retry's converge did not stop the array again")
		}
		assertNoCall(t, f.node, "--zero-superblock")
		if !retrying(t, f.srv) {
			t.Fatalf("the retry's converge the concat refused again left " +
				"the retry")
		}
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
		info := f.syncup(t, withMark(quietLevel(4), testDataLeg2))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"array "+node+" stop not verified", "grp data")
		assertNoCall(t, f.node, "--zero-superblock")
		assertNoCall(t, f.node, "cmd mdadm --assemble")
		if !retrying(t, f.srv) {
			t.Fatalf("the stop that did not verify registered no retry")
		}
		f.node.mu.Lock()
		f.node.stopDeleting = false
		f.node.mu.Unlock()
		f.node.Reset()
		retryAttempt(t, f.srv)
		assertOrder(t, f.node,
			zeroOf(f.leg1),
			f.forced(f.leg2, mdUpdateUuid),
			f.add(f.leg1))
		assertNoCall(t, f.node, "cmd mdadm --stop ")
		if retrying(t, f.srv) {
			t.Fatalf("the retry's converge that started the array left " +
				"the retry")
		}
		assertOk(t, probedCntlrInfo(t, f.srv).GetGrpIdToMdRaid()[testDataGrp],
			"grp data after the retry")
	})
}

// TestForcedStartCountsAStopTheKernelCompleted pins the forced start's
// verify of its stop (stopGroupArray): a stop of the broken array killed at
// the soft timeout after the kernel completed it is read gone from sysfs, as
// the sweep reads one (Md.Gone), so the same pass goes on to the zero, the
// forced assembly and the add, the group's row OK and no retry registered.
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
	if retrying(t, f.srv) {
		t.Fatalf("the pass that started the array registered the retry")
	}
}

// TestForcedStartLeavesASecondArrayFoundAfterTheStop pins the forced start's
// read after its stop (forcedStartPass): the group's broken array holds the
// named leg alone, and a second array of the group, inactive, holds the
// other leg, whose md/ listing did not answer when the pass walked, so the
// first read matched the broken array alone. After the stop the walk lists
// the second array, the read finds it, and the pass hands it to the test of
// an array that runs: nothing is zeroed or assembled beside it, the group's
// row names it, and the retry it registers stops it in its turn and starts
// the array from the named leg.
func TestForcedStartLeavesASecondArrayFoundAfterTheStop(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, quietLevel(3))
	// A split mirror: the group's array holds leg 2 alone, and a second
	// array holds leg 1.
	f.node.mu.Lock()
	array := f.node.arrays[f.mdDev]
	array.members = slices.DeleteFunc(slices.Clone(array.members),
		func(member string) bool { return member == f.leg1 })
	array.degraded = 1
	f.node.publishArray(array)
	f.node.mu.Unlock()
	f.node.setArrayState(f.mdDev, "broken")
	node := f.node.mdNode(f.mdDev)
	f.node.seedArray("/dev/md/split", "split", f.leg1)
	f.node.setArrayState("/dev/md/split", "inactive")
	split := f.node.mdNode("/dev/md/split")
	// The sweep's listing of the second array's md/ answers, the build's
	// walk's does not, and the walk after the stop answers again.
	f.node.mu.Lock()
	f.node.killCmdNth["list "+sysfsBlockDir+"/"+
		strings.TrimPrefix(split, "/dev/")+"/md"] = 2
	f.node.mu.Unlock()
	f.node.Reset()
	info := f.syncup(t, withMark(quietLevel(4), testDataLeg2))
	if !f.node.hasCall("cmd mdadm --stop " + node) {
		t.Fatalf("no stop of the broken array, which the first read " +
			"matched alone")
	}
	assertNoCall(t, f.node, "cmd mdadm --stop "+split)
	assertNoCall(t, f.node, "--zero-superblock")
	assertNoCall(t, f.node, "cmd mdadm --assemble")
	assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
		"array "+split+" is inactive", "grp data")
	if !retrying(t, f.srv) {
		t.Fatalf("the second array's error registered no retry")
	}
	f.node.Reset()
	retryAttempt(t, f.srv)
	assertOrder(t, f.node,
		"cmd mdadm --stop "+split,
		zeroOf(f.leg1),
		f.forced(f.leg2, mdUpdateUuid),
		f.add(f.leg1))
}

// TestForcedStartRetriesAReadBeforeIt pins the retry of a pass whose read of
// the group's array, before any step of the forced start, does not answer
// while the mark stands (markedErr; cnagent.md CN12): one member's state
// read of a broken array killed leaves the group ERROR, nothing stopped or
// zeroed, and registers the retry, whose converge, the read answering, stops
// the array and starts it from the named leg. The same read with no mark
// registers none: every leg is available, and nothing waits.
func TestForcedStartRetriesAReadBeforeIt(t *testing.T) {
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
			f.node.Reset()
			req := quietLevel(4)
			if marked {
				req = withMark(req, testDataLeg2)
			}
			info := f.syncup(t, req)
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
				state, "grp data")
			assertNoCall(t, f.node, "cmd mdadm --stop ")
			assertNoCall(t, f.node, "--zero-superblock")
			if retrying(t, f.srv) != marked {
				t.Fatalf("retrying = %v, want %v", !marked, marked)
			}
			if !marked {
				return
			}
			f.node.mu.Lock()
			delete(f.node.killReadAlways, state)
			f.node.mu.Unlock()
			f.node.Reset()
			retryAttempt(t, f.srv)
			assertOrder(t, f.node,
				"cmd mdadm --stop "+node,
				zeroOf(f.leg1),
				f.forced(f.leg2, mdUpdateUuid),
				f.add(f.leg1))
			if retrying(t, f.srv) {
				t.Fatalf("the retry's converge that started the array " +
					"left the retry")
			}
		})
	}
}

// TestForcedStartRetriesAWalkBeforeIt is TestForcedStartRetriesAReadBeforeIt
// for the md walk's half of the read (markedErr; cnagent.md CN12): with no
// array of the group, the build's /sys/block listing killed while the
// sweep's answers leaves the group ERROR, nothing zeroed or assembled, and
// registers the retry, whose converge, the listing answering, starts the
// array from the named leg. The same walk with no mark registers none.
func TestForcedStartRetriesAWalkBeforeIt(t *testing.T) {
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
			req := quietLevel(4)
			if marked {
				req = withMark(req, testDataLeg2)
			}
			info := f.syncup(t, req)
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
				sysfsBlockDir, "grp data")
			assertNoCall(t, f.node, "--zero-superblock")
			assertNoCall(t, f.node, examineOf(f.leg2))
			assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
			if retrying(t, f.srv) != marked {
				t.Fatalf("retrying = %v, want %v", !marked, marked)
			}
			if !marked {
				return
			}
			f.node.Reset()
			retryAttempt(t, f.srv)
			assertOrder(t, f.node,
				examineOf(f.leg2),
				zeroOf(f.leg1),
				f.forced(f.leg2, mdUpdateUuid),
				f.add(f.leg1))
			if retrying(t, f.srv) {
				t.Fatalf("the retry's converge that started the array " +
					"left the retry")
			}
		})
	}
}

// TestForcedStartActsOnlyOnADeliveredMark pins CN12's gate on the request a
// mark comes from: the startup reconcile of a stored request whose group
// carries a mark, with no array of the group, converges the group as with
// no mark — nothing is examined or zeroed and no forced assembly runs, the
// assembly of a group without a mark is tried instead — and a retry attempt
// from that request does the same, since the stored copy can carry a mark
// the worker has cleared since; the read after it names the mark in its
// verdict (storedMarks; cnagent.md CN30), so the worker re-sends the
// SyncupCntlr; and that SyncupCntlr, at the same revision, starts the array
// from the named leg, after which a read names the mark no more.
func TestForcedStartActsOnlyOnADeliveredMark(t *testing.T) {
	f := downNoRepair(t)
	req := withMark(quietLevel(4), testDataLeg2)
	path := f.srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr)
	if err := f.srv.store.Save(f.ctx, path, req); err != nil {
		t.Fatalf("saving the marked request: %v", err)
	}
	// md refuses the assembly of a group without a mark, as it does in the
	// case the mark is for.
	plain := "cmd mdadm --assemble " + f.mdDev + " --name " + f.arrayName() +
		" /dev/"
	f.node.mu.Lock()
	f.node.failCmdAlways[plain] = "mdadm: " + f.mdDev + " assembled from " +
		"1 drive - need all 2 to start it (use --run to insist)."
	f.node.mu.Unlock()
	f.node.Reset()
	fresh := newCnServer(f.node)
	fresh.retryInterval = time.Hour
	reconcileForTest(t, fresh)
	f.srv = fresh
	assertStoredMarkLeftAlone := func(label string) {
		t.Helper()
		if !f.node.hasCall(plain) {
			t.Fatalf("%s: no assembly of the group as with no mark", label)
		}
		assertNoCall(t, f.node, examineOf(f.leg2))
		assertNoCall(t, f.node, "--zero-superblock")
		assertNoCall(t, f.node, " --run --force")
	}
	assertStoredMarkLeftAlone("the startup reconcile")
	if retrying(t, f.srv) {
		t.Fatalf("the startup reconcile of a stored mark registered the " +
			"retry")
	}
	f.node.Reset()
	retryAttempt(t, f.srv)
	assertStoredMarkLeftAlone("a retry attempt")

	_, code := f.read(t, 4)
	if code.GetCode() != common.ReplyCodeLeftover ||
		!strings.Contains(code.GetDetails(), "forced start of group") {
		t.Fatalf("the read's verdict is %v, want the stored mark named",
			code)
	}

	f.node.Reset()
	info := f.syncup(t, req)
	assertOrder(t, f.node,
		examineOf(f.leg2),
		zeroOf(f.leg1),
		f.forced(f.leg2, mdUpdateUuid),
		f.add(f.leg1))
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	if _, code := f.read(t, 4); strings.Contains(code.GetDetails(),
		"forced start of group") {
		t.Fatalf("the read after the SyncupCntlr names the mark: %v", code)
	}
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
			fresh.retryInterval = time.Hour
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
// not waited for while it is dead.
func TestForcedStartBesideAStartedRepair(t *testing.T) {
	for _, tc := range []struct {
		name string
		dead bool
	}{
		{"the bad leg serving", false},
		{"the bad leg dead", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := downFix(t, repairReq(3, pb.RepStage_REP_STAGE_STARTED, false,
				repOpts{standby: true}))
			if !f.node.hasSuperblock(f.leg1) {
				t.Fatalf("the fixture's bad leg carries no superblock")
			}
			if tc.dead {
				f.badLegDead()
			}
			info := f.syncup(t, withMark(
				repairReq(4, pb.RepStage_REP_STAGE_STARTED, false,
					repOpts{noSubsys: true}), testDataLeg2))
			assertOrder(t, f.node,
				examineOf(f.leg2),
				zeroOf(f.newLegDev),
				f.forced(f.leg2, mdUpdateUuid),
				f.add(f.newLegDev))
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
// the group before any assembly and registers the retry; a zero the kernel
// completed while its run was killed is answered again as done by the
// retry's converge, whose start goes through and which registers the retry
// no more.
func TestForcedStartZeroEndsThePass(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		f := downNoRepair(t)
		f.node.mu.Lock()
		f.node.failCmd[zeroOf(f.leg1)] = "mdadm: Couldn't open " + f.leg1 +
			" for write - not zeroing"
		f.node.mu.Unlock()
		info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"not zeroing", "grp data")
		assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
		if !retrying(t, f.srv) {
			t.Fatalf("the refused zero registered no retry")
		}
	})
	t.Run("killed", func(t *testing.T) {
		f := downNoRepair(t)
		f.node.mu.Lock()
		f.node.killCmd[zeroOf(f.leg1)] = true
		f.node.mu.Unlock()
		info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
		if info.GetGrpIdToMdRaid()[testDataGrp].GetStatus() !=
			pb.ResStatus_RES_STATUS_ERROR {
			t.Fatalf("the killed zero's row is %v",
				info.GetGrpIdToMdRaid()[testDataGrp])
		}
		assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
		if f.node.hasSuperblock(f.leg1) {
			t.Fatalf("the killed zero did not reach the kernel")
		}
		if !retrying(t, f.srv) {
			t.Fatalf("the killed zero registered no retry")
		}
		f.node.Reset()
		retryAttempt(t, f.srv)
		assertOrder(t, f.node,
			zeroOf(f.leg1),
			f.forced(f.leg2, mdUpdateUuid),
			f.add(f.leg1))
		if retrying(t, f.srv) {
			t.Fatalf("the retry's converge that started the array left " +
				"the retry")
		}
		assertOk(t, probedCntlrInfo(t, f.srv).GetGrpIdToMdRaid()[testDataGrp],
			"grp data after the retry")
	})
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
// run comes, and the next pass, which the retry brings, runs the first form
// again. Both runs failing is the group's error with both outputs; a run
// killed after the kernel started the array, of either form, is taken as
// started.
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
		info := f.syncup(t, withMark(noRepairReq(4), testDataLeg2))
		assertOrder(t, f.node,
			examineOf(f.leg2),
			zeroOf(f.leg1),
			f.forced(f.leg2, mdUpdateUuid),
			examineOf(f.leg2),
			f.forced(f.leg2, mdUpdateForceNoBbl),
			f.add(f.leg1))
		assertNoCall(t, f.node, "cmd mdadm --stop ")
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		assertForcedRecord(t, logs, testDataLeg2, mdUpdateForceNoBbl,
			testDataLeg)
		f.assertNewIdentity(t, oldUuid)
	})
	t.Run("the refused run leaves an inactive array", func(t *testing.T) {
		f := downNoRepair(t)
		refused(f)
		leaves(f)
		info := f.syncup(t, withMark(noRepairReq(4), testDataLeg2))
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
		info := f.syncup(t, withMark(noRepairReq(4), testDataLeg2))
		row := info.GetGrpIdToMdRaid()[testDataGrp]
		assertErrorDetails(t, row, "--update="+mdUpdateUuid, "grp data")
		assertErrorDetails(t, row, "Cannot get exclusive access", "grp data")
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
		info := f.syncup(t, withMark(noRepairReq(4), testDataLeg2))
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
		info := f.syncup(t, withMark(noRepairReq(4), testDataLeg2))
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
		info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"--update="+mdUpdateUuid, "grp data")
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
		if !retrying(t, f.srv) {
			t.Fatalf("the first run that failed registered no retry")
		}
		f.node.Reset()
		retryAttempt(t, f.srv)
		assertOrder(t, f.node,
			zeroOf(f.leg1),
			f.forced(f.leg2, mdUpdateUuid),
			f.add(f.leg1))
		assertNoCall(t, f.node, "--update="+mdUpdateForceNoBbl)
		if retrying(t, f.srv) {
			t.Fatalf("the retry's converge that started the array left " +
				"the retry")
		}
		assertOk(t, probedCntlrInfo(t, f.srv).GetGrpIdToMdRaid()[testDataGrp],
			"grp data after the retry")
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
		info := f.syncup(t, withMark(noRepairReq(4), testDataLeg2))
		row := info.GetGrpIdToMdRaid()[testDataGrp]
		assertErrorDetails(t, row, "--update="+mdUpdateUuid, "grp data")
		assertErrorDetails(t, row, "--update="+mdUpdateForceNoBbl,
			"grp data")
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
		info := f.syncup(t, withMark(noRepairReq(4), testDataLeg2))
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
		info := f.syncup(t, withMark(noRepairReq(4), testDataLeg2))
		assertOrder(t, f.node,
			f.forced(f.leg2, mdUpdateForceNoBbl),
			f.add(f.leg1))
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		assertForcedRecord(t, logs, testDataLeg2, mdUpdateForceNoBbl,
			testDataLeg)
		f.assertNewIdentity(t, oldUuid)
	})
}

// TestForcedStartLeavesAnAbortingRepairsNewLeg pins the forced start's kept
// set: a new leg of a CREATED repair whose abort flag the request carries
// is a member the group keeps (memberNames) but none the forced start keeps
// (forcedStartLegs), so a mark naming it is the group's error and a mark
// naming leg 2 zeroes neither it nor the repair's bad leg.
func TestForcedStartLeavesAnAbortingRepairsNewLeg(t *testing.T) {
	t.Run("named", func(t *testing.T) {
		f := downFix(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, true,
			repOpts{standby: true}))
		f.node.setSuperblock(f.newLegDev, fakeSuperblock{events: 9})
		info := f.syncup(t, withMark(
			repairReq(4, pb.RepStage_REP_STAGE_CREATED, true), repNewLeg))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"does not keep", "grp data")
		assertNoCall(t, f.node, "--zero-superblock")
		assertNoCall(t, f.node, "cmd mdadm --assemble "+f.mdDev)
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
	assertNoCall(t, f.node, "cmd mdadm --examine")
	assertMissingSpLevel(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
}

// TestForcedStartLateMember pins the late-member term of the forced start
// (hasLateMember): while the mark stands and no array runs, a repair's new
// leg the forced start waits for registers the retry, whether it is the
// named leg or a member to zero, and a wait zeroes nothing, even a member
// that comes before the missing one; a provisioning new leg it waits for
// registers none, its provisioned flip bringing the converge (dnv-worker.md
// RW18); with the array running it does not, the mark waiting for the
// worker's clear.
func TestForcedStartLateMember(t *testing.T) {
	for _, tc := range []struct {
		name  string
		named uint64
	}{
		{"a member to zero", testDataLeg2},
		{"the named leg", repNewLeg},
		// The bad leg named: leg 2, available, comes before the dead new leg
		// among the members to zero, and is zeroed no more than the new leg
		// is, since every member is checked before the first zero.
		{"a member to zero after one that is there", testDataLeg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := downFix(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false,
				repOpts{standby: true}))
			f.legDead(repNewLeg, testIp, testSvcId)
			info := f.syncup(t, withMark(
				repairReq(4, pb.RepStage_REP_STAGE_CREATED, false,
					repOpts{noSubsys: true}), tc.named))
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
				"forced start waits for leg 0x7b", "grp data")
			assertNoCall(t, f.node, "--zero-superblock")
			if !retrying(t, f.srv) {
				t.Fatalf("the forced start's missing member registered no " +
					"retry")
			}
		})
	}
	t.Run("a provisioning new leg", func(t *testing.T) {
		f := downFix(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false,
			repOpts{standby: true, unprovisioned: true}))
		info := f.syncup(t, withMark(
			repairReq(4, pb.RepStage_REP_STAGE_CREATED, false,
				repOpts{unprovisioned: true, noSubsys: true}), testDataLeg2))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"forced start waits for leg 0x7b", "grp data")
		assertNoCall(t, f.node, "--zero-superblock")
		if retrying(t, f.srv) {
			t.Fatalf("the provisioning new leg registered the retry")
		}
	})
	t.Run("running", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
		f.legDead(repNewLeg, testIp, testSvcId)
		f.syncup(t, withMark(
			repairReq(4, pb.RepStage_REP_STAGE_CREATED, false,
				repOpts{noSubsys: true}), testDataLeg2))
		if retrying(t, f.srv) {
			t.Fatalf("the waiting mark registered the retry")
		}
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
			var examined []string
			for _, call := range f.node.callsMatching("cmd mdadm --examine /") {
				examined = append(examined,
					strings.TrimPrefix(call, "cmd mdadm --examine "))
			}
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
			if retrying(t, f.srv) {
				t.Fatalf("the refusal registered the retry")
			}
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
				"mdadm --examine", "grp data")
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
// when the examine of the member that keeps the data finds no superblock,
// the answer of a read that failed once the path's failfast expired: with
// nothing to compare the newer new leg with, the converge adds nothing,
// takes no bad leg out and reports no refusal, the group's row OK; the read
// names nothing and reports no refusal.
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
	var examined []string
	for _, call := range f.node.callsMatching("cmd mdadm --examine /") {
		examined = append(examined,
			strings.TrimPrefix(call, "cmd mdadm --examine "))
	}
	if want := []string{f.newLegDev, f.leg2}; !slices.Equal(examined, want) {
		t.Fatalf("the converge examined %v, want %v", examined, want)
	}
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	if got := verdict(info).GetRefusal(); got != "" {
		t.Fatalf("the converge's refusal is %q, want none", got)
	}
	assertVerdict(t, info, "the waiting repair", false, false)
	probeLegs(t, f.srv, repNewLeg)
	read, reply := f.read(t, 3)
	if reply.GetCode() != 0 {
		t.Fatalf("the read of the waiting repair: %v, want clean", reply)
	}
	if got := verdict(read).GetRefusal(); got != "" {
		t.Fatalf("the read's refusal is %q, want none", got)
	}
	assertVerdict(t, read, "the waiting repair, read", false, false)
}

// TestMdExamine pins the human-readable examine (Md.Examine): a device
// with no superblock reads nil, a killed run is an error, and the role, the
// recovery offset, the events count and the array's identity read from
// mdadm's own layout, a superblock whose role, events count or identity does
// not read being an error.
func TestMdExamine(t *testing.T) {
	ctx := context.Background()
	const member = "/dev/mapper/dnv-leg-a"
	node := newFakeNode()
	md := NewMd(node.osClient())
	sb, err := md.Examine(ctx, member)
	if err != nil || sb != nil {
		t.Fatalf("a device without a superblock read as (%v, %v)", sb, err)
	}
	node.setSuperblock(member, fakeSuperblock{
		role: "Active device 1", recovery: "45056 sectors", events: 4321})
	sb, err = md.Examine(ctx, member)
	if err != nil || sb.Role != "Active device 1" ||
		sb.RecoveryOffset != "45056 sectors" || sb.Events != 4321 ||
		sb.Uuid != fakeStampedUuid {
		t.Fatalf("the examine read %+v, %v", sb, err)
	}
	if gap := notFullMember(sb); gap == "" {
		t.Fatalf("a half-copied member read as full")
	}
	node.setSuperblock(member, fakeSuperblock{
		role: "Active device 0", events: 12})
	if sb, err = md.Examine(ctx, member); err != nil ||
		notFullMember(sb) != "" {
		t.Fatalf("a full member read as %+v, %v", sb, err)
	}
	node.killCmdAlways["mdadm --examine"] = true
	if sb, err = md.Examine(ctx, member); err == nil {
		t.Fatalf("a killed examine read as %+v", sb)
	}

	lab := strings.Join([]string{
		"/dev/dm-3:",
		"          Magic : a92b4efc",
		"        Version : 1.2",
		"    Feature Map : 0x3",
		"     Array UUID : c3e4f71d:6b3fa8d8:d0d4e115:d8fe68cd",
		"           Name : any:dnv-00000000000003a1-00-00",
		"  Creation Time : Mon Oct 26 19:56:11 2026",
		"    Data Offset : 6144 sectors",
		"   Super Offset : 8 sectors",
		"Recovery Offset : 45056 sectors",
		"    Update Time : Mon Oct 26 20:01:02 2026",
		"       Checksum : 1b2c3d4e - correct",
		"         Events : 4321",
		"",
		"",
		"   Device Role : Active device 1",
		"   Array State : AA ('A' == active, '.' == missing, " +
			"'R' == replacing)",
	}, "\n")
	sb, err = parseMdExamine("/dev/dm-3", lab)
	if err != nil || sb.Role != "Active device 1" ||
		sb.RecoveryOffset != "45056 sectors" || sb.Events != 4321 ||
		sb.Uuid != "c3e4f71d:6b3fa8d8:d0d4e115:d8fe68cd" {
		t.Fatalf("the lab layout read %+v, %v", sb, err)
	}
	for _, drop := range []string{"         Events : 4321",
		"   Device Role : Active device 1",
		"     Array UUID : c3e4f71d:6b3fa8d8:d0d4e115:d8fe68cd"} {
		if _, err := parseMdExamine("/dev/dm-3",
			strings.Replace(lab, drop, "", 1)); err == nil {
			t.Fatalf("an examine without %q read", strings.TrimSpace(drop))
		}
	}
}
