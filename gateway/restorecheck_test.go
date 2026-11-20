package gateway

// The restore check (architecture.md, Controller nodes,
// CheckControllerNodeRestore; gateway.md, Controller nodes): the desired
// state a controller node's agent holds, compared pool by pool with the
// restored etcd. The restored state is planted record by record, as an etcd
// restore leaves it, and the agent's answer is the fake's.

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// rcEnv is one cluster with one controller node, whose fake agent answers
// the restore check's GetCnStore.
type rcEnv struct {
	s       *Server
	cluster string
	cid     uint64
	addr    string
	cnId    uint64
	agent   *fakeAgent
}

func rcMakeEnv(t *testing.T) *rcEnv {
	t.Helper()
	s := newTestServer(t)
	cluster := hnodeName("restore")
	cid := mustCluster(t, s, cluster)
	addr := fakeAddrPort(t, "cn")
	cnId := mustCn(t, s, cluster, addr, "rack-1", 8<<30)
	return &rcEnv{
		s: s, cluster: cluster, cid: cid, addr: addr, cnId: cnId,
		agent: fakeAgentOf(t, addr),
	}
}

// rcPool is one restored pool to plant: its records at a revision, one
// slice whose one data group's legs carry sideIds, one leg a side each, its
// first leg also migrSide when set, the group listing retiredLegs, its
// clones by name with the side ids of their meta groups (a latched clone in
// deletingClones), and its thin devices by name; tdNames names more thin
// devices than tds holds, as a record lost under a listed name.
type rcPool struct {
	spId           uint64
	name           string
	revision       uint64
	nextDevId      uint32
	sideIds        []uint64
	migrSide       uint64
	retiredLegs    []uint64
	clones         map[string][]uint64
	deletingClones map[string]bool
	tds            map[string]*pb.ThinDevice
	tdNames        []string
}

func (e *rcEnv) putPool(t *testing.T, p rcPool) {
	t.Helper()
	const shard = uint32(3)
	sliceId := p.spId<<8 | 1
	tdNames := append([]string(nil), p.tdNames...)
	for name, td := range p.tds {
		tdNames = append(tdNames, name)
		mustPut(t, e.s.cli, model.ThinDeviceKey(e.cid, p.spId, name), td)
	}
	var legs []*pb.Leg
	for i, sideId := range p.sideIds {
		legs = append(legs, &pb.Leg{
			LegId:    sideId - 1,
			LegIdx:   uint32(i),
			SideList: []*pb.Side{{SideId: sideId}},
		})
	}
	if p.migrSide != 0 {
		legs[0].SideList = append(legs[0].SideList,
			&pb.Side{SideId: p.migrSide})
	}
	var cloneNames []string
	for name, sideIds := range p.clones {
		cloneNames = append(cloneNames, name)
		grp := &pb.Group{GrpId: sideIds[0] + 0x100}
		for i, sideId := range sideIds {
			grp.LegList = append(grp.LegList, &pb.Leg{
				LegId:    sideId - 1,
				LegIdx:   uint32(i),
				SideList: []*pb.Side{{SideId: sideId}},
			})
		}
		mustPut(t, e.s.cli, model.CloneKey(e.cid, p.spId, name), &pb.Clone{
			CloneId:  sideIds[0] + 0x200,
			Deleting: p.deletingClones[name],
			MetaGrp:  grp,
		})
	}
	e.putRetired(t, p.spId, sliceId, "", p.retiredLegs)
	mustPut(t, e.s.cli, model.SpNameKey(e.cid, p.spId),
		&pb.SpName{SpName: p.name})
	mustPut(t, e.s.cli, model.SpConfKey(e.cid, p.name), &pb.SpConf{
		SpId:             p.spId,
		ShardCode:        shard,
		NextDevId:        p.nextDevId,
		SliceIdList:      []uint64{sliceId},
		TdNameList:       tdNames,
		CloneNameList:    cloneNames,
		RetiredLegIdList: p.retiredLegs,
	})
	mustPut(t, e.s.cli, model.SpRevKey(shard, e.cid, p.spId),
		&pb.SpRev{SpName: p.name, Revision: p.revision})
	mustPut(t, e.s.cli, model.SliceKey(e.cid, p.spId, sliceId), &pb.Slice{
		DataGrpList: []*pb.Group{{
			GrpId:            sliceId + 1,
			LegList:          legs,
			RetiredLegIdList: p.retiredLegs,
		}},
	})
}

// putRetired plants the RetiredLeg record of each of legIds, its side id
// one above its leg id, as the legs of these tests number their sides.
func (e *rcEnv) putRetired(
	t *testing.T,
	spId, sliceId uint64,
	cloneName string,
	legIds []uint64,
) []uint64 {
	t.Helper()
	for _, legId := range legIds {
		mustPut(t, e.s.cli, model.RetiredLegKey(e.cid, spId, legId),
			&pb.RetiredLeg{
				Side:      &pb.Side{SideId: legId + 1},
				SliceId:   sliceId,
				CloneName: cloneName,
			})
	}
	return legIds
}

// setRestoredPtrs makes the restored node at the address name ptrs.
func (e *rcEnv) setRestoredPtrs(t *testing.T, ptrs ...*pb.CntlrPointer) {
	t.Helper()
	cn := &pb.CnConf{}
	hnodeGet(t, e.s, model.CnConfKey(e.cid, e.addr), cn)
	cn.CntlrPtrList = ptrs
	mustPut(t, e.s.cli, model.CnConfKey(e.cid, e.addr), cn)
}

func (e *rcEnv) check(
	t *testing.T,
) (*pb.CheckControllerNodeRestoreReply, error) {
	t.Helper()
	return e.s.CheckControllerNodeRestore(context.Background(),
		&pb.CheckControllerNodeRestoreRequest{
			ClusterName: e.cluster,
			AddrPort:    e.addr,
		})
}

func rcPtr(spId, cntlrId uint64) *pb.CntlrPointer {
	return &pb.CntlrPointer{SpId: spId, CntlrId: cntlrId}
}

// rcUndecodable is a message that does not decode: a well-formed field
// followed by a truncated tag.
func rcUndecodable(t *testing.T, msg proto.Message) proto.Message {
	t.Helper()
	msg.ProtoReflect().SetUnknown(protoreflect.RawFields{0x08})
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := proto.Unmarshal(raw, msg.ProtoReflect().Type().New().
		Interface()); err == nil {
		t.Fatalf("the value %x decodes", raw)
	}
	return msg
}

// rcPoolOf finds the row of (sp_id, cntlr_id).
func rcPoolOf(
	t *testing.T,
	reply *pb.CheckControllerNodeRestoreReply,
	spId, cntlrId uint64,
) *pb.CheckControllerNodeRestoreReply_Pool {
	t.Helper()
	for _, pool := range reply.GetPoolList() {
		if pool.GetSpId() == spId && pool.GetCntlrId() == cntlrId {
			return pool
		}
	}
	t.Fatalf("no row of sp %d, cntlr %d in %v", spId, cntlrId,
		reply.GetPoolList())
	return nil
}

// TestCheckControllerNodeRestoreJudgesEachPool pins the verdicts of
// architecture.md, Controller nodes, CheckControllerNodeRestore, one pool
// each: the same revision; an older restored revision, which lists the lost
// thin devices with whether their thin id may return and the restored ones
// the agent lacks; a side made after the backup; a pool the restored state
// lacks; a store behind the restored state; a pointer with no request held;
// a restored cntlr the agent does not hold; and a pool whose records are
// missing or do not decode, which gives its row and not the answer's error.
// A pool at the restored revision reads no thin device: an undecodable one
// under it changes nothing.
func TestCheckControllerNodeRestoreJudgesEachPool(t *testing.T) {
	e := rcMakeEnv(t)
	const (
		spSame      = uint64(0x101)
		spOlder     = uint64(0x102)
		spBreak     = uint64(0x103)
		spNew       = uint64(0x104)
		spBehind    = uint64(0x105)
		spNotLoaded = uint64(0x106)
		spNotHeld   = uint64(0x107)
		spLost      = uint64(0x108)
		spBadConf   = uint64(0x109)
	)
	e.putPool(t, rcPool{spId: spSame, name: "same", revision: 5,
		sideIds: []uint64{0x31, 0x33}})
	mustPut(t, e.s.cli, model.ThinDeviceKey(e.cid, spSame, "bad"),
		rcUndecodable(t, &pb.ThinDevice{TdId: 1}))
	e.putPool(t, rcPool{spId: spOlder, name: "older", revision: 5,
		nextDevId: 3, sideIds: []uint64{0x31, 0x33},
		tds: map[string]*pb.ThinDevice{
			"t0": {TdId: 1, DevId: 1, Size: 1 << 30, Created: true},
			"t9": {TdId: 9, DevId: 0, Size: 1 << 30, Created: true},
		}})
	e.putPool(t, rcPool{spId: spBreak, name: "break", revision: 5,
		sideIds: []uint64{0x31, 0x33}})
	e.putPool(t, rcPool{spId: spBehind, name: "behind", revision: 9,
		sideIds: []uint64{0x31}})
	e.putPool(t, rcPool{spId: spNotLoaded, name: "notloaded", revision: 4,
		sideIds: []uint64{0x31}})
	e.putPool(t, rcPool{spId: spNotHeld, name: "notheld", revision: 6,
		sideIds: []uint64{0x31}})
	mustPut(t, e.s.cli, model.SpNameKey(e.cid, spLost),
		&pb.SpName{SpName: "lost"})
	mustPut(t, e.s.cli, model.SpNameKey(e.cid, spBadConf),
		&pb.SpName{SpName: "badconf"})
	mustPut(t, e.s.cli, model.SpConfKey(e.cid, "badconf"),
		rcUndecodable(t, &pb.SpConf{SpId: spBadConf}))
	e.setRestoredPtrs(t, rcPtr(spSame, 1), rcPtr(spOlder, 1),
		rcPtr(spNotHeld, 1))

	heldTds := []*pb.ThinDevice{
		{TdId: 1, DevId: 1, Size: 1 << 30, Created: true},
		{TdId: 2, DevId: 2, Size: 1 << 30, Created: true},
		{TdId: 3, DevId: 3, OriId: 2, Size: 1 << 30},
	}
	// Every held layout counts two sides, as the restored layouts of the
	// pools ahead of the store do.
	cntlr := func(spId, revision, maxSide uint64,
		tds []*pb.ThinDevice, nqns ...string,
	) *pb.GetCnStoreReply_StoredCntlr {
		return &pb.GetCnStoreReply_StoredCntlr{
			CntlrPointer: rcPtr(spId, 1),
			Revision:     revision,
			Primary:      true,
			TdList:       tds,
			NqnList:      nqns,
			MaxSideId:    maxSide,
			SideCnt:      2,
		}
	}
	e.agent.setCnStore(&pb.GetCnStoreReply{
		AgentReply: &pb.AgentReply{},
		CnList: []*pb.GetCnStoreReply_StoredCn{{
			CnId:     e.cnId,
			Revision: 12,
			CntlrPointerList: []*pb.CntlrPointer{
				rcPtr(spSame, 1), rcPtr(spOlder, 1), rcPtr(spBreak, 1),
				rcPtr(spNew, 1), rcPtr(spBehind, 1), rcPtr(spNotLoaded, 1),
				rcPtr(spLost, 1), rcPtr(spBadConf, 1),
			},
			CntlrList: []*pb.GetCnStoreReply_StoredCntlr{
				cntlr(spSame, 5, 0x33, heldTds),
				cntlr(spOlder, 7, 0x33, heldTds),
				cntlr(spBreak, 6, 0x40, nil),
				cntlr(spNew, 2, 0x10, heldTds[:1],
					"nqn.2025-01.io.dnv:csi:pvc-1"),
				cntlr(spBehind, 4, 0x31, heldTds),
				cntlr(spLost, 3, 0x31, nil),
				cntlr(spBadConf, 3, 0x31, nil),
			},
		}},
	})
	cnRevKey := model.CnRevKey(hnodeCnShard(t, e.s, e.cid, e.addr), e.cid,
		e.cnId)
	cnRevBefore := hnodeModRev(t, e.s, cnRevKey)

	reply, err := e.check(t)
	wantCode(t, err, codes.OK, "the restore check")
	if reqs := e.agent.cnStoreReqList(); len(reqs) != 1 ||
		reqs[0].GetClusterId() != e.cid || reqs[0].GetCnId() != 0 {
		t.Errorf("GetCnStore asked %v, want one call for every node of "+
			"cluster %d", reqs, e.cid)
	}
	if reply.GetRestoredCnId() != e.cnId || reply.GetRestoredRevision() != 1 {
		t.Errorf("restored node %d at %d, want %d at 1",
			reply.GetRestoredCnId(), reply.GetRestoredRevision(), e.cnId)
	}
	if held := reply.GetHeldCnList(); len(held) != 1 ||
		held[0].GetCnId() != e.cnId || held[0].GetRevision() != 12 {
		t.Errorf("held nodes %v, want node %d at 12", held, e.cnId)
	}
	if got := len(reply.GetPoolList()); got != 9 {
		t.Errorf("%d rows, want 9: %v", got, reply.GetPoolList())
	}

	want := func(spId uint64, row *pb.CheckControllerNodeRestoreReply_Pool) {
		t.Helper()
		got := rcPoolOf(t, reply, spId, 1)
		row.SpId, row.CntlrId = spId, 1
		if !proto.Equal(got, row) {
			t.Errorf("sp %#x:\n got %v\nwant %v", spId, got, row)
		}
	}
	want(spSame, &pb.CheckControllerNodeRestoreReply_Pool{
		CnId:    e.cnId,
		Verdict: pb.RestoreVerdict_RESTORE_VERDICT_SAME, SpName: "same",
		HeldRevision:     5,
		RestoredRevision: 5, Primary: true, CntlrInRestoredState: true,
		HeldMaxSideId: 0x33, HeldSideCnt: 2,
	})
	want(spOlder, &pb.CheckControllerNodeRestoreReply_Pool{
		CnId:    e.cnId,
		Verdict: pb.RestoreVerdict_RESTORE_VERDICT_OLDER, SpName: "older",
		HeldRevision: 7, RestoredRevision: 5, Primary: true,
		CntlrInRestoredState: true, HeldMaxSideId: 0x33,
		RestoredMaxSideId: 0x33, HeldSideCnt: 2, RestoredSideCnt: 2,
		LostTdList: []*pb.CheckControllerNodeRestoreReply_LostTd{
			{TdId: 2, DevId: 2, Size: 1 << 30, Created: true},
			{TdId: 3, DevId: 3, OriId: 2, Size: 1 << 30, IdMayReturn: true},
		},
		ReturningTdList: []*pb.CheckControllerNodeRestoreReply_ReturningTd{
			{TdId: 9},
		},
	})
	want(spBreak, &pb.CheckControllerNodeRestoreReply_Pool{
		CnId:    e.cnId,
		Verdict: pb.RestoreVerdict_RESTORE_VERDICT_MAY_BREAK,
		SpName:  "break", HeldRevision: 6, RestoredRevision: 5,
		Primary: true, HeldMaxSideId: 0x40, RestoredMaxSideId: 0x33,
		HeldSideCnt: 2, RestoredSideCnt: 2,
	})
	want(spNew, &pb.CheckControllerNodeRestoreReply_Pool{
		CnId:    e.cnId,
		Verdict: pb.RestoreVerdict_RESTORE_VERDICT_NEW, HeldRevision: 2,
		Primary: true, HeldMaxSideId: 0x10, HeldSideCnt: 2,
		NqnList: []string{"nqn.2025-01.io.dnv:csi:pvc-1"},
		LostTdList: []*pb.CheckControllerNodeRestoreReply_LostTd{
			{TdId: 1, DevId: 1, Size: 1 << 30, Created: true},
		},
	})
	want(spBehind, &pb.CheckControllerNodeRestoreReply_Pool{
		CnId:    e.cnId,
		Verdict: pb.RestoreVerdict_RESTORE_VERDICT_BEHIND, SpName: "behind",
		HeldRevision: 4, RestoredRevision: 9, Primary: true,
		HeldMaxSideId: 0x31, HeldSideCnt: 2,
	})
	want(spNotLoaded, &pb.CheckControllerNodeRestoreReply_Pool{
		CnId:    e.cnId,
		Verdict: pb.RestoreVerdict_RESTORE_VERDICT_NOT_LOADED,
		SpName:  "notloaded", RestoredRevision: 4,
	})
	want(spNotHeld, &pb.CheckControllerNodeRestoreReply_Pool{
		CnId:    e.cnId,
		Verdict: pb.RestoreVerdict_RESTORE_VERDICT_NOT_HELD,
		SpName:  "notheld", RestoredRevision: 6, CntlrInRestoredState: true,
	})
	want(spLost, &pb.CheckControllerNodeRestoreReply_Pool{
		CnId:    e.cnId,
		Verdict: pb.RestoreVerdict_RESTORE_VERDICT_RECORD_LOST,
		SpName:  "lost", HeldRevision: 3, Primary: true,
		HeldMaxSideId: 0x31, HeldSideCnt: 2,
	})
	want(spBadConf, &pb.CheckControllerNodeRestoreReply_Pool{
		CnId:    e.cnId,
		Verdict: pb.RestoreVerdict_RESTORE_VERDICT_RECORD_LOST,
		SpName:  "badconf", HeldRevision: 3, Primary: true,
		HeldMaxSideId: 0x31, HeldSideCnt: 2,
	})
	if got := hnodeModRev(t, e.s, cnRevKey); got != cnRevBefore {
		t.Errorf("the check moved the node's cn_rev: %d -> %d",
			cnRevBefore, got)
	}
}

// TestCheckControllerNodeRestoreRefusesAnUnprovenAgent pins the check's
// agent arms (AG3): an agent that does not answer, or answers that it
// starts, or with another code, proves nothing, FAILED_PRECONDITION; the
// unknown object of an agent that holds no node of the cluster reads as no
// node held, and the check answers OK, every cntlr the restored node names
// not held.
func TestCheckControllerNodeRestoreRefusesAnUnprovenAgent(t *testing.T) {
	e := rcMakeEnv(t)
	e.putPool(t, rcPool{spId: 0x201, name: "p", revision: 3,
		sideIds: []uint64{0x31}})
	e.setRestoredPtrs(t, rcPtr(0x201, 4))

	e.agent.setFail(status.Error(codes.Unavailable, "agent gone"))
	_, err := e.check(t)
	wantCode(t, err, codes.FailedPrecondition, "an agent that does not answer")
	hnodeWantMsg(t, err, "did not answer", "an agent that does not answer")
	e.agent.setFail(nil)

	e.agent.setCnStore(&pb.GetCnStoreReply{AgentReply: &pb.AgentReply{
		Code: common.ReplyCodeStarting, Details: "starting"}})
	_, err = e.check(t)
	wantCode(t, err, codes.FailedPrecondition, "a starting agent")
	hnodeWantMsg(t, err, "starting", "a starting agent")

	e.agent.setCnStore(&pb.GetCnStoreReply{AgentReply: &pb.AgentReply{
		Code: common.ReplyCodeLeftover, Details: "odd"}})
	_, err = e.check(t)
	wantCode(t, err, codes.FailedPrecondition, "another code")
	hnodeWantMsg(t, err, "code 4", "another code")

	e.agent.setCnStore(nil)
	reply, err := e.check(t)
	wantCode(t, err, codes.OK, "an agent that holds no node")
	if len(reply.GetHeldCnList()) != 0 {
		t.Errorf("held nodes %v, want none", reply.GetHeldCnList())
	}
	row := rcPoolOf(t, reply, 0x201, 4)
	if row.GetVerdict() != pb.RestoreVerdict_RESTORE_VERDICT_NOT_HELD ||
		row.GetSpName() != "p" || row.GetRestoredRevision() != 3 {
		t.Errorf("the restored cntlr: %v, want NOT_HELD of p at 3", row)
	}
}

// TestCheckControllerNodeRestoreReachesANodeAddedAfterTheBackup pins the
// node the restored state does not name: its agent is asked all the same,
// and what it holds is judged, with no restored node.
func TestCheckControllerNodeRestoreReachesANodeAddedAfterTheBackup(
	t *testing.T,
) {
	e := rcMakeEnv(t)
	addr := fakeAddrPort(t, "cn-new")
	agent := startFakeAgent(t, addr, 8<<30)
	agent.setCnStore(&pb.GetCnStoreReply{
		AgentReply: &pb.AgentReply{},
		CnList: []*pb.GetCnStoreReply_StoredCn{{
			CnId: 77, Revision: 2,
			CntlrPointerList: []*pb.CntlrPointer{rcPtr(0x301, 1)},
			CntlrList: []*pb.GetCnStoreReply_StoredCntlr{{
				CntlrPointer: rcPtr(0x301, 1), Revision: 2,
			}},
		}},
	})
	reply, err := e.s.CheckControllerNodeRestore(context.Background(),
		&pb.CheckControllerNodeRestoreRequest{
			ClusterName: e.cluster, AddrPort: addr})
	wantCode(t, err, codes.OK, "a node added after the backup")
	if reply.GetRestoredCnId() != 0 || reply.GetRestoredRevision() != 0 {
		t.Errorf("restored node %d at %d, want none",
			reply.GetRestoredCnId(), reply.GetRestoredRevision())
	}
	row := rcPoolOf(t, reply, 0x301, 1)
	if row.GetVerdict() != pb.RestoreVerdict_RESTORE_VERDICT_NEW ||
		row.GetCnId() != 77 || row.GetCntlrInRestoredState() {
		t.Errorf("the held cntlr: %v, want NEW under node 77", row)
	}
}

// TestCheckControllerNodeRestoreSeesTheSetOfSidesChange pins MAY_BREAK on
// any change of the set of sides between the backup and the held layout
// (architecture.md, Controller nodes, CheckControllerNodeRestore): side ids
// come from a counter of the pool that only grows, so an added side shows
// as a higher largest id and a removed one as a smaller count. Each pool is
// held one revision ahead of the store: a migration that started before
// the backup and finished after it (the source side gone, the largest id
// the same); one that was cancelled after it (the destination gone, the
// largest id lower); a retired leg deleted after it; and a clone drained
// after it, its meta group gone. A clone latched before the backup is
// absent from the agent's request and is not counted on either side, and
// the same set of sides reads older.
func TestCheckControllerNodeRestoreSeesTheSetOfSidesChange(t *testing.T) {
	e := rcMakeEnv(t)
	const (
		spFinished = uint64(0x401)
		spCanceled = uint64(0x402)
		spRetired  = uint64(0x403)
		spDrained  = uint64(0x404)
		spLatched  = uint64(0x405)
	)
	e.putPool(t, rcPool{spId: spFinished, name: "finished", revision: 5,
		sideIds: []uint64{0x31, 0x33}, migrSide: 0x40})
	e.putPool(t, rcPool{spId: spCanceled, name: "canceled", revision: 5,
		sideIds: []uint64{0x31, 0x33}, migrSide: 0x40})
	e.putPool(t, rcPool{spId: spRetired, name: "retired", revision: 5,
		sideIds: []uint64{0x31, 0x33}, retiredLegs: []uint64{0x20}})
	e.putPool(t, rcPool{spId: spDrained, name: "drained", revision: 5,
		sideIds: []uint64{0x31, 0x33},
		clones:  map[string][]uint64{"c0": {0x45, 0x46}}})
	e.putPool(t, rcPool{spId: spLatched, name: "latched", revision: 5,
		sideIds:        []uint64{0x31, 0x33},
		clones:         map[string][]uint64{"c0": {0x45, 0x46}},
		deletingClones: map[string]bool{"c0": true}})
	held := func(spId, maxSide, sideCnt uint64) *pb.GetCnStoreReply_StoredCntlr {
		return &pb.GetCnStoreReply_StoredCntlr{
			CntlrPointer: rcPtr(spId, 1),
			Revision:     6,
			MaxSideId:    maxSide,
			SideCnt:      sideCnt,
		}
	}
	e.agent.setCnStore(&pb.GetCnStoreReply{
		AgentReply: &pb.AgentReply{},
		CnList: []*pb.GetCnStoreReply_StoredCn{{
			CnId:     e.cnId,
			Revision: 3,
			CntlrPointerList: []*pb.CntlrPointer{
				rcPtr(spFinished, 1), rcPtr(spCanceled, 1),
				rcPtr(spRetired, 1), rcPtr(spDrained, 1),
				rcPtr(spLatched, 1),
			},
			CntlrList: []*pb.GetCnStoreReply_StoredCntlr{
				// D alone in the first leg: the source side 0x31 left.
				held(spFinished, 0x40, 2),
				// S alone again: the destination 0x40 left.
				held(spCanceled, 0x33, 2),
				// The retired leg 0x20 deleted.
				held(spRetired, 0x33, 2),
				// The clone and its meta group gone.
				held(spDrained, 0x33, 2),
				// The latched clone is in no request.
				held(spLatched, 0x33, 2),
			},
		}},
	})
	reply, err := e.check(t)
	wantCode(t, err, codes.OK, "the restore check")
	for _, c := range []struct {
		label      string
		spId       uint64
		verdict    pb.RestoreVerdict
		restoredCn uint64
	}{
		{"a migration finished after the backup", spFinished,
			pb.RestoreVerdict_RESTORE_VERDICT_MAY_BREAK, 3},
		{"a migration cancelled after the backup", spCanceled,
			pb.RestoreVerdict_RESTORE_VERDICT_MAY_BREAK, 3},
		{"a retired leg deleted after the backup", spRetired,
			pb.RestoreVerdict_RESTORE_VERDICT_MAY_BREAK, 3},
		{"a clone drained after the backup", spDrained,
			pb.RestoreVerdict_RESTORE_VERDICT_MAY_BREAK, 4},
		{"a clone latched before the backup", spLatched,
			pb.RestoreVerdict_RESTORE_VERDICT_OLDER, 2},
	} {
		row := rcPoolOf(t, reply, c.spId, 1)
		if row.GetVerdict() != c.verdict ||
			row.GetRestoredSideCnt() != c.restoredCn ||
			row.GetHeldSideCnt() != 2 {
			t.Errorf("%s: %v (held %d sides, restored %d), want %v over "+
				"2 and %d sides", c.label, row.GetVerdict(),
				row.GetHeldSideCnt(), row.GetRestoredSideCnt(), c.verdict,
				c.restoredCn)
		}
	}
}

// TestCheckControllerNodeRestoreGivesALostRecordItsRow pins RECORD_LOST
// beyond the pool records every row reads: a cntlr the restored node names
// whose pool the restored state names nowhere, and a pool held ahead of the
// store whose thin device list names a thin device with no record, which
// the worker's read of the pool would refuse after the restore.
func TestCheckControllerNodeRestoreGivesALostRecordItsRow(t *testing.T) {
	e := rcMakeEnv(t)
	const (
		spGone = uint64(0x501)
		spNoTd = uint64(0x502)
	)
	e.putPool(t, rcPool{spId: spNoTd, name: "notd", revision: 5,
		sideIds: []uint64{0x31}, tdNames: []string{"t-gone"},
		tds: map[string]*pb.ThinDevice{"t0": {TdId: 1}}})
	e.setRestoredPtrs(t, rcPtr(spGone, 1))
	e.agent.setCnStore(&pb.GetCnStoreReply{
		AgentReply: &pb.AgentReply{},
		CnList: []*pb.GetCnStoreReply_StoredCn{{
			CnId:             e.cnId,
			Revision:         2,
			CntlrPointerList: []*pb.CntlrPointer{rcPtr(spNoTd, 1)},
			CntlrList: []*pb.GetCnStoreReply_StoredCntlr{{
				CntlrPointer: rcPtr(spNoTd, 1),
				Revision:     6,
				MaxSideId:    0x31,
				SideCnt:      1,
				TdList:       []*pb.ThinDevice{{TdId: 1}},
			}},
		}},
	})
	reply, err := e.check(t)
	wantCode(t, err, codes.OK, "the restore check")
	for _, c := range []struct {
		label string
		spId  uint64
	}{
		{"a restored cntlr of a pool named nowhere", spGone},
		{"a listed thin device with no record", spNoTd},
	} {
		if got := rcPoolOf(t, reply, c.spId, 1).GetVerdict(); got !=
			pb.RestoreVerdict_RESTORE_VERDICT_RECORD_LOST {
			t.Errorf("%s: %v, want RECORD_LOST", c.label, got)
		}
	}
}

// rcLeg is a leg of one side or more.
func rcLeg(legId uint64, sideIds ...uint64) *pb.Leg {
	leg := &pb.Leg{LegId: legId}
	for _, id := range sideIds {
		leg.SideList = append(leg.SideList, &pb.Side{SideId: id})
	}
	return leg
}

// putLayout plants one restored pool at revision 5 whose one slice, id
// sliceId, holds grps as its data groups, with its repairs and its clones
// by name, the repairs listed in repair_name_list and a name of repNames
// with no record among them as listed.
func (e *rcEnv) putLayout(
	t *testing.T,
	spId uint64,
	name string,
	sliceId uint64,
	grps []*pb.Group,
	reps map[string]*pb.Repair,
	clones map[string]*pb.Clone,
	repNames ...string,
) {
	t.Helper()
	const shard = uint32(3)
	for n, rep := range reps {
		repNames = append(repNames, n)
		mustPut(t, e.s.cli, model.RepairKey(e.cid, spId, n), rep)
	}
	var retired []uint64
	for _, grp := range grps {
		retired = append(retired, e.putRetired(t, spId, sliceId, "",
			grp.GetRetiredLegIdList())...)
	}
	var cloneNames []string
	for n, clone := range clones {
		cloneNames = append(cloneNames, n)
		mustPut(t, e.s.cli, model.CloneKey(e.cid, spId, n), clone)
		retired = append(retired, e.putRetired(t, spId, 0, n,
			clone.GetMetaGrp().GetRetiredLegIdList())...)
	}
	mustPut(t, e.s.cli, model.SpNameKey(e.cid, spId), &pb.SpName{SpName: name})
	mustPut(t, e.s.cli, model.SpConfKey(e.cid, name), &pb.SpConf{
		SpId:             spId,
		ShardCode:        shard,
		SliceIdList:      []uint64{sliceId},
		RepairNameList:   repNames,
		CloneNameList:    cloneNames,
		RetiredLegIdList: retired,
	})
	mustPut(t, e.s.cli, model.SpRevKey(shard, e.cid, spId),
		&pb.SpRev{SpName: name, Revision: 5})
	mustPut(t, e.s.cli, model.SliceKey(e.cid, spId, sliceId),
		&pb.Slice{DataGrpList: grps})
}

// TestCheckControllerNodeRestoreCountsTheRepairsNewLegs pins the new legs of
// the open repairs in the set of sides (architecture.md, Controller nodes,
// CheckControllerNodeRestore, MAY_BREAK): a repair takes its new leg's side
// id at its create and moves the leg into its group only at its finish, so
// counting the new legs on both sides makes a finish and a take-back of a
// retired leg moves inside the set, which read older, while a removal a
// finish would hide reads may break. Each pool is held one revision ahead
// of the store, the held numbers as the agent counts its request:
//   - a repair (new side 0x38) and a migration (destination 0x40) open at
//     the backup, both finished after it: the source side 0x31 is gone;
//   - a worker's repair open at the backup, finished after it with its
//     group at the bound of retired legs, which gives the oldest one back;
//   - a repair made after the backup and still open, with a new side
//     above every restored one;
//   - a repair open at the backup and finished after it, and nothing else;
//   - a retired leg taken back as a repair's new leg after the backup;
//   - a repair of a latched clone's meta group, which the worker sends no
//     cntlr and the check counts on neither side.
func TestCheckControllerNodeRestoreCountsTheRepairsNewLegs(t *testing.T) {
	e := rcMakeEnv(t)
	const (
		spMasked   = uint64(0x601)
		spBound    = uint64(0x602)
		spOpen     = uint64(0x603)
		spFinished = uint64(0x604)
		spTakeBack = uint64(0x605)
		spLatched  = uint64(0x606)
	)
	slice := func(spId uint64) uint64 { return spId<<8 | 1 }
	e.putLayout(t, spMasked, "masked", slice(spMasked), []*pb.Group{
		{GrpId: 0x10, LegList: []*pb.Leg{
			rcLeg(0x30, 0x31, 0x40), rcLeg(0x32, 0x33)}},
		{GrpId: 0x11, RepName: "r", LegList: []*pb.Leg{
			rcLeg(0x34, 0x35), rcLeg(0x36, 0x36)}},
	}, map[string]*pb.Repair{"r": {
		RepId: 0x39, SliceId: slice(spMasked), GrpId: 0x11,
		BadLegIdList: []uint64{0x34},
		NewLegList:   []*pb.Leg{rcLeg(0x37, 0x38)},
	}}, nil)
	e.putLayout(t, spBound, "bound", slice(spBound), []*pb.Group{
		{GrpId: 0x10, RepName: "w", LegList: []*pb.Leg{
			rcLeg(0x30, 0x31), rcLeg(0x32, 0x33)},
			RetiredLegIdList: []uint64{0x20, 0x22}},
		{GrpId: 0x11, LegList: []*pb.Leg{
			rcLeg(0x40, 0x41), rcLeg(0x47, 0x48)},
			RetiredLegIdList: []uint64{0x42}},
	}, map[string]*pb.Repair{"w": {
		RepId: 0x46, SliceId: slice(spBound), GrpId: 0x10,
		BadLegIdList: []uint64{0x32},
		NewLegList:   []*pb.Leg{rcLeg(0x44, 0x45)},
	}}, nil)
	e.putLayout(t, spOpen, "open", slice(spOpen), []*pb.Group{
		{GrpId: 0x10, LegList: []*pb.Leg{rcLeg(0x30, 0x31), rcLeg(0x32, 0x33)}},
	}, nil, nil)
	e.putLayout(t, spFinished, "finished", slice(spFinished), []*pb.Group{
		{GrpId: 0x10, RepName: "f", LegList: []*pb.Leg{
			rcLeg(0x30, 0x31), rcLeg(0x32, 0x33)}},
	}, map[string]*pb.Repair{"f": {
		RepId: 0x36, SliceId: slice(spFinished), GrpId: 0x10,
		BadLegIdList: []uint64{0x32},
		NewLegList:   []*pb.Leg{rcLeg(0x34, 0x35)},
	}}, nil)
	e.putLayout(t, spTakeBack, "takeback", slice(spTakeBack), []*pb.Group{
		{GrpId: 0x10, LegList: []*pb.Leg{rcLeg(0x30, 0x31), rcLeg(0x34, 0x35)},
			RetiredLegIdList: []uint64{0x32}},
	}, nil, nil)
	e.putLayout(t, spLatched, "latched", slice(spLatched), []*pb.Group{
		{GrpId: 0x10, LegList: []*pb.Leg{rcLeg(0x30, 0x31), rcLeg(0x32, 0x33)}},
	}, map[string]*pb.Repair{"c": {
		RepId: 0x50, CloneName: "c0", GrpId: 0x48,
		NewLegList: []*pb.Leg{rcLeg(0x4e, 0x4f)},
	}}, map[string]*pb.Clone{"c0": {
		CloneId: 0x49, Deleting: true, MetaGrp: &pb.Group{
			GrpId: 0x48, RepName: "c", LegList: []*pb.Leg{rcLeg(0x4a, 0x4b)},
		},
	}})
	held := func(spId, maxSide, sideCnt uint64) *pb.GetCnStoreReply_StoredCntlr {
		return &pb.GetCnStoreReply_StoredCntlr{
			CntlrPointer: rcPtr(spId, 1),
			Revision:     6,
			MaxSideId:    maxSide,
			SideCnt:      sideCnt,
		}
	}
	e.agent.setCnStore(&pb.GetCnStoreReply{
		AgentReply: &pb.AgentReply{},
		CnList: []*pb.GetCnStoreReply_StoredCn{{
			CnId:     e.cnId,
			Revision: 3,
			CntlrPointerList: []*pb.CntlrPointer{
				rcPtr(spMasked, 1), rcPtr(spBound, 1), rcPtr(spOpen, 1),
				rcPtr(spFinished, 1), rcPtr(spTakeBack, 1),
				rcPtr(spLatched, 1),
			},
			CntlrList: []*pb.GetCnStoreReply_StoredCntlr{
				// 0x40 alone in the first leg beside 0x33; the repair's
				// new leg 0x38 in its group beside 0x36, the bad leg 0x34
				// retired: five sides, the restored six.
				held(spMasked, 0x40, 5),
				// 0x31, 0x45; retired 0x22, 0x32 (sides 0x23, 0x33); 0x41,
				// 0x48, retired 0x42 (side 0x43): seven, the restored eight
				// with the repair's new leg.
				held(spBound, 0x48, 7),
				// The two legs and the repair's new leg 0x51.
				held(spOpen, 0x51, 3),
				// 0x31, 0x35 and the retired 0x32 (side 0x33): three, as
				// restored.
				held(spFinished, 0x35, 3),
				// 0x31, 0x35 and the taken-back leg as a new leg: three.
				held(spTakeBack, 0x35, 3),
				// The latched clone and its repair are in no request.
				held(spLatched, 0x33, 2),
			},
		}},
	})
	reply, err := e.check(t)
	wantCode(t, err, codes.OK, "the restore check")
	for _, c := range []struct {
		label      string
		spId       uint64
		verdict    pb.RestoreVerdict
		restoredCn uint64
	}{
		{"a migration finished beside a repair finished", spMasked,
			pb.RestoreVerdict_RESTORE_VERDICT_MAY_BREAK, 6},
		{"a repair finished at the retired-leg bound", spBound,
			pb.RestoreVerdict_RESTORE_VERDICT_MAY_BREAK, 8},
		{"a repair made after the backup and open", spOpen,
			pb.RestoreVerdict_RESTORE_VERDICT_MAY_BREAK, 2},
		{"a repair finished after the backup", spFinished,
			pb.RestoreVerdict_RESTORE_VERDICT_OLDER, 3},
		{"a retired leg taken back after the backup", spTakeBack,
			pb.RestoreVerdict_RESTORE_VERDICT_OLDER, 3},
		{"a repair of a latched clone's meta group", spLatched,
			pb.RestoreVerdict_RESTORE_VERDICT_OLDER, 2},
	} {
		row := rcPoolOf(t, reply, c.spId, 1)
		if row.GetVerdict() != c.verdict ||
			row.GetRestoredSideCnt() != c.restoredCn {
			t.Errorf("%s: %v over %d restored sides, want %v over %d",
				c.label, row.GetVerdict(), row.GetRestoredSideCnt(),
				c.verdict, c.restoredCn)
		}
	}
}

// TestCheckControllerNodeRestoreCountsRetiredSidesById pins the side id of
// each retired leg in the set of sides (architecture.md, Controller nodes,
// CheckControllerNodeRestore, MAY_BREAK): the agent takes it from the
// request's retired_leg_id_to_side_id and the check from the restored
// RetiredLeg records, so a take-back of a retired leg moves a side inside
// the set, which reads older, and a side made after the backup that a
// take-back's finish retired still raises the largest id. The held numbers
// are the agent's for the same held layouts (cnagent's
// TestStoredCntlrCountsRetiredSidesById). Each pool is held one revision
// ahead of the store; at the backup, A and B held legs 0x30 (side 0x31) and
// 0x34 (side 0x35) and the retired leg 0x32 (side 0x33):
//   - A: a migration moved 0x35 to 0x40 and its disk node freed 0x35, then
//     an operator's repair took back 0x32 and retired 0x34 with side 0x40;
//   - B: a repair put 0x40 (side 0x41) in place of 0x34, a take-back of 0x32
//     retired 0x40, and the retired leg 0x34 was deleted, 0x35 freed;
//   - C: from legs 0x30 and 0x32 (sides 0x31, 0x33) and the retired leg
//     0x36 (side 0x37), a take-back of 0x36 retired 0x32; nothing freed.
//
// A retired leg a counted group lists with no record reads RECORD_LOST, as
// the worker refuses to build the pool (dnv-worker.md MD3), and a held
// request that lacks a retired leg's side id, as one from a worker without
// the map, reads MAY_BREAK with held_side_ids_incomplete.
func TestCheckControllerNodeRestoreCountsRetiredSidesById(t *testing.T) {
	e := rcMakeEnv(t)
	const (
		spMigrTake  = uint64(0x901)
		spFreshTake = uint64(0x902)
		spTakeHigh  = uint64(0x903)
		spNoRecord  = uint64(0x904)
		spOldWorker = uint64(0x905)
	)
	slice := func(spId uint64) uint64 { return spId<<8 | 1 }
	for _, c := range []struct {
		spId uint64
		name string
	}{
		{spMigrTake, "migrtake"}, {spFreshTake, "freshtake"},
		{spNoRecord, "norecord"}, {spOldWorker, "oldworker"},
	} {
		e.putLayout(t, c.spId, c.name, slice(c.spId), []*pb.Group{
			{GrpId: 0x10, LegList: []*pb.Leg{
				rcLeg(0x30, 0x31), rcLeg(0x34, 0x35)},
				RetiredLegIdList: []uint64{0x32}},
		}, nil, nil)
	}
	e.putLayout(t, spTakeHigh, "takehigh", slice(spTakeHigh), []*pb.Group{
		{GrpId: 0x10, LegList: []*pb.Leg{
			rcLeg(0x30, 0x31), rcLeg(0x32, 0x33)},
			RetiredLegIdList: []uint64{0x36}},
	}, nil, nil)
	if err := e.s.cli.Delete(context.Background(),
		model.RetiredLegKey(e.cid, spNoRecord, 0x32)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	held := func(
		spId, maxSide, sideCnt uint64, missing bool,
	) *pb.GetCnStoreReply_StoredCntlr {
		return &pb.GetCnStoreReply_StoredCntlr{
			CntlrPointer:         rcPtr(spId, 1),
			Revision:             6,
			MaxSideId:            maxSide,
			SideCnt:              sideCnt,
			RetiredSideIdMissing: missing,
		}
	}
	e.agent.setCnStore(&pb.GetCnStoreReply{
		AgentReply: &pb.AgentReply{},
		CnList: []*pb.GetCnStoreReply_StoredCn{{
			CnId:     e.cnId,
			Revision: 3,
			CntlrPointerList: []*pb.CntlrPointer{
				rcPtr(spMigrTake, 1), rcPtr(spFreshTake, 1),
				rcPtr(spTakeHigh, 1), rcPtr(spNoRecord, 1),
				rcPtr(spOldWorker, 1),
			},
			CntlrList: []*pb.GetCnStoreReply_StoredCntlr{
				// 0x31, 0x33 and the retired 0x34's side 0x40.
				held(spMigrTake, 0x40, 3, false),
				// 0x31, 0x33 and the retired 0x40's side 0x41.
				held(spFreshTake, 0x41, 3, false),
				// 0x31, 0x37 and the retired 0x32's side 0x33.
				held(spTakeHigh, 0x37, 3, false),
				held(spNoRecord, 0x35, 3, false),
				// The restored layout, its retired leg without a side id:
				// the largest id may miss a side made after the backup.
				held(spOldWorker, 0x35, 3, true),
			},
		}},
	})
	reply, err := e.check(t)
	wantCode(t, err, codes.OK, "the restore check")
	for _, c := range []struct {
		label       string
		spId        uint64
		verdict     pb.RestoreVerdict
		restoredMax uint64
		incomplete  bool
	}{
		{"A: a migrated side retired by a take-back", spMigrTake,
			pb.RestoreVerdict_RESTORE_VERDICT_MAY_BREAK, 0x35, false},
		{"B: a new side retired by a take-back, a retired leg deleted",
			spFreshTake, pb.RestoreVerdict_RESTORE_VERDICT_MAY_BREAK, 0x35,
			false},
		{"C: a take-back of a retired leg with the largest side id",
			spTakeHigh, pb.RestoreVerdict_RESTORE_VERDICT_OLDER, 0x37,
			false},
		{"a retired leg without its record", spNoRecord,
			pb.RestoreVerdict_RESTORE_VERDICT_RECORD_LOST, 0, false},
		{"a held request without the retired side ids", spOldWorker,
			pb.RestoreVerdict_RESTORE_VERDICT_MAY_BREAK, 0x35, true},
	} {
		row := rcPoolOf(t, reply, c.spId, 1)
		if row.GetVerdict() != c.verdict ||
			row.GetRestoredMaxSideId() != c.restoredMax ||
			row.GetHeldSideIdsIncomplete() != c.incomplete {
			t.Errorf("%s: %v, restored largest %#x, incomplete %v; want "+
				"%v, %#x, %v", c.label, row.GetVerdict(),
				row.GetRestoredMaxSideId(), row.GetHeldSideIdsIncomplete(),
				c.verdict, c.restoredMax, c.incomplete)
		}
	}
}

// TestCheckControllerNodeRestoreGivesAHeldCntlrOfALostPoolItsRow pins
// RECORD_LOST for a cntlr the agent holds and the restored node names, of
// a pool the restored state names nowhere: the restored state lost the
// pool's records, and NEW would say it was made after the backup. One the
// restored node does not name is new.
func TestCheckControllerNodeRestoreGivesAHeldCntlrOfALostPoolItsRow(
	t *testing.T,
) {
	e := rcMakeEnv(t)
	const (
		spNamed = uint64(0x701)
		spNew   = uint64(0x702)
	)
	e.setRestoredPtrs(t, rcPtr(spNamed, 1))
	e.agent.setCnStore(&pb.GetCnStoreReply{
		AgentReply: &pb.AgentReply{},
		CnList: []*pb.GetCnStoreReply_StoredCn{{
			CnId:     e.cnId,
			Revision: 3,
			CntlrPointerList: []*pb.CntlrPointer{
				rcPtr(spNamed, 1), rcPtr(spNew, 1)},
			CntlrList: []*pb.GetCnStoreReply_StoredCntlr{
				{CntlrPointer: rcPtr(spNamed, 1), Revision: 7},
				{CntlrPointer: rcPtr(spNew, 1), Revision: 7},
			},
		}},
	})
	reply, err := e.check(t)
	wantCode(t, err, codes.OK, "the restore check")
	if got := rcPoolOf(t, reply, spNamed, 1).GetVerdict(); got !=
		pb.RestoreVerdict_RESTORE_VERDICT_RECORD_LOST {
		t.Errorf("a held cntlr the restored node names, its pool named "+
			"nowhere: %v, want RECORD_LOST", got)
	}
	if got := rcPoolOf(t, reply, spNew, 1).GetVerdict(); got !=
		pb.RestoreVerdict_RESTORE_VERDICT_NEW {
		t.Errorf("a held cntlr the restored node does not name: %v, "+
			"want NEW", got)
	}
}
