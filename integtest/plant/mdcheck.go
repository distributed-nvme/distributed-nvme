package plant

import (
	"context"
	"fmt"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// SetMdCheckDays rewrites the cluster's md_check_conf.interval_days in its
// stored ClusterConf, keeping every other member and the creation epoch, so
// the cluster keeps its id. A ClusterConf is write-once through the gateway
// (architecture.md, Clusters); the worker suite rewrites it to turn the md
// check on in one case (dnv-worker.md AR16), which the worker's conf cache
// takes from its watch (RW21).
func SetMdCheckDays(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	days uint32,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		key := model.ClusterConfKey(cl.Name)
		cc := &pb.ClusterConf{}
		if !s.Get(key, cc) {
			return fmt.Errorf("%q not found", key)
		}
		cc.MdCheckConf = &pb.MdCheckConf{IntervalDays: days}
		s.Put(key, cc)
		return nil
	})
}

// SetMdCheck writes the md check schedule of the group grpId through
// model.UpdateMdChecks, from the schedule stored now, as the worker writes
// it (dnv-worker.md AR16), so the pool's revision moves once. It returns
// the pool's revision after it.
func SetMdCheck(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	grpId uint64,
	schedule *pb.MdCheck,
) (uint64, error) {
	var parent model.GrpParent
	var stored *pb.MdCheck
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		conf, err := getSpConf(s, cl.Id, target)
		if err != nil {
			return err
		}
		g, err := findPlantedGrp(s, cl.Id, target, conf, grpId)
		if err != nil {
			return err
		}
		parent, stored = g.parent, g.grp.GetMdCheck()
		return nil
	})
	if err != nil {
		return 0, err
	}
	applied, _, err := model.UpdateMdChecks(ctx, cli, cl.Id, target.Shard,
		target.Id, target.Name, []model.MdCheckEdit{{
			Ref:  model.GrpRef{Parent: parent, GrpId: grpId},
			From: stored, To: schedule,
		}})
	if err != nil {
		return 0, err
	}
	if applied != 1 {
		return 0, fmt.Errorf("the schedule of group %d moved under the write",
			grpId)
	}
	return SpRevision(ctx, cli, cl, target)
}
