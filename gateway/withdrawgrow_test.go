package gateway

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file pins a grow that never gets ready (architecture.md, GrowSlice,
// WithdrawGrow and The status read): a grow by hand is refused while the
// newest grown group of its kind is not ready, WithdrawGrow takes such a
// group back, and the status read lists one that has waited longer than the
// side threshold.

// wgGrow grows one slice by hand with the pool's current token.
func wgGrow(
	env *sptEnv,
	spId uint64,
	sliceId uint64,
	isMeta bool,
) (*pb.GrowSliceReply, error) {
	req := &pb.GrowSliceRequest{
		ClusterName: env.name,
		SpName:      sptSpName,
		SpRev:       &pb.SpRev{Revision: env.spRev(0, spId)},
		SliceId:     sliceId,
		IsMeta:      isMeta,
	}
	if !isMeta {
		req.ExtCnt = 1
	}
	return env.srv.GrowSlice(env.ctx, req)
}

// wgReady marks every side of the group grpId of the slice provisioned, as
// the worker's flip does once each disk node has zeroed its side.
func wgReady(env *sptEnv, spId, sliceId, grpId uint64) {
	env.t.Helper()
	slice := env.slice(spId, sliceId)
	for _, grp := range append(append([]*pb.Group(nil),
		slice.GetMetaGrpList()...), slice.GetDataGrpList()...) {
		if grp.GetGrpId() != grpId {
			continue
		}
		for _, leg := range grp.GetLegList() {
			for _, side := range leg.GetSideList() {
				side.Provisioned = true
			}
		}
	}
	mustPut(env.t, env.cli, model.SliceKey(env.cid, spId, sliceId), slice)
}

// sptReady marks every side of every slice of a pool provisioned, as the
// worker's flips do once the disk nodes have zeroed them: a test that grows
// a slice by hand twice in a row calls it between the grows, since a grow
// over a grown group that is not ready is refused (GrowSlice).
func sptReady(env *sptEnv, spName string) {
	env.t.Helper()
	conf := env.spConf(spName)
	for _, sliceId := range conf.GetSliceIdList() {
		slice := env.slice(conf.GetSpId(), sliceId)
		for _, grp := range append(append([]*pb.Group(nil),
			slice.GetMetaGrpList()...), slice.GetDataGrpList()...) {
			for _, leg := range grp.GetLegList() {
				for _, side := range leg.GetSideList() {
					side.Provisioned = true
				}
			}
		}
		mustPut(env.t, env.cli,
			model.SliceKey(env.cid, conf.GetSpId(), sliceId), slice)
	}
}

// TestGrowSliceRefusesOverAGrownGroupNotReady pins the refusal of a grow by
// hand while the newest grown group of its kind has a leg with no side
// ready: FAILED_PRECONDITION, naming the group and the call that takes it
// back, and nothing written. A first grow over the pool's first group, which
// is never taken back, goes on; a grow of the other kind goes on; once the
// grown group is ready, the next grow of its kind goes on. The grown group
// carries the time of its grow.
func TestGrowSliceRefusesOverAGrownGroupNotReady(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	sliceId := env.spConf(sptSpName).GetSliceIdList()[0]
	before := uint64(time.Now().Unix())
	first, err := wgGrow(env, spId, sliceId, false)
	if err != nil {
		t.Fatalf("a grow over the pool's first data group: %v", err)
	}
	slice := env.slice(spId, sliceId)
	if got := slice.GetDataGrpList()[1].GetGrowEpoch(); got < before ||
		got > uint64(time.Now().Unix()) {
		t.Errorf("the grown group's grow_epoch %d, want the grow's time "+
			"(from %d)", got, before)
	}
	dumped := env.dumpAll()
	_, err = wgGrow(env, spId, sliceId, false)
	sptWantCode(t, err, codes.FailedPrecondition)
	msg := status.Convert(err).Message()
	for _, want := range []string{
		fmt.Sprintf("data group %d", first.GetGrpId()),
		"not ready",
		"WithdrawGrow",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not say %q", msg, want)
		}
	}
	if changed := sptChangedKeys(dumped, env.dumpAll()); len(changed) != 0 {
		t.Errorf("a refusal wrote %v", changed)
	}
	if _, err := wgGrow(env, spId, sliceId, true); err != nil {
		t.Errorf("a meta grow beside a data group not ready: %v", err)
	}
	wgReady(env, spId, sliceId, first.GetGrpId())
	if _, err := wgGrow(env, spId, sliceId, false); err != nil {
		t.Errorf("a grow once the grown group is ready: %v", err)
	}
}

// TestWithdrawGrowTakesAStuckGroupBack pins WithdrawGrow: the newest grown
// group of a kind with a leg not ready is taken back, its extents given back
// to its disk nodes and to the CN of every cntlr, the pool's revision bumped
// once; a group that is not the newest, a first group, a group that is
// ready, a stale token, a zero grp_id, a slice not in the pool and a pool at
// a level with no thin pool are refused, and write nothing. A meta group is
// taken back with is_meta.
func TestWithdrawGrowTakesAStuckGroupBack(t *testing.T) {
	env := sptNewEnv(t, sptDnCnt, sptCnCnt, sptCnFree)
	spId := env.createSp(sptDefaultSpec(sptSpName))
	conf := env.spConf(sptSpName)
	sliceId := conf.GetSliceIdList()[0]
	firstData := env.slice(spId, sliceId).GetDataGrpList()[0].GetGrpId()
	ready, err := wgGrow(env, spId, sliceId, false)
	if err != nil {
		t.Fatalf("GrowSlice: %v", err)
	}
	wgReady(env, spId, sliceId, ready.GetGrpId())
	dnFree := map[string]uint64{}
	for _, addr := range env.dnAddrs {
		dnFree[addr] = env.dnConf(addr).GetFreeExtCnt()
	}
	cnFree := map[string]uint64{}
	for _, cntlrId := range conf.GetCntlrIdList() {
		addr := env.cntlr(spId, cntlrId).GetAddrPort()
		cnFree[addr] = env.cnConf(addr).GetFreeExtCnt()
	}
	grown, err := wgGrow(env, spId, sliceId, false)
	if err != nil {
		t.Fatalf("GrowSlice over a ready group: %v", err)
	}
	withdraw := func(slice, grpId, rev uint64, isMeta bool) (
		*pb.WithdrawGrowReply, error) {
		return env.srv.WithdrawGrow(env.ctx, &pb.WithdrawGrowRequest{
			ClusterName: env.name,
			SpName:      sptSpName,
			SpRev:       &pb.SpRev{Revision: rev},
			SliceId:     slice,
			IsMeta:      isMeta,
			GrpId:       grpId,
		})
	}
	refuse := func(label string, slice, grpId, rev uint64, isMeta bool,
		code codes.Code, frag string) {
		t.Helper()
		if rev == 0 {
			rev = env.spRev(0, spId)
		}
		dumped := env.dumpAll()
		_, err := withdraw(slice, grpId, rev, isMeta)
		if status.Code(err) != code ||
			!strings.Contains(status.Convert(err).Message(), frag) {
			t.Errorf("%s: %v, want %v saying %q", label, err, code, frag)
		}
		if changed := sptChangedKeys(dumped, env.dumpAll()); len(
			changed) != 0 {
			t.Errorf("%s: a refusal wrote %v", label, changed)
		}
	}
	refuse("a group that is not the newest", sliceId, ready.GetGrpId(), 0,
		false, codes.FailedPrecondition, "not the newest")
	refuse("a first group", sliceId, firstData, 0, false,
		codes.FailedPrecondition, "first")
	refuse("a stale token", sliceId, grown.GetGrpId(), 1, false,
		codes.Aborted, "")
	refuse("a zero grp_id", sliceId, 0, 0, false, codes.InvalidArgument,
		"grp_id")
	refuse("a slice not in the pool", sliceId+1000, grown.GetGrpId(), 0,
		false, codes.NotFound, "not in storage pool")
	revBefore := env.spRev(0, spId)
	reply, err := withdraw(sliceId, grown.GetGrpId(), revBefore, false)
	if err != nil {
		t.Fatalf("WithdrawGrow: %v", err)
	}
	if reply.GetGrpId() != grown.GetGrpId() ||
		reply.GetSliceId() != sliceId {
		t.Errorf("reply %v, want slice %d, group %d", reply, sliceId,
			grown.GetGrpId())
	}
	if grps := env.slice(spId, sliceId).GetDataGrpList(); len(grps) != 2 ||
		grps[1].GetGrpId() != ready.GetGrpId() {
		t.Errorf("data groups %v, want the first and the ready one", grps)
	}
	if got := env.spRev(0, spId); got != revBefore+1 {
		t.Errorf("sp_rev %d, want one bump from %d", got, revBefore)
	}
	for addr, free := range dnFree {
		if got := env.dnConf(addr).GetFreeExtCnt(); got != free {
			t.Errorf("dn %q: free %d, want %d back", addr, got, free)
		}
	}
	for addr, free := range cnFree {
		if got := env.cnConf(addr).GetFreeExtCnt(); got != free {
			t.Errorf("cn %q: free %d, want %d back", addr, got, free)
		}
	}
	refuse("a ready group", sliceId, ready.GetGrpId(), 0, false,
		codes.FailedPrecondition, "ready")
	meta, err := wgGrow(env, spId, sliceId, true)
	if err != nil {
		t.Fatalf("a meta grow: %v", err)
	}
	refuse("a meta group named as a data group", sliceId, meta.GetGrpId(),
		0, false, codes.FailedPrecondition, "not the newest")
	stored := env.spConf(sptSpName)
	stored.SpLevel = pb.SpLevel_SP_LEVEL_NO_THINPOOL
	mustPut(t, env.cli, model.SpConfKey(env.cid, sptSpName), stored)
	refuse("a pool with no thin pool", sliceId, meta.GetGrpId(), 0, true,
		codes.FailedPrecondition, "level")
	stored.SpLevel = pb.SpLevel_SP_LEVEL_READWRITE
	mustPut(t, env.cli, model.SpConfKey(env.cid, sptSpName), stored)
	if _, err := withdraw(sliceId, meta.GetGrpId(), env.spRev(0, spId),
		true); err != nil {
		t.Errorf("WithdrawGrow of the meta group: %v", err)
	}
	if grps := env.slice(spId, sliceId).GetMetaGrpList(); len(grps) != 1 {
		t.Errorf("meta groups %v, want the first alone", grps)
	}
}

// TestStatusListsAGrowThatWaits pins the status read's row of a grow that
// waits: the newest grown group of a kind has a leg with no side ready, and
// its grow is older than the pool's side threshold. A younger one, and one
// that carries no grow time, give no row.
func TestStatusListsAGrowThatWaits(t *testing.T) {
	threshold := uint64(model.ResolveEventThreshold(nil).GetSideUnhealthy())
	now := uint64(time.Now().Unix())
	for _, tc := range []struct {
		label string
		epoch uint64
		row   bool
	}{
		{"past the threshold", now - threshold - 30, true},
		{"within the threshold", now - 1, false},
		{"no grow time", 0, false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			env := newStEnv(t)
			cn, fake := env.addCn("cn")
			var dns []string
			for idx := 0; idx < 6; idx++ {
				addr := fmt.Sprintf("dn-%d:9000", idx)
				env.addDn(addr, 10)
				dns = append(dns, addr)
			}
			pool := env.newPool("p0", cn, dns)
			slice := pool.slices[stSlice]
			stuck := stLegOf(91, 0, dns[4])
			stuck.SideList[0].Provisioned = false
			slice.DataGrpList = append(slice.DataGrpList, &pb.Group{
				GrpId: 90, ExtCnt: 4, MetaBlocks: 3, DataBlocks: 2045,
				LegList:   []*pb.Leg{stuck, stLegOf(93, 1, dns[5])},
				GrowEpoch: tc.epoch,
			})
			fake.setInfo(nil, nil, nil, &pb.CntlrInfo{
				SliceIdToDmPool: map[uint64]*pb.ResInfo{stSlice: {
					Status:  pb.ResStatus_RES_STATUS_OK,
					Details: stPoolLine(1, 100, 10, 62, "rw"),
				}},
			})
			env.put(pool)
			rows := stRowsOf(env.status(0, ""), "p0")
			if !tc.row {
				stWantNone(t, rows, tc.label)
				return
			}
			row := stOne(t, rows, pb.StatusKind_STATUS_KIND_GROW, tc.label,
				"data group 90", "waits", "WithdrawGrow")
			if row.GetSliceId() != stSlice {
				t.Errorf("row %v: want slice %d", row, stSlice)
			}
		})
	}
}

// TestStatusListsAMetaGrowThatWaitsButNotAtNoThinPool pins the meta list's
// row of a waiting grow, and that a pool at a level with no thin pool, where
// nothing grows and no group is taken back, lists none.
func TestStatusListsAMetaGrowThatWaitsButNotAtNoThinPool(t *testing.T) {
	threshold := uint64(model.ResolveEventThreshold(nil).GetSideUnhealthy())
	now := uint64(time.Now().Unix())
	for _, level := range []pb.SpLevel{
		pb.SpLevel_SP_LEVEL_READWRITE, pb.SpLevel_SP_LEVEL_NO_THINPOOL,
	} {
		t.Run(level.String(), func(t *testing.T) {
			env := newStEnv(t)
			cn, fake := env.addCn("cn")
			var dns []string
			for idx := 0; idx < 6; idx++ {
				addr := fmt.Sprintf("dn-%d:9000", idx)
				env.addDn(addr, 10)
				dns = append(dns, addr)
			}
			pool := env.newPool("p0", cn, dns)
			pool.conf.SpLevel = level
			slice := pool.slices[stSlice]
			stuck := stLegOf(91, 0, dns[4])
			stuck.SideList[0].Provisioned = false
			slice.MetaGrpList = append(slice.MetaGrpList, &pb.Group{
				GrpId: 90, ExtCnt: 1, MetaBlocks: 3, DataBlocks: 509,
				LegList:   []*pb.Leg{stuck, stLegOf(93, 1, dns[5])},
				GrowEpoch: now - threshold - 30,
			})
			fake.setInfo(nil, nil, nil, &pb.CntlrInfo{
				SliceIdToDmPool: map[uint64]*pb.ResInfo{stSlice: {
					Status:  pb.ResStatus_RES_STATUS_OK,
					Details: stPoolLine(1, 100, 10, 62, "rw"),
				}},
			})
			env.put(pool)
			rows := stKinds(stRowsOf(env.status(0, ""), "p0"),
				pb.StatusKind_STATUS_KIND_GROW)
			if level == pb.SpLevel_SP_LEVEL_NO_THINPOOL {
				if len(rows) != 0 {
					t.Errorf("rows %v at %v, want none", rows, level)
				}
				return
			}
			if len(rows) != 1 || !strings.Contains(rows[0].GetText(),
				"metadata grow waits: metadata group 90") {
				t.Errorf("rows %v, want the metadata group's", rows)
			}
		})
	}
}
