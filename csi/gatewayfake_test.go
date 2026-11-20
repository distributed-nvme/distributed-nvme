package csi

import (
	"context"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// fakeGateway is the gateway stand-in of the controller plugin's tests
// (csi.md, Integration test plan): the calls the plugin makes, answered with
// the gateway's codes from clusters, pools, thin devices, subsystems and
// cntlr lists kept in maps, from each cluster's planted space answer for the
// capacity read, and from an ok report of the primary, or a test's own, for
// the detach's wait. It is not the dnvctl suite's fake gateway, which
// answers one canned reply per method and keeps no state.
type fakeGateway struct {
	pb.UnimplementedGatewayServer
	mu       sync.Mutex
	clusters map[string]*fakeCluster
	// fail makes the named method answer the code instead of acting.
	fail map[string]codes.Code
	// calls is every method called, in order.
	calls []string
	// alwaysStale refuses every host write as stale and moves the pool's
	// revision, as a pool that moves under every attempt would.
	alwaysStale bool
	// afterList runs once, outside the lock, after a ListSubsystems has
	// read the hosts: a test's other attach or detach lands between that
	// read and the write that follows it.
	afterList func()
	// capReqs is every GetClusterCapacity request, in order.
	capReqs []*pb.GetClusterCapacityRequest
	// poolRead, when set, runs under the lock at every GetStoragePool of a
	// pool that exists, before the reply is made: a test changes the pool
	// it is handed there, its primary or its revision, between two reads.
	poolRead func(p *fakePool)
	// inspect, when set, answers every InspectCntlr of a cntlr the pool
	// lists, under the lock, in the fake's place: a test steps the
	// primary's report with it (an older revision, a row that is not OK, no
	// rows, a refusal) and may change the pool it is handed. Unset, the
	// report is ok: every subsystem's row OK at the pool's revision.
	inspect func(p *fakePool, req *pb.InspectCntlrRequest) (*pb.InspectCntlrReply, error)
	// inspected is the cntlr id of every InspectCntlr, in order.
	inspected []uint64
	// hostWrite, when set, runs under the lock at every
	// UpdateSubsystemHosts of a pool that exists, before the token check:
	// a test reads the token each write carries.
	hostWrite func(p *fakePool, req *pb.UpdateSubsystemHostsRequest)
}

type fakeCluster struct {
	conf  *pb.ClusterConf
	pools map[string]*fakePool
	// space is the space answer GetClusterCapacity gives, whatever shape
	// it is asked for; nil answers none, as a gateway older than the
	// driver would.
	space *pb.ClusterSpace
}

type fakePool struct {
	req      *pb.CreateStoragePoolRequest
	deleting bool
	// rev is the pool's revision, which a token must match (gateway.md
	// GW6). The fake moves it on a host write only, and a test's hook may
	// move it as another change of the pool would; the gateway moves it on
	// every change of what the agents build.
	rev    uint64
	tds    map[string]*pb.ThinDevice
	nextTd uint64
	ss     map[string]*pb.Subsystem
	nextSs uint64
	// cntlrIds is the pool's cntlr id list, and primary the index of its
	// primary in it, -1 for none: GetStoragePool answers the cntlr list in
	// the id list's order with the primary flag on that index alone, as the
	// gateway does.
	cntlrIds []uint64
	primary  int
}

// The fixture's geometry: 64 MiB extents, 64 KiB stripes, 1 MiB blocks.
const (
	fakeExtent     = uint64(64) << 20
	fakeStripe     = uint64(64) << 10
	fakeBlock      = uint64(1) << 20
	fakeClusterOne = "c0"
	// fakeCntlrIdBase is the id before a pool's first cntlr: the ids stay
	// apart from the slice ids, which count from one.
	fakeCntlrIdBase = uint64(100)
)

func newFakeGateway() *fakeGateway {
	g := &fakeGateway{
		clusters: map[string]*fakeCluster{},
		fail:     map[string]codes.Code{},
	}
	g.addCluster(fakeClusterOne)
	return g
}

// addCluster plants a cluster with the fixture's geometry and the space
// answer the gateway gives a RAID1 pool of two cntlrs and one slice over four
// disk nodes of 1000 free extents in four locations and three controller
// nodes of 900: the budgets limit it.
func (g *fakeGateway) addCluster(name string) *fakeCluster {
	c := &fakeCluster{
		conf: &pb.ClusterConf{
			DnBinConf: &pb.DnBinConf{ExtentSize: fakeExtent},
			BdevConf: &pb.BdevConf{
				DmPoolConf:  &pb.DmPoolConf{DataBlockSize: fakeBlock},
				DmRaid0Conf: &pb.DmRaid0Conf{StripeSize: fakeStripe},
			},
		},
		pools: map[string]*fakePool{},
		space: &pb.ClusterSpace{
			LegCnt:            2,
			CntlrCnt:          2,
			SliceCnt:          1,
			PlaceableBytes:    1350 * fakeExtent,
			LargestInitExtCnt: 899,
			Limit:             pb.CapacityLimit_CAPACITY_LIMIT_CN_BUDGET,
		},
	}
	g.clusters[name] = c
	return c
}

// serve starts the fake on an in-memory connection and returns a client of
// it. The server has no interceptor, so the tests' logs stay quiet.
func (g *fakeGateway) serve(t *testing.T) pb.GatewayClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterGatewayServer(srv, g)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///fakegateway",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, s string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
	)
	if err != nil {
		t.Fatalf("dialing the fake gateway: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
	})
	return pb.NewGatewayClient(conn)
}

// serveTcp starts the fake on a loopback TCP port, for the tests that run
// the whole plugin, and returns the address.
func (g *fakeGateway) serveTcp(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for the fake gateway: %v", err)
	}
	srv := grpc.NewServer()
	pb.RegisterGatewayServer(srv, g)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// pool is the planted pool of a name, or nil.
func (g *fakeGateway) pool(cluster string, name string) *fakePool {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.clusters[clusterNameOf(cluster)]
	if c == nil {
		return nil
	}
	return c.pools[name]
}

// drain removes a deleting pool, as the worker's drain would.
func (g *fakeGateway) drain(cluster string, name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c := g.clusters[clusterNameOf(cluster)]; c != nil {
		delete(c.pools, name)
	}
}

// methodCalls counts the calls of one method.
func (g *fakeGateway) methodCalls(method string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, m := range g.calls {
		if m == method {
			n++
		}
	}
	return n
}

// enter records the call and answers an injected failure.
func (g *fakeGateway) enter(method string) error {
	g.calls = append(g.calls, method)
	if code, ok := g.fail[method]; ok {
		return status.Errorf(code, "injected %s", method)
	}
	return nil
}

func (g *fakeGateway) cluster(name string) (*fakeCluster, error) {
	c := g.clusters[clusterNameOf(name)]
	if c == nil {
		return nil, status.Errorf(codes.NotFound, "cluster %q not found",
			clusterNameOf(name))
	}
	return c, nil
}

// livePool is a pool that exists; a deleting pool refuses a write with
// FAILED_PRECONDITION when write is set, as the gateway's gate does.
func (g *fakeGateway) livePool(
	cluster string, name string, write bool,
) (*fakePool, error) {
	c, err := g.cluster(cluster)
	if err != nil {
		return nil, err
	}
	p := c.pools[name]
	if p == nil {
		return nil, status.Errorf(codes.NotFound, "sp %q not found", name)
	}
	if write && p.deleting {
		return nil, status.Errorf(codes.FailedPrecondition,
			"sp %q is being deleted", name)
	}
	return p, nil
}

func (g *fakeGateway) GetCluster(
	ctx context.Context, req *pb.GetClusterRequest,
) (*pb.GetClusterReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("GetCluster"); err != nil {
		return nil, err
	}
	c, err := g.cluster(req.GetClusterName())
	if err != nil {
		return nil, err
	}
	return &pb.GetClusterReply{
		ClusterName: clusterNameOf(req.GetClusterName()),
		ClusterConf: proto.Clone(c.conf).(*pb.ClusterConf),
		SpGlobal: &pb.SpGlobal{
			ShardBucket: []uint32{uint32(len(c.pools))},
		},
	}, nil
}

func (g *fakeGateway) GetClusterCapacity(
	ctx context.Context, req *pb.GetClusterCapacityRequest,
) (*pb.GetClusterCapacityReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.capReqs = append(g.capReqs,
		proto.Clone(req).(*pb.GetClusterCapacityRequest))
	if err := g.enter("GetClusterCapacity"); err != nil {
		return nil, err
	}
	c, err := g.cluster(req.GetClusterName())
	if err != nil {
		return nil, err
	}
	reply := &pb.GetClusterCapacityReply{
		ClusterName: clusterNameOf(req.GetClusterName()),
		ExtentSize:  c.conf.GetDnBinConf().GetExtentSize(),
	}
	if c.space != nil {
		reply.Space = proto.Clone(c.space).(*pb.ClusterSpace)
	}
	return reply, nil
}

func (g *fakeGateway) CreateStoragePool(
	ctx context.Context, req *pb.CreateStoragePoolRequest,
) (*pb.CreateStoragePoolReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("CreateStoragePool"); err != nil {
		return nil, err
	}
	c, err := g.cluster(req.GetClusterName())
	if err != nil {
		return nil, err
	}
	if req.GetInitExtCnt() == 0 {
		return nil, status.Error(codes.InvalidArgument, "init_ext_cnt is 0")
	}
	if c.pools[req.GetSpName()] != nil {
		return nil, status.Errorf(codes.AlreadyExists, "sp %q exists",
			req.GetSpName())
	}
	cntlrIds := make([]uint64, max(req.GetCntlrCnt(), 1))
	for i := range cntlrIds {
		cntlrIds[i] = fakeCntlrIdBase + uint64(i) + 1
	}
	c.pools[req.GetSpName()] = &fakePool{
		rev:      1,
		req:      proto.Clone(req).(*pb.CreateStoragePoolRequest),
		tds:      map[string]*pb.ThinDevice{},
		nextTd:   1,
		ss:       map[string]*pb.Subsystem{},
		nextSs:   1,
		cntlrIds: cntlrIds,
		primary:  0,
	}
	return &pb.CreateStoragePoolReply{SpId: uint64(len(c.pools))}, nil
}

func (g *fakeGateway) GetStoragePool(
	ctx context.Context, req *pb.GetStoragePoolRequest,
) (*pb.GetStoragePoolReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("GetStoragePool"); err != nil {
		return nil, err
	}
	c, err := g.cluster(req.GetClusterName())
	if err != nil {
		return nil, err
	}
	p, err := g.livePool(req.GetClusterName(), req.GetSpName(), false)
	if err != nil {
		return nil, err
	}
	if g.poolRead != nil {
		g.poolRead(p)
	}
	// The pool's conf as the gateway stores it: the stripe resolved, the
	// cluster's when the create named none, one id per slice, the cntlr ids
	// and the subsystems' NQNs.
	bdev := proto.Clone(p.req.GetBdevConf()).(*pb.BdevConf)
	if bdev == nil {
		bdev = &pb.BdevConf{}
	}
	if bdev.GetDmRaid0Conf().GetStripeSize() == 0 {
		bdev.DmRaid0Conf = &pb.DmRaid0Conf{
			StripeSize: c.conf.GetBdevConf().GetDmRaid0Conf().GetStripeSize(),
		}
	}
	sliceIds := make([]uint64, p.req.GetSliceCnt())
	for i := range sliceIds {
		sliceIds[i] = uint64(i + 1)
	}
	cntlrs := make([]*pb.Cntlr, len(p.cntlrIds))
	for i := range cntlrs {
		cntlrs[i] = &pb.Cntlr{Primary: i == p.primary}
	}
	return &pb.GetStoragePoolReply{
		SpName: req.GetSpName(),
		SpConf: &pb.SpConf{
			Deleting:    p.deleting,
			BdevConf:    bdev,
			SliceIdList: sliceIds,
			CntlrIdList: slices.Clone(p.cntlrIds),
			NqnList:     slices.Sorted(maps.Keys(p.ss)),
		},
		SpRev:     &pb.SpRev{Revision: p.rev},
		CntlrList: cntlrs,
	}, nil
}

// InspectCntlr is the gateway's read of one cntlr's report: NOT_FOUND for a
// pool that does not exist or a cntlr it does not list, as after a cntlr
// replacement, and otherwise the test's report or an ok one. A deleting
// pool is read as any other.
func (g *fakeGateway) InspectCntlr(
	ctx context.Context, req *pb.InspectCntlrRequest,
) (*pb.InspectCntlrReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inspected = append(g.inspected, req.GetCntlrId())
	if err := g.enter("InspectCntlr"); err != nil {
		return nil, err
	}
	p, err := g.livePool(req.GetClusterName(), req.GetSpName(), false)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(p.cntlrIds, req.GetCntlrId()) {
		return nil, status.Errorf(codes.NotFound, "cntlr %d not found",
			req.GetCntlrId())
	}
	if g.inspect != nil {
		return g.inspect(p, req)
	}
	return okReport(p), nil
}

// okReport is the report of a primary that has converged the pool's
// revision: every subsystem's row OK.
func okReport(p *fakePool) *pb.InspectCntlrReply {
	rows := map[uint64]*pb.ResInfo{}
	for nqn, ss := range p.ss {
		rows[ss.GetSsId()] = &pb.ResInfo{
			ResName: nqn,
			Status:  pb.ResStatus_RES_STATUS_OK,
		}
	}
	return &pb.InspectCntlrReply{
		AppliedRevision: p.rev,
		CntlrInfo:       &pb.CntlrInfo{SsIdToSubsystem: rows},
	}
}

// DeleteStoragePool is the gateway's: refused while a user-created object
// remains, done without a write when the pool is already deleting.
func (g *fakeGateway) DeleteStoragePool(
	ctx context.Context, req *pb.DeleteStoragePoolRequest,
) (*pb.DeleteStoragePoolReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("DeleteStoragePool"); err != nil {
		return nil, err
	}
	p, err := g.livePool(req.GetClusterName(), req.GetSpName(), false)
	if err != nil {
		return nil, err
	}
	if p.deleting {
		return &pb.DeleteStoragePoolReply{}, nil
	}
	if len(p.tds) != 0 || len(p.ss) != 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"storage pool %q still holds %d thin devices and %d subsystems",
			req.GetSpName(), len(p.tds), len(p.ss))
	}
	p.deleting = true
	return &pb.DeleteStoragePoolReply{}, nil
}

func (g *fakeGateway) DeleteThinDevice(
	ctx context.Context, req *pb.DeleteThinDeviceRequest,
) (*pb.DeleteThinDeviceReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("DeleteThinDevice"); err != nil {
		return nil, err
	}
	p, err := g.livePool(req.GetClusterName(), req.GetSpName(), true)
	if err != nil {
		return nil, err
	}
	td := p.tds[req.GetTdName()]
	if td == nil {
		return nil, status.Errorf(codes.NotFound, "td %q not found", req.GetTdName())
	}
	for _, ss := range p.ss {
		for _, ns := range ss.GetNsList() {
			if ns.GetTdId() == td.GetTdId() {
				return nil, status.Errorf(codes.FailedPrecondition,
					"td %q is used by a namespace", req.GetTdName())
			}
		}
	}
	delete(p.tds, req.GetTdName())
	return &pb.DeleteThinDeviceReply{TdId: td.GetTdId()}, nil
}

func (g *fakeGateway) DeleteSubsystem(
	ctx context.Context, req *pb.DeleteSubsystemRequest,
) (*pb.DeleteSubsystemReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("DeleteSubsystem"); err != nil {
		return nil, err
	}
	p, err := g.livePool(req.GetClusterName(), req.GetSpName(), true)
	if err != nil {
		return nil, err
	}
	ss := p.ss[req.GetNqn()]
	if ss == nil {
		return nil, status.Errorf(codes.NotFound, "ss %q not found", req.GetNqn())
	}
	if len(ss.GetNsList()) != 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"ss %q still has namespaces", req.GetNqn())
	}
	delete(p.ss, req.GetNqn())
	return &pb.DeleteSubsystemReply{SsId: ss.GetSsId()}, nil
}

func (g *fakeGateway) DeleteNamespace(
	ctx context.Context, req *pb.DeleteNamespaceRequest,
) (*pb.DeleteNamespaceReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("DeleteNamespace"); err != nil {
		return nil, err
	}
	p, err := g.livePool(req.GetClusterName(), req.GetSpName(), true)
	if err != nil {
		return nil, err
	}
	ss := p.ss[req.GetNqn()]
	if ss == nil {
		return nil, status.Errorf(codes.NotFound, "ss %q not found", req.GetNqn())
	}
	for i, ns := range ss.GetNsList() {
		if ns.GetNsIdx() == req.GetNsIdx() {
			ss.NsList = append(ss.NsList[:i], ss.NsList[i+1:]...)
			return &pb.DeleteNamespaceReply{NsId: ns.GetNsId()}, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "ns_idx %d not found", req.GetNsIdx())
}

func (g *fakeGateway) CreateThinDevice(
	ctx context.Context, req *pb.CreateThinDeviceRequest,
) (*pb.CreateThinDeviceReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("CreateThinDevice"); err != nil {
		return nil, err
	}
	p, err := g.livePool(req.GetClusterName(), req.GetSpName(), true)
	if err != nil {
		return nil, err
	}
	if req.GetSize() == 0 || req.GetSize()%fakeStripe != 0 {
		return nil, status.Errorf(codes.InvalidArgument, "size %d",
			req.GetSize())
	}
	if p.tds[req.GetTdName()] != nil {
		return nil, status.Errorf(codes.AlreadyExists, "td %q exists",
			req.GetTdName())
	}
	td := &pb.ThinDevice{
		TdId:  p.nextTd,
		DevId: uint32(p.nextTd),
		Size:  req.GetSize(),
	}
	p.nextTd++
	p.tds[req.GetTdName()] = td
	return &pb.CreateThinDeviceReply{TdId: td.GetTdId(), DevId: td.GetDevId()}, nil
}

func (g *fakeGateway) ListThinDevices(
	ctx context.Context, req *pb.ListThinDevicesRequest,
) (*pb.ListThinDevicesReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("ListThinDevices"); err != nil {
		return nil, err
	}
	p, err := g.livePool(req.GetClusterName(), req.GetSpName(), false)
	if err != nil {
		return nil, err
	}
	reply := &pb.ListThinDevicesReply{NameToTd: map[string]*pb.ThinDevice{}}
	for name, td := range p.tds {
		reply.NameToTd[name] = proto.Clone(td).(*pb.ThinDevice)
	}
	return reply, nil
}

func (g *fakeGateway) CreateSubsystem(
	ctx context.Context, req *pb.CreateSubsystemRequest,
) (*pb.CreateSubsystemReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("CreateSubsystem"); err != nil {
		return nil, err
	}
	p, err := g.livePool(req.GetClusterName(), req.GetSpName(), true)
	if err != nil {
		return nil, err
	}
	// The gateway's NQN rule (architecture.md, Common validation): the
	// pattern, no "..", and outside the dnv-owned namespace.
	if common.IsDnvNqn(req.GetNqn()) || !validNqn.MatchString(req.GetNqn()) ||
		strings.Contains(req.GetNqn(), "..") {
		return nil, status.Errorf(codes.InvalidArgument, "nqn %q", req.GetNqn())
	}
	if p.ss[req.GetNqn()] != nil {
		return nil, status.Errorf(codes.AlreadyExists, "ss %q exists",
			req.GetNqn())
	}
	p.ss[req.GetNqn()] = &pb.Subsystem{
		SsId:         p.nextSs,
		AllowedHosts: append([]string(nil), req.GetAllowedHosts()...),
	}
	p.nextSs++
	return &pb.CreateSubsystemReply{SsId: p.ss[req.GetNqn()].GetSsId()}, nil
}

func (g *fakeGateway) ListSubsystems(
	ctx context.Context, req *pb.ListSubsystemsRequest,
) (*pb.ListSubsystemsReply, error) {
	reply, err := g.listSubsystems(ctx, req)
	g.mu.Lock()
	after := g.afterList
	g.afterList = nil
	g.mu.Unlock()
	if after != nil {
		after()
	}
	return reply, err
}

func (g *fakeGateway) listSubsystems(
	ctx context.Context, req *pb.ListSubsystemsRequest,
) (*pb.ListSubsystemsReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("ListSubsystems"); err != nil {
		return nil, err
	}
	p, err := g.livePool(req.GetClusterName(), req.GetSpName(), false)
	if err != nil {
		return nil, err
	}
	reply := &pb.ListSubsystemsReply{
		NqnToSubsystem: map[string]*pb.Subsystem{},
	}
	for nqn, ss := range p.ss {
		reply.NqnToSubsystem[nqn] = proto.Clone(ss).(*pb.Subsystem)
	}
	return reply, nil
}

func (g *fakeGateway) UpdateSubsystemHosts(
	ctx context.Context, req *pb.UpdateSubsystemHostsRequest,
) (*pb.UpdateSubsystemHostsReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("UpdateSubsystemHosts"); err != nil {
		return nil, err
	}
	p, err := g.livePool(req.GetClusterName(), req.GetSpName(), true)
	if err != nil {
		return nil, err
	}
	if g.hostWrite != nil {
		g.hostWrite(p, req)
	}
	if g.alwaysStale {
		// Another change of the pool won the race: it moved the revision.
		p.rev++
		return nil, status.Error(codes.Aborted, "stale revision")
	}
	if req.GetSpRev() != nil && req.GetSpRev().GetRevision() != p.rev {
		return nil, status.Error(codes.Aborted, "stale revision")
	}
	ss := p.ss[req.GetNqn()]
	if ss == nil {
		return nil, status.Errorf(codes.NotFound, "ss %q not found", req.GetNqn())
	}
	ss.AllowedHosts = append([]string(nil), req.GetAllowedHosts()...)
	p.rev++
	return &pb.UpdateSubsystemHostsReply{SsId: ss.GetSsId()}, nil
}

func (g *fakeGateway) CreateNamespace(
	ctx context.Context, req *pb.CreateNamespaceRequest,
) (*pb.CreateNamespaceReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("CreateNamespace"); err != nil {
		return nil, err
	}
	p, err := g.livePool(req.GetClusterName(), req.GetSpName(), true)
	if err != nil {
		return nil, err
	}
	ss := p.ss[req.GetNqn()]
	if ss == nil {
		return nil, status.Errorf(codes.NotFound, "ss %q not found", req.GetNqn())
	}
	td := p.tds[req.GetTdName()]
	if td == nil {
		return nil, status.Errorf(codes.NotFound, "td %q not found",
			req.GetTdName())
	}
	if req.GetNsIdx() == 0 {
		return nil, status.Error(codes.InvalidArgument, "ns_idx is 0")
	}
	for _, ns := range ss.GetNsList() {
		if ns.GetNsIdx() == req.GetNsIdx() {
			return nil, status.Errorf(codes.InvalidArgument,
				"ns_idx %d is used", req.GetNsIdx())
		}
	}
	ns := &pb.Namespace{
		NsId:    uint64(len(ss.GetNsList()) + 1),
		NsIdx:   req.GetNsIdx(),
		TdId:    td.GetTdId(),
		DevUuid: req.GetDevUuid(),
	}
	ss.NsList = append(ss.NsList, ns)
	return &pb.CreateNamespaceReply{NsId: ns.GetNsId()}, nil
}
