package cnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The ANA rule's condition on what the pass did (cnagent.md CN16, CN17): a
// namespace, or a transfer, moves to the optimized group only in a converge
// that put its device where the desired state wants it, and one parked while
// its rules want a serving device waits inaccessible, so a host queues its IO
// rather than reads IO errors. Both channels compare the namespace's group
// with the group that reading wants (CN28).

// nsDevTableOn is the fixture ns-dev's dm-linear over the device devNo.
func nsDevTableOn(devNo string) string {
	return agent.LinearTable(testTdSize/512, devNo, 0)
}

// devNoOf is the number the fake gave a dm device.
func devNoOf(node *fakeNode, name string) string {
	return node.devNo["/dev/mapper/"+name]
}

// assertAna is the fixture namespace's live ana_grpid.
func assertAna(t *testing.T, node *fakeNode, nqn, want, label string) {
	t.Helper()
	if got := node.fileOf(anaPath(nqn, 1)); got != want {
		t.Fatalf("%s: ana_grpid is %q, want %q", label, got, want)
	}
}

// assertRowStatus is a row's status, whatever its details.
func assertRowStatus(
	t *testing.T,
	info *pb.ResInfo,
	want pb.ResStatus,
	label string,
) {
	t.Helper()
	if info.GetStatus() != want {
		t.Fatalf("%s: status %v, details %q, want %v",
			label, info.GetStatus(), info.GetDetails(), want)
	}
}

// failoverMdAbsent is how a cntlr's read names the absent array of the
// fixture slice's meta group (isMeta) or data group (CN30).
func failoverMdAbsent(srv *CnAgentServer, isMeta bool) string {
	return agent.LeftoverKindMdAbsent + ":" + srv.nf.MdPath(
		srv.nf.CnMdDevName(testCluster, testCn, testSp, 0, 0, isMeta))
}

// TestPromotionAheadOfTheSidesWaitsInaccessible pins the failover window of
// a promotion whose converge cannot build the raid0 (cnagent.md CN16;
// architecture.md, Failover): the namespace stays inaccessible — no
// optimized write at all — while its ns-dev stays parked on the td's
// dm-error. The ns-dev row reads ERROR and the namespace row OK, on the
// converge and on a check round alike. "late members" is the promotion that
// came before the sides' switch-over ([D16]): no leg serves, so a read
// names nothing and no re-send comes; once the sides flip, a read names
// each group's absent array (CN12, CN30). "no late member" is a raid0 whose
// create fails with every leg available: both channels name the raid0, and
// each re-send while the fault stands keeps waiting. Either way the re-send
// the naming round brings (dnv-worker.md RW4) builds the raid0, reloads the
// ns-dev onto it under the round's trace id and only then moves the
// namespace optimized, and the read after it names nothing.
func TestPromotionAheadOfTheSidesWaitsInaccessible(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fault arms the cause and returns what removes it.
		fault func(srv *CnAgentServer, node *fakeNode) func()
		// late: the cause is the legs' paths, which a read does not name
		// until they serve.
		late bool
	}{
		{"late members", func(srv *CnAgentServer, node *fakeNode) func() {
			set := func(state string) {
				for _, legId := range []uint64{testMetaLeg, testDataLeg} {
					node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp,
						legId, testCn), testIp, testSvcId, state)
				}
			}
			set("non-optimized")
			return func() { set("optimized") }
		}, true},
		{"no late member", func(srv *CnAgentServer, node *fakeNode) func() {
			key := "dmsetup create " + raid0Name(srv, testTd) + " "
			node.failCmdAlways[key] = "device-mapper: create ioctl failed"
			return func() { delete(node.failCmdAlways, key) }
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			opts := func(revision uint64, primary bool) reqOpts {
				return reqOpts{revision: revision, primary: primary,
					raid1: true}
			}
			syncupBoth(t, srv, opts(2, true))
			if _, err := srv.SyncupCntlr(ctx,
				cntlrReq(opts(3, false))); err != nil {
				t.Fatalf("demote: %v", err)
			}
			assertAna(t, node, testNqn, "3", "the standby")
			disarm := tc.fault(srv, node)
			ana := "writedirect " + anaPath(testNqn, 1) + "="
			nsDev := nsDevName(srv, testNs)

			node.Reset()
			held := cntlrReq(opts(4, true))
			reply, err := srv.SyncupCntlr(ctx, held)
			if err != nil {
				t.Fatalf("promote: %v", err)
			}
			// The step the fault stopped is named (CN30).
			if !acceptedCode(reply.GetAgentReply(), true) {
				t.Fatalf("rejected: %v", reply.GetAgentReply())
			}
			if _, ok := node.dms[raid0Name(srv, testTd)]; ok {
				t.Fatalf("the case is vacuous: the promotion built the raid0")
			}
			assertNoCall(t, node, ana+"1")
			assertAna(t, node, testNqn, "3", "the promotion")
			assertParked(t, srv, node, testNs, testTd, "the promotion")
			info := reply.GetCntlrInfo()
			assertErrorDetails(t, info.GetNsIdToDmLinear()[testNs],
				"is not there", "the converge's ns-dev row")
			assertOk(t, info.GetNsIdToNamespace()[testNs],
				"the converge's namespace row")
			probed := checkRound(t, srv, 4)
			assertErrorDetails(t, probed.GetNsIdToDmLinear()[testNs],
				"is not there", "the check round's ns-dev row")
			assertOk(t, probed.GetNsIdToNamespace()[testNs],
				"the check round's namespace row")

			// While the cause stands: a read before the flip names
			// nothing, since no member serves; a raid0 whose create fails
			// is named, and its re-send waits again.
			node.Reset()
			round, resent := resendRound(t, srv, held, "with-the-cause")
			if tc.late {
				assertCodeZero(t, "the read before the flip", round)
				if resent != nil {
					t.Fatalf("the read before the flip brought a re-send")
				}
			} else {
				assertCodeNames(t, "the read with the fault", round,
					raid0Name(srv, testTd))
				if resent == nil {
					t.Fatalf("the round that names the raid0 brought no " +
						"re-send")
				}
				assertCodeNames(t, "the re-send with the fault", resent,
					raid0Name(srv, testTd))
			}
			assertNoCall(t, node, ana)
			assertAna(t, node, testNqn, "3", "a round with the cause")

			disarm()
			node.Reset()
			round, resent = resendRound(t, srv, held, "after-the-cause")
			if tc.late {
				assertCodeNames(t, "the read after the flip", round,
					failoverMdAbsent(srv, true),
					failoverMdAbsent(srv, false))
			} else {
				assertCodeNames(t, "the read after the fault", round,
					raid0Name(srv, testTd))
			}
			if resent == nil {
				t.Fatalf("the round that names the stack brought no re-send")
			}
			assertCodeZero(t, "the re-send", resent)
			raid0No := devNoOf(node, raid0Name(srv, testTd))
			if raid0No == "" {
				t.Fatalf("the re-send after the cause built no raid0")
			}
			reload := "dmsetup reload " + nsDev + " --table " +
				nsDevTableOn(raid0No)
			assertOrder(t, node, "cmd "+reload, ana+"1")
			if !tracedCall(node, "after-the-cause", reload) {
				t.Fatalf("the reload did not run under the round's trace id")
			}
			assertAna(t, node, testNqn, "1", "the re-send after the cause")
			round, resent = resendRound(t, srv, held, "after-the-re-send")
			assertCodeZero(t, "the read after the re-send", round)
			if resent != nil {
				t.Fatalf("the read after the re-send brought another one")
			}
			built := probedCntlrInfo(t, srv)
			assertOk(t, built.GetNsIdToDmLinear()[testNs], "built ns-dev")
			assertOk(t, built.GetNsIdToNamespace()[testNs], "built namespace")
		})
	}
}

// TestAFailedNsDevStepMovesOnlyAParkedNamespace pins the two halves of a
// failed ns-dev step on a serving primary (cnagent.md CN16). A failed read
// never moves an optimized namespace back: its ns-dev may still serve, and
// the move would pause its hosts for nothing. An ns-dev found parked on the
// td's dm-error while the rules want the raid0 moves its namespace
// inaccessible, so its hosts queue rather than read IO errors; both
// channels then read its namespace row OK. Either way the converge's reply
// names the ns-dev (CN30), so the round that answers from its observation
// brings a re-send (dnv-worker.md RW4), which fails the same way while the
// fault stands; the re-send after the fault reloads the ns-dev onto the
// raid0 and moves the namespace optimized, and the read after it names
// nothing.
func TestAFailedNsDevStepMovesOnlyAParkedNamespace(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fault arms the failure and returns what removes it.
		fault func(srv *CnAgentServer, node *fakeNode) func()
		// ana is the group the failed pass leaves.
		ana string
	}{
		{"the table read fails",
			func(srv *CnAgentServer, node *fakeNode) func() {
				key := "dmsetup table " + nsDevName(srv, testNs)
				node.failCmdAlways[key] = "device-mapper: table ioctl failed"
				return func() { delete(node.failCmdAlways, key) }
			}, "1"},
		{"found parked, its reload fails",
			func(srv *CnAgentServer, node *fakeNode) func() {
				nsDev := nsDevName(srv, testNs)
				node.dms[nsDev].table = nsDevTableOn(
					devNoOf(node, errorName(srv, testTd)))
				key := "dmsetup reload " + nsDev
				node.failCmdAlways[key] = "device-mapper: reload ioctl failed"
				return func() { delete(node.failCmdAlways, key) }
			}, "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			assertAna(t, node, testNqn, "1", "the fixture")
			disarm := tc.fault(srv, node)
			ana := "writedirect " + anaPath(testNqn, 1) + "="

			node.Reset()
			held := cntlrReq(reqOpts{revision: 3, primary: true})
			reply, err := srv.SyncupCntlr(ctx, held)
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			assertCodeNames(t, "the failed pass", reply,
				nsDevName(srv, testNs))
			assertAna(t, node, testNqn, tc.ana, "the failed pass")
			assertNoCall(t, node, ana+"1")
			if tc.ana == "1" {
				assertNoCall(t, node, ana)
			}
			info := reply.GetCntlrInfo()
			assertRowStatus(t, info.GetNsIdToDmLinear()[testNs],
				pb.ResStatus_RES_STATUS_ERROR, "the converge's ns-dev row")
			assertOk(t, info.GetNsIdToNamespace()[testNs],
				"the converge's namespace row")
			if tc.ana == "3" {
				// The check round reads the parked table and wants
				// inaccessible, as the converge did.
				probed := checkRound(t, srv, 3)
				assertOk(t, probed.GetNsIdToNamespace()[testNs],
					"the check round's namespace row")
			}

			// The round that answers from the converge's observation
			// re-sends; with the fault standing the re-send fails alike.
			round, resent := resendRoundOnly(t, srv, held, "with-the-fault")
			assertCodeNames(t, "the round after the failed pass", round,
				nsDevName(srv, testNs))
			if resent == nil {
				t.Fatalf("the round that names the ns-dev brought no re-send")
			}
			assertCodeNames(t, "the re-send with the fault", resent,
				nsDevName(srv, testNs))
			assertNoCall(t, node, ana+"1")

			disarm()
			node.Reset()
			round, resent = resendRoundOnly(t, srv, held, "after-the-fault")
			if resent == nil {
				t.Fatalf("the round after the fault brought no re-send: %v",
					round.GetAgentReply())
			}
			assertCodeZero(t, "the re-send after the fault", resent)
			raid0No := devNoOf(node, raid0Name(srv, testTd))
			if got := node.dms[nsDevName(srv, testNs)].table; got !=
				nsDevTableOn(raid0No) {
				t.Fatalf("the ns-dev is %q after the fault, want the raid0",
					got)
			}
			assertAna(t, node, testNqn, "1", "the re-send after the fault")
			if tc.ana == "3" {
				reload := "dmsetup reload " + nsDevName(srv, testNs) +
					" --table " + nsDevTableOn(raid0No)
				assertOrder(t, node, "cmd "+reload, ana+"1")
				if !tracedCall(node, "after-the-fault", reload) {
					t.Fatalf("the reload did not run under the round's " +
						"trace id")
				}
			}
			round, resent = resendRound(t, srv, held, "after-the-re-send")
			assertCodeZero(t, "the read after the re-send", round)
			if resent != nil {
				t.Fatalf("the read after the re-send brought another one")
			}
		})
	}
}

// TestAFailedNsDevStepAtAPromotionKeepsInaccessible is the other half of a
// failed ns-dev step (cnagent.md CN16): a step that cannot read the ns-dev's
// table does not know it is parked, so its namespace keeps the group it has
// — at a promotion that came before the sides' switch-over, the standby's
// inaccessible group, and never optimized over a park the step could not
// see. While no leg serves, a read names nothing, since no converge would
// build the stack (CN30); once the sides flip and the table reads again, a
// read names the groups' absent arrays, and the re-send that round brings
// (dnv-worker.md RW4) reloads the ns-dev onto the raid0 and only then moves
// the namespace optimized.
func TestAFailedNsDevStepAtAPromotionKeepsInaccessible(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	opts := func(revision uint64, primary bool) reqOpts {
		return reqOpts{revision: revision, primary: primary, raid1: true}
	}
	syncupBoth(t, srv, opts(2, true))
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(opts(3, false))); err != nil {
		t.Fatalf("demote: %v", err)
	}
	assertAna(t, node, testNqn, "3", "the standby")
	setLegs := func(state string) {
		for _, legId := range []uint64{testMetaLeg, testDataLeg} {
			node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp,
				legId, testCn), testIp, testSvcId, state)
		}
	}
	setLegs("non-optimized")
	nsDev := nsDevName(srv, testNs)
	tableKey := "dmsetup table " + nsDev
	node.failCmdAlways[tableKey] = "device-mapper: table ioctl failed"
	ana := "writedirect " + anaPath(testNqn, 1) + "="

	node.Reset()
	held := cntlrReq(opts(4, true))
	if _, err := srv.SyncupCntlr(ctx, held); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if _, ok := node.dms[raid0Name(srv, testTd)]; ok {
		t.Fatalf("the case is vacuous: the promotion built the raid0")
	}
	assertParked(t, srv, node, testNs, testTd, "the promotion")
	assertNoCall(t, node, ana+"1")
	assertAna(t, node, testNqn, "3", "the promotion")
	round, resent := resendRound(t, srv, held, "before-the-flip")
	assertCodeZero(t, "the read before the flip", round)
	if resent != nil {
		t.Fatalf("the read before the flip brought a re-send")
	}

	delete(node.failCmdAlways, tableKey)
	setLegs("optimized")
	node.Reset()
	round, resent = resendRound(t, srv, held, "after-the-flip")
	assertCodeNames(t, "the read after the flip", round,
		failoverMdAbsent(srv, true), failoverMdAbsent(srv, false))
	if resent == nil {
		t.Fatalf("the round that names the absent arrays brought no re-send")
	}
	raid0No := devNoOf(node, raid0Name(srv, testTd))
	if raid0No == "" {
		t.Fatalf("the re-send after the flip built no raid0")
	}
	reload := "dmsetup reload " + nsDev + " --table " + nsDevTableOn(raid0No)
	assertOrder(t, node, "cmd "+reload, ana+"1")
	if !tracedCall(node, "after-the-flip", reload) {
		t.Fatalf("the reload did not run under the round's trace id")
	}
	assertAna(t, node, testNqn, "1", "the re-send after the flip")
	round, resent = resendRound(t, srv, held, "after-the-re-send")
	assertCodeZero(t, "the read after the re-send", round)
	if resent != nil {
		t.Fatalf("the read after the re-send brought another one")
	}
}

// TestAHeldNsDevWithAnUnreadableDmErrorReadsTheSameOnBothChannels pins the
// one case where the ANA move and the namespace row part (cnagent.md CN16,
// CN18, CN28): an ns-dev held over a dm-clone the plan dropped, whose td's
// dm-error number cannot be read, counts as parked for the move, so its
// namespace goes inaccessible for the pass, a wait and never an IO error;
// the namespace row reads the ns-dev's live table, which maps no dm-error it
// can name, and wants the desired group. Both channels decide the row by
// that one rule, so it reads the same ERROR on the converge and on a check
// round. Both channels name the held ns-dev (CN30), so each round brings a
// re-send (dnv-worker.md RW4); once the dm-error's number reads, the
// re-send's converge takes the dropped dm-clone out from under the ns-dev,
// reloads the ns-dev onto the raid0 and moves the namespace optimized, and
// the read after it names nothing.
func TestAHeldNsDevWithAnUnreadableDmErrorReadsTheSameOnBothChannels(
	t *testing.T,
) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		clones: []*pb.Clone{cloneOf()}})
	nsDev := nsDevName(srv, testNs)
	onClone := nsDevTableOn(devNoOf(node, cloneName(srv, testClone)))
	if got := node.dms[nsDev].table; got != onClone {
		t.Fatalf("fixture: the ns-dev %q does not serve through the "+
			"dm-clone %q", got, onClone)
	}
	assertAna(t, node, testNqn, "1", "the fixture")
	// The dm-error's number reads back with no device number, on a
	// converge's own `dmsetup info` and in the listing a cntlr's read takes
	// alike: P2 cannot park the ns-dev, L3 cannot remove the dm-clone under
	// it, and the build holds it.
	errNo := node.devNo["/dev/mapper/"+errorName(srv, testTd)]
	node.devNo["/dev/mapper/"+errorName(srv, testTd)] = "unreadable"

	node.Reset()
	held := cntlrReq(reqOpts{revision: 3, primary: true})
	reply, err := srv.SyncupCntlr(ctx, held)
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if got := node.dms[nsDev].table; got != onClone {
		t.Fatalf("the case is vacuous: the ns-dev is %q, want it held on "+
			"the dm-clone", got)
	}
	assertAna(t, node, testNqn, "3", "the held pass")
	assertNoCall(t, node, "writedirect "+anaPath(testNqn, 1)+"=1")
	// The held namespace is wanted optimized and sits inaccessible: the
	// primary has no optimized path, so its row reads ERROR (CN28).
	row := reply.GetCntlrInfo().GetNsIdToNamespace()[testNs]
	assertRowStatus(t, row, pb.ResStatus_RES_STATUS_ERROR,
		"the converge's namespace row")
	probed := checkRound(t, srv, 3).GetNsIdToNamespace()[testNs]
	if probed.GetStatus() != row.GetStatus() ||
		probed.GetDetails() != row.GetDetails() {
		t.Fatalf("the check round reads the namespace row %v %q, the "+
			"converge %v %q", probed.GetStatus(), probed.GetDetails(),
			row.GetStatus(), row.GetDetails())
	}
	assertCodeNames(t, "the converge", reply, nsDev)
	round, resent := resendRound(t, srv, held, "held")
	assertCodeNames(t, "the read of the held ns-dev", round, nsDev)
	if resent == nil {
		t.Fatalf("the round that names the held ns-dev brought no re-send")
	}
	assertCodeNames(t, "the re-send while the number is unreadable",
		resent, nsDev)
	assertAna(t, node, testNqn, "3", "the re-send of the held ns-dev")

	node.devNo["/dev/mapper/"+errorName(srv, testTd)] = errNo
	node.Reset()
	round, resent = resendRound(t, srv, held, "readable")
	if resent == nil {
		t.Fatalf("the round after the number reads brought no re-send: %v",
			round.GetAgentReply())
	}
	raid0No := devNoOf(node, raid0Name(srv, testTd))
	if got := node.dms[nsDev].table; got != nsDevTableOn(raid0No) {
		t.Fatalf("the ns-dev is %q after the re-send, want the raid0", got)
	}
	if _, ok := node.dms[cloneName(srv, testClone)]; ok {
		t.Fatalf("the dropped dm-clone outlived the re-send")
	}
	assertAna(t, node, testNqn, "1", "the re-send after the number reads")
	// The dropped clone's meta group under the dm-clone is a leftover the
	// sweep names and removes from the top down, one re-send a layer
	// (CN21); none of those re-sends touches the ns-dev again.
	node.Reset()
	for i := 0; ; i++ {
		round, resent = resendRound(t, srv, held,
			fmt.Sprintf("after-the-re-send-%d", i))
		if resent == nil {
			assertCodeZero(t, "the read after the re-sends", round)
			break
		}
		if i == 3 {
			t.Fatalf("the reads still name %v after %d re-sends",
				round.GetAgentReply(), i+1)
		}
	}
	assertNoCall(t, node, "dmsetup reload "+nsDev)
	assertAna(t, node, testNqn, "1", "the re-sends after the hold")
}

// TestACloneBuildAtAPromotionWaitsInaccessible pins that a clone's build at a
// promotion makes hosts wait (cnagent.md CN16 rule 5, CN18): a promotion to a
// cntlr that builds the clone's dm-clone over its clone meta group, whose
// build stops short of enabling hydration, leaves the namespace inaccessible
// with its ns-dev parked — both rows OK on both channels. The converge's
// reply names the dm-clone with the enable's failure, and a read names it
// while its hydration is off (CN30), so the
// round brings a re-send of the request, whose converge enables hydration,
// puts the ns-dev on the dm-clone and only then moves the namespace
// optimized.
func TestACloneBuildAtAPromotionWaitsInaccessible(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	opts := func(revision uint64, primary bool) reqOpts {
		return reqOpts{revision: revision, primary: primary,
			clones: []*pb.Clone{cloneOf()}}
	}
	syncupBoth(t, srv, opts(2, true))
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(opts(3, false))); err != nil {
		t.Fatalf("demote: %v", err)
	}
	assertAna(t, node, testNqn, "3", "the standby")
	clone := cloneName(srv, testClone)
	enable := "dmsetup message " + clone + " 0 enable_hydration"
	node.failCmdAlways[enable] = "device-mapper: message ioctl failed"
	ana := "writedirect " + anaPath(testNqn, 1) + "="
	nsDev := nsDevName(srv, testNs)

	node.Reset()
	reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts(4, true)))
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if dm := node.dms[clone]; dm == nil || !dm.noHydration {
		t.Fatalf("the case is vacuous: the dm-clone is %+v", dm)
	}
	assertNoCall(t, node, ana)
	assertAna(t, node, testNqn, "3", "the unfinished build")
	assertParked(t, srv, node, testNs, testTd, "the unfinished build")
	info := reply.GetCntlrInfo()
	assertOk(t, info.GetNsIdToDmLinear()[testNs], "the converge's ns-dev")
	assertOk(t, info.GetNsIdToNamespace()[testNs], "the converge's namespace")
	probed := checkRound(t, srv, 4)
	assertOk(t, probed.GetNsIdToDmLinear()[testNs], "the probed ns-dev")
	assertOk(t, probed.GetNsIdToNamespace()[testNs], "the probed namespace")
	if !strings.HasPrefix(reply.GetAgentReply().GetDetails(),
		rowNamed(clone, "differs: ")) ||
		!strings.Contains(reply.GetAgentReply().GetDetails(),
			"message ioctl failed") {
		t.Fatalf("the converge replied %v, want it to name the dm-clone "+
			"with the enable's failure", reply.GetAgentReply())
	}
	round := rowRound(t, srv, 4)
	assertRowVerdict(t, round.GetAgentReply(), "the read",
		rowNamed(clone, "differs: "+detailsCloneNotHydrating))

	delete(node.failCmdAlways, enable)
	node.Reset()
	if rowResend(t, srv, cntlrReq(opts(4, true)), round) == nil {
		t.Fatalf("the round that names the dm-clone brought no re-send")
	}
	cloneNo := devNoOf(node, clone)
	assertOrder(t, node, "cmd "+enable,
		"cmd dmsetup reload "+nsDev+" --table "+nsDevTableOn(cloneNo),
		ana+"1")
	assertAna(t, node, testNqn, "1", "the finished build")
	assertOk(t, probedCntlrInfo(t, srv).GetNsIdToNamespace()[testNs],
		"the served namespace")
	assertRowVerdict(t, rowRound(t, srv, 4).GetAgentReply(),
		"the read after the re-send")
}

// TestAnUnfinishedDmCloneOnARunningPrimaryIsFinished pins CN18's finish of a
// dm-clone found with hydration off on a primary that serves through it: the
// build applies the source chunks the node holds and enables hydration, with
// no park and no removal, and the namespace stays optimized. When the enable
// fails, the ns-dev parks off the dm-clone, its namespace moved inaccessible
// before the reload. A read names the dm-clone
// while its hydration is off, and so does the reply of a converge whose
// enable failed (CN30); the re-send the round brings enables hydration,
// puts the ns-dev back on the dm-clone and only then moves the namespace
// optimized.
func TestAnUnfinishedDmCloneOnARunningPrimaryIsFinished(t *testing.T) {
	for _, tc := range []struct {
		name string
		// finishes: the enable succeeds in the pass.
		finishes bool
	}{
		{"the enable succeeds", true},
		{"the enable fails", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			opts := reqOpts{revision: 2, primary: true,
				clones: []*pb.Clone{cloneOf()}}
			syncupBoth(t, srv, opts)
			pushBitmap(t, srv, hexBytes(t, testSkipHex))
			clone := cloneName(srv, testClone)
			nsDev := nsDevName(srv, testNs)
			onClone := nsDevTableOn(devNoOf(node, clone))
			if node.dms[nsDev].table != onClone {
				t.Fatalf("the fixture ns-dev does not serve through the " +
					"dm-clone")
			}
			assertAna(t, node, testNqn, "1", "the fixture")
			// The dm-clone's hydration is off: a build that stopped before
			// CN18 step 5.
			node.dms[clone].noHydration = true
			enable := "dmsetup message " + clone + " 0 enable_hydration"
			if !tc.finishes {
				node.failCmdAlways[enable] =
					"device-mapper: message ioctl failed"
			}
			ana := "writedirect " + anaPath(testNqn, 1) + "="
			park := "cmd dmsetup reload " + nsDev + " --table " +
				nsDevTableOn(devNoOf(node, errorName(srv, testTd)))
			assertRowVerdict(t, rowRound(t, srv, 2).GetAgentReply(),
				"the read of hydration off",
				rowNamed(clone, "differs: "+detailsCloneNotHydrating))

			node.Reset()
			opts.revision = 3
			reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			assertNoCall(t, node, "cmd dmsetup remove "+clone)
			assertNoCall(t, node, "cmd dmsetup create "+clone)
			if tc.finishes {
				assertOrder(t, node,
					"discard --offset 33554432 --length 33554432",
					"cmd "+enable)
				assertNoCall(t, node, park)
				assertNoCall(t, node, ana)
				assertAna(t, node, testNqn, "1", "the finished build")
				assertRowVerdict(t, reply.GetAgentReply(), "the converge")
				assertRowVerdict(t, rowRound(t, srv, 3).GetAgentReply(),
					"the read after the build")
				return
			}
			assertOrder(t, node, "cmd "+enable, ana+"3", park)
			assertNoCall(t, node, ana+"1")
			assertAna(t, node, testNqn, "3", "the unfinished build")
			if !strings.HasPrefix(reply.GetAgentReply().GetDetails(),
				rowNamed(clone, "differs: ")) {
				t.Fatalf("the converge replied %v, want it to name the "+
					"dm-clone", reply.GetAgentReply())
			}
			delete(node.failCmdAlways, enable)
			node.Reset()
			if rowResend(t, srv, cntlrReq(opts),
				rowRoundOnly(t, srv, 3)) == nil {
				t.Fatalf("the round after the failed enable brought no " +
					"re-send")
			}
			assertOrder(t, node, "cmd "+enable,
				"cmd dmsetup reload "+nsDev+" --table "+onClone,
				ana+"1")
			assertAna(t, node, testNqn, "1", "the finished build")
			assertRowVerdict(t, rowRound(t, srv, 3).GetAgentReply(),
				"the read after the re-send")
		})
	}
}

// TestLoweringTheNoCloneLevelBuildsTheCloneOverItsGroup pins a clone after
// the level that removes the clone stacks (cnagent.md CN18, CN19): at that
// level the dm-clone goes and its clone meta group stays, the ns-dev sits on
// the td's dm-error with its namespace optimized over it on purpose (CN16
// rule 4). When the level is lowered, the dm-clone is created again over the
// group, with the hydrated count its metadata held, and the ns-dev moves
// onto it in the pass, its namespace optimized throughout. When the enable
// fails, the namespace goes inaccessible at the end of the pass, with its
// ns-dev still parked, the converge's reply names the dm-clone (CN30), and
// the namespace turns optimized in the re-send's pass that puts the ns-dev
// on the dm-clone.
func TestLoweringTheNoCloneLevelBuildsTheCloneOverItsGroup(t *testing.T) {
	for _, tc := range []struct {
		name string
		// finishes: the enable succeeds in the pass.
		finishes bool
	}{
		{"the enable succeeds", true},
		{"the enable fails", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			clones := []*pb.Clone{cloneOf()}
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				clones: clones})
			clone := cloneName(srv, testClone)
			node.setCloneHydrated(t, clone, 23)
			if _, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
				revision: 3, primary: true, clones: clones,
				level: pb.SpLevel_SP_LEVEL_NO_CLONE})); err != nil {
				t.Fatalf("no clone: %v", err)
			}
			if _, ok := node.dms[clone]; ok {
				t.Fatalf("the case is vacuous: the level kept the dm-clone")
			}
			if _, ok := node.dms[grpName(srv, testCloneGrp)]; !ok {
				t.Fatalf("the level removed the clone meta group")
			}
			assertParked(t, srv, node, testNs, testTd, "the level")
			assertAna(t, node, testNqn, "1", "the level")
			enable := "dmsetup message " + clone + " 0 enable_hydration"
			if !tc.finishes {
				node.failCmdAlways[enable] =
					"device-mapper: message ioctl failed"
			}
			ana := "writedirect " + anaPath(testNqn, 1) + "="
			nsDev := nsDevName(srv, testNs)

			node.Reset()
			reply, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
				revision: 4, primary: true, clones: clones}))
			if err != nil {
				t.Fatalf("the level lowered: %v", err)
			}
			assertNoCall(t, node, "discard --offset 0")
			if !tc.finishes {
				assertOrder(t, node, "cmd dmsetup create "+clone,
					"cmd "+enable, ana+"3")
				assertNoCall(t, node, ana+"1")
				assertAna(t, node, testNqn, "3", "the unfinished build")
				assertParked(t, srv, node, testNs, testTd,
					"the unfinished build")
				if !strings.HasPrefix(reply.GetAgentReply().GetDetails(),
					rowNamed(clone, "differs: ")) {
					t.Fatalf("the converge replied %v, want it to name "+
						"the dm-clone", reply.GetAgentReply())
				}
				delete(node.failCmdAlways, enable)
				node.Reset()
				if rowResend(t, srv, cntlrReq(reqOpts{revision: 4,
					primary: true, clones: clones}),
					rowRoundOnly(t, srv, 4)) == nil {
					t.Fatalf("the round after the failed enable brought " +
						"no re-send")
				}
				assertOrder(t, node, "cmd "+enable,
					"cmd dmsetup reload "+nsDev+" --table "+
						nsDevTableOn(devNoOf(node, clone)),
					ana+"1")
			} else {
				assertOrder(t, node, "cmd dmsetup create "+clone,
					"cmd "+enable,
					"cmd dmsetup reload "+nsDev+" --table "+
						nsDevTableOn(devNoOf(node, clone)))
				assertNoCall(t, node, ana)
				row := reply.GetCntlrInfo().GetCloneIdToDmClone()[testClone]
				if !strings.Contains(row.GetDetails(), " 23/64 ") {
					t.Fatalf("the dm-clone built again reads %q, want the "+
						"23 regions its metadata held", row.GetDetails())
				}
				assertRowVerdict(t, reply.GetAgentReply(), "the converge")
			}
			assertRowVerdict(t, rowRound(t, srv, 4).GetAgentReply(),
				"the read after the build")
			assertAna(t, node, testNqn, "1", "the finished build")
		})
	}
}

// TestLevelStepsStayOptimizedOverTheDmError pins what serves IO errors on
// purpose (cnagent.md CN16 rules 3 and 4, CN19): at the level that removes
// the thin pools, and at the one that removes the clone stacks of a td a
// clone targets, the dm-error is the device the rules select, so the
// namespace stays optimized over it — no ANA write at all — with both rows
// OK on both channels, and a read names nothing (CN30).
func TestLevelStepsStayOptimizedOverTheDmError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		level  pb.SpLevel
		clones []*pb.Clone
	}{
		{"no thin pool", pb.SpLevel_SP_LEVEL_NO_THINPOOL, nil},
		{"no clone with a clone", pb.SpLevel_SP_LEVEL_NO_CLONE,
			[]*pb.Clone{cloneOf()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				clones: tc.clones})
			assertAna(t, node, testNqn, "1", "the fixture")

			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(reqOpts{revision: 3, primary: true,
					level: tc.level, clones: tc.clones}))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			assertParked(t, srv, node, testNs, testTd, "the level step")
			assertNoCall(t, node, "writedirect "+anaPath(testNqn, 1)+"=")
			assertAna(t, node, testNqn, "1", "the level step")
			info := reply.GetCntlrInfo()
			assertOk(t, info.GetNsIdToDmLinear()[testNs], "converge ns-dev")
			assertOk(t, info.GetNsIdToNamespace()[testNs],
				"converge namespace")
			probed := checkRound(t, srv, 3)
			assertOk(t, probed.GetNsIdToDmLinear()[testNs], "probed ns-dev")
			assertOk(t, probed.GetNsIdToNamespace()[testNs],
				"probed namespace")
			assertCodeZero(t, "the read at the level",
				rowRoundOnly(t, srv, 3))
		})
	}
}

// TestTransferAtAPromotionWaitsInaccessible pins CN17's share of the rule: a
// promotion whose converge cannot build the origin's raid0 leaves the
// transfer device on the standby's error table and the transfer namespace
// inaccessible — no optimized write. Where the step reads that error table,
// the transfer namespace row reads OK on both channels; where the read of
// the device's table fails, the transfer keeps the group it has, since what
// is not known moves no transfer. While no leg serves, a read names
// nothing (CN30); once the sides flip, a read names the groups' absent
// arrays, and the re-send that round brings (dnv-worker.md RW4) builds the
// raid0, reloads the transfer device onto it and only then moves the
// transfer optimized.
func TestTransferAtAPromotionWaitsInaccessible(t *testing.T) {
	for _, tc := range []struct {
		name string
		// unreadable: every `dmsetup table` of the transfer device fails
		// until the raid0 can be built.
		unreadable bool
	}{
		{"its device read on the error table", false},
		{"its device's table unreadable", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			xfers := []*pb.Transfer{
				{XferId: testXfer, OriNqn: testNqn, OriNsIdx: 1}}
			opts := func(revision uint64, primary bool) reqOpts {
				return reqOpts{revision: revision, primary: primary,
					raid1: true, xfers: xfers}
			}
			syncupBoth(t, srv, opts(2, true))
			xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
			assertAna(t, node, xferNqn, "1", "the fixture transfer")
			if _, err := srv.SyncupCntlr(ctx,
				cntlrReq(opts(3, false))); err != nil {
				t.Fatalf("demote: %v", err)
			}
			assertAna(t, node, xferNqn, "3", "the standby's transfer")
			setLegs := func(state string) {
				for _, legId := range []uint64{testMetaLeg, testDataLeg} {
					node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp,
						legId, testCn), testIp, testSvcId, state)
				}
			}
			setLegs("non-optimized")
			xferAna := "writedirect " + anaPath(xferNqn, 1) + "="
			xfer := xferName(srv, testXfer)
			tableKey := "dmsetup table " + xfer
			if tc.unreadable {
				node.failCmdAlways[tableKey] =
					"device-mapper: table ioctl failed"
			}

			node.Reset()
			held := cntlrReq(opts(4, true))
			reply, err := srv.SyncupCntlr(ctx, held)
			if err != nil {
				t.Fatalf("promote: %v", err)
			}
			assertNoCall(t, node, xferAna+"1")
			assertAna(t, node, xferNqn, "3", "the promotion's transfer")
			want := fmt.Sprintf("0 %d error", testTdSize/512)
			if got := node.dms[xfer].table; got != want {
				t.Fatalf("the transfer device is %q, want the error "+
					"table %q", got, want)
			}
			info := reply.GetCntlrInfo()
			assertRowStatus(t, info.GetXferIdToDmLinear()[testXfer],
				pb.ResStatus_RES_STATUS_ERROR,
				"the converge's transfer device")
			if !tc.unreadable {
				assertOk(t, info.GetXferIdToNamespace()[testXfer],
					"the converge's transfer namespace")
				assertOk(t, info.GetXferIdToSubsystem()[testXfer],
					"the converge's transfer subsystem")
				probed := checkRound(t, srv, 4)
				assertOk(t, probed.GetXferIdToNamespace()[testXfer],
					"the check round's transfer namespace")
				assertOk(t, probed.GetXferIdToSubsystem()[testXfer],
					"the check round's transfer subsystem")
			}
			round, resent := resendRound(t, srv, held, "before-the-flip")
			assertCodeZero(t, "the read before the flip", round)
			if resent != nil {
				t.Fatalf("the read before the flip brought a re-send")
			}

			delete(node.failCmdAlways, tableKey)
			setLegs("optimized")
			node.Reset()
			round, resent = resendRound(t, srv, held, "after-the-flip")
			assertCodeNames(t, "the read after the flip", round,
				failoverMdAbsent(srv, true), failoverMdAbsent(srv, false))
			if resent == nil {
				t.Fatalf("the round that names the absent arrays brought " +
					"no re-send")
			}
			raid0No := devNoOf(node, raid0Name(srv, testTd))
			if raid0No == "" {
				t.Fatalf("the re-send after the flip built no raid0")
			}
			assertOrder(t, node, "cmd dmsetup reload "+xfer, xferAna+"1")
			if !tracedCall(node, "after-the-flip", "dmsetup reload "+xfer) {
				t.Fatalf("the reload did not run under the round's trace id")
			}
			if got := node.dms[xfer].table; got != nsDevTableOn(raid0No) {
				t.Fatalf("the transfer device is %q, want the raid0", got)
			}
			assertAna(t, node, xferNqn, "1", "the served transfer")
			round, resent = resendRound(t, srv, held, "after-the-re-send")
			assertCodeZero(t, "the read after the re-send", round)
			if resent != nil {
				t.Fatalf("the read after the re-send brought another one")
			}
		})
	}
}

// TestAServedTransferFoundOnAnErrorTableWaitsInaccessible is the transfer
// twin of a failed ns-dev step that finds its ns-dev parked (cnagent.md
// CN17): on a primary that serves, a transfer whose device the step finds
// on an error table, and whose reload onto the origin's raid0 fails, moves
// to inaccessible, so the clone in the other pool waits rather than reads
// IO errors; its namespace row reads OK on both channels. The origin
// namespace serves on. The converge's reply names the transfer device
// (CN30), so the round that answers from its observation brings a re-send
// (dnv-worker.md RW4), which fails alike while the fault stands; the
// re-send after the fault reloads the device and only then moves the
// transfer optimized, and the read after it names nothing.
func TestAServedTransferFoundOnAnErrorTableWaitsInaccessible(t *testing.T) {
	srv, node := newTestServer(t)
	xfers := []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn, OriNsIdx: 1}}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, xfers: xfers})
	xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
	assertAna(t, node, xferNqn, "1", "the fixture transfer")
	xfer := xferName(srv, testXfer)
	node.dms[xfer].table = fmt.Sprintf("0 %d error", testTdSize/512)
	reload := "dmsetup reload " + xfer
	node.failCmdAlways[reload] = "device-mapper: reload ioctl failed"
	xferAna := "writedirect " + anaPath(xferNqn, 1) + "="

	node.Reset()
	held := cntlrReq(reqOpts{revision: 3, primary: true, xfers: xfers})
	reply, err := srv.SyncupCntlr(context.Background(), held)
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	assertCodeNames(t, "the failed pass", reply, xfer)
	raid0No := devNoOf(node, raid0Name(srv, testTd))
	if got := node.dms[nsDevName(srv, testNs)].table; got !=
		nsDevTableOn(raid0No) {
		t.Fatalf("the case is vacuous: the origin ns-dev is %q, want it "+
			"serving on the raid0", got)
	}
	assertAna(t, node, testNqn, "1", "the origin namespace")
	assertAna(t, node, xferNqn, "3", "the transfer found on an error table")
	info := reply.GetCntlrInfo()
	assertRowStatus(t, info.GetXferIdToDmLinear()[testXfer],
		pb.ResStatus_RES_STATUS_ERROR, "the converge's transfer device")
	assertOk(t, info.GetXferIdToNamespace()[testXfer],
		"the converge's transfer namespace")
	probed := checkRound(t, srv, 3)
	assertOk(t, probed.GetXferIdToNamespace()[testXfer],
		"the check round's transfer namespace")
	round, resent := resendRoundOnly(t, srv, held, "with-the-fault")
	assertCodeNames(t, "the round after the failed pass", round, xfer)
	if resent == nil {
		t.Fatalf("the round that names the transfer device brought no " +
			"re-send")
	}
	assertCodeNames(t, "the re-send with the fault", resent, xfer)
	assertAna(t, node, xferNqn, "3", "the re-send with the fault")

	delete(node.failCmdAlways, reload)
	node.Reset()
	round, resent = resendRoundOnly(t, srv, held, "after-the-fault")
	if resent == nil {
		t.Fatalf("the round after the fault brought no re-send: %v",
			round.GetAgentReply())
	}
	if got := node.dms[xfer].table; got != nsDevTableOn(raid0No) {
		t.Fatalf("the transfer device is %q after the fault, want the "+
			"raid0", got)
	}
	assertOrder(t, node, "cmd "+reload, xferAna+"1")
	if !tracedCall(node, "after-the-fault", reload) {
		t.Fatalf("the reload did not run under the round's trace id")
	}
	assertAna(t, node, xferNqn, "1", "the served transfer")
	round, resent = resendRound(t, srv, held, "after-the-re-send")
	assertCodeZero(t, "the read after the re-send", round)
	if resent != nil {
		t.Fatalf("the read after the re-send brought another one")
	}
}

// TestTransferOnPurposeOnAnErrorTableStaysOptimized is the other direction
// of CN17's rule: a transfer the primary does not serve on purpose — at the
// level that removes the thin pools, or with an origin that no longer
// resolves (CN29) — has its device on the error table its rule selects, so
// it stays optimized over it, with no ANA write, and a read names nothing
// for it (CN30). At the level,
// its namespace and subsystem rows read OK on both channels (CN28): the row
// compares the group with the desired one, since the transfer is not one
// that should serve.
func TestTransferOnPurposeOnAnErrorTableStaysOptimized(t *testing.T) {
	xfers := []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn, OriNsIdx: 1}}
	for _, tc := range []struct {
		name string
		to   reqOpts
		// rowsOk: the transfer's export rows read OK on both channels — the
		// level's error table is the one the desired state wants; a missing
		// origin's rows say the origin is gone (CN29).
		rowsOk bool
	}{
		{"no thin pool", reqOpts{revision: 3, primary: true, xfers: xfers,
			level: pb.SpLevel_SP_LEVEL_NO_THINPOOL}, true},
		{"origin gone", reqOpts{revision: 3, primary: true, xfers: xfers,
			tds: []*pb.ThinDevice{}, subsys: map[string]*pb.Subsystem{}},
			false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				xfers: xfers})
			xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
			assertAna(t, node, xferNqn, "1", "the fixture transfer")

			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(tc.to))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			want := fmt.Sprintf("0 %d error", testTdSize/512)
			if got := node.dms[xferName(srv, testXfer)].table; got != want {
				t.Fatalf("the transfer device is %q, want %q", got, want)
			}
			assertNoCall(t, node, "writedirect "+anaPath(xferNqn, 1)+"=")
			assertAna(t, node, xferNqn, "1", "the unserved transfer")
			if tc.rowsOk {
				// The row compares the group with the desired one, not
				// with the inaccessible group a transfer that should serve
				// waits in over an error table.
				info := reply.GetCntlrInfo()
				probed := checkRound(t, srv, tc.to.revision)
				for _, ch := range []struct {
					label string
					info  *pb.CntlrInfo
				}{{"the converge's", info}, {"the check round's", probed}} {
					assertOk(t, ch.info.GetXferIdToNamespace()[testXfer],
						ch.label+" transfer namespace")
					assertOk(t, ch.info.GetXferIdToSubsystem()[testXfer],
						ch.label+" transfer subsystem")
				}
			}
			assertCodeZero(t, "the read of the unserved transfer",
				rowRound(t, srv, tc.to.revision))
		})
	}
}

// TestADisabledNamespaceGetsInaccessibleBeforeItsEnable pins the object
// step of a namespace that exists but is not enabled — a step left behind
// between its mkdir and its enable, still in the kernel's default group 1:
// the step writes the inaccessible group before the enable, and only the
// build's ANA loop, after its device serves, moves it optimized. An enabled
// namespace keeps its group through the same step. The transfer namespace
// follows the same rule.
func TestADisabledNamespaceGetsInaccessibleBeforeItsEnable(t *testing.T) {
	srv, node := newTestServer(t)
	xfers := []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn, OriNsIdx: 1}}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, xfers: xfers})
	xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)

	// An equal-revision re-apply moves no enabled namespace.
	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 2, primary: true, xfers: xfers})); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	for _, nqn := range []string{testNqn, xferNqn} {
		assertNoCall(t, node, "writedirect "+anaPath(nqn, 1)+"=")
		assertAna(t, node, nqn, "1", "the re-apply of "+nqn)
	}

	for _, nqn := range []string{testNqn, xferNqn} {
		nsPath := fmt.Sprintf("%s/subsystems/%s/namespaces/1",
			agent.NvmetRoot, nqn)
		node.files[nsPath+"/enable"] = "0"
		node.files[anaPath(nqn, 1)] = "1"
	}
	node.Reset()
	if _, err := srv.SyncupCntlr(context.Background(), cntlrReq(reqOpts{
		revision: 3, primary: true, xfers: xfers})); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	for _, nqn := range []string{testNqn, xferNqn} {
		nsPath := fmt.Sprintf("%s/subsystems/%s/namespaces/1",
			agent.NvmetRoot, nqn)
		ana := "writedirect " + anaPath(nqn, 1) + "="
		assertOrder(t, node, ana+"3", "writedirect "+nsPath+"/enable=1",
			ana+"1")
		assertAna(t, node, nqn, "1", "the enabled "+nqn)
	}
}

// TestAFailedAnaMoveIsNamedAndResent pins that a namespace or a transfer
// whose device the converge put where the desired state wants it, but whose
// move to the optimized group fails, is left short of optimized and named
// by the converge's own reply (cnagent.md CN16, CN17, CN30): its row reads
// the group it has against the one its device earns. The round that answers
// from that observation brings a re-send (dnv-worker.md RW4), which fails
// alike while the fault stands; the re-send after the fault moves it
// optimized, and the read after it names nothing.
func TestAFailedAnaMoveIsNamedAndResent(t *testing.T) {
	xfers := []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn, OriNsIdx: 1}}
	for _, tc := range []struct {
		name string
		// nqn names the subsystem whose namespace's move fails.
		nqn func(srv *CnAgentServer) string
		// other names the one whose move succeeds.
		other func(srv *CnAgentServer) string
	}{
		{"a namespace",
			func(*CnAgentServer) string { return testNqn },
			func(srv *CnAgentServer) string {
				return srv.nf.XferNqn(testCluster, testSp, testXfer)
			}},
		{"a transfer",
			func(srv *CnAgentServer) string {
				return srv.nf.XferNqn(testCluster, testSp, testXfer)
			},
			func(*CnAgentServer) string { return testNqn }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			opts := func(revision uint64, primary bool) reqOpts {
				return reqOpts{revision: revision, primary: primary,
					xfers: xfers}
			}
			syncupBoth(t, srv, opts(2, true))
			if _, err := srv.SyncupCntlr(ctx,
				cntlrReq(opts(3, false))); err != nil {
				t.Fatalf("demote: %v", err)
			}
			nqn, other := tc.nqn(srv), tc.other(srv)
			assertAna(t, node, nqn, "3", "the standby")
			moveKey := anaPath(nqn, 1) + "=1"
			node.failWriteAlways[moveKey] = "invalid argument"

			node.Reset()
			held := cntlrReq(opts(4, true))
			reply, err := srv.SyncupCntlr(ctx, held)
			if err != nil {
				t.Fatalf("promote: %v", err)
			}
			assertCodeNames(t, "the failed move", reply, nsResName(nqn, 1))
			raid0No := devNoOf(node, raid0Name(srv, testTd))
			for _, dev := range []string{nsDevName(srv, testNs),
				xferName(srv, testXfer)} {
				if got := node.dms[dev].table; raid0No == "" ||
					got != nsDevTableOn(raid0No) {
					t.Fatalf("the case is vacuous: %s is %q, want it on "+
						"the raid0", dev, got)
				}
			}
			assertOrder(t, node, "writedirect "+moveKey)
			assertAna(t, node, nqn, "3", "the failed move")
			assertAna(t, node, other, "1", "the move that succeeded")
			round, resent := resendRoundOnly(t, srv, held, "with-the-fault")
			assertCodeNames(t, "the round after the failed move", round,
				nsResName(nqn, 1))
			if resent == nil {
				t.Fatalf("the round that names the namespace brought no " +
					"re-send")
			}
			assertCodeNames(t, "the re-send with the fault", resent,
				nsResName(nqn, 1))
			assertAna(t, node, nqn, "3", "the re-send with the fault")

			delete(node.failWriteAlways, moveKey)
			node.Reset()
			round, resent = resendRoundOnly(t, srv, held, "after-the-fault")
			if resent == nil {
				t.Fatalf("the round after the fault brought no re-send: %v",
					round.GetAgentReply())
			}
			assertCodeZero(t, "the re-send after the fault", resent)
			assertAna(t, node, nqn, "1", "the re-send after the fault")
			round, resent = resendRound(t, srv, held, "after-the-re-send")
			assertCodeZero(t, "the read after the re-send", round)
			if resent != nil {
				t.Fatalf("the read after the re-send brought another one")
			}
		})
	}
}

// TestAFailedStepAtTheNoThinPoolLevelKeepsTheGroup pins the last half of
// nsAnaMove's rule for a failed ns-dev step (cnagent.md CN16): a step that
// fails and finds the ns-dev on the dm-error its rules select keeps the
// namespace's group for that pass, as any failed step that did not find a
// park its rules do not want. A standby at the level that removes the thin
// pools is promoted while the td's dm-error cannot be read: the namespace
// stays inaccessible, with no optimized write. The converge's reply names
// the dm-error, whose read failed, the lowest layer that is not OK (CN30),
// so the round that answers from its observation brings a re-send
// (dnv-worker.md RW4); the re-send after the fault moves the
// namespace optimized over the dm-error, where the level wants it, and the
// read after it names nothing.
func TestAFailedStepAtTheNoThinPoolLevelKeepsTheGroup(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	opts := func(revision uint64, primary bool) reqOpts {
		return reqOpts{revision: revision, primary: primary,
			level: pb.SpLevel_SP_LEVEL_NO_THINPOOL}
	}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(opts(3, false))); err != nil {
		t.Fatalf("demote: %v", err)
	}
	assertAna(t, node, testNqn, "3", "the standby")
	assertParked(t, srv, node, testNs, testTd, "the standby")
	key := "dmsetup table " + errorName(srv, testTd)
	node.failCmdAlways[key] = "device-mapper: table ioctl failed"
	ana := "writedirect " + anaPath(testNqn, 1) + "="

	node.Reset()
	held := cntlrReq(opts(4, true))
	reply, err := srv.SyncupCntlr(ctx, held)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	assertRowStatus(t, reply.GetCntlrInfo().GetNsIdToDmLinear()[testNs],
		pb.ResStatus_RES_STATUS_ERROR, "the failed step's ns-dev row")
	assertCodeNames(t, "the failed step", reply, errorName(srv, testTd))
	assertParked(t, srv, node, testNs, testTd, "the failed step")
	assertNoCall(t, node, ana+"1")
	assertAna(t, node, testNqn, "3", "the failed step")

	delete(node.failCmdAlways, key)
	node.Reset()
	round, resent := resendRoundOnly(t, srv, held, "after-the-fault")
	assertCodeNames(t, "the round after the failed step", round,
		errorName(srv, testTd))
	if resent == nil {
		t.Fatalf("the round that names the dm-error brought no re-send")
	}
	assertParked(t, srv, node, testNs, testTd, "the re-send after the fault")
	assertOrder(t, node, ana+"1")
	assertAna(t, node, testNqn, "1", "the re-send after the fault")
	round, resent = resendRound(t, srv, held, "after-the-re-send")
	assertCodeZero(t, "the read after the re-send", round)
	if resent != nil {
		t.Fatalf("the read after the re-send brought another one")
	}
}

// TestAnUnservedTransferFailedOnAnErrorTableKeepsTheGroup is the transfer
// twin of TestAFailedStepAtTheNoThinPoolLevelKeepsTheGroup (cnagent.md CN17,
// xferAnaMove): a failed device step that finds the device on an error table
// moves the transfer to inaccessible only when the transfer is served, and
// one the primary does not serve keeps its group. A standby at the level
// that removes the thin pools holds a transfer whose device is an error
// table of the wrong size, and is promoted at that level while the reload
// fails: the transfer stays inaccessible, with no optimized write. An
// unserved transfer's device rests on no raid0 (xferServed), so both
// channels name it while it is off the table the level wants (CN17, CN30),
// though the level suppresses the origin's raid0; the re-send that round
// brings (dnv-worker.md RW4) fails alike while the fault stands, and the
// re-send after the fault reloads the device to the error table the level
// wants and only then moves the transfer optimized; the read after it names
// nothing.
func TestAnUnservedTransferFailedOnAnErrorTableKeepsTheGroup(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	xfers := []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn, OriNsIdx: 1}}
	opts := func(revision uint64, primary bool) reqOpts {
		return reqOpts{revision: revision, primary: primary, xfers: xfers,
			level: pb.SpLevel_SP_LEVEL_NO_THINPOOL}
	}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, xfers: xfers})
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(opts(3, false))); err != nil {
		t.Fatalf("demote: %v", err)
	}
	xfer := xferName(srv, testXfer)
	xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
	assertAna(t, node, xferNqn, "3", "the standby transfer")
	want := fmt.Sprintf("0 %d error", testTdSize/512)
	wrong := fmt.Sprintf("0 %d error", testTdSize/512/2)
	node.dms[xfer].table = wrong
	reload := "dmsetup reload " + xfer
	node.failCmdAlways[reload] = "device-mapper: reload ioctl failed"
	xferAna := "writedirect " + anaPath(xferNqn, 1) + "="

	node.Reset()
	held := cntlrReq(opts(4, true))
	reply, err := srv.SyncupCntlr(ctx, held)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	assertCodeNames(t, "the failed step", reply, xfer)
	if !node.hasCall("cmd " + reload) {
		t.Fatalf("the case is vacuous: the promotion did not try to " +
			"reload the transfer device")
	}
	if got := node.dms[xfer].table; got != wrong {
		t.Fatalf("the case is vacuous: the transfer device is %q, want it "+
			"still on the wrong-size error table %q", got, wrong)
	}
	assertRowStatus(t, reply.GetCntlrInfo().GetXferIdToDmLinear()[testXfer],
		pb.ResStatus_RES_STATUS_ERROR, "the failed step's transfer device")
	assertNoCall(t, node, xferAna+"1")
	assertAna(t, node, xferNqn, "3", "the failed step")
	round, resent := resendRound(t, srv, held, "with-the-fault")
	assertCodeNames(t, "the read with the fault", round, xfer)
	if resent == nil {
		t.Fatalf("the round that names the transfer device brought no " +
			"re-send")
	}
	assertCodeNames(t, "the re-send with the fault", resent, xfer)
	assertAna(t, node, xferNqn, "3", "the re-send with the fault")

	delete(node.failCmdAlways, reload)
	node.Reset()
	round, resent = resendRound(t, srv, held, "after-the-fault")
	assertCodeNames(t, "the read after the fault", round, xfer)
	if resent == nil {
		t.Fatalf("the round after the fault brought no re-send")
	}
	if got := node.dms[xfer].table; got != want {
		t.Fatalf("the transfer device is %q after the fault, want %q",
			got, want)
	}
	assertOrder(t, node, "cmd "+reload, xferAna+"1")
	if !tracedCall(node, "after-the-fault", reload) {
		t.Fatalf("the reload did not run under the round's trace id")
	}
	assertAna(t, node, xferNqn, "1", "the re-send after the fault")
	round, resent = resendRound(t, srv, held, "after-the-re-send")
	assertCodeZero(t, "the read after the re-send", round)
	if resent != nil {
		t.Fatalf("the read after the re-send brought another one")
	}
}

// TestAServedTransferWhoseDeviceStepFailsIsNamed pins a transfer device
// step that fails where the device is not known to be on an error table
// (cnagent.md CN17), on a primary whose namespaces all serve: a new
// transfer whose device create fails has no device and no export, and a
// served one whose table cannot be read keeps its group. Neither pass
// writes an optimized group for the transfer. The converge's reply names
// the transfer device (CN30), so the round that answers from its
// observation brings a re-send (dnv-worker.md RW4); the re-send after the
// fault creates the device and moves the new transfer optimized, or reads
// the served one's table, and the read after it names nothing. A failed
// table read that a read does not see is named by the converge alone.
func TestAServedTransferWhoseDeviceStepFailsIsNamed(t *testing.T) {
	xfers := []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn, OriNsIdx: 1}}
	for _, tc := range []struct {
		name string
		// before are the transfers the fixture serves before the pass.
		before []*pb.Transfer
		// fault arms the failure and returns what removes it.
		fault func(srv *CnAgentServer, node *fakeNode) func()
	}{
		{"a new transfer whose create fails", nil,
			func(srv *CnAgentServer, node *fakeNode) func() {
				key := "dmsetup create " + xferName(srv, testXfer)
				node.failCmdAlways[key] = "device-mapper: create ioctl failed"
				return func() { delete(node.failCmdAlways, key) }
			}},
		{"a served transfer whose table read fails", xfers,
			func(srv *CnAgentServer, node *fakeNode) func() {
				key := "dmsetup table " + xferName(srv, testXfer)
				node.failCmdAlways[key] = "device-mapper: table ioctl failed"
				return func() { delete(node.failCmdAlways, key) }
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				xfers: tc.before})
			xfer := xferName(srv, testXfer)
			xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
			xferAna := "writedirect " + anaPath(xferNqn, 1) + "="
			before := node.fileOf(anaPath(xferNqn, 1))
			disarm := tc.fault(srv, node)

			node.Reset()
			held := cntlrReq(reqOpts{revision: 3, primary: true,
				xfers: xfers})
			reply, err := srv.SyncupCntlr(context.Background(), held)
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			assertCodeNames(t, "the failed step", reply, xfer)
			raid0No := devNoOf(node, raid0Name(srv, testTd))
			if got := node.dms[nsDevName(srv, testNs)].table; got !=
				nsDevTableOn(raid0No) {
				t.Fatalf("the case is vacuous: the origin ns-dev is %q, "+
					"want it serving on the raid0", got)
			}
			assertAna(t, node, testNqn, "1", "the origin namespace")
			assertRowStatus(t,
				reply.GetCntlrInfo().GetXferIdToDmLinear()[testXfer],
				pb.ResStatus_RES_STATUS_ERROR, "the transfer device row")
			assertNoCall(t, node, xferAna+"1")
			if got := node.fileOf(anaPath(xferNqn, 1)); got != before {
				t.Fatalf("the failed step moved the transfer from %q to %q",
					before, got)
			}

			disarm()
			node.Reset()
			round, resent := resendRoundOnly(t, srv, held, "after-the-fault")
			assertCodeNames(t, "the round after the failed step", round,
				xfer)
			if resent == nil {
				t.Fatalf("the round that names the transfer device " +
					"brought no re-send")
			}
			if got := node.dms[xfer]; got == nil ||
				got.table != nsDevTableOn(raid0No) {
				t.Fatalf("the transfer device is %+v after the fault, "+
					"want it on the raid0", got)
			}
			if tc.before == nil {
				assertOrder(t, node, "cmd dmsetup create "+xfer, xferAna+"1")
				if !tracedCall(node, "after-the-fault",
					"dmsetup create "+xfer) {
					t.Fatalf("the create did not run under the round's " +
						"trace id")
				}
			}
			assertAna(t, node, xferNqn, "1", "the re-send after the fault")
			round, resent = resendRound(t, srv, held, "after-the-re-send")
			assertCodeZero(t, "the read after the re-send", round)
			if resent != nil {
				t.Fatalf("the read after the re-send brought another one")
			}
		})
	}
}

// TestATransferWhoseOriginIsGoneAndWhoseDemotionFails pins the
// missing-origin half of CN17's rule: a transfer whose origin no longer
// resolves is put where the desired state wants it only by a demotion that
// succeeds, so one whose demotion onto its error table fails — the device
// still maps the origin td's raid0 — keeps its group. No row of a transfer
// whose origin the request no longer names is named (CN30), since no
// converge of the request can build it. With the origin's thin device gone
// too, its raid0 is left over under the transfer device, and the sweep
// names the raid0 (CN21), so the round brings a re-send (dnv-worker.md RW4)
// whose converge, after the fault, demotes the device and removes the
// raid0. With the thin device kept, nothing is named: the device stays on
// the raid0 and nothing runs until a new revision, whose converge demotes
// it (cnagent.md, Known limits).
func TestATransferWhoseOriginIsGoneAndWhoseDemotionFails(t *testing.T) {
	xfers := []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn, OriNsIdx: 1}}
	for _, tc := range []struct {
		name string
		// tds is the request's td_list: none, or the fixture's.
		tds []*pb.ThinDevice
	}{
		{"its thin device gone too", []*pb.ThinDevice{}},
		{"its thin device kept", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				xfers: xfers})
			xfer := xferName(srv, testXfer)
			xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
			assertAna(t, node, xferNqn, "1", "the fixture transfer")
			served := node.dms[xfer].table
			reload := "dmsetup reload " + xfer
			node.failCmdAlways[reload] = "device-mapper: reload ioctl failed"
			gone := func(revision uint64) *pb.SyncupCntlrRequest {
				return cntlrReq(reqOpts{revision: revision, primary: true,
					xfers: xfers, tds: tc.tds,
					subsys: map[string]*pb.Subsystem{}})
			}
			held := gone(3)
			want := fmt.Sprintf("0 %d error", testTdSize/512)

			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(), held)
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if got := node.dms[xfer].table; got != served {
				t.Fatalf("the case is vacuous: the transfer device is %q, "+
					"want it still on the origin's raid0 %q", got, served)
			}
			assertNoCall(t, node, "writedirect "+anaPath(xferNqn, 1)+"=")
			assertAna(t, node, xferNqn, "1", "the failed demotion")
			assertRowNotNamed(t, reply.GetAgentReply(), "the converge", xfer)

			if tc.tds == nil {
				assertCodeZero(t, "the converge", reply)
				delete(node.failCmdAlways, reload)
				node.forgetTraces()
				own := []string{"kept", "kept-again"}
				for _, id := range own {
					round, resent := resendRound(t, srv, held, id)
					assertCodeZero(t, id, round)
					if resent != nil {
						t.Fatalf("%s brought a re-send", id)
					}
				}
				assertNothingRunsBetweenCalls(t, node, own...)
				if got := node.dms[xfer].table; got != served {
					t.Fatalf("the transfer device is %q with nothing "+
						"named, want it left on the raid0", got)
				}
				node.Reset()
				if _, err := srv.SyncupCntlr(context.Background(),
					gone(4)); err != nil {
					t.Fatalf("the next revision: %v", err)
				}
				if got := node.dms[xfer].table; got != want {
					t.Fatalf("the next revision left the transfer device "+
						"%q, want %q", got, want)
				}
				return
			}

			raid0 := raid0Name(srv, testTd)
			assertCodeNames(t, "the converge", reply, raid0)
			round, resent := resendRoundOnly(t, srv, held, "with-the-fault")
			assertCodeNames(t, "the round after the failed demotion", round,
				raid0)
			if resent == nil {
				t.Fatalf("the round that names the raid0 brought no re-send")
			}
			assertCodeNames(t, "the re-send with the fault", resent, raid0)

			delete(node.failCmdAlways, reload)
			node.Reset()
			round, resent = resendRoundOnly(t, srv, held, "after-the-fault")
			if resent == nil {
				t.Fatalf("the round after the fault brought no re-send: %v",
					round.GetAgentReply())
			}
			if got := node.dms[xfer].table; got != want {
				t.Fatalf("the transfer device is %q after the fault, "+
					"want %q", got, want)
			}
			if !tracedCall(node, "after-the-fault", reload) {
				t.Fatalf("the demotion did not run under the round's " +
					"trace id")
			}
			if _, ok := node.dms[raid0]; ok {
				t.Fatalf("the re-send left the departed raid0")
			}
			round, resent = resendRound(t, srv, held, "after-the-re-send")
			assertCodeZero(t, "the read after the re-send", round)
			if resent != nil {
				t.Fatalf("the read after the re-send brought another one")
			}
		})
	}
}

// TestAGrowOverALateRedundNoneLegKeepsTheStackServing: a GrowSlice adds a
// RedundNone group whose leg is not available in the pass. Its dm-linear is
// not built, so the slice's concat keeps its old size, while the pool, the
// raid0 and the ns-dev keep serving and the namespace stays optimized
// (cnagent.md CN12). While the leg is late a read names nothing for the
// group; once it serves, a read names the group's dm-linear (CN30), and the
// re-send that round brings (dnv-worker.md RW4) builds the group under the
// round's trace id; the read after it names nothing.
func TestAGrowOverALateRedundNoneLegKeepsTheStackServing(t *testing.T) {
	srv, node := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	assertAna(t, node, testNqn, "1", "the fixture")
	setAna := func(state string) {
		node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp,
			testDataLeg2, testCn), testIp2, testSvcId2, state)
	}
	setAna("non-optimized")
	node.Reset()
	grow := cntlrReq(reqOpts{revision: 3, primary: true})
	grownDataGrp(grow, true)
	reply, err := srv.SyncupCntlr(ctx, grow)
	if err != nil {
		t.Fatalf("grow: %v", err)
	}
	info := reply.GetCntlrInfo()
	assertErrorDetails(t, info.GetGrpIdToMdRaid()[testDataGrp2],
		"no available leg", "the grown group over its late leg")
	if _, ok := node.dms[grpName(srv, testDataGrp2)]; ok {
		t.Fatalf("the grown group's dm-linear was built over its late leg")
	}
	for _, name := range []string{poolName(srv), raid0Name(srv, testTd),
		nsDevName(srv, testNs)} {
		if _, ok := node.dms[name]; !ok {
			t.Fatalf("%s went away under a grow over a late leg", name)
		}
	}
	assertRowStatus(t, info.GetNsIdToDmLinear()[testNs],
		pb.ResStatus_RES_STATUS_OK, "the ns-dev row")
	assertRowStatus(t, info.GetNsIdToNamespace()[testNs],
		pb.ResStatus_RES_STATUS_OK, "the namespace row")
	assertNoCall(t, node, "writedirect "+anaPath(testNqn, 1)+"=3")
	assertAna(t, node, testNqn, "1", "after the grow over a late leg")
	grown := grpName(srv, testDataGrp2)
	round, resent := resendRound(t, srv, grow, "late")
	assertRowNotNamed(t, round.GetAgentReply(), "the read while late", grown)
	if resent != nil {
		t.Fatalf("the read while the leg is late brought a re-send: %v",
			round.GetAgentReply())
	}

	setAna("optimized")
	node.Reset()
	round, resent = resendRound(t, srv, grow, "serving")
	assertCodeNames(t, "the read once the leg serves", round, grown)
	if resent == nil {
		t.Fatalf("the round that names the grown group brought no re-send")
	}
	if _, ok := node.dms[grown]; !ok {
		t.Fatalf("the re-send did not build the grown group")
	}
	if !tracedCall(node, "serving", "dmsetup create "+grown) {
		t.Fatalf("the create did not run under the round's trace id")
	}
	assertAna(t, node, testNqn, "1", "after the re-send")
	round, resent = resendRound(t, srv, grow, "after-the-re-send")
	assertCodeZero(t, "the read after the re-send", round)
	if resent != nil {
		t.Fatalf("the read after the re-send brought another one")
	}
}
