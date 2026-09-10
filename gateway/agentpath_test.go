package gateway

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is gateway.md §9.4, §9.5 and §9.6: the three groups of tests that
// exercise the gateway as a gRPC PEER rather than as a pile of handlers —
// the ten agent calls of §6 against a real in-process agent, the served
// surface of §3 over bufconn, and one Server driven by two goroutines at once.
//
// The three belong together because they share one need the handler tests do
// not have: a second process-like party on the other end of a socket. §9.4's
// fake is that party for the outbound calls (the gateway is the client),
// §9.5's bufconn harness is it for the inbound ones (the gateway is the
// server), and §9.6's two goroutines are the "two instances" of §0 #3 reduced
// to the one case that can be tested in process — etcd, not the gateway, is
// what serializes them, so one Server with two callers is the same experiment
// as two gateways with one caller each.
//
// Everything here is prefixed `agentpath` / `serving` / `race` so it cannot
// collide with the per-resource handler tests of §9.3, which own the
// unprefixed helper names.

// ---------------------------------------------------------------------------
// §9.4 — the in-process agent pair
// ---------------------------------------------------------------------------

// agentpathSeq numbers the cluster names, addresses and trace ids the helpers
// below hand out. The package's etcd is shared by every test, so a name that
// repeats is a test reading another test's keys; deriving names from a
// process-wide counter keeps them distinct across `-count=N` and across
// parallel subtests without any cleanup between tests (§5.2).
var agentpathSeq atomic.Uint64

// agentpathName mints a store-unique name of the given kind. It stays inside
// common.ValidStrPattern so it can be used as a cluster name, an SP name or a
// trace id interchangeably.
func agentpathName(kind string) string {
	return fmt.Sprintf("agentpath-%s-%d", kind, agentpathSeq.Add(1))
}

// agentpathFake is one process serving BOTH generated agent services on one
// real TCP port (§9.4). One struct for both is not a shortcut: a DN and a CN
// are separate processes in production, but the gateway addresses either by
// `addr_port` alone and builds its stub from the RPC it is about to make, so
// a single endpoint answering both proves the stub selection as well as two
// endpoints would, and lets one test drive CreateDiskNode and
// CreateControllerNode against the same fake.
//
// It is a real listener rather than a bufconn because withAgentConn dials
// `addr_port` with grpc.NewClient and no dialer override (AG2): a bufconn
// would require the production code to accept an injected dialer, which is
// exactly the seam §9.4 refuses to add.
type agentpathFake struct {
	pb.UnimplementedDiskNodeAgentServer
	pb.UnimplementedControllerNodeAgentServer

	// addrPort is what a DnConf / CnConf must carry for the gateway to reach
	// this fake.
	addrPort string

	server   *grpc.Server
	stopOnce sync.Once

	mu sync.Mutex
	// dnSize and cnSize are what GetDnSize / GetCnSize report; §6.1 turns
	// them into total_ext_cnt.
	dnSize uint64
	cnSize uint64
	// dnInfo and cnInfo are what GetDnInfo / GetCnInfo report.
	dnInfo *pb.DnInfo
	cnInfo *pb.CnInfo
	// agentRev is the `revision` the agent's own Get*Info reply carries —
	// the number every Inspect* reply must carry back as its
	// `applied_revision` (update_04.md U2–U4). It is deliberately different
	// from every revision written to etcd, so a handler that regressed to
	// the stored one fails.
	agentRev uint64
	// cntlrInfo and sideInfo are what GetCntlrInfo / GetSideInfo report.
	cntlrInfo *pb.CntlrInfo
	sideInfo  *pb.SideInfo
	// hang makes every call block until its ctx ends, which is the only way
	// to exercise the AG2 timeout without touching the constant.
	hang bool

	traceIds   []string
	dnSizeReqs []*pb.GetDnSizeRequest
	cnSizeReqs []*pb.GetCnSizeRequest
	dnInfoReqs []*pb.GetDnInfoRequest
	cnInfoReqs []*pb.GetCnInfoRequest

	cntlrInfoReqs []*pb.GetCntlrInfoRequest
	sideInfoReqs  []*pb.GetSideInfoRequest
}

// agentpathStartAgent starts a fake on 127.0.0.1:0 and stops it at the end of
// the test. Both interceptor chains of grpc.md §4 are installed because that
// is what a real agent runs; the extra capture interceptor in front of them
// records the raw incoming metadata, which is what T3 is actually about — the
// gateway must put common.TraceIdMetadataKey on the wire, not merely carry a
// trace id in its own ctx.
func agentpathStartAgent(t *testing.T) *agentpathFake {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fake := &agentpathFake{addrPort: lis.Addr().String()}
	fake.server = grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			fake.captureInterceptor(),
			common.GrpcUnaryServerInterceptor(),
		),
		grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
	)
	pb.RegisterDiskNodeAgentServer(fake.server, fake)
	pb.RegisterControllerNodeAgentServer(fake.server, fake)
	go func() {
		_ = fake.server.Serve(lis)
	}()
	t.Cleanup(fake.stop)
	return fake
}

// stop shuts the fake down; it is idempotent so a test can make the agent
// unreachable mid-test and still let the cleanup run.
func (f *agentpathFake) stop() {
	f.stopOnce.Do(func() { f.server.Stop() })
}

// captureInterceptor records the trace-id metadata of every unary call.
func (f *agentpathFake) captureInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			f.mu.Lock()
			f.traceIds = append(
				f.traceIds, md.Get(common.TraceIdMetadataKey)...)
			f.mu.Unlock()
		}
		return handler(ctx, req)
	}
}

// setSizes fixes what the two size probes report.
func (f *agentpathFake) setSizes(dnSize uint64, cnSize uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dnSize, f.cnSize = dnSize, cnSize
}

// setInfos fixes what the two node info probes report, together with the
// revision the agent claims — which every Inspect* reply of the gateway must
// echo as its `applied_revision` (update_04.md U2–U4).
func (f *agentpathFake) setInfos(
	dnInfo *pb.DnInfo,
	cnInfo *pb.CnInfo,
	agentRev uint64,
) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dnInfo, f.cnInfo, f.agentRev = dnInfo, cnInfo, agentRev
}

// setSpInfos fixes what the two SP-object info probes report; the revision
// they carry is the same agentRev setInfos fixes.
func (f *agentpathFake) setSpInfos(
	cntlrInfo *pb.CntlrInfo,
	sideInfo *pb.SideInfo,
) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cntlrInfo, f.sideInfo = cntlrInfo, sideInfo
}

// setHang makes every call block until its ctx ends.
func (f *agentpathFake) setHang(hang bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hang = hang
}

// seenTraceIds is every trace id the fake has been sent, in arrival order.
func (f *agentpathFake) seenTraceIds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.traceIds...)
}

// block is the shared body of every call while `hang` is set: it returns only
// when the caller's deadline — withAgentConn's AG2 budget — expires.
func (f *agentpathFake) block(ctx context.Context) error {
	f.mu.Lock()
	hang := f.hang
	f.mu.Unlock()
	if !hang {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *agentpathFake) GetDnSize(
	ctx context.Context,
	req *pb.GetDnSizeRequest,
) (*pb.GetDnSizeReply, error) {
	f.mu.Lock()
	f.dnSizeReqs = append(f.dnSizeReqs, req)
	size := f.dnSize
	f.mu.Unlock()
	if err := f.block(ctx); err != nil {
		return nil, err
	}
	return &pb.GetDnSizeReply{Size: size}, nil
}

func (f *agentpathFake) GetCnSize(
	ctx context.Context,
	req *pb.GetCnSizeRequest,
) (*pb.GetCnSizeReply, error) {
	f.mu.Lock()
	f.cnSizeReqs = append(f.cnSizeReqs, req)
	size := f.cnSize
	f.mu.Unlock()
	if err := f.block(ctx); err != nil {
		return nil, err
	}
	return &pb.GetCnSizeReply{Size: size}, nil
}

func (f *agentpathFake) GetDnInfo(
	ctx context.Context,
	req *pb.GetDnInfoRequest,
) (*pb.GetDnInfoReply, error) {
	f.mu.Lock()
	f.dnInfoReqs = append(f.dnInfoReqs, req)
	info, rev := f.dnInfo, f.agentRev
	f.mu.Unlock()
	if err := f.block(ctx); err != nil {
		return nil, err
	}
	return &pb.GetDnInfoReply{Revision: rev, DnInfo: info}, nil
}

func (f *agentpathFake) GetCnInfo(
	ctx context.Context,
	req *pb.GetCnInfoRequest,
) (*pb.GetCnInfoReply, error) {
	f.mu.Lock()
	f.cnInfoReqs = append(f.cnInfoReqs, req)
	info, rev := f.cnInfo, f.agentRev
	f.mu.Unlock()
	if err := f.block(ctx); err != nil {
		return nil, err
	}
	return &pb.GetCnInfoReply{Revision: rev, CnInfo: info}, nil
}

func (f *agentpathFake) GetCntlrInfo(
	ctx context.Context,
	req *pb.GetCntlrInfoRequest,
) (*pb.GetCntlrInfoReply, error) {
	f.mu.Lock()
	f.cntlrInfoReqs = append(f.cntlrInfoReqs, req)
	info, rev := f.cntlrInfo, f.agentRev
	f.mu.Unlock()
	if err := f.block(ctx); err != nil {
		return nil, err
	}
	return &pb.GetCntlrInfoReply{Revision: rev, CntlrInfo: info}, nil
}

func (f *agentpathFake) GetSideInfo(
	ctx context.Context,
	req *pb.GetSideInfoRequest,
) (*pb.GetSideInfoReply, error) {
	f.mu.Lock()
	f.sideInfoReqs = append(f.sideInfoReqs, req)
	info, rev := f.sideInfo, f.agentRev
	f.mu.Unlock()
	if err := f.block(ctx); err != nil {
		return nil, err
	}
	return &pb.GetSideInfoReply{Revision: rev, SideInfo: info}, nil
}

// agentpathDeadAddr returns an address nothing listens on: a port is bound to
// learn a free one and released again. A refused connection is what AG3 calls
// "unreachable", and it fails in milliseconds — grpc.NewClient's RPCs are not
// wait-for-ready, so a TRANSIENT_FAILURE subchannel fails the call at once
// rather than retrying to the deadline.
func agentpathDeadAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	if err := lis.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

// agentpathTraceCtx returns a ctx carrying a fresh trace id, plus the id, so
// a test can assert what reached the agent (T3).
func agentpathTraceCtx() (context.Context, string) {
	traceId := agentpathName("trace")
	return common.WithTraceId(context.Background(), traceId), traceId
}

// agentpathTrConf is a transport conf for one node; §8.2 refuses an empty one.
func agentpathTrConf(addrPort string) *pb.NvmeTrConf {
	return &pb.NvmeTrConf{
		TrType:  "tcp",
		AdrFam:  "ipv4",
		TrAddr:  addrPort,
		TrSvcId: "4420",
	}
}

// agentpathServer builds a Server on the package's etcd, plus a ctx with a
// trace id on it. Every test that writes to etcd goes through it.
func agentpathServer(t *testing.T) (*Server, *etcdutil.Client) {
	t.Helper()
	cli := newTestClient(t)
	return NewServer(cli), cli
}

// agentpathCreateCluster creates one cluster through the real handler and
// returns its name and derived id.
func agentpathCreateCluster(
	t *testing.T,
	srv *Server,
) (string, uint64) {
	t.Helper()
	name := agentpathName("cluster")
	reply, err := srv.CreateCluster(
		context.Background(), &pb.CreateClusterRequest{ClusterName: name})
	if err != nil {
		t.Fatalf("CreateCluster %s: %v", name, err)
	}
	return name, reply.GetClusterId()
}

// agentpathSeedCluster writes a ClusterConf directly, for the tests whose
// subject is a refusal reached before any cluster-level state matters. It
// stamps its own creation_epoch, so the cluster_id it returns is the one
// resolveCluster will derive (§5.2).
func agentpathSeedCluster(
	t *testing.T,
	cli *etcdutil.Client,
) (string, uint64) {
	t.Helper()
	name := agentpathName("cluster")
	conf := &pb.ClusterConf{CreationEpoch: uint64(time.Now().UnixNano())}
	agentpathPut(t, cli, model.ClusterConfKey(name), conf)
	return name, model.ClusterId(name, conf.GetCreationEpoch())
}

// agentpathPut writes one message or fails the test.
func agentpathPut(
	t *testing.T,
	cli *etcdutil.Client,
	key string,
	msg proto.Message,
) {
	t.Helper()
	if err := cli.Put(context.Background(), key, msg); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// agentpathGet reads one message and reports whether the key exists.
func agentpathGet(
	t *testing.T,
	cli *etcdutil.Client,
	key string,
	msg proto.Message,
) bool {
	t.Helper()
	found, err := cli.Get(context.Background(), key, msg)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return found
}

// agentpathMustGet is agentpathGet for a key the test requires to be there.
func agentpathMustGet(
	t *testing.T,
	cli *etcdutil.Client,
	key string,
	msg proto.Message,
) {
	t.Helper()
	if !agentpathGet(t, cli, key, msg) {
		t.Fatalf("key %s is missing", key)
	}
}

// agentpathCode is the gRPC code of err, codes.OK for nil.
func agentpathCode(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	return status.Code(err)
}

// agentpathBucketSum is the cluster's live object count of one kind: §5.4
// makes sum(shard_bucket) exactly that by construction.
func agentpathBucketSum(bucket []uint32) uint32 {
	var total uint32
	for _, value := range bucket {
		total += value
	}
	return total
}

// TestAgentPathCreateDiskNodeConsumesAgentSize pins AG1 and §6.1 for
// CreateDiskNode: the disk size is the agent's, not the request's, it is
// consumed by the STM that follows the call, and total_ext_cnt rounds DOWN
// to whole extents of the cluster's extent_size.
//
// The size-below-one-extent row also pins house rule 8 for this path: the
// refusal happens inside the STM, so it must leave no DnConf and no minted
// id behind.
func TestAgentPathCreateDiskNodeConsumesAgentSize(t *testing.T) {
	extSize := uint64(common.DefaultDnExtSize)
	tests := []struct {
		name        string
		dnSize      uint64
		wantExtCnt  uint64
		wantCode    codes.Code
		wantNextId  uint64
		wantBuckets uint32
	}{
		{
			name: "whole extents plus a remainder round down",
			// The remainder is what proves the division is not a ceiling:
			// a DN may never advertise an extent it cannot fully back.
			dnSize:      3*extSize + extSize/2,
			wantExtCnt:  3,
			wantCode:    codes.OK,
			wantNextId:  2,
			wantBuckets: 1,
		},
		{
			name:        "exactly one extent is allocatable",
			dnSize:      extSize,
			wantExtCnt:  1,
			wantCode:    codes.OK,
			wantNextId:  2,
			wantBuckets: 1,
		},
		{
			name: "less than one extent is refused and writes nothing",
			// next_id and the bucket must still be the fresh cluster's:
			// an aborted STM commits neither the DnConf nor the mint.
			dnSize:      extSize - 1,
			wantCode:    codes.InvalidArgument,
			wantNextId:  1,
			wantBuckets: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, cli := agentpathServer(t)
			fake := agentpathStartAgent(t)
			fake.setSizes(tt.dnSize, 0)
			clusterName, cid := agentpathCreateCluster(t, srv)

			reply, err := srv.CreateDiskNode(
				context.Background(), &pb.CreateDiskNodeRequest{
					ClusterName: clusterName,
					AddrPort:    fake.addrPort,
					NvmeTrConf:  agentpathTrConf(fake.addrPort),
				})
			if got := agentpathCode(err); got != tt.wantCode {
				t.Fatalf("CreateDiskNode code = %v (%v), want %v",
					got, err, tt.wantCode)
			}

			// The probe is made either way: §6.1 cannot judge the size
			// before it has it, so even the refusal has reached the agent.
			fake.mu.Lock()
			sizeReqs := append(
				[]*pb.GetDnSizeRequest(nil), fake.dnSizeReqs...)
			fake.mu.Unlock()
			if len(sizeReqs) != 1 {
				t.Fatalf("GetDnSize calls = %d, want 1", len(sizeReqs))
			}
			if sizeReqs[0].GetClusterId() != cid {
				t.Errorf("GetDnSize cluster_id = %#x, want %#x",
					sizeReqs[0].GetClusterId(), cid)
			}
			if sizeReqs[0].GetDnId() != 0 {
				t.Errorf("GetDnSize dn_id = %d, want 0 (§8.2: the node has "+
					"no id yet)", sizeReqs[0].GetDnId())
			}

			dn := &pb.DnConf{}
			found := agentpathGet(
				t, cli, model.DnConfKey(cid, fake.addrPort), dn)
			if tt.wantCode != codes.OK {
				if found {
					t.Errorf("dn_conf was written by a refused create")
				}
			} else {
				if !found {
					t.Fatalf("dn_conf is missing after an OK create")
				}
				if dn.GetDnId() != reply.GetDnId() {
					t.Errorf("dn_conf dn_id = %d, reply said %d",
						dn.GetDnId(), reply.GetDnId())
				}
				if dn.GetTotalExtCnt() != tt.wantExtCnt {
					t.Errorf("total_ext_cnt = %d, want %d",
						dn.GetTotalExtCnt(), tt.wantExtCnt)
				}
				if dn.GetFreeExtCnt() != tt.wantExtCnt {
					t.Errorf("free_ext_cnt = %d, want %d (a fresh node hosts "+
						"no side)", dn.GetFreeExtCnt(), tt.wantExtCnt)
				}
			}

			global := &pb.DnGlobal{}
			agentpathMustGet(t, cli, model.DnGlobalKey(cid), global)
			if global.GetNextId() != tt.wantNextId {
				t.Errorf("dn_global next_id = %d, want %d",
					global.GetNextId(), tt.wantNextId)
			}
			if got := agentpathBucketSum(
				global.GetShardBucket()); got != tt.wantBuckets {
				t.Errorf("sum(dn_global shard_bucket) = %d, want %d",
					got, tt.wantBuckets)
			}
		})
	}
}

// TestAgentPathCreateControllerNodeCapBudget pins §6.1's CN budget rule, the
// one place the CN flow deliberately differs from its DN twin: GetCnSize is
// an operator's opinion, so 0 and anything below MinCnCap take DefaultCnCap
// and anything above MaxCnCap is clamped to it.
//
// The MinCnCap row is the boundary that makes the floor a substitution and
// not a clamp-up: exactly MinCnCap is believed, and 1 KiB buys no extent, so
// the create is refused rather than silently promoted to 4 TiB.
func TestAgentPathCreateControllerNodeCapBudget(t *testing.T) {
	extSize := uint64(common.DefaultDnExtSize)
	tests := []struct {
		name       string
		cnSize     uint64
		wantExtCnt uint64
		wantCode   codes.Code
	}{
		{
			name:       "zero takes the default budget",
			cnSize:     0,
			wantExtCnt: common.DefaultCnCap / extSize,
			wantCode:   codes.OK,
		},
		{
			name:       "below the floor takes the default budget too",
			cnSize:     common.MinCnCap - 1,
			wantExtCnt: common.DefaultCnCap / extSize,
			wantCode:   codes.OK,
		},
		{
			name:     "exactly the floor is believed and buys no extent",
			cnSize:   common.MinCnCap,
			wantCode: codes.InvalidArgument,
		},
		{
			name:       "an ordinary budget passes through",
			cnSize:     3 * extSize,
			wantExtCnt: 3,
			wantCode:   codes.OK,
		},
		{
			name:       "above the ceiling is clamped to it",
			cnSize:     common.MaxCnCap + extSize,
			wantExtCnt: common.MaxCnCap / extSize,
			wantCode:   codes.OK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, cli := agentpathServer(t)
			fake := agentpathStartAgent(t)
			fake.setSizes(0, tt.cnSize)
			clusterName, cid := agentpathCreateCluster(t, srv)

			reply, err := srv.CreateControllerNode(
				context.Background(), &pb.CreateControllerNodeRequest{
					ClusterName: clusterName,
					AddrPort:    fake.addrPort,
					NvmeTrConf:  agentpathTrConf(fake.addrPort),
				})
			if got := agentpathCode(err); got != tt.wantCode {
				t.Fatalf("CreateControllerNode code = %v (%v), want %v",
					got, err, tt.wantCode)
			}
			cn := &pb.CnConf{}
			found := agentpathGet(
				t, cli, model.CnConfKey(cid, fake.addrPort), cn)
			if tt.wantCode != codes.OK {
				if found {
					t.Fatalf("cn_conf was written by a refused create")
				}
				return
			}
			if !found {
				t.Fatalf("cn_conf is missing after an OK create")
			}
			if cn.GetCnId() != reply.GetCnId() {
				t.Errorf("cn_conf cn_id = %d, reply said %d",
					cn.GetCnId(), reply.GetCnId())
			}
			if cn.GetTotalExtCnt() != tt.wantExtCnt {
				t.Errorf("total_ext_cnt = %d, want %d",
					cn.GetTotalExtCnt(), tt.wantExtCnt)
			}
			if cn.GetFreeExtCnt() != tt.wantExtCnt {
				t.Errorf("free_ext_cnt = %d, want %d",
					cn.GetFreeExtCnt(), tt.wantExtCnt)
			}
		})
	}
}

// TestAgentPathInspectRepliesTheAppliedRevision pins update_04.md U2–U4: an
// Inspect* reply's `applied_revision` is the one the agent's own reply
// carries — its last applied revision — never the one stored in an etcd rev
// key. The rev keys are hand-written to a value the agent does not report, so
// a handler that regressed to the stored revision — issue_03.md I1's interim
// reading — fails here.
//
// The *Info message travels with it: both fields are the agent's, verbatim,
// because the whole point of Inspect* is one coherent live snapshot the
// store does not have.
func TestAgentPathInspectRepliesTheAppliedRevision(t *testing.T) {
	const storedRev = uint64(7)
	const agentRev = uint64(99)
	wantDnInfo := &pb.DnInfo{
		DiskInfo: &pb.ResInfo{
			ResName: "disk",
			Status:  pb.ResStatus_RES_STATUS_OK,
			Details: "agentpath disk",
			Epoch:   3,
		},
	}
	wantCnInfo := &pb.CnInfo{
		PortInfo: &pb.ResInfo{
			ResName: "port",
			Status:  pb.ResStatus_RES_STATUS_OK,
			Details: "agentpath port",
			Epoch:   4,
		},
	}
	wantCntlrInfo := &pb.CntlrInfo{
		SsIdToSubsystem: map[uint64]*pb.ResInfo{
			1: {
				ResName: "subsystem",
				Status:  pb.ResStatus_RES_STATUS_OK,
				Details: "agentpath subsystem",
				Epoch:   5,
			},
		},
	}
	wantSideInfo := &pb.SideInfo{
		SideDevInfo: &pb.ResInfo{
			ResName: "side_dev",
			Status:  pb.ResStatus_RES_STATUS_OK,
			Details: "agentpath side dev",
			Epoch:   6,
		},
	}

	t.Run("disk node", func(t *testing.T) {
		srv, cli := agentpathServer(t)
		fake := agentpathStartAgent(t)
		fake.setSizes(uint64(common.DefaultDnExtSize), 0)
		fake.setInfos(wantDnInfo, nil, agentRev)
		clusterName, cid := agentpathCreateCluster(t, srv)
		created, err := srv.CreateDiskNode(
			context.Background(), &pb.CreateDiskNodeRequest{
				ClusterName: clusterName,
				AddrPort:    fake.addrPort,
				NvmeTrConf:  agentpathTrConf(fake.addrPort),
			})
		if err != nil {
			t.Fatalf("CreateDiskNode: %v", err)
		}
		dn := &pb.DnConf{}
		agentpathMustGet(t, cli, model.DnConfKey(cid, fake.addrPort), dn)
		// The dn-worker owns this key in production; writing it by hand is
		// how the test makes the stored revision differ both from the fresh
		// 1 and from what the agent reports — the reply must carry neither
		// of these, only agentRev.
		agentpathPut(t, cli,
			model.DnRevKey(dn.GetShardCode(), cid, dn.GetDnId()),
			&pb.DnRev{AddrPort: fake.addrPort, Revision: storedRev})

		reply, err := srv.InspectDiskNode(
			context.Background(), &pb.InspectDiskNodeRequest{
				ClusterName: clusterName,
				AddrPort:    fake.addrPort,
			})
		if err != nil {
			t.Fatalf("InspectDiskNode: %v", err)
		}
		if reply.GetAppliedRevision() != agentRev {
			t.Errorf("applied_revision = %d, want the agent's %d (stored %d)",
				reply.GetAppliedRevision(), agentRev, storedRev)
		}
		if !proto.Equal(reply.GetDnInfo(), wantDnInfo) {
			t.Errorf("dn_info = %v, want the agent's %v",
				reply.GetDnInfo(), wantDnInfo)
		}
		fake.mu.Lock()
		infoReqs := append([]*pb.GetDnInfoRequest(nil), fake.dnInfoReqs...)
		fake.mu.Unlock()
		if len(infoReqs) != 1 {
			t.Fatalf("GetDnInfo calls = %d, want 1", len(infoReqs))
		}
		if infoReqs[0].GetClusterId() != cid ||
			infoReqs[0].GetDnId() != created.GetDnId() {
			t.Errorf("GetDnInfo asked for (%#x, %d), want (%#x, %d)",
				infoReqs[0].GetClusterId(), infoReqs[0].GetDnId(),
				cid, created.GetDnId())
		}
	})

	t.Run("controller node", func(t *testing.T) {
		srv, cli := agentpathServer(t)
		fake := agentpathStartAgent(t)
		fake.setSizes(0, uint64(common.DefaultDnExtSize))
		fake.setInfos(nil, wantCnInfo, agentRev)
		clusterName, cid := agentpathCreateCluster(t, srv)
		created, err := srv.CreateControllerNode(
			context.Background(), &pb.CreateControllerNodeRequest{
				ClusterName: clusterName,
				AddrPort:    fake.addrPort,
				NvmeTrConf:  agentpathTrConf(fake.addrPort),
			})
		if err != nil {
			t.Fatalf("CreateControllerNode: %v", err)
		}
		cn := &pb.CnConf{}
		agentpathMustGet(t, cli, model.CnConfKey(cid, fake.addrPort), cn)
		agentpathPut(t, cli,
			model.CnRevKey(cn.GetShardCode(), cid, cn.GetCnId()),
			&pb.CnRev{AddrPort: fake.addrPort, Revision: storedRev})

		reply, err := srv.InspectControllerNode(
			context.Background(), &pb.InspectControllerNodeRequest{
				ClusterName: clusterName,
				AddrPort:    fake.addrPort,
			})
		if err != nil {
			t.Fatalf("InspectControllerNode: %v", err)
		}
		if reply.GetAppliedRevision() != agentRev {
			t.Errorf("applied_revision = %d, want the agent's %d (stored %d)",
				reply.GetAppliedRevision(), agentRev, storedRev)
		}
		if !proto.Equal(reply.GetCnInfo(), wantCnInfo) {
			t.Errorf("cn_info = %v, want the agent's %v",
				reply.GetCnInfo(), wantCnInfo)
		}
		fake.mu.Lock()
		infoReqs := append([]*pb.GetCnInfoRequest(nil), fake.cnInfoReqs...)
		fake.mu.Unlock()
		if len(infoReqs) != 1 {
			t.Fatalf("GetCnInfo calls = %d, want 1", len(infoReqs))
		}
		if infoReqs[0].GetClusterId() != cid ||
			infoReqs[0].GetCnId() != created.GetCnId() {
			t.Errorf("GetCnInfo asked for (%#x, %d), want (%#x, %d)",
				infoReqs[0].GetClusterId(), infoReqs[0].GetCnId(),
				cid, created.GetCnId())
		}
	})

	// spFixture is the shared bed of the cntlr and side subtests: two fakes,
	// each serving both agent services — the first registered as a DN and
	// the one CN, the second as the second DN the whole-SP distinct-DN rule
	// (D-F) demands (1 slice is still a meta group AND a data group) — under
	// the smallest SP CreateStoragePool accepts (1 slice, 1 cntlr, RedundNone
	// — one leg per group). The sp-worker owns SpRev in production; writing
	// it by hand to storedRev makes the stored number one the agent does not
	// report, so a regressed handler cannot pass.
	type spFixture struct {
		srv     *Server
		cli     *etcdutil.Client
		fakes   map[string]*agentpathFake // by addr_port
		dnIds   map[string]uint64         // by addr_port
		cluster string
		cid     uint64
		cnId    uint64
		spName  string
		conf    *pb.SpConf
	}
	newSpFixture := func(t *testing.T) *spFixture {
		t.Helper()
		srv, cli := agentpathServer(t)
		clusterName, cid := agentpathCreateCluster(t, srv)
		ctx := context.Background()
		fx := &spFixture{
			srv:     srv,
			cli:     cli,
			fakes:   make(map[string]*agentpathFake),
			dnIds:   make(map[string]uint64),
			cluster: clusterName,
			cid:     cid,
		}
		for i := 0; i < 2; i++ {
			fake := agentpathStartAgent(t)
			fake.setSizes(64<<30, 64<<30)
			fake.setInfos(nil, nil, agentRev)
			fake.setSpInfos(wantCntlrInfo, wantSideInfo)
			fx.fakes[fake.addrPort] = fake
			dnCreated, err := srv.CreateDiskNode(
				ctx, &pb.CreateDiskNodeRequest{
					ClusterName: clusterName,
					AddrPort:    fake.addrPort,
					NvmeTrConf:  agentpathTrConf(fake.addrPort),
				})
			if err != nil {
				t.Fatalf("CreateDiskNode: %v", err)
			}
			fx.dnIds[fake.addrPort] = dnCreated.GetDnId()
			if i == 0 {
				cnCreated, err := srv.CreateControllerNode(
					ctx, &pb.CreateControllerNodeRequest{
						ClusterName: clusterName,
						AddrPort:    fake.addrPort,
						NvmeTrConf:  agentpathTrConf(fake.addrPort),
					})
				if err != nil {
					t.Fatalf("CreateControllerNode: %v", err)
				}
				fx.cnId = cnCreated.GetCnId()
			}
		}
		fx.spName = agentpathName("sp")
		if _, err := srv.CreateStoragePool(
			ctx, &pb.CreateStoragePoolRequest{
				ClusterName: clusterName,
				SpName:      fx.spName,
				CntlrCnt:    1,
				SliceCnt:    1,
				InitExtCnt:  1,
			}); err != nil {
			t.Fatalf("CreateStoragePool: %v", err)
		}
		fx.conf = &pb.SpConf{}
		agentpathMustGet(t, cli, model.SpConfKey(cid, fx.spName), fx.conf)
		agentpathPut(t, cli,
			model.SpRevKey(fx.conf.GetShardCode(), cid, fx.conf.GetSpId()),
			&pb.SpRev{SpName: fx.spName, Revision: storedRev})
		return fx
	}

	t.Run("cntlr", func(t *testing.T) {
		fx := newSpFixture(t)
		cntlrId := fx.conf.GetCntlrIdList()[0]
		cntlrObj := &pb.Cntlr{}
		agentpathMustGet(t, fx.cli,
			model.CntlrKey(fx.cid, fx.conf.GetSpId(), cntlrId), cntlrObj)
		fake := fx.fakes[cntlrObj.GetAddrPort()]

		reply, err := fx.srv.InspectCntlr(
			context.Background(), &pb.InspectCntlrRequest{
				ClusterName: fx.cluster,
				SpName:      fx.spName,
				CntlrId:     cntlrId,
			})
		if err != nil {
			t.Fatalf("InspectCntlr: %v", err)
		}
		if reply.GetAppliedRevision() != agentRev {
			t.Errorf("applied_revision = %d, want the agent's %d (stored %d)",
				reply.GetAppliedRevision(), agentRev, storedRev)
		}
		if !proto.Equal(reply.GetCntlrInfo(), wantCntlrInfo) {
			t.Errorf("cntlr_info = %v, want the agent's %v",
				reply.GetCntlrInfo(), wantCntlrInfo)
		}
		fake.mu.Lock()
		infoReqs := append(
			[]*pb.GetCntlrInfoRequest(nil), fake.cntlrInfoReqs...)
		fake.mu.Unlock()
		if len(infoReqs) != 1 {
			t.Fatalf("GetCntlrInfo calls = %d, want 1", len(infoReqs))
		}
		if infoReqs[0].GetClusterId() != fx.cid ||
			infoReqs[0].GetCnId() != fx.cnId ||
			infoReqs[0].GetCntlrPointer().GetSpId() != fx.conf.GetSpId() ||
			infoReqs[0].GetCntlrPointer().GetCntlrId() != cntlrId {
			t.Errorf(
				"GetCntlrInfo asked for (%#x, %d, {%d, %d}), "+
					"want (%#x, %d, {%d, %d})",
				infoReqs[0].GetClusterId(), infoReqs[0].GetCnId(),
				infoReqs[0].GetCntlrPointer().GetSpId(),
				infoReqs[0].GetCntlrPointer().GetCntlrId(),
				fx.cid, fx.cnId, fx.conf.GetSpId(), cntlrId)
		}
	})

	t.Run("side", func(t *testing.T) {
		fx := newSpFixture(t)
		slice := &pb.Slice{}
		agentpathMustGet(t, fx.cli, model.SliceKey(
			fx.cid, fx.conf.GetSpId(), fx.conf.GetSliceIdList()[0]), slice)
		leg := slice.GetDataGrpList()[0].GetLegList()[0]
		side := leg.GetSideList()[0]
		fake := fx.fakes[side.GetAddrPort()]
		dnId := fx.dnIds[side.GetAddrPort()]

		reply, err := fx.srv.InspectSide(
			context.Background(), &pb.InspectSideRequest{
				ClusterName: fx.cluster,
				SpName:      fx.spName,
				SideId:      side.GetSideId(),
			})
		if err != nil {
			t.Fatalf("InspectSide: %v", err)
		}
		if reply.GetAppliedRevision() != agentRev {
			t.Errorf("applied_revision = %d, want the agent's %d (stored %d)",
				reply.GetAppliedRevision(), agentRev, storedRev)
		}
		if !proto.Equal(reply.GetSideInfo(), wantSideInfo) {
			t.Errorf("side_info = %v, want the agent's %v",
				reply.GetSideInfo(), wantSideInfo)
		}
		fake.mu.Lock()
		infoReqs := append(
			[]*pb.GetSideInfoRequest(nil), fake.sideInfoReqs...)
		fake.mu.Unlock()
		if len(infoReqs) != 1 {
			t.Fatalf("GetSideInfo calls = %d, want 1", len(infoReqs))
		}
		wantPointer := &pb.SidePointer{
			SpId:   fx.conf.GetSpId(),
			LegId:  leg.GetLegId(),
			SideId: side.GetSideId(),
		}
		if infoReqs[0].GetClusterId() != fx.cid ||
			infoReqs[0].GetDnId() != dnId ||
			!proto.Equal(infoReqs[0].GetSidePointer(), wantPointer) {
			t.Errorf("GetSideInfo asked for (%#x, %d, %v), want (%#x, %d, %v)",
				infoReqs[0].GetClusterId(), infoReqs[0].GetDnId(),
				infoReqs[0].GetSidePointer(), fx.cid, dnId, wantPointer)
		}
	})
}

// TestAgentPathForwardsTraceIdToAgent pins T3 / AG2: the mandatory client
// interceptor chain puts the request's trace id into the outbound metadata,
// so an agent log record can be tied to the gateway request that caused it.
//
// The assertion is on the raw metadata key rather than on a log line: the
// wire format is the contract between the two processes, and the integration
// suite greps agent logs for exactly what arrives here.
func TestAgentPathForwardsTraceIdToAgent(t *testing.T) {
	srv, _ := agentpathServer(t)
	fake := agentpathStartAgent(t)
	extSize := uint64(common.DefaultDnExtSize)
	fake.setSizes(extSize, extSize)
	clusterName, _ := agentpathCreateCluster(t, srv)

	ctx, traceId := agentpathTraceCtx()
	if _, err := srv.CreateDiskNode(ctx, &pb.CreateDiskNodeRequest{
		ClusterName: clusterName,
		AddrPort:    fake.addrPort,
		NvmeTrConf:  agentpathTrConf(fake.addrPort),
	}); err != nil {
		t.Fatalf("CreateDiskNode: %v", err)
	}
	if _, err := srv.CreateControllerNode(
		ctx, &pb.CreateControllerNodeRequest{
			ClusterName: clusterName,
			AddrPort:    fake.addrPort,
			NvmeTrConf:  agentpathTrConf(fake.addrPort),
		}); err != nil {
		t.Fatalf("CreateControllerNode: %v", err)
	}

	seen := fake.seenTraceIds()
	if len(seen) != 2 {
		t.Fatalf("%s values seen = %v, want two (one per agent call)",
			common.TraceIdMetadataKey, seen)
	}
	for _, got := range seen {
		if got != traceId {
			t.Errorf("%s = %q, want %q",
				common.TraceIdMetadataKey, got, traceId)
		}
	}
}

// TestAgentPathHangingAgentAbortsWithinBudget pins AG2 and AG3's timeout arm:
// withAgentConn bounds dial+call by common.DefaultGatewayAgentTimeout, so a
// hung agent bounds an RPC exactly as a hung etcd does, and the expiry is
// ABORTED like every other transport failure of a pre-STM probe.
//
// The DN and CN probes run concurrently because both must wait out the whole
// budget and the budget is the point; serializing them would double the
// test's runtime and prove nothing more.
func TestAgentPathHangingAgentAbortsWithinBudget(t *testing.T) {
	srv, cli := agentpathServer(t)
	fake := agentpathStartAgent(t)
	fake.setHang(true)
	clusterName, cid := agentpathCreateCluster(t, srv)
	budget := common.DefaultGatewayAgentTimeout * time.Second

	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = srv.CreateDiskNode(
			context.Background(), &pb.CreateDiskNodeRequest{
				ClusterName: clusterName,
				AddrPort:    fake.addrPort,
				NvmeTrConf:  agentpathTrConf(fake.addrPort),
			})
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = srv.CreateControllerNode(
			context.Background(), &pb.CreateControllerNodeRequest{
				ClusterName: clusterName,
				AddrPort:    fake.addrPort,
				NvmeTrConf:  agentpathTrConf(fake.addrPort),
			})
	}()
	start := time.Now()
	wg.Wait()
	elapsed := time.Since(start)

	for idx, err := range errs {
		if got := agentpathCode(err); got != codes.Aborted {
			t.Errorf("call %d code = %v (%v), want %v",
				idx, got, err, codes.Aborted)
		}
	}
	// The lower bound is what proves the budget did the bounding: a probe
	// that failed instantly would satisfy the upper bound alone and would
	// mean the fake never hung.
	if elapsed < budget-time.Second {
		t.Errorf("returned after %v, before the %v budget could expire",
			elapsed, budget)
	}
	if elapsed > budget+5*time.Second {
		t.Errorf("returned after %v, past the %v budget", elapsed, budget)
	}
	// House rule 8: a probe that never answered leaves no node registered.
	if agentpathGet(
		t, cli, model.DnConfKey(cid, fake.addrPort), &pb.DnConf{}) {
		t.Errorf("dn_conf was written although the size probe timed out")
	}
	if agentpathGet(
		t, cli, model.CnConfKey(cid, fake.addrPort), &pb.CnConf{}) {
		t.Errorf("cn_conf was written although the size probe timed out")
	}
}

// TestAgentPathForceFalseRefusesUnreachableAgent pins AG3's one exception:
// for DeleteClone and FinishMigration with force == false an unreachable
// agent is FAILED_PRECONDITION, not the usual ABORTED, because the caller
// cannot PROVE hydration finished and silence is not proof (§8.9, §8.11).
//
// Both subtests seed etcd by hand rather than building an SP through the
// creating handlers: the subject is the between-the-phases call and the code
// it maps to, and a hand-written phase-1 read set is the smallest state that
// reaches it. Each also asserts the deciding STM never ran — the record is
// still there and SpRev never bumped (house rule 8).
func TestAgentPathForceFalseRefusesUnreachableAgent(t *testing.T) {
	const spRev = uint64(5)

	t.Run("DeleteClone", func(t *testing.T) {
		srv, cli := agentpathServer(t)
		clusterName, cid := agentpathSeedCluster(t, cli)
		dead := agentpathDeadAddr(t)
		const (
			spName    = "agentpath-clone-sp"
			cloneName = "agentpath-clone"
			spId      = uint64(0x300)
			shard     = uint32(9)
			cntlrId   = uint64(0x401)
			cloneId   = uint64(0x601)
		)
		agentpathPut(t, cli, model.SpConfKey(cid, spName), &pb.SpConf{
			SpId:          spId,
			ShardCode:     shard,
			CntlrIdList:   []uint64{cntlrId},
			CloneNameList: []string{cloneName},
		})
		agentpathPut(t, cli, model.SpRevKey(shard, cid, spId),
			&pb.SpRev{SpName: spName, Revision: spRev})
		agentpathPut(t, cli, model.CntlrKey(cid, spId, cntlrId), &pb.Cntlr{
			AddrPort:   dead,
			NvmeTrConf: agentpathTrConf(dead),
			Primary:    true,
		})
		agentpathPut(t, cli, model.CnConfKey(cid, dead), &pb.CnConf{
			CnId:       0x501,
			ShardCode:  3,
			NvmeTrConf: agentpathTrConf(dead),
		})
		agentpathPut(t, cli, model.CloneKey(cid, spId, cloneName),
			&pb.Clone{CloneId: cloneId, DstTdId: 0x701})

		_, err := srv.DeleteClone(
			context.Background(), &pb.DeleteCloneRequest{
				ClusterName: clusterName,
				SpName:      spName,
				SpRev:       &pb.SpRev{Revision: spRev},
				CloneName:   cloneName,
			})
		if got := agentpathCode(err); got != codes.FailedPrecondition {
			t.Fatalf("DeleteClone code = %v (%v), want %v",
				got, err, codes.FailedPrecondition)
		}
		if !agentpathGet(
			t, cli, model.CloneKey(cid, spId, cloneName), &pb.Clone{}) {
			t.Errorf("the clone was deleted by a refused DeleteClone")
		}
		rev := &pb.SpRev{}
		agentpathMustGet(t, cli, model.SpRevKey(shard, cid, spId), rev)
		if rev.GetRevision() != spRev {
			t.Errorf("sp_rev revision = %d, want the untouched %d",
				rev.GetRevision(), spRev)
		}
	})

	t.Run("FinishMigration", func(t *testing.T) {
		srv, cli := agentpathServer(t)
		clusterName, cid := agentpathSeedCluster(t, cli)
		dead := agentpathDeadAddr(t)
		const (
			spName    = "agentpath-migr-sp"
			migrName  = "agentpath-migr"
			spId      = uint64(0x310)
			shard     = uint32(11)
			sliceId   = uint64(0x701)
			grpId     = uint64(0x801)
			legId     = uint64(0x901)
			srcSideId = uint64(0xa01)
			dstSideId = uint64(0xa02)
			migrId    = uint64(0xb01)
		)
		agentpathPut(t, cli, model.SpConfKey(cid, spName), &pb.SpConf{
			SpId:         spId,
			ShardCode:    shard,
			SliceIdList:  []uint64{sliceId},
			MigrNameList: []string{migrName},
		})
		agentpathPut(t, cli, model.SpRevKey(shard, cid, spId),
			&pb.SpRev{SpName: spName, Revision: spRev})
		// One leg carrying both sides of the migration: that is the shape
		// §8.11 finishes, and the destination's addr_port is the DN the
		// hydration question is put to.
		agentpathPut(t, cli, model.SliceKey(cid, spId, sliceId), &pb.Slice{
			DataGrpList: []*pb.Group{{
				GrpId:  grpId,
				ExtCnt: 4,
				LegList: []*pb.Leg{{
					LegId: legId,
					SideList: []*pb.Side{
						{
							SideId:      srcSideId,
							AddrPort:    "agentpath-src:9520",
							NvmeTrConf:  agentpathTrConf("agentpath-src:9520"),
							Provisioned: true,
						},
						{
							SideId:     dstSideId,
							AddrPort:   dead,
							NvmeTrConf: agentpathTrConf(dead),
						},
					},
				}},
			}},
		})
		agentpathPut(t, cli, model.MigrationKey(cid, spId, migrName),
			&pb.Migration{
				MigrId:    migrId,
				SrcSideId: srcSideId,
				DstSideId: dstSideId,
			})
		agentpathPut(t, cli, model.DnConfKey(cid, dead), &pb.DnConf{
			DnId:       0xc01,
			ShardCode:  2,
			NvmeTrConf: agentpathTrConf(dead),
		})

		_, err := srv.FinishMigration(
			context.Background(), &pb.FinishMigrationRequest{
				ClusterName: clusterName,
				SpName:      spName,
				SpRev:       &pb.SpRev{Revision: spRev},
				MigrName:    migrName,
			})
		if got := agentpathCode(err); got != codes.FailedPrecondition {
			t.Fatalf("FinishMigration code = %v (%v), want %v",
				got, err, codes.FailedPrecondition)
		}
		if !agentpathGet(t, cli,
			model.MigrationKey(cid, spId, migrName), &pb.Migration{}) {
			t.Errorf("the migration was removed by a refused FinishMigration")
		}
		rev := &pb.SpRev{}
		agentpathMustGet(t, cli, model.SpRevKey(shard, cid, spId), rev)
		if rev.GetRevision() != spRev {
			t.Errorf("sp_rev revision = %d, want the untouched %d",
				rev.GetRevision(), spRev)
		}
	})
}

// ---------------------------------------------------------------------------
// §9.5 — the served surface
// ---------------------------------------------------------------------------

// servingCapture records what the innermost server interceptor sees. It is
// chained AFTER the production chain of serverOptions(), so a trace id here
// can only have been put into the ctx by common.GrpcUnaryServerInterceptor
// (T2) — which is how this file proves the chain is wired without asserting
// on log output.
type servingCapture struct {
	mu       sync.Mutex
	traceIds []string
	methods  []string
}

func (c *servingCapture) interceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		traceId, _ := common.TraceIdFromCtx(ctx)
		c.mu.Lock()
		c.traceIds = append(c.traceIds, traceId)
		c.methods = append(c.methods, info.FullMethod)
		c.mu.Unlock()
		return handler(ctx, req)
	}
}

func (c *servingCapture) seen() ([]string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.traceIds...),
		append([]string(nil), c.methods...)
}

// servingBufconnServe puts srv behind a bufconn listener built from exactly
// the production option set — serverOptions(), so U1's trace-id mint and the
// grpc.md §4 chains under test are the ones Run installs, in the order Run
// installs them — plus any extra option the caller chains behind them, and
// returns the dialer that reaches it. bufconn is deliberate: §9.5 is about
// the interceptors and the status codes, not about sockets, and an in-memory
// pipe removes every port from the test.
//
// The dial is the caller's rather than this helper's because the two callers
// need opposite clients: §9.5's client carries the mandatory client chains,
// U1's carries none at all (traceid_test.go).
func servingBufconnServe(
	t *testing.T,
	srv *Server,
	extra ...grpc.ServerOption,
) func(context.Context, string) (net.Conn, error) {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer(append(serverOptions(), extra...)...)
	pb.RegisterGatewayServer(server, srv)
	go func() {
		_ = server.Serve(lis)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = lis.Close()
	})
	return func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}
}

// servingBufconn serves one gateway over bufconn with the chains Run
// installs, and returns a client dialed with the matching client chains.
func servingBufconn(
	t *testing.T,
	srv *Server,
) (pb.GatewayClient, *servingCapture) {
	t.Helper()
	capture := &servingCapture{}
	dialer := servingBufconnServe(
		t, srv, grpc.ChainUnaryInterceptor(capture.interceptor()))
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(common.GrpcUnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(common.GrpcStreamClientInterceptor()),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// Cleanups run last-in-first-out, so the conn closes before
	// servingBufconnServe's Stop tears the server down under it.
	t.Cleanup(func() {
		_ = conn.Close()
	})
	return pb.NewGatewayClient(conn), capture
}

// TestServingBufconnPropagatesTraceId pins GW3 / grpc.md T1+T2 across the
// served surface: the client chain attaches the caller's trace id and the
// server chain lifts it back into the handler's ctx, so every etcd record and
// every outbound agent call a handler makes carries the caller's id.
func TestServingBufconnPropagatesTraceId(t *testing.T) {
	srv, _ := agentpathServer(t)
	client, capture := servingBufconn(t, srv)
	ctx, traceId := agentpathTraceCtx()

	if _, err := client.GetCluster(ctx, &pb.GetClusterRequest{
		ClusterName: agentpathName("cluster"),
	}); agentpathCode(err) != codes.NotFound {
		t.Fatalf("GetCluster err = %v, want NOT_FOUND", err)
	}

	traceIds, methods := capture.seen()
	if len(traceIds) != 1 {
		t.Fatalf("handler invocations = %d, want 1", len(traceIds))
	}
	if traceIds[0] != traceId {
		t.Errorf("handler ctx trace id = %q, want %q", traceIds[0], traceId)
	}
	// schema.proto declares no proto package, so the full method carries the
	// bare service name.
	if methods[0] != "/Gateway/GetCluster" {
		t.Errorf("full method = %q, want /Gateway/GetCluster", methods[0])
	}
}

// TestServingBufconnStatusCodesCrossTheWire pins GW7 end to end: the codes a
// handler builds with the common.go constructors are the codes a client
// reads, unmodified by the interceptor chain. A chain that swallowed or
// re-wrapped an error would show up here as codes.Unknown.
func TestServingBufconnStatusCodesCrossTheWire(t *testing.T) {
	srv, cli := agentpathServer(t)
	client, _ := servingBufconn(t, srv)
	ctx := context.Background()

	// One live cluster, one live DN, for the rows that need something to
	// collide with, to read back or a token to get wrong. The cluster goes
	// through the real handler because the OK row reads the three globals
	// CreateCluster writes alongside the ClusterConf (§8.1).
	liveCluster, cid := agentpathCreateCluster(t, srv)
	const dnAddr = "agentpath-wire-dn:9520"
	agentpathPut(t, cli, model.DnConfKey(cid, dnAddr), &pb.DnConf{
		DnId:        1,
		ShardCode:   3,
		NvmeTrConf:  agentpathTrConf(dnAddr),
		Location:    dnAddr,
		TotalExtCnt: 4,
		FreeExtCnt:  4,
	})
	agentpathPut(t, cli, model.DnRevKey(3, cid, 1),
		&pb.DnRev{AddrPort: dnAddr, Revision: 1})

	tests := []struct {
		name string
		call func() error
		want codes.Code
	}{
		{
			name: "a served read is OK",
			call: func() error {
				_, err := client.GetCluster(ctx, &pb.GetClusterRequest{
					ClusterName: liveCluster,
				})
				return err
			},
			want: codes.OK,
		},
		{
			name: "a §7 violation is INVALID_ARGUMENT",
			call: func() error {
				_, err := client.CreateCluster(ctx, &pb.CreateClusterRequest{
					ClusterName: "not a legal name!",
				})
				return err
			},
			want: codes.InvalidArgument,
		},
		{
			name: "an absent cluster is NOT_FOUND",
			call: func() error {
				_, err := client.GetCluster(ctx, &pb.GetClusterRequest{
					ClusterName: agentpathName("cluster"),
				})
				return err
			},
			want: codes.NotFound,
		},
		{
			name: "a name collision is ALREADY_EXISTS",
			call: func() error {
				_, err := client.CreateCluster(ctx, &pb.CreateClusterRequest{
					ClusterName: liveCluster,
				})
				return err
			},
			want: codes.AlreadyExists,
		},
		{
			name: "a stale token is ABORTED",
			call: func() error {
				_, err := client.UpdateDiskNodeDisabled(
					ctx, &pb.UpdateDiskNodeDisabledRequest{
						ClusterName: liveCluster,
						AddrPort:    dnAddr,
						DnRev:       &pb.DnRev{Revision: 99},
						Disabled:    true,
					})
				return err
			},
			want: codes.Aborted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if got := agentpathCode(err); got != tt.want {
				t.Fatalf("code = %v (%v), want %v", got, err, tt.want)
			}
		})
	}
}

// servingOwnClient builds an etcd client the caller — here, Run — is expected
// to close. It registers no cleanup Close on purpose: Run closes the client as
// the last step of its drain (GW2/CM3), and a second Close would report an
// error the test would then have to ignore.
func servingOwnClient(t *testing.T) *etcdutil.Client {
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
	return cli
}

// TestServingGracefulStopOnCtxCancel pins GW2/GW3's whole lifecycle: Run
// serves until ctx is canceled and then GracefulStop lets Serve return
// nil — in-flight handlers finish, and there is nothing else to drain (§0 #3).
//
// This one test uses a real listener rather than bufconn, because
// GracefulStop lives inside Run and Run opens its own listener: testing the
// pattern over bufconn would mean re-implementing Run in the test and
// asserting on the copy. The port is learned and released first, which leaves
// a window another process could take — the only alternative is a flag Run
// does not have.
func TestServingGracefulStopOnCtxCancel(t *testing.T) {
	cli := servingOwnClient(t)
	addr := agentpathDeadAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, cli, Config{
			GrpcNetwork: "tcp",
			GrpcAddress: addr,
			Endpoints:   []string{testEndpoint},
		})
	}()

	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(common.GrpcUnaryClientInterceptor()),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer conn.Close()
	// WaitForReady is what removes the sleep: the call parks until the
	// listener Run opened accepts it, so a NOT_FOUND here means "Run is
	// serving", not "Run got as far as net.Listen".
	dialCtx, dialCancel := context.WithTimeout(
		context.Background(), 30*time.Second)
	defer dialCancel()
	_, err = pb.NewGatewayClient(conn).GetCluster(
		dialCtx,
		&pb.GetClusterRequest{ClusterName: agentpathName("cluster")},
		grpc.WaitForReady(true),
	)
	if got := agentpathCode(err); got != codes.NotFound {
		t.Fatalf("GetCluster before the cancel = %v (%v), want NOT_FOUND",
			got, err)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run after ctx cancel = %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("Run did not return within 30s of the ctx cancel")
	}
}

// ---------------------------------------------------------------------------
// §9.6 — one Server, two goroutines
// ---------------------------------------------------------------------------

// raceOutcome is what one racing goroutine came back with.
type raceOutcome struct {
	err   error
	dnId  uint64
	clsId uint64
}

// raceBoth runs two calls concurrently through one Server and returns their
// outcomes in start order.
func raceBoth(
	t *testing.T,
	first func() raceOutcome,
	second func() raceOutcome,
) [2]raceOutcome {
	t.Helper()
	var out [2]raceOutcome
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		out[0] = first()
	}()
	go func() {
		defer wg.Done()
		out[1] = second()
	}()
	wg.Wait()
	return out
}

// raceWinner asserts exactly one of the two calls succeeded and that the
// other lost with a code §5.9 allows a loser to lose with, then returns the
// winner. ALREADY_EXISTS is what the loser normally sees — RunSTM re-runs its
// closure on a write conflict, so the second attempt reads the winner's key —
// and ABORTED is the legal alternative if the retry budget ran out first.
func raceWinner(t *testing.T, out [2]raceOutcome) raceOutcome {
	t.Helper()
	winners := 0
	var winner raceOutcome
	for idx, one := range out {
		if one.err == nil {
			winners++
			winner = one
			continue
		}
		switch agentpathCode(one.err) {
		case codes.AlreadyExists, codes.Aborted:
		default:
			t.Fatalf("loser %d failed with %v (%v), want ALREADY_EXISTS or "+
				"ABORTED", idx, agentpathCode(one.err), one.err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1 (%v / %v)",
			winners, out[0].err, out[1].err)
	}
	return winner
}

// TestRaceSameNameCreatesHaveOneWinner pins §0 #3 and GW8 under `-race`: the
// Server holds no state and takes no lock, so two concurrent same-name
// creates are ordered by etcd alone. Exactly one must commit, the loser must
// write nothing, and the store must be left in the state one create leaves —
// no double-minted id, no orphan bucket increment.
//
// Two goroutines through ONE Server is the in-process form of two gateway
// instances: correctness rests entirely on RunSTM's serializable-snapshot
// isolation, which does not know or care whether the two callers share a
// process.
func TestRaceSameNameCreatesHaveOneWinner(t *testing.T) {
	t.Run("CreateCluster", func(t *testing.T) {
		srv, cli := agentpathServer(t)
		name := agentpathName("cluster")
		create := func() raceOutcome {
			reply, err := srv.CreateCluster(
				context.Background(),
				&pb.CreateClusterRequest{ClusterName: name})
			return raceOutcome{err: err, clsId: reply.GetClusterId()}
		}
		winner := raceWinner(t, raceBoth(t, create, create))

		// Each attempt stamps its own creation_epoch, so the stored conf
		// names the winning cluster_id and nothing else may claim it (§5.2).
		conf := &pb.ClusterConf{}
		agentpathMustGet(t, cli, model.ClusterConfKey(name), conf)
		cid := model.ClusterId(name, conf.GetCreationEpoch())
		if winner.clsId != cid {
			t.Errorf("winning cluster_id = %#x, stored conf implies %#x",
				winner.clsId, cid)
		}
		dnGlobal := &pb.DnGlobal{}
		agentpathMustGet(t, cli, model.DnGlobalKey(cid), dnGlobal)
		cnGlobal := &pb.CnGlobal{}
		agentpathMustGet(t, cli, model.CnGlobalKey(cid), cnGlobal)
		spGlobal := &pb.SpGlobal{}
		agentpathMustGet(t, cli, model.SpGlobalKey(cid), spGlobal)
		globals := []struct {
			name   string
			nextId uint64
			bucket []uint32
		}{
			{"dn_global", dnGlobal.GetNextId(), dnGlobal.GetShardBucket()},
			{"cn_global", cnGlobal.GetNextId(), cnGlobal.GetShardBucket()},
			{"sp_global", spGlobal.GetNextId(), spGlobal.GetShardBucket()},
		}
		for _, global := range globals {
			if global.nextId != 1 {
				t.Errorf("%s next_id = %d, want 1",
					global.name, global.nextId)
			}
			if len(global.bucket) != common.ShardBucketSize {
				t.Errorf("%s shard_bucket length = %d, want %d",
					global.name, len(global.bucket), common.ShardBucketSize)
			}
			if got := agentpathBucketSum(global.bucket); got != 0 {
				t.Errorf("sum(%s shard_bucket) = %d, want 0",
					global.name, got)
			}
		}
	})

	t.Run("CreateDiskNode", func(t *testing.T) {
		srv, cli := agentpathServer(t)
		fake := agentpathStartAgent(t)
		fake.setSizes(4*uint64(common.DefaultDnExtSize), 0)
		clusterName, cid := agentpathCreateCluster(t, srv)
		create := func() raceOutcome {
			reply, err := srv.CreateDiskNode(
				context.Background(), &pb.CreateDiskNodeRequest{
					ClusterName: clusterName,
					AddrPort:    fake.addrPort,
					NvmeTrConf:  agentpathTrConf(fake.addrPort),
				})
			return raceOutcome{err: err, dnId: reply.GetDnId()}
		}
		winner := raceWinner(t, raceBoth(t, create, create))

		dn := &pb.DnConf{}
		agentpathMustGet(t, cli, model.DnConfKey(cid, fake.addrPort), dn)
		if dn.GetDnId() != winner.dnId {
			t.Errorf("dn_conf dn_id = %d, the winner was told %d",
				dn.GetDnId(), winner.dnId)
		}
		if dn.GetTotalExtCnt() != 4 || dn.GetFreeExtCnt() != 4 {
			t.Errorf("dn_conf ext counts = (%d, %d), want (4, 4)",
				dn.GetTotalExtCnt(), dn.GetFreeExtCnt())
		}
		// One create, one mint: next_id advanced by exactly one and exactly
		// one bucket slot carries the node (§5.4, GW12).
		global := &pb.DnGlobal{}
		agentpathMustGet(t, cli, model.DnGlobalKey(cid), global)
		if global.GetNextId() != 2 {
			t.Errorf("dn_global next_id = %d, want 2", global.GetNextId())
		}
		if got := agentpathBucketSum(global.GetShardBucket()); got != 1 {
			t.Errorf("sum(dn_global shard_bucket) = %d, want 1", got)
		}
		bucket := global.GetShardBucket()
		if int(dn.GetShardCode()) >= len(bucket) ||
			bucket[dn.GetShardCode()] != 1 {
			t.Errorf("shard_bucket[%d] does not hold the node: %v",
				dn.GetShardCode(), bucket[:8])
		}
		rev := &pb.DnRev{}
		agentpathMustGet(t, cli,
			model.DnRevKey(dn.GetShardCode(), cid, dn.GetDnId()), rev)
		if rev.GetAddrPort() != fake.addrPort || rev.GetRevision() != 1 {
			t.Errorf("dn_rev = (%q, %d), want (%q, 1)",
				rev.GetAddrPort(), rev.GetRevision(), fake.addrPort)
		}
	})
}
