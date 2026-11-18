package dnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// seedUnsetExport seeds a :2: export whose namespace one has no device
// written: what a build abandoned between its namespace `mkdir` and its
// device_path write leaves. The kernel prints the path as "(null)" and never
// enables such a namespace.
func seedUnsetExport(node *fakeNode, nqn string) {
	seedExport(node, nqn, agent.NsDevicePathUnset)
	node.mu.Lock()
	defer node.mu.Unlock()
	node.files[fmt.Sprintf("%s/subsystems/%s/namespaces/%d/enable",
		agent.NvmetRoot, nqn, sideNsid)] = "0\n"
}

// TestAnExportWithNoDevicePathIsJudgedByAge pins DN6's rule for an export
// whose namespace one has no device written: it names nobody and never
// serves, so it counts as an export with no namespace one — its age, then
// its port links decide, in both scopes and in the read-only verdict as in
// the Syncup's pass. Young, it is left alone whatever port it is on; old, on
// no port or on ours, it is an orphan and goes; old, on a sibling's port, it
// is that sibling's. Read as a namespace naming a device no owner can be read
// off, it would be foreign to every agent, and no sweep would ever remove it.
func TestAnExportWithNoDevicePathIsJudgedByAge(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	for _, scope := range sweepScopes() {
		for _, tc := range []struct {
			name   string
			old    bool
			portId int // 0: linked to no port
			gone   bool
		}{
			{"young, no port", false, 0, false},
			{"young, our port", false, common.NvmetPortId, false},
			{"old, no port", true, 0, true},
			{"old, our port", true, common.NvmetPortId, true},
			{"old, sibling's port", true, siblingPort, false},
		} {
			t.Run(scope.name+"/"+tc.name, func(t *testing.T) {
				srv, node := newTestServer(t)
				ctx := context.Background()
				ours := sweepSidePtr(testLeg, testSide)
				if _, err := srv.SyncupDn(ctx,
					sweepDnReq(1, ours)); err != nil {
					t.Fatalf("SyncupDn: %v", err)
				}
				syncupSideTwoPhase(t, srv, sweepSideReq(1, ours))
				ourObjs := objectsOf(ours)

				stray := scope.stray(nf)
				dir := agent.NvmetRoot + "/subsystems/" + stray
				seedUnsetExport(node, stray)
				addPort(node, siblingPort)
				if tc.portId != 0 {
					linkPort(node, tc.portId, stray)
				}
				if tc.old {
					node.ageDir(dir, common.DnExportOrphanGrace+time.Minute)
				}

				node.Reset()
				verdict := scope.read(t, srv)
				switch {
				case tc.gone && (verdict.GetCode() !=
					common.ReplyCodeLeftover ||
					!strings.Contains(verdict.GetDetails(), stray)):
					t.Fatalf("read-only verdict %d %q, want %d naming %s",
						verdict.GetCode(), verdict.GetDetails(),
						common.ReplyCodeLeftover, stray)
				case !tc.gone && verdict.GetCode() != 0:
					t.Fatalf("read-only verdict %d %q, want 0: %s is no "+
						"leftover", verdict.GetCode(), verdict.GetDetails(),
						stray)
				}
				if got := ageReads(node, dir); got != 1 {
					t.Errorf("the read-only verdict read the age of %s %d "+
						"times, want 1", stray, got)
				}

				node.Reset()
				reply := scope.run(t, srv, 2)
				if got := reply.GetCode(); got != 0 {
					t.Fatalf("code = %d (%s), want 0", got,
						reply.GetDetails())
				}
				switch present := subsysPresent(node, stray); {
				case tc.gone && present:
					t.Fatalf("the sweep kept %s, an old export with no "+
						"device on no port but ours", stray)
				case !tc.gone && !present:
					t.Fatalf("the sweep removed %s", stray)
				case !tc.gone && node.hasCall("cmd rmdir "+dir):
					t.Fatalf("the sweep attempted rmdir on %s", stray)
				}
				ourObjs.assertAllKept(t, node)
			})
		}
	}
}

// TestAnSpWaitsForAnExportWithNoDevicePath pins DN6's "an sp waits for its
// exports" for the same shape: the side dropped is the last of its sp on
// this node, and its export stands with namespace one unwritten and no port
// link, as a build abandoned there leaves it. While the export is young the
// sweep cannot settle it, so the side device and its record stay, unnamed —
// the layers above them go as they would; once the export is older than the
// grace a read names it, and the SyncupDn that brings removes it, the side
// device and the record. Taken for a foreign export, it would let the sweep
// take the side device at once and never look at the export again.
func TestAnSpWaitsForAnExportWithNoDevicePath(t *testing.T) {
	srv, node := newTestServer(t)
	clock := withDnClock(srv, node)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	objs := objectsOf(sidePtr(testSide))
	objs.assertAllPresent(t, node)
	nqn := srv.nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0)
	dir := agent.NvmetRoot + "/subsystems/" + nqn
	node.mu.Lock()
	for link := range node.links {
		if strings.HasSuffix(link, "/subsystems/"+nqn) {
			delete(node.links, link)
		}
	}
	node.files[fmt.Sprintf("%s/namespaces/%d/device_path", dir, sideNsid)] =
		agent.NsDevicePathUnset + "\n"
	node.files[fmt.Sprintf("%s/namespaces/%d/enable", dir, sideNsid)] = "0\n"
	node.mtimes[dir] = clock.Now().Unix()
	node.mu.Unlock()

	read := func(what string) *pb.CheckDnReply {
		t.Helper()
		reply, _ := checkDnNow(srv, traced(what), &pb.CheckDnRequest{
			ClusterId: testCluster, DnId: testDn, Revision: 2,
			ShowInfo: true,
		}, nil)
		return reply
	}
	recordHeld := func() bool {
		t.Helper()
		_, ok, err := srv.meta.LookupSide(ctx, testSp, testSide)
		if err != nil {
			t.Fatalf("LookupSide: %v", err)
		}
		return ok
	}

	reply, err := srv.SyncupDn(ctx, dnReq(2))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	assertNotNamed(t, "the drop inside the grace", reply)
	assertNotNamed(t, "the read inside the grace", read("young-read"))
	if !subsysPresent(node, nqn) {
		t.Fatalf("the young export was removed")
	}
	if !dmPresent(node, objs.sideDev) {
		t.Fatalf("the side device went while an export of its sp is not " +
			"settled: no sweep would judge that export again")
	}
	if !recordHeld() {
		t.Fatalf("the side's record was freed under its device")
	}

	clock.mu.Lock()
	clock.now = clock.now.Add(common.DnExportOrphanGrace + time.Second)
	clock.mu.Unlock()
	assertNamed(t, "the read past the grace", read("aged-read"), nqn)
	reply, err = srv.SyncupDn(ctx, dnReq(2))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	assertNotNamed(t, "the SyncupDn past the grace", reply)
	objs.assertAllGone(t, node)
	if recordHeld() {
		t.Error("the side's record survived its devices")
	}
}
