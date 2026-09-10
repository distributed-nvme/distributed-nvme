package worker

import (
	"context"
	"errors"
	"io"
	"sync"
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
		Conf: &pb.SpConf{
			SpId:      testSpId,
			ShardCode: testShard,
			BdevConf: &pb.BdevConf{
				// data_block_size stays 0 so RW15's default resolution is
				// exercised.
				DmPoolConf:  &pb.DmPoolConf{LowWaterMarkPct: 50},
				DmRaid0Conf: &pb.DmRaid0Conf{StripeSize: 65536},
				RedundConf: &pb.RedundConf{
					RedunKind: &pb.RedundConf_RedundMdRaid1{
						RedundMdRaid1: &pb.RedundMdRaid1{
							BitmapChunkBlockCnt: 128,
						},
					},
				},
			},
			SpLevel: pb.SpLevel_SP_LEVEL_READWRITE,
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
			spCloneNm: {CloneId: spCloneId, BmCnt: 2},
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
			spCloneNm: {{Idx: 0, ModRev: 11}, {Idx: 1, ModRev: 12}},
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
// migration source / destination confs of the two-sided leg, whose zero-valued
// knobs carry their §7 defaults.
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
			BlockSize:     common.DefaultDmPoolDataBlockSize,
			MetaBlocks:    5,
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
	// BM1/BM4: only the primary pushes clone chunks.
	if len(primary.clones) != 1 || primary.clones[0].id != spCloneId {
		t.Fatalf("primary clone plan = %+v", primary.clones)
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
	s.mu.Unlock()
	return &pb.PushCloneBitmapReply{}, nil
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
	mu          sync.Mutex
	state       *model.SpState
	err         error
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
	if o.err != nil {
		return nil, o.err
	}
	return o.state, nil
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
	d.conf.mu.Lock()
	d.conf.entries[testCid] = model.ResolveClusterConf(
		&pb.ClusterConf{CreationEpoch: 1},
	)
	d.conf.mu.Unlock()
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
