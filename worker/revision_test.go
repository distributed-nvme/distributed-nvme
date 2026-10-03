package worker

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// bufconn agents (fake agents built from the generated servers)
// ---------------------------------------------------------------------------

// stubDnAgent is a DiskNodeAgent whose CheckDn and SyncupDn behavior each
// test supplies.
type stubDnAgent struct {
	pb.UnimplementedDiskNodeAgentServer

	mu         sync.Mutex
	checkReqs  []*pb.CheckDnRequest
	syncupReqs []*pb.SyncupDnRequest
	streams    int
	// streamExits counts the CheckDn handlers that have RETURNED. The client
	// side of a dropped stream is invisible from here — CloseSend and the
	// context cancel of dropStream are what end the handler — so this is how
	// a test tells "the stream is gone" from "the stream is idle".
	streamExits int

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
	defer func() {
		s.mu.Lock()
		s.streamExits++
		s.mu.Unlock()
	}()
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

// streamExitCount is how many CheckDn handlers have returned, i.e. how many
// streams the worker has dropped (or lost).
func (s *stubDnAgent) streamExitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamExits
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
// options grpc.md, Wiring, requires, over bufconn instead of TCP.
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

// setClusterConf installs a cluster conf in the RW21 cache exactly as given.
// It resolves nothing, mirroring the cache itself (architecture.md, Common
// validation) — which is the only reason a test can hand the loop a conf the
// gateway could not have written and watch it refuse.
func (h *revHarness) setClusterConf(cid uint64, cc *pb.ClusterConf) {
	h.deps.conf.mu.Lock()
	h.deps.conf.entries[cid] = cc
	h.deps.conf.mu.Unlock()
}

// defaultConf installs the concrete stored conf of testClusterConf, whose four
// intervals are the 5 s the gateway resolved them to.
func (h *revHarness) defaultConf() {
	h.setClusterConf(testCid, testClusterConf())
}

// advanceUntil steps the fake clock by one round period at a time until cond
// holds. It exists because a round's timers are armed by the loop goroutine
// AFTER the stimulus a test observes, so a single advance can land before the
// timer it is meant to fire.
//
// It waits on state, never on time, so its test runs in a testing/synctest
// bubble (inBubble): cond is read, and the clock moved, only once every other
// goroutine of the test is durably blocked (synctest.Wait). Every round the
// last step started has then either had its reply read and observed, the
// Syncup* that reply called for included, or is waiting for an agent that
// does not answer. A step taken every few milliseconds instead can land while
// an answered round's reply is still on its way: it fires that round's timer
// (RW4 step 3), the round is given up and the object reported unreachable,
// and a count of the rounds the agent answered no longer matches what the
// worker did with them.
func (h *revHarness) advanceUntil(
	what string, step time.Duration, cond func() bool,
) {
	h.t.Helper()
	advanceClockUntil(h.t, h.clk, what, step, cond)
}

// maxAdvanceSteps bounds advanceClockUntil: far more clock steps than any
// test needs for its condition, so reaching it means the condition never
// came.
const maxAdvanceSteps = 1000

// advanceClockUntil is advanceUntil for both harnesses. It must run inside a
// synctest bubble.
func advanceClockUntil(
	t *testing.T,
	clk *fakeClock,
	what string,
	step time.Duration,
	cond func() bool,
) {
	t.Helper()
	for i := 0; i < maxAdvanceSteps; i++ {
		synctest.Wait()
		if cond() {
			return
		}
		clk.advance(step)
	}
	t.Fatalf("still waiting for %s after %d steps of %v",
		what, maxAdvanceSteps, step)
}

// inBubble runs a subtest's body in a testing/synctest bubble, which
// advanceUntil needs; a top-level test calls synctest.Test itself. Inside
// one, waitFor's sleeps and a test's own time.Sleep return only once every
// other goroutine of the test is durably blocked.
func inBubble(body func(t *testing.T)) func(t *testing.T) {
	return func(t *testing.T) {
		synctest.Test(t, body)
	}
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
	synctest.Test(t, testRevisionRoundTimeoutMarksUnreachable)
}

func testRevisionRoundTimeoutMarksUnreachable(t *testing.T) {
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
	// A rejection carries no verdict, so health is untouched by the code != 0
	// rows (HL1).
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

// TestRevisionWorkerWaitsForItsPredecessor checks RW1 across a parent's stop
// off its own goroutine (childStops): a worker started while its object's
// previous worker is still stopping opens no stream and logs nothing until
// that stop has returned, and one stopped while it waits returns only after
// that stop too, so a chain of restarts of one object stays in order.
func TestRevisionWorkerWaitsForItsPredecessor(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			return &pb.CheckDnReply{Revision: req.GetRevision()}
		},
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	start := func(after <-chan struct{}) *revWorker {
		params := revWorkerParams{
			deps:    h.deps,
			role:    common.WorkerRoleDn,
			shard:   testShard,
			cid:     testCid,
			id:      testDnId,
			seed:    seedOf(1),
			desired: desiredState{revision: 1, handle: testAddr},
			after:   after,
		}
		return startRevWorker(params, func(host *revWorker) objDriver {
			return newDnDriver(params, host)
		})
	}

	// Stopped while it waits: its stop outlasts the wait.
	first := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		start(first).stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatalf("a waiting worker's stop returned before its " +
			"predecessor's")
	case <-time.After(50 * time.Millisecond):
	}
	close(first)
	waitClosed(t, "the waiting worker's stop", stopped)

	// Left to run: nothing until the predecessor has stopped.
	second := make(chan struct{})
	w := start(second)
	t.Cleanup(w.stop)
	t.Cleanup(func() {
		select {
		case <-second:
		default:
			close(second)
		}
	})
	time.Sleep(50 * time.Millisecond)
	if got := stub.streamCount(); got != 0 {
		t.Fatalf("%d streams opened before the predecessor stopped", got)
	}
	if got := len(h.logs.withMsg(msgRevisionWorkerStarted)); got != 0 {
		t.Fatalf("%d started records before the predecessor stopped", got)
	}
	close(second)
	waitFor(t, "the first check", func() bool { return stub.checkCount() >= 1 })
	if got := len(h.logs.withMsg(msgRevisionWorkerStarted)); got != 1 {
		t.Fatalf("%d started records, want the one worker that ran", got)
	}
}

// TestRevisionIdleWithoutClusterConf checks RW9/SW6: a cluster absent from the
// cache makes the loop idle — no stream, no syncup — with exactly one
// "cluster conf missing" record per idle period.
func TestRevisionIdleWithoutClusterConf(t *testing.T) {
	synctest.Test(t, testRevisionIdleWithoutClusterConf)
}

func testRevisionIdleWithoutClusterConf(t *testing.T) {
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

// TestRevisionIdlesOnAnInvalidClusterConf is the twin of the test above under
// architecture.md, Common validation, for a cluster that IS in the cache but
// whose stored conf the gateway could not have written. The loop never leaves
// the gate — no stream, no syncup, no health write — because the alternative is
// worse than idling: what such a conf is missing is geometry, the bin ladder
// MD4 keys capacity by and the extent_size the dn agent formats every disk
// header with (architecture.md, Disk node), so a round computed from guessed
// values would commit the cluster to numbers nobody else holds.
//
// The bad conf is installed BEFORE the worker starts, so what this test pins
// is that the gate sits ahead of round(): the worker never dials, never opens
// a stream and never writes health in the first place. The other half of RW9's
// quiesced state — a stream and an RW7 reference that an ALREADY RUNNING
// worker hands back when its conf goes bad — cannot be observed from here,
// because there is nothing to hand back. The test below, reaching the refusal
// from a connected state, is where refuseConf's quiesce() is pinned.
//
// The record is its own, not "cluster conf missing": that string means the
// absent-cluster case (RW9), and an operator who sees it goes looking for a
// deleted cluster instead of the field that is wrong.
func TestRevisionIdlesOnAnInvalidClusterConf(t *testing.T) {
	synctest.Test(t, testRevisionIdlesOnAnInvalidClusterConf)
}

func testRevisionIdlesOnAnInvalidClusterConf(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{}
	h.fleet.addDn(t, testAddr, stub)
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	// Stored without a dn_bin_conf: proto3 hands the reader the all-zero
	// shift set, which architecture.md, DN bins, does not accept as a ladder.
	h.setClusterConf(testCid, testClusterConf(func(cc *pb.ClusterConf) {
		cc.DnBinConf = nil
	}))
	h.startDn(testAddr, 1)

	waitFor(t, "refusal record", func() bool {
		return len(h.logs.withMsg(msgInvalidStoredConf)) == 1
	})
	rec := h.logs.withMsg(msgInvalidStoredConf)[0]
	if rec["level"] != "ERROR" {
		t.Fatalf("refusal logged at %v, want ERROR", rec["level"])
	}
	if err, _ := rec["error"].(string); !strings.Contains(
		err, "dn_bin_conf",
	) {
		t.Fatalf("error = %q, want the offending field named", err)
	}
	// Several more rounds: the memo keeps it at one record per invalid period,
	// exactly as the idle arm keeps `cluster conf missing` at one.
	for i := 0; i < 3; i++ {
		h.clk.advance(common.DefaultHealthCheckInterval * time.Second)
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(h.logs.withMsg(msgInvalidStoredConf)); got != 1 {
		t.Fatalf("%d invalid stored conf records, want one per refused period",
			got)
	}
	// Not the absent-cluster record: the two must stay distinguishable.
	if got := len(h.logs.withMsg(msgClusterConfMissing)); got != 0 {
		t.Fatalf("%d cluster conf missing records, want none: the cluster IS "+
			"in the cache", got)
	}
	if stub.checkCount() != 0 || len(stub.syncups()) != 0 {
		t.Fatalf("a worker refusing its conf talked to its agent")
	}
	if got := len(h.hw.all()); got != 0 {
		t.Fatalf("a worker refusing its conf wrote health: %v", h.hw.all())
	}
	// Nothing was ever dialled, so this says the gate precedes connect(), not
	// that a held reference was released — see the doc comment.
	if refs := h.deps.conns.refs(testAddr); refs != 0 {
		t.Fatalf("a worker refused before its first round holds %d "+
			"connection references", refs)
	}
	// A usable conf is installed: the loop recovers on its next retry.
	h.defaultConf()
	h.advanceUntil("round after the conf is repaired", roundInterval,
		func() bool { return stub.checkCount() >= 1 })
}

// TestRevisionQuiescesWhenARunningConfGoesBad is the CONNECTED half of the
// refusal of architecture.md, Common validation: a worker that is already
// driving its object — one Check stream open over one RW7 connection reference
// — and whose cluster conf then becomes unusable. RW9's promise for that case
// is not merely "no new round": it is that the loop gives the stream and the
// reference BACK, so an operator who corrupted a cluster conf does not leave
// one idle gRPC connection per object pinned open for as long as it takes to
// fix it.
//
// Every assertion below is therefore about state the cold-start test cannot
// enter: it asserts refs == 1 and one live stream BEFORE the conf goes bad, so
// "refs == 0" and "the handler returned" afterwards are statements about
// refuseConf's quiesce() rather than about a worker that never dialled.
func TestRevisionQuiescesWhenARunningConfGoesBad(t *testing.T) {
	synctest.Test(t, testRevisionQuiescesWhenARunningConfGoesBad)
}

func testRevisionQuiescesWhenARunningConfGoesBad(t *testing.T) {
	h := newRevHarness(t)
	// An agent that answers every round, holding a revision the worker did
	// not ask for: the round completes and ends in RW4 step 5's SyncupDn.
	// That syncup is the test's signal that a round has FINISHED — it is
	// issued after recv() returned and stopped the round timer — so the fake
	// clock is only ever advanced while the loop is parked in wait(), and an
	// advance can never abandon a round in flight and drop its stream.
	stub := &stubDnAgent{
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			return &pb.CheckDnReply{Revision: 0}
		},
	}
	h.fleet.addDn(t, testAddr, stub)
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	h.defaultConf()
	h.startDn(testAddr, 1)

	// The first round needs no clock: run() drives one before it waits.
	waitFor(t, "the first round to finish", func() bool {
		return len(stub.syncups()) >= 1
	})
	if refs := h.deps.conns.refs(testAddr); refs != 1 {
		t.Fatalf("a running worker holds %d connection references, want 1",
			refs)
	}
	if got := stub.streamCount(); got != 1 {
		t.Fatalf("%d check streams after one round, want 1", got)
	}
	if got := stub.streamExitCount(); got != 0 {
		t.Fatalf("the stream was closed %d times before the conf went bad",
			got)
	}

	// The stored conf goes bad UNDER the running worker.
	h.setClusterConf(testCid, testClusterConf(func(cc *pb.ClusterConf) {
		cc.DnBinConf = nil
	}))
	h.advanceUntil("refusal record", roundInterval, func() bool {
		return len(h.logs.withMsg(msgInvalidStoredConf)) == 1
	})
	// RW9's quiesced state, reached from a connected one: the stream is gone
	// and the reference is handed back. The agent's CheckDn handler returning
	// is the observable half of "gone", and refuseConf's dropStream is the
	// only thing that can have ended it — the exit count was 0 above, RW4 step
	// 4's other drop is a round that failed, and the guard below says none
	// did.
	waitFor(t, "the check stream to be dropped", func() bool {
		return stub.streamExitCount() == 1
	})
	if got := len(h.logs.withMsg("check round failed")); got != 0 {
		t.Fatalf("%d rounds failed, so the drop above is not necessarily "+
			"the refusal's", got)
	}
	if refs := h.deps.conns.refs(testAddr); refs != 0 {
		t.Fatalf("a worker refusing its conf holds %d connection references",
			refs)
	}
	// And it drives nothing while refused, with one record per refused
	// period rather than one per retry.
	before := stub.checkCount()
	for i := 0; i < 3; i++ {
		h.clk.advance(roundInterval)
		time.Sleep(5 * time.Millisecond)
	}
	if got := stub.checkCount(); got != before {
		t.Fatalf("a refusing worker sent %d more check requests",
			got-before)
	}
	if got := len(h.logs.withMsg(msgInvalidStoredConf)); got != 1 {
		t.Fatalf("%d invalid stored conf records, want one per refused period",
			got)
	}

	// Repaired: the next round acquires the reference again and opens a FRESH
	// stream, because the refused one was closed rather than parked.
	h.defaultConf()
	h.advanceUntil("round after the conf is repaired", roundInterval,
		func() bool { return stub.checkCount() > before })
	if got := stub.streamCount(); got != 2 {
		t.Fatalf("%d streams, want a second one after the refusal", got)
	}
	if refs := h.deps.conns.refs(testAddr); refs != 1 {
		t.Fatalf("a recovered worker holds %d connection references, want 1",
			refs)
	}
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

// traceDnAgent is a DiskNodeAgent whose CheckDn derives each round's trace id
// the way the agents do — the request's trace_id when set, else the stream's
// — and records it beside the trace_id metadata the stream was opened with.
// Its replies name revision 0, so every round ends in a SyncupDn.
type traceDnAgent struct {
	pb.UnimplementedDiskNodeAgentServer

	mu       sync.Mutex
	streams  int
	syncups  int
	metaIds  []string
	roundIds []string
}

func (s *traceDnAgent) CheckDn(
	stream grpc.BidiStreamingServer[pb.CheckDnRequest, pb.CheckDnReply],
) error {
	s.mu.Lock()
	s.streams++
	s.mu.Unlock()
	for {
		req, err := stream.Recv()
		if err != nil {
			return nil
		}
		var metaId string
		md, _ := metadata.FromIncomingContext(stream.Context())
		if vals := md.Get(common.TraceIdMetadataKey); len(vals) > 0 {
			metaId = vals[0]
		}
		ctx := stream.Context()
		if req.GetTraceId() != "" {
			ctx = common.WithTraceId(ctx, req.GetTraceId())
		}
		roundId, _ := common.TraceIdFromCtx(ctx)
		s.mu.Lock()
		s.metaIds = append(s.metaIds, metaId)
		s.roundIds = append(s.roundIds, roundId)
		s.mu.Unlock()
		if err := stream.Send(&pb.CheckDnReply{Revision: 0}); err != nil {
			return err
		}
	}
}

// SyncupDn is the signal that a round has FINISHED: every CheckDn reply names
// a revision the worker did not ask for, so RW4 step 5 syncs after recv()
// returned and stopped the round timer.
func (s *traceDnAgent) SyncupDn(
	_ context.Context, req *pb.SyncupDnRequest,
) (*pb.SyncupDnReply, error) {
	s.mu.Lock()
	s.syncups++
	s.mu.Unlock()
	return &pb.SyncupDnReply{Revision: req.GetRevision()}, nil
}

func (s *traceDnAgent) syncupCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncups
}

func (s *traceDnAgent) snapshot() (streams int, metaIds, roundIds []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams, append([]string(nil), s.metaIds...),
		append([]string(nil), s.roundIds...)
}

// TestEachCheckRoundCarriesItsOwnTraceId pins RW10 on the long-lived Check
// stream: the stream's metadata is written once, at open, under the id of
// the round that opened it, so each later round must carry its own id in the
// request — else every agent record of round two, three, … is filed under
// round one's id.
func TestEachCheckRoundCarriesItsOwnTraceId(t *testing.T) {
	h := newRevHarness(t)
	stub := &traceDnAgent{}
	h.fleet.serve(t, testAddr, func(server *grpc.Server) {
		pb.RegisterDiskNodeAgentServer(server, stub)
	})
	h.defaultConf()
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	h.startDn(testAddr, 3)

	rounds := func() int {
		_, _, roundIds := stub.snapshot()
		return len(roundIds)
	}
	// Advance the clock only while the loop is parked in wait(): an advance
	// during a round in flight fires its round timer and drops the stream.
	waitFor(t, "the first round to finish", func() bool {
		return stub.syncupCount() >= 1 && h.clk.waiterCount() == 1
	})
	h.clk.advance(roundInterval)
	waitFor(t, "second check", func() bool { return rounds() >= 2 })
	streams, metaIds, roundIds := stub.snapshot()
	if streams != 1 {
		t.Fatalf("%d streams, want both rounds on one", streams)
	}
	if metaIds[1] != metaIds[0] || roundIds[0] != metaIds[0] {
		t.Fatalf("metadata ids %q, round ids %q: the stream must be opened "+
			"under the first round's id", metaIds, roundIds)
	}
	if roundIds[1] == roundIds[0] {
		t.Fatalf("both rounds on one stream derived trace id %q, "+
			"want one id per round", roundIds[1])
	}
	for _, id := range roundIds[:2] {
		if !strings.HasPrefix(id, seedPrefix(seedOf(1))+"-") {
			t.Fatalf("round trace id %q lacks the worker's seed prefix", id)
		}
	}
}

// captureSend records what a checkStream adapter hands the generated stream.
type captureSend[Req, Rep any] struct {
	grpc.BidiStreamingClient[Req, Rep]
	sent []*Req
}

func (c *captureSend[Req, Rep]) Send(r *Req) error {
	c.sent = append(c.sent, r)
	return nil
}

// TestEveryCheckStreamSendsTheRoundTraceId pins RW10 on all four adapters:
// the loop test above drives only the dn one, so the cn, side and cntlr
// adapters are checked here, each handed a round's trace id directly.
func TestEveryCheckStreamSendsTheRoundTraceId(t *testing.T) {
	dn := &captureSend[pb.CheckDnRequest, pb.CheckDnReply]{}
	cn := &captureSend[pb.CheckCnRequest, pb.CheckCnReply]{}
	side := &captureSend[pb.CheckSideRequest, pb.CheckSideReply]{}
	cntlr := &captureSend[pb.CheckCntlrRequest, pb.CheckCntlrReply]{}
	for _, s := range []checkStream{
		&dnCheckStream{driver: &dnDriver{}, stream: dn},
		&cnCheckStream{driver: &cnDriver{}, stream: cn},
		&sideCheckStream{driver: &sideDriver{}, stream: side},
		&cntlrCheckStream{driver: &cntlrDriver{}, stream: cntlr},
	} {
		if err := s.send("round-7", 1, false); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	for name, got := range map[string]string{
		"CheckDn":    dn.sent[0].GetTraceId(),
		"CheckCn":    cn.sent[0].GetTraceId(),
		"CheckSide":  side.sent[0].GetTraceId(),
		"CheckCntlr": cntlr.sent[0].GetTraceId(),
	} {
		if got != "round-7" {
			t.Errorf("%s request trace_id %q, want round-7", name, got)
		}
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
	synctest.Test(t, testRevisionDesiredChangeDuringRoundSyncsAtOnce)
}

func testRevisionDesiredChangeDuringRoundSyncsAtOnce(t *testing.T) {
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

// TestSyncupLeftoverLogged pins the "syncup leftover" record (log.md,
// Leftovers): a Syncup* reply carrying common.ReplyCodeLeftover was ACCEPTED —
// the desired state is stored and every wanted object converged — so it is not
// a rejection and must never be logged as one. What it adds is the agent's own
// account of what the node still holds that the desired state does not want (or
// of an enumeration that did not answer), which is the only place a lingering
// leftover becomes visible in the worker log.
func TestSyncupLeftoverLogged(t *testing.T) {
	h := newRevHarness(t)
	const details = "leftover(2): d4:dnv-...-d4-..., d0:dnv-...-d0-..."
	stub := &stubDnAgent{
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			// A revision behind, so every round issues the Syncup* below.
			return &pb.CheckDnReply{Revision: req.GetRevision() - 1}
		},
		syncupReply: func(req *pb.SyncupDnRequest) (*pb.SyncupDnReply, error) {
			return &pb.SyncupDnReply{
				Revision: req.GetRevision(),
				AgentReply: &pb.AgentReply{
					Code:    common.ReplyCodeLeftover,
					Details: details,
				},
			}, nil
		},
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	h.startDn(testAddr, 4)

	waitFor(t, "the leftover record", func() bool {
		return len(h.logs.withMsg(msgSyncupLeftover)) >= 1
	})
	rec := h.logs.withMsg(msgSyncupLeftover)[0]
	if rec["level"] != "INFO" {
		t.Fatalf("leftover logged at %v, want INFO", rec["level"])
	}
	if rec["details"] != details {
		t.Fatalf("details = %v, want the agent's leftover names", rec["details"])
	}
	if revision, _ := rec["revision"].(float64); uint64(revision) != 4 {
		t.Fatalf("revision = %v, want the synced 4", rec["revision"])
	}
	if got := rec["dn_id"]; got == nil {
		t.Fatalf("the record carries no ids: %v", rec)
	}
	// RW5's three rejection codes mean the request was NOT applied; a
	// leftover reply was, so "syncup rejected" would be wrong about it.
	if got := len(h.logs.withMsg(msgSyncupRejected)); got != 0 {
		t.Fatalf("%d 'syncup rejected' records for an accepted reply", got)
	}
}
