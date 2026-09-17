// The worker half of the sp drain (dnv-worker.md §11.6, rules SPD1-SPD14).
//
// DeleteStoragePool no longer tears an SP down; it LATCHES it, committing
// `deleting = true` plus one SpRev bump (SPD3/SPD4). The staged teardown that
// follows is the sp coordinator's, entered from AR3's deleting branch: what used
// to be "no reaction runs for a deleting SP" is now "exactly one DRAIN STEP runs
// and no reaction at all" (SPD6).
//
// Three properties make that a complete loop with no timer and no checkpoint:
//
//   - The position is DERIVED, never remembered (SPD8). Every pass re-reads the
//     SpConf and takes the first matching phase; crash, restart and shard
//     handoff all resume through the same derivation, and a progress key would
//     only be a second copy of the truth that can disagree with the first.
//   - A committed step that popped something ends in `BumpSpRev` — every step
//     but the last — so the drain is its own tick: the step arms the next pass
//     directly (SPD6), and the bump also re-fires the coordinator's fan-out,
//     which is what stops the children of the objects the step removed. A step
//     that popped nothing bumps nothing and is left to the RW12 tick.
//   - The last step, D3, DELETES the SpRev key, which is already the shard
//     worker's stop signal (SW3) — so the drain terminates itself in the same
//     transaction that finishes the job.
//
// A failed step commits nothing, bumps nothing and is retried on the next RW12
// tick, forever (SPD6). There is no terminal-failure state: a delete that gave
// up would just strand garbage, and progress is already user-visible through
// `sp get` (a shrinking inventory), so no error field is added.
package worker

import (
	"context"
	"errors"
	"log/slog"

	"github.com/distributed-nvme/distributed-nvme/model"
)

// The §12-style records of the drain. msgSpDrainDone is the one an operator
// waits for; msgSpDrainFailed carries every SPD2 refusal and every STM failure.
// msgSpDrainStep is non-normative — like msgReactionSuppressed and
// msgPoolStatusUnparsable it exists so that an operator (and the §14 suite) can
// see the drain advancing batch by batch rather than only that it finished.
const (
	msgSpDrainFailed = "sp drain failed"
	msgSpDrainStep   = "sp drain step"
	msgSpDrainDone   = "sp drained"
)

// The "phase" attribute of the three records: SPD8's D1 / D2 / D3.
const (
	drainPhaseCntlrs = "cntlrs"
	drainPhaseSlice  = "slice"
	drainPhaseFinal  = "final"
)

// drainStep runs exactly ONE step of the drain of a latched SP (SPD6).
//
// SPD8's derivation, first match wins, from the freshly loaded SpConf alone:
//
//  1. `cntlr_id_list` non-empty  → D1, every cntlr at once;
//  2. `slice_id_list` non-empty  → D2, one batch on the LOWEST listed slice id;
//  3. otherwise                  → D3, the final keys.
//
// No other state is consulted. The accepted transient two-owner overlap is safe
// for the usual reason: both owners run SPD2-guarded ops against whatever
// remains and the loser's STM fails its compares, because a step is an
// idempotent "pop what is still there".
//
// The lowest slice id rather than the first listed one, for the reason AR5
// states explicitly and AR7/AR8 borrow: two overlapping owners must choose the
// same target, and `slice_id_list` order is not guaranteed to be sorted by
// anything.
func (w *spWorker) drainStep(ctx context.Context, p *spPass) {
	conf := p.state.Conf
	ops := w.reactor().ops
	switch {
	case len(conf.GetCntlrIdList()) > 0:
		removed, err := ops.drainSpCntlrs(
			ctx, w.cid, w.shard, w.spId, w.desired.handle,
		)
		if err != nil {
			w.drainFailed(ctx, drainPhaseCntlrs, err)
			return
		}
		w.drainStepped(ctx, drainPhaseCntlrs, removed > 0,
			slog.Int("cntlr_cnt", removed),
		)
	case len(conf.GetSliceIdList()) > 0:
		sliceId := sortedIds(conf.GetSliceIdList())[0]
		removed, done, err := ops.drainSpSlice(
			ctx, w.cid, w.shard, w.spId, w.desired.handle, sliceId, p.cc,
		)
		if err != nil {
			w.drainFailed(ctx, drainPhaseSlice, err,
				slog.Uint64("slice_id", sliceId),
			)
			return
		}
		w.drainStepped(ctx, drainPhaseSlice, removed > 0 || done,
			slog.Uint64("slice_id", sliceId),
			slog.Int("grp_cnt", removed),
			slog.Bool("slice_done", done),
		)
	default:
		err := ops.finishSpDelete(
			ctx, w.cid, w.shard, w.spId, w.desired.handle,
		)
		if err != nil {
			w.drainFailed(ctx, drainPhaseFinal, err)
			return
		}
		// D3 deletes the SpRev key rather than bumping it, so nothing is armed:
		// the shard worker stops this coordinator on that delete (SW3).
		slog.InfoContext(ctx, msgSpDrainDone,
			slog.Uint64("cluster_id", w.cid),
			slog.Uint64("sp_id", w.spId),
			slog.String("sp_name", w.desired.handle),
		)
	}
}

// drainStepped logs one committed step and, when the step actually moved the
// SP, arms the next one.
//
// progressed is false for the idempotent no-op an accepted two-owner overlap
// produces — the other owner had already popped what this step went for. Arming
// on it would spin a pass that can only find the same nothing; the RW12 tick
// picks the drain up again, which is exactly the cadence a step that wrote
// nothing deserves.
func (w *spWorker) drainStepped(
	ctx context.Context,
	phase string,
	progressed bool,
	ids ...slog.Attr,
) {
	attrs := []any{
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("sp_id", w.spId),
		slog.String("sp_name", w.desired.handle),
		slog.String("phase", phase),
	}
	for _, id := range ids {
		attrs = append(attrs, id)
	}
	slog.InfoContext(ctx, msgSpDrainStep, attrs...)
	if progressed {
		w.armDrain()
	}
}

// drainFailed turns one failed step into the §12 record (SPD6). A
// model.ErrPrecondition contributes the precondition that did not hold — SPD2's
// three refusals land here — and every failure, precondition or not, carries the
// error text: unlike a reaction, a drain step that does not run leaves an object
// nothing else will ever remove, so its cause is never elided.
func (w *spWorker) drainFailed(
	ctx context.Context,
	phase string,
	err error,
	ids ...slog.Attr,
) {
	attrs := []any{
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("sp_id", w.spId),
		slog.String("sp_name", w.desired.handle),
		slog.String("phase", phase),
	}
	for _, id := range ids {
		attrs = append(attrs, id)
	}
	var pre *model.ErrPrecondition
	if errors.As(err, &pre) {
		attrs = append(attrs, slog.String("reason", pre.Reason))
	}
	attrs = append(attrs, slog.String("error", err.Error()))
	slog.InfoContext(ctx, msgSpDrainFailed, attrs...)
}

// armDrain schedules the next drain pass at once rather than at the next
// cntlr_interval tick (SPD6: "the drain is its own tick").
//
// SPD6 puts it as a committed step scheduling the next pass "from its own
// commit". The step's SpRev bump reaches the coordinator as a DESIRED CHANGE,
// which drives the fan-out and not the reaction pass (run()), so the
// self-perpetuation is expressed directly: a committed step posts here and
// run() turns that into the next pass. The channel holds one token and the
// send never blocks, so a burst of steps cannot queue passes up.
//
// A coordinator assembled by hand — the plan-only and reaction test fixtures —
// has no loop to wake and no channel; its passes are driven directly.
func (w *spWorker) armDrain() {
	if w.drainCh == nil {
		return
	}
	select {
	case w.drainCh <- struct{}{}:
	default:
	}
}
