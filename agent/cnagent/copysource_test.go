package cnagent

import (
	"context"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Before md copies onto a member that joins the array, another member must
// be in sync on a leg that serves (CN12): md learns that a member is dead
// only from IO to it, so on an idle array a dead member still reads in sync,
// and a copy that reads it breaks the array. The put-back of a failed member
// and the add of a member the array lacks wait for such a source, and so does
// the read's verdict, which names neither while none serves.

// legTwoConnecting puts the data group's second leg's path in the state a
// side whose disk node died leaves it: connecting, not available.
func legTwoConnecting(f *repFix) {
	f.node.setCtrlState(legNqn(f.srv, testDataLeg2), testIp2, testSvcId2,
		"connecting")
}

func legTwoLive(f *repFix) {
	f.node.setCtrlState(legNqn(f.srv, testDataLeg2), testIp2, testSvcId2,
		"live")
}

func TestAMemberJoinsOnlyBesideACopySource(t *testing.T) {
	for _, tc := range []struct {
		name string
		// set leaves leg 1's member out of the array's slots: failed out
		// of its slot (the put-back's case) or gone (the add's case).
		set func(f *repFix)
		// cmds are the commands that put leg 1 back.
		cmds func(f *repFix) []string
	}{
		{"a failed member's put-back", func(f *repFix) {
			failOutOfSlot(f.node, f.mdDev, f.leg1)
		}, func(f *repFix) []string {
			return []string{f.remove(f.leg1), f.add(f.leg1)}
		}},
		{"the add of a member the array lacks", func(f *repFix) {
			f.node.mu.Lock()
			f.node.mdRemove([]string{f.mdDev, "--remove", f.leg1})
			f.node.mu.Unlock()
		}, func(f *repFix) []string {
			return []string{f.add(f.leg1)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			tc.set(f)
			legTwoConnecting(f)
			_, reply := f.read(t, 2)
			if reply.GetCode() != 0 {
				t.Fatalf("the read named %v with no source serving, want "+
					"nothing", reply)
			}
			f.node.Reset()
			f.syncup(t, noRepairReq(2))
			for _, cmd := range tc.cmds(f) {
				assertNoCall(t, f.node, cmd)
			}
			if !retrying(t, f.srv) {
				t.Fatalf("the source's leg that does not serve registered " +
					"no retry")
			}

			legTwoLive(f)
			if _, reply := f.read(t, 2); !strings.Contains(
				reply.GetDetails(), legName(f.srv, testDataLeg)) {
				t.Fatalf("the read with the source serving names %v, want "+
					"leg 1's member", reply)
			}
			f.node.Reset()
			retryAttempt(t, f.srv)
			for _, cmd := range tc.cmds(f) {
				if !f.node.hasCall(cmd) {
					t.Fatalf("the attempt with the source serving made "+
						"%q, want %q", f.mdCalls(), cmd)
				}
			}
		})
	}
}

// failLegProbe runs one probe round of a leg whose IO through its wrapper
// fails, so its row reads ERROR while its path serves.
func failLegProbe(t *testing.T, f *repFix, legId uint64) {
	t.Helper()
	st := legState(t, f.srv)
	f.srv.mu.Lock()
	var prober *legProber
	for _, p := range st.probers {
		if p.legId == legId {
			prober = p
		}
	}
	f.srv.mu.Unlock()
	if prober == nil {
		t.Fatalf("no prober of leg %#x", legId)
	}
	f.node.mu.Lock()
	devNo := f.node.devNo[prober.path]
	delete(f.node.devNo, prober.path)
	f.node.mu.Unlock()
	f.srv.runLegProbe(context.Background(), prober, testCn)
	f.node.mu.Lock()
	f.node.devNo[prober.path] = devNo
	f.node.mu.Unlock()
}

// TestWhichMemberFeedsACopy pins the source test's edges (CN12): a source
// leg whose row reads PENDING, as before a new primary's first probe,
// counts; one whose row reads ERROR does not, nor a member md shows faulty
// beside in_sync; and a repair's bad leg in sync on a serving leg counts,
// since nothing leaves the array when a member joins.
func TestWhichMemberFeedsACopy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) (*repFix, *pb.SyncupCntlrRequest, string)
		joins bool
	}{
		{"a source leg whose row reads PENDING", func(t *testing.T) (
			*repFix, *pb.SyncupCntlrRequest, string) {
			f := newRepFix(t, testMetaLeg, testDataLeg)
			failOutOfSlot(f.node, f.mdDev, f.leg1)
			return f, noRepairReq(2), f.leg1
		}, true},
		{"a source leg whose row reads ERROR", func(t *testing.T) (
			*repFix, *pb.SyncupCntlrRequest, string) {
			f := newRepFix(t, allLegs...)
			failLegProbe(t, f, testDataLeg2)
			failOutOfSlot(f.node, f.mdDev, f.leg1)
			return f, noRepairReq(2), f.leg1
		}, false},
		{"a source md shows faulty beside in_sync", func(t *testing.T) (
			*repFix, *pb.SyncupCntlrRequest, string) {
			f := newRepFix(t, allLegs...)
			failOutOfSlot(f.node, f.mdDev, f.leg1)
			f.node.setMemberState(f.mdDev, f.leg2, "in_sync,faulty,failfast")
			return f, noRepairReq(2), f.leg1
		}, false},
		{"a repair's bad leg as the only source", func(t *testing.T) (
			*repFix, *pb.SyncupCntlrRequest, string) {
			f := newRepFix(t, allLegs...)
			req := repairReq(3, pb.RepStage_REP_STAGE_CREATED, false)
			f.syncup(t, req)
			failOutOfSlot(f.node, f.mdDev, f.leg2)
			return f, req, f.leg2
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, req, member := tc.setup(t)
			_, reply := f.read(t, req.GetRevision())
			named := strings.Contains(reply.GetDetails(),
				"md_member:"+strings.TrimPrefix(member, "/dev/mapper/"))
			if named != tc.joins {
				t.Fatalf("the read names %v, want the member named: %v",
					reply, tc.joins)
			}
			f.node.Reset()
			f.syncup(t, req)
			if got := f.node.hasCall(f.add(member)); got != tc.joins {
				t.Fatalf("the converge made %q, want the member added: %v",
					f.mdCalls(), tc.joins)
			}
		})
	}
}
