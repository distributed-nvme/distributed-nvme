package worker

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// bufconn agents (the §13 "fake agents built from the generated servers")
// ---------------------------------------------------------------------------

// stubDnAgent is a DiskNodeAgent whose CheckDn and SyncupDn behavior each
// test supplies.
type stubDnAgent struct {
	pb.UnimplementedDiskNodeAgentServer

	mu         sync.Mutex
	checkReqs  []*pb.CheckDnRequest
	syncupReqs []*pb.SyncupDnRequest
	streams    int

	// checkReply builds the reply to one CheckDn request; nil means "say
	// nothing", which is how a round timeout is produced (RW4 step 3).
	checkReply func(req *pb.CheckDnRequest) *pb.CheckDnReply
	// syncupReply answers one SyncupDn.
	syncupReply func(req *pb.SyncupDnRequest) (*pb.SyncupDnReply, error)
}

func (s *stubDnAgent) CheckDn(
	stream grpc.BidiStreamingServer[pb.CheckDnRequest, pb.CheckDnReply],
) error {
	s.mu.Lock()
	s.streams++
	s.mu.Unlock()
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.checkReqs = append(s.checkReqs, req)
		build := s.checkReply
		s.mu.Unlock()
		if build == nil {
			continue
		}
		reply := build(req)
		if reply == nil {
			continue
		}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

func (s *stubDnAgent) SyncupDn(
	ctx context.Context, req *pb.SyncupDnRequest,
) (*pb.SyncupDnReply, error) {
	s.mu.Lock()
	s.syncupReqs = append(s.syncupReqs, req)
	answer := s.syncupReply
	s.mu.Unlock()
	if answer == nil {
		return &pb.SyncupDnReply{Revision: req.GetRevision()}, nil
	}
	return answer(req)
}

func (s *stubDnAgent) checkCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.checkReqs)
}

func (s *stubDnAgent) syncups() []*pb.SyncupDnRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.SyncupDnRequest(nil), s.syncupReqs...)
}

// checkReqRevisions is the revision of every CheckDn request received, in
// order (RW4 step 2).
func (s *stubDnAgent) checkReqRevisions() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint64, 0, len(s.checkReqs))
	for _, req := range s.checkReqs {
		out = append(out, req.GetRevision())
	}
	return out
}

func (s *stubDnAgent) streamCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams
}

func (s *stubDnAgent) setCheckReply(
	fn func(req *pb.CheckDnRequest) *pb.CheckDnReply,
) {
	s.mu.Lock()
	s.checkReply = fn
	s.mu.Unlock()
}

// stubCnAgent is the ControllerNodeAgent twin of stubDnAgent.
type stubCnAgent struct {
	pb.UnimplementedControllerNodeAgentServer

	mu         sync.Mutex
	checkReqs  []*pb.CheckCnRequest
	syncupReqs []*pb.SyncupCnRequest

	checkReply func(req *pb.CheckCnRequest) *pb.CheckCnReply
}

func (s *stubCnAgent) CheckCn(
	stream grpc.BidiStreamingServer[pb.CheckCnRequest, pb.CheckCnReply],
) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.checkReqs = append(s.checkReqs, req)
		build := s.checkReply
		s.mu.Unlock()
		if build == nil {
			continue
		}
		if reply := build(req); reply != nil {
			if err := stream.Send(reply); err != nil {
				return err
			}
		}
	}
}

func (s *stubCnAgent) SyncupCn(
	ctx context.Context, req *pb.SyncupCnRequest,
) (*pb.SyncupCnReply, error) {
	s.mu.Lock()
	s.syncupReqs = append(s.syncupReqs, req)
	s.mu.Unlock()
	return &pb.SyncupCnReply{Revision: req.GetRevision()}, nil
}

func (s *stubCnAgent) syncups() []*pb.SyncupCnRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.SyncupCnRequest(nil), s.syncupReqs...)
}

// agentFleet maps agent endpoints to bufconn listeners, so a connCache
// dialing "dn0:9520" reaches the stub registered under that name.
type agentFleet struct {
	mu        sync.Mutex
	listeners map[string]*bufconn.Listener
}

func newAgentFleet(t *testing.T) *agentFleet {
	t.Helper()
	return &agentFleet{listeners: make(map[string]*bufconn.Listener)}
}

// addDn registers a DiskNodeAgent stub at addrPort.
func (f *agentFleet) addDn(t *testing.T, addrPort string, stub *stubDnAgent) {
	t.Helper()
	f.serve(t, addrPort, func(server *grpc.Server) {
		pb.RegisterDiskNodeAgentServer(server, stub)
	})
}

// addCn registers a ControllerNodeAgent stub at addrPort.
func (f *agentFleet) addCn(t *testing.T, addrPort string, stub *stubCnAgent) {
	t.Helper()
	f.serve(t, addrPort, func(server *grpc.Server) {
		pb.RegisterControllerNodeAgentServer(server, stub)
	})
}

func (f *agentFleet) serve(
	t *testing.T, addrPort string, register func(*grpc.Server),
) {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
	)
	register(server)
	go func() {
		_ = server.Serve(lis)
	}()
	f.mu.Lock()
	f.listeners[addrPort] = lis
	f.mu.Unlock()
	t.Cleanup(func() {
		server.Stop()
		lis.Close()
	})
}

// dial is the connCache dialer of RW7 pointed at the fleet: the same chain
// options grpc.md §4 requires, over bufconn instead of TCP.
func (f *agentFleet) dial(addrPort string) (*grpc.ClientConn, error) {
	f.mu.Lock()
	lis, ok := f.listeners[addrPort]
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("no such agent: " + addrPort)
	}
	return grpc.NewClient(
		"passthrough:///"+addrPort,
		grpc.WithContextDialer(
			func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			},
		),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(common.GrpcUnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(common.GrpcStreamClientInterceptor()),
	)
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

const (
	testCid  = uint64(0xc1)
	testDnId = uint64(0xd1)
	testCnId = uint64(0xc2)
	testAddr = "dn0:9520"
)

// revHarness runs one real revision worker against a bufconn agent.
type revHarness struct {
	t     *testing.T
	logs  *logCapture
	clk   *fakeClock
	store *fakeStore
	fleet *agentFleet
	deps  *deps
	hw    *fakeHealthWriter
}

func newRevHarness(t *testing.T) *revHarness {
	t.Helper()
	logs := captureLogs(t)
	clk := newFakeClock()
	store := newFakeStore()
	fleet := newAgentFleet(t)
	d := newTestDeps(testConfig(common.WorkerRoleDn), store, clk)
	hw := &fakeHealthWriter{}
	d.health = hw
	d.conns.dial = fleet.dial
	return &revHarness{
		t: t, logs: logs, clk: clk, store: store, fleet: fleet,
		deps: d, hw: hw,
	}
}

// setClusterConf installs a resolved cluster conf in the RW21 cache.
func (h *revHarness) setClusterConf(cid uint64, cc *pb.ClusterConf) {
	h.deps.conf.mu.Lock()
	h.deps.conf.entries[cid] = model.ResolveClusterConf(cc)
	h.deps.conf.mu.Unlock()
}

// defaultConf installs a conf whose four intervals are the 5 s default.
func (h *revHarness) defaultConf() {
	h.setClusterConf(testCid, &pb.ClusterConf{CreationEpoch: 1})
}

// advanceUntil steps the fake clock by one round period at a time until cond
// holds. It exists because a round's timers are armed by the loop goroutine
// AFTER the stimulus a test observes, so a single advance can land before the
// timer it is meant to fire.
func (h *revHarness) advanceUntil(
	what string, step time.Duration, cond func() bool,
) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		h.clk.advance(step)
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

// seedDnConf writes the DnConf a SyncupDn is built from (RW13).
func (h *revHarness) seedDnConf(addrPort string, conf *pb.DnConf) {
	h.store.seed(h.t, model.DnConfKey(testCid, addrPort), conf)
}

// startDn starts a dn revision worker at the given endpoint and revision.
func (h *revHarness) startDn(addrPort string, revision uint64) *revWorker {
	h.t.Helper()
	params := revWorkerParams{
		deps:    h.deps,
		role:    common.WorkerRoleDn,
		shard:   testShard,
		cid:     testCid,
		id:      testDnId,
		seed:    seedOf(1),
		desired: desiredState{revision: revision, handle: addrPort},
	}
	w := startRevWorker(params, func(host *revWorker) objDriver {
		return newDnDriver(params, host)
	})
	h.t.Cleanup(w.stop)
	return w
}

// roundInterval is the resolved default round period of every kind.
const roundInterval = common.DefaultHealthCheckInterval * time.Second

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestRevisionRoundTimeoutMarksUnreachable checks RW4 steps 3 and 4: a reply
// that does not arrive within the round timeout closes the stream, marks the
// object unreachable (HL1) and makes the next round open a FRESH stream, so a
// late reply can never be read as the next round's.
func TestRevisionRoundTimeoutMarksUnreachable(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{
		checkReply: func(*pb.CheckDnRequest) *pb.CheckDnReply { return nil },
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	h.startDn(testAddr, 5)

	waitFor(t, "first check", func() bool { return stub.checkCount() >= 1 })
	// The round timer is the only thing that can end this round.
	h.advanceUntil("unreachable recorded", roundInterval, func() bool {
		return len(h.hw.all()) == 1
	})
	write := h.hw.all()[0]
	if write.record != healthRecordDn || write.epoch == 0 {
		t.Fatalf("health write = %+v, want a dn epoch", write)
	}
	// Next round: a brand-new stream.
	h.advanceUntil("second stream", roundInterval, func() bool {
		return stub.streamCount() >= 2
	})
}

// TestRevisionMismatchTriggersSyncup checks RW4 step 5: a reply whose
// revision differs from the desired one re-syncs the object.
func TestRevisionMismatchTriggersSyncup(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			// The agent is two revisions behind.
			return &pb.CheckDnReply{Revision: req.GetRevision() - 2}
		},
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	h.startDn(testAddr, 9)

	waitFor(t, "syncup", func() bool { return len(stub.syncups()) >= 1 })
	if got := stub.syncups()[0].GetRevision(); got != 9 {
		t.Fatalf("syncup revision = %d, want the desired 9", got)
	}
	waitFor(t, "syncup result record", func() bool {
		return len(h.logs.withMsg(msgSyncupResult)) >= 1
	})
}

// TestRevisionRejectedSyncupIsLogged checks RW5: code != 0 logs
// "syncup rejected", and ReplyCodeStaleRevision is logged at Error.
func TestRevisionRejectedSyncupIsLogged(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			// code != 0 alone re-syncs, even at a matching revision.
			return &pb.CheckDnReply{
				Revision: req.GetRevision(),
				AgentReply: &pb.AgentReply{
					Code:    common.ReplyCodeUnknownObject,
					Details: "unknown dn",
				},
			}
		},
		syncupReply: func(req *pb.SyncupDnRequest) (*pb.SyncupDnReply, error) {
			return &pb.SyncupDnReply{
				Revision: req.GetRevision(),
				AgentReply: &pb.AgentReply{
					Code:    common.ReplyCodeStaleRevision,
					Details: "stored revision is newer",
				},
			}, nil
		},
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	h.startDn(testAddr, 4)

	waitFor(t, "rejected", func() bool {
		return len(h.logs.withMsg(msgSyncupRejected)) >= 1
	})
	rec := h.logs.withMsg(msgSyncupRejected)[0]
	if rec["level"] != "ERROR" {
		t.Fatalf("stale revision logged at %v, want ERROR", rec["level"])
	}
	if rec["details"] != "stored revision is newer" {
		t.Fatalf("details = %v", rec["details"])
	}
	if code, _ := rec["code"].(float64); uint32(code) !=
		common.ReplyCodeStaleRevision {
		t.Fatalf("code = %v", rec["code"])
	}
	// A rejected syncup never advances synced, so health is untouched by the
	// code != 0 rows (HL1).
	if got := len(h.hw.all()); got != 0 {
		t.Fatalf("code != 0 wrote health: %v", h.hw.all())
	}
}

// TestRevisionDesiredChangeSyncsAtOnce checks RW6 and RW3: a desired change
// syncs immediately, and changes that arrive while one is in flight coalesce
// into the latest.
func TestRevisionDesiredChangeSyncsAtOnce(t *testing.T) {
	h := newRevHarness(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	stub := &stubDnAgent{
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			return &pb.CheckDnReply{Revision: req.GetRevision()}
		},
		syncupReply: func(req *pb.SyncupDnRequest) (*pb.SyncupDnReply, error) {
			entered <- struct{}{}
			<-release
			return &pb.SyncupDnReply{Revision: req.GetRevision()}, nil
		},
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	w := h.startDn(testAddr, 1)

	waitFor(t, "first check", func() bool { return stub.checkCount() >= 1 })
	// A desired change: an immediate syncup, which then blocks in the agent.
	w.update(desiredState{revision: 2, handle: testAddr})
	<-entered
	// Two more changes while it is in flight: only the latest survives (RW3).
	w.update(desiredState{revision: 3, handle: testAddr})
	w.update(desiredState{revision: 4, handle: testAddr})
	close(release)

	waitFor(t, "coalesced syncup", func() bool {
		for _, req := range stub.syncups() {
			if req.GetRevision() == 4 {
				return true
			}
		}
		return false
	})
	for _, req := range stub.syncups() {
		if req.GetRevision() == 3 {
			t.Fatalf("revision 3 was sent; RW3 must coalesce to the latest")
		}
	}
}

// resyncDriver wraps a real driver and marks the object for an
// equal-revision re-apply from inside observe — which is exactly where BM6
// does it, on the revision worker's own goroutine.
type resyncDriver struct {
	objDriver
	host *revWorker
	fire *atomic.Bool
}

func (d *resyncDriver) observe(ctx context.Context, r *replyState) {
	d.objDriver.observe(ctx, r)
	if d.fire.CompareAndSwap(true, false) {
		d.host.wantResync()
	}
}

// TestRevisionResyncWantedResends checks RW4 step 6 / BM6: a failed push sets
// resyncWanted and the next round issues an equal-revision re-apply even
// though the agent is at the right revision and reported code 0.
func TestRevisionResyncWantedResends(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			return &pb.CheckDnReply{Revision: req.GetRevision()}
		},
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	var fire atomic.Bool
	params := revWorkerParams{
		deps:    h.deps,
		role:    common.WorkerRoleDn,
		shard:   testShard,
		cid:     testCid,
		id:      testDnId,
		seed:    seedOf(1),
		desired: desiredState{revision: 6, handle: testAddr},
	}
	w := startRevWorker(params, func(host *revWorker) objDriver {
		return &resyncDriver{
			objDriver: newDnDriver(params, host),
			host:      host,
			fire:      &fire,
		}
	})
	t.Cleanup(w.stop)

	waitFor(t, "steady state", func() bool { return stub.checkCount() >= 1 })
	if got := len(stub.syncups()); got != 0 {
		t.Fatalf("%d syncups in the steady state, want none", got)
	}
	// resyncWanted is owned by the loop goroutine, and BM6 sets it from
	// inside the driver — which is where the wrapper above sets it too.
	fire.Store(true)
	h.advanceUntil("equal-revision re-apply", roundInterval, func() bool {
		for _, req := range stub.syncups() {
			if req.GetRevision() == 6 {
				return true
			}
		}
		return false
	})
}

// TestRevisionStopLetsInFlightUnaryFinish checks RW11: a graceful stop lets an
// in-flight unary call finish — the stop ctx is not the RPC ctx — and only
// then closes the stream and logs "revision worker stopped".
func TestRevisionStopLetsInFlightUnaryFinish(t *testing.T) {
	h := newRevHarness(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	finished := make(chan struct{})
	stub := &stubDnAgent{
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			return &pb.CheckDnReply{Revision: req.GetRevision()}
		},
		syncupReply: func(req *pb.SyncupDnRequest) (*pb.SyncupDnReply, error) {
			entered <- struct{}{}
			<-release
			close(finished)
			return &pb.SyncupDnReply{Revision: req.GetRevision()}, nil
		},
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	w := h.startDn(testAddr, 1)

	waitFor(t, "first check", func() bool { return stub.checkCount() >= 1 })
	w.update(desiredState{revision: 2, handle: testAddr})
	<-entered

	stopped := make(chan struct{})
	go func() {
		w.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatalf("stop returned while a unary call was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-finished
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatalf("stop did not return after the unary call finished")
	}
	waitFor(t, "stopped record", func() bool {
		return len(h.logs.withMsg(msgRevisionWorkerStopped)) == 1
	})
	// The reply of the call that was allowed to finish was processed.
	if got := len(h.logs.withMsg(msgSyncupResult)); got == 0 {
		t.Fatalf("the in-flight syncup produced no result record")
	}
}

// TestRevisionIdleWithoutClusterConf checks RW9/SW6: a cluster absent from the
// cache makes the loop idle — no stream, no syncup — with exactly one
// "cluster conf missing" record per idle period.
func TestRevisionIdleWithoutClusterConf(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{}
	h.fleet.addDn(t, testAddr, stub)
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	h.startDn(testAddr, 1)

	waitFor(t, "idle record", func() bool {
		return len(h.logs.withMsg(msgClusterConfMissing)) == 1
	})
	for i := 0; i < 3; i++ {
		h.clk.advance(common.DefaultHealthCheckInterval * time.Second)
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(h.logs.withMsg(msgClusterConfMissing)); got != 1 {
		t.Fatalf("%d cluster conf missing records, want one per idle period",
			got)
	}
	if stub.checkCount() != 0 || len(stub.syncups()) != 0 {
		t.Fatalf("an idle worker talked to its agent")
	}
	if refs := h.deps.conns.refs(testAddr); refs != 0 {
		t.Fatalf("an idle worker holds %d connection references", refs)
	}
	// The conf shows up: the loop leaves the idle state on its next retry.
	h.defaultConf()
	h.advanceUntil("round after the conf arrives", roundInterval, func() bool {
		return stub.checkCount() >= 1
	})
}

// TestConnCacheRefCounting checks RW7: one connection per endpoint, reference
// counted, closed when the last user releases it.
func TestConnCacheRefCounting(t *testing.T) {
	dialed := 0
	fleet := newAgentFleet(t)
	fleet.addDn(t, "dn0:9520", &stubDnAgent{})
	fleet.addDn(t, "dn1:9520", &stubDnAgent{})
	cache := newConnCache()
	cache.dial = func(addrPort string) (*grpc.ClientConn, error) {
		dialed++
		return fleet.dial(addrPort)
	}

	first, err := cache.acquire("dn0:9520")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	second, err := cache.acquire("dn0:9520")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if first != second {
		t.Fatalf("two connections for one endpoint")
	}
	if dialed != 1 {
		t.Fatalf("dialed %d times for one endpoint", dialed)
	}
	if refs := cache.refs("dn0:9520"); refs != 2 {
		t.Fatalf("refs = %d, want 2", refs)
	}
	if _, err := cache.acquire("dn1:9520"); err != nil {
		t.Fatalf("acquire second endpoint: %v", err)
	}
	if dialed != 2 {
		t.Fatalf("dialed %d times for two endpoints", dialed)
	}

	cache.release("dn0:9520")
	if refs := cache.refs("dn0:9520"); refs != 1 {
		t.Fatalf("refs after one release = %d, want 1", refs)
	}
	cache.release("dn0:9520")
	if refs := cache.refs("dn0:9520"); refs != 0 {
		t.Fatalf("refs after the last release = %d, want 0", refs)
	}
	// Releasing an endpoint nobody holds is a no-op.
	cache.release("dn0:9520")
	cache.release("nowhere:1")
	cache.release("dn1:9520")
	if _, err := cache.acquire(""); err == nil {
		t.Fatalf("an empty addr_port was accepted")
	}
}

// TestRevisionWorkerHoldsOneConnectionReference checks that the per-object
// loop takes exactly one reference and gives it back on stop (RW7, RW11).
func TestRevisionWorkerHoldsOneConnectionReference(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			return &pb.CheckDnReply{Revision: req.GetRevision()}
		},
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	w := h.startDn(testAddr, 1)
	waitFor(t, "first check", func() bool { return stub.checkCount() >= 1 })
	if refs := h.deps.conns.refs(testAddr); refs != 1 {
		t.Fatalf("refs = %d, want 1", refs)
	}
	w.stop()
	if refs := h.deps.conns.refs(testAddr); refs != 0 {
		t.Fatalf("refs after stop = %d, want 0", refs)
	}
}

// TestRevisionCheckRequestShape pins RW4 step 2: the round carries the ids,
// the DESIRED revision and show_info = false.
func TestRevisionCheckRequestShape(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			return &pb.CheckDnReply{Revision: req.GetRevision()}
		},
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	h.startDn(testAddr, 13)
	waitFor(t, "first check", func() bool { return stub.checkCount() >= 1 })
	stub.mu.Lock()
	req := stub.checkReqs[0]
	stub.mu.Unlock()
	if req.GetClusterId() != testCid || req.GetDnId() != testDnId ||
		req.GetRevision() != 13 || req.GetShowInfo() {
		t.Fatalf("check request = %v", req)
	}
}

// TestRevisionDesiredChangeDuringRoundSyncsAtOnce checks RW6 where it can
// actually be late: a desired change delivered while the round is waiting for
// its Check reply (RW4 step 3). Nothing in this test advances the fake clock,
// so the round timer never fires — the loop must pick the change up out of the
// round itself, not after it. RW9 lets the wait be an hour and RW5 adds a
// syncup timeout on top, so an SpRev bump from a failover (AR5) or a spare
// switch (AR8) would otherwise reach a mid-round agent about a minute late,
// preceded by one request built from the superseded revision.
func TestRevisionDesiredChangeDuringRoundSyncsAtOnce(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{
		// The agent never answers this round.
		checkReply: func(*pb.CheckDnRequest) *pb.CheckDnReply { return nil },
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	w := h.startDn(testAddr, 1)

	waitFor(t, "first check", func() bool { return stub.checkCount() >= 1 })
	w.update(desiredState{revision: 2, handle: testAddr})
	waitFor(t, "syncup while the round is still waiting", func() bool {
		return len(stub.syncups()) >= 1
	})
	if got := stub.syncups()[0].GetRevision(); got != 2 {
		t.Fatalf("syncup revision = %d, want the new 2", got)
	}
	// The round was given up, not failed: an abandoned round is not a missed
	// reply, so nothing set an err_epoch (HL1).
	for _, write := range h.hw.all() {
		if write.epoch != 0 {
			t.Fatalf("an overtaken round reported unhealthy: %+v", write)
		}
	}
	// The superseded round's stream went with it, so the reply it is still
	// owed can never be read as the next round's (RW4 step 4).
	h.advanceUntil("round after the change", roundInterval, func() bool {
		return stub.streamCount() >= 2
	})
	if got := stub.checkReqRevisions(); got[len(got)-1] != 2 {
		t.Fatalf("check revisions = %v, want the last one to be 2", got)
	}
}

// syncedProbeDriver reports the host's synced revision (RW2) from inside
// observe, which runs on the revision worker's own goroutine — the only place
// the field may be read without racing the loop.
type syncedProbeDriver struct {
	objDriver
	host *revWorker
	seen *atomic.Uint64
}

func (d *syncedProbeDriver) observe(ctx context.Context, r *replyState) {
	d.objDriver.observe(ctx, r)
	d.seen.Store(d.host.syncedRevision())
}

// TestRevisionCleanRoundAdvancesSynced checks RW2's definition of `synced`:
// "the last revision the agent acknowledged — the revision of a code == 0
// Syncup* reply OR OF A Check* reply". After a shard handoff or a worker
// restart the agent is often already at the desired revision, so no Syncup*
// is issued at all (RW4 step 5) and the Check reply is the only
// acknowledgement there is. Getting this wrong leaves synced at 0, and the BM3
// pushes that carry the object's synced revision are then rejected as stale.
func TestRevisionCleanRoundAdvancesSynced(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{
		// The agent is already where the worker wants it: nothing to sync.
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			return &pb.CheckDnReply{Revision: req.GetRevision()}
		},
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	var seen atomic.Uint64
	params := revWorkerParams{
		deps:    h.deps,
		role:    common.WorkerRoleDn,
		shard:   testShard,
		cid:     testCid,
		id:      testDnId,
		seed:    seedOf(1),
		desired: desiredState{revision: 7, handle: testAddr},
	}
	w := startRevWorker(params, func(host *revWorker) objDriver {
		return &syncedProbeDriver{
			objDriver: newDnDriver(params, host),
			host:      host,
			seen:      &seen,
		}
	})
	t.Cleanup(w.stop)

	waitFor(t, "synced from the Check reply", func() bool {
		return seen.Load() == 7
	})
	if got := len(stub.syncups()); got != 0 {
		t.Fatalf("%d syncups, want the round alone to advance synced", got)
	}
}
