package dnagent

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The disk counts (DN18) as the server sends them: beside the meta row of the
// DN converge's SyncupDn reply (DN5) and of the probe of the DN's read, whose
// observation GetDnInfo and the CheckDn rounds answer (SH29).

// testDataExtCnt is the data area of the test server's disk in extents.
var testDataExtCnt = (uint64(512<<20) - common.DnDataOffset) / testExtentSize

// dnInfoOf is GetDnInfo's DnInfo for the test dn once the dn's read has run
// (dnInfoNow): what the node holds now (SH29).
func dnInfoOf(t *testing.T, srv *DnAgentServer) *pb.DnInfo {
	t.Helper()
	reply, err := dnInfoNow(srv, context.Background(),
		&pb.GetDnInfoRequest{ClusterId: testCluster, DnId: testDn})
	if err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	return reply.GetDnInfo()
}

// wantCounts checks the counts of one DnInfo; total is always the data area.
func wantCounts(
	t *testing.T,
	what string,
	info *pb.DnInfo,
	free uint64,
	sides uint64,
	match bool,
) {
	t.Helper()
	got := info.GetDiskCounts()
	if got == nil {
		t.Fatalf("%s: no disk counts beside meta_info %v/%q", what,
			info.GetMetaInfo().GetStatus(), info.GetMetaInfo().GetDetails())
	}
	if got.GetTotalExtCnt() != testDataExtCnt ||
		got.GetFreeExtCnt() != free ||
		got.GetSideRecCnt() != sides ||
		got.GetFreeMetaUnits() != common.DnCloneMetaUnitCnt ||
		got.GetSideRecsMatch() != match {
		t.Errorf("%s: counts = %v, want %d extents, %d free, %d side "+
			"records, every unit free, match %v",
			what, got, testDataExtCnt, free, sides, match)
	}
}

// TestDnInfoCarriesTheCounts follows the counts through a side's life on both
// channels. A SyncupDn reply's counts are taken before the pointer diff and
// the sweep (DN5, DN6): the reply to the SyncupDn that drops a pointer still
// counts the side's record and says the records do not match the new list,
// and the probe after it finds the record swept.
func TestDnInfoCarriesTheCounts(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()

	// The pointer comes before its side: no record holds it yet.
	reply, err := srv.SyncupDn(ctx, dnReq(1, testSide))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	wantCounts(t, "the SyncupDn that adds the pointer", reply.GetDnInfo(),
		testDataExtCnt, 0, false)
	wantCounts(t, "the probe before the side syncs", dnInfoOf(t, srv),
		testDataExtCnt, 0, false)

	// The side converges and its record holds testExtCnt extents.
	syncupSideTwoPhase(t, srv, sideReq(1, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	wantCounts(t, "the probe once the side is built", dnInfoOf(t, srv),
		testDataExtCnt-testExtCnt, 1, true)

	// The pointer leaves the list.
	reply, err = srv.SyncupDn(ctx, dnReq(2))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Fatalf("SyncupDn code = %d (%s)",
			got, reply.GetAgentReply().GetDetails())
	}
	wantCounts(t, "the SyncupDn that drops the pointer", reply.GetDnInfo(),
		testDataExtCnt-testExtCnt, 1, false)
	wantCounts(t, "the probe after the sweep", dnInfoOf(t, srv),
		testDataExtCnt, 0, true)
}

// TestDnInfoCountsNeedASizeAndThisDnsDisk pins the gates of the counts as the
// server applies them: while no read of the disk's size has answered there
// are none, though the format needs no size and the meta row is OK; a disk
// formatted for another dn sends none on either channel; and none go beside
// a meta row that is not OK, though the table in memory is still this dn's,
// as after a header read that did not answer.
func TestDnInfoCountsNeedASizeAndThisDnsDisk(t *testing.T) {
	ctx := context.Background()

	t.Run("a header read that did not answer", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		setHook(node, node.killRead, fmt.Sprintf("readblock %s off=%d len=%d",
			testDisk, common.DnHeaderOffset, common.DnHeaderSize))
		info := dnInfoOf(t, srv)
		if got := info.GetMetaInfo().GetStatus(); got !=
			pb.ResStatus_RES_STATUS_ERROR {
			t.Fatalf("meta_info = %v, want the failed header read", got)
		}
		if info.GetDiskCounts() != nil {
			t.Errorf("counts %v beside an ERROR meta row", info.GetDiskCounts())
		}
		wantCounts(t, "the probe whose header read answered",
			dnInfoOf(t, srv), testDataExtCnt-testExtCnt, 1, true)
	})

	t.Run("no size read has answered", func(t *testing.T) {
		srv, node := newTestServer(t)
		sizeRead := "/sys/dev/block/" + node.devNo[testDisk] + "/size"
		node.mu.Lock()
		node.failReadAlways[sizeRead] = true
		node.mu.Unlock()
		reply, err := srv.SyncupDn(ctx, dnReq(1))
		if err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		for what, info := range map[string]*pb.DnInfo{
			"the SyncupDn reply": reply.GetDnInfo(),
			"the probe":          dnInfoOf(t, srv),
		} {
			if info.GetDiskInfo().GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
				t.Fatalf("%s: disk_info = %v, want the failed size read",
					what, info.GetDiskInfo())
			}
			if got := info.GetMetaInfo().GetStatus(); got !=
				pb.ResStatus_RES_STATUS_OK {
				t.Fatalf("%s: meta_info = %v, want OK", what, got)
			}
			if info.GetDiskCounts() != nil {
				t.Errorf("%s: counts %v from a disk of unknown size",
					what, info.GetDiskCounts())
			}
		}
		node.mu.Lock()
		delete(node.failReadAlways, sizeRead)
		node.mu.Unlock()
		wantCounts(t, "the probe whose size read answered",
			dnInfoOf(t, srv), testDataExtCnt, 0, true)
	})

	t.Run("a disk formatted for another dn", func(t *testing.T) {
		srv, node := newTestServer(t)
		newVerifiedMeta(t, node)
		req := dnReq(1)
		req.DnId = testDn + 100
		reply, err := srv.SyncupDn(ctx, req)
		if err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		probe, err := dnInfoNow(srv, ctx, &pb.GetDnInfoRequest{
			ClusterId: testCluster, DnId: testDn + 100,
		})
		if err != nil {
			t.Fatalf("GetDnInfo: %v", err)
		}
		for what, info := range map[string]*pb.DnInfo{
			"the SyncupDn reply": reply.GetDnInfo(),
			"the probe":          probe.GetDnInfo(),
		} {
			if got := info.GetMetaInfo().GetStatus(); got !=
				pb.ResStatus_RES_STATUS_ERROR {
				t.Fatalf("%s: meta_info = %v, want the foreign-disk error",
					what, got)
			}
			if info.GetDiskCounts() != nil {
				t.Errorf("%s: counts %v from another dn's disk",
					what, info.GetDiskCounts())
			}
		}
	})
}

// TestCheckDnStreamResendsOnAMatchFlip: a CheckDn round without show_info
// omits the info only when it equals the last one sent (SH26), and the counts
// are part of it. A SyncupDn that adds a pointer whose side never syncs
// changes no row and no record, only side_recs_match, and the round after the
// DN's next read carries the info again. Each round follows a read of the
// DN, so that both compare what the node holds (SH29).
func TestCheckDnStreamResendsOnAMatchFlip(t *testing.T) {
	srv, _ := newTestServer(t)
	client := dialAgent(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	syncupBoth(t, srv, 1, testSide)

	stream, err := client.CheckDn(ctx)
	if err != nil {
		t.Fatalf("CheckDn: %v", err)
	}
	round := func(revision uint64) *pb.CheckDnReply {
		t.Helper()
		if err := stream.Send(&pb.CheckDnRequest{
			ClusterId: testCluster, DnId: testDn, Revision: revision,
		}); err != nil {
			t.Fatalf("send: %v", err)
		}
		reply, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		return reply
	}
	readDnNow(ctx, srv)
	first := round(1).GetDnInfo()
	wantCounts(t, "the first round", first, testDataExtCnt-testExtCnt, 1, true)
	readDnNow(ctx, srv)
	if got := round(1).GetDnInfo(); got != nil {
		t.Fatalf("an unchanged round carried the info: %v", got)
	}

	if _, err := srv.SyncupDn(ctx, dnReq(2, testSide, testSide2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	readDnNow(ctx, srv)
	flipped := round(2).GetDnInfo()
	if flipped == nil {
		t.Fatal("the round after the match flipped omitted the info")
	}
	wantCounts(t, "the round after the flip", flipped,
		testDataExtCnt-testExtCnt, 1, false)
	// Nothing but the flip moved: every row and every other count reads as
	// in the first round.
	want := proto.Clone(first).(*pb.DnInfo)
	want.DiskCounts.SideRecsMatch = false
	if !proto.Equal(flipped, want) {
		t.Errorf("the round after the flip =\n%v\nwant only the flip on\n%v",
			flipped, first)
	}
}

// withoutEpochs is info with every row's epoch zeroed: the time of a row's
// last status change, kept in memory, which a restart resets.
func withoutEpochs(info *pb.DnInfo) *pb.DnInfo {
	out := proto.Clone(info).(*pb.DnInfo)
	for _, row := range []*pb.ResInfo{
		out.GetDiskInfo(), out.GetMetaInfo(), out.GetPortInfo(),
	} {
		if row != nil {
			row.Epoch = 0
		}
	}
	return out
}

// TestDiskCountsSurviveARestart is the unit twin of the dn suite's restart
// case (dnagent_integtest.md): the DnInfo GetDnInfo reports before the agent
// stops and after the restarted agent's Reconcile (DN2) is the same, its
// epochs aside, its counts included. The startup converge confirms the
// table, reads the size and reloads the records from the disk, and the
// startup sweep keeps every record a pointer names or a destination claims.
// The first two shapes are the suite's: an exported side, and a migration
// destination whose clone the level gates. That destination's first request
// already carries its migr_dst_conf, as the suite's and the worker's do, so
// it takes its run of the clone-metadata area before it zeroes (DN13), and
// the claim that keeps the run carries no level gate (DN6). The other two
// are provisioned as plain sides before they become destinations, so they
// take no run before zeroing: one whose clone is built, which takes its run
// for the build, and one whose clone the level gates, which never takes one
// and must not have one after the restart either.
func TestDiskCountsSurviveARestart(t *testing.T) {
	ctx := context.Background()
	// destination makes a plain side first and a destination of it after
	// the side has zeroed.
	destination := func(level pb.SpLevel) func(*testing.T, *DnAgentServer) {
		return func(t *testing.T, srv *DnAgentServer) {
			if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			provisionSide(t, srv, 1, testSide)
			if _, err := srv.SyncupSide(ctx, migrDstReq(1, level)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
		}
	}
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, srv *DnAgentServer)
		// units says whether the shape holds clone-metadata units.
		units bool
	}{
		{name: "an exported side", build: func(
			t *testing.T, srv *DnAgentServer) {
			syncupBoth(t, srv, 1, testSide)
		}},
		{name: "a gated migration destination", units: true, build: func(
			t *testing.T, srv *DnAgentServer) {
			if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			syncupSideTwoPhase(t, srv,
				migrDstReq(1, pb.SpLevel_SP_LEVEL_NO_MIGRATION))
			// The suite pushes it one chunk as well.
			reply, err := srv.PushMigrBitmap(ctx,
				pushReq(0, testMigrId, []byte{0x05}))
			if err != nil || reply.GetAgentReply().GetCode() != 0 {
				t.Fatalf("PushMigrBitmap: %v, %v", err,
					reply.GetAgentReply())
			}
		}},
		{name: "a built migration destination", units: true,
			build: destination(pb.SpLevel_SP_LEVEL_READWRITE)},
		{name: "a gated destination provisioned as a plain side",
			build: destination(pb.SpLevel_SP_LEVEL_NO_MIGRATION)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			tc.build(t, srv)
			before := dnInfoOf(t, srv)
			counts := before.GetDiskCounts()
			if counts == nil || !counts.GetSideRecsMatch() ||
				counts.GetSideRecCnt() != 1 ||
				counts.GetFreeExtCnt() != testDataExtCnt-testExtCnt {
				t.Fatalf("before the restart: counts = %v", counts)
			}
			if held := counts.GetFreeMetaUnits() <
				common.DnCloneMetaUnitCnt; held != tc.units {
				t.Fatalf("before the restart: free_meta_units = %d",
					counts.GetFreeMetaUnits())
			}

			stopTestServer(t, srv)
			restarted := startTestServer(t, node)
			after := dnInfoOf(t, restarted)
			if !proto.Equal(withoutEpochs(before), withoutEpochs(after)) {
				t.Errorf("GetDnInfo across the restart, epochs aside:\n"+
					"before %v\nafter  %v", before, after)
			}
		})
	}
}
