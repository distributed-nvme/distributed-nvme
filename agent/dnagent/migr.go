package dnagent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// migrMetaBaseSize covers the dm-clone superblock and rounding; the bitmap
// itself needs one bit per region, so budgeting one byte per region on top is
// a generous bound. AllocCloneMeta rounds the result up to whole
// DnCloneMetaUnit slots.
//
// The area is **per DN**, not per SP: DnCloneMetaSize is one 48-unit region of
// the disk shared by every destination role the node hosts, across every SP on
// it, so the capacity is sum(migrMetaSize(migr)) <= 48 units node-wide and
// MaxMigrCntPerSp = 4 says nothing about it. Because the base alone is exactly
// one unit, every migration costs >= 2 units, so 24 *minimum-cost* destination
// roles is the node-wide ceiling — and fewer for large sides at a small
// region size, where the one-byte-per-region term dominates (a 1 TiB side at
// 1 MiB regions costs 2 units, at 64 KiB regions 5). This is the dn twin of
// the cn arena ceiling (clonemeta.go's cloneMetaUnits). Nothing gates the
// destination count against this ceiling; the converge that meets exhaustion
// reports it on the migr_dst_info rows (DN13: target_info RES_STATUS_MISSING,
// dm_clone_info RES_STATUS_ERROR with the allocator's message), and growing
// DnCloneMetaSize would be a format-version bump.
const migrMetaBaseSize = 4 * 1024 * 1024

func migrMetaSize(regions uint64) uint64 {
	return uint64(migrMetaBaseSize) + regions
}

// ---------------------------------------------------------------------------
// Migration source (DN12) — steps 1 and 2 happen in convergeSide/ensureCnDm;
// this is step 3.
// ---------------------------------------------------------------------------

func (s *DnAgentServer) ensureMigrSrc(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
) {
	t := st.tracker
	srcInfo := &pb.SideInfo_MigrSrcInfo{}
	info.MigrSrcInfo = srcInfo

	name := plan.migrSrcName()
	err := s.ensureDmLinear(ctx, name, plan.sectors, plan.sideDevPath)
	srcInfo.DmLinearInfo = t.FromErr(resKeyMigrSrcDm, name, "", err)
	if err != nil || !plan.wantExport {
		return
	}
	nqn := plan.migrSrcNqn()
	exportErr := s.ensureExport(ctx,
		agent.SubsysConf{
			Nqn: nqn,
			AllowedHosts: []string{
				s.nf.DnHostNqn(plan.clusterId, plan.migrSrc.GetDstDnId()),
			},
		},
		agent.NsConf{
			Nqn:        nqn,
			Nsid:       sideNsid,
			DevicePath: s.nf.DmPath(name),
			AnaGrpId:   common.AnaGrpIdOptimized,
		})
	srcInfo.NvmeofInfo = t.FromErr(
		resKeyMigrSrcNvmeof, nqn, "", exportErr)
}

// ---------------------------------------------------------------------------
// Migration destination (DN13)
// ---------------------------------------------------------------------------

// ensureMigrDst runs the dst side sequence of architecture.md, Migration, and
// reports whether the
// dm-clone is live — which is what decides the per-CN dm-linear targets and
// ANA groups for this pass.
func (s *DnAgentServer) ensureMigrDst(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
	info *pb.SideInfo,
) bool {
	t := st.tracker
	dstInfo := &pb.SideInfo_MigrDstInfo{}
	info.MigrDstInfo = dstInfo
	nqn := plan.srcNqnOfDst()
	cloneName := plan.migrFinalName()

	// Chunks left over from a previous migration on this side are never
	// applied to a new one.
	if st.chunks.Len() > 0 &&
		st.chunkMigrId != plan.migrDst.GetMigrId() {
		s.dropChunks(ctx, st)
	}

	// (2) the dm-clone metadata slot and its wrapper device.
	if err := s.ensureMigrMeta(ctx, plan); err != nil {
		dstInfo.TargetInfo = t.Missing(resKeyMigrDstTarget, nqn,
			"clone metadata missing")
		dstInfo.DmCloneInfo = t.Err(
			resKeyMigrDstClone, cloneName, err.Error())
		return false
	}

	// (3) one connect attempt per pass, never retried inside the RPC:
	// "retrying until success" (architecture.md, Migration, dst step 3) is the
	// DN8 background registry. The
	// only other wait here comes after a connect that succeeded: for the
	// source's namespace node, which the kernel adds from a scan the connect
	// only queued, pausing DnMigrDstNsWait at most (awaitMigrSrcNs).
	state, err := s.host.ListSubsys(ctx, nqn)
	if err == nil && !state.Found {
		if connErr := s.host.Connect(ctx, agent.TrConf{
			TrType:  plan.migrDst.GetSrcNvmeTrConf().GetTrType(),
			AdrFam:  plan.migrDst.GetSrcNvmeTrConf().GetAdrFam(),
			TrAddr:  plan.migrDst.GetSrcNvmeTrConf().GetTrAddr(),
			TrSvcId: plan.migrDst.GetSrcNvmeTrConf().GetTrSvcId(),
		}, nqn, plan.dnHostNqn()); connErr != nil {
			err = connErr
		} else {
			state, err = s.awaitMigrSrcNs(ctx, nqn)
		}
	}
	switch {
	case err != nil:
		dstInfo.TargetInfo = t.Err(resKeyMigrDstTarget, nqn, err.Error())
	case !state.Found:
		dstInfo.TargetInfo = t.Err(
			resKeyMigrDstTarget, nqn, "no controller for the subsystem")
	case state.DevicePath == "":
		dstInfo.TargetInfo = t.Err(
			resKeyMigrDstTarget, nqn, "controller has no namespace")
	case !state.Live:
		dstInfo.TargetInfo = t.Err(resKeyMigrDstTarget, nqn,
			"paths: "+strings.Join(state.States, ","))
	default:
		dstInfo.TargetInfo = t.Ok(resKeyMigrDstTarget, nqn,
			"paths: "+strings.Join(state.States, ","))
	}
	if err != nil || !state.Found || state.DevicePath == "" {
		dstInfo.DmCloneInfo = t.Missing(
			resKeyMigrDstClone, cloneName, "target not connected")
		s.startMigrRetry(st, plan)
		return false
	}
	s.stopMigrRetry(st)

	// (4) the dm-clone itself, created with hydration off so the bitmap
	// chunks land before any region is copied.
	created, err := s.ensureDmClone(ctx, plan, state.DevicePath)
	if err != nil {
		dstInfo.DmCloneInfo = t.Err(
			resKeyMigrDstClone, cloneName, err.Error())
		return false
	}
	if created {
		// Re-apply every chunk of the applied set whenever the dm-clone is
		// (re)created (architecture.md, Bitmap push protocol,
		// dnv-agent side step 4).
		s.applyChunks(ctx, st, plan)
	}
	// A hydration knob that would not apply is worth reporting, but it must
	// not cost the side its serving path: the dm-clone exists and reads
	// through to the source, so demoting the primary's dm-linear back onto
	// dm-error over it would turn a healthy side into an erroring one.
	hydrationErr := s.ensureHydration(ctx, plan)
	raw, err := s.dm.Status(ctx, cloneName)
	if err != nil {
		dstInfo.DmCloneInfo = t.Err(
			resKeyMigrDstClone, cloneName, err.Error())
		return false
	}
	if hydrationErr != nil {
		dstInfo.DmCloneInfo = t.Err(
			resKeyMigrDstClone, cloneName, hydrationErr.Error())
		return true
	}
	// architecture.md, Live-state reporting: the raw dmsetup status line
	// carries hydration progress.
	dstInfo.DmCloneInfo = t.Ok(resKeyMigrDstClone, cloneName, raw)
	return true
}

// awaitMigrSrcNs re-reads the migration source's subsystem after a connect
// this pass made, until its namespace device is there, pausing
// DnMigrDstNsPause between reads and DnMigrDstNsWait in all — the reads
// themselves are not counted (DN13 step (3)). The kernel returns from
// `nvme connect` as soon as the controller is live and only QUEUES the
// namespace scan that adds the device, so the single re-read this replaced
// could find the controller and no namespace yet, failing the target with
// "controller has no namespace" for a device milliseconds away.
// A ListSubsys that fails ends the wait at once. It connects nothing: one
// connect per pass stays DN13's rule, and the DN8 loop stays the retry.
func (s *DnAgentServer) awaitMigrSrcNs(
	ctx context.Context,
	nqn string,
) (*agent.SubsysState, error) {
	budget := agent.NewWaitBudget(common.DnMigrDstNsWait, s.now, s.sleep)
	for {
		state, err := s.host.ListSubsys(ctx, nqn)
		if err != nil || state.DevicePath != "" {
			return state, err
		}
		if !budget.Pause(ctx, common.DnMigrDstNsPause) {
			return state, nil
		}
	}
}

// ensureMigrMeta reserves this migration's dm-clone metadata slot and builds
// the wrapper dm-linear over it. The wrapper exists because the dm-clone
// target reads its metadata device from sector 0 and takes no offset
// argument (DN13); AllocCloneMeta zeroes a freshly chosen slot's first 8 KiB
// before its record is persisted, so a previous tenant's bytes can never be
// misparsed as a valid dm-clone superblock.
func (s *DnAgentServer) ensureMigrMeta(
	ctx context.Context,
	plan *sidePlan,
) error {
	regionSectors := plan.migrDst.GetBlockSize() / agent.SectorSize
	if regionSectors == 0 {
		return fmt.Errorf("migr_dst_conf.block_size is 0")
	}
	migrId := plan.migrDst.GetMigrId()
	rec, err := s.meta.AllocCloneMeta(ctx, plan.spId, migrId,
		migrMetaSize(plan.sectors/regionSectors))
	if err != nil {
		return err
	}
	devNo, err := s.dm.DevNo(ctx, s.disk)
	if err != nil {
		return err
	}
	name := plan.migrMetaDmName()
	sectors := rec.GetUnitCount() * common.DnCloneMetaUnit / agent.SectorSize
	offsetSectors := (common.DnCloneMetaOffset +
		rec.GetUnitStart()*common.DnCloneMetaUnit) / agent.SectorSize
	table := agent.LinearTable(sectors, devNo, offsetSectors)

	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return err
	}
	if dev == nil {
		return s.dm.Create(ctx, name, table)
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		return err
	}
	converged := len(targets) == 1 && targets[0].Type == "linear" &&
		targets[0].Length == sectors && len(targets[0].Args) == 2 &&
		targets[0].Args[0] == devNo &&
		targets[0].Args[1] == strconv.FormatUint(offsetSectors, 10)
	if !converged || dev.ReadOnly {
		return s.dm.Reload(ctx, name, table)
	}
	if dev.Suspended {
		return s.dm.Resume(ctx, name)
	}
	return nil
}

// ensureDmClone converges the dm-clone and reports whether this pass created
// it. The source device number changes across reconnects, so a converged
// check compares the resolved devices, not just presence.
func (s *DnAgentServer) ensureDmClone(
	ctx context.Context,
	plan *sidePlan,
	srcDevPath string,
) (bool, error) {
	name := plan.migrFinalName()
	regionSectors := plan.migrDst.GetBlockSize() / agent.SectorSize
	if regionSectors == 0 {
		return false, fmt.Errorf("migr_dst_conf.block_size is 0")
	}
	if plan.sectors == 0 {
		return false, fmt.Errorf("side size is 0 (extent_size or ext_cnt unset)")
	}
	metaNo, err := s.dm.DevNo(ctx, plan.migrMetaDmPath())
	if err != nil {
		return false, err
	}
	destNo, err := s.dm.DevNo(ctx, plan.sideDevPath)
	if err != nil {
		return false, err
	}
	srcNo, err := s.dm.DevNo(ctx, srcDevPath)
	if err != nil {
		return false, err
	}
	conf := plan.migrDst.GetDmCloneConf()
	// no_discard_passdown is mandatory on every dnv dm-clone (DN13 step
	// 4): `blkdiscard` on a dm-clone is this design's metadata-only "mark
	// this region hydrated" primitive (architecture.md, Bitmap push protocol,
	// raid0 bitmap math and Clone crash recovery), and dm-clone
	// turns passdown on by default whenever the destination's discard
	// granularity is no larger than a region — which a dm-linear over a raw
	// disk satisfies. The hazard is *after* the cutover (architecture.md,
	// Migration, dst step 5), not before it:
	// host IO then flows through this dm-clone, a host write hydrates region
	// r, and a skip-bitmap chunk whose bit for r was read from the CN thin
	// metadata before that write can still arrive later (chunk pushes are
	// legal at any time, and a restart re-applies every stored chunk). The
	// agent then `blkdiscard`s r; with passdown that discard would reach the
	// side device and destroy the only copy of an acknowledged write. Without
	// it the same discard is the metadata no-op that
	// architecture.md, Bitmap push protocol, and [D7] already
	// assume. Cost: only discards issued during an active migration stop
	// reaching the disk; after FinishMigration the dm-clone is gone.
	table := agent.CloneTable(plan.sectors, metaNo, destNo, srcNo,
		regionSectors, true, true,
		conf.GetHydrationThreshold(), conf.GetHydrationBatchSize())

	dev, err := s.dm.Info(ctx, name)
	if err != nil {
		return false, err
	}
	if dev == nil {
		if err := s.dm.Create(ctx, name, table); err != nil {
			return false, err
		}
		return true, nil
	}
	targets, err := s.dm.Table(ctx, name)
	if err != nil {
		return false, err
	}
	if len(targets) == 1 && targets[0].Type == "clone" &&
		targets[0].Length == plan.sectors && len(targets[0].Args) >= 4 &&
		targets[0].Args[0] == metaNo && targets[0].Args[1] == destNo &&
		targets[0].Args[2] == srcNo &&
		targets[0].Args[3] == fmt.Sprintf("%d", regionSectors) {
		// Suspended on the table it wants — an interrupted reload, or a
		// failed one whose old table is wanted again, since a reload fails
		// closed (dnagent.md, OS wrappers — `dm.go`, `nvmet.go`, `nvmehost.go`)
		// — it is resumed ([D12]).
		if dev.Suspended {
			return false, s.dm.Resume(ctx, name)
		}
		return false, nil
	}
	if err := s.dm.Reload(ctx, name, table); err != nil {
		return false, err
	}
	return false, nil
}

// ensureHydration applies the desired hydration state and knobs through
// dmsetup messages — probe-first, so a converged dm-clone is left alone.
// Hydration is infrastructure IO, not user IO, so no sp_level ever pauses
// it: this function only ever enables it ([D11]).
func (s *DnAgentServer) ensureHydration(
	ctx context.Context,
	plan *sidePlan,
) error {
	name := plan.migrFinalName()
	raw, err := s.dm.Status(ctx, name)
	if err != nil {
		return err
	}
	status, ok := agent.ParseCloneStatus(raw)
	if !ok {
		return fmt.Errorf("unparsable dm-clone status %q", raw)
	}
	conf := plan.migrDst.GetDmCloneConf()
	if threshold := conf.GetHydrationThreshold(); threshold != 0 &&
		status.Threshold != threshold {
		if err := s.dm.Message(ctx, name, 0,
			fmt.Sprintf("hydration_threshold %d", threshold)); err != nil {
			return err
		}
	}
	if batch := conf.GetHydrationBatchSize(); batch != 0 &&
		status.BatchSize != batch {
		if err := s.dm.Message(ctx, name, 0,
			fmt.Sprintf("hydration_batch_size %d", batch)); err != nil {
			return err
		}
	}
	if status.HydrationEnabled {
		return nil
	}
	return s.dm.Message(ctx, name, 0, "enable_hydration")
}

// ---------------------------------------------------------------------------
// DN8 background connect retry
// ---------------------------------------------------------------------------

// startMigrRetry registers a side whose migration-source connect failed. A
// goroutine re-runs the destination converge every
// DnMigrConnectRetryInterval seconds under the DN1 locks, until it succeeds
// or the side is torn down — the RPC itself never retries a connect (it
// waits only for the namespace of one that succeeded, pausing
// DnMigrDstNsWait at most: awaitMigrSrcNs).
//
// The loop is enrolled in the server's WaitGroup so WaitBackground covers the
// connect retries too, not just zeroing (SH27). Like startZeroing it
// therefore refuses once rootCtx is done: an armed DN12 fence timer is not
// enrolled and can still reach a converge after the join returned, and a
// bg.Add after bg.Wait panics.
func (s *DnAgentServer) startMigrRetry(st *sideState, plan *sidePlan) {
	key := sideKey(plan.clusterId, plan.dnId, plan.spId, plan.sideId)
	s.mu.Lock()
	if st.retrying || s.rootCtx.Err() != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.rootCtx)
	st.retrying = true
	st.cancel = cancel
	s.mu.Unlock()
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		s.migrRetryLoop(ctx, key, st)
	}()
}

func (s *DnAgentServer) stopMigrRetry(st *sideState) {
	s.mu.Lock()
	cancel := st.cancel
	st.cancel = nil
	st.retrying = false
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *DnAgentServer) migrRetryLoop(
	ctx context.Context,
	key string,
	st *sideState,
) {
	ticker := time.NewTicker(s.migrRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// THE ATTEMPT RUNS ON rootCtx, NEVER ON THIS LOOP'S ctx, and that is
		// not a detail. The attempt is the converge that will finally
		// connect, and a converge that connects calls stopMigrRetry — which
		// cancels exactly this loop's ctx. Running the attempt on it made
		// that converge cancel ITSELF halfway: every OS call after the
		// stopMigrRetry failed on the dead context, so the dm-clone was
		// never created, and with `retrying` already false nothing ticked
		// again. The side then sat at `dm_clone: RES_STATUS_MISSING, target
		// not connected` for ever while the controller it names was `live` —
		// which is what an e2e `copy` case measured on 2026-09-19 after one
		// transient connect failure (the fence timer already reconverges on
		// rootCtx for the same reason).
		//
		// Cancelling therefore ends the LOOP, not the attempt: the ctx.Err()
		// check below is what reads it, one tick late at worst. Shutdown is
		// unaffected, because rootCtx is cancelled before WaitBackground
		// joins (SH27), so an attempt in flight then still aborts.
		s.reconvergeSide(s.rootCtx, key, st)
		if ctx.Err() != nil || s.rootCtx.Err() != nil {
			return
		}
	}
}

// reconvergeSide re-runs one side's converge from a background goroutine
// under the DN1 locks. It backs both the DN8 connect retry and the DN12
// fence timer — anything that has to happen later without blocking an RPC.
func (s *DnAgentServer) reconvergeSide(
	ctx context.Context,
	key string,
	st *sideState,
) {
	if s.getSide(key) != st {
		return
	}
	// Every background attempt is its own traceable operation (SH2).
	attemptCtx := common.WithTraceId(ctx, common.NewTraceId())
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()
	if s.getSide(key) != st {
		return
	}
	req := st.req.Load()
	dn := s.getDn(dnKey(req.GetClusterId(), req.GetDnId()))
	if dn == nil {
		return
	}
	s.convergeSide(attemptCtx, st, dn.req.Load().GetExtentSize())
}
