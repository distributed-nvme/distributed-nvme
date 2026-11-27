package dnagent

import (
	"fmt"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestACheckRoundReadsTheDiskItself is DN18's meta row: the check round
// reads the disk's header with O_DIRECT, so a disk that fails every read
// reads ERROR there, though a buffered read, which the page cache a side
// device keeps alive can answer, would still read its header.
func TestACheckRoundReadsTheDiskItself(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	if got := dnInfoOf(t, srv).GetMetaInfo().GetStatus(); got !=
		pb.ResStatus_RES_STATUS_OK {
		t.Fatalf("fixture is wrong: meta_info = %v before the fault", got)
	}
	node.mu.Lock()
	node.failReadAlways[fmt.Sprintf("readblockdirect %s ", testDisk)] = true
	node.mu.Unlock()
	meta := dnInfoOf(t, srv).GetMetaInfo()
	if meta.GetStatus() != pb.ResStatus_RES_STATUS_ERROR {
		t.Fatalf("meta_info = %v/%q on a disk that fails its reads, want "+
			"ERROR", meta.GetStatus(), meta.GetDetails())
	}
}
