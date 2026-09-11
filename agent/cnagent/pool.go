package cnagent

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// CN13 — per-slice pool concats and the thin-pool
// ---------------------------------------------------------------------------

// poolSegments is the concat under one of a slice's two pool devices: one
// dm-linear target per group device, in list order. A RedundNone group device
// already starts at its leg's data region and an md array starts at its
// data-offset, so every segment sits at offset 0 of its group device.
//
// Callers pass the slice's **effective** group list ([D15]): a group whose legs
// are still provisioning has no device, so it is not a target yet and the
// concat grows into it on the pass where it clears.
func poolSegments(grps []*grpPlan) []dmSegment {
	segments := make([]dmSegment, 0, len(grps))
	for _, gp := range grps {
		segments = append(segments, dmSegment{
			path:    gp.devPath,
			sectors: gp.dataSectors,
		})
	}
	return segments
}

// ensureSlice converges one slice's meta concat, data concat and thin-pool.
// GrowSlice arrives as longer group lists: the converge reloads the concat(s)
// with the appended targets and reloads the pool table with the new sizes —
// dm-thin picks up both data and metadata growth from the table swap, and no
// pool message is involved.
//
// A grown group whose sides are still provisioning is simply not in the
// effective lists yet ([D15]): the concats and the pool keep their old size, the
// serving pool keeps reporting OK with its raw `dmsetup status` details — the
// §10.4 auto-grow parses them, so PROVISIONING must never reach this row — and
// the reload happens on the pass where the group clears.
func (s *CnAgentServer) ensureSlice(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	sp *slicePlan,
	info *pb.CntlrInfo,
) bool {
	metaChanged, metaErr := s.ensureDmMulti(
		ctx, sp.poolMetaName, poolSegments(sp.effMetaGrps))
	info.SliceIdToMeta[sp.sliceId] = st.tracker.FromErr(
		resKeyOf(resKeyPoolMetaFmt, sp.sliceId), sp.poolMetaName, "", metaErr)
	dataChanged, dataErr := s.ensureDmMulti(
		ctx, sp.poolDataName, poolSegments(sp.effDataGrps))
	info.SliceIdToData[sp.sliceId] = st.tracker.FromErr(
		resKeyOf(resKeyPoolDataFmt, sp.sliceId), sp.poolDataName, "", dataErr)

	poolKey := resKeyOf(resKeyPoolFmt, sp.sliceId)
	if metaErr != nil || dataErr != nil {
		info.SliceIdToDmPool[sp.sliceId] = st.tracker.Err(
			poolKey, sp.poolFinalName, "pool concat missing")
		return false
	}
	if err := s.ensurePool(
		ctx, st, plan, sp, metaChanged || dataChanged); err != nil {
		info.SliceIdToDmPool[sp.sliceId] = st.tracker.Err(
			poolKey, sp.poolFinalName, err.Error())
		return false
	}
	// §9.5/CN28: details is the raw `dmsetup status` line — the worker parses
	// the metadata and data used/total out of it for the §10.4 auto-grow.
	raw, err := s.dm.Status(ctx, sp.poolFinalName)
	if err != nil {
		info.SliceIdToDmPool[sp.sliceId] = st.tracker.Err(
			poolKey, sp.poolFinalName, err.Error())
		return false
	}
	info.SliceIdToDmPool[sp.sliceId] = st.tracker.Ok(
		poolKey, sp.poolFinalName, raw)
	return true
}

// poolArgs is the dm thin-pool table's leading arguments. A fresh pool needs
// its metadata to read zero, and it provably does: the §9.4 protocol writes
// zeros over the whole side before its first export and opens the
// `provisioned` gate only when the last extent's bit is set ([D15]) — the same
// guarantee that funds CN12's `--assume-clean`, and the reason a recycled
// meta-group extent can no longer hand a fresh pool a previous SP's valid
// thin-metadata superblock.
func (s *CnAgentServer) poolArgs(
	ctx context.Context,
	plan *cntlrPlan,
	sp *slicePlan,
) ([]string, error) {
	metaNo, err := s.dm.DevNo(ctx, s.nf.DmPath(sp.poolMetaName))
	if err != nil {
		return nil, err
	}
	dataNo, err := s.dm.DevNo(ctx, s.nf.DmPath(sp.poolDataName))
	if err != nil {
		return nil, err
	}
	if plan.blockSectors() == 0 {
		return nil, fmt.Errorf("dm_pool_conf.data_block_size is 0")
	}
	return []string{
		metaNo,
		dataNo,
		strconv.FormatUint(plan.blockSectors(), 10),
		strconv.FormatUint(plan.lowWaterMark(sp.dataBlocks), 10),
	}, nil
}

// ensurePool converges one slice's thin-pool. concatGrew forces a reload even
// when the table is unchanged: a GrowSlice that appends meta groups only
// changes nothing in the pool's own four arguments, and dm-thin picks up a
// resized metadata or data device solely in `pool_preresume` — i.e. on a
// suspend/resume. Without it an operator's metadata grow reports success while
// the pool keeps running on the old size (CN13).
//
// The Create branch — and **only** it — arms CN14's activation sweep:
// every leak path that skipped a `delete` ends the
// pool device's life on the cntlr that skipped it, so a stray can only be met
// where the device comes back, and within one device lifetime this agent is
// the pool's only writer. The Reload and probe-matched branches arm nothing,
// which is the whole restart-safety argument: a re-converge on a converged
// node — case D of the cn suite, CN2/SH16 — must issue zero mutating calls,
// and a surviving pool device can have grown no strays while its only writer
// was down.
func (s *CnAgentServer) ensurePool(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	sp *slicePlan,
	concatGrew bool,
) error {
	args, err := s.poolArgs(ctx, plan, sp)
	if err != nil {
		return err
	}
	table := dmTable("thin-pool", sp.dataSectors, args)
	dev, err := s.dm.Info(ctx, sp.poolFinalName)
	if err != nil {
		return err
	}
	if dev == nil {
		if err := s.dm.Create(ctx, sp.poolFinalName, table); err != nil {
			return err
		}
		// Armed on success only: a pool device that never came up has no
		// metadata to enumerate. The startup reconcile's Create arms exactly
		// like any other — the reboot deferral lives at the run site, not
		// here.
		st.pendingSweep[sp.sliceId] = true
		return nil
	}
	if concatGrew {
		return s.dm.Reload(ctx, sp.poolFinalName, table)
	}
	// dm-thin appends status-derived feature arguments to its table output,
	// so only the four arguments the agent writes are compared.
	return s.ensureDmSingle(ctx, sp.poolFinalName, "thin-pool",
		sp.dataSectors, args, len(args), false)
}

// ---------------------------------------------------------------------------
// CN14 — thin volumes
// ---------------------------------------------------------------------------

// ensureThin converges one td's thin volume in one slice. The pool message
// that creates the thin device id is sent only when the dm device is absent
// *and* the td is a plain one the control plane has not seen materialized —
// `!created && ori_id == 0` (U4-S1). Both clauses are re-derivable from the
// request alone, which is what let the pre-pass handoff map go: every message
// of an uncreated snapshot belongs to the build() pre-pass, and a created td
// is never messaged by anyone.
//
// `created` means the sp-worker has seen this td's thin volume OK in every
// slice (§10.3), so the id exists in every slice pool and a bare `dmsetup
// create` attaches it. When that fails because a pool no longer holds the
// id, the row reads RES_STATUS_ERROR and no later converge messages either
// (U4-S2): pool-metadata loss surfaces as an intervention event instead of a
// fresh, empty volume silently taking over a live dev_id.
//
// For an uncreated plain td the old rule stands: a message for an id the
// pool already holds fails harmlessly and the `dmsetup create` that follows
// is what decides the outcome, which is what makes a crash between the two
// restart-safe.
func (s *CnAgentServer) ensureThin(
	ctx context.Context,
	plan *cntlrPlan,
	tp *tdPlan,
	sp *slicePlan,
) error {
	name := plan.thinName(tp.tdId, sp.sliceId)
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return err
	}
	if dev == nil && !tp.td.GetCreated() && tp.td.GetOriId() == 0 {
		if err := s.createThinId(ctx, tp, sp); err != nil {
			slog.ErrorContext(ctx, "thin-pool create message failed",
				slog.String("pool", sp.poolFinalName),
				slog.String("thin", name),
				slog.String("error", err.Error()))
		}
	}
	poolNo, err := s.dm.DevNo(ctx, s.nf.DmPath(sp.poolFinalName))
	if err != nil {
		return err
	}
	args := []string{poolNo, strconv.FormatUint(uint64(tp.td.GetDevId()), 10)}
	// dm-thin's table output may carry an external-origin argument the agent
	// never writes; compare the two arguments it does write.
	return s.ensureDmSingle(ctx, name, "thin", tp.thinSectors, args,
		len(args), false)
}

// createThinId sends one slice's `create_thin`. Its only caller is ensureThin
// and only for an uncreated plain td: an uncreated snapshot's `create_snap`
// belongs to the build() pre-pass, which is the only caller of createSnapId
// (U4-S3), and a created td of either kind is never messaged at all (U4-S2).
func (s *CnAgentServer) createThinId(
	ctx context.Context,
	tp *tdPlan,
	sp *slicePlan,
) error {
	return s.dm.Message(ctx, sp.poolFinalName, 0,
		fmt.Sprintf("create_thin %d", tp.td.GetDevId()))
}

// createSnapId sends one slice's `create_snap`. dm-thin requires a quiesced
// origin: when the origin's per-slice thin volume device is live the agent
// suspends it across the message and resumes immediately after. That is a
// second deliberate, bounded suspension beyond [D12]'s window, held only for
// the duration of one `dmsetup message`, and it stays nested *inside* the
// origin raid0 quiesce (CN14) wraps around the whole per-slice sequence.
func (s *CnAgentServer) createSnapId(
	ctx context.Context,
	plan *cntlrPlan,
	tp *tdPlan,
	sp *slicePlan,
) error {
	devId := tp.td.GetDevId()
	oriId := tp.td.GetOriId()
	message := fmt.Sprintf("create_snap %d %d", devId, oriId)
	var oriName string
	if origin := plan.tdByDevId[oriId]; origin != nil {
		oriName = plan.thinName(origin.tdId, sp.sliceId)
	}
	if oriName == "" {
		return s.dm.Message(ctx, sp.poolFinalName, 0, message)
	}
	oriDev, err := s.dm.Info(ctx, oriName)
	if err != nil {
		return err
	}
	if oriDev == nil || oriDev.Suspended {
		return s.dm.Message(ctx, sp.poolFinalName, 0, message)
	}
	if err := s.dm.Suspend(ctx, oriName); err != nil {
		return err
	}
	messageErr := s.dm.Message(ctx, sp.poolFinalName, 0, message)
	if err := s.dm.Resume(ctx, oriName); err != nil {
		slog.ErrorContext(ctx, "resuming the snapshot origin failed",
			slog.String("thin", oriName),
			slog.String("error", err.Error()))
	}
	return messageErr
}

// The activation sweep's two log records (CN14). They are named
// once here because the sweep writes one of them at each of its three exits,
// and an operator greps for the pair — "did the sweep run, and did it get
// through" — after every pool re-creation.
const (
	msgThinSweep       = "thin id sweep"
	msgThinSweepFailed = "thin id sweep failed"
)

// sweepThinIds is CN14's activation sweep: enumerate the
// armed pool's device ids and delete every id no td of td_list owns. Runs at
// most once per pool-device creation, only under an RPC-delivered request
// (the call site's reqFromRpc gate); a failure leaves pendingSweep set so a
// later converge retries.
//
// It is correct to delete against this td_list and no other: GateRevision
// makes an RPC-delivered request the newest desired state this cntlr has ever
// accepted, dev_ids are never reused (SpConf.next_dev_id), thin ids belong
// exclusively to tds — clones and transfers own none — and DN-side fencing
// leaves no other writer, so a pool id absent from it can only belong to a td
// deleted at or before that revision.
//
// deleteThinId is deliberately not reused: it swallows the message error by
// design (CN14's fire-and-forget retire), while the sweep must see a failure
// to keep the flag alive for the retry.
func (s *CnAgentServer) sweepThinIds(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	sp *slicePlan,
) {
	// CN25's machinery: reserve_metadata_snap → thin_dump --metadata-snap →
	// release_metadata_snap, releasing on every path.
	sb, err := s.dumpThinMetadata(ctx, sp)
	if err != nil {
		slog.ErrorContext(ctx, msgThinSweepFailed,
			slog.String("pool", sp.poolFinalName),
			slog.String("error", err.Error()))
		return
	}
	var strays []uint32
	for _, dev := range sb.Devices {
		if plan.tdByDevId[dev.DevId] == nil {
			strays = append(strays, dev.DevId)
		}
	}
	for _, devId := range strays {
		if err := s.dm.Message(ctx, sp.poolFinalName, 0,
			fmt.Sprintf("delete %d", devId)); err != nil {
			slog.ErrorContext(ctx, msgThinSweepFailed,
				slog.String("pool", sp.poolFinalName),
				slog.Uint64("dev_id", uint64(devId)),
				slog.String("error", err.Error()))
			return
		}
	}
	// Only a full pass disarms: anything short of it leaves the slice
	// sweep-pending for the next converge under an RPC-delivered request.
	delete(st.pendingSweep, sp.sliceId)
	slog.InfoContext(ctx, msgThinSweep,
		slog.String("pool", sp.poolFinalName),
		slog.Any("deleted", strays))
}

// deleteThinId is the CN14 **deletion** path — a td that left td_list. A
// cntlr teardown (CN21) never sends it: the pool metadata lives on the DN
// legs, and the next hosting CN must find the thin volumes intact.
func (s *CnAgentServer) deleteThinId(
	ctx context.Context,
	sp *slicePlan,
	devId uint32,
) {
	pool, err := s.dm.Info(ctx, sp.poolFinalName)
	if err != nil || pool == nil {
		return
	}
	if err := s.dm.Message(ctx, sp.poolFinalName, 0,
		fmt.Sprintf("delete %d", devId)); err != nil {
		slog.ErrorContext(ctx, "thin-pool delete message failed",
			slog.String("pool", sp.poolFinalName),
			slog.String("error", err.Error()))
	}
}

// ---------------------------------------------------------------------------
// Probes
// ---------------------------------------------------------------------------

func (s *CnAgentServer) probePool(
	ctx context.Context,
	plan *cntlrPlan,
	sp *slicePlan,
) (pb.ResStatus, string) {
	dev, err := s.dm.Info(ctx, sp.poolFinalName)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if dev == nil {
		return pb.ResStatus_RES_STATUS_MISSING, ""
	}
	args, err := s.poolArgs(ctx, plan, sp)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	targets, err := s.dm.Table(ctx, sp.poolFinalName)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if !dmTableMatches(
		targets, "thin-pool", sp.dataSectors, args, len(args)) {
		return pb.ResStatus_RES_STATUS_ERROR, "table is not the desired pool"
	}
	raw, err := s.dm.Status(ctx, sp.poolFinalName)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	return pb.ResStatus_RES_STATUS_OK, raw
}

func (s *CnAgentServer) probeThin(
	ctx context.Context,
	plan *cntlrPlan,
	tp *tdPlan,
	sp *slicePlan,
) (pb.ResStatus, string) {
	name := plan.thinName(tp.tdId, sp.sliceId)
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if dev == nil {
		return pb.ResStatus_RES_STATUS_MISSING, ""
	}
	poolNo, err := s.dm.DevNo(ctx, s.nf.DmPath(sp.poolFinalName))
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	args := []string{poolNo, strconv.FormatUint(uint64(tp.td.GetDevId()), 10)}
	return s.probeDmArgs(ctx, name, "thin", tp.thinSectors, args, len(args))
}
