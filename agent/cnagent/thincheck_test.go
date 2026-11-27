package cnagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// thinCheckFix is a primary whose slice pool is not loaded and whose meta
// concat's thin superblock carries dm-thin's needs-check mark, as a new
// primary meets it after a commit failed on the old one: the pool built at
// revision 2, the cntlr demoted at 3, which takes the pool down, and the
// mark set. Every thin_check run waits on gate until the test closes it.
type thinCheckFix struct {
	srv  *CnAgentServer
	node *fakeNode
	meta string
	gate chan struct{}
}

func newThinCheckFix(t *testing.T) *thinCheckFix {
	t.Helper()
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	if _, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 3})); err != nil {
		t.Fatalf("the demotion: %v", err)
	}
	if node.dms[poolName(srv)] != nil {
		t.Fatalf("the demotion left the pool")
	}
	srv.thinCheckBackoff = 0
	f := &thinCheckFix{srv: srv, node: node, gate: make(chan struct{}),
		meta: srv.nf.DmPath(names(srv).CnPoolMetaName(
			testCluster, testCn, testSp, testSlice))}
	node.mu.Lock()
	node.thinNeedsCheck[f.meta] = true
	node.gate["thin_check"] = f.gate
	node.mu.Unlock()
	return f
}

func (f *thinCheckFix) syncup(t *testing.T, revision uint64) *pb.CntlrInfo {
	t.Helper()
	reply, err := f.srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: revision, primary: true}))
	if err != nil {
		t.Fatalf("SyncupCntlr %d: %v", revision, err)
	}
	return reply.GetCntlrInfo()
}

// runEnded waits until the slice's check run in the agent's memory ends.
func (f *thinCheckFix) runEnded(t *testing.T) {
	t.Helper()
	st := f.srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	// No converge runs while the test waits, so the map is read with no
	// lock; the run's own fields are read under its mutex (result).
	waitFor(t, func() bool {
		run := st.thinChecks[testSlice]
		if run == nil {
			return false
		}
		done, _, _, _, _ := run.result()
		return done
	})
}

// TestTheNeedsCheckMarkIsCheckedBeforeThePoolLoads pins CN13's needs-check
// mark: a primary that would create a slice's pool reads the mark from the
// meta concat's thin superblock and, with the mark set, loads no pool and
// runs thin_check in the background: the pool row reads PROVISIONING, and
// from the next pass every thin row of the slice too, on the converge and
// on the read; once the check passes it clears the mark, the read names the
// absent pool, and the next converge loads the pool with no mark, its row
// OK. A check that fails leaves the pool unloaded and its row ERROR with
// RES_REASON_POOL_META_DAMAGED on both channels, its thin rows PROVISIONING,
// and the read names nothing. A demotion stops a run that goes on.
func TestTheNeedsCheckMarkIsCheckedBeforeThePoolLoads(t *testing.T) {
	t.Run("the check passes", func(t *testing.T) {
		f := newThinCheckFix(t)
		info := f.syncup(t, 4)
		assertPoolRow(t, info, "the first converge",
			pb.ResStatus_RES_STATUS_PROVISIONING, pb.ResReason_RES_REASON_NONE)
		if f.node.dms[poolName(f.srv)] != nil {
			t.Fatalf("the pool was loaded over the mark")
		}
		info = f.syncup(t, 4)
		assertPoolRow(t, info, "the second converge",
			pb.ResStatus_RES_STATUS_PROVISIONING, pb.ResReason_RES_REASON_NONE)
		assertThinRows(t, info, "the second converge",
			pb.ResStatus_RES_STATUS_PROVISIONING)
		read := checkRound(t, f.srv, 4)
		assertPoolRow(t, read, "the read",
			pb.ResStatus_RES_STATUS_PROVISIONING, pb.ResReason_RES_REASON_NONE)
		assertThinRows(t, read, "the read",
			pb.ResStatus_RES_STATUS_PROVISIONING)
		if calls := f.node.callsMatching("cmd thin_check "); len(calls) != 1 ||
			!strings.Contains(calls[0], "--clear-needs-check-flag "+f.meta) {
			t.Fatalf("thin_check runs %v, want one that clears the mark "+
				"while it waits", calls)
		}
		close(f.gate)
		f.runEnded(t)
		f.node.mu.Lock()
		marked := f.node.thinNeedsCheck[f.meta]
		f.node.mu.Unlock()
		if marked {
			t.Fatalf("the passed check left the mark")
		}
		if v := checkCntlrVerdict(t, f.srv, 4); v.GetCode() == 0 {
			t.Fatalf("the read after the check names nothing: %v", v)
		}
		info = f.syncup(t, 4)
		assertPoolRow(t, info, "the converge after the check",
			pb.ResStatus_RES_STATUS_OK, pb.ResReason_RES_REASON_NONE)
		if f.node.dms[poolName(f.srv)] == nil {
			t.Fatalf("the converge after the check loaded no pool")
		}
	})
	t.Run("the check fails", func(t *testing.T) {
		f := newThinCheckFix(t)
		f.node.mu.Lock()
		f.node.thinCheckFails[f.meta] = "bad checksum in metadata index"
		f.node.mu.Unlock()
		close(f.gate)
		// A refusal counts toward thinCheckRefusals: the slice reads
		// checking, and the next converge runs the check again, until that
		// many runs in a row refused.
		var info *pb.CntlrInfo
		for n := 1; n <= thinCheckRefusals; n++ {
			info = f.syncup(t, 4)
			if n > 1 {
				assertPoolRow(t, info, "a converge between refusals",
					pb.ResStatus_RES_STATUS_PROVISIONING,
					pb.ResReason_RES_REASON_NONE)
			}
			f.runEnded(t)
		}
		info = f.syncup(t, 4)
		read := checkRound(t, f.srv, 4)
		for label, got := range map[string]*pb.CntlrInfo{
			"the converge": info, "the read": read} {
			assertPoolRow(t, got, label, pb.ResStatus_RES_STATUS_ERROR,
				pb.ResReason_RES_REASON_POOL_META_DAMAGED)
			assertThinRows(t, got, label, pb.ResStatus_RES_STATUS_PROVISIONING)
		}
		if f.node.dms[poolName(f.srv)] != nil {
			t.Fatalf("the pool was loaded over damaged metadata")
		}
		if calls := f.node.callsMatching("cmd thin_check "); len(calls) !=
			thinCheckRefusals {
			t.Fatalf("thin_check runs %v, want %d", calls, thinCheckRefusals)
		}
		f.node.mu.Lock()
		marked := f.node.thinNeedsCheck[f.meta]
		f.node.mu.Unlock()
		if !marked {
			t.Fatalf("a refused check cleared the mark")
		}
		if v := checkCntlrVerdict(t, f.srv, 4); v.GetCode() != 0 {
			t.Fatalf("the read names the damaged pool: %v", v)
		}
	})
	t.Run("a run that cannot read the device counts no refusal",
		func(t *testing.T) {
			// thin_check exits non-zero on a device another process holds
			// open, as a run an earlier agent process left does, or on an IO
			// error: no metadata was judged, so the slice keeps reading
			// checking past thinCheckRefusals such runs, and the run after
			// the device is free passes.
			f := newThinCheckFix(t)
			f.node.mu.Lock()
			f.node.thinCheckFails[f.meta] = "Device or resource busy (os error 16)"
			f.node.mu.Unlock()
			close(f.gate)
			for n := 0; n <= thinCheckRefusals; n++ {
				info := f.syncup(t, 4)
				assertPoolRow(t, info, "a converge over a busy device",
					pb.ResStatus_RES_STATUS_PROVISIONING,
					pb.ResReason_RES_REASON_NONE)
				f.runEnded(t)
			}
			f.node.mu.Lock()
			f.node.thinCheckFails[f.meta] = "Input/output error (os error 5)"
			f.node.mu.Unlock()
			info := f.syncup(t, 4)
			assertPoolRow(t, info, "a converge over an IO error",
				pb.ResStatus_RES_STATUS_PROVISIONING, pb.ResReason_RES_REASON_NONE)
			f.runEnded(t)
			f.node.mu.Lock()
			delete(f.node.thinCheckFails, f.meta)
			f.node.mu.Unlock()
			f.syncup(t, 4)
			f.runEnded(t)
			info = f.syncup(t, 4)
			assertPoolRow(t, info, "the converge after the device is free",
				pb.ResStatus_RES_STATUS_OK, pb.ResReason_RES_REASON_NONE)
		})
	t.Run("damaged metadata whose mark goes", func(t *testing.T) {
		// Once the metadata reads damaged, the read looks at the mark: while
		// it is set the read names nothing; once it is gone, as when an
		// earlier process's thin_check or an operator cleared it, the read
		// names the pool, and the converge it brings loads the pool.
		f := newThinCheckFix(t)
		f.node.mu.Lock()
		f.node.thinCheckFails[f.meta] = "bad checksum in metadata index"
		f.node.mu.Unlock()
		close(f.gate)
		for n := 1; n <= thinCheckRefusals; n++ {
			f.syncup(t, 4)
			f.runEnded(t)
		}
		assertPoolRow(t, f.syncup(t, 4), "the damaged pool",
			pb.ResStatus_RES_STATUS_ERROR,
			pb.ResReason_RES_REASON_POOL_META_DAMAGED)
		if v := checkCntlrVerdict(t, f.srv, 4); v.GetCode() != 0 {
			t.Fatalf("the read names the damaged pool with its mark set: %v", v)
		}
		f.node.mu.Lock()
		f.node.thinNeedsCheck[f.meta] = false
		delete(f.node.thinCheckFails, f.meta)
		f.node.mu.Unlock()
		if v := checkCntlrVerdict(t, f.srv, 4); v.GetCode() == 0 ||
			!strings.Contains(v.GetDetails(), detailsThinGone) {
			t.Fatalf("the read after the mark went names %v, want the pool "+
				"whose mark is gone", v)
		}
		calls := len(f.node.callsMatching("cmd thin_check "))
		assertPoolRow(t, f.syncup(t, 4), "the converge after the mark went",
			pb.ResStatus_RES_STATUS_OK, pb.ResReason_RES_REASON_NONE)
		if got := len(f.node.callsMatching("cmd thin_check ")); got != calls {
			t.Fatalf("the converge after the mark went ran thin_check")
		}
	})
	t.Run("a refused run waits out its pause", func(t *testing.T) {
		// A run that did not pass runs again after thinCheckBackoff, on the
		// converge the read's name of the pool brings once the pause is
		// over: the read names nothing during the pause, and a converge
		// then runs no check.
		f := newThinCheckFix(t)
		clock := &fakeClock{now: time.Now()}
		f.srv.now = clock.Now
		f.srv.thinCheckBackoff = time.Minute
		f.node.mu.Lock()
		f.node.thinCheckFails[f.meta] = "bad checksum in metadata index"
		f.node.mu.Unlock()
		close(f.gate)
		f.syncup(t, 4)
		f.runEnded(t)
		if v := checkCntlrVerdict(t, f.srv, 4); v.GetCode() != 0 {
			t.Fatalf("the read during the pause names %v", v)
		}
		info := f.syncup(t, 4)
		assertPoolRow(t, info, "a converge during the pause",
			pb.ResStatus_RES_STATUS_PROVISIONING, pb.ResReason_RES_REASON_NONE)
		if calls := f.node.callsMatching("cmd thin_check "); len(calls) != 1 {
			t.Fatalf("thin_check runs %v during the pause, want one", calls)
		}
		clock.advance(time.Minute)
		if v := checkCntlrVerdict(t, f.srv, 4); v.GetCode() == 0 ||
			!strings.Contains(v.GetDetails(), detailsThinDue) {
			t.Fatalf("the read after the pause names %v, want the pool "+
				"whose check may run again", v)
		}
		f.syncup(t, 4)
		f.runEnded(t)
		if calls := f.node.callsMatching("cmd thin_check "); len(calls) != 2 {
			t.Fatalf("thin_check runs %v after the pause, want two", calls)
		}
	})
	t.Run("a run that does not end in a refusal", func(t *testing.T) {
		// A run killed at its time limit before thin_check touched the
		// metadata counts no refusal: the slice keeps reading checking, the
		// mark still set, and the next run passes.
		f := newThinCheckFix(t)
		f.node.mu.Lock()
		f.node.killCmdNoEffect["cmd thin_check "] = true
		f.node.mu.Unlock()
		close(f.gate)
		f.syncup(t, 4)
		f.runEnded(t)
		info := f.syncup(t, 4)
		assertPoolRow(t, info, "after a killed run",
			pb.ResStatus_RES_STATUS_PROVISIONING, pb.ResReason_RES_REASON_NONE)
		f.runEnded(t)
		info = f.syncup(t, 4)
		assertPoolRow(t, info, "after the run that passed",
			pb.ResStatus_RES_STATUS_OK, pb.ResReason_RES_REASON_NONE)
	})
	t.Run("a demotion stops the run", func(t *testing.T) {
		f := newThinCheckFix(t)
		f.syncup(t, 4)
		if _, err := f.srv.SyncupCntlr(context.Background(),
			cntlrReq(reqOpts{revision: 5})); err != nil {
			t.Fatalf("the demotion: %v", err)
		}
		st := f.srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
		left := len(st.thinChecks)
		if left != 0 {
			t.Fatalf("the demotion kept %d check runs", left)
		}
		close(f.gate)
	})
}

// assertPoolRow checks the slice pool row of a report.
func assertPoolRow(
	t *testing.T,
	info *pb.CntlrInfo,
	label string,
	status pb.ResStatus,
	reason pb.ResReason,
) {
	t.Helper()
	row := info.GetSliceIdToDmPool()[testSlice]
	if row.GetStatus() != status || row.GetReason() != reason {
		t.Fatalf("%s: pool row %v, want %v %v", label, row, status, reason)
	}
	if status == pb.ResStatus_RES_STATUS_PROVISIONING &&
		!strings.Contains(row.GetDetails(), detailsThinCheck) {
		t.Fatalf("%s: pool row %v, want the check's details", label, row)
	}
}

// assertThinRows checks every thin row of the slice in a report.
func assertThinRows(
	t *testing.T,
	info *pb.CntlrInfo,
	label string,
	status pb.ResStatus,
) {
	t.Helper()
	if len(info.GetTdIdToThinInfo()) == 0 {
		t.Fatalf("%s: no thin rows", label)
	}
	for tdId, thin := range info.GetTdIdToThinInfo() {
		if row := thin.GetSliceIdToDmThin()[testSlice]; row.GetStatus() !=
			status {
			t.Fatalf("%s: td %#x's thin row %v, want %v", label, tdId, row,
				status)
		}
	}
}
