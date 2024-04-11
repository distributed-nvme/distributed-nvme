package nodeagent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/ctxhelper"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/localdata"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/oscmd"
	pbnd "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeagent"
)

type dnAgentServer struct {
	pbnd.UnimplementedDiskNodeAgentServer
	mu sync.Mutex
	oc *oscmd.OsCommand
	lData *localdata.LocalData
	dnData *localdata.DnData
	spLdDataList []*localdata.SpLdData
}

func (dnAgent *dnAgentServer) GetDevSize(
	ctx context.Context,
	req *pbnd.GetDevSizeRequest,
) (*pbnd.GetDevSizeReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)
	size, err := dnAgent.oc.GetBlockDevSize(pch, req.DevPath)
	if err != nil {
		return &pbnd.GetDevSizeReply{
			StatusInfo: &pbnd.StatusInfo{
				Code: constants.StatusCodeInternalErr,
				Msg:  err.Error(),
				Timestamp: time.Now().UnixMilli(),
			},
		}, nil
	}
	return &pbnd.GetDevSizeReply{
		StatusInfo: &pbnd.StatusInfo{
			Code: constants.StatusCodeSucceed,
			Msg:  constants.StatusMsgSucceed,
			Timestamp: time.Now().UnixMilli(),
		},
		Size: size,
	}, nil
}

func (dnAgent *dnAgentServer) Syncup(
	ctx context.Context,
	req *pbnd.SyncupDnRequest,
) (*pbnd.SyncupDnReply, error) {
	pch := ctxhelper.GetPerCtxHelper(ctx)
	timestamp := time.Now().UnixMilli()
	dnId := fmt.Sprintf("%016x", req.DnConf.DnId)
	if dnAgent.dnData == nil {
		dnData, err := dnAgent.lData.GetDnData(pch, dnId)
		if err != nil {
			return &pbnd.SyncupDnReply{
				DnInfo: &pbnd.DnInfo{
					StatusInfo: &pbnd.StatusInfo{
						Code: constants.StatusCodeInternalErr,
						Msg: err.Error(),
						Timestamp: time.Now().UnixMilli(),
					},
				},
			}, nil
		}
		dnAgent.dnData = dnData
	}

	if dnId != dnAgent.dnData.DnId {
		return &pbnd.SyncupDnReply{
			DnInfo: &pbnd.DnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code: constants.StatusCodeIdMisMatch,
					Msg: fmt.Sprintf("DnId: %s", dnAgent.dnData.DnId),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	if req.DnConf.Revision < dnAgent.dnData.Revision {
		return &pbnd.SyncupDnReply{
			DnInfo: &pbnd.DnInfo{
				StatusInfo: &pbnd.StatusInfo{
					Code: constants.StatusCodeOldRevision,
					Msg: fmt.Sprintf("Revision: %s", dnAgent.dnData.Revision),
					Timestamp: timestamp,
				},
			},
		}, nil
	}

	return &pbnd.SyncupDnReply{
		DnInfo: &pbnd.DnInfo{
			StatusInfo: &pbnd.StatusInfo{
				Code: constants.StatusCodeSucceed,
				Msg: constants.StatusMsgSucceed,
				Timestamp: timestamp,
			},
		},
	}, nil
}

func newDnAgentServer(
	ldataPath string,
) *dnAgentServer {
	return &dnAgentServer{
		oc: oscmd.NewOsCommand(),
		lData: localdata.NewLocalData(ldataPath),
		dnData: nil,
		spLdDataList: make([]*localdata.SpLdData, 0),
	}
}
