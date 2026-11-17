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

// The rows a cntlr's verdict names by a test of their own (cnagent.md CN30):
// a leg's wrapper (wrapperName), a RedundNone group's dm-linear
// (linearGroupStep), and a transfer device, which rests on its origin's raid0
// only where the transfer is served (xferServed). Each is named while a
// converge would change it, and not otherwise, and one re-send of the held
// request, as the worker sends it on a round that names it (dnv-worker.md
// RW4), cures it.

// rowRound is one read of the fixture cntlr and the check round after it, as
// the worker gets the round.
func rowRound(
	t *testing.T,
	srv *CnAgentServer,
	revision uint64,
) *pb.CheckCntlrReply {
	t.Helper()
	refreshTestCntlr(t, srv)
	return rowRoundOnly(t, srv, revision)
}

// rowRoundOnly is one check round of the fixture cntlr with no read before it:
// it answers from the latest observation, a converge's included.
func rowRoundOnly(
	t *testing.T,
	srv *CnAgentServer,
	revision uint64,
) *pb.CheckCntlrReply {
	t.Helper()
	reply, _ := srv.checkCntlrRound(context.Background(),
		&pb.CheckCntlrRequest{
			ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr(), Revision: revision,
		}, nil)
	return reply
}

// rowResend plays the worker for one round (dnv-worker.md RW4): a round
// whose code is not zero, or whose revision is not the held request's,
// re-sends the held request. It returns that syncup's reply, and nil when
// the round brings none.
func rowResend(
	t *testing.T,
	srv *CnAgentServer,
	held *pb.SyncupCntlrRequest,
	round *pb.CheckCntlrReply,
) *pb.SyncupCntlrReply {
	t.Helper()
	if round.GetAgentReply().GetCode() == 0 &&
		round.GetRevision() == held.GetRevision() {
		return nil
	}
	reply, err := srv.SyncupCntlr(context.Background(), held)
	if err != nil {
		t.Fatalf("the re-sent SyncupCntlr: %v", err)
	}
	return reply
}

// rowNamed is how a verdict names a row: as an enumeration that did not
// answer, the row's name, then what is wrong with it.
func rowNamed(name, what string) string {
	return "enumeration failed: " + name + ": " + what
}

// assertRowVerdict checks a verdict is exactly the names want, in any order:
// code zero for none.
func assertRowVerdict(
	t *testing.T,
	reply *pb.AgentReply,
	label string,
	want ...string,
) {
	t.Helper()
	if len(want) == 0 {
		if reply.GetCode() != 0 {
			t.Fatalf("%s: code %d details %q, want a clean verdict",
				label, reply.GetCode(), reply.GetDetails())
		}
		return
	}
	got := strings.Split(reply.GetDetails(), "; ")
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if reply.GetCode() != common.ReplyCodeLeftover || !slices.Equal(got, want) {
		t.Fatalf("%s: code %d details %q, want code %d naming %q",
			label, reply.GetCode(), reply.GetDetails(),
			common.ReplyCodeLeftover, want)
	}
}

// assertRowNotNamed checks a verdict names no row of the device name.
func assertRowNotNamed(t *testing.T, reply *pb.AgentReply, label, name string) {
	t.Helper()
	if strings.Contains(reply.GetDetails(), "enumeration failed: "+name+": ") {
		t.Fatalf("%s: the verdict %q names %s", label, reply.GetDetails(),
			name)
	}
}

// rowDmChanges are the verbs of the recorded commands that changed the dm
// device name, in order: dmsetup's, and "setrw" or "setro" for a blockdev
// of the device's path.
func rowDmChanges(node *fakeNode, name string) []string {
	var out []string
	for _, call := range node.Mutations() {
		fields := strings.Fields(call)
		switch {
		case len(fields) >= 4 && fields[0] == "cmd" &&
			fields[1] == "dmsetup" && fields[3] == name:
			out = append(out, fields[2])
		case len(fields) == 4 && fields[0] == "cmd" &&
			fields[1] == "blockdev" && fields[3] == "/dev/mapper/"+name:
			out = append(out, strings.TrimPrefix(fields[2], "--"))
		}
	}
	return out
}

// rowStepVerbs are the verbs each converge step runs (Dm.Create, Dm.Reload,
// Dm.Resume, and Dm.Reload then Dm.SetWritable).
var rowStepVerbs = map[dmStep][]string{
	dmStepCreate:         {"create"},
	dmStepReload:         {"suspend", "reload", "resume"},
	dmStepResume:         {"clear", "resume"},
	dmStepReloadWritable: {"suspend", "reload", "resume", "setrw"},
}

// rowDropDm takes a dm device away from the node, its number with it.
func rowDropDm(node *fakeNode, name string) {
	node.mu.Lock()
	defer node.mu.Unlock()
	delete(node.dms, name)
	delete(node.devNo, "/dev/mapper/"+name)
	delete(node.devSize, "/dev/mapper/"+name)
}

// rowHalveDm gives a single-target dm device the same table at half its size.
func rowHalveDm(t *testing.T, node *fakeNode, name string) {
	t.Helper()
	node.mu.Lock()
	defer node.mu.Unlock()
	dm := node.dms[name]
	if dm == nil {
		t.Fatalf("no dm device %s", name)
	}
	fields := strings.Fields(dm.table)
	var sectors uint64
	if _, err := fmt.Sscan(fields[1], &sectors); err != nil {
		t.Fatalf("the table of %s is %q", name, dm.table)
	}
	fields[1] = fmt.Sprint(sectors / 2)
	dm.table = strings.Join(fields, " ")
}

// rowShiftDm gives a dm-linear the same table one sector further into the
// device it maps.
func rowShiftDm(t *testing.T, node *fakeNode, name string) {
	t.Helper()
	node.mu.Lock()
	defer node.mu.Unlock()
	dm := node.dms[name]
	if dm == nil {
		t.Fatalf("no dm device %s", name)
	}
	fields := strings.Fields(dm.table)
	var offset uint64
	if len(fields) != 5 {
		t.Fatalf("the table of %s is %q", name, dm.table)
	}
	if _, err := fmt.Sscan(fields[4], &offset); err != nil {
		t.Fatalf("the table of %s is %q", name, dm.table)
	}
	fields[4] = fmt.Sprint(offset + 1)
	dm.table = strings.Join(fields, " ")
}

// rowSetroDm sets the block layer's own read-only flag of a dm device, as
// an operator's `blockdev --setro` does: a reload and a resume leave it set,
// and only `blockdev --setrw` clears it (fakeDm.bdevReadOnly).
func rowSetroDm(t *testing.T, node *fakeNode, name string) {
	t.Helper()
	node.mu.Lock()
	defer node.mu.Unlock()
	dm := node.dms[name]
	if dm == nil {
		t.Fatalf("no dm device %s", name)
	}
	dm.bdevReadOnly = true
}

// rowLoadedReadOnlyDm makes a dm device's live table read-only, as a load
// with --readonly does, which a reload onto a read-write table clears; dnv
// never loads one so.
func rowLoadedReadOnlyDm(t *testing.T, node *fakeNode, name string) {
	t.Helper()
	node.mu.Lock()
	defer node.mu.Unlock()
	dm := node.dms[name]
	if dm == nil {
		t.Fatalf("no dm device %s", name)
	}
	dm.readOnly = true
}

// rowSuspendDm leaves a dm device suspended on its table.
func rowSuspendDm(t *testing.T, node *fakeNode, name string) {
	t.Helper()
	node.mu.Lock()
	defer node.mu.Unlock()
	dm := node.dms[name]
	if dm == nil {
		t.Fatalf("no dm device %s", name)
	}
	dm.suspended = true
}

// ---------------------------------------------------------------------------
// The leg wrapper (CN10, CN30)
// ---------------------------------------------------------------------------

// rowWrapperShapes are the wrapper's states that wrapperName names, each with
// the verdict's words for it and the step the converge takes.
var rowWrapperShapes = []struct {
	name string
	arm  func(t *testing.T, node *fakeNode, wrapper string)
	what string
	step dmStep
}{
	{"absent", func(t *testing.T, node *fakeNode, wrapper string) {
		rowDropDm(node, wrapper)
	}, "absent", dmStepCreate},
	{"another table", rowHalveDm,
		"differs: table is not the desired linear target", dmStepReload},
	// A head the kernel made anew while the wrapper held the old one open:
	// the wrapper still maps the old number.
	{"maps another head number", func(t *testing.T, node *fakeNode,
		wrapper string) {
		node.mu.Lock()
		defer node.mu.Unlock()
		fields := strings.Fields(node.dms[wrapper].table)
		fields[3] = "259:999"
		node.dms[wrapper].table = strings.Join(fields, " ")
	}, "differs: table is not the desired linear target", dmStepReload},
	{"set read-only", rowSetroDm, "differs: read-only",
		dmStepReloadWritable},
	{"loaded read-only", rowLoadedReadOnlyDm, "differs: read-only",
		dmStepReloadWritable},
	{"suspended", rowSuspendDm, "differs: suspended", dmStepResume},
}

// TestALegWrapperIsNamedWhileAConvergeWouldChangeIt: a read names a wanted
// leg's wrapper that is absent, carries another table, maps another number
// than the head's, is set read-only by the block layer's flag or loaded
// read-only, or is suspended, while the leg's lookup has every controller's
// address and a multipath namespace head, on a standby and on a primary; the
// one SyncupCntlr the worker re-sends on that round creates, reloads, reloads
// and makes writable, or resumes it, and the read after it names nothing.
func TestALegWrapperIsNamedWhileAConvergeWouldChangeIt(t *testing.T) {
	for _, primary := range []bool{false, true} {
		role := "standby"
		if primary {
			role = "primary"
		}
		for _, shape := range rowWrapperShapes {
			t.Run(role+", "+shape.name, func(t *testing.T) {
				srv, node := newTestServer(t)
				srv.retryInterval = time.Hour
				opts := reqOpts{revision: 2, primary: primary}
				syncupBoth(t, srv, opts)
				held := cntlrReq(opts)
				assertRowVerdict(t, rowRound(t, srv, 2).GetAgentReply(),
					"the fixture")
				wrapper := legName(srv, testDataLeg)
				shape.arm(t, node, wrapper)

				round := rowRound(t, srv, 2)
				assertRowVerdict(t, round.GetAgentReply(), "the read",
					rowNamed(wrapper, shape.what))
				node.Reset()
				reply := rowResend(t, srv, held, round)
				if reply == nil {
					t.Fatalf("the round that names the wrapper brought " +
						"no re-send")
				}
				ran := rowDmChanges(node, wrapper)
				// The read after the re-send comes first: a step that does
				// not cure the wrapper shows as the same name again, which
				// would bring a re-send on every second round.
				assertRowVerdict(t, reply.GetAgentReply(), "the re-send")
				assertRowVerdict(t, rowRound(t, srv, 2).GetAgentReply(),
					"the read after the re-send")
				if want := rowStepVerbs[shape.step]; !slices.Equal(ran,
					want) {
					t.Fatalf("the re-send ran %q on the wrapper, want %q",
						ran, want)
				}
			})
		}
	}
}

// TestAFailedWrapperStepIsNamedByTheConverge: a create of the wrapper that
// fails once is named by the converge's own reply too (both channels), so
// the round that answers from that converge brings the re-send that
// creates it.
func TestAFailedWrapperStepIsNamedByTheConverge(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	opts := reqOpts{revision: 2}
	syncupBoth(t, srv, opts)
	held := cntlrReq(opts)
	wrapper := legName(srv, testDataLeg)
	rowDropDm(node, wrapper)
	const refusal = "device-mapper: create ioctl failed"
	node.mu.Lock()
	node.failCmd["dmsetup create "+wrapper] = refusal
	node.mu.Unlock()

	round := rowRound(t, srv, 2)
	assertRowVerdict(t, round.GetAgentReply(), "the read",
		rowNamed(wrapper, "absent"))
	reply := rowResend(t, srv, held, round)
	if reply == nil || !strings.HasPrefix(reply.GetAgentReply().GetDetails(),
		rowNamed(wrapper, "differs: ")) ||
		!strings.Contains(reply.GetAgentReply().GetDetails(), refusal) {
		t.Fatalf("the converge whose create failed replied %v, want it "+
			"to name the wrapper with the refusal", reply.GetAgentReply())
	}
	if dmPresent(node, wrapper) {
		t.Fatalf("the case is vacuous: the create went through")
	}
	node.Reset()
	if rowResend(t, srv, held, rowRoundOnly(t, srv, 2)) == nil {
		t.Fatalf("the round after the failed converge brought no re-send")
	}
	if got := rowDmChanges(node, wrapper); !slices.Equal(got,
		rowStepVerbs[dmStepCreate]) {
		t.Fatalf("the second re-send ran %q on the wrapper, want a create",
			got)
	}
	assertRowVerdict(t, rowRound(t, srv, 2).GetAgentReply(),
		"the read after the create")
}

// TestALegWrapperIsNotNamedWhereAConvergeWouldNotChangeIt bounds the name:
// a wrapper a converge would build is not named while the converge would
// fail the leg before its wrapper step — the leg's lookup did not answer, a
// controller's address read did not answer, the subsystem has no multipath
// namespace head, a sweep is disconnecting the leg, the head's number read
// did not answer — nor while the wrapper's own read did not answer, nor for
// a leg that is provisioning or that the level suppresses. In none of them
// does a converge change the wrapper.
func TestALegWrapperIsNotNamedWhereAConvergeWouldNotChangeIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts reqOpts
		// arm runs after the fixture's converge.
		arm func(t *testing.T, srv *CnAgentServer, node *fakeNode,
			wrapper, nqn string)
		// legErr is what the converge's leg row reads: the step it fails
		// the leg at, before or at the wrapper's; "" leaves it unread.
		legErr string
	}{
		{"its lookup did not answer", reqOpts{revision: 2},
			func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				wrapper, nqn string) {
				rowDropDm(node, wrapper)
				node.mu.Lock()
				node.killCmdAlways["list "+sysfsNvmeSubsysDir] = true
				node.mu.Unlock()
			}, "is unknown"},
		{"a controller's address read did not answer", reqOpts{revision: 2},
			func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				wrapper, nqn string) {
				rowDropDm(node, wrapper)
				ctrl := ctrlNameOf(node, nqn, testIp, testSvcId)
				if ctrl == "" {
					t.Fatalf("the leg has no controller")
				}
				node.mu.Lock()
				node.killReadAlways[sysfsNvmeCtrlDir+"/"+ctrl+"/address"] =
					true
				node.mu.Unlock()
			}, "is unknown"},
		{"no multipath namespace head", reqOpts{revision: 2},
			func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				wrapper, nqn string) {
				rowDropDm(node, wrapper)
				node.dropNsHead(nqn)
			}, "no multipath namespace for"},
		{"a disconnect in flight", reqOpts{revision: 2},
			func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				wrapper, nqn string) {
				rowDropDm(node, wrapper)
				srv.mu.Lock()
				srv.disconnecting[nqn] = struct{}{}
				srv.mu.Unlock()
			}, "disconnect of"},
		{"its head's number read did not answer", reqOpts{revision: 2},
			func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				wrapper, nqn string) {
				rowSuspendDm(t, node, wrapper)
				node.mu.Lock()
				head := fmt.Sprintf("nvme%dn1", node.subsystems[nqn].idx)
				node.killReadAlways[sysfsBlockDir+"/"+head+"/dev"] = true
				node.mu.Unlock()
			}, "context deadline exceeded"},
		{"its table read did not answer", reqOpts{revision: 2},
			func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				wrapper, nqn string) {
				rowSuspendDm(t, node, wrapper)
				node.mu.Lock()
				node.tableOmit[wrapper] = true
				node.failCmdAlways["dmsetup table "+wrapper] =
					"device-mapper: table ioctl failed"
				node.mu.Unlock()
			}, "table ioctl failed"},
		// The leg's sides are still zeroing: nothing is exported for them,
		// and a subsystem of the leg's NQN with a head is planted to show
		// that the lookup is not what holds the name back.
		{"a provisioning leg",
			reqOpts{revision: 2, unprovisionedDataLeg: true},
			func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				wrapper, nqn string) {
				if dmPresent(node, wrapper) {
					t.Fatalf("a provisioning leg has a wrapper")
				}
				node.mu.Lock()
				node.plantSubsys(40, nqn,
					endpoint{trAddr: testIp, trSvcId: testSvcId})
				node.mu.Unlock()
			}, ""},
		// The level removes the wrapper; its disconnect fails, so the leg's
		// subsystem and head stay for the read to find.
		{"a leg the level suppresses",
			reqOpts{revision: 3, level: pb.SpLevel_SP_LEVEL_NO_SIDE},
			nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			wrapper := legName(srv, testDataLeg)
			nqn := legNqn(srv, testDataLeg)
			if tc.arm == nil {
				syncupBoth(t, srv, reqOpts{revision: 2})
				node.mu.Lock()
				node.failCmdAlways["nvme disconnect --nqn "+nqn] =
					"nvme disconnect: Input/output error"
				node.mu.Unlock()
				tc.opts.leftoverOk = true
				syncupBoth(t, srv, tc.opts)
				awaitDisconnects(t, srv)
				node.mu.Lock()
				kept := node.subsystems[nqn] != nil
				node.mu.Unlock()
				if dmPresent(node, wrapper) || !kept {
					t.Fatalf("the case is vacuous: the level left the " +
						"wrapper, or took the leg's subsystem")
				}
			} else {
				syncupBoth(t, srv, tc.opts)
				tc.arm(t, srv, node, wrapper, nqn)
			}

			assertRowNotNamed(t, rowRound(t, srv, tc.opts.revision).
				GetAgentReply(), "the read", wrapper)
			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(tc.opts))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if got := rowDmChanges(node, wrapper); len(got) != 0 {
				t.Fatalf("a converge ran %q on the wrapper the read does "+
					"not name", got)
			}
			if tc.legErr != "" {
				assertErrorDetails(t,
					reply.GetCntlrInfo().GetLegIdToLeg()[testDataLeg],
					tc.legErr, "the converge's leg row")
			}
		})
	}
}

// TestTheReadNamesALegWrapperExactlyWhenAConvergeWouldChangeIt holds the
// read to the converge on both roles, over the wrapper as wanted, absent,
// half its size, over another head number, set read-only, loaded read-only
// and suspended, each under the lookup as built, a lookup that did not answer, a controller's
// address unknown, no head, a disconnect in flight and a head number read
// that did not answer: the read names the wrapper exactly when the converge
// after it changes the wrapper.
func TestTheReadNamesALegWrapperExactlyWhenAConvergeWouldChangeIt(
	t *testing.T,
) {
	shapes := append([]struct {
		name string
		arm  func(t *testing.T, node *fakeNode, wrapper string)
		what string
		step dmStep
	}{{"as wanted", func(*testing.T, *fakeNode, string) {}, "",
		dmStepNone}}, rowWrapperShapes...)
	lookups := []struct {
		name string
		arm  func(t *testing.T, srv *CnAgentServer, node *fakeNode,
			nqn string)
	}{
		{"its lookup as built", func(*testing.T, *CnAgentServer, *fakeNode,
			string) {
		}},
		{"its lookup did not answer", func(t *testing.T, srv *CnAgentServer,
			node *fakeNode, nqn string) {
			node.mu.Lock()
			node.killCmdAlways["list "+sysfsNvmeSubsysDir] = true
			node.mu.Unlock()
		}},
		{"a controller's address unknown", func(t *testing.T,
			srv *CnAgentServer, node *fakeNode, nqn string) {
			ctrl := ctrlNameOf(node, nqn, testIp, testSvcId)
			node.mu.Lock()
			node.killReadAlways[sysfsNvmeCtrlDir+"/"+ctrl+"/address"] = true
			node.mu.Unlock()
		}},
		{"no head", func(t *testing.T, srv *CnAgentServer, node *fakeNode,
			nqn string) {
			node.dropNsHead(nqn)
		}},
		{"a disconnect in flight", func(t *testing.T, srv *CnAgentServer,
			node *fakeNode, nqn string) {
			srv.mu.Lock()
			srv.disconnecting[nqn] = struct{}{}
			srv.mu.Unlock()
		}},
		{"its head's number unread", func(t *testing.T, srv *CnAgentServer,
			node *fakeNode, nqn string) {
			node.mu.Lock()
			head := fmt.Sprintf("nvme%dn1", node.subsystems[nqn].idx)
			node.killReadAlways[sysfsBlockDir+"/"+head+"/dev"] = true
			node.mu.Unlock()
		}},
	}
	seen := map[bool]int{}
	t.Cleanup(func() {
		if !t.Failed() && (seen[true] == 0 || seen[false] == 0) {
			t.Errorf("the matrix is vacuous: %d cases named the wrapper, "+
				"%d did not", seen[true], seen[false])
		}
	})
	for _, primary := range []bool{false, true} {
		for _, shape := range shapes {
			for _, lookup := range lookups {
				name := fmt.Sprintf("primary %v, %s, %s", primary,
					shape.name, lookup.name)
				t.Run(name, func(t *testing.T) {
					srv, node := newTestServer(t)
					srv.retryInterval = time.Hour
					opts := reqOpts{revision: 2, primary: primary,
						leftoverOk: true}
					syncupBoth(t, srv, opts)
					wrapper := legName(srv, testDataLeg)
					nqn := legNqn(srv, testDataLeg)
					shape.arm(t, node, wrapper)
					lookup.arm(t, srv, node, nqn)
					named := strings.Contains(
						rowRound(t, srv, 2).GetAgentReply().GetDetails(),
						"enumeration failed: "+wrapper+": ")
					node.Reset()
					if _, err := srv.SyncupCntlr(context.Background(),
						cntlrReq(opts)); err != nil {
						t.Fatalf("SyncupCntlr: %v", err)
					}
					changed := len(rowDmChanges(node, wrapper)) > 0
					seen[named]++
					if named != changed {
						t.Fatalf("the read named the wrapper: %v; the "+
							"converge after it changed it: %v (%q)", named,
							changed, rowDmChanges(node, wrapper))
					}
				})
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The RedundNone group's dm-linear (CN12, CN30)
// ---------------------------------------------------------------------------

// rowLateLeg sets the ANA state of the fixture leg's one path: "non-optimized"
// makes the leg late, as a side that has not flipped to this cntlr reads,
// and "optimized" makes it serve.
func rowLateLeg(srv *CnAgentServer, node *fakeNode, legId uint64, state string) {
	node.setAnaState(srv.nf.SideToCnNqn(testCluster, testSp, legId, testCn),
		testIp, testSvcId, state)
}

// TestARedundNoneGroupDeviceIsNamedWhileAConvergeWouldChangeIt: a read
// names a RedundNone group's dm-linear that carries another table, at
// another size or offset, or is set read-only, while its leg serves, and one
// suspended on the table it wants whatever its leg; the one SyncupCntlr the
// worker re-sends on that round reloads, reloads and makes writable, or
// resumes it, and the read after it names nothing. Over a late leg one with
// another table or set read-only is not named, and the converge refuses it
// ("no available leg") and changes nothing.
func TestARedundNoneGroupDeviceIsNamedWhileAConvergeWouldChangeIt(
	t *testing.T,
) {
	for _, tc := range []struct {
		name string
		// arm changes the group's device of the built primary; late keeps
		// its leg late until the first read is checked.
		arm  func(t *testing.T, node *fakeNode, grp string)
		late bool
		// lateWhat is the name a read gives over the late leg, "" for
		// none; what the one over the serving leg.
		lateWhat string
		what     string
		step     dmStep
	}{
		{"another table", rowHalveDm, false, "",
			"differs: table is not the desired linear target", dmStepReload},
		{"another offset", rowShiftDm, false, "",
			"differs: table is not the desired linear target", dmStepReload},
		{"set read-only", rowSetroDm, false, "", "differs: read-only",
			dmStepReloadWritable},
		{"set read-only over a late leg", rowSetroDm, true, "",
			"differs: read-only", dmStepReloadWritable},
		{"another table over a late leg", rowHalveDm, true, "",
			"differs: table is not the desired linear target", dmStepReload},
		{"suspended", rowSuspendDm, false, "", "differs: suspended",
			dmStepResume},
		{"suspended over a late leg", rowSuspendDm, true, "differs: suspended",
			"differs: suspended", dmStepResume},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			opts := reqOpts{revision: 2, primary: true}
			syncupBoth(t, srv, opts)
			held := cntlrReq(opts)
			grp := grpName(srv, testDataGrp)
			tc.arm(t, node, grp)
			if tc.late {
				rowLateLeg(srv, node, testDataLeg, "non-optimized")
				round := rowRound(t, srv, 2)
				if tc.lateWhat == "" {
					assertRowVerdict(t, round.GetAgentReply(),
						"the read over the late leg")
					node.Reset()
					if _, err := srv.SyncupCntlr(context.Background(),
						held); err != nil {
						t.Fatalf("SyncupCntlr: %v", err)
					}
					if got := rowDmChanges(node, grp); len(got) != 0 {
						t.Fatalf("a converge over the late leg ran %q on "+
							"the group's device", got)
					}
					rowLateLeg(srv, node, testDataLeg, "optimized")
				} else {
					assertRowVerdict(t, round.GetAgentReply(),
						"the read over the late leg",
						rowNamed(grp, tc.lateWhat))
				}
			}

			round := rowRound(t, srv, 2)
			assertRowVerdict(t, round.GetAgentReply(), "the read",
				rowNamed(grp, tc.what))
			node.Reset()
			reply := rowResend(t, srv, held, round)
			if reply == nil {
				t.Fatalf("the round that names the device brought no " +
					"re-send")
			}
			ran := rowDmChanges(node, grp)
			// The read after the re-send comes first: a step that does not
			// cure the device shows as the same name again.
			assertRowVerdict(t, reply.GetAgentReply(), "the re-send")
			assertRowVerdict(t, rowRound(t, srv, 2).GetAgentReply(),
				"the read after the re-send")
			if want := rowStepVerbs[tc.step]; !slices.Equal(ran, want) {
				t.Fatalf("the re-send ran %q on the group's device, want %q",
					ran, want)
			}
		})
	}
}

// TestAPromotionAheadOfTheFlipIsNamedOnceTheLegServes is the case the
// RedundNone name exists for: a promotion whose data leg reads
// non-optimized leaves the data group unbuilt and the namespace
// inaccessible, the read names nothing while the leg is late, and once the
// side flips the read names the group's absent device; the one re-send
// builds it and the stack over it, and moves the namespace optimized.
func TestAPromotionAheadOfTheFlipIsNamedOnceTheLegServes(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	if _, err := srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 3})); err != nil {
		t.Fatalf("demote: %v", err)
	}
	rowLateLeg(srv, node, testDataLeg, "non-optimized")
	promote := cntlrReq(reqOpts{revision: 4, primary: true})
	reply, err := srv.SyncupCntlr(ctx, promote)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	assertRowVerdict(t, reply.GetAgentReply(), "the promotion")
	grp := grpName(srv, testDataGrp)
	if dmPresent(node, grp) {
		t.Fatalf("the case is vacuous: the promotion built the group over " +
			"its late leg")
	}
	assertRowVerdict(t, rowRound(t, srv, 4).GetAgentReply(),
		"the read before the flip")

	rowLateLeg(srv, node, testDataLeg, "optimized")
	round := rowRound(t, srv, 4)
	assertRowVerdict(t, round.GetAgentReply(), "the read after the flip",
		rowNamed(grp, "absent"))
	node.Reset()
	resent := rowResend(t, srv, promote, round)
	if resent == nil {
		t.Fatalf("the round that names the group brought no re-send")
	}
	if got := rowDmChanges(node, grp); !slices.Equal(got,
		rowStepVerbs[dmStepCreate]) {
		t.Fatalf("the re-send ran %q on the group's device, want a create",
			got)
	}
	assertRowVerdict(t, resent.GetAgentReply(), "the re-send")
	assertAna(t, node, testNqn, "1", "the re-send")
	assertRowVerdict(t, rowRound(t, srv, 4).GetAgentReply(),
		"the read after the re-send")
}

// TestAFailedLinearGroupStepIsNamedByTheConverge: a reload of the group's
// dm-linear that fails once is named by the converge's own reply too (both
// channels), and the round that answers from that converge brings the
// re-send that reloads it.
func TestAFailedLinearGroupStepIsNamedByTheConverge(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	opts := reqOpts{revision: 2, primary: true}
	syncupBoth(t, srv, opts)
	held := cntlrReq(opts)
	grp := grpName(srv, testDataGrp)
	rowHalveDm(t, node, grp)
	const refusal = "device-mapper: reload ioctl failed"
	node.mu.Lock()
	node.failCmd["dmsetup reload "+grp] = refusal
	node.mu.Unlock()

	reply := rowResend(t, srv, held, rowRound(t, srv, 2))
	if reply == nil {
		t.Fatalf("the round that names the device brought no re-send")
	}
	details := reply.GetAgentReply().GetDetails()
	if !strings.Contains(details, rowNamed(grp, "differs: ")) ||
		!strings.Contains(details, refusal) {
		t.Fatalf("the converge whose reload failed replied %v, want it to "+
			"name the group's device with the refusal", reply.GetAgentReply())
	}
	node.Reset()
	if rowResend(t, srv, held, rowRoundOnly(t, srv, 2)) == nil {
		t.Fatalf("the round after the failed converge brought no re-send")
	}
	if got := rowDmChanges(node, grp); !slices.Equal(got,
		rowStepVerbs[dmStepReload]) {
		t.Fatalf("the second re-send ran %q on the group's device, want a "+
			"reload", got)
	}
	assertRowVerdict(t, rowRound(t, srv, 2).GetAgentReply(),
		"the read after the reload")
}

// TestARedundNoneGroupDeviceIsNotNamedWhereAConvergeWouldNotChangeIt bounds
// the name: a group's device is not named, and a converge changes nothing on
// it, while a sweep disconnects its leg, for one set read-only and suspended
// over a late leg, while its table read did not answer, nor for a group the level
// suppresses or one that is deferred.
func TestARedundNoneGroupDeviceIsNotNamedWhereAConvergeWouldNotChangeIt(
	t *testing.T,
) {
	for _, tc := range []struct {
		name string
		opts reqOpts
		arm  func(t *testing.T, srv *CnAgentServer, node *fakeNode,
			grp, wrapper, nqn string)
	}{
		{"a disconnect in flight", reqOpts{revision: 2, primary: true},
			func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				grp, wrapper, nqn string) {
				rowDropDm(node, grp)
				srv.mu.Lock()
				srv.disconnecting[nqn] = struct{}{}
				srv.mu.Unlock()
			}},
		// The converge reloads a read-only device, which it refuses over a
		// late leg, so the suspend bit names nothing: a name would bring a
		// converge that changes nothing on every second round.
		{"read-only and suspended over a late leg",
			reqOpts{revision: 2, primary: true},
			func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				grp, wrapper, nqn string) {
				rowSetroDm(t, node, grp)
				rowSuspendDm(t, node, grp)
				rowLateLeg(srv, node, testDataLeg, "non-optimized")
			}},
		{"its table read did not answer", reqOpts{revision: 2, primary: true},
			func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				grp, wrapper, nqn string) {
				rowSuspendDm(t, node, grp)
				node.mu.Lock()
				node.tableOmit[grp] = true
				node.failCmdAlways["dmsetup table "+grp] =
					"device-mapper: table ioctl failed"
				node.mu.Unlock()
			}},
		{"a level that suppresses the groups",
			reqOpts{revision: 2, primary: true,
				level: pb.SpLevel_SP_LEVEL_NO_REDUND}, nil},
		// The group's one leg is still zeroing, so the group is deferred;
		// the leg's subsystem and a wrapper as wanted are planted to show
		// that the leg is not what holds the name back.
		{"a deferred group",
			reqOpts{revision: 2, primary: true, unprovisionedDataLeg: true},
			func(t *testing.T, srv *CnAgentServer, node *fakeNode,
				grp, wrapper, nqn string) {
				sectors := newCntlrPlan(srv.nf, cntlrReq(reqOpts{
					revision: 2, primary: true,
					unprovisionedDataLeg: true})).legById[testDataLeg].sectors
				node.mu.Lock()
				defer node.mu.Unlock()
				node.plantSubsys(40, nqn,
					endpoint{trAddr: testIp, trSvcId: testSvcId})
				head := fmt.Sprintf("nvme%dn1", node.subsystems[nqn].idx)
				node.dms[wrapper] = &fakeDm{table: fmt.Sprintf(
					"0 %d linear %s 0", sectors, node.devNo["/dev/"+head])}
				node.devNo["/dev/mapper/"+wrapper] = node.newDevNo()
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			tc.opts.leftoverOk = true
			syncupBoth(t, srv, tc.opts)
			grp := grpName(srv, testDataGrp)
			wrapper := legName(srv, testDataLeg)
			nqn := legNqn(srv, testDataLeg)
			if tc.arm != nil {
				tc.arm(t, srv, node, grp, wrapper, nqn)
			}
			assertRowNotNamed(t, rowRound(t, srv, 2).GetAgentReply(),
				"the read", grp)
			node.Reset()
			if _, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(tc.opts)); err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			if got := rowDmChanges(node, grp); len(got) != 0 {
				t.Fatalf("a converge ran %q on the group's device the read "+
					"does not name", got)
			}
		})
	}
}

// TestARedundNoneGroupDeviceWaitsForItsWrappersName pins the lowest-layer
// rule: a group's device that is absent over a wrapper that is suspended,
// set read-only or over another head number is not named, the wrapper alone
// is, and the one re-send that name brings resumes, reloads and makes
// writable, or reloads the wrapper, finds the leg available and creates the
// group's device in the same pass; the read after it names nothing.
func TestARedundNoneGroupDeviceWaitsForItsWrappersName(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(t *testing.T, node *fakeNode, wrapper string)
		what string
		step dmStep
	}{
		{"its wrapper suspended", rowSuspendDm, "differs: suspended",
			dmStepResume},
		{"its wrapper set read-only", rowSetroDm, "differs: read-only",
			dmStepReloadWritable},
		{"its wrapper over another head number", func(t *testing.T,
			node *fakeNode, wrapper string) {
			node.mu.Lock()
			defer node.mu.Unlock()
			fields := strings.Fields(node.dms[wrapper].table)
			fields[3] = "259:999"
			node.dms[wrapper].table = strings.Join(fields, " ")
		}, "differs: table is not the desired linear target", dmStepReload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			opts := reqOpts{revision: 2, primary: true}
			syncupBoth(t, srv, opts)
			held := cntlrReq(opts)
			grp := grpName(srv, testDataGrp)
			wrapper := legName(srv, testDataLeg)
			rowDropDm(node, grp)
			tc.arm(t, node, wrapper)

			round := rowRound(t, srv, 2)
			assertRowVerdict(t, round.GetAgentReply(), "the read",
				rowNamed(wrapper, tc.what))
			node.Reset()
			reply := rowResend(t, srv, held, round)
			if reply == nil {
				t.Fatalf("the round that names the wrapper brought no " +
					"re-send")
			}
			if got, want := rowDmChanges(node, wrapper),
				rowStepVerbs[tc.step]; !slices.Equal(got, want) {
				t.Fatalf("the re-send ran %q on the wrapper, want %q",
					got, want)
			}
			if got := rowDmChanges(node, grp); !slices.Equal(got,
				rowStepVerbs[dmStepCreate]) {
				t.Fatalf("the re-send ran %q on the group's device, want a "+
					"create", got)
			}
			assertRowVerdict(t, reply.GetAgentReply(), "the re-send")
			assertRowVerdict(t, rowRound(t, srv, 2).GetAgentReply(),
				"the read after the re-send")
		})
	}
}

// TestTheReadNamesARedundNoneGroupDeviceExactlyWhenAConvergeWouldChangeIt
// holds the read to the converge over the group's device as wanted, absent,
// half its size, at another offset, set read-only, loaded read-only,
// suspended, and set read-only and suspended, each over a leg that serves,
// one that is late, one whose path is connecting, one being disconnected and
// one whose head's number read does not answer, the leg's wrapper as wanted:
// the
// read names the device exactly when the converge after it changes the
// device. A wrapper that is not as wanted is the lowest-layer rule's
// (TestARedundNoneGroupDeviceWaitsForItsWrappersName).
func TestTheReadNamesARedundNoneGroupDeviceExactlyWhenAConvergeWouldChangeIt(
	t *testing.T,
) {
	devices := []struct {
		name string
		arm  func(t *testing.T, node *fakeNode, grp string)
	}{
		{"as wanted", func(*testing.T, *fakeNode, string) {}},
		{"absent", func(t *testing.T, node *fakeNode, grp string) {
			rowDropDm(node, grp)
		}},
		{"another table", rowHalveDm},
		{"another offset", rowShiftDm},
		{"set read-only", rowSetroDm},
		{"loaded read-only", rowLoadedReadOnlyDm},
		{"suspended", rowSuspendDm},
		{"set read-only and suspended", func(t *testing.T, node *fakeNode,
			grp string) {
			rowSetroDm(t, node, grp)
			rowSuspendDm(t, node, grp)
		}},
	}
	legs := []struct {
		name string
		arm  func(t *testing.T, srv *CnAgentServer, node *fakeNode)
	}{
		{"its leg serves", func(*testing.T, *CnAgentServer, *fakeNode) {}},
		{"its leg late", func(t *testing.T, srv *CnAgentServer,
			node *fakeNode) {
			rowLateLeg(srv, node, testDataLeg, "non-optimized")
		}},
		{"its leg's path connecting", func(t *testing.T, srv *CnAgentServer,
			node *fakeNode) {
			node.setCtrlState(legNqn(srv, testDataLeg), testIp, testSvcId,
				"connecting")
		}},
		{"its leg disconnecting", func(t *testing.T, srv *CnAgentServer,
			node *fakeNode) {
			srv.mu.Lock()
			srv.disconnecting[legNqn(srv, testDataLeg)] = struct{}{}
			srv.mu.Unlock()
		}},
		{"its leg head's number unread", func(t *testing.T,
			srv *CnAgentServer, node *fakeNode) {
			nqn := legNqn(srv, testDataLeg)
			node.mu.Lock()
			head := fmt.Sprintf("nvme%dn1", node.subsystems[nqn].idx)
			node.killReadAlways[sysfsBlockDir+"/"+head+"/dev"] = true
			node.mu.Unlock()
		}},
	}
	seen := map[bool]int{}
	t.Cleanup(func() {
		if !t.Failed() && (seen[true] == 0 || seen[false] == 0) {
			t.Errorf("the matrix is vacuous: %d cases named the device, "+
				"%d did not", seen[true], seen[false])
		}
	})
	for _, device := range devices {
		for _, leg := range legs {
			t.Run(device.name+", "+leg.name, func(t *testing.T) {
				srv, node := newTestServer(t)
				srv.retryInterval = time.Hour
				opts := reqOpts{revision: 2, primary: true, leftoverOk: true}
				syncupBoth(t, srv, opts)
				grp := grpName(srv, testDataGrp)
				device.arm(t, node, grp)
				leg.arm(t, srv, node)
				named := strings.Contains(
					rowRound(t, srv, 2).GetAgentReply().GetDetails(),
					"enumeration failed: "+grp+": ")
				node.Reset()
				if _, err := srv.SyncupCntlr(context.Background(),
					cntlrReq(opts)); err != nil {
					t.Fatalf("SyncupCntlr: %v", err)
				}
				changed := len(rowDmChanges(node, grp)) > 0
				seen[named]++
				if named != changed {
					t.Fatalf("the read named the group's device: %v; the "+
						"converge after it changed it: %v (%q)", named,
						changed, rowDmChanges(node, grp))
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// The transfer device (CN17, CN30)
// ---------------------------------------------------------------------------

// TestATransferDeviceRestsOnItsOriginOnlyWhereItIsServed: a transfer device
// rests on its origin's raid0 only where the transfer is served
// (xferServed). A primary at the level that removes the thin pools serves no
// transfer, so its device is an error table and rests on nothing: one whose
// reload fails is named by the converge's reply and by the read, though the
// raid0's row is the level's, and the re-send after the fault reloads it. A
// served transfer over a raid0 that is not there is not named: the raid0
// alone is, and the re-send builds both.
func TestATransferDeviceRestsOnItsOriginOnlyWhereItIsServed(t *testing.T) {
	xfers := []*pb.Transfer{{XferId: testXfer, OriNqn: testNqn, OriNsIdx: 1}}

	t.Run("unserved at the no-thin-pool level", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.retryInterval = time.Hour
		ctx := context.Background()
		opts := func(revision uint64, primary bool) reqOpts {
			return reqOpts{revision: revision, primary: primary,
				xfers: xfers, level: pb.SpLevel_SP_LEVEL_NO_THINPOOL}
		}
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true, xfers: xfers})
		if _, err := srv.SyncupCntlr(ctx,
			cntlrReq(opts(3, false))); err != nil {
			t.Fatalf("demote: %v", err)
		}
		xfer := xferName(srv, testXfer)
		rowHalveDm(t, node, xfer)
		reload := "dmsetup reload " + xfer
		const refusal = "device-mapper: reload ioctl failed"
		node.mu.Lock()
		node.failCmdAlways[reload] = refusal
		node.mu.Unlock()

		promote := cntlrReq(opts(4, true))
		reply, err := srv.SyncupCntlr(ctx, promote)
		if err != nil {
			t.Fatalf("promote: %v", err)
		}
		details := reply.GetAgentReply().GetDetails()
		if !strings.Contains(details, rowNamed(xfer, "differs: ")) ||
			!strings.Contains(details, refusal) {
			t.Fatalf("the promotion whose reload failed replied %v, want it "+
				"to name the transfer device", reply.GetAgentReply())
		}
		assertRowStatus(t,
			reply.GetCntlrInfo().GetTdIdToRaid0()[testTd],
			pb.ResStatus_RES_STATUS_MISSING, "the raid0 the level removes")
		assertRowVerdict(t, rowRound(t, srv, 4).GetAgentReply(), "the read",
			rowNamed(xfer, "differs: table is not the desired error target"))

		node.mu.Lock()
		delete(node.failCmdAlways, reload)
		node.mu.Unlock()
		node.Reset()
		resent := rowResend(t, srv, promote, rowRound(t, srv, 4))
		if resent == nil {
			t.Fatalf("the round that names the transfer device brought no " +
				"re-send")
		}
		if got := rowDmChanges(node, xfer); !slices.Equal(got,
			rowStepVerbs[dmStepReload]) {
			t.Fatalf("the re-send ran %q on the transfer device, want a "+
				"reload", got)
		}
		assertRowVerdict(t, resent.GetAgentReply(), "the re-send")
	})

	t.Run("served over a raid0 that is not there", func(t *testing.T) {
		srv, node := newTestServer(t)
		srv.retryInterval = time.Hour
		opts := reqOpts{revision: 2, primary: true, xfers: xfers}
		syncupBoth(t, srv, opts)
		held := cntlrReq(opts)
		raid0 := raid0Name(srv, testTd)
		xfer := xferName(srv, testXfer)
		rowDropDm(node, raid0)

		round := rowRound(t, srv, 2)
		assertRowVerdict(t, round.GetAgentReply(), "the read",
			rowNamed(raid0, "absent"))
		node.Reset()
		resent := rowResend(t, srv, held, round)
		if resent == nil {
			t.Fatalf("the round that names the raid0 brought no re-send")
		}
		if got := rowDmChanges(node, xfer); !slices.Equal(got,
			rowStepVerbs[dmStepReload]) {
			t.Fatalf("the re-send ran %q on the transfer device, want a "+
				"reload onto the new raid0", got)
		}
		assertRowVerdict(t, resent.GetAgentReply(), "the re-send")
		assertRowVerdict(t, rowRound(t, srv, 2).GetAgentReply(),
			"the read after the re-send")
	})
}

// TestAConvergeMakesASetReadOnlyDeviceWritable: every device a converge
// reloads because it reads read-only is made writable after the reload
// (cnagent.md CN10, A read-only device is made writable) — an ns-dev and a
// pool concat here, beside the single-target devices and the RedundNone
// dm-linears the tests above cover — since a reload leaves the block
// layer's own flag that `blockdev --setro` sets.
func TestAConvergeMakesASetReadOnlyDeviceWritable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		device func(srv *CnAgentServer, opts reqOpts) string
	}{
		{"an ns-dev", func(srv *CnAgentServer, _ reqOpts) string {
			return nsDevName(srv, testNs)
		}},
		{"a pool concat", func(srv *CnAgentServer, opts reqOpts) string {
			return newCntlrPlan(srv.nf, cntlrReq(opts)).
				sliceById[testSlice].poolMetaName
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			opts := reqOpts{revision: 2, primary: true}
			syncupBoth(t, srv, opts)
			name := tc.device(srv, opts)
			rowSetroDm(t, node, name)

			node.Reset()
			reply, err := srv.SyncupCntlr(context.Background(),
				cntlrReq(opts))
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			assertRowVerdict(t, reply.GetAgentReply(), "the converge")
			if got, want := rowDmChanges(node, name),
				rowStepVerbs[dmStepReloadWritable]; !slices.Equal(got,
				want) {
				t.Fatalf("the converge ran %q on %s, want %q", got, name,
					want)
			}
			node.mu.Lock()
			setro := node.dms[name].bdevReadOnly
			node.mu.Unlock()
			if setro {
				t.Fatalf("%s is still read-only after the converge", name)
			}
		})
	}
}

// TestAFailedReadOnlyClearFailsTheDevicesStep: a clear of a device's
// read-only flag that fails after its reload went through fails the
// device's step (cnagent.md CN10, A read-only device is made writable). The
// converge's reply names the device with blockdev's error. The read after
// it names a wrapper or a RedundNone dm-linear "differs: read-only" again,
// and nothing for a pool concat or an ns-dev, whose rows do not read the
// flag; a concat whose clear failed is there and on its table, so the slice
// goes on to its pool. The re-send the converge's round brings clears the
// flag, and the read after it names nothing.
func TestAFailedReadOnlyClearFailsTheDevicesStep(t *testing.T) {
	const refusal = "blockdev: ioctl error on BLKROSET: Permission denied"
	for _, tc := range []struct {
		name   string
		device func(srv *CnAgentServer, opts reqOpts) string
		// readNames says the read after the failed clear names the device.
		readNames bool
	}{
		{"a wrapper", func(srv *CnAgentServer, _ reqOpts) string {
			return legName(srv, testDataLeg)
		}, true},
		{"a RedundNone dm-linear", func(srv *CnAgentServer, _ reqOpts) string {
			return grpName(srv, testDataGrp)
		}, true},
		{"a pool concat", func(srv *CnAgentServer, opts reqOpts) string {
			return newCntlrPlan(srv.nf, cntlrReq(opts)).
				sliceById[testSlice].poolMetaName
		}, false},
		{"an ns-dev", func(srv *CnAgentServer, _ reqOpts) string {
			return nsDevName(srv, testNs)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			srv.retryInterval = time.Hour
			opts := reqOpts{revision: 2, primary: true}
			syncupBoth(t, srv, opts)
			held := cntlrReq(opts)
			name := tc.device(srv, opts)
			rowSetroDm(t, node, name)
			node.mu.Lock()
			node.failCmd["blockdev --setrw /dev/mapper/"+name] = refusal
			node.mu.Unlock()

			reply, err := srv.SyncupCntlr(context.Background(), held)
			if err != nil {
				t.Fatalf("SyncupCntlr: %v", err)
			}
			details := reply.GetAgentReply().GetDetails()
			if !strings.Contains(details, rowNamed(name, "differs: ")) ||
				!strings.Contains(details, refusal) {
				t.Fatalf("the converge whose clear failed replied %v, want "+
					"it to name %s with blockdev's error",
					reply.GetAgentReply(), name)
			}
			if tc.name == "a pool concat" {
				assertOk(t, reply.GetCntlrInfo().
					GetSliceIdToDmPool()[testSlice],
					"the pool over the concat whose clear failed")
			}
			// The round after the converge answers from its observation,
			// which brings the re-send; a read in between reads the node.
			round := rowRoundOnly(t, srv, 2)
			read := rowRound(t, srv, 2).GetAgentReply()
			if tc.readNames {
				assertRowVerdict(t, read, "the read after the failed clear",
					rowNamed(name, "differs: read-only"))
			} else {
				assertRowVerdict(t, read, "the read after the failed clear")
			}

			node.Reset()
			resent := rowResend(t, srv, held, round)
			if resent == nil {
				t.Fatalf("the round after the failed clear brought no " +
					"re-send")
			}
			assertRowVerdict(t, resent.GetAgentReply(), "the re-send")
			node.mu.Lock()
			setro := node.dms[name].bdevReadOnly
			node.mu.Unlock()
			if setro {
				t.Fatalf("%s is still read-only after the re-send", name)
			}
			if got, want := rowDmChanges(node, name),
				rowStepVerbs[dmStepReloadWritable]; !slices.Equal(got,
				want) {
				t.Fatalf("the re-send ran %q on %s, want %q", got, name,
					want)
			}
			assertRowVerdict(t, rowRound(t, srv, 2).GetAgentReply(),
				"the read after the re-send")
		})
	}
}
