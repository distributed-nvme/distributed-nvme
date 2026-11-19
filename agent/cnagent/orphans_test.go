package cnagent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// otherHostNqn is the host NQN of a host stack on the controller node's own
// machine, such as the CSI node plugin's.
const otherHostNqn = "nqn.2014-08.org.nvmexpress:uuid:host-stack"

// ctrlOfHost is the controller of nqn the fake holds for one host NQN.
func ctrlOfHost(t *testing.T, node *fakeNode, nqn, hostNqn string) string {
	t.Helper()
	node.mu.Lock()
	defer node.mu.Unlock()
	subsys := node.subsystems[nqn]
	if subsys == nil {
		return ""
	}
	for _, ctrl := range subsys.ctrls {
		if ctrl.hostNqn == hostNqn {
			return ctrl.name
		}
	}
	return ""
}

// TestACloneSourceOfAnotherKindGoesAtNodeLevel pins the attribution of a
// clone source whose NQN is no transfer of this cluster (cnagent.md CN21):
// another pool's subsystem, whose NQN its user chose, or a transfer of
// another cluster. Its NQN names no owner here, so the node-level pass
// judges it by the host NQN its controllers were opened with: once no held
// request names it and no live dm-clone maps it, the node pass disconnects
// this cn's controllers of it, by device and never by NQN, so a controller
// of the same subsystem a host stack on the machine opened with its own host
// NQN stays. A cntlr's pass judges none of this: it removes the clone's
// dm-clone and clone meta group and leaves the source, and its reads read no
// host NQN. TestCloneTeardownOrder is the transfer control.
func TestACloneSourceOfAnotherKindGoesAtNodeLevel(t *testing.T) {
	for _, tc := range []struct {
		name string
		nqn  func(srv *CnAgentServer) string
	}{
		{"another pool's subsystem", func(*CnAgentServer) string {
			return "nqn.2024-01.io.dnv-it:s:another-pool"
		}},
		{"a transfer of another cluster", func(srv *CnAgentServer) string {
			return srv.nf.XferNqn(testCluster+1, testSp, 0xc)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			srv, node := newTestServer(t)
			src := tc.nqn(srv)
			clone := cloneOf()
			clone.SrcNqn = src
			syncupBoth(t, srv, reqOpts{
				revision: 2, primary: true, clones: []*pb.Clone{clone}})
			ours := ctrlOfHost(t, node, src,
				srv.nf.CnHostNqn(testCluster, testCn))
			if ours == "" {
				t.Fatalf("the clone's source %s was never connected with "+
					"the cn's host NQN", src)
			}
			// A host stack on the same machine holds the same subsystem.
			if err := srv.host.Connect(ctx, agent.TrConf{TrType: "tcp",
				AdrFam: "ipv4", TrAddr: testIp2, TrSvcId: testSvcId2},
				src, otherHostNqn,
				common.DefaultNvmeFastIoFailTmo); err != nil {
				t.Fatalf("connecting the host stack: %v", err)
			}
			theirs := ctrlOfHost(t, node, src, otherHostNqn)
			if theirs == "" {
				t.Fatalf("the host stack's controller is missing")
			}

			// The cntlr passes: the clone goes, its source stays, and no
			// host NQN is read.
			node.Reset()
			syncupCntlrUntilClean(t, srv, reqOpts{revision: 3, primary: true})
			refreshTestCntlr(t, srv)
			for _, name := range []string{cloneName(srv, testClone),
				grpName(srv, testCloneGrp), legName(srv, testCloneLeg)} {
				if _, ok := node.dms[name]; ok {
					t.Fatalf("%s survived the clone", name)
				}
			}
			assertNoCall(t, node, "cmd nvme disconnect --nqn "+src)
			assertNoCall(t, node, "cmd nvme disconnect --device")
			assertNoCall(t, node, "/hostnqn")

			// The node's read names it.
			reply := checkCnOnce(t, srv)
			cnSweepAssertDetails(t, reply.GetAgentReply(),
				agent.LeftoverKindNvme+":"+src, "the node read")

			// The node pass disconnects our controller, by its device.
			node.Reset()
			cn := cnSweepSyncup(t, srv, 3, true)
			cnSweepOnlyDisconnects(t, cn.GetAgentReply(),
				"the node pass that disconnects")
			awaitDisconnects(t, srv)
			cn = cnSweepSyncup(t, srv, 3, true)
			cnSweepAssertCode(t, cn.GetAgentReply(), 0, "the next node pass")
			if !node.hasCall("cmd nvme disconnect --device " + ours) {
				t.Fatalf("our controller %s was never disconnected:\n%s",
					ours, strings.Join(node.callsMatching("disconnect"), "\n"))
			}
			assertNoCall(t, node, "cmd nvme disconnect --nqn")
			assertNoCall(t, node, "cmd nvme disconnect --device "+theirs)
			if ctrlOfHost(t, node, src, otherHostNqn) != theirs {
				t.Fatalf("the host stack's controller %s was taken", theirs)
			}
			if ctrlOfHost(t, node, src,
				srv.nf.CnHostNqn(testCluster, testCn)) != "" {
				t.Fatalf("our controller of %s survived", src)
			}
		})
	}
}

// A clone build of a source whose per-controller disconnect is in flight
// neither adopts the controller about to go nor connects the source: the
// disconnect is in CN10's registry under the source's NQN, as a disconnect
// by NQN is. Once it has returned, a later converge connects the source
// afresh.
func TestACloneBuildWaitsForASourceDisconnectByController(t *testing.T) {
	const src = "nqn.2024-01.io.dnv-it:s:another-pool"
	srv, node := newTestServer(t)
	clone := cloneOf()
	clone.SrcNqn = src
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{clone}})
	ours := ctrlOfHost(t, node, src, srv.nf.CnHostNqn(testCluster, testCn))
	syncupCntlrUntilClean(t, srv, reqOpts{revision: 3, primary: true})

	key := "nvme disconnect --device " + ours
	node.blockCmd(key)
	t.Cleanup(func() { node.releaseCmd(key) })
	cn := cnSweepSyncup(t, srv, 3, true)
	cnSweepOnlyDisconnects(t, cn.GetAgentReply(), "the node pass")
	if !srv.disconnectInFlight(src) {
		t.Fatalf("the source's disconnect is not in the registry")
	}

	// A new clone of the same source, while the disconnect runs.
	again := cloneOfTd(testClone+1, src, testTd)
	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 4, primary: true, clones: []*pb.Clone{again}}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	target := reply.GetCntlrInfo().GetCloneIdToTarget()[testClone+1]
	if target.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(target.GetDetails(),
			detailsDisconnectInFlight(src)) {
		t.Fatalf("the new clone's source row is %v/%q, want the disconnect "+
			"in flight", target.GetStatus(), target.GetDetails())
	}
	for _, call := range node.callsMatching("cmd nvme connect ") {
		if strings.Contains(call, "--nqn "+src+" ") {
			t.Fatalf("the source was connected while its disconnect ran: %q",
				call)
		}
	}
	if _, ok := node.dms[cloneName(srv, testClone+1)]; ok {
		t.Fatalf("a dm-clone was built on a source being disconnected")
	}

	node.releaseCmd(key)
	awaitDisconnects(t, srv)
	syncupCntlrUntilClean(t, srv, reqOpts{
		revision: 4, primary: true, clones: []*pb.Clone{again}})
	fresh := ctrlOfHost(t, node, src, srv.nf.CnHostNqn(testCluster, testCn))
	if fresh == "" || fresh == ours {
		t.Fatalf("the source was not connected afresh: %q (was %q)",
			fresh, ours)
	}
	if _, ok := node.dms[cloneName(srv, testClone+1)]; !ok {
		t.Fatalf("the new clone's dm-clone was never built")
	}
}

// A clone source of another kind that a held request still names, or that a
// live dm-clone still maps, is in use and stays; one that only a host stack
// holds, with no controller of this cn's host NQN, is never ours. The node
// pass reads a host NQN only for a connection no held request names.
func TestAForeignConnectionInUseOrNotOursStays(t *testing.T) {
	const named = "nqn.2024-01.io.dnv-it:s:named"
	const mapped = "nqn.2024-01.io.dnv-it:s:mapped"
	const theirs = "nqn.2024-01.io.dnv-it:s:theirs"
	srv, node := newTestServer(t)
	clone := cloneOf()
	clone.SrcNqn = named
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{clone}})
	connectStray(t, srv, mapped)
	if err := srv.host.Connect(context.Background(), agent.TrConf{
		TrType: "tcp", AdrFam: "ipv4", TrAddr: testIp2, TrSvcId: testSvcId2},
		theirs, otherHostNqn, common.DefaultNvmeFastIoFailTmo); err != nil {
		t.Fatalf("connecting %s: %v", theirs, err)
	}
	// The live dm-clone is re-pointed at mapped, so only the request keeps
	// named.
	dmClone := node.dms[cloneName(srv, testClone)]
	fields := strings.Fields(dmClone.table)
	fields[5] = connectedNsDevNo(t, srv, node, mapped)
	dmClone.table = strings.Join(fields, " ")

	node.Reset()
	reply := cnSweepSyncup(t, srv, 3, true)
	cnSweepAssertCode(t, reply.GetAgentReply(), 0, "SyncupCn")
	for _, nqn := range []string{named, mapped, theirs} {
		if _, ok := node.subsystems[nqn]; !ok {
			t.Fatalf("%s was disconnected", nqn)
		}
	}
	assertNoCall(t, node, "cmd nvme disconnect")
	// The walk reads every entry's subsysnqn; nothing more of the entry a
	// held request names is read.
	namedDir := fmt.Sprintf("%s/nvme-subsys%d", sysfsNvmeSubsysDir,
		node.subsystems[named].idx)
	for _, call := range node.callsMatching(namedDir) {
		if call != "read "+namedDir+"/subsysnqn" {
			t.Fatalf("the node pass read %q of a source a request names",
				call)
		}
	}
}

// TestAClaimedUndecodableDnvNqnLosesADroppedNamespace pins the claim half of
// the rule for an NQN that carries dnv's prefix and decodes to nothing
// (cnagent.md CN21): a held request that names it makes it its pool's, so
// the namespace step removes a namespace that left that request, moved to
// the inaccessible group first. TestUndecodableDnvNqnLeftAlone is the other
// half: unclaimed, such a subsystem is never touched.
func TestAClaimedUndecodableDnvNqnLosesADroppedNamespace(t *testing.T) {
	ctx := context.Background()
	undecodable := common.NqnPrefix + ":9:" + strings.Repeat("0", 15) + "1"
	if _, ok := common.ParseNqn(undecodable); ok {
		t.Fatalf("%s decodes, so the fixture proves nothing", undecodable)
	}
	const secondNs = uint64(0x1b)
	twoNs := defaultSubsys(false)
	twoNs[undecodable] = twoNs[testNqn]
	delete(twoNs, testNqn)
	twoNs[undecodable].NsList = append(twoNs[undecodable].NsList,
		&pb.Namespace{
			NsId:     secondNs,
			NsIdx:    2,
			TdId:     testTd,
			DevUuid:  "22222222-2222-4222-8222-222222222222",
			DevNguid: "22222222222242228222222222222222",
		})
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, subsys: twoNs})
	nsPath := agent.NvmetRoot + "/subsystems/" + undecodable + "/namespaces/2"
	if !node.dirs[nsPath] {
		t.Fatalf("fixture: nsid 2 of %s was never built", undecodable)
	}

	oneNs := defaultSubsys(false)
	oneNs[undecodable] = oneNs[testNqn]
	delete(oneNs, testNqn)
	node.Reset()
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 3, primary: true, subsys: oneNs}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	cnSweepAssertCode(t, reply.GetAgentReply(), 0, "SyncupCntlr")
	if node.dirs[nsPath] {
		t.Fatalf("the dropped namespace of the claimed subsystem survived")
	}
	assertOrder(t, node,
		"writedirect "+anaPath(undecodable, 2)+"=3",
		"writedirect "+nsPath+"/enable=0",
		"cmd rmdir "+nsPath)
	if !subsysDirPresent(node, undecodable) {
		t.Fatalf("the claimed subsystem itself was removed")
	}
	if !node.dirs[agent.NvmetRoot+"/subsystems/"+undecodable+
		"/namespaces/1"] {
		t.Fatalf("the namespace the request keeps was removed")
	}
}

// TestTheNodeSweepWalksTheHostOnce pins the cost of the checks that removed
// connections are gone (cnagent.md CN10, CN21): a pass lists
// /sys/class/nvme-subsystem once, in its walk, however many connections it
// checks, and each check reads its own entry of that walk again.
func TestTheNodeSweepWalksTheHostOnce(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, raid1: true,
		twoLegs: true})
	cnSweepForget(t, srv, node)

	node.Reset()
	reply := cnSweepSyncup(t, srv, 3, false)
	cnSweepOnlyDisconnects(t, reply.GetAgentReply(), "the teardown pass")
	awaitDisconnects(t, srv)
	if n := len(node.callsMatching("cmd nvme disconnect ")); n < 3 {
		t.Fatalf("the pass set %d disconnects going, want 3 or more, so the "+
			"count below proves something", n)
	}
	walks := 0
	for _, call := range node.Calls() {
		if call == "list "+sysfsNvmeSubsysDir {
			walks++
		}
	}
	if walks != 1 {
		t.Fatalf("the pass listed %s %d times, want once",
			sysfsNvmeSubsysDir, walks)
	}
}

// TestAReusedEntryNumberIsNotTheConnection pins the read again behind the
// one walk: the kernel gives a freed entry's number to the next new
// subsystem, so a check that trusted the walk's entry could read another
// connection's controllers as the one it checks, and disconnect it or hold
// the descent on it. Here a leg's subsystem goes right after the walk read
// its entry, and another subsystem takes the number: the check reads the
// entry's subsysnqn again, finds the other NQN, and the leg's connection
// reads gone; the other is left alone.
func TestAReusedEntryNumberIsNotTheConnection(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	cnSweepForget(t, srv, node)
	nqn := legNqn(srv, testDataLeg)
	other := names(srv).SideToCnNqn(testCluster, testSp+1, 0x97, testCn+1)
	idx := node.subsystems[nqn].idx
	entry := fmt.Sprintf("%s/nvme-subsys%d/subsysnqn", sysfsNvmeSubsysDir, idx)

	node.mu.Lock()
	node.afterRead[entry] = func() {
		dropSubsysLocked(node, nqn)
		node.plantSubsys(idx, other, endpoint{testIp2, testSvcId2})
	}
	node.mu.Unlock()
	node.Reset()
	reply := cnSweepSyncup(t, srv, 3, false)
	awaitDisconnects(t, srv)
	if _, ok := node.subsystems[other]; !ok {
		t.Fatalf("the subsystem that took the number was disconnected")
	}
	assertNoCall(t, node, "cmd nvme disconnect --nqn "+other)
	assertNoCall(t, node, "cmd nvme disconnect --nqn "+nqn)
	if strings.Contains(reply.GetAgentReply().GetDetails(),
		agent.LeftoverKindNvme+":"+nqn) {
		t.Fatalf("the leg's connection, gone, was named: %v",
			reply.GetAgentReply())
	}
}

// TestADisconnectByControllerReadsTheControllerAgain pins the read before
// each `nvme disconnect --device` of a clone source judged by its host NQN
// (CN21): the goroutine can run long after the pass read the controllers'
// names, and the kernel gives a freed controller's number to the next
// controller connected. Here the source has two controllers of this cn's
// host NQN; while the disconnect waits for a slot, the second one goes and
// a leg of another subsystem takes its name. The disconnect takes the first
// controller and passes over the name that is no longer the source's, so
// the other subsystem keeps its controller.
func TestADisconnectByControllerReadsTheControllerAgain(t *testing.T) {
	const src = "nqn.2024-01.io.dnv-it:s:another-pool"
	ctx := context.Background()
	srv, node := newTestServer(t)
	clone := cloneOf()
	clone.SrcNqn = src
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{clone}})
	hostNqn := srv.nf.CnHostNqn(testCluster, testCn)
	if err := srv.host.Connect(ctx, agent.TrConf{TrType: "tcp",
		AdrFam: "ipv4", TrAddr: testIp, TrSvcId: testSvcId},
		src, hostNqn, common.DefaultNvmeFastIoFailTmo); err != nil {
		t.Fatalf("connecting a second controller of %s: %v", src, err)
	}
	syncupCntlrUntilClean(t, srv, reqOpts{revision: 3, primary: true})
	node.mu.Lock()
	var ours []string
	for _, ctrl := range node.subsystems[src].ctrls {
		if ctrl.hostNqn == hostNqn {
			ours = append(ours, ctrl.name)
		}
	}
	node.mu.Unlock()
	if len(ours) != 2 {
		t.Fatalf("fixture: %d controllers of %s with our host NQN, want 2",
			len(ours), src)
	}

	// One slot, held, so the disconnect waits.
	srv.disconnectSlots = make(chan struct{}, 1)
	srv.disconnectSlots <- struct{}{}
	node.Reset()
	cnSweepOnlyDisconnects(t, cnSweepSyncup(t, srv, 3, true).GetAgentReply(),
		"the node pass")
	if !srv.disconnectInFlight(src) {
		t.Fatalf("the source's disconnect is not in flight")
	}
	// The second controller goes, and a leg of another subsystem takes its
	// name.
	other := names(srv).SideToCnNqn(testCluster, testSp+1, 0x97, testCn)
	node.mu.Lock()
	subsys := node.subsystems[src]
	for _, ctrl := range append([]*fakeCtrl(nil), subsys.ctrls...) {
		if ctrl.name == ours[1] {
			node.dropCtrl(subsys, ctrl)
		}
	}
	saved := node.nextCtrl
	node.nextCtrl, _ = strconv.Atoi(strings.TrimPrefix(ours[1], "nvme"))
	node.plantSubsys(node.nextSubsys, other, endpoint{testIp2, testSvcId2})
	node.nextCtrl = saved
	node.mu.Unlock()
	if got := node.subsystems[other].ctrls[0].name; got != ours[1] {
		t.Fatalf("fixture: the other subsystem's controller is %s, want %s",
			got, ours[1])
	}

	<-srv.disconnectSlots
	awaitDisconnects(t, srv)
	if !node.hasCall("cmd nvme disconnect --device " + ours[0]) {
		t.Fatalf("our first controller %s was never disconnected", ours[0])
	}
	assertNoCall(t, node, "cmd nvme disconnect --device "+ours[1])
	if len(node.subsystems[other].ctrls) != 1 {
		t.Fatalf("the controller that took the name %s was disconnected",
			ours[1])
	}
}

// TestAReusedNamespaceIdIsNotDisabledOverASuspendedDevice pins the build's
// half of the park step's stop (CN16, CN21): a request that gives a
// namespace id of a subsystem that stays to a new namespace leaves the live
// namespace, wanted by its id, over the old namespace's ns-dev, which the
// sweep parks as unwanted. When that park fails, the ns-dev stays
// suspended and the chain stops before L1; the build must not disable that
// namespace either, as the disable would wait, unkillable, for the IO the
// suspended ns-dev holds. The namespace's step fails instead and its row
// names it, and the re-send, the park going through, rebuilds the namespace
// over its new ns-dev.
func TestAReusedNamespaceIdIsNotDisabledOverASuspendedDevice(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	oldDev := nsDevName(srv, testNs)
	nsPath := agent.NvmetRoot + "/subsystems/" + testNqn + "/namespaces/1"
	sub := defaultSubsys(false)
	sub[testNqn].NsList[0].NsId = testNs + 0x100
	newDev := nsDevName(srv, testNs+0x100)

	node.Reset()
	node.mu.Lock()
	node.failCmdAlways["dmsetup reload "+oldDev] =
		"device-mapper: reload ioctl on " + oldDev +
			" failed: Cannot allocate memory"
	node.mu.Unlock()
	reply := syncupCntlrReply(t, srv, reqOpts{revision: 3, primary: true,
		subsys: sub})
	cnSweepAssertCode(t, reply, common.ReplyCodeLeftover, "the stopped pass")
	if !node.dms[oldDev].suspended {
		t.Fatalf("fixture: the old ns-dev is not suspended")
	}
	assertNoCall(t, node, "writedirect "+nsPath+"/enable=0")

	node.mu.Lock()
	node.failCmdAlways = make(map[string]string)
	node.mu.Unlock()
	node.Reset()
	syncupCntlrUntilClean(t, srv, reqOpts{revision: 3, primary: true,
		subsys: sub})
	if got := strings.TrimSpace(node.fileOf(nsPath + "/device_path")); got !=
		"/dev/mapper/"+newDev {
		t.Fatalf("the namespace is over %q, want its new ns-dev %s", got,
			newDev)
	}
	if _, ok := node.dms[oldDev]; ok {
		t.Fatalf("the old ns-dev survived")
	}
}

// TestTheNodePassReadsNoDnvConnectionItNeverOpens pins what the node pass
// judges by host NQN (CN21): a connection whose NQN is in no dnv namespace,
// or a transfer of another cluster. A migration source a co-hosted dn agent
// connects, and a connection in the dnv namespace whose NQN decodes to
// nothing — a newer build's, after a rollback — cost no read and are never
// disconnected, even with this cn's host NQN.
func TestTheNodePassReadsNoDnvConnectionItNeverOpens(t *testing.T) {
	ctx := context.Background()
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	migr := srv.nf.MigrSrcNqn(testCluster, 0x55, testSp, 0x66)
	undecodable := common.NqnPrefix + ":9:" + strings.Repeat("0", 15) + "1"
	tr := agent.TrConf{TrType: "tcp", AdrFam: "ipv4", TrAddr: testIp2,
		TrSvcId: testSvcId2}
	if err := srv.host.Connect(ctx, tr, migr,
		srv.nf.DnHostNqn(testCluster, 0x77),
		common.DefaultNvmeFastIoFailTmo); err != nil {
		t.Fatalf("connecting %s: %v", migr, err)
	}
	connectStray(t, srv, undecodable)

	node.Reset()
	reply := cnSweepSyncup(t, srv, 3, true)
	cnSweepAssertCode(t, reply.GetAgentReply(), 0, "SyncupCn")
	for _, nqn := range []string{migr, undecodable} {
		if _, ok := node.subsystems[nqn]; !ok {
			t.Fatalf("%s was disconnected", nqn)
		}
		dir := fmt.Sprintf("%s/nvme-subsys%d", sysfsNvmeSubsysDir,
			node.subsystems[nqn].idx)
		for _, call := range node.callsMatching(dir) {
			if call != "read "+dir+"/subsysnqn" {
				t.Fatalf("the node pass read %q of %s", call, nqn)
			}
		}
	}
	assertNoCall(t, node, "cmd nvme disconnect")
}

// TestARefusedDisableKeepsTheNamespaceInaccessible pins the build's ANA loop
// for the namespace whose disable the step refused (CN9, CN16): it still
// sits over the old ns-dev, which a park that failed left suspended, so the
// loop never moves it to optimized, whatever the new ns-dev's step gave;
// a namespace that was inaccessible stays so and its hosts keep queueing at
// the host rather than sending IO into the suspended device.
func TestARefusedDisableKeepsTheNamespaceInaccessible(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	oldDev := nsDevName(srv, testNs)
	nsPath := agent.NvmetRoot + "/subsystems/" + testNqn + "/namespaces/1"
	sub := defaultSubsys(false)
	sub[testNqn].NsList[0].NsId = testNs + 0x100

	node.mu.Lock()
	// Inaccessible before the pass, as an earlier pass's P1 leaves it.
	node.files[nsPath+"/ana_grpid"] = "3"
	node.failCmdAlways["dmsetup reload "+oldDev] =
		"device-mapper: reload ioctl on " + oldDev +
			" failed: Cannot allocate memory"
	node.mu.Unlock()
	node.Reset()
	reply := syncupCntlrReply(t, srv, reqOpts{revision: 3, primary: true,
		subsys: sub})
	cnSweepAssertCode(t, reply, common.ReplyCodeLeftover, "the pass")
	node.mu.Lock()
	defer node.mu.Unlock()
	if dev := strings.TrimSpace(node.files[nsPath+"/device_path"]); dev !=
		"/dev/mapper/"+oldDev || !node.dms[oldDev].suspended {
		t.Fatalf("fixture: the namespace is over %q, the old ns-dev "+
			"suspended %v", dev, node.dms[oldDev].suspended)
	}
	if grp := strings.TrimSpace(node.files[nsPath+"/ana_grpid"]); grp != "3" {
		t.Fatalf("ana_grpid is %q over the suspended old ns-dev, want 3",
			grp)
	}
	for _, call := range node.calls {
		if call == "writedirect "+nsPath+"/ana_grpid=1" {
			t.Fatalf("the namespace was moved to optimized")
		}
	}
}
