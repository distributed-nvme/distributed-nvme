package lib

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"github.com/google/uuid"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
)

type ctxKey string

func (c ctxKey) String() string {
	return "ctx key " + string(c)
}

var (
	ctxKeyReqId = ctxKey("reqId")
)

func GetReqId(ctx context.Context) string {
	reqId, ok := ctx.Value(ctxKeyReqId).(string)
	if ok {
		return reqId
	} else {
		return "???"
	}
}

func UnarySetReqIdInterceptor(logger *Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	)  (interface{}, error) {
		reqId := uuid.New().String()
		newCtx := context.WithValue(ctx, ctxKeyReqId, reqId)
		logger.Info("reqId: %v", reqId)
		reply, err := handler(newCtx, req)
		return reply, err
	}
}

func UnaryShowReqReplyInterceptor(logger *Logger) grpc.UnaryServerInterceptor {
	return func(
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

func InterceptorLogger(l *Logger) logging.Logger {
	return logging.LoggerFunc(func(_ context.Context, lvl logging.Level, msg string, fields ...any) {
		switch lvl {
		case logging.LevelDebug:
			l.Debug(msg, fields...)
		case logging.LevelInfo:
			l.Info(msg, fields...)
		case logging.LevelWarn:
			l.Warning(msg, fields...)
		case logging.LevelError:
			l.Error(msg, fields...)
		default:
			panic(fmt.Sprintf("unknown level %v", lvl))
		}
	})
}
