package dnagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// The wait for the cutover (DN12, DN13, DN18)
// ---------------------------------------------------------------------------

// pendingDstReq is migrDstReq while the migration's cutover is pending.
func pendingDstReq(revision uint64, level pb.SpLevel) *pb.SyncupSideRequest {
	req := migrDstReq(revision, level)
	req.MigrDstConf.CutoverPending = true
	return req
}

// assertDstRows checks the destination's two rows: their status, their
// details and their reason.
func assertDstRows(
	t *testing.T,
	what string,
	info *pb.SideInfo,
	status pb.ResStatus,
	details string,
	reason pb.ResReason,
) {
	t.Helper()
	dst := info.GetMigrDstInfo()
	for name, row := range map[string]*pb.ResInfo{
		"target_info":   dst.GetTargetInfo(),
		"dm_clone_info": dst.GetDmCloneInfo(),
	} {
		if row.GetStatus() != status || row.GetDetails() != details ||
			row.GetReason() != reason {
			t.Errorf("%s %s = %v/%q/%v, want %v/%q/%v", what, name,
				row.GetStatus(), row.GetDetails(), row.GetReason(), status,
				details, reason)
		}
	}
}

// assertSameDstRows checks that a check round reports the destination's two
// rows exactly as the converge did: status, details, reason and epoch.
func assertSameDstRows(t *testing.T, converged, checked *pb.SideInfo) {
	t.Helper()
	conv := destinationRows(converged)
	round := destinationRows(checked)
	for _, name := range []string{"target_info", "dm_clone_info"} {
		c, r := conv[name], round[name]
		if c.GetStatus() != r.GetStatus() ||
			c.GetDetails() != r.GetDetails() ||
			c.GetReason() != r.GetReason() || c.GetEpoch() != r.GetEpoch() {
			t.Errorf("%s: the round reports %v/%q/%v at epoch %d, the "+
				"converge %v/%q/%v at epoch %d", name, r.GetStatus(),
				r.GetDetails(), r.GetReason(), r.GetEpoch(), c.GetStatus(),
				c.GetDetails(), c.GetReason(), c.GetEpoch())
		}
	}
}

// A migration destination provisions while its cutover is pending as any
// destination does, its rows "side provisioning" with no reason. Once the
// worker flips it, it builds DN13 step (2), the metadata wrapper over its
// run, and its per-CN stacks in the shape of step (1) — every dm-linear on
// its dm-error, every namespace inaccessible — and nothing more: no connect
// to the source, no dm-clone, no read of one. Its two rows read PROVISIONING
// "waiting for the cutover" with RES_REASON_AWAITING_CUTOVER, the same on
// both channels, and no DN8 retry is registered, since only the worker's
// cutover ends the wait and it comes with a new request. That request,
// without the flag, runs steps (3) to (5): the destination ends serving
// through its dm-clone.
func TestADestinationWaitsForTheCutover(t *testing.T) {
	srv, node := newTestServer(t)
	// A retry, were one registered, would run many passes in the window
	// the test reads.
	srv.migrRetryInterval = 5 * time.Millisecond
	ctx := context.Background()
	names := newMigrDstNames()
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}

	// Before the flip the gate is closed: no reason yet.
	req := pendingDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)
	req.SideConf.Provisioned = false
	reply, err := srv.SyncupSide(ctx, req)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertDstRows(t, "before the flip", reply.GetSideInfo(),
		pb.ResStatus_RES_STATUS_PROVISIONING, tagProvisioningWait,
		pb.ResReason_RES_REASON_NONE)
	waitZeroed(t, srv, testSide)

	// The flip, with the cutover still pending.
	node.Reset()
	reply, err = srv.SyncupSide(ctx,
		pendingDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if code := reply.GetAgentReply().GetCode(); code != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	// Read before anything else can run: a retry registered by the pass
	// would have run passes of its own by the end of the wait below.
	if migrRetrying(srv) {
		t.Error("a destination waiting for the cutover registered the " +
			"DN8 retry")
	}
	time.Sleep(10 * srv.migrRetryInterval)
	if n := convergePasses(node); n != 1 {
		t.Errorf("%d converge passes ran, want the RPC's alone", n)
	}

	converged := reply.GetSideInfo()
	assertDstRows(t, "the converge", converged,
		pb.ResStatus_RES_STATUS_PROVISIONING, tagCutoverWait,
		pb.ResReason_RES_REASON_AWAITING_CUTOVER)
	if !dmPresent(node, names.meta) {
		t.Error("the metadata wrapper was not built")
	}
	if dmPresent(node, names.clone) {
		t.Error("a destination waiting for the cutover built its dm-clone")
	}
	for _, call := range []string{
		"cmd nvme connect", "cmd dmsetup status " + names.clone,
	} {
		if node.hasCall(call) {
			t.Errorf("a destination waiting for the cutover ran %q", call)
		}
	}
	for _, cnId := range []uint64{testCn0, testCn1} {
		cn := migrDstNamesOf(cnId)
		if !sitsOnDmError(t, srv, node, cn) {
			t.Errorf("cn %d: the dm-linear is not on its dm-error with its "+
				"namespace inaccessible", cnId)
		}
		for name, row := range map[string]*pb.ResInfo{
			"dm_error":  converged.GetCnIdToDmError()[cnId],
			"dm_linear": converged.GetCnIdToDmLinear()[cnId],
			"nvmeof":    converged.GetCnIdToNvmeof()[cnId],
		} {
			if row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
				t.Errorf("cn %d %s = %v/%q, want OK", cnId, name,
					row.GetStatus(), row.GetDetails())
			}
		}
	}

	// The check round reads the same rows, the per-CN ones included.
	node.Reset()
	checked := checkRoundInfo(t, srv, 1)
	assertSameDstRows(t, converged, checked)
	for name, row := range destinationRows(checked) {
		if strings.HasPrefix(name, "cn ") &&
			row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("the round's %s = %v/%q, want OK", name,
				row.GetStatus(), row.GetDetails())
		}
	}
	for _, call := range []string{
		"list /sys/class/nvme-subsystem/", "cmd dmsetup status " + names.clone,
	} {
		if node.hasCall(call) {
			t.Errorf("a round of a destination waiting for the cutover "+
				"ran %q", call)
		}
	}

	// The cutover: the same request without the flag runs the rest.
	node.Reset()
	reply, err = srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	assertOrder(t, node,
		"cmd nvme connect --transport tcp",
		"cmd dmsetup create "+names.clone,
		"cmd dmsetup reload "+names.lin,
		fmt.Sprintf("writedirect %s/subsystems/%s/namespaces/1/ana_grpid=%d",
			agent.NvmetRoot, names.nqn, common.AnaGrpIdOptimized),
	)
	if node.hasCall("cmd dmsetup create " + names.meta) {
		t.Error("the cutover rebuilt the metadata wrapper")
	}
	for name, row := range destinationRows(reply.GetSideInfo()) {
		if row.GetStatus() != pb.ResStatus_RES_STATUS_OK ||
			row.GetReason() != pb.ResReason_RES_REASON_NONE {
			t.Errorf("after the cutover %s = %v/%q/%v, want OK with no "+
				"reason", name, row.GetStatus(), row.GetDetails(),
				row.GetReason())
		}
	}
	if !servesThroughClone(t, srv, node, names) || migrRetrying(srv) {
		t.Error("after the cutover the destination does not serve through " +
			"its dm-clone with no retry registered")
	}
}

// A destination whose DN13 step (2) does not hold while the cutover is
// pending reads as any destination whose step (2) stopped, on both
// channels, and with no reason: the cutover waits for a wrapper that stands.
// The pass registers the DN8 retry to build it, as such a pass always does.
// The pass that builds it waits for the cutover and stops that retry,
// whether an RPC runs it or the retry itself.
func TestADestinationWaitsForItsWrapperBeforeTheCutover(t *testing.T) {
	names := newMigrDstNames()
	// refusedWrapper provisions the side and runs one pending pass whose
	// wrapper create is refused, and checks what it reads.
	refusedWrapper := func(
		t *testing.T,
		srv *DnAgentServer,
		node *fakeNode,
	) {
		t.Helper()
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		node.mu.Lock()
		node.failCmd["dmsetup create "+names.meta] = "refused once"
		node.mu.Unlock()
		node.Reset()
		reply, err := srv.SyncupSide(ctx,
			pendingDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		info := reply.GetSideInfo()
		dst := info.GetMigrDstInfo()
		if got := dst.GetTargetInfo(); got.GetStatus() !=
			pb.ResStatus_RES_STATUS_MISSING ||
			got.GetDetails() != detailsCloneMetaMissing ||
			got.GetReason() != pb.ResReason_RES_REASON_NONE {
			t.Errorf("target_info = %v/%q/%v, want MISSING/%q with no "+
				"reason", got.GetStatus(), got.GetDetails(), got.GetReason(),
				detailsCloneMetaMissing)
		}
		if got := dst.GetDmCloneInfo(); got.GetStatus() !=
			pb.ResStatus_RES_STATUS_ERROR ||
			!strings.Contains(got.GetDetails(), "refused once") ||
			got.GetReason() != pb.ResReason_RES_REASON_NONE {
			t.Errorf("dm_clone_info = %v/%q/%v, want ERROR with the "+
				"refusal and no reason", got.GetStatus(), got.GetDetails(),
				got.GetReason())
		}
		if !sitsOnDmError(t, srv, node, names) {
			t.Error("the primary is not on its dm-error, its namespace " +
				"inaccessible")
		}
		if node.hasCall("cmd nvme connect") {
			t.Error("a pending pass with no wrapper connected to its source")
		}
	}

	t.Run("an RPC builds it", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.migrRetryInterval = time.Hour
		refusedWrapper(t, srv, node)
		if !migrRetrying(srv) {
			t.Fatal("a pass whose step (2) stopped registered no retry")
		}
		// The round reads the slot's wrapper missing, with no reason.
		checked := checkRoundInfo(t, srv, 2)
		for name, row := range map[string]*pb.ResInfo{
			"target_info":   checked.GetMigrDstInfo().GetTargetInfo(),
			"dm_clone_info": checked.GetMigrDstInfo().GetDmCloneInfo(),
		} {
			if row.GetReason() != pb.ResReason_RES_REASON_NONE ||
				row.GetStatus() == pb.ResStatus_RES_STATUS_PROVISIONING {
				t.Errorf("the round's %s = %v/%q/%v, want step (2)'s "+
					"stop with no reason", name, row.GetStatus(),
					row.GetDetails(), row.GetReason())
			}
		}

		reply, err := srv.SyncupSide(context.Background(),
			pendingDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		assertDstRows(t, "the pass that builds the wrapper",
			reply.GetSideInfo(), pb.ResStatus_RES_STATUS_PROVISIONING,
			tagCutoverWait, pb.ResReason_RES_REASON_AWAITING_CUTOVER)
		if !dmPresent(node, names.meta) {
			t.Error("the wrapper was not built")
		}
		if migrRetrying(srv) {
			t.Error("the retry outlived the pass that left the " +
				"destination waiting for the cutover")
		}
		assertSameDstRows(t, reply.GetSideInfo(), checkRoundInfo(t, srv, 2))
	})

	t.Run("the retry's own pass builds it", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.migrRetryInterval = 5 * time.Millisecond
		refusedWrapper(t, srv, node)
		waitUntil(t, "the retry to build the wrapper and stop", func() bool {
			return dmPresent(node, names.meta) && !migrRetrying(srv)
		})
		if n := convergePasses(node); n < 2 {
			t.Errorf("%d converge passes ran, want the RPC's and the "+
				"retry's", n)
		}
		checked := checkRoundInfo(t, srv, 2)
		assertDstRows(t, "the round after the retry", checked,
			pb.ResStatus_RES_STATUS_PROVISIONING, tagCutoverWait,
			pb.ResReason_RES_REASON_AWAITING_CUTOVER)
		if !sitsOnDmError(t, srv, node, names) {
			t.Error("the primary is not on its dm-error, its namespace " +
				"inaccessible")
		}
		if node.hasCall("cmd nvme connect") || dmPresent(node, names.clone) {
			t.Error("the retry's pass went past step (2) while the " +
				"cutover is pending")
		}
	})
}

// A destination that waits for the cutover still builds the primary's
// per-CN stack, on its dm-error with its namespace inaccessible, and the
// cutover waits for the primary's live path through that stack's export
// (dnv-worker.md RW18). So when a step of that stack fails, the pass keeps
// the DN8 retry, as a pass whose step (5) failed does: the worker re-sends a
// SyncupSide on a revision or a reply code, never on a row, and nothing else
// would build the stack again. So does a pass that cannot read where the
// stacks go, which leaves them as they are, as a step that stops does. The
// retry's pass that builds the stack stops the retry, and still goes no
// further than step (2).
func TestAWaitingDestinationRetriesAPrimaryStackThatFailed(t *testing.T) {
	names := newMigrDstNames()
	for _, tc := range []struct {
		name string
		// failed is the command that fails once, in the flip's pass, or
		// is killed once when killed is set: a probe that did not answer.
		failed string
		killed bool
		// rerun is a command only a converge runs, and runs is how many
		// times it runs by the end: the RPC's pass and the retry's.
		rerun string
		runs  int
		// row is the primary's row the failure leaves not OK.
		row func(*pb.SideInfo) *pb.ResInfo
	}{
		{
			name:   "the export's port link",
			failed: "cmd ln -s " + agent.NvmetRoot + "/subsystems/" + names.nqn,
			rerun:  "cmd ln -s " + agent.NvmetRoot + "/subsystems/" + names.nqn,
			runs:   2,
			row: func(info *pb.SideInfo) *pb.ResInfo {
				return info.GetCnIdToNvmeof()[testCn0]
			},
		},
		{
			name:   "the dm-linear",
			failed: "cmd dmsetup create " + names.lin,
			rerun:  "cmd dmsetup create " + names.lin,
			runs:   2,
			row: func(info *pb.SideInfo) *pb.ResInfo {
				return info.GetCnIdToDmLinear()[testCn0]
			},
		},
		{
			// The flip's pass builds no stack: the retry's builds it.
			name:   "where the stacks go, unread",
			failed: "-o attr " + names.clone,
			killed: true,
			rerun:  "cmd dmsetup create " + names.lin,
			runs:   1,
			row: func(info *pb.SideInfo) *pb.ResInfo {
				return info.GetCnIdToDmLinear()[testCn0]
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.migrRetryInterval = 5 * time.Millisecond
			ctx := context.Background()
			if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			req := pendingDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE)
			req.SideConf.Provisioned = false
			if _, err := srv.SyncupSide(ctx, req); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			waitZeroed(t, srv, testSide)

			// The flip, with the cutover still pending: the first pass that
			// builds the per-CN stacks.
			if tc.killed {
				setHook(node, node.killCmdNoEffect, tc.failed)
			} else {
				node.mu.Lock()
				node.failCmd[tc.failed] = "refused once"
				node.mu.Unlock()
			}
			node.Reset()
			reply, err := srv.SyncupSide(ctx,
				pendingDstReq(1, pb.SpLevel_SP_LEVEL_READWRITE))
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if code := reply.GetAgentReply().GetCode(); code != 0 {
				t.Fatalf("rejected: %v", reply.GetAgentReply())
			}
			if row := tc.row(reply.GetSideInfo()); row.GetStatus() ==
				pb.ResStatus_RES_STATUS_OK {
				t.Fatalf("the primary's row read OK over the refusal: %q",
					row.GetDetails())
			}
			assertDstRows(t, "the pass", reply.GetSideInfo(),
				pb.ResStatus_RES_STATUS_PROVISIONING, tagCutoverWait,
				pb.ResReason_RES_REASON_AWAITING_CUTOVER)

			var checked *pb.SideInfo
			waitUntil(t, "the retry to build the primary's stack and stop",
				func() bool {
					checked = checkRoundInfo(t, srv, 1)
					return tc.row(checked).GetStatus() ==
						pb.ResStatus_RES_STATUS_OK && !migrRetrying(srv)
				})
			// The read before each round lists the dm devices as a
			// converge does, so a command only a converge runs counts the
			// passes.
			if n := len(node.callsMatching(tc.rerun)); n < tc.runs {
				t.Errorf("%q ran %d times, want %d at least: the retry "+
					"ran no pass", tc.rerun, n, tc.runs)
			}
			for name, row := range destinationRows(checked) {
				if strings.HasPrefix(name, "cn ") &&
					row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
					t.Errorf("the round's %s = %v/%q, want OK", name,
						row.GetStatus(), row.GetDetails())
				}
			}
			assertDstRows(t, "the round after the retry", checked,
				pb.ResStatus_RES_STATUS_PROVISIONING, tagCutoverWait,
				pb.ResReason_RES_REASON_AWAITING_CUTOVER)
			if !sitsOnDmError(t, srv, node, names) {
				t.Error("the primary is not on its dm-error, its namespace " +
					"inaccessible")
			}
			if node.hasCall("cmd nvme connect") || dmPresent(node, names.clone) {
				t.Error("the retry's pass went past step (2) while the " +
					"cutover is pending")
			}
		})
	}
}

// The worker never sends cutover_pending again once its cutover cleared it, so
// a destination converges a pending request over a dm-clone that serves only
// when that request is a stale one: a restart's re-run of a stored request
// whose successor, the one that cleared the flag, was converged after a save
// that failed (dnagent.md SH5). Such a pass reads where the per-CN stacks are,
// as a step that stops does, so the primary keeps serving through the dm-clone
// until the worker sends the newer request again: reloading it onto its
// dm-error under a live dm-clone would fail host IO. The check round's read
// reads the per-CN rows by the same answer, and the pass leaves no retry,
// since its primary's stack is where the pass put it.
func TestAStalePendingRequestKeepsAServingClone(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	names := newMigrDstNames()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		pendingDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if !servesThroughClone(t, srv, node, names) {
		t.Fatal("setup: the destination does not serve through its dm-clone")
	}
	// The save of revision 3 is lost: the stored file is the pending
	// request before it.
	stale := pendingDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)
	path := srv.nf.LocalSidePath(stale.GetClusterId(), stale.GetDnId(),
		stale.GetSidePointer().GetSpId(), stale.GetSidePointer().GetSideId())
	if err := srv.store.Save(ctx, path, stale); err != nil {
		t.Fatalf("Save: %v", err)
	}

	node.Reset()
	restarted := startTestServer(t, node)
	if !servesThroughClone(t, restarted, node, names) {
		t.Error("the restart took the primary off its serving dm-clone")
	}
	for _, call := range []string{
		"cmd dmsetup reload " + names.lin,
		fmt.Sprintf("writedirect %s/subsystems/%s/namespaces/1/ana_grpid=%d",
			agent.NvmetRoot, names.nqn, common.AnaGrpIdInaccessible),
	} {
		if node.hasCall(call) {
			t.Errorf("the restart ran %q under a serving dm-clone", call)
		}
	}
	if migrRetrying(restarted) {
		t.Error("the restart registered the DN8 retry over a primary " +
			"stack that serves")
	}
	checked := checkRoundInfo(t, restarted, 2)
	assertDstRows(t, "the round", checked,
		pb.ResStatus_RES_STATUS_PROVISIONING, tagCutoverWait,
		pb.ResReason_RES_REASON_AWAITING_CUTOVER)
	for name, row := range destinationRows(checked) {
		if strings.HasPrefix(name, "cn ") &&
			row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("the round's %s = %v/%q, want OK", name,
				row.GetStatus(), row.GetDetails())
		}
	}
}

// The src side of a migration whose cutover is pending serves exactly as
// with no migr_src_conf, as with any dst_provisioned = false (DN12); only its
// migr_src_info rows differ, which read PROVISIONING "waiting for the
// cutover" in place of "side provisioning", with no reason, the same on both
// channels.
func TestASourceWaitsForTheCutover(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	srcName := nf.DnMigrSrcName(testCluster, testDn, testSp, testMigrId)

	srcRows := func(info *pb.SideInfo) map[string]*pb.ResInfo {
		return map[string]*pb.ResInfo{
			"dm_linear": info.GetMigrSrcInfo().GetDmLinearInfo(),
			"nvmeof":    info.GetMigrSrcInfo().GetNvmeofInfo(),
		}
	}
	for _, tc := range []struct {
		name    string
		pending bool
		details string
	}{
		{"cutover pending", true, tagCutoverWait},
		{"destination provisioning", false, tagProvisioningWait},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := migrSrcReq(2)
			req.MigrSrcConf.DstProvisioned = false
			req.MigrSrcConf.CutoverPending = tc.pending
			node.Reset()
			reply, err := srv.SyncupSide(ctx, req)
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			for _, call := range node.Mutations() {
				if strings.HasPrefix(call, "writeproto") {
					continue // SH5: the request itself is re-persisted
				}
				t.Errorf("a deferred migration source mutated the dn: %s",
					call)
			}
			if dmPresent(node, srcName) {
				t.Error("a deferred migration source built its dm-linear")
			}
			checked := checkRoundInfo(t, srv, 2)
			round := srcRows(checked)
			for name, row := range srcRows(reply.GetSideInfo()) {
				if row.GetStatus() != pb.ResStatus_RES_STATUS_PROVISIONING ||
					row.GetDetails() != tc.details ||
					row.GetReason() != pb.ResReason_RES_REASON_NONE {
					t.Errorf("migr_src %s = %v/%q/%v, want "+
						"PROVISIONING/%q with no reason", name,
						row.GetStatus(), row.GetDetails(), row.GetReason(),
						tc.details)
				}
				if got := round[name]; got.GetStatus() != row.GetStatus() ||
					got.GetDetails() != row.GetDetails() ||
					got.GetEpoch() != row.GetEpoch() {
					t.Errorf("migr_src %s: the round reports %v/%q at "+
						"epoch %d, the converge %v/%q at epoch %d", name,
						got.GetStatus(), got.GetDetails(), got.GetEpoch(),
						row.GetStatus(), row.GetDetails(), row.GetEpoch())
				}
			}
			for cnId, row := range reply.GetSideInfo().GetCnIdToNvmeof() {
				if row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
					t.Errorf("cn %d nvmeof = %v/%q, want OK", cnId,
						row.GetStatus(), row.GetDetails())
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// DN13 step (3): the in-pass retry of the connect
// ---------------------------------------------------------------------------

// budgetRetries is how many in-pass retries of a refused connect the budget
// covers, every pause of them fitting in it.
const budgetRetries = int(
	common.DnMigrConnectPassBudget / common.DnMigrConnectRetryPause)

// A connect the source refuses is tried again within the same pass, every
// DnMigrConnectRetryPause while DnMigrConnectPassBudget covers the pause:
// after the cutover the src side links its export to its port only after its
// own first two steps, so the first connect often comes too early. One that
// answers within the budget is absorbed: the pass builds the dm-clone and
// serves through it, and registers no DN8 retry. The wait for the source's
// namespace runs after it unchanged, on a budget of its own, even after a
// connect that took the whole of the connect's.
func TestMigrationDestinationRetriesItsConnectWithinThePass(t *testing.T) {
	names := newMigrDstNames()
	for _, tc := range []struct {
		name     string
		refusals int
		misses   int
	}{
		{"refused twice", 2, 0},
		{"refused twice, the namespace two reads late", 2, 2},
		{"refused until the budget's last pause, the namespace three " +
			"reads late", budgetRetries, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			clock := withDnClock(srv, node)
			srcNqn := srv.nf.MigrSrcNqn(
				testCluster, testSrcDn, testSp, testMigrId)
			node.mu.Lock()
			node.failCmd["nvme connect "] = "failed to write to " +
				"nvme-fabrics device"
			node.failCmdLeft["nvme connect "] = tc.refusals
			node.nsMisses[srcNqn] = tc.misses
			node.mu.Unlock()
			start := clock.Now()

			node.Reset()
			reply, err := srv.SyncupSide(ctx,
				migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE))
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if n := len(node.callsMatching(
				"cmd nvme connect ")); n != tc.refusals+1 {
				t.Errorf("%d connects, want %d refused and one more", n,
					tc.refusals)
			}
			want := time.Duration(tc.refusals)*common.DnMigrConnectRetryPause +
				time.Duration(tc.misses)*common.DnMigrDstNsPause
			if got := clock.Now().Sub(start); got != want {
				t.Errorf("the pass waited %v, want %v", got, want)
			}
			for name, row := range destinationRows(reply.GetSideInfo()) {
				if row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
					t.Errorf("%s = %v/%q, want OK", name, row.GetStatus(),
						row.GetDetails())
				}
			}
			if !servesThroughClone(t, srv, node, names) {
				t.Error("the destination does not serve through its " +
					"dm-clone")
			}
			if migrRetrying(srv) {
				t.Error("a connect the pass's own retry absorbed " +
					"registered the DN8 retry")
			}
		})
	}
}

// cutoverRetries is how many in-pass retries of a refused connect the
// cutover's budget covers, every pause of them fitting in it.
const cutoverRetries = int(
	common.DnMigrCutoverConnectBudget / common.DnMigrConnectRetryPause)

// The converge the cutover brings — a request without cutover_pending while
// the process held one that carried it for the same migration — tries
// its connect for DnMigrCutoverConnectBudget (DN13): the source links its
// export to the port only after its own step 1 and the suspend of its step
// 2, which on a loaded node takes longer than DnMigrConnectPassBudget. A
// source that answers after the short budget's last retry is still caught
// by that pass, which builds the dm-clone, serves through it and registers
// no DN8 retry, so the migration gap ends in the cutover's own pass.
func TestTheCutoverConvergeOutwaitsASlowSource(t *testing.T) {
	srv, node := newTestServer(t)
	srv.migrRetryInterval = time.Hour
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	clock := withDnClock(srv, node)
	names := newMigrDstNames()
	if _, err := srv.SyncupSide(ctx,
		pendingDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	refusals := budgetRetries + 5
	node.mu.Lock()
	node.failCmd["nvme connect "] = "failed to write to nvme-fabrics device"
	node.failCmdLeft["nvme connect "] = refusals
	node.mu.Unlock()

	start := clock.Now()
	node.Reset()
	reply, err := srv.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if n := len(node.callsMatching("cmd nvme connect ")); n != refusals+1 {
		t.Errorf("%d connects, want %d refused and one more", n, refusals)
	}
	want := time.Duration(refusals) * common.DnMigrConnectRetryPause
	if got := clock.Now().Sub(start); got != want {
		t.Errorf("the pass waited %v, want %v", got, want)
	}
	for name, row := range destinationRows(reply.GetSideInfo()) {
		if row.GetStatus() != pb.ResStatus_RES_STATUS_OK {
			t.Errorf("%s = %v/%q, want OK", name, row.GetStatus(),
				row.GetDetails())
		}
	}
	if !servesThroughClone(t, srv, node, names) || migrRetrying(srv) {
		t.Error("the cutover's pass does not serve through its dm-clone " +
			"with no retry registered")
	}
}

// Only the converge the cutover brings waits the longer budget, and only
// once: against a source that never answers it spends
// DnMigrCutoverConnectBudget, while the same request sent again, the next
// SyncupSide and a pass of the DN8 retry each spend DnMigrConnectPassBudget,
// as every other converge does (DN13). The longer wait holds the side's
// locks once per cutover in a process, not on every retry and not on every
// revision.
func TestOnlyTheCutoverConvergeWaitsTheLongerBudget(t *testing.T) {
	srv, node := newTestServer(t)
	// The DN8 retry's own passes would run on the fake clock while a pass
	// is measured; the test runs one by hand instead.
	srv.migrRetryInterval = time.Hour
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	clock := withDnClock(srv, node)
	if _, err := srv.SyncupSide(ctx,
		pendingDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	setFailAlways(node, "nvme connect", "connect refused")
	key := sideKey(testCluster, testDn, testSp, testSide)
	syncup := func(revision uint64) func() {
		return func() {
			if _, err := srv.SyncupSide(ctx, migrDstReq(revision,
				pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
		}
	}
	for _, pass := range []struct {
		name   string
		run    func()
		budget time.Duration
	}{
		{"the cutover's converge", syncup(3),
			common.DnMigrCutoverConnectBudget},
		{"the cutover's request sent again", syncup(3),
			common.DnMigrConnectPassBudget},
		{"the next SyncupSide", syncup(4), common.DnMigrConnectPassBudget},
		{"a pass of the DN8 retry", func() {
			srv.reconvergeSide(ctx, key, srv.getSide(key))
		}, common.DnMigrConnectPassBudget},
	} {
		start := clock.Now()
		node.Reset()
		pass.run()
		if got := clock.Now().Sub(start); got != pass.budget {
			t.Errorf("%s waited %v, want %v", pass.name, got, pass.budget)
		}
		retries := int(pass.budget / common.DnMigrConnectRetryPause)
		if n := len(node.callsMatching(
			"cmd nvme connect ")); n != retries+1 {
			t.Errorf("%s made %d connects, want the first and %d retries",
				pass.name, n, retries)
		}
	}
	if !migrRetrying(srv) {
		t.Error("no DN8 retry is registered for a source that never answers")
	}
}

// A converge keeps DnMigrConnectPassBudget unless the request held before it
// carried cutover_pending for the same migration: on a side that held no
// migration, on a migration whose requests never carried the flag
// (architecture.md, Upgrade in lockstep), and after a flag of another
// migration, nothing tells the pass that the cutover brought it (DN13).
func TestAConvergeWithNoFlagClearedKeepsTheShortBudget(t *testing.T) {
	otherPending := pendingDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)
	otherPending.MigrDstConf.MigrId = testMigrId + 1
	for _, tc := range []struct {
		name string
		held *pb.SyncupSideRequest
	}{
		{"a side that held no migration", nil},
		{"a migration whose requests never carried the flag",
			migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)},
		{"a flag of another migration", otherPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.migrRetryInterval = time.Hour
			ctx := context.Background()
			syncupBoth(t, srv, 1, testSide)
			clock := withDnClock(srv, node)
			setFailAlways(node, "nvme connect", "connect refused")
			if tc.held != nil {
				if _, err := srv.SyncupSide(ctx, tc.held); err != nil {
					t.Fatalf("SyncupSide: %v", err)
				}
			}

			start := clock.Now()
			node.Reset()
			if _, err := srv.SyncupSide(ctx,
				migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if got := clock.Now().Sub(start); got !=
				common.DnMigrConnectPassBudget {
				t.Errorf("the pass waited %v, want %v", got,
					common.DnMigrConnectPassBudget)
			}
			if n := len(node.callsMatching(
				"cmd nvme connect ")); n != budgetRetries+1 {
				t.Errorf("%d connects, want the first and %d retries", n,
					budgetRetries)
			}
		})
	}
}

// DN2's re-run after a restart keeps DnMigrConnectPassBudget, even over the
// request the cutover brought: it converges from the stored request alone,
// with no request held before it to show the flag cleared (DN13). So a
// restart inside the cutover's converge leaves the gap to the DN8 retry, and
// a restarted agent with many destination sides whose source does not answer
// holds each side's locks the shorter budget, not the longer one, side after
// side.
func TestARestartReRunKeepsTheShortBudget(t *testing.T) {
	srv, node := newTestServer(t)
	srv.migrRetryInterval = time.Hour
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	withDnClock(srv, node)
	setFailAlways(node, "nvme connect", "connect refused")
	for _, req := range []*pb.SyncupSideRequest{
		pendingDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE),
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE),
	} {
		if _, err := srv.SyncupSide(ctx, req); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
	}

	// The kernel state and the stored request survive; the agent process
	// does not.
	restarted := NewDnAgentServer(node.osClient(),
		common.NewNameFmt(common.DefaultLocalStorPrefix),
		common.DefaultLocalStorPrefix, testDisk, testTrConf(),
		common.NvmetPortId)
	restarted.roundReads = false
	restarted.migrRetryInterval = time.Hour
	clock := withDnClock(restarted, node)
	rootCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(func() {
		cancel()
		restarted.WaitBackground()
	})
	start := clock.Now()
	node.Reset()
	if err := restarted.Reconcile(rootCtx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := clock.Now().Sub(start); got != common.DnMigrConnectPassBudget {
		t.Errorf("the re-run waited %v, want %v", got,
			common.DnMigrConnectPassBudget)
	}
	if n := len(node.callsMatching("cmd nvme connect ")); n != budgetRetries+1 {
		t.Errorf("the re-run made %d connects, want the first and %d retries",
			n, budgetRetries)
	}
}

// A restart that leaves the process holding no request for the side keeps
// DnMigrConnectPassBudget for the cutover's request even when that is the
// first request of the side the new process takes: with nothing held,
// nothing shows the pass that the flag was cleared (DN13). So after a lost
// --local-store a source slower than the shorter budget leaves the gap to
// the DN8 retry.
func TestARestartThatHoldsNoRequestKeepsTheShortBudget(t *testing.T) {
	srv, node := newTestServer(t)
	srv.migrRetryInterval = time.Hour
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	if _, err := srv.SyncupSide(ctx,
		pendingDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}

	// The agent restarts over an empty --local-store; the disk and every
	// kernel object survive.
	stopTestServer(t, srv)
	node.mu.Lock()
	node.protos = map[string][]byte{}
	node.mu.Unlock()
	restarted := startTestServer(t, node)
	restarted.migrRetryInterval = time.Hour
	clock := withDnClock(restarted, node)
	if _, err := restarted.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	setFailAlways(node, "nvme connect", "connect refused")

	start := clock.Now()
	node.Reset()
	if _, err := restarted.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := clock.Now().Sub(start); got != common.DnMigrConnectPassBudget {
		t.Errorf("the cutover's request waited %v, want %v", got,
			common.DnMigrConnectPassBudget)
	}
	if n := len(node.callsMatching("cmd nvme connect ")); n != budgetRetries+1 {
		t.Errorf("the cutover's request made %d connects, want the first "+
			"and %d retries", n, budgetRetries)
	}
}

// A restart whose DN2 re-run puts back a stored request that still carries
// cutover_pending gives the request the worker sends again the longer budget
// once more (DN13): the save of the request that cleared the flag failed,
// which is only logged (SH5), so the restarted process holds the flagged
// request, and the request without the flag brings the cutover to it as it
// did to the process before. The longer budget goes to one pass per cutover
// in a process, not in the side's whole life.
func TestARestartOverAFlaggedRequestGivesTheLongerBudgetOnceMore(
	t *testing.T,
) {
	srv, node := newTestServer(t)
	srv.migrRetryInterval = time.Hour
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	withDnClock(srv, node)
	setFailAlways(node, "nvme connect", "connect refused")
	if _, err := srv.SyncupSide(ctx,
		pendingDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	path := srv.nf.LocalSidePath(testCluster, testDn, testSp, testSide)
	oc := node.osClient()
	write := oc.WriteProtoFn
	oc.WriteProtoFn = func(
		ctx context.Context, p string, msg proto.Message,
	) error {
		if p == path {
			return errors.New("write " + p + ": no space left on device")
		}
		return write(ctx, p, msg)
	}
	srv.store = agent.NewStore(oc, common.DefaultLocalStorPrefix)
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := storedSideRevision(t, srv, node); got != 2 {
		t.Fatalf("the side file holds revision %d, want the 2 before the "+
			"failed save", got)
	}

	// The kernel state and the stored request survive; the agent process
	// does not.
	restarted := NewDnAgentServer(node.osClient(),
		common.NewNameFmt(common.DefaultLocalStorPrefix),
		common.DefaultLocalStorPrefix, testDisk, testTrConf(),
		common.NvmetPortId)
	restarted.roundReads = false
	restarted.migrRetryInterval = time.Hour
	clock := withDnClock(restarted, node)
	rootCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(func() {
		cancel()
		restarted.WaitBackground()
	})
	if err := restarted.Reconcile(rootCtx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	start := clock.Now()
	node.Reset()
	if _, err := restarted.SyncupSide(ctx,
		migrDstReq(3, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := clock.Now().Sub(start); got !=
		common.DnMigrCutoverConnectBudget {
		t.Errorf("the request sent again waited %v, want %v", got,
			common.DnMigrCutoverConnectBudget)
	}
	if n := len(node.callsMatching(
		"cmd nvme connect ")); n != cutoverRetries+1 {
		t.Errorf("the request sent again made %d connects, want the first "+
			"and %d retries", n, cutoverRetries)
	}
}

// A connect refused past DnMigrConnectPassBudget fails the pass after the
// budget's last retry: the target reads ERROR with the last refusal, the
// stacks stay on their dm-errors with every namespace inaccessible, and the
// DN8 loop is registered. The next converge whose connect answers runs the
// rest: the primary's dm-linear onto the dm-clone and the namespaces off
// inaccessible (architecture.md, Migration, dst step 5), the transition the
// retry loop exists to reach, and that pass deregisters it.
func TestMigrationDestinationConnectFailureRetries(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	clock := withDnClock(srv, node)

	setFailAlways(node, "nvme connect", "connect refused")
	start := clock.Now()
	node.Reset()
	reply, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE))
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	// The RPC still returns; the connect is retried in the background.
	if reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("rejected: %v", reply.GetAgentReply())
	}
	target := reply.GetSideInfo().GetMigrDstInfo().GetTargetInfo()
	if target.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		!strings.Contains(target.GetDetails(), "connect refused") {
		t.Errorf("target_info = %v/%q, want ERROR with the refusal",
			target.GetStatus(), target.GetDetails())
	}
	if n := len(node.callsMatching(
		"cmd nvme connect ")); n != budgetRetries+1 {
		t.Errorf("%d connects, want the first and %d retries", n,
			budgetRetries)
	}
	if got := clock.Now().Sub(start); got != common.DnMigrConnectPassBudget {
		t.Errorf("the pass waited %v, want the budget %v", got,
			common.DnMigrConnectPassBudget)
	}
	if !migrRetrying(srv) {
		t.Fatal("no background retry was registered")
	}
	// While the dm-clone is absent the destination keeps its exports on
	// dm-error with every namespace inaccessible.
	for cnId := range reply.GetSideInfo().GetCnIdToNvmeof() {
		nqn := srv.nf.SideToCnNqn(testCluster, testSp, testLeg, cnId)
		state, err := srv.nvmet.ProbeNamespace(ctx, nqn, sideNsid)
		if err != nil {
			t.Fatalf("ProbeNamespace: %v", err)
		}
		if state.AnaGrpId != common.AnaGrpIdInaccessible {
			t.Errorf("cn %d ana_grpid = %d, want inaccessible",
				cnId, state.AnaGrpId)
		}
	}

	clearFailAlways(node, "nvme connect")
	node.Reset()
	if _, err := srv.SyncupSide(ctx,
		migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if migrRetrying(srv) {
		t.Error("retry registration survived a pass that ran the whole " +
			"destination sequence")
	}
	names := newMigrDstNames()
	assertOrder(t, node,
		"cmd dmsetup create "+names.clone,
		"cmd dmsetup reload "+names.lin,
		fmt.Sprintf("writedirect %s/subsystems/%s/namespaces/1/ana_grpid=%d",
			agent.NvmetRoot, names.nqn, common.AnaGrpIdOptimized),
	)
	state, err := srv.nvmet.ProbeNamespace(ctx,
		srv.nf.SideToCnNqn(testCluster, testSp, testLeg, testCn1), sideNsid)
	if err != nil {
		t.Fatalf("ProbeNamespace: %v", err)
	}
	if state.AnaGrpId != common.AnaGrpIdNonOptimized {
		t.Errorf("standby ana_grpid = %d, want non-optimized",
			state.AnaGrpId)
	}
}
