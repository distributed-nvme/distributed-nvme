package dnagent

import (
	"context"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// testHostNamePath is the file the kernel keeps the machine's host name in,
// written out here rather than taken from the code under test, so a read of
// another file fails the test.
const testHostNamePath = "/proc/sys/kernel/hostname"

// TestGetDnSizeReportsTheHostName pins dnagent.md SH34 on the dn role's size
// call: the reply carries the machine's host name, trimmed of its line end
// and read again on every call, so a renamed machine reports its new name;
// a failed read and an absent file each give an empty name and still answer
// the size, so a node created with a location never depends on the read.
func TestGetDnSizeReportsTheHostName(t *testing.T) {
	srv, node := newTestServer(t)
	want := node.devSize[testDisk] - common.DnDataOffset
	ask := func(what string, wantName string) {
		t.Helper()
		reply, err := srv.GetDnSize(context.Background(),
			&pb.GetDnSizeRequest{ClusterId: testCluster})
		if err != nil {
			t.Fatalf("%s: GetDnSize: %v", what, err)
		}
		if reply.GetSize() != want {
			t.Errorf("%s: size = %d, want %d", what, reply.GetSize(), want)
		}
		if reply.GetHostName() != wantName {
			t.Errorf("%s: host_name = %q, want %q",
				what, reply.GetHostName(), wantName)
		}
	}
	setName := func(name string) {
		node.mu.Lock()
		defer node.mu.Unlock()
		node.files[testHostNamePath] = name
	}

	setName("node-a\n")
	ask("a machine named node-a", "node-a")
	setName("node-b\n")
	ask("the machine renamed", "node-b")

	node.mu.Lock()
	node.failReadAlways[testHostNamePath] = true
	node.mu.Unlock()
	ask("a read that fails", "")
	node.mu.Lock()
	delete(node.failReadAlways, testHostNamePath)
	delete(node.files, testHostNamePath)
	node.mu.Unlock()
	ask("no host name file", "")
	if got := node.Mutations(); len(got) != 0 {
		t.Errorf("GetDnSize changed the node: %q", got)
	}
}
