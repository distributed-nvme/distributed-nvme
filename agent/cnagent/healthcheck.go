package cnagent

import (
	"context"
	"encoding/binary"
	"log/slog"
	"sync"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The §3.6 / [D6] leg health probe (CN11). Only the **primary** runs it: a
// standby's path deliberately terminates in the side's dm-error (§3.1), so
// block IO through it can never succeed, and a standby reports transport
// liveness instead (leg.go, transportHealth).
//
// Probers take **no** lock (CN1) and do **not** use the process OsClient: they
// call the raw block-IO helpers through LegProbeIO (update_01.md U2), because a
// blocked probe must not hold one of the OsClient's DefaultOsClientLimit
// semaphore slots. Their IO can block far past every command timeout — a leg
// with no serving path queues IO indefinitely — and nothing that holds a lock
// ever waits on them: they only publish results into their own registry, which
// the probes read.

const (
	detailsProbeStalled = "health probe stalled"
	detailsProbePending = "health probe pending"
)

// LegProbeIO is the CN11 probers' block-IO dependency. It deliberately does
// NOT go through the process's LimitedOsClient (update_01.md U2): that client
// is a DefaultOsClientLimit-slot semaphore held across the blocking syscall,
// and a probe of a pathless leg (ctrl_loss_tmo = -1 ⇒ IO queues forever)
// would wedge in D state holding a slot. One dead DN can back many legs of one
// CN, at which point every OS operation on the node starves — including the
// teardown `nvme disconnect` that is the documented release mechanism for a
// wedged probe. A hung probe is partly the feature working, so the fix is to
// make hanging harmless, not to prevent it.
//
// Implementations open the device per call (never a cached fd), may block
// indefinitely, must never be called under a lock, and log their own record
// per half.
type LegProbeIO interface {
	Write(ctx context.Context, path string, offset uint64, data []byte) error
	ReadDirect(ctx context.Context, path string, offset, length uint64) ([]byte, error)
}

// directLegProbeIO is the real implementation: the raw helpers of
// common/osclient.go, no semaphore, no lock, no cached fd. It emits one record
// per half itself, because the OsClient that used to log these calls is no
// longer in the path (osclient.md §4.5.1 as amended). The ctx carries only the
// CN2 per-attempt trace id and the cancellation check below — the helpers take
// none, because a pread in flight cannot be interrupted by one.
type directLegProbeIO struct{}

var _ LegProbeIO = directLegProbeIO{}

func (directLegProbeIO) Write(
	ctx context.Context,
	path string,
	offset uint64,
	data []byte,
) error {
	// A cancelled prober starts no new IO — the same fast fail the OsClient's
	// semaphore acquire used to give, and silent for the same reason: an
	// operation that never happened is not logged (osclient.md §4.6).
	if err := ctx.Err(); err != nil {
		return err
	}
	err := common.WriteBlockAt(path, offset, data)
	// The health block is device content; only its shape is ever logged.
	attrs := []any{
		slog.String("path", path),
		slog.Uint64("offset", offset),
		slog.Int("length", len(data)),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	slog.InfoContext(ctx, "probe write block", attrs...)
	return err
}

func (directLegProbeIO) ReadDirect(
	ctx context.Context,
	path string,
	offset, length uint64,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := common.ReadBlockDirectAt(path, offset, length)
	attrs := []any{
		slog.String("path", path),
		slog.Uint64("offset", offset),
		slog.Uint64("length", length),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	slog.InfoContext(ctx, "probe read block direct", attrs...)
	return data, err
}

// legProber is one leg's prober goroutine plus the state it publishes.
type legProber struct {
	legId  uint64
	name   string
	path   string
	offset uint64
	cancel context.CancelFunc

	mu            sync.Mutex
	inflightSince time.Time
	completed     bool
	lastErr       error
}

// snapshot is the lock-free readers' view: {lastOk, lastErr, inflightSince}.
func (p *legProber) snapshot() (time.Time, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflightSince, p.completed, p.lastErr
}

func (p *legProber) begin(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inflightSince = now
}

func (p *legProber) finish(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inflightSince = time.Time{}
	p.completed = true
	p.lastErr = err
}

// startLegProber registers one leg's prober when the wrapper converges. The
// registry is keyed by leg_id, so an already-running prober is left alone —
// the names it probes are deterministic and never change under it.
func (s *CnAgentServer) startLegProber(
	st *cntlrState,
	plan *cntlrPlan,
	lp *legPlan,
) {
	s.mu.Lock()
	if _, ok := st.probers[lp.legId]; ok {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.rootCtx)
	prober := &legProber{
		legId:  lp.legId,
		name:   lp.name,
		path:   lp.path,
		offset: lp.healthOffset,
		cancel: cancel,
	}
	st.probers[lp.legId] = prober
	interval := s.probeInterval
	cnId := plan.cnId
	s.mu.Unlock()
	go s.legProbeLoop(ctx, prober, cnId, interval)
}

// stopLegProbers cancels the probers of legs that are no longer probed — a
// primary→standby flip, a leg leaving the desired state, a level at or above
// SP_LEVEL_NO_SIDE, or a teardown. A goroutine wedged in D state on a pathless
// leg is released by the teardown's own disconnect (deleting the controller
// errors its queued IO) — which the CN21 order now guarantees runs before the
// wrapper removal (removeLeg, update_01.md U2 spec 4) — and is accepted as
// unreclaimable until then (CN11).
func (s *CnAgentServer) stopLegProbers(
	st *cntlrState,
	wanted map[uint64]struct{},
) {
	s.mu.Lock()
	var cancels []context.CancelFunc
	for legId, prober := range st.probers {
		if _, ok := wanted[legId]; ok {
			continue
		}
		cancels = append(cancels, prober.cancel)
		delete(st.probers, legId)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (s *CnAgentServer) legProbeLoop(
	ctx context.Context,
	prober *legProber,
	cnId uint64,
	interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// Single-flight by construction: the round runs inline, so a
		// blocked IO simply delays the next one.
		s.runLegProbe(ctx, prober, cnId)
	}
}

// runLegProbe writes the health block through the leg wrapper and reads it
// back with O_DIRECT. The content is never interpreted — success within the
// timeouts is the whole signal ([D6]) — so concurrent probers of the same
// block from several cntlrs are harmless.
func (s *CnAgentServer) runLegProbe(
	ctx context.Context,
	prober *legProber,
	cnId uint64,
) {
	// Every round is its own traceable operation (CN2): without a fresh id
	// every probe record for the process's whole life would carry the startup
	// reconcile's, and `grep <that id>` — the documented way to isolate the
	// reconcile — would return an unbounded stream of probe IO instead.
	attemptCtx := common.WithTraceId(ctx, common.NewTraceId())
	prober.begin(s.now())
	payload := healthBlockPayload(cnId, s.now().UnixNano())
	// The two records of a round are `probe write block` and `probe read block
	// direct`, emitted by the LegProbeIO itself (update_01.md U2).
	err := s.probeIO.Write(attemptCtx, prober.path, prober.offset, payload)
	if err == nil {
		_, err = s.probeIO.ReadDirect(attemptCtx, prober.path,
			prober.offset, common.LegHealthBlockSize)
	}
	prober.finish(err)
}

// healthBlockPayload is the [D6] payload: magic + writer id + timestamp, in a
// full LegHealthBlockSize buffer so both halves of the probe stay
// O_DIRECT-aligned.
func healthBlockPayload(cnId uint64, unixNano int64) []byte {
	buf := make([]byte, common.LegHealthBlockSize)
	copy(buf, common.LegHealthMagic)
	binary.BigEndian.PutUint64(buf[len(common.LegHealthMagic):], cnId)
	binary.BigEndian.PutUint64(
		buf[len(common.LegHealthMagic)+8:], uint64(unixNano))
	return buf
}

// legProbeOutcome is the CN28 rule: an attempt in flight longer than
// CnLegProbeStallSeconds is an error; otherwise the last completed outcome;
// before any completion the leg is reported OK as "pending", because the
// wrapper exists and nothing has failed yet.
func (s *CnAgentServer) legProbeOutcome(
	st *cntlrState,
	lp *legPlan,
) (pb.ResStatus, string) {
	s.mu.Lock()
	prober := st.probers[lp.legId]
	stall := s.probeStall
	s.mu.Unlock()
	if prober == nil {
		return pb.ResStatus_RES_STATUS_OK, detailsProbePending
	}
	inflightSince, completed, lastErr := prober.snapshot()
	if !inflightSince.IsZero() && s.now().Sub(inflightSince) > stall {
		return pb.ResStatus_RES_STATUS_ERROR, detailsProbeStalled
	}
	if !completed {
		return pb.ResStatus_RES_STATUS_OK, detailsProbePending
	}
	if lastErr != nil {
		return pb.ResStatus_RES_STATUS_ERROR, lastErr.Error()
	}
	return pb.ResStatus_RES_STATUS_OK, ""
}
