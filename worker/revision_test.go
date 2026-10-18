package worker

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

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
	// streamCtxs is the context of every CheckDn stream, in the order
	// opened. A round the stub holds (holdRound) runs until it ends.
	streamCtxs []context.Context

	// checkReply builds the reply to one CheckDn request; nil means "say
	// nothing", which is how a round timeout is produced (RW4 step 3).
	checkReply func(req *pb.CheckDnRequest) *pb.CheckDnReply
	// holdRound makes a round checkReply says nothing to run until its
	// stream's context ends, a round not answered yet: the handler reads
	// the stream again only once a round is over, so a CloseSend reaches it
	// only then, and only the context cancel of dropStream ends a round it
	// still holds. Without it the handler goes straight back to Recv, where
	// the CloseSend alone ends it.
	holdRound bool
	// syncupReply answers one SyncupDn.
	syncupReply func(req *pb.SyncupDnRequest) (*pb.SyncupDnReply, error)
}

func (s *stubDnAgent) CheckDn(
	stream grpc.BidiStreamingServer[pb.CheckDnRequest, pb.CheckDnReply],
) error {
	s.mu.Lock()
	s.streams++
	s.streamCtxs = append(s.streamCtxs, stream.Context())
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
		hold := s.holdRound
		s.mu.Unlock()
		if build == nil {
			continue
		}
		reply := build(req)
		if reply == nil {
			if hold {
				<-stream.Context().Done()
				return stream.Context().Err()
			}
			continue
		}
		if err := stream.Send(stamped(reply)); err != nil {
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
		return stamped(&pb.SyncupDnReply{Revision: req.GetRevision()}), nil
	}
	reply, err := answer(req)
	if reply != nil {
		reply = syncupStamped(reply)
	}
	return reply, err
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

// lastStreamCtx is the context of the CheckDn stream opened last, nil before
// the first.
func (s *stubDnAgent) lastStreamCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.streamCtxs) == 0 {
		return nil
	}
	return s.streamCtxs[len(s.streamCtxs)-1]
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
			if err := stream.Send(stamped(reply)); err != nil {
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
	return stamped(&pb.SyncupCnReply{Revision: req.GetRevision()}), nil
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
// intervals are DefaultHealthCheckInterval, what the gateway resolves an
// omitted one to.
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
// Its replies name revision 0, so every round ends in a SyncupDn, whose
// trace_id metadata it records too.
type traceDnAgent struct {
	pb.UnimplementedDiskNodeAgentServer

	mu        sync.Mutex
	streams   int
	syncups   int
	metaIds   []string
	roundIds  []string
	syncupIds []string
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
		stamp := testStamp()
		stamp.ObsTraceId = roundId
		if err := stream.Send(&pb.CheckDnReply{
			Revision: 0, ObsStamp: stamp,
		}); err != nil {
			return err
		}
	}
}

// SyncupDn is the signal that a round has FINISHED: every CheckDn reply names
// a revision the worker did not ask for, so RW4 step 5 syncs after recv()
// returned and stopped the round timer.
func (s *traceDnAgent) SyncupDn(
	ctx context.Context, req *pb.SyncupDnRequest,
) (*pb.SyncupDnReply, error) {
	var id string
	md, _ := metadata.FromIncomingContext(ctx)
	if vals := md.Get(common.TraceIdMetadataKey); len(vals) > 0 {
		id = vals[0]
	}
	s.mu.Lock()
	s.syncups++
	s.syncupIds = append(s.syncupIds, id)
	s.mu.Unlock()
	return stamped(&pb.SyncupDnReply{Revision: req.GetRevision()}), nil
}

func (s *traceDnAgent) syncupTraceIds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.syncupIds...)
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
// round one's id. And the Syncup* a round issues (RW4 step 5) runs under that
// round's id, which is what joins the agent's record of a verdict to the
// worker's re-send it brought (log.md, Leftovers).
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
	waitFor(t, "the second round's syncup", func() bool {
		return stub.syncupCount() >= 2
	})
	if got := stub.syncupTraceIds()[:2]; !slices.Equal(got, roundIds[:2]) {
		t.Fatalf("the rounds' syncups ran under %q, want each under its "+
			"round's id %q", got, roundIds[:2])
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
//
// It also pins RW6's order: the given-up round's stream is dropped BEFORE the
// Syncup* is sent. The round's reply would answer the request built from the
// superseded revision, so the stream goes with the round and is never reused
// (RW4 step 4); the agent's Syncup* cancels the object's background read
// itself (dnagent.md SH30). The stub holds the round unanswered (holdRound),
// so only the drop's cancel ends it, not its CloseSend. The stub's SyncupDn
// gives the stream a bounded wait to end — time that, in the synctest bubble,
// passes only once nothing else can run — and records whether it did.
func TestRevisionDesiredChangeDuringRoundSyncsAtOnce(t *testing.T) {
	synctest.Test(t, testRevisionDesiredChangeDuringRoundSyncsAtOnce)
}

func testRevisionDesiredChangeDuringRoundSyncsAtOnce(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{
		// The agent never answers this round, and runs it until the stream
		// is cancelled.
		checkReply: func(*pb.CheckDnRequest) *pb.CheckDnReply { return nil },
		holdRound:  true,
	}
	var (
		mu sync.Mutex
		// roundLive is, per SyncupDn, whether the check stream it found
		// was still live when the syncup was answered.
		roundLive []bool
	)
	stub.syncupReply = func(
		req *pb.SyncupDnRequest,
	) (*pb.SyncupDnReply, error) {
		live := false
		if sctx := stub.lastStreamCtx(); sctx != nil {
			select {
			case <-sctx.Done():
			case <-time.After(time.Second):
				live = true
			}
		}
		mu.Lock()
		roundLive = append(roundLive, live)
		mu.Unlock()
		return &pb.SyncupDnReply{Revision: req.GetRevision()}, nil
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
	waitFor(t, "the syncup's answer", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(roundLive) >= 1
	})
	mu.Lock()
	live := roundLive[0]
	mu.Unlock()
	if live {
		t.Fatalf("the syncup reached the agent while the given-up round's " +
			"stream was still live: the stream outlived its round")
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

// ---------------------------------------------------------------------------
// The observation's age (HL7) and new observations (RW23)
// ---------------------------------------------------------------------------

// obsRecorder wraps a revision worker's real driver and records what the
// loop hands it: a copy of every reply observe sees, in order, and the count
// of unreachable verdicts.
type obsRecorder struct {
	objDriver

	mu           sync.Mutex
	replies      []replyState
	unreachables int
}

func (r *obsRecorder) observe(ctx context.Context, reply *replyState) {
	r.mu.Lock()
	r.replies = append(r.replies, *reply)
	r.mu.Unlock()
	r.objDriver.observe(ctx, reply)
}

func (r *obsRecorder) unreachable(ctx context.Context) {
	r.mu.Lock()
	r.unreachables++
	r.mu.Unlock()
	r.objDriver.unreachable(ctx)
}

// seen returns the replies observe saw and the unreachable verdicts so far.
func (r *obsRecorder) seen() ([]replyState, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]replyState(nil), r.replies...), r.unreachables
}

// startRecordedDn is startDn with the dn driver wrapped in an obsRecorder.
func (h *revHarness) startRecordedDn(
	addrPort string, revision uint64,
) (*revWorker, *obsRecorder, *dnDriver) {
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
	rec := &obsRecorder{}
	var driver *dnDriver
	w := startRevWorker(params, func(host *revWorker) objDriver {
		driver = newDnDriver(params, host).(*dnDriver)
		rec.objDriver = driver
		return rec
	})
	h.t.Cleanup(w.stop)
	return w, rec, driver
}

// stampAt is the stamp of a reply answering at answer from an observation
// made at obs under traceId.
func stampAt(obs, answer time.Time, traceId string) *pb.ObsStamp {
	return &pb.ObsStamp{
		ObsTime:    timestamppb.New(obs),
		ObsTraceId: traceId,
		AnswerTime: timestamppb.New(answer),
	}
}

// recNumber reads a numeric attribute of a JSON record; ok is false when the
// record lacks it.
func recNumber(rec map[string]any, key string) (float64, bool) {
	v, ok := rec[key].(float64)
	return v, ok
}

// TestObservationAgeFrom pins the age HL7 judges (ageFrom): the answer time
// less the later of the observation's time and from, the answer time of the
// first round after a pause, all from the reply's stamp and the loop. A
// negative difference reads zero, a reply with no observation time counts
// from from alone, and a reply with no answer time has no age at all. An
// unset time stays unset through fold, not the Unix epoch.
func TestObservationAgeFrom(t *testing.T) {
	at := testObsBase
	ms := time.Millisecond
	for _, c := range []struct {
		name              string
		obs, answer, from time.Time
		age               time.Duration
		known             bool
	}{
		{"an observation after from", at, at.Add(1500 * ms),
			at.Add(-time.Second), 1500 * ms, true},
		{"an observation as from", at, at.Add(1500 * ms), at, 1500 * ms,
			true},
		{"an observation before from", at, at.Add(1500 * ms),
			at.Add(time.Second), 500 * ms, true},
		{"an answer as the observation ends", at, at, at.Add(-time.Second),
			0, true},
		{"an answer before the observation", at, at.Add(-time.Second),
			at.Add(-2 * time.Second), 0, true},
		{"an answer before from", at, at.Add(time.Second),
			at.Add(2 * time.Second), 0, true},
		{"no observation time", time.Time{}, at.Add(1500 * ms), at,
			1500 * ms, true},
		{"no observation time, an answer before from", time.Time{}, at,
			at.Add(time.Second), 0, true},
		{"no answer time", at, time.Time{}, at.Add(-time.Second), 0, false},
		{"no time at all", time.Time{}, time.Time{}, at, 0, false},
	} {
		r := &replyState{obsTime: c.obs, answerTime: c.answer}
		if age, known := r.ageFrom(c.from); age != c.age ||
			known != c.known {
			t.Errorf("%s: age %v, known %t; want %v, %t", c.name, age, known,
				c.age, c.known)
		}
	}
	r := newReplyState(nil, 0, false,
		&pb.ObsStamp{AnswerTime: timestamppb.New(at)}, nil)
	if !r.obsTime.IsZero() || !r.saveTime.IsZero() || !r.answerTime.Equal(at) {
		t.Fatalf("an unset observation and save time folded to %v, %v; "+
			"answer %v", r.obsTime, r.saveTime, r.answerTime)
	}
	if age, known := r.ageFrom(at.Add(-time.Second)); !known ||
		age != time.Second {
		t.Fatalf("a folded reply with no observation time: age %v, known "+
			"%t; want %v counted from from", age, known, time.Second)
	}
}

// TestStaleObservationIsAMissedRound pins HL7's stale verdict through the
// real dn loop: a reply that came in time but answers from an observation
// older than ObsMaxAgeIntervals of the interval is a missed round in every
// way. The stream drops, the rows read UNKNOWN, the health epoch is set, and
// no Syncup* is issued though the reply's revision is behind; the failed
// round names the cause and the late observation its age. The next round
// opens a fresh stream, and the first fresh observation clears the epoch.
//
// The first round answers from an observation that ended before it, which
// starts the age's count and gives no verdict (HL7's pause rule); the second
// answers from the read the first started, a clean one, and the agent's reads
// stop ending there. The stale third, which a pause cannot precede, is judged
// by that observation's own age.
func TestStaleObservationIsAMissedRound(t *testing.T) {
	synctest.Test(t, testStaleObservationIsAMissedRound)
}

func testStaleObservationIsAMissedRound(t *testing.T) {
	h := newRevHarness(t)
	ms := time.Millisecond
	age := common.ObsMaxAgeIntervals*roundInterval + ms
	t0 := testNodeBase
	read := t0.Add(ms) // the end of the read the first round started
	var mu sync.Mutex
	at := t0 // the agent's latest answer
	var rounds atomic.Int64
	var recovered atomic.Bool
	stub := &stubDnAgent{}
	stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
		mu.Lock()
		defer mu.Unlock()
		n := rounds.Add(1)
		fresh := &pb.CheckDnReply{
			Revision: req.GetRevision(),
			DnInfo:   &pb.DnInfo{DiskInfo: resOk("disk")},
		}
		switch {
		case recovered.Load():
			obs := at.Add(ms)
			at = at.Add(roundInterval)
			fresh.ObsStamp = stampAt(obs, at, "read-fresh")
			return fresh
		case n == 1:
			fresh.ObsStamp = stampAt(t0.Add(-ms), t0, "read-before")
			return fresh
		case n == 2:
			// Within an interval of its observation: not late.
			at = t0.Add(roundInterval)
			fresh.ObsStamp = stampAt(read, at, "read-fresh")
			return fresh
		case n == 3:
			at = read.Add(age)
		default:
			at = at.Add(roundInterval)
		}
		// A revision behind, which a fresh reply would re-sync on, and its
		// info left out as unchanged.
		return &pb.CheckDnReply{
			Revision: req.GetRevision() - 1,
			ObsStamp: stampAt(read, at, "read-trace"),
		}
	})
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, dnTestConf())
	_, rec, driver := h.startRecordedDn(testAddr, 5)

	h.advanceUntil("the first round", roundInterval, func() bool {
		return rounds.Load() >= 1
	})
	if got := h.hw.all(); len(got) != 0 {
		t.Fatalf("the first round, answering from an observation that "+
			"ended before it, wrote %+v", got)
	}
	h.advanceUntil("the first verdict", roundInterval, func() bool {
		return rounds.Load() >= 2
	})
	if got := h.hw.all(); len(got) != 1 || got[0].epoch != 0 {
		t.Fatalf("the round answering from the first round's read wrote "+
			"%+v, want a clear", got)
	}
	if got := h.logs.withMsg(msgLateObservation); len(got) != 0 {
		t.Fatalf("a round within an interval of its observation is logged "+
			"late: %v", got)
	}
	h.advanceUntil("the stale round's verdict", roundInterval, func() bool {
		return len(h.hw.all()) == 2
	})
	synctest.Wait()
	if got := h.hw.all()[1]; got.record != healthRecordDn || got.epoch == 0 {
		t.Fatalf("the stale round wrote %+v, want a dn epoch", got)
	}
	if got := len(stub.syncups()); got != 0 {
		t.Fatalf("the stale round issued %d Syncups, want none", got)
	}
	if got := driver.info().GetDiskInfo().GetStatus(); got !=
		pb.ResStatus_RES_STATUS_UNKNOWN {
		t.Fatalf("disk_info reads %v after the stale round, want UNKNOWN",
			got)
	}
	if got := stub.streamExitCount(); got != 1 {
		t.Fatalf("%d streams ended after the stale round, want 1", got)
	}
	replies, unreachables := rec.seen()
	if len(replies) != 1 || unreachables != 1 {
		t.Fatalf("observe saw %d replies and %d unreachable verdicts; want "+
			"the fresh round's alone and one", len(replies), unreachables)
	}
	failed := h.logs.withMsg("check round failed")
	if len(failed) != 1 || failed[0]["error"] != "observation too old" {
		t.Fatalf("failed rounds %v, want one stale", failed)
	}
	if ms, _ := recNumber(failed[0], "age_ms"); ms != float64(age.Milliseconds()) {
		t.Fatalf("the failed round's age_ms = %v, want %d", ms,
			age.Milliseconds())
	}
	late := h.logs.withMsg(msgLateObservation)
	if len(late) != 1 {
		t.Fatalf("%d late observation records, want 1", len(late))
	}
	for key, want := range map[string]any{
		"cluster_id":   float64(testCid),
		"dn_id":        float64(testDnId),
		"stale":        true,
		"age_ms":       float64(age.Milliseconds()),
		"interval_ms":  float64(roundInterval.Milliseconds()),
		"obs_trace_id": "read-trace",
	} {
		if got := late[0][key]; got != want {
			t.Errorf("late observation %s = %v, want %v", key, got, want)
		}
	}
	if _, ok := late[0]["converge_ms"]; ok {
		t.Errorf("a late observation with no converge running carries "+
			"converge_ms: %v", late[0])
	}
	wantNoObs(t, late[0], false)

	h.advanceUntil("a fresh stream", roundInterval, func() bool {
		return stub.streamCount() >= 2
	})
	recovered.Store(true)
	h.advanceUntil("the clear", roundInterval, func() bool {
		return len(h.hw.all()) == 3
	})
	if got := h.hw.all()[2]; got.epoch != 0 {
		t.Fatalf("the first fresh round after the stale ones wrote %+v, "+
			"want a clear", got)
	}
}

// TestObservationAgeBound pins where HL7 draws its two lines, on the third
// round of a dn loop: the first starts the age's count (HL7's pause rule),
// from an observation that ended before it, and the second answers from the
// read the first started, one interval on, so the third, which a pause cannot
// precede, is judged by its observation's own age. An observation older than one interval is logged as
// late, one older than ObsMaxAgeIntervals of them is stale, and both
// records name the age judged. A reply with no observation time is late once
// more than an interval, and stale once more than the bound, has passed
// since the first round, the age both records name; one with no stamp at
// all is both, with no age. The late record of either says the reply has no
// observation (no_obs), which no other late record says. A
// converge in flight is reported but is not stale by itself, and a reply
// refusing the object as unknown carries no observation and is not judged: it
// re-syncs, which is what makes the object known.
func TestObservationAgeBound(t *testing.T) {
	ms := time.Millisecond
	bound := common.ObsMaxAgeIntervals * roundInterval
	// Each builder is the third round's stamp, given start, the first
	// round's answer, and obs, the end of the read it started, which the
	// second answered from.
	aged := func(age time.Duration) func(time.Time, time.Time) *pb.ObsStamp {
		return func(_, obs time.Time) *pb.ObsStamp {
			return stampAt(obs, obs.Add(age), "read-b")
		}
	}
	answerOnly := func(
		after time.Duration,
	) func(time.Time, time.Time) *pb.ObsStamp {
		return func(start, _ time.Time) *pb.ObsStamp {
			return answerStamp(start.Add(after))
		}
	}
	converging := func(start, obs time.Time) *pb.ObsStamp {
		stamp := aged(roundInterval+ms)(start, obs)
		stamp.ConvergeStart = timestamppb.New(
			stamp.GetAnswerTime().AsTime().Add(-5 * time.Second))
		return stamp
	}
	newer := func(_, obs time.Time) *pb.ObsStamp {
		answer := obs.Add(2 * roundInterval)
		return stampAt(answer.Add(time.Second), answer, "read-b")
	}
	for _, c := range []struct {
		name  string
		stamp func(start, obs time.Time) *pb.ObsStamp
		code  uint32
		late  bool
		stale bool
		// ageMs and convergeMs are the late record's attributes and
		// failedMs the failed round's age, -1 when a record must not carry
		// it; noObs is whether the late record says the reply has no
		// observation.
		ageMs      int64
		convergeMs int64
		failedMs   int64
		noObs      bool
	}{
		{"at one interval", aged(roundInterval), 0, false, false, -1, -1, -1,
			false},
		{"just above one interval", aged(roundInterval + ms), 0,
			true, false, 2001, -1, -1, false},
		{"just under the bound", aged(bound - ms), 0, true, false, 5999, -1,
			-1, false},
		{"at the bound", aged(bound), 0, true, false, 6000, -1, -1, false},
		{"just over the bound", aged(bound + ms), 0, true, true, 6001, -1,
			6001, false},
		{"an observation that ends after the answer", newer, 0,
			false, false, -1, -1, -1, false},
		{"no observation time within the bound",
			answerOnly(2 * roundInterval), 0, true, false, 4000, -1, -1,
			true},
		{"no observation time past the bound", answerOnly(bound + ms), 0,
			true, true, 6001, -1, 6001, true},
		{"no stamp",
			func(_, _ time.Time) *pb.ObsStamp { return &pb.ObsStamp{} },
			0, true, true, -1, -1, -1, true},
		{"a converge in flight", converging, 0, true, false, 2001, 5000, -1,
			false},
		{"an unknown object", answerOnly(2 * roundInterval),
			common.ReplyCodeUnknownObject, false, false, -1, -1, -1, false},
	} {
		t.Run(c.name, inBubble(func(t *testing.T) {
			h := newRevHarness(t)
			start := testNodeBase
			obs := start.Add(ms)
			var rounds atomic.Int64
			stub := &stubDnAgent{}
			stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
				reply := &pb.CheckDnReply{Revision: req.GetRevision()}
				switch rounds.Add(1) {
				case 1:
					reply.ObsStamp = stampAt(start.Add(-ms), start, "read-a")
				case 2:
					reply.ObsStamp = stampAt(obs, obs.Add(roundInterval),
						"read-a")
				default:
					reply.AgentReply = &pb.AgentReply{Code: c.code}
					reply.ObsStamp = c.stamp(start, obs)
				}
				return reply
			})
			h.fleet.addDn(t, testAddr, stub)
			h.defaultConf()
			h.seedDnConf(testAddr, dnTestConf())
			h.startDn(testAddr, 5)

			h.advanceUntil("the two rounds before the case's", roundInterval,
				func() bool { return rounds.Load() >= 2 })
			before := h.hw.all()
			if len(before) != 1 || before[0].epoch != 0 {
				t.Fatalf("the two rounds before the case's wrote %v, want "+
					"one clear", before)
			}
			if n := len(h.logs.withMsg(msgLateObservation)) +
				len(h.logs.withMsg("check round failed")); n != 0 {
				t.Fatalf("the two rounds before the case's left %d late "+
					"or failed records", n)
			}
			h.advanceUntil("the case's round", roundInterval,
				func() bool { return rounds.Load() >= 3 })
			writes := h.hw.all()[len(before):]
			if c.code != 0 {
				if len(writes) != 0 {
					t.Fatalf("the unknown object's round wrote %v", writes)
				}
				if got := len(stub.syncups()); got != 1 {
					t.Fatalf("the unknown object's round issued %d "+
						"Syncups, want its re-sync", got)
				}
			} else {
				set := len(writes) == 1 && writes[0].epoch != 0
				if set != c.stale || len(writes) > 1 {
					t.Fatalf("the round wrote %v; set the epoch: %t, want "+
						"%t", writes, set, c.stale)
				}
			}
			if c.stale {
				if got := len(stub.syncups()); got != 0 {
					t.Fatalf("a stale round issued %d Syncups", got)
				}
			}
			late := h.logs.withMsg(msgLateObservation)
			if c.late != (len(late) == 1) || len(late) > 1 {
				t.Fatalf("%d late observation records, want late %t",
					len(late), c.late)
			}
			failed := h.logs.withMsg("check round failed")
			if c.stale != (len(failed) == 1) || len(failed) > 1 {
				t.Fatalf("%d failed rounds, want stale %t", len(failed),
					c.stale)
			}
			wantAttr := func(rec map[string]any, key string, want int64) {
				t.Helper()
				got, ok := recNumber(rec, key)
				if want < 0 && ok {
					t.Errorf("%s carries %s = %v, want none", rec["msg"],
						key, got)
				}
				if want >= 0 && (!ok || got != float64(want)) {
					t.Errorf("%s %s = %v, want %d", rec["msg"], key,
						rec[key], want)
				}
			}
			if c.late {
				if got := late[0]["stale"]; got != c.stale {
					t.Errorf("late observation stale = %v, want %t", got,
						c.stale)
				}
				wantAttr(late[0], "age_ms", c.ageMs)
				wantAttr(late[0], "interval_ms", roundInterval.Milliseconds())
				wantAttr(late[0], "converge_ms", c.convergeMs)
				wantNoObs(t, late[0], c.noObs)
			}
			if c.stale {
				if got := failed[0]["error"]; got != errStaleObservation.Error() {
					t.Errorf("failed round error = %v", got)
				}
				wantAttr(failed[0], "age_ms", c.failedMs)
			}
		}))
	}
}

// TestRoundAfterASyncupIsNotLate pins HL7's late record around a Syncup*:
// the round right after one answers, as a rule, from the observation that
// reply carried, a whole interval and more after it was made, since the
// Syncup* cancelled the read the round before started and the loop waits an
// interval after the reply. That age is the loop's own making, so the round
// is not logged late, after a round's re-send (RW4 step 5) or an immediate
// syncup (RW6) alike. A later round still answering from it is late in
// earnest, and a stale one right after a Syncup* is logged and fails as any.
//
// The agent's answers never lie more than the bound apart, so no round but
// the first starts the age's count (HL7's pause rule). The stale round comes
// right after a re-send the agent refused: an accepted one stores a new
// observation the round after it answers from, which a round can only find
// stale after a pause; a refused one stores nothing, and its reply carries
// the observation the rounds before it answered from.
func TestRoundAfterASyncupIsNotLate(t *testing.T) {
	synctest.Test(t, testRoundAfterASyncupIsNotLate)
}

func testRoundAfterASyncupIsNotLate(t *testing.T) {
	h := newRevHarness(t)
	ms := time.Millisecond
	var mu sync.Mutex
	at := testNodeBase   // the agent's latest answer
	var synced time.Time // the observation the latest Syncup carried
	var syncs int
	stub := &stubDnAgent{
		syncupReply: func(req *pb.SyncupDnRequest) (*pb.SyncupDnReply, error) {
			mu.Lock()
			defer mu.Unlock()
			syncs++
			at = at.Add(ms)
			if syncs == 2 {
				// The second re-send is refused: it stores nothing.
				return &pb.SyncupDnReply{
					AgentReply: &pb.AgentReply{
						Code:    common.ReplyCodeInvalidConf,
						Details: "refused",
					},
					Revision: req.GetRevision(),
					ObsStamp: stampAt(synced, at, "syncup"),
				}, nil
			}
			synced = at
			return &pb.SyncupDnReply{
				Revision: req.GetRevision(),
				ObsStamp: stampAt(synced, synced, "syncup"),
			}, nil
		},
	}
	// fromSyncup answers req at the revision behind from the latest Syncup's
	// observation, age after it was made.
	fromSyncup := func(
		req *pb.CheckDnRequest, behind uint64, age time.Duration,
	) *pb.CheckDnReply {
		at = synced.Add(age)
		return &pb.CheckDnReply{
			Revision: req.GetRevision() - behind,
			ObsStamp: stampAt(synced, at, "syncup"),
		}
	}
	// fresh answers req at the revision behind an interval after the
	// agent's latest answer, from the read that ended a millisecond after
	// it; one behind re-sends the Syncup.
	fresh := func(req *pb.CheckDnRequest, behind uint64) *pb.CheckDnReply {
		obs := at.Add(ms)
		at = at.Add(roundInterval)
		return &pb.CheckDnReply{
			Revision: req.GetRevision() - behind,
			ObsStamp: stampAt(obs, at, "read"),
		}
	}
	var rounds atomic.Int64
	stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
		mu.Lock()
		defer mu.Unlock()
		switch rounds.Add(1) {
		case 1, 8:
			return fresh(req, 1)
		case 2:
			return fromSyncup(req, 0, roundInterval+ms)
		case 3:
			// No read has ended since: late in earnest, not stale. A
			// revision behind, it re-sends the Syncup, which is refused.
			return fromSyncup(req, 1, 2*roundInterval+ms)
		case 4:
			return fromSyncup(req, 0,
				common.ObsMaxAgeIntervals*roundInterval+ms)
		case 6:
			// Held: the immediate syncup overtakes it.
			return nil
		case 7:
			return fromSyncup(req, 0, roundInterval+ms)
		case 9:
			// No reply: the round times out.
			return nil
		case 10:
			return fromSyncup(req, 0, 2*roundInterval+ms)
		default:
			return fresh(req, 0)
		}
	})
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, dnTestConf())
	w := h.startDn(testAddr, 5)

	late := func() []map[string]any { return h.logs.withMsg(msgLateObservation) }
	failed := func() int { return len(h.logs.withMsg("check round failed")) }

	h.advanceUntil("the round after the re-send", roundInterval, func() bool {
		return rounds.Load() >= 2
	})
	synctest.Wait()
	if got := len(stub.syncups()); got != 1 {
		t.Fatalf("%d Syncups after the first round, want its re-send", got)
	}
	if got := late(); len(got) != 0 {
		t.Fatalf("the round after the re-send, answering from its "+
			"observation one interval on, is logged late: %v", got)
	}

	h.advanceUntil("a second round from the same observation", roundInterval,
		func() bool { return rounds.Load() >= 3 && len(late()) >= 1 })
	synctest.Wait()
	if got := late(); len(got) != 1 || got[0]["stale"] != false {
		t.Fatalf("late records %v, want one, not stale, for the second "+
			"round answering from the Syncup's observation", got)
	}
	if got := len(stub.syncups()); got != 2 {
		t.Fatalf("%d Syncups after the second round from the same "+
			"observation, want its re-send too", got)
	}

	h.advanceUntil("a stale round right after a re-send", roundInterval,
		func() bool { return rounds.Load() >= 4 && failed() >= 1 })
	synctest.Wait()
	if got := late(); len(got) != 2 || got[1]["stale"] != true {
		t.Fatalf("late records %v, want a stale one for the round right "+
			"after the second re-send", got)
	}
	if got := failed(); got != 1 {
		t.Fatalf("%d failed rounds, want the stale one", got)
	}

	// An immediate syncup overtakes a held round (RW6).
	h.advanceUntil("a clean round", roundInterval, func() bool {
		return rounds.Load() >= 5
	})
	stub.mu.Lock()
	stub.holdRound = true
	stub.mu.Unlock()
	h.advanceUntil("the held round", roundInterval, func() bool {
		return rounds.Load() >= 6
	})
	synctest.Wait()
	n := len(stub.syncups())
	w.update(desiredState{revision: 6, handle: testAddr})
	waitFor(t, "the immediate syncup", func() bool {
		return len(stub.syncups()) > n
	})
	synctest.Wait()
	h.advanceUntil("the round after the immediate syncup", roundInterval,
		func() bool { return rounds.Load() >= 7 })
	synctest.Wait()
	if got := late(); len(got) != 2 {
		t.Fatalf("late records %v; the round after the immediate syncup, "+
			"answering from its observation one interval on, is logged "+
			"late", got[2:])
	}
	if got := failed(); got != 1 {
		t.Fatalf("%d failed rounds after the immediate syncup, want the "+
			"stale one alone", got)
	}

	// A round that fails after a re-send ends the leave: the round after
	// it, still answering from the Syncup's observation, is late.
	stub.mu.Lock()
	stub.holdRound = false
	stub.mu.Unlock()
	h.advanceUntil("the round after a re-send and a timed-out round",
		roundInterval, func() bool {
			return rounds.Load() >= 10 && len(late()) >= 3
		})
	synctest.Wait()
	if got := failed(); got != 2 {
		t.Fatalf("%d failed rounds, want the timed-out one too", got)
	}
	if got := late(); len(got) != 3 || got[2]["stale"] != false {
		t.Fatalf("late records %v, want one, not stale, for the round "+
			"after the timed-out one", got)
	}
	if got := len(stub.syncups()); got != 4 {
		t.Fatalf("%d Syncups, want four", got)
	}
}

// ---------------------------------------------------------------------------
// The pause rule (HL7): the age's count starts at the first round after a
// pause in the loop's rounds
// ---------------------------------------------------------------------------

// tooOld returns the records of the rounds that failed on an observation
// too old (HL7), leaving out the other failed rounds.
func tooOld(logs *logCapture) []map[string]any {
	var out []map[string]any
	for _, rec := range logs.withMsg("check round failed") {
		if rec["error"] == errStaleObservation.Error() {
			out = append(out, rec)
		}
	}
	return out
}

// roundsSeen counts the round replies observe saw, leaving out the Syncup*
// replies.
func roundsSeen(rec *obsRecorder) int {
	replies, _ := rec.seen()
	n := 0
	for _, r := range replies {
		if r.fromRound {
			n++
		}
	}
	return n
}

// wantAgeMs fails t unless the failed round rec names age.
func wantAgeMs(t *testing.T, rec map[string]any, age time.Duration) {
	t.Helper()
	if got, ok := recNumber(rec, "age_ms"); !ok ||
		got != float64(age.Milliseconds()) {
		t.Fatalf("the failed round's age_ms = %v, want %d", rec["age_ms"],
			age.Milliseconds())
	}
}

// wantLateAgeMs fails t unless the late observation record rec names age,
// the age HL7 judged.
func wantLateAgeMs(t *testing.T, rec map[string]any, age time.Duration) {
	t.Helper()
	if got, ok := recNumber(rec, "age_ms"); !ok ||
		got != float64(age.Milliseconds()) {
		t.Fatalf("the late observation's age_ms = %v, want %d, the age "+
			"judged", rec["age_ms"], age.Milliseconds())
	}
}

// wantNoObs fails t unless the late observation record rec says the reply
// has no observation exactly when noObs: no_obs true then, and no no_obs at
// all otherwise.
func wantNoObs(t *testing.T, rec map[string]any, noObs bool) {
	t.Helper()
	got, ok := rec["no_obs"]
	if noObs && got != true {
		t.Fatalf("the late observation of a reply with no observation "+
			"carries no_obs = %v, want true", got)
	}
	if !noObs && ok {
		t.Fatalf("the late observation of a reply with an observation "+
			"carries no_obs = %v", got)
	}
}

// TestFirstRoundStartsTheAgeCount pins HL7's pause rule on a loop's first
// round: no round of the loop asked the agent to read before it, so the
// observation it answers from, which ended before it — here an hour before —
// is neither stale nor logged late, and gives no health verdict either: no
// epoch is set or cleared. It still drives the re-sync its revision calls
// for, and it starts the age's count. A later round answering from the read
// the first round started is judged. Later rounds still answering from the
// observation before the first give no verdict either, and are late once
// more than an interval, and stale once more than ObsMaxAgeIntervals of
// them, has passed since the first round's answer; the late records and the
// failed round name that age, not the observation's own.
func TestFirstRoundStartsTheAgeCount(t *testing.T) {
	ms := time.Millisecond
	bound := common.ObsMaxAgeIntervals * roundInterval
	t0 := testNodeBase
	old := t0.Add(-time.Hour)
	ok := &pb.DnInfo{DiskInfo: resOk("disk")}

	t.Run("a read that does not end", inBubble(func(t *testing.T) {
		h := newRevHarness(t)
		// Each round's answer after the first's, the last for any round
		// after it; the first round is a revision behind.
		after := []time.Duration{
			0, roundInterval, roundInterval + ms, bound, bound + ms,
		}
		var rounds atomic.Int64
		stub := &stubDnAgent{
			// The re-send is refused: it stores nothing, and its reply
			// carries the observation the rounds answer from.
			syncupReply: func(
				req *pb.SyncupDnRequest,
			) (*pb.SyncupDnReply, error) {
				return &pb.SyncupDnReply{
					AgentReply: &pb.AgentReply{
						Code:    common.ReplyCodeInvalidConf,
						Details: "refused",
					},
					Revision: req.GetRevision() - 1,
					ObsStamp: stampAt(old, t0.Add(ms), "read-old"),
				}, nil
			},
		}
		stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			n := int(rounds.Add(1))
			reply := &pb.CheckDnReply{
				Revision: req.GetRevision(),
				DnInfo:   proto.CloneOf(ok),
				ObsStamp: stampAt(old, t0.Add(after[min(n, len(after))-1]),
					"read-old"),
			}
			if n == 1 {
				reply.Revision--
			}
			return reply
		})
		h.fleet.addDn(t, testAddr, stub)
		h.defaultConf()
		h.seedDnConf(testAddr, dnTestConf())
		_, rec, _ := h.startRecordedDn(testAddr, 5)
		late := func() []map[string]any {
			return h.logs.withMsg(msgLateObservation)
		}
		round := func(n int64) {
			t.Helper()
			h.advanceUntil("the next round", roundInterval, func() bool {
				return rounds.Load() >= n
			})
		}

		round(1)
		if got := h.hw.all(); len(got) != 0 {
			t.Fatalf("the first round, answering from an observation an "+
				"hour old, wrote %v", got)
		}
		if got := len(stub.syncups()); got != 1 {
			t.Fatalf("%d Syncups after the first round, a revision "+
				"behind, want its re-send", got)
		}
		if got := late(); len(got) != 0 {
			t.Fatalf("the first round, answering from an observation an "+
				"hour old, is logged late: %v", got)
		}

		round(2)
		if got := late(); len(got) != 0 {
			t.Fatalf("a round an interval after the first is logged "+
				"late: %v", got)
		}
		round(3)
		if got := late(); len(got) != 1 || got[0]["stale"] != false {
			t.Fatalf("late records %v, want one, not stale, for a round "+
				"more than an interval after the first", got)
		}
		wantLateAgeMs(t, late()[0], roundInterval+ms)
		round(4)
		if got := late(); len(got) != 2 || got[1]["stale"] != false {
			t.Fatalf("late records %v, want a second, not stale, for a "+
				"round the bound after the first", got)
		}
		wantLateAgeMs(t, late()[1], bound)
		if got := len(h.logs.withMsg("check round failed")); got != 0 {
			t.Fatalf("%d failed rounds within the bound of the first", got)
		}
		if got := h.hw.all(); len(got) != 0 {
			t.Fatalf("rounds answering from an observation older than the "+
				"first round wrote %v", got)
		}
		if got := roundsSeen(rec); got != 0 {
			t.Fatalf("observe saw %d rounds answering from an observation "+
				"older than the first round", got)
		}

		round(5)
		if got := late(); len(got) != 3 || got[2]["stale"] != true {
			t.Fatalf("late records %v, want a stale one for a round past "+
				"the bound after the first", got)
		}
		wantLateAgeMs(t, late()[2], bound+ms)
		stale := tooOld(h.logs)
		if len(stale) != 1 {
			t.Fatalf("%d rounds failed on their observation's age, want "+
				"one", len(stale))
		}
		wantAgeMs(t, stale[0], bound+ms)
		if got := h.hw.all(); len(got) != 1 || got[0].epoch == 0 {
			t.Fatalf("health writes %v, want the stale round's epoch", got)
		}
		if got := len(stub.syncups()); got != 1 {
			t.Fatalf("%d Syncups, want the first round's alone", got)
		}
	}))

	t.Run("the read the first round started", inBubble(func(t *testing.T) {
		h := newRevHarness(t)
		var rounds atomic.Int64
		stub := &stubDnAgent{}
		stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			stamp := stampAt(old, t0, "read-old")
			if rounds.Add(1) > 1 {
				stamp = stampAt(t0.Add(ms), t0.Add(roundInterval), "read-new")
			}
			return &pb.CheckDnReply{
				Revision: req.GetRevision(),
				DnInfo:   proto.CloneOf(ok),
				ObsStamp: stamp,
			}
		})
		h.fleet.addDn(t, testAddr, stub)
		h.defaultConf()
		h.seedDnConf(testAddr, dnTestConf())
		_, rec, _ := h.startRecordedDn(testAddr, 5)

		h.advanceUntil("the first round", roundInterval, func() bool {
			return rounds.Load() >= 1
		})
		if got := h.hw.all(); len(got) != 0 {
			t.Fatalf("the first round, answering from an observation an "+
				"hour old, wrote %v", got)
		}
		h.advanceUntil("the second round", roundInterval, func() bool {
			return rounds.Load() >= 2
		})
		if got := h.hw.all(); len(got) != 1 || got[0].epoch != 0 {
			t.Fatalf("health writes %v, want the clear of the round "+
				"answering from the first round's read", got)
		}
		if got := roundsSeen(rec); got != 1 {
			t.Fatalf("observe saw %d rounds, want the second alone", got)
		}
	}))
}

// TestAGapIsNoPause pins HL7's pause rule on a running loop: only its start,
// an idle or a refused conf, and a move are pauses, so a gap between two of
// its rounds — rounds that got no reply, or a Syncup* that timed out while
// the agent held the object behind a hung converge — leaves the age's count
// where the loop's first round started it. The round after the gap is
// judged by its observation's own age and, here, is stale, the late record
// and the failed round naming that age.
func TestAGapIsNoPause(t *testing.T) {
	ms := time.Millisecond
	t0 := testNodeBase
	read := t0.Add(ms) // the end of the read the first round started
	second := t0.Add(roundInterval)
	for _, c := range []struct {
		name string
		// gap is the agent's clock from the second round's answer to the
		// round after the gap, timeouts how many rounds got no reply in
		// between, and resend whether the second round's re-send fails,
		// the agent having held it for the gap.
		gap      time.Duration
		timeouts int64
		resend   bool
	}{
		{"rounds that got no reply", 5 * roundInterval, 2, false},
		{"a re-send that timed out",
			common.DefaultWorkerSyncupTimeout*time.Second + roundInterval,
			0, true},
	} {
		t.Run(c.name, inBubble(func(t *testing.T) {
			h := newRevHarness(t)
			after := 3 + c.timeouts // the round after the gap
			var rounds atomic.Int64
			stub := &stubDnAgent{
				syncupReply: func(
					*pb.SyncupDnRequest,
				) (*pb.SyncupDnReply, error) {
					return nil, errors.New("context deadline exceeded")
				},
			}
			stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
				n := rounds.Add(1)
				stamp := stampAt(read, second.Add(c.gap), "read")
				switch {
				case n == 1:
					stamp = stampAt(t0.Add(-ms), t0, "read-before")
				case n == 2:
					stamp = stampAt(read, second, "read")
				case n < after:
					return nil
				}
				reply := &pb.CheckDnReply{
					Revision: req.GetRevision(),
					DnInfo:   &pb.DnInfo{DiskInfo: resOk("disk")},
					ObsStamp: stamp,
				}
				if c.resend && n >= 2 {
					// A converge holds the object from just after the read.
					reply.Revision--
					stamp.ConvergeStart = timestamppb.New(read.Add(ms))
				}
				return reply
			})
			h.fleet.addDn(t, testAddr, stub)
			h.defaultConf()
			h.seedDnConf(testAddr, dnTestConf())
			h.startDn(testAddr, 5)

			h.advanceUntil("the second round", roundInterval,
				func() bool { return rounds.Load() >= 2 })
			if got := h.hw.all(); len(got) != 1 || got[0].epoch != 0 {
				t.Fatalf("health writes %v, want the second round's clear",
					got)
			}
			h.advanceUntil("the round after the gap", roundInterval,
				func() bool { return rounds.Load() >= after })
			age := second.Add(c.gap).Sub(read)
			stale := tooOld(h.logs)
			if len(stale) != 1 {
				t.Fatalf("%d rounds failed on their observation's age, want "+
					"the round after the gap", len(stale))
			}
			wantAgeMs(t, stale[0], age)
			late := h.logs.withMsg(msgLateObservation)
			if len(late) != 1 || late[0]["stale"] != true {
				t.Fatalf("late records %v, want a stale one for the round "+
					"after the gap", late)
			}
			wantLateAgeMs(t, late[0], age)
			if got := len(h.logs.withMsg("check round failed")) -
				len(stale); int64(got) != c.timeouts {
				t.Fatalf("%d rounds failed otherwise, want the %d that got "+
					"no reply", got, c.timeouts)
			}
			if got := h.hw.all(); got[len(got)-1].epoch == 0 {
				t.Fatalf("health writes %v, want the epoch set", got)
			}
			want := 0
			if c.resend {
				want = 1
			}
			if got := len(stub.syncups()); got != want {
				t.Fatalf("%d Syncups, want %d: none on the stale round", got,
					want)
			}
		}))
	}
}

// TestAnIdleRestartsTheAgeCount pins HL7's pause rule across a quiesced
// loop: an idle on a missing cluster conf (RW9) and a refused stored conf
// (architecture.md, Common validation) open no stream and so ask the agent
// for no read, and the first round after either starts the age's count
// again. It answers from the observation the loop's last round answered
// from, now older than the bound, and is neither stale nor late and gives no
// verdict; later rounds still answering from it are late and stale counted
// from that round's answer.
func TestAnIdleRestartsTheAgeCount(t *testing.T) {
	ms := time.Millisecond
	bound := common.ObsMaxAgeIntervals * roundInterval
	t0 := testNodeBase
	read := t0.Add(ms) // the end of the read the first round started
	resumed := t0.Add(roundInterval + bound)
	for _, c := range []struct {
		name   string
		pause  func(h *revHarness)
		record string
	}{
		{"a missing cluster conf", func(h *revHarness) {
			h.deps.conf.mu.Lock()
			delete(h.deps.conf.entries, testCid)
			h.deps.conf.mu.Unlock()
		}, msgClusterConfMissing},
		{"a refused cluster conf", func(h *revHarness) {
			h.setClusterConf(testCid, testClusterConf(func(cc *pb.ClusterConf) {
				cc.DnBinConf = nil
			}))
		}, msgInvalidStoredConf},
	} {
		t.Run(c.name, inBubble(func(t *testing.T) {
			h := newRevHarness(t)
			var rounds atomic.Int64
			stub := &stubDnAgent{}
			stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
				obs, at := read, resumed.Add(bound+ms)
				switch rounds.Add(1) {
				case 1:
					obs, at = t0.Add(-ms), t0
				case 2:
					at = t0.Add(roundInterval)
				case 3:
					at = resumed
				case 4:
					at = resumed.Add(roundInterval + ms)
				}
				return &pb.CheckDnReply{
					Revision: req.GetRevision(),
					DnInfo:   &pb.DnInfo{DiskInfo: resOk("disk")},
					ObsStamp: stampAt(obs, at, "read"),
				}
			})
			h.fleet.addDn(t, testAddr, stub)
			h.defaultConf()
			h.seedDnConf(testAddr, dnTestConf())
			_, rec, _ := h.startRecordedDn(testAddr, 5)
			late := func() []map[string]any {
				return h.logs.withMsg(msgLateObservation)
			}

			h.advanceUntil("the second round", roundInterval,
				func() bool { return rounds.Load() >= 2 })
			if got := h.hw.all(); len(got) != 1 || got[0].epoch != 0 {
				t.Fatalf("health writes %v, want the second round's clear",
					got)
			}
			c.pause(h)
			h.advanceUntil("the quiesced loop", roundInterval, func() bool {
				return len(h.logs.withMsg(c.record)) >= 1
			})
			if got := rounds.Load(); got != 2 {
				t.Fatalf("%d rounds while the loop is quiesced, want none "+
					"after the second", got)
			}
			h.defaultConf()

			h.advanceUntil("the round after the pause", roundInterval,
				func() bool { return rounds.Load() >= 3 })
			if got := tooOld(h.logs); len(got) != 0 {
				t.Fatalf("the round after the pause failed on its "+
					"observation's age: %v", got)
			}
			if got := late(); len(got) != 0 {
				t.Fatalf("the round after the pause is logged late: %v", got)
			}
			if got := roundsSeen(rec); got != 1 {
				t.Fatalf("observe saw %d rounds, want the second alone", got)
			}
			if got := h.hw.all(); len(got) != 1 {
				t.Fatalf("health writes %v, want the second round's clear "+
					"alone", got)
			}

			h.advanceUntil("a late round", roundInterval,
				func() bool { return rounds.Load() >= 4 })
			if got := late(); len(got) != 1 || got[0]["stale"] != false {
				t.Fatalf("late records %v, want one, not stale", got)
			}
			wantLateAgeMs(t, late()[0], roundInterval+ms)
			h.advanceUntil("a stale round", roundInterval,
				func() bool { return rounds.Load() >= 5 })
			stale := tooOld(h.logs)
			if len(stale) != 1 {
				t.Fatalf("%d rounds failed on their observation's age, want "+
					"the one past the bound after the round after the pause",
					len(stale))
			}
			wantAgeMs(t, stale[0], bound+ms)
			if got := late(); len(got) != 2 || got[1]["stale"] != true {
				t.Fatalf("late records %v, want a stale one", got)
			}
			wantLateAgeMs(t, late()[1], bound+ms)
		}))
	}
}

// TestRoundWithNoObservation pins HL7's reading of a reply of an object the
// agent holds but has no observation of (its observation time unset, its
// code not ReplyCodeUnknownObject): until the bound has passed since the
// first round after a pause it carries no health verdict — no epoch set or
// cleared, no row marked, the stream kept — but its revision, none, still
// brings the re-sync of RW4 step 5. Once the bound has passed with still no
// observation, the round is stale. Each late record of such a round says the
// reply has no observation (no_obs), beside the age counted from that first
// round. The agent refuses every re-sync here, so it stores nothing and the
// replies go on carrying no observation.
func TestRoundWithNoObservation(t *testing.T) {
	ms := time.Millisecond
	bound := common.ObsMaxAgeIntervals * roundInterval
	t0 := testNodeBase
	// harness starts a dn loop whose agent answers round n with reply(n)
	// and refuses every SyncupDn, which stores nothing, with a reply that
	// carries no stamp.
	harness := func(
		t *testing.T, reply func(n int64) *pb.CheckDnReply,
	) (*revHarness, *stubDnAgent, *obsRecorder, *dnDriver, *atomic.Int64) {
		h := newRevHarness(t)
		var rounds atomic.Int64
		stub := &stubDnAgent{
			syncupReply: func(
				req *pb.SyncupDnRequest,
			) (*pb.SyncupDnReply, error) {
				return &pb.SyncupDnReply{
					AgentReply: &pb.AgentReply{
						Code:    common.ReplyCodeInvalidConf,
						Details: "refused",
					},
				}, nil
			},
		}
		stub.setCheckReply(func(*pb.CheckDnRequest) *pb.CheckDnReply {
			return reply(rounds.Add(1))
		})
		h.fleet.addDn(t, testAddr, stub)
		h.defaultConf()
		h.seedDnConf(testAddr, dnTestConf())
		_, rec, driver := h.startRecordedDn(testAddr, 5)
		return h, stub, rec, driver, &rounds
	}
	// none answers at at from no observation, at no revision.
	none := func(at time.Time) *pb.CheckDnReply {
		return &pb.CheckDnReply{ObsStamp: answerStamp(at)}
	}
	unreachables := func(rec *obsRecorder) int {
		_, n := rec.seen()
		return n
	}

	t.Run("from the loop's first round", inBubble(func(t *testing.T) {
		after := []time.Duration{0, roundInterval + ms, bound, bound + ms}
		h, stub, rec, _, rounds := harness(t, func(n int64) *pb.CheckDnReply {
			return none(t0.Add(after[min(int(n), len(after))-1]))
		})
		late := func() []map[string]any {
			return h.logs.withMsg(msgLateObservation)
		}
		// The age each late record names: the age judged, counted from
		// the first round's answer.
		lateAges := []time.Duration{roundInterval + ms, bound}
		for n, wantLate := range []int{0, 1, 2} {
			h.advanceUntil("the next round", roundInterval, func() bool {
				return rounds.Load() >= int64(n+1)
			})
			if got := h.hw.all(); len(got) != 0 {
				t.Fatalf("round %d with no observation wrote %v", n+1, got)
			}
			if got := roundsSeen(rec); got != 0 {
				t.Fatalf("observe saw %d rounds with no observation", got)
			}
			if got := len(stub.syncups()); got != n+1 {
				t.Fatalf("%d Syncups after %d rounds with no observation, "+
					"want one re-sync each", got, n+1)
			}
			if got := len(h.logs.withMsg("check round failed")); got != 0 {
				t.Fatalf("%d failed rounds within the bound", got)
			}
			if got := stub.streamExitCount(); got != 0 {
				t.Fatalf("%d streams ended within the bound", got)
			}
			got := late()
			if len(got) != wantLate {
				t.Fatalf("late records %v after round %d, want %d", got,
					n+1, wantLate)
			}
			for i, r := range got {
				if r["stale"] != false {
					t.Fatalf("a round within the bound is logged stale: %v",
						r)
				}
				wantLateAgeMs(t, r, lateAges[i])
				wantNoObs(t, r, true)
			}
		}

		h.advanceUntil("the round past the bound", roundInterval,
			func() bool { return rounds.Load() >= 4 })
		stale := tooOld(h.logs)
		if len(stale) != 1 {
			t.Fatalf("%d rounds failed on their observation's age, want "+
				"the round past the bound", len(stale))
		}
		wantAgeMs(t, stale[0], bound+ms)
		if got := late(); len(got) != 3 || got[2]["stale"] != true {
			t.Fatalf("late records %v, want a stale one", got)
		}
		wantLateAgeMs(t, late()[2], bound+ms)
		wantNoObs(t, late()[2], true)
		if got := h.hw.all(); len(got) != 1 || got[0].epoch == 0 {
			t.Fatalf("health writes %v, want the stale round's epoch", got)
		}
		if got := len(stub.syncups()); got != 3 {
			t.Fatalf("%d Syncups, want none on the stale round", got)
		}
		if got := unreachables(rec); got != 1 {
			t.Fatalf("%d unreachable verdicts, want the stale round's", got)
		}
	}))

	t.Run("after a judged round", inBubble(func(t *testing.T) {
		// The first round answers from an observation that ended before
		// it; the second from the read the first started, with a clean
		// row; the third from none; the fourth not at all; the fifth and
		// sixth from none again, the bound and just past it after the
		// first.
		h, stub, rec, driver, rounds := harness(t,
			func(n int64) *pb.CheckDnReply {
				switch n {
				case 1, 2:
					stamp := stampAt(t0.Add(-ms), t0, "read-before")
					if n == 2 {
						stamp = stampAt(t0.Add(ms), t0.Add(roundInterval),
							"read")
					}
					return &pb.CheckDnReply{
						Revision: 5,
						DnInfo:   &pb.DnInfo{DiskInfo: resOk("disk")},
						ObsStamp: stamp,
					}
				case 3:
					return none(t0.Add(2 * roundInterval))
				case 4:
					return nil
				case 5:
					return none(t0.Add(bound))
				default:
					return none(t0.Add(bound + ms))
				}
			})
		disk := func() pb.ResStatus {
			return driver.info().GetDiskInfo().GetStatus()
		}

		h.advanceUntil("the round with no observation", roundInterval,
			func() bool { return rounds.Load() >= 3 })
		if got := h.hw.all(); len(got) != 1 || got[0].epoch != 0 {
			t.Fatalf("health writes %v, want the second round's clear alone",
				got)
		}
		if got := disk(); got != pb.ResStatus_RES_STATUS_OK {
			t.Fatalf("disk_info reads %v after a round with no "+
				"observation, want OK", got)
		}
		if got := stub.streamExitCount(); got != 0 {
			t.Fatalf("%d streams ended after a round with no observation",
				got)
		}
		if got := roundsSeen(rec); got != 1 {
			t.Fatalf("observe saw %d rounds, want the second alone", got)
		}
		if got := len(stub.syncups()); got != 1 {
			t.Fatalf("%d Syncups, want the re-sync of the round with no "+
				"observation", got)
		}

		h.advanceUntil("the timed-out round", roundInterval, func() bool {
			return len(h.hw.all()) >= 2
		})
		if got := h.hw.all(); len(got) != 2 || got[1].epoch == 0 {
			t.Fatalf("health writes %v, want the timeout's epoch", got)
		}
		h.advanceUntil("a round with no observation after the timeout",
			roundInterval, func() bool { return rounds.Load() >= 5 })
		if got := h.hw.all(); len(got) != 2 {
			t.Fatalf("health writes %v; a round with no observation "+
				"cleared the timeout's epoch", got)
		}
		if got := roundsSeen(rec); got != 1 {
			t.Fatalf("observe saw %d rounds, want the second alone", got)
		}
		if got := len(stub.syncups()); got != 2 {
			t.Fatalf("%d Syncups, want a re-sync of each round with no "+
				"observation", got)
		}
		if got := tooOld(h.logs); len(got) != 0 {
			t.Fatalf("a round with no observation the bound after the "+
				"first failed on its age: %v", got)
		}

		h.advanceUntil("the round past the bound", roundInterval,
			func() bool { return rounds.Load() >= 6 })
		stale := tooOld(h.logs)
		if len(stale) != 1 {
			t.Fatalf("%d rounds failed on their observation's age, want "+
				"the round past the bound", len(stale))
		}
		wantAgeMs(t, stale[0], bound+ms)
		if got := len(stub.syncups()); got != 2 {
			t.Fatalf("%d Syncups, want none on the stale round", got)
		}
		if got := unreachables(rec); got != 2 {
			t.Fatalf("%d unreachable verdicts, want the timeout's and the "+
				"stale round's", got)
		}
	}))
}

// TestAMoveForgetsTheAgeCount pins HL7's pause rule across a move (RW13):
// the age's count belongs to the endpoint, as the latest observation seen
// does (RW23), since the node there is another node with another clock. The
// first round at the new endpoint starts the count again, though the agent
// there answers it from an observation older than the old count's start; the
// next round, answering from the read it started, is judged.
func TestAMoveForgetsTheAgeCount(t *testing.T) {
	synctest.Test(t, testAMoveForgetsTheAgeCount)
}

func testAMoveForgetsTheAgeCount(t *testing.T) {
	h := newRevHarness(t)
	const newAddr = "dn1:9520"
	ms := time.Millisecond
	bound := common.ObsMaxAgeIntervals * roundInterval
	t0 := testNodeBase
	ok := &pb.DnInfo{DiskInfo: resOk("disk")}
	var oldRounds atomic.Int64
	old := &stubDnAgent{}
	old.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
		stamp := stampAt(t0.Add(-ms), t0, "old")
		if oldRounds.Add(1) > 1 {
			stamp = stampAt(t0.Add(ms), t0.Add(roundInterval), "old")
		}
		return &pb.CheckDnReply{
			Revision: req.GetRevision(),
			DnInfo:   proto.CloneOf(ok),
			ObsStamp: stamp,
		}
	})
	// The first round at the new endpoint answers the bound after the old
	// endpoint's last answer, from an observation an hour before the old
	// count's start, which the old count would find stale. Later rounds there
	// answer from the read it started.
	first := t0.Add(roundInterval + bound)
	var movedRounds atomic.Int64
	moved := &stubDnAgent{}
	moved.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
		stamp := stampAt(t0.Add(-time.Hour), first, "moved-old")
		if movedRounds.Add(1) > 1 {
			stamp = stampAt(first.Add(ms), first.Add(roundInterval), "moved")
		}
		return &pb.CheckDnReply{
			Revision: req.GetRevision(),
			DnInfo:   proto.CloneOf(ok),
			ObsStamp: stamp,
		}
	})
	h.fleet.addDn(t, testAddr, old)
	h.fleet.addDn(t, newAddr, moved)
	h.defaultConf()
	h.seedDnConf(testAddr, dnTestConf())
	h.seedDnConf(newAddr, dnTestConf())
	w, rec, _ := h.startRecordedDn(testAddr, 5)

	h.advanceUntil("two rounds at the old endpoint", roundInterval,
		func() bool { return old.checkCount() >= 2 })
	w.update(desiredState{revision: 5, handle: newAddr})
	h.advanceUntil("a round at the new endpoint", roundInterval, func() bool {
		return moved.checkCount() >= 1
	})
	if got := h.logs.withMsg("check round failed"); len(got) != 0 {
		t.Fatalf("failed rounds %v: the old endpoint's count was kept", got)
	}
	if got := h.logs.withMsg(msgLateObservation); len(got) != 0 {
		t.Fatalf("late records %v: the old endpoint's count was kept", got)
	}
	h.advanceUntil("a second round at the new endpoint", roundInterval,
		func() bool { return moved.checkCount() >= 2 })
	replies, _ := rec.seen()
	last := replies[len(replies)-1]
	if !last.fromRound || last.obsTraceId != "moved" {
		t.Fatalf("the round answering from the read the new endpoint's "+
			"first round started gave no verdict: the last reply observed "+
			"is %+v", last)
	}
	if got := h.logs.withMsg("check round failed"); len(got) != 0 {
		t.Fatalf("failed rounds %v at the new endpoint", got)
	}
}

// TestASyncupLeavesTheAgeCount pins HL7's pause rule around a Syncup*: its
// reply is no round, so it does not start the age's count again, however
// long the agent took to answer it. Every round here answers a revision
// behind from one observation, which ended before the first round, and the
// stub refuses every re-send, which stores nothing. It still stamps each
// refusal with that observation and an answer time, which no live agent
// sends (architecture.md, Live-state reporting): the stamp is there only to
// hand the worker a Syncup* answer time to ignore. After quick re-sends the
// round past the bound after the first is stale, counted from the first;
// after a re-send the stub took longer than the bound to answer, which is no
// pause, the round right after it is stale, counted from the first too.
func TestASyncupLeavesTheAgeCount(t *testing.T) {
	ms := time.Millisecond
	bound := common.ObsMaxAgeIntervals * roundInterval
	for _, c := range []struct {
		name string
		// rounds is each round's answer after t0, the last for any round
		// after it; took is how long the stub took to answer the first
		// re-send, each later one taking a millisecond.
		rounds []time.Duration
		took   time.Duration
		// stale is the first round that fails on its observation's age,
		// and age the age it names.
		stale int64
		age   time.Duration
	}{
		{"after quick re-sends",
			[]time.Duration{0, roundInterval, bound + ms}, ms, 3, bound + ms},
		{"after a re-send longer than the bound",
			[]time.Duration{0, bound + ms + roundInterval}, bound + ms, 2,
			bound + ms + roundInterval},
	} {
		t.Run(c.name, inBubble(func(t *testing.T) {
			h := newRevHarness(t)
			t0 := testNodeBase
			obs := t0.Add(-ms)
			var mu sync.Mutex
			var at time.Time // the agent's latest round answer
			var syncs int
			var rounds atomic.Int64
			stub := &stubDnAgent{
				syncupReply: func(
					req *pb.SyncupDnRequest,
				) (*pb.SyncupDnReply, error) {
					mu.Lock()
					defer mu.Unlock()
					syncs++
					took := ms
					if syncs == 1 {
						took = c.took
					}
					return &pb.SyncupDnReply{
						AgentReply: &pb.AgentReply{
							Code:    common.ReplyCodeInvalidConf,
							Details: "refused",
						},
						Revision: req.GetRevision() - 1,
						// No live agent stamps a refusal; this
						// one carries the answer time to ignore.
						ObsStamp: stampAt(obs, at.Add(took), "read"),
					}, nil
				},
			}
			stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
				mu.Lock()
				defer mu.Unlock()
				n := min(int(rounds.Add(1)), len(c.rounds))
				at = t0.Add(c.rounds[n-1])
				return &pb.CheckDnReply{
					Revision: req.GetRevision() - 1,
					DnInfo:   &pb.DnInfo{DiskInfo: resOk("disk")},
					ObsStamp: stampAt(obs, at, "read"),
				}
			})
			h.fleet.addDn(t, testAddr, stub)
			h.defaultConf()
			h.seedDnConf(testAddr, dnTestConf())
			h.startDn(testAddr, 5)

			h.advanceUntil("the round before the stale one", roundInterval,
				func() bool { return rounds.Load() >= c.stale-1 })
			if got := tooOld(h.logs); len(got) != 0 {
				t.Fatalf("a round before the stale one failed on its "+
					"observation's age: %v", got)
			}
			if got := len(stub.syncups()); int64(got) != c.stale-1 {
				t.Fatalf("%d Syncups before the stale round, want a "+
					"re-send of each round", got)
			}
			h.advanceUntil("the stale round", roundInterval,
				func() bool { return rounds.Load() >= c.stale })
			stale := tooOld(h.logs)
			if len(stale) != 1 {
				t.Fatalf("%d rounds failed on their observation's age, want "+
					"the round past the bound", len(stale))
			}
			wantAgeMs(t, stale[0], c.age)
			if got := len(stub.syncups()); int64(got) != c.stale-1 {
				t.Fatalf("%d Syncups, want none on the stale round", got)
			}
		}))
	}
}

// TestNewObservationsCount pins RW23 on the loop: a round's reply is new only
// when its observation time is later than every one the loop has seen, in a
// round's reply or a Syncup*'s; a Syncup* reply's observation is seen but
// never new itself, so a round answering from it afterwards is not new
// either.
func TestNewObservationsCount(t *testing.T) {
	synctest.Test(t, testNewObservationsCount)
}

func testNewObservationsCount(t *testing.T) {
	h := newRevHarness(t)
	t1, t2, t3, t4 := nextObsTime(), nextObsTime(), nextObsTime(), nextObsTime()
	// Each round answers from the script's observation; the third is a
	// revision behind, so a Syncup* follows it, answering from t3.
	script := []struct {
		obs    time.Time
		behind bool
	}{{t1, false}, {t1, false}, {t2, true}, {t3, false}, {t4, false}}
	var rounds atomic.Int64
	stub := &stubDnAgent{
		syncupReply: func(req *pb.SyncupDnRequest) (*pb.SyncupDnReply, error) {
			return &pb.SyncupDnReply{
				Revision: req.GetRevision(),
				ObsStamp: stampAt(t3, t3, "syncup"),
			}, nil
		},
	}
	stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
		step := script[min(int(rounds.Add(1))-1, len(script)-1)]
		reply := &pb.CheckDnReply{
			Revision: req.GetRevision(),
			ObsStamp: stampAt(step.obs, step.obs, "read"),
		}
		if step.behind {
			reply.Revision--
		}
		return reply
	})
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, dnTestConf())
	_, rec, _ := h.startRecordedDn(testAddr, 5)

	h.advanceUntil("five rounds and the Syncup", roundInterval, func() bool {
		replies, _ := rec.seen()
		return len(replies) >= 6
	})
	replies, unreachables := rec.seen()
	type seen struct{ fromRound, newObs bool }
	want := []seen{
		{true, true},   // t1, the first observation
		{true, false},  // t1 again
		{true, true},   // t2
		{false, false}, // the Syncup*'s t3: seen, never new
		{true, false},  // t3, seen on the Syncup* reply
		{true, true},   // t4
	}
	for i, w := range want {
		got := seen{replies[i].fromRound, replies[i].newObs}
		if got != w {
			t.Errorf("reply %d (obs %v): fromRound %t, newObs %t; want %t, %t",
				i, replies[i].obsTime, got.fromRound, got.newObs, w.fromRound,
				w.newObs)
		}
	}
	if unreachables != 0 {
		t.Fatalf("%d unreachable verdicts, want none", unreachables)
	}
	// A round repeating t4 is not new.
	h.advanceUntil("a round repeating t4", roundInterval, func() bool {
		replies, _ := rec.seen()
		return len(replies) >= 7
	})
	replies, _ = rec.seen()
	if replies[6].newObs {
		t.Fatalf("a round repeating t4 is new")
	}
}

// TestObservationsSeenResetOnAMove pins RW23's reset: the latest observation
// time a loop has seen belongs to its endpoint, so when the node moves to
// another one (RW13) the first round there is new even though the new node's
// clock reads earlier than the old one's.
func TestObservationsSeenResetOnAMove(t *testing.T) {
	synctest.Test(t, testObservationsSeenResetOnAMove)
}

func testObservationsSeenResetOnAMove(t *testing.T) {
	h := newRevHarness(t)
	const newAddr = "dn1:9520"
	ahead := testObsBase.Add(time.Hour)
	var aheadCount atomic.Int64
	old := &stubDnAgent{}
	old.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
		at := ahead.Add(time.Duration(aheadCount.Add(1)) * time.Millisecond)
		return &pb.CheckDnReply{
			Revision: req.GetRevision(),
			ObsStamp: stampAt(at, at, "old"),
		}
	})
	moved := &stubDnAgent{}
	moved.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
		return &pb.CheckDnReply{Revision: req.GetRevision()}
	})
	h.fleet.addDn(t, testAddr, old)
	h.fleet.addDn(t, newAddr, moved)
	h.defaultConf()
	h.seedDnConf(testAddr, dnTestConf())
	h.seedDnConf(newAddr, dnTestConf())
	w, rec, _ := h.startRecordedDn(testAddr, 5)

	h.advanceUntil("two rounds at the old endpoint", roundInterval,
		func() bool { return old.checkCount() >= 2 })
	before, _ := rec.seen()
	w.update(desiredState{revision: 5, handle: newAddr})
	h.advanceUntil("a round at the new endpoint", roundInterval, func() bool {
		return moved.checkCount() >= 1 && func() bool {
			replies, _ := rec.seen()
			for _, r := range replies[len(before):] {
				if r.fromRound {
					return true
				}
			}
			return false
		}()
	})
	replies, _ := rec.seen()
	for _, r := range replies[len(before):] {
		if !r.fromRound {
			continue
		}
		if !r.obsTime.Before(ahead) {
			t.Fatalf("the new endpoint's observation %v is not behind the "+
				"old one's %v", r.obsTime, ahead)
		}
		if !r.newObs {
			t.Fatal("the first round at the new endpoint is not new: the " +
				"old endpoint's observation time was kept")
		}
		return
	}
}

// TestSyncupResultNamesTheObservation pins the syncup result record of a
// round's re-send (RW10): it names the trace id of the observation the round
// answered from beside the call's own trace id, and gives the time from the
// agent's save of the request to the observation its reply carries, the
// converge's length. An immediate syncup (RW6) names no observation, and a
// reply that saved nothing gives no length.
func TestSyncupResultNamesTheObservation(t *testing.T) {
	synctest.Test(t, testSyncupResultNamesTheObservation)
}

func testSyncupResultNamesTheObservation(t *testing.T) {
	h := newRevHarness(t)
	var saved atomic.Bool
	saved.Store(true)
	stub := &stubDnAgent{
		syncupReply: func(req *pb.SyncupDnRequest) (*pb.SyncupDnReply, error) {
			obs := nextObsTime().Add(time.Hour)
			reply := &pb.SyncupDnReply{
				Revision: req.GetRevision(),
				ObsStamp: stampAt(obs, obs, "syncup"),
			}
			if saved.Load() {
				reply.SaveTime = timestamppb.New(
					obs.Add(-1500 * time.Millisecond))
			}
			return reply, nil
		},
	}
	stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
		return &pb.CheckDnReply{
			Revision: req.GetRevision() - 1,
			ObsStamp: agedStamp(0, "read-7"),
		}
	})
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, dnTestConf())
	w := h.startDn(testAddr, 5)

	waitFor(t, "the re-send's record", func() bool {
		return len(h.logs.withMsg(msgSyncupResult)) >= 1
	})
	rec := h.logs.withMsg(msgSyncupResult)[0]
	if got := rec["obs_trace_id"]; got != "read-7" {
		t.Errorf("the re-send's obs_trace_id = %v, want read-7", got)
	}
	if got, _ := rec["trace_id"].(string); got == "" || got == "read-7" {
		t.Errorf("the re-send's trace_id = %q, want the round's own", got)
	}
	if got, ok := recNumber(rec, "converge_ms"); !ok || got != 1500 {
		t.Errorf("converge_ms = %v, want 1500", rec["converge_ms"])
	}

	// An immediate syncup, whose reply saved nothing.
	stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
		return &pb.CheckDnReply{Revision: req.GetRevision()}
	})
	saved.Store(false)
	synctest.Wait()
	n := len(h.logs.withMsg(msgSyncupResult))
	w.update(desiredState{revision: 6, handle: testAddr})
	waitFor(t, "the immediate syncup's record", func() bool {
		return len(h.logs.withMsg(msgSyncupResult)) > n
	})
	for _, rec := range h.logs.withMsg(msgSyncupResult)[n:] {
		if got, _ := recNumber(rec, "revision"); got != 6 {
			continue
		}
		if _, ok := rec["obs_trace_id"]; ok {
			t.Errorf("an immediate syncup names an observation: %v", rec)
		}
		if _, ok := rec["converge_ms"]; ok {
			t.Errorf("a reply that saved nothing gives a length: %v", rec)
		}
		return
	}
	t.Fatal("no syncup result at revision 6")
}
