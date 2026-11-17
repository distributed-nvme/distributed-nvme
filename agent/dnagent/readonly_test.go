package dnagent

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A read-only device is made writable (DN10)
// ---------------------------------------------------------------------------

// roSetrwRefusal is what a failed `blockdev --setrw` prints.
const roSetrwRefusal = "blockdev: ioctl error on BLKROSET: Permission denied"

// roReloadWritable is the verbs of the step a converge takes on a device that
// reads read-only: Dm.Reload's three commands, then Dm.SetWritable.
var roReloadWritable = []string{"suspend", "reload", "resume", "setrw"}

// roCase is one device a side's read names while it reads read-only, with
// the side brought where the device stands and serves.
type roCase struct {
	name string
	// held is the request the worker holds for the side; setup brings the
	// side to it, converged.
	held  func() *pb.SyncupSideRequest
	setup func(t *testing.T, srv *DnAgentServer)
	// device is the dm device; named is the name the side's verdict names
	// it by.
	device string
	named  string
}

// roCases are the devices a side's read names while they read read-only: a
// per-CN dm-linear, a standby's and the primary's (DN18), a migration
// source's dm-linear (DN18) and a migration destination's metadata wrapper,
// whose step (2)'s read folds into the destination's rows, named by the
// dm-clone (DN13).
func roCases() []roCase {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	plain := func() *pb.SyncupSideRequest {
		return sideReq(1, testSide, testCn0, []uint64{testCn1},
			pb.SpLevel_SP_LEVEL_READWRITE)
	}
	plainSetup := func(t *testing.T, srv *DnAgentServer) {
		t.Helper()
		syncupBoth(t, srv, 1, testSide)
	}
	src := func() *pb.SyncupSideRequest { return migrSrcReq(2) }
	dst := func() *pb.SyncupSideRequest {
		return migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)
	}
	upTo := func(req func() *pb.SyncupSideRequest) func(*testing.T,
		*DnAgentServer) {
		return func(t *testing.T, srv *DnAgentServer) {
			t.Helper()
			syncupBoth(t, srv, 1, testSide)
			reply, err := srv.SyncupSide(context.Background(), req())
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			assertNotNamed(t, "fixture: the converge", reply)
		}
	}
	dstNames := newMigrDstNames()
	standby := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1)
	primary := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	srcLin := nf.DnMigrSrcName(testCluster, testDn, testSp, testMigrId)
	return []roCase{
		{"a standby's dm-linear", plain, plainSetup, standby, standby},
		{"the primary's dm-linear", plain, plainSetup, primary, primary},
		{"a migration source's dm-linear", src, upTo(src), srcLin, srcLin},
		{"a destination's metadata wrapper", dst, upTo(dst), dstNames.meta,
			dstNames.clone},
	}
}

// roSetro sets the block layer's own read-only flag of a dm device, as an
// operator's `blockdev --setro` does: a reload and a resume leave it set,
// and only `blockdev --setrw` clears it (fakeDm.bdevReadOnly).
func roSetro(t *testing.T, node *fakeNode, name string) {
	t.Helper()
	node.mu.Lock()
	defer node.mu.Unlock()
	dm := node.dms[name]
	if dm == nil {
		t.Fatalf("fixture is wrong: no dm device %s", name)
	}
	dm.bdevReadOnly = true
}

// roIsSet says the block layer's read-only flag of the dm device is set.
func roIsSet(node *fakeNode, name string) bool {
	node.mu.Lock()
	defer node.mu.Unlock()
	dm := node.dms[name]
	return dm != nil && dm.bdevReadOnly
}

// roChanges are the verbs of the commands that changed the dm device name
// among calls, in order: dmsetup's, and "setrw" or "setro" for a blockdev of
// the device's path.
func roChanges(calls []string, name string) []string {
	var out []string
	for _, call := range calls {
		fields := strings.Fields(call)
		switch {
		case len(fields) >= 4 && fields[0] == "cmd" &&
			fields[1] == "dmsetup" && fields[3] == name &&
			fields[2] != "info" && fields[2] != "table" &&
			fields[2] != "status":
			out = append(out, fields[2])
		case len(fields) == 4 && fields[0] == "cmd" &&
			fields[1] == "blockdev" && fields[3] == "/dev/mapper/"+name:
			out = append(out, strings.TrimPrefix(fields[2], "--"))
		}
	}
	return out
}

// assertNamedReadOnly fails unless reply names name, and its details say
// read-only, the read's words for a device that reads so (DN18, DN13).
func assertNamedReadOnly(
	t *testing.T,
	what string,
	reply interface{ GetAgentReply() *pb.AgentReply },
	name string,
) {
	t.Helper()
	assertNamed(t, what, reply, name)
	if details := reply.GetAgentReply().GetDetails(); !strings.Contains(
		details, "read-only") {
		t.Errorf("%s: details %q do not say read-only", what, details)
	}
}

// TestASetReadOnlyDeviceIsNamedAndMadeWritable: a device the read names
// while it reads read-only, set so by the block layer's own flag that an
// operator's `blockdev --setro` sets, is named on the worker's round; the
// one SyncupSide the worker re-sends reloads it and clears the flag with
// `blockdev --setrw`, and the read after it names nothing (DN10, A
// read-only device is made writable). A reload alone leaves the flag set,
// and the read would name the device again on every second round.
func TestASetReadOnlyDeviceIsNamedAndMadeWritable(t *testing.T) {
	for _, tc := range roCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			tc.setup(t, srv)
			held := tc.held()
			round, resent := workerRound(t, srv, held, "round-0")
			assertNotNamed(t, "fixture: round-0", round)
			if resent != nil {
				t.Fatalf("fixture: round-0 re-sent: %v",
					resent.GetAgentReply())
			}

			roSetro(t, node, tc.device)
			round, resent = workerRound(t, srv, held, "round-1")
			assertNamedReadOnly(t, "round-1", round, tc.named)
			if resent == nil {
				t.Fatal("round-1 re-sent nothing")
			}
			// The read after the re-send comes first: a step that does not
			// clear the flag shows as the same name again.
			round2, resent2 := workerRound(t, srv, held, "round-2")
			assertNotNamed(t, "round-1's re-send", resent)
			assertNotNamed(t, "round-2", round2)
			if resent2 != nil {
				t.Errorf("round-2 re-sent: %v", resent2.GetAgentReply())
			}
			if got := roChanges(node.Traced("round-1"),
				tc.device); !slices.Equal(got, roReloadWritable) {
				t.Errorf("round-1's re-send ran %q on %s, want %q", got,
					tc.device, roReloadWritable)
			}
			if roIsSet(node, tc.device) {
				t.Errorf("%s is still set read-only", tc.device)
			}
		})
	}
}

// TestAFailedReadOnlyClearFailsTheDevicesStep: a clear of a device's
// read-only flag that fails after its reload went through fails the
// device's step (DN10, A read-only device is made writable): the converge's
// reply names the device with blockdev's error, the read after it names it
// read-only again, and the re-send that read brings clears the flag, after
// which nothing is named.
func TestAFailedReadOnlyClearFailsTheDevicesStep(t *testing.T) {
	for _, tc := range roCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			tc.setup(t, srv)
			held := tc.held()
			roSetro(t, node, tc.device)
			node.mu.Lock()
			node.failCmd["blockdev --setrw /dev/mapper/"+tc.device] =
				roSetrwRefusal
			node.mu.Unlock()

			reply, err := srv.SyncupSide(context.Background(), held)
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			assertNamed(t, "the converge", reply, tc.named)
			if details := reply.GetAgentReply().GetDetails(); !strings.
				Contains(details, roSetrwRefusal) {
				t.Errorf("the converge's details %q do not carry "+
					"blockdev's error", details)
			}
			if !roIsSet(node, tc.device) {
				t.Fatalf("fixture is wrong: the failed clear cleared %s",
					tc.device)
			}

			round, resent := workerRound(t, srv, held, "round-1")
			assertNamedReadOnly(t, "round-1", round, tc.named)
			if resent == nil {
				t.Fatal("round-1 re-sent nothing")
			}
			round2, resent2 := workerRound(t, srv, held, "round-2")
			assertNotNamed(t, "round-1's re-send", resent)
			assertNotNamed(t, "round-2", round2)
			if resent2 != nil {
				t.Errorf("round-2 re-sent: %v", resent2.GetAgentReply())
			}
			if got := roChanges(node.Traced("round-1"),
				tc.device); !slices.Equal(got, roReloadWritable) {
				t.Errorf("round-1's re-send ran %q on %s, want %q", got,
					tc.device, roReloadWritable)
			}
			if roIsSet(node, tc.device) {
				t.Errorf("%s is still set read-only", tc.device)
			}
		})
	}
}

// TestASetReadOnlySideDeviceIsMadeWritableByAConverge: the side device's row
// does not read the read-only flag (DN18), so the read names nothing for a
// side device set read-only; a converge that reaches it reloads it and
// clears the flag all the same. A clear that fails is logged and fails
// nothing: the reply names nothing, the flag stays, and the next converge,
// which finds the device read-only still, clears it (DN10, A read-only
// device is made writable).
func TestASetReadOnlySideDeviceIsMadeWritableByAConverge(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	held := sideReq(1, testSide, testCn0, []uint64{testCn1},
		pb.SpLevel_SP_LEVEL_READWRITE)
	side := newFenceNames(srv).side

	roSetro(t, node, side)
	round, resent := workerRound(t, srv, held, "round-1")
	assertNotNamed(t, "round-1", round)
	if resent != nil {
		t.Fatalf("round-1 re-sent: %v", resent.GetAgentReply())
	}

	node.Reset()
	reply, err := srv.SyncupSide(ctx, held)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertNotNamed(t, "the converge", reply)
	if got := roChanges(node.Calls(), side); !slices.Equal(got,
		roReloadWritable) {
		t.Errorf("the converge ran %q on %s, want %q", got, side,
			roReloadWritable)
	}
	if roIsSet(node, side) {
		t.Fatalf("%s is still set read-only after the converge", side)
	}

	roSetro(t, node, side)
	node.mu.Lock()
	node.failCmd["blockdev --setrw /dev/mapper/"+side] = roSetrwRefusal
	node.mu.Unlock()
	node.Reset()
	reply, err = srv.SyncupSide(ctx, held)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertNotNamed(t, "the converge whose clear failed", reply)
	if got := roChanges(node.Calls(), side); !slices.Equal(got,
		roReloadWritable) {
		t.Fatalf("fixture is wrong: the converge ran %q on %s, want %q",
			got, side, roReloadWritable)
	}
	if !roIsSet(node, side) {
		t.Fatalf("fixture is wrong: the failed clear cleared %s", side)
	}
	node.Reset()
	reply, err = srv.SyncupSide(ctx, held)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertNotNamed(t, "the next converge", reply)
	if got := roChanges(node.Calls(), side); !slices.Equal(got,
		roReloadWritable) {
		t.Errorf("the next converge ran %q on %s, want %q", got, side,
			roReloadWritable)
	}
	if roIsSet(node, side) {
		t.Errorf("%s is still set read-only after the next converge", side)
	}
}

// TestAPrimaryFlipLandsWhileTheSideDevicesClearFails: a side device whose
// clear keeps failing does not hold the per-CN stacks back (DN10, A
// read-only device is made writable): the flag stops nothing above the side
// device, so a primary flip lands in one converge — the old primary's
// dm-linear on its dm-error and its namespace non-optimized, the new
// primary's on the side device and optimized — and the reply names nothing.
func TestAPrimaryFlipLandsWhileTheSideDevicesClearFails(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	n := newFenceNames(srv)
	roSetro(t, node, n.side)
	setFailAlways(node, "blockdev --setrw /dev/mapper/"+n.side,
		roSetrwRefusal)

	node.Reset()
	reply, err := srv.SyncupSide(context.Background(),
		sideReq(2, testSide, testCn1, []uint64{testCn0},
			pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertNotNamed(t, "the flip", reply)
	if !node.hasCall("blockdev --setrw /dev/mapper/" + n.side) {
		t.Fatal("fixture is wrong: the flip never tried the clear")
	}
	if !roIsSet(node, n.side) {
		t.Fatal("fixture is wrong: the failing clear cleared the side device")
	}
	for _, tc := range []struct {
		lin, nqn, onto string
		grp            int
	}{
		{n.oldLin, n.oldNqn, n.oldErr, common.AnaGrpIdNonOptimized},
		{n.newLin, n.newNqn, n.side, common.AnaGrpIdOptimized},
	} {
		if got := anaGrpOf(t, srv, node, tc.nqn); got != tc.grp {
			t.Errorf("%s is in group %d, want %d", tc.nqn, got, tc.grp)
		}
		node.mu.Lock()
		table := node.dms[tc.lin].table
		ontoNo := node.devNo[srv.nf.DmPath(tc.onto)]
		node.mu.Unlock()
		if want := " linear " + ontoNo + " 0"; !strings.Contains(table,
			want) {
			t.Errorf("%s maps %q, want it on %s", tc.lin, table, tc.onto)
		}
	}
}

// TestAWritableDeviceGetsNoClear: only a device that reads read-only gets
// the clear (makeWritable). A primary flip reloads both per-CN dm-linears
// and a side device off its runs is reloaded, each with Dm.Reload's three
// commands and no blockdev: a clear on every reload would let a blockdev
// failure fail every flip and every fence.
func TestAWritableDeviceGetsNoClear(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	n := newFenceNames(srv)
	node.mu.Lock()
	fields := strings.Fields(node.dms[n.side].table)
	offset, err := strconv.ParseUint(fields[len(fields)-1], 10, 64)
	if err != nil {
		node.mu.Unlock()
		t.Fatalf("the side device's table is %q", node.dms[n.side].table)
	}
	fields[len(fields)-1] = strconv.FormatUint(offset+8, 10)
	node.dms[n.side].table = strings.Join(fields, " ")
	node.mu.Unlock()

	node.Reset()
	reply, err := srv.SyncupSide(context.Background(),
		sideReq(2, testSide, testCn1, []uint64{testCn0},
			pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertNotNamed(t, "the flip", reply)
	reload := []string{"suspend", "reload", "resume"}
	for _, name := range []string{n.side, n.oldLin, n.newLin} {
		if got := roChanges(node.Calls(), name); !slices.Equal(got,
			reload) {
			t.Errorf("the flip ran %q on %s, want %q", got, name, reload)
		}
	}
	if got := node.callsMatching("cmd blockdev"); len(got) != 0 {
		t.Errorf("a converge over writable devices ran %q", got)
	}
}

// TestAFailedReloadOfAReadOnlyDeviceRunsNoClear: the clear follows only a
// reload that went through (makeWritable). A read-only dm-linear whose load
// fails stays suspended on its old table (a reload fails closed), no
// blockdev runs on it, and its row carries the load's error; the next
// converge reloads it and clears it.
func TestAFailedReloadOfAReadOnlyDeviceRunsNoClear(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	held := sideReq(1, testSide, testCn0, []uint64{testCn1},
		pb.SpLevel_SP_LEVEL_READWRITE)
	lin := newFenceNames(srv).newLin
	roSetro(t, node, lin)
	const refusal = "device-mapper: reload ioctl failed"
	node.mu.Lock()
	node.failCmd["dmsetup reload "+lin] = refusal
	node.mu.Unlock()

	node.Reset()
	reply, err := srv.SyncupSide(ctx, held)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertNamed(t, "the converge", reply, lin)
	if details := reply.GetAgentReply().GetDetails(); !strings.Contains(
		details, refusal) {
		t.Errorf("the converge's details %q do not carry the load's error",
			details)
	}
	if got := node.callsMatching("cmd blockdev"); len(got) != 0 {
		t.Errorf("a converge whose reload failed ran %q", got)
	}

	node.Reset()
	reply, err = srv.SyncupSide(ctx, held)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertNotNamed(t, "the next converge", reply)
	if roIsSet(node, lin) {
		t.Errorf("%s is still set read-only after the next converge", lin)
	}
}

// TestARetireWhoseClearFailsLetsTheTeardownGoOn: the sweep's retire of a
// suspended per-CN dm-linear (DN6) counts a dm-linear whose reload onto its
// dm-error went through as retired even when the clear of its read-only flag
// fails, which it logs: the flag stops nothing of the teardown, so the same
// pass removes the export, the dm-linear and its dm-error.
func TestARetireWhoseClearFailsLetsTheTeardownGoOn(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	stbLin := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1)
	stbErr := nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn1)
	stbNqn := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn1)

	// The cutover window holds the migration source's per-CN dm-linears
	// suspended.
	srv.fenceWait = time.Hour
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	suspended := node.dms[stbLin].suspended
	node.mu.Unlock()
	if !suspended {
		t.Fatal("fixture is wrong: the standby's dm-linear is not suspended")
	}
	roSetro(t, node, stbLin)
	node.mu.Lock()
	node.failCmd["blockdev --setrw /dev/mapper/"+stbLin] = roSetrwRefusal
	node.mu.Unlock()

	node.Reset()
	dropped := migrSrcReq(3)
	dropped.SideConf.StandbyIdList = nil
	if _, err := srv.SyncupSide(ctx, dropped); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := roChanges(node.Calls(), stbLin); len(got) < 4 ||
		!slices.Equal(got[:4], roReloadWritable) {
		t.Fatalf("fixture is wrong: the retire ran %q on %s, want %q "+
			"first", got, stbLin, roReloadWritable)
	}
	if subsysPresent(node, stbNqn) || dmPresent(node, stbLin) ||
		dmPresent(node, stbErr) {
		t.Errorf("the pass whose retire's clear failed left: export %v, "+
			"dm-linear %v, dm-error %v", subsysPresent(node, stbNqn),
			dmPresent(node, stbLin), dmPresent(node, stbErr))
	}
}

// TestAnExportIsBuiltOverASetReadOnlyLinear: nvmet refuses to enable a
// namespace over a read-only device (EACCES), which the fake models
// (enableRefusal). A level that brings the exports back over a per-CN
// dm-linear set read-only builds them in one converge, because the dm-linear
// step clears the flag before the export step enables the namespace; while
// the clear fails, the enable is refused and the export's row carries it.
func TestAnExportIsBuiltOverASetReadOnlyLinear(t *testing.T) {
	for _, clearFails := range []bool{false, true} {
		name := "the clear works"
		if clearFails {
			name = "the clear fails"
		}
		t.Run(name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			n := newFenceNames(srv)
			reply, err := srv.SyncupSide(ctx, sideReq(2, testSide, testCn0,
				[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_NO_SIDE))
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			assertNotNamed(t, "fixture: the converge at NO_SIDE", reply)
			if subsysPresent(node, n.newNqn) {
				t.Fatal("fixture is wrong: the export stands at NO_SIDE")
			}
			roSetro(t, node, n.newLin)
			if clearFails {
				setFailAlways(node, "blockdev --setrw /dev/mapper/"+n.newLin,
					roSetrwRefusal)
			}

			reply, err = srv.SyncupSide(ctx, sideReq(3, testSide, testCn0,
				[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			row := reply.GetSideInfo().GetCnIdToNvmeof()[testCn1]
			if !clearFails {
				assertNotNamed(t, "the converge", reply)
				if row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
					t.Errorf("the export's row is %v/%q, want OK",
						row.GetStatus(), row.GetDetails())
				}
				return
			}
			assertNamed(t, "the converge", reply, n.newLin, n.newNqn)
			if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
				!strings.Contains(row.GetDetails(), "permission denied") {
				t.Errorf("the export's row is %v/%q, want ERROR with "+
					"the refused enable", row.GetStatus(), row.GetDetails())
			}
		})
	}
}
