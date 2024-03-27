package nodeagent

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

type dnAgentServer struct {
	pbnd.UnimplementedDnAgentServer
}

func (dnAgent *dnAgentServer) GteDevSize(
	ctx context.Context,
	req *pbnd.GetDevSizeRequest,
) (*pbnd.GetDevSizeReply, error) {
	pch := newDnAgentPerCtxHelper(ctx, req.ReqConf.ReqId)
	defer pch.close()
	size, err := pch.getBlockDevSize(req.DevPath)
	if err != nil {
		return &pbnd.GetDevSizeReply{
			ReplyInfo: &pbnd.ReplyInfo{
				ReqId: req.ReqConf.ReqId,
				Code: lib.RpcInternalErrCode,
				Msg: err.Error(),
			}
		}, nil
	}
	return &pbnd.GetDevSizeReply{
		ReqId: req.ReqConf.ReqId,
		StatusInfo: &pbnd.StatusInfo{
			Code: lib.RpcSucceedCode,
			Msg: lib.RpcSucceedMsg,
		},
		Size: size,
	}, nil
}

func newDnAgentServer() *dnAgentServer{
	return &dnAgentServer{}
}
