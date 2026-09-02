package dnagent

import (
	"log/slog"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The §11.2 src-cutover fence ([D12], architecture.md §11.1/§11.2).
//
// Handing a leg over to its migration destination happens in one converge
// pass, but the per-CN dm-linears of the source are retired in two phases:
//
//  1. suspend them where they are, and hold them that way for at least
//     common.SuspendSeconds. Their namespaces are already
//     AnaGrpIdInaccessible by then (DN12 step 1), so the only IO this can
//     absorb is what the old primary still had in flight — which is exactly
//     what the window is for.
//  2. reload them onto their dm-error devices. Because device-mapper
//     releases a suspended device's deferred bios against whatever table is
//     live at resume, and the reload installs dm-error *before* resuming,
//     the absorbed IO is failed at the end of the window rather than
//     replayed onto the side's data — which is what would otherwise let the
//     source silently diverge from the destination after hydration had
//     already copied the region.
//
// The window is a floor, not a schedule: phase 2 runs on the first converge
// at or after the deadline. A timer arms that converge so the RPC never waits
// (the §7 command timeouts are seconds, not minutes), and every converge is
// idempotent, so an early one simply stays in phase 1.
//
// The bound is the safety property. A suspended dm target queues bios with no
// timeout and no error path, so anything that reads it — a udev worker, an
// operator's lsblk, any block-device scan — blocks in uninterruptible D
// state; `exit_aio` then makes that task unkillable and the node needs a
// reboot (dnagent_issue_00.md issue 2). Nothing in the dn agent scans block
// devices any more ([D13] removed the LVM commands that did), so the exposure
// is external tooling during the window. Keeping the window bounded, and
// never letting a device outlive it, is what makes the trade acceptable.

// beginFence reports whether this side is still inside the grace window, and
// starts the clock the first time it is asked. The caller holds the side's
// object lock.
//
// A side whose linears are suspended but whose fenceAt is zero — an agent
// restart inside the window — is treated as **elapsed**, not as a fresh
// window. Restarting must never extend a suspension: the invariant is that no
// dnv device stays suspended for more than the window plus one converge.
func (s *DnAgentServer) beginFence(st *sideState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.fenceAt.IsZero() {
		if st.fenceRestarted {
			// Adopted from a previous process; the window is over.
			return false
		}
		st.fenceAt = time.Now()
		return s.fenceWait > 0
	}
	return time.Since(st.fenceAt) < s.fenceWait
}

// inFence reports whether a window is currently running, without starting
// one. The probe path uses it: a Check round must never mutate, and starting
// the clock is a mutation of the side's state.
func (s *DnAgentServer) inFence(st *sideState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.fenceAt.IsZero() {
		return false
	}
	return time.Since(st.fenceAt) < s.fenceWait
}

// armFenceTimer schedules the converge that ends the window. It is a single
// shot at the deadline, not a ticker: converging early would only re-enter
// phase 1.
func (s *DnAgentServer) armFenceTimer(st *sideState, plan *sidePlan) {
	key := sideKey(plan.clusterId, plan.dnId, plan.spId, plan.sideId)
	s.mu.Lock()
	if st.fenceTimer != nil {
		s.mu.Unlock()
		return
	}
	remaining := s.fenceWait - time.Since(st.fenceAt)
	if remaining < 0 {
		remaining = 0
	}
	rootCtx := s.rootCtx
	st.fenceTimer = time.AfterFunc(remaining, func() {
		s.mu.Lock()
		st.fenceTimer = nil
		s.mu.Unlock()
		slog.InfoContext(rootCtx, "migration cutover grace window elapsed",
			slog.String("side", key),
			slog.Int("suspend_seconds", common.SuspendSeconds))
		s.reconvergeSide(rootCtx, key, st)
	})
	s.mu.Unlock()
}

// clearFence stops the window: the migration source role ended, or the side
// is being torn down. Phase 2 (or an ordinary converge) puts the linears back
// wherever the new desired state wants them, resumed.
func (s *DnAgentServer) clearFence(st *sideState) {
	s.mu.Lock()
	timer := st.fenceTimer
	st.fenceTimer = nil
	st.fenceAt = time.Time{}
	st.fenceRestarted = false
	s.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
}

// adoptFence marks a side reloaded from the local store, so that per-CN
// linears this process finds suspended are recognised as debris from the
// previous one and retired immediately (see beginFence).
func (s *DnAgentServer) adoptFence(st *sideState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.fenceRestarted = true
}
