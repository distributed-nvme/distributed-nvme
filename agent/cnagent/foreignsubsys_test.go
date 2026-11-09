package cnagent

import (
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
)

// The node-level sweep removes an unowned host-facing subsystem, the residue
// of a teardown cut off between its rmdirs; a subsystem dnv did not make is
// left alone (CN21). dnv exports a host namespace only over an ns-dev, so a
// namespace over any other device makes the subsystem foreign; a subsystem
// with no namespace device is dnv's only when its model reads dnv's, which
// the gateway stamps on every subsystem record, and a model read that did not
// answer counts as not dnv's, named as a failure.
func TestTheNodeSweepLeavesAForeignSubsystemAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		device string
		model  string
		// killModel makes the model read not answer.
		killModel bool
		removed   bool
	}{
		{"a namespace over another block device", "/dev/sdb",
			common.NvmeModel, false, false},
		{"no namespace, the kernel's default model", "", "Linux", false,
			false},
		{"no namespace, dnv's model", "", common.NvmeModel, false, true},
		// The kernel prints a device_path never written as "(null)": a build
		// cut off between its namespace mkdir and its device_path write.
		{"a namespace with no device written, dnv's model",
			agent.NsDevicePathUnset, common.NvmeModel, false, true},
		{"a namespace with no device written, the kernel's default model",
			agent.NsDevicePathUnset, "Linux", false, false},
		{"no namespace, a model read that did not answer", "",
			common.NvmeModel, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
			const nqn = "nqn.2024-01.example:operator-export"
			node.mu.Lock()
			seedNvmetSubsysModel(node, nqn, tc.device, tc.model)
			if tc.killModel {
				node.killReadAlways[agent.NvmetRoot+"/subsystems/"+nqn+
					"/attr_model"] = true
			}
			node.mu.Unlock()

			node.Reset()
			reply := cnSweepSyncup(t, srv, 3, true)
			if got := subsysDirPresent(node, nqn); got == tc.removed {
				t.Fatalf("the subsystem is present: %v, want removed: %v",
					got, tc.removed)
			}
			if !tc.removed {
				assertNoCall(t, node, "cmd rmdir "+agent.NvmetRoot+
					"/subsystems/"+nqn)
				assertNoCall(t, node, "writedirect "+agent.NvmetRoot+
					"/subsystems/"+nqn+"/namespaces/1/enable=0")
			}
			details := reply.GetAgentReply().GetDetails()
			if got := strings.Contains(details, nqn+" attr_model"); got !=
				tc.killModel {
				t.Fatalf("SyncupCn reply %v, want the model read named: %v",
					reply.GetAgentReply(), tc.killModel)
			}
		})
	}
}
