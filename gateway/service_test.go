package gateway

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestServerOverridesEveryRpc pins Server's claim that it overrides each of
// the 68 methods of service Gateway, so the embedded
// pb.UnimplementedGatewayServer never answers a request: a handler that is
// missing compiles and answers UNIMPLEMENTED. Each method's generated handler
// is called with an empty request under a context cancelled before the call,
// so no handler gets past its pure checks or its first read of etcd, and
// none may answer UNIMPLEMENTED; the store's revision shows that nothing was
// written.
func TestServerOverridesEveryRpc(t *testing.T) {
	srv := newTestServer(t)
	_, storeBefore, err := srv.cli.Range(context.Background(),
		common.DnvPrefix)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	methods := pb.Gateway_ServiceDesc.Methods
	if len(methods) != 68 {
		t.Fatalf("service Gateway has %d methods, want 68", len(methods))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	empty := func(any) error { return nil }
	for _, method := range methods {
		_, err := method.Handler(srv, ctx, empty, nil)
		if status.Code(err) == codes.Unimplemented {
			t.Errorf("%s answers UNIMPLEMENTED: %v", method.MethodName, err)
		}
	}
	_, storeAfter, err := srv.cli.Range(context.Background(),
		common.DnvPrefix)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if storeAfter != storeBefore {
		t.Errorf("the calls wrote: store revision %d, want %d", storeAfter,
			storeBefore)
	}
}
