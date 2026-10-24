// The etcd-backed tests of the library (dnv-worker.md EU7): each plants into
// the package's real etcd and reads what it planted back through model, as
// the worker reads it — a small full cluster, every sub-object, every
// setter, both drains, the node lists at the per-cluster ceilings, and the
// widest pool.
package plant

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// testCtx bounds one etcd-backed case.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// mustGet reads one key into msg, failing the test when it is absent.
func mustGet(
	t *testing.T,
	ctx context.Context,
	cli *etcdutil.Client,
	key string,
	msg proto.Message,
) {
	t.Helper()
	found, err := cli.Get(ctx, key, msg)
	if err != nil {
		t.Fatalf("Get %q: %v", key, err)
	}
	if !found {
		t.Fatalf("key %q is absent", key)
	}
}

// mustAbsent fails the test when a key exists.
func mustAbsent(
	t *testing.T,
	ctx context.Context,
	cli *etcdutil.Client,
	key string,
	msg proto.Message,
) {
	t.Helper()
	found, err := cli.Get(ctx, key, msg)
	if err != nil {
		t.Fatalf("Get %q: %v", key, err)
	}
	if found {
		t.Fatalf("key %q exists, want it absent", key)
	}
}

// keyCount counts the keys under a prefix.
func keyCount(
	t *testing.T,
	ctx context.Context,
	cli *etcdutil.Client,
	prefix string,
) int {
	t.Helper()
	keys, _, err := cli.RangeKeys(ctx, prefix)
	if err != nil {
		t.Fatalf("RangeKeys %q: %v", prefix, err)
	}
	return len(keys)
}

const (
	// nodeFree is every planted node's free and total extent count.
	nodeFree = uint64(64)
	// smokeShard is the shard code of the small pool's nodes and pool.
	smokeShard = uint32(0x55)
)

// dnAddr and cnAddr are the endpoints of the small cluster's nodes.
func dnAddr(id uint64) string { return fmt.Sprintf("10.0.0.1:%d", 29600+id) }
func cnAddr(id uint64) string { return fmt.Sprintf("10.0.0.1:%d", 29700+id) }

// smallCluster is one planted cluster of five DNs and three CNs, each in a
// location of its own, with the smoke pool on it: smokeSpec on smokeShard.
// dn 3 and dn 4 hold nothing; dn 5 holds nothing and is the one a case may
// move.
type smallCluster struct {
	cli *etcdutil.Client
	cl  Cluster
	sp  Sp
	dns map[uint64]Dn
	cns map[uint64]Cn
}

// smallSpec is the small cluster's pool.
func smallSpec() SpSpec {
	spec := smokeSpec()
	spec.Shard = smokeShard
	return spec
}

// plantSmallCluster plants the cluster, its nodes, and, with pool, its pool.
func plantSmallCluster(
	t *testing.T,
	ctx context.Context,
	pool bool,
) *smallCluster {
	t.Helper()
	cli := newTestClient(t)
	cl, err := PutCluster(ctx, cli, ClusterSpec{
		Name:       testClusterName(t),
		ExtentSize: common.MinDnExtSize,
	})
	if err != nil {
		t.Fatalf("PutCluster: %v", err)
	}
	out := &smallCluster{
		cli: cli,
		cl:  cl,
		dns: make(map[uint64]Dn),
		cns: make(map[uint64]Cn),
	}
	var dnSpecs []DnSpec
	for id := uint64(1); id <= 5; id++ {
		dnSpecs = append(dnSpecs, DnSpec{
			Id: id, Shard: smokeShard, Addr: dnAddr(id),
			Location: fmt.Sprintf("dn-rack%d", id), FreeExt: nodeFree,
		})
	}
	dns, err := PutDns(ctx, cli, cl, dnSpecs)
	if err != nil {
		t.Fatalf("PutDns: %v", err)
	}
	for _, dn := range dns {
		out.dns[dn.Conf.GetDnId()] = dn
	}
	for id := uint64(1); id <= 3; id++ {
		cn, err := PutCn(ctx, cli, cl, CnSpec{
			Id: id, Shard: smokeShard, Addr: cnAddr(id),
			Location: fmt.Sprintf("cn-rack%d", id), FreeExt: nodeFree,
		})
		if err != nil {
			t.Fatalf("PutCn %d: %v", id, err)
		}
		out.cns[id] = cn
	}
	if pool {
		out.sp, err = PutSp(ctx, cli, cl, smallSpec())
		if err != nil {
			t.Fatalf("PutSp: %v", err)
		}
	}
	return out
}

// load reads the small pool through model.LoadSp.
func (c *smallCluster) load(t *testing.T, ctx context.Context) *model.SpState {
	t.Helper()
	state, err := model.LoadSp(ctx, c.cli, c.cl.Id, c.sp.Ref.Name)
	if err != nil {
		t.Fatalf("LoadSp: %v", err)
	}
	if len(state.Missing) != 0 {
		t.Fatalf("LoadSp found keys missing: %v", state.Missing)
	}
	return state
}

// dnConf reads one DN's record; cnConf one CN's.
func (c *smallCluster) dnConf(
	t *testing.T, ctx context.Context, id uint64,
) *pb.DnConf {
	t.Helper()
	dn := &pb.DnConf{}
	mustGet(t, ctx, c.cli, model.DnConfKey(c.cl.Id, dnAddr(id)), dn)
	return dn
}

func (c *smallCluster) cnConf(
	t *testing.T, ctx context.Context, id uint64,
) *pb.CnConf {
	t.Helper()
	cn := &pb.CnConf{}
	mustGet(t, ctx, c.cli, model.CnConfKey(c.cl.Id, cnAddr(id)), cn)
	return cn
}

// dnRev reads one revision; cnRev and spRev likewise.
func (c *smallCluster) dnRev(
	t *testing.T, ctx context.Context, id uint64,
) uint64 {
	t.Helper()
	rev := &pb.DnRev{}
	mustGet(t, ctx, c.cli, model.DnRevKey(smokeShard, c.cl.Id, id), rev)
	return rev.GetRevision()
}

func (c *smallCluster) cnRev(
	t *testing.T, ctx context.Context, id uint64,
) uint64 {
	t.Helper()
	rev := &pb.CnRev{}
	mustGet(t, ctx, c.cli, model.CnRevKey(smokeShard, c.cl.Id, id), rev)
	return rev.GetRevision()
}

func (c *smallCluster) spRev(t *testing.T, ctx context.Context) uint64 {
	t.Helper()
	rev, err := SpRevision(ctx, c.cli, c.cl, c.sp.Ref)
	if err != nil {
		t.Fatalf("SpRevision: %v", err)
	}
	return rev
}

// dnCapacityKey is the capacity key a DN's record implies.
func (c *smallCluster) dnCapacityKey(
	t *testing.T, id uint64, freeExt uint64,
) string {
	t.Helper()
	binIdx, ok := model.DnBinIdx(freeExt, c.cl.Conf.GetDnBinConf())
	if !ok {
		t.Fatalf("%d free extents have no bin", freeExt)
	}
	return model.DnCapacityKey(c.cl.Id, binIdx, freeExt, dnAddr(id))
}

// dnHolds fails the case unless dn id holds free extents and exactly the
// side pointers ptrs, in order, carries the capacity key of that free count
// and not the one of gone, the count it held before the write under test,
// and is at revision rev.
func (c *smallCluster) dnHolds(
	t *testing.T,
	ctx context.Context,
	id uint64,
	free uint64,
	gone uint64,
	ptrs []*pb.SidePointer,
	rev uint64,
) {
	t.Helper()
	dn := c.dnConf(t, ctx, id)
	if dn.GetFreeExtCnt() != free || !equalPtrs(dn.GetSidePtrList(), ptrs) {
		t.Errorf("dn %d holds %d free extents and sides %v, want %d and %v",
			id, dn.GetFreeExtCnt(), dn.GetSidePtrList(), free, ptrs)
	}
	mustGet(t, ctx, c.cli, c.dnCapacityKey(t, id, free), &pb.DnCapacity{})
	if gone != free {
		mustAbsent(t, ctx, c.cli, c.dnCapacityKey(t, id, gone),
			&pb.DnCapacity{})
	}
	if got := c.dnRev(t, ctx, id); got != rev {
		t.Errorf("dn %d is at revision %d, want %d", id, got, rev)
	}
}

// TestPutClusterReadsBack pins PutCluster: the conf it stores is the
// resolved one LoadCluster reads back under the same cluster_id, with the
// three globals beside it, and a second PutCluster of the name is refused.
func TestPutClusterReadsBack(t *testing.T) {
	ctx := testCtx(t)
	cli := newTestClient(t)
	name := testClusterName(t)
	cl, err := PutCluster(ctx, cli, ClusterSpec{Name: name, CntlrInterval: 3})
	if err != nil {
		t.Fatalf("PutCluster: %v", err)
	}
	if cl.Id != model.ClusterId(name, cl.Conf.GetCreationEpoch()) {
		t.Fatalf("cluster_id %#x is not derived from the name and epoch",
			cl.Id)
	}
	if err := model.ValidateClusterConf(cl.Conf); err != nil {
		t.Fatalf("the stored conf is not concrete: %v", err)
	}
	if got := cl.Conf.GetDnBinConf().GetExtentSize(); got !=
		common.DefaultDnExtSize {
		t.Fatalf("a zero extent size stored %d, want the default %d", got,
			common.DefaultDnExtSize)
	}
	if got := cl.Conf.GetHealthCheckConf().GetCntlrInterval(); got != 3 {
		t.Fatalf("cntlr_interval %d, want 3", got)
	}
	loaded, err := LoadCluster(ctx, cli, name)
	if err != nil {
		t.Fatalf("LoadCluster: %v", err)
	}
	if loaded.Id != cl.Id || !proto.Equal(loaded.Conf, cl.Conf) {
		t.Fatalf("LoadCluster = %#x %v, PutCluster wrote %#x %v",
			loaded.Id, loaded.Conf, cl.Id, cl.Conf)
	}
	for _, global := range []struct {
		key string
		msg interface {
			proto.Message
			GetNextId() uint64
			GetShardBucket() []uint32
		}
	}{
		{model.DnGlobalKey(cl.Id), &pb.DnGlobal{}},
		{model.CnGlobalKey(cl.Id), &pb.CnGlobal{}},
		{model.SpGlobalKey(cl.Id), &pb.SpGlobal{}},
	} {
		mustGet(t, ctx, cli, global.key, global.msg)
		if global.msg.GetNextId() != 1 ||
			len(global.msg.GetShardBucket()) != common.ShardBucketSize {
			t.Errorf("%s = %v, want next_id 1 and %d empty buckets",
				global.key, global.msg, common.ShardBucketSize)
		}
	}
	if _, err := PutCluster(ctx, cli, ClusterSpec{Name: name}); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a second PutCluster = %v, want an already-exists refusal",
			err)
	}
	if _, err := LoadCluster(ctx, cli, name+"-none"); err == nil {
		t.Fatalf("LoadCluster of a name never planted succeeded")
	}
}

// TestPutSpReadsBack pins PutSp's transaction as model reads it: the pool's
// records, its slice tree, the geometry
// model.GroupBlocks gives, and the bookkeeping of every node it names — the
// extents and pointers, the capacity key, and one revision bump — while a
// node it does not name is left alone.
func TestPutSpReadsBack(t *testing.T) {
	ctx := testCtx(t)
	c := plantSmallCluster(t, ctx, true)
	state := c.load(t, ctx)
	cid := c.cl.Id

	conf := state.Conf
	if conf.GetSpId() != 1 || conf.GetShardCode() != smokeShard ||
		conf.GetNextId() != 12 || conf.GetNextDevId() != 1 ||
		conf.GetDeleting() || conf.GetSpLevel() != 0 {
		t.Fatalf("SpConf = %v", conf)
	}
	if !equalU64(conf.GetCntlrIdList(), 1, 2) ||
		!equalU64(conf.GetSliceIdList(), 1) {
		t.Fatalf("id lists: cntlrs %v, slices %v", conf.GetCntlrIdList(),
			conf.GetSliceIdList())
	}
	if got := conf.GetCntlidSlotList(); len(got) != 3 || got[1] != 1 {
		t.Fatalf("cntlid_slot_list %v, want 0,1,2", got)
	}
	if conf.GetEventThreshold() == nil ||
		!proto.Equal(conf.GetEventThreshold(), &pb.EventThreshold{}) {
		t.Fatalf("event_threshold %v, want the all-zero one",
			conf.GetEventThreshold())
	}
	bdev := conf.GetBdevConf()
	if err := model.ValidateBdevConf(bdev); err != nil {
		t.Fatalf("the pool's bdev_conf is not concrete: %v", err)
	}
	if bdev.GetRedundConf().GetRedundMdRaid1().GetBitmapChunkBlockCnt() !=
		common.DefaultChunkBlockCnt {
		t.Fatalf("redund_conf %v, want md-raid1 with the cluster's chunk",
			bdev.GetRedundConf())
	}
	if state.SpRevision != 1 || c.sp.Revision != 1 {
		t.Fatalf("SpRev %d, returned %d; want 1", state.SpRevision,
			c.sp.Revision)
	}
	name := &pb.SpName{}
	mustGet(t, ctx, c.cli, model.SpNameKey(cid, 1), name)
	if name.GetSpName() != "sp0" {
		t.Fatalf("sp_id_to_name %q, want sp0", name.GetSpName())
	}
	global := &pb.SpGlobal{}
	mustGet(t, ctx, c.cli, model.SpGlobalKey(cid), global)
	if global.GetNextId() != 2 || global.GetShardBucket()[smokeShard] != 1 {
		t.Fatalf("SpGlobal = %v, want next_id 2 and one pool in shard %#x",
			global, smokeShard)
	}

	// The cntlrs: the primary is created settling.
	for cntlrId, want := range map[uint64]struct {
		cn      uint64
		slot    uint32
		primary bool
	}{1: {1, 0, true}, 2: {2, 1, false}} {
		cntlr := state.Cntlrs[cntlrId]
		if cntlr.GetAddrPort() != cnAddr(want.cn) ||
			cntlr.GetCntlidSlot() != want.slot ||
			cntlr.GetPrimary() != want.primary ||
			cntlr.GetSettling() != want.primary || cntlr.GetDisabled() ||
			!proto.Equal(cntlr.GetNvmeTrConf(),
				c.cns[want.cn].Conf.GetNvmeTrConf()) {
			t.Errorf("cntlr %d = %v", cntlrId, cntlr)
		}
	}

	// The slice tree, joined on the spec's ids.
	slice := state.Slices[1]
	if slice.GetSliceIdx() != 0 || len(slice.GetMetaGrpList()) != 1 ||
		len(slice.GetDataGrpList()) != 1 {
		t.Fatalf("slice = %v", slice)
	}
	extentSize := c.cl.Conf.GetDnBinConf().GetExtentSize()
	checkGroup := func(grp *pb.Group, id, extCnt uint64,
		legs map[uint64]uint64) {
		t.Helper()
		metaBlocks, dataBlocks, err := model.GroupBlocks(
			extCnt, extentSize, bdev)
		if err != nil {
			t.Fatalf("GroupBlocks: %v", err)
		}
		if grp.GetGrpId() != id || grp.GetExtCnt() != extCnt ||
			grp.GetMetaBlocks() != metaBlocks ||
			grp.GetDataBlocks() != dataBlocks {
			t.Errorf("group %#x = %v, want ext_cnt %d, %d+%d blocks",
				id, grp, extCnt, metaBlocks, dataBlocks)
		}
		list := grp.GetLegList()
		if len(list) != len(legs) {
			t.Errorf("group %#x has %d legs, want %d", id, len(list),
				len(legs))
		}
		for _, leg := range list {
			dnId, ok := legs[leg.GetLegId()]
			if !ok || len(leg.GetSideList()) != 1 {
				t.Errorf("group %#x leg %v", id, leg)
				continue
			}
			side := leg.GetSideList()[0]
			if side.GetAddrPort() != dnAddr(dnId) ||
				side.GetProvisioned() || side.GetErrEpoch() != 0 ||
				!proto.Equal(side.GetNvmeTrConf(),
					c.dns[dnId].Conf.GetNvmeTrConf()) {
				t.Errorf("group %#x side %v, want unprovisioned on dn %d",
					id, side, dnId)
			}
		}
	}
	checkGroup(slice.GetMetaGrpList()[0], 2, 1,
		map[uint64]uint64{3: 1, 4: 2})
	checkGroup(slice.GetDataGrpList()[0], 5, 2,
		map[uint64]uint64{6: 1, 7: 2})
	if len(c.sp.Groups) != 2 || c.sp.Groups[1].GrpId != 5 ||
		c.sp.Groups[1].DataBlocks !=
			slice.GetDataGrpList()[0].GetDataBlocks() {
		t.Errorf("PutSp returned the geometry %v", c.sp.Groups)
	}

	// The DNs: dn 1 and dn 2 hold a meta and a data side; dn 3 and dn 4 hold
	// nothing.
	for dnId, want := range map[uint64]struct {
		charge uint64
		ptrs   []*pb.SidePointer
	}{
		1: {3, []*pb.SidePointer{
			{SpId: 1, LegId: 3, SideId: 8}, {SpId: 1, LegId: 6, SideId: 10},
		}},
		2: {3, []*pb.SidePointer{
			{SpId: 1, LegId: 4, SideId: 9}, {SpId: 1, LegId: 7, SideId: 11},
		}},
	} {
		dn := c.dnConf(t, ctx, dnId)
		if dn.GetFreeExtCnt() != nodeFree-want.charge ||
			dn.GetTotalExtCnt() != nodeFree {
			t.Errorf("dn %d has %d of %d extents free, want %d of %d", dnId,
				dn.GetFreeExtCnt(), dn.GetTotalExtCnt(),
				nodeFree-want.charge, nodeFree)
		}
		if !equalPtrs(dn.GetSidePtrList(), want.ptrs) {
			t.Errorf("dn %d side_ptr_list %v, want %v", dnId,
				dn.GetSidePtrList(), want.ptrs)
		}
		mustGet(t, ctx, c.cli,
			c.dnCapacityKey(t, dnId, nodeFree-want.charge), &pb.DnCapacity{})
		mustAbsent(t, ctx, c.cli,
			c.dnCapacityKey(t, dnId, nodeFree), &pb.DnCapacity{})
		if rev := c.dnRev(t, ctx, dnId); rev != 2 ||
			c.sp.DnRevisions[dnAddr(dnId)] != 2 {
			t.Errorf("dn %d revision %d, returned %d; want 2, one bump",
				dnId, rev, c.sp.DnRevisions[dnAddr(dnId)])
		}
	}
	for _, dnId := range []uint64{3, 4} {
		if dn := c.dnConf(t, ctx, dnId); dn.GetFreeExtCnt() != nodeFree ||
			len(dn.GetSidePtrList()) != 0 || c.dnRev(t, ctx, dnId) != 1 {
			t.Errorf("dn %d, which the pool does not name, changed: %v",
				dnId, dn)
		}
	}

	// The CNs: each cntlr's CN pays the footprint, the sum of every group.
	if c.sp.Footprint != 3 {
		t.Fatalf("footprint %d, want 3", c.sp.Footprint)
	}
	for cnId := uint64(1); cnId <= 2; cnId++ {
		cn := c.cnConf(t, ctx, cnId)
		want := []*pb.CntlrPointer{{SpId: 1, CntlrId: cnId}}
		if cn.GetFreeExtCnt() != nodeFree-3 ||
			len(cn.GetCntlrPtrList()) != 1 ||
			!proto.Equal(cn.GetCntlrPtrList()[0], want[0]) {
			t.Errorf("cn %d = %v, want %d free and cntlr %d", cnId, cn,
				nodeFree-3, cnId)
		}
		mustGet(t, ctx, c.cli,
			model.CnCapacityKey(cid, nodeFree-3, cnAddr(cnId)),
			&pb.CnCapacity{})
		if rev := c.cnRev(t, ctx, cnId); rev != 2 {
			t.Errorf("cn %d revision %d, want 2", cnId, rev)
		}
	}
	if cn := c.cnConf(t, ctx, 3); cn.GetFreeExtCnt() != nodeFree ||
		c.cnRev(t, ctx, 3) != 1 {
		t.Errorf("cn 3, which the pool does not name, changed: %v", cn)
	}

	// A second pool of the name is refused, and so is one whose DN is short
	// of extents; neither writes anything.
	if _, err := PutSp(ctx, c.cli, c.cl, smallSpec()); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a second PutSp = %v, want an already-exists refusal", err)
	}
	if _, _, err := SetDnFree(ctx, c.cli, c.cl, 4, 1); err != nil {
		t.Fatalf("SetDnFree: %v", err)
	}
	short := smokeSpec()
	short.Name, short.Id = "sp-short", 2
	short.Sides[1].DnId, short.Sides[3].DnId = 4, 4
	if _, err := PutSp(ctx, c.cli, c.cl, short); err == nil ||
		!strings.Contains(err.Error(), "free_ext_cnt") {
		t.Fatalf("PutSp on a DN short of extents = %v, want a refusal", err)
	}
	mustAbsent(t, ctx, c.cli, model.SpConfKey(cid, "sp-short"), &pb.SpConf{})
	if rev := c.dnRev(t, ctx, 1); rev != 2 {
		t.Fatalf("the refused PutSp moved dn 1's revision to %d", rev)
	}
	// ResolveSp finds the pool by name and by id, and nothing else.
	for _, spec := range []string{"sp0", "1", "0x1"} {
		ref, err := ResolveSp(ctx, c.cli, c.cl, spec)
		if err != nil || ref != c.sp.Ref {
			t.Errorf("ResolveSp(%q) = %v, %v; want %v", spec, ref, err,
				c.sp.Ref)
		}
	}
	if _, err := ResolveSp(ctx, c.cli, c.cl, "nosuch"); err == nil {
		t.Errorf("ResolveSp of a pool never planted succeeded")
	}
}

// TestPutSubObjectsReadBack pins what PutTds, PutSs, PutClone, PutXfer,
// PutMigr and the two bitmap calls store, as model.LoadSp reads it: the ids
// they take for a zero, one pool revision bump per transaction, the CdcEntry
// of a subsystem, a clone's clone meta group laid out, its sides on the DNs
// it names and charged there and no CN charged, a clone planted without one,
// the units of a migration on its destination DN, and a migration's second
// side on its leg.
func TestPutSubObjectsReadBack(t *testing.T) {
	ctx := testCtx(t)
	c := plantSmallCluster(t, ctx, true)
	cid, ref := c.cl.Id, c.sp.Ref
	blockSize := model.PoolBlockSize(c.load(t, ctx).Conf.GetBdevConf())
	rev := c.spRev(t, ctx)
	nextRev := func(t *testing.T, what string, got uint64) {
		t.Helper()
		rev++
		if got != rev {
			t.Fatalf("%s wrote sp revision %d, want %d", what, got, rev)
		}
	}

	tds, err := PutTds(ctx, c.cli, c.cl, ref, []TdSpec{
		{Name: "td0", Id: 20, Size: 1 << 30},
		{Name: "td1", Size: 2 << 30},
		{Name: "td2", DevId: 9, Size: 1 << 30},
	})
	if err != nil {
		t.Fatalf("PutTds: %v", err)
	}
	for i, want := range []Td{
		{Name: "td0", Id: 20, DevId: 1},
		{Name: "td1", Id: 21, DevId: 2},
		{Name: "td2", Id: 22, DevId: 9},
	} {
		want.SpRev = rev + 1
		if tds[i] != want {
			t.Errorf("td %d = %+v, want %+v", i, tds[i], want)
		}
	}
	nextRev(t, "PutTds", tds[0].SpRev)
	if _, err := PutTd(ctx, c.cli, c.cl, ref,
		TdSpec{Name: "td0", Id: 99}); err == nil {
		t.Fatalf("a second td0 was planted")
	}

	ss, err := PutSs(ctx, c.cli, c.cl, ref, SsSpec{
		Nqn: "nqn.2024-01.io.dnv-it:ss0", Id: 30, Serial: "s0",
		Namespaces: []NsSpec{
			{NsId: 31, NsIdx: 1, TdId: 20},
			{NsIdx: 2, TdId: 21},
		},
	})
	if err != nil {
		t.Fatalf("PutSs: %v", err)
	}
	nextRev(t, "PutSs", ss.SpRev)
	if ss.Id != 30 || !equalU64(ss.NsIds, 31, 32) ||
		ss.CdcKey != model.CdcEntryKey(cid, smokeShard, 1, 30) {
		t.Fatalf("PutSs = %+v", ss)
	}
	if _, err := PutSs(ctx, c.cli, c.cl, ref, SsSpec{
		Nqn: "nqn.2024-01.io.dnv-it:ss1",
		Namespaces: []NsSpec{
			{NsIdx: 1, TdId: 20}, {NsIdx: 1, TdId: 21},
		},
	}); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("two namespaces of one index = %v, want a refusal", err)
	}

	// c0's group takes ids below its own, so the ids every later zero takes
	// are the ones a clone with no group would leave: legs on dn 1, which
	// holds the pool's sides too, and on dn 5. No CN is charged for a
	// clone.
	cnsBefore := map[uint64]*pb.CnConf{}
	for cnId := uint64(1); cnId <= 2; cnId++ {
		cnsBefore[cnId] = c.cnConf(t, ctx, cnId)
	}
	clone, err := PutClone(ctx, c.cli, c.cl, ref, CloneSpec{
		Name: "c0", Id: 40, DstTd: "td0", SrcSliceCnt: 1,
		SrcNqn: "nqn.2024-01.io.dnv-it:src",
		GrpId:  33,
		Legs: []CloneLegSpec{
			{LegId: 34, LegIdx: 0, SideId: 35, DnId: 1, Slot: 0},
			{LegId: 36, LegIdx: 1, SideId: 37, DnId: 5, Slot: 1},
		},
	})
	if err != nil {
		t.Fatalf("PutClone: %v", err)
	}
	nextRev(t, "PutClone", clone.SpRev)
	if clone.Id != 40 || clone.DstTdId != 20 {
		t.Fatalf("PutClone = %+v, want clone 40 onto td 20", clone)
	}
	metaBlocks, dataBlocks, err := model.GroupBlocks(
		common.CloneMetaGrpExtCnt, common.MinDnExtSize,
		c.load(t, ctx).Conf.GetBdevConf())
	if err != nil {
		t.Fatalf("GroupBlocks: %v", err)
	}
	if clone.GrpId != 33 || !equalU64(clone.LegIds, 34, 36) ||
		!equalU64(clone.SideIds, 35, 37) ||
		clone.MetaBlocks != metaBlocks || clone.DataBlocks != dataBlocks ||
		len(clone.DnRevisions) != 2 || clone.DnRevisions[dnAddr(1)] != 3 ||
		clone.DnRevisions[dnAddr(5)] != 2 {
		t.Fatalf("PutClone's group = %+v, want group 33 of legs 34 and 36 "+
			"at %d and %d blocks, dn 1 at revision 3, dn 5 at 2", clone,
			metaBlocks, dataBlocks)
	}
	// One extent off each DN of the group, its pointer after the ones the
	// DN held, the capacity key moved and one DnRev bump.
	cloneSide := func(legId, sideId uint64) *pb.SidePointer {
		return &pb.SidePointer{SpId: ref.Id, LegId: legId, SideId: sideId}
	}
	c.dnHolds(t, ctx, 1, nodeFree-4, nodeFree-3, []*pb.SidePointer{
		cloneSide(3, 8), cloneSide(6, 10), cloneSide(34, 35),
	}, 3)
	c.dnHolds(t, ctx, 5, nodeFree-1, nodeFree, []*pb.SidePointer{
		cloneSide(36, 37),
	}, 2)
	// c1 names no leg, so it is planted with no group and charges no DN.
	byId, err := PutClone(ctx, c.cli, c.cl, ref,
		CloneSpec{Name: "c1", DstTd: "0x15"})
	if err != nil {
		t.Fatalf("PutClone by td_id: %v", err)
	}
	nextRev(t, "PutClone", byId.SpRev)
	if byId.Id != 41 || byId.DstTdId != 21 ||
		byId.GrpId != 0 || len(byId.LegIds) != 0 ||
		len(byId.DnRevisions) != 0 {
		t.Fatalf("PutClone by td_id = %+v, want td1's id and no group",
			byId)
	}
	for cnId, before := range cnsBefore {
		if cn := c.cnConf(t, ctx, cnId); !proto.Equal(cn, before) {
			t.Errorf("a clone moved cn %d: %v, want %v", cnId, cn, before)
		}
		if rev := c.cnRev(t, ctx, cnId); rev != 2 {
			t.Errorf("a clone moved cn %d's revision to %d", cnId, rev)
		}
	}

	xfer, err := PutXfer(ctx, c.cli, c.cl, ref, XferSpec{
		Name: "x0", OriNqn: "nqn.2024-01.io.dnv-it:ss0", OriNsIdx: 1,
	})
	if err != nil {
		t.Fatalf("PutXfer: %v", err)
	}
	nextRev(t, "PutXfer", xfer.SpRev)
	if xfer.Id != 42 {
		t.Fatalf("PutXfer took id %d, want 42", xfer.Id)
	}

	migr, err := PutMigr(ctx, c.cli, c.cl, ref, MigrSpec{
		Name: "m0", Id: 50, SrcSide: 11, DstSide: 51, DstDn: 4,
		CutoverPending: true,
	})
	if err != nil {
		t.Fatalf("PutMigr: %v", err)
	}
	nextRev(t, "PutMigr", migr.SpRev)
	migrUnits := common.CloneMetaUnits(2*common.MinDnExtSize, blockSize)
	if migr.LegId != 7 || migr.GrpId != 5 || migr.SliceId != 1 ||
		migr.ExtCnt != 2 || migr.MetaUnits != migrUnits ||
		migr.Addr != dnAddr(4) || migr.DnRevision != 2 ||
		!migr.CutoverPending {
		t.Fatalf("PutMigr = %+v", migr)
	}
	taken, err := PutMigr(ctx, c.cli, c.cl, ref, MigrSpec{
		Name: "m1", SrcSide: 8, DstDn: 4,
	})
	if err != nil {
		t.Fatalf("PutMigr with ids to take: %v", err)
	}
	nextRev(t, "PutMigr", taken.SpRev)
	if taken.Id != 52 || taken.DstSide != 53 || taken.LegId != 3 ||
		taken.DnRevision != 3 {
		t.Fatalf("PutMigr with ids to take = %+v, want ids 52 and 53", taken)
	}
	dn := c.dnConf(t, ctx, 4)
	if dn.GetFreeExtCnt() != nodeFree-3 ||
		dn.GetUsedMetaUnits() != migrUnits+taken.MetaUnits ||
		!equalPtrs(dn.GetSidePtrList(), []*pb.SidePointer{
			{SpId: 1, LegId: 7, SideId: 51}, {SpId: 1, LegId: 3, SideId: 53},
		}) {
		t.Fatalf("the destination dn = %v", dn)
	}

	for i, chunk := range []struct {
		spec    CloneBitmapSpec
		created bool
	}{
		{CloneBitmapSpec{Clone: "c0", SrcSliceIdx: 0, BmIdx: 0,
			Data: []byte{0xaa}}, true},
		{CloneBitmapSpec{Clone: "c0", SrcSliceIdx: 1, BmIdx: 0,
			Data: []byte{0xbb}}, true},
		{CloneBitmapSpec{Clone: "c0", SrcSliceIdx: 0, BmIdx: 0,
			Data: []byte{0xcc}}, false},
	} {
		bitmap, err := PutCloneBitmap(ctx, c.cli, c.cl, ref, chunk.spec)
		if err != nil {
			t.Fatalf("PutCloneBitmap %d: %v", i, err)
		}
		nextRev(t, "PutCloneBitmap", bitmap.SpRev)
		if bitmap.Created != chunk.created || bitmap.BmCnt != 0 {
			t.Errorf("PutCloneBitmap %d = %+v, want created %t", i, bitmap,
				chunk.created)
		}
	}
	for i, chunk := range []struct {
		idx     uint32
		created bool
		bmCnt   uint32
	}{{0, true, 1}, {1, true, 2}, {0, false, 2}} {
		bitmap, err := PutMigrBitmap(ctx, c.cli, c.cl, ref,
			MigrBitmapSpec{Migr: "m0", BmIdx: chunk.idx, Data: []byte{1}})
		if err != nil {
			t.Fatalf("PutMigrBitmap %d: %v", i, err)
		}
		nextRev(t, "PutMigrBitmap", bitmap.SpRev)
		if bitmap.Created != chunk.created || bitmap.BmCnt != chunk.bmCnt {
			t.Errorf("PutMigrBitmap %d = %+v, want created %t, bm_cnt %d",
				i, bitmap, chunk.created, chunk.bmCnt)
		}
	}
	if _, err := PutCloneBitmap(ctx, c.cli, c.cl, ref, CloneBitmapSpec{
		Clone: "nosuch", Data: []byte{1},
	}); err == nil {
		t.Fatalf("a chunk of a clone never planted was planted")
	}
	tooFar := CloneBitmapSpec{Clone: "c0", SrcSliceIdx: 1 << 8}
	if err := tooFar.Check(); err == nil {
		t.Fatalf("a src_slice_idx past its key field passed Check")
	}

	state := c.load(t, ctx)
	if state.SpRevision != rev {
		t.Fatalf("SpRev %d, want %d", state.SpRevision, rev)
	}
	conf := state.Conf
	if conf.GetNextId() != 54 || conf.GetNextDevId() != 10 ||
		strings.Join(conf.GetTdNameList(), ",") != "td0,td1,td2" ||
		strings.Join(conf.GetCloneNameList(), ",") != "c0,c1" ||
		strings.Join(conf.GetMigrNameList(), ",") != "m0,m1" {
		t.Fatalf("SpConf = %v", conf)
	}
	if len(state.Tds) != 3 || state.Tds[2].GetDevId() != 9 ||
		state.Tds[1].GetTdId() != 21 || state.Tds[0].GetCreated() {
		t.Fatalf("tds %v", state.Tds)
	}
	subsystem := state.Subsystems["nqn.2024-01.io.dnv-it:ss0"]
	uuid, nguid := nsIdentity(cid, 1, 31)
	if len(subsystem.GetNsList()) != 2 ||
		subsystem.GetNsList()[0].GetDevUuid() != uuid ||
		subsystem.GetNsList()[0].GetDevNguid() != nguid ||
		subsystem.GetNsList()[1].GetNsId() != 32 ||
		subsystem.GetNsList()[1].GetTdId() != 21 {
		t.Fatalf("subsystem %v", subsystem)
	}
	entry := &pb.CdcEntry{}
	mustGet(t, ctx, c.cli, ss.CdcKey, entry)
	cntlrs := []*pb.Cntlr{state.Cntlrs[1], state.Cntlrs[2]}
	if entry.GetNqn() != "nqn.2024-01.io.dnv-it:ss0" ||
		!equalTrConfs(entry.GetNvmeTrConfList(),
			model.CdcTrConfList(cntlrs)) {
		t.Fatalf("CdcEntry %v", entry)
	}
	trOf := func(id uint64) *pb.NvmeTrConf { return nvmeTrConfOf(dnAddr(id)) }
	wantGrp := &pb.Group{
		GrpId: 33, ExtCnt: common.CloneMetaGrpExtCnt,
		MetaBlocks: metaBlocks, DataBlocks: dataBlocks,
		LegList: []*pb.Leg{
			{LegId: 34, LegIdx: 0, SideList: []*pb.Side{{
				SideId: 35, AddrPort: dnAddr(1), CntlidSlot: 0,
				NvmeTrConf: trOf(1),
			}}},
			{LegId: 36, LegIdx: 1, SideList: []*pb.Side{{
				SideId: 37, AddrPort: dnAddr(5), CntlidSlot: 1,
				NvmeTrConf: trOf(5),
			}}},
		},
	}
	if got := state.Clones["c0"].GetMetaGrp(); !proto.Equal(got, wantGrp) {
		t.Fatalf("c0's clone meta group %v, want %v", got, wantGrp)
	}
	if got := state.Clones["c1"].GetMetaGrp(); got != nil {
		t.Fatalf("c1 carries a group %v, want none", got)
	}
	// The load reads the DN of every side of a clone meta group, as the
	// worker's snapshot does: dn 5 holds no side of a slice.
	if state.DnByAddr[dnAddr(5)].GetDnId() != 5 {
		t.Fatalf("the load holds no record of dn 5, which holds c0's side")
	}
	if state.Clones["c0"].GetCloneId() != 40 ||
		state.Xfers["x0"].GetOriNsIdx() != 1 ||
		state.Migrs["m0"].GetBmCnt() != 2 ||
		!state.Migrs["m0"].GetCutoverPending() ||
		state.Migrs["m1"].GetCutoverPending() {
		t.Fatalf("clones %v, xfers %v, migrs %v", state.Clones, state.Xfers,
			state.Migrs)
	}
	if got := chunkList(state.CloneBmIdx["c0"], true); got != "0:0,1:0" {
		t.Fatalf("c0's chunks %s, want 0:0,1:0", got)
	}
	if got := chunkList(state.MigrBmIdx["m0"], false); got != "0,1" {
		t.Fatalf("m0's chunks %s, want 0,1", got)
	}
	leg := state.Slices[1].GetDataGrpList()[0].GetLegList()[1]
	if leg.GetLegId() != 7 || len(leg.GetSideList()) != 2 ||
		leg.GetSideList()[1].GetSideId() != 51 ||
		leg.GetSideList()[1].GetAddrPort() != dnAddr(4) {
		t.Fatalf("the migrated leg %v, want sides 11 and 51", leg)
	}
}

// TestSettersReadBack pins every setter on a planted pool: each writes what
// it names and nothing else, the pool setters bump the pool's revision once,
// the epochs and the budgets bump none — a clone meta group's as a slice's
// — and the worker-role calls go through the model ops, the clone flip among
// them.
func TestSettersReadBack(t *testing.T) {
	ctx := testCtx(t)
	c := plantSmallCluster(t, ctx, true)
	cid, ref := c.cl.Id, c.sp.Ref
	rev := c.spRev(t, ctx)

	// SetCntlr: each flag flips both ways and leaves the other as stored, so
	// the steps walk cntlr 2 through every pair of the two flags and back to
	// the standby it was planted as.
	yes, no := true, false
	for _, step := range []struct {
		flags             CntlrFlags
		primary, disabled bool
	}{
		{CntlrFlags{Disabled: &yes}, false, true},
		{CntlrFlags{Primary: &yes}, true, true},
		{CntlrFlags{Disabled: &no}, true, false},
		{CntlrFlags{Primary: &no}, false, false},
	} {
		stored, spRev, err := SetCntlr(ctx, c.cli, c.cl, ref, 2, step.flags)
		if err != nil {
			t.Fatalf("SetCntlr %+v: %v", step.flags, err)
		}
		rev++
		if stored.GetPrimary() != step.primary ||
			stored.GetDisabled() != step.disabled || spRev != rev {
			t.Fatalf("SetCntlr = %v at revision %d; want primary %t "+
				"disabled %t at %d", stored, spRev, step.primary,
				step.disabled, rev)
		}
		read := c.load(t, ctx).Cntlrs[2]
		if read.GetPrimary() != step.primary ||
			read.GetDisabled() != step.disabled {
			t.Fatalf("cntlr 2 reads back %v; want primary %t disabled %t",
				read, step.primary, step.disabled)
		}
	}
	if _, _, err := SetCntlr(ctx, c.cli, c.cl, ref, 2,
		CntlrFlags{}); err == nil {
		t.Fatalf("SetCntlr with no flag succeeded")
	}

	if stored, spRev, err := SetLwm(ctx, c.cli, c.cl, ref, 0); err != nil ||
		stored != common.DefaultPoolLowWatermarkPct || spRev != rev+1 {
		t.Fatalf("SetLwm(0) = %d at %d, %v; want the default", stored, spRev,
			err)
	}
	rev++
	if stored, _, err := SetLwm(ctx, c.cli, c.cl, ref, 150); err != nil ||
		stored != 150 {
		t.Fatalf("SetLwm(150) = %d, %v", stored, err)
	}
	rev++
	if got := c.load(t, ctx).Conf.GetBdevConf().GetDmPoolConf().
		GetLowWaterMarkPct(); got != 150 {
		t.Fatalf("low_water_mark_pct %d, want 150", got)
	}

	// The budgets: dn 4 leaves allocation with its capacity key, and no
	// revision moves.
	addr, dn, err := SetDnFree(ctx, c.cli, c.cl, 4, 0)
	if err != nil || addr != dnAddr(4) || dn.GetFreeExtCnt() != 0 {
		t.Fatalf("SetDnFree = %q %v, %v", addr, dn, err)
	}
	if model.DnAllocatable(dn, c.cl.Conf.GetDnBinConf()) {
		t.Fatalf("a DN with no free extent is allocatable")
	}
	mustAbsent(t, ctx, c.cli, c.dnCapacityKey(t, 4, nodeFree),
		&pb.DnCapacity{})
	if got := c.dnRev(t, ctx, 4); got != 1 {
		t.Fatalf("SetDnFree moved the DN's revision to %d", got)
	}
	if _, cn, err := SetCnFree(ctx, c.cli, c.cl, 3, 10); err != nil ||
		cn.GetFreeExtCnt() != 10 {
		t.Fatalf("SetCnFree = %v, %v", cn, err)
	}
	mustGet(t, ctx, c.cli, model.CnCapacityKey(cid, 10, cnAddr(3)),
		&pb.CnCapacity{})
	mustAbsent(t, ctx, c.cli, model.CnCapacityKey(cid, nodeFree, cnAddr(3)),
		&pb.CnCapacity{})

	// The epochs: raw, so they bump nothing.
	if old, err := SetCntlrEpoch(ctx, c.cli, c.cl, ref, 1, 1234); err != nil ||
		old != 0 {
		t.Fatalf("SetCntlrEpoch = %d, %v", old, err)
	}
	if old, err := SetCntlrEpoch(ctx, c.cli, c.cl, ref, 1, 0); err != nil ||
		old != 1234 {
		t.Fatalf("SetCntlrEpoch clear = %d, %v; want the old 1234", old, err)
	}
	if _, err := SetLegEpoch(ctx, c.cli, c.cl, ref, 1, 4, 99); err != nil {
		t.Fatalf("SetLegEpoch on the meta group's second leg: %v", err)
	}
	if _, err := SetSideEpoch(ctx, c.cli, c.cl, ref, 1, 9, 77); err != nil {
		t.Fatalf("SetSideEpoch on that leg's side: %v", err)
	}
	if _, err := SetSideEpoch(ctx, c.cli, c.cl, ref, 1, 99, 77); err == nil ||
		!strings.Contains(err.Error(), "holds no side") {
		t.Fatalf("SetSideEpoch on no side = %v, want a refusal", err)
	}
	second := c.load(t, ctx).Slices[1].GetMetaGrpList()[0].GetLegList()[1]
	if second.GetLegId() != 4 || second.GetErrEpoch() != 99 ||
		second.GetSideList()[0].GetErrEpoch() != 77 {
		t.Fatalf("leg 4 %v, want epochs 99 and 77", second)
	}
	if got := c.spRev(t, ctx); got != rev {
		t.Fatalf("the epochs moved the pool's revision to %d", got)
	}

	// The revision keys.
	if got, err := BumpRev(ctx, c.cli, c.cl, RevDn, smokeShard,
		1); err != nil || got != 3 {
		t.Fatalf("BumpRev dn 1 = %d, %v; want 3", got, err)
	}
	if got, err := BumpRev(ctx, c.cli, c.cl, RevSp, smokeShard,
		1); err != nil || got != c.spRev(t, ctx) {
		t.Fatalf("BumpRev sp = %d, %v", got, err)
	}
	if _, err := BumpRev(ctx, c.cli, c.cl, RevCn, smokeShard,
		99); err == nil {
		t.Fatalf("BumpRev of a revision never planted succeeded")
	}
	key, old, err := DelRev(ctx, c.cli, c.cl, RevCn, smokeShard, 3)
	if err != nil || old != 1 || key != model.CnRevKey(smokeShard, cid, 3) {
		t.Fatalf("DelRev = %q %d, %v", key, old, err)
	}
	mustAbsent(t, ctx, c.cli, key, &pb.CnRev{})
	if _, _, err := DelRev(ctx, c.cli, c.cl, RevCn, smokeShard,
		3); err == nil {
		t.Fatalf("a second DelRev succeeded")
	}

	// The move of a DN that holds nothing: one revision bump, the record and
	// its capacity key at the new endpoint, nothing at the old one.
	const moved = "10.0.0.2:29605"
	oldAddr, revision, err := MoveDn(ctx, c.cli, c.cl, 5, smokeShard, moved)
	if err != nil {
		t.Fatalf("MoveDn: %v", err)
	}
	if oldAddr != dnAddr(5) || revision != c.dnRev(t, ctx, 5) {
		t.Fatalf("MoveDn = %q at %d", oldAddr, revision)
	}
	mustAbsent(t, ctx, c.cli, model.DnConfKey(cid, dnAddr(5)), &pb.DnConf{})
	movedDn := &pb.DnConf{}
	mustGet(t, ctx, c.cli, model.DnConfKey(cid, moved), movedDn)
	mustAbsent(t, ctx, c.cli, c.dnCapacityKey(t, 5, nodeFree),
		&pb.DnCapacity{})
	bin, _ := model.DnBinIdx(nodeFree, c.cl.Conf.GetDnBinConf())
	mustGet(t, ctx, c.cli, model.DnCapacityKey(cid, bin, nodeFree, moved),
		&pb.DnCapacity{})
	if movedDn.GetNvmeTrConf().GetTrAddr() != "10.0.0.2" {
		t.Fatalf("the moved DN's transport %v", movedDn.GetNvmeTrConf())
	}
	revRec := &pb.DnRev{}
	mustGet(t, ctx, c.cli, model.DnRevKey(smokeShard, cid, 5), revRec)
	if revRec.GetAddrPort() != moved {
		t.Fatalf("DnRev %v, want the new endpoint", revRec)
	}
	if got, err := DnAddr(ctx, c.cli, c.cl, 5); err != nil || got != moved {
		t.Fatalf("DnAddr after the move = %q, %v", got, err)
	}
	if _, _, err := MoveDn(ctx, c.cli, c.cl, 5, smokeShard, moved); err ==
		nil {
		t.Fatalf("a move to the endpoint the DN is at succeeded")
	}
	// The worker's flips and the cutover, through the model ops.
	if _, err := PutMigr(ctx, c.cli, c.cl, ref, MigrSpec{
		Name: "m0", Id: 50, SrcSide: 11, DstSide: 51, DstDn: 5,
		CutoverPending: true,
	}); err != nil {
		t.Fatalf("PutMigr: %v", err)
	}
	if _, err := StartCutover(ctx, c.cli, c.cl, ref, "m0", 0); err == nil {
		t.Fatalf("StartCutover before the dst side is provisioned succeeded")
	}
	flipped, spRev, err := SetProvisioned(ctx, c.cli, c.cl, ref,
		model.SideRef{SliceId: 1, LegId: 7, SideId: 51})
	if err != nil || !flipped || spRev != c.spRev(t, ctx) {
		t.Fatalf("SetProvisioned = %t at %d, %v", flipped, spRev, err)
	}
	cutover, err := StartCutover(ctx, c.cli, c.cl, ref, "m0", 0)
	if err != nil {
		t.Fatalf("StartCutover: %v", err)
	}
	if cutover.CntlrId != 1 || cutover.Ref.LegId != 7 ||
		cutover.Ref.SliceId != 1 || cutover.Ref.DstSideId != 51 {
		t.Fatalf("StartCutover = %+v", cutover)
	}
	if c.load(t, ctx).Migrs["m0"].GetCutoverPending() {
		t.Fatalf("the cutover left cutover_pending set")
	}
	if _, err := PutTd(ctx, c.cli, c.cl, ref,
		TdSpec{Name: "td0", Size: 1 << 30}); err != nil {
		t.Fatalf("PutTd: %v", err)
	}
	tdId, flipped, _, err := SetCreated(ctx, c.cli, c.cl, ref, "td0")
	if err != nil || !flipped || tdId == 0 {
		t.Fatalf("SetCreated = %d %t, %v", tdId, flipped, err)
	}
	if _, flipped, _, err := SetCreated(ctx, c.cli, c.cl, ref,
		"td0"); err != nil || flipped {
		t.Fatalf("a second SetCreated = %t, %v; want no flip", flipped, err)
	}
	if !c.load(t, ctx).Tds[0].GetCreated() {
		t.Fatalf("td0 is not created")
	}

	// A clone meta group's legs and sides, through the clone setters: the
	// epochs raw, the flip through model.FlipCloneProvisioned.
	if _, err := PutTd(ctx, c.cli, c.cl, ref,
		TdSpec{Name: "tdC", Size: 1 << 30}); err != nil {
		t.Fatalf("PutTd: %v", err)
	}
	clone, err := PutClone(ctx, c.cli, c.cl, ref, CloneSpec{
		Name: "c0", DstTd: "tdC",
		Legs: []CloneLegSpec{
			{LegIdx: 0, DnId: 1},
			{LegIdx: 1, DnId: 2},
		},
	})
	if err != nil {
		t.Fatalf("PutClone: %v", err)
	}
	rev = c.spRev(t, ctx)
	if old, err := SetCloneLegEpoch(ctx, c.cli, c.cl, ref, "c0",
		clone.LegIds[1], 99); err != nil || old != 0 {
		t.Fatalf("SetCloneLegEpoch on the second leg = %d, %v", old, err)
	}
	if old, err := SetCloneSideEpoch(ctx, c.cli, c.cl, ref, "c0",
		clone.SideIds[0], 77); err != nil || old != 0 {
		t.Fatalf("SetCloneSideEpoch on the first leg's side = %d, %v", old,
			err)
	}
	if old, err := SetCloneSideEpoch(ctx, c.cli, c.cl, ref, "c0",
		clone.SideIds[0], 0); err != nil || old != 77 {
		t.Fatalf("SetCloneSideEpoch clear = %d, %v; want the old 77", old,
			err)
	}
	if _, err := SetCloneSideEpoch(ctx, c.cli, c.cl, ref, "c0", 8,
		77); err == nil || !strings.Contains(err.Error(), "holds no side") {
		t.Fatalf("SetCloneSideEpoch on a slice's side = %v, want a refusal",
			err)
	}
	if _, err := SetCloneLegEpoch(ctx, c.cli, c.cl, ref, "nosuch",
		clone.LegIds[0], 77); err == nil {
		t.Fatalf("SetCloneLegEpoch on a clone never planted succeeded")
	}
	grp := c.load(t, ctx).Clones["c0"].GetMetaGrp()
	if grp.GetLegList()[1].GetErrEpoch() != 99 ||
		grp.GetLegList()[0].GetSideList()[0].GetErrEpoch() != 0 ||
		grp.GetLegList()[0].GetErrEpoch() != 0 {
		t.Fatalf("c0's group %v, want only the second leg's epoch at 99",
			grp)
	}
	if got := c.spRev(t, ctx); got != rev {
		t.Fatalf("the clone epochs moved the pool's revision to %d", got)
	}
	cloneId, flipped, spRev, err := SetCloneProvisioned(ctx, c.cli, c.cl,
		ref, "c0", clone.LegIds[1], clone.SideIds[1])
	if err != nil || !flipped || cloneId != clone.Id || spRev != rev+1 ||
		spRev != c.spRev(t, ctx) {
		t.Fatalf("SetCloneProvisioned = %d, %t at %d, %v; want clone %d "+
			"flipped at %d", cloneId, flipped, spRev, err, clone.Id, rev+1)
	}
	if _, flipped, _, err := SetCloneProvisioned(ctx, c.cli, c.cl, ref,
		"c0", clone.LegIds[1], clone.SideIds[1]); err != nil || flipped {
		t.Fatalf("a second SetCloneProvisioned = %t, %v; want no flip",
			flipped, err)
	}
	if _, _, _, err := SetCloneProvisioned(ctx, c.cli, c.cl, ref, "nosuch",
		clone.LegIds[1], clone.SideIds[1]); err == nil {
		t.Fatalf("SetCloneProvisioned of a clone never planted succeeded")
	}
	grp = c.load(t, ctx).Clones["c0"].GetMetaGrp()
	if !grp.GetLegList()[1].GetSideList()[0].GetProvisioned() ||
		grp.GetLegList()[0].GetSideList()[0].GetProvisioned() {
		t.Fatalf("c0's group %v, want only the second leg's side "+
			"provisioned", grp)
	}

	// The level, after the cutover, which a level of NO_MIGRATION and
	// above would refuse.
	rev = c.spRev(t, ctx)
	if spRev, err := SetLevel(ctx, c.cli, c.cl, ref,
		pb.SpLevel_SP_LEVEL_NO_THINPOOL); err != nil || spRev != rev+1 {
		t.Fatalf("SetLevel = %d, %v", spRev, err)
	}
	if got := c.load(t, ctx).Conf.GetSpLevel(); got !=
		pb.SpLevel_SP_LEVEL_NO_THINPOOL {
		t.Fatalf("sp_level %v", got)
	}
	if _, err := SetLevel(ctx, c.cli, c.cl, ref, pb.SpLevel(47)); err == nil {
		t.Fatalf("SetLevel of an undefined level succeeded")
	}

}

// TestDrainsGiveBackWhatPlantCharged pins the two latches and the two
// drains: a latch bumps the pool's revision once and a second one nothing, a
// drain of one step leaves the object, and a drain to the end removes it and
// gives back exactly what the plants charged — the clone's clone meta
// group's extents, pointers and capacity keys on the DNs of its legs, with
// one DnRev bump each, a DN it shares with the pool keeping the pool's
// sides; the pool's extents and pointers on every node.
func TestDrainsGiveBackWhatPlantCharged(t *testing.T) {
	ctx := testCtx(t)
	c := plantSmallCluster(t, ctx, true)
	cid, ref := c.cl.Id, c.sp.Ref

	if _, err := PutTd(ctx, c.cli, c.cl, ref,
		TdSpec{Name: "td0", Size: 1 << 30}); err != nil {
		t.Fatalf("PutTd: %v", err)
	}
	// The group's legs on dn 1, beside the pool's sides 8 and 10, and on
	// dn 4; every id a zero takes.
	clone, err := PutClone(ctx, c.cli, c.cl, ref, CloneSpec{
		Name: "c0", DstTd: "td0",
		Legs: []CloneLegSpec{
			{LegIdx: 0, DnId: 1},
			{LegIdx: 1, DnId: 4},
		},
	})
	if err != nil {
		t.Fatalf("PutClone: %v", err)
	}
	if clone.GrpId != clone.Id+1 || len(clone.LegIds) != 2 ||
		len(clone.SideIds) != 2 {
		t.Fatalf("PutClone = %+v, want a group of two legs taking the "+
			"ids after the clone's", clone)
	}
	poolSides := []*pb.SidePointer{
		{SpId: ref.Id, LegId: 3, SideId: 8},
		{SpId: ref.Id, LegId: 6, SideId: 10},
	}
	cloneSide := func(i int) *pb.SidePointer {
		return &pb.SidePointer{SpId: ref.Id, LegId: clone.LegIds[i],
			SideId: clone.SideIds[i]}
	}
	c.dnHolds(t, ctx, 1, nodeFree-4, nodeFree-3,
		append(append([]*pb.SidePointer(nil), poolSides...), cloneSide(0)), 3)
	c.dnHolds(t, ctx, 4, nodeFree-1, nodeFree,
		[]*pb.SidePointer{cloneSide(1)}, 2)
	for idx := uint32(0); idx < 3; idx++ {
		if _, err := PutCloneBitmap(ctx, c.cli, c.cl, ref, CloneBitmapSpec{
			Clone: "c0", BmIdx: idx, Data: []byte{byte(idx)},
		}); err != nil {
			t.Fatalf("PutCloneBitmap %d: %v", idx, err)
		}
	}
	latch, err := SetCloneDeleting(ctx, c.cli, c.cl, ref, "c0")
	if err != nil || !latch.Latched || latch.Id != clone.Id ||
		latch.SpRev != c.spRev(t, ctx) {
		t.Fatalf("SetCloneDeleting = %+v, %v", latch, err)
	}
	again, err := SetCloneDeleting(ctx, c.cli, c.cl, ref, "c0")
	if err != nil || again.Latched || again.SpRev != latch.SpRev {
		t.Fatalf("a second SetCloneDeleting = %+v, %v; want no bump", again,
			err)
	}
	// A batch removes chunk keys only: the group stays charged.
	step, err := DrainClone(ctx, c.cli, c.cl, ref, "c0", 1)
	if err != nil || step.Steps != 1 || step.ChunkCnt != 3 || step.Deleted ||
		step.GrpSideCnt != 0 {
		t.Fatalf("DrainClone for one step = %+v, %v", step, err)
	}
	c.dnHolds(t, ctx, 4, nodeFree-1, nodeFree-1,
		[]*pb.SidePointer{cloneSide(1)}, 2)
	done, err := DrainClone(ctx, c.cli, c.cl, ref, "c0", 4096)
	if err != nil || !done.Deleted || done.ChunkCnt != 0 ||
		done.GrpSideCnt != 2 {
		t.Fatalf("DrainClone to the end = %+v, %v", done, err)
	}
	mustAbsent(t, ctx, c.cli, model.CloneKey(cid, ref.Id, "c0"), &pb.Clone{})
	// The final step gave every side of the group back, each DN with one
	// more DnRev bump, and took only the clone's pointer off dn 1. The DNs
	// the clone never used kept their revisions: PutSp's for dn 2, the
	// planted one for dn 3 and dn 5.
	c.dnHolds(t, ctx, 1, nodeFree-3, nodeFree-4, poolSides, 4)
	c.dnHolds(t, ctx, 4, nodeFree, nodeFree-1, nil, 3)
	for dnId, want := range map[uint64]uint64{2: 2, 3: 1, 5: 1} {
		if got := c.dnRev(t, ctx, dnId); got != want {
			t.Errorf("dn %d is at revision %d after the clone's drain, "+
				"want %d", dnId, got, want)
		}
	}
	if _, err := DrainClone(ctx, c.cli, c.cl, ref, "c0", 4096); err == nil {
		t.Fatalf("a drain of a clone that is gone succeeded")
	}

	spLatch, err := SetDeleting(ctx, c.cli, c.cl, ref)
	if err != nil || !spLatch.Latched || spLatch.Id != ref.Id {
		t.Fatalf("SetDeleting = %+v, %v", spLatch, err)
	}
	if again, err := SetDeleting(ctx, c.cli, c.cl, ref); err != nil ||
		again.Latched || again.SpRev != spLatch.SpRev {
		t.Fatalf("a second SetDeleting = %+v, %v; want no bump", again, err)
	}
	first, err := DrainSp(ctx, c.cli, c.cl, ref, 1)
	if err != nil || first.Steps != 1 || first.CntlrCnt != 2 ||
		first.Deleted {
		t.Fatalf("DrainSp for one step = %+v, %v", first, err)
	}
	rest, err := DrainSp(ctx, c.cli, c.cl, ref, 4096)
	if err != nil || !rest.Deleted || rest.SliceCnt != 1 || rest.GrpCnt != 2 {
		t.Fatalf("DrainSp to the end = %+v, %v", rest, err)
	}
	mustAbsent(t, ctx, c.cli, model.SpConfKey(cid, ref.Name), &pb.SpConf{})
	mustAbsent(t, ctx, c.cli, model.SpNameKey(cid, ref.Id), &pb.SpName{})
	mustAbsent(t, ctx, c.cli, model.SpRevKey(ref.Shard, cid, ref.Id),
		&pb.SpRev{})
	mustAbsent(t, ctx, c.cli, model.SliceKey(cid, ref.Id, 1), &pb.Slice{})
	for dnId := uint64(1); dnId <= 3; dnId++ {
		dn := c.dnConf(t, ctx, dnId)
		if dn.GetFreeExtCnt() != nodeFree || len(dn.GetSidePtrList()) != 0 {
			t.Errorf("dn %d after the drain = %v, want %d free and no side",
				dnId, dn, nodeFree)
		}
		mustGet(t, ctx, c.cli, c.dnCapacityKey(t, dnId, nodeFree),
			&pb.DnCapacity{})
	}
	for cnId := uint64(1); cnId <= 2; cnId++ {
		cn := c.cnConf(t, ctx, cnId)
		if cn.GetFreeExtCnt() != nodeFree || len(cn.GetCntlrPtrList()) != 0 {
			t.Errorf("cn %d after the drain = %v, want %d free and no cntlr",
				cnId, cn, nodeFree)
		}
	}
	global := &pb.SpGlobal{}
	mustGet(t, ctx, c.cli, model.SpGlobalKey(cid), global)
	if global.GetShardBucket()[smokeShard] != 0 {
		t.Fatalf("SpGlobal still counts the pool in shard %#x: %v",
			smokeShard, global)
	}
}

// TestPutCloneRefusals pins what PutClone refuses inside its STM, each
// before anything is written: a group, leg or side id a slice or another
// clone of the pool already uses for its kind, a slot outside the pool's
// list, and a DN short of an extent. An id of another kind may meet one of the pool's, as PutSp lets
// it. PutMigr takes no side of a clone meta group as its source, nor a clone
// meta side's id for its destination.
func TestPutCloneRefusals(t *testing.T) {
	ctx := testCtx(t)
	c := plantSmallCluster(t, ctx, true)
	cid, ref := c.cl.Id, c.sp.Ref
	for _, name := range []string{"td0", "td1", "td2"} {
		if _, err := PutTd(ctx, c.cli, c.cl, ref,
			TdSpec{Name: name, Size: 1 << 30}); err != nil {
			t.Fatalf("PutTd %s: %v", name, err)
		}
	}
	// c0's group takes explicit ids: 40, legs 41 and 43, sides 42 and 44.
	if _, err := PutClone(ctx, c.cli, c.cl, ref, CloneSpec{
		Name: "c0", DstTd: "td0", GrpId: 40,
		Legs: []CloneLegSpec{
			{LegId: 41, LegIdx: 0, SideId: 42, DnId: 4},
			{LegId: 43, LegIdx: 1, SideId: 44, DnId: 5},
		},
	}); err != nil {
		t.Fatalf("PutClone c0: %v", err)
	}
	// refused fails the case unless the spec is refused with want and the
	// store is as it was: no clone key, the pool's revision and every DN's
	// record and revision unchanged.
	refused := func(t *testing.T, spec CloneSpec, want string) {
		t.Helper()
		rev := c.spRev(t, ctx)
		var before []*pb.DnConf
		var revs []uint64
		for id := uint64(1); id <= 5; id++ {
			before = append(before, c.dnConf(t, ctx, id))
			revs = append(revs, c.dnRev(t, ctx, id))
		}
		_, err := PutClone(ctx, c.cli, c.cl, ref, spec)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("PutClone %s = %v, want a refusal naming %q",
				spec.Name, err, want)
		}
		mustAbsent(t, ctx, c.cli, model.CloneKey(cid, ref.Id, spec.Name),
			&pb.Clone{})
		if got := c.spRev(t, ctx); got != rev {
			t.Errorf("the refusal moved the pool's revision to %d", got)
		}
		for id := uint64(1); id <= 5; id++ {
			if dn := c.dnConf(t, ctx, id); !proto.Equal(dn, before[id-1]) ||
				c.dnRev(t, ctx, id) != revs[id-1] {
				t.Errorf("the refusal wrote dn %d: %v", id, dn)
			}
		}
	}
	two := func(name string, grp, leg0, side0, leg1, side1 uint64,
		slot uint32) CloneSpec {
		return CloneSpec{
			Name: name, DstTd: "td1", GrpId: grp,
			Legs: []CloneLegSpec{
				{LegId: leg0, LegIdx: 0, SideId: side0, DnId: 1, Slot: slot},
				{LegId: leg1, LegIdx: 1, SideId: side1, DnId: 2, Slot: slot},
			},
		}
	}
	refused(t, two("cSide", 50, 51, 8, 53, 54, 0),
		"side id 0x8 is already used")
	refused(t, two("cLeg", 50, 51, 52, 6, 54, 0),
		"leg id 0x6 is already used")
	refused(t, two("cGrp", 5, 51, 52, 53, 54, 0),
		"group id 0x5 is already used")
	refused(t, two("cCloneSide", 50, 51, 44, 53, 54, 0),
		"side id 0x2c is already used")
	refused(t, two("cCloneLeg", 50, 41, 52, 53, 54, 0),
		"leg id 0x29 is already used")
	refused(t, two("cCloneGrp", 40, 51, 52, 53, 54, 0),
		"group id 0x28 is already used")
	refused(t, two("cSlot", 50, 51, 52, 53, 54, 7), "cntlid slot 7")
	if _, _, err := SetDnFree(ctx, c.cli, c.cl, 2, 0); err != nil {
		t.Fatalf("SetDnFree: %v", err)
	}
	refused(t, two("cShort", 50, 51, 52, 53, 54, 0), "free_ext_cnt 0")
	if _, _, err := SetDnFree(ctx, c.cli, c.cl, 2,
		nodeFree-3); err != nil {
		t.Fatalf("SetDnFree: %v", err)
	}
	// A side of a clone meta group is no migration's source.
	if _, err := PutMigr(ctx, c.cli, c.cl, ref, MigrSpec{
		Name: "mClone", SrcSide: 42, DstDn: 3,
	}); err == nil || !strings.Contains(err.Error(), "no side 0x2a") {
		t.Fatalf("PutMigr of clone side 42 = %v, want a refusal", err)
	}
	// Nor is a clone meta side's id a migration destination's: on a DN
	// holding both, the clone drain would take the destination's pointer
	// with the clone's, since a pointer is removed by pool and side id.
	if _, err := PutMigr(ctx, c.cli, c.cl, ref, MigrSpec{
		Name: "mDst", SrcSide: 8, DstSide: 44, DstDn: 3,
	}); err == nil ||
		!strings.Contains(err.Error(), "side id 0x2c is already used") {
		t.Fatalf("PutMigr onto clone side id 44 = %v, want a refusal", err)
	}
	mustAbsent(t, ctx, c.cli, model.MigrationKey(cid, ref.Id, "mDst"),
		&pb.Migration{})
	// Ids of another kind may meet the pool's: group 8 beside side 8, leg
	// 10 beside side 10, side 2 beside group 2.
	if _, err := PutClone(ctx, c.cli, c.cl, ref,
		two("cKinds", 8, 10, 2, 53, 54, 0)); err != nil {
		t.Fatalf("PutClone with ids of other kinds: %v", err)
	}
}

// TestNodeListsTakeIds pins PutDns and PutCns at the per-cluster ceilings,
// common.MaxDnCntPerCluster and common.MaxCnCntPerCluster nodes in one call
// each, over many transactions: every zero id takes the next free one, so
// the ids run 1 up without a gap whether a spec gives its id or not, every
// node gets its record, revision and capacity key, the shard buckets count
// each node once, and a node written twice in one call — the second time in
// a later transaction — is the update the same two calls would write. The
// ceilings are a full shape, so under -short the lists are cut to span
// three transactions.
func TestNodeListsTakeIds(t *testing.T) {
	ctx := testCtx(t)
	cli := newTestClient(t)
	cl, err := PutCluster(ctx, cli, ClusterSpec{
		Name:       testClusterName(t),
		ExtentSize: common.MinDnExtSize,
	})
	if err != nil {
		t.Fatalf("PutCluster: %v", err)
	}
	dnCnt, cnCnt := common.MaxDnCntPerCluster, common.MaxCnCntPerCluster
	if testing.Short() {
		dnCnt, cnCnt = 2*nodesPerTxn+1, 2*nodesPerTxn+1
	}
	if dnCnt <= 2*nodesPerTxn || cnCnt <= 2*nodesPerTxn {
		t.Fatalf("the lists must span more than two transactions")
	}
	// perShard is how many of n nodes spread over the shards by index land
	// in one shard.
	perShard := func(n int, shard int) uint32 {
		cnt := n / common.ShardBucketSize
		if shard < n%common.ShardBucketSize {
			cnt++
		}
		return uint32(cnt)
	}
	addrOf := func(kind string, i int) string {
		return fmt.Sprintf("10.%s.%d.%d:29600", kind, i/256, i%256)
	}
	dnSpecs := make([]DnSpec, 0, dnCnt+1)
	for i := 0; i < dnCnt; i++ {
		dnSpecs = append(dnSpecs, DnSpec{
			Shard: uint32(i % common.ShardBucketSize), Addr: addrOf("1", i),
			Location: fmt.Sprintf("dn-loc%d", i), FreeExt: nodeFree,
		})
	}
	// The first node again, past the first transaction, with fewer
	// extents: the update a second PutDn would be.
	again := dnSpecs[0]
	again.Id, again.FreeExt = 1, nodeFree-1
	dnSpecs = append(dnSpecs, again)
	cnSpecs := make([]CnSpec, 0, cnCnt)
	for i := 0; i < cnCnt; i++ {
		spec := CnSpec{
			Shard: uint32(i % common.ShardBucketSize), Addr: addrOf("2", i),
			Location: fmt.Sprintf("cn-loc%d", i), FreeExt: nodeFree,
		}
		// Every other spec names its id; the rest take one.
		if i%2 == 0 {
			spec.Id = uint64(i + 1)
		}
		cnSpecs = append(cnSpecs, spec)
	}

	start := time.Now()
	dns, err := PutDns(ctx, cli, cl, dnSpecs)
	if err != nil {
		t.Fatalf("PutDns: %v", err)
	}
	cns, err := PutCns(ctx, cli, cl, cnSpecs)
	if err != nil {
		t.Fatalf("PutCns: %v", err)
	}
	t.Logf("planted %d DNs and %d CNs in %v", dnCnt, cnCnt,
		time.Since(start))

	if len(dns) != dnCnt+1 || len(cns) != cnCnt {
		t.Fatalf("PutDns wrote %d, PutCns %d", len(dns), len(cns))
	}
	for i := 0; i < dnCnt; i++ {
		if got := dns[i].Conf.GetDnId(); got != uint64(i+1) ||
			dns[i].Revision != 1 || dns[i].Addr != addrOf("1", i) {
			t.Fatalf("dn %d = id %d at %q, revision %d", i, got,
				dns[i].Addr, dns[i].Revision)
		}
	}
	if last := dns[dnCnt]; last.Conf.GetDnId() != 1 || last.Revision != 2 {
		t.Fatalf("the second write of dn 1 = id %d, revision %d; want 1, 2",
			last.Conf.GetDnId(), last.Revision)
	}
	for i, cn := range cns {
		if got := cn.Conf.GetCnId(); got != uint64(i+1) {
			t.Fatalf("cn %d took id %d, want %d", i, got, i+1)
		}
	}

	dnGlobal := &pb.DnGlobal{}
	mustGet(t, ctx, cli, model.DnGlobalKey(cl.Id), dnGlobal)
	cnGlobal := &pb.CnGlobal{}
	mustGet(t, ctx, cli, model.CnGlobalKey(cl.Id), cnGlobal)
	if dnGlobal.GetNextId() != uint64(dnCnt+1) ||
		cnGlobal.GetNextId() != uint64(cnCnt+1) {
		t.Fatalf("next_id dn %d, cn %d; want %d and %d",
			dnGlobal.GetNextId(), cnGlobal.GetNextId(), dnCnt+1, cnCnt+1)
	}
	for shard := 0; shard < common.ShardBucketSize; shard++ {
		if dnGlobal.GetShardBucket()[shard] != perShard(dnCnt, shard) ||
			cnGlobal.GetShardBucket()[shard] != perShard(cnCnt, shard) {
			t.Fatalf("shard %#x counts %d DNs and %d CNs", shard,
				dnGlobal.GetShardBucket()[shard],
				cnGlobal.GetShardBucket()[shard])
		}
	}
	if n := keyCount(t, ctx, cli, model.DnConfPrefix(cl.Id)); n != dnCnt {
		t.Fatalf("%d DnConf keys, want %d", n, dnCnt)
	}
	if n := keyCount(t, ctx, cli, model.CnConfPrefix(cl.Id)); n != cnCnt {
		t.Fatalf("%d CnConf keys, want %d", n, cnCnt)
	}
	if n := keyCount(t, ctx, cli, model.CnCapacityPrefix(cl.Id)); n != cnCnt {
		t.Fatalf("%d CN capacity keys, want %d", n, cnCnt)
	}
	// One free count apart, both counts share a bin, so the update moved dn
	// 1's key inside it: one key per DN, dn 1's at its new count.
	dnBin, _ := model.DnBinIdx(nodeFree, cl.Conf.GetDnBinConf())
	if bin, _ := model.DnBinIdx(nodeFree-1, cl.Conf.GetDnBinConf()); bin !=
		dnBin {
		t.Fatalf("%d and %d free extents fall in bins %d and %d", nodeFree,
			nodeFree-1, dnBin, bin)
	}
	if n := keyCount(t, ctx, cli, model.DnCapacityPrefix(cl.Id, dnBin)); n !=
		dnCnt {
		t.Fatalf("%d DN capacity keys, want %d", n, dnCnt)
	}
	mustAbsent(t, ctx, cli,
		model.DnCapacityKey(cl.Id, dnBin, nodeFree, addrOf("1", 0)),
		&pb.DnCapacity{})
	mustGet(t, ctx, cli,
		model.DnCapacityKey(cl.Id, dnBin, nodeFree-1, addrOf("1", 0)),
		&pb.DnCapacity{})
	dn1 := &pb.DnConf{}
	mustGet(t, ctx, cli, model.DnConfKey(cl.Id, addrOf("1", 0)), dn1)
	if dn1.GetFreeExtCnt() != nodeFree-1 {
		t.Fatalf("dn 1 has %d free extents, want the update's %d",
			dn1.GetFreeExtCnt(), nodeFree-1)
	}
	revKeys := 0
	for shard := uint32(0); shard < common.ShardBucketSize; shard++ {
		revKeys += keyCount(t, ctx, cli, model.DnRevPrefix(shard)+
			fmt.Sprintf(common.IdKeyFmt, cl.Id)+" ")
	}
	if revKeys != dnCnt {
		t.Fatalf("%d DnRev keys, want %d", revKeys, dnCnt)
	}
	if _, err := PutDns(ctx, cli, cl, []DnSpec{{Addr: ""}}); err == nil {
		t.Fatalf("a DN with no endpoint was planted")
	}
}

// TestWidestPoolPlants plants a pool at the limit of every object it plants:
// common.MaxSliceCntPerSp slices of a meta and a data group, each of
// common.MaxAllocLegPerGrp legs, common.MaxCntlrCntPerSp cntlrs,
// common.MaxSsCntPerSp subsystems of common.MaxNsCntPerSs namespaces,
// common.MaxTdCntPerSp thin devices, common.MaxCloneCntPerSp clones, each
// with a clone meta group of the widest shape, common.MaxXferCntPerSp
// transfers and common.MaxMigrCntPerSp migrations, and reads it back through
// model.LoadSp. It plants no repair and no retired leg; the widest messages,
// which hold both, are model's (widestCounts). The last clones' puts read
// every slice and every other clone of the pool in one transaction
// (putCloneGrp). It is a full shape, so it is skipped under -short.
func TestWidestPoolPlants(t *testing.T) {
	if testing.Short() {
		t.Skip("a full shape; skipped under -short")
	}
	ctx := testCtx(t)
	cli := newTestClient(t)
	cl, err := PutCluster(ctx, cli, ClusterSpec{
		Name:       testClusterName(t),
		ExtentSize: common.MinDnExtSize,
	})
	if err != nil {
		t.Fatalf("PutCluster: %v", err)
	}
	const (
		sliceCnt = common.MaxSliceCntPerSp
		legCnt   = common.MaxAllocLegPerGrp
		migrCnt  = common.MaxMigrCntPerSp
		free     = 1 << 20
	)
	// One DN per leg position, so every group's legs sit on distinct DNs,
	// and one more per migration destination.
	var dnSpecs []DnSpec
	for i := 0; i < legCnt+migrCnt; i++ {
		dnSpecs = append(dnSpecs, DnSpec{
			Addr: dnAddr(uint64(i + 1)), Location: fmt.Sprintf("r%d", i),
			FreeExt: free,
		})
	}
	if _, err := PutDns(ctx, cli, cl, dnSpecs); err != nil {
		t.Fatalf("PutDns: %v", err)
	}
	spec := SpSpec{Name: "widest", Id: 1}
	for i := 0; i < common.MaxCntlrCntPerSp; i++ {
		if _, err := PutCn(ctx, cli, cl, CnSpec{
			Addr: cnAddr(uint64(i + 1)), Location: fmt.Sprintf("c%d", i),
			FreeExt: free,
		}); err != nil {
			t.Fatalf("PutCn: %v", err)
		}
		spec.Cntlrs = append(spec.Cntlrs, CntlrSpec{
			CntlrId: uint64(i + 1), CnId: uint64(i + 1), Slot: uint32(i),
			Primary: i == 0,
		})
	}
	id := uint64(100)
	next := func() uint64 { id++; return id }
	var dataSides []uint64
	for s := 0; s < sliceCnt; s++ {
		sliceId := next()
		spec.Slices = append(spec.Slices,
			SliceSpec{SliceId: sliceId, SliceIdx: uint32(s)})
		for _, meta := range []bool{true, false} {
			grpId := next()
			extCnt := uint64(2)
			if meta {
				extCnt = 1
			}
			spec.Groups = append(spec.Groups, GroupSpec{
				SliceId: sliceId, GrpId: grpId, Meta: meta, ExtCnt: extCnt,
				Raid1: true,
			})
			for l := 0; l < legCnt; l++ {
				legId, sideId := next(), next()
				spec.Legs = append(spec.Legs, LegSpec{
					GrpId: grpId, LegId: legId, LegIdx: uint32(l),
				})
				spec.Sides = append(spec.Sides, SideSpec{
					LegId: legId, SideId: sideId, DnId: uint64(l + 1),
				})
				if !meta && l == 0 {
					dataSides = append(dataSides, sideId)
				}
			}
		}
	}
	start := time.Now()
	sp, err := PutSp(ctx, cli, cl, spec)
	if err != nil {
		t.Fatalf("PutSp: %v", err)
	}
	ref := sp.Ref
	tdSpecs := make([]TdSpec, 0, common.MaxTdCntPerSp)
	for i := 0; i < common.MaxTdCntPerSp; i++ {
		tdSpecs = append(tdSpecs,
			TdSpec{Name: fmt.Sprintf("td%d", i), Size: 1 << 30})
	}
	tds, err := PutTds(ctx, cli, cl, ref, tdSpecs)
	if err != nil {
		t.Fatalf("PutTds: %v", err)
	}
	for s := 0; s < common.MaxSsCntPerSp; s++ {
		ssSpec := SsSpec{Nqn: fmt.Sprintf("nqn.2024-01.io.dnv-it:ss%d", s)}
		for n := 0; n < common.MaxNsCntPerSs; n++ {
			ssSpec.Namespaces = append(ssSpec.Namespaces, NsSpec{
				NsIdx: uint32(n + 1),
				TdId:  tds[s*common.MaxNsCntPerSs+n].Id,
			})
		}
		if _, err := PutSs(ctx, cli, cl, ref, ssSpec); err != nil {
			t.Fatalf("PutSs %d: %v", s, err)
		}
	}
	for i := 0; i < common.MaxCloneCntPerSp; i++ {
		cloneSpec := CloneSpec{
			Name: fmt.Sprintf("c%d", i), DstTd: tds[i].Name,
		}
		for l := 0; l < legCnt; l++ {
			cloneSpec.Legs = append(cloneSpec.Legs, CloneLegSpec{
				LegIdx: uint32(l), DnId: uint64(l + 1),
			})
		}
		if _, err := PutClone(ctx, cli, cl, ref, cloneSpec); err != nil {
			t.Fatalf("PutClone %d: %v", i, err)
		}
	}
	for i := 0; i < common.MaxXferCntPerSp; i++ {
		if _, err := PutXfer(ctx, cli, cl, ref, XferSpec{
			Name: fmt.Sprintf("x%d", i), OriNqn: "nqn.2024-01.io.dnv-it:ss0",
			OriNsIdx: uint32(i + 1),
		}); err != nil {
			t.Fatalf("PutXfer %d: %v", i, err)
		}
	}
	for i := 0; i < migrCnt; i++ {
		if _, err := PutMigr(ctx, cli, cl, ref, MigrSpec{
			Name: fmt.Sprintf("m%d", i), SrcSide: dataSides[i],
			DstDn: uint64(legCnt + i + 1), CutoverPending: true,
		}); err != nil {
			t.Fatalf("PutMigr %d: %v", i, err)
		}
	}
	t.Logf("planted the widest pool in %v", time.Since(start))

	state, err := model.LoadSp(ctx, cli, cl.Id, ref.Name)
	if err != nil {
		t.Fatalf("LoadSp: %v", err)
	}
	if len(state.Missing) != 0 {
		t.Fatalf("LoadSp found keys missing: %v", state.Missing)
	}
	if len(state.Slices) != sliceCnt || len(state.Cntlrs) !=
		common.MaxCntlrCntPerSp || len(state.Tds) != common.MaxTdCntPerSp ||
		len(state.Subsystems) != common.MaxSsCntPerSp ||
		len(state.Clones) != common.MaxCloneCntPerSp ||
		len(state.Xfers) != common.MaxXferCntPerSp ||
		len(state.Migrs) != migrCnt {
		t.Fatalf("the pool holds %d slices, %d cntlrs, %d tds, %d "+
			"subsystems, %d clones, %d transfers, %d migrations",
			len(state.Slices), len(state.Cntlrs), len(state.Tds),
			len(state.Subsystems), len(state.Clones), len(state.Xfers),
			len(state.Migrs))
	}
	moved := 0
	for _, slice := range state.Slices {
		for _, grp := range append(append([]*pb.Group(nil),
			slice.GetMetaGrpList()...), slice.GetDataGrpList()...) {
			if len(grp.GetLegList()) != common.MaxAllocLegPerGrp {
				t.Fatalf("group %#x has %d legs", grp.GetGrpId(),
					len(grp.GetLegList()))
			}
			if len(grp.GetLegList()[0].GetSideList()) == 2 {
				moved++
			}
		}
	}
	if moved != migrCnt {
		t.Fatalf("%d legs carry a second side, want %d", moved, migrCnt)
	}
	for _, subsystem := range state.Subsystems {
		if len(subsystem.GetNsList()) != common.MaxNsCntPerSs {
			t.Fatalf("subsystem %v", subsystem)
		}
	}
	for name, clone := range state.Clones {
		grp := clone.GetMetaGrp()
		if len(grp.GetLegList()) != common.MaxAllocLegPerGrp {
			t.Fatalf("clone %s's group has %d legs", name,
				len(grp.GetLegList()))
		}
	}
	for addr, cn := range state.CnByAddr {
		if cn.GetFreeExtCnt() != free-sp.Footprint {
			t.Fatalf("cn %s has %d free, want %d", addr,
				cn.GetFreeExtCnt(), free-sp.Footprint)
		}
	}
	// dn 1 holds a side of every group of every slice and of every clone.
	for addr, dn := range state.DnByAddr {
		want := uint64(2*sliceCnt + common.MaxCloneCntPerSp)
		if addr == dnAddr(1) && uint64(len(dn.GetSidePtrList())) != want {
			t.Fatalf("dn 1 holds %d sides, want %d",
				len(dn.GetSidePtrList()), want)
		}
	}
}

// equalU64 reports whether a list is exactly want.
func equalU64(list []uint64, want ...uint64) bool {
	if len(list) != len(want) {
		return false
	}
	for i := range list {
		if list[i] != want[i] {
			return false
		}
	}
	return true
}

// equalPtrs reports whether two side pointer lists are equal in order.
func equalPtrs(list []*pb.SidePointer, want []*pb.SidePointer) bool {
	if len(list) != len(want) {
		return false
	}
	for i := range list {
		if !proto.Equal(list[i], want[i]) {
			return false
		}
	}
	return true
}

// equalTrConfs reports whether two transport lists are equal in order.
func equalTrConfs(list []*pb.NvmeTrConf, want []*pb.NvmeTrConf) bool {
	if len(list) != len(want) {
		return false
	}
	for i := range list {
		if !proto.Equal(list[i], want[i]) {
			return false
		}
	}
	return true
}

// chunkList spells a chunk list the way workerctl's get-sp prints it: a
// clone chunk as its "{src_slice_idx}:{bm_idx}" pair, a migration chunk as
// its sequence number.
func chunkList(chunks []model.BmChunk, pair bool) string {
	parts := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		if pair {
			parts = append(parts, fmt.Sprintf("%d:%d", chunk.SliceIdx,
				chunk.Idx))
			continue
		}
		parts = append(parts, fmt.Sprintf("%d", chunk.Idx))
	}
	return strings.Join(parts, ",")
}
