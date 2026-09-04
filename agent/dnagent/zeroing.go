package dnagent

import (
	"context"
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
// `provisioned` flag ([D15], update_01.md U4).
//
// The work is a goroutine rather than part of the RPC because a whole side is
// minutes of IO: a node-read holder plus one queued SyncupDn writer would
// freeze the entire DN agent (RWMutex writer preference) and block the side's
// Check rounds. The registry is keyed by the side tuple, single-flight per
// side, created on demand by any converge — the startup reconcile included —
// that finds zeroing still needed, and sides zero in parallel with no global
// cap (the fast-Write-Zeroes hardware assumption of §9.4).

// zeroLockPoll is how often the loop retries a lock it could not take. The
// loop must NEVER block on a lock: teardownSide and the DN6 orphan sweep
// cancel it and wait for it while holding the node WRITE lock, and cancelling
// a ctx does not release a goroutine parked in sync.RWMutex.RLock, so a
// blocking acquire would hang the whole agent (ruling R4.20).
const zeroLockPoll = 20 * time.Millisecond

// zeroJob is the geometry one zeroing loop works from. It is captured once, at
// registration, rather than read from a *sidePlan on every batch: the loop
// outlives the converge pass that started it, and a plan is a per-pass value
// (teardownSide even builds one with extentSize 0).
type zeroJob struct {
	spId       uint64
	sideId     uint64
	devPath    string
	extentSize uint64
}

// startZeroing registers the side's zeroing loop if it is not running already.
// The caller holds the node read lock and the side's object lock.
//
// It refuses once rootCtx is done (ruling R4.21): an armed §11.2 fence timer
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
// SIGKILLs it CmdHardTimeout−CmdSoftTimeout later) plus one zeroLockPoll.
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

// zeroLoop zeroes the side's not-yet-zeroed extents, DnZeroBatchExtCnt at a
// time, until every bit is set or the ctx is done.
//
// Each batch is one traceable operation (SH2). The `blkdiscard --zeroout` runs
// **lock-free** under the ordinary SH15 timeouts — unlike U2's probe IO it is a
// killable child process, so holding an OsClient semaphore slot is bounded by
// CmdHardTimeout and needs no carve-out — and only the volume-table update
// afterwards takes node-read plus the side's object lock.
//
// Persisting the bits *after* the command returned is what makes an
// interrupted batch free: its bits stay 0, so the next pass simply redoes it.
// Partial zeros are harmless — zeroing a range twice is idempotent.
func (s *DnAgentServer) zeroLoop(
	ctx context.Context,
	key string,
	st *sideState,
	job zeroJob,
) {
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
		// advance — the same range would be zeroed forever.
		rec, ok, err := s.meta.LookupSide(attemptCtx, job.spId, job.sideId)
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
		from, count, more := sideNextZeroBatch(rec, common.DnZeroBatchExtCnt)
		if !more {
			return
		}
		if err := s.dm.BlkZeroout(attemptCtx, job.devPath,
			from*job.extentSize, count*job.extentSize); err != nil {
			// The killed command's output is what side_dev_info reports, and
			// the pace below is what keeps a persistent failure from becoming
			// a hot loop (§9.4).
			s.setZeroingErr(st, err)
			if !s.zeroPace(ctx) {
				return
			}
			continue
		}
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
		// while one is outstanding (ruling R4.14).
		s.setZeroingErr(st, nil)
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
