package etcdutil

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	etcdpb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// laggingMember is a client interceptor that answers the first n reads
// pinned to a revision — every read of a serializable-snapshot transaction
// after its first — as an etcd member that has not applied that revision
// yet: "required revision is a future revision".
func laggingMember(n int32) (grpc.UnaryClientInterceptor, *atomic.Int32) {
	var left atomic.Int32
	left.Store(n)
	return func(
		ctx context.Context, method string, req, reply any,
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		if txn, ok := req.(*etcdpb.TxnRequest); ok && pinnedRead(txn) &&
			left.Add(-1) >= 0 {
			return rpctypes.ErrGRPCFutureRev
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}, &left
}

// pinnedRead reports whether a txn reads at a fixed revision.
func pinnedRead(txn *etcdpb.TxnRequest) bool {
	for _, op := range txn.GetSuccess() {
		if r := op.GetRequestRange(); r != nil && r.GetRevision() > 0 {
			return true
		}
	}
	return false
}

// laggingClient is a client whose first n reads at a revision the
// interceptor answers as a lagging member would; the counter it returns goes
// below zero once all n were answered so.
func laggingClient(t *testing.T, n int32) (*Client, *atomic.Int32) {
	t.Helper()
	icpt, left := laggingMember(n)
	tmo := prodTimeouts()
	tmo.configure = func(cfg *clientv3.Config) {
		cfg.DialOptions = append(cfg.DialOptions,
			grpc.WithChainUnaryInterceptor(icpt))
	}
	cli, err := newClient(context.Background(), []string{testEndpoint},
		time.Second, tmo)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli, left
}

// TestRunSTMRetriesALaggingMember pins EU4's retry: a read of a read-write
// transaction that an etcd member answers as a future revision runs the
// transaction again within its budget, and the write commits once.
func TestRunSTMRetriesALaggingMember(t *testing.T) {
	cli, left := laggingClient(t, 1)
	ctx := context.Background()
	a, b := testPrefix(t)+"a", testPrefix(t)+"b"
	if err := cli.Put(ctx, a, &pb.WorkerReg{Epoch: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	runs := 0
	err := cli.RunSTM(ctx, func(s STM) error {
		runs++
		reg := &pb.WorkerReg{}
		s.Get(a, reg)
		s.Get(b, &pb.WorkerReg{}) // the read pinned to the first's revision
		s.Put(b, &pb.WorkerReg{Epoch: reg.GetEpoch() + 1})
		return nil
	})
	if err != nil {
		t.Fatalf("RunSTM over a lagging member: %v", err)
	}
	if runs != 2 {
		t.Errorf("the transaction ran %d times, want 2", runs)
	}
	if left.Load() >= 0 {
		t.Fatalf("no read was answered as a lagging member's")
	}
	got := &pb.WorkerReg{}
	if ok, err := cli.Get(ctx, b, got); err != nil || !ok ||
		got.GetEpoch() != 2 {
		t.Errorf("b = %v (found %v, %v), want epoch 2", got, ok, err)
	}
	raw, err := cli.cli.Get(ctx, b)
	if err != nil || len(raw.Kvs) != 1 || raw.Kvs[0].Version != 1 {
		t.Errorf("b written %v times (%v), want once", raw, err)
	}
}

// TestRunSTMKeepsTheCallbacksErrorAfterARetry pins EU4 across the retry: a
// transaction run again after a lagging member's answer that then ends on its
// callback's own error returns that error unchanged, so errors.Is on it
// works at the call site.
func TestRunSTMKeepsTheCallbacksErrorAfterARetry(t *testing.T) {
	cli, left := laggingClient(t, 1)
	ctx := context.Background()
	a, b := testPrefix(t)+"a", testPrefix(t)+"b"
	if err := cli.Put(ctx, a, &pb.WorkerReg{Epoch: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	refusal := errors.New("refused by the callback")
	runs := 0
	err := cli.RunSTM(ctx, func(s STM) error {
		runs++
		s.Get(a, &pb.WorkerReg{})
		s.Get(b, &pb.WorkerReg{})
		return refusal
	})
	if err != refusal {
		t.Fatalf("err = %v, want the callback's own error unchanged", err)
	}
	if runs != 2 || left.Load() >= 0 {
		t.Errorf("runs = %d, lagging answers left %d: want one retry",
			runs, left.Load())
	}
}

// TestSnapshotRetriesALaggingMember is the same for a read-only transaction,
// over three such answers in a row.
func TestSnapshotRetriesALaggingMember(t *testing.T) {
	cli, left := laggingClient(t, 3)
	ctx := context.Background()
	a, b := testPrefix(t)+"a", testPrefix(t)+"b"
	for _, key := range []string{a, b} {
		if err := cli.Put(ctx, key, &pb.WorkerReg{Epoch: 5}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	var got uint64
	err := cli.Snapshot(ctx, func(s STM) error {
		ra, rb := &pb.WorkerReg{}, &pb.WorkerReg{}
		s.Get(a, ra)
		s.Get(b, rb)
		got = ra.GetEpoch() + rb.GetEpoch()
		return nil
	})
	if err != nil {
		t.Fatalf("Snapshot over a lagging member: %v", err)
	}
	if got != 10 {
		t.Errorf("read %d, want 10", got)
	}
	if left.Load() >= 0 {
		t.Errorf("only %d of 3 reads were answered as a lagging member's",
			3-left.Load())
	}
}
