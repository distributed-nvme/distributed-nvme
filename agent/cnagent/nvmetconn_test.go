package cnagent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The cut of a removed host (cnagent.md CN33): every converge steps each
// subsystem's list first and, in cut mode, ends each nvmet connection of it
// whose host is off the subsystem's desired list, a stray connection,
// before the ns-dev loop and before a transfer's device step, and a read
// reports both. A subsystem whose list admits a removed host, or whose cut
// has not held, is not safe: every namespace of it moves to inaccessible
// before those steps and stays there to the pass's end, one whose move
// fails getting no data put under it; its row reads PENDING while a stray
// is still listed and no lockdown the lockdown file shows is in force, and
// ERROR while the connections cannot be read or such a lockdown keeps them
// from being ended; both verdicts name a pending row, and only a read's an
// error row.

const (
	// strayHost is the host the tests take off a list; keptHost stays.
	strayHost = "nqn.2024-01.io.dnv-it:host:stray"
	keptHost  = "nqn.2024-01.io.dnv-it:host:kept"
)

// newCutServer is newTestServer in cut mode, on a node that models nvmet's
// debug directory and, as a node with securityfs mounted does, a lockdown
// file that shows no lockdown in force.
func newCutServer(t *testing.T) (*CnAgentServer, *fakeNode) {
	t.Helper()
	srv, node := newTestServer(t)
	node.withNvmetDebug()
	node.files[agent.LockdownPath] = "[none] integrity confidentiality\n"
	srv.strayCut = true
	return srv, node
}

// connDir is one nvmet connection's directory of the subsystem nqn.
func connDir(nqn, name string) string {
	return agent.NvmetDebugRoot + "/" + nqn + "/" + name
}

// endWrite is the recorded write that ends one nvmet connection.
func endWrite(nqn, name string) string {
	return "writedirect " + connDir(nqn, name) + "/state=fatal"
}

// endConn frees one connection's directory, as the kernel does once the
// connection has ended.
func (f *fakeNode) endConn(nqn, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.connEnds, connDir(nqn, name))
	f.dropConn(connDir(nqn, name))
}

// cutTarget is a subsystem the cut steps: the pool's own or a transfer's.
type cutTarget struct {
	name string
	nqn  func(srv *CnAgentServer) string
	opts func(revision uint64, primary bool, hosts ...string) reqOpts
	row  func(info *pb.CntlrInfo) *pb.ResInfo
	ns   func(info *pb.CntlrInfo) *pb.ResInfo
}

var cutTargets = []cutTarget{
	{
		name: "pool subsystem",
		nqn:  func(*CnAgentServer) string { return testNqn },
		opts: func(revision uint64, primary bool, hosts ...string) reqOpts {
			return reqOpts{revision: revision, primary: primary,
				subsys: subsysWithHosts(hosts...), leftoverOk: true}
		},
		row: func(info *pb.CntlrInfo) *pb.ResInfo {
			return info.GetSsIdToSubsystem()[testSs]
		},
		ns: func(info *pb.CntlrInfo) *pb.ResInfo {
			return info.GetNsIdToNamespace()[testNs]
		},
	},
	{
		name: "transfer subsystem",
		nqn: func(srv *CnAgentServer) string {
			return srv.nf.XferNqn(testCluster, testSp, testXfer)
		},
		opts: func(revision uint64, primary bool, hosts ...string) reqOpts {
			return reqOpts{revision: revision, primary: primary,
				leftoverOk: true, xfers: []*pb.Transfer{{XferId: testXfer,
					OriNqn: testNqn, OriNsIdx: 1, AllowedHosts: hosts}}}
		},
		row: func(info *pb.CntlrInfo) *pb.ResInfo {
			return info.GetXferIdToSubsystem()[testXfer]
		},
		ns: func(info *pb.CntlrInfo) *pb.ResInfo {
			return info.GetXferIdToNamespace()[testXfer]
		},
	},
}

// isXfer says the target is a transfer's subsystem, whose origin is the
// pool namespace testNqn/1.
func (target cutTarget) isXfer() bool {
	return target.name == "transfer subsystem"
}

// assertPoolNsServes checks, for a transfer target, that the pool's own
// namespace moved to optimized and reads OK on both channels, unnamed:
// safety is per subsystem, so a transfer's subsystem keeps its own
// namespace dark and never its origin's (CN16, CN17).
func assertPoolNsServes(
	t *testing.T,
	srv *CnAgentServer,
	node *fakeNode,
	target cutTarget,
	reply *pb.SyncupCntlrReply,
	revision uint64,
) {
	t.Helper()
	if !target.isXfer() {
		return
	}
	assertAna(t, node, testNqn, "1", "the pool's namespace")
	assertOk(t, reply.GetCntlrInfo().GetNsIdToNamespace()[testNs],
		"the converge's pool namespace")
	assertOk(t, checkRound(t, srv, revision).GetNsIdToNamespace()[testNs],
		"the read's pool namespace")
	if strings.Contains(reply.GetAgentReply().GetDetails(), testNqn+"/1") {
		t.Errorf("the converge names the pool namespace: %s",
			reply.GetAgentReply().GetDetails())
	}
}

// syncupCut sends one SyncupCntlr and fails on a transport error.
func syncupCut(
	t *testing.T,
	srv *CnAgentServer,
	o reqOpts,
) (*pb.SyncupCntlrRequest, *pb.SyncupCntlrReply) {
	t.Helper()
	req := cntlrReq(o)
	reply, err := srv.SyncupCntlr(context.Background(), req)
	if err != nil {
		t.Fatalf("SyncupCntlr at %d: %v", o.revision, err)
	}
	return req, reply
}

// pendingName is how the cntlr's verdict names a subsystem whose cut has
// not held while host is connected.
func pendingName(nqn, host string) string {
	return nqn + ": pending: host " + host + " still connected"
}

// TestARemovedHostIsCut: a host taken off a subsystem's list loses its
// nvmet connection at the next converge, on a primary and on a standby, for
// the pool's subsystem and a transfer's: the converge writes "fatal" to that
// connection's "state" after the unlink of its host, and to no connection of
// a host that stays; the cut holds in the pass, so both channels read the
// subsystem OK and answer zero. Emptying the list cuts the last host too.
func TestARemovedHostIsCut(t *testing.T) {
	for _, target := range cutTargets {
		for _, primary := range []bool{true, false} {
			name := target.name + "/standby"
			if primary {
				name = target.name + "/primary"
			}
			t.Run(name, func(t *testing.T) {
				srv, node := newCutServer(t)
				nqn := target.nqn(srv)
				syncupBoth(t, srv, target.opts(2, primary, strayHost, keptHost))
				stray := node.connectNvmet(nqn, strayHost)
				kept := node.connectNvmet(nqn, keptHost)

				node.Reset()
				_, reply := syncupCut(t, srv, target.opts(3, primary, keptHost))
				assertOrder(t, node,
					"cmd rm -f "+agent.NvmetRoot+"/subsystems/"+nqn+
						"/allowed_hosts/"+strayHost,
					endWrite(nqn, stray))
				assertNoCall(t, node, endWrite(nqn, kept))
				if got, want := node.nvmetConnsOf(nqn),
					[]string{kept + "=" + keptHost}; !slices.Equal(got, want) {
					t.Fatalf("connections %v, want %v", got, want)
				}
				assertOk(t, target.row(reply.GetCntlrInfo()),
					"the converge's subsystem")
				assertCodeZero(t, "the converge", reply)
				assertOk(t, target.row(checkRound(t, srv, 3)),
					"the read's subsystem")
				if code := readCode(t, srv, 3); code.GetCode() != 0 {
					t.Fatalf("the read's code %d (%s), want 0",
						code.GetCode(), code.GetDetails())
				}

				node.Reset()
				_, reply = syncupCut(t, srv, target.opts(4, primary))
				if !node.hasCall(endWrite(nqn, kept)) ||
					len(node.nvmetConnsOf(nqn)) != 0 {
					t.Fatalf("emptying the list left %v",
						node.nvmetConnsOf(nqn))
				}
				assertOk(t, target.row(reply.GetCntlrInfo()),
					"the emptied subsystem")
			})
		}
	}
}

// TestAReusedNameWithAnAllowedHostIsNoStray: the kernel gives a freed
// cntlid to the next connection at once, so a connection's directory can
// name another host right after the cut; each listing judges a directory
// by the host it names alone, so the new connection of an allowed host is
// not cut, and the cut holds.
func TestAReusedNameWithAnAllowedHostIsNoStray(t *testing.T) {
	srv, node := newCutServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		subsys: subsysWithHosts(strayHost, keptHost)})
	stray := node.connectNvmet(testNqn, strayHost)
	node.connReuse[connDir(testNqn, stray)] = keptHost

	node.Reset()
	_, reply := syncupCut(t, srv, reqOpts{revision: 3, primary: true,
		subsys: subsysWithHosts(keptHost)})
	if got := node.callsMatching(endWrite(testNqn, stray)); len(got) != 1 {
		t.Fatalf("writes of the reused name %v, want the one cut", got)
	}
	if got, want := node.nvmetConnsOf(testNqn),
		[]string{stray + "=" + keptHost}; !slices.Equal(got, want) {
		t.Fatalf("connections %v, want %v", got, want)
	}
	assertOk(t, reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
		"the converge's subsystem")
	assertCodeZero(t, "the converge", reply)
}

// TestACutThatDoesNotHoldKeepsAPromotionInaccessible: a standby promoted
// while a host it was told to remove is still connected moves none of the
// subsystem's namespaces to optimized, the subsystem not being safe,
// whether it is the pool's or a transfer's: its row reads PENDING with that
// host in its text on both channels, the namespace row reads OK, since it
// wants the inaccessible group, and the code names the pending subsystem,
// not its
// namespace, on the converge and on a read, which writes nothing. Once the
// connection goes, the re-send moves the namespace optimized and the next
// read is clean.
func TestACutThatDoesNotHoldKeepsAPromotionInaccessible(t *testing.T) {
	for _, target := range cutTargets {
		t.Run(target.name, func(t *testing.T) {
			srv, node := newCutServer(t)
			nqn := target.nqn(srv)
			syncupBoth(t, srv, target.opts(2, true, strayHost))
			syncupCut(t, srv, target.opts(3, false, strayHost))
			assertAna(t, node, nqn, "3", "the standby's namespace")
			stray := node.connectNvmet(nqn, strayHost)
			node.connEnds[connDir(nqn, stray)] = connEndsNever

			node.Reset()
			held, reply := syncupCut(t, srv, target.opts(4, true))
			if !node.hasCall(endWrite(nqn, stray)) {
				t.Fatalf("the promotion did not cut the stray connection")
			}
			assertNoCall(t, node, "writedirect "+anaPath(nqn, 1)+"=1")
			assertAna(t, node, nqn, "3", "the promotion's namespace")
			info := reply.GetCntlrInfo()
			for label, row := range map[string]*pb.ResInfo{
				"the converge's subsystem": target.row(info),
				"the read's subsystem":     target.row(checkRound(t, srv, 4)),
			} {
				assertRowStatus(t, row, pb.ResStatus_RES_STATUS_PENDING, label)
				if want := "host " + strayHost + " still connected"; row.
					GetDetails() != want {
					t.Fatalf("%s: details %q, want %q", label,
						row.GetDetails(), want)
				}
			}
			assertOk(t, target.ns(info), "the converge's namespace")
			assertOk(t, target.ns(checkRound(t, srv, 4)),
				"the read's namespace")
			assertCodeNames(t, "the converge", reply,
				pendingName(nqn, strayHost))
			if strings.Contains(reply.GetAgentReply().GetDetails(),
				nqn+"/1") {
				t.Errorf("the converge names the namespace: %s",
					reply.GetAgentReply().GetDetails())
			}
			assertPoolNsServes(t, srv, node, target, reply, 4)
			node.Reset()
			code := readCode(t, srv, 4)
			if got := node.callsMatching("list " + agent.NvmetDebugRoot +
				"/" + nqn); len(got) != 1 {
				t.Fatalf("the read listed the debug directory %d times, "+
					"want once whatever the stray connection", len(got))
			}
			if code.GetCode() != common.ReplyCodeLeftover ||
				!strings.Contains(code.GetDetails(),
					pendingName(nqn, strayHost)) {
				t.Fatalf("the read's code %d (%s), want the pending "+
					"subsystem named", code.GetCode(), code.GetDetails())
			}
			if muts := node.Mutations(); len(muts) != 0 {
				t.Fatalf("the read changed the node: %v", muts)
			}

			// The rule without memory: the re-send cuts again the stray
			// connection a pass before it wrote and that is still there.
			node.Reset()
			_, again := resendRound(t, srv, held, "still-connected")
			if again == nil || !node.hasCall(endWrite(nqn, stray)) {
				t.Fatalf("the re-send did not write the stray connection "+
					"again: %v", node.Calls())
			}
			assertNoCall(t, node, "writedirect "+anaPath(nqn, 1)+"=1")
			assertAna(t, node, nqn, "3", "the re-send's namespace")
			assertRowStatus(t, target.row(again.GetCntlrInfo()),
				pb.ResStatus_RES_STATUS_PENDING, "the re-send's subsystem")

			node.endConn(nqn, stray)
			node.Reset()
			_, resent := resendRound(t, srv, held, "after-the-cut")
			if resent == nil || !node.hasCall(
				"writedirect "+anaPath(nqn, 1)+"=1") {
				t.Fatalf("the re-send after the cut held did not move the "+
					"namespace optimized: %v", node.Calls())
			}
			if code := readCode(t, srv, 4); code.GetCode() != 0 {
				t.Fatalf("the read after the cut: code %d (%s), want 0",
					code.GetCode(), code.GetDetails())
			}
		})
	}
}

// TestAFailedSubsystemStepAtAPromotionKeepsInaccessible: a promotion whose
// EnsureSubsystem fails moves none of the subsystem's namespaces to
// optimized, in cut mode and in no cut mode, for the pool's subsystem and a
// transfer's, when the kernel's list still admits the host the desired
// state removed: the step adds the new host first and stops at its failed
// link, before the unlink. In cut mode the cut does not run either, so a
// stray connection whose host link is still there is neither listed nor
// ended. The row names the step, a read reads the namespace row OK, as it
// wants the inaccessible group, and a transfer's failed step keeps no pool
// namespace dark. The
// re-send that finds the step held cuts the stray connection after the
// unlink of its host and moves the namespace.
func TestAFailedSubsystemStepAtAPromotionKeepsInaccessible(t *testing.T) {
	for _, target := range cutTargets {
		for _, cut := range []bool{true, false} {
			name := target.name + "/no cut mode"
			if cut {
				name = target.name + "/cut mode"
			}
			t.Run(name, func(t *testing.T) {
				srv, node := newTestServer(t)
				if cut {
					node.withNvmetDebug()
					srv.strayCut = true
				}
				nqn := target.nqn(srv)
				syncupBoth(t, srv, target.opts(2, true, strayHost))
				syncupCut(t, srv, target.opts(3, false, strayHost))
				assertAna(t, node, nqn, "3", "the standby's namespace")
				stray := ""
				if cut {
					stray = node.connectNvmet(nqn, strayHost)
				}
				link := "ln -s " + agent.NvmetRoot + "/hosts/" + keptHost
				node.failCmdAlways[link] = "ln: Invalid argument"

				node.Reset()
				held, reply := syncupCut(t, srv, target.opts(4, true, keptHost))
				assertNoCall(t, node, "writedirect "+anaPath(nqn, 1)+"=1")
				assertAna(t, node, nqn, "3", "the promotion's namespace")
				assertRowStatus(t, target.row(reply.GetCntlrInfo()),
					pb.ResStatus_RES_STATUS_ERROR, "the subsystem")
				assertCodeNames(t, "the converge", reply, nqn+": differs")
				if got := node.callsMatching(agent.NvmetDebugRoot + "/" +
					nqn); len(got) != 0 {
					t.Fatalf("a failed subsystem step read or cut the debug "+
						"directory: %v", got)
				}
				if cut {
					if got := node.nvmetConnsOf(nqn); len(got) != 1 {
						t.Fatalf("connections %v, want the stray one kept", got)
					}
				}
				assertPoolNsServes(t, srv, node, target, reply, 4)
				read := checkRound(t, srv, 4)
				assertOk(t, target.ns(read), "the read's namespace")
				assertRowStatus(t, target.row(read),
					pb.ResStatus_RES_STATUS_ERROR, "the read's subsystem")

				delete(node.failCmdAlways, link)
				node.Reset()
				if _, resent := resendRound(t, srv, held, "fixed"); resent ==
					nil || !node.hasCall("writedirect "+anaPath(nqn, 1)+"=1") {
					t.Fatalf("the re-send did not move the namespace: %v",
						node.Calls())
				}
				if cut {
					assertOrder(t, node,
						"cmd rm -f "+agent.NvmetRoot+"/subsystems/"+nqn+
							"/allowed_hosts/"+strayHost,
						endWrite(nqn, stray),
						"writedirect "+anaPath(nqn, 1)+"=1")
				}
			})
		}
	}
}

// TestNoCutModeReadsNothingOfTheDebugDirectory: where the agent runs with
// no cut (its start check found a cause and --ignore-host-disconnection let
// it start), neither a converge nor a read reads or writes nvmet's debug
// directory, even on a node that shows one, and a removed host keeps its
// connection with every row OK.
func TestNoCutModeReadsNothingOfTheDebugDirectory(t *testing.T) {
	srv, node := newTestServer(t)
	node.withNvmetDebug()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		subsys: subsysWithHosts(strayHost)})
	stray := node.connectNvmet(testNqn, strayHost)

	node.Reset()
	_, reply := syncupCut(t, srv, reqOpts{revision: 3, primary: true})
	assertOk(t, reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
		"the converge's subsystem")
	assertCodeZero(t, "the converge", reply)
	assertOk(t, checkRound(t, srv, 3).GetSsIdToSubsystem()[testSs],
		"the read's subsystem")
	if calls := node.callsMatching(agent.NvmetDebugRoot); len(calls) != 0 {
		t.Fatalf("no cut mode touched the debug directory: %v", calls)
	}
	if got := node.nvmetConnsOf(testNqn); len(got) != 1 ||
		!strings.HasPrefix(got[0], stray+"=") {
		t.Fatalf("connections %v, want the removed host's kept", got)
	}
}

// TestTheCutReadsOnlyTheSubsystemsTheRequestNames: the cut lists and cuts
// only the subsystems the cntlr's request names, on a converge and on a
// read: a subsystem made by hand and a dn agent's export on the same kernel
// are never listed or cut, whatever hosts they hold. A read lists the debug
// directory of each named subsystem once, even while a stray connection
// stays, and writes nothing.
func TestTheCutReadsOnlyTheSubsystemsTheRequestNames(t *testing.T) {
	srv, node := newCutServer(t)
	foreign := []string{"nqn.2024-01.io.other:s:hand",
		srv.nf.SideToCnNqn(testCluster, testSp, 0x77, testCn)}
	for _, nqn := range foreign {
		node.dirs[agent.NvmetRoot+"/subsystems/"+nqn] = true
		node.dirs[agent.NvmetDebugRoot+"/"+nqn] = true
		node.connectNvmet(nqn, strayHost)
	}
	xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
	named := []string{testNqn, xferNqn}
	opts := func(revision uint64, hosts ...string) reqOpts {
		return reqOpts{revision: revision, primary: true,
			subsys: subsysWithHosts(hosts...),
			xfers: []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn,
				OriNsIdx: 1, AllowedHosts: hosts}}}
	}
	syncupBoth(t, srv, opts(2, strayHost, keptHost))
	for _, nqn := range named {
		node.connectNvmet(nqn, strayHost)
	}
	assertNoForeign := func(when string) {
		t.Helper()
		for _, nqn := range foreign {
			if calls := node.callsMatching(agent.NvmetDebugRoot + "/" +
				nqn); len(calls) != 0 {
				t.Fatalf("%s read a subsystem the request does not name: %v",
					when, calls)
			}
			if got := node.nvmetConnsOf(nqn); len(got) != 1 {
				t.Fatalf("%s: %s's connections %v, want its stray one kept",
					when, nqn, got)
			}
		}
	}

	node.Reset()
	syncupBoth(t, srv, opts(3, keptHost))
	assertNoForeign("the converge")
	for _, nqn := range named {
		if got := node.nvmetConnsOf(nqn); len(got) != 0 {
			t.Fatalf("the converge left %s's connections %v", nqn, got)
		}
		name := node.connectNvmet(nqn, strayHost)
		node.connEnds[connDir(nqn, name)] = connEndsNever
	}

	node.Reset()
	checkRound(t, srv, 3)
	assertNoForeign("a read")
	for _, nqn := range named {
		if got := node.callsMatching("list " + agent.NvmetDebugRoot + "/" +
			nqn); len(got) != 1 {
			t.Errorf("a read listed %s's debug directory %d times, want "+
				"once", nqn, len(got))
		}
	}
	if muts := node.Mutations(); len(muts) != 0 {
		t.Fatalf("a read changed the node: %v", muts)
	}
}

// TestAnUnreadDebugDirectoryIsACutNotHeld: in cut mode a subsystem whose
// debug directory is not there, or whose listing does not answer, is a cut
// that did not hold: its row reads ERROR naming the read on both channels,
// and a promotion keeps the namespace inaccessible. The row is a name only
// a read gives: the converge's code is zero, so the next round's read runs
// and is not cancelled by a re-send, and the read's code names the row with
// the leftover code, so its round counts the ERROR row toward the worker's
// failover (CN30, CN33; dnv-worker.md RW4, RW23, AR10).
func TestAnUnreadDebugDirectoryIsACutNotHeld(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(node *fakeNode)
		want  string
	}{
		{"no debug directory", func(node *fakeNode) {
			delete(node.dirs, agent.NvmetDebugRoot+"/"+testNqn)
		}, detailsConnsUnread + "no " + agent.NvmetDebugRoot + "/" + testNqn},
		{"a listing that fails", func(node *fakeNode) {
			node.failCmdAlways["list "+agent.NvmetDebugRoot+"/"+testNqn] =
				"input/output error"
		}, detailsConnsUnread},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newCutServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			syncupCut(t, srv, reqOpts{revision: 3})
			tc.fault(node)

			node.Reset()
			_, reply := syncupCut(t, srv, reqOpts{revision: 4, primary: true,
				leftoverOk: true})
			assertNoCall(t, node, "writedirect "+anaPath(testNqn, 1)+"=1")
			for label, row := range map[string]*pb.ResInfo{
				"the converge": reply.GetCntlrInfo().
					GetSsIdToSubsystem()[testSs],
				"a read": checkRound(t, srv, 4).GetSsIdToSubsystem()[testSs],
			} {
				assertRowStatus(t, row, pb.ResStatus_RES_STATUS_ERROR, label)
				if !strings.HasPrefix(row.GetDetails(), tc.want) {
					t.Fatalf("%s: details %q, want them to start %q", label,
						row.GetDetails(), tc.want)
				}
			}
			assertAna(t, node, testNqn, "3", "the promotion's namespace")
			assertCodeZero(t, "the converge", reply)
			code := rowRoundOnly(t, srv, 4)
			assertCodeNames(t, "the read", code,
				testNqn+": differs: "+detailsConnsUnread)
			assertRowStatus(t, code.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
				pb.ResStatus_RES_STATUS_ERROR, "the round's subsystem")
		})
	}
}

// TestTheCutWaitsForTheConnectionToGo: the converge lists the subsystem's
// debug directory again after its write until the connection is gone, so a
// connection the kernel frees a moment later still makes a cut that held in
// that pass; a connection that never goes ends the wait at the pass's
// budget, which every subsystem's cut of the pass shares, and its row reads
// PENDING.
func TestTheCutWaitsForTheConnectionToGo(t *testing.T) {
	xferNqn := func(srv *CnAgentServer) string {
		return srv.nf.XferNqn(testCluster, testSp, testXfer)
	}
	opts := func(revision uint64, hosts ...string) reqOpts {
		return reqOpts{revision: revision, primary: true, leftoverOk: true,
			subsys: subsysWithHosts(hosts...),
			xfers: []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn,
				OriNsIdx: 1, AllowedHosts: hosts}}}
	}

	srv, node := newCutServer(t)
	syncupBoth(t, srv, opts(2, strayHost))
	stray := node.connectNvmet(testNqn, strayHost)
	node.connEnds[connDir(testNqn, stray)] = 2
	node.Reset()
	capture := captureLogs(t)
	_, reply := syncupCut(t, srv, opts(3))
	if got := node.callsMatching("list " + agent.NvmetDebugRoot + "/" +
		testNqn); len(got) != 4 {
		t.Fatalf("the cut listed %d times, want the first listing and "+
			"three after the write", len(got))
	}
	// The connection the kernel is ending reads "fatal": it is not written
	// again while the listings show it.
	if got := node.callsMatching(endWrite(testNqn, stray)); len(got) != 1 {
		t.Fatalf("writes %v, want one", got)
	}
	recs := capture.msgRecords(t, msgStrayEnded)
	if len(recs) != 1 || recs[0]["level"] != "INFO" ||
		recs[0]["nqn"] != testNqn || recs[0]["connection"] != stray ||
		recs[0]["host_nqn"] != strayHost {
		t.Fatalf("the ended records %v: want one at INFO naming the "+
			"subsystem, the connection and its host", recs)
	}
	assertOk(t, reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
		"the subsystem whose connection went late")

	srv, node = newCutServer(t)
	syncupBoth(t, srv, opts(2, strayHost))
	for _, nqn := range []string{testNqn, xferNqn(srv)} {
		name := node.connectNvmet(nqn, strayHost)
		node.connEnds[connDir(nqn, name)] = connEndsNever
	}
	node.Reset()
	capture = captureLogs(t)
	_, reply = syncupCut(t, srv, opts(3))
	spent := capture.msgRecords(t, msgStrayWaitSpent)
	byNqn := map[string]bool{}
	for _, rec := range spent {
		if rec["level"] != "INFO" || rec["host_nqn"] != strayHost ||
			rec["strays"] != float64(1) {
			t.Fatalf("a wait spent record %v: want INFO, the stray host and "+
				"one stray", rec)
		}
		byNqn[fmt.Sprint(rec["nqn"])] = true
	}
	if len(spent) != 2 || !byNqn[testNqn] || !byNqn[xferNqn(srv)] {
		t.Fatalf("the wait spent records %v: want one for each subsystem",
			spent)
	}
	listings := len(node.callsMatching("list " + agent.NvmetDebugRoot + "/"))
	pauses := int(common.CnStrayCutPassBudget / common.CnStrayCutPause)
	if want := pauses + 2; listings != want {
		t.Fatalf("the two cuts listed %d times, want %d: one budget of %d "+
			"pauses for the pass", listings, want, pauses)
	}
	assertRowStatus(t, reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
		pb.ResStatus_RES_STATUS_PENDING, "the pool subsystem")
	assertRowStatus(t, reply.GetCntlrInfo().GetXferIdToSubsystem()[testXfer],
		pb.ResStatus_RES_STATUS_PENDING, "the transfer subsystem")
}

// TestAConnectionThatGoesBeforeItsWriteIsGone: a connection that ends
// between the listing that read its host and the cut's write is a
// connection that has gone, with no record, no refusal and a cut that
// holds: gone
// before the write's own read of its host, nothing is written; gone after
// that read, the write finds no "state", which is no refusal either.
func TestAConnectionThatGoesBeforeItsWriteIsGone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reads int // hostnqn reads after which the connection goes
		wrote bool
	}{
		{"before the write's read of its host", 1, false},
		{"after the write's read of its host", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newCutServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				subsys: subsysWithHosts(strayHost)})
			stray := node.connectNvmet(testNqn, strayHost)
			dir := connDir(testNqn, stray)
			reads := 0
			var hook func()
			hook = func() {
				if reads++; reads == tc.reads {
					node.dropConn(dir)
					return
				}
				node.afterRead[dir+"/hostnqn"] = hook
			}
			node.afterRead[dir+"/hostnqn"] = hook

			node.Reset()
			capture := captureLogs(t)
			_, reply := syncupCut(t, srv, reqOpts{revision: 3, primary: true})
			if recs := capture.msgRecords(t, msgStrayEnded,
				msgStrayEndFailed); len(recs) != 0 {
				t.Fatalf("a connection that went logged %v", recs)
			}
			if got := node.hasCall(endWrite(testNqn, stray)); got != tc.wrote {
				t.Fatalf("the cut wrote = %v, want %v: %v", got, tc.wrote,
					node.Calls())
			}
			assertOk(t, reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
				"the subsystem")
			assertCodeZero(t, "the converge", reply)
		})
	}
}

// TestADeferredTransfersStrayKeepsItPending: a deferred transfer's
// subsystem reads PROVISIONING where it would read OK, and PENDING, on both
// channels, while a stray connection is left.
func TestADeferredTransfersStrayKeepsItPending(t *testing.T) {
	srv, node := newCutServer(t)
	xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
	opts := func(revision uint64, hosts ...string) reqOpts {
		return reqOpts{revision: revision, primary: true, leftoverOk: true,
			unprovisionedDataLeg: true,
			xfers: []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn,
				OriNsIdx: 1, AllowedHosts: hosts}}}
	}
	syncupBoth(t, srv, opts(2, strayHost))
	stray := node.connectNvmet(xferNqn, strayHost)
	node.connEnds[connDir(xferNqn, stray)] = connEndsNever

	_, reply := syncupCut(t, srv, opts(3))
	for label, row := range map[string]*pb.ResInfo{
		"the converge": reply.GetCntlrInfo().GetXferIdToSubsystem()[testXfer],
		"a read":       checkRound(t, srv, 3).GetXferIdToSubsystem()[testXfer],
	} {
		assertRowStatus(t, row, pb.ResStatus_RES_STATUS_PENDING, label)
	}
	node.endConn(xferNqn, stray)
	_, reply = syncupCut(t, srv, opts(4))
	assertRowStatus(t, reply.GetCntlrInfo().GetXferIdToSubsystem()[testXfer],
		pb.ResStatus_RES_STATUS_PROVISIONING, "the cut transfer")
}

// TestAPromotionWhoseCutHoldsMovesInThePass: a standby promoted while a
// removed host is still connected cuts that host and, once the connection
// is gone within the pass's wait, moves the namespace to optimized in the
// same converge, after the unlink of the host and the write of "fatal", for
// the pool's subsystem and a transfer's, whether the connection goes at the
// write or two listings later; both channels read the rows OK and the code
// is zero.
func TestAPromotionWhoseCutHoldsMovesInThePass(t *testing.T) {
	for _, target := range cutTargets {
		for _, late := range []int{0, 2} {
			name := target.name + "/gone at the write"
			if late > 0 {
				name = target.name + "/gone two listings later"
			}
			t.Run(name, func(t *testing.T) {
				srv, node := newCutServer(t)
				nqn := target.nqn(srv)
				syncupBoth(t, srv, target.opts(2, true, strayHost))
				syncupCut(t, srv, target.opts(3, false, strayHost))
				assertAna(t, node, nqn, "3", "the standby's namespace")
				stray := node.connectNvmet(nqn, strayHost)
				if late > 0 {
					node.connEnds[connDir(nqn, stray)] = late
				}

				node.Reset()
				_, reply := syncupCut(t, srv, target.opts(4, true))
				assertOrder(t, node,
					"cmd rm -f "+agent.NvmetRoot+"/subsystems/"+nqn+
						"/allowed_hosts/"+strayHost,
					endWrite(nqn, stray),
					"writedirect "+anaPath(nqn, 1)+"=1")
				assertAna(t, node, nqn, "1", "the promotion's namespace")
				info := reply.GetCntlrInfo()
				read := checkRound(t, srv, 4)
				assertOk(t, target.row(info), "the converge's subsystem")
				assertOk(t, target.ns(info), "the converge's namespace")
				assertOk(t, target.row(read), "the read's subsystem")
				assertOk(t, target.ns(read), "the read's namespace")
				assertCodeZero(t, "the converge", reply)
				if code := readCode(t, srv, 4); code.GetCode() != 0 {
					t.Fatalf("the read's code %d (%s), want 0",
						code.GetCode(), code.GetDetails())
				}
			})
		}
	}
}

// TestAReusedNameOfAStrayHostIsCutInTurn: the kernel gives a freed cntlid to
// the next connection at once, so a stray host's connection that passed the
// kernel's check before the list change can take the name the cut just
// freed, under the same host or another stray host; each listing writes
// every stray connection it shows, so that connection is ended in the same
// pass and the cut holds.
func TestAReusedNameOfAStrayHostIsCutInTurn(t *testing.T) {
	const otherStray = "nqn.2024-01.io.dnv-it:host:stray2"
	for _, next := range []string{strayHost, otherStray} {
		name := "the same stray host"
		if next == otherStray {
			name = "another stray host"
		}
		t.Run(name, func(t *testing.T) {
			srv, node := newCutServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				subsys: subsysWithHosts(strayHost, keptHost, otherStray)})
			stray := node.connectNvmet(testNqn, strayHost)
			node.connReuse[connDir(testNqn, stray)] = next

			node.Reset()
			_, reply := syncupCut(t, srv, reqOpts{revision: 3, primary: true,
				subsys: subsysWithHosts(keptHost)})
			if got := node.callsMatching(endWrite(testNqn, stray)); len(got) !=
				2 {
				t.Fatalf("writes of the reused name %v, want two, one for "+
					"each connection", got)
			}
			if got := node.nvmetConnsOf(testNqn); len(got) != 0 {
				t.Fatalf("connections %v, want none", got)
			}
			assertOk(t, reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
				"the converge's subsystem")
			assertCodeZero(t, "the converge", reply)
			assertOk(t, checkRound(t, srv, 3).GetSsIdToSubsystem()[testSs],
				"the read's subsystem")
		})
	}
}

// TestThePortLinkWinsOverAPendingCut: an error of the subsystem's port link
// wins over a cut that has not held, on both channels: with a stray
// connection that never goes, a port link that fails reads ERROR naming the
// link on the converge and "not linked to the port" on a read, both named,
// and a node whose port is gone reads "the port is not there" on both,
// named by neither; the subsystem is not safe meanwhile, and a promotion
// stays inaccessible.
func TestThePortLinkWinsOverAPendingCut(t *testing.T) {
	for _, target := range cutTargets {
		for _, portGone := range []bool{false, true} {
			name := target.name + "/the link fails"
			if portGone {
				name = target.name + "/the port is gone"
			}
			t.Run(name, func(t *testing.T) {
				srv, node := newCutServer(t)
				nqn := target.nqn(srv)
				syncupBoth(t, srv, target.opts(2, true, strayHost))
				syncupCut(t, srv, target.opts(3, false, strayHost))
				stray := node.connectNvmet(nqn, strayHost)
				node.connEnds[connDir(nqn, stray)] = connEndsNever
				portLink := agent.NvmetRoot + "/ports/1/subsystems/" + nqn
				if portGone {
					dropPort(node)
				} else {
					delete(node.links, portLink)
					node.failCmdAlways["ln -s "+agent.NvmetRoot+
						"/subsystems/"+nqn+" "+portLink] = "ln: refused"
				}

				node.Reset()
				_, reply := syncupCut(t, srv, target.opts(4, true))
				assertNoCall(t, node, "writedirect "+anaPath(nqn, 1)+"=1")
				readRow := target.row(checkRound(t, srv, 4))
				for label, row := range map[string]*pb.ResInfo{
					"the converge": target.row(reply.GetCntlrInfo()),
					"a read":       readRow,
				} {
					assertRowStatus(t, row, pb.ResStatus_RES_STATUS_ERROR,
						label)
					if strings.Contains(row.GetDetails(), strayHost) {
						t.Fatalf("%s: details %q carry the cut's text",
							label, row.GetDetails())
					}
					if portGone && row.GetDetails() != detailsPortMissing {
						t.Fatalf("%s: details %q, want %q", label,
							row.GetDetails(), detailsPortMissing)
					}
				}
				if !portGone && readRow.GetDetails() != "not linked to the port" {
					t.Fatalf("a read: details %q, want the missing link",
						readRow.GetDetails())
				}
				code := readCode(t, srv, 4)
				for label, details := range map[string]string{
					"the converge": reply.GetAgentReply().GetDetails(),
					"a read":       code.GetDetails(),
				} {
					if named := strings.Contains(details, nqn+": "); named ==
						portGone {
						t.Errorf("%s names the subsystem = %v, want %v: %s",
							label, named, !portGone, details)
					}
					if strings.Contains(details, nqn+": pending") {
						t.Errorf("%s names the cut over the port: %s", label,
							details)
					}
				}
			})
		}
	}
}

// TestAPromotedTransferWhoseOriginIsGoneIsAsBefore: a transfer whose origin
// the request no longer names has no export step, so no list step and no
// cut, and its device's error table serves no data: it is safe, and a
// standby promoted after the origin went moves it to optimized over that
// table, as before the cut, in cut mode and in no cut mode (CN17, CN29). A
// stray connection whose host the same request takes off the transfer is
// neither listed nor ended. Its rows read the origin's error, which the
// verdict names nowhere.
func TestAPromotedTransferWhoseOriginIsGoneIsAsBefore(t *testing.T) {
	for _, cut := range []bool{true, false} {
		name := "no cut mode"
		if cut {
			name = "cut mode"
		}
		t.Run(name, func(t *testing.T) {
			srv, node := newTestServer(t)
			if cut {
				node.withNvmetDebug()
				srv.strayCut = true
			}
			xfers := func(hosts ...string) []*pb.Transfer {
				return []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn,
					OriNsIdx: 1, AllowedHosts: hosts}}
			}
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				xfers: xfers(strayHost)})
			xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
			syncupCut(t, srv, reqOpts{revision: 3, xfers: xfers(strayHost)})
			assertAna(t, node, xferNqn, "3", "the standby's transfer")
			if cut {
				node.connectNvmet(xferNqn, strayHost)
			}

			node.Reset()
			_, reply := syncupCut(t, srv, reqOpts{revision: 4, primary: true,
				xfers: xfers(), tds: []*pb.ThinDevice{},
				subsys: map[string]*pb.Subsystem{}})
			assertAna(t, node, xferNqn, "1", "the promoted transfer")
			if got := node.callsMatching(agent.NvmetDebugRoot + "/" +
				xferNqn); len(got) != 0 {
				t.Fatalf("an origin-gone transfer was read or cut: %v", got)
			}
			if got := node.callsMatching(
				xferNqn + "/allowed_hosts"); len(got) != 0 {
				t.Fatalf("an origin-gone transfer's list was stepped: %v",
					got)
			}
			if cut {
				if got := node.nvmetConnsOf(xferNqn); len(got) != 1 {
					t.Fatalf("the transfer's connections %v, want the stray "+
						"one kept", got)
				}
			}
			info := reply.GetCntlrInfo()
			for label, row := range map[string]*pb.ResInfo{
				"device":    info.GetXferIdToDmLinear()[testXfer],
				"subsystem": info.GetXferIdToSubsystem()[testXfer],
				"namespace": info.GetXferIdToNamespace()[testXfer],
			} {
				assertRowStatus(t, row, pb.ResStatus_RES_STATUS_ERROR, label)
				if !strings.HasPrefix(row.GetDetails(), "origin namespace") {
					t.Fatalf("%s: details %q, want the origin's", label,
						row.GetDetails())
				}
			}
			if strings.Contains(reply.GetAgentReply().GetDetails(), xferNqn) {
				t.Fatalf("the converge names the transfer: %s",
					reply.GetAgentReply().GetDetails())
			}
		})
	}
}

// TestATransferWhoseDeviceStepFailsIsCutFirst: a transfer's list step and
// cut come before its device step, so a converge whose transfer device step
// fails has cut the stray connection already and moved the transfer
// namespace inaccessible, and leaves the rest of the export alone; the
// subsystem row reads PENDING with the text a read gives, from the list
// step and a read of the port link. Once the connection has gone, the
// converge's namespace row is the read's.
func TestATransferWhoseDeviceStepFailsIsCutFirst(t *testing.T) {
	srv, node := newCutServer(t)
	xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
	opts := func(revision uint64, primary bool, hosts ...string) reqOpts {
		return reqOpts{revision: revision, primary: primary,
			leftoverOk: true, xfers: []*pb.Transfer{{XferId: testXfer,
				OriNqn: testNqn, OriNsIdx: 1, AllowedHosts: hosts}}}
	}
	syncupBoth(t, srv, opts(2, true, strayHost))
	assertAna(t, node, xferNqn, "1", "the serving transfer")
	stray := node.connectNvmet(xferNqn, strayHost)
	node.connEnds[connDir(xferNqn, stray)] = connEndsNever
	table := "dmsetup table " + xferName(srv, testXfer)
	node.failCmdAlways[table] = "device-mapper: table ioctl failed"

	node.Reset()
	held, reply := syncupCut(t, srv, opts(3, true))
	assertCallBefore(t, node, endWrite(xferNqn, stray), table)
	assertCallBefore(t, node, "writedirect "+anaPath(xferNqn, 1)+"=3", table)
	assertNoCall(t, node, "ln -s "+agent.NvmetRoot+"/subsystems/"+xferNqn)
	assertAna(t, node, xferNqn, "3", "the transfer whose device step failed")
	want := "host " + strayHost + " still connected"
	for label, row := range map[string]*pb.ResInfo{
		"the converge": reply.GetCntlrInfo().GetXferIdToSubsystem()[testXfer],
		"a read":       checkRound(t, srv, 3).GetXferIdToSubsystem()[testXfer],
	} {
		assertRowStatus(t, row, pb.ResStatus_RES_STATUS_PENDING, label)
		if row.GetDetails() != want {
			t.Fatalf("%s: details %q, want %q", label, row.GetDetails(), want)
		}
	}

	node.endConn(xferNqn, stray)
	reply, err := srv.SyncupCntlr(context.Background(), held)
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	converged := reply.GetCntlrInfo().GetXferIdToNamespace()[testXfer]
	read := checkRound(t, srv, 3).GetXferIdToNamespace()[testXfer]
	if converged.GetStatus() != read.GetStatus() ||
		converged.GetDetails() != read.GetDetails() {
		t.Fatalf("the converge's transfer namespace %v/%q, the read's "+
			"%v/%q: want the same row", converged.GetStatus(),
			converged.GetDetails(), read.GetStatus(), read.GetDetails())
	}
}

// TestAPoolSubsystemHoldsNoTransfer: safety is per subsystem in the other
// direction too: a pool subsystem whose cut has not held, or whose list
// still admits a removed host, keeps its own namespace dark at a promotion
// and never a transfer of that namespace, which moves to optimized and
// reads OK on both channels, unnamed (CN16, CN17).
func TestAPoolSubsystemHoldsNoTransfer(t *testing.T) {
	const otherHost = "nqn.2024-01.io.dnv-it:host:other"
	for _, tc := range []struct {
		name   string
		cut    bool
		failed bool
	}{
		{"the pool's cut not held", true, false},
		{"the pool's step failed, cut mode", true, true},
		{"the pool's step failed, no cut mode", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			if tc.cut {
				node.withNvmetDebug()
				srv.strayCut = true
			}
			xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
			opts := func(revision uint64, primary bool,
				poolHosts ...string) reqOpts {
				return reqOpts{revision: revision, primary: primary,
					leftoverOk: true, subsys: subsysWithHosts(poolHosts...),
					xfers: []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn,
						OriNsIdx: 1, AllowedHosts: []string{otherHost}}}}
			}
			syncupBoth(t, srv, opts(2, true, strayHost))
			syncupCut(t, srv, opts(3, false, strayHost))
			assertAna(t, node, xferNqn, "3", "the standby's transfer")
			poolHosts := []string{}
			if tc.failed {
				poolHosts = []string{keptHost}
				node.failCmdAlways["ln -s "+agent.NvmetRoot+"/hosts/"+keptHost+
					" "+agent.NvmetRoot+"/subsystems/"+testNqn+
					"/allowed_hosts/"+keptHost] = "ln: Invalid argument"
			} else {
				stray := node.connectNvmet(testNqn, strayHost)
				node.connEnds[connDir(testNqn, stray)] = connEndsNever
			}

			node.Reset()
			_, reply := syncupCut(t, srv, opts(4, true, poolHosts...))
			assertAna(t, node, testNqn, "3", "the pool's namespace")
			assertAna(t, node, xferNqn, "1", "the transfer")
			read := checkRound(t, srv, 4)
			for label, info := range map[string]*pb.CntlrInfo{
				"the converge": reply.GetCntlrInfo(), "a read": read,
			} {
				assertOk(t, info.GetXferIdToSubsystem()[testXfer],
					label+"'s transfer subsystem")
				assertOk(t, info.GetXferIdToNamespace()[testXfer],
					label+"'s transfer namespace")
			}
			details := reply.GetAgentReply().GetDetails()
			if !strings.Contains(details, testNqn+": ") ||
				strings.Contains(details, xferNqn) {
				t.Fatalf("the converge's code %s: want the pool subsystem "+
					"named and the transfer not", details)
			}
		})
	}
}

// TestAnAllowedHostTakingAWrittenNameIsNotEnded: the kernel ends a written
// connection a moment after the write, while later listings still show
// it, and gives its freed name to the next connection at once; an allowed
// host's new connection that takes the name between a later listing's read
// and its write is not ended: the pass writes a pair it wrote again only
// when its "state" shows a new connection, and reads the host again before
// each write.
func TestAnAllowedHostTakingAWrittenNameIsNotEnded(t *testing.T) {
	srv, node := newCutServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		subsys: subsysWithHosts(strayHost, keptHost)})
	stray := node.connectNvmet(testNqn, strayHost)
	dir := connDir(testNqn, stray)
	node.connEnds[dir] = 2
	var hook func()
	hook = func() {
		if node.files[dir+"/state"] == "fatal\n" {
			// A later read of the written connection's host; now the
			// kernel frees it and the allowed host takes the name.
			node.dropConn(dir)
			node.plantConn(dir, keptHost)
			return
		}
		node.afterRead[dir+"/hostnqn"] = hook
	}
	node.afterRead[dir+"/hostnqn"] = hook

	node.Reset()
	_, reply := syncupCut(t, srv, reqOpts{revision: 3, primary: true,
		subsys: subsysWithHosts(keptHost)})
	if got := node.callsMatching(endWrite(testNqn, stray)); len(got) != 1 {
		t.Fatalf("writes of the name %v, want the one cut", got)
	}
	if got, want := node.nvmetConnsOf(testNqn),
		[]string{stray + "=" + keptHost}; !slices.Equal(got, want) {
		t.Fatalf("connections %v, want the allowed host's kept: %v", got,
			want)
	}
	assertOk(t, reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
		"the converge's subsystem")
	assertCodeZero(t, "the converge", reply)
}

// TestAStrayThatWentAndCameBackIsWrittenAgain: a pair of name and host that
// a listing no longer shows is forgotten, so a new connection of the same
// stray host that takes the freed name later in the pass is written again,
// even while its state reads nothing, as a connection not yet enabled does.
func TestAStrayThatWentAndCameBackIsWrittenAgain(t *testing.T) {
	srv, node := newCutServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		subsys: subsysWithHosts(strayHost, keptHost)})
	stays := node.connectNvmet(testNqn, strayHost)
	node.connEnds[connDir(testNqn, stays)] = connEndsNever
	back := node.connectNvmet(testNqn, strayHost)
	debugDir := agent.NvmetDebugRoot + "/" + testNqn
	listings := 0
	var hook func()
	hook = func() {
		listings++
		if listings == 2 {
			// The second listing showed the pair gone; the stray host's
			// new connection takes the name, not yet enabled.
			node.plantConn(debugDir+"/"+back, strayHost)
			node.files[debugDir+"/"+back+"/state"] = ""
			return
		}
		node.afterList[debugDir] = hook
	}
	node.afterList[debugDir] = hook

	node.Reset()
	syncupCut(t, srv, reqOpts{revision: 3, primary: true,
		subsys: subsysWithHosts(keptHost)})
	if got := node.callsMatching(endWrite(testNqn, back)); len(got) != 2 {
		t.Fatalf("writes of the name that came back %v, want two", got)
	}
}

// TestARemovedDebugFileIsAConnectionThatHasGone: debugfs answers EIO to a
// read or a write through a file the kernel removed after its open; a
// "hostnqn" that answers so, at the listing or at the write's own read of
// it, is read once more and, now not there, is a connection that has gone,
// and a "state" write that answers so is a connection that has gone: the
// cut holds, with no refusal logged.
func TestARemovedDebugFileIsAConnectionThatHasGone(t *testing.T) {
	for _, file := range []string{"hostnqn", "state", "hostnqn at the write"} {
		t.Run(file, func(t *testing.T) {
			srv, node := newCutServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				subsys: subsysWithHosts(strayHost, keptHost)})
			stray := node.connectNvmet(testNqn, strayHost)
			dir := connDir(testNqn, stray)
			if file == "hostnqn at the write" {
				// The listing reads the host; the write's own read of it
				// meets the file removed after its open.
				node.afterRead[dir+"/hostnqn"] = func() {
					node.connEIO[dir+"/hostnqn"] = true
				}
			} else {
				node.connEIO[dir+"/"+file] = true
			}

			node.Reset()
			capture := captureLogs(t)
			_, reply := syncupCut(t, srv, reqOpts{revision: 3, primary: true,
				subsys: subsysWithHosts(keptHost)})
			assertOk(t, reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
				"the converge's subsystem")
			assertCodeZero(t, "the converge", reply)
			if recs := capture.msgRecords(t, msgStrayEndFailed); len(recs) !=
				0 {
				t.Fatalf("a connection that went logged a refusal: %v", recs)
			}
			if got := node.nvmetConnsOf(testNqn); len(got) != 0 {
				t.Fatalf("connections %v, want none", got)
			}
		})
	}
}

// TestAReusedIndexIsANewNamespace: a request that gives a serving
// namespace's index to another namespace makes the step disable it and
// enable it again under the new device and identity: a new namespace,
// enabled inaccessible, which the last loop moves to optimized in the same
// pass when the subsystem is safe, the cut held or in no cut mode (CN16).
// While a removed host is still connected the subsystem is not safe: the
// namespace is moved inaccessible before the ns-dev loop and stays there,
// its row OK and the subsystem's PENDING on both channels.
func TestAReusedIndexIsANewNamespace(t *testing.T) {
	const newNs = testNs + 0x100
	nsPath := agent.NvmetRoot + "/subsystems/" + testNqn + "/namespaces/1"
	for _, tc := range []struct {
		name  string
		cut   bool
		stays bool
	}{
		{"cut mode, a stray that never goes", true, true},
		{"cut mode, the cut held", true, false},
		{"no cut mode", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			if tc.cut {
				node.withNvmetDebug()
				srv.strayCut = true
			}
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				subsys: subsysWithHosts(strayHost, keptHost)})
			assertAna(t, node, testNqn, "1", "the serving namespace")
			if tc.cut {
				stray := node.connectNvmet(testNqn, strayHost)
				if tc.stays {
					node.connEnds[connDir(testNqn, stray)] = connEndsNever
				}
			}
			sub := subsysWithHosts(keptHost)
			sub[testNqn].NsList[0].NsId = newNs
			sub[testNqn].NsList[0].DevUuid =
				"22222222-2222-4222-8222-222222222222"
			sub[testNqn].NsList[0].DevNguid =
				"22222222222242228222222222222222"

			node.Reset()
			_, reply := syncupCut(t, srv, reqOpts{revision: 3, primary: true,
				leftoverOk: true, subsys: sub})
			if got := strings.TrimSpace(node.fileOf(nsPath + "/device_path")); got !=
				"/dev/mapper/"+nsDevName(srv, newNs) ||
				node.fileOf(nsPath+"/enable") != "1" {
				t.Fatalf("the index is over %q, enable %q: the case is "+
					"vacuous", got, node.fileOf(nsPath+"/enable"))
			}
			ana := "writedirect " + anaPath(testNqn, 1) + "="
			if tc.stays {
				assertCallBefore(t, node, ana+"3",
					"cmd dmsetup create "+nsDevName(srv, newNs))
				assertNoCall(t, node, ana+"1")
				assertAna(t, node, testNqn, "3", "the new namespace")
			} else {
				assertOrder(t, node, "writedirect "+nsPath+"/enable=0",
					ana+"3", "writedirect "+nsPath+"/enable=1", ana+"1")
				assertAna(t, node, testNqn, "1", "the new namespace")
			}
			read := checkRound(t, srv, 3)
			for label, info := range map[string]*pb.CntlrInfo{
				"the converge": reply.GetCntlrInfo(), "a read": read,
			} {
				assertOk(t, info.GetNsIdToNamespace()[newNs],
					label+"'s namespace")
				want := pb.ResStatus_RES_STATUS_OK
				if tc.stays {
					want = pb.ResStatus_RES_STATUS_PENDING
				}
				assertRowStatus(t, info.GetSsIdToSubsystem()[testSs], want,
					label+"'s subsystem")
			}
		})
	}
}

// TestAFailedReadOfAConnectionIsACutNotHeld: a read of a connection that
// fails ends the cut not held, the read's own record carrying the error,
// with no refusal logged, and the subsystem is not safe, the two channels
// giving the same row: a failed read of its host, which a read's listing
// meets too, "nvmet connections not read", an ERROR row; a failed read of
// a written connection's "state", which a read never makes, the stray
// still listed, PENDING (CN33).
func TestAFailedReadOfAConnectionIsACutNotHeld(t *testing.T) {
	for _, file := range []string{"state", "hostnqn"} {
		t.Run(file, func(t *testing.T) {
			srv, node := newCutServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				subsys: subsysWithHosts(strayHost, keptHost)})
			stray := node.connectNvmet(testNqn, strayHost)
			dir := connDir(testNqn, stray)
			if file == "state" {
				// Still listed after the write, so the next listing reads
				// its state, which fails.
				node.connEnds[dir] = 2
				node.failReadAlways[dir+"/state"] = true
			} else {
				node.afterRead[dir+"/hostnqn"] = func() {
					node.failReadAlways[dir+"/hostnqn"] = true
				}
			}

			node.Reset()
			capture := captureLogs(t)
			_, reply := syncupCut(t, srv, reqOpts{revision: 3, primary: true,
				leftoverOk: true, subsys: subsysWithHosts(keptHost)})
			row := reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs]
			status := pb.ResStatus_RES_STATUS_ERROR
			want := detailsConnsUnread + "read " + dir + "/" + file
			if file == "state" {
				status = pb.ResStatus_RES_STATUS_PENDING
				want = "host " + strayHost + " still connected"
			}
			assertRowStatus(t, row, status, "the subsystem")
			if !strings.HasPrefix(row.GetDetails(), want) {
				t.Fatalf("details %q, want them to start %q",
					row.GetDetails(), want)
			}
			assertAna(t, node, testNqn, "3", "the namespace")
			read := checkRound(t, srv, 3).GetSsIdToSubsystem()[testSs]
			if read.GetStatus() != row.GetStatus() ||
				read.GetDetails() != row.GetDetails() {
				t.Fatalf("the read's row %v/%q, the converge's %v/%q: "+
					"want the same", read.GetStatus(), read.GetDetails(),
					row.GetStatus(), row.GetDetails())
			}
			if recs := capture.msgRecords(t, msgStrayEndFailed); len(recs) !=
				0 {
				t.Fatalf("a failed read logged a refusal: %v", recs)
			}
		})
	}
}

// TestANewConnectionNotEnabledYetIsCutLater: a stray host's new connection
// that took the name the pass wrote reads nothing in its state until its
// host enables it; the pass does not write it then, which only delays its
// cut: to a later listing once it reads "ready", or, when the pass's wait
// ends first, to the next pass, which writes it at once.
func TestANewConnectionNotEnabledYetIsCutLater(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "enabled at the next listing"
		if !enabled {
			name = "not enabled in the pass"
		}
		t.Run(name, func(t *testing.T) {
			srv, node := newCutServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				subsys: subsysWithHosts(strayHost, keptHost)})
			stray := node.connectNvmet(testNqn, strayHost)
			dir := connDir(testNqn, stray)
			node.connReuse[dir] = strayHost
			debugDir := agent.NvmetDebugRoot + "/" + testNqn
			listings := 0
			var hook func()
			hook = func() {
				listings++
				switch {
				case listings == 1:
					// After the first listing: the write, then the reuse;
					// the new connection is not enabled yet.
					node.afterList[debugDir] = hook
				case listings == 2:
					node.files[dir+"/state"] = ""
					if enabled {
						node.afterList[debugDir] = hook
					}
				case listings == 3:
					node.files[dir+"/state"] = "ready\n"
				}
			}
			node.afterList[debugDir] = hook

			node.Reset()
			held, reply := syncupCut(t, srv, reqOpts{revision: 3,
				primary: true, leftoverOk: true,
				subsys: subsysWithHosts(keptHost)})
			writes := len(node.callsMatching(endWrite(testNqn, stray)))
			if enabled {
				if writes != 2 {
					t.Fatalf("writes %d, want the first and the new "+
						"connection's once enabled", writes)
				}
				assertOk(t, reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
					"the subsystem")
				return
			}
			if writes != 1 {
				t.Fatalf("writes %d, want only the first in the pass", writes)
			}
			assertRowStatus(t,
				reply.GetCntlrInfo().GetSsIdToSubsystem()[testSs],
				pb.ResStatus_RES_STATUS_PENDING, "the subsystem")
			node.Reset()
			if _, again := resendRound(t, srv, held, "next-pass"); again ==
				nil || !node.hasCall(endWrite(testNqn, stray)) {
				t.Fatalf("the next pass did not write the new connection: %v",
					node.Calls())
			}
		})
	}
}

// TestATransferReEnabledUnderAnotherDeviceIsANewNamespace: a transfer
// namespace found enabled over another device is disabled and enabled
// again in the inaccessible group, a new namespace (CN17): no request
// reaches it through the gateway, so the state is planted. When the
// transfer's subsystem is safe, the last loop moves it to optimized in the
// same pass; while a removed host's cut does not hold, it is moved
// inaccessible before its device step and stays there.
func TestATransferReEnabledUnderAnotherDeviceIsANewNamespace(t *testing.T) {
	for _, held := range []bool{false, true} {
		name := "the cut not held"
		if held {
			name = "the cut held"
		}
		t.Run(name, func(t *testing.T) {
			srv, node := newCutServer(t)
			xfers := func(hosts ...string) []*pb.Transfer {
				return []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn,
					OriNsIdx: 1, AllowedHosts: hosts}}
			}
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				xfers: xfers(strayHost)})
			xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
			nsPath := agent.NvmetRoot + "/subsystems/" + xferNqn +
				"/namespaces/1"
			node.files[nsPath+"/device_path"] = "/dev/mapper/another\n"
			assertAna(t, node, xferNqn, "1", "the planted namespace")
			stray := node.connectNvmet(xferNqn, strayHost)
			if !held {
				node.connEnds[connDir(xferNqn, stray)] = connEndsNever
			}

			node.Reset()
			_, reply := syncupCut(t, srv, reqOpts{revision: 3, primary: true,
				leftoverOk: true, xfers: xfers()})
			ana := "writedirect " + anaPath(xferNqn, 1) + "="
			if held {
				assertOrder(t, node, "writedirect "+nsPath+"/enable=0",
					ana+"3", "writedirect "+nsPath+"/enable=1", ana+"1")
				assertAna(t, node, xferNqn, "1", "the cut held")
				return
			}
			assertCallBefore(t, node, ana+"3", xferName(srv, testXfer))
			assertOrder(t, node, ana+"3", "writedirect "+nsPath+"/enable=0",
				"writedirect "+nsPath+"/enable=1")
			assertNoCall(t, node, ana+"1")
			assertAna(t, node, xferNqn, "3", "the cut not held")
			assertRowStatus(t,
				reply.GetCntlrInfo().GetXferIdToSubsystem()[testXfer],
				pb.ResStatus_RES_STATUS_PENDING, "the transfer subsystem")
			assertOk(t, reply.GetCntlrInfo().GetXferIdToNamespace()[testXfer],
				"the transfer namespace")
		})
	}
}

// assertCallBefore fails unless node holds a call containing first and one
// containing then, the first of the one before the first of the other.
func assertCallBefore(t *testing.T, node *fakeNode, first, then string) {
	t.Helper()
	i, j := node.indexOfCall(first), node.indexOfCall(then)
	if i < 0 || j < 0 || i > j {
		t.Fatalf("call %q at %d, call %q at %d: want both, the first one "+
			"first", first, i, then, j)
	}
}

// devStepCall is the first call of the build's device step of the target's
// namespace: the read of the ns-dev's, or the transfer device's, attributes.
func devStepCall(srv *CnAgentServer, target cutTarget) string {
	dev := nsDevName(srv, testNs)
	if target.isXfer() {
		dev = xferName(srv, testXfer)
	}
	return "dmsetup info --columns --noheadings -o attr " + dev
}

// unsafeCause is one way a subsystem is not safe in a converge (CN33),
// armed on a node whose subsystem admits strayHost and holds its stray
// connection, before the pass that takes strayHost off the list.
type unsafeCause struct {
	name string
	arm  func(node *fakeNode, nqn, stray string)
	// status and text, a prefix, are the converge's subsystem row, and
	// readStatus and readText a read's of the same node after it; named
	// and readNamed are how the two verdicts name the subsystem, after its
	// NQN, named "" a row the converge leaves to the read (cutErrorRow).
	status     pb.ResStatus
	text       string
	readStatus pb.ResStatus
	readText   string
	named      string
	readNamed  string
	// noCut: the list still admits the removed host, so no cut runs;
	// refusal: the kernel refuses the cut's write, which the cut logs.
	noCut   bool
	refusal bool
}

func unsafeCauses() []unsafeCause {
	connected := "host " + strayHost + " still connected"
	return []unsafeCause{{
		name: "a stray that never goes",
		arm: func(node *fakeNode, nqn, stray string) {
			node.connEnds[connDir(nqn, stray)] = connEndsNever
		},
		status: pb.ResStatus_RES_STATUS_PENDING, text: connected,
		readStatus: pb.ResStatus_RES_STATUS_PENDING, readText: connected,
		named: ": pending: " + connected, readNamed: ": pending: " + connected,
	}, {
		name: "no debug directory",
		arm: func(node *fakeNode, nqn, _ string) {
			delete(node.dirs, agent.NvmetDebugRoot+"/"+nqn)
		},
		status:     pb.ResStatus_RES_STATUS_ERROR,
		text:       detailsConnsUnread + "no " + agent.NvmetDebugRoot,
		readStatus: pb.ResStatus_RES_STATUS_ERROR,
		readText:   detailsConnsUnread + "no " + agent.NvmetDebugRoot,
		readNamed:  ": differs: " + detailsConnsUnread,
	}, {
		name: "a refused write",
		arm: func(node *fakeNode, nqn, stray string) {
			node.failWriteAlways[connDir(nqn, stray)+"/state=fatal"] =
				"operation not permitted"
		},
		status: pb.ResStatus_RES_STATUS_PENDING, text: connected,
		readStatus: pb.ResStatus_RES_STATUS_PENDING, readText: connected,
		named: ": pending: " + connected, readNamed: ": pending: " + connected,
		refusal: true,
	}, {
		name: "a refused write, no lockdown file",
		arm: func(node *fakeNode, nqn, stray string) {
			node.failWriteAlways[connDir(nqn, stray)+"/state=fatal"] =
				"operation not permitted"
			delete(node.files, agent.LockdownPath)
		},
		status: pb.ResStatus_RES_STATUS_PENDING, text: connected,
		readStatus: pb.ResStatus_RES_STATUS_PENDING, readText: connected,
		named: ": pending: " + connected, readNamed: ": pending: " + connected,
		refusal: true,
	}, {
		name: "a refused write, the lockdown file not read",
		arm: func(node *fakeNode, nqn, stray string) {
			node.failWriteAlways[connDir(nqn, stray)+"/state=fatal"] =
				"operation not permitted"
			node.failReadAlways[agent.LockdownPath] = true
		},
		status: pb.ResStatus_RES_STATUS_PENDING, text: connected,
		readStatus: pb.ResStatus_RES_STATUS_PENDING, readText: connected,
		named: ": pending: " + connected, readNamed: ": pending: " + connected,
		refusal: true,
	}, {
		name: "a failed unlink, the list not read",
		arm: func(node *fakeNode, nqn, _ string) {
			node.failCmdAlways["rm -f "+agent.NvmetRoot+"/subsystems/"+nqn+
				"/allowed_hosts/"+strayHost] = "rm: Operation not permitted"
			node.failReadAlways[agent.NvmetRoot+"/subsystems/"+nqn+
				"/attr_allow_any_host"] = true
		},
		status:     pb.ResStatus_RES_STATUS_ERROR,
		readStatus: pb.ResStatus_RES_STATUS_ERROR,
		readText:   "attr_allow_any_host",
		named:      ": differs",
		readNamed:  ": differs: attr_allow_any_host",
		noCut:      true,
	}, {
		name: "a lockdown in force",
		arm: func(node *fakeNode, nqn, stray string) {
			node.failWriteAlways[connDir(nqn, stray)+"/state=fatal"] =
				"operation not permitted"
			node.files[agent.LockdownPath] =
				"none [integrity] confidentiality\n"
		},
		status:     pb.ResStatus_RES_STATUS_ERROR,
		text:       detailsConnsNotEnded + "lockdown integrity",
		readStatus: pb.ResStatus_RES_STATUS_ERROR,
		readText:   detailsConnsNotEnded + "lockdown integrity",
		readNamed:  ": differs: " + detailsConnsNotEnded + "lockdown integrity",
		refusal:    true,
	}, {
		name: "a failed unlink",
		arm: func(node *fakeNode, nqn, _ string) {
			node.failCmdAlways["rm -f "+agent.NvmetRoot+"/subsystems/"+nqn+
				"/allowed_hosts/"+strayHost] = "rm: Operation not permitted"
		},
		status:     pb.ResStatus_RES_STATUS_ERROR,
		readStatus: pb.ResStatus_RES_STATUS_ERROR,
		readText:   "unexpected allowed host " + strayHost,
		named:      ": differs",
		readNamed:  ": differs: unexpected allowed host " + strayHost,
		noCut:      true,
	}}
}

// TestAnUnsafeSubsystemDarkensAServingPrimary: a serving primary whose
// subsystem is not safe in a pass — a stray connection that never goes, no
// debug directory, a write of "fatal" the kernel refuses, with a lockdown in
// force or without one, or an unlink of the removed host that fails —
// moves the subsystem's namespace to
// inaccessible right after the list step, before its device step, and
// keeps it there to the pass's end, for the pool's subsystem and a
// transfer's (CN33). The subsystem row reads, on both channels alike,
// PENDING for a stray still listed, a refused write's included, ERROR for
// connections that cannot be read, or that a lockdown the lockdown file
// shows keeps from
// being ended, which only a read names, and ERROR for a list that still
// admits the removed host; a read writes nothing. The namespace row wants
// the inaccessible group, so it reads OK on both channels. A transfer's
// subsystem keeps the pool's namespace serving.
func TestAnUnsafeSubsystemDarkensAServingPrimary(t *testing.T) {
	for _, target := range cutTargets {
		for _, cause := range unsafeCauses() {
			t.Run(target.name+"/"+cause.name, func(t *testing.T) {
				srv, node := newCutServer(t)
				nqn := target.nqn(srv)
				syncupBoth(t, srv, target.opts(2, true, strayHost, keptHost))
				assertAna(t, node, nqn, "1", "the serving namespace")
				stray := node.connectNvmet(nqn, strayHost)
				cause.arm(node, nqn, stray)

				node.Reset()
				capture := captureLogs(t)
				_, reply := syncupCut(t, srv, target.opts(3, true, keptHost))
				ana := "writedirect " + anaPath(nqn, 1) + "="
				assertCallBefore(t, node, ana+"3", devStepCall(srv, target))
				assertNoCall(t, node, ana+"1")
				assertAna(t, node, nqn, "3", "the namespace")
				if cause.noCut {
					assertNoCall(t, node, agent.NvmetDebugRoot+"/"+nqn)
				}
				info := reply.GetCntlrInfo()
				read := checkRound(t, srv, 3)
				for _, ch := range []struct {
					label  string
					row    *pb.ResInfo
					status pb.ResStatus
					text   string
				}{
					{"the converge's subsystem", target.row(info),
						cause.status, cause.text},
					{"the read's subsystem", target.row(read),
						cause.readStatus, cause.readText},
				} {
					assertRowStatus(t, ch.row, ch.status, ch.label)
					if !strings.HasPrefix(ch.row.GetDetails(), ch.text) {
						t.Fatalf("%s: details %q, want them to start %q",
							ch.label, ch.row.GetDetails(), ch.text)
					}
				}
				if cause.noCut {
					// A list step that failed steps no namespace object,
					// which then has no row of the converge.
					if row := target.ns(info); row != nil {
						t.Fatalf("the converge's namespace row %v, want none",
							row)
					}
				} else {
					assertOk(t, target.ns(info), "the converge's namespace")
				}
				assertOk(t, target.ns(read), "the read's namespace")
				if cause.named == "" {
					if strings.Contains(reply.GetAgentReply().GetDetails(),
						nqn+":") {
						t.Fatalf("the converge names the subsystem: %s",
							reply.GetAgentReply().GetDetails())
					}
				} else {
					assertCodeNames(t, "the converge", reply, nqn+cause.named)
				}
				node.Reset()
				if code := readCode(t, srv, 3); code.GetCode() !=
					common.ReplyCodeLeftover || !strings.Contains(
					code.GetDetails(), nqn+cause.readNamed) {
					t.Fatalf("the read's code %d (%s), want %q named",
						code.GetCode(), code.GetDetails(), nqn+cause.readNamed)
				}
				if muts := node.Mutations(); len(muts) != 0 {
					t.Fatalf("the read changed the node: %v", muts)
				}
				assertPoolNsServes(t, srv, node, target, reply, 3)
				if !cause.refusal {
					return
				}
				recs := capture.msgRecords(t, msgStrayEndFailed)
				if len(recs) != 1 || recs[0]["level"] != "ERROR" ||
					recs[0]["nqn"] != nqn || recs[0]["connection"] != stray ||
					recs[0]["host_nqn"] != strayHost ||
					!strings.Contains(fmt.Sprint(recs[0]["error"]),
						"operation not permitted") {
					t.Fatalf("the refusal's records %v: want one at ERROR "+
						"naming the subsystem, the connection, its host and "+
						"the refusal", recs)
				}
			})
		}
	}
}

// TestAFailedAddOfAHostIsSafe: a list step that fails only to add a host,
// in a request that removes no host, leaves the kernel admitting no host
// off the desired list, which a strict read finds, so the subsystem is
// safe (a failed link in a request that also removes a host stops before
// the unlink: TestAFailedSubsystemStepAtAPromotionKeepsInaccessible): in
// cut mode the cut runs and holds, and the serving namespace stays
// optimized with no move down, in both
// modes, for the pool's subsystem and a transfer's; the subsystem row names
// the step, and a read reads the namespace row OK (CN33).
func TestAFailedAddOfAHostIsSafe(t *testing.T) {
	const newHost = "nqn.2024-01.io.dnv-it:host:new"
	for _, target := range cutTargets {
		for _, cut := range []bool{true, false} {
			name := target.name + "/no cut mode"
			if cut {
				name = target.name + "/cut mode"
			}
			t.Run(name, func(t *testing.T) {
				srv, node := newTestServer(t)
				if cut {
					node.withNvmetDebug()
					srv.strayCut = true
				}
				nqn := target.nqn(srv)
				syncupBoth(t, srv, target.opts(2, true, keptHost))
				assertAna(t, node, nqn, "1", "the serving namespace")
				node.failCmdAlways["ln -s "+agent.NvmetRoot+"/hosts/"+newHost] =
					"ln: Invalid argument"

				node.Reset()
				_, reply := syncupCut(t, srv,
					target.opts(3, true, keptHost, newHost))
				if !node.hasCall("ln -s " + agent.NvmetRoot + "/hosts/" +
					newHost) {
					t.Fatalf("the case is vacuous: no link of the new host")
				}
				assertNoCall(t, node, "writedirect "+anaPath(nqn, 1)+"=3")
				assertAna(t, node, nqn, "1", "the namespace")
				if cut && !node.hasCall("list "+agent.NvmetDebugRoot+"/"+nqn) {
					t.Fatalf("the cut did not run after a failed add")
				}
				assertRowStatus(t, target.row(reply.GetCntlrInfo()),
					pb.ResStatus_RES_STATUS_ERROR, "the subsystem")
				assertOk(t, target.ns(checkRound(t, srv, 3)),
					"the read's namespace")
			})
		}
	}
}

// TestTheCutComesBeforeTheDeviceSteps: a converge steps a subsystem's list
// and cuts its stray connections before the device step that puts data
// under its namespace: a standby promoted while a removed host is
// connected unlinks that host and writes "fatal" to its connection before
// it reloads the ns-dev, or the transfer device, onto the raid0, and moves
// the namespace to optimized last, the cut having held (CN9, CN33).
func TestTheCutComesBeforeTheDeviceSteps(t *testing.T) {
	for _, target := range cutTargets {
		t.Run(target.name, func(t *testing.T) {
			srv, node := newCutServer(t)
			nqn := target.nqn(srv)
			syncupBoth(t, srv, target.opts(2, true, strayHost))
			syncupCut(t, srv, target.opts(3, false, strayHost))
			assertAna(t, node, nqn, "3", "the standby's namespace")
			stray := node.connectNvmet(nqn, strayHost)
			dev := nsDevName(srv, testNs)
			if target.isXfer() {
				dev = xferName(srv, testXfer)
			}

			node.Reset()
			syncupCut(t, srv, target.opts(4, true))
			reload := "cmd dmsetup reload " + dev + " --table " +
				nsDevTableOn(devNoOf(node, raid0Name(srv, testTd)))
			assertOrder(t, node,
				"cmd rm -f "+agent.NvmetRoot+"/subsystems/"+nqn+
					"/allowed_hosts/"+strayHost,
				endWrite(nqn, stray), reload,
				"writedirect "+anaPath(nqn, 1)+"=1")
			assertCallBefore(t, node, endWrite(nqn, stray), reload)
		})
	}
}

// unsafeScenario is a shape in which a review of the cut found a way past
// an earlier rule (cnagent.md CN33): build runs the steps before the
// stray host connects and returns the last request, the namespace row's
// id, and the call of the last pass that puts data under the namespace,
// or that starts its device step where that step fails; arm runs right
// before the last pass. quiet says the last pass of a safe subsystem writes
// no ANA group at all, as before the cut, and heldAna is the group that
// pass leaves, "1" unless set.
type unsafeScenario struct {
	name  string
	xfer  bool
	build func(t *testing.T, srv *CnAgentServer, node *fakeNode) (
		reqOpts, uint64, func() string)
	arm     func(srv *CnAgentServer, node *fakeNode)
	quiet   bool
	heldAna string
}

// refusedDown runs o with the move of nqn's namespace to inaccessible
// refused, so the namespace stays optimized over the park.
func refusedDown(t *testing.T, srv *CnAgentServer, node *fakeNode,
	nqn string, o reqOpts) {
	t.Helper()
	node.failWriteAlways[anaPath(nqn, 1)+"=3"] = "refused"
	o.leftoverOk = true
	syncupCut(t, srv, o)
	delete(node.failWriteAlways, anaPath(nqn, 1)+"=3")
	assertAna(t, node, nqn, "1", "optimized over the park")
}

// reloadOnto is the reload of dev onto td's raid0, read after the pass.
func reloadOnto(srv *CnAgentServer, node *fakeNode, dev string,
	td uint64) func() string {
	return func() string {
		return "cmd dmsetup reload " + dev + " --table " +
			nsDevTableOn(devNoOf(node, raid0Name(srv, td)))
	}
}

func unsafeScenarios() []unsafeScenario {
	hosts := func(h ...string) map[string]*pb.Subsystem {
		return subsysWithHosts(h...)
	}
	onTd := func(td uint64, h ...string) map[string]*pb.Subsystem {
		sub := subsysForTd(td)
		sub[testNqn].AllowedHosts = h
		return sub
	}
	twoTds := func() []*pb.ThinDevice {
		return append(defaultTds(), &pb.ThinDevice{TdId: testSnapTd,
			DevId: 2, Size: testTdSize})
	}
	xfers := func(h ...string) []*pb.Transfer {
		return []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn,
			OriNsIdx: 1, AllowedHosts: h}}
	}
	both := []string{strayHost, keptHost}
	return []unsafeScenario{{
		name: "a promotion after a failed demotion",
		build: func(t *testing.T, srv *CnAgentServer, node *fakeNode) (
			reqOpts, uint64, func() string) {
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				subsys: hosts(both...)})
			refusedDown(t, srv, node, testNqn,
				reqOpts{revision: 3, subsys: hosts(both...)})
			return reqOpts{revision: 4, primary: true,
					subsys: hosts(keptHost)}, testNs,
				reloadOnto(srv, node, nsDevName(srv, testNs), testTd)
		},
		quiet: true,
	}, {
		name: "an unpark after a failed suspend",
		build: func(t *testing.T, srv *CnAgentServer, node *fakeNode) (
			reqOpts, uint64, func() string) {
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				subsys: hosts(both...)})
			susp := defaultSubsys(true)
			susp[testNqn].AllowedHosts = both
			refusedDown(t, srv, node, testNqn,
				reqOpts{revision: 3, primary: true, subsys: susp})
			return reqOpts{revision: 4, primary: true,
					subsys: hosts(keptHost)}, testNs,
				reloadOnto(srv, node, nsDevName(srv, testNs), testTd)
		},
		quiet: true,
	}, {
		name: "the level that removes the thin pools lowered",
		build: func(t *testing.T, srv *CnAgentServer, node *fakeNode) (
			reqOpts, uint64, func() string) {
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				subsys: hosts(both...)})
			syncupCut(t, srv, reqOpts{revision: 3, primary: true,
				leftoverOk: true, subsys: hosts(both...),
				level: pb.SpLevel_SP_LEVEL_NO_THINPOOL})
			assertAna(t, node, testNqn, "1", "optimized over the park")
			return reqOpts{revision: 4, primary: true,
					subsys: hosts(keptHost)}, testNs,
				reloadOnto(srv, node, nsDevName(srv, testNs), testTd)
		},
		quiet: true,
	}, {
		name: "the level lowered with a td change",
		build: func(t *testing.T, srv *CnAgentServer, node *fakeNode) (
			reqOpts, uint64, func() string) {
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				tds: twoTds(), subsys: hosts(both...)})
			syncupCut(t, srv, reqOpts{revision: 3, primary: true,
				leftoverOk: true, tds: twoTds(), subsys: hosts(both...),
				level: pb.SpLevel_SP_LEVEL_NO_THINPOOL})
			assertAna(t, node, testNqn, "1", "optimized over the park")
			return reqOpts{revision: 4, primary: true, tds: twoTds(),
					subsys: onTd(testSnapTd, keptHost)}, testNs,
				reloadOnto(srv, node, nsDevName(srv, testNs), testSnapTd)
		},
		quiet: true,
	}, {
		name: "a td change of a serving primary",
		build: func(t *testing.T, srv *CnAgentServer, node *fakeNode) (
			reqOpts, uint64, func() string) {
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				tds: twoTds(), subsys: hosts(both...)})
			assertAna(t, node, testNqn, "1", "the serving namespace")
			return reqOpts{revision: 3, primary: true, tds: twoTds(),
					subsys: onTd(testSnapTd, keptHost)}, testNs,
				reloadOnto(srv, node, nsDevName(srv, testNs), testSnapTd)
		},
		quiet: true,
	}, {
		name: "a promotion after a failed demotion with a td change",
		build: func(t *testing.T, srv *CnAgentServer, node *fakeNode) (
			reqOpts, uint64, func() string) {
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				tds: twoTds(), subsys: hosts(both...)})
			refusedDown(t, srv, node, testNqn, reqOpts{revision: 3,
				tds: twoTds(), subsys: hosts(both...)})
			return reqOpts{revision: 4, primary: true,
					tds: []*pb.ThinDevice{{TdId: testSnapTd, DevId: 2,
						Size: testTdSize}},
					subsys: onTd(testSnapTd, keptHost)}, testNs,
				reloadOnto(srv, node, nsDevName(srv, testNs), testSnapTd)
		},
	}, {
		name: "a read of the dm-error that does not answer",
		build: func(t *testing.T, srv *CnAgentServer, node *fakeNode) (
			reqOpts, uint64, func() string) {
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				subsys: hosts(both...)})
			refusedDown(t, srv, node, testNqn,
				reqOpts{revision: 3, subsys: hosts(both...)})
			return reqOpts{revision: 4, primary: true, leftoverOk: true,
					subsys: hosts(keptHost)}, testNs,
				reloadOnto(srv, node, nsDevName(srv, testNs), testTd)
		},
		arm: func(srv *CnAgentServer, node *fakeNode) {
			node.mu.Lock()
			node.killCmdNth["-o name,major,minor,attr "+
				errorName(srv, testTd)] = 1
			node.mu.Unlock()
		},
	}, {
		name: "a reused index",
		build: func(t *testing.T, srv *CnAgentServer, node *fakeNode) (
			reqOpts, uint64, func() string) {
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				subsys: hosts(both...)})
			const newNs = testNs + 0x100
			sub := hosts(keptHost)
			sub[testNqn].NsList[0].NsId = newNs
			sub[testNqn].NsList[0].DevUuid =
				"22222222-2222-4222-8222-222222222222"
			sub[testNqn].NsList[0].DevNguid =
				"22222222222242228222222222222222"
			return reqOpts{revision: 3, primary: true, subsys: sub}, newNs,
				func() string {
					return "cmd dmsetup create " + nsDevName(srv, newNs)
				}
		},
	}, {
		name: "a park in the pass of a serving primary",
		build: func(t *testing.T, srv *CnAgentServer, node *fakeNode) (
			reqOpts, uint64, func() string) {
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				clones: []*pb.Clone{cloneOf()}, subsys: hosts(both...)})
			assertAna(t, node, testNqn, "1", "the serving namespace")
			return reqOpts{revision: 3, primary: true,
					subsys: hosts(keptHost)}, testNs,
				reloadOnto(srv, node, nsDevName(srv, testNs), testTd)
		},
	}, {
		name: "a transfer promoted after a failed demotion",
		xfer: true,
		build: func(t *testing.T, srv *CnAgentServer, node *fakeNode) (
			reqOpts, uint64, func() string) {
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				xfers: xfers(strayHost)})
			refusedDown(t, srv, node,
				srv.nf.XferNqn(testCluster, testSp, testXfer),
				reqOpts{revision: 3, xfers: xfers(strayHost)})
			return reqOpts{revision: 4, primary: true, xfers: xfers()},
				testXfer,
				reloadOnto(srv, node, xferName(srv, testXfer), testTd)
		},
		quiet: true,
	}, {
		name: "a transfer whose device read does not answer",
		xfer: true,
		build: func(t *testing.T, srv *CnAgentServer, node *fakeNode) (
			reqOpts, uint64, func() string) {
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
				xfers: xfers(strayHost)})
			refusedDown(t, srv, node,
				srv.nf.XferNqn(testCluster, testSp, testXfer),
				reqOpts{revision: 3, xfers: xfers(strayHost)})
			// The read that does not answer fails the device step, which
			// leaves the device on its error table, and the transfer
			// inaccessible even where its subsystem is safe, as before the
			// cut (CN17).
			return reqOpts{revision: 4, primary: true, leftoverOk: true,
					xfers: xfers()}, testXfer,
				func() string {
					return "dmsetup info --columns --noheadings -o attr " +
						xferName(srv, testXfer)
				}
		},
		arm: func(srv *CnAgentServer, node *fakeNode) {
			node.mu.Lock()
			node.killCmdNth["dmsetup table "+xferName(srv, testXfer)] = 1
			node.mu.Unlock()
		},
		heldAna: "3",
	}}
}

// TestNoNamespaceServesWhileItsSubsystemIsUnsafe: every shape in which a
// review of the cut found a namespace that served no data at a pass's start
// reaching a removed host still connected — a promotion or an unpark over a
// namespace a failed move down left optimized over the park, a level
// lowered, a td change, a read that does not answer, a reused index, a park
// in the pass — ends the pass with the subsystem's namespace inaccessible
// while the stray connection never goes, moved there before the device
// step that puts data under it, with its row OK on both channels; with the
// cut held, the subsystem is safe, and the pass ends optimized, writing no
// ANA group at all where the namespace was optimized already, as before the
// cut (CN33).
func TestNoNamespaceServesWhileItsSubsystemIsUnsafe(t *testing.T) {
	for _, sc := range unsafeScenarios() {
		for _, held := range []bool{false, true} {
			name := sc.name + "/the cut not held"
			if held {
				name = sc.name + "/the cut held"
			}
			t.Run(name, func(t *testing.T) {
				srv, node := newCutServer(t)
				nqn := testNqn
				if sc.xfer {
					nqn = srv.nf.XferNqn(testCluster, testSp, testXfer)
				}
				last, nsId, serve := sc.build(t, srv, node)
				stray := node.connectNvmet(nqn, strayHost)
				if !held {
					node.connEnds[connDir(nqn, stray)] = connEndsNever
				}
				if sc.arm != nil {
					sc.arm(srv, node)
				}

				node.Reset()
				last.leftoverOk = true
				_, reply := syncupCut(t, srv, last)
				ana := "writedirect " + anaPath(nqn, 1) + "="
				nsRow := func(info *pb.CntlrInfo) *pb.ResInfo {
					if sc.xfer {
						return info.GetXferIdToNamespace()[testXfer]
					}
					return info.GetNsIdToNamespace()[nsId]
				}
				if held {
					want := sc.heldAna
					if want == "" {
						want = "1"
					}
					assertAna(t, node, nqn, want,
						"the safe subsystem's namespace")
					if sc.quiet {
						assertNoCall(t, node, ana)
					}
					return
				}
				assertCallBefore(t, node, ana+"3", serve())
				assertNoCall(t, node, ana+"1")
				assertAna(t, node, nqn, "3", "the unsafe subsystem's namespace")
				assertOk(t, nsRow(reply.GetCntlrInfo()),
					"the converge's namespace")
				assertOk(t, nsRow(checkRound(t, srv, last.revision)),
					"the read's namespace")
			})
		}
	}
}

// TestASafeSubsystemPassWritesAsInNoCutMode: where its subsystem is safe, a
// pass in cut mode writes to configfs and device-mapper exactly what the
// same pass writes in no cut mode, in the same order, the cut's own writes
// to the debug directory apart, and the disconnects the sweep sets going
// off the pass, whose timing is the scheduler's (CN10): a promotion, a
// level lowered, a reused index and a park in the pass, each with a removed
// host whose connection the cut ends (CN33).
func TestASafeSubsystemPassWritesAsInNoCutMode(t *testing.T) {
	for _, sc := range unsafeScenarios() {
		switch sc.name {
		case "a promotion after a failed demotion",
			"the level that removes the thin pools lowered", "a reused index",
			"a park in the pass of a serving primary",
			"a transfer promoted after a failed demotion":
		default:
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			writes := map[bool][]string{}
			for _, cut := range []bool{false, true} {
				srv, node := newTestServer(t)
				nqn := testNqn
				if sc.xfer {
					nqn = srv.nf.XferNqn(testCluster, testSp, testXfer)
				}
				if cut {
					node.withNvmetDebug()
					srv.strayCut = true
				}
				last, _, _ := sc.build(t, srv, node)
				if cut {
					node.connectNvmet(nqn, strayHost)
				}
				node.Reset()
				syncupCut(t, srv, last)
				for _, call := range node.Mutations() {
					if !strings.Contains(call, agent.NvmetDebugRoot) &&
						!strings.HasPrefix(call, "cmd nvme disconnect ") {
						writes[cut] = append(writes[cut], call)
					}
				}
				if cut && !node.hasCall(agent.NvmetDebugRoot+"/"+nqn) {
					t.Fatalf("the case is vacuous: no cut ran")
				}
			}
			if !slices.Equal(writes[true], writes[false]) {
				t.Fatalf("cut mode wrote\n%s\nno cut mode wrote\n%s",
					strings.Join(writes[true], "\n"),
					strings.Join(writes[false], "\n"))
			}
		})
	}
}

// TestAFailedMoveDownOfAnUnsafeSubsystemIsNamed: a move to inaccessible of
// an unsafe subsystem's namespace that fails is logged and leaves the
// namespace in its group, as a failed move of CN9's pre-step 1 does: its
// row, which wants the inaccessible group, reads OK with RES_REASON_DIFFERS
// on both channels; the verdicts name the pending subsystem it rests on
// (cntlrRowVerdict), so the worker sends the syncup again, whose pass moves
// it once the write goes through (CN33). Its device, which already maps
// what the rules select, is left to its step: its row reads OK on both
// channels and neither verdict names it, for an ns-dev and a transfer's
// device; a transfer device left suspended on the linear over its origin is
// resumed, with no reload (CN17, CN33).
func TestAFailedMoveDownOfAnUnsafeSubsystemIsNamed(t *testing.T) {
	for _, target := range cutTargets {
		for _, suspended := range []bool{false, true} {
			if suspended && !target.isXfer() {
				continue
			}
			name := target.name
			if suspended {
				name += "/a device left suspended"
			}
			t.Run(name, func(t *testing.T) {
				srv, node := newCutServer(t)
				nqn := target.nqn(srv)
				dev := nsDevName(srv, testNs)
				devRow := func(info *pb.CntlrInfo) *pb.ResInfo {
					return info.GetNsIdToDmLinear()[testNs]
				}
				if target.isXfer() {
					dev = xferName(srv, testXfer)
					devRow = func(info *pb.CntlrInfo) *pb.ResInfo {
						return info.GetXferIdToDmLinear()[testXfer]
					}
				}
				syncupBoth(t, srv, target.opts(2, true, strayHost, keptHost))
				stray := node.connectNvmet(nqn, strayHost)
				node.connEnds[connDir(nqn, stray)] = connEndsNever
				node.failWriteAlways[anaPath(nqn, 1)+"=3"] = "refused"
				if suspended {
					node.mu.Lock()
					node.dms[dev].suspended = true
					node.mu.Unlock()
				}

				node.Reset()
				held, reply := syncupCut(t, srv,
					target.opts(3, true, keptHost))
				if !node.hasCall("writedirect " + anaPath(nqn, 1) + "=3") {
					t.Fatalf("the case is vacuous: no move down was tried")
				}
				assertAna(t, node, nqn, "1", "the namespace whose move failed")
				assertNoCall(t, node, "cmd dmsetup reload "+dev+" ")
				if suspended {
					if !node.hasCall("cmd dmsetup resume " + dev) {
						t.Fatalf("the suspended device was not resumed")
					}
					node.mu.Lock()
					stays := node.dms[dev].suspended
					node.mu.Unlock()
					if stays {
						t.Fatalf("the device is still suspended")
					}
				}
				read := checkRound(t, srv, 3)
				for label, info := range map[string]*pb.CntlrInfo{
					"the converge": reply.GetCntlrInfo(), "a read": read,
				} {
					row := target.ns(info)
					assertRowStatus(t, row, pb.ResStatus_RES_STATUS_OK,
						label+"'s namespace")
					if row.GetReason() != pb.ResReason_RES_REASON_DIFFERS {
						t.Fatalf("%s: reason %v, want RES_REASON_DIFFERS",
							label, row.GetReason())
					}
					assertOk(t, devRow(info), label+"'s device")
				}
				assertCodeNames(t, "the converge", reply,
					pendingName(nqn, strayHost))
				code := readCode(t, srv, 3)
				if !strings.Contains(code.GetDetails(),
					pendingName(nqn, strayHost)) {
					t.Fatalf("the read's code %d (%s), want the pending "+
						"subsystem named", code.GetCode(), code.GetDetails())
				}
				for label, details := range map[string]string{
					"the converge": reply.GetAgentReply().GetDetails(),
					"the read":     code.GetDetails(),
				} {
					if strings.Contains(details, dev+": ") {
						t.Fatalf("%s names the device: %s", label, details)
					}
				}
				delete(node.failWriteAlways, anaPath(nqn, 1)+"=3")
				if _, resent := resendRound(t, srv, held, "again"); resent ==
					nil {
					t.Fatalf("the round brought no re-send")
				}
				assertAna(t, node, nqn, "3", "the namespace after the re-send")
			})
		}
	}
}

// TestAnUnsafeSubsystemDarkensANamespaceTheSweepLeft: every namespace the
// kernel lists under an unsafe subsystem moves to inaccessible, not only
// the planned ones: a namespace the request takes out of ns_list, which a
// sweep that an unanswered listing stopped leaves in place, optimized, is
// moved too (CN33, CN21).
func TestAnUnsafeSubsystemDarkensANamespaceTheSweepLeft(t *testing.T) {
	srv, node := newCutServer(t)
	twoTds := append(defaultTds(), &pb.ThinDevice{TdId: testSnapTd,
		DevId: 2, Size: testTdSize})
	two := subsysWithHosts(strayHost, keptHost)
	two[testNqn].NsList = append(two[testNqn].NsList, &pb.Namespace{
		NsId: testNs + 1, NsIdx: 2, TdId: testSnapTd,
		DevUuid:  "33333333-3333-4333-8333-333333333333",
		DevNguid: "33333333333343338333333333333333"})
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: twoTds,
		subsys: two})
	assertAna(t, node, testNqn, "1", "the first namespace")
	if got := strings.TrimSpace(node.fileOf(anaPath(testNqn, 2))); got != "1" {
		t.Fatalf("the second namespace reads %q, want 1", got)
	}
	stray := node.connectNvmet(testNqn, strayHost)
	node.connEnds[connDir(testNqn, stray)] = connEndsNever
	// The sweep's first listing, of every subsystem, does not answer, so it
	// stops before it removes the second namespace.
	node.failCmd["list "+agent.NvmetRoot+"/subsystems"] = "input/output error"

	node.Reset()
	syncupCut(t, srv, reqOpts{revision: 3, primary: true, leftoverOk: true,
		tds: twoTds, subsys: subsysWithHosts(keptHost)})
	node.mu.Lock()
	left := node.dirs[agent.NvmetRoot+"/subsystems/"+testNqn+"/namespaces/2"]
	node.mu.Unlock()
	if !left {
		t.Fatalf("the case is vacuous: the sweep removed the namespace")
	}
	if got := strings.TrimSpace(node.fileOf(anaPath(testNqn, 2))); got != "3" {
		t.Fatalf("the namespace the sweep left reads %q, want 3", got)
	}
	assertAna(t, node, testNqn, "3", "the planned namespace")
}

// TestAFailedMoveDownPutsNoDataUnderTheNamespace: where the move of an
// unsafe subsystem's namespace to inaccessible fails in the pass, the device
// step puts no data under that namespace, which may still be optimized: a
// promotion over a namespace a failed demotion left optimized, the level
// that removes the thin pools lowered, the same with a td change, and a
// transfer promoted after a failed demotion each leave the device on the
// park, with no reload onto the raid0, and a td change of a serving
// primary leaves it on the old volume's raid0, with no move onto the new
// volume's data; its row reads ERROR "namespace
// ... not moved to inaccessible" on both channels, both verdicts naming it,
// which brings the worker's re-send of the move (dnv-worker.md RW4; CN16,
// CN17, CN33). A move whose
// group does not read back, its read-back failing, is fenced the same way
// on the converge; the channels differ there alone: a later read finds the
// namespace inaccessible and gives the device's own row.
func TestAFailedMoveDownPutsNoDataUnderTheNamespace(t *testing.T) {
	for _, sc := range unsafeScenarios() {
		switch sc.name {
		case "a promotion after a failed demotion",
			"the level that removes the thin pools lowered",
			"the level lowered with a td change",
			"a td change of a serving primary",
			"a transfer promoted after a failed demotion":
		default:
			continue
		}
		for _, readBack := range []bool{false, true} {
			name := sc.name
			if readBack {
				name += "/the group not read back"
			}
			t.Run(name, func(t *testing.T) {
				srv, node := newCutServer(t)
				nqn := testNqn
				dev := nsDevName(srv, testNs)
				row := func(info *pb.CntlrInfo) *pb.ResInfo {
					return info.GetNsIdToDmLinear()[testNs]
				}
				if sc.xfer {
					nqn = srv.nf.XferNqn(testCluster, testSp, testXfer)
					dev = xferName(srv, testXfer)
					row = func(info *pb.CntlrInfo) *pb.ResInfo {
						return info.GetXferIdToDmLinear()[testXfer]
					}
				}
				last, _, serve := sc.build(t, srv, node)
				stray := node.connectNvmet(nqn, strayHost)
				node.connEnds[connDir(nqn, stray)] = connEndsNever
				ana := anaPath(nqn, 1)
				if readBack {
					// The sweep lists the namespaces first, the move second;
					// after the move's listing, its own read of the group
					// answers, its write goes through, and the read back
					// after it fails.
					nsDir := agent.NvmetRoot + "/subsystems/" + nqn +
						"/namespaces"
					node.mu.Lock()
					node.afterList[nsDir] = func() {
						node.afterList[nsDir] = func() {
							node.afterRead[ana] = func() {
								node.failRead[ana] = true
							}
						}
					}
					node.mu.Unlock()
				} else {
					node.failWriteAlways[ana+"=3"] = "refused"
				}
				node.mu.Lock()
				parked := node.dms[dev].table
				node.mu.Unlock()

				node.Reset()
				last.leftoverOk = true
				_, reply := syncupCut(t, srv, last)
				if !node.hasCall("writedirect " + anaPath(nqn, 1) + "=3") {
					t.Fatalf("the case is vacuous: no move down was tried")
				}
				assertNoCall(t, node, serve())
				node.mu.Lock()
				table := node.dms[dev].table
				node.mu.Unlock()
				if table != parked {
					t.Fatalf("the device maps %q, want the park %q it had",
						table, parked)
				}
				want := notDarkDetails(nqn, 1)
				if readBack {
					// The move went through, so a read finds the namespace
					// inaccessible; only the converge fenced its device.
					node.mu.Lock()
					fired := len(node.failRead) == 0
					node.mu.Unlock()
					if !fired {
						t.Fatalf("the case is vacuous: no read back failed")
					}
					r := row(reply.GetCntlrInfo())
					assertRowStatus(t, r, pb.ResStatus_RES_STATUS_ERROR,
						"the converge")
					if r.GetDetails() != want {
						t.Fatalf("the converge: details %q, want %q",
							r.GetDetails(), want)
					}
					return
				}
				for label, r := range map[string]*pb.ResInfo{
					"the converge": row(reply.GetCntlrInfo()),
					"a read":       row(checkRound(t, srv, last.revision)),
				} {
					assertRowStatus(t, r, pb.ResStatus_RES_STATUS_ERROR, label)
					if r.GetDetails() != want {
						t.Fatalf("%s: details %q, want %q", label,
							r.GetDetails(), want)
					}
				}
				assertCodeNames(t, "the converge", reply, dev+": differs: "+want)
				if code := readCode(t, srv, last.revision); !strings.Contains(
					code.GetDetails(), dev+": differs: "+want) {
					t.Fatalf("the read's code %d (%s), want the device named",
						code.GetCode(), code.GetDetails())
				}
			})
		}
	}
}

// TestADarkenWhoseListingFailsMovesThePlannedNamespaces: where the listing
// of an unsafe subsystem's namespaces does not answer, the move to
// inaccessible still takes the planned ones, before the device step, for
// the pool's subsystem and a transfer's (CN33).
func TestADarkenWhoseListingFailsMovesThePlannedNamespaces(t *testing.T) {
	for _, target := range cutTargets {
		t.Run(target.name, func(t *testing.T) {
			srv, node := newCutServer(t)
			nqn := target.nqn(srv)
			syncupBoth(t, srv, target.opts(2, true, strayHost, keptHost))
			stray := node.connectNvmet(nqn, strayHost)
			node.connEnds[connDir(nqn, stray)] = connEndsNever
			// The sweep lists the namespaces first; the second listing is
			// the move's own.
			key := "list " + agent.NvmetRoot + "/subsystems/" + nqn +
				"/namespaces"
			node.mu.Lock()
			node.killCmdNth[key] = 2
			node.mu.Unlock()

			node.Reset()
			_, reply := syncupCut(t, srv, target.opts(3, true, keptHost))
			node.mu.Lock()
			_, left := node.killCmdNth[key]
			node.mu.Unlock()
			if left {
				t.Fatalf("the case is vacuous: the move's listing answered")
			}
			ana := "writedirect " + anaPath(nqn, 1) + "="
			assertCallBefore(t, node, ana+"3", devStepCall(srv, target))
			assertNoCall(t, node, ana+"1")
			assertAna(t, node, nqn, "3", "the planned namespace")
			assertOk(t, target.ns(reply.GetCntlrInfo()),
				"the converge's namespace")
			assertOk(t, target.ns(checkRound(t, srv, 3)),
				"the read's namespace")
		})
	}
}

// TestAReadListsTheConnectionsOfASafeListThatDiffers: a read whose
// subsystem's configfs differs from the desired one, while the kernel's
// list admits no host off it (a failed add of a host), still lists the
// subsystem's nvmet connections in cut mode, as a converge's cut runs then:
// a stray still listed, or no debug directory, makes the subsystem not
// safe, and its namespace row wants the inaccessible group, reading OK at
// 3, for the pool's subsystem and a transfer's (CN33).
func TestAReadListsTheConnectionsOfASafeListThatDiffers(t *testing.T) {
	const newHost = "nqn.2024-01.io.dnv-it:host:new"
	for _, target := range cutTargets {
		for _, noDir := range []bool{false, true} {
			name := target.name + "/a stray still listed"
			if noDir {
				name = target.name + "/no debug directory"
			}
			t.Run(name, func(t *testing.T) {
				srv, node := newCutServer(t)
				nqn := target.nqn(srv)
				syncupBoth(t, srv, target.opts(2, true, strayHost, keptHost))
				stray := node.connectNvmet(nqn, strayHost)
				node.connEnds[connDir(nqn, stray)] = connEndsNever
				syncupCut(t, srv, target.opts(3, true, keptHost))
				assertAna(t, node, nqn, "3", "the namespace")
				node.failCmdAlways["ln -s "+agent.NvmetRoot+"/hosts/"+
					newHost] = "ln: Invalid argument"
				_, reply := syncupCut(t, srv,
					target.opts(4, true, keptHost, newHost))
				assertRowStatus(t, target.row(reply.GetCntlrInfo()),
					pb.ResStatus_RES_STATUS_ERROR, "the converge's subsystem")
				assertAna(t, node, nqn, "3", "the namespace")
				if noDir {
					node.mu.Lock()
					delete(node.dirs, agent.NvmetDebugRoot+"/"+nqn)
					node.mu.Unlock()
				}

				node.Reset()
				read := checkRound(t, srv, 4)
				if got := node.callsMatching("list " + agent.NvmetDebugRoot +
					"/" + nqn); len(got) != 1 {
					t.Fatalf("the read listed the debug directory %d times, "+
						"want once", len(got))
				}
				assertOk(t, target.ns(read), "the read's namespace")
			})
		}
	}
}

// TestAFailedMoveDownLetsTheReadOnlyLevelThrough: where an unsafe
// subsystem's move to inaccessible fails, the device step still puts the
// read-only level's error-writes table over the device the ns-dev maps,
// which moves no data under the namespace and takes writes away (CN16 rule
// 7, CN33): the reload onto that table over the same raid0 runs, no other,
// and the ns-dev's row reads alike on both channels.
func TestAFailedMoveDownLetsTheReadOnlyLevelThrough(t *testing.T) {
	srv, node := newCutServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		subsys: subsysWithHosts(strayHost, keptHost)})
	stray := node.connectNvmet(testNqn, strayHost)
	node.connEnds[connDir(testNqn, stray)] = connEndsNever
	node.failWriteAlways[anaPath(testNqn, 1)+"=3"] = "refused"
	raid0No := devNoOf(node, raid0Name(srv, testTd))
	dev := nsDevName(srv, testNs)

	node.Reset()
	_, reply := syncupCut(t, srv, reqOpts{revision: 3, primary: true,
		leftoverOk: true, subsys: subsysWithHosts(keptHost),
		level: pb.SpLevel_SP_LEVEL_READONLY})
	if !node.hasCall("writedirect " + anaPath(testNqn, 1) + "=3") {
		t.Fatalf("the case is vacuous: no move down was tried")
	}
	reloads := node.callsMatching("cmd dmsetup reload " + dev + " ")
	if len(reloads) != 1 || !strings.Contains(reloads[0], " flakey ") ||
		!strings.Contains(reloads[0], " "+raid0No+" ") {
		t.Fatalf("reloads of the ns-dev %v, want one onto the error-writes "+
			"table over the raid0 %s", reloads, raid0No)
	}
	converged := reply.GetCntlrInfo().GetNsIdToDmLinear()[testNs]
	read := checkRound(t, srv, 3).GetNsIdToDmLinear()[testNs]
	assertOk(t, converged, "the converge's ns-dev")
	if read.GetStatus() != converged.GetStatus() ||
		read.GetDetails() != converged.GetDetails() {
		t.Fatalf("the read's ns-dev %v/%q, the converge's %v/%q: want the "+
			"same", read.GetStatus(), read.GetDetails(),
			converged.GetStatus(), converged.GetDetails())
	}
}

// TestAFailedMoveDownStillParks: the fence of a namespace whose move to
// inaccessible failed lets a park through: a serving primary whose dm-clone
// stops serving in the pass, its hydration off and the enable refused, has
// its ns-dev reloaded onto the td's dm-error (CN16 rule 5), with no "not
// moved" row on either channel (CN33).
func TestAFailedMoveDownStillParks(t *testing.T) {
	srv, node := newCutServer(t)
	clones := []*pb.Clone{cloneOf()}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, clones: clones,
		subsys: subsysWithHosts(strayHost, keptHost)})
	assertAna(t, node, testNqn, "1", "the serving namespace")
	stray := node.connectNvmet(testNqn, strayHost)
	node.connEnds[connDir(testNqn, stray)] = connEndsNever
	node.failWriteAlways[anaPath(testNqn, 1)+"=3"] = "refused"
	clone := cloneName(srv, testClone)
	node.mu.Lock()
	node.dms[clone].noHydration = true
	node.mu.Unlock()
	node.failCmdAlways["dmsetup message "+clone+" 0 enable_hydration"] =
		"device-mapper: message ioctl failed"
	errNo := devNoOf(node, errorName(srv, testTd))

	node.Reset()
	_, reply := syncupCut(t, srv, reqOpts{revision: 3, primary: true,
		leftoverOk: true, clones: clones,
		subsys: subsysWithHosts(keptHost)})
	if !node.hasCall("writedirect " + anaPath(testNqn, 1) + "=3") {
		t.Fatalf("the case is vacuous: no move down was tried")
	}
	if !node.hasCall("cmd dmsetup reload " + nsDevName(srv, testNs) +
		" --table " + nsDevTableOn(errNo)) {
		t.Fatalf("the ns-dev was not parked on the td's dm-error")
	}
	want := notDarkDetails(testNqn, 1)
	for label, row := range map[string]*pb.ResInfo{
		"the converge": reply.GetCntlrInfo().GetNsIdToDmLinear()[testNs],
		"a read":       checkRound(t, srv, 3).GetNsIdToDmLinear()[testNs],
	} {
		if row.GetDetails() == want {
			t.Fatalf("%s: the parked ns-dev reads %q", label, want)
		}
	}
}

// TestAFailedMoveDownMakesNoNsDev: the fence of a namespace whose move to
// inaccessible failed makes no ns-dev that is not there over its serving
// device: the converge creates nothing, and both channels read the ns-dev's
// row ERROR "namespace ... not moved to inaccessible" (CN33).
func TestAFailedMoveDownMakesNoNsDev(t *testing.T) {
	srv, node := newCutServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		subsys: subsysWithHosts(strayHost, keptHost)})
	stray := node.connectNvmet(testNqn, strayHost)
	node.connEnds[connDir(testNqn, stray)] = connEndsNever
	node.failWriteAlways[anaPath(testNqn, 1)+"=3"] = "refused"
	dev := nsDevName(srv, testNs)
	node.mu.Lock()
	delete(node.dms, dev)
	node.mu.Unlock()

	node.Reset()
	_, reply := syncupCut(t, srv, reqOpts{revision: 3, primary: true,
		leftoverOk: true, subsys: subsysWithHosts(keptHost)})
	if !node.hasCall("writedirect " + anaPath(testNqn, 1) + "=3") {
		t.Fatalf("the case is vacuous: no move down was tried")
	}
	assertNoCall(t, node, "cmd dmsetup create "+dev)
	want := notDarkDetails(testNqn, 1)
	for label, row := range map[string]*pb.ResInfo{
		"the converge": reply.GetCntlrInfo().GetNsIdToDmLinear()[testNs],
		"a read":       checkRound(t, srv, 3).GetNsIdToDmLinear()[testNs],
	} {
		assertRowStatus(t, row, pb.ResStatus_RES_STATUS_ERROR, label)
		if row.GetDetails() != want {
			t.Fatalf("%s: details %q, want %q", label, row.GetDetails(), want)
		}
	}
}

// TestAFailedMoveDownReadsTheReadOnlyWrapAsNotDesired: a read of an ns-dev
// that the read-only level's error-writes table should wrap, under an
// unsafe subsystem whose namespace is not inaccessible, reads it as a table
// a converge would change, not as one the fence keeps (CN33): when the
// converge's reload of that table is refused, the read's row says the
// table is not the desired one.
func TestAFailedMoveDownReadsTheReadOnlyWrapAsNotDesired(t *testing.T) {
	srv, node := newCutServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		subsys: subsysWithHosts(strayHost, keptHost)})
	stray := node.connectNvmet(testNqn, strayHost)
	node.connEnds[connDir(testNqn, stray)] = connEndsNever
	node.failWriteAlways[anaPath(testNqn, 1)+"=3"] = "refused"
	dev := nsDevName(srv, testNs)
	node.failCmdAlways["dmsetup reload "+dev+" "] =
		"device-mapper: reload ioctl failed"

	node.Reset()
	syncupCut(t, srv, reqOpts{revision: 3, primary: true, leftoverOk: true,
		subsys: subsysWithHosts(keptHost),
		level:  pb.SpLevel_SP_LEVEL_READONLY})
	if !node.hasCall("cmd dmsetup reload " + dev + " ") {
		t.Fatalf("the case is vacuous: no reload was tried")
	}
	read := checkRound(t, srv, 3).GetNsIdToDmLinear()[testNs]
	assertRowStatus(t, read, pb.ResStatus_RES_STATUS_ERROR, "the read")
	if read.GetDetails() != detailsNsDevNotDesired {
		t.Fatalf("the read's details %q, want %q", read.GetDetails(),
			detailsNsDevNotDesired)
	}
}

// TestAFailedMoveDownReadsAParkAsTheRulesSay: a read keeps the fence's
// park exception: where the rules select the td's dm-error and the park's
// reload fails, under an unsafe subsystem whose namespace's move to
// inaccessible failed, the read reads the ns-dev's table as not the
// desired one, as the converge's ns-dev step would park it, not as one the
// fence keeps (CN33).
func TestAFailedMoveDownReadsAParkAsTheRulesSay(t *testing.T) {
	srv, node := newCutServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		subsys: subsysWithHosts(strayHost, keptHost)})
	stray := node.connectNvmet(testNqn, strayHost)
	node.connEnds[connDir(testNqn, stray)] = connEndsNever
	node.failWriteAlways[anaPath(testNqn, 1)+"=3"] = "refused"
	dev := nsDevName(srv, testNs)
	node.failCmdAlways["dmsetup reload "+dev+" "] =
		"device-mapper: reload ioctl failed"
	susp := defaultSubsys(true)
	susp[testNqn].AllowedHosts = []string{keptHost}

	node.Reset()
	syncupCut(t, srv, reqOpts{revision: 3, primary: true, leftoverOk: true,
		subsys: susp})
	if !node.hasCall("cmd dmsetup reload " + dev + " ") {
		t.Fatalf("the case is vacuous: no park was tried")
	}
	assertAna(t, node, testNqn, "1", "the namespace whose move failed")
	read := checkRound(t, srv, 3).GetNsIdToDmLinear()[testNs]
	assertRowStatus(t, read, pb.ResStatus_RES_STATUS_ERROR, "the read")
	if read.GetDetails() != detailsNsDevNotDesired {
		t.Fatalf("the read's details %q, want %q", read.GetDetails(),
			detailsNsDevNotDesired)
	}
}

// TestAFailedMoveDownLetsNoGrowThrough: the read-only level's wrap passes
// the fence only over the device the ns-dev maps in full, at its own
// length: a request that raises the level and grows the volume, under an
// unsafe subsystem whose namespace's move to inaccessible fails, gets no
// reload of the ns-dev, which would expose new blocks, and its row reads
// ERROR "namespace ... not moved to inaccessible" (CN33).
func TestAFailedMoveDownLetsNoGrowThrough(t *testing.T) {
	srv, node := newCutServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true,
		subsys: subsysWithHosts(strayHost, keptHost)})
	stray := node.connectNvmet(testNqn, strayHost)
	node.connEnds[connDir(testNqn, stray)] = connEndsNever
	node.failWriteAlways[anaPath(testNqn, 1)+"=3"] = "refused"
	dev := nsDevName(srv, testNs)

	node.Reset()
	_, reply := syncupCut(t, srv, reqOpts{revision: 3, primary: true,
		leftoverOk: true, subsys: subsysWithHosts(keptHost),
		tds:   []*pb.ThinDevice{{TdId: testTd, DevId: 1, Size: 2 * testTdSize}},
		level: pb.SpLevel_SP_LEVEL_READONLY})
	if !node.hasCall("writedirect " + anaPath(testNqn, 1) + "=3") {
		t.Fatalf("the case is vacuous: no move down was tried")
	}
	assertNoCall(t, node, "cmd dmsetup reload "+dev+" ")
	want := notDarkDetails(testNqn, 1)
	row := reply.GetCntlrInfo().GetNsIdToDmLinear()[testNs]
	assertRowStatus(t, row, pb.ResStatus_RES_STATUS_ERROR, "the converge")
	if row.GetDetails() != want {
		t.Fatalf("the converge's details %q, want %q", row.GetDetails(), want)
	}
}

// TestAFailedMoveDownNamesAHeldNsDevAlike: under CN18's hold, a lingering
// dm-clone still writing into the raid0, the ns-dev of a namespace whose
// move to inaccessible failed is held off the raid0, and its row reads
// ERROR "namespace ... not moved to inaccessible" on the converge, as a
// read, which knows no hold, reads it (CN18, CN33).
func TestAFailedMoveDownNamesAHeldNsDevAlike(t *testing.T) {
	srv, node := newCutServer(t)
	clones := []*pb.Clone{cloneOf()}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, clones: clones,
		subsys: subsysWithHosts(strayHost, keptHost)})
	stray := node.connectNvmet(testNqn, strayHost)
	node.connEnds[connDir(testNqn, stray)] = connEndsNever
	node.failWriteAlways[anaPath(testNqn, 1)+"=3"] = "refused"
	clone := cloneName(srv, testClone)
	node.failCmdAlways["dmsetup remove "+clone] =
		"device-mapper: remove ioctl failed: Device or resource busy"

	node.Reset()
	_, reply := syncupCut(t, srv, reqOpts{revision: 3, primary: true,
		leftoverOk: true, subsys: subsysWithHosts(keptHost)})
	if !node.hasCall("cmd dmsetup remove " + clone) {
		t.Fatalf("the case is vacuous: no remove of the dm-clone was tried")
	}
	// The hold took effect: the dm-clone lingers, and the ns-dev the park
	// put on the td's dm-error stays there (CN18's held park), so the row
	// below is the held ns-dev's, not a reload's.
	if node.dms[clone] == nil {
		t.Fatalf("the case is vacuous: the dm-clone %s is gone, so nothing holds", clone)
	}
	parked := node.dms[nsDevName(srv, testNs)].table
	if !strings.Contains(parked, node.devNo["/dev/mapper/"+errorName(srv, testTd)]) {
		t.Fatalf("the ns-dev is not held on the td's dm-error: %q", parked)
	}
	want := notDarkDetails(testNqn, 1)
	for label, row := range map[string]*pb.ResInfo{
		"the converge": reply.GetCntlrInfo().GetNsIdToDmLinear()[testNs],
		"a read":       checkRound(t, srv, 3).GetNsIdToDmLinear()[testNs],
	} {
		assertRowStatus(t, row, pb.ResStatus_RES_STATUS_ERROR, label)
		if row.GetDetails() != want {
			t.Fatalf("%s: details %q, want %q", label, row.GetDetails(), want)
		}
	}
}
