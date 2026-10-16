package cnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// CN14's thin-id activation sweep
// ---------------------------------------------------------------------------

// The fixture's three dev_ids. None is a prefix of another, so a `delete N`
// count over `callsMatching` — a substring match — can never see a different
// id's message.
const (
	sweepLiveDevId  = uint32(1)
	sweepLiveDevId2 = uint32(2)
	sweepStrayDevId = uint32(7)
)

// sweepDump is a `thin_dump --metadata-snap` document listing the given device
// ids with no mappings at all: the sweep reads dev_id and nothing else, and
// zero mapped_blocks is what keeps CN25's mapped-vs-header consistency check
// (thinbm.go) happy. Scripted through the fake's thinDumps exactly as the
// CN25-CN27 bitmap tests script theirs.
func sweepDump(devIds ...uint32) string {
	var sb strings.Builder
	sb.WriteString(`<superblock uuid="" time="0" transaction="0" flags="0" ` +
		"version=\"2\" data_block_size=\"2048\" nr_data_blocks=\"0\">\n")
	for _, devId := range devIds {
		fmt.Fprintf(&sb, "  <device dev_id=\"%d\" mapped_blocks=\"0\" "+
			"transaction=\"0\" creation_time=\"0\" snap_time=\"0\">\n"+
			"  </device>\n", devId)
	}
	sb.WriteString("</superblock>\n")
	return sb.String()
}

// deleteMsg is the pool message one stray costs. Counts over it are asserted
// with callsMatching and never with indexOfCall/assertOrder: both stop at the
// first hit and so cannot tell one delete from three.
func deleteMsg(pool string, devId uint32) string {
	return fmt.Sprintf("cmd dmsetup message %s 0 delete %d", pool, devId)
}

func callCnt(node *fakeNode, fragment string) int {
	return len(node.callsMatching(fragment))
}

// sweepPending reports the in-memory arming of one slice — the flag a failed
// sweep must leave behind for the next converge (CN14).
func sweepPending(srv *CnAgentServer, sliceId uint64) bool {
	st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	if st == nil {
		return false
	}
	return st.pendingSweep[sliceId]
}

// createdTd is one already-materialized td: the sp-worker has seen its volume
// OK in every slice, so its id is in the pool metadata and a bare `dmsetup
// create` attaches it (CN14).
func createdTd(tdId uint64, devId uint32) *pb.ThinDevice {
	return &pb.ThinDevice{
		TdId: tdId, DevId: devId, Size: testTdSize, Created: true}
}

func syncupCntlrAt(
	t *testing.T,
	srv *CnAgentServer,
	o reqOpts,
) *pb.SyncupCntlrReply {
	t.Helper()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(o))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupCntlr rejected: %v", reply.GetAgentReply())
	}
	return reply
}

// TestThinDeleteSkippedWithoutPool pins CN14 and the leak the activation
// sweep heals: the per-td `delete` is gated on plan.wantPool, so the one
// fan-out that both removes the td and takes the pool away — a demote
// coalesced with a delete (RW3) — sends nothing, and afterwards nothing
// remembers the td at all. The gate itself is correct (a standby has no pool
// device, and only the primary may write pool metadata), so the sweep heals
// the leak and the gate stays.
func TestThinDeleteSkippedWithoutPool(t *testing.T) {
	srv, node := newTestServer(t)
	pool := poolName(srv)
	node.holdThinIds(pool, sweepLiveDevId)
	tds := []*pb.ThinDevice{createdTd(testTd, sweepLiveDevId)}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: tds})

	node.Reset()
	// One request that demotes this cntlr and drops the td: td_list and the
	// namespace that named it both go, which is the coalesced shape.
	syncupCntlrAt(t, srv, reqOpts{
		revision: 3,
		primary:  false,
		tds:      []*pb.ThinDevice{},
		subsys:   map[string]*pb.Subsystem{},
	})

	// Nothing was messaged, and the pool went away under the td.
	assertNoCall(t, node, "0 delete ")
	assertOrder(t, node,
		"cmd dmsetup remove "+thinName(srv, testTd),
		"cmd dmsetup remove "+pool,
	)
	for _, name := range []string{
		pool,
		names(srv).CnPoolMetaName(testCluster, testCn, testSp, testSlice),
		names(srv).CnPoolDataName(testCluster, testCn, testSp, testSlice),
		thinName(srv, testTd),
	} {
		if _, ok := node.dms[name]; ok {
			t.Fatalf("dm device %s survived the demote", name)
		}
	}
	// The dev_id is still charged against the pool on the DN legs, owned by
	// nobody — the stray the activation sweep collects at the next pool
	// creation.
	if !node.thinPools[pool][sweepLiveDevId] {
		t.Fatalf("the demote deleted the thin id after all")
	}
}

// TestActivationSweepDeletesStrays is the sweep itself: the converge that
// creates the pool device (here a standby→primary promotion) enumerates the
// pool and deletes every id no td of td_list owns — and only those.
func TestActivationSweepDeletesStrays(t *testing.T) {
	srv, node := newTestServer(t)
	pool := poolName(srv)
	// The pool metadata as the promoted cntlr finds it on the DN legs: the
	// live td's id, plus one stray a demote+delete fan-out left behind.
	node.holdThinIds(pool, sweepLiveDevId, sweepStrayDevId)
	tds := []*pb.ThinDevice{createdTd(testTd, sweepLiveDevId)}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: false, tds: tds})
	scriptDump(srv, node, sweepDump(sweepLiveDevId, sweepStrayDevId))

	node.Reset()
	reply := syncupCntlrAt(t, srv,
		reqOpts{revision: 3, primary: true, tds: tds})

	if n := callCnt(node, deleteMsg(pool, sweepStrayDevId)); n != 1 {
		t.Fatalf("%d deletes of the stray id, want exactly 1\ncalls:\n%s",
			n, strings.Join(node.Calls(), "\n"))
	}
	if n := callCnt(node, deleteMsg(pool, sweepLiveDevId)); n != 0 {
		t.Fatalf("the sweep deleted the live td's id (%d messages)", n)
	}
	// CN25's machinery, in order and released.
	assertOrder(t, node,
		"cmd dmsetup message "+pool+" 0 reserve_metadata_snap",
		"cmd thin_dump --metadata-snap ",
		"cmd dmsetup message "+pool+" 0 release_metadata_snap",
	)
	if node.dms[pool].heldRoot {
		t.Fatalf("the sweep leaked the metadata snapshot")
	}
	// The td built on top of the sweep, so the delete really did precede the
	// creates — and it kept its own id.
	assertOk(t, reply.GetCntlrInfo().GetTdIdToThinInfo()[testTd].
		GetSliceIdToDmThin()[testSlice], "thin after the sweep")
	if node.thinPools[pool][sweepStrayDevId] {
		t.Fatalf("the stray id is still in the pool metadata")
	}
	if !node.thinPools[pool][sweepLiveDevId] {
		t.Fatalf("the sweep deleted the live td's id")
	}

	// A second, identical converge probe-matches the pool device, so it arms
	// nothing and enumerates nothing: the sweep is once per pool-device
	// creation, not once per converge (CN14).
	syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true, tds: tds})
	if n := callCnt(node, "cmd thin_dump"); n != 1 {
		t.Fatalf("%d thin_dump calls over two converges, want exactly 1", n)
	}
	if sweepPending(srv, testSlice) {
		t.Fatalf("a completed sweep left the slice armed")
	}
}

// TestRestartDoesNotSweep pins CN14's restart rule: an agent restart under a
// surviving
// pool device must not sweep. No stray can have appeared while the only writer
// was down, and the CN2/SH16 invariant — a re-converge on a converged node
// issues zero mutating calls, the cn suite's case D — depends on it. Only the
// Create branch arms, and a restart takes the probe-matched one.
func TestRestartDoesNotSweep(t *testing.T) {
	srv, node := newTestServer(t)
	pool := poolName(srv)
	node.holdThinIds(pool, sweepLiveDevId)
	tds := []*pb.ThinDevice{createdTd(testTd, sweepLiveDevId)}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: tds})

	// Bait: an id no td owns, with a dump that would hand it to a sweep. It
	// must still be there afterwards, because nothing may enumerate the pool
	// at all.
	node.holdThinIds(pool, sweepStrayDevId)
	scriptDump(srv, node, sweepDump(sweepLiveDevId, sweepStrayDevId))

	node.Reset()
	fresh := newCnServer(node)
	reconcileForTest(t, fresh)
	for _, call := range node.Mutations() {
		t.Fatalf("the reconcile of a converged node mutated: %q\nall:\n%s",
			call, strings.Join(node.Mutations(), "\n"))
	}
	// The first post-boot request is revision-gated, so its reqFromRpc half
	// is satisfied — and it still sweeps nothing, because its own ensurePool
	// probe-matched the surviving device and armed nothing.
	node.Reset()
	syncupCntlrAt(t, fresh, reqOpts{revision: 3, primary: true, tds: tds})
	for _, fragment := range []string{
		"cmd thin_dump",
		"cmd dmsetup message " + pool + " 0 reserve_metadata_snap",
		"cmd dmsetup message " + pool + " 0 delete ",
	} {
		if n := callCnt(node, fragment); n != 0 {
			t.Fatalf("%d %q calls after a restart, want none", n, fragment)
		}
	}
	if !node.thinPools[pool][sweepStrayDevId] {
		t.Fatalf("a restart swept the pool")
	}
}

// TestSweepFailureRetries: the dump is the sweep's first step and the first
// thing that can fail (a killed reader's reservation, a truncated document).
// The flag survives it, the converge's own rows are untouched — the sweep is
// housekeeping, never a build step — and the next converge under an
// RPC-delivered request finishes the job.
func TestSweepFailureRetries(t *testing.T) {
	srv, node := newTestServer(t)
	pool := poolName(srv)
	node.holdThinIds(pool, sweepLiveDevId, sweepStrayDevId)
	scriptDump(srv, node, sweepDump(sweepLiveDevId, sweepStrayDevId))
	node.failCmd["cmd thin_dump"] = "thin_dump: metadata device is busy"

	tds := []*pb.ThinDevice{createdTd(testTd, sweepLiveDevId)}
	reply := syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: tds})

	if n := callCnt(node, "cmd dmsetup message "+pool+" 0 delete "); n != 0 {
		t.Fatalf("a failed dump still deleted %d ids", n)
	}
	assertOk(t, reply.GetCntlrInfo().GetSliceIdToDmPool()[testSlice],
		"pool after a failed sweep")
	assertOk(t, reply.GetCntlrInfo().GetTdIdToThinInfo()[testTd].
		GetSliceIdToDmThin()[testSlice], "thin after a failed sweep")
	if !sweepPending(srv, testSlice) {
		t.Fatalf("a failed sweep disarmed the slice")
	}

	node.Reset()
	syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true, tds: tds})
	if n := callCnt(node, deleteMsg(pool, sweepStrayDevId)); n != 1 {
		t.Fatalf("the retry sent %d deletes of the stray id, want 1", n)
	}
	if node.thinPools[pool][sweepStrayDevId] {
		t.Fatalf("the retry left the stray id in the pool metadata")
	}
	if sweepPending(srv, testSlice) {
		t.Fatalf("the completed retry left the slice armed")
	}
}

// TestSweepSurvivesDeleteFailure is why the activation sweep sends its own
// delete messages and does not go through deleteThinIdByName, CN21's L7
// delete: the sweep must see each failure, and one it did not see would
// disarm the slice with the stray still in the pool — a leak no pass aims at
// again.
func TestSweepSurvivesDeleteFailure(t *testing.T) {
	srv, node := newTestServer(t)
	pool := poolName(srv)
	node.holdThinIds(pool, sweepLiveDevId, sweepStrayDevId)
	scriptDump(srv, node, sweepDump(sweepLiveDevId, sweepStrayDevId))
	node.failCmd[fmt.Sprintf("0 delete %d", sweepStrayDevId)] =
		"device-mapper: message ioctl on " + pool + " failed: No such device"

	tds := []*pb.ThinDevice{createdTd(testTd, sweepLiveDevId)}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: tds})

	if n := callCnt(node, deleteMsg(pool, sweepStrayDevId)); n != 1 {
		t.Fatalf("%d delete attempts, want exactly 1", n)
	}
	if !node.thinPools[pool][sweepStrayDevId] {
		t.Fatalf("the failed delete removed the id anyway")
	}
	if !sweepPending(srv, testSlice) {
		t.Fatalf("a failed delete disarmed the slice")
	}

	node.Reset()
	syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true, tds: tds})
	if n := callCnt(node, deleteMsg(pool, sweepStrayDevId)); n != 1 {
		t.Fatalf("the retry sent %d deletes, want exactly 1", n)
	}
	if node.thinPools[pool][sweepStrayDevId] {
		t.Fatalf("the retry left the stray id in the pool metadata")
	}
	if sweepPending(srv, testSlice) {
		t.Fatalf("the completed retry left the slice armed")
	}
}

// TestStartupReconcileDefersSweep is CN14's startup-deferral data-loss pin.
// The startup reconcile converges from the persisted request, and a failed
// save (syncupCntlr saves before convergeCntlr but only logs a Save that
// fails, and converges all the same) lets that copy lag the pool's true
// contents. A sweep run against it deletes a live created
// td's thin id, which CN14 then reports as a permanent, never-re-messaged
// RES_STATUS_ERROR — data loss, where the same staleness without a sweep is
// merely a td left unbuilt until the next sync. So the startup Create arms
// only; the first revision-gated SyncupCntlr sweeps, with the fresh td_list,
// keeping the reboot as a heal point — the re-created pool is still swept,
// just at the first post-boot sync.
func TestStartupReconcileDefersSweep(t *testing.T) {
	srv, node := newTestServer(t)
	pool := poolName(srv)
	ctx := context.Background()

	// td B was created after the Save that the persisted copy is stuck at,
	// so the store knows only td A. S is a genuine stray. All three ids are
	// in the pool metadata on the legs, which is what lets A's and B's bare
	// `dmsetup create` attach.
	tdA := createdTd(testTd, sweepLiveDevId)
	tdB := createdTd(testSnapTd, sweepLiveDevId2)
	node.holdThinIds(pool, sweepLiveDevId, sweepLiveDevId2, sweepStrayDevId)
	scriptDump(srv, node, sweepDump(
		sweepLiveDevId, sweepLiveDevId2, sweepStrayDevId))
	if err := node.writeProto(ctx,
		srv.nf.LocalCnPath(testCluster, testCn), cnReq(2, true)); err != nil {
		t.Fatalf("seeding the cn file: %v", err)
	}
	if err := node.writeProto(ctx,
		srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr),
		cntlrReq(reqOpts{revision: 2, primary: true,
			tds: []*pb.ThinDevice{tdA}})); err != nil {
		t.Fatalf("seeding the cntlr file: %v", err)
	}

	// The reboot: an empty dm state, SH1 rebuilding everything, the pool
	// device included — so the Create branch runs and arms.
	node.Reset()
	fresh := newCnServer(node)
	reconcileForTest(t, fresh)

	if n := callCnt(node, "cmd dmsetup create "+pool); n != 1 {
		t.Fatalf("%d pool creates during the reconcile, want exactly 1", n)
	}
	for _, fragment := range []string{
		"cmd dmsetup message " + pool + " 0 reserve_metadata_snap",
		"cmd thin_dump",
		"cmd dmsetup message " + pool + " 0 delete ",
	} {
		if n := callCnt(node, fragment); n != 0 {
			t.Fatalf("the startup reconcile made %d %q calls, want none",
				n, fragment)
		}
	}
	if !sweepPending(fresh, testSlice) {
		t.Fatalf("the startup Create armed nothing")
	}

	// The first post-boot SyncupCntlr: revision-gated, so its td_list is the
	// newest desired state this cntlr has ever accepted — and it holds B.
	node.Reset()
	reply := syncupCntlrAt(t, fresh, reqOpts{revision: 3, primary: true,
		tds: []*pb.ThinDevice{tdA, tdB}})

	if n := callCnt(node, "cmd thin_dump"); n != 1 {
		t.Fatalf("%d thin_dump calls in the deferred sweep, want exactly 1", n)
	}
	if n := callCnt(node, deleteMsg(pool, sweepStrayDevId)); n != 1 {
		t.Fatalf("%d deletes of the stray id, want exactly 1", n)
	}
	// The whole point: B's id was absent from the persisted copy the reboot
	// converged from, and it is still here.
	if n := callCnt(node, deleteMsg(pool, sweepLiveDevId2)); n != 0 {
		t.Fatalf("the sweep deleted td B's id (%d messages) — the id the "+
			"stale persisted request had forgotten", n)
	}
	if n := callCnt(node, deleteMsg(pool, sweepLiveDevId)); n != 0 {
		t.Fatalf("the sweep deleted td A's id (%d messages)", n)
	}
	for _, tdId := range []uint64{testTd, testSnapTd} {
		assertOk(t, reply.GetCntlrInfo().GetTdIdToThinInfo()[tdId].
			GetSliceIdToDmThin()[testSlice],
			fmt.Sprintf("thin td %#x after the deferred sweep", tdId))
	}
	if node.thinPools[pool][sweepStrayDevId] {
		t.Fatalf("the deferred sweep left the stray id in the pool metadata")
	}
	if sweepPending(fresh, testSlice) {
		t.Fatalf("the completed sweep left the slice armed")
	}
}

// TestUnverifiedPoolRemovalKeepsArming applies the layer stop rule to a STATE
// mutation rather than to a device.
//
// L8 drops a pool's CN14 arming on the reasoning that "the pool device's life
// ended, so its arming does too". That reasoning fails in exactly the branch
// where `removeDms` reports a leftover: there the re-probe could NOT confirm
// the device is gone, so the pass reports the pool as surviving and would, in
// the same breath, forget the thin-id sweep it still owes.
//
// The cost of getting it wrong is unrecoverable rather than merely late. Once
// the device really goes and the level comes back up, ensurePool finds no
// device and re-arms — but if the device is still there, the next converge
// takes the probe-match path, which arms nothing, and the activation sweep
// never runs for that pool: every thin id deleted meanwhile can keep its
// data blocks for the life of the pool. Keeping an arming too long costs one
// idempotent sweep.
func TestUnverifiedPoolRemovalKeepsArming(t *testing.T) {
	srv, node := newTestServer(t)
	pool := poolName(srv)
	tds := []*pb.ThinDevice{createdTd(testTd, sweepLiveDevId)}

	// A stray id makes the arming mean something: it is what the sweep the
	// arming defers would delete.
	node.holdThinIds(pool, sweepLiveDevId, sweepStrayDevId)
	scriptDump(srv, node, sweepDump(sweepLiveDevId, sweepStrayDevId))
	node.failCmd["cmd thin_dump"] = "thin_dump: metadata device is busy"
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: tds})
	if !sweepPending(srv, testSlice) {
		t.Fatal("fixture is wrong: the slice was never armed")
	}

	// SP_LEVEL_DISABLE wants nothing, so L8 reaches the pool — and its
	// removal is killed, so the device is still there when the layer
	// re-probes.
	node.Reset()
	// killCmdNoEffect, not killCmdAlways: the latter models "killed but the
	// kernel completed it", which would really remove the device and make
	// the re-probe agree it is gone. The branch under test is the other one
	// — the removal did not happen and the probe still finds the pool.
	node.killCmdNoEffectAlways["dmsetup remove "+pool] = true
	// Not syncupCntlrAt: that helper fails the test on any non-zero code,
	// and a non-zero code is what this pass is supposed to return.
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, tds: tds,
		level: pb.SpLevel_SP_LEVEL_DISABLE}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}

	if got := reply.GetAgentReply().GetCode(); got != common.ReplyCodeLeftover {
		t.Errorf("code = %d, want ReplyCodeLeftover (%d): the pool did not go",
			got, common.ReplyCodeLeftover)
	}
	if _, ok := node.dms[pool]; !ok {
		t.Fatal("fixture is wrong: the pool was removed after all")
	}
	if !sweepPending(srv, testSlice) {
		t.Error("the arming was dropped although the pool removal was never " +
			"verified: the thin-id sweep is now owed by nobody")
	}
}

// sweepFailDevId is the third td's dev_id in the failed-delete fixture.
const sweepFailDevId = uint32(3)

// failedDeleteFixture converges three created tds — dev_ids 1, 2 and 3, the
// namespace on the first — over one slice, or two, whose pools hold the
// three ids, and returns the tds and the third one's id. Each dump is the
// one the fake reads off the pool's ids, so a sweep's dump lists what the
// pool holds at that moment. The pools' activation sweeps find no stray.
func failedDeleteFixture(
	t *testing.T,
	srv *CnAgentServer,
	node *fakeNode,
	twoSlices bool,
) ([]*pb.ThinDevice, uint64) {
	t.Helper()
	const thirdTd = uint64(0x5c)
	slices := []uint64{testSlice}
	if twoSlices {
		slices = append(slices, testSlice2)
	}
	for _, sliceId := range slices {
		node.holdThinIds(poolNameOf(srv, sliceId),
			sweepLiveDevId, sweepLiveDevId2, sweepFailDevId)
	}
	tds := []*pb.ThinDevice{
		createdTd(testTd, sweepLiveDevId),
		createdTd(testSnapTd, sweepLiveDevId2),
		createdTd(thirdTd, sweepFailDevId),
	}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		twoSlices: twoSlices, tds: tds})
	if n := callCnt(node, " 0 delete "); n != 0 {
		t.Fatalf("fixture: the activation sweep deleted %d ids", n)
	}
	return tds, thirdTd
}

// TestAFailedThinDeleteSweepsThePool is CN14's sweep after a failed delete. A
// td leaves td_list; L7 removes its thin volume and the delete message of its
// id fails. No later pass lists that volume again, so the same pass sweeps
// the pool's ids — under an RPC-delivered request only, as the activation
// sweep — and deletes the id there. It never deletes an id a thin volume the
// pass still lists maps, and such a volume keeps its id in its own pool's
// sweep only, when two pools are swept in one pass too. A pool sweeps once in
// the pass however many of its deletes failed, and once the read of the
// listed volumes' ids has failed no pool is dumped any more. It remembers
// nothing: a startup reconcile's failed delete is not swept, by it or by the
// next pass, and a pass with no failed delete sweeps nothing. A sweep that
// gets through ends a pending activation sweep of its slice.
func TestAFailedThinDeleteSweepsThePool(t *testing.T) {
	failDelete := func(node *fakeNode, devId uint32) {
		node.failCmd[fmt.Sprintf("0 delete %d", devId)] =
			msgIoctlFailed("pool", "Operation not permitted")
	}

	t.Run("the same pass sweeps the id", func(t *testing.T) {
		srv, node := newTestServer(t)
		pool := poolName(srv)
		tds, _ := failedDeleteFixture(t, srv, node, false)

		node.Reset()
		failDelete(node, sweepLiveDevId2)
		keep := []*pb.ThinDevice{tds[0], tds[2]}
		syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true, tds: keep})

		if n := callCnt(node, deleteMsg(pool, sweepLiveDevId2)); n != 2 {
			t.Fatalf("%d deletes of the dropped td's id, want 2: L7's "+
				"failed one and the sweep's\ncalls:\n%s", n,
				strings.Join(node.Calls(), "\n"))
		}
		assertOrder(t, node,
			"cmd dmsetup remove "+thinName(srv, testSnapTd),
			deleteMsg(pool, sweepLiveDevId2),
			"cmd dmsetup message "+pool+" 0 reserve_metadata_snap",
			"cmd thin_dump --metadata-snap ",
			"cmd dmsetup message "+pool+" 0 release_metadata_snap",
			deleteMsg(pool, sweepLiveDevId2))
		if n := callCnt(node, "cmd thin_dump"); n != 1 {
			t.Errorf("%d thin_dump calls, want 1", n)
		}
		for _, devId := range []uint32{sweepLiveDevId, sweepFailDevId} {
			if n := callCnt(node, deleteMsg(pool, devId)); n != 0 {
				t.Errorf("the sweep deleted the live id %d", devId)
			}
		}
		if node.thinPools[pool][sweepLiveDevId2] {
			t.Errorf("the dropped td's id is still in the pool metadata")
		}

		// Nothing is remembered: a re-send has no failed delete, and sweeps
		// nothing.
		node.Reset()
		syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true, tds: keep})
		if n := callCnt(node, "cmd thin_dump"); n != 0 {
			t.Errorf("the re-send ran %d thin_dump calls, want none", n)
		}
	})

	t.Run("a volume the pass still lists keeps its id", func(t *testing.T) {
		srv, node := newTestServer(t)
		pool := poolName(srv)
		tds, thirdTd := failedDeleteFixture(t, srv, node, false)

		node.Reset()
		failDelete(node, sweepLiveDevId2)
		node.failCmdAlways["dmsetup remove "+thinName(srv, thirdTd)] =
			"device-mapper: remove ioctl on " + thinName(srv, thirdTd) +
				"  failed: Device or resource busy"
		reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(
			reqOpts{revision: 3, primary: true, tds: tds[:1]}))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		if got := reply.GetAgentReply().GetCode(); got !=
			common.ReplyCodeLeftover {
			t.Fatalf("code %d, want %d: the third volume stays",
				got, common.ReplyCodeLeftover)
		}
		if n := callCnt(node, "cmd thin_dump"); n != 1 {
			t.Fatalf("%d thin_dump calls, want 1", n)
		}
		if n := callCnt(node, deleteMsg(pool, sweepFailDevId)); n != 0 {
			t.Errorf("the sweep deleted id %d from under its live volume",
				sweepFailDevId)
		}
		if !node.thinPools[pool][sweepFailDevId] {
			t.Errorf("the live volume's id left the pool metadata")
		}
		if node.thinPools[pool][sweepLiveDevId2] {
			t.Errorf("the dropped td's id is still in the pool metadata")
		}
	})

	t.Run("a volume of another pool keeps no id here", func(t *testing.T) {
		srv, node := newTestServer(t)
		pool, pool2 := poolNameOf(srv, testSlice), poolNameOf(srv, testSlice2)
		tds, _ := failedDeleteFixture(t, srv, node, true)

		node.Reset()
		node.failCmd["message "+pool+" 0 delete 2"] =
			msgIoctlFailed(pool, "Operation not permitted")
		node.failCmdAlways["dmsetup remove "+
			thinNameOf(srv, testSnapTd, testSlice2)] =
			"device-mapper: remove ioctl failed: Device or resource busy"
		if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(
			reqOpts{revision: 3, primary: true, twoSlices: true,
				tds: []*pb.ThinDevice{tds[0], tds[2]}})); err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		// The second slice's volume of the dropped td is still there, and
		// it maps id 2 in its own pool: the first pool's id 2 is a stray.
		if n := callCnt(node, deleteMsg(pool, sweepLiveDevId2)); n != 2 {
			t.Fatalf("%d deletes of id 2 in the first pool, want 2\n"+
				"calls:\n%s", n, strings.Join(node.Calls(), "\n"))
		}
		if node.thinPools[pool][sweepLiveDevId2] {
			t.Errorf("the first pool still holds the dropped td's id")
		}
		if n := callCnt(node, deleteMsg(pool2, sweepLiveDevId2)); n != 0 {
			t.Errorf("the second pool's id 2 was deleted from under its "+
				"live volume (%d messages)", n)
		}
		if !node.thinPools[pool2][sweepLiveDevId2] {
			t.Errorf("the second pool lost the live volume's id")
		}
	})

	t.Run("two swept pools keep only their own volumes' ids",
		func(t *testing.T) {
			srv, node := newTestServer(t)
			pool, pool2 := poolNameOf(srv, testSlice),
				poolNameOf(srv, testSlice2)
			tds, _ := failedDeleteFixture(t, srv, node, true)

			node.Reset()
			node.failCmd["message "+pool+" 0 delete 2"] =
				msgIoctlFailed(pool, "Operation not permitted")
			node.failCmdAlways["dmsetup remove "+
				thinNameOf(srv, testSnapTd, testSlice2)] =
				"device-mapper: remove ioctl failed: Device or resource busy"
			node.failCmd["message "+pool2+" 0 delete 3"] =
				msgIoctlFailed(pool2, "Operation not permitted")
			if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(
				reqOpts{revision: 3, primary: true, twoSlices: true,
					tds: tds[:1]})); err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if n := callCnt(node, "cmd thin_dump"); n != 2 {
				t.Fatalf("%d thin_dump calls, want 2: both pools swept\n"+
					"calls:\n%s", n, strings.Join(node.Calls(), "\n"))
			}
			// The second slice's volume of the dropped td 2 is still there
			// and maps id 2 in its own pool only: the first pool's id 2 is
			// a stray, though the second pool is swept in the same pass.
			if node.thinPools[pool][sweepLiveDevId2] {
				t.Errorf("the first pool still holds the dropped td's id 2")
			}
			if !node.thinPools[pool2][sweepLiveDevId2] {
				t.Errorf("the second pool lost the live volume's id 2")
			}
			for _, p := range []string{pool, pool2} {
				if node.thinPools[p][sweepFailDevId] {
					t.Errorf("%s still holds the dropped td's id 3", p)
				}
				if !node.thinPools[p][sweepLiveDevId] {
					t.Errorf("%s lost the kept td's id 1", p)
				}
			}
		})

	t.Run("a pool whose deletes fail twice sweeps once", func(t *testing.T) {
		srv, node := newTestServer(t)
		pool := poolName(srv)
		tds, _ := failedDeleteFixture(t, srv, node, false)

		node.Reset()
		failDelete(node, sweepLiveDevId2)
		failDelete(node, sweepFailDevId)
		syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true,
			tds: tds[:1]})
		for _, call := range []string{
			"cmd dmsetup message " + pool + " 0 reserve_metadata_snap",
			"cmd thin_dump",
		} {
			if n := callCnt(node, call); n != 1 {
				t.Errorf("%d calls of %q, want 1: the pool swept once "+
					"for each failed delete\ncalls:\n%s", n, call,
					strings.Join(node.Calls(), "\n"))
			}
		}
		for _, devId := range []uint32{sweepLiveDevId2, sweepFailDevId} {
			if node.thinPools[pool][devId] {
				t.Errorf("the dropped td's id %d is still in the pool "+
					"metadata", devId)
			}
		}
		if !node.thinPools[pool][sweepLiveDevId] {
			t.Errorf("the kept td's id left the pool metadata")
		}
	})

	t.Run("a failed read of the listed ids stops every pool's dump",
		func(t *testing.T) {
			srv, node := newTestServer(t)
			pools := []string{
				poolNameOf(srv, testSlice), poolNameOf(srv, testSlice2)}
			tds, _ := failedDeleteFixture(t, srv, node, true)

			node.Reset()
			for _, pool := range pools {
				node.failCmd["message "+pool+" 0 delete 2"] =
					msgIoctlFailed(pool, "Operation not permitted")
			}
			node.killCmdNoEffectAlways["dmsetup table "+
				thinNameOf(srv, testTd, testSlice)] = true
			if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(
				reqOpts{revision: 3, primary: true, twoSlices: true,
					tds: []*pb.ThinDevice{tds[0], tds[2]}})); err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			// The first pool's dump comes before the read that fails; the
			// second pool is not dumped after it.
			for _, call := range []string{
				" 0 reserve_metadata_snap", "cmd thin_dump",
			} {
				if n := callCnt(node, call); n != 1 {
					t.Errorf("%d calls of %q, want 1: a pool was dumped "+
						"after the read failed\ncalls:\n%s", n, call,
						strings.Join(node.Calls(), "\n"))
				}
			}
			for _, pool := range pools {
				if !node.thinPools[pool][sweepLiveDevId2] {
					t.Errorf("%s lost the dropped td's id without a sweep",
						pool)
				}
			}
		})

	t.Run("a volume whose pool is unknown keeps its id", func(t *testing.T) {
		srv, node := newTestServer(t)
		pool := poolName(srv)
		tds, _ := failedDeleteFixture(t, srv, node, false)
		// A thin volume of this sp whose table names a device the dm
		// listing does not hold, and whose removal fails: its id may be in
		// any pool.
		const strangeDevId = uint32(4)
		strange := thinNameOf(srv, 0x6c, testSlice)
		node.holdThinIds(pool, strangeDevId)
		node.mu.Lock()
		node.dms[strange] = &fakeDm{
			table: fmt.Sprintf("0 %d thin 7:99 %d",
				testTdSize/512, strangeDevId),
			thinIds: map[uint32]bool{},
		}
		node.devNo["/dev/mapper/"+strange] = node.newDevNo()
		node.mu.Unlock()

		node.Reset()
		failDelete(node, sweepLiveDevId2)
		node.failCmdAlways["dmsetup remove "+strange] =
			"device-mapper: remove ioctl failed: Device or resource busy"
		if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(
			reqOpts{revision: 3, primary: true,
				tds: []*pb.ThinDevice{tds[0], tds[2]}})); err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		if n := callCnt(node, "cmd thin_dump"); n != 1 {
			t.Fatalf("%d thin_dump calls, want 1", n)
		}
		if n := callCnt(node, deleteMsg(pool, strangeDevId)); n != 0 {
			t.Errorf("the sweep deleted id %d, which a listed volume may "+
				"map", strangeDevId)
		}
		if node.thinPools[pool][sweepLiveDevId2] {
			t.Errorf("the dropped td's id is still in the pool metadata")
		}
	})

	t.Run("a listed volume whose id cannot be read stops it",
		func(t *testing.T) {
			srv, node := newTestServer(t)
			pool := poolName(srv)
			tds, _ := failedDeleteFixture(t, srv, node, false)

			node.Reset()
			failDelete(node, sweepLiveDevId2)
			node.killCmdNoEffectAlways["dmsetup table "+
				thinName(srv, testTd)] = true
			if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(
				reqOpts{revision: 3, primary: true,
					tds: []*pb.ThinDevice{tds[0], tds[2]}})); err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			// The dump comes first, and the read of the tables after it
			// stops the sweep before any delete of its own.
			if n := callCnt(node, "cmd thin_dump"); n != 1 {
				t.Errorf("%d thin_dump calls, want 1", n)
			}
			if n := callCnt(node, deleteMsg(pool, sweepLiveDevId2)); n != 1 {
				t.Errorf("%d deletes of the dropped td's id, want L7's "+
					"failed one: a volume's id is unknown", n)
			}
			if !node.thinPools[pool][sweepLiveDevId2] {
				t.Errorf("the dropped td's id went without a sweep")
			}
		})

	t.Run("it ends a pending activation sweep", func(t *testing.T) {
		srv, node := newTestServer(t)
		tds, _ := failedDeleteFixture(t, srv, node, false)
		st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
		st.pendingSweep[testSlice] = true

		node.Reset()
		failDelete(node, sweepLiveDevId2)
		syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true,
			tds: []*pb.ThinDevice{tds[0], tds[2]}})
		if n := callCnt(node, "cmd thin_dump"); n != 1 {
			t.Errorf("%d thin_dump calls, want 1: the sweep after the "+
				"failed delete, and no activation sweep after it", n)
		}
		if sweepPending(srv, testSlice) {
			t.Errorf("the sweep left the slice armed")
		}
	})

	t.Run("a startup reconcile does not sweep", func(t *testing.T) {
		srv, node := newTestServer(t)
		pool := poolName(srv)
		ctx := context.Background()
		tds, _ := failedDeleteFixture(t, srv, node, false)
		keep := []*pb.ThinDevice{tds[0], tds[2]}
		if err := node.writeProto(ctx,
			srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr),
			cntlrReq(reqOpts{revision: 3, primary: true,
				tds: keep})); err != nil {
			t.Fatalf("seeding the cntlr file: %v", err)
		}

		node.Reset()
		failDelete(node, sweepLiveDevId2)
		fresh := newCnServer(node)
		reconcileForTest(t, fresh)
		if n := callCnt(node, deleteMsg(pool, sweepLiveDevId2)); n != 1 {
			t.Fatalf("%d deletes in the reconcile, want L7's one", n)
		}
		if n := callCnt(node, "cmd thin_dump"); n != 0 {
			t.Errorf("the reconcile ran %d thin_dump calls, want none", n)
		}

		// The first RPC pass has no failed delete of its own: the volume is
		// gone, so nothing sends the delete again.
		node.Reset()
		syncupCntlrAt(t, fresh, reqOpts{revision: 3, primary: true,
			tds: keep})
		if n := callCnt(node, "cmd thin_dump"); n != 0 {
			t.Errorf("the next pass ran %d thin_dump calls, want none", n)
		}
		if !node.thinPools[pool][sweepLiveDevId2] {
			t.Errorf("the id left the pool metadata without a sweep")
		}
	})
}

// TestASweepAfterFailedDeletesReadsTheTablesOnce counts the thin volumes'
// table reads of CN14's sweep after a failed delete. The pass reads every
// listed volume's table once, for all the pools whose delete failed, so a
// pass whose deletes failed in two pools reads no more tables than one whose
// delete failed in one; and it reads them only once a pool's dump is in, so
// a pass whose pools refuse every message, the reserve included, as a pool
// in read-only or fail mode does, reads none for the sweep. Each pass drops
// one td of three from a fresh fixture of two slices, so the rest of the
// pass reads the same tables in all three.
func TestASweepAfterFailedDeletesReadsTheTablesOnce(t *testing.T) {
	tableReads := func(refuse func(node *fakeNode, pools []string)) int {
		t.Helper()
		srv, node := newTestServer(t)
		tds, thirdTd := failedDeleteFixture(t, srv, node, true)
		pools := []string{
			poolNameOf(srv, testSlice), poolNameOf(srv, testSlice2)}
		node.Reset()
		refuse(node, pools)
		if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(
			reqOpts{revision: 3, primary: true, twoSlices: true,
				tds: []*pb.ThinDevice{tds[0], tds[2]}})); err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		reads := 0
		for _, tdId := range []uint64{testTd, testSnapTd, thirdTd} {
			for _, sliceId := range []uint64{testSlice, testSlice2} {
				reads += callCnt(node,
					"cmd dmsetup table "+thinNameOf(srv, tdId, sliceId))
			}
		}
		return reads
	}
	refuseDelete := func(node *fakeNode, pool string) {
		node.failCmd[fmt.Sprintf("message %s 0 delete %d",
			pool, sweepLiveDevId2)] =
			msgIoctlFailed(pool, "Operation not permitted")
	}

	onePool := tableReads(func(node *fakeNode, pools []string) {
		refuseDelete(node, pools[0])
	})
	twoPools := tableReads(func(node *fakeNode, pools []string) {
		refuseDelete(node, pools[0])
		refuseDelete(node, pools[1])
	})
	sick := tableReads(func(node *fakeNode, pools []string) {
		for _, pool := range pools {
			node.failCmdAlways["message "+pool+" 0 "] =
				msgIoctlFailed(pool, "Operation not supported")
		}
	})
	if twoPools != onePool {
		t.Errorf("%d table reads with two pools' deletes failed, %d with "+
			"one: the sweep read the tables once for each pool",
			twoPools, onePool)
	}
	// The sweep's one read covers the six volumes the pass's snapshot
	// listed, the two L7 removed included.
	if onePool-sick != 6 {
		t.Errorf("%d table reads with the pools refusing every message, %d "+
			"with one delete failed: want the sweep's 6 fewer", sick, onePool)
	}
}
