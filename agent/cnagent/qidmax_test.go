package cnagent

import (
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The host-facing subsystem gets the agent's IO queue cap just before its
// port link, when no controller of it can exist yet; a converge that finds
// it linked writes no cap, whatever the cap reads: nvmet deletes every
// controller of a subsystem whose cap is written (dnagent.md SH32).
func TestASubsystemGetsItsQueueCapBeforeItsLink(t *testing.T) {
	srv, node := newTestServer(t)
	srv.port.QidMax = 4
	subsysPath := agent.NvmetRoot + "/subsystems/" + testNqn
	portPath := agent.NewNvmet(nil).PortPath(common.NvmetPortId)

	syncupBoth(t, srv, reqOpts{revision: 1, primary: true})
	assertOrder(t, node,
		"writedirect "+subsysPath+"/attr_qid_max=4",
		"cmd ln -s "+subsysPath+" "+portPath+"/subsystems/"+testNqn)

	node.mu.Lock()
	node.files[subsysPath+"/attr_qid_max"] = "128\n"
	node.mu.Unlock()
	node.Reset()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	if got := node.callsMatching("attr_qid_max="); len(got) != 0 {
		t.Fatalf("a converge of a linked subsystem wrote the cap: %v", got)
	}
}

// A transfer's subsystem gets the cap before its link too, and the
// constructor keeps the cap it is given.
func TestATransferSubsystemGetsItsQueueCap(t *testing.T) {
	srv, node := newTestServer(t)
	srv.port.QidMax = 4
	portPath := agent.NewNvmet(nil).PortPath(common.NvmetPortId)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	node.Reset()
	syncupBoth(t, srv, reqOpts{revision: 3, primary: true,
		xfers: []*pb.Transfer{{
			XferId:       testXfer,
			OriNqn:       testNqn,
			OriNsIdx:     1,
			AllowedHosts: []string{testHostNqn},
			AutoSuspend:  true,
		}}})
	xferNqn := srv.nf.XferNqn(testCluster, testSp, testXfer)
	subsysPath := agent.NvmetRoot + "/subsystems/" + xferNqn
	assertOrder(t, node,
		"writedirect "+subsysPath+"/attr_qid_max=4",
		"cmd ln -s "+subsysPath+" "+portPath+"/subsystems/"+xferNqn)

	built := NewCnAgentServer(newFakeNode().osClient(),
		common.NewNameFmt(common.DefaultLocalStorPrefix),
		common.DefaultLocalStorPrefix, testCapacity, testTrConf(),
		common.NvmetPortId, 6)
	if built.port.QidMax != 6 {
		t.Fatalf("NewCnAgentServer kept the cap %d, want 6",
			built.port.QidMax)
	}
}
