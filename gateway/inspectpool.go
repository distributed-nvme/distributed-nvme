package gateway

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md, Storage pools, InspectStoragePool / gateway.md,
// Cntlrs and inspects: the read of one pool's space.

// opInspectStoragePool is the op the agent-failure message cites.
const opInspectStoragePool = "InspectStoragePool"

// InspectStoragePool is the InspectStoragePool of architecture.md, Storage
// pools: a pool's space, the records' half and the half only its primary's
// report holds, for an operator to see how full its thin pools are and how
// far its volumes overcommit it.
//
// It is the Inspect shape (InspectCntlr): one Snapshot resolves the pool,
// gates its stored bdev_conf (GW11: the data space is the data groups'
// blocks times the stored block size), reads every slice and every thin
// device the pool lists, and resolves the primary cntlr and the controller
// node it runs on as the bitmap read does (openBitmapTarget: no primary is
// FAILED_PRECONDITION); then, outside the Snapshot, one GetCntlrInfo to that
// node about the primary, with no counters (cnagent.md CN23). An agent that
// fails is ABORTED (AG3). The agent's reply is not judged: one that holds no
// observation of the cntlr, or no row of a slice's thin pool, leaves those
// slices unread and the records' half whole (model.PoolSpaceOf).
func (s *Server) InspectStoragePool(
	ctx context.Context,
	req *pb.InspectStoragePoolRequest,
) (*pb.InspectStoragePoolReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	var (
		target bitmapTarget
		conf   *pb.SpConf
		slices []*pb.Slice
		tds    []*pb.ThinDevice
	)
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		// Reassigned on entry so the closure stays a pure function of what
		// it reads (GW8), whatever a previous attempt left behind.
		target, conf, slices, tds = bitmapTarget{}, nil, nil, nil
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		if err := model.ValidateBdevConf(sc.Conf.GetBdevConf()); err != nil {
			return errAborted("%v", err)
		}
		loaded, err := loadSlices(stm, sc.Cid, sc.Conf)
		if err != nil {
			return err
		}
		listed := make([]*pb.ThinDevice, 0, len(sc.Conf.GetTdNameList()))
		for _, name := range sc.Conf.GetTdNameList() {
			key := model.ThinDeviceKey(sc.Cid, sc.SpId(), name)
			td := &pb.ThinDevice{}
			if !stm.Get(key, td) {
				return errAborted("thin device key %q is missing", key)
			}
			listed = append(listed, td)
		}
		found, err := openBitmapTarget(stm, sc, req.GetSpName())
		if err != nil {
			return err
		}
		target, conf, slices, tds = found, sc.Conf, loaded, listed
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	var reply *pb.GetCntlrInfoReply
	err = withCnAgent(ctx, target.AddrPort,
		func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
			got, err := client.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
				ClusterId: target.Cid,
				CnId:      target.CnId,
				CntlrPointer: &pb.CntlrPointer{
					SpId:    target.SpId,
					CntlrId: target.CntlrId,
				},
			})
			if err != nil {
				return err
			}
			reply = got
			return nil
		})
	if err != nil {
		return nil, errAborted("%s: controller node %q: %v",
			opInspectStoragePool, target.AddrPort, err)
	}
	return &pb.InspectStoragePoolReply{
		PrimaryCntlrId:  target.CntlrId,
		AppliedRevision: reply.GetRevision(),
		ObsStamp:        reply.GetObsStamp(),
		ObsAge:          obsAge(reply.GetObsStamp()),
		Space:           model.PoolSpaceOf(conf, slices, tds, reply.GetCntlrInfo()),
	}, nil
}
