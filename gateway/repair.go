package gateway

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md, Repairs and Retired legs (gateway.md, Repairs
// and retired legs): the eight RPCs of a repair of an md-raid1 group's leg;
// ForceStartGroup, the forced start of a group's array from one leg; and
// UpdateGroupOperatorInCharge, an operator's take-over of a group's repair
// from the sp worker.
// CreateRepair creates an operator's repair only, since it refuses the
// reserved prefix of the sp worker's own repairs (validateRepName); the calls
// that address a repair by its name take both kinds, so an operator can
// drive a repair the worker created. A repair is a record of its own that
// names one bad leg of the group's leg_list and carries one new leg; the
// primary adds the new leg to the running array and the sp worker moves the
// repair's stage from the primary's report, so the gateway only ever writes
// the record and reads it back (architecture.md, Repair). A finished repair
// keeps the bad leg as a retired leg, whose side keeps its data on its DN
// until it is deleted or a repair of the group takes it back.
//
// The seven calls that write run the repair operations of model (gateway.md,
// Scope and placement: reused, never duplicated), each behind a planning
// snapshot: the snapshot resolves the cluster and the SP, checks the token
// and makes the checks whose codes model cannot give, and the op re-checks
// everything inside its own STM, the token first (GW6). ForceStartGroup alone
// also reads the primary's report, between its snapshot and its op (AG1).
// The three reads are one snapshot each over the pool's lists.

// repairOpenSp is the planning read of FinishRepair, CancelRepair,
// TryToAbortRepair and DeleteRetiredLeg: openSp — the resolution, the
// deleting gate and the token check, in that order (GW5, GW6) — in a
// read-only snapshot. Its scope hands the model op the cluster id, the
// shard, the sp id and, for FinishRepair, CancelRepair and DeleteRetiredLeg,
// the stored ClusterConf, which those ops take from their caller and never
// read themselves (architecture.md, STM discipline).
// Everything else the call decides on, the op reads in its own STM.
func (s *Server) repairOpenSp(
	ctx context.Context,
	clusterName string,
	spName string,
	tok *pb.SpRev,
) (*spScope, error) {
	var sc *spScope
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		var err error
		sc, err = openSp(stm, clusterName, spName, tok)
		return err
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return sc, nil
}

// releaseConfGate is GW11's gate of FinishRepair, CancelRepair and
// DeleteRetiredLeg, which give a side back to its DN — FinishRepair the side
// of the retired leg it gives back for the sp worker's repair at the bound —
// and so move the capacity key the STORED ladder names: a stored ClusterConf
// that is not concrete is a lost invariant, ABORTED here from the planning
// read's copy, as newDnLedger refuses it for the gateway's own releases. The
// op checks the same conf again in its STM, at the top whether it releases a
// side or not, where it could answer only FAILED_PRECONDITION; it is handed
// this copy, so through the gateway that check never fires.
func releaseConfGate(sc *spScope) error {
	if err := model.ValidateClusterConf(sc.Cc); err != nil {
		return errAborted("%v", err)
	}
	return nil
}

// grpRepair is the repair the group's rep_name names, at any stage, read
// inside the caller's STM, or nil when the group names none. The Repair a
// group names is an invariant key (architecture.md, Key table), so a missing
// one is ABORTED.
func grpRepair(
	stm etcdutil.STM,
	sc *spScope,
	grp *pb.Group,
) (*pb.Repair, error) {
	name := grp.GetRepName()
	if name == "" {
		return nil, nil
	}
	key := model.RepairKey(sc.Cid, sc.SpId(), name)
	rep := &pb.Repair{}
	if !stm.Get(key, rep) {
		return nil, errAborted("repair key %q is missing", key)
	}
	return rep, nil
}

// loadListedRepair reads the repair repair_name_list names repName, for the
// reads: a name the list does not hold is NOT_FOUND, and a listed name whose
// key is missing is a lost invariant key, ABORTED (architecture.md, Key
// table).
func loadListedRepair(
	stm etcdutil.STM,
	sc *spScope,
	repName string,
) (*pb.Repair, error) {
	if !containsName(sc.Conf.GetRepairNameList(), repName) {
		return nil, errRepairNotFound(repName)
	}
	key := model.RepairKey(sc.Cid, sc.SpId(), repName)
	rep := &pb.Repair{}
	if !stm.Get(key, rep) {
		return nil, errAborted("repair key %q is missing", key)
	}
	return rep, nil
}

// loadListedRetiredLeg is loadListedRepair for the retired leg
// retired_leg_id_list names by legId.
func loadListedRetiredLeg(
	stm etcdutil.STM,
	sc *spScope,
	legId uint64,
) (*pb.RetiredLeg, error) {
	if !containsId(sc.Conf.GetRetiredLegIdList(), legId) {
		return nil, errRetiredLegNotFound(legId)
	}
	key := model.RetiredLegKey(sc.Cid, sc.SpId(), legId)
	retired := &pb.RetiredLeg{}
	if !stm.Get(key, retired) {
		return nil, errAborted("retired_leg key %q is missing", key)
	}
	return retired, nil
}

// findRepairSide locates one side by id among the new legs of the pool's
// repairs and then among its retired legs, and returns the leg id the dn
// agent keys it by — the new leg's, or the retired leg's own — or a nil side
// when neither holds it. It is InspectSide's last walk, after the slices and
// the clones; each list is bounded by the pool's groups (architecture.md,
// Cardinality limits), and a listed key that is missing is ABORTED.
func findRepairSide(
	stm etcdutil.STM,
	sc *spScope,
	sideId uint64,
) (uint64, *pb.Side, error) {
	for _, name := range sc.Conf.GetRepairNameList() {
		rep, err := loadListedRepair(stm, sc, name)
		if err != nil {
			return 0, nil, err
		}
		for _, leg := range rep.GetNewLegList() {
			for _, side := range leg.GetSideList() {
				if side.GetSideId() == sideId {
					return leg.GetLegId(), side, nil
				}
			}
		}
	}
	for _, legId := range sc.Conf.GetRetiredLegIdList() {
		retired, err := loadListedRetiredLeg(stm, sc, legId)
		if err != nil {
			return 0, nil, err
		}
		if side := retired.GetSide(); side.GetSideId() == sideId {
			return legId, side, nil
		}
	}
	return 0, nil, nil
}

// errRepairNotFound is the NOT_FOUND of a repair name the pool does not list,
// whether the planning read sees it or model.ReasonRepairNotFound reaches the
// handler from the op's own STM, so both give one refusal the same words.
func errRepairNotFound(repName string) error {
	return errNotFound("repair %q not found", repName)
}

// errRetiredLegNotFound is errRepairNotFound for a retired leg id, answered
// for model.ReasonRetiredLegNotFound too.
func errRetiredLegNotFound(legId uint64) error {
	return errNotFound("retired leg %d not found", legId)
}

// errRepairExists is CreateRepair's ALREADY_EXISTS, answered for
// model.ReasonRepairExists too: another create took the name after the
// planning read.
func errRepairExists(repName string) error {
	return errExists("repair %q already exists", repName)
}

// errRetiredListFull is CreateRepair's RESOURCE_EXHAUSTED for a fresh new leg
// of a group that holds MaxRetiredLegPerGrp retired legs, answered for
// model.ReasonRetiredListFull too: a finish retired a leg of the group after
// the planning read. The message says how to free a place (architecture.md,
// Repairs).
func errRetiredListFull(grpId uint64) error {
	return errExhausted(
		"group %d already holds the maximum of %d retired legs: delete one "+
			"(DeleteRetiredLeg) first", grpId, common.MaxRetiredLegPerGrp)
}

// modelReason is the Reason of the model.ErrPrecondition err carries, or the
// empty string. A repair handler switches on it for the reasons model exports
// for their codes (GW7) and hands every other error to mapModelErr.
func modelReason(err error) string {
	var pre *model.ErrPrecondition
	if errors.As(err, &pre) {
		return pre.Reason
	}
	return ""
}

// CreateRepair is the CreateRepair of architecture.md, Repairs: a repair of
// the group grp_id that names one bad leg of its leg_list and carries one new
// leg — the retired leg retired_leg_id of the group taken back, or, when that
// is zero, a fresh leg on one DN.
//
// The pure checks come first: the rep_name rule with its reserved prefix and
// exactly one bad leg id, INVALID_ARGUMENT. Then a GW9 candidate unit, each
// round of which plans from its own snapshot. The snapshot makes every refusal
// of architecture.md, Repairs, in the order a request meets them there, behind
// openSp's resolution, deleting gate and token: the sp_level gate of an
// operator's name, a RedundNone pool, a name in use, the group's look-up —
// slices first, then clones (locateGrp) — a clone being deleted, a group with
// a repair, the bad leg's membership, its two sides and its health, and then
// the new leg's own checks. model.CreateRepair makes all of them again in its
// STM, where they are authoritative, but model raises every precondition as
// FAILED_PRECONDITION, so the snapshot is what gives the INVALID_ARGUMENT,
// ALREADY_EXISTS, NOT_FOUND and RESOURCE_EXHAUSTED answers their codes; of
// those, the op's STM can still meet a name taken, a retired leg gone and the
// retired list filled after the snapshot, and their exported reasons are
// answered with the same refusals. The snapshot also keeps a create that
// cannot commit from paying for a candidate scan.
//
// A fresh new leg is placed as architecture.md, Per-operation allocation,
// places a repair's: the group's DN set — its legs, since it has no repair yet
// — as the black list, their locations as tier 1, one DN of room for the
// group's ext_cnt and no metadata units. The op refuses as candidate changed a
// group that gained a DN outside that plan and a pick gone stale, and the unit
// runs the round again from a fresh snapshot (GW9). A new leg taken from a
// retired leg allocates nothing, so its round is the unit's only one.
func (s *Server) CreateRepair(
	ctx context.Context,
	req *pb.CreateRepairRequest,
) (*pb.CreateRepairReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if err := validateRepName(req.GetRepName()); err != nil {
		return nil, err
	}
	if n := len(req.GetBadLegIdList()); n != 1 {
		return nil, errInvalid(
			"bad_leg_id_list holds %d leg ids, a repair replaces exactly one",
			n)
	}
	if err := validateNodeSelector(
		"dn_selector", req.GetDnSelector(),
	); err != nil {
		return nil, err
	}
	var repId, newLegId uint64
	err := candidateUnit(ctx, func() error {
		repId, newLegId = 0, 0
		var sc *spScope
		var loc grpLocation
		err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
			var err error
			sc, loc, err = repairCreateChecks(stm, req)
			return err
		})
		if err != nil {
			return mapStmErr(err)
		}
		newLeg := model.RepairNewLeg{RetiredLegId: req.GetRetiredLegId()}
		if newLeg.RetiredLegId == 0 {
			// The group has no repair, the snapshot's refusal, so its DN
			// set is its legs'; the scan black-lists it and the op checks
			// the group against it for a DN it gained since.
			newLeg.GrpDnAddrs = grpDnAddrs(loc.Grp, nil)
			excludeLocs, err := grpDnLocations(
				ctx, s.cli, sc.Cid, newLeg.GrpDnAddrs)
			if err != nil {
				return err
			}
			picks, err := pickDns(
				ctx, s.cli, sc.Cid, sc.Cc,
				dnPickPlan{
					ExtCnt:      loc.Grp.GetExtCnt(),
					Legs:        1,
					ExcludeLocs: excludeLocs,
				},
				req.GetDnSelector(), newLeg.GrpDnAddrs,
				"new leg of the repair",
			)
			if err != nil {
				return err
			}
			newLeg.Dn = picks[0]
		}
		// The new leg's side is written provisioned false ([D15]); only
		// the sp worker flips it, once its DN reports the zeroing done
		// (architecture.md, Side provisioning protocol). A side taken from
		// a retired leg keeps the provisioned side it has.
		gotRep, gotLeg, err := model.CreateRepair(
			ctx, s.cli, sc.Cid, sc.Shard(), sc.SpId(), req.GetSpName(),
			req.GetSpRev().GetRevision(), req.GetRepName(), loc.parent(),
			req.GetGrpId(), req.GetBadLegIdList()[0], newLeg, sc.Cc,
		)
		switch modelReason(err) {
		case model.ReasonRepairExists:
			return errRepairExists(req.GetRepName())
		case model.ReasonRetiredLegNotFound:
			return errRetiredLegNotFound(req.GetRetiredLegId())
		case model.ReasonRetiredListFull:
			return errRetiredListFull(req.GetGrpId())
		}
		if err != nil {
			return mapModelErr(err)
		}
		repId, newLegId = gotRep, gotLeg
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &pb.CreateRepairReply{RepId: repId, NewLegId: newLegId}, nil
}

// repairCreateChecks is CreateRepair's planning read inside the caller's
// snapshot, its refusals in the order of architecture.md, Repairs. It returns
// the scope and the group, which the round plans from.
func repairCreateChecks(
	stm etcdutil.STM,
	req *pb.CreateRepairRequest,
) (*spScope, grpLocation, error) {
	spName := req.GetSpName()
	grpId := req.GetGrpId()
	sc, err := openSp(stm, req.GetClusterName(), spName, req.GetSpRev())
	if err != nil {
		return nil, grpLocation{}, err
	}
	// The op's level gate for the request's name (model.CreateRepairLevelGate,
	// architecture.md, Repairs). validateRepName has refused the reserved
	// prefix, so the name is an operator's and the gate is
	// SP_LEVEL_NO_REDUND: no group array is built from there up, and below
	// it an operator's create is no reaction, so the levels where reactions
	// stop take it.
	level := sc.Conf.GetSpLevel()
	if level >= model.CreateRepairLevelGate(req.GetRepName()) {
		return nil, grpLocation{}, errPrecondition(
			"storage pool %q is at %s, where no repair is created",
			spName, level)
	}
	// Redundancy is an SP-wide property the group only inherits, so the
	// test is the SP's STORED bdev_conf.
	if !isMdRaid1(spBdevConf(sc.Conf)) {
		return nil, grpLocation{}, errInvalid(
			"storage pool %q is RedundNone: there is no redundancy to repair",
			spName)
	}
	if containsName(sc.Conf.GetRepairNameList(), req.GetRepName()) {
		return nil, grpLocation{}, errRepairExists(req.GetRepName())
	}
	loc, err := locateGrp(stm, sc, spName, grpId)
	if err != nil {
		return nil, grpLocation{}, err
	}
	if err := refuseDeletingClone(loc); err != nil {
		return nil, grpLocation{}, err
	}
	grp := loc.Grp
	if repName := grp.GetRepName(); repName != "" {
		return nil, grpLocation{}, errPrecondition(
			"group %d has the repair %q: a group has one repair at a time",
			grpId, repName)
	}
	badLegId := req.GetBadLegIdList()[0]
	bad := grpLegOf(grp, badLegId)
	if bad == nil {
		return nil, grpLocation{}, errNotFound(
			"leg %d not found in the leg_list of group %d", badLegId, grpId)
	}
	if len(bad.GetSideList()) != 1 {
		return nil, grpLocation{}, errMigrRunning(bad)
	}
	if bad.GetErrEpoch() == 0 {
		return nil, grpLocation{}, errPrecondition(
			"leg %d is healthy: use a migration to move it", badLegId)
	}
	retiredId := req.GetRetiredLegId()
	if retiredId == 0 {
		if len(grp.GetRetiredLegIdList()) >= common.MaxRetiredLegPerGrp {
			return nil, grpLocation{}, errRetiredListFull(grpId)
		}
		return sc, loc, nil
	}
	retired, err := loadListedRetiredLeg(stm, sc, retiredId)
	if err != nil {
		return nil, grpLocation{}, err
	}
	if retired.GetGrpId() != grpId ||
		!containsId(grp.GetRetiredLegIdList(), retiredId) {
		return nil, grpLocation{}, errPrecondition(
			"retired leg %d is not a retired leg of group %d",
			retiredId, grpId)
	}
	side := retired.GetSide()
	if epoch := side.GetErrEpoch(); epoch != 0 {
		return nil, grpLocation{}, errPrecondition(
			"the side of retired leg %d is unhealthy (err_epoch %d)",
			retiredId, epoch)
	}
	// The group's legs sit on different DNs after the finish, when the new
	// leg takes the bad leg's place, so the retired side's DN may hold the
	// bad leg and no other leg of the group.
	for _, leg := range grp.GetLegList() {
		if leg.GetLegId() == badLegId {
			continue
		}
		for _, legSide := range leg.GetSideList() {
			if legSide.GetAddrPort() == side.GetAddrPort() {
				return nil, grpLocation{}, errPrecondition(
					"the side of retired leg %d is on disk node %q, which "+
						"holds leg %d of group %d", retiredId,
					side.GetAddrPort(), leg.GetLegId(), grpId)
			}
		}
	}
	return sc, loc, nil
}

// FinishRepair is the FinishRepair of architecture.md, Repairs: the new leg
// takes the bad leg's place in leg_list, and the bad leg becomes a retired
// leg whose side stays on its DN. The planning read resolves and checks the
// token, and gates the stored ClusterConf (releaseConfGate);
// model.FinishRepair, given the gateway's clock as the retired leg's
// retired_epoch and that conf, makes every other check — a name the pool
// does not list is model.ReasonRepairNotFound, NOT_FOUND here, and a stage
// other than STARTED or a deleting clone FAILED_PRECONDITION naming the
// stage or the clone's state (GW7). The name passes the common name rule
// alone (validateRepairRef), so an operator can finish a repair the sp
// worker created; that finish gives back the group's oldest retired leg when
// the group holds MaxRetiredLegPerGrp of them, and the reply names that leg
// in given_back_leg_id, zero when the finish gave none back, so the operator
// learns which kept copy went.
func (s *Server) FinishRepair(
	ctx context.Context,
	req *pb.FinishRepairRequest,
) (*pb.FinishRepairReply, error) {
	if err := validateRepairRef(
		req.GetClusterName(), req.GetSpName(), req.GetRepName(),
	); err != nil {
		return nil, err
	}
	sc, err := s.repairOpenSp(
		ctx, req.GetClusterName(), req.GetSpName(), req.GetSpRev())
	if err != nil {
		return nil, err
	}
	if err := releaseConfGate(sc); err != nil {
		return nil, err
	}
	res, err := model.FinishRepair(
		ctx, s.cli, sc.Cid, sc.Shard(), sc.SpId(), req.GetSpName(),
		req.GetSpRev().GetRevision(), req.GetRepName(),
		uint64(time.Now().Unix()), sc.Cc,
	)
	if modelReason(err) == model.ReasonRepairNotFound {
		return nil, errRepairNotFound(req.GetRepName())
	}
	if err != nil {
		return nil, mapModelErr(err)
	}
	return &pb.FinishRepairReply{
		RepId:          res.RepId,
		RetiredLegId:   res.RetiredLegId,
		GivenBackLegId: res.GivenBackLegId,
	}, nil
}

// CancelRepair is the CancelRepair of architecture.md, Repairs: an ABORTED
// repair goes, a fresh new leg's side giving its DN back what it took and a
// new leg taken from a retired leg going back as that retired leg, with the
// time the repair kept. The shape is FinishRepair's; model.CancelRepair
// takes the stored ClusterConf for the capacity keys of the release.
func (s *Server) CancelRepair(
	ctx context.Context,
	req *pb.CancelRepairRequest,
) (*pb.CancelRepairReply, error) {
	if err := validateRepairRef(
		req.GetClusterName(), req.GetSpName(), req.GetRepName(),
	); err != nil {
		return nil, err
	}
	sc, err := s.repairOpenSp(
		ctx, req.GetClusterName(), req.GetSpName(), req.GetSpRev())
	if err != nil {
		return nil, err
	}
	if err := releaseConfGate(sc); err != nil {
		return nil, err
	}
	repId, err := model.CancelRepair(
		ctx, s.cli, sc.Cid, sc.Shard(), sc.SpId(), req.GetSpName(),
		req.GetSpRev().GetRevision(), req.GetRepName(), sc.Cc,
	)
	if modelReason(err) == model.ReasonRepairNotFound {
		return nil, errRepairNotFound(req.GetRepName())
	}
	if err != nil {
		return nil, mapModelErr(err)
	}
	return &pb.CancelRepairReply{RepId: repId}, nil
}

// TryToAbortRepair is the TryToAbortRepair of architecture.md, Repairs: it
// sets a CREATED repair's abort flag, which the fan-out carries to the
// cntlrs, and the primary's answer moves the repair (architecture.md,
// Repair). The op is model.SetRepairAbort, the shape FinishRepair's: a
// repair whose flag is already set is answered OK with no write and no bump,
// a token the request carried having been checked first (GW6).
func (s *Server) TryToAbortRepair(
	ctx context.Context,
	req *pb.TryToAbortRepairRequest,
) (*pb.TryToAbortRepairReply, error) {
	if err := validateRepairRef(
		req.GetClusterName(), req.GetSpName(), req.GetRepName(),
	); err != nil {
		return nil, err
	}
	sc, err := s.repairOpenSp(
		ctx, req.GetClusterName(), req.GetSpName(), req.GetSpRev())
	if err != nil {
		return nil, err
	}
	repId, err := model.SetRepairAbort(
		ctx, s.cli, sc.Cid, sc.Shard(), sc.SpId(), req.GetSpName(),
		req.GetSpRev().GetRevision(), req.GetRepName(),
	)
	if modelReason(err) == model.ReasonRepairNotFound {
		return nil, errRepairNotFound(req.GetRepName())
	}
	if err != nil {
		return nil, mapModelErr(err)
	}
	return &pb.TryToAbortRepairReply{RepId: repId}, nil
}

// grpReportTarget is what ForceStartGroup's planning read carries into its
// read of the primary's report: the primary cntlr's pointer and the CN that
// hosts it. AG4 makes every field a hint: the op re-reads the pool and
// re-checks the token in its own STM.
type grpReportTarget struct {
	ClusterId uint64
	SpId      uint64
	CntlrId   uint64
	CnId      uint64
	AddrPort  string
}

// ForceStartGroup is the ForceStartGroup of architecture.md, Repairs: the
// forced-start mark of an md-raid1 group, which asks the group's primary to
// start the array from the one leg the request names (architecture.md,
// Repair), or, with cancel, its withdrawal.
//
// It is two-phase (AG4). The pure checks come first (GW4): the names, and a
// leg_id that matches the flag, zero with cancel and named without it. The
// planning snapshot then makes the refusals of architecture.md, Repairs, in
// their order, behind openSp's resolution, deleting gate and token: the
// level, a RedundNone pool, the group's look-up — slices first, then clones
// (locateGrp) — a clone being deleted, the leg the mark may name
// (forceStartLegChecks) and the primary. Outside any STM the primary's
// report is read (checkGrpArrayDown), and model.ForceStartGroup, the deciding
// STM, makes its own checks again, the token first (GW6); a leg a write took
// away since the snapshot is its model.ReasonLegNotNamable, answered in the
// gateway's words. A cancel names no leg and reads no report: it skips the
// level, the leg, the primary and the read, so that a mark can be withdrawn
// at every level, and a group with no mark is answered OK with no write and
// no bump, a token the request carried still being checked first.
func (s *Server) ForceStartGroup(
	ctx context.Context,
	req *pb.ForceStartGroupRequest,
) (*pb.ForceStartGroupReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	if req.GetCancel() && req.GetLegId() != 0 {
		return nil, errInvalid(
			"leg_id %d is set with cancel: a cancel names no leg",
			req.GetLegId())
	}
	if !req.GetCancel() && req.GetLegId() == 0 {
		return nil, errInvalid(
			"leg_id must name the leg the group's array starts from")
	}
	var sc *spScope
	var loc grpLocation
	var tgt grpReportTarget
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		var err error
		sc, loc, tgt, err = forceStartChecks(stm, req)
		return err
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	if !req.GetCancel() {
		if err := checkGrpArrayDown(ctx, tgt, req.GetGrpId()); err != nil {
			return nil, err
		}
	}
	revision, err := model.ForceStartGroup(
		ctx, s.cli, sc.Cid, sc.Shard(), sc.SpId(), req.GetSpName(),
		req.GetSpRev().GetRevision(), loc.parent(), req.GetGrpId(),
		req.GetLegId(), req.GetCancel(),
	)
	if modelReason(err) == model.ReasonLegNotNamable {
		return nil, errPrecondition(
			"leg %d is no leg the forced start of group %d may name any "+
				"more: neither a leg of its leg_list, other than the bad "+
				"leg of a started repair, nor a new leg of its repair "+
				"while that repair is started or is created with its "+
				"abort flag clear", req.GetLegId(), req.GetGrpId())
	}
	if err != nil {
		return nil, mapModelErr(err)
	}
	return &pb.ForceStartGroupReply{
		GrpId:    req.GetGrpId(),
		Revision: revision,
	}, nil
}

// forceStartChecks is ForceStartGroup's planning read inside the caller's
// snapshot, its refusals in the order of architecture.md, Repairs. It returns
// the scope and the group, which the op is handed, and, for a request that is
// no cancel, the primary whose report the call reads.
func forceStartChecks(
	stm etcdutil.STM,
	req *pb.ForceStartGroupRequest,
) (*spScope, grpLocation, grpReportTarget, error) {
	spName := req.GetSpName()
	grpId := req.GetGrpId()
	cancel := req.GetCancel()
	sc, err := openSp(stm, req.GetClusterName(), spName, req.GetSpRev())
	if err != nil {
		return nil, grpLocation{}, grpReportTarget{}, err
	}
	// The level that stops group arrays is the step at which no group array
	// is built, so no primary could start one; a cancel runs at every level,
	// since a mark that stands acts once the level is lowered.
	level := sc.Conf.GetSpLevel()
	if !cancel && level >= pb.SpLevel_SP_LEVEL_NO_REDUND {
		return nil, grpLocation{}, grpReportTarget{}, errPrecondition(
			"storage pool %q is at %s, where no group array is built",
			spName, level)
	}
	// Redundancy is an SP-wide property the group only inherits, so the
	// test is the SP's STORED bdev_conf, as CreateRepair's is.
	if !isMdRaid1(spBdevConf(sc.Conf)) {
		return nil, grpLocation{}, grpReportTarget{}, errInvalid(
			"storage pool %q is RedundNone: its groups have no array to "+
				"start", spName)
	}
	loc, err := locateGrp(stm, sc, spName, grpId)
	if err != nil {
		return nil, grpLocation{}, grpReportTarget{}, err
	}
	if err := refuseDeletingClone(loc); err != nil {
		return nil, grpLocation{}, grpReportTarget{}, err
	}
	if cancel {
		return sc, loc, grpReportTarget{}, nil
	}
	if err := forceStartLegChecks(
		stm, sc, loc.Grp, req.GetLegId(),
	); err != nil {
		return nil, grpLocation{}, grpReportTarget{}, err
	}
	cntlrs, err := loadCntlrs(stm, sc.Cid, sc.Conf)
	if err != nil {
		return nil, grpLocation{}, grpReportTarget{}, err
	}
	cntlrId, cntlr, ok := primaryCntlr(sc.Conf, cntlrs)
	if !ok {
		return nil, grpLocation{}, grpReportTarget{}, errPrecondition(
			"storage pool %q has no primary cntlr whose report could show "+
				"that the array of group %d does not run", spName, grpId)
	}
	cn := &pb.CnConf{}
	cnKey := model.CnConfKey(sc.Cid, cntlr.GetAddrPort())
	if !stm.Get(cnKey, cn) {
		// A live cntlr names its CN, and DeleteControllerNode refuses a CN
		// that hosts one, so the CN's absence is a lost invariant key
		// (architecture.md, UNEXPECTED_ERROR → `ABORTED`).
		return nil, grpLocation{}, grpReportTarget{}, errAborted(
			"cn_conf key %q is missing", cnKey)
	}
	return sc, loc, grpReportTarget{
		ClusterId: sc.Cid,
		SpId:      sc.SpId(),
		CntlrId:   cntlrId,
		CnId:      cn.GetCnId(),
		AddrPort:  cntlr.GetAddrPort(),
	}, nil
}

// forceStartLegChecks refuses a leg the forced-start mark may not name
// (architecture.md, Repairs): one in neither the group's leg_list nor the
// new legs of its repair is NOT_FOUND; a new leg of a repair whose abort
// flag is set, an ABORTED one or a CREATED one, is FAILED_PRECONDITION,
// since the abort takes that leg out of the members the forced start keeps,
// the members the group keeps but the new legs of a CREATED repair whose
// abort flag is set (architecture.md, Repair, Forced start; cnagent.md
// CN12), while a STARTED repair's new leg may be named whatever its flag
// says, since no abort takes a STARTED repair back; and so is the bad leg of
// a STARTED repair, which left the array before md began copying onto the
// new leg and which the finish retires. model.ForceStartGroup makes the same
// tests in its own STM (model.ReasonLegNotNamable).
func forceStartLegChecks(
	stm etcdutil.STM,
	sc *spScope,
	grp *pb.Group,
	legId uint64,
) error {
	rep, err := grpRepair(stm, sc, grp)
	if err != nil {
		return err
	}
	if grpLegOf(grp, legId) != nil {
		if rep.GetRepStage() == pb.RepStage_REP_STAGE_STARTED &&
			slices.Contains(rep.GetBadLegIdList(), legId) {
			return errPrecondition(
				"leg %d is the bad leg of the repair %q, which is "+
					"REP_STAGE_STARTED: the leg left the array before md "+
					"began copying onto the new leg, and the finish "+
					"retires it, so the forced start of group %d may not "+
					"name it",
				legId, grp.GetRepName(), grp.GetGrpId())
		}
		return nil
	}
	for _, leg := range rep.GetNewLegList() {
		if leg.GetLegId() != legId {
			continue
		}
		if rep.GetRepStage() != pb.RepStage_REP_STAGE_STARTED &&
			rep.GetTryToAbort() {
			return errPrecondition(
				"leg %d is the new leg of the repair %q, which is %s with "+
					"its abort flag set: the forced start of group %d "+
					"keeps no such member", legId,
				grp.GetRepName(), rep.GetRepStage(), grp.GetGrpId())
		}
		return nil
	}
	return errNotFound(
		"leg %d is neither in the leg_list of group %d nor a new leg of "+
			"its repair", legId, grp.GetGrpId())
}

// checkGrpArrayDown is ForceStartGroup's agent call between its two phases
// (AG1, AG3): a group whose row the primary's latest report reads OK has an
// array that runs, and a forced start is for an array md will not start by
// itself; the primary never stops a running array for one (cnagent.md CN12).
// GetCntlrInfo answers from the agent's latest observation and reads nothing
// of the node (architecture.md, Live-state reporting).
//
// The row is read whatever the reply's AgentReply code (AG3). An OK row
// refuses, and so does a report with no row of the group: an absent row, or
// a reply with no info, as the UnknownObject refusal of an agent that does
// not hold the cntlr carries (cnagent.md CN23). Silence about the group
// proves nothing about its array: the primary etcd names reports as a
// standby, with no group row, from a failover's hand-over until its
// promotion's reply, and a fresh primary of a replacement reports nothing
// yet, while the old primary's array may still run; the new primary's node
// holds no array of the group, so a mark would act there at once. A primary
// that has built its groups reports each group's row, so the caller only
// retries. A row in any other status lets the mark be written: the primary,
// which waits while its array runs, is the guard that sees the node. An
// agent that does not answer is FAILED_PRECONDITION, as the hydration checks
// answer one (AG3): its silence proves nothing about the array either.
func checkGrpArrayDown(
	ctx context.Context,
	tgt grpReportTarget,
	grpId uint64,
) error {
	runs, absent := false, false
	callErr := withCnAgent(ctx, tgt.AddrPort,
		func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
			reply, err := client.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
				ClusterId: tgt.ClusterId,
				CnId:      tgt.CnId,
				CntlrPointer: &pb.CntlrPointer{
					SpId:    tgt.SpId,
					CntlrId: tgt.CntlrId,
				},
			})
			if err != nil {
				return err
			}
			row, ok := reply.GetCntlrInfo().GetGrpIdToMdRaid()[grpId]
			absent = !ok || row == nil
			runs = row.GetStatus() == pb.ResStatus_RES_STATUS_OK
			return nil
		})
	if callErr != nil {
		return errPrecondition(
			"group %d: its array is not proven down, controller node %q "+
				"did not answer: %v", grpId, tgt.AddrPort, callErr)
	}
	if absent {
		return errPrecondition(
			"group %d: its array is not proven down, the primary's latest "+
				"report holds no row of the group", grpId)
	}
	if runs {
		return errPrecondition(
			"group %d runs: the primary's latest report reads its row "+
				"RES_STATUS_OK", grpId)
	}
	return nil
}

// UpdateGroupOperatorInCharge is the UpdateGroupOperatorInCharge of
// architecture.md, Repairs: the operator_in_charge flag of an md-raid1
// group, set to take the group's repair over from the sp worker and cleared
// to give it back.
//
// The pure checks come first (GW4): the names. The planning snapshot
// (operatorInChargeChecks) then makes the refusals of architecture.md,
// Repairs, in their order, behind openSp's resolution, deleting gate and
// token: a RedundNone pool, the group's look-up — slices first, then clones
// (locateGrp) — and a clone being deleted. No level gate is among them: the
// call takes the pool at every level, so that an operator sets the flag at a
// level where reactions stop before lowering it. model.SetGrpOperatorInCharge,
// the deciding STM, makes its own checks again, the token first (GW6), and
// answers a group whose flag already holds the request's value with no
// write. No write of the flag bumps SpRev, so a token the caller holds stays
// valid across the call.
func (s *Server) UpdateGroupOperatorInCharge(
	ctx context.Context,
	req *pb.UpdateGroupOperatorInChargeRequest,
) (*pb.UpdateGroupOperatorInChargeReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	var sc *spScope
	var loc grpLocation
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		var err error
		sc, loc, err = operatorInChargeChecks(stm, req)
		return err
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	err = model.SetGrpOperatorInCharge(
		ctx, s.cli, sc.Cid, sc.Shard(), sc.SpId(), req.GetSpName(),
		req.GetSpRev().GetRevision(), loc.parent(), req.GetGrpId(),
		req.GetOperatorInCharge(),
	)
	if err != nil {
		return nil, mapModelErr(err)
	}
	return &pb.UpdateGroupOperatorInChargeReply{GrpId: req.GetGrpId()}, nil
}

// operatorInChargeChecks is UpdateGroupOperatorInCharge's planning read
// inside the caller's snapshot, its refusals in the order of
// architecture.md, Repairs. It returns the scope and the group, whose parent
// the op is handed.
func operatorInChargeChecks(
	stm etcdutil.STM,
	req *pb.UpdateGroupOperatorInChargeRequest,
) (*spScope, grpLocation, error) {
	spName := req.GetSpName()
	sc, err := openSp(stm, req.GetClusterName(), spName, req.GetSpRev())
	if err != nil {
		return nil, grpLocation{}, err
	}
	// Redundancy is an SP-wide property the group only inherits, so the
	// test is the SP's STORED bdev_conf, as CreateRepair's is.
	if !isMdRaid1(spBdevConf(sc.Conf)) {
		return nil, grpLocation{}, errInvalid(
			"storage pool %q is RedundNone: its groups have no repair to "+
				"take over", spName)
	}
	loc, err := locateGrp(stm, sc, spName, req.GetGrpId())
	if err != nil {
		return nil, grpLocation{}, err
	}
	if err := refuseDeletingClone(loc); err != nil {
		return nil, grpLocation{}, err
	}
	return sc, loc, nil
}

// GetRepair is the GetRepair of architecture.md, Repairs: one snapshot reads
// the SpConf and the Repair it lists, so the two agree. It carries no token
// and has no deleting gate (openSpRead): a deleting pool's repairs are still
// read, until the drain deletes them. The reply is the record as stored, its
// stage and abort flag included, which is how an operator watches a repair
// move.
func (s *Server) GetRepair(
	ctx context.Context,
	req *pb.GetRepairRequest,
) (*pb.GetRepairReply, error) {
	if err := validateRepairRef(
		req.GetClusterName(), req.GetSpName(), req.GetRepName(),
	); err != nil {
		return nil, err
	}
	var rep *pb.Repair
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		rep, err = loadListedRepair(stm, sc, req.GetRepName())
		return err
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.GetRepairReply{RepName: req.GetRepName(), Repair: rep}, nil
}

// ListRepairs is the ListRepairs of architecture.md, Repairs: one snapshot
// reads every Repair repair_name_list names, as ListThinDevices reads its
// devices. The list is bounded by the pool's groups, one repair each
// (architecture.md, Cardinality limits), so it is not paged.
func (s *Server) ListRepairs(
	ctx context.Context,
	req *pb.ListRepairsRequest,
) (*pb.ListRepairsReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	var nameToRepair map[string]*pb.Repair
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		nameToRepair = nil
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		names := sc.Conf.GetRepairNameList()
		listed := make(map[string]*pb.Repair, len(names))
		for _, name := range names {
			rep, err := loadListedRepair(stm, sc, name)
			if err != nil {
				return err
			}
			listed[name] = rep
		}
		nameToRepair = listed
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.ListRepairsReply{NameToRepair: nameToRepair}, nil
}

// ListRetiredLegs is the ListRetiredLegs of architecture.md, Retired legs:
// ListRepairs over retired_leg_id_list, bounded by MaxRetiredLegPerGrp per
// group.
func (s *Server) ListRetiredLegs(
	ctx context.Context,
	req *pb.ListRetiredLegsRequest,
) (*pb.ListRetiredLegsReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	var legIdToRetired map[uint64]*pb.RetiredLeg
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		legIdToRetired = nil
		sc, err := openSpRead(stm, req.GetClusterName(), req.GetSpName())
		if err != nil {
			return err
		}
		ids := sc.Conf.GetRetiredLegIdList()
		listed := make(map[uint64]*pb.RetiredLeg, len(ids))
		for _, legId := range ids {
			retired, err := loadListedRetiredLeg(stm, sc, legId)
			if err != nil {
				return err
			}
			listed[legId] = retired
		}
		legIdToRetired = listed
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	return &pb.ListRetiredLegsReply{LegIdToRetiredLeg: legIdToRetired}, nil
}

// DeleteRetiredLeg is the DeleteRetiredLeg of architecture.md, Retired legs:
// the retired leg goes and its side gives its DN back what it took, at every
// sp_level. The shape is CancelRepair's: model.DeleteRetiredLeg, the op the sp
// worker's retire timer runs too, releases the side through the DN ledger of
// the drains, bumping the DN's DnRev, and answers an id the pool does not
// list with model.ReasonRetiredLegNotFound, NOT_FOUND here.
func (s *Server) DeleteRetiredLeg(
	ctx context.Context,
	req *pb.DeleteRetiredLegRequest,
) (*pb.DeleteRetiredLegReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	sc, err := s.repairOpenSp(
		ctx, req.GetClusterName(), req.GetSpName(), req.GetSpRev())
	if err != nil {
		return nil, err
	}
	if err := releaseConfGate(sc); err != nil {
		return nil, err
	}
	err = model.DeleteRetiredLeg(
		ctx, s.cli, sc.Cid, sc.Shard(), sc.SpId(), req.GetSpName(),
		req.GetSpRev().GetRevision(), req.GetLegId(), sc.Cc,
	)
	if modelReason(err) == model.ReasonRetiredLegNotFound {
		return nil, errRetiredLegNotFound(req.GetLegId())
	}
	if err != nil {
		return nil, mapModelErr(err)
	}
	return &pb.DeleteRetiredLegReply{LegId: req.GetLegId()}, nil
}

// validateRepairRef is the pure check of a request that addresses an
// existing repair by name: the cluster and pool names and rep_name by the
// common name rule (architecture.md, Common validation). The reserved prefix
// is CreateRepair's alone (validateRepName).
func validateRepairRef(clusterName, spName, repName string) error {
	if err := validateOptionalName("cluster_name", clusterName); err != nil {
		return err
	}
	if err := validateName("sp_name", spName); err != nil {
		return err
	}
	return validateName("rep_name", repName)
}
