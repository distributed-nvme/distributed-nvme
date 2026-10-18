package dnagent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// Several dn agents on one kernel (architecture.md, Teardown by sweep,
// "attribution").
//
// The dm, nvmet and nvme-host namespaces are per KERNEL, not per agent, so a
// node-level sweep enumerating configfs or /sys/class/nvme-subsystem sees
// every co-hosted agent's objects alongside its own. Two of the three name
// formats it meets there do not carry the id of the agent that built them:
//
//   - a :2: export names (cluster, sp, leg, cn) and no dn at all, because
//     both sides of a migrating leg export the same NQN ([D1]);
//   - a :3: migration-source connection names the SOURCE dn, which is the
//     node being read FROM, never the one that opened the connection.
//
// Attributing either by the ids in its own name therefore reads a sibling's
// object as unowned, and "unowned" is the arm that removes. That is not a
// leak but a live-data outage on somebody else's sp: the export is the path a
// CN is doing IO over, and the connection feeds a hydrating clone.
//
// A unit suite in which every object belongs to the agent under test never
// presents a sibling's object, so neither foreign arm can fail there. Each
// case here is paired with the same fixture built under OUR ids, so no arm
// can pass by sweeping nothing.

const (
	siblingDn   = testDn + 1
	siblingLeg  = testLeg + 0x40
	siblingSide = testSide + 0x40
	siblingPort = common.NvmetPortId + 1
)

// seedExport materialises one :2: export directly in the fake's configfs, the
// way a co-hosted agent's converge would have left it: a subsystem directory,
// one namespace, and that namespace's device_path naming the per-CN dm-linear
// it exports. devPath == "" seeds a subsystem with NO namespace, which is the
// half-built shape classifyExport has to settle by its age, and then by its
// port, instead.
func seedExport(node *fakeNode, nqn string, devPath string) {
	seedExportNs(node, nqn, sideNsid, devPath)
}

// seedExportNs is seedExport with the one namespace at nsid. An id other than
// sideNsid is a shape dnv never builds, but another tool can make: an export
// without namespace one, which classifyExport judges by its age first.
func seedExportNs(node *fakeNode, nqn string, nsid int, devPath string) {
	node.mu.Lock()
	defer node.mu.Unlock()
	dir := agent.NvmetRoot + "/subsystems/" + nqn
	node.dirs[dir] = true
	node.dirs[dir+"/namespaces"] = true
	if devPath == "" {
		return
	}
	ns := fmt.Sprintf("%s/namespaces/%d", dir, nsid)
	node.dirs[ns] = true
	node.files[ns+"/device_path"] = devPath + "\n"
	node.files[ns+"/enable"] = "1\n"
}

// linkPort puts an export on an nvmet port, creating the port if the fake does
// not have it. A sibling agent's port lives in the same configfs tree as ours
// and is told apart only by its id (--nvmet-port-id).
func linkPort(node *fakeNode, portId int, nqn string) {
	addPort(node, portId)
	node.mu.Lock()
	defer node.mu.Unlock()
	node.links[fmt.Sprintf("%s/subsystems/%s", portDir(portId), nqn)] =
		agent.NvmetRoot + "/subsystems/" + nqn
}

// addPort creates an nvmet port with nothing linked to it yet, as a sibling
// agent's converge leaves its port before its first export.
func addPort(node *fakeNode, portId int) {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.dirs[agent.NvmetRoot+"/ports"] = true
	node.dirs[portDir(portId)] = true
	node.dirs[portDir(portId)+"/subsystems"] = true
}

func portDir(portId int) string {
	return fmt.Sprintf("%s/ports/%d", agent.NvmetRoot, portId)
}

// lsCount is how many `ls` of exactly dir the calls since the last Reset
// ran. The port listing starts with an `ls` of the ports directory, which
// nothing else lists.
func lsCount(node *fakeNode, dir string) int {
	n := 0
	for _, call := range node.Calls() {
		if call == "cmd ls -1 "+dir {
			n++
		}
	}
	return n
}

// ageReads is how many reads of exactly dir's mtime, in the agent's own
// process, the calls since the last Reset made.
func ageReads(node *fakeNode, dir string) int {
	n := 0
	for _, call := range node.Calls() {
		if call == "mtime "+dir {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// A :2: export of a sibling agent
// ---------------------------------------------------------------------------

// TestSiblingExportNotSweptByOurAgent runs the node-level sweep over a
// configfs tree holding one export the agent did not build, and pins which of
// the four shapes it is allowed to remove.
//
// The fixture always converges a side of OUR own under testSp first. That is
// load-bearing twice over: it is what puts testSp in the sweep's `hosted` set
// — the cheap gate that stops an agent reading every co-hosted agent's
// namespaces on every pass — so without it the sibling's export would be
// skipped before classifyExport ever ran and the foreign arms would be
// untested; and it is what makes the two control arms reachable.
func TestSiblingExportNotSweptByOurAgent(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	// One leg of ours, and a second leg nothing in the desired state names.
	// The seeded export is always on the second: a :2: NQN is per (sp, leg,
	// cn), so putting it on ours would name an object our own side wants.
	strayNqn := nf.SideToCnNqn(testCluster, testSp, siblingLeg, testCn0)

	cases := []struct {
		name    string
		devPath string
		portId  int
		// old backdates the export past DnExportOrphanGrace. Every export a
		// fixture seeds is young otherwise, and a young namespace-less one
		// is left alone whoever built it (TestHalfBuiltExportAgeGate).
		old     bool
		wantWhy string
		keep    bool
	}{{
		name: "sibling dn's linear",
		devPath: "/dev/mapper/" + nf.DnLinearName(
			testCluster, siblingDn, testSp, siblingSide, testCn0),
		keep: true,
		wantWhy: "its namespace backs onto a d1 of dn " +
			"0x4, so the export is that agent's",
	}, {
		name: "our linear, side long gone",
		devPath: "/dev/mapper/" + nf.DnLinearName(
			testCluster, testDn, testSp, siblingSide, testCn0),
		keep: false,
		wantWhy: "its namespace backs onto a d1 of OURS whose side is in " +
			"no pointer list",
	}, {
		name:   "no namespace, sibling's port",
		portId: siblingPort,
		keep:   true,
		wantWhy: "a namespace-less export this young may be a build in " +
			"flight, whatever port it is linked to",
	}, {
		name:   "no namespace, sibling's port, older than the grace",
		portId: siblingPort,
		old:    true,
		keep:   true,
		wantWhy: "an old namespace-less export linked to another agent's " +
			"port is that agent's",
	}, {
		name:   "no namespace, our port, older than the grace",
		portId: common.NvmetPortId,
		old:    true,
		keep:   false,
		wantWhy: "a half-built export on our port that nobody has touched " +
			"for longer than the grace, exporting nothing",
	}, {
		name:   "no namespace, our port, younger than the grace",
		portId: common.NvmetPortId,
		keep:   true,
		wantWhy: "a namespace-less export this young may be a build in " +
			"flight, and nothing but its age can say it is not",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			ours := sweepSidePtr(testLeg, testSide)
			if _, err := srv.SyncupDn(ctx, sweepDnReq(1, ours)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			syncupSideTwoPhase(t, srv, sweepSideReq(1, ours))
			ourObjs := objectsOf(ours)
			ourObjs.assertAllPresent(t, node)

			seedExport(node, strayNqn, tc.devPath)
			if tc.portId != 0 {
				linkPort(node, tc.portId, strayNqn)
			}
			if tc.old {
				node.ageDir(agent.NvmetRoot+"/subsystems/"+strayNqn,
					common.DnExportOrphanGrace+time.Minute)
			}
			if !subsysPresent(node, strayNqn) {
				t.Fatalf("fixture is wrong: %s was never seeded", strayNqn)
			}

			node.Reset()
			reply, err := srv.SyncupDn(ctx, sweepDnReq(2, ours))
			if err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			if got := reply.GetAgentReply().GetCode(); got != 0 {
				t.Fatalf("code = %d (%s), want 0",
					got, reply.GetAgentReply().GetDetails())
			}

			switch present := subsysPresent(node, strayNqn); {
			case tc.keep && !present:
				t.Errorf("the sweep removed %s: %s", strayNqn, tc.wantWhy)
			case !tc.keep && present:
				t.Errorf("the sweep kept %s: %s", strayNqn, tc.wantWhy)
			}
			if tc.keep && node.hasCall("cmd rmdir "+
				agent.NvmetRoot+"/subsystems/"+strayNqn) {
				t.Errorf("the sweep attempted rmdir on %s", strayNqn)
			}
			// Whatever it did to the stray, our own side is untouched: a
			// pass that swept the whole node would satisfy the arm above
			// by accident.
			ourObjs.assertAllKept(t, node)
		})
	}
}

// ---------------------------------------------------------------------------
// A :3: connection of a sibling agent
// ---------------------------------------------------------------------------

// TestSiblingMigrConnNotDisconnected is the same property one layer down, on
// the only object whose name actively points at the WRONG node: a
// MigrSrcNqn's dn field is the source's, so every co-hosted agent hydrating
// from the same source holds a connection to an NQN that names neither of
// them. The host NQN the controller was opened with is the only field that
// says whose it is.
//
// The stakes are higher than for an export. Disconnecting a sibling's source
// stalls a hydration that has already started, and dm-clone answers a read of
// an unhydrated region from the source it can no longer reach.
func TestSiblingMigrConnNotDisconnected(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	// Not the destination of any held side: claims.migrDst must not be
	// what keeps it, or the host-NQN arm is again untested.
	strayConn := nf.MigrSrcNqn(testCluster, testSrcDn, testSp, testMigrId+0x40)

	cases := []struct {
		name    string
		hostNqn string
		keep    bool
	}{{
		name:    "opened by a sibling agent",
		hostNqn: nf.DnHostNqn(testCluster, siblingDn),
		keep:    true,
	}, {
		name:    "opened by us",
		hostNqn: nf.DnHostNqn(testCluster, testDn),
		keep:    false,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			ours := sweepSidePtr(testLeg, testSide)
			if _, err := srv.SyncupDn(ctx, sweepDnReq(1, ours)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			syncupSideTwoPhase(t, srv, sweepSideReq(1, ours))

			node.mu.Lock()
			node.nvmeConnect([]string{
				"--nqn", strayConn, "--hostnqn", tc.hostNqn,
				"--transport", "tcp",
				"--traddr", "192.168.0.20", "--trsvcid", "4200",
			})
			node.mu.Unlock()
			if !connPresent(node, strayConn) {
				t.Fatalf("fixture is wrong: %s was never connected", strayConn)
			}

			node.Reset()
			reply, err := srv.SyncupDn(ctx, sweepDnReq(2, ours))
			if err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			if got := reply.GetAgentReply().GetCode(); got != 0 {
				t.Fatalf("code = %d (%s), want 0",
					got, reply.GetAgentReply().GetDetails())
			}

			switch present := connPresent(node, strayConn); {
			case tc.keep && !present:
				t.Errorf("the sweep disconnected %s, which was opened with "+
					"%s and is not ours", strayConn, tc.hostNqn)
			case !tc.keep && present:
				t.Errorf("the sweep kept %s, which we opened ourselves and "+
					"no stored side claims", strayConn)
			}
			if tc.keep &&
				node.hasCall("cmd nvme disconnect --nqn "+strayConn) {
				t.Errorf("the sweep attempted to disconnect %s", strayConn)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// A half-built export: the age gate
// ---------------------------------------------------------------------------

// linkAllowedHost gives a seeded export its allowed-host link, the step a
// build takes between the subsystem's attributes and its namespace: the
// incident's shape, a half-built export that already carries the CN's host
// link.
func linkAllowedHost(node *fakeNode, nqn, hostNqn string) string {
	node.mu.Lock()
	defer node.mu.Unlock()
	host := agent.NvmetRoot + "/hosts/" + hostNqn
	node.dirs[agent.NvmetRoot+"/hosts"] = true
	node.dirs[host] = true
	dir := agent.NvmetRoot + "/subsystems/" + nqn
	node.dirs[dir+"/allowed_hosts"] = true
	link := dir + "/allowed_hosts/" + hostNqn
	node.links[link] = host
	return link
}

func linkPresent(node *fakeNode, link string) bool {
	node.mu.Lock()
	defer node.mu.Unlock()
	_, ok := node.links[link]
	return ok
}

// sweepScope is one of the two sweeps DN6 runs, driven the way the worker
// drives it: the node-level one by a SyncupDn, the side-level one by a
// SyncupSide of the side the fixture converged. read is the same scope's
// read-only verdict: the background read's (SH29), which the Get*Info (and
// Check round) answer then carries.
type sweepScope struct {
	name string
	// stray names the export the arms seed. The side-level scope judges its
	// own leg's exports only (the own-leg rule), so its stray is an export of
	// OUR leg to a
	// cn this side does not export to; the node-level scope's is a sibling
	// leg's, in an sp this node hosts.
	stray func(nf *common.NameFmt) string
	// second names another export the scope judges, of the same kind as
	// stray, for a pass that must judge two.
	second func(nf *common.NameFmt) string
	run    func(t *testing.T, srv *DnAgentServer, rev uint64) *pb.AgentReply
	read   func(t *testing.T, srv *DnAgentServer) *pb.AgentReply
}

func sweepScopes() []sweepScope {
	ours := sweepSidePtr(testLeg, testSide)
	return []sweepScope{{
		name: "node-level",
		stray: func(nf *common.NameFmt) string {
			return nf.SideToCnNqn(testCluster, testSp, siblingLeg, testCn0)
		},
		second: func(nf *common.NameFmt) string {
			return nf.SideToCnNqn(testCluster, testSp, siblingLeg+1, testCn0)
		},
		run: func(t *testing.T, srv *DnAgentServer,
			rev uint64) *pb.AgentReply {
			t.Helper()
			reply, err := srv.SyncupDn(context.Background(),
				sweepDnReq(rev, ours))
			if err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			return reply.GetAgentReply()
		},
		read: func(t *testing.T, srv *DnAgentServer) *pb.AgentReply {
			t.Helper()
			reply, err := dnInfoNow(srv, context.Background(),
				&pb.GetDnInfoRequest{ClusterId: testCluster, DnId: testDn})
			if err != nil {
				t.Fatalf("GetDnInfo: %v", err)
			}
			return reply.GetAgentReply()
		},
	}, {
		name: "side-level",
		stray: func(nf *common.NameFmt) string {
			return nf.SideToCnNqn(testCluster, testSp, testLeg, testCn1+0x10)
		},
		second: func(nf *common.NameFmt) string {
			return nf.SideToCnNqn(testCluster, testSp, testLeg, testCn1+0x11)
		},
		run: func(t *testing.T, srv *DnAgentServer,
			rev uint64) *pb.AgentReply {
			t.Helper()
			reply, err := srv.SyncupSide(context.Background(),
				sweepSideReq(rev, ours))
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			return reply.GetAgentReply()
		},
		read: func(t *testing.T, srv *DnAgentServer) *pb.AgentReply {
			t.Helper()
			reply, err := sideInfoNow(srv, context.Background(),
				&pb.GetSideInfoRequest{ClusterId: testCluster, DnId: testDn,
					SidePointer: ours})
			if err != nil {
				t.Fatalf("GetSideInfo: %v", err)
			}
			return reply.GetAgentReply()
		},
	}}
}

// TestHalfBuiltExportAgeGate pins DN6's age gate in both scopes. A :2: export
// with no namespace, linked to no port, is every export's shape between its
// subsystem mkdir and its namespace mkdir, and for the later part of that
// window it already carries the CN's host link. On a kernel several dn agents
// share, the build may be a sibling's, which no claim of ours can show: a
// leg would be lost that way — a sibling's sweep would strip the export's
// host link and namespace — and a
// broken dn export is rebuilt only by some later converge of its side, which
// the breakage never triggers. So such an export goes only once its directory
// is older than DnExportOrphanGrace; a younger one is left alone — not
// removed, not a leftover, not a failure — in the Syncup's pass and in the
// read-only verdict alike. The old one is the orphan rule unchanged: nobody
// is building it, it is reported by the read-only verdict (so the Check
// rounds re-drive it), and the next Syncup removes it, host link and all.
// Each pass's verdict rests on its own read of the age, one read in the
// agent's own process and no `stat` child, never on an age remembered from
// an earlier pass.
func TestHalfBuiltExportAgeGate(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	hostNqn := nf.CnHostNqn(testCluster, testCn0)
	for _, scope := range sweepScopes() {
		for _, tc := range []struct {
			name     string
			old      bool
			hostLink bool
		}{
			{"young", false, false},
			{"young, allowed host linked", false, true},
			{"old", true, false},
			{"old, allowed host linked", true, true},
		} {
			t.Run(scope.name+"/"+tc.name, func(t *testing.T) {
				srv, node := newTestServer(t)
				ctx := context.Background()
				ours := sweepSidePtr(testLeg, testSide)
				if _, err := srv.SyncupDn(ctx,
					sweepDnReq(1, ours)); err != nil {
					t.Fatalf("SyncupDn: %v", err)
				}
				syncupSideTwoPhase(t, srv, sweepSideReq(1, ours))
				ourObjs := objectsOf(ours)
				ourObjs.assertAllPresent(t, node)

				stray := scope.stray(nf)
				dir := agent.NvmetRoot + "/subsystems/" + stray
				seedExport(node, stray, "")
				var link string
				if tc.hostLink {
					link = linkAllowedHost(node, stray, hostNqn)
				}
				if tc.old {
					node.ageDir(dir, common.DnExportOrphanGrace+time.Minute)
				}
				if !subsysPresent(node, stray) {
					t.Fatalf("fixture is wrong: %s was never seeded", stray)
				}

				// aged checks that one pass read the export's age exactly
				// once, in the agent's own process, and ran no `stat` child.
				aged := func(pass string) {
					t.Helper()
					if got := ageReads(node, dir); got != 1 {
						t.Errorf("the %s read the age of %s %d times in "+
							"process, want 1", pass, stray, got)
					}
					if got := len(node.callsMatching(
						"cmd stat ")); got != 0 {
						t.Errorf("the %s ran %d `stat` children, want 0",
							pass, got)
					}
				}

				// The read-only verdict first: an old one is a leftover the
				// Check rounds report (and so re-drive), a young one is not.
				node.Reset()
				verdict := scope.read(t, srv)
				switch {
				case tc.old && (verdict.GetCode() != common.ReplyCodeLeftover ||
					!strings.Contains(verdict.GetDetails(), stray)):
					t.Fatalf("read-only verdict %d %q, want %d naming %s",
						verdict.GetCode(), verdict.GetDetails(),
						common.ReplyCodeLeftover, stray)
				case !tc.old && verdict.GetCode() != 0:
					t.Fatalf("read-only verdict %d %q, want 0: a young "+
						"half-built export is not a leftover",
						verdict.GetCode(), verdict.GetDetails())
				}
				aged("read-only verdict")

				node.Reset()
				reply := scope.run(t, srv, 2)
				if got := reply.GetCode(); got != 0 {
					t.Fatalf("code = %d (%s), want 0", got,
						reply.GetDetails())
				}
				present := subsysPresent(node, stray)
				switch {
				case tc.old && present:
					t.Fatalf("the sweep kept %s, which nobody has touched "+
						"for longer than the grace", stray)
				case !tc.old && !present:
					t.Fatalf("the sweep removed %s, a namespace-less "+
						"export younger than the grace", stray)
				}
				aged("Syncup's pass")
				if !tc.old {
					if node.hasCall("cmd rmdir " + dir) {
						t.Fatalf("the sweep attempted rmdir on %s", stray)
					}
					if tc.hostLink && !linkPresent(node, link) {
						t.Fatalf("the sweep stripped the young export's "+
							"allowed-host link %s", link)
					}
				}
				if tc.old && tc.hostLink && linkPresent(node, link) {
					t.Fatalf("the old export's host link %s survived", link)
				}
				// Whatever it did to the stray, our own side is untouched.
				ourObjs.assertAllKept(t, node)
			})
		}
	}
}

// TestHalfBuiltExportAgeBeforePortLinks pins the order of DN6's attribution of
// a :2: export without namespace one, in both scopes and in the read-only
// verdict as in the Syncup's pass: the export's age is read first, in the
// agent's own process, then, only for an old export, its namespaces are listed
// and, when it has none at all, its port links. A young one is left alone
// whatever it holds and whatever port it is linked to, so its pass lists
// nothing of it at all — a namespace at an id other than one included, which
// decides it only once it is old. An old one with no namespace is that
// sibling's when it is on a sibling's port, and an orphan otherwise. The
// namespace listing is a child per export and the port listing an `ls` of the
// ports directory and one of each port's links, a child each, and every build
// or removal in flight on a kernel many dn agents share has the shape for a
// moment: listed before the age, either would make such an agent's reads
// (SH29) run long in every burst. A sibling's port is on the kernel in every
// case, so a listing would show.
func TestHalfBuiltExportAgeBeforePortLinks(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ports := agent.NvmetRoot + "/ports"
	siblingLinks := portDir(siblingPort) + "/subsystems"
	// A namespace at another id names a sibling dn's linear: once the
	// export is old, that namespace makes it the sibling's.
	siblingLinear := "/dev/mapper/" + nf.DnLinearName(
		testCluster, siblingDn, testSp, siblingSide, testCn0)
	for _, scope := range sweepScopes() {
		for _, tc := range []struct {
			name   string
			old    bool
			portId int // 0: linked to no port
			// nsAt seeds one namespace at that id, naming a sibling dn's
			// linear; 0 seeds none.
			nsAt int
			// gone: the read-only verdict reports it as a leftover and the
			// Syncup removes it.
			gone bool
		}{
			{"young, no port", false, 0, 0, false},
			{"young, our port", false, common.NvmetPortId, 0, false},
			{"young, sibling's port", false, siblingPort, 0, false},
			{"young, namespace at another id", false, 0, 2, false},
			{"old, no port", true, 0, 0, true},
			{"old, our port", true, common.NvmetPortId, 0, true},
			{"old, sibling's port", true, siblingPort, 0, false},
			{"old, namespace at another id", true, 0, 2, false},
		} {
			t.Run(scope.name+"/"+tc.name, func(t *testing.T) {
				srv, node := newTestServer(t)
				ctx := context.Background()
				ours := sweepSidePtr(testLeg, testSide)
				if _, err := srv.SyncupDn(ctx,
					sweepDnReq(1, ours)); err != nil {
					t.Fatalf("SyncupDn: %v", err)
				}
				syncupSideTwoPhase(t, srv, sweepSideReq(1, ours))
				ourObjs := objectsOf(ours)

				stray := scope.stray(nf)
				dir := agent.NvmetRoot + "/subsystems/" + stray
				if tc.nsAt != 0 {
					seedExportNs(node, stray, tc.nsAt, siblingLinear)
				} else {
					seedExport(node, stray, "")
				}
				addPort(node, siblingPort)
				if tc.portId != 0 {
					linkPort(node, tc.portId, stray)
				}
				if tc.old {
					node.ageDir(dir, common.DnExportOrphanGrace+time.Minute)
				}

				// listed checks one pass's reads of the export: its age
				// once, in process; its namespaces once when it is old and
				// not at all when it is young, and once more in a pass that
				// removes it, whose removal walks them; and the port links
				// once when it is old with no namespace, and not at all
				// otherwise.
				listed := func(pass string, removes bool) {
					t.Helper()
					if got := ageReads(node, dir); got != 1 {
						t.Errorf("the %s read the age of %s %d times in "+
							"process, want 1", pass, stray, got)
					}
					if got := len(node.callsMatching(
						"cmd stat ")); got != 0 {
						t.Errorf("the %s ran %d `stat` children, want 0",
							pass, got)
					}
					wantNs, wantPorts := 0, 0
					if tc.old {
						wantNs = 1
						if tc.nsAt == 0 {
							wantPorts = 1
						}
					}
					if removes {
						wantNs++
					}
					if got := lsCount(node, dir+"/namespaces"); got != wantNs {
						t.Errorf("the %s listed the namespaces of %s %d "+
							"times, want %d", pass, stray, got, wantNs)
					}
					if got := lsCount(node, ports); got != wantPorts {
						t.Errorf("the %s listed the ports directory %d "+
							"times, want %d", pass, got, wantPorts)
					}
					if got := lsCount(node, siblingLinks); got != wantPorts {
						t.Errorf("the %s listed the sibling port's links "+
							"%d times, want %d", pass, got, wantPorts)
					}
				}

				node.Reset()
				verdict := scope.read(t, srv)
				switch {
				case tc.gone && (verdict.GetCode() !=
					common.ReplyCodeLeftover ||
					!strings.Contains(verdict.GetDetails(), stray)):
					t.Fatalf("read-only verdict %d %q, want %d naming %s",
						verdict.GetCode(), verdict.GetDetails(),
						common.ReplyCodeLeftover, stray)
				case !tc.gone && verdict.GetCode() != 0:
					t.Fatalf("read-only verdict %d %q, want 0: %s is no "+
						"leftover", verdict.GetCode(), verdict.GetDetails(),
						stray)
				}
				listed("read-only verdict", false)

				node.Reset()
				reply := scope.run(t, srv, 2)
				if got := reply.GetCode(); got != 0 {
					t.Fatalf("code = %d (%s), want 0", got,
						reply.GetDetails())
				}
				switch present := subsysPresent(node, stray); {
				case tc.gone && present:
					t.Fatalf("the sweep kept %s, an old namespace-less "+
						"export on no port but ours", stray)
				case !tc.gone && !present:
					t.Fatalf("the sweep removed %s", stray)
				case !tc.gone && node.hasCall("cmd rmdir "+dir):
					t.Fatalf("the sweep attempted rmdir on %s", stray)
				}
				listed("Syncup's pass", tc.gone)
				ourObjs.assertAllKept(t, node)
			})
		}
	}
}

// TestPortLinksListedOncePerPass pins the port listing's cache: a pass that
// meets two old namespace-less exports reads the age of each once, in
// process, and lists the ports once, not once per export, in both scopes and
// in the read-only verdict as in the Syncup's pass. Each pass builds the map
// afresh; nothing of it is kept for the next.
func TestPortLinksListedOncePerPass(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ports := agent.NvmetRoot + "/ports"
	siblingLinks := portDir(siblingPort) + "/subsystems"
	for _, scope := range sweepScopes() {
		t.Run(scope.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			ours := sweepSidePtr(testLeg, testSide)
			if _, err := srv.SyncupDn(ctx, sweepDnReq(1, ours)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			syncupSideTwoPhase(t, srv, sweepSideReq(1, ours))

			// One orphan, on no port, and one on a sibling's port, kept.
			orphan, siblings := scope.stray(nf), scope.second(nf)
			for _, nqn := range []string{orphan, siblings} {
				seedExport(node, nqn, "")
				node.ageDir(agent.NvmetRoot+"/subsystems/"+nqn,
					common.DnExportOrphanGrace+time.Minute)
			}
			linkPort(node, siblingPort, siblings)

			once := func(pass string) {
				t.Helper()
				for _, nqn := range []string{orphan, siblings} {
					if got := ageReads(node,
						agent.NvmetRoot+"/subsystems/"+nqn); got != 1 {
						t.Errorf("the %s read the age of %s %d times in "+
							"process, want 1", pass, nqn, got)
					}
				}
				if got := lsCount(node, ports); got != 1 {
					t.Errorf("the %s listed the ports directory %d times, "+
						"want 1", pass, got)
				}
				if got := lsCount(node, siblingLinks); got != 1 {
					t.Errorf("the %s listed the sibling port's links %d "+
						"times, want 1", pass, got)
				}
			}

			node.Reset()
			verdict := scope.read(t, srv)
			if verdict.GetCode() != common.ReplyCodeLeftover ||
				!strings.Contains(verdict.GetDetails(), orphan) ||
				strings.Contains(verdict.GetDetails(), siblings) {
				t.Fatalf("read-only verdict %d %q, want %d naming %s "+
					"and not %s", verdict.GetCode(), verdict.GetDetails(),
					common.ReplyCodeLeftover, orphan, siblings)
			}
			once("read-only verdict")

			node.Reset()
			reply := scope.run(t, srv, 2)
			if got := reply.GetCode(); got != 0 {
				t.Fatalf("code = %d (%s), want 0", got, reply.GetDetails())
			}
			if subsysPresent(node, orphan) {
				t.Fatalf("the sweep kept the orphan %s", orphan)
			}
			if !subsysPresent(node, siblings) {
				t.Fatalf("the sweep removed %s, on a sibling's port", siblings)
			}
			once("Syncup's pass")
		})
	}
}

// TestHalfBuiltExportAgeUnreadIsForeign pins what an age read that does not
// answer means: nothing. The age is the first evidence read of a :2: export
// without namespace one, and the orphan verdict rests on it, so an export
// whose age cannot be read this pass — the read cut off at the soft timeout,
// or failed with an error other than "not exist" — is FOREIGN for the pass,
// like every other failed read of classifyExport: not removed, nothing of it
// listed, and the enumeration named as failed, so the verdict is not clean
// and the worker re-drives. That holds for a young export on a sibling's port
// too, which the port links alone would settle: the age comes first. A
// sibling's build never has that shape, since it links its port only after
// its namespace; a removal of an export two agents of one kernel link leaves
// it, and so can a configfs change made outside dnv. The next pass reads it
// again.
func TestHalfBuiltExportAgeUnreadIsForeign(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	for _, scope := range sweepScopes() {
		for _, tc := range []struct {
			name   string
			old    bool
			portId int // 0: linked to no port
			// failed: the read answers an I/O error; otherwise the soft
			// timeout cuts it off.
			failed bool
			// gone: the next pass, with the age answering, removes it.
			gone bool
		}{
			{"old, no port, cut off", true, 0, false, true},
			{"old, no port, I/O error", true, 0, true, true},
			{"young, sibling's port, cut off", false, siblingPort, false,
				false},
		} {
			t.Run(scope.name+"/"+tc.name, func(t *testing.T) {
				srv, node := newTestServer(t)
				ctx := context.Background()
				ours := sweepSidePtr(testLeg, testSide)
				if _, err := srv.SyncupDn(ctx,
					sweepDnReq(1, ours)); err != nil {
					t.Fatalf("SyncupDn: %v", err)
				}
				syncupSideTwoPhase(t, srv, sweepSideReq(1, ours))
				stray := scope.stray(nf)
				dir := agent.NvmetRoot + "/subsystems/" + stray
				seedExport(node, stray, "")
				addPort(node, siblingPort)
				if tc.portId != 0 {
					linkPort(node, tc.portId, stray)
				}
				if tc.old {
					node.ageDir(dir, common.DnExportOrphanGrace+time.Minute)
				}
				hook := "mtime " + dir
				if tc.failed {
					setFailAlways(node, hook, "")
				} else {
					setHook(node, node.killCmdNoEffectAlways, hook)
				}

				node.Reset()
				reply := scope.run(t, srv, 2)
				if reply.GetCode() != common.ReplyCodeLeftover ||
					!strings.Contains(reply.GetDetails(), "age of "+stray) {
					t.Fatalf("code %d %q, want %d naming the unread age "+
						"of %s", reply.GetCode(), reply.GetDetails(),
						common.ReplyCodeLeftover, stray)
				}
				if !subsysPresent(node, stray) {
					t.Fatalf("the sweep removed %s although its age did "+
						"not answer", stray)
				}
				if got := ageReads(node, dir); got != 1 {
					t.Fatalf("the pass read the age of %s %d times, want 1",
						stray, got)
				}
				if got := lsCount(node, dir+"/namespaces"); got != 0 {
					t.Fatalf("the pass listed the namespaces %d times for "+
						"an export whose age did not answer, want 0", got)
				}
				if got := lsCount(node,
					agent.NvmetRoot+"/ports"); got != 0 {
					t.Fatalf("the pass listed the ports %d times for an "+
						"export whose age did not answer, want 0", got)
				}

				if tc.failed {
					clearFailAlways(node, hook)
				} else {
					clearHook(node, node.killCmdNoEffectAlways, hook)
				}
				reply = scope.run(t, srv, 3)
				if got := reply.GetCode(); got != 0 {
					t.Fatalf("the next pass: code = %d (%s), want 0", got,
						reply.GetDetails())
				}
				if present := subsysPresent(node, stray); present == tc.gone {
					t.Fatalf("the next pass, with the age answering: %s "+
						"present = %v, want %v", stray, present, !tc.gone)
				}
			})
		}
	}
}

// TestGoneExportIsPassedOver pins the other half of the age read's split: an
// export removed between the subsystems listing and its reads — every
// removal by a sibling agent in a burst can leave one — is passed over,
// in both scopes and in the read-only verdict as in the Syncup's pass. Its
// age read answers "not exist", which is no failure: the verdict stays clean,
// and nothing of it is listed, so a gone export costs a pass no child process.
func TestGoneExportIsPassedOver(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	for _, scope := range sweepScopes() {
		t.Run(scope.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			ours := sweepSidePtr(testLeg, testSide)
			if _, err := srv.SyncupDn(ctx, sweepDnReq(1, ours)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			syncupSideTwoPhase(t, srv, sweepSideReq(1, ours))
			stray := scope.stray(nf)
			dir := agent.NvmetRoot + "/subsystems/" + stray
			addPort(node, siblingPort)
			node.mu.Lock()
			node.subsysGhosts = []string{stray}
			node.mu.Unlock()

			passedOver := func(pass string, reply *pb.AgentReply) {
				t.Helper()
				if reply.GetCode() != 0 ||
					strings.Contains(reply.GetDetails(), stray) {
					t.Errorf("the %s: code %d %q, want 0 naming nothing "+
						"of %s", pass, reply.GetCode(), reply.GetDetails(),
						stray)
				}
				if got := ageReads(node, dir); got != 1 {
					t.Errorf("the %s read the age of %s %d times in "+
						"process, want 1", pass, stray, got)
				}
				if got := lsCount(node, dir+"/namespaces"); got != 0 {
					t.Errorf("the %s listed the namespaces of the gone %s "+
						"%d times, want 0", pass, stray, got)
				}
				if got := lsCount(node,
					agent.NvmetRoot+"/ports"); got != 0 {
					t.Errorf("the %s listed the ports %d times for a gone "+
						"export, want 0", pass, got)
				}
				if node.hasCall("cmd rmdir " + dir) {
					t.Errorf("the %s tried to remove the gone %s", pass,
						stray)
				}
			}

			node.Reset()
			passedOver("read-only verdict", scope.read(t, srv))
			node.Reset()
			passedOver("Syncup's pass", scope.run(t, srv, 2))
		})
	}
}

// ---------------------------------------------------------------------------
// The own-leg rule: the side-level scope judges its own leg only
// ---------------------------------------------------------------------------

// TestSideScopeJudgesItsOwnLegOnly pins the own-leg rule. The side-level sweep
// runs under the node READ lock, beside every other side's converge on this
// kernel, so it judges its own leg's exports only: a sweep that judged the
// whole sp would strip a sibling agent's half-built export of another leg of
// its host link and namespace.
// Another leg's export is skipped by its NQN alone, before any read, in
// every shape — even the old orphan the node-level pass WOULD remove, which
// the second half shows: removing it is that pass's business, and it does.
func TestSideScopeJudgesItsOwnLegOnly(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	otherLeg := nf.SideToCnNqn(testCluster, testSp, siblingLeg, testCn0)
	dir := agent.NvmetRoot + "/subsystems/" + otherLeg
	for _, tc := range []struct {
		name   string
		seed   func(node *fakeNode)
		dnGoes bool // the node-level pass that follows removes it
	}{
		{"old orphan, no port", func(node *fakeNode) {
			seedExport(node, otherLeg, "")
			node.ageDir(dir, common.DnExportOrphanGrace+time.Minute)
		}, true},
		{"old orphan, our port", func(node *fakeNode) {
			seedExport(node, otherLeg, "")
			linkPort(node, common.NvmetPortId, otherLeg)
			node.ageDir(dir, common.DnExportOrphanGrace+time.Minute)
		}, true},
		{"young orphan", func(node *fakeNode) {
			seedExport(node, otherLeg, "")
		}, false},
		{"our linear, side long gone", func(node *fakeNode) {
			seedExport(node, otherLeg, "/dev/mapper/"+nf.DnLinearName(
				testCluster, testDn, testSp, siblingSide, testCn0))
		}, true},
		{"sibling dn's linear", func(node *fakeNode) {
			seedExport(node, otherLeg, "/dev/mapper/"+nf.DnLinearName(
				testCluster, siblingDn, testSp, siblingSide, testCn0))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			ctx := context.Background()
			ours := sweepSidePtr(testLeg, testSide)
			if _, err := srv.SyncupDn(ctx, sweepDnReq(1, ours)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			syncupSideTwoPhase(t, srv, sweepSideReq(1, ours))
			tc.seed(node)

			node.Reset()
			reply, err := srv.SyncupSide(ctx, sweepSideReq(2, ours))
			if err != nil {
				t.Fatalf("SyncupSide: %v", err)
			}
			if got := reply.GetAgentReply().GetCode(); got != 0 {
				t.Fatalf("code = %d (%s), want 0", got,
					reply.GetAgentReply().GetDetails())
			}
			if !subsysPresent(node, otherLeg) {
				t.Fatalf("the side-level sweep of leg %#x removed leg %#x's "+
					"export %s", testLeg, siblingLeg, otherLeg)
			}
			// Skipped by its NQN, before any read: not one call names it.
			for _, call := range node.Calls() {
				if strings.Contains(call, dir) {
					t.Fatalf("the side-level sweep of leg %#x touched "+
						"another leg's export: %s", testLeg, call)
				}
			}

			reply2, err := srv.SyncupDn(ctx, sweepDnReq(2, ours))
			if err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			if got := reply2.GetAgentReply().GetCode(); got != 0 {
				t.Fatalf("SyncupDn code = %d (%s), want 0", got,
					reply2.GetAgentReply().GetDetails())
			}
			if gone := !subsysPresent(node, otherLeg); gone != tc.dnGoes {
				t.Fatalf("after the node-level pass %s present = %v, want %v",
					otherLeg, !gone, !tc.dnGoes)
			}
		})
	}
}
