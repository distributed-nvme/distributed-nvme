package worker

import (
	"context"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestLegRowsOnlyFromAnEnabledPrimaryAtItsRevision pins HL2's gate on the
// rows a cntlr child reports: leg rows and path rows count from a reply of a
// cntlr its plan makes the primary and not disabled, at the plan's own
// revision; a disabled primary's reply, which runs the standby shape, and a
// reply at an older revision, which a promoted cntlr's can be, report
// neither; a disabled primary's leg rows are logged as a standby's, and an
// enabled primary's are never; and the coordinator records no leg row of a
// cntlr whose latest plan is a disabled primary.
func TestLegRowsOnlyFromAnEnabledPrimaryAtItsRevision(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, disabled bool) (*cntlrDriver, *spWorker,
		*fakeHealthWriter, *logCapture) {
		t.Helper()
		logs := captureLogs(t)
		d := newTestDeps(
			testConfig(common.WorkerRoleSp), newFakeStore(), newFakeClock(),
		)
		hw := &fakeHealthWriter{}
		d.health = hw
		w := spTestWorker(d)
		w.verdicts = newVerdictMemo()
		w.legSlice[spLegMeta] = spSliceA
		plan := settlePlan(true, false)
		plan.req.Cntlr = &pb.Cntlr{Primary: true, Disabled: disabled}
		w.cntlrs[spCntlrPrimary] = &cntlrChild{plan: plan}
		return newCntlrDriver(w, nil, plan), w, hw, logs
	}
	rows := func(t *testing.T, driver *cntlrDriver, w *spWorker,
		revision uint64) (int, bool) {
		t.Helper()
		info := pathInfo(spSideDst)
		info.LegIdToLeg = map[uint64]*pb.ResInfo{
			spLegMeta: resErr("leg-meta", "probe io error"),
		}
		driver.storeInfo(info, 0)
		driver.setStreamInfo(info)
		driver.observe(ctx, &replyState{
			revision: revision, infoPresent: true, fromRound: true,
		})
		select {
		case rep := <-w.reportCh:
			return len(rep.legRows), !rep.paths.empty()
		default:
			return 0, false
		}
	}
	for _, tc := range []struct {
		name     string
		disabled bool
		revision uint64
		want     bool
		logged   int
	}{
		{"an enabled primary at its revision", false, 7, true, 0},
		{"a disabled primary", true, 7, false, 1},
		{"a reply at the revision before", false, 6, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver, w, _, logs := setup(t, tc.disabled)
			legs, paths := rows(t, driver, w, tc.revision)
			if (legs != 0) != tc.want || paths != tc.want {
				t.Fatalf("leg rows %d, paths %v; want rows and paths: %v",
					legs, paths, tc.want)
			}
			if got := len(logs.withMsg(msgSpStandbyLegRow)); got != tc.logged {
				t.Fatalf("%d standby leg row records, want %d", got,
					tc.logged)
			}
		})
	}
	t.Run("recorded only for an enabled primary", func(t *testing.T) {
		_, w, hw, _ := setup(t, true)
		w.handleReports(spReport{
			cntlrId: spCntlrPrimary,
			legRows: []legRow{{
				legId: spLegMeta, obs: healthErrorRow, resName: "leg-meta",
			}},
		})
		for _, write := range hw.all() {
			if write.record == healthRecordLeg {
				t.Fatalf("a disabled primary's leg row was recorded: %+v",
					write)
			}
		}
	})
}
