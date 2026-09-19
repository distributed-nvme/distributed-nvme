package dnagent

import (
	"context"
	"fmt"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// Several dn agents on one kernel (architecture.md §9.8, "attribution").
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
// These tests exist because both foreign arms were added after the LAB found
// them, and a unit suite in which every object belongs to the agent under
// test cannot fail when they regress — it never presents one. Each case is
// paired with the same fixture built under OUR ids, so no arm can pass by
// sweeping nothing.

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
// half-built shape classifyExport has to settle by port instead.
func seedExport(node *fakeNode, nqn string, devPath string) {
	node.mu.Lock()
	defer node.mu.Unlock()
	dir := agent.NvmetRoot + "/subsystems/" + nqn
	node.dirs[dir] = true
	node.dirs[dir+"/namespaces"] = true
	if devPath == "" {
		return
	}
	node.dirs[dir+"/namespaces/1"] = true
	node.files[dir+"/namespaces/1/device_path"] = devPath + "\n"
	node.files[dir+"/namespaces/1/enable"] = "1\n"
}

// linkPort puts an export on an nvmet port, creating the port if the fake does
// not have it. A sibling agent's port lives in the same configfs tree as ours
// and is told apart only by its id (--nvmet-port-id).
func linkPort(node *fakeNode, portId int, nqn string) {
	node.mu.Lock()
	defer node.mu.Unlock()
	ports := agent.NvmetRoot + "/ports"
	node.dirs[ports] = true
	node.dirs[fmt.Sprintf("%s/%d", ports, portId)] = true
	node.dirs[fmt.Sprintf("%s/%d/subsystems", ports, portId)] = true
	node.links[fmt.Sprintf("%s/%d/subsystems/%s", ports, portId, nqn)] =
		agent.NvmetRoot + "/subsystems/" + nqn
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
		name:    "no namespace, sibling's port",
		portId:  siblingPort,
		keep:    true,
		wantWhy: "a half-built export linked to another agent's port",
	}, {
		name:    "no namespace, our port",
		portId:  common.NvmetPortId,
		keep:    false,
		wantWhy: "a half-built export of ours, exporting nothing",
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
	// Not the destination of any stored side: claims.migrDst must not be
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
