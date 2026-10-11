package cnagent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
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
// Clone build, recovery (architecture.md, Clone crash recovery) and teardown
// (CN18)
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

// TestCloneRecovery is the rebuild of architecture.md, Clone crash recovery,
// in the step order of cnagent.md CN18: the volatile metadata wrapper is
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
	// Parking comes before the metadata snapshot is even taken
	// (the park of the recovery build, cnagent.md CN18): nothing may serve the
	// td while the bitmaps are being applied.
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
// next pass must run the recovery of architecture.md, Clone crash recovery,
// again before it enables hydration:
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
			// same wrapper (the recovery build of cnagent.md CN18, which keeps
			// a matching wrapper); one whose removal is refused is
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

// TestCloneRecoveryNeverServesAnUnfinishedDmClone holds "fully applied before
// the dm-clone handles any IO" (architecture.md, Clone crash recovery) against
// a second fault. From
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
				// read, no enable. The dm row carries the read's failure,
				// and the meta row reads the wrapper, which matches.
				assertNoCall(t, node, "cmd dmsetup remove "+clone)
				assertNoCall(t, node, "cmd dmsetup create "+clone)
				assertNoCall(t, node, "cmd thin_dump")
				assertNoCall(t, node, enableOf(clone))
				assertErrorDetails(t, info.GetCloneIdToDmClone()[testClone],
					"signal: killed", "dm-clone")
				assertOk(t, info.GetCloneIdToMeta()[testClone], "clone meta")
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
				assertOk(t, info.GetCloneIdToMeta()[testClone], "clone meta")
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
			// CN16 wants while hydration is off, and a probe issues no
			// mutating command.
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
// dm-clone row (architecture.md, Live-state reporting), then CN16's, which
// decides whether the ns-dev may
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
	// The source's disconnect runs off the pass's locks (CN10), so it is the
	// pass's one leftover, and it is set going only once the clone and its
	// metadata wrapper are gone (CN21).
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
// The clone-metadata slot allocator (CN18 step 2, [D14])
// ---------------------------------------------------------------------------

// twoTds is the fixture for the multi-clone allocator cases: a second
// destination td, so two clones can be built at once.
func twoTds() []*pb.ThinDevice {
	return []*pb.ThinDevice{
		{TdId: testTd, DevId: 1, Size: testTdSize},
		{TdId: testSnapTd, DevId: 2, Size: testTdSize},
	}
}

// wrapperTable is the kind-`cb` table one clone's slot must carry: a single
// linear over the arena's loop device at its own unit offset.
func wrapperTable(node *fakeNode, loop string, unitStart uint64) string {
	return fmt.Sprintf("0 %d linear %s %d",
		2*cnCloneMetaUnitSectors, node.devNo[loop],
		unitStart*cnCloneMetaUnitSectors)
}

// TestCloneMetaFirstFitAndRecycling pins the allocator: contiguous units,
// first fit over the free runs, and a run freed when a clone leaves the
// request handed out again — hole-punched **before** anything is created,
// because a freed unit still holds the previous clone's valid dm-clone
// superblock.
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
	// Its source's disconnect runs off the pass (CN10) and changes the node
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

// plantWrapper puts a kind-`cb` wrapper on the node behind the agent's back:
// another cntlr's clone on this CN, holding unitCnt units from unitStart on
// the arena's loop device. The arena is per CN, so another sp's wrapper fills
// it as well as this cntlr's own would — and it has to be another sp's,
// because this cntlr's own sweep removes a wrapper of testSp that no clone of
// testSp's request names. dropWrapper takes it away again, the way that
// clone's own teardown frees its run.
func plantWrapper(
	t *testing.T,
	srv *CnAgentServer,
	node *fakeNode,
	cloneId uint64,
	unitStart uint64,
	unitCnt uint64,
) string {
	t.Helper()
	loop := loopDev(t, srv, node)
	name := srv.nf.CnCloneMetaDmName(testCluster, testCn, testSp+1, cloneId)
	node.mu.Lock()
	defer node.mu.Unlock()
	node.dms[name] = &fakeDm{
		table: fmt.Sprintf("0 %d linear %s %d",
			unitCnt*cnCloneMetaUnitSectors, node.devNo[loop],
			unitStart*cnCloneMetaUnitSectors),
		thinIds: map[uint32]bool{},
	}
	node.devNo["/dev/mapper/"+name] = node.newDevNo()
	return name
}

func dropWrapper(node *fakeNode, name string) {
	node.mu.Lock()
	defer node.mu.Unlock()
	delete(node.dms, name)
	delete(node.devNo, "/dev/mapper/"+name)
	delete(node.devSize, "/dev/mapper/"+name)
}

// checkRound is one CN24 check round of the fixture cntlr at a revision, and
// the rows it probed.
func checkRound(
	t *testing.T,
	srv *CnAgentServer,
	revision uint64,
) *pb.CntlrInfo {
	t.Helper()
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
// two step-2 rows, epoch included: ResTracker.Set overwrites, so a probe that
// answered anything else would replace the converge's verdict, and the two
// channels would trade the rows, and their epochs, on every round.
func assertSameCloneRows(
	t *testing.T,
	converged *pb.CntlrInfo,
	probed *pb.CntlrInfo,
) {
	t.Helper()
	for _, row := range []struct {
		label string
		rows  func(*pb.CntlrInfo) map[uint64]*pb.ResInfo
	}{
		{"clone_id_to_meta", (*pb.CntlrInfo).GetCloneIdToMeta},
		{"clone_id_to_dm_clone", (*pb.CntlrInfo).GetCloneIdToDmClone},
	} {
		want := row.rows(converged)[testClone]
		got := row.rows(probed)[testClone]
		if !proto.Equal(got, want) {
			t.Fatalf("the check round reports %s as %v, the converge as %v",
				row.label, got, want)
		}
	}
}

// assertSameStatusAndEpoch holds the check round to the converge on all three
// of the clone's rows by status and epoch, the part of a row the worker acts
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

// cloneRowMaps are the three CntlrInfo maps a clone's rows sit in.
var cloneRowMaps = []struct {
	label string
	rows  func(*pb.CntlrInfo) map[uint64]*pb.ResInfo
}{
	{"clone_id_to_target", (*pb.CntlrInfo).GetCloneIdToTarget},
	{"clone_id_to_dm_clone", (*pb.CntlrInfo).GetCloneIdToDmClone},
	{"clone_id_to_meta", (*pb.CntlrInfo).GetCloneIdToMeta},
}

// waitingForTwoUnits is the details text of both rows of the fixture's clone
// while the arena has no free run for its two units.
const waitingForTwoUnits = "waiting for metadata room: no contiguous run " +
	"of 2 clone-metadata units in the 256-unit arena"

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

// TestCloneMetaNoRunIsProvisioning: an arena that answered and has no free
// run for a clone's metadata does not fail the clone, it makes it wait (CN18
// step 2). Both of the clone's step-2 rows read RES_STATUS_PROVISIONING with
// the allocator's own text — the converge's and the check round's alike, so
// they never trade the rows — and PROVISIONING sets no health epoch. Nothing
// of the clone is built: no wrapper, no hole punch, no dm-clone, no bitmap
// read. Its td's ns-dev is parked, with an OK row, because a dm-clone that
// does not exist shows no hydration (CN16 rule 5). The cntlr registers CN18's
// slow clone-metadata retry, never the CN10 one, and the first attempt that
// finds room builds the clone, moves the ns-dev onto it and ends the retry.
func TestCloneMetaNoRunIsProvisioning(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	srv.metaRetryInterval = time.Hour
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	loop := loopDev(t, srv, node)
	filler := plantWrapper(t, srv, node, 0x999, 0, common.CnCloneMetaUnitCnt)

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("full arena: %v", err)
	}
	info := reply.GetCntlrInfo()
	assertProvisioningDetails(t, info.GetCloneIdToMeta()[testClone],
		waitingForTwoUnits, "clone meta")
	assertProvisioningDetails(t, info.GetCloneIdToDmClone()[testClone],
		waitingForTwoUnits, "dm-clone")
	// The source connection is fine, so its row is not dragged down with it.
	assertOk(t, info.GetCloneIdToTarget()[testClone], "clone target")
	metaDm := cloneMetaName(srv, testClone)
	clone := cloneName(srv, testClone)
	for _, call := range []string{"cmd dmsetup create " + metaDm,
		"cmd dmsetup create " + clone, "cmd blkdiscard", "cmd thin_dump"} {
		assertNoCall(t, node, call)
	}
	assertParked(t, srv, node, testNs, testTd, "the waiting clone's td")
	assertOk(t, info.GetNsIdToDmLinear()[testNs], "ns-dev")
	if !metaRetrying(t, srv) {
		t.Fatalf("a clone waiting for room registered no clone-metadata retry")
	}
	if retrying(t, srv) {
		t.Fatalf("a clone waiting for room registered the CN10 retry")
	}

	// The check round reports the same pair, and finds it from this round's
	// enumeration with nothing allocated, punched or created (CN23).
	node.Reset()
	probed := checkRound(t, srv, 3)
	assertSameCloneRows(t, info, probed)
	assertOk(t, probed.GetCloneIdToTarget()[testClone], "probed clone target")
	assertOk(t, probed.GetNsIdToDmLinear()[testNs], "probed ns-dev")
	for _, call := range node.Mutations() {
		t.Fatalf("the check round mutated: %q", call)
	}

	// Room comes back when the other clone goes, which no request of this
	// cntlr says; the slow retry's next attempt builds the clone, moves the
	// ns-dev onto it and ends the retry.
	dropWrapper(node, filler)
	node.Reset()
	retryAttempt(t, srv)
	nsDev := nsDevName(srv, testNs)
	assertOrder(t, node,
		"cmd blkdiscard --offset 0 --length 8388608 "+loop,
		"cmd dmsetup create "+metaDm,
		"cmd dmsetup create "+clone,
		"cmd dmsetup message "+clone+" 0 enable_hydration",
		"cmd dmsetup reload "+nsDev)
	cloneNo := node.devNo["/dev/mapper/"+clone]
	if got, want := node.dms[nsDev].table,
		agent.LinearTable(testTdSize/512, cloneNo, 0); got != want {
		t.Fatalf("the ns-dev table is %q, want the dm-clone %q", got, want)
	}
	built := probedCntlrInfo(t, srv)
	assertOk(t, built.GetCloneIdToMeta()[testClone], "built clone meta")
	assertOk(t, built.GetCloneIdToDmClone()[testClone], "built dm-clone")
	assertOk(t, built.GetNsIdToDmLinear()[testNs], "built ns-dev")
	if metaRetrying(t, srv) {
		t.Fatalf("the clone-metadata retry outlived the build it finished")
	}
	if retrying(t, srv) {
		t.Fatalf("the build registered the CN10 retry")
	}
}

// TestCloneMetaNoRunErrorIsTyped pins the allocator's typed "no free run"
// (CN18 step 2): the converge and the probe tell an arena that waits for room
// from every other failure by the error's type, never by its text, and the
// text stays what the rows carry after their fixed prefix. A run larger than
// the whole arena can never come and is a plain error, and so is a hole punch
// that fails inside Alloc.
func TestCloneMetaNoRunErrorIsTyped(t *testing.T) {
	total := uint64(common.CnCloneMetaUnitCnt)
	full := make(map[uint64]struct{}, total)
	for i := uint64(0); i < total; i++ {
		full[i] = struct{}{}
	}
	_, err := cloneMetaAllocContiguous(full, total, 2)
	var noRun *cloneMetaNoRunError
	if !errors.As(err, &noRun) || !isCloneMetaNoRun(err) {
		t.Fatalf("a full arena answered %T %v, want the typed no-run error",
			err, err)
	}
	if got, want := err.Error(), "no contiguous run of 2 clone-metadata "+
		"units in the 256-unit arena"; got != want {
		t.Fatalf("the no-run text is %q, want %q", got, want)
	}
	if got := detailsWaitingForRoom(err); got != waitingForTwoUnits {
		t.Fatalf("the row text is %q, want %q", got, waitingForTwoUnits)
	}
	if !isCloneMetaNoRun(fmt.Errorf("wrapped: %w", err)) {
		t.Fatalf("a wrapped no-run error is not found by its type")
	}
	// Free units split into runs too short are no free run either.
	split := make(map[uint64]struct{}, total/2)
	for i := uint64(0); i < total; i += 2 {
		split[i] = struct{}{}
	}
	if _, err := cloneMetaAllocContiguous(
		split, total, 2); !isCloneMetaNoRun(err) {
		t.Fatalf("an arena of one-unit runs answered %v, want no free run",
			err)
	}
	// The whole arena fits an empty one; one unit more never fits any.
	if start, err := cloneMetaAllocContiguous(
		map[uint64]struct{}{}, total, total); err != nil || start != 0 {
		t.Fatalf("the whole arena in an empty one: start %d, err %v",
			start, err)
	}
	_, err = cloneMetaAllocContiguous(map[uint64]struct{}{}, total, total+1)
	if err == nil || isCloneMetaNoRun(err) {
		t.Fatalf("a run larger than the arena answered %T %v, want a plain "+
			"error", err, err)
	}
	if !strings.Contains(err.Error(), "larger than the 256-unit arena") {
		t.Fatalf("a run larger than the arena answered %q", err.Error())
	}

	// Alloc hands the typed error back unwrapped, before it runs anything.
	node := newFakeNode()
	cmeta := NewCloneMeta(node.osClient())
	const loop, loopNo = "/dev/loop0", "7:0"
	used := map[string]cloneMetaSlot{"stranger": {
		name: "stranger", devNo: loopNo, unitStart: 0, unitCount: total,
		sectors: total * cnCloneMetaUnitSectors,
	}}
	_, err = cmeta.Alloc(context.Background(), "wrapper", loop, loopNo,
		used, 2)
	if _, ok := err.(*cloneMetaNoRunError); !ok {
		t.Fatalf("Alloc in a full arena answered %T %v, want the typed "+
			"error unwrapped", err, err)
	}
	if calls := node.Calls(); len(calls) != 0 {
		t.Fatalf("Alloc in a full arena ran %v", calls)
	}
	// A hole punch that fails is a failure of its own, not a wait for room.
	node.failCmd["blkdiscard --offset 0 --length 8388608 "+loop] =
		"blkdiscard: " + loop + ": BLKDISCARD ioctl failed: " +
			"Operation not supported"
	_, err = cmeta.Alloc(context.Background(), "wrapper", loop, loopNo,
		map[string]cloneMetaSlot{}, 2)
	if err == nil || isCloneMetaNoRun(err) {
		t.Fatalf("a failed hole punch answered %T %v, want a plain error",
			err, err)
	}
}

// TestCloneMetaCanSupplyFreesTheClonesOwnRun pins the room check's one
// difference from the allocation it predicts: the clone's own wrapper claims
// nothing, because a wrapper that does not match is removed before its fresh
// run is allocated (ensureCloneMeta), so its run is free by the time Alloc
// looks (architecture.md, Clone crash recovery). Any other name's run, on the
// arena's loop device, stays taken; a wrapper on another loop device claims
// nothing at all. The check is pure: the arena it reads is left as it is.
func TestCloneMetaCanSupplyFreesTheClonesOwnRun(t *testing.T) {
	const loopNo, staleNo = "7:0", "7:9"
	total := uint64(common.CnCloneMetaUnitCnt)
	cp := &clonePlan{metaDmName: "own", metaUnits: 2,
		metaSectors: 2 * cnCloneMetaUnitSectors}
	slotOf := func(name, devNo string, start, cnt uint64) cloneMetaSlot {
		return cloneMetaSlot{name: name, devNo: devNo, unitStart: start,
			unitCount: cnt, sectors: cnt * cnCloneMetaUnitSectors}
	}
	arenaOf := func(slots ...cloneMetaSlot) *cloneMetaArena {
		arena := &cloneMetaArena{loopDev: "/dev/loop0", loopDevNo: loopNo,
			slots: make(map[string]cloneMetaSlot, len(slots))}
		for _, slot := range slots {
			arena.slots[slot.name] = slot
		}
		return arena
	}
	// The clone's own wrapper runs one sector into a third unit, so it does
	// not match and claims [0,3); a stranger holds every unit after it.
	own := slotOf("own", loopNo, 0, 3)
	own.sectors = 2*cnCloneMetaUnitSectors + 1
	stranger := slotOf("stranger", loopNo, 3, total-3)

	arena := arenaOf(own, stranger)
	if err := cloneMetaCanSupply(arena, cp); err != nil {
		t.Fatalf("the clone's own run is not counted as free: %v", err)
	}
	if got := arena.slots["own"]; got != own {
		t.Fatalf("the room check changed the arena: %+v", got)
	}
	if taken := cloneMetaTakenUnits(arena.slots, loopNo, "own"); len(taken) !=
		int(total-3) {
		t.Fatalf("%d units taken without the clone's own, want %d",
			len(taken), total-3)
	}
	// The same run under another name is someone else's.
	other := own
	other.name = "other"
	if err := cloneMetaCanSupply(
		arenaOf(other, stranger), cp); !isCloneMetaNoRun(err) {
		t.Fatalf("another clone's run was counted as free: %v", err)
	}
	// A wrapper on a loop device the arena no longer lives on claims
	// nothing: units [0,2) are free beside a stranger holding the rest.
	stale := slotOf("stale", staleNo, 0, 2)
	rest := slotOf("stranger", loopNo, 2, total-2)
	if err := cloneMetaCanSupply(arenaOf(stale, rest), cp); err != nil {
		t.Fatalf("a wrapper on another loop device claimed units: %v", err)
	}
}

// TestCloneRecoveryWithoutRoomParksNothing is the recovery's room check
// (architecture.md, Clone crash recovery): a clone whose wrapper must be
// replaced — here a tmpfs remounted under the live agent, so the wrapper is
// backed by a device that is no longer the arena's loop — while its old
// dm-clone still serves, in an arena with no free run for the new wrapper.
// Parking the namespace for a run that will not come would be an outage for
// nothing, so the pass parks, removes and allocates nothing: the ns-dev keeps
// serving through the old dm-clone, both clone rows read PROVISIONING, and
// the slow clone-metadata retry is registered. Once room comes back, the
// retry's attempt rebuilds the clone in the CN18 order.
func TestCloneRecoveryWithoutRoomParksNothing(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	srv.metaRetryInterval = time.Hour
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	loop := loopDev(t, srv, node)
	metaDm := cloneMetaName(srv, testClone)
	clone := cloneName(srv, testClone)
	nsDev := nsDevName(srv, testNs)
	onClone := agent.LinearTable(testTdSize/512,
		node.devNo["/dev/mapper/"+clone], 0)
	if node.dms[nsDev].table != onClone {
		t.Fatalf("the case is vacuous: the ns-dev %q does not serve "+
			"through the dm-clone", node.dms[nsDev].table)
	}
	stale := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
	node.dms[metaDm].table = fmt.Sprintf("0 %d linear %s 0",
		2*cnCloneMetaUnitSectors, stale)
	filler := plantWrapper(t, srv, node, 0x999, 0, common.CnCloneMetaUnitCnt)

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("recovery without room: %v", err)
	}
	info := reply.GetCntlrInfo()
	assertProvisioningDetails(t, info.GetCloneIdToMeta()[testClone],
		waitingForTwoUnits, "clone meta")
	// The dm-clone serves on, so its row keeps its status line after the
	// wait (CN18).
	assertNoteAndStatusLine(t, info.GetCloneIdToDmClone()[testClone],
		pb.ResStatus_RES_STATUS_PROVISIONING, waitingForTwoUnits, "dm-clone")
	for _, call := range []string{"cmd dmsetup reload " + nsDev,
		"cmd dmsetup remove " + clone, "cmd dmsetup remove " + metaDm,
		"cmd blkdiscard", "cmd thin_dump"} {
		assertNoCall(t, node, call)
	}
	if got := node.dms[nsDev].table; got != onClone {
		t.Fatalf("the ns-dev table is %q, want the serving dm-clone %q",
			got, onClone)
	}
	assertOk(t, info.GetNsIdToDmLinear()[testNs], "ns-dev")
	if !metaRetrying(t, srv) {
		t.Fatalf("a recovery waiting for room registered no " +
			"clone-metadata retry")
	}
	if retrying(t, srv) {
		t.Fatalf("a recovery waiting for room registered the CN10 retry")
	}

	node.Reset()
	probed := checkRound(t, srv, 3)
	assertSameCloneRows(t, info, probed)
	assertOk(t, probed.GetNsIdToDmLinear()[testNs], "probed ns-dev")
	for _, call := range node.Mutations() {
		t.Fatalf("the check round mutated: %q", call)
	}

	// Room comes back: the attempt runs the recovery the room check held
	// off, in the order of TestCloneMetaWrapperMismatchRebuilds.
	dropWrapper(node, filler)
	node.Reset()
	retryAttempt(t, srv)
	assertOrder(t, node,
		"cmd dmsetup reload "+nsDev,
		"cmd dmsetup remove "+clone,
		"cmd dmsetup remove "+metaDm,
		"cmd blkdiscard --offset 0 --length 8388608 "+loop,
		"cmd dmsetup create "+metaDm,
		"cmd dmsetup create "+clone,
		"cmd dmsetup message "+clone+" 0 enable_hydration")
	if got, want := node.dms[metaDm].table,
		wrapperTable(node, loop, 0); got != want {
		t.Fatalf("the rebuilt wrapper table is %q, want %q", got, want)
	}
	if got, want := node.dms[nsDev].table, agent.LinearTable(testTdSize/512,
		node.devNo["/dev/mapper/"+clone], 0); got != want {
		t.Fatalf("the ns-dev table is %q, want the new dm-clone %q",
			got, want)
	}
	built := probedCntlrInfo(t, srv)
	assertOk(t, built.GetCloneIdToMeta()[testClone], "rebuilt clone meta")
	assertOk(t, built.GetCloneIdToDmClone()[testClone], "rebuilt dm-clone")
	if metaRetrying(t, srv) || retrying(t, srv) {
		t.Fatalf("a retry outlived the rebuild")
	}
}

// TestCloneRecoveryReusesTheClonesOwnRun: a wrapper that does not match by
// its length alone holds its own run, and when that run is the only room the
// arena has left, the recovery still runs — the room check counts the
// clone's own run as free, because the wrapper is removed before its fresh
// run is allocated. Read as taken, it would leave the clone waiting for room
// for ever.
func TestCloneRecoveryReusesTheClonesOwnRun(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	srv.metaRetryInterval = time.Hour
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	loop := loopDev(t, srv, node)
	metaDm := cloneMetaName(srv, testClone)
	clone := cloneName(srv, testClone)
	nsDev := nsDevName(srv, testNs)
	// One sector into a third unit: the wrapper claims [0,3), and another
	// clone holds every unit after it.
	node.dms[metaDm].table = fmt.Sprintf("0 %d linear %s 0",
		2*cnCloneMetaUnitSectors+1, node.devNo[loop])
	plantWrapper(t, srv, node, 0x999, 3, common.CnCloneMetaUnitCnt-3)

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("rebuild over the clone's own run: %v", err)
	}
	assertOrder(t, node,
		"cmd dmsetup reload "+nsDev,
		"cmd dmsetup remove "+clone,
		"cmd dmsetup remove "+metaDm,
		"cmd blkdiscard --offset 0 --length 8388608 "+loop,
		"cmd dmsetup create "+metaDm,
		"cmd dmsetup create "+clone,
		"cmd dmsetup message "+clone+" 0 enable_hydration",
		"cmd dmsetup reload "+nsDev)
	if got, want := node.dms[metaDm].table,
		wrapperTable(node, loop, 0); got != want {
		t.Fatalf("the rebuilt wrapper table is %q, want %q", got, want)
	}
	info := reply.GetCntlrInfo()
	assertOk(t, info.GetCloneIdToMeta()[testClone], "clone meta")
	assertOk(t, info.GetCloneIdToDmClone()[testClone], "dm-clone")
	assertOk(t, info.GetNsIdToDmLinear()[testNs], "ns-dev")
	if metaRetrying(t, srv) || retrying(t, srv) {
		t.Fatalf("a rebuild that finished registered a retry")
	}
}

// TestCloneMetaRemovedWrapperFreesItsRunInThePass: the room check of a later
// clone reads the pass's cached enumeration, so that view follows what the
// pass itself did to the arena. Here the first clone's mismatched wrapper is
// removed and its fresh allocation then fails; its run is free in the kernel
// from that removal on, and the second clone, whose only room it is, is
// built there in the same pass instead of waiting for room.
func TestCloneMetaRemovedWrapperFreesItsRunInThePass(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	srv.metaRetryInterval = time.Hour
	first := cloneOf()
	second := cloneOfTd(testClone2, testSrcNqn2, testSnapTd)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: twoTds(),
		clones: []*pb.Clone{first}})
	loop := loopDev(t, srv, node)
	firstDm := cloneMetaName(srv, testClone)
	// The first clone's wrapper runs one sector into a third unit, so it
	// claims [0,3), and another clone holds every unit after it.
	node.dms[firstDm].table = fmt.Sprintf("0 %d linear %s 0",
		2*cnCloneMetaUnitSectors+1, node.devNo[loop])
	plantWrapper(t, srv, node, 0x999, 3, common.CnCloneMetaUnitCnt-3)
	node.failCmd["blkdiscard --offset 0 --length 8388608 "+loop] =
		"blkdiscard: " + loop + ": BLKDISCARD ioctl failed: " +
			"Operation not supported"

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, tds: twoTds(),
		clones: []*pb.Clone{first, second}}))
	if err != nil {
		t.Fatalf("two clones: %v", err)
	}
	info := reply.GetCntlrInfo()
	assertMissingDetails(t, info.GetCloneIdToMeta()[testClone],
		"Operation not supported", "first clone meta")
	if _, ok := node.dms[firstDm]; ok {
		t.Fatalf("the case is vacuous: the first wrapper was not removed")
	}
	assertOk(t, info.GetCloneIdToMeta()[testClone2], "second clone meta")
	assertOk(t, info.GetCloneIdToDmClone()[testClone2], "second dm-clone")
	if got, want := node.dms[cloneMetaName(srv, testClone2)].table,
		wrapperTable(node, loop, 0); got != want {
		t.Fatalf("the second wrapper table is %q, want %q", got, want)
	}
}

// TestCloneMetaRunTakenAfterThePreCheck is the window the room check cannot
// close: it reads the pass's enumeration, and the allocation re-enumerates
// under cloneMetaMu only after the park and the old dm-clone's removal, so
// another cntlr of the node can take the run in between. The pass then finds
// no free run in ensureCloneMeta and reports what the room check would have:
// the waiting pair and the slow retry, never the ERROR of a failed
// allocation — and it creates nothing and leaves the mismatched wrapper as
// it is, while the td stays parked with its row OK.
func TestCloneMetaRunTakenAfterThePreCheck(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	srv.metaRetryInterval = time.Hour
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	loopDev(t, srv, node)
	metaDm := cloneMetaName(srv, testClone)
	clone := cloneName(srv, testClone)
	nsDev := nsDevName(srv, testNs)
	stale := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
	node.dms[metaDm].table = fmt.Sprintf("0 %d linear %s 0",
		2*cnCloneMetaUnitSectors, stale)

	park := "cmd dmsetup reload " + nsDev
	gate := make(chan struct{})
	var release sync.Once
	open := func() { release.Do(func() { close(gate) }) }
	t.Cleanup(open)
	node.mu.Lock()
	node.gate[park] = gate
	node.mu.Unlock()
	node.Reset()
	done := make(chan *pb.SyncupCntlrReply, 1)
	go func() {
		reply, err := srv.SyncupCntlr(context.Background(),
			cntlrReq(reqOpts{revision: 3, primary: true,
				clones: []*pb.Clone{cloneOf()}}))
		if err != nil {
			t.Errorf("the raced pass: %v", err)
		}
		done <- reply
	}()
	// The pass found room and is parking: another cntlr's clone takes every
	// unit of the arena now.
	waitFor(t, func() bool { return node.hasCall(park) })
	plantWrapper(t, srv, node, 0x999, 0, common.CnCloneMetaUnitCnt)
	open()
	var reply *pb.SyncupCntlrReply
	select {
	case reply = <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the raced pass never completed")
	}
	info := reply.GetCntlrInfo()
	assertProvisioningDetails(t, info.GetCloneIdToMeta()[testClone],
		waitingForTwoUnits, "clone meta")
	assertProvisioningDetails(t, info.GetCloneIdToDmClone()[testClone],
		waitingForTwoUnits, "dm-clone")
	assertOrder(t, node, park, "cmd dmsetup remove "+clone)
	for _, call := range []string{"cmd dmsetup remove " + metaDm,
		"cmd blkdiscard", "cmd dmsetup create"} {
		assertNoCall(t, node, call)
	}
	assertParked(t, srv, node, testNs, testTd, "the raced pass")
	assertOk(t, info.GetNsIdToDmLinear()[testNs], "ns-dev")
	if !metaRetrying(t, srv) {
		t.Fatalf("the raced pass registered no clone-metadata retry")
	}
	if retrying(t, srv) {
		t.Fatalf("the raced pass registered the CN10 retry")
	}
	probed := checkRound(t, srv, 3)
	assertSameCloneRows(t, info, probed)
	assertOk(t, probed.GetNsIdToDmLinear()[testNs], "probed ns-dev")
}

// TestCloneMetaLaterCloneReadsTheNewestEnumeration: the room check of a later
// clone of the pass reads the newest enumeration the pass has made, not its
// first. Two clones serve through their dm-clones over wrappers that no
// longer match, as after a tmpfs remount, and the pass's first enumeration
// finds room for each. While the first clone's td is being parked, another
// cntlr of the node takes a run, and the first clone's allocation, which
// enumerates again under cloneMetaMu, sees it. Read against the pass's first
// enumeration, the second clone's room check would find a run that is gone
// and park its serving td and remove its dm-clone for a run that will not
// come (architecture.md, Clone crash recovery). Against the newest one it
// parks and removes nothing: the second clone reads the waiting pair, keeps
// serving, waits for CN18's slow retry, and the check round reports the same
// rows. The first clone waits the same way when the other cntlr took the
// only run, and is rebuilt in the run left to it when there is one.
func TestCloneMetaLaterCloneReadsTheNewestEnumeration(t *testing.T) {
	const secondNs = uint64(0x1b)
	for _, tc := range []struct {
		name string
		// fillFrom is the first unit of the run another sp holds from the
		// start, to the end of the arena; the units below it are free.
		fillFrom uint64
		// takenAt is the first unit of the two-unit run another cntlr takes
		// while the first clone's td is being parked.
		takenAt uint64
		// firstBuilt: the first clone finds a run left to it.
		firstBuilt bool
	}{
		{name: "no run for either", fillFrom: 2, takenAt: 0},
		{name: "a run for the first only", fillFrom: 4, takenAt: 2,
			firstBuilt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			srv.metaRetryInterval = time.Hour
			subsys := defaultSubsys(false)
			subsys[testNqn].NsList = append(subsys[testNqn].NsList,
				&pb.Namespace{
					NsId:     secondNs,
					NsIdx:    2,
					TdId:     testSnapTd,
					DevUuid:  "22222222-2222-4222-8222-222222222222",
					DevNguid: "22222222222242228222222222222222",
				})
			opts := reqOpts{revision: 2, primary: true, tds: twoTds(),
				subsys: subsys, clones: []*pb.Clone{cloneOf(),
					cloneOfTd(testClone2, testSrcNqn2, testSnapTd)}}
			syncupBoth(t, srv, opts)
			loop := loopDev(t, srv, node)
			firstNsDev := nsDevName(srv, testNs)
			firstClone := cloneName(srv, testClone)
			secondNsDev := nsDevName(srv, secondNs)
			secondClone := cloneName(srv, testClone2)
			onSecond := agent.LinearTable(testTdSize/512,
				node.devNo["/dev/mapper/"+secondClone], 0)
			if node.dms[secondNsDev].table != onSecond {
				t.Fatalf("the case is vacuous: the second ns-dev %q does "+
					"not serve through its dm-clone",
					node.dms[secondNsDev].table)
			}
			// Both wrappers go stale: backed by a device that is not the
			// arena's loop device, they claim no unit of it.
			stale := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
			for i, cloneId := range []uint64{testClone, testClone2} {
				node.dms[cloneMetaName(srv, cloneId)].table = fmt.Sprintf(
					"0 %d linear %s %d", 2*cnCloneMetaUnitSectors, stale,
					uint64(i)*2*cnCloneMetaUnitSectors)
			}
			plantWrapper(t, srv, node, 0x999, tc.fillFrom,
				common.CnCloneMetaUnitCnt-tc.fillFrom)

			park := "cmd dmsetup reload " + firstNsDev
			gate := make(chan struct{})
			var release sync.Once
			open := func() { release.Do(func() { close(gate) }) }
			t.Cleanup(open)
			node.mu.Lock()
			node.gate[park] = gate
			node.mu.Unlock()
			node.Reset()
			done := make(chan *pb.SyncupCntlrReply, 1)
			go func() {
				raced := opts
				raced.revision = 3
				reply, err := srv.SyncupCntlr(context.Background(),
					cntlrReq(raced))
				if err != nil {
					t.Errorf("the raced pass: %v", err)
				}
				done <- reply
			}()
			// The pass's first enumeration found room for both clones, and
			// the first is parking: another cntlr's clone takes a run now.
			waitFor(t, func() bool { return node.hasCall(park) })
			plantWrapper(t, srv, node, 0x998, tc.takenAt, 2)
			open()
			var reply *pb.SyncupCntlrReply
			select {
			case reply = <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("the raced pass never completed")
			}
			info := reply.GetCntlrInfo()

			for _, call := range []string{
				"cmd dmsetup reload " + secondNsDev,
				"cmd dmsetup remove " + secondClone,
				"cmd dmsetup remove " + cloneMetaName(srv, testClone2),
			} {
				assertNoCall(t, node, call)
			}
			if got := node.dms[secondNsDev].table; got != onSecond {
				t.Fatalf("the second ns-dev table is %q, want the serving "+
					"dm-clone %q", got, onSecond)
			}
			assertProvisioningDetails(t,
				info.GetCloneIdToMeta()[testClone2], waitingForTwoUnits,
				"second clone meta")
			assertNoteAndStatusLine(t,
				info.GetCloneIdToDmClone()[testClone2],
				pb.ResStatus_RES_STATUS_PROVISIONING, waitingForTwoUnits,
				"second dm-clone")
			assertOk(t, info.GetNsIdToDmLinear()[secondNs], "second ns-dev")
			if tc.firstBuilt {
				assertOk(t, info.GetCloneIdToMeta()[testClone],
					"first clone meta")
				assertOk(t, info.GetCloneIdToDmClone()[testClone],
					"first dm-clone")
				if got, want := node.dms[cloneMetaName(srv, testClone)].table,
					wrapperTable(node, loop, 0); got != want {
					t.Fatalf("the first wrapper table is %q, want %q",
						got, want)
				}
				if got, want := node.dms[firstNsDev].table,
					agent.LinearTable(testTdSize/512,
						node.devNo["/dev/mapper/"+firstClone], 0); got != want {
					t.Fatalf("the first ns-dev table is %q, want the new "+
						"dm-clone %q", got, want)
				}
			} else {
				assertProvisioningDetails(t,
					info.GetCloneIdToMeta()[testClone], waitingForTwoUnits,
					"first clone meta")
				assertProvisioningDetails(t,
					info.GetCloneIdToDmClone()[testClone], waitingForTwoUnits,
					"first dm-clone")
				assertOrder(t, node, park, "cmd dmsetup remove "+firstClone)
				assertParked(t, srv, node, testNs, testTd, "the first clone")
			}
			if !metaRetrying(t, srv) {
				t.Fatalf("the waiting clone registered no clone-metadata " +
					"retry")
			}
			if retrying(t, srv) {
				t.Fatalf("the raced pass registered the CN10 retry")
			}

			node.Reset()
			probed := checkRound(t, srv, 3)
			for _, row := range []struct {
				label string
				rows  func(*pb.CntlrInfo) map[uint64]*pb.ResInfo
			}{
				{"clone_id_to_meta", (*pb.CntlrInfo).GetCloneIdToMeta},
				{"clone_id_to_dm_clone", (*pb.CntlrInfo).GetCloneIdToDmClone},
			} {
				want := row.rows(info)[testClone2]
				if got := row.rows(probed)[testClone2]; !proto.Equal(got, want) {
					t.Fatalf("the check round reports the second clone's %s "+
						"as %v, the converge as %v", row.label, got, want)
				}
			}
			if tc.firstBuilt {
				assertOk(t, probed.GetCloneIdToMeta()[testClone],
					"probed first clone meta")
				assertOk(t, probed.GetCloneIdToDmClone()[testClone],
					"probed first dm-clone")
			} else {
				assertSameCloneRows(t, info, probed)
			}
			for _, call := range node.Mutations() {
				t.Fatalf("the check round mutated: %q", call)
			}
		})
	}
}

// TestCloneMetaOtherAllocFailureRetries: of the failures of CN18 step 2's
// allocation, only the arena's "no free run" waits for room. A hole punch the
// loop device refuses is a failure of the node, and the pass registers the
// CN10 retry, never the clone-metadata one (CN18). The rows are what the pass
// then reads of the node, with the command's own text in their details: no
// wrapper and no dm-clone, both MISSING, the rows the check round after it
// reads too, with the same statuses and epochs (CN18's shared reading). The
// retry is what runs the build again: the check round cannot tell the absent
// wrapper from one not built yet, so it reads both of the clone's rows
// MISSING and the parked ns-dev OK, rows that set no health epoch. The
// retry's next attempt builds the clone, moves the ns-dev onto the dm-clone
// and ends the retry.
func TestCloneMetaOtherAllocFailureRetries(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	srv.metaRetryInterval = time.Hour
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	loop := loopDev(t, srv, node)
	metaDm := cloneMetaName(srv, testClone)
	clone := cloneName(srv, testClone)
	nsDev := nsDevName(srv, testNs)
	node.failCmd["blkdiscard --offset 0 --length 8388608 "+loop] =
		"blkdiscard: " + loop + ": BLKDISCARD ioctl failed: " +
			"Operation not supported"

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("refused hole punch: %v", err)
	}
	info := reply.GetCntlrInfo()
	assertMissingDetails(t, info.GetCloneIdToMeta()[testClone],
		"Operation not supported", "clone meta")
	assertMissingDetails(t, info.GetCloneIdToDmClone()[testClone],
		"Operation not supported", "dm-clone")
	assertNoCall(t, node, "cmd dmsetup create "+metaDm)
	assertNoCall(t, node, "cmd dmsetup create "+clone)
	if !retrying(t, srv) {
		t.Fatalf("a refused hole punch registered no CN10 retry")
	}
	if metaRetrying(t, srv) {
		t.Fatalf("a refused hole punch registered the clone-metadata retry")
	}
	assertParked(t, srv, node, testNs, testTd, "the failed build's td")
	assertOk(t, info.GetNsIdToDmLinear()[testNs], "ns-dev")

	// What the retry makes up for: the check round finds nothing broken, and
	// reads what the converge read.
	node.Reset()
	probed := checkRound(t, srv, 3)
	assertMissing(t, probed.GetCloneIdToMeta()[testClone], "probed clone meta")
	assertMissing(t, probed.GetCloneIdToDmClone()[testClone],
		"probed dm-clone")
	assertOk(t, probed.GetNsIdToDmLinear()[testNs], "probed ns-dev")
	assertSameStatusAndEpoch(t, info, probed)
	for _, call := range node.Mutations() {
		t.Fatalf("the check round mutated: %q", call)
	}

	// The hole punch takes this time: the retry's attempt builds the clone,
	// moves the ns-dev onto it and ends the retry.
	node.Reset()
	retryAttempt(t, srv)
	assertOrder(t, node,
		"cmd blkdiscard --offset 0 --length 8388608 "+loop,
		"cmd dmsetup create "+metaDm,
		"cmd dmsetup create "+clone,
		"cmd dmsetup message "+clone+" 0 enable_hydration",
		"cmd dmsetup reload "+nsDev)
	if got, want := node.dms[nsDev].table, agent.LinearTable(testTdSize/512,
		node.devNo["/dev/mapper/"+clone], 0); got != want {
		t.Fatalf("the ns-dev table is %q, want the dm-clone %q", got, want)
	}
	built := probedCntlrInfo(t, srv)
	assertOk(t, built.GetCloneIdToMeta()[testClone], "built clone meta")
	assertOk(t, built.GetCloneIdToDmClone()[testClone], "built dm-clone")
	if retrying(t, srv) || metaRetrying(t, srv) {
		t.Fatalf("a retry outlived the build")
	}
}

// TestCloneMetaRunLargerThanTheArenaIsAnError: a clone whose metadata needs
// more units than the whole arena holds can never be built, however many
// clones leave the node, so it does not wait: both channels report the ERROR
// pair with the allocator's own text, nothing is allocated, and neither
// background retry is registered. A 1 PiB td at a 1 MiB block needs 257
// units of the 256-unit arena.
func TestCloneMetaRunLargerThanTheArenaIsAnError(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	srv.metaRetryInterval = time.Hour
	tds := []*pb.ThinDevice{{TdId: testTd, DevId: 1, Size: 1 << 50}}
	if units := common.CloneMetaUnits(1<<50, testBlockSize); units <=
		common.CnCloneMetaUnitCnt {
		t.Fatalf("the case is vacuous: %d units fit the arena", units)
	}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: tds})
	loop := loopDev(t, srv, node)

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, tds: tds,
		clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("oversized clone: %v", err)
	}
	info := reply.GetCntlrInfo()
	assertErrorDetails(t, info.GetCloneIdToMeta()[testClone],
		"a run of 257 clone-metadata units is larger than the 256-unit "+
			"arena", "clone meta")
	if dm := info.GetCloneIdToDmClone()[testClone]; dm.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR ||
		dm.GetDetails() != detailsCloneMetaMissing {
		t.Fatalf("dm-clone: %v/%q, want ERROR/%q", dm.GetStatus(),
			dm.GetDetails(), detailsCloneMetaMissing)
	}
	assertNoCall(t, node, "cmd blkdiscard --offset 0 --length")
	assertNoCall(t, node, "cmd dmsetup create "+cloneMetaName(srv, testClone))
	if retrying(t, srv) || metaRetrying(t, srv) {
		t.Fatalf("a clone that can never fit registered a retry")
	}
	if !node.hasCall("cmd losetup --associated") || loop == "" {
		t.Fatalf("the case is vacuous: the arena was never read")
	}

	node.Reset()
	probed := checkRound(t, srv, 3)
	assertSameCloneRows(t, info, probed)
	for _, call := range node.Mutations() {
		t.Fatalf("the check round mutated: %q", call)
	}
}

// TestNsDevParksOverAMissingDmClone is CN16 rule 5 for a dm-clone that is not
// there. Its status read fails, and a `dmsetup info` that confirms the device
// is gone makes that read "no hydration": the ns-dev parks on the td's
// dm-error and its row reads OK, in the converge and the check round alike —
// here after a recovery whose dm-clone create the node refuses. Absence is
// confirmed, never assumed: when that `dmsetup info` does not answer, the
// read stays an error, the ns-dev keeps the table it has and its row reads
// ERROR.
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
			srv.metaRetryInterval = time.Hour
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
// decides CN18 step 2 and the clone's rows (probeCloneDm): a dm-clone that
// its `dmsetup info` finds and that is gone by its status read fails that
// read, and a second `dmsetup info` that confirms it gone makes the read an
// absence. A check round then reads the dm row MISSING, as for any absent
// dm-clone, and a converge runs the recovery in the same pass: the dm-clone
// is built again, its row reads OK and no retry is registered. Read as an
// error, the round would report ERROR and the converge would end its pass
// and wait for the retry.
func TestADmCloneGoneMidReadReadsAbsent(t *testing.T) {
	// goneMidRead runs pass with the dm-clone's first status read held,
	// removes the device while that read waits, then lets it fail. The held
	// read must be the one that follows a `dmsetup info` of the dm-clone,
	// which is how probeCloneDm reads.
	goneMidRead := func(
		t *testing.T,
		srv *CnAgentServer,
		node *fakeNode,
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
		if calls := node.Calls(); first < 1 || calls[first-1] != info {
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
		goneMidRead(t, srv, node, func() {
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
		goneMidRead(t, srv, node, func() {
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
// error. A check round reads the dm row ERROR, not MISSING, and a converge
// stops CN18 step 2 there: it parks, removes and builds nothing, the ns-dev
// keeps serving through the dm-clone, the dm row reads ERROR with the failed
// read's error and the retry is registered. Read as an absence, the converge
// would park the td and rebuild a dm-clone that serves.
func TestADmCloneAbsenceTheInfoDidNotConfirmStaysAnError(t *testing.T) {
	const readErr = "Input/output error"
	// unconfirmed runs pass with the dm-clone's first status read refused
	// and its second `dmsetup info` killed. It then checks that both faults
	// fired, and that the dm-clone's reads went info, the refused status,
	// then the killed info, as probeCloneDm reads.
	unconfirmed := func(
		t *testing.T,
		srv *CnAgentServer,
		node *fakeNode,
		pass func(),
	) {
		t.Helper()
		clone := cloneName(srv, testClone)
		status := "dmsetup status " + clone
		info := "dmsetup info --columns --noheadings -o attr " + clone
		node.mu.Lock()
		node.failCmd[status] = "device-mapper: status ioctl on " + clone +
			" failed: " + readErr
		node.killCmdNth[info] = 2
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
		unconfirmed(t, srv, node, func() { probed = checkRound(t, srv, 2) })
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
		unconfirmed(t, srv, node, func() {
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
			t.Error("the pass that step 2 stopped registered no retry")
		}
	})
}

// TestCloneMetaRetryLifecycle pins CN18's clone-metadata retry from its
// registration to every way it ends. A pass whose clone waits for room
// registers it at the CnCloneMetaRetryInterval pace; the first pass that
// leaves no clone waiting ends it — the clone left clone_list, the level or a
// demotion suppresses it, or a pass built it — and so do SP_LEVEL_DISABLE
// and the cntlr's drop by SyncupCn (CN7). Ending it cancels its loop, not
// only its flag. The last case lets the loop itself run: an attempt that
// still finds no room keeps it, and the one that finds room builds the clone
// and ends it.
func TestCloneMetaRetryLifecycle(t *testing.T) {
	if got, want := newCnServer(newFakeNode()).metaRetryInterval,
		common.CnCloneMetaRetryInterval*time.Second; got != want {
		t.Fatalf("the clone-metadata retry runs every %v, want %v", got, want)
	}
	resync := func(t *testing.T, srv *CnAgentServer, o reqOpts) {
		t.Helper()
		o.revision = 4
		reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(o))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		if code := reply.GetAgentReply().GetCode(); code != 0 &&
			code != common.ReplyCodeLeftover {
			t.Fatalf("SyncupCntlr rejected: %v", reply.GetAgentReply())
		}
	}
	clones := []*pb.Clone{cloneOf()}
	for _, tc := range []struct {
		name string
		// loop lets the loop fire on its own, on a short interval.
		loop bool
		end  func(t *testing.T, srv *CnAgentServer, node *fakeNode,
			filler string)
	}{
		{
			name: "the clone leaves clone_list",
			end: func(t *testing.T, srv *CnAgentServer, _ *fakeNode,
				_ string) {
				resync(t, srv, reqOpts{primary: true})
			},
		},
		{
			name: "SP_LEVEL_NO_CLONE",
			end: func(t *testing.T, srv *CnAgentServer, _ *fakeNode,
				_ string) {
				resync(t, srv, reqOpts{primary: true, clones: clones,
					level: pb.SpLevel_SP_LEVEL_NO_CLONE})
			},
		},
		{
			name: "a demotion",
			end: func(t *testing.T, srv *CnAgentServer, _ *fakeNode,
				_ string) {
				resync(t, srv, reqOpts{clones: clones})
			},
		},
		{
			name: "SP_LEVEL_DISABLE",
			end: func(t *testing.T, srv *CnAgentServer, _ *fakeNode,
				_ string) {
				resync(t, srv, reqOpts{primary: true, clones: clones,
					level: pb.SpLevel_SP_LEVEL_DISABLE})
			},
		},
		{
			name: "SyncupCn drops the cntlr",
			end: func(t *testing.T, srv *CnAgentServer, _ *fakeNode,
				_ string) {
				// The drop's own leg and source disconnects run off the
				// pass, so the reply may name them as leftovers (CN10).
				reply, err := srv.SyncupCn(context.Background(),
					cnReq(4, false))
				if err != nil {
					t.Fatalf("SyncupCn: %v", err)
				}
				if code := reply.GetAgentReply().GetCode(); code != 0 &&
					code != common.ReplyCodeLeftover {
					t.Fatalf("SyncupCn rejected: %v", reply.GetAgentReply())
				}
				if srv.getCntlr(cntlrKey(testCluster, testCn, testSp,
					testCntlr)) != nil {
					t.Fatalf("the cntlr was not dropped")
				}
			},
		},
		{
			name: "a pass builds the clone",
			end: func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				filler string) {
				dropWrapper(node, filler)
				retryAttempt(t, srv)
				assertOk(t, probedCntlrInfo(t, srv).
					GetCloneIdToDmClone()[testClone], "built dm-clone")
			},
		},
		{
			name: "the loop's own attempt builds the clone",
			loop: true,
			end: func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				filler string) {
				st := srv.getCntlr(
					cntlrKey(testCluster, testCn, testSp, testCntlr))
				// Attempts that still find no room keep the retry.
				waitFor(t, func() bool {
					return len(node.callsMatching(
						"cmd losetup --associated")) >= 2
				})
				if !metaRetryingOf(srv, st) {
					t.Fatalf("an attempt that found no room ended the retry")
				}
				dropWrapper(node, filler)
				waitFor(t, func() bool { return !metaRetryingOf(srv, st) })
				node.mu.Lock()
				_, built := node.dms[cloneName(srv, testClone)]
				node.mu.Unlock()
				if !built {
					t.Fatalf("the retry ended without building the clone")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			srv.metaRetryInterval = time.Hour
			if tc.loop {
				srv.metaRetryInterval = 20 * time.Millisecond
			}
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			filler := plantWrapper(t, srv, node, 0x999, 0,
				common.CnCloneMetaUnitCnt)
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 3, primary: true,
					clones: clones}))
			if err != nil {
				t.Fatalf("full arena: %v", err)
			}
			assertProvisioningDetails(t,
				reply.GetCntlrInfo().GetCloneIdToMeta()[testClone],
				waitingForTwoUnits, "clone meta")
			st := srv.getCntlr(
				cntlrKey(testCluster, testCn, testSp, testCntlr))
			cancelled := make(chan struct{})
			srv.mu.Lock()
			registered := st.metaRetrying
			if cancel := st.metaCancel; cancel != nil {
				st.metaCancel = func() {
					close(cancelled)
					cancel()
				}
			}
			srv.mu.Unlock()
			if !registered {
				t.Fatalf("a clone waiting for room registered no " +
					"clone-metadata retry")
			}

			node.Reset()
			tc.end(t, srv, node, filler)
			if metaRetryingOf(srv, st) {
				t.Fatalf("the clone-metadata retry outlived %s", tc.name)
			}
			// stopCloneMetaRetry clears the registration first and cancels
			// after it, so a loop that stopped itself may cancel a moment
			// after the flag reads false.
			select {
			case <-cancelled:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s cleared the registration without cancelling "+
					"its loop", tc.name)
			}
		})
	}
}

// TestCloneMetaUnitsFromCommon: the cn agent sizes a clone's run with the one
// formula the gateway charges by (common.CloneMetaUnits; architecture.md,
// Metadata units), from the destination td and the pool's block size — never
// from the clone record's meta_units, the control plane's count.
func TestCloneMetaUnitsFromCommon(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tdSize uint64
		want   uint64
	}{
		{name: "the fixture's 64 MiB td", tdSize: testTdSize, want: 2},
		{name: "a td of one block", tdSize: testBlockSize, want: 2},
		{name: "a td smaller than one block", tdSize: testBlockSize / 2,
			want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clone := cloneOf()
			// A record that says otherwise budgets the same.
			clone.MetaUnits = 77
			plan := newCntlrPlan(common.NewNameFmt(
				common.DefaultLocalStorPrefix), cntlrReq(reqOpts{
				revision: 2, primary: true,
				tds: []*pb.ThinDevice{
					{TdId: testTd, DevId: 1, Size: tc.tdSize}},
				clones: []*pb.Clone{clone}}))
			cp := plan.cloneById[testClone]
			if cp.metaUnits != tc.want ||
				cp.metaUnits != common.CloneMetaUnits(
					tc.tdSize, testBlockSize) {
				t.Fatalf("%d units, want %d", cp.metaUnits, tc.want)
			}
			if cp.metaSectors != cp.metaUnits*cnCloneMetaUnitSectors {
				t.Fatalf("%d sectors for %d units", cp.metaSectors,
					cp.metaUnits)
			}
		})
	}
}

// TestCloneMetaAbsentWrapperWithRoomProbesMissing is the other side of the
// waiting pair, and the bound on it: a wrapper that is merely not built yet
// — here the crash-recovery case (architecture.md, Clone crash recovery), the
// volatile wrapper and the dm-clone that mapped it both lost — is a truthful
// RES_STATUS_MISSING while the arena still has room, not CN18 step 2's
// verdict. The converge agrees: its next pass allocates the slot and rebuilds
// (TestCloneRecovery), which is the opposite of waiting for room.
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
// 1 gate in front of step 2's verdict, in the direction the two tests above
// cannot reach. A converge whose source connection fails stops at CN18 step 1
// and reports `clone_id_to_dm_clone` MISSING "source not connected" with the
// meta row the wrapper's own (probeCloneMeta), which reads the enumeration
// for the wrapper's row but never asks whether the arena could supply a slot
// — however full the arena happens to be. The check round reads the same
// rows by the same function (cloneRows), so neither channel invents the
// waiting pair for a source that is not ready; one that did would flip
// *both* rows, MISSING→PROVISIONING, against the other on every round.
func TestCloneMetaExhaustedArenaWithDisconnectedSourceProbesMissing(
	t *testing.T,
) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	// Another cntlr's wrapper claiming every unit, so the arena genuinely has
	// no run left. It is another sp's (plantWrapper): one of testSp would be
	// removed by the sweep of the converge below, which names no such clone,
	// and the arena would have room again before either channel asks.
	filler := plantWrapper(t, srv, node, 0x999, 0, common.CnCloneMetaUnitCnt)
	// ...and the source refuses the connect, so step 1 never completes.
	node.failCmdAlways["nvme connect"] = "nvme connect: Connection refused"

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("disconnected source: %v", err)
	}
	if _, ok := node.dms[filler]; !ok {
		t.Fatalf("the case is vacuous: the converge removed the filler, " +
			"so the arena has room")
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

// TestCloneMetaUnprobeableArenaProbesTheConvergePair is the other verdict of
// CN18 step 2 that a read-only pass can establish: an arena that does not
// answer at all. The converge treats that as step 2 failing — `clone_id_to_meta`
// ERROR with planArena's message and `clone_id_to_dm_clone` ERROR "metadata
// wrapper missing" (clone.go's planArena branch) — and registers the CN10
// retry, never the clone-metadata one: an arena that did not answer is not
// one with no room. The check round must say the same, even though the
// dm-clone below is still up and would otherwise probe OK. That last part is
// what makes this case distinct from a full arena: there the dm row would
// flip PROVISIONING→MISSING, here it would flip ERROR→OK.
func TestCloneMetaUnprobeableArenaProbesTheConvergePair(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	srv.metaRetryInterval = time.Hour
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
	// The dm-clone is still up, so its status line follows the verdict's
	// text (CN18).
	converged := reply.GetCntlrInfo().GetCloneIdToDmClone()[testClone]
	assertNoteAndStatusLine(t, converged, pb.ResStatus_RES_STATUS_ERROR,
		detailsCloneMetaMissing, "converged clone_id_to_dm_clone")
	if !retrying(t, srv) {
		t.Fatalf("an arena that did not answer registered no CN10 retry")
	}
	if metaRetrying(t, srv) {
		t.Fatalf("an arena that did not answer registered the " +
			"clone-metadata retry")
	}

	node.Reset()
	_, probed := srv.checkCntlrRound(context.Background(),
		&pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: 3,
		}, nil)
	assertSameCloneRows(t, reply.GetCntlrInfo(), probed)
	probedMeta := probed.GetCloneIdToMeta()[testClone]
	if probedMeta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		probedMeta.GetDetails() != meta.GetDetails() {
		t.Fatalf("the check round flipped clone_id_to_meta to %v/%q, want the "+
			"converge's ERROR/%q",
			probedMeta.GetStatus(), probedMeta.GetDetails(), meta.GetDetails())
	}
	assertNoteAndStatusLine(t, probed.GetCloneIdToDmClone()[testClone],
		pb.ResStatus_RES_STATUS_ERROR, detailsCloneMetaMissing,
		"probed clone_id_to_dm_clone")
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

// TestCloneMetaPresentWrapperInFullArenaProbesOk: a clone whose wrapper
// matches reads OK on both rows however full the arena is beside it — the
// converge reuses such a wrapper as it is, and cloneMetaVerdict decides
// nothing for it — so the check round reports the OK pair the converge
// reported. The control is the same footprint with the wrapper lost and its
// run taken by another clone: then the verdict is this clone's, and both rows
// read PROVISIONING waiting for room.
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
	// ...and a kind-`cb` stranger claims every unit after it, so the arena has
	// no free run left at all.
	filler := srv.nf.CnCloneMetaDmName(testCluster, testCn, testSp, 0x999)
	node.dms[filler] = &fakeDm{
		table: fmt.Sprintf("0 %d linear %s %d",
			(common.CnCloneMetaUnitCnt-2)*cnCloneMetaUnitSectors, node.devNo[loop],
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

	// The control, without which the pair above could be passing for an
	// arena that was not full: hand the wrapper's own run to a second
	// stranger and lose the wrapper (and the dm-clone that mapped it) the way a
	// CN reboot does. The arena's footprint is unchanged — every unit claimed —
	// and now the waiting pair really is this clone's verdict.
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
	waiting := checkRound(t, srv, 2)
	assertProvisioningDetails(t, waiting.GetCloneIdToMeta()[testClone],
		waitingForTwoUnits, "clone_id_to_meta with the wrapper gone")
	assertProvisioningDetails(t, waiting.GetCloneIdToDmClone()[testClone],
		waitingForTwoUnits, "clone_id_to_dm_clone with the wrapper gone")
}

// TestCloneMetaOrphanWrapperSwept is the CN2 reconcile bullet: a kind-`cb`
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
// the rebuild of architecture.md, Clone crash recovery — removing the dm-clone
// first (cnagent.md CN18), so the wrapper's own
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
// PushCloneBitmap (CN22)
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

// TestWrapperEnumerationSurvivesAVanishedWrapper: the kind-`cb` name list comes
// from a `dmsetup ls` snapshot that is stale the instant it is printed —
// SyncupCntlr holds only the node *read* lock, so another cntlr's sweep
// (CN21), at SP_LEVEL_DISABLE or otherwise, can remove a wrapper between the
// `ls` and the `dmsetup table` of that name. Failing the whole CN-wide
// enumeration on it flipped a healthy, serving clone of an unrelated cntlr to
// RES_STATUS_ERROR, and ERROR — unlike PROVISIONING — feeds err_epoch
// (dnv-worker.md HL2) and the reactions of architecture.md, Automatic
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

// TestCloneWrapperRemovalTakesTheArenaLock: removing a kind-`cb` wrapper is a
// mutation of the allocator's registry — the dm table set itself — so it
// belongs inside cloneMetaMu with the enumerate → discard → create section it
// races (CN18). Without it a sweep on one cntlr can delete a wrapper
// in the middle of another cntlr's allocation.
func TestCloneWrapperRemovalTakesTheArenaLock(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	metaDm := cloneMetaName(srv, testClone)

	// Stand in for the concurrent allocator: hold the leaf lock and let the
	// sweep run.
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
		t.Fatalf("the sweep never completed after the lock was released")
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

// ---------------------------------------------------------------------------
// CN18's shared reading of a clone's rows, for the converge and the check
// round (CN28)
// ---------------------------------------------------------------------------

// TestCloneRowsAgreeBetweenConvergeAndCheck holds the two channels to one
// reading of a clone (cloneRows). For each state of the node, a converge and
// the check round after it report the clone's three rows with the same
// status and the same epoch, and the very same rows where no failure of the
// converge rides in them. The states are the ones the channels read apart
// when each decided the rows by its own rule: a source without its
// namespace, a source path that is not live, a walk that did not answer, a
// dm-clone up with hydration off, and a clone that waits for room while its
// dm-clone serves. A fault of the source — no namespace, or an endpoint
// whose controller is not live or is gone — reads MISSING on both channels
// beside a dm-clone with hydration on that serves on, so no row of the clone
// reads ERROR, and none sets a health epoch (CN18). A dm-clone that is up
// keeps its raw status line in its row whatever the row's status, which is
// what the gateway's hydration checks read. What serves a namespace does not
// follow the rows: a hydrating dm-clone serves on, one with hydration off
// stays parked (nsDevNow).
func TestCloneRowsAgreeBetweenConvergeAndCheck(t *testing.T) {
	const (
		ok      = pb.ResStatus_RES_STATUS_OK
		missing = pb.ResStatus_RES_STATUS_MISSING
		errored = pb.ResStatus_RES_STATUS_ERROR
		waiting = pb.ResStatus_RES_STATUS_PROVISIONING
	)
	noNs := fmt.Sprintf("no namespace 1 on %s", testSrcNqn)
	for _, tc := range []struct {
		name string
		// killed starts from the node a build killed before CN18 step 5
		// leaves (seedKilledCloneBuild); otherwise the clone is built and
		// serving.
		killed  bool
		arrange func(t *testing.T, srv *CnAgentServer, node *fakeNode)
		// target, dm and meta are the rows' statuses, on both channels.
		target, dm, meta pb.ResStatus
		// targetHas and dmHas are texts the converge's rows carry.
		targetHas, dmHas string
		// same: the check round's rows are the converge's, details and all.
		same bool
		// serving: the namespace is served through the dm-clone, and parked
		// on its td's dm-error otherwise.
		serving bool
		// noDm: no dm-clone is up after the converge, so no row carries a
		// status line.
		noDm bool
	}{
		{
			name:    "all up",
			arrange: func(*testing.T, *CnAgentServer, *fakeNode) {},
			target:  ok, dm: ok, meta: ok, targetHas: "paths: live",
			same: true, serving: true,
		},
		{
			name: "the source has no namespace",
			arrange: func(_ *testing.T, _ *CnAgentServer, node *fakeNode) {
				node.dropNsHead(testSrcNqn)
			},
			target: missing, dm: ok, meta: ok, targetHas: noNs,
			same: true, serving: true,
		},
		{
			name: "a source path is not live",
			arrange: func(_ *testing.T, _ *CnAgentServer, node *fakeNode) {
				node.setCtrlState(testSrcNqn, testIp2, testSvcId2,
					"connecting")
			},
			target: missing, dm: ok, meta: ok,
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
			target: missing, dm: ok, meta: ok,
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
			target: errored, dm: ok, meta: ok,
			targetHas: errSubsysUnknown.Error(), same: true, serving: true,
		},
		{
			name: "a sweep's disconnect of the source in flight",
			arrange: func(_ *testing.T, srv *CnAgentServer, _ *fakeNode) {
				srv.mu.Lock()
				defer srv.mu.Unlock()
				srv.disconnecting[testSrcNqn] = struct{}{}
			},
			target: errored, dm: ok, meta: ok,
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
			target: ok, dm: ok, meta: ok, targetHas: "paths: live",
			same: true, serving: true,
		},
		{
			name:   "hydration off, the enable refused",
			killed: true,
			arrange: func(_ *testing.T, srv *CnAgentServer, node *fakeNode) {
				clone := cloneName(srv, testClone)
				node.failCmdAlways["dmsetup message "+clone+
					" 0 enable_hydration"] = "device-mapper: message " +
					"ioctl on " + clone + " failed: Invalid argument"
			},
			target: ok, dm: errored, meta: ok, targetHas: "paths: live",
			dmHas: "Invalid argument",
		},
		{
			// The recovery removes the unfinished dm-clone and its
			// re-create is refused: the rows read the device gone.
			name:   "hydration off, the re-create refused",
			killed: true,
			arrange: func(_ *testing.T, srv *CnAgentServer, node *fakeNode) {
				clone := cloneName(srv, testClone)
				node.failCmdAlways["dmsetup create "+clone+" "] =
					"device-mapper: reload ioctl on " + clone +
						" failed: Invalid argument"
			},
			target: ok, dm: missing, meta: ok, targetHas: "paths: live",
			dmHas: "Invalid argument", noDm: true,
		},
		{
			// The wrapper and the dm-clone are lost, and the wrapper's
			// create is killed after it made the wrapper: the rows read the
			// arena afresh and find it.
			name: "the wrapper's create killed after it made the wrapper",
			arrange: func(_ *testing.T, srv *CnAgentServer, node *fakeNode) {
				metaDm := cloneMetaName(srv, testClone)
				removeCloneDevice(node, srv)
				dropWrapper(node, metaDm)
				node.killCmd["dmsetup create "+metaDm+" "] = true
			},
			target: ok, dm: missing, meta: ok, targetHas: "paths: live",
			dmHas: "signal: killed", noDm: true,
		},
		{
			name:   "hydration off, the source has no namespace",
			killed: true,
			arrange: func(_ *testing.T, _ *CnAgentServer, node *fakeNode) {
				node.dropNsHead(testSrcNqn)
			},
			target: missing, dm: errored, meta: ok, targetHas: noNs,
			dmHas: detailsCloneNotHydrating, same: true,
		},
		{
			name: "waiting for room, the dm-clone serving",
			arrange: func(t *testing.T, srv *CnAgentServer, node *fakeNode) {
				stale := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
				node.dms[cloneMetaName(srv, testClone)].table = fmt.Sprintf(
					"0 %d linear %s 0", 2*cnCloneMetaUnitSectors, stale)
				plantWrapper(t, srv, node, 0x999, 0,
					common.CnCloneMetaUnitCnt)
			},
			target: ok, dm: waiting, meta: waiting, targetHas: "paths: live",
			dmHas: waitingForTwoUnits, same: true, serving: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			srv.metaRetryInterval = time.Hour
			if tc.killed {
				seedKilledCloneBuild(t, srv, node)
			} else {
				syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
					clones: []*pb.Clone{cloneOf()}})
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
				{"clone_id_to_meta", (*pb.CntlrInfo).GetCloneIdToMeta,
					tc.meta, ""},
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
				if _, parsed := common.ParseCloneStatus(details); parsed ==
					tc.noDm {
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
// only says why a dm-clone that is not there is not, and the raw status line
// rides along whenever the read took it.
func TestCloneDmRowRule(t *testing.T) {
	cp := testClonePlan(t)
	ready := cloneSource{view: &subsysView{found: true, nsDev: "/dev/nvme5n1",
		ctrls: []ctrlView{{name: "nvme5", trAddr: testIp2,
			trSvcId: testSvcId2, state: "live"}}}}
	unknown := fmt.Errorf("subsystem %s %w: killed", testSrcNqn,
		errSubsysUnknown)
	const raw = "0 131072 clone 8 1/1024 2048 0/64 0 1 no_hydration 0"
	killed := errors.New("signal: killed")
	for _, tc := range []struct {
		name    string
		src     cloneSource
		dm      cloneDmRead
		status  pb.ResStatus
		details string
	}{
		{"the read did not answer", ready, cloneDmRead{err: killed},
			pb.ResStatus_RES_STATUS_ERROR, killed.Error()},
		{"absent beside a ready source", ready, cloneDmRead{},
			pb.ResStatus_RES_STATUS_MISSING, ""},
		{"absent beside an unconnected source",
			cloneSource{view: &subsysView{}}, cloneDmRead{},
			pb.ResStatus_RES_STATUS_MISSING, "source not connected"},
		{"absent beside a disconnect in flight",
			cloneSource{inFlight: true}, cloneDmRead{},
			pb.ResStatus_RES_STATUS_MISSING, "source not connected"},
		{"absent beside a walk unanswered", cloneSource{readErr: unknown},
			cloneDmRead{}, pb.ResStatus_RES_STATUS_MISSING,
			"source unknown: " + unknown.Error()},
		{"absent beside an endpoint with no controller",
			cloneSource{view: &subsysView{found: true,
				nsDev: "/dev/nvme5n1"}},
			cloneDmRead{}, pb.ResStatus_RES_STATUS_MISSING,
			"source not connected"},
		{"hydrating", ready,
			cloneDmRead{present: true, raw: raw, hydrating: true},
			pb.ResStatus_RES_STATUS_OK, raw},
		{"hydrating beside an unconnected source",
			cloneSource{view: &subsysView{}},
			cloneDmRead{present: true, raw: raw, hydrating: true},
			pb.ResStatus_RES_STATUS_OK, raw},
		{"hydration off", ready, cloneDmRead{present: true, raw: raw},
			pb.ResStatus_RES_STATUS_ERROR,
			detailsCloneNotHydrating + "\n" + raw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, details := cloneDmRow(cp, tc.src, tc.dm)
			if status != tc.status || details != tc.details {
				t.Fatalf("%v/%q, want %v/%q", status, details, tc.status,
					tc.details)
			}
		})
	}
}

// TestCloneSourceReadyRule pins CN18 step 1's test on one read of the source
// (cloneSource.ready), which gates step 2's verdict on both channels: every
// configured endpoint has a controller, in whatever state, and the source
// namespace is there.
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
