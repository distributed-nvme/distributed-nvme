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
// Callers pass the slice's **effective** group list (U4): a group whose legs
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
// effective lists yet (U4): the concats and the pool keep their old size, the
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
		ctx, plan, sp, metaChanged || dataChanged); err != nil {
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
func (s *CnAgentServer) ensurePool(
	ctx context.Context,
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
		return s.dm.Create(ctx, sp.poolFinalName, table)
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
// that creates the thin device id is sent only when the dm device is absent;
// a message for an id the pool already holds fails harmlessly and the
// `dmsetup create` that follows is what decides the outcome, which is what
// makes a crash between the two restart-safe.
//
// snapDone carries the thin names whose `create_snap` the U1 pre-pass of
// build() already sent inside the origin raid0's quiesce. It is a handoff,
// never a re-derivable predicate: by the time this runs the origin td's own
// ensureThin has created the origin thin volume, so any recomputed "was the
// pre-pass able to claim this slice?" test would answer yes for a slice the
// pre-pass declined — and that slice's message would be lost outright.
func (s *CnAgentServer) ensureThin(
	ctx context.Context,
	plan *cntlrPlan,
	tp *tdPlan,
	sp *slicePlan,
	snapDone map[string]bool,
) error {
	name := plan.thinName(tp.tdId, sp.sliceId)
	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return err
	}
	if dev == nil && !snapDone[name] {
		if err := s.createThinId(ctx, plan, tp, sp); err != nil {
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

// createThinId sends `create_thin` — or hands a snapshot td to createSnapId.
//
// U1: for a snapshot the build() pre-pass normally owns the message, and
// suppresses this call through its snapDone set. Reaching createSnapId from
// here is the fallback for a slice the pre-pass declined — no origin td in
// the plan, or an origin whose own thin volume does not exist yet — where
// there is nothing to quiesce, and where staying on this lazy path is what
// keeps `create_snap` behind the origin's own `create_thin` in the td loop.
func (s *CnAgentServer) createThinId(
	ctx context.Context,
	plan *cntlrPlan,
	tp *tdPlan,
	sp *slicePlan,
) error {
	if tp.td.GetOriId() != 0 {
		return s.createSnapId(ctx, plan, tp, sp)
	}
	return s.dm.Message(ctx, sp.poolFinalName, 0,
		fmt.Sprintf("create_thin %d", tp.td.GetDevId()))
}

// createSnapId sends one slice's `create_snap`. dm-thin requires a quiesced
// origin: when the origin's per-slice thin volume device is live the agent
// suspends it across the message and resumes immediately after. That is a
// second deliberate, bounded suspension beyond [D12]'s window, held only for
// the duration of one `dmsetup message`, and it stays nested *inside* the
// origin raid0 quiesce U1 wraps around the whole per-slice sequence.
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
