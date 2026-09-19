package cnagent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Reconcile is the SH1 startup pass (CN2): load the local store, converge
// every stored CN's §3.2 base state, tear down cntlrs whose pointer left their
// CN's list, converge the rest — which, per CN18, runs the §11.5 recovery for
// any clone whose metadata wrapper is gone or mismatched or whose dm-clone
// has vanished (after a CN reboot the tmpfs arena and the dm state are both
// empty; after a plain agent restart both survive and the converge is a
// no-op re-apply) — and finally sweeps the kind-`b` wrappers no stored cntlr
// wants any more ([D14]). It runs under the node write lock with the caller's
// startup trace id (SH2), and fails only when the local store itself is
// unreadable (SH3). Background retries (CN10/CN18) and probers (CN11) mint a
// fresh trace id per attempt.
func (s *CnAgentServer) Reconcile(ctx context.Context) error {
	s.rootCtx = ctx
	s.locks.Node().Lock()
	defer s.locks.Node().Unlock()

	files, err := s.store.List(ctx,
		agent.StoreKindCn, agent.StoreKindCntlr, agent.StoreKindCloneBm)
	if err != nil {
		return err
	}

	for _, path := range files[agent.StoreKindCn] {
		req := &pb.SyncupCnRequest{}
		if err := s.store.Load(ctx, path, req); err != nil {
			slog.ErrorContext(ctx, "skipping unreadable cn state file",
				slog.String("path", path),
				slog.String("error", err.Error()))
			continue
		}
		s.putCn(cnKey(req.GetClusterId(), req.GetCnId()), &cnState{
			req:     req,
			tracker: agent.NewResTracker(),
		})
	}
	for _, path := range files[agent.StoreKindCntlr] {
		req := &pb.SyncupCntlrRequest{}
		if err := s.store.Load(ctx, path, req); err != nil {
			slog.ErrorContext(ctx, "skipping unreadable cntlr state file",
				slog.String("path", path),
				slog.String("error", err.Error()))
			continue
		}
		ptr := req.GetCntlrPointer()
		s.putCntlr(cntlrKey(req.GetClusterId(), req.GetCnId(),
			ptr.GetSpId(), ptr.GetCntlrId()), newCntlrState(req))
	}

	// Bitmap chunks name their own cntlr and their own (src_slice_idx,
	// bm_idx), so both the owner and the address are decoded from the
	// persisted request — the file name is only an address, the content is
	// authoritative. They are loaded **before** the converge, which is what
	// lets a (re)built dm-clone re-apply them in the same pass (CN18 step 4).
	var orphans []string
	for _, path := range files[agent.StoreKindCloneBm] {
		chunk := &pb.PushCloneBitmapRequest{}
		if err := s.store.Load(ctx, path, chunk); err != nil {
			slog.ErrorContext(ctx, "skipping unreadable bitmap chunk file",
				slog.String("path", path),
				slog.String("error", err.Error()))
			continue
		}
		ptr := chunk.GetCntlrPointer()
		st := s.getCntlr(cntlrKey(chunk.GetClusterId(), chunk.GetCnId(),
			ptr.GetSpId(), ptr.GetCntlrId()))
		if st == nil || findClone(st.req, chunk.GetCloneId()) == nil {
			orphans = append(orphans, path)
			continue
		}
		st.chunkSet(chunk.GetCloneId()).Put(agent.CloneChunkKey{
			SliceIdx: chunk.GetSrcSliceIdx(),
			BmIdx:    chunk.GetBmIdx(),
		}, chunk.GetBitmap())
	}
	if len(orphans) > 0 {
		if err := s.store.Remove(ctx, orphans...); err != nil {
			slog.ErrorContext(ctx, "removing orphan bitmap chunks failed",
				slog.String("error", err.Error()))
		}
	}

	for _, key := range s.cnKeys() {
		s.convergeCn(ctx, s.getCn(key))
	}
	// A cntlr whose pointer has left its parent's list is FORGOTTEN here —
	// file, chunks, memory entry and object lock — without any attempt to
	// remove its resources. That is safe because the node-level sweep below
	// finds those resources by name, and it is better than the teardown this
	// replaced: a teardown that failed still deleted the file, and nothing
	// ever looked again.
	for _, key := range s.allCntlrKeys() {
		st := s.getCntlr(key)
		cn := s.getCn(cnKey(st.req.GetClusterId(), st.req.GetCnId()))
		if cn == nil || !pointerKnown(cn.req, st.req.GetCntlrPointer()) {
			s.dropCntlrState(ctx, key, st)
		}
	}
	// The node-level sweep, which also owns the kind-cb wrapper sweep. It can
	// run before the cntlr converges because its wanted set comes from the
	// stored REQUESTS, not from what happens to exist: a clone this pass is
	// about to (re)build is named by its cntlr's clone_list already.
	for _, key := range s.cnKeys() {
		s.sweepCn(ctx, s.getCn(key), true)
	}
	for _, key := range s.allCntlrKeys() {
		s.convergeCntlr(ctx, s.getCntlr(key))
	}
	return nil
}

// syncupCn implements CN4-CN7. The node write lock is held by the caller.
func (s *CnAgentServer) syncupCn(
	ctx context.Context,
	req *pb.SyncupCnRequest,
) *pb.SyncupCnReply {
	key := cnKey(req.GetClusterId(), req.GetCnId())
	st := s.getCn(key)
	var stored uint64
	if st != nil {
		stored = st.req.GetRevision()
	}
	if reject := agent.GateRevision(stored, req.GetRevision()); reject != nil {
		return &pb.SyncupCnReply{AgentReply: reject, Revision: stored}
	}
	if st == nil {
		st = &cnState{tracker: agent.NewResTracker()}
	}
	st.req = req
	s.putCn(key, st)

	// The cn file is persisted BEFORE the sweep, not after it. The
	// sweep is what removes the resources of a cntlr whose pointer has just
	// left the list, and it can block for the whole failfast window on a dead
	// leg; a request cancelled in that window used to skip the save entirely,
	// and the next Reconcile then rebuilt the cntlr from the OLD list against
	// sides that no longer exist. With the new list on disk first, a crash
	// mid-sweep is nothing but a startup sweep.
	path := s.nf.LocalCnPath(req.GetClusterId(), req.GetCnId())
	if err := s.store.Save(ctx, path, req); err != nil {
		slog.ErrorContext(ctx, "persisting cn state failed",
			slog.String("path", path),
			slog.String("error", err.Error()))
	}
	info := s.convergeCn(ctx, st)
	s.dropRemovedCntlrs(ctx, req)
	sweep := s.sweepCn(ctx, st, true)

	return &pb.SyncupCnReply{
		AgentReply: sweep.Reply(),
		Revision:   req.GetRevision(),
		CnInfo:     info,
	}
}

// convergeCn builds the once-per-CN base state of §3.2 probe-first (CN5),
// recording each resource's outcome as it goes. A failed resource never aborts
// the pass (CN29).
//
// CN6: QoS is accepted and deliberately **not** enforced in this version. The
// §3.2 step 4 open issue stands — `io.max` written from any agent-created
// cgroup binds the agent's own tools, not the nvmet kernel threads that carry
// host IO — so the agent persists `qos_ratio` with the request (SH5 does that
// for free), applies nothing, and reports no QoS resource in CnInfo.
func (s *CnAgentServer) convergeCn(
	ctx context.Context,
	st *cnState,
) *pb.CnInfo {
	req := st.req
	t := st.tracker
	info := &pb.CnInfo{}
	clusterId := req.GetClusterId()
	cnId := req.GetCnId()

	tmpfsPath := s.nf.CnTmpfsPath(clusterId, cnId)
	info.TmpfsInfo = t.FromErr(resKeyTmpfs, tmpfsPath, "",
		s.ensureTmpfs(ctx, tmpfsPath))

	filePath := s.nf.CnTmpFilePath(clusterId, cnId)
	info.TmpFileInfo = t.FromErr(resKeyTmpFile, filePath, "",
		s.ensureTmpFile(ctx, filePath))

	// The loop device is the whole of the clone-metadata arena: CN18 carves it
	// into kind-`b` wrapper linears and needs no volume manager on top
	// ([D14]).
	loopDev, loopErr := s.ensureLoopDev(ctx, filePath)
	info.LoopDevInfo = t.FromErr(resKeyLoopDev, filePath, loopDev, loopErr)

	info.PortInfo = s.ensurePort(ctx, t)
	return info
}

func (s *CnAgentServer) ensureTmpfs(ctx context.Context, path string) error {
	mounted, fsType, err := s.cmeta.Mounted(ctx, path)
	if err != nil {
		return err
	}
	if mounted {
		if fsType != "tmpfs" {
			return fmt.Errorf("%s carries %s, want tmpfs", path, fsType)
		}
		return nil
	}
	return s.cmeta.MountTmpfs(ctx, path, common.DefaultCnTmpfsSize)
}

// ensureTmpFile creates the clone-metadata arena file sparse: tmpfs pages
// materialize only as a dm-clone writes metadata through its wrapper, and the
// allocator's hole-punch discard frees them again.
func (s *CnAgentServer) ensureTmpFile(ctx context.Context, path string) error {
	size, exists, err := s.cmeta.FileSize(ctx, path)
	if err != nil {
		return err
	}
	if exists {
		if size != common.CnCloneMetaAreaSize {
			return fmt.Errorf("%s is %d bytes, want %d",
				path, size, common.CnCloneMetaAreaSize)
		}
		return nil
	}
	return s.cmeta.Truncate(ctx, path, common.CnCloneMetaAreaSize)
}

func (s *CnAgentServer) ensureLoopDev(
	ctx context.Context,
	path string,
) (string, error) {
	devs, err := s.cmeta.LoopDevices(ctx, path)
	if err != nil {
		return "", err
	}
	switch len(devs) {
	case 0:
		return s.cmeta.LoopAttach(ctx, path)
	case 1:
		return devs[0], nil
	}
	return devs[0], fmt.Errorf(
		"%d loop devices back %s, want 1", len(devs), path)
}

// ensurePort converges this agent's single nvmet port — s.port.PortId, the
// --nvmet-port-id — and its three fixed ANA groups (SH19). Probing first
// keeps a converged port untouched — which is also what lets the dn and cn
// roles co-own one port in the lab, as they do whenever both default to
// common.NvmetPortId *and* their --tr-* values agree: whichever agent runs
// first creates it and the other issues zero writes. The second conjunct is
// not decoration — ProbePort compares the addr_* attributes against the
// CALLER's own PortConf, so two agents on one port id with different
// transports would each rewrite the other's every round, which is why
// different transports need different port ids.
//
// CN host-facing namespaces only ever use groups 1 (optimized) and 3
// (inaccessible); group 2 exists on every port ([D4]) but no cn code path
// assigns it.
func (s *CnAgentServer) ensurePort(
	ctx context.Context,
	t *agent.ResTracker,
) *pb.ResInfo {
	resName := fmt.Sprintf("%d", s.port.PortId)
	ok, details, err := s.nvmet.ProbePort(ctx, s.port.PortId, s.port)
	if err != nil {
		return t.Err(resKeyPort, resName, err.Error())
	}
	if ok {
		return t.Ok(resKeyPort, resName, "")
	}
	if err := s.nvmet.EnsurePort(
		ctx, s.port.PortId, s.port); err != nil {
		return t.Err(resKeyPort, resName, err.Error())
	}
	ok, details, err = s.nvmet.ProbePort(ctx, s.port.PortId, s.port)
	if err != nil {
		return t.Err(resKeyPort, resName, err.Error())
	}
	if !ok {
		return t.Err(resKeyPort, resName, details)
	}
	return t.Ok(resKeyPort, resName, "")
}

// dropRemovedCntlrs implements the CN7 pointer diff: a local cntlr whose
// pointer left the authoritative list is FORGOTTEN — its state file, its
// bitmap chunks, its memory entry and its object lock go, and its goroutines
// are cancelled. Nothing is removed from the node here; the node-level sweep
// that runs next finds every one of its resources by name.
//
// That separation is the whole point of the design. The teardown this
// replaced deleted the same state AFTER a best-effort removal pass whose
// every step only logged its failure, so a cntlr whose array would not stop
// was forgotten with its devices still live and nothing ever enumerated them
// again. Ids are never reused, so a dropped cntlr never comes back.
//
// The base state itself is never torn down — like the DN port it outlives
// every cntlr, and only lab cleanup removes it.
func (s *CnAgentServer) dropRemovedCntlrs(
	ctx context.Context,
	req *pb.SyncupCnRequest,
) {
	for _, key := range s.cntlrKeysOf(req.GetClusterId(), req.GetCnId()) {
		st := s.getCntlr(key)
		if st == nil || pointerKnown(req, st.req.GetCntlrPointer()) {
			continue
		}
		s.dropCntlrState(ctx, key, st)
	}
}

// dropCntlrState is the bookkeeping half of the old teardown: stop what this
// cntlr is running, delete what it persisted, and forget it.
func (s *CnAgentServer) dropCntlrState(
	ctx context.Context,
	key string,
	st *cntlrState,
) {
	s.stopConnectRetry(st)
	s.stopLegProbers(st, nil)
	ptr := st.req.GetCntlrPointer()
	paths := []string{s.nf.LocalCntlrPath(
		st.req.GetClusterId(), st.req.GetCnId(),
		ptr.GetSpId(), ptr.GetCntlrId())}
	paths = append(paths, s.allChunkPathsOf(st,
		st.req.GetClusterId(), st.req.GetCnId(), ptr.GetSpId())...)
	if err := s.store.Remove(ctx, paths...); err != nil {
		slog.ErrorContext(ctx, "removing cntlr state files failed",
			slog.String("error", err.Error()))
	}
	s.dropCntlr(key)
	s.locks.DropObj(key)
}

// ---------------------------------------------------------------------------
// CN28 — the node-level probe map
// ---------------------------------------------------------------------------

func (s *CnAgentServer) probeCn(
	ctx context.Context,
	st *cnState,
) *pb.CnInfo {
	req := st.req
	t := st.tracker
	info := &pb.CnInfo{}
	clusterId := req.GetClusterId()
	cnId := req.GetCnId()

	tmpfsPath := s.nf.CnTmpfsPath(clusterId, cnId)
	mounted, fsType, err := s.cmeta.Mounted(ctx, tmpfsPath)
	switch {
	case err != nil:
		info.TmpfsInfo = t.Err(resKeyTmpfs, tmpfsPath, err.Error())
	case !mounted:
		info.TmpfsInfo = t.Missing(resKeyTmpfs, tmpfsPath, "")
	case fsType != "tmpfs":
		info.TmpfsInfo = t.Err(resKeyTmpfs, tmpfsPath,
			fmt.Sprintf("filesystem is %s, want tmpfs", fsType))
	default:
		info.TmpfsInfo = t.Ok(resKeyTmpfs, tmpfsPath, "")
	}

	filePath := s.nf.CnTmpFilePath(clusterId, cnId)
	size, exists, err := s.cmeta.FileSize(ctx, filePath)
	switch {
	case err != nil:
		info.TmpFileInfo = t.Err(resKeyTmpFile, filePath, err.Error())
	case !exists:
		info.TmpFileInfo = t.Missing(resKeyTmpFile, filePath, "")
	case size != common.CnCloneMetaAreaSize:
		info.TmpFileInfo = t.Err(resKeyTmpFile, filePath,
			fmt.Sprintf("size is %d, want %d",
				size, common.CnCloneMetaAreaSize))
	default:
		info.TmpFileInfo = t.Ok(resKeyTmpFile, filePath, "")
	}

	devs, err := s.cmeta.LoopDevices(ctx, filePath)
	switch {
	case err != nil:
		info.LoopDevInfo = t.Err(resKeyLoopDev, filePath, err.Error())
	case len(devs) == 0:
		info.LoopDevInfo = t.Missing(resKeyLoopDev, filePath, "")
	case len(devs) != 1:
		info.LoopDevInfo = t.Err(resKeyLoopDev, filePath,
			fmt.Sprintf("%d loop devices, want 1", len(devs)))
	default:
		info.LoopDevInfo = t.Ok(resKeyLoopDev, filePath, devs[0])
	}

	portName := fmt.Sprintf("%d", s.port.PortId)
	ok, details, err := s.nvmet.ProbePort(ctx, s.port.PortId, s.port)
	switch {
	case err != nil:
		info.PortInfo = t.Err(resKeyPort, portName, err.Error())
	case !ok:
		info.PortInfo = t.Err(resKeyPort, portName, details)
	default:
		info.PortInfo = t.Ok(resKeyPort, portName, "")
	}
	return info
}
