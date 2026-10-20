package cnagent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The clone fixture mirrors cnagent_integtest.md, Cases (clone_xfer): a 64 MiB
// destination td
// whose source wrote only its first 32 MiB, so the pushed chunk
// `00000000ffffffff` (wire: 1 = never written = skippable) must coalesce into
// exactly one 32 MiB blkdiscard at offset 32 MiB.
const (
	testSrcNqn = "nqn.2024-01.io.dnv:4:" +
		"0000000000000001:00000000000003d1:000000000000000c"
	testSkipHex = "00000000ffffffff"
)

func cloneOf() *pb.Clone {
	return &pb.Clone{
		CloneId:       testClone,
		SrcTrConfList: []*pb.NvmeTrConf{sideTrConf(testIp2, testSvcId2)},
		SrcNqn:        testSrcNqn,
		SrcNsIdx:      1,
		SrcSliceCnt:   1,
		SrcStripeSize: testStripeSize,
		SrcBlockSize:  testBlockSize,
		DstTdId:       testTd,
		DmCloneConf: &pb.DmCloneConf{
			HydrationThreshold: 1, HydrationBatchSize: 1},
		AutoResume: true,
	}
}

// cloneOfTd is a second (or third) clone: its own id, its own source
// subsystem and its own destination td, so several clones can be converged at
// once without sharing a connection or a raid0.
func cloneOfTd(cloneId uint64, srcNqn string, dstTdId uint64) *pb.Clone {
	clone := cloneOf()
	clone.CloneId = cloneId
	clone.SrcNqn = srcNqn
	clone.DstTdId = dstTdId
	return clone
}

// pushChunk pushes one chunk of the pair (srcSliceIdx, bmIdx); pushBitmap is
// the one-slice fixture's chunk (0, 0). A push carries no revision (CN22):
// it is position-addressed data keyed by an id that is never reused, so there
// is nothing for the cntlr's stored revision to be compared with.
func pushChunk(
	t *testing.T,
	srv *CnAgentServer,
	srcSliceIdx uint32,
	bmIdx uint32,
	bitmap []byte,
) {
	t.Helper()
	reply, err := srv.PushCloneBitmap(context.Background(),
		&pb.PushCloneBitmapRequest{
			ClusterId:    testCluster,
			CnId:         testCn,
			CntlrPointer: cntlrPtr(),
			CloneId:      testClone,
			SrcSliceIdx:  srcSliceIdx,
			BmIdx:        bmIdx,
			Bitmap:       bitmap,
		})
	if err != nil {
		t.Fatalf("PushCloneBitmap: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("PushCloneBitmap rejected: %v", reply.GetAgentReply())
	}
}

func pushBitmap(
	t *testing.T,
	srv *CnAgentServer,
	bitmap []byte,
) {
	t.Helper()
	pushChunk(t, srv, 0, 0, bitmap)
}

// chunkIds flattens a clone's reported applied set into comparable pairs.
func chunkIds(info *pb.BitmapInfo) [][2]uint32 {
	out := make([][2]uint32, 0, len(info.GetChunkIdList()))
	for _, id := range info.GetChunkIdList() {
		out = append(out, [2]uint32{id.GetSrcSliceIdx(), id.GetBmIdx()})
	}
	return out
}

func hexBytes(t *testing.T, text string) []byte {
	t.Helper()
	out := make([]byte, len(text)/2)
	for i := range out {
		var value uint32
		if _, err := fmt.Sscanf(text[2*i:2*i+2], "%02x", &value); err != nil {
			t.Fatalf("bad hex %q: %v", text, err)
		}
		out[i] = byte(value)
	}
	return out
}

// ---------------------------------------------------------------------------
// Clone build and teardown (CN18)
// ---------------------------------------------------------------------------

// TestCloneBuild is the CN18 sequence over the clone's clone meta group, at
// raid1 and without redundancy: the group's legs are connected and its array
// or its CnGrpName linear built first (CN12), then the source is connected,
// the dm-clone is created over the group's device with hydration off and
// discard passdown off, the source chunk the node holds is applied, hydration
// is enabled, and the ns-dev moves onto the dm-clone. No arena slot is
// allocated or punched, and the dm-clone's two rows read OK.
func TestCloneBuild(t *testing.T) {
	for _, raid1 := range []bool{false, true} {
		t.Run(fmt.Sprintf("raid1=%v", raid1), func(t *testing.T) {
			srv, node := newTestServer(t)
			// Stage 1: the clone is declared but gated by the level, so the
			// bitmap push cannot race the dm-clone (the CN19 staged gate).
			// Its clone meta group is a group, built under the group gate.
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				raid1: raid1})
			if _, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 3, primary: true, raid1: raid1,
					clones: []*pb.Clone{cloneOf()},
					level:  pb.SpLevel_SP_LEVEL_NO_CLONE})); err != nil {
				t.Fatalf("gated clone: %v", err)
			}
			assertNoCall(t, node, "cmd nvme connect --transport tcp "+
				"--traddr "+testIp2+" --trsvcid "+testSvcId2+" --nqn "+
				testSrcNqn)
			grpDev := "/dev/mapper/" + grpName(srv, testCloneGrp)
			if raid1 {
				grpDev = cloneMdPath(srv)
				if node.mdNode(grpDev) == "" {
					t.Fatalf("the clone meta group has no array at " +
						"SP_LEVEL_NO_CLONE")
				}
			} else if _, ok := node.dms[grpName(srv, testCloneGrp)]; !ok {
				t.Fatalf("the clone meta group has no device at " +
					"SP_LEVEL_NO_CLONE")
			}

			// Stage 2: the chunk lands while nothing serves it.
			node.Reset()
			pushBitmap(t, srv, hexBytes(t, testSkipHex))
			assertNoCall(t, node, "cmd blkdiscard")
			bmPath := srv.nf.LocalCloneBmPath(
				testCluster, testCn, testSp, testClone, 0, 0)
			if _, ok := node.protos[bmPath]; !ok {
				t.Fatalf("the chunk was not persisted at %s", bmPath)
			}

			// Stage 3: enabling the clone runs the whole CN18 sequence.
			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 4, primary: true, raid1: raid1,
					clones: []*pb.Clone{cloneOf()}}))
			if err != nil {
				t.Fatalf("enable: %v", err)
			}
			if reply.GetAgentReply().GetCode() != 0 {
				t.Fatalf("rejected: %v", reply.GetAgentReply())
			}
			clone := cloneName(srv, testClone)
			assertOrder(t, node,
				"cmd nvme connect --transport tcp --traddr "+testIp2,
				"cmd dmsetup create "+clone,
				"cmd blkdiscard --offset 33554432 --length 33554432",
				"cmd dmsetup message "+clone+" 0 enable_hydration",
				"cmd dmsetup reload "+nsDevName(srv, testNs),
			)
			// The dm-clone's metadata device is the clone meta group's
			// device, by its number, and no arena slot is involved.
			metaNo := node.devNo[grpDev]
			if raid1 {
				_, array := node.arrayByDev(grpDev)
				if array == nil {
					t.Fatalf("no array at %s", grpDev)
				}
				metaNo = array.devNo
			}
			fields := strings.Fields(node.dms[clone].table)
			if len(fields) < 4 || fields[3] != metaNo {
				t.Fatalf("the dm-clone's table %q does not name the group "+
					"device %s (%s) as its metadata device",
					node.dms[clone].table, grpDev, metaNo)
			}
			assertNoCall(t, node, "cmd dmsetup create "+
				cloneMetaName(srv, testClone))
			assertNoCall(t, node, "cmd losetup")
			// The table is created with hydration off, always — and with
			// discard passdown off, without which every `blkdiscard` below
			// would be remapped to the destination raid0 (CN18 step 3).
			create := node.callsMatching("cmd dmsetup create " + clone)
			if len(create) != 1 || !strings.Contains(create[0],
				"2 no_hydration no_discard_passdown") {
				t.Fatalf("dm-clone features are wrong: %v", create)
			}
			// Exactly one coalesced hydration discard: skip bits 32..63 →
			// 32 MiB at 32 MiB, and no other discard of the pass.
			if discards := node.callsMatching(
				"cmd blkdiscard"); len(discards) != 1 {
				t.Fatalf("want 1 blkdiscard, got %d: %v", len(discards),
					discards)
			}
			// The ns-dev now sits on the dm-clone, serving despite the
			// stored suspended flag — the auto_resume override of CN16.
			table := node.dms[nsDevName(srv, testNs)].table
			cloneNo := node.devNo["/dev/mapper/"+clone]
			if !strings.Contains(table, "linear "+cloneNo) {
				t.Fatalf("ns-dev table %q is not on the dm-clone", table)
			}

			info := reply.GetCntlrInfo()
			assertOk(t, info.GetCloneIdToTarget()[testClone], "clone target")
			assertOk(t, info.GetCloneIdToDmClone()[testClone], "dm-clone")
			if rows := info.GetCloneIdToMeta(); len(rows) != 0 {
				t.Fatalf("clone_id_to_meta rows %v, want none", rows)
			}
			assertOk(t, info.GetGrpIdToMdRaid()[testCloneGrp],
				"clone meta group")

			// CN20: bm_info_list reports the applied set as chunk_id_list,
			// derived from the files — bm_idx_list stays unset for a clone.
			if len(reply.GetBmInfoList()) != 1 ||
				reply.GetBmInfoList()[0].GetResId() != testClone ||
				len(reply.GetBmInfoList()[0].GetBmIdxList()) != 0 {
				t.Fatalf("bm_info_list is %v", reply.GetBmInfoList())
			}
			if got := chunkIds(reply.GetBmInfoList()[0]); len(got) != 1 ||
				got[0] != [2]uint32{0, 0} {
				t.Fatalf("chunk_id_list is %v, want [(0,0)]", got)
			}
		})
	}
}

func TestCloneAutoResumeOverridesSuspended(t *testing.T) {
	srv, node := newTestServer(t)
	// The destination namespace is created suspended (architecture.md,
	// Transfer + clone = cross-SP live migration) and serves anyway while the
	// clone runs.
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, suspended: true,
		clones: []*pb.Clone{cloneOf()}})
	// The override has to be pinned on the *backing*, not on the device's
	// suspend bit: since the park (architecture.md, Namespace suspend
	// semantics) nothing is ever dm-suspended, so a
	// `!suspended` assert would pass whether the override worked or not.
	cloneNo := node.devNo["/dev/mapper/"+cloneName(srv, testClone)]
	dev := node.dms[nsDevName(srv, testNs)]
	if want := agent.LinearTable(testTdSize/512, cloneNo, 0); dev.table !=
		want {
		t.Fatalf("auto_resume did not override the suspended flag: "+
			"the ns-dev table is %q, want the dm-clone %q", dev.table, want)
	}
	if dev.suspended {
		t.Fatalf("the serving ns-dev is dm-suspended")
	}
	if got := node.files[anaPath(testNqn, 1)]; got != "1" {
		t.Fatalf("ana_grpid is %q, want 1 (optimized)", got)
	}

	// auto_resume = false leaves it effectively suspended, which means
	// **parked**: CN16 rule 1 wins over the clone backing of
	// rule 5, so the ns-dev is a live dm-linear over the td's dm-error and
	// the namespace is inaccessible. Nothing is dm-suspended ([D12]).
	clone := cloneOf()
	clone.AutoResume = false
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, suspended: true,
		clones: []*pb.Clone{clone}})); err != nil {
		t.Fatalf("auto_resume=false: %v", err)
	}
	assertParked(t, srv, node, testNs, testTd, "auto_resume=false")
	if got := node.files[anaPath(testNqn, 1)]; got != "3" {
		t.Fatalf("ana_grpid is %q, want 3 (inaccessible)", got)
	}
}

// TestCloneTeardownOrder: a clone that leaves clone_list is taken down by the
// sweep in CN21's layer order — the ns-dev parked off the dm-clone, the
// dm-clone, the source connection, then its clone meta group's device and
// legs — and its chunk files go; the ns-dev ends back on the raid0.
func TestCloneTeardownOrder(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	pushBitmap(t, srv, hexBytes(t, testSkipHex))

	node.Reset()
	// DeleteClone: the clone leaves clone_list and the namespace's stored
	// suspended flag goes back to false.
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true}))
	if err != nil {
		t.Fatalf("delete clone: %v", err)
	}
	// The source's disconnect runs off the pass's locks (CN10), so the
	// descent stops at it, and the clone meta group under it is the pass's
	// leftover too, until a later pass finds the source gone (CN21).
	cnSweepAssertDetails(t, reply.GetAgentReply(), testSrcNqn, "delete clone")
	cnSweepAssertDetails(t, reply.GetAgentReply(),
		grpName(srv, testCloneGrp), "delete clone")
	awaitDisconnects(t, srv)
	syncupCntlrUntilClean(t, srv, reqOpts{revision: 3, primary: true})
	assertOrder(t, node,
		"cmd dmsetup reload "+nsDevName(srv, testNs),
		"cmd dmsetup remove "+cloneName(srv, testClone),
		"cmd nvme disconnect --nqn "+testSrcNqn,
		"cmd dmsetup remove "+grpName(srv, testCloneGrp),
		"cmd nvme disconnect --nqn "+legNqn(srv, testCloneLeg),
	)
	if !node.hasCall("cmd rm -f " + srv.nf.LocalCloneBmPath(
		testCluster, testCn, testSp, testClone, 0, 0)) {
		t.Fatalf("the clone's chunk file was not removed")
	}
	for _, name := range []string{cloneName(srv, testClone),
		grpName(srv, testCloneGrp), legName(srv, testCloneLeg)} {
		if _, ok := node.dms[name]; ok {
			t.Fatalf("%s survived the clone", name)
		}
	}
	// The ns-dev is back on the raid0, all data local.
	table := node.dms[nsDevName(srv, testNs)].table
	raid0No := node.devNo["/dev/mapper/"+raid0Name(srv, testTd)]
	if !strings.Contains(table, "linear "+raid0No) {
		t.Fatalf("ns-dev table %q is not back on the raid0", table)
	}
}

// ---------------------------------------------------------------------------
// The clone's rows (CN18, CN28)
// ---------------------------------------------------------------------------

// twoTds is the fixture for the multi-clone cases: a second destination td,
// so two clones can be built at once.
func twoTds() []*pb.ThinDevice {
	return []*pb.ThinDevice{
		{TdId: testTd, DevId: 1, Size: testTdSize},
		{TdId: testSnapTd, DevId: 2, Size: testTdSize},
	}
}

// checkRound is one read of the fixture cntlr and the CN24 check round after
// it at a revision, and the rows that read probed, which the round carries.
func checkRound(
	t *testing.T,
	srv *CnAgentServer,
	revision uint64,
) *pb.CntlrInfo {
	t.Helper()
	refreshTestCntlr(t, srv)
	_, probed := srv.checkCntlrRound(context.Background(),
		&pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: revision,
		}, nil)
	if probed == nil {
		t.Fatalf("the check round carried no CntlrInfo")
	}
	return probed
}

// assertSameCloneRows holds the check round to the converge on the clone's
// two rows, epoch and details included: ResTracker.Set overwrites, so a probe
// that answered anything else would replace the converge's row, and the two
// channels would trade the rows, and their epochs, on every round.
func assertSameCloneRows(
	t *testing.T,
	converged *pb.CntlrInfo,
	probed *pb.CntlrInfo,
) {
	t.Helper()
	for _, row := range cloneRowMaps {
		want := row.rows(converged)[testClone]
		got := row.rows(probed)[testClone]
		if want == nil || !proto.Equal(got, want) {
			t.Fatalf("the check round reports %s as %v, the converge as %v",
				row.label, got, want)
		}
	}
}

// assertSameStatusAndEpoch holds the check round to the converge on both of
// the clone's rows by status and epoch, the part of a row the worker acts
// on: the details may differ, since a converge's failure rides in its own
// rows only (CN29).
func assertSameStatusAndEpoch(
	t *testing.T,
	converged *pb.CntlrInfo,
	probed *pb.CntlrInfo,
) {
	t.Helper()
	for _, row := range cloneRowMaps {
		want := row.rows(converged)[testClone]
		got := row.rows(probed)[testClone]
		if want == nil || got == nil {
			t.Fatalf("%s: converged %v, probed %v", row.label, want, got)
		}
		if got.GetStatus() != want.GetStatus() ||
			got.GetEpoch() != want.GetEpoch() {
			t.Fatalf("the check round reports %s as %v (epoch %d, %q), the "+
				"converge as %v (epoch %d, %q)", row.label, got.GetStatus(),
				got.GetEpoch(), got.GetDetails(), want.GetStatus(),
				want.GetEpoch(), want.GetDetails())
		}
	}
}

// cloneRowMaps are the two CntlrInfo maps a clone's rows sit in.
var cloneRowMaps = []struct {
	label string
	rows  func(*pb.CntlrInfo) map[uint64]*pb.ResInfo
}{
	{"clone_id_to_target", (*pb.CntlrInfo).GetCloneIdToTarget},
	{"clone_id_to_dm_clone", (*pb.CntlrInfo).GetCloneIdToDmClone},
}

// assertNoteAndStatusLine pins a dm-clone row that keeps the raw status line
// of a dm-clone that is up beside a status the line does not decide: the
// row's status, its text, and then, on a line of its own, a line
// common.ParseCloneStatus reads, as the gateway's hydration checks do (CN18).
func assertNoteAndStatusLine(
	t *testing.T,
	info *pb.ResInfo,
	status pb.ResStatus,
	note string,
	label string,
) {
	t.Helper()
	details := info.GetDetails()
	line, ok := strings.CutPrefix(details, note+"\n")
	if info.GetStatus() != status || !ok {
		t.Fatalf("%s: status %v, details %q, want %v/%q and a status line",
			label, info.GetStatus(), details, status, note)
	}
	if _, parsed := common.ParseCloneStatus(line); !parsed ||
		strings.Contains(line, "\n") {
		t.Fatalf("%s: %q after the text is not one dm-clone status line",
			label, line)
	}
}

// assertMissingDetails is assertErrorDetails for a MISSING row.
func assertMissingDetails(
	t *testing.T,
	info *pb.ResInfo,
	want string,
	label string,
) {
	t.Helper()
	if info.GetStatus() != pb.ResStatus_RES_STATUS_MISSING ||
		!strings.Contains(info.GetDetails(), want) {
		t.Fatalf("%s: status %v, details %q, want MISSING containing %q",
			label, info.GetStatus(), info.GetDetails(), want)
	}
}

// TestNsDevParksOverAMissingDmClone is CN16 rule 5 for a dm-clone that is not
// there. Its status read fails, and a `dmsetup info` that confirms the device
// is gone makes that read "no hydration": the ns-dev parks on the td's
// dm-error and its row reads OK, in the converge and the check round alike —
// here after a pass whose dm-clone create the node refuses. Absence is
// confirmed, never assumed: when that `dmsetup info` does not answer, the
// read stays an error, the ns-dev keeps the table it has and its row reads
// ERROR. The confirm is the dm-clone's own `dmsetup info` in a cntlr's read
// too, never the reading's listing.
func TestNsDevParksOverAMissingDmClone(t *testing.T) {
	for _, tc := range []struct {
		name        string
		unconfirmed bool
	}{
		{name: "a dm-clone confirmed absent parks the ns-dev"},
		{name: "an absence the info did not confirm keeps the table",
			unconfirmed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				clones: []*pb.Clone{cloneOf()}})
			clone := cloneName(srv, testClone)
			nsDev := nsDevName(srv, testNs)
			onOldClone := node.dms[nsDev].table
			removeCloneDevice(node, srv)
			node.failCmdAlways["dmsetup create "+clone+" "] =
				"device-mapper: reload ioctl on " + clone +
					" failed: Invalid argument"
			if tc.unconfirmed {
				node.killCmdAlways["dmsetup info --columns --noheadings "+
					"-o attr "+clone] = true
			}

			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 3, primary: true,
					clones: []*pb.Clone{cloneOf()}}))
			if err != nil {
				t.Fatalf("the pass without a dm-clone: %v", err)
			}
			if _, ok := node.dms[clone]; ok {
				t.Fatalf("the case is vacuous: the dm-clone is there")
			}
			converged := reply.GetCntlrInfo().GetNsIdToDmLinear()[testNs]
			node.Reset()
			probed := checkRound(t, srv, 3).GetNsIdToDmLinear()[testNs]
			for _, call := range node.Mutations() {
				t.Fatalf("the check round mutated: %q", call)
			}
			if tc.unconfirmed {
				// The round's info of the dm-clone is the reading's, which
				// does not list it; its own `dmsetup info` is the confirm
				// after the failed status read, which the kill meets.
				if !node.hasCall("cmd dmsetup info --columns --noheadings " +
					"-o attr " + clone) {
					t.Fatalf("the check round never confirmed the absence")
				}
				want := "dmsetup status " + clone
				assertErrorDetails(t, converged, want, "ns-dev")
				assertErrorDetails(t, probed, want, "probed ns-dev")
				if got := node.dms[nsDev].table; got != onOldClone {
					t.Fatalf("an unconfirmed absence moved the ns-dev to %q",
						got)
				}
				return
			}
			assertParked(t, srv, node, testNs, testTd, "the pass")
			assertOk(t, converged, "ns-dev")
			assertOk(t, probed, "probed ns-dev")
		})
	}
}

// TestADmCloneGoneMidReadReadsAbsent pins the read of the dm-clone that
// decides CN18 steps 3 and 4 and the clone's rows (probeCloneDm): a dm-clone
// that its `dmsetup info` finds and that is gone by its status read fails
// that read, and a second `dmsetup info` that confirms it gone makes the
// read an absence. A check round then reports the dm row MISSING, as for any
// absent dm-clone, and a converge creates the dm-clone again in the same
// pass: its row reads OK and no retry is registered. Read as an
// error, the round would report ERROR and the converge would end its pass
// and wait for the retry. In a cntlr's read, which a check round answers
// from, the first info is the reading's listing, and the confirming info is
// the dm-clone's own, made after the failed status read: the reading, taken
// before the device went, names it, so a confirm taken from it would keep the
// read an error.
func TestADmCloneGoneMidReadReadsAbsent(t *testing.T) {
	// goneMidRead runs pass with the dm-clone's first status read held,
	// removes the device while that read waits, then lets it fail. In a
	// converge the held read must be the one that follows a `dmsetup info`
	// of the dm-clone, which is how probeCloneDm reads. In a cntlr's read
	// the dm-clone's info is the reading's listing, so no `dmsetup info` of
	// it comes before the held read, and the one right after it is the
	// fresh confirm.
	goneMidRead := func(
		t *testing.T,
		srv *CnAgentServer,
		node *fakeNode,
		round bool,
		pass func(),
	) {
		t.Helper()
		clone := cloneName(srv, testClone)
		status := "cmd dmsetup status " + clone
		info := "cmd dmsetup info --columns --noheadings -o attr " + clone
		node.blockCmd("dmsetup status " + clone)
		t.Cleanup(func() { node.releaseCmd("dmsetup status " + clone) })
		done := make(chan struct{})
		go func() {
			defer close(done)
			pass()
		}()
		waitFor(t, func() bool { return node.hasCall(status) })
		node.mu.Lock()
		removeCloneDevice(node, srv)
		node.mu.Unlock()
		node.releaseCmd("dmsetup status " + clone)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the pass never returned")
		}
		first := node.indexOfCall(status)
		calls := node.Calls()
		if round {
			listing := node.indexOfCall(dmInfoListLine)
			if first < 1 || listing < 0 || listing > first ||
				node.indexOfCall(info) < first ||
				node.indexOfCall(dmNumberRead+clone) >= 0 ||
				first+1 >= len(calls) || calls[first+1] != info {
				t.Fatalf("the round's reads of the dm-clone are not the "+
					"reading's listing, the held status and a fresh info "+
					"of it\ncalls:\n%s", strings.Join(calls, "\n"))
			}
			return
		}
		if first < 1 || calls[first-1] != info {
			t.Fatalf("fixture: the held status read is not the one that "+
				"follows an info of the dm-clone\ncalls:\n%s",
				strings.Join(calls, "\n"))
		}
	}

	t.Run("a check round", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.retryInterval = time.Hour
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
			clones: []*pb.Clone{cloneOf()}})
		node.Reset()
		var probed *pb.CntlrInfo
		key := cntlrKey(testCluster, testCn, testSp, testCntlr)
		st := srv.getCntlr(key)
		goneMidRead(t, srv, node, true, func() {
			// The round answers from the cntlr's read (dnagent.md SH29),
			// so the read is what the device goes under.
			srv.refreshCntlr(context.Background(), key, st)
			_, probed = srv.checkCntlrRound(context.Background(),
				&pb.CheckCntlrRequest{
					ClusterId: testCluster, CnId: testCn,
					CntlrPointer: cntlrPtr(), Revision: 2,
				}, nil)
		})
		if probed == nil {
			t.Fatal("the check round carried no CntlrInfo")
		}
		assertMissing(t, probed.GetCloneIdToDmClone()[testClone],
			"the dm row of a dm-clone gone mid-read")
	})

	t.Run("a converge", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.retryInterval = time.Hour
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
			clones: []*pb.Clone{cloneOf()}})
		clone := cloneName(srv, testClone)
		node.Reset()
		var (
			reply *pb.SyncupCntlrReply
			err   error
		)
		goneMidRead(t, srv, node, false, func() {
			reply, err = srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 3, primary: true,
					clones: []*pb.Clone{cloneOf()}}))
		})
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		if !node.hasCall("cmd dmsetup create " + clone + " ") {
			t.Error("the pass did not build the dm-clone again")
		}
		if !node.hasCall("cmd dmsetup message " + clone +
			" 0 enable_hydration") {
			t.Error("the pass did not enable hydration")
		}
		assertOk(t, reply.GetCntlrInfo().GetCloneIdToDmClone()[testClone],
			"the dm row of the rebuilt dm-clone")
		if retrying(t, srv) {
			t.Error("the pass that rebuilt the dm-clone registered the retry")
		}
	})
}

// TestADmCloneAbsenceTheInfoDidNotConfirmStaysAnError pins the other half of
// probeCloneDm's rule: a dm-clone that is there, whose status read fails and
// whose `dmsetup info` after that read does not answer, keeps the read an
// error. A check round reports the dm row ERROR, not MISSING, and a converge
// stops at that read, before CN18 step 3: it parks, removes and builds
// nothing, the ns-dev keeps serving through the dm-clone, the dm row reads
// ERROR with the failed read's error and the retry is registered. Read as an
// absence, the converge would create a second dm-clone under the name of
// one that serves. In a cntlr's read,
// which a check round answers from, the dm-clone's first info is the
// reading's listing, so its own `dmsetup info` is the confirm alone.
func TestADmCloneAbsenceTheInfoDidNotConfirmStaysAnError(t *testing.T) {
	const readErr = "Input/output error"
	// unconfirmed runs pass with the dm-clone's first status read refused
	// and the `dmsetup info` of it that confirms an absence killed: its
	// second in a converge, its first in a cntlr's read. It then checks that
	// both faults fired, and that the dm-clone's reads went info (a
	// converge's own; a read's comes from the reading), the refused status,
	// then the killed info, as probeCloneDm reads.
	unconfirmed := func(
		t *testing.T,
		srv *CnAgentServer,
		node *fakeNode,
		round bool,
		pass func(),
	) {
		t.Helper()
		clone := cloneName(srv, testClone)
		status := "dmsetup status " + clone
		info := "dmsetup info --columns --noheadings -o attr " + clone
		confirm := 2
		if round {
			confirm = 1
		}
		node.mu.Lock()
		node.failCmd[status] = "device-mapper: status ioctl on " + clone +
			" failed: " + readErr
		node.killCmdNth[info] = confirm
		node.mu.Unlock()
		pass()
		node.mu.Lock()
		_, failLeft := node.failCmd[status]
		_, killLeft := node.killCmdNth[info]
		node.mu.Unlock()
		if failLeft || killLeft {
			t.Fatalf("fixture: a fault did not fire (status %v, info %v)",
				failLeft, killLeft)
		}
		var reads []string
		for _, call := range node.Calls() {
			if call == "cmd "+status || call == "cmd "+info {
				reads = append(reads, call)
			}
		}
		want := []string{"cmd " + info, "cmd " + status, "cmd " + info}
		if round {
			want = want[1:]
		}
		if len(reads) < len(want) || !slices.Equal(reads[:len(want)], want) {
			t.Fatalf("fixture: the dm-clone's first reads are not an "+
				"info, the refused status and the killed info\nreads:\n%s",
				strings.Join(reads, "\n"))
		}
	}

	t.Run("a check round", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.retryInterval = time.Hour
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
			clones: []*pb.Clone{cloneOf()}})
		node.Reset()
		var probed *pb.CntlrInfo
		unconfirmed(t, srv, node, true,
			func() { probed = checkRound(t, srv, 2) })
		for _, call := range node.Mutations() {
			t.Fatalf("the check round mutated: %q", call)
		}
		assertErrorDetails(t, probed.GetCloneIdToDmClone()[testClone],
			readErr, "the dm row of a dm-clone whose absence is unconfirmed")
	})

	t.Run("a converge", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.retryInterval = time.Hour
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
			clones: []*pb.Clone{cloneOf()}})
		clone := cloneName(srv, testClone)
		nsDev := nsDevName(srv, testNs)
		if dm := node.dms[clone]; dm == nil || dm.noHydration {
			t.Fatalf("fixture: no dm-clone with hydration on is up (%v)", dm)
		}
		onClone := agent.LinearTable(testTdSize/512,
			node.devNo["/dev/mapper/"+clone], 0)
		if got := node.dms[nsDev].table; got != onClone {
			t.Fatalf("fixture: the ns-dev %q does not serve through the "+
				"dm-clone", got)
		}
		node.Reset()
		var (
			reply *pb.SyncupCntlrReply
			err   error
		)
		unconfirmed(t, srv, node, false, func() {
			reply, err = srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 3, primary: true,
					clones: []*pb.Clone{cloneOf()}}))
		})
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		assertNoCall(t, node, "cmd dmsetup remove "+clone)
		assertNoCall(t, node, "cmd dmsetup create "+clone)
		assertNoCall(t, node, "cmd dmsetup message "+clone)
		assertNoCall(t, node, "cmd dmsetup reload "+nsDev)
		if got := node.dms[nsDev].table; got != onClone {
			t.Fatalf("the ns-dev moved off the dm-clone to %q", got)
		}
		assertErrorDetails(t,
			reply.GetCntlrInfo().GetCloneIdToDmClone()[testClone], readErr,
			"the dm row of a dm-clone whose absence is unconfirmed")
		if !retrying(t, srv) {
			t.Error("the pass the read stopped registered no retry")
		}
	})
}

// TestCloneMetaWrappersSwept: no clone keeps its metadata in the arena
// (CN18), so every kind-`cb` wrapper is a leftover, one named after a clone
// the request still carries included. A cntlr's sweep removes the wrappers of
// its sp at L4, and the node-level sweep every wrapper of the CN (CN2, CN21);
// the clone's dm-clone, over its clone meta group, serves on.
func TestCloneMetaWrappersSwept(t *testing.T) {
	for _, tc := range []struct {
		name string
		pass func(t *testing.T, srv *CnAgentServer)
	}{
		{"a cntlr's sweep", func(t *testing.T, srv *CnAgentServer) {
			syncupCntlrUntilClean(t, srv, reqOpts{revision: 2,
				primary: true, clones: []*pb.Clone{cloneOf()}})
		}},
		{"the node's sweep", func(t *testing.T, srv *CnAgentServer) {
			if _, err := srv.SyncupCn(
				context.Background(), cnReq(3, true)); err != nil {
				t.Fatalf("SyncupCn: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				clones: []*pb.Clone{cloneOf()}})
			loop := loopDev(t, srv, node)
			wrappers := []string{
				srv.nf.CnCloneMetaDmName(
					testCluster, testCn, testSp, testClone),
				srv.nf.CnCloneMetaDmName(testCluster, testCn, testSp, 0x999),
			}
			for i, name := range wrappers {
				node.dms[name] = &fakeDm{
					table: fmt.Sprintf("0 16384 linear %s %d",
						node.devNo[loop], i*16384),
					thinIds: map[uint32]bool{},
				}
				node.devNo["/dev/mapper/"+name] = node.newDevNo()
			}
			clone := cloneName(srv, testClone)
			serving := node.dms[nsDevName(srv, testNs)].table

			node.Reset()
			tc.pass(t, srv)
			for _, name := range wrappers {
				if !node.hasCall("cmd dmsetup remove " + name) {
					t.Fatalf("the sweep did not remove %s:\n%s", name,
						strings.Join(node.Calls(), "\n"))
				}
				if _, ok := node.dms[name]; ok {
					t.Fatalf("the wrapper %s survived the sweep", name)
				}
			}
			if _, ok := node.dms[clone]; !ok {
				t.Fatalf("the sweep removed the clone's dm-clone")
			}
			if got := node.dms[nsDevName(srv, testNs)].table; got != serving {
				t.Fatalf("the ns-dev moved to %q, want %q", got, serving)
			}
		})
	}
}

// ---------------------------------------------------------------------------

func TestPushCloneBitmapGates(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})

	unknown := func(req *pb.PushCloneBitmapRequest, label string) {
		t.Helper()
		reply, err := srv.PushCloneBitmap(ctx, req)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if reply.GetAgentReply().GetCode() !=
			common.ReplyCodeUnknownObject {
			t.Fatalf("%s: code %d, want %d", label,
				reply.GetAgentReply().GetCode(),
				common.ReplyCodeUnknownObject)
		}
	}
	unknown(&pb.PushCloneBitmapRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr(),
		CloneId: 0x999, SrcSliceIdx: 0, BmIdx: 0,
		Bitmap: []byte{0xff},
	}, "unknown clone")
	// The two indexes bound independently (CN22). A valid slice with an
	// out-of-range bm_idx is rejected on the chunk cap alone...
	unknown(&pb.PushCloneBitmapRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr(),
		CloneId: testClone, SrcSliceIdx: 0,
		BmIdx: common.MaxCloneBmCnt, Bitmap: []byte{0xff},
	}, "bm_idx >= MaxCloneBmCnt")
	// ...and an out-of-range slice is rejected with the chunk index at 0, so
	// neither bound can stand in for the other. The fixture's src_slice_cnt
	// is 1, and bm_idx 0 is always legal.
	unknown(&pb.PushCloneBitmapRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr(),
		CloneId: testClone, SrcSliceIdx: 1, BmIdx: 0,
		Bitmap: []byte{0xff},
	}, "src_slice_idx >= src_slice_cnt")
	// A pair that is in range on both axes is accepted, which is what makes
	// the two rejections above bounds and not blanket refusals.
	pushChunk(t, srv, 0, common.MaxCloneBmCnt-1, []byte{0xff})
}

// TestPushHasNoRevisionGate pins the revision-free push of architecture.md,
// Bitmap push protocol, on the cn side (CN22): PushCloneBitmap carries no
// revision and the handler compares none, so a chunk the worker planned
// against a report the cntlr's stored revision has since superseded is applied
// rather than discarded. A revision gate would only ever throw away work that
// is about to be redone — a chunk is position-addressed data at a
// (clone_id, src_slice_idx, bm_idx) whose ids are never reused, and a push
// never advances the stored revision.
//
// The handler refuses by name instead: a clone the stored request does not
// name is ReplyCodeUnknownObject with a message the worker logs.
func TestPushHasNoRevisionGate(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	// Revision 2 is the report the push below is planned from.
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	// ...and the cntlr moves on to 3 before the push arrives, which is the
	// superseded-report case.
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 3, primary: true,
		clones: []*pb.Clone{cloneOf()}})); err != nil {
		t.Fatalf("advancing the stored revision: %v", err)
	}

	// The request has no revision field, so no edit can add a revision gate
	// without changing the proto.
	if (&pb.PushCloneBitmapRequest{}).ProtoReflect().Descriptor().
		Fields().ByName("revision") != nil {
		t.Fatal("PushCloneBitmapRequest still carries a revision field")
	}

	node.Reset()
	pushBitmap(t, srv, hexBytes(t, testSkipHex))
	// Persisted AND applied: the chunk reached the file and the dm-clone.
	bmPath := srv.nf.LocalCloneBmPath(
		testCluster, testCn, testSp, testClone, 0, 0)
	assertOrder(t, node,
		"writeproto "+bmPath,
		"cmd blkdiscard --offset 33554432 --length 33554432",
	)
	// And it is in the applied set the next reply reports, which is the only
	// thing that stops the worker re-pushing it.
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 4, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if len(reply.GetBmInfoList()) != 1 {
		t.Fatalf("bm_info_list = %v", reply.GetBmInfoList())
	}
	if got := chunkIds(reply.GetBmInfoList()[0]); len(got) != 1 ||
		got[0] != [2]uint32{0, 0} {
		t.Fatalf("applied set = %v, want the pushed pair (0, 0)", got)
	}

	// The refusal by name holds, message and all.
	unknownReply, err := srv.PushCloneBitmap(ctx, &pb.PushCloneBitmapRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr(),
		CloneId: 0x999, SrcSliceIdx: 0, BmIdx: 0, Bitmap: []byte{0xff},
	})
	if err != nil {
		t.Fatalf("unknown clone: %v", err)
	}
	if got := unknownReply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeUnknownObject {
		t.Fatalf("unknown clone: code %d, want %d",
			got, common.ReplyCodeUnknownObject)
	}
	if unknownReply.GetAgentReply().GetDetails() == "" {
		t.Fatal("the refusal carried no message for the worker to log")
	}
}

func TestPushCloneBitmapPersistBeforeApply(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})

	node.Reset()
	pushBitmap(t, srv, hexBytes(t, testSkipHex))
	bmPath := srv.nf.LocalCloneBmPath(
		testCluster, testCn, testSp, testClone, 0, 0)
	assertOrder(t, node,
		"writeproto "+bmPath,
		"cmd blkdiscard --offset 33554432 --length 33554432",
	)

	// A grown chunk overwrites the file and re-applies ([D8]).
	node.Reset()
	grown := append(hexBytes(t, testSkipHex), 0xff)
	pushBitmap(t, srv, grown)
	assertOrder(t, node, "writeproto "+bmPath, "cmd blkdiscard --offset")

	// An identical re-push is neither rewritten nor re-applied.
	node.Reset()
	pushBitmap(t, srv, grown)
	assertNoCall(t, node, "writeproto "+bmPath)
	assertNoCall(t, node, "cmd blkdiscard")
}

// TestPushCloneBitmapSurvivesRestart is SH21: the applied set is derived from
// the files on disk, so a fresh server over the same store reports it back
// unchanged and never needs a re-push. The reload decodes the pair out of the
// stored request — the file name is only an address.
func TestPushCloneBitmapSurvivesRestart(t *testing.T) {
	srv, node := newTestServer(t)
	clone := cloneOf()
	clone.SrcSliceCnt = 2
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{clone}})
	pushBitmap(t, srv, hexBytes(t, testSkipHex))
	// A second slice's chunk 1: a pair neither index alone can name.
	pushChunk(t, srv, 1, 1, []byte{0xff})
	for _, id := range [][2]uint32{{0, 0}, {1, 1}} {
		path := srv.nf.LocalCloneBmPath(
			testCluster, testCn, testSp, testClone, id[0], id[1])
		if _, ok := node.protos[path]; !ok {
			t.Fatalf("chunk %v was not persisted at %s", id, path)
		}
	}

	fresh := newCnServer(node)
	reconcileForTest(t, fresh)
	reply, err := fresh.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{clone}}))
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if len(reply.GetBmInfoList()) != 1 {
		t.Fatalf("bm_info_list is %v", reply.GetBmInfoList())
	}
	got := chunkIds(reply.GetBmInfoList()[0])
	if len(got) != 2 || got[0] != [2]uint32{0, 0} || got[1] != [2]uint32{1, 1} {
		t.Fatalf("the applied set did not survive the restart: %v", got)
	}
}

// TestPushCloneBitmapWithoutDmClone: a chunk that arrives while the dm-clone
// is level-suppressed still counts as applied, and is re-applied when the
// dm-clone is built (CN22, CN18 step 4).
func TestPushCloneBitmapWithoutDmClone(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()},
		level: pb.SpLevel_SP_LEVEL_NO_CLONE})

	node.Reset()
	pushBitmap(t, srv, hexBytes(t, testSkipHex))
	assertNoCall(t, node, "cmd blkdiscard")

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got := chunkIds(reply.GetBmInfoList()[0]); len(got) != 1 ||
		got[0] != [2]uint32{0, 0} {
		t.Fatalf("the chunk was not reported applied: %v", got)
	}
	if !node.hasCall("cmd blkdiscard --offset 33554432 --length 33554432") {
		t.Fatalf("the chunk was not re-applied on the rebuild")
	}
}

// ---------------------------------------------------------------------------
// CN18's shared reading of a clone's rows, for the converge and the check
// round (CN28)
// ---------------------------------------------------------------------------

// TestCloneRowsAgreeBetweenConvergeAndCheck holds the two channels to one
// reading of a clone (cloneRows). For each state of the node, a converge and
// the check round after it report the clone's two rows with the same status
// and the same epoch, and the very same rows where no failure of the
// converge rides in them. The states are the ones the channels could read
// apart: a source without its namespace, a source path that is not live, a
// walk that did not answer, a dm-clone up with hydration off, one whose
// metadata is read-only or failed, and a clone meta group that is not ready,
// with its dm-clone serving or gone. A fault of the source — no namespace, or
// an endpoint whose controller is not live or is gone — reads MISSING on
// both channels beside a dm-clone that serves on, so no row of the clone
// reads ERROR, and none sets a health epoch (CN18). A dm-clone that is up
// keeps its raw status line in its row whatever the row's status, which is
// what the gateway's hydration checks read. What serves a namespace does not
// follow the rows: a dm-clone that serves serves on, one with hydration off
// or its metadata not read-write stays parked (nsDevNow).
func TestCloneRowsAgreeBetweenConvergeAndCheck(t *testing.T) {
	const (
		ok      = pb.ResStatus_RES_STATUS_OK
		missing = pb.ResStatus_RES_STATUS_MISSING
		errored = pb.ResStatus_RES_STATUS_ERROR
	)
	noNs := fmt.Sprintf("no namespace 1 on %s", testSrcNqn)
	grpNotReady := func(srv *CnAgentServer, node *fakeNode) {
		cloneGrpNotReady(srv, node, testCloneGrp, testCloneLeg)
	}
	for _, tc := range []struct {
		name string
		// unfinished starts from a dm-clone a build stopped before CN18
		// step 5 left, hydration off; otherwise the clone is built and
		// serving.
		unfinished bool
		arrange    func(t *testing.T, srv *CnAgentServer, node *fakeNode)
		// target and dm are the rows' statuses, on both channels.
		target, dm pb.ResStatus
		// targetHas and dmHas are texts the converge's rows carry.
		targetHas, dmHas string
		// same: the check round's rows are the converge's, details and all.
		same bool
		// serving: the namespace is served through the dm-clone, and parked
		// on its td's dm-error otherwise.
		serving bool
		// noDm: no dm-clone is up after the converge, so no row carries a
		// status line; failed: the dm-clone's status is the line of fail
		// mode, which carries no count.
		noDm, failed bool
	}{
		{
			name:    "all up",
			arrange: func(*testing.T, *CnAgentServer, *fakeNode) {},
			target:  ok, dm: ok, targetHas: "paths: live",
			same: true, serving: true,
		},
		{
			name: "the source has no namespace",
			arrange: func(_ *testing.T, _ *CnAgentServer, node *fakeNode) {
				node.dropNsHead(testSrcNqn)
			},
			target: missing, dm: ok, targetHas: noNs,
			same: true, serving: true,
		},
		{
			name: "a source path is not live",
			arrange: func(_ *testing.T, _ *CnAgentServer, node *fakeNode) {
				node.setCtrlState(testSrcNqn, testIp2, testSvcId2,
					"connecting")
			},
			target: missing, dm: ok,
			targetHas: "paths: connecting", same: true, serving: true,
		},
		{
			// The source's controller is deleted while its subsystem keeps
			// listing it and the namespace head the dm-clone holds open,
			// and every connect of the endpoint is refused: the endpoint
			// has no controller at all. The refused connect leads the
			// converge's details only.
			name: "the source's controller gone, its reconnect refused",
			arrange: func(_ *testing.T, _ *CnAgentServer, node *fakeNode) {
				node.deleteCtrlDevice(testSrcNqn, testIp2, testSvcId2)
				failConnects(node, testSrcNqn, testIp2, testSvcId2, 1000)
			},
			target: missing, dm: ok,
			targetHas: "connection refused\npaths: ", serving: true,
		},
		{
			name: "the source's walk does not answer",
			arrange: func(t *testing.T, _ *CnAgentServer, node *fakeNode) {
				path := subsysnqnPath(t, node, testSrcNqn)
				node.mu.Lock()
				defer node.mu.Unlock()
				node.killReadAlways[path] = true
			},
			target: errored, dm: ok,
			targetHas: errSubsysUnknown.Error(), same: true, serving: true,
		},
		{
			name: "a sweep's disconnect of the source in flight",
			arrange: func(_ *testing.T, srv *CnAgentServer, _ *fakeNode) {
				srv.mu.Lock()
				defer srv.mu.Unlock()
				srv.disconnecting[testSrcNqn] = struct{}{}
			},
			target: errored, dm: ok,
			targetHas: detailsDisconnectInFlight(testSrcNqn), same: true,
			serving: true,
		},
		{
			// The source's controller is lost, and the converge's connect
			// is killed after the kernel made the new one: the step fails,
			// and its rows read what that connect left, as the check round
			// does.
			name: "a connect killed after it made the controller",
			arrange: func(_ *testing.T, _ *CnAgentServer, node *fakeNode) {
				node.mu.Lock()
				defer node.mu.Unlock()
				node.nvmeDisconnect([]string{"disconnect", "--nqn",
					testSrcNqn})
				node.killCmd["nvme connect --transport tcp --traddr "+
					testIp2] = true
			},
			target: ok, dm: ok, targetHas: "paths: live",
			same: true, serving: true,
		},
		{
			name:       "hydration off, the enable refused",
			unfinished: true,
			arrange: func(_ *testing.T, srv *CnAgentServer, node *fakeNode) {
				clone := cloneName(srv, testClone)
				node.failCmdAlways["dmsetup message "+clone+
					" 0 enable_hydration"] = "device-mapper: message " +
					"ioctl on " + clone + " failed: Invalid argument"
			},
			target: ok, dm: errored, targetHas: "paths: live",
			dmHas: "Invalid argument",
		},
		{
			name: "the dm-clone gone, its re-create refused",
			arrange: func(_ *testing.T, srv *CnAgentServer, node *fakeNode) {
				clone := cloneName(srv, testClone)
				removeCloneDevice(node, srv)
				node.failCmdAlways["dmsetup create "+clone+" "] =
					"device-mapper: reload ioctl on " + clone +
						" failed: Invalid argument"
			},
			target: ok, dm: missing, targetHas: "paths: live",
			dmHas: "Invalid argument", noDm: true,
		},
		{
			// The dm-clone's create is killed after it made the device: the
			// rows read it there, with hydration off.
			name: "the dm-clone's create killed after it made the dm-clone",
			arrange: func(_ *testing.T, srv *CnAgentServer, node *fakeNode) {
				removeCloneDevice(node, srv)
				node.killCmd["dmsetup create "+
					cloneName(srv, testClone)+" "] = true
			},
			target: ok, dm: errored, targetHas: "paths: live",
			dmHas: "signal: killed\n" + detailsCloneNotHydrating,
		},
		{
			name:       "hydration off, the source has no namespace",
			unfinished: true,
			arrange: func(_ *testing.T, _ *CnAgentServer, node *fakeNode) {
				node.dropNsHead(testSrcNqn)
			},
			target: missing, dm: errored, targetHas: noNs,
			dmHas: detailsCloneNotHydrating, same: true,
		},
		{
			name: "the dm-clone's metadata read-only",
			arrange: func(_ *testing.T, srv *CnAgentServer, node *fakeNode) {
				node.dms[cloneName(srv, testClone)].metaMode =
					common.CloneMetadataReadOnly
			},
			target: ok, dm: errored, targetHas: "paths: live",
			dmHas: detailsCloneMetaReadOnly, same: true,
		},
		{
			name: "the dm-clone's metadata failed",
			arrange: func(_ *testing.T, srv *CnAgentServer, node *fakeNode) {
				node.dms[cloneName(srv, testClone)].metaMode =
					common.CloneMetadataFail
			},
			target: ok, dm: errored, targetHas: "paths: live",
			dmHas: detailsCloneMetaFailed, same: true, failed: true,
		},
		{
			name: "the clone meta group not ready, the dm-clone serving",
			arrange: func(_ *testing.T, srv *CnAgentServer, node *fakeNode) {
				grpNotReady(srv, node)
			},
			target: ok, dm: ok, targetHas: "paths: live",
			same: true, serving: true,
		},
		{
			name: "the clone meta group not ready, the dm-clone gone",
			arrange: func(_ *testing.T, srv *CnAgentServer, node *fakeNode) {
				removeCloneDevice(node, srv)
				grpNotReady(srv, node)
			},
			target: ok, dm: missing, targetHas: "paths: live",
			dmHas: detailsCloneMetaGrpNotReady, same: true, noDm: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				clones: []*pb.Clone{cloneOf()}})
			if tc.unfinished {
				node.dms[cloneName(srv, testClone)].noHydration = true
			}
			tc.arrange(t, srv, node)

			// An equal-revision re-send converges the state as it is.
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 2, primary: true,
					clones: []*pb.Clone{cloneOf()}}))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			converged := reply.GetCntlrInfo()
			node.Reset()
			probed := checkRound(t, srv, 2)
			for _, call := range node.Mutations() {
				t.Fatalf("the check round mutated: %q", call)
			}

			for _, row := range []struct {
				label string
				rows  func(*pb.CntlrInfo) map[uint64]*pb.ResInfo
				want  pb.ResStatus
				has   string
			}{
				{"clone_id_to_target", (*pb.CntlrInfo).GetCloneIdToTarget,
					tc.target, tc.targetHas},
				{"clone_id_to_dm_clone", (*pb.CntlrInfo).GetCloneIdToDmClone,
					tc.dm, tc.dmHas},
			} {
				got := row.rows(converged)[testClone]
				if got.GetStatus() != row.want ||
					!strings.Contains(got.GetDetails(), row.has) {
					t.Fatalf("converged %s is %v/%q, want %v containing %q",
						row.label, got.GetStatus(), got.GetDetails(),
						row.want, row.has)
				}
				if checked := row.rows(probed)[testClone]; checked.GetStatus() !=
					row.want {
					t.Fatalf("checked %s is %v/%q, want %v", row.label,
						checked.GetStatus(), checked.GetDetails(), row.want)
				}
				if tc.same && !proto.Equal(row.rows(probed)[testClone], got) {
					t.Fatalf("the check round reports %s as %v, the "+
						"converge as %v", row.label,
						row.rows(probed)[testClone], got)
				}
			}
			assertSameStatusAndEpoch(t, converged, probed)
			// A dm-clone that is up has its status line in its row on both
			// channels.
			clone := cloneName(srv, testClone)
			if _, up := node.dms[clone]; up == tc.noDm {
				t.Fatalf("the case is vacuous: the dm-clone is up %v", up)
			}
			for label, info := range map[string]*pb.CntlrInfo{
				"converged": converged, "probed": probed} {
				details := info.GetCloneIdToDmClone()[testClone].GetDetails()
				_, parsed := common.ParseCloneStatus(details)
				if tc.failed {
					parsed = common.CloneFailed(details)
				}
				if parsed == tc.noDm {
					t.Fatalf("the %s dm row %q: a status line %v, want %v",
						label, details, parsed, !tc.noDm)
				}
			}

			// What serves the namespace is CN16's, and the rows' agreement
			// does not move it.
			if tc.serving {
				want := agent.LinearTable(testTdSize/512,
					node.devNo["/dev/mapper/"+clone], 0)
				if got := node.dms[nsDevName(srv, testNs)].table; got !=
					want {
					t.Fatalf("the ns-dev is %q, want it serving through "+
						"the dm-clone %q", got, want)
				}
			} else {
				assertParked(t, srv, node, testNs, testTd, tc.name)
			}
			assertOk(t, converged.GetNsIdToDmLinear()[testNs], "ns-dev")
			assertOk(t, probed.GetNsIdToDmLinear()[testNs], "probed ns-dev")
		})
	}
}

// TestCloneTargetRowRule pins clone_id_to_target's one rule (cloneTargetRow),
// a branch per case: what the source read says decides the status, and a
// failed connect of the converge's only leads the details of a row that is
// not OK. Only this node's own reading reads ERROR, a disconnect in flight or
// a walk that did not answer. A source with no subsystem entry reads
// MISSING, and so does a fault of the source, an endpoint with no live
// controller or no namespace, which then sets no health epoch (CN18).
func TestCloneTargetRowRule(t *testing.T) {
	cp := testClonePlan(t)
	live := ctrlView{name: "nvme5", trAddr: testIp2, trSvcId: testSvcId2,
		state: "live", anaState: "optimized"}
	connecting := live
	connecting.state = "connecting"
	refused := errors.New("connect refused")
	unknown := fmt.Errorf("subsystem %s %w: killed", testSrcNqn,
		errSubsysUnknown)
	withNs := func(ctrls ...ctrlView) *subsysView {
		return &subsysView{found: true, nsDev: "/dev/nvme5n1", ctrls: ctrls}
	}
	noNs := &subsysView{found: true, ctrls: []ctrlView{live}}
	for _, tc := range []struct {
		name    string
		src     cloneSource
		status  pb.ResStatus
		details string
	}{
		{"a disconnect in flight", cloneSource{inFlight: true},
			pb.ResStatus_RES_STATUS_ERROR,
			detailsDisconnectInFlight(testSrcNqn)},
		{"the walk unanswered", cloneSource{readErr: unknown},
			pb.ResStatus_RES_STATUS_ERROR, unknown.Error()},
		{"the walk unanswered after a refused connect",
			cloneSource{readErr: unknown, stepErr: refused},
			pb.ResStatus_RES_STATUS_ERROR,
			refused.Error() + "\n" + unknown.Error()},
		{"no read", cloneSource{},
			pb.ResStatus_RES_STATUS_MISSING, ""},
		{"no subsystem", cloneSource{view: &subsysView{}},
			pb.ResStatus_RES_STATUS_MISSING, ""},
		{"no subsystem after a refused connect",
			cloneSource{view: &subsysView{}, stepErr: refused},
			pb.ResStatus_RES_STATUS_MISSING, refused.Error()},
		{"no controller for the endpoint",
			cloneSource{view: &subsysView{found: true, nsDev: "/dev/x"}},
			pb.ResStatus_RES_STATUS_MISSING, "paths: "},
		{"no controller for the endpoint after a refused connect",
			cloneSource{view: &subsysView{found: true, nsDev: "/dev/x"},
				stepErr: refused},
			pb.ResStatus_RES_STATUS_MISSING,
			refused.Error() + "\npaths: "},
		{"a path not live", cloneSource{view: withNs(connecting)},
			pb.ResStatus_RES_STATUS_MISSING, "paths: connecting"},
		{"a path not live after a refused connect",
			cloneSource{view: withNs(connecting), stepErr: refused},
			pb.ResStatus_RES_STATUS_MISSING,
			refused.Error() + "\npaths: connecting"},
		{"no namespace", cloneSource{view: noNs},
			pb.ResStatus_RES_STATUS_MISSING,
			fmt.Sprintf("no namespace 1 on %s", testSrcNqn)},
		{"no namespace after a refused connect",
			cloneSource{view: noNs, stepErr: refused},
			pb.ResStatus_RES_STATUS_MISSING,
			refused.Error() + fmt.Sprintf("\nno namespace 1 on %s",
				testSrcNqn)},
		{"live with its namespace", cloneSource{view: withNs(live)},
			pb.ResStatus_RES_STATUS_OK, "paths: live"},
		{"live after a refused connect, the note dropped",
			cloneSource{view: withNs(live), stepErr: refused},
			pb.ResStatus_RES_STATUS_OK, "paths: live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, details := cloneTargetRow(cp, tc.src)
			if status != tc.status || details != tc.details {
				t.Fatalf("%v/%q, want %v/%q", status, details, tc.status,
					tc.details)
			}
		})
	}
}

// TestCloneDmRowRule pins clone_id_to_dm_clone's one rule (cloneDmRow), a
// branch per case: the read of the dm-clone decides the status, the source
// and then the clone meta group only say why a dm-clone that is not there is
// not, and the raw status line rides along whenever the read took it. A
// dm-clone serves, and reads OK, only with hydration on and its metadata
// read-write; its metadata mode is named first, then hydration off.
func TestCloneDmRowRule(t *testing.T) {
	cp := testClonePlan(t)
	ready := cloneSource{view: &subsysView{found: true, nsDev: "/dev/nvme5n1",
		ctrls: []ctrlView{{name: "nvme5", trAddr: testIp2,
			trSvcId: testSvcId2, state: "live"}}}}
	unknown := fmt.Errorf("subsystem %s %w: killed", testSrcNqn,
		errSubsysUnknown)
	const raw = "0 131072 clone 8 1/1024 2048 0/64 0 1 no_hydration 0 rw"
	const failLine = "0 131072 clone Fail"
	killed := errors.New("signal: killed")
	read := func(line string) cloneDmRead { return cloneDmReadOf(line) }
	hydrating := cloneDmRead{present: true, raw: raw, hydrating: true,
		mode: common.CloneMetadataReadWrite}
	for _, tc := range []struct {
		name     string
		src      cloneSource
		grpReady bool
		dm       cloneDmRead
		status   pb.ResStatus
		details  string
	}{
		{"the read did not answer", ready, true, cloneDmRead{err: killed},
			pb.ResStatus_RES_STATUS_ERROR, killed.Error()},
		{"absent beside a ready source", ready, true, cloneDmRead{},
			pb.ResStatus_RES_STATUS_MISSING, ""},
		{"absent beside a group not ready", ready, false, cloneDmRead{},
			pb.ResStatus_RES_STATUS_MISSING, detailsCloneMetaGrpNotReady},
		{"absent beside an unconnected source",
			cloneSource{view: &subsysView{}}, true, cloneDmRead{},
			pb.ResStatus_RES_STATUS_MISSING, "source not connected"},
		{"absent beside an unconnected source and a group not ready",
			cloneSource{view: &subsysView{}}, false, cloneDmRead{},
			pb.ResStatus_RES_STATUS_MISSING, "source not connected"},
		{"absent beside a disconnect in flight",
			cloneSource{inFlight: true}, true, cloneDmRead{},
			pb.ResStatus_RES_STATUS_MISSING, "source not connected"},
		{"absent beside a walk unanswered", cloneSource{readErr: unknown},
			true, cloneDmRead{}, pb.ResStatus_RES_STATUS_MISSING,
			"source unknown: " + unknown.Error()},
		{"absent beside an endpoint with no controller",
			cloneSource{view: &subsysView{found: true,
				nsDev: "/dev/nvme5n1"}}, true,
			cloneDmRead{}, pb.ResStatus_RES_STATUS_MISSING,
			"source not connected"},
		{"serving", ready, true, hydrating,
			pb.ResStatus_RES_STATUS_OK, raw},
		{"serving beside an unconnected source",
			cloneSource{view: &subsysView{}}, true, hydrating,
			pb.ResStatus_RES_STATUS_OK, raw},
		{"serving beside a group not ready", ready, false, hydrating,
			pb.ResStatus_RES_STATUS_OK, raw},
		{"hydration off", ready, true, read(raw),
			pb.ResStatus_RES_STATUS_ERROR,
			detailsCloneNotHydrating + "\n" + raw},
		{"hydration on, metadata read-only", ready, true,
			read(strings.Replace(raw, "1 no_hydration 0 rw", "0 0 ro", 1)),
			pb.ResStatus_RES_STATUS_ERROR, detailsCloneMetaReadOnly + "\n" +
				strings.Replace(raw, "1 no_hydration 0 rw", "0 0 ro", 1)},
		{"hydration off, metadata read-only", ready, true,
			read(strings.Replace(raw, " rw", " ro", 1)),
			pb.ResStatus_RES_STATUS_ERROR, detailsCloneMetaReadOnly + "\n" +
				strings.Replace(raw, " rw", " ro", 1)},
		{"metadata failed", ready, true, read(failLine),
			pb.ResStatus_RES_STATUS_ERROR,
			detailsCloneMetaFailed + "\n" + failLine},
		{"hydration on, no metadata mode", ready, true,
			read(strings.Replace(raw, "1 no_hydration 0 rw", "0 0", 1)),
			pb.ResStatus_RES_STATUS_ERROR, detailsCloneMetaModeUnknown +
				"\n" + strings.Replace(raw, "1 no_hydration 0 rw", "0 0", 1)},
		{"not a dm-clone", ready, true, read("0 131072 linear 253:4 0"),
			pb.ResStatus_RES_STATUS_ERROR,
			detailsCloneNotHydrating + "\n0 131072 linear 253:4 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, details := cloneDmRow(cp, tc.src, tc.grpReady, tc.dm)
			if status != tc.status || details != tc.details {
				t.Fatalf("%v/%q, want %v/%q", status, details, tc.status,
					tc.details)
			}
			if serves := tc.dm.serves(); serves !=
				(status == pb.ResStatus_RES_STATUS_OK) {
				t.Fatalf("the read serves %v beside a row %v", serves,
					status)
			}
		})
	}
}

// TestCloneSourceReadyRule pins CN18 step 1's test on one read of the source
// (cloneSource.ready), which decides on both channels whether the row of an
// absent dm-clone names the source: every configured endpoint has a
// controller, in whatever state, and the source namespace is there.
func TestCloneSourceReadyRule(t *testing.T) {
	cp := testClonePlan(t)
	ctrl := func(state string) ctrlView {
		return ctrlView{name: "nvme5", trAddr: testIp2, trSvcId: testSvcId2,
			state: state}
	}
	other := ctrlView{name: "nvme6", trAddr: testIp, trSvcId: testSvcId,
		state: "live"}
	view := func(nsDev string, ctrls ...ctrlView) *subsysView {
		return &subsysView{found: true, nsDev: nsDev, ctrls: ctrls}
	}
	for _, tc := range []struct {
		name  string
		src   cloneSource
		ready bool
	}{
		{"a disconnect in flight", cloneSource{inFlight: true,
			view: view("/dev/nvme5n1", ctrl("live"))}, false},
		{"the walk unanswered", cloneSource{readErr: errSubsysUnknown},
			false},
		{"no read", cloneSource{}, false},
		{"no subsystem", cloneSource{view: &subsysView{}}, false},
		{"no namespace", cloneSource{view: view("", ctrl("live"))}, false},
		{"another endpoint's controller only",
			cloneSource{view: view("/dev/nvme5n1", other)}, false},
		{"a controller connecting", cloneSource{
			view: view("/dev/nvme5n1", ctrl("connecting"))}, true},
		{"a failed connect beside a ready read", cloneSource{
			view:    view("/dev/nvme5n1", ctrl("live")),
			stepErr: errors.New("refused")}, true},
		{"live", cloneSource{view: view("/dev/nvme5n1", ctrl("live"))},
			true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.src.ready(cp); got != tc.ready {
				t.Fatalf("ready is %v, want %v", got, tc.ready)
			}
		})
	}
}

// testClonePlan is the fixture clone's plan, as a primary's converge builds
// it.
func testClonePlan(t *testing.T) *clonePlan {
	t.Helper()
	plan := newCntlrPlan(common.NewNameFmt(common.DefaultLocalStorPrefix),
		cntlrReq(reqOpts{revision: 2, primary: true,
			clones: []*pb.Clone{cloneOf()}}))
	cp := plan.cloneById[testClone]
	if cp == nil {
		t.Fatalf("the fixture plan has no clone %d", testClone)
	}
	return cp
}
