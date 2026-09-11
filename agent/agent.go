// Package agent holds the mechanism shared by the dn and cn agent roles
// (dnagent.md §2): bootstrap, the local store, the revision gate, the lock
// hierarchy, ResInfo tracking, the OS wrappers and the bitmap-chunk store.
// Policy — which dm tables, md arrays and nvmet objects to build and when —
// lives in the role packages agent/dnagent and agent/cnagent. No LVs: [D14]
// removed the clone VG, LVM's last user, so no LVM runs anywhere in dnv
// (cnagent.md §1).
package agent

import (
	"context"
	"log/slog"
	"net"

	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// Serve runs the shared agent lifecycle (SH1-SH3): reconcile the OS to the
// local store, then listen and serve until ctx is canceled (SIGTERM/SIGINT
// via signal.NotifyContext in cmd/dnv-agent).
//
// waitBackground (may be nil) is joined after GracefulStop has drained every
// RPC, so no background goroutine — and, more to the point, no child process
// one of them owns, such as the §9.4 zeroing `blkdiscard` — outlives the
// agent (dnagent.md SH27). Only a role whose background work holds a
// long-running child passes one: the dn passes its WaitGroup join, the cn
// passes nil because its CN11 probers are stopped by cancellation and never
// joined (a wedged pread is uninterruptible, so waiting would hang shutdown
// forever — the very starvation the probe-IO carve-out exists to prevent).
//
// The cancel-then-join pair is deferred, so *every* return path takes it, not
// just the one through grpcServer.Serve: reconcile has already armed the
// background goroutines (and forked their children) by the time a reconcile
// error or a net.Listen error returns, and cancellation alone does not reap a
// child — the exec.CommandContext watchdog that turns it into SIGTERM and then
// SIGKILL (SH15, osclient.md §4.2) lives in this process and dies with it, so
// the child would be reparented to init still holding its dm device open.
func Serve(
	ctx context.Context,
	network string,
	address string,
	reconcile func(ctx context.Context) error,
	register func(grpcServer *grpc.Server),
	waitBackground func(),
) error {
	startupCtx := common.WithTraceId(ctx, common.NewTraceId())
	// The role servers capture this ctx as their rootCtx, so cancelling it is
	// what stops every background goroutine. It is derived from startupCtx
	// and cancelled unconditionally — not only on SIGTERM — so a reconcile,
	// listener or serve error also winds the background down and the join
	// paired with it can never hang.
	runCtx, cancelRun := context.WithCancel(startupCtx)
	// Cancel first, then join — in that order, on every return path. The
	// background goroutines only unwind on cancellation, so joining first
	// would hang shutdown for ever.
	defer func() {
		cancelRun()
		if waitBackground != nil {
			waitBackground()
		}
	}()
	if err := reconcile(runCtx); err != nil {
		slog.ErrorContext(startupCtx, "agent reconcile failed",
			slog.String("error", err.Error()))
		return err
	}
	lis, err := net.Listen(network, address)
	if err != nil {
		return err
	}
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
	)
	register(grpcServer)
	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()
	slog.InfoContext(startupCtx, "agent serving",
		slog.String("network", network),
		slog.String("address", address))
	// GracefulStop has drained every RPC by the time Serve returns; the
	// deferred cancel-and-join above then stops the background workers and
	// waits for them, so no orphan child process outlives the agent (§9.4).
	return grpcServer.Serve(lis)
}
