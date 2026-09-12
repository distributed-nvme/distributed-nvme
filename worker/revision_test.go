package worker

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
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

// setClusterConf installs a cluster conf in the RW21 cache exactly as given.
// It resolves nothing, mirroring the cache itself (§7) — which is the only
// reason a test can hand the loop a conf the gateway could not have written
// and watch it refuse.
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

// TestRevisionIdlesOnAnInvalidClusterConf is the §7 twin of the test above,
// for a cluster that IS in the cache but whose stored conf the gateway could
// not have written. The loop never leaves the gate — no stream, no syncup, no
// health write — because the alternative is worse than idling: what such a
// conf is missing is geometry, the bin ladder MD4 keys capacity by and the
// extent_size the dn agent formats every disk header with (§3.1), so a round
// computed from guessed values would commit the cluster to numbers nobody else
// holds.
//
// The bad conf is installed BEFORE the worker starts, so what this test pins
// is that the gate sits ahead of round(): the worker never dials, never opens
// a stream and never writes health in the first place. The other half of RW9's
// quiesced state — a stream and an RW7 reference that an ALREADY RUNNING
// worker hands back when its conf goes bad — cannot be observed from here,
// because there is nothing to hand back. The test below, reaching the refusal
// from a connected state, is where refuseConf's quiesce() is pinned.
//
// The record is its own, not "cluster conf missing": §14 greps that string for
// the absent-cluster case, and an operator who sees it goes looking for a
// deleted cluster instead of the field that is wrong.
func TestRevisionIdlesOnAnInvalidClusterConf(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{}
	h.fleet.addDn(t, testAddr, stub)
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	// Stored without a dn_bin_conf: proto3 hands the reader the all-zero
	// shift set, which §6.2 does not accept as a ladder.
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

// TestRevisionQuiescesWhenARunningConfGoesBad is the CONNECTED half of the §7
// refusal: a worker that is already driving its object — one Check stream
// open over one RW7 connection reference — and whose cluster conf then becomes
// unusable. RW9's promise for that case is not merely "no new round": it is
// that the loop gives the stream and the reference BACK, so an operator who
// corrupted a cluster conf does not leave one idle gRPC connection per object
// pinned open for as long as it takes to fix it.
//
// Every assertion below is therefore about state the cold-start test cannot
// enter: it asserts refs == 1 and one live stream BEFORE the conf goes bad, so
// "refs == 0" and "the handler returned" afterwards are statements about
// refuseConf's quiesce() rather than about a worker that never dialled.
func TestRevisionQuiescesWhenARunningConfGoesBad(t *testing.T) {
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
