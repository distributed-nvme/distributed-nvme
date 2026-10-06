package dnagent

import (
	"context"
	"log/slog"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// The src-cutover fence ([D12];
// architecture.md, Failover and Migration, src step 2).
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
// (the command timeouts of architecture.md, Common validation, are seconds,
// not minutes), and every converge is
// idempotent, so an early one simply stays in phase 1 — unless it removes an
// export above a fenced linear: the sweep's P0 runs phase 2 for that linear
// first, and a level with no export layer ends the window (endFence).
//
// The bound is the safety property. A suspended dm target queues bios with no
// timeout and no error path, so anything that reads it — a udev worker, an
// operator's lsblk, any block-device scan — blocks in uninterruptible D
// state; `exit_aio` then makes that task unkillable and the node needs a
// reboot. Nothing in the dn agent scans block
// devices any more ([D13] removed the LVM commands that did), so the exposure
// is external tooling during the window. Keeping the window bounded, and
// never letting a device outlive it, is what makes the trade acceptable. The
// bound does not hold across DN12 rule 1's known limit (beginFence), nor
// past a command that fails: a phase-2 reload whose load fails leaves the
// linear suspended rather than resume it onto its pre-fence table and replay
// the window's IO onto the side's data (a reload fails closed: dnagent.md,
// OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`).

// beginFence reports whether this side is still inside the cutover window, and
// starts the clock the first time it is asked. The caller holds the side's
// object lock.
//
// A side whose linears are suspended but whose fenceAt is zero — an agent
// restart inside the window — is treated as **elapsed**, not as a fresh
// window, once adoptFence has found them so. Restarting must never extend a
// suspension: the invariant is that no dnv device stays suspended for more
// than the window plus one converge. DN12 rule 1's known limit breaks it: a
// restart whose probes of the side's linears all went unanswered adopts
// nothing, and one that ends up holding no state for the side keeps nothing
// of what it found: a lost --local-store, a side file missing or unreadable,
// or a dn file unreadable (DN2 skips its sides) or missing (DN2 drops its
// sides as ones whose pointer left the list). adoptFence runs only in
// Reconcile, its mark lives on the sideState, and the SyncupSide that brings
// the side back builds a new one with none. Then — unless the role ends
// first (clearFence), or a level with no export layer ends the window
// (endFence) — a converge that stops at the DN9 gate finds no window to
// settle (fenceStarted), and the first one that gets here starts a fresh
// window over linears still suspended
// (TestFenceRestartThatHoldsNoSideOpensAWholeWindow). A phase-2 reload
// whose load fails breaks it too: a reload fails closed
// (dnagent.md, OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`), so the linear
// stays suspended on its pre-fence table
// until a later converge's reload of it succeeds or the role's end resumes
// it (unfenceLinears).
func (s *DnAgentServer) beginFence(st *sideState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.fenceAt.IsZero() {
		if st.fenceRestarted || st.fenceEnded {
			// Adopted from a previous process, or ended early by the level
			// (endFence); the window is over.
			return false
		}
		st.fenceAt = time.Now()
		return s.fenceWait > 0
	}
	return time.Since(st.fenceAt) < s.fenceWait
}

// fenceStarted reports whether a window was ever started for this side — or
// adopted, already elapsed, from a previous process, or ended early by the
// level (endFence). It is the guard
// settleFence needs: beginFence would *start* one, which a converge that
// builds nothing must never do.
//
// The adopted arm is what makes DN12's "treated as elapsed, and phase 2 runs
// on the first converge" hold under the DN9 gate too: a restart-adopted fence
// has no fenceAt, so reading that field alone would let an agent restart plus
// one unreadable side device leave the per-CN linears suspended past [D12]'s
// bound.
func (s *DnAgentServer) fenceStarted(st *sideState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !st.fenceAt.IsZero() || st.fenceRestarted || st.fenceEnded
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

// settleFence is the fence bookkeeping of the two converges that stop short
// of ensureCnDm while the per-CN layer is still wanted: one that took the DN9
// side-device gate (syncup_side.go's `state != sideDevReady` fork), or one
// whose migration destination could not read where its per-CN stacks are
// (convergeSide's migrCloneUnread branch).
//
// Without it the window can end with the per-CN dm-linears still suspended
// and nothing left to re-arm: the timer nils itself before it converges
// (armFenceTimer), unfenceLinears runs only from the sweep's pre-step and
// only for a side that is no longer a migration source, the one periodic
// side converge, the DN8 retry, would take the same gate, and a CheckSide
// round neither converges nor bumps a revision — so one transient
// `dmsetup info` failure on the side device would leave suspended devices
// behind until the worker happened to re-sync the side. A suspended dm target
// queues bios with no timeout (see the header), so [D12]'s bound is the
// safety property, not a best effort.
//
// Inside the window it only re-arms the timer. Once the window has elapsed it
// finishes phase 2 itself. That is safe under the DN9 gate: it only puts each
// per-CN dm-linear on its dm-error, building either device where it is absent
// (a level with no export layer marks the window over, endFence, even on a
// side whose gate never opened), and that only moves the side *further* from
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
	st.fenceEnded = false
	s.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
}

// endFence ends the window early: the source role stands, but its level has
// no export layer any more (DN12). With the exports gone no IO can reach the
// per-CN linears, so there is nothing left for the window to absorb, and the
// sweep's P0 retires every suspended one onto its dm-error before L1 takes
// the export off it. The window stays ended until the source role does
// (clearFence) — neither cleared nor left running, since either would let a
// later converge at an exporting level, the level coming back down, suspend
// the linears again over the dm-errors phase 2 left them on, and a probe
// would then expect the pre-fence table there. The mark is in memory, like
// fenceAt, so an agent restart forgets it — DN12's known limit, the same one
// a window phase 2 has already closed runs into.
func (s *DnAgentServer) endFence(st *sideState) {
	s.mu.Lock()
	timer := st.fenceTimer
	st.fenceTimer = nil
	st.fenceAt = time.Time{}
	st.fenceEnded = true
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
	plan := newSidePlan(s.nf, st.req.Load(), 0)
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
