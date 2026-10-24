package model

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Repairs and retired legs: the model operations of a repair of an md-raid1
// group's leg, an operator's or the sp worker's own (architecture.md,
// Repairs; Retired legs; Repair). The gateway's calls run them as their
// deciding STMs. The sp worker runs CreateRepair, FinishRepair, CancelRepair
// and SetRepairAbort on its own repairs only (dnv-worker.md AR8), and the
// stage move, the retire timer's delete, the epoch writes and the
// provisioned flip on every repair (dnv-worker.md MD6).
//
// A repair is a record of its own (RepairKey) that names one bad leg of the
// group's leg_list and carries one new leg, a full Leg with its side, until
// FinishRepair puts the new leg in the bad leg's place and keeps the bad leg
// as a retired leg (RetiredLegKey): its side stays on its disk node with the
// data as it was. A group has at most one repair, named by its rep_name, and
// lists its retired legs in retired_leg_id_list; the pool lists both in its
// SpConf. Every operation changes a record, its group's link and the pool's
// list in one transaction, so LoadSp never meets one without the other.
//
// The group is found through its parent, a slice or a clone (GrpParent):
// CreateRepair takes the parent from its caller's own look-up, and the
// repair and every retired leg record it, so every later operation reads
// that one record and checks that it holds the group. One operation serves
// both kinds of group: what differs is the key the parent is written under.
//
// A repair whose name carries common.RepNameReservedPrefix is the sp
// worker's own; any other name is an operator's. Three rules tell them
// apart: the level gate of the create (CreateRepairLevelGate), the create's
// new leg, always fresh and allowed at the retired-leg bound for the
// worker's, and the finish of the worker's, which gives back the group's
// oldest retired leg at the bound.
//
// Each operation names the check of the pool it makes. CreateRepair takes a
// pool that is not being deleted below its level gate. The others take a
// pool at any level, so an operator can finish, abort, cancel and delete
// while the arrays still run, and refuse a pool being deleted, whose drain
// removes its repairs and retired legs (loadSpConfAnyLevel); the epoch writes
// refuse only a pool that is gone or another one (loadSpConfForHealth).

// The Op names of the repair operations, the exported function names as
// everywhere else in this package.
const (
	opCreateRepair           = "CreateRepair"
	opFinishRepair           = "FinishRepair"
	opCancelRepair           = "CancelRepair"
	opSetRepairAbort         = "SetRepairAbort"
	opSetRepairStage         = "SetRepairStage"
	opDeleteRetiredLeg       = "DeleteRetiredLeg"
	opSetRepairLegErrEpoch   = "SetRepairLegErrEpoch"
	opSetRepairSideErrEpoch  = "SetRepairSideErrEpoch"
	opSetRetiredSideErrEpoch = "SetRetiredSideErrEpoch"
	opFlipRepairProvisioned  = "FlipRepairProvisioned"
)

// ReasonRepairExists is the Reason CreateRepair carries when the pool already
// lists a repair of that name, or holds its key. It is exported because the
// gateway answers it ALREADY_EXISTS (gateway.md GW7) and not with the
// FAILED_PRECONDITION every other ErrPrecondition maps to: a create reaches it
// when another create took the name after the gateway's own check.
const ReasonRepairExists = "repair exists"

// ReasonRepairNotFound is the Reason an operation on a named repair carries
// when the pool lists no repair of that name. It is exported because the
// gateway answers it NOT_FOUND, the request naming an object the pool does
// not have (gateway.md GW7); a listed name whose key is missing is a lost
// key, not this.
const ReasonRepairNotFound = "repair not found"

// ReasonRetiredLegNotFound is ReasonRepairNotFound for a retired leg named
// by its leg id: CreateRepair's retired leg and DeleteRetiredLeg's.
const ReasonRetiredLegNotFound = "retired leg not found"

// ReasonRetiredListFull is the Reason CreateRepair carries when it would take
// a fresh new leg, under an operator's name, for a group that already holds
// common.MaxRetiredLegPerGrp retired legs. It is exported because the gateway
// answers it with the count-ceiling code, RESOURCE_EXHAUSTED, and a message
// that says to delete a retired leg (gateway.md GW7): a create reaches it
// when a finish retired a leg of the group after the gateway's own check.
const ReasonRetiredListFull = "retired leg list full"

// The level refusals of CreateRepair (CreateRepairLevelGate): a worker's
// repair at a level where reactions stop, and an operator's at a level that
// builds no group array.
const (
	reasonRepairLevelReactions = "sp level suppresses reactions"
	reasonRepairLevelNoArray   = "sp level builds no group array"
)

// reasonReservedNameRetiredLeg is CreateRepair's refusal of a new leg taken
// from a retired leg under a name with the reserved prefix: the sp worker's
// repair always takes a fresh leg, since md writes onto the new leg, so taking
// a retired leg back writes over its kept copy, and only an operator's repair
// makes that choice (architecture.md, Retired legs).
const reasonReservedNameRetiredLeg = "a reserved name takes a fresh leg"

// CreateRepairLevelGate is the lowest sp_level at which CreateRepair refuses
// a repair named repName (architecture.md, Repairs). A name with
// common.RepNameReservedPrefix, the sp worker's repair, is refused from
// SP_LEVEL_NO_THINPOOL up: its create is a reaction, and reactions stay off
// the levels where an operator is in charge (dnv-worker.md AR3). Any other
// name, an operator's, is refused from SP_LEVEL_NO_REDUND up, where no group
// array is built for the new leg to join: an operator's own call is no
// reaction, which lets an operator take a group over from the worker
// (architecture.md, Repairs).
func CreateRepairLevelGate(repName string) pb.SpLevel {
	if common.HasRepNameReservedPrefix(repName) {
		return pb.SpLevel_SP_LEVEL_NO_THINPOOL
	}
	return pb.SpLevel_SP_LEVEL_NO_REDUND
}

// checkCreateRepairLevel refuses conf's level when it is at or above
// CreateRepairLevelGate for repName, with the reason of the name's kind.
func checkCreateRepairLevel(conf *pb.SpConf, repName string) error {
	if conf.GetSpLevel() < CreateRepairLevelGate(repName) {
		return nil
	}
	if common.HasRepNameReservedPrefix(repName) {
		return fail(opCreateRepair, reasonRepairLevelReactions)
	}
	return fail(opCreateRepair, reasonRepairLevelNoArray)
}

// ReasonGrpHasRepair is the Reason CreateRepair carries for a group that
// already has a repair under another name: a group has one repair at a time,
// and two would race for the same place in its array (architecture.md,
// Repairs). A create under the name of the group's own repair meets
// ReasonRepairExists instead, since CreateRepair checks the name first. It
// is exported for the reason ReasonGrowPending is: the sp worker's leg
// repair logs the SAME `reaction skipped reason=` string when its pass holds
// a group that has a repair, an operator's or its own, which keeps the worker
// from creating another for the group until its finish or its cancel
// (dnv-worker.md AR8).
const ReasonGrpHasRepair = "grp_has_repair"

// GrpParent names the record that holds a group: a slice by its id, or a
// clone by its name, exactly one of the two set. A group never moves to
// another parent, so the parent a repair or a retired leg records when it is
// created stays true for its life.
type GrpParent struct {
	SliceId   uint64
	CloneName string
}

// repairParent is the parent a repair records.
func repairParent(rep *pb.Repair) GrpParent {
	return GrpParent{SliceId: rep.GetSliceId(), CloneName: rep.GetCloneName()}
}

// retiredParent is the parent a retired leg records.
func retiredParent(retired *pb.RetiredLeg) GrpParent {
	return GrpParent{
		SliceId:   retired.GetSliceId(),
		CloneName: retired.GetCloneName(),
	}
}

// grpHolder is one parent record as an operation read it inside its STM: the
// slice or the clone, and the key it is put back under.
type grpHolder struct {
	key   string
	slice *pb.Slice
	clone *pb.Clone
}

// loadGrpHolder reads the record parent names inside the caller's STM. A
// parent that names both records or neither is refused, and so is a
// record that is gone: a parent never moves, so its absence is a group that
// is gone, and the caller has nothing to act on.
func loadGrpHolder(
	s etcdutil.STM,
	op string,
	cid uint64,
	spId uint64,
	parent GrpParent,
) (*grpHolder, error) {
	switch {
	case parent.SliceId != 0 && parent.CloneName == "":
		key := SliceKey(cid, spId, parent.SliceId)
		slice := &pb.Slice{}
		if !s.Get(key, slice) {
			return nil, fail(op, "slice not found")
		}
		return &grpHolder{key: key, slice: slice}, nil
	case parent.SliceId == 0 && parent.CloneName != "":
		key := CloneKey(cid, spId, parent.CloneName)
		clone := &pb.Clone{}
		if !s.Get(key, clone) {
			return nil, fail(op, "clone not found")
		}
		return &grpHolder{key: key, clone: clone}, nil
	default:
		return nil, fail(op, "no single parent")
	}
}

// group is the group with grpId in the record, or nil.
func (h *grpHolder) group(grpId uint64) *pb.Group {
	if h.slice != nil {
		return findGroup(h.slice, grpId)
	}
	return findCloneGroup(h.clone, grpId)
}

// deleting reports whether the record is a clone being deleted. Its group
// goes back to its disk nodes in the clone drain's final transaction, with
// its repair and its retired legs (dnv-worker.md CLD9), so every repair
// operation refuses it, as the gateway refuses every other mutation of a
// deleting clone (dnv-worker.md CLD1).
func (h *grpHolder) deleting() bool {
	return h.clone.GetDeleting()
}

// put stages the record's write.
func (h *grpHolder) put(s etcdutil.STM) {
	if h.slice != nil {
		s.Put(h.key, h.slice)
		return
	}
	s.Put(h.key, h.clone)
}

// loadGrpOf reads the parent and finds the group with grpId in it: the
// opening of every operation on a repair or a retired leg after its create.
// A group the parent no longer holds is refused, and so is a deleting clone
// (grpHolder.deleting).
func loadGrpOf(
	s etcdutil.STM,
	op string,
	cid uint64,
	spId uint64,
	parent GrpParent,
	grpId uint64,
) (*grpHolder, *pb.Group, error) {
	holder, err := loadGrpHolder(s, op, cid, spId, parent)
	if err != nil {
		return nil, nil, err
	}
	grp := holder.group(grpId)
	if grp == nil {
		return nil, nil, fail(op, "group not found")
	}
	if holder.deleting() {
		return nil, nil, fail(op, "clone deleting")
	}
	return holder, grp, nil
}

// loadSpConfAnyLevel is loadSpConfForOp without its sp_level refusal: the
// SP exists, it is the one the caller addressed, and it is not being
// deleted. Every repair operation loads its pool through it, so an operator
// can finish, abort, cancel and delete at the levels where the operator is in
// charge, and CreateRepair applies its own level gate after it
// (CreateRepairLevelGate); the sp drain removes a deleting pool's repairs and
// retired legs itself (dnv-worker.md, The sp drain).
func loadSpConfAnyLevel(
	s etcdutil.STM,
	op string,
	cid uint64,
	spName string,
	spId uint64,
) (*pb.SpConf, error) {
	conf, err := loadSpConfForHealth(s, op, cid, spName, spId)
	if err != nil {
		return nil, err
	}
	if conf.GetDeleting() {
		return nil, fail(op, "sp deleting")
	}
	return conf, nil
}

// loadSpConfForHealth is the check the epoch writes of a repair's new leg and
// side and of a retired side make: the SP exists and is the one the caller
// addressed, at every level and while it is deleted too, as the other epoch
// writes read their record whatever its pool's state.
func loadSpConfForHealth(
	s etcdutil.STM,
	op string,
	cid uint64,
	spName string,
	spId uint64,
) (*pb.SpConf, error) {
	conf := &pb.SpConf{}
	if !s.Get(SpConfKey(cid, spName), conf) {
		return nil, fail(op, "sp not found")
	}
	if conf.GetSpId() != spId {
		return nil, fail(op, "sp id changed")
	}
	return conf, nil
}

// loadRepair reads the repair named repName for an op. A name the pool's
// repair_name_list does not hold is ReasonRepairNotFound; a listed name
// whose key is missing is a lost key, which LoadSp reports in Missing and
// which the op refuses rather than guess at.
func loadRepair(
	s etcdutil.STM,
	op string,
	cid uint64,
	conf *pb.SpConf,
	repName string,
) (*pb.Repair, error) {
	if !containsName(conf.GetRepairNameList(), repName) {
		return nil, fail(op, ReasonRepairNotFound)
	}
	rep := &pb.Repair{}
	if !s.Get(RepairKey(cid, conf.GetSpId(), repName), rep) {
		return nil, fail(op, "repair record missing")
	}
	return rep, nil
}

// loadRetiredLeg is loadRepair for a retired leg named by its leg id.
func loadRetiredLeg(
	s etcdutil.STM,
	op string,
	cid uint64,
	conf *pb.SpConf,
	legId uint64,
) (*pb.RetiredLeg, error) {
	if !containsId(conf.GetRetiredLegIdList(), legId) {
		return nil, fail(op, ReasonRetiredLegNotFound)
	}
	retired := &pb.RetiredLeg{}
	if !s.Get(RetiredLegKey(cid, conf.GetSpId(), legId), retired) {
		return nil, fail(op, "retired leg record missing")
	}
	return retired, nil
}

// stageReason is the refusal of an operation the repair's stage does not
// allow: the stage, by its name.
func stageReason(rep *pb.Repair) string {
	return "repair stage is " + rep.GetRepStage().String()
}

// RepairNewLeg says where CreateRepair's new leg comes from: the retired leg
// of the group whose leg id RetiredLegId is, or, when it is zero, a fresh leg
// on the disk node Dn, which the caller picked by a scan planned against
// GrpDnAddrs: the disk node of every side of the group as the caller's
// snapshot held them, which the scan black-listed and whose locations it
// excluded at its first tier (architecture.md, Per-operation allocation).
type RepairNewLeg struct {
	RetiredLegId uint64
	Dn           Cand
	GrpDnAddrs   []string
}

// CreateRepair creates the repair repName of the group grpId, which parent
// holds, for the bad leg badLegId, and returns the repair's rep_id and its
// new leg's leg id.
//
// The group must be an md-raid1 group of a pool that is not being deleted
// (loadSpConfAnyLevel), at a level below the name's level gate
// (CreateRepairLevelGate): SP_LEVEL_NO_THINPOOL for a name with
// common.RepNameReservedPrefix, the sp worker's repair, and
// SP_LEVEL_NO_REDUND for an operator's. The name must be free in the pool,
// list and key both (ReasonRepairExists). The group must hold no repair
// (ReasonGrpHasRepair). A clone being deleted is refused. The bad leg
// must be in the group's leg_list, with exactly one side — a second one is a
// migration's destination, and a migration holds its leg — and an err_epoch
// set: a healthy leg is moved with a migration, not repaired.
//
// The new leg comes from newLeg. A retired leg of the group is taken as it
// is, and only under an operator's name: the worker's repair always takes a
// fresh leg. Its record must be in the pool's and the group's lists, of this
// group, with a side whose err_epoch is clear and whose disk node holds no
// leg of the group other than the bad one, since the group's legs sit on
// different disk nodes after the finish. It keeps its leg id, its leg_idx
// and its side, whose disk node already holds its pointer and its extents,
// and the repair keeps its retired_epoch for CancelRepair. Its record and its
// list entries go in the same transaction, and the repair takes one id, its
// rep_id. It is allowed while the group holds common.MaxRetiredLegPerGrp
// retired legs, since the finish then retires one leg and the repair took
// one. A fresh leg under an operator's name is refused for a group that
// holds that many (ReasonRetiredListFull); under the worker's name it is
// allowed there, since the worker cannot wait for an operator, and the
// finish gives back the group's oldest retired leg (FinishRepair).
//
// A fresh leg is checked against the plan before the pick. A disk node of the
// group outside newLeg.GrpDnAddrs joined the group after the caller's
// snapshot, a migration's destination say, so the pick may sit in its
// failure domain or be that very node, and the op fails with
// ReasonCandidateChanged: the caller plans again from a snapshot that holds
// it. Only a gain is checked: a node that left the group frees room the scan
// did not count on. That check runs ahead of the address-based refusal of a
// disk node that already holds a side of the group (grpHostsAddr), so the
// refusal fires only for a pick inside the caller's own plan, a node its
// scan black-listed, and stays a hard error for a caller that hands one.
// Neither check is the failure-domain rule, which the scan's second tier
// relaxes when the cluster has no other domain to offer. The pick itself is
// re-validated with checkDnPick's vanishedIsChanged: a disk node deleted,
// disabled or short of extents since the scan answers ReasonCandidateChanged,
// as a moved capacity key does, so the caller plans again instead of
// failing. A fresh leg takes three ids through TakeSpIds — the rep_id, its
// leg id and its side id in that order — the group's ext_cnt from its disk
// node, which gets its pointer, the slot every new side takes, and the next
// free leg_idx of the group; its side is unprovisioned until the sp worker's
// provisioned flip.
//
// It writes the Repair, CREATED with its flag clear, the parent with the
// group's rep_name set, the SpConf with the name listed and next_id moved,
// the disk node's bookkeeping for a fresh leg, and bumps SpRev.
func CreateRepair(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	expectRev uint64,
	repName string,
	parent GrpParent,
	grpId uint64,
	badLegId uint64,
	newLeg RepairNewLeg,
	cc *pb.ClusterConf,
) (uint64, uint64, error) {
	planned := make(map[string]struct{}, len(newLeg.GrpDnAddrs))
	for _, addrPort := range newLeg.GrpDnAddrs {
		planned[addrPort] = struct{}{}
	}
	reserved := common.HasRepNameReservedPrefix(repName)
	repId, newLegId := uint64(0), uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		repId, newLegId = 0, 0
		if err := checkSpRev(
			s, opCreateRepair, shard, cid, spId, expectRev,
		); err != nil {
			return err
		}
		conf, err := loadSpConfAnyLevel(s, opCreateRepair, cid, spName, spId)
		if err != nil {
			return err
		}
		if err := checkCreateRepairLevel(conf, repName); err != nil {
			return err
		}
		if !isMdRaid1(conf) {
			return fail(opCreateRepair, "group is not md-raid1")
		}
		repKey := RepairKey(cid, spId, repName)
		if containsName(conf.GetRepairNameList(), repName) ||
			s.Get(repKey, &pb.Repair{}) {
			return fail(opCreateRepair, ReasonRepairExists)
		}
		holder, grp, err := loadGrpOf(
			s, opCreateRepair, cid, spId, parent, grpId)
		if err != nil {
			return err
		}
		if grp.GetRepName() != "" {
			return fail(opCreateRepair, ReasonGrpHasRepair)
		}
		badIdx := legIdxIn(grp.GetLegList(), badLegId)
		if badIdx < 0 {
			return fail(opCreateRepair, "bad leg not found")
		}
		bad := grp.GetLegList()[badIdx]
		if len(bad.GetSideList()) != 1 {
			return fail(opCreateRepair, "bad leg has no single side")
		}
		if bad.GetErrEpoch() == 0 {
			return fail(opCreateRepair,
				"bad leg is healthy: use a migration")
		}
		rep := &pb.Repair{
			GrpId:        grpId,
			RepStage:     pb.RepStage_REP_STAGE_CREATED,
			BadLegIdList: []uint64{badLegId},
			SliceId:      parent.SliceId,
			CloneName:    parent.CloneName,
		}
		if newLeg.RetiredLegId != 0 {
			if reserved {
				return fail(opCreateRepair, reasonReservedNameRetiredLeg)
			}
			leg, epoch, err := takeRetiredLeg(
				s, cid, conf, grp, badLegId, newLeg.RetiredLegId)
			if err != nil {
				return err
			}
			if repId, err = TakeSpIds(conf, opCreateRepair, 1); err != nil {
				return err
			}
			newLegId = leg.GetLegId()
			rep.NewLegList = []*pb.Leg{leg}
			rep.NewLegIdToRetiredEpoch = map[uint64]uint64{newLegId: epoch}
		} else {
			leg, err := placeFreshLeg(
				s, cid, spId, conf, grp, planned, newLeg.Dn, cc, reserved)
			if err != nil {
				return err
			}
			repId = leg.GetLegId() - 1
			newLegId = leg.GetLegId()
			rep.NewLegList = []*pb.Leg{leg}
		}
		rep.RepId = repId
		grp.RepName = repName
		s.Put(repKey, rep)
		holder.put(s)
		conf.RepairNameList = append(conf.RepairNameList, repName)
		s.Put(SpConfKey(cid, spName), conf)
		return BumpSpRev(s, opCreateRepair, shard, cid, spId)
	})
	if err != nil {
		return 0, 0, err
	}
	return repId, newLegId, nil
}

// takeRetiredLeg is CreateRepair's new leg taken from the retired leg legId:
// it checks the retired leg against the group and the bad leg, stages the
// delete of its record and takes it out of the pool's and the group's lists,
// and returns it as a Leg with its retired_epoch.
func takeRetiredLeg(
	s etcdutil.STM,
	cid uint64,
	conf *pb.SpConf,
	grp *pb.Group,
	badLegId uint64,
	legId uint64,
) (*pb.Leg, uint64, error) {
	retired, err := loadRetiredLeg(s, opCreateRepair, cid, conf, legId)
	if err != nil {
		return nil, 0, err
	}
	if retired.GetGrpId() != grp.GetGrpId() ||
		!containsId(grp.GetRetiredLegIdList(), legId) {
		return nil, 0, fail(opCreateRepair, "retired leg of another group")
	}
	side := retired.GetSide()
	if side.GetErrEpoch() != 0 {
		return nil, 0, fail(opCreateRepair, "retired side is unhealthy")
	}
	for _, leg := range grp.GetLegList() {
		if leg.GetLegId() == badLegId {
			continue
		}
		for _, legSide := range leg.GetSideList() {
			if legSide.GetAddrPort() == side.GetAddrPort() {
				return nil, 0, fail(opCreateRepair,
					"retired side shares a dn with the group")
			}
		}
	}
	s.Del(RetiredLegKey(cid, conf.GetSpId(), legId))
	conf.RetiredLegIdList = removeId(conf.GetRetiredLegIdList(), legId)
	grp.RetiredLegIdList = removeId(grp.GetRetiredLegIdList(), legId)
	leg := &pb.Leg{
		LegId:    legId,
		LegIdx:   retired.GetLegIdx(),
		SideList: []*pb.Side{side},
	}
	return leg, retired.GetRetiredEpoch(), nil
}

// placeFreshLeg is CreateRepair's fresh new leg: it applies the retired-leg
// bound, which reserved, the worker's name, lifts, and the placement checks
// CreateRepair describes to the pick, takes the three ids, charges the disk
// node and returns the new leg, whose leg id is the rep_id plus one.
func placeFreshLeg(
	s etcdutil.STM,
	cid uint64,
	spId uint64,
	conf *pb.SpConf,
	grp *pb.Group,
	planned map[string]struct{},
	dn Cand,
	cc *pb.ClusterConf,
	reserved bool,
) (*pb.Leg, error) {
	if !reserved &&
		len(grp.GetRetiredLegIdList()) >= common.MaxRetiredLegPerGrp {
		return nil, fail(opCreateRepair, ReasonRetiredListFull)
	}
	// The plan first: a disk node the group gained since the snapshot is a
	// stale plan, which the caller plans again, not a refusal, even when it
	// is the pick's own node. The group has no repair here, so its
	// disk-node set is its legs'.
	if grpGainedDn(grp, nil, planned) {
		return nil, fail(opCreateRepair, ReasonCandidateChanged)
	}
	if grpHostsAddr(grp, nil, dn.AddrPort) {
		return nil, fail(opCreateRepair, "dn already in the group")
	}
	slot, err := firstCntlidSlot(conf, opCreateRepair)
	if err != nil {
		return nil, err
	}
	extCnt := grp.GetExtCnt()
	if extCnt == 0 {
		return nil, fail(opCreateRepair, "group is empty")
	}
	dnConf, err := checkDnPick(s, opCreateRepair, cid, cc, dn, extCnt, true)
	if err != nil {
		return nil, err
	}
	repId, err := TakeSpIds(conf, opCreateRepair, 3)
	if err != nil {
		return nil, err
	}
	legId, sideId := repId+1, repId+2
	leg := &pb.Leg{
		LegId:  legId,
		LegIdx: nextLegIdx(grp),
		SideList: []*pb.Side{{
			SideId:      sideId,
			AddrPort:    dn.AddrPort,
			CntlidSlot:  slot,
			NvmeTrConf:  dnConf.GetNvmeTrConf(),
			ErrEpoch:    0,
			Provisioned: false,
		}},
	}
	ptr := &pb.SidePointer{SpId: spId, LegId: legId, SideId: sideId}
	err = chargeDn(
		s, opCreateRepair, cid, cc, dn.AddrPort, dnConf, ptr, extCnt)
	if err != nil {
		return nil, err
	}
	return leg, nil
}

// FinishRepairResult is what FinishRepair did: the finished repair's rep_id,
// the leg id of the leg it retired, and the leg id of the retired leg it gave
// back to make room for that one, zero when it gave none back.
type FinishRepairResult struct {
	RepId          uint64
	RetiredLegId   uint64
	GivenBackLegId uint64
}

// FinishRepair finishes the STARTED repair repName at now, the caller's unix
// seconds, and returns what it did.
//
// It takes a pool at any level that is not being deleted
// (loadSpConfAnyLevel), and a repair whose group its parent still holds,
// names it, and is not a deleting clone's. The repair must have exactly one
// bad leg, in the group's leg_list with one side, and one new leg. Any stage
// but STARTED is refused with the stage: the primary reports the new leg
// holding a place in the array before the worker moves the repair to
// STARTED, and from then on finishing loses nothing, since the bad leg's
// data is kept.
//
// The new leg takes the bad leg's position in leg_list and its leg_idx, the
// place in the array it fills, and keeps its leg id, its side and its
// err_epoch. The bad leg becomes a RetiredLeg under its own leg id: its
// side, as it is, keeps its pointer and its extents on its disk node, with
// the group's id, its leg_idx, the parent and now as its retired_epoch; the
// bad leg's own err_epoch goes with the leg. The Repair key and its list
// entry go, the group's rep_name is cleared, the retired leg joins the
// group's and the pool's lists, and SpRev is bumped.
//
// The group's retired legs never pass common.MaxRetiredLegPerGrp here. An
// operator's repair finds room: its fresh new leg was refused at the bound,
// and a new leg taken from a retired leg left a place. The sp worker's
// repair, whose name carries common.RepNameReservedPrefix, took a fresh leg
// at the bound, so when its group holds that many retired legs the finish
// first gives back the group's oldest one (oldestRetiredLeg), as
// DeleteRetiredLeg gives one back: its side returns the group's ext_cnt and
// its pointer to its disk node, with the node's capacity key and DnRev,
// through the DN ledger the drains use, and its record and its two list
// entries go. The group keeps its in-sync leg and its newest retired copy,
// and a cancelled repair costs no retired copy, since only the finish gives
// one back. An operator's repair never gives one back.
//
// cc is the cluster's stored conf, gated at the top of the STM as the drains
// gate it, for the bins of the capacity keys the give-back maintains.
func FinishRepair(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	expectRev uint64,
	repName string,
	now uint64,
	cc *pb.ClusterConf,
) (FinishRepairResult, error) {
	var res FinishRepairResult
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		res = FinishRepairResult{}
		if err := ValidateClusterConf(cc); err != nil {
			return fail(opFinishRepair, err.Error())
		}
		if err := checkSpRev(
			s, opFinishRepair, shard, cid, spId, expectRev,
		); err != nil {
			return err
		}
		conf, err := loadSpConfAnyLevel(s, opFinishRepair, cid, spName, spId)
		if err != nil {
			return err
		}
		rep, err := loadRepair(s, opFinishRepair, cid, conf, repName)
		if err != nil {
			return err
		}
		if rep.GetRepStage() != pb.RepStage_REP_STAGE_STARTED {
			return fail(opFinishRepair, stageReason(rep))
		}
		holder, grp, err := loadGrpOf(s, opFinishRepair, cid, spId,
			repairParent(rep), rep.GetGrpId())
		if err != nil {
			return err
		}
		if grp.GetRepName() != repName {
			return fail(opFinishRepair, "group does not name the repair")
		}
		if len(rep.GetBadLegIdList()) != 1 {
			return fail(opFinishRepair, "repair has no single bad leg")
		}
		if len(rep.GetNewLegList()) != 1 {
			return fail(opFinishRepair, "repair has no single new leg")
		}
		badLegId := rep.GetBadLegIdList()[0]
		badIdx := legIdxIn(grp.GetLegList(), badLegId)
		if badIdx < 0 {
			return fail(opFinishRepair, "bad leg not found")
		}
		bad := grp.GetLegList()[badIdx]
		if len(bad.GetSideList()) != 1 {
			return fail(opFinishRepair, "bad leg has no single side")
		}
		var rel *dnReleaser
		givenBack := uint64(0)
		if common.HasRepNameReservedPrefix(repName) &&
			len(grp.GetRetiredLegIdList()) >= common.MaxRetiredLegPerGrp {
			oldestId, oldest, err := oldestRetiredLeg(s, cid, conf, grp)
			if err != nil {
				return err
			}
			rel = newDnReleaser(s, cid, cc)
			side := oldest.GetSide()
			rel.release(
				side.GetAddrPort(), spId, side.GetSideId(), grp.GetExtCnt())
			s.Del(RetiredLegKey(cid, spId, oldestId))
			grp.RetiredLegIdList = removeId(grp.GetRetiredLegIdList(), oldestId)
			conf.RetiredLegIdList = removeId(conf.GetRetiredLegIdList(), oldestId)
			givenBack = oldestId
		}
		newLeg := proto.Clone(rep.GetNewLegList()[0]).(*pb.Leg)
		newLeg.LegIdx = bad.GetLegIdx()
		grp.LegList[badIdx] = newLeg
		s.Put(RetiredLegKey(cid, spId, badLegId), &pb.RetiredLeg{
			GrpId:        grp.GetGrpId(),
			LegIdx:       bad.GetLegIdx(),
			Side:         bad.GetSideList()[0],
			RetiredEpoch: now,
			SliceId:      rep.GetSliceId(),
			CloneName:    rep.GetCloneName(),
		})
		grp.RetiredLegIdList = append(grp.RetiredLegIdList, badLegId)
		grp.RepName = ""
		holder.put(s)
		s.Del(RepairKey(cid, spId, repName))
		conf.RepairNameList = removeName(conf.GetRepairNameList(), repName)
		conf.RetiredLegIdList = append(conf.RetiredLegIdList, badLegId)
		s.Put(SpConfKey(cid, spName), conf)
		if rel != nil {
			if err := rel.flush(opFinishRepair); err != nil {
				return err
			}
		}
		res = FinishRepairResult{
			RepId:          rep.GetRepId(),
			RetiredLegId:   badLegId,
			GivenBackLegId: givenBack,
		}
		return BumpSpRev(s, opFinishRepair, shard, cid, spId)
	})
	if err != nil {
		return FinishRepairResult{}, err
	}
	return res, nil
}

// oldestRetiredLeg is the retired leg FinishRepair gives back for the
// worker's repair at the bound: of the retired legs grp lists, read inside the
// caller's STM, the one with the smallest retired_epoch, and of those the
// smallest leg id, so the choice depends on the records alone and never on
// the order of the list. Each listed id must be in the pool's list with its
// record (loadRetiredLeg), and its record must name the group. The caller
// gives a group at the bound, so the list holds at least one leg
// (TestRepairReasonsAreDistinct keeps the bound above zero).
func oldestRetiredLeg(
	s etcdutil.STM,
	cid uint64,
	conf *pb.SpConf,
	grp *pb.Group,
) (uint64, *pb.RetiredLeg, error) {
	bestId := uint64(0)
	var best *pb.RetiredLeg
	for _, legId := range grp.GetRetiredLegIdList() {
		retired, err := loadRetiredLeg(s, opFinishRepair, cid, conf, legId)
		if err != nil {
			return 0, nil, err
		}
		if retired.GetGrpId() != grp.GetGrpId() {
			return 0, nil, fail(opFinishRepair, "retired leg of another group")
		}
		if best == nil ||
			retired.GetRetiredEpoch() < best.GetRetiredEpoch() ||
			(retired.GetRetiredEpoch() == best.GetRetiredEpoch() &&
				legId < bestId) {
			bestId, best = legId, retired
		}
	}
	return bestId, best, nil
}

// CancelRepair cancels the ABORTED repair repName and returns its rep_id.
//
// It takes a pool at any level that is not being deleted
// (loadSpConfAnyLevel), and a repair whose group its parent still holds,
// names it, and is not a deleting clone's. Any stage but ABORTED is refused
// with the stage: the primary has confirmed that the new leg is no member of
// the array, so taking it away changes nothing the array holds.
//
// A new leg taken from a retired leg goes back as that retired leg, never
// destroyed: a RetiredLeg under its leg id with the retired_epoch the repair
// kept, its own leg_idx, its side as it is now and the repair's parent,
// listed again in the group's and the pool's lists. A fresh new leg's sides
// give their disk nodes back the group's ext_cnt and their pointers, through
// the DN ledger the drains use, with each node's capacity key and DnRev. The
// Repair key and its list entry go, the group's rep_name is cleared and
// SpRev is bumped.
//
// cc is the cluster's stored conf, gated at the top of the STM as the drains
// gate it, for the bins of the capacity keys the release maintains.
func CancelRepair(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	expectRev uint64,
	repName string,
	cc *pb.ClusterConf,
) (uint64, error) {
	repId := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		repId = 0
		if err := ValidateClusterConf(cc); err != nil {
			return fail(opCancelRepair, err.Error())
		}
		if err := checkSpRev(
			s, opCancelRepair, shard, cid, spId, expectRev,
		); err != nil {
			return err
		}
		conf, err := loadSpConfAnyLevel(s, opCancelRepair, cid, spName, spId)
		if err != nil {
			return err
		}
		rep, err := loadRepair(s, opCancelRepair, cid, conf, repName)
		if err != nil {
			return err
		}
		if rep.GetRepStage() != pb.RepStage_REP_STAGE_ABORTED {
			return fail(opCancelRepair, stageReason(rep))
		}
		holder, grp, err := loadGrpOf(s, opCancelRepair, cid, spId,
			repairParent(rep), rep.GetGrpId())
		if err != nil {
			return err
		}
		if grp.GetRepName() != repName {
			return fail(opCancelRepair, "group does not name the repair")
		}
		rel := newDnReleaser(s, cid, cc)
		for _, leg := range rep.GetNewLegList() {
			epoch, reused := rep.GetNewLegIdToRetiredEpoch()[leg.GetLegId()]
			if !reused {
				releaseLeg(rel, spId, leg, grp.GetExtCnt())
				continue
			}
			if len(leg.GetSideList()) != 1 {
				return fail(opCancelRepair, "new leg has no single side")
			}
			s.Put(RetiredLegKey(cid, spId, leg.GetLegId()), &pb.RetiredLeg{
				GrpId:        grp.GetGrpId(),
				LegIdx:       leg.GetLegIdx(),
				Side:         leg.GetSideList()[0],
				RetiredEpoch: epoch,
				SliceId:      rep.GetSliceId(),
				CloneName:    rep.GetCloneName(),
			})
			grp.RetiredLegIdList = append(
				grp.RetiredLegIdList, leg.GetLegId())
			conf.RetiredLegIdList = append(
				conf.RetiredLegIdList, leg.GetLegId())
		}
		grp.RepName = ""
		holder.put(s)
		s.Del(RepairKey(cid, spId, repName))
		conf.RepairNameList = removeName(conf.GetRepairNameList(), repName)
		s.Put(SpConfKey(cid, spName), conf)
		if err := rel.flush(opCancelRepair); err != nil {
			return err
		}
		repId = rep.GetRepId()
		return BumpSpRev(s, opCancelRepair, shard, cid, spId)
	})
	if err != nil {
		return 0, err
	}
	return repId, nil
}

// releaseLeg gives every side of leg back to its disk node through rel, with
// its group's extCnt.
func releaseLeg(rel *dnReleaser, spId uint64, leg *pb.Leg, extCnt uint64) {
	for _, side := range leg.GetSideList() {
		rel.release(side.GetAddrPort(), spId, side.GetSideId(), extCnt)
	}
}

// loadRepairLive is the opening SetRepairAbort and SetRepairStage share
// after the pool's check: the repair named repName and, for a clone meta
// group's repair, the refusal of a clone that is gone or being deleted
// (grpHolder.deleting). A slice's group needs no read here: the slice lives
// as long as its pool.
func loadRepairLive(
	s etcdutil.STM,
	op string,
	cid uint64,
	conf *pb.SpConf,
	repName string,
) (*pb.Repair, error) {
	rep, err := loadRepair(s, op, cid, conf, repName)
	if err != nil {
		return nil, err
	}
	if rep.GetCloneName() == "" {
		return rep, nil
	}
	holder, err := loadGrpHolder(
		s, op, cid, conf.GetSpId(), repairParent(rep))
	if err != nil {
		return nil, err
	}
	if holder.deleting() {
		return nil, fail(op, "clone deleting")
	}
	return rep, nil
}

// SetRepairAbort sets the abort flag of the CREATED repair repName and
// returns its rep_id; TryToAbortRepair is this operation, and so is the sp
// worker's try-to-abort of its own repair (dnv-worker.md AR8). The fan-out the
// SpRev bump starts carries the flag to the cntlrs, and the primary answers
// with its verdict: the worker moves the repair to STARTED when the new leg
// holds a place whatever the flag says, and to ABORTED when the primary
// confirms the abort (SetRepairStage).
//
// It takes a pool at any level that is not being deleted
// (loadSpConfAnyLevel), and a repair that is not a deleting clone's. Any
// stage but CREATED is refused with the stage: a started copy has no exit,
// and an aborted repair is aborted. A CREATED repair whose flag is already
// set is answered as done and written to not at all, so a repeated request
// raises no revision.
func SetRepairAbort(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	expectRev uint64,
	repName string,
) (uint64, error) {
	repId := uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		repId = 0
		if err := checkSpRev(
			s, opSetRepairAbort, shard, cid, spId, expectRev,
		); err != nil {
			return err
		}
		conf, err := loadSpConfAnyLevel(
			s, opSetRepairAbort, cid, spName, spId)
		if err != nil {
			return err
		}
		rep, err := loadRepairLive(s, opSetRepairAbort, cid, conf, repName)
		if err != nil {
			return err
		}
		if rep.GetRepStage() != pb.RepStage_REP_STAGE_CREATED {
			return fail(opSetRepairAbort, stageReason(rep))
		}
		repId = rep.GetRepId()
		if rep.GetTryToAbort() {
			return nil
		}
		rep.TryToAbort = true
		s.Put(RepairKey(cid, spId, repName), rep)
		return BumpSpRev(s, opSetRepairAbort, shard, cid, spId)
	})
	if err != nil {
		return 0, err
	}
	return repId, nil
}

// SetRepairStage moves the CREATED repair repName to stage, STARTED or
// ABORTED: the sp worker's move from the primary's verdict on the repair.
// repId is the rep_id the verdict named, and a repair that carries another
// one — finished or cancelled since, and created again under the name — is
// refused, as is any stage but CREATED, so a verdict moves only the repair
// it answers and only once. A move to ABORTED also needs the stored abort
// flag set: the primary confirms an abort only for a repair whose flag its
// request carried, and the flag is never cleared.
//
// It takes a pool at any level that is not being deleted
// (loadSpConfAnyLevel), since it only follows the primary's report and an
// operator may need to finish a repair at a level where the operator is in
// charge, and a repair that is not a deleting clone's. It writes the Repair
// and bumps SpRev, since the cn agent acts on the stage. It takes no
// expected revision: the worker converges on what etcd holds.
func SetRepairStage(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	repName string,
	repId uint64,
	stage pb.RepStage,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		conf, err := loadSpConfAnyLevel(
			s, opSetRepairStage, cid, spName, spId)
		if err != nil {
			return err
		}
		rep, err := loadRepairLive(s, opSetRepairStage, cid, conf, repName)
		if err != nil {
			return err
		}
		if rep.GetRepId() != repId {
			return fail(opSetRepairStage, "repair id changed")
		}
		if rep.GetRepStage() != pb.RepStage_REP_STAGE_CREATED {
			return fail(opSetRepairStage, stageReason(rep))
		}
		switch stage {
		case pb.RepStage_REP_STAGE_STARTED:
		case pb.RepStage_REP_STAGE_ABORTED:
			if !rep.GetTryToAbort() {
				return fail(opSetRepairStage, "abort flag not set")
			}
		default:
			return fail(opSetRepairStage, "not a stage move")
		}
		rep.RepStage = stage
		s.Put(RepairKey(cid, spId, repName), rep)
		return BumpSpRev(s, opSetRepairStage, shard, cid, spId)
	})
}

// DeleteRetiredLeg deletes the retired leg legId and gives its side back to
// its disk node.
//
// It takes a pool at any level that is not being deleted
// (loadSpConfAnyLevel), and a retired leg whose group its parent still holds
// and lists it, and that is not a deleting clone's: the clone drain's final
// transaction gives that one back. The side gives its disk node back the
// group's ext_cnt and its pointer, through the DN ledger the drains use; the
// RetiredLeg key and its two list entries go, and SpRev is bumped. The
// operator's delete and the sp worker's retire timer run it alike, and
// FinishRepair gives a retired leg back the same way for the sp worker's
// repair at the bound.
//
// cc is the cluster's stored conf, gated at the top of the STM as the drains
// gate it.
func DeleteRetiredLeg(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	expectRev uint64,
	legId uint64,
	cc *pb.ClusterConf,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		if err := ValidateClusterConf(cc); err != nil {
			return fail(opDeleteRetiredLeg, err.Error())
		}
		if err := checkSpRev(
			s, opDeleteRetiredLeg, shard, cid, spId, expectRev,
		); err != nil {
			return err
		}
		conf, err := loadSpConfAnyLevel(
			s, opDeleteRetiredLeg, cid, spName, spId)
		if err != nil {
			return err
		}
		retired, err := loadRetiredLeg(s, opDeleteRetiredLeg, cid, conf, legId)
		if err != nil {
			return err
		}
		holder, grp, err := loadGrpOf(s, opDeleteRetiredLeg, cid, spId,
			retiredParent(retired), retired.GetGrpId())
		if err != nil {
			return err
		}
		if !containsId(grp.GetRetiredLegIdList(), legId) {
			return fail(opDeleteRetiredLeg,
				"group does not list the retired leg")
		}
		rel := newDnReleaser(s, cid, cc)
		side := retired.GetSide()
		rel.release(
			side.GetAddrPort(), spId, side.GetSideId(), grp.GetExtCnt())
		s.Del(RetiredLegKey(cid, spId, legId))
		grp.RetiredLegIdList = removeId(grp.GetRetiredLegIdList(), legId)
		holder.put(s)
		conf.RetiredLegIdList = removeId(conf.GetRetiredLegIdList(), legId)
		s.Put(SpConfKey(cid, spName), conf)
		if err := rel.flush(opDeleteRetiredLeg); err != nil {
			return err
		}
		return BumpSpRev(s, opDeleteRetiredLeg, shard, cid, spId)
	})
}

// loadRepairForHealth is the opening of the epoch writes on a repair's new
// leg and side: the pool's check (loadSpConfForHealth), the repair named
// repName, and its rep_id, which proves the record is the one whose leg the
// caller observed.
func loadRepairForHealth(
	s etcdutil.STM,
	op string,
	cid uint64,
	spId uint64,
	spName string,
	repName string,
	repId uint64,
) (*pb.Repair, error) {
	conf, err := loadSpConfForHealth(s, op, cid, spName, spId)
	if err != nil {
		return nil, err
	}
	rep, err := loadRepair(s, op, cid, conf, repName)
	if err != nil {
		return nil, err
	}
	if rep.GetRepId() != repId {
		return nil, fail(op, "repair id changed")
	}
	return rep, nil
}

// newLegOf is the repair's new leg with legId, or nil.
func newLegOf(rep *pb.Repair, legId uint64) *pb.Leg {
	for _, leg := range rep.GetNewLegList() {
		if leg.GetLegId() == legId {
			return leg
		}
	}
	return nil
}

// SetRepairLegErrEpoch sets or clears the err_epoch of the new leg legId of
// the repair repName by the rule every epoch write shares (applyErrEpoch):
// a new leg is probed by the primary as a group's legs are, and its health
// lives in the repair until the finish makes it a leg of the group. The
// repair is addressed by its name and proved by repId; the pool must exist
// and be the one addressed (loadSpConfForHealth). A record that already
// holds what is wanted is not written, and nothing bumps a revision.
func SetRepairLegErrEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	spId uint64,
	spName string,
	repName string,
	repId uint64,
	legId uint64,
	epoch uint64,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		rep, err := loadRepairForHealth(s, opSetRepairLegErrEpoch, cid,
			spId, spName, repName, repId)
		if err != nil {
			return err
		}
		leg := newLegOf(rep, legId)
		if leg == nil {
			return fail(opSetRepairLegErrEpoch, "leg not found")
		}
		value, changed := applyErrEpoch(leg.GetErrEpoch(), epoch)
		if !changed {
			return nil
		}
		leg.ErrEpoch = value
		s.Put(RepairKey(cid, spId, repName), rep)
		return nil
	})
}

// SetRepairSideErrEpoch is SetRepairLegErrEpoch for the side sideId of a
// new leg of the repair, whose health its disk node's reports keep.
func SetRepairSideErrEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	spId uint64,
	spName string,
	repName string,
	repId uint64,
	sideId uint64,
	epoch uint64,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		rep, err := loadRepairForHealth(s, opSetRepairSideErrEpoch, cid,
			spId, spName, repName, repId)
		if err != nil {
			return err
		}
		var side *pb.Side
		for _, leg := range rep.GetNewLegList() {
			for _, legSide := range leg.GetSideList() {
				if legSide.GetSideId() == sideId {
					side = legSide
				}
			}
		}
		if side == nil {
			return fail(opSetRepairSideErrEpoch, "side not found")
		}
		value, changed := applyErrEpoch(side.GetErrEpoch(), epoch)
		if !changed {
			return nil
		}
		side.ErrEpoch = value
		s.Put(RepairKey(cid, spId, repName), rep)
		return nil
	})
}

// SetRetiredSideErrEpoch sets or clears the err_epoch of the side of the
// retired leg legId by the rule every epoch write shares: a retired side
// keeps its exports, its disk node reports it, and its health is what
// CreateRepair reads before it takes the leg back. The record is addressed
// by its leg id and its side proved by sideId; the pool must exist and be
// the one addressed (loadSpConfForHealth). A record that already holds what
// is wanted is not written, and nothing bumps a revision.
func SetRetiredSideErrEpoch(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	spId uint64,
	spName string,
	legId uint64,
	sideId uint64,
	epoch uint64,
) error {
	return cli.RunSTM(ctx, func(s etcdutil.STM) error {
		conf, err := loadSpConfForHealth(
			s, opSetRetiredSideErrEpoch, cid, spName, spId)
		if err != nil {
			return err
		}
		retired, err := loadRetiredLeg(
			s, opSetRetiredSideErrEpoch, cid, conf, legId)
		if err != nil {
			return err
		}
		side := retired.GetSide()
		if side.GetSideId() != sideId {
			return fail(opSetRetiredSideErrEpoch, "side not found")
		}
		value, changed := applyErrEpoch(side.GetErrEpoch(), epoch)
		if !changed {
			return nil
		}
		side.ErrEpoch = value
		s.Put(RetiredLegKey(cid, spId, legId), retired)
		return nil
	})
}

// RepairSideRef names one side of a repair's new leg for
// FlipRepairProvisioned: the name that keys the repair, the rep_id that
// proves the record is the one observed, and the side pointer the disk node
// reports.
type RepairSideRef struct {
	RepName string
	RepId   uint64
	LegId   uint64
	SideId  uint64
}

// FlipRepairProvisioned sets provisioned on the side ref names, a side of a
// repair's new leg, once its disk node reports its zeroing done
// (architecture.md, Side provisioning protocol), and bumps SpRev once when it
// wrote. It reports whether it wrote and the revision its STM committed, 0
// when it wrote nothing, for FlipProvisioned's reasons. One side per call: a
// repair is a record of its own, so a flip over every repair of a pool would
// be as wide as the pool's groups.
//
// It takes a pool at any level that is not being deleted
// (loadSpConfAnyLevel). A ref that is stale is skipped, never refused, as
// FlipCloneProvisioned skips a clone that left: a repair that is gone or
// carries another rep_id, finished or cancelled since the round; a new leg or
// a side the repair does not hold; a side already provisioned; and a repair
// of a clone that is gone or being deleted, whose sides go back with the
// clone in the clone drain's final transaction (dnv-worker.md CLD9).
func FlipRepairProvisioned(
	ctx context.Context,
	cli *etcdutil.Client,
	cid uint64,
	shard uint32,
	spId uint64,
	spName string,
	ref RepairSideRef,
) (bool, uint64, error) {
	flipped, revision := false, uint64(0)
	err := cli.RunSTM(ctx, func(s etcdutil.STM) error {
		flipped, revision = false, 0
		if _, err := loadSpConfAnyLevel(
			s, opFlipRepairProvisioned, cid, spName, spId,
		); err != nil {
			return err
		}
		key := RepairKey(cid, spId, ref.RepName)
		rep := &pb.Repair{}
		if !s.Get(key, rep) || rep.GetRepId() != ref.RepId {
			return nil
		}
		if name := rep.GetCloneName(); name != "" {
			clone := &pb.Clone{}
			if !s.Get(CloneKey(cid, spId, name), clone) ||
				clone.GetDeleting() {
				return nil
			}
		}
		leg := newLegOf(rep, ref.LegId)
		var side *pb.Side
		for _, legSide := range leg.GetSideList() {
			if legSide.GetSideId() == ref.SideId {
				side = legSide
			}
		}
		if side == nil || side.GetProvisioned() {
			return nil
		}
		side.Provisioned = true
		s.Put(key, rep)
		var err error
		revision, err = bumpSpRevTo(
			s, opFlipRepairProvisioned, shard, cid, spId)
		flipped = err == nil
		return err
	})
	if err != nil {
		return false, 0, err
	}
	return flipped, revision, nil
}
