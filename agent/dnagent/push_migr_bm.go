package dnagent

import (
	"bytes"
	"context"
	"log/slog"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
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
	// The chunk index is bounded as the worker bounds its pieces (DN15): an
	// index at or past MaxMigrBmCnt names no piece the worker can cut, and
	// a file stored for it would stay in the applied set.
	if req.GetBmIdx() >= common.MaxMigrBmCnt {
		return &pb.PushMigrBitmapReply{
			AgentReply: agent.UnknownObjectReply(
				"chunk bm_idx %d out of range for migration %d on side %s "+
					"(max_migr_bm_cnt %d)",
				req.GetBmIdx(), req.GetMigrId(),
				sidePointerText(req.GetSidePointer()), common.MaxMigrBmCnt),
		}
	}
	// A piece the applied set already holds, byte for byte, changes nothing:
	// it is taken without a save and without an apply, whose pause of the
	// volume (agent.ApplySkipRanges) it would only repeat. A worker that
	// does not know the applied set, such as one restarted while the agent
	// stayed at its revision, pushes every piece again (DN15; dnv-worker.md
	// BM7).
	if held, ok := st.chunks.Get(req.GetBmIdx()); ok &&
		st.chunkMigrId == req.GetMigrId() &&
		bytes.Equal(held, req.GetBitmap()) {
		return &pb.PushMigrBitmapReply{AgentReply: agent.OkReply()}
	}
	path := s.nf.LocalMigrBmPath(req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer().GetSpId(), req.GetMigrId(), req.GetBmIdx())
	if err := s.store.Save(ctx, path, req); err != nil {
		// Not persisted ⇒ not applied: the push leaves the applied set as it
		// was, so a chunk not yet in it stays out, even when only the
		// directory sync after the rename failed and its file is in place,
		// and the worker pushes it again after a later accepted SyncupSide
		// reply, whose bm_info leaves it out (DN15; dnv-worker.md BM2;
		// architecture.md, Bitmap push protocol).
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
// does not exist yet, or is suspended, still counts as applied; it is
// re-applied when the dm-clone is created, resumed or reloaded (dnagent.md
// DN15).
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
	// Chunks concatenate over the leg's *data* region, so the dm-clone
	// regions they describe start at the leg's meta_blocks (architecture.md,
	// Migrations).
	ranges := agent.SkipRanges(
		st.chunks.ContiguousPrefix(),
		plan.migrDst.GetMetaBlocks(),
		plan.sectors*agent.SectorSize/regionSize,
		regionSize,
	)
	// The apply pauses the per-CN dm-linears that map the dm-clone and
	// sends nothing to a dm-clone that is absent or suspended
	// (agent.ApplySkipRanges; DN15).
	if err := agent.ApplySkipRanges(
		ctx, s.dm, name, s.nf.DmPath(name), ranges); err != nil {
		slog.ErrorContext(ctx, "applying migration bitmap failed",
			slog.String("dm", name),
			slog.String("error", err.Error()))
	}
}

// bitmapInfo reports the applied set: the chunks this process loaded at
// startup or received since. That is every file present (SH21) save those
// DN2's reload skipped and those a push left in place for a chunk outside
// the set when its save failed only at the directory sync, since a push
// whose save fails changes nothing in the set (DN15). So the set survives
// an agent restart, and a chunk left out is pushed again.
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
