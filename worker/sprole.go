// The sp role of dnv-worker.md (RW14-RW20, RW22).
//
// The sp revision worker is not a per-object loop but a COORDINATOR: on every
// desired change (an SpRev put: revision + sp_name) it loads the whole SP with
// model.LoadSp, builds one SyncupSideRequest per side — spare legs' sides
// included — and one SyncupCntlrRequest per cntlr (RW15/RW16), and diffs those
// against its running children. Each child is an ordinary revision worker
// (RW1-RW12) with the side resp. cntlr driver below, so "one goroutine, one
// Check* stream, one round timer per object" (RW1) holds for sides and cntlrs
// exactly as it does for DNs and CNs.
//
// What the coordinator keeps for itself is everything that is not per-object:
// the two flips (RW18/RW19), the Leg health rows — which the PRIMARY cntlr
// reports but which are written on the SLICE, so only the coordinator knows
// where they go (HL2) — and the reaction pass (AR1-AR10).
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// msgFlipApplied is the record of the two flips (RW18, RW19; dnv-worker.md, Log
// records).
const msgFlipApplied = "flip applied"

// The "kind" attribute of the "flip applied" record (dnv-worker.md, Log
// records).
const (
	flipKindProvisioned = "provisioned"
	flipKindCreated     = "created"
)

// Non-normative records of the sp coordinator. The strings of dnv-worker.md,
// Log records, are the ones the worker suite (dnv-worker.md, Integration test
// plan) greps; these exist so an operator can see why an SP is not
// being driven.
const (
	// msgSpDeleting is RW14's ErrNotFound path: the SpConf is gone, the SP is
	// being deleted and the SpRev delete follows.
	msgSpDeleting = "sp deleting"
	// msgSpLoadFailed is a transient model.LoadSp failure; the coordinator
	// retries on its own ticker.
	msgSpLoadFailed = "sp load failed"
	// msgSpChildUnresolved is RW14's idle child: an endpoint with no
	// DnConf/CnConf yields no dn_id/cn_id, so no request can be built for it.
	msgSpChildUnresolved = "sp child unresolved"
	// msgSpFlipFailed reports a flip STM that did not commit.
	msgSpFlipFailed = "sp flip failed"
	// msgSpStandbyLegRow is HL2's "a standby's leg row is logged, never
	// recorded".
	msgSpStandbyLegRow = "standby leg row"
	// msgSpSubObjectMissing is MD3's "a listed sub-object whose key is
	// missing is reported in SpState.Missing and LOGGED BY THE CALLER".
	msgSpSubObjectMissing = "sp sub-object missing"
	// msgSpSidesIdle is RW15's side_conf completeness gate: no side of the SP
	// can be driven while the SP's cntlr set is not fully resolved.
	msgSpSidesIdle = "sp sides idle"
)

// The records the sp coordinator emits when a hold ends by its timer (RW14,
// RW22). dnv-worker.md, Log records, lists both, so their names are
// normative.
const (
	// msgSpSidesUnsynced is RW14's sides-first barrier released by its timer:
	// one cntlr_interval passed before every side child reported the
	// fan-out's revision applied, and the cntlrs are sent it all the same.
	msgSpSidesUnsynced = "sp sides unsynced"
	// msgSpDemotionUnsynced is RW22's demotion hold ended by its timer: a
	// demoted cntlr did not report its demotion applied within
	// common.DemotionHoldTimeout, and the sides are sent the fan-out all the
	// same.
	msgSpDemotionUnsynced = "sp demotion unsynced"
)

// spConfRefusal is the memo that keeps a steady invalid stored conf to one
// Error record instead of one per tick, keyed on the error text so a conf that
// changes from one invalid value to another still reports.
type spConfRefusal struct {
	bdev string
	cc   string
}

// ---------------------------------------------------------------------------
// The model surface (MD3, MD6)
// ---------------------------------------------------------------------------

// spOps is everything the sp coordinator reads and writes through model. It is
// an interface so the unit tests can drive a real coordinator against a fixture
// SpState without etcd; production is modelSpOps, which does nothing but call
// model (whose ops re-validate inside their own STM).
type spOps interface {
	// loadSp is MD3: the SP's whole desired state at one store revision.
	loadSp(
		ctx context.Context,
		cid uint64,
		spName string,
	) (*model.SpState, error)
	// flipProvisioned is RW18/MD6; it returns the sides it actually wrote,
	// which is what the "flip applied" record (dnv-worker.md, Log records)
	// names (one per side).
	flipProvisioned(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		sides []model.SideRef,
	) ([]model.SideRef, error)
	// flipCreated is RW19/MD6; it returns the tds it actually wrote.
	flipCreated(
		ctx context.Context,
		cid uint64,
		shard uint32,
		spId uint64,
		cands []model.TdRef,
	) ([]model.TdRef, error)
}

// modelSpOps is the production spOps.
type modelSpOps struct {
	cli *etcdutil.Client
}

func (o *modelSpOps) loadSp(
	ctx context.Context,
	cid uint64,
	spName string,
) (*model.SpState, error) {
	return model.LoadSp(ctx, o.cli, cid, spName)
}

func (o *modelSpOps) flipProvisioned(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	sides []model.SideRef,
) ([]model.SideRef, error) {
	return model.FlipProvisioned(ctx, o.cli, cid, shard, spId, sides)
}

func (o *modelSpOps) flipCreated(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	cands []model.TdRef,
) ([]model.TdRef, error) {
	return model.FlipCreated(ctx, o.cli, cid, shard, spId, cands)
}

// ---------------------------------------------------------------------------
// The fan-out plan (RW14-RW16)
// ---------------------------------------------------------------------------

// sideKey identifies one side child inside its SP (RW14): the sp_id is the
// coordinator's own, so the leg and side ids are the whole key.
type sideKey struct {
	legId  uint64
	sideId uint64
}

// sidePlan is one side child's share of a fan-out: where it is driven, what it
// is to send (RW15) and, when the side is a migration's DESTINATION, the
// bitmap chunks that migration has in etcd (BM1).
type sidePlan struct {
	addr string
	dnId uint64
	ref  model.SideRef
	req  *pb.SyncupSideRequest
	// migrName and migrId are set only for a migration DESTINATION side
	// (BM4: migration chunks go only there); chunks are that migration's
	// indexes from the snapshot (MD3).
	migrName string
	migrId   uint64
	chunks   []model.BmChunk
}

// clonePlan is one clone's bitmap source for the primary cntlr (BM1): chunks
// are the (src_slice_idx, bm_idx) pairs the snapshot found in etcd (MD3).
type clonePlan struct {
	name   string
	id     uint64
	chunks []model.BmChunk
}

// cntlrPlan is one cntlr child's share of a fan-out (RW16).
type cntlrPlan struct {
	addr    string
	cnId    uint64
	cntlrId uint64
	primary bool
	// settling is the loaded record's flag (HL2): the child seeds its
	// monitor's memo from it whenever it takes this plan.
	settling bool
	req      *pb.SyncupCntlrRequest
	// sliceIds are the SP's slice ids, ascending: RW19's condition (3)
	// compares them with a reply's slice_id_to_dm_thin key set.
	sliceIds []uint64
	// clones are the clone bitmaps this child pushes when it is the primary
	// (BM1/BM4); empty on a standby.
	clones []clonePlan
}

// spPlan is one whole fan-out (RW14): every child the SP should have, plus the
// coordinator's own indexes into the loaded state.
type spPlan struct {
	sides  map[sideKey]*sidePlan
	cntlrs map[uint64]*cntlrPlan
	// legSlice maps every leg of the SP — spares included — to the slice its
	// record lives in, which is what lets the coordinator write the PRIMARY's
	// leg rows (HL2).
	legSlice map[uint64]uint64
	// tdRefs are the tds whose created flag is still false, keyed by td_id:
	// the only candidates RW19 may flip.
	tdRefs map[uint64]model.TdRef
	// unresolved counts what the snapshot could not resolve — an endpoint
	// with no DnConf/CnConf, a cntlr whose record is missing — each of which
	// leaves a child idle: no request, so a running one is stopped (RW14). A
	// nonzero count makes the coordinator re-resolve on its ticker.
	unresolved int
}

// ---------------------------------------------------------------------------
// The children
// ---------------------------------------------------------------------------

// sideChild is one running side child of the coordinator.
type sideChild struct {
	handle *revWorker
	driver *sideDriver
	plan   *sidePlan
	// synced is the highest revision the child reported its agent holds
	// (spReport.synced): what RW14's sides-first barrier waits for.
	synced uint64
}

// cntlrChild is one running cntlr child of the coordinator.
type cntlrChild struct {
	handle *revWorker
	driver *cntlrDriver
	plan   *cntlrPlan
	// synced is the highest revision the child reported its agent holds
	// (spReport.cntlrSynced): what RW22's demotion hold waits for.
	synced uint64
}

// legRow is one leg's probe row (architecture.md, Group on-leg layout: meta
// region, data region, health block) as the PRIMARY cntlr reported it (HL2).
// The child cannot write it: the record lives on the leg's SLICE and only the
// coordinator knows which slice that is.
type legRow struct {
	legId   uint64
	obs     healthObs
	resName string
}

// spReport is what an sp child hands back to its coordinator. One reply
// produces at most one report, and the coordinator batches whatever arrived
// within one drain of the channel (RW18: several sides reported within one
// round MAY share one STM).
type spReport struct {
	// provisioned names a side whose request carried provisioned == false and
	// that the agent reports fully zeroed (RW18).
	provisioned *model.SideRef
	// createdTdIds are the td_ids one cntlr reply completed (RW19
	// conditions 1-4); the coordinator keeps those its loaded state still
	// shows created == false.
	createdTdIds []uint64
	// legRows are the primary cntlr's leg probe rows (HL2).
	legRows []legRow
	// cntlrId is the cntlr whose reply produced the report: its legRows are
	// recorded only while the coordinator's latest plan for that cntlr makes
	// it the primary (recordsLegRows).
	cntlrId uint64
	// synced is a side child's first accepted reply at a revision: the
	// revision its agent now holds, which RW14's sides-first barrier waits
	// for. nil on every other report.
	synced *sideSynced
	// cntlrSynced is the same for a cntlr child, the cntlr being cntlrId:
	// the revision its agent now holds, which RW22's demotion hold waits
	// for. 0 on every other report.
	cntlrSynced uint64
}

// sideSynced names one side and the revision its agent reported applied.
type sideSynced struct {
	key      sideKey
	revision uint64
}

// ---------------------------------------------------------------------------
// The coordinator (RW14)
// ---------------------------------------------------------------------------

// spWorker is the sp role's revision worker: a coordinator with one child
// goroutine per side and per cntlr (RW14).
type spWorker struct {
	deps  *deps
	ops   spOps
	shard uint32
	cid   uint64
	spId  uint64
	seed  string

	// desiredCh has capacity one and is overwritten, so the coordinator only
	// ever sees the latest SpRev (RW3).
	desiredCh chan desiredState
	// reportCh carries the children's flip and leg-row reports.
	reportCh chan spReport
	// drainCh is the two drains' own tick (drain.go, clonedrain.go): a
	// committed drain step that left work behind posts one token here, so the
	// next step runs at once instead of at the next cntlr_interval. Capacity
	// one, never blocking — a burst of steps cannot queue passes up.
	drainCh chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	// after is revWorkerParams.after: the coordinator starts once it is
	// closed (RW1).
	after <-chan struct{}

	// Everything below is owned by the run goroutine.
	desired   desiredState
	sides     map[sideKey]*sideChild
	cntlrs    map[uint64]*cntlrChild
	legs      map[uint64]*healthMonitor
	legSlice  map[uint64]uint64
	tdRefs    map[uint64]model.TdRef
	fanWanted bool
	idleCnt   int
	// sideStops and cntlrStops are the graceful stops RW14's diff issued,
	// kept until they have returned (childStops).
	sideStops  childStops[sideKey]
	cntlrStops childStops[uint64]
	// held is what a fan-out holds back: the cntlr half while RW14's
	// sides-first barrier waits, and the side half as well while RW22's
	// demotion hold waits; nil when nothing is held.
	held *cntlrHold
	// confRefusal memoizes the last stored-conf error the fan-out and the
	// reaction pass each refused on, so a steady bad conf costs one Error
	// record rather than one per tick.
	confRefusal spConfRefusal
	// react is the reaction half of the coordinator (AR1-AR10): the model
	// surface of the automatic reactions, the little memo their log records
	// need, and the records AR5 and AR7 decide by, of the last failover and the
	// last replacement of a primary it applied (reaction.go).
	react *reactor
	// verdicts is AR10's own-verdict memo, which the health monitors of the
	// children and of the legs note into and the reaction pass reads.
	verdicts *verdictMemo
}

// newSpWorker starts the sp coordinator (SW2). Its signature is the
// revKind.newWorker contract.
func newSpWorker(p revWorkerParams) revWorkerHandle {
	return startSpWorker(p, &modelSpOps{cli: p.deps.cli})
}

// startSpWorker is newSpWorker with the model surface injected, so the unit
// tests drive a real coordinator without etcd.
func startSpWorker(p revWorkerParams, ops spOps) *spWorker {
	ctx, cancel := context.WithCancel(context.Background())
	w := &spWorker{
		deps:      p.deps,
		ops:       ops,
		shard:     p.shard,
		cid:       p.cid,
		spId:      p.id,
		seed:      p.seed,
		desiredCh: make(chan desiredState, 1),
		reportCh:  make(chan spReport, 64),
		drainCh:   make(chan struct{}, 1),
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		after:     p.after,
		desired:   p.desired,
		sides:     make(map[sideKey]*sideChild),
		cntlrs:    make(map[uint64]*cntlrChild),
		legs:      make(map[uint64]*healthMonitor),
		legSlice:  make(map[uint64]uint64),
		tdRefs:    make(map[uint64]model.TdRef),
		react:     newReactor(&modelReactionOps{cli: p.deps.cli}),
		verdicts:  newVerdictMemo(),
	}
	go w.run()
	return w
}

// update delivers a new desired state, overwriting an undelivered one (RW3).
func (w *spWorker) update(d desiredState) {
	select {
	case <-w.desiredCh:
	default:
	}
	select {
	case w.desiredCh <- d:
	default:
	}
}

// stop cancels the coordinator and joins it (RW11, SW5). Its children are
// stopped in parallel on the way out, and joined with the stops its diff left
// running, so a stop costs one in-flight call, not one per child.
func (w *spWorker) stop() {
	w.cancel()
	<-w.done
}

// run is the coordinator's loop (RW14, RW20). Unlike a per-object loop it
// issues no RPC of its own: it fans out on every desired change, applies the
// children's reports — a demoted cntlr's ends RW22's demotion hold, and the
// sides' release the cntlr half a fan-out holds back, as does each wait's
// own timer — and ticks once per cntlr_interval for the idle-child
// re-resolution and the reaction pass (AR1).
func (w *spWorker) run() {
	defer close(w.done)
	if !awaitPredecessor(w.ctx, w.after) {
		return
	}
	slog.InfoContext(w.ctx, msgRevisionWorkerStarted, w.attrs()...)
	defer func() {
		if w.held != nil {
			w.held.timer.stop()
		}
		w.stopChildren()
		// The stop ctx is done by now; the record still has to come out.
		slog.InfoContext(
			context.WithoutCancel(w.ctx),
			msgRevisionWorkerStopped,
			w.attrs()...,
		)
	}()
	w.fanOut()
	period := w.tickPeriod()
	ticker := w.deps.clk.newTicker(period)
	defer func() { ticker.stop() }()
	for {
		select {
		case <-w.ctx.Done():
			return
		case next := <-w.desiredCh:
			if next == w.desired {
				// RW3: a put that changes neither revision nor sp_name.
				continue
			}
			w.desired = next
			w.fanOut()
		case rep := <-w.reportCh:
			w.handleReports(rep)
		case <-w.heldC():
			// RW22: the demotion hold's bound — the sides go without
			// waiting for a demoted cntlr that has not reported. RW14: the
			// sides-first barrier's bound — one cntlr_interval after the
			// sides were handed the fan-out, the cntlrs go without waiting
			// for the sides that have not reported.
			w.expireHold(newTraceCtx(w.ctx, w.seed))
		case <-w.drainCh:
			// The drains' self-tick (SPD6, CLD12): one more pass, which
			// re-derives the next step from what the last one left (SPD8 for
			// the SP, CLD7 for a clone).
			w.reactionPass(newTraceCtx(w.ctx, w.seed))
		case <-ticker.C:
			w.tick()
			if next := w.tickPeriod(); next != period {
				// RW9: the interval is re-read from the cache, so a
				// ClusterConf change reaches the pass without a restart.
				ticker.stop()
				period = next
				ticker = w.deps.clk.newTicker(period)
			}
		}
	}
}

// tick is the coordinator's own cadence (RW14, RW20/AR1): re-resolve the
// children an absent DnConf/CnConf left idle, retry a failed load, then run
// one reaction pass.
func (w *spWorker) tick() {
	if w.fanWanted || w.idleCnt > 0 {
		w.fanOut()
	}
	w.reactionPass(newTraceCtx(w.ctx, w.seed))
}

// tickPeriod is the pass cadence: the cluster's cntlr_interval (RW9, AR1). A
// cluster missing from the cache does not stop the coordinator — its children
// idle and log that themselves (RW9) — it only leaves the pass at the default
// period.
func (w *spWorker) tickPeriod() time.Duration {
	cc, ok := w.deps.conf.get(w.cid)
	// An unusable conf falls back to the same period, and logs NOTHING: this
	// is a cadence, not a value anything is computed with, and a coordinator
	// that stopped ticking would stop retrying the fan-out and the reaction
	// pass forever. Those two are where the refusal is recorded, once.
	if !ok || model.ValidateClusterConf(cc) != nil {
		return common.DefaultHealthCheckInterval * time.Second
	}
	return roundPeriod(cc.GetHealthCheckConf().GetCntlrInterval())
}

// attrs are the "revision worker started"/"stopped" attributes (dnv-worker.md,
// Log records) of the coordinator. Its children add side_pointer /
// cntlr_pointer of their own.
func (w *spWorker) attrs() []any {
	return []any{
		slog.String("role", common.WorkerRoleSp),
		slog.String("shard", shardCode(w.shard)),
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("id", w.spId),
	}
}

// fanOut is RW14: load the SP, build every request once, diff the children.
func (w *spWorker) fanOut() {
	ctx := newTraceCtx(w.ctx, w.seed)
	w.fanWanted = false
	state, err := w.ops.loadSp(ctx, w.cid, w.desired.handle)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			// RW14: the SP is being deleted. The children keep driving what
			// they have until the SpRev delete arrives (SW3) — the agents
			// tear their resources down through the DN's and CN's pointer
			// lists (architecture.md, Common agent rules), so the sp role has
			// nothing left to send.
			slog.InfoContext(ctx, msgSpDeleting,
				slog.Uint64("cluster_id", w.cid),
				slog.Uint64("sp_id", w.spId),
				slog.String("sp_name", w.desired.handle),
			)
			return
		}
		// Transient: retried on the next tick (RW12, no backoff).
		w.fanWanted = true
		slog.InfoContext(ctx, msgSpLoadFailed,
			slog.Uint64("cluster_id", w.cid),
			slog.Uint64("sp_id", w.spId),
			slog.String("sp_name", w.desired.handle),
			slog.String("error", err.Error()),
		)
		return
	}
	if state.SpRevision > w.desired.revision {
		// RW14: the SP has moved past the revision this coordinator was
		// delivered — a tick's fan-out ran before the watch delivered a bump,
		// say, or a new owner started from its parent's older value — and
		// every request is labelled with the delivered one (RW15/RW16). A plan
		// built now would pair that label with a newer role: a promotion
		// labelled with the revision the agent applied as a standby restarts
		// the child, whose first Check is answered at that revision with the
		// standby shape — a reply HL2 would take for the primary's and settle
		// on, and one RW4 step 5 never re-syncs. Nothing is built, and no
		// child is started, stopped or updated; the bump's own delivery is a
		// desired change, which re-enters the fan-out, so no retry is armed.
		return
	}
	if len(state.Missing) > 0 {
		// MD3: the load succeeded and the coordinator drives what it can,
		// but every one of these keys is a sub-object BOTH agents converge on
		// by tearing its resources down — the only record that names them is
		// this one.
		slog.InfoContext(ctx, msgSpSubObjectMissing,
			slog.Uint64("cluster_id", w.cid),
			slog.Uint64("sp_id", w.spId),
			slog.String("sp_name", w.desired.handle),
			slog.Any("keys", state.Missing),
		)
	}
	if err := model.ValidateBdevConf(state.Conf.GetBdevConf()); err != nil {
		// architecture.md, Common validation: the SP's stored geometry is what
		// every side and cntlr request is built from. dm_raid0_conf.stripe_size
		// is used as a value by no worker computation, and
		// redund_md_raid1.bitmap_chunk_block_cnt only inside model.GrowSlice's
		// geometry (architecture.md, Group on-leg layout: meta region, data
		// region, health block); this gate only checks both for zero, and
		// otherwise both travel verbatim
		// inside the bdev_conf buildCntlrPlans forwards, so this is the one
		// place the worker can refuse to hand the cn agent a geometry nobody
		// chose.
		//
		// Refusing here builds no request, starts no child and updates no
		// running one: buildPlan and applyPlan are simply not reached, and
		// neither is any STM. Children ALREADY running keep driving the plan
		// built from the last good conf; nothing is torn down, because a conf
		// that cannot be read is not a reason to stop serving IO.
		//
		// fanWanted is set for the same reason msgSpLoadFailed sets it: tick()
		// only re-enters fanOut when it is, and applyPlan — the only other
		// thing that would arm a retry, through idleCnt — was not reached. A
		// repaired conf does arrive as a desired change, but relying on that
		// alone would leave the coordinator quiet after an operator fixed the
		// SP by any other route. The memo keeps the retry to one record.
		w.fanWanted = true
		w.refuseSpConf(ctx, err)
		return
	}
	w.confRefusal.bdev = ""
	plan := w.buildPlan(ctx, state)
	w.applyPlan(ctx, plan)
}

// refuseSpConf records fanOut's refusal, once per distinct error.
func (w *spWorker) refuseSpConf(ctx context.Context, err error) {
	if w.confRefusal.bdev == err.Error() {
		return
	}
	w.confRefusal.bdev = err.Error()
	slog.ErrorContext(ctx, msgInvalidStoredConf,
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("sp_id", w.spId),
		slog.String("sp_name", w.desired.handle),
		slog.String("error", err.Error()),
	)
}

// buildPlan turns one loaded SP into every request the fan-out sends
// (RW15/RW16). It resolves each side's dn_id through SpState.DnByAddr and each
// cntlr's cn_id through CnByAddr (architecture.md, sp role): those two maps are
// read at the same store revision as the SP itself, so no request can mix two
// views.
func (w *spWorker) buildPlan(
	ctx context.Context,
	state *model.SpState,
) *spPlan {
	conf := state.Conf
	plan := &spPlan{
		sides:    make(map[sideKey]*sidePlan),
		cntlrs:   make(map[uint64]*cntlrPlan),
		legSlice: make(map[uint64]uint64),
		tdRefs:   make(map[uint64]model.TdRef),
	}
	for i, td := range state.Tds {
		if td.GetCreated() {
			continue
		}
		plan.tdRefs[td.GetTdId()] = model.TdRef{
			Name: state.TdNames[i],
			TdId: td.GetTdId(),
		}
	}

	// The cntlr side of the plan first: the side requests need the primary's
	// cn_id and the standby list (RW15).
	cnIds := make(map[uint64]uint64, len(conf.GetCntlrIdList()))
	var primaryId uint64
	var primaryCnId uint64
	hasPrimary := false
	// RW15's primary_cn_id / standby_id_list must name EVERY cntlr of the SP.
	// A cntlr this snapshot cannot resolve — its record is missing (MD3) or
	// its CnConf is absent (RW14) — would silently shrink that set, and the
	// dn agent converges by TEARING DOWN every CN it is not told about, so an
	// incomplete set may not be sent at all (see below).
	cntlrsResolved := true
	for _, cntlrId := range conf.GetCntlrIdList() {
		cntlr, ok := state.Cntlrs[cntlrId]
		if !ok {
			// MD3: the key is listed but absent. fanOut has already named it
			// in the "sp sub-object missing" record; here it only counts, so
			// that the cntlr_interval ticker re-resolves (RW14).
			plan.unresolved++
			cntlrsResolved = false
			continue
		}
		cn, ok := state.CnByAddr[cntlr.GetAddrPort()]
		if !ok {
			plan.unresolved++
			cntlrsResolved = false
			w.logUnresolved(ctx, cntlr.GetAddrPort(),
				slog.Uint64("cntlr_id", cntlrId),
			)
			continue
		}
		cnIds[cntlrId] = cn.GetCnId()
		if cntlr.GetPrimary() {
			primaryId, primaryCnId, hasPrimary = cntlrId, cn.GetCnId(), true
		}
	}
	// RW15: every OTHER cntlr is a standby of the side — disabled ones
	// included, because a disabled cntlr keeps its standby shape
	// (cnagent.md CN9) — in SpConf.cntlr_id_list order, so an unchanged state
	// produces a byte-identical request.
	standby := make([]uint64, 0, len(conf.GetCntlrIdList()))
	for _, cntlrId := range conf.GetCntlrIdList() {
		if hasPrimary && cntlrId == primaryId {
			continue
		}
		cnId, ok := cnIds[cntlrId]
		if !ok {
			continue
		}
		standby = append(standby, cnId)
	}

	// SPD7: an SP with NO cntlr at all is a shape creation can never produce —
	// CreateStoragePool always makes at least one — but the sp drain's D1 makes
	// it for every latched SP, between D1 and D3. buildCntlrPlans already
	// yields an empty map for it; the SIDE half needs its own arm, because
	// RW15's "primary_cn_id = 0 if none" would otherwise reach the dn agent as
	// a real CN id: agent/dnagent's cnIdsOf puts the primary at the head of the
	// export list unconditionally, so an empty cntlr set would have every DN of
	// the doomed SP BUILD a dm-error, a dm-linear, a subsystem and a namespace
	// for the CN numbered 0 — new garbage, created while the SP is being torn
	// down, and outliving the drain if a DN is slow to sweep it.
	//
	// The arm is "no cntlr at all", not "no primary": an SP that momentarily
	// has cntlrs and no primary keeps RW15's documented behaviour exactly
	// (TestSpSideRequestNoPrimary pins it), because there the zero is one entry
	// in a list that still names real CNs.
	//
	// Leaving the sides idle costs the drain nothing. After D1 the sides are
	// retired through the DN pointer lists as D2 empties them (SPD7), not
	// through these children.
	sideReason := ""
	switch {
	case !cntlrsResolved:
		// RW15 + RW14: rather than shipping half a side_conf — an unresolved
		// PRIMARY would send primary_cn_id = 0 and drop the real primary from
		// standby_id_list, and every DN of the SP would tear that CN's
		// dm-error, dm-linear, subsystem and namespace down — every SIDE
		// child is left idle, exactly as addMigrConf leaves a migration whose
		// peer cannot be resolved. plan.unresolved is already nonzero, so the
		// cntlr_interval ticker re-resolves (RW14). The CNTLR children are
		// unaffected: RW16's request carries no peer's cn_id, so an
		// unresolved endpoint's effect stays confined to its own child.
		sideReason = "cntlr unresolved"
	case len(conf.GetCntlrIdList()) == 0:
		// SPD7. Nothing is unresolved, so no re-resolution is armed: the next
		// drain step's SpRev bump rebuilds the plan.
		sideReason = "no cntlr"
	}
	if sideReason != "" {
		slog.InfoContext(ctx, msgSpSidesIdle,
			slog.Uint64("cluster_id", w.cid),
			slog.Uint64("sp_id", w.spId),
			slog.String("reason", sideReason),
		)
	}
	w.buildSidePlans(ctx, plan, state, primaryCnId, standby, sideReason == "")
	w.buildCntlrPlans(ctx, plan, state, cnIds)
	return plan
}

// buildSidePlans walks every side of the SP — both groups of every slice, both
// the active legs and the SPARE legs (RW14; architecture.md, Spare legs) — and
// builds its request.
//
// withRequests is false while the SP's cntlr set is unresolved: the walk then
// only indexes the legs, which is what lets the coordinator keep placing the
// PRIMARY's leg rows on their slice (HL2) while every side child is idle.
func (w *spWorker) buildSidePlans(
	ctx context.Context,
	plan *spPlan,
	state *model.SpState,
	primaryCnId uint64,
	standby []uint64,
	withRequests bool,
) {
	conf := state.Conf
	spId := conf.GetSpId()
	for _, sliceId := range conf.GetSliceIdList() {
		slice, ok := state.Slices[sliceId]
		if !ok {
			continue
		}
		grpLists := [][]*pb.Group{
			slice.GetMetaGrpList(),
			slice.GetDataGrpList(),
		}
		for _, grpList := range grpLists {
			for _, grp := range grpList {
				legLists := [][]*pb.Leg{
					grp.GetLegList(),
					grp.GetSpareLegList(),
				}
				for _, legList := range legLists {
					for _, leg := range legList {
						plan.legSlice[leg.GetLegId()] = sliceId
						if !withRequests {
							continue
						}
						for _, side := range leg.GetSideList() {
							w.buildSidePlan(
								ctx, plan, state, sliceId, grp, leg, side,
								primaryCnId, standby, spId,
							)
						}
					}
				}
			}
		}
	}
}

// buildSidePlan is RW15 for one side.
func (w *spWorker) buildSidePlan(
	ctx context.Context,
	plan *spPlan,
	state *model.SpState,
	sliceId uint64,
	grp *pb.Group,
	leg *pb.Leg,
	side *pb.Side,
	primaryCnId uint64,
	standby []uint64,
	spId uint64,
) {
	key := sideKey{legId: leg.GetLegId(), sideId: side.GetSideId()}
	dn, ok := state.DnByAddr[side.GetAddrPort()]
	if !ok {
		plan.unresolved++
		w.logUnresolved(ctx, side.GetAddrPort(),
			slog.Uint64("leg_id", key.legId),
			slog.Uint64("side_id", key.sideId),
		)
		return
	}
	req := &pb.SyncupSideRequest{
		ClusterId: w.cid,
		DnId:      dn.GetDnId(),
		SidePointer: &pb.SidePointer{
			SpId:   spId,
			LegId:  key.legId,
			SideId: key.sideId,
		},
		Revision: w.desired.revision,
		SideConf: &pb.SyncupSideRequest_SideConf{
			ExtCnt:        grp.GetExtCnt(),
			CntlidSlot:    side.GetCntlidSlot(),
			PrimaryCnId:   primaryCnId,
			StandbyIdList: append([]uint64(nil), standby...),
			SpLevel:       state.Conf.GetSpLevel(),
			Provisioned:   side.GetProvisioned(),
		},
	}
	out := &sidePlan{
		addr: side.GetAddrPort(),
		dnId: dn.GetDnId(),
		ref: model.SideRef{
			SliceId: sliceId,
			LegId:   key.legId,
			SideId:  key.sideId,
		},
		req: req,
	}
	if !w.addMigrConf(ctx, plan, state, grp, leg, side, out) {
		return
	}
	// Every child marshals its own request on its own goroutine, so no two
	// of them may share a sub-message of the loaded state: a request is
	// cloned before it leaves the coordinator.
	out.req = proto.Clone(out.req).(*pb.SyncupSideRequest)
	plan.sides[key] = out
}

// addMigrConf adds RW15's migr_src_conf / migr_dst_conf when a Migration of
// the SP names this side. Only a leg with TWO sides can carry a migration: one
// side is the source, the other the destination (architecture.md, Migration).
// It reports whether the request is complete — a migration whose peer side's DN
// cannot be resolved leaves this child idle rather than sending half a role.
func (w *spWorker) addMigrConf(
	ctx context.Context,
	plan *spPlan,
	state *model.SpState,
	grp *pb.Group,
	leg *pb.Leg,
	side *pb.Side,
	out *sidePlan,
) bool {
	if len(leg.GetSideList()) != 2 {
		return true
	}
	conf := state.Conf
	for _, migrName := range conf.GetMigrNameList() {
		migr, ok := state.Migrs[migrName]
		if !ok {
			continue
		}
		if migr.GetSrcSideId() == side.GetSideId() {
			dst := sideOfLeg(leg, migr.GetDstSideId())
			if dst == nil {
				continue
			}
			dstDn, ok := state.DnByAddr[dst.GetAddrPort()]
			if !ok {
				plan.unresolved++
				w.logUnresolved(ctx, dst.GetAddrPort(),
					slog.Uint64("leg_id", leg.GetLegId()),
					slog.Uint64("side_id", dst.GetSideId()),
				)
				return false
			}
			out.req.MigrSrcConf = &pb.SyncupSideRequest_MigrSrcConf{
				MigrId:         migr.GetMigrId(),
				DstSideId:      dst.GetSideId(),
				DstDnId:        dstDn.GetDnId(),
				DstProvisioned: dst.GetProvisioned(),
			}
		}
		if migr.GetDstSideId() == side.GetSideId() {
			src := sideOfLeg(leg, migr.GetSrcSideId())
			if src == nil {
				continue
			}
			srcDn, ok := state.DnByAddr[src.GetAddrPort()]
			if !ok {
				plan.unresolved++
				w.logUnresolved(ctx, src.GetAddrPort(),
					slog.Uint64("leg_id", leg.GetLegId()),
					slog.Uint64("side_id", src.GetSideId()),
				)
				return false
			}
			out.req.MigrDstConf = &pb.SyncupSideRequest_MigrDstConf{
				MigrId:        migr.GetMigrId(),
				SrcSideId:     src.GetSideId(),
				SrcDnId:       srcDn.GetDnId(),
				SrcNvmeTrConf: src.GetNvmeTrConf(),
				BlockSize:     dataBlockSize(conf.GetBdevConf()),
				MetaBlocks:    grp.GetMetaBlocks(),
				DmCloneConf:   migrCloneConf(migr.GetDmCloneConf()),
				BmCnt:         migr.GetBmCnt(),
			}
			// BM1/BM4: only the destination side's DN receives this
			// migration's chunks.
			out.migrName = migrName
			out.migrId = migr.GetMigrId()
			out.chunks = state.MigrBmIdx[migrName]
		}
	}
	return true
}

// buildCntlrPlans is RW16: every cntlr of the SP receives the full state
// (architecture.md, sp role), in SpConf's list orders so the requests are
// deterministic.
func (w *spWorker) buildCntlrPlans(
	ctx context.Context,
	plan *spPlan,
	state *model.SpState,
	cnIds map[uint64]uint64,
) {
	conf := state.Conf
	spId := conf.GetSpId()
	// RW19 condition (3) compares a reply's slice_id_to_dm_thin key set with
	// "the SP's slice ids" — SpConf.slice_id_list, NOT the subset whose Slice
	// records happened to load. A slice whose key is missing (MD3) is absent
	// from id_to_slice, so the cn agent never builds its dm-thin and the td
	// stays uncreated: shrinking the comparison set instead would flip a td
	// `created` with a slice unaccounted for.
	sliceIds := append([]uint64(nil), conf.GetSliceIdList()...)
	idToSlice := make(map[string]*pb.Slice, len(conf.GetSliceIdList()))
	for _, sliceId := range conf.GetSliceIdList() {
		slice, ok := state.Slices[sliceId]
		if !ok {
			continue
		}
		// The key the cn agent reads (RW16; architecture.md, `service
		// ControllerNodeAgent`).
		idToSlice[fmt.Sprintf(common.IdKeyFmt, sliceId)] = slice
	}
	clones := make([]*pb.Clone, 0, len(conf.GetCloneNameList()))
	clonePlans := make([]clonePlan, 0, len(conf.GetCloneNameList()))
	for _, cloneName := range conf.GetCloneNameList() {
		clone, ok := state.Clones[cloneName]
		if !ok {
			continue
		}
		if clone.GetDeleting() {
			// CLD5: a LATCHED clone is fully absent from every cntlr's
			// clone_list and from the primary's chunk-push plans, at every
			// sp_level. Full absence is deliberately distinct from level
			// suppression: a level-suppressed clone (SP_LEVEL_NO_CLONE) keeps
			// its local chunk files for a later rebuild, while a deleting one
			// must lose them — and the cn agent's sweep (cnagent.md CN21)
			// drops them precisely when the clone id is absent from the plan.
			// The primary's child is handed this exclusion when the RW14
			// sides-first hold the latch's fan-out lands in releases the
			// cntlrs, and no push of the clone is submitted from then on.
			// Until then it drives the pre-latch plan and can still push
			// from it while a drain batch deletes chunk keys; a push that
			// finds its chunk gone ends there, harmlessly (CLD5, BM6).
			continue
		}
		clones = append(clones, clone)
		clonePlans = append(clonePlans, clonePlan{
			name:   cloneName,
			id:     clone.GetCloneId(),
			chunks: state.CloneBmIdx[cloneName],
		})
	}
	xfers := make([]*pb.Transfer, 0, len(conf.GetXferNameList()))
	for _, xferName := range conf.GetXferNameList() {
		if xfer, ok := state.Xfers[xferName]; ok {
			xfers = append(xfers, xfer)
		}
	}
	migrs := make([]*pb.Migration, 0, len(conf.GetMigrNameList()))
	for _, migrName := range conf.GetMigrNameList() {
		if migr, ok := state.Migrs[migrName]; ok {
			migrs = append(migrs, migr)
		}
	}
	for _, cntlrId := range conf.GetCntlrIdList() {
		cntlr, ok := state.Cntlrs[cntlrId]
		if !ok {
			continue
		}
		cnId, ok := cnIds[cntlrId]
		if !ok {
			// Already counted and logged in buildPlan.
			continue
		}
		out := &cntlrPlan{
			addr:     cntlr.GetAddrPort(),
			cnId:     cnId,
			cntlrId:  cntlrId,
			primary:  cntlr.GetPrimary(),
			settling: cntlr.GetSettling(),
			req: &pb.SyncupCntlrRequest{
				ClusterId: w.cid,
				CnId:      cnId,
				CntlrPointer: &pb.CntlrPointer{
					SpId:    spId,
					CntlrId: cntlrId,
				},
				Revision:       w.desired.revision,
				BdevConf:       conf.GetBdevConf(),
				SpLevel:        conf.GetSpLevel(),
				Cntlr:          cntlr,
				IdToSlice:      idToSlice,
				TdList:         state.Tds,
				NqnToSubsystem: state.Subsystems,
				CloneList:      clones,
				XferList:       xfers,
				MigrList:       migrs,
			},
			sliceIds: sliceIds,
		}
		// As for the sides: the cntlr requests of one SP share the whole
		// loaded state, so each child gets its own copy to marshal.
		out.req = proto.Clone(out.req).(*pb.SyncupCntlrRequest)
		if out.primary {
			// BM4: clone chunks go only to the CN hosting the primary.
			out.clones = clonePlans
		}
		plan.cntlrs[cntlrId] = out
	}
}

// cntlrHold is what a fan-out holds back. Under RW14's sides-first barrier it
// is the cntlr half: the plans the cntlr children are to be handed, the
// revision every side child must report applied first, and the timer that
// bounds the wait. Under RW22's demotion hold, which comes first, it holds
// the side half as well, until every cntlr the fan-out demoted reports its
// demotion applied or the demotion hold's own timer fires.
type cntlrHold struct {
	revision uint64
	plans    map[uint64]*cntlrPlan
	timer    *timerHandle
	// demoted maps each cntlr RW22's demotion hold waits for to the revision
	// of its demotion; nil when that hold is over, or never began.
	demoted map[uint64]uint64
	// sides are the side plans RW22's demotion hold keeps back.
	sides map[sideKey]*sidePlan
}

// holdCntlrs is RW14's sides-first barrier: the cntlr children are handed the
// fan-out's requests only once every side child has reported its revision
// applied, or one cntlr_interval after the barrier began — when the sides were
// handed the fan-out, at the fan-out or at the end of RW22's demotion hold —
// whichever comes first. A fan-out that finds a hold pending replaces its plans
// and keeps its deadline, so a stream of bumps cannot hold the cntlrs past the
// bound. The cntlr children a plan no longer names were stopped by the diff
// already.
func (w *spWorker) holdCntlrs(plans map[uint64]*cntlrPlan) {
	if w.held == nil {
		w.held = &cntlrHold{timer: w.deps.clk.newTimer(w.tickPeriod())}
	}
	w.held.revision = w.desired.revision
	w.held.plans = plans
}

// demoting reports whether RW22's demotion hold is keeping the fan-out back.
func (w *spWorker) demoting() bool {
	return w.held != nil && w.held.demoted != nil
}

// demotions is RW22's test, each demoted cntlr mapped to the revision of its
// demotion: a cntlr whose child was last handed a primary plan and whose plan
// in this fan-out is a standby's. A cntlr with no running child has no agent
// this coordinator told it was the primary, and a cntlr replacement deletes
// the old cntlr, so neither is a demotion.
func (w *spWorker) demotions(plan *spPlan) map[uint64]uint64 {
	var demoted map[uint64]uint64
	for cntlrId, next := range plan.cntlrs {
		child, ok := w.cntlrs[cntlrId]
		if !ok || !child.plan.primary || next.primary {
			continue
		}
		if demoted == nil {
			demoted = make(map[uint64]uint64)
		}
		demoted[cntlrId] = next.req.GetRevision()
	}
	return demoted
}

// holdDemotion is RW22's demotion hold: every cntlr the fan-out demotes is
// handed its standby plan at once, so that its agent moves its namespaces to
// ANA inaccessible before any side fences it, and the side plans and the
// other cntlr plans wait until each demoted cntlr reports its demotion
// applied, or common.DemotionHoldTimeout passes. The worker judges no
// reachability: an agent that never answers simply uses up the wait, which is
// what a host following the discovery log needs to drop the path the
// failover withdrew.
//
// A fan-out during the demotion hold replaces what is held and hands a demoted
// cntlr its newest plan at once. A further demotion joins the hold and re-arms
// its timer, so every demoted agent gets the whole wait before its sides are
// told; demotions come only from failovers, so this cannot hold the sides for
// long. A demoted cntlr that a newer plan makes the primary again leaves the
// demotion hold and is held like any other cntlr. One the plan no longer names
// — a cntlr replacement that deleted it — stays in the demotion hold: its agent
// may still export its namespaces optimized, and with its child gone only the
// timer ends the wait the hosts need. A demotion found while RW14's barrier
// holds an earlier fan-out supersedes that barrier: its timer stops, and the
// barrier begins again when the demotion hold ends.
func (w *spWorker) holdDemotion(plan *spPlan, fresh map[uint64]uint64) {
	if !w.demoting() {
		if w.held != nil {
			w.held.timer.stop()
		}
		w.held = &cntlrHold{demoted: make(map[uint64]uint64)}
	}
	held := w.held
	if len(fresh) > 0 || held.timer == nil {
		if held.timer != nil {
			held.timer.stop()
		}
		held.timer = w.deps.clk.newTimer(
			common.DemotionHoldTimeout * time.Second,
		)
	}
	for cntlrId, revision := range fresh {
		held.demoted[cntlrId] = revision
	}
	held.revision = w.desired.revision
	held.sides = plan.sides
	held.plans = make(map[uint64]*cntlrPlan, len(plan.cntlrs))
	for cntlrId, next := range plan.cntlrs {
		if _, ok := held.demoted[cntlrId]; ok {
			if !next.primary {
				w.handCntlr(cntlrId, next)
				continue
			}
			delete(held.demoted, cntlrId)
		}
		held.plans[cntlrId] = next
	}
}

// heldC is the timer of whatever is held, or nil — a channel that never
// fires — when nothing is.
func (w *spWorker) heldC() <-chan time.Time {
	if w.held == nil {
		return nil
	}
	return w.held.timer.C
}

// releaseHeld moves a held fan-out on as far as the reports allow: RW22's
// demotion hold ends once every demoted cntlr reports its demotion applied,
// and RW14's barrier then releases once every side reports the revision. A
// demoted cntlr whose child is gone reports nothing more, so only the
// demotion hold's timer ends the wait for it.
func (w *spWorker) releaseHeld(ctx context.Context) {
	if w.demoting() {
		for cntlrId, revision := range w.held.demoted {
			child, ok := w.cntlrs[cntlrId]
			if !ok || child.synced < revision {
				return
			}
		}
		w.endDemotion(ctx, false)
	}
	w.releaseSynced()
}

// endDemotion ends RW22's demotion hold: the held side plans are handed out,
// and RW14's barrier takes over the held cntlr plans with a bound of one
// cntlr_interval from now. When the demotion hold's timer ended it, the record
// names the demoted cntlrs that had not reported.
func (w *spWorker) endDemotion(ctx context.Context, expired bool) {
	held := w.held
	if expired {
		var unsynced []uint64
		for cntlrId, revision := range held.demoted {
			child, ok := w.cntlrs[cntlrId]
			if !ok || child.synced < revision {
				unsynced = append(unsynced, cntlrId)
			}
		}
		slog.InfoContext(ctx, msgSpDemotionUnsynced,
			slog.Uint64("cluster_id", w.cid),
			slog.Uint64("sp_id", w.spId),
			slog.Uint64("revision", held.revision),
			slog.Any("cntlr_ids", sortedIds(unsynced)),
		)
	}
	held.timer.stop()
	held.timer = w.deps.clk.newTimer(w.tickPeriod())
	sides := held.sides
	held.demoted = nil
	held.sides = nil
	w.handSides(sides)
}

// releaseSynced releases the cntlr half RW14's barrier holds once every side
// child reports the held revision applied. RW22's demotion hold releases
// nothing here: its sides have not been handed the revision yet.
func (w *spWorker) releaseSynced() {
	if w.held == nil || w.demoting() {
		return
	}
	for _, child := range w.sides {
		if child.synced < w.held.revision {
			return
		}
	}
	w.releaseCntlrs()
}

// expireHold is the bound of whatever is held. RW22's demotion hold ends and
// RW14's barrier begins; RW14's barrier releases the cntlrs one cntlr_interval
// after it began, whatever the sides reported, and the record counts the side
// children that had not reported the revision.
func (w *spWorker) expireHold(ctx context.Context) {
	if w.held == nil {
		return
	}
	if w.demoting() {
		w.endDemotion(ctx, true)
		w.releaseSynced()
		return
	}
	unsynced := 0
	for _, child := range w.sides {
		if child.synced < w.held.revision {
			unsynced++
		}
	}
	slog.InfoContext(ctx, msgSpSidesUnsynced,
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("sp_id", w.spId),
		slog.Uint64("revision", w.held.revision),
		slog.Int("side_cnt", unsynced),
	)
	w.releaseCntlrs()
}

// releaseCntlrs is the cntlr half of RW14's child diff, which the barrier
// deferred: every running cntlr child receives its new request as a desired
// change (RW6), and every new one is started.
func (w *spWorker) releaseCntlrs() {
	held := w.held
	w.held = nil
	held.timer.stop()
	for cntlrId, next := range held.plans {
		w.handCntlr(cntlrId, next)
	}
}

// handCntlr hands one cntlr its plan: a running child receives it as a
// desired change (RW6, immediate syncup), and a new one is started with it.
func (w *spWorker) handCntlr(cntlrId uint64, next *cntlrPlan) {
	if child, ok := w.cntlrs[cntlrId]; ok {
		child.plan = next
		child.driver.install(next)
		child.handle.update(desiredState{
			revision: next.req.GetRevision(),
			handle:   next.addr,
		})
		return
	}
	w.cntlrs[cntlrId] = w.startCntlrChild(next)
}

// handSides hands every side its plan the same way: a running child receives
// it as a desired change, and a new one is started with it.
func (w *spWorker) handSides(sides map[sideKey]*sidePlan) {
	for key, next := range sides {
		if child, ok := w.sides[key]; ok {
			child.plan = next
			child.driver.install(next)
			child.handle.update(desiredState{
				revision: next.req.GetRevision(),
				handle:   next.addr,
			})
			continue
		}
		w.sides[key] = w.startSideChild(key, next)
	}
}

// recordsLegRows is HL2's "only the primary's leg rows are recorded", judged
// by the plan the coordinator last decided for the cntlr: the held one while
// a hold keeps the cntlrs, else the one its child was handed. A demoted
// primary's agent runs the primary shape until it applies its demotion
// (RW22), possibly over legs the sides have already reloaded onto dm-error,
// so its rows describe that fence, not the legs; a demoted cntlr is in no
// held plan and its child holds the standby plan, so its rows are dropped.
// The reports travel over a buffered channel, so rows a cntlr built as the
// primary can be read after it was handed the standby plan: they are dropped
// the same way.
func (w *spWorker) recordsLegRows(cntlrId uint64) bool {
	if w.held != nil {
		next, ok := w.held.plans[cntlrId]
		return ok && next.primary
	}
	child, ok := w.cntlrs[cntlrId]
	return ok && child.plan.primary
}

// applyPlan is RW14's child diff: new => start; gone => stop (RW11), off the
// coordinator's goroutine (childStops); a child whose ENDPOINT changed is
// restarted at the new one, the new child waiting for the old one's stop
// (RW1); every remaining child receives its new request as a desired change
// (RW6, immediate syncup) — a side at once, a cntlr once the sides hold the
// revision (holdCntlrs). A fan-out that demotes a primary, or that arrives
// while RW22's demotion hold is pending, hands the demoted cntlr its request
// first and holds the sides as well (holdDemotion).
//
// A re-resolution tick can also change a request without changing the SpRev
// revision — a cntlr whose CnConf finally appeared changes every side's
// standby list. The generic loop coalesces an unchanged desired state away
// (RW3), so such a child is restarted instead: rare, and the alternative
// would be a child driving a stale request until the next bump.
func (w *spWorker) applyPlan(ctx context.Context, plan *spPlan) {
	for key, child := range w.sides {
		next, ok := plan.sides[key]
		if ok && keepChild(
			child.plan.addr, next.addr, child.plan.req, next.req,
		) {
			continue
		}
		delete(w.sides, key)
		w.sideStops.stop(key, w.sideStopper(child))
	}
	// The demotions are read before the diff, which may restart a child: the
	// agent of a restarted cntlr was told it was the primary all the same, so
	// its new child is handed the demotion at once.
	fresh := w.demotions(plan)
	for cntlrId, child := range w.cntlrs {
		next, ok := plan.cntlrs[cntlrId]
		if ok && keepChild(
			child.plan.addr, next.addr, child.plan.req, next.req,
		) {
			continue
		}
		delete(w.cntlrs, cntlrId)
		w.cntlrStops.stop(cntlrId, w.cntlrStopper(child))
	}

	if len(fresh) > 0 || w.demoting() {
		w.holdDemotion(plan, fresh)
	} else {
		// Held before any side is handed its request, so the bound runs from
		// the fan-out and no side's Syncup* is ahead of the hold.
		w.holdCntlrs(plan.cntlrs)
		w.handSides(plan.sides)
	}
	// A re-fan whose sides all hold the revision already — a re-resolution
	// tick's, say — releases the cntlrs here and now, and a demotion hold
	// left with no cntlr to wait for ends here.
	w.releaseHeld(ctx)

	w.legSlice = plan.legSlice
	w.tdRefs = plan.tdRefs
	for legId := range w.legs {
		if _, ok := w.legSlice[legId]; !ok {
			delete(w.legs, legId)
		}
	}
	w.idleCnt = plan.unresolved
}

// startSideChild starts one side child (RW14): an ordinary revision worker
// (RW1-RW12) with the side driver, which waits for the stop of the side's
// previous child if that is still running (RW1).
func (w *spWorker) startSideChild(key sideKey, p *sidePlan) *sideChild {
	params := revWorkerParams{
		deps:  w.deps,
		role:  common.WorkerRoleSp,
		shard: w.shard,
		cid:   w.cid,
		id:    w.spId,
		seed:  w.seed,
		desired: desiredState{
			revision: p.req.GetRevision(),
			handle:   p.addr,
		},
		after: w.sideStops.after(key),
	}
	var driver *sideDriver
	handle := startRevWorker(params, func(host *revWorker) objDriver {
		driver = newSideDriver(w, host, p)
		return driver
	})
	return &sideChild{handle: handle, driver: driver, plan: p}
}

// startCntlrChild starts one cntlr child (RW14), which waits for the stop of
// the cntlr's previous child if that is still running (RW1).
func (w *spWorker) startCntlrChild(p *cntlrPlan) *cntlrChild {
	params := revWorkerParams{
		deps:  w.deps,
		role:  common.WorkerRoleSp,
		shard: w.shard,
		cid:   w.cid,
		id:    w.spId,
		seed:  w.seed,
		desired: desiredState{
			revision: p.req.GetRevision(),
			handle:   p.addr,
		},
		after: w.cntlrStops.after(p.cntlrId),
	}
	var driver *cntlrDriver
	handle := startRevWorker(params, func(host *revWorker) objDriver {
		driver = newCntlrDriver(w, host, p)
		return driver
	})
	return &cntlrChild{handle: handle, driver: driver, plan: p}
}

// sideStopper returns the graceful stop of one side child: the loop first
// (RW11), then its bitmap pusher, which may still be delivering a chunk.
func (w *spWorker) sideStopper(child *sideChild) func() {
	return func() {
		child.handle.stop()
		child.driver.pusher.stop()
	}
}

// cntlrStopper returns the graceful stop of one cntlr child.
func (w *spWorker) cntlrStopper(child *cntlrChild) func() {
	return func() {
		child.handle.stop()
		child.driver.pusher.stop()
	}
}

// stopChildren stops every child in parallel and joins them, together with
// the stops the diff left running (SW5, RW11).
func (w *spWorker) stopChildren() {
	var stops []func()
	for key, child := range w.sides {
		stops = append(stops, w.sideStopper(child))
		delete(w.sides, key)
	}
	for cntlrId, child := range w.cntlrs {
		stops = append(stops, w.cntlrStopper(child))
		delete(w.cntlrs, cntlrId)
	}
	stopSpChildren(stops)
	w.sideStops.join()
	w.cntlrStops.join()
}

// keepChild reports whether a running child may stay where it is (RW14). It
// must be restarted when its endpoint moved, and — because the generic loop
// coalesces an unchanged desired state away (RW3) — also when a re-resolution
// changed its request without changing the SpRev revision.
func keepChild(
	oldAddr string,
	newAddr string,
	oldReq revisionedRequest,
	newReq revisionedRequest,
) bool {
	if oldAddr != newAddr {
		return false
	}
	if oldReq.GetRevision() != newReq.GetRevision() {
		return true
	}
	return proto.Equal(oldReq, newReq)
}

// revisionedRequest is what keepChild needs of a Syncup* request: its
// revision, plus enough of proto.Message to compare two of them.
type revisionedRequest interface {
	proto.Message
	GetRevision() uint64
}

// stopSpChildren runs a set of child stops concurrently and joins them (SW5):
// each may take up to common.DefaultWorkerSyncupTimeout to finish an in-flight
// unary call, so they must not be stopped one by one.
func stopSpChildren(stops []func()) {
	if len(stops) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, stop := range stops {
		wg.Add(1)
		go func(fn func()) {
			defer wg.Done()
			fn()
		}(stop)
	}
	wg.Wait()
}

// logUnresolved reports one child RW14 left idle for want of a DnConf/CnConf.
func (w *spWorker) logUnresolved(
	ctx context.Context,
	addrPort string,
	ids ...slog.Attr,
) {
	attrs := []any{
		slog.Uint64("cluster_id", w.cid),
		slog.Uint64("sp_id", w.spId),
		slog.String("addr_port", addrPort),
	}
	for _, attr := range ids {
		attrs = append(attrs, attr)
	}
	slog.InfoContext(ctx, msgSpChildUnresolved, attrs...)
}

// ---------------------------------------------------------------------------
// Reports: the two flips (RW18, RW19) and the leg rows (HL2)
// ---------------------------------------------------------------------------

// handleReports drains everything the children reported and applies it. The
// drain is RW18's collection window: several sides reported within one round
// share one FlipProvisioned STM, and every td completed by the replies of that
// moment shares one FlipCreated. A demoted cntlr's synced revision goes to
// RW22's demotion hold and a side's to RW14's barrier, which the last report
// each waits for moves on ahead of those STMs, and a cntlr's leg rows are
// recorded only while it is the primary of the plan the coordinator last
// decided (recordsLegRows).
func (w *spWorker) handleReports(first spReport) {
	ctx := newTraceCtx(w.ctx, w.seed)
	reports := []spReport{first}
	draining := true
	for draining {
		select {
		case rep := <-w.reportCh:
			reports = append(reports, rep)
		default:
			draining = false
		}
	}
	var sides []model.SideRef
	seenSide := make(map[sideKey]bool)
	var tds []model.TdRef
	seenTd := make(map[uint64]bool)
	for _, rep := range reports {
		if w.recordsLegRows(rep.cntlrId) {
			for _, row := range rep.legRows {
				w.observeLeg(ctx, row)
			}
		}
		if ref := rep.provisioned; ref != nil {
			key := sideKey{legId: ref.LegId, sideId: ref.SideId}
			if !seenSide[key] {
				seenSide[key] = true
				sides = append(sides, *ref)
			}
		}
		for _, tdId := range rep.createdTdIds {
			ref, ok := w.tdRefs[tdId]
			if !ok || seenTd[tdId] {
				// RW19: only a td the LOADED state shows created == false is
				// a candidate. Everything else causes no etcd traffic.
				continue
			}
			seenTd[tdId] = true
			tds = append(tds, ref)
		}
		if synced := rep.synced; synced != nil {
			child, ok := w.sides[synced.key]
			if ok && synced.revision > child.synced {
				child.synced = synced.revision
			}
		}
		if rep.cntlrSynced != 0 {
			child, ok := w.cntlrs[rep.cntlrId]
			if ok && rep.cntlrSynced > child.synced {
				child.synced = rep.cntlrSynced
			}
		}
	}
	w.releaseHeld(ctx)
	w.applyProvisionedFlip(ctx, sides)
	w.applyCreatedFlip(ctx, tds)
}

// observeLeg writes one leg's health (HL2). The row came from the PRIMARY
// cntlr's reply but the record lives on the leg's SLICE, which is why this is
// the coordinator's job; a leg the current state does not list is ignored.
func (w *spWorker) observeLeg(ctx context.Context, row legRow) {
	sliceId, ok := w.legSlice[row.legId]
	if !ok {
		return
	}
	monitor, ok := w.legs[row.legId]
	if !ok {
		monitor = newLegMonitor(w.deps, w.cid, w.spId, sliceId, row.legId)
		memo, legId := w.verdicts, row.legId
		monitor.verdict = func(unhealthy bool) {
			memo.noteLeg(legId, unhealthy)
		}
		w.legs[row.legId] = monitor
	}
	monitor.observe(ctx, row.obs, row.resName)
}

// healthSeqs reads the write count of every side and cntlr child's health
// monitor, BEFORE the load whose records reseedHealth then offers them (HL3):
// a child that has written since cannot tell whether that load holds its
// write, so it drops the offer (offerRecord). A child assembled by hand for a
// pass-only test has no monitor.
func (w *spWorker) healthSeqs() map[*healthMonitor]uint64 {
	seqs := make(map[*healthMonitor]uint64, len(w.cntlrs)+len(w.sides))
	for _, child := range w.cntlrs {
		if child.driver != nil && child.driver.health != nil {
			seqs[child.driver.health] = child.driver.health.loadSeq()
		}
	}
	for _, child := range w.sides {
		if child.driver != nil && child.driver.health != nil {
			seqs[child.driver.health] = child.driver.health.loadSeq()
		}
	}
	return seqs
}

// reseedHealth re-seeds every health memo of the SP from the records one load
// read (HL3): each side and cntlr child is offered its record's err_epoch with
// the write count healthSeqs read before the load, which the child folds in on
// its own goroutine before it judges its next reply or missed round (RW1),
// and the leg monitors, which are the coordinator's own, are re-seeded at
// once. A record the load did not find leaves its memo as it is, and so does
// a child the count was not read for.
func (w *spWorker) reseedHealth(
	state *model.SpState,
	seqs map[*healthMonitor]uint64,
) {
	for cntlrId, child := range w.cntlrs {
		cntlr, ok := state.Cntlrs[cntlrId]
		if !ok || child.driver == nil {
			continue
		}
		seq, ok := seqs[child.driver.health]
		if !ok {
			continue
		}
		child.driver.health.offerRecord(cntlr.GetErrEpoch(), seq)
	}
	legs := make(map[uint64]*pb.Leg)
	sides := make(map[sideKey]*pb.Side)
	for _, slice := range state.Slices {
		for _, grp := range allGroups(slice) {
			for _, list := range [][]*pb.Leg{
				grp.GetLegList(),
				grp.GetSpareLegList(),
			} {
				for _, leg := range list {
					legs[leg.GetLegId()] = leg
					for _, side := range leg.GetSideList() {
						key := sideKey{
							legId:  leg.GetLegId(),
							sideId: side.GetSideId(),
						}
						sides[key] = side
					}
				}
			}
		}
	}
	for key, child := range w.sides {
		side, ok := sides[key]
		if !ok || child.driver == nil {
			continue
		}
		seq, ok := seqs[child.driver.health]
		if !ok {
			continue
		}
		child.driver.health.offerRecord(side.GetErrEpoch(), seq)
	}
	for legId, monitor := range w.legs {
		if leg, ok := legs[legId]; ok {
			monitor.seedRecord(leg.GetErrEpoch())
		}
	}
}

// applyProvisionedFlip runs RW18's STM and logs the "flip applied" record
// (dnv-worker.md, Log records).
func (w *spWorker) applyProvisionedFlip(
	ctx context.Context,
	sides []model.SideRef,
) {
	if len(sides) == 0 {
		return
	}
	flipped, err := w.ops.flipProvisioned(ctx, w.cid, w.shard, w.spId, sides)
	if err != nil {
		slog.InfoContext(ctx, msgSpFlipFailed,
			slog.String("kind", flipKindProvisioned),
			slog.Uint64("cluster_id", w.cid),
			slog.Uint64("sp_id", w.spId),
			slog.String("error", err.Error()),
		)
		return
	}
	if len(flipped) == 0 {
		// Every listed side had been flipped already: no write, no bump, no
		// record (RW18 is idempotent).
		return
	}
	// One record per side the STM WROTE, never per candidate reported: a side
	// another owner flipped first is not this worker's flip, and the record
	// carries the revision this STM produced (dnv-worker.md, Log records).
	revision := w.currentRevision(ctx)
	for _, ref := range flipped {
		slog.InfoContext(ctx, msgFlipApplied,
			slog.String("kind", flipKindProvisioned),
			slog.Uint64("cluster_id", w.cid),
			slog.Uint64("sp_id", w.spId),
			slog.Uint64("slice_id", ref.SliceId),
			slog.Uint64("leg_id", ref.LegId),
			slog.Uint64("side_id", ref.SideId),
			slog.Uint64("revision", revision),
		)
	}
}

// applyCreatedFlip runs RW19's STM and logs the "flip applied" record
// (dnv-worker.md, Log records).
func (w *spWorker) applyCreatedFlip(ctx context.Context, cands []model.TdRef) {
	if len(cands) == 0 {
		return
	}
	created, err := w.ops.flipCreated(ctx, w.cid, w.shard, w.spId, cands)
	if err != nil {
		slog.InfoContext(ctx, msgSpFlipFailed,
			slog.String("kind", flipKindCreated),
			slog.Uint64("cluster_id", w.cid),
			slog.Uint64("sp_id", w.spId),
			slog.String("error", err.Error()),
		)
		return
	}
	if len(created) == 0 {
		// RW19: a candidate whose key is gone, whose td_id differs or that
		// is already created writes nothing, so nothing is reported.
		return
	}
	// One record per td the STM WROTE; see applyProvisionedFlip.
	revision := w.currentRevision(ctx)
	for _, ref := range created {
		slog.InfoContext(ctx, msgFlipApplied,
			slog.String("kind", flipKindCreated),
			slog.Uint64("cluster_id", w.cid),
			slog.Uint64("sp_id", w.spId),
			slog.String("td_name", ref.Name),
			slog.Uint64("td_id", ref.TdId),
			slog.Uint64("revision", revision),
		)
	}
}

// currentRevision reads the SpRev the flip just bumped, for the "revision"
// attribute of the "flip applied" record (dnv-worker.md, Log records). The flip
// ops report only how many records they wrote, so the new revision is read
// back; a concurrent bump would make this report a slightly newer one, which is
// a log detail, not a decision.
func (w *spWorker) currentRevision(ctx context.Context) uint64 {
	rev := &pb.SpRev{}
	found, err := w.deps.store.Get(
		ctx, model.SpRevKey(w.shard, w.cid, w.spId), rev,
	)
	if err != nil || !found {
		return 0
	}
	return rev.GetRevision()
}

// ---------------------------------------------------------------------------
// The side child (RW15, RW17, RW18, HL2, BM1-BM6)
// ---------------------------------------------------------------------------

// sideDriver is one side's half of the per-object loop (RW14-RW20). Everything
// but the plan slot runs on the child's own goroutine (RW1).
type sideDriver struct {
	deps   *deps
	host   *revWorker
	seed   string
	report chan spReport

	cid  uint64
	spId uint64
	dnId uint64
	ptr  *pb.SidePointer
	addr string

	// mu guards next — the newest plan the coordinator built (RW6), which the
	// loop takes in setDesired — and lastInfo, which fold writes from the
	// stream's pump goroutine and the loop reads.
	mu       sync.Mutex
	next     *sidePlan
	lastInfo *pb.SideInfo

	plan   *sidePlan
	health *healthMonitor
	pusher *bmPusher
	// synced is the last revision reportSynced handed the coordinator, so a
	// standing reply does not report every round.
	synced uint64
}

// storeInfo remembers the latest SideInfo (HL5); info returns it. A Check
// reply is decoded on the stream's pump goroutine and a Syncup* reply on the
// loop's, so the pointer is guarded.
func (d *sideDriver) storeInfo(info *pb.SideInfo) {
	d.mu.Lock()
	d.lastInfo = info
	d.mu.Unlock()
}

func (d *sideDriver) info() *pb.SideInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastInfo
}

// newSideDriver builds one side child's driver.
func newSideDriver(
	w *spWorker,
	host *revWorker,
	p *sidePlan,
) *sideDriver {
	d := &sideDriver{
		deps:   w.deps,
		host:   host,
		seed:   w.seed,
		report: w.reportCh,
		cid:    w.cid,
		spId:   w.spId,
		dnId:   p.dnId,
		ptr:    p.req.GetSidePointer(),
		addr:   p.addr,
		plan:   p,
	}
	d.health = newSideMonitor(w.deps, w.cid, w.spId, p.ref.SliceId, p.ref.SideId)
	memo := w.verdicts
	key := sideKey{legId: p.ref.LegId, sideId: p.ref.SideId}
	d.health.verdict = func(unhealthy bool) {
		memo.noteSide(key, unhealthy)
	}
	d.pusher = newBmPusher(bmPusherParams{
		deps:     w.deps,
		seed:     w.seed,
		kind:     bmKindMigr,
		addrPort: p.addr,
		idAttr:   "migr_id",
		ids: []slog.Attr{
			slog.Uint64("cluster_id", w.cid),
			slog.Uint64("dn_id", p.dnId),
			slog.Any("side_pointer", common.PbToLogValue(p.req.GetSidePointer())),
		},
		// A migration has one bitmap per leg, addressed by bm_idx alone: the
		// pusher's shared fetch signature carries a source slice index, and
		// it is always 0 here (architecture.md, Bitmap push protocol).
		fetch: func(
			ctx context.Context,
			name string,
			_ uint32,
			bmIdx uint32,
		) ([]byte, bool, error) {
			chunk := &pb.MigrBitmap{}
			found, err := d.deps.store.Get(
				ctx, model.MigrBitmapKey(d.cid, d.spId, name, bmIdx), chunk,
			)
			if err != nil {
				return nil, false, err
			}
			return chunk.GetBitmap(), found, nil
		},
		deliver: func(
			ctx context.Context,
			conn *grpc.ClientConn,
			part bmPart,
		) (uint32, string, error) {
			reply, err := pb.NewDiskNodeAgentClient(conn).PushMigrBitmap(
				ctx, &pb.PushMigrBitmapRequest{
					ClusterId:   d.cid,
					DnId:        d.dnId,
					SidePointer: d.ptr,
					MigrId:      part.resId,
					BmIdx:       part.bmIdx,
					Bitmap:      part.bitmap,
				},
			)
			if err != nil {
				return 0, "", fmt.Errorf("push migr bitmap: %w", err)
			}
			return reply.GetAgentReply().GetCode(),
				reply.GetAgentReply().GetDetails(), nil
		},
	})
	return d
}

// install hands the child its newest request (RW6). It runs on the
// coordinator's goroutine; the loop picks the plan up in current.
func (d *sideDriver) install(p *sidePlan) {
	d.mu.Lock()
	d.next = p
	d.mu.Unlock()
}

// current is the plan the child drives: whatever the coordinator installed
// last. Every loop-goroutine entry point takes it exactly once, so a fan-out
// that changed the plan without changing the desired state — a re-resolution
// tick, or a bitmap chunk appended at the same revision — still reaches the
// agent on the next round rather than waiting for the next bump.
func (d *sideDriver) current() *sidePlan {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.next != nil {
		d.plan = d.next
		d.next = nil
	}
	return d.plan
}

// logAttrs are the side's ids as the records of dnv-worker.md, Log records,
// carry them.
func (d *sideDriver) logAttrs() []slog.Attr {
	return []slog.Attr{
		slog.Uint64("cluster_id", d.cid),
		slog.Uint64("dn_id", d.dnId),
		slog.Any("side_pointer", common.PbToLogValue(d.ptr)),
	}
}

// childAttrs is the side_pointer an sp child's lifecycle records carry
// (dnv-worker.md, Log records).
func (d *sideDriver) childAttrs() []slog.Attr {
	return []slog.Attr{
		slog.Any("side_pointer", common.PbToLogValue(d.ptr)),
	}
}

// addrPort is the DN agent this side is driven at. It never changes under a
// child: a moved side is a restarted child (RW14).
func (d *sideDriver) addrPort() string {
	return d.addr
}

// interval is the side round period (RW9).
func (d *sideDriver) interval(cc *pb.ClusterConf) time.Duration {
	return roundPeriod(cc.GetHealthCheckConf().GetSideInterval())
}

// setDesired takes the request the coordinator installed (RW3, RW6).
func (d *sideDriver) setDesired(next desiredState) {
	d.current()
}

// openStream opens the side's CheckSide stream (RW4 step 1; architecture.md,
// Check streams).
func (d *sideDriver) openStream(
	ctx context.Context,
	conn *grpc.ClientConn,
) (checkStream, error) {
	stream, err := pb.NewDiskNodeAgentClient(conn).CheckSide(ctx)
	if err != nil {
		return nil, fmt.Errorf("check side stream: %w", err)
	}
	return &sideCheckStream{driver: d, stream: stream}, nil
}

// syncup issues one SyncupSide (RW5, RW15) and runs the BM2 diff on its reply:
// bm_info travels only on a SyncupSide reply, never on a Check round.
func (d *sideDriver) syncup(
	ctx context.Context,
	conn *grpc.ClientConn,
	cc *pb.ClusterConf,
) (*replyState, error) {
	plan := d.current()
	reply, err := pb.NewDiskNodeAgentClient(conn).SyncupSide(ctx, plan.req)
	if err != nil {
		return nil, fmt.Errorf("syncup side: %w", err)
	}
	state := d.fold(
		reply.GetAgentReply(), reply.GetRevision(), reply.GetSideInfo(),
	)
	if accepted(state.code) {
		d.diffBitmap(plan, reply.GetBmInfo())
	}
	return state, nil
}

// migrChunkIds is a migration agent's applied set in the pusher's shape: a
// migration reports the flat bm_idx_list of its one bitmap, which names no
// source slice, so every chunk of it is at slice 0 (architecture.md, Bitmap
// push protocol).
func migrChunkIds(bmIdxList []uint32) []model.BmChunk {
	out := make([]model.BmChunk, 0, len(bmIdxList))
	for _, bmIdx := range bmIdxList {
		out = append(out, model.BmChunk{Idx: bmIdx})
	}
	return out
}

// diffBitmap is BM2 for a migration destination side: the chunks etcd holds
// minus the ones the agent acknowledges in bm_idx_list, pushed in ascending
// bm_idx (BM3). Only a destination side pushes at all (BM4), and only when
// the reply's bm_info is about this side's migration.
func (d *sideDriver) diffBitmap(
	plan *sidePlan,
	info *pb.BitmapInfo,
) {
	if plan.migrName == "" || len(plan.chunks) == 0 {
		return
	}
	if info.GetResId() != plan.migrId {
		return
	}
	missing := d.pusher.missing(
		plan.migrId, plan.chunks, migrChunkIds(info.GetBmIdxList()),
	)
	if len(missing) == 0 {
		return
	}
	d.pusher.submit(&bmPlan{
		resId: plan.migrId,
		name:  plan.migrName,
		parts: missing,
	})
}

// observe folds one CheckSide/SyncupSide reply into the side's health (HL2,
// HL4), the RW18 flip report and RW14's barrier. A failed push is not re-armed
// here: it is logged and left to the next Syncup* reply, which re-plans the
// diff from the agent's own acknowledged set.
func (d *sideDriver) observe(ctx context.Context, r *replyState) {
	obs, res := sideObservation(r.code, d.info())
	d.health.observe(ctx, obs, res)
	d.reportProvisioned(ctx, r, d.current())
	d.reportSynced(ctx, r)
}

// reportProvisioned is RW18: a side whose request carried provisioned == false
// and that the agent reports fully zeroed is handed to the coordinator, which
// runs the flip.
//
// The condition reads the request this child is driving rather than a
// remembered "synced" one. The two can never disagree in a way that matters:
// provisioned only ever goes false -> true, so a synced false with a desired
// true means the flip already happened. Reading the current request is also
// what makes RW18's handoff rule work — a new owner has synced nothing yet and
// must still flip whatever its first round finds zeroed.
func (d *sideDriver) reportProvisioned(
	ctx context.Context,
	r *replyState,
	plan *sidePlan,
) {
	if !accepted(r.code) || plan.req.GetSideConf().GetProvisioned() {
		return
	}
	info := d.info()
	total := info.GetTotalExtCnt()
	if total == 0 || info.GetZeroedExtCnt() != total {
		return
	}
	ref := plan.ref
	d.send(ctx, spReport{provisioned: &ref})
}

// reportSynced hands the coordinator the revision this side's agent reports
// applied, once per revision: RW14's barrier holds the cntlrs until every
// side has. Any accepted reply counts, a Check round's as much as a
// SyncupSide's, because an agent that already holds the revision — after a
// handoff, say — is sent no Syncup* at all (RW4 step 5).
func (d *sideDriver) reportSynced(ctx context.Context, r *replyState) {
	if !accepted(r.code) || r.revision <= d.synced {
		return
	}
	d.synced = r.revision
	d.send(ctx, spReport{synced: &sideSynced{
		key:      sideKey{legId: d.ptr.GetLegId(), sideId: d.ptr.GetSideId()},
		revision: r.revision,
	}})
}

// send hands one report to the coordinator. It blocks only until the child is
// stopped, so a coordinator busy in an STM slows a child's round but can never
// wedge its shutdown (RW11).
func (d *sideDriver) send(ctx context.Context, rep spReport) {
	select {
	case d.report <- rep:
	case <-ctx.Done():
	}
}

// unreachable folds a broken stream or a missed reply into the side's health
// (HL2). The in-memory info is marked RES_STATUS_UNKNOWN (architecture.md,
// Live-state reporting) and never written to etcd.
func (d *sideDriver) unreachable(ctx context.Context) {
	markSideUnknown(d.info())
	d.health.observe(ctx, healthUnreachable, "")
}

// fold turns one reply into a replyState, remembering the SideInfo it carried
// (HL5).
func (d *sideDriver) fold(
	agentReply *pb.AgentReply,
	revision uint64,
	info *pb.SideInfo,
) *replyState {
	if info != nil {
		d.storeInfo(info)
	}
	return &replyState{
		revision:    revision,
		code:        agentReply.GetCode(),
		details:     agentReply.GetDetails(),
		infoPresent: info != nil,
	}
}

// sideCheckStream adapts the generated CheckSide stream to checkStream (RW17).
type sideCheckStream struct {
	driver *sideDriver
	stream grpc.BidiStreamingClient[pb.CheckSideRequest, pb.CheckSideReply]
}

func (s *sideCheckStream) send(
	traceId string, revision uint64, showInfo bool,
) error {
	req := sideCheckRequest(
		s.driver.cid, s.driver.dnId, s.driver.ptr, revision, showInfo,
	)
	req.TraceId = traceId
	if err := s.stream.Send(req); err != nil {
		return fmt.Errorf("check side send: %w", err)
	}
	return nil
}

func (s *sideCheckStream) recv() (*replyState, error) {
	reply, err := s.stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("check side recv: %w", err)
	}
	return s.driver.fold(
		reply.GetAgentReply(), reply.GetRevision(), reply.GetSideInfo(),
	), nil
}

func (s *sideCheckStream) closeSend() error {
	return s.stream.CloseSend()
}

// sideCheckRequest builds one CheckSide round request (RW17).
func sideCheckRequest(
	cid uint64,
	dnId uint64,
	ptr *pb.SidePointer,
	revision uint64,
	showInfo bool,
) *pb.CheckSideRequest {
	return &pb.CheckSideRequest{
		ClusterId:   cid,
		DnId:        dnId,
		SidePointer: ptr,
		Revision:    revision,
		ShowInfo:    showInfo,
	}
}

// ---------------------------------------------------------------------------
// The cntlr child (RW16, RW17, RW19, HL2, BM1-BM6)
// ---------------------------------------------------------------------------

// cntlrDriver is one cntlr's half of the per-object loop (RW14-RW20, RW22).
type cntlrDriver struct {
	deps   *deps
	host   *revWorker
	seed   string
	report chan spReport

	cid  uint64
	spId uint64
	cnId uint64
	ptr  *pb.CntlrPointer
	addr string

	// mu guards next (RW6) and lastInfo, which fold writes from the stream's
	// pump goroutine and the loop reads.
	mu       sync.Mutex
	next     *cntlrPlan
	lastInfo *pb.CntlrInfo

	plan   *cntlrPlan
	health *healthMonitor
	pusher *bmPusher
	// legLogged remembers the last standby leg row logged per leg, so HL2's
	// "logged, never recorded" does not repeat one row every round.
	legLogged map[uint64]pb.ResStatus
	// synced is the last revision a report handed the coordinator, so a
	// standing reply does not report every round (RW22).
	synced uint64
}

// newCntlrDriver builds one cntlr child's driver.
func newCntlrDriver(
	w *spWorker,
	host *revWorker,
	p *cntlrPlan,
) *cntlrDriver {
	d := &cntlrDriver{
		deps:      w.deps,
		host:      host,
		seed:      w.seed,
		report:    w.reportCh,
		cid:       w.cid,
		spId:      w.spId,
		cnId:      p.cnId,
		ptr:       p.req.GetCntlrPointer(),
		addr:      p.addr,
		plan:      p,
		legLogged: make(map[uint64]pb.ResStatus),
	}
	d.health = newCntlrMonitor(w.deps, w.cid, w.spId, p.cntlrId)
	d.health.settlePending = p.settling
	memo, cntlrId := w.verdicts, p.cntlrId
	d.health.verdict = func(unhealthy bool) {
		memo.noteCntlr(cntlrId, unhealthy)
	}
	d.pusher = newBmPusher(bmPusherParams{
		deps:     w.deps,
		seed:     w.seed,
		kind:     bmKindClone,
		addrPort: p.addr,
		idAttr:   "clone_id",
		ids: []slog.Attr{
			slog.Uint64("cluster_id", w.cid),
			slog.Uint64("cn_id", p.cnId),
			slog.Any(
				"cntlr_pointer",
				common.PbToLogValue(p.req.GetCntlrPointer()),
			),
		},
		fetch: func(
			ctx context.Context,
			name string,
			sliceIdx uint32,
			bmIdx uint32,
		) ([]byte, bool, error) {
			chunk := &pb.CloneBitmap{}
			found, err := d.deps.store.Get(
				ctx,
				model.CloneBitmapKey(d.cid, d.spId, name, sliceIdx, bmIdx),
				chunk,
			)
			if err != nil {
				return nil, false, err
			}
			return chunk.GetBitmap(), found, nil
		},
		deliver: func(
			ctx context.Context,
			conn *grpc.ClientConn,
			part bmPart,
		) (uint32, string, error) {
			reply, err := pb.NewControllerNodeAgentClient(conn).
				PushCloneBitmap(ctx, &pb.PushCloneBitmapRequest{
					ClusterId:    d.cid,
					CnId:         d.cnId,
					CntlrPointer: d.ptr,
					CloneId:      part.resId,
					SrcSliceIdx:  part.sliceIdx,
					BmIdx:        part.bmIdx,
					Bitmap:       part.bitmap,
				})
			if err != nil {
				return 0, "", fmt.Errorf("push clone bitmap: %w", err)
			}
			return reply.GetAgentReply().GetCode(),
				reply.GetAgentReply().GetDetails(), nil
		},
	})
	return d
}

// storeInfo remembers the latest CntlrInfo (HL5); info returns it. See
// sideDriver.storeInfo for why the pointer is guarded.
func (d *cntlrDriver) storeInfo(info *pb.CntlrInfo) {
	d.mu.Lock()
	d.lastInfo = info
	d.mu.Unlock()
}

func (d *cntlrDriver) info() *pb.CntlrInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastInfo
}

// infoSnapshot returns a COPY of the cntlr's latest info, taken under the
// driver's lock. info() hands out the live message, which only the child's own
// goroutine may read: the coordinator's reaction pass runs on ITS goroutine
// (AR1) while this one keeps folding replies into the field and marking its
// rows UNKNOWN, so the pass gets a copy or nothing.
func (d *cntlrDriver) infoSnapshot() *pb.CntlrInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastInfo == nil {
		return nil
	}
	return proto.Clone(d.lastInfo).(*pb.CntlrInfo)
}

// install hands the child its newest request (RW6).
func (d *cntlrDriver) install(p *cntlrPlan) {
	d.mu.Lock()
	d.next = p
	d.mu.Unlock()
}

// current is the plan the child drives; see sideDriver.current. Taking a new
// plan re-seeds the settle memo from its record (HL2): the record is the
// truth, so a plan loaded after the settle write clears the memo, and one
// loaded before it costs at most one redundant, no-op settle write. It is
// seeded here rather than in install because the monitor belongs to the
// child's goroutine (RW1), the only caller of current, while install runs on
// the coordinator's.
func (d *cntlrDriver) current() *cntlrPlan {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.next != nil {
		d.plan = d.next
		d.next = nil
		d.health.settlePending = d.plan.settling
	}
	return d.plan
}

// logAttrs are the cntlr's ids as the records of dnv-worker.md, Log records,
// carry them.
func (d *cntlrDriver) logAttrs() []slog.Attr {
	return []slog.Attr{
		slog.Uint64("cluster_id", d.cid),
		slog.Uint64("cn_id", d.cnId),
		slog.Any("cntlr_pointer", common.PbToLogValue(d.ptr)),
	}
}

// childAttrs is the cntlr_pointer an sp child's lifecycle records carry
// (dnv-worker.md, Log records).
func (d *cntlrDriver) childAttrs() []slog.Attr {
	return []slog.Attr{
		slog.Any("cntlr_pointer", common.PbToLogValue(d.ptr)),
	}
}

// addrPort is the CN agent this cntlr is driven at (RW14: a moved cntlr is a
// restarted child).
func (d *cntlrDriver) addrPort() string {
	return d.addr
}

// interval is the cntlr round period (RW9).
func (d *cntlrDriver) interval(cc *pb.ClusterConf) time.Duration {
	return roundPeriod(cc.GetHealthCheckConf().GetCntlrInterval())
}

// setDesired takes the request the coordinator installed (RW3, RW6).
func (d *cntlrDriver) setDesired(next desiredState) {
	d.current()
}

// openStream opens the cntlr's CheckCntlr stream (RW4 step 1; architecture.md,
// Check streams).
func (d *cntlrDriver) openStream(
	ctx context.Context,
	conn *grpc.ClientConn,
) (checkStream, error) {
	stream, err := pb.NewControllerNodeAgentClient(conn).CheckCntlr(ctx)
	if err != nil {
		return nil, fmt.Errorf("check cntlr stream: %w", err)
	}
	return &cntlrCheckStream{driver: d, stream: stream}, nil
}

// syncup issues one SyncupCntlr (RW5, RW16) and runs the BM2 diff on its
// reply: bm_info_list travels only on a SyncupCntlr reply.
func (d *cntlrDriver) syncup(
	ctx context.Context,
	conn *grpc.ClientConn,
	cc *pb.ClusterConf,
) (*replyState, error) {
	plan := d.current()
	reply, err := pb.NewControllerNodeAgentClient(conn).SyncupCntlr(
		ctx, plan.req,
	)
	if err != nil {
		return nil, fmt.Errorf("syncup cntlr: %w", err)
	}
	state := d.fold(
		reply.GetAgentReply(), reply.GetRevision(), reply.GetCntlrInfo(),
	)
	if accepted(state.code) {
		d.diffBitmaps(plan, reply.GetBmInfoList())
	}
	return state, nil
}

// cloneChunkIds is a clone agent's applied set in the pusher's shape: a clone
// reports the (src_slice_idx, bm_idx) pairs of chunk_id_list, bm_idx_list
// being migration-only (BM2).
func cloneChunkIds(chunkIdList []*pb.BmChunkId) []model.BmChunk {
	out := make([]model.BmChunk, 0, len(chunkIdList))
	for _, chunkId := range chunkIdList {
		out = append(out, model.BmChunk{
			SliceIdx: chunkId.GetSrcSliceIdx(),
			Idx:      chunkId.GetBmIdx(),
		})
	}
	return out
}

// diffBitmaps is BM2 for the clones of an SP, and BM4's target rule: only the
// PRIMARY cntlr's CN runs the dm-clones, so a standby's bm_info_list is
// ignored. A clone absent from the list has everything missing, and so has
// every pair its chunk_id_list omits.
func (d *cntlrDriver) diffBitmaps(
	plan *cntlrPlan,
	list []*pb.BitmapInfo,
) {
	if !plan.primary || len(plan.clones) == 0 {
		return
	}
	applied := make(map[uint64][]model.BmChunk, len(list))
	for _, info := range list {
		applied[info.GetResId()] = cloneChunkIds(info.GetChunkIdList())
	}
	for _, clone := range plan.clones {
		if len(clone.chunks) == 0 {
			continue
		}
		missing := d.pusher.missing(
			clone.id, clone.chunks, applied[clone.id],
		)
		if len(missing) == 0 {
			continue
		}
		d.pusher.submit(&bmPlan{
			resId: clone.id,
			name:  clone.name,
			parts: missing,
		})
	}
}

// observe folds one CheckCntlr/SyncupCntlr reply into the cntlr's health
// (HL2 — every ERROR row of the CntlrInfo OTHER than leg_id_to_leg), the
// settling flag, the RW19 created candidates and the leg rows the coordinator
// records. A failed push is not re-armed here: it is logged and left to the
// next Syncup* reply.
func (d *cntlrDriver) observe(ctx context.Context, r *replyState) {
	plan := d.current()
	info := d.info()
	obs, res := cntlrObservation(r.code, info)
	if obs == healthClean && !plan.primary &&
		r.revision < plan.req.GetRevision() {
		// HL2: a cntlr driven as a standby proves nothing by a clean reply
		// at an older revision. The agent of a demoted primary may still
		// run the primary shape over legs its sides have fenced, and
		// clearing its err_epoch would list it in the discovery records
		// again (architecture.md [D18]) while its namespaces are optimized
		// and fail every IO. The Syncup* of the driven revision, which RW4
		// step 5 or RW6 sends next, brings the verdict.
		obs = healthNone
	}
	// HL2: only a clean reply of the PRIMARY at the revision the child drives
	// settles it. The revision gate is load-bearing: when the promotion's
	// SyncupCntlr never reached the agent, the next Check reply carries the
	// previous revision and describes the standby shape — clean, and
	// meaningless for the promotion (one the agent applied but whose reply
	// was lost leaves the agent at the driven revision). Rounds and syncups
	// run on this child's one goroutine, and a revision carries one set of
	// roles: a role change bumps SpRev (architecture.md, Revision keys and the
	// sync fan-out) and the fan-out builds no plan from a state newer than its
	// label (RW14), an etcd restore, which can reuse a revision, aside. So an
	// accepted reply at the driven revision of an ENABLED primary is a report
	// of the primary shape. The role gate is the cn agent's own (cnagent.md
	// CN9: primary iff primary && !disabled): a disabled primary converges the
	// standby shape, so its clean reply proves nothing, and the gateway's
	// UpdateCntlrEnabled marks a primary it re-enables settling again. And the
	// report must show the stack built (primaryShapeBuilt): a new SP's primary
	// reports its pools and what is over them PROVISIONING, clean, until its
	// sides are zeroed, and a Check round between a converge that left members
	// unavailable and the retry that builds over them reports the unbuilt
	// devices MISSING, clean too when the SP has no td; settling on either
	// would leave the build that follows to primary_unhealthy.
	canSettle := plan.primary && !plan.req.GetCntlr().GetDisabled() &&
		r.revision == plan.req.GetRevision() && primaryShapeBuilt(info)
	if d.health.observeSettle(ctx, obs, res, canSettle) {
		slog.InfoContext(ctx, msgCntlrSettled,
			slog.String("role", common.WorkerRoleSp),
			slog.Uint64("cluster_id", d.cid),
			slog.Uint64("sp_id", d.spId),
			slog.Uint64("cn_id", d.cnId),
			slog.Any("cntlr_pointer", common.PbToLogValue(d.ptr)),
			slog.Uint64("revision", r.revision),
		)
	}
	if !accepted(r.code) {
		// HL2/RW19: a rejected request neither sets health nor completes a
		// td. A leftover reply is accepted and evaluated like code 0.
		return
	}
	rep := spReport{
		cntlrId:      d.ptr.GetCntlrId(),
		createdTdIds: completedTds(info, plan.sliceIds),
	}
	if plan.primary {
		rep.legRows = d.legRows(info)
	} else {
		d.logStandbyLegRows(ctx, info, plan.cntlrId)
	}
	if r.revision > d.synced {
		// RW22: the revision the agent now holds, once per revision; any
		// accepted reply counts, as for a side (sideDriver.reportSynced).
		d.synced = r.revision
		rep.cntlrSynced = r.revision
	}
	if len(rep.createdTdIds) == 0 && len(rep.legRows) == 0 &&
		rep.cntlrSynced == 0 {
		return
	}
	d.send(ctx, rep)
}

// legRows is the PRIMARY's probe (architecture.md, Group on-leg layout: meta
// region, data region, health block) of every leg it reported, spares
// included (HL2). ERROR sets the leg's err_epoch, OK clears it, everything
// else neither.
func (d *cntlrDriver) legRows(info *pb.CntlrInfo) []legRow {
	rows := make([]legRow, 0, len(info.GetLegIdToLeg()))
	for _, legId := range sortedKeys(info.GetLegIdToLeg()) {
		// The caller has already established an accepted code (HL2).
		obs, res := legObservation(0, info, legId)
		if obs == healthNone {
			continue
		}
		rows = append(rows, legRow{legId: legId, obs: obs, resName: res})
	}
	return rows
}

// logStandbyLegRows is HL2's "a standby's leg row is logged, never recorded":
// a standby reports transport liveness and ana_state (architecture.md, Group
// on-leg layout: meta region, data region, health block), which says
// nothing about the leg's health. Logged only when the row changes, so a
// standing row does not repeat every round.
func (d *cntlrDriver) logStandbyLegRows(
	ctx context.Context,
	info *pb.CntlrInfo,
	cntlrId uint64,
) {
	rows := info.GetLegIdToLeg()
	for _, legId := range sortedKeys(rows) {
		status := rows[legId].GetStatus()
		if last, ok := d.legLogged[legId]; ok && last == status {
			continue
		}
		d.legLogged[legId] = status
		slog.InfoContext(ctx, msgSpStandbyLegRow,
			slog.Uint64("cluster_id", d.cid),
			slog.Uint64("sp_id", d.spId),
			slog.Uint64("cntlr_id", cntlrId),
			slog.Uint64("leg_id", legId),
			slog.String("status", status.String()),
			slog.String("details", rows[legId].GetDetails()),
		)
	}
	for legId := range d.legLogged {
		if _, ok := rows[legId]; !ok {
			delete(d.legLogged, legId)
		}
	}
}

// send hands one report to the coordinator (see sideDriver.send).
func (d *cntlrDriver) send(ctx context.Context, rep spReport) {
	select {
	case d.report <- rep:
	case <-ctx.Done():
	}
}

// unreachable folds a broken stream or a missed reply into the cntlr's health
// (HL2; architecture.md, Live-state reporting). It reports nothing about the
// legs: only an ERROR row from the primary sets a leg's err_epoch, and an
// unreachable primary is the CNTLR's health, not the legs'.
func (d *cntlrDriver) unreachable(ctx context.Context) {
	d.markInfoUnknown()
	d.health.observe(ctx, healthUnreachable, "")
}

// markInfoUnknown is the "no answer from the node" of architecture.md,
// Live-state reporting, applied to the last known info in place. It takes the
// driver's lock because the coordinator may be snapshotting the same message
// for a reaction pass (AR1).
func (d *cntlrDriver) markInfoUnknown() {
	d.mu.Lock()
	defer d.mu.Unlock()
	markCntlrUnknown(d.lastInfo)
}

// fold turns one reply into a replyState, remembering the CntlrInfo it carried
// (HL5).
func (d *cntlrDriver) fold(
	agentReply *pb.AgentReply,
	revision uint64,
	info *pb.CntlrInfo,
) *replyState {
	if info != nil {
		d.storeInfo(info)
	}
	return &replyState{
		revision:    revision,
		code:        agentReply.GetCode(),
		details:     agentReply.GetDetails(),
		infoPresent: info != nil,
	}
}

// cntlrCheckStream adapts the generated CheckCntlr stream to checkStream
// (RW17).
type cntlrCheckStream struct {
	driver *cntlrDriver
	stream grpc.BidiStreamingClient[pb.CheckCntlrRequest, pb.CheckCntlrReply]
}

func (s *cntlrCheckStream) send(
	traceId string, revision uint64, showInfo bool,
) error {
	req := cntlrCheckRequest(
		s.driver.cid, s.driver.cnId, s.driver.ptr, revision, showInfo,
	)
	req.TraceId = traceId
	if err := s.stream.Send(req); err != nil {
		return fmt.Errorf("check cntlr send: %w", err)
	}
	return nil
}

func (s *cntlrCheckStream) recv() (*replyState, error) {
	reply, err := s.stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("check cntlr recv: %w", err)
	}
	return s.driver.fold(
		reply.GetAgentReply(), reply.GetRevision(), reply.GetCntlrInfo(),
	), nil
}

func (s *cntlrCheckStream) closeSend() error {
	return s.stream.CloseSend()
}

// cntlrCheckRequest builds one CheckCntlr round request (RW17).
func cntlrCheckRequest(
	cid uint64,
	cnId uint64,
	ptr *pb.CntlrPointer,
	revision uint64,
	showInfo bool,
) *pb.CheckCntlrRequest {
	return &pb.CheckCntlrRequest{
		ClusterId:    cid,
		CnId:         cnId,
		CntlrPointer: ptr,
		Revision:     revision,
		ShowInfo:     showInfo,
	}
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// completedTds applies RW19's row conditions — the accepted code (0 or
// common.ReplyCodeLeftover, accepted) is the caller's to check — to one
// CntlrInfo, and returns the td_ids the reply COMPLETES
// (RW19): the thin info exists, its slice_id_to_dm_thin key
// set equals the SP's slice ids exactly (every slice, no extra, no missing) and
// every row is RES_STATUS_OK.
//
// Anything else is "not yet": no entry at all (a standby, cnagent.md CN14
// "primary only"), a partial map, or any MISSING/ERROR/PROVISIONING/UNKNOWN
// row. The reply's revision is deliberately not compared with anything — thin
// ids are monotonic facts about the pool metadata, and identity is guarded
// inside model.FlipCreated's STM.
func completedTds(info *pb.CntlrInfo, sliceIds []uint64) []uint64 {
	thins := info.GetTdIdToThinInfo()
	if len(thins) == 0 || len(sliceIds) == 0 {
		return nil
	}
	var out []uint64
	for _, tdId := range sortedKeys(thins) {
		rows := thins[tdId].GetSliceIdToDmThin()
		if len(rows) != len(sliceIds) {
			continue
		}
		complete := true
		for _, sliceId := range sliceIds {
			res, ok := rows[sliceId]
			if !ok || res.GetStatus() != pb.ResStatus_RES_STATUS_OK {
				complete = false
				break
			}
		}
		if complete {
			out = append(out, tdId)
		}
	}
	return out
}

// sideOfLeg returns the side of a leg with the given id, nil when the leg does
// not hold it.
func sideOfLeg(leg *pb.Leg, sideId uint64) *pb.Side {
	for _, side := range leg.GetSideList() {
		if side.GetSideId() == sideId {
			return side
		}
	}
	return nil
}

// dataBlockSize is the pool's STORED data block size: what a migration
// destination's dm-clone uses as its region size (RW15) and what AR6 converts
// a meta group's data region with. It substitutes nothing — CreateStoragePool
// made the value concrete (architecture.md, Common validation) and both callers
// sit behind a ValidateBdevConf gate. It stays a one-line delegation to model
// so this pre-check and the GrowSlice STM cannot end up meaning different
// fields.
func dataBlockSize(bdevConf *pb.BdevConf) uint64 {
	return model.PoolBlockSize(bdevConf)
}

// migrCloneConf resolves the defaults (architecture.md, Common validation) of a
// migration's dm-clone knobs before they are sent (RW15). The conf is copied,
// never patched in place: it belongs to the loaded state, which the coordinator
// hands to every child.
func migrCloneConf(conf *pb.DmCloneConf) *pb.DmCloneConf {
	out := &pb.DmCloneConf{
		HydrationThreshold: conf.GetHydrationThreshold(),
		HydrationBatchSize: conf.GetHydrationBatchSize(),
	}
	if out.HydrationThreshold == 0 {
		out.HydrationThreshold = common.DefaultMigrThreshold
	}
	if out.HydrationBatchSize == 0 {
		out.HydrationBatchSize = common.DefaultMigrBatchSize
	}
	return out
}
