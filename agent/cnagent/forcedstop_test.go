package cnagent

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The read's name of CN12's forced-start stop (forcedStartNamed,
// errForcedStartStopDue), on the repair fixture's two-leg data group
// (repFix): an array of the group that does not run, which a converge
// stops before the forced start's checks, named while those checks do not
// hold.

// forcedStopNamed is the verdict's name of the stop of the data group's
// array, whose kernel node is node.
func forcedStopNamed(node string) string {
	return fmt.Sprintf("enumeration failed: forced start of group %#x: "+
		"array %s%s", testDataGrp, node, forcedStopText)
}

// forcedStopText is the end of every name of a forced start's stop, of any
// array (errForcedStartStopDue).
const forcedStopText = " of the group does not run, and a converge stops it"

// leg2Dead makes leg 2, the leg the tests' marks name, stop serving.
func (f *repFix) leg2Dead() { f.legDead(testDataLeg2, testIp2, testSvcId2) }

// TestForcedStartStopsAFlappingArrayWhileTheNamedLegIsDead pins the shape
// e2e react step 07 met: leg 2, the one member md holds in sync and the leg
// the mark names, is dead; leg 1 is the half-copied member md keeps retrying
// the copy onto, so array_state moves between write-pending (each
// superblock write to the dead member) and broken. The mark's own converge
// reads write-pending and leaves the mark waiting. A later read that reads
// broken must name the forced start's stop, so that the worker's re-send
// stops the array while the named leg is still dead; nothing is zeroed or
// assembled, the forced start then waiting for leg 2, and the reads after
// the stop name nothing while leg 2 stays dead. Once leg 2 serves, the read
// names the forced start, and the re-send on that round starts the array
// from leg 2.
func TestForcedStartStopsAFlappingArrayWhileTheNamedLegIsDead(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, quietLevel(3))
	f.copying(f.leg1, "1")
	f.leg2Dead()
	f.node.setArrayState(f.mdDev, "write-pending")
	node := f.node.mdNode(f.mdDev)
	f.node.forgetTraces()
	f.node.Reset()
	held := withMark(quietLevel(4), testDataLeg2)
	info := f.syncup(t, held)
	f.assertMarkLeftWaiting(t, f.leg2)
	t.Logf("the mark's converge, array write-pending: row %v",
		info.GetGrpIdToMdRaid()[testDataGrp])

	// md's next retry: the array reads broken, the named leg still dead.
	f.node.setArrayState(f.mdDev, "broken")
	f.node.Reset()
	round, resent := resendRound(t, f.srv, held, "named")
	t.Logf("the round over the broken array: code %d details %q",
		round.GetAgentReply().GetCode(), round.GetAgentReply().GetDetails())
	if resent == nil {
		t.Fatalf("the round over the broken array whose named leg is dead " +
			"brought no re-send: nothing stops the array until the leg " +
			"is back")
	}
	if got := round.GetAgentReply().GetDetails(); !strings.Contains(got,
		forcedStopNamed(node)) {
		t.Fatalf("the round's verdict is %q, want the stop of %s named",
			got, node)
	}
	if !tracedCall(f.node, "named", "cmd mdadm --stop "+node) {
		t.Fatalf("the re-send did not stop the broken array")
	}
	if !f.node.arrayGone(f.mdDev) {
		t.Fatalf("the array is still there after the re-send's stop")
	}
	assertNoCall(t, f.node, "--zero-superblock")
	assertNoCall(t, f.node, "cmd mdadm --assemble")
	row := resent.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]
	if !strings.Contains(row.GetDetails(), "forced start waits for leg") {
		t.Fatalf("the row after the stop is %v, want the wait for leg 2", row)
	}
	f.assertForcedStartUnnamed(t, 4, "after the stop, leg 2 still dead",
		"named")

	// Leg 2 serves again: the forced start is named, and its re-send starts
	// the array from leg 2 alone and adds leg 1 back, zeroed.
	f.node.setCtrlState(legNqn(f.srv, testDataLeg2), testIp2, testSvcId2,
		"live")
	f.node.Reset()
	f.forcedStartResent(t, held, testDataLeg2, "back")
	assertOrder(t, f.node,
		examineOf(f.leg2),
		zeroOf(f.leg1),
		f.forced(f.leg2, mdUpdateUuid),
		f.add(f.leg1))
	f.assertForcedStartUnnamed(t, 4, "after the start", "named", "back")
}

// TestTheReadNamesAForcedStartsStopExactlyWhenAConvergeWouldStopIt holds the
// read's name of the forced start's stop to the converge, over the data
// group's mark on leg 2 in a request that counts as delivered, after the
// mark's converge left it waiting (the array ran then, or its read of leg 2
// failed once): the read names the stop exactly when the converge after it
// stops an array of the group that is there and does not run, broken or
// inactive, while the forced start's checks do not hold — the named leg
// dead, a member it zeroes dead, a named leg that is not full or older than
// a member it zeroes, a member's superblock read that fails — and names it
// nowhere the converge stops nothing: over an array read write-pending or
// active, which counts as running, one that holds another sp's leg or a
// member that is no dm device, one a device above holds, and with no array
// of the group. A listing of the holders that does not answer names nothing,
// though the converge, which lists no holders, stops the array; the next
// read reads again. The forced start itself is named in none of these, and
// the read after the stop names nothing.
func TestTheReadNamesAForcedStartsStopExactlyWhenAConvergeWouldStopIt(
	t *testing.T,
) {
	const rev = 4
	broken := func(t *testing.T, f *repFix) {
		f.node.setArrayState(f.mdDev, "broken")
	}
	brokenDead := func(t *testing.T, f *repFix) {
		broken(t, f)
		f.leg2Dead()
	}
	planted := func(
		state string,
		member func(f *repFix) string,
	) func(t *testing.T, f *repFix) {
		return func(t *testing.T, f *repFix) {
			f.node.mu.Lock()
			array := f.node.arrays[f.mdDev]
			held := append(slices.Clone(array.members), member(f))
			name := array.name
			f.node.mu.Unlock()
			f.node.seedArray(f.mdDev, name, held...)
			f.node.setArrayState(f.mdDev, state)
			f.leg2Dead()
		}
	}
	otherSpLeg := func(f *repFix) string {
		return "/dev/mapper/" + names(f.srv).CnLegName(
			testCluster, testCn, testSp+1, testDataLeg)
	}
	noDm := func(*repFix) string { return "/dev/sdb1" }
	for _, tc := range []struct {
		name    string
		base    forcedBase
		arm     func(t *testing.T, f *repFix)
		named   bool
		stopped bool
	}{
		{"a broken array, the named leg dead", forcedRunning, brokenDead,
			true, true},
		{"an inactive array, the named leg dead", forcedRunning,
			func(t *testing.T, f *repFix) {
				f.node.setArrayState(f.mdDev, "inactive")
				f.leg2Dead()
			}, true, true},
		{"a broken array, a member it zeroes dead", forcedRunning,
			func(t *testing.T, f *repFix) {
				broken(t, f)
				f.badLegDead()
			}, true, true},
		{"a broken array whose named leg is not full", forcedRunning,
			func(t *testing.T, f *repFix) {
				broken(t, f)
				f.node.setSuperblock(f.leg2, fakeSuperblock{
					recovery: "1024 sectors", events: 10})
			}, true, true},
		{"a broken array whose named leg is older than a member it zeroes",
			forcedRunning, func(t *testing.T, f *repFix) {
				broken(t, f)
				f.node.setSuperblock(f.leg2, fakeSuperblock{events: 10})
				f.node.setSuperblock(f.leg1, fakeSuperblock{events: 50})
			}, true, true},
		{"a broken array, a member's superblock read fails", forcedRunning,
			func(t *testing.T, f *repFix) {
				broken(t, f)
				f.node.mu.Lock()
				f.node.failCmdAlways["readsb "+f.leg1] = ""
				f.node.mu.Unlock()
			}, true, true},
		{"an array read write-pending, the named leg dead", forcedRunning,
			func(t *testing.T, f *repFix) {
				f.node.setArrayState(f.mdDev, "write-pending")
				f.leg2Dead()
			}, false, false},
		{"an array read active, the named leg dead", forcedRunning,
			func(t *testing.T, f *repFix) {
				f.node.setArrayState(f.mdDev, "active")
				f.leg2Dead()
			}, false, false},
		{"an array that holds another sp's leg, the named leg dead",
			forcedRunning, planted("inactive", otherSpLeg), false, false},
		{"an array that holds a member that is no dm device, the named " +
			"leg dead", forcedRunning, planted("broken", noDm),
			false, false},
		{"a device above holds the broken array, the named leg dead",
			forcedPooled, brokenDead, false, false},
		{"no array of the group, the named leg dead", forcedNone,
			func(t *testing.T, f *repFix) { f.leg2Dead() }, false, false},
		{"the holders listing does not answer", forcedRunning,
			func(t *testing.T, f *repFix) {
				brokenDead(t, f)
				holders := "list " + sysfsBlockDir + "/" + strings.TrimPrefix(
					f.node.mdNode(f.mdDev), "/dev/") + "/holders"
				f.node.mu.Lock()
				f.node.failCmdAlways[holders] = "input/output error"
				f.node.mu.Unlock()
			}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.base.fix(t)
			req := tc.base.req()
			// The converge that leaves the mark standing: its read of the
			// named leg fails once, where it gets that far.
			blocker := "readsb " + f.leg2
			f.node.mu.Lock()
			f.node.failCmd[blocker] = ""
			f.node.mu.Unlock()
			f.syncupAny(t, req)
			f.node.mu.Lock()
			delete(f.node.failCmd, blocker)
			f.node.mu.Unlock()
			tc.arm(t, f)
			node := f.node.mdNode(f.mdDev)
			there := !f.node.arrayGone(f.mdDev)

			_, reply := f.read(t, rev)
			details := reply.GetDetails()
			named := strings.Contains(details, forcedStopText)
			if named && there &&
				!strings.Contains(details, forcedStopNamed(node)) {
				t.Fatalf("the read names the stop of another array than "+
					"%s: %q", node, details)
			}
			if strings.Contains(details, forcedNamed(testDataLeg2)) {
				t.Fatalf("the read names the forced start, whose checks "+
					"do not hold: %q", details)
			}
			f.node.Reset()
			f.syncupAny(t, req)
			stopped := there && f.node.arrayGone(f.mdDev)
			if named != tc.named || stopped != tc.stopped {
				t.Fatalf("the read named the stop: %v (%q), want %v; the "+
					"converge after it stopped the array: %v (%q), want %v",
					named, details, tc.named, stopped,
					f.node.callsMatching("cmd mdadm"), tc.stopped)
			}
			assertNoCall(t, f.node, "--zero-superblock")
			assertNoCall(t, f.node, "cmd mdadm --assemble")
			if _, after := f.read(t, rev); stopped && strings.Contains(
				after.GetDetails(), "forced start of group") {
				t.Fatalf("the read after the stop names the forced start: "+
					"%q", after.GetDetails())
			}
		})
	}
}

// TestTheStopOfAForcedStartIsNamedOnlyForAMarkAConvergeActsOn: over a broken
// array whose named leg is dead, the read names the stop only for a mark a
// converge acts on (actsOnMark). The stored mark the startup reconcile
// loaded, which counts as not delivered, is named as stored (storedMarks),
// never as the stop, and the re-send that name brings, which counts as
// delivered, stops the array; a group with no mark is named by no forced
// start at all.
func TestTheStopOfAForcedStartIsNamedOnlyForAMarkAConvergeActsOn(
	t *testing.T,
) {
	t.Run("a stored mark", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		req := withMark(quietLevel(4), testDataLeg2)
		path := f.srv.nf.LocalCntlrPath(testCluster, testCn, testSp,
			testCntlr)
		if err := f.srv.store.Save(f.ctx, path, req); err != nil {
			t.Fatalf("saving the marked request: %v", err)
		}
		f.node.setArrayState(f.mdDev, "broken")
		f.leg2Dead()
		node := f.node.mdNode(f.mdDev)
		f.node.Reset()
		fresh := newCnServer(f.node)
		reconcileForTest(t, fresh)
		f.srv = fresh
		f.assertMarkLeftWaiting(t, f.leg2)
		f.node.forgetTraces()
		f.node.Reset()
		round, resent := resendRound(t, f.srv, req, "stored")
		details := round.GetAgentReply().GetDetails()
		if !strings.Contains(details, fmt.Sprintf(
			"forced start of group %#x: %s", testDataGrp, errStoredMark)) ||
			resent == nil {
			t.Fatalf("the round after the startup reconcile is %q, "+
				"re-sent %v; want the stored mark named and a re-send",
				details, resent != nil)
		}
		if strings.Contains(details, forcedStopText) {
			t.Fatalf("the round names the stop of a mark no converge acts "+
				"on: %q", details)
		}
		if !tracedCall(f.node, "stored", "cmd mdadm --stop "+node) ||
			!f.node.arrayGone(f.mdDev) {
			t.Fatalf("the delivered re-send did not stop the broken array")
		}
		f.assertForcedStartUnnamed(t, 4, "after the stop", "stored")
	})
	t.Run("no mark", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		f.node.setArrayState(f.mdDev, "broken")
		f.leg2Dead()
		if _, reply := f.read(t, 3); strings.Contains(reply.GetDetails(),
			"forced start of group") {
			t.Fatalf("the read names a forced start of a group with no "+
				"mark: %q", reply.GetDetails())
		}
	})
}
