package worker

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The clone meta group in the sp role (RW14, RW15, RW18, HL2, HL3, AR10)
// ---------------------------------------------------------------------------

const (
	// The fixture clone's clone meta group (withCloneGrp): two legs of an
	// md-raid1, one provisioned and one not, and a spare, on three of the
	// fixture's DNs.
	spCloneGrp       = uint64(120)
	spCloneLegA      = uint64(1020)
	spCloneLegB      = uint64(1021)
	spCloneLegSpare  = uint64(1022)
	spCloneSideA     = uint64(2020)
	spCloneSideB     = uint64(2021)
	spCloneSideSpare = uint64(2022)

	// A second clone, being deleted, with a group of one leg
	// (withDeletingClone).
	spCloneGoneNm   = "clone1"
	spCloneGoneId   = uint64(401)
	spCloneGoneGrp  = uint64(121)
	spCloneGoneLeg  = uint64(1030)
	spCloneGoneSide = uint64(2030)
)

// spCloneZeroBytes is the length to zero RW15 gives every side of the
// fixture's clone meta groups: the data-group length, its meta region and its
// first data block (model.CloneMetaSideZeroBytes; architecture.md, Side
// provisioning protocol), and never its whole leg span, (7 + 1017) blocks,
// which "meta" in the group's name might suggest. The counts are hand-chosen,
// as spFixture's are, and differ from every slice group's.
const spCloneZeroBytes = (7 + 1) * testBlockSize

// cloneMetaGrp is a clone meta group of one extent per side with the
// fixture's block counts.
func cloneMetaGrp(grpId uint64, legs []*pb.Leg, spares []*pb.Leg) *pb.Group {
	return &pb.Group{
		GrpId:        grpId,
		ExtCnt:       1,
		MetaBlocks:   7,
		DataBlocks:   1017,
		LegList:      legs,
		SpareLegList: spares,
	}
}

// cloneLeg is a leg of one side.
func cloneLeg(
	legId uint64,
	sideId uint64,
	addrPort string,
	provisioned bool,
) *pb.Leg {
	return &pb.Leg{
		LegId: legId,
		SideList: []*pb.Side{{
			SideId:      sideId,
			AddrPort:    addrPort,
			CntlidSlot:  10000,
			Provisioned: provisioned,
		}},
	}
}

// withCloneGrp gives the fixture's clone, which spFixture leaves with no
// group, a clone meta group. spFixture itself stays as it is: its cases count
// the slices' side children.
func withCloneGrp(state *model.SpState) {
	state.Clones[spCloneNm].MetaGrp = cloneMetaGrp(spCloneGrp,
		[]*pb.Leg{
			cloneLeg(spCloneLegA, spCloneSideA, spDnB, true),
			cloneLeg(spCloneLegB, spCloneSideB, spDnC, false),
		},
		[]*pb.Leg{
			cloneLeg(spCloneLegSpare, spCloneSideSpare, spDnD, false),
		},
	)
}

// withDeletingClone adds a second clone, latched by DeleteClone, whose group
// still holds its one unprovisioned side: the clone drain's final
// transaction has not run yet (CLD9).
func withDeletingClone(state *model.SpState) {
	state.Conf.CloneNameList = append(
		state.Conf.CloneNameList, spCloneGoneNm,
	)
	state.Clones[spCloneGoneNm] = &pb.Clone{
		CloneId:  spCloneGoneId,
		Deleting: true,
		MetaGrp: cloneMetaGrp(spCloneGoneGrp,
			[]*pb.Leg{
				cloneLeg(spCloneGoneLeg, spCloneGoneSide, spDnA, false),
			},
			nil,
		),
	}
}

// cloneSideKey is the side child key of a side of the fixture's clone.
func cloneSideKey(legId uint64, sideId uint64) sideKey {
	return sideKey{legId: legId, sideId: sideId}
}

// TestSpCloneSideRequestGolden pins RW14 and RW15 on the sides of clone meta
// groups: every side of a clone's group — the spare's and a deleting clone's
// included — gets a plan and a request of the same shape as a slice's side,
// its group's ext_cnt and length to zero, the primary's cn_id, the standby
// list, the level and the side's provisioned flag, and no migration conf;
// the plan names the side's clone, by name and id, where a slice's names its
// slice, and carries the side's err_epoch for the sides-first hold. Every
// leg of a group is mapped to its clone for the leg rows (HL2). A clone with
// no group, and a listed clone the load did not find, add nothing, and the
// slices' sides are planned as before.
func TestSpCloneSideRequestGolden(t *testing.T) {
	captureLogs(t)
	state := spFixture()
	withCloneGrp(state)
	withDeletingClone(state)
	state.Clones[spCloneNm].MetaGrp.LegList[1].SideList[0].ErrEpoch = 77
	// A clone with no group, and one the load lost (MD3).
	state.Conf.CloneNameList = append(state.Conf.CloneNameList,
		"bare", "lost")
	state.Clones["bare"] = &pb.Clone{CloneId: 402}
	plan := spTestWorker(nil).buildPlan(context.Background(), state)

	if len(plan.sides) != 5+3+1 {
		t.Fatalf("%d side plans, want the slices' 5, the clone's 3 and "+
			"the deleting clone's 1", len(plan.sides))
	}
	standby := []uint64{spCnIdB, spCnIdC}
	want := &pb.SyncupSideRequest{
		ClusterId: testCid,
		DnId:      spDnIdB,
		SidePointer: &pb.SidePointer{
			SpId:   testSpId,
			LegId:  spCloneLegA,
			SideId: spCloneSideA,
		},
		Revision: testSpRev,
		SideConf: &pb.SyncupSideRequest_SideConf{
			ExtCnt:        1,
			CntlidSlot:    10000,
			PrimaryCnId:   spCnIdA,
			StandbyIdList: standby,
			SpLevel:       pb.SpLevel_SP_LEVEL_READWRITE,
			Provisioned:   true,
			ZeroBytes:     spCloneZeroBytes,
		},
	}
	a := plan.sides[cloneSideKey(spCloneLegA, spCloneSideA)]
	if a == nil {
		t.Fatalf("no plan for the clone's first side")
	}
	if !proto.Equal(a.req, want) {
		t.Fatalf("clone side request =\n%v\nwant\n%v", a.req, want)
	}
	for _, tc := range []struct {
		legId       uint64
		sideId      uint64
		cloneName   string
		cloneId     uint64
		dnId        uint64
		provisioned bool
		deleting    bool
		errEpoch    uint64
	}{
		{spCloneLegA, spCloneSideA, spCloneNm, spCloneId, spDnIdB, true,
			false, 0},
		{spCloneLegB, spCloneSideB, spCloneNm, spCloneId, spDnIdC, false,
			false, 77},
		{spCloneLegSpare, spCloneSideSpare, spCloneNm, spCloneId, spDnIdD,
			false, false, 0},
		{spCloneGoneLeg, spCloneGoneSide, spCloneGoneNm, spCloneGoneId,
			spDnIdA, false, true, 0},
	} {
		p := plan.sides[cloneSideKey(tc.legId, tc.sideId)]
		if p == nil {
			t.Fatalf("no plan for side %d of clone %s", tc.sideId,
				tc.cloneName)
		}
		ref := model.CloneSideRef{
			CloneName: tc.cloneName,
			CloneId:   tc.cloneId,
			LegId:     tc.legId,
			SideId:    tc.sideId,
		}
		if p.clone == nil || *p.clone != ref {
			t.Errorf("side %d: clone = %+v, want %+v", tc.sideId,
				p.clone, ref)
		}
		if p.ref != (model.SideRef{}) {
			t.Errorf("side %d of a clone names a slice: %+v", tc.sideId,
				p.ref)
		}
		if p.cloneDeleting != tc.deleting {
			t.Errorf("side %d: cloneDeleting = %v, want %v", tc.sideId,
				p.cloneDeleting, tc.deleting)
		}
		if p.errEpoch != tc.errEpoch {
			t.Errorf("side %d: errEpoch = %d, want %d", tc.sideId,
				p.errEpoch, tc.errEpoch)
		}
		conf := p.req.GetSideConf()
		if p.req.GetDnId() != tc.dnId || p.dnId != tc.dnId ||
			conf.GetProvisioned() != tc.provisioned ||
			conf.GetExtCnt() != 1 ||
			conf.GetZeroBytes() != spCloneZeroBytes ||
			conf.GetPrimaryCnId() != spCnIdA ||
			!slices.Equal(conf.GetStandbyIdList(), standby) {
			t.Errorf("side %d: request = %v", tc.sideId, p.req)
		}
		if p.req.GetMigrSrcConf() != nil || p.req.GetMigrDstConf() != nil ||
			p.migrName != "" {
			t.Errorf("side %d of a clone carries a migration: %v",
				tc.sideId, p.req)
		}
		owner := cloneOwner{name: tc.cloneName, id: tc.cloneId}
		if plan.legClone[tc.legId] != owner {
			t.Errorf("leg %d maps to %+v, want %+v", tc.legId,
				plan.legClone[tc.legId], owner)
		}
		if _, ok := plan.legSlice[tc.legId]; ok {
			t.Errorf("leg %d of a clone maps to a slice", tc.legId)
		}
	}
	if len(plan.legClone) != 4 {
		t.Fatalf("legClone = %v, want the four legs of the two groups",
			plan.legClone)
	}
	for _, legId := range []uint64{spLegMeta, spLegMigr, spLegSpare, spLegB} {
		if _, ok := plan.legSlice[legId]; !ok {
			t.Fatalf("the slices' leg %d lost its slice", legId)
		}
	}
}

// TestSpCloneSideZeroBytes pins RW15's length to zero on the sides of a clone
// meta group: the data-group length on every side, the active legs' and the
// spare's, provisioned or not, with md-raid1 and without, never the whole leg
// span of a slice's meta group. A group whose stored counts give no length
// drives none of its sides, while its legs keep their clone for the leg rows
// and nothing is counted for re-resolution.
func TestSpCloneSideZeroBytes(t *testing.T) {
	captureLogs(t)
	slicesAt := func(zeroBytes map[sideKey]uint64) map[sideKey]uint64 {
		want := map[sideKey]uint64{
			{legId: spLegMeta, sideId: spSideMeta}:   spMetaZeroBytes,
			{legId: spLegMigr, sideId: spSideSrc}:    spDataZeroBytes,
			{legId: spLegMigr, sideId: spSideDst}:    spDataZeroBytes,
			{legId: spLegSpare, sideId: spSideSpare}: spDataZeroBytes,
			{legId: spLegB, sideId: spSideB}:         spDataZeroBytes,
		}
		for key, bytes := range zeroBytes {
			want[key] = bytes
		}
		return want
	}
	t.Run("md-raid1", func(t *testing.T) {
		state := spFixture()
		withCloneGrp(state)
		plan := spTestWorker(nil).buildPlan(context.Background(), state)
		wantZeroBytes(t, plan, slicesAt(map[sideKey]uint64{
			cloneSideKey(spCloneLegA, spCloneSideA):         spCloneZeroBytes,
			cloneSideKey(spCloneLegB, spCloneSideB):         spCloneZeroBytes,
			cloneSideKey(spCloneLegSpare, spCloneSideSpare): spCloneZeroBytes,
		}))
	})
	t.Run("redund_none", func(t *testing.T) {
		// One leg, one meta block (the health block), no spare: the shape
		// a pool without redundancy gives every group.
		state := spFixture()
		state.Clones[spCloneNm].MetaGrp = &pb.Group{
			GrpId: spCloneGrp, ExtCnt: 1, MetaBlocks: 1, DataBlocks: 1023,
			LegList: []*pb.Leg{
				cloneLeg(spCloneLegA, spCloneSideA, spDnB, false),
			},
		}
		plan := spTestWorker(nil).buildPlan(context.Background(), state)
		wantZeroBytes(t, plan, slicesAt(map[sideKey]uint64{
			cloneSideKey(spCloneLegA, spCloneSideA): 2 * testBlockSize,
		}))
	})
	t.Run("a group with no length", func(t *testing.T) {
		state := spFixture()
		withCloneGrp(state)
		state.Clones[spCloneNm].MetaGrp.MetaBlocks = 0
		plan := spTestWorker(nil).buildPlan(context.Background(), state)
		wantZeroBytes(t, plan, slicesAt(nil))
		want := cloneOwner{name: spCloneNm, id: spCloneId}
		for _, legId := range []uint64{
			spCloneLegA, spCloneLegB, spCloneLegSpare,
		} {
			if plan.legClone[legId] != want {
				t.Errorf("leg %d maps to %+v", legId, plan.legClone[legId])
			}
		}
		if plan.unresolved != 0 {
			t.Errorf("unresolved = %d, want 0", plan.unresolved)
		}
	})
}

// TestCheckSideZeroBytesWalksTheCloneGroups pins the clone half of the group
// gate (RW14, AR1): a clone meta group that gives its sides a length passes,
// a clone with no group and a listed clone the load did not find pass, a
// group with no length is refused with an error naming it, and the slices'
// groups are walked first, so with a bad group in each the slice's is named.
func TestCheckSideZeroBytesWalksTheCloneGroups(t *testing.T) {
	state := spFixture()
	withCloneGrp(state)
	withDeletingClone(state)
	state.Conf.CloneNameList = append(state.Conf.CloneNameList,
		"bare", "lost")
	state.Clones["bare"] = &pb.Clone{CloneId: 402}
	if err := checkSideZeroBytes(state); err != nil {
		t.Fatalf("a usable state is refused: %v", err)
	}
	state.Clones[spCloneNm].MetaGrp.DataBlocks = 0
	want := fmt.Sprintf("group %d data_blocks is zero", spCloneGrp)
	if err := checkSideZeroBytes(state); err == nil ||
		!strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %q named", err, want)
	}
	noMetaBlocks(state)
	if err := checkSideZeroBytes(state); err == nil ||
		!strings.Contains(err.Error(), "group 101 meta_blocks is zero") {
		t.Fatalf("error = %v, want the slice's group named first", err)
	}
}

// TestSpUnresolvedCloneSideLeavesChildIdle checks RW14 on a side of a clone
// meta group: a side whose DN has no DnConf gets no request and is counted
// for re-resolution, with one record naming its clone, and every other side
// is still driven.
func TestSpUnresolvedCloneSideLeavesChildIdle(t *testing.T) {
	logs := captureLogs(t)
	state := spFixture()
	withCloneGrp(state)
	const unknown = "spdn9:9520"
	state.Clones[spCloneNm].MetaGrp.SpareLegList[0].SideList[0].
		AddrPort = unknown
	plan := spTestWorker(nil).buildPlan(context.Background(), state)
	if _, ok := plan.sides[cloneSideKey(
		spCloneLegSpare, spCloneSideSpare,
	)]; ok {
		t.Fatalf("a clone's side with no DnConf was given a request")
	}
	if len(plan.sides) != 5+2 {
		t.Fatalf("%d side plans, want the other seven", len(plan.sides))
	}
	if plan.unresolved != 1 {
		t.Fatalf("unresolved = %d, want 1", plan.unresolved)
	}
	recs := logs.withMsg(msgSpChildUnresolved)
	if len(recs) != 1 || recs[0]["addr_port"] != unknown ||
		recs[0]["clone_name"] != spCloneNm {
		t.Fatalf("unresolved records = %v, want one naming the clone", recs)
	}
}

// TestReportProvisionedOfACloneSide pins RW18's report on the side of a clone
// meta group: done, it is handed to the coordinator as a clone flip
// candidate naming its clone, never as a slice's; provisioned already, or of
// a clone being deleted, which the clone flip would skip (MD6), it is not
// handed over at all.
func TestReportProvisionedOfACloneSide(t *testing.T) {
	ref := model.CloneSideRef{
		CloneName: spCloneNm, CloneId: spCloneId,
		LegId: spCloneLegB, SideId: spCloneSideB,
	}
	done := &pb.SideInfo{
		ZeroedBytes: spCloneZeroBytes, ZeroBytes: spCloneZeroBytes,
	}
	cases := []struct {
		name        string
		provisioned bool
		deleting    bool
		want        bool
	}{
		{"done", false, false, true},
		{"already provisioned", true, false, false},
		{"of a clone being deleted", false, true, false},
	}
	for _, tc := range cases {
		d := &sideDriver{report: make(chan spReport, 1)}
		d.storeInfo(done)
		clone := ref
		plan := &sidePlan{
			clone:         &clone,
			cloneDeleting: tc.deleting,
			req: &pb.SyncupSideRequest{
				SideConf: &pb.SyncupSideRequest_SideConf{
					Provisioned: tc.provisioned,
					ZeroBytes:   spCloneZeroBytes,
				},
			},
		}
		d.reportProvisioned(context.Background(), &replyState{}, plan)
		if got := len(d.report) == 1; got != tc.want {
			t.Errorf("%s: reported %v, want %v", tc.name, got, tc.want)
			continue
		}
		if !tc.want {
			continue
		}
		rep := <-d.report
		if rep.provisioned != nil {
			t.Errorf("%s: a clone's side reported as a slice's: %+v",
				tc.name, rep.provisioned)
		}
		if rep.cloneProvisioned == nil || *rep.cloneProvisioned != ref {
			t.Errorf("%s: report = %+v, want the side %+v", tc.name,
				rep.cloneProvisioned, ref)
		}
	}
}

// TestSpCloneProvisionedFlipReported is RW18's clone flip end to end: a side
// of a clone meta group whose request carried provisioned false and whose
// agent reports its zeroing done is reported to the coordinator, which runs
// FlipCloneProvisioned with the side's clone; a provisioned side reporting
// the same, a spare still zeroing and the side of a clone being deleted are
// not, and no side of a clone reaches the slice flip.
func TestSpCloneProvisionedFlipReported(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newSpHarness(t)
		h.addFixtureAgents()
		state := spFixture()
		withCloneGrp(state)
		withDeletingClone(state)
		h.ops.setState(state)
		done := func() *pb.SideInfo {
			return &pb.SideInfo{
				ZeroedBytes: spCloneZeroBytes, ZeroBytes: spCloneZeroBytes,
			}
		}
		infos := map[uint64]func() *pb.SideInfo{
			spCloneSideB: done,
			spCloneSideA: done,
			spCloneSideSpare: func() *pb.SideInfo {
				return &pb.SideInfo{
					ZeroedBytes: 4 << 20, ZeroBytes: spCloneZeroBytes,
				}
			},
			spCloneGoneSide: done,
		}
		var mu sync.Mutex
		rounds := make(map[uint64]int)
		reply := func(req *pb.CheckSideRequest) *pb.CheckSideReply {
			sideId := req.GetSidePointer().GetSideId()
			mu.Lock()
			rounds[sideId]++
			mu.Unlock()
			// The slices' sides report no length, so none is done.
			info := &pb.SideInfo{}
			if build, ok := infos[sideId]; ok {
				info = build()
			}
			return &pb.CheckSideReply{
				Revision: req.GetRevision(),
				SideInfo: info,
			}
		}
		for _, addr := range []string{spDnA, spDnB, spDnC, spDnD} {
			h.sides[addr].checkReply = reply
		}
		// The deleting clone's drain runs in every pass (CLD7), on a model
		// surface that must not reach the harness's nil etcd client.
		h.reactWith(h.start(), &fakeReactionOps{})

		h.advanceUntil("the clone flip and a second round of every "+
			"other clone side", roundInterval, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(h.ops.cloneProvisionedCalls()) > 0 &&
				rounds[spCloneSideA] > 1 && rounds[spCloneSideSpare] > 1 &&
				rounds[spCloneGoneSide] > 1
		})
		want := model.CloneSideRef{
			CloneName: spCloneNm, CloneId: spCloneId,
			LegId: spCloneLegB, SideId: spCloneSideB,
		}
		for _, call := range h.ops.cloneProvisionedCalls() {
			for _, ref := range call {
				if ref != want {
					t.Fatalf("clone flip of %+v, want only %+v", ref, want)
				}
			}
		}
		if calls := h.ops.provisionedCalls(); len(calls) != 0 {
			t.Fatalf("slice flips %v, want none", calls)
		}
	})
}

// TestSpCloneFlipRecordsOnlyAppliedRefs pins the clone flip's share of the
// "flip applied" record (dnv-worker.md, Log records; RW18): the sides of
// clone meta groups one drain reports share one FlipCloneProvisioned call,
// and each side the STM ACTUALLY wrote gets one record, of kind provisioned,
// naming its clone, its leg and side and the revision the STM committed —
// none for a side the STM skipped, as another owner's flip or a clone gone
// or being deleted makes it.
func TestSpCloneFlipRecordsOnlyAppliedRefs(t *testing.T) {
	h := newSpHarness(t)
	h.ops.provisionedRev = testSpRev + 1
	w := spTestWorker(h.deps)
	w.ops = h.ops
	applied := model.CloneSideRef{
		CloneName: spCloneNm, CloneId: spCloneId,
		LegId: spCloneLegB, SideId: spCloneSideB,
	}
	skipped := model.CloneSideRef{
		CloneName: spCloneNm, CloneId: spCloneId,
		LegId: spCloneLegSpare, SideId: spCloneSideSpare,
	}
	h.ops.applyCloneSides = map[model.CloneSideRef]bool{applied: true}
	w.reportCh <- spReport{cloneProvisioned: &skipped}
	w.reportCh <- spReport{cloneProvisioned: &applied}
	w.handleReports(spReport{cloneProvisioned: &applied})

	calls := h.ops.cloneProvisionedCalls()
	if len(calls) != 1 || len(calls[0]) != 2 {
		t.Fatalf("clone flip calls = %v, want one STM of the two "+
			"distinct sides", calls)
	}
	if got := h.ops.provisionedCalls(); len(got) != 0 {
		t.Fatalf("slice flip calls = %v, want none", got)
	}
	records := h.logs.withMsg(msgFlipApplied)
	if len(records) != 1 {
		t.Fatalf("%d flip applied records, want one per WRITTEN side: %v",
			len(records), records)
	}
	rec := records[0]
	if rec["kind"] != flipKindProvisioned ||
		rec["clone_name"] != spCloneNm ||
		rec["clone_id"] != float64(spCloneId) ||
		rec["leg_id"] != float64(spCloneLegB) ||
		rec["side_id"] != float64(spCloneSideB) ||
		rec["revision"] != float64(testSpRev+1) {
		t.Fatalf("record = %v, want the written side, its clone and the "+
			"revision the STM committed", rec)
	}
	if _, ok := rec["slice_id"]; ok {
		t.Fatalf("a clone's side record names a slice: %v", rec)
	}
}

// TestSpFlipOrder pins the order of what one drain of reports runs (RW18,
// RW19): the slice flip, then the clone flip, each an STM of its own, then
// the cutover beside them, then the created flip.
func TestSpFlipOrder(t *testing.T) {
	w, ops, _ := cutoverWorker(t, true, testSpRev+1)
	w.tdRefs = map[uint64]model.TdRef{
		spTdOpen: {Name: "td0", TdId: spTdOpen},
	}
	slice := model.SideRef{
		SliceId: spSliceA, LegId: spLegSpare, SideId: spSideSpare,
	}
	clone := model.CloneSideRef{
		CloneName: spCloneNm, CloneId: spCloneId,
		LegId: spCloneLegB, SideId: spCloneSideB,
	}
	w.reportCh <- spReport{cloneProvisioned: &clone}
	w.reportCh <- spReport{
		cntlrId: spCntlrPrimary,
		paths:   livePaths(testSpRev, spSideSrc, spSideDst),
	}
	w.reportCh <- spReport{createdTdIds: []uint64{spTdOpen}}
	w.handleReports(spReport{provisioned: &slice})
	want := []string{"provisioned", "clone_provisioned", "cutover", "created"}
	if got := ops.callOrder(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

// TestSpCloneLegRowsFromPrimaryOnly is HL2's leg row on the legs of a clone
// meta group: the primary's ERROR row of a clone's leg sets the leg's
// err_epoch through the clone-keyed write, naming the clone, never a slice;
// a standby's row is logged, never recorded.
func TestSpCloneLegRowsFromPrimaryOnly(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	state := spFixture()
	withCloneGrp(state)
	h.ops.setState(state)
	h.cntlrs[spCnA].checkReply = func(
		req *pb.CheckCntlrRequest,
	) *pb.CheckCntlrReply {
		return &pb.CheckCntlrReply{
			Revision: req.GetRevision(),
			CntlrInfo: &pb.CntlrInfo{
				LegIdToLeg: map[uint64]*pb.ResInfo{
					spCloneLegB:     resErr("clone-leg-b", "probe io error"),
					spCloneLegSpare: resOk("clone-leg-spare"),
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
					spCloneLegSpare: resErr("clone-leg-spare", "ana"),
				},
			},
		}
	}
	h.start()

	waitFor(t, "the clone's leg health recorded", func() bool {
		for _, write := range h.hw.all() {
			if write.record == healthRecordLeg &&
				write.objId == spCloneLegB && write.epoch != 0 {
				return true
			}
		}
		return false
	})
	for _, write := range h.hw.all() {
		if write.record != healthRecordLeg {
			continue
		}
		if write.objId == spCloneLegSpare && write.epoch != 0 {
			t.Fatalf("a standby's leg row was recorded: %+v", write)
		}
		if write.objId == spCloneLegB &&
			(write.cloneName != spCloneNm || write.cloneId != spCloneId ||
				write.sliceId != 0) {
			t.Fatalf("clone leg written as %+v, want on clone %s (%d)",
				write, spCloneNm, spCloneId)
		}
	}
	waitFor(t, "the standby's leg row logged", func() bool {
		return len(h.logs.withMsg(msgSpStandbyLegRow)) > 0
	})
}

// TestSpCloneSideHealthWritesTheClone is HL2's side row on a side of a clone
// meta group: the side child's monitor writes the side's err_epoch through
// the clone-keyed write, naming the clone, never a slice.
func TestSpCloneSideHealthWritesTheClone(t *testing.T) {
	h := newSpHarness(t)
	h.addFixtureAgents()
	state := spFixture()
	withCloneGrp(state)
	h.ops.setState(state)
	h.sides[spDnC].syncupReply = func(
		req *pb.SyncupSideRequest,
	) *pb.SyncupSideReply {
		info := &pb.SideInfo{SideDevInfo: resOk("side")}
		if req.GetSidePointer().GetSideId() == spCloneSideB {
			info.SideDevInfo = resErr("side", "io")
		}
		return &pb.SyncupSideReply{
			Revision: req.GetRevision(), SideInfo: info,
		}
	}
	h.start()

	waitFor(t, "the clone side's health recorded", func() bool {
		for _, write := range h.hw.all() {
			if write.record == healthRecordSide &&
				write.objId == spCloneSideB && write.epoch != 0 {
				return true
			}
		}
		return false
	})
	for _, write := range h.hw.all() {
		if write.record != healthRecordSide {
			continue
		}
		clone := slices.Contains([]uint64{
			spCloneSideA, spCloneSideB, spCloneSideSpare,
		}, write.objId)
		if clone && (write.cloneName != spCloneNm ||
			write.cloneId != spCloneId || write.sliceId != 0) {
			t.Fatalf("clone side written as %+v, want on clone %s (%d)",
				write, spCloneNm, spCloneId)
		}
		if !clone && write.cloneName != "" {
			t.Fatalf("a slice's side written on a clone: %+v", write)
		}
	}
}

// TestSpApplyPlanKeepsCloneLegMonitors pins the coordinator's leg monitors
// against the fan-out's diff (HL2): a monitor of a leg of a clone meta group
// survives the fan-outs that still list the leg, as a slice's leg's does, and
// goes with one that does not; a leg row of a clone's leg then writes through
// the clone-keyed write, and one of a leg no plan lists writes nothing.
func TestSpApplyPlanKeepsCloneLegMonitors(t *testing.T) {
	captureLogs(t)
	d := newTestDeps(
		testConfig(common.WorkerRoleSp), newFakeStore(), newFakeClock(),
	)
	hw := &fakeHealthWriter{}
	d.health = hw
	w := spTestWorker(d)
	owner := cloneOwner{name: spCloneNm, id: spCloneId}
	plan := func(clone bool) *spPlan {
		p := &spPlan{
			sides:    make(map[sideKey]*sidePlan),
			cntlrs:   make(map[uint64]*cntlrPlan),
			legSlice: map[uint64]uint64{spLegMeta: spSliceA},
			legClone: make(map[uint64]cloneOwner),
			tdRefs:   make(map[uint64]model.TdRef),
		}
		if clone {
			p.legClone[spCloneLegB] = owner
		}
		return p
	}
	ctx := context.Background()
	w.applyPlan(ctx, plan(true))
	w.observeLeg(ctx, legRow{
		legId: spCloneLegB, obs: healthErrorRow, resName: "leg",
	})
	w.observeLeg(ctx, legRow{
		legId: 9999, obs: healthErrorRow, resName: "leg",
	})
	writes := hw.all()
	if len(writes) != 1 || writes[0].objId != spCloneLegB ||
		writes[0].cloneName != spCloneNm || writes[0].cloneId != spCloneId ||
		writes[0].epoch == 0 {
		t.Fatalf("writes = %+v, want the clone's leg set on its clone",
			writes)
	}
	monitor := w.legs[spCloneLegB]
	w.applyPlan(ctx, plan(true))
	if w.legs[spCloneLegB] != monitor {
		t.Fatalf("a fan-out that still lists the clone's leg dropped its " +
			"monitor")
	}
	w.applyPlan(ctx, plan(false))
	if _, ok := w.legs[spCloneLegB]; ok {
		t.Fatalf("a fan-out that no longer lists the clone's leg kept its " +
			"monitor")
	}
}

// TestHealthCloneMonitorsWriteThroughTheTwins pins the HL2 monitors of a leg
// and a side of a clone meta group: their writes go through the clone-keyed
// twins (MD6), naming the clone by its name and id and no slice, a set and a
// clear alike, and their records name the clone.
func TestHealthCloneMonitorsWriteThroughTheTwins(t *testing.T) {
	logs := captureLogs(t)
	d, writer := healthTestDeps(t)
	ctx := context.Background()
	leg := newCloneLegMonitor(d, 1, 2, "c0", 9, 5)
	side := newCloneSideMonitor(d, 1, 2, "c0", 9, 6)
	leg.observe(ctx, healthErrorRow, "leg")
	side.observe(ctx, healthErrorRow, "side")
	leg.observe(ctx, healthClean, "")
	side.observe(ctx, healthClean, "")

	writes := writer.all()
	if len(writes) != 4 {
		t.Fatalf("writes = %+v, want a set and a clear of each", writes)
	}
	for i, want := range []struct {
		record string
		objId  uint64
		set    bool
	}{
		{healthRecordLeg, 5, true},
		{healthRecordSide, 6, true},
		{healthRecordLeg, 5, false},
		{healthRecordSide, 6, false},
	} {
		got := writes[i]
		if got.record != want.record || got.objId != want.objId ||
			got.cloneName != "c0" || got.cloneId != 9 || got.sliceId != 0 ||
			got.spId != 2 || got.cid != 1 || (got.epoch != 0) != want.set {
			t.Fatalf("write %d = %+v, want %s %d on clone c0 (9), set %v",
				i, got, want.record, want.objId, want.set)
		}
	}
	for _, rec := range logs.withMsg(msgHealthChanged) {
		if rec["clone_name"] != "c0" || rec["clone_id"] != float64(9) {
			t.Fatalf("record without its clone: %v", rec)
		}
		if _, ok := rec["slice_id"]; ok {
			t.Fatalf("a clone's record names a slice: %v", rec)
		}
	}
}

// TestVerdictMemoKeepsCloneLegsAndSides pins AR10's memo against the clone
// meta groups: a verdict on a leg or a side of a clone's group is kept while
// the pass's snapshot lists the group, a deleting clone's included, and
// dropped once the snapshot no longer does, the clone drain having removed
// the clone.
func TestVerdictMemoKeepsCloneLegsAndSides(t *testing.T) {
	state := spFixture()
	withCloneGrp(state)
	withDeletingClone(state)
	memo := newVerdictMemo()
	sideB := cloneSideKey(spCloneLegB, spCloneSideB)
	gone := cloneSideKey(spCloneGoneLeg, spCloneGoneSide)
	memo.noteLeg(spCloneLegB, true)
	memo.noteLeg(spCloneGoneLeg, true)
	memo.noteSide(sideB, true)
	memo.noteSide(gone, true)
	got := memo.snapshot(state)
	if !got.legs[spCloneLegB] || !got.legs[spCloneGoneLeg] ||
		!got.sides[sideB] || !got.sides[gone] {
		t.Fatalf("snapshot %+v lost a listed clone leg's or side's verdict",
			got)
	}
	drained := spFixture()
	withCloneGrp(drained)
	got = memo.snapshot(drained)
	if !got.legs[spCloneLegB] || !got.sides[sideB] {
		t.Fatalf("snapshot %+v lost the live clone's verdicts", got)
	}
	if got.legs[spCloneGoneLeg] || got.sides[gone] {
		t.Fatalf("snapshot %+v kept a drained clone's verdicts", got)
	}
}

// TestReseedHealthCoversCloneLegsAndSides pins the pass's half of HL3 on the
// clone meta groups: the load's err_epoch of a clone's side is offered to the
// side's child and that of a clone's leg seeds the coordinator's monitor of
// the leg, so another observer's stamp is corrected at the owner's next clean
// verdict, as for a slice's leg and side.
func TestReseedHealthCoversCloneLegsAndSides(t *testing.T) {
	captureLogs(t)
	ctx := context.Background()
	d := newTestDeps(
		testConfig(common.WorkerRoleSp), newFakeStore(), newFakeClock(),
	)
	hw := &fakeHealthWriter{}
	d.health = hw
	w := spTestWorker(d)
	sideMon := newCloneSideMonitor(
		d, testCid, testSpId, spCloneNm, spCloneId, spCloneSideB,
	)
	key := cloneSideKey(spCloneLegB, spCloneSideB)
	w.sides[key] = &sideChild{driver: &sideDriver{health: sideMon}}
	legMon := newCloneLegMonitor(
		d, testCid, testSpId, spCloneNm, spCloneId, spCloneLegB,
	)
	w.legs[spCloneLegB] = legMon
	// writes counts the clears each monitor has written.
	clears := func(record string) int {
		n := 0
		for _, write := range hw.all() {
			if write.record == record && write.epoch == 0 {
				n++
			}
		}
		return n
	}
	sideMon.observe(ctx, healthClean, "")
	legMon.observe(ctx, healthClean, "")

	// Another observer stamped both; the load reads the stamps.
	state := spFixture()
	withCloneGrp(state)
	grp := state.Clones[spCloneNm].MetaGrp
	grp.LegList[1].ErrEpoch = 1
	grp.LegList[1].SideList[0].ErrEpoch = 1
	w.reseedHealth(state, w.healthSeqs())
	sideMon.observe(ctx, healthClean, "")
	legMon.observe(ctx, healthClean, "")
	if got := clears(healthRecordSide); got != 2 {
		t.Fatalf("%d side clears, want 2: the clone side's stamp was not "+
			"offered", got)
	}
	if got := clears(healthRecordLeg); got != 2 {
		t.Fatalf("%d leg clears, want 2: the clone leg's stamp was not "+
			"seeded", got)
	}
}

// ---------------------------------------------------------------------------
// AR8 on a clone meta group
// ---------------------------------------------------------------------------

const (
	// The reaction fixture's clone (withReactClone): its clone meta group
	// has two legs of one extent, on the DNs of the slice's legs, and leg
	// ids that sit between the slice's meta legs (100, 101) and its data
	// legs (110, 111), so that AR8's order across the SP is observable both
	// ways.
	reactCloneNm    = "rc0"
	reactCloneId    = uint64(600)
	reactCloneGrp   = uint64(30)
	reactCloneLegA  = uint64(105)
	reactCloneLegB  = uint64(106)
	reactCloneSideA = uint64(205)
	reactCloneSideB = uint64(206)
)

// withReactClone gives the reaction fixture a clone with a clone meta group
// of two healthy, provisioned legs on reactDnA and reactDnB, and returns the
// group.
func withReactClone(t *testing.T, state *model.SpState) *pb.Group {
	t.Helper()
	grp := reactGroup(t, reactCloneGrp, 1,
		[]uint64{reactCloneLegA, reactCloneLegB},
		[]uint64{reactCloneSideA, reactCloneSideB},
		[]string{reactDnA, reactDnB},
	)
	state.Conf.CloneNameList = append(state.Conf.CloneNameList, reactCloneNm)
	if state.Clones == nil {
		state.Clones = map[string]*pb.Clone{}
	}
	state.Clones[reactCloneNm] = &pb.Clone{
		CloneId: reactCloneId,
		MetaGrp: grp,
	}
	return grp
}

// cloneLegOf finds a leg of the fixture clone's group by id, in either list.
func cloneLegOf(t *testing.T, grp *pb.Group, legId uint64) *pb.Leg {
	t.Helper()
	for _, list := range [][]*pb.Leg{grp.GetLegList(), grp.GetSpareLegList()} {
		for _, leg := range list {
			if leg.GetLegId() == legId {
				return leg
			}
		}
	}
	t.Fatalf("leg %d not in the clone's group", legId)
	return nil
}

// withCloneSpare adds a spare to the fixture clone's group: one side, on
// addr, provisioned as asked.
func withCloneSpare(
	grp *pb.Group,
	legId uint64,
	sideId uint64,
	addr string,
	provisioned bool,
) *pb.Leg {
	spare := &pb.Leg{
		LegId:  legId,
		LegIdx: nextLegIdxOf(grp),
		SideList: []*pb.Side{{
			SideId:      sideId,
			AddrPort:    addr,
			Provisioned: provisioned,
		}},
	}
	grp.SpareLegList = append(grp.SpareLegList, spare)
	return spare
}

// nextLegIdxOf is one past the largest leg_idx of the group's two lists.
func nextLegIdxOf(grp *pb.Group) uint32 {
	idx := uint32(0)
	for _, list := range [][]*pb.Leg{grp.GetLegList(), grp.GetSpareLegList()} {
		for _, leg := range list {
			if leg.GetLegIdx() >= idx {
				idx = leg.GetLegIdx() + 1
			}
		}
	}
	return idx
}

// wantCloneRecord asserts that a record names the fixture clone, its group and
// leg, and no slice.
func wantCloneRecord(t *testing.T, rec map[string]any, legId uint64) {
	t.Helper()
	if rec["clone_name"] != reactCloneNm ||
		rec["clone_id"] != float64(reactCloneId) ||
		rec["grp_id"] != float64(reactCloneGrp) ||
		rec["leg_id"] != float64(legId) {
		t.Fatalf("record %v does not name clone %s (%d), group %d, leg %d",
			rec, reactCloneNm, reactCloneId, reactCloneGrp, legId)
	}
	if _, ok := rec["slice_id"]; ok {
		t.Fatalf("a clone meta group's record names a slice: %v", rec)
	}
}

// TestReactionCloneLegRepairCreates pins AR8 step 3 on a clone meta group: an
// unhealthy leg whose group has no spare gets one through the clone-keyed
// create, scanned as a slice's group's spare is — the group's ext_cnt, the
// stored batch, a black list of the group's DNs, which is the plan the op is
// handed — and the record names the clone instead of a slice.
func TestReactionCloneLegRepairCreates(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	grp := withReactClone(t, h.state)
	cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(
		common.DefaultLegUnhealthy,
	)
	h.dnCands(reactDnC)
	h.pass()
	calls := h.wantOps("create_clone_spare")
	got := calls[0]
	if got.cloneName != reactCloneNm || got.cloneId != reactCloneId ||
		got.grpId != reactCloneGrp || got.sliceId != 0 {
		t.Fatalf("create_clone_spare = %+v, want clone %s (%d) group %d",
			got, reactCloneNm, reactCloneId, reactCloneGrp)
	}
	if got.legs[0].AddrPort != reactDnC {
		t.Fatalf("spare on %s, want %s", got.legs[0].AddrPort, reactDnC)
	}
	if !slices.Equal(got.grpDn, []string{reactDnA, reactDnB}) {
		t.Fatalf("plan = %v, want the group's DNs", got.grpDn)
	}
	query := h.rops.allQueries()[0]
	if query.candExt != 1 || query.candCnt != reactDnBatch ||
		query.requiredCnt != 1 ||
		!slices.Equal(query.black, []string{reactDnA, reactDnB}) {
		t.Fatalf("scan = %+v, want the group's ext_cnt, the stored batch, "+
			"one DN and the group's DNs black-listed", query)
	}
	recs := h.wantApplied(reactionSpareCreate)
	wantCloneRecord(t, recs[0], reactCloneLegA)
	if recs[0]["spare_leg_id"] != float64(888) {
		t.Fatalf("record %v without the new spare", recs[0])
	}
}

// TestReactionCloneLegRepairSwitches pins AR8 step 1 on a clone meta group: a
// ready spare — provisioned, its row OK on the primary — is switched in for
// the unhealthy leg through the clone-keyed switch while the group's array
// runs (its row OK on the primary, step 1's gate; the gate's holds on a clone
// meta group are TestReactionSpareSwitchNeedsTheGroupRunning's).
func TestReactionCloneLegRepairSwitches(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	grp := withReactClone(t, h.state)
	cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
	withCloneSpare(grp, 107, 207, reactDnC, true)
	h.setLegRow(107, pb.ResStatus_RES_STATUS_OK)
	h.setGrpRow(reactCloneGrp, pb.ResStatus_RES_STATUS_OK)
	h.pass()
	calls := h.wantOps("switch_clone_spare")
	got := calls[0]
	if got.cloneName != reactCloneNm || got.cloneId != reactCloneId ||
		got.grpId != reactCloneGrp || got.spareLegId != 107 ||
		got.targetLegId != reactCloneLegA {
		t.Fatalf("switch_clone_spare = %+v, want spare 107 for leg %d of "+
			"clone %s (%d)", got, reactCloneLegA, reactCloneNm, reactCloneId)
	}
	recs := h.wantApplied(reactionSpareSwitch)
	wantCloneRecord(t, recs[0], reactCloneLegA)
}

// TestReactionCloneLegRepairHolds pins the holds of AR8 that a clone meta
// group shares with a slice's: a pending spare is waited for, a full spare
// list and a spare still unprovisioned hold the group, each logged, with no
// op, and a pool without redundancy ends the pass with redund_none.
func TestReactionCloneLegRepairHolds(t *testing.T) {
	t.Run("pending spare", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		grp := withReactClone(t, h.state)
		cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
		// Provisioned but never reported by the primary: on its way.
		withCloneSpare(grp, 107, 207, reactDnC, true)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionSpareSwitch, reasonSparePending)
	})

	t.Run("spare list full", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		grp := withReactClone(t, h.state)
		cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
		for idx := 0; idx < common.MaxSpareLegPerGrp; idx++ {
			parked := withCloneSpare(grp, uint64(107+idx), uint64(207+idx),
				reactDnC, true)
			parked.ErrEpoch = h.ago(9000)
		}
		h.dnCands(reactDnD)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionSpareCreate, reasonSpareListFull)
	})

	t.Run("unprovisioned spare", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		grp := withReactClone(t, h.state)
		cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
		spare := withCloneSpare(grp, 107, 207, reactDnC, false)
		spare.SideList[0].ErrEpoch = h.ago(1)
		h.dnCands(reactDnD)
		h.pass()
		h.wantOps()
		if got := len(h.rops.allQueries()); got != 0 {
			t.Fatalf("scans = %d, want 0", got)
		}
		h.wantSkippedBy(reactionSpareCreate, reasonSpareUnprovisioned, "")
	})

	t.Run("redund none", func(t *testing.T) {
		// A pool without redundancy gives its clone meta group one leg, so
		// that leg has no other; redund_none comes first and ends the pass,
		// so the slice's unhealthy leg after it is not tried.
		state := reactFixture(t)
		state.Conf.BdevConf.RedundConf = &pb.RedundConf{
			RedunKind: &pb.RedundConf_RedundNone{
				RedundNone: &pb.RedundNone{},
			},
		}
		h := newReactHarness(t, state)
		grp := withReactClone(t, h.state)
		grp.LegList = grp.LegList[:1]
		cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps()
		recs := h.logs.withMsg(msgReactionSkipped)
		if len(recs) != 1 || recs[0]["reason"] != reasonRedundNone {
			t.Fatalf("skips = %v, want one redund_none", recs)
		}
		wantCloneRecord(t, recs[0], reactCloneLegA)
	})
}

// TestReactionCloneLegRepairNeedsAHealthyLeg pins AR8's healthy-leg
// precondition on a clone meta group: a switch or a create runs only while
// another leg of the group's leg_list has no health epoch. With every other
// leg unhealthy the pass logs no_healthy_leg for the group, even with a
// ready spare at hand and the group's array running, or a pending spare,
// neither counting, and the scan goes on to the next group. A slice's group
// takes the same precondition (TestReactionLegRepairNeedsAHealthyLeg).
func TestReactionCloneLegRepairNeedsAHealthyLeg(t *testing.T) {
	t.Run("every leg unhealthy, a ready spare at hand", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		grp := withReactClone(t, h.state)
		cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
		cloneLegOf(t, grp, reactCloneLegB).ErrEpoch = h.ago(1)
		withCloneSpare(grp, 107, 207, reactDnC, true)
		h.setLegRow(107, pb.ResStatus_RES_STATUS_OK)
		// The group's row still reads OK, as an idle array's does after its
		// legs died, so step 1's gate would let the switch through.
		h.setGrpRow(reactCloneGrp, pb.ResStatus_RES_STATUS_OK)
		h.dnCands(reactDnD)
		h.pass()
		h.wantOps()
		if got := len(h.rops.allQueries()); got != 0 {
			t.Fatalf("scans = %d, want 0", got)
		}
		recs := h.logs.withMsg(msgReactionSkipped)
		if len(recs) != 1 || recs[0]["reason"] != reasonNoHealthyLeg ||
			recs[0]["kind"] != reactionSpareCreate {
			t.Fatalf("skips = %v, want one no_healthy_leg", recs)
		}
		wantCloneRecord(t, recs[0], reactCloneLegA)
	})

	t.Run("both legs due, each refused", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		grp := withReactClone(t, h.state)
		cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
		cloneLegOf(t, grp, reactCloneLegB).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps()
		recs := h.logs.withMsg(msgReactionSkipped)
		if len(recs) != 2 {
			t.Fatalf("skips = %v, want one per leg", recs)
		}
		wantCloneRecord(t, recs[0], reactCloneLegA)
		wantCloneRecord(t, recs[1], reactCloneLegB)
		for _, rec := range recs {
			if rec["reason"] != reasonNoHealthyLeg {
				t.Fatalf("skip %v, want no_healthy_leg", rec)
			}
		}
	})

	t.Run("a pending spare counts for nothing", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		grp := withReactClone(t, h.state)
		cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
		cloneLegOf(t, grp, reactCloneLegB).ErrEpoch = h.ago(1)
		// Not provisioned, not reported and with no health epoch: AR8
		// step 2's pending spare, outside the group's leg_list.
		withCloneSpare(grp, 107, 207, reactDnC, false)
		h.dnCands(reactDnD)
		h.pass()
		h.wantOps()
		h.wantSkipped(reactionSpareCreate, reasonNoHealthyLeg)
	})

	t.Run("the scan goes on to the next group", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		grp := withReactClone(t, h.state)
		cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
		cloneLegOf(t, grp, reactCloneLegB).ErrEpoch = h.ago(1)
		// The data group's leg has the larger leg_id, and its other leg is
		// healthy.
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnC)
		h.pass()
		calls := h.wantOps("create_spare")
		if calls[0].grpId != reactDataGrp {
			t.Fatalf("create_spare = %+v, want the data group", calls[0])
		}
		h.wantSkipped(reactionSpareCreate, reasonNoHealthyLeg)
		h.wantApplied(reactionSpareCreate)
	})
}

// TestReactionCloneLegRepairOrder pins AR8's "the smallest leg_id first"
// across the whole SP: one candidate list holds the slices' legs and the clone
// meta groups' legs, so a clone's leg goes before a slice's leg with a larger
// id and after one with a smaller id.
func TestReactionCloneLegRepairOrder(t *testing.T) {
	t.Run("clone leg first", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		grp := withReactClone(t, h.state)
		h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
		cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnC)
		h.pass()
		h.wantOps("create_clone_spare")
	})

	t.Run("slice leg first", func(t *testing.T) {
		h := newReactHarness(t, reactFixture(t))
		grp := withReactClone(t, h.state)
		h.legOf(reactMetaLegB).ErrEpoch = h.ago(9000)
		cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
		h.dnCands(reactDnC)
		h.pass()
		calls := h.wantOps("create_spare")
		if calls[0].grpId != reactMetaGrp {
			t.Fatalf("create_spare = %+v, want the meta group", calls[0])
		}
	})
}

// TestReactionDeletingCloneIsNotRepaired pins AR8 on a clone being deleted:
// its legs are no candidates — no spare op, no record — so the scan reaches
// the slice's leg behind it, while its legs keep their monitors' verdicts.
// The clone drain's final step runs in each pass ahead of the reactions
// (CLD7); the fake leaves the clone in place, so it runs again.
func TestReactionDeletingCloneIsNotRepaired(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	grp := withReactClone(t, h.state)
	h.state.Clones[reactCloneNm].Deleting = true
	cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
	h.dnCands(reactDnC)
	h.pass()
	h.wantOps("finish_clone")
	h.wantNoSkip()
	if got := len(h.rops.allQueries()); got != 0 {
		t.Fatalf("scans = %d, want 0", got)
	}

	h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
	h.pass()
	calls := h.wantOps("finish_clone", "finish_clone", "create_spare")
	if calls[2].grpId != reactDataGrp {
		t.Fatalf("create_spare = %+v, want the data group", calls[2])
	}
	if !h.w.verdicts.snapshot(h.state).legs[reactCloneLegA] {
		t.Fatalf("the deleting clone's leg lost its verdict")
	}
}

// TestReactionCloneSpareRefusalEndsThePass pins AR2 on the clone-keyed ops:
// an ErrPrecondition from CreateCloneSpareLeg's transaction is a `reaction
// skipped` naming the op, and it ends the pass, so the slice's leg behind
// the clone's is not tried.
func TestReactionCloneSpareRefusalEndsThePass(t *testing.T) {
	h := newReactHarness(t, reactFixture(t))
	grp := withReactClone(t, h.state)
	cloneLegOf(t, grp, reactCloneLegA).ErrEpoch = h.ago(9000)
	h.legOf(reactDataLegA).ErrEpoch = h.ago(9000)
	h.dnCands(reactDnC)
	h.rops.opErr = &model.ErrPrecondition{
		Op: "CreateCloneSpareLeg", Reason: "clone deleting",
	}
	h.pass()
	h.wantOps("create_clone_spare")
	h.wantApplied()
	h.wantSkippedBy(reactionSpareCreate, "clone deleting",
		"CreateCloneSpareLeg")
}
