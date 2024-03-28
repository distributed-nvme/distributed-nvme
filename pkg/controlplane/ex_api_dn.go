package controlplane

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplane"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

func (exApi *exApiServer) CreateDn(
	ctx context.Context,
	req *pbcp.CreateDnRequest,
) (*pbcp.CreateDnReply, error) {
	pch := lib.GetPerCtxHelper(ctx)
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
	_, _, _, _ = extentInitCalc(metaSize, cluster.MetaExtentSizeShift, cluster.MetaExtentPerSetShift)
	_, _, _, _ = extentInitCalc(dataSize, cluster.DataExtentSizeShift, cluster.DataExtentPerSetShift)

	return &pbcp.CreateDnReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: lib.ReplyCodeSucceed,
			ReplyMsg: lib.ReplyMsgSucceed,
		},
	}, nil
}
