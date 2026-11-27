package cnagent

import (
	"context"
	"sort"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// GetThinDeviceBm / GetLegBm serve the bitmap reads of architecture.md,
// Bitmap reads — the gateway's of a thin device, the worker's of a migrating
// leg (dnv-worker.md BM7) — from a dm-thin metadata snapshot (CN25-CN27). They carry no AgentReply, so every failure —
// unknown cntlr, non-primary role, a missing pool, a failing command — ends
// the RPC with an Internal status naming the step (architecture.md, Common
// agent rules), but CN26's refusals, an unsure answer among them, which are
// FailedPrecondition. A pool whose
// metadata outgrows what thin_dump emits inside CmdSoftTimeout fails the RPC
// and the caller falls back to a full copy: a bitmap read when
// architecture.md, Clones or Migration, says is an optimization, never a
// correctness input. The worker gives a leg's bitmap up after a few refused
// reads (dnv-worker.md BM7).

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
	plan := newCntlrPlan(s.nf, st.loadReq())
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
	if err := s.startingErr(); err != nil {
		return nil, err
	}
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

	// CN26: no read while a namespace device of the node still maps the
	// volume, as its namespace's IO still reaches it.
	if err := s.volumeServed(ctx, plan, tp); err != nil {
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

// volumeServed is CN26's refusal of a volume's bitmap read while a
// namespace device of the cntlr's pool on this node maps the volume: a
// device whose name parses as an ns-dev of the pool (any namespace id) and
// whose live table names, as an argument of any target, the volume's raid0
// or a device whose own table names the raid0, as a clone's dm-clone does
// with its destination. Such a device passes its namespace's IO to the
// volume until the converge takes it away, a deleted namespace's or one
// pointed at another volume among them, which no request names any more; a
// parked one maps the volume's dm-error and is no refusal. One at the
// read-only level takes no write and is refused all the same, as the
// gateway asks for every namespace parked. The tables are listed first and
// the devices second, so a device of the table listing is in the device
// listing unless it went in between; a listing that may lack a device, a
// raid0 or a device over it with no number, and a raid0 in the tables and
// gone from the devices are unsure answers and refuse the read, never
// reading as "nothing serves". A volume with no raid0 on the node has
// nothing over it. Every refusal is FailedPrecondition; a listing that
// fails is Internal, naming the step.
func (s *CnAgentServer) volumeServed(
	ctx context.Context,
	plan *cntlrPlan,
	tp *tdPlan,
) error {
	refuse := func(format string, args ...any) error {
		return status.Errorf(codes.FailedPrecondition,
			"volume %d: "+format, append([]any{tp.tdId}, args...)...)
	}
	tables, err := s.dm.ListTables(ctx)
	if err != nil {
		return status.Errorf(codes.Internal,
			"listing the node's dm tables (CN26): %v", err)
	}
	devs, err := s.dm.ListInfo(ctx)
	if err != nil {
		return status.Errorf(codes.Internal,
			"listing the node's dm devices (CN26): %v", err)
	}
	if tables.Partial || devs.Partial {
		return refuse("a device listing may lack a device; read again")
	}
	raid0 := devs.Devs[tp.raid0Name]
	if raid0 == nil {
		if _, listed := tables.Tables[tp.raid0Name]; listed {
			return refuse("raid0 %s went during the listings; read again",
				tp.raid0Name)
		}
		return nil
	}
	if raid0.DevNo == "" {
		return refuse("raid0 %s has no device number; read again",
			tp.raid0Name)
	}
	maps := func(targets []common.DmTarget, devNos map[string]bool) bool {
		for _, target := range targets {
			for _, arg := range target.Args {
				if devNos[arg] {
					return true
				}
			}
		}
		return false
	}
	raid0Only := map[string]bool{raid0.DevNo: true}
	over := map[string]bool{raid0.DevNo: true}
	names := make([]string, 0, len(tables.Tables))
	for name := range tables.Tables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !maps(tables.Tables[name], raid0Only) {
			continue
		}
		info := devs.Devs[name]
		if info == nil || info.DevNo == "" {
			return refuse("device %s maps raid0 %s and has no device "+
				"number; read again", name, tp.raid0Name)
		}
		over[info.DevNo] = true
	}
	for _, name := range names {
		dn, ok := common.ParseDmName(name)
		if !ok || dn.Kind != common.DmKindCnNsDev ||
			dn.ClusterId != plan.clusterId || dn.NodeId != plan.cnId ||
			len(dn.Ids) == 0 || dn.Ids[0] != plan.spId {
			continue
		}
		if maps(tables.Tables[name], over) {
			return refuse("namespace device %s still maps raid0 %s, so "+
				"its namespace's IO still reaches the volume; read again "+
				"once the primary's converge has taken it away or parked it",
				name, tp.raid0Name)
		}
	}
	return nil
}

func (s *CnAgentServer) GetLegBm(
	ctx context.Context,
	req *pb.GetLegBmRequest,
) (*pb.GetLegBmReply, error) {
	if err := s.startingErr(); err != nil {
		return nil, err
	}
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
	if gp.clone != nil {
		// CN27: a leg of a clone meta group backs no pool, so no thin
		// mapping says which of its blocks are written. Its group has no
		// slice for the span arithmetic below to read either.
		return nil, status.Errorf(codes.Internal,
			"leg %d is a leg of clone %d's clone meta group",
			req.GetLegId(), gp.clone.GetCloneId())
	}
	if lp.provisioning || !gp.effective() {
		// [D15]: a group that is not in the *effective* list is not in the
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
	// CN27: a write whose block dm-thin has not recorded yet may already sit
	// on the leg; the drain ends it, and with it the record, before the
	// snapshot.
	if err := s.drainThinDevices(ctx, gp.slice); err != nil {
		return nil, status.Errorf(codes.Internal,
			"draining the thin devices: %v", err)
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

// bitmapWindow resolves the paged read of architecture.md, Bitmap reads:
// block_cnt = 0 means "to the
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
	// blockCnt is compared with what is left, never summed with
	// startBlock: a sum past 2^64 wraps, and a count that wrapped would size
	// a bitmap of the request's own choosing.
	if blockCnt > total-startBlock {
		return 0, 0, status.Errorf(codes.Internal,
			"window of %d blocks from %d is past the end (%d blocks)",
			blockCnt, startBlock, total)
	}
	return startBlock, blockCnt, nil
}
