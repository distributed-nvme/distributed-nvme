// Package agent holds the mechanism shared by the dn and cn agent roles
// (dnagent.md, Shared mechanism — package `agent`): bootstrap, the local
// store, the revision gate, the lock hierarchy, ResInfo tracking, the OS
// wrappers and the bitmap-chunk store.
// Policy — which dm tables, md arrays and nvmet objects to build and when —
// lives in the role packages agent/dnagent and agent/cnagent. No LVM runs
// anywhere in dnv ([D13], [D14]; cnagent.md, Scope and placement).
package agent

import (
	"context"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// Serve runs the shared agent lifecycle (SH1-SH3, SH33): prepare the role
// server with the process's ctx, listen and serve, then reconcile the OS to
// the local store while the agent answers that it is starting, and serve
// until ctx is canceled (the first SIGTERM or SIGINT, through signalContext
// and watchSignals in cmd/dnv-agent; a second signal exits 1 at once,
// skipping the join below, dnagent.md CM4, SH27). A listen that fails
// touches nothing of the node, so a second agent on the same gRPC address
// stops before its reconcile.
//
// prepare hands the role server the ctx its background work derives from
// (its rootCtx) and begins its startup state, before any RPC can arrive, so
// no RPC goroutine reads that ctx while the reconcile sets it.
//
// waitBackground (may be nil) is joined after the gRPC server has drained
// every RPC, so no background goroutine it covers — and, more to the point,
// no child process one of them owns, such as DN9's zeroing `blkdiscard` —
// outlives an agent that exits through it (dnagent.md SH27); an exit on a
// second signal skips it, and such a child can outlive the agent. Only a
// role whose background work holds a long-running child that must not
// outlive it passes one: the dn passes its WaitGroup join, the cn passes
// nil because its CN11 probers are stopped by cancellation and never joined
// (a wedged pread is
// uninterruptible, so waiting would hang shutdown forever — the very
// starvation the probe-IO carve-out exists to prevent) and the child of its
// sweep's background `nvme disconnect` holds nothing a restarted agent needs
// (cnagent.md CN10).
//
// The cancel-then-join pair is deferred, so *every* return path takes it. A
// return after reconcile, which has armed the background goroutines (and
// forked their children), is the one that needs it: on a signal the cancel
// is done already and the join remains, and a grpcServer.Serve that ends
// without a signal returns with runCtx still live, so it needs both;
// cancellation alone does not reap a child — the exec.CommandContext
// watchdog that turns it into SIGTERM and then SIGKILL (SH15, osclient.md,
// RunCommand) lives in this process and dies with it, so the child would be
// reparented to init still holding its dm device open. A failed listen
// returns before reconcile, and a failed reconcile from its listing of the
// local store, its first step (SH3), so neither has armed anything.
func Serve(
	ctx context.Context,
	network string,
	address string,
	prepare func(ctx context.Context),
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
	if prepare != nil {
		prepare(runCtx)
	}
	lis, err := net.Listen(network, address)
	if err != nil {
		return err
	}
	// The message limit and both server chains (grpc.md, Wiring).
	// The connection timeout bounds a handshake still pending when the stop
	// begins: grpc's stop waits for it before it ends any stream.
	grpcServer := grpc.NewServer(append(common.GrpcServerMsgOptions(),
		grpc.ConnectionTimeout(common.AgentHandshakeTimeout),
		grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
	)...)
	register(grpcServer)
	served := make(chan error, 1)
	go func() {
		served <- grpcServer.Serve(lis)
	}()
	// The stop runs once ctx is cancelled, or once grpc's Serve has ended
	// without a signal (a failed accept, or the hard stop of a reconcile
	// that failed), which leaves the connections and their handlers
	// running. Serve returns only after the stop has ended (stopDone):
	// grpc's Serve returns as soon as a hard stop begins, before the
	// handlers that stop cut have returned, and a handler cut mid-converge
	// must end, its commands killed (SH15), before the process does.
	serveEnded := make(chan struct{})
	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		select {
		case <-ctx.Done():
		case <-serveEnded:
		}
		agentStop(grpcServer, common.AgentStopGrace)
	}()
	finish := func(err error) error {
		close(serveEnded)
		<-stopDone
		return err
	}
	slog.InfoContext(startupCtx, "agent serving",
		slog.String("network", network),
		slog.String("address", address))
	start := time.Now()
	if err := reconcile(runCtx); err != nil {
		slog.ErrorContext(startupCtx, "agent reconcile failed",
			slog.String("error", err.Error()))
		grpcServer.Stop()
		<-served
		return finish(err)
	}
	slog.InfoContext(startupCtx, "agent started",
		slog.Int64("startup_ms", time.Since(start).Milliseconds()))
	// The stop has drained every RPC, or cut the ones past its grace and
	// waited for their handlers, by the time Serve returns (finish). A
	// signal has cancelled runCtx by then, since it derives from ctx; the
	// deferred cancel-and-join above cancels it on an error return and then
	// waits for the workers waitBackground covers, so no orphan child
	// process of theirs outlives the agent (SH27).
	return finish(<-served)
}

// agentStop is the stop Serve runs; a test replaces it to order the stop
// against Serve's return.
var agentStop = stopGracefully

// stopGracefully is the agent's stop: a graceful stop, which lets the RPCs
// in flight end, bounded by grace, past which the server stops at once,
// which cancels the handlers still running; it returns once every handler
// has returned, as the graceful stop it began waits for them. A
// worker's Check* stream never ends by itself, and the worker drops it only
// once the agent's answers, which no read refreshes any more, have aged past
// its bound, so an unbounded drain would keep a stopping agent answering
// stale rounds for several seconds before it exits (dnagent.md, Bootstrap
// and lifecycle).
func stopGracefully(grpcServer *grpc.Server, grace time.Duration) {
	stopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopped)
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-stopped:
	case <-timer.C:
		grpcServer.Stop()
		<-stopped
	}
}

// CheckRoundCtx is the ctx one Check* round runs under (SH24): the stream's,
// re-keyed to the trace id the round's request carries. The stream's own id is
// the one its metadata brought at open (grpc.md T2) — on a worker's stream,
// the id of the round that opened it — so without this every later round's
// records would be filed under that first round. An empty id keeps the
// stream's.
func CheckRoundCtx(streamCtx context.Context, traceId string) context.Context {
	if traceId == "" {
		return streamCtx
	}
	return common.WithTraceId(streamCtx, traceId)
}
