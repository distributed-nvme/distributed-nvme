package dnagent

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A destination step that stops (DN13)
// ---------------------------------------------------------------------------

// migrDstNames are the names the stopped-step tests match on.
type migrDstNames struct {
	clone string // DnMigrFinalName, the dm-clone
	meta  string // DnMigrMetaDmName, the slot's wrapper
	lin   string // the CN's dm-linear
	dmErr string // the CN's dm-error
	nqn   string // the CN's export
}

// newMigrDstNames names the destination's objects with the stack of the
// primary of migrDstReq, testCn0.
func newMigrDstNames() migrDstNames {
	return migrDstNamesOf(testCn0)
}

// migrDstNamesOf is newMigrDstNames with the stack of cnId.
func migrDstNamesOf(cnId uint64) migrDstNames {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	return migrDstNames{
		clone: nf.DnMigrFinalName(testCluster, testDn, testSp, testMigrId),
		meta:  nf.DnMigrMetaDmName(testCluster, testDn, testSp, testMigrId),
		lin:   nf.DnLinearName(testCluster, testDn, testSp, testSide, cnId),
		dmErr: nf.DnErrorName(testCluster, testDn, testSp, testSide, cnId),
		nqn:   nf.SideToCnNqn(testCluster, testSp, testLeg, cnId),
	}
}

// flippedMigrDstReq is migrDstReq with the primary moved to testCn1.
func flippedMigrDstReq(revision uint64) *pb.SyncupSideRequest {
	req := migrDstReq(revision, pb.SpLevel_SP_LEVEL_READWRITE)
	req.SideConf.PrimaryCnId = testCn1
	req.SideConf.StandbyIdList = []uint64{testCn0}
	return req
}

func migrRetrying(srv *DnAgentServer) bool {
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return st.retrying
}

// servesThroughClone reports whether the dm-linear of names' CN maps the
// dm-clone and its namespace is in the optimized group — DN13 step (5)'s end
// state for the primary, read from the fake node.
func servesThroughClone(
	t *testing.T,
	srv *DnAgentServer,
	node *fakeNode,
	names migrDstNames,
) bool {
	t.Helper()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	node.mu.Lock()
	cloneNo, built := node.devNo[nf.DmPath(names.clone)]
	_, present := node.dms[names.clone]
	lin, ok := node.dms[names.lin]
	onClone := built && present && ok && strings.Contains(lin.table, cloneNo)
	node.mu.Unlock()
	if !onClone {
		return false
	}
	return anaGrpOf(t, srv, node, names.nqn) == common.AnaGrpIdOptimized
}

// sitsOnDmError reports whether the dm-linear of names' CN maps its dm-error
// and its namespace is in the inaccessible group — DN13 step (1)'s state,
// read from the fake node.
func sitsOnDmError(
	t *testing.T,
	srv *DnAgentServer,
	node *fakeNode,
	names migrDstNames,
) bool {
	t.Helper()
	return linearMapsDm(node, names.lin, names.dmErr) &&
		anaGrpOf(t, srv, node, names.nqn) == common.AnaGrpIdInaccessible
}

// linearMapsDm reports whether the live table of the dm-linear lin is a
// single linear target over the dm device dev.
func linearMapsDm(node *fakeNode, lin, dev string) bool {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	node.mu.Lock()
	defer node.mu.Unlock()
	devNo, built := node.devNo[nf.DmPath(dev)]
	linDm, ok := node.dms[lin]
	if !built || !ok {
		return false
	}
	fields := strings.Fields(linDm.table)
	return len(fields) == 5 && fields[2] == "linear" && fields[3] == devNo
}

func dmSuspended(node *fakeNode, name string) bool {
	node.mu.Lock()
	defer node.mu.Unlock()
	dm, ok := node.dms[name]
	return ok && dm.suspended
}

// anaGrpOf reads the ANA group of the namespace nqn exports from the fake
// node itself, never through the agent's reads, so a test can read it while
// one of those reads is held failing. A namespace with no ana_grpid reads 0,
// which is no group's id.
func anaGrpOf(
	t *testing.T,
	srv *DnAgentServer,
	node *fakeNode,
	nqn string,
) int {
	t.Helper()
	node.mu.Lock()
	raw, ok := node.files[srv.nvmet.NsPath(nqn, sideNsid)+"/ana_grpid"]
	node.mu.Unlock()
	if !ok {
		return 0
	}
	grpId, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		t.Fatalf("the ana_grpid of %s holds %q", nqn, raw)
	}
	return grpId
}

// assertStoppedAt fails the test unless the reply says its pass stopped at
// the call that key matches: `dm_clone_info` is ERROR with that call's error,
// which names it. A pass that did not stop there can end in the same state by
// another exit, so the end state alone does not say the stopped pass ran.
func assertStoppedAt(t *testing.T, reply *pb.SyncupSideReply, key string) {
	t.Helper()
	got := reply.GetSideInfo().GetMigrDstInfo().GetDmCloneInfo()
	if got.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(got.GetDetails(), key) {
		t.Fatalf("fixture is wrong: dm_clone_info = %v/%q, want ERROR from "+
			"the step that stopped (%q)", got.GetStatus(), got.GetDetails(),
			key)
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A destination pass that stops at a step other than the connect, or whose
// hydration will not converge (a knob or the enable), is retried with no
// further RPC until a pass runs the whole sequence. The worker re-sends a
// SyncupSide on a revision or a reply code, never on a row, so a step nothing
// retried would stay undone until the sp's next revision: with the source
// handed over by then (DN12), a destination with no dm-clone leaves the leg
// with no serving path, and one whose hydration is off copies nothing in the
// background.
func TestMigrationDestinationStoppedStepIsRetried(t *testing.T) {
	names := newMigrDstNames()
	for _, tc := range []struct {
		name string
		cmd  string
	}{
		{"the slot's wrapper", "dmsetup create " + names.meta},
		{"the dm-clone", "dmsetup create " + names.clone},
		{"hydration", "dmsetup message " + names.clone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.migrRetryInterval = 5 * time.Millisecond
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)

			// The step is refused until the registration has been read, so
			// no retry pass can run the whole sequence, and deregister,
			// before the test looks.
			setFailAlways(node, tc.cmd, "refused")
			if _, err := srv.SyncupSide(ctx,
				migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if !node.hasCall(tc.cmd) {
				t.Fatalf("fixture is wrong: %q never ran", tc.cmd)
			}
			if !migrRetrying(srv) {
				t.Fatal("no background retry was registered")
			}

			// The step answers from now on, and no further RPC comes. The
			// retry loop alone has to get there, and it deregisters itself
			// in the pass that does.
			clearFailAlways(node, tc.cmd)
			waitUntil(t, "the destination to serve through its dm-clone "+
				"with hydration on and no retry left", func() bool {
				if !servesThroughClone(t, srv, node, names) {
					return false
				}
				node.mu.Lock()
				hydrating := !node.dms[names.clone].noHydration
				node.mu.Unlock()
				return hydrating && !migrRetrying(srv)
			})
		})
	}
}

// A step that stops for a reason of its own — short of the source connection
// answering that it is gone — on a destination already serving through its
// dm-clone leaves the path where it is. Whether the dm-clone serves is read
// from where the per-CN stacks are — a per-CN stack serving through it — never
// inferred from the step that stopped: reloading the primary's dm-linear onto
// its dm-error over one unanswered command would cut the only serving path of
// the leg until a later pass finished the build. When that reading does not
// answer either — the dm-clone's probe or its device number, or the primary's
// dm-linear (its probe or its table) or its namespace's group, with no other
// stack serving through the dm-clone — the pass leaves the per-CN stacks
// exactly as they are and reports them read-only. Either way the reply carries
// every CN's rows, and says which step stopped: its `dm_clone_info` row is
// ERROR with the error of the call that stopped it, which names that call.
func TestAStoppedStepKeepsALiveCloneServing(t *testing.T) {
	names := newMigrDstNames()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	const killed = "signal: killed"
	// The fake's maps a fault goes into: a command killed before it touched
	// anything, and a read cut off by the soft timeout.
	cmdKilled := func(n *fakeNode) map[string]bool {
		return n.killCmdNoEffectAlways
	}
	readKilled := func(n *fakeNode) map[string]bool { return n.killReadAlways }
	// The primary's namespace group, by the NsPath the agent reads it
	// through; NsPath reads no field, and the table is built before any
	// server.
	grp := agent.NewNvmet(nil).NsPath(names.nqn, sideNsid) + "/ana_grpid"
	for _, tc := range []struct {
		name string
		// hook is the fake's map the fault goes into; key matches the call
		// of the step that stops.
		hook func(node *fakeNode) map[string]bool
		key  string
		// row is what the reply's dm_clone_info details must hold besides
		// key.
		row string
		// alsoHook and also, when set, are a second fault held beside key:
		// one of the reads that say where the stacks are.
		alsoHook func(node *fakeNode) map[string]bool
		also     string
	}{
		{"the wrapper's probe is killed",
			cmdKilled, "-o attr " + names.meta, killed, nil, ""},
		{"the wrapper's table read is killed",
			cmdKilled, "dmsetup table " + names.meta, killed, nil, ""},
		{"the source walk does not answer",
			readKilled, "/subsysnqn", "target not read", nil, ""},
		{"the dm-clone's table read is killed",
			cmdKilled, "dmsetup table " + names.clone, killed, nil, ""},
		{"the dm-clone's status read is killed",
			cmdKilled, "dmsetup status " + names.clone, killed, nil, ""},
		// Nothing answers for the dm-clone at all, its own probe included:
		// the per-CN layer is left exactly as it is, and the destination's
		// rows stay the step's own, not the read-only probe's.
		{"every probe of the dm-clone is killed",
			cmdKilled, "-o attr " + names.clone, killed, nil, ""},
		// The dm-clone answers, but where the stacks are does not: its
		// device number, or the primary's dm-linear (its probe or its table)
		// or namespace group, which together say whether it serves through
		// the dm-clone.
		{"the dm-clone's device number is not read",
			cmdKilled, "-o attr " + names.meta, killed,
			cmdKilled, "MAJ:MIN " + nf.DmPath(names.clone)},
		{"the primary's dm-linear probe is killed",
			cmdKilled, "-o attr " + names.meta, killed,
			cmdKilled, "-o attr " + names.lin},
		{"the primary's dm-linear table read is killed",
			cmdKilled, "-o attr " + names.meta, killed,
			cmdKilled, "dmsetup table " + names.lin},
		{"the primary's namespace group read does not answer",
			cmdKilled, "-o attr " + names.meta, killed,
			readKilled, grp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.migrRetryInterval = 5 * time.Millisecond
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			if _, err := srv.SyncupSide(ctx,
				migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if !servesThroughClone(t, srv, node, names) || migrRetrying(srv) {
				t.Fatal("fixture is wrong: the destination is not serving " +
					"through its dm-clone with no retry registered")
			}

			node.Reset()
			setHook(node, tc.hook(node), tc.key)
			if tc.also != "" {
				setHook(node, tc.alsoHook(node), tc.also)
			}
			reply, err := srv.SyncupSide(ctx,
				migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE))
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			for _, key := range []string{tc.key, tc.also} {
				if key != "" && len(node.callsMatching(key)) == 0 {
					t.Fatalf("fixture is wrong: nothing matched %q", key)
				}
			}
			if got := reply.GetSideInfo().GetMigrDstInfo().GetDmCloneInfo(); got.
				GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
				!strings.Contains(got.GetDetails(), tc.row) ||
				!strings.Contains(got.GetDetails(), tc.key) {
				t.Errorf("dm_clone_info = %v/%q, want ERROR holding %q and %q",
					got.GetStatus(), got.GetDetails(), tc.row, tc.key)
			}
			info := reply.GetSideInfo()
			for _, rows := range []struct {
				name string
				rows map[uint64]*pb.ResInfo
			}{
				{"cn_id_to_dm_error", info.GetCnIdToDmError()},
				{"cn_id_to_dm_linear", info.GetCnIdToDmLinear()},
				{"cn_id_to_nvmeof", info.GetCnIdToNvmeof()},
			} {
				for _, cnId := range []uint64{testCn0, testCn1} {
					if rows.rows[cnId] == nil {
						t.Errorf("%s holds no row for cn %d", rows.name, cnId)
					}
				}
			}
			if !servesThroughClone(t, srv, node, names) {
				t.Error("the stopped step took the serving path away")
			}
			if !migrRetrying(srv) {
				t.Error("no background retry was registered")
			}

			// Once the node answers again the retry finishes the pass and
			// deregisters itself — still without touching the path.
			clearHook(node, tc.hook(node), tc.key)
			if tc.also != "" {
				clearHook(node, tc.alsoHook(node), tc.also)
			}
			waitUntil(t, "the retry to finish the pass",
				func() bool { return !migrRetrying(srv) })
			if !servesThroughClone(t, srv, node, names) {
				t.Error("the retry took the serving path away")
			}
			if got := node.callsMatching(
				"cmd dmsetup reload " + names.lin); len(got) != 0 {
				t.Errorf("the primary's dm-linear was reloaded: %v", got)
			}
			if got := node.callsMatching("/ana_grpid="); len(got) != 0 {
				t.Errorf("a namespace changed its ANA group: %v", got)
			}
		})
	}
}

// A primary flip that keeps the old primary among the side's CNs, arriving in a
// pass whose step stopped, moves the serving path to the new primary at once.
// The old primary's stack still serves through the dm-clone, which is what says
// the dm-clone serves, so the new primary's stack goes onto it and the old
// primary's onto its dm-error in that pass — with only the new primary's
// dm-linear consulted, both would sit on their dm-errors, inaccessible, until a
// later pass finished the build.
func TestAPrimaryFlipInAStoppedPassMovesTheServingPath(t *testing.T) {
	oldPrimary, newPrimary := migrDstNamesOf(testCn0), migrDstNamesOf(testCn1)
	srv, node := newTestServer(t)
	srv.migrRetryInterval = 5 * time.Millisecond
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !servesThroughClone(t, srv, node, oldPrimary) || migrRetrying(srv) {
		t.Fatal("fixture is wrong: the destination is not serving through " +
			"its dm-clone with no retry registered")
	}

	key := "-o attr " + oldPrimary.meta
	setHook(node, node.killCmdNoEffectAlways, key)
	reply, err := srv.SyncupSide(ctx, flippedMigrDstReq(3))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertStoppedAt(t, reply, key)
	if !servesThroughClone(t, srv, node, newPrimary) {
		t.Error("the new primary does not serve through the dm-clone")
	}
	if !linearMapsDm(node, oldPrimary.lin, oldPrimary.dmErr) ||
		anaGrpOf(t, srv, node, oldPrimary.nqn) !=
			common.AnaGrpIdNonOptimized {
		t.Error("the old primary is not on its dm-error, non-optimized")
	}
	if !migrRetrying(srv) {
		t.Error("no background retry was registered")
	}

	clearHook(node, node.killCmdNoEffectAlways, key)
	waitUntil(t, "the retry to finish the pass",
		func() bool { return !migrRetrying(srv) })
	if !servesThroughClone(t, srv, node, newPrimary) {
		t.Error("the retry took the serving path off the new primary")
	}
}

// A destination whose source connection answers that it is gone — no
// controller, and a connect that fails — is taken off its dm-clone, which has
// lost the device it reads unhydrated regions from: the stacks go to their
// dm-errors with every namespace inaccessible, and the retry puts the
// destination back on its dm-clone once a connect succeeds. The pass that
// found the source gone leaves the dm-clone in place, still mapping the
// device that is gone, so a later pass that stops — at the slot's wrapper,
// or at a walk of the source connection that does not answer — must not read
// the dm-clone's presence as serving: it keeps the stacks where they are. So
// does a stopped pass that cannot read where the stacks are either — the
// dm-clone's probe or its device number, the primary's dm-linear's probe or
// table: it leaves them on their dm-errors and moves no namespace.
func TestADestinationThatLostItsSourceStaysOffItsClone(t *testing.T) {
	names := newMigrDstNames()
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	for _, tc := range []struct {
		name string
		// key, when set, is held killed (with no effect) over one more
		// SyncupSide after the source is lost.
		key string
		// also, when set, is held killed (with no effect) beside key: one
		// of the reads that say where the stacks are.
		also string
	}{
		{"the connect answers again", "", ""},
		{"the wrapper's probe is killed", "-o attr " + names.meta, ""},
		// The sweep lists the subsystems first, so its listing does not
		// answer either.
		{"the source walk does not answer",
			"ls -1 /sys/class/nvme-subsystem", ""},
		{"the dm-clone's probe is killed too",
			"-o attr " + names.meta, "-o attr " + names.clone},
		{"the dm-clone's device number is not read either",
			"-o attr " + names.meta, "MAJ:MIN " + nf.DmPath(names.clone)},
		{"the primary's dm-linear probe is killed too",
			"-o attr " + names.meta, "-o attr " + names.lin},
		{"the primary's dm-linear table read is killed too",
			"-o attr " + names.meta, "dmsetup table " + names.lin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.migrRetryInterval = 5 * time.Millisecond
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			if _, err := srv.SyncupSide(ctx,
				migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if !servesThroughClone(t, srv, node, names) || migrRetrying(srv) {
				t.Fatal("fixture is wrong: the destination is not serving " +
					"through its dm-clone with no retry registered")
			}

			// The source's controller goes, and every connect is refused
			// until the test lets it through.
			srcNqn := srv.nf.MigrSrcNqn(
				testCluster, testSrcDn, testSp, testMigrId)
			node.mu.Lock()
			_, code := node.nvmeDisconnect(
				[]string{"disconnect", "--nqn", srcNqn})
			node.mu.Unlock()
			if code != 0 {
				t.Fatal("fixture is wrong: the source was not connected")
			}
			setFailAlways(node, "nvme connect",
				"failed to write to nvme-fabrics device")
			reply, err := srv.SyncupSide(ctx,
				migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE))
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if got := reply.GetSideInfo().GetMigrDstInfo().GetDmCloneInfo(); got.
				GetStatus() != pb.ResStatus_RES_STATUS_MISSING ||
				got.GetDetails() != "target not connected" {
				t.Errorf("dm_clone_info = %v/%q, want MISSING/%q",
					got.GetStatus(), got.GetDetails(), "target not connected")
			}
			if !dmPresent(node, names.clone) {
				t.Fatal("fixture is wrong: the pass removed the dm-clone")
			}
			if !sitsOnDmError(t, srv, node, names) {
				t.Error("a lost source left the primary off its dm-error")
			}
			if !migrRetrying(srv) {
				t.Error("no background retry was registered")
			}

			if tc.key != "" {
				node.Reset()
				setHook(node, node.killCmdNoEffectAlways, tc.key)
				if tc.also != "" {
					setHook(node, node.killCmdNoEffectAlways, tc.also)
				}
				reply, err := srv.SyncupSide(ctx,
					migrDstReq(4, pb.SpLevel_SP_LEVEL_READWRITE))
				if err != nil {
					t.Fatalf("SyncupSide: %v", err)
				}
				assertStoppedAt(t, reply, tc.key)
				if tc.also != "" && len(node.callsMatching(tc.also)) == 0 {
					t.Fatalf("fixture is wrong: nothing matched %q", tc.also)
				}
				if !sitsOnDmError(t, srv, node, names) {
					t.Error("a stopped step put the primary back on a " +
						"dm-clone whose source is gone")
				}
				if got := node.callsMatching(
					"cmd dmsetup reload " + names.lin); len(got) != 0 {
					t.Errorf("the primary's dm-linear was reloaded: %v", got)
				}
				if got := node.callsMatching("/ana_grpid="); len(got) != 0 {
					t.Errorf("a namespace changed its ANA group: %v", got)
				}
				clearHook(node, node.killCmdNoEffectAlways, tc.key)
				if tc.also != "" {
					clearHook(node, node.killCmdNoEffectAlways, tc.also)
				}
			}

			// The connect answers again, and no RPC follows.
			clearFailAlways(node, "nvme connect")
			waitUntil(t, "the retry to put the destination back on its "+
				"dm-clone with no retry left", func() bool {
				return servesThroughClone(t, srv, node, names) &&
					!migrRetrying(srv)
			})
		})
	}
}

// DN13 step (5) — the primary's dm-linear onto the dm-clone, its namespace to
// the optimized group — is retried like every step before it. The worker
// re-sends a SyncupSide on a revision or a reply code, never on a row, so a
// step (5) that failed with nothing retrying it would stay failed until the
// sp's next revision: after a refused reload the primary's dm-linear stays
// suspended on its old table (a reload fails closed) while its namespace is
// optimized over it, and after an ANA move that did not happen no CN has an
// optimized path. A pass whose steps before step (5) all held deregisters the
// retry only once step (5) has held too, and registers it otherwise, so the
// destination ends serving through its dm-clone with no further RPC and no
// retry left.
func TestMigrationDestinationStepFiveIsRetried(t *testing.T) {
	names := newMigrDstNames()
	reload := "dmsetup reload " + names.lin
	refuseOnce := func(node *fakeNode, key string) {
		node.mu.Lock()
		defer node.mu.Unlock()
		node.failCmd[key] = "refused once"
	}
	refused := func(t *testing.T, node *fakeNode, key string) {
		t.Helper()
		node.mu.Lock()
		_, armed := node.failCmd[key]
		node.mu.Unlock()
		if armed {
			t.Fatalf("fixture is wrong: %q never ran", key)
		}
	}
	served := func(t *testing.T, srv *DnAgentServer, node *fakeNode,
		names migrDstNames) {
		t.Helper()
		waitUntil(t, "the retry to finish step (5) with no retry left",
			func() bool {
				return servesThroughClone(t, srv, node, names) &&
					!migrRetrying(srv)
			})
		if dmSuspended(node, names.lin) {
			t.Error("the primary's dm-linear is left suspended")
		}
	}

	// The fault lands in the retry loop's own pass: the connect is refused
	// until the registration has been read, and the loop's first pass after
	// it answers is the one that finishes the build. That pass is held at
	// step (5)'s suspend, and the side is still registered there.
	t.Run("a refused reload in the retry loop's pass", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.migrRetryInterval = 5 * time.Millisecond
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		setFailAlways(node, "nvme connect",
			"failed to write to nvme-fabrics device")
		if _, err := srv.SyncupSide(ctx,
			migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if !migrRetrying(srv) || !sitsOnDmError(t, srv, node, names) {
			t.Fatal("fixture is wrong: the refused connect did not leave " +
				"the stacks on their dm-errors with a retry registered")
		}

		suspend := "dmsetup suspend " + names.lin
		node.Reset()
		node.blockCmd(suspend)
		t.Cleanup(func() { node.releaseCmd(suspend) })
		refuseOnce(node, reload)
		clearFailAlways(node, "nvme connect")
		waitUntil(t, "the retry loop's pass to reach step (5)",
			func() bool { return node.hasCall("cmd " + suspend) })
		if !migrRetrying(srv) {
			t.Error("the pass that finishes the build dropped the retry " +
				"before its step (5) held")
		}
		node.releaseCmd(suspend)
		served(t, srv, node, names)
		refused(t, node, reload)
	})

	// The fault lands in an RPC pass with no retry registered before it: the
	// pass that turns the serving side into a destination and builds it.
	t.Run("a refused reload in an RPC pass", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.migrRetryInterval = 5 * time.Millisecond
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		refuseOnce(node, reload)
		reply, err := srv.SyncupSide(ctx,
			migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		refused(t, node, reload)
		info := reply.GetSideInfo()
		if got := info.GetMigrDstInfo().GetDmCloneInfo(); got.GetStatus() !=
			pb.ResStatus_RES_STATUS_OK {
			t.Fatalf("fixture is wrong: dm_clone_info = %v/%q, want OK",
				got.GetStatus(), got.GetDetails())
		}
		if got := info.GetCnIdToDmLinear()[testCn0]; got.GetStatus() !=
			pb.ResStatus_RES_STATUS_ERROR {
			t.Errorf("the primary's dm_linear row = %v/%q, want ERROR",
				got.GetStatus(), got.GetDetails())
		}
		served(t, srv, node, names)
	})

	// The ANA move does not happen: on a destination already serving, the
	// new primary's namespace probe is killed in the pass that flips the
	// primary, so its namespace stays non-optimized over a dm-linear that is
	// already on the dm-clone, and no CN has an optimized path.
	t.Run("a killed namespace probe in a primary flip", func(t *testing.T) {
		newPrimary := migrDstNamesOf(testCn1)
		srv, node := newTestServer(t)
		srv.migrRetryInterval = 5 * time.Millisecond
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		if _, err := srv.SyncupSide(ctx,
			migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if !servesThroughClone(t, srv, node, names) || migrRetrying(srv) {
			t.Fatal("fixture is wrong: the destination is not serving " +
				"through its dm-clone with no retry registered")
		}

		probe := "ls -1 " + srv.nvmet.NsPath(newPrimary.nqn, sideNsid)
		setHook(node, node.killCmdNoEffect, probe)
		reply, err := srv.SyncupSide(ctx, flippedMigrDstReq(3))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		node.mu.Lock()
		armed := node.killCmdNoEffect[probe]
		node.mu.Unlock()
		if armed {
			t.Fatalf("fixture is wrong: %q never ran", probe)
		}
		info := reply.GetSideInfo()
		if got := info.GetMigrDstInfo().GetDmCloneInfo(); got.GetStatus() !=
			pb.ResStatus_RES_STATUS_OK {
			t.Fatalf("fixture is wrong: dm_clone_info = %v/%q, want OK",
				got.GetStatus(), got.GetDetails())
		}
		if got := info.GetCnIdToNvmeof()[testCn1]; got.GetStatus() !=
			pb.ResStatus_RES_STATUS_ERROR {
			t.Errorf("the new primary's nvmeof row = %v/%q, want ERROR",
				got.GetStatus(), got.GetDetails())
		}
		served(t, srv, node, newPrimary)
	})
}

// A destination whose slot the metadata area cannot supply says so on both
// channels in the same rows — the converge's reply and the check round's
// probe — so the side stays unhealthy for as long as it is starved instead of
// trading the rows, and the epoch on them, every round; and it builds with no
// further RPC once the units are free.
func TestMigrationDestinationExhaustionIsReportedAndRetried(t *testing.T) {
	srv, node := newTestServer(t)
	srv.migrRetryInterval = 5 * time.Millisecond
	ctx := context.Background()
	names := newMigrDstNames()
	syncupBoth(t, srv, 1, testSide)

	// Another sp's migration holds the whole area.
	const otherSp, otherMigr = testSp + 1, testMigrId + 1
	if _, err := srv.meta.AllocCloneMeta(ctx, otherSp, otherMigr,
		common.DnCloneMetaSize); err != nil {
		t.Fatalf("AllocCloneMeta: %v", err)
	}

	reply, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Fatalf("SyncupSide code %d, want 0", got)
	}
	synced := reply.GetSideInfo().GetMigrDstInfo()
	if got := synced.GetTargetInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_MISSING ||
		got.GetDetails() != detailsCloneMetaMissing {
		t.Errorf("target_info = %v/%q, want MISSING/%q", got.GetStatus(),
			got.GetDetails(), detailsCloneMetaMissing)
	}
	if got := synced.GetDmCloneInfo(); got.GetStatus() !=
		pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(got.GetDetails(), "no contiguous run") {
		t.Errorf("dm_clone_info = %v/%q, want ERROR with the allocator's "+
			"refusal", got.GetStatus(), got.GetDetails())
	}
	if !migrRetrying(srv) {
		t.Error("no background retry was registered")
	}
	// The step stopped with no dm-clone: the stacks sit where step (1) puts
	// them. Every retry pass is starved the same way, so this holds.
	if !sitsOnDmError(t, srv, node, names) {
		t.Error("a stopped step with no dm-clone left the primary off its " +
			"dm-error")
	}

	// The check round reports the same refusal, row for row, epoch included.
	check, _ := srv.checkSideRound(ctx, &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn,
		SidePointer: sidePtr(testSide), Revision: 2, ShowInfo: true,
	}, nil)
	if got := check.GetAgentReply().GetCode(); got != 0 {
		t.Errorf("CheckSide code %d, want 0", got)
	}
	probed := check.GetSideInfo().GetMigrDstInfo()
	for _, row := range []struct {
		name          string
		synced, probe *pb.ResInfo
	}{
		{"target_info", synced.GetTargetInfo(), probed.GetTargetInfo()},
		{"dm_clone_info", synced.GetDmCloneInfo(), probed.GetDmCloneInfo()},
	} {
		if row.probe.GetStatus() != row.synced.GetStatus() ||
			row.probe.GetDetails() != row.synced.GetDetails() ||
			row.probe.GetEpoch() != row.synced.GetEpoch() {
			t.Errorf("%s: the check round reports %v/%q at epoch %d, the "+
				"converge %v/%q at epoch %d", row.name,
				row.probe.GetStatus(), row.probe.GetDetails(),
				row.probe.GetEpoch(), row.synced.GetStatus(),
				row.synced.GetDetails(), row.synced.GetEpoch())
		}
	}
	if dmPresent(node, names.clone) {
		t.Fatal("fixture is wrong: the dm-clone was built in a full area")
	}

	// The other migration ends; no RPC follows.
	if err := srv.meta.FreeCloneMeta(ctx, otherSp, otherMigr); err != nil {
		t.Fatalf("FreeCloneMeta: %v", err)
	}
	waitUntil(t, "the starved destination to build and serve", func() bool {
		return servesThroughClone(t, srv, node, names) && !migrRetrying(srv)
	})
}

// A pass that takes the stacks off a dm-clone whose source is gone can have
// its reload of the primary's dm-linear refused: a reload fails closed, so the
// dm-linear stays on the dm-clone, suspended, while its namespace goes to the
// inaccessible group, and a later pass's pre-step resumes it there
// (unfenceLinears). A later pass that stops must not read that stack as
// serving: putting its namespace back in the optimized group would serve a
// dm-clone with no source for the regions it has not hydrated
// (migrCloneMapped). One whose read of the primary's namespace group does not
// answer cannot read whether the stack serves: it leaves the stack as it is,
// its namespace out of the optimized group. The next pass that reads the stack
// reads it as off the dm-clone and finishes the move onto the dm-error.
func TestALinearLeftOnASourcelessCloneIsNotServing(t *testing.T) {
	names := newMigrDstNames()
	srv, node := newTestServer(t)
	// No retry pass runs between the RPCs: every pass here is the test's.
	srv.migrRetryInterval = time.Hour
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !servesThroughClone(t, srv, node, names) {
		t.Fatal("fixture is wrong: the destination is not serving through " +
			"its dm-clone")
	}

	// The source's controller goes, every connect is refused, and the reload
	// that takes the primary off the dm-clone is refused once.
	srcNqn := srv.nf.MigrSrcNqn(testCluster, testSrcDn, testSp, testMigrId)
	node.mu.Lock()
	_, code := node.nvmeDisconnect([]string{"disconnect", "--nqn", srcNqn})
	node.failCmd["dmsetup reload "+names.lin] = "refused once"
	node.mu.Unlock()
	if code != 0 {
		t.Fatal("fixture is wrong: the source was not connected")
	}
	setFailAlways(node, "nvme connect",
		"failed to write to nvme-fabrics device")
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !dmSuspended(node, names.lin) ||
		!linearMapsDm(node, names.lin, names.clone) {
		t.Fatal("fixture is wrong: the refused reload did not leave the " +
			"primary's dm-linear suspended on the dm-clone")
	}

	// A later pass stops at the slot's wrapper, and the primary's namespace
	// group read does not answer either: the pass cannot read whether the
	// stack serves.
	key := "-o attr " + names.meta
	grp := srv.nvmet.NsPath(names.nqn, sideNsid) + "/ana_grpid"
	node.Reset()
	setHook(node, node.killCmdNoEffectAlways, key)
	setHook(node, node.killReadAlways, grp)
	reply, err := srv.SyncupSide(ctx,
		migrDstReq(4, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	clearHook(node, node.killReadAlways, grp)
	assertStoppedAt(t, reply, key)
	if len(node.callsMatching(grp)) == 0 {
		t.Fatalf("fixture is wrong: nothing read %q", grp)
	}
	if !linearMapsDm(node, names.lin, names.clone) {
		t.Error("a pass that could not read the stack moved the primary's " +
			"dm-linear")
	}
	if got := node.callsMatching("/ana_grpid="); len(got) != 0 {
		t.Errorf("a pass that could not read the stack moved a namespace: %v",
			got)
	}
	if anaGrpOf(t, srv, node, names.nqn) == common.AnaGrpIdOptimized {
		t.Error("a pass that could not read the stack served a dm-clone " +
			"whose source is gone")
	}

	// The next pass stops at the slot's wrapper alone. The connect is still
	// refused, so a pass that did not stop would take the not-connected exit
	// to the same end state: the stop is asserted, not inferred.
	reply, err = srv.SyncupSide(ctx,
		migrDstReq(5, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	clearHook(node, node.killCmdNoEffectAlways, key)
	assertStoppedAt(t, reply, key)
	if linearMapsDm(node, names.lin, names.clone) {
		t.Error("a stopped pass kept the primary's dm-linear on a dm-clone " +
			"whose source is gone")
	}
	if dmSuspended(node, names.lin) {
		t.Error("the primary's dm-linear is still suspended")
	}
	if !sitsOnDmError(t, srv, node, names) {
		t.Error("the stopped pass did not finish the move onto the dm-error")
	}
}

// Each attempt of the retry loop runs on rootCtx, not on the loop's own
// context, and this is the pass that needs it: a destination role that ends
// while its retry is registered, in a SyncupSide whose sweep could not list
// the dm devices. That sweep skips its pre-steps, so the registration stays,
// and the loop's next pass is the one whose sweep deregisters it
// (sidePreSteps) before it removes the dm-clone the role no longer wants. An
// attempt on the loop's context would be cancelled by that deregistration
// halfway through its own pass: the removal would fail on the dead context,
// and with the registration gone nothing would tick again.
func TestARoleEndedUnderAnUnlistedSweepIsTornDownByTheRetry(t *testing.T) {
	names := newMigrDstNames()
	srv, node := newTestServer(t)
	srv.migrRetryInterval = 5 * time.Millisecond
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	// Hydration never turns on, so every pass keeps the retry registered.
	setFailAlways(node, "dmsetup message "+names.clone, "refused")
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !dmPresent(node, names.clone) || !migrRetrying(srv) {
		t.Fatal("fixture is wrong: want a dm-clone and a registered retry")
	}

	// The role ends in a pass whose sweep cannot list the dm devices.
	setHook(node, node.killCmdNoEffectAlways, "dmsetup ls")
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_NO_MIGRATION)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	registered, present := migrRetrying(srv), dmPresent(node, names.clone)
	clearHook(node, node.killCmdNoEffectAlways, "dmsetup ls")
	if !registered || !present {
		t.Fatalf("fixture is wrong: after the unlisted pass registered = "+
			"%v, dm-clone present = %v; want both", registered, present)
	}

	// No further RPC: the loop's next pass deregisters and tears the role
	// down.
	waitUntil(t, "the retry's pass to remove the dm-clone the role no "+
		"longer wants, with no retry left", func() bool {
		return !dmPresent(node, names.clone) && !migrRetrying(srv)
	})
}
