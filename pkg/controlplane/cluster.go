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
		DataExtentCntShift: lib.DataExtentCntShiftDefault,
		MetaExtentSizeShift: lib.MetaExtentSizeShiftDefault,
		MetaExtentCntShift: lib.MetaExtentCntShiftDefault,
		ExtentRatio: lib.ExtentRatioShiftDefault,
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

	// globalSummary := &pbds.GlobalSummary{
	// 	DnSummary: &pbds.DnSummary{},
	// 	CnSummary: &pbds.CnSummary{},
	// 	SpSummary: &pbds.SpSummary{},
	// }
	// globalSummaryEntityKey := cpas.kf.GlobalSummaryEntityKey()
	// globalSummaryEntityVal, err := proto.Marshal(globalSummary)
	// if err != nil {
	// 	pch.logger.Error("Marshal globalSummary err: %v %v", globalSummary, err)
	// 	return &pbcp.CreateClusterReply{
	// 		ReplyInfo: &pbcp.ReplyInfo{
	// 			ReqId:     lib.GetReqId(ctx),
	// 			ReplyCode: lib.CpApiInternalErrCode,
	// 			ReplyMsg:  err.Error(),
	// 		},
	// 	}, nil
	// }
	// globalSummaryEntityValStr := string(globalSummaryEntityVal)

	// dnGlobal := &bpds.DnGlobal{
	// 	GlobalCounter: 0,
	// 	FullExtentDnCnt: 0,
	// 	ShardCntList: make([]uint32, lib.ShardSize),
	// 	PortNextBit: &pbds.NextBit{
	// 		CurrIdx: 0,
	// 		Bitmap: make([]byte, lib.DnPortSize / 8),
	// 	}
	// }
	// dnGlobalEntityKey := cpas.kf.DnGlobalEntityKey()
	// dnGlobalEntityVal, err := proto.Marshal(dnGlobal)
	// if err != nil {
	// 	pch.logger.Error("Marshal dnGlobal err: %v %v", dnGlobal, err)
	// 	return &pbcp.CreateClusterReply{
	// 		ReplyInfo: &pbcp.ReplyInfo{
	// 			ReqId:     lib.GetReqId(ctx),
	// 			ReplyCode: lib.CpApiInternalErrCode,
	// 			ReplyMsg:  err.Error(),
	// 		},
	// 	}, nil
	// }
	// dnGlobalEntityValStr := string(dnGlobalEntityVal)

	// cnGlobal := &pbds.CnGlobal{
	// 	GlobalCounter: 0,
	// 	ShardCntList: make([]uint32, lib.ShardSize),
	// }
	// cnGlobalEntityKey := cpas.kf.CnGlobalEntityKey()
	// cnGlobalEntityVal, err := proto.Marshal(cnGlobal)
	// if err != nil {
	// 	pch.logger.Error("Marshal cnGlobal err: %v %v", cnGlobal, err)
	// 	return &pbcp.CreateClusterReply{
	// 		ReplyInfo: &pbcp.ReplyInfo{
	// 			ReqId:     lib.GetReqId(ctx),
	// 			ReplyCode: lib.CpApiInternalErrCode,
	// 			ReplyMsg:  err.Error(),
	// 		},
	// 	}, nil
	// }
	// cnGlobalEntityValStr := string(cnGlobalEntityVal)

	// spGlobal := &pbds.SpGlobal{
	// 	GlobalCounter: 0,
	// 	ShardCntList: make([]uint32, lib.ShardSize),
	// }
	// spGlobalEntityKey := cpas.kf.SpGlobalEntityKey()
	// spGlobalEntityVal, err := proto.Marshal(spGlobal)
	// if err != nil {
	// 	pch.logger.Error("Marshal spGlobal err: %v %v", spGlobal, err)
	// 	return &pbcp.CreateClusterReply{
	// 		ReplyInfo: &pbcp.ReplyInfo{
	// 			ReqId:     lib.GetReqId(ctx),
	// 			ReplyCode: lib.CpApiInternalErrCode,
	// 			ReplyMsg:  err.Error(),
	// 		},
	// 	}, nil
	// }
	// spGlobalEntityValStr := string(spGlobalEntityVal)

	apply := func(stm concurrency.STM) error {
		if val := []byte(stm.Get(clusterEntityKey)); len(val) != 0 {
			return &cpStmError{
				code: lib.CpApiDupResErrCode,
				msg:  clusterEntityKey,
			}
		}
		stm.Put(clusterEntityKey, clusterEntityValStr)
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

	apply := func(stm concurrency.STM) error {
		if len(stm.Get(clusterEntityKey)) > 0 {
			stm.Del(clusterEntityKey)
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
	}, nil
}
