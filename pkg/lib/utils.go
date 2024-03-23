package lib

import (
	"context"

	"google.golang.org/grpc"
	"github.com/google/uuid"
)

type ctxKey string

func (c ctxKey) String() string {
	return "ctx key " + string(c)
}

var (
	ctxKeyReqId = ctxKey("reqId")
)

func SetReqIdInterceptor(
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

func GetReqId(ctx context.Context) string {
	reqId, ok := ctx.Value(ctxKeyReqId).(string)
	if ok {
		return reqId
	} else {
		return "???"
	}
}

func ShowReqReplyInterceptor(logger *Logger) grpc.UnaryServerInterceptor {
	return func (
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		logger.Info("gRPC request: %v", req)
		logger.Info("reqId: %v", GetReqId(ctx))
		reply, err := handler(ctx, req)
		if err != nil {
			logger.Error("gRPC error: %v", err)
		} else {
			logger.Info("gRPC reply: %v", reply)
		}
		return reply, err
	}
}
