package gateway

import (
	"context"
	"slices"

	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
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
// group's retired legs, the first refusal no override passes that
// ForceStartGroup would answer from its planning read for the dry run's
// leg, nil when it has none, and its refusal of a partial leg, which an
// override passes, its text whole as ForceStartGroup answers it, nil when
// the leg carries no partial mark.
type inspectRecords struct {
	tgt          grpReportTarget
	spRevision   uint64
	legIds       []uint64
	legs         map[uint64]*pb.Leg
	retired      []uint64
	startRefusal error
	partial      *pb.ForcedStartRefusal
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
// keeping the first for the dry run instead of failing the call, and the
// refusal of a partial leg, which an override passes, for the dry run's
// list (inspectDryRun). The pure checks before the snapshot are the names
// and the overrides, as ForceStartGroup's (validateOverrideList), which only
// a dry run reads, so a list with no leg is INVALID_ARGUMENT. Then two
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
	if err := validateOverrideList(
		"override_list", req.GetOverrideList(),
	); err != nil {
		return nil, err
	}
	if req.GetLegId() == 0 && len(req.GetOverrideList()) > 0 {
		return nil, errInvalid("override_list is set with no leg_id: " +
			"only the dry run reads it")
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
				ClusterId:    rec.tgt.ClusterId,
				CnId:         rec.tgt.CnId,
				SpId:         rec.tgt.SpId,
				CntlrId:      rec.tgt.CntlrId,
				GrpId:        req.GetGrpId(),
				LegId:        req.GetLegId(),
				OverrideList: req.GetOverrideList(),
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
	reply.DryRun = inspectDryRun(rec, agentReply.GetDryRun(),
		grpRowRefusal(info, req.GetGrpId()))
	return reply, nil
}

// inspectDryRun is InspectGroup's dry run from the agent's (agentDry) and
// the gateway's own refusals: the list of refusals an override passes holds
// the gateway's partial mark first, its text cut to the row text cap as
// the agent cuts its own (common.CutRowText), then the agent's list, as the
// call meets the partial mark before it writes a mark and the agent the
// rest after. The first refusal ForceStartGroup would answer, in its order — a
// refusal of its planning read that no override passes, a partial mark the
// request does not pass, the group's row in the report (rowRefusal) — is
// the dry run's refusal, whole, word for word the call's, with no zeroes,
// update or stop beside it, since the call would write no mark, so nothing
// would be stopped; the agent's dry run stands otherwise.
func inspectDryRun(
	rec inspectRecords,
	agentDry *pb.ForcedStartDryRun,
	rowRefusal error,
) *pb.ForcedStartDryRun {
	list := agentDry.GetRefusalList()
	if rec.partial != nil {
		list = append([]*pb.ForcedStartRefusal{{
			Override:   rec.partial.GetOverride(),
			Text:       common.CutRowText(rec.partial.GetText()),
			Overridden: rec.partial.GetOverridden(),
		}}, list...)
	}
	refusal := ""
	switch {
	case rec.startRefusal != nil:
		refusal = status.Convert(rec.startRefusal).Message()
	case rec.partial != nil && !rec.partial.GetOverridden():
		refusal = rec.partial.GetText()
	case rowRefusal != nil:
		refusal = status.Convert(rowRefusal).Message()
	}
	if refusal != "" {
		return &pb.ForcedStartDryRun{Refusal: refusal, RefusalList: list}
	}
	return &pb.ForcedStartDryRun{
		Refusal:       agentDry.GetRefusal(),
		ZeroLegIdList: agentDry.GetZeroLegIdList(),
		Update:        agentDry.GetUpdate(),
		StopsArray:    agentDry.GetStopsArray(),
		RefusalList:   list,
	}
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
		rec.partial, rec.startRefusal = forceStartRefusal(stm, sc, loc, req)
	}
	rec.tgt, err = grpPrimaryTarget(stm, sc, spName, req.GetGrpId(),
		"whose agent could read the members of group %d")
	if err != nil {
		return inspectRecords{}, err
	}
	return rec, nil
}

// forceStartRefusal is the first refusal no override passes that
// ForceStartGroup's planning read (forceStartChecks) would make of a write
// naming the request's leg, after the resolution InspectGroup has made: a
// pool being deleted, the level that builds no group array, a clone being
// deleted, and the leg (forceStartLegChecks), nil when it makes none; and,
// once the leg's checks held, its refusal of a leg that carries the partial
// mark (errPartialLeg), which an override passes, as the dry run lists it,
// nil when the leg carries none.
func forceStartRefusal(
	stm etcdutil.STM,
	sc *spScope,
	loc grpLocation,
	req *pb.InspectGroupRequest,
) (*pb.ForcedStartRefusal, error) {
	spName := req.GetSpName()
	if sc.Conf.GetDeleting() {
		return nil, errPrecondition("storage pool %q is being deleted", spName)
	}
	if level := sc.Conf.GetSpLevel(); level >= pb.SpLevel_SP_LEVEL_NO_REDUND {
		return nil, errPrecondition(
			"storage pool %q is at %s, where no group array is built",
			spName, level)
	}
	if err := refuseDeletingClone(loc); err != nil {
		return nil, err
	}
	partial, err := forceStartLegChecks(stm, sc, loc.Grp, req.GetLegId())
	if err != nil || !partial {
		return nil, err
	}
	return &pb.ForcedStartRefusal{
		Override: pb.ForcedStartOverride_FORCED_START_OVERRIDE_PARTIAL,
		Text: status.Convert(
			errPartialLeg(req.GetLegId(), loc.Grp.GetRepName())).Message(),
		Overridden: slices.Contains(req.GetOverrideList(),
			pb.ForcedStartOverride_FORCED_START_OVERRIDE_PARTIAL),
	}, nil
}
