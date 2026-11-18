package cnagent

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// foreignNqn is the operator-made host-facing subsystem of these tests: no
// held request names it.
const foreignNqn = "nqn.2024-01.example:operator-export"

// The node-level sweep removes an unowned host-facing subsystem, the residue
// of a teardown cut off between its rmdirs, of a deletion whose namespaces
// went first, or of a build cut off before its model write; a subsystem dnv
// did not make is left alone (CN21). dnv exports a host namespace only over
// an ns-dev, so a namespace over any other device makes the subsystem
// foreign. A subsystem with no namespace device has its hosts and port
// links listed first. With a host or a port link its model alone decides,
// at any age — dnv's makes it unowned, any other foreign, and a model read
// that did not answer counts as not dnv's, named as a failure. With
// neither, its age decides and no attribute file of it is read: younger
// than the grace it is left alone, as a build in flight may be; older, it
// exports nothing and is removed whatever its model. A namespace whose
// device was never written counts as none.
func TestTheNodeSweepLeavesAForeignSubsystemAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		device string
		model  string
		// young plants the subsystem's directory with the clock's time.
		young bool
		// hostLink and portLink link a host and the cn's port.
		hostLink, portLink bool
		// killModel makes the model read not answer; killAge the age read.
		killModel, killAge bool
		removed            bool
	}{
		{name: "a namespace over another block device", device: "/dev/sdb",
			model: common.NvmeModel},
		{name: "no namespace, the kernel's default model, young",
			model: "Linux", young: true},
		{name: "no namespace, the kernel's default model, old",
			model: "Linux", removed: true},
		{name: "no namespace, dnv's model, young",
			model: common.NvmeModel, young: true},
		{name: "no namespace, dnv's model, old",
			model: common.NvmeModel, removed: true},
		// The kernel prints a device_path never written as "(null)": a build
		// cut off between its namespace mkdir and its device_path write.
		{name: "a namespace with no device written, young",
			device: agent.NsDevicePathUnset, model: "Linux", young: true},
		{name: "a namespace with no device written, old",
			device: agent.NsDevicePathUnset, model: "Linux", removed: true},
		{name: "no namespace, a host link, the kernel's default model",
			model: "Linux", hostLink: true},
		{name: "no namespace, a port link, the kernel's default model",
			model: "Linux", portLink: true},
		{name: "no namespace, a host link, dnv's model",
			model: common.NvmeModel, hostLink: true, removed: true},
		{name: "no namespace, a port link, dnv's model, young",
			model: common.NvmeModel, portLink: true, young: true,
			removed: true},
		{name: "no namespace, a host link, a model read that did not " +
			"answer", model: common.NvmeModel, hostLink: true,
			killModel: true},
		{name: "no namespace, an age read that did not answer",
			model: common.NvmeModel, killAge: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			root := agent.NvmetRoot + "/subsystems/" + foreignNqn
			linked := tc.hostLink || tc.portLink
			node.mu.Lock()
			seedNvmetSubsysModel(node, foreignNqn, tc.device, tc.model)
			if tc.young {
				node.mtimes[root] = srv.now()
			}
			if tc.hostLink {
				node.links[root+"/allowed_hosts/nqn.2024-01.example:host"] =
					agent.NvmetRoot + "/hosts/nqn.2024-01.example:host"
			}
			if tc.portLink {
				node.links[agent.NvmetRoot+"/ports/"+
					strconv.Itoa(common.NvmetPortId)+"/subsystems/"+
					foreignNqn] = root
			}
			if tc.killModel {
				node.killReadAlways[root+"/attr_model"] = true
			}
			if tc.killAge {
				node.killCmdAlways["mtime "+root] = true
			}
			node.mu.Unlock()

			node.Reset()
			reply := cnSweepSyncup(t, srv, 3, true)
			if got := subsysDirPresent(node, foreignNqn); got == tc.removed {
				t.Fatalf("the subsystem is present: %v, want removed: %v",
					got, tc.removed)
			}
			if !tc.removed {
				assertNoCall(t, node, "cmd rmdir "+root)
				assertNoCall(t, node, "writedirect "+root+
					"/namespaces/1/enable=0")
			}
			details := reply.GetAgentReply().GetDetails()
			if got := strings.Contains(details, foreignNqn+" attr_model"); got !=
				tc.killModel {
				t.Fatalf("SyncupCn reply %v, want the model read named: %v",
					reply.GetAgentReply(), tc.killModel)
			}
			if got := strings.Contains(details,
				"nvmet subsystem age of "+foreignNqn); got != tc.killAge {
				t.Fatalf("SyncupCn reply %v, want the age read named: %v",
					reply.GetAgentReply(), tc.killAge)
			}
			// What is read of it: the links first, then the model for a
			// linked one and the age for one with no link, never both.
			if tc.device == "/dev/sdb" {
				assertNoCall(t, node, "mtime "+root)
				assertNoCall(t, node, "list "+root+"/allowed_hosts")
				return
			}
			first := node.indexOfCall("list " + root + "/allowed_hosts")
			if first < 0 {
				t.Fatalf("the subsystem's hosts were never listed")
			}
			if got := node.hasCall(root + "/attr_model"); got != linked {
				t.Fatalf("attr_model read: %v, want %v\ncalls:\n%s", got,
					linked, strings.Join(node.callsMatching(root), "\n"))
			}
			if got := node.hasCall("mtime " + root); got == linked {
				t.Fatalf("age read: %v, want %v\ncalls:\n%s", got, !linked,
					strings.Join(node.callsMatching(root), "\n"))
			}
			for _, read := range []string{"mtime " + root, root + "/attr_"} {
				if at := node.indexOfCall(read); at >= 0 && at < first {
					t.Fatalf("%q came before the hosts' listing", read)
				}
			}
		})
	}
}

// TestADeletedSubsystemGoesOnAStampingKernel pins the node-level removal of
// a subsystem a deletion left, on a kernel that stamps a subsystem
// directory on every attribute lookup (7.0): the namespaces go first
// (DeleteNamespace, the cntlr's L1), then the request drops the subsystem
// (DeleteSubsystem), which keeps its allowed hosts and its port link. The
// node's read names it after reading its model, which stamps the directory,
// and the SyncupCn the read brings removes it: a linked subsystem is judged
// by its model alone, with no age that the read's own lookup would have made
// young again. The same for one planted with dnv's model and a host link.
func TestADeletedSubsystemGoesOnAStampingKernel(t *testing.T) {
	t.Run("DeleteNamespace, then DeleteSubsystem", func(t *testing.T) {
		srv, node := newTestServer(t)
		clock := withClock(srv)
		node.mu.Lock()
		node.stampAttrs = clock.Now
		node.mu.Unlock()
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
		syncupCntlrUntilClean(t, srv, noNsReq(3))
		clock.advance(2 * time.Second)
		syncupCntlrUntilClean(t, srv, reqOpts{revision: 4, primary: true,
			subsys: map[string]*pb.Subsystem{}})
		if !subsysDirPresent(node, testNqn) {
			t.Fatalf("fixture: the cntlr removed the subsystem itself")
		}
		reply := checkCnOnce(t, srv)
		cnSweepAssertDetails(t, reply.GetAgentReply(),
			agent.LeftoverKindNvmet+":"+testNqn, "the node's read")
		clock.advance(2 * time.Second)
		cn := cnSweepSyncup(t, srv, 2, true)
		cnSweepAssertCode(t, cn.GetAgentReply(), 0, "the SyncupCn")
		if subsysDirPresent(node, testNqn) {
			t.Fatalf("the deleted subsystem survived the SyncupCn its read " +
				"brought")
		}
	})
	t.Run("a host link and dnv's model", func(t *testing.T) {
		srv, node := newTestServer(t)
		clock := withClock(srv)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
		root := agent.NvmetRoot + "/subsystems/" + foreignNqn
		node.mu.Lock()
		node.stampAttrs = clock.Now
		seedNvmetSubsysModel(node, foreignNqn, "", common.NvmeModel)
		node.mtimes[root] = clock.Now()
		node.links[root+"/allowed_hosts/nqn.2024-01.example:host"] =
			agent.NvmetRoot + "/hosts/nqn.2024-01.example:host"
		node.mu.Unlock()
		reply := checkCnOnce(t, srv)
		cnSweepAssertDetails(t, reply.GetAgentReply(),
			agent.LeftoverKindNvmet+":"+foreignNqn, "the node's read")
		clock.advance(2 * time.Second)
		cn := cnSweepSyncup(t, srv, 2, true)
		cnSweepAssertCode(t, cn.GetAgentReply(), 0, "the SyncupCn")
		if subsysDirPresent(node, foreignNqn) {
			t.Fatalf("the linked subsystem with dnv's model survived")
		}
	})
}

// A cntlr's pass judges no host-facing subsystem with no namespace device
// that no held request claims: it is no pool's at cntlr level, and nothing
// more of it is read there — no age, no listing of its hosts, no attribute
// file. On a kernel that stamps the subsystem directory on an attribute
// lookup, a model read in every cntlr read would keep the directory young
// for good (CN21), and the node sweep would never find it old. Here the
// stand-in node stamps the directory on such a read, as kernel 7.0 does: the
// subsystem sits through cntlr reads and converges across the grace, and the
// node-level sweep after the grace still removes it.
func TestACntlrPassReadsNothingOfAnUnclaimedEmptySubsystem(t *testing.T) {
	ctx := context.Background()
	srv, node := newTestServer(t)
	clock := withClock(srv)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	root := agent.NvmetRoot + "/subsystems/" + foreignNqn
	node.mu.Lock()
	seedNvmetSubsysModel(node, foreignNqn, "", "Linux")
	node.mtimes[root] = clock.Now()
	node.stampAttrs = clock.Now
	node.mu.Unlock()

	for step := time.Duration(0); step <= common.CnSubsysOrphanGrace; step +=
		common.CnSubsysOrphanGrace / 4 {
		node.Reset()
		refreshTestCntlr(t, srv)
		reply, err := srv.SyncupCntlr(ctx,
			cntlrReq(reqOpts{revision: 2, primary: true}))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		cnSweepAssertCode(t, reply.GetAgentReply(), 0, "SyncupCntlr")
		for _, read := range []string{
			"mtime " + root, root + "/attr_", "list " + root + "/allowed_hosts",
		} {
			assertNoCall(t, node, read)
		}
		if !subsysDirPresent(node, foreignNqn) {
			t.Fatalf("a cntlr's pass removed the subsystem")
		}
		clock.advance(common.CnSubsysOrphanGrace / 4)
	}
	clock.advance(time.Second)
	node.Reset()
	reply := cnSweepSyncup(t, srv, 3, true)
	cnSweepAssertCode(t, reply.GetAgentReply(), 0, "SyncupCn")
	if subsysDirPresent(node, foreignNqn) {
		t.Fatalf("the old empty subsystem survived the node sweep:\n%s",
			strings.Join(node.callsMatching(root), "\n"))
	}
}
