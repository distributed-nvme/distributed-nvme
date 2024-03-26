package controlplane

import (
	"context"

	"google.golang.org/protobuf/proto"
	"go.etcd.io/etcd/client/v3/concurrency"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbds "github.com/distributed-nvme/distributed-nvme/pkg/proto/dataschema"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplaneapi"
)

func (cpas *cpApiServer) CreateCluster(
	ctx context.Context,
	req *pbcp.CreateClusterRequest,
) (*pbcp.CreateClusterReply, error) {
	pch := newPerCtxHelper(ctx, cpas)
	defer pch.close()

	cluster := &pbds.Cluster{
		DataExtentSizeShift: lib.DataExtentSizeShiftDefault,
		DataExtentPerSetShift: lib.DataExtentPerSetShiftDefault,
		MetaExtentSizeShift: lib.MetaExtentSizeShiftDefault,
		MetaExtentPerSetShift: lib.MetaExtentPerSetShiftDefault,
		ExtentRatioShift: lib.ExtentRatioShiftDefault,
	}
	clusterEntityKey := cpas.kf.ClusterEntityKey()
	clusterEntityVal, err := proto.Marshal(cluster)
	if err != nil {
		pch.logger.Error("Marshal cluster err: %v %v", cluster, err)
		return &pbcp.CreateClusterReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReqId:     lib.GetReqId(ctx),
				ReplyCode: lib.CpApiInternalErrCode,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	clusterEntityValStr := string(clusterEntityVal)

	dnGlobal := &pbds.DnGlobal{
		GlobalCounter: 0,
		ExtentSetBucket: make([]uint32, cluster.DataExtentPerSetShift),
		ShardBucket: make([]uint32, lib.ShardSize),
	}
	dnGlobalEntityKey := cpas.kf.DnGlobalEntityKey()
	dnGlobalEntityVal, err := proto.Marshal(dnGlobal)
	if err != nil {
		pch.logger.Error("Marshal dnGlobal err: %v %v", dnGlobal, err)
		return &pbcp.CreateClusterReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReqId:     lib.GetReqId(ctx),
				ReplyCode: lib.CpApiInternalErrCode,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	dnGlobalEntityValStr := string(dnGlobalEntityVal)

	cnGlobal := &pbds.CnGlobal{
		GlobalCounter: 0,
		ShardBucket: make([]uint32, lib.ShardSize),
	}
	cnGlobalEntityKey := cpas.kf.CnGlobalEntityKey()
	cnGlobalEntityVal, err := proto.Marshal(cnGlobal)
	if err != nil {
		pch.logger.Error("Marshal cnGlobal err: %v %v", cnGlobal, err)
		return &pbcp.CreateClusterReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReqId:     lib.GetReqId(ctx),
				ReplyCode: lib.CpApiInternalErrCode,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	cnGlobalEntityValStr := string(cnGlobalEntityVal)

	spGlobal := &pbds.SpGlobal{
		GlobalCounter: 0,
		ShardBucket: make([]uint32, lib.ShardSize),
	}
	spGlobalEntityKey := cpas.kf.SpGlobalEntityKey()
	spGlobalEntityVal, err := proto.Marshal(spGlobal)
	if err != nil {
		pch.logger.Error("Marshal spGlobal err: %v %v", spGlobal, err)
		return &pbcp.CreateClusterReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReqId:     lib.GetReqId(ctx),
				ReplyCode: lib.CpApiInternalErrCode,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	spGlobalEntityValStr := string(spGlobalEntityVal)

	apply := func(stm concurrency.STM) error {
		if val := []byte(stm.Get(clusterEntityKey)); len(val) != 0 {
			return &cpStmError{
				code: lib.CpApiDupResErrCode,
				msg:  clusterEntityKey,
			}
		}
		stm.Put(clusterEntityKey, clusterEntityValStr)

		if val := []byte(stm.Get(dnGlobalEntityKey)); len(val) != 0 {
			return &cpStmError{
				code: lib.CpApiDupResErrCode,
				msg:  dnGlobalEntityKey,
			}
		}
		stm.Put(dnGlobalEntityKey, dnGlobalEntityValStr)

		if val := []byte(stm.Get(cnGlobalEntityKey)); len(val) != 0 {
			return &cpStmError{
				code: lib.CpApiDupResErrCode,
				msg:  cnGlobalEntityKey,
			}
		}
		stm.Put(cnGlobalEntityKey, cnGlobalEntityValStr)

		if val := []byte(stm.Get(spGlobalEntityKey)); len(val) != 0 {
			return &cpStmError{
				code: lib.CpApiDupResErrCode,
				msg:  spGlobalEntityKey,
			}
		}
		stm.Put(spGlobalEntityKey, spGlobalEntityValStr)

		return nil
	}

	err = pch.runStm(apply, "CreateCluster")
	if err != nil {
		if serr, ok := err.(*cpStmError); ok {
			return &pbcp.CreateClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReqId:     lib.GetReqId(ctx),
					ReplyCode: serr.code,
					ReplyMsg:  serr.msg,
				},
			}, nil
		} else {
			return &pbcp.CreateClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReqId:     lib.GetReqId(ctx),
					ReplyCode: lib.CpApiInternalErrCode,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.CreateClusterReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReqId:     lib.GetReqId(ctx),
			ReplyCode: lib.CpApiSucceedCode,
			ReplyMsg:  lib.CpApiSucceedMsg,
		},
	}, nil
}

func (cpas *cpApiServer) DeleteCluster(
	ctx context.Context,
	req *pbcp.DeleteClusterRequest,
) (*pbcp.DeleteClusterReply, error) {
	pch := newPerCtxHelper(ctx, cpas)
	defer pch.close()

	clusterEntityKey := cpas.kf.ClusterEntityKey()
	dnGlobalEntityKey := cpas.kf.DnGlobalEntityKey()
	cnGlobalEntityKey := cpas.kf.CnGlobalEntityKey()
	spGlobalEntityKey := cpas.kf.SpGlobalEntityKey()

	apply := func(stm concurrency.STM) error {
		if len(stm.Get(clusterEntityKey)) > 0 {
			stm.Del(clusterEntityKey)
		}
		if len(stm.Get(dnGlobalEntityKey)) > 0 {
			stm.Del(dnGlobalEntityKey)
		}
		if len(stm.Get(cnGlobalEntityKey)) > 0 {
			stm.Del(cnGlobalEntityKey)
		}
		if len(stm.Get(spGlobalEntityKey)) > 0 {
			stm.Del(spGlobalEntityKey)
		}
		return nil
	}

	err := pch.runStm(apply, "DeleteCluster")
	if err != nil {
		if serr, ok := err.(*cpStmError); ok {
			return &pbcp.DeleteClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReqId:     lib.GetReqId(ctx),
					ReplyCode: serr.code,
					ReplyMsg:  serr.msg,
				},
			}, nil
		} else {
			return &pbcp.DeleteClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReqId:     lib.GetReqId(ctx),
					ReplyCode: lib.CpApiInternalErrCode,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.DeleteClusterReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReqId:     lib.GetReqId(ctx),
			ReplyCode: lib.CpApiSucceedCode,
			ReplyMsg:  lib.CpApiSucceedMsg,
		},
	}, nil
}

func (cpas *cpApiServer) GetCluster(
	ctx context.Context,
	req *pbcp.GetClusterRequest,
) (*pbcp.GetClusterReply, error) {
	pch := newPerCtxHelper(ctx, cpas)
	defer pch.close()

	clusterEntityKey := cpas.kf.ClusterEntityKey()
	cluster := &pbds.Cluster{}

	apply := func(stm concurrency.STM) error {
		val := []byte(stm.Get(clusterEntityKey))
		if len(val) == 0 {
			return &cpStmError{
				lib.CpApiUnknownResErrCode,
				clusterEntityKey,
			}
		}
		err := proto.Unmarshal(val, cluster)
		return err
	}

	err := pch.runStm(apply, "GetCluster")
	if err != nil {
		if serr, ok := err.(*cpStmError); ok {
			return &pbcp.GetClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReqId:     lib.GetReqId(ctx),
					ReplyCode: serr.code,
					ReplyMsg:  serr.msg,
				},
			}, nil
		} else {
			return &pbcp.GetClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReqId:     lib.GetReqId(ctx),
					ReplyCode: lib.CpApiInternalErrCode,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	pch.logger.Info("cluster: %v", cluster)

	return &pbcp.GetClusterReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReqId:     lib.GetReqId(ctx),
			ReplyCode: lib.CpApiSucceedCode,
			ReplyMsg:  lib.CpApiSucceedMsg,
		},
		Cluster: &pbcp.Cluster{
			DataExtentSizeShift: cluster.DataExtentSizeShift,
			DataExtentPerSetShift: cluster.DataExtentPerSetShift,
			MetaExtentSizeShift: cluster.MetaExtentSizeShift,
			MetaExtentPerSetShift: cluster.MetaExtentPerSetShift,
			ExtentRatioShift: cluster.ExtentRatioShift,
			QosUnit: &pbcp.QosFields{
				Rbps: cluster.QosUnit.Rbps,
				Wbps: cluster.QosUnit.Wbps,
				Riops: cluster.QosUnit.Riops,
				Wiops: cluster.QosUnit.Wiops,
			},
		},
	}, nil
}
