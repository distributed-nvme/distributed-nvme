package controlplane

import (
	"context"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"github.com/google/uuid"

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

type ctxKey string

func (c ctxKey) String() string {
	return "ctx key " + string(c)
}

var (
	ctxKeyReqId = ctxKey("reqId")
)

func setReqIdInterceptor(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	reqId := uuid.New().String()
	newCtx := context.WithValue(ctx, ctxKeyReqId, reqId)
	reply, err := handler(newCtx, req)
	return reply, err
}

func getReqId(ctx context.Context) string {
	reqId, ok := ctx.Value(ctxKeyReqId).(string)
	if ok {
		return reqId
	} else {
		return "???"
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
		logger.Info("reqId: %v", getReqId(ctx))
		reply, err := handler(ctx, req)
		if err != nil {
			logger.Error("gRPC error: %v", err)
		} else {
			logger.Info("gRPC reply: %v", reply)
		}
		return reply, err
	}
}
