package worker

import (
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/distributed-nvme/distributed-nvme/common"
)

// connCache is the process-wide addr_port -> *grpc.ClientConn cache of RW7.
// Every stream and every unary call the worker makes to one agent multiplexes
// over that agent's single connection; the entry is reference-counted by the
// objects using the endpoint and closed when the last one releases it.
// Concurrency-safe: revision workers of different shards, roles and SPs
// acquire and release from their own goroutines.
type connCache struct {
	// dial builds one connection. It is a field so the unit tests can dial a
	// bufconn listener instead of a real endpoint; production uses
	// dialAgent, which installs the grpc.md §4 chain options.
	dial func(addrPort string) (*grpc.ClientConn, error)

	mu      sync.Mutex
	entries map[string]*connEntry
}

// connEntry is one cached connection and its reference count (RW7).
type connEntry struct {
	conn *grpc.ClientConn
	refs int
}

// newConnCache builds the cache with the production dialer.
func newConnCache() *connCache {
	return &connCache{
		dial:    dialAgent,
		entries: make(map[string]*connEntry),
	}
}

// dialAgent opens one agent connection with the chain options every dnv
// client connection MUST carry (grpc.md §4): insecure transport plus the
// unary and stream client interceptors, which propagate the RW10 trace id and
// log every message.
//
// grpc.NewClient does not block, so this never fails because the agent is
// down; the first round's stream or syncup reports that instead.
func dialAgent(addrPort string) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(
		addrPort,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(common.GrpcUnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(common.GrpcStreamClientInterceptor()),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc client %s: %w", addrPort, err)
	}
	return conn, nil
}

// acquire returns the connection for addrPort, building it on first use, and
// takes one reference (RW7). Every acquire is matched by exactly one release.
func (c *connCache) acquire(addrPort string) (*grpc.ClientConn, error) {
	if addrPort == "" {
		return nil, fmt.Errorf("grpc client: empty addr_port")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[addrPort]; ok {
		entry.refs++
		return entry.conn, nil
	}
	conn, err := c.dial(addrPort)
	if err != nil {
		return nil, err
	}
	c.entries[addrPort] = &connEntry{conn: conn, refs: 1}
	return conn, nil
}

// release drops one reference and closes the connection when it was the last
// one (RW7). Releasing an endpoint that is not cached is a no-op, so a
// revision worker may release unconditionally on stop.
func (c *connCache) release(addrPort string) {
	c.mu.Lock()
	entry, ok := c.entries[addrPort]
	if !ok {
		c.mu.Unlock()
		return
	}
	entry.refs--
	if entry.refs > 0 {
		c.mu.Unlock()
		return
	}
	delete(c.entries, addrPort)
	c.mu.Unlock()
	// Closed outside the lock: Close blocks until the transport is torn down
	// and no other acquire may hand this entry out any more.
	_ = entry.conn.Close()
}

// refs reports the current reference count of an endpoint, 0 when it is not
// cached. It exists for the RW7 unit test.
func (c *connCache) refs(addrPort string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[addrPort]
	if !ok {
		return 0
	}
	return entry.refs
}
