// CT-T5 — trace ids (dnvctl.md §6, CT2/§2.3).
//
// §2.3 says dnvctl builds its ctx as common.WithTraceId(…, id) — `--trace-id`
// when non-empty, else common.NewTraceId() — and that the grpc.md §4 CLIENT
// chain is what moves that id into the outgoing `trace_id` metadata, because
// "dnvctl never touches metadata directly" (the AppendToOutgoingContext
// shortcut is the integtest drivers' carve-out, §6 of grpc.md, and does not
// apply here).
//
// That makes the ctx a useless place to assert: an id in dnvctl's own ctx
// proves only that dnvctl put it there. The proof has to be taken at the FAR
// END, out of the server's incoming metadata, because only a value that got
// there travelled the whole path — run's ctx → attachTraceId → the wire →
// the server chain. So these tests run a real gRPC server over bufconn behind
// the §4 server chain and read what arrives.
//
// The dial seam is replaced rather than bypassed, but with the SAME option
// block ctl/root.go's dial uses, plus bufconn's contextDialer — the
// interceptor chain under test is common.GrpcUnaryClientInterceptor() itself,
// not a stand-in. TestProductionDialCarriesTraceId then closes the remaining
// gap by exercising the untouched production dial against a loopback
// listener, so a change to root.go's own option block cannot pass unnoticed.
package ctl

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// traceServer answers ListClusters and records the trace id metadata of every
// call that reaches it.
type traceServer struct {
	pb.UnimplementedGatewayServer

	// hang, when non-nil, blocks every call until it is closed or the call's
	// context dies — how §7.13's d2 manufactures a DEADLINE_EXCEEDED. It is
	// set before the server starts serving, so the handler goroutine never
	// races the test over it.
	hang chan struct{}

	mu  sync.Mutex
	ids []string
}

func (s *traceServer) ListClusters(
	ctx context.Context, _ *pb.ListClustersRequest,
) (*pb.ListClustersReply, error) {
	id := ""
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		// Exactly one value: a second would mean the id was attached twice,
		// once by the §4 chain and once by a metadata shortcut dnvctl is not
		// allowed to take.
		if values := md.Get(common.TraceIdMetadataKey); len(values) == 1 {
			id = values[0]
		} else if len(values) > 1 {
			id = "MULTIPLE:" + strings.Join(values, ",")
		}
	}
	s.mu.Lock()
	s.ids = append(s.ids, id)
	s.mu.Unlock()
	if s.hang != nil {
		select {
		case <-s.hang:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &pb.ListClustersReply{}, nil
}

func (s *traceServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ids...)
}

// serveTrace starts one gateway on bufconn behind the grpc.md §4 SERVER
// chain — both chains, as §4 requires on every dnv connection — and returns
// the server plus a dial seam that reaches it through the §4 CLIENT chain.
func serveTrace(t *testing.T) (*traceServer, dialFunc) {
	t.Helper()
	srv := &traceServer{}
	return srv, serveTraceWith(t, srv)
}

// serveTraceWith is serveTrace for a server the caller configured first.
func serveTraceWith(t *testing.T, srv *traceServer) dialFunc {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
	)
	pb.RegisterGatewayServer(server, srv)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(func() {
		server.Stop()
		_ = lis.Close()
	})

	return func(_ context.Context, _ string) (
		pb.GatewayClient, func() error, error,
	) {
		conn, err := grpc.NewClient(
			"passthrough:///bufnet",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (
				net.Conn, error,
			) {
				return lis.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithChainUnaryInterceptor(
				common.GrpcUnaryClientInterceptor()),
			grpc.WithChainStreamInterceptor(
				common.GrpcStreamClientInterceptor()),
		)
		if err != nil {
			return nil, nil, err
		}
		return pb.NewGatewayClient(conn), conn.Close, nil
	}
}

// quietLogging installs the logger cmd/dnvctl/main.go installs — level Warn,
// nothing on stdout — over a buffer the test can inspect, and restores the
// process default afterwards. Every §4 interceptor record is Info, so the
// buffer staying empty is log.md's "dnvctl's own gRPC logging is silenced by
// design" (CT7), asserted rather than assumed.
func quietLogging(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(&common.TraceIdHandler{
		Handler: slog.NewJSONHandler(
			&buf, &slog.HandlerOptions{Level: slog.LevelWarn}),
	}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestTraceIdReachesTheServer is CT-T5 proper: the explicit id arrives
// verbatim, an omitted one arrives as a non-empty mint, and two invocations
// mint two DIFFERENT ids.
func TestTraceIdReachesTheServer(t *testing.T) {
	t.Run("explicit", func(t *testing.T) {
		srv, seam := serveTrace(t)
		logs := quietLogging(t)

		res := runCLIWithDial(t, seam, globalArgv(
			"--trace-id", "it-smoke-1", "cluster", "list")...)
		if res.code != 0 {
			t.Fatalf("exited %d, stderr %q", res.code, res.stderr)
		}
		seen := srv.seen()
		if len(seen) != 1 {
			t.Fatalf("the server saw %d calls, want 1", len(seen))
		}
		if seen[0] != "it-smoke-1" {
			t.Errorf("the server saw trace_id %q, want it-smoke-1", seen[0])
		}

		// The whole §3.1 contract still holds over a real connection: one
		// document on stdout, nothing on stderr, and the interceptors'
		// Info records silenced (CT7).
		want := `{"cluster_name":[],"page_token":""}` + "\n"
		if res.stdout != want {
			t.Errorf("stdout\n got: %q\nwant: %q", res.stdout, want)
		}
		if res.stderr != "" {
			t.Errorf("stderr = %q, want empty", res.stderr)
		}
		if logs.Len() != 0 {
			t.Errorf("the §4 client chain logged %q at Warn, want silence",
				logs.String())
		}
	})

	t.Run("minted", func(t *testing.T) {
		srv, seam := serveTrace(t)
		quietLogging(t)

		// Two invocations, neither naming an id.
		for i := 0; i < 2; i++ {
			res := runCLIWithDial(t, seam, globalArgv("cluster", "list")...)
			if res.code != 0 {
				t.Fatalf("run %d exited %d, stderr %q", i, res.code,
					res.stderr)
			}
		}
		seen := srv.seen()
		if len(seen) != 2 {
			t.Fatalf("the server saw %d calls, want 2", len(seen))
		}
		for i, id := range seen {
			if id == "" {
				t.Errorf("run %d arrived with no trace_id: §2.3 mints one "+
					"per invocation", i)
			}
			if !mintedTraceId.MatchString(id) {
				t.Errorf("run %d arrived with trace_id %q, which is not "+
					"common.NewTraceId's shape", i, id)
			}
		}
		if seen[0] == seen[1] {
			t.Errorf("two invocations minted the same trace_id %q: the mint "+
				"is per invocation, not per process", seen[0])
		}
	})

	// An EMPTY --trace-id is the same as not passing it (run trims and falls
	// back to the mint), rather than sending an empty metadata value.
	t.Run("empty flag mints", func(t *testing.T) {
		srv, seam := serveTrace(t)
		quietLogging(t)

		res := runCLIWithDial(t, seam,
			globalArgv("--trace-id", "", "cluster", "list")...)
		if res.code != 0 {
			t.Fatalf("exited %d, stderr %q", res.code, res.stderr)
		}
		seen := srv.seen()
		if len(seen) != 1 || !mintedTraceId.MatchString(seen[0]) {
			t.Errorf("the server saw %v, want one minted id", seen)
		}
	})
}

// TestProductionDialCarriesTraceId runs the SAME proof through ctl/root.go's
// own dial function, untouched, over a loopback listener. serveTrace's seam
// is a faithful copy of that option block, but a copy is exactly the thing
// that drifts: if root.go ever loses grpc.WithChainUnaryInterceptor, the
// bufconn tests above keep passing and this one stops.
func TestProductionDialCarriesTraceId(t *testing.T) {
	srv := &traceServer{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener available: %v", err)
	}
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
	)
	pb.RegisterGatewayServer(server, srv)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(func() {
		server.Stop()
		_ = lis.Close()
	})
	quietLogging(t)

	res := runCLIWithDial(t, dial,
		"--gateway-address", lis.Addr().String(),
		"--trace-id", "it-smoke-2",
		"--cluster", itCluster, "--sp", itSp,
		"cluster", "list")
	if res.code != 0 {
		t.Fatalf("exited %d, stderr %q", res.code, res.stderr)
	}
	if seen := srv.seen(); len(seen) != 1 || seen[0] != "it-smoke-2" {
		t.Errorf("the server saw %v, want [it-smoke-2]", seen)
	}
}
