package controlplane

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbcp "github.com/distributed-nvme/distributed-nvme/pkg/proto/cpapi"
)

func (cpas *cpApiServer) CreateDn(ctx context.Context, req *pbcp.CreateDnRequest) (
	*pbcp.CreateDnReply, error) {
	cpas.logger.Info("Hello world!")
	return &pbcp.CreateDnReply{
		ReqId:     lib.GetReqId(ctx),
		ReplyInfo: &pbcp.ReplyInfo{
			ReplyCode: 255,
			ReplyMsg:  "hello",
		},
	}, nil
}
