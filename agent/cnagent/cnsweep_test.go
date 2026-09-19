package cnagent

import (
	"context"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Teardown by sweep (§4.1)
//
// Every test in this file pins one property of the same rule: what the agent
// REMOVES is derived by enumerating the node and subtracting the desired
// state, and nothing about a past failure is ever remembered. The old retire
// phase derived removals from "the plan I applied last time minus the plan I
// am applying now" and then overwrote the applied plan whether or not the
// removals worked, so a removal that failed was forgotten together with the
// plan that named it — which is how one killed `mdadm --detail` leaked an
// array and its two leg wrappers for ever.
//
// The tests are therefore written against OBJECTS and REPLY CODES, not
// against the shape of any plan: a sweep that only works while its cntlr's
// plan is still in memory would pass a trace assertion and fail every one of
// these.
//
// The CN14 thin-id activation sweep, which is a different mechanism that
// happens to share the word, lives in sweep_test.go.
// ---------------------------------------------------------------------------

// cnSweepSyncup drives one SyncupCn and returns the reply whatever its code
// is. It is deliberately not cnSyncup, which fails the test on a non-zero
// code: a leftover reply IS the expected outcome of half these cases.
func cnSweepSyncup(
	t *testing.T,
	srv *CnAgentServer,
	revision uint64,
	withCntlr bool,
) *pb.SyncupCnReply {
	t.Helper()
	reply, err := srv.SyncupCn(context.Background(),
		cnReq(revision, withCntlr))
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	return reply
}

// cnSweepAssertCode reports the code AND the details, because a leftover reply
// is only useful to an operator if it names what is left.
func cnSweepAssertCode(
	t *testing.T,
	reply *pb.AgentReply,
	want uint32,
	label string,
) {
	t.Helper()
	if reply.GetCode() != want {
		t.Fatalf("%s: code %d (%q), want %d",
			label, reply.GetCode(), reply.GetDetails(), want)
	}
}

// cnSweepAssertDetails is the other half of a leftover reply: the worker logs
// these details and an operator reads them, so a code-4 reply that names
// nothing is a regression of its own.
func cnSweepAssertDetails(
	t *testing.T,
	reply *pb.AgentReply,
	want string,
	label string,
) {
	t.Helper()
	if !strings.Contains(reply.GetDetails(), want) {
		t.Fatalf("%s: details %q do not name %q",
			label, reply.GetDetails(), want)
	}
}

// cnSweepForget drops the cntlr's state file and its memory entry the way a
// pointer removal does (`architecture.md` §9.8, "State is dropped at pointer
// removal"), WITHOUT touching the node. What is left is
// the situation the sweep has to cope with: resources on the node and no
// plan, no request and no state anywhere that names them.
func cnSweepForget(t *testing.T, srv *CnAgentServer, node *fakeNode) {
	t.Helper()
	key := cntlrKey(testCluster, testCn, testSp, testCntlr)
	if srv.getCntlr(key) == nil {
		t.Fatalf("the fixture cntlr is not in memory")
	}
	srv.dropCntlr(key)
	path := srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr)
	if _, ok := node.protos[path]; !ok {
		t.Fatalf("the fixture cntlr has no state file at %s", path)
	}
	delete(node.protos, path)
}

// cnSweepMdNode is the /dev/mdN a sweep sees for one group's array. It must be
// read BEFORE the pass that stops the array: a stopped array has no sysfs
// node left to name, and the sweep never uses the /dev/md/<name> spelling
// because that one depends on udev having run.
func cnSweepMdNode(
	t *testing.T,
	srv *CnAgentServer,
	node *fakeNode,
	grpIdx uint32,
	isMeta bool,
) string {
	t.Helper()
	dev := node.mdNode(srv.nf.MdPath(srv.nf.CnMdDevName(
		testCluster, testCn, testSp, 0, grpIdx, isMeta)))
	if dev == "" {
		t.Fatalf("group %d (meta=%v) has no sysfs array node", grpIdx, isMeta)
	}
	return dev
}

// cnSweepRemovals are the calls that change something on the node in a way
// only a teardown does. "Nothing was removed" is asserted over all of them at
// once, because a sweep that skipped `dmsetup remove` but still disconnected
// a leg would be just as wrong as one that removed everything.
var cnSweepRemovals = []string{
	"cmd dmsetup remove ", "cmd dmsetup reload ", "cmd dmsetup message ",
	"cmd mdadm --stop ", "cmd nvme disconnect ", "cmd rmdir ",
	"cmd rm -f ",
}

// cnSweepFirstRemoval is the index of the first such call, or -1.
func cnSweepFirstRemoval(node *fakeNode) (int, string) {
	for i, call := range node.Calls() {
		for _, fragment := range cnSweepRemovals {
			if strings.HasPrefix(call, fragment) {
				return i, call
			}
		}
	}
	return -1, ""
}

// cnSweepAssertNoRemoval is the negative form, with the whole call log in the
// failure so a regression is diagnosable from one run.
func cnSweepAssertNoRemoval(t *testing.T, node *fakeNode) {
	t.Helper()
	if idx, call := cnSweepFirstRemoval(node); idx >= 0 {
		t.Fatalf("a removal was attempted: %q\ncalls:\n%s",
			call, strings.Join(node.Calls(), "\n"))
	}
}

// cnSweepNoDmLeft is the assertion every whole-sp teardown ends with: the
// fake node holds no dm device at all. The base state (tmpfs, arena file,
// loop device) is not device-mapper and outlives every cntlr.
func cnSweepNoDmLeft(t *testing.T, node *fakeNode) {
	t.Helper()
	for name := range node.dms {
		t.Fatalf("dm device %s survived the sweep", name)
	}
}

// subsysDirPresent reads the nvmet configfs tree the way an operator would:
// the subsystem directory either exists or it does not.
func subsysDirPresent(node *fakeNode, nqn string) bool {
	return node.dirs[agent.NvmetRoot+"/subsystems/"+nqn]
}

// TestRemovedCntlrSweptByName is the design's headline: removal is derived
// from the live system, not from a plan.
//
// The cntlr's state file and its memory entry are dropped BEFORE the pass, so
// there is provably no plan, no request and no bookkeeping left that names a
// single one of its objects — exactly the state §9.8's drop-at-pointer-removal
// leaves behind when a pointer disappears, and exactly the state an agent restarted in the middle
// of a teardown wakes up in. Everything of that sp must still go, by name,
// and the reply must be a plain OK: an agent that could only remove what it
// still remembered would leak the whole stack here and say nothing.
func TestRemovedCntlrSweptByName(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	cnSweepForget(t, srv, node)

	node.Reset()
	reply := cnSweepSyncup(t, srv, 3, false)
	cnSweepAssertCode(t, reply.GetAgentReply(), 0, "SyncupCn")

	cnSweepNoDmLeft(t, node)
	if subsysDirPresent(node, testNqn) {
		t.Fatalf("the host-facing subsystem survived the sweep")
	}
	if len(node.subsystems) != 0 {
		t.Fatalf("nvme connections survived the sweep: %v", node.subsystems)
	}
	// The sweep found the objects through the node's own enumerations, which
	// is what makes the previous three assertions mean what they say.
	assertOrder(t, node, "cmd dmsetup ls", "cmd dmsetup remove ")
}

// TestRemovedCntlrLeftoverReported is the D8 rule and the D3 verdict in one
// pass: a layer that leaves something behind stops the descent, and the pass
// says so in its reply.
//
// The data group's `mdadm --stop` is killed BEFORE it touched anything, so
// the array is still assembled and still pins its two leg wrappers. Two
// things must follow. The chain must stop at that layer — disconnecting a leg
// out from under a live array is destructive on the migration path, where the
// array's superblocks are what the next CN assembles — and the reply must
// carry ReplyCodeLeftover naming the array, because that reply is the whole
// retry machinery: the worker re-issues the Syncup while the code is
// non-zero, and nothing on the agent side remembers that the removal failed.
//
// The second pass runs at the SAME revision, which is what a worker re-sync
// looks like, and must finish the job.
func TestRemovedCntlrLeftoverReported(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, raid1: true, twoLegs: true})
	dataDev := cnSweepMdNode(t, srv, node, 0, false)
	metaDev := cnSweepMdNode(t, srv, node, 0, true)

	node.Reset()
	// Killed with no effect: the signal arrived before mdadm did anything.
	node.killCmdNoEffect["mdadm --stop "+dataDev] = true
	reply := cnSweepSyncup(t, srv, 3, false)
	cnSweepAssertCode(t, reply.GetAgentReply(),
		common.ReplyCodeLeftover, "SyncupCn with a stuck array")
	cnSweepAssertDetails(t, reply.GetAgentReply(), dataDev, "leftover details")

	if node.arrayGone(dataDev) {
		t.Fatalf("%s is gone: the kill did not model a no-op", dataDev)
	}
	if !node.arrayGone(metaDev) {
		t.Fatalf("%s survived although its own stop was never killed", metaDev)
	}
	// D8: every wrapper of the stuck layer's chain is still there, and no leg
	// was disconnected. The meta leg's wrapper is in the assertion too — the
	// descent stops for the whole sp, not only for the array that failed.
	for _, legId := range []uint64{testDataLeg, testDataLeg2, testMetaLeg} {
		if _, ok := node.dms[legName(srv, legId)]; !ok {
			t.Fatalf("leg %#x's wrapper was removed under a live array", legId)
		}
	}
	assertNoCall(t, node, "cmd nvme disconnect")

	// The re-sync at the same revision. Nothing was remembered, so this pass
	// re-enumerates and finds the same array — which is the point: the first
	// pass stored no "pending" flag anywhere for it to consult.
	node.Reset()
	reply = cnSweepSyncup(t, srv, 3, false)
	cnSweepAssertCode(t, reply.GetAgentReply(), 0, "the re-sync")
	if !node.arrayGone(dataDev) {
		t.Fatalf("the re-sync left %s assembled", dataDev)
	}
	cnSweepNoDmLeft(t, node)
	if len(node.subsystems) != 0 {
		t.Fatalf("nvme connections survived the re-sync: %v", node.subsystems)
	}
}

// TestRemovedCntlrKilledButCompleted is the other half of D6, and the half
// that is easy to get wrong: the same killed command, with the opposite truth
// underneath.
//
// `mdadm --stop` was killed at the soft timeout AFTER the kernel had already
// stopped the array. The exit status is identical to the previous test's —
// -1 with "signal: killed" — and it says nothing at all. Only the sysfs probe
// knows, and it says the array is gone, so the pass must descend to the legs
// and reply OK. An agent that trusted the exit status would report a leftover
// here for ever, re-driving the worker every round over an array that does
// not exist.
func TestRemovedCntlrKilledButCompleted(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, raid1: true, twoLegs: true})
	dataDev := cnSweepMdNode(t, srv, node, 0, false)

	node.Reset()
	// Killed, but dispatched first: the ioctl completed in the kernel.
	node.killCmd["mdadm --stop "+dataDev] = true
	reply := cnSweepSyncup(t, srv, 3, false)
	cnSweepAssertCode(t, reply.GetAgentReply(), 0, "SyncupCn")

	if !node.hasCall("cmd mdadm --stop " + dataDev) {
		t.Fatalf("the array was never stopped:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	if !node.arrayGone(dataDev) {
		t.Fatalf("%s survived a stop that reached the kernel", dataDev)
	}
	// The descent continued past L9: the legs are disconnected and unwrapped.
	cnSweepNoDmLeft(t, node)
	if len(node.subsystems) != 0 {
		t.Fatalf("nvme connections survived the sweep: %v", node.subsystems)
	}
}

// TestPersistBeforeSweep pins §9.8's persist-first ordering: the cn request — the pointer
// list the sweep removes against — is on disk before the first removal.
//
// The sweep can block for the whole failfast window on a dead leg, so an RPC
// cancelled in the middle of one is ordinary. With the save at the end (where
// SH5 puts every other request) that cancellation lost the new list, and the
// next Reconcile rebuilt the cntlr from the OLD one against sides that no
// longer exist. With the save first, a crash anywhere in the pass is nothing
// but a startup sweep.
func TestPersistBeforeSweep(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	node.Reset()
	reply := cnSweepSyncup(t, srv, 3, false)
	cnSweepAssertCode(t, reply.GetAgentReply(), 0, "SyncupCn")

	saved := node.indexOfCall("writeproto " +
		srv.nf.LocalCnPath(testCluster, testCn))
	if saved < 0 {
		t.Fatalf("the cn request was never persisted:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	removed, call := cnSweepFirstRemoval(node)
	if removed < 0 {
		t.Fatalf("the pass removed nothing, so the order is vacuous:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	if saved > removed {
		t.Fatalf("the cn file was written at %d, after the first removal "+
			"%q at %d\ncalls:\n%s",
			saved, call, removed, strings.Join(node.Calls(), "\n"))
	}
}

// cnSweepStrandedSp is an sp this CN's pointer list does not name. To the
// agent that is the whole definition of "a removed sp": the pointer list is
// the only record of what this node is supposed to hold, so an object naming
// any other sp is residue, whether it was left by a teardown that failed, by
// a crash, or by an agent that was down when the sp was deleted.
const cnSweepStrandedSp = uint64(0x9a1)

// seedStrayDm installs a dm device of the stranded sp, the way a teardown
// that never finished would have left it. The table is a plain error target:
// a sweep decides by NAME and never by what a device maps.
func seedStrayDm(node *fakeNode, name string, devNo string) {
	node.dms[name] = &fakeDm{
		table:   "0 8 error",
		thinIds: map[uint32]bool{},
	}
	node.devNo["/dev/mapper/"+name] = devNo
}

// TestCheckCnReportsLeftover pins `architecture.md` §9.8's "The verdict is
// recomputed every time and stored nowhere".
//
// That is what makes the leftover code a working retry: the worker re-issues
// SyncupCn while a Check replies non-zero (RW4 step 5), and it stops as soon
// as the node is clean — including when the object went away for a reason
// this agent knows nothing about, such as an operator's own `dmsetup remove`
// or a co-hosted tool. A stored "pending" flag would be memory of failure,
// and memory of failure is exactly what the design does without: it would
// keep re-driving the worker over an object that no longer exists, and would
// need a second mechanism to ever be cleared.
//
// The reply must still carry the CnInfo. The leftover is not a rejection —
// the request was applied — so the rows the worker evaluates for health,
// pushes and td completion have to keep flowing while residue is being
// chased.
func TestCheckCnReportsLeftover(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	stray := srv.nf.CnNsDevName(testCluster, testCn, cnSweepStrandedSp, testNs)
	seedStrayDm(node, stray, "253:200")

	ctx := context.Background()
	node.Reset()
	checkReq := &pb.CheckCnRequest{
		ClusterId: testCluster, CnId: testCn, Revision: 2}
	reply, _ := srv.checkCnRound(ctx, checkReq, nil)
	cnSweepAssertCode(t, reply.GetAgentReply(),
		common.ReplyCodeLeftover, "CheckCn with residue")
	cnSweepAssertDetails(t, reply.GetAgentReply(), stray, "CheckCn details")
	if reply.GetCnInfo() == nil {
		t.Fatalf("a leftover reply dropped the CnInfo")
	}

	getReply, err := srv.GetCnInfo(ctx,
		&pb.GetCnInfoRequest{ClusterId: testCluster, CnId: testCn})
	if err != nil {
		t.Fatalf("GetCnInfo: %v", err)
	}
	cnSweepAssertCode(t, getReply.GetAgentReply(),
		common.ReplyCodeLeftover, "GetCnInfo with residue")
	if getReply.GetCnInfo() == nil {
		t.Fatalf("a leftover GetCnInfo reply dropped the CnInfo")
	}

	// CN23: the verdict is the sweep's enumeration with nothing touched.
	for _, call := range node.Mutations() {
		t.Fatalf("a read-only verdict mutated: %q", call)
	}

	// The object goes behind the agent's back — no Syncup, no RPC of any
	// kind between the two rounds — and the next verdict is recomputed from
	// the node, not replayed from the last one.
	delete(node.dms, stray)
	delete(node.devNo, "/dev/mapper/"+stray)
	reply, _ = srv.checkCnRound(ctx, checkReq, nil)
	cnSweepAssertCode(t, reply.GetAgentReply(), 0, "CheckCn once clean")
}

// TestReconcileSweepsAtStartup pins the startup half of the same rule. A
// crash between a pointer removal and its sweep is ordinary — the sweep is
// where the pass can block for a whole failfast window — so the state an
// agent finds on disk routinely names fewer cntlrs than the node holds
// resources for.
//
// Two shapes have to work. With no cntlr file at all the resources can only
// be found by name, which is the startup form of the design's headline. With
// a cntlr file whose pointer is gone, the file is DROPPED rather than
// converged: converging it would rebuild the very stack the sweep is about to
// remove, against sides the control plane has already let go.
func TestReconcileSweepsAtStartup(t *testing.T) {
	cnPath := func(srv *CnAgentServer) string {
		return srv.nf.LocalCnPath(testCluster, testCn)
	}
	cntlrPath := func(srv *CnAgentServer) string {
		return srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr)
	}

	t.Run("no cntlr file", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
		// The list loses the pointer and the cntlr file is already gone:
		// nothing on disk names a single object of this sp.
		if err := node.writeProto(context.Background(), cnPath(srv),
			cnReq(3, false)); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		delete(node.protos, cntlrPath(srv))

		node.Reset()
		fresh := newCnServer(node)
		reconcileForTest(t, fresh)
		cnSweepNoDmLeft(t, node)
		if subsysDirPresent(node, testNqn) {
			t.Fatalf("the host-facing subsystem survived the startup sweep")
		}
	})

	t.Run("orphan cntlr file", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
		if err := node.writeProto(context.Background(), cnPath(srv),
			cnReq(3, false)); err != nil {
			t.Fatalf("rewrite: %v", err)
		}

		node.Reset()
		fresh := newCnServer(node)
		reconcileForTest(t, fresh)
		if _, ok := node.protos[cntlrPath(srv)]; ok {
			t.Fatalf("the orphan cntlr file was kept")
		}
		if st := fresh.getCntlr(cntlrKey(
			testCluster, testCn, testSp, testCntlr)); st != nil {
			t.Fatalf("the orphan cntlr stayed in memory")
		}
		// Dropped WITHOUT a converge. This is asserted before the device
		// check below, because a converge that rebuilt what the sweep had
		// just removed leaves a node that looks merely half-swept and the
		// reason would be lost.
		for _, forbidden := range []string{
			"cmd dmsetup create ", "cmd nvme connect ", "cmd mdadm --create",
			"cmd mdadm --assemble",
		} {
			assertNoCall(t, node, forbidden)
		}
		cnSweepNoDmLeft(t, node)
	})
}

// TestDisableLevelSweepsEverything is CN19's strongest form: at
// SP_LEVEL_DISABLE the wanted set is EMPTY, so the sweep removes the sp's
// whole stack — and the desired state stays, on disk and in memory, because
// the operator turned the sp off rather than deleting it. Lowering the level
// again must find its request where it left it.
//
// The sweep is what makes this one rule instead of two: nothing about
// DISABLE is special-cased any more, it is simply a plan that wants nothing.
func TestDisableLevelSweepsEverything(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	node.Reset()
	syncupCntlrAt(t, srv, reqOpts{
		revision: 3, primary: true, level: pb.SpLevel_SP_LEVEL_DISABLE})

	cnSweepNoDmLeft(t, node)
	if subsysDirPresent(node, testNqn) {
		t.Fatalf("the host-facing subsystem survived SP_LEVEL_DISABLE")
	}
	if len(node.subsystems) != 0 {
		t.Fatalf("nvme connections survived SP_LEVEL_DISABLE: %v",
			node.subsystems)
	}

	path := srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr)
	if _, ok := node.protos[path]; !ok {
		t.Fatalf("SP_LEVEL_DISABLE deleted the cntlr state file")
	}
	st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	if st == nil {
		t.Fatalf("SP_LEVEL_DISABLE dropped the in-memory desired state")
	}
	if st.req.GetSpLevel() != pb.SpLevel_SP_LEVEL_DISABLE {
		t.Fatalf("the stored level is %v", st.req.GetSpLevel())
	}
}

// TestStandbyKeepsOnlyStandbyObjects pins the failover half of §11.1: the
// sweep is the whole implementation of "the desired set shrank to the standby
// shape", with no retire step naming any object by hand.
//
// What a standby must NOT keep is everything that writes: the md arrays whose
// superblocks only one CN may own, the thin-pool whose metadata lives on the
// DN legs, the thin volumes and the raid0 over them. What it must keep is the
// whole host-facing skin — the leg wrappers, the per-td dm-error, the ns-devs
// and the nvmet objects — because a standby is a CN that a host may already
// be connected to and that must be promotable without rebuilding it.
func TestStandbyKeepsOnlyStandbyObjects(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, raid1: true})
	metaDev := cnSweepMdNode(t, srv, node, 0, true)
	dataDev := cnSweepMdNode(t, srv, node, 0, false)

	node.Reset()
	syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: false, raid1: true})

	for _, dev := range []string{metaDev, dataDev} {
		if !node.arrayGone(dev) {
			t.Fatalf("array %s survived the demotion", dev)
		}
	}
	for _, name := range []string{
		poolName(srv),
		names(srv).CnPoolMetaName(testCluster, testCn, testSp, testSlice),
		names(srv).CnPoolDataName(testCluster, testCn, testSp, testSlice),
		thinName(srv, testTd),
		raid0Name(srv, testTd),
	} {
		if _, ok := node.dms[name]; ok {
			t.Fatalf("dm device %s survived the demotion", name)
		}
	}
	for _, name := range []string{
		legName(srv, testMetaLeg), legName(srv, testDataLeg),
		errorName(srv, testTd), nsDevName(srv, testNs),
	} {
		if _, ok := node.dms[name]; !ok {
			t.Fatalf("dm device %s was removed from a standby", name)
		}
	}
	if !subsysDirPresent(node, testNqn) {
		t.Fatalf("the host-facing subsystem was removed from a standby")
	}
	if !node.dirs[agent.NvmetRoot+"/subsystems/"+testNqn+"/namespaces/1"] {
		t.Fatalf("the nvmet namespace was removed from a standby")
	}
	if len(node.subsystems) == 0 {
		t.Fatalf("the standby's leg connections were dropped")
	}
	// Every one of those is KEPT, not removed and rebuilt: the build phase
	// runs immediately after the sweep in the same converge, so only the
	// absence of the removal itself says the host never lost its namespace.
	for _, forbidden := range []string{
		"cmd dmsetup remove " + legName(srv, testMetaLeg),
		"cmd dmsetup remove " + legName(srv, testDataLeg),
		"cmd dmsetup remove " + errorName(srv, testTd),
		"cmd dmsetup remove " + nsDevName(srv, testNs),
		"cmd rmdir " + agent.NvmetRoot + "/subsystems/" + testNqn,
		"cmd nvme disconnect",
	} {
		assertNoCall(t, node, forbidden)
	}
	// §11.6: the ns-dev it keeps is live and serving errors, not suspended
	// and not still mapping the raid0 that has just gone.
	assertParked(t, srv, node, testNs, testTd, "demoted ns-dev")
}

// TestRetireByEnumeration is the case the old incremental retire could never
// repair. It computed what to remove as "the plan I applied last time minus
// the plan I am applying now" and then overwrote the applied plan, so a group
// whose `mdadm --stop` failed was never named again — not by the next pass,
// which diffs the same request against itself, and not by any later one. The
// sweep derives the same work from the node, so the retry is simply the next
// pass.
//
// The re-sync runs at the SAME revision on purpose: that is what the worker
// issues while a reply is non-zero, and it is exactly the pass the old diff
// turned into a no-op.
//
// The sp sits at SP_LEVEL_NO_THINPOOL so that the group's array is the only
// thing above it: a live pool concat may never shrink (CN13, dmutil.go), so
// on a serving pool the removal of a group is refused by design and would
// hide what this test is about.
func TestRetireByEnumeration(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	grown := cntlrReq(reqOpts{revision: 2, primary: true, raid1: true,
		level: pb.SpLevel_SP_LEVEL_NO_THINPOOL})
	grownDataGrp(grown, true)
	if _, err := srv.SyncupCn(ctx, cnReq(2, true)); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if _, err := srv.SyncupCntlr(ctx, grown); err != nil {
		t.Fatalf("grow: %v", err)
	}
	goneDev := cnSweepMdNode(t, srv, node, 1, false)
	keptDev := cnSweepMdNode(t, srv, node, 0, false)

	node.Reset()
	node.killCmdNoEffect["mdadm --stop "+goneDev] = true
	shrunk := cntlrReq(reqOpts{revision: 3, primary: true, raid1: true,
		level: pb.SpLevel_SP_LEVEL_NO_THINPOOL})
	reply, err := srv.SyncupCntlr(ctx, shrunk)
	if err != nil {
		t.Fatalf("shrink: %v", err)
	}
	cnSweepAssertCode(t, reply.GetAgentReply(),
		common.ReplyCodeLeftover, "the group removal")
	cnSweepAssertDetails(t, reply.GetAgentReply(), goneDev, "leftover details")
	if node.arrayGone(goneDev) {
		t.Fatalf("%s is gone: the kill did not model a no-op", goneDev)
	}
	if _, ok := node.dms[legName(srv, testDataLeg2)]; !ok {
		t.Fatalf("the wrapper under the stuck array was removed")
	}

	// The retry, at the same revision, with nothing remembered from the pass
	// that failed.
	node.Reset()
	reply, err = srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 3, primary: true, raid1: true,
		level: pb.SpLevel_SP_LEVEL_NO_THINPOOL}))
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	cnSweepAssertCode(t, reply.GetAgentReply(), 0, "the re-sync")
	if !node.arrayGone(goneDev) {
		t.Fatalf("the re-sync left %s assembled", goneDev)
	}
	if _, ok := node.dms[legName(srv, testDataLeg2)]; ok {
		t.Fatalf("the removed group's wrapper survived the re-sync")
	}
	// The group that stayed in the request is untouched throughout: a sweep
	// removes what the desired state does not want, and nothing else.
	if node.arrayGone(keptDev) {
		t.Fatalf("the surviving group's array %s was stopped", keptDev)
	}
	if _, ok := node.dms[legName(srv, testDataLeg)]; !ok {
		t.Fatalf("the surviving group's wrapper was removed")
	}
}

// TestParkBeforeRemoval pins CN21's park: a wanted ns-dev whose LIVE table
// still maps something this pass is about to remove is reloaded onto its td's
// dm-error first.
//
// Two things make the order load-bearing. The reload's own flushing suspend
// is what completes the host IO that is already in flight, instead of
// replaying it at resume onto a stack that has gone; and a device the ns-dev
// still maps cannot be removed at all — `dmsetup remove` fails EBUSY behind
// it and takes the whole chain below down with it.
//
// The namespace here moves to another td, so the desired backing is a raid0
// and not the dm-error: the park can only come from reading the ns-dev's live
// table and recognising the device it maps as unwanted. The old retire phase
// answered the same question from the plan it had applied last time, which is
// memory this design does without — and which is simply absent after a
// restart, when the live table is still exactly where the last pass left it.
func TestParkBeforeRemoval(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: twoTds()})
	nsDev := nsDevName(srv, testNs)
	raid0Devno := node.devNo["/dev/mapper/"+raid0Name(srv, testTd)]
	if !strings.Contains(node.dms[nsDev].table, "linear "+raid0Devno) {
		t.Fatalf("the fixture ns-dev does not map its raid0: %q",
			node.dms[nsDev].table)
	}

	node.Reset()
	// The namespace follows the snapshot td; the origin td leaves the
	// request altogether.
	syncupCntlrAt(t, srv, reqOpts{
		revision: 3, primary: true,
		tds:    []*pb.ThinDevice{{TdId: testSnapTd, DevId: 2, Size: testTdSize}},
		subsys: subsysForTd(testSnapTd),
	})

	assertOrder(t, node,
		"cmd dmsetup reload "+nsDev,
		"cmd dmsetup remove "+raid0Name(srv, testTd),
	)
	// The park's target is the NEW td's dm-error, which is what the ns-dev
	// has to sit on while the old one is dismantled under it.
	parked := node.callsMatching("cmd dmsetup reload " + nsDev)
	if len(parked) == 0 {
		t.Fatalf("the ns-dev was never reloaded:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	errDevno := node.devNo["/dev/mapper/"+errorName(srv, testSnapTd)]
	if errDevno == "" || !strings.Contains(parked[0], "linear "+errDevno) {
		t.Fatalf("the first reload %q is not the park onto %s",
			parked[0], errorName(srv, testSnapTd))
	}
	if _, ok := node.dms[raid0Name(srv, testTd)]; ok {
		t.Fatalf("the origin td's raid0 survived")
	}
	// And the build phase put it back to work on the new td.
	newRaid0 := node.devNo["/dev/mapper/"+raid0Name(srv, testSnapTd)]
	if !strings.Contains(node.dms[nsDev].table, "linear "+newRaid0) {
		t.Fatalf("the ns-dev was left parked: %q", node.dms[nsDev].table)
	}
}

// TestThinDeleteOnlyUnderWantedPool pins the two halves of CN14's deletion
// rule, which the sweep now derives instead of remembering.
//
// A thin volume whose td left td_list is deleted from the pool metadata —
// otherwise its dev_id stays charged against a pool nobody owns — and the
// dev_id comes from the volume's LIVE TABLE, because the request that named
// it is exactly the one that has gone. The table is also what the kernel will
// act on: an id read from anywhere else can address another td's volume.
//
// A thin volume whose POOL is not wanted is only deactivated. The pool
// metadata lives on the DN legs; a `delete` aimed at a pool device that is
// itself about to go is both unsendable and pointless, and the ids left
// behind are what CN14's activation sweep collects the next time a pool is
// created.
func TestThinDeleteOnlyUnderWantedPool(t *testing.T) {
	srv, node := newTestServer(t)
	pool := poolName(srv)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: twoTds()})

	// The live volume carries a dev_id no request of this cntlr names — what
	// an interrupted pass or an older build leaves behind. Only the table can
	// say which id the pool has to be told about.
	// 5 is neither td's dev_id and neither td's id, so a `delete 5` can only
	// have come from the table.
	const liveDevId = uint32(5)
	node.holdThinIds(pool, liveDevId)
	live := node.dms[thinName(srv, testTd)]
	if live == nil {
		t.Fatalf("the fixture thin volume is missing")
	}
	fields := strings.Fields(live.table)
	fields[len(fields)-1] = "5"
	live.table = strings.Join(fields, " ")

	node.Reset()
	syncupCntlrAt(t, srv, reqOpts{
		revision: 3, primary: true,
		tds:    []*pb.ThinDevice{{TdId: testSnapTd, DevId: 2, Size: testTdSize}},
		subsys: subsysForTd(testSnapTd),
	})
	if !node.hasCall(deleteMsg(pool, liveDevId)) {
		t.Fatalf("the live table's dev_id was not deleted:\n%s",
			strings.Join(node.callsMatching("0 delete "), "\n"))
	}
	if node.hasCall(deleteMsg(pool, 1)) {
		t.Fatalf("the request's dev_id was deleted instead of the table's")
	}
	if node.thinPools[pool][liveDevId] {
		t.Fatalf("the pool still holds %d", liveDevId)
	}

	// Now the pool itself goes. The surviving td's volume is deactivated and
	// its id is left charged against the pool on the DN legs.
	node.Reset()
	syncupCntlrAt(t, srv, reqOpts{
		revision: 4, primary: true,
		tds:    []*pb.ThinDevice{{TdId: testSnapTd, DevId: 2, Size: testTdSize}},
		subsys: subsysForTd(testSnapTd),
		level:  pb.SpLevel_SP_LEVEL_NO_THINPOOL,
	})
	if _, ok := node.dms[thinNameOf(srv, testSnapTd, testSlice)]; ok {
		t.Fatalf("the thin volume survived a level that wants no pool")
	}
	if _, ok := node.dms[pool]; ok {
		t.Fatalf("the pool survived a level that wants no pool")
	}
	assertNoCall(t, node, "0 delete ")
	if !node.thinPools[pool][2] {
		t.Fatalf("the surviving td's id was deleted with the pool")
	}
}

// seedNvmetSubsys installs a subsystem in the fake's configfs tree the way a
// previous incarnation of this agent would have left it: the directory, its
// two standard children, and — when devicePath is not empty — one namespace
// backed by that device. A subsystem with no namespace at all is the shape a
// teardown that was killed between the namespace rmdir and the subsystem
// rmdir leaves behind.
func seedNvmetSubsys(node *fakeNode, nqn string, devicePath string) {
	root := agent.NvmetRoot + "/subsystems/" + nqn
	node.dirs[root] = true
	node.dirs[root+"/namespaces"] = true
	node.dirs[root+"/allowed_hosts"] = true
	if devicePath == "" {
		return
	}
	node.dirs[root+"/namespaces/1"] = true
	node.files[root+"/namespaces/1/device_path"] = devicePath + "\n"
}

// connectStray opens an nvme connection this agent's desired state does not
// name — the residue a cntlr that has gone leaves behind, since a connection
// outlives the process that made it.
func connectStray(t *testing.T, srv *CnAgentServer, nqn string) {
	t.Helper()
	if err := srv.host.Connect(context.Background(),
		agent.TrConf{TrType: "tcp", AdrFam: "ipv4",
			TrAddr: testIp2, TrSvcId: testSvcId2},
		nqn, srv.nf.CnHostNqn(testCluster, testCn)); err != nil {
		t.Fatalf("connecting %s: %v", nqn, err)
	}
}

// connectedNsDevNo is the device number of one connection's namespace node —
// what a dm-clone's table carries as its source argument. It is read through
// the agent's own sysfs walk, so the test names the device the same way the
// sweep does.
func connectedNsDevNo(
	t *testing.T,
	srv *CnAgentServer,
	node *fakeNode,
	nqn string,
) string {
	t.Helper()
	state, err := srv.host.ListSubsys(context.Background(), nqn)
	if err != nil {
		t.Fatalf("listing %s: %v", nqn, err)
	}
	devNo := node.devNo[state.DevicePath]
	if devNo == "" {
		t.Fatalf("%s has no namespace device (path %q)", nqn,
			state.DevicePath)
	}
	return devNo
}

// TestUnownedXferConnectionSwept pins `architecture.md` §9.8's attribution
// rule for the one object whose name says nothing about who needs it.
//
// A clone-source connection is named for the SOURCE sp, not for the cntlr
// that dials it, so there is no id in it to sweep by. "Somebody here still
// wants it" is the only test there is, and it has two halves that must BOTH
// hold: a stored request that names the NQN as a clone's src_nqn, and a live
// dm-clone that maps its namespace. The second half is not redundant — a
// dm-clone holds its source open whatever any request says, and disconnecting
// under it strands the hydration IO it is in the middle of.
//
// The connection that neither half claims is residue that keeps a controller,
// a transport connection and a namespace alive for ever, and only the
// node-level sweep may remove it: it runs under the node write lock, so it
// cannot race a build that has just connected a source and not yet created
// its dm-clone.
func TestUnownedXferConnectionSwept(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})

	mapped := srv.nf.XferNqn(testCluster, testSp, 0x777)
	stray := srv.nf.XferNqn(testCluster, testSp, 0x888)
	connectStray(t, srv, mapped)
	connectStray(t, srv, stray)
	// The live dm-clone is re-pointed at the connection no request names, so
	// each of the two halves is the ONLY thing keeping its own connection.
	dmClone := node.dms[cloneName(srv, testClone)]
	if dmClone == nil {
		t.Fatalf("the fixture dm-clone is missing")
	}
	mappedDev := connectedNsDevNo(t, srv, node, mapped)
	fields := strings.Fields(dmClone.table)
	fields[5] = mappedDev
	dmClone.table = strings.Join(fields, " ")

	node.Reset()
	reply := cnSweepSyncup(t, srv, 3, true)
	cnSweepAssertCode(t, reply.GetAgentReply(), 0, "SyncupCn")

	if _, ok := node.subsystems[stray]; ok {
		t.Fatalf("the unowned clone source survived the node-level sweep")
	}
	if !node.hasCall("cmd nvme disconnect --nqn " + stray) {
		t.Fatalf("the unowned clone source was never disconnected:\n%s",
			strings.Join(node.Calls(), "\n"))
	}
	for _, nqn := range []string{testSrcNqn, mapped} {
		if _, ok := node.subsystems[nqn]; !ok {
			t.Fatalf("%s was disconnected", nqn)
		}
		assertNoCall(t, node, "cmd nvme disconnect --nqn "+nqn)
	}
}

// TestHostFacingSubsystemAttributedByDevicePath pins the other half of that
// rule: a host-facing subsystem's NQN is chosen by the user and carries
// nothing of ours, so once no stored cntlr names it the only thing left that
// can attribute it is what its namespaces are backed by.
//
// Attribution by the stored plan is what this replaces, and it could not
// survive §9.8's drop-at-pointer-removal: the file that held the NQN list is
// dropped the moment the pointer goes, which is precisely when the subsystem has to be found. A
// subsystem whose namespace backs onto a removed sp's ns-dev is that sp's and
// goes with it; one with no attributable namespace at all — what a teardown
// killed between two rmdirs leaves — is unowned and only the node-level sweep
// may remove it; one backed by an ns-dev the desired state still wants is
// untouchable, because a host is using it right now.
func TestHostFacingSubsystemAttributedByDevicePath(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	strandedNs := srv.nf.CnNsDevName(
		testCluster, testCn, cnSweepStrandedSp, testNs)
	seedStrayDm(node, strandedNs, "253:210")
	const strandedNqn = "nqn.2024-01.io.dnv-it:s:stranded"
	const emptyNqn = "nqn.2024-01.io.dnv-it:s:halfremoved"
	seedNvmetSubsys(node, strandedNqn, "/dev/mapper/"+strandedNs)
	seedNvmetSubsys(node, emptyNqn, "")

	node.Reset()
	reply := cnSweepSyncup(t, srv, 3, true)
	cnSweepAssertCode(t, reply.GetAgentReply(), 0, "SyncupCn")

	if subsysDirPresent(node, strandedNqn) {
		t.Fatalf("the subsystem of a removed sp survived")
	}
	if _, ok := node.dms[strandedNs]; ok {
		t.Fatalf("the removed sp's ns-dev survived")
	}
	if subsysDirPresent(node, emptyNqn) {
		t.Fatalf("the unowned subsystem survived")
	}
	// The serving one is not removed and rebuilt: SyncupCn runs no cntlr
	// converge, so a `rmdir` here would be a host losing its namespace.
	if !subsysDirPresent(node, testNqn) {
		t.Fatalf("the serving subsystem was removed")
	}
	assertNoCall(t, node, "cmd rmdir "+agent.NvmetRoot+"/subsystems/"+testNqn)
}

// TestForeignMdArrayUntouched pins the attribution rule that keeps this agent
// out of other people's business: an array is ours only when EVERY member is
// a kind-c9 leg wrapper of this cluster and this cn.
//
// The lab runs a dn agent in the same kernel, and a DN's own disks get
// assembled by udev into arrays this agent enumerates along with its own —
// `ls /sys/block` has no owner column. An array with a member that is not one
// of our wrappers is therefore never attributed and never stopped, however
// unwanted it looks. Stopping one would take a co-hosted DN's storage down.
func TestForeignMdArrayUntouched(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, raid1: true})
	ours := cnSweepMdNode(t, srv, node, 0, false)
	const foreign = "/dev/md/other"
	node.seedArray(foreign, "other", "/dev/sdb")
	foreignNode := node.mdNode(foreign)
	if foreignNode == "" {
		t.Fatalf("the foreign array has no sysfs node")
	}

	node.Reset()
	reply := cnSweepSyncup(t, srv, 3, false)
	cnSweepAssertCode(t, reply.GetAgentReply(), 0, "SyncupCn")

	// Non-vacuous: the same pass stopped our own array.
	if !node.arrayGone(ours) {
		t.Fatalf("our own array %s was not stopped, so the negative "+
			"assertion below proves nothing", ours)
	}
	if node.arrayGone(foreign) {
		t.Fatalf("the foreign array was stopped")
	}
	assertNoCall(t, node, "cmd mdadm --stop "+foreignNode)
}

// TestEnumerationFailureIsLeftover pins the one rule that makes a sweep safe
// to run at all: an enumeration that DID NOT ANSWER cannot prove the node is
// clean.
//
// `dmsetup ls` killed at the soft timeout returns exit -1 and nothing else. A
// sweep that read that as "this node holds no dm devices" would subtract the
// desired state from an empty set, conclude that everything is in order, and
// reply OK — and the leak would be permanent, because nothing would ever look
// again. So the failure is reported as a leftover in its own right, with the
// enumeration named, and not one removal is attempted on the strength of an
// answer that never came.
func TestEnumerationFailureIsLeftover(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	stray := srv.nf.CnNsDevName(
		testCluster, testCn, cnSweepStrandedSp, testNs)
	seedStrayDm(node, stray, "253:220")

	node.Reset()
	node.killCmdNoEffectAlways["dmsetup ls"] = true
	reply := cnSweepSyncup(t, srv, 3, true)
	cnSweepAssertCode(t, reply.GetAgentReply(),
		common.ReplyCodeLeftover, "SyncupCn with a killed enumeration")
	cnSweepAssertDetails(t, reply.GetAgentReply(),
		"enumeration failed", "the failure details")
	cnSweepAssertDetails(t, reply.GetAgentReply(),
		"dmsetup ls", "the failure details")

	cnSweepAssertNoRemoval(t, node)
	if _, ok := node.dms[stray]; !ok {
		t.Fatalf("the stray device was removed on an unanswered listing")
	}
}

// TestUndecodableDnvNqnLeftAlone pins the third arm of the NQN rule, the one
// that exists to be conservative.
//
// ParseNqn is strict: an unknown kind digit or the wrong number of ids makes
// a name decode to nothing. Such a name is still plainly in the dnv namespace,
// so it is NOT the user-chosen host-facing NQN that a parse failure otherwise
// means — and the difference matters, because a host-facing subsystem is
// attributed by what its namespaces point at, and a subsystem attributed to a
// removed sp is DELETED. Treating a malformed name as host-facing would
// therefore let a name this build does not understand — one a newer build
// wrote, or one somebody typed — get a subsystem removed. Both are left
// alone, and the namespaces of neither are even read.
func TestUndecodableDnvNqnLeftAlone(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})

	strandedNs := "/dev/mapper/" + srv.nf.CnNsDevName(
		testCluster, testCn, cnSweepStrandedSp, testNs)
	// An unknown kind digit, and a known kind with the wrong arity: both
	// would be removed as host-facing subsystems of the stranded sp if the
	// prefix were ignored.
	unknownKind := common.NqnPrefix + ":9:" + strings.Repeat("0", 15) + "1"
	wrongArity := common.NqnPrefix + ":4:" + strings.Repeat("0", 15) + "1"
	for _, nqn := range []string{unknownKind, wrongArity} {
		if _, ok := common.ParseNqn(nqn); ok {
			t.Fatalf("%s decodes, so the fixture proves nothing", nqn)
		}
		seedNvmetSubsys(node, nqn, strandedNs)
	}

	node.Reset()
	reply := cnSweepSyncup(t, srv, 3, true)
	cnSweepAssertCode(t, reply.GetAgentReply(), 0, "SyncupCn")

	for _, nqn := range []string{unknownKind, wrongArity} {
		if !subsysDirPresent(node, nqn) {
			t.Fatalf("%s was removed", nqn)
		}
		assertNoCall(t, node, "cmd rmdir "+agent.NvmetRoot+"/subsystems/"+nqn)
		// Never even attributed: a dnv-prefixed name is not read for a
		// backing device, because the answer could only be used to remove it.
		assertNoCall(t, node,
			agent.NvmetRoot+"/subsystems/"+nqn+"/namespaces")
	}
}

// ---------------------------------------------------------------------------
// An enumeration that did not answer licenses nothing
// ---------------------------------------------------------------------------

// TestUnansweredEnumerationRemovesNothing pins the rule the whole design
// rests on, at the one place it is easiest to get wrong: the ENUMERATION
// itself.
//
// Every removal here is "actual minus desired". When `dmsetup ls` does not
// answer, `actual` is empty for want of an answer — and an empty `actual`
// subtracts to "remove nothing", which looks safe and is not. It reads as
// "there is nothing left" everywhere a removal is gated on ABSENCE rather
// than on presence: the layer stop rule never fires, so every lower layer
// runs as though the ones above it had succeeded, and the live-device half of
// the evidence that protects a shared object disappears node-wide.
//
// So the pass must touch nothing at all and say so. The fixture is the
// harshest one available: the cntlr is forgotten first, so the desired state
// wants none of these objects and a working pass would remove every one of
// them.
func TestUnansweredEnumerationRemovesNothing(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	cnSweepForget(t, srv, node)

	node.mu.Lock()
	before := len(node.dms)
	node.mu.Unlock()
	if before == 0 {
		t.Fatal("fixture is wrong: the cntlr built no dm device")
	}

	node.Reset()
	// Killed, not "said no": exit -1 with an error is the shape that must
	// never read as an empty node.
	node.killCmdAlways["dmsetup ls"] = true
	reply := cnSweepSyncup(t, srv, 3, false)

	if got := reply.GetAgentReply().GetCode(); got != common.ReplyCodeLeftover {
		t.Errorf("code = %d, want ReplyCodeLeftover (%d): an unanswered "+
			"enumeration must re-drive, not report success",
			got, common.ReplyCodeLeftover)
	}
	node.mu.Lock()
	after := len(node.dms)
	node.mu.Unlock()
	if after != before {
		t.Errorf("dm devices went from %d to %d: the pass removed something "+
			"it could not see", before, after)
	}
	if !subsysDirPresent(node, testNqn) {
		t.Error("the host-facing subsystem was removed on an unanswered " +
			"enumeration")
	}
	if len(node.subsystems) == 0 {
		t.Error("nvme connections were disconnected on an unanswered " +
			"enumeration")
	}
	for _, call := range []string{
		"cmd dmsetup remove ", "cmd mdadm --stop ", "cmd nvme disconnect ",
	} {
		if node.hasCall(call) {
			t.Errorf("the pass issued %q with no enumeration to justify it",
				call)
		}
	}
}

// TestUnreadableCloneTableKeepsEverySource is the "did not answer" rule
// applied to the live-device half of that attribution.
//
// `srcNqnsInUse` protects a clone source two ways: a stored request that
// names it, and a LIVE dm-clone whose table maps it. The second half is read
// with `dmsetup table`, and when that command does not answer while the
// device is still present, the clone contributes no source devno — which is
// indistinguishable from "this clone maps nothing". Recording the failure is
// not the same as acting on it: nothing downstream consults the failure list
// before disconnecting, so the omission alone would disconnect a live
// clone's source and strand the hydration IO it is in the middle of. That is
// the exact inverse of the rule the function's own comment states.
//
// So a clone that would not say what it maps makes EVERY `:4:` connection
// in-use for that pass. It costs a round; the alternative cannot be undone,
// because nothing names that NQN any more once it is gone and no later
// converge reconnects it.
func TestUnreadableCloneTableKeepsEverySource(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})

	mapped := srv.nf.XferNqn(testCluster, testSp, 0x777)
	stray := srv.nf.XferNqn(testCluster, testSp, 0x888)
	connectStray(t, srv, mapped)
	connectStray(t, srv, stray)
	clone := cloneName(srv, testClone)
	dmClone := node.dms[clone]
	if dmClone == nil {
		t.Fatalf("the fixture dm-clone is missing")
	}
	mappedDev := connectedNsDevNo(t, srv, node, mapped)
	fields := strings.Fields(dmClone.table)
	fields[5] = mappedDev
	dmClone.table = strings.Join(fields, " ")

	node.Reset()
	// The clone is still there; it just will not say what it maps. Killed
	// with no effect, so the device is untouched and the re-probe finds it.
	node.killCmdNoEffectAlways["dmsetup table "+clone] = true
	reply := cnSweepSyncup(t, srv, 3, true)
	if got := reply.GetAgentReply().GetCode(); got == 0 {
		t.Errorf("code = 0: a pass that could not read a live clone's table " +
			"has not finished and must re-drive")
	}

	// NEITHER connection goes: not the one the unreadable clone may be
	// mapping, and not the one no request names — because the clone that
	// would not answer might have been mapping either.
	for _, nqn := range []string{mapped, stray} {
		if _, ok := node.subsystems[nqn]; !ok {
			t.Errorf("%s was disconnected although a live dm-clone would "+
				"not say whether it maps it", nqn)
		}
		if node.hasCall("cmd nvme disconnect --nqn " + nqn) {
			t.Errorf("the pass tried to disconnect %s on an unreadable "+
				"clone table", nqn)
		}
	}
}
