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

// sharedReq2 is cntlr 2, of the second SP, naming the fixture's own NQN:
// the shared NQN of CN16.
func sharedReq2(o reqOpts) *pb.SyncupCntlrRequest {
	req := cntlrReq(o)
	req.CntlrPointer = cntlrPtr2()
	return req
}

// subsysChanges are the calls of node that change something under the
// kernel subsystem nqn: every call naming its configfs directory but a read.
func subsysChanges(node *fakeNode, nqn string) []string {
	dir := agent.NvmetRoot + "/subsystems/" + nqn
	var out []string
	for _, call := range node.Calls() {
		if !strings.Contains(call, dir+"/") && !strings.HasSuffix(call, dir) {
			continue
		}
		switch {
		case strings.HasPrefix(call, "read "), strings.HasPrefix(call, "list "),
			strings.HasPrefix(call, "mtime "), strings.HasPrefix(call, "devno "):
			continue
		}
		out = append(out, call)
	}
	return out
}

// TestASharedNqnIsBuiltByNoCntlr is CN16's shared NQN: two cntlrs of one
// node, of two pools, name one NQN. The second writes nothing under the
// kernel subsystem the first built, its subsystem row reads ERROR naming the
// first's pool (PENDING when it has no namespace under the NQN), and its
// verdict never names the subsystem; the first, once it sees the second,
// writes nothing either and its row reads PENDING naming the second's pool,
// while its own namespace keeps serving its own volume and keeps its row;
// the read reports the same rows. A cntlr that wants its namespace
// inaccessible moves it only when its device is the cntlr's own ns-dev: the
// second never darkens the first's, and the first, once it loses the
// primary role, darkens its own, and while that move fails the namespace's
// row reads the difference and the verdict names it. A namespace the first
// drops goes with its ns-dev, the subsystem staying. Once the second leaves
// the node, the first builds its subsystem again.
func TestASharedNqnIsBuiltByNoCntlr(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.SyncupCn(ctx, cnReq2(2)); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	reply, err := srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 3, primary: true}))
	if err != nil {
		t.Fatalf("cntlr 1: %v", err)
	}
	assertOk(t, reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
		"cntlr 1's subsystem before the second names it")
	nsPath := agent.NvmetRoot + "/subsystems/" + testNqn + "/namespaces/1"
	ownDev := srv.nf.DmPath(nsDevName(srv, testNs))
	optimized := strconv.Itoa(common.AnaGrpIdOptimized)
	inaccessible := strconv.Itoa(common.AnaGrpIdInaccessible)
	assertAna(t, node, testNqn, optimized, "cntlr 1's namespace")
	wantDevice := func(label string) {
		t.Helper()
		if got := strings.TrimSpace(node.fileOf(nsPath + "/device_path")); got != ownDev {
			t.Fatalf("%s: the namespace's device is %q, want cntlr 1's %q",
				label, got, ownDev)
		}
	}
	wantDevice("before cntlr 2")
	// sharedRow: the subsystem row reads want with RES_REASON_SHARED_NQN,
	// naming the other pool; the namespace has a row only on the builder.
	sharedRow := func(info *pb.CntlrInfo, other uint64, want pb.ResStatus,
		nsRow bool, label string) {
		t.Helper()
		row := info.GetSsIdToSubsystem()[testSs]
		if row.GetStatus() != want ||
			row.GetReason() != pb.ResReason_RES_REASON_SHARED_NQN ||
			!strings.Contains(row.GetDetails(), fmt.Sprintf("pool %d", other)) {
			t.Fatalf("%s: the subsystem row is %v, want %v with "+
				"RES_REASON_SHARED_NQN naming pool %d", label, row, want, other)
		}
		if got := info.GetNsIdToNamespace()[testNs]; (got != nil) != nsRow {
			t.Fatalf("%s: the shared subsystem's namespace has the row %v, "+
				"want a row %v", label, got, nsRow)
		}
	}
	refused := func(info *pb.CntlrInfo, other uint64, label string) {
		t.Helper()
		sharedRow(info, other, pb.ResStatus_RES_STATUS_ERROR, false, label)
	}
	kept := func(info *pb.CntlrInfo, other uint64, label string) {
		t.Helper()
		sharedRow(info, other, pb.ResStatus_RES_STATUS_PENDING, true, label)
		assertOk(t, info.GetNsIdToNamespace()[testNs],
			label+": the builder's own namespace")
	}
	// unnamed: the verdict names nothing of the subsystem.
	unnamed := func(reply *pb.SyncupCntlrReply, label string) {
		t.Helper()
		if details := reply.GetAgentReply().GetDetails(); strings.Contains(
			details, testNqn) {
			t.Fatalf("%s: the verdict names the shared subsystem: code %d, "+
				"details %q", label, reply.GetAgentReply().GetCode(), details)
		}
	}

	for i, tc := range []struct {
		primary bool
		// empty gives cntlr 2's subsystem no namespace, so a sweep that
		// took the NQN for cntlr 2 would remove cntlr 1's.
		empty bool
	}{{true, false}, {false, false}, {false, true}} {
		node.Reset()
		req2 := sharedReq2(reqOpts{revision: uint64(4 + i),
			primary: tc.primary})
		if tc.empty {
			req2.GetNqnToSubsystem()[testNqn].NsList = nil
		}
		reply, err = srv.SyncupCntlr(ctx, req2)
		if err != nil {
			t.Fatalf("cntlr 2 (%+v): %v", tc, err)
		}
		label := fmt.Sprintf("cntlr 2 %+v", tc)
		unnamed(reply, label)
		if tc.empty {
			sharedRow(reply.GetCntlrInfo(), testSp,
				pb.ResStatus_RES_STATUS_PENDING, false, label)
			if got := subsysChanges(node, testNqn); len(got) != 0 {
				t.Fatalf("%s changed the kernel subsystem: %v", label, got)
			}
			wantDevice(label)
			assertAna(t, node, testNqn, optimized, label)
			continue
		}
		refused(reply.GetCntlrInfo(), testSp, label)
		if got := subsysChanges(node, testNqn); len(got) != 0 {
			t.Fatalf("%s changed the kernel subsystem: %v", label, got)
		}
		wantDevice(label)
		assertAna(t, node, testNqn, optimized, label)
	}
	key2 := cntlrKey(testCluster, testCn, testSp2, testCntlr2)
	srv.refreshCntlr(ctx, key2, srv.getCntlr(key2))
	read2, err := srv.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr2()})
	if err != nil {
		t.Fatalf("GetCntlrInfo of cntlr 2: %v", err)
	}
	// Cntlr 2's last request names no namespace under the NQN.
	sharedRow(read2.GetCntlrInfo(), testSp, pb.ResStatus_RES_STATUS_PENDING,
		false, "cntlr 2's read")
	if got := subsysChanges(node, testNqn); len(got) != 0 {
		t.Fatalf("cntlr 2's read or sweep changed the subsystem: %v", got)
	}

	// The sweep takes the shared NQN for no pool, whichever claim it would
	// meet first (nvmetOwner).
	if _, ours, _ := srv.nvmetOwner(ctx, testCluster, testCn, testNqn, false,
		nil, &agent.SweepResult{}); ours {
		t.Fatalf("the sweep takes the shared NQN for a pool")
	}

	// Cntlr 1 sees cntlr 2 now, and writes nothing under the NQN either,
	// its own namespace serving on.
	node.Reset()
	reply, err = srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 5, primary: true}))
	if err != nil {
		t.Fatalf("cntlr 1 again: %v", err)
	}
	kept(reply.GetCntlrInfo(), testSp2, "cntlr 1 beside cntlr 2")
	if got := subsysChanges(node, testNqn); len(got) != 0 {
		t.Fatalf("cntlr 1 changed the shared subsystem: %v", got)
	}
	if code := reply.GetAgentReply().GetCode(); code != 0 {
		t.Fatalf("cntlr 1's verdict names something: code %d, details %q",
			code, reply.GetAgentReply().GetDetails())
	}
	assertAna(t, node, testNqn, optimized, "cntlr 1's namespace serves on")
	refreshTestCntlr(t, srv)
	kept(cntlrInfoNow(t, srv).GetCntlrInfo(), testSp2, "cntlr 1's read")

	// Cntlr 1 loses the primary role while the move of its namespace
	// fails: the namespace's row reads the difference, and the verdict
	// names it.
	anaWrite := nsPath + "/ana_grpid=" + inaccessible
	node.mu.Lock()
	node.failWriteAlways[anaWrite] = "refused"
	node.mu.Unlock()
	reply, err = srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 6, primary: false}))
	if err != nil {
		t.Fatalf("cntlr 1 demoted, its move failing: %v", err)
	}
	assertAna(t, node, testNqn, optimized, "the namespace whose move failed")
	nsRow := reply.GetCntlrInfo().GetNsIdToNamespace()[testNs]
	if nsRow.GetStatus() != pb.ResStatus_RES_STATUS_OK ||
		nsRow.GetReason() != pb.ResReason_RES_REASON_DIFFERS {
		t.Fatalf("the row of the namespace whose move failed is %v, want "+
			"OK with RES_REASON_DIFFERS", nsRow)
	}
	if reply.GetAgentReply().GetCode() == 0 || !strings.Contains(
		reply.GetAgentReply().GetDetails(), testNqn+"/1") {
		t.Fatalf("the verdict does not name the namespace whose move "+
			"failed: code %d, details %q", reply.GetAgentReply().GetCode(),
			reply.GetAgentReply().GetDetails())
	}

	// The re-send moves it: its own namespace goes inaccessible.
	node.mu.Lock()
	delete(node.failWriteAlways, anaWrite)
	node.mu.Unlock()
	reply, err = srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 6, primary: false}))
	if err != nil {
		t.Fatalf("cntlr 1 demoted: %v", err)
	}
	assertAna(t, node, testNqn, inaccessible, "the demoted cntlr's own namespace")
	wantDevice("after the demotion")
	kept(reply.GetCntlrInfo(), testSp2, "the demoted cntlr 1")

	// Cntlr 2, primary, never moves cntlr 1's namespace back to optimized.
	node.Reset()
	reply, err = srv.SyncupCntlr(ctx, sharedReq2(reqOpts{revision: 7,
		primary: true}))
	if err != nil {
		t.Fatalf("cntlr 2 beside the demoted cntlr 1: %v", err)
	}
	refused(reply.GetCntlrInfo(), testSp, "cntlr 2 beside the demoted cntlr 1")
	if got := subsysChanges(node, testNqn); len(got) != 0 {
		t.Fatalf("cntlr 2 changed the subsystem of the demoted cntlr 1: %v",
			got)
	}
	assertAna(t, node, testNqn, inaccessible,
		"the demoted cntlr's namespace beside cntlr 2")

	// Cntlr 1 drops its namespace: the sweep removes the namespace over its
	// own ns-dev, and the ns-dev, but not the subsystem.
	drop := cntlrReq(reqOpts{revision: 7, primary: false})
	drop.GetNqnToSubsystem()[testNqn].NsList = nil
	if _, err := srv.SyncupCntlr(ctx, drop); err != nil {
		t.Fatalf("cntlr 1 drops its namespace: %v", err)
	}
	if node.dirs[nsPath] {
		t.Fatalf("the namespace over cntlr 1's dropped ns-dev stays")
	}
	if !node.dirs[agent.NvmetRoot+"/subsystems/"+testNqn] {
		t.Fatalf("cntlr 1's sweep removed the shared subsystem")
	}
	node.mu.Lock()
	_, devLeft := node.dms[nsDevName(srv, testNs)]
	node.mu.Unlock()
	if devLeft {
		t.Fatalf("cntlr 1's dropped ns-dev stays")
	}

	// Cntlr 2 leaves the node; cntlr 1 builds its subsystem again.
	if _, err := srv.SyncupCn(ctx, &pb.SyncupCnRequest{
		ClusterId: testCluster, CnId: testCn, Revision: 7,
		CntlrPointerList: []*pb.CntlrPointer{cntlrPtr()},
	}); err != nil {
		t.Fatalf("SyncupCn without cntlr 2: %v", err)
	}
	reply, err = srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 8, primary: true}))
	if err != nil {
		t.Fatalf("cntlr 1 alone: %v", err)
	}
	wantDevice("cntlr 1 alone")
	assertOk(t, reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
		"cntlr 1's subsystem once cntlr 2 left")
	assertAna(t, node, testNqn, optimized, "cntlr 1's namespace once alone")
}

// TestASharedNqnBuilderLeaves is CN16's shared NQN once the cntlr that built
// the kernel subsystem leaves the node: the node's sweep moves the namespace
// over its ns-dev inaccessible before the park of that ns-dev, and removes
// it, and the other cntlr then builds its own namespace, over its own
// ns-dev. Before the first cntlr leaves, the other cntlr's fence watch
// neither counts nor moves the first's serving namespace.
func TestASharedNqnBuilderLeaves(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.SyncupCn(ctx, cnReq2(2)); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if _, err := srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 3, primary: true})); err != nil {
		t.Fatalf("cntlr 1: %v", err)
	}
	reply, err := srv.SyncupCntlr(ctx,
		sharedReq2(reqOpts{revision: 4, primary: true}))
	if err != nil {
		t.Fatalf("cntlr 2: %v", err)
	}
	if row := reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs]; row.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("cntlr 2 beside cntlr 1: the subsystem row is %v", row)
	}

	// Cntlr 2's fence watch counts cntlr 1's serving namespace as none of
	// its own, and its fence moves it nowhere (ownsFenceNs).
	key2 := cntlrKey(testCluster, testCn, testSp2, testCntlr2)
	srv.mu.Lock()
	w2 := srv.fenceWatches[key2]
	srv.mu.Unlock()
	if w2 == nil {
		t.Fatalf("fixture is wrong: cntlr 2 has no fence watch")
	}
	if srv.fenceServed(ctx, w2.nss, nil) {
		t.Fatalf("cntlr 2's watch counts cntlr 1's namespace as served")
	}
	for _, leg := range []uint64{testMetaLeg, testDataLeg} {
		node.setAnaState(names(srv).SideToCnNqn(testCluster, testSp2, leg,
			testCn), testIp, testSvcId, "non-optimized")
	}
	srv.fenceNamespaces(ctx, w2)
	if !w2.st.fenced.Load() {
		t.Fatalf("the case is vacuous: cntlr 2's watch read no fence")
	}
	assertAna(t, node, testNqn, strconv.Itoa(common.AnaGrpIdOptimized),
		"cntlr 1's namespace after cntlr 2's fence")
	for _, leg := range []uint64{testMetaLeg, testDataLeg} {
		node.setAnaState(names(srv).SideToCnNqn(testCluster, testSp2, leg,
			testCn), testIp, testSvcId, "optimized")
	}
	w2.st.fenced.Store(false)

	// Cntlr 1 leaves the node: its namespace goes inaccessible before its
	// ns-dev's park, and goes.
	nsPath := agent.NvmetRoot + "/subsystems/" + testNqn + "/namespaces/1"
	firstDev := nsDevName(srv, testNs)
	node.Reset()
	if _, err := srv.SyncupCn(ctx, &pb.SyncupCnRequest{
		ClusterId: testCluster, CnId: testCn, Revision: 3,
		CntlrPointerList: []*pb.CntlrPointer{cntlrPtr2()},
	}); err != nil {
		t.Fatalf("SyncupCn without cntlr 1: %v", err)
	}
	dark := node.indexOfCall("writedirect " + nsPath + "/ana_grpid=" +
		strconv.Itoa(common.AnaGrpIdInaccessible))
	park := -1
	for i, call := range node.Calls() {
		if strings.HasPrefix(call, "cmd dmsetup ") &&
			strings.Contains(call, firstDev) {
			park = i
			break
		}
	}
	if dark < 0 || park < 0 || dark > park {
		t.Fatalf("cntlr 1's namespace did not move inaccessible before its "+
			"ns-dev's park: move at %d, first dmsetup of the ns-dev at %d",
			dark, park)
	}
	if node.dirs[nsPath] {
		t.Fatalf("the namespace over cntlr 1's ns-dev stays after it left")
	}

	reply, err = srv.SyncupCntlr(ctx,
		sharedReq2(reqOpts{revision: 5, primary: true}))
	if err != nil {
		t.Fatalf("cntlr 2 alone: %v", err)
	}
	assertOk(t, reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
		"cntlr 2's subsystem once cntlr 1 left")
	ownDev := srv.nf.DmPath(names(srv).CnNsDevName(testCluster, testCn,
		testSp2, testNs))
	if got := strings.TrimSpace(node.fileOf(nsPath + "/device_path")); got != ownDev {
		t.Fatalf("the namespace's device is %q, want cntlr 2's %q", got,
			ownDev)
	}
}

// TestASharedNqnLeftSubsystemIsReplaced: a kernel subsystem that holds a
// namespace over another pool's ns-dev of the node, once one cntlr alone
// names the NQN (the builder's request gone with no sweep after it, as
// after a restart), is read ERROR naming that pool, and the converge moves
// its namespace inaccessible, removes the subsystem and builds its own,
// over its own ns-dev (replaceForeignSubsys).
func TestASharedNqnLeftSubsystemIsReplaced(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.SyncupCn(ctx, cnReq2(2)); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if _, err := srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 3, primary: true})); err != nil {
		t.Fatalf("cntlr 1: %v", err)
	}
	if _, err := srv.SyncupCntlr(ctx,
		sharedReq2(reqOpts{revision: 4, primary: true})); err != nil {
		t.Fatalf("cntlr 2: %v", err)
	}
	srv.dropCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	nsPath := agent.NvmetRoot + "/subsystems/" + testNqn + "/namespaces/1"
	firstDev := srv.nf.DmPath(nsDevName(srv, testNs))
	if got := strings.TrimSpace(node.fileOf(nsPath + "/device_path")); got != firstDev {
		t.Fatalf("the case is vacuous: the namespace's device is %q, want "+
			"the first pool's %q", got, firstDev)
	}
	key2 := cntlrKey(testCluster, testCn, testSp2, testCntlr2)
	srv.refreshCntlr(ctx, key2, srv.getCntlr(key2))
	read2, err := srv.GetCntlrInfo(ctx, &pb.GetCntlrInfoRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr2()})
	if err != nil {
		t.Fatalf("GetCntlrInfo of cntlr 2: %v", err)
	}
	if row := read2.GetCntlrInfo().GetSsIdToSubsystem()[testSs]; row.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(row.GetDetails(), fmt.Sprintf("pool %d", testSp)) {
		t.Fatalf("cntlr 2's read: the subsystem row is %v, want ERROR "+
			"naming the first pool's ns-dev", row)
	}

	node.Reset()
	reply, err := srv.SyncupCntlr(ctx,
		sharedReq2(reqOpts{revision: 5, primary: true}))
	if err != nil {
		t.Fatalf("cntlr 2 alone: %v", err)
	}
	assertOk(t, reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
		"cntlr 2's subsystem once the other pool's subsystem went")
	ownDev := srv.nf.DmPath(names(srv).CnNsDevName(testCluster, testCn,
		testSp2, testNs))
	if got := strings.TrimSpace(node.fileOf(nsPath + "/device_path")); got != ownDev {
		t.Fatalf("the namespace's device is %q, want cntlr 2's %q", got,
			ownDev)
	}
	subsysDir := agent.NvmetRoot + "/subsystems/" + testNqn
	dark := node.indexOfCall("writedirect " + nsPath + "/ana_grpid=" +
		strconv.Itoa(common.AnaGrpIdInaccessible))
	removed := -1
	for i, call := range node.Calls() {
		if strings.HasPrefix(call, "cmd rmdir ") &&
			strings.HasSuffix(call, " "+subsysDir) {
			removed = i
			break
		}
	}
	if dark < 0 || removed < 0 || dark > removed {
		t.Fatalf("the first pool's namespace did not move inaccessible "+
			"before the subsystem's removal: move at %d, removal at %d",
			dark, removed)
	}
}

// TestASharedNqnBuilderServes: the cntlr that built a shared kernel
// subsystem as a standby, once promoted, moves its own namespace to
// optimized, and its rows read the subsystem PENDING and the namespace OK
// (CN16, A shared NQN). A host taken off its list, which no list step cuts
// while the NQN is shared, makes the subsystem not safe: its namespace goes
// inaccessible and the subsystem's row says why.
func TestASharedNqnBuilderServes(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.SyncupCn(ctx, cnReq2(2)); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	const hostA = "nqn.2014-08.org.nvmexpress:uuid:host-a"
	const hostB = "nqn.2014-08.org.nvmexpress:uuid:host-b"
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{revision: 3,
		primary: false, subsys: subsysWithHosts(hostA)})); err != nil {
		t.Fatalf("cntlr 1 as a standby: %v", err)
	}
	inaccessible := strconv.Itoa(common.AnaGrpIdInaccessible)
	assertAna(t, node, testNqn, inaccessible, "the standby's namespace")
	if _, err := srv.SyncupCntlr(ctx,
		sharedReq2(reqOpts{revision: 4, primary: true})); err != nil {
		t.Fatalf("cntlr 2: %v", err)
	}
	assertAna(t, node, testNqn, inaccessible,
		"the standby's namespace beside cntlr 2")

	reply, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{revision: 5,
		primary: true, subsys: subsysWithHosts(hostA)}))
	if err != nil {
		t.Fatalf("cntlr 1 promoted: %v", err)
	}
	assertAna(t, node, testNqn, strconv.Itoa(common.AnaGrpIdOptimized),
		"the promoted builder's own namespace")
	row := reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs]
	if row.GetStatus() != pb.ResStatus_RES_STATUS_PENDING ||
		row.GetReason() != pb.ResReason_RES_REASON_SHARED_NQN {
		t.Fatalf("the promoted builder's subsystem row is %v", row)
	}
	assertOk(t, reply.GetCntlrInfo().GetNsIdToNamespace()[testNs],
		"the promoted builder's namespace")

	// A host taken off cntlr 1's list: no list step cuts it, so the
	// namespace goes inaccessible.
	reply, err = srv.SyncupCntlr(ctx, cntlrReq(reqOpts{revision: 6,
		primary: true, subsys: subsysWithHosts(hostB)}))
	if err != nil {
		t.Fatalf("cntlr 1 with another host list: %v", err)
	}
	assertAna(t, node, testNqn, inaccessible,
		"the builder's namespace while a host off its list is admitted")
	row = reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs]
	if row.GetStatus() != pb.ResStatus_RES_STATUS_PENDING ||
		!strings.Contains(row.GetDetails(), "admits a host off its list") {
		t.Fatalf("the builder's subsystem row is %v, want PENDING saying "+
			"a host off its list is admitted", row)
	}
}
