package dnagent

import (
	"context"
	"fmt"
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
// hydration will not converge (a knob or the enable), leaves its rows as they
// read, and nothing runs the converge again by itself. The side's verdict
// names the dm-clone row on both channels while the step fails (SH31), so
// the worker re-sends the request it holds on each round (dnv-worker.md
// RW4), and the first re-send after the step answers finishes the sequence.
// With the source handed over by then (DN12), a destination with no dm-clone
// leaves the leg with no serving path, and one whose hydration is off copies
// nothing in the background, so a step nothing re-drove would stay undone
// until the sp's next revision.
func TestAStoppedDestinationStepIsCuredByTheResend(t *testing.T) {
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
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)

			setFailAlways(node, tc.cmd, "refused")
			req := migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)
			reply, err := srv.SyncupSide(ctx, req)
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if !node.hasCall(tc.cmd) {
				t.Fatalf("fixture is wrong: %q never ran", tc.cmd)
			}
			assertNamed(t, "the converge", reply, names.clone)
			assertNothingRunsBetweenCalls(t, node)

			// While the step fails, a round names it and its re-send stops
			// at the same step.
			round, resent := workerRound(t, srv, req, "round-1")
			assertNamed(t, "round-1", round, names.clone)
			if resent == nil {
				t.Fatal("round-1 re-sent nothing")
			}
			assertNamed(t, "round-1's re-send", resent, names.clone)

			// The step answers from now on: the next round's re-send, under
			// that round's trace id, finishes the sequence.
			clearFailAlways(node, tc.cmd)
			round, resent = workerRound(t, srv, req, "round-2")
			assertNamed(t, "round-2", round, names.clone)
			if resent == nil {
				t.Fatal("round-2 re-sent nothing")
			}
			assertNotNamed(t, "round-2's re-send", resent)
			if !tracedCall(node, "round-2", tc.cmd) {
				t.Errorf("%q did not run under round-2's trace id", tc.cmd)
			}
			if !servesThroughClone(t, srv, node, names) {
				t.Error("the destination does not serve through its dm-clone")
			}
			node.mu.Lock()
			hydrating := !node.dms[names.clone].noHydration
			node.mu.Unlock()
			if !hydrating {
				t.Error("hydration is off")
			}
			round, resent = workerRound(t, srv, req, "round-3")
			assertNotNamed(t, "round-3", round)
			if resent != nil {
				t.Errorf("round-3 re-sent: %v", resent.GetAgentReply())
			}
			assertNothingRunsBetweenCalls(t, node,
				"round-1", "round-2", "round-3")
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
// ERROR with the error of the call that stopped it, which names that call,
// and which the side's verdict names on both channels while the fault stands.
// A re-send meanwhile stops the same way and leaves the path alone. Once the
// node answers, a read finds nothing a converge would act on: it names
// nothing, so nothing is re-sent, and the path stays where it is.
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
			cmdKilled, "dmsetup status --noflush " + names.clone, killed, nil, ""},
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
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			if _, err := srv.SyncupSide(ctx,
				migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if !servesThroughClone(t, srv, node, names) {
				t.Fatal("fixture is wrong: the destination is not serving " +
					"through its dm-clone")
			}

			node.Reset()
			setHook(node, tc.hook(node), tc.key)
			if tc.also != "" {
				setHook(node, tc.alsoHook(node), tc.also)
			}
			req := migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)
			reply, err := srv.SyncupSide(ctx, req)
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
			assertNamed(t, "the converge", reply, names.clone)

			// While the fault stands, a round names the row and its re-send
			// stops the same way.
			round, resent := workerRound(t, srv, req, "round-1")
			assertNamed(t, "round-1", round, names.clone)
			if resent == nil {
				t.Fatal("round-1 re-sent nothing")
			}
			assertNamed(t, "round-1's re-send", resent, names.clone)
			if !servesThroughClone(t, srv, node, names) {
				t.Error("the re-sent pass took the serving path away")
			}

			// Once the node answers again the read finds nothing to do, and
			// the round re-sends nothing.
			clearHook(node, tc.hook(node), tc.key)
			if tc.also != "" {
				clearHook(node, tc.alsoHook(node), tc.also)
			}
			round, resent = workerRound(t, srv, req, "round-2")
			assertNotNamed(t, "round-2", round)
			if resent != nil {
				t.Errorf("round-2 re-sent: %v", resent.GetAgentReply())
			}
			assertNothingRunsBetweenCalls(t, node, "round-1", "round-2")
			if !servesThroughClone(t, srv, node, names) {
				t.Error("the serving path moved")
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
// later pass finished the build. Once the node answers, the read finds
// nothing a converge would act on, so nothing is re-sent.
func TestAPrimaryFlipInAStoppedPassMovesTheServingPath(t *testing.T) {
	oldPrimary, newPrimary := migrDstNamesOf(testCn0), migrDstNamesOf(testCn1)
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !servesThroughClone(t, srv, node, oldPrimary) {
		t.Fatal("fixture is wrong: the destination is not serving through " +
			"its dm-clone")
	}

	key := "-o attr " + oldPrimary.meta
	setHook(node, node.killCmdNoEffectAlways, key)
	req := flippedMigrDstReq(3)
	reply, err := srv.SyncupSide(ctx, req)
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
	assertNamed(t, "the converge", reply, oldPrimary.clone)

	clearHook(node, node.killCmdNoEffectAlways, key)
	round, resent := workerRound(t, srv, req, "round-1")
	assertNotNamed(t, "round-1", round)
	if resent != nil {
		t.Errorf("round-1 re-sent: %v", resent.GetAgentReply())
	}
	assertNothingRunsBetweenCalls(t, node, "round-1")
	if !servesThroughClone(t, srv, node, newPrimary) {
		t.Error("the serving path left the new primary")
	}
}

// A destination whose source connection answers that it is gone — no
// controller, and a connect that fails — is taken off its dm-clone, which has
// lost the device it reads unhydrated regions from: the stacks go to their
// dm-errors with every namespace inaccessible. Both destination rows are
// named while the walk finds no controller, since a converge connects then,
// and nothing runs the converge again but the worker's re-send, which puts
// the destination back on its dm-clone once a connect succeeds. The pass that
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
			"list /sys/class/nvme-subsystem", ""},
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
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			if _, err := srv.SyncupSide(ctx,
				migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if !servesThroughClone(t, srv, node, names) {
				t.Fatal("fixture is wrong: the destination is not serving " +
					"through its dm-clone")
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
			// Every refused connect spends the pass's connect budget, on a
			// clock that does not sleep.
			withDnClock(srv, node)
			setFailAlways(node, "nvme connect",
				"failed to write to nvme-fabrics device")
			held := migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)
			reply, err := srv.SyncupSide(ctx, held)
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
			assertNamed(t, "the converge", reply, srcNqn, names.clone)

			if tc.key != "" {
				node.Reset()
				setHook(node, node.killCmdNoEffectAlways, tc.key)
				if tc.also != "" {
					setHook(node, node.killCmdNoEffectAlways, tc.also)
				}
				held = migrDstReq(4, pb.SpLevel_SP_LEVEL_READWRITE)
				reply, err := srv.SyncupSide(ctx, held)
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

			// Nothing runs between the calls. The connect answers again:
			// the next round names both rows, and its re-send connects and
			// puts the destination back on its dm-clone.
			assertNothingRunsBetweenCalls(t, node)
			clearFailAlways(node, "nvme connect")
			round, resent := workerRound(t, srv, held, "round-1")
			assertNamed(t, "round-1", round, srcNqn, names.clone)
			if resent == nil {
				t.Fatal("round-1 re-sent nothing")
			}
			assertNotNamed(t, "round-1's re-send", resent)
			if !tracedCall(node, "round-1", "cmd nvme connect ") {
				t.Error("the connect did not run under round-1's trace id")
			}
			if !servesThroughClone(t, srv, node, names) {
				t.Error("the re-send did not put the destination back on " +
					"its dm-clone")
			}
			round, resent = workerRound(t, srv, held, "round-2")
			assertNotNamed(t, "round-2", round)
			if resent != nil {
				t.Errorf("round-2 re-sent: %v", resent.GetAgentReply())
			}
		})
	}
}

// DN13 step (5) — the primary's dm-linear onto the dm-clone, its namespace to
// the optimized group — is re-driven like every step before it: by the
// worker's re-send of the request it holds, which the side's verdict brings
// while the step's row reads not as wanted (SH31). After a refused reload
// the primary's dm-linear stays suspended on its old table (a reload fails
// closed) while its namespace is optimized over it, and after an ANA move
// that did not happen no CN has an optimized path; either is named on both
// channels, nothing runs the converge again between the worker's calls, and
// one re-send leaves the destination serving through its dm-clone.
func TestAFailedStepFiveIsCuredByTheResend(t *testing.T) {
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
	// cured plays the worker's rounds of held, whose step (5) row the last
	// converge left named: the first round names it and its re-send
	// finishes step (5), and the next round names nothing.
	cured := func(t *testing.T, srv *DnAgentServer, node *fakeNode,
		held *pb.SyncupSideRequest, names migrDstNames, row string) {
		t.Helper()
		round, resent := workerRound(t, srv, held, "cure-1")
		assertNamed(t, "cure-1", round, row)
		if resent == nil {
			t.Fatal("cure-1 re-sent nothing")
		}
		assertNotNamed(t, "cure-1's re-send", resent)
		round, resent = workerRound(t, srv, held, "cure-2")
		assertNotNamed(t, "cure-2", round)
		if resent != nil {
			t.Errorf("cure-2 re-sent: %v", resent.GetAgentReply())
		}
		if !servesThroughClone(t, srv, node, names) {
			t.Error("the destination does not serve through its dm-clone")
		}
		if dmSuspended(node, names.lin) {
			t.Error("the primary's dm-linear is left suspended")
		}
	}

	// The fault lands in a re-sent pass: the connect is refused in the
	// converge that builds the destination, and the re-send the next round
	// brings connects and is the pass whose step (5) reload is refused.
	t.Run("a refused reload in a re-sent pass", func(t *testing.T) {
		srv, node := newTestServer(t)
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		// Every refused connect spends the pass's connect budget, on a
		// clock that does not sleep.
		withDnClock(srv, node)
		setFailAlways(node, "nvme connect",
			"failed to write to nvme-fabrics device")
		held := migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)
		if _, err := srv.SyncupSide(ctx, held); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if !sitsOnDmError(t, srv, node, names) {
			t.Fatal("fixture is wrong: the refused connect did not leave " +
				"the stacks on their dm-errors")
		}

		node.Reset()
		refuseOnce(node, reload)
		clearFailAlways(node, "nvme connect")
		round, resent := workerRound(t, srv, held, "round-1")
		assertNamed(t, "round-1", round, names.clone)
		if resent == nil {
			t.Fatal("round-1 re-sent nothing")
		}
		refused(t, node, reload)
		if !tracedCall(node, "round-1", "cmd "+reload) {
			t.Error("the refused reload did not run under round-1's id")
		}
		assertNamed(t, "round-1's re-send", resent, names.lin)
		assertNothingRunsBetweenCalls(t, node, "round-1")
		cured(t, srv, node, held, names, names.lin)
	})

	// The fault lands in the pass that turns the serving side into a
	// destination and builds it.
	t.Run("a refused reload in the building pass", func(t *testing.T) {
		srv, node := newTestServer(t)
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		refuseOnce(node, reload)
		held := migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)
		reply, err := srv.SyncupSide(ctx, held)
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
		assertNamed(t, "the converge", reply, names.lin)
		assertNothingRunsBetweenCalls(t, node)
		cured(t, srv, node, held, names, names.lin)
	})

	// The ANA move does not happen: on a destination already serving, the
	// new primary's namespace probe is killed in the pass that flips the
	// primary, so its namespace stays non-optimized over a dm-linear that is
	// already on the dm-clone, and no CN has an optimized path.
	t.Run("a killed namespace probe in a primary flip", func(t *testing.T) {
		newPrimary := migrDstNamesOf(testCn1)
		srv, node := newTestServer(t)
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		if _, err := srv.SyncupSide(ctx,
			migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if !servesThroughClone(t, srv, node, names) {
			t.Fatal("fixture is wrong: the destination is not serving " +
				"through its dm-clone")
		}

		probe := "list " + srv.nvmet.NsPath(newPrimary.nqn, sideNsid)
		setHook(node, node.killCmdNoEffect, probe)
		held := flippedMigrDstReq(3)
		reply, err := srv.SyncupSide(ctx, held)
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
		// The new primary's namespace is not optimized, so its export
		// does not serve as the primary's: an ERROR row, which the
		// reply's code names (SH31).
		if got := info.GetCnIdToNvmeof()[testCn1]; got.GetStatus() !=
			pb.ResStatus_RES_STATUS_ERROR {
			t.Errorf("the new primary's nvmeof row = %v/%q, want ERROR",
				got.GetStatus(), got.GetDetails())
		}
		if got := reply.GetAgentReply(); got.GetCode() !=
			common.ReplyCodeLeftover || !strings.Contains(got.GetDetails(),
			newPrimary.nqn+": differs:") {
			t.Errorf("reply = %v, want the leftover code naming %s",
				got, newPrimary.nqn)
		}
		assertNothingRunsBetweenCalls(t, node)
		cured(t, srv, node, held, newPrimary, newPrimary.nqn)
	})
}

// A destination whose slot the metadata area has no free run for waits: both
// migr_dst_info rows read PROVISIONING with the area's text, so no health
// epoch is set (DN13). It says so on both channels in the same rows — the
// converge's reply and the probe of the check round's read — instead of
// trading the rows, and the epoch on them, every round. A wait is not named,
// and nothing runs the converge again between the worker's calls. Once the
// units are free the read finds no run held for the slot, which it names
// (HeldCloneMeta), and the re-send that brings builds the destination. A
// destination normally takes its run before it zeroes
// (TestMigrationDestinationWaitsForRoomBeforeZeroing); this one provisions
// as a plain side, so DN13 step (2) is where it meets the full area.
func TestMigrationDestinationExhaustionIsReportedAndResent(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	names := newMigrDstNames()
	syncupBoth(t, srv, 1, testSide)

	// Another sp's migration holds the whole area.
	const otherSp, otherMigr = testSp + 1, testMigrId + 1
	if _, err := srv.meta.AllocCloneMeta(ctx, testCluster, testDn, otherSp, otherMigr,
		common.DnCloneMetaUnitCnt); err != nil {
		t.Fatalf("AllocCloneMeta: %v", err)
	}

	held := migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)
	reply, err := srv.SyncupSide(ctx, held)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := reply.GetAgentReply().GetCode(); got != 0 {
		t.Fatalf("SyncupSide code %d, want 0", got)
	}
	synced := reply.GetSideInfo().GetMigrDstInfo()
	for _, row := range []struct {
		name string
		info *pb.ResInfo
	}{
		{"target_info", synced.GetTargetInfo()},
		{"dm_clone_info", synced.GetDmCloneInfo()},
	} {
		if got := row.info; got.GetStatus() !=
			pb.ResStatus_RES_STATUS_PROVISIONING ||
			!strings.HasPrefix(got.GetDetails(), tagMetaRoomWait+": ") ||
			!strings.Contains(got.GetDetails(), "no contiguous run") {
			t.Errorf("%s = %v/%q, want PROVISIONING with %q and the "+
				"allocator's refusal", row.name, got.GetStatus(),
				got.GetDetails(), tagMetaRoomWait)
		}
	}
	// The step stopped with no dm-clone: the stacks sit where step (1) puts
	// them.
	if !sitsOnDmError(t, srv, node, names) {
		t.Error("a stopped step with no dm-clone left the primary off its " +
			"dm-error")
	}

	// The check round reports the same refusal, row for row, epoch included.
	check, _ := checkSideNow(srv, ctx, &pb.CheckSideRequest{
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

	assertNothingRunsBetweenCalls(t, node)

	// The other migration ends: the next round names the slot, and its
	// re-send builds the destination.
	if err := srv.meta.FreeCloneMeta(ctx, testCluster, testDn, otherSp, otherMigr); err != nil {
		t.Fatalf("FreeCloneMeta: %v", err)
	}
	round, resent := workerRound(t, srv, held, "round-1")
	assertNamed(t, "round-1", round, names.clone)
	if resent == nil {
		t.Fatal("round-1 re-sent nothing")
	}
	assertNotNamed(t, "round-1's re-send", resent)
	if !servesThroughClone(t, srv, node, names) {
		t.Error("the re-send did not build the destination")
	}
	round, resent = workerRound(t, srv, held, "round-2")
	assertNotNamed(t, "round-2", round)
	if resent != nil {
		t.Errorf("round-2 re-sent: %v", resent.GetAgentReply())
	}
}

// destinationRows is every row of info the destination rule decides (DN13):
// the destination's two rows, and the dm-linear and the export of each CN,
// which are judged by where the rule says the stacks serve from.
func destinationRows(info *pb.SideInfo) map[string]*pb.ResInfo {
	rows := map[string]*pb.ResInfo{
		"target_info":   info.GetMigrDstInfo().GetTargetInfo(),
		"dm_clone_info": info.GetMigrDstInfo().GetDmCloneInfo(),
	}
	for _, cnId := range []uint64{testCn0, testCn1} {
		rows[fmt.Sprintf("cn %d dm_linear", cnId)] =
			info.GetCnIdToDmLinear()[cnId]
		rows[fmt.Sprintf("cn %d nvmeof", cnId)] = info.GetCnIdToNvmeof()[cnId]
	}
	return rows
}

// checkRoundInfo runs the test side's read and then one CheckSide round of
// it at revision, and returns the info the round carries: what the read found
// (SH29).
func checkRoundInfo(
	t *testing.T,
	srv *DnAgentServer,
	revision uint64,
) *pb.SideInfo {
	t.Helper()
	reply, _ := checkSideNow(srv, context.Background(),
		&pb.CheckSideRequest{
			ClusterId: testCluster, DnId: testDn,
			SidePointer: sidePtr(testSide), Revision: revision,
			ShowInfo: true,
		}, nil)
	if got := reply.GetAgentReply().GetCode(); got ==
		common.ReplyCodeUnknownObject {
		t.Fatalf("CheckSide does not know the side: %v",
			reply.GetAgentReply())
	}
	return reply.GetSideInfo()
}

// One rule decides a migration destination's rows on both channels (DN13):
// for the same state, a CheckSide round reports the status a converge does
// for the destination's two rows, and judges every CN's dm-linear and export
// by the same answer to where the stacks serve from, so no row, and no epoch
// on it, changes between the two. Each case pins the statuses too, so a rule
// both channels got wrong alike fails as well. The status line rides in
// dm_clone_info whenever the round read it — whenever the dm-clone is there,
// whatever the row's status — and parses there. Both channels' codes agree
// too, and each names the destination only while a converge would act on it
// (DN16): not while the walk finds a controller with no namespace device, or
// one whose paths are not live.
func TestADestinationReadsTheSameOnBothChannels(t *testing.T) {
	names := newMigrDstNames()
	const (
		ok      = pb.ResStatus_RES_STATUS_OK
		errored = pb.ResStatus_RES_STATUS_ERROR
		missing = pb.ResStatus_RES_STATUS_MISSING
	)
	// serving builds the destination and leaves it serving through its
	// dm-clone at revision 2; the case's converge comes at revision 3.
	serving := func(t *testing.T, srv *DnAgentServer, node *fakeNode) {
		t.Helper()
		syncupBoth(t, srv, 1, testSide)
		if _, err := srv.SyncupSide(context.Background(),
			migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if !servesThroughClone(t, srv, node, names) {
			t.Fatal("fixture is wrong: the destination does not serve " +
				"through its dm-clone")
		}
	}
	for _, tc := range []struct {
		name string
		// fixture brings the node to the case's state and returns the
		// revision of the converge that reads it.
		fixture func(t *testing.T, srv *DnAgentServer,
			node *fakeNode) uint64
		target, clone pb.ResStatus
		// cloneThere says whether the dm-clone exists, so the round reads
		// its status line.
		cloneThere bool
		// rows pins a per-CN row to a status other than OK, by its name in
		// destinationRows.
		rows map[string]pb.ResStatus
		// named says both channels answer the leftover code, naming the
		// dm-clone row or the target's; code zero otherwise.
		named bool
	}{
		{"all up", func(t *testing.T, srv *DnAgentServer,
			node *fakeNode) uint64 {
			serving(t, srv, node)
			return 3
		}, ok, ok, true, nil, false},
		// The walk finds no controller and the connect is refused: the
		// target reads ERROR on both channels, not MISSING on a round.
		{"never connected", func(t *testing.T, srv *DnAgentServer,
			node *fakeNode) uint64 {
			syncupBoth(t, srv, 1, testSide)
			withDnClock(srv, node)
			setFailAlways(node, "nvme connect", "connect refused")
			return 2
		}, errored, missing, false, nil, true},
		// The source's controllers go under a serving dm-clone and the
		// reconnect is refused: the stacks go to their dm-errors, and a
		// round judges them there, not by the dm-clone that is still there.
		{"the source lost its controllers", func(t *testing.T,
			srv *DnAgentServer, node *fakeNode) uint64 {
			serveThenDropSourceCtrls(t, srv, node, names, false)
			withDnClock(srv, node)
			setFailAlways(node, "nvme connect",
				"Connect Invalid Data Parameter")
			return 3
		}, errored, missing, true, nil, true},
		// A controller whose namespace never comes: no source to read from.
		{"a controller with no namespace", func(t *testing.T,
			srv *DnAgentServer, node *fakeNode) uint64 {
			syncupBoth(t, srv, 1, testSide)
			withDnClock(srv, node)
			node.mu.Lock()
			node.nsMisses[srv.nf.MigrSrcNqn(
				testCluster, testSrcDn, testSp, testMigrId)] = 1000
			node.mu.Unlock()
			return 2
		}, errored, missing, false, nil, false},
		// The source drops its namespace under a serving dm-clone while its
		// controller stays live: the converge stops at the walk, and a read
		// reads the dm-clone's status line too, yet neither names the
		// destination, since a converge connects nowhere beside a
		// controller.
		{"the source's namespace gone under a serving dm-clone", func(
			t *testing.T, srv *DnAgentServer, node *fakeNode) uint64 {
			serving(t, srv, node)
			nqn := srv.nf.MigrSrcNqn(
				testCluster, testSrcDn, testSp, testMigrId)
			node.mu.Lock()
			conn := node.conns[nqn]
			delete(node.dirs,
				"/sys/class/nvme-subsystem/"+conn.subsys+"/"+conn.device)
			node.mu.Unlock()
			state, err := srv.host.ListSubsys(context.Background(), nqn)
			if err != nil || !state.HasCtrl() || !state.Live ||
				state.DevicePath != "" {
				t.Fatalf("fixture is wrong: the walk reads %v, %v; want a "+
					"live controller with no namespace device", state, err)
			}
			return 3
		}, errored, missing, true, nil, false},
		// The source answers but is reconnecting: not live.
		{"a source reconnecting", func(t *testing.T, srv *DnAgentServer,
			node *fakeNode) uint64 {
			serving(t, srv, node)
			node.setConnState(srv.nf.MigrSrcNqn(
				testCluster, testSrcDn, testSp, testMigrId), "connecting")
			return 3
		}, errored, ok, true, nil, false},
		// Hydration turned off behind the agent's back, and the enable
		// refused: the status line shows it on both channels.
		{"hydration off", func(t *testing.T, srv *DnAgentServer,
			node *fakeNode) uint64 {
			serving(t, srv, node)
			node.mu.Lock()
			node.dms[names.clone].noHydration = true
			node.failCmdAlways["dmsetup message "+names.clone] =
				"device busy"
			node.mu.Unlock()
			return 3
		}, ok, errored, true, nil, true},
		// DN13 step (2) does not hold under a serving dm-clone: the
		// wrapper's table read is killed on both channels.
		{"the slot's wrapper not read", func(t *testing.T,
			srv *DnAgentServer, node *fakeNode) uint64 {
			serving(t, srv, node)
			setHook(node, node.killCmdNoEffectAlways,
				"dmsetup table "+names.meta)
			return 3
		}, missing, errored, true, nil, true},
		// DN13 step (4) does not hold: the dm-clone's table read is killed.
		{"the dm-clone's table not read", func(t *testing.T,
			srv *DnAgentServer, node *fakeNode) uint64 {
			serving(t, srv, node)
			setHook(node, node.killCmdNoEffectAlways,
				"dmsetup table "+names.clone)
			return 3
		}, ok, errored, true, nil, true},
		// A stopped step after a pass took the stacks off a dm-clone that
		// is still there: they stay on their dm-errors, and a round judges
		// them there, not by the dm-clone's presence.
		{"a stopped step after the stacks left the dm-clone", func(
			t *testing.T, srv *DnAgentServer, node *fakeNode) uint64 {
			serveThenDropSourceCtrls(t, srv, node, names, false)
			withDnClock(srv, node)
			setFailAlways(node, "nvme connect",
				"Connect Invalid Data Parameter")
			if _, err := srv.SyncupSide(context.Background(),
				migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if !sitsOnDmError(t, srv, node, names) ||
				!dmPresent(node, names.clone) {
				t.Fatal("fixture is wrong: the stacks are not on their " +
					"dm-errors beside the dm-clone")
			}
			setHook(node, node.killCmdNoEffectAlways,
				"dmsetup table "+names.meta)
			return 4
		}, missing, errored, true, nil, true},
		// A stopped step whose read of where the stacks are does not
		// answer, over a dm-clone they serve through: they stay on it, and
		// both channels judge them there. The primary's own group read is
		// the one that does not answer, so its export reads ERROR on both.
		{"a stopped step that cannot read where the stacks are", func(
			t *testing.T, srv *DnAgentServer, node *fakeNode) uint64 {
			serving(t, srv, node)
			setHook(node, node.killCmdNoEffectAlways,
				"dmsetup table "+names.meta)
			setHook(node, node.killReadAlways,
				srv.nvmet.NsPath(names.nqn, sideNsid)+"/ana_grpid")
			return 3
		}, missing, errored, true, map[string]pb.ResStatus{
			fmt.Sprintf("cn %d nvmeof", testCn0): errored,
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			revision := tc.fixture(t, srv, node)
			reply, err := srv.SyncupSide(context.Background(),
				migrDstReq(revision, pb.SpLevel_SP_LEVEL_READWRITE))
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			round, _ := checkSideNow(srv, context.Background(),
				&pb.CheckSideRequest{
					ClusterId: testCluster, DnId: testDn,
					SidePointer: sidePtr(testSide), Revision: revision,
					ShowInfo: true,
				}, nil)
			for label, got := range map[string]interface {
				GetAgentReply() *pb.AgentReply
			}{"the converge": reply, "the round": round} {
				if tc.named {
					assertNamed(t, label, got, names.clone)
				} else {
					assertNotNamed(t, label, got)
				}
			}
			converged := destinationRows(reply.GetSideInfo())
			checked := destinationRows(round.GetSideInfo())
			for name, conv := range converged {
				want := ok
				switch name {
				case "target_info":
					want = tc.target
				case "dm_clone_info":
					want = tc.clone
				}
				if pinned, found := tc.rows[name]; found {
					want = pinned
				}
				round := checked[name]
				if conv.GetStatus() != want {
					t.Errorf("%s: the converge reports %v/%q, want %v",
						name, conv.GetStatus(), conv.GetDetails(), want)
				}
				if round.GetStatus() != conv.GetStatus() ||
					round.GetEpoch() != conv.GetEpoch() {
					t.Errorf("%s: the round reports %v/%q at epoch %d, "+
						"the converge %v/%q at epoch %d", name,
						round.GetStatus(), round.GetDetails(),
						round.GetEpoch(), conv.GetStatus(),
						conv.GetDetails(), conv.GetEpoch())
				}
			}
			details := checked["dm_clone_info"].GetDetails()
			if _, parsed := common.ParseCloneStatus(
				details); parsed != tc.cloneThere {
				t.Errorf("the round's dm_clone_info details %q: a status "+
					"line parses = %v, want %v", details, parsed,
					tc.cloneThere)
			}
		})
	}
}

// TestMigrDstRowRule pins the destination rule itself (DN13), on readings
// built by hand: which step's fault each row reports, in step order, the
// status line kept in dm_clone_info whenever the pass read it, whatever the
// status, which of the two rows the side's verdict names (nameMigrDstRows:
// only while a converge would act, DN16), where the stacks serve from, and
// the rows' reason, which only the wait for the cutover gives (DN18). A
// destination whose dm-clone probe does not answer tells the rule's own
// answers from the one read from the stacks (migrDstStopped), which is
// unread there.
func TestMigrDstRowRule(t *testing.T) {
	srv, node := newTestServer(t)
	names := newMigrDstNames()
	setHook(node, node.killCmdNoEffectAlways, "-o attr "+names.clone)
	plan := newSidePlan(srv.nf,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE), testExtentSize)
	// raw is the line of a dm-clone hydrating with the knobs migrDstReq
	// sets; unhydrated lacks the enable, and otherThreshold and otherBatch
	// each show one knob other than the conf's.
	const (
		raw = "0 51200 clone 8 1/1024 2048 0/25 0 1 no_discard_passdown " +
			"4 hydration_threshold 1 hydration_batch_size 1"
		unhydrated = "0 51200 clone 8 1/1024 2048 0/25 0 2 no_hydration " +
			"no_discard_passdown 4 hydration_threshold 1 " +
			"hydration_batch_size 1"
		otherThreshold = "0 51200 clone 8 1/1024 2048 0/25 0 1 " +
			"no_discard_passdown 4 hydration_threshold 5 " +
			"hydration_batch_size 1"
		otherBatch = "0 51200 clone 8 1/1024 2048 0/25 0 1 " +
			"no_discard_passdown 4 hydration_threshold 1 " +
			"hydration_batch_size 3"
	)
	lines := map[string]bool{
		raw: true, unhydrated: true, otherThreshold: true, otherBatch: true}
	live := &agent.SubsysState{
		Found: true, Live: true, States: []string{"live"},
		DevicePath: "/dev/nvme9n1",
		Paths:      []agent.PathState{{Name: "nvme9", State: "live"}},
	}
	noNs := &agent.SubsysState{
		Found: true, Live: true, States: []string{"live"},
		Paths: []agent.PathState{{Name: "nvme9", State: "live"}},
	}
	connecting := &agent.SubsysState{
		Found: true, States: []string{"connecting"},
		DevicePath: "/dev/nvme9n1",
		Paths:      []agent.PathState{{Name: "nvme9", State: "connecting"}},
	}
	connectingNoNs := &agent.SubsysState{
		Found: true, States: []string{"connecting"},
		Paths: []agent.PathState{{Name: "nvme9", State: "connecting"}},
	}
	noRun := fmt.Errorf("allocating 2 clone-metadata units: %w",
		&noFreeRunError{want: 2, total: 1})
	walk := fmt.Errorf("walk killed")
	const (
		ok          = pb.ResStatus_RES_STATUS_OK
		errored     = pb.ResStatus_RES_STATUS_ERROR
		missing     = pb.ResStatus_RES_STATUS_MISSING
		provisioned = pb.ResStatus_RES_STATUS_PROVISIONING
	)
	// reasons names the one case whose rows carry a reason.
	reasons := map[string]pb.ResReason{
		"awaiting the cutover": pb.ResReason_RES_REASON_AWAITING_CUTOVER,
	}
	// named is which of the two rows the side's verdict names.
	type named int
	const (
		none named = iota
		cloneOnly
		both
	)
	for _, tc := range []struct {
		name          string
		seen          migrDstSeen
		target        pb.ResStatus
		targetDetails string
		clone         pb.ResStatus
		cloneDetails  string
		named         named
		serving       migrCloneState
	}{
		{"no free run", migrDstSeen{metaErr: noRun},
			provisioned, metaRoomDetails(noRun),
			provisioned, metaRoomDetails(noRun), none, migrCloneUnread},
		{"no free run with the status read", migrDstSeen{metaErr: noRun,
			raw: raw, statusRead: true},
			provisioned, metaRoomDetails(noRun),
			provisioned, metaRoomDetails(noRun) + "\n" + raw, none,
			migrCloneUnread},
		{"the slot refused", migrDstSeen{
			metaErr: fmt.Errorf("wrapper killed"), raw: raw,
			statusRead: true},
			missing, detailsCloneMetaMissing,
			errored, "wrapper killed\n" + raw, both, migrCloneUnread},
		// The wait for the cutover reads where the stacks are, as a step
		// that stops does.
		{"awaiting the cutover", migrDstSeen{awaitingCutover: true},
			provisioned, tagCutoverWait,
			provisioned, tagCutoverWait, none, migrCloneUnread},
		{"the walk unread", migrDstSeen{connErr: walk, connUnread: true},
			errored, "walk killed",
			errored, detailsTargetNotRead + "walk killed", both,
			migrCloneUnread},
		{"the connect refused", migrDstSeen{conn: &agent.SubsysState{},
			connErr: fmt.Errorf("refused")},
			errored, "refused",
			missing, detailsTargetNotConnected, both, migrCloneDown},
		{"no controller", migrDstSeen{conn: &agent.SubsysState{},
			raw: raw, statusRead: true},
			errored, detailsNoCtrl,
			missing, detailsTargetNotConnected + "\n" + raw, both,
			migrCloneDown},
		{"no namespace", migrDstSeen{conn: noNs},
			errored, detailsNoNs,
			missing, detailsTargetNotConnected, none, migrCloneDown},
		{"the dm-clone refused", migrDstSeen{conn: live,
			cloneErr: fmt.Errorf("table killed"), raw: raw,
			statusRead: true},
			ok, "paths: live",
			errored, "table killed\n" + raw, cloneOnly, migrCloneUnread},
		{"the status unread", migrDstSeen{conn: live,
			statusErr: fmt.Errorf("status killed")},
			ok, "paths: live",
			errored, "status killed", cloneOnly, migrCloneUnread},
		{"hydration off", migrDstSeen{conn: live, raw: unhydrated,
			statusRead: true},
			ok, "paths: live",
			errored, "hydration not converged: enable_hydration\n" +
				unhydrated, cloneOnly, migrCloneUp},
		{"another hydration threshold", migrDstSeen{conn: live,
			raw: otherThreshold, statusRead: true},
			ok, "paths: live",
			errored, "hydration not converged: hydration_threshold 1\n" +
				otherThreshold, cloneOnly, migrCloneUp},
		{"another hydration batch size", migrDstSeen{conn: live,
			raw: otherBatch, statusRead: true},
			ok, "paths: live",
			errored, "hydration not converged: hydration_batch_size 1\n" +
				otherBatch, cloneOnly, migrCloneUp},
		{"the enable refused", migrDstSeen{conn: live, raw: unhydrated,
			statusRead: true, hydrationErr: fmt.Errorf("busy")},
			ok, "paths: live",
			errored, "busy\n" + unhydrated, cloneOnly, migrCloneUp},
		{"a refused message the line shows applied", migrDstSeen{
			conn: live, raw: raw, statusRead: true,
			hydrationErr: fmt.Errorf("busy")},
			ok, "paths: live",
			ok, raw, none, migrCloneUp},
		{"an unparsable line", migrDstSeen{conn: live, raw: "0 8 linear",
			statusRead: true},
			ok, "paths: live",
			errored, `unparsable dm-clone status "0 8 linear"` +
				"\n0 8 linear", cloneOnly, migrCloneUp},
		{"a source reconnecting", migrDstSeen{conn: connecting, raw: raw,
			statusRead: true},
			errored, "paths: connecting",
			ok, raw, none, migrCloneUp},
		// A source reconnecting under a dm-clone a converge would rebuild:
		// the dm-clone row is the dm-clone's own, and a converge acts on it.
		{"a source reconnecting under a refused dm-clone", migrDstSeen{
			conn: connecting, cloneErr: fmt.Errorf("table killed")},
			errored, "paths: connecting",
			errored, "table killed", cloneOnly, migrCloneUnread},
		{"no namespace on a reconnecting controller", migrDstSeen{
			conn: connectingNoNs},
			errored, detailsNoNs,
			missing, detailsTargetNotConnected, none, migrCloneDown},
		// A read that finds the dm-clone standing reads its status line
		// whatever stopped before it, and the row carries the line after
		// the walk's answer: the verdict still names nothing.
		{"no namespace with the status read", migrDstSeen{conn: noNs,
			raw: raw, statusRead: true},
			errored, detailsNoNs,
			missing, detailsTargetNotConnected + "\n" + raw, none,
			migrCloneDown},
		{"no namespace on a reconnecting controller with the status read",
			migrDstSeen{conn: connectingNoNs, raw: raw, statusRead: true},
			errored, detailsNoNs,
			missing, detailsTargetNotConnected + "\n" + raw, none,
			migrCloneDown},
		{"all held", migrDstSeen{conn: live, raw: raw, statusRead: true},
			ok, "paths: live",
			ok, raw, none, migrCloneUp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := tc.seen
			if status, details := seen.targetRow(); status != tc.target ||
				details != tc.targetDetails {
				t.Errorf("target_info = %v/%q, want %v/%q", status, details,
					tc.target, tc.targetDetails)
			}
			status, details := seen.cloneRow(plan)
			if status != tc.clone || details != tc.cloneDetails {
				t.Errorf("dm_clone_info = %v/%q, want %v/%q", status,
					details, tc.clone, tc.cloneDetails)
			}
			wantLine := seen.statusRead && lines[seen.raw]
			if _, parsed := common.ParseCloneStatus(
				details); parsed != wantLine {
				t.Errorf("dm_clone_info %q: a status line parses = %v, "+
					"want %v", details, parsed, wantLine)
			}
			res := &agent.SweepResult{}
			srv.nameMigrDstRows(context.Background(), plan,
				&pb.SideInfo_MigrDstInfo{
					TargetInfo: &pb.ResInfo{ResName: "target",
						Status: tc.target, Details: tc.targetDetails},
					DmCloneInfo: &pb.ResInfo{ResName: "clone",
						Status: tc.clone, Details: tc.cloneDetails},
				}, res)
			verdict := res.Reply().GetDetails()
			got := none
			switch {
			case strings.Contains(verdict, "target: ") &&
				strings.Contains(verdict, "clone: "):
				got = both
			case strings.Contains(verdict, "target: "):
				t.Errorf("the verdict names the target alone: %q", verdict)
			case strings.Contains(verdict, "clone: "):
				got = cloneOnly
			}
			if got != tc.named {
				t.Errorf("named = %v, want %v: %q", got, tc.named, verdict)
			}
			if got, want := seen.reason(), reasons[tc.name]; got != want {
				t.Errorf("reason = %v, want %v", got, want)
			}
			if got := srv.migrDstServing(context.Background(), plan,
				&seen); got.state != tc.serving {
				t.Errorf("serving = %v, want %v", got.state, tc.serving)
			}
		})
	}
}

// A dm-clone whose hydration knob differs from what its conf sets — after
// the worker's default for a knob left at zero changed, or a message sent
// outside the agent — reads dm_clone_info ERROR on a check round (DN13),
// which the round names, so the worker re-sends the request it holds; the
// converge that brings sends that knob's message, under the round's trace
// id, after which both channels read the row OK.
func TestADestinationConvergesItsHydrationKnobs(t *testing.T) {
	names := newMigrDstNames()
	for _, tc := range []struct {
		name string
		// drift sets the knob on the fake's dm-clone.
		drift func(dm *fakeDm)
		msg   string
	}{
		{"the hydration threshold",
			func(dm *fakeDm) { dm.threshold = 5 }, "hydration_threshold 1"},
		{"the hydration batch size",
			func(dm *fakeDm) { dm.batchSize = 3 }, "hydration_batch_size 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			held := migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)
			if _, err := srv.SyncupSide(ctx, held); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if !servesThroughClone(t, srv, node, names) {
				t.Fatal("fixture: the side does not serve through the " +
					"dm-clone")
			}
			node.mu.Lock()
			tc.drift(node.dms[names.clone])
			node.mu.Unlock()

			node.Reset()
			round, reply := workerRound(t, srv, held, "round-1")
			row := round.GetSideInfo().GetMigrDstInfo().GetDmCloneInfo()
			if want := "hydration not converged: " + tc.msg; row.GetStatus() !=
				pb.ResStatus_RES_STATUS_ERROR ||
				!strings.HasPrefix(row.GetDetails(), want+"\n") {
				t.Errorf("check round: dm_clone_info = %v/%q, want ERROR "+
					"led by %q", row.GetStatus(), row.GetDetails(), want)
			}
			assertNamed(t, "round-1", round, names.clone)
			if reply == nil {
				t.Fatal("round-1 re-sent nothing")
			}
			if msg := "cmd dmsetup message " + names.clone + " 0 " +
				tc.msg; !tracedCall(node, "round-1", msg) {
				t.Errorf("the re-sent converge never sent %q; messages: %q",
					msg, node.callsMatching("dmsetup message"))
			}
			for label, info := range map[string]*pb.SideInfo{
				"converge": reply.GetSideInfo(), "check round": checkRoundInfo(
					t, srv, 2)} {
				row := info.GetMigrDstInfo().GetDmCloneInfo()
				if row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
					t.Errorf("%s after the message: dm_clone_info = %v/%q, "+
						"want OK", label, row.GetStatus(), row.GetDetails())
				}
			}
		})
	}
}

// A check round's read reads DN13 steps (2) and (4) as the converge's steps
// leave them (migrMetaRead, migrCloneRead): each read holds exactly where its
// step holds with nothing to do. Over each fault below the read refuses and
// the step changes the node; once the step has run, the read holds and the
// step changes nothing.
func TestAStepReadHoldsWhereItsStepHasNothingToDo(t *testing.T) {
	names := newMigrDstNames()
	// shiftArg moves the numeric table argument at index i of a single-line
	// table by delta.
	shiftArg := func(table string, i int, delta uint64) string {
		fields := strings.Fields(table)
		value, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			panic(err)
		}
		fields[i] = strconv.FormatUint(value+delta, 10)
		return strings.Join(fields, " ")
	}
	for _, tc := range []struct {
		name string
		dev  string
		// fault changes the device, under the fake's lock; a nil one
		// removes it.
		fault func(dm *fakeDm)
		// cloneStep says the fault is step (4)'s, the dm-clone's.
		cloneStep bool
	}{
		{"the wrapper suspended", names.meta,
			func(dm *fakeDm) { dm.suspended = true }, false},
		{"the wrapper read-only", names.meta,
			func(dm *fakeDm) { dm.readOnly = true }, false},
		{"the wrapper off its run", names.meta, func(dm *fakeDm) {
			// 0 <sectors> linear <disk> <offset>
			dm.table = shiftArg(dm.table, 4, 8)
		}, false},
		{"the wrapper gone", names.meta, nil, false},
		{"the dm-clone suspended", names.clone,
			func(dm *fakeDm) { dm.suspended = true }, true},
		{"the dm-clone on another region size", names.clone,
			func(dm *fakeDm) {
				// 0 <sectors> clone <meta> <dest> <source> <region> ...
				dm.table = shiftArg(dm.table, 6, 8)
			}, true},
		{"the dm-clone gone", names.clone, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			req := migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)
			if _, err := srv.SyncupSide(ctx, req); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			plan := newSidePlan(srv.nf, req, testExtentSize)
			src, err := srv.host.ListSubsys(ctx, plan.srcNqnOfDst())
			if err != nil || src.DevicePath == "" {
				t.Fatalf("fixture is wrong: the source reads %v, %v",
					src, err)
			}
			read := func() error {
				if tc.cloneStep {
					return srv.migrCloneRead(ctx, plan, src.DevicePath)
				}
				return srv.migrMetaRead(ctx, plan)
			}
			step := func() error {
				if tc.cloneStep {
					_, err := srv.ensureDmClone(ctx, plan, src.DevicePath)
					return err
				}
				return srv.ensureMigrMeta(ctx, plan)
			}
			holds := func(what string) {
				t.Helper()
				if err := read(); err != nil {
					t.Errorf("%s: the read refuses: %v", what, err)
				}
				node.Reset()
				if err := step(); err != nil {
					t.Errorf("%s: the step fails: %v", what, err)
				}
				if got := node.Mutations(); len(got) != 0 {
					t.Errorf("%s: the step changed the node: %v", what, got)
				}
			}

			holds("built")
			node.mu.Lock()
			if tc.fault == nil {
				delete(node.dms, tc.dev)
			} else {
				tc.fault(node.dms[tc.dev])
			}
			node.mu.Unlock()
			if err := read(); err == nil {
				t.Errorf("the read holds over %s", tc.name)
			}
			node.Reset()
			if err := step(); err != nil {
				t.Fatalf("the step fails over %s: %v", tc.name, err)
			}
			if len(node.Mutations()) == 0 {
				t.Fatalf("fixture is wrong: the step had nothing to do "+
					"over %s", tc.name)
			}
			holds("converged")
		})
	}
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
	// Every refused connect spends the pass's connect budget, on a clock
	// that does not sleep.
	withDnClock(srv, node)
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

// A destination role that ends in a SyncupSide whose sweep could not list the
// dm devices leaves the dm-clone the role no longer wants: that sweep removes
// nothing and skips its pre-steps, and its code says so. Nothing tears the
// role down between the worker's calls. The side's next read lists the
// devices and names the dm-clone as a leftover, and the re-send that brings
// removes it (DN6).
func TestARoleEndedUnderAnUnlistedSweepIsTornDownByTheResend(t *testing.T) {
	names := newMigrDstNames()
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !dmPresent(node, names.clone) {
		t.Fatal("fixture is wrong: want a dm-clone")
	}

	// The role ends in a pass whose sweep cannot list the dm devices.
	setHook(node, node.killCmdNoEffectAlways, "dmsetup ls")
	held := migrDstReq(3, pb.SpLevel_SP_LEVEL_NO_MIGRATION)
	reply, err := srv.SyncupSide(ctx, held)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	present := dmPresent(node, names.clone)
	clearHook(node, node.killCmdNoEffectAlways, "dmsetup ls")
	if !present || reply.GetAgentReply().GetCode() == 0 {
		t.Fatalf("fixture is wrong: after the unlisted pass the dm-clone "+
			"present = %v, reply %v; want it present and a code",
			present, reply.GetAgentReply())
	}
	assertNothingRunsBetweenCalls(t, node)
	if !dmPresent(node, names.clone) {
		t.Fatal("the dm-clone went with no call of the worker's")
	}

	round, resent := workerRound(t, srv, held, "round-1")
	assertNamed(t, "round-1", round, names.clone)
	if resent == nil {
		t.Fatal("round-1 re-sent nothing")
	}
	assertNotNamed(t, "round-1's re-send", resent)
	if dmPresent(node, names.clone) {
		t.Error("the re-send left the dm-clone the role no longer wants")
	}
	round, resent = workerRound(t, srv, held, "round-2")
	assertNotNamed(t, "round-2", round)
	if resent != nil {
		t.Errorf("round-2 re-sent: %v", resent.GetAgentReply())
	}
}
