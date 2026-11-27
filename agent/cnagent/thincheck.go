package cnagent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The check of a slice's thin-pool metadata before its pool is loaded
// (cnagent.md CN13, The needs-check mark). dm-thin sets a mark in the
// metadata superblock when a commit fails, as when every leg of a data
// group is lost for a few seconds; dm-thin then opens the pool read-only
// wherever it is loaded. Before the primary creates a slice's pool device
// it reads the mark
// from the meta concat; with the mark set it runs `thin_check
// --clear-needs-check-flag` on the metadata in the background, the pool not
// loaded, which clears the mark when its check passes, after which the next
// converge loads the pool as usual. LVM does the same at each activation of
// a thin pool.

// The thin-pool metadata superblock, block 0 of the metadata device
// (dm-thin-metadata.c, struct thin_disk_superblock): a checksum, the flags,
// the block number and the uuid come first, then the magic number.
const (
	thinSbFlagsAt     = 4
	thinSbMagicAt     = 32
	thinSbMagic       = 27022010
	thinSbReadLen     = 4096
	thinSbNeedsCheck  = 1 << 0
	detailsThinCheck  = "checking the thin pool's metadata"
	detailsThinDue    = "the check of its metadata may run again"
	detailsThinGone   = "the needs-check mark of its metadata is gone"
	msgThinCheckStart = "thin-pool metadata check started"
	msgThinCheckEnd   = "thin-pool metadata check ended"
)

// errThinChecking is ensurePool's answer while the background check of a
// slice's metadata runs: the pool's row reads PROVISIONING, and nothing is
// wrong yet.
var errThinChecking = errors.New(detailsThinCheck)

// thinMetaDamaged is ensurePool's answer once thinCheckRefusals runs in a
// row refused a slice's metadata: the metadata is damaged, which a failover
// cannot cure, so the pool is not loaded and its row reads ERROR with
// RES_REASON_POOL_META_DAMAGED; the case stays with the operator.
type thinMetaDamaged struct{ err error }

func (e *thinMetaDamaged) Error() string {
	return fmt.Sprintf("the thin pool's metadata is damaged: %v", e.err)
}

// thinCheckRun is the background check of one slice's metadata: its
// fields are read and written under mu, the goroutine setting them and a
// converge or a read reading them. cancel stops the run (stopThinCheck), and
// ended closes when its goroutine has returned. A run ends passed, refused
// (thin_check ran and refused the metadata) or not run (killed, timed out,
// or never started); tries counts the refusals in a row of this slice's
// runs, which carry over to the next run, and retryAt is when a slice whose
// last run did not pass may run again.
type thinCheckRun struct {
	mu      sync.Mutex
	done    bool
	passed  bool
	err     error
	tries   int
	retryAt time.Time
	cancel  context.CancelFunc
	ended   chan struct{}
}

// result is whether the run has ended, whether it passed, its error, the
// refusals in a row and when the slice may run again.
func (r *thinCheckRun) result() (done, passed bool, err error, tries int,
	retryAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done, r.passed, r.err, r.tries, r.retryAt
}

// thinCheckState says how a slice's pool stands against its metadata
// check, for the plan (newCntlrPlan's checking) and the rows:
// thinCheckNone while no check holds the pool; thinCheckRunning while a run
// goes on or waits to run again; thinCheckDamaged once thinCheckRefusals
// runs in a row refused the metadata.
type thinCheckState int

const (
	thinCheckNone thinCheckState = iota
	thinCheckRunning
	thinCheckDamaged
)

// thinCheckRefusals is how many runs in a row must refuse a slice's
// metadata before the agent reads it damaged: a refusal can also come from
// a cause of the moment that thin_check does not name, which a later run
// does not meet. A run that judged no metadata counts none: one killed or
// timed out, and one that could not read the device (thinCheckCannotRead).
const thinCheckRefusals = 3

// thinCheckCannotRead holds the error texts, as the kernel's strerror words
// them, with which a thin_check that could not read the metadata ends, its
// exit status non-zero as for a refusal: a device another process holds
// open, as a thin_check an earlier agent process left running does, and an
// IO error of the device. Such a run judged no metadata.
var thinCheckCannotRead = []string{"resource busy", "input/output error"}

// thinCheckRefused reports whether a run's error is thin_check's refusal of
// the metadata: the tool ran to its end, under a ctx not cancelled, exited
// non-zero, and named no error of thinCheckCannotRead.
func thinCheckRefused(ctx context.Context, err error) bool {
	var cmdErr *agent.CmdError
	if err == nil || ctx.Err() != nil || !errors.As(err, &cmdErr) ||
		cmdErr.ExitCode <= 0 {
		return false
	}
	out := strings.ToLower(cmdErr.Output)
	for _, text := range thinCheckCannotRead {
		if strings.Contains(out, text) {
			return false
		}
	}
	return true
}

// thinCheckOf is the state of slice sliceId's check in this incarnation,
// under the cntlr's object lock or the node write lock, as tornTds is. A run
// that passed holds nothing: the converge that next finds the mark gone
// loads the pool and drops the entry.
func thinCheckOf(st *cntlrState, sliceId uint64) (thinCheckState, error) {
	run := st.thinChecks[sliceId]
	if run == nil {
		return thinCheckNone, nil
	}
	done, passed, err, tries, _ := run.result()
	switch {
	case !done:
		return thinCheckRunning, nil
	case passed:
		return thinCheckNone, nil
	case tries >= thinCheckRefusals:
		return thinCheckDamaged, err
	}
	return thinCheckRunning, nil
}

// thinChecking is the plan's test of a slice whose pool its metadata check
// holds (newCntlrPlan): a run that goes on, waits to run again, or found the
// metadata damaged. Such a slice's concats are built and its pool is not,
// and every td waits, since its raid0 spans every slice
// (cntlrPlan.anySliceDeferred).
func thinChecking(st *cntlrState) func(uint64) bool {
	return func(sliceId uint64) bool {
		state, _ := thinCheckOf(st, sliceId)
		return state != thinCheckNone
	}
}

// readThinNeedsCheck reads the thin-pool metadata superblock at the start
// of the metadata device dev with direct IO: whether it carries the
// needs-check mark. A block whose magic is not dm-thin's, as the zeroed
// start of a fresh pool's metadata, carries no mark; a read that fails is
// the error.
func (s *CnAgentServer) readThinNeedsCheck(
	ctx context.Context, dev string,
) (bool, error) {
	block, err := s.oc.ReadBlockDirect(ctx, dev, 0, thinSbReadLen)
	if err != nil {
		return false, fmt.Errorf("reading the thin-pool metadata superblock "+
			"of %s: %w", dev, err)
	}
	if len(block) < thinSbMagicAt+8 {
		return false, fmt.Errorf("the thin-pool metadata superblock of %s "+
			"reads %d bytes", dev, len(block))
	}
	le := binary.LittleEndian
	if le.Uint64(block[thinSbMagicAt:]) != thinSbMagic {
		return false, nil
	}
	return le.Uint32(block[thinSbFlagsAt:])&thinSbNeedsCheck != 0, nil
}

// thinCheckGate is ensurePool's step before it creates the slice's pool
// device: nil when the pool may be created, errThinChecking while a check
// of the metadata runs or waits to run again, and thinMetaDamaged once
// thinCheckRefusals runs in a row refused it. It reads the mark
// (readThinNeedsCheck); with no mark it stops and drops the slice's entry,
// and with the mark it starts a run when none is there, when the last one
// passed though the mark still reads set, or when the last one did not
// pass and its pause (thinCheckBackoff) is over, keeping the count of
// refusals in a row.
func (s *CnAgentServer) thinCheckGate(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	sp *slicePlan,
) error {
	meta := s.nf.DmPath(sp.poolMetaName)
	needs, err := s.readThinNeedsCheck(ctx, meta)
	if err != nil {
		return err
	}
	run := st.thinChecks[sp.sliceId]
	if !needs {
		if run != nil {
			s.stopThinCheck(run)
			delete(st.thinChecks, sp.sliceId)
		}
		return nil
	}
	tries := 0
	if run != nil {
		done, passed, runErr, n, retryAt := run.result()
		switch {
		case !done:
			return errThinChecking
		case !passed && n >= thinCheckRefusals:
			return &thinMetaDamaged{err: runErr}
		case !passed && s.now().Before(retryAt):
			return errThinChecking
		case !passed:
			tries = n
		}
	}
	s.startThinCheck(st, plan, sp, meta, tries)
	return errThinChecking
}

// startThinCheck starts the background check of the slice's metadata
// device meta and records it in st.thinChecks: one run of `thin_check
// --clear-needs-check-flag`, which checks the whole metadata and clears the
// mark only when the check passes, as LVM runs it at each activation, under
// common.ThinCheckTimeout, as it can run for minutes, far past an agent
// command's limit and a syncup's. The run is a background task (dnagent.md
// SH27): its ctx derives from rootCtx with a trace id of its own, it is
// enrolled in thinBg, which WaitBackground joins, and it holds one of the
// common.ThinCheckConcurrency places of thinSlots while it runs, so the
// checks of many slices take at most that many of the OsClient's command
// places. It holds no lock: the converge that finds it ended acts on it
// (thinCheckGate), as a read's name of the absent pool brings that converge
// once the run has passed (CN30). tries is the slice's refusals in a row
// before this run. Once the server's join has begun no run starts, and the
// slice reads checking until the next process starts one.
func (s *CnAgentServer) startThinCheck(
	st *cntlrState,
	plan *cntlrPlan,
	sp *slicePlan,
	meta string,
	tries int,
) {
	ctx, cancel := context.WithCancel(
		common.WithTraceId(s.rootCtx, common.NewTraceId()))
	run := &thinCheckRun{cancel: cancel, tries: tries,
		ended: make(chan struct{})}
	st.thinChecks[sp.sliceId] = run
	s.thinMu.Lock()
	if s.thinClosed {
		s.thinMu.Unlock()
		cancel()
		close(run.ended)
		return
	}
	s.thinBg.Add(1)
	s.thinMu.Unlock()
	attrs := append(s.cntlrIdAttrs(plan),
		slog.Uint64("slice_id", sp.sliceId),
		slog.String("meta", meta))
	slog.WarnContext(ctx, msgThinCheckStart, attrs...)
	limit := time.Duration(common.ThinCheckTimeout) * time.Second
	go func() {
		defer s.thinBg.Done()
		defer close(run.ended)
		defer cancel()
		var err error
		select {
		case s.thinSlots <- struct{}{}:
			err = s.cmd.RunOkFor(ctx, limit, "thin_check",
				"--clear-needs-check-flag", meta)
			<-s.thinSlots
		case <-ctx.Done():
			err = ctx.Err()
		}
		refused := thinCheckRefused(ctx, err)
		run.mu.Lock()
		run.done, run.passed, run.err = true, err == nil, err
		switch {
		case err == nil:
			run.tries = 0
		case refused:
			run.tries++
		}
		if err != nil {
			run.retryAt = s.now().Add(s.thinCheckBackoff)
		}
		tries := run.tries
		run.mu.Unlock()
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()),
				slog.Bool("refused", refused),
				slog.Int("refusals_in_a_row", tries))
		}
		slog.WarnContext(ctx, msgThinCheckEnd,
			append(attrs, slog.Bool("passed", err == nil))...)
	}()
}

// stopThinCheck cancels a run and waits for its goroutine to return, which
// kills its thin_check child through the soft and hard timeouts (SH15), so
// the device it held open is free when it returns.
func (s *CnAgentServer) stopThinCheck(run *thinCheckRun) {
	run.cancel()
	<-run.ended
}

// WaitBackground joins the cn agent's thin-pool metadata checks
// (startThinCheck), the background tasks of this role that own a child
// process (dnagent.md SH27): it marks the join begun under thinMu, so a run
// either enrolled before it and is waited for, or never starts, and waits
// for every enrolled run, which rootCtx's cancel, as the stop begins, has
// ended. cmd/dnv-agent passes it to agent.Serve.
func (s *CnAgentServer) WaitBackground() {
	s.thinMu.Lock()
	s.thinClosed = true
	s.thinMu.Unlock()
	s.thinBg.Wait()
}

// keepThinChecks stops and forgets the check runs of slices whose pools
// plan does not build: every run when the cntlr builds no pool (a standby,
// a level below the pools, a cntlr no longer held), and the run of a slice
// the request no longer lists. Each run is waited for, so its thin_check no
// longer holds the meta concat when the sweep that follows tears it down.
// nil plan stops every run, for a cntlr the agent drops.
func (s *CnAgentServer) keepThinChecks(st *cntlrState, plan *cntlrPlan) {
	for sliceId, run := range st.thinChecks {
		if plan != nil && plan.wantPool && plan.sliceById[sliceId] != nil {
			continue
		}
		s.stopThinCheck(run)
		delete(st.thinChecks, sliceId)
	}
}

// nameThinChecksDue names in res, on the read, the pool of a slice whose
// check the converge would take up, as "absent", the pool not loaded, so
// the worker's re-send brings that converge (CN30): one whose last run
// ended without a pass, short of thinCheckRefusals refusals in a row, once
// its pause is over, as the converge's gate then runs the check again; and
// one whose metadata reads damaged once the mark reads gone from its
// superblock, as when an earlier process's thin_check or an operator
// cleared it, as the converge's gate then loads the pool. The rows read
// PROVISIONING or ERROR with RES_REASON_POOL_META_DAMAGED, which the
// verdict names for no other cause (nameCntlrRow). A run that goes on or
// waits out its pause names nothing, nor a damaged slice whose mark reads
// set or does not read; one that passed is named by the probe's absent
// pool.
func (s *CnAgentServer) nameThinChecksDue(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	res *agent.SweepResult,
) {
	if !plan.wantPool {
		return
	}
	now := s.now()
	for _, sp := range plan.slices {
		run := st.thinChecks[sp.sliceId]
		if run == nil {
			continue
		}
		done, passed, _, tries, retryAt := run.result()
		switch {
		case !done || passed:
		case tries < thinCheckRefusals:
			if !now.Before(retryAt) {
				res.Fail(sp.poolFinalName,
					rowFailure(errRowAbsent, detailsThinDue))
			}
		default:
			needs, err := s.readThinNeedsCheck(ctx,
				s.nf.DmPath(sp.poolMetaName))
			if err == nil && !needs {
				res.Fail(sp.poolFinalName,
					rowFailure(errRowAbsent, detailsThinGone))
			}
		}
	}
}

// thinCheckRow is the pool row of a slice whose metadata check holds its
// pool (thinChecking), the converge's and the read's alike: PROVISIONING
// while a run goes on or waits to run again, and ERROR with
// RES_REASON_POOL_META_DAMAGED once the metadata reads damaged.
func thinCheckRow(st *cntlrState, sp *slicePlan) *pb.ResInfo {
	key := resKeyOf(resKeyPoolFmt, sp.sliceId)
	state, err := thinCheckOf(st, sp.sliceId)
	if state == thinCheckDamaged {
		return st.tracker.SetReason(key, sp.poolFinalName,
			pb.ResStatus_RES_STATUS_ERROR,
			(&thinMetaDamaged{err: err}).Error(),
			pb.ResReason_RES_REASON_POOL_META_DAMAGED)
	}
	return st.tracker.Provisioning(key, sp.poolFinalName, detailsThinCheck)
}
