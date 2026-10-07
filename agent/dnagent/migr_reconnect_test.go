package dnagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A source subsystem with no controller (DN13 step (3))
// ---------------------------------------------------------------------------

// serveThenDropSourceCtrls brings the destination to serve through its
// dm-clone, then deletes the source's controllers while the dm-clone holds
// their multipath head (dropCtrlsWhileHeld, with listed) and checks that
// every walk of the host reads what the kernel then shows: the subsystem's
// directory with no namespace node and no controller in it — or, with
// listed, only the deleted ones, with nothing of them left to read. It
// returns the source's NQN and the device number the dm-clone still maps,
// whose device is gone.
func serveThenDropSourceCtrls(
	t *testing.T,
	srv *DnAgentServer,
	node *fakeNode,
	names migrDstNames,
	listed bool,
) (string, string) {
	t.Helper()
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !servesThroughClone(t, srv, node, names) || migrRetrying(srv) {
		t.Fatal("fixture is wrong: the destination is not serving " +
			"through its dm-clone with no retry registered")
	}

	nqn := srv.nf.MigrSrcNqn(testCluster, testSrcDn, testSp, testMigrId)
	lost := node.dropCtrlsWhileHeld(nqn, listed)
	if got := cloneSourceNo(node, names); lost == "" || got != lost {
		t.Fatalf("fixture is wrong: the dm-clone maps %q, the dropped "+
			"head was %q", got, lost)
	}
	state, err := srv.host.ListSubsys(ctx, nqn)
	if err != nil {
		t.Fatalf("ListSubsys: %v", err)
	}
	if !state.Found || state.DevicePath != "" ||
		(len(state.Paths) > 0) != listed {
		t.Fatalf("fixture is wrong: the walk reads found=%v, %d "+
			"controller(s), namespace %q; want the kept directory, "+
			"listing its deleted controllers: %v", state.Found,
			len(state.Paths), state.DevicePath, listed)
	}
	for _, path := range state.Paths {
		if path.State != "" || path.TrAddr != "" {
			t.Fatalf("fixture is wrong: the deleted controller %s reads "+
				"state %q, address %q", path.Name, path.State, path.TrAddr)
		}
	}
	briefs, err := srv.host.ListAllSubsys(ctx)
	if err != nil {
		t.Fatalf("ListAllSubsys: %v", err)
	}
	found := false
	for _, brief := range briefs {
		if brief.Nqn != nqn {
			continue
		}
		found = true
		held, err := srv.host.HeldWithHostNqn(ctx, brief,
			srv.nf.DnHostNqn(testCluster, testDn))
		if err != nil {
			t.Fatalf("HeldWithHostNqn: %v", err)
		}
		dev, err := srv.host.SubsysDevicePath(ctx, brief)
		if err != nil {
			t.Fatalf("SubsysDevicePath: %v", err)
		}
		if held || dev != "" {
			t.Fatalf("fixture is wrong: the sweep's walk reads held=%v, "+
				"namespace %q in the kept directory", held, dev)
		}
	}
	if !found {
		t.Fatal("fixture is wrong: the sweep's walk does not list the " +
			"kept directory")
	}
	node.Reset()
	return nqn, lost
}

// cloneSourceNo is the source device number in the dm-clone's live table.
func cloneSourceNo(node *fakeNode, names migrDstNames) string {
	node.mu.Lock()
	defer node.mu.Unlock()
	dm, ok := node.dms[names.clone]
	if !ok {
		return ""
	}
	// 0 <sectors> clone <meta> <dest> <source> <region> ...
	fields := strings.Fields(dm.table)
	if len(fields) < 6 {
		return ""
	}
	return fields[5]
}

// assertCloneOnTheNewHead checks the recovery's end on the node: the source
// holds one controller, and the dm-clone was reloaded once, onto the head that
// controller's connect added, and is not left suspended.
func assertCloneOnTheNewHead(
	t *testing.T,
	node *fakeNode,
	names migrDstNames,
	nqn string,
	lost string,
) {
	t.Helper()
	if got := node.callsMatching(
		"cmd dmsetup reload " + names.clone); len(got) != 1 {
		t.Errorf("%d reloads of the dm-clone, want 1: %v", len(got), got)
	}
	src := cloneSourceNo(node, names)
	node.mu.Lock()
	defer node.mu.Unlock()
	ctrls := 0
	conn, ok := node.conns[nqn]
	if ok {
		ctrls = len(conn.ctrls)
	}
	if ctrls != 1 {
		t.Errorf("the source holds %d controller(s), want 1", ctrls)
		return
	}
	head := node.devNo["/dev/"+conn.device]
	if head == "" || head == lost || src != head {
		t.Errorf("the dm-clone maps %q; the source's head is %q, the lost "+
			"one %q", src, head, lost)
	}
	if node.dms[names.clone].suspended {
		t.Error("the dm-clone was left suspended")
	}
}

// The source's controllers can go for good while the destination serves — a
// reconnect the target refused with DNR deletes them — and the kernel keeps
// the source's subsystem directory while the dm-clone holds its multipath
// head. A directory with no controller is not a connection, and neither is a
// deleted controller the directory still lists until the last reference to
// it drops (HasCtrl): one SyncupSide connects again, reloads the dm-clone
// onto the source's new device and leaves the destination serving through
// it, with no retry left.
func TestMigrationDestinationReconnectsASourceWithNoController(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listed bool
	}{
		{"the kept directory alone", false},
		{"the deleted controller still listed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			names := newMigrDstNames()
			nqn, lost := serveThenDropSourceCtrls(
				t, srv, node, names, tc.listed)

			reply, err := srv.SyncupSide(context.Background(),
				migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE))
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if got := node.callsMatching(
				"cmd nvme connect "); len(got) != 1 {
				t.Fatalf("%d connects, want exactly 1: %v", len(got), got)
			}
			dst := reply.GetSideInfo().GetMigrDstInfo()
			if got := dst.GetTargetInfo(); got.GetStatus() !=
				pb.ResStatus_RES_STATUS_OK {
				t.Errorf("target_info = %v/%q, want OK", got.GetStatus(),
					got.GetDetails())
			}
			if got := dst.GetDmCloneInfo(); got.GetStatus() !=
				pb.ResStatus_RES_STATUS_OK {
				t.Errorf("dm_clone_info = %v/%q, want OK", got.GetStatus(),
					got.GetDetails())
			}
			assertCloneOnTheNewHead(t, node, names, nqn, lost)
			if !servesThroughClone(t, srv, node, names) {
				t.Error("the destination does not serve through its " +
					"dm-clone")
			}
			if migrRetrying(srv) {
				t.Error("a retry is registered after a pass that ran the " +
					"whole sequence")
			}
		})
	}
}

// The same source, but that pass's connect is refused: the pass reports the
// refusal, takes the primary off its dm-clone and registers the retry, and the
// retry loop alone, with no further RPC, connects once the target answers and
// leaves the destination serving through its dm-clone.
func TestMigrationRetryReconnectsASourceWithNoController(t *testing.T) {
	srv, node := newTestServer(t)
	srv.migrRetryInterval = 5 * time.Millisecond
	names := newMigrDstNames()
	nqn, lost := serveThenDropSourceCtrls(t, srv, node, names, false)

	const refused = "Connect Invalid Data Parameter"
	setFailAlways(node, "nvme connect", refused)
	reply, err := srv.SyncupSide(context.Background(),
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	dst := reply.GetSideInfo().GetMigrDstInfo()
	if got := dst.GetTargetInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(got.GetDetails(), refused) {
		t.Errorf("target_info = %v/%q, want ERROR with the refused "+
			"connect's error", got.GetStatus(), got.GetDetails())
	}
	if got := dst.GetDmCloneInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_MISSING ||
		got.GetDetails() != "target not connected" {
		t.Errorf("dm_clone_info = %v/%q, want MISSING/%q", got.GetStatus(),
			got.GetDetails(), "target not connected")
	}
	// Read while the refusal still holds: nothing can have finished yet,
	// and every retry pass meanwhile leaves the stacks where this one put
	// them.
	if !migrRetrying(srv) {
		t.Fatal("no background retry was registered")
	}
	if !sitsOnDmError(t, srv, node, names) {
		t.Error("the primary is not on its dm-error, its namespace " +
			"inaccessible, after a refused connect")
	}
	clearFailAlways(node, "nvme connect")

	waitUntil(t, "the retry loop to reconnect the source and leave the "+
		"destination serving through its dm-clone with no retry "+
		"registered", func() bool {
		return servesThroughClone(t, srv, node, names) && !migrRetrying(srv)
	})
	assertCloneOnTheNewHead(t, node, names, nqn, lost)
}

// A connect that answers success can still leave no controller to read: the
// controller it added is deleted before the pass reads the subsystem again,
// while the dm-clone keeps the subsystem's directory. That is the source
// connection's own answer, read as it stands: the target row says there is no
// controller, the primary goes off a dm-clone that has lost its source, onto
// its dm-error, and the retry is registered.
func TestMigrationDestinationReadsAConnectThatLeftNoController(t *testing.T) {
	srv, node := newTestServer(t)
	// No retry pass may run before the test has looked.
	srv.migrRetryInterval = time.Hour
	names := newMigrDstNames()
	nqn, lost := serveThenDropSourceCtrls(t, srv, node, names, false)
	// The pass waits for a namespace device that never comes; the fake
	// clock spends that bound at once.
	withDnClock(srv, node)
	node.mu.Lock()
	node.lostAfterConnect[nqn] = true
	node.mu.Unlock()

	reply, err := srv.SyncupSide(context.Background(),
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := node.callsMatching("cmd nvme connect "); len(got) != 1 {
		t.Fatalf("%d connects, want exactly 1: %v", len(got), got)
	}
	node.mu.Lock()
	armed := node.lostAfterConnect[nqn]
	node.mu.Unlock()
	if armed {
		t.Fatal("fixture is wrong: the connect kept its controller")
	}
	dst := reply.GetSideInfo().GetMigrDstInfo()
	if got := dst.GetTargetInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR ||
		got.GetDetails() != "no controller for the subsystem" {
		t.Errorf("target_info = %v/%q, want ERROR/%q", got.GetStatus(),
			got.GetDetails(), "no controller for the subsystem")
	}
	if got := dst.GetDmCloneInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_MISSING ||
		got.GetDetails() != "target not connected" {
		t.Errorf("dm_clone_info = %v/%q, want MISSING/%q", got.GetStatus(),
			got.GetDetails(), "target not connected")
	}
	if !migrRetrying(srv) {
		t.Error("no background retry was registered")
	}
	if !sitsOnDmError(t, srv, node, names) {
		t.Error("the primary stayed on a dm-clone whose source has no " +
			"controller")
	}
	if got := node.callsMatching(
		"cmd dmsetup reload " + names.clone); len(got) != 0 {
		t.Errorf("the dm-clone was reloaded: %v", got)
	}
	if got := cloneSourceNo(node, names); got != lost {
		t.Errorf("the dm-clone maps %q, want the lost head %q left in "+
			"place", got, lost)
	}
}

// A namespace node with no controller beside it is no connection either: a
// head the kernel keeps listed with no path (its delayed_removal_secs), or one
// a walk lists just before the head goes with its last controller. When the
// connect brings no controller, the pass reads the namespace device it finds
// as no source: it takes the primary off its dm-clone, onto its dm-error, and
// registers the retry.
func TestMigrationDestinationReadsAPathlessHeadAsNoConnection(t *testing.T) {
	srv, node := newTestServer(t)
	// No retry pass may run before the test has looked.
	srv.migrRetryInterval = time.Hour
	names := newMigrDstNames()
	nqn, lost := serveThenDropSourceCtrls(t, srv, node, names, false)
	// Should the pass wait for a namespace device, the fake clock spends
	// that bound at once.
	withDnClock(srv, node)
	// The dropped head's node comes back as the one the kernel keeps listed,
	// with the device number the dm-clone still maps, and the connect that
	// joins the subsystem loses its controller and the head it added.
	node.mu.Lock()
	conn := node.conns[nqn]
	node.dirs["/sys/class/nvme-subsystem/"+conn.subsys+"/"+conn.device] = true
	node.devNo["/dev/"+conn.device] = lost
	node.lostAfterConnect[nqn] = true
	node.mu.Unlock()

	reply, err := srv.SyncupSide(context.Background(),
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := node.callsMatching("cmd nvme connect "); len(got) != 1 {
		t.Fatalf("%d connects, want exactly 1: %v", len(got), got)
	}
	state, err := srv.host.ListSubsys(context.Background(), nqn)
	if err != nil {
		t.Fatalf("ListSubsys: %v", err)
	}
	if state.HasCtrl() || state.DevicePath == "" {
		t.Fatalf("fixture is wrong: the walk reads %d controller(s), "+
			"namespace %q; want the kept head with no controller",
			len(state.Paths), state.DevicePath)
	}
	dst := reply.GetSideInfo().GetMigrDstInfo()
	if got := dst.GetTargetInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR ||
		got.GetDetails() != "no controller for the subsystem" {
		t.Errorf("target_info = %v/%q, want ERROR/%q", got.GetStatus(),
			got.GetDetails(), "no controller for the subsystem")
	}
	if got := dst.GetDmCloneInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_MISSING ||
		got.GetDetails() != "target not connected" {
		t.Errorf("dm_clone_info = %v/%q, want MISSING/%q", got.GetStatus(),
			got.GetDetails(), "target not connected")
	}
	if !sitsOnDmError(t, srv, node, names) {
		t.Error("the primary stayed on a dm-clone whose source has no " +
			"controller")
	}
	if !migrRetrying(srv) {
		t.Error("no background retry was registered")
	}
	if got := node.callsMatching(
		"cmd dmsetup reload " + names.clone); len(got) != 0 {
		t.Errorf("the dm-clone was reloaded: %v", got)
	}
}

// A source controller that is still reconnecting is a connection: the pass
// connects nothing beside it, reports its path's state, and leaves the
// dm-clone on the namespace device it reads through.
func TestMigrationDestinationLeavesAReconnectingSourceAlone(t *testing.T) {
	srv, node := newTestServer(t)
	names := newMigrDstNames()
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !servesThroughClone(t, srv, node, names) || migrRetrying(srv) {
		t.Fatal("fixture is wrong: the destination is not serving " +
			"through its dm-clone with no retry registered")
	}
	nqn := srv.nf.MigrSrcNqn(testCluster, testSrcDn, testSp, testMigrId)
	src := cloneSourceNo(node, names)
	node.setConnState(nqn, "connecting")
	node.Reset()

	reply, err := srv.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := node.callsMatching("cmd nvme connect "); len(got) != 0 {
		t.Fatalf("%d connects beside a reconnecting controller: %v",
			len(got), got)
	}
	dst := reply.GetSideInfo().GetMigrDstInfo()
	if got := dst.GetTargetInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR ||
		got.GetDetails() != "paths: connecting" {
		t.Errorf("target_info = %v/%q, want ERROR/%q", got.GetStatus(),
			got.GetDetails(), "paths: connecting")
	}
	if got := node.callsMatching(
		"cmd dmsetup reload " + names.clone); len(got) != 0 {
		t.Errorf("the dm-clone was reloaded: %v", got)
	}
	if got := cloneSourceNo(node, names); got != src {
		t.Errorf("the dm-clone maps %q, want %q", got, src)
	}
	if !servesThroughClone(t, srv, node, names) {
		t.Error("the destination does not serve through its dm-clone")
	}
}
