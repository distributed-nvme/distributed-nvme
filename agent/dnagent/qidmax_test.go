package dnagent

import (
	"context"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// Each side export gets the agent's IO queue cap just before its port link,
// when no controller of it can exist yet; a converge that finds the export
// linked writes no cap, whatever the cap reads: nvmet deletes every
// controller of a subsystem whose cap is written (dnagent.md SH32).
func TestAnExportGetsItsQueueCapBeforeItsLink(t *testing.T) {
	srv, node := newTestServer(t)
	srv.port.QidMax = 4
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	portPath := portPathOf(common.NvmetPortId)

	syncupBoth(t, srv, 1, testSide)
	var subsysPaths []string
	for _, cnId := range []uint64{testCn0, testCn1} {
		nqn := nf.SideToCnNqn(testCluster, testSp, testLeg, cnId)
		subsysPath := agent.NvmetRoot + "/subsystems/" + nqn
		subsysPaths = append(subsysPaths, subsysPath)
		assertOrder(t, node,
			"writedirect "+subsysPath+"/attr_qid_max=4",
			"cmd ln -s "+subsysPath+" "+portPath+"/subsystems/"+nqn)
	}

	node.mu.Lock()
	for _, subsysPath := range subsysPaths {
		node.files[subsysPath+"/attr_qid_max"] = "128\n"
	}
	node.mu.Unlock()
	node.Reset()
	syncupBoth(t, srv, 2, testSide)
	if got := node.callsMatching("attr_qid_max="); len(got) != 0 {
		t.Fatalf("a converge of linked exports wrote the cap: %v", got)
	}
}

// A cap of zero writes none.
func TestAZeroQueueCapWritesNone(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	if got := node.callsMatching("attr_qid_max"); len(got) != 0 {
		t.Fatalf("a zero cap touched the cap: %v", got)
	}
}

// The migration source export gets the cap before its link too, and the
// constructor keeps the cap it is given.
func TestAMigrationSourceExportGetsItsQueueCap(t *testing.T) {
	srv, node := newTestServer(t)
	srv.port.QidMax = 4
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	syncupBoth(t, srv, 1, testSide)
	node.Reset()
	reply, err := srv.SyncupSide(context.Background(), migrSrcReq(2))
	if err != nil || reply.GetAgentReply().GetCode() != 0 {
		t.Fatalf("SyncupSide = %v, %v", reply.GetAgentReply(), err)
	}
	srcNqn := nf.MigrSrcNqn(testCluster, testDn, testSp, testMigrId)
	subsysPath := agent.NvmetRoot + "/subsystems/" + srcNqn
	assertOrder(t, node,
		"writedirect "+subsysPath+"/attr_qid_max=4",
		"cmd ln -s "+subsysPath+" "+portPathOf(common.NvmetPortId)+
			"/subsystems/"+srcNqn)

	built := NewDnAgentServer(newFakeNode().osClient(), nf,
		common.DefaultLocalStorPrefix, testDisk, testTrConf(),
		common.NvmetPortId, 6)
	if built.port.QidMax != 6 {
		t.Fatalf("NewDnAgentServer kept the cap %d, want 6",
			built.port.QidMax)
	}
}
