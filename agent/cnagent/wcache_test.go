package cnagent

import (
	"strconv"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	pb "github.com/distributed-nvme/distributed-nvme/pb"
)

// The write cache a host or this node reads (CN10, CN15; wcache.go).

// wcacheOf reads the fake's write cache of the dm device name, "" when it
// has none.
func wcacheOf(node *fakeNode, name string) string {
	node.mu.Lock()
	defer node.mu.Unlock()
	devNo := node.devNo["/dev/mapper/"+name]
	return strings.TrimSpace(
		node.files["/sys/dev/block/"+devNo+"/queue/write_cache"])
}

// dmTableOf is the fake's live table of the dm device name.
func dmTableOf(node *fakeNode, name string) string {
	node.mu.Lock()
	defer node.mu.Unlock()
	if dm, ok := node.dms[name]; ok {
		return dm.table
	}
	return ""
}

// TestATdErrorReportsAWriteCache: the td's dm-error is the two-target table
// whose last target names the agent's write cache loop device, so it and the
// ns-dev over it read "write back" though they fail every IO, and a host
// scans the namespace as it will read it once it serves (CN15). The loop
// device is made once, and a restart finds it again rather than making a
// second one. Its row reads OK.
func TestATdErrorReportsAWriteCache(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2})
	errName := errorName(srv, testTd)
	sectors := testTdSize / 512
	first := sectors - common.DmErrorWcSectors
	want := []string{
		"0 " + itoa(first) + " error",
		itoa(first) + " 8 error 7:0 0",
	}
	if got := strings.Fields(strings.ReplaceAll(dmTableOf(node, errName),
		"\n", " | ")); strings.Join(got, " ") !=
		strings.Join(want, " | ") {
		t.Fatalf("the td's dm-error is %q, want %q", dmTableOf(node, errName),
			strings.Join(want, "\n"))
	}
	if got := wcacheOf(node, errName); got != writeBack {
		t.Errorf("the td's dm-error reads write cache %q, want %q", got,
			writeBack)
	}
	if got := wcacheOf(node, nsDevName(srv, testNs)); got != writeBack {
		t.Errorf("the ns-dev over it reads write cache %q, want %q", got,
			writeBack)
	}
	if got := len(node.callsMatching("cmd losetup --find --show")); got != 1 {
		t.Fatalf("losetup made %d loop devices, want 1", got)
	}
	reply := syncupBoth(t, srv, reqOpts{revision: 3})
	assertOk(t, reply.GetCntlrInfo().GetTdIdToDmError()[testTd], "td dm-error")
	if got := len(node.callsMatching("cmd losetup --find --show")); got != 1 {
		t.Fatalf("a second converge made a loop device again: %d", got)
	}

	node.Reset()
	reconcileForTest(t, newCnServer(node))
	if node.hasCall("cmd losetup --find --show") {
		t.Errorf("the restart made a second loop device")
	}
	if !node.hasCall("cmd losetup -j") {
		t.Errorf("the restart did not look for the loop device it had")
	}
}

// itoa is strconv.FormatUint in base 10.
func itoa(v uint64) string { return strconv.FormatUint(v, 10) }

// TestAnOlderTdErrorAndItsNsDevAreLoadedAgain: a td's dm-error an older
// agent built, one error target, is loaded again into the two-target table
// once the loop device is there, and the ns-dev over it, which kept the
// "write through" it took at its own load, is loaded again with the same
// table and reads "write back" (CN15).
func TestAnOlderTdErrorAndItsNsDevAreLoadedAgain(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2})
	errName := errorName(srv, testTd)
	nsDev := nsDevName(srv, testNs)
	node.mu.Lock()
	node.dms[errName].table = "0 " + itoa(testTdSize/512) + " error"
	errNo := node.devNo["/dev/mapper/"+errName]
	nsNo := node.devNo["/dev/mapper/"+nsDev]
	delete(node.files, "/sys/dev/block/"+errNo+"/queue/write_cache")
	node.files["/sys/dev/block/"+nsNo+"/queue/write_cache"] = "write through\n"
	node.mu.Unlock()

	node.Reset()
	syncupBoth(t, srv, reqOpts{revision: 3})
	if got := strings.Count(dmTableOf(node, errName), "error"); got != 2 {
		t.Fatalf("the older td dm-error is %q, want two error targets",
			dmTableOf(node, errName))
	}
	if !node.hasCall("cmd dmsetup reload " + nsDev) {
		t.Errorf("the ns-dev with a stale write cache was not loaded again")
	}
	if got := wcacheOf(node, nsDev); got != writeBack {
		t.Errorf("the ns-dev reads write cache %q, want %q", got, writeBack)
	}
}

// TestNoLoopDeviceLeavesTheOneTargetTable: a loop device that cannot be made
// leaves the td's dm-error on the one-target table, with no error and an OK
// row, and the make is not tried again before common.CnWcacheRetry (CN15).
func TestNoLoopDeviceLeavesTheOneTargetTable(t *testing.T) {
	srv, node := newTestServer(t)
	node.mu.Lock()
	node.failCmdAlways["cmd losetup"] = "losetup: not found"
	node.mu.Unlock()
	reply := syncupBoth(t, srv, reqOpts{revision: 2})
	errName := errorName(srv, testTd)
	if got := dmTableOf(node, errName); got != "0 "+itoa(testTdSize/512)+
		" error" {
		t.Fatalf("the td's dm-error is %q, want the one-target table", got)
	}
	assertOk(t, reply.GetCntlrInfo().GetTdIdToDmError()[testTd], "td dm-error")
	tries := len(node.callsMatching("cmd losetup"))
	syncupBoth(t, srv, reqOpts{revision: 3})
	if got := len(node.callsMatching("cmd losetup")); got != tries {
		t.Errorf("losetup ran again within the retry wait: %d, then %d",
			tries, got)
	}
}

// legPathFixture connects the fixture's two legs as a standby while their
// disk nodes serve it from a device with no write cache, then makes the
// targets report what targetWc says, as a failover puts the side's own
// device under each path. It returns the two legs' NQNs.
func legPathFixture(
	t *testing.T,
	srv *CnAgentServer,
	node *fakeNode,
	targetWc string,
) []string {
	t.Helper()
	nqns := []string{legNqn(srv, testMetaLeg), legNqn(srv, testDataLeg)}
	for _, nqn := range nqns {
		node.setTargetWc(nqn, testIp, testSvcId, writeThrough)
	}
	syncupBoth(t, srv, reqOpts{revision: 2})
	for _, nqn := range nqns {
		node.setTargetWc(nqn, testIp, testSvcId, targetWc)
	}
	return nqns
}

// pathWcacheOf reads the write cache of the fake's path device of nqn's
// controller.
func pathWcacheOf(node *fakeNode, nqn string) string {
	node.mu.Lock()
	defer node.mu.Unlock()
	subsys := node.subsystems[nqn]
	if subsys == nil || len(subsys.ctrls) == 0 {
		return ""
	}
	ctrl := subsys.ctrls[0]
	return strings.TrimSpace(node.files[sysfsNvmeCtrlDir+"/"+ctrl.name+"/"+
		ctrl.pathDev+"/queue/write_cache"])
}

// TestAPrimaryRescansALegPathItReadAsAStandby: a standby connected each leg
// while its disk node served it from a device with no write cache; the
// failover puts the side's own device, which has one, under the path, and
// the new primary's converge asks the target, scans the path again, and
// loads the leg's wrapper again over the head that now reads "write back",
// before the arrays start, so its flushes reach the disk node (CN10). No row
// reads error and nothing is named.
func TestAPrimaryRescansALegPathItReadAsAStandby(t *testing.T) {
	srv, node := newTestServer(t)
	nqns := legPathFixture(t, srv, node, writeBack)
	if node.hasCall("identify ") {
		t.Fatalf("a standby asked a target for its write cache")
	}
	for _, nqn := range nqns {
		if got := pathWcacheOf(node, nqn); got != writeThrough {
			t.Fatalf("the standby's path of %s reads %q, want %q", nqn, got,
				writeThrough)
		}
	}
	node.Reset()
	reply := syncupBoth(t, srv, reqOpts{revision: 3, primary: true})
	assertCodeZero(t, "the new primary's converge", reply)
	for _, legId := range []uint64{testMetaLeg, testDataLeg} {
		nqn := legNqn(srv, legId)
		if got := pathWcacheOf(node, nqn); got != writeBack {
			t.Errorf("leg %#x's path reads %q after the converge, want %q",
				legId, got, writeBack)
		}
		if got := wcacheOf(node, legName(srv, legId)); got != writeBack {
			t.Errorf("leg %#x's wrapper reads %q, want %q", legId, got,
				writeBack)
		}
		row := reply.GetCntlrInfo().GetLegIdToLeg()[legId]
		if row.GetStatus() == pb.ResStatus_RES_STATUS_ERROR {
			t.Errorf("leg %#x's row reads ERROR %q", legId, row.GetDetails())
		}
	}
	if !node.hasCall("rescan_controller=1") {
		t.Errorf("no leg path was scanned again")
	}
}

// TestALegPathThatStaysStaleIsAnErrorRowAndNamed: a rescan that does not take
// leaves the path reading "write through" over a target that reports a
// write cache: the leg's row reads error, the converge names its wrapper, a
// round names it and brings the re-send, and once a rescan takes, the read
// and the round after it are clean (CN10, CN30).
func TestALegPathThatStaysStaleIsAnErrorRowAndNamed(t *testing.T) {
	srv, node := newTestServer(t)
	legPathFixture(t, srv, node, writeBack)
	node.mu.Lock()
	node.failWriteAlways["rescan_controller"] = "refused"
	node.mu.Unlock()
	opts := reqOpts{revision: 3, primary: true, leftoverOk: true}
	reply := syncupBoth(t, srv, opts)
	row := reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg]
	if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(row.GetDetails(), "reads write through") {
		t.Fatalf("the stale leg's row reads %v %q, want ERROR naming the path",
			row.GetStatus(), row.GetDetails())
	}
	assertCodeNames(t, "the converge over a stale path", reply,
		legName(srv, testDataLeg))
	held := cntlrReq(opts)
	round, resent := resendRound(t, srv, held, "stale")
	assertCodeNames(t, "the read of a stale path", round,
		legName(srv, testDataLeg))
	if resent == nil {
		t.Fatalf("the round that names the leg brought no re-send")
	}
	node.mu.Lock()
	delete(node.failWriteAlways, "rescan_controller")
	node.mu.Unlock()
	node.Reset()
	_, resent = resendRound(t, srv, held, "rescanned")
	if resent == nil || !node.hasCall("rescan_controller=1") {
		t.Fatalf("the re-send did not scan the path again")
	}
	round, resent = resendRound(t, srv, held, "fresh")
	assertCodeZero(t, "the read after the rescan", round)
	if resent != nil {
		t.Errorf("the round after the rescan re-sent the request")
	}
}

// TestADiskWithNoWriteCacheReadsWriteThroughAtEveryLayer: a disk node whose
// disk has no write cache serves each path from devices with none, before
// the failover and after it; the primary asks the target once and finds
// nothing stale: no rescan, no reload, no error row and nothing named
// (CN10). A path that reads "write back" is never asked.
func TestADiskWithNoWriteCacheReadsWriteThroughAtEveryLayer(t *testing.T) {
	srv, node := newTestServer(t)
	legPathFixture(t, srv, node, writeThrough)
	node.Reset()
	reply := syncupBoth(t, srv, reqOpts{revision: 3, primary: true})
	assertCodeZero(t, "the converge over a disk with no write cache", reply)
	if node.hasCall("rescan_controller") {
		t.Errorf("a path whose target reports no write cache was scanned " +
			"again")
	}
	if !node.hasCall("identify ") {
		t.Errorf("the primary did not ask the target of a path that reads " +
			"write through")
	}
	for _, legId := range []uint64{testMetaLeg, testDataLeg} {
		row := reply.GetCntlrInfo().GetLegIdToLeg()[legId]
		if row.GetStatus() == pb.ResStatus_RES_STATUS_ERROR {
			t.Errorf("leg %#x's row reads ERROR %q", legId, row.GetDetails())
		}
	}
	round, resent := resendRound(t, srv, cntlrReq(reqOpts{revision: 3,
		primary: true}), "nocache")
	assertCodeZero(t, "the read over a disk with no write cache", round)
	if resent != nil {
		t.Errorf("the round re-sent the request")
	}
}

// TestAPathThatOpensWithNoConvergeIsReadAgain: a primary whose leg path is
// live but not optimized when its converge runs, as a migration's new side
// is before its cutover, marks the leg; the disk node then opens the path
// over a device with a write cache, with no converge of the cntlr, and the
// next read asks the target, names the leg, and brings the re-send whose
// converge scans the path again (CN10). The read after that is clean and
// drops the mark.
func TestAPathThatOpensWithNoConvergeIsReadAgain(t *testing.T) {
	srv, node := newTestServer(t)
	nqns := legPathFixture(t, srv, node, writeThrough)
	dataNqn := nqns[1]
	node.setAnaState(dataNqn, testIp, testSvcId, "non-optimized")
	opts := reqOpts{revision: 3, primary: true, leftoverOk: true}
	syncupBoth(t, srv, opts)
	st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	if _, marked := st.wcStale[testDataLeg]; !marked {
		t.Fatalf("a primary's leg with a path not optimized is not watched")
	}
	node.setAnaState(dataNqn, testIp, testSvcId, "optimized")
	node.setTargetWc(dataNqn, testIp, testSvcId, writeBack)
	held := cntlrReq(opts)
	node.Reset()
	round, resent := resendRound(t, srv, held, "opened")
	assertCodeNames(t, "the read of a path that opened", round,
		legName(srv, testDataLeg))
	if resent == nil || !node.hasCall("rescan_controller=1") {
		t.Fatalf("the re-send did not scan the opened path again")
	}
	if got := pathWcacheOf(node, dataNqn); got != writeBack {
		t.Fatalf("the opened path reads %q after the re-send, want %q", got,
			writeBack)
	}
	round, resent = resendRound(t, srv, held, "settled")
	assertCodeZero(t, "the read after the rescan", round)
	if resent != nil {
		t.Errorf("the round after the rescan re-sent the request")
	}
	if _, marked := st.wcStale[testDataLeg]; marked {
		t.Errorf("the leg is still marked once every path is open and fresh")
	}
}

// TestAReadNamesAnOlderTdErrorOnceTheLoopDeviceIsThere: a td dm-error on the
// one-target table while the agent has its loop device reads OK with
// RES_REASON_DIFFERS, which names it, and the converge of the re-send loads
// the two-target table (CN15, CN30).
func TestAReadNamesAnOlderTdErrorOnceTheLoopDeviceIsThere(t *testing.T) {
	srv, node := newTestServer(t)
	opts := reqOpts{revision: 2}
	syncupBoth(t, srv, opts)
	errName := errorName(srv, testTd)
	node.mu.Lock()
	node.dms[errName].table = "0 " + itoa(testTdSize/512) + " error"
	node.mu.Unlock()
	round, resent := resendRound(t, srv, cntlrReq(opts), "older")
	assertCodeNames(t, "the read of an older td dm-error", round, errName)
	if resent == nil {
		t.Fatalf("the round that names the dm-error brought no re-send")
	}
	if got := strings.Count(dmTableOf(node, errName), "error"); got != 2 {
		t.Fatalf("the re-send left the td dm-error %q", dmTableOf(node, errName))
	}
}

// TestALoopDeviceWithNoWriteCacheIsDetached: a loop device that losetup made
// and that reads no write cache is detached again, and the td dm-error gets
// the one-target table (CN15).
func TestALoopDeviceWithNoWriteCacheIsDetached(t *testing.T) {
	srv, node := newTestServer(t)
	node.mu.Lock()
	node.loopWc = writeThrough
	node.mu.Unlock()
	syncupBoth(t, srv, reqOpts{revision: 2})
	if !node.hasCall("cmd losetup -d /dev/loop0") {
		t.Errorf("the loop device with no write cache was not detached")
	}
	if got := dmTableOf(node, errorName(srv, testTd)); got != "0 "+
		itoa(testTdSize/512)+" error" {
		t.Errorf("the td dm-error is %q, want the one-target table", got)
	}
}
