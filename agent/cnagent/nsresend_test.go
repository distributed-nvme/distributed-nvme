package cnagent

import (
	"context"
	"fmt"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// A namespace's waits and failed steps, as the worker's re-send drives them
// (dnv-worker.md RW4): a cntlr's verdict names a namespace's row, or the
// layer under it, only while a converge would act on it (cnagent.md CN16,
// CN30), and nothing in the agent runs a converge again between the
// worker's calls.

// snapNsOpts is the request of a primary with an origin td, a snapshot td
// not yet created and a new namespace over the snapshot, at revision.
func snapNsOpts(revision uint64) reqOpts {
	subsys := defaultSubsys(false)
	subsys[testNqn].NsList = append(subsys[testNqn].NsList,
		&pb.Namespace{
			NsId:     snapNsId,
			NsIdx:    2,
			TdId:     testSnapTd,
			DevUuid:  "22222222-2222-4222-8222-222222222222",
			DevNguid: "22222222222242228222222222222222",
		})
	return reqOpts{revision: revision, primary: true, twoSlices: true,
		tds: snapTds(), subsys: subsys, leftoverOk: true}
}

// snapNsId is the namespace snapNsOpts adds over the snapshot td.
const snapNsId = uint64(0x1b)

// snapDueName is how a read names the uncreated snapshot td (snapshotDue).
func snapDueName() string {
	return fmt.Sprintf("snapshot td %#x: %v", testSnapTd, errSnapshotDue)
}

// TestASnapshotWhoseCreateFailsOnceIsNamedAndResent: a new namespace over a
// snapshot not yet created, whose create_snap in one slice fails once,
// waits inaccessible, its ns-dev not built over a raid0 that is not there.
// The snapshot's rows are left out of the verdict (snapsLackingADevice),
// and the converge's own reply names nothing for it; a read names the
// snapshot while the converge's pre-pass would send its message
// (snapshotDue, CN14, CN30), and the one re-send that name brings makes the
// id under the round's trace id, builds the stack and moves the namespace
// optimized. The read after it names nothing.
func TestASnapshotWhoseCreateFailsOnceIsNamedAndResent(t *testing.T) {
	srv, node := newTestServer(t)
	message := "message " + poolNameOf(srv, testSlice2) + " 0 create_snap"
	node.failCmd[message] = "device-mapper: message ioctl failed: " +
		"Connection timed out"
	reply := syncupSnapshot(t, srv, node, snapNsOpts(3))
	assertCodeZero(t, "the converge whose create failed", reply)
	assertErrorDetails(t, reply.GetCntlrInfo().GetNsIdToDmLinear()[snapNsId],
		raid0Name(srv, testSnapTd)+" is not there",
		"the waiting snapshot's ns-dev")

	held := cntlrReq(snapNsOpts(3))
	node.Reset()
	node.forgetTraces()
	round, resent := resendRound(t, srv, held, "named")
	assertRowVerdict(t, round.GetAgentReply(), "the read after the converge",
		"enumeration failed: "+snapDueName())
	if resent == nil {
		t.Fatalf("the round that names the snapshot brought no re-send")
	}
	if !tracedCall(node, "named", snapMessage(srv, testSlice2)) {
		t.Fatalf("the re-send sent no create message under the round's " +
			"trace id")
	}
	if got := node.fileOf(anaPath(testNqn, 2)); got != "1" {
		t.Fatalf("after the re-send the snapshot's namespace has "+
			"ana_grpid %q, want 1", got)
	}
	round, resent = resendRound(t, srv, held, "after")
	assertCodeZero(t, "the read after the re-send", round)
	if resent != nil {
		t.Errorf("the round after the re-send re-sent the request")
	}
	assertNothingRunsBetweenCalls(t, node, "named", "after")
}

// TestASnapshotWhoseCreateKeepsFailingIsResentEveryOtherRound: the same
// namespace over a snapshot whose create_snap the pool keeps refusing. Each
// read names the snapshot (snapshotDue), the converge's reply names nothing
// for it, so the worker re-sends every second round at most (dnv-worker.md
// RW4), and each re-send suspends the origin's raid0 once and sends the
// message once (CN14). Nothing runs between the worker's calls. A pool
// whose row does not read OK holds the name off, so a pool that reads
// needs_check or failed costs nothing.
func TestASnapshotWhoseCreateKeepsFailingIsResentEveryOtherRound(
	t *testing.T,
) {
	srv, node := newTestServer(t)
	message := "message " + poolNameOf(srv, testSlice2) + " 0 create_snap"
	node.failCmdAlways[message] = "device-mapper: message ioctl failed: " +
		"No space left on device"
	syncupSnapshot(t, srv, node, snapNsOpts(3))
	origin := raid0Name(srv, testTd)
	suspend := "cmd dmsetup suspend " + origin

	held := cntlrReq(snapNsOpts(3))
	node.Reset()
	node.forgetTraces()
	var own []string
	resends, last := 0, -2
	const rounds = 8
	for i := 0; i < rounds; i++ {
		id := fmt.Sprintf("round-%d", i)
		own = append(own, id)
		// A round right after a re-send answers from that converge's
		// observation, as the worker's next round does.
		var round *pb.CheckCntlrReply
		var resent *pb.SyncupCntlrReply
		if last == i-1 {
			round, resent = resendRoundOnly(t, srv, held, id)
		} else {
			round, resent = resendRound(t, srv, held, id)
		}
		if resent == nil {
			continue
		}
		if last == i-1 {
			t.Errorf("%s re-sent right after the re-send of the round "+
				"before it", id)
		}
		last = i
		resends++
		assertRowVerdict(t, round.GetAgentReply(), id,
			"enumeration failed: "+snapDueName())
		assertCodeZero(t, id+"'s re-send", resent)
		if n := len(tracedCalls(node, id, suspend)); n != 1 {
			t.Errorf("%s's re-send suspended the origin's raid0 %d "+
				"times, want once", id, n)
		}
		if n := len(tracedCalls(node, id, snapMessage(srv,
			testSlice2))); n != 1 {
			t.Errorf("%s's re-send sent %d create messages, want one",
				id, n)
		}
	}
	if resends != rounds/2 {
		t.Errorf("%d re-sends over %d rounds, want one every second round",
			resends, rounds)
	}
	assertNothingRunsBetweenCalls(t, node, own...)

	// A pool whose row does not read OK holds the name off.
	poolDm := poolNameOf(srv, testSlice2)
	rowSuspendDm(t, node, poolDm)
	node.mu.Lock()
	node.failCmdAlways["dmsetup resume "+poolDm] =
		"device-mapper: resume ioctl failed"
	node.mu.Unlock()
	refreshTestCntlr(t, srv)
	round := roundCntlr(srv, "pool-suspended", 3)
	if row := round.GetCntlrInfo().GetSliceIdToDmPool()[testSlice2]; row.
		GetStatus() == pb.ResStatus_RES_STATUS_OK {
		t.Fatalf("fixture is wrong: the suspended pool's row reads OK")
	}
	assertRowNotNamed(t, round.GetAgentReply(), "a read over a pool that "+
		"does not read OK", fmt.Sprintf("snapshot td %#x", testSnapTd))
}

// TestAPlannedParkWhoseLoadFailsIsNamedAndResent: a namespace suspended by
// its record (Namespace.suspended) wants its ns-dev parked on the td's
// dm-error (CN16), and the park's load fails, in the pre-step and in the
// build alike: the ns-dev stays on the raid0, and both channels name it
// (CN30). The round that names it brings a re-send at the same revision
// (dnv-worker.md RW4), which fails alike while the fault stands, and
// nothing runs between the worker's calls; the re-send after the fault
// parks the ns-dev under the round's trace id, and the read after it names
// nothing.
func TestAPlannedParkWhoseLoadFailsIsNamedAndResent(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	nsDev := nsDevName(srv, testNs)
	errNo := devNoOf(node, errorName(srv, testTd))
	park := "dmsetup reload " + nsDev + " --table " + nsDevTableOn(errNo)
	node.failCmdAlways[park] = "device-mapper: reload ioctl failed"
	held := cntlrReq(reqOpts{revision: 3, primary: true, suspended: true})

	node.Reset()
	node.forgetTraces()
	reply, err := srv.SyncupCntlr(context.Background(), held)
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if !node.hasCall("cmd " + park) {
		t.Fatalf("the case is vacuous: the converge did not try the park")
	}
	raid0No := devNoOf(node, raid0Name(srv, testTd))
	if got := node.dms[nsDev].table; got != nsDevTableOn(raid0No) {
		t.Fatalf("the case is vacuous: the ns-dev is %q, want it still on "+
			"the raid0", got)
	}
	assertAna(t, node, testNqn, "3", "the failed park")
	assertCodeNames(t, "the failed park", reply, nsDev)
	round, resent := resendRound(t, srv, held, "with-the-fault")
	assertCodeNames(t, "the read with the fault", round, nsDev)
	if resent == nil {
		t.Fatalf("the round that names the ns-dev brought no re-send")
	}
	assertCodeNames(t, "the re-send with the fault", resent, nsDev)
	assertNothingRunsBetweenCalls(t, node, "with-the-fault")

	delete(node.failCmdAlways, park)
	node.Reset()
	round, resent = resendRound(t, srv, held, "after-the-fault")
	assertCodeNames(t, "the read after the fault", round, nsDev)
	if resent == nil {
		t.Fatalf("the round after the fault brought no re-send")
	}
	assertParked(t, srv, node, testNs, testTd, "the re-send after the fault")
	if !tracedCall(node, "after-the-fault", park) {
		t.Fatalf("the park did not run under the round's trace id")
	}
	assertAna(t, node, testNqn, "3", "the parked namespace")
	round, resent = resendRound(t, srv, held, "after-the-re-send")
	assertCodeZero(t, "the read after the re-send", round)
	if resent != nil {
		t.Fatalf("the read after the re-send brought another one")
	}
}

// TestADeferredNamespaceWhoseObjectStepFailsIsNotNamed: while a leg of the
// data group still provisions, the td and its namespace are deferred
// ([D15]), and a failed step of the namespace's object reads ERROR on its
// row, beside an ns-dev that reads PROVISIONING. No channel names it (CN30):
// the row rests on an ns-dev that is not OK, and the side's provisioned
// flip raises the revision, so the converge it brings (dnv-worker.md RW18)
// steps the namespace again. Nothing runs between the worker's calls
// meanwhile.
func TestADeferredNamespaceWhoseObjectStepFailsIsNotNamed(t *testing.T) {
	srv, node := newTestServer(t)
	nsDir := agent.NvmetRoot + "/subsystems/" + testNqn + "/namespaces/1"
	mkdir := "mkdir -p " + nsDir
	node.failCmdAlways[mkdir] = "mkdir: refused"
	held := cntlrReq(reqOpts{revision: 2, primary: true,
		unprovisionedDataLeg: true})
	if _, err := srv.SyncupCn(context.Background(),
		cnReq(2, true)); err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(), held)
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if !node.hasCall("cmd " + mkdir) {
		t.Fatalf("the case is vacuous: the converge did not step the " +
			"namespace's object")
	}
	info := reply.GetCntlrInfo()
	assertProvisioning(t, info.GetNsIdToDmLinear()[testNs],
		"the deferred ns-dev")
	assertRowStatus(t, info.GetNsIdToNamespace()[testNs],
		pb.ResStatus_RES_STATUS_ERROR, "the failed namespace step")
	assertCodeZero(t, "the converge", reply)

	node.Reset()
	node.forgetTraces()
	own := []string{"deferred-1", "deferred-2"}
	for _, id := range own {
		round, resent := resendRound(t, srv, held, id)
		assertCodeZero(t, id, round)
		if resent != nil {
			t.Fatalf("%s brought a re-send", id)
		}
	}
	assertNothingRunsBetweenCalls(t, node, own...)
	assertNoCall(t, node, "cmd "+mkdir)

	// The provisioned flip: a new revision, whose converge steps the
	// namespace with the fault gone.
	delete(node.failCmdAlways, mkdir)
	node.Reset()
	reply, err = srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 3, primary: true}))
	if err != nil {
		t.Fatalf("the provisioned flip: %v", err)
	}
	if !node.hasCall("cmd " + mkdir) {
		t.Fatalf("the flip's converge did not step the namespace")
	}
	assertOk(t, reply.GetCntlrInfo().GetNsIdToNamespace()[testNs],
		"the namespace after the flip")
	assertCodeZero(t, "the flip's converge", reply)
}

// TestAnUncreatedSnapshotOverAPoolThatTakesNoMessageIsNotNamed: a read
// names an uncreated snapshot only while the converge's pre-pass would send
// its create message (readPoolTakesMessages, CN14), so it names nothing,
// and no converge suspends the origin's raid0 or sends the message, where
// the slice's pool takes none: a pool dm-thin holds read-only, for want of
// metadata space ("ro") or for a check it needs, and a pool whose grown
// metadata concat a late leg left unbuilt, whose own row reads OK on a read
// since the old pool's table stands. Nor is a created snapshot named, whose
// thin device a slice lacks: the pre-pass messages no created td.
func TestAnUncreatedSnapshotOverAPoolThatTakesNoMessageIsNotNamed(
	t *testing.T,
) {
	snapNamed := fmt.Sprintf("snapshot td %#x", testSnapTd)
	for _, tc := range []struct {
		name string
		// arm sets the slice's pool after a converge whose create of the
		// snapshot's id in slice 2 failed once.
		arm func(t *testing.T, srv *CnAgentServer, node *fakeNode)
	}{
		{"read-only for want of metadata space", func(t *testing.T,
			srv *CnAgentServer, node *fakeNode) {
			node.mu.Lock()
			node.dms[poolNameOf(srv, testSlice2)].poolMode = "ro"
			node.mu.Unlock()
		}},
		{"needs_check", func(t *testing.T, srv *CnAgentServer,
			node *fakeNode) {
			node.mu.Lock()
			node.dms[poolNameOf(srv, testSlice2)].poolNeedsCheck = true
			node.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			message := "message " + poolNameOf(srv, testSlice2) +
				" 0 create_snap"
			node.failCmd[message] = "device-mapper: message ioctl failed: " +
				"Connection timed out"
			syncupSnapshot(t, srv, node, snapNsOpts(3))
			tc.arm(t, srv, node)
			held := cntlrReq(snapNsOpts(3))
			node.Reset()
			node.forgetTraces()
			round := rowRound(t, srv, 3)
			assertRowNotNamed(t, round.GetAgentReply(), "the read",
				snapNamed)
			// A converge of the held request sends nothing to that pool.
			if _, err := srv.SyncupCntlr(traced("converge"),
				held); err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			assertNoCall(t, node, snapMessage(srv, testSlice2))
			assertNoCall(t, node, "cmd dmsetup suspend "+
				raid0Name(srv, testTd))
			assertNothingRunsBetweenCalls(t, node, "converge")
		})
	}

	t.Run("a metadata grow over a late leg", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
		newLeg := testMetaLeg + 0x100
		node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp, newLeg,
			testCn), testIp2, testSvcId2, "non-optimized")
		held := cntlrReq(reqOpts{revision: 3, primary: true,
			tds: snapTds(), leftoverOk: true})
		slice := held.GetIdToSlice()[fmt.Sprintf(common.IdKeyFmt, testSlice)]
		slice.MetaGrpList = append(slice.MetaGrpList, &pb.Group{
			GrpId:      testMetaGrp + 0x100,
			ExtCnt:     1,
			MetaBlocks: 1,
			DataBlocks: 63,
			LegList: []*pb.Leg{legOf(newLeg,
				sideOf(testMetaSide+0x200, testIp2, testSvcId2))},
		})
		node.Reset()
		if _, err := srv.SyncupCntlr(context.Background(), held); err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		node.Reset()
		node.forgetTraces()
		var own []string
		for i := 0; i < 4; i++ {
			id := fmt.Sprintf("round-%d", i)
			own = append(own, id)
			round, _ := resendRound(t, srv, held, id)
			if row := round.GetCntlrInfo().GetSliceIdToDmPool()[testSlice]; row.
				GetStatus() != pb.ResStatus_RES_STATUS_OK {
				t.Fatalf("fixture is wrong: %s reads the pool %v, want OK "+
					"over the old table", id, row)
			}
			assertRowNotNamed(t, round.GetAgentReply(), id, snapNamed)
		}
		assertNoCall(t, node, "create_snap")
		assertNothingRunsBetweenCalls(t, node, own...)
	})

	t.Run("a created snapshot whose thin device a slice lacks",
		func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupSnapshot(t, srv, node, reqOpts{revision: 3, primary: true,
				twoSlices: true, tds: snapTds()})
			created := snapTds()
			created[1].Created = true
			if _, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 4, primary: true,
					twoSlices: true, tds: created})); err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			rowDropDm(node, thinNameOf(srv, testSnapTd, testSlice2))
			round := rowRound(t, srv, 4)
			assertRowNotNamed(t, round.GetAgentReply(), "the read",
				snapNamed)
		})
}

// TestAnUncreatedSnapshotIsMadeWhereThePoolTakesTheMessage: after a
// converge whose create of the snapshot's id in slice 2 failed once, a read
// names the snapshot, and the one re-send that name brings suspends the
// origin's raid0 once and sends the message under the round's trace id,
// where slice 2's pool takes it (CN14): a pool out of data space, whose
// mode dm-thin still sends messages to, and a pool built over a metadata
// concat whose block-layer read-only flag the converge fails to clear —
// the converge's concat row reads that failure, but it builds the pool
// over the concat (poolReady), and the read's concat row does not read the
// flag. A data concat a read finds suspended keeps the snapshot's name off
// (readPoolTakesMessages): the read names the concat, and the re-send that
// name brings resumes it and sends the message.
func TestAnUncreatedSnapshotIsMadeWhereThePoolTakesTheMessage(t *testing.T) {
	snapNamed := fmt.Sprintf("snapshot td %#x", testSnapTd)
	for _, tc := range []struct {
		name string
		arm  func(t *testing.T, srv *CnAgentServer, node *fakeNode,
			sp *slicePlan)
		// snapNamed says whether the first read names the snapshot.
		snapNamed bool
	}{
		{"a pool out of data space", func(t *testing.T, srv *CnAgentServer,
			node *fakeNode, sp *slicePlan) {
			node.mu.Lock()
			node.dms[sp.poolFinalName].poolMode = "out_of_data_space"
			node.mu.Unlock()
		}, true},
		{"a metadata concat whose read-only clear fails", func(t *testing.T,
			srv *CnAgentServer, node *fakeNode, sp *slicePlan) {
			rowSetroDm(t, node, sp.poolMetaName)
			node.mu.Lock()
			node.failCmdAlways["blockdev --setrw /dev/mapper/"+
				sp.poolMetaName] = "blockdev: ioctl error on BLKROSET: " +
				"Permission denied"
			node.mu.Unlock()
		}, true},
		{"a suspended data concat", func(t *testing.T, srv *CnAgentServer,
			node *fakeNode, sp *slicePlan) {
			rowSuspendDm(t, node, sp.poolDataName)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			message := "message " + poolNameOf(srv, testSlice2) +
				" 0 create_snap"
			node.failCmd[message] = "device-mapper: message ioctl failed: " +
				"Connection timed out"
			syncupSnapshot(t, srv, node, snapNsOpts(3))
			held := cntlrReq(snapNsOpts(3))
			tc.arm(t, srv, node, newCntlrPlan(srv.nf, held).
				sliceById[testSlice2])
			node.Reset()
			node.forgetTraces()
			round, resent := resendRound(t, srv, held, "named")
			if tc.snapNamed {
				assertCodeNames(t, "the read", round, snapNamed)
			} else {
				assertRowNotNamed(t, round.GetAgentReply(), "the read",
					snapNamed)
			}
			if resent == nil {
				t.Fatalf("the round brought no re-send: %v",
					round.GetAgentReply())
			}
			if n := len(tracedCalls(node, "named",
				snapMessage(srv, testSlice2))); n != 1 {
				t.Fatalf("the re-send sent %d create messages, want one", n)
			}
			if n := len(tracedCalls(node, "named", "cmd dmsetup suspend "+
				raid0Name(srv, testTd))); n != 1 {
				t.Fatalf("the re-send suspended the origin's raid0 %d "+
					"times, want once", n)
			}
			round, _ = resendRound(t, srv, held, "after")
			assertRowNotNamed(t, round.GetAgentReply(), "the read after the "+
				"re-send", snapNamed)
			assertNothingRunsBetweenCalls(t, node, "named", "after")
		})
	}
}

// TestAnUncreatedSnapshotWaitsForItsPoolsReadOnlyClear: slice 2's pool
// reads read-only by the block layer's flag, whose clear the converge's
// pool step keeps failing (ensurePool), so the pass builds no pool there and
// sends no create message, and its own reply names the pool. A read, whose
// pool row does not read the flag, reads it from the pool device and names
// no snapshot (readPoolTakesMessages), so no re-send comes for a message no
// converge would send. Once the clear goes through, the converge the pool's
// name brings clears the flag and sends the message under its trace id. A
// pool built and flagged read-only would take the message all the same —
// dm messages are ioctls, which the flag does not refuse — but the
// converge's step does not build it for the pass.
func TestAnUncreatedSnapshotWaitsForItsPoolsReadOnlyClear(t *testing.T) {
	srv, node := newTestServer(t)
	message := "message " + poolNameOf(srv, testSlice2) + " 0 create_snap"
	node.failCmd[message] = "device-mapper: message ioctl failed: " +
		"Connection timed out"
	syncupSnapshot(t, srv, node, snapNsOpts(3))
	held := cntlrReq(snapNsOpts(3))
	pool := poolNameOf(srv, testSlice2)
	rowSetroDm(t, node, pool)
	clear := "blockdev --setrw /dev/mapper/" + pool
	node.mu.Lock()
	node.failCmdAlways[clear] = "blockdev: ioctl error on BLKROSET: " +
		"Permission denied"
	node.mu.Unlock()
	node.Reset()
	node.forgetTraces()
	reply, err := srv.SyncupCntlr(traced("refused"), held)
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	assertCodeNames(t, "the converge whose clear fails", reply, pool)
	assertNoCall(t, node, snapMessage(srv, testSlice2))
	round := rowRound(t, srv, 3)
	assertRowNotNamed(t, round.GetAgentReply(), "the read",
		fmt.Sprintf("snapshot td %#x", testSnapTd))
	node.mu.Lock()
	delete(node.failCmdAlways, clear)
	node.mu.Unlock()
	if _, err := srv.SyncupCntlr(traced("cleared"), held); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if n := len(tracedCalls(node, "cleared", snapMessage(srv,
		testSlice2))); n != 1 {
		t.Fatalf("the converge after the clear sent %d create messages, "+
			"want one", n)
	}
	round = rowRound(t, srv, 3)
	assertRowNotNamed(t, round.GetAgentReply(), "the read after it",
		fmt.Sprintf("snapshot td %#x", testSnapTd))
	assertNothingRunsBetweenCalls(t, node, "refused", "cleared")
}
