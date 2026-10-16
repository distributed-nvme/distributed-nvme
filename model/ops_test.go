package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// noExpectRev is the expectRev the worker's own calls pass to the three ops
// the gateway shares with it (gateway.md GW6): 0 skips the SpRev token
// check, which is what every test below wants unless it is testing the check.
const noExpectRev = uint64(0)

// ---------------------------------------------------------------------------
// The MD6 fixture: one SP with two cntlrs and one slice whose meta and data
// groups are md-raid1 pairs, plus four DNs and three CNs with capacity and
// revision keys. Ids are distinct per kind so that a wrong key shows up as a
// missing object rather than as a lucky hit.
// ---------------------------------------------------------------------------

const (
	opsShard     = uint32(7)
	opsSpId      = uint64(0x100)
	opsSpName    = "ops-pool"
	opsNqn       = "nqn.2024-01.io.dnv:ops-pool"
	opsSsId      = uint64(501)
	opsCntlrA    = uint64(201)
	opsCntlrB    = uint64(202)
	opsSliceId   = uint64(301)
	opsMetaGrpId = uint64(401)
	opsDataGrpId = uint64(402)
	opsMetaLegA  = uint64(411)
	opsMetaLegB  = uint64(412)
	opsDataLegA  = uint64(421)
	opsDataLegB  = uint64(422)
	opsMetaSideA = uint64(431)
	opsMetaSideB = uint64(432)
	opsDataSideA = uint64(441)
	opsDataSideB = uint64(442)
	opsSpareLeg  = uint64(451)
	opsSpareSide = uint64(452)
	opsNextId    = uint64(1000)
	opsSlot      = uint32(3)
	// opsNotPending is a reported pool total no slice of this fixture can
	// imply, so AR6's pending rule never holds: the tests that are not about
	// the pending precondition pass it.
	opsNotPending = ^uint64(0)

	opsDnA = "dn-a:9000"
	opsDnB = "dn-b:9000"
	opsDnC = "dn-c:9000"
	opsDnD = "dn-d:9000"
	opsCnA = "cn-a:9000"
	opsCnB = "cn-b:9000"
	opsCnC = "cn-c:9000"
	// opsCnD is a fourth CN, outside the fixture: only the ReplaceCntlr
	// cases that gain a cntlr on a CN of its own write it.
	opsCnD = "cn-d:9000"

	opsExtSize    = uint64(1) << 30
	opsBlockSize  = uint64(1) << 20
	opsDataExtCnt = uint64(4)
	opsMetaExtCnt = uint64(1)
	// opsFootprint is Σ ext_cnt over every group of every slice.
	opsFootprint = opsMetaExtCnt + opsDataExtCnt
	opsDnFree    = uint64(100)
	opsCnFree    = uint64(50)
)

// opsEnv is one test's SP, written into its own cluster id so that the shared
// etcd needs no cleanup between tests.
type opsEnv struct {
	t   *testing.T
	ctx context.Context
	cli *etcdutil.Client
	cid uint64
	cc  *pb.ClusterConf
}

// opsTrConf is one node's transport configuration, distinct per node so that a
// CdcEntry rewrite can be checked field by field.
func opsTrConf(addrPort string) *pb.NvmeTrConf {
	return &pb.NvmeTrConf{
		TrType:  "tcp",
		AdrFam:  "ipv4",
		TrAddr:  addrPort,
		TrSvcId: "4420",
	}
}

// opsSpConf is the SP the fixture writes; every op reads it back.
func opsSpConf() *pb.SpConf {
	// A stored bdev_conf, every member concrete: CreateStoragePool resolved
	// it once and every op below reads it as written (architecture.md, Common
	// validation).
	bdevConf := testRaid1BdevConf(opsBlockSize, common.DefaultChunkBlockCnt)
	return &pb.SpConf{
		SpId:           opsSpId,
		ShardCode:      opsShard,
		NextId:         opsNextId,
		NextDevId:      1,
		BdevConf:       bdevConf,
		EventThreshold: &pb.EventThreshold{},
		CntlidSlotList: []uint32{opsSlot, 4, 5},
		SpLevel:        pb.SpLevel_SP_LEVEL_READWRITE,
		CntlrIdList:    []uint64{opsCntlrA, opsCntlrB},
		SliceIdList:    []uint64{opsSliceId},
		TdNameList:     []string{"td0", "td1"},
		NqnList:        []string{opsNqn},
		CloneNameList:  nil,
		XferNameList:   nil,
		MigrNameList:   nil,
	}
}

// opsSide builds one side of the fixture slice.
func opsSide(sideId uint64, addrPort string, provisioned bool) *pb.Side {
	return &pb.Side{
		SideId:      sideId,
		AddrPort:    addrPort,
		CntlidSlot:  opsSlot,
		NvmeTrConf:  opsTrConf(addrPort),
		Provisioned: provisioned,
	}
}

// opsSlice is the fixture's single slice: one meta group and one data group,
// each an md-raid1 pair on dn-a and dn-b, every side already provisioned.
func opsSlice() *pb.Slice {
	return &pb.Slice{
		SliceIdx: 0,
		MetaGrpList: []*pb.Group{{
			GrpId:      opsMetaGrpId,
			ExtCnt:     opsMetaExtCnt,
			MetaBlocks: 3,
			DataBlocks: 1021,
			LegList: []*pb.Leg{
				{
					LegId:    opsMetaLegA,
					LegIdx:   0,
					SideList: []*pb.Side{opsSide(opsMetaSideA, opsDnA, true)},
				},
				{
					LegId:    opsMetaLegB,
					LegIdx:   1,
					SideList: []*pb.Side{opsSide(opsMetaSideB, opsDnB, true)},
				},
			},
		}},
		DataGrpList: []*pb.Group{{
			GrpId:      opsDataGrpId,
			ExtCnt:     opsDataExtCnt,
			MetaBlocks: 3,
			DataBlocks: 4093,
			LegList: []*pb.Leg{
				{
					LegId:    opsDataLegA,
					LegIdx:   0,
					SideList: []*pb.Side{opsSide(opsDataSideA, opsDnA, true)},
				},
				{
					LegId:    opsDataLegB,
					LegIdx:   1,
					SideList: []*pb.Side{opsSide(opsDataSideB, opsDnB, true)},
				},
			},
		}},
	}
}

// newOpsEnv writes the whole fixture and returns the environment the tests
// mutate.
func newOpsEnv(t *testing.T) *opsEnv {
	t.Helper()
	cli := newTestClient(t)
	// A stored ClusterConf: concrete from CreateCluster on, which is what the
	// GrowSlice gate requires (architecture.md, Common validation). Its extent
	// size is named here because the
	// whole fixture's block arithmetic is written against 1 GiB extents.
	cc := testClusterConf()
	cc.DnBinConf.ExtentSize = opsExtSize
	env := &opsEnv{
		t:   t,
		ctx: context.Background(),
		cli: cli,
		cid: testCid(t),
		cc:  cc,
	}
	cid := env.cid
	mustPut(t, cli, SpConfKey(cid, opsSpName), opsSpConf())
	mustPut(t, cli, SpNameKey(cid, opsSpId), &pb.SpName{SpName: opsSpName})
	mustPut(t, cli, SpRevKey(opsShard, cid, opsSpId), &pb.SpRev{
		SpName:   opsSpName,
		Revision: 1,
	})
	mustPut(t, cli, SliceKey(cid, opsSpId, opsSliceId), opsSlice())
	mustPut(t, cli, CntlrKey(cid, opsSpId, opsCntlrA), &pb.Cntlr{
		AddrPort:   opsCnA,
		NvmeTrConf: opsTrConf(opsCnA),
		CntlidSlot: 0,
		Primary:    true,
	})
	mustPut(t, cli, CntlrKey(cid, opsSpId, opsCntlrB), &pb.Cntlr{
		AddrPort:   opsCnB,
		NvmeTrConf: opsTrConf(opsCnB),
		CntlidSlot: 1,
	})
	mustPut(t, cli, ThinDeviceKey(cid, opsSpId, "td0"), &pb.ThinDevice{
		TdId: 601, DevId: 1, Size: 1 << 30,
	})
	mustPut(t, cli, ThinDeviceKey(cid, opsSpId, "td1"), &pb.ThinDevice{
		TdId: 602, DevId: 2, Size: 1 << 30,
	})
	mustPut(t, cli, SubsystemKey(cid, opsSpId, opsNqn), &pb.Subsystem{
		SsId: opsSsId, Serial: "s0", Model: "m0",
	})
	mustPut(t, cli, CdcEntryKey(cid, opsShard, opsSpId, opsSsId), &pb.CdcEntry{
		Nqn: opsNqn,
		NvmeTrConfList: []*pb.NvmeTrConf{
			opsTrConf(opsCnA),
			opsTrConf(opsCnB),
		},
	})
	for idx, addrPort := range []string{opsDnA, opsDnB, opsDnC, opsDnD} {
		env.putDn(addrPort, uint64(700+idx), uint32(idx), opsDnFree)
	}
	for idx, addrPort := range []string{opsCnA, opsCnB, opsCnC} {
		env.putCn(addrPort, uint64(800+idx), uint32(idx), opsCnFree)
	}
	return env
}

// putDn writes one DnConf, its capacity key (when the presence rule of
// architecture.md, Capacity index keys, says it has one) and its DnRev, all
// consistently.
func (e *opsEnv) putDn(
	addrPort string,
	dnId uint64,
	shard uint32,
	freeExt uint64,
) {
	e.t.Helper()
	dn := &pb.DnConf{
		DnId:        dnId,
		ShardCode:   shard,
		NvmeTrConf:  opsTrConf(addrPort),
		Location:    addrPort,
		TotalExtCnt: 1000,
		FreeExtCnt:  freeExt,
	}
	mustPut(e.t, e.cli, DnConfKey(e.cid, addrPort), dn)
	mustPut(e.t, e.cli, DnRevKey(shard, e.cid, dnId), &pb.DnRev{
		AddrPort: addrPort,
		Revision: 1,
	})
	if binIdx, ok := DnBinIdx(freeExt, e.cc.GetDnBinConf()); ok {
		mustPut(
			e.t, e.cli,
			DnCapacityKey(e.cid, binIdx, freeExt, addrPort),
			&pb.DnCapacity{Location: addrPort},
		)
	}
}

// putCn writes one CnConf, its capacity key and its CnRev.
func (e *opsEnv) putCn(
	addrPort string,
	cnId uint64,
	shard uint32,
	freeExt uint64,
) {
	e.t.Helper()
	cn := &pb.CnConf{
		CnId:        cnId,
		ShardCode:   shard,
		NvmeTrConf:  opsTrConf(addrPort),
		Location:    addrPort,
		TotalExtCnt: 1000,
		FreeExtCnt:  freeExt,
	}
	mustPut(e.t, e.cli, CnConfKey(e.cid, addrPort), cn)
	mustPut(e.t, e.cli, CnRevKey(shard, e.cid, cnId), &pb.CnRev{
		AddrPort: addrPort,
		Revision: 1,
	})
	mustPut(
		e.t, e.cli,
		CnCapacityKey(e.cid, freeExt, addrPort),
		&pb.CnCapacity{Location: addrPort},
	)
}

// get reads one key, failing the test when it is absent.
func (e *opsEnv) get(key string, msg proto.Message) {
	e.t.Helper()
	found, err := e.cli.Get(e.ctx, key, msg)
	if err != nil {
		e.t.Fatalf("Get %s: %v", key, err)
	}
	if !found {
		e.t.Fatalf("Get %s: not found", key)
	}
}

// exists reports whether a key is present.
func (e *opsEnv) exists(key string) bool {
	e.t.Helper()
	found, err := e.cli.Get(e.ctx, key, &pb.DnCapacity{})
	if err != nil {
		e.t.Fatalf("Get %s: %v", key, err)
	}
	return found
}

// delKey removes one key, which the "candidate changed" cases use to simulate
// what a concurrent allocation does to a capacity key: the key the scan saw is
// gone, replaced by one for the node's new free count.
func (e *opsEnv) delKey(key string) {
	e.t.Helper()
	if err := e.cli.Delete(e.ctx, key); err != nil {
		e.t.Fatalf("Delete %s: %v", key, err)
	}
}

func (e *opsEnv) spConf() *pb.SpConf {
	conf := &pb.SpConf{}
	e.get(SpConfKey(e.cid, opsSpName), conf)
	return conf
}

func (e *opsEnv) slice() *pb.Slice {
	slice := &pb.Slice{}
	e.get(SliceKey(e.cid, opsSpId, opsSliceId), slice)
	return slice
}

func (e *opsEnv) cntlr(cntlrId uint64) *pb.Cntlr {
	cntlr := &pb.Cntlr{}
	e.get(CntlrKey(e.cid, opsSpId, cntlrId), cntlr)
	return cntlr
}

func (e *opsEnv) dn(addrPort string) *pb.DnConf {
	dn := &pb.DnConf{}
	e.get(DnConfKey(e.cid, addrPort), dn)
	return dn
}

func (e *opsEnv) cn(addrPort string) *pb.CnConf {
	cn := &pb.CnConf{}
	e.get(CnConfKey(e.cid, addrPort), cn)
	return cn
}

func (e *opsEnv) td(name string) *pb.ThinDevice {
	td := &pb.ThinDevice{}
	e.get(ThinDeviceKey(e.cid, opsSpId, name), td)
	return td
}

func (e *opsEnv) spRev() uint64 {
	rev := &pb.SpRev{}
	e.get(SpRevKey(opsShard, e.cid, opsSpId), rev)
	if rev.GetSpName() != opsSpName {
		e.t.Fatalf("sp rev handle: got %q, want %q", rev.GetSpName(), opsSpName)
	}
	return rev.GetRevision()
}

func (e *opsEnv) dnRev(addrPort string) uint64 {
	dn := e.dn(addrPort)
	rev := &pb.DnRev{}
	e.get(DnRevKey(dn.GetShardCode(), e.cid, dn.GetDnId()), rev)
	if rev.GetAddrPort() != addrPort {
		e.t.Fatalf("dn rev handle: got %q, want %q", rev.GetAddrPort(), addrPort)
	}
	return rev.GetRevision()
}

func (e *opsEnv) cnRev(addrPort string) uint64 {
	cn := e.cn(addrPort)
	rev := &pb.CnRev{}
	e.get(CnRevKey(cn.GetShardCode(), e.cid, cn.GetCnId()), rev)
	if rev.GetAddrPort() != addrPort {
		e.t.Fatalf("cn rev handle: got %q, want %q", rev.GetAddrPort(), addrPort)
	}
	return rev.GetRevision()
}

// dnCand rebuilds the candidate a DN candidate scan (architecture.md, Finding
// DN candidates) would have produced for a DN.
func (e *opsEnv) dnCand(addrPort string) Cand {
	e.t.Helper()
	dn := e.dn(addrPort)
	binIdx, ok := DnBinIdx(dn.GetFreeExtCnt(), e.cc.GetDnBinConf())
	if !ok {
		e.t.Fatalf("dn %s has no bin", addrPort)
	}
	return Cand{
		AddrPort: addrPort,
		Location: dn.GetLocation(),
		FreeExt:  dn.GetFreeExtCnt(),
		BinIdx:   binIdx,
	}
}

// cnCand rebuilds the candidate a CN candidate scan (architecture.md, Finding
// CN candidates) would have produced for a CN.
func (e *opsEnv) cnCand(addrPort string) Cand {
	e.t.Helper()
	cn := e.cn(addrPort)
	return Cand{
		AddrPort: addrPort,
		Location: cn.GetLocation(),
		FreeExt:  cn.GetFreeExtCnt(),
	}
}

// otherCntlrAddrs is the CN of every cntlr of the fixture SP but oldId, in
// cntlr_id_list order: the plan a caller replacing oldId scans against.
func (e *opsEnv) otherCntlrAddrs(oldId uint64) []string {
	e.t.Helper()
	var addrs []string
	for _, cntlrId := range e.spConf().GetCntlrIdList() {
		if cntlrId != oldId {
			addrs = append(addrs, e.cntlr(cntlrId).GetAddrPort())
		}
	}
	return addrs
}

// gainCntlr commits one more standby of the fixture SP on addrPort, as a
// gateway CreateCntlr would, id from next_id: the cntlr a snapshot taken
// before it does not hold. Only the records ReplaceCntlr reads are written.
func (e *opsEnv) gainCntlr(addrPort string) uint64 {
	e.t.Helper()
	conf := e.spConf()
	cntlrId := SpNextId(conf)
	conf.NextId = cntlrId + 1
	conf.CntlrIdList = append(conf.GetCntlrIdList(), cntlrId)
	mustPut(e.t, e.cli, CntlrKey(e.cid, opsSpId, cntlrId), &pb.Cntlr{
		AddrPort:   addrPort,
		NvmeTrConf: opsTrConf(addrPort),
		CntlidSlot: 2,
	})
	mustPut(e.t, e.cli, SpConfKey(e.cid, opsSpName), conf)
	return cntlrId
}

// setDeleting marks the fixture SP as being deleted (AR3 suppression).
func (e *opsEnv) setDeleting() {
	conf := e.spConf()
	conf.Deleting = true
	mustPut(e.t, e.cli, SpConfKey(e.cid, opsSpName), conf)
}

// setLevel raises the fixture SP's level (AR3 suppression).
func (e *opsEnv) setLevel(level pb.SpLevel) {
	conf := e.spConf()
	conf.SpLevel = level
	mustPut(e.t, e.cli, SpConfKey(e.cid, opsSpName), conf)
}

// setCntlrErr stamps one cntlr's err_epoch directly, bypassing the op.
func (e *opsEnv) setCntlrErr(cntlrId uint64, epoch uint64) {
	cntlr := e.cntlr(cntlrId)
	cntlr.ErrEpoch = epoch
	mustPut(e.t, e.cli, CntlrKey(e.cid, opsSpId, cntlrId), cntlr)
}

// setCntlrSettling sets one cntlr's settling flag directly, bypassing the ops.
func (e *opsEnv) setCntlrSettling(cntlrId uint64, settling bool) {
	cntlr := e.cntlr(cntlrId)
	cntlr.Settling = settling
	mustPut(e.t, e.cli, CntlrKey(e.cid, opsSpId, cntlrId), cntlr)
}

// setThresholds stores the SP's primary_unhealthy and cntlr_unhealthy, the
// other two left to their defaults.
func (e *opsEnv) setThresholds(primary uint32, cntlr uint32) {
	conf := e.spConf()
	conf.EventThreshold = &pb.EventThreshold{
		PrimaryUnhealthy: primary,
		CntlrUnhealthy:   cntlr,
	}
	mustPut(e.t, e.cli, SpConfKey(e.cid, opsSpName), conf)
}

// modRev is one key's mod_revision, which tells a no-op op (no put) from one
// that rewrote the same value.
func (e *opsEnv) modRev(key string) int64 {
	e.t.Helper()
	keys, _, err := e.cli.RangeKeys(e.ctx, key)
	if err != nil {
		e.t.Fatalf("RangeKeys %s: %v", key, err)
	}
	for _, kr := range keys {
		if kr.Key == key {
			return kr.ModRev
		}
	}
	e.t.Fatalf("RangeKeys %s: not found", key)
	return 0
}

// addSpare appends one spare leg to the fixture's data group.
func (e *opsEnv) addSpare(provisioned bool) {
	slice := e.slice()
	grp := findGroup(slice, opsDataGrpId)
	grp.SpareLegList = append(grp.SpareLegList, &pb.Leg{
		LegId:    opsSpareLeg,
		LegIdx:   2,
		SideList: []*pb.Side{opsSide(opsSpareSide, opsDnC, provisioned)},
	})
	mustPut(e.t, e.cli, SliceKey(e.cid, opsSpId, opsSliceId), slice)
}

// grpDnAddrs is the plan a caller of CreateSpareLeg scans against, read now:
// the DN of every side of every leg and every spare of the group, as the
// worker's grpAddrs and the gateway's grpDnAddrs build it. A group the slice
// does not hold has none.
func (e *opsEnv) grpDnAddrs(grpId uint64) []string {
	e.t.Helper()
	grp := findGroup(e.slice(), grpId)
	var addrs []string
	for _, legList := range [][]*pb.Leg{
		grp.GetLegList(), grp.GetSpareLegList(),
	} {
		for _, leg := range legList {
			for _, side := range leg.GetSideList() {
				addrs = append(addrs, side.GetAddrPort())
			}
		}
	}
	return addrs
}

// putMetaClone plants one clone of the fixture SP holding units metadata
// units, latched or not, and lists its name in clone_name_list. No node is
// charged: the tests that need the charge put it with setCnUsed.
func (e *opsEnv) putMetaClone(
	name string,
	cloneId uint64,
	units uint64,
	deleting bool,
) {
	e.t.Helper()
	conf := e.spConf()
	if !slices.Contains(conf.GetCloneNameList(), name) {
		conf.CloneNameList = append(conf.GetCloneNameList(), name)
		mustPut(e.t, e.cli, SpConfKey(e.cid, opsSpName), conf)
	}
	mustPut(e.t, e.cli, CloneKey(e.cid, opsSpId, name), &pb.Clone{
		CloneId:   cloneId,
		SrcNqn:    "nqn.2024-01.io.dnv:src",
		DstTdId:   601,
		Deleting:  deleting,
		MetaUnits: units,
	})
}

// setCnUsed sets a CN's used metadata units, in its record and in its
// capacity value alike, as the charges and releases of architecture.md,
// Metadata units, leave the two.
func (e *opsEnv) setCnUsed(addrPort string, used uint64) {
	e.t.Helper()
	cn := e.cn(addrPort)
	cn.UsedMetaUnits = used
	mustPut(e.t, e.cli, CnConfKey(e.cid, addrPort), cn)
	if CnAllocatable(cn) {
		mustPut(e.t, e.cli,
			CnCapacityKey(e.cid, cn.GetFreeExtCnt(), addrPort),
			CnCapacityValue(cn))
	}
}

// cnCapValue is the value of the capacity key a CN's record implies, which
// must be there.
func (e *opsEnv) cnCapValue(addrPort string) *pb.CnCapacity {
	e.t.Helper()
	cn := e.cn(addrPort)
	value := &pb.CnCapacity{}
	e.get(CnCapacityKey(e.cid, cn.GetFreeExtCnt(), addrPort), value)
	return value
}

// wantPrecondition asserts that err is an *ErrPrecondition raised by op. It
// also asserts the EU4 contract: the error wraps etcdutil.ErrNoCommit, which
// is what aborts the transaction without commit and without retry.
func wantPrecondition(t *testing.T, err error, op string) *ErrPrecondition {
	t.Helper()
	if err == nil {
		t.Fatalf("want *ErrPrecondition, got nil")
	}
	var precondition *ErrPrecondition
	if !errors.As(err, &precondition) {
		t.Fatalf("want *ErrPrecondition, got %T: %v", err, err)
	}
	if precondition.Op != op {
		t.Errorf("Op: got %q, want %q", precondition.Op, op)
	}
	if !errors.Is(err, etcdutil.ErrNoCommit) {
		t.Errorf("want errors.Is(err, etcdutil.ErrNoCommit)")
	}
	return precondition
}

// ---------------------------------------------------------------------------
// MD7
// ---------------------------------------------------------------------------

func TestErrPreconditionUnwrap(t *testing.T) {
	err := error(&ErrPrecondition{Op: "GrowSlice", Reason: "candidate changed"})
	if !errors.Is(err, etcdutil.ErrNoCommit) {
		t.Errorf("ErrPrecondition must wrap etcdutil.ErrNoCommit")
	}
	want := "GrowSlice: precondition failed: candidate changed"
	if err.Error() != want {
		t.Errorf("Error(): got %q, want %q", err.Error(), want)
	}
	var precondition *ErrPrecondition
	if !errors.As(err, &precondition) {
		t.Fatalf("errors.As failed")
	}
	if precondition.Reason != "candidate changed" {
		t.Errorf("Reason: got %q", precondition.Reason)
	}
}

// TestErrPreconditionAbortsWithoutCommit is the EU4/MD7 property every op
// depends on: an ErrPrecondition returned from inside the callback aborts the
// transaction, reaches the caller unchanged, and leaves nothing written.
func TestErrPreconditionAbortsWithoutCommit(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	cid := testCid(t)
	key := SpNameKey(cid, 1)
	attempts := 0
	sentinel := &ErrPrecondition{Op: "TestOp", Reason: "nope"}
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		attempts++
		s.Put(key, &pb.SpName{SpName: "written"})
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("RunSTM must return the error unchanged: got %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts: got %d, want 1 (no retry)", attempts)
	}
	found, err := cli.Get(ctx, key, &pb.SpName{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Errorf("the aborted transaction must not have written %s", key)
	}
}

// ---------------------------------------------------------------------------
// Thresholds and geometry (no etcd needed)
// ---------------------------------------------------------------------------

// TestResolveEventThreshold pins one of the two conf messages still resolved
// at READ time, deliberately (architecture.md, Common validation):
// event_threshold is a policy timer, not
// geometry — nothing is formatted or addressed with it — so CreateStoragePool
// stores it verbatim and a stored zero here keeps meaning "the default".
// (The other is the DmCloneConf hydration pair, resolved in worker/sprole.go.)
func TestResolveEventThreshold(t *testing.T) {
	resolved := ResolveEventThreshold(nil)
	if resolved.GetPrimaryUnhealthy() != common.DefaultPrimaryUnhealthy ||
		resolved.GetCntlrUnhealthy() != common.DefaultCntlrUnhealthy ||
		resolved.GetSideUnhealthy() != common.DefaultSideUnhealthy ||
		resolved.GetLegUnhealthy() != common.DefaultLegUnhealthy {
		t.Errorf("nil must resolve to the Common validation defaults: %v", resolved)
	}
	stored := &pb.EventThreshold{PrimaryUnhealthy: 11, LegUnhealthy: 22}
	resolved = ResolveEventThreshold(stored)
	if resolved.GetPrimaryUnhealthy() != 11 || resolved.GetLegUnhealthy() != 22 {
		t.Errorf("stored values must survive: %v", resolved)
	}
	if resolved.GetCntlrUnhealthy() != common.DefaultCntlrUnhealthy ||
		resolved.GetSideUnhealthy() != common.DefaultSideUnhealthy {
		t.Errorf("zero fields must default: %v", resolved)
	}
	if stored.GetCntlrUnhealthy() != 0 {
		t.Errorf("the input must not be mutated")
	}
}

func TestThresholdReached(t *testing.T) {
	for _, tc := range []struct {
		name      string
		now       uint64
		errEpoch  uint64
		threshold uint64
		want      bool
	}{
		{"healthy", 1000, 0, 5, false},
		{"exactly", 1005, 1000, 5, true},
		{"before", 1004, 1000, 5, false},
		{"after", 2000, 1000, 5, true},
		{"clock went backwards", 900, 1000, 5, false},
	} {
		got := thresholdReached(tc.now, tc.errEpoch, tc.threshold)
		if got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestGroupBlocks pins a worked example of the group geometry
// (architecture.md, Group on-leg layout: meta region, data region, health
// block): a 1 TiB raid1 group with 1 GiB extents, 1 MiB blocks and 128-block
// bitmap chunks has meta_blocks = 3.
func TestGroupBlocks(t *testing.T) {
	raid1 := testRaid1BdevConf(opsBlockSize, 128)
	metaBlocks, dataBlocks, err := GroupBlocks(1024, opsExtSize, raid1)
	if err != nil {
		t.Fatalf("GroupBlocks: %v", err)
	}
	if metaBlocks != 3 || dataBlocks != 1024*1024-3 {
		t.Errorf(
			"1 TiB raid1 group: got meta %d data %d, want 3 / %d",
			metaBlocks, dataBlocks, 1024*1024-3,
		)
	}
	// RedundNone keeps only the health block.
	none := testBdevConf(opsBlockSize)
	metaBlocks, dataBlocks, err = GroupBlocks(1, opsExtSize, none)
	if err != nil {
		t.Fatalf("GroupBlocks none: %v", err)
	}
	if metaBlocks != 1 || dataBlocks != 1023 {
		t.Errorf(
			"1 GiB RedundNone group: got meta %d data %d, want 1 / 1023",
			metaBlocks, dataBlocks,
		)
	}
	if _, _, err := GroupBlocks(0, opsExtSize, none); err == nil {
		t.Errorf("ext_cnt 0 must fail")
	}
	// A group whose extents cannot even hold the meta region is refused.
	tiny := testBdevConf(opsExtSize)
	if _, _, err := GroupBlocks(1, opsExtSize, tiny); err == nil {
		t.Errorf("a one-block group must fail")
	}
}

// TestGroupBlocksRefusesAZero pins that the three numbers the group geometry
// (architecture.md, Group on-leg layout: meta region, data region, health
// block) needs are stored values, concrete since the create RPC wrote them
// (architecture.md, Common validation), and that a zero is corruption or
// foreign data — GroupBlocks refuses it by name instead of guessing a geometry
// a pool may not have been formatted with.
func TestGroupBlocksRefusesAZero(t *testing.T) {
	noBlockSize := testRaid1BdevConf(opsBlockSize, 128)
	noBlockSize.DmPoolConf.DataBlockSize = 0
	cases := []struct {
		name       string
		extentSize uint64
		conf       *pb.BdevConf
		wantErr    string
	}{
		{
			name:       "extent_size zero",
			extentSize: 0,
			conf:       testRaid1BdevConf(opsBlockSize, 128),
			wantErr: "group blocks: invalid stored conf: " +
				"dn_bin_conf.extent_size is zero",
		},
		{
			name:       "data_block_size zero",
			extentSize: opsExtSize,
			conf:       noBlockSize,
			wantErr: "group blocks: invalid stored conf: " +
				"bdev_conf.dm_pool_conf.data_block_size is zero",
		},
		{
			name:       "md-raid1 with no bitmap chunk count",
			extentSize: opsExtSize,
			conf:       testRaid1BdevConf(opsBlockSize, 0),
			wantErr: "group blocks: invalid stored conf: " +
				"bdev_conf.redund_conf.redund_md_raid1." +
				"bitmap_chunk_block_cnt is zero",
		},
	}
	for _, tc := range cases {
		_, _, err := GroupBlocks(1024, tc.extentSize, tc.conf)
		if err == nil || err.Error() != tc.wantErr {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}

// TestSideZeroBytes pins the length to zero of a side (architecture.md, Side
// provisioning protocol) on groups the group geometry really produces, at the
// fixture's 1 GiB extents and 1 MiB blocks: a side of a meta group is zeroed
// over its whole leg, which is the whole side whenever the extent size is a
// multiple of the block size, and a side of a data group over its meta region
// and its first data block — md superblock, bitmap and health block with
// md-raid1, the health block alone without.
func TestSideZeroBytes(t *testing.T) {
	groupOf := func(extentSize uint64, conf *pb.BdevConf) *pb.Group {
		t.Helper()
		metaBlocks, dataBlocks, err := GroupBlocks(1, extentSize, conf)
		if err != nil {
			t.Fatalf("GroupBlocks: %v", err)
		}
		return &pb.Group{
			GrpId:      7,
			ExtCnt:     1,
			MetaBlocks: metaBlocks,
			DataBlocks: dataBlocks,
		}
	}
	raid1 := groupOf(opsExtSize, testRaid1BdevConf(opsBlockSize, 128))
	none := groupOf(opsExtSize, testBdevConf(opsBlockSize))
	// An extent size that is not a multiple of the block size: the leg ends
	// at its last whole block, and the tail behind it is addressed by no
	// node, so it is not zeroed either.
	ragged := groupOf(
		opsExtSize+64*1024, testRaid1BdevConf(opsBlockSize, 128))
	cases := []struct {
		name   string
		grp    *pb.Group
		isMeta bool
		want   uint64
	}{
		{"meta side: the whole side", raid1, true, opsExtSize},
		{"md-raid1 data side: three meta blocks and one data block",
			raid1, false, 4 * opsBlockSize},
		{"redund_none data side: the health block and one data block",
			none, false, 2 * opsBlockSize},
		{"redund_none meta side: the whole side", none, true, opsExtSize},
		{"meta side of a ragged extent: the leg, short of the side",
			ragged, true, opsExtSize},
		// The counts are the group's STORED ones, never recomputed: these
		// two match no geometry at all.
		{"stored counts, meta side",
			&pb.Group{MetaBlocks: 5, DataBlocks: 9}, true, 14 * opsBlockSize},
		{"stored counts, data side",
			&pb.Group{MetaBlocks: 5, DataBlocks: 9}, false, 6 * opsBlockSize},
	}
	for _, tc := range cases {
		got, err := SideZeroBytes(tc.grp, tc.isMeta, opsBlockSize)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %d (%v), want %d", tc.name, got, err, tc.want)
		}
	}
}

// TestSideZeroBytesRefuses pins that SideZeroBytes, like GroupBlocks, resolves
// nothing (architecture.md, Common validation): a zero among its stored
// inputs, a length that overflows, or one the dn agent's side conf gate would
// refuse as not a whole multiple of common.DnZeroAlign (dnagent.md DN8) is an
// invalid stored conf naming the group, and never a length of zero the sp
// worker could send.
func TestSideZeroBytesRefuses(t *testing.T) {
	grp := func(metaBlocks uint64, dataBlocks uint64) *pb.Group {
		return &pb.Group{
			GrpId:      7,
			ExtCnt:     1,
			MetaBlocks: metaBlocks,
			DataBlocks: dataBlocks,
		}
	}
	cases := []struct {
		name      string
		grp       *pb.Group
		isMeta    bool
		blockSize uint64
		wantErr   string
	}{
		{
			name:      "data_block_size zero",
			grp:       grp(3, 1021),
			isMeta:    true,
			blockSize: 0,
			wantErr: "invalid stored conf: " +
				"bdev_conf.dm_pool_conf.data_block_size is zero",
		},
		{
			name:      "meta_blocks zero",
			grp:       grp(0, 1021),
			isMeta:    false,
			blockSize: opsBlockSize,
			wantErr:   "invalid stored conf: group 7 meta_blocks is zero",
		},
		{
			// A data side's length takes one data block whatever the
			// count, but a stored zero is corruption all the same.
			name:      "data_blocks zero",
			grp:       grp(3, 0),
			isMeta:    false,
			blockSize: opsBlockSize,
			wantErr:   "invalid stored conf: group 7 data_blocks is zero",
		},
		{
			name:      "the block counts overflow",
			grp:       grp(math.MaxUint64, 1),
			isMeta:    true,
			blockSize: opsBlockSize,
			wantErr: fmt.Sprintf("invalid stored conf: group 7 "+
				"meta_blocks %d and data_blocks 1 overflow the length to "+
				"zero at data_block_size %d",
				uint64(math.MaxUint64), opsBlockSize),
		},
		{
			name:      "the bytes overflow, meta side",
			grp:       grp(1<<43, 1<<43),
			isMeta:    true,
			blockSize: opsBlockSize,
			wantErr: fmt.Sprintf("invalid stored conf: group 7 "+
				"meta_blocks %d and data_blocks %d overflow the length to "+
				"zero at data_block_size %d",
				uint64(1<<43), uint64(1<<43), opsBlockSize),
		},
		{
			name:      "the bytes overflow, data side",
			grp:       grp(1<<44, 1),
			isMeta:    false,
			blockSize: opsBlockSize,
			wantErr: fmt.Sprintf("invalid stored conf: group 7 "+
				"meta_blocks %d and data_blocks 1 overflow the length to "+
				"zero at data_block_size %d",
				uint64(1<<44), opsBlockSize),
		},
		{
			name:      "not a whole multiple, data side",
			grp:       grp(3, 1021),
			isMeta:    false,
			blockSize: 512,
			wantErr: "invalid stored conf: group 7 length to zero 2048 " +
				"is not a multiple of 4096",
		},
		{
			name:      "not a whole multiple, meta side",
			grp:       grp(3, 5),
			isMeta:    true,
			blockSize: 1000,
			wantErr: "invalid stored conf: group 7 length to zero 8000 " +
				"is not a multiple of 4096",
		},
	}
	for _, tc := range cases {
		got, err := SideZeroBytes(tc.grp, tc.isMeta, tc.blockSize)
		if err == nil || err.Error() != tc.wantErr {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.wantErr)
		}
		if got != 0 {
			t.Errorf("%s: a refusal returned the length %d", tc.name, got)
		}
	}
}

// TestPoolBlockSize pins the plain accessor (architecture.md, Common
// validation): it reports the
// stored value and substitutes nothing, so a conf that somehow carries a zero
// reports zero and its caller's ValidateBdevConf gate is what refuses it — a
// silent 1 MiB here would address a pool in a unit it was never formatted
// with.
func TestPoolBlockSize(t *testing.T) {
	if got := PoolBlockSize(testBdevConf(64 * 1024)); got != 64*1024 {
		t.Errorf("stored data_block_size: got %d, want %d", got, 64*1024)
	}
	if got := PoolBlockSize(&pb.BdevConf{}); got != 0 {
		t.Errorf("an empty bdev_conf: got %d, want 0", got)
	}
	if got := PoolBlockSize(nil); got != 0 {
		t.Errorf("a nil bdev_conf: got %d, want 0", got)
	}
}

// TestMetaLadderExtCnt walks the meta ladder of architecture.md, GrowSlice,
// 1 → 2 → 4 → 8 → 16 GiB and
// refuses at the cap.
func TestMetaLadderExtCnt(t *testing.T) {
	total := uint64(1)
	for _, want := range []uint64{1, 2, 4, 8} {
		extCnt, ok := MetaLadderExtCnt(total, opsExtSize)
		if !ok {
			t.Fatalf("total %d: unexpectedly capped", total)
		}
		if extCnt != want {
			t.Fatalf("total %d: got %d, want %d", total, extCnt, want)
		}
		total += extCnt
	}
	if total != 16 {
		t.Fatalf("ladder total: got %d, want 16", total)
	}
	if _, ok := MetaLadderExtCnt(total, opsExtSize); ok {
		t.Errorf("16 GiB must be the cap")
	}
	if _, ok := MetaLadderExtCnt(0, opsExtSize); ok {
		t.Errorf("a slice with no meta group has no ladder step")
	}
	// With 1 TiB extents the very first meta group is already past the cap.
	if _, ok := MetaLadderExtCnt(1, common.MaxDnExtSize); ok {
		t.Errorf("1 TiB extents must cap immediately")
	}
	// A zero extent size is not defaulted (architecture.md, Common
	// validation): it is a divide by zero,
	// so the last-resort guard reports false rather than panicking. Callers
	// validate the ClusterConf first, which is what keeps this arm
	// unreachable in production.
	if extCnt, ok := MetaLadderExtCnt(1, 0); ok || extCnt != 0 {
		t.Errorf("extent_size 0 gave (%d, %v), want (0, false)", extCnt, ok)
	}
}

// ---------------------------------------------------------------------------
// err_epoch maintenance
// ---------------------------------------------------------------------------

func TestSetDnErrEpoch(t *testing.T) {
	env := newOpsEnv(t)
	capKey := DnCapacityKey(
		env.cid, mustBin(t, env, opsDnFree), opsDnFree, opsDnA,
	)
	if !env.exists(capKey) {
		t.Fatalf("fixture: dn-a must start allocatable")
	}
	revBefore := env.dnRev(opsDnA)
	// Set: the record takes the epoch and loses its capacity key.
	if err := SetDnErrEpoch(
		env.ctx, env.cli, env.cid, opsDnA, 1000, env.cc,
	); err != nil {
		t.Fatalf("SetDnErrEpoch: %v", err)
	}
	if got := env.dn(opsDnA).GetErrEpoch(); got != 1000 {
		t.Errorf("err_epoch: got %d, want 1000", got)
	}
	if env.exists(capKey) {
		t.Errorf("an unhealthy DN must have no capacity key (architecture.md, Capacity index keys)")
	}
	if got := env.dnRev(opsDnA); got != revBefore {
		t.Errorf("err_epoch must not bump DnRev: got %d, want %d", got, revBefore)
	}
	// A second, later observation never restarts the clock.
	if err := SetDnErrEpoch(
		env.ctx, env.cli, env.cid, opsDnA, 2000, env.cc,
	); err != nil {
		t.Fatalf("SetDnErrEpoch again: %v", err)
	}
	if got := env.dn(opsDnA).GetErrEpoch(); got != 1000 {
		t.Errorf("the threshold clock must not restart: got %d", got)
	}
	// Clear: the capacity key comes back.
	if err := SetDnErrEpoch(
		env.ctx, env.cli, env.cid, opsDnA, 0, env.cc,
	); err != nil {
		t.Fatalf("SetDnErrEpoch clear: %v", err)
	}
	if got := env.dn(opsDnA).GetErrEpoch(); got != 0 {
		t.Errorf("err_epoch: got %d, want 0", got)
	}
	if !env.exists(capKey) {
		t.Errorf("a recovered DN must be allocatable again")
	}
	if got := env.dnRev(opsDnA); got != revBefore {
		t.Errorf("clearing must not bump DnRev either: got %d", got)
	}
	err := SetDnErrEpoch(env.ctx, env.cli, env.cid, "dn-x:9000", 1, env.cc)
	wantPrecondition(t, err, opSetDnErrEpoch)
}

// mustBin is the bin a DN with freeExt free extents sits in.
func mustBin(t *testing.T, env *opsEnv, freeExt uint64) uint32 {
	t.Helper()
	binIdx, ok := DnBinIdx(freeExt, env.cc.GetDnBinConf())
	if !ok {
		t.Fatalf("free %d has no bin", freeExt)
	}
	return binIdx
}

func TestSetCnErrEpoch(t *testing.T) {
	env := newOpsEnv(t)
	capKey := CnCapacityKey(env.cid, opsCnFree, opsCnA)
	revBefore := env.cnRev(opsCnA)
	if err := SetCnErrEpoch(
		env.ctx, env.cli, env.cid, opsCnA, 1000,
	); err != nil {
		t.Fatalf("SetCnErrEpoch: %v", err)
	}
	if got := env.cn(opsCnA).GetErrEpoch(); got != 1000 {
		t.Errorf("err_epoch: got %d, want 1000", got)
	}
	if env.exists(capKey) {
		t.Errorf("an unhealthy CN must have no capacity key")
	}
	if err := SetCnErrEpoch(
		env.ctx, env.cli, env.cid, opsCnA, 2000,
	); err != nil {
		t.Fatalf("SetCnErrEpoch again: %v", err)
	}
	if got := env.cn(opsCnA).GetErrEpoch(); got != 1000 {
		t.Errorf("the threshold clock must not restart: got %d", got)
	}
	if err := SetCnErrEpoch(env.ctx, env.cli, env.cid, opsCnA, 0); err != nil {
		t.Fatalf("SetCnErrEpoch clear: %v", err)
	}
	if !env.exists(capKey) {
		t.Errorf("a recovered CN must be allocatable again")
	}
	if got := env.cnRev(opsCnA); got != revBefore {
		t.Errorf("err_epoch must not bump CnRev: got %d", got)
	}
	err := SetCnErrEpoch(env.ctx, env.cli, env.cid, "cn-x:9000", 1)
	wantPrecondition(t, err, opSetCnErrEpoch)
}

func TestSetCntlrErrEpoch(t *testing.T) {
	env := newOpsEnv(t)
	revBefore := env.spRev()
	if err := SetCntlrErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsCntlrA, 1000, false,
	); err != nil {
		t.Fatalf("SetCntlrErrEpoch: %v", err)
	}
	if got := env.cntlr(opsCntlrA).GetErrEpoch(); got != 1000 {
		t.Errorf("err_epoch: got %d, want 1000", got)
	}
	if err := SetCntlrErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsCntlrA, 2000, false,
	); err != nil {
		t.Fatalf("SetCntlrErrEpoch again: %v", err)
	}
	if got := env.cntlr(opsCntlrA).GetErrEpoch(); got != 1000 {
		t.Errorf("the threshold clock must not restart: got %d", got)
	}
	if err := SetCntlrErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsCntlrA, 0, false,
	); err != nil {
		t.Fatalf("SetCntlrErrEpoch clear: %v", err)
	}
	if got := env.cntlr(opsCntlrA).GetErrEpoch(); got != 0 {
		t.Errorf("err_epoch: got %d, want 0", got)
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("health must not bump SpRev: got %d, want %d", got, revBefore)
	}
	err := SetCntlrErrEpoch(env.ctx, env.cli, env.cid, opsSpId, 999, 1, false)
	wantPrecondition(t, err, opSetCntlrErrEpoch)
}

// TestSetCntlrErrEpochSettle pins the settle half of the op (HL2): the flag
// is cleared only by settle = true with a zero epoch, in the same STM as the
// err_epoch clear, and a call that changes nothing writes nothing.
func TestSetCntlrErrEpochSettle(t *testing.T) {
	env := newOpsEnv(t)
	key := CntlrKey(env.cid, opsSpId, opsCntlrA)
	env.setCntlrSettling(opsCntlrA, true)
	env.setCntlrErr(opsCntlrA, 1000)
	revBefore := env.spRev()
	set := func(epoch uint64, settle bool) {
		t.Helper()
		if err := SetCntlrErrEpoch(
			env.ctx, env.cli, env.cid, opsSpId, opsCntlrA, epoch, settle,
		); err != nil {
			t.Fatalf("SetCntlrErrEpoch(%d, %v): %v", epoch, settle, err)
		}
	}
	// A settle with a nonzero epoch proves no role: nothing changes.
	modBefore := env.modRev(key)
	set(2000, true)
	if !env.cntlr(opsCntlrA).GetSettling() {
		t.Errorf("a settle with a nonzero epoch must not clear the flag")
	}
	if got := env.modRev(key); got != modBefore {
		t.Errorf("a call that changes nothing must not put: mod_revision "+
			"%d -> %d", modBefore, got)
	}
	// settle = false clears err_epoch and leaves the flag.
	set(0, false)
	got := env.cntlr(opsCntlrA)
	if got.GetErrEpoch() != 0 || !got.GetSettling() {
		t.Errorf("settle=false: got err_epoch %d settling %v, want 0 true",
			got.GetErrEpoch(), got.GetSettling())
	}
	// settle = true with epoch 0 on a record already clean clears the flag
	// alone: the settle is a write of its own.
	set(0, true)
	got = env.cntlr(opsCntlrA)
	if got.GetErrEpoch() != 0 || got.GetSettling() {
		t.Errorf("settle=true: got err_epoch %d settling %v, want 0 false",
			got.GetErrEpoch(), got.GetSettling())
	}
	// Again on a settled record: a no-op, no put.
	modBefore = env.modRev(key)
	set(0, true)
	if got := env.modRev(key); got != modBefore {
		t.Errorf("settling an already settled record must not put: "+
			"mod_revision %d -> %d", modBefore, got)
	}
	// The flag and err_epoch clear together in one write.
	env.setCntlrSettling(opsCntlrA, true)
	env.setCntlrErr(opsCntlrA, 3000)
	set(0, true)
	got = env.cntlr(opsCntlrA)
	if got.GetErrEpoch() != 0 || got.GetSettling() {
		t.Errorf("combined clear: got err_epoch %d settling %v, want 0 false",
			got.GetErrEpoch(), got.GetSettling())
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("a settle must not bump SpRev: got %d, want %d",
			got, revBefore)
	}
}

func TestSetLegErrEpoch(t *testing.T) {
	env := newOpsEnv(t)
	env.addSpare(false)
	revBefore := env.spRev()
	// An active leg of the data group.
	if err := SetLegErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsSliceId, opsDataLegA, 1000,
	); err != nil {
		t.Fatalf("SetLegErrEpoch: %v", err)
	}
	if got := findLeg(env.slice(), opsDataLegA).GetErrEpoch(); got != 1000 {
		t.Errorf("leg err_epoch: got %d, want 1000", got)
	}
	// A leg of the META group is found too.
	if err := SetLegErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsSliceId, opsMetaLegB, 1100,
	); err != nil {
		t.Fatalf("SetLegErrEpoch meta: %v", err)
	}
	if got := findLeg(env.slice(), opsMetaLegB).GetErrEpoch(); got != 1100 {
		t.Errorf("meta leg err_epoch: got %d, want 1100", got)
	}
	// A SPARE leg is found too (architecture.md, Spare legs: spares are probed
	// like active legs).
	if err := SetLegErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsSliceId, opsSpareLeg, 1200,
	); err != nil {
		t.Fatalf("SetLegErrEpoch spare: %v", err)
	}
	if got := findLeg(env.slice(), opsSpareLeg).GetErrEpoch(); got != 1200 {
		t.Errorf("spare leg err_epoch: got %d, want 1200", got)
	}
	// No restart, then clear.
	if err := SetLegErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsSliceId, opsDataLegA, 9000,
	); err != nil {
		t.Fatalf("SetLegErrEpoch again: %v", err)
	}
	if got := findLeg(env.slice(), opsDataLegA).GetErrEpoch(); got != 1000 {
		t.Errorf("the threshold clock must not restart: got %d", got)
	}
	if err := SetLegErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsSliceId, opsDataLegA, 0,
	); err != nil {
		t.Fatalf("SetLegErrEpoch clear: %v", err)
	}
	if got := findLeg(env.slice(), opsDataLegA).GetErrEpoch(); got != 0 {
		t.Errorf("leg err_epoch: got %d, want 0", got)
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("health must not bump SpRev: got %d, want %d", got, revBefore)
	}
	err := SetLegErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsSliceId, 9999, 1,
	)
	wantPrecondition(t, err, opSetLegErrEpoch)
	err = SetLegErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, 9999, opsDataLegA, 1,
	)
	wantPrecondition(t, err, opSetLegErrEpoch)
}

func TestSetSideErrEpoch(t *testing.T) {
	env := newOpsEnv(t)
	env.addSpare(false)
	revBefore := env.spRev()
	if err := SetSideErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsSliceId, opsDataSideB, 1000,
	); err != nil {
		t.Fatalf("SetSideErrEpoch: %v", err)
	}
	if got := findSide(env.slice(), opsDataSideB).GetErrEpoch(); got != 1000 {
		t.Errorf("side err_epoch: got %d, want 1000", got)
	}
	if err := SetSideErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsSliceId, opsSpareSide, 1100,
	); err != nil {
		t.Fatalf("SetSideErrEpoch spare: %v", err)
	}
	if got := findSide(env.slice(), opsSpareSide).GetErrEpoch(); got != 1100 {
		t.Errorf("spare side err_epoch: got %d, want 1100", got)
	}
	if err := SetSideErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsSliceId, opsDataSideB, 9000,
	); err != nil {
		t.Fatalf("SetSideErrEpoch again: %v", err)
	}
	if got := findSide(env.slice(), opsDataSideB).GetErrEpoch(); got != 1000 {
		t.Errorf("the threshold clock must not restart: got %d", got)
	}
	if err := SetSideErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsSliceId, opsDataSideB, 0,
	); err != nil {
		t.Fatalf("SetSideErrEpoch clear: %v", err)
	}
	if got := findSide(env.slice(), opsDataSideB).GetErrEpoch(); got != 0 {
		t.Errorf("side err_epoch: got %d, want 0", got)
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("health must not bump SpRev: got %d, want %d", got, revBefore)
	}
	err := SetSideErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsSliceId, 9999, 1,
	)
	wantPrecondition(t, err, opSetSideErrEpoch)
}

// ---------------------------------------------------------------------------
// The two flips
// ---------------------------------------------------------------------------

// unprovisionAll rewrites the fixture slice with every side unprovisioned,
// which is the state a freshly created SP is in ([D15]).
func (e *opsEnv) unprovisionAll() {
	slice := opsSlice()
	for _, leg := range allLegs(slice) {
		for _, side := range leg.GetSideList() {
			side.Provisioned = false
		}
	}
	mustPut(e.t, e.cli, SliceKey(e.cid, opsSpId, opsSliceId), slice)
}

// equalSideRefs / equalTdRefs compare what a flip REPORTED with what it was
// expected to write, order included.
func equalSideRefs(got []SideRef, want []SideRef) bool {
	if len(got) != len(want) {
		return false
	}
	for idx := range got {
		if got[idx] != want[idx] {
			return false
		}
	}
	return true
}

func equalTdRefs(got []TdRef, want []TdRef) bool {
	if len(got) != len(want) {
		return false
	}
	for idx := range got {
		if got[idx] != want[idx] {
			return false
		}
	}
	return true
}

func TestFlipProvisioned(t *testing.T) {
	env := newOpsEnv(t)
	env.unprovisionAll()
	revBefore := env.spRev()
	sides := []SideRef{
		{SliceId: opsSliceId, LegId: opsDataLegA, SideId: opsDataSideA},
		{SliceId: opsSliceId, LegId: opsMetaLegB, SideId: opsMetaSideB},
	}
	flipped, revision, err := FlipProvisioned(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, sides,
	)
	if err != nil {
		t.Fatalf("FlipProvisioned: %v", err)
	}
	// The op reports the sides it WROTE, in the order they were listed, and
	// the revision its STM committed: the caller logs one "flip applied"
	// record (dnv-worker.md, Log records) per side, naming that revision.
	if !equalSideRefs(flipped, sides) {
		t.Errorf("flipped: got %v, want %v", flipped, sides)
	}
	if revision != revBefore+1 {
		t.Errorf("revision: got %d, want %d, the one the STM committed",
			revision, revBefore+1)
	}
	slice := env.slice()
	if !findSide(slice, opsDataSideA).GetProvisioned() ||
		!findSide(slice, opsMetaSideB).GetProvisioned() {
		t.Errorf("both listed sides must be provisioned")
	}
	if findSide(slice, opsDataSideB).GetProvisioned() {
		t.Errorf("an unlisted side must not be touched")
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d (exactly one bump)", got, revBefore+1)
	}
	// Idempotent: a repeated batch writes nothing and bumps nothing.
	flipped, revision, err = FlipProvisioned(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, sides,
	)
	if err != nil {
		t.Fatalf("FlipProvisioned again: %v", err)
	}
	if len(flipped) != 0 || revision != 0 {
		t.Errorf("flipped: got %v at revision %d, want none at 0", flipped,
			revision)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev must not move: got %d, want %d", got, revBefore+1)
	}
	// A stale side pointer is skipped, not an error; a wrong leg id makes
	// the pointer stale too.
	flipped, _, err = FlipProvisioned(
		env.ctx, env.cli, env.cid, opsShard, opsSpId,
		[]SideRef{
			{SliceId: opsSliceId, LegId: opsDataLegB, SideId: 9999},
			{SliceId: opsSliceId, LegId: opsMetaLegA, SideId: opsDataSideB},
		},
	)
	if err != nil {
		t.Fatalf("FlipProvisioned stale: %v", err)
	}
	if len(flipped) != 0 {
		t.Errorf("a stale pointer must flip nothing: got %v", flipped)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev must not move: got %d", got)
	}
	// A missing slice is a precondition failure and drops the whole batch.
	_, _, err = FlipProvisioned(
		env.ctx, env.cli, env.cid, opsShard, opsSpId,
		[]SideRef{
			{SliceId: opsSliceId, LegId: opsDataLegB, SideId: opsDataSideB},
			{SliceId: 9999, LegId: 1, SideId: 2},
		},
	)
	wantPrecondition(t, err, opFlipProvisioned)
	if findSide(env.slice(), opsDataSideB).GetProvisioned() {
		t.Errorf("an aborted batch must write nothing")
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("an aborted batch must not bump SpRev: got %d", got)
	}
}

func TestFlipCreated(t *testing.T) {
	env := newOpsEnv(t)
	revBefore := env.spRev()
	created, err := FlipCreated(
		env.ctx, env.cli, env.cid, opsShard, opsSpId,
		[]TdRef{
			{Name: "td0", TdId: 601},
			{Name: "td1", TdId: 999},   // td_id differs: skipped
			{Name: "gone", TdId: 1234}, // key absent: skipped
		},
	)
	if err != nil {
		t.Fatalf("FlipCreated: %v", err)
	}
	// Only the candidate the STM wrote is reported: the td_id mismatch and
	// the absent key are skipped, and the caller must not log a "flip
	// applied" record for either.
	if !equalTdRefs(created, []TdRef{{Name: "td0", TdId: 601}}) {
		t.Errorf("created: got %v, want [{td0 601}]", created)
	}
	if !env.td("td0").GetCreated() {
		t.Errorf("td0 must be created")
	}
	if env.td("td1").GetCreated() {
		t.Errorf("a td_id mismatch must be skipped")
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d (exactly one bump)", got, revBefore+1)
	}
	// Already created ⇒ nothing written, no bump.
	created, err = FlipCreated(
		env.ctx, env.cli, env.cid, opsShard, opsSpId,
		[]TdRef{{Name: "td0", TdId: 601}},
	)
	if err != nil {
		t.Fatalf("FlipCreated again: %v", err)
	}
	if len(created) != 0 {
		t.Errorf("created: got %v, want none", created)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev must not move: got %d", got)
	}
	// Two candidates in one batch share the one bump.
	mustPut(env.t, env.cli, ThinDeviceKey(env.cid, opsSpId, "td1"),
		&pb.ThinDevice{TdId: 602, DevId: 2, Size: 1 << 30})
	mustPut(env.t, env.cli, ThinDeviceKey(env.cid, opsSpId, "td2"),
		&pb.ThinDevice{TdId: 603, DevId: 3, Size: 1 << 30})
	created, err = FlipCreated(
		env.ctx, env.cli, env.cid, opsShard, opsSpId,
		[]TdRef{{Name: "td1", TdId: 602}, {Name: "td2", TdId: 603}},
	)
	if err != nil {
		t.Fatalf("FlipCreated batch: %v", err)
	}
	if !equalTdRefs(created, []TdRef{
		{Name: "td1", TdId: 602}, {Name: "td2", TdId: 603},
	}) {
		t.Errorf("created: got %v, want both", created)
	}
	if got := env.spRev(); got != revBefore+2 {
		t.Errorf("SpRev: got %d, want %d (one bump for the batch)", got, revBefore+2)
	}
}

// TestFlipCreatedWritesTheTornMark pins RW19's copy of the torn mark: the put
// that sets created writes each candidate's mark, true or false, into
// ThinDevice.torn, every other field kept, and the refs it returns carry the
// marks. A candidate the STM skips writes nothing, so a later mark cannot
// change the record of a td already created, nor clear its mark.
func TestFlipCreatedWritesTheTornMark(t *testing.T) {
	env := newOpsEnv(t)
	cands := []TdRef{
		{Name: "td0", TdId: 601, Torn: true},
		{Name: "td1", TdId: 602},
	}
	created, err := FlipCreated(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, cands,
	)
	if err != nil {
		t.Fatalf("FlipCreated: %v", err)
	}
	if !equalTdRefs(created, cands) {
		t.Fatalf("created: got %v, want %v", created, cands)
	}
	for name, want := range map[string]*pb.ThinDevice{
		"td0": {TdId: 601, DevId: 1, Size: 1 << 30, Created: true, Torn: true},
		"td1": {TdId: 602, DevId: 2, Size: 1 << 30, Created: true},
	} {
		if got := env.td(name); !proto.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	revBefore := env.spRev()
	created, err = FlipCreated(
		env.ctx, env.cli, env.cid, opsShard, opsSpId,
		[]TdRef{
			{Name: "td0", TdId: 601},
			{Name: "td1", TdId: 602, Torn: true},
		},
	)
	if err != nil {
		t.Fatalf("FlipCreated again: %v", err)
	}
	if len(created) != 0 {
		t.Errorf("created: got %v, want none", created)
	}
	if !env.td("td0").GetTorn() || env.td("td1").GetTorn() {
		t.Errorf("a skipped candidate rewrote a torn mark: td0 %v, td1 %v",
			env.td("td0").GetTorn(), env.td("td1").GetTorn())
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("SpRev must not move: got %d, want %d", got, revBefore)
	}
}

// TestFlipCreatedAtTheTdCeiling is RW19's transaction bound, committed against
// the real etcd this package runs with --max-txn-ops=common.EtcdMaxTxnOps. The
// sp worker folds every td one drain of its reports completed into ONE
// FlipCreated call, and an SP whose tds were all created before its pool came
// up can complete every one of them in one reply. One STM over such a list costs
// a compare for every key it read and every key it wrote — the td key twice
// per flipped candidate, SpRev twice — so past 511 candidates etcd refuses it,
// and refuses the same STM again on every later round: nothing ever flips and
// SpRev never moves. FlipCreated commits MaxFlipCreatedPerTxn candidates per
// transaction instead, so every candidate flips and SpRev moves once per
// transaction.
//
// 600 is past those 511, within MaxTdCntPerSp, and not a multiple of
// MaxFlipCreatedPerTxn, so the last transaction is a short one and a loop that
// dropped the remainder would leave tds uncreated.
//
// SpRev moving three times does not pin the chunk: every uniform chunk from
// 200 to 299 moves it three times over 600 candidates, and one of 257 or more
// breaks the 514 compares TestFlipCreatedTxnBudget pins. So the transactions
// are read back from the td keys: one etcd transaction is one revision, and
// the candidates, in list order, must fall into runs of exactly
// MaxFlipCreatedPerTxn, MaxFlipCreatedPerTxn and the remainder, each run
// sharing one mod_revision and the revisions ascending.
func TestFlipCreatedAtTheTdCeiling(t *testing.T) {
	const tdCnt = 600
	// The fixture has to stay a reproduction: one STM over tdCnt candidates
	// must exceed what etcd allows, and tdCnt must be a td count an SP can
	// hold.
	if 2*tdCnt+2 <= common.EtcdMaxTxnOps || tdCnt > common.MaxTdCntPerSp {
		t.Fatalf("%d candidates no longer reproduce an oversized flip: one "+
			"STM over them is %d compares against EtcdMaxTxnOps %d, and "+
			"MaxTdCntPerSp is %d", tdCnt, 2*tdCnt+2, common.EtcdMaxTxnOps,
			common.MaxTdCntPerSp)
	}
	env := newOpsEnv(t)
	cands := env.seedFlipCands(tdCnt)
	revBefore := env.spRev()

	created, err := FlipCreated(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, cands,
	)
	if err != nil {
		t.Fatalf("FlipCreated of %d candidates: %v (etcd's \"too many "+
			"operations in txn request\" here means one transaction carried "+
			"more of them than common.EtcdMaxTxnOps admits)", tdCnt, err)
	}
	if !equalTdRefs(created, cands) {
		t.Errorf("created: got %d refs, want all %d in list order",
			len(created), tdCnt)
	}
	for _, cand := range cands {
		if !env.td(cand.Name).GetCreated() {
			t.Errorf("%s (td_id %d) was not flipped", cand.Name, cand.TdId)
		}
	}
	var want []int
	for left := tdCnt; left > 0; left -= common.MaxFlipCreatedPerTxn {
		want = append(want, min(left, common.MaxFlipCreatedPerTxn))
	}
	runs, runRevs := env.flipTxnRuns(cands)
	if !slices.Equal(runs, want) || !slices.IsSorted(runRevs) {
		t.Errorf("tds per transaction in list order (by mod_revision): got "+
			"%v at revisions %v, want %v at ascending ones",
			runs, runRevs, want)
	}
	txnCnt := uint64(len(want))
	if got := env.spRev(); got != revBefore+txnCnt {
		t.Errorf("SpRev: got %d, want %d (one bump per transaction of at "+
			"most MaxFlipCreatedPerTxn = %d candidates)",
			got, revBefore+txnCnt, common.MaxFlipCreatedPerTxn)
	}
}

// TestFlipCreatedStopsAtTheFailingTxn is FlipCreated's error contract (MD6,
// MD7): an error ends the call at the transaction that failed, the ones
// before it stay committed, and the tds they wrote come back WITH the error.
// One td value no message can be decoded from fails the second transaction;
// 600 candidates leave a third one behind it, which must never run.
func TestFlipCreatedStopsAtTheFailingTxn(t *testing.T) {
	const tdCnt = 600
	const per = common.MaxFlipCreatedPerTxn
	if tdCnt <= 2*per || tdCnt > common.MaxTdCntPerSp {
		t.Fatalf("%d candidates no longer leave a transaction behind the "+
			"second one at MaxFlipCreatedPerTxn %d, within MaxTdCntPerSp %d",
			tdCnt, per, common.MaxTdCntPerSp)
	}
	env := newOpsEnv(t)
	cands := env.seedFlipCands(tdCnt)
	// One past the second transaction's first candidate, so that the
	// transaction the bad value aborts has staged writes of its own, which
	// must not land.
	bad := per + 1
	mustPut(t, env.cli, ThinDeviceKey(env.cid, opsSpId, cands[bad].Name),
		undecodable())
	revBefore := env.spRev()

	created, err := FlipCreated(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, cands,
	)
	if err == nil || !strings.Contains(err.Error(), "unmarshal") {
		t.Fatalf("FlipCreated: got error %v, want the unmarshal failure of %s",
			err, cands[bad].Name)
	}
	if !equalTdRefs(created, cands[:per]) {
		t.Errorf("created %d refs with the error, want the first %d: those "+
			"of the transaction that committed before the failing one",
			len(created), per)
	}
	var wrong []string
	for idx, cand := range cands {
		if idx == bad {
			continue // undecodable: env.td would fail the test on it
		}
		if got := env.td(cand.Name).GetCreated(); got != (idx < per) {
			wrong = append(wrong,
				fmt.Sprintf("#%d %s created=%v", idx, cand.Name, got))
		}
	}
	if len(wrong) > 0 {
		t.Errorf("%d tds carry the wrong created flag, want exactly the "+
			"first %d true; e.g. %v", len(wrong), per,
			wrong[:min(len(wrong), 3)])
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d (one bump, by the one transaction "+
			"that committed)", got, revBefore+1)
	}
}

// opsMigrName, opsMigrId and opsDstSide are the migration StartCutover's
// tests add to the fixture: its src side is opsDataSideA, and opsDstSide a
// second side of the same leg, on dn-c.
const (
	opsMigrName = "migr0"
	opsMigrId   = uint64(461)
	opsDstSide  = uint64(462)
)

// addMigration gives opsDataLegA a dst side, provisioned as given, and writes
// a Migration record with cutover_pending set, as CreateMigration does; it
// returns the CutoverRef the sp worker would build from that load.
func (e *opsEnv) addMigration(dstProvisioned bool) CutoverRef {
	e.t.Helper()
	slice := e.slice()
	leg := findLeg(slice, opsDataLegA)
	leg.SideList = append(leg.SideList,
		opsSide(opsDstSide, opsDnC, dstProvisioned))
	mustPut(e.t, e.cli, SliceKey(e.cid, opsSpId, opsSliceId), slice)
	conf := e.spConf()
	conf.MigrNameList = []string{opsMigrName}
	mustPut(e.t, e.cli, SpConfKey(e.cid, opsSpName), conf)
	mustPut(e.t, e.cli, MigrationKey(e.cid, opsSpId, opsMigrName),
		&pb.Migration{
			MigrId:         opsMigrId,
			SrcSideId:      opsDataSideA,
			DstSideId:      opsDstSide,
			CutoverPending: true,
		})
	return CutoverRef{
		MigrName:  opsMigrName,
		MigrId:    opsMigrId,
		SliceId:   opsSliceId,
		LegId:     opsDataLegA,
		SrcSideId: opsDataSideA,
		DstSideId: opsDstSide,
	}
}

func (e *opsEnv) migration() *pb.Migration {
	migr := &pb.Migration{}
	e.get(MigrationKey(e.cid, opsSpId, opsMigrName), migr)
	return migr
}

// TestStartCutover pins the cutover's STM (MD6, RW18): it clears the
// migration's cutover_pending and bumps SpRev once, and returns the revision
// it committed, and only while the SP's level is below
// SP_LEVEL_NO_MIGRATION, the flag is set, the dst side is provisioned in its
// leg and the reporting cntlr is still the primary and not disabled. Every
// refusal is an ErrPrecondition that writes nothing.
func TestStartCutover(t *testing.T) {
	env := newOpsEnv(t)
	ref := env.addMigration(false)
	revBefore := env.spRev()
	refusedIn := func(what string, spName string, ref CutoverRef,
		cntlrId uint64, reason string) {
		t.Helper()
		revision, err := StartCutover(env.ctx, env.cli, env.cid, opsShard,
			opsSpId, spName, ref, cntlrId)
		precondition := wantPrecondition(t, err, opStartCutover)
		if revision != 0 {
			t.Errorf("%s: revision %d from a refused STM, want 0", what,
				revision)
		}
		if precondition.Reason != reason {
			t.Errorf("%s: reason %q, want %q", what, precondition.Reason,
				reason)
		}
		if got := env.spRev(); got != revBefore {
			t.Errorf("%s: SpRev %d, want %d: a refusal bumps nothing", what,
				got, revBefore)
		}
		if !env.migration().GetCutoverPending() {
			t.Errorf("%s: the flag was cleared by a refused STM", what)
		}
	}
	refused := func(what string, ref CutoverRef, cntlrId uint64,
		reason string) {
		t.Helper()
		refusedIn(what, opsSpName, ref, cntlrId, reason)
	}

	refused("a dst side still zeroing", ref, opsCntlrA,
		"dst side not provisioned")

	slice := env.slice()
	findSideOfLeg(slice, opsDataLegA, opsDstSide).Provisioned = true
	mustPut(t, env.cli, SliceKey(env.cid, opsSpId, opsSliceId), slice)
	revBefore = env.spRev()

	refused("a standby", ref, opsCntlrB, "cntlr not primary")
	refused("a cntlr that is gone", ref, 9999, "cntlr not found")
	// A disabled primary runs the standby shape (cnagent.md CN9): its path
	// says nothing of the cntlr AR5 makes the primary next.
	primary := env.cntlr(opsCntlrA)
	primary.Disabled = true
	mustPut(t, env.cli, CntlrKey(env.cid, opsSpId, opsCntlrA), primary)
	refused("a disabled primary", ref, opsCntlrA, "cntlr disabled")
	primary.Disabled = false
	mustPut(t, env.cli, CntlrKey(env.cid, opsSpId, opsCntlrA), primary)
	// Every level from SP_LEVEL_NO_MIGRATION up keeps the dst side from
	// building its dm-clone (dnagent.md DN11), so the cutover waits there,
	// whatever the dst side's last report said.
	for _, level := range []pb.SpLevel{
		pb.SpLevel_SP_LEVEL_NO_MIGRATION,
		pb.SpLevel_SP_LEVEL_NO_SIDE,
		pb.SpLevel_SP_LEVEL_DISABLE,
	} {
		env.setLevel(level)
		refused(level.String(), ref, opsCntlrA,
			"sp level suppresses migrations")
	}
	env.setLevel(pb.SpLevel_SP_LEVEL_READWRITE)
	refusedIn("an SP that is gone", "ops-pool-gone", ref, opsCntlrA,
		"sp not found")
	conf := env.spConf()
	conf.SpId = opsSpId + 1
	mustPut(t, env.cli, SpConfKey(env.cid, opsSpName), conf)
	refused("an SP re-created under its name", ref, opsCntlrA,
		"sp id changed")
	conf.SpId = opsSpId
	mustPut(t, env.cli, SpConfKey(env.cid, opsSpName), conf)
	gone := ref
	gone.MigrName = "migr-gone"
	refused("a migration that is gone", gone, opsCntlrA,
		"migration not found")
	other := ref
	other.MigrId = opsMigrId + 1
	refused("a migration re-created under its name", other, opsCntlrA,
		"migration changed")
	other = ref
	other.DstSideId = opsDataSideB
	refused("another dst side", other, opsCntlrA, "migration changed")
	other = ref
	other.LegId = opsDataLegB
	refused("a dst side not in its leg", other, opsCntlrA,
		"dst side not found")
	other = ref
	other.SliceId = 9999
	refused("a slice that is gone", other, opsCntlrA, "slice not found")

	// The levels below SP_LEVEL_NO_MIGRATION are the CN's alone, and the dst
	// side serves at them (dnagent.md DN11): the highest of them holds
	// nothing up.
	env.setLevel(pb.SpLevel_SP_LEVEL_NO_REDUND)
	revision, err := StartCutover(env.ctx, env.cli, env.cid, opsShard,
		opsSpId, opsSpName, ref, opsCntlrA)
	if err != nil {
		t.Fatalf("StartCutover: %v", err)
	}
	if revision != revBefore+1 {
		t.Errorf("StartCutover returned revision %d, want %d, the one its "+
			"STM committed", revision, revBefore+1)
	}
	migr := env.migration()
	if migr.GetCutoverPending() {
		t.Errorf("cutover_pending still set after the cutover")
	}
	want := &pb.Migration{
		MigrId:    opsMigrId,
		SrcSideId: opsDataSideA,
		DstSideId: opsDstSide,
	}
	if !proto.Equal(migr, want) {
		t.Errorf("migration = %v, want %v: the cutover writes the flag alone",
			migr, want)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d (exactly one bump)", got,
			revBefore+1)
	}
	if !findSideOfLeg(env.slice(), opsDataLegA, opsDstSide).GetProvisioned() {
		t.Errorf("the dst side lost its provisioned flag")
	}

	// A cutover that started is not started again: a second call is
	// refused and bumps nothing.
	revBefore = env.spRev()
	_, err = StartCutover(env.ctx, env.cli, env.cid, opsShard, opsSpId,
		opsSpName, ref, opsCntlrA)
	if precondition := wantPrecondition(t, err, opStartCutover); precondition.Reason != "cutover started" {
		t.Errorf("a second cutover: reason %q, want %q",
			precondition.Reason, "cutover started")
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("a second cutover: SpRev %d, want %d", got, revBefore)
	}
}

// seedFlipCands writes n uncreated tds, td_ids counting up from opsNextId,
// and returns them as FlipCreated candidates in that order. Every name starts
// with "td-", which keeps them apart from the fixture's td0 and td1.
func (e *opsEnv) seedFlipCands(n int) []TdRef {
	e.t.Helper()
	cands := make([]TdRef, 0, n)
	for idx := 0; idx < n; idx++ {
		tdId := opsNextId + uint64(idx)
		name := "td-" + idField(tdId)
		mustPut(e.t, e.cli, ThinDeviceKey(e.cid, opsSpId, name),
			&pb.ThinDevice{TdId: tdId, DevId: uint32(10 + idx), Size: 1 << 30})
		cands = append(cands, TdRef{Name: name, TdId: tdId})
	}
	return cands
}

// flipTxnRuns reads back which transaction wrote each candidate. One etcd
// transaction is one revision, so the candidates, in list order, fall into
// runs that share a td key mod_revision: runs holds each run's length and
// revs its revision. A candidate nothing rewrote keeps the revision of its
// own seed put, and so is a run of its own.
func (e *opsEnv) flipTxnRuns(cands []TdRef) ([]int, []int64) {
	e.t.Helper()
	keys, _, err := e.cli.RangeKeys(
		e.ctx, ThinDeviceKey(e.cid, opsSpId, "td-"),
	)
	if err != nil {
		e.t.Fatalf("RangeKeys: %v", err)
	}
	modRev := make(map[string]int64, len(keys))
	for _, key := range keys {
		modRev[key.Key] = key.ModRev
	}
	var runs []int
	var revs []int64
	for _, cand := range cands {
		rev := modRev[ThinDeviceKey(e.cid, opsSpId, cand.Name)]
		if len(revs) == 0 || rev != revs[len(revs)-1] {
			runs = append(runs, 0)
			revs = append(revs, rev)
		}
		runs[len(runs)-1]++
	}
	return runs, revs
}

// undecodable is a td value no message can be decoded from: one byte that
// starts a varint and never ends it. It is carried as an unknown field, which
// proto.Marshal emits verbatim, so it goes through etcdutil's typed Put like
// every other fixture value and this package still imports no etcd client.
func undecodable() proto.Message {
	msg := &pb.ThinDevice{}
	msg.ProtoReflect().SetUnknown(protoreflect.RawFields{0xff})
	return msg
}

// ---------------------------------------------------------------------------
// Failover
// ---------------------------------------------------------------------------

func TestFailover(t *testing.T) {
	env := newOpsEnv(t)
	env.setCntlrErr(opsCntlrA, 1000)
	revBefore := env.spRev()
	now := uint64(1000 + common.DefaultPrimaryUnhealthy)
	if err := Failover(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrA, opsCntlrB, now,
	); err != nil {
		t.Fatalf("Failover: %v", err)
	}
	if env.cntlr(opsCntlrA).GetPrimary() {
		t.Errorf("the old primary must be demoted")
	}
	if !env.cntlr(opsCntlrB).GetPrimary() {
		t.Errorf("the new primary must be promoted")
	}
	if got := env.cntlr(opsCntlrA).GetErrEpoch(); got != 1000 {
		t.Errorf("failover must not touch err_epoch: got %d", got)
	}
	// HL2: the new primary settles before AR5 judges it; the old one does
	// not carry the flag into its standby role.
	if !env.cntlr(opsCntlrB).GetSettling() {
		t.Errorf("the new primary must be settling")
	}
	if env.cntlr(opsCntlrA).GetSettling() {
		t.Errorf("the demoted cntlr must not be settling")
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d", got, revBefore+1)
	}
	// Re-running is refused: the old cntlr is not primary any more.
	err := Failover(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrA, opsCntlrB, now,
	)
	wantPrecondition(t, err, opFailover)
}

// TestFailoverDisabledPrimary pins AR5's second trigger in the STM: a
// `disabled` primary fails over on its own (architecture.md, Cntlrs), so a
// HEALTHY one moves the
// role with no threshold to wait out.
func TestFailoverDisabledPrimary(t *testing.T) {
	env := newOpsEnv(t)
	old := env.cntlr(opsCntlrA)
	old.Disabled = true
	mustPut(env.t, env.cli, CntlrKey(env.cid, opsSpId, opsCntlrA), old)
	revBefore := env.spRev()
	// now == the fixture's own epoch: nothing has been unhealthy for any
	// length of time, and the op still applies.
	if err := Failover(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrA, opsCntlrB, 1000,
	); err != nil {
		t.Fatalf("Failover: %v", err)
	}
	if env.cntlr(opsCntlrA).GetPrimary() {
		t.Errorf("the disabled old primary must be demoted")
	}
	if !env.cntlr(opsCntlrB).GetPrimary() {
		t.Errorf("the new primary must be promoted")
	}
	if got := env.cntlr(opsCntlrA).GetErrEpoch(); got != 0 {
		t.Errorf("failover must not invent an err_epoch: got %d", got)
	}
	if !env.cntlr(opsCntlrA).GetDisabled() {
		t.Errorf("failover must not re-enable the old cntlr")
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d", got, revBefore+1)
	}
}

// TestFailoverSettlingPrimary pins the settling threshold in the STM (AR5,
// HL2): a settling primary fails over at cntlr_unhealthy, not before, and the
// op moves the flag with the role.
func TestFailoverSettlingPrimary(t *testing.T) {
	env := newOpsEnv(t)
	env.setCntlrErr(opsCntlrA, 1000)
	env.setCntlrSettling(opsCntlrA, true)
	if err := Failover(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrA, opsCntlrB, 1000+common.DefaultCntlrUnhealthy,
	); err != nil {
		t.Fatalf("Failover at cntlr_unhealthy: %v", err)
	}
	old, fresh := env.cntlr(opsCntlrA), env.cntlr(opsCntlrB)
	if old.GetPrimary() || !fresh.GetPrimary() {
		t.Fatalf("the role must move: old %v, new %v", old, fresh)
	}
	if old.GetSettling() {
		t.Errorf("the demoted cntlr's settling flag must be cleared")
	}
	if !fresh.GetSettling() {
		t.Errorf("the new primary must be settling")
	}
}

// TestFailoverSettlingShorterCntlrUnhealthy pins the other half of the
// selection: nothing orders the two thresholds (AR4), and where
// cntlr_unhealthy is the SHORTER a settling primary is still held to
// primary_unhealthy — the hold only ever lengthens the wait. The refusal
// short of it is TestFailoverPreconditions'.
func TestFailoverSettlingShorterCntlrUnhealthy(t *testing.T) {
	env := newOpsEnv(t)
	env.setThresholds(60, 10)
	env.setCntlrErr(opsCntlrA, 1000)
	env.setCntlrSettling(opsCntlrA, true)
	if err := Failover(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrA, opsCntlrB, 1000+60,
	); err != nil {
		t.Fatalf("Failover at primary_unhealthy: %v", err)
	}
	if !env.cntlr(opsCntlrB).GetPrimary() {
		t.Fatalf("the role did not move")
	}
}

func TestFailoverPreconditions(t *testing.T) {
	now := uint64(1000 + common.DefaultPrimaryUnhealthy)
	for _, tc := range []struct {
		name   string
		setup  func(env *opsEnv)
		oldId  uint64
		newId  uint64
		now    uint64
		reason string
	}{
		{
			name:   "old is healthy",
			setup:  func(env *opsEnv) {},
			oldId:  opsCntlrA,
			newId:  opsCntlrB,
			now:    now,
			reason: "old cntlr is healthy and enabled",
		},
		{
			name:   "threshold not reached",
			setup:  func(env *opsEnv) { env.setCntlrErr(opsCntlrA, 1000) },
			oldId:  opsCntlrA,
			newId:  opsCntlrB,
			now:    1000 + common.DefaultPrimaryUnhealthy - 1,
			reason: "primary_unhealthy not reached",
		},
		{
			name: "settling old at primary_unhealthy",
			setup: func(env *opsEnv) {
				env.setCntlrErr(opsCntlrA, 1000)
				env.setCntlrSettling(opsCntlrA, true)
			},
			oldId:  opsCntlrA,
			newId:  opsCntlrB,
			now:    now,
			reason: "cntlr_unhealthy not reached for a settling primary",
		},
		{
			name: "settling old just short of cntlr_unhealthy",
			setup: func(env *opsEnv) {
				env.setCntlrErr(opsCntlrA, 1000)
				env.setCntlrSettling(opsCntlrA, true)
			},
			oldId:  opsCntlrA,
			newId:  opsCntlrB,
			now:    1000 + common.DefaultCntlrUnhealthy - 1,
			reason: "cntlr_unhealthy not reached for a settling primary",
		},
		{
			// cntlr_unhealthy the shorter: the settling primary is held to
			// primary_unhealthy, never to less.
			name: "settling old past a shorter cntlr_unhealthy",
			setup: func(env *opsEnv) {
				env.setThresholds(60, 10)
				env.setCntlrErr(opsCntlrA, 1000)
				env.setCntlrSettling(opsCntlrA, true)
			},
			oldId:  opsCntlrA,
			newId:  opsCntlrB,
			now:    1000 + 60 - 1,
			reason: "primary_unhealthy not reached",
		},
		{
			name:   "old is not primary",
			setup:  func(env *opsEnv) { env.setCntlrErr(opsCntlrB, 1000) },
			oldId:  opsCntlrB,
			newId:  opsCntlrA,
			now:    now,
			reason: "old cntlr is not primary",
		},
		{
			name: "new is disabled",
			setup: func(env *opsEnv) {
				env.setCntlrErr(opsCntlrA, 1000)
				cntlr := env.cntlr(opsCntlrB)
				cntlr.Disabled = true
				mustPut(env.t, env.cli,
					CntlrKey(env.cid, opsSpId, opsCntlrB), cntlr)
			},
			oldId:  opsCntlrA,
			newId:  opsCntlrB,
			now:    now,
			reason: "new cntlr is disabled",
		},
		{
			name: "new is unhealthy",
			setup: func(env *opsEnv) {
				env.setCntlrErr(opsCntlrA, 1000)
				env.setCntlrErr(opsCntlrB, 1000)
			},
			oldId:  opsCntlrA,
			newId:  opsCntlrB,
			now:    now,
			reason: "new cntlr is unhealthy",
		},
		{
			name: "new is not the smallest candidate",
			setup: func(env *opsEnv) {
				env.setCntlrErr(opsCntlrA, 1000)
				// A third, smaller-id candidate appears.
				mustPut(env.t, env.cli,
					CntlrKey(env.cid, opsSpId, 199), &pb.Cntlr{
						AddrPort:   opsCnC,
						NvmeTrConf: opsTrConf(opsCnC),
					})
				conf := env.spConf()
				conf.CntlrIdList = append(conf.CntlrIdList, 199)
				mustPut(env.t, env.cli,
					SpConfKey(env.cid, opsSpName), conf)
			},
			oldId:  opsCntlrA,
			newId:  opsCntlrB,
			now:    now,
			reason: "new cntlr is not the smallest candidate",
		},
		{
			name: "sp deleting",
			setup: func(env *opsEnv) {
				env.setCntlrErr(opsCntlrA, 1000)
				env.setDeleting()
			},
			oldId:  opsCntlrA,
			newId:  opsCntlrB,
			now:    now,
			reason: "sp deleting",
		},
		{
			name: "sp level suppresses reactions",
			setup: func(env *opsEnv) {
				env.setCntlrErr(opsCntlrA, 1000)
				env.setLevel(pb.SpLevel_SP_LEVEL_NO_THINPOOL)
			},
			oldId:  opsCntlrA,
			newId:  opsCntlrB,
			now:    now,
			reason: "sp level suppresses reactions",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			tc.setup(env)
			revBefore := env.spRev()
			err := Failover(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
				tc.oldId, tc.newId, tc.now,
			)
			precondition := wantPrecondition(t, err, opFailover)
			if precondition.Reason != tc.reason {
				t.Errorf(
					"Reason: got %q, want %q",
					precondition.Reason, tc.reason,
				)
			}
			if got := env.spRev(); got != revBefore {
				t.Errorf("an aborted op must not bump SpRev: got %d", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GrowSlice
// ---------------------------------------------------------------------------

func TestGrowSliceData(t *testing.T) {
	env := newOpsEnv(t)
	legs := []Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)}
	before := struct {
		spRev  uint64
		dnCRev uint64
		dnDRev uint64
		cnARev uint64
		cnBRev uint64
	}{
		env.spRev(), env.dnRev(opsDnC), env.dnRev(opsDnD),
		env.cnRev(opsCnA), env.cnRev(opsCnB),
	}
	grpId, err := GrowSlice(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, false, opsNotPending, env.cc, legs,
	)
	if err != nil {
		t.Fatalf("GrowSlice: %v", err)
	}
	if grpId != opsNextId {
		t.Errorf("grp_id: got %d, want %d (from next_id)", grpId, opsNextId)
	}
	conf := env.spConf()
	// One group id plus two legs and two sides.
	if conf.GetNextId() != opsNextId+5 {
		t.Errorf("next_id: got %d, want %d", conf.GetNextId(), opsNextId+5)
	}
	slice := env.slice()
	if len(slice.GetDataGrpList()) != 2 {
		t.Fatalf("data groups: got %d, want 2", len(slice.GetDataGrpList()))
	}
	if len(slice.GetMetaGrpList()) != 1 {
		t.Errorf("a data grow must not touch the meta list")
	}
	grp := slice.GetDataGrpList()[1]
	if grp.GetExtCnt() != opsDataExtCnt {
		t.Errorf(
			"ext_cnt: got %d, want %d (the first data group's)",
			grp.GetExtCnt(), opsDataExtCnt,
		)
	}
	wantMeta, wantData, err := GroupBlocks(
		opsDataExtCnt, opsExtSize, conf.GetBdevConf(),
	)
	if err != nil {
		t.Fatalf("GroupBlocks: %v", err)
	}
	if grp.GetMetaBlocks() != wantMeta || grp.GetDataBlocks() != wantData {
		t.Errorf(
			"blocks: got %d/%d, want %d/%d",
			grp.GetMetaBlocks(), grp.GetDataBlocks(), wantMeta, wantData,
		)
	}
	if len(grp.GetLegList()) != 2 || len(grp.GetSpareLegList()) != 0 {
		t.Fatalf("legs: got %d active, %d spare",
			len(grp.GetLegList()), len(grp.GetSpareLegList()))
	}
	for legIdx, leg := range grp.GetLegList() {
		if leg.GetLegIdx() != uint32(legIdx) {
			t.Errorf("leg_idx: got %d, want %d", leg.GetLegIdx(), legIdx)
		}
		if leg.GetErrEpoch() != 0 {
			t.Errorf("a new leg must be healthy")
		}
		if len(leg.GetSideList()) != 1 {
			t.Fatalf("sides: got %d, want 1", len(leg.GetSideList()))
		}
		side := leg.GetSideList()[0]
		if side.GetProvisioned() {
			t.Errorf("a new side must be unprovisioned ([D15])")
		}
		if side.GetCntlidSlot() != opsSlot {
			t.Errorf(
				"cntlid_slot: got %d, want cntlid_slot_list[0] = %d",
				side.GetCntlidSlot(), opsSlot,
			)
		}
		if side.GetAddrPort() != legs[legIdx].AddrPort {
			t.Errorf(
				"addr_port: got %q, want %q",
				side.GetAddrPort(), legs[legIdx].AddrPort,
			)
		}
		if !proto.Equal(
			side.GetNvmeTrConf(), opsTrConf(legs[legIdx].AddrPort),
		) {
			t.Errorf("nvme_tr_conf must be copied from the DN")
		}
	}
	// DN bookkeeping: pointer in, budget out, capacity key moved, one bump.
	for _, addrPort := range []string{opsDnC, opsDnD} {
		dn := env.dn(addrPort)
		if dn.GetFreeExtCnt() != opsDnFree-opsDataExtCnt {
			t.Errorf(
				"%s free_ext_cnt: got %d, want %d",
				addrPort, dn.GetFreeExtCnt(), opsDnFree-opsDataExtCnt,
			)
		}
		if len(dn.GetSidePtrList()) != 1 {
			t.Fatalf("%s side_ptr_list: got %d, want 1",
				addrPort, len(dn.GetSidePtrList()))
		}
		if dn.GetSidePtrList()[0].GetSpId() != opsSpId {
			t.Errorf("%s side pointer must name the SP", addrPort)
		}
		oldKey := DnCapacityKey(
			env.cid, mustBin(t, env, opsDnFree), opsDnFree, addrPort,
		)
		newFree := opsDnFree - opsDataExtCnt
		newKey := DnCapacityKey(
			env.cid, mustBin(t, env, newFree), newFree, addrPort,
		)
		if env.exists(oldKey) {
			t.Errorf("%s: the old capacity key must be gone", addrPort)
		}
		if !env.exists(newKey) {
			t.Errorf("%s: the new capacity key must exist", addrPort)
		}
	}
	if got := env.dnRev(opsDnC); got != before.dnCRev+1 {
		t.Errorf("dn-c rev: got %d, want %d", got, before.dnCRev+1)
	}
	if got := env.dnRev(opsDnD); got != before.dnDRev+1 {
		t.Errorf("dn-d rev: got %d, want %d", got, before.dnDRev+1)
	}
	// Both cntlr CNs are charged once each.
	for _, addrPort := range []string{opsCnA, opsCnB} {
		cn := env.cn(addrPort)
		if cn.GetFreeExtCnt() != opsCnFree-opsDataExtCnt {
			t.Errorf(
				"%s free_ext_cnt: got %d, want %d",
				addrPort, cn.GetFreeExtCnt(), opsCnFree-opsDataExtCnt,
			)
		}
		if env.exists(CnCapacityKey(env.cid, opsCnFree, addrPort)) {
			t.Errorf("%s: the old capacity key must be gone", addrPort)
		}
		if !env.exists(CnCapacityKey(
			env.cid, opsCnFree-opsDataExtCnt, addrPort,
		)) {
			t.Errorf("%s: the new capacity key must exist", addrPort)
		}
	}
	if got := env.cnRev(opsCnA); got != before.cnARev+1 {
		t.Errorf("cn-a rev: got %d, want %d", got, before.cnARev+1)
	}
	if got := env.cnRev(opsCnB); got != before.cnBRev+1 {
		t.Errorf("cn-b rev: got %d, want %d", got, before.cnBRev+1)
	}
	if got := env.spRev(); got != before.spRev+1 {
		t.Errorf("SpRev: got %d, want %d (exactly one bump)", got, before.spRev+1)
	}
	// The picks are stale now, so the same call is refused (MD5).
	_, err = GrowSlice(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, false, opsNotPending, env.cc, legs,
	)
	precondition := wantPrecondition(t, err, opGrowSlice)
	if precondition.Reason != ReasonCandidateChanged {
		t.Errorf("Reason: got %q, want %q",
			precondition.Reason, ReasonCandidateChanged)
	}
}

// TestGrowSliceRefusesASecondGrowForOneBreach is AR2's "two owners overlapping
// on one SP cannot apply an action twice — the second STM fails its
// precondition", applied to GrowSlice's STM (CreateSpareLeg's is
// TestCreateSpareLegRefusesWhileASpareIsPending's).
//
// Both owners evaluate the SAME pre-grow snapshot during an accepted
// shard-handoff overlap (VW7): both find AR6's pending rule false, both
// scan candidates and both call GrowSlice. Their picks are drawn at random, so
// the second owner's DNs need not be the first's and MD5's capacity guard does
// not fire — the test gives the second call fresh DNs on purpose. Without a
// precondition the slice would get TWO new groups for one breach: double the
// DN extents and double the CN footprint on every cntlr, undoable only by an
// operator.
func TestGrowSliceRefusesASecondGrowForOneBreach(t *testing.T) {
	for _, tc := range []struct {
		name   string
		isMeta bool
		// total is what the pool reported BEFORE either grow landed: the
		// blocks of the fixture's single group of that kind.
		total uint64
	}{
		{name: "data", isMeta: false, total: 4093},
		{
			name:   "meta",
			isMeta: true,
			// The metadata device is the meta group's data region in dm-thin's
			// 4 KiB blocks.
			total: 1021 * opsBlockSize / 4096,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			// The first owner grows.
			if _, err := GrowSlice(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
				opsSliceId, tc.isMeta, tc.total, env.cc,
				[]Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)},
			); err != nil {
				t.Fatalf("first grow: %v", err)
			}
			after := env.slice()
			grps := len(after.GetDataGrpList())
			if tc.isMeta {
				grps = len(after.GetMetaGrpList())
			}
			if grps != 2 {
				t.Fatalf("%d groups after the first grow, want 2", grps)
			}
			revBefore := env.spRev()

			// The second owner, still holding the pre-grow snapshot and the
			// pre-grow status line, picks two DNs of its own.
			env.putDn("dn-g0:9000", 960, 5, opsDnFree)
			env.putDn("dn-g1:9000", 961, 5, opsDnFree)
			_, err := GrowSlice(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
				opsSliceId, tc.isMeta, tc.total, env.cc,
				[]Cand{
					env.dnCand("dn-g0:9000"), env.dnCand("dn-g1:9000"),
				},
			)
			precondition := wantPrecondition(t, err, opGrowSlice)
			if precondition.Reason != ReasonGrowPending {
				t.Errorf("Reason: got %q, want %q",
					precondition.Reason, ReasonGrowPending)
			}
			slice := env.slice()
			if len(slice.GetDataGrpList())+len(slice.GetMetaGrpList()) != 3 {
				t.Errorf("a second group was appended for one breach: %d/%d",
					len(slice.GetMetaGrpList()),
					len(slice.GetDataGrpList()))
			}
			if got := env.dn("dn-g0:9000").GetFreeExtCnt(); got != opsDnFree {
				t.Errorf("dn-g0 free_ext_cnt: got %d, want %d",
					got, opsDnFree)
			}
			if got := env.spRev(); got != revBefore {
				t.Errorf("an aborted grow must not bump SpRev: got %d", got)
			}
		})
	}
}

// TestGrowPending walks AR6's stateless pending rule, which GrowSlice applies
// as its precondition and worker/reaction.go as its pre-check.
func TestGrowPending(t *testing.T) {
	slice := opsSlice()
	// One group of a kind: the sum is over an empty set, so the first grow of
	// a slice is never held back by its own (as yet unreported) group.
	if GrowPending(slice, false, 0, opsBlockSize) {
		t.Errorf("a slice with one data group must never be pending")
	}
	if GrowPending(slice, true, 0, opsBlockSize) {
		t.Errorf("a slice with one meta group must never be pending")
	}
	slice.DataGrpList = append(slice.DataGrpList, &pb.Group{
		GrpId: 999, ExtCnt: opsDataExtCnt, MetaBlocks: 3, DataBlocks: 4093,
	})
	if !GrowPending(slice, false, 4093, opsBlockSize) {
		t.Errorf("a total that still reflects only the first group is pending")
	}
	if GrowPending(slice, false, 4094, opsBlockSize) {
		t.Errorf("a total past the first group's is not pending")
	}
	slice.MetaGrpList = append(slice.MetaGrpList, &pb.Group{
		GrpId: 998, ExtCnt: opsMetaExtCnt, MetaBlocks: 3, DataBlocks: 1021,
	})
	metaBlocks := 1021 * opsBlockSize / 4096
	if !GrowPending(slice, true, metaBlocks, opsBlockSize) {
		t.Errorf("meta: %d blocks must still be pending", metaBlocks)
	}
	if GrowPending(slice, true, metaBlocks+1, opsBlockSize) {
		t.Errorf("meta: %d blocks must not be pending", metaBlocks+1)
	}
}

// TestGrowSliceMetaLadder grows the meta list 1 → 2 → 4 → 8 → 16 extents and
// then hits the cap of architecture.md, GrowSlice.
func TestGrowSliceMetaLadder(t *testing.T) {
	env := newOpsEnv(t)
	// Enough DN pairs for four meta grows.
	pairs := [][2]string{{opsDnC, opsDnD}}
	for idx := 0; idx < 3; idx++ {
		addrA := "dn-m" + string(rune('a'+idx*2)) + ":9000"
		addrB := "dn-m" + string(rune('b'+idx*2)) + ":9000"
		env.putDn(addrA, uint64(900+idx*2), 5, opsDnFree)
		env.putDn(addrB, uint64(901+idx*2), 5, opsDnFree)
		pairs = append(pairs, [2]string{addrA, addrB})
	}
	total := opsMetaExtCnt
	for step, pair := range pairs {
		legs := []Cand{env.dnCand(pair[0]), env.dnCand(pair[1])}
		grpId, err := GrowSlice(
			env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
			opsSliceId, true, opsNotPending, env.cc, legs,
		)
		if err != nil {
			t.Fatalf("meta grow %d: %v", step, err)
		}
		slice := env.slice()
		grp := findGroup(slice, grpId)
		if grp == nil {
			t.Fatalf("meta grow %d: group %d not found", step, grpId)
		}
		if grp.GetExtCnt() != total {
			t.Errorf(
				"meta grow %d: ext_cnt got %d, want %d",
				step, grp.GetExtCnt(), total,
			)
		}
		if len(slice.GetMetaGrpList()) != step+2 {
			t.Errorf("meta grow %d: %d meta groups",
				step, len(slice.GetMetaGrpList()))
		}
		if len(slice.GetDataGrpList()) != 1 {
			t.Errorf("a meta grow must not touch the data list")
		}
		total += grp.GetExtCnt()
	}
	if total != 16 {
		t.Fatalf("meta total: got %d, want 16", total)
	}
	env.putDn("dn-z0:9000", 950, 5, opsDnFree)
	env.putDn("dn-z1:9000", 951, 5, opsDnFree)
	revBefore := env.spRev()
	_, err := GrowSlice(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, true, opsNotPending, env.cc,
		[]Cand{env.dnCand("dn-z0:9000"), env.dnCand("dn-z1:9000")},
	)
	precondition := wantPrecondition(t, err, opGrowSlice)
	if precondition.Reason != "meta ladder at the 16 GiB cap" {
		t.Errorf("Reason: got %q", precondition.Reason)
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("an aborted grow must not bump SpRev: got %d", got)
	}
}

// TestGrowSliceRefusesAFullGroupList pins the group ceiling of architecture.md,
// GrowSlice: a group's md names carry its index in its list as two hex digits
// (architecture.md, md names), so GrowSlice
// refuses to append to a list that already holds MaxGrpCntPerSlice groups,
// for either kind, and writes nothing. The ceiling is per list: a grow of the
// other kind still commits on the same slice. The data list is also walked up
// to the ceiling by one real grow, which pins the boundary: the grow that
// makes the list exactly full commits, only the next one is refused. The meta
// list is padded straight to the ceiling, because at one extent per group the
// ladder would refuse long before it (and the refusal must not depend on
// that).
func TestGrowSliceRefusesAFullGroupList(t *testing.T) {
	pad := func(env *opsEnv, isMeta bool, cnt int) {
		slice := env.slice()
		grps := &slice.DataGrpList
		if isMeta {
			grps = &slice.MetaGrpList
		}
		for len(*grps) < cnt {
			grp := proto.Clone((*grps)[0]).(*pb.Group)
			grp.GrpId = 5000 + uint64(len(*grps))
			*grps = append(*grps, grp)
		}
		mustPut(env.t, env.cli, SliceKey(env.cid, opsSpId, opsSliceId), slice)
	}
	grpCnt := func(env *opsEnv, isMeta bool) int {
		if isMeta {
			return len(env.slice().GetMetaGrpList())
		}
		return len(env.slice().GetDataGrpList())
	}
	for _, tc := range []struct {
		name   string
		isMeta bool
	}{
		{name: "data", isMeta: false},
		{name: "meta", isMeta: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			if !tc.isMeta {
				pad(env, false, common.MaxGrpCntPerSlice-1)
				if _, err := GrowSlice(
					env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
					noExpectRev, opsSliceId, false, opsNotPending, env.cc,
					[]Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)},
				); err != nil {
					t.Fatalf("the grow that fills the list: %v", err)
				}
			}
			pad(env, tc.isMeta, common.MaxGrpCntPerSlice)
			env.putDn("dn-f0:9000", 970, 5, opsDnFree)
			env.putDn("dn-f1:9000", 971, 5, opsDnFree)
			revBefore := env.spRev()
			nextIdBefore := env.spConf().GetNextId()
			_, err := GrowSlice(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
				noExpectRev, opsSliceId, tc.isMeta, opsNotPending, env.cc,
				[]Cand{env.dnCand("dn-f0:9000"), env.dnCand("dn-f1:9000")},
			)
			precondition := wantPrecondition(t, err, opGrowSlice)
			if precondition.Reason != ReasonGrpListFull {
				t.Errorf("Reason: got %q, want %q",
					precondition.Reason, ReasonGrpListFull)
			}
			if got := grpCnt(env, tc.isMeta); got != common.MaxGrpCntPerSlice {
				t.Errorf("%d groups in the list, want the ceiling %d",
					got, common.MaxGrpCntPerSlice)
			}
			if got := env.dn("dn-f0:9000").GetFreeExtCnt(); got != opsDnFree {
				t.Errorf("dn-f0 free_ext_cnt: got %d, want %d", got, opsDnFree)
			}
			if got := env.spConf().GetNextId(); got != nextIdBefore {
				t.Errorf("next_id: got %d, want %d", got, nextIdBefore)
			}
			if got := env.spRev(); got != revBefore {
				t.Errorf("an aborted grow must not bump SpRev: got %d", got)
			}
			// The ceiling is per list: a full list of one kind leaves the
			// same slice free to grow the other kind.
			if _, err := GrowSlice(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
				noExpectRev, opsSliceId, !tc.isMeta, opsNotPending, env.cc,
				[]Cand{env.dnCand("dn-f0:9000"), env.dnCand("dn-f1:9000")},
			); err != nil {
				t.Fatalf("a grow of the other kind beside a full %s list: %v",
					tc.name, err)
			}
		})
	}
}

func TestGrowSlicePreconditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(env *opsEnv) []Cand
		reason string
	}{
		{
			name: "candidate changed",
			setup: func(env *opsEnv) []Cand {
				legs := []Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)}
				// Somebody else consumed one extent of dn-d.
				env.delKey(DnCapacityKey(
					env.cid, mustBin(env.t, env, opsDnFree),
					opsDnFree, opsDnD,
				))
				env.putDn(opsDnD, 703, 3, opsDnFree-1)
				return legs
			},
			reason: ReasonCandidateChanged,
		},
		{
			name: "dn free_ext_cnt too low",
			setup: func(env *opsEnv) []Cand {
				env.putDn(opsDnD, 703, 3, 2)
				return []Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)}
			},
			reason: "dn free_ext_cnt too low",
		},
		{
			// GrowSlice keeps checkDnPick's named refusals: unlike
			// CreateSpareLeg it does not read a vanished pick as a stale
			// plan.
			name: "dn not allocatable",
			setup: func(env *opsEnv) []Cand {
				legs := []Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)}
				dn := env.dn(opsDnD)
				dn.Disabled = true
				mustPut(env.t, env.cli, DnConfKey(env.cid, opsDnD), dn)
				return legs
			},
			reason: "dn not allocatable",
		},
		{
			name: "dn not found",
			setup: func(env *opsEnv) []Cand {
				legs := []Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)}
				env.delKey(DnConfKey(env.cid, opsDnD))
				return legs
			},
			reason: "dn not found",
		},
		{
			name: "cn free_ext_cnt too low",
			setup: func(env *opsEnv) []Cand {
				env.putCn(opsCnB, 801, 1, 2)
				return []Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)}
			},
			reason: "cn free_ext_cnt too low",
		},
		{
			name: "wrong leg count",
			setup: func(env *opsEnv) []Cand {
				return []Cand{env.dnCand(opsDnC)}
			},
			reason: "wrong leg count",
		},
		{
			name: "duplicate candidate",
			setup: func(env *opsEnv) []Cand {
				return []Cand{env.dnCand(opsDnC), env.dnCand(opsDnC)}
			},
			reason: "duplicate candidate",
		},
		{
			name: "sp deleting",
			setup: func(env *opsEnv) []Cand {
				legs := []Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)}
				env.setDeleting()
				return legs
			},
			reason: "sp deleting",
		},
		{
			name: "sp level suppresses reactions",
			setup: func(env *opsEnv) []Cand {
				legs := []Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)}
				env.setLevel(pb.SpLevel_SP_LEVEL_NO_REDUND)
				return legs
			},
			reason: "sp level suppresses reactions",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			legs := tc.setup(env)
			revBefore := env.spRev()
			dnCFree := env.dn(opsDnC).GetFreeExtCnt()
			_, err := GrowSlice(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
				opsSliceId, false, opsNotPending, env.cc, legs,
			)
			precondition := wantPrecondition(t, err, opGrowSlice)
			if precondition.Reason != tc.reason {
				t.Errorf(
					"Reason: got %q, want %q",
					precondition.Reason, tc.reason,
				)
			}
			// Nothing at all was written: not the slice, not the first DN
			// the op had already charged when it failed on the second.
			if len(env.slice().GetDataGrpList()) != 1 {
				t.Errorf("the slice must be untouched")
			}
			if got := env.dn(opsDnC).GetFreeExtCnt(); got != dnCFree {
				t.Errorf("dn-c free_ext_cnt: got %d, want %d", got, dnCFree)
			}
			if got := env.spConf().GetNextId(); got != opsNextId {
				t.Errorf("next_id: got %d, want %d", got, opsNextId)
			}
			if got := env.spRev(); got != revBefore {
				t.Errorf("SpRev: got %d, want %d", got, revBefore)
			}
		})
	}
	t.Run("slice not found", func(t *testing.T) {
		env := newOpsEnv(t)
		legs := []Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)}
		_, err := GrowSlice(
			env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
			9999, false, opsNotPending, env.cc, legs,
		)
		precondition := wantPrecondition(t, err, opGrowSlice)
		if precondition.Reason != "slice not in sp" {
			t.Errorf("Reason: got %q", precondition.Reason)
		}
	})
}

// TestGrowSliceRefusesAnInvalidStoredConf pins the gate (architecture.md,
// Common validation) at the top of the
// GrowSlice STM. Both confs are validated before anything is computed from
// them — the two zeros below are the two members a reader must never
// substitute a constant for — and the refusal is an ErrPrecondition, so the
// transaction aborts without committing and every key the op would have
// written is left as it was.
func TestGrowSliceRefusesAnInvalidStoredConf(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(env *opsEnv)
		reason string
	}{
		{
			// The pool block size the group geometry (architecture.md, Group
			// on-leg layout: meta region, data region, health block) and AR6's
			// pending rule are both expressed in.
			name: "the sp's stored bdev_conf",
			setup: func(env *opsEnv) {
				conf := env.spConf()
				conf.BdevConf.DmPoolConf.DataBlockSize = 0
				mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
			},
			reason: "invalid stored conf: " +
				"bdev_conf.dm_pool_conf.data_block_size is zero",
		},
		{
			// The extent size the ladder and every group size are computed
			// with.
			name: "the stored cluster conf",
			setup: func(env *opsEnv) {
				env.cc.DnBinConf.ExtentSize = 0
			},
			reason: "invalid stored conf: dn_bin_conf.extent_size is zero",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			legs := []Cand{env.dnCand(opsDnC), env.dnCand(opsDnD)}
			tc.setup(env)
			revBefore := env.spRev()
			dnCFree := env.dn(opsDnC).GetFreeExtCnt()
			_, err := GrowSlice(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
				noExpectRev, opsSliceId, false, opsNotPending, env.cc, legs,
			)
			precondition := wantPrecondition(t, err, opGrowSlice)
			if precondition.Reason != tc.reason {
				t.Errorf(
					"Reason: got %q, want %q",
					precondition.Reason, tc.reason,
				)
			}
			slice := env.slice()
			if len(slice.GetDataGrpList()) != 1 ||
				len(slice.GetMetaGrpList()) != 1 {
				t.Errorf(
					"a group was appended: %d meta, %d data",
					len(slice.GetMetaGrpList()),
					len(slice.GetDataGrpList()),
				)
			}
			if got := env.spConf().GetNextId(); got != opsNextId {
				t.Errorf("next_id: got %d, want %d", got, opsNextId)
			}
			if got := env.spRev(); got != revBefore {
				t.Errorf("SpRev: got %d, want %d", got, revBefore)
			}
			if got := env.dn(opsDnC).GetFreeExtCnt(); got != dnCFree {
				t.Errorf("dn-c free_ext_cnt: got %d, want %d", got, dnCFree)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ReplaceCntlr
// ---------------------------------------------------------------------------

func TestReplaceCntlr(t *testing.T) {
	env := newOpsEnv(t)
	env.setCntlrErr(opsCntlrB, 1000)
	now := uint64(1000 + common.DefaultCntlrUnhealthy)
	newCn := env.cnCand(opsCnC)
	before := struct{ spRev, cnBRev, cnCRev uint64 }{
		env.spRev(), env.cnRev(opsCnB), env.cnRev(opsCnC),
	}
	newId, err := ReplaceCntlr(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrB, newCn, []string{opsCnA}, false, now,
	)
	if err != nil {
		t.Fatalf("ReplaceCntlr: %v", err)
	}
	if newId != opsNextId {
		t.Errorf("cntlr_id: got %d, want %d (from next_id)", newId, opsNextId)
	}
	if env.exists(CntlrKey(env.cid, opsSpId, opsCntlrB)) {
		t.Errorf("the old cntlr key must be deleted")
	}
	fresh := env.cntlr(newId)
	if fresh.GetAddrPort() != opsCnC {
		t.Errorf("addr_port: got %q, want %q", fresh.GetAddrPort(), opsCnC)
	}
	if !proto.Equal(fresh.GetNvmeTrConf(), opsTrConf(opsCnC)) {
		t.Errorf("nvme_tr_conf must come from the new CN")
	}
	if fresh.GetCntlidSlot() != 1 {
		t.Errorf("cntlid_slot: got %d, want the old cntlr's 1",
			fresh.GetCntlidSlot())
	}
	if fresh.GetPrimary() || fresh.GetDisabled() || fresh.GetErrEpoch() != 0 {
		t.Errorf("the new cntlr must be a healthy, enabled standby: %v", fresh)
	}
	if fresh.GetSettling() {
		t.Errorf("a standby replacement must not be settling (HL2)")
	}
	conf := env.spConf()
	if conf.GetNextId() != opsNextId+1 {
		t.Errorf("next_id: got %d, want %d", conf.GetNextId(), opsNextId+1)
	}
	wantList := []uint64{opsCntlrA, newId}
	if len(conf.GetCntlrIdList()) != 2 ||
		conf.GetCntlrIdList()[0] != wantList[0] ||
		conf.GetCntlrIdList()[1] != wantList[1] {
		t.Errorf("cntlr_id_list: got %v, want %v",
			conf.GetCntlrIdList(), wantList)
	}
	// The old CN gets the footprint back and loses its pointer.
	oldCn := env.cn(opsCnB)
	if oldCn.GetFreeExtCnt() != opsCnFree+opsFootprint {
		t.Errorf("cn-b free_ext_cnt: got %d, want %d",
			oldCn.GetFreeExtCnt(), opsCnFree+opsFootprint)
	}
	if len(oldCn.GetCntlrPtrList()) != 0 {
		t.Errorf("cn-b must have no cntlr pointer left")
	}
	// The new CN is charged the footprint and gains the pointer.
	fresher := env.cn(opsCnC)
	if fresher.GetFreeExtCnt() != opsCnFree-opsFootprint {
		t.Errorf("cn-c free_ext_cnt: got %d, want %d",
			fresher.GetFreeExtCnt(), opsCnFree-opsFootprint)
	}
	if len(fresher.GetCntlrPtrList()) != 1 ||
		fresher.GetCntlrPtrList()[0].GetCntlrId() != newId {
		t.Errorf("cn-c must point at the new cntlr: %v",
			fresher.GetCntlrPtrList())
	}
	if !env.exists(CnCapacityKey(
		env.cid, opsCnFree+opsFootprint, opsCnB,
	)) {
		t.Errorf("cn-b's capacity key must follow its new free count")
	}
	if !env.exists(CnCapacityKey(
		env.cid, opsCnFree-opsFootprint, opsCnC,
	)) {
		t.Errorf("cn-c's capacity key must follow its new free count")
	}
	if got := env.cnRev(opsCnB); got != before.cnBRev+1 {
		t.Errorf("cn-b rev: got %d, want %d", got, before.cnBRev+1)
	}
	if got := env.cnRev(opsCnC); got != before.cnCRev+1 {
		t.Errorf("cn-c rev: got %d, want %d", got, before.cnCRev+1)
	}
	if got := env.spRev(); got != before.spRev+1 {
		t.Errorf("SpRev: got %d, want %d (exactly one bump)",
			got, before.spRev+1)
	}
	// The discovery entry advertises the new CN instead of the old one.
	entry := &pb.CdcEntry{}
	env.get(CdcEntryKey(env.cid, opsShard, opsSpId, opsSsId), entry)
	if len(entry.GetNvmeTrConfList()) != 2 {
		t.Fatalf("cdc entry: got %d transports, want 2",
			len(entry.GetNvmeTrConfList()))
	}
	if !proto.Equal(entry.GetNvmeTrConfList()[0], opsTrConf(opsCnA)) ||
		!proto.Equal(entry.GetNvmeTrConfList()[1], opsTrConf(opsCnC)) {
		t.Errorf("cdc entry: got %v", entry.GetNvmeTrConfList())
	}
}

// TestReplaceCntlrSolePrimary is AR7's sole-primary variant: a primary with no
// failover candidate is replaced by a new primary with the same cntlid_slot.
func TestReplaceCntlrSolePrimary(t *testing.T) {
	env := newOpsEnv(t)
	// Make the SP sole-cntlr: only the unhealthy primary is left.
	conf := env.spConf()
	conf.CntlrIdList = []uint64{opsCntlrA}
	mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
	env.setCntlrErr(opsCntlrA, 1000)
	now := uint64(1000 + common.DefaultCntlrUnhealthy)
	newId, err := ReplaceCntlr(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrA, env.cnCand(opsCnC), nil, true, now,
	)
	if err != nil {
		t.Fatalf("ReplaceCntlr: %v", err)
	}
	fresh := env.cntlr(newId)
	if !fresh.GetPrimary() {
		t.Errorf("the replacement of a sole primary must be primary")
	}
	if !fresh.GetSettling() {
		t.Errorf("a primary replacement must be settling (HL2)")
	}
	if fresh.GetCntlidSlot() != 0 {
		t.Errorf("cntlid_slot: got %d, want the old cntlr's 0",
			fresh.GetCntlidSlot())
	}
}

func TestReplaceCntlrPreconditions(t *testing.T) {
	now := uint64(1000 + common.DefaultCntlrUnhealthy)
	for _, tc := range []struct {
		name      string
		setup     func(env *opsEnv) Cand
		oldId     uint64
		asPrimary bool
		now       uint64
		reason    string
	}{
		{
			name:   "old cntlr is healthy",
			setup:  func(env *opsEnv) Cand { return env.cnCand(opsCnC) },
			oldId:  opsCntlrB,
			now:    now,
			reason: "old cntlr is healthy",
		},
		{
			name: "cntlr_unhealthy not reached",
			setup: func(env *opsEnv) Cand {
				env.setCntlrErr(opsCntlrB, 1000)
				return env.cnCand(opsCnC)
			},
			oldId:  opsCntlrB,
			now:    1000 + common.DefaultCntlrUnhealthy - 1,
			reason: "cntlr_unhealthy not reached",
		},
		{
			name: "old cntlr is disabled",
			setup: func(env *opsEnv) Cand {
				env.setCntlrErr(opsCntlrB, 1000)
				cntlr := env.cntlr(opsCntlrB)
				cntlr.Disabled = true
				mustPut(env.t, env.cli,
					CntlrKey(env.cid, opsSpId, opsCntlrB), cntlr)
				return env.cnCand(opsCnC)
			},
			oldId:  opsCntlrB,
			now:    now,
			reason: "old cntlr is disabled",
		},
		{
			name: "failover candidate exists",
			setup: func(env *opsEnv) Cand {
				env.setCntlrErr(opsCntlrA, 1000)
				return env.cnCand(opsCnC)
			},
			oldId:     opsCntlrA,
			asPrimary: true,
			now:       now,
			reason:    "failover candidate exists",
		},
		{
			name: "primary replacement must stay primary",
			setup: func(env *opsEnv) Cand {
				env.setCntlrErr(opsCntlrA, 1000)
				return env.cnCand(opsCnC)
			},
			oldId:     opsCntlrA,
			asPrimary: false,
			now:       now,
			reason:    "primary replacement must stay primary",
		},
		{
			name: "sp already has a primary",
			setup: func(env *opsEnv) Cand {
				env.setCntlrErr(opsCntlrB, 1000)
				return env.cnCand(opsCnC)
			},
			oldId:     opsCntlrB,
			asPrimary: true,
			now:       now,
			reason:    "sp already has a primary",
		},
		{
			name: "cn already hosts a cntlr",
			setup: func(env *opsEnv) Cand {
				env.setCntlrErr(opsCntlrB, 1000)
				return env.cnCand(opsCnA)
			},
			oldId:  opsCntlrB,
			now:    now,
			reason: "cn already hosts a cntlr",
		},
		{
			name: "candidate changed",
			setup: func(env *opsEnv) Cand {
				env.setCntlrErr(opsCntlrB, 1000)
				cand := env.cnCand(opsCnC)
				env.delKey(CnCapacityKey(env.cid, opsCnFree, opsCnC))
				env.putCn(opsCnC, 802, 2, opsCnFree-1)
				return cand
			},
			oldId:  opsCntlrB,
			now:    now,
			reason: ReasonCandidateChanged,
		},
		{
			// A cntlr the gateway committed after the snapshot the pick was
			// planned from, on a CN of its own: the pick may share its
			// failure domain, which only the plan's list can tell.
			name: "a cntlr the plan did not hold",
			setup: func(env *opsEnv) Cand {
				env.setCntlrErr(opsCntlrB, 1000)
				env.putCn(opsCnD, 803, 3, opsCnFree)
				env.gainCntlr(opsCnD)
				return env.cnCand(opsCnC)
			},
			oldId:  opsCntlrB,
			now:    now,
			reason: ReasonCandidateChanged,
		},
		{
			// The same gain on the pick's own CN is a stale plan too, and
			// the plan check precedes the same-CN one (architecture.md, Finding CN
			// candidates), so the next pass
			// re-plans instead of reading a refusal.
			name: "a cntlr the plan did not hold on the pick's cn",
			setup: func(env *opsEnv) Cand {
				env.setCntlrErr(opsCntlrB, 1000)
				cand := env.cnCand(opsCnC)
				env.gainCntlr(opsCnC)
				return cand
			},
			oldId:  opsCntlrB,
			now:    now,
			reason: ReasonCandidateChanged,
		},
		{
			name: "cn free_ext_cnt too low",
			setup: func(env *opsEnv) Cand {
				env.setCntlrErr(opsCntlrB, 1000)
				env.putCn(opsCnC, 802, 2, opsFootprint-1)
				return env.cnCand(opsCnC)
			},
			oldId:  opsCntlrB,
			now:    now,
			reason: "cn free_ext_cnt too low",
		},
		{
			name: "cn not allocatable",
			setup: func(env *opsEnv) Cand {
				env.setCntlrErr(opsCntlrB, 1000)
				cand := env.cnCand(opsCnC)
				cn := env.cn(opsCnC)
				cn.Disabled = true
				mustPut(env.t, env.cli, CnConfKey(env.cid, opsCnC), cn)
				return cand
			},
			oldId:  opsCntlrB,
			now:    now,
			reason: "cn not allocatable",
		},
		{
			name: "sp deleting",
			setup: func(env *opsEnv) Cand {
				env.setCntlrErr(opsCntlrB, 1000)
				env.setDeleting()
				return env.cnCand(opsCnC)
			},
			oldId:  opsCntlrB,
			now:    now,
			reason: "sp deleting",
		},
		{
			// A listed clone with no key: the units it holds cannot be
			// moved exactly, for spFootprint's reason.
			name: "clone not found",
			setup: func(env *opsEnv) Cand {
				env.setCntlrErr(opsCntlrB, 1000)
				env.putMetaClone("c0", 901, 3, false)
				env.setCnUsed(opsCnB, 3)
				conf := env.spConf()
				conf.CloneNameList = append(conf.GetCloneNameList(), "lost")
				mustPut(env.t, env.cli, SpConfKey(env.cid, opsSpName), conf)
				return env.cnCand(opsCnC)
			},
			oldId:  opsCntlrB,
			now:    now,
			reason: "clone not found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			// The plan is the SP's other cntlrs as the fixture holds them,
			// read before setup: what a caller's snapshot would have held.
			plan := env.otherCntlrAddrs(tc.oldId)
			cand := tc.setup(env)
			revBefore := env.spRev()
			nextBefore := env.spConf().GetNextId()
			cnB, cnC := env.cn(opsCnB), env.cn(opsCnC)
			_, err := ReplaceCntlr(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
				tc.oldId, cand, plan, tc.asPrimary, tc.now,
			)
			precondition := wantPrecondition(t, err, opReplaceCntlr)
			if precondition.Reason != tc.reason {
				t.Errorf(
					"Reason: got %q, want %q",
					precondition.Reason, tc.reason,
				)
			}
			if !env.exists(CntlrKey(env.cid, opsSpId, tc.oldId)) {
				t.Errorf("the old cntlr must still be there")
			}
			if got := env.spConf().GetNextId(); got != nextBefore {
				t.Errorf("next_id: got %d, want %d", got, nextBefore)
			}
			if got := env.spRev(); got != revBefore {
				t.Errorf("SpRev: got %d, want %d", got, revBefore)
			}
			// Neither CN moved: not the extents, not the units.
			if got := env.cn(opsCnB); !proto.Equal(got, cnB) {
				t.Errorf("cn-b moved on a refusal: %v", got)
			}
			if got := env.cn(opsCnC); !proto.Equal(got, cnC) {
				t.Errorf("cn-c moved on a refusal: %v", got)
			}
		})
	}
}

// TestReplaceCntlrChecksOnlyAGain is the other half of the plan check: a
// cntlr the plan held that has left since is no reason to re-plan. The pick is
// still off every CN and out of every location the SP's cntlrs hold, which
// were all in the plan, and the room the departure freed is a candidate the
// scan did not count on, like a CN that joins after it.
func TestReplaceCntlrChecksOnlyAGain(t *testing.T) {
	env := newOpsEnv(t)
	env.setCntlrErr(opsCntlrB, 1000)
	now := uint64(1000 + common.DefaultCntlrUnhealthy)
	// The plan names cn-d, whose cntlr the SP no longer lists.
	newId, err := ReplaceCntlr(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrB, env.cnCand(opsCnC), []string{opsCnA, opsCnD}, false, now,
	)
	if err != nil {
		t.Fatalf("ReplaceCntlr: %v", err)
	}
	if got := env.cntlr(newId).GetAddrPort(); got != opsCnC {
		t.Errorf("addr_port: got %q, want %q", got, opsCnC)
	}
}

// replaceStandbyB replaces the fixture's standby cntlr B, unhealthy for
// cntlr_unhealthy, by a cntlr on cn-c, planned against cn-a's surviving
// cntlr, and returns the new cntlr's id.
func replaceStandbyB(t *testing.T, env *opsEnv) uint64 {
	t.Helper()
	env.setCntlrErr(opsCntlrB, 1000)
	now := uint64(1000 + common.DefaultCntlrUnhealthy)
	newId, err := ReplaceCntlr(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrB, env.cnCand(opsCnC), []string{opsCnA}, false, now,
	)
	if err != nil {
		t.Fatalf("ReplaceCntlr: %v", err)
	}
	return newId
}

// TestReplaceCntlrMovesCloneMetaUnits pins the units half of ReplaceCntlr
// (architecture.md, Metadata units). The CN of every cntlr is charged every
// clone of the SP, so the replacement moves the SP's clone sum off the old CN
// and onto the new one, in the same writes as the footprint. A latched clone
// counts: FinishCloneDelete gives its units back on the CNs of the cntlrs
// the SP has when it runs, which after this op is the new CN. Counts other
// SPs charged stay where they are, the capacity values follow the records,
// the clones themselves are not rewritten, and each CN still gets exactly one
// CnRev bump, the surviving cntlr's CN none.
func TestReplaceCntlrMovesCloneMetaUnits(t *testing.T) {
	env := newOpsEnv(t)
	env.putMetaClone("c0", 901, 3, false)
	env.putMetaClone("c1", 902, 5, true) // latched, still charged
	const sum = 3 + 5
	// Every CN also holds units of another SP, which no step here touches.
	env.setCnUsed(opsCnA, sum+2)
	env.setCnUsed(opsCnB, sum+4)
	env.setCnUsed(opsCnC, 6)
	before := struct {
		cnA                    *pb.CnConf
		c0, c1                 *pb.Clone
		cnARev, cnBRev, cnCRev uint64
	}{
		env.cn(opsCnA), env.clone("c0"), env.clone("c1"),
		env.cnRev(opsCnA), env.cnRev(opsCnB), env.cnRev(opsCnC),
	}

	replaceStandbyB(t, env)

	if got := env.cn(opsCnB).GetUsedMetaUnits(); got != 4 {
		t.Errorf("cn-b used_meta_units: got %d, want 4 (the SP's %d gone)",
			got, sum)
	}
	if got := env.cn(opsCnC).GetUsedMetaUnits(); got != 6+sum {
		t.Errorf("cn-c used_meta_units: got %d, want %d (6 plus the SP's "+
			"%d)", got, 6+sum, sum)
	}
	for _, tc := range []struct {
		addrPort string
		used     uint64
	}{{opsCnB, 4}, {opsCnC, 6 + sum}} {
		value := env.cnCapValue(tc.addrPort)
		if value.GetUsedMetaUnits() != tc.used ||
			value.GetLocation() != tc.addrPort {
			t.Errorf("%s capacity value: got %v, want location %s and "+
				"%d used", tc.addrPort, value, tc.addrPort, tc.used)
		}
	}
	if got := env.cn(opsCnA); !proto.Equal(got, before.cnA) {
		t.Errorf("cn-a, the surviving cntlr's CN, moved: %v", got)
	}
	if got := env.clone("c0"); !proto.Equal(got, before.c0) {
		t.Errorf("clone c0 rewritten: %v", got)
	}
	if got := env.clone("c1"); !proto.Equal(got, before.c1) {
		t.Errorf("clone c1 rewritten: %v", got)
	}
	for _, tc := range []struct {
		addrPort string
		rev      uint64
	}{
		{opsCnA, before.cnARev},
		{opsCnB, before.cnBRev + 1},
		{opsCnC, before.cnCRev + 1},
	} {
		if got := env.cnRev(tc.addrPort); got != tc.rev {
			t.Errorf("%s rev: got %d, want %d", tc.addrPort, got, tc.rev)
		}
	}
}

// TestReplaceCntlrChargesPastTheCeiling is the one charge allowed to pass a
// CN's ceiling (architecture.md, Metadata units): a replacement restores the
// whole SP, so the pick's room is no precondition. cn-c starts one unit below
// common.CnCloneMetaUnitCnt and ends past it, and keeps its capacity key —
// units never decide the key's presence — with a value that reads no free
// unit, so a scan that asks for units skips it from now on.
func TestReplaceCntlrChargesPastTheCeiling(t *testing.T) {
	env := newOpsEnv(t)
	env.putMetaClone("c0", 901, 5, false)
	env.setCnUsed(opsCnB, 5)
	env.setCnUsed(opsCnC, common.CnCloneMetaUnitCnt-1)

	replaceStandbyB(t, env)

	cn := env.cn(opsCnC)
	if want := uint64(common.CnCloneMetaUnitCnt - 1 + 5); cn.GetUsedMetaUnits() !=
		want {
		t.Errorf("cn-c used_meta_units: got %d, want %d",
			cn.GetUsedMetaUnits(), want)
	}
	if got := CnFreeMetaUnits(cn); got != 0 {
		t.Errorf("cn-c free units: got %d, want 0 past the ceiling", got)
	}
	if !env.exists(CnCapacityKey(env.cid, opsCnFree-opsFootprint, opsCnC)) {
		t.Fatalf("cn-c lost its capacity key: units must not decide it")
	}
	value := env.cnCapValue(opsCnC)
	if value.GetUsedMetaUnits() != cn.GetUsedMetaUnits() {
		t.Errorf("cn-c capacity value: got %d used, want the record's %d",
			value.GetUsedMetaUnits(), cn.GetUsedMetaUnits())
	}
	if got := CnCapacityFreeMetaUnits(value); got != 0 {
		t.Errorf("cn-c capacity value: %d free units, want 0", got)
	}
}

// TestReplaceCntlrReleaseStopsAtZero pins the release's floor (architecture.md,
// Metadata units): an old CN holding fewer units than the SP's sum ends at
// zero, never at a count wrapped around to one that would refuse every later
// charge, and the new CN is still charged the whole sum.
func TestReplaceCntlrReleaseStopsAtZero(t *testing.T) {
	env := newOpsEnv(t)
	env.putMetaClone("c0", 901, 5, false)
	env.setCnUsed(opsCnB, 2)

	replaceStandbyB(t, env)

	if got := env.cn(opsCnB).GetUsedMetaUnits(); got != 0 {
		t.Errorf("cn-b used_meta_units: got %d, want 0", got)
	}
	if got := env.cnCapValue(opsCnB).GetUsedMetaUnits(); got != 0 {
		t.Errorf("cn-b capacity value: got %d used, want 0", got)
	}
	if got := env.cn(opsCnC).GetUsedMetaUnits(); got != 5 {
		t.Errorf("cn-c used_meta_units: got %d, want the SP's 5", got)
	}
}

// TestReplaceCntlrAtTheCeiling commits one maximum-shape ReplaceCntlr against
// the real etcd this package runs with --max-txn-ops=common.EtcdMaxTxnOps:
// MaxCntlrCntPerSp cntlrs on as many CNs, MaxSliceCntPerSp slices for the
// footprint, MaxSsCntPerSp subsystems whose CdcEntry the listing rule
// rewrites, and MaxCloneCntPerSp clones, each read once for the units the op
// moves. That shape is past etcd's own default limit (the EtcdMaxTxnOps
// comment counts it), so this is where a read added to the op, or a change
// in how etcdutil builds the txn, would show up as "too many operations in
// txn request" once it passes the requirement. Every key the op writes
// carries one mod_revision: it is one transaction.
func TestReplaceCntlrAtTheCeiling(t *testing.T) {
	env := newOpsEnv(t)
	const newCnAddr = "ceil-cn-new:9000"
	conf := env.spConf()

	// The cntlrs: the fixture's A (primary, cn-a) and B (cn-b), and one
	// more on a CN of its own up to the ceiling, each listed by the rule.
	cntlrAddrs := []string{opsCnA, opsCnB}
	for idx := len(conf.GetCntlrIdList()); idx < common.MaxCntlrCntPerSp; idx++ {
		addrPort := fmt.Sprintf("ceil-cn-%d:9000", idx)
		cntlrId := opsCntlrB + uint64(idx)
		env.putCn(addrPort, uint64(810+idx), uint32(idx), opsCnFree)
		mustPut(t, env.cli, CntlrKey(env.cid, opsSpId, cntlrId), &pb.Cntlr{
			AddrPort:   addrPort,
			NvmeTrConf: opsTrConf(addrPort),
			CntlidSlot: uint32(idx),
		})
		conf.CntlrIdList = append(conf.CntlrIdList, cntlrId)
		cntlrAddrs = append(cntlrAddrs, addrPort)
	}
	env.putCn(newCnAddr, 899, 9, 4*opsCnFree)

	// The slices: one extent each on top of the fixture's.
	for idx := len(conf.GetSliceIdList()); idx < common.MaxSliceCntPerSp; idx++ {
		sliceId := uint64(3000 + idx)
		mustPut(t, env.cli, SliceKey(env.cid, opsSpId, sliceId), &pb.Slice{
			SliceIdx:    uint32(idx),
			DataGrpList: []*pb.Group{{GrpId: uint64(30000 + idx), ExtCnt: 1}},
		})
		conf.SliceIdList = append(conf.SliceIdList, sliceId)
	}
	footprint := opsFootprint + uint64(common.MaxSliceCntPerSp-1)

	// The subsystems, every CdcEntry listing all four cntlrs: whatever it
	// lists, the rewrite to a list with the new cntlr changes every one.
	var trConfs []*pb.NvmeTrConf
	for _, addrPort := range cntlrAddrs {
		trConfs = append(trConfs, opsTrConf(addrPort))
	}
	var cdcKeys []string
	for idx := 0; idx < common.MaxSsCntPerSp; idx++ {
		nqn, ssId := opsNqn, opsSsId
		if idx > 0 {
			nqn = fmt.Sprintf("nqn.2024-01.io.dnv:ceil-%d", idx)
			ssId = opsSsId + uint64(idx)
			mustPut(t, env.cli, SubsystemKey(env.cid, opsSpId, nqn),
				&pb.Subsystem{SsId: ssId})
			conf.NqnList = append(conf.NqnList, nqn)
		}
		key := CdcEntryKey(env.cid, opsShard, opsSpId, ssId)
		mustPut(t, env.cli, key, &pb.CdcEntry{Nqn: nqn, NvmeTrConfList: trConfs})
		cdcKeys = append(cdcKeys, key)
	}

	// The clones, every third one latched.
	sum := uint64(0)
	for idx := 0; idx < common.MaxCloneCntPerSp; idx++ {
		name := fmt.Sprintf("ceil-clone-%02d", idx)
		units := uint64(1 + idx%3)
		mustPut(t, env.cli, CloneKey(env.cid, opsSpId, name), &pb.Clone{
			CloneId:   uint64(9000 + idx),
			DstTdId:   601,
			Deleting:  idx%3 == 0,
			MetaUnits: units,
		})
		conf.CloneNameList = append(conf.CloneNameList, name)
		sum += units
	}
	mustPut(t, env.cli, SpConfKey(env.cid, opsSpName), conf)
	env.setCnUsed(opsCnB, sum)

	env.setCntlrErr(opsCntlrB, 1000)
	now := uint64(1000 + common.DefaultCntlrUnhealthy)
	newId, err := ReplaceCntlr(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
		opsCntlrB, env.cnCand(newCnAddr), env.otherCntlrAddrs(opsCntlrB),
		false, now,
	)
	if err != nil {
		t.Fatalf("a maximum-shape ReplaceCntlr (%d cntlrs, %d slices, %d "+
			"subsystems, %d clones) did not commit: %v (a \"too many "+
			"operations in txn request\" here means common.EtcdMaxTxnOps no "+
			"longer covers it)", common.MaxCntlrCntPerSp,
			common.MaxSliceCntPerSp, common.MaxSsCntPerSp,
			common.MaxCloneCntPerSp, err)
	}

	if got := len(env.spConf().GetCntlrIdList()); got != common.MaxCntlrCntPerSp {
		t.Errorf("cntlr_id_list: %d cntlrs, want %d", got,
			common.MaxCntlrCntPerSp)
	}
	oldCn, newCn := env.cn(opsCnB), env.cn(newCnAddr)
	if oldCn.GetUsedMetaUnits() != 0 || newCn.GetUsedMetaUnits() != sum {
		t.Errorf("used_meta_units: cn-b %d, new cn %d, want 0 and the "+
			"sum %d over every clone", oldCn.GetUsedMetaUnits(),
			newCn.GetUsedMetaUnits(), sum)
	}
	if newCn.GetFreeExtCnt() != 4*opsCnFree-footprint {
		t.Errorf("new cn free_ext_cnt: got %d, want %d",
			newCn.GetFreeExtCnt(), 4*opsCnFree-footprint)
	}
	// One transaction, one revision: the new cntlr, both CNs, the SpConf
	// and every CdcEntry, each rewritten to list the new cntlr.
	rev := env.modRev(CntlrKey(env.cid, opsSpId, newId))
	written := append([]string{
		CnConfKey(env.cid, opsCnB),
		CnConfKey(env.cid, newCnAddr),
		SpConfKey(env.cid, opsSpName),
	}, cdcKeys...)
	for _, key := range written {
		if got := env.modRev(key); got != rev {
			t.Errorf("%s: mod_revision %d, want the op's %d", key, got, rev)
		}
	}
	for _, key := range cdcKeys {
		entry := &pb.CdcEntry{}
		env.get(key, entry)
		list := entry.GetNvmeTrConfList()
		if len(list) == 0 ||
			!proto.Equal(list[len(list)-1], opsTrConf(newCnAddr)) {
			t.Errorf("%s: %v, want the new cntlr listed last", key, list)
		}
	}
}

// ---------------------------------------------------------------------------
// Spare legs
// ---------------------------------------------------------------------------

func TestCreateSpareLeg(t *testing.T) {
	env := newOpsEnv(t)
	before := struct{ spRev, dnCRev uint64 }{env.spRev(), env.dnRev(opsDnC)}
	legId, err := CreateSpareLeg(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, opsDataGrpId, env.dnCand(opsDnC),
		env.grpDnAddrs(opsDataGrpId), env.cc,
	)
	if err != nil {
		t.Fatalf("CreateSpareLeg: %v", err)
	}
	if legId != opsNextId {
		t.Errorf("leg_id: got %d, want %d (from next_id)", legId, opsNextId)
	}
	if got := env.spConf().GetNextId(); got != opsNextId+2 {
		t.Errorf("next_id: got %d, want %d", got, opsNextId+2)
	}
	grp := findGroup(env.slice(), opsDataGrpId)
	if len(grp.GetLegList()) != 2 {
		t.Errorf("the active list must be untouched")
	}
	if len(grp.GetSpareLegList()) != 1 {
		t.Fatalf("spares: got %d, want 1", len(grp.GetSpareLegList()))
	}
	spare := grp.GetSpareLegList()[0]
	if spare.GetLegIdx() != 2 {
		t.Errorf("leg_idx: got %d, want 1 + max over both lists = 2",
			spare.GetLegIdx())
	}
	if len(spare.GetSideList()) != 1 {
		t.Fatalf("sides: got %d, want 1", len(spare.GetSideList()))
	}
	side := spare.GetSideList()[0]
	if side.GetSideId() != opsNextId+1 {
		t.Errorf("side_id: got %d, want %d", side.GetSideId(), opsNextId+1)
	}
	if side.GetProvisioned() {
		t.Errorf("a new spare side must be unprovisioned ([D15])")
	}
	if side.GetCntlidSlot() != opsSlot {
		t.Errorf("cntlid_slot: got %d, want %d", side.GetCntlidSlot(), opsSlot)
	}
	if side.GetAddrPort() != opsDnC ||
		!proto.Equal(side.GetNvmeTrConf(), opsTrConf(opsDnC)) {
		t.Errorf("the side must carry the DN's endpoint: %v", side)
	}
	dn := env.dn(opsDnC)
	if dn.GetFreeExtCnt() != opsDnFree-opsDataExtCnt {
		t.Errorf("dn-c free_ext_cnt: got %d, want %d",
			dn.GetFreeExtCnt(), opsDnFree-opsDataExtCnt)
	}
	if len(dn.GetSidePtrList()) != 1 ||
		dn.GetSidePtrList()[0].GetLegId() != legId {
		t.Errorf("dn-c must point at the new leg: %v", dn.GetSidePtrList())
	}
	if got := env.dnRev(opsDnC); got != before.dnCRev+1 {
		t.Errorf("dn-c rev: got %d, want %d", got, before.dnCRev+1)
	}
	if got := env.spRev(); got != before.spRev+1 {
		t.Errorf("SpRev: got %d, want %d", got, before.spRev+1)
	}
	// A second spare on a different DN fills the list once the first one's
	// side is provisioned (an unprovisioned spare holds the next create
	// back); a third is refused. Each create is planned after the one
	// before it, so its plan holds every spare the group has.
	if _, _, err := FlipProvisioned(
		env.ctx, env.cli, env.cid, opsShard, opsSpId,
		[]SideRef{{
			SliceId: opsSliceId, LegId: legId, SideId: side.GetSideId(),
		}},
	); err != nil {
		t.Fatalf("FlipProvisioned: %v", err)
	}
	if _, err := CreateSpareLeg(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, opsDataGrpId, env.dnCand(opsDnD),
		env.grpDnAddrs(opsDataGrpId), env.cc,
	); err != nil {
		t.Fatalf("CreateSpareLeg second: %v", err)
	}
	env.putDn("dn-e:9000", 704, 4, opsDnFree)
	_, err = CreateSpareLeg(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, opsDataGrpId, env.dnCand("dn-e:9000"),
		env.grpDnAddrs(opsDataGrpId), env.cc,
	)
	precondition := wantPrecondition(t, err, opCreateSpareLeg)
	if precondition.Reason != "spare list full" {
		t.Errorf("Reason: got %q", precondition.Reason)
	}
}

// overlapSpareCreate is AR2's spare-create overlap up to the first commit:
// two owners overlapping on one SP plan a spare for the same repair from ONE
// snapshot, in which the group has none — one plan, the group's DNs — and
// each picks a DN of its own. Owner A's create commits. It returns the plan,
// owner B's pick and owner A's new spare.
func overlapSpareCreate(t *testing.T, env *opsEnv) ([]string, Cand, *pb.Leg) {
	t.Helper()
	plan := env.grpDnAddrs(opsDataGrpId)
	ownerA := env.dnCand(opsDnC)
	ownerB := env.dnCand(opsDnD)
	legId, err := CreateSpareLeg(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, opsDataGrpId, ownerA, plan, env.cc,
	)
	if err != nil {
		t.Fatalf("CreateSpareLeg (owner A): %v", err)
	}
	spare := findLeg(env.slice(), legId)
	if spare == nil {
		t.Fatalf("owner A's spare %d is not in the slice", legId)
	}
	return plan, ownerB, spare
}

// spareRefusal is what a refused CreateSpareLeg must leave as it found it:
// the SP's revision and conf, the picked DN's record and revision, and the
// group's spare list.
type spareRefusal struct {
	addrPort string
	spRev    uint64
	conf     *pb.SpConf
	dn       *pb.DnConf
	dnRev    uint64
	spares   []*pb.Leg
}

// witnessSpareRefusal records the state a refused create on addrPort must
// not move.
func (e *opsEnv) witnessSpareRefusal(addrPort string) spareRefusal {
	e.t.Helper()
	return spareRefusal{
		addrPort: addrPort,
		spRev:    e.spRev(),
		conf:     e.spConf(),
		dn:       e.dn(addrPort),
		dnRev:    e.dnRev(addrPort),
		spares:   findGroup(e.slice(), opsDataGrpId).GetSpareLegList(),
	}
}

// check asserts that nothing the witness recorded moved.
func (w spareRefusal) check(t *testing.T, env *opsEnv) {
	t.Helper()
	if got := env.spRev(); got != w.spRev {
		t.Errorf("SpRev: got %d, want %d", got, w.spRev)
	}
	if got := env.spConf(); !proto.Equal(got, w.conf) {
		t.Errorf("SpConf moved on a refusal: %v", got)
	}
	if got := env.dn(w.addrPort); !proto.Equal(got, w.dn) {
		t.Errorf("%s moved on a refusal: %v", w.addrPort, got)
	}
	if got := env.dnRev(w.addrPort); got != w.dnRev {
		t.Errorf("%s rev: got %d, want %d", w.addrPort, got, w.dnRev)
	}
	spares := findGroup(env.slice(), opsDataGrpId).GetSpareLegList()
	if len(spares) != len(w.spares) {
		t.Fatalf("spares: got %d, want %d", len(spares), len(w.spares))
	}
	for idx := range spares {
		if !proto.Equal(spares[idx], w.spares[idx]) {
			t.Errorf("spare %d moved on a refusal: %v", idx, spares[idx])
		}
	}
}

// TestCreateSpareLegRefusesWhileASpareIsPending pins AR8 step 3's hold inside
// the STM (AR2): of two owners that planned one repair from one snapshot,
// the second must fail its precondition while the first one's spare is still
// unprovisioned, and write nothing, rather than add a second spare no failure
// asked for.
func TestCreateSpareLegRefusesWhileASpareIsPending(t *testing.T) {
	env := newOpsEnv(t)
	plan, ownerB, spare := overlapSpareCreate(t, env)
	witness := env.witnessSpareRefusal(opsDnD)
	_, err := CreateSpareLeg(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, opsDataGrpId, ownerB, plan, env.cc,
	)
	precondition := wantPrecondition(t, err, opCreateSpareLeg)
	if precondition.Reason != ReasonSpareUnprovisioned {
		t.Errorf("Reason: got %q, want %q",
			precondition.Reason, ReasonSpareUnprovisioned)
	}
	witness.check(t, env)
	spares := findGroup(env.slice(), opsDataGrpId).GetSpareLegList()
	if len(spares) != 1 || spares[0].GetLegId() != spare.GetLegId() {
		t.Fatalf("spares: got %v, want owner A's leg %d alone",
			spares, spare.GetLegId())
	}
}

// TestCreateSpareLegRefusesASecondOwnerAfterTheFlip is the same overlap once
// RW18 has flipped the first owner's spare, which lifts the hold above: a
// group may keep MaxSpareLegPerGrp spares of standby capacity, and nothing
// records which repair a spare was made for. The gain check closes that gap
// for two plans read from one snapshot, as here; AR2 names the one case it
// leaves open. The second owner's plan, read before the first create, does
// not hold the first spare's DN, so its STM finds the group has gained a DN
// and fails with ReasonCandidateChanged, writing nothing; its next pass
// plans from a snapshot that holds the spare, which AR8 switches in or
// waits for. A create planned after the flip, from a plan that holds the
// spare, still commits.
func TestCreateSpareLegRefusesASecondOwnerAfterTheFlip(t *testing.T) {
	env := newOpsEnv(t)
	plan, ownerB, spare := overlapSpareCreate(t, env)
	if _, _, err := FlipProvisioned(
		env.ctx, env.cli, env.cid, opsShard, opsSpId,
		[]SideRef{{
			SliceId: opsSliceId,
			LegId:   spare.GetLegId(),
			SideId:  spare.GetSideList()[0].GetSideId(),
		}},
	); err != nil {
		t.Fatalf("FlipProvisioned: %v", err)
	}
	witness := env.witnessSpareRefusal(opsDnD)
	_, err := CreateSpareLeg(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, opsDataGrpId, ownerB, plan, env.cc,
	)
	precondition := wantPrecondition(t, err, opCreateSpareLeg)
	if precondition.Reason != ReasonCandidateChanged {
		t.Errorf("Reason: got %q, want %q",
			precondition.Reason, ReasonCandidateChanged)
	}
	witness.check(t, env)
	// Planned again, from the group as it is now, the same DN commits.
	if _, err := CreateSpareLeg(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, opsDataGrpId, env.dnCand(opsDnD),
		env.grpDnAddrs(opsDataGrpId), env.cc,
	); err != nil {
		t.Fatalf("CreateSpareLeg from a fresh plan: %v", err)
	}
	spares := findGroup(env.slice(), opsDataGrpId).GetSpareLegList()
	if len(spares) != 2 {
		t.Errorf("spares after the fresh plan: got %d, want 2", len(spares))
	}
}

// TestCreateSpareLegPreconditions is the op's refusals, each writing nothing.
// The plan a row hands the op is read after its setup unless planBefore is
// set: a plan read before the setup is the snapshot a caller planned from
// when the setup's change landed between its scan and its STM.
//
// The rows split three ways. A DN the group gained since the plan, even the
// pick's own, and a pick gone stale since the scan — its capacity key moved,
// its DnConf deleted, the DN disabled or short of extents — are all
// ReasonCandidateChanged, on which the caller plans again. A pick inside
// the caller's own plan stays the hard "dn already in the group": no caller
// that black-lists its plan can hand one. The rest are the op's own
// refusals.
func TestCreateSpareLegPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		grpId      uint64
		planBefore bool
		setup      func(env *opsEnv) Cand
		reason     string
	}{
		{
			name:   "group not found",
			grpId:  9999,
			setup:  func(env *opsEnv) Cand { return env.dnCand(opsDnC) },
			reason: "group not found",
		},
		{
			name:   "dn already in the group",
			grpId:  opsDataGrpId,
			setup:  func(env *opsEnv) Cand { return env.dnCand(opsDnA) },
			reason: "dn already in the group",
		},
		{
			// Provisioned, or the unprovisioned-spare refusal would answer
			// first. The plan holds the spare, so this is a pick inside it.
			name:  "dn already a spare of the group",
			grpId: opsDataGrpId,
			setup: func(env *opsEnv) Cand {
				env.addSpare(true)
				return env.dnCand(opsDnC)
			},
			reason: "dn already in the group",
		},
		{
			// A spare committed after the plan, on a DN of its own: the
			// pick may share its failure domain, which only the plan can
			// tell.
			name:       "a spare the plan did not hold",
			grpId:      opsDataGrpId,
			planBefore: true,
			setup: func(env *opsEnv) Cand {
				env.addSpare(true)
				return env.dnCand(opsDnD)
			},
			reason: ReasonCandidateChanged,
		},
		{
			// A migration's destination, the second side of an active leg,
			// is a gain too, and the unprovisioned-spare hold, which reads
			// spares only, does not see it.
			name:       "a migration destination the plan did not hold",
			grpId:      opsDataGrpId,
			planBefore: true,
			setup: func(env *opsEnv) Cand {
				slice := env.slice()
				leg := findLeg(slice, opsDataLegB)
				leg.SideList = append(leg.SideList,
					opsSide(opsDataSideB+100, opsDnD, false))
				mustPut(env.t, env.cli,
					SliceKey(env.cid, opsSpId, opsSliceId), slice)
				return env.dnCand(opsDnC)
			},
			reason: ReasonCandidateChanged,
		},
		{
			// The gain on the pick's own DN is a stale plan as well, and
			// the gain check comes before "dn already in the group", so the
			// caller plans again instead of reading a refusal.
			name:       "a gain the plan did not hold on the pick's dn",
			grpId:      opsDataGrpId,
			planBefore: true,
			setup: func(env *opsEnv) Cand {
				env.addSpare(true)
				return env.dnCand(opsDnC)
			},
			reason: ReasonCandidateChanged,
		},
		{
			name:       "the pick's dn_conf deleted",
			grpId:      opsDataGrpId,
			planBefore: true,
			setup: func(env *opsEnv) Cand {
				cand := env.dnCand(opsDnC)
				env.delKey(DnConfKey(env.cid, opsDnC))
				return cand
			},
			reason: ReasonCandidateChanged,
		},
		{
			name:       "the pick disabled",
			grpId:      opsDataGrpId,
			planBefore: true,
			setup: func(env *opsEnv) Cand {
				cand := env.dnCand(opsDnC)
				dn := env.dn(opsDnC)
				dn.Disabled = true
				mustPut(env.t, env.cli, DnConfKey(env.cid, opsDnC), dn)
				return cand
			},
			reason: ReasonCandidateChanged,
		},
		{
			// A migration off a spare gives it a second side, unprovisioned
			// until the destination is zeroed: ANY side of a spare counts.
			name:  "a spare's second side is unprovisioned",
			grpId: opsDataGrpId,
			setup: func(env *opsEnv) Cand {
				env.addSpare(true)
				slice := env.slice()
				spare := findLeg(slice, opsSpareLeg)
				spare.SideList = append(spare.SideList,
					opsSide(opsSpareSide+1, opsDnD, false))
				mustPut(env.t, env.cli,
					SliceKey(env.cid, opsSpId, opsSliceId), slice)
				env.putDn("dn-e:9000", 704, 4, opsDnFree)
				return env.dnCand("dn-e:9000")
			},
			reason: ReasonSpareUnprovisioned,
		},
		{
			name:  "group is not md-raid1",
			grpId: opsDataGrpId,
			setup: func(env *opsEnv) Cand {
				conf := env.spConf()
				conf.BdevConf.RedundConf = &pb.RedundConf{
					RedunKind: &pb.RedundConf_RedundNone{
						RedundNone: &pb.RedundNone{},
					},
				}
				mustPut(env.t, env.cli,
					SpConfKey(env.cid, opsSpName), conf)
				return env.dnCand(opsDnC)
			},
			reason: "group is not md-raid1",
		},
		{
			name:  "candidate changed",
			grpId: opsDataGrpId,
			setup: func(env *opsEnv) Cand {
				cand := env.dnCand(opsDnC)
				env.delKey(DnCapacityKey(
					env.cid, mustBin(env.t, env, opsDnFree),
					opsDnFree, opsDnC,
				))
				env.putDn(opsDnC, 702, 2, opsDnFree-1)
				return cand
			},
			reason: ReasonCandidateChanged,
		},
		{
			// A pick whose key still matches but whose free count is below
			// the group's ext_cnt is stale all the same: a rescan never
			// offers it.
			name:  "the pick short of extents",
			grpId: opsDataGrpId,
			setup: func(env *opsEnv) Cand {
				env.putDn(opsDnC, 702, 2, 2)
				return env.dnCand(opsDnC)
			},
			reason: ReasonCandidateChanged,
		},
		{
			name:  "sp level suppresses reactions",
			grpId: opsDataGrpId,
			setup: func(env *opsEnv) Cand {
				cand := env.dnCand(opsDnC)
				env.setLevel(pb.SpLevel_SP_LEVEL_NO_THINPOOL)
				return cand
			},
			reason: "sp level suppresses reactions",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			var plan []string
			if tc.planBefore {
				plan = env.grpDnAddrs(tc.grpId)
			}
			cand := tc.setup(env)
			if !tc.planBefore {
				plan = env.grpDnAddrs(tc.grpId)
			}
			revBefore := env.spRev()
			nextBefore := env.spConf().GetNextId()
			spareBefore := len(
				findGroup(env.slice(), opsDataGrpId).GetSpareLegList(),
			)
			_, err := CreateSpareLeg(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
				opsSliceId, tc.grpId, cand, plan, env.cc,
			)
			precondition := wantPrecondition(t, err, opCreateSpareLeg)
			if precondition.Reason != tc.reason {
				t.Errorf(
					"Reason: got %q, want %q",
					precondition.Reason, tc.reason,
				)
			}
			spareAfter := len(
				findGroup(env.slice(), opsDataGrpId).GetSpareLegList(),
			)
			if spareAfter != spareBefore {
				t.Errorf("the spare list must be untouched")
			}
			if got := env.spConf().GetNextId(); got != nextBefore {
				t.Errorf("next_id: got %d, want %d", got, nextBefore)
			}
			if got := env.spRev(); got != revBefore {
				t.Errorf("SpRev: got %d, want %d", got, revBefore)
			}
		})
	}
}

// TestCreateSpareLegChecksOnlyAGain is the other half of the gain check, as
// TestReplaceCntlrChecksOnlyAGain is ReplaceCntlr's: a DN the plan held that
// has left the group since is no reason to plan again. The pick is still off
// every DN and out of every location the group's sides hold, which were all
// in the plan, and the room the departure freed is a candidate the scan did
// not count on, like a DN that joins after it.
func TestCreateSpareLegChecksOnlyAGain(t *testing.T) {
	env := newOpsEnv(t)
	// The plan names dn-d, which no side of the group holds any more.
	plan := append(env.grpDnAddrs(opsDataGrpId), opsDnD)
	legId, err := CreateSpareLeg(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, opsDataGrpId, env.dnCand(opsDnC), plan, env.cc,
	)
	if err != nil {
		t.Fatalf("CreateSpareLeg: %v", err)
	}
	spare := findLeg(env.slice(), legId)
	if spare == nil || singleSideAddr(spare) != opsDnC {
		t.Errorf("spare: got %v, want leg %d on %s", spare, legId, opsDnC)
	}
}

// singleSideAddr is the DN of a leg's one side, or "" when it has not
// exactly one.
func singleSideAddr(leg *pb.Leg) string {
	if len(leg.GetSideList()) != 1 {
		return ""
	}
	return leg.GetSideList()[0].GetAddrPort()
}

func TestSwitchSpareLeg(t *testing.T) {
	env := newOpsEnv(t)
	env.addSpare(true)
	// Park the target's err_epoch so that the parked leg can be checked.
	if err := SetLegErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsSliceId, opsDataLegB, 1234,
	); err != nil {
		t.Fatalf("SetLegErrEpoch: %v", err)
	}
	revBefore := env.spRev()
	if err := SwitchSpareLeg(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, opsDataGrpId, opsSpareLeg, opsDataLegB,
	); err != nil {
		t.Fatalf("SwitchSpareLeg: %v", err)
	}
	grp := findGroup(env.slice(), opsDataGrpId)
	if len(grp.GetLegList()) != 2 || len(grp.GetSpareLegList()) != 1 {
		t.Fatalf("lists: got %d active, %d spare",
			len(grp.GetLegList()), len(grp.GetSpareLegList()))
	}
	if grp.GetLegList()[0].GetLegId() != opsDataLegA {
		t.Errorf("the untouched leg must keep its position")
	}
	if grp.GetLegList()[1].GetLegId() != opsSpareLeg {
		t.Errorf("the spare must take the target's position: got %d",
			grp.GetLegList()[1].GetLegId())
	}
	if grp.GetLegList()[1].GetLegIdx() != 2 {
		t.Errorf("the spare keeps its own leg_idx: got %d",
			grp.GetLegList()[1].GetLegIdx())
	}
	parked := grp.GetSpareLegList()[0]
	if parked.GetLegId() != opsDataLegB {
		t.Errorf("the target must be parked: got %d", parked.GetLegId())
	}
	if parked.GetErrEpoch() != 1234 {
		t.Errorf("a parked leg keeps its err_epoch: got %d",
			parked.GetErrEpoch())
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev: got %d, want %d", got, revBefore+1)
	}
	// Re-running is refused: the spare is now an active leg.
	err := SwitchSpareLeg(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, opsDataGrpId, opsSpareLeg, opsDataLegB,
	)
	precondition := wantPrecondition(t, err, opSwitchSpareLeg)
	if precondition.Reason != "spare leg not found" {
		t.Errorf("Reason: got %q", precondition.Reason)
	}
}

func TestSwitchSpareLegPreconditions(t *testing.T) {
	// addMigrDst gives one leg the second side a CreateMigration hangs off
	// it (architecture.md, Migrations): an unprovisioned destination on dn-d, the
	// one DN no leg of the fixture uses.
	addMigrDst := func(env *opsEnv, legId uint64) {
		slice := env.slice()
		leg := findLeg(slice, legId)
		leg.SideList = append(leg.SideList, opsSide(461, opsDnD, false))
		mustPut(env.t, env.cli, SliceKey(env.cid, opsSpId, opsSliceId), slice)
	}
	for _, tc := range []struct {
		name     string
		setup    func(env *opsEnv)
		grpId    uint64
		spareId  uint64
		targetId uint64
		reason   string
	}{
		{
			name:     "spare side is not provisioned",
			setup:    func(env *opsEnv) { env.addSpare(false) },
			grpId:    opsDataGrpId,
			spareId:  opsSpareLeg,
			targetId: opsDataLegB,
			reason:   "spare side is not provisioned",
		},
		{
			name: "spare leg under migration",
			setup: func(env *opsEnv) {
				env.addSpare(true)
				addMigrDst(env, opsSpareLeg)
			},
			grpId:    opsDataGrpId,
			spareId:  opsSpareLeg,
			targetId: opsDataLegB,
			reason:   "spare leg has no single side",
		},
		{
			// AR8 leaves a migrating leg alone, and the STM re-checks it:
			// a migration may start after the worker's pass read the leg.
			name: "target leg under migration",
			setup: func(env *opsEnv) {
				env.addSpare(true)
				addMigrDst(env, opsDataLegB)
			},
			grpId:    opsDataGrpId,
			spareId:  opsSpareLeg,
			targetId: opsDataLegB,
			reason:   "target leg has no single side",
		},
		{
			name:     "spare leg not found",
			setup:    func(env *opsEnv) { env.addSpare(true) },
			grpId:    opsDataGrpId,
			spareId:  9999,
			targetId: opsDataLegB,
			reason:   "spare leg not found",
		},
		{
			name:     "target leg not found",
			setup:    func(env *opsEnv) { env.addSpare(true) },
			grpId:    opsDataGrpId,
			spareId:  opsSpareLeg,
			targetId: 9999,
			reason:   "target leg not found",
		},
		{
			name:     "target must be active, not a spare",
			setup:    func(env *opsEnv) { env.addSpare(true) },
			grpId:    opsDataGrpId,
			spareId:  opsSpareLeg,
			targetId: opsSpareLeg,
			reason:   "target leg not found",
		},
		{
			name:     "group not found",
			setup:    func(env *opsEnv) { env.addSpare(true) },
			grpId:    9999,
			spareId:  opsSpareLeg,
			targetId: opsDataLegB,
			reason:   "group not found",
		},
		{
			name: "sp deleting",
			setup: func(env *opsEnv) {
				env.addSpare(true)
				env.setDeleting()
			},
			grpId:    opsDataGrpId,
			spareId:  opsSpareLeg,
			targetId: opsDataLegB,
			reason:   "sp deleting",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			tc.setup(env)
			revBefore := env.spRev()
			err := SwitchSpareLeg(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
				opsSliceId, tc.grpId, tc.spareId, tc.targetId,
			)
			precondition := wantPrecondition(t, err, opSwitchSpareLeg)
			if precondition.Reason != tc.reason {
				t.Errorf(
					"Reason: got %q, want %q",
					precondition.Reason, tc.reason,
				)
			}
			grp := findGroup(env.slice(), opsDataGrpId)
			if grp.GetLegList()[1].GetLegId() != opsDataLegB {
				t.Errorf("the active list must be untouched")
			}
			if got := env.spRev(); got != revBefore {
				t.Errorf("SpRev: got %d, want %d", got, revBefore)
			}
		})
	}
}
