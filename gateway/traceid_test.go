package gateway

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is gateway/traceid.go's test: the gateway mints a trace id for a
// request that arrives without one, and that id is the one its outbound agent
// call carries.
//
// The proof point is the AGENT's incoming metadata, not a log line and not
// the handler's ctx: the minted id has to survive the whole path — incoming
// metadata → common's server interceptor (T2) → the handler ctx → common's
// client interceptor (T1/T3) → the wire — and only the far end sees all of
// it. §9.4's fake agent already records exactly that (agentpath_test.go
// captureInterceptor / seenTraceIds), so the two halves of the trace-id
// contract are asserted by the same recorder.

// traceIdFixture is one gateway served on bufconn with the production chain,
// a raw client for it, and one fake agent registered as the cluster's only
// DN — everything TestServerMintsTraceIdWhenAbsent and its `provided` subtest
// each need their own copy of, because both assert on an exact call count.
type traceIdFixture struct {
	client      pb.GatewayClient
	fake        *agentpathFake
	clusterName string
}

// traceIdSetup builds one fixture.
//
// The DN is written straight to etcd rather than created through
// CreateDiskNode because that handler makes an agent call of its own
// (GetDnSize, §6.1): the fake would then hold two trace ids and "exactly one"
// would stop meaning "the one InspectDiskNode forwarded". The cluster is
// seeded the same way and for the same reason.
//
// The client carries NO interceptors, which is the experiment: common's
// client chain is precisely what would supply a trace id (T1), so an id
// arriving at the fake can only have been minted by the server chain.
func traceIdSetup(t *testing.T) *traceIdFixture {
	t.Helper()
	srv, cli := agentpathServer(t)
	fake := agentpathStartAgent(t)
	fake.setInfos(&pb.DnInfo{
		DiskInfo: &pb.ResInfo{
			ResName: "disk",
			Status:  pb.ResStatus_RES_STATUS_OK,
			Details: "traceid disk",
			Epoch:   1,
		},
	}, nil, 11)
	clusterName, cid := agentpathSeedCluster(t, cli)
	agentpathPut(t, cli, model.DnConfKey(cid, fake.addrPort), &pb.DnConf{
		DnId:        1,
		ShardCode:   3,
		NvmeTrConf:  agentpathTrConf(fake.addrPort),
		Location:    fake.addrPort,
		TotalExtCnt: 4,
		FreeExtCnt:  4,
	})
	dialer := servingBufconnServe(t, srv)
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// LIFO with servingBufconnServe's Stop: the conn goes first.
	t.Cleanup(func() {
		_ = conn.Close()
	})
	return &traceIdFixture{
		client:      pb.NewGatewayClient(conn),
		fake:        fake,
		clusterName: clusterName,
	}
}

// inspect makes the one served call that reaches the fake agent:
// InspectDiskNode is a handler whose whole body is a snapshot read plus one
// GetDnInfo (disknode.go §8.2), so one client call is one agent call.
func (fx *traceIdFixture) inspect(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, err := fx.client.InspectDiskNode(
		ctx, &pb.InspectDiskNodeRequest{
			ClusterName: fx.clusterName,
			AddrPort:    fx.fake.addrPort,
		}); err != nil {
		t.Fatalf("InspectDiskNode: %v", err)
	}
}

// traceIdIsMinted reports whether s is shaped like common.NewTraceId's
// output: 8 random bytes hex-encoded, so 16 lowercase hex characters. The
// shape is asserted rather than the value because the value is random by
// construction; what matters is that it is a real id and not "", a placeholder
// or the caller's own name.
func traceIdIsMinted(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// TestServerMintsTraceIdWhenAbsent pins gateway.md §0 #6: a request that
// arrives with no trace_id metadata still gets one, and the agent call the
// handler makes carries it. Without the mint the fake would see no trace-id
// metadata at all here, because the shared server interceptor only adopts an
// incoming id and there would be none to adopt.
func TestServerMintsTraceIdWhenAbsent(t *testing.T) {
	fx := traceIdSetup(t)

	fx.inspect(t, context.Background())

	seen := fx.fake.seenTraceIds()
	if len(seen) != 1 {
		t.Fatalf("%s values seen = %v, want exactly one (the minted id)",
			common.TraceIdMetadataKey, seen)
	}
	if !traceIdIsMinted(seen[0]) {
		t.Errorf("minted %s = %q, want 16 lowercase hex characters",
			common.TraceIdMetadataKey, seen[0])
	}

	// The other half of the contract: the mint is for a request that arrived
	// WITHOUT an id and must never overwrite one that arrived with it, or a
	// caller's trace would break at the gateway — the one hop that is
	// supposed to join it to the agents.
	t.Run("provided", func(t *testing.T) {
		const provided = "cafe0123deadbeef"
		fx := traceIdSetup(t)

		fx.inspect(t, metadata.AppendToOutgoingContext(
			context.Background(), common.TraceIdMetadataKey, provided))

		seen := fx.fake.seenTraceIds()
		if len(seen) != 1 {
			t.Fatalf("%s values seen = %v, want exactly one",
				common.TraceIdMetadataKey, seen)
		}
		if seen[0] != provided {
			t.Errorf("%s = %q, want the caller's %q",
				common.TraceIdMetadataKey, seen[0], provided)
		}
	})
}
