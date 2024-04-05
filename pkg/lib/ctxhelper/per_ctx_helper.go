package ctxhelper

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/distributed-nvme/distributed-nvme/pkg/lib/constants"
	"github.com/distributed-nvme/distributed-nvme/pkg/lib/prefixlog"
)

type PerCtxHelper struct {
	Ctx     context.Context
	Cancel  context.CancelFunc
	Logger  *prefixlog.PrefixLogger
	TraceId string
}

type ctxKey string

func (c ctxKey) String() string {
	return "ctx key " + string(c)
}

var (
	ctxKeyPch = ctxKey("PerCtxHelper")
)

func NewPerCtxHelper(
	parentCtx context.Context,
	logger *prefixlog.PrefixLogger,
	traceId string,
) *PerCtxHelper {
	pch := &PerCtxHelper{}
	tmpCtx := context.WithValue(parentCtx, ctxKeyPch, pch)
	ctx, cancel := context.WithCancel(tmpCtx)
	pch.Ctx = ctx
	pch.Cancel = cancel
	pch.Logger = logger
	pch.TraceId = traceId
	return pch
}

func buildPerCtxHelper(ctx context.Context, method string) *PerCtxHelper {
	var logger *prefixlog.PrefixLogger
	var traceId string

	md, ok := metadata.FromIncomingContext(ctx)
	if ok {
		traceIdList, ok := md[constants.TraceIdKey]
		if ok && len(traceIdList) > 0 {
			traceId = traceIdList[0]
			prefix := fmt.Sprintf("%s|%s ", method, traceId)
			logger = prefixlog.NewPrefixLogger(prefix)
			logger.Info("Set traceId from metadata")
		}
	}

	if logger == nil {
		traceId = uuid.New().String()
		prefix := fmt.Sprintf("%s|%s ", method, traceId)
		logger := prefixlog.NewPrefixLogger(prefix)
		logger.Info("No traceId in metadata, create a new one")
	}

	return NewPerCtxHelper(ctx, logger, traceId)
}

func GetPerCtxHelper(ctx context.Context) *PerCtxHelper {
	pch, ok := ctx.Value(ctxKeyPch).(*PerCtxHelper)
	if !ok {
		panic("No PerCtxHelper")
	}
	return pch
}

func UnaryServerPerCtxHelperInterceptor(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	pch := buildPerCtxHelper(ctx, info.FullMethod)
	pch.Logger.Info("Server side req: %v", req)
	reply, err := handler(pch.Ctx, req)
	if err != nil {
		pch.Logger.Error("Server side err: %v", err)
	} else {
		pch.Logger.Info("Server side reply: %v", reply)
	}
	return reply, err
}

func UnaryClientPerCtxHelperInterceptor(
	ctx context.Context,
	method string,
	req, reply any,
	cc *grpc.ClientConn,
	invoker grpc.UnaryInvoker,
	opts ...grpc.CallOption,
) error {
	pch := GetPerCtxHelper(ctx)
	md := metadata.Pairs(constants.TraceIdKey, pch.TraceId)
	newCtx := metadata.NewOutgoingContext(ctx, md)
	pch.Logger.Info("Client side req: %s %v", method, req)
	err := invoker(newCtx, method, req, reply, cc, opts...)
	if err != nil {
		pch.Logger.Error("Client side err: %v", err)
	} else {
		pch.Logger.Info("Client sdier reply: %v")
	}
	return err
}
