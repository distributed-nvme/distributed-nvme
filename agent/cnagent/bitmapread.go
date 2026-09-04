package cnagent

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// GetThinDeviceBm / GetLegBm serve the §8.13 gateway reads from a dm-thin
// metadata snapshot (CN25-CN27). They carry no AgentReply, so every failure —
// unknown cntlr, non-primary role, a missing pool, a failing command — ends
// the RPC with an Internal status naming the step (§9.1). A pool whose
// metadata outgrows what thin_dump emits inside CmdSoftTimeout fails the RPC
// and the caller falls back to a full copy: bitmaps are an optimization, never
// a correctness input (§8.9).

// primaryPlanFor resolves the request's cntlr and returns its plan. Both RPCs
// need the same three checks, and both must run under the CN1 locks — the
// snapshot must not race a converge.
func (s *CnAgentServer) primaryPlanFor(
	clusterId, cnId, spId, cntlrId uint64,
) (*cntlrPlan, error) {
	key := cntlrKey(clusterId, cnId, spId, cntlrId)
	st := s.getCntlr(key)
	if st == nil {
		return nil, status.Errorf(codes.Internal,
			"unknown cntlr (sp %d, cntlr %d)", spId, cntlrId)
	}
	plan := newCntlrPlan(s.nf, st.req)
	if !plan.primary {
		return nil, status.Errorf(codes.Internal,
			"cntlr (sp %d, cntlr %d) is not primary", spId, cntlrId)
	}
	return plan, nil
}

func (s *CnAgentServer) GetThinDeviceBm(
	ctx context.Context,
	req *pb.GetThinDeviceBmRequest,
) (*pb.GetThinDeviceBmReply, error) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	key := cntlrKey(req.GetClusterId(), req.GetCnId(),
		req.GetSpId(), req.GetCntlrId())
	if s.getCntlr(key) == nil {
		return nil, status.Errorf(codes.Internal,
			"unknown cntlr (sp %d, cntlr %d)",
			req.GetSpId(), req.GetCntlrId())
	}
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()

	plan, err := s.primaryPlanFor(req.GetClusterId(), req.GetCnId(),
		req.GetSpId(), req.GetCntlrId())
	if err != nil {
		return nil, err
	}
	tp := plan.tdById[req.GetTdId()]
	if tp == nil {
		return nil, status.Errorf(codes.Internal,
			"unknown thin device %d", req.GetTdId())
	}
	var sp *slicePlan
	for _, candidate := range plan.slices {
		if candidate.sliceIdx == req.GetSliceIdx() {
			sp = candidate
			break
		}
	}
	if sp == nil {
		return nil, status.Errorf(codes.Internal,
			"unknown slice_idx %d", req.GetSliceIdx())
	}
	if plan.sliceCnt == 0 || plan.blockSize == 0 {
		return nil, status.Error(codes.Internal, "slice geometry is 0")
	}
	virtualBlocks := tp.td.GetSize() / plan.sliceCnt / plan.blockSize
	start, count, err := bitmapWindow(
		req.GetStartBlock(), req.GetBlockCnt(), virtualBlocks)
	if err != nil {
		return nil, err
	}

	sb, err := s.dumpThinMetadata(ctx, sp)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"reading the thin metadata snapshot: %v", err)
	}
	extents, err := deviceMappings(sb, tp.td.GetDevId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &pb.GetThinDeviceBmReply{
		Bitmap: thinDeviceBitmap(extents, start, count),
	}, nil
}

func (s *CnAgentServer) GetLegBm(
	ctx context.Context,
	req *pb.GetLegBmRequest,
) (*pb.GetLegBmReply, error) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	key := cntlrKey(req.GetClusterId(), req.GetCnId(),
		req.GetSpId(), req.GetCntlrId())
	if s.getCntlr(key) == nil {
		return nil, status.Errorf(codes.Internal,
			"unknown cntlr (sp %d, cntlr %d)",
			req.GetSpId(), req.GetCntlrId())
	}
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()

	plan, err := s.primaryPlanFor(req.GetClusterId(), req.GetCnId(),
		req.GetSpId(), req.GetCntlrId())
	if err != nil {
		return nil, err
	}
	lp := plan.legById[req.GetLegId()]
	if lp == nil {
		return nil, status.Errorf(codes.Internal,
			"unknown leg %d", req.GetLegId())
	}
	gp := lp.grp
	if lp.provisioning || !gp.effective() {
		// U4: a group that is not in the *effective* list is not in the
		// pool-data concat at all, so it has no span to report — and that is
		// not only the deferred group itself but every group after it in the
		// list, because deferral is a prefix cut (effectiveGrps). Saying so
		// plainly beats the data_grp_list error dataGrpSpanStart would raise
		// below — its effective walk cannot find a group that is not a target
		// yet.
		return nil, status.Errorf(codes.Internal,
			"leg %d is still provisioning", req.GetLegId())
	}
	start, count, err := bitmapWindow(
		req.GetStartBlock(), req.GetBlockCnt(), gp.dataBlocks)
	if err != nil {
		return nil, err
	}
	if gp.isMeta {
		// A meta-group leg replies all-zero: the pool metadata device's own
		// utilization is not derivable from thin mappings, and over-copying
		// is always safe (CN27).
		return &pb.GetLegBmReply{
			Bitmap: make([]byte, bitmapBytes(count)),
		}, nil
	}
	spanStart, ok := gp.slice.dataGrpSpanStart(gp.grpId)
	if !ok {
		return nil, status.Errorf(codes.Internal,
			"group %d is not in its slice's data_grp_list", gp.grpId)
	}
	sb, err := s.dumpThinMetadata(ctx, gp.slice)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"reading the thin metadata snapshot: %v", err)
	}
	return &pb.GetLegBmReply{
		Bitmap: legDataBitmap(sb, spanStart, gp.dataBlocks, start, count),
	}, nil
}

// bitmapWindow resolves the paged read of §8.13: block_cnt = 0 means "to the
// end", and a window past the end is a request error rather than a silently
// truncated bitmap.
func bitmapWindow(
	startBlock uint64,
	blockCnt uint64,
	total uint64,
) (uint64, uint64, error) {
	if startBlock > total {
		return 0, 0, status.Errorf(codes.Internal,
			"start_block %d is past the end (%d blocks)", startBlock, total)
	}
	if blockCnt == 0 {
		return startBlock, total - startBlock, nil
	}
	if startBlock+blockCnt > total {
		return 0, 0, status.Errorf(codes.Internal,
			"window [%d,%d) is past the end (%d blocks)",
			startBlock, startBlock+blockCnt, total)
	}
	return startBlock, blockCnt, nil
}
