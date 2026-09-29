package worker

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The fixture SP (§13: "golden requests from a fixture SpState")
// ---------------------------------------------------------------------------

const (
	testSpId   = uint64(0x5100)
	testSpName = "sp0"
	testSpRev  = uint64(42)

	// Four DNs: the meta side, the migration source, the migration
	// destination and the spare leg's side.
	spDnA = "spdn0:9520"
	spDnB = "spdn1:9520"
	spDnC = "spdn2:9520"
	spDnD = "spdn3:9520"

	// Three CNs: the primary, a standby and a DISABLED standby, which keeps
	// its standby shape (RW15).
	spCnA = "spcn0:9620"
	spCnB = "spcn1:9620"
	spCnC = "spcn2:9620"

	spDnIdA = uint64(0xd0)
	spDnIdB = uint64(0xd1)
	spDnIdC = uint64(0xd2)
	spDnIdD = uint64(0xd3)

	spCnIdA = uint64(0xc0)
	spCnIdB = uint64(0xc1)
	spCnIdC = uint64(0xc2)

	spSliceA = uint64(10)
	spSliceB = uint64(11)

	spLegMeta  = uint64(1000)
	spLegMigr  = uint64(1001)
	spLegSpare = uint64(1002)
	spLegB     = uint64(1010)

	spSideMeta  = uint64(2000)
	spSideSrc   = uint64(2001)
	spSideDst   = uint64(2002)
	spSideSpare = uint64(2003)
	spSideB     = uint64(2010)

	spCntlrPrimary  = uint64(1)
	spCntlrStandby  = uint64(2)
	spCntlrDisabled = uint64(3)

	spMigrName = "migr0"
	spMigrId   = uint64(300)
	spCloneNm  = "clone0"
	spCloneId  = uint64(400)
	spXferName = "xfer0"

	spTdOpen = uint64(500)
	spTdDone = uint64(501)
)

// spCloneChunks are the fixture clone's bitmap chunks as MD3 reports them: two
// chunks of two DIFFERENT source slices, so every test that carries a pair
// around fails when only the bm_idx survives. The second slice is not slice 1,
// because the chunks are gap-tolerant (architecture.md §9.6) and nothing
// derives one index from the other.
var spCloneChunks = []model.BmChunk{
	{SliceIdx: 0, Idx: 0, ModRev: 11},
	{SliceIdx: 2, Idx: 1, ModRev: 12},
}

// srcTrConf is the source side's transport conf, which RW15 copies into the
// destination's migr_dst_conf.
func srcTrConf() *pb.NvmeTrConf {
	return &pb.NvmeTrConf{
		TrType:  "tcp",
		AdrFam:  "ipv4",
		TrAddr:  "10.0.0.2",
		TrSvcId: "4420",
	}
}

// spFixture builds the SpState every sprole test drives: two slices, a meta
// group, a data group whose leg carries a migration (two sides), a SPARE leg,
// and three cntlrs of which one is disabled.
func spFixture() *model.SpState {
	state := &model.SpState{
		Rev: 7,
		// The revision the harness delivers: a load ahead of it would build
		// nothing (RW14).
		SpRevision: testSpRev,
		Conf: &pb.SpConf{
			SpId:      testSpId,
			ShardCode: testShard,
			// The concrete geometry CreateStoragePool stored (§7): every
			// member the worker forwards is a value the SP was created with.
			BdevConf: testBdevConf(),
			SpLevel:  pb.SpLevel_SP_LEVEL_READWRITE,
			CntlrIdList: []uint64{
				spCntlrPrimary, spCntlrStandby, spCntlrDisabled,
			},
			SliceIdList:   []uint64{spSliceA, spSliceB},
			TdNameList:    []string{"td0", "td1"},
			NqnList:       []string{"nqn.2024-01.io.dnv:sp0"},
			CloneNameList: []string{spCloneNm},
			XferNameList:  []string{spXferName},
			MigrNameList:  []string{spMigrName},
		},
		Cntlrs: map[uint64]*pb.Cntlr{
			spCntlrPrimary: {
				AddrPort:   spCnA,
				CntlidSlot: 10000,
				Primary:    true,
			},
			spCntlrStandby: {
				AddrPort:   spCnB,
				CntlidSlot: 15000,
			},
			spCntlrDisabled: {
				AddrPort:   spCnC,
				CntlidSlot: 20000,
				Disabled:   true,
			},
		},
		// The groups' meta_blocks / data_blocks are hand-chosen numbers, NOT
		// what model.GroupBlocks computes for this SP's stored geometry: the
		// worker only ever forwards a group's stored counts (sprole.go reads
		// grp.GetMetaBlocks()) and model.GrowSlice recomputes its own inside
		// its STM, so a fixture whose numbers a recomputation would reproduce
		// could not tell the two apart.
		Slices: map[uint64]*pb.Slice{
			spSliceA: {
				SliceIdx: 0,
				MetaGrpList: []*pb.Group{{
					GrpId:      100,
					ExtCnt:     1,
					MetaBlocks: 3,
					DataBlocks: 1021,
					LegList: []*pb.Leg{{
						LegId: spLegMeta,
						SideList: []*pb.Side{{
							SideId:      spSideMeta,
							AddrPort:    spDnA,
							CntlidSlot:  10000,
							Provisioned: true,
						}},
					}},
				}},
				DataGrpList: []*pb.Group{{
					GrpId:      101,
					ExtCnt:     8,
					MetaBlocks: 5,
					DataBlocks: 8187,
					LegList: []*pb.Leg{{
						LegId: spLegMigr,
						SideList: []*pb.Side{
							{
								SideId:      spSideSrc,
								AddrPort:    spDnB,
								CntlidSlot:  10000,
								NvmeTrConf:  srcTrConf(),
								Provisioned: true,
							},
							{
								SideId:     spSideDst,
								AddrPort:   spDnC,
								CntlidSlot: 15000,
							},
						},
					}},
					SpareLegList: []*pb.Leg{{
						LegId: spLegSpare,
						SideList: []*pb.Side{{
							SideId:     spSideSpare,
							AddrPort:   spDnD,
							CntlidSlot: 10000,
						}},
					}},
				}},
			},
			spSliceB: {
				SliceIdx: 1,
				DataGrpList: []*pb.Group{{
					GrpId:      110,
					ExtCnt:     8,
					MetaBlocks: 5,
					DataBlocks: 8187,
					LegList: []*pb.Leg{{
						LegId: spLegB,
						SideList: []*pb.Side{{
							SideId:      spSideB,
							AddrPort:    spDnA,
							CntlidSlot:  10000,
							Provisioned: true,
						}},
					}},
				}},
			},
		},
		Tds: []*pb.ThinDevice{
			{TdId: spTdOpen, DevId: 1, Size: 1 << 30},
			{TdId: spTdDone, DevId: 2, Size: 1 << 30, Created: true},
		},
		TdNames: []string{"td0", "td1"},
		Subsystems: map[string]*pb.Subsystem{
			"nqn.2024-01.io.dnv:sp0": {SsId: 600, Serial: "dnv0"},
		},
		Clones: map[string]*pb.Clone{
			spCloneNm: {CloneId: spCloneId},
		},
		Xfers: map[string]*pb.Transfer{
			spXferName: {XferId: 700},
		},
		Migrs: map[string]*pb.Migration{
			spMigrName: {
				MigrId:      spMigrId,
				SrcSideId:   spSideSrc,
				DstSideId:   spSideDst,
				BmCnt:       2,
				DmCloneConf: &pb.DmCloneConf{},
			},
		},
		CloneBmIdx: map[string][]model.BmChunk{
			spCloneNm: spCloneChunks,
		},
		MigrBmIdx: map[string][]model.BmChunk{
			spMigrName: {{Idx: 0, ModRev: 21}, {Idx: 1, ModRev: 22}},
		},
		DnByAddr: map[string]*pb.DnConf{
			spDnA: {DnId: spDnIdA},
			spDnB: {DnId: spDnIdB, NvmeTrConf: srcTrConf()},
			spDnC: {DnId: spDnIdC},
			spDnD: {DnId: spDnIdD},
		},
		CnByAddr: map[string]*pb.CnConf{
			spCnA: {CnId: spCnIdA},
			spCnB: {CnId: spCnIdB},
			spCnC: {CnId: spCnIdC},
		},
	}
	return state
}

// spTestWorker builds a coordinator that only builds plans (no children), for
// the golden-request tests.
func spTestWorker(d *deps) *spWorker {
	return &spWorker{
		deps:     d,
		ctx:      context.Background(),
		shard:    testShard,
		cid:      testCid,
		spId:     testSpId,
		seed:     seedOf(3),
		desired:  desiredState{revision: testSpRev, handle: testSpName},
		sides:    make(map[sideKey]*sideChild),
		cntlrs:   make(map[uint64]*cntlrChild),
		legs:     make(map[uint64]*healthMonitor),
		legSlice: make(map[uint64]uint64),
		tdRefs:   make(map[uint64]model.TdRef),
		reportCh: make(chan spReport, 8),
	}
}

// ---------------------------------------------------------------------------
// RW15 / RW16 golden requests
// ---------------------------------------------------------------------------

// TestSpSideRequestGolden pins RW15: one request per side — spare legs
// included — with the owning group's ext_cnt, the primary's cn_id, the
// standby list in cntlr_id_list order WITH the disabled cntlr present, and the
// migration source / destination confs of the two-sided leg. The destination's
// block_size is the SP's STORED data_block_size, forwarded (§7); only the
// dm-clone hydration knobs are still resolved here, because they are a policy
// timer rather than geometry.
func TestSpSideRequestGolden(t *testing.T) {
	captureLogs(t)
	w := spTestWorker(nil)
	plan := w.buildPlan(context.Background(), spFixture())

	if len(plan.sides) != 5 {
		t.Fatalf("%d side children, want 5 (spare included)", len(plan.sides))
	}
	standby := []uint64{spCnIdB, spCnIdC}

	meta := plan.sides[sideKey{legId: spLegMeta, sideId: spSideMeta}]
	if meta == nil {
		t.Fatalf("no plan for the meta side")
	}
	want := &pb.SyncupSideRequest{
		ClusterId: testCid,
		DnId:      spDnIdA,
		SidePointer: &pb.SidePointer{
			SpId:   testSpId,
			LegId:  spLegMeta,
			SideId: spSideMeta,
		},
		Revision: testSpRev,
		SideConf: &pb.SyncupSideRequest_SideConf{
			ExtCnt:        1,
			CntlidSlot:    10000,
			PrimaryCnId:   spCnIdA,
			StandbyIdList: standby,
			SpLevel:       pb.SpLevel_SP_LEVEL_READWRITE,
			Provisioned:   true,
		},
	}
	if !proto.Equal(meta.req, want) {
		t.Fatalf("meta side request =\n%v\nwant\n%v", meta.req, want)
	}

	src := plan.sides[sideKey{legId: spLegMigr, sideId: spSideSrc}]
	want = &pb.SyncupSideRequest{
		ClusterId: testCid,
		DnId:      spDnIdB,
		SidePointer: &pb.SidePointer{
			SpId:   testSpId,
			LegId:  spLegMigr,
			SideId: spSideSrc,
		},
		Revision: testSpRev,
		SideConf: &pb.SyncupSideRequest_SideConf{
			ExtCnt:        8,
			CntlidSlot:    10000,
			PrimaryCnId:   spCnIdA,
			StandbyIdList: standby,
			SpLevel:       pb.SpLevel_SP_LEVEL_READWRITE,
			Provisioned:   true,
		},
		MigrSrcConf: &pb.SyncupSideRequest_MigrSrcConf{
			MigrId:         spMigrId,
			DstSideId:      spSideDst,
			DstDnId:        spDnIdC,
			DstProvisioned: false,
		},
	}
	if !proto.Equal(src.req, want) {
		t.Fatalf("src side request =\n%v\nwant\n%v", src.req, want)
	}
	if src.migrName != "" {
		t.Fatalf("the SOURCE side pushes migration chunks (BM4)")
	}

	dst := plan.sides[sideKey{legId: spLegMigr, sideId: spSideDst}]
	want = &pb.SyncupSideRequest{
		ClusterId: testCid,
		DnId:      spDnIdC,
		SidePointer: &pb.SidePointer{
			SpId:   testSpId,
			LegId:  spLegMigr,
			SideId: spSideDst,
		},
		Revision: testSpRev,
		SideConf: &pb.SyncupSideRequest_SideConf{
			ExtCnt:        8,
			CntlidSlot:    15000,
			PrimaryCnId:   spCnIdA,
			StandbyIdList: standby,
			SpLevel:       pb.SpLevel_SP_LEVEL_READWRITE,
		},
		MigrDstConf: &pb.SyncupSideRequest_MigrDstConf{
			MigrId:        spMigrId,
			SrcSideId:     spSideSrc,
			SrcDnId:       spDnIdB,
			SrcNvmeTrConf: srcTrConf(),
			// The fixture SP's stored dm_pool_conf.data_block_size, forwarded
			// (RW15). testBlockSize is deliberately not the §7 default, so a
			// request that substituted the constant would carry 1 MiB here
			// and this golden would fail. meta_blocks is the fixture group's
			// own number for the same reason.
			BlockSize:  testBlockSize,
			MetaBlocks: 5,
			DmCloneConf: &pb.DmCloneConf{
				HydrationThreshold: common.DefaultMigrThreshold,
				HydrationBatchSize: common.DefaultMigrBatchSize,
			},
			BmCnt: 2,
		},
	}
	if !proto.Equal(dst.req, want) {
		t.Fatalf("dst side request =\n%v\nwant\n%v", dst.req, want)
	}
	// BM1/BM4: only the destination side carries the migration's chunks.
	if dst.migrName != spMigrName || dst.migrId != spMigrId ||
		len(dst.chunks) != 2 {
		t.Fatalf("dst side migration plan = %+v", dst)
	}

	// A spare leg's side is driven exactly like an active one (RW14).
	spare := plan.sides[sideKey{legId: spLegSpare, sideId: spSideSpare}]
	if spare == nil || spare.req.GetDnId() != spDnIdD {
		t.Fatalf("no plan for the spare leg's side")
	}
	if spare.req.GetSideConf().GetExtCnt() != 8 {
		t.Fatalf("spare ext_cnt = %d, want the group's 8",
			spare.req.GetSideConf().GetExtCnt())
	}

	// The two sides of one DN are two children, and every leg — spares
	// included — is mapped to its slice for the HL2 leg rows.
	for _, legId := range []uint64{spLegMeta, spLegMigr, spLegSpare} {
		if plan.legSlice[legId] != spSliceA {
			t.Fatalf("leg %d maps to slice %d", legId, plan.legSlice[legId])
		}
	}
	if plan.legSlice[spLegB] != spSliceB {
		t.Fatalf("leg of the second slice is not mapped")
	}
}

// TestSpSideRequestNoPrimary checks RW15's "0 if none": an SP whose cntlrs are
// all standbys sends primary_cn_id = 0 and lists every cntlr as a standby.
func TestSpSideRequestNoPrimary(t *testing.T) {
	captureLogs(t)
	state := spFixture()
	state.Cntlrs[spCntlrPrimary].Primary = false
	w := spTestWorker(nil)
	plan := w.buildPlan(context.Background(), state)
	conf := plan.sides[sideKey{legId: spLegMeta, sideId: spSideMeta}].
		req.GetSideConf()
	if conf.GetPrimaryCnId() != 0 {
		t.Fatalf("primary_cn_id = %d, want 0", conf.GetPrimaryCnId())
	}
	want := []uint64{spCnIdA, spCnIdB, spCnIdC}
	if len(conf.GetStandbyIdList()) != 3 {
		t.Fatalf("standby list = %v, want %v", conf.GetStandbyIdList(), want)
	}
	for i, cnId := range want {
		if conf.GetStandbyIdList()[i] != cnId {
			t.Fatalf("standby list = %v, want %v",
				conf.GetStandbyIdList(), want)
		}
	}
}

// TestSpCntlrRequestGolden pins RW16: every cntlr of the SP — the disabled one
// included — receives the FULL state, with id_to_slice keyed by
// fmt.Sprintf(IdKeyFmt, slice_id) and every list in SpConf's order.
func TestSpCntlrRequestGolden(t *testing.T) {
	captureLogs(t)
	state := spFixture()
	w := spTestWorker(nil)
	plan := w.buildPlan(context.Background(), state)

	if len(plan.cntlrs) != 3 {
		t.Fatalf("%d cntlr children, want 3", len(plan.cntlrs))
	}
	primary := plan.cntlrs[spCntlrPrimary]
	want := &pb.SyncupCntlrRequest{
		ClusterId: testCid,
		CnId:      spCnIdA,
		CntlrPointer: &pb.CntlrPointer{
			SpId:    testSpId,
			CntlrId: spCntlrPrimary,
		},
		Revision: testSpRev,
		BdevConf: state.Conf.GetBdevConf(),
		SpLevel:  pb.SpLevel_SP_LEVEL_READWRITE,
		Cntlr:    state.Cntlrs[spCntlrPrimary],
		IdToSlice: map[string]*pb.Slice{
			"000000000000000a": state.Slices[spSliceA],
			"000000000000000b": state.Slices[spSliceB],
		},
		TdList:         state.Tds,
		NqnToSubsystem: state.Subsystems,
		CloneList:      []*pb.Clone{state.Clones[spCloneNm]},
		XferList:       []*pb.Transfer{state.Xfers[spXferName]},
		MigrList:       []*pb.Migration{state.Migrs[spMigrName]},
	}
	if !proto.Equal(primary.req, want) {
		t.Fatalf("primary cntlr request =\n%v\nwant\n%v", primary.req, want)
	}
	// BM1/BM4: only the primary pushes clone chunks, addressed by the
	// (src_slice_idx, bm_idx) pairs the snapshot found in etcd (MD3).
	if len(primary.clones) != 1 || primary.clones[0].id != spCloneId ||
		len(primary.clones[0].chunks) != len(spCloneChunks) {
		t.Fatalf("primary clone plan = %+v", primary.clones)
	}
	for i, chunk := range primary.clones[0].chunks {
		if chunk != spCloneChunks[i] {
			t.Fatalf("clone chunk %d = %+v, want %+v",
				i, chunk, spCloneChunks[i])
		}
	}

	// The standbys carry the same state; only their own identity differs.
	for _, cntlrId := range []uint64{spCntlrStandby, spCntlrDisabled} {
		child := plan.cntlrs[cntlrId]
		if child == nil {
			t.Fatalf("no plan for cntlr %d", cntlrId)
		}
		if len(child.clones) != 0 {
			t.Fatalf("a standby was given clone chunks to push (BM4)")
		}
		mirrored := proto.Clone(child.req).(*pb.SyncupCntlrRequest)
		mirrored.CnId = want.GetCnId()
		mirrored.CntlrPointer = want.GetCntlrPointer()
		mirrored.Cntlr = want.GetCntlr()
		if !proto.Equal(mirrored, want) {
			t.Fatalf("cntlr %d request =\n%v\nwant the full state\n%v",
				cntlrId, child.req, want)
		}
	}
	// The slice ids RW19's condition (3) compares against.
	if len(primary.sliceIds) != 2 || primary.sliceIds[0] != spSliceA ||
		primary.sliceIds[1] != spSliceB {
		t.Fatalf("slice ids = %v", primary.sliceIds)
	}
	// RW19: only a td whose record still says created == false is a
	// candidate.
	if len(plan.tdRefs) != 1 {
		t.Fatalf("td candidates = %v, want only the uncreated one",
			plan.tdRefs)
	}
	if ref := plan.tdRefs[spTdOpen]; ref.Name != "td0" ||
		ref.TdId != spTdOpen {
		t.Fatalf("td candidate = %+v", ref)
	}
}

// TestSpCntlrPlanCarriesSettling checks that buildPlan copies each cntlr
// record's settling flag into its plan (HL2): the child seeds its settle memo
// from it.
func TestSpCntlrPlanCarriesSettling(t *testing.T) {
	captureLogs(t)
	state := spFixture()
	state.Cntlrs[spCntlrPrimary].Settling = true
	plan := spTestWorker(nil).buildPlan(context.Background(), state)
	for cntlrId, want := range map[uint64]bool{
		spCntlrPrimary:  true,
		spCntlrStandby:  false,
		spCntlrDisabled: false,
	} {
		if got := plan.cntlrs[cntlrId].settling; got != want {
			t.Fatalf("cntlr %d plan settling = %v, want %v",
				cntlrId, got, want)
		}
	}
}

// settleDriver builds one cntlr child's driver outside any loop, the way the
// coordinator's startCntlrChild does, over a recording health writer: plan
// (settlePlan's shape, driving revision 7) and a clean stored CntlrInfo. Its
// report channel is spTestWorker's buffered one, since send would block
// forever on a nil channel.
func settleDriver(
	t *testing.T,
	plan *cntlrPlan,
) (*cntlrDriver, *fakeHealthWriter, *logCapture) {
	t.Helper()
	logs := captureLogs(t)
	d := newTestDeps(
		testConfig(common.WorkerRoleSp), newFakeStore(), newFakeClock(),
	)
	hw := &fakeHealthWriter{}
	d.health = hw
	driver := newCntlrDriver(spTestWorker(d), nil, plan)
	driver.storeInfo(&pb.CntlrInfo{
		GrpIdToMdRaid: map[uint64]*pb.ResInfo{1: resOk("md")},
	})
	return driver, hw, logs
}

// settlePlan is the plan settleDriver drives: revision 7, the fixture's
// primary cntlr.
func settlePlan(primary bool, settling bool) *cntlrPlan {
	return &cntlrPlan{
		addr:     spCnA,
		cnId:     spCnIdA,
		cntlrId:  spCntlrPrimary,
		primary:  primary,
		settling: settling,
		req: &pb.SyncupCntlrRequest{
			ClusterId: testCid,
			CnId:      spCnIdA,
			CntlrPointer: &pb.CntlrPointer{
				SpId:    testSpId,
				CntlrId: spCntlrPrimary,
			},
			Revision: 7,
		},
	}
}

// settleWrites returns the recorded writes that asked for a settle.
func settleWrites(hw *fakeHealthWriter) []healthWrite {
	var out []healthWrite
	for _, write := range hw.all() {
		if write.settle {
			out = append(out, write)
		}
	}
	return out
}

// TestSpCntlrSettle pins the driver's settle gate (HL2): only an accepted,
// clean reply of an ENABLED PRIMARY at the revision the child drives settles
// it, and that write is what the §12 `cntlr settled` record reports. The
// revision half is load-bearing: a clean reply at the previous revision
// describes the standby shape the promotion's failed SyncupCntlr left behind.
// So is the disabled half: a disabled primary converges the standby shape
// (cnagent.md CN9). So is the built half (primaryShapeBuilt): a clean reply
// whose pools are still PROVISIONING, or probed MISSING after a failed
// build, describes a build still to come. The
// memo follows every plan the child takes, both ways: a plan that says
// settling re-arms it, one that does not clears it.
func TestSpCntlrSettle(t *testing.T) {
	ctx := context.Background()

	t.Run("primary", func(t *testing.T) {
		driver, hw, logs := settleDriver(t, settlePlan(true, true))
		// The previous revision: clean, but the standby shape.
		driver.observe(ctx, &replyState{revision: 6, infoPresent: true})
		if got := settleWrites(hw); len(got) != 0 {
			t.Fatalf("settled on a revision-6 reply: %+v", got)
		}
		// The driven revision, but rejected: no verdict at all.
		driver.observe(ctx, &replyState{
			revision: 7, code: common.ReplyCodeStaleRevision,
		})
		if got := settleWrites(hw); len(got) != 0 {
			t.Fatalf("settled on a rejected reply: %+v", got)
		}
		// Accepted, clean, at the driven revision.
		driver.observe(ctx, &replyState{revision: 7, infoPresent: true})
		got := settleWrites(hw)
		want := healthWrite{
			record: healthRecordCntlr, cid: testCid, spId: testSpId,
			objId: spCntlrPrimary, epoch: 0, settle: true,
		}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("settle writes = %+v, want [%+v]", got, want)
		}
		recs := logs.withMsg(msgCntlrSettled)
		if len(recs) != 1 {
			t.Fatalf("%d cntlr settled records, want 1", len(recs))
		}
		rec := recs[0]
		if rec["role"] != common.WorkerRoleSp {
			t.Fatalf("role = %v", rec["role"])
		}
		for attr, want := range map[string]uint64{
			"cluster_id": testCid,
			"sp_id":      testSpId,
			"cn_id":      spCnIdA,
			"revision":   7,
		} {
			if got, _ := rec[attr].(float64); uint64(got) != want {
				t.Fatalf("%s = %v, want %d", attr, rec[attr], want)
			}
		}
		ptr, _ := rec["cntlr_pointer"].(map[string]any)
		if ptr == nil {
			t.Fatalf("cntlr_pointer = %v", rec["cntlr_pointer"])
		}
		// The memo is cleared: another clean round writes nothing.
		driver.observe(ctx, &replyState{revision: 7, infoPresent: true})
		if got := len(hw.all()); got != 2 {
			t.Fatalf("writes = %+v, want the recovery and the settle only",
				hw.all())
		}
		// Taking a plan re-seeds the memo from its record: one loaded
		// before the settle write landed costs one redundant settle.
		driver.install(settlePlan(true, true))
		driver.observe(ctx, &replyState{revision: 7, infoPresent: true})
		if got := len(settleWrites(hw)); got != 2 {
			t.Fatalf("%d settle writes after a stale plan, want 2", got)
		}
	})

	t.Run("stack not built yet", func(t *testing.T) {
		// A new SP's primary reports its pools PROVISIONING until its sides
		// are zeroed: clean, at the driven revision, and no proof of the
		// build still to come (primaryShapeBuilt). Leg rows do not count: a
		// spare that is still zeroing reads PROVISIONING there. Nor do group
		// rows: a grow's group reads PROVISIONING beside a serving pool.
		driver, hw, logs := settleDriver(t, settlePlan(true, true))
		provisioning := &pb.ResInfo{
			ResName: "md", Status: pb.ResStatus_RES_STATUS_PROVISIONING,
		}
		driver.storeInfo(&pb.CntlrInfo{
			GrpIdToMdRaid:   map[uint64]*pb.ResInfo{1: provisioning},
			SliceIdToDmPool: map[uint64]*pb.ResInfo{1: provisioning},
		})
		driver.observe(ctx, &replyState{revision: 7, infoPresent: true})
		all := hw.all()
		if len(all) != 1 || all[0].epoch != 0 || all[0].settle {
			t.Fatalf("writes on an all-PROVISIONING reply = %+v, want the "+
				"recovery alone", all)
		}
		if got := len(logs.withMsg(msgCntlrSettled)); got != 0 {
			t.Fatalf("%d cntlr settled records before the build", got)
		}
		// A build that fails partway: one group ERROR while its slice's pool
		// is still PROVISIONING. HL2 judges the ERROR row all the same.
		driver.storeInfo(&pb.CntlrInfo{
			GrpIdToMdRaid:   map[uint64]*pb.ResInfo{1: resErr("md", "boom")},
			SliceIdToDmPool: map[uint64]*pb.ResInfo{1: provisioning},
		})
		driver.observe(ctx, &replyState{revision: 7, infoPresent: true})
		all = hw.all()
		if len(all) != 2 || all[1].epoch == 0 || all[1].settle {
			t.Fatalf("writes on a failed build = %+v", all)
		}
		if got := len(logs.withMsg(msgCntlrSettled)); got != 0 {
			t.Fatalf("%d cntlr settled records on a failed build", got)
		}
		// Built, with a grow's group and a spare's leg row still
		// PROVISIONING: settles, with the recovery in the same write.
		driver.storeInfo(&pb.CntlrInfo{
			GrpIdToMdRaid: map[uint64]*pb.ResInfo{
				1: resOk("md"),
				2: {
					ResName: "grown",
					Status:  pb.ResStatus_RES_STATUS_PROVISIONING,
				},
			},
			SliceIdToDmPool: map[uint64]*pb.ResInfo{1: resOk("pool")},
			LegIdToLeg: map[uint64]*pb.ResInfo{9: {
				ResName: "spare",
				Status:  pb.ResStatus_RES_STATUS_PROVISIONING,
			}},
		})
		driver.observe(ctx, &replyState{revision: 7, infoPresent: true})
		got := settleWrites(hw)
		if len(got) != 1 || got[0].epoch != 0 {
			t.Fatalf("settle writes on the built reply = %+v, want one", got)
		}
		if got := len(logs.withMsg(msgCntlrSettled)); got != 1 {
			t.Fatalf("%d cntlr settled records on the built reply", got)
		}
	})

	t.Run("stack probed missing after a failed build", func(t *testing.T) {
		// A td-less primary whose converge at the driven revision found its
		// members not available: the SyncupCntlr reply reads its groups and
		// pool ERROR, and the Check round before the agent's retry probes
		// the devices absent and reads them MISSING "" — clean, at the
		// driven revision, and no proof of the build the retry still has to
		// do (primaryShapeBuilt). A row the sp_level suppresses reads
		// MISSING "sp_level" and holds nothing.
		driver, hw, logs := settleDriver(t, settlePlan(true, true))
		driver.storeInfo(&pb.CntlrInfo{
			GrpIdToMdRaid: map[uint64]*pb.ResInfo{
				1: resErr("md", "no available leg"),
			},
			SliceIdToMeta:   map[uint64]*pb.ResInfo{1: resErr("meta", "x")},
			SliceIdToData:   map[uint64]*pb.ResInfo{1: resErr("data", "x")},
			SliceIdToDmPool: map[uint64]*pb.ResInfo{1: resErr("pool", "x")},
			SsIdToSubsystem: map[uint64]*pb.ResInfo{1: resOk("ss")},
		})
		driver.observe(ctx, &replyState{revision: 7, infoPresent: true})
		missing := func(name string, details string) *pb.ResInfo {
			return &pb.ResInfo{
				ResName: name,
				Status:  pb.ResStatus_RES_STATUS_MISSING,
				Details: details,
			}
		}
		driver.storeInfo(&pb.CntlrInfo{
			GrpIdToMdRaid:   map[uint64]*pb.ResInfo{1: missing("md", "")},
			SliceIdToMeta:   map[uint64]*pb.ResInfo{1: missing("meta", "")},
			SliceIdToData:   map[uint64]*pb.ResInfo{1: missing("data", "")},
			SliceIdToDmPool: map[uint64]*pb.ResInfo{1: missing("pool", "")},
			SsIdToSubsystem: map[uint64]*pb.ResInfo{1: resOk("ss")},
		})
		driver.observe(ctx, &replyState{revision: 7, infoPresent: true})
		all := hw.all()
		if len(all) != 2 || all[0].epoch == 0 || all[1].epoch != 0 ||
			all[0].settle || all[1].settle {
			t.Fatalf("writes on a failed build and its MISSING probe = %+v, "+
				"want the error and the recovery alone", all)
		}
		if got := len(logs.withMsg(msgCntlrSettled)); got != 0 {
			t.Fatalf("%d cntlr settled records on the MISSING probe", got)
		}
		// The level raised to suppress the md and the pools: settles.
		driver.storeInfo(&pb.CntlrInfo{
			GrpIdToMdRaid: map[uint64]*pb.ResInfo{
				1: missing("md", "sp_level"),
			},
			SliceIdToMeta: map[uint64]*pb.ResInfo{
				1: missing("meta", "sp_level"),
			},
			SliceIdToData: map[uint64]*pb.ResInfo{
				1: missing("data", "sp_level"),
			},
			SliceIdToDmPool: map[uint64]*pb.ResInfo{
				1: missing("pool", "sp_level"),
			},
			SsIdToSubsystem: map[uint64]*pb.ResInfo{1: resOk("ss")},
		})
		driver.observe(ctx, &replyState{revision: 7, infoPresent: true})
		if got := settleWrites(hw); len(got) != 1 || got[0].epoch != 0 {
			t.Fatalf("settle writes on the suppressed reply = %+v, want one",
				got)
		}
		if got := len(logs.withMsg(msgCntlrSettled)); got != 1 {
			t.Fatalf("%d cntlr settled records on the suppressed reply", got)
		}
	})

	t.Run("plan loaded after the settle", func(t *testing.T) {
		// The memo is seeded true, then a plan built after another writer
		// settled the record arrives: taking it clears the memo, so a clean
		// primary-shape reply writes no settle.
		driver, hw, logs := settleDriver(t, settlePlan(true, true))
		driver.install(settlePlan(true, false))
		driver.observe(ctx, &replyState{revision: 7, infoPresent: true})
		if got := settleWrites(hw); len(got) != 0 {
			t.Fatalf("settled after a settled plan: %+v", got)
		}
		if got := len(logs.withMsg(msgCntlrSettled)); got != 0 {
			t.Fatalf("%d cntlr settled records after a settled plan", got)
		}
	})

	t.Run("standby", func(t *testing.T) {
		driver, hw, logs := settleDriver(t, settlePlan(false, true))
		driver.observe(ctx, &replyState{revision: 7, infoPresent: true})
		if got := settleWrites(hw); len(got) != 0 {
			t.Fatalf("a standby settled: %+v", got)
		}
		if got := len(logs.withMsg(msgCntlrSettled)); got != 0 {
			t.Fatalf("%d cntlr settled records for a standby", got)
		}
	})

	t.Run("disabled primary", func(t *testing.T) {
		// The record says primary, but the agent converges the standby
		// shape for a disabled cntlr: its clean reply at the driven
		// revision proves nothing about the role.
		plan := settlePlan(true, true)
		plan.req.Cntlr = &pb.Cntlr{
			Primary: true, Disabled: true, Settling: true,
		}
		driver, hw, logs := settleDriver(t, plan)
		driver.observe(ctx, &replyState{revision: 7, infoPresent: true})
		if got := settleWrites(hw); len(got) != 0 {
			t.Fatalf("a disabled primary settled: %+v", got)
		}
		if got := len(logs.withMsg(msgCntlrSettled)); got != 0 {
			t.Fatalf("%d cntlr settled records for a disabled primary", got)
		}
	})
}

// TestSpRequestsAreIndependentCopies checks that no two children share a
// sub-message of the loaded state: each marshals its request on its own
// goroutine.
func TestSpRequestsAreIndependentCopies(t *testing.T) {
	captureLogs(t)
	state := spFixture()
	w := spTestWorker(nil)
	plan := w.buildPlan(context.Background(), state)
	a := plan.cntlrs[spCntlrPrimary].req
	b := plan.cntlrs[spCntlrStandby].req
	if a.GetTdList()[0] == b.GetTdList()[0] {
		t.Fatalf("two cntlr requests share a ThinDevice message")
	}
	if a.GetBdevConf() == state.Conf.GetBdevConf() {
		t.Fatalf("a request aliases the loaded state's BdevConf")
	}
}

// TestSpUnresolvedEndpointLeavesChildIdle checks RW14: a SIDE endpoint without
// a DnConf yields no dn_id, so no request is built for that child; the
// coordinator counts it and re-resolves on its ticker. RW14 confines the
// effect to that child — every other side and every cntlr is still driven.
func TestSpUnresolvedEndpointLeavesChildIdle(t *testing.T) {
	logs := captureLogs(t)
	state := spFixture()
	delete(state.DnByAddr, spDnD)
	w := spTestWorker(nil)
	plan := w.buildPlan(context.Background(), state)

	if _, ok := plan.sides[sideKey{
		legId: spLegSpare, sideId: spSideSpare,
	}]; ok {
		t.Fatalf("a side with no DnConf was given a request")
	}
	if len(plan.sides) != 4 {
		t.Fatalf("%d side children, want the other four", len(plan.sides))
	}
	if len(plan.cntlrs) != 3 {
		t.Fatalf("%d cntlr children, want all three", len(plan.cntlrs))
	}
	if plan.unresolved != 1 {
		t.Fatalf("unresolved = %d, want 1", plan.unresolved)
	}
	if got := len(logs.withMsg(msgSpChildUnresolved)); got != 1 {
		t.Fatalf("%d unresolved records, want 1", got)
	}
}

// TestSpUnresolvedCntlrLeavesEverySideIdle checks RW15 against RW14: a
// side_conf's primary_cn_id and standby_id_list name EVERY cntlr of the SP, so
// a cntlr the snapshot cannot resolve may not simply drop out of the set.
//
// It is not a cosmetic truncation. The dn agent computes the SP's CN set as
// {primary_cn_id} ∪ standby_id_list and tears down the dm-error, dm-linear,
// subsystem and namespace of every CN it is not told about: an unresolved
// PRIMARY would ship primary_cn_id = 0 and take the real primary's whole data
// path down on every DN of the SP, an unresolved STANDBY would cost that CN
// its path and the SP a failover target. So every SIDE child stays idle until
// the cntlr set resolves, exactly as addMigrConf leaves a side idle rather
// than sending half a migration role.
func TestSpUnresolvedCntlrLeavesEverySideIdle(t *testing.T) {
	for _, tc := range []struct {
		name    string
		unhinge func(state *model.SpState)
		cntlrs  int
	}{
		{
			name:    "primary CnConf absent",
			unhinge: func(s *model.SpState) { delete(s.CnByAddr, spCnA) },
			cntlrs:  2,
		},
		{
			name:    "standby CnConf absent",
			unhinge: func(s *model.SpState) { delete(s.CnByAddr, spCnB) },
			cntlrs:  2,
		},
		{
			name:    "cntlr record missing (MD3)",
			unhinge: func(s *model.SpState) { delete(s.Cntlrs, spCntlrPrimary) },
			cntlrs:  2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			state := spFixture()
			tc.unhinge(state)
			w := spTestWorker(nil)
			plan := w.buildPlan(context.Background(), state)

			for key, side := range plan.sides {
				t.Fatalf(
					"side %+v was shipped with primary_cn_id %d, standby %v",
					key, side.req.GetSideConf().GetPrimaryCnId(),
					side.req.GetSideConf().GetStandbyIdList(),
				)
			}
			if len(plan.cntlrs) != tc.cntlrs {
				t.Fatalf("%d cntlr children, want %d: RW14 confines an "+
					"unresolved endpoint to its OWN child",
					len(plan.cntlrs), tc.cntlrs)
			}
			if plan.unresolved == 0 {
				t.Fatalf("nothing counted unresolved: the cntlr_interval " +
					"ticker would never re-resolve")
			}
			if got := len(logs.withMsg(msgSpSidesIdle)); got != 1 {
				t.Fatalf("%d sides-idle records, want 1", got)
			}
			// The leg -> slice index survives the idle window: the primary
			// cntlr child keeps reporting leg rows and only the coordinator
			// knows which slice they belong on (HL2).
			if plan.legSlice[spLegMeta] != spSliceA {
				t.Fatalf("legSlice = %v, want the legs still indexed",
					plan.legSlice)
			}
		})
	}
}

// TestSpMigrationPeerUnresolvedSkipsSide checks RW15: a migration whose peer
// side's DN cannot be resolved leaves the child idle rather than sending half
// a migration role.
func TestSpMigrationPeerUnresolvedSkipsSide(t *testing.T) {
	captureLogs(t)
	state := spFixture()
	delete(state.DnByAddr, spDnC)
	w := spTestWorker(nil)
	plan := w.buildPlan(context.Background(), state)
	if _, ok := plan.sides[sideKey{
		legId: spLegMigr, sideId: spSideSrc,
	}]; ok {
		t.Fatalf("the source side was driven without the destination's dn_id")
	}
	if _, ok := plan.sides[sideKey{
		legId: spLegMeta, sideId: spSideMeta,
	}]; !ok {
		t.Fatalf("an unrelated side was dropped")
	}
}

// ---------------------------------------------------------------------------
// RW19 candidate selection
// ---------------------------------------------------------------------------

// TestSpCompletedTds walks architecture.md §10.3's materialization flip: the
// four conditions of a complete td and every negative it lists.
func TestSpCompletedTds(t *testing.T) {
	sliceIds := []uint64{spSliceA, spSliceB}
	thin := func(rows map[uint64]*pb.ResInfo) *pb.CntlrInfo_ThinInfo {
		return &pb.CntlrInfo_ThinInfo{SliceIdToDmThin: rows}
	}
	cases := []struct {
		name string
		info *pb.CntlrInfo
		want []uint64
	}{
		{
			name: "complete",
			info: &pb.CntlrInfo{
				TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
					spTdOpen: thin(map[uint64]*pb.ResInfo{
						spSliceA: resOk("thin-a"),
						spSliceB: resOk("thin-b"),
					}),
				},
			},
			want: []uint64{spTdOpen},
		},
		{
			name: "no entry at all (a standby, CN14)",
			info: &pb.CntlrInfo{},
			want: nil,
		},
		{
			name: "partial map",
			info: &pb.CntlrInfo{
				TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
					spTdOpen: thin(map[uint64]*pb.ResInfo{
						spSliceA: resOk("thin-a"),
					}),
				},
			},
			want: nil,
		},
		{
			name: "an extra slice",
			info: &pb.CntlrInfo{
				TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
					spTdOpen: thin(map[uint64]*pb.ResInfo{
						spSliceA: resOk("thin-a"),
						spSliceB: resOk("thin-b"),
						99:       resOk("thin-x"),
					}),
				},
			},
			want: nil,
		},
		{
			name: "a PROVISIONING row",
			info: &pb.CntlrInfo{
				TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
					spTdOpen: thin(map[uint64]*pb.ResInfo{
						spSliceA: resOk("thin-a"),
						spSliceB: resStatus(
							"thin-b", pb.ResStatus_RES_STATUS_PROVISIONING,
						),
					}),
				},
			},
			want: nil,
		},
		{
			name: "an ERROR row",
			info: &pb.CntlrInfo{
				TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
					spTdOpen: thin(map[uint64]*pb.ResInfo{
						spSliceA: resOk("thin-a"),
						spSliceB: resErr("thin-b", "boom"),
					}),
				},
			},
			want: nil,
		},
		{
			name: "a MISSING row",
			info: &pb.CntlrInfo{
				TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
					spTdOpen: thin(map[uint64]*pb.ResInfo{
						spSliceA: resOk("thin-a"),
						spSliceB: resStatus(
							"thin-b", pb.ResStatus_RES_STATUS_MISSING,
						),
					}),
				},
			},
			want: nil,
		},
		{
			name: "two tds, one complete",
			info: &pb.CntlrInfo{
				TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
					spTdOpen: thin(map[uint64]*pb.ResInfo{
						spSliceA: resOk("thin-a"),
						spSliceB: resOk("thin-b"),
					}),
					spTdDone: thin(map[uint64]*pb.ResInfo{
						spSliceA: resOk("thin-a"),
					}),
				},
			},
			want: []uint64{spTdOpen},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := completedTds(tc.info, sliceIds)
			if len(got) != len(tc.want) {
				t.Fatalf("completed = %v, want %v", got, tc.want)
			}
			for i, tdId := range tc.want {
				if got[i] != tdId {
					t.Fatalf("completed = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestSpCreatedFlipOnlyForUncreatedTds checks RW19: the coordinator keeps only
// the candidates its LOADED state still shows created == false, and a report
// that completes none causes no etcd traffic at all.
func TestSpCreatedFlipOnlyForUncreatedTds(t *testing.T) {
	h := newSpHarness(t)
	w := spTestWorker(h.deps)
	w.ops = h.ops
	w.tdRefs = map[uint64]model.TdRef{
		spTdOpen: {Name: "td0", TdId: spTdOpen},
	}
	w.handleReports(spReport{
		createdTdIds: []uint64{spTdOpen, spTdDone},
	})
	calls := h.ops.createdCalls()
	if len(calls) != 1 || len(calls[0]) != 1 ||
		calls[0][0].TdId != spTdOpen {
		t.Fatalf("FlipCreated calls = %v", calls)
	}

	// A td the state already shows created: no STM at all.
	w.handleReports(spReport{createdTdIds: []uint64{spTdDone}})
	if got := len(h.ops.createdCalls()); got != 1 {
		t.Fatalf("%d FlipCreated calls, want no traffic for a done td", got)
	}
}

// TestSpFlipBatchesOneStm checks RW18: several sides reported within one drain
// of the report channel share one FlipProvisioned STM, and duplicates are
// dropped.
func TestSpFlipBatchesOneStm(t *testing.T) {
	h := newSpHarness(t)
	logs := h.logs
	h.store.seed(t, model.SpRevKey(testShard, testCid, testSpId),
		&pb.SpRev{SpName: testSpName, Revision: testSpRev + 1})
	w := spTestWorker(h.deps)
	w.ops = h.ops
	first := model.SideRef{
		SliceId: spSliceA, LegId: spLegMigr, SideId: spSideDst,
	}
	second := model.SideRef{
		SliceId: spSliceA, LegId: spLegSpare, SideId: spSideSpare,
	}
	// Two more reports are already queued when the first is handled.
	w.reportCh <- spReport{provisioned: &second}
	w.reportCh <- spReport{provisioned: &first}
	w.handleReports(spReport{provisioned: &first})

	calls := h.ops.provisionedCalls()
	if len(calls) != 1 {
		t.Fatalf("%d FlipProvisioned calls, want one batched STM", len(calls))
	}
	if len(calls[0]) != 2 {
		t.Fatalf("batch = %v, want the two distinct sides", calls[0])
	}
	records := logs.withMsg(msgFlipApplied)
	if len(records) != 2 {
		t.Fatalf("%d flip applied records, want one per side", len(records))
	}
	if records[0]["kind"] != flipKindProvisioned {
		t.Fatalf("kind = %v", records[0]["kind"])
	}
	if rev, _ := records[0]["revision"].(float64); uint64(rev) != testSpRev+1 {
		t.Fatalf("revision = %v, want the new SpRev", records[0]["revision"])
	}
}

// TestSpFlipRecordsOnlyAppliedRefs pins §12's "flip applied" record against
// RW18/RW19: one record per side / td the STM ACTUALLY wrote.
//
// Both ops skip candidates individually — a side another owner flipped first,
// a td whose key is gone, whose td_id differs or that is already created — so
// a record per REPORTED candidate would claim flips this worker never
// performed and pin them to a revision it did not cause. §14.11 case F step 2
// counts these records across all worker logs to prove a handoff produces no
// second flip, which only holds if the count is the count of writes.
func TestSpFlipRecordsOnlyAppliedRefs(t *testing.T) {
	h := newSpHarness(t)
	h.store.seed(t, model.SpRevKey(testShard, testCid, testSpId),
		&pb.SpRev{SpName: testSpName, Revision: testSpRev + 1})
	w := spTestWorker(h.deps)
	w.ops = h.ops
	applied := model.SideRef{
		SliceId: spSliceA, LegId: spLegMigr, SideId: spSideDst,
	}
	skipped := model.SideRef{
		SliceId: spSliceA, LegId: spLegSpare, SideId: spSideSpare,
	}
	// The other owner of the overlap got to the spare's side first, so the
	// STM writes one of the two.
	h.ops.applySides = map[model.SideRef]bool{applied: true}
	w.reportCh <- spReport{provisioned: &skipped}
	w.handleReports(spReport{provisioned: &applied})

	if got := len(h.ops.provisionedCalls()); got != 1 {
		t.Fatalf("%d FlipProvisioned calls, want one batched STM", got)
	}
	records := h.logs.withMsg(msgFlipApplied)
	if len(records) != 1 {
		t.Fatalf("%d flip applied records, want one per WRITTEN side: %v",
			len(records), records)
	}
	if id, _ := records[0]["side_id"].(float64); uint64(id) != spSideDst {
		t.Fatalf("side_id = %v, want the side the STM wrote",
			records[0]["side_id"])
	}

	// The created half of the same rule (RW19).
	done := model.TdRef{Name: "td0", TdId: spTdOpen}
	gone := model.TdRef{Name: "td1", TdId: spTdDone}
	w.tdRefs = map[uint64]model.TdRef{spTdOpen: done, spTdDone: gone}
	h.ops.applyTds = map[model.TdRef]bool{done: true}
	w.handleReports(spReport{createdTdIds: []uint64{spTdOpen, spTdDone}})

	created := 0
	for _, record := range h.logs.withMsg(msgFlipApplied) {
		if record["kind"] == flipKindCreated {
			created++
			if name, _ := record["td_name"].(string); name != "td0" {
				t.Fatalf("td_name = %v, want the td the STM wrote",
					record["td_name"])
			}
		}
	}
	if created != 1 {
		t.Fatalf("%d created records, want one per WRITTEN td", created)
	}
}

// TestSpMissingSliceKeepsCreatedComparisonSet pins RW19 condition (3): the
// key set of a reply's slice_id_to_dm_thin is compared with the SP's slice ids
// — SpConf.slice_id_list — and NOT with the subset whose Slice records
// happened to load (MD3). Shrinking the set would flip a td `created` while a
// slice of the SP is unaccounted for.
func TestSpMissingSliceKeepsCreatedComparisonSet(t *testing.T) {
	captureLogs(t)
	state := spFixture()
	delete(state.Slices, spSliceB)
	w := spTestWorker(nil)
	plan := w.buildPlan(context.Background(), state)

	primary := plan.cntlrs[spCntlrPrimary]
	if len(primary.sliceIds) != 2 ||
		primary.sliceIds[0] != spSliceA || primary.sliceIds[1] != spSliceB {
		t.Fatalf("slice ids = %v, want SpConf.slice_id_list",
			primary.sliceIds)
	}
	// The absent slice is absent from the request too, so the cn agent never
	// builds its dm-thin (RW16).
	if len(primary.req.GetIdToSlice()) != 1 {
		t.Fatalf("id_to_slice = %v, want only the slice that loaded",
			primary.req.GetIdToSlice())
	}
	info := &pb.CntlrInfo{
		TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
			spTdOpen: {SliceIdToDmThin: map[uint64]*pb.ResInfo{
				spSliceA: resOk("thin-a"),
			}},
		},
	}
	if got := completedTds(info, primary.sliceIds); len(got) != 0 {
		t.Fatalf("completed = %v, want none while a slice is unaccounted for",
			got)
	}
}

// ---------------------------------------------------------------------------
// A live coordinator (RW14, RW18, HL2)
// ---------------------------------------------------------------------------

// stubSideAgent is a DiskNodeAgent serving the side half of the dn service.
type stubSideAgent struct {
	pb.UnimplementedDiskNodeAgentServer

	mu         sync.Mutex
	syncupReqs []*pb.SyncupSideRequest
	checkReqs  []*pb.CheckSideRequest
	pushReqs   []*pb.PushMigrBitmapRequest

	syncupReply func(req *pb.SyncupSideRequest) *pb.SyncupSideReply
	checkReply  func(req *pb.CheckSideRequest) *pb.CheckSideReply
	pushCode    uint32
}

func (s *stubSideAgent) SyncupSide(
	ctx context.Context, req *pb.SyncupSideRequest,
) (*pb.SyncupSideReply, error) {
	s.mu.Lock()
	s.syncupReqs = append(s.syncupReqs, req)
	build := s.syncupReply
	s.mu.Unlock()
	if build == nil {
		// The agent applied it: the revision matches from now on.
		return &pb.SyncupSideReply{Revision: req.GetRevision()}, nil
	}
	return build(req), nil
}

func (s *stubSideAgent) CheckSide(
	stream grpc.BidiStreamingServer[pb.CheckSideRequest, pb.CheckSideReply],
) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.checkReqs = append(s.checkReqs, req)
		build := s.checkReply
		s.mu.Unlock()
		if build == nil {
			// A revision the worker does not expect forces the RW4 step 5
			// syncup, which is what a fresh agent does too.
			if err := stream.Send(&pb.CheckSideReply{}); err != nil {
				return err
			}
			continue
		}
		if reply := build(req); reply != nil {
			if err := stream.Send(reply); err != nil {
				return err
			}
		}
	}
}

func (s *stubSideAgent) PushMigrBitmap(
	ctx context.Context, req *pb.PushMigrBitmapRequest,
) (*pb.PushMigrBitmapReply, error) {
	s.mu.Lock()
	s.pushReqs = append(s.pushReqs, req)
	code := s.pushCode
	s.mu.Unlock()
	return &pb.PushMigrBitmapReply{
		AgentReply: &pb.AgentReply{Code: code},
	}, nil
}

func (s *stubSideAgent) syncups() []*pb.SyncupSideRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.SyncupSideRequest(nil), s.syncupReqs...)
}

func (s *stubSideAgent) pushes() []*pb.PushMigrBitmapRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.PushMigrBitmapRequest(nil), s.pushReqs...)
}

// stubCntlrAgent is a ControllerNodeAgent serving the cntlr half of the cn
// service.
type stubCntlrAgent struct {
	pb.UnimplementedControllerNodeAgentServer

	mu         sync.Mutex
	syncupReqs []*pb.SyncupCntlrRequest
	pushReqs   []*pb.PushCloneBitmapRequest

	syncupReply func(req *pb.SyncupCntlrRequest) *pb.SyncupCntlrReply
	checkReply  func(req *pb.CheckCntlrRequest) *pb.CheckCntlrReply
	// pushCode is the AgentReply code every PushCloneBitmap answers with, the
	// cn twin of stubSideAgent.pushCode.
	pushCode uint32
}

func (s *stubCntlrAgent) SyncupCntlr(
	ctx context.Context, req *pb.SyncupCntlrRequest,
) (*pb.SyncupCntlrReply, error) {
	s.mu.Lock()
	s.syncupReqs = append(s.syncupReqs, req)
	build := s.syncupReply
	s.mu.Unlock()
	if build == nil {
		return &pb.SyncupCntlrReply{Revision: req.GetRevision()}, nil
	}
	return build(req), nil
}

func (s *stubCntlrAgent) CheckCntlr(
	stream grpc.BidiStreamingServer[pb.CheckCntlrRequest, pb.CheckCntlrReply],
) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		s.mu.Lock()
		build := s.checkReply
		s.mu.Unlock()
		if build == nil {
			if err := stream.Send(&pb.CheckCntlrReply{}); err != nil {
				return err
			}
			continue
		}
		if reply := build(req); reply != nil {
			if err := stream.Send(reply); err != nil {
				return err
			}
		}
	}
}

func (s *stubCntlrAgent) PushCloneBitmap(
	ctx context.Context, req *pb.PushCloneBitmapRequest,
) (*pb.PushCloneBitmapReply, error) {
	s.mu.Lock()
	s.pushReqs = append(s.pushReqs, req)
	code := s.pushCode
	s.mu.Unlock()
	return &pb.PushCloneBitmapReply{
		AgentReply: &pb.AgentReply{Code: code, Details: "no such clone"},
	}, nil
}

func (s *stubCntlrAgent) syncups() []*pb.SyncupCntlrRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.SyncupCntlrRequest(nil), s.syncupReqs...)
}

func (s *stubCntlrAgent) pushes() []*pb.PushCloneBitmapRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.PushCloneBitmapRequest(nil), s.pushReqs...)
}

// fakeSpOps is the §13 stand-in for model: a fixture SpState and a record of
// every flip the coordinator ran.
type fakeSpOps struct {
	mu    sync.Mutex
	state *model.SpState
	err   error
	// loads counts loadSp calls — the fan-out's and the reaction pass's —
	// so a test can tell that ticks have run.
	loads       int
	provisioned [][]model.SideRef
	created     [][]model.TdRef
	// applySides / applyTds decide what the STM reports back as WRITTEN. nil
	// means "everything the caller listed"; a set restricts it, which is how
	// the tests reproduce a candidate another owner flipped first.
	applySides map[model.SideRef]bool
	applyTds   map[model.TdRef]bool
}

func (o *fakeSpOps) setState(state *model.SpState) {
	o.mu.Lock()
	o.state = state
	o.mu.Unlock()
}

func (o *fakeSpOps) setErr(err error) {
	o.mu.Lock()
	o.err = err
	o.mu.Unlock()
}

func (o *fakeSpOps) loadSp(
	ctx context.Context, cid uint64, spName string,
) (*model.SpState, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.loads++
	if o.err != nil {
		return nil, o.err
	}
	return o.state, nil
}

func (o *fakeSpOps) loadCnt() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.loads
}

func (o *fakeSpOps) flipProvisioned(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	sides []model.SideRef,
) ([]model.SideRef, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.provisioned = append(o.provisioned, sides)
	if o.applySides == nil {
		return sides, nil
	}
	var applied []model.SideRef
	for _, ref := range sides {
		if o.applySides[ref] {
			applied = append(applied, ref)
		}
	}
	return applied, nil
}

func (o *fakeSpOps) flipCreated(
	ctx context.Context,
	cid uint64,
	shard uint32,
	spId uint64,
	cands []model.TdRef,
) ([]model.TdRef, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.created = append(o.created, cands)
	if o.applyTds == nil {
		return cands, nil
	}
	var applied []model.TdRef
	for _, ref := range cands {
		if o.applyTds[ref] {
			applied = append(applied, ref)
		}
	}
	return applied, nil
}

func (o *fakeSpOps) provisionedCalls() [][]model.SideRef {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([][]model.SideRef(nil), o.provisioned...)
}

func (o *fakeSpOps) createdCalls() [][]model.TdRef {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([][]model.TdRef(nil), o.created...)
}

// spHarness runs a real sp coordinator against bufconn agents.
type spHarness struct {
	t      *testing.T
	logs   *logCapture
	clk    *fakeClock
	store  *fakeStore
	fleet  *agentFleet
	deps   *deps
	hw     *fakeHealthWriter
	ops    *fakeSpOps
	sides  map[string]*stubSideAgent
	cntlrs map[string]*stubCntlrAgent
}

func newSpHarness(t *testing.T) *spHarness {
	t.Helper()
	logs := captureLogs(t)
	clk := newFakeClock()
	store := newFakeStore()
	fleet := newAgentFleet(t)
	d := newTestDeps(testConfig(common.WorkerRoleSp), store, clk)
	hw := &fakeHealthWriter{}
	d.health = hw
	d.conns.dial = fleet.dial
	h := &spHarness{
		t: t, logs: logs, clk: clk, store: store, fleet: fleet,
		deps: d, hw: hw, ops: &fakeSpOps{state: spFixture()},
		sides:  make(map[string]*stubSideAgent),
		cntlrs: make(map[string]*stubCntlrAgent),
	}
	// As stored: the cache resolves nothing (§7), and neither does this.
	setCachedConf(d, testCid, testClusterConf())
	return h
}

// addSide registers a DN agent serving the side RPCs at addrPort.
func (h *spHarness) addSide(addrPort string) *stubSideAgent {
	h.t.Helper()
	stub := &stubSideAgent{}
	h.fleet.serve(h.t, addrPort, func(server *grpc.Server) {
		pb.RegisterDiskNodeAgentServer(server, stub)
	})
	h.sides[addrPort] = stub
	return stub
}

// addCntlr registers a CN agent serving the cntlr RPCs at addrPort.
func (h *spHarness) addCntlr(addrPort string) *stubCntlrAgent {
	h.t.Helper()
	stub := &stubCntlrAgent{}
	h.fleet.serve(h.t, addrPort, func(server *grpc.Server) {
		pb.RegisterControllerNodeAgentServer(server, stub)
	})
	h.cntlrs[addrPort] = stub
	return stub
}

// addFixtureAgents registers every endpoint the fixture SP names.
func (h *spHarness) addFixtureAgents() {
	for _, addr := range []string{spDnA, spDnB, spDnC, spDnD} {
		h.addSide(addr)
	}
	for _, addr := range []string{spCnA, spCnB, spCnC} {
		h.addCntlr(addr)
	}
}

// start starts the coordinator on the fixture SP.
func (h *spHarness) start() *spWorker {
	h.t.Helper()
	w := startSpWorker(revWorkerParams{
		deps:    h.deps,
		role:    common.WorkerRoleSp,
		shard:   testShard,
		cid:     testCid,
		id:      testSpId,
		seed:    seedOf(3),
		desired: desiredState{revision: testSpRev, handle: testSpName},
	}, h.ops)
	h.t.Cleanup(w.stop)
	return w
}

// advanceUntil steps the fake clock by one round period at a time until cond
// holds (see revHarness.advanceUntil).
func (h *spHarness) advanceUntil(
	what string, step time.Duration, cond func() bool,
) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		h.clk.advance(step)
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

// TestSpFanOutStartsOneChildPerObject checks RW14: one child per side — spare
// legs included — and one per cntlr, each driven at its own endpoint with its
// own request.
func TestSpFanOutStartsOneChildPerObject(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	h.start()

	waitFor(t, "every side syncup", func() bool {
		for _, addr := range []string{spDnA, spDnB, spDnC, spDnD} {
			if len(h.sides[addr].syncups()) == 0 {
				return false
			}
		}
		return true
	})
	waitFor(t, "every cntlr syncup", func() bool {
		for _, addr := range []string{spCnA, spCnB, spCnC} {
			if len(h.cntlrs[addr].syncups()) == 0 {
				return false
			}
		}
		return true
	})
	// The DN that hosts two sides was told about both of them.
	seen := make(map[uint64]bool)
	for _, req := range h.sides[spDnA].syncups() {
		seen[req.GetSidePointer().GetSideId()] = true
	}
	if !seen[spSideMeta] || !seen[spSideB] {
		t.Fatalf("dn0 saw sides %v", seen)
	}
	// Every child logs its pointer (§12).
	started := h.logs.withMsg(msgRevisionWorkerStarted)
	pointers := 0
	for _, rec := range started {
		if _, ok := rec["side_pointer"]; ok {
			pointers++
		}
		if _, ok := rec["cntlr_pointer"]; ok {
			pointers++
		}
	}
	if pointers != 8 {
		t.Fatalf("%d child lifecycle records, want 5 sides + 3 cntlrs",
			pointers)
	}
}

// spRefusedFanOut starts a coordinator on an SP whose STORED bdev_conf is one
// CreateStoragePool could not have written, and returns once the §7 gate in
// front of RW14 has refused it and the refusal has been shown to reach
// nothing: no child started, no Syncup* sent to any endpoint.
//
// It has to go through the coordinator's real start path — buildPlan alone
// would happily build requests from the bad conf, because the check sits ahead
// of it.
func spRefusedFanOut(t *testing.T) (*spHarness, *spWorker) {
	t.Helper()
	h := newSpHarness(t)
	h.addFixtureAgents()
	// A stripe size of 0: legal protobuf, impossible from CreateStoragePool.
	bad := spFixture()
	bad.Conf.BdevConf.DmRaid0Conf.StripeSize = 0
	h.ops.setState(bad)
	w := h.start()

	waitFor(t, "refusal record", func() bool {
		return len(h.logs.withMsg(msgInvalidStoredConf)) == 1
	})
	rec := h.logs.withMsg(msgInvalidStoredConf)[0]
	if rec["level"] != "ERROR" {
		t.Fatalf("refusal logged at %v, want ERROR", rec["level"])
	}
	if err, _ := rec["error"].(string); !strings.Contains(err, "stripe_size") {
		t.Fatalf("error = %q, want the offending field named", err)
	}
	for _, addr := range []string{spDnA, spDnB, spDnC, spDnD} {
		if got := len(h.sides[addr].syncups()); got != 0 {
			t.Fatalf("%d side syncups to %s under a refused conf", got, addr)
		}
	}
	for _, addr := range []string{spCnA, spCnB, spCnC} {
		if got := len(h.cntlrs[addr].syncups()); got != 0 {
			t.Fatalf("%d cntlr syncups to %s under a refused conf", got, addr)
		}
	}
	for _, rec := range h.logs.withMsg(msgRevisionWorkerStarted) {
		if _, ok := rec["side_pointer"]; ok {
			t.Fatalf("a side child was started under a refused conf: %v", rec)
		}
		if _, ok := rec["cntlr_pointer"]; ok {
			t.Fatalf("a cntlr child was started under a refused conf: %v", rec)
		}
	}
	return h, w
}

// TestSpFanOutRefusesAnInvalidSpConf checks the §7 gate in front of RW14. The
// SP's stored bdev_conf is what every side and cntlr request is built from,
// and two of its members are barely read on this side at all:
// dm_raid0_conf.stripe_size on no worker path whatsoever, and
// redund_md_raid1.bitmap_chunk_block_cnt only inside model.GrowSlice's §3.6
// geometry, which re-reads the SP conf from the store in its own STM.
// Otherwise both just travel through the verbatim bdev_conf the cntlr request
// forwards. So the fan-out is the one place the worker can refuse to hand the
// cn agent a geometry nobody chose, and it refuses the whole plan rather than
// part of it.
//
// The two subtests are the two ways OUT of that refusal, and the coordinator
// reaches them through different code: run()'s desiredCh arm calls fanOut()
// unconditionally, while tick() re-enters it only when the refusal arm has
// re-armed fanWanted — applyPlan, the only other thing that arms a retry (via
// idleCnt), is never reached on that arm. An SP repaired without an SpRev bump
// — an operator rewriting the stored conf, a rollback — has the ticker and
// nothing else.
func TestSpFanOutRefusesAnInvalidSpConf(t *testing.T) {
	t.Run("recovers on a desired change", func(t *testing.T) {
		h, w := spRefusedFanOut(t)
		// The next fan-out — here the one a desired change drives (RW3) —
		// picks up a repaired conf and starts the children it owed.
		h.ops.setState(spFixture())
		w.update(desiredState{revision: testSpRev + 1, handle: testSpName})
		waitFor(t, "fan-out after the conf is repaired", func() bool {
			return len(h.sides[spDnA].syncups()) > 0 &&
				len(h.cntlrs[spCnA].syncups()) > 0
		})
	})

	t.Run("recovers on the ticker", func(t *testing.T) {
		h, _ := spRefusedFanOut(t)
		// Ticks under the STILL-bad conf. The coordinator's two §7 gates
		// refuse it once each — the fan-out's and the reaction pass's, which
		// keep separate halves of one memo — and then stay quiet however many
		// ticks follow.
		h.advanceUntil("the reaction pass to refuse the same conf",
			roundInterval, func() bool {
				return len(h.logs.withMsg(msgInvalidStoredConf)) == 2
			})
		for i := 0; i < 3; i++ {
			h.clk.advance(roundInterval)
			time.Sleep(5 * time.Millisecond)
		}
		if got := len(h.logs.withMsg(msgInvalidStoredConf)); got != 2 {
			t.Fatalf("%d refusal records, want one from the fan-out and one "+
				"from the reaction pass", got)
		}

		// The conf is repaired with NO desired change and no report from a
		// child — there is no child. The ticker's re-entry into fanOut is the
		// only thing left that can start them.
		h.ops.setState(spFixture())
		h.advanceUntil("fan-out after the conf is repaired", roundInterval,
			func() bool {
				return len(h.sides[spDnA].syncups()) > 0 &&
					len(h.cntlrs[spCnA].syncups()) > 0
			})
	})
}

// TestSpChildRestartedOnEndpointChange checks RW14: a child whose endpoint
// changed is restarted at the new one, while its siblings keep running.
func TestSpChildRestartedOnEndpointChange(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	const moved = "spdn9:9520"
	movedStub := h.addSide(moved)
	w := h.start()

	waitFor(t, "first syncups", func() bool {
		return len(h.sides[spDnD].syncups()) > 0
	})
	before := len(h.cntlrs[spCnA].syncups())

	// The spare leg's side moved to another DN, at a new revision.
	state := spFixture()
	grp := state.Slices[spSliceA].GetDataGrpList()[0]
	grp.GetSpareLegList()[0].GetSideList()[0].AddrPort = moved
	state.DnByAddr[moved] = &pb.DnConf{DnId: 0xd9}
	h.ops.setState(state)
	w.update(desiredState{revision: testSpRev + 1, handle: testSpName})

	waitFor(t, "syncup at the new endpoint", func() bool {
		return len(movedStub.syncups()) > 0
	})
	req := movedStub.syncups()[0]
	if req.GetDnId() != 0xd9 ||
		req.GetSidePointer().GetSideId() != spSideSpare {
		t.Fatalf("moved side request = %v", req)
	}
	// The old endpoint was never told to delete anything (§10.2, [D10]).
	for _, old := range h.sides[spDnD].syncups() {
		if old.GetRevision() > testSpRev {
			t.Fatalf("the old endpoint was driven after the move")
		}
	}
	// A sibling kept its child and simply got the new revision (RW6).
	waitFor(t, "sibling re-synced", func() bool {
		return len(h.cntlrs[spCnA].syncups()) > before
	})
	stopped := 0
	for _, rec := range h.logs.withMsg(msgRevisionWorkerStopped) {
		if _, ok := rec["side_pointer"]; ok {
			stopped++
		}
	}
	if stopped != 1 {
		t.Fatalf("%d side children stopped, want only the moved one", stopped)
	}
}

// TestSpFanOutWaitsForItsRevision checks RW14's revision guard: a fan-out
// whose load is ahead of the revision the coordinator was delivered builds
// nothing. The case is the one the guard exists for (HL2): a Failover
// committed N+1 and the SpRev watch has not delivered it when a tick re-fans
// the SP. Built, that plan would label the promotion N — the revision the
// standby's agent applied as a standby — and restart the standby's child,
// whose first Check the agent answers at N with a clean standby shape: the
// promoted cntlr would settle without ever being sent the primary request.
func TestSpFanOutWaitsForItsRevision(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	// The spare's DN has no DnConf: one idle side, so every tick re-fans.
	state := spFixture()
	delete(state.DnByAddr, spDnD)
	h.ops.setState(state)
	// Every cntlr agent answers a Check at the revision it last applied, as
	// the cn agent does, with a clean CntlrInfo.
	for _, stub := range h.cntlrs {
		stub.checkReply = func(*pb.CheckCntlrRequest) *pb.CheckCntlrReply {
			var rev uint64
			if reqs := stub.syncups(); len(reqs) > 0 {
				rev = reqs[len(reqs)-1].GetRevision()
			}
			return &pb.CheckCntlrReply{Revision: rev, CntlrInfo: &pb.CntlrInfo{}}
		}
	}
	w := h.start()
	waitFor(t, "the standby synced at N", func() bool {
		return len(h.cntlrs[spCnB].syncups()) > 0
	})
	before := len(h.cntlrs[spCnB].syncups())

	// model.Failover committed N+1; the watch has not delivered it.
	next := spFixture()
	delete(next.DnByAddr, spDnD)
	next.SpRevision = testSpRev + 1
	next.Cntlrs[spCntlrPrimary].Primary = false
	next.Cntlrs[spCntlrStandby].Primary = true
	next.Cntlrs[spCntlrStandby].Settling = true
	h.ops.setState(next)
	loads := h.ops.loadCnt()
	// One tick: a fan-out load, then a reaction-pass load. A child that
	// fan-out restarts is stopped, and logs it, before the pass loads.
	h.advanceUntil("a tick", roundInterval, func() bool {
		return h.ops.loadCnt() >= loads+2
	})
	if got := len(h.logs.withMsg(msgRevisionWorkerStopped)); got != 0 {
		t.Fatalf("%d children restarted ahead of the revision", got)
	}
	// More ticks, and rounds on the clock the children share: the standby's
	// child keeps its standby plan, so its agent's clean answer at N settles
	// nothing and re-syncs nothing.
	for i := 0; i < 3; i++ {
		h.clk.advance(roundInterval)
		time.Sleep(5 * time.Millisecond)
	}
	if got := settleWrites(h.hw); len(got) != 0 {
		t.Fatalf("settled ahead of the revision: %+v", got)
	}
	if got := len(h.logs.withMsg(msgCntlrSettled)); got != 0 {
		t.Fatalf("%d cntlr settled records ahead of the revision", got)
	}
	if got := len(h.cntlrs[spCnB].syncups()); got != before {
		t.Fatalf("the standby was re-synced %d times ahead of the revision",
			got-before)
	}

	// The delivery re-fans at N+1: the promoted cntlr is sent the primary
	// request, and its reply to that settles it.
	w.update(desiredState{revision: testSpRev + 1, handle: testSpName})
	// The record is logged after the settle write returns, on the child's
	// goroutine: waiting for the write alone would race it.
	waitFor(t, "the settle", func() bool {
		return len(h.logs.withMsg(msgCntlrSettled)) > 0
	})
	promoted := false
	for _, req := range h.cntlrs[spCnB].syncups()[before:] {
		if req.GetRevision() == testSpRev+1 && req.GetCntlr().GetPrimary() {
			promoted = true
		}
	}
	if !promoted {
		t.Fatalf("settled without a primary request at N+1")
	}
	got := settleWrites(h.hw)
	if len(got) != 1 || got[0].objId != spCntlrStandby {
		t.Fatalf("settle writes = %+v, want the promoted cntlr's", got)
	}
	recs := h.logs.withMsg(msgCntlrSettled)
	if len(recs) != 1 {
		t.Fatalf("%d cntlr settled records, want 1", len(recs))
	}
	if rev, _ := recs[0]["revision"].(float64); uint64(rev) != testSpRev+1 {
		t.Fatalf("cntlr settled at revision %v, want N+1", recs[0]["revision"])
	}
}

// TestSidesAreSyncedBeforeCntlrs checks RW14's sides-first order: every
// SyncupSide at a revision is answered before any SyncupCntlr at that revision
// is sent, which mitigates the races between a cntlr's converge and its
// sides' ([D16]). The fake clock never moves here, so the barrier's timer is
// never what releases the cntlrs: the sides' reports are.
//
// At the start's fan-out every child is new, and dn3 already holds N, as after
// a handoff: it answers its first Check at N and is sent no SyncupSide at all,
// so that Check reply has to count, or the cntlrs would wait out the timer. At
// the bump every child is kept, and dn1 holds its answer back: the cntlrs must
// wait for it, and go as soon as it comes.
func TestSidesAreSyncedBeforeCntlrs(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	const bump = testSpRev + 1
	// One counter stamps every SyncupSide as it is answered and every
	// SyncupCntlr as it arrives, so their order is total across the agents.
	// Both maps are revision -> side_id / cntlr_id -> stamps.
	var (
		mu     sync.Mutex
		seq    int64
		sides  = make(map[uint64]map[uint64][]int64)
		cntlrs = make(map[uint64]map[uint64][]int64)
	)
	stamp := func(byRev map[uint64]map[uint64][]int64, rev, id uint64) {
		mu.Lock()
		defer mu.Unlock()
		seq++
		if byRev[rev] == nil {
			byRev[rev] = make(map[uint64][]int64)
		}
		byRev[rev][id] = append(byRev[rev][id], seq)
	}
	count := func(byRev map[uint64]map[uint64][]int64, rev uint64) int {
		mu.Lock()
		defer mu.Unlock()
		return len(byRev[rev])
	}
	h.sides[spDnD].checkReply = func(*pb.CheckSideRequest) *pb.CheckSideReply {
		return &pb.CheckSideReply{Revision: testSpRev}
	}
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	for addr, stub := range h.sides {
		held := addr == spDnB
		stub.syncupReply = func(req *pb.SyncupSideRequest) *pb.SyncupSideReply {
			if held && req.GetRevision() == bump {
				<-gate
			}
			stamp(sides, req.GetRevision(), req.GetSidePointer().GetSideId())
			return &pb.SyncupSideReply{Revision: req.GetRevision()}
		}
	}
	for _, stub := range h.cntlrs {
		stub.syncupReply = func(req *pb.SyncupCntlrRequest) *pb.SyncupCntlrReply {
			stamp(cntlrs, req.GetRevision(), req.GetCntlrPointer().GetCntlrId())
			return &pb.SyncupCntlrReply{Revision: req.GetRevision()}
		}
	}
	w := h.start()
	// Registered after start, so it runs before the coordinator's stop, which
	// would otherwise wait for the held SyncupSide to time out.
	t.Cleanup(release)

	waitFor(t, "the cntlrs synced at N", func() bool {
		return count(cntlrs, testSpRev) == 3
	})
	next := spFixture()
	next.SpRevision = bump
	h.ops.setState(next)
	w.update(desiredState{revision: bump, handle: testSpName})
	waitFor(t, "four sides answered at N+1, the fifth held", func() bool {
		held := false
		for _, req := range h.sides[spDnB].syncups() {
			held = held || req.GetRevision() == bump
		}
		return held && count(sides, bump) == 4
	})
	// The window in which a cntlr would be sent the bump without the
	// barrier: nothing but the held answer may end it.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) && count(cntlrs, bump) == 0 {
		time.Sleep(time.Millisecond)
	}
	release()
	waitFor(t, "every side and every cntlr synced at N+1", func() bool {
		return count(sides, bump) == 5 && count(cntlrs, bump) == 3
	})

	mu.Lock()
	defer mu.Unlock()
	if got := len(sides[testSpRev][spSideSpare]); got != 0 {
		t.Fatalf("the side whose agent held N already was sent %d SyncupSide "+
			"at N, want none", got)
	}
	for _, want := range []struct {
		rev   uint64
		sides int
	}{{bump, 5}, {testSpRev, 4}} {
		if got := len(sides[want.rev]); got != want.sides {
			t.Fatalf("%d sides synced at %d, want %d", got, want.rev, want.sides)
		}
		var lastSide uint64
		var lastStamp int64
		for _, sideId := range sortedKeys(sides[want.rev]) {
			stamps := sides[want.rev][sideId]
			if len(stamps) != 1 {
				t.Fatalf("side %d was sent %d SyncupSide at %d, want 1",
					sideId, len(stamps), want.rev)
			}
			if stamps[0] > lastStamp {
				lastSide, lastStamp = sideId, stamps[0]
			}
		}
		for _, cntlrId := range sortedKeys(cntlrs[want.rev]) {
			stamps := cntlrs[want.rev][cntlrId]
			if len(stamps) != 1 {
				t.Fatalf("cntlr %d was sent %d SyncupCntlr at %d, want 1",
					cntlrId, len(stamps), want.rev)
			}
			if stamps[0] < lastStamp {
				t.Fatalf("SyncupCntlr at %d reached cntlr %d as event #%d, "+
					"before side %d answered its SyncupSide at %d (#%d)",
					want.rev, cntlrId, stamps[0], lastSide, want.rev, lastStamp)
			}
		}
	}
}

// TestSidesFirstBarrierIsBounded checks RW14's bound on the sides-first order:
// a side whose DN never answers holds the cntlrs for one cntlr_interval from
// the fan-out that first held them, and no longer. A bump inside that
// interval replaces what is held — the cntlrs are sent the newer revision
// only, never the superseded one — without moving the deadline, so a stream
// of bumps cannot hold them past one interval either. The side children keep
// being driven meanwhile: the bump reaches every live DN at once.
func TestSidesFirstBarrierIsBounded(t *testing.T) {
	h := newSpHarness(t)
	// The spare's DN is down: nothing answers at spDnD.
	for _, addr := range []string{spDnA, spDnB, spDnC} {
		h.addSide(addr)
	}
	for _, addr := range []string{spCnA, spCnB, spCnC} {
		h.addCntlr(addr)
	}
	const bump = testSpRev + 1
	// sent stamps every SyncupCntlr with its revision and the fake time since
	// the start's fan-out, per cntlr_id.
	type stamp struct {
		revision uint64
		after    time.Duration
	}
	start := h.clk.now()
	var mu sync.Mutex
	sent := make(map[uint64][]stamp)
	for _, stub := range h.cntlrs {
		stub.syncupReply = func(req *pb.SyncupCntlrRequest) *pb.SyncupCntlrReply {
			mu.Lock()
			defer mu.Unlock()
			id := req.GetCntlrPointer().GetCntlrId()
			sent[id] = append(sent[id], stamp{
				revision: req.GetRevision(),
				after:    h.clk.now().Sub(start),
			})
			return &pb.SyncupCntlrReply{Revision: req.GetRevision()}
		}
	}
	snapshot := func() map[uint64][]stamp {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[uint64][]stamp, len(sent))
		for id, stamps := range sent {
			out[id] = append([]stamp(nil), stamps...)
		}
		return out
	}
	// live reports whether the four sides whose DNs answer have each been
	// sent a SyncupSide at rev; dn0 hosts two of them.
	live := func(rev uint64) bool {
		for addr, want := range map[string]int{spDnA: 2, spDnB: 1, spDnC: 1} {
			got := 0
			for _, req := range h.sides[addr].syncups() {
				if req.GetRevision() == rev {
					got++
				}
			}
			if got < want {
				return false
			}
		}
		return true
	}
	w := h.start()
	waitFor(t, "the live sides synced at N", func() bool {
		return live(testSpRev)
	})

	// Three seconds into the hold, the bump: the live sides are sent it at
	// once, and what the cntlrs are held with is replaced.
	h.clk.advance(3 * time.Second)
	next := spFixture()
	next.SpRevision = bump
	h.ops.setState(next)
	w.update(desiredState{revision: bump, handle: testSpName})
	waitFor(t, "the live sides synced at N+1", func() bool {
		return live(bump)
	})

	// A second short of one cntlr_interval since the start's fan-out.
	h.clk.advance(roundInterval - 3*time.Second - time.Second)
	// An early release needs real time to reach a cntlr: a stamp is taken on
	// arrival, so without this window a release one second early would only
	// be seen after the next jump, stamped at the full interval.
	settle := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(settle) && len(snapshot()) == 0 {
		time.Sleep(time.Millisecond)
	}
	early := snapshot()
	for _, cntlrId := range sortedKeys(early) {
		first := early[cntlrId][0]
		t.Fatalf("cntlr %d was sent a SyncupCntlr at %d %v after the fan-out "+
			"while the spare's DN never answered, want none before one "+
			"cntlr_interval (%v)",
			cntlrId, first.revision, first.after, roundInterval)
	}
	h.clk.advance(time.Second)
	waitFor(t, "every cntlr synced", func() bool {
		return len(snapshot()) == 3
	})
	got := snapshot()
	for _, cntlrId := range sortedKeys(got) {
		stamps := got[cntlrId]
		if len(stamps) != 1 {
			t.Fatalf("cntlr %d was sent %d SyncupCntlr, want exactly one",
				cntlrId, len(stamps))
		}
		if stamps[0].revision != bump || stamps[0].after != roundInterval {
			t.Fatalf("cntlr %d was sent a SyncupCntlr at %d %v after the "+
				"fan-out, want it at %d %v after: held for one cntlr_interval "+
				"and no longer, with the superseded revision never sent",
				cntlrId, stamps[0].revision, stamps[0].after,
				bump, roundInterval)
		}
	}
	recs := h.logs.withMsg(msgSpSidesUnsynced)
	if len(recs) != 1 {
		t.Fatalf("%d %q records, want the one release by the timer",
			len(recs), msgSpSidesUnsynced)
	}
	if rev, _ := recs[0]["revision"].(float64); uint64(rev) != bump {
		t.Fatalf("released at revision %v, want %d", recs[0]["revision"], bump)
	}
	// A live side's report can still be on its way to the coordinator when
	// the fake clock jumps, so the count is bounded below only: the spare's
	// side is always in it.
	if cnt, _ := recs[0]["side_cnt"].(float64); cnt < 1 {
		t.Fatalf("side_cnt = %v, want at least the spare's side",
			recs[0]["side_cnt"])
	}
}

// TestSidesFirstDropsTheDemotedPrimarysLegRows checks HL2 under RW14's
// sides-first hold. A failover's sides reload the old primary's per-CN
// dm-linear onto dm-error while its demotion is still held, so its agent,
// still running the primary shape, probes every leg through that fence and
// reports it ERROR. Those rows describe the fence, not the legs, and the
// loaded state already names the new primary: none may set a leg's err_epoch.
//
// The spare's DN never answers the failover, and the fake clock stops short
// of the hold's deadline, so the old primary's next Check round falls inside
// the hold. Once any side has been told the new primary, that round reports
// the meta leg ERROR and, in the same reply, the open td complete: RW19 takes
// a completed td from any cntlr, so the td's flip marks the moment the
// coordinator has processed the report that carried the ERROR row.
func TestSidesFirstDropsTheDemotedPrimarysLegRows(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	const bump = testSpRev + 1
	// fenced is set once any side has been sent the new primary's cn_id:
	// from then on the old primary's probes run into dm-error.
	var fenced atomic.Bool
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	for addr, stub := range h.sides {
		held := addr == spDnD
		stub.syncupReply = func(req *pb.SyncupSideRequest) *pb.SyncupSideReply {
			if req.GetSideConf().GetPrimaryCnId() == spCnIdB {
				fenced.Store(true)
			}
			if held && req.GetRevision() == bump {
				<-gate
			}
			return &pb.SyncupSideReply{Revision: req.GetRevision()}
		}
	}
	checks := make(chan struct{}, 64)
	h.cntlrs[spCnA].checkReply = func(
		req *pb.CheckCntlrRequest,
	) *pb.CheckCntlrReply {
		info := &pb.CntlrInfo{
			LegIdToLeg: map[uint64]*pb.ResInfo{
				spLegMeta: resOk("leg-meta"),
			},
		}
		if fenced.Load() {
			info.LegIdToLeg[spLegMeta] = resErr("leg-meta", "probe io error")
			info.TdIdToThinInfo = map[uint64]*pb.CntlrInfo_ThinInfo{
				spTdOpen: {SliceIdToDmThin: map[uint64]*pb.ResInfo{
					spSliceA: resOk("thin-a"),
					spSliceB: resOk("thin-b"),
				}},
			}
		}
		select {
		case checks <- struct{}{}:
		default:
		}
		return &pb.CheckCntlrReply{Revision: req.GetRevision(), CntlrInfo: info}
	}
	w := h.start()
	// Registered after start, so it runs before the coordinator's stop, which
	// would otherwise wait for the held SyncupSide to time out.
	t.Cleanup(release)

	// The old primary's agent answers its first Check at N, so it is sent no
	// SyncupCntlr; the other two are. All three running means the start's
	// hold is over.
	waitFor(t, "every cntlr driven at N", func() bool {
		return len(checks) > 0 &&
			len(h.cntlrs[spCnB].syncups()) > 0 &&
			len(h.cntlrs[spCnC].syncups()) > 0
	})
	waitFor(t, "the old primary's leg recorded healthy", func() bool {
		for _, write := range h.hw.all() {
			if write.record == healthRecordLeg && write.objId == spLegMeta {
				return true
			}
		}
		return false
	})
	// Its next round must come due before the hold's deadline, so its round
	// timer has to be armed now, at the start. A child holds one fake timer
	// at a time — the round timer between rounds, the reply timer inside one,
	// none in a Syncup* — and the coordinator its ticker, so nine waiters
	// (five sides, three cntlrs, the ticker) with the old primary past its
	// first reply mean that timer is armed.
	waitFor(t, "every child between rounds", func() bool {
		return h.clk.waiterCount() == 9
	})

	// Three seconds in, the failover: cntlr 2 is the primary from N+1 on.
	h.clk.advance(3 * time.Second)
	next := spFixture()
	next.SpRevision = bump
	next.Cntlrs[spCntlrPrimary].Primary = false
	next.Cntlrs[spCntlrStandby].Primary = true
	h.ops.setState(next)
	w.update(desiredState{revision: bump, handle: testSpName})
	waitFor(t, "the old primary fenced, the spare's side held", func() bool {
		held := false
		for _, req := range h.sides[spDnD].syncups() {
			held = held || req.GetRevision() == bump
		}
		return held && fenced.Load()
	})

	// The cntlrs' next round, two seconds later: one cntlr_interval after
	// their first, and three seconds before the hold's deadline.
	h.clk.advance(2 * time.Second)
	waitFor(t, "the old primary's fenced report processed", func() bool {
		return len(h.ops.createdCalls()) > 0
	})
	for _, stub := range h.cntlrs {
		for _, req := range stub.syncups() {
			if req.GetRevision() == bump {
				t.Fatalf("cntlr %d was sent the failover before the hold's "+
					"deadline, with the spare's side silent",
					req.GetCntlrPointer().GetCntlrId())
			}
		}
	}
	for _, write := range h.hw.all() {
		if write.record == healthRecordLeg && write.epoch != 0 {
			t.Fatalf("a leg row of the primary the held failover demotes "+
				"was recorded: %+v", write)
		}
	}
}

// TestSpLegRowsFollowTheLatestPlan pins which cntlr's leg rows the coordinator
// records (HL2): only a cntlr's that the plan it last decided makes the
// primary — the held plan while RW14's barrier holds the cntlrs, else the plan
// the child was handed. The reports travel over a buffered channel, so rows
// the old primary built during a hold can be read after the release that
// handed it the standby plan; they are dropped then as well.
func TestSpLegRowsFollowTheLatestPlan(t *testing.T) {
	type holdCase int
	const (
		noHold holdCase = iota
		heldPrimary
		heldStandby
		heldAbsent
	)
	cases := []struct {
		name string
		// child is a running cntlr child, handed a primary plan or not.
		child  bool
		handed bool
		held   holdCase
		want   bool
	}{
		{"the handed primary, nothing held", true, true, noHold, true},
		{"read after the release handed it the standby plan",
			true, false, noHold, false},
		{"its child stopped, nothing held", false, false, noHold, false},
		{"a primary the held plan keeps", true, true, heldPrimary, true},
		{"a primary the held plan demotes", true, true, heldStandby, false},
		{"a primary the held plan no longer names, its child stopped",
			false, false, heldAbsent, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captureLogs(t)
			d := newTestDeps(
				testConfig(common.WorkerRoleSp), newFakeStore(), newFakeClock(),
			)
			hw := &fakeHealthWriter{}
			d.health = hw
			w := spTestWorker(d)
			w.legSlice[spLegMeta] = spSliceA
			if tc.child {
				w.cntlrs[spCntlrPrimary] = &cntlrChild{
					plan: settlePlan(tc.handed, false),
				}
			}
			if tc.held != noHold {
				plans := make(map[uint64]*cntlrPlan)
				if tc.held != heldAbsent {
					plans[spCntlrPrimary] = settlePlan(tc.held == heldPrimary, false)
				}
				w.held = &cntlrHold{revision: testSpRev + 1, plans: plans}
				// A side short of the held revision keeps the hold pending
				// through handleReports' own release check.
				key := sideKey{legId: spLegMeta, sideId: spSideMeta}
				w.sides[key] = &sideChild{synced: testSpRev}
			}
			w.handleReports(spReport{
				cntlrId: spCntlrPrimary,
				legRows: []legRow{{
					legId:   spLegMeta,
					obs:     healthErrorRow,
					resName: "leg-meta",
				}},
			})
			recorded := false
			for _, write := range hw.all() {
				recorded = recorded || (write.record == healthRecordLeg &&
					write.objId == spLegMeta && write.epoch != 0)
			}
			if recorded != tc.want {
				t.Fatalf("the ERROR row set the leg's err_epoch: %v, want %v",
					recorded, tc.want)
			}
		})
	}
}

// TestSidesFirstIdleSidesHoldNothing checks RW14's "a side left idle is not
// waited for, and with every side idle the cntlrs go at once": a standby whose
// CnConf is absent leaves every side idle (RW15), and the resolved cntlrs are
// driven with the fake clock never moving, so the barrier's timer cannot be
// what releases them. No child exists yet to report, so only the fan-out's own
// release can: the case fails without it.
func TestSidesFirstIdleSidesHoldNothing(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	state := spFixture()
	delete(state.CnByAddr, spCnB)
	h.ops.setState(state)
	h.start()
	waitFor(t, "the resolved cntlrs synced with every side idle", func() bool {
		return len(h.cntlrs[spCnA].syncups()) > 0 &&
			len(h.cntlrs[spCnC].syncups()) > 0
	})
	for _, addr := range []string{spDnA, spDnB, spDnC, spDnD} {
		if n := len(h.sides[addr].syncups()); n != 0 {
			t.Fatalf("%s was sent %d SyncupSide with the cntlr set unresolved",
				addr, n)
		}
	}
	if recs := h.logs.withMsg(msgSpSidesUnsynced); len(recs) != 0 {
		t.Fatalf("%d %q records, want none", len(recs), msgSpSidesUnsynced)
	}
}

// TestSidesFirstHeldPromotionHasNoPrimaryInfo checks AR1 under RW14's
// sides-first hold. A failover's promotion is held with the other cntlrs'
// requests, so the pass's snapshot already names the new primary while that
// cntlr's child still drives the standby plan, and the child's info is a
// standby's report: its leg rows are transport liveness and ana_state, not
// the §3.6 block probe AR8 step 1 needs. Inside the hold the pass must read no
// primary info, so AR8 waits for the spare rather than switching it in on the
// standby's row; once the release has handed the child the primary plan, the
// pass reads its info again and the spare is switched in.
func TestSidesFirstHeldPromotionHasNoPrimaryInfo(t *testing.T) {
	const spareLegId = uint64(700)
	state := reactFixture(t)
	state.Cntlrs[reactCntlrA].Primary = false
	state.Cntlrs[reactCntlrB].Primary = true
	h := newReactHarness(t, state)
	h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
	h.dataGrp().SpareLegList = append(h.dataGrp().SpareLegList, &pb.Leg{
		LegId:  spareLegId,
		LegIdx: 2,
		SideList: []*pb.Side{{
			SideId:      800,
			AddrPort:    reactDnC,
			Provisioned: true,
		}},
	})
	h.dnCands(reactDnD)
	spareOk := func() *pb.CntlrInfo {
		return &pb.CntlrInfo{LegIdToLeg: map[uint64]*pb.ResInfo{
			spareLegId: {ResName: "leg", Status: pb.ResStatus_RES_STATUS_OK},
		}}
	}
	// The old primary still drives the primary plan, and the promoted
	// standby's last report reads its live path to the spare OK.
	demoted := &cntlrChild{
		plan:   &cntlrPlan{cntlrId: reactCntlrA, primary: true},
		driver: &cntlrDriver{lastInfo: &pb.CntlrInfo{}},
	}
	promoted := &cntlrChild{
		plan:   &cntlrPlan{cntlrId: reactCntlrB},
		driver: &cntlrDriver{lastInfo: spareOk()},
	}
	h.w.cntlrs[reactCntlrA] = demoted
	h.w.cntlrs[reactCntlrB] = promoted
	h.w.held = &cntlrHold{
		revision: reactSpRevision + 1,
		plans: map[uint64]*cntlrPlan{
			reactCntlrA: {cntlrId: reactCntlrA},
			reactCntlrB: {cntlrId: reactCntlrB, primary: true},
		},
	}

	p := h.w.newPass(h.state, reactClusterConf())
	if p.primaryId != reactCntlrB {
		t.Fatalf("the pass's primary is cntlr %d, want %d", p.primaryId,
			reactCntlrB)
	}
	if p.info != nil {
		t.Fatalf("inside the hold the pass read cntlr %d's standby report "+
			"as the primary's info: %v", reactCntlrB, p.info)
	}
	h.pass()
	h.wantOps()
	h.wantSkipped(reactionSpareSwitch, reasonSparePending)

	// The release hands both children their new plans, and the promotion's
	// reply reports the new primary's probe of the spare.
	h.w.held = nil
	demoted.plan = &cntlrPlan{cntlrId: reactCntlrA}
	promoted.plan = &cntlrPlan{cntlrId: reactCntlrB, primary: true}
	promoted.driver.lastInfo = spareOk()
	h.pass()
	calls := h.wantOps("switch_spare")
	if calls[0].spareLegId != spareLegId ||
		calls[0].targetLegId != reactDataLegA {
		t.Fatalf("switch = %+v", calls[0])
	}
}

// TestSpProvisionedFlipReported checks RW18 end to end: a side whose request
// carried provisioned == false and whose agent reports zeroed == total > 0 is
// reported to the coordinator, which runs the flip; a provisioned side and a
// still-zeroing one are not.
func TestSpProvisionedFlipReported(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	// The destination side is fully zeroed; the spare is still zeroing.
	h.sides[spDnC].checkReply = func(
		req *pb.CheckSideRequest,
	) *pb.CheckSideReply {
		return &pb.CheckSideReply{
			Revision: req.GetRevision(),
			SideInfo: &pb.SideInfo{ZeroedExtCnt: 8, TotalExtCnt: 8},
		}
	}
	h.sides[spDnD].checkReply = func(
		req *pb.CheckSideRequest,
	) *pb.CheckSideReply {
		return &pb.CheckSideReply{
			Revision: req.GetRevision(),
			SideInfo: &pb.SideInfo{ZeroedExtCnt: 3, TotalExtCnt: 8},
		}
	}
	// A provisioned side reporting the same counters must not be reported.
	h.sides[spDnA].checkReply = func(
		req *pb.CheckSideRequest,
	) *pb.CheckSideReply {
		return &pb.CheckSideReply{
			Revision: req.GetRevision(),
			SideInfo: &pb.SideInfo{ZeroedExtCnt: 1, TotalExtCnt: 1},
		}
	}
	h.start()

	// No clock advance: the first round of every child delivers its reply, so
	// nothing here can be mistaken for a round timeout.
	waitFor(t, "provisioned flip", func() bool {
		return len(h.ops.provisionedCalls()) > 0
	})
	for _, call := range h.ops.provisionedCalls() {
		for _, ref := range call {
			if ref.SideId != spSideDst {
				t.Fatalf("flipped %+v, want only the zeroed side", ref)
			}
			if ref.SliceId != spSliceA || ref.LegId != spLegMigr {
				t.Fatalf("flip ref = %+v", ref)
			}
		}
	}
}

// TestSpCreatedFlipFromACheckRound is RW19 end to end over the Check stream,
// on the path a deferred slice takes. Every round asks for NO info at all
// (RW4 step 2 sends show_info = false); it is the agent that attaches the
// CntlrInfo whenever a resource changed status (§9.7), and a thin row moving
// PROVISIONING → OK as the last deferred slice clears ([D15]) is such a
// change. So the flip has to run off exactly this reply: a worker that waited
// for a show_info round would leave the td uncreated — and every snapshot of
// it refused — until some unrelated resource happened to move.
func TestSpCreatedFlipFromACheckRound(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	var mu sync.Mutex
	rounds := 0
	cleared := false
	asked := false
	h.cntlrs[spCnA].checkReply = func(
		req *pb.CheckCntlrRequest,
	) *pb.CheckCntlrReply {
		mu.Lock()
		rounds++
		asked = asked || req.GetShowInfo()
		deferred := !cleared
		mu.Unlock()
		row := resOk("thin-b")
		if deferred {
			row = resStatus("thin-b", pb.ResStatus_RES_STATUS_PROVISIONING)
		}
		return &pb.CheckCntlrReply{
			Revision: req.GetRevision(),
			CntlrInfo: &pb.CntlrInfo{
				TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
					spTdOpen: {SliceIdToDmThin: map[uint64]*pb.ResInfo{
						spSliceA: resOk("thin-a"),
						spSliceB: row,
					}},
				},
			},
		}
	}
	h.start()

	// The negative has to be gated on the deferred reply having been FOLDED
	// AND OBSERVED, not merely sent: the round loop is sequential — RW4 step
	// 2 issues the next request only after step 5 processed the previous
	// reply — so a SECOND round is that evidence, while the arrival of the
	// first request at the stub says nothing yet about how a PROVISIONING row
	// was treated. The first round of every child delivers its reply with no
	// clock advance; the second one needs the round timer.
	h.advanceUntil("a round after the deferred slice", roundInterval,
		func() bool {
			mu.Lock()
			defer mu.Unlock()
			return rounds > 1
		})
	if got := len(h.ops.createdCalls()); got != 0 {
		t.Fatalf("%d FlipCreated calls while a slice is PROVISIONING", got)
	}
	mu.Lock()
	cleared = true
	mu.Unlock()
	h.advanceUntil("created flip", roundInterval, func() bool {
		return len(h.ops.createdCalls()) > 0
	})

	calls := h.ops.createdCalls()
	if len(calls[0]) != 1 || calls[0][0].TdId != spTdOpen ||
		calls[0][0].Name != "td0" {
		t.Fatalf("flip batch = %v, want the uncreated td", calls[0])
	}
	mu.Lock()
	defer mu.Unlock()
	if asked {
		t.Fatalf("a round asked for show_info; the flip must not depend on it")
	}
}

// TestSpCreatedFlipIgnoresTheReplyRevision is RW19's "the reply's revision is
// deliberately not compared with anything": a Check reply from a cntlr that
// still holds an OLDER revision than the one the worker is driving flips every
// td whose rows it reports complete, and the round then syncs the cntlr up
// (RW4 step 5) as it would for any mismatch. Thin ids are monotonic facts
// about the shared pool metadata, so a row that is OK at an older revision
// stays OK; identity is guarded by the td_id re-read inside the flip's own STM
// (R3), never by the revision of the reply that reported it.
func TestSpCreatedFlipIgnoresTheReplyRevision(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	// The syncup the mismatch forces answers with an EMPTY CntlrInfo, which
	// replaces the stored one (HL5). Without that, its reply would be
	// observed against the Check reply's info and the flip below could not be
	// attributed to the older-revision reply alone.
	h.cntlrs[spCnA].syncupReply = func(
		req *pb.SyncupCntlrRequest,
	) *pb.SyncupCntlrReply {
		return &pb.SyncupCntlrReply{
			Revision:  req.GetRevision(),
			CntlrInfo: &pb.CntlrInfo{},
		}
	}
	h.cntlrs[spCnA].checkReply = func(
		req *pb.CheckCntlrRequest,
	) *pb.CheckCntlrReply {
		return &pb.CheckCntlrReply{
			// One revision behind the SP the coordinator is driving.
			Revision: testSpRev - 1,
			CntlrInfo: &pb.CntlrInfo{
				TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
					spTdOpen: {SliceIdToDmThin: map[uint64]*pb.ResInfo{
						spSliceA: resOk("thin-a"),
						spSliceB: resOk("thin-b"),
					}},
				},
			},
		}
	}
	h.start()

	waitFor(t, "created flip from the older-revision reply", func() bool {
		return len(h.ops.createdCalls()) > 0
	})
	calls := h.ops.createdCalls()
	if len(calls[0]) != 1 || calls[0][0].TdId != spTdOpen {
		t.Fatalf("flip batch = %v, want the uncreated td", calls[0])
	}
	waitFor(t, "the syncup the older revision forces", func() bool {
		return len(h.cntlrs[spCnA].syncups()) > 0
	})
	if got := h.cntlrs[spCnA].syncups()[0].GetRevision(); got != testSpRev {
		t.Fatalf("syncup revision = %d, want the desired %d", got, testSpRev)
	}
}

// TestSpLegRowsFromPrimaryOnly checks HL2: the PRIMARY's leg_id_to_leg rows are
// recorded on the leg's slice, a STANDBY's are logged and never recorded, and
// the cntlr's own health ignores leg rows entirely.
func TestSpLegRowsFromPrimaryOnly(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	h.cntlrs[spCnA].checkReply = func(
		req *pb.CheckCntlrRequest,
	) *pb.CheckCntlrReply {
		return &pb.CheckCntlrReply{
			Revision: req.GetRevision(),
			CntlrInfo: &pb.CntlrInfo{
				LegIdToLeg: map[uint64]*pb.ResInfo{
					spLegMeta:  resErr("leg-meta", "probe io error"),
					spLegSpare: resOk("leg-spare"),
				},
			},
		}
	}
	h.cntlrs[spCnB].checkReply = func(
		req *pb.CheckCntlrRequest,
	) *pb.CheckCntlrReply {
		return &pb.CheckCntlrReply{
			Revision: req.GetRevision(),
			CntlrInfo: &pb.CntlrInfo{
				LegIdToLeg: map[uint64]*pb.ResInfo{
					// A standby's view of a leg the primary calls healthy.
					spLegSpare: resErr("leg-spare", "ana not optimized"),
				},
			},
		}
	}
	h.start()

	// No clock advance: the first round of every child delivers its reply, so
	// no cntlr can be marked unreachable here.
	waitFor(t, "leg health recorded", func() bool {
		for _, write := range h.hw.all() {
			if write.record == healthRecordLeg && write.epoch != 0 {
				return true
			}
		}
		return false
	})
	for _, write := range h.hw.all() {
		if write.record != healthRecordLeg {
			continue
		}
		if write.objId == spLegSpare && write.epoch != 0 {
			t.Fatalf("a standby's leg row was recorded: %+v", write)
		}
		if write.objId == spLegMeta {
			if write.epoch == 0 {
				t.Fatalf("the primary's ERROR row did not set err_epoch")
			}
			if write.sliceId != spSliceA {
				t.Fatalf("leg written on slice %d", write.sliceId)
			}
		}
	}
	// HL2: a cntlr's own health ignores leg_id_to_leg, so the primary stays
	// healthy while its leg is not.
	for _, write := range h.hw.all() {
		if write.record == healthRecordCntlr && write.epoch != 0 {
			t.Fatalf("a leg row set the cntlr's err_epoch: %+v", write)
		}
	}
	waitFor(t, "standby leg row logged", func() bool {
		return len(h.logs.withMsg(msgSpStandbyLegRow)) > 0
	})
}

// orphanPrimaryUnhealthy is the primary_unhealthy of
// TestOrphanedEpochIsClearedByTheOwner: longer than the owner's correction
// takes — the pass that loads the record plus the round after it — so the
// test pins the correction rather than a race with the reaction pass. At the
// product default of 5 s, one pass, a pass may act on an orphaned epoch before
// the correcting round lands, as it would on one bad round; here the first
// pass to load the primary's stamp does, finding it 5 s old, so a fix of that
// residual is pinned by running the test at 5.
const orphanPrimaryUnhealthy = 60

// spStateHealthWriter is a fake health writer whose cntlr, leg and side writes
// also land in the harness's stored SpState, under MD6's set/clear rule, so
// the reaction pass reads what the monitors wrote (AR1): the etcd of a
// live-coordinator health test. A write lands in the state before it is
// recorded: the test takes a recorded write as proof that the state holds it,
// and another observer's stamp laid between the two would be overwritten by a
// write the test had already seen. A write the fake fails lands nowhere.
type spStateHealthWriter struct {
	*fakeHealthWriter
	ops *fakeSpOps
}

func (w *spStateHealthWriter) setCntlrErrEpoch(
	ctx context.Context,
	cid uint64,
	spId uint64,
	cntlrId uint64,
	epoch uint64,
	settle bool,
) error {
	if err := w.refusal(); err != nil {
		return err
	}
	w.ops.putCntlrErrEpoch(cntlrId, epoch)
	return w.fakeHealthWriter.setCntlrErrEpoch(
		ctx, cid, spId, cntlrId, epoch, settle,
	)
}

func (w *spStateHealthWriter) setLegErrEpoch(
	ctx context.Context,
	cid uint64,
	spId uint64,
	sliceId uint64,
	legId uint64,
	epoch uint64,
) error {
	if err := w.refusal(); err != nil {
		return err
	}
	w.ops.putSliceErrEpoch(sliceId, legId, 0, epoch)
	return w.fakeHealthWriter.setLegErrEpoch(
		ctx, cid, spId, sliceId, legId, epoch,
	)
}

func (w *spStateHealthWriter) setSideErrEpoch(
	ctx context.Context,
	cid uint64,
	spId uint64,
	sliceId uint64,
	sideId uint64,
	epoch uint64,
) error {
	if err := w.refusal(); err != nil {
		return err
	}
	w.ops.putSliceErrEpoch(sliceId, 0, sideId, epoch)
	return w.fakeHealthWriter.setSideErrEpoch(
		ctx, cid, spId, sliceId, sideId, epoch,
	)
}

// putCntlrErrEpoch applies MD6's set/clear rule — a nonzero epoch only onto a
// stored 0, a zero always — to one stored Cntlr, copy-on-write: loadSp hands
// the coordinator the stored state itself, so a record is replaced rather than
// changed under a pass that may be reading it.
func (o *fakeSpOps) putCntlrErrEpoch(cntlrId uint64, epoch uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	old, ok := o.state.Cntlrs[cntlrId]
	if !ok || (epoch != 0 && old.GetErrEpoch() != 0) {
		return
	}
	next := *o.state
	next.Cntlrs = make(map[uint64]*pb.Cntlr, len(o.state.Cntlrs))
	for id, cntlr := range o.state.Cntlrs {
		next.Cntlrs[id] = cntlr
	}
	cntlr := proto.Clone(old).(*pb.Cntlr)
	cntlr.ErrEpoch = epoch
	next.Cntlrs[cntlrId] = cntlr
	o.state = &next
}

// putSliceErrEpoch is putCntlrErrEpoch for the side sideId of a slice or, with
// sideId 0, for its leg legId: both live inside the Slice record, which is
// replaced by a clone.
func (o *fakeSpOps) putSliceErrEpoch(
	sliceId uint64,
	legId uint64,
	sideId uint64,
	epoch uint64,
) {
	o.mu.Lock()
	defer o.mu.Unlock()
	old, ok := o.state.Slices[sliceId]
	if !ok {
		return
	}
	slice := proto.Clone(old).(*pb.Slice)
	field := sliceEpochField(slice, legId, sideId)
	if field == nil || (epoch != 0 && *field != 0) {
		return
	}
	*field = epoch
	next := *o.state
	next.Slices = make(map[uint64]*pb.Slice, len(o.state.Slices))
	for id, stored := range o.state.Slices {
		next.Slices[id] = stored
	}
	next.Slices[sliceId] = slice
	o.state = &next
}

// cntlrErrEpoch is one stored Cntlr's err_epoch.
func (o *fakeSpOps) cntlrErrEpoch(cntlrId uint64) uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.state.Cntlrs[cntlrId].GetErrEpoch()
}

// sliceErrEpoch is the stored err_epoch putSliceErrEpoch writes.
func (o *fakeSpOps) sliceErrEpoch(
	sliceId uint64,
	legId uint64,
	sideId uint64,
) uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	field := sliceEpochField(o.state.Slices[sliceId], legId, sideId)
	if field == nil {
		return 0
	}
	return *field
}

// sliceEpochField finds the err_epoch of the side sideId anywhere in a slice
// (side ids are unique within the SP) or, with sideId 0, of the leg legId, in
// either list of either group; nil when the slice holds neither.
func sliceEpochField(slice *pb.Slice, legId uint64, sideId uint64) *uint64 {
	for _, grp := range allGroups(slice) {
		for _, list := range [][]*pb.Leg{
			grp.GetLegList(),
			grp.GetSpareLegList(),
		} {
			for _, leg := range list {
				if sideId == 0 && leg.GetLegId() == legId {
					return &leg.ErrEpoch
				}
				for _, side := range leg.GetSideList() {
					if sideId != 0 && side.GetSideId() == sideId {
						return &side.ErrEpoch
					}
				}
			}
		}
	}
	return nil
}

// reactWith gives a running coordinator a reactor over a recording model
// surface: the production one holds the harness's nil etcd client, which any
// reaction would dereference. It is safe until the clock first advances — a
// pass runs only on a tick, or on the drain token only a pass arms, and the
// tick's channel send orders this write before the pass reads it.
func (h *spHarness) reactWith(w *spWorker, rops *fakeReactionOps) {
	w.react = newReactor(rops)
}

// epochWrites is the err_epoch of every health write of one record, in order.
func epochWrites(
	hw *fakeHealthWriter,
	record string,
	objId uint64,
) []uint64 {
	var out []uint64
	for _, write := range hw.all() {
		if write.record == record && write.objId == objId {
			out = append(out, write.epoch)
		}
	}
	return out
}

// TestOrphanedEpochIsClearedByTheOwner pins HL3's cache rule end to end, on a
// live coordinator with the fake clock. During the accepted ownership overlap
// (§0 item 4) two drivers of one SP can hold different verdicts: here the
// owner, whose rounds are clean, and another observer whose view differs — a
// gRPC path broken while its etcd path works, say — which stamps an err_epoch
// on records the owner sees healthy and then fences: the primary cntlr's, the
// standby's, a side's, a spare leg's side's and a leg's. The owner's monitors
// had written clean already and wrote only on a transition from what they had
// written, so the orphaned epochs stayed: primary_unhealthy failed the
// healthy primary over, and the demoted cntlr kept its epoch until AR7
// replaced the cntlr; the leg's would have repaired a healthy leg at
// leg_unhealthy (AR8). The memo is a cache of the record: the owner's next
// pass re-seeds every monitor from its load, the next clean verdict on each
// object is a transition and clears its epoch — before a third pass has run
// — and no pass acts on any of them.
func TestOrphanedEpochIsClearedByTheOwner(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	var primaryChecks atomic.Int64
	for addr, stub := range h.cntlrs {
		stub.checkReply = func(
			req *pb.CheckCntlrRequest,
		) *pb.CheckCntlrReply {
			// Clean, at the revision asked: no syncup.
			reply := &pb.CheckCntlrReply{Revision: req.GetRevision()}
			if addr == spCnA {
				primaryChecks.Add(1)
				// The primary's probe of the meta leg, which HL2 records
				// on the leg and leaves out of the cntlr's own health.
				reply.CntlrInfo = &pb.CntlrInfo{
					LegIdToLeg: map[uint64]*pb.ResInfo{
						spLegMeta: resOk("leg-meta"),
					},
				}
			}
			return reply
		}
	}
	for _, stub := range h.sides {
		stub.checkReply = func(req *pb.CheckSideRequest) *pb.CheckSideReply {
			return &pb.CheckSideReply{Revision: req.GetRevision()}
		}
	}
	state := spFixture()
	state.Conf.EventThreshold = &pb.EventThreshold{
		PrimaryUnhealthy: orphanPrimaryUnhealthy,
	}
	h.ops.setState(state)
	h.deps.health = &spStateHealthWriter{fakeHealthWriter: h.hw, ops: h.ops}
	rops := &fakeReactionOps{}
	h.reactWith(h.start(), rops)

	// round lets one more pass and one more round of the primary run. The
	// clock moves a second at a time, so that a round's reply is read long
	// before the round's own timeout can fire (RW4 step 3); and the
	// primary's agent sees a Check only once the previous round's reply has
	// been observed, while the coordinator runs one pass at a time, so every
	// earlier pass and every earlier round of the primary has finished once
	// both counts moved.
	round := func() {
		t.Helper()
		loads, checks := h.ops.loadCnt(), primaryChecks.Load()
		h.advanceUntil("a pass and a round of the primary", time.Second,
			func() bool {
				return h.ops.loadCnt() > loads &&
					primaryChecks.Load() > checks
			})
	}
	type record struct {
		name   string
		kind   string
		objId  uint64
		stamp  func(epoch uint64)
		stored func() uint64
	}
	cntlrRecord := func(name string, cntlrId uint64) record {
		return record{name, healthRecordCntlr, cntlrId,
			func(epoch uint64) { h.ops.putCntlrErrEpoch(cntlrId, epoch) },
			func() uint64 { return h.ops.cntlrErrEpoch(cntlrId) }}
	}
	sliceRecord := func(
		name string,
		kind string,
		legId uint64,
		sideId uint64,
	) record {
		objId := legId
		if sideId != 0 {
			objId = sideId
		}
		return record{name, kind, objId,
			func(epoch uint64) {
				h.ops.putSliceErrEpoch(spSliceA, legId, sideId, epoch)
			},
			func() uint64 {
				return h.ops.sliceErrEpoch(spSliceA, legId, sideId)
			}}
	}
	// The standby is stamped on its own, after the others are put right: a
	// standby with an epoch is no failover candidate (AR5), so stamped with
	// the primary it would keep a pass from failing the primary over, and
	// the test from seeing whether the stamp outlived the threshold.
	primarySet := []record{
		cntlrRecord("the primary", spCntlrPrimary),
		sliceRecord("the meta side", healthRecordSide, 0, spSideMeta),
		sliceRecord("the spare side", healthRecordSide, 0, spSideSpare),
		sliceRecord("the meta leg", healthRecordLeg, spLegMeta, 0),
	}
	standbySet := []record{cntlrRecord("the standby", spCntlrStandby)}
	records := append(append([]record(nil), primarySet...), standbySet...)

	// The owner's first verdicts: clean, and written, because a monitor that
	// has written nothing yet has an empty memo.
	for _, rec := range records {
		waitFor(t, "the owner's first verdict on "+rec.name, func() bool {
			return len(epochWrites(h.hw, rec.kind, rec.objId)) == 1
		})
	}
	// stampAndClear writes the other observer's epoch straight onto the
	// stored records and waits for the owner to put them right. The first
	// pass after the stamps loads them and hands them over, and the next
	// verdict on each object after that clears its record (HL3). Two rounds
	// see two passes, and the first has handed the stamps over once the
	// second has loaded — one pass runs at a time — while every object's
	// next round is due within one interval of that hand-over. Two more
	// seconds of clock let a round whose timer was armed a second or two
	// late still run, and stay short of a third pass.
	stampAndClear := func(set []record) uint64 {
		t.Helper()
		orphan := h.clk.nowUnix()
		for _, rec := range set {
			rec.stamp(orphan)
			if rec.stored() != orphan {
				t.Fatalf("the stamp on %s did not land", rec.name)
			}
		}
		round()
		round()
		for i := 0; i < 2; i++ {
			h.clk.advance(time.Second)
			time.Sleep(2 * time.Millisecond)
		}
		// A clear lands in the state before it is recorded, and the write
		// counts below read the record.
		for _, rec := range set {
			waitFor(t, "the owner's clear of "+rec.name, func() bool {
				return rec.stored() == 0 &&
					len(epochWrites(h.hw, rec.kind, rec.objId)) >= 2
			})
		}
		return orphan
	}
	orphan := stampAndClear(primarySet)
	stampAndClear(standbySet)
	for h.clk.nowUnix() < orphan+orphanPrimaryUnhealthy+10 {
		round()
	}

	if recs := h.logs.withMsg(msgReactionApplied); len(recs) != 0 {
		kinds := make([]any, 0, len(recs))
		for _, rec := range recs {
			kinds = append(kinds, rec["kind"])
		}
		t.Fatalf("reaction applied, kind %v: the healthy primary was "+
			"failed over on the orphaned epoch", kinds)
	}
	if calls := rops.allCalls(); len(calls) != 0 {
		t.Fatalf("reaction ops = %+v, want none", calls)
	}
	settled := make([]int, len(records))
	for idx, rec := range records {
		if got := rec.stored(); got != 0 {
			t.Fatalf("stored err_epoch of %s = %d, want the orphaned %d "+
				"cleared", rec.name, got, orphan)
		}
		// The clear is the owner's own write, and there is exactly one: an
		// offer whose load may predate the monitor's latest write is
		// dropped (offerRecord), so no pass hands the stamp back after the
		// clear and makes the owner clear it twice.
		writes := epochWrites(h.hw, rec.kind, rec.objId)
		if len(writes) != 2 {
			t.Fatalf("epoch writes of %s = %v, want the first verdict's "+
				"clear and the orphan's", rec.name, writes)
		}
		for _, epoch := range writes {
			if epoch != 0 {
				t.Fatalf("epoch writes of %s = %v, want clears only",
					rec.name, writes)
			}
		}
		settled[idx] = len(writes)
	}
	// And the memo is a cache, not a reason to write every round: once the
	// records agree, clean rounds and passes write nothing (HL3).
	for i := 0; i < 3; i++ {
		round()
	}
	for idx, rec := range records {
		got := epochWrites(h.hw, rec.kind, rec.objId)
		if len(got) != settled[idx] {
			t.Fatalf("epoch writes of %s = %v, want no write after the %d "+
				"that corrected the record", rec.name, got, settled[idx])
		}
	}
}

// TestSpDeletingKeepsChildren checks RW14: model.ErrNotFound means the SP is
// being deleted, so the coordinator logs and keeps its children until the
// SpRev delete arrives.
func TestSpDeletingKeepsChildren(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	w := h.start()
	waitFor(t, "first syncups", func() bool {
		return len(h.cntlrs[spCnA].syncups()) > 0
	})
	h.ops.setErr(model.ErrNotFound)
	w.update(desiredState{revision: testSpRev + 1, handle: testSpName})

	waitFor(t, "sp deleting", func() bool {
		return len(h.logs.withMsg(msgSpDeleting)) > 0
	})
	if got := len(h.logs.withMsg(msgRevisionWorkerStopped)); got != 0 {
		t.Fatalf("%d children stopped while the SP is being deleted", got)
	}
}

// TestSpMissingSubObjectsLogged checks MD3: a listed sub-object whose key is
// missing is reported in SpState.Missing and LOGGED BY THE CALLER. Nothing
// else in worker/ reads that field, and every one of those keys is a
// sub-object both agents converge on by tearing its resources down — without
// this record no log names the key that vanished.
func TestSpMissingSubObjectsLogged(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	state := spFixture()
	state.Missing = []string{
		model.SliceKey(testCid, testSpId, spSliceB),
		model.CntlrKey(testCid, testSpId, spCntlrDisabled),
	}
	h.ops.setState(state)
	h.start()

	waitFor(t, "the missing sub-objects record", func() bool {
		return len(h.logs.withMsg(msgSpSubObjectMissing)) > 0
	})
	record := h.logs.withMsg(msgSpSubObjectMissing)[0]
	keys, ok := record["keys"].([]any)
	if !ok || len(keys) != 2 {
		t.Fatalf("keys = %v, want both missing keys", record["keys"])
	}
	if keys[0] != state.Missing[0] || keys[1] != state.Missing[1] {
		t.Fatalf("keys = %v, want %v", keys, state.Missing)
	}
	if record["sp_name"] != testSpName {
		t.Fatalf("sp_name = %v", record["sp_name"])
	}
}

// TestSpCloneBitmapWiringCarriesThePair checks the clone half of BM1/BM2 end
// to end: the chunk value is read at the (src_slice_idx, bm_idx) key of §9.6,
// the PushCloneBitmap it becomes carries that same pair, and the agent's
// applied set is its chunk_id_list. bm_idx_list is a migration field — a clone
// that listed a bm_idx there has acknowledged nothing.
func TestSpCloneBitmapWiringCarriesThePair(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	for _, chunk := range spCloneChunks {
		h.store.seed(t,
			model.CloneBitmapKey(
				testCid, testSpId, spCloneNm, chunk.SliceIdx, chunk.Idx,
			),
			&pb.CloneBitmap{Bitmap: []byte{
				byte(chunk.SliceIdx), byte(chunk.Idx),
			}},
		)
	}
	// The primary holds (0, 0) and lists bm_idx 1 in the MIGRATION field: the
	// pair (2, 1) is the only chunk it has acknowledged nothing about.
	h.cntlrs[spCnA].syncupReply = func(
		req *pb.SyncupCntlrRequest,
	) *pb.SyncupCntlrReply {
		return &pb.SyncupCntlrReply{
			Revision: req.GetRevision(),
			BmInfoList: []*pb.BitmapInfo{{
				ResId:     spCloneId,
				BmIdxList: []uint32{1},
				ChunkIdList: []*pb.BmChunkId{
					{SrcSliceIdx: 0, BmIdx: 0},
				},
			}},
		}
	}
	h.start()

	waitFor(t, "the missing chunk pushed", func() bool {
		return len(h.cntlrs[spCnA].pushes()) > 0
	})
	push := h.cntlrs[spCnA].pushes()[0]
	if push.GetSrcSliceIdx() != 2 || push.GetBmIdx() != 1 {
		t.Fatalf("push = %v, want the missing chunk (2, 1)", push)
	}
	// The value of the (2, 1) key, i.e. the fetch keyed on the whole pair.
	if len(push.GetBitmap()) != 2 || push.GetBitmap()[0] != 2 ||
		push.GetBitmap()[1] != 1 {
		t.Fatalf("push carried %v, want the (2, 1) chunk's value",
			push.GetBitmap())
	}
	if push.GetCloneId() != spCloneId {
		t.Fatalf("push = %v", push)
	}
	for _, sent := range h.cntlrs[spCnA].pushes() {
		if sent.GetSrcSliceIdx() == 0 && sent.GetBmIdx() == 0 {
			t.Fatalf("the chunk_id_list-acknowledged (0, 0) was pushed again")
		}
	}
}

// TestSpLoadFailureRetriesOnTick checks RW14/RW12: a transient LoadSp failure
// is retried on the coordinator's own ticker, with no backoff of its own.
func TestSpLoadFailureRetriesOnTick(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	h.ops.setErr(errors.New("etcd unavailable"))
	h.start()
	waitFor(t, "load failure", func() bool {
		return len(h.logs.withMsg(msgSpLoadFailed)) > 0
	})
	h.ops.setErr(nil)
	h.advanceUntil("fan-out after the retry", roundInterval, func() bool {
		return len(h.cntlrs[spCnA].syncups()) > 0
	})
}

// ---------------------------------------------------------------------------
// [D12] — common.ReplyCodeLeftover is an ACCEPTED reply
// ---------------------------------------------------------------------------

// TestLeftoverCodeIsAccepted pins [D12]: a reply carrying
// common.ReplyCodeLeftover is an accepted request — the desired state is
// stored and every WANTED object converged; what is left over is something the
// desired state does not want, or an enumeration that did not answer, and the
// agent goes on sweeping either way. Its *Info rows are therefore a full probe
// of the wanted objects and are read exactly as code 0's, while the code alone
// re-drives the sweep by forcing a Syncup* every round (RW4 step 5).
//
// Every site that reads a reply's rows returned "no verdict" on ANY non-zero
// code before [D12]. Left that way, one leftover would have frozen health,
// bitmap pushes, RW19 td completion and the HL2 leg rows for as long as it
// survived — which is exactly as long as a dead remote's failfast window, i.e.
// exactly when they matter.
func TestLeftoverCodeIsAccepted(t *testing.T) {
	// (a) The five observation functions of HL1/HL2 read a leftover reply's
	// rows exactly as a code 0 reply's — and still return no verdict for the
	// three REJECTION codes, which is what keeps this from being the vacuous
	// "every code is accepted".
	t.Run("rows are evaluated as for code 0", func(t *testing.T) {
		legs := func(res *pb.ResInfo) *pb.CntlrInfo {
			return &pb.CntlrInfo{
				LegIdToLeg: map[uint64]*pb.ResInfo{spLegMeta: res},
			}
		}
		cases := []struct {
			name string
			run  func(code uint32) (healthObs, string)
		}{
			{"dn clean", func(code uint32) (healthObs, string) {
				return dnObservation(code, &pb.DnInfo{DiskInfo: resOk("disk")})
			}},
			{"dn error row", func(code uint32) (healthObs, string) {
				return dnObservation(code, &pb.DnInfo{
					DiskInfo: resErr("disk", "io error"),
				})
			}},
			{"cn error row", func(code uint32) (healthObs, string) {
				return cnObservation(code, &pb.CnInfo{
					PortInfo: resErr("port", "nvmet"),
				})
			}},
			{"cntlr clean", func(code uint32) (healthObs, string) {
				return cntlrObservation(code, &pb.CntlrInfo{
					GrpIdToMdRaid: map[uint64]*pb.ResInfo{1: resOk("md")},
				})
			}},
			{"cntlr error row", func(code uint32) (healthObs, string) {
				return cntlrObservation(code, &pb.CntlrInfo{
					GrpIdToMdRaid: map[uint64]*pb.ResInfo{
						1: resErr("md", "degraded"),
					},
				})
			}},
			{"side error row", func(code uint32) (healthObs, string) {
				return sideObservation(code, &pb.SideInfo{
					SideDevInfo: resErr("side-dev", "gone"),
				})
			}},
			{"leg ok row", func(code uint32) (healthObs, string) {
				return legObservation(code, legs(resOk("leg")), spLegMeta)
			}},
			{"leg error row", func(code uint32) (healthObs, string) {
				return legObservation(
					code, legs(resErr("leg", "probe io error")), spLegMeta)
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				wantObs, wantRes := tc.run(0)
				obs, res := tc.run(common.ReplyCodeLeftover)
				if obs != wantObs || res != wantRes {
					t.Fatalf("leftover gave (%v, %q), code 0 gave (%v, %q)",
						obs, res, wantObs, wantRes)
				}
				for _, code := range []uint32{
					common.ReplyCodeStaleRevision,
					common.ReplyCodeUnknownObject,
					common.ReplyCodeInvalidConf,
				} {
					if obs, _ := tc.run(code); obs != healthNone {
						t.Fatalf("code %d gave %v, want no verdict", code, obs)
					}
				}
			})
		}
	})

	// (b), (c) and (d) end to end through a real cntlr child: the reply's
	// revision MATCHES, so the code alone is what re-syncs, and nothing here
	// depends on a revision mismatch.
	t.Run("pushes, td completion, leg rows and the re-sync", func(t *testing.T) {
		h := newSpHarness(t)
		h.addFixtureAgents()
		for _, chunk := range spCloneChunks {
			h.store.seed(t,
				model.CloneBitmapKey(
					testCid, testSpId, spCloneNm, chunk.SliceIdx, chunk.Idx,
				),
				&pb.CloneBitmap{Bitmap: []byte{
					byte(chunk.SliceIdx), byte(chunk.Idx),
				}},
			)
		}
		const details = "leftover(1): c9:dnv-...-c9-..."
		var rounds atomic.Int64
		h.cntlrs[spCnA].checkReply = func(
			req *pb.CheckCntlrRequest,
		) *pb.CheckCntlrReply {
			rounds.Add(1)
			return &pb.CheckCntlrReply{
				Revision: req.GetRevision(),
				AgentReply: &pb.AgentReply{
					Code:    common.ReplyCodeLeftover,
					Details: details,
				},
				CntlrInfo: &pb.CntlrInfo{
					// Every slice of the uncreated td is complete (RW19).
					TdIdToThinInfo: map[uint64]*pb.CntlrInfo_ThinInfo{
						spTdOpen: {SliceIdToDmThin: map[uint64]*pb.ResInfo{
							spSliceA: resOk("thin-a"),
							spSliceB: resOk("thin-b"),
						}},
					},
					// ...and one leg is bad (HL2).
					LegIdToLeg: map[uint64]*pb.ResInfo{
						spLegMeta: resErr("leg-meta", "probe io error"),
					},
				},
			}
		}
		// The sweep's reply, with no bm_info_list: every chunk of the fixture
		// clone is missing, so a push is planned off a leftover reply (BM2).
		h.cntlrs[spCnA].syncupReply = func(
			req *pb.SyncupCntlrRequest,
		) *pb.SyncupCntlrReply {
			return &pb.SyncupCntlrReply{
				Revision: req.GetRevision(),
				AgentReply: &pb.AgentReply{
					Code:    common.ReplyCodeLeftover,
					Details: details,
				},
			}
		}
		h.start()

		// (b) pushes are still planned.
		waitFor(t, "a bitmap push off a leftover reply", func() bool {
			return len(h.cntlrs[spCnA].pushes()) > 0
		})
		// (c) RW19 td completion and the HL2 leg rows are still reported.
		waitFor(t, "the RW19 created flip", func() bool {
			return len(h.ops.createdCalls()) > 0
		})
		if calls := h.ops.createdCalls(); len(calls[0]) != 1 ||
			calls[0][0].TdId != spTdOpen {
			t.Fatalf("flip batch = %v, want the uncreated td",
				h.ops.createdCalls()[0])
		}
		waitFor(t, "the HL2 leg row", func() bool {
			for _, write := range h.hw.all() {
				if write.record == healthRecordLeg &&
					write.objId == spLegMeta && write.epoch != 0 {
					return true
				}
			}
			return false
		})
		// (d) and the code re-syncs every round although the revision matches.
		h.advanceUntil("three more check rounds", roundInterval, func() bool {
			return rounds.Load() >= 4
		})
		if got := len(h.cntlrs[spCnA].syncups()); got < 3 {
			t.Fatalf("%d SyncupCntlr calls over %d rounds, want one per "+
				"round while the leftover lasts",
				got, rounds.Load())
		}
		// It is an accepted reply, so it is never logged as a rejection.
		if got := len(h.logs.withMsg(msgSyncupRejected)); got != 0 {
			t.Fatalf("%d 'syncup rejected' records for a leftover reply", got)
		}
		waitFor(t, "the leftover record", func() bool {
			return len(h.logs.withMsg(msgSyncupLeftover)) > 0
		})
	})
}
