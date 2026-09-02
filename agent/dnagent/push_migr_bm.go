package dnagent

import (
	"context"
	"log/slog"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// pushMigrBitmap implements DN15: gate, persist, then apply. The node read
// lock and this side's object lock are held by the caller.
func (s *DnAgentServer) pushMigrBitmap(
	ctx context.Context,
	key string,
	req *pb.PushMigrBitmapRequest,
) *pb.PushMigrBitmapReply {
	st := s.getSide(key)
	if st == nil {
		return &pb.PushMigrBitmapReply{
			AgentReply: agent.UnknownObjectReply(
				"unknown side %s", sidePointerText(req.GetSidePointer())),
		}
	}
	// Only a destination side accepts chunks, and only for its own
	// migration.
	dst := st.req.GetMigrDstConf()
	if dst == nil || dst.GetMigrId() != req.GetMigrId() {
		return &pb.PushMigrBitmapReply{
			AgentReply: agent.UnknownObjectReply(
				"unknown migration %d on side %s", req.GetMigrId(),
				sidePointerText(req.GetSidePointer())),
		}
	}
	// A push never advances the stored revision; it only may not be older.
	if reject := agent.GateRevision(
		st.req.GetRevision(), req.GetRevision()); reject != nil {
		return &pb.PushMigrBitmapReply{AgentReply: reject}
	}

	path := s.nf.LocalMigrBmPath(req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer().GetSpId(), req.GetMigrId(), req.GetBmIdx())
	if err := s.store.Save(ctx, path, req); err != nil {
		// Not persisted ⇒ not applied: the chunk stays out of the applied
		// set, so the worker re-pushes it on its next round (§9.6).
		slog.ErrorContext(ctx, "persisting bitmap chunk failed",
			slog.String("path", path),
			slog.String("error", err.Error()))
		return &pb.PushMigrBitmapReply{AgentReply: agent.OkReply()}
	}
	st.chunkMigrId = req.GetMigrId()
	st.chunks.Put(req.GetBmIdx(), req.GetBitmap())

	dn := s.getDn(dnKey(req.GetClusterId(), req.GetDnId()))
	if dn != nil {
		s.applyMigrBitmaps(ctx, st, dn.req.GetExtentSize())
	}
	return &pb.PushMigrBitmapReply{AgentReply: agent.OkReply()}
}

// applyMigrBitmaps recomputes the skippable regions from every chunk present
// and marks them hydrated on the dm-clone. A chunk whose dm-clone does not
// exist yet still counts as applied; it is re-applied when the dm-clone is
// (re)created (§9.6 step 2/4).
func (s *DnAgentServer) applyMigrBitmaps(
	ctx context.Context,
	st *sideState,
	extentSize uint64,
) {
	if st.req.GetMigrDstConf() == nil || st.chunks.Len() == 0 {
		return
	}
	s.applyChunks(ctx, st, newSidePlan(s.nf, st.req, extentSize))
}

func (s *DnAgentServer) applyChunks(
	ctx context.Context,
	st *sideState,
	plan *sidePlan,
) {
	if plan.migrDst == nil || st.chunks.Len() == 0 {
		return
	}
	regionSize := plan.migrDst.GetBlockSize()
	if regionSize == 0 || plan.sectors == 0 {
		return
	}
	name := plan.migrFinalName()
	dev, err := s.dm.Info(ctx, name)
	if err != nil || dev == nil {
		return
	}
	// Chunks concatenate over the leg's *data* region, so the dm-clone
	// regions they describe start at the leg's meta_blocks (§8.11).
	ranges := agent.SkipRanges(
		st.chunks.ContiguousPrefix(),
		plan.migrDst.GetMetaBlocks(),
		plan.sectors*agent.SectorSize/regionSize,
		regionSize,
	)
	if err := agent.ApplySkipRanges(
		ctx, s.dm, s.nf.DmPath(name), ranges); err != nil {
		slog.ErrorContext(ctx, "applying migration bitmap failed",
			slog.String("dm", name),
			slog.String("error", err.Error()))
	}
}

// bitmapInfo reports the applied set, always derived from the files present
// (SH21), so it survives an agent restart.
func (s *DnAgentServer) bitmapInfo(st *sideState) *pb.BitmapInfo {
	dst := st.req.GetMigrDstConf()
	if dst == nil {
		return nil
	}
	return &pb.BitmapInfo{
		ResId:     dst.GetMigrId(),
		BmIdxList: st.chunks.Indexes(),
	}
}

// chunkPaths lists the local files of the chunks this side currently holds.
func (s *DnAgentServer) chunkPaths(st *sideState) []string {
	if st.chunks.Len() == 0 {
		return nil
	}
	ptr := st.req.GetSidePointer()
	paths := make([]string, 0, st.chunks.Len())
	for _, bmIdx := range st.chunks.Indexes() {
		paths = append(paths, s.nf.LocalMigrBmPath(
			st.req.GetClusterId(), st.req.GetDnId(), ptr.GetSpId(),
			st.chunkMigrId, bmIdx))
	}
	return paths
}

func (s *DnAgentServer) dropChunks(ctx context.Context, st *sideState) {
	if err := s.store.Remove(ctx, s.chunkPaths(st)...); err != nil {
		slog.ErrorContext(ctx, "removing stale bitmap chunks failed",
			slog.String("error", err.Error()))
	}
	st.chunks = agent.NewChunkSet()
	st.chunkMigrId = 0
}
