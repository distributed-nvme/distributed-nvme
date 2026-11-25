package cnagent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The member kinds InspectGrp names (InspectedMember.kind).
const (
	inspectKindLeg    = "leg"
	inspectKindBadLeg = "bad leg"
	inspectKindNewLeg = "new leg"
)

// startingErr is the error of a call whose reply carries no code — a bitmap
// read, the inspect call — while the startup pass runs (dnagent.md SH33):
// Unavailable, so the caller tries again later; nil once the pass ended.
func (s *CnAgentServer) startingErr() error {
	if !s.startup.Starting() {
		return nil
	}
	return status.Error(codes.Unavailable,
		"the agent's startup pass still runs")
}

// cntlrIoStats is what a GetCntlrInfo that asks for it reads beside the
// observation (CN23's exception, which only InspectCntlr's call takes): the
// counters of each leg's wrapper and each namespace's ns-dev, read now from
// sysfs (Dm.DevIoStats), and each leg's probe times, from its prober's
// memory (CN11). It takes no lock of CN1, starts no read and touches no
// observation, so no check round and no syncup reply carries what it reads.
// The devices are those the held request's plan names, as far as its level
// keeps them: a provisioning leg has no wrapper, so it is not read, and a
// deferred namespace's ns-dev stands on the td's dm-error and serves no IO,
// so it is not read either; a device that is not there reads as an error
// in its entry, never as zero counters. A request the conf gate refuses
// (CN8) plans nothing and reads nothing. The probe ages count back from
// answered, the answer time the reply's stamp carries (pb.LegProbeTimes).
func (s *CnAgentServer) cntlrIoStats(
	ctx context.Context,
	st *cntlrState,
	answered time.Time,
) *pb.CntlrIoStats {
	out := &pb.CntlrIoStats{
		LegIdToIo: make(map[uint64]*pb.LegIoStats),
		NsIdToIo:  make(map[uint64]*pb.DevIoStats),
	}
	req := st.loadReq()
	if validateCntlrConf(req) != nil {
		return out
	}
	plan := newCntlrPlan(s.nf, req)
	s.mu.Lock()
	probers := make(map[uint64]*legProber, len(st.probers))
	for legId, prober := range st.probers {
		probers[legId] = prober
	}
	s.mu.Unlock()
	if plan.wantLeg {
		for _, lp := range plan.legs {
			if lp.provisioning {
				continue
			}
			io := &pb.LegIoStats{Dev: s.dm.DevIoStats(ctx, lp.path)}
			if prober := probers[lp.legId]; prober != nil {
				io.ProbeTimes = prober.probeTimes(answered)
			}
			out.LegIdToIo[lp.legId] = io
		}
	}
	if plan.wantAny {
		for _, np := range plan.namespaces {
			if np.deferred {
				continue
			}
			out.NsIdToIo[np.nsId] = s.dm.DevIoStats(ctx,
				s.nf.DmPath(np.devName))
		}
	}
	return out
}

// InspectGrp is cnagent.md CN31, the inspect of one md-raid1 group on the
// primary: for every member the group keeps — its leg_list legs and the new
// legs of its open repair — whether the leg serves, its superblock read
// (Md.ReadMember, the read every md decision of CN12 takes) and md's view of
// it in the group's array that is there; and, with a leg in the request,
// what CN12's forced start from that leg would do (planForcedStart), with no
// mdadm command and no write.
//
// It holds the node read lock and the cntlr's object lock, as GetLegBm
// does, only while it reads the cntlr's plan, the array and which legs
// serve; the superblock reads and the dry run's run after the locks are
// let go, in a goroutine the call waits for until its own deadline. Reason:
// a read of a member whose disk node stalls can block without limit, and a
// lock held through it would wedge every converge and read of the node
// behind it; a read that outlives the call ends on its own, its answer
// dropped. A converge between the two halves can change the node, so the
// reply is a look at the node at about the time of the call, as every read
// is. It reads and reports nothing else: the gateway adds the records' and
// the report's view (gateway.md, Repairs and retired legs).
func (s *CnAgentServer) InspectGrp(
	ctx context.Context,
	req *pb.InspectGrpRequest,
) (*pb.InspectGrpReply, error) {
	if err := s.startingErr(); err != nil {
		return nil, err
	}
	look, err := s.inspectLook(ctx, req)
	if err != nil {
		return nil, err
	}
	done := make(chan *pb.InspectGrpReply, 1)
	go func() {
		done <- s.inspectRead(ctx, look, req.GetLegId(),
			req.GetOverrideList())
	}()
	select {
	case reply := <-done:
		return reply, nil
	case <-ctx.Done():
		return nil, status.Errorf(codes.DeadlineExceeded,
			"reading the members of group %d did not end: %v",
			req.GetGrpId(), ctx.Err())
	}
}

// inspectLook is what InspectGrp reads under the locks: the cntlr's plan of
// the group, the group's array, the error of a read of the array that
// failed, and the legs that serve.
type inspectLook struct {
	plan      *cntlrPlan
	gp        *grpPlan
	detail    *MdDetail
	detailErr error
	serves    map[uint64]bool
}

// inspectLook takes InspectGrp's locked half (InspectGrp).
func (s *CnAgentServer) inspectLook(
	ctx context.Context,
	req *pb.InspectGrpRequest,
) (*inspectLook, error) {
	s.locks.Node().RLock()
	defer s.locks.Node().RUnlock()
	key := cntlrKey(req.GetClusterId(), req.GetCnId(),
		req.GetSpId(), req.GetCntlrId())
	if s.getCntlr(key) == nil {
		return nil, status.Errorf(codes.Internal,
			"unknown cntlr (sp %d, cntlr %d)",
			req.GetSpId(), req.GetCntlrId())
	}
	objLock := s.locks.Obj(key)
	objLock.Lock()
	defer objLock.Unlock()

	plan, err := s.primaryPlanFor(req.GetClusterId(), req.GetCnId(),
		req.GetSpId(), req.GetCntlrId())
	if err != nil {
		return nil, err
	}
	gp := plan.grpById[req.GetGrpId()]
	if gp == nil {
		return nil, status.Errorf(codes.Internal,
			"unknown group %d", req.GetGrpId())
	}
	if !gp.raid1 {
		return nil, status.Errorf(codes.Internal,
			"group %d is not md-raid1: it has no array", gp.grpId)
	}
	look := &inspectLook{
		plan:   plan,
		gp:     gp,
		detail: &MdDetail{},
		serves: make(map[uint64]bool),
	}
	plan.walk = s.newPassWalk(ctx)
	plan.mdWalks = &mdWalkOnce{md: s.md}
	walk, err := plan.mdWalks.get(ctx)
	if err == nil {
		look.detail, err = s.md.Detail(ctx, walk, gp.memberNames())
	}
	if err != nil {
		// The members are read all the same: their superblocks do not
		// depend on the array, which only an operator may have to sort out.
		look.detail, look.detailErr = &MdDetail{}, err
	}
	for _, lp := range inspectedLegs(gp) {
		look.serves[lp.legId] = !lp.provisioning && s.legServes(ctx, plan, lp)
	}
	return look, nil
}

// inspectedLegs are the members InspectGrp reads: the group's leg_list
// legs, then the new legs of its repair while the repair is open; an
// ABORTED repair's new legs are no members of the group.
func inspectedLegs(gp *grpPlan) []*legPlan {
	legs := append([]*legPlan(nil), gp.legs...)
	if gp.rep != nil && gp.rep.open() {
		legs = append(legs, gp.rep.newLegs...)
	}
	return legs
}

// inspectRead is InspectGrp's unlocked half: each member's read and, with a
// leg, the dry run.
func (s *CnAgentServer) inspectRead(
	ctx context.Context,
	look *inspectLook,
	legId uint64,
	overrides []pb.ForcedStartOverride,
) *pb.InspectGrpReply {
	reply := &pb.InspectGrpReply{}
	var held map[string]MdMember
	switch {
	case look.detailErr != nil:
		reply.ArrayState = "not read: " + look.detailErr.Error()
	case look.detail.Exists:
		reply.ArrayState = look.detail.State
		held = heldMembers(look.detail)
	}
	for _, lp := range inspectedLegs(look.gp) {
		reply.MemberList = append(reply.MemberList,
			s.inspectMember(ctx, look, lp, held))
	}
	if legId != 0 {
		reply.DryRun = s.forcedStartDryRun(ctx, look, legId, overrides)
	}
	return reply
}

// inspectMember is one member of InspectGrp: whether its leg serves
// (legServes, the read path's test, which builds nothing), its superblock
// read when it does, and md's state and slot for it in the array held.
func (s *CnAgentServer) inspectMember(
	ctx context.Context,
	look *inspectLook,
	lp *legPlan,
	held map[string]MdMember,
) *pb.InspectedMember {
	gp := look.gp
	member := &pb.InspectedMember{LegId: lp.legId, Kind: inspectKindLeg}
	switch {
	case !slices.Contains(gp.legs, lp):
		member.Kind = inspectKindNewLeg
	case gp.isBadLeg(lp.legId):
		member.Kind = inspectKindBadLeg
	}
	if md, ok := held[lp.name]; ok {
		member.MdState = md.State
		member.MdSlot = md.Slot
	}
	switch {
	case lp.provisioning:
		member.ReadError = "not read: the leg is still provisioning"
		return member
	case !look.serves[lp.legId]:
		member.ReadError = "not read: the leg does not serve"
		return member
	}
	member.Available = true
	reading := s.md.ReadMember(ctx, lp.path)
	if reading.Err != nil {
		member.ReadError = reading.Err.Error()
		return member
	}
	if sb := reading.Sb; sb != nil {
		member.HasSuperblock = true
		member.Uuid = sb.Uuid
		member.Events = sb.Events
		member.Role = mdRoleText(sb.Role)
		member.Recovering = sb.Recovering
		member.RecoveryOffset = sb.RecoveryOffset
		member.BadBlocks = uint32(sb.BadBlocks)
	}
	return member
}

// forcedStartDryRun is InspectGrp's dry run: what CN12's forced start would
// do from a mark naming legId and passing the kinds of refusal overrides
// names, from the same checks the real start runs (forcedStartPass,
// planForcedStart) on a mark of the call's own, which no request carries
// and nothing keeps. An array of the group that runs is the refusal, since
// the primary leaves a mark waiting while it runs (ensureGroup); one
// that is there and does not run is stopped first (forcedStartPass), unless
// it holds a member that is no leg wrapper of this pool, which the real
// start refuses to stop (MdDetail.strangerErr), or a device holds it open
// (Md.Holders), which makes the real start's stop fail and the start wait;
// a read of the array that failed is the refusal, as it is the real start's
// error (ensureGroup).
// Every refusal an override passes that the checks met is listed, passed or
// not, so one dry run shows them all (planForcedStart). Every refusal and
// each listed text is cut to the row text cap as the real start's row is
// (agent.CutAgentText), so the dry run's refusal and the row read the same.
func (s *CnAgentServer) forcedStartDryRun(
	ctx context.Context,
	look *inspectLook,
	legId uint64,
	overrides []pb.ForcedStartOverride,
) *pb.ForcedStartDryRun {
	gp, detail, plan := look.gp, look.detail, look.plan
	dry := &pb.ForcedStartDryRun{}
	switch {
	case look.detailErr != nil:
		dry.Refusal = agent.CutAgentText(look.detailErr.Error())
		return dry
	case detail.Exists && mdRunning(detail.State):
		dry.Refusal = agent.CutAgentText(fmt.Sprintf("the array of group "+
			"%#x runs (%s): the primary leaves a mark waiting while it "+
			"runs", gp.grpId, detail.State))
		return dry
	case detail.Exists:
		if err := detail.strangerErr(
			plan.clusterId, plan.cnId, plan.spId); err != nil {
			dry.Refusal = agent.CutAgentText(err.Error())
			return dry
		}
		dry.StopsArray = true
		// The real start's stop is refused while a device holds the array
		// open, as the pool's layers above do at a level that builds thin
		// pools, and the start then waits; the dry run reads the holders as
		// the re-assembly does (reassemblyRefusal) and says so.
		holders, err := s.md.Holders(ctx, detail.Dev)
		if err != nil {
			dry.Refusal = agent.CutAgentText(fmt.Sprintf(
				"the array of group %#x: its holders could not be read, so "+
					"whether its stop would be refused is not known: %v",
				gp.grpId, err))
			return dry
		}
		if len(holders) > 0 {
			// The step that frees the array: at a level that builds thin
			// pools they hold it, and below that level only a device dnv did
			// not remove, or one outside dnv, can.
			step := "step the pool's level down to NO_THINPOOL first"
			if !plan.levelPool {
				step = "remove that device first, which dnv did not remove " +
					"or did not make"
			}
			dry.Refusal = agent.CutAgentText(fmt.Sprintf(
				"the array of group %#x is held open by %s, so its stop "+
					"would be refused and the start would wait: %s",
				gp.grpId, strings.Join(holders, ", "), step))
			return dry
		}
	}
	gp.mark = &pb.ForcedStart{LegId: legId, OverrideList: overrides}
	fp, refusals, err := s.planForcedStart(ctx, gp, look.serves)
	for _, r := range refusals {
		dry.RefusalList = append(dry.RefusalList, &pb.ForcedStartRefusal{
			Override:   r.override,
			Text:       agent.CutAgentText(r.text),
			Overridden: forcedStartPasses(gp, r.override),
		})
	}
	if err != nil {
		// The real start stops the array before these checks
		// (forcedStartPass), so stops_array stands beside the refusal.
		dry.Refusal = agent.CutAgentText(err.Error())
		return dry
	}
	for _, lp := range fp.zero {
		dry.ZeroLegIdList = append(dry.ZeroLegIdList, lp.legId)
	}
	dry.Update = fp.update()
	return dry
}
