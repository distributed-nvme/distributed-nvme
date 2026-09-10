package gateway

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is gateway.md §9.3 for architecture.md §8.4–§8.6: the eight
// storage-pool RPCs of storagepool.go and the three cntlr mutators of
// cntlr.go, driven against a real etcd through a Server built directly.
//
// Every assertion is on the STORE, not on the handler's reply alone: the
// contract of a mutator is the exact set of keys it writes (§5.3), the exact
// DN/CN budgets it moves (§5.6) and the exactly-once revision bump that makes
// the change visible to a worker (§5.5). A reply that looks right over a write
// set that is wrong is the failure mode these tests exist to catch.
//
// The two Inspect RPCs of cntlr.go are not here: they end in an agent call and
// belong to the §9.4 agent-path suite.

// ---------------------------------------------------------------------------
// The fixture: one cluster, twelve disk nodes and five controller nodes
// ---------------------------------------------------------------------------

const (
	// sptExtSize / sptBlockSize are the cluster geometry every expected
	// meta_blocks / data_blocks below is computed from (§3.6).
	sptExtSize   = uint64(1) << 30
	sptBlockSize = uint64(1) << 20
	// sptDnFree / sptCnFree sit in DN bin 1 (levels 1/16/256/4096), so a
	// charge of a few extents moves the capacity key without moving the bin
	// — which is what makes "the old key is gone, the new key is there" a
	// meaningful assertion (§5.6, §6.2).
	sptDnFree = uint64(64)
	sptCnFree = uint64(64)
	sptDnCnt  = 12
	sptCnCnt  = 5

	// The default SP: two slices, two cntlrs, md-raid1 groups. Two legs per
	// group over two slices is eight sides on eight DISTINCT disk nodes,
	// which is the §6.5 property with the most room to go wrong.
	sptSliceCnt = 2
	sptCntlrCnt = 2
	sptInitExt  = uint64(4)
	sptSpName   = "pool0"
	// sptFootprint is Σ ext_cnt over every group of every slice: what ONE
	// cntlr's CN reserves for the SP (§8.4, §6.5).
	sptFootprint = uint64(sptSliceCnt) * (1 + sptInitExt)

	// §3.6 for this geometry: a 1 GiB md-raid1 group is 1024 pool blocks of
	// which 3 are meta (superblock + one bitmap block + health), and a 4 GiB
	// one is 4096 blocks with the same 3.
	sptMetaBlocks  = uint64(3)
	sptMetaGrpData = uint64(1021)
	sptDataGrpData = uint64(4093)
)

// sptEnvSeq numbers the cluster each environment gets, so that two tests — and
// two iterations of one test under `go test -count=2` — never share a
// cluster_id: it is fnv64a(name ‖ creation_epoch) and both halves move (§5.2).
var sptEnvSeq atomic.Uint64

// sptEnv is one test's world: a cluster with its three globals, a set of disk
// and controller nodes with consistent capacity and revision keys, and a
// Server wired straight to the fixture etcd (no listener — §9.3).
type sptEnv struct {
	t       *testing.T
	ctx     context.Context
	cli     *etcdutil.Client
	srv     *Server
	name    string
	cid     uint64
	cc      *pb.ClusterConf
	dnAddrs []string
	cnAddrs []string
}

// sptTrConf is one node's transport configuration, distinct per node so that a
// Cntlr's, a Side's and a CdcEntry's copy can be checked member by member.
func sptTrConf(addrPort string) *pb.NvmeTrConf {
	return &pb.NvmeTrConf{
		TrType:  "tcp",
		AdrFam:  "ipv4",
		TrAddr:  addrPort,
		TrSvcId: "4420",
	}
}

// sptNewEnv writes the whole fixture and returns it. The ClusterConf carries
// no redund_conf, so decision D-C's structural default applies unless a
// request asks for md-raid1: an SP created with no bdev_conf at all is
// redund_none.
func sptNewEnv(t *testing.T, dnCnt int, cnCnt int, cnFree uint64) *sptEnv {
	t.Helper()
	cli := newTestClient(t)
	seq := sptEnvSeq.Add(1)
	name := fmt.Sprintf("spt-%d", seq)
	cc := &pb.ClusterConf{
		CreationEpoch: seq,
		BdevConf: &pb.BdevConf{
			DmPoolConf: &pb.DmPoolConf{DataBlockSize: sptBlockSize},
		},
		DnBinConf: &pb.DnBinConf{ExtentSize: sptExtSize},
	}
	env := &sptEnv{
		t:    t,
		ctx:  context.Background(),
		cli:  cli,
		srv:  NewServer(cli),
		name: name,
		cid:  model.ClusterId(name, seq),
		cc:   cc,
	}
	mustPut(t, cli, model.ClusterConfKey(name), cc)
	dnBucket := zeroBucket()
	cnBucket := zeroBucket()
	for idx := 0; idx < dnCnt; idx++ {
		addrPort := fmt.Sprintf("dn-%02d:9000", idx)
		env.dnAddrs = append(env.dnAddrs, addrPort)
		env.putDn(addrPort, uint64(idx)+1, uint32(idx), sptDnFree)
		dnBucket[idx]++
	}
	for idx := 0; idx < cnCnt; idx++ {
		addrPort := fmt.Sprintf("cn-%02d:9000", idx)
		env.cnAddrs = append(env.cnAddrs, addrPort)
		env.putCn(addrPort, uint64(idx)+1, uint32(idx), cnFree)
		cnBucket[idx]++
	}
	mustPut(t, cli, model.DnGlobalKey(env.cid), &pb.DnGlobal{
		NextId:      uint64(dnCnt) + 1,
		ShardBucket: dnBucket,
	})
	mustPut(t, cli, model.CnGlobalKey(env.cid), &pb.CnGlobal{
		NextId:      uint64(cnCnt) + 1,
		ShardBucket: cnBucket,
	})
	// A fresh SpGlobal: next_id starts at 1 and shard_bucket is
	// ShardBucketSize zeros (§5.4), exactly as CreateCluster leaves it.
	mustPut(t, cli, model.SpGlobalKey(env.cid), &pb.SpGlobal{
		NextId:      1,
		ShardBucket: zeroBucket(),
	})
	return env
}

// putDn writes one DnConf, the capacity key the §5.6 presence rule implies for
// it, and its DnRev at revision 1.
func (e *sptEnv) putDn(
	addrPort string,
	dnId uint64,
	shard uint32,
	freeExt uint64,
) {
	e.t.Helper()
	mustPut(e.t, e.cli, model.DnConfKey(e.cid, addrPort), &pb.DnConf{
		DnId:        dnId,
		ShardCode:   shard,
		NvmeTrConf:  sptTrConf(addrPort),
		Location:    addrPort,
		TotalExtCnt: 1024,
		FreeExtCnt:  freeExt,
	})
	mustPut(e.t, e.cli, model.DnRevKey(shard, e.cid, dnId), &pb.DnRev{
		AddrPort: addrPort,
		Revision: 1,
	})
	if key := e.dnCapacityKey(addrPort, freeExt); key != "" {
		mustPut(e.t, e.cli, key, &pb.DnCapacity{Location: addrPort})
	}
}

// putCn writes one CnConf, its capacity key and its CnRev at revision 1.
func (e *sptEnv) putCn(
	addrPort string,
	cnId uint64,
	shard uint32,
	freeExt uint64,
) {
	e.t.Helper()
	mustPut(e.t, e.cli, model.CnConfKey(e.cid, addrPort), &pb.CnConf{
		CnId:        cnId,
		ShardCode:   shard,
		NvmeTrConf:  sptTrConf(addrPort),
		Location:    addrPort,
		TotalExtCnt: 1024,
		FreeExtCnt:  freeExt,
	})
	mustPut(e.t, e.cli, model.CnRevKey(shard, e.cid, cnId), &pb.CnRev{
		AddrPort: addrPort,
		Revision: 1,
	})
	mustPut(
		e.t, e.cli,
		model.CnCapacityKey(e.cid, freeExt, addrPort),
		&pb.CnCapacity{Location: addrPort},
	)
}

// dnCapacityKey is the key a DN with freeExt free extents implies, or "" when
// it implies none (§5.6).
func (e *sptEnv) dnCapacityKey(addrPort string, freeExt uint64) string {
	binIdx, ok := model.DnBinIdx(freeExt, e.cc.GetDnBinConf())
	if !ok {
		return ""
	}
	return model.DnCapacityKey(e.cid, binIdx, freeExt, addrPort)
}

// ---------------------------------------------------------------------------
// Reading the store back
// ---------------------------------------------------------------------------

func (e *sptEnv) get(key string, msg proto.Message) {
	e.t.Helper()
	found, err := e.cli.Get(e.ctx, key, msg)
	if err != nil {
		e.t.Fatalf("Get %s: %v", key, err)
	}
	if !found {
		e.t.Fatalf("Get %s: not found", key)
	}
}

func (e *sptEnv) exists(key string) bool {
	e.t.Helper()
	found, err := e.cli.Get(e.ctx, key, &pb.SpName{})
	if err != nil {
		e.t.Fatalf("Get %s: %v", key, err)
	}
	return found
}

func (e *sptEnv) spConf(spName string) *pb.SpConf {
	conf := &pb.SpConf{}
	e.get(model.SpConfKey(e.cid, spName), conf)
	return conf
}

func (e *sptEnv) spGlobal() *pb.SpGlobal {
	global := &pb.SpGlobal{}
	e.get(model.SpGlobalKey(e.cid), global)
	return global
}

func (e *sptEnv) slice(spId uint64, sliceId uint64) *pb.Slice {
	slice := &pb.Slice{}
	e.get(model.SliceKey(e.cid, spId, sliceId), slice)
	return slice
}

func (e *sptEnv) cntlr(spId uint64, cntlrId uint64) *pb.Cntlr {
	cntlr := &pb.Cntlr{}
	e.get(model.CntlrKey(e.cid, spId, cntlrId), cntlr)
	return cntlr
}

func (e *sptEnv) dnConf(addrPort string) *pb.DnConf {
	dn := &pb.DnConf{}
	e.get(model.DnConfKey(e.cid, addrPort), dn)
	return dn
}

func (e *sptEnv) cnConf(addrPort string) *pb.CnConf {
	cn := &pb.CnConf{}
	e.get(model.CnConfKey(e.cid, addrPort), cn)
	return cn
}

// spRev is the SP's stored revision — the token every SP mutator consumes and
// bumps (§5.5).
func (e *sptEnv) spRev(shard uint32, spId uint64) uint64 {
	rev := &pb.SpRev{}
	e.get(model.SpRevKey(shard, e.cid, spId), rev)
	return rev.GetRevision()
}

func (e *sptEnv) dnRev(addrPort string) uint64 {
	dn := e.dnConf(addrPort)
	rev := &pb.DnRev{}
	e.get(model.DnRevKey(dn.GetShardCode(), e.cid, dn.GetDnId()), rev)
	return rev.GetRevision()
}

func (e *sptEnv) cnRev(addrPort string) uint64 {
	cn := e.cnConf(addrPort)
	rev := &pb.CnRev{}
	e.get(model.CnRevKey(cn.GetShardCode(), e.cid, cn.GetCnId()), rev)
	return rev.GetRevision()
}

func (e *sptEnv) cdcEntry(
	shard uint32,
	spId uint64,
	ssId uint64,
) *pb.CdcEntry {
	entry := &pb.CdcEntry{}
	e.get(model.CdcEntryKey(e.cid, shard, spId, ssId), entry)
	return entry
}

// dump is every key of THIS cluster with its raw value. Cluster-scoped keys
// all carry the cluster_id as one field (§5.3), so the id's rendering is an
// exact filter on a store several tests share.
func (e *sptEnv) dump() map[string][]byte {
	e.t.Helper()
	kvs, _, err := e.cli.Range(e.ctx, common.DnvPrefix)
	if err != nil {
		e.t.Fatalf("Range: %v", err)
	}
	tag := fmt.Sprintf(common.IdKeyFmt, e.cid)
	out := make(map[string][]byte, len(kvs))
	for _, kv := range kvs {
		if !strings.Contains(kv.Key, tag) {
			continue
		}
		out[kv.Key] = kv.Value
	}
	return out
}

// ---------------------------------------------------------------------------
// Driving the handlers
// ---------------------------------------------------------------------------

// sptSpec is one CreateStoragePool request's shape. Tests that only need an SP
// to exist use the cheapest one that still has the structure they assert on.
type sptSpec struct {
	name     string
	cntlrCnt uint32
	sliceCnt uint32
	initExt  uint64
	raid1    bool
	slots    []uint32
}

// sptDefaultSpec is the two-slice, two-cntlr md-raid1 SP most tests use.
func sptDefaultSpec(name string) sptSpec {
	return sptSpec{
		name:     name,
		cntlrCnt: sptCntlrCnt,
		sliceCnt: sptSliceCnt,
		initExt:  sptInitExt,
		raid1:    true,
	}
}

// sptSmallSpec is one cntlr, one slice, one extent and no redundancy: the
// cheapest SP that still exercises the whole write path, for the tests whose
// subject is a list or a lookup rather than the geometry.
func sptSmallSpec(name string) sptSpec {
	return sptSpec{name: name, cntlrCnt: 1, sliceCnt: 1, initExt: 1}
}

// req builds the CreateStoragePoolRequest a spec describes. The
// event_threshold is deliberately non-default: GW11 says it is stored
// verbatim, and a zero one could not tell that apart from a dropped field.
func (spec sptSpec) req(clusterName string) *pb.CreateStoragePoolRequest {
	req := &pb.CreateStoragePoolRequest{
		ClusterName:    clusterName,
		SpName:         spec.name,
		EventThreshold: &pb.EventThreshold{SideUnhealthy: 3, LegUnhealthy: 9},
		CntlidSlotList: spec.slots,
		CntlrCnt:       spec.cntlrCnt,
		SliceCnt:       spec.sliceCnt,
		InitExtCnt:     spec.initExt,
	}
	if spec.raid1 {
		req.BdevConf = &pb.BdevConf{
			RedundConf: &pb.RedundConf{
				RedunKind: &pb.RedundConf_RedundMdRaid1{
					RedundMdRaid1: &pb.RedundMdRaid1{},
				},
			},
		}
	}
	return req
}

// createSp runs CreateStoragePool and fails the test if it does not succeed.
func (e *sptEnv) createSp(spec sptSpec) uint64 {
	e.t.Helper()
	reply, err := e.srv.CreateStoragePool(e.ctx, spec.req(e.name))
	if err != nil {
		e.t.Fatalf("CreateStoragePool %s: %v", spec.name, err)
	}
	return reply.GetSpId()
}

// sptWantCode asserts the GW7 code of a refusal. It is a Fatal: a test that
// expected a refusal and got the wrong one has nothing left to assert.
func sptWantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s, got nil error", want)
	}
	if got := status.Code(err); got != want {
		t.Fatalf("want %s, got %s: %v", want, got, err)
	}
}

// sptWantStale asserts GW6's token refusal: ABORTED carrying the one sentence
// §0 #7 fixes, which the integration suite greps for.
func sptWantStale(t *testing.T, err error) {
	t.Helper()
	sptWantCode(t, err, codes.Aborted)
	if status.Convert(err).Message() != msgStaleRevision {
		t.Fatalf("want %q, got %q", msgStaleRevision,
			status.Convert(err).Message())
	}
}

// sptSideWalk is every side of an SP with the group it belongs to, which is
// what the DN accounting assertions are computed from.
type sptSideWalk struct {
	AddrPort string
	ExtCnt   uint64
	LegId    uint64
	SideId   uint64
}

// walkSides collects every side of every group of every slice of the SP.
func (e *sptEnv) walkSides(conf *pb.SpConf) []sptSideWalk {
	e.t.Helper()
	var out []sptSideWalk
	for _, sliceId := range conf.GetSliceIdList() {
		slice := e.slice(conf.GetSpId(), sliceId)
		for _, grp := range allGroups(slice) {
			for _, leg := range allLegs(grp) {
				for _, side := range leg.GetSideList() {
					out = append(out, sptSideWalk{
						AddrPort: side.GetAddrPort(),
						ExtCnt:   grp.GetExtCnt(),
						LegId:    leg.GetLegId(),
						SideId:   side.GetSideId(),
					})
				}
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// CreateStoragePool (§8.4)
// ---------------------------------------------------------------------------

// TestCreateStoragePoolWriteSet pins the whole write set of §8.4: the SpConf
// with decision D-C's merged bdev_conf and the §8.4 default slot list, the
// sp_id_to_name reverse key, an SpRev CREATED at revision 1 (never bumped),
// one Cntlr per CN pick with exactly one primary and pairwise distinct cntlid
// slots (§11.8), one Slice per slice carrying its meta and data group, every
// side unprovisioned ([D15]) on a DN no other leg of the SP uses (§6.5), and
// the cluster's SpGlobal advanced by exactly one id and one bucket slot
// (GW12).
func TestCreateStoragePoolWriteSet(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	reply, err := env.srv.CreateStoragePool(
		env.ctx, sptDefaultSpec(sptSpName).req(env.name))
	if err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	// The first SP of a fresh cluster: next_id starts at 1 (§5.4).
	if reply.GetSpId() != 1 {
		t.Fatalf("sp_id: got %d, want 1", reply.GetSpId())
	}
	spId := reply.GetSpId()
	conf := env.spConf(sptSpName)
	if conf.GetSpId() != spId {
		t.Errorf("sp_conf.sp_id: got %d, want %d", conf.GetSpId(), spId)
	}
	// An empty shard_bucket picks the smallest value, first index on ties.
	if conf.GetShardCode() != 0 {
		t.Errorf("shard_code: got %d, want 0", conf.GetShardCode())
	}
	if conf.GetNextDevId() != 1 {
		t.Errorf("next_dev_id: got %d, want 1", conf.GetNextDevId())
	}
	if conf.GetSpLevel() != pb.SpLevel_SP_LEVEL_READWRITE {
		t.Errorf("sp_level: got %v", conf.GetSpLevel())
	}
	if conf.GetDeleting() {
		t.Errorf("deleting must be false on a fresh SP")
	}
	// D-C: the request's redund_md_raid1 wins the kind, the cluster's
	// dm_pool_conf.data_block_size carries over, and members left zero in
	// both stay zero — model.GroupBlocks resolves them identically.
	wantBdev := &pb.BdevConf{
		DmPoolConf:  &pb.DmPoolConf{DataBlockSize: sptBlockSize},
		DmRaid0Conf: &pb.DmRaid0Conf{},
		RedundConf: &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{},
			},
		},
	}
	if !proto.Equal(conf.GetBdevConf(), wantBdev) {
		t.Errorf("bdev_conf: got %v, want %v", conf.GetBdevConf(), wantBdev)
	}
	// GW11: event_threshold is stored verbatim, defaults resolved at use.
	wantThreshold := &pb.EventThreshold{SideUnhealthy: 3, LegUnhealthy: 9}
	if !proto.Equal(conf.GetEventThreshold(), wantThreshold) {
		t.Errorf("event_threshold: got %v", conf.GetEventThreshold())
	}
	// §8.4: an empty cntlid_slot_list defaults to every slot a CN has.
	if len(conf.GetCntlidSlotList()) != common.CnCntlidSlotCnt {
		t.Fatalf("cntlid_slot_list: got %v", conf.GetCntlidSlotList())
	}
	for idx, slot := range conf.GetCntlidSlotList() {
		if slot != uint32(idx) {
			t.Errorf("cntlid_slot_list[%d]: got %d", idx, slot)
		}
	}
	for _, list := range [][]string{
		conf.GetTdNameList(), conf.GetNqnList(), conf.GetCloneNameList(),
		conf.GetXferNameList(), conf.GetMigrNameList(),
	} {
		if len(list) != 0 {
			t.Errorf("a fresh SP holds no named objects: %v", list)
		}
	}
	if len(conf.GetCntlrIdList()) != sptCntlrCnt {
		t.Fatalf("cntlr_id_list: got %v", conf.GetCntlrIdList())
	}
	if len(conf.GetSliceIdList()) != sptSliceCnt {
		t.Fatalf("slice_id_list: got %v", conf.GetSliceIdList())
	}

	name := &pb.SpName{}
	env.get(model.SpNameKey(env.cid, spId), name)
	if name.GetSpName() != sptSpName {
		t.Errorf("sp_id_to_name: got %q", name.GetSpName())
	}
	// §5.5: the rev key is CREATED at 1 and carries the sp_name a watching
	// worker forms the SpConf key from [D10]; CreateStoragePool bumps
	// nothing.
	rev := &pb.SpRev{}
	env.get(model.SpRevKey(0, env.cid, spId), rev)
	if rev.GetRevision() != 1 || rev.GetSpName() != sptSpName {
		t.Errorf("sp_rev: got %v, want {%q 1}", rev, sptSpName)
	}

	// One Cntlr per CN pick: exactly one primary, distinct slots, distinct
	// CNs, each carrying its CN's own transport conf.
	primaries := 0
	seenSlot := make(map[uint32]bool)
	seenCn := make(map[string]bool)
	for idx, cntlrId := range conf.GetCntlrIdList() {
		cntlr := env.cntlr(spId, cntlrId)
		if cntlr.GetPrimary() {
			primaries++
		}
		if cntlr.GetPrimary() != (idx == 0) {
			t.Errorf("cntlr %d: primary %v at index %d",
				cntlrId, cntlr.GetPrimary(), idx)
		}
		if cntlr.GetDisabled() {
			t.Errorf("cntlr %d must be enabled from birth", cntlrId)
		}
		if cntlr.GetErrEpoch() != 0 {
			t.Errorf("cntlr %d: err_epoch %d", cntlrId, cntlr.GetErrEpoch())
		}
		// §11.8: cntlid_slot_list[idx] in pick order.
		if cntlr.GetCntlidSlot() != uint32(idx) {
			t.Errorf("cntlr %d: cntlid_slot %d, want %d",
				cntlrId, cntlr.GetCntlidSlot(), idx)
		}
		if seenSlot[cntlr.GetCntlidSlot()] {
			t.Errorf("cntlid_slot %d used twice", cntlr.GetCntlidSlot())
		}
		seenSlot[cntlr.GetCntlidSlot()] = true
		if seenCn[cntlr.GetAddrPort()] {
			t.Errorf("two cntlrs of one SP on CN %q", cntlr.GetAddrPort())
		}
		seenCn[cntlr.GetAddrPort()] = true
		if !proto.Equal(
			cntlr.GetNvmeTrConf(), sptTrConf(cntlr.GetAddrPort()),
		) {
			t.Errorf("cntlr %d: nvme_tr_conf %v", cntlrId,
				cntlr.GetNvmeTrConf())
		}
	}
	if primaries != 1 {
		t.Errorf("primary cntlr count: got %d, want 1", primaries)
	}

	// One Slice per slice, each with one meta group of 1 extent — the first
	// rung of the §8.5 ladder — and one data group of init_ext_cnt.
	seenDn := make(map[string]bool)
	for idx, sliceId := range conf.GetSliceIdList() {
		slice := env.slice(spId, sliceId)
		if slice.GetSliceIdx() != uint32(idx) {
			t.Errorf("slice %d: slice_idx %d, want %d",
				sliceId, slice.GetSliceIdx(), idx)
		}
		if len(slice.GetMetaGrpList()) != 1 ||
			len(slice.GetDataGrpList()) != 1 {
			t.Fatalf("slice %d: %d meta / %d data groups", sliceId,
				len(slice.GetMetaGrpList()), len(slice.GetDataGrpList()))
		}
		for _, want := range []struct {
			grp        *pb.Group
			extCnt     uint64
			dataBlocks uint64
		}{
			{slice.GetMetaGrpList()[0], 1, sptMetaGrpData},
			{slice.GetDataGrpList()[0], sptInitExt, sptDataGrpData},
		} {
			grp := want.grp
			if grp.GetExtCnt() != want.extCnt {
				t.Errorf("group %d: ext_cnt %d, want %d",
					grp.GetGrpId(), grp.GetExtCnt(), want.extCnt)
			}
			if grp.GetMetaBlocks() != sptMetaBlocks ||
				grp.GetDataBlocks() != want.dataBlocks {
				t.Errorf("group %d: %d/%d blocks, want %d/%d",
					grp.GetGrpId(), grp.GetMetaBlocks(), grp.GetDataBlocks(),
					sptMetaBlocks, want.dataBlocks)
			}
			if len(grp.GetSpareLegList()) != 0 {
				t.Errorf("group %d: a new group has no spare leg",
					grp.GetGrpId())
			}
			// md-raid1 is two legs (§6.5).
			if len(grp.GetLegList()) != 2 {
				t.Fatalf("group %d: %d legs, want 2",
					grp.GetGrpId(), len(grp.GetLegList()))
			}
			for legIdx, leg := range grp.GetLegList() {
				if leg.GetLegIdx() != uint32(legIdx) {
					t.Errorf("leg %d: leg_idx %d, want %d",
						leg.GetLegId(), leg.GetLegIdx(), legIdx)
				}
				if len(leg.GetSideList()) != 1 {
					t.Fatalf("leg %d: %d sides, want 1",
						leg.GetLegId(), len(leg.GetSideList()))
				}
				side := leg.GetSideList()[0]
				// [D15]: only the sp-worker flips it.
				if side.GetProvisioned() {
					t.Errorf("side %d must be written unprovisioned",
						side.GetSideId())
				}
				if side.GetErrEpoch() != 0 {
					t.Errorf("side %d: err_epoch %d",
						side.GetSideId(), side.GetErrEpoch())
				}
				// Every side is exported under cntlid_slot_list[0].
				if side.GetCntlidSlot() != 0 {
					t.Errorf("side %d: cntlid_slot %d, want 0",
						side.GetSideId(), side.GetCntlidSlot())
				}
				if !proto.Equal(
					side.GetNvmeTrConf(), sptTrConf(side.GetAddrPort()),
				) {
					t.Errorf("side %d: nvme_tr_conf %v",
						side.GetSideId(), side.GetNvmeTrConf())
				}
				if seenDn[side.GetAddrPort()] {
					t.Errorf("DN %q carries two legs of the SP",
						side.GetAddrPort())
				}
				seenDn[side.GetAddrPort()] = true
			}
		}
	}
	// Two slices × (meta + data) × two legs.
	if len(seenDn) != 2*sptSliceCnt*2 {
		t.Errorf("distinct DNs: got %d, want %d", len(seenDn), 2*sptSliceCnt*2)
	}

	// GW12: one id drawn, one bucket slot taken.
	global := env.spGlobal()
	if global.GetNextId() != 2 {
		t.Errorf("sp_global.next_id: got %d, want 2", global.GetNextId())
	}
	if bucketSum(global.GetShardBucket()) != 1 ||
		global.GetShardBucket()[0] != 1 {
		t.Errorf("sp_global.shard_bucket: sum %d, [0] %d",
			bucketSum(global.GetShardBucket()), global.GetShardBucket()[0])
	}
}

// TestCreateStoragePoolNodeAccounting pins the §5.6 / §5.5 half of §8.4: every
// picked DN loses exactly its group's extents and gains exactly one side
// pointer, every cntlr's CN loses the SP's whole footprint and gains exactly
// one cntlr pointer, both capacity keys move to the new free count, and every
// touched node's revision is bumped exactly once — while a node nothing picked
// is byte-for-byte where it was.
func TestCreateStoragePoolNodeAccounting(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	before := env.dump()
	spId := env.createSp(sptDefaultSpec(sptSpName))
	conf := env.spConf(sptSpName)

	touchedDn := make(map[string]bool)
	for _, walk := range env.walkSides(conf) {
		touchedDn[walk.AddrPort] = true
		dn := env.dnConf(walk.AddrPort)
		wantFree := sptDnFree - walk.ExtCnt
		if dn.GetFreeExtCnt() != wantFree {
			t.Errorf("dn %q: free_ext_cnt %d, want %d",
				walk.AddrPort, dn.GetFreeExtCnt(), wantFree)
		}
		wantPtr := []*pb.SidePointer{{
			SpId: spId, LegId: walk.LegId, SideId: walk.SideId,
		}}
		if len(dn.GetSidePtrList()) != 1 ||
			!proto.Equal(dn.GetSidePtrList()[0], wantPtr[0]) {
			t.Errorf("dn %q: side_ptr_list %v, want %v",
				walk.AddrPort, dn.GetSidePtrList(), wantPtr)
		}
		if env.exists(env.dnCapacityKey(walk.AddrPort, sptDnFree)) {
			t.Errorf("dn %q: the capacity key of the old free count survived",
				walk.AddrPort)
		}
		if !env.exists(env.dnCapacityKey(walk.AddrPort, wantFree)) {
			t.Errorf("dn %q: no capacity key for free %d",
				walk.AddrPort, wantFree)
		}
		if got := env.dnRev(walk.AddrPort); got != 2 {
			t.Errorf("dn %q: revision %d, want exactly one bump to 2",
				walk.AddrPort, got)
		}
	}

	touchedCn := make(map[string]bool)
	for _, cntlrId := range conf.GetCntlrIdList() {
		addrPort := env.cntlr(spId, cntlrId).GetAddrPort()
		touchedCn[addrPort] = true
		cn := env.cnConf(addrPort)
		wantFree := sptCnFree - sptFootprint
		if cn.GetFreeExtCnt() != wantFree {
			t.Errorf("cn %q: free_ext_cnt %d, want %d",
				addrPort, cn.GetFreeExtCnt(), wantFree)
		}
		wantPtr := &pb.CntlrPointer{SpId: spId, CntlrId: cntlrId}
		if len(cn.GetCntlrPtrList()) != 1 ||
			!proto.Equal(cn.GetCntlrPtrList()[0], wantPtr) {
			t.Errorf("cn %q: cntlr_ptr_list %v, want [%v]",
				addrPort, cn.GetCntlrPtrList(), wantPtr)
		}
		if env.exists(model.CnCapacityKey(env.cid, sptCnFree, addrPort)) {
			t.Errorf("cn %q: the capacity key of the old free count survived",
				addrPort)
		}
		if !env.exists(model.CnCapacityKey(env.cid, wantFree, addrPort)) {
			t.Errorf("cn %q: no capacity key for free %d", addrPort, wantFree)
		}
		if got := env.cnRev(addrPort); got != 2 {
			t.Errorf("cn %q: revision %d, want exactly one bump to 2",
				addrPort, got)
		}
	}

	// A node the allocator did not pick must be untouched down to its bytes:
	// an allocation writes only what it charges (§5.6).
	after := env.dump()
	for _, addrPort := range env.dnAddrs {
		if touchedDn[addrPort] {
			continue
		}
		key := model.DnConfKey(env.cid, addrPort)
		if !bytes.Equal(before[key], after[key]) {
			t.Errorf("dn %q was rewritten without being picked", addrPort)
		}
		if got := env.dnRev(addrPort); got != 1 {
			t.Errorf("dn %q: revision %d, want 1", addrPort, got)
		}
	}
	for _, addrPort := range env.cnAddrs {
		if touchedCn[addrPort] {
			continue
		}
		key := model.CnConfKey(env.cid, addrPort)
		if !bytes.Equal(before[key], after[key]) {
			t.Errorf("cn %q was rewritten without being picked", addrPort)
		}
		if got := env.cnRev(addrPort); got != 1 {
			t.Errorf("cn %q: revision %d, want 1", addrPort, got)
		}
	}
}

// TestCreateStoragePoolIdSequence pins decision D-D: every per-SP id comes out
// of the single next_id counter in one fixed order — every cntlr_id in pick
// order, then per slice its slice_id, then the META group (grp_id, then per
// leg leg_id and its side_id) and the DATA group the same way — and the
// counter is committed exactly once. A retried attempt re-reads next_id and
// must reproduce this exact write set, which is only true if the order is
// fixed.
//
// It then grows a slice and adds a cntlr to pin the other half of §5.4: ids
// continue from the counter and are never reused.
func TestCreateStoragePoolIdSequence(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	conf := env.spConf(sptSpName)
	seen := make(map[uint64]string)
	claim := func(id uint64, what string) {
		if prev, ok := seen[id]; ok {
			t.Errorf("id %d used for both %s and %s", id, prev, what)
		}
		seen[id] = what
	}
	var got []uint64
	for _, cntlrId := range conf.GetCntlrIdList() {
		got = append(got, cntlrId)
		claim(cntlrId, "cntlr")
	}
	for _, sliceId := range conf.GetSliceIdList() {
		got = append(got, sliceId)
		claim(sliceId, "slice")
		slice := env.slice(spId, sliceId)
		// allGroups is meta groups first, which is D-D's order.
		for _, grp := range allGroups(slice) {
			got = append(got, grp.GetGrpId())
			claim(grp.GetGrpId(), "grp")
			for _, leg := range grp.GetLegList() {
				got = append(got, leg.GetLegId())
				claim(leg.GetLegId(), "leg")
				for _, side := range leg.GetSideList() {
					got = append(got, side.GetSideId())
					claim(side.GetSideId(), "side")
				}
			}
		}
	}
	// 2 cntlrs, then per slice: slice_id, meta (grp + 2×(leg, side)), data
	// the same — ids 1…24, so next_id commits at 25.
	want := make([]uint64, 0, 24)
	for id := uint64(1); id <= 24; id++ {
		want = append(want, id)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("D-D id order:\n got %v\nwant %v", got, want)
	}
	if conf.GetNextId() != 25 {
		t.Fatalf("next_id: got %d, want 25", conf.GetNextId())
	}

	// A data grow draws grp_id, then leg_id and side_id per leg.
	growReply, err := env.srv.GrowSlice(env.ctx, &pb.GrowSliceRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		SpRev:       &pb.SpRev{Revision: 1},
		SliceId:     conf.GetSliceIdList()[0],
		ExtCnt:      1,
	})
	if err != nil {
		t.Fatalf("GrowSlice: %v", err)
	}
	if growReply.GetGrpId() != 25 {
		t.Errorf("grown grp_id: got %d, want 25", growReply.GetGrpId())
	}
	if got := env.spConf(sptSpName).GetNextId(); got != 30 {
		t.Errorf("next_id after a two-leg grow: got %d, want 30", got)
	}
	// And a cntlr draws exactly one more.
	cntlrReply, err := env.srv.CreateCntlr(env.ctx, &pb.CreateCntlrRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		SpRev:       &pb.SpRev{Revision: 2},
		CntlidSlot:  2,
	})
	if err != nil {
		t.Fatalf("CreateCntlr: %v", err)
	}
	if cntlrReply.GetCntlrId() != 30 {
		t.Errorf("new cntlr_id: got %d, want 30", cntlrReply.GetCntlrId())
	}
	if got := env.spConf(sptSpName).GetNextId(); got != 31 {
		t.Errorf("next_id after CreateCntlr: got %d, want 31", got)
	}
}

// TestCreateStoragePoolRefusals pins the refusals of §8.4 that write nothing:
// a name already taken is ALREADY_EXISTS, and each RESOURCE_EXHAUSTED path —
// too few disk nodes for the leg count, no controller node with room for the
// SP's footprint, and a cluster that has reached MaxSpCntPerCluster — leaves
// the SpGlobal and every node exactly as it found them (GW7, §6.5).
func TestCreateStoragePoolRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dnCnt  int
		cnCnt  int
		cnFree uint64
		setup  func(env *sptEnv)
		want   codes.Code
		// msg pins WHICH refusal of the code was taken, so that two paths
		// sharing one code cannot stand in for each other.
		msg string
	}{
		{
			// Eight distinct DNs are needed and only three exist, so
			// pickDns runs out of candidates before the transaction opens.
			name:   "too few disk nodes",
			dnCnt:  3,
			cnCnt:  sptCnCnt,
			cnFree: sptCnFree,
			want:   codes.ResourceExhausted,
			msg:    "disk nodes",
		},
		{
			// Every CN reserves the SP's whole footprint (§6.5).
			name:   "no controller node with room",
			dnCnt:  sptDnCnt,
			cnCnt:  sptCnCnt,
			cnFree: sptFootprint - 1,
			want:   codes.ResourceExhausted,
			msg:    "no controller node",
		},
		{
			name:   "cluster at the storage-pool ceiling",
			dnCnt:  sptDnCnt,
			cnCnt:  sptCnCnt,
			cnFree: sptCnFree,
			setup: func(env *sptEnv) {
				bucket := zeroBucket()
				bucket[0] = common.MaxSpCntPerCluster
				mustPut(env.t, env.cli, model.SpGlobalKey(env.cid),
					&pb.SpGlobal{NextId: 1, ShardBucket: bucket})
			},
			want: codes.ResourceExhausted,
			msg:  "storage pool count",
		},
		{
			name:   "name already taken",
			dnCnt:  sptDnCnt,
			cnCnt:  sptCnCnt,
			cnFree: sptCnFree,
			setup: func(env *sptEnv) {
				env.createSp(sptDefaultSpec(sptSpName))
			},
			want: codes.AlreadyExists,
			msg:  "already exists",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := sptNewEnv(t, tc.dnCnt, tc.cnCnt, tc.cnFree)
			if tc.setup != nil {
				tc.setup(env)
			}
			before := env.dump()
			_, err := env.srv.CreateStoragePool(
				env.ctx, sptDefaultSpec(sptSpName).req(env.name))
			sptWantCode(t, err, tc.want)
			if !strings.Contains(status.Convert(err).Message(), tc.msg) {
				t.Errorf("message %q does not mention %q",
					status.Convert(err).Message(), tc.msg)
			}
			after := env.dump()
			if len(before) != len(after) {
				t.Fatalf("a refusal wrote keys: %d before, %d after",
					len(before), len(after))
			}
			for key, value := range before {
				if !bytes.Equal(value, after[key]) {
					t.Errorf("a refusal rewrote %q", key)
				}
			}
		})
	}
}

// TestCreateStoragePoolValidation pins the §7 / §8.4 checks that run before
// any read, so a malformed request never touches the store (GW4).
func TestCreateStoragePoolValidation(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	for _, tc := range []struct {
		name  string
		patch func(req *pb.CreateStoragePoolRequest)
	}{
		{"empty sp_name", func(r *pb.CreateStoragePoolRequest) {
			r.SpName = ""
		}},
		{"cntlr_cnt above the maximum", func(r *pb.CreateStoragePoolRequest) {
			r.CntlrCnt = common.MaxCntlrCntPerSp + 1
		}},
		{"cntlr_cnt exceeds the slot list",
			func(r *pb.CreateStoragePoolRequest) {
				r.CntlidSlotList = []uint32{0}
				r.CntlrCnt = 2
			}},
		{"slice_cnt zero", func(r *pb.CreateStoragePoolRequest) {
			r.SliceCnt = 0
		}},
		{"slice_cnt above the maximum",
			func(r *pb.CreateStoragePoolRequest) {
				r.SliceCnt = common.MaxSliceCntPerSp + 1
			}},
		{"init_ext_cnt zero", func(r *pb.CreateStoragePoolRequest) {
			r.InitExtCnt = 0
		}},
		{"duplicate cntlid slot", func(r *pb.CreateStoragePoolRequest) {
			r.CntlidSlotList = []uint32{1, 1}
		}},
		{"cntlid slot out of range", func(r *pb.CreateStoragePoolRequest) {
			r.CntlidSlotList = []uint32{common.CnCntlidSlotCnt}
		}},
		{"non-empty bdev_feature_list",
			func(r *pb.CreateStoragePoolRequest) {
				r.BdevConf = &pb.BdevConf{
					BdevFeatureList: []*pb.BdevFeature{{}},
				}
			}},
		{"leg_unhealthy below side_unhealthy",
			func(r *pb.CreateStoragePoolRequest) {
				r.EventThreshold = &pb.EventThreshold{
					SideUnhealthy: 9, LegUnhealthy: 3,
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := sptDefaultSpec(sptSpName).req(env.name)
			tc.patch(req)
			_, err := env.srv.CreateStoragePool(env.ctx, req)
			sptWantCode(t, err, codes.InvalidArgument)
			if env.exists(model.SpConfKey(env.cid, sptSpName)) {
				t.Errorf("a §7 refusal must not create the SP")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// DeleteStoragePool (§8.4)
// ---------------------------------------------------------------------------

// TestDeleteStoragePoolFullTeardown is the accounting counterpart of
// CreateStoragePool: after a create and a delete the cluster is byte-for-byte
// where the create found it — every SP key gone, every DN and CN budget,
// pointer list and capacity key restored, the SpGlobal's bucket slot released
// — with exactly three deliberate exceptions: next_id never rewinds (GW12: a
// deleted sp_id must never come back), and each node the two RPCs touched has
// been bumped exactly twice (§5.5).
func TestDeleteStoragePoolFullTeardown(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	before := env.dump()
	spId := env.createSp(sptDefaultSpec(sptSpName))
	conf := env.spConf(sptSpName)
	touched := make(map[string]bool)
	for _, walk := range env.walkSides(conf) {
		touched[model.DnConfKey(env.cid, walk.AddrPort)] = true
	}
	for _, cntlrId := range conf.GetCntlrIdList() {
		addrPort := env.cntlr(spId, cntlrId).GetAddrPort()
		touched[model.CnConfKey(env.cid, addrPort)] = true
	}

	reply, err := env.srv.DeleteStoragePool(
		env.ctx, &pb.DeleteStoragePoolRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			SpRev:       &pb.SpRev{Revision: 1},
		})
	if err != nil {
		t.Fatalf("DeleteStoragePool: %v", err)
	}
	if reply.GetSpId() != spId {
		t.Errorf("sp_id: got %d, want %d", reply.GetSpId(), spId)
	}
	after := env.dump()
	if len(before) != len(after) {
		t.Errorf("key count: %d before the SP, %d after its teardown",
			len(before), len(after))
	}
	for key := range after {
		if _, ok := before[key]; !ok {
			t.Errorf("teardown left %q behind", key)
		}
	}
	for key, want := range before {
		got, ok := after[key]
		if !ok {
			t.Errorf("teardown removed %q, which it did not create", key)
			continue
		}
		switch {
		case key == model.SpGlobalKey(env.cid):
			old, now := &pb.SpGlobal{}, &pb.SpGlobal{}
			sptUnmarshal(t, want, old)
			sptUnmarshal(t, got, now)
			if now.GetNextId() != old.GetNextId()+1 {
				t.Errorf("sp_global.next_id: got %d, want %d",
					now.GetNextId(), old.GetNextId()+1)
			}
			if bucketSum(now.GetShardBucket()) != 0 {
				t.Errorf("sp_global.shard_bucket must be released: sum %d",
					bucketSum(now.GetShardBucket()))
			}
		case sptIsRevKey(key):
			wantRev := uint64(1)
			if touched[sptConfKeyOfRev(env, key)] {
				wantRev = 3
			}
			if gotRev := sptRevisionOf(t, got); gotRev != wantRev {
				t.Errorf("%q: revision %d, want %d", key, gotRev, wantRev)
			}
		default:
			if !bytes.Equal(want, got) {
				t.Errorf("%q did not return to its pre-create bytes", key)
			}
		}
	}
}

// sptUnmarshal decodes one raw stored value.
func sptUnmarshal(t *testing.T, raw []byte, msg proto.Message) {
	t.Helper()
	if err := proto.Unmarshal(raw, msg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
}

// sptIsRevKey reports whether a key is a DnRev or a CnRev key. Both are
// bumped by an allocation and are therefore the one class of key a full
// teardown does NOT restore (§5.5: a revision only ever grows).
func sptIsRevKey(key string) bool {
	if _, _, _, ok := model.ParseDnRevKey(key); ok {
		return true
	}
	_, _, _, ok := model.ParseCnRevKey(key)
	return ok
}

// sptConfKeyOfRev maps a rev key back to the conf key of the node it belongs
// to, which is how the teardown test tells a node the SP used from one it did
// not.
func sptConfKeyOfRev(env *sptEnv, key string) string {
	if _, _, _, ok := model.ParseDnRevKey(key); ok {
		rev := &pb.DnRev{}
		env.get(key, rev)
		return model.DnConfKey(env.cid, rev.GetAddrPort())
	}
	rev := &pb.CnRev{}
	env.get(key, rev)
	return model.CnConfKey(env.cid, rev.GetAddrPort())
}

// sptRevisionOf decodes the revision out of a DnRev or CnRev value. The two
// messages put `revision` in the same field, so one decode serves both.
func sptRevisionOf(t *testing.T, raw []byte) uint64 {
	t.Helper()
	rev := &pb.DnRev{}
	sptUnmarshal(t, raw, rev)
	return rev.GetRevision()
}

// TestDeleteStoragePoolBumpsASharedNodeOnce pins the ledger property §5.5
// exists for: one DN carrying SEVERAL sides of the SP being torn down is read
// once, written once, has its capacity key maintained once and its revision
// bumped ONCE — with the whole of what it carried returned to its budget.
//
// CreateStoragePool can never build such an SP (§6.5 puts every leg on a
// distinct DN), so the fixture is written by hand: this is exactly the state a
// GrowSlice reaching back to an already-used DN produces (decision D-F).
func TestDeleteStoragePoolBumpsASharedNodeOnce(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId, spName := env.putSharedDnSp()
	dnA, dnB := env.dnAddrs[0], env.dnAddrs[1]
	cnA := env.cnAddrs[0]
	// Each DN carries the meta side (1 extent) and the data side (4).
	const carried = 1 + sptInitExt
	if got := env.dnConf(dnA).GetFreeExtCnt(); got != sptDnFree-carried {
		t.Fatalf("fixture dn %q: free %d", dnA, got)
	}
	_, err := env.srv.DeleteStoragePool(
		env.ctx, &pb.DeleteStoragePoolRequest{
			ClusterName: env.name,
			SpName:      spName,
			SpRev:       &pb.SpRev{Revision: 1},
		})
	if err != nil {
		t.Fatalf("DeleteStoragePool: %v", err)
	}
	for _, addrPort := range []string{dnA, dnB} {
		dn := env.dnConf(addrPort)
		if dn.GetFreeExtCnt() != sptDnFree {
			t.Errorf("dn %q: free_ext_cnt %d, want %d",
				addrPort, dn.GetFreeExtCnt(), sptDnFree)
		}
		if len(dn.GetSidePtrList()) != 0 {
			t.Errorf("dn %q: side_ptr_list %v", addrPort, dn.GetSidePtrList())
		}
		if got := env.dnRev(addrPort); got != 2 {
			t.Errorf("dn %q carried two sides: revision %d, want 2 (one bump)",
				addrPort, got)
		}
		if !env.exists(env.dnCapacityKey(addrPort, sptDnFree)) {
			t.Errorf("dn %q: no capacity key for the restored free count",
				addrPort)
		}
		if env.exists(env.dnCapacityKey(addrPort, sptDnFree-carried)) {
			t.Errorf("dn %q: the charged capacity key survived", addrPort)
		}
	}
	if got := env.cnConf(cnA).GetFreeExtCnt(); got != sptCnFree {
		t.Errorf("cn %q: free_ext_cnt %d, want %d", cnA, got, sptCnFree)
	}
	if got := env.cnRev(cnA); got != 2 {
		t.Errorf("cn %q: revision %d, want 2", cnA, got)
	}
	if bucketSum(env.spGlobal().GetShardBucket()) != 0 {
		t.Errorf("the SP's bucket slot was not released")
	}
	if env.exists(model.SpConfKey(env.cid, spName)) {
		t.Errorf("sp_conf survived the teardown")
	}
	if env.exists(model.SpRevKey(0, env.cid, spId)) {
		t.Errorf("sp_rev survived the teardown")
	}
	if env.exists(model.SpNameKey(env.cid, spId)) {
		t.Errorf("sp_id_to_name survived the teardown")
	}
}

// putSharedDnSp writes an SP whose meta and data groups deliberately share
// their two disk nodes, with every DN, CN, capacity and global key charged as
// CreateStoragePool would have charged them. It returns the sp_id and name.
func (e *sptEnv) putSharedDnSp() (uint64, string) {
	e.t.Helper()
	const (
		spId    = uint64(1)
		spName  = "shared0"
		cntlrId = uint64(11)
		sliceId = uint64(21)
	)
	dnA, dnB, cnA := e.dnAddrs[0], e.dnAddrs[1], e.cnAddrs[0]
	grp := func(grpId uint64, extCnt uint64, base uint64) *pb.Group {
		return &pb.Group{
			GrpId:      grpId,
			ExtCnt:     extCnt,
			MetaBlocks: sptMetaBlocks,
			DataBlocks: sptMetaGrpData,
			LegList: []*pb.Leg{
				{
					LegId:  base,
					LegIdx: 0,
					SideList: []*pb.Side{{
						SideId:     base + 1,
						AddrPort:   dnA,
						NvmeTrConf: sptTrConf(dnA),
					}},
				},
				{
					LegId:  base + 2,
					LegIdx: 1,
					SideList: []*pb.Side{{
						SideId:     base + 3,
						AddrPort:   dnB,
						NvmeTrConf: sptTrConf(dnB),
					}},
				},
			},
		}
	}
	mustPut(e.t, e.cli, model.SpConfKey(e.cid, spName), &pb.SpConf{
		SpId:           spId,
		ShardCode:      0,
		NextId:         100,
		NextDevId:      1,
		BdevConf:       &pb.BdevConf{},
		CntlidSlotList: []uint32{0, 1},
		SpLevel:        pb.SpLevel_SP_LEVEL_READWRITE,
		CntlrIdList:    []uint64{cntlrId},
		SliceIdList:    []uint64{sliceId},
	})
	mustPut(e.t, e.cli, model.SpNameKey(e.cid, spId), &pb.SpName{
		SpName: spName,
	})
	mustPut(e.t, e.cli, model.SpRevKey(0, e.cid, spId), &pb.SpRev{
		SpName:   spName,
		Revision: 1,
	})
	mustPut(e.t, e.cli, model.SliceKey(e.cid, spId, sliceId), &pb.Slice{
		SliceIdx:    0,
		MetaGrpList: []*pb.Group{grp(31, 1, 41)},
		DataGrpList: []*pb.Group{grp(32, sptInitExt, 51)},
	})
	mustPut(e.t, e.cli, model.CntlrKey(e.cid, spId, cntlrId), &pb.Cntlr{
		AddrPort:   cnA,
		NvmeTrConf: sptTrConf(cnA),
		Primary:    true,
	})
	mustPut(e.t, e.cli, model.SpGlobalKey(e.cid), &pb.SpGlobal{
		NextId:      spId + 1,
		ShardBucket: sptBucketWith(0),
	})
	e.reserveDn(dnA, 1+sptInitExt, []*pb.SidePointer{
		{SpId: spId, LegId: 41, SideId: 42},
		{SpId: spId, LegId: 51, SideId: 52},
	})
	e.reserveDn(dnB, 1+sptInitExt, []*pb.SidePointer{
		{SpId: spId, LegId: 43, SideId: 44},
		{SpId: spId, LegId: 53, SideId: 54},
	})
	e.reserveCn(cnA, 1+sptInitExt, []*pb.CntlrPointer{
		{SpId: spId, CntlrId: cntlrId},
	})
	return spId, spName
}

// sptBucketWith is a shard_bucket holding exactly one object, in shard.
func sptBucketWith(shard uint32) []uint32 {
	bucket := zeroBucket()
	bucket[shard]++
	return bucket
}

// reserveDn charges a hand-written SP's footprint onto a DN exactly as the
// ledger would have: the budget drops, the pointers appear and the capacity
// key moves with the free count (§5.6).
func (e *sptEnv) reserveDn(
	addrPort string,
	extCnt uint64,
	ptrs []*pb.SidePointer,
) {
	e.t.Helper()
	dn := e.dnConf(addrPort)
	oldKey := e.dnCapacityKey(addrPort, dn.GetFreeExtCnt())
	dn.FreeExtCnt -= extCnt
	dn.SidePtrList = ptrs
	mustPut(e.t, e.cli, model.DnConfKey(e.cid, addrPort), dn)
	newKey := e.dnCapacityKey(addrPort, dn.GetFreeExtCnt())
	if oldKey != "" && oldKey != newKey {
		if err := e.cli.Delete(e.ctx, oldKey); err != nil {
			e.t.Fatalf("Delete %s: %v", oldKey, err)
		}
	}
	if newKey != "" {
		mustPut(e.t, e.cli, newKey, &pb.DnCapacity{Location: addrPort})
	}
}

// reserveCn is reserveDn for a controller node.
func (e *sptEnv) reserveCn(
	addrPort string,
	extCnt uint64,
	ptrs []*pb.CntlrPointer,
) {
	e.t.Helper()
	cn := e.cnConf(addrPort)
	oldKey := model.CnCapacityKey(e.cid, cn.GetFreeExtCnt(), addrPort)
	cn.FreeExtCnt -= extCnt
	cn.CntlrPtrList = ptrs
	mustPut(e.t, e.cli, model.CnConfKey(e.cid, addrPort), cn)
	if err := e.cli.Delete(e.ctx, oldKey); err != nil {
		e.t.Fatalf("Delete %s: %v", oldKey, err)
	}
	mustPut(
		e.t, e.cli,
		model.CnCapacityKey(e.cid, cn.GetFreeExtCnt(), addrPort),
		&pb.CnCapacity{Location: addrPort},
	)
}

// TestDeleteStoragePoolRefusals pins §8.4's preconditions: the five name lists
// are the whole gate — a thin device, subsystem, clone, transfer or migration
// the user made must be removed first — and a stale or missing token is
// refused before any of them is even looked at (GW6). Every refusal writes
// nothing.
func TestDeleteStoragePoolRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch func(conf *pb.SpConf)
		rev   *pb.SpRev
		want  codes.Code
	}{
		{"holds a thin device", func(c *pb.SpConf) {
			c.TdNameList = []string{"td0"}
		}, &pb.SpRev{Revision: 1}, codes.FailedPrecondition},
		{"holds a subsystem", func(c *pb.SpConf) {
			c.NqnList = []string{"nqn.2024-01.io.dnv:x"}
		}, &pb.SpRev{Revision: 1}, codes.FailedPrecondition},
		{"holds a clone", func(c *pb.SpConf) {
			c.CloneNameList = []string{"c0"}
		}, &pb.SpRev{Revision: 1}, codes.FailedPrecondition},
		{"holds a transfer", func(c *pb.SpConf) {
			c.XferNameList = []string{"x0"}
		}, &pb.SpRev{Revision: 1}, codes.FailedPrecondition},
		{"holds a migration", func(c *pb.SpConf) {
			c.MigrNameList = []string{"m0"}
		}, &pb.SpRev{Revision: 1}, codes.FailedPrecondition},
		{"stale token", nil, &pb.SpRev{Revision: 2}, codes.Aborted},
		// §0 #7: a nil token message reads as 0, and a stored revision
		// starts at 1, so it can never match.
		{"no token", nil, nil, codes.Aborted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
			env.createSp(sptSmallSpec(sptSpName))
			if tc.patch != nil {
				conf := env.spConf(sptSpName)
				tc.patch(conf)
				mustPut(t, env.cli, model.SpConfKey(env.cid, sptSpName), conf)
			}
			before := env.dump()
			_, err := env.srv.DeleteStoragePool(
				env.ctx, &pb.DeleteStoragePoolRequest{
					ClusterName: env.name,
					SpName:      sptSpName,
					SpRev:       tc.rev,
				})
			sptWantCode(t, err, tc.want)
			after := env.dump()
			if len(before) != len(after) {
				t.Fatalf("a refusal changed the key set: %d -> %d",
					len(before), len(after))
			}
			for key, value := range before {
				if !bytes.Equal(value, after[key]) {
					t.Errorf("a refusal rewrote %q", key)
				}
			}
		})
	}
}

// TestDeleteStoragePoolUnknown pins the NOT_FOUND rows of §8.4: an SP name
// nothing wrote, and a cluster name nothing wrote.
func TestDeleteStoragePoolUnknown(t *testing.T) {
	env := sptNewEnv(t, 1, 1, sptCnFree)
	_, err := env.srv.DeleteStoragePool(
		env.ctx, &pb.DeleteStoragePoolRequest{
			ClusterName: env.name,
			SpName:      "nosuch",
			SpRev:       &pb.SpRev{Revision: 1},
		})
	sptWantCode(t, err, codes.NotFound)
	_, err = env.srv.DeleteStoragePool(
		env.ctx, &pb.DeleteStoragePoolRequest{
			ClusterName: env.name + "-nope",
			SpName:      sptSpName,
			SpRev:       &pb.SpRev{Revision: 1},
		})
	sptWantCode(t, err, codes.NotFound)
}

// TestDeletingStoragePoolRefusesOtherMutators pins resolveSp's rejectDeleting
// gate (§8 preamble): an SP whose teardown has begun accepts no further
// changes, and DeleteStoragePool is the ONE mutator that must still proceed —
// refusing the RPC that finishes the teardown would strand the SP.
//
// Nothing in v1 ever sets `deleting`, so this is the branch's only exercise
// (gateway.md §10.18) and the flag is written directly.
func TestDeletingStoragePoolRefusesOtherMutators(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptSmallSpec(sptSpName))
	conf := env.spConf(sptSpName)
	conf.Deleting = true
	mustPut(t, env.cli, model.SpConfKey(env.cid, sptSpName), conf)

	_, err := env.srv.UpdateStoragePoolCntlidSlotList(
		env.ctx, &pb.UpdateStoragePoolCntlidSlotListRequest{
			ClusterName:    env.name,
			SpName:         sptSpName,
			SpRev:          &pb.SpRev{Revision: 1},
			CntlidSlotList: []uint32{0, 1},
		})
	sptWantCode(t, err, codes.FailedPrecondition)
	_, err = env.srv.CreateCntlr(env.ctx, &pb.CreateCntlrRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		SpRev:       &pb.SpRev{Revision: 1},
		CntlidSlot:  1,
	})
	sptWantCode(t, err, codes.FailedPrecondition)
	if got := env.spRev(0, spId); got != 1 {
		t.Errorf("a refusal bumped sp_rev to %d", got)
	}
	// The teardown itself still runs: openSpFlags(..., false).
	if _, err := env.srv.DeleteStoragePool(
		env.ctx, &pb.DeleteStoragePoolRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			SpRev:       &pb.SpRev{Revision: 1},
		}); err != nil {
		t.Fatalf("DeleteStoragePool on a deleting SP: %v", err)
	}
	if env.exists(model.SpConfKey(env.cid, sptSpName)) {
		t.Errorf("sp_conf survived the teardown")
	}
}

// ---------------------------------------------------------------------------
// The read paths (§8.4)
// ---------------------------------------------------------------------------

// TestGetStoragePool pins §8.4's read: the reply carries the stored SpConf,
// the SpRev token a caller then spends, and the cntlr and slice lists
// INDEX-ALIGNED with sp_conf.cntlr_id_list / slice_id_list, all read in one
// Snapshot so a client can never see a cntlr list from one revision beside a
// token from another.
func TestGetStoragePool(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	reply, err := env.srv.GetStoragePool(env.ctx, &pb.GetStoragePoolRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
	})
	if err != nil {
		t.Fatalf("GetStoragePool: %v", err)
	}
	if reply.GetSpName() != sptSpName {
		t.Errorf("sp_name: got %q", reply.GetSpName())
	}
	if !proto.Equal(reply.GetSpConf(), env.spConf(sptSpName)) {
		t.Errorf("sp_conf: got %v", reply.GetSpConf())
	}
	if reply.GetSpRev().GetRevision() != 1 ||
		reply.GetSpRev().GetSpName() != sptSpName {
		t.Errorf("sp_rev: got %v", reply.GetSpRev())
	}
	conf := reply.GetSpConf()
	if len(reply.GetCntlrList()) != len(conf.GetCntlrIdList()) {
		t.Fatalf("cntlr_list: %d entries for %d ids",
			len(reply.GetCntlrList()), len(conf.GetCntlrIdList()))
	}
	for idx, cntlrId := range conf.GetCntlrIdList() {
		if !proto.Equal(reply.GetCntlrList()[idx], env.cntlr(spId, cntlrId)) {
			t.Errorf("cntlr_list[%d] is not cntlr %d", idx, cntlrId)
		}
	}
	if len(reply.GetSliceList()) != len(conf.GetSliceIdList()) {
		t.Fatalf("slice_list: %d entries for %d ids",
			len(reply.GetSliceList()), len(conf.GetSliceIdList()))
	}
	for idx, sliceId := range conf.GetSliceIdList() {
		if !proto.Equal(reply.GetSliceList()[idx], env.slice(spId, sliceId)) {
			t.Errorf("slice_list[%d] is not slice %d", idx, sliceId)
		}
	}
	_, err = env.srv.GetStoragePool(env.ctx, &pb.GetStoragePoolRequest{
		ClusterName: env.name,
		SpName:      "nosuch",
	})
	sptWantCode(t, err, codes.NotFound)
}

// TestListStoragePools pins GW10's paging over the sp_conf prefix: names come
// back in key order, a full page hands out a token that continues exactly
// after the last name, the last page's token is empty, and a token that is not
// base64 is INVALID_ARGUMENT rather than an empty page.
func TestListStoragePools(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	want := []string{"pool-a", "pool-b", "pool-c"}
	for _, name := range want {
		env.createSp(sptSmallSpec(name))
	}
	first, err := env.srv.ListStoragePools(
		env.ctx, &pb.ListStoragePoolsRequest{
			ClusterName: env.name,
			Count:       2,
		})
	if err != nil {
		t.Fatalf("ListStoragePools: %v", err)
	}
	if fmt.Sprint(first.GetSpName()) != fmt.Sprint(want[:2]) {
		t.Errorf("page 1: got %v, want %v", first.GetSpName(), want[:2])
	}
	if first.GetPageToken() == "" {
		t.Fatalf("a full page must hand out a continuation token")
	}
	second, err := env.srv.ListStoragePools(
		env.ctx, &pb.ListStoragePoolsRequest{
			ClusterName: env.name,
			Count:       2,
			PageToken:   first.GetPageToken(),
		})
	if err != nil {
		t.Fatalf("ListStoragePools page 2: %v", err)
	}
	if fmt.Sprint(second.GetSpName()) != fmt.Sprint(want[2:]) {
		t.Errorf("page 2: got %v, want %v", second.GetSpName(), want[2:])
	}
	if second.GetPageToken() != "" {
		t.Errorf("a short page ends the listing: token %q",
			second.GetPageToken())
	}
	all, err := env.srv.ListStoragePools(
		env.ctx, &pb.ListStoragePoolsRequest{ClusterName: env.name})
	if err != nil {
		t.Fatalf("ListStoragePools default count: %v", err)
	}
	if fmt.Sprint(all.GetSpName()) != fmt.Sprint(want) {
		t.Errorf("default page: got %v, want %v", all.GetSpName(), want)
	}
	_, err = env.srv.ListStoragePools(env.ctx, &pb.ListStoragePoolsRequest{
		ClusterName: env.name,
		PageToken:   "not base64!!",
	})
	sptWantCode(t, err, codes.InvalidArgument)
	_, err = env.srv.ListStoragePools(env.ctx, &pb.ListStoragePoolsRequest{
		ClusterName: env.name,
		Count:       common.MaxListCnt + 1,
	})
	sptWantCode(t, err, codes.InvalidArgument)
}

// TestFindStoragePoolNames pins §8.4's reverse lookup: known ids map to their
// names and an id nothing wrote is simply OMITTED — absence IS the answer "no
// such sp_id", and making it an error would force a caller to probe one id at
// a time.
func TestFindStoragePoolNames(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	idA := env.createSp(sptSmallSpec("pool-a"))
	idB := env.createSp(sptSmallSpec("pool-b"))
	reply, err := env.srv.FindStoragePoolNames(
		env.ctx, &pb.FindStoragePoolNamesRequest{
			ClusterName: env.name,
			SpIdList:    []uint64{idA, 999, idB},
		})
	if err != nil {
		t.Fatalf("FindStoragePoolNames: %v", err)
	}
	found := reply.GetSpIdToName()
	if len(found) != 2 {
		t.Fatalf("sp_id_to_name: got %v, want two entries", found)
	}
	if found[idA] != "pool-a" || found[idB] != "pool-b" {
		t.Errorf("sp_id_to_name: got %v", found)
	}
	if _, ok := found[999]; ok {
		t.Errorf("an unknown sp_id must be omitted, not answered")
	}
	empty, err := env.srv.FindStoragePoolNames(
		env.ctx, &pb.FindStoragePoolNamesRequest{ClusterName: env.name})
	if err != nil {
		t.Fatalf("FindStoragePoolNames with no ids: %v", err)
	}
	if len(empty.GetSpIdToName()) != 0 {
		t.Errorf("no ids asked for: got %v", empty.GetSpIdToName())
	}
	// The cluster itself is still resolved, so an unknown one is NOT_FOUND.
	_, err = env.srv.FindStoragePoolNames(
		env.ctx, &pb.FindStoragePoolNamesRequest{
			ClusterName: env.name + "-nope",
			SpIdList:    []uint64{idA},
		})
	sptWantCode(t, err, codes.NotFound)
}

// ---------------------------------------------------------------------------
// UpdateStoragePoolCntlidSlotList (§8.4)
// ---------------------------------------------------------------------------

// TestUpdateStoragePoolCntlidSlotList pins §8.4's in-use-slot rule: a slot may
// only leave the list when nothing names it, and what names one is every
// cntlr's cntlid_slot AND every side's — a side's slot is what its DN's nvmet
// subsystem exports it under (§11.8), so dropping one would make the SP
// undeployable. Every refusal writes nothing and bumps nothing; the accepted
// list is stored and bumps SpRev exactly once (§5.5).
func TestUpdateStoragePoolCntlidSlotList(t *testing.T) {
	for _, tc := range []struct {
		name  string
		slots []uint32
		rev   uint64
		want  codes.Code
	}{
		// Cntlrs hold slots 0 and 1, every side holds slot 0.
		{"drops a slot a cntlr uses", []uint32{0, 2}, 1,
			codes.InvalidArgument},
		{"drops the slot every side uses", []uint32{1, 2}, 1,
			codes.InvalidArgument},
		{"empty list", nil, 1, codes.InvalidArgument},
		{"duplicate value", []uint32{0, 1, 1}, 1, codes.InvalidArgument},
		{"value out of range", []uint32{0, 1, common.CnCntlidSlotCnt}, 1,
			codes.InvalidArgument},
		{"stale token", []uint32{0, 1}, 7, codes.Aborted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
			spId := env.createSp(sptDefaultSpec(sptSpName))
			before := env.dump()
			_, err := env.srv.UpdateStoragePoolCntlidSlotList(
				env.ctx, &pb.UpdateStoragePoolCntlidSlotListRequest{
					ClusterName:    env.name,
					SpName:         sptSpName,
					SpRev:          &pb.SpRev{Revision: tc.rev},
					CntlidSlotList: tc.slots,
				})
			sptWantCode(t, err, tc.want)
			if got := env.spRev(0, spId); got != 1 {
				t.Errorf("a refusal bumped sp_rev to %d", got)
			}
			for key, value := range before {
				if !bytes.Equal(value, env.dump()[key]) {
					t.Errorf("a refusal rewrote %q", key)
				}
			}
		})
	}

	t.Run("keeps every used slot", func(t *testing.T) {
		env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
		spId := env.createSp(sptDefaultSpec(sptSpName))
		reply, err := env.srv.UpdateStoragePoolCntlidSlotList(
			env.ctx, &pb.UpdateStoragePoolCntlidSlotListRequest{
				ClusterName:    env.name,
				SpName:         sptSpName,
				SpRev:          &pb.SpRev{Revision: 1},
				CntlidSlotList: []uint32{0, 1, 5},
			})
		if err != nil {
			t.Fatalf("UpdateStoragePoolCntlidSlotList: %v", err)
		}
		if reply.GetSpId() != spId {
			t.Errorf("sp_id: got %d, want %d", reply.GetSpId(), spId)
		}
		got := env.spConf(sptSpName).GetCntlidSlotList()
		if fmt.Sprint(got) != fmt.Sprint([]uint32{0, 1, 5}) {
			t.Errorf("cntlid_slot_list: got %v", got)
		}
		if rev := env.spRev(0, spId); rev != 2 {
			t.Errorf("sp_rev: got %d, want exactly one bump to 2", rev)
		}
	})
}

// TestUpdateStoragePoolCntlidSlotListRefusesASideSlot separates the SIDE half
// of the in-use rule from the cntlr half, which otherwise shadows it: every
// side of a fresh SP holds cntlid_slot_list[0], the same slot the first cntlr
// holds, so the cntlr loop refuses first. Moving the cntlrs off slot 0 leaves
// the sides as the only thing naming it.
func TestUpdateStoragePoolCntlidSlotListRefusesASideSlot(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	conf := env.spConf(sptSpName)
	for idx, cntlrId := range conf.GetCntlrIdList() {
		cntlr := env.cntlr(spId, cntlrId)
		cntlr.CntlidSlot = uint32(idx) + 4
		mustPut(t, env.cli, model.CntlrKey(env.cid, spId, cntlrId), cntlr)
	}
	_, err := env.srv.UpdateStoragePoolCntlidSlotList(
		env.ctx, &pb.UpdateStoragePoolCntlidSlotListRequest{
			ClusterName:    env.name,
			SpName:         sptSpName,
			SpRev:          &pb.SpRev{Revision: 1},
			CntlidSlotList: []uint32{4, 5},
		})
	sptWantCode(t, err, codes.InvalidArgument)
	if !strings.Contains(status.Convert(err).Message(), "side") {
		t.Errorf("the message must name the side that still uses the slot: %q",
			status.Convert(err).Message())
	}
	if got := env.spRev(0, spId); got != 1 {
		t.Errorf("a refusal bumped sp_rev to %d", got)
	}
}

// ---------------------------------------------------------------------------
// GrowSlice (§8.5)
// ---------------------------------------------------------------------------

// TestGrowSliceData pins §8.5's data grow and decision D-E: the new group's
// size is the slice's FIRST data group's ext_cnt, NOT the request's ext_cnt —
// which is only §8.5's exclusivity signal. The group lands on legs on distinct
// disk nodes (decision D-F), its sides are unprovisioned ([D15]) and carry
// cntlid_slot_list[0], every leg's DN is charged the new size and every
// cntlr's CN reserves it too (§6.5), and SpRev, DnRev and CnRev each bump
// exactly once — all inside model.GrowSlice's own transaction, so the handler
// itself bumps nothing.
func TestGrowSliceData(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	conf := env.spConf(sptSpName)
	sliceId := conf.GetSliceIdList()[0]
	// The free count and revision of every DN as the grow finds them: a DN
	// already carrying another group's leg stays eligible (D-F), so "charged
	// exactly ext_cnt" has to be measured against what it had, not against a
	// pristine node.
	dnFreeBefore := make(map[string]uint64, len(env.dnAddrs))
	dnRevBefore := make(map[string]uint64, len(env.dnAddrs))
	for _, addrPort := range env.dnAddrs {
		dnFreeBefore[addrPort] = env.dnConf(addrPort).GetFreeExtCnt()
		dnRevBefore[addrPort] = env.dnRev(addrPort)
	}
	cnRevBefore := make(map[string]uint64)
	for _, cntlrId := range conf.GetCntlrIdList() {
		addrPort := env.cntlr(spId, cntlrId).GetAddrPort()
		cnRevBefore[addrPort] = env.cnRev(addrPort)
	}

	reply, err := env.srv.GrowSlice(env.ctx, &pb.GrowSliceRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		SpRev:       &pb.SpRev{Revision: 1},
		SliceId:     sliceId,
		// Deliberately not the slice's allocation unit. D-E's other half —
		// that the gateway must SCAN candidates for the size model.GrowSlice
		// will itself compute — is not observable from here: model.GrowSlice
		// recomputes the size and re-validates every pick, so a gateway that
		// scanned for the wrong one still commits the right group whenever
		// the picks happen to be large enough. Only the stored size is
		// asserted.
		ExtCnt: 1,
	})
	if err != nil {
		t.Fatalf("GrowSlice: %v", err)
	}
	if reply.GetSliceId() != sliceId {
		t.Errorf("slice_id: got %d, want %d", reply.GetSliceId(), sliceId)
	}
	slice := env.slice(spId, sliceId)
	if len(slice.GetDataGrpList()) != 2 || len(slice.GetMetaGrpList()) != 1 {
		t.Fatalf("slice: %d meta / %d data groups",
			len(slice.GetMetaGrpList()), len(slice.GetDataGrpList()))
	}
	grp := slice.GetDataGrpList()[1]
	if grp.GetGrpId() != reply.GetGrpId() {
		t.Errorf("grp_id: reply %d, stored %d",
			reply.GetGrpId(), grp.GetGrpId())
	}
	if grp.GetExtCnt() != sptInitExt {
		t.Errorf("ext_cnt: got %d, want the slice's allocation unit %d",
			grp.GetExtCnt(), sptInitExt)
	}
	if grp.GetMetaBlocks() != sptMetaBlocks ||
		grp.GetDataBlocks() != sptDataGrpData {
		t.Errorf("blocks: got %d/%d, want %d/%d", grp.GetMetaBlocks(),
			grp.GetDataBlocks(), sptMetaBlocks, sptDataGrpData)
	}
	if len(grp.GetLegList()) != 2 {
		t.Fatalf("legs: got %d, want 2", len(grp.GetLegList()))
	}
	newDns := make(map[string]bool)
	for legIdx, leg := range grp.GetLegList() {
		if leg.GetLegIdx() != uint32(legIdx) {
			t.Errorf("leg %d: leg_idx %d", leg.GetLegId(), leg.GetLegIdx())
		}
		side := leg.GetSideList()[0]
		if side.GetProvisioned() {
			t.Errorf("side %d must be written unprovisioned",
				side.GetSideId())
		}
		if side.GetCntlidSlot() != 0 {
			t.Errorf("side %d: cntlid_slot %d, want cntlid_slot_list[0]",
				side.GetSideId(), side.GetCntlidSlot())
		}
		if newDns[side.GetAddrPort()] {
			t.Errorf("both legs of the new group on DN %q",
				side.GetAddrPort())
		}
		newDns[side.GetAddrPort()] = true
		addrPort := side.GetAddrPort()
		dn := env.dnConf(addrPort)
		want := dnFreeBefore[addrPort] - sptInitExt
		if dn.GetFreeExtCnt() != want {
			t.Errorf("dn %q: free_ext_cnt %d, want %d",
				addrPort, dn.GetFreeExtCnt(), want)
		}
		if got := env.dnRev(addrPort); got != dnRevBefore[addrPort]+1 {
			t.Errorf("dn %q: revision %d, want %d",
				addrPort, got, dnRevBefore[addrPort]+1)
		}
	}
	// A DN the grow did not pick keeps its budget and its revision.
	for _, addrPort := range env.dnAddrs {
		if newDns[addrPort] {
			continue
		}
		if got := env.dnConf(addrPort).GetFreeExtCnt(); got !=
			dnFreeBefore[addrPort] {
			t.Errorf("dn %q was charged without being picked: %d, want %d",
				addrPort, got, dnFreeBefore[addrPort])
		}
		if got := env.dnRev(addrPort); got != dnRevBefore[addrPort] {
			t.Errorf("dn %q was bumped without being picked: %d, want %d",
				addrPort, got, dnRevBefore[addrPort])
		}
	}
	// §5.5: exactly one bump, and it is model.GrowSlice's, not the
	// handler's.
	if got := env.spRev(0, spId); got != 2 {
		t.Errorf("sp_rev: got %d, want exactly one bump to 2", got)
	}
	// §8.5 / §6.5: every cntlr stacks the new group, so every one of their
	// CNs reserves it.
	for _, cntlrId := range env.spConf(sptSpName).GetCntlrIdList() {
		addrPort := env.cntlr(spId, cntlrId).GetAddrPort()
		cn := env.cnConf(addrPort)
		want := sptCnFree - sptFootprint - sptInitExt
		if cn.GetFreeExtCnt() != want {
			t.Errorf("cn %q: free_ext_cnt %d, want %d",
				addrPort, cn.GetFreeExtCnt(), want)
		}
		if got := env.cnRev(addrPort); got != cnRevBefore[addrPort]+1 {
			t.Errorf("cn %q: revision %d, want %d",
				addrPort, got, cnRevBefore[addrPort]+1)
		}
	}
}

// TestGrowSliceConsecutiveDataGrows pins gateway.md §5.4's poolTotal
// argument (update_04.md U7): the gateway passes math.MaxUint64, so AR6's
// pending rule — a WORKER convergence guard judged by the primary's
// reported usage — never refuses a user-driven grow. The discriminator is
// the SECOND grow of a kind: per kind the first is never pending
// (len(grps) < 2), so a gateway that passed a real total (0 being what
// "no report" naively becomes) would refuse it FAILED_PRECONDITION
// "grow_pending" — issue_03.md I2's measured table.
func TestGrowSliceConsecutiveDataGrows(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	sliceId := env.spConf(sptSpName).GetSliceIdList()[0]
	for _, tok := range []uint64{1, 2} { // GrowSlice bumps SpRev itself
		reply, err := env.srv.GrowSlice(env.ctx, &pb.GrowSliceRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			SpRev:       &pb.SpRev{Revision: tok},
			SliceId:     sliceId,
			ExtCnt:      1, // the exclusivity signal, not the size (D-E)
		})
		if err != nil {
			t.Fatalf("GrowSlice with token %d: %v", tok, err)
		}
		if reply.GetSliceId() != sliceId {
			t.Errorf("slice_id: got %d, want %d",
				reply.GetSliceId(), sliceId)
		}
	}
	slice := env.slice(spId, sliceId)
	if len(slice.GetDataGrpList()) != 3 || len(slice.GetMetaGrpList()) != 1 {
		t.Fatalf("slice: %d meta / %d data groups, want 1 / 3",
			len(slice.GetMetaGrpList()), len(slice.GetDataGrpList()))
	}
	for _, grp := range slice.GetDataGrpList()[1:] {
		if grp.GetExtCnt() != sptInitExt {
			t.Errorf("grp %d: ext_cnt %d, want the allocation unit %d",
				grp.GetGrpId(), grp.GetExtCnt(), sptInitExt)
		}
		if grp.GetMetaBlocks() != sptMetaBlocks ||
			grp.GetDataBlocks() != sptDataGrpData {
			t.Errorf("grp %d: blocks %d/%d, want %d/%d", grp.GetGrpId(),
				grp.GetMetaBlocks(), grp.GetDataBlocks(),
				sptMetaBlocks, sptDataGrpData)
		}
	}
	if got := env.spRev(0, spId); got != 3 {
		t.Errorf("sp_rev: got %d, want 3 (one bump per grow)", got)
	}
}

// TestGrowSliceMeta pins the other half of §8.5: a meta grow states no ext_cnt
// and takes its size from the ladder — a slice whose meta groups total one
// extent grows by one — and the group is appended to meta_grp_list, not to
// data_grp_list.
func TestGrowSliceMeta(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	sliceId := env.spConf(sptSpName).GetSliceIdList()[0]
	reply, err := env.srv.GrowSlice(env.ctx, &pb.GrowSliceRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		SpRev:       &pb.SpRev{Revision: 1},
		SliceId:     sliceId,
		IsMeta:      true,
	})
	if err != nil {
		t.Fatalf("GrowSlice: %v", err)
	}
	slice := env.slice(spId, sliceId)
	if len(slice.GetMetaGrpList()) != 2 || len(slice.GetDataGrpList()) != 1 {
		t.Fatalf("slice: %d meta / %d data groups",
			len(slice.GetMetaGrpList()), len(slice.GetDataGrpList()))
	}
	grp := slice.GetMetaGrpList()[1]
	if grp.GetGrpId() != reply.GetGrpId() {
		t.Errorf("grp_id: reply %d, stored %d",
			reply.GetGrpId(), grp.GetGrpId())
	}
	// The ladder doubles the slice's meta total: 1 -> +1.
	if grp.GetExtCnt() != 1 {
		t.Errorf("ext_cnt: got %d, want the ladder value 1", grp.GetExtCnt())
	}
	if grp.GetMetaBlocks() != sptMetaBlocks ||
		grp.GetDataBlocks() != sptMetaGrpData {
		t.Errorf("blocks: got %d/%d, want %d/%d", grp.GetMetaBlocks(),
			grp.GetDataBlocks(), sptMetaBlocks, sptMetaGrpData)
	}
	seen := make(map[string]bool)
	for _, leg := range grp.GetLegList() {
		side := leg.GetSideList()[0]
		if side.GetProvisioned() {
			t.Errorf("side %d must be written unprovisioned",
				side.GetSideId())
		}
		if seen[side.GetAddrPort()] {
			t.Errorf("both legs of the new group on DN %q",
				side.GetAddrPort())
		}
		seen[side.GetAddrPort()] = true
	}
	if got := env.spRev(0, spId); got != 2 {
		t.Errorf("sp_rev: got %d, want exactly one bump to 2", got)
	}
}

// TestGrowSliceMetaLadderCap pins the meta ladder's ceiling AND the error
// class update_05.md U4 settled on: the 16 GiB cap is the SP's own permanent
// structural ceiling — per-object state, not exhaustible capacity — so it is
// FAILED_PRECONDITION, and model.GrowSlice's in-STM re-check of the same cap
// already maps there, so before U4 the gateway pre-check (RESOURCE_EXHAUSTED)
// and the transaction agreed on the client's code only by accident of which
// won the race.
//
// U4 is also which error table wins where the specs disagree: gateway.md §5.4
// and GW7 put "the meta ladder is at the 16 GiB dm-thin metadata cap" in the
// RESOURCE_EXHAUSTED row, while architecture.md §8.5's error list calls it
// FAILED_PRECONDITION — and gateway.md §0 #2's own precedence rule gives §8.5
// the last word. This test is the pre-check's half of that.
//
// The slice's meta total is raised to the cap directly rather than by four
// real grows: the refusal is decided from the slice alone and returns before
// anything is written, so the DN budgets those grows would have spent never
// enter it.
func TestGrowSliceMetaLadderCap(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	sliceId := env.spConf(sptSpName).GetSliceIdList()[0]
	slice := env.slice(spId, sliceId)
	// 16 extents of 1 GiB is exactly the 16 GiB ceiling, and the ladder is
	// refused once the total has REACHED it.
	slice.GetMetaGrpList()[0].ExtCnt = 16
	mustPut(t, env.cli, model.SliceKey(env.cid, spId, sliceId), slice)
	before := env.dump()
	_, err := env.srv.GrowSlice(env.ctx, &pb.GrowSliceRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		SpRev:       &pb.SpRev{Revision: 1},
		SliceId:     sliceId,
		IsMeta:      true,
	})
	sptWantCode(t, err, codes.FailedPrecondition)
	if got := env.spRev(0, spId); got != 1 {
		t.Errorf("a refusal bumped sp_rev to %d", got)
	}
	after := env.dump()
	if len(before) != len(after) {
		t.Fatalf("a refusal changed the key set: %d -> %d",
			len(before), len(after))
	}
	for key, value := range before {
		if !bytes.Equal(value, after[key]) {
			t.Errorf("a refusal rewrote %q", key)
		}
	}
}

// TestGrowSliceRefusals pins §8.5's refusals: the ext_cnt / is_meta
// exclusivity of §7, a slice_id the SP does not list, and GW6's token check —
// which openSp runs BEFORE the slice lookup, so a stale client hears "stale
// revision" and never a NOT_FOUND computed against a list it has not read.
func TestGrowSliceRefusals(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	sliceId := env.spConf(sptSpName).GetSliceIdList()[0]
	for _, tc := range []struct {
		name    string
		sliceId uint64
		extCnt  uint64
		isMeta  bool
		rev     *pb.SpRev
		want    codes.Code
	}{
		{"meta grow with an ext_cnt", sliceId, 4, true,
			&pb.SpRev{Revision: 1}, codes.InvalidArgument},
		{"data grow without an ext_cnt", sliceId, 0, false,
			&pb.SpRev{Revision: 1}, codes.InvalidArgument},
		{"unknown slice", 999, 4, false,
			&pb.SpRev{Revision: 1}, codes.NotFound},
		{"stale token", sliceId, 4, false,
			&pb.SpRev{Revision: 9}, codes.Aborted},
		{"no token", sliceId, 4, false, nil, codes.Aborted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.srv.GrowSlice(env.ctx, &pb.GrowSliceRequest{
				ClusterName: env.name,
				SpName:      sptSpName,
				SpRev:       tc.rev,
				SliceId:     tc.sliceId,
				ExtCnt:      tc.extCnt,
				IsMeta:      tc.isMeta,
			})
			sptWantCode(t, err, tc.want)
			if got := env.spRev(0, spId); got != 1 {
				t.Errorf("a refusal bumped sp_rev to %d", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Cntlrs (§8.6)
// ---------------------------------------------------------------------------

// sptNqn is the subsystem the CdcEntry tests hang off.
const sptNqn = "nqn.2024-01.io.dnv:spt-pool0"

// sptSsId is that subsystem's ss_id, which addresses its CdcEntry.
const sptSsId = uint64(9001)

// addSubsystem gives the SP one subsystem and the CdcEntry that advertises
// every ENABLED cntlr's CN (§8.8) — the state CreateSubsystem leaves behind,
// written directly because this file's subject is the cntlr RPCs that MAINTAIN
// that entry, not the one that creates it.
func (e *sptEnv) addSubsystem(spId uint64) {
	e.t.Helper()
	conf := e.spConf(sptSpName)
	conf.NqnList = append(conf.GetNqnList(), sptNqn)
	mustPut(e.t, e.cli, model.SpConfKey(e.cid, sptSpName), conf)
	mustPut(e.t, e.cli, model.SubsystemKey(e.cid, spId, sptNqn),
		&pb.Subsystem{SsId: sptSsId, Serial: "s0", Model: "dnv"})
	var trConfs []*pb.NvmeTrConf
	for _, cntlrId := range conf.GetCntlrIdList() {
		cntlr := e.cntlr(spId, cntlrId)
		if cntlr.GetDisabled() {
			continue
		}
		trConfs = append(trConfs, cntlr.GetNvmeTrConf())
	}
	mustPut(e.t, e.cli, model.CdcEntryKey(e.cid, 0, spId, sptSsId),
		&pb.CdcEntry{Nqn: sptNqn, NvmeTrConfList: trConfs})
}

// sptTrAddrs is the tr_addr of every entry of a transport list, which is what
// a CdcEntry assertion compares — the four members are compared by trConfEqual
// everywhere else, and tr_addr is this fixture's node identity.
func sptTrAddrs(list []*pb.NvmeTrConf) []string {
	out := make([]string, 0, len(list))
	for _, conf := range list {
		out = append(out, conf.GetTrAddr())
	}
	return out
}

// TestCreateCntlr pins §8.6's CreateCntlr: a standby is added on a CN that
// hosts none of the SP's cntlrs, it reserves the SP's whole footprint there
// (§6.5), it is enabled from birth so its CN joins every CdcEntry of the SP
// (§8.8), the id joins cntlr_id_list, and SpRev and the CN's CnRev each bump
// exactly once.
func TestCreateCntlr(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	env.addSubsystem(spId)
	before := env.spConf(sptSpName)
	oldAddrs := make(map[string]bool)
	for _, cntlrId := range before.GetCntlrIdList() {
		oldAddrs[env.cntlr(spId, cntlrId).GetAddrPort()] = true
	}
	reply, err := env.srv.CreateCntlr(env.ctx, &pb.CreateCntlrRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		SpRev:       &pb.SpRev{Revision: 1},
		CntlidSlot:  2,
	})
	if err != nil {
		t.Fatalf("CreateCntlr: %v", err)
	}
	cntlrId := reply.GetCntlrId()
	conf := env.spConf(sptSpName)
	if len(conf.GetCntlrIdList()) != sptCntlrCnt+1 ||
		conf.GetCntlrIdList()[sptCntlrCnt] != cntlrId {
		t.Fatalf("cntlr_id_list: got %v, want %v appended",
			conf.GetCntlrIdList(), cntlrId)
	}
	cntlr := env.cntlr(spId, cntlrId)
	if cntlr.GetPrimary() {
		t.Errorf("a new cntlr is a standby, not the primary")
	}
	if cntlr.GetDisabled() {
		t.Errorf("a new cntlr is enabled from birth")
	}
	if cntlr.GetCntlidSlot() != 2 {
		t.Errorf("cntlid_slot: got %d, want 2", cntlr.GetCntlidSlot())
	}
	if oldAddrs[cntlr.GetAddrPort()] {
		t.Errorf("two cntlrs of one SP on CN %q", cntlr.GetAddrPort())
	}
	if !proto.Equal(cntlr.GetNvmeTrConf(), sptTrConf(cntlr.GetAddrPort())) {
		t.Errorf("nvme_tr_conf: got %v", cntlr.GetNvmeTrConf())
	}
	cn := env.cnConf(cntlr.GetAddrPort())
	if cn.GetFreeExtCnt() != sptCnFree-sptFootprint {
		t.Errorf("cn %q: free_ext_cnt %d, want %d", cntlr.GetAddrPort(),
			cn.GetFreeExtCnt(), sptCnFree-sptFootprint)
	}
	wantPtr := &pb.CntlrPointer{SpId: spId, CntlrId: cntlrId}
	if len(cn.GetCntlrPtrList()) != 1 ||
		!proto.Equal(cn.GetCntlrPtrList()[0], wantPtr) {
		t.Errorf("cn %q: cntlr_ptr_list %v", cntlr.GetAddrPort(),
			cn.GetCntlrPtrList())
	}
	if got := env.cnRev(cntlr.GetAddrPort()); got != 2 {
		t.Errorf("cn %q: revision %d, want exactly one bump to 2",
			cntlr.GetAddrPort(), got)
	}
	entry := env.cdcEntry(0, spId, sptSsId)
	got := sptTrAddrs(entry.GetNvmeTrConfList())
	if len(got) != sptCntlrCnt+1 ||
		got[sptCntlrCnt] != cntlr.GetAddrPort() {
		t.Errorf("cdc entry: got %v, want %q appended",
			got, cntlr.GetAddrPort())
	}
	if rev := env.spRev(0, spId); rev != 2 {
		t.Errorf("sp_rev: got %d, want exactly one bump to 2", rev)
	}
}

// TestCreateCntlrRefusals pins §8.6's refusals, all of which write nothing:
// a cntlid_slot outside [0, 8), one the SP's cntlid_slot_list does not name,
// one another cntlr of the SP already holds, a stale token, and the
// MaxCntlrCntPerSp ceiling — which is RESOURCE_EXHAUSTED and is checked before
// the slot rules, so an SP already at the ceiling reports the ceiling.
func TestCreateCntlrRefusals(t *testing.T) {
	t.Run("slot rules and token", func(t *testing.T) {
		env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
		spId := env.createSp(sptSpec{
			name:     sptSpName,
			cntlrCnt: sptCntlrCnt,
			sliceCnt: 1,
			initExt:  1,
			slots:    []uint32{0, 1, 2},
		})
		for _, tc := range []struct {
			name string
			slot uint32
			rev  *pb.SpRev
			want codes.Code
		}{
			{"slot out of range", common.CnCntlidSlotCnt,
				&pb.SpRev{Revision: 1}, codes.InvalidArgument},
			{"slot not in the SP's list", 3,
				&pb.SpRev{Revision: 1}, codes.InvalidArgument},
			{"slot already used", 0,
				&pb.SpRev{Revision: 1}, codes.InvalidArgument},
			{"stale token", 2, &pb.SpRev{Revision: 9}, codes.Aborted},
			{"no token", 2, nil, codes.Aborted},
		} {
			t.Run(tc.name, func(t *testing.T) {
				before := env.dump()
				_, err := env.srv.CreateCntlr(
					env.ctx, &pb.CreateCntlrRequest{
						ClusterName: env.name,
						SpName:      sptSpName,
						SpRev:       tc.rev,
						CntlidSlot:  tc.slot,
					})
				sptWantCode(t, err, tc.want)
				if got := env.spRev(0, spId); got != 1 {
					t.Errorf("a refusal bumped sp_rev to %d", got)
				}
				after := env.dump()
				if len(before) != len(after) {
					t.Fatalf("a refusal changed the key set: %d -> %d",
						len(before), len(after))
				}
				for key, value := range before {
					if !bytes.Equal(value, after[key]) {
						t.Errorf("a refusal rewrote %q", key)
					}
				}
			})
		}
	})

	t.Run("at the per-SP ceiling", func(t *testing.T) {
		env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
		spId := env.createSp(sptSpec{
			name:     sptSpName,
			cntlrCnt: common.MaxCntlrCntPerSp,
			sliceCnt: 1,
			initExt:  1,
		})
		_, err := env.srv.CreateCntlr(env.ctx, &pb.CreateCntlrRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			SpRev:       &pb.SpRev{Revision: 1},
			CntlidSlot:  common.MaxCntlrCntPerSp,
		})
		sptWantCode(t, err, codes.ResourceExhausted)
		if got := env.spRev(0, spId); got != 1 {
			t.Errorf("a refusal bumped sp_rev to %d", got)
		}
	})

	t.Run("every controller node already hosts a cntlr", func(t *testing.T) {
		// Exactly as many CNs as the SP already uses, so §6.4's "two cntlrs
		// of one SP never share a CN" leaves the scan nothing to draw.
		env := sptNewEnv(t, sptDnCnt, sptCntlrCnt, sptCnFree)
		spId := env.createSp(sptSpec{
			name:     sptSpName,
			cntlrCnt: sptCntlrCnt,
			sliceCnt: 1,
			initExt:  1,
		})
		_, err := env.srv.CreateCntlr(env.ctx, &pb.CreateCntlrRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			SpRev:       &pb.SpRev{Revision: 1},
			CntlidSlot:  3,
		})
		sptWantCode(t, err, codes.ResourceExhausted)
		if got := env.spRev(0, spId); got != 1 {
			t.Errorf("a refusal bumped sp_rev to %d", got)
		}
	})

	t.Run("the free controller node cannot hold the footprint",
		func(t *testing.T) {
			// One CN is left unused by the SP and then charged down below
			// the SP's footprint, which is the size the scan asks for
			// (§6.5): the only node §6.4 would allow is not a candidate.
			env := sptNewEnv(t, sptDnCnt, sptCntlrCnt+1, sptCnFree)
			spId := env.createSp(sptSpec{
				name:     sptSpName,
				cntlrCnt: sptCntlrCnt,
				sliceCnt: 1,
				initExt:  1,
			})
			used := make(map[string]bool)
			for _, id := range env.spConf(sptSpName).GetCntlrIdList() {
				used[env.cntlr(spId, id).GetAddrPort()] = true
			}
			spare := ""
			for _, addrPort := range env.cnAddrs {
				if !used[addrPort] {
					spare = addrPort
				}
			}
			// This SP's footprint is one meta plus one data extent, so a
			// single free extent is one short.
			env.reserveCn(spare, sptCnFree-1, nil)
			_, err := env.srv.CreateCntlr(env.ctx, &pb.CreateCntlrRequest{
				ClusterName: env.name,
				SpName:      sptSpName,
				SpRev:       &pb.SpRev{Revision: 1},
				CntlidSlot:  3,
			})
			sptWantCode(t, err, codes.ResourceExhausted)
			if got := env.spRev(0, spId); got != 1 {
				t.Errorf("a refusal bumped sp_rev to %d", got)
			}
		})
}

// TestDeleteCntlr pins §8.6's DeleteCntlr: it refuses the primary and refuses
// an ENABLED cntlr — requiring the disable first means the §10.4 re-election
// has already happened and hosts have moved before their paths do — and it
// then undoes everything CreateCntlr did, item for item: the id leaves the
// list, the key goes, the CN gets its pointer and the SP's footprint back
// through one flush, the address is out of every CdcEntry, and SpRev bumps
// once.
func TestDeleteCntlr(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	env.addSubsystem(spId)
	conf := env.spConf(sptSpName)
	primaryId := conf.GetCntlrIdList()[0]
	standbyId := conf.GetCntlrIdList()[1]
	standbyAddr := env.cntlr(spId, standbyId).GetAddrPort()

	_, err := env.srv.DeleteCntlr(env.ctx, &pb.DeleteCntlrRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		SpRev:       &pb.SpRev{Revision: 1},
		CntlrId:     primaryId,
	})
	sptWantCode(t, err, codes.FailedPrecondition)
	_, err = env.srv.DeleteCntlr(env.ctx, &pb.DeleteCntlrRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		SpRev:       &pb.SpRev{Revision: 1},
		CntlrId:     standbyId,
	})
	sptWantCode(t, err, codes.FailedPrecondition)
	_, err = env.srv.DeleteCntlr(env.ctx, &pb.DeleteCntlrRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		SpRev:       &pb.SpRev{Revision: 1},
		CntlrId:     9999,
	})
	sptWantCode(t, err, codes.NotFound)
	if got := env.spRev(0, spId); got != 1 {
		t.Fatalf("a refusal bumped sp_rev to %d", got)
	}

	if _, err := env.srv.UpdateCntlrEnabled(
		env.ctx, &pb.UpdateCntlrEnabledRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			SpRev:       &pb.SpRev{Revision: 1},
			CntlrId:     standbyId,
			Enabled:     false,
		}); err != nil {
		t.Fatalf("UpdateCntlrEnabled: %v", err)
	}
	cnRevBefore := env.cnRev(standbyAddr)
	reply, err := env.srv.DeleteCntlr(env.ctx, &pb.DeleteCntlrRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		SpRev:       &pb.SpRev{Revision: 2},
		CntlrId:     standbyId,
	})
	if err != nil {
		t.Fatalf("DeleteCntlr: %v", err)
	}
	if reply.GetCntlrId() != standbyId {
		t.Errorf("cntlr_id: got %d, want %d", reply.GetCntlrId(), standbyId)
	}
	if env.exists(model.CntlrKey(env.cid, spId, standbyId)) {
		t.Errorf("the cntlr key survived the delete")
	}
	if containsId(env.spConf(sptSpName).GetCntlrIdList(), standbyId) {
		t.Errorf("cntlr %d is still listed", standbyId)
	}
	cn := env.cnConf(standbyAddr)
	if cn.GetFreeExtCnt() != sptCnFree {
		t.Errorf("cn %q: free_ext_cnt %d, want the whole footprint back (%d)",
			standbyAddr, cn.GetFreeExtCnt(), sptCnFree)
	}
	if len(cn.GetCntlrPtrList()) != 0 {
		t.Errorf("cn %q: cntlr_ptr_list %v", standbyAddr,
			cn.GetCntlrPtrList())
	}
	if got := env.cnRev(standbyAddr); got != cnRevBefore+1 {
		t.Errorf("cn %q: revision %d, want %d",
			standbyAddr, got, cnRevBefore+1)
	}
	got := sptTrAddrs(env.cdcEntry(0, spId, sptSsId).GetNvmeTrConfList())
	for _, addr := range got {
		if addr == standbyAddr {
			t.Errorf("cdc entry still advertises %q: %v", standbyAddr, got)
		}
	}
	if rev := env.spRev(0, spId); rev != 3 {
		t.Errorf("sp_rev: got %d, want 3 (one disable, one delete)", rev)
	}
}

// TestUpdateCntlrEnabled pins §8.6's enable flag together with its §8.8 side
// effect: disabling takes the cntlr's CN out of every CdcEntry of the SP at
// the same instant its namespaces go ANA-inaccessible, enabling puts it back,
// and each transition bumps SpRev exactly once.
func TestUpdateCntlrEnabled(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	env.addSubsystem(spId)
	conf := env.spConf(sptSpName)
	standbyId := conf.GetCntlrIdList()[1]
	standbyAddr := env.cntlr(spId, standbyId).GetAddrPort()
	primaryAddr := env.cntlr(spId, conf.GetCntlrIdList()[0]).GetAddrPort()

	reply, err := env.srv.UpdateCntlrEnabled(
		env.ctx, &pb.UpdateCntlrEnabledRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			SpRev:       &pb.SpRev{Revision: 1},
			CntlrId:     standbyId,
			Enabled:     false,
		})
	if err != nil {
		t.Fatalf("UpdateCntlrEnabled disable: %v", err)
	}
	if reply.GetCntlrId() != standbyId || reply.GetEnabled() {
		t.Errorf("reply: got %v", reply)
	}
	if !env.cntlr(spId, standbyId).GetDisabled() {
		t.Errorf("cntlr %d is still enabled", standbyId)
	}
	got := sptTrAddrs(env.cdcEntry(0, spId, sptSsId).GetNvmeTrConfList())
	if fmt.Sprint(got) != fmt.Sprint([]string{primaryAddr}) {
		t.Errorf("cdc entry after the disable: got %v, want [%q]",
			got, primaryAddr)
	}
	if rev := env.spRev(0, spId); rev != 2 {
		t.Errorf("sp_rev: got %d, want exactly one bump to 2", rev)
	}

	// Enabling puts the address back, at the end of the list.
	if _, err := env.srv.UpdateCntlrEnabled(
		env.ctx, &pb.UpdateCntlrEnabledRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			SpRev:       &pb.SpRev{Revision: 2},
			CntlrId:     standbyId,
			Enabled:     true,
		}); err != nil {
		t.Fatalf("UpdateCntlrEnabled enable: %v", err)
	}
	if env.cntlr(spId, standbyId).GetDisabled() {
		t.Errorf("cntlr %d is still disabled", standbyId)
	}
	got = sptTrAddrs(env.cdcEntry(0, spId, sptSsId).GetNvmeTrConfList())
	if fmt.Sprint(got) != fmt.Sprint([]string{primaryAddr, standbyAddr}) {
		t.Errorf("cdc entry after the enable: got %v", got)
	}
	if rev := env.spRev(0, spId); rev != 3 {
		t.Errorf("sp_rev: got %d, want 3", rev)
	}

	_, err = env.srv.UpdateCntlrEnabled(
		env.ctx, &pb.UpdateCntlrEnabledRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			SpRev:       &pb.SpRev{Revision: 3},
			CntlrId:     9999,
			Enabled:     false,
		})
	sptWantCode(t, err, codes.NotFound)
}

// TestUpdateCntlrEnabledNoWrite pins §0 #17: a request that asks for the state
// already stored writes NOTHING and bumps NOTHING — a no-op that bumped SpRev
// would invalidate every client's token and make every agent re-sync for a
// change that did not happen — while the token is still checked FIRST (GW6),
// so a stale client hears ABORTED rather than a misleading OK.
func TestUpdateCntlrEnabledNoWrite(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	env.addSubsystem(spId)
	standbyId := env.spConf(sptSpName).GetCntlrIdList()[1]

	before := env.dump()
	reply, err := env.srv.UpdateCntlrEnabled(
		env.ctx, &pb.UpdateCntlrEnabledRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			SpRev:       &pb.SpRev{Revision: 1},
			CntlrId:     standbyId,
			Enabled:     true,
		})
	if err != nil {
		t.Fatalf("UpdateCntlrEnabled: %v", err)
	}
	if reply.GetCntlrId() != standbyId || !reply.GetEnabled() {
		t.Errorf("reply: got %v", reply)
	}
	after := env.dump()
	if len(before) != len(after) {
		t.Fatalf("the no-op changed the key set: %d -> %d",
			len(before), len(after))
	}
	for key, value := range before {
		if !bytes.Equal(value, after[key]) {
			t.Errorf("the no-op rewrote %q", key)
		}
	}
	if rev := env.spRev(0, spId); rev != 1 {
		t.Errorf("sp_rev: got %d, want 1 — a no-op bumps nothing", rev)
	}
	// GW6 still runs first: the token is checked before the flag is read.
	_, err = env.srv.UpdateCntlrEnabled(
		env.ctx, &pb.UpdateCntlrEnabledRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			SpRev:       &pb.SpRev{Revision: 9},
			CntlrId:     standbyId,
			Enabled:     true,
		})
	sptWantStale(t, err)
	_, err = env.srv.UpdateCntlrEnabled(
		env.ctx, &pb.UpdateCntlrEnabledRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			CntlrId:     standbyId,
			Enabled:     true,
		})
	sptWantStale(t, err)
}
