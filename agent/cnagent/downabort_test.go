package cnagent

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestAbortConfirmedWithNoArray pins CN12's confirmation of a CREATED
// repair's abort while no array of the group is there: the case of a repair
// whose bad leg came back with an older copy after the group's other leg
// died for good, while a mark from that leg waits for the other leg. md
// holds nothing then, and md writes its superblock onto a member at its add,
// so a new leg that reads no superblock, on a leg that serves, never joined
// the array: the converge and the read confirm the abort, which lets the sp
// worker move the repair to ABORTED and the operator cancel it. A new leg
// that carries a superblock, one whose leg does not serve, and a repair
// whose abort flag is clear are confirmed by neither channel.
func TestAbortConfirmedWithNoArray(t *testing.T) {
	for _, tc := range []struct {
		name    string
		abort   bool
		setup   func(f *repFix)
		confirm bool
	}{
		{"the abort flag set, the new leg blank", true, nil, true},
		{"the abort flag clear", false, nil, false},
		{"the new leg carries a superblock", true, func(f *repFix) {
			f.node.setSuperblock(f.newLegDev, fakeSuperblock{
				role: "spare", events: 40})
		}, false},
		{"the new leg does not serve", true, func(f *repFix) {
			f.legDead(repNewLeg, testIp, testSvcId)
		}, false},
		{"the new leg's superblock read fails", true, func(f *repFix) {
			f.node.mu.Lock()
			f.node.failCmdAlways[examineOf(f.newLegDev)] = "read failed"
			f.node.mu.Unlock()
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := downFix(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED,
				tc.abort, repOpts{standby: true}))
			// The other leg is gone for good, and the mark names the bad
			// leg, which came back: the forced start waits for leg 2.
			f.leg2Dead()
			if tc.setup != nil {
				tc.setup(f)
			}
			req := withMark(repairReq(4, pb.RepStage_REP_STAGE_CREATED,
				tc.abort, repOpts{noSubsys: true}), testDataLeg)
			info := f.syncup(t, req)
			if !f.node.arrayGone(f.mdDev) {
				t.Fatalf("an array of the group runs")
			}
			assertNoCall(t, f.node, "--zero-superblock")
			got := verdict(info)
			if tc.confirm {
				assertVerdict(t, info, "the converge", false, true)
			} else if got.GetAbortConfirmed() {
				t.Fatalf("the converge confirms the abort: %v", got)
			}
			read, _ := f.read(t, 4)
			got = verdict(read)
			if tc.confirm {
				assertVerdict(t, read, "the read", false, true)
			} else if got.GetAbortConfirmed() {
				t.Fatalf("the read confirms the abort: %v", got)
			}
		})
	}
}
