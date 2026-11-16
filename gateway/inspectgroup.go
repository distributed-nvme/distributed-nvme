package gateway

import (
	"context"
	"slices"

	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// opInspectGroup names InspectGroup in its errors.
const opInspectGroup = "InspectGroup"

// notInRequest is the read error of a leg the pool's records hold and the
// primary's held request does not, so the agent read nothing of it.
const notInRequest = "not read: the primary's held request does not name " +
	"the leg yet"

// inspectRecords is what InspectGroup's snapshot reads of a group: the
// primary to ask, the pool's revision, each leg's record, in order, the
// group's retired legs, and the first refusal ForceStartGroup would answer
// from its planning read for the dry run's leg, nil when it has none.
type inspectRecords struct {
	tgt          grpReportTarget
	spRevision   uint64
	legIds       []uint64
	legs         map[uint64]*pb.Leg
	retired      []uint64
	startRefusal error
}

// InspectGroup is the InspectGroup of architecture.md, Repairs: a read-only
// look at one md-raid1 group's members through its primary, so an operator
// can choose the leg of a forced start. One snapshot resolves the pool with
// no token and no deleting gate (openSpRead), refuses a RedundNone pool and
// finds the group as ForceStartGroup does (locateGrp), reads its legs, its
// open repair's new legs, its retired legs, the pool's revision and its
// primary (grpPrimaryTarget); with a leg in the request it makes the
// refusals ForceStartGroup's planning read would make of a write from that
// leg — a pool being deleted, the level, a clone being deleted, the leg —
// keeping the first for the dry run instead of failing the call. Then two
// calls to the primary's agent: GetCntlrInfo, for each leg's row, the
// report's stamp and the revision of the request the agent holds, whose
// group row ForceStartGroup's check of the array judges too
// (grpRowRefusal); and InspectGrp, for each member's superblock read, md's
// view of it and, with a leg, the agent's dry run. A transport failure or
// an agent's error is ABORTED, as InspectCntlr's is. Nothing is written.
func (s *Server) InspectGroup(
	ctx context.Context,
	req *pb.InspectGroupRequest,
) (*pb.InspectGroupReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("sp_name", req.GetSpName()); err != nil {
		return nil, err
	}
	var rec inspectRecords
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		var err error
		rec, err = inspectGroupChecks(stm, req)
		return err
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	var cntlrInfo *pb.GetCntlrInfoReply
	var agentReply *pb.InspectGrpReply
	err = withCnAgent(ctx, rec.tgt.AddrPort,
		func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
			var err error
			cntlrInfo, err = client.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
				ClusterId: rec.tgt.ClusterId,
				CnId:      rec.tgt.CnId,
				CntlrPointer: &pb.CntlrPointer{
					SpId:    rec.tgt.SpId,
					CntlrId: rec.tgt.CntlrId,
				},
			})
			if err != nil {
				return err
			}
			agentReply, err = client.InspectGrp(ctx, &pb.InspectGrpRequest{
				ClusterId: rec.tgt.ClusterId,
				CnId:      rec.tgt.CnId,
				SpId:      rec.tgt.SpId,
				CntlrId:   rec.tgt.CntlrId,
				GrpId:     req.GetGrpId(),
				LegId:     req.GetLegId(),
				Force:     req.GetForce(),
			})
			return err
		})
	if err != nil {
		return nil, errAborted("%s: controller node %q: %v",
			opInspectGroup, rec.tgt.AddrPort, err)
	}
	info := cntlrInfo.GetCntlrInfo()
	reply := &pb.InspectGroupReply{
		GrpId:            req.GetGrpId(),
		CntlrId:          rec.tgt.CntlrId,
		ArrayState:       agentReply.GetArrayState(),
		RetiredLegIdList: rec.retired,
		DryRun:           agentReply.GetDryRun(),
		ObsStamp:         cntlrInfo.GetObsStamp(),
		AppliedRevision:  cntlrInfo.GetRevision(),
		SpRevision:       rec.spRevision,
	}
	members := make(map[uint64]*pb.InspectedMember)
	for _, member := range agentReply.GetMemberList() {
		members[member.GetLegId()] = member
	}
	for _, legId := range rec.legIds {
		member := members[legId]
		if member == nil {
			member = &pb.InspectedMember{
				LegId: legId, ReadError: notInRequest}
		}
		leg := &pb.InspectedLeg{
			Member: member,
			Row:    info.GetLegIdToLeg()[legId],
		}
		record := rec.legs[legId]
		leg.LegErrEpoch = record.GetErrEpoch()
		if sides := record.GetSideList(); len(sides) > 0 {
			leg.AddrPort = sides[0].GetAddrPort()
			leg.SideErrEpoch = sides[0].GetErrEpoch()
		}
		reply.LegList = append(reply.LegList, leg)
	}
	if req.GetLegId() == 0 {
		return reply, nil
	}
	refusal := rec.startRefusal
	if refusal == nil {
		refusal = grpRowRefusal(info, req.GetGrpId())
	}
	if refusal != nil {
		// ForceStartGroup would write no mark, so nothing would be stopped.
		reply.DryRun = &pb.ForcedStartDryRun{
			Refusal: status.Convert(refusal).Message(),
		}
	}
	return reply, nil
}

// inspectGroupChecks is InspectGroup's snapshot read (InspectGroup).
func inspectGroupChecks(
	stm etcdutil.STM,
	req *pb.InspectGroupRequest,
) (inspectRecords, error) {
	spName := req.GetSpName()
	sc, err := openSpRead(stm, req.GetClusterName(), spName)
	if err != nil {
		return inspectRecords{}, err
	}
	if !isMdRaid1(spBdevConf(sc.Conf)) {
		return inspectRecords{}, errInvalid(
			"storage pool %q is RedundNone: its groups have no array to "+
				"inspect", spName)
	}
	loc, err := locateGrp(stm, sc, spName, req.GetGrpId())
	if err != nil {
		return inspectRecords{}, err
	}
	grp := loc.Grp
	rec := inspectRecords{
		legs:    make(map[uint64]*pb.Leg),
		retired: slices.Clone(grp.GetRetiredLegIdList()),
	}
	revKey := model.SpRevKey(sc.Shard(), sc.Cid, sc.SpId())
	spRev := &pb.SpRev{}
	if stm.Get(revKey, spRev) {
		rec.spRevision = spRev.GetRevision()
	}
	for _, leg := range grp.GetLegList() {
		rec.legIds = append(rec.legIds, leg.GetLegId())
		rec.legs[leg.GetLegId()] = leg
	}
	rep, err := grpRepair(stm, sc, grp)
	if err != nil {
		return inspectRecords{}, err
	}
	switch rep.GetRepStage() {
	case pb.RepStage_REP_STAGE_CREATED, pb.RepStage_REP_STAGE_STARTED:
		for _, leg := range rep.GetNewLegList() {
			rec.legIds = append(rec.legIds, leg.GetLegId())
			rec.legs[leg.GetLegId()] = leg
		}
	}
	if req.GetLegId() != 0 {
		rec.startRefusal = forceStartRefusal(stm, sc, loc, req)
	}
	rec.tgt, err = grpPrimaryTarget(stm, sc, spName, req.GetGrpId(),
		"whose agent could read the members of group %d")
	if err != nil {
		return inspectRecords{}, err
	}
	return rec, nil
}

// forceStartRefusal is the first refusal ForceStartGroup's planning read
// (forceStartChecks) would make of a write naming the request's leg with
// its force, after the resolution InspectGroup has made: a pool being
// deleted, the level that builds no group array, a clone being deleted, and
// the leg (forceStartLegChecks); nil when it makes none.
func forceStartRefusal(
	stm etcdutil.STM,
	sc *spScope,
	loc grpLocation,
	req *pb.InspectGroupRequest,
) error {
	spName := req.GetSpName()
	if sc.Conf.GetDeleting() {
		return errPrecondition("storage pool %q is being deleted", spName)
	}
	if level := sc.Conf.GetSpLevel(); level >= pb.SpLevel_SP_LEVEL_NO_REDUND {
		return errPrecondition(
			"storage pool %q is at %s, where no group array is built",
			spName, level)
	}
	if err := refuseDeletingClone(loc); err != nil {
		return err
	}
	return forceStartLegChecks(
		stm, sc, loc.Grp, req.GetLegId(), req.GetForce())
}
