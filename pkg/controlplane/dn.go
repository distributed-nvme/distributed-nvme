package controlplane

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/controlplaneapi"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeapi"
)

func (cpas *cpApiServer) CreateDn(ctx context.Context, req *pbcp.CreateDnRequest) (
	*pbcp.CreateDnReply, error) {
	pch := newPerCtxHelper(ctx, cpas)
	defer pch.close()
	client, err := pch.getDnAgentClient(req.SockAddr)
	if err != nil {
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReqId: lib.GetReqId(ctx),
				ReplyCode: lib.CpApiAgentConnErrCode,
				ReplyMsg: err.Error(),
			},
		}, nil
	}
	getDevSizeRequest := &pbnd.GetDevSizeRequest{
		ReqId: lib.GetReqId(ctx),
		DevPath: req.DevPath,
	}
	getDevSizeReply, err := client.GetDevSize(ctx, getDevSizeRequest)
	if err != nil {
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReqId: lib.GetReqId(ctx),
				ReplyCode: lib.CpApiAgentGrpcErrCode,
				ReplyMsg: err.Error(),
			},
		}, nil
	}
	if getDevSizeReply.StatusInfo.Code != lib.AgentSucceedCode {
		return &pbcp.CreateDnReply{
			ReplyInfo: &pbcp.ReplyInfo{
				ReqId: lib.GetReqId(ctx),
				ReplyCode: lib.CpApiAgentReplyErrCode,
				ReplyMsg: getDevSizeReply.StatusInfo.Msg,
			},
		}, nil
	}
	return &pbcp.CreateDnReply{
		ReplyInfo: &pbcp.ReplyInfo{
			ReqId: lib.GetReqId(ctx),
			ReplyCode: lib.CpApiSucceedCode,
			ReplyMsg: lib.CpApiSucceedMsg,
		},
	}, nil
}
