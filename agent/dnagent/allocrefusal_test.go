package dnagent

import (
	"context"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestARefusedAllocationReadsErrorOnTheRead pins DN9's refused allocation
// on both channels: a side whose AllocSide the disk refuses — a request
// that disagrees with the side's record, a new side the disk has no room
// for — reads RES_STATUS_ERROR with the refusal's text on the read as on the
// converge, so the side's health epoch is set by both and cleared by
// neither (dnv-worker.md HL2), and leg repair can act on it. The read's
// verdict still names nothing for it (DN16).
func TestARefusedAllocationReadsErrorOnTheRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(req *pb.SyncupSideRequest)
		words string
	}{
		{"a record that disagrees", func(req *pb.SyncupSideRequest) {
			req.SideConf.ExtCnt = testExtCnt + 5
		}, "allocated"},
		{"no room", func(req *pb.SyncupSideRequest) {
			req.SideConf.ExtCnt = 1 << 30
			req.SideConf.Provisioned = false
		}, "allocating"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServer(t)
			ctx := context.Background()
			sideId := testSide
			if tc.name == "no room" {
				// A side of its own, never allocated.
				sideId = testSide + 0x100
				syncupBoth(t, srv, 1, testSide)
				if _, err := srv.SyncupDn(ctx, dnReq(2, testSide,
					sideId)); err != nil {
					t.Fatalf("SyncupDn: %v", err)
				}
			} else {
				syncupBoth(t, srv, 1, testSide)
			}
			req := sideReq(2, sideId, testCn0, nil,
				pb.SpLevel_SP_LEVEL_READWRITE)
			tc.set(req)
			reply, err := srv.SyncupSide(ctx, req)
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			check, _ := checkSideNow(srv, ctx, &pb.CheckSideRequest{
				ClusterId: testCluster, DnId: testDn,
				SidePointer: sidePtr(sideId), Revision: 2,
			}, nil)
			if code := check.GetAgentReply().GetCode(); code != 0 {
				t.Fatalf("the read's verdict: code %d, want 0", code)
			}
			read, err := sideInfoNow(srv, ctx, &pb.GetSideInfoRequest{
				ClusterId: testCluster, DnId: testDn,
				SidePointer: sidePtr(sideId),
			})
			if err != nil {
				t.Fatalf("GetSideInfo: %v", err)
			}
			for channel, row := range map[string]*pb.ResInfo{
				"converge": reply.GetSideInfo().GetSideDevInfo(),
				"read":     read.GetSideInfo().GetSideDevInfo(),
			} {
				if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
					!strings.Contains(row.GetDetails(), tc.words) {
					t.Fatalf("%s: side_dev %v/%q, want ERROR naming %q",
						channel, row.GetStatus(), row.GetDetails(), tc.words)
				}
			}
		})
	}
}
