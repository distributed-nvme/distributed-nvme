// The automatic reactions of dnv-worker.md §11 (AR1-AR9), run by the sp
// coordinator of §8.4 as one PASS per SP per cntlr_interval (AR1).
//
// A pass is stateless by construction: it starts from a fresh model.LoadSp
// snapshot plus the in-memory CntlrInfo the PRIMARY cntlr's child last
// reported, decides ONE action (AR2), and forgets everything again. Two
// owners overlapping on one SP therefore cannot apply an action twice — every
// action is a model op that re-validates its own preconditions inside its STM
// (MD6/MD7), and the loser gets model.ErrPrecondition back.
//
// Two spec ambiguities are resolved here, both in one place so a reader does
// not have to reconstruct them:
//
//  1. AR2 says a `reaction skipped` ends the pass, while AR6, AR7 and AR8
//     each define conditions under which a reaction is simply NOT APPLICABLE
//     to a given cntlr, slice or leg. Taken literally both cannot hold. The
//     invariant AR2 is protecting is that AT MOST ONE ACTION IS APPLIED PER
//     SP PER PASS, and "not applicable, keep looking" is not an action, so
//     the pass CONTINUES past: AR5's no failover candidate (AR7's
//     sole-primary variant is defined as "AR5 found none", so §0 item 16
//     would otherwise be unreachable); AR6's grow_pending, meta_ladder_cap
//     and no_data_group (AR6 scopes pending to "no grow OF THAT KIND", and
//     all three can hold indefinitely — a grow deferred on the CN, the §8.5
//     ceiling — so ending the pass would disable AR7 and AR8 for as long as
//     they do); and AR8's leg_has_two_sides and spare_list_full, which move
//     the scan to the next candidate leg (a migration lasts hours and only
//     an operator frees a spare slot, §0 item 17). Everything else ends the
//     pass as AR2 says: every model.ErrPrecondition, every empty allocator
//     scan, every transient op failure, and AR8 step 2's "wait for the
//     pending spare", which clears itself within a provisioning.
//  2. AR7 and AR8 do not say which of several eligible cntlrs / legs to take.
//     Both use the smallest id, the rule AR5 states explicitly, so a pass is
//     deterministic and two overlapping owners choose the same target.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The §12 records of a pass. The strings are normative — the §14 suite greps
// them — so they are constants and never formatted.
const (
	msgReactionApplied = "reaction applied"
	msgReactionSkipped = "reaction skipped"
)

// The "kind" attribute of the two §12 records (AR2). Every reaction the
// worker can run is one of these six; nothing else is ever logged as a kind.
const (
	reactionFailover     = "failover"
	reactionGrowData     = "grow_data"
	reactionGrowMeta     = "grow_meta"
	reactionReplaceCntlr = "replace_cntlr"
	reactionSpareCreate  = "spare_create"
	reactionSpareSwitch  = "spare_switch"
)

// The "reason" attribute of `reaction skipped`. A model.ErrPrecondition
// contributes its own Reason instead of one of these (MD7).
const (
	// reasonNoCandidate is AR5's and the allocator's "nothing to pick".
	reasonNoCandidate = "no candidate"
	// reasonGrowPending is AR6's stateless pending rule. It is model's
	// string: the same reason reaches this record from the pass's pre-check
	// and from a GrowSlice whose STM found the grow pending (AR2).
	reasonGrowPending = model.ReasonGrowPending
	// reasonSparePending is AR8 step 2: a spare is on its way.
	reasonSparePending = "spare_pending"
	// reasonSpareListFull is AR8 step 4: only an operator frees a slot.
	reasonSpareListFull = "spare_list_full"
	// reasonRedundNone is AR8's "RedundNone groups only log" (AR9).
	reasonRedundNone = "redund_none"
	// reasonTwoSides is AR8's "a leg with two sides has a user migration in
	// flight and is left alone".
	reasonTwoSides = "leg_has_two_sides"
	// reasonMetaLadderCap is §8.5's 16 GiB dm-thin metadata ceiling, reached
	// before model.GrowSlice is even called because the allocator has to
	// search for the ladder's size first.
	reasonMetaLadderCap = "meta_ladder_cap"
	// reasonNoDataGroup is a slice with no data group to size a data grow by.
	reasonNoDataGroup = "no_data_group"
	// reasonOpFailed is a transient failure — an etcd error, a scan that did
	// not complete — as opposed to a precondition that did not hold. The
	// record then carries an "error" attribute as well.
	reasonOpFailed = "op_failed"
)

// Non-normative records of the pass. The §12 strings above are the ones the
// §14 suite greps; these exist so an operator can see why an SP is not being
// reacted on at all.
const (
	// msgReactionSuppressed is AR3, logged on the transition only: a deleting
	// SP or one at sp_level >= SP_LEVEL_NO_THINPOOL would otherwise produce
	// one record per cntlr_interval forever.
	msgReactionSuppressed = "reaction suppressed"
	// msgPoolStatusUnparsable is AR6's "an unparsable line is skipped and
	// logged once per change".
	msgPoolStatusUnparsable = "pool status unparsable"
)

// thinMetaBlockSize is dm-thin's FIXED metadata block size (AR6): a thin
// pool's metadata used/total counts are in these 4 KiB blocks, never in the
// pool's data_block_size.
const thinMetaBlockSize = uint64(4096)

// ---------------------------------------------------------------------------
// The model surface (MD5, MD6)
// ---------------------------------------------------------------------------

// reactionOps is everything a pass allocates and mutates through model. It is
// an interface for the same reason spOps is: the §13 tests drive a real
// coordinator over a fixture SpState without etcd. Production is
// modelReactionOps, which does nothing but call model.
type reactionOps interface {
	// findDnCandidates is MD5 for a side allocation (AR6 step, AR8 step 3).
	findDnCandidates(
		ctx context.Context,
		cid uint64,
		cc *pb.ClusterConf,
		candExt uint64,
		candCnt int,
		black []string,
	) ([]model.Cand, error)
	// findCnCandidates is MD5 for a cntlr allocation (AR7).
	findCnCandidates(
		ctx context.Context,
		cid uint64,
		candExt uint64,
		candCnt int,
		black []string,
		spCnAddrs []string,
	) ([]model.Cand, error)
	// failover is AR5.
	failover(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		oldId uint64,
		newId uint64,
		now uint64,
	) error
	// growSlice is AR6; it returns the new group's grp_id. poolTotal is the
	// total the primary reported for the pool of this kind, which the op
	// re-runs the AR6 pending rule against inside its STM (AR2).
	growSlice(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		sliceId uint64,
		isMeta bool,
		poolTotal uint64,
		cc *pb.ClusterConf,
		legs []model.Cand,
	) (uint64, error)
	// replaceCntlr is AR7; it returns the new cntlr_id.
	replaceCntlr(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		oldId uint64,
		newCn model.Cand,
		asPrimary bool,
		now uint64,
	) (uint64, error)
	// createSpareLeg is AR8 step 3; it returns the new leg_id.
	createSpareLeg(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		sliceId uint64,
		grpId uint64,
		dn model.Cand,
		cc *pb.ClusterConf,
	) (uint64, error)
	// switchSpareLeg is AR8 step 1.
	switchSpareLeg(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		spName string,
		sliceId uint64,
		grpId uint64,
		spareLegId uint64,
		targetLegId uint64,
	) error
}

// modelReactionOps is the production reactionOps.
type modelReactionOps struct {
	cli *etcdutil.Client
}

func (o *modelReactionOps) findDnCandidates(
	ctx context.Context,
	cid uint64,
	cc *pb.ClusterConf,
	candExt uint64,
	candCnt int,
	black []string,
) ([]model.Cand, error) {
	return model.FindDnCandidates(
		ctx, o.cli, cid, cc, candExt, candCnt, black, nil,
	)
}

func (o *modelReactionOps) findCnCandidates(
	ctx context.Context,
	cid uint64,
	candExt uint64,
	candCnt int,
	black []string,
	spCnAddrs []string,
) ([]model.Cand, error) {
	return model.FindCnCandidates(
		ctx, o.cli, cid, candExt, candCnt, black, nil, spCnAddrs,
	)
}

func (o *modelReactionOps) failover(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	oldId uint64,
	newId uint64,
	now uint64,
) error {
	return model.Failover(
		ctx, o.cli, cid, shard, spId, spName, oldId, newId, now,
	)
}

func (o *modelReactionOps) growSlice(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	sliceId uint64,
	isMeta bool,
	poolTotal uint64,
	cc *pb.ClusterConf,
	legs []model.Cand,
) (uint64, error) {
	// expectRev 0: the worker holds no client token and converges on what
	// etcd holds (gateway.md §2.2 #3); only the gateway passes a real one.
	return model.GrowSlice(
		ctx, o.cli, cid, shard, spId, spName, 0, sliceId, isMeta,
		poolTotal, cc, legs,
	)
}

func (o *modelReactionOps) replaceCntlr(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	oldId uint64,
	newCn model.Cand,
	asPrimary bool,
	now uint64,
) (uint64, error) {
	return model.ReplaceCntlr(
		ctx, o.cli, cid, shard, spId, spName, oldId, newCn, asPrimary, now,
	)
}

func (o *modelReactionOps) createSpareLeg(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	sliceId uint64,
	grpId uint64,
	dn model.Cand,
	cc *pb.ClusterConf,
) (uint64, error) {
	return model.CreateSpareLeg(
		ctx, o.cli, cid, shard, spId, spName, 0, sliceId, grpId, dn, cc,
	)
}

func (o *modelReactionOps) switchSpareLeg(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	sliceId uint64,
	grpId uint64,
	spareLegId uint64,
	targetLegId uint64,
) error {
	return model.SwitchSpareLeg(
		ctx, o.cli, cid, shard, spId, spName, 0, sliceId, grpId,
		spareLegId, targetLegId,
	)
}

// ---------------------------------------------------------------------------
// The reactor: the little state a stateless pass still keeps
// ---------------------------------------------------------------------------

// reactor is the sp coordinator's §11 half: the model surface, plus the two
// memos that exist only to keep the log honest. No decision is ever taken from
// a memo — AR6's pending rule and AR8's spare rules are reconstructed from
// etcd and the status line on every pass (§0 item 14).
type reactor struct {
	ops reactionOps
	// badLine is the last unparsable pool status line per slice_id, so a
	// steady bad line is logged once per CHANGE and not once per pass (AR6).
	badLine map[uint64]string
	// suppressed is the last AR3 verdict, so msgReactionSuppressed is emitted
	// on the transition only.
	suppressed bool
}

// newReactor builds a reactor over one model surface.
func newReactor(ops reactionOps) *reactor {
	return &reactor{ops: ops, badLine: make(map[uint64]string)}
}

// reactor returns the coordinator's §11 state, building the production one
// from its deps on first use. A coordinator assembled by hand — the plan-only
// test fixture — therefore still runs a well-formed pass.
func (w *spWorker) reactor() *reactor {
	if w.react == nil {
		w.react = newReactor(&modelReactionOps{cli: w.deps.cli})
	}
	return w.react
}

// ---------------------------------------------------------------------------
// The pass (AR1, AR2)
// ---------------------------------------------------------------------------

// spPass is one evaluation of the reactions for one SP (AR1): the fresh
// snapshot, the cluster conf, the resolved thresholds, the primary cntlr with
// the latest CntlrInfo its child reported, and now in unix seconds.
type spPass struct {
	state *model.SpState
	cc    *pb.ClusterConf
	th    *pb.EventThreshold
	now   uint64

	// primaryId and primary are the SP's primary cntlr as the SNAPSHOT sees
	// it; primary is nil for an SP that momentarily has none.
	primaryId uint64
	primary   *pb.Cntlr
	// info is the primary's latest CntlrInfo (pool usage for AR6, spare
	// readiness for AR8); nil when its child has never reported one.
	info *pb.CntlrInfo
	// failoverCand is AR5's election, computed once because AR7's
	// sole-primary variant is defined as "AR5 found none". 0 = none.
	failoverCand uint64
}

// reactionPass is RW20's hook: one automatic-reaction pass for this SP, on the
// coordinator's own cntlr_interval ticker (AR1, AR2).
//
// The snapshot is taken fresh here rather than reused from the fan-out: a pass
// must see the err_epochs this same worker wrote since (HL3), and model.LoadSp
// is one Snapshot (EU4) even though it loads more than a pass reads.
func (w *spWorker) reactionPass(ctx context.Context) {
	state, err := w.ops.loadSp(ctx, w.cid, w.desired.handle)
	if err != nil {
		if !errors.Is(err, model.ErrNotFound) {
			// RW14 logs the load failure of a fan-out the same way; the pass
			// simply runs again on the next tick.
			slog.InfoContext(ctx, msgSpLoadFailed,
				slog.Uint64("cluster_id", w.cid),
				slog.Uint64("sp_id", w.spId),
				slog.String("sp_name", w.desired.handle),
				slog.String("error", err.Error()),
			)
		}
		return
	}
	cc, ok := w.deps.conf.get(w.cid)
	if !ok {
		// RW9: a cluster missing from the cache drives nothing. Every
		// allocating reaction needs its extent_size and batch sizes, and the
		// children already log `cluster conf missing` for the idle period.
		return
	}
	p := w.newPass(state, cc)
	if w.reactionSuppressed(ctx, p) {
		return
	}
	// AR2: the first applicable reaction runs and the pass ends.
	switch {
	case w.tryFailover(ctx, p):
	case w.tryGrow(ctx, p):
	case w.tryReplaceCntlr(ctx, p):
	default:
		w.tryLegRepair(ctx, p)
	}
}

// newPass indexes one snapshot into everything the four reactions read (AR1).
func (w *spWorker) newPass(state *model.SpState, cc *pb.ClusterConf) *spPass {
	p := &spPass{
		state: state,
		cc:    cc,
		th:    model.ResolveEventThreshold(state.Conf.GetEventThreshold()),
		now:   w.deps.clk.nowUnix(),
	}
	for _, cntlrId := range sortedIds(state.Conf.GetCntlrIdList()) {
		cntlr, ok := state.Cntlrs[cntlrId]
		if !ok {
			continue
		}
		if cntlr.GetPrimary() && p.primary == nil {
			p.primaryId, p.primary = cntlrId, cntlr
		}
		if failoverEligible(cntlr) && p.failoverCand == 0 {
			// AR5: the smallest cntlr_id among the healthy, enabled,
			// non-primary cntlrs; the ids are walked ascending.
			p.failoverCand = cntlrId
		}
	}
	if p.primary != nil {
		p.info = w.primaryInfo(p.primaryId)
	}
	return p
}

// failoverEligible is AR5's candidate test on one cntlr: not the primary, not
// disabled (AR3: disabling is the operator's hands-off signal) and healthy.
func failoverEligible(cntlr *pb.Cntlr) bool {
	return !cntlr.GetPrimary() &&
		!cntlr.GetDisabled() &&
		cntlr.GetErrEpoch() == 0
}

// primaryInfo is the latest CntlrInfo the PRIMARY cntlr's child reported
// (AR1). AR6 reads the pool usage out of it and AR8 the spare readiness; a
// cntlr whose child has not reported yet — or that has no child at all,
// because its CN could not be resolved (RW14) — yields nil, and both
// reactions then wait rather than guess.
//
// It is a snapshot, not the live message: the child keeps folding replies into
// its info on its own goroutine while the pass runs on the coordinator's.
func (w *spWorker) primaryInfo(cntlrId uint64) *pb.CntlrInfo {
	child, ok := w.cntlrs[cntlrId]
	if !ok || child.driver == nil {
		return nil
	}
	return child.driver.infoSnapshot()
}

// reactionSuppressed is AR3: no reaction runs for a deleting SP or at
// sp_level >= SP_LEVEL_NO_THINPOOL, where an operator is in charge (§11.7).
// The record is emitted on the transition only.
func (w *spWorker) reactionSuppressed(ctx context.Context, p *spPass) bool {
	conf := p.state.Conf
	reason := ""
	switch {
	case conf.GetDeleting():
		reason = "deleting"
	case conf.GetSpLevel() >= pb.SpLevel_SP_LEVEL_NO_THINPOOL:
		reason = "sp_level"
	}
	r := w.reactor()
	if reason == "" {
		r.suppressed = false
		return false
	}
	if !r.suppressed {
		r.suppressed = true
		slog.InfoContext(ctx, msgReactionSuppressed,
			slog.Uint64("cluster_id", w.cid),
			slog.Uint64("sp_id", w.spId),
			slog.String("reason", reason),
			slog.String("sp_level", conf.GetSpLevel().String()),
		)
	}
	return true
}

// ---------------------------------------------------------------------------
// AR5 — primary failover
// ---------------------------------------------------------------------------

// tryFailover is AR5. It reports whether the pass ends here.
//
// The no-candidate case deliberately does NOT end the pass: see the file
// comment, ambiguity (1). It is the one skip that lets the next reaction run.
func (w *spWorker) tryFailover(ctx context.Context, p *spPass) bool {
	if p.primary == nil {
		return false
	}
	if !reached(p.now, p.primary.GetErrEpoch(), p.th.GetPrimaryUnhealthy()) {
		return false
	}
	if p.failoverCand == 0 {
		w.reactionSkipped(ctx, reactionFailover, reasonNoCandidate,
			slog.Uint64("cntlr_id", p.primaryId),
		)
		return false
	}
	ids := []slog.Attr{
		slog.Uint64("old_cntlr_id", p.primaryId),
		slog.Uint64("new_cntlr_id", p.failoverCand),
	}
	err := w.reactor().ops.failover(
		ctx, w.cid, w.shard, w.spId, w.desired.handle,
		p.primaryId, p.failoverCand, p.now,
	)
	if err != nil {
		w.reactionFailed(ctx, reactionFailover, err, ids...)
		return true
	}
	w.reactionApplied(ctx, reactionFailover, ids...)
	return true
}

// ---------------------------------------------------------------------------
// AR6 — thin-pool auto-grow
// ---------------------------------------------------------------------------

// poolUsage is one thin pool's used/total counts as `dmsetup status` reports
// them (AR6): the metadata pair in dm-thin's fixed 4 KiB metadata blocks, the
// data pair in the pool's data_block_size.
type poolUsage struct {
	usedMeta  uint64
	totalMeta uint64
	usedData  uint64
	totalData uint64
}

// parseThinPoolStatus parses one raw `dmsetup status` line of a thin pool
// (AR6). The fields after the `thin-pool` token are
//
//	<transaction_id> <used_meta>/<total_meta> <used_data>/<total_data> …
//
// so the token is located rather than a fixed column counted: the line may or
// may not be prefixed with the device name and the start/length pair, and
// everything after the two ratios (held metadata root, the rw/ro and discard
// flags, needs_check, the low watermark) is of no interest here.
//
// A pool that has failed reports `thin-pool Fail` and parses as garbage, which
// is exactly right: a failed pool is not grown, it is reported.
func parseThinPoolStatus(details string) (poolUsage, bool) {
	fields := strings.Fields(details)
	token := -1
	for idx, field := range fields {
		if field == "thin-pool" {
			token = idx
			break
		}
	}
	if token < 0 || len(fields) < token+4 {
		return poolUsage{}, false
	}
	usedMeta, totalMeta, ok := parseRatio(fields[token+2])
	if !ok {
		return poolUsage{}, false
	}
	usedData, totalData, ok := parseRatio(fields[token+3])
	if !ok {
		return poolUsage{}, false
	}
	return poolUsage{
		usedMeta:  usedMeta,
		totalMeta: totalMeta,
		usedData:  usedData,
		totalData: totalData,
	}, true
}

// parseRatio parses one `<used>/<total>` field of a thin-pool status line.
func parseRatio(field string) (uint64, uint64, bool) {
	used, total, found := strings.Cut(field, "/")
	if !found {
		return 0, 0, false
	}
	usedVal, err := strconv.ParseUint(used, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	totalVal, err := strconv.ParseUint(total, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return usedVal, totalVal, true
}

// parsePoolStatus is parseThinPoolStatus plus AR6's "an unparsable line is
// skipped and logged once per change": the memo is per slice and is dropped
// again as soon as the slice reports a line that parses, so a line that goes
// bad, good and bad again is reported each time it turns.
func (w *spWorker) parsePoolStatus(
	ctx context.Context,
	sliceId uint64,
	details string,
) (poolUsage, bool) {
	usage, ok := parseThinPoolStatus(details)
	r := w.reactor()
	if ok {
		delete(r.badLine, sliceId)
		return usage, true
	}
	if last, seen := r.badLine[sliceId]; seen && last == details {
		return poolUsage{}, false
	}
	r.badLine[sliceId] = details
	slog.InfoContext(ctx, msgPoolStatusUnparsable,
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("sp_id", w.spId),
		slog.Uint64("slice_id", sliceId),
		slog.String("details", details),
	)
	return poolUsage{}, false
}

// tryGrow is AR6. It reports whether the pass ends here.
//
// Slices are walked in SpConf.slice_id_list order and DATA is checked before
// metadata, so a pool breaching both grows its data first — the one that runs
// out with user IO on it.
//
// A breach the worker cannot turn into a grow is NOT an action, and does not
// end the pass. AR6 scopes its pending rule to "no grow OF THAT KIND starts",
// so a pending data grow leaves the same slice's metadata — and every other
// slice — free to grow; and a grow that is pending on the CN ([D15], §10.4)
// or capped by the §8.5 meta ladder can stay that way indefinitely, so ending
// the pass on it would disable AR7 and AR8 for the whole SP for exactly as
// long. Only an APPLIED grow, or one of the two skips AR2 makes pass-ending
// (ErrPrecondition, no candidate), ends the pass.
func (w *spWorker) tryGrow(ctx context.Context, p *spPass) bool {
	lwm := uint64(
		p.state.Conf.GetBdevConf().GetDmPoolConf().GetLowWaterMarkPct(),
	)
	if lwm == 0 {
		lwm = common.DefaultPoolLowWatermarkPct
	}
	if lwm > 100 {
		// §7: values above 100 switch the automation off; operators grow
		// manually.
		return false
	}
	if p.info == nil {
		return false
	}
	rows := p.info.GetSliceIdToDmPool()
	for _, sliceId := range p.state.Conf.GetSliceIdList() {
		slice, ok := p.state.Slices[sliceId]
		if !ok {
			continue
		}
		row, ok := rows[sliceId]
		if !ok || row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			// AR6: an ERROR / PROVISIONING / absent pool is never grown.
			continue
		}
		usage, ok := w.parsePoolStatus(ctx, sliceId, row.GetDetails())
		if !ok {
			continue
		}
		// Each kind is tried on its own: AR6's pending rule is per kind,
		// so a data grow the CN has not acted on yet must not shadow a
		// metadata breach of the same pool — metadata filling up puts the
		// pool into needs_check and the whole SP read-only.
		if usage.usedData*100 > lwm*usage.totalData &&
			w.runGrow(ctx, p, sliceId, slice, false, usage) {
			return true
		}
		if usage.usedMeta*100 > lwm*usage.totalMeta &&
			w.runGrow(ctx, p, sliceId, slice, true, usage) {
			return true
		}
	}
	return false
}

// runGrow runs one AR6 grow of one kind on one slice. It reports whether the
// pass ends here, which distinguishes the two shapes of "no grow ran":
//
//   - the grow is not APPLICABLE — pending (AR6), the §8.5 ladder cap, no data
//     group to size a data grow by: the record is emitted and false is
//     returned, so the walk goes on to the other kind, the next slice and
//     finally to AR7 and AR8;
//   - the grow WAS applicable and did not complete — no candidate, an
//     ErrPrecondition, a failed scan or op: AR2 ends the pass, so that a pass
//     that has already touched etcd (or may have) applies nothing else.
func (w *spWorker) runGrow(
	ctx context.Context,
	p *spPass,
	sliceId uint64,
	slice *pb.Slice,
	isMeta bool,
	usage poolUsage,
) bool {
	kind := reactionGrowData
	if isMeta {
		kind = reactionGrowMeta
	}
	sliceAttr := slog.Uint64("slice_id", sliceId)
	blockSize := dataBlockSize(p.state.Conf.GetBdevConf())
	if growPending(slice, isMeta, usage, blockSize) {
		// AR6: while pending, no grow of THAT KIND starts. Another kind, and
		// every other reaction, is untouched.
		w.reactionSkipped(ctx, kind, reasonGrowPending, sliceAttr)
		return false
	}
	extentSize := model.ResolveDnBinConf(p.cc.GetDnBinConf()).GetExtentSize()
	extCnt, reason := growExtCnt(slice, isMeta, extentSize)
	if reason != "" {
		// meta_ladder_cap and no_data_group are both permanent for this
		// slice and this kind: there is no grow to run, ever, and the pass
		// must go on to the reactions that still can do something.
		w.reactionSkipped(ctx, kind, reason, sliceAttr)
		return false
	}
	legs := legCnt(p.state.Conf)
	batch := int(
		model.ResolveAllocConf(p.cc.GetAllocConf()).GetDnBatchSize(),
	)
	// AR6: the black list starts empty — a new group may perfectly well land
	// on a DN that already carries another group of this SP — and grows as
	// the picks are drawn, so the legs of the ONE new group land on distinct
	// DNs.
	cands, err := w.reactor().ops.findDnCandidates(
		ctx, w.cid, p.cc, extCnt, legs*batch, nil,
	)
	if err != nil {
		w.reactionFailed(ctx, kind, err, sliceAttr)
		return true
	}
	picks := pickDistinct(cands, legs)
	if len(picks) < legs {
		w.reactionSkipped(ctx, kind, reasonNoCandidate, sliceAttr)
		return true
	}
	grpId, err := w.reactor().ops.growSlice(
		ctx, w.cid, w.shard, w.spId, w.desired.handle,
		sliceId, isMeta, poolTotal(usage, isMeta), p.cc, picks,
	)
	if err != nil {
		w.reactionFailed(ctx, kind, err, sliceAttr)
		return true
	}
	w.reactionApplied(ctx, kind,
		sliceAttr,
		slog.Uint64("grp_id", grpId),
		slog.Uint64("ext_cnt", extCnt),
		slog.Any("addr_port_list", candAddrs(picks)),
	)
	return true
}

// poolTotal is the total the primary reported for the pool of one kind
// (AR6): the metadata total for a meta grow, the data total for a data one.
func poolTotal(usage poolUsage, isMeta bool) uint64 {
	if isMeta {
		return usage.totalMeta
	}
	return usage.totalData
}

// growPending is AR6's stateless pending rule as the pass's cheap pre-check:
// it saves a candidate scan for a grow that cannot run. The rule itself is
// model.GrowPending, which model.GrowSlice also applies inside its STM as the
// precondition AR2 requires — one rule, one implementation, so the pass and
// the transaction can never disagree about what "pending" means.
func growPending(
	slice *pb.Slice,
	isMeta bool,
	usage poolUsage,
	blockSize uint64,
) bool {
	return model.GrowPending(slice, isMeta, poolTotal(usage, isMeta), blockSize)
}

// growExtCnt is the ext_cnt of the group a grow would append (§8.5). The
// worker computes it because the allocator must search for exactly that size
// BEFORE model.GrowSlice recomputes it inside its own STM: a data grow uses
// the slice's first data group's ext_cnt — the original allocation unit — and
// a meta grow the ladder value, which doubles the slice's meta total.
//
// The second return value is a `reaction skipped` reason, empty on success.
func growExtCnt(
	slice *pb.Slice,
	isMeta bool,
	extentSize uint64,
) (uint64, string) {
	if !isMeta {
		grps := slice.GetDataGrpList()
		if len(grps) == 0 || grps[0].GetExtCnt() == 0 {
			return 0, reasonNoDataGroup
		}
		return grps[0].GetExtCnt(), ""
	}
	total := uint64(0)
	for _, grp := range slice.GetMetaGrpList() {
		total += grp.GetExtCnt()
	}
	extCnt, ok := model.MetaLadderExtCnt(total, extentSize)
	if !ok {
		return 0, reasonMetaLadderCap
	}
	return extCnt, ""
}

// ---------------------------------------------------------------------------
// AR7 — cntlr replacement
// ---------------------------------------------------------------------------

// tryReplaceCntlr is AR7. It reports whether the pass ends here.
func (w *spWorker) tryReplaceCntlr(ctx context.Context, p *spPass) bool {
	oldId, old := replaceTarget(p)
	if old == nil {
		return false
	}
	ids := []slog.Attr{slog.Uint64("old_cntlr_id", oldId)}
	// The footprint is what one cntlr's CN reserves for the whole SP (§8.6),
	// and it is what model.ReplaceCntlr re-computes inside its STM: the scan
	// has to ask for the same number or the pick would fail there.
	footprint := spFootprint(p.state)
	batch := int(
		model.ResolveAllocConf(p.cc.GetAllocConf()).GetCnBatchSize(),
	)
	// AR7: the old CN is black-listed even when the node itself is healthy —
	// its cntlr is what failed. The SP's other cntlrs' CNs are excluded by
	// the §6.4 rule instead, which is spCnAddrs.
	cands, err := w.reactor().ops.findCnCandidates(
		ctx, w.cid, footprint, batch,
		[]string{old.GetAddrPort()}, otherCntlrAddrs(p, oldId),
	)
	if err != nil {
		w.reactionFailed(ctx, reactionReplaceCntlr, err, ids...)
		return true
	}
	picks := model.PickRandom(cands, 1)
	if len(picks) == 0 {
		w.reactionSkipped(ctx, reactionReplaceCntlr, reasonNoCandidate, ids...)
		return true
	}
	newId, err := w.reactor().ops.replaceCntlr(
		ctx, w.cid, w.shard, w.spId, w.desired.handle,
		oldId, picks[0], old.GetPrimary(), p.now,
	)
	if err != nil {
		w.reactionFailed(ctx, reactionReplaceCntlr, err, ids...)
		return true
	}
	w.reactionApplied(ctx, reactionReplaceCntlr,
		slog.Uint64("old_cntlr_id", oldId),
		slog.Uint64("new_cntlr_id", newId),
		slog.String("addr_port", picks[0].AddrPort),
		slog.Bool("primary", old.GetPrimary()),
	)
	return true
}

// replaceTarget is AR7's trigger: the cntlr with the smallest cntlr_id that
// has been unhealthy for cntlr_unhealthy, is not disabled (AR3), and is
// either not the primary or is the primary of an SP with no failover
// candidate — the sole-cntlr SP of §0 item 16.
func replaceTarget(p *spPass) (uint64, *pb.Cntlr) {
	for _, cntlrId := range sortedIds(p.state.Conf.GetCntlrIdList()) {
		cntlr, ok := p.state.Cntlrs[cntlrId]
		if !ok {
			continue
		}
		if cntlr.GetDisabled() {
			continue
		}
		if !reached(p.now, cntlr.GetErrEpoch(), p.th.GetCntlrUnhealthy()) {
			continue
		}
		if cntlr.GetPrimary() && p.failoverCand != 0 {
			// AR5 will move the role away first; replacing it as primary
			// would fail model.ReplaceCntlr's own precondition anyway.
			continue
		}
		return cntlrId, cntlr
	}
	return 0, nil
}

// otherCntlrAddrs are the endpoints of every cntlr of the SP except the one
// being replaced: §6.4 forbids two cntlrs of one SP on one CN, and MD5 takes
// that list as spCnAddrs.
func otherCntlrAddrs(p *spPass, oldId uint64) []string {
	addrs := make([]string, 0, len(p.state.Cntlrs))
	for _, cntlrId := range sortedIds(p.state.Conf.GetCntlrIdList()) {
		if cntlrId == oldId {
			continue
		}
		cntlr, ok := p.state.Cntlrs[cntlrId]
		if !ok {
			continue
		}
		addrs = append(addrs, cntlr.GetAddrPort())
	}
	return addrs
}

// spFootprint is the Σ ext_cnt over ALL groups of ALL slices of the SP, meta
// and data alike — the worker-side twin of model's, which recomputes it inside
// the STM (§8.6).
func spFootprint(state *model.SpState) uint64 {
	total := uint64(0)
	for _, sliceId := range state.Conf.GetSliceIdList() {
		slice, ok := state.Slices[sliceId]
		if !ok {
			continue
		}
		for _, grp := range allGroups(slice) {
			total += grp.GetExtCnt()
		}
	}
	return total
}

// ---------------------------------------------------------------------------
// AR8 — leg repair
// ---------------------------------------------------------------------------

// repairTarget is one leg the pass may repair, with everything the ops need to
// address it.
type repairTarget struct {
	sliceId uint64
	grp     *pb.Group
	leg     *pb.Leg
}

// tryLegRepair is AR8: one step per pass on the group of the unhealthy leg
// with the smallest leg_id. It reports whether the pass ends here.
//
// AR8's two PER-LEG preconditions — "the leg has exactly one side" and step
// 4's full spare list — are part of the candidate test, not reasons to end the
// pass. Both are properties of ONE leg and ONE group, and both can hold for a
// very long time: a two-sided leg has a user migration in flight (hours), and
// only DeleteSpareLeg by an operator frees a spare slot (§0 item 17). Ending
// the pass on either would leave every OTHER group of the SP degraded on a
// single md-raid1 member for exactly as long, so a further failure there is
// data loss. Each is still recorded per leg for visibility (§14.11 case D
// step 9 greps `reason=spare_list_full`) and the scan moves on.
func (w *spWorker) tryLegRepair(ctx context.Context, p *spPass) bool {
	for _, target := range repairCandidates(p) {
		ids := []slog.Attr{
			slog.Uint64("slice_id", target.sliceId),
			slog.Uint64("grp_id", target.grp.GetGrpId()),
			slog.Uint64("leg_id", target.leg.GetLegId()),
		}
		if !isMdRaid1(p.state.Conf) {
			// AR8/AR9: a RedundNone group has no redundancy to re-home, so
			// the worker only ever logs it. An operator moves the data.
			// Redundancy is an SP-wide property (§8.5), so no later
			// candidate can be any different and the pass ends here.
			w.reactionSkipped(
				ctx, reactionSpareCreate, reasonRedundNone, ids...,
			)
			return true
		}
		if len(target.leg.GetSideList()) != 1 {
			// AR8: two sides means a user migration is in flight on this
			// leg. The next candidate may still be repairable.
			w.reactionSkipped(
				ctx, reactionSpareCreate, reasonTwoSides, ids...,
			)
			continue
		}
		if spare := readySpare(target.grp, p.info); spare != nil {
			return w.switchSpare(ctx, target, spare, ids)
		}
		if spare := pendingSpare(target.grp, p.info); spare != nil {
			// The kind names the step of the AR8 procedure that is being
			// deferred: here the switch, which runs as soon as the spare is
			// ready. Every other AR8 skip names the create — the step the
			// procedure would otherwise have started with.
			//
			// This one DOES end the pass: AR8 step 2 is "wait for it", and
			// the wait is bounded by the spare's provisioning — unlike the
			// two preconditions above, it clears itself.
			w.reactionSkipped(ctx, reactionSpareSwitch, reasonSparePending,
				withAttr(ids, slog.Uint64("spare_leg_id", spare.GetLegId()))...,
			)
			return true
		}
		if len(target.grp.GetSpareLegList()) >= common.MaxSpareLegPerGrp {
			// §0 item 17: the worker never deletes a parked leg; only
			// DeleteSpareLeg by an operator frees a slot. That is an
			// operator event on THIS group, so the scan goes on to the next
			// candidate rather than stranding the rest of the SP behind it.
			w.reactionSkipped(
				ctx, reactionSpareCreate, reasonSpareListFull, ids...,
			)
			continue
		}
		return w.createSpare(ctx, p, target, ids)
	}
	return false
}

// repairCandidates is AR8's trigger scan over BOTH group lists of every slice:
// every leg_list leg that needs repair, in ascending leg_id order — AR8's
// "several unhealthy legs ⇒ the smallest leg_id first". Spare legs are never
// scanned — a parked leg keeps its err_epoch and is never repaired again
// (§0 item 17).
//
// The whole ordered list is returned rather than only its head because AR8's
// per-leg preconditions are applied by tryLegRepair as it walks it: the
// smallest-leg_id ordering has to run over the legs that are actually
// repairable, or one leg an operator has to unblock would block them all.
func repairCandidates(p *spPass) []*repairTarget {
	var found []*repairTarget
	for _, sliceId := range p.state.Conf.GetSliceIdList() {
		slice, ok := p.state.Slices[sliceId]
		if !ok {
			continue
		}
		for _, grp := range allGroups(slice) {
			for _, leg := range grp.GetLegList() {
				if !legNeedsRepair(p, leg) {
					continue
				}
				found = append(found, &repairTarget{
					sliceId: sliceId, grp: grp, leg: leg,
				})
			}
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		return found[i].leg.GetLegId() < found[j].leg.GetLegId()
	})
	return found
}

// legNeedsRepair is AR8's two triggers. Both require Leg.err_epoch != 0: a
// side the worker cannot reach while the primary still sees the leg healthy
// triggers nothing.
//
//	Case 1 — the leg has been unhealthy for leg_unhealthy: the primary has no
//	         healthy path to it, possibly a CN↔DN problem, hence the long wait.
//	Case 2 — the leg is unhealthy AND its side has been unhealthy for
//	         side_unhealthy: the DN itself is probably dead, hence the short
//	         wait.
func legNeedsRepair(p *spPass, leg *pb.Leg) bool {
	if leg.GetErrEpoch() == 0 {
		return false
	}
	if reached(p.now, leg.GetErrEpoch(), p.th.GetLegUnhealthy()) {
		return true
	}
	for _, side := range leg.GetSideList() {
		if reached(p.now, side.GetErrEpoch(), p.th.GetSideUnhealthy()) {
			return true
		}
	}
	return false
}

// spareReady is AR8 step 1's readiness test on one spare: its single side is
// provisioned (§9.4) and the PRIMARY's latest report has its leg
// RES_STATUS_OK — connected and probed (§8.12).
//
// Leg.err_epoch is deliberately NOT part of it: a leg the primary reports OK
// has had its err_epoch cleared by HL2 already, so a parked leg that recovers
// becomes usable spare capacity again, which is exactly what parking it was
// for.
func spareReady(spare *pb.Leg, info *pb.CntlrInfo) bool {
	side := singleSide(spare)
	if side == nil || !side.GetProvisioned() {
		return false
	}
	row, ok := info.GetLegIdToLeg()[spare.GetLegId()]
	return ok && row.GetStatus() == pb.ResStatus_RES_STATUS_OK
}

// readySpare is AR8 step 1: the ready spare with the smallest leg_id. A
// user-created ready spare qualifies the same way; that is what spares are
// for.
func readySpare(grp *pb.Group, info *pb.CntlrInfo) *pb.Leg {
	for _, spare := range sortedLegs(grp.GetSpareLegList()) {
		if spareReady(spare, info) {
			return spare
		}
	}
	return nil
}

// pendingSpare is AR8 step 2: a spare that is on its way — its leg and its
// side are both healthy — but is not ready yet, either still provisioning or
// not yet reported OK by the primary. A spare with an err_epoch of its own is
// neither ready nor pending: it is a dead spare, and a PARKED old leg is the
// commonest one, which is what makes a second repair of the same group create
// a second spare rather than wait for one that will never arrive.
func pendingSpare(grp *pb.Group, info *pb.CntlrInfo) *pb.Leg {
	for _, spare := range sortedLegs(grp.GetSpareLegList()) {
		if spare.GetErrEpoch() != 0 {
			continue
		}
		side := singleSide(spare)
		if side == nil || side.GetErrEpoch() != 0 {
			continue
		}
		if spareReady(spare, info) {
			continue
		}
		return spare
	}
	return nil
}

// switchSpare runs AR8 step 1.
func (w *spWorker) switchSpare(
	ctx context.Context,
	target *repairTarget,
	spare *pb.Leg,
	ids []slog.Attr,
) bool {
	ids = withAttr(ids, slog.Uint64("spare_leg_id", spare.GetLegId()))
	err := w.reactor().ops.switchSpareLeg(
		ctx, w.cid, w.shard, w.spId, w.desired.handle,
		target.sliceId, target.grp.GetGrpId(),
		spare.GetLegId(), target.leg.GetLegId(),
	)
	if err != nil {
		w.reactionFailed(ctx, reactionSpareSwitch, err, ids...)
		return true
	}
	w.reactionApplied(ctx, reactionSpareSwitch, ids...)
	return true
}

// createSpare runs AR8 step 3: a spare on a FRESH DN, black-listing the DNs of
// every leg and every spare of the group so the replacement never shares the
// failure domain it exists to replace.
func (w *spWorker) createSpare(
	ctx context.Context,
	p *spPass,
	target *repairTarget,
	ids []slog.Attr,
) bool {
	batch := int(
		model.ResolveAllocConf(p.cc.GetAllocConf()).GetDnBatchSize(),
	)
	cands, err := w.reactor().ops.findDnCandidates(
		ctx, w.cid, p.cc, target.grp.GetExtCnt(), batch,
		grpAddrs(target.grp),
	)
	if err != nil {
		w.reactionFailed(ctx, reactionSpareCreate, err, ids...)
		return true
	}
	picks := model.PickRandom(cands, 1)
	if len(picks) == 0 {
		w.reactionSkipped(ctx, reactionSpareCreate, reasonNoCandidate, ids...)
		return true
	}
	legId, err := w.reactor().ops.createSpareLeg(
		ctx, w.cid, w.shard, w.spId, w.desired.handle,
		target.sliceId, target.grp.GetGrpId(), picks[0], p.cc,
	)
	if err != nil {
		w.reactionFailed(ctx, reactionSpareCreate, err, ids...)
		return true
	}
	w.reactionApplied(ctx, reactionSpareCreate,
		withAttr(ids,
			slog.Uint64("spare_leg_id", legId),
			slog.String("addr_port", picks[0].AddrPort),
		)...,
	)
	return true
}

// grpAddrs is AR8's black list: the DN of every side of every leg and every
// spare of the group.
func grpAddrs(grp *pb.Group) []string {
	var addrs []string
	for _, legList := range [][]*pb.Leg{
		grp.GetLegList(), grp.GetSpareLegList(),
	} {
		for _, leg := range legList {
			for _, side := range leg.GetSideList() {
				addrs = append(addrs, side.GetAddrPort())
			}
		}
	}
	return addrs
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// reached is AR4's threshold comparison `now − err_epoch ≥ T`, written so that
// it can never underflow: a zero epoch is healthy, and a clock that has gone
// backwards has simply not reached the threshold yet. It is the worker-side
// twin of model's, which re-applies it inside every op's STM.
func reached(now uint64, errEpoch uint64, threshold uint32) bool {
	if errEpoch == 0 || now < errEpoch {
		return false
	}
	return now-errEpoch >= uint64(threshold)
}

// isMdRaid1 reports whether the SP's groups are md-raid1 groups (AR8).
// Redundancy is an SP-wide property; a group only inherits it.
func isMdRaid1(conf *pb.SpConf) bool {
	return conf.GetBdevConf().GetRedundConf().GetRedundMdRaid1() != nil
}

// legCnt is how many legs one new group of the SP has (AR6): 2 for
// RedundMdRaid1, 1 for RedundNone.
func legCnt(conf *pb.SpConf) int {
	if isMdRaid1(conf) {
		return 2
	}
	return 1
}

// allGroups is a slice's meta groups followed by its data groups.
func allGroups(slice *pb.Slice) []*pb.Group {
	grps := make([]*pb.Group, 0,
		len(slice.GetMetaGrpList())+len(slice.GetDataGrpList()))
	grps = append(grps, slice.GetMetaGrpList()...)
	return append(grps, slice.GetDataGrpList()...)
}

// singleSide returns a leg's one side, or nil when it does not have exactly
// one (AR8: a two-sided leg is a migration in flight).
func singleSide(leg *pb.Leg) *pb.Side {
	if len(leg.GetSideList()) != 1 {
		return nil
	}
	return leg.GetSideList()[0]
}

// sortedLegs returns a leg list in ascending leg_id order, leaving the stored
// list — whose ORDER is the md member order (§8.12) — untouched.
func sortedLegs(legList []*pb.Leg) []*pb.Leg {
	sorted := append([]*pb.Leg(nil), legList...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].GetLegId() < sorted[j].GetLegId()
	})
	return sorted
}

// sortedIds returns a copy of an id list in ascending order, so "the smallest
// id" is well defined however the list happens to be stored.
func sortedIds(ids []uint64) []uint64 {
	sorted := append([]uint64(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted
}

// withAttr returns ids plus extra as a NEW slice, so a caller that hands the
// same id list to two records cannot have the first one overwrite the second's
// backing array.
func withAttr(ids []slog.Attr, extra ...slog.Attr) []slog.Attr {
	out := make([]slog.Attr, 0, len(ids)+len(extra))
	out = append(out, ids...)
	return append(out, extra...)
}

// candAddrs are the endpoints of a pick set, for the `reaction applied`
// record.
func candAddrs(cands []model.Cand) []string {
	addrs := make([]string, 0, len(cands))
	for _, cand := range cands {
		addrs = append(addrs, cand.AddrPort)
	}
	return addrs
}

// pickDistinct draws n candidates at random with a GROWING black list (AR6),
// so the legs of one new group land on distinct DNs. The scan already returns
// one candidate per node and per location, so the black list only has to hold
// what has been drawn; it returns fewer than n when the scan did.
func pickDistinct(cands []model.Cand, n int) []model.Cand {
	picked := make([]model.Cand, 0, n)
	remaining := append([]model.Cand(nil), cands...)
	for len(picked) < n && len(remaining) > 0 {
		one := model.PickRandom(remaining, 1)
		if len(one) == 0 {
			return picked
		}
		pick := one[0]
		picked = append(picked, pick)
		kept := remaining[:0]
		for _, cand := range remaining {
			if cand.AddrPort != pick.AddrPort {
				kept = append(kept, cand)
			}
		}
		remaining = kept
	}
	return picked
}

// ---------------------------------------------------------------------------
// The §12 records (AR2)
// ---------------------------------------------------------------------------

// reactionApplied logs the §12 `reaction applied` record. The revision is the
// SpRev the op's STM bumped, read back the way the flips read it (RW18): the
// ops report their new ids, not the revision, and a concurrent bump would
// make this report a slightly newer one — a log detail, not a decision.
func (w *spWorker) reactionApplied(
	ctx context.Context,
	kind string,
	ids ...slog.Attr,
) {
	attrs := []any{
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("sp_id", w.spId),
		slog.String("kind", kind),
	}
	for _, id := range ids {
		attrs = append(attrs, id)
	}
	attrs = append(attrs, slog.Uint64("revision", w.currentRevision(ctx)))
	slog.InfoContext(ctx, msgReactionApplied, attrs...)
}

// reactionSkipped logs the §12 `reaction skipped` record: the reaction was
// applicable but did not run.
func (w *spWorker) reactionSkipped(
	ctx context.Context,
	kind string,
	reason string,
	ids ...slog.Attr,
) {
	attrs := []any{
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("sp_id", w.spId),
		slog.String("kind", kind),
		slog.String("reason", reason),
	}
	for _, id := range ids {
		attrs = append(attrs, id)
	}
	slog.InfoContext(ctx, msgReactionSkipped, attrs...)
}

// reactionFailed turns one op error into a `reaction skipped` record (AR2): a
// model.ErrPrecondition contributes the precondition that did not hold, which
// is not a failure at all — the next pass re-evaluates — and anything else is
// a transient failure that additionally carries the error text.
func (w *spWorker) reactionFailed(
	ctx context.Context,
	kind string,
	err error,
	ids ...slog.Attr,
) {
	var pre *model.ErrPrecondition
	if errors.As(err, &pre) {
		w.reactionSkipped(ctx, kind, pre.Reason, ids...)
		return
	}
	w.reactionSkipped(ctx, kind, reasonOpFailed,
		withAttr(ids, slog.String("error", err.Error()))...,
	)
}
