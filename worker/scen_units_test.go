package worker

import (
	"context"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Unit cases of change-6 rules a whole worker cannot reach on cue, over the
// package's fake model surface (reactHarness): the refusal of a grow from
// inside its transaction (AR2), and the sp drain's last step under the stop
// of the pool's coordinator that its own commit brings (SPD12).

// TestReactionGrowRefusedInsideItsTransactionEndsThePass pins AR2's rule on
// a grow that GrowSlice refuses from inside its transaction: the pass's own
// check found the grow applicable, so the refusal ends the pass, though it
// carries a reason that the same check would let the pass go on past
// (TestReactionPendingGrowDoesNotBlockThePass): the leg repair that is due
// in the same pass does not run. The record names the op, which tells it
// from the same reason found by the pass's own check.
func TestReactionGrowRefusedInsideItsTransactionEndsThePass(t *testing.T) {
	for _, reason := range []string{model.ReasonGrowPending, model.ReasonGrpListFull} {
		t.Run(reason, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.dnCands(reactDnC, reactDnD)
			total := reactDataBlocks(t, 2)
			h.setPool(reactSliceId, pb.ResStatus_RES_STATUS_OK,
				poolLine(1, 1000, total*90/100, total))
			h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
			h.rops.opErr = &model.ErrPrecondition{Op: "GrowSlice", Reason: reason}
			h.pass()
			h.wantOps("grow")
			h.wantSkippedBy(reactionGrowData, reason, "GrowSlice")
			h.wantApplied()
		})
	}
}

// cancelOnFinish is the fake model surface whose last drain step brings the
// stop of the coordinator, as the delete of its SpRev key does (SW3): the
// stop cancels the context the pass runs under, and the step's commit then
// fails as an etcd call does whose context ended while its reply was on its
// way, unless the step runs under a context that stop does not cancel.
type cancelOnFinish struct {
	*fakeReactionOps
	stop func()
}

func (o *cancelOnFinish) finishSpDelete(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
) error {
	if err := o.fakeReactionOps.finishSpDelete(ctx, cid, shard, spId, spName); err != nil {
		return err
	}
	o.stop()
	return ctx.Err()
}

// TestDrainFinalStepSurvivesTheStopItCauses pins SPD12's last step: it
// deletes the pool's revision key, whose delete stops the pool's coordinator
// and cancels the context the pass runs under, possibly before the commit's
// reply reaches the step; a stop lets a call in flight finish (RW11), so the
// step ends in exactly one `sp drained` and no `sp drain failed`, on the
// pass and on the drain's wake-up alike.
func TestDrainFinalStepSurvivesTheStopItCauses(t *testing.T) {
	paths := []struct {
		name string
		run  func(w *spWorker, ctx context.Context)
	}{
		{"pass", (*spWorker).reactionPass},
		{"wake-up", (*spWorker).drainTick},
	}
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			h := newReactHarness(t, reactFixture(t))
			h.latch()
			h.state.Conf.CntlrIdList = nil
			h.state.Conf.SliceIdList = nil
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.w.react = newReactor(&cancelOnFinish{fakeReactionOps: h.rops, stop: cancel})
			path.run(h.w, ctx)
			h.wantOps("finish_delete")
			if got := len(h.logs.withMsg(msgSpDrainDone)); got != 1 {
				t.Errorf("%q records = %d, want 1", msgSpDrainDone, got)
			}
			if got := h.logs.withMsg(msgSpDrainFailed); len(got) != 0 {
				t.Errorf("%q records = %v, want none", msgSpDrainFailed, got)
			}
		})
	}
}
