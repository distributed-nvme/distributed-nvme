package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is the boundary tests of the gateway (gateway.md, Integration
// test plan): count ceilings of architecture.md, Cardinality limits, the
// size edges of architecture.md, Size → extents and Group on-leg layout, and
// of the transport service id, the meta ladder's cap (architecture.md,
// GrowSlice), and the id counters at their top (architecture.md, Globals: id
// allocation + shard buckets), each named by its case and reached through
// the real handlers against the package's etcd. A count ceiling is filled by
// real creates, the create that takes the last place is accepted, and the
// one after it is refused and writes nothing; a size bound is accepted at its
// edge and refused one past it, at both ends where it has two. The meta
// ladder's cap is not pinned at its edge here: its case pins that the cap is
// judged on the total before a grow, so that the last rung passes it, and
// model's TestMetaLadderExtCnt pins the cap's exact edge. The rest of
// architecture.md, Common validation, is validate_test.go's.
//
// Every count ceiling here is also raced at its last place. Two token-less
// creates ask for it — a token would make the later commit ABORTED "stale
// revision" (GW6) before it ever reached the count — and the second runs to
// its end inside the first, at the moment the first has read the counted key
// (bndRaceAtRead): the first then meets the ceiling in its deciding STM, so
// exactly one of the two commits on every run, and a ceiling checked anywhere
// but in that STM lets both through.

// bndRaceAtRead runs first under a ctx volReadHook watches and, at first's
// first read of key, runs second to its end before first goes on. It fails
// the test unless that read happened, and returns both results.
//
// The hook is the process's slog handler only while first runs, which reaches
// only this test's requests because no gateway test runs in parallel.
func bndRaceAtRead(
	t *testing.T,
	key string,
	first func(ctx context.Context) error,
	second func() error,
) (firstErr error, secondErr error) {
	t.Helper()
	prev := slog.Default()
	hook := &volReadHook{
		Handler: prev.Handler(),
		key:     key,
		actor:   func() { secondErr = second() },
	}
	slog.SetDefault(slog.New(hook))
	defer slog.SetDefault(prev)
	ctx, cancel := context.WithTimeout(
		context.WithValue(context.Background(), volReadHookKey{}, true),
		30*time.Second)
	defer cancel()
	firstErr = first(ctx)
	if !hook.fired.Load() {
		t.Fatalf("the first request never read %s: nothing was raced", key)
	}
	return firstErr, secondErr
}

// bndWantOneWinner asserts the outcome of a race at a ceiling's last place:
// the second request, which ran inside the first, committed, and the first
// was refused with code, its message naming the ceiling by msg.
func bndWantOneWinner(
	t *testing.T,
	firstErr error,
	secondErr error,
	code codes.Code,
	msg string,
) {
	t.Helper()
	if secondErr != nil {
		t.Fatalf("the create that took the last place: %v", secondErr)
	}
	wantCode(t, firstErr, code, "the create that raced it")
	if got := status.Convert(firstErr).Message(); !strings.Contains(got, msg) {
		t.Errorf("the refusal %q does not mention %q", got, msg)
	}
}

// bndWantMsg asserts code and that the message mentions msg.
func bndWantMsg(
	t *testing.T,
	err error,
	code codes.Code,
	msg string,
	label string,
) {
	t.Helper()
	wantCode(t, err, code, label)
	if got := status.Convert(err).Message(); !strings.Contains(got, msg) {
		t.Errorf("%s: the refusal %q does not mention %q", label, got, msg)
	}
}

// bndKeyCnt counts the keys under prefix.
func bndKeyCnt(t *testing.T, s *Server, prefix string) int {
	t.Helper()
	keys, _, err := s.cli.RangeKeys(context.Background(), prefix)
	if err != nil {
		t.Fatalf("RangeKeys %s: %v", prefix, err)
	}
	return len(keys)
}

// ---------------------------------------------------------------------------
// Per cluster: disk nodes, controller nodes, storage pools
// ---------------------------------------------------------------------------

// bndNodeKind is one of the two node kinds a cluster counts in its global
// (architecture.md, Globals: id allocation + shard buckets).
type bndNodeKind struct {
	what      string
	max       int
	create    func(ctx context.Context, s *Server, cluster, addr string) error
	confKey   func(cid uint64, addr string) string
	prefix    func(cid uint64) string
	globalKey func(cid uint64) string
	bucket    func(reply *pb.GetClusterReply) []uint32
}

var bndDnKind = bndNodeKind{
	what: "disk node",
	max:  common.MaxDnCntPerCluster,
	create: func(ctx context.Context, s *Server, cluster, addr string) error {
		_, err := s.CreateDiskNode(ctx, &pb.CreateDiskNodeRequest{
			ClusterName: cluster,
			AddrPort:    addr,
			NvmeTrConf:  testTrConf(addr),
		})
		return err
	},
	confKey:   model.DnConfKey,
	prefix:    model.DnConfPrefix,
	globalKey: model.DnGlobalKey,
	bucket: func(reply *pb.GetClusterReply) []uint32 {
		return reply.GetDnGlobal().GetShardBucket()
	},
}

var bndCnKind = bndNodeKind{
	what: "controller node",
	max:  common.MaxCnCntPerCluster,
	create: func(ctx context.Context, s *Server, cluster, addr string) error {
		_, err := s.CreateControllerNode(ctx, &pb.CreateControllerNodeRequest{
			ClusterName: cluster,
			AddrPort:    addr,
			NvmeTrConf:  testTrConf(addr),
		})
		return err
	},
	confKey:   model.CnConfKey,
	prefix:    model.CnConfPrefix,
	globalKey: model.CnGlobalKey,
	bucket: func(reply *pb.GetClusterReply) []uint32 {
		return reply.GetCnGlobal().GetShardBucket()
	},
}

// bndNodeCountLimit fills one cluster with nodes of one kind through the real
// create, each with a fake agent of its own that reports its size, races the
// last place and refuses the node after it (architecture.md, Globals: id
// allocation + shard buckets; GW12).
func bndNodeCountLimit(t *testing.T, kind bndNodeKind) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("count")
	cid := mustCluster(t, s, cluster)
	newNode := func(name string) string {
		addr := fakeAddrPort(t, name)
		startFakeAgent(t, addr, 16<<30)
		return addr
	}
	for idx := 0; idx < kind.max-1; idx++ {
		if err := kind.create(ctx, s, cluster, newNode("n")); err != nil {
			t.Fatalf("%s %d of %d: %v", kind.what, idx+1, kind.max, err)
		}
	}
	addrA, addrB := newNode("a"), newNode("b")
	errA, errB := bndRaceAtRead(t, kind.globalKey(cid),
		func(ctx context.Context) error {
			return kind.create(ctx, s, cluster, addrA)
		},
		func() error { return kind.create(ctx, s, cluster, addrB) })
	bndWantOneWinner(t, errA, errB, codes.ResourceExhausted,
		kind.what+" count")
	if hnodeExists(t, s, kind.confKey(cid, addrA)) {
		t.Errorf("the refused create wrote its %s", kind.what)
	}
	if got := bndKeyCnt(t, s, kind.prefix(cid)); got != kind.max {
		t.Errorf("%d %s records, want %d", got, kind.what, kind.max)
	}
	reply, err := s.GetCluster(ctx, &pb.GetClusterRequest{ClusterName: cluster})
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if got := bucketSum(kind.bucket(reply)); got != uint64(kind.max) {
		t.Errorf("shard_bucket sum %d, want %d", got, kind.max)
	}

	addrC := newNode("c")
	globalRev := hnodeModRev(t, s, kind.globalKey(cid))
	bndWantMsg(t, kind.create(ctx, s, cluster, addrC),
		codes.ResourceExhausted, kind.what+" count", "one over the ceiling")
	if hnodeExists(t, s, kind.confKey(cid, addrC)) {
		t.Errorf("the create one over the ceiling wrote its %s", kind.what)
	}
	if got := hnodeModRev(t, s, kind.globalKey(cid)); got != globalRev {
		t.Errorf("the global moved on a refusal: %d -> %d", globalRev, got)
	}
}

// TestDiskNodeCountLimit is MaxDnCntPerCluster through CreateDiskNode: 1024
// disk nodes, each behind its own fake agent, the last place raced, the next
// refused RESOURCE_EXHAUSTED (GW7).
func TestDiskNodeCountLimit(t *testing.T) {
	bndNodeCountLimit(t, bndDnKind)
}

// TestControllerNodeCountLimit is MaxCnCntPerCluster through
// CreateControllerNode, the same way.
func TestControllerNodeCountLimit(t *testing.T) {
	bndNodeCountLimit(t, bndCnKind)
}

// bndPoolReq is a one-cntlr, one-slice, one-extent RedundNone pool held to the
// named nodes, so that two such pools never draw the same node.
func bndPoolReq(
	cluster string,
	name string,
	dns []string,
	cns []string,
) *pb.CreateStoragePoolRequest {
	req := sptSmallSpec(name).req(cluster)
	req.DnSelector = &pb.NodeSelector{WhiteList: dns}
	req.CnSelector = &pb.NodeSelector{WhiteList: cns}
	return req
}

// bndPlantSpCnt plants the cluster's SpGlobal as if cnt pools lived in it.
// Only the bucket sum counts (architecture.md, Globals: id allocation + shard
// buckets).
func bndPlantSpCnt(env *sptEnv, cnt int) {
	env.t.Helper()
	bucket := zeroBucket()
	for idx := 0; idx < cnt; idx++ {
		bucket[idx%common.ShardBucketSize]++
	}
	mustPut(env.t, env.cli, model.SpGlobalKey(env.cid), &pb.SpGlobal{
		NextId:      uint64(cnt) + 1,
		ShardBucket: bucket,
	})
}

// TestStoragePoolCountLimit is MaxSpCntPerCluster on a planted count, which
// keeps it fast enough for every run; TestStoragePoolCountFullShape fills the
// same ceiling through real creates. One place left: two pools on disjoint
// nodes race for it, the second running inside the first's read of SpGlobal,
// which with the ceiling checked before the scans (GW9) is that pre-check's
// read, so the first passes it and meets the ceiling again in its STM; the
// next pool is refused and writes nothing, and a create re-sent under the
// name of the pool that took the last place is ALREADY_EXISTS, though free
// nodes would let its scan pass: the name is judged before the ceiling, as
// the STM judges it. A cluster at the ceiling with no node at all hears the
// ceiling, not the scan's shortfall, and a request that breaks a check of
// the request itself — the overflow, the merged geometry, the primary
// threshold — hears that check, since the ceiling comes after them all.
func TestStoragePoolCountLimit(t *testing.T) {
	env := sptNewEnv(t, 4, 2, sptCnFree)
	bndPlantSpCnt(env, common.MaxSpCntPerCluster-1)
	dnsA, cnsA := env.dnAddrs[:2], env.cnAddrs[:1]
	dnsB, cnsB := env.dnAddrs[2:], env.cnAddrs[1:]
	before := make(map[string]proto.Message)
	for _, addr := range dnsA {
		before[addr] = env.dnConf(addr)
	}
	before[cnsA[0]] = env.cnConf(cnsA[0])
	errA, errB := bndRaceAtRead(t, model.SpGlobalKey(env.cid),
		func(ctx context.Context) error {
			_, err := env.srv.CreateStoragePool(
				ctx, bndPoolReq(env.name, "pool-a", dnsA, cnsA))
			return err
		},
		func() error {
			_, err := env.srv.CreateStoragePool(
				env.ctx, bndPoolReq(env.name, "pool-b", dnsB, cnsB))
			return err
		})
	bndWantOneWinner(t, errA, errB, codes.ResourceExhausted,
		"storage pool count")
	if env.exists(model.SpConfKey(env.cid, "pool-a")) {
		t.Errorf("the refused create wrote its pool")
	}
	for _, addr := range dnsA {
		if got := env.dnConf(addr); !proto.Equal(got, before[addr]) {
			t.Errorf("the refused create moved %s", addr)
		}
	}
	if got := env.cnConf(cnsA[0]); !proto.Equal(got, before[cnsA[0]]) {
		t.Errorf("the refused create moved %s", cnsA[0])
	}
	if got := bucketSum(env.spGlobal().GetShardBucket()); got !=
		common.MaxSpCntPerCluster {
		t.Errorf("shard_bucket sum %d, want %d", got,
			common.MaxSpCntPerCluster)
	}

	dump := env.dump()
	_, err := env.srv.CreateStoragePool(env.ctx,
		bndPoolReq(env.name, "pool-c", dnsA, cnsA))
	bndWantMsg(t, err, codes.ResourceExhausted, "storage pool count",
		"one over the ceiling")
	bndWantSameDump(t, dump, env.dump())
	_, err = env.srv.CreateStoragePool(env.ctx,
		bndPoolReq(env.name, "pool-b", dnsA, cnsA))
	bndWantMsg(t, err, codes.AlreadyExists, `"pool-b" already exists`,
		"a create under the winner's name, on free nodes")
	bndWantSameDump(t, dump, env.dump())

	bare := sptNewEnv(t, 0, 0, 0)
	bndPlantSpCnt(bare, common.MaxSpCntPerCluster)
	bareDump := bare.dump()
	bareCreate := func(edit func(req *pb.CreateStoragePoolRequest)) error {
		req := sptSmallSpec("pool-a").req(bare.name)
		edit(req)
		_, err := bare.srv.CreateStoragePool(bare.ctx, req)
		return err
	}
	bndWantMsg(t, bareCreate(func(*pb.CreateStoragePoolRequest) {}),
		codes.ResourceExhausted, "storage pool count",
		"at the ceiling with no node")
	bndWantMsg(t, bareCreate(func(req *pb.CreateStoragePoolRequest) {
		req.InitExtCnt = math.MaxUint64/sptExtSize + 1
	}), codes.InvalidArgument, "overflows", "an overflow at the ceiling")
	bndWantMsg(t, bareCreate(func(req *pb.CreateStoragePoolRequest) {
		// Three chunk blocks of the cluster's power-of-two block size make
		// a chunk that is not a power of two: only the merge breaks it.
		req.BdevConf = &pb.BdevConf{RedundConf: &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{BitmapChunkBlockCnt: 3},
			},
		}}
	}), codes.InvalidArgument, "bdev_conf merged over the cluster's",
		"a merged geometry broken at the ceiling")
	bndWantSameDump(t, bareDump, bare.dump())
	// An interval whose two rounds pass the default primary_unhealthy,
	// which the request leaves unset.
	bare.setCntlrInterval(common.DefaultPrimaryUnhealthy/2 + 1)
	bareDump = bare.dump()
	bndWantMsg(t, bareCreate(func(*pb.CreateStoragePoolRequest) {}),
		codes.InvalidArgument, "primary_unhealthy",
		"a primary threshold under its bound at the ceiling")
	bndWantSameDump(t, bareDump, bare.dump())
}

// bndWantSameDump fails for every key a refusal added, removed or rewrote.
func bndWantSameDump(t *testing.T, before, after map[string][]byte) {
	t.Helper()
	if len(before) != len(after) {
		t.Errorf("a refusal changed the key set: %d -> %d",
			len(before), len(after))
	}
	for key, value := range before {
		if string(value) != string(after[key]) {
			t.Errorf("a refusal rewrote %q", key)
		}
	}
}

// bndShapeDnCnt and bndShapeCnCnt are the planted nodes of the pool full
// shape. 4096 one-cntlr RedundNone pools are 8192 sides and 4096 cntlrs, so
// each node carries 128 of them: far below MaxSideCntPerDn and
// MaxCntlrCntPerCn, and few enough that the records every create rewrites stay
// small.
const (
	bndShapeDnCnt = 64
	bndShapeCnCnt = 32
)

// TestStoragePoolCountFullShape is the full shape of MaxSpCntPerCluster:
// every pool of a full cluster created one after the other through the real
// handler, the first taking sp_id 1, and the next one refused, writing
// nothing. No timing is asserted. It is skipped under -short.
func TestStoragePoolCountFullShape(t *testing.T) {
	if testing.Short() {
		t.Skip("the full shape of the pool count creates 4096 pools")
	}
	env := sptNewEnv(t, 0, 0, 0)
	for idx := 0; idx < bndShapeDnCnt; idx++ {
		env.putDn(fmt.Sprintf("dn-%02d:9000", idx), uint64(idx)+1,
			uint32(idx), 1024)
	}
	for idx := 0; idx < bndShapeCnCnt; idx++ {
		env.putCn(fmt.Sprintf("cn-%02d:9000", idx), uint64(idx)+1,
			uint32(idx), 1024)
	}
	for idx := 0; idx < common.MaxSpCntPerCluster; idx++ {
		reply, err := env.srv.CreateStoragePool(env.ctx,
			sptSmallSpec(fmt.Sprintf("pool-%04d", idx)).req(env.name))
		if err != nil {
			t.Fatalf("pool %d of %d: %v", idx+1,
				common.MaxSpCntPerCluster, err)
		}
		if idx == 0 && reply.GetSpId() != 1 {
			t.Fatalf("the first pool got sp_id %d, want 1", reply.GetSpId())
		}
	}
	if got := bndKeyCnt(t, env.srv, model.SpConfPrefix(env.cid)); got !=
		common.MaxSpCntPerCluster {
		t.Errorf("%d pools, want %d", got, common.MaxSpCntPerCluster)
	}
	if got := bucketSum(env.spGlobal().GetShardBucket()); got !=
		common.MaxSpCntPerCluster {
		t.Errorf("shard_bucket sum %d, want %d", got,
			common.MaxSpCntPerCluster)
	}
	globalRev := hnodeModRev(t, env.srv, model.SpGlobalKey(env.cid))
	_, err := env.srv.CreateStoragePool(env.ctx,
		sptSmallSpec("pool-over").req(env.name))
	bndWantMsg(t, err, codes.ResourceExhausted, "storage pool count",
		"one over the ceiling")
	if env.exists(model.SpConfKey(env.cid, "pool-over")) {
		t.Errorf("the refused create wrote its pool")
	}
	if got := hnodeModRev(t, env.srv, model.SpGlobalKey(env.cid)); got !=
		globalRev {
		t.Errorf("sp_global moved on a refusal: %d -> %d", globalRev, got)
	}
}

// ---------------------------------------------------------------------------
// Per node: sides on a disk node, cntlrs on a controller node
// ---------------------------------------------------------------------------

// bndPutRoomyDn plants a disk node whose total and free extent counts are
// both freeExt, so that no create a case makes runs it out of extents and only
// its side count can take it out of the allocator's view.
func bndPutRoomyDn(env *sptEnv, addr string, idx int, freeExt uint64) {
	env.t.Helper()
	env.putDn(addr, uint64(idx)+1, uint32(idx), freeExt)
	dn := env.dnConf(addr)
	dn.TotalExtCnt = freeExt
	mustPut(env.t, env.cli, model.DnConfKey(env.cid, addr), dn)
}

// bndPutRoomyCn is bndPutRoomyDn for a controller node.
func bndPutRoomyCn(env *sptEnv, addr string, idx int, freeExt uint64) {
	env.t.Helper()
	env.putCn(addr, uint64(idx)+1, uint32(idx), freeExt)
	cn := env.cnConf(addr)
	cn.TotalExtCnt = freeExt
	mustPut(env.t, env.cli, model.CnConfKey(env.cid, addr), cn)
}

// bndDnHasCapKey reports whether addr's capacity key is in the store at the
// free count its DnConf holds, the one place the presence rule of
// architecture.md, Capacity index keys, puts it.
func bndDnHasCapKey(env *sptEnv, addr string) bool {
	env.t.Helper()
	key := env.dnCapacityKey(addr, env.dnConf(addr).GetFreeExtCnt())
	return key != "" && hnodeExists(env.t, env.srv, key)
}

// bndCnHasCapKey is bndDnHasCapKey for a controller node.
func bndCnHasCapKey(env *sptEnv, addr string) bool {
	env.t.Helper()
	key := model.CnCapacityKey(
		env.cid, env.cnConf(addr).GetFreeExtCnt(), addr)
	return hnodeExists(env.t, env.srv, key)
}

// bndGrowOnto is a token-less data grow of one slice of spName, its new
// group held to the named disk nodes.
func bndGrowOnto(
	ctx context.Context,
	env *sptEnv,
	spName string,
	sliceId uint64,
	dns []string,
) error {
	_, err := env.srv.GrowSlice(ctx, &pb.GrowSliceRequest{
		ClusterName: env.name,
		SpName:      spName,
		SliceId:     sliceId,
		ExtCnt:      1,
		DnSelector:  &pb.NodeSelector{WhiteList: dns},
	})
	return err
}

// TestDiskNodeSideCountLimit is MaxSideCntPerDn, the most sides one disk
// node carries (architecture.md, Cardinality limits), through the real
// handlers. The gateway holds it through the presence rule of
// architecture.md, Capacity index keys: a node at the ceiling loses its
// capacity key, so no scan finds it, and a create that picked it before is
// sent back to its scan by the re-check of the pick in its STM (GW9).
//
// One disk node, dn-x, is filled to one side short by real data grows of one
// pool, each grow held to dn-x and the grows spread over the pool's
// MaxSliceCntPerSp slices, which keeps each slice record small. Two pool
// creates, each held to dn-x and a disk node of its own, race for the last
// side: the second runs to its end inside the first's first read of dn-x's
// DnConf, which is the re-check in the first's STM, since the scans read
// only capacity keys. The second takes the last side, and the first
// re-scans, finds dn-x gone and is refused. Once dn-x is full, a pool create
// and a grow that may use only it are refused and write nothing.
func TestDiskNodeSideCountLimit(t *testing.T) {
	const (
		dnX      = "dn-x:9000"
		sliceCnt = common.MaxSliceCntPerSp
		// A pool's legs land on distinct disk nodes, two groups a slice.
		fillDns = 2 * sliceCnt
	)
	env := sptNewEnv(t, 0, 0, 0)
	var dns []string
	for idx := 0; idx < fillDns+3; idx++ {
		addr := fmt.Sprintf("dn-%02d:9000", idx)
		dns = append(dns, addr)
		env.putDn(addr, uint64(idx)+1, uint32(idx), sptDnFree)
	}
	// The fill pool's one cntlr reserves every grown group on cn-fill.
	bndPutRoomyCn(env, "cn-fill:9000", 0, 4*common.MaxSideCntPerDn)
	for idx, addr := range []string{"cn-a:9000", "cn-b:9000", "cn-c:9000"} {
		env.putCn(addr, uint64(idx)+2, uint32(idx)+1, sptCnFree)
	}
	fill := sptSmallSpec("pool-fill")
	fill.sliceCnt = uint32(sliceCnt)
	req := fill.req(env.name)
	req.DnSelector = &pb.NodeSelector{WhiteList: dns[:fillDns]}
	req.CnSelector = &pb.NodeSelector{WhiteList: []string{"cn-fill:9000"}}
	if _, err := env.srv.CreateStoragePool(env.ctx, req); err != nil {
		t.Fatalf("CreateStoragePool pool-fill: %v", err)
	}
	sliceIds := env.spConf("pool-fill").GetSliceIdList()
	bndPutRoomyDn(env, dnX, fillDns+3, 4*common.MaxSideCntPerDn)
	for cnt := 0; cnt < common.MaxSideCntPerDn-1; cnt++ {
		err := bndGrowOnto(env.ctx, env, "pool-fill",
			sliceIds[cnt%sliceCnt], []string{dnX})
		if err != nil {
			t.Fatalf("side %d on %s: %v", cnt+1, dnX, err)
		}
	}
	if got := len(env.dnConf(dnX).GetSidePtrList()); got !=
		common.MaxSideCntPerDn-1 {
		t.Fatalf("%s holds %d sides, want %d", dnX, got,
			common.MaxSideCntPerDn-1)
	}
	if !bndDnHasCapKey(env, dnX) {
		t.Fatalf("%s one side short has no capacity key", dnX)
	}

	errA, errB := bndRaceAtRead(t, model.DnConfKey(env.cid, dnX),
		func(ctx context.Context) error {
			_, err := env.srv.CreateStoragePool(ctx, bndPoolReq(env.name,
				"pool-a", []string{dnX, dns[fillDns]}, []string{"cn-a:9000"}))
			return err
		},
		func() error {
			_, err := env.srv.CreateStoragePool(env.ctx, bndPoolReq(env.name,
				"pool-b", []string{dnX, dns[fillDns+1]},
				[]string{"cn-b:9000"}))
			return err
		})
	bndWantOneWinner(t, errA, errB, codes.ResourceExhausted, "disk nodes")
	if env.exists(model.SpConfKey(env.cid, "pool-a")) {
		t.Errorf("the refused create wrote its pool")
	}
	if got := len(env.dnConf(dnX).GetSidePtrList()); got !=
		common.MaxSideCntPerDn {
		t.Fatalf("%s holds %d sides, want %d", dnX, got,
			common.MaxSideCntPerDn)
	}
	if bndDnHasCapKey(env, dnX) {
		t.Errorf("%s at the ceiling kept its capacity key", dnX)
	}

	dump := env.dump()
	_, err := env.srv.CreateStoragePool(env.ctx, bndPoolReq(env.name,
		"pool-c", []string{dnX, dns[fillDns+2]}, []string{"cn-c:9000"}))
	bndWantMsg(t, err, codes.ResourceExhausted, "disk nodes",
		"a pool one side over the ceiling")
	bndWantMsg(t,
		bndGrowOnto(env.ctx, env, "pool-fill", sliceIds[sliceCnt-1],
			[]string{dnX}),
		codes.ResourceExhausted, "disk nodes",
		"a grow one side over the ceiling")
	bndWantSameDump(t, dump, env.dump())
}

// TestControllerNodeCntlrCountLimit is MaxCntlrCntPerCn, the most cntlrs one
// controller node hosts (architecture.md, Cardinality limits), the same way:
// cn-x filled to one cntlr short by real pool creates, each held to cn-x;
// two more pool creates race for the last cntlr, the second inside the
// first's first read of cn-x's CnConf in its STM; and once cn-x is full, a
// pool create and a cntlr create that may use only it are refused and write
// nothing. A pool has at most one cntlr on a node, so every cntlr on cn-x is
// another pool's.
func TestControllerNodeCntlrCountLimit(t *testing.T) {
	const (
		cnX   = "cn-x:9000"
		dnCnt = 8
	)
	env := sptNewEnv(t, 0, 0, 0)
	for idx := 0; idx < dnCnt; idx++ {
		bndPutRoomyDn(env, fmt.Sprintf("dn-%02d:9000", idx), idx,
			4*common.MaxCntlrCntPerCn)
	}
	env.putCn("cn-d:9000", 1, 0, sptCnFree)
	bndPutRoomyCn(env, cnX, 1, 4*common.MaxCntlrCntPerCn)
	onX := []string{cnX}
	reqD := sptSmallSpec("pool-d").req(env.name)
	reqD.CnSelector = &pb.NodeSelector{WhiteList: []string{"cn-d:9000"}}
	if _, err := env.srv.CreateStoragePool(env.ctx, reqD); err != nil {
		t.Fatalf("CreateStoragePool pool-d: %v", err)
	}
	for idx := 0; idx < common.MaxCntlrCntPerCn-1; idx++ {
		req := sptSmallSpec(fmt.Sprintf("pool-%03d", idx)).req(env.name)
		req.CnSelector = &pb.NodeSelector{WhiteList: onX}
		if _, err := env.srv.CreateStoragePool(env.ctx, req); err != nil {
			t.Fatalf("cntlr %d on %s: %v", idx+1, cnX, err)
		}
	}
	if got := len(env.cnConf(cnX).GetCntlrPtrList()); got !=
		common.MaxCntlrCntPerCn-1 {
		t.Fatalf("%s hosts %d cntlrs, want %d", cnX, got,
			common.MaxCntlrCntPerCn-1)
	}
	if !bndCnHasCapKey(env, cnX) {
		t.Fatalf("%s one cntlr short has no capacity key", cnX)
	}

	poolOnX := func(ctx context.Context, name string) error {
		req := sptSmallSpec(name).req(env.name)
		req.CnSelector = &pb.NodeSelector{WhiteList: onX}
		_, err := env.srv.CreateStoragePool(ctx, req)
		return err
	}
	errA, errB := bndRaceAtRead(t, model.CnConfKey(env.cid, cnX),
		func(ctx context.Context) error { return poolOnX(ctx, "pool-a") },
		func() error { return poolOnX(env.ctx, "pool-b") })
	bndWantOneWinner(t, errA, errB, codes.ResourceExhausted,
		"controller node")
	if env.exists(model.SpConfKey(env.cid, "pool-a")) {
		t.Errorf("the refused create wrote its pool")
	}
	if got := len(env.cnConf(cnX).GetCntlrPtrList()); got !=
		common.MaxCntlrCntPerCn {
		t.Fatalf("%s hosts %d cntlrs, want %d", cnX, got,
			common.MaxCntlrCntPerCn)
	}
	if bndCnHasCapKey(env, cnX) {
		t.Errorf("%s at the ceiling kept its capacity key", cnX)
	}

	dump := env.dump()
	bndWantMsg(t, poolOnX(env.ctx, "pool-c"), codes.ResourceExhausted,
		"controller node", "a pool one cntlr over the ceiling")
	_, err := env.srv.CreateCntlr(env.ctx, &pb.CreateCntlrRequest{
		ClusterName: env.name,
		SpName:      "pool-d",
		CntlidSlot:  1,
		CnSelector:  &pb.NodeSelector{WhiteList: onX},
	})
	bndWantMsg(t, err, codes.ResourceExhausted, "controller node",
		"a cntlr one over the ceiling")
	bndWantSameDump(t, dump, env.dump())
}

// ---------------------------------------------------------------------------
// Per storage pool: cntlrs, thin devices, subsystems, clones, transfers,
// migrations
// ---------------------------------------------------------------------------

// bndCreateCntlr is a token-less CreateCntlr on one slot, optionally held to
// the named CNs.
func bndCreateCntlr(
	ctx context.Context,
	env *sptEnv,
	slot uint32,
	cns []string,
) error {
	req := &pb.CreateCntlrRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		CntlidSlot:  slot,
	}
	if cns != nil {
		req.CnSelector = &pb.NodeSelector{WhiteList: cns}
	}
	_, err := env.srv.CreateCntlr(ctx, req)
	return err
}

// TestCntlrCountLimit is MaxCntlrCntPerSp through CreateCntlr. A pool one
// cntlr short races two creates on free slots and free CNs, the second inside
// the first's planning read, which then sees one place left, scans and meets
// the ceiling in its STM. One more is refused with free CNs left, and refused
// with the ceiling's own message when the only CN it may use already hosts a
// cntlr of the pool: the ceiling is checked before the scan (GW9).
func TestCntlrCountLimit(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, common.MaxCntlrCntPerSp+2, sptCnFree)
	spec := sptSmallSpec(sptSpName)
	spec.cntlrCnt = common.MaxCntlrCntPerSp - 1
	spId := env.createSp(spec)
	errA, errB := bndRaceAtRead(t, model.SpConfKey(env.cid, sptSpName),
		func(ctx context.Context) error {
			return bndCreateCntlr(ctx, env, 4, nil)
		},
		func() error { return bndCreateCntlr(env.ctx, env, 5, nil) })
	bndWantOneWinner(t, errA, errB, codes.ResourceExhausted, "cntlr count")
	conf := env.spConf(sptSpName)
	if got := len(conf.GetCntlrIdList()); got != common.MaxCntlrCntPerSp {
		t.Fatalf("%d cntlrs, want %d", got, common.MaxCntlrCntPerSp)
	}

	dump := env.dump()
	bndWantMsg(t, bndCreateCntlr(env.ctx, env, 6, nil),
		codes.ResourceExhausted, "cntlr count", "one over the ceiling")
	taken := env.cntlr(spId, conf.GetCntlrIdList()[0]).GetAddrPort()
	bndWantMsg(t, bndCreateCntlr(env.ctx, env, 6, []string{taken}),
		codes.ResourceExhausted, "cntlr count", "one over with no CN free")
	bndWantSameDump(t, dump, env.dump())
}

// bndCreateTd is a token-less CreateThinDevice of the fixture's size.
func bndCreateTd(ctx context.Context, env *volEnv, name string) error {
	_, err := env.srv.CreateThinDevice(ctx, &pb.CreateThinDeviceRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		TdName:      name,
		Size:        volTdSize,
	})
	return err
}

// TestThinDeviceCountLimit is MaxTdCntPerSp through CreateThinDevice: the
// pool filled to one place short by real creates, the last place raced, the
// next one refused.
func TestThinDeviceCountLimit(t *testing.T) {
	env := newVolEnv(t)
	for idx := 0; idx < common.MaxTdCntPerSp-1; idx++ {
		err := bndCreateTd(env.ctx, env, fmt.Sprintf("td-%04d", idx))
		if err != nil {
			t.Fatalf("thin device %d: %v", idx+1, err)
		}
	}
	errA, errB := bndRaceAtRead(t, model.SpConfKey(env.cid, volSpName),
		func(ctx context.Context) error {
			return bndCreateTd(ctx, env, "td-a")
		},
		func() error { return bndCreateTd(env.ctx, env, "td-b") })
	bndWantOneWinner(t, errA, errB, codes.ResourceExhausted, "thin devices")
	if env.exists(model.ThinDeviceKey(env.cid, volSpId, "td-a"),
		&pb.ThinDevice{}) {
		t.Errorf("the refused create wrote its thin device")
	}
	if got := len(env.spConf().GetTdNameList()); got != common.MaxTdCntPerSp {
		t.Errorf("%d thin devices, want %d", got, common.MaxTdCntPerSp)
	}
	before, beforeRev := env.spConf(), env.spRev()
	bndWantMsg(t, bndCreateTd(env.ctx, env, "td-c"), codes.ResourceExhausted,
		"thin devices", "one over the ceiling")
	env.wantUntouched(before, beforeRev)
}

// bndCreateSs is a token-less CreateSubsystem with the given allowed hosts.
func bndCreateSs(
	ctx context.Context,
	env *volEnv,
	nqn string,
	hosts []string,
) error {
	_, err := env.srv.CreateSubsystem(ctx, &pb.CreateSubsystemRequest{
		ClusterName:  env.cluster,
		SpName:       volSpName,
		Nqn:          nqn,
		AllowedHosts: hosts,
	})
	return err
}

// bndSsNqn is the host-facing NQN of subsystem idx.
func bndSsNqn(idx string) string {
	return "nqn.2024-01.io.example:ss-" + idx
}

// TestSubsystemCountLimit is MaxSsCntPerSp through CreateSubsystem: the first
// subsystem of an empty pool accepted, the last place raced, the next refused.
func TestSubsystemCountLimit(t *testing.T) {
	env := newVolEnv(t)
	for idx := 0; idx < common.MaxSsCntPerSp-1; idx++ {
		err := bndCreateSs(env.ctx, env, bndSsNqn(fmt.Sprint(idx)), nil)
		if err != nil {
			t.Fatalf("subsystem %d: %v", idx+1, err)
		}
	}
	errA, errB := bndRaceAtRead(t, model.SpConfKey(env.cid, volSpName),
		func(ctx context.Context) error {
			return bndCreateSs(ctx, env, bndSsNqn("a"), nil)
		},
		func() error { return bndCreateSs(env.ctx, env, bndSsNqn("b"), nil) })
	bndWantOneWinner(t, errA, errB, codes.ResourceExhausted, "subsystems")
	if env.exists(model.SubsystemKey(env.cid, volSpId, bndSsNqn("a")),
		&pb.Subsystem{}) {
		t.Errorf("the refused create wrote its subsystem")
	}
	before, beforeRev := env.spConf(), env.spRev()
	bndWantMsg(t, bndCreateSs(env.ctx, env, bndSsNqn("c"), nil),
		codes.ResourceExhausted, "subsystems", "one over the ceiling")
	env.wantUntouched(before, beforeRev)
}

// bndCreateNs is a token-less CreateNamespace in volNqn over the td "vol".
func bndCreateNs(
	ctx context.Context,
	env *volEnv,
	cluster string,
	nsIdx uint32,
) error {
	_, err := env.srv.CreateNamespace(ctx, &pb.CreateNamespaceRequest{
		ClusterName: cluster,
		SpName:      volSpName,
		Nqn:         volNqn,
		NsIdx:       nsIdx,
		TdName:      "vol",
	})
	return err
}

// TestNamespaceCountLimit is MaxNsCntPerSs through CreateNamespace: the last
// place raced, the next refused. On the full subsystem an ns_idx of 0 is
// still INVALID_ARGUMENT, and so it is against a cluster that does not exist:
// the reserved index is refused before any read (GW4).
func TestNamespaceCountLimit(t *testing.T) {
	env := newVolEnv(t)
	env.putSubsystem(volNqn, 501, nil)
	env.putTd("vol", 900, 7, 0, true)
	for idx := 1; idx < common.MaxNsCntPerSs; idx++ {
		err := bndCreateNs(env.ctx, env, env.cluster, uint32(idx))
		if err != nil {
			t.Fatalf("namespace %d: %v", idx, err)
		}
	}
	errA, errB := bndRaceAtRead(t, model.SubsystemKey(env.cid, volSpId, volNqn),
		func(ctx context.Context) error {
			return bndCreateNs(ctx, env, env.cluster, 10)
		},
		func() error { return bndCreateNs(env.ctx, env, env.cluster, 11) })
	bndWantOneWinner(t, errA, errB, codes.ResourceExhausted, "namespaces")
	if got := len(env.subsystem(volNqn).GetNsList()); got !=
		common.MaxNsCntPerSs {
		t.Fatalf("%d namespaces, want %d", got, common.MaxNsCntPerSs)
	}
	before, beforeRev := env.spConf(), env.spRev()
	bndWantMsg(t, bndCreateNs(env.ctx, env, env.cluster, 12),
		codes.ResourceExhausted, "namespaces", "one over the ceiling")
	bndWantMsg(t, bndCreateNs(env.ctx, env, env.cluster, 0),
		codes.InvalidArgument, "ns_idx", "ns_idx 0 on a full subsystem")
	bndWantMsg(t, bndCreateNs(env.ctx, env, env.cluster+"-none", 0),
		codes.InvalidArgument, "ns_idx", "ns_idx 0 in no cluster")
	env.wantUntouched(before, beforeRev)
}

// bndCreateClone is a token-less CreateClone onto dst.
func bndCreateClone(
	ctx context.Context,
	env *volEnv,
	name string,
	dst string,
) error {
	req := volCloneRequest(env, name, dst)
	req.SpRev = nil
	_, err := env.srv.CreateClone(ctx, req)
	return err
}

// TestCloneCountLimit is MaxCloneCntPerSp through CreateClone, each clone on
// its own destination thin device: the last place raced, the next refused.
// A latched clone still counts (architecture.md, Clones): while one drains
// the full pool still refuses, and once its drain has ended the place is
// free again.
func TestCloneCountLimit(t *testing.T) {
	env := newVolEnv(t)
	dst := func(idx int) string { return fmt.Sprintf("dst-%02d", idx) }
	for idx := 0; idx <= common.MaxCloneCntPerSp; idx++ {
		if err := bndCreateTd(env.ctx, env, dst(idx)); err != nil {
			t.Fatalf("thin device %s: %v", dst(idx), err)
		}
	}
	clone := func(idx int) string { return fmt.Sprintf("clone-%02d", idx) }
	last := common.MaxCloneCntPerSp - 1
	for idx := 0; idx < last; idx++ {
		err := bndCreateClone(env.ctx, env, clone(idx), dst(idx))
		if err != nil {
			t.Fatalf("clone %d: %v", idx+1, err)
		}
	}
	errA, errB := bndRaceAtRead(t, model.SpConfKey(env.cid, volSpName),
		func(ctx context.Context) error {
			return bndCreateClone(ctx, env, "clone-a", dst(last))
		},
		func() error {
			return bndCreateClone(env.ctx, env, "clone-b", dst(last+1))
		})
	bndWantOneWinner(t, errA, errB, codes.ResourceExhausted, "clones")
	if got := len(env.spConf().GetCloneNameList()); got !=
		common.MaxCloneCntPerSp {
		t.Fatalf("%d clones, want %d", got, common.MaxCloneCntPerSp)
	}
	before, beforeRev := env.spConf(), env.spRev()
	bndWantMsg(t, bndCreateClone(env.ctx, env, "clone-c", dst(last)),
		codes.ResourceExhausted, "clones", "one over the ceiling")
	env.wantUntouched(before, beforeRev)

	if _, err := env.srv.DeleteClone(env.ctx, &pb.DeleteCloneRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		CloneName:   clone(0),
		Force:       true,
	}); err != nil {
		t.Fatalf("DeleteClone %s: %v", clone(0), err)
	}
	bndWantMsg(t, bndCreateClone(env.ctx, env, "clone-c", dst(last)),
		codes.ResourceExhausted, "clones", "while a clone drains")
	volDrainClone(env, clone(0))
	if err := bndCreateClone(env.ctx, env, "clone-c", dst(last)); err != nil {
		t.Fatalf("the create once the drain has ended: %v", err)
	}
}

// bndCreateXfer is a token-less CreateTransfer over the origin namespace 1 of
// volNqn with the given allowed hosts.
func bndCreateXfer(
	ctx context.Context,
	env *volEnv,
	name string,
	hosts []string,
) error {
	_, err := env.srv.CreateTransfer(ctx, &pb.CreateTransferRequest{
		ClusterName:  env.cluster,
		SpName:       volSpName,
		XferName:     name,
		OriNqn:       volNqn,
		OriNsIdx:     1,
		AllowedHosts: hosts,
	})
	return err
}

// bndPutOrigin plants the subsystem with the one namespace every transfer of
// this file is built on.
func bndPutOrigin(env *volEnv) {
	env.t.Helper()
	env.putSubsystem(volNqn, 501, []*pb.Namespace{{
		NsId: 601, NsIdx: 1, TdId: 900,
	}})
}

// TestTransferCountLimit is MaxXferCntPerSp through CreateTransfer: the last
// place raced, the next refused.
func TestTransferCountLimit(t *testing.T) {
	env := newVolEnv(t)
	bndPutOrigin(env)
	for idx := 0; idx < common.MaxXferCntPerSp-1; idx++ {
		err := bndCreateXfer(env.ctx, env, fmt.Sprintf("xfer-%d", idx), nil)
		if err != nil {
			t.Fatalf("transfer %d: %v", idx+1, err)
		}
	}
	errA, errB := bndRaceAtRead(t, model.SpConfKey(env.cid, volSpName),
		func(ctx context.Context) error {
			return bndCreateXfer(ctx, env, "xfer-a", nil)
		},
		func() error { return bndCreateXfer(env.ctx, env, "xfer-b", nil) })
	bndWantOneWinner(t, errA, errB, codes.ResourceExhausted, "transfer count")
	if env.exists(model.TransferKey(env.cid, volSpId, "xfer-a"),
		&pb.Transfer{}) {
		t.Errorf("the refused create wrote its transfer")
	}
	before, beforeRev := env.spConf(), env.spRev()
	bndWantMsg(t, bndCreateXfer(env.ctx, env, "xfer-c", nil),
		codes.ResourceExhausted, "transfer count", "one over the ceiling")
	env.wantUntouched(before, beforeRev)
}

// bndCreateMigr is a token-less CreateMigration of one side, optionally held
// to the named DNs.
func bndCreateMigr(
	ctx context.Context,
	env *sptEnv,
	name string,
	sideId uint64,
	dns []string,
) error {
	req := &pb.CreateMigrationRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		MigrName:    name,
		SrcSideId:   sideId,
	}
	if dns != nil {
		req.DnSelector = &pb.NodeSelector{WhiteList: dns}
	}
	_, err := env.srv.CreateMigration(ctx, req)
	return err
}

// TestMigrationCountLimit is MaxMigrCntPerSp through CreateMigration, on the
// eight legs of a two-slice md-raid1 pool, each migration on a leg of its
// own: the last place raced, the second create inside the first's planning
// read, which then scans and meets the ceiling in its STM. One more is refused
// on a free leg, and refused with the ceiling's own message when the only DN
// it may use is its own leg's: the ceiling is checked before the scan (GW9).
func TestMigrationCountLimit(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	env.createSp(sptDefaultSpec(sptSpName))
	sides := env.walkSides(env.spConf(sptSpName))
	if len(sides) < common.MaxMigrCntPerSp+2 {
		t.Fatalf("the pool has %d legs, the case needs %d",
			len(sides), common.MaxMigrCntPerSp+2)
	}
	last := common.MaxMigrCntPerSp - 1
	for idx := 0; idx < last; idx++ {
		err := bndCreateMigr(env.ctx, env, fmt.Sprintf("migr-%d", idx),
			sides[idx].SideId, nil)
		if err != nil {
			t.Fatalf("migration %d: %v", idx+1, err)
		}
	}
	errA, errB := bndRaceAtRead(t, model.SpConfKey(env.cid, sptSpName),
		func(ctx context.Context) error {
			return bndCreateMigr(ctx, env, "migr-a", sides[last].SideId, nil)
		},
		func() error {
			return bndCreateMigr(env.ctx, env, "migr-b",
				sides[last+1].SideId, nil)
		})
	bndWantOneWinner(t, errA, errB, codes.ResourceExhausted, "migrations")
	if got := len(env.spConf(sptSpName).GetMigrNameList()); got !=
		common.MaxMigrCntPerSp {
		t.Fatalf("%d migrations, want %d", got, common.MaxMigrCntPerSp)
	}

	dump := env.dump()
	free := sides[last]
	bndWantMsg(t, bndCreateMigr(env.ctx, env, "migr-c", free.SideId, nil),
		codes.ResourceExhausted, "migrations", "one over the ceiling")
	bndWantMsg(t, bndCreateMigr(env.ctx, env, "migr-c", free.SideId,
		[]string{free.AddrPort}),
		codes.ResourceExhausted, "migrations", "one over with no DN free")
	bndWantSameDump(t, dump, env.dump())
}

// ---------------------------------------------------------------------------
// Per group and slice: spare legs and groups; per migration: bitmap chunks
// ---------------------------------------------------------------------------

// bndCreateSpare is a token-less CreateSpareLeg on the fixture's data group.
func bndCreateSpare(ctx context.Context, env *volEnv) error {
	_, err := env.srv.CreateSpareLeg(ctx, &pb.CreateSpareLegRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		GrpId:       volDataGrpId,
	})
	return err
}

// TestSpareLegCountLimit is MaxSpareLegPerGrp through CreateSpareLeg: the
// first spare of a group accepted, its side flipped provisioned as the worker
// does, and the last place raced. The second create runs inside the first's
// planning snapshot, so the first passes the gateway's own check of the list
// and meets the model op's check in its STM, which answers the ceiling with
// the code GW7 lists, RESOURCE_EXHAUSTED, and the words of the gateway's
// check. The next spare is refused by that check, writing nothing.
func TestSpareLegCountLimit(t *testing.T) {
	env := newVolEnv(t)
	if err := bndCreateSpare(env.ctx, env); err != nil {
		t.Fatalf("the first spare: %v", err)
	}
	slice := env.slice()
	for _, spare := range volGrpOf(t, slice, volDataGrpId).GetSpareLegList() {
		for _, side := range spare.GetSideList() {
			side.Provisioned = true
		}
	}
	env.putSlice(slice)
	errA, errB := bndRaceAtRead(t,
		model.SliceKey(env.cid, volSpId, volSliceId),
		func(ctx context.Context) error { return bndCreateSpare(ctx, env) },
		func() error { return bndCreateSpare(env.ctx, env) })
	bndWantOneWinner(t, errA, errB, codes.ResourceExhausted, "spare legs")
	grp := volGrpOf(t, env.slice(), volDataGrpId)
	if got := len(grp.GetSpareLegList()); got != common.MaxSpareLegPerGrp {
		t.Fatalf("%d spare legs, want %d", got, common.MaxSpareLegPerGrp)
	}
	before, beforeRev := env.spConf(), env.spRev()
	slice = env.slice()
	over := bndCreateSpare(env.ctx, env)
	bndWantMsg(t, over, codes.ResourceExhausted, "spare legs",
		"one over the ceiling")
	if got, want := status.Convert(errA).Message(),
		status.Convert(over).Message(); got != want {
		t.Errorf("the ceiling has two refusals: %q in the STM, %q before "+
			"the scan", got, want)
	}
	env.wantUntouched(before, beforeRev)
	if !proto.Equal(env.slice(), slice) {
		t.Errorf("the refusal rewrote the slice")
	}
}

// bndGrowData is a token-less data grow of one slice.
func bndGrowData(
	ctx context.Context,
	env *sptEnv,
	sliceId uint64,
) error {
	_, err := env.srv.GrowSlice(ctx, &pb.GrowSliceRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		SliceId:     sliceId,
		ExtCnt:      1,
	})
	return err
}

// TestSliceGroupCountLimit is MaxGrpCntPerSlice through GrowSlice: a slice's
// data list grown by real grows to one place short, the last place raced —
// the second grow inside the first's planning snapshot, so the first passes
// the gateway's check of the list and meets model.GrowSlice's in its STM —
// and the next grow refused. The group ceiling is the object's own state, so
// both refusals are FAILED_PRECONDITION (GW7).
func TestSliceGroupCountLimit(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, 1024)
	spId := env.createSp(sptSmallSpec(sptSpName))
	sliceId := env.spConf(sptSpName).GetSliceIdList()[0]
	for cnt := 1; cnt < common.MaxGrpCntPerSlice-1; cnt++ {
		if err := bndGrowData(env.ctx, env, sliceId); err != nil {
			t.Fatalf("data group %d: %v", cnt+1, err)
		}
	}
	errA, errB := bndRaceAtRead(t, model.SliceKey(env.cid, spId, sliceId),
		func(ctx context.Context) error {
			return bndGrowData(ctx, env, sliceId)
		},
		func() error { return bndGrowData(env.ctx, env, sliceId) })
	bndWantOneWinner(t, errA, errB, codes.FailedPrecondition,
		model.ReasonGrpListFull)
	if got := len(env.slice(spId, sliceId).GetDataGrpList()); got !=
		common.MaxGrpCntPerSlice {
		t.Fatalf("%d data groups, want %d", got, common.MaxGrpCntPerSlice)
	}
	dump := env.dump()
	bndWantMsg(t, bndGrowData(env.ctx, env, sliceId),
		codes.FailedPrecondition, "data groups", "one over the ceiling")
	bndWantSameDump(t, dump, env.dump())
}

// bndAppendMigrBm is a token-less AppendMigrationBitmap of one byte.
func bndAppendMigrBm(ctx context.Context, env *volEnv, name string) error {
	_, err := env.srv.AppendMigrationBitmap(ctx,
		&pb.AppendMigrationBitmapRequest{
			ClusterName: env.cluster,
			SpName:      volSpName,
			MigrName:    name,
			Bitmap:      []byte{0x01},
		})
	return err
}

// TestMigrationBitmapCountLimit is MaxMigrBmCnt through AppendMigrationBitmap:
// one chunk short, two appends race for the last index, and the next append
// is refused. TestAppendMigrationBitmapIsAppendOnly holds the rest.
func TestMigrationBitmapCountLimit(t *testing.T) {
	env := newVolEnv(t)
	volCreateMigration(env, "migr-a")
	for idx := 0; idx < common.MaxMigrBmCnt-1; idx++ {
		if err := bndAppendMigrBm(env.ctx, env, "migr-a"); err != nil {
			t.Fatalf("chunk %d: %v", idx, err)
		}
	}
	errA, errB := bndRaceAtRead(t,
		model.MigrationKey(env.cid, volSpId, "migr-a"),
		func(ctx context.Context) error {
			return bndAppendMigrBm(ctx, env, "migr-a")
		},
		func() error { return bndAppendMigrBm(env.ctx, env, "migr-a") })
	wantCode(t, errB, codes.OK, "the append that took the last index")
	wantCode(t, errA, codes.ResourceExhausted, "the append that raced it")
	if got := env.migration("migr-a").GetBmCnt(); got != common.MaxMigrBmCnt {
		t.Fatalf("bm_cnt %d, want %d", got, common.MaxMigrBmCnt)
	}
	before, beforeRev := env.spConf(), env.spRev()
	wantCode(t, bndAppendMigrBm(env.ctx, env, "migr-a"),
		codes.ResourceExhausted, "one over the ceiling")
	env.wantUntouched(before, beforeRev)
}

// ---------------------------------------------------------------------------
// Allowed hosts through the handlers
// ---------------------------------------------------------------------------

// bndHosts is cnt distinct host NQNs.
func bndHosts(cnt int) []string {
	hosts := make([]string, 0, cnt)
	for idx := 0; idx < cnt; idx++ {
		hosts = append(hosts, fmt.Sprintf("nqn.2024-01.io.dnv:host-%d", idx))
	}
	return hosts
}

// TestAllowedHostLimitThroughHandlers is MaxHostCntPerSs through the four
// RPCs that take an allowed_hosts list: an empty list and a list of exactly
// the maximum are accepted and stored as sent, in the subsystem or the
// transfer the RPC writes, and one more host is INVALID_ARGUMENT and writes
// nothing.
func TestAllowedHostLimitThroughHandlers(t *testing.T) {
	env := newVolEnv(t)
	bndPutOrigin(env)
	full, over := bndHosts(common.MaxHostCntPerSs),
		bndHosts(common.MaxHostCntPerSs+1)
	refuse := func(label string, err error) {
		t.Helper()
		bndWantMsg(t, err, codes.InvalidArgument, "allowed_hosts", label)
	}
	wantStored := func(label string, got []string, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s: stored %q, want %q", label, got, want)
		}
	}
	xferHosts := func(name string) []string {
		t.Helper()
		xfer := &pb.Transfer{}
		env.get(model.TransferKey(env.cid, volSpId, name), xfer)
		return xfer.GetAllowedHosts()
	}

	if err := bndCreateSs(env.ctx, env, bndSsNqn("none"), nil); err != nil {
		t.Fatalf("CreateSubsystem with no host: %v", err)
	}
	wantStored("CreateSubsystem with no host",
		env.subsystem(bndSsNqn("none")).GetAllowedHosts(), nil)
	if err := bndCreateSs(env.ctx, env, bndSsNqn("full"), full); err != nil {
		t.Fatalf("CreateSubsystem with %d hosts: %v", len(full), err)
	}
	wantStored("CreateSubsystem at the maximum",
		env.subsystem(bndSsNqn("full")).GetAllowedHosts(), full)
	before, beforeRev := env.spConf(), env.spRev()
	refuse("CreateSubsystem one over",
		bndCreateSs(env.ctx, env, bndSsNqn("over"), over))
	env.wantUntouched(before, beforeRev)

	update := func(hosts []string) error {
		_, err := env.srv.UpdateSubsystemHosts(env.ctx,
			&pb.UpdateSubsystemHostsRequest{
				ClusterName:  env.cluster,
				SpName:       volSpName,
				Nqn:          bndSsNqn("none"),
				AllowedHosts: hosts,
			})
		return err
	}
	if err := update(full); err != nil {
		t.Fatalf("UpdateSubsystemHosts to %d hosts: %v", len(full), err)
	}
	wantStored("UpdateSubsystemHosts to the maximum",
		env.subsystem(bndSsNqn("none")).GetAllowedHosts(), full)
	before, beforeRev = env.spConf(), env.spRev()
	refuse("UpdateSubsystemHosts one over", update(over))
	env.wantUntouched(before, beforeRev)
	wantStored("UpdateSubsystemHosts after the refusal",
		env.subsystem(bndSsNqn("none")).GetAllowedHosts(), full)

	if err := bndCreateXfer(env.ctx, env, "xfer-full", full); err != nil {
		t.Fatalf("CreateTransfer with %d hosts: %v", len(full), err)
	}
	wantStored("CreateTransfer at the maximum", xferHosts("xfer-full"), full)
	before, beforeRev = env.spConf(), env.spRev()
	refuse("CreateTransfer one over",
		bndCreateXfer(env.ctx, env, "xfer-over", over))
	env.wantUntouched(before, beforeRev)
	if env.exists(model.TransferKey(env.cid, volSpId, "xfer-over"),
		&pb.Transfer{}) {
		t.Errorf("the refused CreateTransfer wrote its transfer")
	}
	updateXfer := func(hosts []string) error {
		_, err := env.srv.UpdateTransferHosts(env.ctx,
			&pb.UpdateTransferHostsRequest{
				ClusterName:  env.cluster,
				SpName:       volSpName,
				XferName:     "xfer-full",
				AllowedHosts: hosts,
			})
		return err
	}
	if err := updateXfer(nil); err != nil {
		t.Fatalf("UpdateTransferHosts to no host: %v", err)
	}
	wantStored("UpdateTransferHosts to no host", xferHosts("xfer-full"), nil)
	if err := updateXfer(full); err != nil {
		t.Fatalf("UpdateTransferHosts to %d hosts: %v", len(full), err)
	}
	wantStored("UpdateTransferHosts to the maximum",
		xferHosts("xfer-full"), full)
	before, beforeRev = env.spConf(), env.spRev()
	refuse("UpdateTransferHosts one over", updateXfer(over))
	env.wantUntouched(before, beforeRev)
	wantStored("UpdateTransferHosts after the refusal",
		xferHosts("xfer-full"), full)
}

// ---------------------------------------------------------------------------
// Sizes
// ---------------------------------------------------------------------------

// TestNodeSizeBoundaries pins the size edges of architecture.md, Size →
// extents, through the two node creates on the default 1 GiB extent: a disk
// node of exactly one extent is accepted and one byte less is refused; a
// controller node budget of exactly MaxCnCap is used whole, one extent more is
// cut to it — one byte more would floor to the same count cut or not — a
// budget of exactly one extent is accepted and one byte less is refused, and
// a non-zero budget under MinCnCap counts as no opinion. Every refusal writes
// nothing.
func TestNodeSizeBoundaries(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("size")
	cid := mustCluster(t, s, cluster)
	ext := uint64(common.DefaultDnExtSize)
	for _, tc := range []struct {
		label    string
		kind     bndNodeKind
		size     uint64
		wantCode codes.Code
		wantExt  uint64
	}{
		{"a disk node of one extent", bndDnKind, ext, codes.OK, 1},
		{"a disk node one byte short of an extent", bndDnKind, ext - 1,
			codes.InvalidArgument, 0},
		{"a controller node of exactly MaxCnCap", bndCnKind,
			common.MaxCnCap, codes.OK, common.MaxCnCap / ext},
		{"a controller node one extent over MaxCnCap", bndCnKind,
			common.MaxCnCap + ext, codes.OK, common.MaxCnCap / ext},
		{"a controller node of one extent", bndCnKind, ext, codes.OK, 1},
		{"a controller node one byte short of an extent", bndCnKind,
			ext - 1, codes.InvalidArgument, 0},
		{"a controller node one byte under MinCnCap", bndCnKind,
			common.MinCnCap - 1, codes.OK, common.DefaultCnCap / ext},
	} {
		addr := fakeAddrPort(t, "size")
		startFakeAgent(t, addr, tc.size)
		globalRev := hnodeModRev(t, s, tc.kind.globalKey(cid))
		err := tc.kind.create(ctx, s, cluster, addr)
		wantCode(t, err, tc.wantCode, tc.label)
		if tc.wantCode != codes.OK {
			if hnodeExists(t, s, tc.kind.confKey(cid, addr)) {
				t.Errorf("%s: the refusal wrote a record", tc.label)
			}
			if got := hnodeModRev(t, s, tc.kind.globalKey(cid)); got !=
				globalRev {
				t.Errorf("%s: the refusal moved the global", tc.label)
			}
			continue
		}
		var total, free uint64
		if tc.kind.what == bndDnKind.what {
			dn := &pb.DnConf{}
			hnodeGet(t, s, model.DnConfKey(cid, addr), dn)
			total, free = dn.GetTotalExtCnt(), dn.GetFreeExtCnt()
		} else {
			cn := &pb.CnConf{}
			hnodeGet(t, s, model.CnConfKey(cid, addr), cn)
			total, free = cn.GetTotalExtCnt(), cn.GetFreeExtCnt()
		}
		if total != tc.wantExt || free != tc.wantExt {
			t.Errorf("%s: total %d free %d, want %d", tc.label, total, free,
				tc.wantExt)
		}
	}
}

// bndGeomCluster creates a cluster with the given extent size through
// CreateCluster, four disk nodes and one controller node behind fake agents,
// each with room for eight extents.
func bndGeomCluster(t *testing.T, s *Server, extentSize uint64) string {
	t.Helper()
	ctx := context.Background()
	cluster := hnodeName("geom")
	if _, err := s.CreateCluster(ctx, &pb.CreateClusterRequest{
		ClusterName: cluster,
		DnBinConf:   &pb.DnBinConf{ExtentSize: extentSize},
	}); err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	for idx := 0; idx < 4; idx++ {
		mustDn(t, s, cluster, fakeAddrPort(t, "dn"), "", 8*extentSize)
	}
	mustCn(t, s, cluster, fakeAddrPort(t, "cn"), "", 8*extentSize)
	return cluster
}

// TestGroupHoldsItsMetaBlocksExactly pins the group geometry rule of
// architecture.md, Group on-leg layout: meta region, data region, health
// block, at its edge, through CreateStoragePool on 1 GiB pool blocks. A group
// one block larger than its meta blocks is the smallest that is accepted and
// stores one data block; a group of exactly its meta blocks is refused
// INVALID_ARGUMENT and writes no pool. A RedundNone group needs one meta block,
// so 2 GiB extents are accepted and 1 GiB extents refused; an md-raid1 group
// with one-block bitmap chunks needs three (the md superblock block, one
// bitmap block, the health block), so 4 GiB extents are accepted and 3 GiB
// extents refused. The one-extent meta group and the one-extent data group
// meet the edge together.
func TestGroupHoldsItsMetaBlocksExactly(t *testing.T) {
	const gib = uint64(1) << 30
	for _, tc := range []struct {
		label      string
		extentSize uint64
		raid1      bool
		wantCode   codes.Code
		wantMeta   uint64
	}{
		{"RedundNone, one data block", 2 * gib, false, codes.OK, 1},
		{"RedundNone, no data block", 1 * gib, false,
			codes.InvalidArgument, 0},
		{"md-raid1, one data block", 4 * gib, true, codes.OK, 3},
		{"md-raid1, no data block", 3 * gib, true, codes.InvalidArgument, 0},
	} {
		t.Run(tc.label, func(t *testing.T) {
			s := newTestServer(t)
			ctx := context.Background()
			cluster := bndGeomCluster(t, s, tc.extentSize)
			bdev := &pb.BdevConf{
				DmPoolConf: &pb.DmPoolConf{DataBlockSize: gib},
			}
			if tc.raid1 {
				bdev.RedundConf = &pb.RedundConf{
					RedunKind: &pb.RedundConf_RedundMdRaid1{
						RedundMdRaid1: &pb.RedundMdRaid1{
							BitmapChunkBlockCnt: 1,
						},
					},
				}
			}
			_, err := s.CreateStoragePool(ctx, &pb.CreateStoragePoolRequest{
				ClusterName: cluster,
				SpName:      "pool",
				CntlrCnt:    1,
				SliceCnt:    1,
				InitExtCnt:  1,
				BdevConf:    bdev,
			})
			if tc.wantCode != codes.OK {
				bndWantMsg(t, err, tc.wantCode, "meta blocks", tc.label)
				reply, err := s.ListStoragePools(ctx,
					&pb.ListStoragePoolsRequest{ClusterName: cluster})
				if err != nil {
					t.Fatalf("ListStoragePools: %v", err)
				}
				if len(reply.GetSpName()) != 0 {
					t.Errorf("the refusal wrote a pool: %v",
						reply.GetSpName())
				}
				return
			}
			wantCode(t, err, codes.OK, tc.label)
			reply, err := s.GetStoragePool(ctx, &pb.GetStoragePoolRequest{
				ClusterName: cluster,
				SpName:      "pool",
			})
			if err != nil {
				t.Fatalf("GetStoragePool: %v", err)
			}
			for _, slice := range reply.GetSliceList() {
				for _, grp := range append(slice.GetMetaGrpList(),
					slice.GetDataGrpList()...) {
					if grp.GetMetaBlocks() != tc.wantMeta ||
						grp.GetDataBlocks() != 1 {
						t.Errorf("group %d: meta %d data %d, want %d and 1",
							grp.GetGrpId(), grp.GetMetaBlocks(),
							grp.GetDataBlocks(), tc.wantMeta)
					}
				}
			}
		})
	}
}

// TestCreateStoragePoolOverflowBeforeTheScan pins the overflow refusal of
// architecture.md, Storage pools: an init_ext_cnt whose bytes at the cluster's
// extent size pass uint64 is INVALID_ARGUMENT on a cluster with no node at
// all, so it is judged before the scans, and the largest init_ext_cnt that
// does not pass it goes on to the scan, which finds no node
// (RESOURCE_EXHAUSTED).
func TestCreateStoragePoolOverflowBeforeTheScan(t *testing.T) {
	env := sptNewEnv(t, 0, 0, 0)
	most := uint64(math.MaxUint64) / sptExtSize
	req := sptSmallSpec(sptSpName).req(env.name)
	req.InitExtCnt = most + 1
	_, err := env.srv.CreateStoragePool(env.ctx, req)
	bndWantMsg(t, err, codes.InvalidArgument, "overflows", "one past uint64")
	req.InitExtCnt = most
	_, err = env.srv.CreateStoragePool(env.ctx, req)
	wantCode(t, err, codes.ResourceExhausted, "the largest that fits")
	if strings.Contains(status.Convert(err).Message(), "overflows") {
		t.Errorf("the largest init_ext_cnt that fits was refused as an " +
			"overflow")
	}
}

// TestMetaLadderPassesItsCapWithAnOddExtent pins the meta ladder of
// architecture.md, GrowSlice, where its cap, dm-thin's largest metadata
// device, is judged on the meta total before a grow, so the rung it allows
// may carry the total past the cap: with 5 GiB extents the meta total grows
// 5, 10, 20 GiB, and only the grow after that is FAILED_PRECONDITION. The
// last rung passes the cap so at every extent size whose ladder does not
// land on the cap, the default size included; 5 GiB extents put the cap
// well inside that rung, so the case holds for any cap above 10 GiB and up
// to 20 GiB and pins the rule, not the cap's value.
func TestMetaLadderPassesItsCapWithAnOddExtent(t *testing.T) {
	const extentSize = uint64(5) << 30
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	env.cc.DnBinConf.ExtentSize = extentSize
	mustPut(t, env.cli, model.ClusterConfKey(env.name), env.cc)
	spId := env.createSp(sptSmallSpec(sptSpName))
	sliceId := env.spConf(sptSpName).GetSliceIdList()[0]
	grow := func() error {
		_, err := env.srv.GrowSlice(env.ctx, &pb.GrowSliceRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			SliceId:     sliceId,
			IsMeta:      true,
		})
		return err
	}
	for idx := 0; idx < 2; idx++ {
		if err := grow(); err != nil {
			t.Fatalf("meta grow %d: %v", idx+1, err)
		}
	}
	total := uint64(0)
	var extCnts []uint64
	for _, grp := range env.slice(spId, sliceId).GetMetaGrpList() {
		total += grp.GetExtCnt()
		extCnts = append(extCnts, grp.GetExtCnt())
	}
	if fmt.Sprint(extCnts) != "[1 1 2]" {
		t.Errorf("meta groups of %v extents, want [1 1 2]", extCnts)
	}
	if total*extentSize != 20<<30 {
		t.Errorf("meta total %d bytes, want 20 GiB", total*extentSize)
	}
	bndWantMsg(t, grow(), codes.FailedPrecondition, "dm-thin metadata cap",
		"the grow past 20 GiB")
}

// ---------------------------------------------------------------------------
// The id counters at their top: the id step's last value and the last id
// ---------------------------------------------------------------------------

// bndPlantLastIds plants every id counter of the cluster and of pool-a at the
// last value the id step can move by its stride without wrapping, and the
// pool's next thin id at the last value that moves to MaxThinDevId itself.
// Real calls cannot get there: each step moves a counter by IdStepStride.
func bndPlantLastIds(env *sptEnv) {
	env.t.Helper()
	last := uint64(math.MaxUint64) - uint64(common.IdStepStride)
	dnG, cnG, spG := env.idStepGlobals()
	dnG.NextId, cnG.NextId, spG.NextId = last, last, last
	mustPut(env.t, env.cli, model.DnGlobalKey(env.cid), dnG)
	mustPut(env.t, env.cli, model.CnGlobalKey(env.cid), cnG)
	mustPut(env.t, env.cli, model.SpGlobalKey(env.cid), spG)
	conf := env.spConf("pool-a")
	conf.NextId = last
	conf.NextDevId = common.MaxThinDevId - common.ThinIdStepStride
	mustPut(env.t, env.cli, model.SpConfKey(env.cid, "pool-a"), conf)
}

// TestIdStepAtItsLastValue pins GW19 at its edge: with every counter at the
// last value its stride does not wrap, the step is accepted and lands each
// on math.MaxUint64, and the pool's thin id on MaxThinDevId; the step after
// it is FAILED_PRECONDITION and writes nothing.
func TestIdStepAtItsLastValue(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	env.createSp(sptSmallSpec("pool-a"))
	bndPlantLastIds(env)
	reply, err := env.advanceIds()
	if err != nil {
		t.Fatalf("the step at the last value: %v", err)
	}
	if reply.GetSpCnt() != 1 {
		t.Errorf("sp_cnt %d, want 1", reply.GetSpCnt())
	}
	dnG, cnG, spG := env.idStepGlobals()
	conf := env.spConf("pool-a")
	for what, got := range map[string]uint64{
		"dn_global": dnG.GetNextId(),
		"cn_global": cnG.GetNextId(),
		"sp_global": spG.GetNextId(),
		"pool-a":    conf.GetNextId(),
	} {
		if got != math.MaxUint64 {
			t.Errorf("%s next_id %d, want %d", what, got,
				uint64(math.MaxUint64))
		}
	}
	if got := conf.GetNextDevId(); got != common.MaxThinDevId {
		t.Errorf("next_dev_id %d, want %d", got, common.MaxThinDevId)
	}
	before := env.dumpAll()
	_, err = env.advanceIds()
	wantCode(t, err, codes.FailedPrecondition, "the step after it")
	if changed := idStepChanged(before, env.dumpAll()); len(changed) != 0 {
		t.Errorf("the refused step wrote %q", changed)
	}
}

// TestIdCounterAtItsLastValueIsNotReused pins the rule of architecture.md,
// Globals: id allocation + shard buckets, that no next_id hands out the
// largest 64-bit value, on counters planted one below it, the last id they
// hold. For the cluster's disk node counter (mintClusterId) and for a pool's
// counter (the per-SP minter's commit), the create there gets that id and
// leaves next_id at the largest value, and the create after it is
// FAILED_PRECONDITION and writes nothing. A create that takes two ids,
// CreateMigration's migration id and its new side's, is refused the same way
// while one id is left. The pool's next thin id is left low, so that no pool
// create is refused for want of a thin id first.
func TestIdCounterAtItsLastValueIsNotReused(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	env.createSp(sptSmallSpec(sptSpName))
	last := uint64(math.MaxUint64) - 1
	dnG, _, _ := env.idStepGlobals()
	dnG.NextId = last
	mustPut(t, env.cli, model.DnGlobalKey(env.cid), dnG)
	planted := env.spConf(sptSpName)
	planted.NextId = last
	mustPut(t, env.cli, model.SpConfKey(env.cid, sptSpName), planted)
	// refused runs one create that must be FAILED_PRECONDITION, its message
	// naming msg, and fails the test if the create wrote anything at all.
	refused := func(label string, msg string, create func() error) {
		t.Helper()
		before := env.dumpAll()
		bndWantMsg(t, create(), codes.FailedPrecondition, msg, label)
		if changed := idStepChanged(before, env.dumpAll()); len(changed) != 0 {
			t.Errorf("%s wrote %q", label, changed)
		}
	}
	createDn := func() (uint64, error) {
		addr := fakeAddrPort(t, "top")
		startFakeAgent(t, addr, 16<<30)
		reply, err := env.srv.CreateDiskNode(env.ctx,
			&pb.CreateDiskNodeRequest{
				ClusterName: env.name,
				AddrPort:    addr,
				NvmeTrConf:  testTrConf(addr),
			})
		return reply.GetDnId(), err
	}
	createTd := func(name string) (uint64, error) {
		reply, err := env.srv.CreateThinDevice(env.ctx,
			&pb.CreateThinDeviceRequest{
				ClusterName: env.name,
				SpName:      sptSpName,
				TdName:      name,
				Size:        uint64(common.DefaultDmRaid0StripeSize),
			})
		return reply.GetTdId(), err
	}

	if dnId, err := createDn(); err != nil || dnId != last {
		t.Fatalf("the create at the last dn id: %v, dn_id %d", err, dnId)
	}
	if dnG, _, _ = env.idStepGlobals(); dnG.GetNextId() != math.MaxUint64 {
		t.Errorf("dn_global next_id %d, want %d", dnG.GetNextId(),
			uint64(math.MaxUint64))
	}
	refused("the create after the last dn id", "no disk node id left",
		func() error {
			_, err := createDn()
			return err
		})

	side := env.walkSides(env.spConf(sptSpName))[0]
	refused("a migration, two ids with one left", model.ReasonNoIdLeft,
		func() error {
			return bndCreateMigr(env.ctx, env, "top", side.SideId, nil)
		})
	if tdId, err := createTd("top-0"); err != nil || tdId != last {
		t.Fatalf("the create at the last pool id: %v, td_id %d", err, tdId)
	}
	if got := env.spConf(sptSpName).GetNextId(); got != math.MaxUint64 {
		t.Errorf("the pool's next_id %d, want %d", got,
			uint64(math.MaxUint64))
	}
	refused("the create after the last pool id", model.ReasonNoIdLeft,
		func() error {
			_, err := createTd("top-1")
			return err
		})
}

// ---------------------------------------------------------------------------
// The transport service id
// ---------------------------------------------------------------------------

// TestTrSvcIdAtTheDiscoveryLogWidth pins the tr_svc_id bound of
// architecture.md, Common validation, through CreateControllerNode, whose
// transport conf is the one the cdc copies into the discovery log (cdc.md
// DS3): a service id of exactly MaxTrSvcIdLength bytes is stored as sent,
// one byte more is INVALID_ARGUMENT and writes nothing.
func TestTrSvcIdAtTheDiscoveryLogWidth(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("svcid")
	cid := mustCluster(t, s, cluster)
	create := func(svcId string) (string, error) {
		addr := fakeAddrPort(t, "cn")
		startFakeAgent(t, addr, 16<<30)
		trConf := testTrConf(addr)
		trConf.TrSvcId = svcId
		_, err := s.CreateControllerNode(ctx, &pb.CreateControllerNodeRequest{
			ClusterName: cluster,
			AddrPort:    addr,
			NvmeTrConf:  trConf,
		})
		return addr, err
	}
	at := strings.Repeat("9", common.MaxTrSvcIdLength)
	addr, err := create(at)
	wantCode(t, err, codes.OK, "a service id at the width")
	cn := &pb.CnConf{}
	hnodeGet(t, s, model.CnConfKey(cid, addr), cn)
	if got := cn.GetNvmeTrConf().GetTrSvcId(); got != at {
		t.Errorf("stored tr_svc_id %q, want %q", got, at)
	}
	globalRev := hnodeModRev(t, s, model.CnGlobalKey(cid))
	addr, err = create(at + "9")
	bndWantMsg(t, err, codes.InvalidArgument, "tr_svc_id",
		"a service id one byte over")
	if hnodeExists(t, s, model.CnConfKey(cid, addr)) {
		t.Errorf("the refusal wrote a record")
	}
	if got := hnodeModRev(t, s, model.CnGlobalKey(cid)); got != globalRev {
		t.Errorf("the refusal moved the global")
	}
}

// ---------------------------------------------------------------------------
// Rules the docs leave open, pinned as the code has them
// ---------------------------------------------------------------------------

// TestAddrPortIsNotParsed pins the addr_port check of architecture.md, Common
// validation, as the gateway has it: the name rule alone. An endpoint that is
// not a host and a port — a unix socket — is accepted, and a host with no
// port passes the check and fails only at the agent call (ABORTED, AG3).
func TestAddrPortIsNotParsed(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	cluster := hnodeName("addr")
	mustCluster(t, s, cluster)
	sock := fakeAddrPort(t, "dn")
	startFakeAgent(t, sock, 16<<30)
	if err := bndDnKind.create(ctx, s, cluster, sock); err != nil {
		t.Fatalf("a unix socket endpoint: %v", err)
	}
	wantCode(t, bndDnKind.create(ctx, s, cluster, "127.0.0.1"),
		codes.Aborted, "a host with no port")
}

// TestThinDeviceSizeNeedNotBeABlockMultiple pins the size rule of
// architecture.md, Thin devices, as the gateway has it: a multiple of the
// slice count times the stripe, here one 128 KiB stripe, is accepted although
// it is not a multiple of the pool's 2 MiB block, and half a stripe is
// INVALID_ARGUMENT.
func TestThinDeviceSizeNeedNotBeABlockMultiple(t *testing.T) {
	env := newVolEnv(t)
	create := func(name string, size uint64) error {
		_, err := env.srv.CreateThinDevice(env.ctx,
			&pb.CreateThinDeviceRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				TdName:      name,
				Size:        size,
			})
		return err
	}
	if err := create("one-stripe", volStripe); err != nil {
		t.Fatalf("a size of one stripe: %v", err)
	}
	if got := env.td("one-stripe").GetSize(); got != volStripe {
		t.Errorf("stored size %d, want %d", got, volStripe)
	}
	wantCode(t, create("half-stripe", volStripe/2), codes.InvalidArgument,
		"half a stripe")
}
