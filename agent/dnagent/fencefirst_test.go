package dnagent

import (
	"context"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// fenceNames are the fixture side's devices and exports a failover from
// testCn0 to testCn1 touches.
type fenceNames struct {
	side, oldLin, oldErr, newLin, newErr, oldNqn, newNqn string
}

func newFenceNames(srv *DnAgentServer) fenceNames {
	nf := srv.nf
	return fenceNames{
		side:   nf.DnSideName(testCluster, testDn, testSp, testSide),
		oldLin: nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0),
		oldErr: nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0),
		newLin: nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1),
		newErr: nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn1),
		oldNqn: nf.SideToCnNqn(testCluster, testSp, testLeg, testCn0),
		newNqn: nf.SideToCnNqn(testCluster, testSp, testLeg, testCn1),
	}
}

// readFixtureSide is a CheckSide round of the fixture side that answers from
// a read made now.
func readFixtureSide(
	t *testing.T,
	srv *DnAgentServer,
	revision uint64,
) *pb.CheckSideReply {
	t.Helper()
	round, _ := checkSideNow(srv, context.Background(), &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn,
		SidePointer: sidePtr(testSide), Revision: revision, ShowInfo: true,
	}, nil)
	return round
}

// assertPrimaryClosed checks a failover pass that could not fence the old
// primary: the new primary's dm-linear stays on its own dm-error, never
// released onto the side device, its namespace inaccessible, its row
// RES_STATUS_PENDING, and the code names it, on the converge and on the read
// alike (DN10, DN16).
func assertPrimaryClosed(
	t *testing.T,
	srv *DnAgentServer,
	node *fakeNode,
	n fenceNames,
	reply *pb.SyncupSideReply,
	what string,
) {
	t.Helper()
	sideNo := node.devNo[srv.nf.DmPath(n.side)]
	if !linearMapsDm(node, n.newLin, n.newErr) ||
		releasedOntoDev(node, n.newLin, sideNo) {
		t.Errorf("%s: the new primary's path opened before the old "+
			"primary's fence", what)
	}
	if grp := anaGrpOf(t, srv, node, n.newNqn); grp !=
		common.AnaGrpIdInaccessible {
		t.Errorf("%s: the new primary's namespace is in group %d, want "+
			"inaccessible", what, grp)
	}
	row := reply.GetSideInfo().GetCnIdToDmLinear()[testCn1]
	if row.GetStatus() != pb.ResStatus_RES_STATUS_PENDING ||
		!strings.HasPrefix(row.GetDetails(), detailsFenceUnproven) {
		t.Errorf("%s: the new primary's row is %v/%q, want PENDING with "+
			"the fence's gap", what, row.GetStatus(), row.GetDetails())
	}
	if got := reply.GetAgentReply(); got.GetCode() !=
		common.ReplyCodeLeftover || !strings.Contains(
		got.GetDetails(), n.newLin) {
		t.Errorf("%s: the converge answered %v, want the new primary's "+
			"dm-linear named", what, got)
	}
	round := readFixtureSide(t, srv, 2)
	if read := round.GetSideInfo().GetCnIdToDmLinear()[testCn1]; read.
		GetStatus() != pb.ResStatus_RES_STATUS_PENDING {
		t.Errorf("%s: the read's row of the new primary is %v/%q, want "+
			"PENDING", what, read.GetStatus(), read.GetDetails())
	}
	if got := round.GetAgentReply(); got.GetCode() !=
		common.ReplyCodeLeftover || !strings.Contains(
		got.GetDetails(), n.newLin) {
		t.Errorf("%s: the read answered %v, want the new primary's "+
			"dm-linear named", what, got)
	}
	if exp := round.GetSideInfo().GetCnIdToNvmeof()[testCn1]; exp.
		GetStatus() != pb.ResStatus_RES_STATUS_OK || exp.GetReason() !=
		pb.ResReason_RES_REASON_NONE {
		t.Errorf("%s: the read's export row of the new primary is "+
			"%v/%q/%v, want OK: its group follows its closed path", what,
			exp.GetStatus(), exp.GetDetails(), exp.GetReason())
	}
}

// assertPrimaryOpen checks a pass that fenced the old primary and then
// opened the new one's path: the old primary's dm-linear on its dm-error,
// the new one's on the side device, the groups moved, code zero.
func assertPrimaryOpen(
	t *testing.T,
	srv *DnAgentServer,
	node *fakeNode,
	n fenceNames,
	reply *pb.SyncupSideReply,
	what string,
) {
	t.Helper()
	sideNo := node.devNo[srv.nf.DmPath(n.side)]
	if !linearMapsDm(node, n.oldLin, n.oldErr) || dmSuspended(node, n.oldLin) ||
		releasedOntoDev(node, n.oldLin, sideNo) {
		t.Errorf("%s: the old primary's dm-linear is not fenced on its "+
			"dm-error", what)
	}
	if !linearMapsDm(node, n.newLin, n.side) || dmSuspended(node, n.newLin) {
		t.Errorf("%s: the new primary's dm-linear does not serve on the "+
			"side device", what)
	}
	if grp := anaGrpOf(t, srv, node, n.newNqn); grp !=
		common.AnaGrpIdOptimized {
		t.Errorf("%s: the new primary's namespace is in group %d, want "+
			"optimized", what, grp)
	}
	if grp := anaGrpOf(t, srv, node, n.oldNqn); grp ==
		common.AnaGrpIdOptimized {
		t.Errorf("%s: the old primary's namespace is still optimized", what)
	}
	if got := reply.GetAgentReply(); got.GetCode() != 0 {
		t.Errorf("%s: the converge answered %v, want code zero", what, got)
	}
}

// At a failover the old primary's dm-linear is fenced before the new
// primary's path opens (DN10). Each step of that fence failing in turn
// leaves the new primary's dm-linear on its own dm-error and its namespace
// inaccessible, and the code names it, so the worker sends the syncup again;
// the next converge fences the old primary and opens the new one's path.
func TestAFailedFenceStepKeepsTheNewPrimaryClosed(t *testing.T) {
	for _, c := range []struct {
		name string
		// unanswered: the step's read did not answer; a refused
		// `dmsetup info` reads as an absent device (agent.Dm.Info).
		unanswered bool
		key        func(n fenceNames) string
	}{
		{"the dm-error step", true, func(n fenceNames) string {
			return "dmsetup info --columns --noheadings -o attr " + n.oldErr
		}},
		{"the dm-error's number", false, func(n fenceNames) string {
			return "lsblk --nodeps --noheadings --output MAJ:MIN " +
				"/dev/mapper/" + n.oldErr
		}},
		{"the dm-linear's info", true, func(n fenceNames) string {
			return "dmsetup info --columns --noheadings -o attr " + n.oldLin
		}},
		{"the dm-linear's table", false, func(n fenceNames) string {
			return "dmsetup table " + n.oldLin
		}},
		{"the suspend", false, func(n fenceNames) string {
			return "dmsetup suspend " + n.oldLin
		}},
		{"the load", false, func(n fenceNames) string {
			return "dmsetup reload " + n.oldLin
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			n := newFenceNames(srv)
			key := c.key(n)

			node.Reset()
			node.mu.Lock()
			if c.unanswered {
				node.killCmdNoEffectAlways[key] = true
			} else {
				node.failCmdAlways[key] = "refused"
			}
			node.mu.Unlock()
			reply, err := srv.SyncupSide(ctx, failoverReq(2))
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if linearMapsDm(node, n.oldLin, n.oldErr) &&
				!dmSuspended(node, n.oldLin) {
				t.Fatal("fixture is wrong: the old primary was fenced")
			}
			assertPrimaryClosed(t, srv, node, n, reply, c.name)

			node.mu.Lock()
			delete(node.failCmdAlways, key)
			delete(node.killCmdNoEffectAlways, key)
			node.mu.Unlock()
			node.Reset()
			reply, err = srv.SyncupSide(ctx, failoverReq(2))
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			assertPrimaryOpen(t, srv, node, n, reply, "the next converge")
		})
	}
}

// A dm-linear left suspended on the side device by a load that failed is
// not fenced: a resume would let the IO it holds reach the side. It keeps
// the primary's path closed, and the pass's pre-step does not resume it.
func TestASuspendedLinearOnTheSideDeviceKeepsThePathClosed(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	n := newFenceNames(srv)
	sideNo := node.devNo[srv.nf.DmPath(n.side)]

	node.failCmd["dmsetup reload "+n.oldLin] = "refused once"
	reply, err := srv.SyncupSide(ctx, failoverReq(2))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !dmSuspended(node, n.oldLin) || !linearMapsDm(node, n.oldLin, n.side) {
		t.Fatal("fixture is wrong: the old primary's dm-linear is not " +
			"suspended on the side device")
	}
	assertPrimaryClosed(t, srv, node, n, reply, "the failed load")
	if releasedOntoDev(node, n.oldLin, sideNo) {
		t.Error("the old primary's held IO was released onto the side " +
			"device")
	}

	node.Reset()
	reply, err = srv.SyncupSide(ctx, failoverReq(2))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if releasedOntoDev(node, n.oldLin, sideNo) {
		t.Error("the next pass released the old primary's held IO onto " +
			"the side device")
	}
	assertPrimaryOpen(t, srv, node, n, reply, "the next converge")
}

// A dm-linear the desired state no longer names counts too: while one of
// the side still maps the side device — its removal failed — the primary's
// path stays closed (the big plan's decision 7).
func TestAnUnwantedLinearOnTheSideDeviceKeepsThePathClosed(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	n := newFenceNames(srv)
	sideNo := node.devNo[srv.nf.DmPath(n.side)]

	// The old primary leaves the side's CNs altogether; its dm-linear,
	// still on the side device, cannot be removed.
	gone := sideReq(2, testSide, testCn1, nil,
		pb.SpLevel_SP_LEVEL_READWRITE)
	node.mu.Lock()
	node.failCmdAlways["dmsetup remove "+n.oldLin] = "Device or resource busy"
	node.failCmdAlways["dmsetup suspend "+n.oldLin] = "refused"
	node.mu.Unlock()
	reply, err := srv.SyncupSide(ctx, gone)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !linearMapsDm(node, n.oldLin, n.side) {
		t.Fatal("fixture is wrong: the unwanted dm-linear left the side " +
			"device")
	}
	if !linearMapsDm(node, n.newLin, n.newErr) ||
		releasedOntoDev(node, n.newLin, sideNo) {
		t.Error("the new primary's path opened beside a dm-linear that " +
			"still maps the side device")
	}
	if grp := anaGrpOf(t, srv, node, n.newNqn); grp !=
		common.AnaGrpIdInaccessible {
		t.Errorf("the new primary's namespace is in group %d, want "+
			"inaccessible", grp)
	}
	if row := reply.GetSideInfo().GetCnIdToDmLinear()[testCn1]; row.
		GetStatus() != pb.ResStatus_RES_STATUS_PENDING ||
		!strings.Contains(row.GetDetails(), n.oldLin) {
		t.Errorf("the new primary's row is %v/%q, want PENDING naming %s",
			row.GetStatus(), row.GetDetails(), n.oldLin)
	}

	node.mu.Lock()
	delete(node.failCmdAlways, "dmsetup remove "+n.oldLin)
	delete(node.failCmdAlways, "dmsetup suspend "+n.oldLin)
	node.mu.Unlock()
	node.Reset()
	reply, err = srv.SyncupSide(ctx, gone)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if dmPresent(node, n.oldLin) {
		t.Error("the unwanted dm-linear survived the next converge")
	}
	if !linearMapsDm(node, n.newLin, n.side) ||
		anaGrpOf(t, srv, node, n.newNqn) != common.AnaGrpIdOptimized {
		t.Error("the next converge did not open the new primary's path")
	}
	if got := reply.GetAgentReply(); got.GetCode() != 0 {
		t.Errorf("the next converge answered %v, want code zero", got)
	}
}

// The check guards opening a path only: a primary's dm-linear already live
// on the side device stays there, and its namespace optimized, when the read
// of another dm-linear does not answer (the big plan's decision 10).
func TestAServingPrimaryStaysWhenAnotherReadFails(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	n := newFenceNames(srv)

	// cn0 is the primary; cn1's dm-linear does not answer.
	node.mu.Lock()
	node.failCmdAlways["dmsetup table "+n.newLin] = "refused"
	node.mu.Unlock()
	node.Reset()
	reply, err := srv.SyncupSide(ctx, sideReq(1, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	round := readFixtureSide(t, srv, 1)
	node.mu.Lock()
	delete(node.failCmdAlways, "dmsetup table "+n.newLin)
	node.mu.Unlock()
	if !linearMapsDm(node, n.oldLin, n.side) || dmSuspended(node, n.oldLin) {
		t.Error("the serving primary's dm-linear left the side device")
	}
	if grp := anaGrpOf(t, srv, node, n.oldNqn); grp !=
		common.AnaGrpIdOptimized {
		t.Errorf("the serving primary's namespace is in group %d, want "+
			"optimized", grp)
	}
	for _, call := range node.Mutations() {
		if strings.Contains(call, n.oldLin) || strings.Contains(call,
			srv.nvmet.NsPath(n.oldNqn, sideNsid)) {
			t.Errorf("the pass touched the serving primary: %q", call)
		}
	}
	for what, row := range map[string]*pb.ResInfo{
		"the converge's": reply.GetSideInfo().GetCnIdToDmLinear()[testCn0],
		"the read's":     round.GetSideInfo().GetCnIdToDmLinear()[testCn0],
	} {
		if row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("%s row of the serving primary is %v/%q, want OK",
				what, row.GetStatus(), row.GetDetails())
		}
	}
}

// The pass's pre-step opens no path either: a primary's dm-linear suspended
// on the side device is not resumed while another dm-linear of the side is
// not fenced, and the build keeps it suspended, holding its own IO, rather
// than fail that IO on its dm-error; its namespace is inaccessible meanwhile.
// The pass that fences the other resumes it there.
func TestAHeldPrimaryIsResumedOnlyAfterTheFence(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	n := newFenceNames(srv)
	newErrNo := node.devNo[srv.nf.DmPath(n.newErr)]

	// A failover whose old-primary step failed before its load, then a
	// later pass that left the new primary's dm-linear suspended on the side
	// device: both stand on the side device, the old one live.
	node.mu.Lock()
	node.failCmdAlways["dmsetup suspend "+n.oldLin] = "refused"
	node.mu.Unlock()
	if _, err := srv.SyncupSide(ctx, failoverReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	lin := node.dms[n.newLin]
	lin.table = strings.Replace(lin.table, " linear "+newErrNo+" ",
		" linear "+node.devNo[srv.nf.DmPath(n.side)]+" ", 1)
	lin.suspended = true
	node.mu.Unlock()
	if !linearMapsDm(node, n.newLin, n.side) {
		t.Fatal("fixture is wrong: the new primary is not on the side device")
	}

	node.Reset()
	reply, err := srv.SyncupSide(ctx, failoverReq(2))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !dmSuspended(node, n.newLin) || len(node.releasedOnto[n.newLin]) != 0 {
		t.Error("the pass ended the held primary's suspension while the " +
			"old primary was not fenced")
	}
	if row := reply.GetSideInfo().GetCnIdToDmLinear()[testCn1]; row.
		GetStatus() != pb.ResStatus_RES_STATUS_PENDING {
		t.Errorf("the held primary's row is %v/%q, want PENDING",
			row.GetStatus(), row.GetDetails())
	}
	// A held path is not pointed at: the cntlr would count its leg as
	// available and read through it, and its reads would wait for the
	// fence. The read judges the export by the same rule.
	if grp := anaGrpOf(t, srv, node, n.newNqn); grp !=
		common.AnaGrpIdInaccessible {
		t.Errorf("the held primary's namespace is in group %d, want "+
			"inaccessible", grp)
	}
	round := readFixtureSide(t, srv, 2)
	if exp := round.GetSideInfo().GetCnIdToNvmeof()[testCn1]; exp.
		GetStatus() != pb.ResStatus_RES_STATUS_OK || exp.GetReason() !=
		pb.ResReason_RES_REASON_NONE {
		t.Errorf("the read's export row of the held primary is %v/%q/%v, "+
			"want OK", exp.GetStatus(), exp.GetDetails(), exp.GetReason())
	}

	node.mu.Lock()
	delete(node.failCmdAlways, "dmsetup suspend "+n.oldLin)
	node.mu.Unlock()
	node.Reset()
	reply, err = srv.SyncupSide(ctx, failoverReq(2))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if releasedOntoDev(node, n.newLin, newErrNo) {
		t.Error("the held primary's IO was failed on its dm-error")
	}
	assertPrimaryOpen(t, srv, node, n, reply, "the pass after the fence")
}

// While the fence is not proven, the primary's step changes nothing that
// serves: a primary's dm-linear live on a device that is neither the side
// device nor its own dm-error is left as it is, not reloaded onto its
// dm-error, and the next pass that proves the fence puts it on the side
// device.
func TestAnUnprovenFenceLeavesAPrimaryOnAnotherDevice(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	n := newFenceNames(srv)
	sideNo := node.devNo[srv.nf.DmPath(n.side)]
	newErrNo := node.devNo[srv.nf.DmPath(n.newErr)]
	oldErrNo := node.devNo[srv.nf.DmPath(n.oldErr)]

	// The primary's dm-linear maps cn0's dm-error, a device that is
	// neither the side device nor its own dm-error; cn0's dm-linear stays
	// live on the side device.
	node.mu.Lock()
	lin := node.dms[n.newLin]
	lin.table = strings.Replace(lin.table, " linear "+newErrNo+" ",
		" linear "+oldErrNo+" ", 1)
	node.failCmdAlways["dmsetup suspend "+n.oldLin] = "refused"
	node.mu.Unlock()

	node.Reset()
	reply, err := srv.SyncupSide(ctx, failoverReq(2))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	for _, call := range node.Mutations() {
		if strings.Contains(call, "dmsetup") &&
			strings.Contains(call, n.newLin) {
			t.Errorf("the pass changed the primary's dm-linear: %q", call)
		}
	}
	if row := reply.GetSideInfo().GetCnIdToDmLinear()[testCn1]; row.
		GetStatus() != pb.ResStatus_RES_STATUS_PENDING {
		t.Errorf("the primary's row is %v/%q, want PENDING",
			row.GetStatus(), row.GetDetails())
	}

	node.mu.Lock()
	delete(node.failCmdAlways, "dmsetup suspend "+n.oldLin)
	node.mu.Unlock()
	node.Reset()
	reply, err = srv.SyncupSide(ctx, failoverReq(2))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !linearMapsDm(node, n.newLin, n.side) ||
		!releasedOntoDev(node, n.newLin, sideNo) {
		t.Error("the pass that proved the fence did not put the primary " +
			"on the side device")
	}
	assertPrimaryOpen(t, srv, node, n, reply, "the pass after the fence")
}

// A finished migration's repoint of the primary's dm-linear from its
// dm-clone onto the side device opens no path: the dm-clone writes to the
// side device already. So it does not wait for the fence check, and a read
// of another dm-linear that does not answer neither keeps the dm-clone in
// place nor cuts the primary (DN10).
func TestAFinishedMigrationsRepointNeedsNoFenceCheck(t *testing.T) {
	srv, node := newTestServer(t)
	nf := srv.nf
	ctx := context.Background()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	provisionSide(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	n := newFenceNames(srv)
	cloneName := nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId)
	if !linearMapsDm(node, n.oldLin, cloneName) {
		t.Fatal("fixture is wrong: the primary does not serve through the " +
			"dm-clone")
	}

	node.mu.Lock()
	node.killCmdNoEffectAlways["dmsetup table "+n.newLin] = true
	node.mu.Unlock()
	node.Reset()
	reply, err := srv.SyncupSide(ctx, sideReq(2, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	delete(node.killCmdNoEffectAlways, "dmsetup table "+n.newLin)
	node.mu.Unlock()
	if !linearMapsDm(node, n.oldLin, n.side) || dmSuspended(node, n.oldLin) {
		t.Error("the primary's dm-linear was not repointed onto the side " +
			"device")
	}
	if dmPresent(node, cloneName) {
		t.Error("the finished migration's dm-clone stayed")
	}
	if grp := anaGrpOf(t, srv, node, n.oldNqn); grp !=
		common.AnaGrpIdOptimized {
		t.Errorf("the primary's namespace is in group %d, want optimized",
			grp)
	}
	if row := reply.GetSideInfo().GetCnIdToDmLinear()[testCn0]; row.
		GetStatus() != pb.ResStatus_RES_STATUS_OK {
		t.Errorf("the primary's row is %v/%q, want OK", row.GetStatus(),
			row.GetDetails())
	}
}

// A promotion whose reload onto the side device was cut after its load
// heads there: the next pass finishes it. While the fence is not proven in
// that pass, the step keeps it as it is — suspended, its reload waiting, its
// namespace optimized — rather than resume it on its dm-error, which would
// drop the waiting table and fail the IO it holds (DN10).
func TestACutPromotionWaitsForTheFence(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	n := newFenceNames(srv)
	newErrNo := node.devNo[srv.nf.DmPath(n.newErr)]

	// The failover's pass fences the old primary, loads the new one's
	// reload onto the side device, and is refused its resume.
	node.failCmd["dmsetup resume "+n.newLin] = "refused once"
	if _, err := srv.SyncupSide(ctx, failoverReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	waiting := node.dms[n.newLin].waiting != nil
	node.mu.Unlock()
	if !waiting || !dmSuspended(node, n.newLin) {
		t.Fatal("fixture is wrong: the promotion's reload did not stop " +
			"after its load")
	}

	// The next pass cannot read the old primary's dm-linear.
	node.mu.Lock()
	node.killCmdNoEffectAlways["dmsetup table "+n.oldLin] = true
	node.mu.Unlock()
	node.Reset()
	reply, err := srv.SyncupSide(ctx, failoverReq(2))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	delete(node.killCmdNoEffectAlways, "dmsetup table "+n.oldLin)
	waiting = node.dms[n.newLin].waiting != nil
	node.mu.Unlock()
	if !waiting || !dmSuspended(node, n.newLin) ||
		releasedOntoDev(node, n.newLin, newErrNo) {
		t.Error("the pass ended the cut promotion while the fence was " +
			"not proven")
	}
	if grp := anaGrpOf(t, srv, node, n.newNqn); grp !=
		common.AnaGrpIdOptimized {
		t.Errorf("the cut promotion's namespace is in group %d, want "+
			"optimized", grp)
	}
	if row := reply.GetSideInfo().GetCnIdToDmLinear()[testCn1]; row.
		GetStatus() != pb.ResStatus_RES_STATUS_PENDING {
		t.Errorf("the cut promotion's row is %v/%q, want PENDING",
			row.GetStatus(), row.GetDetails())
	}
	// The read judges it alike: its path is open, so its export is as
	// wanted while its dm-linear's row waits for the fence.
	node.mu.Lock()
	node.killCmdNoEffectAlways["dmsetup table "+n.oldLin] = true
	node.mu.Unlock()
	round := readFixtureSide(t, srv, 2)
	node.mu.Lock()
	delete(node.killCmdNoEffectAlways, "dmsetup table "+n.oldLin)
	node.mu.Unlock()
	if row := round.GetSideInfo().GetCnIdToDmLinear()[testCn1]; row.
		GetStatus() != pb.ResStatus_RES_STATUS_PENDING {
		t.Errorf("the read's row of the cut promotion is %v/%q, want "+
			"PENDING", row.GetStatus(), row.GetDetails())
	}
	if exp := round.GetSideInfo().GetCnIdToNvmeof()[testCn1]; exp.
		GetStatus() != pb.ResStatus_RES_STATUS_OK || exp.GetReason() !=
		pb.ResReason_RES_REASON_NONE {
		t.Errorf("the read's export row of the cut promotion is %v/%q/%v, "+
			"want OK", exp.GetStatus(), exp.GetDetails(), exp.GetReason())
	}

	node.Reset()
	reply, err = srv.SyncupSide(ctx, failoverReq(2))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertPrimaryOpen(t, srv, node, n, reply, "the pass after the fence")
}

// A primary that has no dm-linear yet gets one on its own dm-error while
// the fence is not proven, so its export has a device and serves no host,
// and the pass that proves the fence opens it.
func TestANewPrimaryWithNoLinearStartsOnItsDmError(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	n := newFenceNames(srv)
	nf := srv.nf
	const newCn = uint64(9)
	lin := nf.DnLinearName(testCluster, testDn, testSp, testSide, newCn)
	errName := nf.DnErrorName(testCluster, testDn, testSp, testSide, newCn)
	nqn := nf.SideToCnNqn(testCluster, testSp, testLeg, newCn)
	req := sideReq(2, testSide, newCn, []uint64{testCn0, testCn1},
		pb.SpLevel_SP_LEVEL_READWRITE)

	node.mu.Lock()
	node.failCmdAlways["dmsetup suspend "+n.oldLin] = "refused"
	node.mu.Unlock()
	reply, err := srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !linearMapsDm(node, lin, errName) {
		t.Error("the new primary's dm-linear was not built on its dm-error")
	}
	if grp := anaGrpOf(t, srv, node, nqn); grp !=
		common.AnaGrpIdInaccessible {
		t.Errorf("the new primary's namespace is in group %d, want "+
			"inaccessible", grp)
	}
	if row := reply.GetSideInfo().GetCnIdToDmLinear()[newCn]; row.
		GetStatus() != pb.ResStatus_RES_STATUS_PENDING {
		t.Errorf("the new primary's row is %v/%q, want PENDING",
			row.GetStatus(), row.GetDetails())
	}

	node.mu.Lock()
	delete(node.failCmdAlways, "dmsetup suspend "+n.oldLin)
	node.mu.Unlock()
	if _, err := srv.SyncupSide(ctx, req); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !linearMapsDm(node, lin, n.side) || anaGrpOf(t, srv, node, nqn) !=
		common.AnaGrpIdOptimized {
		t.Error("the pass that proved the fence did not open the new " +
			"primary's path")
	}
}

// A read of the primary's own dm-linear, or of the side device's number,
// that does not answer leaves a serving primary's namespace in its group,
// on the converge and on the read: the pass cannot tell where the path is,
// and a read that did not answer must not cut a serving primary from its
// leg (DN10). Each such read of the pass is killed in turn.
func TestAServingPrimaryKeepsItsGroupWhenItsOwnReadFails(t *testing.T) {
	for _, c := range []struct {
		name string
		key  func(srv *DnAgentServer, n fenceNames) string
	}{
		{"its dm-linear's info", func(_ *DnAgentServer, n fenceNames) string {
			return "-o attr " + n.oldLin
		}},
		{"its dm-linear's table", func(_ *DnAgentServer, n fenceNames) string {
			return "dmsetup table " + n.oldLin
		}},
		{"the side device's number", func(srv *DnAgentServer,
			n fenceNames) string {
			return "MAJ:MIN " + srv.nf.DmPath(n.side)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for k := 1; ; k++ {
				srv, node := newTestServer(t)
				syncupBoth(t, srv, 1, testSide)
				n := newFenceNames(srv)
				key := c.key(srv, n)
				node.mu.Lock()
				node.killCmdNoEffectNth[key] = k
				node.mu.Unlock()
				if _, err := srv.SyncupSide(context.Background(),
					sideReq(1, testSide, testCn0, []uint64{testCn1},
						pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
					t.Fatalf("SyncupSide: %v", err)
				}
				node.mu.Lock()
				_, unspent := node.killCmdNoEffectNth[key]
				delete(node.killCmdNoEffectNth, key)
				node.mu.Unlock()
				if unspent {
					if k == 1 {
						t.Fatalf("fixture is wrong: no %q in the pass", key)
					}
					break
				}
				if grp := anaGrpOf(t, srv, node, n.oldNqn); grp !=
					common.AnaGrpIdOptimized {
					t.Errorf("killing match %d of %q moved the serving "+
						"primary's namespace to group %d", k, key, grp)
				}
			}
			// A read in which every such read is killed cannot tell where
			// the path is either: it wants the group the namespace has.
			srv, node := newTestServer(t)
			syncupBoth(t, srv, 1, testSide)
			n := newFenceNames(srv)
			key := c.key(srv, n)
			setHook(node, node.killCmdNoEffectAlways, key)
			round := readFixtureSide(t, srv, 1)
			clearHook(node, node.killCmdNoEffectAlways, key)
			if exp := round.GetSideInfo().GetCnIdToNvmeof()[testCn0]; exp.
				GetStatus() != pb.ResStatus_RES_STATUS_OK ||
				exp.GetReason() != pb.ResReason_RES_REASON_NONE {
				t.Errorf("a read with %q killed: the export row is %v/%q/%v, "+
					"want OK", key, exp.GetStatus(), exp.GetDetails(),
					exp.GetReason())
			}
		})
	}
}

// A migration destination that serves through its dm-clone keeps doing so
// across a pass in which a read of the primary's own dm-linear does not
// answer: its namespace keeps the optimized group, which is the record a
// later stopped pass reads its last decision from (migrCloneMapped), so
// that stopped pass does not take the serving path off the dm-clone.
func TestADestinationKeepsItsRecordAcrossAnUnansweredRead(t *testing.T) {
	names := newMigrDstNames()
	for k := 1; ; k++ {
		srv, node := newTestServer(t)
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		if _, err := srv.SyncupSide(ctx,
			migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if !servesThroughClone(t, srv, node, names) {
			t.Fatal("fixture is wrong: not serving through the dm-clone")
		}
		key := "-o attr " + names.lin
		node.mu.Lock()
		node.killCmdNoEffectNth[key] = k
		node.mu.Unlock()
		if _, err := srv.SyncupSide(ctx,
			migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		node.mu.Lock()
		_, unspent := node.killCmdNoEffectNth[key]
		delete(node.killCmdNoEffectNth, key)
		node.mu.Unlock()
		if unspent {
			break
		}
		stop := "dmsetup status --noflush " + names.clone
		setHook(node, node.killCmdNoEffectAlways, stop)
		node.Reset()
		if _, err := srv.SyncupSide(ctx,
			migrDstReq(4, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		clearHook(node, node.killCmdNoEffectAlways, stop)
		if !servesThroughClone(t, srv, node, names) {
			t.Errorf("after killing match %d of %q a stopped pass took the "+
				"serving primary off the dm-clone", k, key)
		}
	}
}

// A primary flip in a destination pass that stops early, whose fence of
// the old primary fails: the old primary's dm-linear stays on the dm-clone
// in the non-optimized group, the new primary's waits on its dm-error. The
// converge and the read answer alike, and a later stopped pass, once the
// fence holds, still reads that the stacks serve through the dm-clone
// (migrCloneMapped), fences the old primary and opens the new one's path.
func TestAFlipInAStoppedDestinationPassOpensOnceTheFenceHolds(t *testing.T) {
	oldP, newP := migrDstNamesOf(testCn0), migrDstNamesOf(testCn1)
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !servesThroughClone(t, srv, node, oldP) {
		t.Fatal("fixture is wrong: not serving through the dm-clone")
	}
	stop := "-o attr " + oldP.meta
	setHook(node, node.killCmdNoEffectAlways, stop)
	setFailAlways(node, "dmsetup reload "+oldP.lin, "refused")
	node.Reset()
	reply, err := srv.SyncupSide(ctx, flippedMigrDstReq(3))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertStoppedAt(t, reply, stop)
	if linearMapsDm(node, newP.lin, newP.clone) {
		t.Fatal("the new primary's path opened beside the old primary's")
	}
	round := readFixtureSide(t, srv, 3)
	for _, cn := range []uint64{testCn0, testCn1} {
		for what, rows := range map[string][2]*pb.ResInfo{
			"dm-linear": {reply.GetSideInfo().GetCnIdToDmLinear()[cn],
				round.GetSideInfo().GetCnIdToDmLinear()[cn]},
			"export": {reply.GetSideInfo().GetCnIdToNvmeof()[cn],
				round.GetSideInfo().GetCnIdToNvmeof()[cn]},
		} {
			c, r := rows[0], rows[1]
			if c.GetStatus() != r.GetStatus() ||
				c.GetReason() != r.GetReason() {
				t.Errorf("cn %d's %s row: the converge's %v/%q/%v, the "+
					"read's %v/%q/%v", cn, what, c.GetStatus(),
					c.GetDetails(), c.GetReason(), r.GetStatus(),
					r.GetDetails(), r.GetReason())
			}
		}
	}

	node.mu.Lock()
	delete(node.failCmdAlways, "dmsetup reload "+oldP.lin)
	node.mu.Unlock()
	node.Reset()
	reply, err = srv.SyncupSide(ctx, flippedMigrDstReq(4))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertStoppedAt(t, reply, stop)
	if !linearMapsDm(node, oldP.lin, oldP.dmErr) {
		t.Error("the stopped pass did not fence the old primary")
	}
	if !servesThroughClone(t, srv, node, newP) {
		t.Error("the stopped pass did not open the new primary's path " +
			"through the dm-clone once the fence held")
	}
}

// A read-only dm-linear is not as wanted on either channel: nvmet refuses to
// enable a namespace over it, though the reads, writes and discards of one
// already enabled go on, and its converge reloads it and makes it writable.
// The primary's that is read-only on the side device, while another
// dm-linear's read does not answer, reads alike on the converge and on the
// read.
func TestAReadOnlyPrimaryReadsAlikeOnBothChannels(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	n := newFenceNames(srv)
	node.mu.Lock()
	node.dms[n.oldLin].readOnly = true
	node.failCmdAlways["dmsetup table "+n.newLin] = "refused"
	node.mu.Unlock()
	reply, err := srv.SyncupSide(context.Background(), sideReq(2, testSide,
		testCn0, []uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	round := readFixtureSide(t, srv, 2)
	for what, rows := range map[string][2]*pb.ResInfo{
		"dm-linear": {reply.GetSideInfo().GetCnIdToDmLinear()[testCn0],
			round.GetSideInfo().GetCnIdToDmLinear()[testCn0]},
		"export": {reply.GetSideInfo().GetCnIdToNvmeof()[testCn0],
			round.GetSideInfo().GetCnIdToNvmeof()[testCn0]},
	} {
		c, r := rows[0], rows[1]
		if c.GetStatus() != r.GetStatus() || c.GetReason() != r.GetReason() {
			t.Errorf("the primary's %s row: the converge's %v/%q/%v, the "+
				"read's %v/%q/%v", what, c.GetStatus(), c.GetDetails(),
				c.GetReason(), r.GetStatus(), r.GetDetails(), r.GetReason())
		}
	}
}

// The pass's pre-step leaves a promotion cut after its load alone when the
// side device's number does not answer: it cannot tell whether the waiting
// table maps it, and resuming the dm-linear on its dm-error instead would
// drop that table and fail the IO it holds. The build then finishes it.
func TestACutPromotionSurvivesAnUnreadSideDeviceNumber(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	n := newFenceNames(srv)
	newErrNo := node.devNo[srv.nf.DmPath(n.newErr)]

	node.failCmd["dmsetup resume "+n.newLin] = "refused once"
	if _, err := srv.SyncupSide(ctx, failoverReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	waiting := node.dms[n.newLin].waiting != nil
	node.mu.Unlock()
	if !waiting || !dmSuspended(node, n.newLin) {
		t.Fatal("fixture is wrong: the promotion's reload did not stop " +
			"after its load")
	}

	key := "MAJ:MIN " + srv.nf.DmPath(n.side)
	node.mu.Lock()
	node.killCmdNoEffectNth[key] = 1
	node.mu.Unlock()
	node.Reset()
	reply, err := srv.SyncupSide(ctx, failoverReq(2))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	_, unspent := node.killCmdNoEffectNth[key]
	delete(node.killCmdNoEffectNth, key)
	node.mu.Unlock()
	if unspent {
		t.Fatal("fixture is wrong: the pass read no side device number")
	}
	if releasedOntoDev(node, n.newLin, newErrNo) {
		t.Error("the cut promotion's held IO was failed on its dm-error")
	}
	assertPrimaryOpen(t, srv, node, n, reply, "the pass")
}

// setAnaGrp puts the namespace nqn exports in group grp on the fake node,
// as an earlier ANA move that failed or was cut leaves it.
func setAnaGrp(srv *DnAgentServer, node *fakeNode, nqn, grp string) {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.files[srv.nvmet.NsPath(nqn, sideNsid)+"/ana_grpid"] = grp
}

// A failback whose finish of the old fence is refused leaves the primary's
// dm-linear suspended with its own dm-error waiting: a refusal, not a read
// that did not answer, so the path reads closed and the namespace leaves
// the optimized group it was left in, on the converge and the read alike.
func TestARefusedFinishOfTheFenceClosesThePrimary(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	n := newFenceNames(srv)
	node.failCmd["dmsetup resume "+n.oldLin] = "refused once"
	if _, err := srv.SyncupSide(ctx, failoverReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	waiting := node.dms[n.oldLin].waiting != nil
	node.mu.Unlock()
	if !waiting {
		t.Fatal("fixture is wrong: the old primary's fence was not cut " +
			"after its load")
	}
	setAnaGrp(srv, node, n.oldNqn, "1")
	setFailAlways(node, "dmsetup resume "+n.oldLin, "refused")
	reply, err := srv.SyncupSide(ctx, sideReq(3, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if grp := anaGrpOf(t, srv, node, n.oldNqn); grp !=
		common.AnaGrpIdInaccessible {
		t.Errorf("the primary held on its fence is in group %d, want "+
			"inaccessible", grp)
	}
	round := readFixtureSide(t, srv, 3)
	clearFailAlways(node, "dmsetup resume "+n.oldLin)
	c := reply.GetSideInfo().GetCnIdToNvmeof()[testCn0]
	r := round.GetSideInfo().GetCnIdToNvmeof()[testCn0]
	if c.GetStatus() != r.GetStatus() || c.GetReason() != r.GetReason() {
		t.Errorf("the primary's export row: the converge's %v/%q/%v, the "+
			"read's %v/%q/%v", c.GetStatus(), c.GetDetails(), c.GetReason(),
			r.GetStatus(), r.GetDetails(), r.GetReason())
	}
}

// A failback onto a primary whose namespace was left optimized over its own
// dm-error: a read of the side device's number that does not answer cannot
// keep it optimized, since a dm-linear live on its own dm-error is closed
// whatever that number says. Each such read of the pass is killed in turn.
func TestAnUnreadSideNumberDoesNotKeepADmErrorOptimized(t *testing.T) {
	for k := 1; ; k++ {
		srv, node := newTestServer(t)
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		n := newFenceNames(srv)
		if _, err := srv.SyncupSide(ctx, failoverReq(2)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		setAnaGrp(srv, node, n.oldNqn, "1")
		key := "MAJ:MIN " + srv.nf.DmPath(n.side)
		node.mu.Lock()
		node.killCmdNoEffectNth[key] = k
		node.mu.Unlock()
		if _, err := srv.SyncupSide(ctx, sideReq(3, testSide, testCn0,
			[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		node.mu.Lock()
		_, unspent := node.killCmdNoEffectNth[key]
		delete(node.killCmdNoEffectNth, key)
		node.mu.Unlock()
		if unspent {
			if k == 1 {
				t.Fatalf("fixture is wrong: no %q in the pass", key)
			}
			break
		}
		if linearMapsDm(node, n.oldLin, n.oldErr) &&
			anaGrpOf(t, srv, node, n.oldNqn) == common.AnaGrpIdOptimized {
			t.Errorf("killing match %d of %q left the primary optimized "+
				"over its own dm-error", k, key)
		}
	}
}

// A primary whose path the fence keeps closed, in the inaccessible group,
// keeps that group when a read of its own dm-linear does not answer: the
// pass cannot tell where the path is, and must not point hosts at it.
func TestAnUnreadClosedPrimaryKeepsItsGroup(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	n := newFenceNames(srv)
	setFailAlways(node, "dmsetup suspend "+n.oldLin, "refused")
	if _, err := srv.SyncupSide(ctx, failoverReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if grp := anaGrpOf(t, srv, node, n.newNqn); grp !=
		common.AnaGrpIdInaccessible {
		t.Fatalf("fixture is wrong: the closed primary is in group %d", grp)
	}
	clearFailAlways(node, "dmsetup suspend "+n.oldLin)
	setHook(node, node.killCmdNoEffectAlways, "-o attr "+n.newLin)
	if _, err := srv.SyncupSide(ctx, failoverReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	round := readFixtureSide(t, srv, 2)
	clearHook(node, node.killCmdNoEffectAlways, "-o attr "+n.newLin)
	if grp := anaGrpOf(t, srv, node, n.newNqn); grp !=
		common.AnaGrpIdInaccessible {
		t.Errorf("an unread pass moved the closed primary to group %d", grp)
	}
	if exp := round.GetSideInfo().GetCnIdToNvmeof()[testCn1]; exp.
		GetStatus() != pb.ResStatus_RES_STATUS_OK || exp.GetReason() !=
		pb.ResReason_RES_REASON_NONE {
		t.Errorf("the unread read's export row is %v/%q/%v, want OK",
			exp.GetStatus(), exp.GetDetails(), exp.GetReason())
	}
}

// A migration destination before its dm-clone exists: the pre-step resumes
// a primary's dm-linear suspended on its own dm-error, though the dm-clone's
// number does not answer, as no table waits on it (DN12 rule 2).
func TestADestinationPrimaryOnItsDmErrorIsResumedBeforeItsClone(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	names := newMigrDstNames()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	provisionSide(t, srv, 1, testSide)
	waits := migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)
	waits.MigrDstConf.CutoverPending = true
	if _, err := srv.SyncupSide(ctx, waits); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	node.dms[names.lin].suspended = true
	_, clone := node.dms[names.clone]
	node.mu.Unlock()
	if clone || !linearMapsDm(node, names.lin, names.dmErr) {
		t.Fatal("fixture is wrong: want no dm-clone and the primary on its " +
			"dm-error")
	}
	side := srv.nf.DnSideName(testCluster, testDn, testSp, testSide)
	setFailAlways(node, "dmsetup table "+side, "refused")
	waits.Revision = 3
	if _, err := srv.SyncupSide(ctx, waits); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	clearFailAlways(node, "dmsetup table "+side)
	if dmSuspended(node, names.lin) {
		t.Error("the pass left the primary's dm-linear suspended on its " +
			"dm-error")
	}
}

// The pass's pre-step takes the primary's dm-linear last, so a stopped pass
// that finds both a fence and a promotion cut after their loads finishes
// the fence first and then the promotion: taken first, the promotion would
// find the fence unproven and wait for a build the pass never reaches.
func TestAStoppedPassFinishesTheFenceBeforeThePromotion(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	// cn1 is the old primary, cn0 the new one: cn0's dm-linear sorts first
	// by name, so only the pre-step's own order puts it last.
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	syncupSideTwoPhase(t, srv, sideReq(1, testSide, testCn1,
		[]uint64{testCn0}, pb.SpLevel_SP_LEVEL_READWRITE))
	n := newFenceNames(srv)
	sideNo := node.devNo[srv.nf.DmPath(n.side)]
	oldErrNo := node.devNo[srv.nf.DmPath(n.oldErr)]
	newErrNo := node.devNo[srv.nf.DmPath(n.newErr)]
	// cn1's fence and cn0's promotion, each cut after its load.
	node.mu.Lock()
	for name, w := range map[string][2]string{
		n.newLin: {sideNo, newErrNo},
		n.oldLin: {oldErrNo, sideNo},
	} {
		dm := node.dms[name]
		live := dm.table
		dm.waiting = &fakeTable{table: strings.Replace(live,
			" linear "+w[0]+" ", " linear "+w[1]+" ", 1)}
		dm.suspended = true
	}
	node.mu.Unlock()
	if !linearMapsDm(node, n.newLin, n.side) ||
		!linearMapsDm(node, n.oldLin, n.oldErr) {
		t.Fatal("fixture is wrong: the live tables are not the old ones")
	}
	side := srv.nf.DnSideName(testCluster, testDn, testSp, testSide)
	setFailAlways(node, "dmsetup table "+side, "refused")
	if _, err := srv.SyncupSide(ctx, sideReq(2, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	clearFailAlways(node, "dmsetup table "+side)
	if !linearMapsDm(node, n.newLin, n.newErr) || dmSuspended(node, n.newLin) {
		t.Error("the stopped pass did not finish the old primary's fence")
	}
	if !linearMapsDm(node, n.oldLin, n.side) || dmSuspended(node, n.oldLin) {
		t.Error("the stopped pass did not finish the new primary's promotion")
	}
}

// While every read of the side device's number fails, the primary's path
// is still closed where the dm-linear itself says so — suspended with no
// table waiting, or live on its own dm-error — so its namespace leaves the
// optimized group it was in rather than keep pointing hosts there.
func TestAnUnreadSideNumberStillClosesAClosedPath(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, srv *DnAgentServer, node *fakeNode,
			n fenceNames) (string, *pb.SyncupSideRequest)
	}{
		{"suspended with nothing waiting", func(t *testing.T,
			srv *DnAgentServer, node *fakeNode,
			n fenceNames) (string, *pb.SyncupSideRequest) {
			node.mu.Lock()
			node.dms[n.oldLin].suspended = true
			node.mu.Unlock()
			return n.oldNqn, sideReq(1, testSide, testCn0,
				[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)
		}},
		{"live on its own dm-error", func(t *testing.T,
			srv *DnAgentServer, node *fakeNode,
			n fenceNames) (string, *pb.SyncupSideRequest) {
			if _, err := srv.SyncupSide(context.Background(),
				failoverReq(2)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			setAnaGrp(srv, node, n.oldNqn, "1")
			return n.oldNqn, sideReq(3, testSide, testCn0,
				[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, 1, testSide)
			n := newFenceNames(srv)
			nqn, req := c.setup(t, srv, node, n)
			key := "MAJ:MIN " + srv.nf.DmPath(n.side)
			setHook(node, node.killCmdNoEffectAlways, key)
			if _, err := srv.SyncupSide(context.Background(),
				req); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			clearHook(node, node.killCmdNoEffectAlways, key)
			if grp := anaGrpOf(t, srv, node, nqn); grp ==
				common.AnaGrpIdOptimized {
				t.Errorf("the closed path's namespace stayed optimized")
			}
		})
	}
}

// A primary's path the fence keeps closed takes its namespace out of the
// optimized group even when the export's step stops at a read of another of
// the namespace's attributes: the move to inaccessible is tried on its own,
// as no CN may be pointed at a path the fence holds closed (DN10).
func TestAClosedPathLeavesTheOptimizedGroupWhenANamespaceReadFails(
	t *testing.T,
) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	n := newFenceNames(srv)
	if grp := anaGrpOf(t, srv, node, n.oldNqn); grp !=
		common.AnaGrpIdOptimized {
		t.Fatalf("fixture is wrong: the serving primary is in group %d", grp)
	}
	// The primary's dm-linear held suspended on the side device, and the
	// other's read not answering: the fence check fails and the path stays
	// closed.
	node.mu.Lock()
	node.dms[n.oldLin].suspended = true
	node.mu.Unlock()
	setFailAlways(node, "dmsetup table "+n.newLin, "refused")
	uuidRead := srv.nvmet.NsPath(n.oldNqn, sideNsid) + "/device_uuid"
	setHook(node, node.killReadAlways, uuidRead)
	reply, err := srv.SyncupSide(ctx, sideReq(1, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	clearHook(node, node.killReadAlways, uuidRead)
	clearFailAlways(node, "dmsetup table "+n.newLin)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if row := reply.GetSideInfo().GetCnIdToDmLinear()[testCn0]; row.
		GetStatus() != pb.ResStatus_RES_STATUS_PENDING {
		t.Fatalf("fixture is wrong: the primary's row is %v/%q",
			row.GetStatus(), row.GetDetails())
	}
	if grp := anaGrpOf(t, srv, node, n.oldNqn); grp !=
		common.AnaGrpIdInaccessible {
		t.Errorf("the closed path's namespace is in group %d, want "+
			"inaccessible", grp)
	}
}
