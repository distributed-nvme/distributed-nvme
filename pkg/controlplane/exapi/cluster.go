package exapi

import (
	"context"

	"go.etcd.io/etcd/client/v3/concurrency"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/stmwrapper"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

func (exApi *exApiServer) CreateCluster(
	ctx context.Context,
	req *pbcp.CreateClusterRequest,
) (*pbcp.CreateClusterReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)

	cluster := &pbcp.Cluster{
		DataExtentSizeShift:   constants.DataExtentSizeShiftDefault,
		DataExtentPerSetShift: constants.DataExtentPerSetShiftDefault,
		MetaExtentSizeShift:   constants.MetaExtentSizeShiftDefault,
		MetaExtentPerSetShift: constants.MetaExtentPerSetShiftDefault,
		ExtentRatioShift:      constants.ExtentRatioShiftDefault,
	}
	pch.Logger.Debug("cluster: %v", cluster)
	clusterEntityKey := exApi.kf.ClusterEntityKey()
	clusterEntityVal, err := proto.Marshal(cluster)
	if err != nil {
		pch.Logger.Error("Marshal cluster err: %v %v", cluster, err)
		return &pbcp.CreateClusterReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	clusterEntityValStr := string(clusterEntityVal)

	dnGlobal := &pbcp.DnGlobal{
		GlobalCounter:   0,
		ExtentSetBucket: make([]uint32, cluster.DataExtentPerSetShift),
		ShardBucket:     make([]uint32, constants.ShardCnt),
	}
	pch.Logger.Debug("dnGlobal: %v", dnGlobal)
	dnGlobalEntityKey := exApi.kf.DnGlobalEntityKey()
	dnGlobalEntityVal, err := proto.Marshal(dnGlobal)
	if err != nil {
		pch.Logger.Error("Marshal dnGlobal err: %v %v", dnGlobal, err)
		return &pbcp.CreateClusterReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	dnGlobalEntityValStr := string(dnGlobalEntityVal)

	cnGlobal := &pbcp.CnGlobal{
		GlobalCounter: 0,
		ShardBucket:   make([]uint32, constants.ShardCnt),
	}
	pch.Logger.Debug("cnGlobal: %v", cnGlobal)
	cnGlobalEntityKey := exApi.kf.CnGlobalEntityKey()
	cnGlobalEntityVal, err := proto.Marshal(cnGlobal)
	if err != nil {
		pch.Logger.Error("Marshal cnGlobal err: %v %v", cnGlobal, err)
		return &pbcp.CreateClusterReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	cnGlobalEntityValStr := string(cnGlobalEntityVal)

	spGlobal := &pbcp.SpGlobal{
		GlobalCounter: 0,
		ShardBucket:   make([]uint32, constants.ShardCnt),
	}
	pch.Logger.Debug("spGlobal: %v", spGlobal)
	spGlobalEntityKey := exApi.kf.SpGlobalEntityKey()
	spGlobalEntityVal, err := proto.Marshal(spGlobal)
	if err != nil {
		pch.Logger.Error("Marshal spGlobal err: %v %v", spGlobal, err)
		return &pbcp.CreateClusterReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	spGlobalEntityValStr := string(spGlobalEntityVal)

	apply := func(stm concurrency.STM) error {
		if val := []byte(stm.Get(clusterEntityKey)); len(val) != 0 {
			return &stmwrapper.StmError{
				Code: constants.ReplyCodeDupRes,
				Msg:  clusterEntityKey,
			}
		}
		stm.Put(clusterEntityKey, clusterEntityValStr)

		if val := []byte(stm.Get(dnGlobalEntityKey)); len(val) != 0 {
			return &stmwrapper.StmError{
				Code: constants.ReplyCodeDupRes,
				Msg:  dnGlobalEntityKey,
			}
		}
		stm.Put(dnGlobalEntityKey, dnGlobalEntityValStr)

		if val := []byte(stm.Get(cnGlobalEntityKey)); len(val) != 0 {
			return &stmwrapper.StmError{
				Code: constants.ReplyCodeDupRes,
				Msg:  cnGlobalEntityKey,
			}
		}
		stm.Put(cnGlobalEntityKey, cnGlobalEntityValStr)

		if val := []byte(stm.Get(spGlobalEntityKey)); len(val) != 0 {
			return &stmwrapper.StmError{
				Code: constants.ReplyCodeDupRes,
				Msg:  spGlobalEntityKey,
			}
		}
		stm.Put(spGlobalEntityKey, spGlobalEntityValStr)

		return nil
	}

	err = exApi.sm.RunStm(pch, apply)
	if err != nil {
		if serr, ok := err.(*stmwrapper.StmError); ok {
			return &pbcp.CreateClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.Code,
					ReplyMsg:  serr.Msg,
				},
			}, nil
		} else {
			return &pbcp.CreateClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.CreateClusterReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeSucceed,
			ReplyMsg:  constants.ReplyMsgSucceed,
		},
	}, nil
}

func (exApi *exApiServer) DeleteCluster(
	ctx context.Context,
	req *pbcp.DeleteClusterRequest,
) (*pbcp.DeleteClusterReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)

	clusterEntityKey := exApi.kf.ClusterEntityKey()
	dnGlobalEntityKey := exApi.kf.DnGlobalEntityKey()
	cnGlobalEntityKey := exApi.kf.CnGlobalEntityKey()
	spGlobalEntityKey := exApi.kf.SpGlobalEntityKey()

	apply := func(stm concurrency.STM) error {
		if len(stm.Get(clusterEntityKey)) > 0 {
			pch.Logger.Debug("Delete %s", clusterEntityKey)
			stm.Del(clusterEntityKey)
		}
		if len(stm.Get(dnGlobalEntityKey)) > 0 {
			pch.Logger.Debug("Delete %s", dnGlobalEntityKey)
			stm.Del(dnGlobalEntityKey)
		}
		if len(stm.Get(cnGlobalEntityKey)) > 0 {
			pch.Logger.Debug("Delete %s", cnGlobalEntityKey)
			stm.Del(cnGlobalEntityKey)
		}
		if len(stm.Get(spGlobalEntityKey)) > 0 {
			pch.Logger.Debug("Delete %s", spGlobalEntityKey)
			stm.Del(spGlobalEntityKey)
		}
		return nil
	}

	err := exApi.sm.RunStm(pch, apply)
	if err != nil {
		if serr, ok := err.(*stmwrapper.StmError); ok {
			return &pbcp.DeleteClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.Code,
					ReplyMsg:  serr.Msg,
				},
			}, nil
		} else {
			return &pbcp.DeleteClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeAgentErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.DeleteClusterReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeSucceed,
			ReplyMsg:  constants.ReplyMsgSucceed,
		},
	}, nil
}

func (exApi *exApiServer) GetCluster(
	ctx context.Context,
	req *pbcp.GetClusterRequest,
) (*pbcp.GetClusterReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)

	clusterEntityKey := exApi.kf.ClusterEntityKey()
	cluster := &pbcp.Cluster{}

	apply := func(stm concurrency.STM) error {
		val := []byte(stm.Get(clusterEntityKey))
		if len(val) == 0 {
			return &stmwrapper.StmError{
				constants.ReplyCodeUnknownRes,
				clusterEntityKey,
			}
		}
		err := proto.Unmarshal(val, cluster)
		pch.Logger.Debug("cluster: %v", cluster)
		return err
	}

	err := exApi.sm.RunStm(pch, apply)
	if err != nil {
		if serr, ok := err.(*stmwrapper.StmError); ok {
			return &pbcp.GetClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.Code,
					ReplyMsg:  serr.Msg,
				},
			}, nil
		} else {
			return &pbcp.GetClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.GetClusterReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeSucceed,
			ReplyMsg:  constants.ReplyMsgSucceed,
		},
		Cluster: &pbcp.Cluster{
			DataExtentSizeShift:   cluster.DataExtentSizeShift,
			DataExtentPerSetShift: cluster.DataExtentPerSetShift,
			MetaExtentSizeShift:   cluster.MetaExtentSizeShift,
			MetaExtentPerSetShift: cluster.MetaExtentPerSetShift,
			ExtentRatioShift:      cluster.ExtentRatioShift,
			QosUnit: &pbcp.QosFields{
				Rbps:  cluster.QosUnit.Rbps,
				Wbps:  cluster.QosUnit.Wbps,
				Riops: cluster.QosUnit.Riops,
				Wiops: cluster.QosUnit.Wiops,
			},
		},
	}, nil
}
