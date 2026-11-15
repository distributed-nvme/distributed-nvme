package cnagent

import (
	"context"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestPushOntoAServingDmClonePausesItsHolders is CN22 on a dm-clone that
// serves: the destination's namespace device maps it, so the chunk's
// discards go only once the background copy is stopped and that device is
// suspended, and both are restored after them (agent.ApplySkipRanges).
func TestPushOntoAServingDmClonePausesItsHolders(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	clone := cloneName(srv, testClone)
	nsDev := nsDevName(srv, testNs)
	cloneNo := node.devNo["/dev/mapper/"+clone]
	if cloneNo == "" || !strings.Contains(node.dms[nsDev].table, cloneNo) {
		t.Fatalf("precondition: the namespace device %q does not map the "+
			"dm-clone %s", node.dms[nsDev].table, cloneNo)
	}
	if node.dms[clone].noHydration {
		t.Fatal("precondition: the dm-clone's copy is off")
	}

	node.Reset()
	pushBitmap(t, srv, hexBytes(t, testSkipHex))
	assertOrder(t, node,
		"cmd dmsetup message "+clone+" 0 disable_hydration",
		"cmd dmsetup suspend "+nsDev,
		"discard --offset 33554432 --length 33554432",
		"cmd dmsetup resume "+nsDev,
		"cmd dmsetup message "+clone+" 0 enable_hydration",
	)
	if node.dms[nsDev].suspended || node.dms[clone].noHydration {
		t.Fatal("the push left the namespace device suspended or the copy " +
			"off")
	}
}

// TestPushOntoASuspendedDmCloneWaitsForItsResume is item 1 of the review on
// a controller node: a chunk for a suspended dm-clone sends no discard and
// still counts as applied; the converge that resumes the dm-clone applies it
// then (CN22, CN18 step 4).
func TestPushOntoASuspendedDmCloneWaitsForItsResume(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{
		revision: 2, primary: true, clones: []*pb.Clone{cloneOf()}})
	clone := cloneName(srv, testClone)
	node.dms[clone].suspended = true

	node.Reset()
	pushBitmap(t, srv, hexBytes(t, testSkipHex))
	assertNoCall(t, node, "discard --offset")
	assertNoCall(t, node, "cmd dmsetup suspend")
	assertNoCall(t, node, "disable_hydration")

	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, clones: []*pb.Clone{cloneOf()}}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if got := chunkIds(reply.GetBmInfoList()[0]); len(got) != 1 ||
		got[0] != [2]uint32{0, 0} {
		t.Fatalf("the chunk was not reported applied: %v", got)
	}
	assertOrder(t, node,
		"cmd dmsetup resume "+clone,
		"discard --offset 33554432 --length 33554432",
	)
}
