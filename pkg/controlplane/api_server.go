package controlplane

import (
	"context"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib"
	pbCpApi "github.com/distributed-nvme/distributed-nvme/pkg/proto/cpapi"
)

type cpApiServer struct {
	pbCpApi.UnimplementedControlPlaneServer
	etcdCli *clientv3.Client
	logger *lib.Logger
}

func newCpApiServer(etcdCli *clientv3.Client, logger *lib.Logger) *cpApiServer {
	return &cpApiServer{
		etcdCli: etcdCli,
		logger: logger,
	}
}

func cpApiUnaryInterceptor(logger *lib.Logger) grpc.UnaryServerInterceptor {
	return func (
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		logger.Info("API request: %v", req)
		reply, err := handler(ctx, req)
		if err != nil {
			logger.Error("API error: %v", err)
		} else {
			logger.Info("API reply: %v", reply)
		}
		return reply, err
	}
}

