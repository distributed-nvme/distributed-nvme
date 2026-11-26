package worker

import (
	"testing"
	"time"
)

// TestAgentReach pins the worker's record of agent reach (AR3, VW8): a
// worker none of whose loops waits for an answer is never unreached and
// holds nothing; rounds asked with no answer hold the reactions from the
// first, before any answer since the reset, and are unreached from the
// bound on; an answer clears both, and one that ends a silence of two of its
// loop's intervals keeps the hold for the bound and the reconnect wait,
// while a shorter silence, or an answer with no slot, starts no such hold;
// a reset forgets the answer, the rounds and the recovery; a loop that
// leaves or idles waits for nothing, and a loop that asks after them starts
// a new stretch; a loop that waits for no answer and has a long interval
// stretches the fence's bound to twice it, but not the hold's, and a loop
// that waits stretches nothing.
func TestAgentReach(t *testing.T) {
	const bound = 10 * time.Second
	const interval = 2 * time.Second
	t0 := time.Unix(1_000_000, 0)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	r := &agentReach{}
	if r.unreached(at(time.Hour), bound) || r.holds(at(time.Hour), bound) ||
		r.heardNone() {
		t.Fatalf("a worker with no loop is unreached, holds or heard none")
	}
	a := r.join()
	if r.holds(at(time.Hour), bound) || r.heardNone() {
		t.Fatalf("a loop that asks nothing holds or heard none")
	}
	r.asked(a, t0, interval)
	if !r.holds(t0, bound) || !r.heardNone() ||
		r.unreached(at(bound-time.Second), bound) {
		t.Fatalf("before any answer: holds %v, heard none %v, unreached "+
			"early %v", r.holds(t0, bound), r.heardNone(),
			r.unreached(at(bound-time.Second), bound))
	}
	r.heard(a, at(time.Second))
	if r.heardNone() || r.holds(at(time.Second), bound) {
		t.Fatalf("an answer left the worker one that heard none, or held")
	}

	// A stretch: held from the bound on, and for the bound and the
	// reconnect wait after the answer that ends it.
	r.asked(a, at(2*time.Second), interval)
	if r.holds(at(11*time.Second), bound) {
		t.Fatalf("held a second before the bound")
	}
	if !r.unreached(at(12*time.Second), bound) ||
		!r.holds(at(12*time.Second), bound) {
		t.Fatalf("rounds unanswered for the bound are not unreached or held")
	}
	r.heard(a, at(20*time.Second))
	if r.unreached(at(20*time.Second), bound) {
		t.Fatalf("an answer did not clear the unanswered rounds")
	}
	end := at(20*time.Second + bound + agentReconnectWait)
	if !r.holds(end.Add(-time.Second), bound) || r.holds(end, bound) {
		t.Fatalf("the hold after its answer: held %v a second before the "+
			"end, %v at the end; want true, false",
			r.holds(end.Add(-time.Second), bound), r.holds(end, bound))
	}
	// A silence shorter than two intervals starts no recovery, nor does an
	// answer with no slot.
	r.asked(a, at(time.Hour), interval)
	r.heard(a, at(time.Hour+3*time.Second))
	if r.holds(at(time.Hour+3*time.Second), bound) {
		t.Fatalf("a silence of a round and a half started a recovery")
	}
	r.asked(a, at(2*time.Hour), interval)
	r.heard(nil, at(2*time.Hour+time.Minute))
	if r.holds(at(2*time.Hour+time.Minute), bound) || !r.answeredSinceReset() {
		t.Fatalf("an answer with no slot started a recovery, or was not heard")
	}
	r.withdraw(a)
	r.asked(a, at(3*time.Hour), interval)
	r.heard(a, at(3*time.Hour+time.Minute))
	r.reset()
	if r.answeredSinceReset() || r.holds(at(3*time.Hour+time.Minute), bound) {
		t.Fatalf("a reset kept the answer or the recovery")
	}

	// A loop that leaves, or idles, waits for nothing: no hold, no fence.
	r = &agentReach{}
	a, b := r.join(), r.join()
	r.heard(a, t0)
	r.asked(a, t0, interval)
	r.leave(a)
	if r.unreached(at(time.Hour), bound) || r.holds(at(time.Hour), bound) {
		t.Fatalf("a loop that left still counts")
	}
	// A loop that asks later starts its own stretch.
	r.asked(b, at(time.Hour), interval)
	if r.holds(at(time.Hour+time.Second), bound) ||
		r.unreached(at(time.Hour+time.Second), bound) {
		t.Fatalf("a new ask took the stretch of a loop that left")
	}
	r.withdraw(b)
	if r.unreached(at(2*time.Hour), bound) {
		t.Fatalf("an idle loop still counts")
	}

	// A loop that waits for no answer and has a long interval stretches the
	// fence's bound to twice it, not the hold's; a loop that waits, however
	// long its interval, stretches nothing.
	r = &agentReach{}
	a, b = r.join(), r.join()
	r.asked(a, t0, time.Minute)
	r.heard(a, t0)
	r.asked(b, t0, interval)
	if r.unreached(at(119*time.Second), bound) {
		t.Fatalf("unreached inside twice the answering loop's interval")
	}
	if !r.unreached(at(120*time.Second), bound) {
		t.Fatalf("not unreached at twice the answering loop's interval")
	}
	if !r.holds(at(bound), bound) {
		t.Fatalf("the hold's bound was stretched")
	}
	r = &agentReach{}
	a, b = r.join(), r.join()
	r.heard(a, t0)
	r.asked(b, t0, time.Hour)
	if !r.unreached(at(bound), bound) {
		t.Fatalf("a waiting loop's long interval stretched the bound")
	}
}
