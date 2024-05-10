package exapi

import (
	"context"
	"fmt"

	"go.etcd.io/etcd/client/v3/concurrency"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/stmwrapper"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

func validDnReq(req *pbcp.CreateDnRequest) error {
	if err := validStringLength(req.GrpcTarget, "GrpcTarget"); err != nil {
		return err
	}
	if err := validStringLength(req.DevPath, "DevPath"); err != nil {
		return err
	}
	if err := validStringLength(req.TrType, "TrType"); err != nil {
		return err
	}
	if err := validStringLength(req.AdrFam, "AdrFam"); err != nil {
		return err
	}
	if err := validStringLength(req.TrAddr, "TrAddr"); err != nil {
		return err
	}
	if err := validStringLength(req.TrSvcId, "TrSvcId"); err != nil {
		return err
	}
	if req.PortNum > constants.PortNumMax {
		return fmt.Errorf("PortNum larger than %d", constants.PortNumMax)
	}
	for _, tag := range req.TagList {
		if err := validStringLength(tag.Key, "tag Key "+tag.Key); err != nil {
			return err
		}
		if err := validStringLength(tag.Value, "tag Value "+tag.Value); err != nil {
			return err
		}
	}
	return nil
}

func (exApi *exApiServer) CreateDn(
	ctx context.Context,
	req *pbcp.CreateDnRequest,
) (*pbcp.CreateDnReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)

	if err := validDnReq(req); err != nil {
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInvalidArg,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	conn, err := grpc.DialContext(
		ctx,
		req.GrpcTarget,
		grpc.WithInsecure(),
		grpc.WithBlock(),
		grpc.WithTimeout(exApi.agentTimeout),
		grpc.WithChainUnaryInterceptor(
			ctxhelper.UnaryClientPerCtxHelperInterceptor,
		),
	)
	if err != nil {
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	conn.Close()

	c := pbnd.NewDiskNodeAgentClient(conn)
	getDevSizeRequest := &pbnd.GetDevSizeRequest{
		DevPath: req.DevPath,
	}
	getDevSizeReply, err := c.GetDevSize(ctx, getDevSizeRequest)
	if err != nil {
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeAgentErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	if getDevSizeReply.StatusInfo.Code != constants.StatusCodeSucceed {
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeAgentErr,
				ReplyMsg: fmt.Sprintf(
					"%d %s",
					getDevSizeReply.StatusInfo.Code,
					getDevSizeReply.StatusInfo.Msg,
				),
			},
		}, nil
	}

	cluster, err := exApi.getCluster(pch)
	if err != nil {
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	metaSize := getDevSizeReply.Size >> cluster.ExtentRatioShift
	dataSize := getDevSizeReply.Size - metaSize

	metaBitmap, metaBucket, metaExtentCnt := extentInitCalc(
		metaSize,
		cluster.MetaExtentSizeShift,
		cluster.MetaExtentPerSetShift,
	)

	dataBitmap, dataBucket, dataExtentCnt := extentInitCalc(
		dataSize,
		cluster.DataExtentSizeShift,
		cluster.DataExtentPerSetShift,
	)

	metaBaseAddr := uint64(0)
	dataBaseAddr := metaBaseAddr + uint64(metaExtentCnt)*(1<<cluster.MetaExtentSizeShift)

	dnConf := &pbcp.DiskNodeConf{
		TagList: req.TagList,
		GeneralConf: &pbcp.DnGeneralConf{
			GrpcTarget: req.GrpcTarget,
			Online:     req.Online,
			DevPath:    req.DevPath,
			NvmePortConf: &pbcp.NvmePortConf{
				PortNum: string(req.PortNum),
				NvmeListener: &pbcp.NvmeListener{
					TrType:  req.TrType,
					AdrFam:  req.AdrFam,
					TrAddr:  req.TrAddr,
					TrSvcId: req.TrSvcId,
				},
				TrEq: &pbcp.NvmeTReq{
					SeqCh: 0,
				},
			},
			DnCapacity: &pbcp.DnCapacity{
				LdCnt:                0,
				TotalQos:             0,
				FreeQos:              0,
				MetaMaxExtentSetSize: metaBucket[0],
				MetaTotalExtentCnt:   metaExtentCnt,
				DataMaxExtentSetSize: dataBucket[0],
				DataTotalExtentCnt:   dataExtentCnt,
			},
			MetaExtentConf: &pbcp.ExtentConf{
				BaseAddr:        metaBaseAddr,
				ExtentSetBucket: metaBucket,
				Bitmap:          metaBitmap,
			},
			DataExtentConf: &pbcp.ExtentConf{
				BaseAddr:        dataBaseAddr,
				ExtentSetBucket: dataBucket,
				Bitmap:          dataBitmap,
			},
		},
		SpLdIdList: make([]*pbcp.SpLdId, 0),
	}
	dnConfVal, err := proto.Marshal(dnConf)
	if err != nil {
		pch.Logger.Error("Marshal dnConf err: %v %v", dnConf, err)
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	dnConfStr := string(dnConfVal)

	dnInfo := &pbcp.DiskNodeInfo{
		ConfRev: 0,
		StatusInfo: &pbcp.StatusInfo{
			Code:      constants.StatusCodeUninit,
			Msg:       "uninit",
			Timestamp: pch.Timestamp,
		},
	}
	dnInfoVal, err := proto.Marshal(dnInfo)
	if err != nil {
		pch.Logger.Error("Marshal dnInfo err: %v %v", dnInfo, err)
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: constants.ReplyCodeInternalErr,
				ReplyMsg:  err.Error(),
			},
		}, nil
	}
	dnInfoStr := string(dnInfoVal)

	dnGlobalKey := exApi.kf.DnGlobalEntityKey()
	dnGlobal := &pbcp.DnGlobal{}

	apply := func(stm concurrency.STM) error {
		val := []byte(stm.Get(dnGlobalKey))
		if len(val) == 0 {
			pch.Logger.Error("No dnGlobal: %s", dnGlobalKey)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				dnGlobalKey,
			}
		}
		if err := proto.Unmarshal(val, dnGlobal); err != nil {
			pch.Logger.Error(
				"dnGlobal unmarshal err: %s %v",
				dnGlobalKey,
				err,
			)
			return &stmwrapper.StmError{
				constants.ReplyCodeInternalErr,
				err.Error(),
			}
		}
		dnGlobal.GlobalCounter++
		counter := dnGlobal.GlobalCounter
		minCnt := constants.Uint32Max
		idx := -1
		for i, cnt := range dnGlobal.ShardBucket {
			if cnt < minCnt {
				minCnt = cnt
				idx = i
			}
		}
		if idx < 0 {
			panic("Do not find minimal cnt")
		}
		dnGlobal.ShardBucket[idx] = dnGlobal.ShardBucket[idx] + 1
		dnIdNum := (uint64(idx) << (64 - constants.ShardShift)) | counter
		dnId := fmt.Sprintf("%016x", dnIdNum)

		dnConfKey := exApi.kf.DnConfEntityKey(dnId)
		if val := []byte(stm.Get(dnConfKey)); len(val) != 0 {
			return &stmwrapper.StmError{
				Code: constants.ReplyCodeDupRes,
				Msg:  dnConfKey,
			}
		}
		stm.Put(dnConfKey, dnConfStr)
		dnInfoKey := exApi.kf.DnInfoEntityKey(dnId)
		if val := []byte(stm.Get(dnInfoKey)); len(val) != 0 {
			return &stmwrapper.StmError{
				Code: constants.ReplyCodeDupRes,
				Msg:  dnInfoKey,
			}
		}
		stm.Put(dnInfoKey, dnInfoStr)

		return nil
	}

	if err = exApi.sm.RunStm(pch, apply); err != nil {
		if serr, ok := err.(*stmwrapper.StmError); ok {
			return &pbcp.CreateDnReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: serr.Code,
					ReplyMsg:  serr.Msg,
				},
			}, nil
		} else {
			return &pbcp.CreateDnReply{
				ReplyInfo: &pbcp.ReplyInfo{
					ReplyCode: constants.ReplyCodeInternalErr,
					ReplyMsg:  err.Error(),
				},
			}, nil
		}
	}

	return &pbcp.CreateDnReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: constants.ReplyCodeSucceed,
			ReplyMsg:  constants.ReplyMsgSucceed,
		},
	}, nil
}
