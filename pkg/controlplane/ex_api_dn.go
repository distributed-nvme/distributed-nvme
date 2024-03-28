package controlplane

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
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
	if err := validStringLength(req.PortConf.NvmeListener.TrType, "TrType"); err != nil {
		return err
	}
	if err := validStringLength(req.PortConf.NvmeListener.AdrFam, "AdrFam"); err != nil {
		return err
	}
	if err := validStringLength(req.PortConf.NvmeListener.TrAddr, "TrAddr"); err != nil {
		return err
	}
	if err := validStringLength(req.PortConf.NvmeListener.TrSvcId, "TrSvcId"); err != nil {
		return err
	}
	if req.PortConf.PortNum > lib.PortNumMax {
		return fmt.Errorf("PortNum larger than %d", lib.PortNumMax)
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
	pch := lib.GetPerCtxHelper(ctx)

	if err := validDnReq(req); err != nil {
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: lib.ReplyCodeInvalidArg,
				ReplyMsg: err.Error(),
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
			lib.UnaryClientPerCtxHelperInterceptor,
		),
	)
	if err != nil {
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: lib.ReplyCodeInternalErr,
				ReplyMsg: err.Error(),
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
				ReplyCode: lib.ReplyCodeAgentErr,
				ReplyMsg: err.Error(),
			},
		}, nil
	}
	if getDevSizeReply.StatusInfo.Code != lib.StatusCodeSucceed {
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReplyCode: lib.ReplyCodeAgentErr,
				ReplyMsg: fmt.Sprintf(
					"%d %s",
					getDevSizeReply.StatusInfo.Code,
					getDevSizeReply.StatusInfo.Msg,
				),
			},
		}, nil
	}

	cluster, _ := exApi.getCluster(pch)
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
	metaBaseAddr := 0
	dataBaseAddr := uint32(metaBaseAddr) + uint32(metaExtentCnt) * (1 << cluster.MetaExtentSizeShift)
	pch.Logger.Info("%v %v %v %v %v %v %v %v",
		metaBitmap, metaBucket, metaExtentCnt,
		dataBitmap, dataBucket, dataExtentCnt,
		metaBaseAddr, dataBaseAddr,
	)

	return &pbcp.CreateDnReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: lib.ReplyCodeSucceed,
			ReplyMsg: lib.ReplyMsgSucceed,
		},
	}, nil
}
