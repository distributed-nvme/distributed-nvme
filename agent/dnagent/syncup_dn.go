package dnagent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ResTracker keys of the node-level resources.
const (
	resKeyDisk = "disk"
	resKeyMeta = "meta"
	resKeyPort = "port"
)

// tagNoWriteZeroes is the DN5 fail-fast detail of §9.4's standing hardware
// assumption: a disk whose write_zeroes_max_bytes is 0 would make the kernel
// fall back to writing zero pages at bulk speed, so a DnZeroBatchExtCnt batch
// could not finish inside CmdSoftTimeout and side provisioning would never
// converge. Reporting it on meta_info is what flows into err_epoch →
// capacity-key removal, taking the unsuitable DN out of allocation.
const tagNoWriteZeroes = "disk lacks Write Zeroes"

// Reconcile is the SH1 startup pass (DN2): load the local store, converge
// every stored DN, tear down sides whose pointer left their DN's list,
// converge the rest, then re-apply every persisted bitmap chunk. It runs
// under the node write lock with the caller's startup trace id (SH2), and
// fails only when the local store itself is unreadable (SH3).
func (s *DnAgentServer) Reconcile(ctx context.Context) error {
	s.rootCtx = ctx
	s.locks.Node().Lock()
	defer s.locks.Node().Unlock()

	files, err := s.store.List(ctx,
		agent.StoreKindDn, agent.StoreKindSide, agent.StoreKindMigrBm)
	if err != nil {
		return err
	}

	for _, path := range files[agent.StoreKindDn] {
		req := &pb.SyncupDnRequest{}
		if err := s.store.Load(ctx, path, req); err != nil {
			slog.ErrorContext(ctx, "skipping unreadable dn state file",
				slog.String("path", path),
				slog.String("error", err.Error()))
			continue
		}
		// §7: a file an older build persisted with a zero extent size is
		// LOADED, and refused below by convergeDn, rather than skipped here.
		// Skipping it would drop the DN record, and the side loop further down
		// reads a missing DN as "this side left its parent's list" and tears
		// every one of them down — exports, dm devices and local state. A conf
		// fault must not destroy resources, so the record is kept exactly as
		// the restart found it and convergeDn is the single place that reports
		// it, once per pass.
		s.putDn(dnKey(req.GetClusterId(), req.GetDnId()), &dnState{
			req:     req,
			tracker: agent.NewResTracker(),
		})
	}
	for _, path := range files[agent.StoreKindSide] {
		req := &pb.SyncupSideRequest{}
		if err := s.store.Load(ctx, path, req); err != nil {
			slog.ErrorContext(ctx, "skipping unreadable side state file",
				slog.String("path", path),
				slog.String("error", err.Error()))
			continue
		}
		key := sideKey(req.GetClusterId(), req.GetDnId(),
			req.GetSidePointer().GetSpId(), req.GetSidePointer().GetSideId())
		st := newSideState(req)
		// Any per-CN linear this side left suspended belongs to the previous
		// process; DN12 retires it at once rather than opening a second
		// grace window.
		s.adoptFence(ctx, st)
		s.putSide(key, st)
	}

	// Bitmap chunks name their own side, so the owning side is found by
	// decoding the persisted request (SH21).
	orphans := make(map[string]struct{})
	for _, path := range files[agent.StoreKindMigrBm] {
		chunk := &pb.PushMigrBitmapRequest{}
		if err := s.store.Load(ctx, path, chunk); err != nil {
			slog.ErrorContext(ctx, "skipping unreadable bitmap chunk file",
				slog.String("path", path),
				slog.String("error", err.Error()))
			continue
		}
		key := sideKey(chunk.GetClusterId(), chunk.GetDnId(),
			chunk.GetSidePointer().GetSpId(),
			chunk.GetSidePointer().GetSideId())
		st := s.getSide(key)
		if st == nil || st.req.GetMigrDstConf().GetMigrId() !=
			chunk.GetMigrId() {
			orphans[path] = struct{}{}
			continue
		}
		st.chunkMigrId = chunk.GetMigrId()
		st.chunks.Put(chunk.GetBmIdx(), chunk.GetBitmap())
	}
	if len(orphans) > 0 {
		paths := make([]string, 0, len(orphans))
		for path := range orphans {
			paths = append(paths, path)
		}
		if err := s.store.Remove(ctx, paths...); err != nil {
			slog.ErrorContext(ctx, "removing orphan bitmap chunks failed",
				slog.String("error", err.Error()))
		}
	}

	for _, key := range s.dnKeys() {
		st := s.getDn(key)
		s.convergeDn(ctx, st)
	}
	for _, key := range s.allSideKeys() {
		st := s.getSide(key)
		dn := s.getDn(dnKey(st.req.GetClusterId(), st.req.GetDnId()))
		if dn == nil || !pointerKnown(dn.req, st.req.GetSidePointer()) {
			// Removed from its parent's list mid-teardown.
			s.teardownSide(ctx, key, st)
			continue
		}
		if agent.ValidateExtentSize(dn.req.GetExtentSize()) != nil {
			// §7: the parent's conf is unusable, which is NOT the same thing
			// as this side having left its parent's list. Every run of this
			// side is carved out of that extent size, so nothing here may be
			// converged — and nothing may be torn down either. convergeDn
			// above already recorded the refusal for this DN.
			continue
		}
		s.convergeSide(ctx, st, dn.req.GetExtentSize())
	}
	// DN2: the dm-clone may have survived the restart, so re-apply every
	// chunk once here rather than only on (re)creation.
	for _, key := range s.allSideKeys() {
		st := s.getSide(key)
		dn := s.getDn(dnKey(st.req.GetClusterId(), st.req.GetDnId()))
		if dn == nil {
			continue
		}
		if agent.ValidateExtentSize(dn.req.GetExtentSize()) != nil {
			// §7, as in the converge loop above: a chunk's offset is computed
			// from the extent size, so an unusable one applies nothing.
			continue
		}
		s.applyMigrBitmaps(ctx, st, dn.req.GetExtentSize())
	}
	s.sweepOrphanRecords(ctx)
	return nil
}

// sweepOrphanRecords closes the crash window between a teardown's resource
// removal and its table update: a side torn down by DN6 whose FreeSide never
// ran would otherwise leak its extents forever.
//
// The rule is deliberately narrow, because the volume table — not the local
// store — is authoritative for extent placement ([D13], [P4]): a record is
// swept only when the DN's **authoritative side_pointer_list** proves its
// owner is gone. "No local state for this side" is NOT such a proof. A node
// that lost --local-store but kept its disk still has every side in its DN's
// pointer list, and must rebuild those sides from their records; sweeping
// them would free the extents and send the next SyncupSide through the §9.4
// provisioning protocol again, zeroing live data.
//
// The caller holds the node write lock, so neither the DN set nor the side
// set can move under it.
func (s *DnAgentServer) sweepOrphanRecords(ctx context.Context) {
	clusterId, dnId, ok := s.meta.Identity()
	if !ok {
		// Unformatted, unreadable, or a disk this node has not confirmed as
		// its own — nothing here may be freed.
		return
	}
	known, haveState, ok := s.knownSides()
	if !ok {
		// No DN has been synced or reloaded yet, so nothing is authoritative
		// and no record can be shown to be an orphan.
		return
	}

	sideRecs, err := s.meta.SideRecords(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "reading the volume table failed",
			slog.String("error", err.Error()))
		return
	}
	for _, rec := range sideRecs {
		key := [2]uint64{rec.GetSpId(), rec.GetSideId()}
		if _, live := known[key]; live {
			continue
		}
		// A record is provably orphaned only when no local state claims it, so
		// there is normally no goroutine to stop here — but the join must
		// happen before the removal all the same: a `blkdiscard --zeroout`
		// child holds the side device open and `dmsetup remove` would fail
		// EBUSY (§9.4).
		s.stopZeroingOf(clusterId, dnId, rec.GetSpId(), rec.GetSideId())
		if !s.removeDm(ctx, s.nf.DnSideName(
			clusterId, dnId, rec.GetSpId(), rec.GetSideId())) {
			// The extents stay allocated while a device still maps them;
			// the next node-level pass retries.
			continue
		}
		if err := s.meta.FreeSide(
			ctx, rec.GetSpId(), rec.GetSideId()); err != nil {
			slog.ErrorContext(ctx, "freeing an orphan side record failed",
				slog.String("error", err.Error()))
		}
	}

	cloneRecs, err := s.meta.CloneMetaRecords(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "reading the volume table failed",
			slog.String("error", err.Error()))
		return
	}
	claimed := s.claimedMigrs()
	for _, rec := range cloneRecs {
		if _, live := claimed[[2]uint64{
			rec.GetSpId(), rec.GetMigrId()}]; live {
			continue
		}
		// A metadata slot is only provably orphaned when every side of its
		// sp is one whose state we actually hold — otherwise a side we have
		// not heard from yet could still own it, and freeing the slot would
		// strand an in-flight migration whose hydration is supposed to
		// resume from disk (§11.2).
		if !s.spFullyKnown(rec.GetSpId(), known, haveState) {
			continue
		}
		if !s.removeDm(ctx, s.nf.DnMigrMetaDmName(
			clusterId, dnId, rec.GetSpId(), rec.GetMigrId())) {
			continue
		}
		if err := s.meta.FreeCloneMeta(
			ctx, rec.GetSpId(), rec.GetMigrId()); err != nil {
			slog.ErrorContext(ctx,
				"freeing an orphan clone-metadata record failed",
				slog.String("error", err.Error()))
		}
	}
}

// stopZeroingOf cancels and joins the §9.4 zeroing goroutine of a side named
// only by its allocation record — the shape the DN6 orphan sweep works in. It
// is a no-op when no local state for that side exists, which is the sweep's
// normal case.
func (s *DnAgentServer) stopZeroingOf(
	clusterId uint64,
	dnId uint64,
	spId uint64,
	sideId uint64,
) {
	if st := s.getSide(sideKey(clusterId, dnId, spId, sideId)); st != nil {
		s.stopZeroing(st)
	}
}

// knownSides returns every side this node may still host: the union of every
// synced DN's authoritative side_pointer_list and every side with local
// state. haveState is the subset whose SyncupSideRequest the agent actually
// holds. ok is false when no DN has been synced or reloaded at all.
func (s *DnAgentServer) knownSides() (
	map[[2]uint64]struct{}, map[[2]uint64]struct{}, bool,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.dns) == 0 {
		return nil, nil, false
	}
	known := make(map[[2]uint64]struct{})
	haveState := make(map[[2]uint64]struct{})
	for _, dn := range s.dns {
		for _, ptr := range dn.req.GetSidePointerList() {
			known[[2]uint64{ptr.GetSpId(), ptr.GetSideId()}] = struct{}{}
		}
	}
	for _, st := range s.sides {
		ptr := st.req.GetSidePointer()
		key := [2]uint64{ptr.GetSpId(), ptr.GetSideId()}
		known[key] = struct{}{}
		haveState[key] = struct{}{}
	}
	return known, haveState, true
}

// claimedMigrs lists the (sp_id, migr_id) pairs a live destination role owns.
// Both the currently requested and the last applied conf count, so a converge
// that has not run yet never loses its metadata slot.
func (s *DnAgentServer) claimedMigrs() map[[2]uint64]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[[2]uint64]struct{})
	for _, st := range s.sides {
		spId := st.req.GetSidePointer().GetSpId()
		for _, dst := range []*pb.SyncupSideRequest_MigrDstConf{
			st.req.GetMigrDstConf(), st.appliedMigrDst,
		} {
			if dst == nil {
				continue
			}
			out[[2]uint64{spId, dst.GetMigrId()}] = struct{}{}
		}
	}
	return out
}

// spFullyKnown reports whether every side of one sp that this node may host
// is a side whose local state the agent holds.
func (s *DnAgentServer) spFullyKnown(
	spId uint64,
	known map[[2]uint64]struct{},
	haveState map[[2]uint64]struct{},
) bool {
	for key := range known {
		if key[0] != spId {
			continue
		}
		if _, ok := haveState[key]; !ok {
			return false
		}
	}
	return true
}

func (s *DnAgentServer) dnKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.dns))
	for key := range s.dns {
		keys = append(keys, key)
	}
	return keys
}

func (s *DnAgentServer) allSideKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.sides))
	for key := range s.sides {
		keys = append(keys, key)
	}
	return keys
}

func newSideState(req *pb.SyncupSideRequest) *sideState {
	return &sideState{
		req:               req,
		tracker:           agent.NewResTracker(),
		chunks:            agent.NewChunkSet(),
		appliedCnIds:      cnIdsOf(req.GetSideConf()),
		appliedMigrSrc:    req.GetMigrSrcConf(),
		appliedMigrSrcRaw: req.GetMigrSrcConf(),
		appliedMigrDst:    req.GetMigrDstConf(),
	}
}

// pointerKnown reports whether a side pointer is in the DN's authoritative
// list — what makes a side known to the agent at all (DN6, DN8).
func pointerKnown(req *pb.SyncupDnRequest, ptr *pb.SidePointer) bool {
	for _, known := range req.GetSidePointerList() {
		if known.GetSpId() == ptr.GetSpId() &&
			known.GetSideId() == ptr.GetSideId() {
			return true
		}
	}
	return false
}

// msgInvalidStoredConf is the §7 refusal record: a conf member the control
// plane cannot have written reached this agent, and the converge it would have
// driven did not happen. The string is shared with the cn role and with
// dnv-worker's own refusal so one grep finds every one of them.
const msgInvalidStoredConf = "invalid stored conf"

// syncupDn implements DN4-DN7. The node write lock is held by the caller.
func (s *DnAgentServer) syncupDn(
	ctx context.Context,
	req *pb.SyncupDnRequest,
) *pb.SyncupDnReply {
	key := dnKey(req.GetClusterId(), req.GetDnId())
	st := s.getDn(key)
	var stored uint64
	if st != nil {
		stored = st.req.GetRevision()
	}
	if reject := agent.GateRevision(stored, req.GetRevision()); reject != nil {
		return &pb.SyncupDnReply{AgentReply: reject, Revision: stored}
	}
	// §7: extent_size is what this disk's [D13] header is formatted with and
	// what every side's run is carved out of, so a zero is refused rather
	// than replaced with a constant the rest of the cluster does not share.
	// This is the last point with literally zero side effects: the request
	// has not become the desired state, nothing has been converged, no dm
	// device removed, no volume-table block written and no local-store file
	// touched. Putting it after st.req = req would persist the zero and let
	// the next Reconcile converge it.
	if err := agent.ValidateExtentSize(req.GetExtentSize()); err != nil {
		slog.ErrorContext(ctx, msgInvalidStoredConf,
			slog.Uint64("cluster_id", req.GetClusterId()),
			slog.Uint64("dn_id", req.GetDnId()),
			slog.String("error", err.Error()))
		return &pb.SyncupDnReply{
			AgentReply: agent.InvalidConfReply("%v", err),
			Revision:   stored,
		}
	}
	if st == nil {
		st = &dnState{tracker: agent.NewResTracker()}
	}
	st.req = req
	s.putDn(key, st)

	info := s.convergeDn(ctx, st)
	s.teardownRemovedSides(ctx, req)
	s.sweepOrphanRecords(ctx)

	path := s.nf.LocalDnPath(req.GetClusterId(), req.GetDnId())
	if err := s.store.Save(ctx, path, req); err != nil {
		slog.ErrorContext(ctx, "persisting dn state failed",
			slog.String("path", path),
			slog.String("error", err.Error()))
	}
	return &pb.SyncupDnReply{
		AgentReply: agent.OkReply(),
		Revision:   req.GetRevision(),
		DnInfo:     info,
	}
}

// convergeDn builds the once-per-DN base state of §3.1 probe-first (DN5),
// recording each resource's outcome as it goes. A failed resource never
// aborts the pass (DN19).
func (s *DnAgentServer) convergeDn(
	ctx context.Context,
	st *dnState,
) *pb.DnInfo {
	req := st.req
	t := st.tracker
	info := &pb.DnInfo{}

	// §7: the entrance that does not come through syncupDn's gate is the
	// startup Reconcile, which converges from a file an older build may have
	// persisted with a zero. extent_size is what this disk's [D13] header is
	// formatted and verified against, so a zero must not reach EnsureFormatted
	// at all — it would report an identity mismatch naming the disk rather
	// than the field that is actually wrong. Nothing is mutated on the way
	// out: the meta row carries the reason and the port is left as found.
	if err := agent.ValidateExtentSize(req.GetExtentSize()); err != nil {
		slog.ErrorContext(ctx, msgInvalidStoredConf,
			slog.Uint64("cluster_id", req.GetClusterId()),
			slog.Uint64("dn_id", req.GetDnId()),
			slog.String("error", err.Error()))
		info.MetaInfo = t.Err(resKeyMeta, s.disk, err.Error())
		return info
	}

	size, err := s.dm.DiskSize(ctx, s.disk)
	info.DiskInfo = t.FromErr(resKeyDisk, s.disk, "", err)
	if err == nil {
		// The allocator needs the extent count of the data area; the raw
		// size is the only part of it the disk format does not carry.
		s.meta.SetDiskSize(size)
	}

	info.MetaInfo = s.ensureDiskMeta(ctx, t, req)

	info.PortInfo = s.ensurePort(ctx, t)
	return info
}

// ensureDiskMeta converges the [D13] disk format: a blank disk is formatted
// (header + an empty volume table in slot A), a disk already formatted for
// this cluster/dn/extent_size is left untouched, and a disk formatted for
// anything else is refused rather than overwritten (DN5).
func (s *DnAgentServer) ensureDiskMeta(
	ctx context.Context,
	t *agent.ResTracker,
	req *pb.SyncupDnRequest,
) *pb.ResInfo {
	if err := s.meta.EnsureFormatted(ctx, req.GetClusterId(),
		req.GetDnId(), req.GetExtentSize()); err != nil {
		return t.Err(resKeyMeta, s.disk, err.Error())
	}
	if details, ok := s.checkWriteZeroes(ctx); !ok {
		return t.Err(resKeyMeta, s.disk, details)
	}
	return t.Ok(resKeyMeta, s.disk, s.meta.Describe())
}

// checkWriteZeroes is §9.4's DN5 fail-fast. It returns
// (tagNoWriteZeroes, false) **only** when the sysfs attribute is present and
// reads 0. An absent or unreadable attribute is not a verdict — an older
// kernel simply may not publish it, and failing a healthy DN for that would
// take it out of allocation for a reason the spec never states (ruling R4.11).
//
// A failure reports meta_info but never gates converging (ruling R4.12): an
// already-populated DN keeps serving the sides it hosts, and DN5's identity
// check stays the only write gate.
func (s *DnAgentServer) checkWriteZeroes(ctx context.Context) (string, bool) {
	value, present, err := s.dm.WriteZeroesMaxBytes(ctx, s.disk)
	if err != nil {
		slog.WarnContext(ctx, "reading write_zeroes_max_bytes failed",
			slog.String("disk", s.disk),
			slog.String("error", err.Error()))
		return "", true
	}
	if !present || value != 0 {
		return "", true
	}
	return tagNoWriteZeroes, false
}

// ensurePort converges the node's single nvmet port and its three fixed ANA
// groups (SH19). Probing first keeps a converged port untouched: the port
// attributes cannot be rewritten once a subsystem is linked.
func (s *DnAgentServer) ensurePort(
	ctx context.Context,
	t *agent.ResTracker,
) *pb.ResInfo {
	resName := fmt.Sprintf("%d", common.NvmetPortId)
	ok, details, err := s.nvmet.ProbePort(ctx, common.NvmetPortId, s.port)
	if err != nil {
		return t.Err(resKeyPort, resName, err.Error())
	}
	if ok {
		return t.Ok(resKeyPort, resName, "")
	}
	if err := s.nvmet.EnsurePort(
		ctx, common.NvmetPortId, s.port); err != nil {
		return t.Err(resKeyPort, resName, err.Error())
	}
	ok, details, err = s.nvmet.ProbePort(ctx, common.NvmetPortId, s.port)
	if err != nil {
		return t.Err(resKeyPort, resName, err.Error())
	}
	if !ok {
		return t.Err(resKeyPort, resName, details)
	}
	return t.Ok(resKeyPort, resName, "")
}

// teardownRemovedSides implements the DN6 pointer diff: a local side whose
// pointer left the authoritative list is torn down top-down. Ids are never
// reused, so a deleted side never comes back.
func (s *DnAgentServer) teardownRemovedSides(
	ctx context.Context,
	req *pb.SyncupDnRequest,
) {
	for _, key := range s.sideKeysOf(req.GetClusterId(), req.GetDnId()) {
		st := s.getSide(key)
		if st == nil || pointerKnown(req, st.req.GetSidePointer()) {
			continue
		}
		s.teardownSide(ctx, key, st)
	}
}
