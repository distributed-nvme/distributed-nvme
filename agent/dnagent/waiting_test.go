package dnagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// A resume keeps the live table the agent compared (dnagent.md, OS wrappers
// — `dm.go`, `nvmet.go`, `nvmehost.go`). A failover whose reload of the new
// primary's dm-linear stopped after its load leaves that dm-linear
// suspended on its dm-error with the side device's table waiting. The
// revision after it makes that CN a standby again, which wants the
// dm-error: the converge finds the live table as it wants it and resumes
// the device, and the resume must not make the waiting table live, which
// would put a standby's dm-linear on the side's data. Both resumes are
// covered: the sweep's pre-step (unfenceLinears) and, when the sweep's
// listing does not answer and no pre-step runs, the build's own
// (ensureDmLinear).
func TestAResumeKeepsTheLiveTableAfterAStoppedReload(t *testing.T) {
	for _, tc := range []struct {
		name string
		// noListing fails the converge's `dmsetup ls`, so the sweep runs
		// no pre-step.
		noListing bool
	}{
		{name: "the sweep's pre-step"},
		{name: "the build's own resume", noListing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			nf := srv.nf
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			sideDev := nf.DnSideName(testCluster, testDn, testSp, testSide)
			sideNo := node.devNo[nf.DmPath(sideDev)]
			lin := nf.DnLinearName(
				testCluster, testDn, testSp, testSide, testCn1)
			errName := nf.DnErrorName(
				testCluster, testDn, testSp, testSide, testCn1)

			node.failCmd["dmsetup resume "+lin] = "refused once"
			if _, err := srv.SyncupSide(ctx, failoverReq(2)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			node.mu.Lock()
			dm := node.dms[lin]
			waits := dm.waiting != nil
			node.mu.Unlock()
			if !waits || !dmSuspended(node, lin) ||
				!linearMapsDm(node, lin, errName) {
				t.Fatal("fixture is wrong: the failover's reload did not " +
					"stop after its load")
			}

			if tc.noListing {
				node.failCmd["dmsetup ls"] = "refused once"
			}
			node.Reset()
			if _, err := srv.SyncupSide(ctx, sideReq(3, testSide, testCn0,
				[]uint64{testCn1},
				pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			node.mu.Lock()
			waits = node.dms[lin].waiting != nil
			node.mu.Unlock()
			if !linearMapsDm(node, lin, errName) {
				t.Error("the standby's dm-linear left its dm-error")
			}
			if releasedOntoDev(node, lin, sideNo) {
				t.Error("a resume made the waiting table live and " +
					"released the standby's IO onto the side device")
			}
			if waits || dmSuspended(node, lin) {
				t.Errorf("the dm-linear is suspended %v, a table waits %v",
					dmSuspended(node, lin), waits)
			}
		})
	}
}

// A primary's dm-linear that a promotion's reload left suspended on its
// dm-error, the serving device's table waiting, is not resumed by the
// sweep's pre-step (unfenceLinears): the resume would drop the waiting
// table and release the IO the linear holds onto the dm-error, failing it.
// The build phase's reload puts it on the serving device and releases the
// IO there. That serving device is the side device after a failover, and a
// migration destination's dm-clone after its cutover. The same request is
// sent again, as the worker re-sends it.
func TestAPrimaryHeldOffItsServingDeviceIsNotResumedOntoItsDmError(
	t *testing.T,
) {
	t.Run("a failover", func(t *testing.T) {
		srv, node := newTestServer(t)
		nf := srv.nf
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		sideNo := node.devNo[nf.DmPath(
			nf.DnSideName(testCluster, testDn, testSp, testSide))]
		lin := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1)
		errNo := node.devNo[nf.DmPath(
			nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn1))]

		node.failCmd["dmsetup resume "+lin] = "refused once"
		for range 2 {
			if _, err := srv.SyncupSide(ctx, failoverReq(2)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
		}
		if releasedOntoDev(node, lin, errNo) {
			t.Error("the IO the new primary's dm-linear held was " +
				"released onto its dm-error")
		}
		if !releasedOntoDev(node, lin, sideNo) {
			t.Error("the IO the new primary's dm-linear held was not " +
				"released onto the side device")
		}
	})
	t.Run("a migration destination's cutover", func(t *testing.T) {
		srv, node := newTestServer(t)
		nf := srv.nf
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
		if !linearMapsDm(node, names.lin, names.dmErr) {
			t.Fatal("fixture is wrong: the primary's dm-linear is not on " +
				"its dm-error before the cutover")
		}
		node.failCmd["dmsetup resume "+names.lin] = "refused once"
		for range 2 {
			if _, err := srv.SyncupSide(ctx, migrDstReq(3,
				pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
		}
		node.mu.Lock()
		cloneNo := node.devNo[nf.DmPath(names.clone)]
		errNo := node.devNo[nf.DmPath(names.dmErr)]
		node.mu.Unlock()
		if releasedOntoDev(node, names.lin, errNo) {
			t.Error("the IO the primary's dm-linear held was released " +
				"onto its dm-error")
		}
		if !releasedOntoDev(node, names.lin, cloneNo) {
			t.Error("the IO the primary's dm-linear held was not " +
				"released onto the dm-clone")
		}
	})
}

// The sweep's step before its first layer retires a suspended per-CN
// dm-linear onto its dm-error, building that dm-error first when it is gone
// (retireIfSuspended): without it the retire would fail on every pass and
// the teardown would never get past that step. A cutover suspends the
// primary's dm-linear on the side device; its dm-error is then taken away by
// hand, and the side is torn down.
func TestARetireBuildsAMissingDmError(t *testing.T) {
	srv, node := newTestServer(t)
	nf := srv.nf
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	linName := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	errName := nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn0)
	sideNo := node.devNo[nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide))]

	srv.fenceWait = time.Hour
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !dmSuspended(node, linName) {
		t.Fatal("fixture is wrong: the cutover did not suspend the " +
			"primary's dm-linear")
	}
	node.mu.Lock()
	delete(node.dms, errName)
	delete(node.devNo, nf.DmPath(errName))
	delete(node.devSize, nf.DmPath(errName))
	node.mu.Unlock()

	node.Reset()
	if _, err := srv.SyncupDn(ctx, dnReq(3)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if dmPresent(node, linName) {
		t.Errorf("%s survived the teardown", linName)
	}
	if releasedOntoDev(node, linName, sideNo) {
		t.Errorf("%s released its deferred IO onto the side device", linName)
	}
	assertOrder(t, node,
		"cmd dmsetup create "+errName,
		"cmd dmsetup reload "+linName,
		"cmd dmsetup resume "+linName,
		"cmd dmsetup remove "+linName,
	)
}

// A migration destination's cutover whose reload of the primary's dm-linear
// onto the dm-clone stopped after its load, followed by a pass whose
// dm-clone status read does not answer: where the stacks serve from is read
// from where they are (migrDstStopped), and a dm-linear with a table
// waiting does not say it, so that pass leaves the stacks as they are. Read
// as down, the build would resume the dm-linear on its dm-error, fail the
// IO it holds and move the namespace to inaccessible. The next pass that
// reads the dm-clone serves through it.
func TestADestinationCutAfterItsLoadThenStoppedKeepsItsHeldIO(t *testing.T) {
	srv, node := newTestServer(t)
	nf := srv.nf
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
	node.failCmd["dmsetup resume "+names.lin] = "refused once"
	if _, err := srv.SyncupSide(ctx, migrDstReq(3,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	node.mu.Lock()
	waiting := node.dms[names.lin].waiting != nil
	node.mu.Unlock()
	if !waiting || !dmSuspended(node, names.lin) {
		t.Fatal("fixture is wrong: the cutover's reload did not stop " +
			"after its load")
	}

	statusRead := "dmsetup status --noflush " + names.clone
	before := len(node.callsMatching(statusRead))
	node.failCmdAlways[statusRead] = "refused"
	if _, err := srv.SyncupSide(ctx, migrDstReq(3,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	delete(node.failCmdAlways, statusRead)
	if len(node.callsMatching(statusRead)) == before {
		t.Fatal("fixture is wrong: the pass read no dm-clone status, so " +
			"no refusal stopped it")
	}
	node.mu.Lock()
	cloneNo := node.devNo[nf.DmPath(names.clone)]
	errNo := node.devNo[nf.DmPath(names.dmErr)]
	node.mu.Unlock()
	if releasedOntoDev(node, names.lin, errNo) {
		t.Error("the stopped pass released the primary's held IO onto " +
			"its dm-error")
	}
	if grp := anaGrpOf(t, srv, node, names.nqn); grp ==
		common.AnaGrpIdInaccessible {
		t.Error("the stopped pass moved the primary's namespace to " +
			"inaccessible")
	}

	if _, err := srv.SyncupSide(ctx, migrDstReq(3,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !releasedOntoDev(node, names.lin, cloneNo) ||
		!servesThroughClone(t, srv, node, names) {
		t.Error("the next pass does not serve the held IO through the " +
			"dm-clone")
	}
}

// A request that ends a migration source's role inside its cutover window
// and also makes the standby the primary: the pre-step resumes each
// dm-linear the window holds on its live table, so the new primary's
// dm-linear, held on its dm-error while it was a standby, fails what it
// holds there, and the old primary's, held on the side device, is left
// for the build's reload onto its dm-error, which fails what it holds.
// Neither CN's held IO reaches the side device; the new primary's path
// opens after.
func TestARoleEndThatChangesThePrimaryWritesNoHeldIOOnTheSide(t *testing.T) {
	srv, node := newTestServer(t)
	nf := srv.nf
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	sideNo := node.devNo[nf.DmPath(
		nf.DnSideName(testCluster, testDn, testSp, testSide))]
	lin0 := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn0)
	lin1 := nf.DnLinearName(testCluster, testDn, testSp, testSide, testCn1)
	srv.fenceWait = time.Hour
	if _, err := srv.SyncupSide(ctx, migrSrcReq(2)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !dmSuspended(node, lin0) || !dmSuspended(node, lin1) {
		t.Fatal("fixture is wrong: the cutover window does not hold " +
			"both dm-linears")
	}

	if _, err := srv.SyncupSide(ctx, failoverReq(3)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	// The first release of each is the one that ends the window's hold;
	// any later one ends a reload's own moment of suspension.
	for _, lin := range []string{lin0, lin1} {
		node.mu.Lock()
		released := append([]string(nil), node.releasedOnto[lin]...)
		node.mu.Unlock()
		if len(released) == 0 {
			t.Errorf("%s was never brought out of suspension", lin)
			continue
		}
		if strings.Contains(released[0], " linear "+sideNo+" ") {
			t.Errorf("%s released the IO the window held onto the side "+
				"device", lin)
		}
	}
	if !linearMapsDm(node, lin1, nf.DnSideName(
		testCluster, testDn, testSp, testSide)) {
		t.Error("the new primary's dm-linear is not on the side device")
	}
}

// A migration destination's primary dm-linear left suspended on the
// dm-clone with no table waiting — a demotion's reload whose load failed,
// say — is resumed there by the sweep's pre-step (unfenceLinears), since the
// dm-clone is the device the primary serves through. The pass's check of
// the side device does not answer, so no build follows to resume it.
func TestADestinationPrimarySuspendedOnItsDmCloneIsResumedThere(
	t *testing.T,
) {
	srv, node := newTestServer(t)
	nf := srv.nf
	ctx := context.Background()
	names := newMigrDstNames()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	provisionSide(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx, migrDstReq(2,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !servesThroughClone(t, srv, node, names) {
		t.Fatal("fixture is wrong: the destination does not serve " +
			"through its dm-clone")
	}
	node.mu.Lock()
	node.dms[names.lin].suspended = true
	node.mu.Unlock()

	sideDev := nf.DnSideName(testCluster, testDn, testSp, testSide)
	node.failCmdAlways["dmsetup table "+sideDev] = "refused"
	if _, err := srv.SyncupSide(ctx, migrDstReq(2,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	delete(node.failCmdAlways, "dmsetup table "+sideDev)
	node.mu.Lock()
	cloneNo := node.devNo[nf.DmPath(names.clone)]
	node.mu.Unlock()
	if dmSuspended(node, names.lin) ||
		!releasedOntoDev(node, names.lin, cloneNo) {
		t.Errorf("the primary's dm-linear was not resumed on its "+
			"dm-clone (suspended %v)", dmSuspended(node, names.lin))
	}
}

// A promotion whose reload stopped after its load leaves the new primary's
// dm-linear suspended on its dm-error with the serving device's table
// waiting, and its namespace already optimized. The next pass's pre-step
// finishes that reload (unfenceLinears, Dm.FinishReload), since the desired
// state still puts the linear there, so the IO it holds is served even when
// that pass stops before its build: at the side device's check after a
// failover, at the new primary's own dm-error step, or, on a migration
// destination after its cutover, at a dm-clone status read that does not
// answer.
func TestACutPromotionIsFinishedWhenTheNextPassStops(t *testing.T) {
	for _, tc := range []struct {
		name string
		// dst makes the case a migration destination's cutover.
		dst bool
		// stop is the command refused in every run of the next pass.
		stop func(nf *common.NameFmt, names migrDstNames) string
	}{
		{name: "at the side device's check",
			stop: func(nf *common.NameFmt, _ migrDstNames) string {
				return "dmsetup table " + nf.DnSideName(
					testCluster, testDn, testSp, testSide)
			}},
		{name: "at the new primary's dm-error step",
			stop: func(nf *common.NameFmt, _ migrDstNames) string {
				return "dmsetup info --columns --noheadings -o attr " +
					nf.DnErrorName(testCluster, testDn, testSp, testSide,
						testCn1)
			}},
		{name: "at a dm-clone status read", dst: true,
			stop: func(_ *common.NameFmt, names migrDstNames) string {
				return "dmsetup status --noflush " + names.clone
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			nf := srv.nf
			ctx := context.Background()
			names := newMigrDstNames()
			lin := nf.DnLinearName(testCluster, testDn, testSp, testSide,
				testCn1)
			nqn := nf.SideToCnNqn(testCluster, testSp, testLeg, testCn1)
			serving := nf.DnSideName(testCluster, testDn, testSp, testSide)
			req := failoverReq(2)
			if tc.dst {
				lin, nqn, serving = names.lin, names.nqn, names.clone
				if _, err := srv.SyncupDn(ctx,
					dnReq(1, testSide)); err != nil {
					t.Fatalf("SyncupDn: %v", err)
				}
				provisionSide(t, srv, 1, testSide)
				waits := migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)
				waits.MigrDstConf.CutoverPending = true
				if _, err := srv.SyncupSide(ctx, waits); err != nil {
					t.Fatalf("SyncupSide: %v", err)
				}
				req = migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)
			} else {
				syncupBoth(t, srv, 1, testSide)
			}

			node.failCmd["dmsetup resume "+lin] = "refused once"
			if _, err := srv.SyncupSide(ctx, req); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			node.mu.Lock()
			waits := node.dms[lin].waiting != nil
			node.mu.Unlock()
			if !waits || !dmSuspended(node, lin) {
				t.Fatal("fixture is wrong: the promotion's reload did " +
					"not stop after its load")
			}

			stop := tc.stop(nf, names)
			before := len(node.callsMatching(stop))
			node.failCmdAlways[stop] = "refused"
			if _, err := srv.SyncupSide(ctx, req); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			delete(node.failCmdAlways, stop)
			if len(node.callsMatching(stop)) == before {
				t.Fatalf("fixture is wrong: the pass never ran %q, so no "+
					"refusal stopped it", stop)
			}
			node.mu.Lock()
			servingNo := node.devNo[nf.DmPath(serving)]
			node.mu.Unlock()
			if dmSuspended(node, lin) ||
				!releasedOntoDev(node, lin, servingNo) {
				t.Errorf("the stopped pass did not serve the held IO "+
					"(suspended %v)", dmSuspended(node, lin))
			}
			if grp := anaGrpOf(t, srv, node, nqn); grp !=
				common.AnaGrpIdOptimized {
				t.Errorf("the namespace's ANA group is %d, want optimized",
					grp)
			}
		})
	}
}

// When a pass's listing of the node's dm devices does not answer, the sweep
// runs no pre-step, so a migration destination's primary dm-linear that a
// cutover's reload left suspended with the dm-clone's table waiting is still
// so when the pass, its dm-clone status read refused too, reads where the
// stacks serve from (migrDstStopped). That read judges the dm-linear by its
// waiting table (migrCloneMapped): the stack is on the dm-clone, so the
// build reloads it there and serves the IO it holds.
func TestAStoppedDestinationReadsAWaitingTable(t *testing.T) {
	srv, node := newTestServer(t)
	nf := srv.nf
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
	node.failCmd["dmsetup resume "+names.lin] = "refused once"
	if _, err := srv.SyncupSide(ctx, migrDstReq(3,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}

	node.failCmd["dmsetup ls"] = "refused once"
	statusRead := "dmsetup status --noflush " + names.clone
	before := len(node.callsMatching(statusRead))
	node.failCmdAlways[statusRead] = "refused"
	if _, err := srv.SyncupSide(ctx, migrDstReq(3,
		pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	delete(node.failCmdAlways, statusRead)
	if len(node.callsMatching(statusRead)) == before {
		t.Fatal("fixture is wrong: the pass read no dm-clone status, so " +
			"no refusal stopped it")
	}
	node.mu.Lock()
	_, listed := node.failCmd["dmsetup ls"]
	cloneNo := node.devNo[nf.DmPath(names.clone)]
	errNo := node.devNo[nf.DmPath(names.dmErr)]
	node.mu.Unlock()
	if listed {
		t.Fatal("fixture is wrong: the pass did not list the node")
	}
	if releasedOntoDev(node, names.lin, errNo) {
		t.Error("the stopped pass released the held IO onto the dm-error")
	}
	if !releasedOntoDev(node, names.lin, cloneNo) ||
		!servesThroughClone(t, srv, node, names) {
		t.Error("the stopped pass did not serve the held IO through the " +
			"dm-clone")
	}
}

// A demotion whose reload stopped after its load leaves the old primary's
// dm-linear suspended on the side device with its dm-error waiting: a fence
// in progress. A failback that makes that CN the primary again must not
// release the IO it held at the fence onto the side, since the new primary
// wrote there meanwhile and those older writes would land over it. The fence
// is finished first, on the dm-error, which fails that IO, and only then is
// the linear put back on the side device: by the sweep's pre-step
// (unfenceLinears) and, when the sweep's listing does not answer and no
// pre-step runs, by the build (finishFence).
func TestAFailbackAfterACutDemotionFailsTheIOHeldAtTheFence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		noListing bool
	}{
		{name: "the sweep's pre-step"},
		{name: "the build", noListing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			nf := srv.nf
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			sideDev := nf.DnSideName(testCluster, testDn, testSp, testSide)
			sideNo := node.devNo[nf.DmPath(sideDev)]
			lin0 := nf.DnLinearName(testCluster, testDn, testSp, testSide,
				testCn0)
			errNo := node.devNo[nf.DmPath(nf.DnErrorName(
				testCluster, testDn, testSp, testSide, testCn0))]

			node.failCmd["dmsetup resume "+lin0] = "refused once"
			if _, err := srv.SyncupSide(ctx, failoverReq(2)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			node.mu.Lock()
			waits := node.dms[lin0].waiting != nil
			node.mu.Unlock()
			if !waits || !dmSuspended(node, lin0) ||
				!linearMapsDm(node, lin0, sideDev) {
				t.Fatal("fixture is wrong: the demotion's reload did not " +
					"stop after its load")
			}

			if tc.noListing {
				node.failCmd["dmsetup ls"] = "refused once"
			}
			if _, err := srv.SyncupSide(ctx, sideReq(3, testSide, testCn0,
				[]uint64{testCn1},
				pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			node.mu.Lock()
			released := append([]string(nil), node.releasedOnto[lin0]...)
			node.mu.Unlock()
			if len(released) == 0 ||
				!strings.Contains(released[0], " linear "+errNo+" ") {
				t.Errorf("the IO held at the fence was released onto %q, "+
					"want the dm-error %s first", released, errNo)
			}
			for _, tb := range released[:min(1, len(released))] {
				if strings.Contains(tb, " linear "+sideNo+" ") {
					t.Error("the IO held at the fence reached the side device")
				}
			}
			if !linearMapsDm(node, lin0, sideDev) || dmSuspended(node, lin0) {
				t.Error("the failback did not put the primary back on the " +
					"side device")
			}
		})
	}
}
