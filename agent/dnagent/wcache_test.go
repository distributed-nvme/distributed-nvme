package dnagent

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/common"
	pb "github.com/distributed-nvme/distributed-nvme/pb"
)

// dnDmTable is the fake's live table of the dm device name.
func dnDmTable(node *fakeNode, name string) string {
	node.mu.Lock()
	defer node.mu.Unlock()
	if dm, ok := node.dms[name]; ok {
		return dm.table
	}
	return ""
}

// errorWcTargets checks that table is ErrorWcTable's over devNo: two error
// targets, the last common.DmErrorWcSectors long and naming devNo.
func errorWcTargets(table, devNo string) bool {
	lines := strings.Split(strings.TrimSpace(table), "\n")
	if len(lines) != 2 {
		return false
	}
	first, last := strings.Fields(lines[0]), strings.Fields(lines[1])
	return len(first) == 3 && first[2] == "error" && len(last) == 5 &&
		last[1] == "8" && last[2] == "error" && last[3] == devNo &&
		last[4] == "0" && last[0] == first[1] &&
		common.DmErrorWcSectors == 8
}

// TestADmErrorReportsTheDisksWriteCache: each per-CN dm-error names the
// node's disk in the last target of its table, so it takes the disk's write
// cache while it fails every IO, and a cntlr's path the node serves from it
// reads, at its controller node's scan, the write cache the side's own
// device has (DN10). The one-target table of an older agent is loaded
// again into this one, and the read finds either table OK.
func TestADmErrorReportsTheDisksWriteCache(t *testing.T) {
	srv, node := newTestServer(t)
	nf := srv.nf
	syncupBoth(t, srv, 1, testSide)
	node.mu.Lock()
	diskNo := node.devNo[srv.disk]
	node.mu.Unlock()
	if diskNo == "" {
		t.Fatalf("fixture is wrong: the disk %s has no number", srv.disk)
	}
	for _, cnId := range []uint64{testCn0, testCn1} {
		errName := nf.DnErrorName(testCluster, testDn, testSp, testSide, cnId)
		if table := dnDmTable(node, errName); !errorWcTargets(table, diskNo) {
			t.Fatalf("cn %d's dm-error is %q, want two error targets, the "+
				"last naming the disk %s", cnId, table, diskNo)
		}
	}

	older := nf.DnErrorName(testCluster, testDn, testSp, testSide, testCn1)
	ctx := context.Background()
	node.mu.Lock()
	first := strings.Fields(node.dms[older].table)[1]
	node.mu.Unlock()
	n, err := strconv.ParseUint(first, 10, 64)
	if err != nil {
		t.Fatalf("the dm-error's first target length %q: %v", first, err)
	}
	sectors := n + common.DmErrorWcSectors
	if status, details := srv.probeDmError(ctx, older, sectors); status !=
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("the read of the two-target dm-error reads %v %q, want OK",
			status, details)
	}
	node.mu.Lock()
	node.dms[older].table = "0 " + strconv.FormatUint(sectors, 10) + " error"
	node.mu.Unlock()
	if status, details := srv.probeDmError(ctx, older, sectors); status !=
		pb.ResStatus_RES_STATUS_OK {
		t.Errorf("the read of an older one-target dm-error reads %v %q, "+
			"want OK", status, details)
	}

	node.Reset()
	syncupBoth(t, srv, 2, testSide)
	if table := dnDmTable(node, older); !errorWcTargets(table, diskNo) {
		t.Fatalf("the older dm-error is %q after a converge, want the "+
			"two-target table", table)
	}
}
