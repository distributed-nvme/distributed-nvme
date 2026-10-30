// Command fakeagent is the fake dn/cn agent pair of the dnv-worker
// integration suite (dnv-worker.md, Integration test plan, The fake
// agent): the behavior-file front of the integtest/agentfake library, served
// over TCP. One binary with a `dn` and a `cn` subcommand, serving the
// generated DiskNodeAgent resp. ControllerNodeAgent service on a plaintext
// listener behind the real server interceptors (grpc.md, Wiring), so
// `agent.log` carries one `grpc server request`/`reply`/`recv`/`send` record
// per message with the caller's trace id — the suite's evidence of what the
// worker sent and which worker sent it (RW10). The JSON log goes to stderr
// through common's default logger; the script redirects it into `agent.log`.
//
// Two files in --dir drive and record the fake:
//
//	behavior.json  the levers, in the format agentfake.ParseBehavior parses.
//	               Re-read at the arrival of every request when its mtime or
//	               size changed, and every pollInterval, so the script flips
//	               behaviour without restarting the fake and a request the
//	               file holds sees the flip with no other request. Absent (or
//	               `{}`) means: sides instantly zeroed and every row OK, save
//	               the rows the fake derives from the request. A malformed
//	               file is logged and ignored, keeping the previous
//	               behaviour, so a bad file fails a test on its assertion
//	               instead of killing the agent.
//	state.json     the stored state (agentfake.StateFileName): the last
//	               accepted request and revision per object plus the received
//	               bitmap chunks, written on every change and loaded at
//	               start, so a killed and restarted fake replies like a
//	               restarted agent; a hand edit while the fake runs — a
//	               stored revision set by hand, the store emptied — is seen
//	               by the next request.
//
//	usage: fakeagent dn|cn --grpc-address <ip:port> --dir <dir> [--size N]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/integtest/agentfake"
)

const (
	// behaviorFileName is the behavior file of the per-agent directory,
	// next to agent.log and the state file.
	behaviorFileName = "behavior.json"

	// pollInterval is how often the behavior file is re-read, besides at
	// the arrival of every request, so that a request a lever holds sees
	// the script's rewrite with no other request.
	pollInterval = 200 * time.Millisecond

	usageText = "usage: fakeagent dn|cn --grpc-address <ip:port> --dir <dir> " +
		"[--size N]"
)

// ---------------------------------------------------------------------------
// The command line
// ---------------------------------------------------------------------------

// options are the parsed command line.
type options struct {
	mode string
	kind agentfake.Kind
	addr string
	dir  string
	size uint64
}

// exitError is a command line the program refuses, with the status it exits
// with and the text it prints first, if any.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// parseArgs parses the arguments after the program name. A missing or
// unknown subcommand is a usage error (status 2), as is a flag the flag
// package refuses, which prints its own diagnostics to output; -h exits 0;
// a missing required flag exits 1.
func parseArgs(args []string, output io.Writer) (options, error) {
	if len(args) < 1 || (args[0] != "dn" && args[0] != "cn") {
		return options{}, &exitError{code: 2, msg: usageText}
	}
	mode := args[0]
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	fs.SetOutput(output)
	addr := fs.String("grpc-address", "",
		"listen address ip:port (required)")
	dir := fs.String("dir", "",
		"directory holding behavior.json and state.json (required)")
	size := fs.Uint64("size", agentfake.DefaultNodeSize,
		"the GetDnSize/GetCnSize reply, unless behavior.json sets \"size\"")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return options{}, &exitError{code: 0}
		}
		return options{}, &exitError{code: 2}
	}
	if *addr == "" {
		return options{}, &exitError{
			code: 1, msg: "fakeagent: --grpc-address is required"}
	}
	if *dir == "" {
		return options{}, &exitError{
			code: 1, msg: "fakeagent: --dir is required"}
	}
	kind := agentfake.Dn
	if mode == "cn" {
		kind = agentfake.Cn
	}
	return options{mode: mode, kind: kind, addr: *addr, dir: *dir,
		size: *size}, nil
}

// ---------------------------------------------------------------------------
// The behavior file front (dnv-worker.md, Integration test plan, The fake
// agent)
// ---------------------------------------------------------------------------

// behaviorFront keeps the agent's levers equal to the behavior file: refresh
// re-reads the file when its mtime or size changed, sets what it parses and
// keeps the levers in force on a malformed file. The agent calls refresh at
// the arrival of every request (agentfake.Config.Refresh) and poll calls it
// every pollInterval; mu serializes the two.
type behaviorFront struct {
	path  string
	agent *agentfake.Agent

	mu      sync.Mutex
	present bool
	mtime   time.Time
	size    int64
}

// refresh implements the behavior file's re-read when its mtime or size
// changed. A malformed file is logged once per mtime or size change and
// ignored, keeping the previous levers; a file that is gone clears them.
// Every load sets every lever, so it re-arms hang_rounds and leftover_rounds
// and a rewrite counts their rounds afresh.
func (f *behaviorFront) refresh(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, err := os.Stat(f.path)
	if err != nil {
		if f.present {
			slog.InfoContext(ctx, "behavior file gone",
				slog.String("path", f.path))
			f.present = false
			f.mtime = time.Time{}
			f.size = 0
			f.set(ctx, nil)
		}
		return
	}
	if f.present && info.ModTime().Equal(f.mtime) && info.Size() == f.size {
		return
	}
	f.present = true
	f.mtime = info.ModTime()
	f.size = info.Size()
	data, err := os.ReadFile(f.path)
	if err != nil {
		slog.ErrorContext(ctx, "behavior file unreadable",
			slog.String("path", f.path),
			slog.String("error", err.Error()))
		return
	}
	parsed, err := agentfake.ParseBehavior(data)
	if err != nil {
		slog.ErrorContext(ctx, "behavior file malformed",
			slog.String("path", f.path),
			slog.String("error", err.Error()))
		return
	}
	if f.set(ctx, parsed) {
		slog.InfoContext(ctx, "behavior file loaded",
			slog.String("path", f.path),
			slog.Int("object_cnt", len(parsed.Objects)))
	}
}

// set hands the parsed levers to the agent and reports whether it took them.
func (f *behaviorFront) set(ctx context.Context, b *agentfake.Behavior) bool {
	if err := f.agent.SetBehavior(b); err != nil {
		slog.ErrorContext(ctx, "behavior file refused",
			slog.String("path", f.path),
			slog.String("error", err.Error()))
		return false
	}
	return true
}

// poll re-reads the behavior file every pollInterval until ctx ends.
func (f *behaviorFront) poll(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.refresh(ctx)
		}
	}
}

// ---------------------------------------------------------------------------
// The program
// ---------------------------------------------------------------------------

// program is one fake agent with its behavior file front and its server.
type program struct {
	front  *behaviorFront
	agent  *agentfake.Agent
	server *grpc.Server
}

// newProgram builds the agent of opts over its directory — the stored state
// loaded from the state file, the levers from the behavior file — and the
// server that serves it behind the real server interceptors: they are not
// optional, since agent.log is the suite's only record of what the worker
// sent and which worker sent it (grpc.md, Wiring).
func newProgram(ctx context.Context, opts options) (*program, error) {
	front := &behaviorFront{path: filepath.Join(opts.dir, behaviorFileName)}
	agent, err := agentfake.New(ctx, agentfake.Config{
		Size:    opts.size,
		Dir:     opts.dir,
		Refresh: front.refresh,
	})
	if err != nil {
		return nil, err
	}
	front.agent = agent
	front.refresh(ctx)
	return &program{
		front:  front,
		agent:  agent,
		server: agentfake.NewServer(agent, opts.kind, false),
	}, nil
}

// serve serves the program on lis, polling the behavior file meanwhile,
// until the server stops.
func (p *program) serve(ctx context.Context, lis net.Listener) error {
	go p.front.poll(ctx)
	return p.server.Serve(lis)
}

func main() {
	opts, err := parseArgs(os.Args[1:], os.Stderr)
	if err != nil {
		var exit *exitError
		if errors.As(err, &exit) {
			if exit.msg != "" {
				fmt.Fprintln(os.Stderr, exit.msg)
			}
			os.Exit(exit.code)
		}
		die("%v", err)
	}

	ctx := common.WithTraceId(context.Background(), common.NewTraceId())
	p, err := newProgram(ctx, opts)
	if err != nil {
		die("%v", err)
	}
	listener, err := net.Listen("tcp", opts.addr)
	if err != nil {
		die("listening on %s failed: %v", opts.addr, err)
	}
	slog.InfoContext(ctx, "fakeagent started",
		slog.String("mode", opts.mode),
		slog.String("grpc_address", opts.addr),
		slog.String("dir", opts.dir),
	)
	if err := p.serve(ctx, listener); err != nil {
		die("serving failed: %v", err)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fakeagent: "+format+"\n", args...)
	os.Exit(1)
}
