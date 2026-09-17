// The worker half of the clone drain (dnv-worker.md §11.7, rules CLD1-CLD12).
//
// DeleteClone no longer sweeps a clone's bitmap chunks. It LATCHES the clone —
// `deleting = true`, the destination namespaces resumed, one SpRev bump — and
// the sp coordinator removes the chunk keys in batches of a constant size and
// then the clone itself.
//
// It is the sp drain's sibling (drain.go) and deliberately shares its shapes,
// with three differences that are each a decision:
//
//   - It runs ALONGSIDE the normal pass, not instead of it (CLD7). The sp drain
//     owns a doomed SP, so its pass does nothing else; here the SP is healthy
//     and every other child of it must keep converging while one clone goes
//     away.
//   - The teardown comes from EXCLUSION rather than from a step (CLD5). From
//     the first post-latch fan-out the deleting clone is absent from every
//     cntlr's plan and from the primary's chunk-push plans, and the CN's
//     existing removed-clone retire path dismantles the stack exactly once.
//     Zero agent changes: to an agent this is indistinguishable from today's
//     post-delete syncup.
//   - The final step BUMPS SpRev instead of deleting it. The SP outlives the
//     clone, so the bump is every mutator's normal epilogue rather than the
//     stop signal the sp drain's D3 removes.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"sort"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The §12-style records of the clone drain, in the same house style as the sp
// drain's. msgCloneDrainStep is non-normative — it names no decision — but a
// max-shape drain is four batches and this is the only record that shows them.
const (
	msgCloneDrainFailed = "clone drain failed"
	msgCloneDrainStep   = "clone drain step"
	msgCloneDrainDone   = "clone drained"
)

// The "step" attribute of those records: CLD7's two states.
const (
	cloneDrainStepBm    = "bitmap"
	cloneDrainStepFinal = "final"
)

// drainClones runs at most ONE drain step for each deleting clone of the SP
// (CLD7's cadence half).
//
// It is called on every reaction pass of a live SP, ahead of the reactions and
// outside both of their gates, because neither applies to it: a doomed clone
// drains at any `sp_level` (SPD6's rule scoped down to the clone), and the
// drain reads no geometry, so the SP's own `bdev_conf` gate — which exists for
// tryGrow's block size — must not be able to strand a clone the way it could
// have stranded a latched SP.
//
// One step per clone per pass, bounded by MaxCloneCntPerSp. Clones are taken in
// `clone_name_list` order, which both owners of an accepted overlap load from
// the same key, so the order is the same on both.
func (w *spWorker) drainClones(ctx context.Context, state *model.SpState) {
	if state.Conf.GetDeleting() {
		// An SP being drained has no clones by precondition (its
		// clone_name_list had to be empty to be latched), so there is nothing
		// here — and the guard keeps the two drains from ever running on one
		// SP in one pass, which loadCloneForOp would refuse anyway.
		return
	}
	for _, name := range state.Conf.GetCloneNameList() {
		clone, ok := state.Clones[name]
		if !ok || !clone.GetDeleting() {
			continue
		}
		w.drainCloneStep(ctx, state, name, clone)
	}
}

// drainCloneStep is CLD7's derivation half, from the pass's snapshot alone:
// surviving chunk keys ⇒ one batch on the lowest MaxDelBmPerTxn of them;
// none ⇒ the final STM.
//
// No other state is consulted — not the Clone record, not the rectangle, not a
// progress key — so crash, restart and ownership handoff all resume through this same
// derivation. Ascending order is free to choose, chunk keys being independent
// and self-positioning, and is picked for determinism.
func (w *spWorker) drainCloneStep(
	ctx context.Context,
	state *model.SpState,
	name string,
	clone *pb.Clone,
) {
	ops := w.reactor().ops
	ids := []slog.Attr{
		slog.String("clone_name", name),
		slog.Uint64("clone_id", clone.GetCloneId()),
	}
	chunks := sortedChunks(state.CloneBmIdx[name])
	if len(chunks) == 0 {
		// CLD9: the final STM is invoked only when the scan found zero
		// surviving chunk keys, and that emptiness is permanent — after the
		// latch no append can add one (CLD1) and batches only delete.
		err := ops.finishCloneDelete(
			ctx, w.cid, w.shard, w.spId, w.desired.handle,
			name, clone.GetCloneId(),
		)
		if err != nil {
			w.cloneDrainFailed(ctx, cloneDrainStepFinal, err, ids...)
			return
		}
		slog.InfoContext(ctx, msgCloneDrainDone,
			slog.Uint64("cluster_id", w.cid),
			slog.Uint64("sp_id", w.spId),
			slog.String("clone_name", name),
			slog.Uint64("clone_id", clone.GetCloneId()),
		)
		// The final STM bumps SpRev like every other mutator, and that bump
		// is what re-fans the SP without this clone. Nothing is armed: there
		// is no next step for this clone, and a sibling still draining armed
		// its own.
		return
	}
	batch := chunks
	if len(batch) > common.MaxDelBmPerTxn {
		batch = batch[:common.MaxDelBmPerTxn]
	}
	removed, err := ops.drainCloneBm(
		ctx, w.cid, w.shard, w.spId, w.desired.handle,
		name, clone.GetCloneId(), batch,
	)
	if err != nil {
		w.cloneDrainFailed(ctx, cloneDrainStepBm, err, ids...)
		return
	}
	attrs := []any{
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("sp_id", w.spId),
		slog.String("clone_name", name),
		slog.Uint64("clone_id", clone.GetCloneId()),
		slog.String("step", cloneDrainStepBm),
		// chunk_cnt is the batch's size — how many chunk keys THIS step
		// removed — and not any count carried by the Clone record, which
		// carries none (§11.7 CLD8).
		slog.Int("chunk_cnt", removed),
	}
	slog.InfoContext(ctx, msgCloneDrainStep, attrs...)
	// CLD12: arming unconditionally here terminates, and there is deliberately
	// no `removed > 0` guard of the sp drain's kind — it would be dead code. A
	// batch is issued ONLY when this pass's scan found chunks, and `removed`
	// is then the batch size, so the guard could never be false. What the sp
	// drain guards against cannot arise either: the loser of a two-owner
	// overlap deletes keys that are already gone, but its NEXT pass re-scans,
	// finds none, and goes to the final STM, so the loop still shrinks.
	w.armDrain()
}

// sortedChunks returns the chunks in ascending (src_slice_idx, bm_idx) order
// without disturbing the snapshot's slice.
//
// MD3's keys-only scan already returns them in key order, which for
// CloneBitmapKey's fixed-width hex fields IS that order — this makes the rule
// visible where the batch is cut rather than leaving it a property of the key
// format two packages away.
func sortedChunks(chunks []model.BmChunk) []model.BmChunk {
	out := append([]model.BmChunk(nil), chunks...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].SliceIdx != out[j].SliceIdx {
			return out[i].SliceIdx < out[j].SliceIdx
		}
		return out[i].Idx < out[j].Idx
	})
	return out
}

// cloneDrainFailed is the record of a step that did not commit (CLD10: retry
// forever, log only — there is no terminal-failure state). A model.ErrPrecondition contributes the
// precondition that did not hold — CLD2's refusals land here — and every
// failure carries the error text: a drain step that does not run leaves keys
// nothing else will ever remove.
func (w *spWorker) cloneDrainFailed(
	ctx context.Context,
	step string,
	err error,
	ids ...slog.Attr,
) {
	attrs := []any{
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("sp_id", w.spId),
		slog.String("sp_name", w.desired.handle),
		slog.String("step", step),
	}
	for _, id := range ids {
		attrs = append(attrs, id)
	}
	var pre *model.ErrPrecondition
	if errors.As(err, &pre) {
		attrs = append(attrs, slog.String("reason", pre.Reason))
	}
	attrs = append(attrs, slog.String("error", err.Error()))
	slog.InfoContext(ctx, msgCloneDrainFailed, attrs...)
}
