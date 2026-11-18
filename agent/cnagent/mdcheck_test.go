package cnagent

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// withMdCheck is req with the data group's md check request set to epoch.
func withMdCheck(req *pb.SyncupCntlrRequest, epoch uint64) *pb.SyncupCntlrRequest {
	dataGrpOf(req).MdCheck = &pb.MdCheck{NextEpoch: 1, RequestEpoch: epoch}
	return req
}

// syncActionWrites lists the md/sync_action writes the node recorded.
func syncActionWrites(f *repFix) []string {
	var writes []string
	for _, call := range f.node.Calls() {
		if strings.Contains(call, "/md/sync_action=") {
			writes = append(writes, call[strings.LastIndex(call, "=")+1:])
		}
	}
	return writes
}

func mdDoneOf(info *pb.CntlrInfo) uint64 {
	return info.GetGrpIdToGrpInfo()[testDataGrp].GetMdCheckDoneEpoch()
}

// TestMdCheckRunsForItsRequest pins CN12's md check on a primary: a request
// on a group whose array runs whole, every member in sync, starts md's check
// with one write to md/sync_action; the reading names nothing while md
// checks, and names the request done on the read once md reads idle again,
// with no converge in between; a converge of the same request starts no
// second check; and the request's withdrawal ends the memory, so the
// reading names nothing.
func TestMdCheckRunsForItsRequest(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, quietLevel(3))
	f.node.Reset()
	info := f.syncup(t, withMdCheck(quietLevel(4), 1000))
	if got := syncActionWrites(f); len(got) != 1 || got[0] != mdActionCheck {
		t.Fatalf("sync_action writes %v, want one check", got)
	}
	if got := mdDoneOf(info); got != 0 {
		t.Fatalf("the converge that started the check reads done %d", got)
	}
	if read, _ := f.read(t, 4); mdDoneOf(read) != 0 {
		t.Fatalf("a read while md checks reads done %d", mdDoneOf(read))
	}
	f.node.setMdSync(f.mdDev, 0, mdActionIdle, "none")
	if read, _ := f.read(t, 4); mdDoneOf(read) != 1000 {
		t.Fatalf("the read after md's check reads done %d, want 1000",
			mdDoneOf(read))
	}
	f.node.Reset()
	info = f.syncup(t, withMdCheck(quietLevel(5), 1000))
	if got := syncActionWrites(f); len(got) != 0 {
		t.Fatalf("a converge of a done request wrote %v", got)
	}
	if got := mdDoneOf(info); got != 1000 {
		t.Fatalf("the converge of a done request reads done %d", got)
	}
	info = f.syncup(t, quietLevel(6))
	if got := mdDoneOf(info); got != 0 {
		t.Fatalf("the withdrawn request reads done %d", got)
	}
	info = f.syncup(t, withMdCheck(quietLevel(7), 1000))
	if got := mdDoneOf(info); got != 0 {
		t.Fatalf("the request back reads done %d: the withdrawal kept the "+
			"memory", got)
	}
}

// TestMdCheckAdoptsStopsAndLeavesAlone pins the rest of CN12's md check: a
// check md already runs when the request comes is adopted, with no write,
// and named done once md is idle; a check of the request is stopped (idle)
// when the request goes away or the array turns degraded; a check the
// agent never started, with no request, is left alone; and a degraded
// array starts none.
func TestMdCheckAdoptsStopsAndLeavesAlone(t *testing.T) {
	t.Run("adopted", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		f.node.setMdSync(f.mdDev, 0, mdActionCheck, "10 / 100")
		f.node.Reset()
		f.syncup(t, withMdCheck(quietLevel(4), 1000))
		if got := syncActionWrites(f); len(got) != 0 {
			t.Fatalf("a running check was not adopted: writes %v", got)
		}
		f.node.setMdSync(f.mdDev, 0, mdActionIdle, "none")
		if read, _ := f.read(t, 4); mdDoneOf(read) != 1000 {
			t.Fatalf("the adopted check reads done %d, want 1000",
				mdDoneOf(read))
		}
	})
	t.Run("the request withdrawn", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		f.syncup(t, withMdCheck(quietLevel(4), 1000))
		f.node.Reset()
		f.syncup(t, quietLevel(5))
		if got := syncActionWrites(f); len(got) != 1 ||
			got[0] != mdActionIdle {
			t.Fatalf("writes %v, want one idle", got)
		}
	})
	t.Run("a stop that fails is tried again", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		f.syncup(t, withMdCheck(quietLevel(4), 1000))
		key := "/md/sync_action=" + mdActionIdle
		f.node.failWriteAlways[key] = "device busy"
		f.syncup(t, quietLevel(5))
		delete(f.node.failWriteAlways, key)
		f.node.Reset()
		f.syncup(t, quietLevel(6))
		if got := syncActionWrites(f); len(got) != 1 ||
			got[0] != mdActionIdle {
			t.Fatalf("writes %v after a failed stop, want the stop "+
				"tried again", got)
		}
	})
	t.Run("the array degraded", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		f.syncup(t, withMdCheck(quietLevel(4), 1000))
		f.node.setMdSync(f.mdDev, 1, mdActionCheck, "10 / 100")
		f.node.Reset()
		f.syncup(t, withMdCheck(quietLevel(5), 1000))
		if got := syncActionWrites(f); len(got) == 0 ||
			got[0] != mdActionIdle {
			t.Fatalf("writes %v, want the check stopped", got)
		}
	})
	t.Run("someone else's check", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		f.node.setMdSync(f.mdDev, 0, mdActionCheck, "10 / 100")
		f.node.Reset()
		f.syncup(t, quietLevel(4))
		if got := syncActionWrites(f); len(got) != 0 {
			t.Fatalf("a check the agent never started got writes %v", got)
		}
	})
	t.Run("no start on a degraded array", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		f.node.setMdSync(f.mdDev, 1, mdActionIdle, "none")
		f.node.Reset()
		f.syncup(t, withMdCheck(quietLevel(4), 1000))
		for _, write := range syncActionWrites(f) {
			if write == mdActionCheck {
				t.Fatalf("a degraded array got a check")
			}
		}
	})
}

// TestBadBlocksAreCountedInTheReading pins CN12's and CN28's bad-block
// count: a member whose md bad-block list holds ranges gives its leg the
// count in the group's reading, on the converge and on the read alike, and
// no leg row changes: both legs keep their OK rows (the worker reads the
// count; dnv-worker.md AR8).
func TestBadBlocksAreCountedInTheReading(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, quietLevel(3))
	f.node.mu.Lock()
	_, array := f.node.arrayByDev(f.mdDev)
	array.badBlocks = map[string]string{f.leg2: "24064 128\n40000 8\n"}
	f.node.publishArray(array)
	f.node.mu.Unlock()
	info := f.syncup(t, quietLevel(4))
	read, _ := f.read(t, 4)
	for channel, got := range map[string]*pb.CntlrInfo{
		"converge": info, "read": read,
	} {
		counts := got.GetGrpIdToGrpInfo()[testDataGrp].GetLegIdToBadBlocks()
		if counts[testDataLeg2] != 2 || counts[testDataLeg] != 0 {
			t.Fatalf("%s: counts %v, want leg2 at 2 alone", channel, counts)
		}
		for _, legId := range []uint64{testDataLeg, testDataLeg2} {
			if got.GetLegIdToLeg()[legId].GetStatus() !=
				pb.ResStatus_RES_STATUS_OK {
				t.Fatalf("%s: leg %d row %v, want OK", channel, legId,
					got.GetLegIdToLeg()[legId])
			}
		}
	}
}

// TestADemotionForgetsTheMdCheck: a demotion stops the arrays, and with them
// any check, so the agent forgets the check of every group (keepMdChecks): a
// promotion back under the same request starts a new check and reads
// nothing done until md has run it.
func TestADemotionForgetsTheMdCheck(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, quietLevel(3))
	f.syncup(t, withMdCheck(quietLevel(4), 1000))
	f.node.setMdSync(f.mdDev, 0, mdActionIdle, "none")
	if read, _ := f.read(t, 4); mdDoneOf(read) != 1000 {
		t.Fatalf("before the demotion: done %d, want 1000", mdDoneOf(read))
	}
	standby := cntlrReq(reqOpts{revision: 5, primary: false, raid1: true,
		twoLegs: true, level: pb.SpLevel_SP_LEVEL_NO_THINPOOL,
		subsys: map[string]*pb.Subsystem{}})
	f.syncup(t, withMdCheck(standby, 1000))
	f.node.Reset()
	info := f.syncup(t, withMdCheck(quietLevel(6), 1000))
	if got := mdDoneOf(info); got != 0 {
		t.Fatalf("after the promotion: done %d, want 0", got)
	}
	if got := syncActionWrites(f); len(got) != 1 || got[0] != mdActionCheck {
		t.Fatalf("after the promotion: writes %v, want one check", got)
	}
}

// TestMdCheckEndsOnlyWhenMdRanItToItsEnd pins noteMdCheck: a check md
// stopped — idle after a recovery, or idle with sync_min left where the
// check had come to — is not done and is forgotten, so the next converge
// writes sync_min back to 0 before it starts the check again; one md ran
// to its end (last action check, sync_min 0, the array whole) is done.
func TestMdCheckEndsOnlyWhenMdRanItToItsEnd(t *testing.T) {
	for _, tc := range []struct {
		name    string
		last    string
		syncMin string
		done    bool
	}{
		{"to its end", mdActionCheck, "0", true},
		{"a recovery after it", "recover", "0", false},
		{"stopped part way", mdActionCheck, "4096", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			f.syncup(t, quietLevel(3))
			f.node.Reset()
			f.syncup(t, withMdCheck(quietLevel(4), 1000))
			if got := syncActionWrites(f); len(got) != 1 ||
				got[0] != mdActionCheck {
				t.Fatalf("writes %v, want one check", got)
			}
			if !f.node.hasCall("write") ||
				!strings.Contains(strings.Join(f.node.Calls(), "\n"),
					"/md/sync_min=0") {
				t.Fatalf("no sync_min write before the check: %v",
					f.node.Calls())
			}
			f.node.mu.Lock()
			_, array := f.node.arrayByDev(f.mdDev)
			array.syncAction = mdActionIdle
			array.lastSyncAction = tc.last
			array.syncMin = tc.syncMin
			f.node.publishArray(array)
			f.node.mu.Unlock()
			read, _ := f.read(t, 4)
			if got := mdDoneOf(read) == 1000; got != tc.done {
				t.Fatalf("done %v, want %v", got, tc.done)
			}
			if tc.done {
				return
			}
			f.node.Reset()
			f.syncup(t, withMdCheck(quietLevel(5), 1000))
			if got := syncActionWrites(f); len(got) != 1 ||
				got[0] != mdActionCheck {
				t.Fatalf("after the stop: writes %v, want the check again",
					got)
			}
		})
	}
}

// TestMdCheckLeavesAnAdoptedCheckAndStopsAnUnsuitedGroup: a check the agent
// adopted, not started, is left to run when the request goes; a check of
// the request is stopped once the group carries a forced-start mark; and a
// level that stops the arrays forgets the check, so after it the reading
// names nothing done until md runs a check again.
func TestMdCheckLeavesAnAdoptedCheckAndStopsAnUnsuitedGroup(t *testing.T) {
	t.Run("adopted, then the request withdrawn", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		f.node.setMdSync(f.mdDev, 0, mdActionCheck, "10 / 100")
		f.syncup(t, withMdCheck(quietLevel(4), 1000))
		f.node.Reset()
		f.syncup(t, quietLevel(5))
		if got := syncActionWrites(f); len(got) != 0 {
			t.Fatalf("an adopted check got writes %v on the withdrawal", got)
		}
	})
	t.Run("a forced-start mark", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		f.syncup(t, withMdCheck(quietLevel(4), 1000))
		f.node.Reset()
		f.syncup(t, withMark(withMdCheck(quietLevel(5), 1000), testDataLeg2))
		if got := syncActionWrites(f); len(got) == 0 ||
			got[0] != mdActionIdle {
			t.Fatalf("writes %v, want the check stopped", got)
		}
	})
	t.Run("a stored request's mark", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		req := withMark(withMdCheck(quietLevel(4), 1000), testDataLeg2)
		path := f.srv.nf.LocalCntlrPath(testCluster, testCn, testSp, testCntlr)
		if err := f.srv.store.Save(f.ctx, path, req); err != nil {
			t.Fatalf("saving the marked request: %v", err)
		}
		f.node.Reset()
		fresh := newCnServer(f.node)
		reconcileForTest(t, fresh)
		for _, write := range syncActionWrites(f) {
			if write == mdActionCheck {
				t.Fatalf("the startup reconcile of a request whose group " +
					"carries a mark started a check")
			}
		}
	})
	t.Run("a level that stops the arrays", func(t *testing.T) {
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		f.syncup(t, withMdCheck(quietLevel(4), 1000))
		f.node.setMdSync(f.mdDev, 0, mdActionIdle, "none")
		if read, _ := f.read(t, 4); mdDoneOf(read) != 1000 {
			t.Fatalf("before the level: done %d", mdDoneOf(read))
		}
		noRedund := withMdCheck(cntlrReq(reqOpts{revision: 5, primary: true,
			raid1: true, twoLegs: true, level: pb.SpLevel_SP_LEVEL_NO_REDUND,
			subsys: map[string]*pb.Subsystem{}}), 1000)
		f.syncup(t, noRedund)
		info := f.syncup(t, withMdCheck(quietLevel(6), 1000))
		if got := mdDoneOf(info); got != 0 {
			t.Fatalf("after the level: done %d, want 0", got)
		}
	})
}

// TestAnMdCheckADeliveringConvergeSkippedIsNamed: the SyncupCntlr that
// delivers a group's md check request meets an array read that does not
// answer, so its pass leaves the group's row ERROR and steps no check (CN12,
// The md check). A read then finds the array running, its row OK, and names
// the group's md check where a converge would act on it (mdCheckDue): a
// start over an idle array that runs whole, an adoption of a check md
// already runs, and a stop of a check md runs on a degraded array. The one
// re-send that name brings acts, and the read after it names nothing.
func TestAnMdCheckADeliveringConvergeSkippedIsNamed(t *testing.T) {
	for _, tc := range []struct {
		name string
		read string
		// arm sets md's sync state before the delivering converge.
		arm func(f *repFix)
		// writes are the sync_action writes the re-send makes.
		writes []string
		// done sets md idle after the re-send and checks the request reads
		// done, for an adoption, which writes nothing.
		done bool
	}{
		{"a start, sync_action unread", "/md/sync_action", nil,
			[]string{mdActionCheck}, false},
		{"a start, degraded unread", "/md/degraded", nil,
			[]string{mdActionCheck}, false},
		{"an adoption", "/md/degraded", func(f *repFix) {
			f.node.setMdSync(f.mdDev, 0, mdActionCheck, "10 / 100")
		}, nil, true},
		{"a stop", "/md/sync_completed", func(f *repFix) {
			f.node.setMdSync(f.mdDev, 1, mdActionCheck, "10 / 100")
		}, []string{mdActionIdle}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			f.syncup(t, quietLevel(3))
			if tc.arm != nil {
				tc.arm(f)
			}
			f.node.Reset()
			f.node.mu.Lock()
			f.node.killReadAlways[tc.read] = true
			f.node.mu.Unlock()
			held := withMdCheck(quietLevel(4), 1000)
			reply, err := f.srv.SyncupCntlr(f.ctx, held)
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			f.node.mu.Lock()
			clear(f.node.killReadAlways)
			f.node.mu.Unlock()
			if row := reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]; row.
				GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
				t.Fatalf("fixture is wrong: the delivering converge's group "+
					"row = %v", row)
			}
			if got := syncActionWrites(f); len(got) != 0 {
				t.Fatalf("fixture is wrong: the delivering converge wrote "+
					"%v", got)
			}
			named := fmt.Sprintf("md check of group %#x: %v", testDataGrp,
				errMdCheckDue)
			f.node.Reset()
			f.node.forgetTraces()
			round, resent := resendRound(t, f.srv, held, "named")
			assertCodeNames(t, "the read after the converge", round, named)
			if resent == nil {
				t.Fatalf("the round that names the md check brought no " +
					"re-send")
			}
			// The fake files a write by its path, not by trace id; the
			// round's read writes nothing, so the writes are the re-send's.
			if got := syncActionWrites(f); !slices.Equal(got, tc.writes) {
				t.Fatalf("the re-send wrote sync_action %q, want %q", got,
					tc.writes)
			}
			round, resent = resendRound(t, f.srv, held, "after")
			assertRowNotNamed(t, round.GetAgentReply(), "the read after the "+
				"re-send", fmt.Sprintf("md check of group %#x", testDataGrp))
			if resent != nil && strings.Contains(
				round.GetAgentReply().GetDetails(), "md check") {
				t.Errorf("the round after the re-send re-sent for the md " +
					"check")
			}
			if tc.done {
				f.node.setMdSync(f.mdDev, 0, mdActionIdle, "none")
				if read, _ := f.read(t, 4); mdDoneOf(read) != 1000 {
					t.Fatalf("the adopted check reads done %d, want 1000",
						mdDoneOf(read))
				}
			}
			assertNothingRunsBetweenCalls(t, f.node, "named", "after")
		})
	}
}

// TestMdCheckLeavesAReadOnlyArrayAlone: md takes a "check" written to the
// sync_action of an array it holds read-only and starts nothing, so a start
// there would be decided again on every pass. Neither a converge nor a read
// acts on a request over a read-only array (mdCheckable): the converge
// writes nothing, and the read names nothing.
func TestMdCheckLeavesAReadOnlyArrayAlone(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.syncup(t, quietLevel(3))
	f.node.setArrayState(f.mdDev, mdStateReadonly)
	f.node.Reset()
	held := withMdCheck(quietLevel(4), 1000)
	if reply, err := f.srv.SyncupCntlr(f.ctx, held); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	} else if row := reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]; row.
		GetStatus() != pb.ResStatus_RES_STATUS_OK {
		t.Fatalf("fixture is wrong: the read-only array's row = %v", row)
	}
	if got := syncActionWrites(f); len(got) != 0 {
		t.Fatalf("the converge wrote sync_action %q over a read-only array",
			got)
	}
	_, verdict := f.read(t, 4)
	assertRowNotNamed(t, verdict, "the read over a read-only array",
		fmt.Sprintf("md check of group %#x", testDataGrp))
	// Positive control: the same array read-write gets the check.
	f.node.setArrayState(f.mdDev, "clean")
	f.node.Reset()
	if _, err := f.srv.SyncupCntlr(f.ctx, held); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if got := syncActionWrites(f); !slices.Equal(got,
		[]string{mdActionCheck}) {
		t.Fatalf("the read-write array got sync_action writes %q, want "+
			"one check", got)
	}
}

// TestAnMdCheckOverAReconciliationThatFailsIsSteppedByTheResend: the
// converge's member reconciliation of a group whose array runs keeps failing
// — a take-back repair's new leg whose superblock read does not answer, its
// row PENDING, OK or ERROR as a stalled leg's prober leaves it, or an extra
// member whose --fail md keeps refusing — and md then runs a check the group
// does not suit (an open repair). A read names the check's stop (mdCheckDue);
// the converge still steps the check over the array its pass read
// (ensureGroup), so the one re-send that name brings writes idle, and the
// read after it names the check no more.
func TestAnMdCheckOverAReconciliationThatFailsIsSteppedByTheResend(
	t *testing.T,
) {
	type shape struct {
		name string
		// arm builds the failing reconciliation and returns the request.
		arm func(t *testing.T, f *repFix) *pb.SyncupCntlrRequest
		// after runs once the delivering converge returned.
		after func(t *testing.T, f *repFix)
	}
	stalledTakeBack := func(t *testing.T, f *repFix) *pb.SyncupCntlrRequest {
		f.badLegDead()
		f.node.setSuperblock(f.leg2, fakeSuperblock{
			role: "Active device 1", events: 10})
		f.node.setSuperblock(f.newLegDev, fakeSuperblock{events: 50})
		f.node.mu.Lock()
		f.node.killCmdAlways[examineOf(f.newLegDev)] = true
		f.node.mu.Unlock()
		return withMdCheck(
			takenBackReq(3, pb.RepStage_REP_STAGE_CREATED, false), 1000)
	}
	for _, tc := range []shape{
		{"a stalled take-back leg, its row PENDING", stalledTakeBack, nil},
		{"a stalled take-back leg, its row OK", stalledTakeBack,
			func(t *testing.T, f *repFix) { probeLegs(t, f.srv, repNewLeg) }},
		{"a stalled take-back leg, its row ERROR", stalledTakeBack,
			func(t *testing.T, f *repFix) {
				st := legState(t, f.srv)
				f.srv.mu.Lock()
				prober := st.probers[repNewLeg]
				f.srv.mu.Unlock()
				if prober == nil {
					t.Fatalf("no prober for the new leg")
				}
				prober.begin(f.srv.now())
				prober.finish(errors.New("probe stalled"))
			}},
		{"an extra whose --fail md refuses", func(t *testing.T,
			f *repFix) *pb.SyncupCntlrRequest {
			f.plantAbortedExtra(t)
			f.node.mu.Lock()
			f.node.failCmdAlways["mdadm "+f.mdDev+" --fail "+f.newLegDev] =
				"mdadm: set device faulty failed"
			f.node.mu.Unlock()
			return withMdCheck(abortedReq(4), 1000)
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepFix(t, allLegs...)
			held := tc.arm(t, f)
			f.node.Reset()
			reply, err := f.srv.SyncupCntlr(f.ctx, held)
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if row := reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]; row.
				GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
				t.Fatalf("fixture is wrong: the converge's group row = %v",
					row)
			}
			if tc.after != nil {
				tc.after(t, f)
			}
			// md starts a check of its own over the group the open repair
			// keeps from suiting one.
			f.node.setMdSync(f.mdDev, 0, mdActionCheck, "10 / 100")
			named := fmt.Sprintf("md check of group %#x", testDataGrp)
			f.node.Reset()
			f.node.forgetTraces()
			round, resent := resendRound(t, f.srv, held, "named")
			assertCodeNames(t, "the read while md checks", round, named)
			if resent == nil {
				t.Fatalf("the round that names the md check brought no " +
					"re-send")
			}
			if row := resent.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]; row.
				GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
				t.Fatalf("fixture is wrong: the re-send's group row = %v",
					row)
			}
			if got := syncActionWrites(f); !slices.Equal(got,
				[]string{mdActionIdle}) {
				t.Fatalf("the re-send wrote sync_action %q, want one %q",
					got, mdActionIdle)
			}
			round, _ = resendRound(t, f.srv, held, "after")
			assertRowNotNamed(t, round.GetAgentReply(), "the read after the "+
				"re-send", named)
			assertNothingRunsBetweenCalls(t, f.node, "named", "after")
		})
	}
}

// TestAnMdCheckIsLeftAloneWhileTheInSyncGuardHolds: while the in-sync guard
// holds over a running array (CN12, Member reconciliation) — the member that
// leaves is the only one in sync on a leg that serves — the converge steps
// no md check beside the guard's error (inSyncGuardError) and a read, whose
// group row reads the same error, names none: the members md shows in sync
// there may be on legs that do not serve.
func TestAnMdCheckIsLeftAloneWhileTheInSyncGuardHolds(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.plantAbortedExtra(t)
	probeAllLegs(t, f.srv)
	f.legDead(testDataLeg2, testIp2, testSvcId2)
	f.node.setMdSync(f.mdDev, 0, mdActionCheck, "10 / 100")
	held := withMdCheck(abortedReq(4), 1000)
	f.node.Reset()
	reply, err := f.srv.SyncupCntlr(f.ctx, held)
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if row := reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]; !strings.
		Contains(row.GetDetails(), "left the group but is not failed") {
		t.Fatalf("fixture is wrong: the converge's group row = %v", row)
	}
	if got := syncActionWrites(f); len(got) != 0 {
		t.Fatalf("the converge wrote sync_action %q while the guard holds",
			got)
	}
	_, verdict := f.read(t, 4)
	assertRowNotNamed(t, verdict, "the read while the guard holds",
		fmt.Sprintf("md check of group %#x", testDataGrp))
}

// TestAnMdCheckStepReadsTheArrayAgainAfterAMemberCommand: the member
// reconciliation fails an extra member (--fail goes through) and then fails
// itself, md refusing the extra's --remove. The step over the failed
// reconciliation decides over the array read again after the fail
// (arrayAfter), which holds the extra faulty, so it starts no check though a
// request stands and the group suits one; a step over the array as read
// before the fail would start one over an array that is not whole. The read
// after it names no md check either.
func TestAnMdCheckStepReadsTheArrayAgainAfterAMemberCommand(t *testing.T) {
	f := newRepFix(t, allLegs...)
	f.plantAbortedExtra(t)
	f.node.mu.Lock()
	f.node.failCmdAlways["mdadm "+f.mdDev+" --remove "+f.newLegDev] =
		"mdadm: hot remove failed: Device or resource busy"
	f.node.mu.Unlock()
	held := withMdCheck(noRepairReq(4), 1000)
	f.node.Reset()
	reply, err := f.srv.SyncupCntlr(f.ctx, held)
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if row := reply.GetCntlrInfo().GetGrpIdToMdRaid()[testDataGrp]; row.
		GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("fixture is wrong: the converge's group row = %v", row)
	}
	if !f.node.hasCall(f.fail(f.newLegDev)) {
		t.Fatalf("fixture is wrong: the extra was not failed")
	}
	if got := syncActionWrites(f); len(got) != 0 {
		t.Fatalf("the step wrote sync_action %q over an array whose extra "+
			"the pass failed", got)
	}
	_, verdict := f.read(t, 4)
	assertRowNotNamed(t, verdict, "the read after the converge",
		fmt.Sprintf("md check of group %#x", testDataGrp))
}
