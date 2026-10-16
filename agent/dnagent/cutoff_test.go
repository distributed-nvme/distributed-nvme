package dnagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// When a side's request is saved (SH5, DN8)
// ---------------------------------------------------------------------------

// failoverReq is the fixture side's request after a failover: testCn1 is
// the primary, testCn0 the standby.
func failoverReq(revision uint64) *pb.SyncupSideRequest {
	return sideReq(revision, testSide, testCn1, []uint64{testCn0},
		pb.SpLevel_SP_LEVEL_READWRITE)
}

// storedSideRevision is the revision of the fixture side's request in the
// local store, read from the fake node itself.
func storedSideRevision(
	t *testing.T,
	srv *DnAgentServer,
	node *fakeNode,
) uint64 {
	t.Helper()
	path := srv.nf.LocalSidePath(testCluster, testDn, testSp, testSide)
	node.mu.Lock()
	raw, ok := node.protos[path]
	node.mu.Unlock()
	if !ok {
		t.Fatalf("no side file at %s", path)
	}
	req := &pb.SyncupSideRequest{}
	if err := proto.Unmarshal(raw, req); err != nil {
		t.Fatalf("the side file does not decode: %v", err)
	}
	return req.GetRevision()
}

// cutOffSyncupSide runs one SyncupSide whose call the worker cancels inside
// its converge. The pass is held at the first command whose line contains
// key, the call's ctx is cancelled while it waits there, and the command is
// then let go: it runs to its end, as a child outlives the cancel of its
// RunCommand (blockCmd), and every command of the pass after it is refused
// on the cancelled ctx — and so is a save placed after them, as
// LimitedOsClient.WriteProto refuses one. It returns once the call has.
func cutOffSyncupSide(
	t *testing.T,
	srv *DnAgentServer,
	node *fakeNode,
	req *pb.SyncupSideRequest,
	key string,
) {
	t.Helper()
	node.blockCmd(key)
	t.Cleanup(func() { node.releaseCmd(key) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = srv.SyncupSide(ctx, req)
	}()
	waitUntil(t, "the pass to reach "+key,
		func() bool { return node.hasCall("cmd " + key) })
	cancel()
	node.releaseCmd(key)
	<-done
	calls := node.Calls()
	for _, call := range calls[node.indexOfCall("cmd "+key)+1:] {
		if strings.HasPrefix(call, "cmd ") {
			t.Fatalf("fixture is wrong: the cut-off pass ran %q after %q",
				call, key)
		}
	}
}

// releasedOntoDev reports whether the dm device lin released the IO it
// deferred while suspended onto a table that maps devNo.
func releasedOntoDev(node *fakeNode, lin, devNo string) bool {
	node.mu.Lock()
	defer node.mu.Unlock()
	for _, table := range node.releasedOnto[lin] {
		if strings.Contains(table, " linear "+devNo+" ") {
			return true
		}
	}
	return false
}

// A side's request is saved as soon as the gates admit it: its file is the
// call's first write, before the converge's first mutating OS call (SH5,
// DN8). A save that fails is only logged: the converge runs all the same,
// and the reply carries the request's revision (DN14), as after a save that
// held.
func TestASideRequestIsSavedBeforeItsConverge(t *testing.T) {
	ctx := context.Background()
	t.Run("the save holds", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		path := srv.nf.LocalSidePath(testCluster, testDn, testSp, testSide)
		node.Reset()
		if _, err := srv.SyncupSide(ctx, failoverReq(2)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		muts := node.Mutations()
		if len(muts) < 2 || muts[0] != "writeproto "+path {
			t.Fatalf("want the side file written before the pass's first "+
				"mutation, got:\n%s", strings.Join(muts, "\n"))
		}
		if got := storedSideRevision(t, srv, node); got != 2 {
			t.Errorf("the side file holds revision %d, want 2", got)
		}
	})
	t.Run("the save fails", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		path := srv.nf.LocalSidePath(testCluster, testDn, testSp, testSide)
		oc := node.osClient()
		write := oc.WriteProtoFn
		oc.WriteProtoFn = func(
			ctx context.Context, p string, msg proto.Message,
		) error {
			if p == path {
				return errors.New("write " + p + ": no space left on device")
			}
			return write(ctx, p, msg)
		}
		srv.store = agent.NewStore(oc, common.DefaultLocalStorPrefix)
		lin0 := srv.nf.DnLinearName(testCluster, testDn, testSp, testSide,
			testCn0)
		dmErr0 := srv.nf.DnErrorName(testCluster, testDn, testSp, testSide,
			testCn0)
		node.Reset()
		reply, err := srv.SyncupSide(ctx, failoverReq(2))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if got := reply.GetRevision(); got != 2 {
			t.Errorf("the reply carries revision %d, want 2", got)
		}
		if !linearMapsDm(node, lin0, dmErr0) {
			t.Error("a failed save stopped the converge: the old primary's " +
				"dm-linear is not on its dm-error")
		}
		if got := storedSideRevision(t, srv, node); got != 1 {
			t.Errorf("the side file holds revision %d, want the 1 before "+
				"the failed save", got)
		}
	})
}

// A restart after a cutover pass the worker cut off completes the cutover.
// The old side's request was saved before its converge (SH5), so the
// restart converges the request that began the cutover, never the one before
// it. That one has no migration: its converge would resume the fenced
// dm-linears onto the side's data, move the primary's namespace back to
// optimized and sweep the migration source export away — and with the new
// side already serving through that export, the primary would write to the
// old side alone while the new side lost its source. The pass is cut off
// right after the export's port link, inside the cutover window; the
// restart finds the linears suspended and ends the window at once (DN12),
// onto their dm-errors, and keeps the export.
func TestARestartCompletesACutOffCutover(t *testing.T) {
	srv, node := newTestServer(t)
	nf := srv.nf
	syncupBoth(t, srv, 1, testSide)
	srcName := nf.DnMigrSrcName(testCluster, testDn, testSp, testMigrId)
	srcNqn := nf.MigrSrcNqn(testCluster, testDn, testSp, testMigrId)
	portLink := portPathOf(common.NvmetPortId) + "/subsystems/" + srcNqn
	sideNo := node.devNo[nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide))]
	linked := func() bool {
		node.mu.Lock()
		defer node.mu.Unlock()
		_, ok := node.links[portLink]
		return ok
	}

	srv.fenceWait = time.Hour
	node.Reset()
	cutOffSyncupSide(t, srv, node, migrSrcReq(2),
		"ln -s "+agent.NvmetRoot+"/subsystems/"+srcNqn)
	primaryLin := nf.DnLinearName(testCluster, testDn, testSp, testSide,
		testCn0)
	if !dmSuspended(node, primaryLin) || !linked() {
		t.Fatal("fixture is wrong: the cut-off pass did not leave the " +
			"window open with the export linked")
	}

	// The kernel state and the local store survive; the process does not.
	stopTestServer(t, srv)
	node.Reset()
	restarted := startTestServer(t, node)

	for _, cnId := range []uint64{testCn0, testCn1} {
		lin := nf.DnLinearName(testCluster, testDn, testSp, testSide, cnId)
		dmErr := nf.DnErrorName(testCluster, testDn, testSp, testSide, cnId)
		if dmSuspended(node, lin) || !linearMapsDm(node, lin, dmErr) {
			t.Errorf("the restart did not end the window with %s on its "+
				"dm-error", lin)
		}
		if releasedOntoDev(node, lin, sideNo) {
			t.Errorf("the restart resumed %s onto the side device", lin)
		}
		nqn := nf.SideToCnNqn(testCluster, testSp, testLeg, cnId)
		if node.hasCall(fmt.Sprintf("writedirect %s/ana_grpid=%d",
			restarted.nvmet.NsPath(nqn, sideNsid),
			common.AnaGrpIdOptimized)) {
			t.Errorf("the restart moved cn %d's namespace back to optimized",
				cnId)
		}
		if grp := anaGrpOf(t, restarted, node, nqn); grp !=
			common.AnaGrpIdInaccessible {
			t.Errorf("cn %d's namespace is in group %d, want inaccessible",
				cnId, grp)
		}
	}
	if !dmPresent(node, srcName) || !subsysPresent(node, srcNqn) ||
		!linked() {
		t.Error("the restart took down the migration source export")
	}
	if got := storedSideRevision(t, restarted, node); got != 2 {
		t.Errorf("the side file holds revision %d, want 2", got)
	}
}

// A restart after a finish pass the worker cut off completes the finish.
// The new side's request was saved before its converge (SH5), so the restart
// converges the plain side the finish brought, never the migration's request
// before it. That one would take a fresh run of the clone-metadata area,
// whose zeroed head makes the dm-clone read every region as not yet copied,
// connect to the old side again and build a dm-clone over the run: while
// the old side's export still answers, it would copy the old side's data,
// which lacks every write since the cutover, over the newer data. The pass
// is cut off after its sweep — the per-CN stacks on the side device, the
// dm-clone, the source connection and the wrapper gone, the run freed — at
// the first command of its build.
func TestARestartCompletesACutOffFinish(t *testing.T) {
	srv, node := newTestServer(t)
	nf := srv.nf
	ctx := context.Background()
	names := newMigrDstNames()
	sideDev := nf.DnSideName(testCluster, testDn, testSp, testSide)
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	provisionSide(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !servesThroughClone(t, srv, node, names) {
		t.Fatal("setup: the destination does not serve through its dm-clone")
	}

	node.Reset()
	cutOffSyncupSide(t, srv, node, sideReq(2, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE),
		"lsblk --nodeps --noheadings --output MAJ:MIN "+testDisk)
	_, held, err := srv.meta.LookupCloneMeta(ctx, testSp, testMigrId)
	if err != nil || held || dmPresent(node, names.clone) ||
		!linearMapsDm(node, names.lin, sideDev) {
		t.Fatalf("fixture is wrong: the cut-off pass did not finish its "+
			"sweep (run held %v, %v)", held, err)
	}

	stopTestServer(t, srv)
	node.Reset()
	restarted := startTestServer(t, node)

	for _, call := range []string{
		"cmd nvme connect",
		"cmd dmsetup create " + names.meta,
		"cmd dmsetup create " + names.clone,
	} {
		if node.hasCall(call) {
			t.Errorf("the restart ran %q after the finish", call)
		}
	}
	if _, held, err := restarted.meta.LookupCloneMeta(
		ctx, testSp, testMigrId); err != nil || held {
		t.Errorf("the restart holds a run of the clone-metadata area "+
			"(%v, %v)", held, err)
	}
	if !linearMapsDm(node, names.lin, sideDev) ||
		anaGrpOf(t, restarted, node, names.nqn) != common.AnaGrpIdOptimized {
		t.Error("the restart took the primary off the side device")
	}
	if got := storedSideRevision(t, restarted, node); got != 2 {
		t.Errorf("the side file holds revision %d, want 2", got)
	}
}

// A restart after a failover pass the worker cut off keeps the fence. The
// side's request was saved before its converge (SH5), so the restart
// converges the request that moved the primary, never the one before it,
// which would put the old primary's dm-linear back onto the side's data and
// leave its namespace optimized: the fence that keeps a demoted primary off
// the leg would be undone, and the IO the linear held while suspended would
// land on the side's data. The pass is cut off at the old primary's reload
// onto its dm-error, after the new primary's dm-linear went onto the side
// device.
func TestARestartKeepsACutOffFailoversFence(t *testing.T) {
	srv, node := newTestServer(t)
	nf := srv.nf
	syncupBoth(t, srv, 1, testSide)
	sideDev := nf.DnSideName(testCluster, testDn, testSp, testSide)
	sideNo := node.devNo[nf.DmPath(sideDev)]
	oldLin := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	oldErr := nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0)
	oldNqn := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0)
	newLin := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1)
	newNqn := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn1)

	node.Reset()
	cutOffSyncupSide(t, srv, node, failoverReq(2), "dmsetup reload "+oldLin)
	if !linearMapsDm(node, newLin, sideDev) {
		t.Fatal("fixture is wrong: the cut-off pass did not put the new " +
			"primary's dm-linear on the side device")
	}

	stopTestServer(t, srv)
	node.Reset()
	restarted := startTestServer(t, node)

	if dmSuspended(node, oldLin) || !linearMapsDm(node, oldLin, oldErr) {
		t.Error("the restart did not leave the old primary's dm-linear on " +
			"its dm-error")
	}
	if releasedOntoDev(node, oldLin, sideNo) {
		t.Error("the restart resumed the old primary's dm-linear onto the " +
			"side device")
	}
	if grp := anaGrpOf(t, restarted, node, oldNqn); grp ==
		common.AnaGrpIdOptimized {
		t.Error("the old primary's namespace is optimized after the restart")
	}
	if !linearMapsDm(node, newLin, sideDev) ||
		anaGrpOf(t, restarted, node, newNqn) != common.AnaGrpIdOptimized {
		t.Error("the restart took the new primary off the side device")
	}
	if got := storedSideRevision(t, restarted, node); got != 2 {
		t.Errorf("the side file holds revision %d, want 2", got)
	}
}
