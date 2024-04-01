package controlplane

import (
	"context"

	"google.golang.org/protobuf/proto"
	"go.etcd.io/etcd/client/v3/concurrency"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
)

func (exApi *exApiServer) CreateCluster(
	ctx context.Context,
	req *pbcp.CreateClusterRequest,
) (*pbcp.CreateClusterReply, error) {
	pch := lib.GetPerCtxHelper(ctx)

	cluster := &pbcp.Cluster{
		DataExtentSizeShift: lib.DataExtentSizeShiftDefault,
		DataExtentPerSetShift: lib.DataExtentPerSetShiftDefault,
		MetaExtentSizeShift: lib.MetaExtentSizeShiftDefault,
		MetaExtentPerSetShift: lib.MetaExtentPerSetShiftDefault,
		ExtentRatioShift: lib.ExtentRatioShiftDefault,
	}
	pch.Logger.Debug("cluster: %v", cluster)
	clusterEntityKey := exApi.kf.clusterEntityKey()
	clusterEntityVal, err := proto.Marshal(cluster)
	if err != nil {
		pch.Logger.Error("Marshal cluster err: %v %v", cluster, err)
		return &pbcp.CreateClusterReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: lib.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	clusterEntityValStr := string(clusterEntityVal)

	dnGlobal := &pbcp.DnGlobal{
		GlobalCounter: 0,
		ExtentSetBucket: make([]uint32, cluster.DataExtentPerSetShift),
		ShardBucket: make([]uint32, lib.ShardCnt),
	}
	pch.Logger.Debug("dnGlobal: %v", dnGlobal)
	dnGlobalEntityKey := exApi.kf.dnGlobalEntityKey()
	dnGlobalEntityVal, err := proto.Marshal(dnGlobal)
	if err != nil {
		pch.Logger.Error("Marshal dnGlobal err: %v %v", dnGlobal, err)
		return &pbcp.CreateClusterReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: lib.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	dnGlobalEntityValStr := string(dnGlobalEntityVal)

	cnGlobal := &pbcp.CnGlobal{
		GlobalCounter: 0,
		ShardBucket: make([]uint32, lib.ShardCnt),
	}
	pch.Logger.Debug("cnGlobal: %v", cnGlobal)
	cnGlobalEntityKey := exApi.kf.cnGlobalEntityKey()
	cnGlobalEntityVal, err := proto.Marshal(cnGlobal)
	if err != nil {
		pch.Logger.Error("Marshal cnGlobal err: %v %v", cnGlobal, err)
		return &pbcp.CreateClusterReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: lib.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	cnGlobalEntityValStr := string(cnGlobalEntityVal)

	spGlobal := &pbcp.SpGlobal{
		GlobalCounter: 0,
		ShardBucket: make([]uint32, lib.ShardCnt),
	}
	pch.Logger.Debug("spGlobal: %v", spGlobal)
	spGlobalEntityKey := exApi.kf.spGlobalEntityKey()
	spGlobalEntityVal, err := proto.Marshal(spGlobal)
	if err != nil {
		pch.Logger.Error("Marshal spGlobal err: %v %v", spGlobal, err)
		return &pbcp.CreateClusterReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: lib.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	spGlobalEntityValStr := string(spGlobalEntityVal)

	apply := func(stm concurrency.STM) error {
		if val := []byte(stm.Get(clusterEntityKey)); len(val) != 0 {
			return &cpStmError{
				code: lib.ReplyCodeDupRes,
				msg:  clusterEntityKey,
			}
		}
		stm.Put(clusterEntityKey, clusterEntityValStr)

		if val := []byte(stm.Get(dnGlobalEntityKey)); len(val) != 0 {
			return &cpStmError{
				code: lib.ReplyCodeDupRes,
				msg:  dnGlobalEntityKey,
			}
		}
		stm.Put(dnGlobalEntityKey, dnGlobalEntityValStr)

		if val := []byte(stm.Get(cnGlobalEntityKey)); len(val) != 0 {
			return &cpStmError{
				code: lib.ReplyCodeDupRes,
				msg:  cnGlobalEntityKey,
			}
		}
		stm.Put(cnGlobalEntityKey, cnGlobalEntityValStr)

		if val := []byte(stm.Get(spGlobalEntityKey)); len(val) != 0 {
			return &cpStmError{
				code: lib.ReplyCodeDupRes,
				msg:  spGlobalEntityKey,
			}
		}
		stm.Put(spGlobalEntityKey, spGlobalEntityValStr)

		return nil
	}

	err = exApi.sm.runStm(pch, apply)
	if err != nil {
		if serr, ok := err.(*cpStmError); ok {
			return &pbcp.CreateClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.code,
					ReplyMsg:  serr.msg,
				},
			}, nil
		} else {
			return &pbcp.CreateClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: lib.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.CreateClusterReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: lib.ReplyCodeSucceed,
			ReplyMsg:  lib.ReplyMsgSucceed,
		},
	}, nil
}

func (exApi *exApiServer) DeleteCluster(
	ctx context.Context,
	req *pbcp.DeleteClusterRequest,
) (*pbcp.DeleteClusterReply, error) {
	pch := lib.GetPerCtxHelper(ctx)

	clusterEntityKey := exApi.kf.clusterEntityKey()
	dnGlobalEntityKey := exApi.kf.dnGlobalEntityKey()
	cnGlobalEntityKey := exApi.kf.cnGlobalEntityKey()
	spGlobalEntityKey := exApi.kf.spGlobalEntityKey()

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

	err := exApi.sm.runStm(pch, apply)
	if err != nil {
		if serr, ok := err.(*cpStmError); ok {
			return &pbcp.DeleteClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.code,
					ReplyMsg:  serr.msg,
				},
			}, nil
		} else {
			return &pbcp.DeleteClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: lib.ReplyCodeAgentErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.DeleteClusterReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: lib.ReplyCodeSucceed,
			ReplyMsg:  lib.ReplyMsgSucceed,
		},
	}, nil
}

func (exApi *exApiServer) GetCluster(
	ctx context.Context,
	req *pbcp.GetClusterRequest,
) (*pbcp.GetClusterReply, error) {
	pch := lib.GetPerCtxHelper(ctx)

	clusterEntityKey := exApi.kf.clusterEntityKey()
	cluster := &pbcp.Cluster{}

	apply := func(stm concurrency.STM) error {
		val := []byte(stm.Get(clusterEntityKey))
		if len(val) == 0 {
			return &cpStmError{
				lib.ReplyCodeUnknownRes,
				clusterEntityKey,
			}
		}
		err := proto.Unmarshal(val, cluster)
		pch.Logger.Debug("cluster: %v", cluster)
		return err
	}

	err := exApi.sm.runStm(pch, apply)
	if err != nil {
		if serr, ok := err.(*cpStmError); ok {
			return &pbcp.GetClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.code,
					ReplyMsg:  serr.msg,
				},
			}, nil
		} else {
			return &pbcp.GetClusterReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: lib.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.GetClusterReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: lib.ReplyCodeSucceed,
			ReplyMsg:  lib.ReplyMsgSucceed,
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
