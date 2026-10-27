package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The repair and retired-leg RPCs (architecture.md, Repairs and Retired legs;
// gateway.md, Repairs and retired legs), on the fixture of
// handler_vol_test.go: an md-raid1 pool whose data group's two legs sit on
// dn-a and dn-b, with dn-c and dn-d free. Every write is asserted against
// the store, every refusal by its code, its message and a store that did not
// move. The sp worker's part — the stage moves from the primary's report —
// is played with model.SetRepairStage, the operation the worker runs, and the
// health epoch that makes a leg bad is planted, as the worker's monitor
// writes one: a direct put that moves no revision (architecture.md, Revision
// keys and the sync fan-out).

// repEpoch is the err_epoch the tests plant on a bad leg.
const repEpoch = uint64(1700000000)

// repSetLegEpoch sets one leg's err_epoch in the fixture slice.
func repSetLegEpoch(env *volEnv, grpId uint64, legId uint64, epoch uint64) {
	env.t.Helper()
	slice := env.slice()
	leg := grpLegOf(volGrpOf(env.t, slice, grpId), legId)
	if leg == nil {
		env.t.Fatalf("leg %d is not in group %d", legId, grpId)
	}
	leg.ErrEpoch = epoch
	env.putSlice(slice)
}

// repRequest is the create of "rep-a" on the data group, leg A bad, the new
// leg fresh and pinned to dn-c, under the current token.
func repRequest(env *volEnv) *pb.CreateRepairRequest {
	env.t.Helper()
	return &pb.CreateRepairRequest{
		ClusterName:  env.cluster,
		SpName:       volSpName,
		SpRev:        env.token(),
		RepName:      "rep-a",
		GrpId:        volDataGrpId,
		BadLegIdList: []uint64{volDataLegA},
		DnSelector:   volDnSelector(volDnC),
	}
}

// repCreate creates a repair of the data group with a fresh new leg on
// dnAddr, the bad leg's err_epoch planted first.
func repCreate(
	env *volEnv,
	name string,
	badLegId uint64,
	dnAddr string,
) *pb.CreateRepairReply {
	env.t.Helper()
	repSetLegEpoch(env, volDataGrpId, badLegId, repEpoch)
	req := repRequest(env)
	req.RepName = name
	req.BadLegIdList = []uint64{badLegId}
	req.DnSelector = volDnSelector(dnAddr)
	reply, err := env.srv.CreateRepair(env.ctx, req)
	if err != nil {
		env.t.Fatalf("CreateRepair %s: %v", name, err)
	}
	return reply
}

// repair reads one Repair key.
func (e *volEnv) repair(name string) *pb.Repair {
	e.t.Helper()
	rep := &pb.Repair{}
	e.get(model.RepairKey(e.cid, volSpId, name), rep)
	return rep
}

// retiredLeg reads one RetiredLeg key.
func (e *volEnv) retiredLeg(legId uint64) *pb.RetiredLeg {
	e.t.Helper()
	retired := &pb.RetiredLeg{}
	e.get(model.RetiredLegKey(e.cid, volSpId, legId), retired)
	return retired
}

// repMoveStage plays the sp worker's stage move from the primary's verdict
// (architecture.md, Repair), with the rep_id the record carries.
func repMoveStage(env *volEnv, name string, stage pb.RepStage) {
	env.t.Helper()
	err := model.SetRepairStage(env.ctx, env.cli, env.cid, volShard,
		volSpId, volSpName, name, env.repair(name).GetRepId(), stage)
	if err != nil {
		env.t.Fatalf("SetRepairStage %s %s: %v", name, stage, err)
	}
}

// repAbort is TryToAbortRepair under the current token.
func repAbort(env *volEnv, name string) (*pb.TryToAbortRepairReply, error) {
	env.t.Helper()
	return env.srv.TryToAbortRepair(env.ctx, &pb.TryToAbortRepairRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		RepName:     name,
	})
}

// repFinish is FinishRepair under the current token.
func repFinish(env *volEnv, name string) (*pb.FinishRepairReply, error) {
	env.t.Helper()
	return env.srv.FinishRepair(env.ctx, &pb.FinishRepairRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		RepName:     name,
	})
}

// repCancel is CancelRepair under the current token.
func repCancel(env *volEnv, name string) (*pb.CancelRepairReply, error) {
	env.t.Helper()
	return env.srv.CancelRepair(env.ctx, &pb.CancelRepairRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		RepName:     name,
	})
}

// repDeleteRetired is DeleteRetiredLeg under the current token.
func repDeleteRetired(
	env *volEnv,
	legId uint64,
) (*pb.DeleteRetiredLegReply, error) {
	env.t.Helper()
	return env.srv.DeleteRetiredLeg(env.ctx, &pb.DeleteRetiredLegRequest{
		ClusterName: env.cluster,
		SpName:      volSpName,
		SpRev:       env.token(),
		LegId:       legId,
	})
}

// repChargeDn gives one DN a side the way a create charges it: the group's
// extents off its free count, the capacity key moved with it, and the
// side's pointer in its list. No revision moves, as for every planted
// record.
func repChargeDn(env *volEnv, addrPort string, ptr *pb.SidePointer, ext uint64) {
	env.t.Helper()
	env.setDnFree(addrPort, env.dn(addrPort).GetFreeExtCnt()-ext)
	dn := env.dn(addrPort)
	dn.SidePtrList = append(dn.SidePtrList, ptr)
	mustPut(env.t, env.cli, model.DnConfKey(env.cid, addrPort), dn)
}

// repPutRepair plants a repair of a group of the fixture slice, as
// CreateRepair writes one: the record, the group's rep_name and the pool's
// list. Its new leg's side is newSide, which no DN is charged for: the tests
// that plant one read the record, never the DN.
func repPutRepair(
	env *volEnv,
	name string,
	grpId uint64,
	badLegId uint64,
	stage pb.RepStage,
	newLegId uint64,
	newSide *pb.Side,
) {
	env.t.Helper()
	slice := env.slice()
	volGrpOf(env.t, slice, grpId).RepName = name
	env.putSlice(slice)
	mustPut(env.t, env.cli, model.RepairKey(env.cid, volSpId, name),
		&pb.Repair{
			RepId:    newLegId - 1,
			GrpId:    grpId,
			RepStage: stage,
			NewLegList: []*pb.Leg{{
				LegId:    newLegId,
				LegIdx:   2,
				SideList: []*pb.Side{newSide},
			}},
			BadLegIdList: []uint64{badLegId},
			SliceId:      volSliceId,
		})
	conf := env.spConf()
	conf.RepairNameList = append(conf.RepairNameList, name)
	env.putSpConf(conf)
}

// repPutRetired plants a retired leg of a group of the fixture slice, as a
// finish writes one: the record, the group's list and the pool's list, and
// its side charged to its DN with the group's extents.
func repPutRetired(
	env *volEnv,
	grpId uint64,
	legId uint64,
	side *pb.Side,
	retiredEpoch uint64,
) *pb.RetiredLeg {
	env.t.Helper()
	slice := env.slice()
	grp := volGrpOf(env.t, slice, grpId)
	grp.RetiredLegIdList = append(grp.RetiredLegIdList, legId)
	env.putSlice(slice)
	retired := &pb.RetiredLeg{
		GrpId:        grpId,
		LegIdx:       uint32(legId % 8),
		Side:         side,
		RetiredEpoch: retiredEpoch,
		SliceId:      volSliceId,
	}
	mustPut(env.t, env.cli, model.RetiredLegKey(env.cid, volSpId, legId),
		retired)
	conf := env.spConf()
	conf.RetiredLegIdList = append(conf.RetiredLegIdList, legId)
	env.putSpConf(conf)
	repChargeDn(env, side.GetAddrPort(), &pb.SidePointer{
		SpId: volSpId, LegId: legId, SideId: side.GetSideId(),
	}, grp.GetExtCnt())
	return retired
}

// repWantDn asserts one DN's free count, its revision and whether it holds
// one side's pointer.
func repWantDn(
	t *testing.T,
	env *volEnv,
	addrPort string,
	free uint64,
	rev uint64,
	sideId uint64,
	holds bool,
) {
	t.Helper()
	dn := env.dn(addrPort)
	if dn.GetFreeExtCnt() != free {
		t.Errorf("%s free_ext_cnt: got %d, want %d",
			addrPort, dn.GetFreeExtCnt(), free)
	}
	if !env.exists(env.dnCapKey(addrPort, free), &pb.DnCapacity{}) {
		t.Errorf("%s has no capacity key at free_ext_cnt %d", addrPort, free)
	}
	if got := env.dnRev(addrPort); got != rev {
		t.Errorf("%s dn_rev: got %d, want %d", addrPort, got, rev)
	}
	found := false
	for _, ptr := range dn.GetSidePtrList() {
		if ptr.GetSideId() == sideId {
			found = true
		}
	}
	if found != holds {
		t.Errorf("%s holds side %d: %v, want %v", addrPort, sideId,
			found, holds)
	}
}

// TestCreateRepairWritesTheRecord pins CreateRepair with a fresh new leg
// (architecture.md, Repairs): three ids from next_id, the rep_id, the new leg
// and its side in that order; the new leg at the group's next leg_idx with
// one side written unprovisioned ([D15]) on a DN outside the group, with the
// pool's first cntlid slot; the record CREATED with its flag clear, its bad
// leg and its parent, the slice; the group's rep_name and the pool's list;
// the DN charged the group's extents with one DnRev bump; no CN touched; one
// SpRev bump. The bad leg stays in leg_list with its err_epoch.
func TestCreateRepairWritesTheRecord(t *testing.T) {
	env := newVolEnv(t)
	reply := repCreate(env, "rep-a", volDataLegA, volDnC)
	if reply.GetRepId() != volNextId || reply.GetNewLegId() != volNextId+1 {
		t.Fatalf("reply: rep_id %d, new_leg_id %d, want %d and %d",
			reply.GetRepId(), reply.GetNewLegId(), volNextId, volNextId+1)
	}
	want := &pb.Repair{
		RepId:    volNextId,
		GrpId:    volDataGrpId,
		RepStage: pb.RepStage_REP_STAGE_CREATED,
		NewLegList: []*pb.Leg{{
			LegId:  volNextId + 1,
			LegIdx: 2,
			SideList: []*pb.Side{{
				SideId:      volNextId + 2,
				AddrPort:    volDnC,
				CntlidSlot:  volSlot,
				NvmeTrConf:  volTrConf(volDnC),
				Provisioned: false,
			}},
		}},
		BadLegIdList: []uint64{volDataLegA},
		SliceId:      volSliceId,
	}
	if got := env.repair("rep-a"); !proto.Equal(got, want) {
		t.Errorf("repair:\n got %v\nwant %v", got, want)
	}
	grp := volGrpOf(t, env.slice(), volDataGrpId)
	if grp.GetRepName() != "rep-a" {
		t.Errorf("group rep_name: got %q, want %q", grp.GetRepName(), "rep-a")
	}
	if len(grp.GetLegList()) != 2 ||
		grp.GetLegList()[0].GetErrEpoch() != repEpoch {
		t.Errorf("the bad leg must stay in leg_list with its err_epoch: %v",
			grp.GetLegList())
	}
	conf := env.spConf()
	if !proto.Equal(&pb.SpConf{RepairNameList: conf.GetRepairNameList()},
		&pb.SpConf{RepairNameList: []string{"rep-a"}}) {
		t.Errorf("repair_name_list: got %v", conf.GetRepairNameList())
	}
	if conf.GetNextId() != volNextId+3 {
		t.Errorf("next_id: got %d, want %d", conf.GetNextId(), volNextId+3)
	}
	repWantDn(t, env, volDnC, volDnFree-volDataExtCnt, 2, volNextId+2, true)
	if got := env.dn(volDnC).GetUsedMetaUnits(); got != 0 {
		t.Errorf("dn-c used_meta_units: got %d, want 0", got)
	}
	for _, addrPort := range []string{volCnA, volCnB} {
		if got := env.cnRev(addrPort); got != 1 {
			t.Errorf("%s rev: got %d, want 1 (a new leg charges no CN)",
				addrPort, got)
		}
	}
	if rev := env.spRev(); rev != 2 {
		t.Errorf("sp_rev: got %d, want 2", rev)
	}
}

// TestCreateRepairPlacesTheNewLegOffTheGroup pins the two tiers of a fresh
// new leg's placement (architecture.md, Per-operation allocation,
// CreateRepair): with no selector, the scan black-lists the group's DNs and
// keeps out of their failure domains while another domain has room, since a
// new leg in the domain of the leg it replaces dies with it. dn-c shares
// dn-a's domain and holds the most, so a scan without tier 1 would hand it
// back first; the new leg lands on dn-d. With dn-d gone, tier 2 still places
// the new leg, on dn-c, because no repair at all is the worse outcome.
func TestCreateRepairPlacesTheNewLegOffTheGroup(t *testing.T) {
	place := func(env *volEnv) (string, error) {
		env.t.Helper()
		repSetLegEpoch(env, volDataGrpId, volDataLegA, repEpoch)
		req := repRequest(env)
		req.DnSelector = nil
		if _, err := env.srv.CreateRepair(env.ctx, req); err != nil {
			return "", err
		}
		side := env.repair("rep-a").GetNewLegList()[0].GetSideList()[0]
		return side.GetAddrPort(), nil
	}
	t.Run("tier 1 avoids the group's domains", func(t *testing.T) {
		// One draw proves nothing here — see volTier1DrawCnt.
		for draw := 0; draw < volTier1DrawCnt; draw++ {
			env := volTwoDomainEnv(t)
			got, err := place(env)
			if err != nil {
				t.Fatalf("CreateRepair: %v", err)
			}
			if got != volDnD {
				t.Fatalf("draw %d: the new leg on %s, want %s (dn-c is in "+
					"dn-a's domain, and tier 2 must not have run)", draw,
					got, volDnD)
			}
		}
	})
	t.Run("tier 2 places in an occupied domain", func(t *testing.T) {
		env := volTwoDomainEnv(t)
		env.dropDn(volDnD)
		got, err := place(env)
		if err != nil {
			t.Fatalf("CreateRepair: %v (tier 2 must place, never "+
				"RESOURCE_EXHAUSTED)", err)
		}
		if got != volDnC {
			t.Errorf("the new leg on %s, want %s", got, volDnC)
		}
		if got := env.dn(volDnC).GetFreeExtCnt(); got !=
			volDnCFree-volDataExtCnt {
			t.Errorf("dn-c free_ext_cnt: got %d, want %d", got,
				volDnCFree-volDataExtCnt)
		}
	})
}

// TestCreateRepairOnACloneMetaGroup pins the look-up of architecture.md,
// Repairs: a grp_id no slice holds is looked for among the clones, and the
// repair records the clone as its parent and its name on the clone's group.
// The new leg takes the clone meta group's extents on a DN outside it.
func TestCreateRepairOnACloneMetaGroup(t *testing.T) {
	env := newVolEnv(t)
	env.putTd("dst", 900, 7, 0, true)
	volCreateClone(env, "clone-a", "dst")
	clone := env.clone("clone-a")
	grp := clone.GetMetaGrp()
	bad := grp.GetLegList()[0]
	bad.ErrEpoch = repEpoch
	mustPut(t, env.cli, model.CloneKey(env.cid, volSpId, "clone-a"), clone)
	grpAddrs := grpDnAddrs(grp, nil)
	reply, err := env.srv.CreateRepair(env.ctx, &pb.CreateRepairRequest{
		ClusterName:  env.cluster,
		SpName:       volSpName,
		SpRev:        env.token(),
		RepName:      "rep-a",
		GrpId:        grp.GetGrpId(),
		BadLegIdList: []uint64{bad.GetLegId()},
	})
	if err != nil {
		t.Fatalf("CreateRepair: %v", err)
	}
	rep := env.repair("rep-a")
	if rep.GetCloneName() != "clone-a" || rep.GetSliceId() != 0 ||
		rep.GetGrpId() != grp.GetGrpId() {
		t.Errorf("parent: clone %q, slice %d, group %d; want clone %q, "+
			"slice 0, group %d", rep.GetCloneName(), rep.GetSliceId(),
			rep.GetGrpId(), "clone-a", grp.GetGrpId())
	}
	if got := env.clone("clone-a").GetMetaGrp().GetRepName(); got != "rep-a" {
		t.Errorf("the clone's group rep_name: got %q, want %q", got, "rep-a")
	}
	side := rep.GetNewLegList()[0].GetSideList()[0]
	for _, addrPort := range grpAddrs {
		if side.GetAddrPort() == addrPort {
			t.Errorf("the new leg on %s, a DN of the group", addrPort)
		}
	}
	if reply.GetNewLegId() != rep.GetNewLegList()[0].GetLegId() {
		t.Errorf("reply new_leg_id %d, the record's %d",
			reply.GetNewLegId(), rep.GetNewLegList()[0].GetLegId())
	}
	dn := env.dn(side.GetAddrPort())
	if dn.GetFreeExtCnt() != volDnFree-common.CloneMetaGrpExtCnt {
		t.Errorf("%s free_ext_cnt: got %d, want %d", side.GetAddrPort(),
			dn.GetFreeExtCnt(), volDnFree-common.CloneMetaGrpExtCnt)
	}
}

// TestCreateRepairTakesARetiredLegBack pins CreateRepair from a retired leg
// (architecture.md, Repairs): nothing is allocated — the retired leg's
// record and both list entries go, and its leg id, leg_idx and side become
// the new leg, whose DN keeps the side's pointer and extents and bumps no
// revision — the repair keeps its retired_epoch for a cancel and takes one
// id. It is allowed while the group holds MaxRetiredLegPerGrp retired legs.
func TestCreateRepairTakesARetiredLegBack(t *testing.T) {
	env := newVolEnv(t)
	repSetLegEpoch(env, volDataGrpId, volDataLegA, repEpoch)
	first := repPutRetired(env, volDataGrpId, 900, volSide(901, volDnC), 11)
	repPutRetired(env, volDataGrpId, 910, volSide(911, volDnD), 22)
	if n := len(volGrpOf(t, env.slice(), volDataGrpId).
		GetRetiredLegIdList()); n != common.MaxRetiredLegPerGrp {
		t.Fatalf("the fixture's group holds %d retired legs, want %d", n,
			common.MaxRetiredLegPerGrp)
	}
	dnCBefore := env.dn(volDnC)
	req := repRequest(env)
	req.RetiredLegId = 900
	req.DnSelector = nil
	reply, err := env.srv.CreateRepair(env.ctx, req)
	if err != nil {
		t.Fatalf("CreateRepair: %v", err)
	}
	if reply.GetRepId() != volNextId || reply.GetNewLegId() != 900 {
		t.Errorf("reply: rep_id %d, new_leg_id %d, want %d and 900",
			reply.GetRepId(), reply.GetNewLegId(), volNextId)
	}
	want := &pb.Repair{
		RepId:    volNextId,
		GrpId:    volDataGrpId,
		RepStage: pb.RepStage_REP_STAGE_CREATED,
		NewLegList: []*pb.Leg{{
			LegId:    900,
			LegIdx:   first.GetLegIdx(),
			SideList: []*pb.Side{first.GetSide()},
		}},
		BadLegIdList:           []uint64{volDataLegA},
		SliceId:                volSliceId,
		NewLegIdToRetiredEpoch: map[uint64]uint64{900: 11},
	}
	if got := env.repair("rep-a"); !proto.Equal(got, want) {
		t.Errorf("repair:\n got %v\nwant %v", got, want)
	}
	if env.exists(model.RetiredLegKey(env.cid, volSpId, 900),
		&pb.RetiredLeg{}) {
		t.Errorf("the taken retired leg's key survived")
	}
	grp := volGrpOf(t, env.slice(), volDataGrpId)
	conf := env.spConf()
	for what, list := range map[string][]uint64{
		"group": grp.GetRetiredLegIdList(),
		"pool":  conf.GetRetiredLegIdList(),
	} {
		if len(list) != 1 || list[0] != 910 {
			t.Errorf("the %s's retired_leg_id_list: got %v, want [910]",
				what, list)
		}
	}
	if conf.GetNextId() != volNextId+1 {
		t.Errorf("next_id: got %d, want %d (the rep_id alone)",
			conf.GetNextId(), volNextId+1)
	}
	if got := env.dn(volDnC); !proto.Equal(got, dnCBefore) {
		t.Errorf("dn-c moved:\n got %v\nwant %v", got, dnCBefore)
	}
	if got := env.dnRev(volDnC); got != 1 {
		t.Errorf("dn-c rev: got %d, want 1", got)
	}
	if rev := env.spRev(); rev != 2 {
		t.Errorf("sp_rev: got %d, want 2", rev)
	}
}

// TestCreateRepairTakesARetiredLegOnTheBadLegsDn pins the one DN a retired
// side may share with the group (architecture.md, Repairs): the bad leg's,
// since the bad leg leaves the group at the finish.
func TestCreateRepairTakesARetiredLegOnTheBadLegsDn(t *testing.T) {
	env := newVolEnv(t)
	repSetLegEpoch(env, volDataGrpId, volDataLegA, repEpoch)
	repPutRetired(env, volDataGrpId, 900, volSide(901, volDnA), 11)
	req := repRequest(env)
	req.RetiredLegId = 900
	if _, err := env.srv.CreateRepair(env.ctx, req); err != nil {
		t.Fatalf("CreateRepair from a retired side on the bad leg's DN: %v",
			err)
	}
}

// repRefusal is one refusal of CreateRepair: what the case changes in the
// fixture and in the request, and the code and message it must meet.
type repRefusal struct {
	name  string
	setup func(env *volEnv, req *pb.CreateRepairRequest)
	code  codes.Code
	msg   string
}

// repRefusals is every refusal of CreateRepair in architecture.md, Repairs,
// in the order a request meets them there, with the cases that break two
// rules at once to pin that order. Each starts from the fixture with leg A's
// err_epoch set and repRequest, which would succeed.
func repRefusals() []repRefusal {
	setConf := func(env *volEnv, patch func(conf *pb.SpConf)) {
		conf := env.spConf()
		patch(conf)
		env.putSpConf(conf)
	}
	redundNone := func(env *volEnv) {
		setConf(env, func(conf *pb.SpConf) {
			conf.BdevConf.RedundConf = &pb.RedundConf{
				RedunKind: &pb.RedundConf_RedundNone{
					RedundNone: &pb.RedundNone{},
				},
			}
		})
	}
	setLevel := func(env *volEnv, level pb.SpLevel) {
		setConf(env, func(conf *pb.SpConf) { conf.SpLevel = level })
	}
	noRedund := func(env *volEnv) {
		setLevel(env, pb.SpLevel_SP_LEVEL_NO_REDUND)
	}
	metaRepair := func(env *volEnv) {
		repPutRepair(env, "rep-a", volMetaGrpId, volMetaLegA,
			pb.RepStage_REP_STAGE_CREATED, 801, volSide(802, volDnD))
	}
	grpRepair := func(env *volEnv) {
		repPutRepair(env, "rep-b", volDataGrpId, volDataLegB,
			pb.RepStage_REP_STAGE_CREATED, 801, volSide(802, volDnD))
	}
	twoSides := func(env *volEnv) {
		slice := env.slice()
		leg := grpLegOf(volGrpOf(env.t, slice, volDataGrpId), volDataLegA)
		leg.SideList = append(leg.SideList, volSide(801, volDnD))
		env.putSlice(slice)
	}
	healthy := func(env *volEnv) {
		repSetLegEpoch(env, volDataGrpId, volDataLegA, 0)
	}
	retiredBound := func(env *volEnv) {
		repPutRetired(env, volDataGrpId, 900, volSide(901, volDnC), 11)
		repPutRetired(env, volDataGrpId, 910, volSide(911, volDnD), 22)
	}
	repairMsg := fmt.Sprintf("group %d has the repair %q: a group has one "+
		"repair at a time", volDataGrpId, "rep-b")
	healthyMsg := fmt.Sprintf("leg %d is healthy: use a migration to move it",
		volDataLegA)
	boundMsg := fmt.Sprintf("group %d already holds the maximum of %d "+
		"retired legs: delete one (DeleteRetiredLeg) first",
		volDataGrpId, common.MaxRetiredLegPerGrp)
	workerName := common.WorkerRepName(volSpId, volDataGrpId, repEpoch)
	badSelector := &pb.NodeSelector{WhiteList: []string{"bad entry"}}
	badSelectorMsg := fmt.Sprintf("dn_selector.white_list %q does not match %s",
		"bad entry", common.ValidStrPattern)
	return []repRefusal{
		{
			name:  "rep_name empty",
			setup: func(_ *volEnv, req *pb.CreateRepairRequest) { req.RepName = "" },
			code:  codes.InvalidArgument,
			msg:   "rep_name must not be empty",
		},
		{
			name: "rep_name outside the pattern",
			setup: func(_ *volEnv, req *pb.CreateRepairRequest) {
				req.RepName = "rep a"
			},
			code: codes.InvalidArgument,
			msg: fmt.Sprintf("rep_name %q does not match %s", "rep a",
				common.ValidStrPattern),
		},
		{
			name: "rep_name too long",
			setup: func(_ *volEnv, req *pb.CreateRepairRequest) {
				req.RepName = strings.Repeat("r", common.MaxStrSize+1)
			},
			code: codes.InvalidArgument,
			msg: fmt.Sprintf("rep_name is %d bytes, the maximum is %d",
				common.MaxStrSize+1, common.MaxStrSize),
		},
		{
			name: "rep_name with the reserved prefix",
			setup: func(_ *volEnv, req *pb.CreateRepairRequest) {
				req.RepName = "__internal__rep"
			},
			code: codes.InvalidArgument,
			msg: fmt.Sprintf("rep_name %q starts with the reserved prefix %q",
				"__internal__rep", "__internal__"),
		},
		{
			name: "the worker's name",
			setup: func(_ *volEnv, req *pb.CreateRepairRequest) {
				req.RepName = workerName
			},
			code: codes.InvalidArgument,
			msg: fmt.Sprintf("rep_name %q starts with the reserved prefix %q",
				workerName, "__internal__"),
		},
		{
			name: "the reserved prefix before the level gate",
			setup: func(env *volEnv, req *pb.CreateRepairRequest) {
				req.RepName = workerName
				setLevel(env, pb.SpLevel_SP_LEVEL_DISABLE)
			},
			code: codes.InvalidArgument,
			msg: fmt.Sprintf("rep_name %q starts with the reserved prefix %q",
				workerName, "__internal__"),
		},
		{
			name: "no bad leg",
			setup: func(_ *volEnv, req *pb.CreateRepairRequest) {
				req.BadLegIdList = nil
			},
			code: codes.InvalidArgument,
			msg:  "bad_leg_id_list holds 0 leg ids, a repair replaces exactly one",
		},
		{
			name: "two bad legs",
			setup: func(_ *volEnv, req *pb.CreateRepairRequest) {
				req.BadLegIdList = []uint64{volDataLegA, volDataLegB}
			},
			code: codes.InvalidArgument,
			msg:  "bad_leg_id_list holds 2 leg ids, a repair replaces exactly one",
		},
		{
			name: "a bad selector",
			setup: func(_ *volEnv, req *pb.CreateRepairRequest) {
				req.DnSelector = badSelector
			},
			code: codes.InvalidArgument,
			msg:  badSelectorMsg,
		},
		{
			name: "a bad selector before a pool being deleted",
			setup: func(env *volEnv, req *pb.CreateRepairRequest) {
				req.DnSelector = badSelector
				setConf(env, func(conf *pb.SpConf) { conf.Deleting = true })
			},
			code: codes.InvalidArgument,
			msg:  badSelectorMsg,
		},
		{
			name: "a pool being deleted",
			setup: func(env *volEnv, _ *pb.CreateRepairRequest) {
				setConf(env, func(conf *pb.SpConf) { conf.Deleting = true })
			},
			code: codes.FailedPrecondition,
			msg:  fmt.Sprintf("storage pool %q is being deleted", volSpName),
		},
		{
			name:  "a level that builds no group array",
			setup: func(env *volEnv, _ *pb.CreateRepairRequest) { noRedund(env) },
			code:  codes.FailedPrecondition,
			msg: fmt.Sprintf("storage pool %q is at %s, where no repair is "+
				"created", volSpName, pb.SpLevel_SP_LEVEL_NO_REDUND),
		},
		{
			name: "the highest level",
			setup: func(env *volEnv, _ *pb.CreateRepairRequest) {
				setLevel(env, pb.SpLevel_SP_LEVEL_DISABLE)
			},
			code: codes.FailedPrecondition,
			msg: fmt.Sprintf("storage pool %q is at %s, where no repair is "+
				"created", volSpName, pb.SpLevel_SP_LEVEL_DISABLE),
		},
		{
			name: "the level before RedundNone",
			setup: func(env *volEnv, _ *pb.CreateRepairRequest) {
				noRedund(env)
				redundNone(env)
			},
			code: codes.FailedPrecondition,
			msg: fmt.Sprintf("storage pool %q is at %s, where no repair is "+
				"created", volSpName, pb.SpLevel_SP_LEVEL_NO_REDUND),
		},
		{
			name:  "a RedundNone pool",
			setup: func(env *volEnv, _ *pb.CreateRepairRequest) { redundNone(env) },
			code:  codes.InvalidArgument,
			msg: fmt.Sprintf("storage pool %q is RedundNone: there is no "+
				"redundancy to repair", volSpName),
		},
		{
			name: "RedundNone before a name in use",
			setup: func(env *volEnv, _ *pb.CreateRepairRequest) {
				metaRepair(env)
				redundNone(env)
			},
			code: codes.InvalidArgument,
			msg: fmt.Sprintf("storage pool %q is RedundNone: there is no "+
				"redundancy to repair", volSpName),
		},
		{
			name:  "a name in use",
			setup: func(env *volEnv, _ *pb.CreateRepairRequest) { metaRepair(env) },
			code:  codes.AlreadyExists,
			msg:   fmt.Sprintf("repair %q already exists", "rep-a"),
		},
		{
			name: "a name in use before a missing group",
			setup: func(env *volEnv, req *pb.CreateRepairRequest) {
				metaRepair(env)
				req.GrpId = 0xdead
			},
			code: codes.AlreadyExists,
			msg:  fmt.Sprintf("repair %q already exists", "rep-a"),
		},
		{
			name: "a missing group",
			setup: func(_ *volEnv, req *pb.CreateRepairRequest) {
				req.GrpId = 0xdead
			},
			code: codes.NotFound,
			msg: fmt.Sprintf("group %d not found in storage pool %q",
				0xdead, volSpName),
		},
		{
			name:  "a group with a repair",
			setup: func(env *volEnv, _ *pb.CreateRepairRequest) { grpRepair(env) },
			code:  codes.FailedPrecondition,
			msg:   repairMsg,
		},
		{
			name: "a group with a repair before a missing bad leg",
			setup: func(env *volEnv, req *pb.CreateRepairRequest) {
				grpRepair(env)
				req.BadLegIdList = []uint64{0xdead}
			},
			code: codes.FailedPrecondition,
			msg:  repairMsg,
		},
		{
			name: "a bad leg not in leg_list",
			setup: func(_ *volEnv, req *pb.CreateRepairRequest) {
				req.BadLegIdList = []uint64{0xdead}
			},
			code: codes.NotFound,
			msg: fmt.Sprintf("leg %d not found in the leg_list of group %d",
				0xdead, volDataGrpId),
		},
		{
			name:  "a bad leg with two sides",
			setup: func(env *volEnv, _ *pb.CreateRepairRequest) { twoSides(env) },
			code:  codes.FailedPrecondition,
			msg: fmt.Sprintf("leg %d already has 2 sides: a migration is "+
				"running on it", volDataLegA),
		},
		{
			name: "a bad leg with two sides before a healthy one",
			setup: func(env *volEnv, _ *pb.CreateRepairRequest) {
				twoSides(env)
				healthy(env)
			},
			code: codes.FailedPrecondition,
			msg: fmt.Sprintf("leg %d already has 2 sides: a migration is "+
				"running on it", volDataLegA),
		},
		{
			name:  "a healthy bad leg",
			setup: func(env *volEnv, _ *pb.CreateRepairRequest) { healthy(env) },
			code:  codes.FailedPrecondition,
			msg:   healthyMsg,
		},
		{
			name: "a healthy bad leg before an unlisted retired leg",
			setup: func(env *volEnv, req *pb.CreateRepairRequest) {
				healthy(env)
				req.RetiredLegId = 0xdead
			},
			code: codes.FailedPrecondition,
			msg:  healthyMsg,
		},
		{
			name: "a healthy bad leg before the retired-leg bound",
			setup: func(env *volEnv, _ *pb.CreateRepairRequest) {
				healthy(env)
				retiredBound(env)
			},
			code: codes.FailedPrecondition,
			msg:  healthyMsg,
		},
		{
			name: "a retired leg the pool does not list",
			setup: func(_ *volEnv, req *pb.CreateRepairRequest) {
				req.RetiredLegId = 0xdead
			},
			code: codes.NotFound,
			msg:  fmt.Sprintf("retired leg %d not found", 0xdead),
		},
		{
			name: "a retired leg of another group",
			setup: func(env *volEnv, req *pb.CreateRepairRequest) {
				repPutRetired(env, volMetaGrpId, 900,
					volSide(901, volDnC), 11)
				req.RetiredLegId = 900
			},
			code: codes.FailedPrecondition,
			msg: fmt.Sprintf("retired leg 900 is not a retired leg of "+
				"group %d", volDataGrpId),
		},
		{
			name: "a retired leg its group does not list",
			setup: func(env *volEnv, req *pb.CreateRepairRequest) {
				repPutRetired(env, volDataGrpId, 900,
					volSide(901, volDnC), 11)
				slice := env.slice()
				volGrpOf(env.t, slice, volDataGrpId).RetiredLegIdList = nil
				env.putSlice(slice)
				req.RetiredLegId = 900
			},
			code: codes.FailedPrecondition,
			msg: fmt.Sprintf("retired leg 900 is not a retired leg of "+
				"group %d", volDataGrpId),
		},
		{
			name: "an unhealthy retired side",
			setup: func(env *volEnv, req *pb.CreateRepairRequest) {
				side := volSide(901, volDnC)
				side.ErrEpoch = 77
				repPutRetired(env, volDataGrpId, 900, side, 11)
				req.RetiredLegId = 900
			},
			code: codes.FailedPrecondition,
			msg:  "the side of retired leg 900 is unhealthy (err_epoch 77)",
		},
		{
			name: "a retired side on the DN of a leg that stays",
			setup: func(env *volEnv, req *pb.CreateRepairRequest) {
				repPutRetired(env, volDataGrpId, 900,
					volSide(901, volDnB), 11)
				req.RetiredLegId = 900
			},
			code: codes.FailedPrecondition,
			msg: fmt.Sprintf("the side of retired leg 900 is on disk node "+
				"%q, which holds leg %d of group %d", volDnB, volDataLegB,
				volDataGrpId),
		},
		{
			name: "a fresh new leg for a group at the retired-leg bound",
			setup: func(env *volEnv, _ *pb.CreateRepairRequest) {
				retiredBound(env)
			},
			code: codes.ResourceExhausted,
			msg:  boundMsg,
		},
		{
			name: "the retired-leg bound before the scan",
			setup: func(env *volEnv, req *pb.CreateRepairRequest) {
				retiredBound(env)
				req.DnSelector = volDnSelector(volDnA)
			},
			code: codes.ResourceExhausted,
			msg:  boundMsg,
		},
		{
			name: "no DN for a fresh new leg",
			setup: func(_ *volEnv, req *pb.CreateRepairRequest) {
				req.DnSelector = volDnSelector(volDnA)
			},
			code: codes.ResourceExhausted,
			msg: fmt.Sprintf("new leg of the repair: 0 disk nodes with %d "+
				"free extents, need 1", volDataExtCnt),
		},
	}
}

// TestCreateRepairRefusals pins every refusal of CreateRepair
// (architecture.md, Repairs), its code and its words, each against a fresh
// fixture, and that it writes nothing anywhere in the cluster.
func TestCreateRepairRefusals(t *testing.T) {
	for _, tc := range repRefusals() {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			repSetLegEpoch(env, volDataGrpId, volDataLegA, repEpoch)
			req := repRequest(env)
			tc.setup(env, req)
			req.SpRev = env.token()
			before := env.dump()
			_, err := env.srv.CreateRepair(env.ctx, req)
			if msg := volWantCode(t, err, tc.code); msg != tc.msg {
				t.Errorf("message:\n got %q\nwant %q", msg, tc.msg)
			}
			env.wantNoWrite("the refused CreateRepair", before)
		})
	}
}

// TestCreateRepairRefusesADeletingClonesGroup pins the refusal of a group
// whose clone is being deleted (dnv-worker.md CLD1): its group goes back to
// its DNs in the clone drain's final transaction. The clone's refusal comes
// before the group's repair in the order of architecture.md, Repairs, so a
// group that also has a repair is refused for its clone.
func TestCreateRepairRefusesADeletingClonesGroup(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(env *volEnv) *pb.Group
	}{
		{"a deleting clone", repCloneBadLeg},
		{"a deleting clone before the group's repair", repCloneRepair},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			grp := tc.setup(env)
			volLatchClone(env, "clone-a")
			before := env.dump()
			_, err := env.srv.CreateRepair(env.ctx, &pb.CreateRepairRequest{
				ClusterName:  env.cluster,
				SpName:       volSpName,
				SpRev:        env.token(),
				RepName:      "rep-b",
				GrpId:        grp.GetGrpId(),
				BadLegIdList: []uint64{grp.GetLegList()[0].GetLegId()},
			})
			msg := volWantCode(t, err, codes.FailedPrecondition)
			if want := `clone "clone-a" is being deleted`; msg != want {
				t.Errorf("message: got %q, want %q", msg, want)
			}
			env.wantNoWrite("the refused CreateRepair", before)
		})
	}
}

// repRaceAtSpRev runs call — a repair call under the hook's ctx — and runs
// actor the moment call's planning snapshot has read the SpRev key, which
// openSp reads whether or not the call carries a token (GW6). The snapshot is
// pinned at its first read, so the planning read sees the store as it was
// before the actor, and the model op's own STM sees it after: the order in
// which only the op can answer, by its exported reasons or by its own token
// check.
func repRaceAtSpRev(
	t *testing.T,
	env *volEnv,
	actor func() error,
	call func(ctx context.Context) error,
) error {
	t.Helper()
	var actorErr error
	prev := slog.Default()
	hook := &volReadHook{
		Handler: prev.Handler(),
		key:     model.SpRevKey(volShard, env.cid, volSpId),
		actor:   func() { actorErr = actor() },
	}
	slog.SetDefault(slog.New(hook))
	defer slog.SetDefault(prev)
	ctx, cancel := context.WithTimeout(
		context.WithValue(env.ctx, volReadHookKey{}, true), 30*time.Second)
	defer cancel()
	err := call(ctx)
	if !hook.fired.Load() {
		t.Fatalf("the call never read the SpRev key: nothing was raced")
	}
	if actorErr != nil {
		t.Fatalf("the interleaved request: %v", actorErr)
	}
	return err
}

// TestCreateRepairAnswersTheOpsReasons pins the mapping of the reasons model
// exports for CreateRepair (gateway.md GW7): a name another create took, a
// retired leg another call deleted, and a retired list a finish filled, each
// after the planning read passed, reach the client with the codes and the
// words of the planning read's own refusals, and write nothing.
func TestCreateRepairAnswersTheOpsReasons(t *testing.T) {
	t.Run("name taken", func(t *testing.T) {
		env := newVolEnv(t)
		repSetLegEpoch(env, volDataGrpId, volDataLegA, repEpoch)
		repSetLegEpoch(env, volMetaGrpId, volMetaLegA, repEpoch)
		err := repRaceAtSpRev(t, env, func() error {
			_, err := env.srv.CreateRepair(env.ctx, &pb.CreateRepairRequest{
				ClusterName:  env.cluster,
				SpName:       volSpName,
				RepName:      "rep-a",
				GrpId:        volMetaGrpId,
				BadLegIdList: []uint64{volMetaLegA},
				DnSelector:   volDnSelector(volDnD),
			})
			return err
		}, func(ctx context.Context) error {
			req := repRequest(env)
			req.SpRev = nil
			_, err := env.srv.CreateRepair(ctx, req)
			return err
		})
		if msg := volWantCode(t, err, codes.AlreadyExists); msg !=
			fmt.Sprintf("repair %q already exists", "rep-a") {
			t.Errorf("message: got %q", msg)
		}
		if got := env.repair("rep-a").GetGrpId(); got != volMetaGrpId {
			t.Errorf("rep-a repairs group %d, want the other create's %d",
				got, volMetaGrpId)
		}
		if got := volGrpOf(t, env.slice(), volDataGrpId).GetRepName(); got != "" {
			t.Errorf("the data group names the repair %q", got)
		}
	})
	t.Run("retired leg gone", func(t *testing.T) {
		env := newVolEnv(t)
		repSetLegEpoch(env, volDataGrpId, volDataLegA, repEpoch)
		repPutRetired(env, volDataGrpId, 900, volSide(901, volDnC), 11)
		err := repRaceAtSpRev(t, env, func() error {
			_, err := env.srv.DeleteRetiredLeg(env.ctx,
				&pb.DeleteRetiredLegRequest{
					ClusterName: env.cluster,
					SpName:      volSpName,
					LegId:       900,
				})
			return err
		}, func(ctx context.Context) error {
			req := repRequest(env)
			req.SpRev = nil
			req.RetiredLegId = 900
			_, err := env.srv.CreateRepair(ctx, req)
			return err
		})
		if msg := volWantCode(t, err, codes.NotFound); msg !=
			"retired leg 900 not found" {
			t.Errorf("message: got %q", msg)
		}
	})
	t.Run("retired list filled", func(t *testing.T) {
		env := newVolEnv(t)
		repSetLegEpoch(env, volDataGrpId, volDataLegA, repEpoch)
		repSetLegEpoch(env, volDataGrpId, volDataLegB, repEpoch)
		repPutRetired(env, volDataGrpId, 900, volSide(901, volDnA), 11)
		// The other writer repairs leg B onto dn-d from start to finish,
		// which retires leg B and fills the group's retired list.
		err := repRaceAtSpRev(t, env, func() error {
			_, err := env.srv.CreateRepair(env.ctx, &pb.CreateRepairRequest{
				ClusterName:  env.cluster,
				SpName:       volSpName,
				RepName:      "rep-b",
				GrpId:        volDataGrpId,
				BadLegIdList: []uint64{volDataLegB},
				DnSelector:   volDnSelector(volDnD),
			})
			if err != nil {
				return err
			}
			if err := model.SetRepairStage(env.ctx, env.cli, env.cid,
				volShard, volSpId, volSpName, "rep-b",
				env.repair("rep-b").GetRepId(),
				pb.RepStage_REP_STAGE_STARTED); err != nil {
				return err
			}
			_, err = env.srv.FinishRepair(env.ctx, &pb.FinishRepairRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				RepName:     "rep-b",
			})
			return err
		}, func(ctx context.Context) error {
			req := repRequest(env)
			req.SpRev = nil
			_, err := env.srv.CreateRepair(ctx, req)
			return err
		})
		if msg := volWantCode(t, err, codes.ResourceExhausted); msg !=
			fmt.Sprintf("group %d already holds the maximum of %d "+
				"retired legs: delete one (DeleteRetiredLeg) first",
				volDataGrpId, common.MaxRetiredLegPerGrp) {
			t.Errorf("message: got %q", msg)
		}
		if env.exists(model.RepairKey(env.cid, volSpId, "rep-a"),
			&pb.Repair{}) {
			t.Errorf("the refused create wrote its repair")
		}
	})
}

// TestRepairMutatorsCheckTheTokenInTheirOp pins the second token check of
// GW6 on five of the six repair calls that write (gateway.md, Repairs and
// retired legs), the sixth's being TestForceStartGroupChecksTheTokenInItsOp:
// each hands the model op its token's revision, which the op takes as its
// expected revision before any other read. The planning read here
// passes the token, a token-less TryToAbortRepair of another repair then
// bumps SpRev before the op's STM runs, and the call must answer ABORTED
// "stale revision" and write nothing. A handler that handed the op 0 would
// skip that check and commit over a revision its client never read.
func TestRepairMutatorsCheckTheTokenInTheirOp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(env *volEnv)
		call  func(env *volEnv, ctx context.Context, tok *pb.SpRev) error
	}{
		{
			name: "CreateRepair",
			setup: func(env *volEnv) {
				repSetLegEpoch(env, volDataGrpId, volDataLegA, repEpoch)
			},
			call: func(env *volEnv, ctx context.Context, tok *pb.SpRev) error {
				req := repRequest(env)
				req.SpRev = tok
				_, err := env.srv.CreateRepair(ctx, req)
				return err
			},
		},
		{
			name: "FinishRepair",
			setup: func(env *volEnv) {
				repCreate(env, "rep-a", volDataLegA, volDnC)
				repMoveStage(env, "rep-a", pb.RepStage_REP_STAGE_STARTED)
			},
			call: func(env *volEnv, ctx context.Context, tok *pb.SpRev) error {
				_, err := env.srv.FinishRepair(ctx, &pb.FinishRepairRequest{
					ClusterName: env.cluster,
					SpName:      volSpName,
					SpRev:       tok,
					RepName:     "rep-a",
				})
				return err
			},
		},
		{
			name: "CancelRepair",
			setup: func(env *volEnv) {
				repCreate(env, "rep-a", volDataLegA, volDnC)
				if _, err := repAbort(env, "rep-a"); err != nil {
					env.t.Fatalf("TryToAbortRepair: %v", err)
				}
				repMoveStage(env, "rep-a", pb.RepStage_REP_STAGE_ABORTED)
			},
			call: func(env *volEnv, ctx context.Context, tok *pb.SpRev) error {
				_, err := env.srv.CancelRepair(ctx, &pb.CancelRepairRequest{
					ClusterName: env.cluster,
					SpName:      volSpName,
					SpRev:       tok,
					RepName:     "rep-a",
				})
				return err
			},
		},
		{
			name: "TryToAbortRepair",
			setup: func(env *volEnv) {
				repCreate(env, "rep-a", volDataLegA, volDnC)
			},
			call: func(env *volEnv, ctx context.Context, tok *pb.SpRev) error {
				_, err := env.srv.TryToAbortRepair(ctx,
					&pb.TryToAbortRepairRequest{
						ClusterName: env.cluster,
						SpName:      volSpName,
						SpRev:       tok,
						RepName:     "rep-a",
					})
				return err
			},
		},
		{
			name: "DeleteRetiredLeg",
			setup: func(env *volEnv) {
				repPutRetired(env, volDataGrpId, 900, volSide(901, volDnC), 11)
			},
			call: func(env *volEnv, ctx context.Context, tok *pb.SpRev) error {
				_, err := env.srv.DeleteRetiredLeg(ctx,
					&pb.DeleteRetiredLegRequest{
						ClusterName: env.cluster,
						SpName:      volSpName,
						SpRev:       tok,
						LegId:       900,
					})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			tc.setup(env)
			// The other writer's repair, on the meta group, so that its
			// abort flag moves SpRev and nothing the call reads.
			repPutRepair(env, "rep-z", volMetaGrpId, volMetaLegA,
				pb.RepStage_REP_STAGE_CREATED, 801, volSide(802, volDnD))
			tok := env.token()
			var before map[string][]byte
			err := repRaceAtSpRev(t, env, func() error {
				_, err := env.srv.TryToAbortRepair(env.ctx,
					&pb.TryToAbortRepairRequest{
						ClusterName: env.cluster,
						SpName:      volSpName,
						RepName:     "rep-z",
					})
				before = env.dump()
				return err
			}, func(ctx context.Context) error {
				return tc.call(env, ctx, tok)
			})
			if msg := volWantCode(t, err, codes.Aborted); msg !=
				msgStaleRevision {
				t.Errorf("message: got %q, want %q", msg, msgStaleRevision)
			}
			if rev := env.spRev(); rev != tok.GetRevision()+1 {
				t.Errorf("sp_rev: got %d, want the other writer's %d",
					rev, tok.GetRevision()+1)
			}
			env.wantNoWrite("the stale "+tc.name, before)
		})
	}
}

// TestFinishRepairRetiresTheBadLeg pins FinishRepair (architecture.md,
// Repairs): the new leg takes the bad leg's position in leg_list and its
// leg_idx, keeping its own id and side; the bad leg becomes a RetiredLeg
// under its own id with its side as it was, the group's id, its leg_idx, the
// slice and the gateway's clock as its retired_epoch, listed by the group
// and the pool; the repair's record, its list entry and the group's rep_name
// go; one SpRev bump. No DN moves: the bad leg's side keeps its pointer and
// its extents on dn-a, and the new leg's side was charged at the create. The
// group holds no retired leg before the finish, so the reply gives none back.
func TestFinishRepairRetiresTheBadLeg(t *testing.T) {
	env := newVolEnv(t)
	repCreate(env, "rep-a", volDataLegA, volDnC)
	repMoveStage(env, "rep-a", pb.RepStage_REP_STAGE_STARTED)
	badSide := grpLegOf(volGrpOf(t, env.slice(), volDataGrpId),
		volDataLegA).GetSideList()[0]
	newLeg := env.repair("rep-a").GetNewLegList()[0]
	dnA, dnC := env.dn(volDnA), env.dn(volDnC)
	revBefore := env.spRev()
	start := uint64(time.Now().Unix())
	reply, err := repFinish(env, "rep-a")
	end := uint64(time.Now().Unix())
	if err != nil {
		t.Fatalf("FinishRepair: %v", err)
	}
	if reply.GetRepId() != volNextId ||
		reply.GetRetiredLegId() != volDataLegA ||
		reply.GetGivenBackLegId() != 0 {
		t.Errorf("reply: rep_id %d, retired_leg_id %d, given_back_leg_id "+
			"%d, want %d, %d and 0", reply.GetRepId(),
			reply.GetRetiredLegId(), reply.GetGivenBackLegId(), volNextId,
			volDataLegA)
	}
	grp := volGrpOf(t, env.slice(), volDataGrpId)
	wantLeg := proto.Clone(newLeg).(*pb.Leg)
	wantLeg.LegIdx = 0
	if len(grp.GetLegList()) != 2 ||
		!proto.Equal(grp.GetLegList()[0], wantLeg) ||
		grp.GetLegList()[1].GetLegId() != volDataLegB {
		t.Errorf("leg_list: got %v, want [%v, leg %d]",
			grp.GetLegList(), wantLeg, volDataLegB)
	}
	if grp.GetRepName() != "" {
		t.Errorf("group rep_name: got %q, want none", grp.GetRepName())
	}
	retired := env.retiredLeg(volDataLegA)
	if retired.GetRetiredEpoch() < start || retired.GetRetiredEpoch() > end {
		t.Errorf("retired_epoch %d, want the gateway's clock in [%d, %d]",
			retired.GetRetiredEpoch(), start, end)
	}
	wantRetired := &pb.RetiredLeg{
		GrpId:        volDataGrpId,
		LegIdx:       0,
		Side:         badSide,
		RetiredEpoch: retired.GetRetiredEpoch(),
		SliceId:      volSliceId,
	}
	if !proto.Equal(retired, wantRetired) {
		t.Errorf("retired leg:\n got %v\nwant %v", retired, wantRetired)
	}
	conf := env.spConf()
	if len(conf.GetRepairNameList()) != 0 {
		t.Errorf("repair_name_list: got %v, want none",
			conf.GetRepairNameList())
	}
	for what, list := range map[string][]uint64{
		"group": grp.GetRetiredLegIdList(),
		"pool":  conf.GetRetiredLegIdList(),
	} {
		if len(list) != 1 || list[0] != volDataLegA {
			t.Errorf("the %s's retired_leg_id_list: got %v, want [%d]",
				what, list, volDataLegA)
		}
	}
	if env.exists(model.RepairKey(env.cid, volSpId, "rep-a"), &pb.Repair{}) {
		t.Errorf("the finished repair's key survived")
	}
	if got := env.dn(volDnA); !proto.Equal(got, dnA) {
		t.Errorf("dn-a moved:\n got %v\nwant %v", got, dnA)
	}
	if got := env.dn(volDnC); !proto.Equal(got, dnC) {
		t.Errorf("dn-c moved:\n got %v\nwant %v", got, dnC)
	}
	if rev := env.spRev(); rev != revBefore+1 {
		t.Errorf("sp_rev: got %d, want %d", rev, revBefore+1)
	}
}

// TestCancelRepairGivesTheFreshSideBack pins the abort exchange and
// CancelRepair with a fresh new leg (architecture.md, Repairs, Repair):
// TryToAbortRepair sets the flag with one bump and a second one writes
// nothing; the sp worker moves the flagged repair to ABORTED; the cancel
// gives the new leg's side back to dn-c — its extents, its pointer and its
// capacity key, with one DnRev bump — and the repair's record, list entry
// and the group's rep_name go, with one SpRev bump.
func TestCancelRepairGivesTheFreshSideBack(t *testing.T) {
	env := newVolEnv(t)
	repCreate(env, "rep-a", volDataLegA, volDnC)
	sideId := env.repair("rep-a").GetNewLegList()[0].GetSideList()[0].
		GetSideId()
	revBefore := env.spRev()
	reply, err := repAbort(env, "rep-a")
	if err != nil {
		t.Fatalf("TryToAbortRepair: %v", err)
	}
	if reply.GetRepId() != volNextId {
		t.Errorf("reply rep_id: got %d, want %d", reply.GetRepId(), volNextId)
	}
	rep := env.repair("rep-a")
	if !rep.GetTryToAbort() ||
		rep.GetRepStage() != pb.RepStage_REP_STAGE_CREATED {
		t.Errorf("after the abort: flag %v, stage %s; want true, CREATED",
			rep.GetTryToAbort(), rep.GetRepStage())
	}
	if rev := env.spRev(); rev != revBefore+1 {
		t.Errorf("sp_rev after the abort: got %d, want %d", rev, revBefore+1)
	}
	before := env.dump()
	if _, err := repAbort(env, "rep-a"); err != nil {
		t.Fatalf("a second TryToAbortRepair: %v", err)
	}
	env.wantNoWrite("the second TryToAbortRepair", before)
	repMoveStage(env, "rep-a", pb.RepStage_REP_STAGE_ABORTED)
	revBefore = env.spRev()
	cancel, err := repCancel(env, "rep-a")
	if err != nil {
		t.Fatalf("CancelRepair: %v", err)
	}
	if cancel.GetRepId() != volNextId {
		t.Errorf("reply rep_id: got %d, want %d", cancel.GetRepId(), volNextId)
	}
	repWantDn(t, env, volDnC, volDnFree, 3, sideId, false)
	if env.exists(model.RepairKey(env.cid, volSpId, "rep-a"), &pb.Repair{}) {
		t.Errorf("the cancelled repair's key survived")
	}
	if got := volGrpOf(t, env.slice(), volDataGrpId).GetRepName(); got != "" {
		t.Errorf("group rep_name: got %q, want none", got)
	}
	if got := env.spConf().GetRepairNameList(); len(got) != 0 {
		t.Errorf("repair_name_list: got %v, want none", got)
	}
	if rev := env.spRev(); rev != revBefore+1 {
		t.Errorf("sp_rev after the cancel: got %d, want %d", rev, revBefore+1)
	}
}

// TestCancelRepairGivesTheRetiredLegBack pins the cancel of a repair whose
// new leg was a retired leg (architecture.md, Repairs): the leg goes back as
// that retired leg, with the retired_epoch the repair kept, its own leg_idx,
// its side and the slice, listed again by the group and the pool, and its DN
// keeps its side untouched.
func TestCancelRepairGivesTheRetiredLegBack(t *testing.T) {
	env := newVolEnv(t)
	repSetLegEpoch(env, volDataGrpId, volDataLegA, repEpoch)
	retired := repPutRetired(env, volDataGrpId, 900, volSide(901, volDnC), 11)
	dnC := env.dn(volDnC)
	req := repRequest(env)
	req.RetiredLegId = 900
	if _, err := env.srv.CreateRepair(env.ctx, req); err != nil {
		t.Fatalf("CreateRepair: %v", err)
	}
	if _, err := repAbort(env, "rep-a"); err != nil {
		t.Fatalf("TryToAbortRepair: %v", err)
	}
	repMoveStage(env, "rep-a", pb.RepStage_REP_STAGE_ABORTED)
	if _, err := repCancel(env, "rep-a"); err != nil {
		t.Fatalf("CancelRepair: %v", err)
	}
	if got := env.retiredLeg(900); !proto.Equal(got, retired) {
		t.Errorf("the retired leg back:\n got %v\nwant %v", got, retired)
	}
	grp := volGrpOf(t, env.slice(), volDataGrpId)
	conf := env.spConf()
	for what, list := range map[string][]uint64{
		"group": grp.GetRetiredLegIdList(),
		"pool":  conf.GetRetiredLegIdList(),
	} {
		if len(list) != 1 || list[0] != 900 {
			t.Errorf("the %s's retired_leg_id_list: got %v, want [900]",
				what, list)
		}
	}
	if got := env.dn(volDnC); !proto.Equal(got, dnC) {
		t.Errorf("dn-c moved:\n got %v\nwant %v", got, dnC)
	}
}

// TestRepairCallsRefuseTheWrongStage pins the stage rule of architecture.md,
// Repair: FinishRepair takes a STARTED repair, CancelRepair an ABORTED one
// and TryToAbortRepair a CREATED one; every other stage is
// FAILED_PRECONDITION naming the stage, with nothing written.
func TestRepairCallsRefuseTheWrongStage(t *testing.T) {
	calls := map[string]func(env *volEnv) error{
		"FinishRepair": func(env *volEnv) error {
			_, err := repFinish(env, "rep-a")
			return err
		},
		"CancelRepair": func(env *volEnv) error {
			_, err := repCancel(env, "rep-a")
			return err
		},
		"TryToAbortRepair": func(env *volEnv) error {
			_, err := repAbort(env, "rep-a")
			return err
		},
	}
	for _, tc := range []struct {
		call  string
		stage pb.RepStage
	}{
		{"FinishRepair", pb.RepStage_REP_STAGE_CREATED},
		{"FinishRepair", pb.RepStage_REP_STAGE_ABORTED},
		{"CancelRepair", pb.RepStage_REP_STAGE_CREATED},
		{"CancelRepair", pb.RepStage_REP_STAGE_STARTED},
		{"TryToAbortRepair", pb.RepStage_REP_STAGE_STARTED},
		{"TryToAbortRepair", pb.RepStage_REP_STAGE_ABORTED},
	} {
		t.Run(tc.call+"/"+tc.stage.String(), func(t *testing.T) {
			env := newVolEnv(t)
			repCreate(env, "rep-a", volDataLegA, volDnC)
			switch tc.stage {
			case pb.RepStage_REP_STAGE_STARTED:
				repMoveStage(env, "rep-a", tc.stage)
			case pb.RepStage_REP_STAGE_ABORTED:
				if _, err := repAbort(env, "rep-a"); err != nil {
					t.Fatalf("TryToAbortRepair: %v", err)
				}
				repMoveStage(env, "rep-a", tc.stage)
			}
			before := env.dump()
			msg := volWantCode(t, calls[tc.call](env),
				codes.FailedPrecondition)
			if !strings.HasSuffix(msg, "repair stage is "+tc.stage.String()) {
				t.Errorf("message %q does not name the stage %s", msg,
					tc.stage)
			}
			env.wantNoWrite("the refused "+tc.call, before)
		})
	}
}

// TestRepairCallsRefuseUnknownNames pins the NOT_FOUND of a name or a leg id
// the pool does not list (architecture.md, Repairs, Retired legs; GW7): the
// reasons model exports for them reach the client as NOT_FOUND with the
// gateway's words, and nothing is written. A name with the reserved prefix
// is NOT_FOUND too, not INVALID_ARGUMENT: the prefix is refused for the
// rep_name of CreateRepair alone (architecture.md, Common validation), so
// the calls that address a repair by name pass it to the look-up.
func TestRepairCallsRefuseUnknownNames(t *testing.T) {
	env := newVolEnv(t)
	repCreate(env, "rep-a", volDataLegA, volDnC)
	before := env.dump()
	for _, repName := range []string{"nosuch", "__internal__x"} {
		wantRep := fmt.Sprintf("repair %q not found", repName)
		for name, call := range map[string]func() error{
			"FinishRepair": func() error {
				_, err := repFinish(env, repName)
				return err
			},
			"CancelRepair": func() error {
				_, err := repCancel(env, repName)
				return err
			},
			"TryToAbortRepair": func() error {
				_, err := repAbort(env, repName)
				return err
			},
			"GetRepair": func() error {
				_, err := env.srv.GetRepair(env.ctx, &pb.GetRepairRequest{
					ClusterName: env.cluster,
					SpName:      volSpName,
					RepName:     repName,
				})
				return err
			},
		} {
			if msg := volWantCode(t, call(), codes.NotFound); msg != wantRep {
				t.Errorf("%s %s: message %q, want %q", name, repName, msg,
					wantRep)
			}
		}
	}
	_, err := repDeleteRetired(env, 0xdead)
	if msg := volWantCode(t, err, codes.NotFound); msg !=
		fmt.Sprintf("retired leg %d not found", 0xdead) {
		t.Errorf("DeleteRetiredLeg: message %q", msg)
	}
	env.wantNoWrite("the refused calls", before)
}

// TestRepairCallsOnADeletingPool pins the deleting gate (architecture.md,
// Repairs; GW5): every call that writes, ForceStartGroup's write and its
// cancel among them, is FAILED_PRECONDITION on a pool being deleted, with
// nothing written, and the three reads still answer, so an operator sees the
// repairs and retired legs the drain is about to delete.
func TestRepairCallsOnADeletingPool(t *testing.T) {
	env := newVolEnv(t)
	repCreate(env, "rep-a", volDataLegA, volDnC)
	repPutRetired(env, volMetaGrpId, 900, volSide(901, volDnD), 11)
	conf := env.spConf()
	conf.Deleting = true
	env.putSpConf(conf)
	before := env.dump()
	want := fmt.Sprintf("storage pool %q is being deleted", volSpName)
	for name, call := range map[string]func() error{
		"CreateRepair": func() error {
			req := repRequest(env)
			req.RepName = "rep-b"
			_, err := env.srv.CreateRepair(env.ctx, req)
			return err
		},
		"FinishRepair": func() error {
			_, err := repFinish(env, "rep-a")
			return err
		},
		"CancelRepair": func() error {
			_, err := repCancel(env, "rep-a")
			return err
		},
		"TryToAbortRepair": func() error {
			_, err := repAbort(env, "rep-a")
			return err
		},
		"DeleteRetiredLeg": func() error {
			_, err := repDeleteRetired(env, 900)
			return err
		},
		"ForceStartGroup": func() error {
			_, err := env.srv.ForceStartGroup(env.ctx,
				fsRequest(env, volDataLegA))
			return err
		},
		"ForceStartGroup with cancel": func() error {
			_, err := env.srv.ForceStartGroup(env.ctx, fsCancel(env))
			return err
		},
	} {
		msg := volWantCode(t, call(), codes.FailedPrecondition)
		if msg != want {
			t.Errorf("%s: message %q, want %q", name, msg, want)
		}
	}
	env.wantNoWrite("the refused calls", before)
	get, err := env.srv.GetRepair(env.ctx, &pb.GetRepairRequest{
		ClusterName: env.cluster, SpName: volSpName, RepName: "rep-a",
	})
	if err != nil || get.GetRepair().GetRepId() != volNextId {
		t.Errorf("GetRepair on a deleting pool: %v, %v", get, err)
	}
	list, err := env.srv.ListRepairs(env.ctx, &pb.ListRepairsRequest{
		ClusterName: env.cluster, SpName: volSpName,
	})
	if err != nil || len(list.GetNameToRepair()) != 1 {
		t.Errorf("ListRepairs on a deleting pool: %v, %v", list, err)
	}
	retired, err := env.srv.ListRetiredLegs(env.ctx,
		&pb.ListRetiredLegsRequest{
			ClusterName: env.cluster, SpName: volSpName,
		})
	if err != nil || len(retired.GetLegIdToRetiredLeg()) != 1 {
		t.Errorf("ListRetiredLegs on a deleting pool: %v, %v", retired, err)
	}
}

// repCloneBadLeg creates the clone "clone-a" with leg 0 of its clone meta
// group bad, and returns the group.
func repCloneBadLeg(env *volEnv) *pb.Group {
	env.t.Helper()
	env.putTd("dst", 900, 7, 0, true)
	volCreateClone(env, "clone-a", "dst")
	clone := env.clone("clone-a")
	clone.GetMetaGrp().GetLegList()[0].ErrEpoch = repEpoch
	mustPut(env.t, env.cli, model.CloneKey(env.cid, volSpId, "clone-a"),
		clone)
	return clone.GetMetaGrp()
}

// repCloneRepair creates the clone "clone-a" and the repair "rep-a" of its
// clone meta group with a fresh new leg, leg 0 bad, and returns the group.
func repCloneRepair(env *volEnv) *pb.Group {
	env.t.Helper()
	grp := repCloneBadLeg(env)
	if _, err := env.srv.CreateRepair(env.ctx, &pb.CreateRepairRequest{
		ClusterName:  env.cluster,
		SpName:       volSpName,
		SpRev:        env.token(),
		RepName:      "rep-a",
		GrpId:        grp.GetGrpId(),
		BadLegIdList: []uint64{grp.GetLegList()[0].GetLegId()},
	}); err != nil {
		env.t.Fatalf("CreateRepair on the clone meta group: %v", err)
	}
	return grp
}

// TestRepairCallsRefuseADeletingClone pins the refusal of the calls that
// write a deleting clone's existing repair or retired leg (architecture.md,
// Repairs; dnv-worker.md CLD1), from model's own check — the clone drain's
// final transaction gives the group back with its repair and its retired
// legs — with nothing written. Each call meets the repair at the stage it
// takes, so the clone's state is what refuses it. CreateRepair and
// ForceStartGroup, whose planning reads refuse a deleting clone in the
// gateway's words before their ops run, have theirs in
// TestCreateRepairRefusesADeletingClonesGroup and, the write and the cancel,
// TestForceStartGroupOnACloneMetaGroup.
func TestRepairCallsRefuseADeletingClone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage pb.RepStage
		call  func(env *volEnv) error
	}{
		{"FinishRepair", pb.RepStage_REP_STAGE_STARTED,
			func(env *volEnv) error {
				_, err := repFinish(env, "rep-a")
				return err
			}},
		{"CancelRepair", pb.RepStage_REP_STAGE_ABORTED,
			func(env *volEnv) error {
				_, err := repCancel(env, "rep-a")
				return err
			}},
		{"TryToAbortRepair", pb.RepStage_REP_STAGE_CREATED,
			func(env *volEnv) error {
				_, err := repAbort(env, "rep-a")
				return err
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			repCloneRepair(env)
			switch tc.stage {
			case pb.RepStage_REP_STAGE_STARTED:
				repMoveStage(env, "rep-a", tc.stage)
			case pb.RepStage_REP_STAGE_ABORTED:
				if _, err := repAbort(env, "rep-a"); err != nil {
					t.Fatalf("TryToAbortRepair: %v", err)
				}
				repMoveStage(env, "rep-a", tc.stage)
			}
			volLatchClone(env, "clone-a")
			before := env.dump()
			msg := volWantCode(t, tc.call(env), codes.FailedPrecondition)
			if !strings.HasSuffix(msg, "clone deleting") {
				t.Errorf("message %q does not name the deleting clone", msg)
			}
			env.wantNoWrite("the refused "+tc.name, before)
		})
	}
	t.Run("DeleteRetiredLeg", func(t *testing.T) {
		env := newVolEnv(t)
		repCloneRepair(env)
		repMoveStage(env, "rep-a", pb.RepStage_REP_STAGE_STARTED)
		finish, err := repFinish(env, "rep-a")
		if err != nil {
			t.Fatalf("FinishRepair: %v", err)
		}
		volLatchClone(env, "clone-a")
		before := env.dump()
		_, err = repDeleteRetired(env, finish.GetRetiredLegId())
		msg := volWantCode(t, err, codes.FailedPrecondition)
		if !strings.HasSuffix(msg, "clone deleting") {
			t.Errorf("message %q does not name the deleting clone", msg)
		}
		env.wantNoWrite("the refused DeleteRetiredLeg", before)
	})
}

// TestRepairCallsRunAtEveryLevel pins the levels of architecture.md, Repairs:
// only CreateRepair refuses a level, from SP_LEVEL_NO_REDUND up for the
// operator's names it takes, so it still creates a repair at
// SP_LEVEL_NO_THINPOOL, the highest level below that, where the operator is
// in charge; and at the highest level an operator still aborts, cancels and
// finishes a repair and deletes a retired leg.
func TestRepairCallsRunAtEveryLevel(t *testing.T) {
	env := newVolEnv(t)
	setLevel := func(level pb.SpLevel) {
		conf := env.spConf()
		conf.SpLevel = level
		env.putSpConf(conf)
	}
	setLevel(pb.SpLevel_SP_LEVEL_NO_THINPOOL)
	repCreate(env, "rep-a", volDataLegA, volDnC)
	setLevel(pb.SpLevel_SP_LEVEL_DISABLE)
	if _, err := repAbort(env, "rep-a"); err != nil {
		t.Fatalf("TryToAbortRepair at DISABLE: %v", err)
	}
	repMoveStage(env, "rep-a", pb.RepStage_REP_STAGE_ABORTED)
	if _, err := repCancel(env, "rep-a"); err != nil {
		t.Fatalf("CancelRepair at DISABLE: %v", err)
	}
	setLevel(pb.SpLevel_SP_LEVEL_READWRITE)
	repCreate(env, "rep-b", volDataLegA, volDnC)
	setLevel(pb.SpLevel_SP_LEVEL_DISABLE)
	repMoveStage(env, "rep-b", pb.RepStage_REP_STAGE_STARTED)
	finish, err := repFinish(env, "rep-b")
	if err != nil {
		t.Fatalf("FinishRepair at DISABLE: %v", err)
	}
	if _, err := repDeleteRetired(env, finish.GetRetiredLegId()); err != nil {
		t.Fatalf("DeleteRetiredLeg at DISABLE: %v", err)
	}
}

// TestCreateRepairLevelGate pins CreateRepair's level gate at its boundary,
// for both kinds of name (architecture.md, Repairs;
// model.CreateRepairLevelGate). An operator's name is created at
// SP_LEVEL_NO_THINPOOL and refused FAILED_PRECONDITION from
// SP_LEVEL_NO_REDUND up, where no group array is built. The sp worker's name
// is INVALID_ARGUMENT at every level, below the gate and above it alike: the
// rep_name rule comes first, so the gateway never creates one.
func TestCreateRepairLevelGate(t *testing.T) {
	workerName := common.WorkerRepName(volSpId, volDataGrpId, repEpoch)
	for _, tc := range []struct {
		repName string
		level   pb.SpLevel
		code    codes.Code
	}{
		{"rep-a", pb.SpLevel_SP_LEVEL_NO_THINPOOL, codes.OK},
		{"rep-a", pb.SpLevel_SP_LEVEL_NO_REDUND, codes.FailedPrecondition},
		{"rep-a", pb.SpLevel_SP_LEVEL_NO_MIGRATION, codes.FailedPrecondition},
		{workerName, pb.SpLevel_SP_LEVEL_READWRITE, codes.InvalidArgument},
		{workerName, pb.SpLevel_SP_LEVEL_NO_CLONE, codes.InvalidArgument},
		{workerName, pb.SpLevel_SP_LEVEL_NO_THINPOOL, codes.InvalidArgument},
		{workerName, pb.SpLevel_SP_LEVEL_NO_REDUND, codes.InvalidArgument},
	} {
		t.Run(tc.repName+"/"+tc.level.String(), func(t *testing.T) {
			env := newVolEnv(t)
			repSetLegEpoch(env, volDataGrpId, volDataLegA, repEpoch)
			conf := env.spConf()
			conf.SpLevel = tc.level
			env.putSpConf(conf)
			req := repRequest(env)
			req.RepName = tc.repName
			before := env.dump()
			_, err := env.srv.CreateRepair(env.ctx, req)
			if tc.code == codes.OK {
				if err != nil {
					t.Fatalf("CreateRepair at %s: %v", tc.level, err)
				}
				if got := volGrpOf(t, env.slice(), volDataGrpId).
					GetRepName(); got != tc.repName {
					t.Errorf("the group names the repair %q, want %q", got,
						tc.repName)
				}
				return
			}
			msg := volWantCode(t, err, tc.code)
			want := fmt.Sprintf("storage pool %q is at %s, where no repair "+
				"is created", volSpName, tc.level)
			if tc.code == codes.InvalidArgument {
				want = fmt.Sprintf("rep_name %q starts with the reserved "+
					"prefix %q", tc.repName, common.RepNameReservedPrefix)
			}
			if msg != want {
				t.Errorf("message:\n got %q\nwant %q", msg, want)
			}
			env.wantNoWrite("the refused CreateRepair", before)
		})
	}
}

// TestFinishRepairGivesBackForTheWorkersRepair pins FinishRepair on a repair
// the sp worker created, which the call takes by its name (architecture.md,
// Repairs): with its group at MaxRetiredLegPerGrp retired legs, the finish
// gives back the oldest one, its record and list entries gone and its side
// back on its DN with one DnRev bump, before the bad leg joins the lists. The
// reply names the finished repair, the retired bad leg and the retired leg
// given back, so an operator who finishes a worker's repair learns which kept
// copy went; the stored conf the gateway hands the op prices the DN's
// capacity key.
func TestFinishRepairGivesBackForTheWorkersRepair(t *testing.T) {
	env := newVolEnv(t)
	if common.MaxRetiredLegPerGrp != 2 {
		t.Fatalf("the case plants 2 retired legs, the bound is %d",
			common.MaxRetiredLegPerGrp)
	}
	workerName := common.WorkerRepName(volSpId, volDataGrpId, repEpoch)
	repPutRetired(env, volDataGrpId, 910, volSide(911, volDnD), 22)
	repPutRetired(env, volDataGrpId, 900, volSide(901, volDnC), 11)
	repPutRepair(env, workerName, volDataGrpId, volDataLegA,
		pb.RepStage_REP_STAGE_STARTED, 801, volSide(802, volDnD))
	freeC, revC := env.dn(volDnC).GetFreeExtCnt(), env.dnRev(volDnC)
	dnD, revD := env.dn(volDnD), env.dnRev(volDnD)
	kept := env.retiredLeg(910)
	reply, err := repFinish(env, workerName)
	if err != nil {
		t.Fatalf("FinishRepair: %v", err)
	}
	if reply.GetRepId() != 800 || reply.GetRetiredLegId() != volDataLegA ||
		reply.GetGivenBackLegId() != 900 {
		t.Errorf("reply: rep_id %d, retired_leg_id %d, given_back_leg_id "+
			"%d, want 800, %d and 900", reply.GetRepId(),
			reply.GetRetiredLegId(), reply.GetGivenBackLegId(), volDataLegA)
	}
	if env.exists(model.RetiredLegKey(env.cid, volSpId, 900),
		&pb.RetiredLeg{}) {
		t.Errorf("the oldest retired leg survived the finish")
	}
	if got := env.retiredLeg(910); !proto.Equal(got, kept) {
		t.Errorf("the younger retired leg: got %v, want %v", got, kept)
	}
	want := []uint64{910, volDataLegA}
	grp := volGrpOf(t, env.slice(), volDataGrpId)
	conf := env.spConf()
	for what, list := range map[string][]uint64{
		"group": grp.GetRetiredLegIdList(),
		"pool":  conf.GetRetiredLegIdList(),
	} {
		if fmt.Sprint(list) != fmt.Sprint(want) {
			t.Errorf("the %s's retired_leg_id_list: got %v, want %v",
				what, list, want)
		}
	}
	repWantDn(t, env, volDnC, freeC+volDataExtCnt, revC+1, 901, false)
	if got := env.dn(volDnD); !proto.Equal(got, dnD) ||
		env.dnRev(volDnD) != revD {
		t.Errorf("dn-d, the younger retired side's DN, moved")
	}
}

// TestGetAndListRepairs pins the three reads (architecture.md, Repairs,
// Retired legs): each reply is the records as stored, by name or by leg id;
// an empty list is an empty map; and a listed key that is missing is a lost
// invariant key, ABORTED.
func TestGetAndListRepairs(t *testing.T) {
	env := newVolEnv(t)
	list := func() (*pb.ListRepairsReply, error) {
		return env.srv.ListRepairs(env.ctx, &pb.ListRepairsRequest{
			ClusterName: env.cluster, SpName: volSpName,
		})
	}
	listRetired := func() (*pb.ListRetiredLegsReply, error) {
		return env.srv.ListRetiredLegs(env.ctx, &pb.ListRetiredLegsRequest{
			ClusterName: env.cluster, SpName: volSpName,
		})
	}
	if got, err := list(); err != nil || len(got.GetNameToRepair()) != 0 {
		t.Fatalf("ListRepairs of a pool with none: %v, %v", got, err)
	}
	if got, err := listRetired(); err != nil ||
		len(got.GetLegIdToRetiredLeg()) != 0 {
		t.Fatalf("ListRetiredLegs of a pool with none: %v, %v", got, err)
	}
	repCreate(env, "rep-a", volDataLegA, volDnC)
	repSetLegEpoch(env, volMetaGrpId, volMetaLegA, repEpoch)
	req := repRequest(env)
	req.RepName = "rep-b"
	req.GrpId = volMetaGrpId
	req.BadLegIdList = []uint64{volMetaLegA}
	req.DnSelector = volDnSelector(volDnD)
	if _, err := env.srv.CreateRepair(env.ctx, req); err != nil {
		t.Fatalf("CreateRepair rep-b: %v", err)
	}
	retired := repPutRetired(env, volMetaGrpId, 900, volSide(901, volDnC), 11)
	get, err := env.srv.GetRepair(env.ctx, &pb.GetRepairRequest{
		ClusterName: env.cluster, SpName: volSpName, RepName: "rep-a",
	})
	if err != nil {
		t.Fatalf("GetRepair: %v", err)
	}
	if get.GetRepName() != "rep-a" ||
		!proto.Equal(get.GetRepair(), env.repair("rep-a")) {
		t.Errorf("GetRepair: got %v, want rep-a %v", get, env.repair("rep-a"))
	}
	got, err := list()
	if err != nil {
		t.Fatalf("ListRepairs: %v", err)
	}
	want := map[string]*pb.Repair{
		"rep-a": env.repair("rep-a"),
		"rep-b": env.repair("rep-b"),
	}
	if !proto.Equal(&pb.ListRepairsReply{NameToRepair: want}, got) {
		t.Errorf("ListRepairs:\n got %v\nwant %v", got.GetNameToRepair(), want)
	}
	gotRetired, err := listRetired()
	if err != nil {
		t.Fatalf("ListRetiredLegs: %v", err)
	}
	if !proto.Equal(gotRetired, &pb.ListRetiredLegsReply{
		LegIdToRetiredLeg: map[uint64]*pb.RetiredLeg{900: retired},
	}) {
		t.Errorf("ListRetiredLegs: got %v", gotRetired)
	}
	for _, key := range []string{
		model.RepairKey(env.cid, volSpId, "rep-b"),
		model.RetiredLegKey(env.cid, volSpId, 900),
	} {
		if err := env.cli.Delete(env.ctx, key); err != nil {
			t.Fatalf("Delete %s: %v", key, err)
		}
	}
	_, err = env.srv.GetRepair(env.ctx, &pb.GetRepairRequest{
		ClusterName: env.cluster, SpName: volSpName, RepName: "rep-b",
	})
	volWantCode(t, err, codes.Aborted)
	_, err = list()
	volWantCode(t, err, codes.Aborted)
	_, err = listRetired()
	volWantCode(t, err, codes.Aborted)
}

// TestDeleteRetiredLegGivesTheSideBack pins DeleteRetiredLeg
// (architecture.md, Retired legs): the retired side gives its DN the group's
// extents, its pointer and its capacity key back with one DnRev bump; the
// record and both list entries go; one SpRev bump; the reply is the leg id.
func TestDeleteRetiredLegGivesTheSideBack(t *testing.T) {
	env := newVolEnv(t)
	repPutRetired(env, volDataGrpId, 900, volSide(901, volDnC), 11)
	repPutRetired(env, volDataGrpId, 910, volSide(911, volDnD), 22)
	revBefore := env.spRev()
	reply, err := repDeleteRetired(env, 900)
	if err != nil {
		t.Fatalf("DeleteRetiredLeg: %v", err)
	}
	if reply.GetLegId() != 900 {
		t.Errorf("reply leg_id: got %d, want 900", reply.GetLegId())
	}
	repWantDn(t, env, volDnC, volDnFree, 2, 901, false)
	repWantDn(t, env, volDnD, volDnFree-volDataExtCnt, 1, 911, true)
	if env.exists(model.RetiredLegKey(env.cid, volSpId, 900),
		&pb.RetiredLeg{}) {
		t.Errorf("the deleted retired leg's key survived")
	}
	grp := volGrpOf(t, env.slice(), volDataGrpId)
	conf := env.spConf()
	for what, list := range map[string][]uint64{
		"group": grp.GetRetiredLegIdList(),
		"pool":  conf.GetRetiredLegIdList(),
	} {
		if len(list) != 1 || list[0] != 910 {
			t.Errorf("the %s's retired_leg_id_list: got %v, want [910]",
				what, list)
		}
	}
	if rev := env.spRev(); rev != revBefore+1 {
		t.Errorf("sp_rev: got %d, want %d", rev, revBefore+1)
	}
}

// TestCreateMigrationAndARepair pins CreateMigration beside a repair
// (architecture.md, Migrations): a side of the repair's bad leg is refused
// FAILED_PRECONDITION, since the repair replaces that leg; the side of the
// repair's new leg and a retired side are no leg's, so they are NOT_FOUND;
// and the repair's new leg belongs to the group's DN set, so with dn-d gone
// the migration of the group's other leg has no DN left.
func TestCreateMigrationAndARepair(t *testing.T) {
	env := newVolEnv(t)
	repCreate(env, "rep-a", volDataLegA, volDnC)
	newSideId := env.repair("rep-a").GetNewLegList()[0].GetSideList()[0].
		GetSideId()
	repPutRetired(env, volMetaGrpId, 900, volSide(901, volDnD), 11)
	migrate := func(srcSideId uint64) error {
		_, err := env.srv.CreateMigration(env.ctx,
			&pb.CreateMigrationRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				SpRev:       env.token(),
				MigrName:    "migr-a",
				SrcSideId:   srcSideId,
			})
		return err
	}
	before := env.dump()
	msg := volWantCode(t, migrate(volDataSideA), codes.FailedPrecondition)
	want := fmt.Sprintf("side %d belongs to leg %d, the bad leg of the "+
		"repair %q, which replaces it", volDataSideA, volDataLegA, "rep-a")
	if msg != want {
		t.Errorf("bad leg: message %q, want %q", msg, want)
	}
	for _, sideId := range []uint64{newSideId, 901} {
		msg := volWantCode(t, migrate(sideId), codes.NotFound)
		want := fmt.Sprintf("side %d is not in any leg of storage pool %q",
			sideId, volSpName)
		if msg != want {
			t.Errorf("side %d: message %q, want %q", sideId, msg, want)
		}
	}
	env.wantNoWrite("the refused migrations", before)
	env.dropDn(volDnD)
	before = env.dump()
	volWantCode(t, migrate(volDataSideB), codes.ResourceExhausted)
	env.wantNoWrite("the migration with no DN left", before)
}

// TestCreateMigrationReplansAroundARepairsNewLeg pins the deciding STM's
// check of the round's plan against the group's DN set with its repair's new
// leg in it (architecture.md, Migrations, Per-operation allocation): a repair
// of leg B created after the migration of leg A planned puts its new leg on
// dn-c, which the plan did not hold, and the scan still offers dn-c. The
// round is a changed candidate, and the next one plans from the group and
// its repair as they now stand, so the destination lands on dn-d, through
// tier 2. Every draw is forced, as in
// TestCreateMigrationRefusesADnTheGroupAlreadyUses.
func TestCreateMigrationReplansAroundARepairsNewLeg(t *testing.T) {
	env := newVolEnv(t)
	env.relocateDn(volDnD, volDnC)
	env.setDnFree(volDnC, volDnCFree)
	repSetLegEpoch(env, volDataGrpId, volDataLegB, repEpoch)
	got := volRaceMigrationA(t, env, func() error {
		_, err := env.srv.CreateRepair(env.ctx, &pb.CreateRepairRequest{
			ClusterName:  env.cluster,
			SpName:       volSpName,
			RepName:      "rep-b",
			GrpId:        volDataGrpId,
			BadLegIdList: []uint64{volDataLegB},
			DnSelector:   volDnSelector(volDnC),
		})
		return err
	})
	if got != volDnD {
		t.Errorf("leg %d destination on %s, want %s", volDataLegA, got,
			volDnD)
	}
	side := env.repair("rep-b").GetNewLegList()[0].GetSideList()[0]
	if side.GetAddrPort() != volDnC {
		t.Fatalf("the repair's new leg on %s, want %s", side.GetAddrPort(),
			volDnC)
	}
	if rev := env.spRev(); rev != 3 {
		t.Errorf("sp_rev: got %d, want 3 (one bump per request)", rev)
	}
}

// TestCreateMigrationKeepsOffARepairsNewLegsDomain pins the tier-1 location
// exclusion of a migration's plan with the group's repair in it
// (architecture.md, Per-operation allocation): the new leg of leg A's repair
// sits on dn-c, dn-d shares dn-c's failure domain, and dn-e has a domain of
// its own, so a migration of leg B with no selector lands on dn-e. The black
// list keeps dn-c out but not dn-d: only the new leg's location keeps dn-d
// out. A plan that left that location out would see the scan offer dn-d and
// dn-e and the draw take dn-d half the time, so the test draws
// volTier1DrawCnt times, each on a fresh fixture.
func TestCreateMigrationKeepsOffARepairsNewLegsDomain(t *testing.T) {
	for draw := 0; draw < volTier1DrawCnt; draw++ {
		env := newVolEnv(t)
		env.relocateDn(volDnD, volDnC)
		env.putDn(volDnE, 704, 4, nil)
		repCreate(env, "rep-a", volDataLegA, volDnC)
		_, err := env.srv.CreateMigration(env.ctx,
			&pb.CreateMigrationRequest{
				ClusterName: env.cluster,
				SpName:      volSpName,
				SpRev:       env.token(),
				MigrName:    "migr-b",
				SrcSideId:   volDataSideB,
				DmCloneConf: &pb.DmCloneConf{HydrationThreshold: 2},
			})
		if err != nil {
			t.Fatalf("draw %d: CreateMigration: %v", draw, err)
		}
		leg := grpLegOf(volGrpOf(t, env.slice(), volDataGrpId),
			volDataLegB)
		if leg == nil || len(leg.GetSideList()) != 2 {
			t.Fatalf("draw %d: leg %d must own two sides: %v", draw,
				volDataLegB, leg)
		}
		if got := leg.GetSideList()[1].GetAddrPort(); got != volDnE {
			t.Fatalf("draw %d: leg %d destination on %s, want %s (dn-d "+
				"shares dn-c's domain, where the repair's new leg is)",
				draw, volDataLegB, got, volDnE)
		}
	}
}

// TestUpdateCntlidSlotListSkipsRepairAndRetiredSides pins work the slot list
// update does not do (architecture.md, Storage pools): it reads no repair's
// new side and no retired side, so a slot only such sides hold leaves the
// list. The cntlrs hold slots 3 and 4, every slice side 3, and a repair's
// new side and a retired side 5.
func TestUpdateCntlidSlotListSkipsRepairAndRetiredSides(t *testing.T) {
	env := newVolEnv(t)
	env.patchCntlr(volCntlrA, func(cntlr *pb.Cntlr) { cntlr.CntlidSlot = 3 })
	env.patchCntlr(volCntlrB, func(cntlr *pb.Cntlr) { cntlr.CntlidSlot = 4 })
	newSide := volSide(802, volDnC)
	newSide.CntlidSlot = 5
	repPutRepair(env, "rep-a", volDataGrpId, volDataLegA,
		pb.RepStage_REP_STAGE_CREATED, 801, newSide)
	retiredSide := volSide(901, volDnD)
	retiredSide.CntlidSlot = 5
	repPutRetired(env, volMetaGrpId, 900, retiredSide, 11)
	if _, err := env.srv.UpdateStoragePoolCntlidSlotList(env.ctx,
		&pb.UpdateStoragePoolCntlidSlotListRequest{
			ClusterName:    env.cluster,
			SpName:         volSpName,
			SpRev:          env.token(),
			CntlidSlotList: []uint32{3, 4},
		}); err != nil {
		t.Fatalf("UpdateStoragePoolCntlidSlotList [3 4]: %v", err)
	}
	if got := env.spConf().GetCntlidSlotList(); len(got) != 2 ||
		got[0] != 3 || got[1] != 4 {
		t.Errorf("cntlid_slot_list: got %v, want [3 4]", got)
	}
}

// TestRepairCallsRefuseAnInvalidStoredConf pins GW11 on the repair calls
// that move a capacity key: CancelRepair and DeleteRetiredLeg give a side
// back, and CreateRepair with a fresh new leg scans for one, each by the
// stored ladder and batch sizes, so a stored ClusterConf with a zero extent
// size is ABORTED with the validator's sentence and nothing is written. With
// the conf repaired, the same three requests succeed.
func TestRepairCallsRefuseAnInvalidStoredConf(t *testing.T) {
	env := newVolEnv(t)
	repCreate(env, "rep-a", volDataLegA, volDnC)
	if _, err := repAbort(env, "rep-a"); err != nil {
		t.Fatalf("TryToAbortRepair: %v", err)
	}
	repMoveStage(env, "rep-a", pb.RepStage_REP_STAGE_ABORTED)
	repPutRetired(env, volMetaGrpId, 900, volSide(901, volDnD), 11)
	repSetLegEpoch(env, volMetaGrpId, volMetaLegA, repEpoch)
	calls := []struct {
		name string
		call func() error
	}{
		{"CancelRepair", func() error {
			_, err := env.srv.CancelRepair(env.ctx, &pb.CancelRepairRequest{
				ClusterName: env.cluster, SpName: volSpName, RepName: "rep-a",
			})
			return err
		}},
		{"DeleteRetiredLeg", func() error {
			_, err := env.srv.DeleteRetiredLeg(env.ctx,
				&pb.DeleteRetiredLegRequest{
					ClusterName: env.cluster, SpName: volSpName, LegId: 900,
				})
			return err
		}},
		{"CreateRepair", func() error {
			_, err := env.srv.CreateRepair(env.ctx, &pb.CreateRepairRequest{
				ClusterName:  env.cluster,
				SpName:       volSpName,
				RepName:      "rep-b",
				GrpId:        volMetaGrpId,
				BadLegIdList: []uint64{volMetaLegA},
				DnSelector:   volDnSelector(volDnC),
			})
			return err
		}},
	}
	good := proto.Clone(env.cc).(*pb.ClusterConf)
	bad := proto.Clone(env.cc).(*pb.ClusterConf)
	bad.GetDnBinConf().ExtentSize = 0
	mustPut(t, env.cli, model.ClusterConfKey(env.cluster), bad)
	before := env.dump()
	for _, tc := range calls {
		msg := volWantCode(t, tc.call(), codes.Aborted)
		if msg != scMsgExtentSize {
			t.Errorf("%s: message %q, want %q", tc.name, msg, scMsgExtentSize)
		}
	}
	env.wantNoWrite("the refused calls", before)
	mustPut(t, env.cli, model.ClusterConfKey(env.cluster), good)
	for _, tc := range calls {
		if err := tc.call(); err != nil {
			t.Errorf("%s after the stored conf was repaired: %v", tc.name, err)
		}
	}
}

// TestFinishRepairRefusesAnInvalidStoredConf pins GW11's gate on
// FinishRepair, which gives a retired leg's side back for the sp worker's
// repair at the bound: a stored ClusterConf with a zero extent size is
// ABORTED with the validator's sentence from the planning read, whether the
// finish would give a side back or not, and nothing is written; with the
// conf repaired the same request finishes the repair.
func TestFinishRepairRefusesAnInvalidStoredConf(t *testing.T) {
	env := newVolEnv(t)
	repCreate(env, "rep-a", volDataLegA, volDnC)
	repMoveStage(env, "rep-a", pb.RepStage_REP_STAGE_STARTED)
	good := proto.Clone(env.cc).(*pb.ClusterConf)
	bad := proto.Clone(env.cc).(*pb.ClusterConf)
	bad.GetDnBinConf().ExtentSize = 0
	mustPut(t, env.cli, model.ClusterConfKey(env.cluster), bad)
	before := env.dump()
	_, err := repFinish(env, "rep-a")
	if msg := volWantCode(t, err, codes.Aborted); msg != scMsgExtentSize {
		t.Errorf("message %q, want %q", msg, scMsgExtentSize)
	}
	env.wantNoWrite("the refused FinishRepair", before)
	mustPut(t, env.cli, model.ClusterConfKey(env.cluster), good)
	if _, err := repFinish(env, "rep-a"); err != nil {
		t.Errorf("FinishRepair after the stored conf was repaired: %v", err)
	}
}

// TestCreateRepairReplansAroundADnTheGroupGained pins the per-round plan of
// a fresh new leg (architecture.md, Repairs, Per-operation allocation; GW9):
// a round plans its black list and its tier-1 locations from the group as
// its snapshot read it, so a migration of leg B onto dn-c committed after
// that read is missing from the plan and the scan offers dn-c.
// model.CreateRepair refuses the gain as candidate changed, and the next
// round plans from the group as it now stands, so the new leg lands on dn-d,
// through tier 2. A plan read once for the whole unit would be refused the
// same way in every round until the round cap.
//
// Every draw is forced, as in TestCreateMigrationRefusesADnTheGroupAlreadyUses:
// dn-d shares dn-c's failure domain and holds less, so the first scan offers
// dn-c alone, and the second — dn-c black-listed and its domain excluded —
// offers dn-d alone, through tier 2.
func TestCreateRepairReplansAroundADnTheGroupGained(t *testing.T) {
	env := newVolEnv(t)
	env.relocateDn(volDnD, volDnC)
	env.setDnFree(volDnC, volDnCFree)
	repSetLegEpoch(env, volDataGrpId, volDataLegA, repEpoch)
	var actorErr error
	prev := slog.Default()
	hook := &volReadHook{
		Handler: prev.Handler(),
		key:     model.SliceKey(env.cid, volSpId, volSliceId),
		actor:   func() { actorErr = volMigrateLegB(env) },
	}
	slog.SetDefault(slog.New(hook))
	defer slog.SetDefault(prev)
	ctx, cancel := context.WithTimeout(
		context.WithValue(env.ctx, volReadHookKey{}, true), 30*time.Second)
	defer cancel()
	req := repRequest(env)
	req.SpRev = nil
	req.DnSelector = nil
	_, err := env.srv.CreateRepair(ctx, req)
	if !hook.fired.Load() {
		t.Fatalf("the plan never read the slice: nothing was raced")
	}
	if actorErr != nil {
		t.Fatalf("the interleaved migration: %v", actorErr)
	}
	if err != nil {
		t.Fatalf("CreateRepair: %v", err)
	}
	side := env.repair("rep-a").GetNewLegList()[0].GetSideList()[0]
	if side.GetAddrPort() != volDnD {
		t.Errorf("the new leg on %s, want %s", side.GetAddrPort(), volDnD)
	}
	if rev := env.spRev(); rev != 3 {
		t.Errorf("sp_rev: got %d, want 3 (one bump per request)", rev)
	}
}

// TestCreateRepairReplansAroundADnDisabledOrDeleted pins model.CreateRepair's
// reading of a pick gone stale (architecture.md, Per-operation allocation;
// GW9): the DN the scan offered is disabled, or deleted, while the op's
// transaction runs, and the op answers candidate changed rather than
// refusing, so the next round scans again and the new leg lands on the other
// DN. dn_batch_size is 1 and dn-c holds the most free extents, so the first
// scan offers dn-c alone; the other writer runs inside the op's first read of
// dn-c's record, whose transaction then fails its commit and reads the
// record again as the other writer left it.
func TestCreateRepairReplansAroundADnDisabledOrDeleted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		actor func(env *volEnv) error
	}{
		{
			name: "disabled",
			actor: func(env *volEnv) error {
				_, err := env.srv.UpdateDiskNodeDisabled(env.ctx,
					&pb.UpdateDiskNodeDisabledRequest{
						ClusterName: env.cluster,
						AddrPort:    volDnC,
						Disabled:    true,
					})
				return err
			},
		},
		{
			name: "deleted",
			actor: func(env *volEnv) error {
				env.dropDn(volDnC)
				return nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newVolEnv(t)
			env.setDnBatchSize(1)
			env.setDnFree(volDnC, volDnCFree)
			repSetLegEpoch(env, volDataGrpId, volDataLegA, repEpoch)
			var actorErr error
			prev := slog.Default()
			hook := &volReadHook{
				Handler: prev.Handler(),
				key:     model.DnConfKey(env.cid, volDnC),
				actor:   func() { actorErr = tc.actor(env) },
			}
			slog.SetDefault(slog.New(hook))
			defer slog.SetDefault(prev)
			// Bounded twice over, so that a unit which never settles fails
			// as ABORTED instead of hanging the package: GW9 retries until
			// the round cap or the ctx's end, whichever comes first.
			ctx, cancel := context.WithTimeout(
				context.WithValue(env.ctx, volReadHookKey{}, true),
				30*time.Second)
			defer cancel()
			req := repRequest(env)
			req.SpRev = nil
			req.DnSelector = nil
			_, err := env.srv.CreateRepair(ctx, req)
			if !hook.fired.Load() {
				t.Fatalf("the request never read dn-c's record: the other " +
					"writer never ran and nothing was raced")
			}
			if actorErr != nil {
				t.Fatalf("the interleaved writer: %v", actorErr)
			}
			if err != nil {
				t.Fatalf("CreateRepair: %v", err)
			}
			side := env.repair("rep-a").GetNewLegList()[0].GetSideList()[0]
			if side.GetAddrPort() != volDnD {
				t.Errorf("the new leg on %s, want %s", side.GetAddrPort(),
					volDnD)
			}
			if free := env.dn(volDnD).GetFreeExtCnt(); free !=
				volDnFree-volDataExtCnt {
				t.Errorf("dn-d free_ext_cnt: got %d, want %d", free,
					volDnFree-volDataExtCnt)
			}
			if rev := env.spRev(); rev != 2 {
				t.Errorf("sp_rev: got %d, want 2 (the create's one bump)",
					rev)
			}
		})
	}
}

// TestRepairCallsResolveThePool pins the opening every repair call shares
// (GW4, GW5): a pool name or a rep_name that breaks the common name rule is
// INVALID_ARGUMENT before any read, and a pool the cluster does not hold is
// NOT_FOUND, for each of the eight calls, with nothing written.
func TestRepairCallsResolveThePool(t *testing.T) {
	env := newVolEnv(t)
	calls := map[string]func(spName, repName string) error{
		"CreateRepair": func(spName, repName string) error {
			_, err := env.srv.CreateRepair(env.ctx, &pb.CreateRepairRequest{
				ClusterName: env.cluster, SpName: spName, RepName: repName,
				GrpId: volDataGrpId, BadLegIdList: []uint64{volDataLegA},
			})
			return err
		},
		"FinishRepair": func(spName, repName string) error {
			_, err := env.srv.FinishRepair(env.ctx, &pb.FinishRepairRequest{
				ClusterName: env.cluster, SpName: spName, RepName: repName,
			})
			return err
		},
		"CancelRepair": func(spName, repName string) error {
			_, err := env.srv.CancelRepair(env.ctx, &pb.CancelRepairRequest{
				ClusterName: env.cluster, SpName: spName, RepName: repName,
			})
			return err
		},
		"TryToAbortRepair": func(spName, repName string) error {
			_, err := env.srv.TryToAbortRepair(env.ctx,
				&pb.TryToAbortRepairRequest{
					ClusterName: env.cluster, SpName: spName,
					RepName: repName,
				})
			return err
		},
		"GetRepair": func(spName, repName string) error {
			_, err := env.srv.GetRepair(env.ctx, &pb.GetRepairRequest{
				ClusterName: env.cluster, SpName: spName, RepName: repName,
			})
			return err
		},
		"ListRepairs": func(spName, _ string) error {
			_, err := env.srv.ListRepairs(env.ctx, &pb.ListRepairsRequest{
				ClusterName: env.cluster, SpName: spName,
			})
			return err
		},
		"ListRetiredLegs": func(spName, _ string) error {
			_, err := env.srv.ListRetiredLegs(env.ctx,
				&pb.ListRetiredLegsRequest{
					ClusterName: env.cluster, SpName: spName,
				})
			return err
		},
		"DeleteRetiredLeg": func(spName, _ string) error {
			_, err := env.srv.DeleteRetiredLeg(env.ctx,
				&pb.DeleteRetiredLegRequest{
					ClusterName: env.cluster, SpName: spName, LegId: 900,
				})
			return err
		},
	}
	byName := map[string]bool{
		"CreateRepair": true, "FinishRepair": true, "CancelRepair": true,
		"TryToAbortRepair": true, "GetRepair": true,
	}
	before := env.dump()
	for name, call := range calls {
		msg := volWantCode(t, call("", "rep-a"), codes.InvalidArgument)
		if msg != "sp_name must not be empty" {
			t.Errorf("%s with no sp_name: message %q", name, msg)
		}
		if byName[name] {
			msg := volWantCode(t, call(volSpName, "rep a"),
				codes.InvalidArgument)
			want := fmt.Sprintf("rep_name %q does not match %s", "rep a",
				common.ValidStrPattern)
			if msg != want {
				t.Errorf("%s with a bad rep_name: message %q, want %q",
					name, msg, want)
			}
		}
		msg = volWantCode(t, call("nosuch", "rep-a"), codes.NotFound)
		if want := fmt.Sprintf("storage pool %q not found", "nosuch"); msg != want {
			t.Errorf("%s of a missing pool: message %q, want %q", name, msg,
				want)
		}
	}
	env.wantNoWrite("the refused calls", before)
}
