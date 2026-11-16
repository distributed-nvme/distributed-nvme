package cnagent

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The early fail of a dead leg's member (cnagent.md CN11, CN12, CN30): the
// prober counts its failed rounds in a row, and on an idle array the
// primary fails a member whose leg is gone while another member keeps the
// data, so the death reaches the survivor's superblock.

// failRounds completes n failed rounds of a leg's prober, as a dead leg's
// probe IO fails (CN11).
func failRounds(t *testing.T, srv *CnAgentServer, legId uint64, n int) {
	t.Helper()
	st := legState(t, srv)
	srv.mu.Lock()
	prober := st.probers[legId]
	srv.mu.Unlock()
	if prober == nil {
		t.Fatalf("leg %d has no prober", legId)
	}
	for range n {
		prober.finish(errors.New("input/output error"))
	}
}

// TestLegProberCountsFailedRoundsInARow pins CN11's count: completed rounds
// in a row that failed, zero after one that succeeds, and one more for a
// round in flight past the stall bound, which a failed end then does not
// count twice.
func TestLegProberCountsFailedRoundsInARow(t *testing.T) {
	now := time.Unix(1<<30, 0)
	stall := 15 * time.Second
	errIO := errors.New("input/output error")
	p := &legProber{}
	p.finish(errIO)
	p.finish(errIO)
	if got := p.deadRounds(now, stall); got != 2 {
		t.Fatalf("two failed rounds count %d, want 2", got)
	}
	p.finish(nil)
	if got := p.deadRounds(now, stall); got != 0 {
		t.Fatalf("a round that succeeds leaves %d, want 0", got)
	}
	p.finish(errIO)
	p.begin(now.Add(-stall + time.Second))
	if got := p.deadRounds(now, stall); got != 1 {
		t.Errorf("a round in flight within the bound adds: %d, want 1", got)
	}
	p.begin(now.Add(-stall - time.Second))
	if got := p.deadRounds(now, stall); got != 2 {
		t.Errorf("a round in flight past the bound counts %d, want 2", got)
	}
	p.finish(errIO)
	if got := p.deadRounds(now, stall); got != 2 {
		t.Errorf("the stalled round that then fails counts %d, want 2", got)
	}
}

// TestADeadIdleMemberIsFailedNotRemoved pins the early fail (CN12): a
// leg_list member md still holds in sync, whose leg does not serve and whose
// prober failed twice in a row, is failed and not removed while the other
// leg keeps the data; the read names it first (CN30), and once md shows it
// failed the read names it no more.
func TestADeadIdleMemberIsFailedNotRemoved(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, quietLevel(3))
	f.legDead(testDataLeg2, testIp2, testSvcId2)
	failRounds(t, f.srv, testDataLeg2, deadLegRounds)
	_, reply := f.read(t, 3)
	assertNamesOnly(t, reply, "dead member", legName(f.srv, testDataLeg2))
	f.node.Reset()
	f.syncup(t, quietLevel(4))
	f.assertMdCalls(t, "dead member", f.fail(f.leg2))
	if !slices.Contains(f.members(), f.leg2) {
		t.Fatalf("the failed member was removed: %v", f.members())
	}
	if _, reply := f.read(t, 4); reply.GetCode() != 0 {
		t.Errorf("a read after the fail: code %d (%s), want none",
			reply.GetCode(), reply.GetDetails())
	}
	f.node.Reset()
	f.syncup(t, quietLevel(5))
	f.assertMdCalls(t, "the leg still dead")
}

// TestADeadMemberBesideAPendingSource pins the copy source of the early fail
// (CN12): a source leg whose row reads PENDING, as before a new primary's
// first probe round, does not hold the fail back.
func TestADeadMemberBesideAPendingSource(t *testing.T) {
	f := newRepFix(t, testMetaLeg, testDataLeg2)
	f.syncup(t, quietLevel(3))
	f.legDead(testDataLeg2, testIp2, testSvcId2)
	failRounds(t, f.srv, testDataLeg2, deadLegRounds)
	f.node.Reset()
	f.syncup(t, quietLevel(4))
	f.assertMdCalls(t, "a PENDING source", f.fail(f.leg2))
}

// TestAStalledRoundMakesALegDead pins the stall half of CN11's count
// through the converge: one failed round and a round in flight past the
// stall bound make the leg dead, and the member is failed; the same round
// within the bound does not.
func TestAStalledRoundMakesALegDead(t *testing.T) {
	for _, past := range []bool{false, true} {
		f := newRepFix(t, allLegs...)
		clock := withClock(f.srv)
		f.syncup(t, quietLevel(3))
		f.legDead(testDataLeg2, testIp2, testSvcId2)
		failRounds(t, f.srv, testDataLeg2, 1)
		st := legState(t, f.srv)
		f.srv.mu.Lock()
		prober := st.probers[testDataLeg2]
		stall := f.srv.probeStall
		f.srv.mu.Unlock()
		prober.begin(clock.Now())
		if past {
			clock.advance(stall + time.Second)
		} else {
			clock.advance(stall - time.Second)
		}
		f.node.Reset()
		f.syncup(t, quietLevel(4))
		if past {
			f.assertMdCalls(t, "a stalled round", f.fail(f.leg2))
		} else {
			f.assertMdCalls(t, "a round within the bound")
		}
	}
}

// TestADeadMemberIsNotFailedUnlessSafe pins the early fail's conditions
// (CN12): no fail after one failed round, for a leg that serves whatever its
// prober says, while the other leg's row reads ERROR, or while the other
// member is not in sync; the read names nothing in each.
func TestADeadMemberIsNotFailedUnlessSafe(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *repFix)
	}{
		{"one failed round", func(t *testing.T, f *repFix) {
			f.legDead(testDataLeg2, testIp2, testSvcId2)
			failRounds(t, f.srv, testDataLeg2, deadLegRounds-1)
		}},
		{"the leg serves", func(t *testing.T, f *repFix) {
			failRounds(t, f.srv, testDataLeg2, deadLegRounds)
		}},
		{"the other leg's row reads ERROR", func(t *testing.T, f *repFix) {
			f.legDead(testDataLeg2, testIp2, testSvcId2)
			failRounds(t, f.srv, testDataLeg2, deadLegRounds)
			failRounds(t, f.srv, testDataLeg, 1)
		}},
		{"the other member is not in sync", func(t *testing.T, f *repFix) {
			f.legDead(testDataLeg2, testIp2, testSvcId2)
			failRounds(t, f.srv, testDataLeg2, deadLegRounds)
			f.copying(f.leg1, "0")
		}},
		{"both legs dead", func(t *testing.T, f *repFix) {
			f.legDead(testDataLeg2, testIp2, testSvcId2)
			f.badLegDead()
			failRounds(t, f.srv, testDataLeg2, deadLegRounds)
			failRounds(t, f.srv, testDataLeg, deadLegRounds)
		}},
		{"md shows it faulty in its place", func(t *testing.T, f *repFix) {
			// md holds a failed member in its slot while IO to it is
			// pending: the fail has happened, so the step leaves it.
			f.legDead(testDataLeg2, testIp2, testSvcId2)
			failRounds(t, f.srv, testDataLeg2, deadLegRounds)
			f.node.setMemberState(f.mdDev, f.leg2, "faulty,failfast")
			f.node.setMemberSlot(f.mdDev, f.leg2, "1")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			f.syncup(t, quietLevel(3))
			tc.setup(t, f)
			f.node.Reset()
			if _, reply := f.read(t, 3); reply.GetCode() != 0 {
				t.Errorf("the read names: code %d (%s), want none",
					reply.GetCode(), reply.GetDetails())
			}
			f.syncup(t, quietLevel(4))
			for _, call := range f.mdCalls() {
				t.Errorf("mdadm ran %q, want nothing", call)
			}
		})
	}
}

// TestADeadBadLegIsFailedOnce pins the early fail of an open repair's bad
// leg (CN12): it counts as any leg_list member, and when the repair's step
// then takes it out in the same pass, it is removed with no second fail;
// with its new leg not available, it is failed and stays.
func TestADeadBadLegIsFailedOnce(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.badLegDead()
	failRounds(t, f.srv, testDataLeg, deadLegRounds)
	f.node.Reset()
	f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
	f.assertMdCalls(t, "the add", f.fail(f.leg1), f.remove(f.leg1),
		f.add(f.newLegDev))

	f = newRepFix(t, allLegs...)
	f.newLegUnreachable()
	f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
	f.badLegDead()
	failRounds(t, f.srv, testDataLeg, deadLegRounds)
	// The late-member retry skips an open repair's bad leg while the array
	// runs, so the read's name is what brings the converge.
	_, reply := f.read(t, 3)
	assertNamesOnly(t, reply, "the dead bad leg", legName(f.srv, testDataLeg))
	f.node.Reset()
	f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_CREATED, false))
	f.assertMdCalls(t, "no new leg", f.fail(f.leg1))
	if !slices.Contains(f.members(), f.leg1) {
		t.Fatalf("the failed bad leg was removed: %v", f.members())
	}
}
