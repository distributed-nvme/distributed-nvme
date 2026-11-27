package cnagent

import (
	"errors"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// recopyReq is noRepairReq with the data group's first leg marked to be
// copied again at revision mark (Leg.recopy_revision), as a forced
// migration cancel after the cutover leaves it; zero marks nothing.
func recopyReq(revision, mark uint64) *pb.SyncupCntlrRequest {
	req := noRepairReq(revision)
	for _, leg := range dataGrpOf(req).GetLegList() {
		if leg.GetLegId() == testDataLeg {
			leg.RecopyRevision = mark
		}
	}
	return req
}

// zeroCalls are the superblock zeroings the node ran on path since its last
// Reset.
func (f *repFix) zeroCalls(path string) int {
	return len(f.node.callsMatching("cmd mdadm --zero-superblock " +
		mdMetadata + " " + path))
}

// TestARecopyMarkCopiesTheLegAgain pins CN12's recopy: a leg whose request
// carries a recopy mark this incarnation has not acted on, served by its
// own side alone and beside another member in sync, leaves the array, loses
// its md superblock and is added fresh, once per mark; both channels report
// the mark's revision in the group's reading; a request with the mark gone
// drops the memory. With no other member in sync nothing is done, the read
// names nothing, and once the other leg serves again the read names the
// member and the converge it brings copies the leg.
func TestARecopyMarkCopiesTheLegAgain(t *testing.T) {
	t.Run("the leg is copied again once", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.node.Reset()
		info := f.syncup(t, recopyReq(3, 3))
		f.assertMdCalls(t, "the recopy", f.fail(f.leg1), f.remove(f.leg1),
			f.addFresh(f.leg1))
		if n := f.zeroCalls(f.leg1); n != 1 {
			t.Fatalf("the recopy zeroed leg 1's superblock %d times, want "+
				"once", n)
		}
		if got := info.GetGrpIdToGrpInfo()[testDataGrp].
			GetLegIdToRecopyRevision()[testDataLeg]; got != 3 {
			t.Fatalf("the converge's reading names recopy revision %d, "+
				"want 3", got)
		}
		read, reply := f.read(t, 3)
		if strings.Contains(reply.GetDetails(), agent.LeftoverKindMdMember) {
			t.Fatalf("the read after the recopy names a member: %v", reply)
		}
		if got := read.GetGrpIdToGrpInfo()[testDataGrp].
			GetLegIdToRecopyRevision()[testDataLeg]; got != 3 {
			t.Fatalf("the read's reading names recopy revision %d, want 3",
				got)
		}
		f.node.Reset()
		f.syncup(t, recopyReq(4, 3))
		f.assertMdCalls(t, "a converge of the same mark")
		info = f.syncup(t, recopyReq(5, 0))
		if got := info.GetGrpIdToGrpInfo()[testDataGrp].
			GetLegIdToRecopyRevision(); len(got) != 0 {
			t.Fatalf("the reading after the clear names %v", got)
		}
		st := f.srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
		if len(st.recopies) != 0 {
			t.Fatalf("the clear left the memory %v", st.recopies)
		}
		f.node.Reset()
		f.syncup(t, recopyReq(6, 6))
		f.assertMdCalls(t, "a new mark", f.fail(f.leg1), f.remove(f.leg1),
			f.addFresh(f.leg1))
	})
	t.Run("not while another side serves the leg", func(t *testing.T) {
		// A live controller at the cancelled migration's new side's
		// address: a fresh add could copy onto it, so the converge copies
		// nothing and reads the leg row RES_REASON_LEG_PATH_NOT_SOLE. The
		// converge's sweep disconnects that controller, as its address is
		// no side of the leg; the read then finds the leg's side alone,
		// reads the row OK with no reason and names the leg's member, and
		// the converge that name brings copies the leg.
		f := newRepFix(t, allLegs...)
		nqn := legNqn(f.srv, testDataLeg)
		f.node.mu.Lock()
		subsys := f.node.subsystems[nqn]
		f.node.addCtrl(subsys, "10.9.9.9", testSvcId, "")
		stray := subsys.ctrls[len(subsys.ctrls)-1].name
		f.node.mu.Unlock()
		f.node.Reset()
		info := f.syncup(t, recopyReq(3, 3))
		if n := f.zeroCalls(f.leg1); n != 0 || len(f.mdCalls()) != 0 {
			t.Fatalf("the recopy ran while another side served the leg: %v",
				f.mdCalls())
		}
		if got := info.GetLegIdToLeg()[testDataLeg].GetReason(); got !=
			pb.ResReason_RES_REASON_LEG_PATH_NOT_SOLE {
			t.Fatalf("the converge's leg row reads reason %v with two "+
				"live controllers, want RES_REASON_LEG_PATH_NOT_SOLE", got)
		}
		if calls := f.node.callsMatching("--device " + stray); len(calls) == 0 {
			t.Fatalf("the converge did not cut the new side's controller")
		}
		read, reply := f.read(t, 3)
		if !strings.Contains(reply.GetDetails(), legName(f.srv,
			testDataLeg)) {
			t.Fatalf("the read once the old side alone serves names %v, "+
				"want leg 1's member", reply)
		}
		if row := read.GetLegIdToLeg()[testDataLeg]; row.GetStatus() !=
			pb.ResStatus_RES_STATUS_OK ||
			row.GetReason() != pb.ResReason_RES_REASON_NONE {
			t.Fatalf("the read's leg row once the old side alone serves "+
				"reads %v, want OK with no reason", row)
		}
		f.node.Reset()
		f.syncup(t, recopyReq(3, 3))
		f.assertMdCalls(t, "the recopy the read brought", f.fail(f.leg1),
			f.remove(f.leg1), f.addFresh(f.leg1))
	})
	t.Run("one leg of a group a pass", func(t *testing.T) {
		// Both legs marked, as two forced cancels leave them: one pass
		// copies one leg again, and the other waits until that one is back
		// in sync, so the group's last member in sync is never failed.
		f := newRepFix(t, allLegs...)
		req := recopyReq(3, 3)
		for _, leg := range dataGrpOf(req).GetLegList() {
			leg.RecopyRevision = 3
		}
		f.node.Reset()
		f.syncup(t, req)
		failed := 0
		for _, call := range f.mdCalls() {
			if strings.Contains(call, " --fail ") {
				failed++
			}
		}
		if failed != 1 {
			t.Fatalf("one pass over two marked legs failed %d members: %v",
				failed, f.mdCalls())
		}
	})
	t.Run("only beside a member in sync", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.leg2Dead()
		f.syncup(t, noRepairReq(3))
		f.node.Reset()
		f.syncup(t, recopyReq(4, 4))
		if calls := f.mdCalls(); len(calls) != 0 &&
			strings.Contains(strings.Join(calls, " "), f.leg1) {
			t.Fatalf("leg 1 was copied again with no copy source: %v",
				calls)
		}
		if n := f.zeroCalls(f.leg1); n != 0 {
			t.Fatalf("leg 1's superblock zeroed with no copy source")
		}
		_, reply := f.read(t, 4)
		if strings.Contains(reply.GetDetails(), "md_member:"+
			legName(f.srv, testDataLeg)) {
			t.Fatalf("the read names leg 1 with no copy source: %v", reply)
		}
	})
}

// TestTheServingSideIsTheOnlyLiveController pins CN28's serving side
// (subsysView.servingSide), whose absence a leg row of a leg with one side
// reads as RES_REASON_LEG_PATH_NOT_SOLE: a leg is served by its side when
// every
// live controller of its subsystem has that side's address and one reads
// ANA optimized; another controller that is live or reconnects, as a
// cancelled migration's new side's, a controller whose address did not
// read, and no optimized one give no serving side, and a dead controller
// does not count.
func TestTheServingSideIsTheOnlyLiveController(t *testing.T) {
	lp := &legPlan{sides: []*pb.Side{sideOf(41, "10.0.0.1", "4420")}}
	own := ctrlView{name: "nvme1", trAddr: "10.0.0.1", trSvcId: "4420",
		state: "live", anaState: "optimized"}
	other := ctrlView{name: "nvme2", trAddr: "10.0.0.2", trSvcId: "4420",
		state: "live", anaState: "optimized"}
	for _, tc := range []struct {
		name  string
		ctrls []ctrlView
		want  uint64
	}{
		{"its own side alone", []ctrlView{own}, 41},
		{"another live side", []ctrlView{own, other}, 0},
		{"another side that reconnects", []ctrlView{own, func() ctrlView {
			c := other
			c.state = "connecting"
			return c
		}()}, 0},
		{"another side that is dead", []ctrlView{own, func() ctrlView {
			c := other
			c.state = "dead"
			return c
		}()}, 41},
		{"an address that did not read", []ctrlView{own, {name: "nvme3",
			addrUnknown: errors.New("read failed")}}, 0},
		{"no optimized path", []ctrlView{func() ctrlView {
			c := own
			c.anaState = "inaccessible"
			return c
		}()}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := &subsysView{found: true, ctrls: tc.ctrls}
			if got := view.servingSide(lp); got != tc.want {
				t.Fatalf("serving side %d, want %d", got, tc.want)
			}
		})
	}
}
