package cnagent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// What a cntlr's verdict names of a clone (cnagent.md CN18, CN30): each entry
// of the source's src_tr_conf_list that CN18 step 1 would connect
// (srcToConnect, cloneSource.toName), a name only a read gives, and the
// dm-clone while CN18 steps 3 to 5 would change it (cloneDmGate,
// cloneDmBuild), a name both channels give. Each is named while a converge
// would act, and not otherwise, and one re-send of the held request, as the
// worker sends it on a round that names it (dnv-worker.md RW4), cures it.

// cloneOpts is the fixture's primary with one clone over a clone meta group
// without redundancy, at the revision.
func cloneOpts(revision uint64, clones ...*pb.Clone) reqOpts {
	if len(clones) == 0 {
		clones = []*pb.Clone{cloneOf()}
	}
	return reqOpts{revision: revision, primary: true, clones: clones}
}

// srcEndpointName is how a verdict names an entry of the clone source's
// src_tr_conf_list with no controller.
func srcEndpointName(addr, svcId string) string {
	return "enumeration failed: " + noCtrlName(testSrcNqn, addr, svcId)
}

// srcHasCtrl says whether the source subsystem holds a controller, in any
// state, at the endpoint.
func srcHasCtrl(node *fakeNode, addr, svcId string) bool {
	node.mu.Lock()
	defer node.mu.Unlock()
	subsys := node.subsystems[testSrcNqn]
	if subsys == nil {
		return false
	}
	for _, ctrl := range subsys.ctrls {
		if ctrl.trAddr == addr && ctrl.trSvcId == svcId {
			return true
		}
	}
	return false
}

// srcReplug takes the clone source's subsystem away with its controllers and
// head, and connects it again at a new entry number, as when the source
// comes back after its controllers were lost: its namespace head comes back
// under a new device number, which the dm-clone's table does not map.
func srcReplug(t *testing.T, node *fakeNode) {
	t.Helper()
	before, _ := headOf(t, node, testSrcNqn)
	node.mu.Lock()
	dropSubsysLocked(node, testSrcNqn)
	node.plantSubsys(node.nextSubsys, testSrcNqn,
		endpoint{testIp2, testSvcId2})
	node.mu.Unlock()
	if after, _ := headOf(t, node, testSrcNqn); after == before {
		t.Fatalf("the source came back under the same head %s", after)
	}
}

// cloneTableArg sets one positional argument of the dm-clone's live table
// (`0 <sectors> clone <meta> <dest> <src> <region> …`), argument 0 being the
// metadata device.
func cloneTableArg(t *testing.T, node *fakeNode, clone string, arg int,
	value string) {
	t.Helper()
	node.mu.Lock()
	defer node.mu.Unlock()
	dm := node.dms[clone]
	if dm == nil {
		t.Fatalf("no dm-clone %s", clone)
	}
	fields := strings.Fields(dm.table)
	if len(fields) < 7 || fields[2] != "clone" {
		t.Fatalf("%s is not a dm-clone: %q", clone, dm.table)
	}
	fields[3+arg] = value
	dm.table = strings.Join(fields, " ")
}

// ---------------------------------------------------------------------------
// The clone source's entries (CN18 step 1, CN30)
// ---------------------------------------------------------------------------

// TestACloneSourceEndpointIsNamedWhileAConvergeWouldConnectIt: a read names
// the entry of the clone source's src_tr_conf_list that the source's lookup
// holds no controller for, as "<nqn> at <addr>:<svc>: no controller", while
// no disconnect of the source is in flight, the lookup answered and every
// controller's address read answered: a source never connected after its
// controller went, or with its subsystem kept and that controller's device
// gone. The one SyncupCntlr the worker re-sends on that round connects it
// once, and the read after it names nothing. The converge's own reply does
// not name it: a read alone does.
func TestACloneSourceEndpointIsNamedWhileAConvergeWouldConnectIt(
	t *testing.T,
) {
	for _, tc := range []struct {
		name string
		arm  func(t *testing.T, node *fakeNode)
	}{
		{"its controller gone with the subsystem",
			func(t *testing.T, node *fakeNode) {
				dropLegCtrl(t, node, testSrcNqn, testIp2, testSvcId2)
			}},
		{"its controller's device gone, the entry kept",
			func(t *testing.T, node *fakeNode) {
				if node.deleteCtrlDevice(
					testSrcNqn, testIp2, testSvcId2) == "" {
					t.Fatalf("the source has no controller")
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			withPassClock(srv, node)
			opts := cloneOpts(2)
			syncupBoth(t, srv, opts)
			held := cntlrReq(opts)
			assertRowVerdict(t, rowRound(t, srv, 2).GetAgentReply(),
				"the fixture")
			tc.arm(t, node)

			round := rowRound(t, srv, 2)
			if !strings.Contains(round.GetAgentReply().GetDetails(),
				srcEndpointName(testIp2, testSvcId2)) {
				t.Fatalf("the read %v does not name the source's entry",
					round.GetAgentReply())
			}
			node.Reset()
			reply := rowResend(t, srv, held, round)
			if reply == nil {
				t.Fatalf("the round that names the entry brought no " +
					"re-send")
			}
			if got := connectsTo(node, testSrcNqn, testIp2,
				testSvcId2); got != 1 {
				t.Fatalf("the re-send made %d connects of the entry, "+
					"want 1", got)
			}
			if strings.Contains(reply.GetAgentReply().GetDetails(),
				noCtrlName(testSrcNqn, testIp2, testSvcId2)) {
				t.Fatalf("the converge's reply %v names the entry",
					reply.GetAgentReply())
			}
			assertRowVerdict(t, rowRound(t, srv, 2).GetAgentReply(),
				"the read after the re-send")
			assertOk(t, probedCntlrInfo(t, srv).
				GetCloneIdToDmClone()[testClone], "the dm-clone")
		})
	}
}

// TestACloneSourceEndpointIsNotNamedWhereAConvergeWouldNotConnectIt bounds
// the name: an entry with a controller in any state, a source a sweep is
// disconnecting, a lookup that did not answer and a controller whose address
// read did not answer each name no entry, and the converge connects none.
// Under the unanswered address read the converge tries the entry the
// controller serves, which the host refuses as a duplicate.
func TestACloneSourceEndpointIsNotNamedWhereAConvergeWouldNotConnectIt(
	t *testing.T,
) {
	for _, tc := range []struct {
		name string
		arm  func(t *testing.T, srv *CnAgentServer, node *fakeNode)
		// gone: the entry has no controller once armed.
		gone bool
	}{
		{"a controller that is connecting",
			func(t *testing.T, srv *CnAgentServer, node *fakeNode) {
				node.setCtrlState(testSrcNqn, testIp2, testSvcId2,
					"connecting")
			}, false},
		{"a disconnect of the source in flight",
			func(t *testing.T, srv *CnAgentServer, node *fakeNode) {
				dropLegCtrl(t, node, testSrcNqn, testIp2, testSvcId2)
				srv.mu.Lock()
				srv.disconnecting[testSrcNqn] = struct{}{}
				srv.mu.Unlock()
			}, true},
		{"its lookup did not answer",
			func(t *testing.T, srv *CnAgentServer, node *fakeNode) {
				dropLegCtrl(t, node, testSrcNqn, testIp2, testSvcId2)
				node.mu.Lock()
				node.killCmdAlways["list "+sysfsNvmeSubsysDir] = true
				node.mu.Unlock()
			}, true},
		{"its controller's address read did not answer",
			func(t *testing.T, srv *CnAgentServer, node *fakeNode) {
				ctrl := ctrlNameOf(node, testSrcNqn, testIp2, testSvcId2)
				if ctrl == "" {
					t.Fatalf("the source has no controller")
				}
				node.mu.Lock()
				node.killReadAlways[sysfsNvmeCtrlDir+"/"+ctrl+"/address"] =
					true
				node.mu.Unlock()
			}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			withPassClock(srv, node)
			opts := cloneOpts(2)
			syncupBoth(t, srv, opts)
			tc.arm(t, srv, node)

			if details := rowRound(t, srv, 2).GetAgentReply().
				GetDetails(); strings.Contains(details,
				noCtrlName(testSrcNqn, testIp2, testSvcId2)) {
				t.Fatalf("the read %q names the source's entry", details)
			}
			if srcHasCtrl(node, testIp2, testSvcId2) == tc.gone {
				t.Fatalf("the case is vacuous: the entry's controller is "+
					"there: %v", !tc.gone)
			}
			node.Reset()
			if _, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(opts)); err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if srcHasCtrl(node, testIp2, testSvcId2) != !tc.gone {
				t.Fatalf("the converge connected the entry the read " +
					"does not name")
			}
		})
	}
}

// TestTheReadNamesACloneSourceEndpointExactlyWhenAConvergeWouldConnectIt
// holds the read to the converge over a source with two entries, each with
// a live controller, a connecting one or none, under a lookup as built, a
// disconnect in flight, a lookup that did not answer and an address read of
// one entry's controller that did not answer: the read names an entry
// exactly when the converge after it connects it. The one state where they
// part is pinned on its own: an unanswered address read of the second
// entry's controller while the first entry has none, where the read names
// nothing and the converge connects the first entry before the host refuses
// the second as a duplicate (cnagent.md CN30: that controller may be any
// entry's).
func TestTheReadNamesACloneSourceEndpointExactlyWhenAConvergeWouldConnectIt(
	t *testing.T,
) {
	type ep struct{ addr, svcId string }
	eps := []ep{{testIp2, testSvcId2}, {testIp, testSvcId}}
	twoEps := func() *pb.Clone {
		clone := cloneOf()
		clone.SrcTrConfList = []*pb.NvmeTrConf{
			sideTrConf(eps[0].addr, eps[0].svcId),
			sideTrConf(eps[1].addr, eps[1].svcId),
		}
		return clone
	}
	states := []string{"live", "connecting", "none"}
	gates := []struct {
		name string
		// arm runs after the entries' states are set; unknown is the entry
		// whose controller's address read it kills, -1 for none.
		arm     func(t *testing.T, srv *CnAgentServer, node *fakeNode)
		unknown int
	}{
		{"its lookup as built",
			func(*testing.T, *CnAgentServer, *fakeNode) {}, -1},
		{"a disconnect in flight",
			func(t *testing.T, srv *CnAgentServer, node *fakeNode) {
				srv.mu.Lock()
				srv.disconnecting[testSrcNqn] = struct{}{}
				srv.mu.Unlock()
			}, -1},
		{"its lookup did not answer",
			func(t *testing.T, srv *CnAgentServer, node *fakeNode) {
				node.mu.Lock()
				node.killCmdAlways["list "+sysfsNvmeSubsysDir] = true
				node.mu.Unlock()
			}, -1},
		{"the first entry's address unknown", nil, 0},
		{"the second entry's address unknown", nil, 1},
	}
	seen := map[bool]int{}
	t.Cleanup(func() {
		if !t.Failed() && (seen[true] == 0 || seen[false] == 0) {
			t.Errorf("the matrix is vacuous: %d entries named, %d not",
				seen[true], seen[false])
		}
	})
	for _, s0 := range states {
		for _, s1 := range states {
			for _, gate := range gates {
				if gate.unknown >= 0 &&
					[]string{s0, s1}[gate.unknown] == "none" {
					continue
				}
				name := fmt.Sprintf("%s, %s, %s", s0, s1, gate.name)
				t.Run(name, func(t *testing.T) {
					srv, node := newTestServer(t)
					srv.retryInterval = time.Hour
					withPassClock(srv, node)
					opts := cloneOpts(2, twoEps())
					opts.leftoverOk = true
					syncupBoth(t, srv, opts)
					for i, state := range []string{s0, s1} {
						switch state {
						case "connecting":
							node.setCtrlState(testSrcNqn, eps[i].addr,
								eps[i].svcId, "connecting")
						case "none":
							dropLegCtrl(t, node, testSrcNqn, eps[i].addr,
								eps[i].svcId)
						}
					}
					if gate.unknown >= 0 {
						e := eps[gate.unknown]
						ctrl := ctrlNameOf(node, testSrcNqn, e.addr, e.svcId)
						node.mu.Lock()
						node.killReadAlways[sysfsNvmeCtrlDir+"/"+ctrl+
							"/address"] = true
						node.mu.Unlock()
					} else {
						gate.arm(t, srv, node)
					}
					details := rowRound(t, srv, 2).GetAgentReply().
						GetDetails()
					var had []bool
					for _, e := range eps {
						had = append(had, srcHasCtrl(node, e.addr, e.svcId))
					}
					if _, err := srv.SyncupCntlr(context.Background(),
						cntlrReq(opts)); err != nil {
						t.Fatalf("SyncupCntlr: %v", err)
					}
					// The one state where the two part, on purpose.
					parts := gate.unknown == 1 && s0 == "none"
					for i, e := range eps {
						named := strings.Contains(details,
							noCtrlName(testSrcNqn, e.addr, e.svcId))
						connected := !had[i] &&
							srcHasCtrl(node, e.addr, e.svcId)
						seen[named]++
						if parts && i == 0 {
							if named || !connected {
								t.Fatalf("entry %d: named %v, connected "+
									"%v; want the converge alone to "+
									"connect it", i, named, connected)
							}
							continue
						}
						if named != connected {
							t.Fatalf("entry %d: the read named it: %v; "+
								"the converge after it connected it: %v "+
								"(%q)", i, named, connected, details)
						}
					}
				})
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The dm-clone (CN18 steps 3 to 5, CN30)
// ---------------------------------------------------------------------------

// rowDmCloneShapes are the dm-clone's states that cloneDmName names, each
// with the verdict's words for it and the dmsetup verbs the converge's
// re-send runs on the dm-clone: a create, reload or resume of step 3, and
// the enable of step 5 after a create or a reload, which load hydration off.
var rowDmCloneShapes = []struct {
	name  string
	arm   func(t *testing.T, srv *CnAgentServer, node *fakeNode, clone string)
	what  string
	verbs []string
}{
	{"absent", func(t *testing.T, srv *CnAgentServer, node *fakeNode,
		clone string) {
		rowDropDm(node, clone)
	}, "absent", []string{"create", "message"}},
	{"suspended", func(t *testing.T, srv *CnAgentServer, node *fakeNode,
		clone string) {
		rowSuspendDm(t, node, clone)
	}, "differs: suspended", []string{"clear", "resume"}},
	{"another metadata device", func(t *testing.T, srv *CnAgentServer,
		node *fakeNode, clone string) {
		cloneTableArg(t, node, clone, 0, "253:999")
	}, "differs: table is not the desired clone target",
		[]string{"suspend", "reload", "resume", "message"}},
	{"another destination", func(t *testing.T, srv *CnAgentServer,
		node *fakeNode, clone string) {
		cloneTableArg(t, node, clone, 1, "253:998")
	}, "differs: table is not the desired clone target",
		[]string{"suspend", "reload", "resume", "message"}},
	{"another source number", func(t *testing.T, srv *CnAgentServer,
		node *fakeNode, clone string) {
		cloneTableArg(t, node, clone, 2, "259:997")
	}, "differs: table is not the desired clone target",
		[]string{"suspend", "reload", "resume", "message"}},
	{"another region size", func(t *testing.T, srv *CnAgentServer,
		node *fakeNode, clone string) {
		cloneTableArg(t, node, clone, 3, "16")
	}, "differs: table is not the desired clone target",
		[]string{"suspend", "reload", "resume", "message"}},
	{"hydration off", func(t *testing.T, srv *CnAgentServer,
		node *fakeNode, clone string) {
		node.mu.Lock()
		node.dms[clone].noHydration = true
		node.mu.Unlock()
	}, "differs: " + detailsCloneNotHydrating, []string{"message"}},
	// The cn suite's giveback: the source comes back, and its head under a
	// new number, which only the table compare finds.
	{"its source back under a new head number", func(t *testing.T,
		srv *CnAgentServer, node *fakeNode, clone string) {
		srcReplug(t, node)
	}, "differs: table is not the desired clone target",
		[]string{"suspend", "reload", "resume", "message"}},
}

// TestADmCloneIsNamedWhileAConvergeWouldChangeIt: a read names a clone's
// dm-clone that is absent, suspended, whose table differs in one of its four
// positional arguments — the metadata device, the destination, the source,
// as when the source came back under a new head number, and the region size
// — or whose hydration is off, while its source reads ready, its clone meta
// group's and its destination raid0's rows read OK and its read answered
// with its metadata read-write. The one SyncupCntlr the worker re-sends on
// that round creates, reloads, resumes or enables it, and the read after it
// names nothing.
func TestADmCloneIsNamedWhileAConvergeWouldChangeIt(t *testing.T) {
	for _, raid1 := range []bool{false, true} {
		for _, shape := range rowDmCloneShapes {
			t.Run(fmt.Sprintf("raid1=%v, %s", raid1, shape.name),
				func(t *testing.T) {
					srv, node := newTestServer(t)
					srv.retryInterval = time.Hour
					withPassClock(srv, node)
					opts := cloneOpts(2)
					opts.raid1 = raid1
					syncupBoth(t, srv, opts)
					held := cntlrReq(opts)
					assertRowVerdict(t, rowRound(t, srv, 2).GetAgentReply(),
						"the fixture")
					clone := cloneName(srv, testClone)
					shape.arm(t, srv, node, clone)

					round := rowRound(t, srv, 2)
					assertRowVerdict(t, round.GetAgentReply(), "the read",
						rowNamed(clone, shape.what))
					node.Reset()
					reply := rowResend(t, srv, held, round)
					if reply == nil {
						t.Fatalf("the round that names the dm-clone " +
							"brought no re-send")
					}
					if got := rowDmChanges(node, clone); !slices.Equal(got,
						shape.verbs) {
						t.Fatalf("the re-send ran %q on the dm-clone, "+
							"want %q", got, shape.verbs)
					}
					assertRowVerdict(t, reply.GetAgentReply(), "the re-send")
					assertRowVerdict(t, rowRound(t, srv, 2).GetAgentReply(),
						"the read after the re-send")
					assertOk(t, probedCntlrInfo(t, srv).
						GetCloneIdToDmClone()[testClone], "the dm-clone")
				})
		}
	}
}

// TestASuspendedDmCloneReadsSuspended: a dm-clone suspended on the table it
// wants serves no IO, so its row reads ERROR "suspended", the status line
// after it, on a read, as a suspended pool, concat or raid0 reads; the
// converge resumes it, and its row reads OK.
func TestASuspendedDmCloneReadsSuspended(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	opts := cloneOpts(2)
	syncupBoth(t, srv, opts)
	clone := cloneName(srv, testClone)
	rowSuspendDm(t, node, clone)

	row := probedCntlrInfo(t, srv).GetCloneIdToDmClone()[testClone]
	first, raw, _ := strings.Cut(row.GetDetails(), "\n")
	if row.GetStatus() != pb.ResStatus_RES_STATUS_ERROR ||
		first != detailsSuspended || !strings.Contains(raw, " clone ") {
		t.Fatalf("the suspended dm-clone's row is %v %q, want ERROR %q "+
			"and the status line", row.GetStatus(), row.GetDetails(),
			detailsSuspended)
	}
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(opts))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	assertOk(t, reply.GetCntlrInfo().GetCloneIdToDmClone()[testClone],
		"the converge's dm-clone row")
}

// TestAnAbsentDmCloneHeldBackByItsDestinationRaid0SaysSo: an absent
// dm-clone that only its destination raid0 holds back reads MISSING
// "destination raid0 not ready" with no reason, on the read and in the
// converge's reply alike (CN18, CN30), as cloneRows writes it.
func TestAnAbsentDmCloneHeldBackByItsDestinationRaid0SaysSo(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	opts := cloneOpts(2)
	syncupBoth(t, srv, opts)
	rowDropDm(node, cloneName(srv, testClone))
	raid0 := raid0Name(srv, testTd)
	rowSuspendDm(t, node, raid0)
	node.mu.Lock()
	node.failCmdAlways["dmsetup resume "+raid0] =
		"device-mapper: resume ioctl failed"
	node.mu.Unlock()
	check := func(channel string, row *pb.ResInfo) {
		t.Helper()
		if row.GetStatus() != pb.ResStatus_RES_STATUS_MISSING ||
			row.GetDetails() != detailsCloneDstNotReady ||
			row.GetReason() != pb.ResReason_RES_REASON_NONE {
			t.Errorf("the %s's dm-clone row is %v %q %v, want MISSING %q "+
				"with no reason", channel, row.GetStatus(), row.GetDetails(),
				row.GetReason(), detailsCloneDstNotReady)
		}
	}
	check("read", probedCntlrInfo(t, srv).GetCloneIdToDmClone()[testClone])
	reply, err := srv.SyncupCntlr(context.Background(), cntlrReq(opts))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	check("converge", reply.GetCntlrInfo().GetCloneIdToDmClone()[testClone])
}

// TestAFailedDmCloneStepIsNamedByTheConverge: a create of the dm-clone that
// fails once is named by the converge's own reply too (both channels), with
// the refusal, so the round that answers from that converge brings the
// re-send that creates it; and so is an enable that fails once.
func TestAFailedDmCloneStepIsNamedByTheConverge(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(node *fakeNode, clone string)
		// fails is the command the converge's step runs and the fake
		// refuses once.
		fails string
		verbs []string
	}{
		{"a create", func(node *fakeNode, clone string) {
			rowDropDm(node, clone)
		}, "dmsetup create ", []string{"create", "message"}},
		{"an enable", func(node *fakeNode, clone string) {
			node.mu.Lock()
			defer node.mu.Unlock()
			node.dms[clone].noHydration = true
		}, "dmsetup message ", []string{"message"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			opts := cloneOpts(2)
			syncupBoth(t, srv, opts)
			held := cntlrReq(opts)
			clone := cloneName(srv, testClone)
			const refusal = "device-mapper: ioctl failed"
			tc.arm(node, clone)
			node.mu.Lock()
			node.failCmd[tc.fails+clone] = refusal
			node.mu.Unlock()

			reply, err := srv.SyncupCntlr(context.Background(), held)
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if details := reply.GetAgentReply().GetDetails(); !strings.
				HasPrefix(details, rowNamed(clone, "differs: ")) ||
				!strings.Contains(details, refusal) {
				t.Fatalf("the converge whose step failed replied %v, want "+
					"it to name the dm-clone with the refusal",
					reply.GetAgentReply())
			}
			node.Reset()
			if rowResend(t, srv, held, rowRoundOnly(t, srv, 2)) == nil {
				t.Fatalf("the round after the failed converge brought no " +
					"re-send")
			}
			if got := rowDmChanges(node, clone); !slices.Equal(got,
				tc.verbs) {
				t.Fatalf("the re-send ran %q on the dm-clone, want %q",
					got, tc.verbs)
			}
			assertRowVerdict(t, rowRound(t, srv, 2).GetAgentReply(),
				"the read after the re-send")
		})
	}
}

// rowDmCloneGates are the states in which no converge changes the dm-clone
// whatever its shape, each with arm, which sets it up over the built clone
// after the shape, present, set for a gate that reads a dm-clone that is
// there, and raid1, set for a gate over an md clone meta group.
var rowDmCloneGates = []struct {
	name    string
	arm     func(t *testing.T, srv *CnAgentServer, node *fakeNode, clone string)
	present bool
	raid1   bool
}{
	// The source has a controller and no namespace head: step 1 connects
	// nothing and is not ready.
	{"its source has no namespace", func(t *testing.T, srv *CnAgentServer,
		node *fakeNode, clone string) {
		node.dropNsHead(testSrcNqn)
	}, false, false},
	{"a disconnect of its source in flight", func(t *testing.T,
		srv *CnAgentServer, node *fakeNode, clone string) {
		srv.mu.Lock()
		srv.disconnecting[testSrcNqn] = struct{}{}
		srv.mu.Unlock()
	}, false, false},
	{"its source's lookup did not answer", func(t *testing.T,
		srv *CnAgentServer, node *fakeNode, clone string) {
		node.mu.Lock()
		node.killCmdAlways["list "+sysfsNvmeSubsysDir] = true
		node.mu.Unlock()
	}, false, false},
	{"its source controller's address read did not answer",
		func(t *testing.T, srv *CnAgentServer, node *fakeNode,
			clone string) {
			ctrl := ctrlNameOf(node, testSrcNqn, testIp2, testSvcId2)
			if ctrl == "" {
				t.Fatalf("the source has no controller")
			}
			node.mu.Lock()
			node.killReadAlways[sysfsNvmeCtrlDir+"/"+ctrl+"/address"] = true
			node.mu.Unlock()
		}, false, false},
	{"its clone meta group's row not OK", func(t *testing.T,
		srv *CnAgentServer, node *fakeNode, clone string) {
		cloneGrpNotReady(srv, node, testCloneGrp, testCloneLeg)
	}, false, false},
	// The raid0 is suspended and its resume keeps failing, so its row reads
	// ERROR on both channels.
	{"its destination raid0's row not OK", func(t *testing.T,
		srv *CnAgentServer, node *fakeNode, clone string) {
		raid0 := raid0Name(srv, testTd)
		rowSuspendDm(t, node, raid0)
		node.mu.Lock()
		node.failCmdAlways["dmsetup resume "+raid0] =
			"device-mapper: resume ioctl failed"
		node.mu.Unlock()
	}, false, false},
	{"its status read did not answer", func(t *testing.T,
		srv *CnAgentServer, node *fakeNode, clone string) {
		node.mu.Lock()
		node.failCmdAlways["dmsetup status --noflush "+clone] =
			"device-mapper: status ioctl failed"
		node.mu.Unlock()
	}, true, false},
	{"its metadata read-only", func(t *testing.T, srv *CnAgentServer,
		node *fakeNode, clone string) {
		node.mu.Lock()
		node.dms[clone].metaMode = common.CloneMetadataReadOnly
		node.mu.Unlock()
	}, true, false},
	{"its metadata failed", func(t *testing.T, srv *CnAgentServer,
		node *fakeNode, clone string) {
		node.mu.Lock()
		node.dms[clone].metaMode = common.CloneMetadataFail
		node.mu.Unlock()
	}, true, false},
	// The numbers step 3 reads past the gate: a read that did not answer
	// names nothing, and the converge, which fails step 3 on it, changes
	// nothing and names nothing either.
	{"its source head's number read did not answer", func(t *testing.T,
		srv *CnAgentServer, node *fakeNode, clone string) {
		head, _ := headOf(t, node, testSrcNqn)
		node.mu.Lock()
		node.killReadAlways[sysfsBlockDir+"/"+head+"/dev"] = true
		node.mu.Unlock()
		assertCloneGateOpen(t, srv)
	}, false, false},
	{"its md clone meta group's number read did not answer",
		func(t *testing.T, srv *CnAgentServer, node *fakeNode,
			clone string) {
			md := node.mdNode(cloneMdPath(srv))
			if md == "" {
				t.Fatalf("the clone meta group has no array")
			}
			node.mu.Lock()
			node.killReadAlways[sysfsBlockDir+"/"+
				strings.TrimPrefix(md, "/dev/")+"/dev"] = true
			node.mu.Unlock()
			assertCloneGateOpen(t, srv)
		}, false, true},
}

// assertCloneGateOpen holds a number-read gate case to its point: a read of
// the cntlr finds the source ready and the clone meta group's and the
// destination raid0's rows OK, so only the number read inside the build
// stands between the dm-clone and its name (cloneDmGate, readCloneDmBuild).
func assertCloneGateOpen(t *testing.T, srv *CnAgentServer) {
	t.Helper()
	info := probedCntlrInfo(t, srv)
	assertOk(t, info.GetCloneIdToTarget()[testClone], "the source's row")
	assertOk(t, info.GetGrpIdToMdRaid()[testCloneGrp],
		"the clone meta group's row")
	assertOk(t, info.GetTdIdToRaid0()[testTd], "the destination raid0's row")
}

// rowBdevReadOnly sets the block layer's read-only flag of a dm device, as an
// operator's `blockdev --setro` leaves it, its table read-write.
func rowBdevReadOnly(t *testing.T, srv *CnAgentServer, node *fakeNode,
	name string) {
	t.Helper()
	node.mu.Lock()
	defer node.mu.Unlock()
	dm := node.dms[name]
	if dm == nil {
		t.Fatalf("no dm device %s", name)
	}
	dm.bdevReadOnly = true
}

// TestAReadOnlyDmCloneIsLeftAsItIs: a dm-clone whose block-layer read-only
// flag an operator set is left as it is (CN18 step 3): a read names nothing,
// and the converge reloads nothing, makes nothing writable and sends no
// message, on a dm-clone that otherwise serves. The flag stops no host
// write, which reaches the dm-clone as a bio remapped through the ns-dev
// above it. A read-only dm-clone that the build acts on for another cause is
// named and changed for that cause, and is still not made writable.
func TestAReadOnlyDmCloneIsLeftAsItIs(t *testing.T) {
	for _, hydrationOff := range []bool{false, true} {
		t.Run(fmt.Sprintf("hydration off %v", hydrationOff),
			func(t *testing.T) {
				srv, node := newTestServer(t)
				srv.retryInterval = time.Hour
				opts := cloneOpts(2)
				syncupBoth(t, srv, opts)
				clone := cloneName(srv, testClone)
				rowBdevReadOnly(t, srv, node, clone)
				var want []string
				if hydrationOff {
					node.mu.Lock()
					node.dms[clone].noHydration = true
					node.mu.Unlock()
					want = []string{"message"}
				}
				read := rowRound(t, srv, 2).GetAgentReply()
				named := strings.Contains(read.GetDetails(),
					"enumeration failed: "+clone+": ")
				if named != hydrationOff {
					t.Fatalf("the read %v names the dm-clone: %v, want %v",
						read, named, hydrationOff)
				}
				node.Reset()
				reply, err := srv.SyncupCntlr(context.Background(),
					cntlrReq(opts))
				if err != nil {
					t.Fatalf("SyncupCntlr: %v", err)
				}
				if got := rowDmChanges(node, clone); !slices.Equal(got,
					want) {
					t.Fatalf("the converge ran %q on the dm-clone, want %q",
						got, want)
				}
				if got := node.callsMatching("blockdev --setrw " +
					srv.nf.DmPath(clone)); len(got) != 0 {
					t.Fatalf("the converge made the dm-clone writable: %q",
						got)
				}
				assertRowVerdict(t, reply.GetAgentReply(), "the converge")
				assertRowVerdict(t, rowRound(t, srv, 2).GetAgentReply(),
					"the read after the converge")
			})
	}
}

// TestADmCloneReadThatDidNotAnswerInAConvergeNamesNothing: a converge whose
// own read inside the build did not answer names nothing (CN30), whatever
// that read kept from being done; only a command that fails names the
// dm-clone. The read inside step 5, the enable's status read, and the
// source head's number read of step 3 each leave a dm-clone with hydration
// off as it is, and the converge's reply names nothing. The read after the
// enable's status read names the dm-clone with hydration off, and the re-send
// on that round enables it.
func TestADmCloneReadThatDidNotAnswerInAConvergeNamesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(t *testing.T, node *fakeNode, clone string)
		// lasts: the read keeps not answering, so the read after the
		// converge names nothing either.
		lasts bool
	}{
		// The pass reads the dm-clone's status before step 3, for its knobs
		// and for its enable: the third read is the enable's own.
		{"the enable's status read", func(t *testing.T, node *fakeNode,
			clone string) {
			node.mu.Lock()
			node.killCmdNth["dmsetup status --noflush "+clone] = 3
			node.mu.Unlock()
		}, false},
		{"step 3's source head number read", func(t *testing.T,
			node *fakeNode, clone string) {
			head, _ := headOf(t, node, testSrcNqn)
			node.mu.Lock()
			node.killReadAlways[sysfsBlockDir+"/"+head+"/dev"] = true
			node.mu.Unlock()
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			opts := cloneOpts(2)
			syncupBoth(t, srv, opts)
			held := cntlrReq(opts)
			clone := cloneName(srv, testClone)
			node.mu.Lock()
			node.dms[clone].noHydration = true
			node.mu.Unlock()
			tc.arm(t, node, clone)

			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(), held)
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if got := rowDmChanges(node, clone); len(got) != 0 {
				t.Fatalf("the case is vacuous: the converge ran %q on the "+
					"dm-clone", got)
			}
			assertRowNotNamed(t, reply.GetAgentReply(), "the converge", clone)
			round := rowRound(t, srv, 2)
			if tc.lasts {
				assertRowNotNamed(t, round.GetAgentReply(), "the read", clone)
				return
			}
			assertRowVerdict(t, round.GetAgentReply(), "the read",
				rowNamed(clone, "differs: "+detailsCloneNotHydrating))
			node.Reset()
			if rowResend(t, srv, held, round) == nil {
				t.Fatalf("the round that names the dm-clone brought no " +
					"re-send")
			}
			if got := rowDmChanges(node, clone); !slices.Equal(got,
				[]string{"message"}) {
				t.Fatalf("the re-send ran %q on the dm-clone, want the "+
					"enable", got)
			}
		})
	}
}

// TestADmCloneIsNotNamedWhereAConvergeWouldNotChangeIt bounds the name: a
// dm-clone a converge would otherwise create, reload, resume or enable is
// not named while its source is not ready — no namespace, a disconnect in
// flight, a lookup or an address read that did not answer — while its clone
// meta group's or its destination raid0's row is not OK, while its status
// read did not answer, or while its metadata is read-only or failed; and in
// none of them does a converge change it.
func TestADmCloneIsNotNamedWhereAConvergeWouldNotChangeIt(t *testing.T) {
	for _, gate := range rowDmCloneGates {
		for _, shape := range rowDmCloneShapes {
			if shape.name == "absent" && gate.present {
				continue
			}
			t.Run(gate.name+", "+shape.name, func(t *testing.T) {
				srv, node := newTestServer(t)
				srv.retryInterval = time.Hour
				withPassClock(srv, node)
				opts := cloneOpts(2)
				opts.leftoverOk = true
				opts.raid1 = gate.raid1
				syncupBoth(t, srv, opts)
				clone := cloneName(srv, testClone)
				shape.arm(t, srv, node, clone)
				gate.arm(t, srv, node, clone)

				assertRowNotNamed(t, rowRound(t, srv, 2).GetAgentReply(),
					"the read", clone)
				node.Reset()
				reply, err := srv.SyncupCntlr(context.Background(),
					cntlrReq(opts))
				if err != nil {
					t.Fatalf("SyncupCntlr: %v", err)
				}
				if got := rowDmChanges(node, clone); len(got) != 0 {
					t.Fatalf("a converge ran %q on the dm-clone the read "+
						"does not name", got)
				}
				assertRowNotNamed(t, reply.GetAgentReply(), "the converge",
					clone)
			})
		}
	}
}

// TestTheReadNamesADmCloneExactlyWhenAConvergeWouldChangeIt holds the read to
// the converge over the dm-clone as built and in each shape the name covers,
// under its gates passing and under each gate that holds it back: the read
// names the dm-clone exactly when the converge after it changes it.
func TestTheReadNamesADmCloneExactlyWhenAConvergeWouldChangeIt(t *testing.T) {
	shapes := append([]struct {
		name  string
		arm   func(t *testing.T, srv *CnAgentServer, node *fakeNode, clone string)
		what  string
		verbs []string
	}{{"as built", func(*testing.T, *CnAgentServer, *fakeNode, string) {},
		"", nil}, {"read-only", rowBdevReadOnly, "", nil}},
		rowDmCloneShapes...)
	gates := append([]struct {
		name    string
		arm     func(t *testing.T, srv *CnAgentServer, node *fakeNode, clone string)
		present bool
		raid1   bool
	}{{"its gates passing", func(*testing.T, *CnAgentServer, *fakeNode,
		string) {
	}, false, false}}, rowDmCloneGates...)
	seen := map[bool]int{}
	t.Cleanup(func() {
		if !t.Failed() && (seen[true] == 0 || seen[false] == 0) {
			t.Errorf("the matrix is vacuous: %d cases named the dm-clone, "+
				"%d did not", seen[true], seen[false])
		}
	})
	for _, shape := range shapes {
		for _, gate := range gates {
			if shape.name == "absent" && gate.present {
				continue
			}
			t.Run(shape.name+", "+gate.name, func(t *testing.T) {
				srv, node := newTestServer(t)
				srv.retryInterval = time.Hour
				withPassClock(srv, node)
				opts := cloneOpts(2)
				opts.leftoverOk = true
				opts.raid1 = gate.raid1
				syncupBoth(t, srv, opts)
				clone := cloneName(srv, testClone)
				shape.arm(t, srv, node, clone)
				gate.arm(t, srv, node, clone)
				named := strings.Contains(
					rowRound(t, srv, 2).GetAgentReply().GetDetails(),
					"enumeration failed: "+clone+": ")
				node.Reset()
				if _, err := srv.SyncupCntlr(context.Background(),
					cntlrReq(opts)); err != nil {
					t.Fatalf("SyncupCntlr: %v", err)
				}
				changed := len(rowDmChanges(node, clone)) > 0
				seen[named]++
				if named != changed {
					t.Fatalf("the read named the dm-clone: %v; the "+
						"converge after it changed it: %v (%q)", named,
						changed, rowDmChanges(node, clone))
				}
			})
		}
	}
}
