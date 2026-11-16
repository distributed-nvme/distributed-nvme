package worker

import (
	"errors"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The migration of the fixture's data leg A that the tests below plant: its
// destination side, on dn D, and its record's name and id.
const (
	migrEndName = "migr0"
	migrEndId   = uint64(77)
	migrEndDst  = uint64(999)
	// wholeClone and halfClone are a dm-clone status line with every
	// region hydrated, and one with half (common.ParseCloneStatus).
	wholeClone = "0 20480 clone 8 1/1024 2048 10/10 0 0 0"
	halfClone  = "0 20480 clone 8 1/1024 2048 5/10 0 0 0"
)

// withLegMigration makes the fixture's data leg A bad past the leg
// threshold and gives it a migration to dn D, its cutover pending or not,
// and, unless dst is "", the destination side's child a latest report whose
// dm-clone row carries dst.
func withLegMigration(h *reactHarness, pending bool, dst string) {
	h.t.Helper()
	leg := h.legOf(reactDataLegA)
	leg.ErrEpoch = h.ago(9000)
	src := leg.GetSideList()[0].GetSideId()
	leg.SideList = append(leg.SideList, &pb.Side{
		SideId: migrEndDst, AddrPort: reactDnD, Provisioned: true,
	})
	if h.state.Migrs == nil {
		h.state.Migrs = map[string]*pb.Migration{}
	}
	h.state.Migrs[migrEndName] = &pb.Migration{
		MigrId: migrEndId, SrcSideId: src, DstSideId: migrEndDst,
		CutoverPending: pending,
	}
	h.state.Conf.MigrNameList = []string{migrEndName}
	if dst == "" {
		return
	}
	h.w.sides[sideKey{legId: reactDataLegA, sideId: migrEndDst}] =
		&sideChild{driver: &sideDriver{lastInfo: &pb.SideInfo{
			MigrDstInfo: &pb.SideInfo_MigrDstInfo{
				DmCloneInfo: &pb.ResInfo{
					Status: pb.ResStatus_RES_STATUS_OK, Details: dst,
				},
			},
		}}}
}

// TestLegRepairEndsABadLegsMigration pins AR8's end of a bad leg's
// migration in its safe direction: a cancel while the cutover is pending,
// a finish once the cutover has started and the destination's latest report
// shows its copy whole, and nothing, with a record why, while it shows the
// copy not whole or shows nothing; the operator's holds come first; a leg
// that is a candidate only for md bad-block ranges keeps its migration; a
// refusal holds this leg alone, the pass going on, and another failure ends
// the pass, its transaction maybe committed.
func TestLegRepairEndsABadLegsMigration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending bool
		dst     string
		op      string
		kind    string
		reason  string
	}{
		{"the cutover pending", true, "", "cancel_migration",
			reactionMigrationCancel, ""},
		{"the copy whole", false, wholeClone, "finish_migration",
			reactionMigrationFinish, ""},
		{"the copy not whole", false, halfClone, "", "",
			reasonMigrCopyNotWhole},
		{"no report of the copy", false, "", "", "",
			reasonMigrCopyNotWhole},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			withLegMigration(h, tc.pending, tc.dst)
			h.dnCands(reactDnC)
			h.pass()
			if tc.op == "" {
				h.wantOps()
				h.wantOneSkip(reactionRepairCreate, tc.reason,
					reactDataGrp, reactDataLegA)
				return
			}
			calls := h.wantOps(tc.op)
			if calls[0].repName != migrEndName || calls[0].repId != migrEndId {
				t.Fatalf("%s %+v, want %s at %d", tc.op, calls[0],
					migrEndName, migrEndId)
			}
			rec := h.wantApplied(tc.kind)[0]
			if rec["migr_name"] != migrEndName ||
				rec["leg_id"] != float64(reactDataLegA) {
				t.Fatalf("the record %v", rec)
			}
		})
	}
	t.Run("the operator in charge", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withLegMigration(h, true, "")
		h.dataGrp().OperatorInCharge = true
		h.pass()
		h.wantOps()
		h.wantOneSkip(reactionRepairCreate, reasonGrpOperatorInCharge,
			reactDataGrp, reactDataLegA)
	})
	t.Run("no migration of the leg", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withLegMigration(h, true, "")
		h.state.Conf.MigrNameList = nil
		h.pass()
		h.wantOps()
		h.wantOneSkip(reactionRepairCreate, reasonTwoSides, reactDataGrp,
			reactDataLegA)
	})
	t.Run("a leg with ranges alone keeps its migration", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withLegMigration(h, true, "")
		h.legOf(reactDataLegA).ErrEpoch = 0
		setRanges(h, reactDataLegA)
		h.pass()
		h.wantOps()
		h.wantOneSkip(reactionRepairCreate, reasonTwoSides, reactDataGrp,
			reactDataLegA)
	})
	t.Run("a failure ends the pass", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		meta := h.legOf(reactMetaLegA)
		meta.ErrEpoch = h.ago(9000)
		meta.SideList = append(meta.SideList, &pb.Side{
			SideId: migrEndDst, AddrPort: reactDnD, Provisioned: true,
		})
		h.state.Migrs = map[string]*pb.Migration{migrEndName: {
			MigrId: migrEndId, SrcSideId: meta.GetSideList()[0].GetSideId(),
			DstSideId: migrEndDst, CutoverPending: true,
		}}
		h.state.Conf.MigrNameList = []string{migrEndName}
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.rops.repairErr = map[string]error{
			migrEndName: errors.New("etcd down")}
		h.dnCands(reactDnC, reactDnD)
		h.pass()
		h.wantOps("cancel_migration")
	})
	t.Run("a refusal goes on to the next leg", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		// The migration on the meta group's leg A, whose id is the
		// smaller, and data leg A bad beside it.
		meta := h.legOf(reactMetaLegA)
		meta.ErrEpoch = h.ago(9000)
		meta.SideList = append(meta.SideList, &pb.Side{
			SideId: migrEndDst, AddrPort: reactDnD, Provisioned: true,
		})
		h.state.Migrs = map[string]*pb.Migration{migrEndName: {
			MigrId: migrEndId, SrcSideId: meta.GetSideList()[0].GetSideId(),
			DstSideId: migrEndDst, CutoverPending: true,
		}}
		h.state.Conf.MigrNameList = []string{migrEndName}
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.rops.repairErr = map[string]error{migrEndName: &model.ErrPrecondition{
			Op: "CancelMigration", Reason: model.ReasonCutoverStarted}}
		h.dnCands(reactDnC, reactDnD)
		h.pass()
		ops := h.rops.allCalls()
		if len(ops) != 2 || ops[0].op != "cancel_migration" ||
			ops[1].op != "create_repair" ||
			ops[1].badLegId != reactDataLegA {
			t.Fatalf("ops %+v, want the refused cancel, then data leg A's "+
				"create", ops)
		}
	})
}
