package dnagent

import (
	"context"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestPushOntoAServingDmClonePausesItsHolders is DN15 on a dm-clone that
// serves: the primary's per-CN dm-linear maps it, so the piece's discards go
// only once the background copy is stopped and that dm-linear is suspended,
// and both are restored after them (agent.ApplySkipRanges). The standby's
// dm-linear, on its own dm-error, is not touched.
func TestPushOntoAServingDmClonePausesItsHolders(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	primaryLin := nf.DnLinearName(testCluster, testDn, testSp, testSide,
		testCn0)
	standbyLin := nf.DnLinearName(testCluster, testDn, testSp, testSide,
		testCn1)
	cloneNo := node.devNo[nf.DmPath(cloneName)]
	if cloneNo == "" ||
		!strings.Contains(node.dms[primaryLin].table, cloneNo) {
		t.Fatalf("precondition: the primary's dm-linear %q does not map "+
			"the dm-clone %s", node.dms[primaryLin].table, cloneNo)
	}
	if node.dms[cloneName].noHydration {
		t.Fatal("precondition: the dm-clone's copy is off")
	}

	node.Reset()
	reply, err := srv.PushMigrBitmap(ctx, pushReq(0, testMigrId, []byte{0x05}))
	if err != nil || reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("PushMigrBitmap: %v, %v", err, reply.GetAgentReply())
	}
	assertOrder(t, node,
		"cmd dmsetup message "+cloneName+" 0 disable_hydration",
		"cmd dmsetup suspend "+primaryLin,
		"discard --offset 3145728 --length 1048576 "+nf.DmPath(cloneName),
		"cmd dmsetup resume "+primaryLin,
		"cmd dmsetup message "+cloneName+" 0 enable_hydration",
	)
	if node.hasCall("cmd dmsetup suspend " + standbyLin) {
		t.Error("the standby's dm-linear, which maps its dm-error, was paused")
	}
	if node.dms[primaryLin].suspended || node.dms[cloneName].noHydration {
		t.Fatal("the push left the dm-linear suspended or the copy off")
	}
}

// TestPushOntoASuspendedDmCloneWaitsForItsResume: a piece for a suspended
// dm-clone sends no discard, which would wait for the
// resume with the side's lock held, and still counts as applied; the
// converge that resumes the dm-clone applies it then (DN15).
func TestPushOntoASuspendedDmCloneWaitsForItsResume(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	node.dms[cloneName].suspended = true

	node.Reset()
	reply, err := srv.PushMigrBitmap(ctx, pushReq(0, testMigrId, []byte{0x05}))
	if err != nil || reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("PushMigrBitmap: %v, %v", err, reply.GetAgentReply())
	}
	if node.hasCall("discard --offset") ||
		node.hasCall("cmd dmsetup suspend") ||
		node.hasCall("disable_hydration") {
		t.Fatalf("a suspended dm-clone was touched: %q", node.Calls())
	}
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	if got := srv.bitmapInfo(st).GetBmIdxList(); len(got) != 1 ||
		got[0] != 0 {
		t.Fatalf("applied set = %v, want the piece counted", got)
	}

	node.Reset()
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertOrder(t, node,
		"cmd dmsetup resume "+cloneName,
		"discard --offset 3145728 --length 1048576 "+nf.DmPath(cloneName),
	)
}

// TestAReloadedDmCloneGetsItsPiecesAgain is DN15's re-apply on a reload: a
// converge that finds the dm-clone's table differing reloads it, and applies
// the stored pieces again after the reload.
func TestAReloadedDmCloneGetsItsPiecesAgain(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if _, err := srv.PushMigrBitmap(ctx,
		pushReq(0, testMigrId, []byte{0x05})); err != nil {
		t.Fatalf("PushMigrBitmap: %v", err)
	}
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	// The table the kernel holds no longer has the wanted region size, so
	// the converge reloads it (ensureDmClone).
	node.mu.Lock()
	dm := node.dms[cloneName]
	fields := strings.Fields(dm.table)
	fields[6] = "16"
	dm.table = strings.Join(fields, " ")
	node.mu.Unlock()

	node.Reset()
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertOrder(t, node,
		"cmd dmsetup reload "+cloneName,
		"discard --offset 3145728 --length 1048576 "+nf.DmPath(cloneName),
	)
}
