package agent

import (
	"context"
	"time"
)

// WaitBudget is a bounded amount of waiting one pass may spend on the node
// catching up with what the pass itself just asked of it: the cn connect
// step's one budget per converge pass (cnagent.md CN10, CN18) and the dn
// migration destination's wait for the source namespace (dnagent.md DN13).
//
// It is an ACCUMULATOR, not a deadline. Nothing but what a caller explicitly
// charges or pauses draws on it — a pass may spend any amount of time on
// work that is not waiting (a sweep, a successful connect, an mdadm) without
// touching it — and it is never shared between passes: every pass makes its
// own, so a budget spent by one pass cannot starve the next.
//
// A nil *WaitBudget is a budget that is always spent: every Pause refuses, so
// a caller holding none behaves exactly as it did before budgets existed.
type WaitBudget struct {
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
	left  time.Duration
}

// NewWaitBudget makes a budget of total, timed through now and waited through
// sleep: the two clock seams the role servers carry (real by default), which
// is what lets unit tests run a pass on a fake clock.
func NewWaitBudget(
	total time.Duration,
	now func() time.Time,
	sleep func(context.Context, time.Duration) error,
) *WaitBudget {
	return &WaitBudget{now: now, sleep: sleep, left: total}
}

// Now reads the budget's clock, so a caller can time an attempt it may then
// Charge. A nil budget reads the zero time, and a Charge of the difference of
// two of those is nothing.
func (b *WaitBudget) Now() time.Time {
	if b == nil {
		return time.Time{}
	}
	return b.now()
}

// Charge draws time the caller measured — a failed attempt's own elapsed time
// — from the budget. It may overdraw it: an attempt that ran past what was
// left has spent the rest, and every later Pause refuses.
func (b *WaitBudget) Charge(d time.Duration) {
	if b == nil || d <= 0 {
		return
	}
	b.left -= d
}

// Spent reports whether nothing is left.
func (b *WaitBudget) Spent() bool {
	return b == nil || b.left <= 0
}

// Pause waits d and draws it from the budget, but only when d fits in what is
// left: a pause that does not fit is refused outright, without waiting, so no
// pause starts that the budget cannot cover. It reports whether the pause
// happened — false when it did not fit, and false when ctx ended during the
// wait: an RPC whose caller went away, or agent shutdown cancelling rootCtx
// under a background attempt.
//
// The draw is the MEASURED wait and never less than d. In production the two
// agree (a sleep lasts at least its duration); the floor is for a clock that
// does not move — a test's frozen fake — under which a loop of pauses would
// otherwise draw nothing and never end.
func (b *WaitBudget) Pause(ctx context.Context, d time.Duration) bool {
	if b == nil || d > b.left {
		return false
	}
	start := b.now()
	err := b.sleep(ctx, d)
	waited := b.now().Sub(start)
	if waited < d {
		waited = d
	}
	b.left -= waited
	return err == nil
}

// SleepCtx is the real sleep behind the role servers' sleep seams: it waits d
// or until ctx ends, whichever comes first, and returns ctx's error when ctx
// ended first.
func SleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
