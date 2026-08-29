package common

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// pbLogAttr renders a gRPC message for logging (log.md R10, grpc.md L1/L5).
func pbLogAttr(key string, msg any) slog.Attr {
	if pbMsg, ok := msg.(proto.Message); ok {
		return slog.Any(key, PbToLogValue(pbMsg))
	}
	return slog.Any(key, msg)
}

// attachTraceId copies the ctx trace id into the outgoing metadata (T1).
func attachTraceId(ctx context.Context) context.Context {
	if traceId, ok := TraceIdFromCtx(ctx); ok {
		return metadata.AppendToOutgoingContext(
			ctx, TraceIdMetadataKey, traceId,
		)
	}
	return ctx
}

// extractTraceId copies the incoming-metadata trace id into the ctx (T2).
func extractTraceId(ctx context.Context) context.Context {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(TraceIdMetadataKey); len(vals) > 0 && vals[0] != "" {
			return WithTraceId(ctx, vals[0])
		}
	}
	return ctx
}

// ---------------------------------------------------------------------------
// Client side
// ---------------------------------------------------------------------------

// GrpcUnaryClientInterceptor propagates the trace id and logs the request and
// reply of every unary call.
func GrpcUnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req any,
		reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		ctx = attachTraceId(ctx)
		slog.InfoContext(ctx, "grpc client request",
			slog.String("method", method),
			pbLogAttr("data", req),
		)
		err := invoker(ctx, method, req, reply, cc, opts...)
		if err != nil {
			slog.InfoContext(ctx, "grpc client reply",
				slog.String("method", method),
				slog.String("error", err.Error()),
			)
			return err
		}
		slog.InfoContext(ctx, "grpc client reply",
			slog.String("method", method),
			pbLogAttr("data", reply),
		)
		return nil
	}
}

// GrpcStreamClientInterceptor propagates the trace id at stream creation and
// logs every sent/received message.
func GrpcStreamClientInterceptor() grpc.StreamClientInterceptor {
	return func(
		ctx context.Context,
		desc *grpc.StreamDesc,
		cc *grpc.ClientConn,
		method string,
		streamer grpc.Streamer,
		opts ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		ctx = attachTraceId(ctx)
		clientStream, err := streamer(ctx, desc, cc, method, opts...)
		if err != nil {
			slog.InfoContext(ctx, "grpc client stream open",
				slog.String("method", method),
				slog.String("error", err.Error()),
			)
			return nil, err
		}
		slog.InfoContext(ctx, "grpc client stream open",
			slog.String("method", method),
		)
		return &loggingClientStream{
			ClientStream: clientStream,
			ctx:          ctx,
			method:       method,
		}, nil
	}
}

type loggingClientStream struct {
	grpc.ClientStream
	ctx    context.Context
	method string
}

func (s *loggingClientStream) SendMsg(m any) error {
	err := s.ClientStream.SendMsg(m)
	if err != nil {
		slog.InfoContext(s.ctx, "grpc client send",
			slog.String("method", s.method),
			slog.String("error", err.Error()),
		)
		return err
	}
	slog.InfoContext(s.ctx, "grpc client send",
		slog.String("method", s.method),
		pbLogAttr("data", m),
	)
	return nil
}

func (s *loggingClientStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if errors.Is(err, io.EOF) {
		return err // normal end of stream: not logged (L4)
	}
	if err != nil {
		slog.InfoContext(s.ctx, "grpc client recv",
			slog.String("method", s.method),
			slog.String("error", err.Error()),
		)
		return err
	}
	slog.InfoContext(s.ctx, "grpc client recv",
		slog.String("method", s.method),
		pbLogAttr("data", m),
	)
	return nil
}

// ---------------------------------------------------------------------------
// Server side
// ---------------------------------------------------------------------------

// GrpcUnaryServerInterceptor extracts the trace id from metadata into the ctx
// and logs the request and reply of every unary call.
func GrpcUnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		ctx = extractTraceId(ctx)
		slog.InfoContext(ctx, "grpc server request",
			slog.String("method", info.FullMethod),
			pbLogAttr("data", req),
		)
		reply, err := handler(ctx, req)
		if err != nil {
			slog.InfoContext(ctx, "grpc server reply",
				slog.String("method", info.FullMethod),
				slog.String("error", err.Error()),
			)
			return reply, err
		}
		slog.InfoContext(ctx, "grpc server reply",
			slog.String("method", info.FullMethod),
			pbLogAttr("data", reply),
		)
		return reply, nil
	}
}

// GrpcStreamServerInterceptor extracts the trace id into the stream context
// and logs every received/sent message.
func GrpcStreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv any,
		serverStream grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		ctx := extractTraceId(serverStream.Context())
		slog.InfoContext(ctx, "grpc server stream open",
			slog.String("method", info.FullMethod),
		)
		wrapped := &loggingServerStream{
			ServerStream: serverStream,
			ctx:          ctx,
			method:       info.FullMethod,
		}
		err := handler(srv, wrapped)
		if err != nil {
			slog.InfoContext(ctx, "grpc server stream close",
				slog.String("method", info.FullMethod),
				slog.String("error", err.Error()),
			)
			return err
		}
		slog.InfoContext(ctx, "grpc server stream close",
			slog.String("method", info.FullMethod),
		)
		return nil
	}
}

type loggingServerStream struct {
	grpc.ServerStream
	ctx    context.Context
	method string
}

// Context returns the trace-enriched ctx so handler code (and everything it
// calls: OsClient, etcd helpers, outbound RPCs) logs with the trace id (T2).
func (s *loggingServerStream) Context() context.Context {
	return s.ctx
}

func (s *loggingServerStream) RecvMsg(m any) error {
	err := s.ServerStream.RecvMsg(m)
	if errors.Is(err, io.EOF) {
		return err // client finished sending: not logged (L4)
	}
	if err != nil {
		slog.InfoContext(s.ctx, "grpc server recv",
			slog.String("method", s.method),
			slog.String("error", err.Error()),
		)
		return err
	}
	slog.InfoContext(s.ctx, "grpc server recv",
		slog.String("method", s.method),
		pbLogAttr("data", m),
	)
	return nil
}

func (s *loggingServerStream) SendMsg(m any) error {
	err := s.ServerStream.SendMsg(m)
	if err != nil {
		slog.InfoContext(s.ctx, "grpc server send",
			slog.String("method", s.method),
			slog.String("error", err.Error()),
		)
		return err
	}
	slog.InfoContext(s.ctx, "grpc server send",
		slog.String("method", s.method),
		pbLogAttr("data", m),
	)
	return nil
}
