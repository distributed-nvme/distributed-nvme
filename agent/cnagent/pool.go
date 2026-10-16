package cnagent

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// CN13 — per-slice pool concats and the thin-pool
// ---------------------------------------------------------------------------

// poolSegments is the concat under one of a slice's two pool devices: one
// dm-linear target per group device, in list order. A RedundNone group device
// already starts at its leg's data region and an md array starts at its
// data-offset, so every segment sits at offset 0 of its group device. A
// segment carries its group, by which buildConcat reads the device's number.
//
// Callers pass the slice's **effective** group list ([D15]): a group whose legs
// are still provisioning has no device, so it is not a target yet and the
// concat grows into it on the pass where it clears.
func poolSegments(grps []*grpPlan) []dmSegment {
	segments := make([]dmSegment, 0, len(grps))
	for _, gp := range grps {
		segments = append(segments, dmSegment{
			grp:     gp,
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
// thin-pool auto-grow (dnv-worker.md AR6) parses them, so
// PROVISIONING must never reach this row — and
// the reload happens on the pass where the group clears.
func (s *CnAgentServer) ensureSlice(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	sp *slicePlan,
	info *pb.CntlrInfo,
) bool {
	metaChanged, metaErr := s.ensureDmMulti(
		ctx, plan, sp.poolMetaName, poolSegments(sp.effMetaGrps))
	info.SliceIdToMeta[sp.sliceId] = st.tracker.FromErr(
		resKeyOf(resKeyPoolMetaFmt, sp.sliceId), sp.poolMetaName, "", metaErr)
	dataChanged, dataErr := s.ensureDmMulti(
		ctx, plan, sp.poolDataName, poolSegments(sp.effDataGrps))
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
	// CN28 (architecture.md, Live-state reporting): details is the raw
	// `dmsetup status` line — the worker parses the metadata and data
	// used/total out of it for the thin-pool auto-grow (dnv-worker.md
	// AR6). The read carries the noflush flag, so it commits nothing
	// (dnagent.md SH17).
	raw, err := s.dm.StatusNoFlush(ctx, sp.poolFinalName)
	if err != nil {
		info.SliceIdToDmPool[sp.sliceId] = st.tracker.Err(
			poolKey, sp.poolFinalName, err.Error())
		return false
	}
	info.SliceIdToDmPool[sp.sliceId] = st.tracker.Ok(
		poolKey, sp.poolFinalName, raw)
	return true
}

// poolArgs is the dm thin-pool table's arguments: these four, and nothing
// after them. With no feature arguments dm-thin writes every block it
// provisions whole before a host can read any of it — a partial first write
// zeroes the rest of the block — and that is what keeps the old bytes the
// provisioning protocol leaves in a data group's data region from ever
// reaching a host of this pool. So the table never asks dm-thin to skip block
// zeroing ([D15]; cnagent.md CN13).
//
// A fresh pool also needs its metadata to read zero, and it does: a meta
// group's sides are zeroed over the whole leg before their first export, and
// their `provisioned` gate opens only once that is done (architecture.md,
// Side provisioning protocol) — the reason a recycled meta-group extent
// cannot hand a fresh pool a previous SP's valid thin-metadata superblock.
func (s *CnAgentServer) poolArgs(
	ctx context.Context,
	plan *cntlrPlan,
	sp *slicePlan,
) ([]string, error) {
	metaNo, err := s.dmDevNo(ctx, plan.rd, sp.poolMetaName)
	if err != nil {
		return nil, err
	}
	dataNo, err := s.dmDevNo(ctx, plan.rd, sp.poolDataName)
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
// The Create branch — and **only** it — arms CN14's activation sweep. The
// fan-outs that skip a `delete` on every cntlr (a demote and a delete
// coalesced into one revision, a delete at a pool-suppressing sp_level, a
// failover and delete race) end the pool device's life on the cntlr that
// skipped it, so their strays are met where the device comes back. A stray
// left in a live pool device arms nothing: it waits for the next sweep of
// that pool's ids that gets through — the one after a later failed delete in
// that pool, or an activation sweep (CN14). Within one device lifetime this
// agent is the pool's only writer. The Reload and probe-matched branches arm
// nothing, which is the whole restart-safety argument: a re-converge on a
// converged node — case D of the cn suite, CN2/SH16 — must issue zero
// mutating calls, and a surviving pool device can have grown no strays while
// its only writer was down.
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
		sp.dataSectors, args, len(args))
}

// ---------------------------------------------------------------------------
// CN14 — thin volumes
// ---------------------------------------------------------------------------

// ensureThin converges one td's thin volume in one slice. The pool message
// that creates the thin device id is sent only when the dm device is absent
// *and* the td is a plain one the control plane has not seen materialized —
// `!created && ori_id == 0` (CN14). Both clauses are re-derivable from the
// request alone: every message of an uncreated snapshot belongs to the
// build() pre-pass, and a created td is never messaged by anyone.
//
// `created` means the sp-worker has seen this td's thin volume OK in every
// slice (dnv-worker.md RW19), so the id exists in every slice pool and a
// bare `dmsetup
// create` attaches it. When that fails because a pool no longer holds the
// id, the row reads RES_STATUS_ERROR with the reason RES_REASON_THIN_ID_LOST
// (thinRowOf) and no later converge messages either (CN14): pool-metadata
// loss surfaces as an intervention event instead of a fresh, empty volume
// silently taking over a live dev_id.
//
// For an uncreated plain td the rule is: a message for an id the
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
	poolNo, err := s.dmDevNo(ctx, nil, sp.poolFinalName)
	if err != nil {
		return err
	}
	args := []string{poolNo, strconv.FormatUint(uint64(tp.td.GetDevId()), 10)}
	// dm-thin's table output may carry an external-origin argument the agent
	// never writes; compare the two arguments it does write.
	return s.ensureDmSingle(ctx, name, "thin", tp.thinSectors, args,
		len(args))
}

// thinRowOf is the converge's row for one slice's thin volume of a td, from
// what ensureThin returned. It is FromErr's row, except for a created td
// whose thin table the kernel refused with ENODATA: dm-thin answers so when
// the slice's pool does not hold the id, and for a created td that is the
// lost id of CN14, so the row reads RES_STATUS_ERROR with the dmsetup output
// in its details and the reason RES_REASON_THIN_ID_LOST. The worker's class
// for a lost id reads the reason and never the details (CN29;
// dnv-worker.md HL2). The errno comes from the refusal's type
// (agent.IsNoData). An uncreated td's ENODATA is no lost id — a snapshot
// whose create_snap has not made its id yet reads the same — and keeps no
// reason. Only a converge names a lost id: a check round reads the absent
// volume RES_STATUS_MISSING (probeThin).
func thinRowOf(
	st *cntlrState,
	tp *tdPlan,
	key string,
	name string,
	err error,
) *pb.ResInfo {
	if err != nil && tp.td.GetCreated() && agent.IsNoData(err) {
		return st.tracker.ErrReason(key, name, err.Error(),
			pb.ResReason_RES_REASON_THIN_ID_LOST)
	}
	return st.tracker.FromErr(key, name, "", err)
}

// createThinId sends one slice's `create_thin`. Its only caller is ensureThin
// and only for an uncreated plain td: an uncreated snapshot's `create_snap`
// belongs to the build() pre-pass, which is the only caller of createSnapId
// (CN14, case 3), and a created td of either kind is never messaged at all
// (CN14).
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
// the duration of one `dmsetup message` short of a dmsetup command on the
// origin that fails (cnagent.md CN16), and it stays nested *inside* the
// origin raid0 quiesce (CN14) wraps around the whole per-slice sequence. The
// message's refusal carries its errno in its type (agent.CmdError): EEXIST
// says the slice's pool holds the id already, which the pre-pass reads for
// CN14's torn mark (snapshotPrePass).
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

// The thin id sweep's two log records (CN14). They are named once here
// because every exit of the sweep writes one of them, and an operator greps
// for the pair — "did the sweep run, and did it get through" — after every
// pool re-creation and every failed delete message.
const (
	msgThinSweep       = "thin id sweep"
	msgThinSweepFailed = "thin id sweep failed"
)

// sweepThinIds is CN14's thin id sweep: enumerate one pool's device ids and
// delete every id that no td of td_list owns and keep does not name. Two runs
// use it, each only under an RPC-delivered request (the call sites'
// reqFromRpc gates): the activation sweep, at most once per pool-device
// creation, which passes no keep; and the sweep after an L7 delete message
// failed (sweepAfterFailedDeletes), whose keep reads the ids the pass's thin
// volumes still map. keep runs only once the dump is in, so a pool whose
// dump fails costs none of its reads, and an error keep returns stops the
// sweep.
// Either one that gets through ends the slice's pending activation sweep; a
// failure leaves pendingSweep as it is, so a pending one is retried by a
// later converge.
//
// It is correct to delete against this td_list and no other: GateRevision
// makes an RPC-delivered request the newest desired state this cntlr has ever
// accepted, dev_ids are never reused (SpConf.next_dev_id; architecture.md,
// Known limits, names the etcd restore that can break that), thin ids belong
// exclusively to tds — clones and transfers own none — and DN-side fencing
// leaves no other writer, so a pool id absent from it can only belong to a td
// deleted at or before that revision.
//
// Its deletions are its own messages, not L7's (deleteThinIdByName,
// sweep.go): it must see each failure, to keep its arming alive, and it
// stops at the first.
func (s *CnAgentServer) sweepThinIds(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	sp *slicePlan,
	keep func() (map[uint32]struct{}, error),
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
	var keepIds map[uint32]struct{}
	if keep != nil {
		if keepIds, err = keep(); err != nil {
			slog.ErrorContext(ctx, msgThinSweepFailed,
				slog.String("pool", sp.poolFinalName),
				slog.String("error", err.Error()))
			return
		}
	}
	var strays []uint32
	for _, dev := range sb.Devices {
		if _, kept := keepIds[dev.DevId]; kept {
			continue
		}
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

// ---------------------------------------------------------------------------
// Probes
// ---------------------------------------------------------------------------

func (s *CnAgentServer) probePool(
	ctx context.Context,
	plan *cntlrPlan,
	sp *slicePlan,
) (pb.ResStatus, string) {
	dev, err := s.dmInfo(ctx, plan.rd, sp.poolFinalName)
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
	targets, err := s.dmTable(ctx, plan.rd, sp.poolFinalName)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if !dmTableMatches(
		targets, "thin-pool", sp.dataSectors, args, len(args)) {
		return pb.ResStatus_RES_STATUS_ERROR, "table is not the desired pool"
	}
	// The same noflush read as the converge's (dnagent.md SH17).
	raw, err := s.dm.StatusNoFlush(ctx, sp.poolFinalName)
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
	dev, err := s.dmInfo(ctx, plan.rd, name)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	if dev == nil {
		return pb.ResStatus_RES_STATUS_MISSING, ""
	}
	poolNo, err := s.dmDevNo(ctx, plan.rd, sp.poolFinalName)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error()
	}
	args := []string{poolNo, strconv.FormatUint(uint64(tp.td.GetDevId()), 10)}
	return s.probeDmArgs(
		ctx, plan.rd, name, "thin", tp.thinSectors, args, len(args))
}
