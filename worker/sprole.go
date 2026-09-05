// The sp role of dnv-worker.md §8.4 (RW14-RW20).
//
// The sp revision worker is not a per-object loop but a COORDINATOR: on every
// desired change (an SpRev put: revision + sp_name) it loads the whole SP with
// model.LoadSp, builds one SyncupSideRequest per side — spare legs' sides
// included — and one SyncupCntlrRequest per cntlr (RW15/RW16), and diffs those
// against its running children. Each child is an ordinary revision worker of
// §8.1 with the side resp. cntlr driver below, so "one goroutine, one Check*
// stream, one round timer per object" (RW1) holds for sides and cntlrs exactly
// as it does for DNs and CNs.
//
// What the coordinator keeps for itself is everything that is not per-object:
// the two flips (RW18/RW19), the Leg health rows — which the PRIMARY cntlr
// reports but which are written on the SLICE, so only the coordinator knows
// where they go (HL2) — and the reaction pass (§11).
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

// msgFlipApplied is the §12 record of the two flips (RW18, RW19).
const msgFlipApplied = "flip applied"

// The "kind" attribute of the §12 "flip applied" record.
const (
	flipKindProvisioned = "provisioned"
	flipKindCreated     = "created"
)

// Non-normative records of the sp coordinator. The §12 strings are the ones
// the §14 suite greps; these exist so an operator can see why an SP is not
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

// ---------------------------------------------------------------------------
// The model surface (MD3, MD6)
// ---------------------------------------------------------------------------

// spOps is everything the sp coordinator reads and writes through model. It is
// an interface so the §13 tests can drive a real coordinator against a fixture
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
	// which is what the §12 "flip applied" record names (one per side).
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

// clonePlan is one clone's bitmap source for the primary cntlr (BM1).
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
	req     *pb.SyncupCntlrRequest
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
	// unresolved counts the children an absent DnConf/CnConf left idle
	// (RW14); a nonzero count makes the coordinator re-resolve on its ticker.
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
}

// cntlrChild is one running cntlr child of the coordinator.
type cntlrChild struct {
	handle *revWorker
	driver *cntlrDriver
	plan   *cntlrPlan
}

// legRow is one leg's §3.6 probe row as the PRIMARY cntlr reported it (HL2).
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
}

// ---------------------------------------------------------------------------
// The coordinator (RW14)
// ---------------------------------------------------------------------------

// spWorker is the sp role's revision worker: a coordinator with one child
// goroutine per side and per cntlr (§8.4).
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
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}

	// Everything below is owned by the run goroutine.
	desired   desiredState
	sides     map[sideKey]*sideChild
	cntlrs    map[uint64]*cntlrChild
	legs      map[uint64]*healthMonitor
	legSlice  map[uint64]uint64
	tdRefs    map[uint64]model.TdRef
	fanWanted bool
	idleCnt   int
	// react is the §11 half of the coordinator: the model surface of the
	// automatic reactions and the little memo their log records need
	// (reaction.go).
	react *reactor
}

// newSpWorker starts the sp coordinator (SW2). Its signature is the
// revKind.newWorker contract.
func newSpWorker(p revWorkerParams) revWorkerHandle {
	return startSpWorker(p, &modelSpOps{cli: p.deps.cli})
}

// startSpWorker is newSpWorker with the model surface injected, so the §13
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
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		desired:   p.desired,
		sides:     make(map[sideKey]*sideChild),
		cntlrs:    make(map[uint64]*cntlrChild),
		legs:      make(map[uint64]*healthMonitor),
		legSlice:  make(map[uint64]uint64),
		tdRefs:    make(map[uint64]model.TdRef),
		react:     newReactor(&modelReactionOps{cli: p.deps.cli}),
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
// stopped in parallel on the way out, so a stop costs one in-flight call, not
// one per child.
func (w *spWorker) stop() {
	w.cancel()
	<-w.done
}

// run is the coordinator's loop (RW14, RW20). Unlike a per-object loop it
// issues no RPC of its own: it fans out on every desired change, applies the
// children's reports, and ticks once per cntlr_interval for the idle-child
// re-resolution and the reaction pass (AR1).
func (w *spWorker) run() {
	defer close(w.done)
	slog.InfoContext(w.ctx, msgRevisionWorkerStarted, w.attrs()...)
	defer func() {
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
	if !ok {
		return common.DefaultHealthCheckInterval * time.Second
	}
	return roundPeriod(cc.GetHealthCheckConf().GetCntlrInterval())
}

// attrs are the §12 "revision worker started"/"stopped" attributes of the
// coordinator. Its children add side_pointer / cntlr_pointer of their own.
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
			// lists (§9.1), so the sp role has nothing left to send.
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
	plan := w.buildPlan(ctx, state)
	w.applyPlan(plan)
}

// buildPlan turns one loaded SP into every request the fan-out sends
// (RW15/RW16). It resolves each side's dn_id through SpState.DnByAddr and each
// cntlr's cn_id through CnByAddr (§10.3): those two maps are read at the same
// store revision as the SP itself, so no request can mix two views.
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
	// (cnagent.md §4) — in SpConf.cntlr_id_list order, so an unchanged state
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

	if !cntlrsResolved {
		// RW15 + RW14: rather than shipping half a side_conf — an unresolved
		// PRIMARY would send primary_cn_id = 0 and drop the real primary from
		// standby_id_list, and every DN of the SP would tear that CN's
		// dm-error, dm-linear, subsystem and namespace down — every SIDE
		// child is left idle, exactly as addMigrConf leaves a migration whose
		// peer cannot be resolved. plan.unresolved is already nonzero, so the
		// cntlr_interval ticker re-resolves (RW14). The CNTLR children are
		// unaffected: RW16's request carries no peer's cn_id, so an
		// unresolved endpoint's effect stays confined to its own child.
		slog.InfoContext(ctx, msgSpSidesIdle,
			slog.Uint64("cluster_id", w.cid),
			slog.Uint64("sp_id", w.spId),
			slog.String("reason", "cntlr unresolved"),
		)
	}
	w.buildSidePlans(ctx, plan, state, primaryCnId, standby, cntlrsResolved)
	w.buildCntlrPlans(ctx, plan, state, cnIds)
	return plan
}

// buildSidePlans walks every side of the SP — both groups of every slice, both
// the active legs and the SPARE legs (RW14, §8.12) — and builds its request.
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
// side is the source, the other the destination (§11.2). It reports whether
// the request is complete — a migration whose peer side's DN cannot be
// resolved leaves this child idle rather than sending half a role.
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
// (§10.3), in SpConf's list orders so the requests are deterministic.
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
		// The key the cn agent reads (RW16, §9.3).
		idToSlice[fmt.Sprintf(common.IdKeyFmt, sliceId)] = slice
	}
	clones := make([]*pb.Clone, 0, len(conf.GetCloneNameList()))
	clonePlans := make([]clonePlan, 0, len(conf.GetCloneNameList()))
	for _, cloneName := range conf.GetCloneNameList() {
		clone, ok := state.Clones[cloneName]
		if !ok {
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
			addr:    cntlr.GetAddrPort(),
			cnId:    cnId,
			cntlrId: cntlrId,
			primary: cntlr.GetPrimary(),
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

// applyPlan is RW14's child diff: new => start; gone => stop (RW11); a child
// whose ENDPOINT changed is restarted at the new one; every remaining child
// receives its new request as a desired change (RW6, immediate syncup).
//
// A re-resolution tick can also change a request without changing the SpRev
// revision — a cntlr whose CnConf finally appeared changes every side's
// standby list. The generic loop coalesces an unchanged desired state away
// (RW3), so such a child is restarted instead: rare, and the alternative
// would be a child driving a stale request until the next bump.
func (w *spWorker) applyPlan(plan *spPlan) {
	var stops []func()
	for key, child := range w.sides {
		next, ok := plan.sides[key]
		if ok && keepChild(
			child.plan.addr, next.addr, child.plan.req, next.req,
		) {
			continue
		}
		stops = append(stops, w.sideStopper(child))
		delete(w.sides, key)
	}
	for cntlrId, child := range w.cntlrs {
		next, ok := plan.cntlrs[cntlrId]
		if ok && keepChild(
			child.plan.addr, next.addr, child.plan.req, next.req,
		) {
			continue
		}
		stops = append(stops, w.cntlrStopper(child))
		delete(w.cntlrs, cntlrId)
	}
	stopSpChildren(stops)

	for key, next := range plan.sides {
		if child, ok := w.sides[key]; ok {
			child.plan = next
			child.driver.install(next)
			child.handle.update(desiredState{
				revision: next.req.GetRevision(),
				handle:   next.addr,
			})
			continue
		}
		w.sides[key] = w.startSideChild(next)
	}
	for cntlrId, next := range plan.cntlrs {
		if child, ok := w.cntlrs[cntlrId]; ok {
			child.plan = next
			child.driver.install(next)
			child.handle.update(desiredState{
				revision: next.req.GetRevision(),
				handle:   next.addr,
			})
			continue
		}
		w.cntlrs[cntlrId] = w.startCntlrChild(next)
	}

	w.legSlice = plan.legSlice
	w.tdRefs = plan.tdRefs
	for legId := range w.legs {
		if _, ok := w.legSlice[legId]; !ok {
			delete(w.legs, legId)
		}
	}
	w.idleCnt = plan.unresolved
}

// startSideChild starts one side child (RW14): an ordinary revision worker of
// §8.1 with the side driver.
func (w *spWorker) startSideChild(p *sidePlan) *sideChild {
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
	}
	var driver *sideDriver
	handle := startRevWorker(params, func(host *revWorker) objDriver {
		driver = newSideDriver(w, host, p)
		return driver
	})
	return &sideChild{handle: handle, driver: driver, plan: p}
}

// startCntlrChild starts one cntlr child (RW14).
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

// stopChildren stops every child in parallel and joins them (SW5, RW11).
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
// moment shares one FlipCreated.
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
		for _, row := range rep.legRows {
			w.observeLeg(ctx, row)
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
	}
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
		w.legs[row.legId] = monitor
	}
	monitor.observe(ctx, row.obs, row.resName)
}

// applyProvisionedFlip runs RW18's STM and logs the §12 record.
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
	// carries the revision this STM produced (§12).
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

// applyCreatedFlip runs RW19's STM and logs the §12 record.
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
// attribute of the §12 record. The flip ops report only how many records they
// wrote, so the new revision is read back; a concurrent bump would make this
// report a slightly newer one, which is a log detail, not a decision.
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

// sideDriver is one side's half of the per-object loop (§8.4). Everything but
// the plan slot runs on the child's own goroutine (RW1).
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
		fetch: func(
			ctx context.Context,
			name string,
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
					Revision:    part.revision,
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

// logAttrs are the side's ids as the §12 records carry them.
func (d *sideDriver) logAttrs() []slog.Attr {
	return []slog.Attr{
		slog.Uint64("cluster_id", d.cid),
		slog.Uint64("dn_id", d.dnId),
		slog.Any("side_pointer", common.PbToLogValue(d.ptr)),
	}
}

// childAttrs is the side_pointer an sp child's lifecycle records carry (§12).
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

// openStream opens the side's CheckSide stream (RW4 step 1, §9.7).
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
	if state.code == 0 {
		d.diffBitmap(plan, reply.GetBmInfo(), reply.GetRevision())
	}
	return state, nil
}

// diffBitmap is BM2 for a migration destination side: the chunks etcd holds
// minus the ones the agent acknowledges, pushed in ascending bm_idx (BM3).
// Only a destination side pushes at all (BM4), and only when the reply's
// bm_info is about this side's migration.
func (d *sideDriver) diffBitmap(
	plan *sidePlan,
	info *pb.BitmapInfo,
	revision uint64,
) {
	if plan.migrName == "" || len(plan.chunks) == 0 {
		return
	}
	if info.GetResId() != plan.migrId {
		return
	}
	missing := d.pusher.missing(
		plan.migrId, plan.chunks, info.GetBmIdxList(),
	)
	if len(missing) == 0 {
		return
	}
	d.pusher.submit(&bmPlan{
		resId:    plan.migrId,
		name:     plan.migrName,
		revision: revision,
		parts:    missing,
	})
}

// observe folds one CheckSide/SyncupSide reply into the side's health (HL2,
// HL4), the BM6 resync flag and the RW18 flip report.
func (d *sideDriver) observe(ctx context.Context, r *replyState) {
	obs, res := sideObservation(r.code, d.info())
	d.health.observe(ctx, obs, res)
	if d.pusher.takeFailed() {
		// BM6: the next round issues an equal-revision Syncup* whose reply
		// restarts the diff.
		d.host.wantResync()
	}
	d.reportProvisioned(ctx, r, d.current())
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
	if r.code != 0 || plan.req.GetSideConf().GetProvisioned() {
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
// (HL2). The in-memory info is marked RES_STATUS_UNKNOWN (§9.5) and never
// written to etcd.
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

func (s *sideCheckStream) send(revision uint64, showInfo bool) error {
	req := sideCheckRequest(
		s.driver.cid, s.driver.dnId, s.driver.ptr, revision, showInfo,
	)
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

// cntlrDriver is one cntlr's half of the per-object loop (§8.4).
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
			bmIdx uint32,
		) ([]byte, bool, error) {
			chunk := &pb.CloneBitmap{}
			found, err := d.deps.store.Get(
				ctx, model.CloneBitmapKey(d.cid, d.spId, name, bmIdx), chunk,
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
					Revision:     part.revision,
					CloneId:      part.resId,
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

// current is the plan the child drives; see sideDriver.current.
func (d *cntlrDriver) current() *cntlrPlan {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.next != nil {
		d.plan = d.next
		d.next = nil
	}
	return d.plan
}

// logAttrs are the cntlr's ids as the §12 records carry them.
func (d *cntlrDriver) logAttrs() []slog.Attr {
	return []slog.Attr{
		slog.Uint64("cluster_id", d.cid),
		slog.Uint64("cn_id", d.cnId),
		slog.Any("cntlr_pointer", common.PbToLogValue(d.ptr)),
	}
}

// childAttrs is the cntlr_pointer an sp child's lifecycle records carry (§12).
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

// openStream opens the cntlr's CheckCntlr stream (RW4 step 1, §9.7).
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
	if state.code == 0 {
		d.diffBitmaps(plan, reply.GetBmInfoList(), reply.GetRevision())
	}
	return state, nil
}

// diffBitmaps is BM2 for the clones of an SP, and BM4's target rule: only the
// PRIMARY cntlr's CN runs the dm-clones, so a standby's bm_info_list is
// ignored. A clone absent from the list has everything missing.
func (d *cntlrDriver) diffBitmaps(
	plan *cntlrPlan,
	list []*pb.BitmapInfo,
	revision uint64,
) {
	if !plan.primary || len(plan.clones) == 0 {
		return
	}
	applied := make(map[uint64][]uint32, len(list))
	for _, info := range list {
		applied[info.GetResId()] = info.GetBmIdxList()
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
			resId:    clone.id,
			name:     clone.name,
			revision: revision,
			parts:    missing,
		})
	}
}

// observe folds one CheckCntlr/SyncupCntlr reply into the cntlr's health
// (HL2 — every ERROR row of the CntlrInfo OTHER than leg_id_to_leg), the BM6
// resync flag, the RW19 created candidates and the leg rows the coordinator
// records.
func (d *cntlrDriver) observe(ctx context.Context, r *replyState) {
	info := d.info()
	obs, res := cntlrObservation(r.code, info)
	d.health.observe(ctx, obs, res)
	if d.pusher.takeFailed() {
		d.host.wantResync()
	}
	if r.code != 0 {
		// HL2/RW19: code != 0 neither sets health nor completes a td.
		return
	}
	plan := d.current()
	rep := spReport{createdTdIds: completedTds(info, plan.sliceIds)}
	if plan.primary {
		rep.legRows = d.legRows(info)
	} else {
		d.logStandbyLegRows(ctx, info, plan.cntlrId)
	}
	if len(rep.createdTdIds) == 0 && len(rep.legRows) == 0 {
		return
	}
	d.send(ctx, rep)
}

// legRows is the PRIMARY's §3.6 probe of every leg it reported, spares
// included (HL2). ERROR sets the leg's err_epoch, OK clears it, everything
// else neither.
func (d *cntlrDriver) legRows(info *pb.CntlrInfo) []legRow {
	rows := make([]legRow, 0, len(info.GetLegIdToLeg()))
	for _, legId := range sortedKeys(info.GetLegIdToLeg()) {
		// The caller has already established code == 0 (HL2).
		obs, res := legObservation(0, info, legId)
		if obs == healthNone {
			continue
		}
		rows = append(rows, legRow{legId: legId, obs: obs, resName: res})
	}
	return rows
}

// logStandbyLegRows is HL2's "a standby's leg row is logged, never recorded":
// a standby reports transport liveness and ana_state (§3.6), which says
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
// (HL2, §9.5). It reports nothing about the legs: only an ERROR row from the
// primary sets a leg's err_epoch, and an unreachable primary is the CNTLR's
// health, not the legs'.
func (d *cntlrDriver) unreachable(ctx context.Context) {
	d.markInfoUnknown()
	d.health.observe(ctx, healthUnreachable, "")
}

// markInfoUnknown is §9.5's "no answer from the node" applied to the last
// known info in place. It takes the driver's lock because the coordinator may
// be snapshotting the same message for a reaction pass (AR1).
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

func (s *cntlrCheckStream) send(revision uint64, showInfo bool) error {
	req := cntlrCheckRequest(
		s.driver.cid, s.driver.cnId, s.driver.ptr, revision, showInfo,
	)
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

// completedTds applies RW19's conditions (2), (3) and (4) — condition (1),
// code == 0, is the caller's — to one CntlrInfo, and returns the td_ids the
// reply COMPLETES (architecture.md §10.3): the thin info exists, its
// slice_id_to_dm_thin key set equals the SP's slice ids exactly (every slice,
// no extra, no missing) and every row is RES_STATUS_OK.
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

// dataBlockSize resolves the §7 default of the pool's data block size: it is
// what a migration destination's dm-clone uses as its region size (RW15) and
// what AR6 converts a meta group's data region with. The rule lives in model,
// which applies it inside the GrowSlice STM as well.
func dataBlockSize(bdevConf *pb.BdevConf) uint64 {
	return model.PoolBlockSize(bdevConf)
}

// migrCloneConf resolves the §7 defaults of a migration's dm-clone knobs
// before they are sent (RW15). The conf is copied, never patched in place: it
// belongs to the loaded state, which the coordinator hands to every child.
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
