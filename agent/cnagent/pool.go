package cnagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
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
// with the appended targets and then reloads the pool. A data grow changes
// the pool table — the data length and the low-water mark — while a metadata
// grow changes nothing in it, since the table names the metadata device and
// does not size it; dm-thin takes the data size from the table's length and
// reads the metadata device's size only at the resume, so the pool is
// reloaded even over an unchanged table (concatGrew), and no pool message is
// involved (cnagent.md CN13).
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
	// A concat whose only failure is the clear of its read-only flag is
	// there, resumed and on its table, so the pool is built over it; its own
	// row reads the failure (stopsOver).
	if stopsOver(metaErr) || stopsOver(dataErr) {
		info.SliceIdToDmPool[sp.sliceId] = st.tracker.Err(
			poolKey, sp.poolFinalName, "pool concat missing")
		return false
	}
	if err := s.ensurePool(
		ctx, st, plan, sp, metaChanged || dataChanged); err != nil {
		var damaged *thinMetaDamaged
		switch {
		case errors.Is(err, errThinChecking), errors.As(err, &damaged):
			// CN13, The needs-check mark: the check's own row.
			info.SliceIdToDmPool[sp.sliceId] = thinCheckRow(st, sp)
		default:
			info.SliceIdToDmPool[sp.sliceId] = st.tracker.Err(
				poolKey, sp.poolFinalName, err.Error())
		}
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
	// The row by the pool's mode, as the read reads it (poolHealth). A pool
	// that does not write is still a pool: the thin volumes over it report
	// their own state, so the converge goes on over it.
	status, reason := poolHealth(raw, sp.metaSectors)
	info.SliceIdToDmPool[sp.sliceId] = st.tracker.SetReason(
		poolKey, sp.poolFinalName, status, raw, reason)
	return true
}

// poolHealth is CN13's pool row from one `dmsetup status` line of a pool
// whose table is as wanted, the converge's and the read's alike:
//
//   - a line that carries `Fail` or `Error` in place of its numbers
//     (common.ThinPoolFault) reads RES_STATUS_ERROR with
//     RES_REASON_POOL_FAILED: dm-thin failed the loaded pool, or could not
//     read its metadata. The failure lives in the loaded pool alone, so it
//     is the cntlr's own: a new primary opens the pool from its last commit.
//   - a line with needs_check reads RES_STATUS_ERROR with
//     RES_REASON_POOL_NEEDS_CHECK: dm-thin keeps the loaded pool read-only
//     and set the mark in the pool's metadata. The row is the cntlr's own
//     (dnv-worker.md HL2): a new primary checks the metadata before it
//     loads the pool and clears the mark when the check passes (CN13, The
//     needs-check mark).
//   - any other line reads RES_STATUS_OK: a pool that writes, one out of
//     data space, and one read-only for want of metadata space, whose cure
//     is the metadata auto-grow (dnv-worker.md AR6), which an error row
//     would stop; with RES_REASON_DIFFERS while it is short of its metadata
//     device (poolMetaShort) or holds a metadata snapshot the converge
//     releases (poolHeldSnap), so the verdict names it (CN30).
//
// The details are the raw line in every case.
func poolHealth(raw string, metaSectors uint64) (pb.ResStatus, pb.ResReason) {
	if common.ThinPoolFault(raw) != "" {
		return pb.ResStatus_RES_STATUS_ERROR,
			pb.ResReason_RES_REASON_POOL_FAILED
	}
	if st, ok := common.ParseThinPoolStatus(raw); ok && st.NeedsCheck {
		return pb.ResStatus_RES_STATUS_ERROR,
			pb.ResReason_RES_REASON_POOL_NEEDS_CHECK
	}
	if poolMetaShort(raw, metaSectors) || poolHeldSnap(raw) {
		return pb.ResStatus_RES_STATUS_OK, pb.ResReason_RES_REASON_DIFFERS
	}
	return pb.ResStatus_RES_STATUS_OK, pb.ResReason_RES_REASON_NONE
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
// the pool keeps running on the old size (CN13). A pass that grew the
// metadata concat and stopped before the pool's reload leaves a pool whose
// table matches; the next pass finds it by the pool's own status, its total
// metadata blocks short of what dm-thin takes from the concat
// (poolMetaShort), and reloads it then.
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
		// CN13, The needs-check mark: the metadata is checked before the
		// pool is loaded, while it carries dm-thin's mark.
		if err := s.thinCheckGate(ctx, st, plan, sp); err != nil {
			return err
		}
		if err := s.dm.Create(ctx, sp.poolFinalName, table); err != nil {
			return err
		}
		// Armed on success only: a pool device that never came up has no
		// metadata to enumerate. The startup reconcile's Create arms exactly
		// like any other — the reboot deferral lives at the run site, not
		// here.
		st.pendingSweep[sp.sliceId] = true
		// dm-thin keeps a held metadata snapshot in the pool's metadata,
		// so a pool opened on a new primary or after a reboot can come up
		// holding one.
		if raw, err := s.dm.StatusNoFlush(ctx, sp.poolFinalName); err == nil {
			s.releaseHeldSnap(ctx, sp, raw)
		}
		return nil
	}
	if concatGrew {
		return s.dm.Reload(ctx, sp.poolFinalName, table)
	}
	// A status that cannot be read is no reason to reload: the row's own
	// status read reports it after this.
	raw, err := s.dm.StatusNoFlush(ctx, sp.poolFinalName)
	if err == nil {
		s.releaseHeldSnap(ctx, sp, raw)
	}
	if err == nil && poolMetaShort(raw, sp.metaSectors) {
		slog.InfoContext(ctx, "thin-pool metadata short of its device",
			slog.String("pool", sp.poolFinalName),
			slog.String("status", raw),
			slog.Uint64("meta_sectors", sp.metaSectors))
		return s.dm.Reload(ctx, sp.poolFinalName, table)
	}
	// dm-thin appends status-derived feature arguments to its table output,
	// so only the four arguments the agent writes are compared.
	return s.ensureDmSingle(ctx, sp.poolFinalName, "thin-pool",
		sp.dataSectors, args, len(args))
}

// releaseHeldSnap releases the metadata snapshot the pool holds when its
// status line raw shows one held (poolHeldSnap). A metadata snapshot a
// converge finds held is no live reader's: every reserve of the pool runs
// under this cntlr's object lock or inside its converge (CN1, CN14, CN25),
// so it is one an agent died holding, or whose release failed. Held, it pins
// the pool's metadata blocks until some later reserve meets it, which may
// never come (CN13). A release that fails is logged, and the pool's row then
// names it again.
func (s *CnAgentServer) releaseHeldSnap(
	ctx context.Context,
	sp *slicePlan,
	raw string,
) {
	if !poolHeldSnap(raw) {
		return
	}
	slog.InfoContext(ctx, "thin-pool holds a metadata snapshot",
		slog.String("pool", sp.poolFinalName),
		slog.String("status", raw))
	s.releaseMetadataSnap(ctx, sp)
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
// For an uncreated plain td the rule is: a message that makes the id is
// followed by the `dmsetup create`. A message that fails for any cause — the
// pool holds the id already (EEXIST, agent.IsExists), after a crash between
// the message and the create or a message another primary sent; the pool
// takes no message (EOPNOTSUPP, read-only, out of metadata space or under
// needs_check), while dm-thin still loads a thin table over an id it holds;
// a message the soft timeout killed, which the kernel may have carried out
// — is followed by the pool's device headers (adoptThinId): the volume is
// built over the id only while they show it mapping no block, or not held
// at all, when the `dmsetup create` decides; an id that holds data is
// another volume's, after an etcd restore handed the id out again, and a
// new volume over it would serve that volume's data. A header read that
// fails refuses the id, save one refused because a metadata snapshot is
// already held, which makes the volume wait for the worker's re-send. A
// device already there is adopted as it is: it is this node's own earlier
// build. A created td is never messaged, so the guard never runs for the
// pool's own created volume.
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
			if err := s.adoptThinId(ctx, plan, tp, sp); err != nil {
				return err
			}
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
// reason. Only a converge names a lost id: a read reads the absent
// volume RES_STATUS_MISSING (probeThin).
//
// A new volume's build that adoptThinId stopped reads its outcome: an id
// the agent does not adopt (thinIdNotAdopted) reads RES_STATUS_ERROR with
// the reason RES_REASON_THIN_ID_NOT_ADOPTED, which the verdict does not name
// (CN30) and the worker's shared-state class reads (dnv-worker.md HL2); a
// header read that waits for a metadata snapshot already held
// (thinHeadersHeld) reads RES_STATUS_MISSING with the refusal's text and no
// reason, which the verdict names, so the worker's re-send tries again.
func thinRowOf(
	st *cntlrState,
	tp *tdPlan,
	key string,
	name string,
	err error,
) *pb.ResInfo {
	var refused *thinIdNotAdopted
	var held *thinHeadersHeld
	switch {
	case err != nil && tp.td.GetCreated() && agent.IsNoData(err):
		return st.tracker.ErrReason(key, name, err.Error(),
			pb.ResReason_RES_REASON_THIN_ID_LOST)
	case errors.As(err, &refused):
		return st.tracker.ErrReason(key, name, err.Error(),
			pb.ResReason_RES_REASON_THIN_ID_NOT_ADOPTED)
	case errors.As(err, &held):
		return st.tracker.Missing(key, name, err.Error())
	}
	return st.tracker.FromErr(key, name, "", err)
}

// thinIdNotAdopted is adoptThinId's refusal of a thin id for a new volume
// whose create failed (CN14): the slice's pool holds the id and maps blocks
// to it, or the header read that would tell failed other than for a
// metadata snapshot already held. Its text says which.
type thinIdNotAdopted struct {
	text string
}

func (e *thinIdNotAdopted) Error() string {
	return e.text
}

// thinHeadersHeld is adoptThinId's wait: the slice's header read was refused
// because a metadata snapshot is already held, which a release ends (CN14).
type thinHeadersHeld struct {
	err error
}

func (e *thinHeadersHeld) Error() string {
	return "the thin create failed; its pool's header read waits for a " +
		"held metadata snapshot: " + e.err.Error()
}

func (e *thinHeadersHeld) Unwrap() error {
	return e.err
}

// thinHeaderRead is one slice's header read of a pass (cntlrPlan.
// thinHeaders): the blocks each thin id the pool holds maps, or the read's
// error.
type thinHeaderRead struct {
	mapped map[uint32]uint64
	err    error
}

// adoptThinId decides whether a new plain volume may be built over its thin
// id after its create message failed, whatever the failure (CN14): nil when
// the pool's device headers show the id mapping no block, or not holding it
// at all, so the `dmsetup create` that follows decides; a thinIdNotAdopted
// when the id
// maps blocks, or when the header read failed for any cause but a metadata
// snapshot already held; a thinHeadersHeld for that one. The header read
// (readThinHeaders) runs once per slice per pass, at the first such volume
// of the slice, and serves every other one (thinHeadersOf). Reason for the
// headers: no device is there to read the id from, and a full metadata dump
// walks every mapping of the pool under the command limit, which on a large
// pool would never let the agent adopt even its own empty id.
func (s *CnAgentServer) adoptThinId(
	ctx context.Context,
	plan *cntlrPlan,
	tp *tdPlan,
	sp *slicePlan,
) error {
	devId := tp.td.GetDevId()
	read := s.thinHeadersOf(ctx, plan, sp)
	switch {
	case read.err != nil && (agent.IsBusy(read.err) ||
		agent.IsExists(read.err)):
		return &thinHeadersHeld{err: read.err}
	case read.err != nil:
		return &thinIdNotAdopted{text: fmt.Sprintf(
			"the create of thin id %d failed, and its pool's header read "+
				"failed: %v", devId, read.err)}
	}
	if mapped := read.mapped[devId]; mapped > 0 {
		return &thinIdNotAdopted{text: fmt.Sprintf(
			"thin id %d exists in pool %s and maps %d blocks; "+
				"not adopted for a new volume",
			devId, sp.poolFinalName, mapped)}
	}
	return nil
}

// thinHeadersOf is the pass's header read of one slice's pool (cntlrPlan.
// thinHeaders), taken at its first call in the pass.
func (s *CnAgentServer) thinHeadersOf(
	ctx context.Context,
	plan *cntlrPlan,
	sp *slicePlan,
) *thinHeaderRead {
	if read := plan.thinHeaders[sp.sliceId]; read != nil {
		return read
	}
	if plan.thinHeaders == nil {
		plan.thinHeaders = make(map[uint64]*thinHeaderRead)
	}
	mapped, err := s.readThinHeaders(ctx, sp)
	read := &thinHeaderRead{mapped: mapped, err: err}
	plan.thinHeaders[sp.sliceId] = read
	return read
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
// CN14's torn mark (snapshotPrePass). sent says the message was sent, so
// that an error the message never answered (agent.Reported) — the soft
// timeout killed it, and the kernel may have made the id — is told from a
// failed read or suspend of the origin, after which no message went.
func (s *CnAgentServer) createSnapId(
	ctx context.Context,
	plan *cntlrPlan,
	tp *tdPlan,
	sp *slicePlan,
) (sent bool, err error) {
	devId := tp.td.GetDevId()
	oriId := tp.td.GetOriId()
	message := fmt.Sprintf("create_snap %d %d", devId, oriId)
	var oriName string
	if origin := plan.tdByDevId[oriId]; origin != nil {
		oriName = plan.thinName(origin.tdId, sp.sliceId)
	}
	if oriName == "" {
		return true, s.dm.Message(ctx, sp.poolFinalName, 0, message)
	}
	oriDev, err := s.dm.Info(ctx, oriName)
	if err != nil {
		return false, err
	}
	if oriDev == nil || oriDev.Suspended {
		return true, s.dm.Message(ctx, sp.poolFinalName, 0, message)
	}
	if err := s.dm.Suspend(ctx, oriName); err != nil {
		return false, err
	}
	messageErr := s.dm.Message(ctx, sp.poolFinalName, 0, message)
	// The resume survives a cancelled call, as the pre-pass's resume of the
	// origin's raid0 does (snapshotPrePass): a cancelled ctx would run no
	// command and leave the origin's thin volume suspended.
	if err := s.dm.Resume(context.WithoutCancel(ctx), oriName); err != nil {
		slog.ErrorContext(ctx, "resuming the snapshot origin failed",
			slog.String("thin", oriName),
			slog.String("error", err.Error()))
	}
	return true, messageErr
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
// use it, each only under a request that counts as delivered (the call
// sites' reqFromRpc gates): the activation sweep, at most once per
// pool-device creation, which passes no keep; and the sweep after an L7
// delete message failed (sweepAfterFailedDeletes), whose keep reads the ids
// the pass's thin volumes still map. keep runs only once the dump is in, so a
// pool whose dump fails costs none of its reads, and an error keep returns
// stops the sweep.
// Either one that gets through ends the slice's pending activation sweep; a
// failure leaves pendingSweep as it is, so a pending one is retried by a
// later converge.
//
// It is correct to delete against this td_list: GateRevision makes a
// request that counts as delivered the newest desired state this
// cntlr has ever accepted, dev_ids are never reused (SpConf.next_dev_id;
// architecture.md, Known limits, names the etcd restore that can break
// that), thin ids belong exclusively to tds — clones and transfers own none —
// and DN-side fencing leaves no other writer, so a pool id absent from it can
// only belong to a td deleted at or before that revision.
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
	// release_metadata_snap, releasing after every reserve that succeeded or
	// did not answer.
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
	// sweep-pending for the next converge under a request that counts as
	// delivered.
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
) (pb.ResStatus, string, pb.ResReason) {
	const none = pb.ResReason_RES_REASON_NONE
	dev, err := s.dmInfo(ctx, plan.rd, sp.poolFinalName)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error(), none
	}
	if dev == nil {
		return pb.ResStatus_RES_STATUS_MISSING, "", none
	}
	args, err := s.poolArgs(ctx, plan, sp)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error(), none
	}
	targets, err := s.dmTable(ctx, plan.rd, sp.poolFinalName)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error(), none
	}
	if !dmTableMatches(
		targets, "thin-pool", sp.dataSectors, args, len(args)) {
		return pb.ResStatus_RES_STATUS_ERROR, "table is not the desired pool",
			none
	}
	// A suspended pool serves no IO (detailsSuspended).
	if dev.Suspended {
		return pb.ResStatus_RES_STATUS_ERROR, detailsSuspended, none
	}
	// The same noflush read as the converge's (dnagent.md SH17).
	raw, err := s.dm.StatusNoFlush(ctx, sp.poolFinalName)
	if err != nil {
		return pb.ResStatus_RES_STATUS_ERROR, err.Error(), none
	}
	// The row by the pool's mode, as the converge reads it (poolHealth). A
	// pool short of its metadata device serves; the reason makes the
	// verdict name the row, so the worker re-sends the SyncupCntlr whose
	// converge reloads it (ensurePool; CN30). The details stay the raw
	// status line, which the worker's auto-grow parses (dnv-worker.md AR6).
	status, reason := poolHealth(raw, sp.metaSectors)
	return status, raw, reason
}

// poolMetaShort reports whether a thin pool's status line shows fewer total
// metadata blocks than dm-thin takes from a metadata device of metaSectors,
// the slice's metadata concat (common.ThinMetaBlocks): the state a pass
// leaves that grew the concat and stopped before the pool's reload, as only a
// resume makes dm-thin take a larger device (CN13). It is the kernel's own
// arithmetic, so a pool that took its device is never short. A line that
// does not parse, a failed pool's among them, is not short, and neither is
// one with needs_check: dm-thin grows no metadata device then, and a reload
// would change nothing.
func poolMetaShort(raw string, metaSectors uint64) bool {
	st, ok := common.ParseThinPoolStatus(raw)
	if !ok || st.NeedsCheck {
		return false
	}
	return st.TotalMetaBlocks < common.ThinMetaBlocks(metaSectors)
}

// poolHeldSnap reports whether a thin pool's status line shows a metadata
// snapshot held (common.ThinPoolStatus.HeldRoot) on a pool that serves pool
// messages: the state an agent leaves that died between a
// reserve_metadata_snap and its release (CN13, CN25). A line that does not
// parse, a failed pool's among them, and one whose mode reads ro, under
// needs_check or out of metadata space, are not judged: dm-thin refuses the
// release then, and naming the pool would bring a converge on every round
// that changes nothing.
func poolHeldSnap(raw string) bool {
	st, ok := common.ParseThinPoolStatus(raw)
	return ok && st.HeldRoot && !st.NeedsCheck && !st.ReadOnly
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
