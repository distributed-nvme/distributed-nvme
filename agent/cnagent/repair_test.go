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

// The repair fixture: the two-leg raid1 data group of cntlrReq, whose leg 1
// (testDataLeg) is the bad leg of one repair and whose new leg is a fresh leg
// with one side (architecture.md, Repair; cnagent.md CN12).
const (
	repName    = "rep-1"
	repId      = uint64(0x7d)
	repNewLeg  = uint64(0x7b)
	repNewSide = uint64(0x7c)
)

// repOpts shapes repairReq beyond its stage and its abort flag.
type repOpts struct {
	// standby converges the request on a standby.
	standby bool
	// unprovisioned leaves the new leg's side provisioned = false.
	unprovisioned bool
	// level is the request's sp_level.
	level pb.SpLevel
	// noSubsys gives the request no subsystem, so that no namespace waits
	// for the pool and registers its own retry (CN16).
	noSubsys bool
}

// repairReq is the two-leg raid1 request whose data group has a repair of
// the given stage, with the abort flag set or clear, naming testDataLeg as
// its bad leg.
func repairReq(
	revision uint64,
	stage pb.RepStage,
	abort bool,
	opts ...repOpts,
) *pb.SyncupCntlrRequest {
	var o repOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	var subsys map[string]*pb.Subsystem
	if o.noSubsys {
		subsys = map[string]*pb.Subsystem{}
	}
	req := cntlrReq(reqOpts{revision: revision, primary: !o.standby,
		raid1: true, twoLegs: true, level: o.level, subsys: subsys})
	side := sideOf(repNewSide, testIp, testSvcId)
	side.Provisioned = !o.unprovisioned
	newLeg := legOf(repNewLeg, side)
	newLeg.LegIdx = 2
	dataGrpOf(req).RepName = repName
	req.NameToRepair = map[string]*pb.Repair{repName: {
		RepId:        repId,
		GrpId:        testDataGrp,
		RepStage:     stage,
		NewLegList:   []*pb.Leg{newLeg},
		BadLegIdList: []uint64{testDataLeg},
		TryToAbort:   abort,
		SliceId:      testSlice,
	}}
	return req
}

// noRepairReq is repairReq's request with no repair: the pool before the
// create, or after a finish or a cancel took the repair away.
func noRepairReq(revision uint64) *pb.SyncupCntlrRequest {
	return cntlrReq(reqOpts{revision: revision, primary: true, raid1: true,
		twoLegs: true})
}

func dataGrpOf(req *pb.SyncupCntlrRequest) *pb.Group {
	return req.GetIdToSlice()[fmt.Sprintf(common.IdKeyFmt, testSlice)].
		GetDataGrpList()[0]
}

// repFix is one repair case's server and the paths it asserts on.
type repFix struct {
	srv                   *CnAgentServer
	node                  *fakeNode
	ctx                   context.Context
	mdDev                 string
	leg1, leg2, newLegDev string
}

// newRepFix converges the pool with no repair at revision 2, its arrays
// created over fresh legs, and completes one probe round of the listed
// legs, whose rows then read OK; every other leg's row reads PENDING (CN11).
func newRepFix(t *testing.T, probed ...uint64) *repFix {
	t.Helper()
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	f := &repFix{
		srv:  srv,
		node: node,
		ctx:  context.Background(),
		mdDev: srv.nf.MdPath(srv.nf.CnMdDevName(
			testCluster, testCn, testSp, 0, 0, false)),
		leg1:      srv.nf.DmPath(legName(srv, testDataLeg)),
		leg2:      srv.nf.DmPath(legName(srv, testDataLeg2)),
		newLegDev: srv.nf.DmPath(legName(srv, repNewLeg)),
	}
	if _, err := srv.SyncupCn(f.ctx, cnReq(2, true)); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	f.syncup(t, noRepairReq(2))
	probeLegs(t, srv, probed...)
	return f
}

// syncup converges one request, which the agent must accept.
func (f *repFix) syncup(
	t *testing.T,
	req *pb.SyncupCntlrRequest,
) *pb.CntlrInfo {
	t.Helper()
	reply, err := f.srv.SyncupCntlr(f.ctx, req)
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if code := reply.GetAgentReply().GetCode(); code != 0 {
		t.Fatalf("SyncupCntlr: code %d (%s)", code,
			reply.GetAgentReply().GetDetails())
	}
	return reply.GetCntlrInfo()
}

// read is one read of the fixture cntlr and the check round that answers
// from it: its info and its verdict.
func (f *repFix) read(
	t *testing.T,
	revision uint64,
) (*pb.CntlrInfo, *pb.AgentReply) {
	t.Helper()
	refreshTestCntlr(t, f.srv)
	reply, info := f.srv.checkCntlrRound(f.ctx, &pb.CheckCntlrRequest{
		ClusterId: testCluster, CnId: testCn,
		CntlrPointer: cntlrPtr(), Revision: revision,
	}, nil)
	return info, reply.GetAgentReply()
}

// mdCalls are the mdadm commands the node ran against the data group's
// array since its last Reset.
func (f *repFix) mdCalls() []string {
	return f.node.callsMatching("cmd mdadm " + f.mdDev + " ")
}

func (f *repFix) fail(path string) string {
	return "cmd mdadm " + f.mdDev + " --fail " + path
}

func (f *repFix) remove(path string) string {
	return "cmd mdadm " + f.mdDev + " --remove " + path
}

func (f *repFix) add(path string) string {
	return "cmd mdadm " + f.mdDev + " --add --failfast " + path
}

// members are the dm paths the data group's array holds.
func (f *repFix) members() []string {
	f.node.mu.Lock()
	defer f.node.mu.Unlock()
	return slices.Sorted(slices.Values(f.node.arrays[f.mdDev].members))
}

// dataMdDir is the md/ directory of the data group's array in sysfs.
func (f *repFix) dataMdDir() string {
	return sysfsBlockDir + "/" + strings.TrimPrefix(
		f.node.mdNode(f.mdDev), "/dev/") + "/md"
}

// assertMdCalls checks the exact mdadm commands against the array.
func (f *repFix) assertMdCalls(t *testing.T, label string, want ...string) {
	t.Helper()
	if got := f.mdCalls(); !slices.Equal(got, want) {
		t.Fatalf("%s: mdadm ran\n%s\nwant\n%s", label,
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// legDead makes a leg's path stop serving, as its side's death leaves it
// on an array with no IO: the controller reconnecting, the leg not
// available (CN12).
func (f *repFix) legDead(legId uint64, addr, svcId string) {
	f.node.setCtrlState(legNqn(f.srv, legId), addr, svcId, "connecting")
}

func (f *repFix) badLegDead() { f.legDead(testDataLeg, testIp, testSvcId) }

// newLegUnreachable makes every connect to the new leg's side fail, so its
// leg is not available (CN10, CN12).
func (f *repFix) newLegUnreachable() {
	f.node.mu.Lock()
	defer f.node.mu.Unlock()
	f.node.failCmdAlways["--nqn "+legNqn(f.srv, repNewLeg)+" --hostnqn"] =
		"nvme connect: Connection refused"
}

// copying plants md copying onto a member: a member without in_sync that
// holds a place, the array degraded and recovering.
func (f *repFix) copying(member, slot string) {
	f.node.setMdSync(f.mdDev, 1, "recover", "100 / 200")
	f.node.setMemberState(f.mdDev, member, "spare,failfast")
	f.node.setMemberSlot(f.mdDev, member, slot)
}

// verdict is the info's verdict on the fixture repair, nil when it gives
// none.
func verdict(info *pb.CntlrInfo) *pb.CntlrInfo_RepairInfo {
	return info.GetRepIdToRepairInfo()[repId]
}

// assertVerdict checks the info's verdict on the fixture repair.
func assertVerdict(
	t *testing.T,
	info *pb.CntlrInfo,
	label string,
	started, confirmed bool,
) {
	t.Helper()
	got := verdict(info)
	if got == nil {
		t.Fatalf("%s: no verdict on the repair: %v", label,
			info.GetRepIdToRepairInfo())
	}
	if got.GetStarted() != started || got.GetAbortConfirmed() != confirmed {
		t.Fatalf("%s: verdict started %v abort confirmed %v, want %v %v",
			label, got.GetStarted(), got.GetAbortConfirmed(), started,
			confirmed)
	}
}

// assertNoReport checks that an info carries neither a repair verdict nor a
// group reading.
func assertNoReport(t *testing.T, info *pb.CntlrInfo, label string) {
	t.Helper()
	if n := len(info.GetRepIdToRepairInfo()); n != 0 {
		t.Fatalf("%s: %d repair verdicts, want none: %v", label, n,
			info.GetRepIdToRepairInfo())
	}
	if n := len(info.GetGrpIdToGrpInfo()); n != 0 {
		t.Fatalf("%s: %d group readings, want none: %v", label, n,
			info.GetGrpIdToGrpInfo())
	}
}

// assertReading checks the info's reading of the data group's array.
func assertReading(
	t *testing.T,
	info *pb.CntlrInfo,
	label string,
	whole bool,
	inSync ...uint64,
) {
	t.Helper()
	got := info.GetGrpIdToGrpInfo()[testDataGrp]
	if got == nil {
		t.Fatalf("%s: no reading of the data group: %v", label,
			info.GetGrpIdToGrpInfo())
	}
	if got.GetWhole() != whole ||
		!slices.Equal(got.GetInSyncLegIdList(), inSync) {
		t.Fatalf("%s: reading whole %v in sync %v, want %v %v", label,
			got.GetWhole(), got.GetInSyncLegIdList(), whole, inSync)
	}
}

// probeLegs completes one successful probe round of each listed leg's
// prober, as probeAllLegs does for every leg: those legs' rows then read OK,
// and the other legs' rows stay as they are.
func probeLegs(t *testing.T, srv *CnAgentServer, legIds ...uint64) {
	t.Helper()
	st := legState(t, srv)
	srv.mu.Lock()
	var probers []*legProber
	for _, prober := range st.probers {
		if slices.Contains(legIds, prober.legId) {
			probers = append(probers, prober)
		}
	}
	srv.mu.Unlock()
	if len(probers) != len(legIds) {
		t.Fatalf("probers for %d of the legs %v", len(probers), legIds)
	}
	for _, prober := range probers {
		srv.runLegProbe(context.Background(), prober, testCn)
		if _, completed, err := prober.snapshot(); !completed || err != nil {
			t.Fatalf("leg %d: probe round completed=%v err=%v",
				prober.legId, completed, err)
		}
	}
}

// allLegs are the fixture's legs before the repair.
var allLegs = []uint64{testMetaLeg, testDataLeg, testDataLeg2}

// TestRepairNewLegIsConnectedAndProbed pins how every cntlr carries a
// repair's new leg (cnagent.md CN10, CN12): it is connected, wrapped,
// probed and reported as a spare is, the sweep keeps it, it is no member
// until the add's conditions hold, it never defers its group, and it is
// never a late member. Here the bad leg serves with an OK row, so the
// repair waits: no mdadm command runs against the array, and the verdict
// says not started. A standby connects the new leg too and reports neither
// a verdict nor a reading.
func TestRepairNewLegIsConnectedAndProbed(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.node.Reset()
	info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
	if !f.node.hasCall("cmd dmsetup create " + legName(f.srv, repNewLeg)) {
		t.Fatalf("the new leg was not wrapped")
	}
	assertPending(t, info.GetLegIdToLeg()[repNewLeg], "the new leg's row")
	f.assertMdCalls(t, "the waiting repair")
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	assertVerdict(t, info, "the waiting repair", false, false)
	assertReading(t, info, "the waiting repair", true,
		testDataLeg, testDataLeg2)
	if retrying(t, f.srv) {
		t.Fatalf("the new leg registered the late-member retry")
	}

	// An equal-revision converge keeps the new leg: its wrapper and its
	// connection are in the sweep's wanted set.
	f.node.Reset()
	f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
	assertNoCall(t, f.node, "cmd dmsetup remove "+legName(f.srv, repNewLeg))
	assertNoCall(t, f.node, "--nqn "+legNqn(f.srv, repNewLeg))
	probeLegs(t, f.srv, repNewLeg)
	read, reply := f.read(t, 3)
	assertOk(t, read.GetLegIdToLeg()[repNewLeg], "the new leg's row, read")
	assertVerdict(t, read, "the waiting repair, read", false, false)
	if reply.GetCode() != 0 {
		t.Fatalf("the read of the waiting repair: %v, want a clean verdict",
			reply)
	}

	// A standby connects and wraps the new leg too, and reports neither a
	// verdict nor a reading: it builds no group.
	f.node.Reset()
	info = f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_CREATED, false,
		repOpts{standby: true}))
	if info.GetLegIdToLeg()[repNewLeg] == nil {
		t.Fatalf("the standby reports no row for the new leg")
	}
	assertNoCall(t, f.node, "cmd dmsetup remove "+legName(f.srv, repNewLeg))
	assertNoReport(t, info, "the standby")
}

// TestRepairUnprovisionedNewLegDefersOnlyItself: a new leg whose side still
// zeroes is provisioning, as a spare is (cnagent.md CN9): its row reads
// PROVISIONING, nothing connects to it, and the group is not deferred — its
// row reads OK and the bad leg, though it does not serve, is not failed,
// since no new leg can follow.
func TestRepairUnprovisionedNewLegDefersOnlyItself(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.badLegDead()
	f.node.Reset()
	info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false,
		repOpts{unprovisioned: true}))
	assertProvisioning(t, info.GetLegIdToLeg()[repNewLeg], "the new leg")
	assertNoCall(t, f.node, "--nqn "+legNqn(f.srv, repNewLeg))
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	f.assertMdCalls(t, "a provisioning new leg")
	assertVerdict(t, info, "a provisioning new leg", false, false)
}

// TestRepairOfAnotherGroupIsNone: a group whose rep_name names a repair of
// another group has no repair (cnagent.md CN12), and since no group's
// rep_name links the repair to the group it names, neither has that group.
// Here the data group names a repair of the meta group whose add would run
// at once were it the data group's own — its bad leg dead, its new leg
// reachable — and nothing of the repair happens: no mdadm --add, --fail or
// --remove, no verdict, and its new leg neither connected nor wrapped.
func TestRepairOfAnotherGroupIsNone(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.badLegDead()
	req := repairReq(3, pb.RepStage_REP_STAGE_CREATED, false)
	req.GetNameToRepair()[repName].GrpId = testMetaGrp
	f.node.Reset()
	info := f.syncup(t, req)
	for _, verb := range []string{" --add ", " --fail ", " --remove "} {
		if got := f.node.callsMatching(verb); len(got) != 0 {
			t.Fatalf("a repair of another group ran %q", got)
		}
	}
	if n := len(info.GetRepIdToRepairInfo()); n != 0 {
		t.Fatalf("%d repair verdicts, want none: %v", n,
			info.GetRepIdToRepairInfo())
	}
	assertNoCall(t, f.node, "--nqn "+legNqn(f.srv, repNewLeg))
	assertNoCall(t, f.node, "cmd dmsetup create "+legName(f.srv, repNewLeg))
	if row := info.GetLegIdToLeg()[repNewLeg]; row != nil {
		t.Fatalf("a row for the new leg of a repair of another group: %v",
			row)
	}
}

// TestRepairAddsTheNewLeg pins CN12's add of a repair's new leg. For a
// CREATED repair whose request carries no abort flag, in a converge where
// the array runs, the new leg is added once it is available and another
// member keeps the data: one md marks in sync, of a leg_list leg other than
// the bad leg, available with an OK row. The bad leg goes first: failed and
// removed when it cannot take a member (it does not serve, or its row is not
// OK), removed when md has moved it out of its place already, left alone when
// md does not hold it; and when it serves with an OK row it stays and the
// repair waits. The bad leg is failed only in a pass whose add's conditions
// hold, so a case whose new leg cannot be added runs no mdadm at all. A
// STARTED repair's new leg the array lacks is added as a leg_list member is;
// an aborting or aborted repair's is never added. Each case reads the
// converge's own verdict, from the array as the converge left it.
func TestRepairAddsTheNewLeg(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage pb.RepStage
		abort bool
		// probed are the legs whose probers complete a round before the
		// repair's converge; the others read PENDING.
		probed []uint64
		set    func(f *repFix)
		// want builds the mdadm commands the converge runs.
		want func(f *repFix) []string
		// started and confirmed are the converge's verdict.
		started, confirmed bool
	}{
		{"bad leg does not serve", pb.RepStage_REP_STAGE_CREATED, false,
			allLegs, (*repFix).badLegDead,
			func(f *repFix) []string {
				return []string{f.fail(f.leg1), f.remove(f.leg1),
					f.add(f.newLegDev)}
			}, true, false},
		{"bad leg's row not ok", pb.RepStage_REP_STAGE_CREATED, false,
			[]uint64{testMetaLeg, testDataLeg2}, nil,
			func(f *repFix) []string {
				return []string{f.fail(f.leg1), f.remove(f.leg1),
					f.add(f.newLegDev)}
			}, true, false},
		{"bad leg out of its place", pb.RepStage_REP_STAGE_CREATED, false,
			allLegs, func(f *repFix) { failOutOfSlot(f.node, f.mdDev, f.leg1) },
			func(f *repFix) []string {
				return []string{f.remove(f.leg1), f.add(f.newLegDev)}
			}, true, false},
		{"bad leg not held", pb.RepStage_REP_STAGE_CREATED, false,
			allLegs, func(f *repFix) {
				f.node.mu.Lock()
				f.node.mdRemove([]string{f.mdDev, "--remove", f.leg1})
				f.node.mu.Unlock()
			},
			func(f *repFix) []string {
				return []string{f.add(f.newLegDev)}
			}, true, false},
		{"bad leg serves with an ok row", pb.RepStage_REP_STAGE_CREATED,
			false, allLegs, nil, nil, false, false},
		{"new leg not available", pb.RepStage_REP_STAGE_CREATED, false,
			allLegs, func(f *repFix) {
				f.badLegDead()
				f.newLegUnreachable()
			}, nil, false, false},
		{"leg 2 copying", pb.RepStage_REP_STAGE_CREATED, false, allLegs,
			func(f *repFix) {
				f.badLegDead()
				f.copying(f.leg2, "1")
			}, nil, false, false},
		{"leg 2 does not serve", pb.RepStage_REP_STAGE_CREATED, false,
			allLegs, func(f *repFix) {
				f.badLegDead()
				f.legDead(testDataLeg2, testIp2, testSvcId2)
			}, nil, false, false},
		{"leg 2's row not ok", pb.RepStage_REP_STAGE_CREATED, false,
			[]uint64{testMetaLeg, testDataLeg}, (*repFix).badLegDead, nil,
			false, false},
		{"abort flag set", pb.RepStage_REP_STAGE_CREATED, true, allLegs,
			(*repFix).badLegDead, nil, false, true},
		{"aborted", pb.RepStage_REP_STAGE_ABORTED, true, allLegs,
			(*repFix).badLegDead, nil, false, false},
		// A STARTED repair's bad leg is out of the array in production;
		// the case leaves it in to show that the add of a STARTED repair's
		// new leg never touches it.
		{"started", pb.RepStage_REP_STAGE_STARTED, false, allLegs,
			(*repFix).badLegDead,
			func(f *repFix) []string {
				return []string{f.add(f.newLegDev)}
			}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepFix(t, tc.probed...)
			if tc.set != nil {
				tc.set(f)
			}
			before := f.members()
			f.node.Reset()
			info := f.syncup(t, repairReq(3, tc.stage, tc.abort))
			var want []string
			if tc.want != nil {
				want = tc.want(f)
			}
			f.assertMdCalls(t, "the repair's converge", want...)
			assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
			if want == nil {
				if after := f.members(); !slices.Equal(after, before) {
					t.Fatalf("the array's members changed to %v, want %v",
						after, before)
				}
			}
			assertVerdict(t, info, "the converge", tc.started, tc.confirmed)
		})
	}
}

// TestRepairBadLegIsNotLate: an open repair's bad leg that does not serve
// registers no late-member retry while the group's array runs, since no
// converge adds it back to a running array while the repair is open, while
// a dead leg_list leg that is no bad leg does, and so does the bad leg once
// the repair is aborted (cnagent.md CN10, CN12). While the array does not
// run, the bad leg is late as any other leg_list member is: the assembly
// takes it once it is available, so the retry that waits for it is what
// assembles the array when mdadm refuses to start it from leg 2 alone.
func TestRepairBadLegIsNotLate(t *testing.T) {
	t.Run("array absent", func(t *testing.T) {
		for _, stage := range []pb.RepStage{pb.RepStage_REP_STAGE_CREATED,
			pb.RepStage_REP_STAGE_STARTED} {
			t.Run(stage.String(), func(t *testing.T) {
				testRepairBadLegLateWhileArrayAbsent(t, stage)
			})
		}
	})
	for _, tc := range []struct {
		name  string
		stage pb.RepStage
		leg2  bool // leg 2 does not serve either
		late  bool
	}{
		{"created", pb.RepStage_REP_STAGE_CREATED, false, false},
		{"started", pb.RepStage_REP_STAGE_STARTED, false, false},
		{"aborted", pb.RepStage_REP_STAGE_ABORTED, false, true},
		{"leg 2 dead too", pb.RepStage_REP_STAGE_CREATED, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			f.badLegDead()
			if tc.leg2 {
				f.legDead(testDataLeg2, testIp2, testSvcId2)
			}
			f.syncup(t, repairReq(3, tc.stage,
				tc.stage == pb.RepStage_REP_STAGE_ABORTED))
			if got := retrying(t, f.srv); got != tc.late {
				t.Fatalf("the late-member retry is %v, want %v", got, tc.late)
			}
		})
	}
}

// testRepairBadLegLateWhileArrayAbsent is TestRepairBadLegIsNotLate's case
// of an array that does not run, in the shape
// TestLateMemberRefusedStartIsAssembledByTheRetry plays: a demote stops the
// array with both superblocks still counting both members, and the
// promotion's first converge finds the bad leg connected while its path is
// not optimized, so case 2 starts the array from leg 2 alone, which is
// refused (injected, as there, for as long as the bad leg is late), and the
// bad leg registers the retry. The request has no subsystem, so no waiting
// namespace registers the retry instead. The attempt after the bad leg's
// path reads optimized assembles the array from both legs and stops the
// retry, and a converge that finds that path not optimized again while the
// array runs registers none.
func testRepairBadLegLateWhileArrayAbsent(t *testing.T, stage pb.RepStage) {
	f := newRepFix(t, allLegs...)
	noSubsys := repOpts{noSubsys: true}
	f.syncup(t, repairReq(3, stage, false,
		repOpts{standby: true, noSubsys: true}))
	nqn1 := legNqn(f.srv, testDataLeg)
	f.node.setAnaState(nqn1, testIp, testSvcId, "non-optimized")
	start := "mdadm --assemble " + f.mdDev + " "
	lone := "cmd " + start + "--name " +
		f.srv.nf.CnMdArrayName(testSp, 0, 0, false) + " " + f.leg2
	both := "cmd " + start + "--name " +
		f.srv.nf.CnMdArrayName(testSp, 0, 0, false) + " " + f.leg1 + " " +
		f.leg2
	f.node.mu.Lock()
	f.node.failCmdAlways[start] = "mdadm: " + f.mdDev + " assembled from 1 " +
		"drive - need 2 to start (use --run to insist)."
	f.node.mu.Unlock()

	f.node.Reset()
	reply, err := f.srv.SyncupCntlr(f.ctx, repairReq(4, stage, false, noSubsys))
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	assertErrorDetails(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
		"need 2 to start", "grp data, bad leg late")
	if got := f.node.callsMatching("cmd " + start); !slices.Equal(got,
		[]string{lone}) {
		t.Fatalf("want one start of the data array from leg 2 alone, "+
			"exactly %q, got %q", lone, got)
	}
	if !retrying(t, f.srv) {
		t.Fatalf("a refused start beside a bad leg that is not available " +
			"registered no retry: nothing else would assemble the array")
	}

	f.node.mu.Lock()
	delete(f.node.failCmdAlways, start)
	f.node.mu.Unlock()
	f.node.setAnaState(nqn1, testIp, testSvcId, "optimized")
	f.node.Reset()
	retryAttempt(t, f.srv)
	if got := f.node.callsMatching("cmd " + start); !slices.Equal(got,
		[]string{both}) {
		t.Fatalf("want one assembly of the data array from both legs, "+
			"exactly %q, got %q", both, got)
	}
	if retrying(t, f.srv) {
		t.Fatalf("the retry outlived the bad leg's lateness")
	}

	// The array runs: the same bad leg, its path not optimized again, is
	// no late member.
	f.node.setAnaState(nqn1, testIp, testSvcId, "non-optimized")
	f.syncup(t, repairReq(5, stage, false, noSubsys))
	if retrying(t, f.srv) {
		t.Fatalf("the bad leg of a running array registered the " +
			"late-member retry")
	}
}

// TestRepairUnavailableNewLegIsNotLate: a repair's new leg that the cntlr
// has connected and that then stops serving registers no late-member retry,
// at either open stage (cnagent.md CN10, CN12): it is no leg_list member,
// and the read's verdict names it once its add can run.
func TestRepairUnavailableNewLegIsNotLate(t *testing.T) {
	for _, stage := range []pb.RepStage{pb.RepStage_REP_STAGE_CREATED,
		pb.RepStage_REP_STAGE_STARTED} {
		t.Run(stage.String(), func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
			f.legDead(repNewLeg, testIp, testSvcId)
			f.syncup(t, repairReq(4, stage, false))
			if retrying(t, f.srv) {
				t.Fatalf("the new leg that does not serve registered the " +
					"late-member retry")
			}
		})
	}
}

// TestRepairRereadThatFailsReportsNothing: the converge reads the array
// again after its own member changes, for its verdict and its reading; a
// read again that does not answer turns no row ERROR, since the
// reconciliation itself went through — neither the group's nor, since it
// leaves the pass's md walk as it was, the slice's concat, pool and thin rows
// that take the array's number from that walk — and the converge reports
// neither for the group, while the next read of the cntlr reports both
// (cnagent.md CN12).
func TestRepairRereadThatFailsReportsNothing(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.badLegDead()
	mdDir := f.dataMdDir()
	// The converge lists the array's md/ in its sweep, then in its md walk,
	// then once more to read it again: the third listing fails.
	f.node.mu.Lock()
	f.node.afterList[mdDir] = func() {
		f.node.afterList[mdDir] = func() {
			f.node.failCmd["list "+mdDir] = "input/output error"
		}
	}
	f.node.mu.Unlock()
	f.node.Reset()
	info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
	f.assertMdCalls(t, "the add", f.fail(f.leg1), f.remove(f.leg1),
		f.add(f.newLegDev))
	if n := len(f.node.callsMatching("list " + mdDir)); n != 3 {
		t.Fatalf("the converge listed %s %d times, want 3", mdDir, n)
	}
	// The reconciliation found the array running, so the dead bad leg is
	// no late member (CN12).
	if retrying(t, f.srv) {
		t.Fatalf("the late-member retry registered after the failed read " +
			"again")
	}
	assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
	assertOk(t, info.GetSliceIdToData()[testSlice], "pool data concat")
	assertOk(t, info.GetSliceIdToDmPool()[testSlice], "pool")
	assertOk(t, info.GetTdIdToThinInfo()[testTd].
		GetSliceIdToDmThin()[testSlice], "thin")
	assertNoReport(t, onlyDataGrp(info), "the converge")
	read, _ := f.read(t, 3)
	assertVerdict(t, read, "the read after it", true, false)
	assertReading(t, read, "the read after it", true, testDataLeg2,
		repNewLeg)
}

// TestRepairRereadOfABrokenArrayReportsNothing: a read again after the
// converge's own member changes whose listing answers but which then finds
// the array not running gives neither a verdict nor a reading (cnagent.md
// CN12, CN28): that a new leg holds a place in an array that does not run
// proves nothing. The array reads broken; or it stopped between the listing
// and its read of array_state, so the read finds it gone; or that read does
// not answer. In each the reconciliation ran over a running array.
func TestRepairRereadOfABrokenArrayReportsNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after func(f *repFix, mdDir string)
	}{
		{
			name: "broken",
			after: func(f *repFix, mdDir string) {
				f.node.files[mdDir+"/array_state"] = "broken\n"
			},
		},
		{
			name: "gone",
			after: func(f *repFix, mdDir string) {
				delete(f.node.files, mdDir+"/array_state")
			},
		},
		{
			name: "unreadable",
			after: func(f *repFix, mdDir string) {
				f.node.failRead[mdDir+"/array_state"] = true
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			f.badLegDead()
			mdDir := f.dataMdDir()
			// The third listing of the array's md/ is the read again (see
			// TestRepairRereadThatFailsReportsNothing): right after it, the
			// array changes as the case says.
			f.node.mu.Lock()
			f.node.afterList[mdDir] = func() {
				f.node.afterList[mdDir] = func() {
					f.node.afterList[mdDir] = func() { tc.after(f, mdDir) }
				}
			}
			f.node.mu.Unlock()
			f.node.Reset()
			info := f.syncup(t,
				repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
			f.assertMdCalls(t, "the add", f.fail(f.leg1), f.remove(f.leg1),
				f.add(f.newLegDev))
			if n := len(f.node.callsMatching("list " + mdDir)); n != 3 {
				t.Fatalf("the converge listed %s %d times, want 3", mdDir, n)
			}
			assertNoReport(t, onlyDataGrp(info), "the converge")
			// The reconciliation found the array running, so the dead bad
			// leg is no late member (CN12).
			if retrying(t, f.srv) {
				t.Fatalf("the late-member retry registered after the read "+
					"again found the array %s", tc.name)
			}
		})
	}
}

// TestRepairBadLegStaysOut pins that while its repair is CREATED or STARTED
// the bad leg is never added back nor put back, and no verdict names it
// for either (cnagent.md CN12, CN30), even on a leg that serves with an OK
// row: md holding it would hold the place the new leg needs, and a retired
// leg md still held after the finish would make the sweep stop the array.
// The new leg's connect fails throughout, so its add, which would remove a
// bad leg md has moved out of its place, never runs; once the repair is
// STARTED, a bad leg md holds without a place is removed all the same, and
// named until it is. Once the repair is aborted the bad leg is a leg_list
// member like any other: named, and added back by the converge its name
// brings.
func TestRepairBadLegStaysOut(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(f *repFix)
		held bool
	}{
		{"failed out of its place", func(f *repFix) {
			failOutOfSlot(f.node, f.mdDev, f.leg1)
		}, true},
		{"not held", func(f *repFix) {
			f.node.mu.Lock()
			f.node.mdRemove([]string{f.mdDev, "--remove", f.leg1})
			f.node.mu.Unlock()
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			f.newLegUnreachable()
			tc.set(f)
			clean := func(label string, rev uint64) {
				t.Helper()
				if _, reply := f.read(t, rev); reply.GetCode() != 0 {
					t.Fatalf("%s: the read's verdict is %v, want clean",
						label, reply)
				}
			}
			// failOnce makes the next mdadm command of the line fail, so the
			// read after its converge sees the member still to change.
			failOnce := func(line string) {
				f.node.mu.Lock()
				f.node.failCmd[line] = "mdadm: busy"
				f.node.mu.Unlock()
			}

			f.node.Reset()
			info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED,
				false))
			f.assertMdCalls(t, "created")
			assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
			clean("created", 3)

			f.node.Reset()
			if tc.held {
				failOnce(f.remove(f.leg1))
			}
			f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_STARTED, false))
			if tc.held {
				_, reply := f.read(t, 4)
				assertNamesOnly(t, reply, "started, the bad leg held",
					legName(f.srv, testDataLeg))
				f.node.Reset()
				f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_STARTED, false))
				f.assertMdCalls(t, "started, the bad leg held",
					f.remove(f.leg1))
			} else {
				f.assertMdCalls(t, "started")
			}
			clean("started", 4)
			if slices.Contains(f.members(), f.leg1) {
				t.Fatalf("the started repair's bad leg is held: %v",
					f.members())
			}

			failOnce(f.add(f.leg1))
			f.syncup(t, repairReq(5, pb.RepStage_REP_STAGE_ABORTED, true))
			_, reply := f.read(t, 5)
			assertNamesOnly(t, reply, "aborted", legName(f.srv, testDataLeg))
			f.node.Reset()
			f.syncup(t, repairReq(5, pb.RepStage_REP_STAGE_ABORTED, true))
			f.assertMdCalls(t, "aborted", f.add(f.leg1))
		})
	}
}

// TestRepairCreatedBadLegBackWithoutAPlace: md holds a CREATED repair's
// bad leg without a place while it holds the new leg in a place, as an
// assembly that took the bad leg back leaves it when a failover comes before
// the worker's move to STARTED. The read names the bad leg, and the converge
// removes it, running nothing else, and reports started, with or without
// the abort flag (cnagent.md CN12, CN30): left there, the bad leg would keep
// the repair from ever being reported started, and, its new leg holding a
// place, from having its abort confirmed.
func TestRepairCreatedBadLegBackWithoutAPlace(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(fmt.Sprintf("abort flag %v", abort), func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			f.badLegDead()
			info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED,
				false))
			assertVerdict(t, info, "the add", true, false)
			f.node.mu.Lock()
			f.node.mdAdd([]string{f.mdDev, "--add", "--failfast", f.leg1})
			f.node.mu.Unlock()
			f.node.setMemberState(f.mdDev, f.leg1, "spare,failfast")
			f.node.setMemberSlot(f.mdDev, f.leg1, mdSlotNone)
			read, reply := f.read(t, 3)
			assertVerdict(t, read, "the read of the bad leg back", false,
				false)
			assertNamesOnly(t, reply, "the read of the bad leg back",
				legName(f.srv, testDataLeg))

			f.node.Reset()
			info = f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_CREATED,
				abort))
			f.assertMdCalls(t, "the converge", f.remove(f.leg1))
			assertVerdict(t, info, "the converge", true, false)
			read, reply = f.read(t, 4)
			assertVerdict(t, read, "the read after it", true, false)
			if reply.GetCode() != 0 {
				t.Fatalf("the read after the removal: %v, want clean", reply)
			}
		})
	}
}

// TestRepairVerdictNamesANewLegToAdd pins the read's half of the add
// (cnagent.md CN30): the verdict names a new leg the converge would add,
// so an add that failed is brought again by the worker's re-send, and it
// names none whose add waits for the bad leg or for a member that keeps
// the data, which would bring a syncup on every round for nothing.
func TestRepairVerdictNamesANewLegToAdd(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.badLegDead()
	f.node.mu.Lock()
	f.node.failCmd[" --add --failfast "+f.newLegDev] =
		"mdadm: add new device failed"
	f.node.mu.Unlock()
	f.node.Reset()
	f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
	f.assertMdCalls(t, "the failed add",
		f.fail(f.leg1), f.remove(f.leg1), f.add(f.newLegDev))
	// The array runs whatever the add did, so the dead bad leg is no late
	// member (CN12).
	if retrying(t, f.srv) {
		t.Fatalf("the late-member retry registered after the failed add")
	}
	// The new leg's row reads OK once its prober has run: then its name
	// comes, and the converge its name brings adds it.
	_, reply := f.read(t, 3)
	if reply.GetCode() != 0 {
		t.Fatalf("the read before the new leg's probe: %v, want clean", reply)
	}
	probeLegs(t, f.srv, repNewLeg)
	_, reply = f.read(t, 3)
	assertNamesOnly(t, reply, "the read after the failed add",
		legName(f.srv, repNewLeg))
	f.node.Reset()
	info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
	f.assertMdCalls(t, "the re-sent converge", f.add(f.newLegDev))
	assertVerdict(t, info, "the re-sent converge", true, false)

	// No name while the add would wait: leg 2, the member that keeps the
	// data, copying.
	g := newRepFix(t, allLegs...)
	g.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
	probeLegs(t, g.srv, repNewLeg)
	g.badLegDead()
	g.copying(g.leg2, "1")
	_, reply = g.read(t, 3)
	if reply.GetCode() != 0 {
		t.Fatalf("the read while leg 2 copies: %v, want clean", reply)
	}
	// Nor while the bad leg serves with an OK row.
	h := newRepFix(t, allLegs...)
	h.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
	probeLegs(t, h.srv, repNewLeg)
	_, reply = h.read(t, 3)
	if reply.GetCode() != 0 {
		t.Fatalf("the read while the bad leg serves: %v, want clean", reply)
	}
}

// TestRepairFailedNewLegIsPutBack pins CN12's put-back of a new leg md has
// failed and moved out of its place: for a STARTED repair, and for a
// CREATED one with no abort flag in the request, it is removed and added
// again as a leg_list member is once its leg can take a member, and the
// read's verdict names it first; for a CREATED repair whose request carries
// the flag it is removed and never added, and the abort is confirmed.
func TestRepairFailedNewLegIsPutBack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stage   pb.RepStage
		abort   bool
		putBack bool
	}{
		{"started", pb.RepStage_REP_STAGE_STARTED, false, true},
		{"created", pb.RepStage_REP_STAGE_CREATED, false, true},
		{"created, abort flag set", pb.RepStage_REP_STAGE_CREATED, true,
			false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			f.badLegDead()
			f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
			if want := slices.Sorted(slices.Values([]string{
				f.leg2, f.newLegDev})); !slices.Equal(f.members(), want) {
				t.Fatalf("the add left the array holding %v, want %v",
					f.members(), want)
			}
			probeLegs(t, f.srv, repNewLeg)
			failOutOfSlot(f.node, f.mdDev, f.newLegDev)
			// The request with the case's stage and flag is stored first, as
			// the worker's fan-out stores it, and its read names the new leg.
			f.node.mu.Lock()
			f.node.failCmd[f.remove(f.newLegDev)] = "mdadm: busy"
			f.node.mu.Unlock()
			f.syncup(t, repairReq(4, tc.stage, tc.abort))
			_, reply := f.read(t, 4)
			assertNamesOnly(t, reply, "the read of the failed new leg",
				legName(f.srv, repNewLeg))

			f.node.Reset()
			info := f.syncup(t, repairReq(4, tc.stage, tc.abort))
			if tc.putBack {
				f.assertMdCalls(t, "the put-back",
					f.remove(f.newLegDev), f.add(f.newLegDev))
				assertVerdict(t, info, "the put-back", true, false)
				return
			}
			f.assertMdCalls(t, "the abort's removal", f.remove(f.newLegDev))
			assertVerdict(t, info, "the abort's removal", false, true)
			read, reply := f.read(t, 4)
			assertVerdict(t, read, "the read after the removal", false, true)
			if reply.GetCode() != 0 {
				t.Fatalf("the read after the removal: %v, want clean", reply)
			}
		})
	}
}

// TestRepairAbortRace pins both orders of the race between an abort and the
// add (architecture.md, Repair; cnagent.md CN12): when the abort flag
// reaches the primary first, the primary never adds that new leg and
// confirms the abort, converge after converge; when the primary has added
// the new leg first, a new leg that holds a place, copying or in sync, stays
// and the verdict says started whatever the flag says, while one md holds
// without a place, failed or not, is removed and the abort confirmed. A
// verdict confirms an abort only for a request that carries the flag.
func TestRepairAbortRace(t *testing.T) {
	t.Run("flag first", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.badLegDead()
		for rev := uint64(3); rev <= 4; rev++ {
			f.node.Reset()
			info := f.syncup(t, repairReq(rev, pb.RepStage_REP_STAGE_CREATED,
				true))
			f.assertMdCalls(t, "the flagged repair")
			assertVerdict(t, info, "the flagged repair", false, true)
		}
		read, _ := f.read(t, 4)
		assertVerdict(t, read, "the flagged repair, read", false, true)
	})
	for _, tc := range []struct {
		name      string
		set       func(f *repFix)
		removed   bool
		started   bool
		confirmed bool
	}{
		{"add first, copying", func(f *repFix) {
			f.copying(f.newLegDev, "1")
		}, false, true, false},
		{"add first, in sync", nil, false, true, false},
		{"add first, without a place", func(f *repFix) {
			f.node.setMemberState(f.mdDev, f.newLegDev, "spare,failfast")
			f.node.setMemberSlot(f.mdDev, f.newLegDev, mdSlotNone)
		}, true, false, true},
		{"add first, failed out", func(f *repFix) {
			failOutOfSlot(f.node, f.mdDev, f.newLegDev)
		}, true, false, true},
		// md has failed the new leg and keeps its slot while IO to it is
		// pending: it holds no place, and md still holds it, so neither
		// verdict comes until md moves it out.
		{"add first, failed in its place", func(f *repFix) {
			f.node.setMdSync(f.mdDev, 1, "idle", "none")
			f.node.setMemberState(f.mdDev, f.newLegDev, "faulty,failfast")
		}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			f.badLegDead()
			info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED,
				false))
			assertVerdict(t, info, "the add", true, false)
			if tc.set != nil {
				tc.set(f)
			}
			f.node.Reset()
			info = f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_CREATED,
				true))
			var want []string
			if tc.removed {
				want = []string{f.remove(f.newLegDev)}
			}
			f.assertMdCalls(t, "the flag after the add", want...)
			assertVerdict(t, info, "the flag after the add", tc.started,
				tc.confirmed)
			read, _ := f.read(t, 4)
			assertVerdict(t, read, "the flag after the add, read",
				tc.started, tc.confirmed)
		})
	}
	// With no flag in the request, a new leg md holds nothing on is no
	// confirmation.
	t.Run("no flag", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.badLegDead()
		f.newLegUnreachable()
		info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
		assertVerdict(t, info, "no flag", false, false)
		read, _ := f.read(t, 3)
		assertVerdict(t, read, "no flag, read", false, false)
	})
}

// TestRepairAbortedHasNeitherVerdict: an aborted repair's verdict says
// neither started nor abort confirmed (cnagent.md CN28), even while md still
// holds its new leg in a place, here because the converge that would fail and
// remove it as a member the group no longer keeps failed: the sp worker moves
// only a CREATED repair.
func TestRepairAbortedHasNeitherVerdict(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.badLegDead()
	f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
	f.node.mu.Lock()
	f.node.failCmd[f.fail(f.newLegDev)] = "mdadm: busy"
	f.node.mu.Unlock()
	info := f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_ABORTED, true))
	assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp], "busy",
		"grp data")
	if !slices.Contains(f.members(), f.newLegDev) {
		t.Fatalf("the failed fail left the array holding %v", f.members())
	}
	read, _ := f.read(t, 4)
	assertOk(t, read.GetGrpIdToMdRaid()[testDataGrp], "grp data, read")
	assertVerdict(t, read, "the read", false, false)
}

// TestRepairVerdictsOnlyFromARunningArray pins that the primary reports a
// repair's verdict and a group's reading only for a group whose row reads
// OK, a running array of a group its plan builds (cnagent.md CN12, CN28):
// that a new leg is no member of an array that does not run proves nothing.
// A stopped or broken array, an array that did not start, an array the
// in-sync guard holds, a standby and a level that builds no group each give
// neither, on the converge and on a read, while the abort flag is set and
// the new leg is no member.
func TestRepairVerdictsOnlyFromARunningArray(t *testing.T) {
	flagged := func(rev uint64) *pb.SyncupCntlrRequest {
		return repairReq(rev, pb.RepStage_REP_STAGE_CREATED, true)
	}
	for _, state := range []string{"inactive", "broken"} {
		t.Run("array "+state, func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			f.node.setArrayState(f.mdDev, state)
			info := f.syncup(t, flagged(3))
			assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp], state,
				"grp data")
			assertNoReport(t, onlyDataGrp(info), "the converge")
			read, _ := f.read(t, 3)
			assertNoReport(t, onlyDataGrp(read), "the read")
		})
	}
	t.Run("array did not start", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, cntlrReq(reqOpts{revision: 3, primary: false,
			raid1: true, twoLegs: true}))
		f.node.mu.Lock()
		f.node.failCmdAlways["mdadm --assemble"] = "mdadm: not enough to start"
		f.node.mu.Unlock()
		info := f.syncup(t, flagged(4))
		if info.GetGrpIdToMdRaid()[testDataGrp].GetStatus() !=
			pb.ResStatus_RES_STATUS_ERROR {
			t.Fatalf("a refused assembly reads %v",
				info.GetGrpIdToMdRaid()[testDataGrp])
		}
		assertNoReport(t, onlyDataGrp(info), "the converge")
		read, _ := f.read(t, 4)
		assertNoReport(t, onlyDataGrp(read), "the read")
	})
	t.Run("in-sync guard", func(t *testing.T) {
		// The aborted repair's new leg md holds is no member the group keeps,
		// and the only leg_list member in sync is on the bad leg, which does
		// not serve: the guard holds and the row reads ERROR.
		f := newRepFix(t, allLegs...)
		f.badLegDead()
		f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
		f.node.mu.Lock()
		f.node.mdAdd([]string{f.mdDev, "--add", "--failfast", f.leg1})
		f.node.mu.Unlock()
		f.copying(f.leg2, "1")
		info := f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_ABORTED, true))
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"left the group but is not failed", "grp data")
		assertNoReport(t, onlyDataGrp(info), "the converge")
		read, _ := f.read(t, 4)
		assertErrorDetails(t, read.GetGrpIdToMdRaid()[testDataGrp],
			"left the group but is not failed", "grp data, read")
		assertNoReport(t, onlyDataGrp(read), "the read")
	})
	t.Run("standby", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, true,
			repOpts{standby: true}))
		assertNoReport(t, info, "the converge")
		read, _ := f.read(t, 3)
		assertNoReport(t, read, "the read")
	})
	t.Run("no redundancy level", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, true,
			repOpts{level: pb.SpLevel_SP_LEVEL_NO_REDUND}))
		assertNoReport(t, info, "the converge")
		read, _ := f.read(t, 3)
		assertNoReport(t, read, "the read")
	})
}

// onlyDataGrp is an info's verdicts and the data group's reading alone: the
// fixture's meta group, whose one-leg array the cases leave running,
// reports its own reading.
func onlyDataGrp(info *pb.CntlrInfo) *pb.CntlrInfo {
	out := &pb.CntlrInfo{
		RepIdToRepairInfo: info.GetRepIdToRepairInfo(),
		GrpIdToGrpInfo:    map[uint64]*pb.CntlrInfo_GrpInfo{},
	}
	if reading := info.GetGrpIdToGrpInfo()[testDataGrp]; reading != nil {
		out.GrpIdToGrpInfo[testDataGrp] = reading
	}
	return out
}

// TestRepairGrpReading pins what the primary reports beside its rows for
// each group whose row reads OK (cnagent.md CN28): whole while md counts
// no degraded place, and the leg ids, of leg_list legs and of the repair's
// new legs, whose members md marks in sync and not faulty, ascending — a
// copy onto a new leg leaves it out until the copy ends, and after the
// finish, which puts the new leg at the bad leg's leg_idx, ahead of leg 2
// in leg_idx order and behind it in id order, the ids still come
// ascending. The converge's reading and a read's agree.
func TestRepairGrpReading(t *testing.T) {
	f := newRepFix(t, allLegs...)
	info := f.syncup(t, noRepairReq(3))
	assertReading(t, info, "no repair", true, testDataLeg, testDataLeg2)
	if info.GetGrpIdToGrpInfo()[testMetaGrp] == nil {
		t.Fatalf("no reading of the meta group's array")
	}
	f.badLegDead()
	info = f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_CREATED, false))
	assertReading(t, info, "after the add", true, testDataLeg2, repNewLeg)
	f.copying(f.newLegDev, "1")
	read, _ := f.read(t, 4)
	assertReading(t, read, "the new leg copying", false, testDataLeg2)
	f.node.setMemberState(f.mdDev, f.leg2, "faulty,in_sync,failfast")
	read, _ = f.read(t, 4)
	assertReading(t, read, "leg 2 faulty", false)
	f.node.setMemberState(f.mdDev, f.leg2, "in_sync,failfast")
	f.node.setMemberState(f.mdDev, f.newLegDev, "in_sync,failfast")
	f.node.setMdSync(f.mdDev, 0, "idle", "none")
	read, _ = f.read(t, 4)
	assertReading(t, read, "the copy ended", true, testDataLeg2, repNewLeg)

	// The finish: the new leg takes the bad leg's place in leg_list and its
	// leg_idx, and the request carries no repair.
	finished := noRepairReq(5)
	legs := dataGrpOf(finished).GetLegList()
	for i, leg := range legs {
		if leg.GetLegId() == testDataLeg {
			newLeg := legOf(repNewLeg, sideOf(repNewSide, testIp, testSvcId))
			newLeg.LegIdx = leg.GetLegIdx()
			legs[i] = newLeg
		}
	}
	if legs[0].GetLegId() != repNewLeg || legs[0].GetLegIdx() >=
		legs[1].GetLegIdx() || legs[1].GetLegId() != testDataLeg2 {
		t.Fatalf("the finished leg_list is not the new leg ahead of leg 2 "+
			"in leg_idx order: %v", legs)
	}
	// The converge's sweep disconnects the bad leg, which left the request,
	// off the pass, so its reply names that connection as a leftover.
	reply, err := f.srv.SyncupCntlr(f.ctx, finished)
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	assertReading(t, reply.GetCntlrInfo(), "after the finish", true,
		testDataLeg2, repNewLeg)
	read, _ = f.read(t, 5)
	assertReading(t, read, "after the finish, read", true, testDataLeg2,
		repNewLeg)
}

// TestRepairAssembly pins the assembly set with a repair (cnagent.md CN12):
// the available leg_list legs with a superblock and the available new legs
// with one of a CREATED or STARTED repair, all handed to mdadm, which judges
// membership from the superblocks. After a failover a half-copied new leg is
// assembled beside the leg that kept the data and its copy resumes; alone it
// is refused by mdadm, and the group reads ERROR with no create; a whole new
// leg alone is handed to mdadm alone, and where md recorded the other legs
// as failed (the fake starts it here) the array is found by the new leg, on
// the converge, in the pool's concat and on the read; an aborted repair's
// new leg is no candidate, nor is a new leg that is not available, which is
// not even probed. The create case holds only when no candidate carries a
// superblock, and its members are the leg_list legs alone.
func TestRepairAssembly(t *testing.T) {
	// failover converges the repair on the primary, whose converge adds
	// the new leg in the bad leg's place, demotes it, and makes the
	// assembly's shape: the bad leg dead throughout.
	failover := func(t *testing.T, stage pb.RepStage) *repFix {
		f := newRepFix(t, allLegs...)
		f.badLegDead()
		f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
		f.syncup(t, repairReq(4, stage, stage == pb.RepStage_REP_STAGE_ABORTED,
			repOpts{standby: true}))
		if !f.node.arrayGone(f.mdDev) {
			t.Fatalf("the demotion left the array")
		}
		f.node.Reset()
		return f
	}
	assembled := func(f *repFix) string {
		calls := f.node.callsMatching("cmd mdadm --assemble " + f.mdDev)
		if len(calls) != 1 {
			return ""
		}
		return calls[0]
	}
	t.Run("half-copied new leg beside leg 2", func(t *testing.T) {
		f := failover(t, pb.RepStage_REP_STAGE_STARTED)
		info := f.syncup(t, repairReq(5, pb.RepStage_REP_STAGE_STARTED, false))
		call := assembled(f)
		if !strings.HasSuffix(call, " "+f.leg2+" "+f.newLegDev) {
			t.Fatalf("the assembly is %q, want leg 2 then the new leg", call)
		}
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp], "grp data")
		assertNoCall(t, f.node, "cmd mdadm --create")
		assertVerdict(t, info, "the assembled repair", true, false)
	})
	t.Run("half-copied new leg alone", func(t *testing.T) {
		f := failover(t, pb.RepStage_REP_STAGE_STARTED)
		f.legDead(testDataLeg2, testIp2, testSvcId2)
		f.node.mu.Lock()
		f.node.failCmdAlways["mdadm --assemble"] =
			"mdadm: not enough to start the array"
		f.node.mu.Unlock()
		info := f.syncup(t, repairReq(5, pb.RepStage_REP_STAGE_STARTED, false))
		if call := assembled(f); !strings.HasSuffix(call, " "+f.newLegDev) ||
			strings.Contains(call, f.leg2) {
			t.Fatalf("the assembly is %q, want the new leg alone", call)
		}
		assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"not enough to start", "grp data")
		assertNoCall(t, f.node, "cmd mdadm --create")
		assertNoReport(t, onlyDataGrp(info), "the refused assembly")
	})
	t.Run("whole new leg alone", func(t *testing.T) {
		f := failover(t, pb.RepStage_REP_STAGE_STARTED)
		f.legDead(testDataLeg2, testIp2, testSvcId2)
		info := f.syncup(t, repairReq(5, pb.RepStage_REP_STAGE_STARTED, false))
		if call := assembled(f); !strings.HasSuffix(call, " "+f.newLegDev) ||
			strings.Contains(call, f.leg2) {
			t.Fatalf("the assembly is %q, want the new leg alone", call)
		}
		assertOk(t, info.GetGrpIdToMdRaid()[testDataGrp],
			"grp data started from the new leg")
		assertVerdict(t, info, "the array of the new leg", true, false)
		// The new leg finds the array for the pool's concat too, and on
		// the read, which knows the array only by its members.
		assertOk(t, info.GetSliceIdToData()[testSlice], "pool data concat")
		assertOk(t, info.GetSliceIdToDmPool()[testSlice], "pool")
		read, _ := f.read(t, 5)
		assertOk(t, read.GetGrpIdToMdRaid()[testDataGrp], "grp data read")
		assertVerdict(t, read, "the array of the new leg, read", true, false)
		assertOk(t, read.GetSliceIdToData()[testSlice],
			"pool data concat read")
		assertOk(t, read.GetSliceIdToDmPool()[testSlice], "pool read")
		f.node.Reset()
		f.syncup(t, repairReq(5, pb.RepStage_REP_STAGE_STARTED, false))
		assertNoCall(t, f.node, "cmd mdadm --assemble")
	})
	t.Run("aborted repair's new leg", func(t *testing.T) {
		f := failover(t, pb.RepStage_REP_STAGE_ABORTED)
		f.syncup(t, repairReq(5, pb.RepStage_REP_STAGE_ABORTED, true))
		if call := assembled(f); !strings.HasSuffix(call, " "+f.leg2) ||
			strings.Contains(call, f.newLegDev) {
			t.Fatalf("the assembly is %q, want leg 2 alone", call)
		}
		assertNoCall(t, f.node, "--examine --export "+f.newLegDev)
	})
	t.Run("unavailable new leg", func(t *testing.T) {
		f := failover(t, pb.RepStage_REP_STAGE_STARTED)
		f.legDead(repNewLeg, testIp, testSvcId)
		f.syncup(t, repairReq(5, pb.RepStage_REP_STAGE_STARTED, false))
		if call := assembled(f); !strings.HasSuffix(call, " "+f.leg2) ||
			strings.Contains(call, f.newLegDev) {
			t.Fatalf("the assembly is %q, want leg 2 alone", call)
		}
		assertNoCall(t, f.node, "--examine --export "+f.newLegDev)
	})
	for _, tc := range []struct {
		name string
		// newLegSuperblock stamps an md superblock on the new leg.
		newLegSuperblock bool
	}{
		{"create: none carries a superblock", false},
		{"no create: the new leg carries one", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			ctx := context.Background()
			newLegDev := srv.nf.DmPath(legName(srv, repNewLeg))
			mdDev := srv.nf.MdPath(srv.nf.CnMdDevName(
				testCluster, testCn, testSp, 0, 0, false))
			if tc.newLegSuperblock {
				node.superblocks[newLegDev] = true
			}
			if _, err := srv.SyncupCn(ctx, cnReq(2, true)); err != nil {
				t.Fatalf("SyncupCn: %v", err)
			}
			reply, err := srv.SyncupCntlr(ctx,
				repairReq(2, pb.RepStage_REP_STAGE_CREATED, false))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			created := node.callsMatching("cmd mdadm --create " + mdDev)
			assembledCalls := node.callsMatching(
				"cmd mdadm --assemble " + mdDev)
			if tc.newLegSuperblock {
				if len(created) != 0 || len(assembledCalls) != 1 ||
					!strings.HasSuffix(assembledCalls[0], " "+newLegDev) {
					t.Fatalf("created %v, assembled %v; want the new leg "+
						"assembled alone", created, assembledCalls)
				}
				return
			}
			if len(created) != 1 || strings.Contains(created[0], newLegDev) ||
				!strings.Contains(created[0], "--raid-devices 2 ") {
				t.Fatalf("created %v, want one create over leg 1 and leg 2",
					created)
			}
			assertOk(t, reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp],
				"grp data")
		})
	}
}

// TestRepairOnACloneMetaGroup is the add on a clone's clone meta group,
// which the cntlr builds as a slice's group (cnagent.md CN12): the bad leg
// that does not serve is failed and removed and the new leg added to the
// group's own array, while the dm-clone over that array is left as it is.
func TestRepairOnACloneMetaGroup(t *testing.T) {
	const cloneNewLeg, cloneNewSide = uint64(0x68), uint64(0x69)
	// request is the pool with the clone, its clone meta group with or
	// without the repair.
	request := func(revision uint64, withRepair bool) *pb.SyncupCntlrRequest {
		clone := cloneWithGrp(true)
		req := cntlrReq(reqOpts{revision: revision, primary: true,
			raid1: true, clones: []*pb.Clone{clone}})
		if !withRepair {
			return req
		}
		req.CloneList[0].MetaGrp.RepName = repName
		newLeg := legOf(cloneNewLeg, sideOf(cloneNewSide, testIp, testSvcId))
		newLeg.LegIdx = 2
		req.NameToRepair = map[string]*pb.Repair{repName: {
			RepId:        repId,
			GrpId:        testCloneGrp,
			RepStage:     pb.RepStage_REP_STAGE_CREATED,
			NewLegList:   []*pb.Leg{newLeg},
			BadLegIdList: []uint64{testCloneLeg},
			CloneName:    "clone-1",
		}}
		return req
	}
	converge := func(t *testing.T, srv *CnAgentServer,
		req *pb.SyncupCntlrRequest) *pb.CntlrInfo {
		t.Helper()
		reply, err := srv.SyncupCntlr(context.Background(), req)
		if err != nil || reply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("SyncupCntlr: %v %v", err, reply.GetAgentReply())
		}
		return reply.GetCntlrInfo()
	}
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	if _, err := srv.SyncupCn(context.Background(), cnReq(2, true)); err !=
		nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	converge(t, srv, request(2, false))
	probeAllLegs(t, srv)
	dmClone := cloneName(srv, testClone)
	node.setCloneHydrated(t, dmClone, 37)
	node.setCtrlState(legNqn(srv, testCloneLeg), testIp, testSvcId,
		"connecting")
	mdPath := cloneMdPath(srv)
	bad := srv.nf.DmPath(legName(srv, testCloneLeg))
	node.Reset()
	info := converge(t, srv, request(3, true))
	got := node.callsMatching("cmd mdadm " + mdPath + " ")
	want := []string{
		"cmd mdadm " + mdPath + " --fail " + bad,
		"cmd mdadm " + mdPath + " --remove " + bad,
		"cmd mdadm " + mdPath + " --add --failfast " +
			srv.nf.DmPath(legName(srv, cloneNewLeg)),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the clone meta group's mdadm commands:\n%s\nwant\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	assertOk(t, info.GetGrpIdToMdRaid()[testCloneGrp], "the clone meta group")
	assertVerdict(t, info, "the clone meta group's repair", true, false)
	assertNoCall(t, node, "cmd dmsetup create "+dmClone)
	assertNoCall(t, node, "cmd dmsetup reload "+dmClone)
	assertOk(t, info.GetCloneIdToDmClone()[testClone], "the dm-clone")
}
