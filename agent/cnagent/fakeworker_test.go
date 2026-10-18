package cnagent

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The fake worker
// ---------------------------------------------------------------------------

// dialCnAgent serves the ControllerNodeAgent over bufconn with the grpc.md
// server interceptors, exactly as cmd/dnv-agent wires it, and raises both
// directions' message limit to GrpcMaxMsgSize as every dnv endpoint does.
func dialCnAgent(t *testing.T, srv *CnAgentServer) pb.ControllerNodeAgentClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
		grpc.MaxRecvMsgSize(common.GrpcMaxMsgSize),
		grpc.MaxSendMsgSize(common.GrpcMaxMsgSize),
	)
	pb.RegisterControllerNodeAgentServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(
			func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(common.GrpcMaxMsgSize),
			grpc.MaxCallSendMsgSize(common.GrpcMaxMsgSize)),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		grpcServer.Stop()
	})
	return pb.NewControllerNodeAgentClient(conn)
}

// fakeWorker is the worker's half of the ControllerNodeAgent surface, played
// by the test: it sends the syncups and the check rounds a worker sends, as
// real gRPC calls over the in-memory connection dialCnAgent serves. It opens
// one check stream per object and keeps it open across rounds, as the worker
// does.
type fakeWorker struct {
	t      *testing.T
	client pb.ControllerNodeAgentClient
	// cntlrs holds the open CheckCntlr streams by sp id; cn is the CheckCn
	// stream, nil until the first CN round.
	cntlrs map[uint64]pb.ControllerNodeAgent_CheckCntlrClient
	cn     pb.ControllerNodeAgent_CheckCnClient
	ctx    context.Context
	cancel context.CancelFunc
	// callTimeout bounds each unary call: scenarioCallTimeout, or
	// fullShapeCallTimeout once a full shape sets it before its first call.
	callTimeout time.Duration
}

const (
	// scenarioCallTimeout bounds a scenario case's unary call, generous for
	// a loaded machine, so a hung converge fails its case.
	scenarioCallTimeout = 20 * time.Second
	// fullShapeCallTimeout bounds a full shape's unary call: a full node's
	// syncups wait on each other's locks, and the widest cntlr's one
	// converge takes most of the case's bound under the race detector. It
	// stays inside go test's default timeout, so a hung converge fails its
	// case, not the package.
	fullShapeCallTimeout = 5 * time.Minute
)

func newFakeWorker(t *testing.T, srv *CnAgentServer) *fakeWorker {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &fakeWorker{
		t:           t,
		client:      dialCnAgent(t, srv),
		cntlrs:      make(map[uint64]pb.ControllerNodeAgent_CheckCntlrClient),
		ctx:         ctx,
		cancel:      cancel,
		callTimeout: scenarioCallTimeout,
	}
}

// close ends the worker's streams, as a worker's connection to an agent that
// exits ends.
func (w *fakeWorker) close() {
	w.cancel()
}

// callCtx is one unary call's deadline, the worker's callTimeout.
func (w *fakeWorker) callCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(w.ctx, w.callTimeout)
}

func (w *fakeWorker) syncupCn(req *pb.SyncupCnRequest) *pb.SyncupCnReply {
	w.t.Helper()
	ctx, cancel := w.callCtx()
	defer cancel()
	reply, err := w.client.SyncupCn(ctx, req)
	if err != nil {
		w.t.Fatalf("SyncupCn rev %d: %v", req.GetRevision(), err)
	}
	return reply
}

func (w *fakeWorker) syncupCntlr(
	req *pb.SyncupCntlrRequest,
) *pb.SyncupCntlrReply {
	w.t.Helper()
	ctx, cancel := w.callCtx()
	defer cancel()
	reply, err := w.client.SyncupCntlr(ctx, req)
	if err != nil {
		w.t.Fatalf("SyncupCntlr %s rev %d: %v",
			cntlrPointerText(req.GetCntlrPointer()), req.GetRevision(), err)
	}
	return reply
}

// trySyncupCntlr is syncupCntlr returning its failure, a reply with a code
// other than zero included, instead of failing the test.
func (w *fakeWorker) trySyncupCntlr(req *pb.SyncupCntlrRequest) error {
	ctx, cancel := w.callCtx()
	defer cancel()
	reply, err := w.client.SyncupCntlr(ctx, req)
	if err != nil {
		return fmt.Errorf("SyncupCntlr %s rev %d: %w",
			cntlrPointerText(req.GetCntlrPointer()), req.GetRevision(), err)
	}
	if code := reply.GetAgentReply().GetCode(); code != 0 {
		return fmt.Errorf("SyncupCntlr %s rev %d: code %d (%s)",
			cntlrPointerText(req.GetCntlrPointer()), req.GetRevision(), code,
			reply.GetAgentReply().GetDetails())
	}
	return nil
}

// cntlrSyncup is the outcome of one SyncupCntlr sent from its own goroutine.
type cntlrSyncup struct {
	reply *pb.SyncupCntlrReply
	err   error
}

// asyncSyncupCntlr sends one SyncupCntlr from its own goroutine; its outcome
// arrives on the returned channel, for the test's goroutine to judge.
func (w *fakeWorker) asyncSyncupCntlr(
	req *pb.SyncupCntlrRequest,
) <-chan cntlrSyncup {
	done := make(chan cntlrSyncup, 1)
	go func() {
		ctx, cancel := w.callCtx()
		defer cancel()
		reply, err := w.client.SyncupCntlr(ctx, req)
		if err != nil {
			err = fmt.Errorf("SyncupCntlr rev %d: %w", req.GetRevision(), err)
		}
		done <- cntlrSyncup{reply, err}
	}()
	return done
}

// awaitCntlr waits for the outcome of an asyncSyncupCntlr and fails the test
// unless the syncup replied.
func (w *fakeWorker) awaitCntlr(
	done <-chan cntlrSyncup,
	what string,
) *pb.SyncupCntlrReply {
	w.t.Helper()
	select {
	case r := <-done:
		if r.err != nil {
			w.t.Fatalf("%s: %v", what, r.err)
		}
		return r.reply
	case <-time.After(15 * time.Second):
		w.t.Fatalf("%s never replied", what)
	}
	return nil
}

// fanOutWidth is how many syncups of one node the full shapes keep in
// flight at once. The worker sends each cntlr's syncups from a child of its
// own (dnv-worker.md, RW14), so the agent meets a node's syncups together;
// a bound keeps each call's wait for the agent's locks well inside its
// deadline under the race detector.
const fanOutWidth = 32

// fanOut runs call for every i below n from goroutines of its own, at most
// width at a time, and returns the first failure for the test's goroutine
// to report.
func fanOut(n, width int, call func(i int) error) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	slots := make(chan struct{}, width)
	for i := 0; i < n; i++ {
		slots <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-slots }()
			if err := call(i); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	return first
}

// cnRound is one CheckCn round of the fixture CN at the desired revision.
func (w *fakeWorker) cnRound(revision uint64) *pb.CheckCnReply {
	w.t.Helper()
	if w.cn == nil {
		stream, err := w.client.CheckCn(w.ctx)
		if err != nil {
			w.t.Fatalf("CheckCn: %v", err)
		}
		w.cn = stream
	}
	if err := w.cn.Send(&pb.CheckCnRequest{
		ClusterId: testCluster, CnId: testCn, Revision: revision,
		ShowInfo: true,
	}); err != nil {
		w.t.Fatalf("CheckCn send: %v", err)
	}
	reply, err := w.cn.Recv()
	if err != nil {
		w.t.Fatalf("CheckCn recv: %v", err)
	}
	return reply
}

// cntlrRound is one CheckCntlr round of the cntlr ptr names, on that cntlr's
// own stream, under the round's own trace id ("" keeps the stream's).
func (w *fakeWorker) cntlrRound(
	ptr *pb.CntlrPointer,
	revision uint64,
	traceId string,
) *pb.CheckCntlrReply {
	w.t.Helper()
	reply, err := w.tryCntlrRound(ptr, revision, traceId)
	if err != nil {
		w.t.Fatal(err)
	}
	return reply
}

// tryCntlrRound is cntlrRound returning its failure instead of failing the
// test, so a goroutine other than the test's can play the round.
func (w *fakeWorker) tryCntlrRound(
	ptr *pb.CntlrPointer,
	revision uint64,
	traceId string,
) (*pb.CheckCntlrReply, error) {
	stream := w.cntlrs[ptr.GetSpId()]
	if stream == nil {
		var err error
		stream, err = w.client.CheckCntlr(w.ctx)
		if err != nil {
			return nil, fmt.Errorf("CheckCntlr: %w", err)
		}
		w.cntlrs[ptr.GetSpId()] = stream
	}
	if err := stream.Send(&pb.CheckCntlrRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: ptr,
		Revision: revision, ShowInfo: true, TraceId: traceId,
	}); err != nil {
		return nil, fmt.Errorf("CheckCntlr send: %w", err)
	}
	reply, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("CheckCntlr recv: %w", err)
	}
	return reply, nil
}

// dmPresent reports whether node holds the dm device name, read under the
// node's lock: the leg probers and the background reads run meanwhile.
func dmPresent(node *fakeNode, name string) bool {
	node.mu.Lock()
	defer node.mu.Unlock()
	_, ok := node.dms[name]
	return ok
}

// dmNamesContaining lists node's dm devices whose names contain substr,
// read under the node's lock.
func dmNamesContaining(node *fakeNode, substr string) []string {
	node.mu.Lock()
	defer node.mu.Unlock()
	var out []string
	for name := range node.dms {
		if strings.Contains(name, substr) {
			out = append(out, name)
		}
	}
	return out
}

// settledRound plays rounds of the cntlr ptr names until one answers clean
// at revision, joining the read each round starts: a primary's leg rows read
// pending until its leg probers have completed a round (CN11), and a round
// answers what the last converge or read stored (dnagent.md SH28, SH29). It
// fails the test at its deadline with the last reply's rows.
func (w *fakeWorker) settledRound(
	srv *CnAgentServer,
	ptr *pb.CntlrPointer,
	revision uint64,
) *pb.CheckCntlrReply {
	w.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		reply := w.cntlrRound(ptr, revision, "")
		if reply.GetAgentReply().GetCode() == 0 &&
			reply.GetRevision() == revision &&
			len(notOkRows(reply.GetCntlrInfo())) == 0 &&
			reply.GetCntlrInfo() != nil {
			return reply
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("cntlr %s never settled at revision %d: code %d "+
				"(%s) rev %d, rows %v", cntlrPointerText(ptr), revision,
				reply.GetAgentReply().GetCode(),
				reply.GetAgentReply().GetDetails(), reply.GetRevision(),
				notOkRows(reply.GetCntlrInfo()))
		}
		joinLongReads(w.t, srv)
		time.Sleep(10 * time.Millisecond)
	}
}

// joinLongReads is joinReads with a bound for the reads of a full shape,
// which enumerate tens of thousands of objects.
func joinLongReads(t *testing.T, srv *CnAgentServer) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		srv.reads.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("a background read never ended")
	}
}

// waitProbed waits until every leg prober of every cntlr the server holds
// has completed a round.
func waitProbed(t *testing.T, srv *CnAgentServer) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		left := 0
		srv.mu.Lock()
		for _, st := range srv.cntlrs {
			for _, prober := range st.probers {
				if _, completed, err := prober.snapshot(); !completed ||
					err != nil {
					left++
				}
			}
		}
		srv.mu.Unlock()
		if left == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d leg probers never completed a round", left)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// probing runs the server's leg probers and its rounds' reads as production
// does, the probers on interval, shorter than the production one so a case
// need not wait it out. It must run before the first converge starts a
// prober.
func probing(srv *CnAgentServer, interval time.Duration) {
	srv.probeInterval = interval
	srv.roundReads = true
}

// notOkRows names every row of info, at any depth, whose status is not OK.
func notOkRows(info proto.Message) []string {
	var out []string
	var walk func(prefix string, m protoreflect.Message)
	walk = func(prefix string, m protoreflect.Message) {
		if row, ok := m.Interface().(*pb.ResInfo); ok {
			if row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
				out = append(out, fmt.Sprintf("%s %v %q", prefix,
					row.GetStatus(), row.GetDetails()))
			}
			return
		}
		m.Range(func(fd protoreflect.FieldDescriptor,
			v protoreflect.Value) bool {
			name := prefix + "." + string(fd.Name())
			switch {
			case fd.IsMap() && fd.MapValue().Message() != nil:
				v.Map().Range(func(k protoreflect.MapKey,
					mv protoreflect.Value) bool {
					walk(fmt.Sprintf("%s[%v]", name, k.Interface()),
						mv.Message())
					return true
				})
			case fd.IsList() && fd.Message() != nil:
				for i := 0; i < v.List().Len(); i++ {
					walk(fmt.Sprintf("%s[%d]", name, i),
						v.List().Get(i).Message())
				}
			case fd.Message() != nil && !fd.IsMap() && !fd.IsList():
				walk(name, v.Message())
			}
			return true
		})
	}
	if info != nil {
		walk("", info.ProtoReflect())
	}
	return out
}

// assertCleanRound fails unless a reply answered code zero at revision,
// from an observation, with every row it carries OK.
func assertCleanRound(
	t *testing.T,
	what string,
	reply interface {
		GetAgentReply() *pb.AgentReply
		GetRevision() uint64
		GetObsStamp() *pb.ObsStamp
	},
	revision uint64,
	info proto.Message,
) {
	t.Helper()
	if code := reply.GetAgentReply().GetCode(); code != 0 {
		t.Errorf("%s: code %d (%s), want 0", what, code,
			reply.GetAgentReply().GetDetails())
	}
	if got := reply.GetRevision(); got != revision {
		t.Errorf("%s: revision %d, want %d", what, got, revision)
	}
	if reply.GetObsStamp().GetObsTime() == nil {
		t.Errorf("%s: answered with no observation", what)
	}
	if rows := notOkRows(info); len(rows) > 0 {
		t.Errorf("%s: rows not OK: %v", what, rows)
	}
}

// ---------------------------------------------------------------------------
// The full controller node (MaxCntlrCntPerCn)
// ---------------------------------------------------------------------------

// fullCntlrPtr is cntlr i of the full controller node: each of a pool of its
// own.
func fullCntlrPtr(i int) *pb.CntlrPointer {
	return &pb.CntlrPointer{SpId: uint64(0x1000 + i), CntlrId: testCntlr}
}

// fullCnReq is the full controller node's SyncupCn naming cntlrCnt cntlrs.
func fullCnReq(revision uint64, cntlrCnt int) *pb.SyncupCnRequest {
	req := cnReq(revision, false)
	for i := 0; i < cntlrCnt; i++ {
		req.CntlrPointerList = append(req.CntlrPointerList, fullCntlrPtr(i))
	}
	return req
}

// fullCntlrReq is cntlr i's request: the fixture's primary with its own
// pool, its own subsystem NQN and its own namespace identity.
func fullCntlrReq(revision uint64, i int) *pb.SyncupCntlrRequest {
	req := cntlrReq(reqOpts{revision: revision, primary: true})
	req.CntlrPointer = fullCntlrPtr(i)
	ss := req.NqnToSubsystem[testNqn]
	delete(req.NqnToSubsystem, testNqn)
	ss.NsList[0].DevUuid = fmt.Sprintf("11111111-1111-4111-8111-%012x", i)
	ss.NsList[0].DevNguid = fmt.Sprintf("1111111111114111811%013x", i)
	req.NqnToSubsystem[fmt.Sprintf("%s-%d", testNqn, i)] = ss
	return req
}

// TestFakeWorkerConvergesAFullControllerNode builds the most cntlrs one
// controller node holds, MaxCntlrCntPerCn, and one more, each the primary of
// a pool of its own, through the worker's calls: the CN's syncup naming every
// cntlr, then each cntlr's syncup, sent concurrently as the worker's
// children send them, then a round on every object's stream.
// The count is the control plane's bound (architecture.md, Capacity index
// keys), not the agent's, so the cntlr past it converges like the others.
func TestFakeWorkerConvergesAFullControllerNode(t *testing.T) {
	if testing.Short() {
		t.Skip("full shape: skipped under -short")
	}
	const cntlrCnt = common.MaxCntlrCntPerCn + 1
	srv, _ := newTestServer(t)
	probing(srv, 500*time.Millisecond)
	w := newFakeWorker(t, srv)
	w.callTimeout = fullShapeCallTimeout
	start := time.Now()
	if reply := w.syncupCn(fullCnReq(1, cntlrCnt)); reply.GetAgentReply().
		GetCode() != 0 {
		t.Fatalf("SyncupCn: %v", reply.GetAgentReply())
	}
	if err := fanOut(cntlrCnt, fanOutWidth, func(i int) error {
		return w.trySyncupCntlr(fullCntlrReq(1, i))
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d cntlrs converged in %v", cntlrCnt, time.Since(start))
	// The rounds answer the converges' observations, the legs pending their
	// first probe; each starts a read, which takes the probers' outcome.
	waitProbed(t, srv)
	for i := 0; i < cntlrCnt; i++ {
		w.cntlrRound(fullCntlrPtr(i), 1, "")
	}
	joinLongReads(t, srv)
	for i := 0; i < cntlrCnt; i++ {
		reply := w.cntlrRound(fullCntlrPtr(i), 1, "")
		assertCleanRound(t, fmt.Sprintf("cntlr %d round", i), reply, 1,
			reply.GetCntlrInfo())
		if n := len(reply.GetCntlrInfo().GetLegIdToLeg()); n != 2 {
			t.Errorf("cntlr %d reports %d legs, want its two", i, n)
		}
	}
	w.cnRound(1)
	joinLongReads(t, srv)
	cn := w.cnRound(1)
	assertCleanRound(t, "cn round", cn, 1, cn.GetCnInfo())
	t.Logf("full controller node case took %v", time.Since(start))
}

// ---------------------------------------------------------------------------
// The widest cntlr
// ---------------------------------------------------------------------------

// widestShape is how many entries the widest-cntlr case builds in each list
// of one cntlr's request. At the limits every count is the one model's
// TestWidestMessagesFitGrpcMaxMsgSize builds: MaxSliceCntPerSp slices of
// MaxGrpCntPerSlice groups in each list, every group MaxAllocLegPerGrp legs
// and MaxSpareLegPerGrp spares at RAID1, MaxMigrCntPerSp legs with a second
// side, MaxTdCntPerSp thin devices, MaxSsCntPerSp subsystems of
// MaxNsCntPerSs namespaces and MaxHostCntPerSs hosts, MaxCloneCntPerSp
// clones of MaxCntlrCntPerSp source transports, and MaxXferCntPerSp
// transfers.
type widestShape struct {
	slices, grps, tds, clones, xfers, migrs int
}

// widestGrpsPerList is the groups per list of each slice the widest-cntlr
// case builds, the one axis it cuts below its limit (MaxGrpCntPerSlice). The
// cut keeps every run inside a full shape's bound with a quarter of it to
// spare, since `go test ./...` runs this build beside every other package's
// on a loaded machine. The build walks the host's subsystems again after
// each connect (cnagent.md CN10, Known limits), and the fake lists a
// directory by scanning every entry it holds, so the time grows faster than
// the groups. The race detector adds memory and time to every access the
// case makes, and a race build passes the bound well below this cut, so a
// race build takes widestGrpsPerListRace groups, whose runs stay inside the
// bound. There the lists the cut leaves at their limit take most of the
// time, so fewer groups buy little room.
const (
	widestGrpsPerList     = 16
	widestGrpsPerListRace = 4
)

// widestGrps is the groups per list this test binary's widest cntlr builds.
func widestGrps() int {
	if raceEnabled {
		return widestGrpsPerListRace
	}
	return widestGrpsPerList
}

// maxNqn is an NQN of exactly MaxNqnLength bytes: prefix, filler, then the
// suffix that keeps it unique.
func maxNqn(prefix, suffix string) string {
	return prefix + strings.Repeat("x",
		common.MaxNqnLength-len(prefix)-len(suffix)) + suffix
}

// widestCntlrReq is the primary of a pool of that shape. Every id is the
// shape's own: groups, legs and sides count up from one base, so no two share
// a name or an NQN, and each RAID1 group mirrors across the two disk node
// addresses. Every NQN a user or the gateway may choose (a subsystem's, its
// hosts', a clone source's, a transfer's hosts') is as long as an NQN may
// be, as model's widest message holds them.
func widestCntlrReq(revision uint64, shape widestShape) *pb.SyncupCntlrRequest {
	req := cntlrReq(reqOpts{revision: revision, primary: true, raid1: true})
	next := uint64(0x100000)
	id := func() uint64 {
		next++
		return next
	}
	migrs := shape.migrs
	legsOf := func() ([]*pb.Leg, []*pb.Leg) {
		var legs, spares []*pb.Leg
		for k := 0; k < common.MaxAllocLegPerGrp+common.MaxSpareLegPerGrp; k++ {
			addr, svcId := testIp, testSvcId
			otherAddr, otherSvcId := testIp2, testSvcId2
			if k%2 == 1 {
				addr, svcId, otherAddr, otherSvcId = otherAddr, otherSvcId,
					addr, svcId
			}
			leg := legOf(id(), sideOf(id(), addr, svcId))
			leg.LegIdx = uint32(k)
			if k < common.MaxAllocLegPerGrp {
				// A migration's destination is on another disk node than
				// its source.
				if migrs > 0 {
					migrs--
					leg.SideList = append(leg.SideList,
						sideOf(id(), otherAddr, otherSvcId))
				}
				legs = append(legs, leg)
			} else {
				spares = append(spares, leg)
			}
		}
		return legs, spares
	}
	grpsOf := func(extCnt, dataBlocks uint64) []*pb.Group {
		var grps []*pb.Group
		for g := 0; g < shape.grps; g++ {
			legs, spares := legsOf()
			grps = append(grps, &pb.Group{
				GrpId: id(), ExtCnt: extCnt, MetaBlocks: 3,
				DataBlocks: dataBlocks, LegList: legs, SpareLegList: spares,
			})
		}
		return grps
	}
	req.IdToSlice = make(map[string]*pb.Slice, shape.slices)
	for s := 0; s < shape.slices; s++ {
		sliceId := id()
		req.IdToSlice[fmt.Sprintf(common.IdKeyFmt, sliceId)] = &pb.Slice{
			SliceIdx:    uint32(s),
			MetaGrpList: grpsOf(1, 61),
			DataGrpList: grpsOf(2, 125),
		}
	}
	req.TdList = nil
	for k := 0; k < shape.tds; k++ {
		req.TdList = append(req.TdList, &pb.ThinDevice{
			TdId: uint64(0x10000 + k), DevId: uint32(k + 1), Size: testTdSize,
		})
	}
	hosts := func(tag string) []string {
		var out []string
		for h := 0; h < common.MaxHostCntPerSs; h++ {
			out = append(out, maxNqn(testHostNqn+":",
				fmt.Sprintf(":%s:%d", tag, h)))
		}
		return out
	}
	req.NqnToSubsystem = map[string]*pb.Subsystem{}
	var nqns []string
	for j := 0; j < common.MaxSsCntPerSp; j++ {
		nqn := maxNqn(testNqn+"-", fmt.Sprintf("-%d", j))
		nqns = append(nqns, nqn)
		ss := &pb.Subsystem{
			SsId:         uint64(0x30000 + j),
			Serial:       fmt.Sprintf(common.IdKeyFmt, 0x30000+j),
			Model:        "dnv",
			AllowedHosts: hosts(fmt.Sprintf("ss-%d", j)),
		}
		for n := 0; n < common.MaxNsCntPerSs; n++ {
			k := j*common.MaxNsCntPerSs + n
			ss.NsList = append(ss.NsList, &pb.Namespace{
				NsId:     uint64(0x40000 + k),
				NsIdx:    uint32(n + 1),
				TdId:     uint64(0x10000 + k),
				DevUuid:  fmt.Sprintf("11111111-1111-4111-8111-%012x", k),
				DevNguid: fmt.Sprintf("1111111111114111811%013x", k),
			})
		}
		req.NqnToSubsystem[nqn] = ss
	}
	req.CloneList = nil
	for c := 0; c < shape.clones; c++ {
		clone := cloneOfTd(uint64(0x50000+c), maxNqn(testSrcNqn+"-",
			fmt.Sprintf("-%d", c)), uint64(0x10000+common.MaxSsCntPerSp*
			common.MaxNsCntPerSs+c))
		clone.SrcTrConfList = nil
		for k := 0; k < common.MaxCntlrCntPerSp; k++ {
			clone.SrcTrConfList = append(clone.SrcTrConfList,
				sideTrConf(testIp2, fmt.Sprintf("%d", 4300+k)))
		}
		req.CloneList = append(req.CloneList, clone)
	}
	req.XferList = nil
	for x := 0; x < shape.xfers; x++ {
		req.XferList = append(req.XferList, &pb.Transfer{
			XferId:       uint64(0x60000 + x),
			OriNqn:       nqns[x%len(nqns)],
			OriNsIdx:     uint32(x/len(nqns) + 1),
			AllowedHosts: hosts(fmt.Sprintf("xfer-%d", x)),
		})
	}
	return req
}

// TestFakeWorkerConvergesTheWidestCntlr converges one primary whose request
// holds every list at its limit but one, the groups per slice, through the
// CN's syncup and the cntlr's, then plays rounds until its probers have run:
// every row it reports reads OK. MaxGrpCntPerSlice groups in both lists
// of every slice are more than sixteen thousand md arrays over more than
// sixty thousand leg connections, which no converge in this package's fake
// finishes inside the case's bound, so the case takes the most groups per
// list that do.
func TestFakeWorkerConvergesTheWidestCntlr(t *testing.T) {
	if testing.Short() {
		t.Skip("full shape: skipped under -short")
	}
	shape := widestShape{
		slices: common.MaxSliceCntPerSp,
		grps:   widestGrps(),
		tds:    common.MaxTdCntPerSp,
		clones: common.MaxCloneCntPerSp,
		xfers:  common.MaxXferCntPerSp,
		migrs:  common.MaxMigrCntPerSp,
	}
	srv, _ := newTestServer(t)
	probing(srv, time.Second)
	w := newFakeWorker(t, srv)
	w.callTimeout = fullShapeCallTimeout
	start := time.Now()
	if reply := w.syncupCn(cnReq(1, true)); reply.GetAgentReply().
		GetCode() != 0 {
		t.Fatalf("SyncupCn: %v", reply.GetAgentReply())
	}
	req := widestCntlrReq(1, shape)
	t.Logf("widest cntlr syncup: %d bytes", proto.Size(req))
	reply := w.syncupCntlr(req)
	if code := reply.GetAgentReply().GetCode(); code != 0 {
		t.Fatalf("SyncupCntlr: code %d (%s)", code,
			reply.GetAgentReply().GetDetails())
	}
	t.Logf("%+v converged in %v", shape, time.Since(start))
	settled := w.settledRound(srv, cntlrPtr(), 1)
	info := settled.GetCntlrInfo()
	legs := shape.slices * 2 * shape.grps *
		(common.MaxAllocLegPerGrp + common.MaxSpareLegPerGrp)
	for _, c := range []struct {
		what      string
		got, want int
	}{
		{"md arrays", len(info.GetGrpIdToMdRaid()), shape.slices * 2 * shape.grps},
		{"legs", len(info.GetLegIdToLeg()), legs},
		{"thin devices", len(info.GetTdIdToThinInfo()), shape.tds},
		{"namespaces", len(info.GetNsIdToNamespace()),
			common.MaxSsCntPerSp * common.MaxNsCntPerSs},
		{"clones", len(info.GetCloneIdToDmClone()), shape.clones},
		{"transfers", len(info.GetXferIdToSubsystem()), shape.xfers},
		{"migration paths", len(info.GetSideIdToPath()), 2 * shape.migrs},
	} {
		if c.got != c.want {
			t.Errorf("%s reported: %d, want %d", c.what, c.got, c.want)
		}
	}
	t.Logf("widest cntlr case took %v", time.Since(start))
}

// ---------------------------------------------------------------------------
// Restarts, with the local files kept and with them lost
// ---------------------------------------------------------------------------

// startCnServer builds a cn server over node and runs its startup reconcile
// under a root of its own, so a restart can end the old process's
// background work (its probers and retries) without ending the test. Its
// probers run on probeInterval and its rounds start their reads.
func startCnServer(
	t *testing.T,
	node *fakeNode,
	probeInterval time.Duration,
) (*CnAgentServer, context.CancelFunc) {
	t.Helper()
	srv := newCnServer(node)
	probing(srv, probeInterval)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := srv.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return srv, cancel
}

// cnRestart plays an agent restart under the worker: the worker's streams
// end, the process exits, and a new one starts over the same kernel objects
// with its local store kept or lost. It returns the new server, a worker
// dialed to it, and what the startup reconcile changed on the node.
func cnRestart(
	t *testing.T,
	w *fakeWorker,
	stop context.CancelFunc,
	node *fakeNode,
	keepFiles bool,
) (*CnAgentServer, context.CancelFunc, *fakeWorker, []string) {
	t.Helper()
	w.close()
	stop()
	node.mu.Lock()
	if !keepFiles {
		node.protos = map[string][]byte{}
	}
	node.mu.Unlock()
	node.Reset()
	srv, cancel := startCnServer(t, node, 50*time.Millisecond)
	muts := node.Mutations()
	return srv, cancel, newFakeWorker(t, srv), muts
}

// syncupCnSettled sends a SyncupCn that sweeps a cntlr's stack, the way the
// worker drives it: the pass names the leg connections it set disconnecting
// and nothing else (CN10), and the SyncupCn the worker re-sends for them
// (dnv-worker.md RW4), once they have returned, answers clean.
func (w *fakeWorker) syncupCnSettled(
	srv *CnAgentServer,
	req *pb.SyncupCnRequest,
) {
	w.t.Helper()
	cnSweepOnlyDisconnects(w.t, w.syncupCn(req).GetAgentReply(),
		"the pass that disconnects")
	awaitDisconnects(w.t, srv)
	if reply := w.syncupCn(req); reply.GetAgentReply().GetCode() != 0 {
		w.t.Fatalf("the re-sent SyncupCn: %v", reply.GetAgentReply())
	}
}

// otherCntlrPtr is the scenarios' second cntlr, of a pool of its own.
func otherCntlrPtr() *pb.CntlrPointer {
	return fullCntlrPtr(1)
}

// twoCntlrCnReq is the CN's syncup naming the fixture cntlr, the other one,
// or both.
func twoCntlrCnReq(revision uint64, fixture, other bool) *pb.SyncupCnRequest {
	req := cnReq(revision, fixture)
	if other {
		req.CntlrPointerList = append(req.CntlrPointerList, otherCntlrPtr())
	}
	return req
}

// otherCntlrReq is the other cntlr's request: the fixture's shape, primary
// or standby.
func otherCntlrReq(revision uint64, primary bool) *pb.SyncupCntlrRequest {
	req := fullCntlrReq(revision, 1)
	req.Cntlr.Primary = primary
	return req
}

// reconcileMutations drops what a startup reconcile writes on a node that
// already matches the stored requests and is no change of the node: its
// leg probers' health-block IO.
func reconcileMutations(muts []string) []string {
	var out []string
	for _, call := range muts {
		if strings.HasPrefix(call, "writeblock ") {
			continue
		}
		out = append(out, call)
	}
	return out
}

// TestFakeWorkerRestartsWithKeptFiles restarts the agent three times with its
// local store kept, between syncups that fail the primary over from one
// cntlr to the other and drop a cntlr. Each startup reconcile converges every
// stored object (CN2) and changes nothing on a node that already matches
// it (dnagent.md SH16), and stores an observation of each, so the rounds
// after the restart answer at the held revisions, clean once the restarted
// probers have run, with no syncup from the worker (dnv-worker.md RW4). A
// cntlr whose pointer left the CN's list before the restart stays unknown.
func TestFakeWorkerRestartsWithKeptFiles(t *testing.T) {
	node := newFakeNode()
	node.dirs[agent.NvmetRoot] = true
	srv, stop := startCnServer(t, node, 50*time.Millisecond)
	w := newFakeWorker(t, srv)
	if reply := w.syncupCn(twoCntlrCnReq(1, true, true)); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCn: %v", reply.GetAgentReply())
	}
	for _, req := range []*pb.SyncupCntlrRequest{
		cntlrReq(reqOpts{revision: 1, primary: true}),
		otherCntlrReq(1, false),
	} {
		if reply := w.syncupCntlr(req); reply.GetAgentReply().
			GetCode() != 0 {
			t.Fatalf("SyncupCntlr: %v", reply.GetAgentReply())
		}
	}
	settleBoth := func(what string, revision uint64) {
		t.Helper()
		for _, ptr := range []*pb.CntlrPointer{cntlrPtr(), otherCntlrPtr()} {
			w.settledRound(srv, ptr, revision)
		}
		w.cnRound(1)
		joinReads(t, srv)
		cn := w.cnRound(1)
		assertCleanRound(t, what+": cn round", cn, 1, cn.GetCnInfo())
	}
	settleBoth("built", 1)

	// restart restarts the agent and checks the first round of each cntlr
	// it holds: it answers from the startup reconcile's observation, at the
	// revision the agent holds, before any read of its own has run.
	restart := func(what string, revision uint64,
		held ...*pb.CntlrPointer) {
		t.Helper()
		var muts []string
		srv, stop, w, muts = cnRestart(t, w, stop, node, true)
		if muts = reconcileMutations(muts); len(muts) > 0 {
			t.Errorf("%s: the startup reconcile changed the node: %v",
				what, muts)
		}
		for _, ptr := range held {
			first := w.cntlrRound(ptr, revision, "")
			if first.GetAgentReply().GetCode() != 0 ||
				first.GetRevision() != revision ||
				first.GetObsStamp().GetObsTime() == nil {
				t.Errorf("%s: first round of %s = code %d rev %d stamp %v; "+
					"want the reconcile's observation at revision %d",
					what, cntlrPointerText(ptr),
					first.GetAgentReply().GetCode(), first.GetRevision(),
					first.GetObsStamp(), revision)
			}
		}
	}
	restart("restart 1", 1, cntlrPtr(), otherCntlrPtr())
	settleBoth("after restart 1", 1)

	// The failover: the fixture cntlr is demoted, the other promoted.
	if reply := w.syncupCntlr(cntlrReq(reqOpts{revision: 2})); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("demotion: %v", reply.GetAgentReply())
	}
	if reply := w.syncupCntlr(otherCntlrReq(2, true)); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("promotion: %v", reply.GetAgentReply())
	}
	settleBoth("failed over", 2)
	restart("restart 2", 2, cntlrPtr(), otherCntlrPtr())
	settleBoth("after restart 2", 2)

	w.syncupCnSettled(srv, twoCntlrCnReq(2, false, true))
	restart("restart 3", 2, otherCntlrPtr())
	w.settledRound(srv, otherCntlrPtr(), 2)
	gone := w.cntlrRound(cntlrPtr(), 2, "")
	if gone.GetAgentReply().GetCode() != common.ReplyCodeUnknownObject ||
		gone.GetRevision() != 0 {
		t.Errorf("the dropped cntlr's round = code %d rev %d, want unknown "+
			"at revision 0", gone.GetAgentReply().GetCode(),
			gone.GetRevision())
	}
	if dmPresent(node, poolName(srv)) {
		t.Errorf("the dropped cntlr's pool %s survived", poolName(srv))
	}
}

// TestFakeWorkerRestartsWithEmptyFiles restarts the agent twice over an empty
// local store, the kernel objects kept. The agent then holds nothing: every
// round answers the unknown-object code at revision zero until the object's
// syncup arrives (dnagent.md SH25). The CN's syncup naming the cntlr keeps
// its whole stack (CN21: an sp in the pointer list gets no chain at node
// level), and the cntlr's own syncup adopts the stack probe-first: it
// changes nothing on the node that already matches it (dnagent.md SH16), so
// no device is made, reloaded or suspended again, and no array or connection
// is made again. When the second restart's CN syncup no longer names the
// cntlr, its stack is swept.
func TestFakeWorkerRestartsWithEmptyFiles(t *testing.T) {
	node := newFakeNode()
	node.dirs[agent.NvmetRoot] = true
	srv, stop := startCnServer(t, node, 50*time.Millisecond)
	w := newFakeWorker(t, srv)
	if reply := w.syncupCn(cnReq(1, true)); reply.GetAgentReply().
		GetCode() != 0 {
		t.Fatalf("SyncupCn: %v", reply.GetAgentReply())
	}
	if reply := w.syncupCntlr(cntlrReq(reqOpts{revision: 1,
		primary: true})); reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCntlr: %v", reply.GetAgentReply())
	}
	w.settledRound(srv, cntlrPtr(), 1)

	unknownRounds := func(what string) {
		t.Helper()
		for round := 0; round < 3; round++ {
			reply := w.cntlrRound(cntlrPtr(), 1, "")
			if reply.GetAgentReply().GetCode() !=
				common.ReplyCodeUnknownObject || reply.GetRevision() != 0 {
				t.Errorf("%s: round %d = code %d rev %d, want unknown at "+
					"revision 0", what, round,
					reply.GetAgentReply().GetCode(), reply.GetRevision())
			}
		}
	}
	// Every change of the node but the store's own write, the probers'
	// health-block IO aside: what an adoption must not issue, a reload or a
	// suspend of a live device as much as a create or a connect.
	built := func(what string) {
		t.Helper()
		for _, call := range reconcileMutations(node.Mutations()) {
			if !strings.HasPrefix(call, "writeproto ") {
				t.Errorf("%s: %s", what, call)
			}
		}
	}

	var muts []string
	srv, stop, w, muts = cnRestart(t, w, stop, node, false)
	if muts = reconcileMutations(muts); len(muts) > 0 {
		t.Errorf("the startup reconcile over an empty store changed the "+
			"node: %v", muts)
	}
	if cn := w.cnRound(1); cn.GetAgentReply().GetCode() !=
		common.ReplyCodeUnknownObject || cn.GetRevision() != 0 {
		t.Errorf("cn round over an empty store = code %d rev %d, want "+
			"unknown at revision 0", cn.GetAgentReply().GetCode(),
			cn.GetRevision())
	}
	unknownRounds("before the cn syncup")
	node.Reset()
	if reply := w.syncupCn(cnReq(1, true)); reply.GetAgentReply().
		GetCode() != 0 {
		t.Fatalf("SyncupCn over an empty store: %v", reply.GetAgentReply())
	}
	built("the cn syncup over an empty store changed the cntlr's stack")
	unknownRounds("after the cn syncup")
	node.Reset()
	if reply := w.syncupCntlr(cntlrReq(reqOpts{revision: 1,
		primary: true})); reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCntlr over an empty store: %v",
			reply.GetAgentReply())
	}
	built("the cntlr syncup over an empty store changed the node")
	w.settledRound(srv, cntlrPtr(), 1)

	srv, stop, w, _ = cnRestart(t, w, stop, node, false)
	unknownRounds("after the second restart")
	w.syncupCnSettled(srv, cnReq(2, false))
	cnSweepNoDmLeft(t, node)
	unknownRounds("the swept cntlr")
	_ = stop
}

// ---------------------------------------------------------------------------
// A connect that hangs, and a syncup that arrives during a converge
// ---------------------------------------------------------------------------

// promptRound runs one cntlr round and fails the test unless it answers
// within the bound a worker's round deadline would allow on a loaded
// machine.
func (w *fakeWorker) promptRound(
	ptr *pb.CntlrPointer,
	revision uint64,
	traceId string,
) *pb.CheckCntlrReply {
	w.t.Helper()
	type outcome struct {
		reply *pb.CheckCntlrReply
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		reply, err := w.tryCntlrRound(ptr, revision, traceId)
		done <- outcome{reply, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			w.t.Fatalf("%s: %v", traceId, r.err)
		}
		return r.reply
	case <-time.After(2 * time.Second):
		w.t.Fatalf("%s: a CheckCntlr round at revision %d did not answer "+
			"in time", traceId, revision)
	}
	return nil
}

// waitUntil polls cond until it holds, failing the test after a bound that
// a loaded machine stays well inside.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// grownLegConnect is the key of the `nvme connect` of the leg growReq adds.
func grownLegConnect(srv *CnAgentServer) string {
	return "--nqn " + legNqn(srv, testDataLeg2) + " --hostnqn"
}

// TestFakeWorkerHungConnectPastThePassBudget grows a settled primary by a
// data group whose leg's connect hangs until the soft timeout kills it
// (dnagent.md SH15), across many rounds and past the pass's wait budget
// (CnConnectPassBudget). Every round meanwhile answers at once from the
// observation before the syncup, with the converge's start, and starts no
// read (dnagent.md SH28 to SH30). The connect's own time spends the budget,
// so the pass tries the connect no second time (CN10) and leaves the leg to
// the background connect retry, which connects it once the disk node answers:
// the rounds after answer clean at the syncup's revision, with no further
// syncup.
func TestFakeWorkerHungConnectPastThePassBudget(t *testing.T) {
	srv, node := newTestServer(t)
	probing(srv, 50*time.Millisecond)
	srv.retryInterval = 200 * time.Millisecond
	w := newFakeWorker(t, srv)
	if reply := w.syncupCn(cnReq(1, true)); reply.GetAgentReply().
		GetCode() != 0 {
		t.Fatalf("SyncupCn: %v", reply.GetAgentReply())
	}
	if reply := w.syncupCntlr(cntlrReq(reqOpts{revision: 1,
		primary: true})); reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCntlr: %v", reply.GetAgentReply())
	}
	w.settledRound(srv, cntlrPtr(), 1)
	joinReads(t, srv)
	before := latestCntlrObs(t, srv).Time

	release := openGate(t, node, grownLegConnect(srv))
	start := time.Now()
	done := w.asyncSyncupCntlr(growReq(2))
	waitUntil(t, "the grown leg's connect", func() bool {
		return node.hasCall(grownLegConnect(srv))
	})
	type played struct {
		traceId string
		reply   *pb.CheckCntlrReply
	}
	var rounds []played
	var reply *pb.SyncupCntlrReply
	for reply == nil {
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatal(r.err)
			}
			reply = r.reply
			continue
		case <-time.After(200 * time.Millisecond):
		}
		if time.Since(start) > 15*time.Second {
			t.Fatal("the syncup never returned past its killed connect")
		}
		traceId := fmt.Sprintf("hung-round-%d", len(rounds))
		rounds = append(rounds, played{traceId,
			w.promptRound(cntlrPtr(), 2, traceId)})
	}
	if took := time.Since(start); took < common.CnConnectPassBudget {
		t.Errorf("the syncup returned after %v, before the hang outlasted "+
			"the pass budget", took)
	}
	converged := reply.GetObsStamp().GetObsTime().AsTime()
	hung, pastBudget := 0, 0
	for _, r := range rounds {
		stamp := r.reply.GetObsStamp()
		if !stamp.GetAnswerTime().AsTime().Before(converged) {
			continue
		}
		hung++
		if r.reply.GetRevision() != 1 ||
			!stamp.GetObsTime().AsTime().Equal(before) ||
			stamp.GetConvergeStart() == nil {
			t.Errorf("%s during the hang = rev %d, stamp %v; want the "+
				"observation before the syncup, at revision 1, and the "+
				"converge's start", r.traceId, r.reply.GetRevision(), stamp)
			continue
		}
		if stamp.GetAnswerTime().AsTime().Sub(
			stamp.GetConvergeStart().AsTime()) > common.CnConnectPassBudget {
			pastBudget++
		}
		if calls := node.Traced(r.traceId); len(calls) > 0 {
			t.Errorf("%s started a read during the converge: %v",
				r.traceId, calls)
		}
	}
	if hung < 3 || pastBudget == 0 {
		t.Errorf("%d rounds answered during the hang, %d of them past the "+
			"pass budget; want several, and one past it", hung, pastBudget)
	}
	if n := len(node.callsMatching(grownLegConnect(srv))); n != 1 {
		t.Errorf("the pass made %d connects of the grown leg, want the one "+
			"that hung: the hang spent the budget", n)
	}
	if row := reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg2]; row.
		GetStatus() == pb.ResStatus_RES_STATUS_OK {
		t.Errorf("the grown leg reads OK after its connect was killed: %v",
			row)
	}

	release()
	w.settledRound(srv, cntlrPtr(), 2)
}

// TestFakeWorkerSyncupDuringAConverge sends a cntlr's next syncup while the
// converge of the one before it hangs in a command. The newer syncup marks
// its converge as it arrives and waits for the cntlr's lock (dnagent.md
// SH30): the older converge runs to its end and replies first, at its own
// revision, and the newer one converges after it. Rounds meanwhile answer
// at once from the observation before both, with the first converge's
// start, which the newer syncup's mark does not move (SH30), and start no
// read (SH29). The rounds after answer the newer converge's observation.
func TestFakeWorkerSyncupDuringAConverge(t *testing.T) {
	srv, node := newTestServer(t)
	probing(srv, 50*time.Millisecond)
	w := newFakeWorker(t, srv)
	if reply := w.syncupCn(cnReq(1, true)); reply.GetAgentReply().
		GetCode() != 0 {
		t.Fatalf("SyncupCn: %v", reply.GetAgentReply())
	}
	if reply := w.syncupCntlr(cntlrReq(reqOpts{revision: 1,
		primary: true})); reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCntlr: %v", reply.GetAgentReply())
	}
	w.settledRound(srv, cntlrPtr(), 1)
	joinReads(t, srv)
	before := latestCntlrObs(t, srv).Time

	key := grownLegConnect(srv)
	node.blockCmd(key)
	t.Cleanup(func() { node.releaseCmd(key) })
	olderSent := time.Now()
	older := w.asyncSyncupCntlr(growReq(2))
	waitUntil(t, "the grow's converge to hang", func() bool {
		return node.hasCall(key)
	})
	newerReq := growReq(3)
	extraTd := &pb.ThinDevice{TdId: testTd + 0x100, DevId: 2,
		Size: testTdSize}
	newerReq.TdList = append(newerReq.TdList, extraTd)
	newerSent := time.Now()
	newer := w.asyncSyncupCntlr(newerReq)
	// The rounds come once the newer syncup has marked its converge: a read
	// one of them started before that mark would be the one the mark
	// cancels, which counts as running until it ends and so would keep
	// every later round from starting a read whatever the rule.
	time.Sleep(200 * time.Millisecond)
	var traces []string
	var firstStart *timestamppb.Timestamp
	for round := 0; round < 5; round++ {
		traceId := fmt.Sprintf("busy-round-%d", round)
		traces = append(traces, traceId)
		reply := w.promptRound(cntlrPtr(), 3, traceId)
		if round == 0 {
			firstStart = reply.GetObsStamp().GetConvergeStart()
			if firstStart == nil {
				t.Fatal("a round during the converge reports no converge " +
					"start")
			}
			// The cntlr's marks have stood without a break since the older
			// syncup arrived, so the start is that one's, not the newer's
			// (dnagent.md SH30).
			if got := firstStart.AsTime(); got.Before(olderSent) ||
				!got.Before(newerSent) {
				t.Errorf("converge start %v, want the older syncup's: "+
					"after it was sent at %v and before the newer one was "+
					"sent at %v", got.UTC(), olderSent.UTC(),
					newerSent.UTC())
			}
		}
		if reply.GetRevision() != 1 ||
			!reply.GetObsStamp().GetObsTime().AsTime().Equal(before) ||
			!proto.Equal(reply.GetObsStamp().GetConvergeStart(),
				firstStart) {
			t.Errorf("round %d = rev %d, stamp %v; want the observation "+
				"before both syncups and the first converge's start",
				round, reply.GetRevision(), reply.GetObsStamp())
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case <-newer:
		t.Fatal("the newer syncup replied while the older converge held " +
			"the cntlr")
	default:
	}

	node.releaseCmd(key)
	olderReply := w.awaitCntlr(older, "the older syncup")
	newerReply := w.awaitCntlr(newer, "the newer syncup")
	if olderReply.GetRevision() != 2 || olderReply.GetAgentReply().
		GetCode() != 0 {
		t.Errorf("older reply = code %d rev %d, want 0 at 2",
			olderReply.GetAgentReply().GetCode(), olderReply.GetRevision())
	}
	if newerReply.GetRevision() != 3 || newerReply.GetAgentReply().
		GetCode() != 0 {
		t.Errorf("newer reply = code %d rev %d, want 0 at 3",
			newerReply.GetAgentReply().GetCode(), newerReply.GetRevision())
	}
	if !newerReply.GetObsStamp().GetObsTime().AsTime().After(
		olderReply.GetObsStamp().GetObsTime().AsTime()) {
		t.Errorf("the newer converge's observation %v is not after the "+
			"older's %v", newerReply.GetObsStamp().GetObsTime().AsTime(),
			olderReply.GetObsStamp().GetObsTime().AsTime())
	}
	joinReads(t, srv)
	for _, traceId := range traces {
		if calls := node.Traced(traceId); len(calls) > 0 {
			t.Errorf("round %s started a read during the converges: %v",
				traceId, calls)
		}
	}
	after := w.settledRound(srv, cntlrPtr(), 3)
	if after.GetObsStamp().GetConvergeStart() != nil {
		t.Errorf("a converge start stands after both replied: %v",
			after.GetObsStamp())
	}
	if _, ok := after.GetCntlrInfo().GetTdIdToRaid0()[extraTd.GetTdId()]; !ok {
		t.Error("the newer request's thin device is not built")
	}
}

// ---------------------------------------------------------------------------
// A sweep that meets stale listings
// ---------------------------------------------------------------------------

// leftoverNames is the sorted list of objects a leftover reply's details
// name, read as LeftoverReply writes them when it names cnt objects and no
// failure; nil if details is not such a reply.
func leftoverNames(details string, cnt int) []string {
	body, ok := strings.CutPrefix(details, fmt.Sprintf("leftover(%d): ", cnt))
	if !ok || strings.Contains(body, "; ") {
		return nil
	}
	names := strings.Split(body, ", ")
	slices.Sort(names)
	return names
}

// TestFakeWorkerSweepMeetsStaleListings drops a cntlr while `dmsetup ls`
// is stale: it names devices of another pool on this node that are already
// gone, as another cntlr's sweep leaves them between a listing and the
// probes after it, which the node-level sweep judges with nothing wanted.
// The sweep removes what is really left over and verifies every removal by
// a probe (CN21), so a name that is gone is nothing to remove and no
// leftover: the CN's syncup names only the dropped cntlr's leg connections
// its disconnects are still taking down, its re-send answers clean, and the
// kept cntlr's syncup and rounds stay clean over the stale listing. A CN
// read over the stale listing names those devices and nothing else, its
// verdict being the sweep with the removals left out (CN30), and the
// SyncupCn that brings answers clean; once the listing is fresh, the CN's
// rounds are clean.
func TestFakeWorkerSweepMeetsStaleListings(t *testing.T) {
	srv, node := newTestServer(t)
	probing(srv, 50*time.Millisecond)
	w := newFakeWorker(t, srv)
	if reply := w.syncupCn(twoCntlrCnReq(1, true, true)); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCn: %v", reply.GetAgentReply())
	}
	for _, req := range []*pb.SyncupCntlrRequest{
		cntlrReq(reqOpts{revision: 1, primary: true}),
		otherCntlrReq(1, true),
	} {
		if reply := w.syncupCntlr(req); reply.GetAgentReply().
			GetCode() != 0 {
			t.Fatalf("SyncupCntlr: %v", reply.GetAgentReply())
		}
	}
	w.settledRound(srv, cntlrPtr(), 1)
	w.settledRound(srv, otherCntlrPtr(), 1)

	ghostSp := fullCntlrPtr(2).GetSpId()
	nf := srv.nf
	ghosts := []string{
		nf.CnGrpName(testCluster, testCn, ghostSp, testMetaGrp),
		nf.CnPoolMetaName(testCluster, testCn, ghostSp, testSlice),
		nf.CnPoolFinalName(testCluster, testCn, ghostSp, testSlice),
	}
	var wantLeft []string
	for _, name := range ghosts {
		wantLeft = append(wantLeft, agent.LeftoverKindDm+":"+name)
	}
	slices.Sort(wantLeft)
	node.mu.Lock()
	node.lsGhosts = ghosts
	node.mu.Unlock()

	w.syncupCnSettled(srv, twoCntlrCnReq(2, true, false))
	if !dmPresent(node, poolNameOf(srv, testSlice)) {
		t.Error("the kept cntlr's pool went with the dropped one's")
	}
	for _, name := range dmNamesContaining(node,
		fmt.Sprintf("%016x", otherCntlrPtr().GetSpId())) {
		t.Errorf("the dropped cntlr's %s survived the sweep", name)
	}
	kept := w.syncupCntlr(cntlrReq(reqOpts{revision: 1, primary: true}))
	assertCleanRound(t, "the kept cntlr's syncup over a stale listing",
		kept, 1, kept.GetCntlrInfo())
	// A CN's read takes its verdict from the same enumeration with the
	// removals left out (CN30), so it names the stale devices as leftovers,
	// and the SyncupCn the worker re-sends for them (dnv-worker.md RW4)
	// probes them gone and answers clean. A cntlr's read covers its own sp.
	for round := 0; round < 3; round++ {
		what := fmt.Sprintf("stale round %d", round)
		w.settledRound(srv, cntlrPtr(), 1)
		w.cnRound(2)
		joinReads(t, srv)
		cn := w.cnRound(2)
		if cn.GetAgentReply().GetCode() != common.ReplyCodeLeftover {
			t.Errorf("%s: cn round over a stale listing = code %d (%s), "+
				"want the read's leftover", what,
				cn.GetAgentReply().GetCode(), cn.GetAgentReply().GetDetails())
		}
		if got := leftoverNames(cn.GetAgentReply().GetDetails(),
			len(wantLeft)); !slices.Equal(got, wantLeft) {
			t.Errorf("%s: the read's leftover %q, want exactly the dm "+
				"ghosts %v", what, cn.GetAgentReply().GetDetails(), wantLeft)
		}
		node.Reset()
		if resent := w.syncupCn(twoCntlrCnReq(2, true, false)); resent.
			GetAgentReply().GetCode() != 0 {
			t.Errorf("%s: re-sent SyncupCn = code %d (%s), want 0", what,
				resent.GetAgentReply().GetCode(),
				resent.GetAgentReply().GetDetails())
		}
		for _, call := range node.Mutations() {
			if !strings.HasPrefix(call, "writeproto ") &&
				!strings.HasPrefix(call, "writeblock ") {
				t.Errorf("%s: the re-sent SyncupCn changed the node: %s",
					what, call)
			}
		}
	}
	node.mu.Lock()
	node.lsGhosts = nil
	node.mu.Unlock()
	w.cnRound(2)
	joinReads(t, srv)
	cn := w.cnRound(2)
	assertCleanRound(t, "fresh cn round", cn, 2, cn.GetCnInfo())
	w.settledRound(srv, cntlrPtr(), 1)
}
