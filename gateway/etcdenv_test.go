package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is gateway.md §9.1 and §9.4: the package's one etcd server, the
// one in-process fake agent, and the handful of helpers every other
// gateway/*_test.go builds its fixture out of. Nothing here tests a handler
// beyond what it takes to prove the fixture itself is sound.
//
// The etcd half is the shape of model/etcdenv_test.go copied verbatim,
// deliberately duplicated rather than factored into a package of its own
// (MD9/EU7): it is a test fixture, and a shared one would put a third package
// between gateway and etcdutil for no gain. Everything goes through etcdutil,
// so the gateway's tests import no etcd client either (layout.md §3).
//
// CONTRACT for the rest of the package — these names and signatures are what
// the other test files are written against:
//
//	newTestClient(t) *etcdutil.Client
//	newTestServer(t) *Server
//	testCid(t) uint64
//	mustPut(t, cli, key, msg)
//	mustCluster(t, s, name) uint64
//	mustDn(t, s, cluster, addr, loc, size) uint64
//	mustCn(t, s, cluster, addr, loc, size) uint64
//	spTok(t, s, cluster, spName) uint64
//	dnTok(t, s, cluster, addrPort) uint64
//	cnTok(t, s, cluster, addrPort) uint64
//	wantCode(t, err, code, label)
//	fakeAddrPort(t, name) string
//	startFakeAgent(t, addrPort, size) *fakeAgent
//
// One rule the signatures do not show: `addr` is a real dial target, because
// CreateDiskNode/CreateControllerNode reach the agent at exactly the
// `addr_port` they store (AG1), and every later RPC addresses the node by that
// same string. Callers therefore take it from fakeAddrPort(t, name) — a unix
// socket, so there is no port to race over and no listener to leak — and never
// from a made-up "dn-a:9000".
//
// This file OWNS the fake agent (fakeAgent, startFakeAgent): mustDn/mustCn are
// part of the contract above and cannot start one they do not define.

// ---------------------------------------------------------------------------
// A real etcd server, shared by every test that needs one (MD9, EU7)
// ---------------------------------------------------------------------------

// testEndpoint is the client URL of the etcd started by TestMain, empty when
// no etcd binary was found.
var testEndpoint string

// findEtcdBin returns the etcd binary named by ETCD_BIN, else the one on
// PATH, else "" (EU7: no dependency on the etcd server module).
func findEtcdBin() string {
	if bin := os.Getenv("ETCD_BIN"); bin != "" {
		info, err := os.Stat(bin)
		if err != nil || info.IsDir() {
			// A misconfigured ETCD_BIN must not look like "no etcd
			// available": silently skipping would hide the whole
			// etcd-backed suite from a run that asked for it (EU7).
			fmt.Fprintf(
				os.Stderr,
				"ETCD_BIN=%q is not a readable file\n",
				bin,
			)
			os.Exit(1)
		}
		return bin
	}
	bin, err := exec.LookPath("etcd")
	if err != nil {
		return ""
	}
	return bin
}

// freePort returns a currently free localhost port.
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// startEtcd runs a single-node etcd on free localhost ports with a temporary
// data dir and waits until it serves.
func startEtcd(bin string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "dnv-gateway-")
	if err != nil {
		return "", nil, err
	}
	cleanupDir := func() { os.RemoveAll(dir) }
	clientPort, err := freePort()
	if err != nil {
		cleanupDir()
		return "", nil, err
	}
	peerPort, err := freePort()
	if err != nil {
		cleanupDir()
		return "", nil, err
	}
	clientUrl := fmt.Sprintf("http://127.0.0.1:%d", clientPort)
	peerUrl := fmt.Sprintf("http://127.0.0.1:%d", peerPort)
	name := "dnv-gateway-test"
	cmd := exec.Command(
		bin,
		"--name", name,
		"--data-dir", filepath.Join(dir, "data"),
		"--listen-client-urls", clientUrl,
		"--advertise-client-urls", clientUrl,
		"--listen-peer-urls", peerUrl,
		"--initial-advertise-peer-urls", peerUrl,
		"--initial-cluster", name+"="+peerUrl,
		"--initial-cluster-token", name,
		"--log-level", "error",
		"--log-outputs", "stderr",
	)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		cleanupDir()
		return "", nil, err
	}
	stop := func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		cleanupDir()
	}
	endpoint := fmt.Sprintf("127.0.0.1:%d", clientPort)
	if err := waitForEtcd(endpoint); err != nil {
		stop()
		return "", nil, err
	}
	return endpoint, stop, nil
}

// waitForEtcd polls the server until one point read succeeds.
func waitForEtcd(endpoint string) error {
	cli, err := etcdutil.New(
		context.Background(),
		[]string{endpoint},
		time.Second,
	)
	if err != nil {
		return err
	}
	defer cli.Close()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, lastErr = cli.Get(ctx, "dnv-gateway-probe", &pb.SpName{})
		cancel()
		if lastErr == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("etcd did not come up: %w", lastErr)
}

// testSockDir is the directory fakeAddrPort puts its unix sockets in, made by
// TestMain so that every socket of the run dies with the run. It is short on
// purpose: a unix socket path is capped at ~104 bytes by the kernel and an
// addr_port at common.MaxStrSize (64) by §7, and both caps apply to the whole
// "unix://" target the gateway dials.
var testSockDir string

func TestMain(m *testing.M) {
	// The gateway emits an LG2 record per RPC and etcdutil one per call
	// (log.md §5.3). The tests assert behavior, not logs, so the records go
	// nowhere and keep the test output readable.
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	// findEtcdBin first, because a misconfigured ETCD_BIN exits before
	// anything else exists and must not leave a socket dir behind.
	bin := findEtcdBin()
	dir, err := os.MkdirTemp("", "dnv-gw-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot make a socket dir: %v\n", err)
		os.Exit(1)
	}
	testSockDir = dir
	if bin != "" {
		endpoint, stop, err := startEtcd(bin)
		if err != nil {
			os.RemoveAll(dir)
			fmt.Fprintf(os.Stderr, "cannot start etcd: %v\n", err)
			os.Exit(1)
		}
		testEndpoint = endpoint
		code := m.Run()
		stop()
		os.RemoveAll(dir)
		os.Exit(code)
	}
	// Loud, not silent: without a binary every etcd-backed test t.Skip()s and
	// the package still prints "ok", so a regression in a handler's STM, its
	// GW7 mapping or its GW6 token check would sail through a plain
	// `go test ./...`. This banner is the only thing that distinguishes that
	// run from a real one (EU7/MD9 allow the skip; they do not allow it to be
	// invisible).
	fmt.Fprintln(
		os.Stderr,
		"SKIPPING every etcd-backed test: no etcd binary "+
			"(set ETCD_BIN or put etcd on PATH)",
	)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// newTestClient returns a Client against the shared etcd, or skips (EU7).
func newTestClient(t *testing.T) *etcdutil.Client {
	t.Helper()
	if testEndpoint == "" {
		t.Skip("no etcd binary (set ETCD_BIN or put etcd on PATH)")
	}
	cli, err := etcdutil.New(
		context.Background(),
		[]string{testEndpoint},
		time.Second,
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := cli.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return cli
}

// testCid gives each test its own cluster id, so that the shared etcd needs no
// cleanup between tests: two clusters never share a key (§5.2). It is for
// tests that write §5 keys directly; a test that goes through CreateCluster
// gets the same isolation from a per-test cluster NAME, because the handler
// folds a fresh creation_epoch into the id it mints.
func testCid(t *testing.T) uint64 {
	t.Helper()
	// The creation epoch is a per-invocation counter, not 0: ClusterId folds
	// it in (§5.2), so every call — including the second and third iteration
	// of the same test under `go test -count=3`, and two tests running in
	// parallel — gets its own key space.
	return model.ClusterId(t.Name(), testCidSeq.Add(1))
}

// testCidSeq numbers the key spaces testCid hands out; testSeq numbers
// everything else this file has to keep unique across a whole run.
var (
	testCidSeq atomic.Uint64
	testSeq    atomic.Uint64
)

// mustPut writes one message or fails the test.
func mustPut(
	t *testing.T,
	cli *etcdutil.Client,
	key string,
	msg proto.Message,
) {
	t.Helper()
	if err := cli.Put(context.Background(), key, msg); err != nil {
		t.Fatalf("Put %s: %v", key, err)
	}
}

// ---------------------------------------------------------------------------
// The in-process agent (§9.4)
// ---------------------------------------------------------------------------
//
// One fake serves BOTH generated agent services on one listener, because the
// two halves of the gateway's agent surface (AG1/AG2) are configured and
// asserted the same way and a test that needs a DN and a CN should not need
// two vocabularies. Only the eight methods the gateway actually calls are
// overridden; anything else stays UNIMPLEMENTED, so an RPC that reaches for a
// call it is not supposed to make fails loudly instead of getting a zero
// value.

// fakeAgent is one fake node: the knobs a test turns and the record of what
// the gateway asked it. Every field is behind mu because the gateway dials it
// over a real connection and the handler goroutine is not the test's (-race
// would see the write otherwise).
type fakeAgent struct {
	// AddrPort is the target the gateway dials, i.e. the addr_port the node
	// is registered under.
	AddrPort string

	mu        sync.Mutex
	size      uint64
	fail      error
	block     chan struct{}
	calls     []string
	traceIds  []string
	dnInfo    *pb.DnInfo
	sideInfo  *pb.SideInfo
	cnInfo    *pb.CnInfo
	cntlrInfo *pb.CntlrInfo
	tdBitmap  []byte
	legBitmap []byte
}

// enter is what every fake method runs first: it records the call and the T3
// trace id the gateway's client interceptor forwarded, then applies the two
// failure knobs. The block knob returns the ctx error rather than nil, which
// is what a real hung agent looks like once the caller's
// DefaultGatewayAgentTimeout budget expires (AG3).
func (f *fakeAgent) enter(ctx context.Context, method string) error {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	traceId, _ := common.TraceIdFromCtx(ctx)
	f.traceIds = append(f.traceIds, traceId)
	fail, block := f.fail, f.block
	f.mu.Unlock()
	if fail != nil {
		return fail
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// setSize changes what GetDnSize/GetCnSize report from the next call on.
func (f *fakeAgent) setSize(size uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.size = size
}

// setFail makes every method answer err, which is how a test drives AG3's
// "non-OK status" arm without killing the listener.
func (f *fakeAgent) setFail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = err
}

// setBlock makes every method hang until the returned func is called or the
// caller's deadline expires — the §9.4 timeout path. The unblock func is
// idempotent so a test can defer it and still call it explicitly.
func (f *fakeAgent) setBlock() func() {
	block := make(chan struct{})
	f.mu.Lock()
	f.block = block
	f.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(block) }) }
}

// setInfo installs the *Info messages the Inspect* RPCs must pass through
// unchanged (§9.4). A nil argument leaves that one alone.
func (f *fakeAgent) setInfo(
	dnInfo *pb.DnInfo,
	sideInfo *pb.SideInfo,
	cnInfo *pb.CnInfo,
	cntlrInfo *pb.CntlrInfo,
) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if dnInfo != nil {
		f.dnInfo = dnInfo
	}
	if sideInfo != nil {
		f.sideInfo = sideInfo
	}
	if cnInfo != nil {
		f.cnInfo = cnInfo
	}
	if cntlrInfo != nil {
		f.cntlrInfo = cntlrInfo
	}
}

// setBitmaps installs the bytes GetThinDeviceBm/GetLegBm return, which GW14
// says the gateway must reply verbatim.
func (f *fakeAgent) setBitmaps(tdBitmap []byte, legBitmap []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tdBitmap = tdBitmap
	f.legBitmap = legBitmap
}

// callCount is how many times one method was called — the count, not the
// order, because a handler is allowed to retry its candidate unit (GW9) and a
// test that asserted an exact sequence would be pinning the retry, not the
// rule.
func (f *fakeAgent) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if call == method {
			count++
		}
	}
	return count
}

// traceIdList is every trace id the fake saw, in call order (T3).
func (f *fakeAgent) traceIdList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.traceIds...)
}

// fakeDnServer is the DiskNodeAgent half of a fakeAgent. It is a separate
// type from the CN half rather than one struct embedding both Unimplemented
// servers: those two carry the same unexported testEmbeddedByValue marker, and
// embedding both would make it ambiguous and silently drop the check
// RegisterXServer does.
type fakeDnServer struct {
	pb.UnimplementedDiskNodeAgentServer

	agent *fakeAgent
}

func (f *fakeDnServer) GetDnSize(
	ctx context.Context,
	req *pb.GetDnSizeRequest,
) (*pb.GetDnSizeReply, error) {
	if err := f.agent.enter(ctx, "GetDnSize"); err != nil {
		return nil, err
	}
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	return &pb.GetDnSizeReply{Size: f.agent.size}, nil
}

func (f *fakeDnServer) GetDnInfo(
	ctx context.Context,
	req *pb.GetDnInfoRequest,
) (*pb.GetDnInfoReply, error) {
	if err := f.agent.enter(ctx, "GetDnInfo"); err != nil {
		return nil, err
	}
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	// The revision the agent reports is the one every Inspect* reply MUST
	// carry back to the client (architecture.md §8.2); it is a distinctive
	// number here so that a handler which regressed to the stored rev key
	// would be caught.
	return &pb.GetDnInfoReply{
		Revision: fakeAgentRevision,
		DnInfo:   f.agent.dnInfo,
	}, nil
}

func (f *fakeDnServer) GetSideInfo(
	ctx context.Context,
	req *pb.GetSideInfoRequest,
) (*pb.GetSideInfoReply, error) {
	if err := f.agent.enter(ctx, "GetSideInfo"); err != nil {
		return nil, err
	}
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	return &pb.GetSideInfoReply{
		Revision: fakeAgentRevision,
		SideInfo: f.agent.sideInfo,
	}, nil
}

// fakeCnServer is the ControllerNodeAgent half of a fakeAgent.
type fakeCnServer struct {
	pb.UnimplementedControllerNodeAgentServer

	agent *fakeAgent
}

func (f *fakeCnServer) GetCnSize(
	ctx context.Context,
	req *pb.GetCnSizeRequest,
) (*pb.GetCnSizeReply, error) {
	if err := f.agent.enter(ctx, "GetCnSize"); err != nil {
		return nil, err
	}
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	return &pb.GetCnSizeReply{Size: f.agent.size}, nil
}

func (f *fakeCnServer) GetCnInfo(
	ctx context.Context,
	req *pb.GetCnInfoRequest,
) (*pb.GetCnInfoReply, error) {
	if err := f.agent.enter(ctx, "GetCnInfo"); err != nil {
		return nil, err
	}
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	return &pb.GetCnInfoReply{
		Revision: fakeAgentRevision,
		CnInfo:   f.agent.cnInfo,
	}, nil
}

func (f *fakeCnServer) GetCntlrInfo(
	ctx context.Context,
	req *pb.GetCntlrInfoRequest,
) (*pb.GetCntlrInfoReply, error) {
	if err := f.agent.enter(ctx, "GetCntlrInfo"); err != nil {
		return nil, err
	}
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	return &pb.GetCntlrInfoReply{
		Revision:  fakeAgentRevision,
		CntlrInfo: f.agent.cntlrInfo,
	}, nil
}

func (f *fakeCnServer) GetThinDeviceBm(
	ctx context.Context,
	req *pb.GetThinDeviceBmRequest,
) (*pb.GetThinDeviceBmReply, error) {
	if err := f.agent.enter(ctx, "GetThinDeviceBm"); err != nil {
		return nil, err
	}
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	return &pb.GetThinDeviceBmReply{Bitmap: f.agent.tdBitmap}, nil
}

func (f *fakeCnServer) GetLegBm(
	ctx context.Context,
	req *pb.GetLegBmRequest,
) (*pb.GetLegBmReply, error) {
	if err := f.agent.enter(ctx, "GetLegBm"); err != nil {
		return nil, err
	}
	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	return &pb.GetLegBmReply{Bitmap: f.agent.legBitmap}, nil
}

// fakeAgentRevision is the `revision` every fake *Info reply carries, and —
// architecture.md §8.2/§8.6 — the number every Inspect* reply MUST carry back
// as its `applied_revision`. A rev key starts at 1 and is bumped one at a
// time, so a value no plausible bump sequence reaches makes a handler that
// regressed to the stored revision unmistakable.
const fakeAgentRevision = uint64(0xfa5e)

// fakeAddrPort returns an addr_port a fake agent can bind and the gateway can
// dial, built from a name the test chooses so a failure message says which
// node it was.
//
// It is a unix socket, not a loopback port: the gateway stores addr_port as
// the node's identity and reaches the agent at exactly that string (AG1), so
// the address has to be real, and a "pick a free port, close it, hope" dance
// would race every other test in the package. A per-run counter keeps two
// tests that pick the same name apart.
func fakeAddrPort(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(
		testSockDir, fmt.Sprintf("%s-%d.sock", name, testSeq.Add(1)))
	addrPort := "unix://" + path
	if len(addrPort) > common.MaxStrSize {
		t.Fatalf(
			"addr_port %q is %d bytes, over the §7 maximum of %d: "+
				"set TMPDIR to something shorter",
			addrPort, len(addrPort), common.MaxStrSize)
	}
	return addrPort
}

// startFakeAgent serves both agent services on addrPort until the test ends.
//
// The network is read off the target the same way grpc.NewClient reads it, so
// the fake listens on exactly what withAgentConn will dial: a "unix://" target
// is a socket path, anything else a host:port. A target that cannot be bound —
// a made-up "dn-a:9000", say — is a fixture mistake and fails here rather than
// as an inscrutable ABORTED out of the handler under test.
//
// Both interceptor chains are wired exactly as agent/agent.go wires them: they
// are what extracts the trace id the gateway's client chain sent (T3), and a
// fake without them could not tell a test whether the gateway forwarded one.
func startFakeAgent(t *testing.T, addrPort string, size uint64) *fakeAgent {
	t.Helper()
	network, target := "tcp", addrPort
	if path, ok := strings.CutPrefix(addrPort, "unix://"); ok {
		network, target = "unix", path
	}
	lis, err := net.Listen(network, target)
	if err != nil {
		t.Fatalf(
			"listen %s %s: %v (take addr_port from fakeAddrPort)",
			network, target, err)
	}
	agent := &fakeAgent{AddrPort: addrPort, size: size}
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
	)
	pb.RegisterDiskNodeAgentServer(server, &fakeDnServer{agent: agent})
	pb.RegisterControllerNodeAgentServer(server, &fakeCnServer{agent: agent})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(lis)
	}()
	t.Cleanup(func() {
		// Stop, not GracefulStop: a test that left the fake blocked would
		// otherwise wait for a call that is never coming back.
		server.Stop()
		<-done
	})
	return agent
}

// ---------------------------------------------------------------------------
// Handler-test helpers
// ---------------------------------------------------------------------------

// newTestServer builds a Server over the shared etcd, with no listener: §9.3
// drives the handlers as plain method calls, so nothing but the gRPC
// interceptors is left out and every assertion is about etcd state and the
// reply message.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	return NewServer(newTestClient(t))
}

// mustCluster creates one cluster and returns its cluster_id.
//
// name is the caller's, not defaulted: the cluster is a test's key space
// (§5.2), and two tests sharing the literal "default" would share every key
// under it. A test that wants the defaulting path exercises it by passing ""
// to the handler under test, not here.
func mustCluster(t *testing.T, s *Server, name string) uint64 {
	t.Helper()
	reply, err := s.CreateCluster(context.Background(),
		&pb.CreateClusterRequest{ClusterName: name})
	if err != nil {
		t.Fatalf("CreateCluster %q: %v", name, err)
	}
	return reply.GetClusterId()
}

// testTrConf is the transport conf a fixture node is registered with. §8.2
// refuses an empty one, and making it distinct per node is what lets a
// CdcEntry or a Side be checked field by field against the node it names.
func testTrConf(addrPort string) *pb.NvmeTrConf {
	return &pb.NvmeTrConf{
		TrType:  "tcp",
		AdrFam:  "ipv4",
		TrAddr:  addrPort,
		TrSvcId: "4420",
	}
}

// mustDn starts a fake dn agent on addr, registers the node and returns its
// dn_id.
//
// The agent comes first because CreateDiskNode probes GetDnSize before it
// writes anything (AG1): without a listener the RPC is ABORTED and no DnConf
// exists. size is the byte count the fake reports, so a caller sizes a node in
// bytes and lets §6.1's floor division decide its total_ext_cnt.
func mustDn(
	t *testing.T,
	s *Server,
	cluster string,
	addr string,
	loc string,
	size uint64,
) uint64 {
	t.Helper()
	startFakeAgent(t, addr, size)
	reply, err := s.CreateDiskNode(context.Background(),
		&pb.CreateDiskNodeRequest{
			ClusterName: cluster,
			AddrPort:    addr,
			NvmeTrConf:  testTrConf(addr),
			Location:    loc,
		})
	if err != nil {
		t.Fatalf("CreateDiskNode %q: %v", addr, err)
	}
	return reply.GetDnId()
}

// mustCn is mustDn for a controller node. size is the budget the fake reports
// to GetCnSize, which cnCapBudget floors at MinCnCap and caps at MaxCnCap
// before §6.1 divides it — so a caller that wants a predictable
// total_ext_cnt passes something inside that window.
func mustCn(
	t *testing.T,
	s *Server,
	cluster string,
	addr string,
	loc string,
	size uint64,
) uint64 {
	t.Helper()
	startFakeAgent(t, addr, size)
	reply, err := s.CreateControllerNode(context.Background(),
		&pb.CreateControllerNodeRequest{
			ClusterName: cluster,
			AddrPort:    addr,
			NvmeTrConf:  testTrConf(addr),
			Location:    loc,
		})
	if err != nil {
		t.Fatalf("CreateControllerNode %q: %v", addr, err)
	}
	return reply.GetCnId()
}

// spTok is the revision an SP-scoped mutator has to put in its SpRev message
// right now — when it sends one at all. GW6 is presence-based: a request that
// carries no SpRev skips the comparison, and one that carries an SpRev is held
// to exact equality against this number, so a token built out of anything else
// (0 included) is ABORTED "stale revision".
//
// The three tok helpers read through the Get* RPCs rather than off the rev
// key, because that is the only way a client ever learns a token: a test that
// reached into etcd for it could pass while the reply that carries it to a
// real caller was empty.
func spTok(t *testing.T, s *Server, cluster string, spName string) uint64 {
	t.Helper()
	reply, err := s.GetStoragePool(context.Background(),
		&pb.GetStoragePoolRequest{ClusterName: cluster, SpName: spName})
	if err != nil {
		t.Fatalf("GetStoragePool %q: %v", spName, err)
	}
	return reply.GetSpRev().GetRevision()
}

// dnTok is spTok for a DN: the revision a DnRev message has to carry to pass
// GW6, for the requests that carry one.
func dnTok(t *testing.T, s *Server, cluster string, addrPort string) uint64 {
	t.Helper()
	reply, err := s.GetDiskNode(context.Background(),
		&pb.GetDiskNodeRequest{ClusterName: cluster, AddrPort: addrPort})
	if err != nil {
		t.Fatalf("GetDiskNode %q: %v", addrPort, err)
	}
	return reply.GetDnRev().GetRevision()
}

// cnTok is spTok for a CN: the revision a CnRev message has to carry to pass
// GW6, for the requests that carry one.
func cnTok(t *testing.T, s *Server, cluster string, addrPort string) uint64 {
	t.Helper()
	reply, err := s.GetControllerNode(context.Background(),
		&pb.GetControllerNodeRequest{ClusterName: cluster, AddrPort: addrPort})
	if err != nil {
		t.Fatalf("GetControllerNode %q: %v", addrPort, err)
	}
	return reply.GetCnRev().GetRevision()
}

// wantCode asserts the GW7 code of one handler result. label names the case so
// a table-driven test says which row failed.
//
// It is fatal rather than cumulative: a row that got the wrong code has
// usually also got a nil reply, and the assertions after it would panic
// instead of reporting.
func wantCode(t *testing.T, err error, want codes.Code, label string) {
	t.Helper()
	if want == codes.OK {
		if err != nil {
			t.Fatalf("%s: got %v, want OK", label, err)
		}
		return
	}
	if err == nil {
		t.Fatalf("%s: got no error, want %s", label, want)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("%s: %v carries no gRPC status, want %s", label, err, want)
	}
	if st.Code() != want {
		t.Fatalf("%s: got %s (%v), want %s", label, st.Code(), err, want)
	}
}

// ---------------------------------------------------------------------------
// The fixture's own tests
// ---------------------------------------------------------------------------

// TestFakeAgentAnswersAndSeesTheTraceId pins the two properties every
// agent-path test rests on: the fake really serves both generated services
// over a real connection, and the trace id the caller put in its ctx arrives
// at the far end (T3, grpc.md §4). It needs no etcd, so it is one of the tests
// that still run when the suite skips.
func TestFakeAgentAnswersAndSeesTheTraceId(t *testing.T) {
	addrPort := fakeAddrPort(t, "trace")
	agent := startFakeAgent(t, addrPort, 7<<30)
	ctx := common.WithTraceId(context.Background(), "gw-trace-id")
	var dnSize uint64
	if err := withDnAgent(ctx, addrPort,
		func(ctx context.Context, client pb.DiskNodeAgentClient) error {
			reply, err := client.GetDnSize(ctx, &pb.GetDnSizeRequest{})
			if err != nil {
				return err
			}
			dnSize = reply.GetSize()
			return nil
		}); err != nil {
		t.Fatalf("GetDnSize: %v", err)
	}
	if dnSize != 7<<30 {
		t.Errorf("GetDnSize: got %d, want %d", dnSize, uint64(7)<<30)
	}
	var cnSize uint64
	if err := withCnAgent(ctx, addrPort,
		func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
			reply, err := client.GetCnSize(ctx, &pb.GetCnSizeRequest{})
			if err != nil {
				return err
			}
			cnSize = reply.GetSize()
			return nil
		}); err != nil {
		t.Fatalf("GetCnSize: %v", err)
	}
	if cnSize != 7<<30 {
		t.Errorf("GetCnSize: got %d, want %d", cnSize, uint64(7)<<30)
	}
	if got := agent.callCount("GetDnSize"); got != 1 {
		t.Errorf("GetDnSize call count: got %d, want 1", got)
	}
	traceIds := agent.traceIdList()
	if len(traceIds) != 2 {
		t.Fatalf("trace ids: got %v, want two entries", traceIds)
	}
	for _, traceId := range traceIds {
		if traceId != "gw-trace-id" {
			t.Errorf("trace id: got %q, want %q", traceId, "gw-trace-id")
		}
	}
}

// TestFakeAgentFailurePropagates pins AG3's input side: a non-OK status from
// the agent reaches the caller of withDnAgent as a status error, which is what
// every handler turns into ABORTED. It needs no etcd.
func TestFakeAgentFailurePropagates(t *testing.T) {
	addrPort := fakeAddrPort(t, "fail")
	agent := startFakeAgent(t, addrPort, 1<<30)
	agent.setFail(status.Error(codes.Internal, "disk is on fire"))
	err := withDnAgent(context.Background(), addrPort,
		func(ctx context.Context, client pb.DiskNodeAgentClient) error {
			_, err := client.GetDnSize(ctx, &pb.GetDnSizeRequest{})
			return err
		})
	wantCode(t, err, codes.Internal, "GetDnSize with a failing agent")
}

// TestTestCidIsolatesKeySpaces pins the fixture rule that makes the shared
// etcd need no cleanup: testCid folds a per-invocation counter into the id
// (§5.2), so two calls — the same test under -count=2 included — never share
// a key prefix. It needs no etcd.
func TestTestCidIsolatesKeySpaces(t *testing.T) {
	first := testCid(t)
	second := testCid(t)
	if first == second {
		t.Errorf("testCid returned %#016x twice", first)
	}
	if model.DnGlobalKey(first) == model.DnGlobalKey(second) {
		t.Errorf("two cids share a key prefix")
	}
}

// TestFixtureBuildsACluster is the fixture end to end: mustCluster,
// mustDn and mustCn must leave exactly the §5 keys CreateCluster,
// CreateDiskNode and CreateControllerNode are specified to write, and the tok
// helpers must return the revisions those RPCs stamped. Everything else in the
// package builds on this, so it is asserted key by key rather than through the
// Get* replies alone.
func TestFixtureBuildsACluster(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	name := fmt.Sprintf("fixture-%d", testSeq.Add(1))
	cid := mustCluster(t, s, name)
	if cid == 0 {
		t.Fatalf("CreateCluster returned cluster_id 0")
	}
	cc := &pb.ClusterConf{}
	found, err := s.cli.Get(ctx, model.ClusterConfKey(name), cc)
	if err != nil {
		t.Fatalf("Get cluster_conf: %v", err)
	}
	if !found {
		t.Fatalf("cluster_conf %q was not written", name)
	}
	if got := model.ClusterId(name, cc.GetCreationEpoch()); got != cid {
		t.Errorf("cluster_id: got %#016x, want %#016x", got, cid)
	}
	// The three globals are CreateCluster's invariant keys: every later mint
	// reads one, and a handler that finds one missing is required to abort
	// rather than invent it.
	dnGlobal := &pb.DnGlobal{}
	if found, err := s.cli.Get(
		ctx, model.DnGlobalKey(cid), dnGlobal,
	); err != nil || !found {
		t.Fatalf("dn_global: found %v, err %v", found, err)
	}
	if dnGlobal.GetNextId() != 1 ||
		len(dnGlobal.GetShardBucket()) != common.ShardBucketSize {
		t.Errorf(
			"dn_global: got next_id %d, bucket len %d, want 1 and %d",
			dnGlobal.GetNextId(), len(dnGlobal.GetShardBucket()),
			common.ShardBucketSize)
	}

	dnAddr := fakeAddrPort(t, "dn")
	// 100 GiB against the default 1 GiB extent is 100 extents, and free ==
	// total on a node that hosts nothing yet (§8.2).
	dnId := mustDn(t, s, name, dnAddr, "rack-1", 100<<30)
	dn := &pb.DnConf{}
	if found, err := s.cli.Get(
		ctx, model.DnConfKey(cid, dnAddr), dn,
	); err != nil || !found {
		t.Fatalf("dn_conf: found %v, err %v", found, err)
	}
	if dn.GetDnId() != dnId {
		t.Errorf("dn_id: stored %d, replied %d", dn.GetDnId(), dnId)
	}
	if dn.GetTotalExtCnt() != 100 || dn.GetFreeExtCnt() != 100 {
		t.Errorf(
			"dn ext counts: got total %d free %d, want 100 and 100",
			dn.GetTotalExtCnt(), dn.GetFreeExtCnt())
	}
	if dn.GetLocation() != "rack-1" {
		t.Errorf("dn location: got %q, want %q", dn.GetLocation(), "rack-1")
	}
	binIdx, ok := model.DnBinIdx(
		dn.GetFreeExtCnt(), cc.GetDnBinConf())
	if !ok {
		t.Fatalf("a 100-extent DN must fall in a bin")
	}
	capKey := model.DnCapacityKey(cid, binIdx, dn.GetFreeExtCnt(), dnAddr)
	if found, err := s.cli.Get(
		ctx, capKey, &pb.DnCapacity{},
	); err != nil || !found {
		t.Fatalf("dn_capacity %s: found %v, err %v", capKey, found, err)
	}
	if got := dnTok(t, s, name, dnAddr); got != 1 {
		t.Errorf("dnTok: got %d, want 1 (a fresh DnRev)", got)
	}

	cnAddr := fakeAddrPort(t, "cn")
	// 1 TiB is inside [MinCnCap, MaxCnCap], so cnCapBudget passes it through
	// and §6.1 turns it into 1024 extents.
	cnId := mustCn(t, s, name, cnAddr, "rack-2", 1<<40)
	cn := &pb.CnConf{}
	if found, err := s.cli.Get(
		ctx, model.CnConfKey(cid, cnAddr), cn,
	); err != nil || !found {
		t.Fatalf("cn_conf: found %v, err %v", found, err)
	}
	if cn.GetCnId() != cnId {
		t.Errorf("cn_id: stored %d, replied %d", cn.GetCnId(), cnId)
	}
	if cn.GetTotalExtCnt() != 1024 || cn.GetFreeExtCnt() != 1024 {
		t.Errorf(
			"cn ext counts: got total %d free %d, want 1024 and 1024",
			cn.GetTotalExtCnt(), cn.GetFreeExtCnt())
	}
	cnCapKey := model.CnCapacityKey(cid, cn.GetFreeExtCnt(), cnAddr)
	if found, err := s.cli.Get(
		ctx, cnCapKey, &pb.CnCapacity{},
	); err != nil || !found {
		t.Fatalf("cn_capacity %s: found %v, err %v", cnCapKey, found, err)
	}
	if got := cnTok(t, s, name, cnAddr); got != 1 {
		t.Errorf("cnTok: got %d, want 1 (a fresh CnRev)", got)
	}
}

// TestFixtureCodeMapping is wantCode against one refusal of every GW7 class
// the fixture RPCs can raise, which pins both the helper and the mapping the
// rest of the package will assert with it. Each row also proves the §0 #17 /
// GW6 ordering it belongs to: the ABORTED row sends a dn_rev message that is
// PRESENT and one ahead of the stored revision, because GW6 compares only the
// token a request actually carries. A request that sends no dn_rev at all
// skips the comparison instead of being refused, and is covered — as a
// success — by TestFixtureAbsentTokenSkipsTheRevisionCheck.
func TestFixtureCodeMapping(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	name := fmt.Sprintf("codes-%d", testSeq.Add(1))
	mustCluster(t, s, name)
	dnAddr := fakeAddrPort(t, "dn")
	mustDn(t, s, name, dnAddr, "rack-1", 100<<30)

	// wantMsg, where a row sets it, is the exact status message: the ABORTED
	// row is GW6's, and "stale revision" (§0 #7) is the whole of what a token
	// mismatch is allowed to say, so the row asserts the sentence and not only
	// the class.
	for _, tc := range []struct {
		label   string
		call    func() error
		want    codes.Code
		wantMsg string
	}{
		{
			label: "cluster_name that breaks ValidStrPattern",
			call: func() error {
				_, err := s.GetCluster(ctx, &pb.GetClusterRequest{
					ClusterName: "not a name",
				})
				return err
			},
			want: codes.InvalidArgument,
		},
		{
			label: "cluster that was never created",
			call: func() error {
				_, err := s.GetCluster(ctx, &pb.GetClusterRequest{
					ClusterName: name + "-nope",
				})
				return err
			},
			want: codes.NotFound,
		},
		{
			label: "cluster created twice",
			call: func() error {
				_, err := s.CreateCluster(ctx, &pb.CreateClusterRequest{
					ClusterName: name,
				})
				return err
			},
			want: codes.AlreadyExists,
		},
		{
			label: "disk node that was never created",
			call: func() error {
				_, err := s.GetDiskNode(ctx, &pb.GetDiskNodeRequest{
					ClusterName: name,
					AddrPort:    dnAddr + "-nope",
				})
				return err
			},
			want: codes.NotFound,
		},
		{
			label: "UpdateDiskNodeDisabled with a present, stale dn_rev",
			call: func() error {
				_, err := s.UpdateDiskNodeDisabled(
					ctx, &pb.UpdateDiskNodeDisabledRequest{
						ClusterName: name,
						AddrPort:    dnAddr,
						Disabled:    true,
						// One ahead of what GetDiskNode hands out right
						// now: GW6 tests a present token for equality, so
						// a revision on either side of the stored one is
						// the same ABORTED "stale revision".
						DnRev: &pb.DnRev{
							AddrPort: dnAddr,
							Revision: dnTok(t, s, name, dnAddr) + 1,
						},
					})
				return err
			},
			want:    codes.Aborted,
			wantMsg: msgStaleRevision,
		},
		{
			label: "DeleteCluster while the cluster still holds a DN",
			call: func() error {
				_, err := s.DeleteCluster(ctx, &pb.DeleteClusterRequest{
					ClusterName: name,
				})
				return err
			},
			want: codes.FailedPrecondition,
		},
	} {
		err := tc.call()
		wantCode(t, err, tc.want, tc.label)
		if tc.wantMsg != "" {
			if got := status.Convert(err).Message(); got != tc.wantMsg {
				t.Errorf("%s: message %q, want %q",
					tc.label, got, tc.wantMsg)
			}
		}
	}

	// The refusals above wrote nothing: the DN is still there, still enabled
	// and still on revision 1. GW6 says a mutator whose present token missed
	// returns before any Put, and an aborted STM commits nothing.
	dn := &pb.DnConf{}
	found, err := s.cli.Get(ctx, model.DnConfKey(
		model.ClusterId(name, testClusterEpoch(t, s, name)), dnAddr), dn)
	if err != nil {
		t.Fatalf("Get dn_conf: %v", err)
	}
	if !found {
		t.Fatalf("dn_conf %q vanished", dnAddr)
	}
	if dn.GetDisabled() {
		t.Errorf("the ABORTED UpdateDiskNodeDisabled must not have written")
	}
	if got := dnTok(t, s, name, dnAddr); got != 1 {
		t.Errorf("dnTok after refusals: got %d, want 1", got)
	}
}

// TestFixtureAbsentTokenSkipsTheRevisionCheck is the other half of GW6 as the
// fixture sees it: the check is keyed on the token MESSAGE being present, not
// on the value it carries.
//
//   - No dn_rev in the request at all: the comparison is skipped, and
//     UpdateDiskNodeDisabled is judged only by its own preconditions — so it
//     succeeds, flips the flag and drops the capacity key (§5.6), while the
//     stored DnRev stays where it was, because Update*Disabled is one of the
//     mutations §5.5 exempts from the bump.
//   - A dn_rev message that IS there but carries revision 0 — whether written
//     out as &pb.DnRev{} or as the echoed addr_port with the revision field
//     left at its zero value — is a real token, not an omission, and is
//     ABORTED "stale revision": stored revisions seed at 1 and only grow, so
//     0 matches nothing. These two rows are what separates "presence" from
//     "non-zero"; a check that had merely been relaxed to skip on 0 would let
//     them through.
//   - A present token that does match is unchanged: it goes through.
//
// It builds its own cluster and DN because the bypass really mutates: sharing
// TestFixtureCodeMapping's fixture would leave that test's "the refusals wrote
// nothing" assertions reading a DN somebody else disabled.
func TestFixtureAbsentTokenSkipsTheRevisionCheck(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	name := fmt.Sprintf("gw6-presence-%d", testSeq.Add(1))
	cid := mustCluster(t, s, name)
	dnAddr := fakeAddrPort(t, "dn")
	dnId := mustDn(t, s, name, dnAddr, "rack-1", 100<<30)

	cc := &pb.ClusterConf{}
	if found, err := s.cli.Get(
		ctx, model.ClusterConfKey(name), cc,
	); err != nil || !found {
		t.Fatalf("cluster_conf %q: found %v, err %v", name, found, err)
	}
	binIdx, ok := model.DnBinIdx(100, cc.GetDnBinConf())
	if !ok {
		t.Fatalf("a 100-extent DN must fall in a bin")
	}
	capKey := model.DnCapacityKey(cid, binIdx, 100, dnAddr)

	// The discriminators first, while the DN is still untouched: a present
	// token can only be refused against the revision the fixture stamped.
	for _, tc := range []struct {
		label string
		tok   *pb.DnRev
	}{
		{label: "an empty dn_rev message", tok: &pb.DnRev{}},
		{
			label: "a dn_rev that echoes only addr_port",
			tok:   &pb.DnRev{AddrPort: dnAddr},
		},
	} {
		_, err := s.UpdateDiskNodeDisabled(
			ctx, &pb.UpdateDiskNodeDisabledRequest{
				ClusterName: name,
				AddrPort:    dnAddr,
				Disabled:    true,
				DnRev:       tc.tok,
			})
		wantCode(t, err, codes.Aborted, tc.label)
		if got := status.Convert(err).Message(); got != msgStaleRevision {
			t.Errorf("%s: message %q, want %q",
				tc.label, got, msgStaleRevision)
		}
	}
	dn := &pb.DnConf{}
	if found, err := s.cli.Get(
		ctx, model.DnConfKey(cid, dnAddr), dn,
	); err != nil || !found {
		t.Fatalf("dn_conf after the refusals: found %v, err %v", found, err)
	}
	if dn.GetDisabled() {
		t.Fatalf("a present revision-0 token must not have written")
	}

	// Now the bypass. Same request minus the dn_rev field: OK, and the reply
	// still names the node it mutated.
	reply, err := s.UpdateDiskNodeDisabled(
		ctx, &pb.UpdateDiskNodeDisabledRequest{
			ClusterName: name,
			AddrPort:    dnAddr,
			Disabled:    true,
		})
	wantCode(t, err, codes.OK, "UpdateDiskNodeDisabled with no dn_rev at all")
	if reply.GetDnId() != dnId {
		t.Errorf("reply dn_id: got %d, want %d", reply.GetDnId(), dnId)
	}
	dn = &pb.DnConf{}
	if found, err := s.cli.Get(
		ctx, model.DnConfKey(cid, dnAddr), dn,
	); err != nil || !found {
		t.Fatalf("dn_conf after the bypass: found %v, err %v", found, err)
	}
	if !dn.GetDisabled() {
		t.Errorf("the tokenless UpdateDiskNodeDisabled must have written")
	}
	// A disabled DN implies no capacity key (§5.6), so MaintainDnCapacity
	// deleting it is the second, independent witness that the mutation ran.
	if found, err := s.cli.Get(
		ctx, capKey, &pb.DnCapacity{},
	); err != nil {
		t.Fatalf("Get dn_capacity: %v", err)
	} else if found {
		t.Errorf("dn_capacity %s survived the disable", capKey)
	}
	// Skipping the comparison is not bumping: Update*Disabled never bumps
	// (§5.5), so the token GetDiskNode hands out is still the fixture's 1.
	if got := dnTok(t, s, name, dnAddr); got != 1 {
		t.Errorf("dnTok after the tokenless disable: got %d, want 1", got)
	}

	// And presence still works the moment the value is right: the token the
	// node is actually on re-enables it.
	if _, err := s.UpdateDiskNodeDisabled(
		ctx, &pb.UpdateDiskNodeDisabledRequest{
			ClusterName: name,
			AddrPort:    dnAddr,
			Disabled:    false,
			DnRev: &pb.DnRev{
				AddrPort: dnAddr,
				Revision: dnTok(t, s, name, dnAddr),
			},
		}); err != nil {
		t.Fatalf("UpdateDiskNodeDisabled with the matching dn_rev: %v", err)
	}
	dn = &pb.DnConf{}
	if found, err := s.cli.Get(
		ctx, model.DnConfKey(cid, dnAddr), dn,
	); err != nil || !found {
		t.Fatalf("dn_conf after the re-enable: found %v, err %v", found, err)
	}
	if dn.GetDisabled() {
		t.Errorf("the matching-token UpdateDiskNodeDisabled must have written")
	}
}

// testClusterEpoch reads back one cluster's creation_epoch, which is the only
// stored half of the derived cluster_id (§5.2).
func testClusterEpoch(t *testing.T, s *Server, name string) uint64 {
	t.Helper()
	cc := &pb.ClusterConf{}
	found, err := s.cli.Get(
		context.Background(), model.ClusterConfKey(name), cc)
	if err != nil {
		t.Fatalf("Get cluster_conf %q: %v", name, err)
	}
	if !found {
		t.Fatalf("cluster_conf %q not found", name)
	}
	return cc.GetCreationEpoch()
}
