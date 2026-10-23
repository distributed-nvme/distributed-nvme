package plant

import (
	"context"
	"fmt"
	"slices"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Repairs and retired legs (model/repair.go)
// ---------------------------------------------------------------------------

// plantedGrp is where findPlantedGrp found a group: its parent, the slice or
// the clone record that holds it as the caller's STM read it, and the group.
type plantedGrp struct {
	parent model.GrpParent
	key    string
	slice  *pb.Slice
	clone  *pb.Clone
	grp    *pb.Group
}

// findPlantedGrp finds the group with grpId in the pool, in its slices first
// and then in its clones' clone meta groups, as the gateway looks a group up
// for a repair. A listed slice or clone whose key is missing refuses.
func findPlantedGrp(
	s etcdutil.STM,
	cid uint64,
	target SpRef,
	conf *pb.SpConf,
	grpId uint64,
) (*plantedGrp, error) {
	for _, sliceId := range conf.GetSliceIdList() {
		key := model.SliceKey(cid, target.Id, sliceId)
		slice := &pb.Slice{}
		if !s.Get(key, slice) {
			return nil, fmt.Errorf("%q not found", key)
		}
		for _, grp := range append(
			append([]*pb.Group(nil), slice.GetMetaGrpList()...),
			slice.GetDataGrpList()...,
		) {
			if grp.GetGrpId() == grpId {
				return &plantedGrp{
					parent: model.GrpParent{SliceId: sliceId},
					key:    key, slice: slice, grp: grp,
				}, nil
			}
		}
	}
	for _, cloneName := range conf.GetCloneNameList() {
		key := model.CloneKey(cid, target.Id, cloneName)
		clone := &pb.Clone{}
		if !s.Get(key, clone) {
			return nil, fmt.Errorf("%q not found", key)
		}
		if grp := clone.GetMetaGrp(); grp != nil && grp.GetGrpId() == grpId {
			return &plantedGrp{
				parent: model.GrpParent{CloneName: cloneName},
				key:    key, clone: clone, grp: grp,
			}, nil
		}
	}
	return nil, fmt.Errorf("no group %#x in storage pool %q", grpId,
		target.Name)
}

// put stages the write of the group's parent.
func (g *plantedGrp) put(s etcdutil.STM) {
	if g.slice != nil {
		s.Put(g.key, g.slice)
		return
	}
	s.Put(g.key, g.clone)
}

// nextPlantedLegIdx is 1 + the largest leg_idx of the group's legs and
// spares: the leg_idx CreateRepair gives a fresh new leg.
func nextPlantedLegIdx(grp *pb.Group) uint32 {
	next := uint32(0)
	for _, leg := range append(
		append([]*pb.Leg(nil), grp.GetLegList()...),
		grp.GetSpareLegList()...,
	) {
		if leg.GetLegIdx()+1 > next {
			next = leg.GetLegIdx() + 1
		}
	}
	return next
}

// RepairSpec is one repair to plant, its new leg placed as the caller
// chooses.
type RepairSpec struct {
	Name string
	// Id is the rep_id; 0 takes the pool's next free one.
	Id uint64
	// GrpId is the repaired group, a slice's or a clone's, and BadLegId the
	// leg of its leg_list the repair replaces.
	GrpId    uint64
	BadLegId uint64
	// RetiredLegId is the retired leg of the group the new leg is taken
	// from; 0 plants a fresh new leg on Dn instead.
	RetiredLegId uint64
	// LegId and SideId are a fresh new leg's and its side's ids; 0 takes the
	// pool's next free one. Dn is the dn_id that hosts the side, Slot its
	// cntlid_slot, and Provisioned plants the side as the provisioned flip
	// leaves it.
	LegId       uint64
	SideId      uint64
	Dn          uint64
	Slot        uint32
	Provisioned bool
}

// Check refuses a repair no key could be written for, and a new leg named
// both ways or neither: a retired leg, or a fresh one on a DN.
func (spec RepairSpec) Check() error {
	switch {
	case spec.Name == "":
		return fmt.Errorf("the repair name is empty")
	case spec.GrpId == 0:
		return fmt.Errorf("repair %q: group 0 is never valid", spec.Name)
	case spec.BadLegId == 0:
		return fmt.Errorf("repair %q: bad leg 0 is never valid", spec.Name)
	case spec.RetiredLegId != 0 && (spec.Dn != 0 || spec.LegId != 0 ||
		spec.SideId != 0):
		return fmt.Errorf("repair %q: a new leg taken from a retired leg "+
			"is placed already", spec.Name)
	case spec.RetiredLegId == 0 && spec.Dn == 0:
		return fmt.Errorf("repair %q: a fresh new leg needs a dn",
			spec.Name)
	}
	return nil
}

// Repair is one planted repair: its ids, the parent of its group, its new
// leg's ids and DN, the charge the DN took for a fresh new leg, the DN's new
// revision then, and the pool revision.
type Repair struct {
	Name       string
	Id         uint64
	GrpId      uint64
	SliceId    uint64
	CloneName  string
	LegId      uint64
	SideId     uint64
	Addr       string
	ExtCnt     uint64
	DnRevision uint64
	SpRev      uint64
}

// opPutRepair names PutRepair in a refusal of model.TakeSpIds.
const opPutRepair = "PutRepair"

// PutRepair writes one Repair, CREATED with its flag clear, the group's
// rep_name and the SpConf.repair_name_list entry, then bumps SpRev once: the
// records model.CreateRepair writes, with the new leg placed as the spec
// says. A fresh new leg takes the next free leg_idx of the group and charges
// its DN the group's ext_cnt through chargeDn; its ids, the rep_id's first,
// are taken as CreateRepair mints them, and a leg or side id the pool already
// uses is refused, as PutClone refuses one. A new leg taken from a retired
// leg keeps its record's ids, leg_idx and side, which stay charged, and its
// record and list entries go, its retired_epoch kept in the repair.
//
// It refuses a name the pool already holds, a group that has a repair, a
// bad leg not in the group's leg_list and a retired leg of another group,
// which no create writes. It does not apply CreateRepair's other refusals:
// the library plants whatever shape a case needs.
func PutRepair(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	spec RepairSpec,
) (Repair, error) {
	if err := spec.Check(); err != nil {
		return Repair{}, err
	}
	cid := cl.Id
	var addr string
	if spec.Dn != 0 {
		var err error
		if addr, err = DnAddr(ctx, cli, cl, spec.Dn); err != nil {
			return Repair{}, err
		}
	}
	var out Repair
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		out = Repair{Name: spec.Name, GrpId: spec.GrpId, Addr: addr}
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		repKey := model.RepairKey(cid, target.Id, spec.Name)
		if containsName(conf.GetRepairNameList(), spec.Name) ||
			s.Get(repKey, &pb.Repair{}) {
			return fmt.Errorf("repair %q already exists", spec.Name)
		}
		g, err := findPlantedGrp(s, cid, target, conf, spec.GrpId)
		if err != nil {
			return err
		}
		if name := g.grp.GetRepName(); name != "" {
			return fmt.Errorf("group %#x has the repair %q", spec.GrpId, name)
		}
		if !slices.ContainsFunc(g.grp.GetLegList(), func(leg *pb.Leg) bool {
			return leg.GetLegId() == spec.BadLegId
		}) {
			return fmt.Errorf("group %#x has no leg %#x in its leg_list",
				spec.GrpId, spec.BadLegId)
		}
		out.SliceId, out.CloneName = g.parent.SliceId, g.parent.CloneName
		out.ExtCnt = g.grp.GetExtCnt()
		if out.Id, err = takeId(conf, opPutRepair, spec.Id); err != nil {
			return err
		}
		rep := &pb.Repair{
			RepId:        out.Id,
			GrpId:        spec.GrpId,
			RepStage:     pb.RepStage_REP_STAGE_CREATED,
			BadLegIdList: []uint64{spec.BadLegId},
			SliceId:      out.SliceId,
			CloneName:    out.CloneName,
		}
		if spec.RetiredLegId != 0 {
			leg, epoch, err := takePlantedRetired(
				s, cid, target, conf, g.grp, spec.RetiredLegId)
			if err != nil {
				return err
			}
			rep.NewLegList = []*pb.Leg{leg}
			rep.NewLegIdToRetiredEpoch = map[uint64]uint64{
				leg.GetLegId(): epoch,
			}
			out.LegId = leg.GetLegId()
			out.SideId = leg.GetSideList()[0].GetSideId()
			out.Addr = leg.GetSideList()[0].GetAddrPort()
		} else {
			leg, revision, err := placePlantedLeg(
				s, cl, target, conf, g.grp, spec, addr)
			if err != nil {
				return err
			}
			rep.NewLegList = []*pb.Leg{leg}
			out.LegId = leg.GetLegId()
			out.SideId = leg.GetSideList()[0].GetSideId()
			out.DnRevision = revision
		}
		g.grp.RepName = spec.Name
		g.put(s)
		s.Put(repKey, rep)
		conf.RepairNameList = append(conf.RepairNameList, spec.Name)
		s.Put(model.SpConfKey(cid, target.Name), conf)
		out.SpRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		return Repair{}, err
	}
	return out, nil
}

// takePlantedRetired is PutRepair's new leg taken from the retired leg legId
// of grp: its record is deleted and its entries leave the pool's and the
// group's lists, and it comes back as a Leg with its retired_epoch.
func takePlantedRetired(
	s etcdutil.STM,
	cid uint64,
	target SpRef,
	conf *pb.SpConf,
	grp *pb.Group,
	legId uint64,
) (*pb.Leg, uint64, error) {
	key := model.RetiredLegKey(cid, target.Id, legId)
	retired := &pb.RetiredLeg{}
	if !slices.Contains(conf.GetRetiredLegIdList(), legId) ||
		!s.Get(key, retired) {
		return nil, 0, fmt.Errorf("no retired leg %#x in storage pool %q",
			legId, target.Name)
	}
	if retired.GetGrpId() != grp.GetGrpId() ||
		!slices.Contains(grp.GetRetiredLegIdList(), legId) {
		return nil, 0, fmt.Errorf("retired leg %#x is not group %#x's",
			legId, grp.GetGrpId())
	}
	s.Del(key)
	conf.RetiredLegIdList = slices.DeleteFunc(
		conf.RetiredLegIdList, func(id uint64) bool { return id == legId })
	grp.RetiredLegIdList = slices.DeleteFunc(
		grp.RetiredLegIdList, func(id uint64) bool { return id == legId })
	return &pb.Leg{
		LegId:    legId,
		LegIdx:   retired.GetLegIdx(),
		SideList: []*pb.Side{retired.GetSide()},
	}, retired.GetRetiredEpoch(), nil
}

// placePlantedLeg is PutRepair's fresh new leg: its ids taken and claimed,
// its slot checked against the pool's list, and its DN charged the group's
// ext_cnt. It returns the leg and the DN's new revision.
func placePlantedLeg(
	s etcdutil.STM,
	cl Cluster,
	target SpRef,
	conf *pb.SpConf,
	grp *pb.Group,
	spec RepairSpec,
	addr string,
) (*pb.Leg, uint64, error) {
	cid := cl.Id
	legId, err := takeId(conf, opPutRepair, spec.LegId)
	if err != nil {
		return nil, 0, err
	}
	sideId, err := takeId(conf, opPutRepair, spec.SideId)
	if err != nil {
		return nil, 0, err
	}
	used, err := readPoolIds(s, cid, conf)
	if err != nil {
		return nil, 0, err
	}
	for _, claim := range []struct {
		kind string
		id   uint64
	}{{"leg", legId}, {"side", sideId}} {
		if err := used.claim(claim.kind, claim.id); err != nil {
			return nil, 0, fmt.Errorf("repair %q: %w in storage pool %q",
				spec.Name, err, target.Name)
		}
	}
	if !slices.Contains(conf.GetCntlidSlotList(), spec.Slot) {
		return nil, 0, fmt.Errorf("repair %q: cntlid slot %d is not in the "+
			"slot list of storage pool %q", spec.Name, spec.Slot, target.Name)
	}
	dn, err := plantedDn(s, cid, addr, spec.Dn)
	if err != nil {
		return nil, 0, err
	}
	revision, err := chargeDn(s, cid, cl.Conf, addr, dn,
		[]*pb.SidePointer{{SpId: target.Id, LegId: legId, SideId: sideId}},
		grp.GetExtCnt(), 0)
	if err != nil {
		return nil, 0, err
	}
	return &pb.Leg{
		LegId:  legId,
		LegIdx: nextPlantedLegIdx(grp),
		SideList: []*pb.Side{{
			SideId:      sideId,
			AddrPort:    addr,
			CntlidSlot:  spec.Slot,
			NvmeTrConf:  dn.GetNvmeTrConf(),
			ErrEpoch:    0,
			Provisioned: spec.Provisioned,
		}},
	}, revision, nil
}

// plantedDn reads the DN record at addr inside the caller's STM and checks
// that it is dnId's.
func plantedDn(
	s etcdutil.STM,
	cid uint64,
	addr string,
	dnId uint64,
) (*pb.DnConf, error) {
	key := model.DnConfKey(cid, addr)
	dn := &pb.DnConf{}
	if !s.Get(key, dn) {
		return nil, fmt.Errorf("%q not found", key)
	}
	if dn.GetDnId() != dnId {
		return nil, fmt.Errorf("%q holds dn %#x, want %#x", addr,
			dn.GetDnId(), dnId)
	}
	return dn, nil
}

// RetiredLegSpec is one retired leg to plant, its side placed as the caller
// chooses.
type RetiredLegSpec struct {
	// LegId is the retired leg's id, the id of the leg it was; 0 takes the
	// pool's next free one. SideId is its side's; 0 likewise.
	LegId  uint64
	SideId uint64
	// GrpId is the group it left, a slice's or a clone's, and LegIdx its
	// leg_idx there.
	GrpId  uint64
	LegIdx uint32
	// Dn is the dn_id that hosts the side, Slot its cntlid_slot.
	Dn   uint64
	Slot uint32
	// Epoch is the retired_epoch, in unix seconds.
	Epoch uint64
}

// Check refuses a retired leg no key could be written for.
func (spec RetiredLegSpec) Check() error {
	if spec.GrpId == 0 {
		return fmt.Errorf("retired leg %#x: group 0 is never valid",
			spec.LegId)
	}
	if spec.Dn == 0 {
		return fmt.Errorf("retired leg %#x: dn 0 is never valid", spec.LegId)
	}
	return nil
}

// RetiredLeg is one planted retired leg: its ids, the parent of its group,
// its DN and the charge it took, the DN's new revision and the pool
// revision.
type RetiredLeg struct {
	LegId      uint64
	SideId     uint64
	GrpId      uint64
	SliceId    uint64
	CloneName  string
	Addr       string
	ExtCnt     uint64
	DnRevision uint64
	SpRev      uint64
}

// opPutRetiredLeg names PutRetiredLeg in a refusal of model.TakeSpIds.
const opPutRetiredLeg = "PutRetiredLeg"

// PutRetiredLeg writes one RetiredLeg with its entries in the group's and
// the pool's lists, as model.FinishRepair leaves the leg it retires, and
// bumps SpRev once. Its side is provisioned, as a leg's side is, on the DN
// the spec names, which is charged the group's ext_cnt through chargeDn; a
// leg or side id the pool already uses is refused, as PutClone refuses one.
// It does not refuse a group at common.MaxRetiredLegPerGrp: the library
// plants whatever shape a case needs.
func PutRetiredLeg(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	spec RetiredLegSpec,
) (RetiredLeg, error) {
	if err := spec.Check(); err != nil {
		return RetiredLeg{}, err
	}
	cid := cl.Id
	addr, err := DnAddr(ctx, cli, cl, spec.Dn)
	if err != nil {
		return RetiredLeg{}, err
	}
	var out RetiredLeg
	err = cli.RunSTM(ctx, func(s etcdutil.STM) error {
		out = RetiredLeg{GrpId: spec.GrpId, Addr: addr}
		conf, err := getSpConf(s, cid, target)
		if err != nil {
			return err
		}
		g, err := findPlantedGrp(s, cid, target, conf, spec.GrpId)
		if err != nil {
			return err
		}
		out.SliceId, out.CloneName = g.parent.SliceId, g.parent.CloneName
		out.ExtCnt = g.grp.GetExtCnt()
		if out.LegId, err = takeId(conf, opPutRetiredLeg, spec.LegId); err != nil {
			return err
		}
		if out.SideId, err = takeId(conf, opPutRetiredLeg, spec.SideId); err != nil {
			return err
		}
		used, err := readPoolIds(s, cid, conf)
		if err != nil {
			return err
		}
		for _, claim := range []struct {
			kind string
			id   uint64
		}{{"leg", out.LegId}, {"side", out.SideId}} {
			if err := used.claim(claim.kind, claim.id); err != nil {
				return fmt.Errorf("retired leg: %w in storage pool %q", err,
					target.Name)
			}
		}
		if !slices.Contains(conf.GetCntlidSlotList(), spec.Slot) {
			return fmt.Errorf("retired leg %#x: cntlid slot %d is not in the "+
				"slot list of storage pool %q", out.LegId, spec.Slot,
				target.Name)
		}
		dn, err := plantedDn(s, cid, addr, spec.Dn)
		if err != nil {
			return err
		}
		out.DnRevision, err = chargeDn(s, cid, cl.Conf, addr, dn,
			[]*pb.SidePointer{{
				SpId: target.Id, LegId: out.LegId, SideId: out.SideId,
			}}, out.ExtCnt, 0)
		if err != nil {
			return err
		}
		s.Put(model.RetiredLegKey(cid, target.Id, out.LegId), &pb.RetiredLeg{
			GrpId:  spec.GrpId,
			LegIdx: spec.LegIdx,
			Side: &pb.Side{
				SideId:      out.SideId,
				AddrPort:    addr,
				CntlidSlot:  spec.Slot,
				NvmeTrConf:  dn.GetNvmeTrConf(),
				Provisioned: true,
			},
			RetiredEpoch: spec.Epoch,
			SliceId:      out.SliceId,
			CloneName:    out.CloneName,
		})
		g.grp.RetiredLegIdList = append(g.grp.RetiredLegIdList, out.LegId)
		g.put(s)
		conf.RetiredLegIdList = append(conf.RetiredLegIdList, out.LegId)
		s.Put(model.SpConfKey(cid, target.Name), conf)
		out.SpRev, err = bumpSpRev(s, cid, conf)
		return err
	})
	if err != nil {
		return RetiredLeg{}, err
	}
	return out, nil
}

// storedRepair reads one repair of the pool, which the setters below take
// its rep_id from, for SetCreated's reason: the ops take the repair's name
// and its id so that a repair finished and created again under the name
// between the read and the transaction is refused or skipped.
func storedRepair(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	repName string,
) (*pb.Repair, error) {
	if repName == "" {
		return nil, fmt.Errorf("the repair name is empty")
	}
	key := model.RepairKey(cl.Id, target.Id, repName)
	rep := &pb.Repair{}
	found, err := cli.Get(ctx, key, rep)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("key %q not found", key)
	}
	return rep, nil
}

// SetRepairAbort sets a CREATED repair's abort flag through
// model.SetRepairAbort, the operation the gateway's TryToAbortRepair runs,
// which the worker suite drives with no gateway. It returns the repair's
// rep_id and the pool's revision after it.
func SetRepairAbort(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	repName string,
) (uint64, uint64, error) {
	repId, err := model.SetRepairAbort(ctx, cli, cl.Id, target.Shard,
		target.Id, target.Name, 0, repName)
	if err != nil {
		return 0, 0, err
	}
	spRev, err := SpRevision(ctx, cli, cl, target)
	if err != nil {
		return 0, 0, err
	}
	return repId, spRev, nil
}

// SetRepairStage moves a CREATED repair to stage through
// model.SetRepairStage: the sp worker's move from the primary's verdict,
// which a suite that runs no worker, or no agent that reports one, plays.
// The rep_id comes from the stored record (storedRepair). It returns the
// rep_id and the pool's revision after it.
func SetRepairStage(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	repName string,
	stage pb.RepStage,
) (uint64, uint64, error) {
	rep, err := storedRepair(ctx, cli, cl, target, repName)
	if err != nil {
		return 0, 0, err
	}
	if err := model.SetRepairStage(ctx, cli, cl.Id, target.Shard, target.Id,
		target.Name, repName, rep.GetRepId(), stage); err != nil {
		return 0, 0, err
	}
	spRev, err := SpRevision(ctx, cli, cl, target)
	if err != nil {
		return 0, 0, err
	}
	return rep.GetRepId(), spRev, nil
}

// SetRepairProvisioned flips the side sideId of a repair's new leg legId
// through model.FlipRepairProvisioned, as SetProvisioned flips a slice's
// side, the rep_id from the stored record. It returns whether the flip
// applied and the pool's revision after it.
func SetRepairProvisioned(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	repName string,
	legId uint64,
	sideId uint64,
) (bool, uint64, error) {
	rep, err := storedRepair(ctx, cli, cl, target, repName)
	if err != nil {
		return false, 0, err
	}
	flipped, _, err := model.FlipRepairProvisioned(ctx, cli, cl.Id,
		target.Shard, target.Id, target.Name, model.RepairSideRef{
			RepName: repName, RepId: rep.GetRepId(),
			LegId: legId, SideId: sideId,
		})
	if err != nil {
		return false, 0, err
	}
	spRev, err := SpRevision(ctx, cli, cl, target)
	if err != nil {
		return false, 0, err
	}
	return flipped, spRev, nil
}

// SetRepairLegEpoch plants the err_epoch of a repair's new leg, and
// SetRepairSideEpoch of a side of one, where model.SetRepairLegErrEpoch and
// model.SetRepairSideErrEpoch look them up; each returns the value it
// replaced.
func SetRepairLegEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	repName string,
	legId uint64,
	epoch uint64,
) (uint64, error) {
	return setRepairObjEpoch(ctx, cli, cl, target, "leg", repName, legId,
		epoch)
}

// SetRepairSideEpoch is SetRepairLegEpoch for a side of a new leg.
func SetRepairSideEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	repName string,
	sideId uint64,
	epoch uint64,
) (uint64, error) {
	return setRepairObjEpoch(ctx, cli, cl, target, "side", repName, sideId,
		epoch)
}

// setRepairObjEpoch is SetRepairLegEpoch and SetRepairSideEpoch: kind is
// "leg" or "side", looked up in the repair's new legs as setGroupEpoch looks
// one up in a group.
func setRepairObjEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	kind string,
	repName string,
	id uint64,
	epoch uint64,
) (uint64, error) {
	if repName == "" {
		return 0, fmt.Errorf("the repair name is empty")
	}
	cid := cl.Id
	var old uint64
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		old = 0
		if _, err := getSpConf(s, cid, target); err != nil {
			return err
		}
		key := model.RepairKey(cid, target.Id, repName)
		rep := &pb.Repair{}
		if !s.Get(key, rep) {
			return fmt.Errorf("%q not found", key)
		}
		var found bool
		old, found = setGroupEpoch(
			&pb.Group{LegList: rep.GetNewLegList()}, kind, id, epoch)
		if !found {
			return fmt.Errorf("repair %q holds no %s %#x", repName, kind, id)
		}
		s.Put(key, rep)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return old, nil
}

// SetRetiredSideEpoch plants the err_epoch of a retired leg's side, where
// model.SetRetiredSideErrEpoch looks it up, and returns the value it
// replaced.
func SetRetiredSideEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cl Cluster,
	target SpRef,
	legId uint64,
	epoch uint64,
) (uint64, error) {
	cid := cl.Id
	var old uint64
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		old = 0
		if _, err := getSpConf(s, cid, target); err != nil {
			return err
		}
		key := model.RetiredLegKey(cid, target.Id, legId)
		retired := &pb.RetiredLeg{}
		if !s.Get(key, retired) {
			return fmt.Errorf("%q not found", key)
		}
		if retired.GetSide() == nil {
			return fmt.Errorf("retired leg %#x has no side", legId)
		}
		old = retired.GetSide().GetErrEpoch()
		retired.Side.ErrEpoch = epoch
		s.Put(key, retired)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return old, nil
}
