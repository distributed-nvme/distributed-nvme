package dnagent

import (
	"context"
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

// fenceStarted reports whether a window was ever started for this side,
// elapsed or not. It is the guard settleFence needs: beginFence would *start*
// one, which a converge that builds nothing must never do.
func (s *DnAgentServer) fenceStarted(st *sideState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !st.fenceAt.IsZero()
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

// settleFence is the fence bookkeeping of a converge that took the U4 gate
// (syncup_side.go's `state != sideDevReady` fork) and so never reached
// ensureCnDm.
//
// Without it the window can end with the per-CN dm-linears still suspended and
// nothing left to re-arm: the timer nils itself before it converges
// (armFenceTimer), unfenceLinears runs only from teardownSide, there is no
// periodic side converge, and a CheckSide round neither converges nor bumps a
// revision — so one transient `dmsetup info` failure on the side device would
// leave suspended devices behind until the worker happened to re-sync the
// side. A suspended dm target queues bios with no timeout (see the header), so
// [D12]'s bound is the safety property, not a best effort.
//
// Inside the window it only re-arms the timer. Once the window has elapsed it
// finishes phase 2 itself. That is safe under the U4 gate: the dm-error and
// the dm-linear are the devices the fence suspended, not something built on
// top of the side, and retiring them only moves the side *further* from
// exporting data — which is exactly what the end of the window is for.
func (s *DnAgentServer) settleFence(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
) {
	if plan.migrSrc == nil || !s.fenceStarted(st) {
		return
	}
	if s.inFence(st) {
		s.armFenceTimer(st, plan)
		return
	}
	for _, cnId := range plan.cnIds {
		errName := plan.errName(cnId)
		if err := s.ensureDmError(ctx, errName, plan.sectors); err != nil {
			slog.ErrorContext(ctx, "retiring a fenced dm-linear failed",
				slog.String("name", errName),
				slog.String("error", err.Error()))
			continue
		}
		linName := plan.linearName(cnId)
		// cloneLive is irrelevant here: a migration source fences every
		// per-CN linear onto its dm-error, the primary's included.
		if err := s.ensureDmLinear(ctx, linName, plan.sectors,
			plan.linearBacking(cnId, false)); err != nil {
			slog.ErrorContext(ctx, "retiring a fenced dm-linear failed",
				slog.String("name", linName),
				slog.String("error", err.Error()))
		}
	}
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

// adoptFence marks a side reloaded from the local store **whose per-CN linears
// this process actually finds suspended**, so that debris from the previous
// process is recognised as such and retired immediately (see beginFence).
//
// The probe is what confines the adoption to the sides it is meant for. A flag
// set on every reloaded side would sit there until the side's next migration —
// hours or days later — and then consume that cutover's whole
// common.SuspendSeconds window on a side the previous process never fenced:
// the primary's in-flight writes would be errored in the same pass that moved
// its namespaces to AnaGrpIdInaccessible, which is precisely the two-phase
// property [D12] exists to provide. The bound is on how long a device may stay
// suspended, not a licence to skip the window.
//
// The caller holds the node write lock (Reconcile, DN2), and s.mu is taken
// only after the last OS call — it is a leaf lock never held across one.
func (s *DnAgentServer) adoptFence(ctx context.Context, st *sideState) {
	// extentSize is irrelevant here: only the per-CN linear names are needed,
	// and those are pure functions of the side's ids.
	plan := newSidePlan(s.nf, st.req, 0)
	suspended := false
	for _, cnId := range plan.cnIds {
		dev, err := s.dm.Info(ctx, plan.linearName(cnId))
		if err != nil || dev == nil || !dev.Suspended {
			continue
		}
		suspended = true
		break
	}
	if !suspended {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st.fenceRestarted = true
}
