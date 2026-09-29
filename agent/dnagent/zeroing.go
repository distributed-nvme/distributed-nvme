package dnagent

import (
	"context"
	"fmt"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The §9.4 background side-zeroing loop — the dn twin of the DN8 connect-retry
// registry (migr.go) and of the cn leg probers (cnagent/healthcheck.go).
//
// dnv is multi-tenant: one tenant must never read another's bytes. discard is
// not a zero guarantee (the kernel dropped discard_zeroes_data in 4.12 and
// NVMe DLFEAT read-zeroes is optional), so every side is fully zeroed with
// `blkdiscard --zeroout` before its first export, tracked per logical extent
// in the side's on-disk allocation record and gated by the CP-visible
// `provisioned` flag ([D15]).
//
// The work is a goroutine rather than part of the RPC because a whole side is
// minutes of IO: a node-read holder plus one queued SyncupDn writer would
// freeze the entire DN agent (RWMutex writer preference) and block the side's
// Check rounds. The registry is keyed by the side tuple, single-flight per
// side, created on demand by any converge — the startup reconcile included —
// that finds zeroing still needed, and sides zero in parallel, at most
// common.DnZeroConcurrency batches at a time per agent: N concurrent batches
// split the disk's Write Zeroes rate N ways, and with no cap a busy DN would
// have every batch killed at the soft timeout and redone for ever. Each side
// sizes its own batches from the kills it sees (zeroLoop).

// zeroLockPoll is how often the loop retries a lock it could not take. The
// loop must NEVER block on a lock: dropSideState and the DN6 record sweep
// cancel it and wait for it while holding the node WRITE lock, and cancelling
// a ctx does not release a goroutine parked in sync.RWMutex.RLock, so a
// blocking acquire would hang the whole agent (DN9).
const zeroLockPoll = 20 * time.Millisecond

// zeroBackoffFmt follows the killed command's output in the error the loop
// publishes for side_dev_info once DnZeroKillBackoff or more kills in a row
// have backed the side off (DN9): the streak, and the rate the side now zeroes
// at.
const zeroBackoffFmt = "%w (%d batches killed in a row: backed off to 1 " +
	"extent per batch)"

// zeroJob is the geometry one zeroing loop works from. It is captured once, at
// registration, rather than read from a *sidePlan on every batch: the loop
// outlives the converge pass that started it, and a plan is a per-pass value
// (collectClaims even builds one with extentSize 0, to read names and gates
// out of a request without an extent size to hand).
type zeroJob struct {
	spId       uint64
	sideId     uint64
	devPath    string
	extentSize uint64
}

// startZeroing registers the side's zeroing loop if it is not running already.
// The caller holds the node read lock and the side's object lock.
//
// It refuses once rootCtx is done (SH27): an armed §11.2 fence timer
// is not enrolled in the WaitGroup and can still reach a converge after
// WaitBackground returned, and a bg.Add after bg.Wait panics.
func (s *DnAgentServer) startZeroing(st *sideState, plan *sidePlan) {
	key := sideKey(plan.clusterId, plan.dnId, plan.spId, plan.sideId)
	job := zeroJob{
		spId:       plan.spId,
		sideId:     plan.sideId,
		devPath:    plan.sideDevPath,
		extentSize: plan.extentSize,
	}
	s.mu.Lock()
	if st.zeroing || s.rootCtx.Err() != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.rootCtx)
	done := make(chan struct{})
	st.zeroing = true
	st.zeroCancel = cancel
	st.zeroDone = done
	s.mu.Unlock()

	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		defer close(done)
		defer s.deregisterZeroing(st, done)
		defer cancel()
		s.zeroLoop(ctx, key, st, job)
	}()
}

// stopZeroing cancels the loop **and waits for it**. The wait is the whole
// point: the `blkdiscard --zeroout` child holds /dev/mapper/{DnSideName} open,
// and `dmsetup remove` on a device with an open fd fails EBUSY. It is bounded
// by CmdHardTimeout (the OsClient SIGTERMs the child at CmdSoftTimeout and
// SIGKILLs it CmdHardTimeout−CmdSoftTimeout later) plus one zeroLockPoll —
// unless the child sits in an uninterruptible kernel wait, which no signal
// ends (SH15): the wait then lasts until the kernel returns.
//
// s.mu is a leaf lock never held across an OS call, so the cancel and the wait
// happen outside it — the stopMigrRetry / stopLegProbers shape.
func (s *DnAgentServer) stopZeroing(st *sideState) {
	s.mu.Lock()
	cancel := st.zeroCancel
	done := st.zeroDone
	st.zeroCancel = nil
	st.zeroDone = nil
	st.zeroing = false
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// deregisterZeroing clears the registry entry when the loop exits by itself —
// the side finished zeroing, or its record went away. It leaves a *newer*
// registration alone: stopZeroing may already have cleared these fields and a
// later startZeroing refilled them, and clobbering those would lose the next
// loop's cancel handle.
func (s *DnAgentServer) deregisterZeroing(st *sideState, done chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.zeroDone == done {
		st.zeroing = false
		st.zeroCancel = nil
		st.zeroDone = nil
	}
}

// zeroLoop zeroes the side's not-yet-zeroed extents, at most
// DnZeroBatchExtCnt at a time, until every bit is set or the ctx is done.
//
// Each batch is one traceable operation (SH2). The `blkdiscard --zeroout` runs
// **lock-free** under the ordinary SH15 timeouts — unlike the probers' block
// IO it is a
// child process, so holding an OsClient semaphore slot is bounded by
// CmdHardTimeout, unless the child sits in an uninterruptible kernel wait
// (SH15), and needs no carve-out — and only the volume-table update
// afterwards takes node-read plus the side's object lock. The command alone
// holds one of the agent's zeroing slots (zeroSlot); the record read, the
// table update and the retry pace hold none.
//
// Persisting the bits *after* the command returned is what makes an
// interrupted batch free: its bits stay 0, so the next pass simply redoes it.
// Partial zeros are harmless — zeroing a range twice is idempotent.
//
// batch and kills are the side's rate control (DN9): a batch the soft timeout
// killed makes the next one half its size, a success doubles it again up to
// DnZeroBatchExtCnt, and DnZeroKillBackoff kills in a row drop it to one
// extent, which the published error then carries after the killed command's
// output for as long as that kill is outstanding. side_dev_info shows that
// error only on a side still at provisioned = false; at true the row reads
// "not zeroed" (the DN9 matrix), whatever this loop saw.
// A batch the tool refused says nothing about the rate: it leaves the size
// alone and only ends the streak. Both are this goroutine's locals: the size
// caps the next command, the streak also words the published error, and they
// decide nothing else — which extents to zero, and whether the side is done,
// the bits alone decide. Being locals, they reset with every new goroutine
// for the side, a restart's included: the size to DnZeroBatchExtCnt, the
// streak to no kills.
func (s *DnAgentServer) zeroLoop(
	ctx context.Context,
	key string,
	st *sideState,
	job zeroJob,
) {
	batch := uint64(common.DnZeroBatchExtCnt)
	kills := 0
	for {
		if ctx.Err() != nil {
			return
		}
		// The side may have been torn down and re-created under this loop; the
		// new sideState owns its own goroutine (the DN8 guard).
		if s.getSide(key) != st {
			return
		}
		attemptCtx := common.WithTraceId(ctx, common.NewTraceId())

		// The record is re-read every batch: a returned record pointer goes
		// stale after the next write, and a cached one's bits would never
		// advance — the same range would be zeroed forever. It is read as
		// this node's only (DN5): a record in a table whose header names
		// another node — one that turned into another node's under the
		// loop, say — is refused here, before a batch is computed from its
		// bits, and the loop paces until a read of the disk confirms it.
		rec, ok, err := s.meta.LookupConfirmedSide(
			attemptCtx, job.spId, job.sideId)
		if err != nil {
			s.setZeroingErr(st, err)
			if !s.zeroPace(ctx) {
				return
			}
			continue
		}
		if !ok {
			// The allocation is gone: the side was freed under this loop.
			// There is nothing left to zero and nobody to report to.
			return
		}
		from, count, more := sideNextZeroBatch(rec, batch)
		if !more {
			return
		}
		freeSlot, ok := s.zeroSlot(ctx)
		if !ok {
			// Shutdown or teardown while every slot was in use.
			return
		}
		answered, err := s.dm.BlkZeroout(attemptCtx, job.devPath,
			from*job.extentSize, count*job.extentSize)
		freeSlot()
		if err != nil {
			if answered {
				kills = 0
			} else {
				// Killed: too big a batch for the disk's current rate. A
				// streak reports the rate it backs off to with the output,
				// so ERROR reads as a slow disk, not a broken one.
				kills++
				batch = zeroBatchAfterKill(count, kills)
				if kills >= common.DnZeroKillBackoff {
					err = fmt.Errorf(zeroBackoffFmt, err, kills)
				}
			}
			// The killed command's output is what side_dev_info reports, and
			// the pace below is what keeps a persistent failure from becoming
			// a hot loop (§9.4).
			s.setZeroingErr(st, err)
			if !s.zeroPace(ctx) {
				return
			}
			continue
		}
		kills = 0
		batch = min(2*batch, common.DnZeroBatchExtCnt)
		release, ok := s.zeroAcquire(ctx, key)
		if !ok {
			// Shutdown or teardown; the batch's bits stay unset and the next
			// process redoes it.
			return
		}
		err = s.meta.SetSideZeroed(
			attemptCtx, job.spId, job.sideId, from, from+count)
		release()
		if err != nil {
			s.setZeroingErr(st, err)
			if !s.zeroPace(ctx) {
				return
			}
			continue
		}
		// A successful batch clears an outstanding failure: ERROR wins only
		// while one is outstanding (DN9).
		s.setZeroingErr(st, nil)
	}
}

// zeroBatchAfterKill is a side's next batch size after the soft timeout killed
// a batch of count extents, the kills-th kill in a row (DN9): half the killed
// batch, rounded down but never below one extent, and one extent once
// DnZeroKillBackoff kills in a row say that halving is not keeping up. Never
// zero: a zero-extent batch zeroes nothing and doubles to zero, so the side
// would never finish.
func zeroBatchAfterKill(count uint64, kills int) uint64 {
	if kills >= common.DnZeroKillBackoff {
		return 1
	}
	return max(count/2, 1)
}

// zeroSlot takes one of the agent's DnZeroConcurrency zeroing slots, waiting
// while every one is in use, and reports false as soon as ctx is done. It may
// block where zeroAcquire must poll: a channel wait is released by the cancel,
// so stopZeroing's cancel-and-wait reaches a loop parked here at once, and the
// loop holds no lock while it waits.
func (s *DnAgentServer) zeroSlot(
	ctx context.Context,
) (free func(), ok bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	select {
	case <-ctx.Done():
		return nil, false
	case s.zeroSlots <- struct{}{}:
		return func() { <-s.zeroSlots }, true
	}
}

// zeroAcquire takes the node read lock and the side's object lock without ever
// blocking, and reports false as soon as ctx is done. See zeroLockPoll for why
// this is mandatory rather than an optimization.
func (s *DnAgentServer) zeroAcquire(
	ctx context.Context,
	key string,
) (release func(), ok bool) {
	for {
		if ctx.Err() != nil {
			return nil, false
		}
		if s.locks.Node().TryRLock() {
			objLock := s.locks.Obj(key)
			if objLock.TryLock() {
				return func() {
					objLock.Unlock()
					s.locks.Node().RUnlock()
				}, true
			}
			s.locks.Node().RUnlock()
		}
		timer := time.NewTimer(zeroLockPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, false
		case <-timer.C:
		}
	}
}

// zeroPace waits out DnZeroRetryInterval before the next attempt and reports
// whether the loop should carry on.
func (s *DnAgentServer) zeroPace(ctx context.Context) bool {
	timer := time.NewTimer(s.zeroRetryInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// zeroingErr / setZeroingErr publish the last batch's outcome for
// side_dev_info, exactly the way a cn legProber publishes its own (CN11).
func (s *DnAgentServer) zeroingErr(st *sideState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return st.zeroErr
}

func (s *DnAgentServer) setZeroingErr(st *sideState, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.zeroErr = err
}
