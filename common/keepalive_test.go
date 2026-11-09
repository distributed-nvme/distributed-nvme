package common

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The worker's keepalive (grpc.md, Wiring)
// ---------------------------------------------------------------------------

// silentConn is the client's end of a connection that can turn silent: once
// silenced, every write is swallowed and nothing more is read, as a machine
// that lost power with no TCP reset looks to its peer. Only a close ends it.
type silentConn struct {
	net.Conn
	silent atomic.Bool
}

func (c *silentConn) Write(b []byte) (int, error) {
	if c.silent.Load() {
		return len(b), nil
	}
	return c.Conn.Write(b)
}

func (c *silentConn) Read(b []byte) (int, error) {
	for {
		n, err := c.Conn.Read(b)
		if err != nil || !c.silent.Load() {
			return n, err
		}
		// Silent: what the peer still sends never arrives.
	}
}

// startSilenceableStub is startStubOpts over a connection the test can
// silence. It returns the client and a function that silences the most
// recent connection.
func startSilenceableStub(
	t *testing.T,
	stub *stubDnAgent,
	serverOpts []grpc.ServerOption,
	dialOpts []grpc.DialOption,
) (pb.DiskNodeAgentClient, func()) {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer(serverOpts...)
	pb.RegisterDiskNodeAgentServer(server, stub)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	var mu sync.Mutex
	var last *silentConn
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		append(dialOpts,
			grpc.WithContextDialer(func(ctx context.Context, _ string) (
				net.Conn, error,
			) {
				raw, err := lis.DialContext(ctx)
				if err != nil {
					return nil, err
				}
				c := &silentConn{Conn: raw}
				mu.Lock()
				last = c
				mu.Unlock()
				return c, nil
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)...,
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	silence := func() {
		mu.Lock()
		defer mu.Unlock()
		if last != nil {
			last.silent.Store(true)
		}
	}
	return pb.NewDiskNodeAgentClient(conn), silence
}

// TestKeepaliveFailsACallToASilentPeer pins the worker's keepalive: a call
// whose peer stops answering mid-call, with no reset, fails Unavailable
// within GrpcKeepaliveTime plus GrpcKeepaliveTimeout, long before the call's
// own deadline, so a syncup to a dead machine frees its object's rounds
// (dnv-worker.md RW7, HL4).
func TestKeepaliveFailsACallToASilentPeer(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	var once sync.Once
	stub := &stubDnAgent{
		getDnInfoFn: func(ctx context.Context, _ *pb.GetDnInfoRequest) (
			*pb.GetDnInfoReply, error,
		) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	client, silence := startSilenceableStub(t, stub, GrpcServerMsgOptions(),
		append(GrpcKeepaliveDialOptions(), GrpcClientMsgOption()))
	go func() {
		<-started
		silence()
	}()
	deadline := 3 * (GrpcKeepaliveTime + GrpcKeepaliveTimeout) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	begin := time.Now()
	_, err := client.GetDnInfo(ctx, &pb.GetDnInfoRequest{})
	took := time.Since(begin)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("call to a silent peer ended with %v after %v, want "+
			"Unavailable from the keepalive", err, took)
	}
	bound := (GrpcKeepaliveTime + GrpcKeepaliveTimeout + 5) * time.Second
	if took > bound {
		t.Fatalf("call to a silent peer took %v, want under %v", took, bound)
	}
}

// TestKeepaliveSparesALongCall pins the server half: a call that a live peer
// answers only after several keepalive pings keeps its connection, because
// every dnv server lets a client ping every GrpcKeepaliveMinTime. A server at
// gRPC's default would end the connection "too_many_pings" after the fourth
// ping of one call.
func TestKeepaliveSparesALongCall(t *testing.T) {
	if testing.Short() {
		t.Skip("takes over 40 s of keepalive pings")
	}
	t.Parallel()
	hold := 4*GrpcKeepaliveTime*time.Second + 5*time.Second
	stub := &stubDnAgent{
		getDnInfoFn: func(ctx context.Context, _ *pb.GetDnInfoRequest) (
			*pb.GetDnInfoReply, error,
		) {
			select {
			case <-time.After(hold):
				return &pb.GetDnInfoReply{}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	client, _ := startSilenceableStub(t, stub, GrpcServerMsgOptions(),
		append(GrpcKeepaliveDialOptions(), GrpcClientMsgOption()))
	ctx, cancel := context.WithTimeout(context.Background(), 2*hold)
	defer cancel()
	if _, err := client.GetDnInfo(ctx, &pb.GetDnInfoRequest{}); err != nil {
		t.Fatalf("a long call to a live peer failed: %v", err)
	}
}

// TestConnectBoundFailsACallToAMachineThatAcceptsAndSaysNothing pins the
// connect bound: a machine whose kernel takes the TCP connection while its
// agent never answers, as a stopped agent's machine does, fails a call within
// GrpcConnectTimeout, not at the call's own deadline.
func TestConnectBoundFailsACallToAMachineThatAcceptsAndSaysNothing(t *testing.T) {
	t.Parallel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	var held sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			held.Lock()
			conns = append(conns, conn)
			held.Unlock()
		}
	}()
	t.Cleanup(func() {
		held.Lock()
		defer held.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	conn, err := grpc.NewClient(lis.Addr().String(),
		append(GrpcKeepaliveDialOptions(),
			grpc.WithTransportCredentials(insecure.NewCredentials()))...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := pb.NewDiskNodeAgentClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(),
		4*GrpcConnectTimeout*time.Second)
	defer cancel()
	begin := time.Now()
	_, err = client.GetDnInfo(ctx, &pb.GetDnInfoRequest{})
	took := time.Since(begin)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("call ended with %v after %v, want Unavailable from the "+
			"connect bound", err, took)
	}
	if bound := (GrpcConnectTimeout + 3) * time.Second; took > bound {
		t.Fatalf("call took %v, want under %v", took, bound)
	}
}
