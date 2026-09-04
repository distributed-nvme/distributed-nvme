package cnagent

import (
	"bytes"
	"context"
	"log/slog"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// pushCloneBitmap implements CN22: gate, persist, then apply. The node read
// lock and this cntlr's object lock are held by the caller.
func (s *CnAgentServer) pushCloneBitmap(
	ctx context.Context,
	key string,
	req *pb.PushCloneBitmapRequest,
) *pb.PushCloneBitmapReply {
	st := s.getCntlr(key)
	if st == nil {
		return &pb.PushCloneBitmapReply{
			AgentReply: agent.UnknownObjectReply(
				"unknown cntlr %s",
				cntlrPointerText(req.GetCntlrPointer())),
		}
	}
	clone := findClone(st.req, req.GetCloneId())
	if clone == nil {
		return &pb.PushCloneBitmapReply{
			AgentReply: agent.UnknownObjectReply(
				"unknown clone %d on cntlr %s", req.GetCloneId(),
				cntlrPointerText(req.GetCntlrPointer())),
		}
	}
	if req.GetBmIdx() >= common.MaxCloneBmCnt ||
		req.GetBmIdx() >= clone.GetSrcSliceCnt() {
		return &pb.PushCloneBitmapReply{
			AgentReply: agent.UnknownObjectReply(
				"bm_idx %d out of range for clone %d (src_slice_cnt %d)",
				req.GetBmIdx(), req.GetCloneId(), clone.GetSrcSliceCnt()),
		}
	}
	// A push never advances the stored revision; it only may not be older.
	if reject := agent.GateRevision(
		st.req.GetRevision(), req.GetRevision()); reject != nil {
		return &pb.PushCloneBitmapReply{AgentReply: reject}
	}

	set := st.chunkSet(req.GetCloneId())
	if stored, ok := set.Get(req.GetBmIdx()); ok &&
		bytes.Equal(stored, req.GetBitmap()) {
		// Already persisted and applied — the chunk stays in the applied set
		// and nothing needs re-doing.
		return &pb.PushCloneBitmapReply{AgentReply: agent.OkReply()}
	}

	ptr := req.GetCntlrPointer()
	path := s.nf.LocalCloneBmPath(req.GetClusterId(), req.GetCnId(),
		ptr.GetSpId(), req.GetCloneId(), req.GetBmIdx())
	if err := s.store.Save(ctx, path, req); err != nil {
		// Not persisted ⇒ not applied: the chunk stays out of the applied
		// set, so the worker re-pushes it on its next round (§9.6).
		slog.ErrorContext(ctx, "persisting clone bitmap chunk failed",
			slog.String("path", path),
			slog.String("error", err.Error()))
		return &pb.PushCloneBitmapReply{AgentReply: agent.OkReply()}
	}
	set.Put(req.GetBmIdx(), req.GetBitmap())

	// A chunk whose dm-clone does not currently exist (standby, not built
	// yet, level-suppressed) still counts as applied; chunks are re-applied
	// whenever the dm-clone is (re)created (CN18 step 4).
	plan := newCntlrPlan(s.nf, st.req)
	if cp := plan.cloneById[req.GetCloneId()]; cp != nil {
		if dev, err := s.dm.Info(ctx, cp.finalName); err == nil &&
			dev != nil {
			s.applyCloneChunks(ctx, st, plan, cp)
		}
	}
	return &pb.PushCloneBitmapReply{AgentReply: agent.OkReply()}
}

func findClone(req *pb.SyncupCntlrRequest, cloneId uint64) *pb.Clone {
	for _, clone := range req.GetCloneList() {
		if clone.GetCloneId() == cloneId {
			return clone
		}
	}
	return nil
}

// bitmapInfoList is the CN20 reply field: one BitmapInfo per clone of the
// stored request, its bm_idx_list always derived from the files present
// (SH21), so it survives an agent restart and the worker never re-pushes what
// the node already holds.
func (s *CnAgentServer) bitmapInfoList(st *cntlrState) []*pb.BitmapInfo {
	clones := st.req.GetCloneList()
	if len(clones) == 0 {
		return nil
	}
	out := make([]*pb.BitmapInfo, 0, len(clones))
	for _, clone := range clones {
		info := &pb.BitmapInfo{ResId: clone.GetCloneId()}
		if set := st.chunks[clone.GetCloneId()]; set != nil {
			info.BmIdxList = set.Indexes()
		}
		out = append(out, info)
	}
	return out
}

// cloneChunkPaths lists the local files of the chunks one clone holds.
func (s *CnAgentServer) cloneChunkPaths(
	st *cntlrState,
	plan *cntlrPlan,
	cloneId uint64,
) []string {
	set := st.chunks[cloneId]
	if set == nil || set.Len() == 0 {
		return nil
	}
	paths := make([]string, 0, set.Len())
	for _, bmIdx := range set.Indexes() {
		paths = append(paths, s.nf.LocalCloneBmPath(
			plan.clusterId, plan.cnId, plan.spId, cloneId, bmIdx))
	}
	return paths
}

// dropCloneChunks deletes one clone's chunk files with the rest of its state
// (SH7, CN18 teardown).
func (s *CnAgentServer) dropCloneChunks(
	ctx context.Context,
	st *cntlrState,
	plan *cntlrPlan,
	cloneId uint64,
) {
	paths := s.cloneChunkPaths(st, plan, cloneId)
	if len(paths) > 0 {
		if err := s.store.Remove(ctx, paths...); err != nil {
			slog.ErrorContext(ctx, "removing clone bitmap chunks failed",
				slog.String("error", err.Error()))
		}
	}
	delete(st.chunks, cloneId)
}

// allChunkPaths lists every chunk file of a cntlr — what a full teardown
// removes (CN7, CN21).
func (s *CnAgentServer) allChunkPaths(
	st *cntlrState,
	plan *cntlrPlan,
) []string {
	var paths []string
	for cloneId := range st.chunks {
		paths = append(paths, s.cloneChunkPaths(st, plan, cloneId)...)
	}
	return paths
}
