package cnagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// pathToCutName is how a verdict names a side whose path the converge would
// cut (pathsToCut).
func pathToCutName(nqn, addr, svcId string) string {
	return nqn + " at " + addr + ":" + svcId + ": path to cut"
}

// TestTheCutOfAMigratingLegsPath pins CN10's cut on the data leg of the
// extraSide fixture, its first side at testIp and its second at testIp2: on
// a primary, while one path is down (connecting) and the other is live and
// reads inaccessible, a check round names the live path to cut, and the
// converge disconnects it and connects it again on no later converge while
// the other stays down; once the down path is live again, the converge
// connects the cut side. A standby, a live path that reads optimized, the
// migration's gap (both live, both inaccessible), and a pool without RAID,
// whose leg has no other leg to take the write, cut nothing.
func TestTheCutOfAMigratingLegsPath(t *testing.T) {
	type path struct{ addr, svcId string }
	src, dst := path{testIp, testSvcId}, path{testIp2, testSvcId2}
	for _, tc := range []struct {
		name    string
		primary bool
		down    *path
		live    path
		liveAna string
		cut     bool
		noRaid  bool
	}{
		{"before the cutover, the source down", true, &src, dst,
			"inaccessible", true, false},
		{"after the cutover, the destination down", true, &dst, src,
			"inaccessible", true, false},
		{"a standby", false, &src, dst, "inaccessible", false, false},
		{"the live path optimized", true, &dst, src, "optimized", false,
			false},
		{"the migration's gap", true, nil, dst, "inaccessible", false, false},
		{"a pool without RAID", true, &src, dst, "inaccessible", false,
			true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			opts := reqOpts{revision: 2, primary: tc.primary,
				extraSide: true, raid1: !tc.noRaid, twoLegs: !tc.noRaid}
			syncupBoth(t, srv, opts)
			nqn := legNqn(srv, testDataLeg)
			node.setAnaState(nqn, tc.live.addr, tc.live.svcId, tc.liveAna)
			if tc.down != nil {
				node.setCtrlState(nqn, tc.down.addr, tc.down.svcId,
					"connecting")
			} else {
				node.setAnaState(nqn, src.addr, src.svcId, "inaccessible")
			}

			reply := checkCntlrVerdict(t, srv, opts.revision)
			want := pathToCutName(nqn, tc.live.addr, tc.live.svcId)
			if got := strings.Contains(reply.GetDetails(), want); got != tc.cut {
				t.Fatalf("the check round names %q: %v, want %v (%v)",
					want, got, tc.cut, reply)
			}
			opts.revision = 3
			if _, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(opts)); err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			cut := ctrlNameOf(node, nqn, tc.live.addr, tc.live.svcId) == ""
			if cut != tc.cut {
				t.Fatalf("the converge cut the live path: %v, want %v",
					cut, tc.cut)
			}
			if !tc.cut {
				return
			}
			// No reconnect while the other path stays down, and the read
			// names neither a side to connect nor a path to cut.
			opts.revision = 4
			if _, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(opts)); err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if ctrlNameOf(node, nqn, tc.live.addr, tc.live.svcId) != "" {
				t.Fatalf("a converge connected the cut path while the " +
					"other was down")
			}
			reply = checkCntlrVerdict(t, srv, opts.revision)
			if strings.Contains(reply.GetDetails(), nqn) {
				t.Fatalf("the check round after the cut names the leg: %v",
					reply)
			}
			// The down path comes back: the cut side is connected again.
			node.setCtrlState(nqn, tc.down.addr, tc.down.svcId, "live")
			opts.revision = 5
			if _, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(opts)); err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if ctrlNameOf(node, nqn, tc.live.addr, tc.live.svcId) == "" {
				t.Fatalf("the cut side was not connected again once the " +
					"other path was live")
			}
		})
	}
}

// TestThePathWatcherCutsWithNoConverge pins CN10's path watcher: on a
// primary, the watcher a converge started for the migrating data leg cuts
// the live inaccessible path on its own once the other path is down, with
// no converge and no read in between, since a converge can wait behind a
// read wedged on the leg's pool; a standby has no watcher, and its path
// stays.
func TestThePathWatcherCutsWithNoConverge(t *testing.T) {
	for _, primary := range []bool{true, false} {
		name := "standby"
		if primary {
			name = "primary"
		}
		t.Run(name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.probeInterval = 10 * time.Millisecond
			opts := reqOpts{revision: 2, primary: primary, extraSide: true,
				raid1: true, twoLegs: true}
			syncupBoth(t, srv, opts)
			nqn := legNqn(srv, testDataLeg)
			node.setAnaState(nqn, testIp2, testSvcId2, "inaccessible")
			node.setCtrlState(nqn, testIp, testSvcId, "connecting")
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) &&
				ctrlNameOf(node, nqn, testIp2, testSvcId2) != "" {
				time.Sleep(5 * time.Millisecond)
			}
			cut := ctrlNameOf(node, nqn, testIp2, testSvcId2) == ""
			if cut != primary {
				t.Fatalf("the watcher cut the live path: %v, want %v", cut,
					primary)
			}
		})
	}
}

// dropCtrlByAddr takes away the controller of one side of a leg, if any,
// the way the kernel ends it.
func dropCtrlByAddr(node *fakeNode, nqn, addr, svcId string) {
	node.mu.Lock()
	defer node.mu.Unlock()
	subsys := node.subsystems[nqn]
	if subsys == nil {
		return
	}
	for _, ctrl := range subsys.ctrls {
		if ctrl.trAddr == addr && ctrl.trSvcId == svcId {
			node.dropCtrl(subsys, ctrl)
			return
		}
	}
}

// TestTheCutsEdges pins the edges of CN10's cut and of its connect hold on
// the primary's migrating data leg of an md-raid1 pool: a resetting path is
// not down and a live non-optimized path is not cut; a dead path is down;
// the converge that cuts reports the cut path as it left it, not OK; the
// hold keeps the cut side out only while the down path last read optimized
// or non-optimized, so a serving side the kernel deleted beside a down path
// that never served is connected again; and a standby connects beside a
// down path.
func TestTheCutsEdges(t *testing.T) {
	setup := func(t *testing.T, primary bool) (*CnAgentServer, *fakeNode,
		string, reqOpts) {
		srv, node := newTestServer(t)
		opts := reqOpts{revision: 2, primary: primary, extraSide: true,
			raid1: true, twoLegs: true}
		syncupBoth(t, srv, opts)
		opts.revision = 3
		return srv, node, legNqn(srv, testDataLeg), opts
	}
	converge := func(t *testing.T, srv *CnAgentServer, opts reqOpts) {
		t.Helper()
		if _, err := srv.SyncupCntlr(context.Background(),
			cntlrReq(opts)); err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
	}
	for _, tc := range []struct {
		name   string
		srcSt  string
		dstAna string
		cut    bool
	}{
		{"the source resetting", "resetting", "inaccessible", false},
		{"the destination non-optimized", "connecting", "non-optimized",
			false},
		{"the source dead", "dead", "inaccessible", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node, nqn, opts := setup(t, true)
			node.setAnaState(nqn, testIp2, testSvcId2, tc.dstAna)
			node.setCtrlState(nqn, testIp, testSvcId, tc.srcSt)
			converge(t, srv, opts)
			if cut := ctrlNameOf(node, nqn, testIp2,
				testSvcId2) == ""; cut != tc.cut {
				t.Fatalf("the destination's path cut: %v, want %v", cut,
					tc.cut)
			}
		})
	}
	t.Run("the cutting converge's row", func(t *testing.T) {
		srv, node, nqn, opts := setup(t, true)
		node.setAnaState(nqn, testIp2, testSvcId2, "inaccessible")
		node.setCtrlState(nqn, testIp, testSvcId, "connecting")
		reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(opts))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		row := reply.GetCntlrInfo().GetSideIdToPath()[testDataDst]
		if row.GetStatus() == pb.ResStatus_RES_STATUS_OK {
			t.Fatalf("the converge that cut the path reports it OK: %v",
				row)
		}
	})
	t.Run("a serving side back beside a down one", func(t *testing.T) {
		srv, node, nqn, opts := setup(t, true)
		node.setAnaState(nqn, testIp2, testSvcId2, "inaccessible")
		node.setCtrlState(nqn, testIp2, testSvcId2, "connecting")
		dropCtrlByAddr(node, nqn, testIp, testSvcId)
		converge(t, srv, opts)
		if ctrlNameOf(node, nqn, testIp, testSvcId) == "" {
			t.Fatalf("the serving side was not connected again beside " +
				"a down path that never served")
		}
	})
	t.Run("a standby beside a down path", func(t *testing.T) {
		srv, node, nqn, opts := setup(t, false)
		node.setCtrlState(nqn, testIp, testSvcId, "connecting")
		dropCtrlByAddr(node, nqn, testIp2, testSvcId2)
		converge(t, srv, opts)
		if ctrlNameOf(node, nqn, testIp2, testSvcId2) == "" {
			t.Fatalf("a standby held a connect beside a down path")
		}
	})
}

// TestNoCutWithNoOtherMemberInSync pins the cut's md condition (CN10): with
// the group's other member not in sync, md copying onto it, the primary
// cuts nothing and its read names nothing, since md would end the wait in
// an IO error on the array's last member.
func TestNoCutWithNoOtherMemberInSync(t *testing.T) {
	srv, node := newTestServer(t)
	opts := reqOpts{revision: 2, primary: true, extraSide: true, raid1: true,
		twoLegs: true}
	syncupBoth(t, srv, opts)
	nqn := legNqn(srv, testDataLeg)
	mdDev := srv.nf.MdPath(srv.nf.CnMdDevName(testCluster, testCn, testSp, 0,
		0, false))
	leg2 := srv.nf.DmPath(legName(srv, testDataLeg2))
	node.setMdSync(mdDev, 1, "recover", "100 / 200")
	node.setMemberState(mdDev, leg2, "spare,failfast")
	node.setAnaState(nqn, testIp2, testSvcId2, "inaccessible")
	node.setCtrlState(nqn, testIp, testSvcId, "connecting")
	reply := checkCntlrVerdict(t, srv, opts.revision)
	if strings.Contains(reply.GetDetails(), "path to cut") {
		t.Fatalf("the read names a path to cut: %v", reply)
	}
	opts.revision = 3
	if _, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(opts)); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	if ctrlNameOf(node, nqn, testIp2, testSvcId2) == "" {
		t.Fatalf("the converge cut a path with no other member in sync")
	}
}

// TestThePathWatchersFollowThePlan pins the watchers' lifecycle (CN10): a
// primary's converge starts one for its migrating leg, a demotion stops it,
// a promotion starts it again, a leg back to one side stops it, and the
// cntlr's drop stops them all.
func TestThePathWatchersFollowThePlan(t *testing.T) {
	srv, _ := newTestServer(t)
	opts := reqOpts{revision: 2, primary: true, extraSide: true, raid1: true,
		twoLegs: true}
	syncupBoth(t, srv, opts)
	st := legState(t, srv)
	watchers := func() int {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return len(st.cutters)
	}
	converge := func(o reqOpts) {
		t.Helper()
		if _, err := srv.SyncupCntlr(context.Background(),
			cntlrReq(o)); err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
	}
	if n := watchers(); n == 0 {
		t.Fatalf("the primary's converge started no watcher")
	}
	opts.revision, opts.primary = 3, false
	converge(opts)
	if n := watchers(); n != 0 {
		t.Fatalf("the demotion left %d watchers", n)
	}
	opts.revision, opts.primary = 4, true
	converge(opts)
	if n := watchers(); n == 0 {
		t.Fatalf("the promotion started no watcher")
	}
	opts.revision, opts.extraSide = 5, false
	converge(opts)
	if n := watchers(); n != 0 {
		t.Fatalf("legs of one side left %d watchers", n)
	}
	opts.revision, opts.extraSide = 6, true
	converge(opts)
	key := cntlrKey(testCluster, testCn, testSp, testCntlr)
	srv.locks.Node().Lock()
	srv.dropCntlrState(context.Background(), key, st)
	srv.locks.Node().Unlock()
	if n := watchers(); n != 0 {
		t.Fatalf("the drop left %d watchers", n)
	}
}
