// Package gateway is dnv-gateway's control-plane API server (gateway.md): the
// 59 RPCs of `service Gateway`, implemented directly on etcd through
// etcdutil's STM machinery, plus the ten agent calls of §6 that reach a dn or
// cn agent for a size, an *Info or a bitmap.
//
// It is stateless and active-active (§0 #3): no leader election, no
// registration key, no shard split and no instance-count limit. Any instance
// serves any request; multi-instance correctness rests entirely on
// etcdutil.RunSTM's serializable-snapshot isolation plus the §5.5 revision
// tokens. A gateway writes nothing to etcd that describes itself.
//
// It imports only common, pb, etcdutil, model and the grpc runtime
// (layout.md §3): it is a gRPC server AND a client, and cobra/viper live in
// cmd/ only.
package gateway

import (
	"context"
	"log/slog"
	"net"

	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The normative msg strings of gateway.md §8 (LG2). The integration suite
// greps them, so they are constants and never formatted.
const (
	msgGatewayStarting = "gateway starting"
	msgGatewayServing  = "gateway serving"
	msgGatewayStopping = "gateway stopping"
)

// Config is everything cmd/dnv-gateway passes to Run (GW2, CM3).
type Config struct {
	// GrpcNetwork and GrpcAddress are the listener, exactly as in
	// cmd/dnv-agent: --grpc-network defaults to "tcp", --grpc-address is
	// required and has no default port (§0 #10).
	GrpcNetwork string
	GrpcAddress string
	// Endpoints are the etcd endpoints the client was built with. The
	// gateway never dials them itself; they exist for the "gateway starting"
	// record (LG2).
	Endpoints []string
}

// Server implements pb.GatewayServer (GW1). It holds exactly one piece of
// state, the etcd client: handlers keep nothing in process, take no locks and
// impose no concurrency limit — all coordination is etcd's, so two requests
// racing inside one instance and two racing across two instances are the same
// case by construction.
//
// pb.UnimplementedGatewayServer is embedded because the generated code
// requires it (mustEmbedUnimplementedGatewayServer); it is an empty struct and
// carries no state. Every one of the 59 methods is overridden below, so it
// never actually answers a request.
type Server struct {
	pb.UnimplementedGatewayServer

	cli *etcdutil.Client
}

// NewServer builds a Server over an existing etcd client. Run uses it; the
// unit tests of §9 use it to drive handlers without a listener.
func NewServer(cli *etcdutil.Client) *Server {
	return &Server{cli: cli}
}

// Run serves the Gateway service until ctx is canceled (GW2, GW3).
//
// It mirrors agent/agent.go Serve: one listener, one grpc.Server carrying both
// interceptor chains of grpc.md §4, a goroutine that turns ctx cancellation
// into GracefulStop, and Serve. Shutdown is GracefulStop and nothing else:
// in-flight handlers finish (each bounded by its client deadline and the 10 s
// per-STM budget of EU5), new requests are refused, and there is nothing to
// drain — no registry key to delete, no background loop to stop (§0 #3).
//
// Run closes cli as the last step of its drain, so cmd/dnv-gateway does not
// (CM3). Startup never fails because etcd is unreachable: etcdutil.New dials
// lazily, so only a configuration error — a listener that cannot be opened —
// fails before serving.
func Run(ctx context.Context, cli *etcdutil.Client, cfg Config) error {
	slog.InfoContext(ctx, msgGatewayStarting,
		slog.String("grpc_network", cfg.GrpcNetwork),
		slog.String("grpc_address", cfg.GrpcAddress),
		slog.Any("etcd_endpoints", cfg.Endpoints),
	)
	defer func() {
		// Closed here and not in main (CM3): Run owns the client for its
		// whole lifetime, and the last handler has returned by the time
		// GracefulStop lets Serve return.
		if err := cli.Close(); err != nil {
			slog.WarnContext(ctx, "etcd client close failed",
				slog.String("error", err.Error()))
		}
		slog.InfoContext(ctx, msgGatewayStopping)
	}()

	lis, err := net.Listen(cfg.GrpcNetwork, cfg.GrpcAddress)
	if err != nil {
		return err
	}
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
	)
	pb.RegisterGatewayServer(grpcServer, NewServer(cli))
	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()
	slog.InfoContext(ctx, msgGatewayServing,
		slog.String("network", cfg.GrpcNetwork),
		slog.String("address", cfg.GrpcAddress),
	)
	return grpcServer.Serve(lis)
}
