package gateway

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md §8.13 / gateway.md §5.12: the two bitmap reads
// a clone or a migration pages through before it copies anything.
//
// Both are read-only two-phase RPCs (AG1): one `Snapshot` resolves everything
// the agent request needs at a single store revision, and the one agent call
// happens strictly outside it (§5.8). Neither writes a key, so neither takes
// a revision token and neither bumps anything (§5.5) — AG4's "re-run
// resolution in the deciding STM" has no second STM to apply to here, because
// there is nothing to decide: the reply is a measurement of the primary's
// live dm-thin metadata, not a change to desired state. A concurrent
// mutation therefore cannot corrupt anything; it can only make the answer
// describe a slightly older world, which is exactly what a bitmap consumer
// re-reads for.
//
// Both go to the PRIMARY cntlr's CN. The primary is the one cntlr that builds
// the §3.3 stack — the per-slice thin pools and the leg devices under them —
// so it is the only node that can walk the pool metadata these two answers
// are computed from. A secondary holds none of it.

// The op names the agent-failure messages cite, so a log line names something
// greppable back to the RPC the operator called.
const (
	opGetThinDeviceBitmap = "GetThinDeviceBitmap"
	opGetLegBitmap        = "GetLegBitmap"
)

// bitmapTarget is everything the phase-2 agent call needs, carried out of the
// resolving Snapshot: the ids that address the object on the CN plus the
// endpoint to dial. It exists so the two handlers resolve "the primary's CN"
// through one piece of code and can never disagree about which node answers.
type bitmapTarget struct {
	Cid      uint64
	SpId     uint64
	CntlrId  uint64
	CnId     uint64
	AddrPort string
}

// openBitmapTarget resolves the SP's primary cntlr and the CN it runs on
// (§8.13, gateway.md §5.12).
//
// An SP with no primary is FAILED_PRECONDITION rather than NOT_FOUND: the SP
// and its cntlrs exist, but no node has been promoted yet (the sp-worker
// elects one, §10.3), so the bitmap is unavailable *for now* — a caller that
// retries after promotion succeeds, which is what a precondition means.
//
// The CnConf is read for its `cn_id` only, exactly as InspectCntlr does
// (gateway.md §5.5): the Cntlr stores the addr_port, and the agent request
// addresses the node by id. Its absence is §5.9's ABORTED and not NOT_FOUND —
// DeleteControllerNode refuses a CN whose `cntlr_ptr_list` is non-empty, so a
// live cntlr pointing at a missing CnConf is a lost invariant key, not a
// user-visible "no such node".
func openBitmapTarget(
	stm etcdutil.STM,
	sc *spScope,
	spName string,
) (bitmapTarget, error) {
	cntlrs, err := loadCntlrs(stm, sc.Cid, sc.Conf)
	if err != nil {
		return bitmapTarget{}, err
	}
	cntlrId, cntlr, ok := primaryCntlr(sc.Conf, cntlrs)
	if !ok {
		return bitmapTarget{}, errPrecondition(
			"storage pool %q has no primary cntlr", spName)
	}
	key := model.CnConfKey(sc.Cid, cntlr.GetAddrPort())
	cn := &pb.CnConf{}
	if !stm.Get(key, cn) {
		return bitmapTarget{}, errAborted("cn_conf key %q is missing", key)
	}
	return bitmapTarget{
		Cid:      sc.Cid,
		SpId:     sc.SpId(),
		CntlrId:  cntlrId,
		CnId:     cn.GetCnId(),
		AddrPort: cntlr.GetAddrPort(),
	}, nil
}

// GetThinDeviceBitmap is architecture.md §8.13's GetThinDeviceBitmap: the
// mapping bitmap of one thin device's volume in one slice, which a clone
// pages through and feeds to AppendCloneBitmap on the destination.
//
// Phase 1 is a Snapshot and not a RunSTM because the RPC writes nothing: one
// store revision is enough to make the td, the primary and the CN it names
// mutually consistent (§5.8, GW8).
//
// `slice_idx` is checked against the SP's own slice count and not against a
// constant: it is state-dependent, so it belongs inside the transaction and
// not in validate.go (GW4). It is checked after the td and the primary are
// resolved, so a caller naming a td that does not exist hears that first.
//
// `block_cnt == 0` means "to the end of the volume" and is passed through
// unchanged: the agent is the only party that knows where the end is.
//
// The reply is the agent's bytes VERBATIM (GW14, [D-J]). The wire convention
// — bit k = 1 iff block start_block+k is unmapped, LSB-first within each byte
// (bit i at `bitmap[i/8] & (1 << (i%8))`), trailing pad bits 0 — is produced
// by the agent, which inverts thin-pool metadata's native "mapped = written"
// once at that boundary (§11.4). The gateway never inspects or rewrites a
// bit, so it can never disagree with the agent about the convention, and
// byte-aligned chunks stay concatenable by the caller.
func (s *Server) GetThinDeviceBitmap(
	ctx context.Context,
	req *pb.GetThinDeviceBitmapRequest,
) (*pb.GetThinDeviceBitmapReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateName("td_name", req.GetTdName()); err != nil {
		return nil, err
	}
	var target bitmapTarget
	var tdId uint64
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		// Reassigned on entry so the closure stays a pure function of what
		// it reads (GW8), whatever a previous attempt left behind.
		target = bitmapTarget{}
		tdId = 0
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		td := &pb.ThinDevice{}
		if !stm.Get(
			model.ThinDeviceKey(sc.Cid, sc.SpId(), req.GetTdName()), td,
		) {
			return errNotFound(
				"thin device %q not found", req.GetTdName())
		}
		found, err := openBitmapTarget(stm, sc, req.GetSpName())
		if err != nil {
			return err
		}
		if int(req.GetSliceIdx()) >= spSliceCnt(sc.Conf) {
			return errInvalid(
				"slice_idx %d is not below the storage pool's slice count %d",
				req.GetSliceIdx(), spSliceCnt(sc.Conf))
		}
		target = found
		tdId = td.GetTdId()
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	var bitmap []byte
	err = withCnAgent(ctx, target.AddrPort,
		func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
			reply, err := client.GetThinDeviceBm(
				ctx,
				&pb.GetThinDeviceBmRequest{
					ClusterId:  target.Cid,
					CnId:       target.CnId,
					SpId:       target.SpId,
					CntlrId:    target.CntlrId,
					TdId:       tdId,
					SliceIdx:   req.GetSliceIdx(),
					StartBlock: req.GetStartBlock(),
					BlockCnt:   req.GetBlockCnt(),
				},
			)
			if err != nil {
				return err
			}
			bitmap = reply.GetBitmap()
			return nil
		})
	if err != nil {
		// AG3: a transport failure or a non-OK status from the agent is this
		// RPC's ABORTED — the gateway has no second source for the answer.
		return nil, errAborted("%s: controller node %q: %v",
			opGetThinDeviceBitmap, target.AddrPort, err)
	}
	return &pb.GetThinDeviceBitmapReply{Bitmap: bitmap}, nil
}

// GetLegBitmap is architecture.md §8.13's GetLegBitmap: the bitmap of one
// leg's data region, which a migration pages through and feeds to
// AppendMigrationBitmap.
//
// It is GetThinDeviceBitmap's shape with the object named differently. The
// leg is located by the bounded slice scan of findLeg — every slice, every
// meta and data group, active legs and spare legs alike — because a leg_id is
// unique inside the SP but carries no hint of which slice holds it (§8.6);
// an unknown id is NOT_FOUND. No slice_idx is taken: the agent derives the
// owning slice from the leg itself, then walks that slice's pool metadata
// down through the group geometry to this leg's data region.
//
// The reply is the agent's bytes VERBATIM (GW14, [D-J]) — same convention as
// GetThinDeviceBitmap, bit k = 1 iff no pool block maps there, LSB-first with
// zero pad bits, produced by the agent and never touched here.
func (s *Server) GetLegBitmap(
	ctx context.Context,
	req *pb.GetLegBitmapRequest,
) (*pb.GetLegBitmapReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	var target bitmapTarget
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		target = bitmapTarget{}
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		slices, err := loadSlices(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		if _, ok := findLeg(sc.Conf, slices, req.GetLegId()); !ok {
			return errNotFound("leg %d not found", req.GetLegId())
		}
		found, err := openBitmapTarget(stm, sc, req.GetSpName())
		if err != nil {
			return err
		}
		target = found
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	var bitmap []byte
	err = withCnAgent(ctx, target.AddrPort,
		func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
			reply, err := client.GetLegBm(
				ctx,
				&pb.GetLegBmRequest{
					ClusterId:  target.Cid,
					CnId:       target.CnId,
					SpId:       target.SpId,
					CntlrId:    target.CntlrId,
					LegId:      req.GetLegId(),
					StartBlock: req.GetStartBlock(),
					BlockCnt:   req.GetBlockCnt(),
				},
			)
			if err != nil {
				return err
			}
			bitmap = reply.GetBitmap()
			return nil
		})
	if err != nil {
		return nil, errAborted("%s: controller node %q: %v",
			opGetLegBitmap, target.AddrPort, err)
	}
	return &pb.GetLegBitmapReply{Bitmap: bitmap}, nil
}
