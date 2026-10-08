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
	dst := st.req.Load().GetMigrDstConf()
	if dst == nil || dst.GetMigrId() != req.GetMigrId() {
		return &pb.PushMigrBitmapReply{
			AgentReply: agent.UnknownObjectReply(
				"unknown migration %d on side %s", req.GetMigrId(),
				sidePointerText(req.GetSidePointer())),
		}
	}
	path := s.nf.LocalMigrBmPath(req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer().GetSpId(), req.GetMigrId(), req.GetBmIdx())
	if err := s.store.Save(ctx, path, req); err != nil {
		// Not persisted ⇒ not applied: the chunk stays out of the applied
		// set, so the worker re-pushes it on its next round (architecture.md,
		// Bitmap push protocol).
		slog.ErrorContext(ctx, "persisting bitmap chunk failed",
			slog.String("path", path),
			slog.String("error", err.Error()))
		return &pb.PushMigrBitmapReply{AgentReply: agent.OkReply()}
	}
	st.chunkMigrId = req.GetMigrId()
	st.chunks.Put(req.GetBmIdx(), req.GetBitmap())

	dn := s.getDn(dnKey(req.GetClusterId(), req.GetDnId()))
	if dn != nil {
		s.applyMigrBitmaps(ctx, st, dn.req.Load().GetExtentSize())
	}
	return &pb.PushMigrBitmapReply{AgentReply: agent.OkReply()}
}

// applyMigrBitmaps recomputes the skippable regions from every chunk of the
// applied set and marks them hydrated on the dm-clone. A chunk whose dm-clone
// does not exist yet still counts as applied; it is re-applied when the
// dm-clone is (re)created (dnagent.md DN15).
func (s *DnAgentServer) applyMigrBitmaps(
	ctx context.Context,
	st *sideState,
	extentSize uint64,
) {
	req := st.req.Load()
	if req.GetMigrDstConf() == nil || st.chunks.Len() == 0 {
		return
	}
	s.applyChunks(ctx, st, newSidePlan(s.nf, req, extentSize))
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
	// regions they describe start at the leg's meta_blocks (architecture.md,
	// Migrations).
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

// bitmapInfo reports the applied set: the chunks this process loaded at
// startup or received since. That is every file present (SH21) save those
// DN2's reload skipped, so the set survives an agent restart, and a skipped
// chunk is pushed again.
func (s *DnAgentServer) bitmapInfo(st *sideState) *pb.BitmapInfo {
	dst := st.req.Load().GetMigrDstConf()
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
	req := st.req.Load()
	ptr := req.GetSidePointer()
	paths := make([]string, 0, st.chunks.Len())
	for _, bmIdx := range st.chunks.Indexes() {
		paths = append(paths, s.nf.LocalMigrBmPath(
			req.GetClusterId(), req.GetDnId(), ptr.GetSpId(),
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
