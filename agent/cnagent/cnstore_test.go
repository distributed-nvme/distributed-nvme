package cnagent

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestGetCnStoreAnswersTheHeldDesiredState pins CN32: GetCnStore answers,
// from the requests the agent holds, each CN's revision and pointer list
// and a summary of each held cntlr the list names — its revision, role,
// thin devices with their thin ids, NQNs and largest side id — a pointer
// whose cntlr the agent holds no request of in the pointer list only; a
// zero cn_id lists every CN of the cluster the agent holds and no other
// cluster's; a cluster it holds no CN of is an unknown object; and the call
// touches nothing of the node.
func TestGetCnStoreAnswersTheHeldDesiredState(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()

	reply, err := srv.GetCnStore(ctx, &pb.GetCnStoreRequest{
		ClusterId: testCluster})
	if err != nil {
		t.Fatalf("GetCnStore: %v", err)
	}
	if code := reply.GetAgentReply().GetCode(); code !=
		common.ReplyCodeUnknownObject || len(reply.GetCnList()) != 0 {
		t.Fatalf("an agent that holds nothing: code %d, %d CNs; want "+
			"unknown object and none", code, len(reply.GetCnList()))
	}

	syncupBoth(t, srv, reqOpts{revision: 3, primary: true, extraSide: true})
	// The node also names a cntlr the agent holds no request of.
	other := &pb.CntlrPointer{SpId: testSp + 1, CntlrId: 2}
	cnSyncupReq := cnReq(4, true)
	cnSyncupReq.CntlrPointerList = append(cnSyncupReq.CntlrPointerList,
		other)
	cnReply, err := srv.SyncupCn(ctx, cnSyncupReq)
	if err != nil || cnReply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCn with a second pointer: %v %v", err,
			cnReply.GetAgentReply())
	}
	// A second CN of the cluster, and a CN of another cluster.
	srv.putCn(cnKey(testCluster, testCn+1), newCnState(&pb.SyncupCnRequest{
		ClusterId: testCluster, CnId: testCn + 1, Revision: 9}))
	srv.putCn(cnKey(testCluster+1, testCn), newCnState(&pb.SyncupCnRequest{
		ClusterId: testCluster + 1, CnId: testCn, Revision: 5}))
	node.Reset()

	held := cntlrReq(reqOpts{revision: 3, primary: true, extraSide: true})
	var nqns []string
	for nqn := range held.GetNqnToSubsystem() {
		nqns = append(nqns, nqn)
	}
	slices.Sort(nqns)
	want := &pb.GetCnStoreReply{
		AgentReply: &pb.AgentReply{},
		CnList: []*pb.GetCnStoreReply_StoredCn{
			{
				CnId:             testCn,
				Revision:         4,
				CntlrPointerList: []*pb.CntlrPointer{cntlrPtr(), other},
				CntlrList: []*pb.GetCnStoreReply_StoredCntlr{{
					CntlrPointer: cntlrPtr(),
					Revision:     3,
					Primary:      true,
					TdList:       held.GetTdList(),
					NqnList:      nqns,
					MaxSideId: max(testMetaSide, testDataSide,
						testMetaSide+0x100, testDataSide+0x100),
					SideCnt: 4,
				}},
			},
			{CnId: testCn + 1, Revision: 9},
		},
	}
	reply, err = srv.GetCnStore(ctx, &pb.GetCnStoreRequest{
		ClusterId: testCluster})
	if err != nil {
		t.Fatalf("GetCnStore: %v", err)
	}
	if !proto.Equal(reply, want) {
		t.Errorf("every CN of the cluster:\n got %v\nwant %v", reply, want)
	}

	reply, err = srv.GetCnStore(ctx, &pb.GetCnStoreRequest{
		ClusterId: testCluster, CnId: testCn + 1})
	if err != nil {
		t.Fatalf("GetCnStore: %v", err)
	}
	if len(reply.GetCnList()) != 1 ||
		reply.GetCnList()[0].GetCnId() != testCn+1 {
		t.Errorf("one CN asked for: got %v, want cn %d alone",
			reply.GetCnList(), testCn+1)
	}

	reply, err = srv.GetCnStore(ctx, &pb.GetCnStoreRequest{
		ClusterId: testCluster + 2})
	if err != nil {
		t.Fatalf("GetCnStore: %v", err)
	}
	if reply.GetAgentReply().GetCode() != common.ReplyCodeUnknownObject {
		t.Errorf("a cluster the agent holds no CN of: code %d, want "+
			"unknown object", reply.GetAgentReply().GetCode())
	}
	if calls := node.Calls(); len(calls) != 0 {
		t.Errorf("GetCnStore touched the node: %v", calls)
	}
}

// TestGetCnStoreAnswersStartingWhileTheAgentStarts pins CN32's starting
// answer: while the startup pass runs, the states are still being filled,
// so the agent answers ReplyCodeStarting and no CN (dnagent.md SH33).
func TestGetCnStoreAnswersStartingWhileTheAgentStarts(t *testing.T) {
	node := newFakeNode()
	node.dirs[agent.NvmetRoot] = true
	srv := newCnServerOnPort(node, common.NvmetPortId)
	srv.Prepare(t.Context())
	reply, err := srv.GetCnStore(context.Background(),
		&pb.GetCnStoreRequest{ClusterId: testCluster})
	if err != nil {
		t.Fatalf("GetCnStore: %v", err)
	}
	if reply.GetAgentReply().GetCode() != common.ReplyCodeStarting ||
		len(reply.GetCnList()) != 0 {
		t.Fatalf("a starting agent: %v, want the starting code and no CN",
			reply)
	}
	reconcileForTest(t, srv)
}

// TestStoredCntlrCountsTheSidesOfTheLayout pins CN32's summary of a held
// cntlr's layout: the largest side id and the count of the sides of every
// leg of the slices' meta and data groups, of each clone's meta group and
// of each repair's new legs, and of each retired leg those groups list, by
// the side id the request carries for it; a retired leg with no side id in
// the request is counted, and marks the summary as missing one.
func TestStoredCntlrCountsTheSidesOfTheLayout(t *testing.T) {
	leg := func(sideIds ...uint64) *pb.Leg {
		l := &pb.Leg{LegId: sideIds[0] - 1}
		for _, id := range sideIds {
			l.SideList = append(l.SideList, &pb.Side{SideId: id})
		}
		return l
	}
	req := &pb.SyncupCntlrRequest{
		CntlrPointer: cntlrPtr(),
		Revision:     7,
		IdToSlice: map[string]*pb.Slice{
			"a": {
				MetaGrpList: []*pb.Group{{LegList: []*pb.Leg{leg(0x11)}}},
				DataGrpList: []*pb.Group{{
					LegList:          []*pb.Leg{leg(0x21, 0x22), leg(0x23)},
					RetiredLegIdList: []uint64{0x5, 0x6},
				}},
			},
			"b": {DataGrpList: []*pb.Group{{LegList: []*pb.Leg{leg(0x31)}}}},
		},
		CloneList: []*pb.Clone{{MetaGrp: &pb.Group{
			LegList:          []*pb.Leg{leg(0x41), leg(0x42)},
			RetiredLegIdList: []uint64{0x7},
		}}},
		NameToRepair: map[string]*pb.Repair{"r": {
			NewLegList: []*pb.Leg{leg(0x99)},
		}},
		RetiredLegIdToSideId: map[uint64]uint64{0x5: 0x15, 0x6: 0x16,
			0x7: 0x107},
	}
	got := storedCntlr(req)
	if got.GetMaxSideId() != 0x107 || got.GetSideCnt() != 11 ||
		got.GetRetiredSideIdMissing() {
		t.Errorf("largest side id %#x over %d sides (missing %v), want "+
			"0x107 over 11, none missing", got.GetMaxSideId(),
			got.GetSideCnt(), got.GetRetiredSideIdMissing())
	}
	delete(req.RetiredLegIdToSideId, 0x7)
	got = storedCntlr(req)
	if got.GetMaxSideId() != 0x99 || got.GetSideCnt() != 11 ||
		!got.GetRetiredSideIdMissing() {
		t.Errorf("a retired leg with no side id: largest %#x over %d "+
			"sides (missing %v), want 0x99 over 11, one missing",
			got.GetMaxSideId(), got.GetSideCnt(),
			got.GetRetiredSideIdMissing())
	}
}

// TestStoredCntlrCountsRetiredSidesById pins the third review's three held
// layouts (architecture.md, Controller nodes, CheckControllerNodeRestore,
// MAY_BREAK): a side made after the backup that a take-back's finish
// retired stays in the largest id, by its side id, and a take-back moves a
// side and keeps the numbers. At the backup the group held legs 0x30 (side
// 0x31) and 0x34 (side 0x35) and the retired leg 0x32 (side 0x33).
//   - A: a migration moved 0x35 to 0x40, then a take-back of 0x32 retired
//     leg 0x34 with side 0x40: largest 0x40 over three sides.
//   - B: a repair put the new leg 0x40 (side 0x41) in place of 0x34, a
//     take-back of 0x32 retired 0x40, and 0x34 was deleted: 0x41 over
//     three.
//   - C: from legs 0x31 and 0x33 and the retired leg 0x36 (side 0x37), a
//     take-back of 0x36 retired 0x32: the same three sides, 0x37 over
//     three.
func TestStoredCntlrCountsRetiredSidesById(t *testing.T) {
	leg := func(legId, sideId uint64) *pb.Leg {
		return &pb.Leg{LegId: legId, SideList: []*pb.Side{{SideId: sideId}}}
	}
	held := func(
		legs []*pb.Leg, retired map[uint64]uint64,
	) *pb.SyncupCntlrRequest {
		grp := &pb.Group{LegList: legs}
		for legId := range retired {
			grp.RetiredLegIdList = append(grp.RetiredLegIdList, legId)
		}
		return &pb.SyncupCntlrRequest{
			CntlrPointer: cntlrPtr(),
			IdToSlice: map[string]*pb.Slice{
				"a": {DataGrpList: []*pb.Group{grp}},
			},
			RetiredLegIdToSideId: retired,
		}
	}
	for _, c := range []struct {
		label  string
		req    *pb.SyncupCntlrRequest
		max    uint64
		sideCn uint64
	}{
		{"A", held([]*pb.Leg{leg(0x30, 0x31), leg(0x32, 0x33)},
			map[uint64]uint64{0x34: 0x40}), 0x40, 3},
		{"B", held([]*pb.Leg{leg(0x30, 0x31), leg(0x32, 0x33)},
			map[uint64]uint64{0x40: 0x41}), 0x41, 3},
		{"C", held([]*pb.Leg{leg(0x30, 0x31), leg(0x36, 0x37)},
			map[uint64]uint64{0x32: 0x33}), 0x37, 3},
	} {
		got := storedCntlr(c.req)
		if got.GetMaxSideId() != c.max || got.GetSideCnt() != c.sideCn {
			t.Errorf("%s: largest side id %#x over %d sides, want %#x "+
				"over %d", c.label, got.GetMaxSideId(), got.GetSideCnt(),
				c.max, c.sideCn)
		}
	}
}
