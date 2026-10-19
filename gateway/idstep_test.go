package gateway

import (
	"bytes"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is the id step's tests (gateway.md GW19; architecture.md,
// Clusters): AdvanceClusterIds driven against a real etcd on the sptEnv
// fixture of handler_sp_test.go, and its two counter moves, advanceNextId and
// advanceDevId, on their own. It also holds the top of a pool's thin ids,
// past which CreateThinDevice refuses (architecture.md, Thin devices) and at
// which the step leaves next_dev_id, and the two id minters (gateway.md,
// GW12) on their own at the top of their counters, past which every create
// refuses (architecture.md, Globals: id allocation + shard buckets).
//
// The handler tests assert on the store: the exact set of keys the step
// rewrites, each counter at exactly its old value plus its stride, and every
// other member of the rewritten messages unchanged. A step that also bumped a
// revision, skipped a counter or moved one twice changes one of those.

// advanceIds runs the id step on the fixture's cluster.
func (e *sptEnv) advanceIds() (*pb.AdvanceClusterIdsReply, error) {
	return e.srv.AdvanceClusterIds(e.ctx,
		&pb.AdvanceClusterIdsRequest{ClusterName: e.name})
}

// createTd runs CreateThinDevice for a one-MiB thin device, a whole number of
// stripe units on the pools of sptDefaultSpec and sptSmallSpec, and returns
// its reply.
func (e *sptEnv) createTd(spName string, tdName string) *pb.CreateThinDeviceReply {
	e.t.Helper()
	reply, err := e.srv.CreateThinDevice(e.ctx, &pb.CreateThinDeviceRequest{
		ClusterName: e.name, SpName: spName, TdName: tdName, Size: 1 << 20,
	})
	if err != nil {
		e.t.Fatalf("CreateThinDevice %s/%s: %v", spName, tdName, err)
	}
	return reply
}

// idStepChanged is every key two dumps disagree on: a value that differs, a
// key that appeared and a key that went, sorted.
func idStepChanged(before map[string][]byte, after map[string][]byte) []string {
	var keys []string
	for key, value := range after {
		if old, ok := before[key]; !ok || !bytes.Equal(old, value) {
			keys = append(keys, key)
		}
	}
	for key := range before {
		if _, ok := after[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// setIdStepRanged installs hook as AdvanceClusterIds' seam between its range
// of the pools and its first pool transaction for the rest of the test, and
// puts the no-op back after it. The seam is package state, which is safe
// because no test of this package runs in parallel with another.
func setIdStepRanged(t *testing.T, hook func()) {
	t.Helper()
	saved := idStepRanged
	idStepRanged = hook
	t.Cleanup(func() { idStepRanged = saved })
}

// idStepGlobals reads the three globals of the fixture's cluster.
func (e *sptEnv) idStepGlobals() (*pb.DnGlobal, *pb.CnGlobal, *pb.SpGlobal) {
	dn, cn, sp := &pb.DnGlobal{}, &pb.CnGlobal{}, &pb.SpGlobal{}
	e.get(model.DnGlobalKey(e.cid), dn)
	e.get(model.CnGlobalKey(e.cid), cn)
	e.get(model.SpGlobalKey(e.cid), sp)
	return dn, cn, sp
}

// TestAdvanceClusterIdsMovesEveryCounter is the step on a working cluster: two
// pools, one of them holding a thin device. Exactly five keys change, the
// three globals and the two SpConfs; each counter moves by exactly its
// stride; and every other member of the five messages is what it was. No
// revision key moves (GW19).
func TestAdvanceClusterIdsMovesEveryCounter(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	env.createSp(sptDefaultSpec("pool-a"))
	env.createSp(sptSmallSpec("pool-b"))
	env.createTd("pool-a", "t0")
	dnG, cnG, spG := env.idStepGlobals()
	confs := map[string]*pb.SpConf{
		"pool-a": env.spConf("pool-a"),
		"pool-b": env.spConf("pool-b"),
	}
	before := env.dumpAll()

	reply, err := env.advanceIds()
	if err != nil {
		t.Fatalf("AdvanceClusterIds: %v", err)
	}
	if reply.GetClusterId() != env.cid || reply.GetSpCnt() != 2 {
		t.Errorf("reply %v, want cluster_id %d and sp_cnt 2", reply, env.cid)
	}
	want := []string{
		model.DnGlobalKey(env.cid),
		model.CnGlobalKey(env.cid),
		model.SpGlobalKey(env.cid),
		model.SpConfKey(env.cid, "pool-a"),
		model.SpConfKey(env.cid, "pool-b"),
	}
	sort.Strings(want)
	if got := idStepChanged(before, env.dumpAll()); !slices.Equal(got, want) {
		t.Errorf("the step changed %q, want exactly %q", got, want)
	}

	stride := uint64(common.IdStepStride)
	gotDn, gotCn, gotSp := env.idStepGlobals()
	if gotDn.GetNextId() != dnG.GetNextId()+stride {
		t.Errorf("dn_global next_id %d, want %d",
			gotDn.GetNextId(), dnG.GetNextId()+stride)
	}
	if gotCn.GetNextId() != cnG.GetNextId()+stride {
		t.Errorf("cn_global next_id %d, want %d",
			gotCn.GetNextId(), cnG.GetNextId()+stride)
	}
	if gotSp.GetNextId() != spG.GetNextId()+stride {
		t.Errorf("sp_global next_id %d, want %d",
			gotSp.GetNextId(), spG.GetNextId()+stride)
	}
	gotDn.NextId, gotCn.NextId, gotSp.NextId =
		dnG.GetNextId(), cnG.GetNextId(), spG.GetNextId()
	if !proto.Equal(gotDn, dnG) || !proto.Equal(gotCn, cnG) ||
		!proto.Equal(gotSp, spG) {
		t.Errorf("a global changed beyond its next_id")
	}
	for name, old := range confs {
		got := env.spConf(name)
		if got.GetNextId() != old.GetNextId()+stride {
			t.Errorf("%s next_id %d, want %d",
				name, got.GetNextId(), old.GetNextId()+stride)
		}
		if got.GetNextDevId() != old.GetNextDevId()+common.ThinIdStepStride {
			t.Errorf("%s next_dev_id %d, want %d", name, got.GetNextDevId(),
				old.GetNextDevId()+common.ThinIdStepStride)
		}
		got.NextId, got.NextDevId = old.GetNextId(), old.GetNextDevId()
		if !proto.Equal(got, old) {
			t.Errorf("%s changed beyond its two counters", name)
		}
	}
}

// TestAdvanceClusterIdsNewIdsLandAboveEveryOld is what the step is for: after
// it, a create draws its ids from the moved counters, exactly the old value
// plus the stride. The five creates draw from all five counters, through
// mintClusterId for the three globals (CreateDiskNode, CreateControllerNode,
// CreateStoragePool), the gateway's per-SP minter and nextDevId
// (CreateThinDevice), and model.TakeSpIds in a model op (GrowSlice).
func TestAdvanceClusterIdsNewIdsLandAboveEveryOld(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	env.createSp(sptSmallSpec("pool-a"))
	env.createSp(sptSmallSpec("pool-b"))
	dnG, cnG, spG := env.idStepGlobals()
	confA, confB := env.spConf("pool-a"), env.spConf("pool-b")
	if _, err := env.advanceIds(); err != nil {
		t.Fatalf("AdvanceClusterIds: %v", err)
	}
	stride := uint64(common.IdStepStride)

	td := env.createTd("pool-a", "t0")
	if td.GetTdId() != confA.GetNextId()+stride {
		t.Errorf("td_id %d, want %d", td.GetTdId(), confA.GetNextId()+stride)
	}
	if td.GetDevId() != confA.GetNextDevId()+common.ThinIdStepStride {
		t.Errorf("dev_id %d, want %d", td.GetDevId(),
			confA.GetNextDevId()+common.ThinIdStepStride)
	}
	grow, err := env.srv.GrowSlice(env.ctx, &pb.GrowSliceRequest{
		ClusterName: env.name,
		SpName:      "pool-b",
		SliceId:     confB.GetSliceIdList()[0],
		ExtCnt:      1,
	})
	if err != nil {
		t.Fatalf("GrowSlice: %v", err)
	}
	if grow.GetGrpId() != confB.GetNextId()+stride {
		t.Errorf("grp_id %d, want %d", grow.GetGrpId(),
			confB.GetNextId()+stride)
	}
	if spId := env.createSp(sptSmallSpec("pool-c")); spId !=
		spG.GetNextId()+stride {
		t.Errorf("sp_id %d, want %d", spId, spG.GetNextId()+stride)
	}
	dnId := mustDn(t, env.srv, env.name, fakeAddrPort(t, "dn"), "", 64<<30)
	if dnId != dnG.GetNextId()+stride {
		t.Errorf("dn_id %d, want %d", dnId, dnG.GetNextId()+stride)
	}
	cnId := mustCn(t, env.srv, env.name, fakeAddrPort(t, "cn"), "", 64<<30)
	if cnId != cnG.GetNextId()+stride {
		t.Errorf("cn_id %d, want %d", cnId, cnG.GetNextId()+stride)
	}
}

// TestAdvanceClusterIdsTakesOneTransactionPerPool steps one pool more than a
// single transaction could carry: a transaction compares every key it read
// and every key it wrote, so one over these pools would hold two compares
// per pool, past EtcdMaxTxnOps, and etcd would refuse it. Every pool moves,
// and the reply counts them all.
func TestAdvanceClusterIdsTakesOneTransactionPerPool(t *testing.T) {
	env := sptNewEnv(t, 0, 0, 0)
	n := common.EtcdMaxTxnOps/2 + 1
	for idx := 0; idx < n; idx++ {
		mustPut(t, env.cli,
			model.SpConfKey(env.cid, fmt.Sprintf("p%04d", idx)),
			&pb.SpConf{SpId: uint64(idx) + 1, NextId: 7, NextDevId: 3})
	}
	reply, err := env.advanceIds()
	if err != nil {
		t.Fatalf("AdvanceClusterIds over %d pools: %v", n, err)
	}
	if int(reply.GetSpCnt()) != n {
		t.Errorf("sp_cnt %d, want %d", reply.GetSpCnt(), n)
	}
	kvs, _, err := env.cli.Range(env.ctx, model.SpConfPrefix(env.cid))
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(kvs) != n {
		t.Fatalf("%d SpConfs after the step, want %d", len(kvs), n)
	}
	for _, kv := range kvs {
		conf := &pb.SpConf{}
		if err := proto.Unmarshal(kv.Value, conf); err != nil {
			t.Fatalf("%s: %v", kv.Key, err)
		}
		if conf.GetNextId() != 7+uint64(common.IdStepStride) ||
			conf.GetNextDevId() != 3+common.ThinIdStepStride {
			t.Errorf("%s holds next_id %d and next_dev_id %d after the step",
				kv.Key, conf.GetNextId(), conf.GetNextDevId())
		}
	}
}

// TestAdvanceClusterIdsAcrossPoolsThatMoveUnderIt deletes one pool and creates
// another between the step's range and its pool transactions, through the
// seam. The deleted pool is skipped: nothing is put back under its key and
// sp_cnt does not count it. The created pool draws an sp_id above every old
// one, because the globals moved first, and it is not stepped, since the
// range never listed it; the pool that stayed moves.
func TestAdvanceClusterIdsAcrossPoolsThatMoveUnderIt(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	env.createSp(sptSmallSpec("pool-a"))
	env.createSp(sptSmallSpec("pool-b"))
	spG := env.spGlobal()
	confB := env.spConf("pool-b")
	var freshId uint64
	var fresh *pb.SpConf
	hookRuns := 0
	setIdStepRanged(t, func() {
		hookRuns++
		err := env.cli.Delete(env.ctx, model.SpConfKey(env.cid, "pool-a"))
		if err != nil {
			t.Fatalf("Delete: %v", err)
		}
		freshId = env.createSp(sptSmallSpec("pool-c"))
		fresh = env.spConf("pool-c")
	})

	reply, err := env.advanceIds()
	if err != nil {
		t.Fatalf("AdvanceClusterIds: %v", err)
	}
	if hookRuns != 1 {
		t.Fatalf("the seam ran %d times, want 1", hookRuns)
	}
	if reply.GetSpCnt() != 1 {
		t.Errorf("sp_cnt %d, want 1: pool-b alone", reply.GetSpCnt())
	}
	if env.exists(model.SpConfKey(env.cid, "pool-a")) {
		t.Errorf("the step put back pool-a, deleted under it")
	}
	if freshId != spG.GetNextId()+uint64(common.IdStepStride) {
		t.Errorf("pool-c sp_id %d, want %d", freshId,
			spG.GetNextId()+uint64(common.IdStepStride))
	}
	if got := env.spConf("pool-c"); !proto.Equal(got, fresh) {
		t.Errorf("pool-c, created after the range, was stepped: "+
			"next_id %d -> %d, next_dev_id %d -> %d",
			fresh.GetNextId(), got.GetNextId(),
			fresh.GetNextDevId(), got.GetNextDevId())
	}
	if got := env.spConf("pool-b").GetNextId(); got !=
		confB.GetNextId()+uint64(common.IdStepStride) {
		t.Errorf("pool-b next_id %d, want %d", got,
			confB.GetNextId()+uint64(common.IdStepStride))
	}
}

// TestAdvanceClusterIdsRefusalsWriteNothing brackets each refusal that comes
// before the globals' transaction commits with a dump of the cluster: a name
// the validation refuses, a cluster that does not exist, each global missing,
// and each global one past the last value it can move by its stride without
// wrapping.
func TestAdvanceClusterIdsRefusalsWriteNothing(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	env.createSp(sptSmallSpec("pool-a"))
	refuse := func(label string, name string, want codes.Code) {
		t.Helper()
		before := env.dumpAll()
		_, err := env.srv.AdvanceClusterIds(env.ctx,
			&pb.AdvanceClusterIdsRequest{ClusterName: name})
		wantCode(t, err, want, label)
		if changed := idStepChanged(before, env.dumpAll()); len(changed) != 0 {
			t.Errorf("%s wrote %q", label, changed)
		}
	}
	refuse("an invalid name", "bad name", codes.InvalidArgument)
	refuse("an unknown cluster", env.name+"-none", codes.NotFound)

	dnG, cnG, spG := env.idStepGlobals()
	globals := []struct {
		kind string
		key  string
		msg  proto.Message
		top  proto.Message
	}{
		{"dn_global", model.DnGlobalKey(env.cid), dnG, &pb.DnGlobal{
			NextId:      math.MaxUint64 - common.IdStepStride + 1,
			ShardBucket: dnG.GetShardBucket(),
		}},
		{"cn_global", model.CnGlobalKey(env.cid), cnG, &pb.CnGlobal{
			NextId:      math.MaxUint64 - common.IdStepStride + 1,
			ShardBucket: cnG.GetShardBucket(),
		}},
		{"sp_global", model.SpGlobalKey(env.cid), spG, &pb.SpGlobal{
			NextId:      math.MaxUint64 - common.IdStepStride + 1,
			ShardBucket: spG.GetShardBucket(),
		}},
	}
	for _, global := range globals {
		if err := env.cli.Delete(env.ctx, global.key); err != nil {
			t.Fatalf("Delete %s: %v", global.key, err)
		}
		refuse("a missing "+global.kind, env.name, codes.Aborted)
		mustPut(t, env.cli, global.key, global.top)
		refuse(global.kind+" at its top", env.name, codes.FailedPrecondition)
		mustPut(t, env.cli, global.key, global.msg)
	}
}

// TestAdvanceClusterIdsEndsAtAPoolThatCannotMove: a pool whose next_id cannot
// move by the stride without wrapping fails the step there. The globals'
// transaction and the pool before it have committed, the refused pool's
// transaction puts nothing, and the pool after it is never reached; the
// message names the pool.
func TestAdvanceClusterIdsEndsAtAPoolThatCannotMove(t *testing.T) {
	env := sptNewEnv(t, 0, 0, 0)
	for name, next := range map[string]uint64{
		"p0": 7,
		"p1": math.MaxUint64 - common.IdStepStride + 1,
		"p2": 7,
	} {
		mustPut(t, env.cli, model.SpConfKey(env.cid, name),
			&pb.SpConf{SpId: 1, NextId: next, NextDevId: 3})
	}
	before := env.dumpAll()
	_, err := env.advanceIds()
	wantCode(t, err, codes.FailedPrecondition, "a pool at its top")
	if msg := status.Convert(err).Message(); !strings.Contains(msg, `"p1"`) {
		t.Errorf("the refusal %q does not name the pool", msg)
	}
	want := []string{
		model.DnGlobalKey(env.cid),
		model.CnGlobalKey(env.cid),
		model.SpGlobalKey(env.cid),
		model.SpConfKey(env.cid, "p0"),
	}
	sort.Strings(want)
	if got := idStepChanged(before, env.dumpAll()); !slices.Equal(got, want) {
		t.Errorf("the step changed %q, want exactly %q", got, want)
	}
}

// TestAdvanceClusterIdsStartsZeroCountersAtTheirFirstValue: a counter that
// holds the proto3 zero has handed out nothing, and moves from its first
// value, 1, like one that holds 1. Reading the zero as itself would step it
// to the stride alone.
func TestAdvanceClusterIdsStartsZeroCountersAtTheirFirstValue(t *testing.T) {
	env := sptNewEnv(t, 0, 0, 0)
	mustPut(t, env.cli, model.DnGlobalKey(env.cid),
		&pb.DnGlobal{ShardBucket: zeroBucket()})
	mustPut(t, env.cli, model.CnGlobalKey(env.cid),
		&pb.CnGlobal{ShardBucket: zeroBucket()})
	mustPut(t, env.cli, model.SpGlobalKey(env.cid),
		&pb.SpGlobal{ShardBucket: zeroBucket()})
	mustPut(t, env.cli, model.SpConfKey(env.cid, "p0"), &pb.SpConf{SpId: 1})
	if _, err := env.advanceIds(); err != nil {
		t.Fatalf("AdvanceClusterIds: %v", err)
	}
	first := 1 + uint64(common.IdStepStride)
	dnG, cnG, spG := env.idStepGlobals()
	for kind, got := range map[string]uint64{
		"dn_global": dnG.GetNextId(),
		"cn_global": cnG.GetNextId(),
		"sp_global": spG.GetNextId(),
		"p0":        env.spConf("p0").GetNextId(),
	} {
		if got != first {
			t.Errorf("%s next_id %d, want %d", kind, got, first)
		}
	}
	if got := env.spConf("p0").GetNextDevId(); got !=
		1+common.ThinIdStepStride {
		t.Errorf("p0 next_dev_id %d, want %d", got, 1+common.ThinIdStepStride)
	}
}

// TestAdvanceIdsStayInTheirRanges pins the two counter moves at their edges.
// A thin id moves by its stride until the stride would take it past the
// first id above MaxThinDevId, then lands there, and never moves backward,
// even from a value beyond it. A uint64 counter moves by its stride up to the
// last value that does not wrap, and past it is refused FAILED_PRECONDITION.
func TestAdvanceIdsStayInTheirRanges(t *testing.T) {
	const top = uint32(common.MaxThinDevId + 1)
	for _, tc := range []struct{ in, want uint32 }{
		{0, 1 + common.ThinIdStepStride},
		{1, 1 + common.ThinIdStepStride},
		{top - common.ThinIdStepStride - 1, top - 1},
		{top - common.ThinIdStepStride, top},
		{top - 5, top},
		{top, top},
		{top + 100, top + 100},
		{math.MaxUint32, math.MaxUint32},
	} {
		if got := advanceDevId(tc.in); got != tc.want {
			t.Errorf("advanceDevId(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
	stride := uint64(common.IdStepStride)
	for _, tc := range []struct{ in, want uint64 }{
		{0, 1 + stride},
		{1, 1 + stride},
		{math.MaxUint64 - stride, math.MaxUint64},
	} {
		got, err := advanceNextId(tc.in, "x")
		if err != nil || got != tc.want {
			t.Errorf("advanceNextId(%d) = %d, %v; want %d",
				tc.in, got, err, tc.want)
		}
	}
	for _, in := range []uint64{math.MaxUint64 - stride + 1, math.MaxUint64} {
		got, err := advanceNextId(in, "x")
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("advanceNextId(%d) = %d, %v; want FAILED_PRECONDITION",
				in, got, err)
		}
	}
}

// TestCreateThinDeviceRefusesPastTheThinIdRange: a pool hands out thin ids up
// to MaxThinDevId, the largest dm-thin takes, and refuses the create after
// that one FAILED_PRECONDITION, naming the pool and writing nothing
// (architecture.md, Thin devices). An id step at the top leaves next_dev_id
// at the first id above MaxThinDevId, so the pool stays refused.
func TestCreateThinDeviceRefusesPastTheThinIdRange(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	env.createSp(sptSmallSpec("pool-a"))
	conf := env.spConf("pool-a")
	conf.NextDevId = common.MaxThinDevId
	mustPut(t, env.cli, model.SpConfKey(env.cid, "pool-a"), conf)

	if got := env.createTd("pool-a", "t0").GetDevId(); got !=
		common.MaxThinDevId {
		t.Errorf("t0 dev_id %d, want MaxThinDevId %d", got,
			common.MaxThinDevId)
	}
	const top = uint32(common.MaxThinDevId + 1)
	if got := env.spConf("pool-a").GetNextDevId(); got != top {
		t.Fatalf("next_dev_id %d after the last thin id, want %d", got, top)
	}

	refuse := func(label string) {
		t.Helper()
		before := env.dumpAll()
		_, err := env.srv.CreateThinDevice(env.ctx,
			&pb.CreateThinDeviceRequest{
				ClusterName: env.name, SpName: "pool-a", TdName: "t1",
				Size: 1 << 20,
			})
		wantCode(t, err, codes.FailedPrecondition, label)
		msg := status.Convert(err).Message()
		if !strings.Contains(msg, `"pool-a"`) {
			t.Errorf("%s: the refusal %q does not name the pool", label, msg)
		}
		if changed := idStepChanged(before, env.dumpAll()); len(changed) != 0 {
			t.Errorf("%s wrote %q", label, changed)
		}
	}
	refuse("a create past the top")

	if _, err := env.advanceIds(); err != nil {
		t.Fatalf("AdvanceClusterIds: %v", err)
	}
	if got := env.spConf("pool-a").GetNextDevId(); got != top {
		t.Errorf("the step moved next_dev_id from the top to %d, want %d",
			got, top)
	}
	refuse("a create past the top after an id step")
}

// TestMintClusterIdAtTheTop is the cluster minter (gateway.md, GW12) at its
// top (architecture.md, Globals: id allocation + shard buckets): one below the
// largest 64-bit value it mints that id and moves next_id onto the largest
// value; at the largest value it is FAILED_PRECONDITION, after the count
// gate, whose RESOURCE_EXHAUSTED a full cluster still hears first.
func TestMintClusterIdAtTheTop(t *testing.T) {
	const top = uint64(math.MaxUint64)
	bucket := zeroBucket()
	got, err := mintClusterId(top-1, bucket, 4, "disk node")
	if err != nil {
		t.Fatalf("the last id: %v", err)
	}
	if got.Id != top-1 || got.NextId != top {
		t.Errorf("the last id: id %d and next_id %d, want %d and %d",
			got.Id, got.NextId, top-1, top)
	}
	_, err = mintClusterId(top, bucket, 4, "disk node")
	bndWantMsg(t, err, codes.FailedPrecondition, "no disk node id left",
		"no id left")
	full := zeroBucket()
	full[0] = 4
	_, err = mintClusterId(top, full, 4, "disk node")
	wantCode(t, err, codes.ResourceExhausted, "a full cluster with no id left")
}

// TestSpIdMinterAtTheTop is the per-SP minter (gateway.md, GW12) at its top:
// a run that ends one below the largest 64-bit value commits and leaves
// next_id on the largest value; a run one id longer, or any run from the
// largest value, is FAILED_PRECONDITION and leaves the SpConf's next_id as it
// was.
func TestSpIdMinterAtTheTop(t *testing.T) {
	const top = uint64(math.MaxUint64)
	for _, tc := range []struct {
		name   string
		nextId uint64
		n      int
		ok     bool
	}{
		{name: "the last two ids", nextId: top - 2, n: 2, ok: true},
		{name: "two ids with one left", nextId: top - 1, n: 2},
		{name: "one id with none left", nextId: top, n: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := &pb.SpConf{NextId: tc.nextId}
			minter := newSpIdMinter(conf)
			for idx := 0; idx < tc.n; idx++ {
				if id := minter.mint(); tc.ok && id != tc.nextId+uint64(idx) {
					t.Errorf("id %d: got %d, want %d", idx, id,
						tc.nextId+uint64(idx))
				}
			}
			err := minter.commit(conf, "TestOp")
			if !tc.ok {
				bndWantMsg(t, err, codes.FailedPrecondition,
					model.ReasonNoIdLeft, "the commit")
				if conf.GetNextId() != tc.nextId {
					t.Errorf("the refused commit moved next_id from %d to %d",
						tc.nextId, conf.GetNextId())
				}
				return
			}
			if err != nil {
				t.Fatalf("commit: %v", err)
			}
			if conf.GetNextId() != top {
				t.Errorf("next_id %d, want %d", conf.GetNextId(), top)
			}
		})
	}
}
