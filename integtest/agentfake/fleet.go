package agentfake

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Kind is the set of agent services a server registers.
type Kind uint8

const (
	// Dn registers the DiskNodeAgent service.
	Dn Kind = 1 << iota
	// Cn registers the ControllerNodeAgent service.
	Cn
)

// NewServer builds the gRPC server of one agent with the service of every
// kind in kinds registered. It carries the message limits of the agents'
// wiring (grpc.md, Wiring), so the fake takes any request an agent takes,
// and, unless quiet, the real server interceptors, whose log records every
// message the agent received under the caller's trace id — the shell
// suites' evidence of what was sent and by which worker. A quiet server
// logs nothing and still stamps the caller's trace id (callTraceId).
func NewServer(a *Agent, kinds Kind, quiet bool) *grpc.Server {
	opts := common.GrpcServerMsgOptions()
	if !quiet {
		opts = append(opts,
			grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor()),
			grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
		)
	}
	server := grpc.NewServer(opts...)
	if kinds&Dn != 0 {
		pb.RegisterDiskNodeAgentServer(server, a)
	}
	if kinds&Cn != 0 {
		pb.RegisterControllerNodeAgentServer(server, a)
	}
	return server
}

// ---------------------------------------------------------------------------
// The in-process fleet
// ---------------------------------------------------------------------------

// bufconnSize is the buffer of each direction of one in-memory connection.
// bufconn allocates it for every connection and a fleet holds thousands, so
// it is small; a larger message passes through it in pieces.
const bufconnSize = 32 * 1024

// FleetConfig is what a Fleet is built with.
type FleetConfig struct {
	// Quiet serves every agent without the server interceptors' message
	// log, for a fleet so large that the log would cost more than the
	// test; the records each agent keeps (Record, Calls) stay.
	Quiet bool
	// DialOptions are added to every connection Dial builds, after the
	// wiring's own — a harness shortens the reconnect backoff here.
	DialOptions []grpc.DialOption
}

// Fleet serves agents in-process over in-memory connections, one listener
// per addr_port, so a test reaches each through the address its record
// carries. Safe for concurrent use.
type Fleet struct {
	cfg FleetConfig

	mu     sync.Mutex
	agents map[string]*Agent
}

// serving is one fleet member's serving state; nil server and listener mean
// stopped.
type serving struct {
	fleet  *Fleet
	addr   string
	kind   Kind
	lis    *bufconn.Listener
	server *grpc.Server
}

// NewFleet builds an empty fleet.
func NewFleet(cfg FleetConfig) *Fleet {
	return &Fleet{cfg: cfg, agents: make(map[string]*Agent)}
}

// Add builds an agent of the kind at addrPort and starts serving it.
func (f *Fleet) Add(addrPort string, kind Kind, cfg Config) (*Agent, error) {
	a, err := New(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	a.serving = &serving{fleet: f, addr: addrPort, kind: kind}
	f.mu.Lock()
	if _, taken := f.agents[addrPort]; taken {
		f.mu.Unlock()
		return nil, fmt.Errorf("an agent already serves at %s", addrPort)
	}
	f.agents[addrPort] = a
	f.mu.Unlock()
	a.smu.Lock()
	defer a.smu.Unlock()
	a.serveLocked()
	return a, nil
}

// Agent is the agent at addrPort, nil when none was added there.
func (f *Fleet) Agent(addrPort string) *Agent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.agents[addrPort]
}

// Close stops every agent of the fleet.
func (f *Fleet) Close() {
	f.mu.Lock()
	agents := make([]*Agent, 0, len(f.agents))
	for _, a := range f.agents {
		agents = append(agents, a)
	}
	f.mu.Unlock()
	for _, a := range agents {
		a.Stop()
	}
}

// DialContext opens one in-memory connection to the agent at addr, the
// dialer of grpc.WithContextDialer for a target "passthrough:///<addr>".
// Where no agent serves — none was added, or it is stopped — every attempt
// fails as a TCP connect to a closed port does, so the caller's connection
// stays down until the agent starts, as a connection to a stopped agent
// does.
func (f *Fleet) DialContext(ctx context.Context, addr string) (net.Conn, error) {
	f.mu.Lock()
	a := f.agents[addr]
	f.mu.Unlock()
	var lis *bufconn.Listener
	if a != nil {
		a.smu.Lock()
		lis = a.serving.lis
		a.smu.Unlock()
	}
	if lis == nil {
		return nil, fmt.Errorf("dial %s: %w", addr, syscall.ECONNREFUSED)
	}
	return lis.DialContext(ctx)
}

// Dial builds one client connection to addrPort over DialContext, with the
// options every dnv client connection carries (grpc.md, Wiring) — the
// client interceptors, which carry the trace id, and the message limit —
// and then FleetConfig.DialOptions. It fits the worker's connection cache
// seam; like grpc.NewClient it never fails because the agent is down: the
// first call on the connection reports that.
func (f *Fleet) Dial(addrPort string) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{
		grpc.WithContextDialer(f.DialContext),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(common.GrpcUnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(common.GrpcStreamClientInterceptor()),
		common.GrpcClientMsgOption(),
	}
	opts = append(opts, f.cfg.DialOptions...)
	conn, err := grpc.NewClient("passthrough:///"+addrPort, opts...)
	if err != nil {
		return nil, fmt.Errorf("grpc client %s: %w", addrPort, err)
	}
	return conn, nil
}

// serveLocked starts serving a fleet member on a fresh listener; a.smu is
// held.
func (a *Agent) serveLocked() {
	s := a.serving
	s.lis = bufconn.Listen(bufconnSize)
	s.server = NewServer(a, s.kind, s.fleet.cfg.Quiet)
	go s.server.Serve(s.lis)
}

// Store says what a restarted agent finds of its stored state (Start).
type Store int

const (
	// KeepStore keeps the stored state, as a restarted agent finds its
	// local files.
	KeepStore Store = iota
	// DropStore empties it, as an agent started on empty local files.
	DropStore
)

// Stop stops a fleet member as a killed agent stops: every call and Check
// stream it serves ends with a transport error, and every connection
// attempt fails until Start. A call in flight at the stop — held by a
// lever, or not yet past its arrival — applies, records and answers
// nothing after it, in this lifetime or the next (liveLocked), so a held
// syncup ends unapplied. The server does not wait for the handlers to
// return: the lifetime test stops each at its next step, and a handler that
// waits on something the call does not end, such as a test's Refresh, would
// otherwise hold the stop. Stopping a stopped agent, or one no fleet serves,
// does nothing.
func (a *Agent) Stop() {
	a.smu.Lock()
	defer a.smu.Unlock()
	s := a.serving
	if s == nil || s.server == nil {
		return
	}
	a.mu.Lock()
	a.gen++
	a.mu.Unlock()
	s.server.Stop()
	s.server, s.lis = nil, nil
}

// Start serves a stopped fleet member again at its address, as a restarted
// agent, in a new lifetime: with its stored state kept or dropped (store),
// with every lever kept, and with what a process holds in memory alone
// started afresh — the times of the rows' status changes and the
// observations, and the counted levers, re-armed from the levers as a
// restarted program arms them from its behavior file.
func (a *Agent) Start(store Store) error {
	a.smu.Lock()
	defer a.smu.Unlock()
	s := a.serving
	if s == nil {
		return errors.New("agentfake: Start of an agent no fleet serves")
	}
	if s.server != nil {
		return fmt.Errorf("agentfake: the agent at %s is serving", s.addr)
	}
	a.mu.Lock()
	a.gen++
	a.restartStoreLocked(context.Background(), store)
	clear(a.epochs)
	clear(a.observed)
	clear(a.heldRounds)
	clear(a.leftRounds)
	for key := range a.beh.Objects {
		a.armLocked(key)
	}
	a.notifyLocked()
	a.mu.Unlock()
	a.serveLocked()
	return nil
}
