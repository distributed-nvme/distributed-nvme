package etcdutil

import (
	"context"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// aboveClientDefault is above clientv3's default send limit of 2 MiB, and so
// above etcd's default request cap of 1.5 MiB too, and far under
// EtcdMaxRequestBytes.
const aboveClientDefault = 3 << 20

// TestPutAboveTheClientDefault: a put above clientv3's default send limit
// lands and reads back, because New sets the send limit to
// EtcdMaxRequestBytes (EU1) and the test etcd runs with --max-request-bytes
// at it, as every etcd serving dnv must (dnv-worker.md, Constants this
// document owns). A client at clientv3's default refuses the same put, which
// is what shows the size really is above that default.
func TestPutAboveTheClientDefault(t *testing.T) {
	if aboveClientDefault+aboveClientDefault/8 > common.EtcdMaxRequestBytes {
		t.Fatalf("a %d-byte value no longer fits EtcdMaxRequestBytes (%d) with "+
			"room for the request around it", aboveClientDefault,
			common.EtcdMaxRequestBytes)
	}
	cli := newTestClient(t)
	ctx := context.Background()
	key := testPrefix(t) + "big"

	if err := cli.Put(ctx, key, &pb.CloneBitmap{
		Bitmap: make([]byte, aboveClientDefault),
	}); err != nil {
		t.Fatalf("Put of a %d-byte value: %v", aboveClientDefault, err)
	}
	got := &pb.CloneBitmap{}
	found, err := cli.Get(ctx, key, got)
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if len(got.GetBitmap()) != aboveClientDefault {
		t.Fatalf("read back %d bytes, want %d", len(got.GetBitmap()),
			aboveClientDefault)
	}
	if err := cli.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	stock, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{testEndpoint},
		DialTimeout: time.Second,
		Logger:      zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("a client at clientv3's defaults: %v", err)
	}
	defer stock.Close()
	_, err = stock.Put(ctx, testPrefix(t)+"stock",
		string(make([]byte, aboveClientDefault)))
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("a client at clientv3's default send limit put %d bytes: "+
			"err = %v, want RESOURCE_EXHAUSTED; the size no longer passes "+
			"that default, so the put above proves nothing",
			aboveClientDefault, err)
	}
}
