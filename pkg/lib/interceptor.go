package lib

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
)

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
