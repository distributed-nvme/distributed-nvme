package cnagent

import (
	"context"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestThePoolRowReadsThePoolsMode pins CN13's pool row by the pool's mode,
// on the converge and on the read alike: a pool that writes, one out of
// data space and one read-only for full metadata (no needs_check) read
// RES_STATUS_OK, the metadata auto-grow being the cure of the last
// (dnv-worker.md AR6); one with needs_check reads RES_STATUS_ERROR with
// RES_REASON_POOL_NEEDS_CHECK, which every primary that opens the pool
// reads alike; and a pool dm-thin failed, or whose status read could not
// read its metadata, reads RES_STATUS_ERROR with RES_REASON_POOL_FAILED.
// The details stay the raw status line. No such row is named by the read's
// verdict, since a converge cures none of them (CN30).
func TestThePoolRowReadsThePoolsMode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       string
		needsCheck bool
		fault      string
		status     pb.ResStatus
		reason     pb.ResReason
	}{
		{"rw", "", false, "", pb.ResStatus_RES_STATUS_OK,
			pb.ResReason_RES_REASON_NONE},
		{"out of data space", "out_of_data_space", false, "",
			pb.ResStatus_RES_STATUS_OK, pb.ResReason_RES_REASON_NONE},
		{"read-only, metadata full", "ro", false, "",
			pb.ResStatus_RES_STATUS_OK, pb.ResReason_RES_REASON_NONE},
		{"needs_check", "", true, "", pb.ResStatus_RES_STATUS_ERROR,
			pb.ResReason_RES_REASON_POOL_NEEDS_CHECK},
		{"failed", "", false, "Fail", pb.ResStatus_RES_STATUS_ERROR,
			pb.ResReason_RES_REASON_POOL_FAILED},
		{"a status read that failed", "", false, "Error",
			pb.ResStatus_RES_STATUS_ERROR,
			pb.ResReason_RES_REASON_POOL_FAILED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			pool := node.dms[poolName(srv)]
			pool.poolMode = tc.mode
			pool.poolNeedsCheck = tc.needsCheck
			pool.poolFault = tc.fault

			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 3, primary: true}))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			for channel, row := range map[string]*pb.ResInfo{
				"converge": reply.GetCntlrInfo().
					GetSliceIdToDmPool()[testSlice],
				"read": poolRow(t, srv),
			} {
				if row.GetStatus() != tc.status ||
					row.GetReason() != tc.reason ||
					!strings.Contains(row.GetDetails(), " thin-pool ") {
					t.Fatalf("%s: pool row %v, want %v %v", channel, row,
						tc.status, tc.reason)
				}
			}
			if verdict := checkCntlrVerdict(t, srv, 3); verdict.GetCode() !=
				0 {
				t.Fatalf("the read's verdict: %v, want clean", verdict)
			}
		})
	}
}
