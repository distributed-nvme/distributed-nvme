package worker

import (
	"sync"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// agentReach is the worker's one record of whether it reaches the agents,
// over every loop of every role (dnv-worker.md AR3, VW8): whether an agent
// has answered a round or a Syncup* of it since the record was reset, since
// when its rounds have gone unanswered with no answer between, which loops
// wait for an answer now, and when an answer ended a silence of the rounds.
// A worker none of whose loops waits for an answer, as one that drives no
// object or whose loops have stopped, idle or wait in a Syncup*, is never
// unreached. Times are by the worker's clock.
type agentReach struct {
	mu sync.Mutex
	// answered is whether an agent has answered since the last reset.
	answered bool
	// unansweredSince is the time of the first round asked since the last
	// answer, zero while none has been asked since.
	unansweredSince time.Time
	// loops are the slots of the loops that run, one each (join, leave),
	// and waitingCnt how many of them wait for an answer.
	loops      map[*reachSlot]struct{}
	waitingCnt int
	// recoverFrom is the answer that ended a silence of two of its loop's
	// intervals or more: the hold goes on for a while after it (holds).
	recoverFrom time.Time
}

// reachSlot is one loop's part of the record: whether it waits for an
// answer, a round or a Syncup* asked and not answered since, and its round
// interval.
type reachSlot struct {
	waiting  bool
	interval time.Duration
}

// agentReconnectWait is the longest a connection to an agent that comes back
// takes to try again: gRPC's reconnect backoff stops growing at
// GrpcReconnectMaxDelay (grpc.md, Wiring), give or take gRPC's jitter of a
// fifth.
const agentReconnectWait = common.GrpcReconnectMaxDelay * time.Second * 6 / 5

// join gives a loop its slot, which it hands to every call below, and leave
// takes it back as the loop ends. A nil record gives a nil slot.
func (r *agentReach) join() *reachSlot {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loops == nil {
		r.loops = make(map[*reachSlot]struct{})
	}
	slot := &reachSlot{}
	r.loops[slot] = struct{}{}
	return slot
}

// leave takes a loop's slot back: a loop that has ended waits for nothing.
func (r *agentReach) leave(slot *reachSlot) {
	if r == nil || slot == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setWaitingLocked(slot, false)
	delete(r.loops, slot)
}

// setWaitingLocked sets, under r.mu, whether a slot waits for an answer, and
// keeps the count.
func (r *agentReach) setWaitingLocked(slot *reachSlot, waiting bool) {
	if slot.waiting == waiting {
		return
	}
	slot.waiting = waiting
	if waiting {
		r.waitingCnt++
	} else {
		r.waitingCnt--
	}
}

// asked notes a round a loop starts to ask its agent, before its stream is
// opened, with the loop's round interval: the loop waits for an answer, and
// the worker's rounds are unanswered from now if none was before, or if no
// loop waited for an answer, so that a stretch a loop that left or idled
// began does not count for a loop that asks later.
func (r *agentReach) asked(slot *reachSlot, now time.Time, interval time.Duration) {
	if r == nil || slot == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.waitingCnt == 0 || r.unansweredSince.IsZero() {
		r.unansweredSince = now
	}
	r.setWaitingLocked(slot, true)
	slot.interval = interval
}

// withdraw notes a loop that asks nothing more for now, as one that idles:
// it waits for no answer.
func (r *agentReach) withdraw(slot *reachSlot) {
	if r == nil || slot == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setWaitingLocked(slot, false)
}

// heard notes an answer of a loop's agent, of a round or of a Syncup*,
// whatever its code: the agent was reached. An answer that ends a silence
// of the worker's rounds two of its loop's intervals long or longer starts
// a recovery (holds): no healthy worker is silent so long, since its loops
// answer each round.
func (r *agentReach) heard(slot *reachSlot, now time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if slot != nil {
		r.setWaitingLocked(slot, false)
		if !r.unansweredSince.IsZero() && slot.interval > 0 &&
			now.Sub(r.unansweredSince) >= 2*slot.interval {
			r.recoverFrom = now
		}
	}
	r.answered = true
	r.unansweredSince = time.Time{}
}

// reset forgets every answer, every round and every hold, as a new
// incarnation starts (VW8); the loops of the old one have left.
func (r *agentReach) reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answered = false
	r.unansweredSince = time.Time{}
	r.recoverFrom = time.Time{}
}

// spanLocked is, under r.mu, twice the longest round interval of the loops
// that wait for no answer: such a loop's agent answered it last and answers
// each round, so it may be silent that long with no cut. A loop that waits
// is no such sign and stretches nothing.
func (r *agentReach) spanLocked() time.Duration {
	var span time.Duration
	for slot := range r.loops {
		if !slot.waiting {
			span = max(span, 2*slot.interval)
		}
	}
	return span
}

// silentLocked says, under r.mu, whether a loop waits for an answer while
// the worker's rounds have gone unanswered for bound.
func (r *agentReach) silentLocked(now time.Time, bound time.Duration) bool {
	return r.waitingCnt > 0 && !r.unansweredSince.IsZero() &&
		now.Sub(r.unansweredSince) >= bound
}

// unreached says whether the worker's rounds have gone unanswered for bound,
// or for the span of the loops that wait for no answer if that is longer,
// while a loop waits for an answer (VW8 (e)).
func (r *agentReach) unreached(now time.Time, bound time.Duration) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.silentLocked(now, max(bound, r.spanLocked()))
}

// holds says whether the reactions must be held (AR3): while a loop waits for
// an answer the worker has heard no agent since the record was reset, or its
// rounds have gone unanswered for bound; or an answer ended a silence of the
// rounds less than bound and agentReconnectWait ago, as every other agent's
// connection comes back on its own backoff. The bound is not stretched by
// the loops' intervals: a hold costs only time.
func (r *agentReach) holds(now time.Time, bound time.Duration) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.silentLocked(now, bound) {
		return true
	}
	if !r.recoverFrom.IsZero() &&
		now.Sub(r.recoverFrom) < bound+agentReconnectWait {
		return true
	}
	return r.waitingCnt > 0 && !r.answered
}

// heardNone says whether a loop waits for an answer while the worker has
// heard no agent since the record was reset: an unhealthy verdict is then
// not written (HL3).
func (r *agentReach) heardNone() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.waitingCnt > 0 && !r.answered
}

// answeredSinceReset says whether an agent has answered since the record
// was reset.
func (r *agentReach) answeredSinceReset() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.answered
}
