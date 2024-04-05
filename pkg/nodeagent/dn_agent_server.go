package nodeagent

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

type dnAgentServer struct {
	pbnd.UnimplementedDiskNodeAgentServer
	oc *osCmd
}

func (dnAgent *dnAgentServer) GetDevSize(
	ctx context.Context,
	req *pbnd.GetDevSizeRequest,
) (*pbnd.GetDevSizeReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)
	size, err := dnAgent.oc.getBlockDevSize(pch, req.DevPath)
	if err != nil {
		return &pbnd.GetDevSizeReply{
			StatusInfo: &pbnd.StatusInfo{
				Code: constants.StatusCodeInternalErr,
				Msg:  err.Error(),
			},
		}, nil
	}
	return &pbnd.GetDevSizeReply{
		StatusInfo: &pbnd.StatusInfo{
			Code: constants.StatusCodeSucceed,
			Msg:  constants.StatusMsgSucceed,
		},
		Size: size,
	}, nil
}

func newDnAgentServer() *dnAgentServer {
	return &dnAgentServer{
		oc: &osCmd{},
	}
}
