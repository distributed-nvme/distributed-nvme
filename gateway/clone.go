package gateway

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md §8.9, the destination side of a copy. A Clone
// names one thin device of this SP as the destination of a dm-clone whose
// source is a namespace somewhere else; the primary cntlr does the actual
// work — connect the source, build the dm-clone, reload the td's namespaces
// onto it (fig. `090Clone`). The gateway only ever writes desired state and
// bumps SpRev, and never waits for a cntlr to act on it.
//
// Four of the five RPCs are the plain SP-scoped shapes of gateway.md §4.
// DeleteClone is the exception: with force = false it must PROVE the copy
// finished before it may take the dm-clone away, and that proof lives only on
// the primary's CN — hence the two-phase AG4 shape.

// The op names the bump helper cites; they are the RPC names so a log line
// names something greppable.
const (
	opCreateClone       = "CreateClone"
	opDeleteClone       = "DeleteClone"
	opUpdateCloneTrConf = "UpdateCloneTrConf"
	opAppendCloneBitmap = "AppendCloneBitmap"
)

// loadClone reads one Clone of an already resolved SP. Every clone RPC but
// CreateClone opens with it, so "absent ⇒ NOT_FOUND" (GW7) is stated once.
func loadClone(
	stm etcdutil.STM,
	sc *spScope,
	cloneName string,
) (*pb.Clone, error) {
	clone := &pb.Clone{}
	if !stm.Get(model.CloneKey(sc.Cid, sc.SpId(), cloneName), clone) {
		return nil, errNotFound("clone %q not found", cloneName)
	}
	return clone, nil
}

// CreateClone is architecture.md §8.9's CreateClone: one Clone record, one
// name in `clone_name_list`, one SpRev bump. Everything the primary needs to
// build the dm-clone is in that record, so the RPC is pure etcd.
//
// Two of the section's rules are deliberately NOT enforced here. The
// destination td MUST be empty — freshly created and never written — because
// §11.5 crash recovery equates "mapped in the destination thin pool" with
// "already copied"; the CP cannot see whether a td was written, so that
// stays a documented-unverifiable contract ([D3]). And the ceiling that
// actually binds is the primary CN's clone-metadata arena, shared by every
// clone of every cntlr on that CN, not MaxCloneCntPerSp; v1 does not track
// it (§0 #16, architecture.md §8.9's admission note), so an over-committed clone is created
// normally and reports RES_STATUS_ERROR until arena units free up.
func (s *Server) CreateClone(
	ctx context.Context,
	req *pb.CreateCloneRequest,
) (*pb.CreateCloneReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("clone_name", req.GetCloneName()); err != nil {
		return nil, err
	}
	if err := validateName("dst_td_name", req.GetDstTdName()); err != nil {
		return nil, err
	}
	if err := validateTrConfList(
		"src_tr_conf", req.GetSrcTrConf(),
	); err != nil {
		return nil, err
	}
	if err := validateNqn("src_nqn", req.GetSrcNqn()); err != nil {
		return nil, err
	}
	if err := validateCloneGeometry(
		req.GetSrcSliceCnt(), req.GetSrcStripeSize(), req.GetSrcBlockSize(),
	); err != nil {
		return nil, err
	}
	if err := validateDmCloneConf(
		"dm_clone_conf", req.GetDmCloneConf(),
	); err != nil {
		return nil, err
	}
	var cloneId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		cloneId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		if len(sc.Conf.GetCloneNameList()) >= common.MaxCloneCntPerSp {
			return errExhausted(
				"storage pool %q holds %d clones, the maximum is %d",
				req.GetSpName(), len(sc.Conf.GetCloneNameList()),
				common.MaxCloneCntPerSp)
		}
		cloneKey := model.CloneKey(sc.Cid, sc.SpId(), req.GetCloneName())
		// Both halves are checked: the list is what every other RPC walks,
		// the key is what actually holds the record, and a create must not
		// overwrite either one if they ever disagree.
		if containsName(sc.Conf.GetCloneNameList(), req.GetCloneName()) ||
			stm.Get(cloneKey, &pb.Clone{}) {
			return errExists("clone %q already exists", req.GetCloneName())
		}
		td := &pb.ThinDevice{}
		tdKey := model.ThinDeviceKey(sc.Cid, sc.SpId(), req.GetDstTdName())
		if !stm.Get(tdKey, td) {
			return errNotFound(
				"thin device %q not found", req.GetDstTdName())
		}
		// One td can be the destination of at most one clone: two dm-clones
		// writing the same raid0 would each believe they own its regions.
		// The scan is bounded by MaxCloneCntPerSp.
		for _, name := range sc.Conf.GetCloneNameList() {
			otherKey := model.CloneKey(sc.Cid, sc.SpId(), name)
			other := &pb.Clone{}
			if !stm.Get(otherKey, other) {
				return errAborted("clone key %q is missing", otherKey)
			}
			if other.GetDstTdId() == td.GetTdId() {
				return errPrecondition(
					"thin device %q is already the destination of clone %q",
					req.GetDstTdName(), name)
			}
		}
		minter := newSpIdMinter(sc.Conf)
		cloneId = minter.mint()
		stm.Put(cloneKey, &pb.Clone{
			CloneId:       cloneId,
			SrcTrConfList: req.GetSrcTrConf(),
			SrcNqn:        req.GetSrcNqn(),
			SrcNsIdx:      req.GetSrcNsIdx(),
			SrcSliceCnt:   req.GetSrcSliceCnt(),
			SrcStripeSize: req.GetSrcStripeSize(),
			SrcBlockSize:  req.GetSrcBlockSize(),
			DstTdId:       td.GetTdId(),
			DmCloneConf:   req.GetDmCloneConf(),
			AutoResume:    req.GetAutoResume(),
			// The clone starts with no source bitmap; AppendCloneBitmap
			// grows the count, and the chunks are a pure optimization that
			// may arrive at any time (§8.9).
			BmCnt: 0,
		})
		sc.Conf.CloneNameList = append(
			sc.Conf.CloneNameList, req.GetCloneName())
		minter.commit(sc.Conf)
		stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), sc.Conf)
		return bumpSp(stm, opCreateClone, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.CreateCloneReply{CloneId: cloneId}, nil
}

// clonePhase1 is what DeleteClone's read-only phase carries into its agent
// call. AG4 makes every field a hint and nothing more: phase 2 re-resolves all
// of it and re-checks the token, so for a token-carrying caller an interleaved
// mutation turns into ABORTED rather than into a decision taken on stale
// facts. A token-less caller keeps the re-resolution but not that guarantee
// (GW6 is presence-based, §0 #7).
type clonePhase1 struct {
	ClusterId uint64
	SpId      uint64
	CntlrId   uint64
	CnId      uint64
	AddrPort  string
	CloneId   uint64
}

// DeleteClone is architecture.md §8.9's DeleteClone, the two-phase RPC of
// AG4: a read-only Snapshot, then — for force = false — one GetCntlrInfo
// against the primary's CN agent, then the deciding STM.
//
// force = false is a proof obligation, not a best effort. Removing the
// dm-clone while regions are still unhydrated would silently lose every
// byte that was never pulled from the source, so the RPC refuses unless the
// primary's `clone_id_to_dm_clone` row shows the copy complete. An
// unreachable agent is FAILED_PRECONDITION for exactly the same reason
// (§8.9): silence is not proof. force = true skips the whole check and is
// how an operator abandons a clone whose source is gone.
//
// The deciding STM also resumes the destination's namespaces: §11.3 has them
// suspended (created that way, or flipped) before the clone is made, and the
// clone record is the only thing that remembers why. Clearing the flag in the
// same transaction that removes the clone is what stops a delete from leaving
// a namespace ANA-inaccessible for ever.
func (s *Server) DeleteClone(
	ctx context.Context,
	req *pb.DeleteCloneRequest,
) (*pb.DeleteCloneReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("clone_name", req.GetCloneName()); err != nil {
		return nil, err
	}
	var phase1 clonePhase1
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		phase1 = clonePhase1{}
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		clone, err := loadClone(stm, sc, req.GetCloneName())
		if err != nil {
			return err
		}
		if req.GetForce() {
			// No agent call follows, so the primary need not even exist:
			// force is what deletes a clone whose cntlr is unreachable.
			return nil
		}
		cntlrs, err := loadCntlrs(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		cntlrId, cntlr, ok := primaryCntlr(sc.Conf, cntlrs)
		if !ok {
			return errPrecondition(
				"storage pool %q has no primary cntlr to prove hydration "+
					"with", req.GetSpName())
		}
		cn := &pb.CnConf{}
		cnKey := model.CnConfKey(sc.Cid, cntlr.GetAddrPort())
		if !stm.Get(cnKey, cn) {
			// GW7: NOT_FOUND is for an object the REQUEST named. This CN
			// is named by a live cntlr of the SP, and DeleteControllerNode
			// refuses a CN whose cntlr_ptr_list is non-empty, so its absence
			// is a lost invariant key — §5.9's ABORTED.
			return errAborted("cn_conf key %q is missing", cnKey)
		}
		phase1 = clonePhase1{
			ClusterId: sc.Cid,
			SpId:      sc.SpId(),
			CntlrId:   cntlrId,
			CnId:      cn.GetCnId(),
			AddrPort:  cntlr.GetAddrPort(),
			CloneId:   clone.GetCloneId(),
		}
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	if !req.GetForce() {
		if err := checkCloneHydrated(
			ctx, phase1, req.GetCloneName(),
		); err != nil {
			return nil, err
		}
	}
	var cloneId uint64
	err = s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		cloneId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		clone, err := loadClone(stm, sc, req.GetCloneName())
		if err != nil {
			return err
		}
		if err := resumeCloneDstNs(stm, sc, clone.GetDstTdId()); err != nil {
			return err
		}
		stm.Del(model.CloneKey(sc.Cid, sc.SpId(), req.GetCloneName()))
		// An STM cannot range, so the chunks are deleted one by one from the
		// count the record itself carries — which is exactly what
		// AppendCloneBitmap maintains it for.
		for idx := uint32(0); idx < clone.GetBmCnt(); idx++ {
			stm.Del(model.CloneBitmapKey(
				sc.Cid, sc.SpId(), req.GetCloneName(), idx))
		}
		sc.Conf.CloneNameList = removeName(
			sc.Conf.GetCloneNameList(), req.GetCloneName())
		stm.Put(model.SpConfKey(sc.Cid, req.GetSpName()), sc.Conf)
		cloneId = clone.GetCloneId()
		return bumpSp(stm, opDeleteClone, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.DeleteCloneReply{CloneId: cloneId}, nil
}

// checkCloneHydrated is the between-the-phases agent call of DeleteClone
// (AG1, AG3): the primary's `clone_id_to_dm_clone` row carries the raw
// dm-clone status line, and hydrationComplete refuses to read anything but a
// finished copy out of it.
//
// Both failure modes are FAILED_PRECONDITION rather than the usual ABORTED
// of AG3, because both mean the same thing to the caller: hydration could
// not be proven complete, so retry later or pass force (§8.9).
func checkCloneHydrated(
	ctx context.Context,
	phase1 clonePhase1,
	cloneName string,
) error {
	details := ""
	callErr := withCnAgent(ctx, phase1.AddrPort,
		func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
			reply, err := client.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
				ClusterId: phase1.ClusterId,
				CnId:      phase1.CnId,
				CntlrPointer: &pb.CntlrPointer{
					SpId:    phase1.SpId,
					CntlrId: phase1.CntlrId,
				},
			})
			if err != nil {
				return err
			}
			details = reply.GetCntlrInfo().
				GetCloneIdToDmClone()[phase1.CloneId].GetDetails()
			return nil
		})
	if callErr != nil {
		return errPrecondition(
			"clone %q: hydration is unproven, controller node %q did not "+
				"answer: %v", cloneName, phase1.AddrPort, callErr)
	}
	if !hydrationComplete(details) {
		return errPrecondition(
			"clone %q has not finished hydrating", cloneName)
	}
	return nil
}

// resumeCloneDstNs clears `suspended` on every namespace of the SP backed by
// the clone's destination td (§8.9 Action). Only the subsystems that actually
// change are written back, so an SP with many subsystems produces the
// smallest write set that still describes the new desired state.
//
// A listed nqn whose Subsystem key is gone aborts the RPC (§5.9): the SP has
// lost an invariant key, and half-resuming the namespaces of an SP is worse
// than refusing — the caller can retry, whereas a namespace left suspended
// with no clone to explain it never resumes on its own.
func resumeCloneDstNs(
	stm etcdutil.STM,
	sc *spScope,
	dstTdId uint64,
) error {
	for _, nqn := range sc.Conf.GetNqnList() {
		ssKey := model.SubsystemKey(sc.Cid, sc.SpId(), nqn)
		subsystem := &pb.Subsystem{}
		if !stm.Get(ssKey, subsystem) {
			return errAborted("subsystem key %q is missing", ssKey)
		}
		changed := false
		for _, ns := range subsystem.GetNsList() {
			if ns.GetTdId() != dstTdId || !ns.GetSuspended() {
				continue
			}
			ns.Suspended = false
			changed = true
		}
		if changed {
			stm.Put(ssKey, subsystem)
		}
	}
	return nil
}

// GetClone is architecture.md §8.9's GetClone: one consistency read at a
// single store revision, no token and no bump.
func (s *Server) GetClone(
	ctx context.Context,
	req *pb.GetCloneRequest,
) (*pb.GetCloneReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("clone_name", req.GetCloneName()); err != nil {
		return nil, err
	}
	var clone *pb.Clone
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		clone = nil
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		found, err := loadClone(stm, sc, req.GetCloneName())
		if err != nil {
			return err
		}
		clone = found
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.GetCloneReply{Clone: clone}, nil
}

// UpdateCloneTrConf is architecture.md §8.9's UpdateCloneTrConf: the source
// SP's cntlrs moved, so the addresses the primary reconnects to are replaced
// wholesale. The list is a replacement and never a merge — an address that
// is gone must stop being retried — and the bump is what tells the primary
// to reconnect.
func (s *Server) UpdateCloneTrConf(
	ctx context.Context,
	req *pb.UpdateCloneTrConfRequest,
) (*pb.UpdateCloneTrConfReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("clone_name", req.GetCloneName()); err != nil {
		return nil, err
	}
	if err := validateTrConfList(
		"src_tr_conf", req.GetSrcTrConf(),
	); err != nil {
		return nil, err
	}
	var cloneId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		cloneId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		clone, err := loadClone(stm, sc, req.GetCloneName())
		if err != nil {
			return err
		}
		clone.SrcTrConfList = req.GetSrcTrConf()
		stm.Put(model.CloneKey(
			sc.Cid, sc.SpId(), req.GetCloneName()), clone)
		cloneId = clone.GetCloneId()
		return bumpSp(stm, opUpdateCloneTrConf, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.UpdateCloneTrConfReply{CloneId: cloneId}, nil
}

// AppendCloneBitmap is architecture.md §8.9's AppendCloneBitmap: one chunk of
// the SOURCE bitmap, addressed by the source slice it describes.
//
// `bm_idx` is the source `slice_idx` and not an append sequence, so a chunk
// is addressed rather than allocated: a caller that pages one source slice's
// bitmap sends every page under that slice's own index, and each page is
// appended to the chunk already there (the STM below; a page is never a
// replacement, or paging would keep only the last one). Re-addressing the
// same slice is therefore what makes `bm_cnt` a high-water mark (max, never
// +1) — it is what DeleteClone deletes the chunk keys from, and lowering it
// would orphan them.
//
// Both bounds are INVALID_ARGUMENT and not the RESOURCE_EXHAUSTED of GW7's
// Append*Bitmap row: that row is AppendMigrationBitmap's `bm_cnt ≥
// MaxMigrBmCnt`, a ceiling reached by previous appends, whereas these two
// judge the slice_idx of THIS request against a geometry the clone was
// created with (§8.9 Errors).
//
// The bytes are stored exactly as sent: bitmaps are opaque to the gateway
// (GW14, [D-J]) — the 1 = never-written, LSB-first convention is the agents'
// and the callers'.
func (s *Server) AppendCloneBitmap(
	ctx context.Context,
	req *pb.AppendCloneBitmapRequest,
) (*pb.AppendCloneBitmapReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("clone_name", req.GetCloneName()); err != nil {
		return nil, err
	}
	if err := validateBitmap(req.GetBitmap()); err != nil {
		return nil, err
	}
	var cloneId uint64
	err := s.cli.RunSTM(ctx, func(stm etcdutil.STM) error {
		cloneId = 0
		sc, err := openSp(stm, req.GetClusterName(), req.GetSpName(),
			req.GetSpRev())
		if err != nil {
			return err
		}
		clone, err := loadClone(stm, sc, req.GetCloneName())
		if err != nil {
			return err
		}
		sliceIdx := req.GetSliceIdx()
		if sliceIdx >= clone.GetSrcSliceCnt() {
			return errInvalid(
				"slice_idx %d is not below the clone's src_slice_cnt %d",
				sliceIdx, clone.GetSrcSliceCnt())
		}
		if sliceIdx >= common.MaxCloneBmCnt {
			return errInvalid("slice_idx %d is not below %d",
				sliceIdx, common.MaxCloneBmCnt)
		}
		// §8.9 Action: "APPEND the bytes to CloneBitmap key
		// bm_idx = slice_idx (create if absent)". The chunk grows; it is
		// never replaced. Callers page one source slice's bitmap through
		// GetThinDeviceBitmap and hand each page here, and the concatenation
		// of those pages IS the slice's bitmap — bit k of the stored value
		// covers block k of that slice (§9.6, [D8]). Overwriting would keep
		// only the last page and place its bits at block 0, which
		// PushCloneBitmap would then hand the primary as "these low blocks
		// were never written" and the agent would blkdiscard regions the
		// source really wrote.
		//
		// The zero value of the read is what makes "create if absent" fall
		// out: an absent key decodes to an empty bitmap and the append is the
		// request's bytes alone.
		bmKey := model.CloneBitmapKey(
			sc.Cid, sc.SpId(), req.GetCloneName(), sliceIdx)
		chunk := &pb.CloneBitmap{}
		stm.Get(bmKey, chunk)
		chunk.Bitmap = append(chunk.GetBitmap(), req.GetBitmap()...)
		stm.Put(bmKey, chunk)
		if sliceIdx+1 > clone.GetBmCnt() {
			clone.BmCnt = sliceIdx + 1
		}
		stm.Put(model.CloneKey(
			sc.Cid, sc.SpId(), req.GetCloneName()), clone)
		cloneId = clone.GetCloneId()
		return bumpSp(stm, opAppendCloneBitmap, sc)
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.AppendCloneBitmapReply{CloneId: cloneId}, nil
}
