package agent

import (
	"context"
	"sync"
	"testing"
	"time"
)

// budgetClock is a fake clock whose sleep advances it by exactly the duration
// asked for, the way the role servers' tests drive a pass.
type budgetClock struct {
	mu  sync.Mutex
	now time.Time
	// frozen makes sleep leave the clock where it is — the clock a test gets
	// when it fakes `now` and leaves the sleep real, or the reverse.
	frozen bool
	sleeps []time.Duration
}

func (c *budgetClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *budgetClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	if !c.frozen {
		c.now = c.now.Add(d)
	}
	return ctx.Err()
}

func newBudgetClock() *budgetClock {
	return &budgetClock{now: time.Unix(1<<30, 0)}
}

// TestWaitBudgetPausesUntilTheyNoLongerFit pins the fits rule: a pause is
// taken only while it fits in what is left, so a 1 s budget holds exactly ten
// 100 ms pauses and refuses the eleventh without waiting.
func TestWaitBudgetPausesUntilTheyNoLongerFit(t *testing.T) {
	clock := newBudgetClock()
	b := NewWaitBudget(time.Second, clock.Now, clock.Sleep)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if !b.Pause(ctx, 100*time.Millisecond) {
			t.Fatalf("pause %d of 10 was refused", i+1)
		}
	}
	if !b.Spent() {
		t.Fatalf("ten 100 ms pauses left the 1 s budget unspent")
	}
	if b.Pause(ctx, 100*time.Millisecond) {
		t.Fatalf("an eleventh pause was taken past the budget")
	}
	if got := len(clock.sleeps); got != 10 {
		t.Fatalf("%d sleeps, want 10: a refused pause must not wait", got)
	}
}

// TestWaitBudgetChargeOverdraws pins what makes a slow failure end the
// waiting: a charge larger than what is left spends the whole budget, and no
// pause follows it.
func TestWaitBudgetChargeOverdraws(t *testing.T) {
	clock := newBudgetClock()
	b := NewWaitBudget(time.Second, clock.Now, clock.Sleep)
	b.Charge(3 * time.Second)
	if !b.Spent() {
		t.Fatalf("a 3 s charge left a 1 s budget unspent")
	}
	if b.Pause(context.Background(), time.Millisecond) {
		t.Fatalf("a pause was taken after the budget was overdrawn")
	}
	// A charge that fits leaves room for exactly what is left.
	b = NewWaitBudget(time.Second, clock.Now, clock.Sleep)
	b.Charge(950 * time.Millisecond)
	if b.Pause(context.Background(), 100*time.Millisecond) {
		t.Fatalf("a 100 ms pause fitted in the 50 ms left")
	}
	if !b.Pause(context.Background(), 50*time.Millisecond) {
		t.Fatalf("a 50 ms pause did not fit in the 50 ms left")
	}
}

// TestWaitBudgetFrozenClockStillEnds pins the floor: under a clock that does
// not move, every pause still draws its own length, so a loop of pauses ends.
func TestWaitBudgetFrozenClockStillEnds(t *testing.T) {
	clock := newBudgetClock()
	clock.frozen = true
	b := NewWaitBudget(time.Second, clock.Now, clock.Sleep)
	n := 0
	for b.Pause(context.Background(), 50*time.Millisecond) {
		n++
		if n > 100 {
			t.Fatalf("a frozen clock let the pauses run for ever")
		}
	}
	if n != 20 {
		t.Fatalf("%d pauses of 50 ms in a 1 s budget, want 20", n)
	}
}

// TestWaitBudgetStopsOnCtx pins that a pause ends with its ctx: an RPC whose
// caller went away, or agent shutdown under a background attempt.
func TestWaitBudgetStopsOnCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := NewWaitBudget(time.Second, time.Now, SleepCtx)
	start := time.Now()
	if b.Pause(ctx, 500*time.Millisecond) {
		t.Fatalf("a pause on a cancelled ctx reported that it happened")
	}
	if waited := time.Since(start); waited > 250*time.Millisecond {
		t.Fatalf("a pause on a cancelled ctx waited %v", waited)
	}
}

// TestWaitBudgetNilIsSpent pins the nil budget: always spent, never waits,
// never panics — a caller that holds none behaves as before budgets existed.
func TestWaitBudgetNilIsSpent(t *testing.T) {
	var b *WaitBudget
	b.Charge(time.Second)
	if !b.Spent() {
		t.Fatalf("a nil budget is not spent")
	}
	if b.Pause(context.Background(), time.Millisecond) {
		t.Fatalf("a nil budget paused")
	}
	if !b.Now().IsZero() {
		t.Fatalf("a nil budget read a clock")
	}
}

// TestSleepCtx pins the real sleep: it lasts its duration on a live ctx and
// returns the ctx's error at once on a dead one.
func TestSleepCtx(t *testing.T) {
	start := time.Now()
	if err := SleepCtx(context.Background(), 20*time.Millisecond); err != nil {
		t.Fatalf("SleepCtx: %v", err)
	}
	if waited := time.Since(start); waited < 20*time.Millisecond {
		t.Fatalf("SleepCtx returned after %v, want at least 20ms", waited)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	if err := SleepCtx(ctx, time.Hour); err == nil {
		t.Fatalf("SleepCtx on a cancelled ctx returned nil")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("SleepCtx on a cancelled ctx waited %v", waited)
	}
}
