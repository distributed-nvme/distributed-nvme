package gateway

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// This file is architecture.md, Controller nodes, CheckControllerNodeRestore
// / gateway.md, Controller nodes: the restore check, a read that compares
// the desired state one controller node's agent holds with the restored
// etcd, pool by pool, after an etcd restore and before the agents' stores
// are reset (architecture.md, etcd backup and restore). It writes nothing
// and takes no token.

// CheckControllerNodeRestore is the CheckControllerNodeRestore of
// architecture.md, Controller nodes.
//
// A snapshot resolves the cluster and reads the restored node at addr_port,
// which may be absent: a node added after the backup is not in the restored
// state, and its agent is asked all the same. Then, outside any STM (AG1),
// one GetCnStore asks the agent for every controller node of the cluster it
// holds (heldCnStore); an agent that does not answer, still starts or
// answers any code but zero and the unknown object is FAILED_PRECONDITION
// (AG3), and the unknown object of an agent that holds no node of the
// cluster reads as no node held, so the check answers OK with every
// restored pointer NOT_HELD, or RECORD_LOST where the restored pool's
// records are lost. Then each pool is read in a snapshot of its own, and
// the thin devices of a pool whose held revision is above the restored one
// in one range of their prefix outside any STM: during a restore nothing
// writes, and one transaction over every pool of a node would hold hundreds
// of thousands of reads (architecture.md, STM discipline). A pool whose
// records are lost gives its row, never the answer's error.
func (s *Server) CheckControllerNodeRestore(
	ctx context.Context,
	req *pb.CheckControllerNodeRestoreRequest,
) (*pb.CheckControllerNodeRestoreReply, error) {
	if err := validateOptionalName(
		"cluster_name", req.GetClusterName(),
	); err != nil {
		return nil, err
	}
	if err := validateName("addr_port", req.GetAddrPort()); err != nil {
		return nil, err
	}
	var cid uint64
	var restoredCn *pb.CnConf
	var restoredRev uint64
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		cid, restoredCn, restoredRev = 0, nil, 0
		clusterId, _, err := resolveCluster(stm, req.GetClusterName())
		if err != nil {
			return err
		}
		cid = clusterId
		cn := &pb.CnConf{}
		if !stm.Get(model.CnConfKey(clusterId, req.GetAddrPort()), cn) {
			return nil
		}
		rev, err := checkCnToken(stm, clusterId, cn, nil)
		if err != nil {
			return err
		}
		restoredCn, restoredRev = cn, rev.GetRevision()
		return nil
	})
	if err != nil {
		return nil, mapStmErr(err)
	}
	store, err := heldCnStore(ctx, req.GetAddrPort(), cid)
	if err != nil {
		return nil, err
	}
	reply := &pb.CheckControllerNodeRestoreReply{
		RestoredCnId:     restoredCn.GetCnId(),
		RestoredRevision: restoredRev,
	}
	restoredPtrs := map[[2]uint64]bool{}
	for _, ptr := range restoredCn.GetCntlrPtrList() {
		restoredPtrs[[2]uint64{ptr.GetSpId(), ptr.GetCntlrId()}] = true
	}
	heldPtrs := map[[2]uint64]bool{}
	for _, cn := range store.GetCnList() {
		reply.HeldCnList = append(reply.HeldCnList,
			&pb.CheckControllerNodeRestoreReply_HeldCn{
				CnId:     cn.GetCnId(),
				Revision: cn.GetRevision(),
			})
		loaded := map[[2]uint64]*pb.GetCnStoreReply_StoredCntlr{}
		for _, cntlr := range cn.GetCntlrList() {
			ptr := cntlr.GetCntlrPointer()
			loaded[[2]uint64{ptr.GetSpId(), ptr.GetCntlrId()}] = cntlr
		}
		inRestored := restoredCn != nil &&
			restoredCn.GetCnId() == cn.GetCnId()
		for _, ptr := range cn.GetCntlrPointerList() {
			id := [2]uint64{ptr.GetSpId(), ptr.GetCntlrId()}
			if inRestored {
				heldPtrs[id] = true
			}
			pool, err := s.judgeHeldCntlr(ctx, cid, cn.GetCnId(), ptr,
				loaded[id], inRestored && restoredPtrs[id])
			if err != nil {
				return nil, err
			}
			reply.PoolList = append(reply.PoolList, pool)
		}
	}
	for _, ptr := range restoredCn.GetCntlrPtrList() {
		id := [2]uint64{ptr.GetSpId(), ptr.GetCntlrId()}
		if heldPtrs[id] {
			continue
		}
		pool := &pb.CheckControllerNodeRestoreReply_Pool{
			SpId:                 ptr.GetSpId(),
			CntlrId:              ptr.GetCntlrId(),
			CnId:                 restoredCn.GetCnId(),
			Verdict:              pb.RestoreVerdict_RESTORE_VERDICT_NOT_HELD,
			CntlrInRestoredState: true,
		}
		restored, err := s.readRestoredPool(ctx, cid, ptr.GetSpId(),
			ptr.GetCntlrId(), 0)
		if err != nil {
			return nil, err
		}
		pool.SpName = restored.name
		pool.RestoredRevision = restored.revision
		// The restored node names a cntlr of a pool the restored state
		// names nowhere: a lost record too.
		if restored.lost || !restored.found {
			pool.Verdict = pb.RestoreVerdict_RESTORE_VERDICT_RECORD_LOST
		}
		reply.PoolList = append(reply.PoolList, pool)
	}
	slices.SortFunc(reply.PoolList,
		func(a, b *pb.CheckControllerNodeRestoreReply_Pool) int {
			return cmp.Or(cmp.Compare(a.GetSpId(), b.GetSpId()),
				cmp.Compare(a.GetCntlrId(), b.GetCntlrId()),
				cmp.Compare(a.GetCnId(), b.GetCnId()))
		})
	return reply, nil
}

// heldCnStore is the restore check's agent call: GetCnStore for every
// controller node of the cluster the agent at addrPort holds. An agent that
// does not answer, or answers that it starts, or with any code but the
// unknown object of an agent that holds no node of the cluster, proves
// nothing about what the restore loses: FAILED_PRECONDITION (AG3), the
// agent's details cut to the row text cap (common.CutRowText).
func heldCnStore(
	ctx context.Context,
	addrPort string,
	cid uint64,
) (*pb.GetCnStoreReply, error) {
	var reply *pb.GetCnStoreReply
	callErr := withCnAgent(ctx, addrPort,
		func(ctx context.Context, client pb.ControllerNodeAgentClient) error {
			got, err := client.GetCnStore(ctx, &pb.GetCnStoreRequest{
				ClusterId: cid,
			})
			reply = got
			return err
		})
	if callErr != nil {
		return nil, errPrecondition(
			"controller node %q: its agent did not answer: %v", addrPort,
			callErr)
	}
	switch code := reply.GetAgentReply().GetCode(); code {
	case 0:
		return reply, nil
	case common.ReplyCodeUnknownObject:
		return &pb.GetCnStoreReply{}, nil
	case common.ReplyCodeStarting:
		return nil, errPrecondition(
			"controller node %q: its agent is starting; run the check "+
				"again once it has started", addrPort)
	default:
		return nil, errPrecondition(
			"controller node %q: its agent answers code %d: %s", addrPort,
			code, common.CutRowText(reply.GetAgentReply().GetDetails()))
	}
}

// restoredPool is what the restore check read of one pool of the restored
// state: found when the restored state names a pool of the sp_id, lost when
// a record of it is missing or does not decode, its name, SpConf, revision
// and, for a pool the agent holds at a newer revision, the largest side id
// and the count of the sides of its layout (layoutSides), whether the
// checked cntlr is the primary, the migrations whose cutover is pending and
// the namespaces with whether the cn agent's rule parks them.
type restoredPool struct {
	found        bool
	lost         bool
	name         string
	conf         *pb.SpConf
	revision     uint64
	maxSideId    uint64
	sideCnt      uint64
	primary      bool
	listed       bool
	pendingMigrs []string
	namespaces   map[uint64]restoredNs
}

// restoredNs is one namespace of a restored pool: its address for a reason's
// text and whether the restored state parks it.
type restoredNs struct {
	nqn    string
	nsIdx  uint32
	parked bool
}

// nsParked is the effective-suspend rule of cnagent.md CN16 on the records
// of a pool, the rule the cn agent parks a namespace by: suspended iff its
// flag says so or an auto_suspend transfer names it, unless an auto_resume
// clone fills its volume. clones are those the worker sends a cntlr, the
// ones not latched (dnv-worker.md CLD5).
func nsParked(
	nqn string,
	ns *pb.Namespace,
	xfers []*pb.Transfer,
	clones []*pb.Clone,
) bool {
	for _, clone := range clones {
		if clone.GetAutoResume() && clone.GetDstTdId() == ns.GetTdId() {
			return false
		}
	}
	if ns.GetSuspended() {
		return true
	}
	for _, xfer := range xfers {
		if xfer.GetAutoSuspend() && xfer.GetOriNqn() == nqn &&
			xfer.GetOriNsIdx() == ns.GetNsIdx() {
			return true
		}
	}
	return false
}

// readRestoredPool reads one pool of the restored state by its sp_id, in a
// snapshot of its own: the sp_id_to_name record, the SpConf, the SpRev and,
// when the restored revision is below heldRevision, every Slice of
// slice_id_list, every Clone of clone_name_list, every Repair of
// repair_name_list and the RetiredLeg of each retired leg a counted group
// lists, for the sides of its layout; the Cntlr of cntlrId, when
// cntlr_id_list names it, for its role (a cntlr the list does not name is
// no primary); every Migration of migr_name_list, for a pending cutover;
// and every Subsystem of nqn_list and Transfer of xfer_name_list, for the
// namespaces the cn agent's rule parks. A missing or undecodable record
// makes the pool lost, never an error; only a failure to reach etcd is one.
// The sides are counted over the groups the agent counts in the request it
// holds (cnagent.md CN32): the slices' meta and data groups and the meta
// groups of the clones that are not latched, since the worker sends a
// latched clone to no cntlr (dnv-worker.md CLD5).
func (s *Server) readRestoredPool(
	ctx context.Context,
	cid uint64,
	spId uint64,
	cntlrId uint64,
	heldRevision uint64,
) (restoredPool, error) {
	var pool restoredPool
	err := s.cli.Snapshot(ctx, func(stm etcdutil.STM) error {
		pool = restoredPool{}
		name := &pb.SpName{}
		if !stm.Get(model.SpNameKey(cid, spId), name) {
			return nil
		}
		pool.found = true
		pool.name = name.GetSpName()
		conf := &pb.SpConf{}
		if !stm.Get(model.SpConfKey(cid, name.GetSpName()), conf) ||
			conf.GetSpId() != spId {
			pool.lost = true
			return nil
		}
		pool.conf = conf
		rev := &pb.SpRev{}
		if !stm.Get(model.SpRevKey(conf.GetShardCode(), cid, spId), rev) {
			pool.lost = true
			return nil
		}
		pool.revision = rev.GetRevision()
		if rev.GetRevision() >= heldRevision {
			return nil
		}
		var grps []*pb.Group
		sliceGrps := map[[2]uint64]bool{}
		for _, sliceId := range conf.GetSliceIdList() {
			slice := &pb.Slice{}
			if !stm.Get(model.SliceKey(cid, spId, sliceId), slice) {
				pool.lost = true
				return nil
			}
			for _, grp := range append(append([]*pb.Group(nil),
				slice.GetMetaGrpList()...), slice.GetDataGrpList()...) {
				grps = append(grps, grp)
				sliceGrps[[2]uint64{sliceId, grp.GetGrpId()}] = true
			}
		}
		cloneGrps := map[string]uint64{}
		var sentClones []*pb.Clone
		for _, cloneName := range conf.GetCloneNameList() {
			clone := &pb.Clone{}
			if !stm.Get(model.CloneKey(cid, spId, cloneName), clone) {
				pool.lost = true
				return nil
			}
			if !clone.GetDeleting() {
				sentClones = append(sentClones, clone)
			}
			if !clone.GetDeleting() && clone.GetMetaGrp() != nil {
				grps = append(grps, clone.GetMetaGrp())
				cloneGrps[cloneName] = clone.GetMetaGrp().GetGrpId()
			}
		}
		if slices.Contains(conf.GetCntlrIdList(), cntlrId) {
			cntlr := &pb.Cntlr{}
			if !stm.Get(model.CntlrKey(cid, spId, cntlrId), cntlr) {
				pool.lost = true
				return nil
			}
			pool.listed = true
			pool.primary = cntlr.GetPrimary()
		}
		for _, migrName := range conf.GetMigrNameList() {
			migr := &pb.Migration{}
			if !stm.Get(model.MigrationKey(cid, spId, migrName), migr) {
				pool.lost = true
				return nil
			}
			if migr.GetCutoverPending() {
				pool.pendingMigrs = append(pool.pendingMigrs, migrName)
			}
		}
		var xfers []*pb.Transfer
		for _, xferName := range conf.GetXferNameList() {
			xfer := &pb.Transfer{}
			if !stm.Get(model.TransferKey(cid, spId, xferName), xfer) {
				pool.lost = true
				return nil
			}
			xfers = append(xfers, xfer)
		}
		pool.namespaces = map[uint64]restoredNs{}
		for _, nqn := range conf.GetNqnList() {
			subsystem := &pb.Subsystem{}
			if !stm.Get(model.SubsystemKey(cid, spId, nqn), subsystem) {
				pool.lost = true
				return nil
			}
			for _, ns := range subsystem.GetNsList() {
				pool.namespaces[ns.GetNsId()] = restoredNs{
					nqn:    nqn,
					nsIdx:  ns.GetNsIdx(),
					parked: nsParked(nqn, ns, xfers, sentClones),
				}
			}
		}
		// The side id of each retired leg those groups list, from its
		// RetiredLeg record, as the worker fills the request's
		// retired_leg_id_to_side_id; a missing record is one the worker
		// refuses to build the pool without (dnv-worker.md MD3).
		retiredSides := map[uint64]uint64{}
		for _, grp := range grps {
			for _, legId := range grp.GetRetiredLegIdList() {
				retired := &pb.RetiredLeg{}
				if !stm.Get(model.RetiredLegKey(cid, spId, legId),
					retired) {
					pool.lost = true
					return nil
				}
				retiredSides[legId] = retired.GetSide().GetSideId()
			}
		}
		// The repairs the worker sends a cntlr (cntlrRepairs in
		// worker/sprole.go): those whose parent group is a slice's or the
		// meta group of a clone that is not latched.
		var newLegs []*pb.Leg
		for _, repName := range conf.GetRepairNameList() {
			rep := &pb.Repair{}
			if !stm.Get(model.RepairKey(cid, spId, repName), rep) {
				pool.lost = true
				return nil
			}
			parented := false
			switch {
			case rep.GetSliceId() != 0 && rep.GetCloneName() == "":
				parented = sliceGrps[[2]uint64{
					rep.GetSliceId(), rep.GetGrpId()}]
			case rep.GetSliceId() == 0 && rep.GetCloneName() != "":
				grpId, ok := cloneGrps[rep.GetCloneName()]
				parented = ok && grpId == rep.GetGrpId()
			}
			if parented {
				newLegs = append(newLegs, rep.GetNewLegList()...)
			}
		}
		pool.maxSideId, pool.sideCnt = layoutSides(grps, newLegs,
			retiredSides)
		return nil
	})
	if err != nil && errors.Is(err, proto.Error) {
		// A record that does not decode: the STM's Get failed the
		// snapshot (dnv-worker.md EU6).
		return restoredPool{found: true, lost: true, name: pool.name}, nil
	}
	if err != nil {
		return restoredPool{}, mapStmErr(err)
	}
	return pool, nil
}

// judgeHeldCntlr is the restore check's row of one cntlr a held node names
// (architecture.md, Controller nodes, CheckControllerNodeRestore): the
// verdict from the held and the restored revision and, past the backup, the
// largest side ids and the counts of sides of the two layouts and the causes
// of breakReasons; and, for a pool whose held revision is above the restored
// one, the thin devices on each side the other lacks. held is nil for a pointer whose cntlr the
// agent holds no request of; inRestored says the restored node names the
// cntlr, so that a pool the restored state names nowhere is a lost record,
// not a pool made after the backup.
func (s *Server) judgeHeldCntlr(
	ctx context.Context,
	cid uint64,
	cnId uint64,
	ptr *pb.CntlrPointer,
	held *pb.GetCnStoreReply_StoredCntlr,
	inRestored bool,
) (*pb.CheckControllerNodeRestoreReply_Pool, error) {
	pool := &pb.CheckControllerNodeRestoreReply_Pool{
		SpId:                 ptr.GetSpId(),
		CntlrId:              ptr.GetCntlrId(),
		CnId:                 cnId,
		CntlrInRestoredState: inRestored,
	}
	if held != nil {
		pool.HeldRevision = held.GetRevision()
		pool.Primary = held.GetPrimary()
		pool.NqnList = held.GetNqnList()
		pool.HeldMaxSideId = held.GetMaxSideId()
		pool.HeldSideCnt = held.GetSideCnt()
		pool.HeldSideIdsIncomplete = held.GetRetiredSideIdMissing()
	}
	restored, err := s.readRestoredPool(ctx, cid, ptr.GetSpId(),
		ptr.GetCntlrId(), pool.GetHeldRevision())
	if err != nil {
		return nil, err
	}
	pool.SpName = restored.name
	pool.RestoredRevision = restored.revision
	switch {
	case restored.lost, !restored.found && inRestored:
		pool.Verdict = pb.RestoreVerdict_RESTORE_VERDICT_RECORD_LOST
		return pool, nil
	case held == nil:
		pool.Verdict = pb.RestoreVerdict_RESTORE_VERDICT_NOT_LOADED
		return pool, nil
	case !restored.found:
		pool.Verdict = pb.RestoreVerdict_RESTORE_VERDICT_NEW
		for _, td := range held.GetTdList() {
			pool.LostTdList = append(pool.LostTdList, lostTdOf(td, false))
		}
		return pool, nil
	case held.GetRevision() == restored.revision:
		pool.Verdict = pb.RestoreVerdict_RESTORE_VERDICT_SAME
		return pool, nil
	case held.GetRevision() < restored.revision:
		pool.Verdict = pb.RestoreVerdict_RESTORE_VERDICT_BEHIND
		return pool, nil
	}
	pool.RestoredMaxSideId = restored.maxSideId
	pool.RestoredSideCnt = restored.sideCnt
	// Every place a side sits is counted on both sides by its side id — the
	// groups' legs, their retired legs, the open repairs' new legs and the
	// meta groups of the clones that are not latched — so a repair's finish
	// and a take-back of a retired leg move a side inside the set, which
	// stands; a side removed, a give-back included, lowers the count unless
	// one is added; and an added side raises the largest id, since side ids
	// come from one counter of the pool that only grows. A held request
	// without a retired leg's side id, as one from a worker without
	// retired_leg_id_to_side_id carries, may miss an added side in its
	// largest id, so it may break. Three changes keep the set of sides and
	// may break all the same (breakReasons).
	restoredTds, lost, err := s.restoredTds(ctx, cid, ptr.GetSpId(),
		restored.conf.GetTdNameList())
	if err != nil {
		return nil, err
	}
	if lost {
		pool.Verdict = pb.RestoreVerdict_RESTORE_VERDICT_RECORD_LOST
		return pool, nil
	}
	pool.BreakReasonList = breakReasons(held, restored)
	pool.Verdict = pb.RestoreVerdict_RESTORE_VERDICT_OLDER
	if len(pool.BreakReasonList) != 0 {
		pool.Verdict = pb.RestoreVerdict_RESTORE_VERDICT_MAY_BREAK
	}
	heldIds := map[uint64]bool{}
	restoredIds := map[uint64]bool{}
	for _, td := range restoredTds {
		restoredIds[td.GetTdId()] = true
	}
	nextDevId := restored.conf.GetNextDevId()
	for _, td := range held.GetTdList() {
		heldIds[td.GetTdId()] = true
		if !restoredIds[td.GetTdId()] {
			pool.LostTdList = append(pool.LostTdList,
				lostTdOf(td, td.GetDevId() >= nextDevId))
		}
	}
	for _, td := range restoredTds {
		if !heldIds[td.GetTdId()] {
			pool.ReturningTdList = append(pool.ReturningTdList,
				&pb.CheckControllerNodeRestoreReply_ReturningTd{
					TdId:  td.GetTdId(),
					DevId: td.GetDevId(),
				})
		}
	}
	slices.SortFunc(pool.LostTdList,
		func(a, b *pb.CheckControllerNodeRestoreReply_LostTd) int {
			return cmp.Compare(a.GetTdId(), b.GetTdId())
		})
	slices.SortFunc(pool.ReturningTdList,
		func(a, b *pb.CheckControllerNodeRestoreReply_ReturningTd) int {
			return cmp.Compare(a.GetTdId(), b.GetTdId())
		})
	return pool, nil
}

// breakReasons is why a pool held past the store may break, one text for
// each cause (architecture.md, Controller nodes,
// CheckControllerNodeRestore): the set of sides differs, or the held request
// lacks a retired leg's side id; and three changes that keep the set of
// sides: a migration whose cutover the restored state holds pending, which
// may have cut over since the backup, so that its old side serves again
// beside the new one; a primary role that differs, which the first worker
// hands out with no demotion first; and a namespace the restored state has
// and serves that the agent holds parked, or does not hold, deleted since
// the backup as at the end of a move to a pool of another etcd, so that
// hosts can reach two serving copies of its volume. A held summary from an
// agent without ns_id_list, which lists no namespace, reads every restored
// namespace as deleted: cautious. One text per cause, the migrations and
// the namespaces of a cause named in it, each cut to the row text cap
// (common.CutRowText), so the reply stays within its bound (model's limits
// test).
func breakReasons(
	held *pb.GetCnStoreReply_StoredCntlr,
	restored restoredPool,
) []string {
	var out []string
	if held.GetMaxSideId() > restored.maxSideId ||
		held.GetSideCnt() != restored.sideCnt {
		out = append(out, fmt.Sprintf("held sides differ from the "+
			"restored ones (largest side id %d against %d, %d sides "+
			"against %d): a side was made or removed since the backup",
			held.GetMaxSideId(), restored.maxSideId, held.GetSideCnt(),
			restored.sideCnt))
	}
	if held.GetRetiredSideIdMissing() {
		out = append(out, "the held request lacks the side id of a "+
			"retired leg, so a side made since the backup may not show")
	}
	if len(restored.pendingMigrs) != 0 {
		out = append(out, fmt.Sprintf("cutover pending in the restored "+
			"state for migration %s: if it ran since the backup, the old "+
			"side serves again beside the new one, with old or another "+
			"pool's bytes", strings.Join(restored.pendingMigrs, ", ")))
	}
	if held.GetPrimary() != restored.primary {
		heldRole := "a standby"
		if held.GetPrimary() {
			heldRole = "the primary"
		}
		restoredRole := "a standby"
		switch {
		case !restored.listed:
			restoredRole = "no cntlr, made since the backup"
		case restored.primary:
			restoredRole = "the primary"
		}
		out = append(out, fmt.Sprintf("held as %s, restored as %s: the "+
			"role moved since the backup, and the first worker hands the "+
			"restored role out with no demotion first", heldRole,
			restoredRole))
	}
	heldNs := map[uint64]bool{}
	for _, nsId := range held.GetNsIdList() {
		heldNs[nsId] = true
	}
	heldParked := map[uint64]bool{}
	for _, nsId := range held.GetParkedNsIdList() {
		heldParked[nsId] = true
	}
	var parked, deleted []string
	for _, nsId := range slices.Sorted(maps.Keys(restored.namespaces)) {
		switch {
		case restored.namespaces[nsId].parked:
		case heldParked[nsId]:
			parked = append(parked, strconv.FormatUint(nsId, 10))
		case !heldNs[nsId]:
			deleted = append(deleted, strconv.FormatUint(nsId, 10))
		}
	}
	if len(parked) != 0 {
		out = append(out, fmt.Sprintf("parked in what the agent holds and "+
			"serving in the restored state, ns_id %s: hosts can reach two "+
			"serving copies of a volume", strings.Join(parked, ", ")))
	}
	if len(deleted) != 0 {
		out = append(out, fmt.Sprintf("deleted since the backup and "+
			"serving in the restored state, ns_id %s, as after a move to "+
			"another etcd: hosts can reach a second copy of a volume",
			strings.Join(deleted, ", ")))
	}
	for i := range out {
		out[i] = common.CutRowText(out[i])
	}
	return out
}

// restoredTds reads every thin device of one restored pool in one range of
// their prefix, outside any STM (architecture.md, STM discipline); lost is a
// record that does not decode, or a name of the pool's td_name_list with no
// record, which the worker's read of the pool would refuse after the
// restore.
func (s *Server) restoredTds(
	ctx context.Context,
	cid uint64,
	spId uint64,
	names []string,
) ([]*pb.ThinDevice, bool, error) {
	prefix := model.ThinDevicePrefix(cid, spId)
	kvs, _, err := s.cli.Range(ctx, prefix)
	if err != nil {
		return nil, false, errAborted("%v", err)
	}
	tds := make([]*pb.ThinDevice, 0, len(kvs))
	have := make(map[string]bool, len(kvs))
	for _, kv := range kvs {
		td := &pb.ThinDevice{}
		if err := s.cli.Decode(ctx, kv, td); err != nil {
			return nil, true, nil
		}
		tds = append(tds, td)
		have[strings.TrimPrefix(kv.Key, prefix)] = true
	}
	for _, name := range names {
		if !have[name] {
			return nil, true, nil
		}
	}
	return tds, false, nil
}

// layoutSides is the largest side id and the count of the sides of grps and
// of the repairs' newLegs, as cnagent.md CN32 takes them from a held
// request: every side of every leg, and the side of each retired leg a
// group lists, which keeps its side out of the group's legs
// (architecture.md, Retired legs), by its id in retiredSides.
func layoutSides(
	grps []*pb.Group,
	newLegs []*pb.Leg,
	retiredSides map[uint64]uint64,
) (uint64, uint64) {
	var maxSideId, sideCnt uint64
	legs := append([]*pb.Leg(nil), newLegs...)
	for _, grp := range grps {
		legs = append(legs, grp.GetLegList()...)
		for _, legId := range grp.GetRetiredLegIdList() {
			maxSideId = max(maxSideId, retiredSides[legId])
			sideCnt++
		}
	}
	for _, leg := range legs {
		for _, side := range leg.GetSideList() {
			maxSideId = max(maxSideId, side.GetSideId())
			sideCnt++
		}
	}
	return maxSideId, sideCnt
}

// lostTdOf is the row of one held thin device the restored pool lacks.
func lostTdOf(
	td *pb.ThinDevice,
	idMayReturn bool,
) *pb.CheckControllerNodeRestoreReply_LostTd {
	return &pb.CheckControllerNodeRestoreReply_LostTd{
		TdId:        td.GetTdId(),
		DevId:       td.GetDevId(),
		OriId:       td.GetOriId(),
		Size:        td.GetSize(),
		Created:     td.GetCreated(),
		IdMayReturn: idMayReturn,
	}
}
