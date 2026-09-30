package cnagent

import (
	"context"
	"strings"
	"testing"
	"time"

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

// cnSweepMdNode is the /dev/mdN (or /dev/md_<name>) a sweep sees for one
// group's array. It must be read BEFORE the pass that stops the array: a
// stopped array has no sysfs node left to name, and the sweep never uses the
// /dev/md/<name> spelling because that one depends on udev having run.
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

// awaitDisconnects waits until every `nvme disconnect` a pass has set going
// in the background has returned: CN10's disconnect registry is empty. An
// entry lives exactly as long as its goroutine, so one that outlives its
// command — a failed disconnect remembered — fails here by name.
func awaitDisconnects(t *testing.T, srv *CnAgentServer) {
	t.Helper()
	inFlight := func() []string {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		var nqns []string
		for nqn := range srv.disconnecting {
			nqns = append(nqns, nqn)
		}
		return nqns
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(inFlight()) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("disconnects never left the registry: %v", inFlight())
		}
		time.Sleep(time.Millisecond)
	}
}

// cnSweepOnlyDisconnects is the reply of a pass that disconnects anything:
// ReplyCodeLeftover naming nvme host connections and nothing else — no other
// leftover, no enumeration failure. The disconnects run off the pass's locks
// (CN21), so every connection the pass sets disconnecting is still there when
// it replies, and only a later pass's own probe finds it gone.
func cnSweepOnlyDisconnects(t *testing.T, reply *pb.AgentReply, label string) {
	t.Helper()
	cnSweepAssertCode(t, reply, common.ReplyCodeLeftover, label)
	_, names, ok := strings.Cut(reply.GetDetails(), "): ")
	if !ok || strings.Contains(names, "; ") ||
		strings.Contains(names, " more]") {
		t.Fatalf("%s: details %q are not a plain leftover list",
			label, reply.GetDetails())
	}
	for _, name := range strings.Split(names, ", ") {
		if !strings.HasPrefix(name, agent.LeftoverKindNvme+":") {
			t.Fatalf("%s: %q is left over besides the disconnects (%q)",
				label, name, reply.GetDetails())
		}
	}
}

// cnSweepSyncupSettled is cnSweepSyncup for a teardown that disconnects,
// seen the way the worker sees it: the pass names the connections it set
// disconnecting and nothing else, and the re-sync at the same revision, once
// they have returned, is the reply that says what the sweep achieved.
func cnSweepSyncupSettled(
	t *testing.T,
	srv *CnAgentServer,
	revision uint64,
	withCntlr bool,
) *pb.SyncupCnReply {
	t.Helper()
	cnSweepOnlyDisconnects(t,
		cnSweepSyncup(t, srv, revision, withCntlr).GetAgentReply(),
		"the pass that disconnects")
	awaitDisconnects(t, srv)
	return cnSweepSyncup(t, srv, revision, withCntlr)
}

// syncupCntlrSettled is the same for a cntlr-level pass.
func syncupCntlrSettled(
	t *testing.T,
	srv *CnAgentServer,
	o reqOpts,
) *pb.SyncupCntlrReply {
	t.Helper()
	send := func() *pb.SyncupCntlrReply {
		t.Helper()
		reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(o))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		return reply
	}
	cnSweepOnlyDisconnects(t, send().GetAgentReply(),
		"the pass that disconnects")
	awaitDisconnects(t, srv)
	return send()
}

// TestRemovedCntlrSweptByName is the design's headline: removal is derived
// from the live system, not from a plan.
//
// The cntlr's state file and its memory entry are dropped BEFORE the pass, so
// there is provably no plan, no request and no bookkeeping left that names a
// single one of its objects — exactly the state §9.8's drop-at-pointer-removal
// leaves behind when a pointer disappears, and exactly the state an agent restarted in the middle
// of a teardown wakes up in. Everything of that sp must still go, by name,
// and the reply must be a plain OK once the leg disconnects the pass set
// going have returned: an agent that could only remove what it still
// remembered would leak the whole stack here and say nothing.
func TestRemovedCntlrSweptByName(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	cnSweepForget(t, srv, node)

	node.Reset()
	reply := cnSweepSyncupSettled(t, srv, 3, false)
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
	reply = cnSweepSyncupSettled(t, srv, 3, false)
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
	reply := cnSweepSyncupSettled(t, srv, 3, false)
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

// TestSweepStopsANamedArrayNode pins the sweep's stop of an array on a named
// kernel node (CN12, CN21; amended 2026-09-26). With mdadm.conf `CREATE
// names=yes` the group's array runs on md_<name>, which /sys/block lists
// under that name; ListArrays names it /dev/md_<name>, and that is the node
// `mdadm --stop` gets and Gone reads — never the /dev/md/<name> symlink,
// which depends on udev having run. The fake resolves the symlink's spelling
// too, so only the exact command tells the two apart. Two node names are
// run: md_<CnMdDevName>, hex, which the agent's own `--create` and
// `--assemble` of /dev/md/<CnMdDevName> give it, and md_<CnMdArrayName>, not
// hex, which an `--assemble --scan` or incremental assembly gives it from the
// superblock name.
func TestSweepStopsANamedArrayNode(t *testing.T) {
	for _, tc := range []struct {
		name string
		node func(nf *common.NameFmt) string
	}{
		{"dev name", func(nf *common.NameFmt) string {
			return "md_" + nf.CnMdDevName(
				testCluster, testCn, testSp, 0, 0, false)
		}},
		{"superblock name", func(nf *common.NameFmt) string {
			return "md_" + nf.CnMdArrayName(testSp, 0, 0, false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{
				revision: 2, primary: true, raid1: true, twoLegs: true})
			name := srv.nf.CnMdDevName(
				testCluster, testCn, testSp, 0, 0, false)
			kernelNode := tc.node(srv.nf)
			node.mu.Lock()
			array := node.arrays[srv.nf.MdPath(name)]
			if array == nil {
				node.mu.Unlock()
				t.Fatalf("the data group has no array at %s",
					srv.nf.MdPath(name))
			}
			node.unpublishArray(array)
			array.node = kernelNode
			node.publishArray(array)
			node.mu.Unlock()
			dataDev := cnSweepMdNode(t, srv, node, 0, false)
			if dataDev != "/dev/"+kernelNode {
				t.Fatalf("the data group's array is at %s, want /dev/%s; "+
					"the case is vacuous otherwise", dataDev, kernelNode)
			}

			node.Reset()
			reply := cnSweepSyncupSettled(t, srv, 3, false)
			cnSweepAssertCode(t, reply.GetAgentReply(), 0, "SyncupCn")

			stopped := node.indexOfCall("cmd mdadm --stop " + dataDev)
			if stopped < 0 {
				t.Fatalf("the named node was never stopped:\n%s",
					strings.Join(node.Calls(), "\n"))
			}
			assertNoCall(t, node, "cmd mdadm --stop /dev/md/")
			if node.indexOfCallFrom("read "+sysfsBlockDir+"/"+kernelNode+
				"/md/array_state", stopped) < 0 {
				t.Fatalf("the stop of %s was not verified from its own "+
					"node:\n%s", dataDev, strings.Join(node.Calls(), "\n"))
			}
			if !node.arrayGone(dataDev) {
				t.Fatalf("%s survived the sweep", dataDev)
			}
			cnSweepNoDmLeft(t, node)
		})
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
	reply := cnSweepSyncupSettled(t, srv, 3, false)
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
		// The legs' disconnects run off the startup pass (CN21) and change
		// the node under the reads below.
		awaitDisconnects(t, fresh)
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
	reply := syncupCntlrSettled(t, srv, reqOpts{
		revision: 3, primary: true, level: pb.SpLevel_SP_LEVEL_DISABLE})
	cnSweepAssertCode(t, reply.GetAgentReply(), 0, "SP_LEVEL_DISABLE")

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

// TestDisableCreatesNoParkTarget: at SP_LEVEL_DISABLE nothing parks a planned
// ns-dev onto its td's dm-error — which here does not exist, because the build
// that made the ns-dev had that create refused. The CN9 pre-step park would
// create it, after the sweep's enumeration, so in no chain: the pass would
// remove the ns-dev parked on it, reply clean, and leave an unwanted device
// for the next round to find. The wanted set holds no ns-dev at that level, so
// P0 parks every one by its own live table, onto an error table of its own
// size, and nothing is created.
func TestDisableCreatesNoParkTarget(t *testing.T) {
	srv, node := newTestServer(t)
	c5 := errorName(srv, testTd)
	nsDev := nsDevName(srv, testNs)
	node.failCmd["dmsetup create "+c5] = "device-mapper: create ioctl failed"
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	if _, ok := node.dms[c5]; ok {
		t.Fatalf("fixture is wrong: %s exists", c5)
	}
	if _, ok := node.dms[nsDev]; !ok {
		t.Fatalf("fixture is wrong: the ns-dev was not built")
	}

	disabled := reqOpts{revision: 3, primary: true,
		level: pb.SpLevel_SP_LEVEL_DISABLE}

	node.Reset()
	// The legs' disconnects run off the pass (CN10), so the pass names them
	// and nothing else, and it is the re-drive after they return that
	// replies clean.
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(disabled))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	cnSweepOnlyDisconnects(t, reply.GetAgentReply(), "SP_LEVEL_DISABLE")
	cnSweepNoDmLeft(t, node)
	awaitDisconnects(t, srv)
	syncupCntlrAt(t, srv, disabled)

	cnSweepNoDmLeft(t, node)
	assertNoCall(t, node, "cmd dmsetup create "+c5)
	reloads := node.callsMatching("cmd dmsetup reload " + nsDev)
	if len(reloads) != 1 {
		t.Fatalf("the ns-dev was parked %d times, want 1", len(reloads))
	}
	if !strings.HasSuffix(reloads[0], " error") {
		t.Fatalf("the ns-dev was not parked on an error table: %q",
			reloads[0])
	}
	assertOrder(t, node,
		"cmd dmsetup reload "+nsDev,
		"cmd dmsetup remove "+nsDev,
	)
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
	reply = syncupCntlrSettled(t, srv, reqOpts{
		revision: 3, primary: true, raid1: true,
		level: pb.SpLevel_SP_LEVEL_NO_THINPOOL})
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
	reply := cnSweepSyncupSettled(t, srv, 3, true)
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
	reply := cnSweepSyncupSettled(t, srv, 3, false)
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

// TestNamespaceRemovalIsTheChainsAlone: a namespace that left `ns_list`
// while its subsystem stays is removed by CN21's L1 and by nothing else. The
// build phase used to drop it too, from its own listing, which put a second
// removal beside the chain: one that ran when the pass's enumeration had not
// answered — the pass that must touch nothing — and that skipped L1's move to
// inaccessible, so a host holding the path lost it under IO instead of being
// told to stop using it.
func TestNamespaceRemovalIsTheChainsAlone(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	nsPath := agent.NvmetRoot + "/subsystems/" + testNqn + "/namespaces/1"
	if !node.dirs[nsPath] {
		t.Fatalf("fixture is wrong: no nvmet namespace at %s", nsPath)
	}
	subsys := defaultSubsys(false)
	subsys[testNqn].NsList = nil
	left := reqOpts{revision: 3, primary: true, subsys: subsys}

	node.Reset()
	node.killCmdAlways["dmsetup ls"] = true
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(left))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	cnSweepAssertCode(t, reply.GetAgentReply(), common.ReplyCodeLeftover,
		"the pass whose enumeration did not answer")
	if !node.dirs[nsPath] {
		t.Fatalf("the namespace was removed by a pass whose enumeration " +
			"did not answer")
	}
	assertNoCall(t, node, "cmd rmdir "+nsPath)

	// The next pass sees the node, and L1 removes the namespace — moved to
	// inaccessible first, and exactly once.
	delete(node.killCmdAlways, "dmsetup ls")
	node.Reset()
	syncupCntlrAt(t, srv, left)
	if node.dirs[nsPath] {
		t.Fatalf("the namespace that left ns_list survived")
	}
	assertOrder(t, node,
		"writedirect "+nsPath+"/ana_grpid=3",
		"cmd rmdir "+nsPath,
	)
	if n := len(node.callsMatching("cmd rmdir " + nsPath)); n != 1 {
		t.Fatalf("the namespace was removed %d times, want 1", n)
	}
}

// TestUnansweredMdListingStopsTheDescent pins the same rule on the listing
// whose silence costs the most: the md arrays.
//
// The data group is a live two-leg raid1, and this pass wants neither the
// array nor its legs — the sp has left the node, or its level has risen to
// SP_LEVEL_NO_SIDE. The `/sys/block` listing ListArrays opens with does not
// answer, while every other listing does, so `actual.arrays` is empty for want
// of an answer rather than because the node runs no array. A descent that went
// on regardless would find no array for L9 to stop, read that layer as clean,
// and let L10 disconnect both legs from under the running mirror: md fails
// them, and on the migration path that strands the superblocks the next CN
// assembles from. So neither scope removes anything on that snapshot — the
// reply is a Leftover naming the md enumeration — and the re-drive at the same
// revision, with the listing answering, stops the arrays with nothing
// remembered from the pass that stopped.
func TestUnansweredMdListingStopsTheDescent(t *testing.T) {
	for _, tc := range []struct {
		name string
		pass func(t *testing.T, srv *CnAgentServer) *pb.AgentReply
	}{
		{"node-level", func(t *testing.T, srv *CnAgentServer) *pb.AgentReply {
			return cnSweepSyncup(t, srv, 3, false).GetAgentReply()
		}},
		{"cntlr-level", func(t *testing.T, srv *CnAgentServer) *pb.AgentReply {
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 3, primary: true, raid1: true,
					twoLegs: true, level: pb.SpLevel_SP_LEVEL_NO_SIDE}))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			return reply.GetAgentReply()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{
				revision: 2, primary: true, raid1: true, twoLegs: true})
			arrays := []string{
				cnSweepMdNode(t, srv, node, 0, false),
				cnSweepMdNode(t, srv, node, 0, true),
			}

			node.Reset()
			// Killed, not answered "no", and one-shot: the pass's first
			// `ls` of /sys/block is the sweep's own enumeration.
			node.killCmd["ls -1 "+sysfsBlockDir] = true
			reply := tc.pass(t, srv)
			cnSweepAssertCode(t, reply, common.ReplyCodeLeftover,
				"the pass over an unanswered md listing")
			cnSweepAssertDetails(t, reply, "enumeration failed: md arrays",
				"the failure details")
			for _, call := range []string{
				"cmd nvme disconnect", "cmd mdadm --stop", "cmd dmsetup remove",
			} {
				assertNoCall(t, node, call)
			}
			for _, dev := range arrays {
				if node.arrayGone(dev) {
					t.Fatalf("%s was stopped on an unanswered listing", dev)
				}
			}
			node.mu.Lock()
			for _, legId := range []uint64{
				testDataLeg, testDataLeg2, testMetaLeg,
			} {
				if _, ok := node.dms[legName(srv, legId)]; !ok {
					t.Errorf("leg %#x lost its wrapper", legId)
				}
				if _, ok := node.subsystems[legNqn(srv, legId)]; !ok {
					t.Errorf("leg %#x lost its connection", legId)
				}
			}
			node.mu.Unlock()
			if t.Failed() {
				t.FailNow()
			}

			// The re-drive, the listing answering: the same enumeration now
			// finds both arrays and the descent stops them — they were
			// unwanted all along, which is what makes their survival above
			// mean something. Nothing of the pass that stopped is consulted;
			// nothing of it was remembered.
			node.Reset()
			reply = tc.pass(t, srv)
			if strings.Contains(reply.GetDetails(), "md arrays") {
				t.Fatalf("the re-drive still names the md enumeration: %q",
					reply.GetDetails())
			}
			for _, dev := range arrays {
				if !node.arrayGone(dev) {
					t.Fatalf("the re-drive left %s assembled", dev)
				}
			}
		})
	}
}

// TestUnansweredSubsystemListingRemovesNothing pins the same rule on the two
// listings of subsystems: the sysfs walk of this host's nvme connections and
// the `ls` of the nvmet configfs tree. Either one that does not answer leaves
// its part of the snapshot empty, and an empty part is the shape of "nothing
// here" to the layers that read it — L1 finds no nvmet object above the
// ns-devs, L5 and L10 no connection — so a pass that went on would run every
// layer below as though those had gone clean. The fixture is the harshest one
// again: the cntlr is forgotten, so a pass that did run would tear down
// everything of the sp it could see. The `dmsetup ls` row holds that listing
// to the same full list of teardown commands, which
// TestUnansweredEnumerationRemovesNothing checks only in part.
func TestUnansweredSubsystemListingRemovesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string // the enumeration, as the verdict names it
		kill string // the listing behind it
	}{
		{"dmsetup ls", "dmsetup ls"},
		{"nvme subsystems", "ls -1 " + sysfsNvmeSubsysDir},
		{"nvmet subsystems", "ls -1 " + agent.NvmetRoot + "/subsystems"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			cnSweepForget(t, srv, node)
			node.mu.Lock()
			before := len(node.dms)
			node.mu.Unlock()

			node.Reset()
			node.killCmdAlways[tc.kill] = true
			reply := cnSweepSyncup(t, srv, 3, false)
			cnSweepAssertCode(t, reply.GetAgentReply(),
				common.ReplyCodeLeftover, "SyncupCn with a killed listing")
			cnSweepAssertDetails(t, reply.GetAgentReply(),
				"enumeration failed: "+tc.name, "the failure details")
			cnSweepAssertNoRemoval(t, node)
			node.mu.Lock()
			after := len(node.dms)
			node.mu.Unlock()
			if after != before {
				t.Fatalf("dm devices went from %d to %d on an unanswered "+
					"listing", before, after)
			}
			if !subsysDirPresent(node, testNqn) {
				t.Fatal("the host-facing subsystem was removed on an " +
					"unanswered listing")
			}
		})
	}
}

// cnSweepListings are the four listings a pass is derived from, each killed by
// the one command behind it and named in the verdict the way enumerateCn
// names it.
var cnSweepListings = []struct {
	name string // the enumeration, as the verdict names it
	kill string // the listing behind it
}{
	{"dmsetup ls", "dmsetup ls"},
	{"md arrays", "ls -1 " + sysfsBlockDir},
	{"nvme subsystems", "ls -1 " + sysfsNvmeSubsysDir},
	{"nvmet subsystems", "ls -1 " + agent.NvmetRoot + "/subsystems"},
}

// TestUnansweredListingStillMovesAnaBeforeThePark pins what a converge still
// owes the hosts on a pass whose sweep an unanswered listing stopped. That
// sweep removes nothing, and it skips the two pre-steps that exist only for a
// removal, but not the ANA move: the build phase after the sweep parks
// whether or not the sweep ran. ensureNsDev reloads a demoted or suspended
// namespace's ns-dev onto the td's dm-error, ensureXfer reloads an unserved
// transfer device onto an error table, and neither moves an ANA group. With
// no move first, a host keeps an optimized path over an error table and takes
// IO errors where it should have queued.
//
// The two shapes differ in what comes after. The demotion leaves the pool
// and the arrays to remove, so its verdict stays a Leftover and the worker
// drives another pass. The suspend on a steady primary leaves nothing to
// remove: from the next round on the verdict reads OK, and nothing would
// correct a wrong group before the next revision. So the move is asserted
// on the pass itself, once and ahead of the park, for every listing.
func TestUnansweredListingStillMovesAnaBeforeThePark(t *testing.T) {
	xfers := []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn, OriNsIdx: 1}}
	for _, shape := range []struct {
		name string
		from reqOpts
		to   reqOpts
		// xfer: the fixture carries a transfer, whose namespace P1 moves
		// ahead of the transfer device's demotion as well.
		xfer bool
		// settled: the pass leaves nothing to remove, so the Check verdict
		// that follows reads OK.
		settled bool
	}{
		{"demotion",
			reqOpts{revision: 2, primary: true, raid1: true, xfers: xfers},
			reqOpts{revision: 3, primary: false, raid1: true, xfers: xfers},
			true, false},
		{"suspend",
			reqOpts{revision: 2, primary: true, raid1: true},
			reqOpts{revision: 3, primary: true, raid1: true, suspended: true},
			false, true},
	} {
		for _, listing := range cnSweepListings {
			t.Run(shape.name+"/"+listing.name, func(t *testing.T) {
				ctx := context.Background()
				srv, node := newTestServer(t)
				syncupBoth(t, srv, shape.from)
				xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
				if got := node.files[anaPath(testNqn, 1)]; got != "1" {
					t.Fatalf("fixture: ana_grpid is %q before the pass, "+
						"want 1", got)
				}
				if shape.xfer {
					if got := node.files[anaPath(xferNqn, 1)]; got != "1" {
						t.Fatalf("fixture: the transfer's ana_grpid is %q "+
							"before the pass, want 1", got)
					}
				}

				node.Reset()
				// One-shot: the pass's first such command is the sweep's own
				// enumeration.
				node.killCmd[listing.kill] = true
				reply, err := srv.SyncupCntlr(ctx, cntlrReq(shape.to))
				if err != nil {
					t.Fatalf("SyncupCntlr: %v", err)
				}
				cnSweepAssertCode(t, reply.GetAgentReply(),
					common.ReplyCodeLeftover, "the pass over an unanswered "+
						"listing")
				cnSweepAssertDetails(t, reply.GetAgentReply(),
					"enumeration failed: "+listing.name, "the failure details")
				for _, call := range []string{
					"cmd dmsetup remove", "cmd mdadm --stop",
					"cmd nvme disconnect",
				} {
					assertNoCall(t, node, call)
				}

				// Exactly one write, to 3, ahead of the build's park: an
				// ordering assertion stops at its first match, so only the
				// count shows that nothing moved the group back later on.
				nsAna := "writedirect " + anaPath(testNqn, 1) + "="
				if moves := node.callsMatching(nsAna); len(moves) != 1 ||
					moves[0] != nsAna+"3" {
					t.Fatalf("ana_grpid writes %q, want exactly one, to 3",
						moves)
				}
				assertOrder(t, node, nsAna+"3",
					"cmd dmsetup reload "+nsDevName(srv, testNs))
				if got := node.files[anaPath(testNqn, 1)]; got != "3" {
					t.Fatalf("ana_grpid is %q after the pass, want 3", got)
				}
				assertParked(t, srv, node, testNs, testTd, shape.name)
				if shape.xfer {
					xferAna := "writedirect " + anaPath(xferNqn, 1) + "="
					if moves := node.callsMatching(xferAna); len(moves) != 1 ||
						moves[0] != xferAna+"3" {
						t.Fatalf("transfer ana_grpid writes %q, want exactly "+
							"one, to 3", moves)
					}
					assertOrder(t, node, xferAna+"3",
						"cmd dmsetup reload "+xferName(srv, testXfer))
					table := node.dms[xferName(srv, testXfer)].table
					if !strings.HasSuffix(table, " error") {
						t.Fatalf("the transfer device is %q, want its error "+
							"table", table)
					}
				}

				// What the worker sees next: the namespace is where the
				// desired state wants it, whether or not a leftover still
				// drives another pass.
				node.Reset()
				check, _ := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
					ClusterId: testCluster, CnId: testCn,
					CntlrPointer: cntlrPtr(), Revision: 3, ShowInfo: true,
				}, nil)
				assertOk(t, check.GetCntlrInfo().GetNsIdToNamespace()[testNs],
					"the namespace on the next check round")
				if shape.xfer {
					assertOk(t,
						check.GetCntlrInfo().GetXferIdToNamespace()[testXfer],
						"the transfer namespace on the next check round")
				}
				if shape.settled {
					cnSweepAssertCode(t, check.GetAgentReply(), 0,
						"the next check round")
				}
			})
		}
	}
}

// TestUnansweredListingStillSweepsCloneChunks pins the other work a stopped
// sweep still does. The chunk files of a clone that left clone_list are local
// state, swept against the request alone, so an unanswered listing is no
// reason to leave them: they go in this pass, and a standby, which builds no
// clone device, has nothing left to remove, so the Check round after it reads
// OK. (A chunk file a pass did not remove would keep the verdict naming it:
// TestUnremovedChunkFileIsRedriven.)
func TestUnansweredListingStillSweepsCloneChunks(t *testing.T) {
	for _, listing := range cnSweepListings {
		t.Run(listing.name, func(t *testing.T) {
			ctx := context.Background()
			srv, node := newTestServer(t)
			// A standby that carries the clone in its desired state.
			syncupBoth(t, srv, reqOpts{
				revision: 2, primary: false, clones: []*pb.Clone{cloneOf()}})
			pushBitmap(t, srv, hexBytes(t, testSkipHex))
			bmPath := srv.nf.LocalCloneBmPath(
				testCluster, testCn, testSp, testClone, 0, 0)
			if _, ok := node.protos[bmPath]; !ok {
				t.Fatalf("fixture: the chunk was not persisted")
			}

			node.Reset()
			node.killCmd[listing.kill] = true
			reply, err := srv.SyncupCntlr(ctx,
				cntlrReq(reqOpts{revision: 3, primary: false}))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			cnSweepAssertCode(t, reply.GetAgentReply(),
				common.ReplyCodeLeftover, "the pass that drops the clone "+
					"over an unanswered listing")
			cnSweepAssertDetails(t, reply.GetAgentReply(),
				"enumeration failed: "+listing.name, "the failure details")
			if _, ok := node.protos[bmPath]; ok {
				t.Fatalf("the dropped clone's chunk file survived the pass")
			}
			if got := reply.GetBmInfoList(); len(got) != 0 {
				t.Fatalf("the reply still reports applied chunks: %v", got)
			}
			check, _ := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
				ClusterId: testCluster, CnId: testCn,
				CntlrPointer: cntlrPtr(), Revision: 3,
			}, nil)
			cnSweepAssertCode(t, check.GetAgentReply(), 0,
				"the next check round")
		})
	}
}

// TestUnansweredNamespaceListingRemovesNoNamespace pins the rule on the one
// listing the four do not cover: the sweep's own listing of the namespaces
// under a subsystem that stays. When it does not answer — a failure that
// stops no pass, the four listings having answered — L1 never learns of a
// namespace that left ns_list, and nothing else removes one: the build has
// no removal of its own (TestNamespaceRemovalIsTheChainsAlone). So that pass
// leaves the namespace as it is — no move to the inaccessible group, no
// disable, no rmdir — and its reply names the listing, which keeps the
// worker driving passes. The next pass whose listing answers finds the
// namespace, and L1 removes it, moved to inaccessible first, and once.
func TestUnansweredNamespaceListingRemovesNoNamespace(t *testing.T) {
	const secondNs = uint64(0x1b)
	twoNs := defaultSubsys(false)
	twoNs[testNqn].NsList = append(twoNs[testNqn].NsList, &pb.Namespace{
		NsId:     secondNs,
		NsIdx:    2,
		TdId:     testTd,
		DevUuid:  "22222222-2222-4222-8222-222222222222",
		DevNguid: "22222222222242228222222222222222",
	})
	ctx := context.Background()
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, raid1: true, subsys: twoNs})
	if got := node.files[anaPath(testNqn, 2)]; got != "1" {
		t.Fatalf("fixture: nsid 2's ana_grpid is %q, want 1", got)
	}
	nsPath := agent.NvmetRoot + "/subsystems/" + testNqn + "/namespaces/2"
	ana := "writedirect " + anaPath(testNqn, 2) + "="
	req := cntlrReq(reqOpts{revision: 3, primary: true, raid1: true})

	node.Reset()
	// One-shot: the pass's first listing of the subsystem's namespaces is
	// the sweep's.
	node.killCmd["ls -1 "+agent.NvmetRoot+"/subsystems/"+testNqn+
		"/namespaces"] = true
	reply, err := srv.SyncupCntlr(ctx, req)
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	cnSweepAssertCode(t, reply.GetAgentReply(), common.ReplyCodeLeftover,
		"the pass over an unanswered namespace listing")
	cnSweepAssertDetails(t, reply.GetAgentReply(),
		"enumeration failed: nvmet namespaces of "+testNqn,
		"the failure details")
	for _, listing := range cnSweepListings {
		if strings.Contains(reply.GetAgentReply().GetDetails(),
			"enumeration failed: "+listing.name+":") {
			t.Fatalf("fixture: the %s listing failed too, so the sweep "+
				"was stopped", listing.name)
		}
	}
	assertNoCall(t, node, ana)
	assertNoCall(t, node, "writedirect "+nsPath+"/enable=0")
	assertNoCall(t, node, "cmd rmdir "+nsPath)
	if !node.dirs[nsPath] {
		t.Fatalf("nsid 2 was removed by a pass that could not list it")
	}
	// The namespace that stays is left alone.
	assertNoCall(t, node, "writedirect "+anaPath(testNqn, 1)+"=")

	// The next pass, its listing answering: L1 finds nsid 2, with nothing
	// remembered from the pass that could not see it.
	node.Reset()
	reply, err = srv.SyncupCntlr(ctx, req)
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	cnSweepAssertCode(t, reply.GetAgentReply(), 0,
		"the pass whose listing answered")
	if node.dirs[nsPath] {
		t.Fatalf("nsid 2 survived the pass whose listing answered")
	}
	// Exactly one write, to 3, ahead of the removal: an ordering assertion
	// stops at its first match, so only the count shows that nothing moved
	// the group back later on.
	if moves := node.callsMatching(ana); len(moves) != 1 ||
		moves[0] != ana+"3" {
		t.Fatalf("nsid 2 ana_grpid writes %q, want exactly one, to 3", moves)
	}
	assertOrder(t, node, ana+"3",
		"writedirect "+nsPath+"/enable=0",
		"cmd rmdir "+nsPath)
	if n := len(node.callsMatching("cmd rmdir " + nsPath)); n != 1 {
		t.Fatalf("nsid 2 was removed %d times, want 1", n)
	}
	assertNoCall(t, node, "writedirect "+anaPath(testNqn, 1)+"=")
}

// TestUnansweredListingLeavesADroppedNamespaceToTheSweep pins what a stopped
// sweep leaves alone: a namespace that has left ns_list under a subsystem
// that stays. Its removal is L1's and nothing else's
// (TestNamespaceRemovalIsTheChainsAlone), and P0 comes first because
// disabling the namespace closes the ns-dev under it, which does not complete
// on a dm-suspended device — the state an older build's leftover is in here.
// A stopped pass runs neither, so nothing touches the namespace: the reply
// names it — or, when that is the listing that did not answer, the nvmet
// listing — and the next pass whose listings answer parks the ns-dev before
// it removes the namespace, once.
func TestUnansweredListingLeavesADroppedNamespaceToTheSweep(t *testing.T) {
	nsPath := agent.NvmetRoot + "/subsystems/" + testNqn + "/namespaces/1"
	for _, listing := range cnSweepListings {
		t.Run(listing.name, func(t *testing.T) {
			ctx := context.Background()
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{
				revision: 2, primary: true, suspended: true})
			// An older build's leftover: the ns-dev held dm-suspended, its
			// table still over the raid0.
			nsDev := node.dms[nsDevName(srv, testNs)]
			nsDev.table = agent.LinearTable(testTdSize/512,
				node.devNo["/dev/mapper/"+raid0Name(srv, testTd)], 0)
			nsDev.suspended = true
			// Inaccessible already, as the suspend left it: L1's move
			// finds nothing to write, and none of the passes below may
			// move it anywhere else.
			if got := node.files[anaPath(testNqn, 1)]; got != "3" {
				t.Fatalf("fixture: ana_grpid is %q, want 3", got)
			}
			ana := "writedirect " + anaPath(testNqn, 1) + "="
			noNs := defaultSubsys(false)
			noNs[testNqn].NsList = nil
			req := cntlrReq(reqOpts{revision: 3, primary: true, subsys: noNs})

			node.Reset()
			node.killCmd[listing.kill] = true
			reply, err := srv.SyncupCntlr(ctx, req)
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			cnSweepAssertCode(t, reply.GetAgentReply(),
				common.ReplyCodeLeftover, "the pass over an unanswered "+
					"listing")
			cnSweepAssertDetails(t, reply.GetAgentReply(),
				"enumeration failed: "+listing.name, "the failure details")
			if listing.name != "nvmet subsystems" {
				cnSweepAssertDetails(t, reply.GetAgentReply(),
					agent.LeftoverKindNvmetNs+":"+testNqn+"/1",
					"the leftover")
			}
			assertNoCall(t, node, ana)
			assertNoCall(t, node, "writedirect "+nsPath+"/enable=0")
			assertNoCall(t, node, "cmd rmdir "+nsPath)
			if !node.dirs[nsPath] {
				t.Fatalf("the namespace was removed on a stopped pass")
			}

			// The next pass, its listings answering: P0 before L1.
			node.Reset()
			if _, err := srv.SyncupCntlr(ctx, req); err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			assertOrder(t, node,
				"cmd dmsetup reload "+nsDevName(srv, testNs),
				"cmd dmsetup resume "+nsDevName(srv, testNs),
				"writedirect "+nsPath+"/enable=0",
				"cmd rmdir "+nsPath,
				"cmd dmsetup remove "+nsDevName(srv, testNs),
			)
			for _, move := range node.callsMatching(ana) {
				if move != ana+"3" {
					t.Fatalf("ana_grpid moved by %q before the removal",
						move)
				}
			}
			if n := len(node.callsMatching("cmd rmdir " + nsPath)); n != 1 {
				t.Fatalf("the namespace was removed %d times, want 1", n)
			}
		})
	}
}

// TestUnansweredListingKeepsTheNsDevOffALeavingClone pins a move the build
// must not make after a stopped sweep: an ns-dev onto the raid0 that a
// dm-clone which has left clone_list may still be hydrating into.
//
// The chain's order is what makes that move safe: the ns-dev parked (P2),
// the dm-clone removed (L3), and only then the build's reload onto the raid0.
// A stopped pass runs neither step, so the dm-clone stays loaded with its
// hydration enabled, and a region it has not copied yet would overwrite a
// host's write made to the raid0 directly. That is live whenever a clone
// still hydrating is deleted with force, in three shapes: a namespace serving
// through the dm-clone (auto_resume; once more with its ns-dev left
// dm-suspended by an older build, which is resumed where it is), one parked
// while the clone ran (auto_resume false — the delete's latch resumes it in
// the same revision), and a namespace new in that revision. On the stopped
// pass each stays off the raid0 and none is moved to the optimized group —
// the first serves on through the dm-clone, the other two are parked on the
// td's dm-error — and the pass after it, its listings answering, removes
// the dm-clone before any reload onto the raid0. A held ns-dev is not where
// its plan wants it, so the pass reports its row ERROR, in the words the
// Check probe uses for it, and registers the CN10 retry: the worker re-syncs
// on a revision or a reply code, never on a row.
func TestUnansweredListingKeepsTheNsDevOffALeavingClone(t *testing.T) {
	const newNs = uint64(0x1b)
	parkedClone := cloneOf()
	parkedClone.AutoResume = false
	withNewNs := defaultSubsys(false)
	withNewNs[testNqn].NsList = append(withNewNs[testNqn].NsList,
		&pb.Namespace{
			NsId:     newNs,
			NsIdx:    2,
			TdId:     testTd,
			DevUuid:  "22222222-2222-4222-8222-222222222222",
			DevNguid: "22222222222242228222222222222222",
		})
	// nsWant is one namespace after the stopped pass: the device its ns-dev
	// maps ("clone" or "error") and its ana_grpid.
	type nsWant struct {
		nsId  uint64
		nsIdx int
		on    string
		ana   string
	}
	for _, shape := range []struct {
		name string
		from reqOpts
		to   reqOpts
		// suspended leaves the ns-dev dm-suspended after the fixture, as
		// an older build held one for §11.6: kept where it is, it is still
		// resumed.
		suspended bool
		want      []nsWant
	}{
		{"serving through the clone",
			reqOpts{revision: 2, primary: true,
				clones: []*pb.Clone{cloneOf()}},
			reqOpts{revision: 3, primary: true}, false,
			[]nsWant{{testNs, 1, "clone", "1"}}},
		{"held suspended by an older build",
			reqOpts{revision: 2, primary: true,
				clones: []*pb.Clone{cloneOf()}},
			reqOpts{revision: 3, primary: true}, true,
			[]nsWant{{testNs, 1, "clone", "1"}}},
		{"parked while the clone ran",
			reqOpts{revision: 2, primary: true, suspended: true,
				clones: []*pb.Clone{parkedClone}},
			reqOpts{revision: 3, primary: true}, false,
			[]nsWant{{testNs, 1, "error", "3"}}},
		{"new in the same revision",
			reqOpts{revision: 2, primary: true,
				clones: []*pb.Clone{cloneOf()}},
			reqOpts{revision: 3, primary: true, subsys: withNewNs}, false,
			[]nsWant{{testNs, 1, "clone", "1"}, {newNs, 2, "error", "3"}}},
	} {
		for _, listing := range cnSweepListings {
			t.Run(shape.name+"/"+listing.name, func(t *testing.T) {
				ctx := context.Background()
				srv, node := newTestServer(t)
				srv.retryInterval = time.Hour
				syncupBoth(t, srv, shape.from)
				if shape.suspended {
					node.dms[nsDevName(srv, testNs)].suspended = true
				}
				clone := cloneName(srv, testClone)
				if dm := node.dms[clone]; dm == nil || dm.noHydration {
					t.Fatalf("fixture: no dm-clone with its hydration on")
				}
				devNoOf := func(name string) string {
					return node.devNo["/dev/mapper/"+name]
				}
				linear := func(devNo string) string {
					return agent.LinearTable(testTdSize/512, devNo, 0)
				}
				raid0No := devNoOf(raid0Name(srv, testTd))
				on := map[string]string{
					"clone": devNoOf(clone),
					"error": devNoOf(errorName(srv, testTd)),
				}

				node.Reset()
				node.killCmd[listing.kill] = true
				reply, err := srv.SyncupCntlr(ctx, cntlrReq(shape.to))
				if err != nil {
					t.Fatalf("SyncupCntlr: %v", err)
				}
				cnSweepAssertCode(t, reply.GetAgentReply(),
					common.ReplyCodeLeftover, "the pass over an unanswered "+
						"listing")
				cnSweepAssertDetails(t, reply.GetAgentReply(),
					"enumeration failed: "+listing.name, "the failure details")
				assertNoCall(t, node, "cmd dmsetup remove")
				if node.dms[clone] == nil {
					t.Fatalf("the dm-clone is gone after a stopped pass")
				}
				for _, ns := range shape.want {
					dev := node.dms[nsDevName(srv, ns.nsId)]
					if dev == nil {
						t.Fatalf("ns %#x has no ns-dev", ns.nsId)
					}
					if dev.table != linear(on[ns.on]) || dev.suspended {
						t.Fatalf("ns %#x's ns-dev is %q (suspended %v), "+
							"want it live on the %s: %q", ns.nsId, dev.table,
							dev.suspended, ns.on, linear(on[ns.on]))
					}
					got := node.files[anaPath(testNqn, ns.nsIdx)]
					if got != ns.ana {
						t.Fatalf("nsid %d's ana_grpid is %q, want %q",
							ns.nsIdx, got, ns.ana)
					}
					assertNoCall(t, node,
						"writedirect "+anaPath(testNqn, ns.nsIdx)+"=1")
					assertErrorDetails(t,
						reply.GetCntlrInfo().GetNsIdToDmLinear()[ns.nsId],
						detailsNsDevNotDesired, "the held ns-dev's row")
					assertErrorDetails(t,
						probedCntlrInfo(t, srv).GetNsIdToDmLinear()[ns.nsId],
						detailsNsDevNotDesired, "the held ns-dev's probe")
				}
				if !retrying(t, srv) {
					t.Fatalf("a pass that held an ns-dev registered no retry")
				}

				// The next pass, its listings answering: the dm-clone goes
				// first, and every ns-dev lands on the raid0 after it —
				// a namespace that was optimized over the dm-clone being
				// parked on the way (P2), one that was parked going
				// optimized only once its ns-dev is on the raid0.
				node.Reset()
				reply, err = srv.SyncupCntlr(ctx, cntlrReq(shape.to))
				if err != nil {
					t.Fatalf("SyncupCntlr: %v", err)
				}
				// The clone's source goes with it: its disconnect runs off
				// the pass (CN10), so the pass's only leftover is that
				// connection, and the pass after it is clean.
				cnSweepOnlyDisconnects(t, reply.GetAgentReply(),
					"the pass whose listings answer")
				for _, ns := range shape.want {
					reload := "cmd dmsetup reload " +
						nsDevName(srv, ns.nsId) + " --table "
					if ns.on == "clone" {
						assertOrder(t, node, reload+linear(on["error"]),
							"cmd dmsetup remove "+clone,
							reload+linear(raid0No))
					} else {
						assertOrder(t, node, "cmd dmsetup remove "+clone,
							reload+linear(raid0No),
							"writedirect "+anaPath(testNqn, ns.nsIdx)+"=1")
					}
					table := node.dms[nsDevName(srv, ns.nsId)].table
					if table != linear(raid0No) {
						t.Fatalf("ns %#x's ns-dev is %q after the pass, "+
							"want it on the raid0", ns.nsId, table)
					}
					got := node.files[anaPath(testNqn, ns.nsIdx)]
					if got != "1" {
						t.Fatalf("nsid %d's ana_grpid is %q after the "+
							"pass, want 1", ns.nsIdx, got)
					}
					assertOk(t,
						reply.GetCntlrInfo().GetNsIdToDmLinear()[ns.nsId],
						"the ns-dev's row after the pass")
				}
				if retrying(t, srv) {
					t.Fatalf("the pass that held nothing kept the retry")
				}
				awaitDisconnects(t, srv)
				reply, err = srv.SyncupCntlr(ctx, cntlrReq(shape.to))
				if err != nil {
					t.Fatalf("SyncupCntlr: %v", err)
				}
				cnSweepAssertCode(t, reply.GetAgentReply(), 0,
					"the pass after the disconnect")
			})
		}
	}
}

// TestUnansweredListingDoesNotParkAServingNamespace pins the rest of what a
// stopped sweep leaves out: CN9's pre-steps 2 and 3, which exist only so the
// layers below them can remove. The namespace here moves to another td while
// its old td leaves td_list. Run on this pass, P2 would park it on the new
// td's dm-error first — while it is still optimized, so its host takes IO
// errors — for the sake of a raid0 removal the stopped pass does not make.
// Without P2 the build moves it in one reload, straight onto the new td's
// raid0. The dm listing's own silence is the exception: with no snapshot to
// rule out a dm-clone still hydrating into a raid0, the build keeps the
// ns-dev where it is (TestUnansweredListingKeepsTheNsDevOffALeavingClone),
// and there is no reload at all.
func TestUnansweredListingDoesNotParkAServingNamespace(t *testing.T) {
	tds := []*pb.ThinDevice{
		{TdId: testTd, DevId: 1, Size: testTdSize},
		{TdId: testSnapTd, DevId: 2, Size: testTdSize},
	}
	subsys := defaultSubsys(false)
	subsys[testNqn].NsList[0].TdId = testSnapTd
	for _, listing := range cnSweepListings {
		t.Run(listing.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: tds})
			errNo := node.devNo["/dev/mapper/"+errorName(srv, testSnapTd)]
			raid0No := node.devNo["/dev/mapper/"+raid0Name(srv, testSnapTd)]
			if errNo == "" || raid0No == "" {
				t.Fatalf("fixture: the new td is not built")
			}

			node.Reset()
			node.killCmd[listing.kill] = true
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 3, primary: true, tds: tds[1:],
					subsys: subsys}))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			cnSweepAssertCode(t, reply.GetAgentReply(),
				common.ReplyCodeLeftover, "the pass over an unanswered "+
					"listing")
			cnSweepAssertDetails(t, reply.GetAgentReply(),
				"enumeration failed: "+listing.name, "the failure details")
			want := []string{"cmd dmsetup reload " + nsDevName(srv, testNs) +
				" --table " + agent.LinearTable(testTdSize/512, raid0No, 0)}
			if listing.name == "dmsetup ls" {
				want = nil
			}
			got := node.callsMatching(
				"cmd dmsetup reload " + nsDevName(srv, testNs))
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("ns-dev reloads %q, want %q (the td's dm-error "+
					"is %s)", got, want, errNo)
			}
			assertNoCall(t, node, "writedirect "+anaPath(testNqn, 1)+"=")
		})
	}
}

// TestALeftDmCloneKeepsTheNsDevOffItsRaid0 is the hold of
// TestUnansweredListingKeepsTheNsDevOffALeavingClone on a pass whose listings
// all answer: what keeps an ns-dev off the raid0 is a dm-clone the plan does
// not want that is still live after the chain, not how the pass learned it.
// Three routes leave it live: P2's park of the ns-dev over it fails its load
// (the ns-dev stays suspended on the dm-clone's table, so L3's remove fails
// EBUSY), L3's remove is killed before it acts, and the descent stops above
// L3 — here at L2, on a sibling namespace dropped in the same revision whose
// ns-dev will not go. On each the pass reloads nothing onto the raid0 and
// moves nothing to the optimized group. The ns-dev P2 did not park serves on
// through the dm-clone, still optimized; one it parked stays parked and goes
// inaccessible, so a host queues its IO rather than take errors on an
// optimized path. Its row reads ERROR, as the Check probe reads it, and the
// pass registers the CN10 retry. The pass after the fault, the dm-clone
// removed, puts the ns-dev on the raid0 and only then moves it optimized.
func TestALeftDmCloneKeepsTheNsDevOffItsRaid0(t *testing.T) {
	const sibling = uint64(0x1b)
	withSibling := defaultSubsys(false)
	withSibling[testNqn].NsList = append(withSibling[testNqn].NsList,
		&pb.Namespace{
			NsId:     sibling,
			NsIdx:    2,
			TdId:     testTd,
			DevUuid:  "22222222-2222-4222-8222-222222222222",
			DevNguid: "22222222222242228222222222222222",
		})
	linear := func(devNo string) string {
		return agent.LinearTable(testTdSize/512, devNo, 0)
	}
	for _, route := range []struct {
		name string
		from reqOpts
		// fault arms the fault and returns what disarms it.
		fault func(srv *CnAgentServer, node *fakeNode) func()
		// on is where the pass leaves the ns-dev ("clone" or "error"), and
		// ana its ana_grpid.
		on  string
		ana string
	}{
		{"the park's load fails",
			reqOpts{revision: 2, primary: true,
				clones: []*pb.Clone{cloneOf()}},
			func(srv *CnAgentServer, node *fakeNode) func() {
				errNo := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
				node.failCmd["dmsetup reload "+nsDevName(srv, testNs)+
					" --table "+linear(errNo)] = "device busy"
				return func() {}
			},
			"clone", "1"},
		{"the dm-clone's remove is killed before it acts",
			reqOpts{revision: 2, primary: true,
				clones: []*pb.Clone{cloneOf()}},
			func(srv *CnAgentServer, node *fakeNode) func() {
				key := "dmsetup remove " + cloneName(srv, testClone)
				node.killCmdNoEffectAlways[key] = true
				return func() { delete(node.killCmdNoEffectAlways, key) }
			},
			"error", "3"},
		{"the descent stops above the dm-clone",
			reqOpts{revision: 2, primary: true, subsys: withSibling,
				clones: []*pb.Clone{cloneOf()}},
			func(srv *CnAgentServer, node *fakeNode) func() {
				key := "dmsetup remove " + nsDevName(srv, sibling)
				node.failCmdAlways[key] = "device busy"
				return func() { delete(node.failCmdAlways, key) }
			},
			"error", "3"},
	} {
		t.Run(route.name, func(t *testing.T) {
			ctx := context.Background()
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			syncupBoth(t, srv, route.from)
			clone := cloneName(srv, testClone)
			if dm := node.dms[clone]; dm == nil || dm.noHydration {
				t.Fatalf("fixture: no dm-clone with its hydration on")
			}
			devNoOf := func(name string) string {
				return node.devNo["/dev/mapper/"+name]
			}
			raid0No := devNoOf(raid0Name(srv, testTd))
			on := map[string]string{
				"clone": devNoOf(clone),
				"error": devNoOf(errorName(srv, testTd)),
			}
			nsDev := nsDevName(srv, testNs)
			toRaid0 := "cmd dmsetup reload " + nsDev + " --table " +
				linear(raid0No)
			ana := anaPath(testNqn, 1)
			req := cntlrReq(reqOpts{revision: 3, primary: true})

			node.Reset()
			disarm := route.fault(srv, node)
			reply, err := srv.SyncupCntlr(ctx, req)
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			cnSweepAssertCode(t, reply.GetAgentReply(),
				common.ReplyCodeLeftover, "the pass the dm-clone outlives")
			cnSweepAssertDetails(t, reply.GetAgentReply(),
				agent.LeftoverKindDm+":"+clone, "the leftover details")
			if dm := node.dms[clone]; dm == nil || dm.noHydration {
				t.Fatalf("the route is vacuous: the dm-clone is gone or "+
					"not hydrating (%v)", dm)
			}
			if n := len(node.callsMatching(toRaid0)); n != 0 {
				t.Fatalf("the ns-dev was reloaded onto the raid0 %d times "+
					"under a live dm-clone, want 0", n)
			}
			dev := node.dms[nsDev]
			if dev == nil || dev.table != linear(on[route.on]) ||
				dev.suspended {
				t.Fatalf("the ns-dev is %+v, want it live on the %s: %q",
					dev, route.on, linear(on[route.on]))
			}
			if got := node.files[ana]; got != route.ana {
				t.Fatalf("ana_grpid is %q, want %q", got, route.ana)
			}
			assertNoCall(t, node, "writedirect "+ana+"=1")
			assertErrorDetails(t,
				reply.GetCntlrInfo().GetNsIdToDmLinear()[testNs],
				detailsNsDevNotDesired, "the held ns-dev's row")
			assertErrorDetails(t,
				probedCntlrInfo(t, srv).GetNsIdToDmLinear()[testNs],
				detailsNsDevNotDesired, "the held ns-dev's probe")
			if !retrying(t, srv) {
				t.Fatalf("a pass that held an ns-dev registered no retry")
			}

			// The fault gone: the dm-clone goes first, the reload onto the
			// raid0 follows it, and the optimized move comes last.
			disarm()
			node.Reset()
			reply, err = srv.SyncupCntlr(ctx, req)
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			cnSweepOnlyDisconnects(t, reply.GetAgentReply(),
				"the pass after the fault")
			if route.on == "clone" {
				assertOrder(t, node, "cmd dmsetup reload "+nsDev+
					" --table "+linear(on["error"]),
					"cmd dmsetup remove "+clone, toRaid0)
				assertNoCall(t, node, "writedirect "+ana+"=")
			} else {
				assertOrder(t, node, "cmd dmsetup remove "+clone, toRaid0,
					"writedirect "+ana+"=1")
			}
			if n := len(node.callsMatching(toRaid0)); n != 1 {
				t.Fatalf("the ns-dev was reloaded onto the raid0 %d times, "+
					"want 1", n)
			}
			if got := node.dms[nsDev].table; got != linear(raid0No) {
				t.Fatalf("the ns-dev is %q after the pass, want it on the "+
					"raid0", got)
			}
			if got := node.files[ana]; got != "1" {
				t.Fatalf("ana_grpid is %q after the pass, want 1", got)
			}
			assertOk(t, reply.GetCntlrInfo().GetNsIdToDmLinear()[testNs],
				"the ns-dev's row after the pass")
			if retrying(t, srv) {
				t.Fatalf("the pass that held nothing kept the retry")
			}
		})
	}
}

// TestAHeldNsDevIsRedrivenWithoutARevision pins the other half of the hold:
// with no dm-clone at all, an unanswered `dmsetup ls` holds an ns-dev off
// its raid0 too — nothing can rule out a dm-clone hydrating into it — and
// here nothing is left for the Check verdict to report once the listing
// answers again. A standby promoted to primary, and a namespace resumed, each
// keep their ns-dev parked and inaccessible on that pass. The worker re-syncs
// on a revision or a reply code, never on a row, so without the CN10 retry
// the namespace would stay unserved until the next revision. The pass reports
// the ns-dev's row ERROR, as the probe does, and registers the retry, whose
// one attempt puts the ns-dev on the raid0 and moves it optimized, each
// exactly once, and then stops.
func TestAHeldNsDevIsRedrivenWithoutARevision(t *testing.T) {
	linear := func(devNo string) string {
		return agent.LinearTable(testTdSize/512, devNo, 0)
	}
	for _, shape := range []struct {
		name string
		from reqOpts
	}{
		{"a standby promoted", reqOpts{revision: 2, primary: false}},
		{"a namespace resumed",
			reqOpts{revision: 2, primary: true, suspended: true}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			ctx := context.Background()
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			syncupBoth(t, srv, shape.from)
			nsDev := nsDevName(srv, testNs)
			ana := anaPath(testNqn, 1)

			node.Reset()
			node.killCmd["dmsetup ls"] = true
			reply, err := srv.SyncupCntlr(ctx,
				cntlrReq(reqOpts{revision: 3, primary: true}))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			cnSweepAssertCode(t, reply.GetAgentReply(),
				common.ReplyCodeLeftover, "the pass over an unanswered "+
					"listing")
			errNo := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
			raid0No := node.devNo["/dev/mapper/"+raid0Name(srv, testTd)]
			if raid0No == "" {
				t.Fatalf("the pass built no raid0")
			}
			if got := node.dms[nsDev].table; got != linear(errNo) {
				t.Fatalf("the ns-dev is %q, want it held on the dm-error",
					got)
			}
			if got := node.files[ana]; got != "3" {
				t.Fatalf("ana_grpid is %q, want 3", got)
			}
			assertErrorDetails(t,
				reply.GetCntlrInfo().GetNsIdToDmLinear()[testNs],
				detailsNsDevNotDesired, "the held ns-dev's row")
			assertErrorDetails(t,
				probedCntlrInfo(t, srv).GetNsIdToDmLinear()[testNs],
				detailsNsDevNotDesired, "the held ns-dev's probe")
			if !retrying(t, srv) {
				t.Fatalf("a pass that held an ns-dev registered no retry")
			}

			node.Reset()
			retryAttempt(t, srv)
			toRaid0 := node.callsMatching("cmd dmsetup reload " + nsDev +
				" --table " + linear(raid0No))
			optimized := node.callsMatching("writedirect " + ana + "=1")
			if len(toRaid0) != 1 || len(optimized) != 1 {
				t.Fatalf("the retry made %d reloads onto the raid0 and %d "+
					"optimized moves, want 1 and 1", len(toRaid0),
					len(optimized))
			}
			assertOrder(t, node, toRaid0[0], optimized[0])
			if got := node.files[ana]; got != "1" {
				t.Fatalf("ana_grpid is %q after the retry, want 1", got)
			}
			assertOk(t,
				probedCntlrInfo(t, srv).GetNsIdToDmLinear()[testNs],
				"the ns-dev's probe after the retry")
			if retrying(t, srv) {
				t.Fatalf("the attempt that held nothing kept the retry")
			}
		})
	}
}

// TestUnremovedChunkFileIsRedriven pins the chunk index against a removal
// that did not happen. The files of a clone that left clone_list go with one
// `rm -f`; killed before it did anything, it leaves them on disk. The clone's
// entry in the index stays, so the pass names each file as a leftover record,
// and so does the Check verdict after it — which is what makes the worker
// re-drive — and the re-driven pass removes the file. Forgetting the entry
// first would leave nothing but a restart that ever lists the file again,
// and every reply would read OK. The same holds on a pass an unanswered
// listing stopped.
func TestUnremovedChunkFileIsRedriven(t *testing.T) {
	for _, kill := range []string{"", "dmsetup ls"} {
		name := "listings answer"
		if kill != "" {
			name = kill + " killed"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			srv, node := newTestServer(t)
			// A standby that carries the clone in its desired state.
			syncupBoth(t, srv, reqOpts{
				revision: 2, primary: false, clones: []*pb.Clone{cloneOf()}})
			pushBitmap(t, srv, hexBytes(t, testSkipHex))
			bmPath := srv.nf.LocalCloneBmPath(
				testCluster, testCn, testSp, testClone, 0, 0)
			if _, ok := node.protos[bmPath]; !ok {
				t.Fatalf("fixture: the chunk was not persisted")
			}
			record := agent.LeftoverKindRecord + ":" + bmPath
			req := cntlrReq(reqOpts{revision: 3, primary: false})
			checkRound := func() *pb.AgentReply {
				check, _ := srv.checkCntlrRound(ctx, &pb.CheckCntlrRequest{
					ClusterId: testCluster, CnId: testCn,
					CntlrPointer: cntlrPtr(), Revision: 3,
				}, nil)
				return check.GetAgentReply()
			}

			node.Reset()
			if kill != "" {
				node.killCmd[kill] = true
			}
			node.killCmdNoEffect["rm -f "+bmPath] = true
			reply, err := srv.SyncupCntlr(ctx, req)
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if _, ok := node.protos[bmPath]; !ok {
				t.Fatalf("fixture: the killed rm removed the chunk file")
			}
			cnSweepAssertCode(t, reply.GetAgentReply(),
				common.ReplyCodeLeftover, "the pass whose rm was killed")
			cnSweepAssertDetails(t, reply.GetAgentReply(), record,
				"the leftover")
			check := checkRound()
			cnSweepAssertCode(t, check, common.ReplyCodeLeftover,
				"the check round after it")
			cnSweepAssertDetails(t, check, record, "the verdict")

			// The worker's re-drive.
			node.Reset()
			reply, err = srv.SyncupCntlr(ctx, req)
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			cnSweepAssertCode(t, reply.GetAgentReply(), 0, "the re-drive")
			if _, ok := node.protos[bmPath]; ok {
				t.Fatalf("the chunk file survived the re-drive")
			}
			if rms := node.callsMatching("cmd rm -f " + bmPath); len(rms) != 1 {
				t.Fatalf("the re-drive issued %d rm of the chunk file, "+
					"want 1: %q", len(rms), rms)
			}
			cnSweepAssertCode(t, checkRound(), 0,
				"the check round after the re-drive")
		})
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
