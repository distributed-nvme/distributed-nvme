package cnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The clone fixture mirrors cnagent_integtest.md §13: a 64 MiB destination td
// whose source wrote only its first 32 MiB, so the pushed chunk
// `00000000ffffffff` (wire: 1 = never written = skippable) must coalesce into
// exactly one 32 MiB blkdiscard at offset 32 MiB.
const (
	testSrcNqn = "nqn.2024-01.io.dnv:4:" +
		"0000000000000001:00000000000003d1:000000000000000c"
	// The allocator tests run two and three clones at once; each has its own
	// source subsystem, so retiring one never disturbs another's connection.
	testSrcNqn2 = testSrcNqn + "2"
	testSrcNqn3 = testSrcNqn + "3"
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
// the one-slice fixture's chunk (0, 0). A push carries no revision ([D13]):
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
// §6.9 — clone build, §11.5 recovery and teardown (CN18)
// ---------------------------------------------------------------------------

func TestCloneBuild(t *testing.T) {
	srv, node := newTestServer(t)
	// Stage 1: the clone is declared but gated by the level, so the bitmap
	// push cannot race the dm-clone (the CN19 staged gate).
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()},
		level: pb.SpLevel_SP_LEVEL_NO_CLONE})); err != nil {
		t.Fatalf("gated clone: %v", err)
	}
	assertNoCall(t, node, "cmd nvme connect --transport tcp --traddr "+
		testIp2+" --trsvcid "+testSvcId2)

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
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 4, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	// The fixture's 64 MiB td over a 1 MiB block gives 64 regions, so the
	// CN18 step 2 budget is ceil((4 MiB + 64) / 4 MiB) = 2 units = 8 MiB =
	// 16384 sectors, first-fit at unit 0 of an empty arena.
	metaDm := cloneMetaName(srv, testClone)
	loop := loopDev(t, srv, node)
	assertOrder(t, node,
		"cmd nvme connect --transport tcp --traddr "+testIp2,
		"cmd blkdiscard --offset 0 --length 8388608 "+loop,
		"cmd dmsetup create "+metaDm,
		"cmd dmsetup create "+cloneName(srv, testClone),
		"cmd blkdiscard --offset 33554432 --length 33554432",
		"cmd dmsetup message "+cloneName(srv, testClone)+
			" 0 enable_hydration",
		"cmd dmsetup reload "+nsDevName(srv, testNs),
	)
	// The wrapper is a plain linear over the arena's first slot: the dm-clone
	// reads its superblock from sector 0 and takes no offset, which is the
	// whole reason the wrapper exists ([D14]).
	if got, want := node.dms[metaDm].table,
		"0 16384 linear "+node.devNo[loop]+" 0"; got != want {
		t.Fatalf("wrapper table is %q, want %q", got, want)
	}
	// The table is created with hydration off, always — and with discard
	// passdown off, without which every `blkdiscard` below would be remapped
	// to the destination raid0 and unmap blocks it already owns (CN18 step 3).
	create := node.callsMatching(
		"cmd dmsetup create " + cloneName(srv, testClone))
	if len(create) != 1 ||
		!strings.Contains(create[0], "2 no_hydration no_discard_passdown") {
		t.Fatalf("dm-clone features are wrong: %v", create)
	}
	// Exactly one coalesced hydration discard: skip bits 32..63 → 32 MiB at
	// 32 MiB. The arena hole-punch is the other `blkdiscard` of this pass and
	// is counted separately — the two are on different devices and mean
	// completely different things.
	discards := node.callsMatching("cmd blkdiscard --offset 33554432")
	if len(discards) != 1 {
		t.Fatalf("want 1 blkdiscard, got %d: %v", len(discards), discards)
	}
	punches := node.callsMatching(
		"cmd blkdiscard --offset 0 --length 8388608 " + loop)
	if len(punches) != 1 {
		t.Fatalf("want 1 arena hole punch, got %d: %v", len(punches), punches)
	}
	// The ns-dev now sits on the dm-clone, serving despite the stored
	// suspended flag — the auto_resume override of CN16.
	table := node.dms[nsDevName(srv, testNs)].table
	cloneNo := node.devNo["/dev/mapper/"+cloneName(srv, testClone)]
	if !strings.Contains(table, "linear "+cloneNo) {
		t.Fatalf("ns-dev table %q is not on the dm-clone", table)
	}

	info := reply.GetCntlrInfo()
	assertOk(t, info.GetCloneIdToTarget()[testClone], "clone target")
	assertOk(t, info.GetCloneIdToDmClone()[testClone], "dm-clone")
	assertOk(t, info.GetCloneIdToMeta()[testClone], "clone meta")

	// CN20: bm_info_list reports the applied set as chunk_id_list, derived
	// from the files — bm_idx_list stays unset for a clone.
	if len(reply.GetBmInfoList()) != 1 ||
		reply.GetBmInfoList()[0].GetResId() != testClone ||
		len(reply.GetBmInfoList()[0].GetBmIdxList()) != 0 {
		t.Fatalf("bm_info_list is %v", reply.GetBmInfoList())
	}
	if got := chunkIds(reply.GetBmInfoList()[0]); len(got) != 1 ||
		got[0] != [2]uint32{0, 0} {
		t.Fatalf("chunk_id_list is %v, want [(0,0)]", got)
	}
}

func TestCloneAutoResumeOverridesSuspended(t *testing.T) {
	srv, node := newTestServer(t)
	// The destination namespace is created suspended (§11.3) and serves
	// anyway while the clone runs.
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, suspended: true,
		clones: []*pb.Clone{cloneOf()}})
	// The override has to be pinned on the *backing*, not on the device's
	// suspend bit: since the park (§11.6) nothing is ever dm-suspended, so a
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

	// auto_resume = false leaves it effectively suspended — which since
	// 2026-09-16 means **parked**: CN16 rule 1 wins over the clone backing of
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

// TestCloneRecovery is the §11.5 rebuild: the volatile metadata wrapper is
// gone, so the destination thin bitmaps are read and applied before hydration
// is ever enabled, with the ns-devs parked on dm-error throughout.
func TestCloneRecovery(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})

	// The tmpfs arena did not survive: drop the wrapper (and the dm-clone
	// that mapped it) behind the agent's back, exactly as a CN reboot does.
	metaDm := cloneMetaName(srv, testClone)
	loop := loopDev(t, srv, node)
	removeCloneDevice(node, srv)
	delete(node.dms, metaDm)
	delete(node.devNo, "/dev/mapper/"+metaDm)
	delete(node.devSize, "/dev/mapper/"+metaDm)

	// The destination has blocks 0..3 mapped ⇒ regions 0..3 are already
	// copied and must be discarded before hydration starts.
	metaPath := srv.nf.DmPath(srv.nf.CnPoolMetaName(
		testCluster, testCn, testSp, testSlice))
	node.thinDumps[metaPath] = `<superblock uuid="" time="0" transaction="0" ` +
		`flags="0" version="2" data_block_size="2048" nr_data_blocks="0">
  <device dev_id="1" mapped_blocks="4" transaction="0" creation_time="0" snap_time="0">
    <range_mapping origin_begin="0" data_begin="0" length="4" time="0"/>
  </device>
</superblock>
`

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true,
		clones: []*pb.Clone{cloneOf()}})); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	pool := poolName(srv)
	assertOrder(t, node,
		"cmd dmsetup reload "+nsDevName(srv, testNs),
		"cmd blkdiscard --offset 0 --length 8388608 "+loop,
		"cmd dmsetup create "+metaDm,
		"cmd dmsetup create "+cloneName(srv, testClone),
		"cmd dmsetup message "+pool+" 0 reserve_metadata_snap",
		"cmd thin_dump --metadata-snap "+metaPath,
		"cmd dmsetup message "+pool+" 0 release_metadata_snap",
		"cmd blkdiscard --offset 0 --length 4194304",
		"cmd dmsetup message "+cloneName(srv, testClone)+
			" 0 enable_hydration",
		"cmd dmsetup reload "+nsDevName(srv, testNs),
	)
	// Parking comes before the metadata snapshot is even taken (§11.5 step
	// 1): nothing may serve the td while the bitmaps are being applied.
	park := node.indexOfCall("cmd dmsetup reload " + nsDevName(srv, testNs))
	snap := node.indexOfCall(
		"cmd dmsetup message " + pool + " 0 reserve_metadata_snap")
	if park > snap {
		t.Fatalf("the ns-dev was not parked before the snapshot")
	}
}

// seedKilledCloneBuild leaves the node the way an agent killed after CN18 step
// 3 created the dm-clone, and before step 5 enabled its hydration, leaves it:
// the ns-dev parked on the td's dm-error by the killed pass's step 2, the
// matching wrapper that pass kept, and the dm-clone its step 3 created with
// hydration off — no destination bitmap applied, no chunk re-applied, the
// chunk file still in the store. The destination pool's metadata has blocks
// 0..3 mapped, so regions 0..3 are already copied and a recovery must
// discard them (one 4 MiB discard at offset 0) before hydration starts. It
// returns the dm-clone's name and the pool metadata path the recovery dumps.
func seedKilledCloneBuild(
	t *testing.T,
	srv *CnAgentServer,
	node *fakeNode,
) (string, string) {
	t.Helper()
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	pushBitmap(t, srv, hexBytes(t, testSkipHex))
	clone := cloneName(srv, testClone)

	node.mu.Lock()
	errNo := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
	node.dms[nsDevName(srv, testNs)].table =
		agent.LinearTable(testTdSize/512, errNo, 0)
	table := node.dms[clone].table
	removeCloneDevice(node, srv)
	_, code := node.dmCreate(
		[]string{"create", clone, "--table", table}, "")
	node.mu.Unlock()
	if code != 0 || !node.dms[clone].noHydration {
		t.Fatalf("seeding the killed pass's dm-clone failed (%d)", code)
	}
	assertParked(t, srv, node, testNs, testTd, "the killed pass")

	metaPath := srv.nf.DmPath(srv.nf.CnPoolMetaName(
		testCluster, testCn, testSp, testSlice))
	node.thinDumps[metaPath] = `<superblock uuid="" time="0" ` +
		`transaction="0" flags="0" version="2" data_block_size="2048" ` +
		`nr_data_blocks="0">
  <device dev_id="1" mapped_blocks="4" transaction="0" creation_time="0" snap_time="0">
    <range_mapping origin_begin="0" data_begin="0" length="4" time="0"/>
  </device>
</superblock>
`
	return clone, metaPath
}

// TestCloneRecoveryResumesAfterAKillBetweenCreateAndBitmaps is the crash
// window inside a build: an agent killed after CN18 step 3 created the
// dm-clone and before step 4 applied the destination bitmaps leaves nothing
// missing — the wrapper still matches and the dm-clone is up — so the one
// sign that step 4 never finished is the dm-clone's own `no_hydration`. The
// next pass must run the §11.5 recovery again before it enables hydration:
// enabling it straight away re-fetches every region the destination already
// owns, stale source bytes over newer local ones. The stale dm-clone may
// also survive the recovery's removal (a refused `dmsetup remove`); the
// bitmaps then land on the surviving device, because they belong to the
// recovery, not to the create. And the device under the clone's name may
// not show a dm-clone's status at all, which is no mark of step 5 either.
func TestCloneRecoveryResumesAfterAKillBetweenCreateAndBitmaps(t *testing.T) {
	for _, tc := range []struct {
		name     string
		survives bool
		notClone bool
	}{
		{name: "the stale dm-clone is replaced"},
		{name: "the stale dm-clone survives its removal", survives: true},
		{name: "the device under the clone's name is not a dm-clone",
			notClone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			clone, metaPath := seedKilledCloneBuild(t, srv, node)
			metaDm := cloneMetaName(srv, testClone)
			loop := loopDev(t, srv, node)
			if tc.notClone {
				// A status that does not parse as a dm-clone's shows no
				// hydration (cloneHydrating) — a dm-clone in `Fail` mode
				// prints `Fail` where its fields go, and an error table
				// stands in for such a device here. Read as hydrating, it
				// would skip the recovery: the table's reload would count as
				// a create, and step 5 would follow step 4's source chunks
				// alone.
				node.mu.Lock()
				node.dms[clone].table = agent.ErrorTable(testTdSize / 512)
				node.mu.Unlock()
			}
			if tc.survives {
				node.failCmd["dmsetup remove "+clone] =
					"device-mapper: remove ioctl on " + clone +
						" failed: Device or resource busy"
			}

			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 3, primary: true,
					clones: []*pb.Clone{cloneOf()}}))
			if err != nil {
				t.Fatalf("the pass after the kill: %v", err)
			}
			dev := "/dev/mapper/" + clone
			dstDiscard := "cmd blkdiscard --offset 0 --length 4194304 " + dev
			enable := "cmd dmsetup message " + clone + " 0 enable_hydration"
			reload := "cmd dmsetup reload " + nsDevName(srv, testNs)
			// (a) the destination regions — read from the destination pool's
			// metadata snapshot — and the pushed source chunk are marked
			// hydrated on the dm-clone before hydration is enabled, and only
			// then does the ns-dev leave the park (CN18 steps 4-6).
			assertOrder(t, node, dstDiscard, enable, reload)
			assertOrder(t, node,
				"cmd thin_dump --metadata-snap "+metaPath, dstDiscard)
			assertOrder(t, node,
				"cmd blkdiscard --offset 33554432 --length 33554432 "+dev,
				enable)
			if node.indexOfCall(reload) < node.indexOfCall(enable) {
				t.Fatalf("the ns-dev left the park before hydration was " +
					"enabled")
			}
			// (b) once each: one destination-bitmap read, one discard of its
			// regions, one enable.
			for _, call := range []string{"cmd thin_dump", dstDiscard, enable} {
				if got := node.callsMatching(call); len(got) != 1 {
					t.Fatalf("%q ran %d times, want 1: %v",
						call, len(got), got)
				}
			}
			// (c) the clone reads OK, and a pass that finished the build
			// leaves no CN10 retry behind.
			info := reply.GetCntlrInfo()
			assertOk(t, info.GetCloneIdToDmClone()[testClone], "dm-clone")
			assertOk(t, info.GetCloneIdToMeta()[testClone], "clone meta")
			assertOk(t, info.GetNsIdToDmLinear()[testNs], "ns-dev")
			if retrying(t, srv) {
				t.Fatalf("a recovery that finished registered the retry")
			}
			// The matching wrapper is left alone: re-punching it would wipe
			// the dm-clone superblock the killed pass wrote (CN18).
			assertNoCall(t, node, "cmd dmsetup create "+metaDm)
			assertNoCall(t, node,
				"cmd blkdiscard --offset 0 --length 8388608 "+loop)
			// The stale dm-clone goes before a fresh one is created over the
			// same wrapper (§11.5 steps 1-2); one whose removal is refused is
			// kept, and the bitmaps above landed on it.
			if tc.survives {
				if !node.hasCall("cmd dmsetup remove " + clone) {
					t.Fatalf("no removal of the stale dm-clone was tried; " +
						"the case is vacuous")
				}
				assertNoCall(t, node, "cmd dmsetup create "+clone)
			} else {
				assertOrder(t, node,
					"cmd dmsetup remove "+clone,
					"cmd dmsetup create "+clone,
					dstDiscard)
			}
		})
	}
}

// TestCloneRecoveryNeverServesAnUnfinishedDmClone holds §11.5's "fully
// applied before the dm-clone handles any IO" against a second fault. From
// the state the kill leaves, one more failure stops the next pass short of
// CN18 step 5: the arena listing does not answer, the dm-clone's status read
// does not answer (once, or every time), the recovery's own `dmsetup create`
// is killed after its ioctl ran, the destination bitmaps cannot be read while
// the stale dm-clone refuses to go, or the enable is refused. Each leaves a
// dm-clone up with hydration off, which is exactly what CN16 must not serve
// through: the ns-dev stays parked (rule 5 takes the dm-clone only while its
// status shows hydration on), the Check round reads that park as the state
// CN16 wants — not as a table to repair — and the pass registers the CN10
// retry, whose next attempt, the fault gone, finishes the recovery and moves
// the ns-dev on. The refused enable runs once more at SP_LEVEL_READONLY,
// where rule 7 puts the rule-5 backing under dm-flakey: the park is still
// rule 1's plain dm-linear, never a flakey table over the td's dm-error.
func TestCloneRecoveryNeverServesAnUnfinishedDmClone(t *testing.T) {
	enableOf := func(clone string) string {
		return "cmd dmsetup message " + clone + " 0 enable_hydration"
	}
	refuseEnable := func(node *fakeNode, clone string) {
		node.failCmd[enableOf(clone)[len("cmd "):]] =
			"device-mapper: message ioctl on " + clone +
				" failed: Invalid argument"
	}
	enableRefused := func(t *testing.T, node *fakeNode, info *pb.CntlrInfo,
		clone string) {
		// Step 4 finished; only the enable did not.
		assertOrder(t, node,
			"cmd blkdiscard --offset 0 --length 4194304 "+
				"/dev/mapper/"+clone,
			enableOf(clone))
		assertErrorDetails(t, info.GetCloneIdToDmClone()[testClone],
			"Invalid argument", "dm-clone")
	}
	for _, tc := range []struct {
		name  string
		level pb.SpLevel
		fault func(node *fakeNode, clone string)
		clear func(node *fakeNode)
		// pass checks the failed pass beyond what every case shares.
		pass func(t *testing.T, node *fakeNode, info *pb.CntlrInfo,
			clone string)
		// nsErr is what the ns-dev row carries when the pass could not read
		// the dm-clone's status at all; "" means the row reads OK.
		nsErr string
	}{
		{
			name: "the arena does not answer",
			fault: func(node *fakeNode, _ string) {
				node.killCmd["losetup --associated"] = true
			},
			clear: func(*fakeNode) {},
			pass: func(t *testing.T, node *fakeNode, info *pb.CntlrInfo,
				clone string) {
				// Step 2 stopped before it could tell a recovery: nothing is
				// removed, created, read or enabled.
				assertNoCall(t, node, "cmd dmsetup remove "+clone)
				assertNoCall(t, node, "cmd dmsetup create "+clone)
				assertNoCall(t, node, "cmd thin_dump")
				assertNoCall(t, node, enableOf(clone))
				assertErrorDetails(t, info.GetCloneIdToMeta()[testClone],
					"clone-metadata arena unavailable", "clone meta")
				assertErrorDetails(t, info.GetCloneIdToDmClone()[testClone],
					detailsCloneMetaMissing, "dm-clone")
			},
		},
		{
			name: "the status read does not answer",
			fault: func(node *fakeNode, clone string) {
				node.killCmd["dmsetup status "+clone] = true
			},
			clear: func(*fakeNode) {},
			pass: func(t *testing.T, node *fakeNode, info *pb.CntlrInfo,
				clone string) {
				// Step 2 decided nothing: no park-and-rebuild, no bitmap
				// read, no enable, and both rows carry the read's failure.
				assertNoCall(t, node, "cmd dmsetup remove "+clone)
				assertNoCall(t, node, "cmd dmsetup create "+clone)
				assertNoCall(t, node, "cmd thin_dump")
				assertNoCall(t, node, enableOf(clone))
				assertErrorDetails(t, info.GetCloneIdToDmClone()[testClone],
					"signal: killed", "dm-clone")
				assertErrorDetails(t, info.GetCloneIdToMeta()[testClone],
					"signal: killed", "clone meta")
			},
		},
		{
			name: "no status read answers",
			fault: func(node *fakeNode, clone string) {
				node.killCmdAlways["dmsetup status "+clone] = true
			},
			clear: func(node *fakeNode) {
				clear(node.killCmdAlways)
			},
			pass: func(t *testing.T, node *fakeNode, info *pb.CntlrInfo,
				clone string) {
				assertNoCall(t, node, "cmd dmsetup remove "+clone)
				assertNoCall(t, node, "cmd dmsetup create "+clone)
				assertNoCall(t, node, "cmd thin_dump")
				assertNoCall(t, node, enableOf(clone))
				assertErrorDetails(t, info.GetCloneIdToDmClone()[testClone],
					"signal: killed", "dm-clone")
				assertErrorDetails(t, info.GetCloneIdToMeta()[testClone],
					"signal: killed", "clone meta")
			},
			nsErr: "signal: killed",
		},
		{
			name: "the recovery's create does not answer",
			fault: func(node *fakeNode, clone string) {
				node.killCmd["dmsetup create "+clone+" "] = true
			},
			clear: func(*fakeNode) {},
			pass: func(t *testing.T, node *fakeNode, info *pb.CntlrInfo,
				clone string) {
				// The stale dm-clone went and the new one is up — the ioctl
				// ran — with hydration off and not a bitmap applied.
				assertOrder(t, node, "cmd dmsetup remove "+clone,
					"cmd dmsetup create "+clone)
				assertNoCall(t, node, "cmd thin_dump")
				assertNoCall(t, node, enableOf(clone))
				assertErrorDetails(t, info.GetCloneIdToDmClone()[testClone],
					"signal: killed", "dm-clone")
			},
		},
		{
			name: "the bitmaps are unreadable and the stale dm-clone stays",
			fault: func(node *fakeNode, clone string) {
				node.failCmdAlways["dmsetup remove "+clone] =
					"device-mapper: remove ioctl on " + clone +
						" failed: Device or resource busy"
				node.failCmdAlways["thin_dump"] = "thin_dump: bad checksum"
			},
			clear: func(node *fakeNode) {
				clear(node.failCmdAlways)
			},
			pass: func(t *testing.T, node *fakeNode, info *pb.CntlrInfo,
				clone string) {
				// Step 4 failed closed, and its removal failed too: the
				// stale dm-clone is still up.
				if got := node.callsMatching(
					"cmd dmsetup remove " + clone); len(got) != 2 {
					t.Fatalf("want the step 2 and the step 4 removal, "+
						"got %v", got)
				}
				assertNoCall(t, node, enableOf(clone))
				assertErrorDetails(t, info.GetCloneIdToDmClone()[testClone],
					"destination bitmaps not applied", "dm-clone")
			},
		},
		{
			name:  "the enable is refused",
			fault: refuseEnable,
			clear: func(*fakeNode) {},
			pass:  enableRefused,
		},
		{
			name:  "the enable is refused at SP_LEVEL_READONLY",
			level: pb.SpLevel_SP_LEVEL_READONLY,
			fault: refuseEnable,
			clear: func(*fakeNode) {},
			pass:  enableRefused,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			clone, _ := seedKilledCloneBuild(t, srv, node)
			nsDev := nsDevName(srv, testNs)
			tc.fault(node, clone)

			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 3, primary: true,
					level: tc.level, clones: []*pb.Clone{cloneOf()}}))
			if err != nil {
				t.Fatalf("the pass after the kill: %v", err)
			}
			info := reply.GetCntlrInfo()
			tc.pass(t, node, info, clone)
			// What every case shares: a dm-clone is up with hydration off,
			// and the ns-dev never went near it.
			if dm := node.dms[clone]; dm == nil || !dm.noHydration {
				t.Fatalf("the case is vacuous: no dm-clone with hydration "+
					"off is up (%v)", dm)
			}
			assertParked(t, srv, node, testNs, testTd, "the failed pass")
			assertNoCall(t, node, "cmd dmsetup reload "+nsDev)
			if tc.nsErr != "" {
				assertErrorDetails(t, info.GetNsIdToDmLinear()[testNs],
					tc.nsErr, "ns-dev")
			} else {
				assertOk(t, info.GetNsIdToDmLinear()[testNs], "ns-dev")
			}
			if !retrying(t, srv) {
				t.Fatalf("a pass that stopped short of the enable " +
					"registered no retry")
			}

			// The Check round agrees with the converge: the park is what
			// CN16 wants while hydration is off, and a probe never mutates.
			node.Reset()
			_, probed := srv.checkCntlrRound(context.Background(),
				&pb.CheckCntlrRequest{
					ClusterId: testCluster, CnId: testCn,
					CntlrPointer: cntlrPtr(), Revision: 3,
				}, nil)
			if tc.nsErr != "" {
				assertErrorDetails(t, probed.GetNsIdToDmLinear()[testNs],
					tc.nsErr, "probed ns-dev")
			} else {
				assertOk(t, probed.GetNsIdToDmLinear()[testNs],
					"probed ns-dev")
			}
			for _, call := range node.Mutations() {
				t.Fatalf("the check round mutated: %q", call)
			}

			// The retry's next attempt, the fault gone, recovers the clone:
			// the destination regions are discarded before the one enable,
			// and only then does the ns-dev move onto the dm-clone.
			tc.clear(node)
			node.Reset()
			retryAttempt(t, srv)
			dstDiscard := "cmd blkdiscard --offset 0 --length 4194304 " +
				"/dev/mapper/" + clone
			assertOrder(t, node, dstDiscard, enableOf(clone),
				"cmd dmsetup reload "+nsDev)
			for _, call := range []string{"cmd thin_dump", dstDiscard,
				enableOf(clone)} {
				if got := node.callsMatching(call); len(got) != 1 {
					t.Fatalf("the retry ran %q %d times, want 1: %v",
						call, len(got), got)
				}
			}
			cloneNo := node.devNo["/dev/mapper/"+clone]
			want := agent.LinearTable(testTdSize/512, cloneNo, 0)
			if tc.level == pb.SpLevel_SP_LEVEL_READONLY {
				want = agent.FlakeyErrorWritesTable(testTdSize/512, cloneNo)
			}
			if got := node.dms[nsDev].table; got != want {
				t.Fatalf("after the retry the ns-dev table is %q, want the "+
					"dm-clone %q", got, want)
			}
			if retrying(t, srv) {
				t.Fatalf("the retry outlived the recovery it finished")
			}
		})
	}
}

// TestCloneRecoveryRetriesAReadAfterTheEnable holds the CN10 retry to the two
// `dmsetup status` reads that follow a recovery's enable. From the state a
// killed build leaves, one pass runs the recovery through CN18 step 5 and
// then reads the dm-clone's status twice more: CN18's own read for the
// dm-clone row (§9.5), then CN16's, which decides whether the ns-dev may
// leave the park (rule 5). Either may not answer. Hydration is on by then,
// so the next pass redoes nothing of the recovery — but the dm-clone row
// the first read leaves ERROR, and the parked ns-dev the second leaves on
// the td's dm-error under an `optimized` namespace, are both waiting for a
// converge, and the worker re-syncs on a revision or a reply code, never on
// a row. So either failure registers the retry, whose next attempt reads
// again, moves what is left and stops.
func TestCloneRecoveryRetriesAReadAfterTheEnable(t *testing.T) {
	for _, tc := range []struct {
		name string
		// nth is the `dmsetup status` of the dm-clone the pass kills,
		// counted from the pass's start: the recovery reads it at step 2,
		// for step 3's knobs and for step 5's enable before these two.
		nth int
		// pass checks the failed pass beyond what both cases share.
		pass func(t *testing.T, srv *CnAgentServer, node *fakeNode,
			info *pb.CntlrInfo, onClone string)
		// reloads is how often the retry's attempt reloads the ns-dev: once
		// for the one the failed pass left parked, never for one it moved.
		reloads int
	}{
		{
			name: "the dm-clone row's read does not answer",
			nth:  4,
			pass: func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				info *pb.CntlrInfo, onClone string) {
				assertErrorDetails(t, info.GetCloneIdToDmClone()[testClone],
					"signal: killed", "dm-clone")
				// CN16's own read answered: the ns-dev is on the dm-clone.
				if got := node.dms[nsDevName(srv, testNs)].table; got !=
					onClone {
					t.Fatalf("the ns-dev table is %q, want the dm-clone %q",
						got, onClone)
				}
				assertOk(t, info.GetNsIdToDmLinear()[testNs], "ns-dev")
			},
		},
		{
			name: "the namespace step's read does not answer",
			nth:  5,
			pass: func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				info *pb.CntlrInfo, _ string) {
				assertOk(t, info.GetCloneIdToDmClone()[testClone], "dm-clone")
				// The read licensed no move: the ns-dev keeps the park the
				// recovery put it on, and its row names the read.
				assertParked(t, srv, node, testNs, testTd, "the failed pass")
				assertErrorDetails(t, info.GetNsIdToDmLinear()[testNs],
					"signal: killed", "ns-dev")
			},
			reloads: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			clone, _ := seedKilledCloneBuild(t, srv, node)
			nsDev := nsDevName(srv, testNs)
			dev := "/dev/mapper/" + clone
			enable := "cmd dmsetup message " + clone + " 0 enable_hydration"
			node.killCmdNth["dmsetup status "+clone] = tc.nth

			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 3, primary: true,
					clones: []*pb.Clone{cloneOf()}}))
			if err != nil {
				t.Fatalf("the pass after the kill: %v", err)
			}
			if len(node.killCmdNth) != 0 {
				t.Fatalf("the pass read the dm-clone's status fewer than "+
					"%d times; the case is vacuous", tc.nth)
			}
			// The recovery itself ran in full, once, before the read that
			// failed: hydration is on.
			dstDiscard := "cmd blkdiscard --offset 0 --length 4194304 " + dev
			for _, call := range []string{"cmd thin_dump", dstDiscard,
				enable} {
				if got := node.callsMatching(call); len(got) != 1 {
					t.Fatalf("%q ran %d times, want 1: %v",
						call, len(got), got)
				}
			}
			if node.dms[clone].noHydration {
				t.Fatalf("the recovery left hydration off")
			}
			onClone := agent.LinearTable(testTdSize/512,
				node.devNo[dev], 0)
			tc.pass(t, srv, node, reply.GetCntlrInfo(), onClone)
			if !retrying(t, srv) {
				t.Fatalf("a read after the enable that did not answer " +
					"registered no retry")
			}

			// The retry's attempt redoes nothing of the recovery — hydration
			// is on — and leaves the ns-dev on the dm-clone, every row OK,
			// and no retry.
			node.Reset()
			retryAttempt(t, srv)
			for _, call := range []string{"cmd thin_dump", "cmd blkdiscard",
				enable, "cmd dmsetup remove " + clone,
				"cmd dmsetup create " + clone} {
				assertNoCall(t, node, call)
			}
			if got := node.dms[nsDev].table; got != onClone {
				t.Fatalf("after the retry the ns-dev table is %q, want the "+
					"dm-clone %q", got, onClone)
			}
			if got := node.callsMatching(
				"cmd dmsetup reload " + nsDev); len(got) != tc.reloads {
				t.Fatalf("the retry reloaded the ns-dev %d times, want %d: "+
					"%v", len(got), tc.reloads, got)
			}
			info := probedCntlrInfo(t, srv)
			assertOk(t, info.GetCloneIdToDmClone()[testClone], "dm-clone")
			assertOk(t, info.GetNsIdToDmLinear()[testNs], "ns-dev")
			if retrying(t, srv) {
				t.Fatalf("the retry outlived the reads it re-ran")
			}
		})
	}
}

func TestCloneMetadataSnapAlwaysReleased(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	metaPath := srv.nf.DmPath(srv.nf.CnPoolMetaName(
		testCluster, testCn, testSp, testSlice))
	node.Reset()
	node.failCmdAlways["thin_dump"] = "thin_dump: corrupt metadata"

	_, err := srv.GetThinDeviceBm(context.Background(),
		&pb.GetThinDeviceBmRequest{
			ClusterId: testCluster, CnId: testCn, SpId: testSp,
			CntlrId: testCntlr, TdId: testTd, SliceIdx: 0,
		})
	if err == nil {
		t.Fatalf("a failing thin_dump must fail the RPC")
	}
	pool := poolName(srv)
	assertOrder(t, node,
		"cmd dmsetup message "+pool+" 0 reserve_metadata_snap",
		"cmd thin_dump --metadata-snap "+metaPath,
		"cmd dmsetup message "+pool+" 0 release_metadata_snap",
	)
	if node.dms[pool].heldRoot {
		t.Fatalf("the metadata snapshot leaked")
	}
}

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
	// The source's disconnect runs off the pass's locks (CN21), so it is the
	// pass's one leftover, and it is set going only once the clone and its
	// metadata wrapper are gone.
	cnSweepOnlyDisconnects(t, reply.GetAgentReply(), "delete clone")
	cnSweepAssertDetails(t, reply.GetAgentReply(), testSrcNqn, "delete clone")
	awaitDisconnects(t, srv)
	assertOrder(t, node,
		"cmd dmsetup reload "+nsDevName(srv, testNs),
		"cmd dmsetup remove "+cloneName(srv, testClone),
		"cmd dmsetup remove "+cloneMetaName(srv, testClone),
		"cmd rm -f "+srv.nf.LocalCloneBmPath(
			testCluster, testCn, testSp, testClone, 0, 0),
	)
	assertOrder(t, node,
		"cmd dmsetup remove "+cloneMetaName(srv, testClone),
		"cmd nvme disconnect --nqn "+testSrcNqn,
	)
	// The ns-dev is back on the raid0, all data local.
	table := node.dms[nsDevName(srv, testNs)].table
	raid0No := node.devNo["/dev/mapper/"+raid0Name(srv, testTd)]
	if !strings.Contains(table, "linear "+raid0No) {
		t.Fatalf("ns-dev table %q is not back on the raid0", table)
	}
}

// ---------------------------------------------------------------------------
// §6.9 — the clone-metadata slot allocator (CN18 step 2, [D14])
// ---------------------------------------------------------------------------

// twoTds is the fixture for the multi-clone allocator cases: a second
// destination td, so two clones can be built at once.
func twoTds() []*pb.ThinDevice {
	return []*pb.ThinDevice{
		{TdId: testTd, DevId: 1, Size: testTdSize},
		{TdId: testSnapTd, DevId: 2, Size: testTdSize},
	}
}

// wrapperTable is the kind-`b` table one clone's slot must carry: a single
// linear over the arena's loop device at its own unit offset.
func wrapperTable(node *fakeNode, loop string, unitStart uint64) string {
	return fmt.Sprintf("0 %d linear %s %d",
		2*cnCloneMetaUnitSectors, node.devNo[loop],
		unitStart*cnCloneMetaUnitSectors)
}

// TestCloneMetaFirstFitAndRecycling pins the allocator: contiguous units,
// first fit over the free runs, and a run freed by a retired clone handed out
// again — hole-punched **before** anything is created, because a freed unit
// still holds the previous clone's valid dm-clone superblock.
func TestCloneMetaFirstFitAndRecycling(t *testing.T) {
	srv, node := newTestServer(t)
	first := cloneOf()
	second := cloneOfTd(testClone2, testSrcNqn2, testSnapTd)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: twoTds(),
		clones: []*pb.Clone{first, second}})
	loop := loopDev(t, srv, node)

	// 2 units each (CN18 step 2), so the second clone lands directly after
	// the first: units [0,2) and [2,4).
	firstDm := cloneMetaName(srv, testClone)
	secondDm := cloneMetaName(srv, testClone2)
	if got, want := node.dms[firstDm].table,
		wrapperTable(node, loop, 0); got != want {
		t.Fatalf("first wrapper table is %q, want %q", got, want)
	}
	if got, want := node.dms[secondDm].table,
		wrapperTable(node, loop, 2); got != want {
		t.Fatalf("second wrapper table is %q, want %q", got, want)
	}
	if !node.hasCall(
		"cmd blkdiscard --offset 8388608 --length 8388608 " + loop) {
		t.Fatalf("the second slot was not hole-punched:\n%s",
			strings.Join(node.Calls(), "\n"))
	}

	// Retiring the first clone removes its wrapper, so its units are free
	// again in the next enumeration — there is no allocation table to update.
	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, tds: twoTds(),
		clones: []*pb.Clone{second}})); err != nil {
		t.Fatalf("retire the first clone: %v", err)
	}
	// Its source's disconnect runs off the pass (CN21) and changes the node
	// under the reads below.
	awaitDisconnects(t, srv)
	if _, ok := node.dms[firstDm]; ok {
		t.Fatalf("the retired clone kept its metadata wrapper")
	}

	// A third clone first-fits back into the recycled run.
	node.Reset()
	third := cloneOfTd(testClone3, testSrcNqn3, testTd)
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 4, primary: true, tds: twoTds(),
		clones: []*pb.Clone{second, third}})); err != nil {
		t.Fatalf("third clone: %v", err)
	}
	thirdDm := cloneMetaName(srv, testClone3)
	assertOrder(t, node,
		"cmd blkdiscard --offset 0 --length 8388608 "+loop,
		"cmd dmsetup create "+thirdDm,
	)
	if got, want := node.dms[thirdDm].table,
		wrapperTable(node, loop, 0); got != want {
		t.Fatalf("recycled wrapper table is %q, want %q", got, want)
	}
	// The surviving clone's slot is left strictly alone.
	if got, want := node.dms[secondDm].table,
		wrapperTable(node, loop, 2); got != want {
		t.Fatalf("surviving wrapper table is %q, want %q", got, want)
	}
	assertNoCall(t, node, "cmd dmsetup create "+secondDm)
}

// TestCloneMetaArenaExhaustion is the old lvcreate-ENOSPC equivalent: with no
// contiguous run left the clone's metadata and dm-clone rows go ERROR and
// nothing is built, while the pass itself carries on (CN29).
func TestCloneMetaArenaExhaustion(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	loop := loopDev(t, srv, node)

	// A kind-`cb` wrapper claiming every unit of the arena, planted behind
	// the agent's back. The arena is per CN, so a wrapper of ANOTHER sp
	// exhausts it just as well — and it has to be another sp's, because this
	// cntlr's own sweep would remove a wrapper of testSp that no clone of
	// testSp's request names.
	const fillerSp = testSp + 1
	filler := srv.nf.CnCloneMetaDmName(testCluster, testCn, fillerSp, 0x999)
	node.dms[filler] = &fakeDm{
		table: fmt.Sprintf("0 %d linear %s 0",
			cnCloneMetaUnitCnt*cnCloneMetaUnitSectors, node.devNo[loop]),
		thinIds: map[uint32]bool{},
	}
	node.devNo["/dev/mapper/"+filler] = "253:200"

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("exhausted arena: %v", err)
	}
	meta := reply.GetCntlrInfo().GetCloneIdToMeta()[testClone]
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(meta.GetDetails(),
			"no contiguous run of 2 clone-metadata units in the "+
				"256-unit arena") {
		t.Fatalf("clone_id_to_meta is %v/%q",
			meta.GetStatus(), meta.GetDetails())
	}
	dm := reply.GetCntlrInfo().GetCloneIdToDmClone()[testClone]
	if dm.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		dm.GetDetails() != "metadata wrapper missing" {
		t.Fatalf("clone_id_to_dm_clone is %v/%q",
			dm.GetStatus(), dm.GetDetails())
	}
	assertNoCall(t, node, "cmd dmsetup create "+cloneName(srv, testClone))
	// The source connection is fine, so its row is not dragged down with it.
	assertOk(t, reply.GetCntlrInfo().GetCloneIdToTarget()[testClone],
		"clone target")

	// The CN24 check round must report the *same* pair. ResTracker.Set
	// overwrites, so a probe that answered the plain "no wrapper, no dm-clone"
	// MISSING here would replace the converge's verdict, and the two channels
	// would trade these two rows — and the `epoch` on them — on every round.
	node.Reset()
	_, probed := srv.checkCntlrRound(context.Background(),
		&pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: 3,
		}, nil)
	probedMeta := probed.GetCloneIdToMeta()[testClone]
	if probedMeta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		probedMeta.GetDetails() != meta.GetDetails() {
		t.Fatalf("the check round flipped clone_id_to_meta to %v/%q, want the "+
			"converge's ERROR/%q",
			probedMeta.GetStatus(), probedMeta.GetDetails(), meta.GetDetails())
	}
	probedDm := probed.GetCloneIdToDmClone()[testClone]
	if probedDm.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		probedDm.GetDetails() != "metadata wrapper missing" {
		t.Fatalf("the check round flipped clone_id_to_dm_clone to %v/%q, want "+
			"the converge's ERROR/%q", probedDm.GetStatus(),
			probedDm.GetDetails(), "metadata wrapper missing")
	}
	// The source is still connected on this round, which is what lets the probe
	// answer for CN18 step 2 at all: a clone whose step 1 has not succeeded is
	// one the converge never carried that far.
	assertOk(t, probed.GetCloneIdToTarget()[testClone], "probed clone target")
	// Preserving that verdict must not cost the probe its read-only contract
	// (CN23): the arena question is answered from this pass's enumeration, with
	// no allocation, no hole punch and no `dmsetup create`.
	for _, call := range node.Mutations() {
		t.Fatalf("the check round mutated: %q", call)
	}
}

// TestCloneMetaAbsentWrapperWithRoomProbesMissing is the other side of the
// exhaustion pair, and the bound on it: a wrapper that is merely not built yet
// — here the §11.5 case, the volatile wrapper and the dm-clone that mapped it
// both lost — is a truthful RES_STATUS_MISSING while the arena still has room,
// not the CN18 step 2 refusal. The converge agrees: its next pass allocates the
// slot and rebuilds (TestCloneRecovery), which is the opposite of reporting
// that the arena could not supply it.
func TestCloneMetaAbsentWrapperWithRoomProbesMissing(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})

	// The wrapper and the dm-clone that mapped it go behind the agent's back,
	// exactly as a CN reboot loses them. No other wrapper claims a unit, so the
	// whole 256-unit arena is free.
	metaDm := cloneMetaName(srv, testClone)
	removeCloneDevice(node, srv)
	delete(node.dms, metaDm)
	delete(node.devNo, "/dev/mapper/"+metaDm)
	delete(node.devSize, "/dev/mapper/"+metaDm)

	node.Reset()
	_, probed := srv.checkCntlrRound(context.Background(),
		&pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: 2,
		}, nil)
	meta := probed.GetCloneIdToMeta()[testClone]
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_MISSING ||
		meta.GetDetails() != "" {
		t.Fatalf("clone_id_to_meta is %v/%q, want MISSING/\"\"",
			meta.GetStatus(), meta.GetDetails())
	}
	dm := probed.GetCloneIdToDmClone()[testClone]
	if dm.GetStatus() != pb.ResStatus_RES_STATUS_MISSING ||
		dm.GetDetails() != "" {
		t.Fatalf("clone_id_to_dm_clone is %v/%q, want MISSING/\"\"",
			dm.GetStatus(), dm.GetDetails())
	}
	for _, call := range node.Mutations() {
		t.Fatalf("the check round mutated: %q", call)
	}
}

// TestCloneMetaExhaustedArenaWithDisconnectedSourceProbesMissing pins the step
// 1 gate in front of the refusal pair, in the direction the two tests above
// cannot reach. A converge whose source connection fails stops at CN18 step 1
// and reports `clone_id_to_dm_clone` MISSING "source not connected" with the
// meta row left to cloneMetaInfo, which reads the enumeration for the wrapper's
// own row but never asks whether the arena could supply a slot — however full
// the arena happens to be. A probe that asked anyway would invent an
// ERROR/ERROR pair against that, and this one flips *both* rows, the meta row
// MISSING→ERROR, which is what the worker's health pass keys on.
func TestCloneMetaExhaustedArenaWithDisconnectedSourceProbesMissing(
	t *testing.T,
) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	loop := loopDev(t, srv, node)

	// The same filler as TestCloneMetaArenaExhaustion: one kind-`b` wrapper
	// claiming every unit, so the arena genuinely has no run left.
	filler := srv.nf.CnCloneMetaDmName(testCluster, testCn, testSp, 0x999)
	node.dms[filler] = &fakeDm{
		table: fmt.Sprintf("0 %d linear %s 0",
			cnCloneMetaUnitCnt*cnCloneMetaUnitSectors, node.devNo[loop]),
		thinIds: map[uint32]bool{},
	}
	node.devNo["/dev/mapper/"+filler] = "253:200"
	// ...and the source refuses the connect, so step 1 never completes.
	node.failCmdAlways["nvme connect"] = "nvme connect: Connection refused"

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("disconnected source: %v", err)
	}
	dm := reply.GetCntlrInfo().GetCloneIdToDmClone()[testClone]
	if dm.GetStatus() != pb.ResStatus_RES_STATUS_MISSING ||
		dm.GetDetails() != "source not connected" {
		t.Fatalf("converged clone_id_to_dm_clone is %v/%q, want "+
			"MISSING/\"source not connected\"",
			dm.GetStatus(), dm.GetDetails())
	}
	meta := reply.GetCntlrInfo().GetCloneIdToMeta()[testClone]
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_MISSING ||
		meta.GetDetails() != "" {
		t.Fatalf("converged clone_id_to_meta is %v/%q, want MISSING/\"\"",
			meta.GetStatus(), meta.GetDetails())
	}

	node.Reset()
	_, probed := srv.checkCntlrRound(context.Background(),
		&pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: 3,
		}, nil)
	probedMeta := probed.GetCloneIdToMeta()[testClone]
	if probedMeta.GetStatus() != pb.ResStatus_RES_STATUS_MISSING ||
		probedMeta.GetDetails() != "" {
		t.Fatalf("the check round flipped clone_id_to_meta to %v/%q, want "+
			"MISSING/\"\"", probedMeta.GetStatus(), probedMeta.GetDetails())
	}
	probedDm := probed.GetCloneIdToDmClone()[testClone]
	if probedDm.GetStatus() != pb.ResStatus_RES_STATUS_MISSING {
		t.Fatalf("the check round flipped clone_id_to_dm_clone to %v/%q, want "+
			"MISSING", probedDm.GetStatus(), probedDm.GetDetails())
	}
	// The source really is the thing that is down on this round — otherwise
	// the two assertions above could be passing for an unrelated reason.
	if tgt := probed.GetCloneIdToTarget()[testClone]; tgt.GetStatus() ==
		pb.ResStatus_RES_STATUS_OK {
		t.Fatalf("clone_id_to_target is OK, so the source connected after all")
	}
	for _, call := range node.Mutations() {
		t.Fatalf("the check round mutated: %q", call)
	}
}

// TestCloneMetaUnprobeableArenaProbesTheConvergePair is the other half of CN18
// step 2's refusal that a read-only pass can establish: an arena that does not
// answer at all. The converge treats that as step 2 failing — `clone_id_to_meta`
// ERROR with planArena's message and `clone_id_to_dm_clone` ERROR "metadata
// wrapper missing" (clone.go's planArena branch) — so the check round must say
// the same, even though the dm-clone below is still up and would otherwise
// probe OK. That last part is what makes this case distinct from exhaustion:
// there the dm row flipped ERROR→MISSING, here it would flip ERROR→OK.
func TestCloneMetaUnprobeableArenaProbesTheConvergePair(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	filePath := srv.nf.CnTmpFilePath(testCluster, testCn)

	// A second loop device attached to the arena file behind the agent's back
	// — CN5 wants exactly one, and nothing ever detaches the extra, which is
	// why LoopDevices documents this state as sticky.
	node.loops[filePath] = append(node.loops[filePath], "/dev/loop99")

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("unprobeable arena: %v", err)
	}
	meta := reply.GetCntlrInfo().GetCloneIdToMeta()[testClone]
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(meta.GetDetails(), "clone-metadata arena "+
			"unavailable: 2 loop devices back "+filePath+", want 1") {
		t.Fatalf("converged clone_id_to_meta is %v/%q, want the arena error",
			meta.GetStatus(), meta.GetDetails())
	}
	dm := reply.GetCntlrInfo().GetCloneIdToDmClone()[testClone]
	if dm.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		dm.GetDetails() != "metadata wrapper missing" {
		t.Fatalf("converged clone_id_to_dm_clone is %v/%q, want "+
			"ERROR/\"metadata wrapper missing\"",
			dm.GetStatus(), dm.GetDetails())
	}

	node.Reset()
	_, probed := srv.checkCntlrRound(context.Background(),
		&pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: 3,
		}, nil)
	probedMeta := probed.GetCloneIdToMeta()[testClone]
	if probedMeta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		probedMeta.GetDetails() != meta.GetDetails() {
		t.Fatalf("the check round flipped clone_id_to_meta to %v/%q, want the "+
			"converge's ERROR/%q",
			probedMeta.GetStatus(), probedMeta.GetDetails(), meta.GetDetails())
	}
	probedDm := probed.GetCloneIdToDmClone()[testClone]
	if probedDm.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		probedDm.GetDetails() != "metadata wrapper missing" {
		t.Fatalf("the check round flipped clone_id_to_dm_clone to %v/%q, want "+
			"the converge's ERROR/%q", probedDm.GetStatus(),
			probedDm.GetDetails(), "metadata wrapper missing")
	}
	// The dm-clone is still there: the ERROR above is the arena's, not a
	// device that happens to be gone.
	if _, ok := node.dms[cloneName(srv, testClone)]; !ok {
		t.Fatalf("the dm-clone is gone, so the dm row could be ERROR for an " +
			"unrelated reason")
	}
	for _, call := range node.Mutations() {
		t.Fatalf("the check round mutated: %q", call)
	}
}

// TestCloneMetaPresentWrapperInFullArenaProbesOk pins the absence check the
// refusal branch opens with. cloneMetaCanSupply asks the allocator's own
// question — is there a free run for this clone's wrapper? — and a wrapper that
// already exists holds its own units in the taken set, so for a converged clone
// in an arena with nothing free beside it the answer is "no contiguous run
// left", which is no verdict on that clone at all. probeCloneArenaCannotSupply
// therefore refuses nothing for a wrapper the enumeration found, and the check
// round reports the OK pair the converge reported. Without that short-circuit
// this fixture — a clone the converge is perfectly happy with — would probe
// ERROR/ERROR and feed err_epoch every round, which is the very flip the branch
// exists to prevent.
func TestCloneMetaPresentWrapperInFullArenaProbesOk(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	loop := loopDev(t, srv, node)

	// The clone's own wrapper holds units [0,2)...
	metaDm := cloneMetaName(srv, testClone)
	if got, want := node.dms[metaDm].table,
		wrapperTable(node, loop, 0); got != want {
		t.Fatalf("the clone's wrapper table is %q, want %q", got, want)
	}
	// ...and a kind-`b` stranger claims every unit after it, so the arena has
	// no free run left at all.
	filler := srv.nf.CnCloneMetaDmName(testCluster, testCn, testSp, 0x999)
	node.dms[filler] = &fakeDm{
		table: fmt.Sprintf("0 %d linear %s %d",
			(cnCloneMetaUnitCnt-2)*cnCloneMetaUnitSectors, node.devNo[loop],
			2*cnCloneMetaUnitSectors),
		thinIds: map[uint32]bool{},
	}
	node.devNo["/dev/mapper/"+filler] = "253:200"

	node.Reset()
	_, probed := srv.checkCntlrRound(context.Background(),
		&pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: 2,
		}, nil)
	assertOk(t, probed.GetCloneIdToMeta()[testClone], "clone_id_to_meta")
	assertOk(t, probed.GetCloneIdToDmClone()[testClone], "clone_id_to_dm_clone")
	for _, call := range node.Mutations() {
		t.Fatalf("the check round mutated: %q", call)
	}

	// The control, without which the pair above could be passing for a probe
	// that never reached the branch: hand the wrapper's own run to a second
	// stranger and lose the wrapper (and the dm-clone that mapped it) the way a
	// CN reboot does. The arena's footprint is unchanged — every unit claimed —
	// and now the refusal really is this clone's verdict.
	removeCloneDevice(node, srv)
	delete(node.dms, metaDm)
	delete(node.devNo, "/dev/mapper/"+metaDm)
	delete(node.devSize, "/dev/mapper/"+metaDm)
	squatter := srv.nf.CnCloneMetaDmName(testCluster, testCn, testSp, 0x998)
	node.dms[squatter] = &fakeDm{
		table:   wrapperTable(node, loop, 0),
		thinIds: map[uint32]bool{},
	}
	node.devNo["/dev/mapper/"+squatter] = "253:201"

	node.Reset()
	_, refused := srv.checkCntlrRound(context.Background(),
		&pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: 2,
		}, nil)
	refusedMeta := refused.GetCloneIdToMeta()[testClone]
	if refusedMeta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(refusedMeta.GetDetails(),
			"no contiguous run of 2 clone-metadata units") {
		t.Fatalf("with the wrapper gone clone_id_to_meta is %v/%q, want the "+
			"allocator's refusal — the arena was not full, so the OK pair "+
			"above proves nothing", refusedMeta.GetStatus(),
			refusedMeta.GetDetails())
	}
	refusedDm := refused.GetCloneIdToDmClone()[testClone]
	if refusedDm.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		refusedDm.GetDetails() != "metadata wrapper missing" {
		t.Fatalf("with the wrapper gone clone_id_to_dm_clone is %v/%q, want "+
			"ERROR/\"metadata wrapper missing\"",
			refusedDm.GetStatus(), refusedDm.GetDetails())
	}
}

// TestCloneMetaOrphanWrapperSwept is the CN2 reconcile bullet: a kind-`b`
// wrapper no stored cntlr's clone_list names is an orphan and its units go
// back to the arena. The sweep runs from the two passes that hold the node
// write lock, so a live clone's wrapper is never mistaken for one.
func TestCloneMetaOrphanWrapperSwept(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	loop := loopDev(t, srv, node)

	orphan := srv.nf.CnCloneMetaDmName(testCluster, testCn, testSp, 0x999)
	node.dms[orphan] = &fakeDm{
		table:   wrapperTable(node, loop, 8),
		thinIds: map[uint32]bool{},
	}
	node.devNo["/dev/mapper/"+orphan] = "253:201"

	node.Reset()
	if _, err := srv.SyncupCn(
		context.Background(), cnReq(3, true)); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	assertOrder(t, node, "cmd dmsetup ls", "cmd dmsetup remove "+orphan)
	if _, ok := node.dms[orphan]; ok {
		t.Fatalf("the orphan wrapper survived the sweep")
	}
	if _, ok := node.dms[cloneMetaName(srv, testClone)]; !ok {
		t.Fatalf("the sweep removed a live clone's wrapper")
	}
}

// TestCloneMetaWrapperMismatchRebuilds is the tmpfs-remounted-under-a-live-
// agent case: the wrapper is still there but no longer backed by the currently
// probed loop device. CN28 reports ERROR and the converge repairs it through
// the §11.5 rebuild — removing the dm-clone first, so the wrapper's own
// removal cannot fail EBUSY.
func TestCloneMetaWrapperMismatchRebuilds(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	loop := loopDev(t, srv, node)
	metaDm := cloneMetaName(srv, testClone)

	stale := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
	node.dms[metaDm].table = fmt.Sprintf("0 %d linear %s 0",
		2*cnCloneMetaUnitSectors, stale)

	info, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{
			ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr(),
		})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	meta := info.GetCntlrInfo().GetCloneIdToMeta()[testClone]
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(meta.GetDetails(), "backed by "+stale) {
		t.Fatalf("clone_id_to_meta is %v/%q",
			meta.GetStatus(), meta.GetDetails())
	}

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	assertOrder(t, node,
		"cmd dmsetup remove "+cloneName(srv, testClone),
		"cmd dmsetup remove "+metaDm,
		"cmd blkdiscard --offset 0 --length 8388608 "+loop,
		"cmd dmsetup create "+metaDm,
		"cmd dmsetup create "+cloneName(srv, testClone),
	)
	if got, want := node.dms[metaDm].table,
		wrapperTable(node, loop, 0); got != want {
		t.Fatalf("rebuilt wrapper table is %q, want %q", got, want)
	}
	assertOk(t, reply.GetCntlrInfo().GetCloneIdToMeta()[testClone],
		"clone meta")
}

// TestCloneMetaRegistrySurvivesRestart: the registry *is* the kernel's dm
// table set, so a restarted agent re-derives the used map from it and never
// double-allocates — the reason the CN allocator needs none of diskmeta.go's
// header/CRC/A-B machinery.
func TestCloneMetaRegistrySurvivesRestart(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	loop := loopDev(t, srv, node)
	metaDm := cloneMetaName(srv, testClone)
	table := node.dms[metaDm].table

	node.Reset()
	fresh := newCnServer(node)
	reconcileForTest(t, fresh)
	assertNoCall(t, node, "cmd dmsetup create "+metaDm)
	assertNoCall(t, node, "cmd blkdiscard --offset 0 --length 8388608 "+loop)
	if node.dms[metaDm].table != table {
		t.Fatalf("the wrapper was rebuilt: %q, want %q",
			node.dms[metaDm].table, table)
	}
}

// ---------------------------------------------------------------------------
// §6.10 — PushCloneBitmap (CN22)
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

// TestPushHasNoRevisionGate pins [D13] on the cn side: PushCloneBitmap carries
// no revision and the handler compares none, so a chunk the worker planned
// against a report the cntlr's stored revision has since superseded is applied
// rather than discarded. The old gate refused exactly this, and refusing it
// only ever threw away work that was about to be redone — a chunk is
// position-addressed data at a (clone_id, src_slice_idx, bm_idx) whose ids are
// never reused, and a push never advances the stored revision.
//
// The one refusal that survives is the object one: a clone the stored request
// does not name is ReplyCodeUnknownObject with a message the worker logs.
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

	// Nothing on the wire can carry a revision any more, so no later edit can
	// reintroduce the gate without changing the proto.
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

	// The object refusal is untouched, message and all.
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

// TestWrapperEnumerationSurvivesAVanishedWrapper: the kind-`b` name list comes
// from a `dmsetup ls` snapshot that is stale the instant it is printed —
// SyncupCntlr holds only the node *read* lock, so another cntlr's retire or
// SP_LEVEL_DISABLE teardown can remove a wrapper between the `ls` and the
// `dmsetup table` of that name. Failing the whole CN-wide enumeration on it
// flipped a healthy, serving clone of an unrelated cntlr to RES_STATUS_ERROR,
// and ERROR — unlike PROVISIONING — feeds err_epoch and the §10.2/§10.4
// reactions ([D14]).
func TestWrapperEnumerationSurvivesAVanishedWrapper(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})

	// Another cntlr's wrapper, listed by `ls` and gone by the time its table
	// is read.
	node.lsGhosts = append(node.lsGhosts,
		srv.nf.CnCloneMetaDmName(testCluster, testCn, testSp+1, 0x999))

	probe, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	assertOk(t, probe.GetCntlrInfo().GetCloneIdToMeta()[testClone],
		"clone meta")
	assertOk(t, probe.GetCntlrInfo().GetCloneIdToDmClone()[testClone],
		"clone dm")

	// The converge is not disturbed either — and it must not rebuild the live
	// wrapper, whose slot carries a serving dm-clone's superblock.
	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	assertOk(t, reply.GetCntlrInfo().GetCloneIdToMeta()[testClone],
		"converged clone meta")
	assertNoCall(t, node, "cmd blkdiscard")
	assertNoCall(t, node, "cmd dmsetup remove "+cloneMetaName(srv, testClone))
}

// TestWrapperEnumerationFailsOnALiveWrapper is the other half: a `dmsetup
// table` that fails for a wrapper that is still *there* is a real failure and
// must stay fatal. Skipping it would report its units as free, and the
// allocator's discard-first hole punch would then wipe a live dm-clone's
// superblock — the exact corruption the discard-first order exists to prevent.
func TestWrapperEnumerationFailsOnALiveWrapper(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})

	metaDm := cloneMetaName(srv, testClone)
	node.failCmdAlways["dmsetup table "+metaDm] = "fake: transient failure"
	probe, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	meta := probe.GetCntlrInfo().GetCloneIdToMeta()[testClone]
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(meta.GetDetails(),
			"clone-metadata arena unavailable") {
		t.Fatalf("clone_id_to_meta is %v/%q, want the arena error",
			meta.GetStatus(), meta.GetDetails())
	}
}

// TestCloneWrapperRemovalTakesTheArenaLock: removing a kind-`b` wrapper is a
// mutation of the allocator's registry — the dm table set itself — so it
// belongs inside cloneMetaMu with the enumerate → discard → create section it
// races (CN18). Without it a retire on one cntlr can delete a wrapper
// in the middle of another cntlr's allocation.
func TestCloneWrapperRemovalTakesTheArenaLock(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	metaDm := cloneMetaName(srv, testClone)

	// Stand in for the concurrent allocator: hold the leaf lock and let the
	// retire run.
	srv.cloneMetaMu.Lock()
	retired := make(chan struct{})
	var retireErr error
	go func() {
		defer close(retired)
		_, retireErr = srv.SyncupCntlr(context.Background(),
			cntlrReq(reqOpts{revision: 3, primary: true}))
	}()
	select {
	case <-retired:
		srv.cloneMetaMu.Unlock()
		t.Fatalf("the wrapper removal ran outside cloneMetaMu:\n%s",
			strings.Join(node.Calls(), "\n"))
	case <-time.After(100 * time.Millisecond):
	}
	srv.cloneMetaMu.Unlock()

	select {
	case <-retired:
	case <-time.After(10 * time.Second):
		t.Fatalf("the retire never completed after the lock was released")
	}
	if retireErr != nil {
		t.Fatalf("retire: %v", retireErr)
	}
	if _, ok := node.dms[metaDm]; ok {
		t.Fatalf("the retired clone kept its metadata wrapper")
	}
}

// TestWrapperLengthIsComparedRaw: CN28 checks "table length matches the
// computed size". Comparing a unit count rounded down
// from that length instead made the check fail *open* over a whole unit's
// worth of lengths — the one malformed-table case that reported OK.
func TestWrapperLengthIsComparedRaw(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	loop := loopDev(t, srv, node)
	metaDm := cloneMetaName(srv, testClone)

	// One sector past the budgeted two units — a hand-written reload, or a
	// partially applied table.
	node.dms[metaDm].table = fmt.Sprintf("0 %d linear %s 0",
		2*cnCloneMetaUnitSectors+1, node.devNo[loop])

	probe, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	meta := probe.GetCntlrInfo().GetCloneIdToMeta()[testClone]
	want := fmt.Sprintf("table is %d sectors, want %d",
		2*cnCloneMetaUnitSectors+1, 2*cnCloneMetaUnitSectors)
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		meta.GetDetails() != want {
		t.Fatalf("clone_id_to_meta is %v/%q, want ERROR/%q",
			meta.GetStatus(), meta.GetDetails(), want)
	}
}

// TestMisSizedWrapperClaimsItsPartialUnit: the used-unit map is a *footprint*,
// so a wrapper whose table runs one sector into the next unit still claims that
// unit. Rounding down instead handed the tail unit out again, and Alloc's
// discard-first hole punch — which is correct and mandatory for a recycled
// unit — would then zero a range the live wrapper still maps.
func TestMisSizedWrapperClaimsItsPartialUnit(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	loop := loopDev(t, srv, node)

	// Another SP's wrapper on this CN, one sector longer than a whole unit.
	// It is planted behind the agent's back, so this cntlr's converge cannot
	// repair it and has to allocate around it.
	filler := srv.nf.CnCloneMetaDmName(testCluster, testCn, testSp+1, 0x999)
	node.dms[filler] = &fakeDm{
		table: fmt.Sprintf("0 %d linear %s 0",
			cnCloneMetaUnitSectors+1, node.devNo[loop]),
		thinIds: map[uint32]bool{},
	}
	node.devNo["/dev/mapper/"+filler] = "253:200"

	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true,
		clones: []*pb.Clone{cloneOf()}})); err != nil {
		t.Fatalf("allocate beside the mis-sized wrapper: %v", err)
	}
	// Units 0 and 1 are both claimed by the filler, so the clone starts at 2.
	if got, want := node.dms[cloneMetaName(srv, testClone)].table,
		wrapperTable(node, loop, 2); got != want {
		t.Fatalf("wrapper table is %q, want %q", got, want)
	}
	punch := fmt.Sprintf("cmd blkdiscard --offset %d ", common.CnCloneMetaUnit)
	if node.hasCall(punch) {
		t.Fatalf("the hole punch hit a unit the live wrapper maps:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
}
