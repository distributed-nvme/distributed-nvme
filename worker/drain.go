// The worker half of the sp drain (dnv-worker.md, The sp drain, rules
// SPD1-SPD14).
//
// DeleteStoragePool LATCHES the SP, committing `deleting = true` plus one
// SpRev bump (SPD3/SPD4). The staged teardown that follows is the sp
// coordinator's, entered from the deleting branch of the pass or of the
// drain's own wake-up (drainTick): a latched SP runs at most one DRAIN STEP
// per pass or wake-up and no reaction at all (SPD6).
//
// Three properties make that a complete loop with no timer and no checkpoint:
//
//   - The position is DERIVED, never remembered (SPD8). Every pass and every
//     wake-up re-reads the SpConf and takes the first matching phase; crash,
//     restart and shard handoff all resume through the same derivation, and a
//     progress key would only be a second copy of the truth that can disagree
//     with the first.
//   - A committed step that popped something ends in `BumpSpRev` — every step
//     but the last — and posts the drain's own wake-up, so the drain is its
//     own tick (SPD6): run() turns the wake-up into drainTick, which loads the
//     SP and runs the next step alone, behind the pass's cluster-conf gate.
//     The bump re-fires the coordinator's fan-out, which is what stops the
//     children of the objects the step removed. A step that popped nothing
//     bumps nothing, posts nothing, and is left to the RW12 tick.
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
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The records of the drain, in the style of dnv-worker.md, Log records.
// msgSpDrainDone is the one an operator waits for; msgSpDrainFailed carries
// every SPD2 refusal and every STM failure. msgSpDrainStep is non-normative —
// like msgReactionSuppressed and msgPoolStatusUnparsable it exists so that an
// operator (and the worker suite, dnv-worker.md, Integration test plan) can
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

// drainStep runs exactly ONE step of the drain of a latched SP (SPD6). The
// pass's deleting branch and the drain's wake-up (drainTick) both call it,
// each with the state it loaded and the cluster conf its gate let through,
// which a D2 batch needs for the DN capacity keys it maintains.
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
func (w *spWorker) drainStep(
	ctx context.Context,
	state *model.SpState,
	cc *pb.ClusterConf,
) {
	conf := state.Conf
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
			ctx, w.cid, w.shard, w.spId, w.desired.handle, sliceId, cc,
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
// on it would spin a wake-up that can only find the same nothing; the RW12 tick
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

// drainFailed turns one failed step into the "sp drain failed" record (SPD6;
// dnv-worker.md, Log records). A model.ErrPrecondition contributes the
// precondition that did not hold — SPD2's three refusals land here — and every
// failure, precondition or not, carries the error text: unlike a reaction, a
// drain step that does not run leaves an object nothing else will ever remove,
// so its cause is never elided.
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

// armDrain posts the drain's wake-up, so the next drain step runs at once
// rather than at the next cntlr_interval tick (SPD6: "the drain is its own
// tick").
//
// A committed step posts here and run() turns the token into drainTick, which
// runs the next steps alone. The signal is the step's commit itself, not an
// SpRev bump: a clone drain's batch bumps nothing (CLD8), and a bump reaches
// the coordinator as a DESIRED CHANGE, which drives the fan-out and not the
// drain (run()). The channel holds one token and the send never blocks, so a
// burst of steps cannot queue wake-ups up.
//
// A coordinator assembled by hand — the plan-only and reaction test fixtures —
// has no loop to wake and no channel; its passes and wake-ups are driven
// directly.
func (w *spWorker) armDrain() {
	if w.drainCh == nil {
		return
	}
	select {
	case w.drainCh <- struct{}{}:
	default:
	}
}

// drainTick is the drain's wake-up (SPD6, CLD7), what run() does with the
// token a committed drain step posted (armDrain). It runs exactly the drain
// half of a reaction pass, in the pass's order and behind the pass's gates:
// the load (loadPassState); the clone drain (drainClones), which the pass runs
// ahead of every gate; and for a latched SP the cluster-conf gate
// (passClusterConf), the clear of that gate's refusal memo, and one sp drain
// step (drainStep).
//
// It runs nothing else, because a drain step calls no agent (SPD13, CLD10):
// it reads no health write count and re-seeds no health memo (healthSeqs,
// reseedHealth; HL3), builds no pass (newPass, whose AR10 snapshot prunes the
// verdict memo), and runs none of the SP's own gates, no AR3 suppression and
// no reaction. Those keep the cntlr_interval pass (AR1), so a drain of many
// steps evaluates the reactions on the pass's cadence and not once more per
// step. A live SP takes no cluster-conf gate here, exactly as its pass runs
// the clone drain ahead of that gate: the clone drain reads no cluster conf
// (CLD7). A wake-up that finds nothing left to drain does nothing.
func (w *spWorker) drainTick(ctx context.Context) {
	state, ok := w.loadPassState(ctx)
	if !ok {
		return
	}
	w.drainClones(ctx, state)
	if !state.Conf.GetDeleting() {
		return
	}
	cc, ok := w.passClusterConf(ctx)
	if !ok {
		return
	}
	w.confRefusal.cc = ""
	w.drainStep(ctx, state, cc)
}
