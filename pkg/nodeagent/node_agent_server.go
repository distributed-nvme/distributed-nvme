package nodeagent

import (
	"context"

	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbNdApi "github.com/distributed-nvme/distributed-nvme/pkg/proto/nodeapi"
)

type dnAgentServer struct {
	pbNdApi.UnimplementedDnAgentServer
	logger *lib.Logger
	oc *lib.OsCmd
}

func newDnAgentServer(logger *lib.Logger) *dnAgentServer {
	return &dnAgentServer{
		logger: logger,
		oc: lib.NewOsCmd(logger),
	}
}

type cnAgentServer struct {
	pbNdApi.UnimplementedCnAgentServer
	logger *lib.Logger
	oc *lib.OsCmd
}

func newCnAgentServer(logger *lib.Logger) *cnAgentServer {
	return &cnAgentServer{
		logger: logger,
		oc: lib.NewOsCmd(logger),
	}
}

func showReqReplyInterceptor(logger *lib.Logger) grpc.UnaryServerInterceptor {
	return func (
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		logger.Info("gRPC request: %v", req)
		reply, err := handler(ctx, req)
		if err != nil {
			logger.Error("gRPC error: %v", err)
		} else {
			logger.Info("gRPC reply: %v", reply)
		}
		return reply, err
	}
}
