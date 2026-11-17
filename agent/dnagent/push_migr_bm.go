package dnagent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// pushMigrBitmap implements DN15: gate, drop the chunks from the piece's
// index on, persist, then apply. The node read lock and this side's object
// lock are held by the caller.
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
	// it is taken without a removal, a save or an apply, whose pause of the
	// volume (agent.ApplySkipRanges) it would only repeat, and the chunks
	// above it keep their places. The worker pushes every piece again after
	// each read of the bitmap (DN15; dnv-worker.md BM7).
	if held, ok := st.chunks.Get(req.GetBmIdx()); ok &&
		st.chunkMigrId == req.GetMigrId() &&
		bytes.Equal(held, req.GetBitmap()) {
		return &pb.PushMigrBitmapReply{AgentReply: agent.OkReply()}
	}
	// Every stored chunk at the piece's index or above goes first, files
	// and set alike: a chunk above it was placed behind the chunk this
	// piece replaces, which may be of another length, as an older build's
	// chunks are, so it would join this piece at the wrong place; and the
	// chunk at its index must not outlive a save that fails. The worker
	// pushes every piece above it after it (dnv-worker.md BM2, BM7). A
	// removal that fails refuses the push, which ends the worker's plan
	// before any piece above it is saved over a chunk the removal may have
	// left (DN15).
	if err := s.dropChunksFrom(ctx, st, req); err != nil {
		slog.ErrorContext(ctx, "removing bitmap chunks failed",
			slog.Uint64("bm_idx", uint64(req.GetBmIdx())),
			slog.String("error", err.Error()))
		return &pb.PushMigrBitmapReply{AgentReply: &pb.AgentReply{
			Code: common.ReplyCodeSaveFailed,
			Details: fmt.Sprintf("removing the chunks at bm_idx %d "+
				"and above failed: %v", req.GetBmIdx(), err),
		}}
	}
	path := s.nf.LocalMigrBmPath(req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer().GetSpId(), req.GetMigrId(), req.GetBmIdx())
	if err := s.store.Save(ctx, path, req); err != nil {
		// Not persisted ⇒ not applied: the push leaves the applied set
		// without a chunk at its index or above, even when only the
		// directory sync after the rename failed and its file is in place.
		// It is refused, so the worker's plan ends and its next offer, or
		// an accepted SyncupSide reply that comes first, pushes the piece
		// again (dnv-worker.md BM6, BM7): the removal
		// above is durable only with the directory sync of a save that
		// holds, and a crash of the machine before one can bring the
		// removed chunks back, so a code-zero reply would let the worker
		// push a later piece that is saved behind an older chunk at this
		// index (DN15).
		slog.ErrorContext(ctx, "persisting bitmap chunk failed",
			slog.String("path", path),
			slog.String("error", err.Error()))
		return &pb.PushMigrBitmapReply{AgentReply: &pb.AgentReply{
			Code: common.ReplyCodeSaveFailed,
			Details: fmt.Sprintf("saving the chunk at bm_idx %d failed: %v",
				req.GetBmIdx(), err),
		}}
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
// DN2's reload skipped and the one a push left in place when its save
// failed only at the directory sync, since a push whose save fails adds
// nothing to the set, and has taken out of it every chunk at its index or
// above (DN15); and it still lists the chunks whose files a failed removal
// deleted, until a push at or below their indexes takes them out of it or a
// restart reloads the set from the files present (DN15, DN2). So the set
// survives an agent restart, and a chunk left out is pushed again.
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

// dropChunksFrom removes the push's migration's chunk files at its bm_idx
// and every index above it, up to MaxMigrBmCnt, by their paths, so a file
// the startup reload left unloaded goes too; then it takes those indexes
// out of the set (DN15). A removal that fails leaves the set as it was, even
// where rm deleted some of the files before its error; the next push of the
// piece removes them again. The rm syncs no directory: the save that follows
// makes the removal durable.
func (s *DnAgentServer) dropChunksFrom(
	ctx context.Context,
	st *sideState,
	req *pb.PushMigrBitmapRequest,
) error {
	paths := make([]string, 0, common.MaxMigrBmCnt-req.GetBmIdx())
	for idx := req.GetBmIdx(); idx < common.MaxMigrBmCnt; idx++ {
		paths = append(paths, s.nf.LocalMigrBmPath(req.GetClusterId(),
			req.GetDnId(), req.GetSidePointer().GetSpId(), req.GetMigrId(),
			idx))
	}
	if err := s.store.Remove(ctx, paths...); err != nil {
		return err
	}
	for _, idx := range st.chunks.Indexes() {
		if idx >= req.GetBmIdx() {
			st.chunks.Delete(idx)
		}
	}
	return nil
}

func (s *DnAgentServer) dropChunks(ctx context.Context, st *sideState) {
	if err := s.store.Remove(ctx, s.chunkPaths(st)...); err != nil {
		slog.ErrorContext(ctx, "removing stale bitmap chunks failed",
			slog.String("error", err.Error()))
	}
	st.chunks = agent.NewChunkSet()
	st.chunkMigrId = 0
}
