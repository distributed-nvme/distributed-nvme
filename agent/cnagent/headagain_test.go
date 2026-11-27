package cnagent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestALegHeadNameTakenDuringTheSlowStepBuildsNoWrapper pins CN10's second
// lookup around the head's number. A migrating leg's src side leaves the
// request (FinishMigration), so the converge disconnects its dead path, a
// slow step. While it runs, or later, right before the converge reads the
// head's number, the leg's subsystem goes with its last controller, and
// another pool's leg subsystem takes its entry number, and with it the head
// name the converge's lookup read. The number read by that name is the
// other head's, so the converge fails the leg and builds no wrapper over
// it, and touches no controller of the other subsystem: the lookup after the
// number's read sees the change, whenever before the read it came. The
// re-sent request connects the leg again and builds its wrapper over its
// own new head.
func TestALegHeadNameTakenDuringTheSlowStepBuildsNoWrapper(t *testing.T) {
	for _, when := range []string{
		"during the dead path's disconnect",
		"right before the number's read",
	} {
		t.Run(when, func(t *testing.T) {
			headTakenDuringTheSlowStep(t, when == "during the dead path's disconnect")
		})
	}
}

func headTakenDuringTheSlowStep(t *testing.T, atDisconnect bool) {
	ctx := context.Background()
	srv, node := newTestServer(t)
	withClock(srv)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, extraSide: true})
	nqn := legNqn(srv, testDataLeg)
	other := names(srv).SideToCnNqn(testCluster, testSp+1, 0x99, testCn)
	wrapperName := legName(srv, testDataLeg)
	// A leg with no wrapper yet, as at its first build: nothing holds its
	// head open, so the kernel frees its subsystem with its last controller.
	dropDm(node, wrapperName)

	node.mu.Lock()
	mine := node.subsystems[nqn]
	idx := mine.idx
	head := fmt.Sprintf("nvme%dn1", idx)
	ran := 0
	take := func() {
		ran++
		for name := range node.duringDisconnect {
			delete(node.duringDisconnect, name)
		}
		dropSubsysLocked(node, nqn)
		node.plantSubsys(idx, other, endpoint{"10.0.0.99", "4420"})
	}
	if atDisconnect {
		for _, ctrl := range mine.ctrls {
			node.duringDisconnect[ctrl.name] = take
		}
	} else {
		node.beforeRead[sysfsBlockDir+"/"+head+"/dev"] = take
	}
	node.mu.Unlock()

	node.Reset()
	reply, err := srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 3, primary: true}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	node.mu.Lock()
	otherNo := node.devNo["/dev/"+head]
	held := node.subsystems[other]
	wrapper := node.dms[wrapperName]
	node.mu.Unlock()
	if ran != 1 {
		t.Fatalf("the change ran %d times, want 1:\n%s", ran,
			strings.Join(node.Calls(), "\n"))
	}
	if held == nil || held.idx != idx || otherNo == "" {
		t.Fatalf("the other subsystem is %+v at %s, want it at "+
			"nvme-subsys%d", held, otherNo, idx)
	}
	if wrapper != nil && strings.Contains(wrapper.table, " "+otherNo+" ") {
		t.Fatalf("the data leg's wrapper maps the other pool's head %s: "+
			"%q", otherNo, wrapper.table)
	}
	assertErrorDetails(t, reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg],
		"the head of "+nqn+" changed",
		"leg data, its head name taken during the slow step")
	for _, call := range node.callsMatching("cmd nvme disconnect") {
		if strings.Contains(call, other) ||
			slices.Contains(strings.Fields(call), held.ctrls[0].name) {
			t.Fatalf("the converge touched the other subsystem: %q", call)
		}
	}

	// The re-sent request connects the leg afresh and builds its wrapper
	// over its own head.
	node.Reset()
	reply, err = srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 3, primary: true}))
	if err != nil {
		t.Fatalf("SyncupCntlr again: %v", err)
	}
	// A primary's leg reads its first probe pending until its prober runs.
	if row := reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg]; row.
		GetStatus() == pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("leg data, connected again: %v", row)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	again := node.subsystems[nqn]
	if again == nil || again.idx == idx {
		t.Fatalf("the data leg's subsystem is %+v, want a new entry", again)
	}
	newNo := node.devNo[fmt.Sprintf("/dev/nvme%dn1", again.idx)]
	wrapper = node.dms[wrapperName]
	if newNo == "" || wrapper == nil ||
		!strings.Contains(wrapper.table, " "+newNo+" ") {
		t.Fatalf("the data leg's wrapper is %+v, want it over its new "+
			"head %s", wrapper, newNo)
	}
}

// TestALegWhoseSecondLookupDoesNotAnswerBuildsNoWrapper: the lookup after the
// head's number read does not answer, here its read of the entry's
// subsysnqn, so nothing proves the name was the leg's head when its number
// was read, and the converge fails the leg for the pass with the lookup's
// error and builds no wrapper. The re-sent request builds the wrapper over
// the same head.
func TestALegWhoseSecondLookupDoesNotAnswerBuildsNoWrapper(t *testing.T) {
	ctx := context.Background()
	srv, node := newTestServer(t)
	withClock(srv)
	opts := reqOpts{revision: 2, primary: true}
	syncupBoth(t, srv, opts)
	nqn := legNqn(srv, testDataLeg)
	wrapperName := legName(srv, testDataLeg)
	dropDm(node, wrapperName)
	dir := subsysDirOf(t, node, nqn)

	node.mu.Lock()
	idx := node.subsystems[nqn].idx
	head := fmt.Sprintf("nvme%dn1", idx)
	armed := false
	node.beforeRead[sysfsBlockDir+"/"+head+"/dev"] = func() {
		armed = true
		node.failRead[dir+"/subsysnqn"] = true
	}
	node.mu.Unlock()

	reply, err := srv.SyncupCntlr(ctx, cntlrReq(opts))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	node.mu.Lock()
	wrapper := node.dms[wrapperName]
	node.mu.Unlock()
	if !armed {
		t.Fatalf("the converge never read %s's number", head)
	}
	if wrapper != nil {
		t.Fatalf("the data leg's wrapper was built though its second "+
			"lookup did not answer: %q", wrapper.table)
	}
	assertErrorDetails(t, reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg],
		errSubsysUnknown.Error(),
		"leg data, its second lookup did not answer")

	reply, err = srv.SyncupCntlr(ctx, cntlrReq(opts))
	if err != nil {
		t.Fatalf("SyncupCntlr again: %v", err)
	}
	if row := reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg]; row.
		GetStatus() == pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("leg data, sent again: %v", row)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	no := node.devNo["/dev/"+head]
	wrapper = node.dms[wrapperName]
	if no == "" || wrapper == nil ||
		!strings.Contains(wrapper.table, " "+no+" ") {
		t.Fatalf("the data leg's wrapper is %+v, want it over %s", wrapper,
			no)
	}
}
