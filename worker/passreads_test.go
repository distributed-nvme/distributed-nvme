package worker

import (
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestAPassReadsTheNodeRecordsOnlyWhenAStepNeedsThem pins AR1's load: a pass
// loads the pool without its node records, and reads them once, at the
// revision of its load, only when a step is about to allocate or needs a
// node's record. An idle pass reads none, nor does a grow pending on the CN
// with no side to withdraw it for; a withdrawal reads them; two creates of
// one pass read them once, and each create's scan still excludes its group's
// locations; a failed read records the step's failure and ends the pass, as
// a failed scan does (AR2).
func TestAPassReadsTheNodeRecordsOnlyWhenAStepNeedsThem(t *testing.T) {
	t.Run("an idle pass", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.pass()
		h.wantOps()
		if got := h.ops.passLoadCnt(); got != 1 {
			t.Fatalf("%d pass loads, want 1", got)
		}
		if got := h.ops.nodeLoadCnt(); got != 0 {
			t.Fatalf("an idle pass read the node records %d times", got)
		}
	})
	t.Run("a grow pending on the CN", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withGrownGroup(t, h, 0)
		h.pass()
		h.wantSkipped(reactionGrowData, reasonGrowPending)
		if got := h.ops.nodeLoadCnt(); got != 0 {
			t.Fatalf("a pending grow with no side to withdraw for read the "+
				"node records %d times", got)
		}
	})
	t.Run("a grow to withdraw", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		withGrownGroup(t, h, h.ago(9000))
		h.pass()
		h.wantOps("withdraw_grow")
		if got := h.ops.nodeLoadCnt(); got != 1 {
			t.Fatalf("%d node reads, want one for the withdrawal", got)
		}
	})
	t.Run("two creates", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactMetaLegA).ErrEpoch = h.ago(9000)
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.state.DnByAddr[reactDnA] = &pb.DnConf{Location: "rack0"}
		h.state.DnByAddr[reactDnB] = &pb.DnConf{Location: "rack1"}
		h.dnCands(reactDnD)
		metaRep := h.workerRepNameOf(reactMetaGrp)
		h.rops.repairErr = map[string]error{metaRep: &model.ErrPrecondition{
			Op: "CreateRepair", Reason: model.ReasonGrpOperatorInCharge,
		}}
		h.pass()
		h.wantOps("create_repair", "create_repair")
		if got := h.ops.nodeLoadCnt(); got != 1 {
			t.Fatalf("%d node reads, want one for the pass", got)
		}
		for i, q := range h.rops.allQueries() {
			if fmt.Sprintf("%q", q.excludeLocs) != `["rack0" "rack1"]` {
				t.Fatalf("scan %d excluded %q, want both racks", i,
					q.excludeLocs)
			}
		}
	})
	t.Run("a failed read", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnD)
		h.ops.nodesErr = errors.New("etcd down")
		h.pass()
		h.wantOps()
		if got := h.rops.allQueries(); len(got) != 0 {
			t.Fatalf("scans %+v after a failed node read", got)
		}
		recs := h.logs.withMsg(msgReactionSkipped)
		if len(recs) != 1 || recs[0]["kind"] != reactionRepairCreate ||
			recs[0]["reason"] != reasonOpFailed ||
			recs[0]["error"] != "etcd down" {
			t.Fatalf("records %v, want the create's failed read", recs)
		}
	})
}

// TestThePassStartsAfterItsOffset pins AR1's offset: the coordinator's first
// pass comes deps.passOffset after its start, and the next ones one
// cntlr_interval apart from it.
func TestThePassStartsAfterItsOffset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSpHarness(t)
		h.addFixtureAgents()
		period := time.Duration(
			testClusterConf().GetHealthCheckConf().GetCntlrInterval(),
		) * time.Second
		offset := period / 4
		h.deps.passOffset = func(p time.Duration) time.Duration {
			if p != period {
				t.Errorf("the offset is drawn for %v, want %v", p, period)
			}
			return offset
		}
		h.start()
		step := func(d time.Duration, want int) {
			t.Helper()
			h.clk.advance(d)
			synctest.Wait()
			if got := h.ops.passLoadCnt(); got != want {
				t.Fatalf("%d passes, want %d", got, want)
			}
		}
		step(0, 0)
		step(offset-time.Millisecond, 0)
		step(time.Millisecond, 1)
		step(period-time.Millisecond, 1)
		step(time.Millisecond, 2)
	})
}

// TestAGrowIsTimedFromTheFirstPassPastTheMark pins AR6's `grow done`: the
// seconds from the first pass that read the pool past its mark to the pass
// that read a larger total, once; a pool read under its mark again at the
// same total is forgotten, and its later growth logs nothing, as does the
// growth of a pool never read past its mark and of one whose breach the
// coordinator's first reading of the pool rows already found.
func TestAGrowIsTimedFromTheFirstPassPastTheMark(t *testing.T) {
	// The coordinator's first reading of the pool rows finds no breach, so
	// that every later breach has a pass of its own that first read it.
	setup := func(t *testing.T) *reactHarness {
		h := newReactHarness(t, reactFixture(t))
		h.state.Conf.BdevConf.DmPoolConf.LowWaterMarkPct = 80
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(10, 100, 10, 100))
		h.pass()
		return h
	}
	done := func(h *reactHarness) []map[string]any {
		return h.logs.withMsg(msgGrowDone)
	}
	t.Run("timed once", func(t *testing.T) {
		h := setup(t)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(10, 100, 90, 100))
		h.pass()
		h.clk.advance(30 * time.Second)
		h.pass()
		if got := done(h); len(got) != 0 {
			t.Fatalf("records %v before the pool grew", got)
		}
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(10, 100, 90, 200))
		h.clk.advance(12 * time.Second)
		h.pass()
		h.clk.advance(10 * time.Second)
		h.pass()
		got := done(h)
		if len(got) != 1 || got[0]["kind"] != reactionGrowData ||
			got[0]["slice_id"] != float64(reactSliceId) ||
			got[0]["seconds"] != float64(42) ||
			got[0]["old_total"] != float64(100) ||
			got[0]["new_total"] != float64(200) {
			t.Fatalf("records %v, want one data grow of 42 s", got)
		}
	})
	t.Run("forgotten under the mark", func(t *testing.T) {
		h := setup(t)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(10, 100, 90, 100))
		h.pass()
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(10, 100, 50, 100))
		h.pass()
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(10, 100, 50, 200))
		h.pass()
		if got := done(h); len(got) != 0 {
			t.Fatalf("records %v for a pool that fell under its mark", got)
		}
	})
	t.Run("never past the mark", func(t *testing.T) {
		h := setup(t)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(10, 100, 50, 100))
		h.pass()
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(10, 100, 50, 200))
		h.pass()
		if got := done(h); len(got) != 0 {
			t.Fatalf("records %v for a pool never past its mark", got)
		}
	})
	t.Run("a breach the first reading finds", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		h.state.Conf.BdevConf.DmPoolConf.LowWaterMarkPct = 80
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(10, 100, 90, 100))
		h.pass()
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(10, 100, 190, 200))
		h.clk.advance(7 * time.Second)
		h.pass()
		if got := done(h); len(got) != 0 {
			t.Fatalf("records %v for a breach older than the first reading",
				got)
		}
		// The pool still past its mark at the new total: a breach this
		// coordinator's next pass reads first, which is timed from there.
		h.clk.advance(time.Second)
		h.pass()
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(10, 100, 190, 300))
		h.clk.advance(3 * time.Second)
		h.pass()
		if got := done(h); len(got) != 1 || got[0]["seconds"] != float64(3) {
			t.Fatalf("records %v, want the next grow timed at 3 s", got)
		}
	})
	t.Run("metadata", func(t *testing.T) {
		h := setup(t)
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(90, 100, 10, 100))
		h.pass()
		h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
			poolLine(90, 300, 10, 100))
		h.clk.advance(5 * time.Second)
		h.pass()
		got := done(h)
		if len(got) != 1 || got[0]["kind"] != reactionGrowMeta ||
			got[0]["seconds"] != float64(5) {
			t.Fatalf("records %v, want one metadata grow of 5 s", got)
		}
	})
}
