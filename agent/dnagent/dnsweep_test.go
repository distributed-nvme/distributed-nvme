package dnagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Teardown by sweep, the dn half (architecture.md, Teardown by sweep; DN6).
//
// Every test here pins one property of the same principle: what the agent
// removes is derived by subtracting the desired state from what the node
// ACTUALLY holds, and nothing about a failed removal is remembered. The dn's
// stake in it is larger than the cn's, because a dn removal gates an on-disk
// ALLOCATION RECORD: freeing the extents of a side whose device still maps
// them hands those extents to the next side, which is a corruption path and
// not a leak.
//
// The fixtures deliberately use explicit side pointers rather than the
// package's sidePtr, because several of these cases need two sides on ONE sp:
// a :2: export names (cluster, sp, leg, cn) and no side at all, so two sides
// of the same leg legitimately share one NQN ([D1]) and a test that put them
// on one leg would be asserting about a name two objects answer to.

// sweepSidePtr is sidePtr with the leg id spelled out.
func sweepSidePtr(legId, sideId uint64) *pb.SidePointer {
	return &pb.SidePointer{SpId: testSp, LegId: legId, SideId: sideId}
}

// sweepDnReq is dnReq over explicit pointers.
func sweepDnReq(
	revision uint64,
	ptrs ...*pb.SidePointer,
) *pb.SyncupDnRequest {
	req := dnReq(revision)
	req.SidePointerList = ptrs
	return req
}

// sweepSideReq is sideReq over an explicit pointer: cn0 primary, cn1 standby,
// SP_LEVEL_READWRITE — the shape every sweep case tears down from.
func sweepSideReq(
	revision uint64,
	ptr *pb.SidePointer,
) *pb.SyncupSideRequest {
	req := sideReq(revision, ptr.GetSideId(), testCn0, []uint64{testCn1},
		pb.SpLevel_SP_LEVEL_READWRITE)
	req.SidePointer = ptr
	return req
}

// sweepDstReq is sweepSideReq playing the destination of one migration.
func sweepDstReq(
	revision uint64,
	ptr *pb.SidePointer,
	migrId uint64,
) *pb.SyncupSideRequest {
	req := sweepSideReq(revision, ptr)
	req.MigrDstConf = &pb.SyncupSideRequest_MigrDstConf{
		MigrId:        migrId,
		SrcSideId:     ptr.GetSideId() + 0x100,
		SrcDnId:       testSrcDn,
		SrcNvmeTrConf: testTrConf(),
		BlockSize:     testBlockSize,
		MetaBlocks:    testMetaBlocks,
		DmCloneConf: &pb.DmCloneConf{
			HydrationThreshold: 1, HydrationBatchSize: 1,
		},
		BmCnt: 1,
	}
	return req
}

// sweepSrcReq is sweepSideReq playing the source of one migration whose
// destination has provisioned, so the role is live (architecture.md,
// Migration).
func sweepSrcReq(
	revision uint64,
	ptr *pb.SidePointer,
	migrId uint64,
) *pb.SyncupSideRequest {
	req := sweepSideReq(revision, ptr)
	req.MigrSrcConf = &pb.SyncupSideRequest_MigrSrcConf{
		MigrId:         migrId,
		DstSideId:      ptr.GetSideId() + 0x100,
		DstDnId:        testSrcDn,
		DstProvisioned: true,
	}
	return req
}

// dmPresent / subsysPresent read the fake's objects rather than its call log:
// a sweep is judged by what is left on the node, never by what it said.
func dmPresent(node *fakeNode, name string) bool {
	node.mu.Lock()
	defer node.mu.Unlock()
	_, ok := node.dms[name]
	return ok
}

func subsysPresent(node *fakeNode, nqn string) bool {
	node.mu.Lock()
	defer node.mu.Unlock()
	return node.dirs[agent.NvmetRoot+"/subsystems/"+nqn]
}

func connPresent(node *fakeNode, nqn string) bool {
	node.mu.Lock()
	defer node.mu.Unlock()
	_, ok := node.conns[nqn]
	return ok
}

// setHook installs one of the fake's command hooks under its lock, because a
// zeroing goroutine may be running commands while the test writes them.
func setHook(node *fakeNode, hook map[string]bool, key string) {
	node.mu.Lock()
	defer node.mu.Unlock()
	hook[key] = true
}

func clearHook(node *fakeNode, hook map[string]bool, key string) {
	node.mu.Lock()
	defer node.mu.Unlock()
	delete(hook, key)
}

func setFailAlways(node *fakeNode, key string, stderr string) {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.failCmdAlways[key] = stderr
}

func clearFailAlways(node *fakeNode, key string) {
	node.mu.Lock()
	defer node.mu.Unlock()
	delete(node.failCmdAlways, key)
}

// sideObjects is every name one converged side of this package's fixtures
// answers to: the d4 side device, a d0/d1 pair per cn, and a :2: export per
// cn.
type sideObjects struct {
	sideDev string
	dms     []string
	nqns    []string
}

func objectsOf(ptr *pb.SidePointer) sideObjects {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	sp, side := ptr.GetSpId(), ptr.GetSideId()
	objs := sideObjects{
		sideDev: nf.DnSideName(testCluster, testDn, sp, side),
	}
	objs.dms = append(objs.dms, objs.sideDev)
	for _, cnId := range []uint64{testCn0, testCn1} {
		objs.dms = append(objs.dms,
			nf.DnErrorName(testCluster, testDn, sp, side, cnId),
			nf.DnLinearName(testCluster, testDn, sp, side, cnId))
		objs.nqns = append(objs.nqns,
			nf.SideToCnNqn(testCluster, sp, ptr.GetLegId(), cnId))
	}
	return objs
}

func (o sideObjects) assertAllPresent(t *testing.T, node *fakeNode) {
	t.Helper()
	for _, name := range o.dms {
		if !dmPresent(node, name) {
			t.Fatalf("fixture is wrong: %s was never built", name)
		}
	}
	for _, nqn := range o.nqns {
		if !subsysPresent(node, nqn) {
			t.Fatalf("fixture is wrong: %s was never exported", nqn)
		}
	}
}

// assertAllKept is assertAllPresent after a sweep: the objects are the ones a
// pass had to leave alone, so every one of them is reported rather than
// stopping at the first.
func (o sideObjects) assertAllKept(t *testing.T, node *fakeNode) {
	t.Helper()
	for _, name := range o.dms {
		if !dmPresent(node, name) {
			t.Errorf("%s was removed by a pass that had to leave it alone",
				name)
		}
	}
	for _, nqn := range o.nqns {
		if !subsysPresent(node, nqn) {
			t.Errorf("%s was removed by a pass that had to leave it alone",
				nqn)
		}
	}
}

func (o sideObjects) assertAllGone(t *testing.T, node *fakeNode) {
	t.Helper()
	for _, name := range o.dms {
		if dmPresent(node, name) {
			t.Errorf("%s survived the sweep", name)
		}
	}
	for _, nqn := range o.nqns {
		if subsysPresent(node, nqn) {
			t.Errorf("%s survived the sweep", nqn)
		}
	}
}

// ---------------------------------------------------------------------------
// The node-level sweep finds a forgotten side by name
// ---------------------------------------------------------------------------

// TestRemovedSideSweptByName pins the property the whole design rests on: a
// side's resources are removed because their NAMES are on the node and the
// desired state does not want them, not because the agent remembers the side.
//
// It matters because architecture.md, Teardown by sweep, deletes that memory
// FIRST: a side whose pointer has left its DN's list has its state file, its
// chunks, its memory entry and its
// object lock dropped in the same pass, before a single removal is attempted.
// A removal path that needed any of it would leak every such side for ever —
// which is exactly the bug this design started from, where the teardown
// deleted the state after a best-effort pass whose every step only logged its
// failure.
func TestRemovedSideSweptByName(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	objs := objectsOf(sidePtr(testSide))
	objs.assertAllPresent(t, node)

	// Forget the side the way dropSideState leaves it, so the only thing the
	// pass below can possibly work from is the enumeration.
	key := sideKey(testCluster, testDn, testSp, testSide)
	srv.dropSide(key)
	sidePath := nf.LocalSidePath(testCluster, testDn, testSp, testSide)
	node.mu.Lock()
	delete(node.protos, sidePath)
	node.mu.Unlock()

	node.Reset()
	reply, err := srv.SyncupDn(ctx, dnReq(2))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Fatalf("code = %d (%s), want 0: the sweep left something behind",
			got, reply.GetAgentReply().GetDetails())
	}
	objs.assertAllGone(t, node)
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); ok {
		t.Error("the allocation record survived a side nothing wants")
	}
	// The removals really happened in THIS pass; the fixture was not already
	// clean when it started.
	if !node.hasCall("cmd dmsetup remove " + objs.sideDev) {
		t.Errorf("the side device was never removed:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
}

// ---------------------------------------------------------------------------
// A record is released only after the device is VERIFIED gone
// ---------------------------------------------------------------------------

// TestSideRecordFreedOnlyAfterDeviceGone pins the corruption path this design
// closes, and it is the one property here that is not about leaks: the
// allocation record of a side stays in the volume table for as long as a
// device still maps its extents. Free it early and the allocator hands those
// same extents to the next side, which then writes over live data through a
// device nobody remembers.
//
// The failure modelled is the one that cannot be told from success: a
// `dmsetup remove` killed at the soft timeout, exit -1, with nothing done in
// the kernel. The agent learned NOTHING from that command, so the only
// evidence it may act on is the probe afterwards — and the probe says the
// device is still there.
func TestSideRecordFreedOnlyAfterDeviceGone(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	objs := objectsOf(sidePtr(testSide))
	objs.assertAllPresent(t, node)

	// killCmdNoEffectAlways, not the one-shot form: the side device is
	// reached twice in one pass (the chain's last layer, then the record
	// sweep that follows it), and the property is that NEITHER may free the
	// record while the device is there.
	setHook(node, node.killCmdNoEffectAlways, "dmsetup remove "+objs.sideDev)

	node.Reset()
	reply, err := srv.SyncupDn(ctx, dnReq(2))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	// The whole test is this assertion: the extents are still allocated.
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); !ok {
		t.Fatal("the extents were freed while a device still maps them: " +
			"the next side allocated here would overwrite live data")
	}
	if !dmPresent(node, objs.sideDev) {
		t.Fatal("the fake removed the device the test told it to leave")
	}
	if got := reply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeLeftover {
		t.Errorf("code = %d (%s), want %d: the worker is only told to re-sync "+
			"by the reply code", got, reply.GetAgentReply().GetDetails(),
			common.ReplyCodeLeftover)
	}
	if got := reply.GetAgentReply().GetDetails(); !strings.Contains(
		got, objs.sideDev) {
		t.Errorf("details = %q, want the stuck device named", got)
	}

	// The next pass is an ordinary one — nothing about the failure was
	// stored, so the same request at a later revision retries by
	// re-enumerating — and it both removes the device and frees the record.
	clearHook(node, node.killCmdNoEffectAlways,
		"dmsetup remove "+objs.sideDev)
	node.Reset()
	reply, err = srv.SyncupDn(ctx, dnReq(3))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("retry code = %d (%s), want 0",
			got, reply.GetAgentReply().GetDetails())
	}
	if dmPresent(node, objs.sideDev) {
		t.Error("the retry did not remove the side device")
	}
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); ok {
		t.Error("the record was never freed once its device was gone")
	}
}

// TestKilledDmInfoDoesNotFreeARecord is the narrower half of the same
// property, driven through the primitive instead of the removal: with
// `dmsetup info` killed, the agent cannot know whether the side device is
// there, and "I could not tell" must never be read as "it is gone".
//
// Before the fix Dm.Info returned nil, nil on ANY failure, so a killed probe
// reported the device absent, removeDm reported success and the caller freed
// the extents underneath a device that was still live. That is why Dm.Info
// now separates a tool that answered "no such device" from a tool that did
// not answer at all.
func TestKilledDmInfoDoesNotFreeARecord(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	objs := objectsOf(sidePtr(testSide))
	objs.assertAllPresent(t, node)

	// "-o attr <name>" is the tail of `dmsetup info --columns --noheadings
	// -o attr <name>` and of nothing else, so only the probe of this one
	// device is killed.
	setHook(node, node.killCmdNoEffectAlways, "-o attr "+objs.sideDev)

	node.Reset()
	reply, err := srv.SyncupDn(ctx, dnReq(2))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); !ok {
		t.Fatal("a killed probe was read as \"the device is gone\" and the " +
			"extents were freed under a live device")
	}
	if !dmPresent(node, objs.sideDev) {
		t.Fatal("the fake removed the device although its probe was killed")
	}
	if got := reply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeLeftover {
		t.Errorf("code = %d (%s), want %d: an object whose state is unknown "+
			"is a leftover", got, reply.GetAgentReply().GetDetails(),
			common.ReplyCodeLeftover)
	}

	// And once the probe answers again the record goes, so the assertion
	// above is about the killed probe and not about a side that could never
	// be freed at all.
	clearHook(node, node.killCmdNoEffectAlways, "-o attr "+objs.sideDev)
	if _, err := srv.SyncupDn(ctx, dnReq(3)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if _, ok, _ := srv.meta.LookupSide(ctx, testSp, testSide); ok {
		t.Error("the record survived a pass whose probes all answered")
	}
}

// ---------------------------------------------------------------------------
// Migration objects: derived from live tables, removed in DN6 order
// ---------------------------------------------------------------------------

// TestFinishedMigrationRepointsThenRemovesClone pins P-DN1: a per-CN
// dm-linear whose LIVE TABLE still maps a finished migration's dm-clone is
// reloaded onto the plain side device before the clone is removed, in the
// same pass. The dm-clone sits under the linear, and `dmsetup remove` on a
// device another live table maps fails EBUSY — so without the repoint the
// removal cannot work, and the clone, its metadata wrapper and (because both
// rest on it) the side device itself wait a round for the build phase.
//
// The pass runs on a RESTARTED agent on purpose: a memory of the previous
// converge would not survive the restart, and a process that came up after
// the cutover is where the removal is needed most.
// Reading the live table instead is what makes the repoint a
// property of the node rather than of the agent's memory.
func TestFinishedMigrationRepointsThenRemovesClone(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	provisionSide(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	metaName := nf.DnMigrMetaDmName(testCluster, testDn, testSp, testMigrId)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	sideName := nf.DnSideName(testCluster, testDn, testSp, testSide)
	if !dmPresent(node, cloneName) {
		t.Fatal("the destination never built a dm-clone")
	}

	// The restart: srv2 knows only what is on the disk, in the store and on
	// the node.
	srv2 := startTestServer(t, node)
	node.Reset()
	reply, err := srv2.SyncupSide(ctx, sideReq(2, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Fatalf("code = %d (%s), want 0",
			got, reply.GetAgentReply().GetDetails())
	}
	reload := node.indexOfCall("cmd dmsetup reload " + linName)
	remove := node.indexOfCall("cmd dmsetup remove " + cloneName)
	if reload < 0 || remove < 0 {
		t.Fatalf("reload at %d, remove at %d, want both in:\n%s",
			reload, remove, strings.Join(node.Calls(), "\n"))
	}
	// The FIRST removal attempt is after the repoint, not merely some
	// attempt: an attempt made under the live linear is the EBUSY this
	// pre-step exists to avoid.
	if reload > remove {
		t.Errorf("the clone was removed under a linear that still mapped "+
			"it (reload at %d, remove at %d)", reload, remove)
	}
	if got := len(node.callsMatching(
		"cmd dmsetup remove " + cloneName)); got != 1 {
		t.Errorf("%d removal attempts for the clone, want 1", got)
	}
	for _, name := range []string{cloneName, metaName} {
		if dmPresent(node, name) {
			t.Errorf("%s survived the finish", name)
		}
	}
	if _, ok, _ := srv2.meta.LookupCloneMeta(ctx, testSp, testMigrId); ok {
		t.Error("the clone-metadata record survived the finish")
	}
	node.mu.Lock()
	table, sideNo := node.dms[linName].table, node.devNo[nf.DmPath(sideName)]
	node.mu.Unlock()
	if !strings.Contains(table, sideNo) {
		t.Errorf("dm-linear table %q was not repointed at the side device "+
			"(%s)", table, sideNo)
	}
}

// TestMigrationCloneRemovedBeforeItsSourceDisconnect pins DN6's one ordering
// rule inside a chain layer, and the stop rule of architecture.md,
// Teardown by sweep, that protects it: the
// destination's dm-clone goes before the :3: connection it hydrates through,
// and if the clone will not go, the connection is NOT touched at all.
//
// A dm-clone with a live hydration reader strands its in-flight IO the moment
// the source device disappears from under it — the removal then blocks until
// the hard timeout, and the region it was copying is neither hydrated nor
// marked. "Finish the layer, never descend below a layer with leftovers" is
// what keeps a pass that could not remove the clone from doing exactly that.
func TestMigrationCloneRemovedBeforeItsSourceDisconnect(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	srcNqn := nf.MigrSrcNqn(testCluster, testSrcDn, testSp, testMigrId)

	setup := func(t *testing.T) (*DnAgentServer, *fakeNode) {
		t.Helper()
		srv, node := newTestServer(t)
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		if _, err := srv.SyncupSide(ctx,
			migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if !connPresent(node, srcNqn) {
			t.Fatal("the destination never connected to its source")
		}
		node.Reset()
		return srv, node
	}

	t.Run("order", func(t *testing.T) {
		srv, node := setup(t)
		if _, err := srv.SyncupSide(context.Background(), sideReq(3,
			testSide, testCn0, []uint64{testCn1},
			pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		assertOrder(t, node,
			"cmd dmsetup remove "+cloneName,
			"cmd nvme disconnect --nqn "+srcNqn,
		)
		if connPresent(node, srcNqn) {
			t.Error("the migration connection survived the finish")
		}
	})

	t.Run("stuck clone holds the descent", func(t *testing.T) {
		srv, node := setup(t)
		setHook(node, node.killCmdNoEffectAlways,
			"dmsetup remove "+cloneName)
		reply, err := srv.SyncupSide(context.Background(), sideReq(3,
			testSide, testCn0, []uint64{testCn1},
			pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if !dmPresent(node, cloneName) {
			t.Fatal("the fake removed the clone the test told it to leave")
		}
		if !connPresent(node, srcNqn) {
			t.Fatal("the source was pulled out from under a live dm-clone")
		}
		if node.hasCall("cmd nvme disconnect") {
			t.Errorf("a disconnect was attempted below a stuck layer:\n%s",
				strings.Join(node.Calls(), "\n"))
		}
		if got := reply.GetAgentReply().GetCode(); got !=
			common.ReplyCodeLeftover {
			t.Errorf("code = %d (%s), want %d", got,
				reply.GetAgentReply().GetDetails(), common.ReplyCodeLeftover)
		}
		if got := reply.GetAgentReply().GetDetails(); !strings.Contains(
			got, cloneName) {
			t.Errorf("details = %q, want the stuck clone named", got)
		}

		// The next pass finishes the job: nothing about the stuck layer was
		// remembered, so the descent simply happens again.
		clearHook(node, node.killCmdNoEffectAlways,
			"dmsetup remove "+cloneName)
		node.Reset()
		reply, err = srv.SyncupSide(context.Background(), sideReq(4,
			testSide, testCn0, []uint64{testCn1},
			pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if got := reply.GetAgentReply().GetCode(); got != 0 {
			t.Errorf("retry code = %d (%s), want 0",
				got, reply.GetAgentReply().GetDetails())
		}
		if dmPresent(node, cloneName) || connPresent(node, srcNqn) {
			t.Error("the retry left the migration behind")
		}
	})
}

// TestMigrationSourceGoneBesideADeletedController pins what L3's probe
// reads as gone (DN6): a controller that `nvme disconnect` deleted can stay
// listed in its subsystem until the last reference to it drops, with nothing
// of it left to read (cnagent.md CN10), and it is no controller (hasCtrl).
// The SyncupSide that ends the destination role finds the source connection
// gone after its one disconnect and descends below L3, so the clone-metadata
// wrapper and its record go in that same pass.
func TestMigrationSourceGoneBesideADeletedController(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	names := newMigrDstNames()
	srcNqn := srv.nf.MigrSrcNqn(testCluster, testSrcDn, testSp, testMigrId)
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !connPresent(node, srcNqn) || !dmPresent(node, names.meta) {
		t.Fatal("fixture is wrong: the destination holds no source " +
			"connection or no clone-metadata wrapper")
	}
	node.mu.Lock()
	node.listedAfterDisconnect[srcNqn] = true
	node.mu.Unlock()
	node.Reset()

	reply, err := srv.SyncupSide(ctx, sideReq(3, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := node.callsMatching(
		"cmd nvme disconnect --nqn " + srcNqn); len(got) != 1 {
		t.Fatalf("%d disconnects, want exactly 1: %v", len(got), got)
	}
	state, err := srv.host.ListSubsys(ctx, srcNqn)
	if err != nil {
		t.Fatalf("ListSubsys: %v", err)
	}
	if !state.Found || len(state.Paths) == 0 || hasCtrl(state) {
		t.Fatalf("fixture is wrong: the walk reads found=%v, %d listed "+
			"controller(s), a live one %v; want the deleted controller "+
			"still listed", state.Found, len(state.Paths), hasCtrl(state))
	}
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("code = %d (%s), want 0", got,
			reply.GetAgentReply().GetDetails())
	}
	for _, name := range []string{names.clone, names.meta} {
		if dmPresent(node, name) {
			t.Errorf("%s survived the pass that ended the role", name)
		}
	}
	_, ok, err := srv.meta.LookupCloneMeta(ctx, testSp, testMigrId)
	if err != nil {
		t.Fatalf("LookupCloneMeta: %v", err)
	}
	if ok {
		t.Error("the clone-metadata record survived the pass that ended " +
			"the role")
	}
}

// ---------------------------------------------------------------------------
// Cross-side objects: judged by the claim rule, not by a name
// ---------------------------------------------------------------------------

// TestOrphanExportsAndConnectionsSwept pins the attribution rule for the two
// dn objects whose names cannot say which side they belong to. A :2: export
// names (cluster, sp, leg, cn) and no side at all — both sides of a migrating
// leg export that one NQN ([D1]) — and a :3: host connection names the SOURCE
// dn's id, not ours. Neither can be swept by decoding its name, so each is
// kept iff some side this agent still holds state for claims it, and removed
// otherwise.
//
// The rule has to work in both directions in the SAME pass, which is why this
// test tears one of two sides down and asserts the other's export and
// connection are untouched: a sweep that only got the "remove" half right
// would disconnect a live migration's source the moment any other side left
// the list.
func TestOrphanExportsAndConnectionsSwept(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()

	const migrKept = testMigrId
	const migrGone = testMigrId + 1
	kept := sweepSidePtr(testLeg, testSide)
	gone := sweepSidePtr(testLeg+1, testSide2)

	if _, err := srv.SyncupDn(ctx, sweepDnReq(1, kept, gone)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	syncupSideTwoPhase(t, srv, sweepDstReq(1, kept, migrKept))
	syncupSideTwoPhase(t, srv, sweepDstReq(1, gone, migrGone))

	keptObjs, goneObjs := objectsOf(kept), objectsOf(gone)
	keptObjs.assertAllPresent(t, node)
	goneObjs.assertAllPresent(t, node)
	keptConn := nf.MigrSrcNqn(testCluster, testSrcDn, testSp, migrKept)
	goneConn := nf.MigrSrcNqn(testCluster, testSrcDn, testSp, migrGone)
	for _, nqn := range []string{keptConn, goneConn} {
		if !connPresent(node, nqn) {
			t.Fatalf("fixture is wrong: %s was never connected", nqn)
		}
	}

	node.Reset()
	reply, err := srv.SyncupDn(ctx, sweepDnReq(2, kept))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Fatalf("code = %d (%s), want 0",
			got, reply.GetAgentReply().GetDetails())
	}

	// The unclaimed half goes.
	goneObjs.assertAllGone(t, node)
	if connPresent(node, goneConn) {
		t.Error("a :3: connection no stored side's destination claims " +
			"survived")
	}
	// The claimed half stays — every object of it, not just the one the
	// removed side's name could have collided with.
	keptObjs.assertAllKept(t, node)
	if !connPresent(node, keptConn) {
		t.Error("the surviving side's migration connection was disconnected")
	}
	for _, nqn := range keptObjs.nqns {
		if node.hasCall("cmd rmdir " + agent.NvmetRoot + "/subsystems/" + nqn) {
			t.Errorf("the sweep tried to remove a claimed export: %s", nqn)
		}
	}
	if node.hasCall("cmd nvme disconnect --nqn " + keptConn) {
		t.Error("the sweep tried to disconnect a claimed migration source")
	}
}

// ---------------------------------------------------------------------------
// What the node-level pass pays for its siblings' exports
// ---------------------------------------------------------------------------

// TestNodeSweepCostIsLinearInOwnExports pins the cost of attributing a :2:
// export on a kernel several dn agents share. The NQN names no dn, so the
// node-level pass attributes every export of an sp this node hosts that no
// held side of its own claims — in the verdict of every Check round and in
// every SyncupDn — and on a shared kernel nearly all of those are the
// siblings': each side-holding agent exports its own leg of the sp, one
// export per cn. Attributing each with an `ls` exec of its namespaces made
// every side-holding agent's pass grow with everybody else's exports of the
// sp, so the node ran about s·(s−1) execs per cn per round for s
// side-holding agents: at 32 slices most of the roughly 617 execs a second
// estimated for one disk-node VM.
//
// dnv builds every side export with one namespace, nsid 1, so its
// device_path is read directly and the namespaces are listed only when there
// is no namespace 1. The test runs both passes — a Check round's verdict and a
// SyncupDn — over four sibling agents of one sp holding one export each, then
// eight each, then thirty-two agents holding one each, and counts the execs:
// the count may not move. The third run holds the number of exports of the
// second and changes only the number of agents, each on its own port: a cost
// per sibling AGENT (a port listing, say) is as quadratic on the node as one
// per export, and the first two runs hold the agent count still. Each pass
// also reads each sibling export's namespace 1 exactly once, which is what
// makes a cache of the attribution pointless within a pass.
func TestNodeSweepCostIsLinearInOwnExports(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)

	// execs seeds `per` exports for each of `siblings` sibling agents, runs
	// the two passes and returns the `ls` and the total exec count of each.
	type cost struct{ verdictLs, verdictCmds, syncLs, syncCmds int }
	execs := func(t *testing.T, siblings, per int) cost {
		srv, node := newTestServer(t)
		ctx := context.Background()
		// A side of OUR own under testSp is what puts the sp in the
		// pass's hosted set; without it no sibling export of it would be
		// attributed at all, and the count would pin nothing.
		ours := sweepSidePtr(testLeg, testSide)
		if _, err := srv.SyncupDn(ctx, sweepDnReq(1, ours)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		syncupSideTwoPhase(t, srv, sweepSideReq(1, ours))

		var seeded []string
		for i := 0; i < siblings; i++ {
			for j := 0; j < per; j++ {
				n := uint64(i*16 + j)
				nqn := nf.SideToCnNqn(testCluster, testSp, siblingLeg+n,
					testCn0)
				seedExport(node, nqn, "/dev/mapper/"+nf.DnLinearName(
					testCluster, siblingDn+uint64(i), testSp,
					siblingSide+n, testCn0))
				linkPort(node, siblingPort+i, nqn)
				seeded = append(seeded, nqn)
			}
		}
		// readsOnce checks that the pass just run attributed every sibling
		// export by exactly one read of its namespace 1: none skipped, none
		// read twice.
		readsOnce := func(pass string) {
			t.Helper()
			for _, nqn := range seeded {
				got := len(node.callsMatching("read " + agent.NvmetRoot +
					"/subsystems/" + nqn + "/namespaces/1/device_path"))
				if got != 1 {
					t.Errorf("siblings=%d per=%d: the %s read %s's "+
						"namespace 1 %d times, want 1", siblings, per, pass,
						nqn, got)
				}
			}
		}

		var c cost
		node.Reset()
		reply, _ := srv.checkDnRound(ctx, &pb.CheckDnRequest{
			ClusterId: testCluster, DnId: testDn,
		}, nil)
		if got := reply.GetAgentReply().GetCode(); got != 0 {
			t.Fatalf("siblings=%d per=%d: CheckDn code = %d (%s), want 0",
				siblings, per, got, reply.GetAgentReply().GetDetails())
		}
		c.verdictLs = len(node.callsMatching("cmd ls "))
		c.verdictCmds = len(node.callsMatching("cmd "))
		readsOnce("CheckDn verdict")

		node.Reset()
		syncReply, err := srv.SyncupDn(ctx, sweepDnReq(2, ours))
		if err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		if got := syncReply.GetAgentReply().GetCode(); got != 0 {
			t.Fatalf("siblings=%d per=%d: SyncupDn code = %d (%s), want 0",
				siblings, per, got, syncReply.GetAgentReply().GetDetails())
		}
		c.syncLs = len(node.callsMatching("cmd ls "))
		c.syncCmds = len(node.callsMatching("cmd "))
		readsOnce("SyncupDn")

		// Cheaper is only worth having if it is still right: every
		// sibling export is attributed to its agent and left alone.
		for _, nqn := range seeded {
			if !subsysPresent(node, nqn) {
				t.Errorf("siblings=%d per=%d: the sweep removed a "+
					"sibling's export %s", siblings, per, nqn)
			}
		}
		objectsOf(ours).assertAllKept(t, node)
		return c
	}

	const siblings = 4
	few, many := execs(t, siblings, 1), execs(t, siblings, 8)
	// The same 32 exports held by 32 agents, each on its own port: a cost
	// per sibling AGENT (a port listing, say) is as quadratic on the node
	// as one per export, and the two runs above hold the agent count still.
	agents := execs(t, siblings*8, 1)
	t.Logf("execs with %d sibling exports: %+v; with %d: %+v; with %d "+
		"agents holding one each: %+v",
		siblings, few, siblings*8, many, siblings*8, agents)
	if agents != few {
		t.Errorf("execs grew with the number of sibling agents: %+v with "+
			"%d of them, %+v with %d", few, siblings, agents, siblings*8)
	}
	if many.verdictLs != few.verdictLs || many.syncLs != few.syncLs {
		t.Errorf("`ls` execs grew with the siblings' exports: the verdict "+
			"ran %d with %d of them and %d with %d, the SyncupDn %d and %d",
			few.verdictLs, siblings, many.verdictLs, siblings*8,
			few.syncLs, many.syncLs)
	}
	if many.verdictCmds != few.verdictCmds || many.syncCmds != few.syncCmds {
		t.Errorf("execs grew with the siblings' exports: the verdict ran "+
			"%d with %d of them and %d with %d, the SyncupDn %d and %d",
			few.verdictCmds, siblings, many.verdictCmds, siblings*8,
			few.syncCmds, many.syncCmds)
	}
}

// ---------------------------------------------------------------------------
// A conf fault must never destroy resources (architecture.md, Common validation)
// ---------------------------------------------------------------------------

// TestDnDeferredParentConfSkipsSweep pins the Common validation rule at its
// sharpest point.
// A DN whose stored extent_size is unusable converges nothing — every side's
// run is carved out of that number, so nothing can be computed from it — and
// it must sweep nothing either, even though the sweep would otherwise be
// entitled to remove every one of these objects: their sides are in no
// pointer list at all.
//
// The shape matters as much as the rule. The obvious implementation of "skip
// the unusable DN" reads to the rest of the agent as "this DN does not
// exist", and a side whose parent does not exist is a side that left its
// parent's list — which is how a conf fault turns into a teardown of live
// data. The control below is the other half: with a usable extent size the
// very same node IS swept, so this test cannot pass by never sweeping.
func TestDnDeferredParentConfSkipsSweep(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	objs := objectsOf(sidePtr(testSide))

	// build leaves a node holding one converged side whose pointer is no
	// longer in the dn file: a restart of this node with a usable extent size
	// sweeps the side away, and with an unusable one must not.
	build := func(t *testing.T, extentSize uint64) *fakeNode {
		t.Helper()
		ctx := context.Background()
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		objs.assertAllPresent(t, node)
		// The CP's drop, persisted but never converged: the dn file names no
		// side any more, and the side's own file is gone, exactly as a
		// pointer removal leaves them (architecture.md, Teardown by sweep).
		req := dnReq(2)
		req.ExtentSize = extentSize
		node.mu.Lock()
		delete(node.protos,
			nf.LocalSidePath(testCluster, testDn, testSp, testSide))
		node.mu.Unlock()
		if err := node.writeProto(
			ctx, nf.LocalDnPath(testCluster, testDn), req); err != nil {
			t.Fatalf("seeding the dn state file: %v", err)
		}
		node.Reset()
		return node
	}

	t.Run("unusable extent size", func(t *testing.T) {
		node := build(t, 0)
		srv := startTestServer(t, node)
		// Not one removal, of any kind, anywhere on the node.
		for _, banned := range []string{
			"cmd dmsetup remove", "cmd nvme disconnect", "cmd rmdir",
		} {
			if calls := node.callsMatching(banned); len(calls) != 0 {
				t.Errorf("a refused dn conf drove removals: %v", calls)
			}
		}
		objs.assertAllKept(t, node)
		if _, ok, _ := srv.meta.LookupSide(
			context.Background(), testSp, testSide); !ok {
			t.Error("a refused dn conf freed the side's extents")
		}
	})

	t.Run("usable extent size (control)", func(t *testing.T) {
		node := build(t, testExtentSize)
		srv := startTestServer(t, node)
		objs.assertAllGone(t, node)
		if _, ok, _ := srv.meta.LookupSide(
			context.Background(), testSp, testSide); ok {
			t.Error("the control did not sweep, so the case above proves " +
				"nothing")
		}
	})
}

// ---------------------------------------------------------------------------
// The cutover fence window (architecture.md, Migration, src step 2) and the
// order the layers run in
// ---------------------------------------------------------------------------

// TestSuspendedLinearResumedBeforeNvmetDisable pins the sweep's P0 step for a
// linear the pass removes: every suspended per-CN dm-linear it is about to
// remove, or whose export it is about to remove, is put on its dm-error and
// resumed BEFORE the nvmet namespace above it is disabled — for this standby,
// whose pre-fence table already is its dm-error, the resume alone
// (TestTeardownInsideTheFenceWindow and TestNoSideInsideTheFenceWindow pin
// the primary's reload).
//
// Writing 0 to a namespace's `enable` first waits for every request in flight
// on the namespace, and one whose bio a suspended dm target holds never
// completes — such a target queues bios with no timeout and no error path —
// so the writer sits in uninterruptible D state and the pass never returns.
// The DN12 cutover fence leaves exactly such devices behind for
// common.SuspendSeconds, holding what the old primary had in flight, and
// anything the control plane retires inside that window is retired over
// them. Resuming inside removeDm would be too late: by then the namespace
// above has already been written.
//
// The scope here is the SIDE's own sweep — a source side inside its window
// whose standby is dropped — because the window belongs to a side that is
// very much still in the pointer list.
func TestSuspendedLinearResumedBeforeNvmetDisable(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	stbLin := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1)
	stbNqn := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn1)

	// A window long enough that it cannot elapse mid-test.
	srv.fenceWait = time.Hour
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	suspended := node.dms[stbLin].suspended
	node.mu.Unlock()
	if !suspended {
		t.Fatal("the cutover did not suspend the standby's dm-linear")
	}

	// The standby leaves the side's cn list while the fence holds: its
	// linear is unwanted, suspended, and has a live export over it.
	node.Reset()
	dropped := migrSrcReq(3)
	dropped.SideConf.StandbyIdList = nil
	reply, err := srv.SyncupSide(ctx, dropped)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Fatalf("code = %d (%s), want 0",
			got, reply.GetAgentReply().GetDetails())
	}
	assertOrder(t, node,
		"cmd dmsetup resume "+stbLin,
		"writedirect "+agent.NvmetRoot+"/subsystems/"+stbNqn+
			"/namespaces/1/enable=0",
		"cmd dmsetup remove "+stbLin,
	)
	if dmPresent(node, stbLin) {
		t.Error("a suspended dm-linear survived the sweep")
	}
	if subsysPresent(node, stbNqn) {
		t.Error("the dropped standby's export survived the sweep")
	}
	// The side itself is untouched: the fence is still on for the CN that
	// is still in the list.
	primary := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	node.mu.Lock()
	primarySuspended := node.dms[primary].suspended
	node.mu.Unlock()
	if !primarySuspended {
		t.Error("the sweep ended the cutover window of a wanted linear")
	}
}

// ---------------------------------------------------------------------------
// The read-only verdicts (Check* and Get*Info)
// ---------------------------------------------------------------------------

// TestCheckSideReportsLeftover pins the side-level verdict: the same
// enumeration and the same comparison the sweep makes, with nothing touched.
//
// It is the whole retry machinery (the recomputed verdict of architecture.md,
// Teardown by sweep; `dnv-worker.md` RW4). The agent stores no "pending" flag
// — a stored flag is memory of failure, and memory of failure is what let the
// old teardowns forget an object that would not go — so what drives the
// worker's next SyncupSide is a code recomputed from the node on every round.
// A verdict that mutated would be worse than useless: Check runs under the
// node READ lock and against objects another converge may own (DN16/SH25).
func TestCheckSideReportsLeftover(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	stbLin := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1)

	// The standby leaves the list, but its dm-linear will not go.
	setFailAlways(node, "dmsetup remove "+stbLin, "device-mapper: busy")
	dropped := sideReq(2, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)
	if _, err := srv.SyncupSide(ctx, dropped); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !dmPresent(node, stbLin) {
		t.Fatal("the fake removed the device the test told it to refuse")
	}

	node.Reset()
	reply, info := srv.checkSideRound(ctx, &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn,
		SidePointer: sidePtr(testSide), ShowInfo: true,
	}, nil)
	if got := reply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeLeftover {
		t.Errorf("CheckSide code = %d (%s), want %d", got,
			reply.GetAgentReply().GetDetails(), common.ReplyCodeLeftover)
	}
	if !strings.Contains(reply.GetAgentReply().GetDetails(), stbLin) {
		t.Errorf("details = %q, want the leftover named",
			reply.GetAgentReply().GetDetails())
	}
	// The info still rides along: a leftover is an accepted request with
	// residue, not a refusal, and the worker evaluates the rows either way.
	if info == nil || reply.GetSideInfo().GetSideDevInfo() == nil {
		t.Error("a leftover verdict dropped the SideInfo")
	}
	getReply, err := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	})
	if err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	if got := getReply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeLeftover {
		t.Errorf("GetSideInfo code = %d, want %d",
			got, common.ReplyCodeLeftover)
	}
	if getReply.GetSideInfo().GetSideDevInfo() == nil {
		t.Error("a leftover verdict dropped GetSideInfo's SideInfo")
	}
	if got := node.Mutations(); len(got) != 0 {
		t.Errorf("a verdict mutated the node:\n%s", strings.Join(got, "\n"))
	}

	// Clean once the object is gone, with the info still carried.
	clearFailAlways(node, "dmsetup remove "+stbLin)
	if _, err := srv.SyncupSide(ctx, sideReq(3, testSide, testCn0, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.Reset()
	reply, _ = srv.checkSideRound(ctx, &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn,
		SidePointer: sidePtr(testSide), ShowInfo: true,
	}, nil)
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("clean CheckSide code = %d (%s), want 0",
			got, reply.GetAgentReply().GetDetails())
	}
	if reply.GetSideInfo().GetSideDevInfo() == nil {
		t.Error("a clean verdict dropped the SideInfo")
	}
	if got := node.Mutations(); len(got) != 0 {
		t.Errorf("a clean verdict mutated the node:\n%s",
			strings.Join(got, "\n"))
	}
}

// TestCheckDnReportsLeftover is the node-level twin. Its scope is what makes
// it worth having separately: a dn's verdict covers the objects of sides that
// are in NO pointer list, which is precisely the set that has no *Info row to
// report through — the rows are keyed by the ids of wanted objects, and these
// objects are wanted by nothing. Without the reply code the worker would
// never learn they exist.
func TestCheckDnReportsLeftover(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	objs := objectsOf(sidePtr(testSide))

	setFailAlways(node, "dmsetup remove "+objs.sideDev, "device-mapper: busy")
	if _, err := srv.SyncupDn(ctx, dnReq(2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if !dmPresent(node, objs.sideDev) {
		t.Fatal("the fake removed the device the test told it to refuse")
	}

	node.Reset()
	reply, info := srv.checkDnRound(ctx, &pb.CheckDnRequest{
		ClusterId: testCluster, DnId: testDn, ShowInfo: true,
	}, nil)
	if got := reply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeLeftover {
		t.Errorf("CheckDn code = %d (%s), want %d", got,
			reply.GetAgentReply().GetDetails(), common.ReplyCodeLeftover)
	}
	if !strings.Contains(reply.GetAgentReply().GetDetails(), objs.sideDev) {
		t.Errorf("details = %q, want the leftover named",
			reply.GetAgentReply().GetDetails())
	}
	if info == nil || reply.GetDnInfo().GetMetaInfo() == nil {
		t.Error("a leftover verdict dropped the DnInfo")
	}
	getReply, err := srv.GetDnInfo(ctx, &pb.GetDnInfoRequest{
		ClusterId: testCluster, DnId: testDn,
	})
	if err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	if got := getReply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeLeftover {
		t.Errorf("GetDnInfo code = %d, want %d",
			got, common.ReplyCodeLeftover)
	}
	if getReply.GetDnInfo().GetMetaInfo() == nil {
		t.Error("a leftover verdict dropped GetDnInfo's DnInfo")
	}
	if got := node.Mutations(); len(got) != 0 {
		t.Errorf("a verdict mutated the node:\n%s", strings.Join(got, "\n"))
	}

	clearFailAlways(node, "dmsetup remove "+objs.sideDev)
	if _, err := srv.SyncupDn(ctx, dnReq(3)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	node.Reset()
	reply, _ = srv.checkDnRound(ctx, &pb.CheckDnRequest{
		ClusterId: testCluster, DnId: testDn, ShowInfo: true,
	}, nil)
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("clean CheckDn code = %d (%s), want 0",
			got, reply.GetAgentReply().GetDetails())
	}
	if reply.GetDnInfo().GetMetaInfo() == nil {
		t.Error("a clean verdict dropped the DnInfo")
	}
	if got := node.Mutations(); len(got) != 0 {
		t.Errorf("a clean verdict mutated the node:\n%s",
			strings.Join(got, "\n"))
	}
}

// ---------------------------------------------------------------------------
// The clone-metadata record and the proof it waits for
// ---------------------------------------------------------------------------

// TestCloneMetaRecordFreedUnderTheProof pins the second half of the
// probe-verified "gone" of architecture.md, Teardown by sweep, the one a device
// probe alone cannot supply: a clone-metadata slot is released
// only when no held side claims its migration AND every side of that sp
// this node may host is a side whose local state the agent actually holds.
//
// The second condition is the one that looks redundant and is not. A side
// named by its DN's pointer list but not yet synced (DN8, or a restart that
// has not received its SyncupSide) may be the destination that owns the slot:
// its dm-clone hydration resumes from that metadata after a crash
// (architecture.md, Migration), so
// freeing the slot on the strength of "nobody I know of claims it" would hand
// a live migration's metadata to the next one and strand the transfer. The
// slot simply waits for a pass that can prove it — which costs nothing,
// because the record sweep runs on every node-level pass.
func TestCloneMetaRecordFreedUnderTheProof(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	dst := sweepSidePtr(testLeg, testSide)
	other := sweepSidePtr(testLeg+1, testSide2)
	metaName := nf.DnMigrMetaDmName(testCluster, testDn, testSp, testMigrId)

	t.Run("freed when the sp is fully known", func(t *testing.T) {
		srv, node := newTestServer(t)
		ctx := context.Background()
		if _, err := srv.SyncupDn(ctx, sweepDnReq(1, dst)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		syncupSideTwoPhase(t, srv, sweepDstReq(1, dst, testMigrId))
		if _, ok, _ := srv.meta.LookupCloneMeta(
			ctx, testSp, testMigrId); !ok {
			t.Fatal("the destination reserved no clone-metadata slot")
		}

		// The finish: migr_dst_conf is gone, and this agent holds the state
		// of every side of the sp its DN names.
		if _, err := srv.SyncupSide(ctx, sweepSideReq(2, dst)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if dmPresent(node, metaName) {
			t.Error("the clone-metadata wrapper survived the finish")
		}
		if _, ok, _ := srv.meta.LookupCloneMeta(ctx, testSp, testMigrId); ok {
			t.Error("the slot was not released although nothing claims it " +
				"and every side of the sp is known")
		}
	})

	t.Run("held while a side is known only by pointer", func(t *testing.T) {
		srv, node := newTestServer(t)
		ctx := context.Background()
		// Two sides on the sp; only the destination is ever synced, so the
		// other exists for this agent as a pointer and nothing else.
		if _, err := srv.SyncupDn(ctx, sweepDnReq(1, dst, other)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		syncupSideTwoPhase(t, srv, sweepDstReq(1, dst, testMigrId))

		reply, err := srv.SyncupSide(ctx, sweepSideReq(2, dst))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		// The wrapper waits with its record. The side known by its pointer
		// alone may be the one whose claim a lost store took, so this pass
		// judges no migration device of the sp either (DN6).
		if !dmPresent(node, metaName) {
			t.Error("the clone-metadata wrapper went while a side of the sp " +
				"was known only by pointer")
		}
		if _, ok, _ := srv.meta.LookupCloneMeta(
			ctx, testSp, testMigrId); !ok {
			t.Fatal("the slot was released while a side of the sp could " +
				"still own it: an in-flight migration's hydration metadata")
		}
		// An object waiting for a proof is not a leftover: nothing is stuck,
		// and a pass that has the proof takes it.
		if got := reply.GetAgentReply().GetCode(); got != 0 {
			t.Errorf("code = %d (%s), want 0",
				got, reply.GetAgentReply().GetDetails())
		}

		// The missing side arrives; now the proof exists. That side's own
		// pass takes the wrapper, and the slot is released.
		syncupSideTwoPhase(t, srv, sweepSideReq(1, other))
		if dmPresent(node, metaName) {
			t.Error("the clone-metadata wrapper survived once the sp was " +
				"fully known")
		}
		if _, err := srv.SyncupDn(ctx, sweepDnReq(2, dst, other)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		if _, ok, _ := srv.meta.LookupCloneMeta(ctx, testSp, testMigrId); ok {
			t.Error("the slot was never released once the sp was fully known")
		}
	})
}

// ---------------------------------------------------------------------------
// An enumeration that did not answer licenses nothing
// ---------------------------------------------------------------------------

// TestUnansweredEnumerationRemovesNothingDn is the cn test's twin, and the
// stakes here are higher than a leak. Every removal is "actual minus
// desired"; when `dmsetup ls` does not answer, `actual` is empty for want of
// an answer, and an empty `actual` reads as "there is nothing left" wherever
// a removal is gated on ABSENCE — the layer stop rule never fires, so lower
// layers run as though the ones above them had succeeded. On a dn the lowest
// layer of all frees an on-disk ALLOCATION RECORD, and handing a live side's
// extents to the next side is corruption, not a leak.
//
// THE FIXTURE KEEPS ONE SIDE, and that is what makes the test mean anything.
// The node-level export sweep is gated on `hosted` — the sps this node holds
// evidence for — and `hosted` is built from the dm enumeration UNION the
// authoritative pointer lists. Drop every pointer and `hosted` is empty from
// the pointer half alone, so the export loop is skipped whatever the dm
// listing did and the pass is trivially harmless. Keeping a sibling side on
// the same sp puts that sp in `hosted` through the half the failure cannot
// touch, so the dropped side's `:2:` export really does reach attribution —
// where, with `actual` empty, only the gate stands between it and removal.
func TestUnansweredEnumerationRemovesNothingDn(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	kept := sweepSidePtr(testLeg, testSide)
	gone := sweepSidePtr(testLeg+1, testSide2)
	if _, err := srv.SyncupDn(ctx, sweepDnReq(1, kept, gone)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	syncupSideTwoPhase(t, srv, sweepSideReq(1, kept))
	syncupSideTwoPhase(t, srv, sweepSideReq(1, gone))
	keptObjs, goneObjs := objectsOf(kept), objectsOf(gone)
	keptObjs.assertAllPresent(t, node)
	goneObjs.assertAllPresent(t, node)

	node.Reset()
	setHook(node, node.killCmdAlways, "dmsetup ls")
	reply, err := srv.SyncupDn(ctx, sweepDnReq(2, kept))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	clearHook(node, node.killCmdAlways, "dmsetup ls")

	if got := reply.GetAgentReply().GetCode(); got != common.ReplyCodeLeftover {
		t.Errorf("code = %d, want ReplyCodeLeftover (%d): an unanswered "+
			"enumeration must re-drive, not report success",
			got, common.ReplyCodeLeftover)
	}
	// Every object of BOTH sides is still there. The dropped side's are the
	// ones a working pass would have taken, and this pass had no evidence
	// for taking any of them.
	goneObjs.assertAllKept(t, node)
	keptObjs.assertAllKept(t, node)
	for _, call := range []string{
		"cmd dmsetup remove ", "cmd nvme disconnect ",
		"cmd rmdir " + agent.NvmetRoot + "/subsystems/",
	} {
		if node.hasCall(call) {
			t.Errorf("the pass issued %q with no enumeration to justify it",
				call)
		}
	}
	// The allocation record is the one that matters: freeing it hands live
	// extents to the next side.
	if node.hasCall("writeblock") {
		t.Error("the pass wrote the volume table with no enumeration to " +
			"justify it")
	}
}

// ---------------------------------------------------------------------------
// A claim on a kernel object carries its claimant's gate
// ---------------------------------------------------------------------------

// TestMigrationSourceSweptByLevel pins the rule a claim map is easy to get
// wrong: a claim on a kernel object is not "this object exists", it is
// "somebody still WANTS it", so it has to carry that somebody's gate.
//
// The migration SOURCE is the one role whose two objects are wanted at
// different levels — the `d2` linear under wantDm, its `:3:` export under
// wantExport — and `SP_LEVEL_NO_SIDE` sits exactly between them. A single
// ungated claim (which is what this started as) made both objects permanently
// unsweepable: `sideWanted` correctly dropped them from the wanted set, but
// every sweep skipped anything the claim map held, so they were neither
// removed NOR reported. The reply stayed code 0, so the worker never
// re-drove, and an sp taken to NO_SIDE — the level whose whole point is that
// it is off the network — went on exporting its migration source until
// `migr_src_conf` itself was dropped.
//
// Each level is asserted for BOTH objects, because the gate that was missing
// is precisely the one that tells them apart.
func TestMigrationSourceSweptByLevel(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	srcDm := nf.DnMigrSrcName(testCluster, testDn, testSp, testMigrId)
	srcNqn := nf.MigrSrcNqn(testCluster, testDn, testSp, testMigrId)

	cases := []struct {
		name    string
		level   pb.SpLevel
		wantDm  bool
		wantNqn bool
	}{
		{"READWRITE keeps both", pb.SpLevel_SP_LEVEL_READWRITE, true, true},
		{"NO_SIDE keeps the linear, drops the export",
			pb.SpLevel_SP_LEVEL_NO_SIDE, true, false},
		{"DISABLE drops both", pb.SpLevel_SP_LEVEL_DISABLE, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if !dmPresent(node, srcDm) || !subsysPresent(node, srcNqn) {
				t.Fatalf("fixture is wrong: the source role was never built")
			}

			node.Reset()
			req := migrSrcReq(3)
			req.SideConf.SpLevel = tc.level
			reply, err := srv.SyncupSide(ctx, req)
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if got := reply.GetAgentReply().GetCode(); got != 0 {
				t.Errorf("code = %d (%s), want 0: the sweep had to finish",
					got, reply.GetAgentReply().GetDetails())
			}
			if got := dmPresent(node, srcDm); got != tc.wantDm {
				t.Errorf("%s present = %v, want %v at %s",
					srcDm, got, tc.wantDm, tc.level)
			}
			if got := subsysPresent(node, srcNqn); got != tc.wantNqn {
				t.Errorf("%s present = %v, want %v at %s",
					srcNqn, got, tc.wantNqn, tc.level)
			}
		})
	}
}
