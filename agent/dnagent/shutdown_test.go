package dnagent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// ---------------------------------------------------------------------------
// Process exit: the join of the background tasks (SH27)
// ---------------------------------------------------------------------------

// Nothing starts once the join has begun, whether or not rootCtx is done
// yet: no zeroing loop, no connect retry, no fence timer. A start the join
// did not refuse would run a task, and fork its children, that the join does
// not wait for. Each start here would stay visible were it made: the side
// has bytes to zero and its zeroing batch parks, the retry ticks once an
// hour, and the timer's window lasts an hour.
func TestNothingStartsOnceTheJoinHasBegun(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	plan := newSidePlan(srv.nf, st.req.Load(), testExtentSize)
	clearZeroed(t, srv, node)
	node.blockCmd("blkdiscard --zeroout")
	t.Cleanup(func() { node.releaseCmd("blkdiscard --zeroout") })
	srv.migrRetryInterval = time.Hour
	srv.fenceWait = time.Hour
	srv.mu.Lock()
	st.fenceAt = time.Now()
	srv.mu.Unlock()

	srv.WaitBackground()
	if srv.rootCtx.Err() != nil {
		t.Fatal("fixture is wrong: rootCtx is done")
	}
	srv.startZeroing(st, plan)
	srv.startMigrRetry(st, plan)
	srv.armFenceTimer(st, plan)

	srv.mu.Lock()
	zeroing, retrying, timer := st.zeroing, st.retrying, st.fenceTimer
	srv.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	if zeroing {
		t.Error("a zeroing loop started after the join")
	}
	if retrying {
		t.Error("a connect retry started after the join")
	}
	if timer != nil {
		t.Error("a fence timer was armed after the join")
	}
}

// The join marks the server closed under mu, the lock every start reads the
// mark under, so it cannot begin while a start holds that lock. That every
// start also enrolls its task under it is TestEveryEnrollmentHoldsTheJoinLock's
// to show.
func TestTheJoinWaitsForTheStartLock(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.mu.Lock()
	joined := make(chan struct{})
	go func() {
		srv.WaitBackground()
		close(joined)
	}()
	select {
	case <-joined:
		srv.mu.Unlock()
		t.Fatal("the join began while a start held the lock")
	case <-time.After(100 * time.Millisecond):
	}
	srv.mu.Unlock()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("the join never returned")
	}
}

// The join stops an armed fence timer: its converge would run after the join
// returned, on a rootCtx that is still alive here. The window's linears stay
// suspended for the next process's restart to settle (DN12 rule 1).
func TestTheJoinStopsAPendingFenceTimer(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	srv.fenceWait = 200 * time.Millisecond
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	lin := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	srv.mu.Lock()
	armed := st.fenceTimer != nil
	srv.mu.Unlock()
	if !dmSuspended(node, lin) || !armed {
		t.Fatal("fixture is wrong: the cutover left no suspended linear " +
			"with its timer armed")
	}

	node.Reset()
	srv.WaitBackground()
	srv.mu.Lock()
	armed = st.fenceTimer != nil
	srv.mu.Unlock()
	if armed {
		t.Error("the join left the fence timer armed")
	}
	time.Sleep(3 * srv.fenceWait)
	if !dmSuspended(node, lin) || node.hasCall("cmd dmsetup reload "+lin) {
		t.Error("the fence timer fired after the join")
	}
}

// A fence callback already running when the join begins is not waited for
// (SH27): it is not enrolled, and the join does not wait out its converge,
// which here parks on its phase-2 reload. Once released, that converge
// finishes the window.
func TestARunningFenceCallbackIsNotJoined(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	lin := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	// Phase 1 suspends in place and reloads nothing, so only the callback's
	// phase 2 reaches this reload.
	reload := "dmsetup reload " + lin
	node.blockCmd(reload)
	t.Cleanup(func() { node.releaseCmd(reload) })
	srv.fenceWait = 30 * time.Millisecond
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		return node.hasCall("cmd " + reload)
	}) {
		t.Fatal("the fence callback never reached its phase-2 reload")
	}

	joined := make(chan struct{})
	go func() {
		srv.WaitBackground()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		node.releaseCmd(reload)
		t.Fatal("the join waited for a running fence callback")
	}

	node.releaseCmd(reload)
	if !waitFor(t, 5*time.Second, func() bool {
		return !dmSuspended(node, lin)
	}) {
		t.Fatal("the released callback never finished the window")
	}
	// The callback's pass holds the side's object lock until it ends, and
	// nothing waits for it at the test's end either.
	lock := srv.locks.Obj(sideKey(testCluster, testDn, testSp, testSide))
	if !waitFor(t, 5*time.Second, func() bool {
		if !lock.TryLock() {
			return false
		}
		lock.Unlock()
		return true
	}) {
		t.Fatal("the released callback's pass never ended")
	}
}

// Every enrollment in bg is made under mu, the lock the join marks the server
// closed under (SH27): a start that enrolled after it let go of mu could be
// passed by the whole join — the mark, and a bg.Wait that does not count the
// task — and its task would run unjoined. The race detector sees such a
// start only when the join lands between the two, so the seam checks the
// lock at every enrollment instead. The zeroing loop parks in its blkdiscard
// and the retry ticks once an hour, so no task of the test holds mu while the
// other start enrolls.
func TestEveryEnrollmentHoldsTheJoinLock(t *testing.T) {
	var enrolled, unlocked atomic.Int32
	saved := bgEnrolling
	bgEnrolling = func(mu *sync.Mutex) {
		enrolled.Add(1)
		if mu.TryLock() {
			mu.Unlock()
			unlocked.Add(1)
		}
	}
	t.Cleanup(func() { bgEnrolling = saved })

	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	plan := newSidePlan(srv.nf, st.req.Load(), testExtentSize)
	if !waitFor(t, 5*time.Second, func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return !st.zeroing
	}) {
		t.Fatal("fixture is wrong: the side's first zeroing never ended")
	}
	clearZeroed(t, srv, node)
	node.blockCmd("blkdiscard --zeroout")
	t.Cleanup(func() { node.releaseCmd("blkdiscard --zeroout") })
	srv.migrRetryInterval = time.Hour

	enrolled.Store(0)
	unlocked.Store(0)
	srv.startZeroing(st, plan)
	if !waitFor(t, 5*time.Second, func() bool {
		return node.hasCall("cmd blkdiscard --zeroout")
	}) {
		t.Fatal("the zeroing loop never reached its blkdiscard")
	}
	srv.startMigrRetry(st, plan)
	if got := enrolled.Load(); got != 2 {
		t.Fatalf("%d enrollments through enrollLocked, want one for each "+
			"of the 2 starts", got)
	}
	if got := unlocked.Load(); got != 0 {
		t.Errorf("%d of the 2 enrollments were made without mu", got)
	}
}

// Starts racing the join while rootCtx is live: each start is either
// enrolled before the join's mark, and its task joined, or refused, so the
// join returns with the server marked closed, no zeroing loop of the starts
// still registered and no fence timer armed. The tasks end by themselves: a
// zeroing loop finds its side state not held on its first pass, and each
// retry is stopped as soon as it has started. A starter stops once it reads
// the mark or the join has returned, and once maxRun has passed in any case,
// so a join that never marks the server closed fails here instead of making
// the starters run, and keep their states, without end. A round in which a
// starter ran out of time ends the test, so a broken join makes the starters
// run for maxRun once at most, not in every round. On one CPU, a join that
// marks the server closed only after its wait can keep them running that
// long: its wait does not end while they keep enrolling tasks. A start that
// enrolled after letting go of mu fails here only when the join lands in
// that gap; the test that pins the lock is TestEveryEnrollmentHoldsTheJoinLock.
func TestShutdownUnderConcurrentStarts(t *testing.T) {
	// maxRun is far above the time a correct join takes to mark the server
	// closed, on any number of CPUs. A count of passes could not bound the
	// starters: on one CPU the join runs only once the runtime preempts a
	// starter, and by then that starter has made thousands of passes.
	const maxRun = 5 * time.Second
	for i := 0; i < 10; i++ {
		srv, _ := newTestServer(t)
		srv.migrRetryInterval = time.Hour
		srv.fenceWait = time.Hour
		syncupBoth(t, srv, 1, testSide)
		st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
		plan := newSidePlan(srv.nf, st.req.Load(), testExtentSize)
		srv.mu.Lock()
		st.fenceAt = time.Now()
		srv.mu.Unlock()
		var (
			wg      sync.WaitGroup
			started sync.Mutex
			zeroing []*sideState
			// joined is set once WaitBackground has returned.
			joined atomic.Bool
			// capped counts the starters that ran out of time.
			capped atomic.Int32
		)
		start := make(chan struct{})
		deadline := time.Now().Add(maxRun)
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for {
					// States the server does not hold.
					zst := newSideState(st.req.Load())
					rst := newSideState(st.req.Load())
					started.Lock()
					zeroing = append(zeroing, zst)
					started.Unlock()
					srv.startZeroing(zst, plan)
					srv.startMigrRetry(rst, plan)
					srv.stopMigrRetry(rst)
					srv.armFenceTimer(st, plan)
					srv.mu.Lock()
					closed := srv.bgClosed
					srv.mu.Unlock()
					if closed || joined.Load() {
						return
					}
					// Read after the mark, so a starter the scheduler kept
					// off the CPU past the deadline is not counted.
					if time.Now().After(deadline) {
						capped.Add(1)
						return
					}
				}
			}()
		}
		close(start)
		time.Sleep(time.Millisecond)
		srv.WaitBackground()
		joined.Store(true)
		srv.mu.Lock()
		marked := srv.bgClosed
		started.Lock()
		left := 0
		for _, zst := range zeroing {
			if zst.zeroing {
				left++
			}
		}
		started.Unlock()
		// A timer armed before the join began is one the join stopped and
		// forgot; one armed after it is still here.
		timer := st.fenceTimer
		srv.mu.Unlock()
		wg.Wait()
		if timer != nil {
			timer.Stop()
		}
		if !marked {
			t.Fatalf("round %d: the join returned without marking the "+
				"server closed", i)
		}
		if left != 0 {
			t.Errorf("round %d: %d zeroing loops still registered after "+
				"the join", i, left)
		}
		if timer != nil {
			t.Errorf("round %d: a fence timer was armed after the join", i)
		}
		// Last, so that the round's other checks still report, and fatal,
		// so that a broken join does not run each later round's starters
		// for maxRun too.
		if n := capped.Load(); n != 0 {
			t.Fatalf("round %d: %d starters ran for %v without reading "+
				"the join's mark", i, n, maxRun)
		}
	}
}
