package nodeagent

import (
	"context"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeapi"
)

func (dnAgent *dnAgentServer) GetDevSize(
	ctx context.Context,
	req *pbnd.GetDevSizeRequest,
) (*pbnd.GetDevSizeReply, error){
	size, err := dnAgent.oc.GetBlockDevSize(req.DevPath)
	if err != nil {
		return &pbnd.GetDevSizeReply{
			ReqId: req.ReqId,
			StatusInfo: &pbnd.StatusInfo{
				StatusCode: lib.AgentOsCmdErrCode,
				StatusMsg: err.Error(),
			},
		}, nil
	}
	return &pbnd.GetDevSizeReply{
		ReqId: req.ReqId,
		StatusInfo: &pbnd.StatusInfo{
			StatusCode: lib.AgentSucceedCode,
			StatusMsg: lib.AgentSucceedMsg,
		},
		Size: size,
	}, nil
}
