package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Harness: one fake agent behind the real §4 server interceptors on bufconn,
// exactly as main() wires it (common/interceptor_test.go's house pattern).
// ---------------------------------------------------------------------------

func newTestAgent(t *testing.T) *fakeAgent {
	t.Helper()
	agent, err := newFakeAgent(context.Background(), t.TempDir(), 4096)
	if err != nil {
		t.Fatalf("newFakeAgent: %v", err)
	}
	return agent
}

func startAgent(
	t *testing.T, agent *fakeAgent,
) (pb.DiskNodeAgentClient, pb.ControllerNodeAgentClient) {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(common.GrpcUnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(common.GrpcStreamServerInterceptor()),
	)
	pb.RegisterDiskNodeAgentServer(server, agent)
	pb.RegisterControllerNodeAgentServer(server, agent)
	go func() {
		if err := server.Serve(lis); err != nil {
			t.Logf("fake agent server stopped: %v", err)
		}
	}()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(
			func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		server.Stop()
		lis.Close()
	})
	return pb.NewDiskNodeAgentClient(conn), pb.NewControllerNodeAgentClient(conn)
}

// writeFile writes one of the agent's two files and stamps a strictly newer
// mtime, so the reload rules ("mtime changed" for behavior.json, "newer than
// the fake's own last write" for state.json) fire deterministically however
// coarse the filesystem's timestamps are.
func writeFile(t *testing.T, agent *fakeAgent, name, content string) {
	t.Helper()
	path := filepath.Join(agent.dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
}

func sidePtr(spId, legId, sideId uint64) *pb.SidePointer {
	return &pb.SidePointer{SpId: spId, LegId: legId, SideId: sideId}
}

func cntlrPtr(spId, cntlrId uint64) *pb.CntlrPointer {
	return &pb.CntlrPointer{SpId: spId, CntlrId: cntlrId}
}

func keysOf[V any](m map[uint64]V) []uint64 {
	keys := make([]uint64, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func wantKeys[V any](t *testing.T, name string, m map[uint64]V, want ...uint64) {
	t.Helper()
	got := keysOf(m)
	if want == nil {
		want = []uint64{}
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s keys = %v, want %v", name, got, want)
	}
}

// ---------------------------------------------------------------------------
// behavior.json parsing (§14.9)
// ---------------------------------------------------------------------------

func TestParseBehaviorBothStatusSpellings(t *testing.T) {
	parsed, err := parseBehavior([]byte(`{
	  "default": {"status": "OK"},
	  "objects": {
	    "dn": {"rows": {"meta_info": {"status": "PROVISIONING"}}},
	    "cn": {"status": "RES_STATUS_MISSING",
	           "rows": {"port_info": {"status": "res_status_error",
	                                  "details": "boom"}}},
	    "side 1:3:5": {"zeroed_ext_cnt": 0, "total_ext_cnt": 2},
	    "cntlr 1:1": {"thin_ok": true, "thin_missing_slices": [1, 2],
	                  "bm_idx_list": [0, 1], "hang": false,
	                  "drop_stream": true, "reply_code": 2,
	                  "rows": {"leg_id_to_leg.7": {"status": "ERROR",
	                                               "details": "probe timeout"}}}
	  }
	}`))
	if err != nil {
		t.Fatalf("parseBehavior: %v", err)
	}
	if parsed.Default.status != pb.ResStatus_RES_STATUS_OK {
		t.Errorf("default status = %v", parsed.Default.status)
	}
	dn := parsed.Objects["dn"]
	if got := dn.Rows["meta_info"].status; got !=
		pb.ResStatus_RES_STATUS_PROVISIONING {
		t.Errorf("dn meta_info status = %v", got)
	}
	cn := parsed.Objects["cn"]
	if cn.status != pb.ResStatus_RES_STATUS_MISSING {
		t.Errorf("cn status = %v", cn.status)
	}
	// The full RES_STATUS_* spelling must resolve to the same enum as the
	// short one (§14.9: "Accept BOTH").
	if got := cn.Rows["port_info"].status; got !=
		pb.ResStatus_RES_STATUS_ERROR {
		t.Errorf("cn port_info status = %v", got)
	}
	side := parsed.Objects["side 1:3:5"]
	if side.ZeroedExtCnt == nil || *side.ZeroedExtCnt != 0 {
		t.Errorf("side zeroed_ext_cnt = %v", side.ZeroedExtCnt)
	}
	if side.TotalExtCnt == nil || *side.TotalExtCnt != 2 {
		t.Errorf("side total_ext_cnt = %v", side.TotalExtCnt)
	}
	cntlr := parsed.Objects["cntlr 1:1"]
	if !cntlr.ThinOk || cntlr.Hang || !cntlr.DropStream ||
		cntlr.ReplyCode != 2 {
		t.Errorf("cntlr levers = %+v", cntlr)
	}
	if !slices.Equal(cntlr.ThinMissingSlices, []uint64{1, 2}) {
		t.Errorf("thin_missing_slices = %v", cntlr.ThinMissingSlices)
	}
	if cntlr.BmIdxList == nil || !slices.Equal(*cntlr.BmIdxList,
		[]uint32{0, 1}) {
		t.Errorf("bm_idx_list = %v", cntlr.BmIdxList)
	}
}

func TestParseBehaviorMalformed(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{"broken json", `{"default": {`},
		{"unknown field", `{"objects": {"dn": {"reply-code": 2}}}`},
		{"unknown status", `{"objects": {"dn": {"status": "BROKEN"}}}`},
		{"unknown row status",
			`{"objects": {"dn": {"rows": {"disk_info": {"status": "nope"}}}}}`},
		{"wrong type", `{"objects": {"dn": {"thin_ok": "yes"}}}`},
		{"trailing data", `{} {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseBehavior([]byte(tc.data)); err == nil {
				t.Fatalf("parseBehavior(%q) succeeded, want an error", tc.data)
			}
		})
	}
}

// TestBehaviorReloadKeepsPreviousOnMalformed is §14.9's "a malformed
// behavior.json must be logged and IGNORED": the agent must keep answering
// with the last good behaviour instead of dying.
func TestBehaviorReloadKeepsPreviousOnMalformed(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, _ := startAgent(t, agent)
	ctx := context.Background()

	if _, err := dnClient.SyncupDn(ctx, &pb.SyncupDnRequest{
		ClusterId: 1, DnId: 1, Revision: 1,
	}); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}

	writeFile(t, agent, behaviorFileName,
		`{"objects": {"dn": {"rows": {"disk_info": {"status": "ERROR",
		 "details": "io"}}}}}`)
	reply, err := dnClient.GetDnInfo(ctx, &pb.GetDnInfoRequest{
		ClusterId: 1, DnId: 1,
	})
	if err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	if got := reply.GetDnInfo().GetDiskInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR || got.GetDetails() != "io" {
		t.Fatalf("disk_info = %v", got)
	}

	writeFile(t, agent, behaviorFileName, `{"objects": {"dn": {`)
	reply, err = dnClient.GetDnInfo(ctx, &pb.GetDnInfoRequest{
		ClusterId: 1, DnId: 1,
	})
	if err != nil {
		t.Fatalf("GetDnInfo after the malformed file: %v", err)
	}
	if got := reply.GetDnInfo().GetDiskInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR || got.GetDetails() != "io" {
		t.Fatalf("disk_info after the malformed file = %v, want the "+
			"previous behaviour", got)
	}

	// An absent file means {}: every row OK again.
	if err := os.Remove(
		filepath.Join(agent.dir, behaviorFileName)); err != nil {
		t.Fatalf("removing behavior.json: %v", err)
	}
	reply, err = dnClient.GetDnInfo(ctx, &pb.GetDnInfoRequest{
		ClusterId: 1, DnId: 1,
	})
	if err != nil {
		t.Fatalf("GetDnInfo after the removal: %v", err)
	}
	if got := reply.GetDnInfo().GetDiskInfo().GetStatus(); got !=
		pb.ResStatus_RES_STATUS_OK {
		t.Fatalf("disk_info status after the removal = %v, want OK", got)
	}
}

// ---------------------------------------------------------------------------
// The revision gate (§14.9)
// ---------------------------------------------------------------------------

// syncupDn is the fixture every side test needs first: the DN must know the
// side pointer before a SyncupSide is anything but ReplyCodeUnknownObject.
func syncupDn(
	t *testing.T, client pb.DiskNodeAgentClient,
	revision uint64, ptrList ...*pb.SidePointer,
) *pb.SyncupDnReply {
	t.Helper()
	reply, err := client.SyncupDn(context.Background(), &pb.SyncupDnRequest{
		ClusterId:       1,
		DnId:            1,
		Revision:        revision,
		SidePointerList: ptrList,
		ExtentSize:      67108864,
	})
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	return reply
}

func syncupSide(
	t *testing.T, client pb.DiskNodeAgentClient,
	ptr *pb.SidePointer, revision uint64, migrId uint64,
) *pb.SyncupSideReply {
	t.Helper()
	req := &pb.SyncupSideRequest{
		ClusterId:   1,
		DnId:        1,
		SidePointer: ptr,
		Revision:    revision,
		SideConf: &pb.SyncupSideRequest_SideConf{
			ExtCnt:        2,
			PrimaryCnId:   1,
			StandbyIdList: []uint64{2, 3},
		},
	}
	if migrId != 0 {
		req.MigrDstConf = &pb.SyncupSideRequest_MigrDstConf{
			MigrId: migrId,
			BmCnt:  2,
		}
	}
	reply, err := client.SyncupSide(context.Background(), req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	return reply
}

// TestGateStaleSyncupRevision covers §14.9's first rejection: a Syncup* whose
// revision is lower than the stored one is ReplyCodeStaleRevision, and the
// reply still reports the agent's own (higher) revision.
func TestGateStaleSyncupRevision(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, cnClient := startAgent(t, agent)

	if code := syncupDn(t, dnClient, 2).GetAgentReply().GetCode(); code != 0 {
		t.Fatalf("SyncupDn revision 2 code = %d", code)
	}
	reply := syncupDn(t, dnClient, 1)
	if got := reply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeStaleRevision {
		t.Fatalf("SyncupDn revision 1 code = %d, want %d",
			got, common.ReplyCodeStaleRevision)
	}
	if reply.GetRevision() != 2 {
		t.Errorf("rejected reply revision = %d, want 2", reply.GetRevision())
	}
	// Equal revisions re-apply (workers retry), architecture.md §9.1.
	if code := syncupDn(t, dnClient, 2).GetAgentReply().GetCode(); code != 0 {
		t.Errorf("SyncupDn at the equal revision code = %d, want 0", code)
	}

	cnReply, err := cnClient.SyncupCn(context.Background(), &pb.SyncupCnRequest{
		ClusterId: 1, CnId: 1, Revision: 4,
	})
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if cnReply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCn code = %d", cnReply.GetAgentReply().GetCode())
	}
	cnReply, err = cnClient.SyncupCn(context.Background(), &pb.SyncupCnRequest{
		ClusterId: 1, CnId: 1, Revision: 3,
	})
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if got := cnReply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeStaleRevision {
		t.Fatalf("stale SyncupCn code = %d, want %d",
			got, common.ReplyCodeStaleRevision)
	}
}

// TestGateStalePushRevision covers §14.9's second rejection: a Push* with a
// revision lower than the object's stored revision is
// ReplyCodeStaleRevision, and the chunk is not recorded.
func TestGateStalePushRevision(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, _ := startAgent(t, agent)
	ptr := sidePtr(1, 3, 5)
	syncupDn(t, dnClient, 2, ptr)
	syncupSide(t, dnClient, ptr, 2, 30)

	reply, err := dnClient.PushMigrBitmap(context.Background(),
		&pb.PushMigrBitmapRequest{
			ClusterId:   1,
			DnId:        1,
			SidePointer: ptr,
			Revision:    1,
			MigrId:      30,
			BmIdx:       0,
			Bitmap:      []byte{0xff, 0x00},
		})
	if err != nil {
		t.Fatalf("PushMigrBitmap: %v", err)
	}
	if got := reply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeStaleRevision {
		t.Fatalf("stale push code = %d, want %d",
			got, common.ReplyCodeStaleRevision)
	}
	if got := syncupSide(t, dnClient, ptr, 2, 30).GetBmInfo().
		GetBmIdxList(); len(got) != 0 {
		t.Fatalf("bm_idx_list = %v after a rejected push, want none", got)
	}
}

// TestGateUnknownPushId covers §14.9's third rejection: a Push* naming a
// migr_id/clone_id absent from the object's last applied request is
// ReplyCodeUnknownObject.
func TestGateUnknownPushId(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, cnClient := startAgent(t, agent)
	ctx := context.Background()

	ptr := sidePtr(1, 3, 5)
	syncupDn(t, dnClient, 2, ptr)
	syncupSide(t, dnClient, ptr, 2, 30)

	reply, err := dnClient.PushMigrBitmap(ctx, &pb.PushMigrBitmapRequest{
		ClusterId: 1, DnId: 1, SidePointer: ptr, Revision: 2,
		MigrId: 99, BmIdx: 0, Bitmap: []byte{0xff},
	})
	if err != nil {
		t.Fatalf("PushMigrBitmap: %v", err)
	}
	if got := reply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeUnknownObject {
		t.Fatalf("unknown migr_id code = %d, want %d",
			got, common.ReplyCodeUnknownObject)
	}

	// The known id is accepted and lands in the derived applied set.
	if _, err := dnClient.PushMigrBitmap(ctx, &pb.PushMigrBitmapRequest{
		ClusterId: 1, DnId: 1, SidePointer: ptr, Revision: 2,
		MigrId: 30, BmIdx: 1, Bitmap: []byte{0xff, 0xff},
	}); err != nil {
		t.Fatalf("PushMigrBitmap: %v", err)
	}
	if _, err := dnClient.PushMigrBitmap(ctx, &pb.PushMigrBitmapRequest{
		ClusterId: 1, DnId: 1, SidePointer: ptr, Revision: 2,
		MigrId: 30, BmIdx: 0, Bitmap: []byte{0xff, 0xff},
	}); err != nil {
		t.Fatalf("PushMigrBitmap: %v", err)
	}
	bmInfo := syncupSide(t, dnClient, ptr, 2, 30).GetBmInfo()
	if bmInfo.GetResId() != 30 ||
		!slices.Equal(bmInfo.GetBmIdxList(), []uint32{0, 1}) {
		t.Fatalf("bm_info = %v, want res_id 30 and [0 1]", bmInfo)
	}

	// The cn twin: a clone_id absent from the last SyncupCntlr.
	cPtr := cntlrPtr(1, 1)
	if _, err := cnClient.SyncupCn(ctx, &pb.SyncupCnRequest{
		ClusterId: 1, CnId: 1, Revision: 1,
		CntlrPointerList: []*pb.CntlrPointer{cPtr},
	}); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if _, err := cnClient.SyncupCntlr(ctx, &pb.SyncupCntlrRequest{
		ClusterId: 1, CnId: 1, CntlrPointer: cPtr, Revision: 1,
		CloneList: []*pb.Clone{{CloneId: 41}},
	}); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	cloneReply, err := cnClient.PushCloneBitmap(ctx,
		&pb.PushCloneBitmapRequest{
			ClusterId: 1, CnId: 1, CntlrPointer: cPtr, Revision: 1,
			CloneId: 42, BmIdx: 0, Bitmap: []byte{0x01},
		})
	if err != nil {
		t.Fatalf("PushCloneBitmap: %v", err)
	}
	if got := cloneReply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeUnknownObject {
		t.Fatalf("unknown clone_id code = %d, want %d",
			got, common.ReplyCodeUnknownObject)
	}
}

// TestGateOrderingUnknownChild covers §14.9's fourth rejection, the real
// agents' ordering rule the worker's independent roles must survive: a side
// absent from the DN's last SyncupDn — or a cntlr absent from the CN's last
// SyncupCn — is ReplyCodeUnknownObject on Syncup*, Check* and Get*Info alike.
func TestGateOrderingUnknownChild(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, cnClient := startAgent(t, agent)
	ctx := context.Background()
	ptr := sidePtr(1, 3, 5)

	// No SyncupDn at all yet.
	if got := syncupSide(t, dnClient, ptr, 1, 0).GetAgentReply().
		GetCode(); got != common.ReplyCodeUnknownObject {
		t.Fatalf("SyncupSide before SyncupDn code = %d, want %d",
			got, common.ReplyCodeUnknownObject)
	}

	// A SyncupDn that does not list this pointer.
	syncupDn(t, dnClient, 1, sidePtr(1, 3, 6))
	if got := syncupSide(t, dnClient, ptr, 1, 0).GetAgentReply().
		GetCode(); got != common.ReplyCodeUnknownObject {
		t.Fatalf("SyncupSide for an unlisted pointer code = %d, want %d",
			got, common.ReplyCodeUnknownObject)
	}

	stream, err := dnClient.CheckSide(ctx)
	if err != nil {
		t.Fatalf("CheckSide: %v", err)
	}
	if err := stream.Send(&pb.CheckSideRequest{
		ClusterId: 1, DnId: 1, SidePointer: ptr, Revision: 1,
	}); err != nil {
		t.Fatalf("CheckSide send: %v", err)
	}
	checkReply, err := stream.Recv()
	if err != nil {
		t.Fatalf("CheckSide recv: %v", err)
	}
	if got := checkReply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeUnknownObject {
		t.Fatalf("CheckSide for an unlisted pointer code = %d, want %d",
			got, common.ReplyCodeUnknownObject)
	}
	if checkReply.GetSideInfo() != nil || checkReply.GetRevision() != 0 {
		t.Errorf("unknown CheckSide reply = %v, want no info and revision 0",
			checkReply)
	}
	stream.CloseSend()

	// Listing it makes the very same request apply.
	syncupDn(t, dnClient, 2, ptr)
	if got := syncupSide(t, dnClient, ptr, 2, 0).GetAgentReply().
		GetCode(); got != 0 {
		t.Fatalf("SyncupSide for a listed pointer code = %d, want 0", got)
	}
	// ... and dropping it again forgets the side (architecture.md §9.1).
	syncupDn(t, dnClient, 3)
	if got := syncupSide(t, dnClient, ptr, 3, 0).GetAgentReply().
		GetCode(); got != common.ReplyCodeUnknownObject {
		t.Fatalf("SyncupSide after the pointer was dropped code = %d, want %d",
			got, common.ReplyCodeUnknownObject)
	}

	// The cn twin.
	cPtr := cntlrPtr(1, 1)
	cntlrReply, err := cnClient.SyncupCntlr(ctx, &pb.SyncupCntlrRequest{
		ClusterId: 1, CnId: 1, CntlrPointer: cPtr, Revision: 1,
	})
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if got := cntlrReply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeUnknownObject {
		t.Fatalf("SyncupCntlr before SyncupCn code = %d, want %d",
			got, common.ReplyCodeUnknownObject)
	}
}

// TestForcedReplyCodeRejects covers behavior.json's reply_code lever: it
// forces agent_reply.code on every reply of the object and, because a
// rejected request must not have been applied, suppresses the apply — §14.11
// case C step 6 clears the lever and expects the push to still be missing.
func TestForcedReplyCodeRejects(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, _ := startAgent(t, agent)
	ptr := sidePtr(1, 3, 5)
	syncupDn(t, dnClient, 1, ptr)
	syncupSide(t, dnClient, ptr, 1, 30)

	writeFile(t, agent, behaviorFileName,
		`{"objects": {"side 1:3:5": {"reply_code": 1}}}`)
	pushReply, err := dnClient.PushMigrBitmap(context.Background(),
		&pb.PushMigrBitmapRequest{
			ClusterId: 1, DnId: 1, SidePointer: ptr, Revision: 1,
			MigrId: 30, BmIdx: 0, Bitmap: []byte{0x01},
		})
	if err != nil {
		t.Fatalf("PushMigrBitmap: %v", err)
	}
	if got := pushReply.GetAgentReply().GetCode(); got != 1 {
		t.Fatalf("forced push code = %d, want 1", got)
	}

	writeFile(t, agent, behaviorFileName, `{}`)
	if got := syncupSide(t, dnClient, ptr, 1, 30).GetBmInfo().
		GetBmIdxList(); len(got) != 0 {
		t.Fatalf("bm_idx_list = %v, want the forced push not to have "+
			"been applied", got)
	}
}

// ---------------------------------------------------------------------------
// *Info derivation (§14.9)
// ---------------------------------------------------------------------------

// cntlrFixture is the §14.11-shaped SyncupCntlrRequest the derivation tests
// assert against: two slices, a meta and a data group with a spare leg each,
// two tds, one subsystem with two namespaces, one clone and one xfer.
func cntlrFixture(primary bool) *pb.SyncupCntlrRequest {
	return &pb.SyncupCntlrRequest{
		ClusterId:    1,
		CnId:         1,
		CntlrPointer: cntlrPtr(1, 1),
		Revision:     5,
		Cntlr:        &pb.Cntlr{CntlidSlot: 0, Primary: primary},
		IdToSlice: map[string]*pb.Slice{
			fmt.Sprintf(common.IdKeyFmt, 1): {
				SliceIdx: 0,
				MetaGrpList: []*pb.Group{{
					GrpId:   11,
					ExtCnt:  1,
					LegList: []*pb.Leg{{LegId: 101, LegIdx: 0}},
					SpareLegList: []*pb.Leg{
						{LegId: 102, LegIdx: 1},
					},
				}},
				DataGrpList: []*pb.Group{{
					GrpId:  12,
					ExtCnt: 2,
					LegList: []*pb.Leg{
						{LegId: 103, LegIdx: 0},
						{LegId: 104, LegIdx: 1},
					},
				}},
			},
			fmt.Sprintf(common.IdKeyFmt, 2): {
				SliceIdx: 1,
				DataGrpList: []*pb.Group{{
					GrpId:        21,
					ExtCnt:       2,
					LegList:      []*pb.Leg{{LegId: 201, LegIdx: 0}},
					SpareLegList: []*pb.Leg{{LegId: 202, LegIdx: 1}},
				}},
			},
		},
		TdList: []*pb.ThinDevice{
			{TdId: 7, DevId: 1, Size: 1 << 30},
			{TdId: 8, DevId: 2, Size: 1 << 30},
		},
		NqnToSubsystem: map[string]*pb.Subsystem{
			"nqn.2024-01.io.dnv-it:ss0": {
				SsId: 3,
				NsList: []*pb.Namespace{
					{NsId: 31, NsIdx: 1, TdId: 7},
					{NsId: 32, NsIdx: 2, TdId: 8},
				},
			},
		},
		CloneList: []*pb.Clone{{CloneId: 41, DstTdId: 7}},
		XferList:  []*pb.Transfer{{XferId: 51}},
	}
}

// syncupCntlrFixture brings the CN and the cntlr up to the fixture state and
// returns the SyncupCntlr reply.
func syncupCntlrFixture(
	t *testing.T, cnClient pb.ControllerNodeAgentClient, primary bool,
) *pb.SyncupCntlrReply {
	t.Helper()
	ctx := context.Background()
	if _, err := cnClient.SyncupCn(ctx, &pb.SyncupCnRequest{
		ClusterId: 1, CnId: 1, Revision: 1,
		CntlrPointerList: []*pb.CntlrPointer{cntlrPtr(1, 1)},
	}); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	reply, err := cnClient.SyncupCntlr(ctx, cntlrFixture(primary))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCntlr code = %d (%s)",
			reply.GetAgentReply().GetCode(),
			reply.GetAgentReply().GetDetails())
	}
	return reply
}

func TestCntlrInfoDerivation(t *testing.T) {
	agent := newTestAgent(t)
	_, cnClient := startAgent(t, agent)
	writeFile(t, agent, behaviorFileName, `{
	  "objects": {"cntlr 1:1": {"rows": {
	    "leg_id_to_leg.103": {"status": "ERROR", "details": "probe timeout"},
	    "slice_id_to_dm_pool.1": {"details": "0 8192 thin-pool 5 40/1024"}
	  }}}
	}`)

	reply := syncupCntlrFixture(t, cnClient, true)
	info := reply.GetCntlrInfo()
	if info == nil {
		t.Fatal("cntlr_info is nil")
	}
	if reply.GetRevision() != 5 {
		t.Errorf("revision = %d, want 5", reply.GetRevision())
	}

	wantKeys(t, "slice_id_to_dm_pool", info.GetSliceIdToDmPool(), 1, 2)
	wantKeys(t, "slice_id_to_meta", info.GetSliceIdToMeta(), 1, 2)
	wantKeys(t, "slice_id_to_data", info.GetSliceIdToData(), 1, 2)
	wantKeys(t, "grp_id_to_md_raid", info.GetGrpIdToMdRaid(), 11, 12, 21)
	// One row per leg AND per SPARE leg of every group (§14.9): 102 and 202
	// are the spares, and §14.11 case D step 7 reads exactly them.
	wantKeys(t, "leg_id_to_leg", info.GetLegIdToLeg(),
		101, 102, 103, 104, 201, 202)
	wantKeys(t, "td_id_to_raid0", info.GetTdIdToRaid0(), 7, 8)
	wantKeys(t, "td_id_to_dm_error", info.GetTdIdToDmError(), 7, 8)
	wantKeys(t, "ss_id_to_subsystem", info.GetSsIdToSubsystem(), 3)
	wantKeys(t, "ns_id_to_namespace", info.GetNsIdToNamespace(), 31, 32)
	wantKeys(t, "ns_id_to_dm_linear", info.GetNsIdToDmLinear(), 31, 32)
	wantKeys(t, "clone_id_to_target", info.GetCloneIdToTarget(), 41)
	wantKeys(t, "clone_id_to_dm_clone", info.GetCloneIdToDmClone(), 41)
	wantKeys(t, "clone_id_to_meta", info.GetCloneIdToMeta(), 41)
	wantKeys(t, "xfer_id_to_dm_linear", info.GetXferIdToDmLinear(), 51)
	wantKeys(t, "xfer_id_to_subsystem", info.GetXferIdToSubsystem(), 51)
	wantKeys(t, "xfer_id_to_namespace", info.GetXferIdToNamespace(), 51)
	// thin_ok is absent, so the thin map stays empty even on the primary.
	wantKeys(t, "td_id_to_thin_info", info.GetTdIdToThinInfo())

	leg := info.GetLegIdToLeg()[103]
	if leg.GetResName() != "leg_id_to_leg.103" ||
		leg.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		leg.GetDetails() != "probe timeout" {
		t.Errorf("leg_id_to_leg.103 = %v", leg)
	}
	if leg.GetEpoch() == 0 {
		t.Errorf("leg_id_to_leg.103 epoch = 0, want the change time")
	}
	pool := info.GetSliceIdToDmPool()[1]
	if pool.GetStatus() != pb.ResStatus_RES_STATUS_OK ||
		pool.GetDetails() != "0 8192 thin-pool 5 40/1024" {
		t.Errorf("slice_id_to_dm_pool.1 = %v, want an OK row with details",
			pool)
	}
	if got := info.GetLegIdToLeg()[101].GetStatus(); got !=
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("leg_id_to_leg.101 status = %v, want the default OK", got)
	}

	// bm_info_list: one entry per clone, res_id = clone_id (§9.6).
	if len(reply.GetBmInfoList()) != 1 ||
		reply.GetBmInfoList()[0].GetResId() != 41 {
		t.Errorf("bm_info_list = %v, want one entry for clone 41",
			reply.GetBmInfoList())
	}
}

func TestCntlrInfoThinRows(t *testing.T) {
	for _, tc := range []struct {
		name       string
		primary    bool
		behavior   string
		wantTds    []uint64
		wantSlices []uint64
	}{
		{
			name:     "primary without thin_ok",
			primary:  true,
			behavior: `{"objects": {"cntlr 1:1": {}}}`,
			wantTds:  nil,
		},
		{
			name:     "standby with thin_ok",
			primary:  false,
			behavior: `{"objects": {"cntlr 1:1": {"thin_ok": true}}}`,
			wantTds:  nil,
		},
		{
			name:       "primary with thin_ok",
			primary:    true,
			behavior:   `{"objects": {"cntlr 1:1": {"thin_ok": true}}}`,
			wantTds:    []uint64{7, 8},
			wantSlices: []uint64{1, 2},
		},
		{
			name:    "primary with a missing slice",
			primary: true,
			behavior: `{"objects": {"cntlr 1:1": {"thin_ok": true,
			            "thin_missing_slices": [2]}}}`,
			wantTds:    []uint64{7, 8},
			wantSlices: []uint64{1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := newTestAgent(t)
			_, cnClient := startAgent(t, agent)
			writeFile(t, agent, behaviorFileName, tc.behavior)
			info := syncupCntlrFixture(t, cnClient, tc.primary).GetCntlrInfo()
			wantKeys(t, "td_id_to_thin_info", info.GetTdIdToThinInfo(),
				tc.wantTds...)
			for _, tdId := range tc.wantTds {
				thin := info.GetTdIdToThinInfo()[tdId]
				wantKeys(t,
					fmt.Sprintf("td_id_to_thin_info.%d.slice_id_to_dm_thin",
						tdId),
					thin.GetSliceIdToDmThin(), tc.wantSlices...)
				for _, sliceId := range tc.wantSlices {
					row := thin.GetSliceIdToDmThin()[sliceId]
					want := fmt.Sprintf("td_id_to_thin_info.%d.%d",
						tdId, sliceId)
					if row.GetResName() != want {
						t.Errorf("res_name = %q, want %q",
							row.GetResName(), want)
					}
					if row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
						t.Errorf("%s status = %v, want OK",
							want, row.GetStatus())
					}
				}
			}
		})
	}
}

// TestSideInfoDerivation asserts the §14.9 SideInfo shape: one row per
// per-CN export stack, the migration roles only when their conf rode along,
// and the §9.4 counters (total = ext_cnt, zeroed = total by default).
func TestSideInfoDerivation(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, _ := startAgent(t, agent)
	ptr := sidePtr(1, 3, 5)
	syncupDn(t, dnClient, 1, ptr)

	info := syncupSide(t, dnClient, ptr, 1, 0).GetSideInfo()
	if info == nil {
		t.Fatal("side_info is nil")
	}
	wantKeys(t, "cn_id_to_dm_error", info.GetCnIdToDmError(), 1, 2, 3)
	wantKeys(t, "cn_id_to_dm_linear", info.GetCnIdToDmLinear(), 1, 2, 3)
	wantKeys(t, "cn_id_to_nvmeof", info.GetCnIdToNvmeof(), 1, 2, 3)
	if info.GetMigrSrcInfo() != nil || info.GetMigrDstInfo() != nil {
		t.Errorf("migr info = %v/%v without a conf",
			info.GetMigrSrcInfo(), info.GetMigrDstInfo())
	}
	if info.GetTotalExtCnt() != 2 || info.GetZeroedExtCnt() != 2 {
		t.Errorf("counters = %d/%d, want the instant-zeroing default 2/2",
			info.GetZeroedExtCnt(), info.GetTotalExtCnt())
	}
	if got := info.GetSideDevInfo().GetResName(); got != "side_dev_info" {
		t.Errorf("side_dev_info res_name = %q", got)
	}

	// The destination role and a partial-zeroing override (§14.11 case F
	// step 2 holds a side unprovisioned exactly this way).
	writeFile(t, agent, behaviorFileName, `{
	  "objects": {"side 1:3:5": {"zeroed_ext_cnt": 0,
	    "rows": {"side_dev_info": {"status": "PROVISIONING"}}}}
	}`)
	info = syncupSide(t, dnClient, ptr, 1, 30).GetSideInfo()
	if info.GetMigrDstInfo().GetTargetInfo() == nil ||
		info.GetMigrDstInfo().GetDmCloneInfo() == nil {
		t.Errorf("migr_dst_info = %v, want both rows",
			info.GetMigrDstInfo())
	}
	if info.GetZeroedExtCnt() != 0 || info.GetTotalExtCnt() != 2 {
		t.Errorf("counters = %d/%d, want 0/2",
			info.GetZeroedExtCnt(), info.GetTotalExtCnt())
	}
	if got := info.GetSideDevInfo().GetStatus(); got !=
		pb.ResStatus_RES_STATUS_PROVISIONING {
		t.Errorf("side_dev_info status = %v, want PROVISIONING", got)
	}
}

func TestDnAndCnInfoRows(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, cnClient := startAgent(t, agent)
	ctx := context.Background()

	// Nothing applied yet: no info, revision 0, so the worker re-syncs.
	dnInfo, err := dnClient.GetDnInfo(ctx, &pb.GetDnInfoRequest{
		ClusterId: 1, DnId: 1,
	})
	if err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	if dnInfo.GetDnInfo() != nil || dnInfo.GetRevision() != 0 {
		t.Errorf("GetDnInfo before any syncup = %v", dnInfo)
	}

	syncupDn(t, dnClient, 1)
	dnInfo, err = dnClient.GetDnInfo(ctx, &pb.GetDnInfoRequest{
		ClusterId: 1, DnId: 1,
	})
	if err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	got := dnInfo.GetDnInfo()
	if got.GetDiskInfo().GetResName() != "disk_info" ||
		got.GetMetaInfo().GetResName() != "meta_info" ||
		got.GetPortInfo().GetResName() != "port_info" {
		t.Errorf("dn_info = %v", got)
	}

	if _, err := cnClient.SyncupCn(ctx, &pb.SyncupCnRequest{
		ClusterId: 1, CnId: 1, Revision: 1,
	}); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	cnInfo, err := cnClient.GetCnInfo(ctx, &pb.GetCnInfoRequest{
		ClusterId: 1, CnId: 1,
	})
	if err != nil {
		t.Fatalf("GetCnInfo: %v", err)
	}
	cn := cnInfo.GetCnInfo()
	if cn.GetPortInfo().GetResName() != "port_info" ||
		cn.GetTmpfsInfo().GetResName() != "tmpfs_info" ||
		cn.GetTmpFileInfo().GetResName() != "tmp_file_info" ||
		cn.GetLoopDevInfo().GetResName() != "loop_dev_info" {
		t.Errorf("cn_info = %v", cn)
	}
}

// ---------------------------------------------------------------------------
// The Check streams (§14.9, architecture.md §9.7)
// ---------------------------------------------------------------------------

// TestCheckDnChangeOnlyInfo is §14.11 case S step 2's "show_info false after
// the first": the first reply on a fresh stream always carries the full
// info, show_info = false carries it only when something changed, and
// show_info = true always carries it.
func TestCheckDnChangeOnlyInfo(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, _ := startAgent(t, agent)
	ctx := context.Background()
	syncupDn(t, dnClient, 1)

	stream, err := dnClient.CheckDn(ctx)
	if err != nil {
		t.Fatalf("CheckDn: %v", err)
	}
	round := func(showInfo bool) *pb.CheckDnReply {
		t.Helper()
		if err := stream.Send(&pb.CheckDnRequest{
			ClusterId: 1, DnId: 1, Revision: 1, ShowInfo: showInfo,
		}); err != nil {
			t.Fatalf("CheckDn send: %v", err)
		}
		reply, err := stream.Recv()
		if err != nil {
			t.Fatalf("CheckDn recv: %v", err)
		}
		if reply.GetRevision() != 1 {
			t.Errorf("reply revision = %d, want 1", reply.GetRevision())
		}
		return reply
	}

	if round(false).GetDnInfo() == nil {
		t.Fatal("the first reply on a fresh stream carries no dn_info")
	}
	if got := round(false).GetDnInfo(); got != nil {
		t.Fatalf("an unchanged round carries dn_info = %v", got)
	}
	if round(true).GetDnInfo() == nil {
		t.Fatal("show_info = true carries no dn_info")
	}
	if got := round(false).GetDnInfo(); got != nil {
		t.Fatalf("an unchanged round after show_info = true carries %v", got)
	}

	writeFile(t, agent, behaviorFileName,
		`{"objects": {"dn": {"rows": {"disk_info": {"status": "ERROR",
		 "details": "io"}}}}}`)
	changed := round(false).GetDnInfo()
	if changed == nil {
		t.Fatal("a changed round carries no dn_info")
	}
	if changed.GetDiskInfo().GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Errorf("disk_info = %v, want ERROR", changed.GetDiskInfo())
	}
	if got := round(false).GetDnInfo(); got != nil {
		t.Fatalf("the round after the change carries %v", got)
	}

	// A fresh stream always starts with the full info again.
	stream2, err := dnClient.CheckDn(ctx)
	if err != nil {
		t.Fatalf("CheckDn: %v", err)
	}
	if err := stream2.Send(&pb.CheckDnRequest{
		ClusterId: 1, DnId: 1, Revision: 1,
	}); err != nil {
		t.Fatalf("CheckDn send: %v", err)
	}
	reply, err := stream2.Recv()
	if err != nil {
		t.Fatalf("CheckDn recv: %v", err)
	}
	if reply.GetDnInfo() == nil {
		t.Fatal("the first reply on the second stream carries no dn_info")
	}
	stream.CloseSend()
	stream2.CloseSend()
}

// TestCheckCntlrHang is §14.9's hang lever: the round is accepted and not
// answered while the lever is set, so the worker's round times out (§14.11
// case B step 4), and the handler ends promptly both when the worker gives up
// on the stream and when the script clears the lever.
func TestCheckCntlrHang(t *testing.T) {
	agent := newTestAgent(t)
	_, cnClient := startAgent(t, agent)
	syncupCntlrFixture(t, cnClient, true)
	writeFile(t, agent, behaviorFileName,
		`{"objects": {"cntlr 1:1": {"hang": true}}}`)

	sendRound := func(
		stream grpc.BidiStreamingClient[pb.CheckCntlrRequest, pb.CheckCntlrReply],
	) chan error {
		t.Helper()
		if err := stream.Send(&pb.CheckCntlrRequest{
			ClusterId: 1, CnId: 1, CntlrPointer: cntlrPtr(1, 1), Revision: 5,
		}); err != nil {
			t.Fatalf("CheckCntlr send: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := stream.Recv()
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("a hanging round replied: %v", err)
		case <-time.After(2 * hangPollInterval):
		}
		return done
	}

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := cnClient.CheckCntlr(ctx)
	if err != nil {
		t.Fatalf("CheckCntlr: %v", err)
	}
	done := sendRound(stream)

	// Another object's stream must stay live while the cntlr hangs.
	other, err := cnClient.CheckCn(context.Background())
	if err != nil {
		t.Fatalf("CheckCn: %v", err)
	}
	if err := other.Send(&pb.CheckCnRequest{
		ClusterId: 1, CnId: 1, Revision: 1,
	}); err != nil {
		t.Fatalf("CheckCn send: %v", err)
	}
	if _, err := other.Recv(); err != nil {
		t.Fatalf("CheckCn recv while another object hangs: %v", err)
	}
	other.CloseSend()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the hanging round did not end when the stream was cancelled")
	}

	// Clearing the lever releases a round the worker is still waiting on.
	stream2, err := cnClient.CheckCntlr(context.Background())
	if err != nil {
		t.Fatalf("CheckCntlr: %v", err)
	}
	done = sendRound(stream2)
	writeFile(t, agent, behaviorFileName, `{}`)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the released round failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the round did not resume when hang was cleared")
	}
	stream2.CloseSend()
}

// TestCheckSideDropStream is §14.9's drop_stream lever.
func TestCheckSideDropStream(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, _ := startAgent(t, agent)
	ptr := sidePtr(1, 3, 5)
	syncupDn(t, dnClient, 1, ptr)
	syncupSide(t, dnClient, ptr, 1, 0)
	writeFile(t, agent, behaviorFileName,
		`{"objects": {"side 1:3:5": {"drop_stream": true}}}`)

	stream, err := dnClient.CheckSide(context.Background())
	if err != nil {
		t.Fatalf("CheckSide: %v", err)
	}
	if err := stream.Send(&pb.CheckSideRequest{
		ClusterId: 1, DnId: 1, SidePointer: ptr, Revision: 1,
	}); err != nil {
		t.Fatalf("CheckSide send: %v", err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("drop_stream still replied")
	}
}

// ---------------------------------------------------------------------------
// state.json (§14.9)
// ---------------------------------------------------------------------------

// TestStateSurvivesRestart is §14.11 case B step 5: a killed and restarted
// fake reports the stored revision (so the worker does not re-sync) and the
// bm_idx_list derived from the recorded chunks.
func TestStateSurvivesRestart(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, _ := startAgent(t, agent)
	ptr := sidePtr(1, 3, 5)
	syncupDn(t, dnClient, 7, ptr)
	syncupSide(t, dnClient, ptr, 7, 30)
	if _, err := dnClient.PushMigrBitmap(context.Background(),
		&pb.PushMigrBitmapRequest{
			ClusterId: 1, DnId: 1, SidePointer: ptr, Revision: 7,
			MigrId: 30, BmIdx: 1, Bitmap: []byte{0x01, 0x02, 0x03},
		}); err != nil {
		t.Fatalf("PushMigrBitmap: %v", err)
	}

	restarted, err := newFakeAgent(context.Background(), agent.dir, 4096)
	if err != nil {
		t.Fatalf("restarting: %v", err)
	}
	restartedClient, _ := startAgent(t, restarted)
	reply, err := restartedClient.GetSideInfo(context.Background(),
		&pb.GetSideInfoRequest{ClusterId: 1, DnId: 1, SidePointer: ptr})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("GetSideInfo code = %d (%s)",
			reply.GetAgentReply().GetCode(),
			reply.GetAgentReply().GetDetails())
	}
	if reply.GetRevision() != 7 {
		t.Errorf("restarted revision = %d, want 7", reply.GetRevision())
	}
	wantKeys(t, "cn_id_to_nvmeof", reply.GetSideInfo().GetCnIdToNvmeof(),
		1, 2, 3)

	restarted.mu.Lock()
	idxList := restarted.state[sideObjKey(ptr)].bmIdxList(30)
	restarted.mu.Unlock()
	if !slices.Equal(idxList, []uint32{1}) {
		t.Errorf("restarted bm_idx_list = %v, want [1]", idxList)
	}

	// A stale round after the restart is still refused.
	if got := syncupDn(t, restartedClient, 6).GetAgentReply().
		GetCode(); got != common.ReplyCodeStaleRevision {
		t.Errorf("stale SyncupDn after the restart code = %d, want %d",
			got, common.ReplyCodeStaleRevision)
	}
}

// TestStateHandEdit is §14.11 case A step 3: the script rewrites a stored
// revision in state.json while the fake runs, and the next round must see it.
func TestStateHandEdit(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, _ := startAgent(t, agent)
	syncupDn(t, dnClient, 3)

	raw, err := os.ReadFile(filepath.Join(agent.dir, stateFileName))
	if err != nil {
		t.Fatalf("reading state.json: %v", err)
	}
	parsed := &stateFile{}
	if err := json.Unmarshal(raw, parsed); err != nil {
		t.Fatalf("state.json is not readable JSON: %v", err)
	}
	if parsed.Objects[dnObjKey].Revision != 3 {
		t.Fatalf("stored revision = %d, want 3",
			parsed.Objects[dnObjKey].Revision)
	}
	if len(parsed.Objects[dnObjKey].Request) == 0 {
		t.Fatal("the stored request is empty")
	}
	parsed.Objects[dnObjKey].Revision = 9
	edited, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		t.Fatalf("marshaling the edit: %v", err)
	}
	writeFile(t, agent, stateFileName, string(edited))

	reply := syncupDn(t, dnClient, 3)
	if got := reply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeStaleRevision {
		t.Fatalf("SyncupDn after the edit code = %d, want %d",
			got, common.ReplyCodeStaleRevision)
	}
	if reply.GetRevision() != 9 {
		t.Errorf("reply revision = %d, want the edited 9", reply.GetRevision())
	}
}

func TestGetSizeAndBitmapStubs(t *testing.T) {
	agent := newTestAgent(t)
	dnClient, cnClient := startAgent(t, agent)
	ctx := context.Background()

	dnSize, err := dnClient.GetDnSize(ctx, &pb.GetDnSizeRequest{
		ClusterId: 1, DnId: 1,
	})
	if err != nil {
		t.Fatalf("GetDnSize: %v", err)
	}
	if dnSize.GetSize() != 4096 {
		t.Errorf("GetDnSize = %d, want the configured 4096", dnSize.GetSize())
	}
	writeFile(t, agent, behaviorFileName, `{"size": 8192}`)
	cnSize, err := cnClient.GetCnSize(ctx, &pb.GetCnSizeRequest{
		ClusterId: 1, CnId: 1,
	})
	if err != nil {
		t.Fatalf("GetCnSize: %v", err)
	}
	if cnSize.GetSize() != 8192 {
		t.Errorf("GetCnSize = %d, want the behavior file's 8192",
			cnSize.GetSize())
	}

	tdBm, err := cnClient.GetThinDeviceBm(ctx, &pb.GetThinDeviceBmRequest{
		ClusterId: 1, CnId: 1,
	})
	if err != nil {
		t.Fatalf("GetThinDeviceBm: %v", err)
	}
	if len(tdBm.GetBitmap()) != 0 {
		t.Errorf("GetThinDeviceBm = %v, want an empty bitmap", tdBm.GetBitmap())
	}
	legBm, err := cnClient.GetLegBm(ctx, &pb.GetLegBmRequest{
		ClusterId: 1, CnId: 1,
	})
	if err != nil {
		t.Fatalf("GetLegBm: %v", err)
	}
	if len(legBm.GetBitmap()) != 0 {
		t.Errorf("GetLegBm = %v, want an empty bitmap", legBm.GetBitmap())
	}
}
