package csi

import (
	"context"
	"net"
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
// nodes kept in maps. It is not the dnvctl suite's fake gateway, which
// answers one canned reply per method and keeps no state.
type fakeGateway struct {
	pb.UnimplementedGatewayServer
	mu       sync.Mutex
	clusters map[string]*fakeCluster
	// fail makes the named method answer the code instead of acting.
	fail map[string]codes.Code
	// calls is every method called, in order.
	calls []string
}

type fakeCluster struct {
	conf  *pb.ClusterConf
	pools map[string]*fakePool
	dns   map[string]*pb.DnCapacityEntry
	cns   map[string]*pb.CnCapacityEntry
}

type fakePool struct {
	req      *pb.CreateStoragePoolRequest
	deleting bool
	tds      map[string]*pb.ThinDevice
	nextTd   uint64
	ss       map[string]*pb.Subsystem
	nextSs   uint64
}

// The fixture's geometry: 64 MiB extents, 64 KiB stripes, 1 MiB blocks,
// four disk nodes of 1024 extents with 1000 free, three controller nodes of
// 1024 with 900 free.
const (
	fakeExtent     = uint64(64) << 20
	fakeStripe     = uint64(64) << 10
	fakeBlock      = uint64(1) << 20
	fakeDnTotal    = uint64(1024)
	fakeDnFree     = uint64(1000)
	fakeCnTotal    = uint64(1024)
	fakeCnFree     = uint64(900)
	fakeClusterOne = "c0"
)

func newFakeGateway() *fakeGateway {
	g := &fakeGateway{
		clusters: map[string]*fakeCluster{},
		fail:     map[string]codes.Code{},
	}
	g.addCluster(fakeClusterOne)
	return g
}

// addCluster plants a cluster with the fixture's geometry and nodes.
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
		dns:   map[string]*pb.DnCapacityEntry{},
		cns:   map[string]*pb.CnCapacityEntry{},
	}
	for i := range 4 {
		addr := "dn" + string(rune('0'+i)) + ":9000"
		c.dns[addr] = &pb.DnCapacityEntry{
			Location: addr, TotalExtCnt: fakeDnTotal, FreeExtCnt: fakeDnFree,
		}
	}
	for i := range 3 {
		addr := "cn" + string(rune('0'+i)) + ":9000"
		c.cns[addr] = &pb.CnCapacityEntry{
			Location: addr, TotalExtCnt: fakeCnTotal, FreeExtCnt: fakeCnFree,
		}
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
	}, nil
}

func (g *fakeGateway) GetClusterCapacity(
	ctx context.Context, req *pb.GetClusterCapacityRequest,
) (*pb.GetClusterCapacityReply, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.enter("GetClusterCapacity"); err != nil {
		return nil, err
	}
	c, err := g.cluster(req.GetClusterName())
	if err != nil {
		return nil, err
	}
	reply := &pb.GetClusterCapacityReply{
		ClusterName:  clusterNameOf(req.GetClusterName()),
		ExtentSize:   c.conf.GetDnBinConf().GetExtentSize(),
		AddrPortToDn: map[string]*pb.DnCapacityEntry{},
		AddrPortToCn: map[string]*pb.CnCapacityEntry{},
	}
	for addr, dn := range c.dns {
		reply.AddrPortToDn[addr] = proto.Clone(dn).(*pb.DnCapacityEntry)
	}
	for addr, cn := range c.cns {
		reply.AddrPortToCn[addr] = proto.Clone(cn).(*pb.CnCapacityEntry)
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
	c.pools[req.GetSpName()] = &fakePool{
		req:    proto.Clone(req).(*pb.CreateStoragePoolRequest),
		tds:    map[string]*pb.ThinDevice{},
		nextTd: 1,
		ss:     map[string]*pb.Subsystem{},
		nextSs: 1,
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
	p, err := g.livePool(req.GetClusterName(), req.GetSpName(), false)
	if err != nil {
		return nil, err
	}
	return &pb.GetStoragePoolReply{
		SpName: req.GetSpName(),
		SpConf: &pb.SpConf{Deleting: p.deleting},
	}, nil
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
	if common.IsDnvNqn(req.GetNqn()) || !validNqn.MatchString(req.GetNqn()) {
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
	ss := p.ss[req.GetNqn()]
	if ss == nil {
		return nil, status.Errorf(codes.NotFound, "ss %q not found", req.GetNqn())
	}
	ss.AllowedHosts = append([]string(nil), req.GetAllowedHosts()...)
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
