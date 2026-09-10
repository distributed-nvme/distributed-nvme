package cnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// CN14's thin-id activation sweep (update_05.md U3)
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
// §6.14 bitmap tests script theirs.
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
// sweep must leave behind for the next converge (update_05.md U3).
func sweepPending(srv *CnAgentServer, sliceId uint64) bool {
	st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	if st == nil {
		return false
	}
	return st.pendingSweep[sliceId]
}

// createdTd is one already-materialized td: the sp-worker has seen its volume
// OK in every slice, so its id is in the pool metadata and a bare `dmsetup
// create` attaches it (U4-S2).
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

// TestRetireSkipsThinDeleteWithoutPool is the long-missing CN14 pin, and the
// leak update_05.md U3 exists to heal: the per-td `delete` is gated on
// plan.wantPool, so the one fan-out that both removes the td and takes the
// pool away — a demote coalesced with a delete (RW3) — sends nothing, and
// afterwards no cntlr's st.applied remembers the td at all. The gate itself is
// correct (a standby has no pool device, and only the primary may write pool
// metadata), which is why the fix is the sweep and not the removal of this
// behavior.
func TestRetireSkipsThinDeleteWithoutPool(t *testing.T) {
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
	// creation, not once per converge (§0 U3 point 2).
	syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true, tds: tds})
	if n := callCnt(node, "cmd thin_dump"); n != 1 {
		t.Fatalf("%d thin_dump calls over two converges, want exactly 1", n)
	}
	if sweepPending(srv, testSlice) {
		t.Fatalf("a completed sweep left the slice armed")
	}
}

// TestRestartDoesNotSweep is §0 U3 point 3: an agent restart under a surviving
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

// TestSweepSurvivesDeleteFailure is why the sweep does not reuse deleteThinId:
// that path swallows the message error by design (CN14's fire-and-forget
// retire), and a swallowed error here would disarm the slice with the stray
// still in the pool — a leak nothing would ever look at again.
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

// TestStartupReconcileDefersSweep is §0 U3 point 4, the data-loss pin. The
// startup reconcile converges from the persisted request, and
// converge-then-persist (syncupCntlr saves after convergeCntlr and only logs a
// failed Save; a crash in the same window has the same effect) lets that copy
// lag the pool's true contents. A sweep run against it deletes a live created
// td's thin id, which U4-S2 then reports as a permanent, never-re-messaged
// RES_STATUS_ERROR — data loss, where the same staleness without a sweep is
// merely a td left unbuilt until the next sync. So the startup Create arms
// only; the first revision-gated SyncupCntlr sweeps, with the fresh td_list,
// keeping the reboot heal-point of point 2.
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
