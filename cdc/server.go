package cdc

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// This file is the NP1 listener: one TCP socket, one goroutine per accepted
// connection, and the two pieces of state a connection needs from the
// instance — the round-robin CNTLID counter and the Identify data.

// acceptRetryDelay is how long the accept loop pauses after an error that is
// not the listener closing, so a transient failure (a file descriptor
// shortage, say) costs a pause rather than the service.
const acceptRetryDelay = 100 * time.Millisecond

// server is the NVMe/TCP service of one dnv-cdc instance.
type server struct {
	deps *deps
	reg  *registry
	ln   net.Listener
	ctx  context.Context
	// serial is Identify's SN, derived from the listen endpoint so that two
	// twins are distinguishable in a host's `nvme list-subsys` output (NP7).
	serial string

	mu    sync.Mutex
	conns map[*conn]struct{}
	// cntlIdSeq is the round-robin dynamic CNTLID counter of NP5. It walks
	// [1, common.CdcCntlIdMax]; ids are never reused before the whole range
	// has been, which keeps a reconnecting host from seeing its own old id.
	cntlIdSeq uint32
}

// newServer opens the listener (NP1). It fails fast: a busy port is a
// configuration error and dnv-cdc must not come up half-serving.
func newServer(ctx context.Context, d *deps, reg *registry) (*server, error) {
	ln, err := net.Listen("tcp", d.cfg.listenAddr())
	if err != nil {
		return nil, err
	}
	return &server{
		deps:   d,
		reg:    reg,
		ln:     ln,
		ctx:    ctx,
		serial: endpointSerial(d.cfg.TrAddr, d.cfg.TrSvcId),
		conns:  make(map[*conn]struct{}),
	}, nil
}

// addr is the address actually listened on, which the §8 tests need when they
// ask for port 0.
func (s *server) addr() net.Addr {
	return s.ln.Addr()
}

// run is the accept loop. It returns once ctx has ended and every connection
// has been closed and joined (NP1: stop accepting, close every connection,
// stop).
func (s *server) run(ctx context.Context) {
	var wg sync.WaitGroup
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = s.ln.Close()
		case <-stopped:
		}
	}()
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			slog.ErrorContext(ctx, "cdc accept failed",
				slog.String("error", err.Error()),
			)
			select {
			case <-ctx.Done():
			case <-time.After(acceptRetryDelay):
			}
			continue
		}
		c := newConn(s, nc)
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.serve()
		}()
	}
	close(stopped)
	// Unconditionally, not only on the ctx path: the loop can also leave on
	// an accept error observed in the same instant ctx ended, and the
	// closer goroutine would then take its `stopped` branch instead.
	// Closing twice is harmless.
	_ = s.ln.Close()
	s.shutdownConns()
	wg.Wait()
}

// shutdownConns closes every live connection with the §7 shutdown reason.
func (s *server) shutdownConns() {
	s.mu.Lock()
	live := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		live = append(live, c)
	}
	s.mu.Unlock()
	for _, c := range live {
		c.shutdown(reasonShutdown)
	}
}

// forget drops one finished connection from the instance's set (NP13).
func (s *server) forget(c *conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// connCount is the live connection count, for the §8 tests.
func (s *server) connCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// nextCntlId hands out the next dynamic controller id (NP5).
func (s *server) nextCntlId() uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cntlIdSeq++
	if s.cntlIdSeq > common.CdcCntlIdMax {
		s.cntlIdSeq = 1
	}
	return uint16(s.cntlIdSeq)
}

// identifyController renders the Identify data for one connection. CNTLID
// must equal the id that connection was given at Connect: the Linux host
// compares the two and refuses the controller when they differ.
func (s *server) identifyController(cntlId uint16) []byte {
	return buildIdentify(cntlId, s.serial)
}
