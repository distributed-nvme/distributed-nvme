package gateway

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// This file is update_05.md U1. gateway.md §0 #6 has always said the gateway
// is one of grpc.md T4's entry points — the one that mints a trace id for a
// request that arrived without one — and cmd/dnv-gateway/main.go and
// common/log.go both assert it as fact, but nothing implemented it: the
// shared server interceptor only ADOPTS an incoming id, so an id-less client
// produced gateway/etcd/agent log chains with no `trace_id` at all and
// forwarded none to the agents it called.
//
// The mint lives here, gateway-local, and not in common/interceptor.go
// because that file is a byte-for-byte copy of grpc.md §3's reference listing
// pinned by tests. Run installs it FIRST in both chains (server.go
// serverOptions), so it is upstream of the shared chain and the shared
// chain's own request/reply records carry the id.

// ensureTraceIdCtx returns ctx whose INCOMING metadata carries a trace_id,
// minting one when absent (gateway.md §0 #6, grpc.md T4's MAY). Injecting
// into the metadata — not the ctx value — upstream of the shared chain is
// deliberate: common's interceptor then adopts it exactly as "a request that
// arrived with one", its own request/reply records carry the id, and
// common/interceptor.go stays the grpc.md §3 reference verbatim
// (update_05.md U1).
func ensureTraceIdCtx(ctx context.Context) context.Context {
	md, ok := metadata.FromIncomingContext(ctx)
	if ok {
		if vals := md.Get(common.TraceIdMetadataKey); len(vals) > 0 &&
			vals[0] != "" {
			return ctx
		}
		md = md.Copy()
	} else {
		md = metadata.MD{}
	}
	md.Set(common.TraceIdMetadataKey, common.NewTraceId())
	return metadata.NewIncomingContext(ctx, md)
}

// ensureTraceIdUnary is the unary half: every unary handler — and therefore
// every interceptor chained behind this one — runs under a ctx whose incoming
// metadata carries a trace id (update_05.md U1).
func ensureTraceIdUnary() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		return handler(ensureTraceIdCtx(ctx), req)
	}
}

// ensureTraceIdStream is the stream half. A stream carries its ctx on the
// grpc.ServerStream rather than as an argument, so the id is delivered by
// wrapping the stream and overriding Context() — the same shape common's
// client stream wrapper uses (common/interceptor.go:226-228).
//
// `service Gateway` has no streaming RPC today (gateway.md §3 is 59 unary
// calls), so this half never runs; it is wired for symmetry with the shared
// chain, which installs both halves, so that the first streaming RPC added
// here inherits the mint instead of quietly losing it.
func ensureTraceIdStream() grpc.StreamServerInterceptor {
	return func(
		srv any,
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		return handler(srv, &ensureTraceIdServerStream{
			ServerStream: ss,
			ctx:          ensureTraceIdCtx(ss.Context()),
		})
	}
}

type ensureTraceIdServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

// Context returns the ctx whose incoming metadata carries the trace id, so
// common's stream interceptor — next in the chain — adopts it exactly as it
// adopts a client-supplied one (grpc.md T2).
func (s *ensureTraceIdServerStream) Context() context.Context {
	return s.ctx
}
