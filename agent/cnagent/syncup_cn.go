package cnagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Reconcile is the SH1 startup pass (CN2): load the local store — while a
// cn-* file does not load, no cntlr or chunk whose CN is not loaded is loaded
// or deleted, and while a cntlr-* file does not load, neither is a chunk
// whose cntlr is not loaded but whose loaded CN still names that cntlr —
// converge every loaded CN's base state (architecture.md, Controller node,
// common), tear down cntlrs
// whose pointer left their CN's list, converge the rest — which, per CN18,
// runs the recovery of architecture.md, Clone crash recovery, for
// any clone whose metadata wrapper is gone or mismatched, whose dm-clone has
// vanished, or whose dm-clone does not show hydration enabled (after a CN
// reboot the tmpfs arena and the dm state are both empty; after a plain agent
// restart both survive, and the converge re-applies a clone whose build had
// enabled hydration as a no-op and recovers one the dead agent left short of
// that) — and finally sweeps the kind-`cb` wrappers no stored cntlr
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

	// unreadCn is set when a cn-* file did not load. Such a file names no CN
	// — the ids come from the decoded request, never from the file name
	// (SH6) — so any cntlr whose CN is not loaded may be one its list still
	// names, and nothing read here proves that it left that list.
	unreadCn := false
	for _, path := range files[agent.StoreKindCn] {
		req := &pb.SyncupCnRequest{}
		if err := s.store.Load(ctx, path, req); err != nil {
			slog.ErrorContext(ctx, "skipping unreadable cn state file",
				slog.String("path", path),
				slog.String("error", err.Error()))
			unreadCn = true
			continue
		}
		s.putCn(cnKey(req.GetClusterId(), req.GetCnId()), newCnState(req))
	}
	// unreadParent reports whether a cntlr or a chunk of the CN (clusterId,
	// cnId) goes with a cn-* file that did not load: its CN is not loaded
	// while such a file exists. Those are skipped with their CN — neither
	// loaded nor deleted, so nothing of them is converged, swept or applied.
	// Loading them would hand them to the pointer-absent branch below, which
	// reads a missing CN as "this cntlr left its parent's list" and deletes
	// the cntlr's request and chunks for want of a list that could not be
	// read. Out of memory, the CN and each such cntlr answer their Check
	// rounds with UnknownObject, so the worker re-sends the SyncupCn, which
	// rewrites the file, and then each SyncupCntlr, which CN8 admits once
	// that SyncupCn has put its pointer back.
	unreadParent := func(clusterId, cnId uint64) bool {
		return unreadCn && s.getCn(cnKey(clusterId, cnId)) == nil
	}
	// unreadCntlr is unreadCn one level down: set when a cntlr-* file did not
	// load, which names no cntlr either.
	unreadCntlr := false
	for _, path := range files[agent.StoreKindCntlr] {
		req := &pb.SyncupCntlrRequest{}
		if err := s.store.Load(ctx, path, req); err != nil {
			slog.ErrorContext(ctx, "skipping unreadable cntlr state file",
				slog.String("path", path),
				slog.String("error", err.Error()))
			unreadCntlr = true
			continue
		}
		if unreadParent(req.GetClusterId(), req.GetCnId()) {
			slog.WarnContext(ctx,
				"skipping cntlr state file of an unloaded cn",
				slog.String("path", path),
				slog.Uint64("cluster_id", req.GetClusterId()),
				slog.Uint64("cn_id", req.GetCnId()))
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
	// A chunk whose cntlr is not loaded is an orphan unless it goes with a
	// cn-* file that did not load (unreadParent), or with a cntlr-* file that
	// did not load while its loaded CN still names its cntlr (below).
	var orphans []string
	for _, path := range files[agent.StoreKindCloneBm] {
		chunk := &pb.PushCloneBitmapRequest{}
		if err := s.store.Load(ctx, path, chunk); err != nil {
			slog.ErrorContext(ctx, "skipping unreadable bitmap chunk file",
				slog.String("path", path),
				slog.String("error", err.Error()))
			continue
		}
		// Not an orphan: nothing read here proves its cntlr gone.
		if unreadParent(chunk.GetClusterId(), chunk.GetCnId()) {
			slog.WarnContext(ctx,
				"skipping bitmap chunk file of an unloaded cn",
				slog.String("path", path),
				slog.Uint64("cluster_id", chunk.GetClusterId()),
				slog.Uint64("cn_id", chunk.GetCnId()))
			continue
		}
		ptr := chunk.GetCntlrPointer()
		st := s.getCntlr(cntlrKey(chunk.GetClusterId(), chunk.GetCnId(),
			ptr.GetSpId(), ptr.GetCntlrId()))
		// Nor, while a cntlr-* file did not load, is a chunk whose cntlr is
		// not loaded but whose loaded CN still names that cntlr: the file may
		// be this cntlr's, so nothing read here proves the cntlr gone. A
		// chunk whose CN no longer names its cntlr is an orphan whatever
		// cntlr-* file failed to decode — the cntlr left the list, and its
		// chunks go with it (SH7) — and so is one whose CN is not loaded
		// (with no cn-* file unread, see above), which reads as a list that
		// names no cntlr.
		if st == nil && unreadCntlr {
			cn := s.getCn(cnKey(chunk.GetClusterId(), chunk.GetCnId()))
			if cn != nil && pointerKnown(cn.loadReq(), ptr) {
				slog.WarnContext(ctx,
					"skipping bitmap chunk file of an unloaded cntlr",
					slog.String("path", path),
					slog.Uint64("sp_id", ptr.GetSpId()),
					slog.Uint64("cntlr_id", ptr.GetCntlrId()))
				continue
			}
		}
		if st == nil || findClone(st.loadReq(), chunk.GetCloneId()) == nil {
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
	// ever looked again. A missing CN reads as a list that names no cntlr —
	// unless a cn-* file did not load, and then its cntlrs never got this
	// far (unreadParent).
	for _, key := range s.allCntlrKeys() {
		st := s.getCntlr(key)
		req := st.loadReq()
		cn := s.getCn(cnKey(req.GetClusterId(), req.GetCnId()))
		if cn == nil ||
			!pointerKnown(cn.loadReq(), req.GetCntlrPointer()) {
			s.dropCntlrState(ctx, key, st)
		}
	}
	// The node-level sweep, which also owns the kind-cb wrapper sweep. It can
	// run before the cntlr converges because its wanted set comes from the
	// stored REQUESTS, not from what happens to exist: a clone this pass is
	// about to (re)build is named by its cntlr's clone_list already.
	for _, key := range s.cnKeys() {
		s.sweepCn(ctx, s.getCn(key), true, nil)
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
		stored = st.loadReq().GetRevision()
	}
	if reject := agent.GateRevision(stored, req.GetRevision()); reject != nil {
		return &pb.SyncupCnReply{AgentReply: reject, Revision: stored}
	}
	if st == nil {
		st = newCnState(req)
	} else {
		st.storeReq(req)
	}
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
	sweep := s.sweepCn(ctx, st, true, nil)

	return &pb.SyncupCnReply{
		AgentReply: sweep.Reply(),
		Revision:   req.GetRevision(),
		CnInfo:     info,
	}
}

// convergeCn builds the once-per-CN base state of architecture.md, Controller
// node, common, probe-first (CN5),
// recording each resource's outcome as it goes. A failed resource never aborts
// the pass (CN29).
//
// CN6: QoS is accepted and deliberately **not** enforced in this version. The
// step 4 open issue of architecture.md, Controller node, common, stands —
// `io.max` written from any agent-created
// cgroup binds the agent's own tools, not the nvmet kernel threads that carry
// host IO — so the agent persists `qos_ratio` with the request (SH5 does that
// for free), applies nothing, and reports no QoS resource in CnInfo.
func (s *CnAgentServer) convergeCn(
	ctx context.Context,
	st *cnState,
) *pb.CnInfo {
	req := st.loadReq()
	t := st.tracker
	info := &pb.CnInfo{}
	clusterId := req.GetClusterId()
	cnId := req.GetCnId()

	tmpfsPath := s.nf.CnTmpfsPath(clusterId, cnId)
	tmpfsErr := s.ensureTmpfs(ctx, tmpfsPath)
	info.TmpfsInfo = t.FromErr(resKeyTmpfs, tmpfsPath, "", tmpfsErr)

	// The file and the loop device are created only on a tmpfs this pass
	// found or mounted at the path. Without one the mountpoint may be a bare
	// directory — left in a /tmp a reboot kept, or made by the `mkdir -p` of
	// a refused mount — and the file would land on the filesystem that holds
	// it, the loop on that file; the next converge's mount would hide the
	// file, and a fresh one and a second loop would follow (CN5).
	tmpfsOk := tmpfsErr == nil
	filePath := s.nf.CnTmpFilePath(clusterId, cnId)
	info.TmpFileInfo = t.FromErr(resKeyTmpFile, filePath, "",
		s.ensureTmpFile(ctx, filePath, tmpfsPath, tmpfsOk))

	// The loop device is the whole of the clone-metadata arena: CN18 carves it
	// into kind-`cb` wrapper linears and needs no volume manager on top
	// ([D14]).
	loopDev, loopErr := s.ensureLoopDev(ctx, filePath, tmpfsPath, tmpfsOk)
	info.LoopDevInfo = t.FromErr(resKeyLoopDev, filePath, loopDev, loopErr)

	info.PortInfo = s.ensurePort(ctx, t)
	return info
}

// ensureTmpfs mounts the arena's tmpfs when the probe answers that nothing is
// mounted there. A probe that did not answer is run once more, from its first
// call, in the same pass: all a caller learns from a kill is that it must ask
// again (agent.Reported), and an answer to the second asking is as good as
// one to the first — a tmpfs that really is absent, after a reboot, is then
// mounted by this pass. When the second does not answer either, it is the
// row's error and nothing is mounted: a mount over the live arena would stack
// a second tmpfs on it (CN5).
func (s *CnAgentServer) ensureTmpfs(ctx context.Context, path string) error {
	mounted, fsType, err := s.cmeta.Mounted(ctx, path)
	if err != nil {
		mounted, fsType, err = s.cmeta.Mounted(ctx, path)
	}
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
// allocator's hole-punch discard frees them again. As in ensureTmpfs, a `stat`
// that did not answer, or whose answer is not a size, is asked once more in
// the same pass, and when that one fails too it is the row's error and
// truncates nothing (CN5). An absent file is created only when tmpfsOk says
// the tmpfs step found or mounted a tmpfs at tmpfsPath; otherwise it is the
// row's error, and a file that is there is still probed and reported.
func (s *CnAgentServer) ensureTmpFile(
	ctx context.Context,
	path string,
	tmpfsPath string,
	tmpfsOk bool,
) error {
	size, exists, err := s.cmeta.FileSize(ctx, path)
	if err != nil {
		size, exists, err = s.cmeta.FileSize(ctx, path)
	}
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
	if !tmpfsOk {
		return fmt.Errorf("%s is absent and not created: no tmpfs is "+
			"confirmed at %s", path, tmpfsPath)
	}
	return s.cmeta.Truncate(ctx, path, common.CnCloneMetaAreaSize)
}

// ensureLoopDev attaches the one loop device to the arena file when none
// backs it, and, like ensureTmpFile, only on a confirmed tmpfs (tmpfsOk).
func (s *CnAgentServer) ensureLoopDev(
	ctx context.Context,
	path string,
	tmpfsPath string,
	tmpfsOk bool,
) (string, error) {
	devs, err := s.cmeta.LoopDevices(ctx, path)
	if err != nil {
		return "", err
	}
	switch len(devs) {
	case 0:
		if !tmpfsOk {
			return "", fmt.Errorf("no loop device backs %s and none is "+
				"attached: no tmpfs is confirmed at %s", path, tmpfsPath)
		}
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
		if st == nil || pointerKnown(req, st.loadReq().GetCntlrPointer()) {
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
	req := st.loadReq()
	ptr := req.GetCntlrPointer()
	paths := []string{s.nf.LocalCntlrPath(
		req.GetClusterId(), req.GetCnId(),
		ptr.GetSpId(), ptr.GetCntlrId())}
	paths = append(paths, s.allChunkPathsOf(st,
		req.GetClusterId(), req.GetCnId(), ptr.GetSpId())...)
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

// errBaseStateAbsent and errAnaStateDiffers are the failures the node-level
// verdict records for what the round's probe read of the base state that a
// SyncupCn would cure (CN30): a piece of it absent, or an ANA group of the
// port in a state other than its fixed one.
var (
	errBaseStateAbsent = errors.New("absent")
	errAnaStateDiffers = errors.New("differs")
)

// baseRedrive is one piece of the base state (architecture.md, Controller
// node, common) that a probe read absent,
// or an ANA group it read in a state other than its fixed one, in the words
// the node-level verdict reports it with (sweepCn).
type baseRedrive struct {
	what string
	err  error
}

// probeCn reads the base state (architecture.md, Controller node, common) into
// its rows, and also returns what of
// it the probe read wrong in a way a SyncupCn's converge would cure (CN30):
// an absent tmpfs, which convergeCn mounts; an absent arena file or loop
// device, which it creates only on a tmpfs it found or mounted, so only
// while this probe read the tmpfs either there or absent; an absent port
// directory or ANA group, which EnsurePort makes; and an ANA group in a
// state other than its fixed one on a port whose transport attributes all
// match, which EnsurePort rewrites, since nvmet takes an ana_state write
// whatever is linked to the port. A probe that did not answer proves
// nothing, and the next round asks again. The rest of what is there but not
// as wanted is left to its row: a filesystem of another type at the path
// (and with it the arena file and loop device the converge would not create
// on it), a file of another size and more than one loop device, which the
// converge reports and leaves as they are, so a re-sent SyncupCn could not
// cure them; and a transport attribute of the port that differs, which nvmet
// refuses to rewrite while a subsystem is linked to the port (EACCES), so
// that while one is every re-send would fail on it the same way. None of
// those is returned.
func (s *CnAgentServer) probeCn(
	ctx context.Context,
	st *cnState,
) (*pb.CnInfo, []baseRedrive) {
	req := st.loadReq()
	t := st.tracker
	info := &pb.CnInfo{}
	var redrive []baseRedrive
	clusterId := req.GetClusterId()
	cnId := req.GetCnId()

	tmpfsPath := s.nf.CnTmpfsPath(clusterId, cnId)
	// arenaWanted is the converge's tmpfsOk as this probe can foresee it:
	// the file and the loop device are created on a tmpfs the converge
	// found, or on one it mounts where none was (CN5).
	arenaWanted := false
	mounted, fsType, err := s.cmeta.Mounted(ctx, tmpfsPath)
	switch {
	case err != nil:
		info.TmpfsInfo = t.Err(resKeyTmpfs, tmpfsPath, err.Error())
	case !mounted:
		info.TmpfsInfo = t.Missing(resKeyTmpfs, tmpfsPath, "")
		redrive = append(redrive,
			baseRedrive{"tmpfs " + tmpfsPath, errBaseStateAbsent})
		arenaWanted = true
	case fsType != "tmpfs":
		info.TmpfsInfo = t.Err(resKeyTmpfs, tmpfsPath,
			fmt.Sprintf("filesystem is %s, want tmpfs", fsType))
	default:
		info.TmpfsInfo = t.Ok(resKeyTmpfs, tmpfsPath, "")
		arenaWanted = true
	}

	filePath := s.nf.CnTmpFilePath(clusterId, cnId)
	size, exists, err := s.cmeta.FileSize(ctx, filePath)
	switch {
	case err != nil:
		info.TmpFileInfo = t.Err(resKeyTmpFile, filePath, err.Error())
	case !exists:
		info.TmpFileInfo = t.Missing(resKeyTmpFile, filePath, "")
		if arenaWanted {
			redrive = append(redrive, baseRedrive{
				"clone metadata arena file " + filePath,
				errBaseStateAbsent})
		}
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
		if arenaWanted {
			redrive = append(redrive, baseRedrive{
				"loop device of " + filePath, errBaseStateAbsent})
		}
	case len(devs) != 1:
		info.LoopDevInfo = t.Err(resKeyLoopDev, filePath,
			fmt.Sprintf("%d loop devices, want 1", len(devs)))
	default:
		info.LoopDevInfo = t.Ok(resKeyLoopDev, filePath, devs[0])
	}

	portName := fmt.Sprintf("%d", s.port.PortId)
	portState, details, err := s.nvmet.ProbePortState(
		ctx, s.port.PortId, s.port)
	switch {
	case err != nil:
		info.PortInfo = t.Err(resKeyPort, portName, err.Error())
	case portState == agent.PortAbsent:
		info.PortInfo = t.Err(resKeyPort, portName, details)
		redrive = append(redrive, baseRedrive{"nvmet port " + portName,
			fmt.Errorf("%w: %s", errBaseStateAbsent, details)})
	case portState == agent.PortAnaStateMismatch:
		info.PortInfo = t.Err(resKeyPort, portName, details)
		redrive = append(redrive, baseRedrive{"nvmet port " + portName,
			fmt.Errorf("%w: %s", errAnaStateDiffers, details)})
	case portState != agent.PortOk:
		// A transport attribute that differs (PortAttrMismatch): not
		// re-driven, see above.
		info.PortInfo = t.Err(resKeyPort, portName, details)
	default:
		info.PortInfo = t.Ok(resKeyPort, portName, "")
	}
	return info, redrive
}
