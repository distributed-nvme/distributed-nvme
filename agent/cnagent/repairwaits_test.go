package cnagent

import (
	"fmt"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// wantWaits checks the waits text of the info's verdict on the fixture
// repair.
func wantWaits(t *testing.T, info *pb.CntlrInfo, label, want string) {
	t.Helper()
	got := verdict(info)
	if got == nil {
		t.Fatalf("%s: no verdict on the repair: %v", label,
			info.GetRepIdToRepairInfo())
	}
	if got.GetWaits() != want {
		t.Fatalf("%s: waits %q, want %q", label, got.GetWaits(), want)
	}
}

// TestRepairVerdictSaysWhyItWaits pins CN12's Why a repair waits: the
// verdict on a repair the primary holds back names why, on a read and on a
// converge alike — a new leg whose side is not provisioned yet, a new leg
// the read cannot take yet, no member in sync to feed a started repair's
// copy or, on a leg whose row reads OK, to keep the data, a bad leg md holds
// in its place while it serves, or one with md bad-block ranges beside no
// clean member, a new leg md holds without a place under an abort, a bad
// leg md holds after every new leg took a place — and a verdict that moves
// the repair, started, an abort after the add included, or abort confirmed,
// names nothing.
func TestRepairVerdictSaysWhyItWaits(t *testing.T) {
	t.Run("a new leg whose side is not provisioned", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.badLegDead()
		info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED,
			false, repOpts{unprovisioned: true}))
		want := fmt.Sprintf(waitsNewLegUnprovisioned, repNewLeg)
		wantWaits(t, info, "the converge", want)
		read, _ := f.read(t, 3)
		wantWaits(t, read, "the read", want)
	})
	t.Run("a new leg not probed yet", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.badLegDead()
		f.node.mu.Lock()
		f.node.failCmd[" --add --failfast "+f.newLegDev] =
			"mdadm: add new device failed"
		f.node.mu.Unlock()
		f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
		read, _ := f.read(t, 3)
		wantWaits(t, read, "the read before the new leg's probe",
			fmt.Sprintf(waitsNewLegUnavailable, repNewLeg))
		probeLegs(t, f.srv, repNewLeg)
		read, _ = f.read(t, 3)
		wantWaits(t, read, "the read once the new leg serves", "")
		info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED,
			false))
		assertVerdict(t, info, "the add", true, false)
		wantWaits(t, info, "the add", "")
	})
	t.Run("a started repair with no copy source", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.badLegDead()
		f.legDead(testDataLeg2, testIp2, testSvcId2)
		info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_STARTED,
			false))
		wantWaits(t, info, "the converge",
			fmt.Sprintf(waitsNoCopySource, repNewLeg))
	})
	t.Run("no member keeps the data", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
		probeLegs(t, f.srv, repNewLeg)
		f.badLegDead()
		f.copying(f.leg2, "1")
		read, _ := f.read(t, 3)
		wantWaits(t, read, "the read while leg 2 copies", waitsNoKeeper)
	})
	t.Run("no member in sync on a leg whose row reads ok", func(t *testing.T) {
		f := newRepFix(t, testMetaLeg, testDataLeg)
		f.badLegDead()
		info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED,
			false))
		wantWaits(t, info, "the converge", waitsNoKeeper)
	})
	t.Run("ranges on every member in sync", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.node.mu.Lock()
		_, array := f.node.arrayByDev(f.mdDev)
		array.badBlocks = map[string]string{
			f.leg1: "24064 128\n", f.leg2: "40000 8\n"}
		f.node.publishArray(array)
		f.node.mu.Unlock()
		info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED,
			false))
		wantWaits(t, info, "the converge",
			fmt.Sprintf(waitsNoCleanKeeper, testDataLeg))
	})
	t.Run("the bad leg serves", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		info := f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED,
			false))
		want := fmt.Sprintf(waitsBadLegServes, testDataLeg)
		wantWaits(t, info, "the converge", want)
		probeLegs(t, f.srv, repNewLeg)
		read, _ := f.read(t, 3)
		wantWaits(t, read, "the read", want)
	})
	t.Run("an abort md has not let go of", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.badLegDead()
		f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
		probeLegs(t, f.srv, repNewLeg)
		failOutOfSlot(f.node, f.mdDev, f.newLegDev)
		f.node.mu.Lock()
		f.node.failCmd[f.remove(f.newLegDev)] = "mdadm: busy"
		f.node.mu.Unlock()
		f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_CREATED, true))
		read, _ := f.read(t, 4)
		wantWaits(t, read, "the read while md holds the new leg",
			fmt.Sprintf(waitsNewLegHeld, repNewLeg))
		info := f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_CREATED,
			true))
		assertVerdict(t, info, "the abort's removal", false, true)
		wantWaits(t, info, "the abort's removal", "")
	})
	t.Run("a failed new leg with no abort flag", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.badLegDead()
		f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
		probeLegs(t, f.srv, repNewLeg)
		failOutOfSlot(f.node, f.mdDev, f.newLegDev)
		read, _ := f.read(t, 3)
		wantWaits(t, read, "the read of the failed new leg", "")
	})
	for _, abort := range []bool{false, true} {
		t.Run(fmt.Sprintf("the bad leg back, abort flag %v", abort),
			func(t *testing.T) {
				f := newRepFix(t, allLegs...)
				f.badLegDead()
				f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED,
					false))
				f.node.mu.Lock()
				f.node.mdAdd([]string{f.mdDev, "--add", "--failfast",
					f.leg1})
				f.node.mu.Unlock()
				f.node.setMemberState(f.mdDev, f.leg1, "spare,failfast")
				f.node.setMemberSlot(f.mdDev, f.leg1, mdSlotNone)
				f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_CREATED,
					abort))
				read, _ := f.read(t, 4)
				wantWaits(t, read, "the read after the converge", "")
				f.node.mu.Lock()
				f.node.mdAdd([]string{f.mdDev, "--add", "--failfast",
					f.leg1})
				f.node.mu.Unlock()
				f.node.setMemberState(f.mdDev, f.leg1, "spare,failfast")
				f.node.setMemberSlot(f.mdDev, f.leg1, mdSlotNone)
				read, _ = f.read(t, 4)
				wantWaits(t, read, "the read of the bad leg back",
					fmt.Sprintf(waitsBadLegHeld, testDataLeg))
			})
	}
	t.Run("an abort after the add", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.badLegDead()
		f.syncup(t, repairReq(3, pb.RepStage_REP_STAGE_CREATED, false))
		info := f.syncup(t, repairReq(4, pb.RepStage_REP_STAGE_CREATED,
			true))
		assertVerdict(t, info, "the abort after the add", true, false)
		wantWaits(t, info, "the abort after the add", "")
	})
}
