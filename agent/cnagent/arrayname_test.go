package cnagent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The two names a cntlr's read gives a group whose array does not run
// (cnagent.md CN12, CN30): the absent array the converge's assembly would
// start (absentArrayNamed), and the forced start the converge would carry
// through (forcedStartNamed). Each is named exactly while the next converge
// starts the group's array, and the one re-send the worker sends on the
// round that names it (dnv-worker.md RW4) cures it. Both are read-only: a
// converge's own reply names neither.

// absentNamed is how a verdict names the data group's absent array.
func (f *repFix) absentNamed() string {
	return agent.LeftoverKindMdAbsent + ":" + f.mdDev
}

// forcedNamed is how a verdict names the data group's forced start from
// legId.
func forcedNamed(legId uint64) string {
	return fmt.Sprintf("enumeration failed: forced start of group %#x: no "+
		"array of the group runs, and a converge starts it from leg %#x",
		testDataGrp, legId)
}

// dataArrayRuns reports whether the data group's array runs on the node.
func (f *repFix) dataArrayRuns() bool {
	f.node.mu.Lock()
	defer f.node.mu.Unlock()
	array := f.node.arrays[f.mdDev]
	return array != nil && mdRunning(array.arrayState)
}

// legAna sets the ANA state of the path of one of the data group's legs:
// "non-optimized" makes the leg late, as a side that has not flipped to
// this cntlr reads, and "optimized" makes it serve.
func (f *repFix) legAna(legId uint64, state string) {
	addr, svcId := testIp, testSvcId
	if legId == testDataLeg2 {
		addr, svcId = testIp2, testSvcId2
	}
	f.node.setAnaState(legNqn(f.srv, legId), addr, svcId, state)
}

// dropSuperblocks takes md's superblocks off both legs of the data group,
// as on legs freshly provisioned, or zeroed by a forced start cut short.
func (f *repFix) dropSuperblocks() {
	f.node.mu.Lock()
	defer f.node.mu.Unlock()
	delete(f.node.superblocks, f.leg1)
	delete(f.node.superblocks, f.leg2)
}

// unansweredArray seeds an array of another sp whose member's dm name does
// not answer, so the md walk records it unanswered and finds no array of the
// data group it could vouch absent (Md.Detail): a read of the group's array
// that did not answer, on a read and on a converge alike.
func (f *repFix) unansweredArray() {
	const leg = "/dev/mapper/other-sp-leg"
	f.node.seedArray("/dev/md/other", "other", leg)
	f.node.mu.Lock()
	defer f.node.mu.Unlock()
	f.node.failReadAlways["/dev-"+f.node.kernelName(leg)+
		"/block/dm/name"] = true
}

// syncupAny converges one request whatever the reply's code.
func (f *repFix) syncupAny(t *testing.T, req *pb.SyncupCntlrRequest) {
	t.Helper()
	if _, err := f.srv.SyncupCntlr(f.ctx, req); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
}

// ---------------------------------------------------------------------------
// mdadm's degraded gate (mdadmStarts)
// ---------------------------------------------------------------------------

// TestMdadmStartsAsMdadmDoes pins the read's guess of mdadm's plain
// assembly (mdadmStarts) against what mdadm's Assemble.c decides for an
// explicit device list with neither --run nor --force, and against what the
// lab's mdadm did (cnagent_integtest.md, Lab facts): a lone survivor whose
// superblock still counts its peer is refused, one that recorded its peer
// failed starts, a half-copied member alone is refused, a member whose
// bad-block list the kernel cannot import is refused its add. mdadm adds
// every member it keeps in a place, counted or not, and takes each refused
// add off its count; md then runs the array only with an in-sync member
// whose add went through. Each case is a member list as the fake's
// superblock facts give it, refused marking the members whose list holds an
// entry of length zero, read back through Md.ReadMember from the fake, so
// the fake's own gate (fakeAssemblyStarts), which the agreement tests below
// start arrays by, is held to the same answers.
func TestMdadmStartsAsMdadmDoes(t *testing.T) {
	type member = fakeSuperblock
	const one = "Active device 1"
	for _, tc := range []struct {
		name    string
		members []member
		starts  bool
		refused []int
	}{
		{"a lone survivor that counts its peer",
			[]member{{events: 10, arrayState: "AA"}}, false, nil},
		{"a lone survivor that recorded its peer failed",
			[]member{{events: 10, arrayState: "A."}}, true, nil},
		{"both at one count",
			[]member{{events: 10, arrayState: "AA"},
				{role: one, events: 10, arrayState: "AA"}}, true, nil},
		{"a member one change behind",
			[]member{{events: 10, arrayState: "AA"},
				{role: one, events: 9, arrayState: "AA"}}, true, nil},
		{"a member two changes behind",
			[]member{{events: 10, arrayState: "AA"},
				{role: one, events: 8, arrayState: "AA"}}, false, nil},
		{"the older member handed first",
			[]member{{role: one, events: 9, arrayState: "AA"},
				{events: 10, arrayState: "AA"}}, true, nil},
		{"a member that records the newest failed",
			[]member{{events: 10, arrayState: "AA"},
				{role: one, events: 10, arrayState: ".A"}}, false, nil},
		{"a half-copied member alone",
			[]member{{role: one, events: 11, arrayState: "AA",
				recovery: "146944 sectors"}}, false, nil},
		{"a whole member and a half-copied one",
			[]member{{events: 10, arrayState: "AA"},
				{role: one, events: 10, arrayState: "AA",
					recovery: "146944 sectors"}}, true, nil},
		{"a spare alone",
			[]member{{role: "spare", events: 3, arrayState: "AA"}}, false, nil},
		{"a whole member that counts its peer, and a spare",
			[]member{{events: 10, arrayState: "AA"},
				{role: "spare", events: 10, arrayState: "AA"}}, false, nil},
		{"a faulty member alone",
			[]member{{role: "faulty", events: 10, arrayState: "A."}}, false, nil},
		{"two members in one place that record the other failed",
			[]member{{events: 10, arrayState: "A."},
				{events: 9, arrayState: "A."}}, true, nil},
		{"two members in one place that count a second place",
			[]member{{events: 10, arrayState: "AA"},
				{events: 10, arrayState: "AA"}}, false, nil},
		{"two of three places, the third failed",
			[]member{{events: 10, arrayState: "AA."},
				{role: one, events: 10, arrayState: "AA."}}, true, nil},
		{"two of three places, the third counted",
			[]member{{events: 10, arrayState: "AAA"},
				{role: one, events: 10, arrayState: "AAA"}}, false, nil},
		// A broken array stopped with its in-sync leg back: the leg's list
		// is refused, and the half-copied leg alone cannot start it.
		{"a whole member whose list is refused, and a half-copied one",
			[]member{{events: 20, arrayState: "AA"},
				{role: one, events: 20, arrayState: "AA",
					recovery: "146944 sectors"}}, false, []int{0}},
		{"a lone survivor and a stale peer",
			[]member{{events: 12, arrayState: "A."},
				{role: one, events: 9, arrayState: "AA"}}, true, nil},
		{"a lone survivor and a stale peer whose list is refused",
			[]member{{events: 12, arrayState: "A."},
				{role: one, events: 9, arrayState: "AA"}}, false, []int{1}},
		{"two in sync, one's list refused",
			[]member{{events: 10, arrayState: "AA"},
				{role: one, events: 10, arrayState: "AA"}}, false,
			[]int{1}},
		{"a survivor that recorded its peer failed, the peer's list refused",
			[]member{{events: 10, arrayState: "A."},
				{role: one, events: 10, arrayState: "AA"}}, true, []int{1}},
		{"a lone survivor whose own list is refused",
			[]member{{events: 10, arrayState: "A."}}, false, []int{0}},
		// mdadm's count passes, 1 in sync and 1 rebuilding less 1 refused
		// against 1 working, but md runs no mirror with no in-sync member.
		{"a survivor whose list is refused, and a half-copied peer",
			[]member{{events: 10, arrayState: "A."},
				{role: one, events: 10, arrayState: "AA",
					recovery: "146944 sectors"}}, false, []int{0}},
		// mdadm drops a member that records the newest failed before it
		// adds anything, so its refused list costs nothing.
		{"a refused member that records the newest failed",
			[]member{{events: 10, arrayState: "A."},
				{role: one, events: 10, arrayState: ".A"}}, true, []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := newFakeNode()
			var paths []string
			for i, m := range tc.members {
				path := fmt.Sprintf("/dev/mapper/member%d", i)
				paths = append(paths, path)
				node.superblocks[path] = true
				node.sbFacts[path] = m
				node.devNo[path] = node.newDevNo()
				node.bblRefused[path] = slices.Contains(tc.refused, i)
			}
			var sbs []*mdSuperblock
			for i, path := range paths {
				reading := NewMd(node.osClient()).ReadMember(
					context.Background(), path)
				if reading.Err != nil || reading.Sb == nil ||
					reading.Sb.BadBlocksRefused !=
						slices.Contains(tc.refused, i) {
					t.Fatalf("member %d: %+v, %v", i, reading.Sb, reading.Err)
				}
				sbs = append(sbs, reading.Sb)
			}
			if got := mdadmStarts(sbs); got != tc.starts {
				t.Errorf("mdadmStarts: %v, want %v", got, tc.starts)
			}
			if got, _ := node.fakeAssemblyStarts(paths); got != tc.starts {
				t.Errorf("the fake's gate: %v, want %v", got, tc.starts)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The absent array (absentArrayNamed)
// ---------------------------------------------------------------------------

// TestTheReadNamesAnAbsentArrayExactlyWhenAConvergeWouldStartIt holds the
// read to the converge over a two-leg group with no array on a primary,
// which a converge whose superblock read failed once left so: the read
// names the absent array exactly when the converge after it starts the
// array, by each case of CN12's assembly — case 2 from members of one
// identity that mdadm's degraded gate lets start, at its edges, and case 1
// over legs that carry no superblock — and names nothing for each refusal
// that lasts: a late leg whose peer's superblock counts it, a member two
// changes behind or that records the newest failed, a superblock read that
// fails, members of two identities, a case 1 with a leg late, over a group
// whose array has run or under a name in use, and no leg that serves.
func TestTheReadNamesAnAbsentArrayExactlyWhenAConvergeWouldStartIt(
	t *testing.T,
) {
	const rev = 4
	const one = "Active device 1"
	views := func(f *repFix, leg1, leg2 fakeSuperblock) {
		f.node.setSuperblock(f.leg1, leg1)
		f.node.setSuperblock(f.leg2, leg2)
	}
	for _, tc := range []struct {
		name  string
		made  bool
		arm   func(t *testing.T, f *repFix)
		named bool
	}{
		{"case 2, both legs serve", false, nil, true},
		{"case 2, a lone survivor that counts its late peer", false,
			func(t *testing.T, f *repFix) {
				views(f, fakeSuperblock{events: 10, arrayState: "AA"},
					fakeSuperblock{role: one, events: 10, arrayState: "AA"})
				f.legAna(testDataLeg2, "non-optimized")
			}, false},
		{"case 2, a lone survivor that recorded its late peer failed",
			false, func(t *testing.T, f *repFix) {
				views(f, fakeSuperblock{events: 10, arrayState: "A."},
					fakeSuperblock{role: one, events: 9, arrayState: "AA"})
				f.legAna(testDataLeg2, "non-optimized")
			}, true},
		{"case 2, a member one change behind", false,
			func(t *testing.T, f *repFix) {
				views(f, fakeSuperblock{events: 10, arrayState: "AA"},
					fakeSuperblock{role: one, events: 9, arrayState: "AA"})
			}, true},
		{"case 2, a member two changes behind", false,
			func(t *testing.T, f *repFix) {
				views(f, fakeSuperblock{events: 10, arrayState: "AA"},
					fakeSuperblock{role: one, events: 8, arrayState: "AA"})
			}, false},
		{"case 2, a member that records the newest failed", false,
			func(t *testing.T, f *repFix) {
				views(f, fakeSuperblock{events: 10, arrayState: "AA"},
					fakeSuperblock{role: one, events: 10, arrayState: ".A"})
			}, false},
		{"case 2, a superblock read that fails", false,
			func(t *testing.T, f *repFix) {
				f.node.mu.Lock()
				f.node.failCmdAlways["readsb "+f.leg1] = ""
				f.node.mu.Unlock()
			}, false},
		{"case 2, members of two identities", false,
			func(t *testing.T, f *repFix) {
				f.identity(f.leg1, "older")
			}, false},
		{"case 1", false, func(t *testing.T, f *repFix) {
			f.dropSuperblocks()
		}, true},
		{"case 1, a leg late", false, func(t *testing.T, f *repFix) {
			f.dropSuperblocks()
			f.legAna(testDataLeg2, "non-optimized")
		}, false},
		{"case 1, a group whose array has run", true,
			func(t *testing.T, f *repFix) {
				f.dropSuperblocks()
			}, false},
		{"case 1, the name in use", false, func(t *testing.T, f *repFix) {
			f.dropSuperblocks()
			f.node.mu.Lock()
			f.node.devNo[f.mdDev] = "9:127"
			f.node.mu.Unlock()
		}, false},
		{"no leg serves", false, func(t *testing.T, f *repFix) {
			f.legAna(testDataLeg, "non-optimized")
			f.legAna(testDataLeg2, "non-optimized")
		}, false},
		// A broken array stopped with its in-sync leg back: the kernel
		// refuses leg 1's list, and leg 2, half-copied, cannot start the
		// array alone.
		{"case 2, a whole member whose bad-block list is refused", false,
			func(t *testing.T, f *repFix) {
				views(f, fakeSuperblock{events: 20, arrayState: "AA"},
					fakeSuperblock{role: one, events: 20, arrayState: "AA",
						recovery: "146944 sectors"})
				f.node.mu.Lock()
				f.node.bblRefused[f.leg1] = true
				f.node.mu.Unlock()
			}, false},
		{"case 2, a refused peer the start goes on without", false,
			func(t *testing.T, f *repFix) {
				views(f, fakeSuperblock{events: 10, arrayState: "A."},
					fakeSuperblock{role: one, events: 10, arrayState: "AA"})
				f.node.mu.Lock()
				f.node.bblRefused[f.leg2] = true
				f.node.mu.Unlock()
			}, true},
		{"the read of the group's array does not answer", false,
			func(t *testing.T, f *repFix) {
				f.unansweredArray()
			}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := downNoRepair(t)
			req := quietReq(rev)
			dataGrpOf(req).ArrayMade = tc.made
			// The converge that leaves the array absent: its first
			// superblock read fails once.
			blocker := "readsb " + f.leg1
			f.node.mu.Lock()
			f.node.failCmd[blocker] = ""
			f.node.mu.Unlock()
			f.syncupAny(t, req)
			f.node.mu.Lock()
			delete(f.node.failCmd, blocker)
			f.node.mu.Unlock()
			if f.dataArrayRuns() {
				t.Fatalf("the case is vacuous: the first converge started " +
					"the array")
			}
			if tc.arm != nil {
				tc.arm(t, f)
			}

			_, reply := f.read(t, rev)
			named := strings.Contains(reply.GetDetails(), f.absentNamed())
			f.node.Reset()
			f.syncupAny(t, req)
			started := f.dataArrayRuns()
			if named != started {
				t.Fatalf("the read named the absent array: %v (%q); the "+
					"converge after it started the array: %v (%q)", named,
					reply.GetDetails(), started,
					f.node.callsMatching("cmd mdadm"))
			}
			if named != tc.named {
				t.Fatalf("the read named the absent array: %v, want %v (%q)",
					named, tc.named, reply.GetDetails())
			}
			if started {
				if _, after := f.read(t, rev); strings.Contains(
					after.GetDetails(), agent.LeftoverKindMdAbsent+":") {
					t.Fatalf("the read after the start names the array: %q",
						after.GetDetails())
				}
			}
		})
	}
}

// TestALoneSurvivorIsNamedOnceItsPeerServes is the case the absent array's
// name exists for: a promotion whose second leg reads non-optimized, over
// legs whose superblocks both count both members, as a clean demotion
// leaves them, is refused by mdadm's degraded gate; the read names nothing
// while the leg stays late, though its peer serves; once the side flips the
// read names the absent array, the one SyncupCntlr the worker re-sends on
// that round assembles it from both legs, and the read after it names
// nothing. The converge's own reply never names it.
func TestALoneSurvivorIsNamedOnceItsPeerServes(t *testing.T) {
	const rev = 4
	f := downNoRepair(t)
	f.node.setSuperblock(f.leg1, fakeSuperblock{events: 10, arrayState: "AA"})
	f.node.setSuperblock(f.leg2, fakeSuperblock{role: "Active device 1",
		events: 10, arrayState: "AA"})
	f.legAna(testDataLeg2, "non-optimized")
	held := quietReq(rev)
	reply, err := f.srv.SyncupCntlr(f.ctx, held)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	assertErrorDetails(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
		"need 2 to start", "grp data, leg 2 late")
	if strings.Contains(reply.GetAgentReply().GetDetails(), f.absentNamed()) {
		t.Fatalf("the converge's own reply names the array: %v",
			reply.GetAgentReply())
	}
	if _, read := f.read(t, rev); strings.Contains(read.GetDetails(),
		agent.LeftoverKindMdAbsent+":") {
		t.Fatalf("the read names the array while its peer is late: %q",
			read.GetDetails())
	}

	f.legAna(testDataLeg2, "optimized")
	round := rowRound(t, f.srv, rev)
	if round.GetAgentReply().GetCode() != common.ReplyCodeLeftover ||
		!strings.Contains(round.GetAgentReply().GetDetails(),
			f.absentNamed()) {
		t.Fatalf("the round after the flip is %v, want the absent array "+
			"named", round.GetAgentReply())
	}
	f.node.Reset()
	if rowResend(t, f.srv, held, round) == nil {
		t.Fatalf("the round that names the array brought no re-send")
	}
	both := "cmd mdadm --assemble " + f.mdDev + " --name " + f.arrayName() +
		" " + f.leg1 + " " + f.leg2
	if got := f.node.callsMatching("cmd mdadm --assemble"); len(got) != 1 ||
		got[0] != both {
		t.Fatalf("the re-send ran %q, want exactly %q", got, both)
	}
	if !f.dataArrayRuns() {
		t.Fatalf("the re-send left no array running")
	}
	if _, read := f.read(t, rev); strings.Contains(read.GetDetails(),
		agent.LeftoverKindMdAbsent+":") {
		t.Fatalf("the read after the re-send names the array: %q",
			read.GetDetails())
	}
}

// ---------------------------------------------------------------------------
// The forced start (forcedStartNamed)
// ---------------------------------------------------------------------------

// forcedBase is a fixture of the forced start's agreement test: the repair
// fixture as the base leaves it, and the request with the data group's mark
// on leg 2 that the test holds.
type forcedBase struct {
	fix func(t *testing.T) *repFix
	req func() *pb.SyncupCntlrRequest
}

var (
	// forcedNone has no array of the data group, and the request builds no
	// thin pool, so nothing above holds an array of the group.
	forcedNone = forcedBase{downNoRepair, func() *pb.SyncupCntlrRequest {
		return withMark(quietLevel(4), testDataLeg2)
	}}
	// forcedRunning has the data group's array running at the level that
	// builds no thin pool.
	forcedRunning = forcedBase{func(t *testing.T) *repFix {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		return f
	}, func() *pb.SyncupCntlrRequest {
		return withMark(quietLevel(4), testDataLeg2)
	}}
	// forcedPooled has the data group's array running under the slice's
	// pool, whose concat holds it.
	forcedPooled = forcedBase{func(t *testing.T) *repFix {
		return newRepFix(t, allLegs...)
	}, func() *pb.SyncupCntlrRequest {
		return withMark(quietReq(4), testDataLeg2)
	}}
	// forcedProvisioning has no array of the data group, and a CREATED
	// repair whose new leg, which the forced start keeps, is still
	// provisioning.
	forcedProvisioning = forcedBase{func(t *testing.T) *repFix {
		return downFix(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false,
			repOpts{standby: true, unprovisioned: true}))
	}, func() *pb.SyncupCntlrRequest {
		return withMark(repairReq(4, pb.RepStage_REP_STAGE_CREATED, false,
			repOpts{unprovisioned: true, noSubsys: true}), testDataLeg2)
	}}
)

// TestTheReadNamesAForcedStartExactlyWhenAConvergeWouldStartIt holds the
// read to the converge over the data group's mark on leg 2, in a request
// that counts as delivered, after a converge that did not start the array
// (its read of the named leg failed once, or the array ran then): the read
// names the forced start exactly when the converge after it starts the
// array, with no array of the group or over one that is there and does not
// run, inactive or broken, and names nothing for each refusal that lasts —
// a named leg that is not full or older than a member it would zero, a
// member it keeps that is late or whose superblock read fails, a kept new
// leg still provisioning, also with a subsystem, a head and a wrapper of
// its own planted, an array that holds another sp's leg, a device above
// that holds the array — nor over an array that runs, where the mark waits.
func TestTheReadNamesAForcedStartExactlyWhenAConvergeWouldStartIt(
	t *testing.T,
) {
	const rev = 4
	stranger := func(t *testing.T, f *repFix) {
		other := "/dev/mapper/" + names(f.srv).CnLegName(
			testCluster, testCn, testSp+1, testDataLeg)
		f.node.mu.Lock()
		array := f.node.arrays[f.mdDev]
		held := append(append([]string(nil), array.members...), other)
		name := array.name
		f.node.mu.Unlock()
		f.node.seedArray(f.mdDev, name, held...)
		f.node.setArrayState(f.mdDev, "inactive")
	}
	// A provisioning new leg's subsystem, head and wrapper, as the converge
	// would build them for a leg that is not provisioning: only the leg's
	// gate keeps a read from finding it available (readAvailable), as the
	// converge never finds a provisioning leg available (ensureLegs).
	provisioningLegBuilt := func(t *testing.T, f *repFix) {
		var lp *legPlan
		for _, l := range newCntlrPlan(f.srv.nf,
			forcedProvisioning.req()).legs {
			if l.legId == repNewLeg {
				lp = l
			}
		}
		if lp == nil || !lp.provisioning {
			t.Fatalf("the new leg is no provisioning leg of the plan")
		}
		nqn := legNqn(f.srv, repNewLeg)
		f.node.mu.Lock()
		f.node.plantSubsys(40, nqn,
			endpoint{trAddr: testIp, trSvcId: testSvcId})
		headNo := f.node.devNo[fmt.Sprintf("/dev/nvme%dn1",
			f.node.subsystems[nqn].idx)]
		f.node.mu.Unlock()
		if err := f.srv.dm.Create(f.ctx, lp.name, dmTable("linear",
			lp.sectors, linearArgs(headNo, 0))); err != nil {
			t.Fatalf("the new leg's wrapper: %v", err)
		}
	}
	for _, tc := range []struct {
		name  string
		base  forcedBase
		arm   func(t *testing.T, f *repFix)
		named bool
	}{
		{"no array of the group", forcedNone, nil, true},
		{"the named leg is not full", forcedNone,
			func(t *testing.T, f *repFix) {
				f.node.setSuperblock(f.leg2, fakeSuperblock{
					recovery: "1024 sectors", events: 10})
			}, false},
		{"the named leg is older than a member it zeroes", forcedNone,
			func(t *testing.T, f *repFix) {
				f.node.setSuperblock(f.leg2, fakeSuperblock{events: 10})
				f.node.setSuperblock(f.leg1, fakeSuperblock{events: 50})
			}, false},
		{"the named leg is late", forcedNone,
			func(t *testing.T, f *repFix) {
				f.legAna(testDataLeg2, "non-optimized")
			}, false},
		{"a member it zeroes is late", forcedNone,
			func(t *testing.T, f *repFix) {
				f.legAna(testDataLeg, "non-optimized")
			}, false},
		{"a member's superblock read fails", forcedNone,
			func(t *testing.T, f *repFix) {
				f.node.mu.Lock()
				f.node.failCmdAlways["readsb "+f.leg1] = ""
				f.node.mu.Unlock()
			}, false},
		{"a kept new leg still provisioning", forcedProvisioning, nil, false},
		{"a kept new leg still provisioning, its head and wrapper there",
			forcedProvisioning, provisioningLegBuilt, false},
		{"an array that runs", forcedRunning, nil, false},
		{"an inactive array of the group", forcedRunning,
			func(t *testing.T, f *repFix) {
				f.node.setArrayState(f.mdDev, "inactive")
			}, true},
		{"a broken array of the group", forcedRunning,
			func(t *testing.T, f *repFix) {
				f.node.setArrayState(f.mdDev, "broken")
			}, true},
		{"an array that holds another sp's leg", forcedRunning, stranger,
			false},
		{"a device above holds the array", forcedPooled,
			func(t *testing.T, f *repFix) {
				f.node.setArrayState(f.mdDev, "broken")
			}, false},
		{"the read of the group's array does not answer", forcedNone,
			func(t *testing.T, f *repFix) {
				f.unansweredArray()
			}, false},
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
			if tc.arm != nil {
				tc.arm(t, f)
			}
			ranBefore := f.dataArrayRuns()

			_, reply := f.read(t, rev)
			named := strings.Contains(reply.GetDetails(),
				forcedNamed(testDataLeg2))
			if strings.Contains(reply.GetDetails(),
				agent.LeftoverKindMdAbsent+":") {
				t.Fatalf("the read names the absent array beside a mark a "+
					"converge acts on: %q", reply.GetDetails())
			}
			f.node.Reset()
			f.syncupAny(t, req)
			started := !ranBefore && f.dataArrayRuns()
			if named != started {
				t.Fatalf("the read named the forced start: %v (%q); the "+
					"converge after it started the array: %v (%q)", named,
					reply.GetDetails(), started,
					f.node.callsMatching("cmd mdadm"))
			}
			if named != tc.named {
				t.Fatalf("the read named the forced start: %v, want %v (%q)",
					named, tc.named, reply.GetDetails())
			}
			if started && !f.node.hasCall(" --run --force") {
				t.Fatalf("the array started by no forced assembly: %q",
					f.node.callsMatching("cmd mdadm"))
			}
			if _, after := f.read(t, rev); started && strings.Contains(
				after.GetDetails(), "forced start of group") {
				t.Fatalf("the read after the start names the mark: %q",
					after.GetDetails())
			}
		})
	}
}

// TestAForcedStartIsNamedOnceItsMembersServe: a mark whose member to zero
// is late leaves the forced start waiting and the read names nothing; once
// the member's side flips the read names the forced start, the one
// SyncupCntlr the worker re-sends on that round zeroes the member, starts
// the array from the named leg and adds the member back, and the read after
// it names nothing, the mark waiting over the array that runs. The
// converge's own reply never names it.
func TestAForcedStartIsNamedOnceItsMembersServe(t *testing.T) {
	const rev = 4
	f := downNoRepair(t)
	f.legAna(testDataLeg, "non-optimized")
	held := withMark(quietLevel(rev), testDataLeg2)
	reply, err := f.srv.SyncupCntlr(f.ctx, held)
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	assertErrorDetails(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
		"forced start waits for leg 0x7", "grp data, leg 1 late")
	assertNoCall(t, f.node, "--zero-superblock")
	if strings.Contains(reply.GetAgentReply().GetDetails(),
		"forced start of group") {
		t.Fatalf("the converge's own reply names the forced start: %v",
			reply.GetAgentReply())
	}
	if _, read := f.read(t, rev); strings.Contains(read.GetDetails(),
		"forced start of group") {
		t.Fatalf("the read names the forced start while a member it "+
			"zeroes is late: %q", read.GetDetails())
	}

	f.legAna(testDataLeg, "optimized")
	round := rowRound(t, f.srv, rev)
	if round.GetAgentReply().GetCode() != common.ReplyCodeLeftover ||
		!strings.Contains(round.GetAgentReply().GetDetails(),
			forcedNamed(testDataLeg2)) {
		t.Fatalf("the round after the flip is %v, want the forced start "+
			"named", round.GetAgentReply())
	}
	f.node.Reset()
	if rowResend(t, f.srv, held, round) == nil {
		t.Fatalf("the round that names the forced start brought no re-send")
	}
	assertOrder(t, f.node,
		examineOf(f.leg2),
		zeroOf(f.leg1),
		f.forced(f.leg2, mdUpdateUuid),
		f.add(f.leg1))
	if _, read := f.read(t, rev); strings.Contains(read.GetDetails(),
		"forced start of group") {
		t.Fatalf("the read after the re-send names the forced start: %q",
			read.GetDetails())
	}
}

// TestNoSuperblockIsReadThroughAWrapperAConvergeWouldChange pins the read's
// leg test for both names (readAvailable): while leg 2's wrapper is absent,
// carries another table, maps another number than the head's, is read-only
// or is suspended, a read reads no superblock through it — a read through a
// suspended wrapper would wait for its resume, and one through a wrapper over
// another number would read another device — and names neither the absent
// array nor the forced start, but the wrapper, whose own name brings the
// converge; that converge mends the wrapper and starts the array. Both legs'
// superblocks count both members, so the assembly from leg 1 alone is one
// mdadm's degraded gate refuses.
func TestNoSuperblockIsReadThroughAWrapperAConvergeWouldChange(t *testing.T) {
	const rev = 4
	for _, item := range []struct {
		name    string
		req     func() *pb.SyncupCntlrRequest
		blocked func(f *repFix) string
		names   string
	}{
		{"the absent array", func() *pb.SyncupCntlrRequest {
			return quietReq(rev)
		}, func(f *repFix) string { return f.leg1 },
			agent.LeftoverKindMdAbsent + ":"},
		{"the forced start", func() *pb.SyncupCntlrRequest {
			return withMark(quietLevel(rev), testDataLeg2)
		}, func(f *repFix) string { return f.leg2 },
			"forced start of group"},
	} {
		for _, shape := range rowWrapperShapes {
			t.Run(item.name+", leg 2's wrapper "+shape.name, func(t *testing.T) {
				f := downNoRepair(t)
				req := item.req()
				// The converge that leaves the array absent: its first
				// superblock read fails once.
				blocker := "readsb " + item.blocked(f)
				f.node.mu.Lock()
				f.node.failCmd[blocker] = ""
				f.node.mu.Unlock()
				f.syncupAny(t, req)
				f.node.mu.Lock()
				delete(f.node.failCmd, blocker)
				f.node.mu.Unlock()
				f.node.setSuperblock(f.leg1,
					fakeSuperblock{events: 10, arrayState: "AA"})
				f.node.setSuperblock(f.leg2, fakeSuperblock{
					role: "Active device 1", events: 10, arrayState: "AA"})
				wrapper := legName(f.srv, testDataLeg2)
				shape.arm(t, f.node, wrapper)

				f.node.Reset()
				_, reply := f.read(t, rev)
				if got := f.node.callsMatching("readsb " + f.leg2); len(got) != 0 {
					t.Fatalf("the read read leg 2's superblock through its "+
						"wrapper: %q", got)
				}
				if strings.Contains(reply.GetDetails(), item.names) {
					t.Fatalf("the read names the group: %q", reply.GetDetails())
				}
				if !strings.Contains(reply.GetDetails(),
					rowNamed(wrapper, shape.what)) {
					t.Fatalf("the read does not name the wrapper %q: %q",
						shape.what, reply.GetDetails())
				}
				f.node.Reset()
				f.syncupAny(t, req)
				if !f.dataArrayRuns() {
					t.Fatalf("the converge the wrapper's name brings left no "+
						"array running: %q", f.node.callsMatching("cmd "))
				}
			})
		}
	}
}

// TestAStoredMarkIsNamedAsStoredNotAsAForcedStart pins the delivered term of
// the two names (actsOnMark): after a restart whose startup reconcile loaded
// a request whose mark names leg 2, a request that does not count as
// delivered, with no array of the group and both legs serving, the read
// names the absent array, whose plain assembly is what a converge of that
// request runs, beside the stored mark (storedMarks), and names no forced
// start, which no converge of that request runs. The SyncupCntlr the worker
// re-sends on that round counts as delivered and acts on the mark: it starts
// the array from leg 2, and the read after it names neither.
func TestAStoredMarkIsNamedAsStoredNotAsAForcedStart(t *testing.T) {
	const rev = 4
	f := downNoRepair(t)
	req := withMark(quietReq(rev), testDataLeg2)
	path := f.srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr)
	if err := f.srv.store.Save(f.ctx, path, req); err != nil {
		t.Fatalf("saving the marked request: %v", err)
	}
	// The startup reconcile's assembly reads leg 1 first; its read fails
	// once, so the reconcile leaves the array absent.
	blocker := "readsb " + f.leg1
	f.node.mu.Lock()
	f.node.failCmd[blocker] = ""
	f.node.mu.Unlock()
	fresh := newCnServer(f.node)
	fresh.retryInterval = time.Hour
	reconcileForTest(t, fresh)
	f.srv = fresh
	f.node.mu.Lock()
	delete(f.node.failCmd, blocker)
	f.node.mu.Unlock()
	if f.dataArrayRuns() {
		t.Fatalf("the case is vacuous: the startup reconcile started the " +
			"array")
	}
	if st := f.srv.getCntlr(cntlrKey(testCluster, testCn, testSp,
		testCntlr)); st == nil || st.reqFromRpc {
		t.Fatalf("the held request counts as delivered after the reconcile")
	}

	round := rowRound(t, f.srv, rev)
	details := round.GetAgentReply().GetDetails()
	if !strings.Contains(details, f.absentNamed()) ||
		!strings.Contains(details, "forced start of group 0x6: the mark "+
			"comes from the stored request") ||
		strings.Contains(details, forcedNamed(testDataLeg2)) {
		t.Fatalf("the read names %q, want the absent array and the stored "+
			"mark, and no forced start", details)
	}
	f.node.Reset()
	if rowResend(t, f.srv, req, round) == nil {
		t.Fatalf("the round that names the stored mark brought no re-send")
	}
	if !f.node.hasCall(f.forced(f.leg2, mdUpdateUuid)) || !f.dataArrayRuns() {
		t.Fatalf("the re-send did not start the array from leg 2: %q",
			f.node.callsMatching("cmd mdadm"))
	}
	if after := rowRound(t, f.srv, rev).GetAgentReply().
		GetDetails(); strings.Contains(after, agent.LeftoverKindMdAbsent+
		":") || strings.Contains(after, "forced start of group") {
		t.Fatalf("the read after the re-send names %q", after)
	}
}

// blockedAbsent is downNoRepair promoted by quietReq(rev) with the data
// group's array left absent: the promotion's first superblock read fails
// once. Every prober row reads PENDING then, so a read's leg loop looks up
// every leg.
func blockedAbsent(t *testing.T, rev uint64) *repFix {
	t.Helper()
	f := downNoRepair(t)
	blocker := "readsb " + f.leg1
	f.node.mu.Lock()
	f.node.failCmd[blocker] = ""
	f.node.mu.Unlock()
	f.syncupAny(t, quietReq(rev))
	f.node.mu.Lock()
	delete(f.node.failCmd, blocker)
	f.node.mu.Unlock()
	if f.dataArrayRuns() {
		t.Fatalf("the case is vacuous: the promotion started the array")
	}
	return f
}

// legReads are how many times the calls read leg legId's subsystem name,
// the first read of a lookup of it (lookupSubsys), and its multipath head's
// number in /sys/block (blockDevNo).
func (f *repFix) legReads(legId uint64) (lookups, numbers int) {
	f.node.mu.Lock()
	idx := f.node.subsystems[legNqn(f.srv, legId)].idx
	f.node.mu.Unlock()
	lookups = len(f.node.callsMatching(fmt.Sprintf(
		"read %s/nvme-subsys%d/subsysnqn", sysfsNvmeSubsysDir, idx)))
	numbers = len(f.node.callsMatching(fmt.Sprintf(
		"read %s/nvme%dn1/dev", sysfsBlockDir, idx)))
	return lookups, numbers
}

// TestAGroupNameLooksUpAgainOnlyALegThatServed pins what a read's group
// names cost each member (readAvailable; cnagent.md CN28): a member whose
// lookup in the leg loop found a path both live and optimized is looked up
// once more and its head's number read once more, before its superblock
// read; one whose path did not serve costs neither, its only lookup and
// number read the leg loop's; and the meta group, whose array runs, costs
// nothing beyond the leg loop. Each subsystem's name is also read once by
// the verdict's own listing of the node's connections (CN30), so each
// member is counted against the meta group's leg.
func TestAGroupNameLooksUpAgainOnlyALegThatServed(t *testing.T) {
	const rev = 4
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprintf("leg 2 late %v", late), func(t *testing.T) {
			f := blockedAbsent(t, rev)
			if late {
				f.legAna(testDataLeg2, "non-optimized")
			}
			f.node.Reset()
			f.read(t, rev)
			base, metaNumbers := f.legReads(testMetaLeg)
			if metaNumbers != 1 {
				t.Fatalf("the meta leg's head number read %d times, want "+
					"the leg loop's one", metaNumbers)
			}
			want := map[uint64][2]int{
				testDataLeg:  {base + 1, 2},
				testDataLeg2: {base + 1, 2},
			}
			if late {
				want[testDataLeg2] = [2]int{base, 1}
			}
			for legId, w := range want {
				lookups, numbers := f.legReads(legId)
				if lookups != w[0] || numbers != w[1] {
					t.Errorf("leg %#x: %d lookups and %d number reads, "+
						"want %d and %d", legId, lookups, numbers, w[0], w[1])
				}
			}
			if got := f.node.callsMatching("readsb " + f.leg2); (len(got) ==
				0) != late {
				t.Errorf("leg 2's superblock reads: %q, with leg 2 late %v",
					got, late)
			}
		})
	}
}

// TestNoSuperblockIsReadThroughAPathThatStoppedServing pins the fresh
// lookup a group's name takes before its superblock reads (readAvailable):
// leg 2's path turns inaccessible after the read's leg loop found it
// serving, while the read probes the meta group, and the data group's name
// then reads no superblock through it and reads its head's number no more,
// the leg loop's read alone.
func TestNoSuperblockIsReadThroughAPathThatStoppedServing(t *testing.T) {
	const rev = 4
	f := blockedAbsent(t, rev)
	metaNode := f.node.mdNode(f.srv.nf.MdPath(f.srv.nf.CnMdDevName(
		testCluster, testCn, testSp, 0, 0, true)))
	if metaNode == "" {
		t.Fatalf("the meta group's array does not run")
	}
	nqn := legNqn(f.srv, testDataLeg2)
	const flip = "FLIP leg 2 inaccessible"
	f.node.mu.Lock()
	f.node.afterRead[sysfsBlockDir+strings.TrimPrefix(metaNode, "/dev")+
		"/md/degraded"] = func() {
		// Called with the fake's lock held: setAnaState's body.
		f.node.anaOf[nqn+"|"+testIp2+":"+testSvcId2] = "inaccessible"
		for _, ctrl := range f.node.subsystems[nqn].ctrls {
			if ctrl.trAddr == testIp2 && ctrl.trSvcId == testSvcId2 {
				ctrl.anaState = "inaccessible"
				f.node.files[sysfsNvmeCtrlDir+"/"+ctrl.name+"/"+
					ctrl.pathDev+"/ana_state"] = "inaccessible\n"
			}
		}
		f.node.record("%s", flip)
	}
	f.node.mu.Unlock()
	f.node.Reset()
	f.read(t, rev)
	calls := f.node.Calls()
	flipAt := slices.Index(calls, flip)
	if flipAt < 0 {
		t.Fatalf("the case is vacuous: no read of the meta array's degraded")
	}
	for _, call := range calls[flipAt:] {
		if strings.HasPrefix(call, "readsb "+f.leg2) {
			t.Fatalf("the read read leg 2's superblock after its path " +
				"turned inaccessible")
		}
	}
	if _, numbers := f.legReads(testDataLeg2); numbers != 1 {
		t.Fatalf("leg 2's head number read %d times, want the leg loop's "+
			"one", numbers)
	}
}
