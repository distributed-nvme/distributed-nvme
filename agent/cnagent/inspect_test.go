package cnagent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// inspect is InspectGrp of the fixture's data group, with a dry run from
// legId when it is not zero.
func (f *repFix) inspect(
	t *testing.T,
	legId uint64,
	force bool,
) *pb.InspectGrpReply {
	t.Helper()
	reply, err := f.srv.InspectGrp(f.ctx, &pb.InspectGrpRequest{
		ClusterId: testCluster, CnId: testCn, SpId: testSp,
		CntlrId: testCntlr, GrpId: testDataGrp, LegId: legId, Force: force,
	})
	if err != nil {
		t.Fatalf("InspectGrp: %v", err)
	}
	return reply
}

// memberOf is the reply's member for legId, or nil.
func memberOf(reply *pb.InspectGrpReply, legId uint64) *pb.InspectedMember {
	for _, member := range reply.GetMemberList() {
		if member.GetLegId() == legId {
			return member
		}
	}
	return nil
}

// TestInspectGrpReadsEveryMember pins InspectGrp's read of a running array
// (cnagent.md CN31): every leg_list leg and the repair's new leg,
// each with whether it serves, its superblock read and md's view of it,
// and the array's state; it runs no mdadm command and changes nothing; a
// dry run over the running array is refused, as the primary leaves a mark
// waiting while the array runs.
func TestInspectGrpReadsEveryMember(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
	before := f.members()
	f.node.Reset()
	reply := f.inspect(t, 0, false)
	if !strings.Contains(reply.GetArrayState(), "clean") &&
		!strings.Contains(reply.GetArrayState(), "active") {
		t.Errorf("array state %q, want the running array's",
			reply.GetArrayState())
	}
	if reply.GetDryRun() != nil {
		t.Errorf("a dry run with no leg named: %v", reply.GetDryRun())
	}
	if got := len(reply.GetMemberList()); got != 3 {
		t.Fatalf("%d members, want both legs and the new leg: %v", got,
			reply.GetMemberList())
	}
	// Leg 1 is the repair's bad leg, leg 2 a plain leg of the group.
	for legId, kind := range map[uint64]string{
		testDataLeg: inspectKindBadLeg, testDataLeg2: inspectKindLeg,
	} {
		m := memberOf(reply, legId)
		if m.GetKind() != kind || !m.GetAvailable() ||
			!m.GetHasSuperblock() || m.GetUuid() == "" ||
			!strings.Contains(m.GetMdState(), "in_sync") {
			t.Errorf("leg %d reads %v, want a serving in-sync %s with "+
				"a superblock", legId, m, kind)
		}
	}
	if m := memberOf(reply, testDataLeg); m.GetUuid() !=
		memberOf(reply, testDataLeg2).GetUuid() {
		t.Errorf("the two legs read identities %q and %q, want one",
			m.GetUuid(), memberOf(reply, testDataLeg2).GetUuid())
	}
	if m := memberOf(reply, repNewLeg); m.GetKind() != inspectKindNewLeg ||
		m.GetMdState() != "" {
		t.Errorf("the new leg reads %v, want a new leg md does not hold", m)
	}
	assertNoCall(t, f.node, "cmd mdadm")
	if after := f.members(); !slices.Equal(after, before) {
		t.Errorf("the inspect changed the array's members to %v", after)
	}
	dry := f.inspect(t, testDataLeg2, false).GetDryRun()
	if !strings.Contains(dry.GetRefusal(), "runs") ||
		len(dry.GetZeroLegIdList()) != 0 {
		t.Errorf("the dry run over the running array: %v, want refused", dry)
	}
}

// TestInspectGrpDryRunMatchesTheStart pins the dry run (cnagent.md
// CN31): with no array of the group there, a dry run from a full
// member names the members the start would zero and its update, with no
// mdadm command, and the real start from the same mark then zeroes exactly
// those; a dry run from a leg the start refuses carries the start's own
// refusal.
func TestInspectGrpDryRunMatchesTheStart(t *testing.T) {
	f := downNoRepair(t)
	// A first mark the start refuses, naming a half-copied leg 2, makes the
	// cntlr primary with no array of the group and nothing zeroed.
	f.node.setSuperblock(f.leg2, fakeSuperblock{
		role: "Active device 1", recovery: "45056 sectors"})
	info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
	refusal := info.GetGrpIdToMdRaid()[testDataGrp].GetDetails()
	if !strings.Contains(refusal, "not a full member") {
		t.Fatalf("the first start: %q, want refused", refusal)
	}
	f.node.Reset()
	dry := f.inspect(t, testDataLeg2, false).GetDryRun()
	if dry.GetRefusal() == "" ||
		!strings.Contains(refusal, dry.GetRefusal()) {
		t.Errorf("the dry run from leg 2 refuses %q, want the start's own "+
			"refusal %q", dry.GetRefusal(), refusal)
	}
	dry = f.inspect(t, testDataLeg, false).GetDryRun()
	if dry.GetRefusal() != "" ||
		!slices.Equal(dry.GetZeroLegIdList(), []uint64{testDataLeg2}) ||
		dry.GetUpdate() != mdUpdateUuid || dry.GetStopsArray() {
		t.Fatalf("the dry run from leg 1: %v, want leg 2 zeroed under %s",
			dry, mdUpdateUuid)
	}
	assertNoCall(t, f.node, "cmd mdadm")
	f.node.Reset()
	f.syncup(t, withMark(quietReq(5), testDataLeg))
	assertOrder(t, f.node, zeroOf(f.leg2), f.forced(f.leg1, mdUpdateUuid))
	assertNoCall(t, f.node, zeroOf(f.leg1))
}

// TestInspectGrpDryRunCarriesTheForce pins the dry run's force (cnagent.md
// CN31): a named leg whose change count is below that of a full member of
// its identity the start would zero is refused without the force, as the
// real start refuses it, and with the force the dry run names that member
// zeroed.
func TestInspectGrpDryRunCarriesTheForce(t *testing.T) {
	f := downNoRepair(t)
	f.node.setSuperblock(f.leg2, fakeSuperblock{events: 10})
	f.node.setSuperblock(f.leg1, fakeSuperblock{events: 50})
	// The real start refuses the older leg, which leaves the cntlr primary
	// with no array of the group.
	info := f.syncup(t, withMark(quietReq(4), testDataLeg2))
	refusal := info.GetGrpIdToMdRaid()[testDataGrp].GetDetails()
	if !strings.Contains(refusal, "is a full member with md change count") {
		t.Fatalf("the start: %q, want the older leg refused", refusal)
	}
	f.node.Reset()
	dry := f.inspect(t, testDataLeg2, false).GetDryRun()
	if dry.GetRefusal() == "" || !strings.Contains(refusal, dry.GetRefusal()) {
		t.Errorf("the dry run with no force refuses %q, want the start's %q",
			dry.GetRefusal(), refusal)
	}
	dry = f.inspect(t, testDataLeg2, true).GetDryRun()
	if dry.GetRefusal() != "" ||
		!slices.Equal(dry.GetZeroLegIdList(), []uint64{testDataLeg}) {
		t.Errorf("the dry run with the force: %v, want leg 1 zeroed", dry)
	}
	assertNoCall(t, f.node, "cmd mdadm")
}

// TestInspectGrpDryRunOverAnArrayThatDoesNotRun pins the dry run over an
// array of the group that is there and does not run (cnagent.md CN31): it
// says the start stops it first, and refuses an array that holds a member
// that is no leg wrapper of this pool, as the real start refuses to stop
// one, word for word; it stops nothing itself.
func TestInspectGrpDryRunOverAnArrayThatDoesNotRun(t *testing.T) {
	t.Run("ours, broken", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		f.node.setArrayState(f.mdDev, "broken")
		f.node.Reset()
		dry := f.inspect(t, testDataLeg2, false).GetDryRun()
		if dry.GetRefusal() != "" || !dry.GetStopsArray() ||
			!slices.Equal(dry.GetZeroLegIdList(), []uint64{testDataLeg}) {
			t.Fatalf("dry run %v, want the stop first and leg 1 zeroed", dry)
		}
		assertNoCall(t, f.node, "cmd mdadm")
	})
	t.Run("another sp's leg in it", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, levelReq(3, pb.SpLevel_SP_LEVEL_NO_THINPOOL))
		f.node.mu.Lock()
		array := f.node.arrays[f.mdDev]
		held := append(append([]string(nil), array.members...),
			"/dev/mapper/"+names(f.srv).CnLegName(
				testCluster, testCn, testSp+1, testDataLeg))
		name := array.name
		f.node.mu.Unlock()
		f.node.seedArray(f.mdDev, name, held...)
		f.node.setArrayState(f.mdDev, "inactive")
		f.node.Reset()
		dry := f.inspect(t, testDataLeg2, false).GetDryRun()
		if !strings.Contains(dry.GetRefusal(), "which is no leg of this sp") {
			t.Fatalf("dry run %v, want the real start's refusal to stop "+
				"the array", dry)
		}
		assertNoCall(t, f.node, "cmd mdadm")
	})
}

// TestInspectGrpLeavesAnAbortedRepairsLegOut: the new leg of an ABORTED
// repair is no member of the group, so the inspect does not list it.
func TestInspectGrpLeavesAnAbortedRepairsLegOut(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_ABORTED, true))
	reply := f.inspect(t, 0, false)
	if m := memberOf(reply, repNewLeg); m != nil {
		t.Fatalf("the aborted repair's new leg is listed: %v", m)
	}
}

// TestInspectGrpEndsAtItsDeadline pins InspectGrp's locks (cnagent.md
// CN31): a member read that stalls holds neither the node lock nor the
// cntlr's object lock, and the call answers DEADLINE_EXCEEDED at its
// deadline while the read goes on.
func TestInspectGrpEndsAtItsDeadline(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, quietLevel(3))
	key := "readsb " + f.leg2
	f.node.blockCmd(key)
	t.Cleanup(func() { f.node.releaseCmd(key) })
	ctx, cancel := context.WithTimeout(f.ctx, 200*time.Millisecond)
	defer cancel()
	_, err := f.srv.InspectGrp(ctx, &pb.InspectGrpRequest{
		ClusterId: testCluster, CnId: testCn, SpId: testSp,
		CntlrId: testCntlr, GrpId: testDataGrp,
	})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("InspectGrp over a stalled read: %v, want DEADLINE_EXCEEDED",
			err)
	}
	node := f.srv.locks.Node()
	if !node.TryLock() {
		t.Fatalf("the stalled read holds the node lock")
	}
	node.Unlock()
	obj := f.srv.locks.Obj(cntlrKey(testCluster, testCn, testSp, testCntlr))
	if !obj.TryLock() {
		t.Fatalf("the stalled read holds the cntlr's object lock")
	}
	obj.Unlock()
}

// TestInspectGrpReadsMembersOverAFailedArrayRead pins a failed read of the
// group's array (cnagent.md CN31): the members are read all the same, the
// array's state carries the read's error, and the dry run is refused with
// it, as the real start ends on it.
func TestInspectGrpReadsMembersOverAFailedArrayRead(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, quietLevel(3))
	f.node.mu.Lock()
	f.node.failReadAlways["/md/array_state"] = true
	f.node.mu.Unlock()
	f.node.Reset()
	reply := f.inspect(t, testDataLeg2, false)
	if !strings.HasPrefix(reply.GetArrayState(), "not read: ") {
		t.Errorf("array state %q, want the read's error",
			reply.GetArrayState())
	}
	if m := memberOf(reply, testDataLeg2); !m.GetHasSuperblock() {
		t.Errorf("leg 2 reads %v, want its superblock read", m)
	}
	if dry := reply.GetDryRun(); dry.GetRefusal() == "" ||
		!strings.Contains(reply.GetArrayState(), dry.GetRefusal()) {
		t.Errorf("dry run %v, want the array read's error as the refusal",
			dry)
	}
	assertNoCall(t, f.node, "cmd mdadm")
}

// TestInspectGrpReadsNoLegThatDoesNotServe pins the serving test of
// InspectGrp (cnagent.md CN31): a member whose leg does not serve, here
// the new leg whose disk node refuses the connect, is reported as not
// serving, with no read of it.
func TestInspectGrpReadsNoLegThatDoesNotServe(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.newLegUnreachable()
	f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
	f.node.Reset()
	m := memberOf(f.inspect(t, 0, false), repNewLeg)
	if m.GetAvailable() || m.GetHasSuperblock() ||
		!strings.Contains(m.GetReadError(), "does not serve") {
		t.Errorf("the unreachable new leg reads %v, want not serving", m)
	}
	assertNoCall(t, f.node, "readsb "+f.newLegDev)
}
