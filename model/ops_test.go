package model

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// noExpectRev is the expectRev the worker's own calls pass to the three ops
// the gateway shares with it (gateway.md §2.2 #3): 0 skips the SpRev token
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
	return &pb.SpConf{
		SpId:      opsSpId,
		ShardCode: opsShard,
		NextId:    opsNextId,
		NextDevId: 1,
		BdevConf: &pb.BdevConf{
			DmPoolConf: &pb.DmPoolConf{DataBlockSize: opsBlockSize},
			RedundConf: &pb.RedundConf{
				RedunKind: &pb.RedundConf_RedundMdRaid1{
					RedundMdRaid1: &pb.RedundMdRaid1{
						BitmapChunkBlockCnt: common.DefaultChunkBlockCnt,
					},
				},
			},
		},
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
// mutate. It skips when no etcd binary is available (EU7).
func newOpsEnv(t *testing.T) *opsEnv {
	t.Helper()
	cli := newTestClient(t)
	env := &opsEnv{
		t:   t,
		ctx: context.Background(),
		cli: cli,
		cid: testCid(t),
		cc: &pb.ClusterConf{
			DnBinConf: &pb.DnBinConf{ExtentSize: opsExtSize},
		},
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

// putDn writes one DnConf, its capacity key (when the §5.6 rule says it has
// one) and its DnRev, all consistently.
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

// dnCand rebuilds the candidate a §6.3 scan would have produced for a DN.
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

// cnCand rebuilds the candidate a §6.4 scan would have produced for a CN.
func (e *opsEnv) cnCand(addrPort string) Cand {
	e.t.Helper()
	cn := e.cn(addrPort)
	return Cand{
		AddrPort: addrPort,
		Location: cn.GetLocation(),
		FreeExt:  cn.GetFreeExtCnt(),
	}
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

func TestResolveEventThreshold(t *testing.T) {
	resolved := ResolveEventThreshold(nil)
	if resolved.GetPrimaryUnhealthy() != common.DefaultPrimaryUnhealthy ||
		resolved.GetCntlrUnhealthy() != common.DefaultCntlrUnhealthy ||
		resolved.GetSideUnhealthy() != common.DefaultSideUnhealthy ||
		resolved.GetLegUnhealthy() != common.DefaultLegUnhealthy {
		t.Errorf("nil must resolve to the §7 defaults: %v", resolved)
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

// TestGroupBlocks pins the §3.6 worked example: a 1 TiB raid1 group with 1 GiB
// extents, 1 MiB blocks and 128-block bitmap chunks has meta_blocks = 3.
func TestGroupBlocks(t *testing.T) {
	raid1 := &pb.BdevConf{
		DmPoolConf: &pb.DmPoolConf{DataBlockSize: opsBlockSize},
		RedundConf: &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{BitmapChunkBlockCnt: 128},
			},
		},
	}
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
	// Defaults resolve to exactly the same numbers.
	bare := &pb.BdevConf{
		RedundConf: &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundMdRaid1{
				RedundMdRaid1: &pb.RedundMdRaid1{},
			},
		},
	}
	defMeta, defData, err := GroupBlocks(1024, 0, bare)
	if err != nil {
		t.Fatalf("GroupBlocks defaults: %v", err)
	}
	if defMeta != metaBlocks || defData != dataBlocks {
		t.Errorf(
			"defaults: got meta %d data %d, want %d / %d",
			defMeta, defData, metaBlocks, dataBlocks,
		)
	}
	// RedundNone keeps only the health block.
	none := &pb.BdevConf{
		DmPoolConf: &pb.DmPoolConf{DataBlockSize: opsBlockSize},
	}
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
	tiny := &pb.BdevConf{
		DmPoolConf: &pb.DmPoolConf{DataBlockSize: opsExtSize},
	}
	if _, _, err := GroupBlocks(1, opsExtSize, tiny); err == nil {
		t.Errorf("a one-block group must fail")
	}
}

// TestMetaLadderExtCnt walks the §8.5 ladder 1 → 2 → 4 → 8 → 16 GiB and
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
		t.Errorf("an unhealthy DN must have no capacity key (§5.6)")
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
		env.ctx, env.cli, env.cid, opsSpId, opsCntlrA, 1000,
	); err != nil {
		t.Fatalf("SetCntlrErrEpoch: %v", err)
	}
	if got := env.cntlr(opsCntlrA).GetErrEpoch(); got != 1000 {
		t.Errorf("err_epoch: got %d, want 1000", got)
	}
	if err := SetCntlrErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsCntlrA, 2000,
	); err != nil {
		t.Fatalf("SetCntlrErrEpoch again: %v", err)
	}
	if got := env.cntlr(opsCntlrA).GetErrEpoch(); got != 1000 {
		t.Errorf("the threshold clock must not restart: got %d", got)
	}
	if err := SetCntlrErrEpoch(
		env.ctx, env.cli, env.cid, opsSpId, opsCntlrA, 0,
	); err != nil {
		t.Fatalf("SetCntlrErrEpoch clear: %v", err)
	}
	if got := env.cntlr(opsCntlrA).GetErrEpoch(); got != 0 {
		t.Errorf("err_epoch: got %d, want 0", got)
	}
	if got := env.spRev(); got != revBefore {
		t.Errorf("health must not bump SpRev: got %d, want %d", got, revBefore)
	}
	err := SetCntlrErrEpoch(env.ctx, env.cli, env.cid, opsSpId, 999, 1)
	wantPrecondition(t, err, opSetCntlrErrEpoch)
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
	// A SPARE leg is found too (§8.12: spares are probed like active legs).
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
	flipped, err := FlipProvisioned(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, sides,
	)
	if err != nil {
		t.Fatalf("FlipProvisioned: %v", err)
	}
	// The op reports the sides it WROTE, in the order they were listed: the
	// caller logs one §12 "flip applied" record per side.
	if !equalSideRefs(flipped, sides) {
		t.Errorf("flipped: got %v, want %v", flipped, sides)
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
	flipped, err = FlipProvisioned(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, sides,
	)
	if err != nil {
		t.Fatalf("FlipProvisioned again: %v", err)
	}
	if len(flipped) != 0 {
		t.Errorf("flipped: got %v, want none", flipped)
	}
	if got := env.spRev(); got != revBefore+1 {
		t.Errorf("SpRev must not move: got %d, want %d", got, revBefore+1)
	}
	// A stale side pointer is skipped, not an error; a wrong leg id makes
	// the pointer stale too.
	flipped, err = FlipProvisioned(
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
	_, err = FlipProvisioned(
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
			reason: "old cntlr is healthy",
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
// on one SP cannot apply an action twice: the second STM fails its
// precondition", applied to the one reaction whose STM used to have no
// re-validating precondition of its own.
//
// Both owners evaluate the SAME pre-grow snapshot during an accepted
// shard-handoff overlap (§0 item 4): both find AR6's pending rule false, both
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
// then hits the §8.5 cap.
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
		opsCntlrB, newCn, false, now,
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
		opsCntlrA, env.cnCand(opsCnC), true, now,
	)
	if err != nil {
		t.Fatalf("ReplaceCntlr: %v", err)
	}
	fresh := env.cntlr(newId)
	if !fresh.GetPrimary() {
		t.Errorf("the replacement of a sole primary must be primary")
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOpsEnv(t)
			cand := tc.setup(env)
			revBefore := env.spRev()
			_, err := ReplaceCntlr(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName,
				tc.oldId, cand, tc.asPrimary, tc.now,
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
			if got := env.spConf().GetNextId(); got != opsNextId {
				t.Errorf("next_id: got %d, want %d", got, opsNextId)
			}
			if got := env.spRev(); got != revBefore {
				t.Errorf("SpRev: got %d, want %d", got, revBefore)
			}
		})
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
		opsSliceId, opsDataGrpId, env.dnCand(opsDnC), env.cc,
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
	// A second spare on a different DN fills the list; a third is refused.
	if _, err := CreateSpareLeg(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, opsDataGrpId, env.dnCand(opsDnD), env.cc,
	); err != nil {
		t.Fatalf("CreateSpareLeg second: %v", err)
	}
	env.putDn("dn-e:9000", 704, 4, opsDnFree)
	_, err = CreateSpareLeg(
		env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
		opsSliceId, opsDataGrpId, env.dnCand("dn-e:9000"), env.cc,
	)
	precondition := wantPrecondition(t, err, opCreateSpareLeg)
	if precondition.Reason != "spare list full" {
		t.Errorf("Reason: got %q", precondition.Reason)
	}
}

func TestCreateSpareLegPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		grpId  uint64
		setup  func(env *opsEnv) Cand
		reason string
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
			name:  "dn already a spare of the group",
			grpId: opsDataGrpId,
			setup: func(env *opsEnv) Cand {
				env.addSpare(false)
				return env.dnCand(opsDnC)
			},
			reason: "dn already in the group",
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
			name:  "dn free_ext_cnt too low",
			grpId: opsDataGrpId,
			setup: func(env *opsEnv) Cand {
				env.putDn(opsDnC, 702, 2, 2)
				return env.dnCand(opsDnC)
			},
			reason: "dn free_ext_cnt too low",
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
			cand := tc.setup(env)
			revBefore := env.spRev()
			spareBefore := len(
				findGroup(env.slice(), opsDataGrpId).GetSpareLegList(),
			)
			_, err := CreateSpareLeg(
				env.ctx, env.cli, env.cid, opsShard, opsSpId, opsSpName, noExpectRev,
				opsSliceId, tc.grpId, cand, env.cc,
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
			if got := env.spRev(); got != revBefore {
				t.Errorf("SpRev: got %d, want %d", got, revBefore)
			}
		})
	}
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
