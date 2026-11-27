package dnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// removalCalls are the calls of node's record that remove something: a dm
// device, an nvmet directory or link, a host connection.
func removalCalls(node *fakeNode) []string {
	var out []string
	for _, banned := range []string{
		"cmd dmsetup remove", "cmd rmdir", "cmd rm -f /sys/kernel/config",
		"cmd nvme disconnect",
	} {
		out = append(out, node.callsMatching(banned)...)
	}
	return out
}

// TestAStartOnAnOlderDnListRemovesNothing is DN2's start on local files
// older than the node: the stored list of the disk node lacks a side the
// disk holds, built after the files' time. The startup pass removes none of
// that side's devices and frees no record; the disk node's round names the
// side's devices and differs in revision, so the worker sends its SyncupDn
// (dnv-worker.md RW4). The first SyncupDn of the process that names the side
// keeps it, and once a later one drops it, its removal runs: removal waits
// for the worker's own list, and starts with it.
func TestAStartOnAnOlderDnListRemovesNothing(t *testing.T) {
	srv, node := newTestServer(t)
	w := newFakeWorker(t, srv)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	if reply := w.syncupDn(legDnReq(2, testSide, testSide2)); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn: %v", reply.GetAgentReply())
	}
	for _, sideId := range []uint64{testSide, testSide2} {
		w.provision(srv, legSideReq(1, sideId))
	}
	w.roundAll("built", 2, 1, testSide, testSide2)
	newer := nf.DnSideName(testCluster, testDn, testSp, testSide2)

	// The disk node's file goes back to an older copy, which lists the
	// first side alone.
	if err := node.writeProto(context.Background(),
		nf.LocalDnPath(testCluster, testDn), legDnReq(1, testSide)); err != nil {
		t.Fatalf("seeding the older dn file: %v", err)
	}
	srv, w, _ = restartAgent(t, w, srv, node, true)
	if calls := removalCalls(node); len(calls) != 0 {
		t.Fatalf("the startup pass removed: %v", calls)
	}
	if !dmPresent(node, newer) {
		t.Fatalf("the startup pass removed %s", newer)
	}
	if _, ok, _ := srv.meta.LookupSide(context.Background(), testSp,
		testSide2); !ok {
		t.Fatal("the startup pass freed the newer side's record")
	}
	round := w.dnRound(2)
	if round.GetRevision() != 1 || round.GetAgentReply().GetCode() !=
		common.ReplyCodeLeftover || !strings.Contains(
		round.GetAgentReply().GetDetails(), newer) {
		t.Fatalf("the disk node's round = revision %d, %v; want revision 1 "+
			"and the newer side's device named", round.GetRevision(),
			round.GetAgentReply())
	}

	// The worker's list names the side: it stays.
	if reply := w.syncupDn(legDnReq(2, testSide, testSide2)); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("the first SyncupDn: %v", reply.GetAgentReply())
	}
	if !dmPresent(node, newer) {
		t.Fatalf("the first SyncupDn removed %s, which it names", newer)
	}
	// The side's file went with the older list (it named no such side);
	// the side's own syncup adopts what the node holds.
	w.provision(srv, legSideReq(1, testSide2))
	w.roundAll("after the first SyncupDn", 2, 1, testSide, testSide2)

	// A later list drops the side: its removal runs.
	if reply := w.syncupDn(legDnReq(3, testSide)); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("the SyncupDn dropping the side: %v", reply.GetAgentReply())
	}
	if dmPresent(node, newer) {
		t.Fatalf("%s survived the SyncupDn that dropped its side", newer)
	}
	if _, ok, _ := srv.meta.LookupSide(context.Background(), testSp,
		testSide2); ok {
		t.Fatal("the dropped side's record stayed")
	}
}

// TestAStartOnAnOlderSideFileRemovesNothingOfTheSide is DN2's start on an
// older file of a side: the file names one CN, while the node holds the
// side's stack for two. The startup pass converges the side from its file
// and removes nothing of it; the side's round names the second CN's devices
// and differs in revision, so the worker sends the side's syncup. A SyncupSide
// of the process is then the proof the side's sweep needs: one that names
// the second CN keeps it, and one that does not removes it.
func TestAStartOnAnOlderSideFileRemovesNothingOfTheSide(t *testing.T) {
	srv, node := newTestServer(t)
	w := newFakeWorker(t, srv)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	if reply := w.syncupDn(legDnReq(1, testSide)); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn: %v", reply.GetAgentReply())
	}
	w.provision(srv, legSideReq(2, testSide))
	w.roundAll("built", 1, 2, testSide)
	standby := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1)
	if !dmPresent(node, standby) {
		t.Fatalf("fixture is wrong: no %s", standby)
	}

	// The side's file goes back to an older copy, which names the primary
	// CN alone.
	older := legSideReq(1, testSide)
	older.SideConf.StandbyIdList = nil
	if err := node.writeProto(context.Background(),
		nf.LocalSidePath(testCluster, testDn, testSp, testSide),
		older); err != nil {
		t.Fatalf("seeding the older side file: %v", err)
	}
	srv, w, _ = restartAgent(t, w, srv, node, true)
	if calls := removalCalls(node); len(calls) != 0 {
		t.Fatalf("the startup pass removed: %v", calls)
	}
	if !dmPresent(node, standby) {
		t.Fatalf("the startup pass removed %s", standby)
	}
	round := w.sideRound(legPtr(testSide), 2)
	if round.GetRevision() != 1 || round.GetAgentReply().GetCode() !=
		common.ReplyCodeLeftover || !strings.Contains(
		round.GetAgentReply().GetDetails(), standby) {
		t.Fatalf("the side's round = revision %d, %v; want revision 1 and "+
			"the second CN's device named", round.GetRevision(),
			round.GetAgentReply())
	}

	// The worker's request names the second CN: it stays.
	if reply := w.syncupSide(legSideReq(2, testSide)); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("the side's first SyncupSide: %v", reply.GetAgentReply())
	}
	if !dmPresent(node, standby) {
		t.Fatalf("the side's first SyncupSide removed %s", standby)
	}
	w.roundAll("after the side's syncup", 1, 2, testSide)

	// A later request drops the second CN: its removal runs.
	drop := legSideReq(3, testSide)
	drop.SideConf.StandbyIdList = nil
	if reply := w.syncupSide(drop); reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("the SyncupSide dropping the CN: %v", reply.GetAgentReply())
	}
	if dmPresent(node, standby) {
		t.Fatalf("%s survived the SyncupSide that dropped its CN", standby)
	}
}

// TestAStartsFenceTimerConvergeRemovesNothing: the fence timer's converge
// (reconvergeSide) of a side whose request is still its file's removes
// nothing either, and its observation names what it would remove: only a
// SyncupSide of the process opens the side's removal (sideState.synced).
func TestAStartsFenceTimerConvergeRemovesNothing(t *testing.T) {
	srv, node := newTestServer(t)
	w := newFakeWorker(t, srv)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	if reply := w.syncupDn(legDnReq(1, testSide)); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn: %v", reply.GetAgentReply())
	}
	w.provision(srv, legSideReq(2, testSide))
	standby := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1)
	older := legSideReq(1, testSide)
	older.SideConf.StandbyIdList = nil
	if err := node.writeProto(context.Background(),
		nf.LocalSidePath(testCluster, testDn, testSp, testSide),
		older); err != nil {
		t.Fatalf("seeding the older side file: %v", err)
	}
	srv, _, _ = restartAgent(t, w, srv, node, true)
	key := sideKey(testCluster, testDn, testSp, testSide)
	st := srv.getSide(key)
	if st == nil || st.synced.Load() {
		t.Fatalf("fixture is wrong: the side is not loaded from its file")
	}
	node.Reset()
	srv.reconvergeSide(context.Background(), key, st)
	if calls := removalCalls(node); len(calls) != 0 || !dmPresent(node,
		standby) {
		t.Fatalf("the fence timer's converge before the side's first "+
			"syncup removed: %v", calls)
	}
	round := checkSideRoundOf(t, srv, legPtr(testSide), 1)
	if round.GetAgentReply().GetCode() != common.ReplyCodeLeftover ||
		!strings.Contains(round.GetAgentReply().GetDetails(), standby) {
		t.Fatalf("the converge's observation %v does not name %s",
			round.GetAgentReply(), standby)
	}
}

// TestAStartOnAnOlderSideFileKeepsItsMigration: a migration destination
// whose side file goes back to a copy older than its migration (no
// migr_dst_conf) holds a dm-clone, its wrapper and its clone-metadata record
// that the file does not claim. The start removes none of them, and a
// SyncupDn that comes before the side's own syncup removes none of them
// either: a side loaded from its file is no proof of its pool's migrations
// (knownSides, spFullyKnown). The side's round names its wait, so the
// worker sends its SyncupSide, which claims them again: the same record,
// never a fresh run (DN2, DN13).
func TestAStartOnAnOlderSideFileKeepsItsMigration(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	provisionSide(t, srv, 1, testSide)
	if reply, err := srv.SyncupSide(ctx, migrDstReq(2,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil ||
		reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("the destination's SyncupSide: %v, %v",
			reply.GetAgentReply(), err)
	}
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	metaName := nf.DnMigrMetaDmName(testCluster, testDn, testSp, testMigrId)
	rec, held, _ := srv.meta.LookupCloneMeta(ctx, testSp, testMigrId)
	if !dmPresent(node, cloneName) || !dmPresent(node, metaName) || !held {
		t.Fatal("fixture is wrong: no destination stack")
	}

	// The side's file goes back to a copy older than the migration.
	older := sideReq(1, testSide, testCn0, []uint64{testCn1},
		pb.SpLevel_SP_LEVEL_READWRITE)
	if err := node.writeProto(ctx,
		nf.LocalSidePath(testCluster, testDn, testSp, testSide),
		older); err != nil {
		t.Fatalf("seeding the older side file: %v", err)
	}
	stopTestServer(t, srv)
	node.Reset()
	srv = startTestServer(t, node)
	if calls := removalCalls(node); len(calls) != 0 {
		t.Fatalf("the startup pass removed: %v", calls)
	}

	// The node syncup comes first: it removes nothing of the migration.
	node.Reset()
	if reply, err := srv.SyncupDn(ctx, dnReq(2, testSide)); err != nil ||
		reply.GetAgentReply().GetCode() == common.ReplyCodeStaleRevision {
		t.Fatalf("SyncupDn: %v, %v", reply.GetAgentReply(), err)
	}
	if !dmPresent(node, cloneName) || !dmPresent(node, metaName) {
		t.Fatalf("the SyncupDn before the side's syncup removed the "+
			"migration: %v", removalCalls(node))
	}
	if again, stillHeld, _ := srv.meta.LookupCloneMeta(ctx, testSp,
		testMigrId); !stillHeld || again.GetUnitStart() != rec.GetUnitStart() {
		t.Fatal("the SyncupDn before the side's syncup freed the " +
			"clone-metadata record")
	}

	// The side's round names its wait.
	round, _ := checkSideNow(srv, traced("older"), &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
		Revision: 2, ShowInfo: true,
	}, nil)
	if round.GetAgentReply().GetCode() == 0 {
		t.Fatalf("the unsynced side's round is clean: %v",
			round.GetAgentReply())
	}

	// Its SyncupSide claims the migration again: the same record.
	if reply, err := srv.SyncupSide(ctx, migrDstReq(2,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil ||
		reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("the destination's SyncupSide after the restart: %v, %v",
			reply.GetAgentReply(), err)
	}
	if again, stillHeld, _ := srv.meta.LookupCloneMeta(ctx, testSp,
		testMigrId); !stillHeld || again.GetUnitStart() != rec.GetUnitStart() {
		t.Fatal("the destination's syncup took a fresh clone-metadata run")
	}
}

// TestAStartRetiresAnUnnamedSuspendedLinear: the startup pass removes
// nothing, but it still runs P0, which removes nothing: a per-CN linear left
// suspended, of a side the stored list does not name, goes onto its dm-error
// and is resumed, so no device stays suspended while the start waits for the
// worker ([D12]; DN2). The linear itself stays.
func TestAStartRetiresAnUnnamedSuspendedLinear(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	syncupBoth(t, srv, 1, testSide)
	lin := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	dmErr := nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0)
	node.mu.Lock()
	node.dms[lin].suspended = true
	node.mu.Unlock()
	if err := node.writeProto(ctx, nf.LocalDnPath(testCluster, testDn),
		dnReq(2)); err != nil {
		t.Fatalf("seeding the older dn file: %v", err)
	}
	stopTestServer(t, srv)
	node.Reset()
	startTestServer(t, node)
	if calls := removalCalls(node); len(calls) != 0 {
		t.Fatalf("the startup pass removed: %v", calls)
	}
	if !dmPresent(node, lin) {
		t.Fatalf("the startup pass removed %s", lin)
	}
	if dmSuspended(node, lin) || !linearMapsDm(node, lin, dmErr) {
		t.Fatalf("the startup pass left %s suspended or off its dm-error", lin)
	}
}

// checkSideRoundOf answers a side's round from its stored observation, with
// no read of its own.
func checkSideRoundOf(
	t *testing.T,
	srv *DnAgentServer,
	ptr *pb.SidePointer,
	revision uint64,
) *pb.CheckSideReply {
	t.Helper()
	reply, _ := srv.checkSideRound(traced("observation"), &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: ptr,
		Revision: revision,
	}, nil)
	return reply
}

// TestAStartRetiresASuspendedLinearTheSideFileDropped: the side's own start
// converge runs P0 too: a per-CN linear its older file does not name, left
// suspended over the side device (a failed reload's leftover, which the
// fence's resume leaves for its dm-error), goes onto its dm-error and is
// resumed, and stays ([D12]; DN2).
func TestAStartRetiresASuspendedLinearTheSideFileDropped(t *testing.T) {
	srv, node := newTestServer(t)
	w := newFakeWorker(t, srv)
	ctx := context.Background()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	if reply := w.syncupDn(legDnReq(1, testSide)); reply.
		GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupDn: %v", reply.GetAgentReply())
	}
	w.provision(srv, legSideReq(2, testSide))
	lin := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1)
	dmErr := nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn1)
	sideNo := node.devNo[nf.DmPath(nf.DnSideName(testCluster, testDn, testSp,
		testSide))]
	node.mu.Lock()
	fields := strings.Fields(node.dms[lin].table)
	node.dms[lin].table = fmt.Sprintf("0 %s linear %s 0", fields[1], sideNo)
	node.dms[lin].suspended = true
	node.mu.Unlock()
	older := legSideReq(1, testSide)
	older.SideConf.StandbyIdList = nil
	if err := node.writeProto(ctx,
		nf.LocalSidePath(testCluster, testDn, testSp, testSide),
		older); err != nil {
		t.Fatalf("seeding the older side file: %v", err)
	}
	restartAgent(t, w, srv, node, true)
	if calls := removalCalls(node); len(calls) != 0 || !dmPresent(node, lin) {
		t.Fatalf("the startup pass removed: %v", calls)
	}
	if dmSuspended(node, lin) || !linearMapsDm(node, lin, dmErr) {
		t.Fatalf("the startup pass left %s suspended or off its dm-error", lin)
	}
}
