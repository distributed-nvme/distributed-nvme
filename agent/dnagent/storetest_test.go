package dnagent

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// TestAStoreTestMarksAFullDisk is DN18's store test: a store disk that
// fills while nothing is saved refuses the store test's write, and the next
// read's store row reads the refusal, so the worker marks the node before a
// save is needed; once the disk takes writes again, the read's probe clears
// the row. A store test on a healthy disk leaves no file behind, and while a
// refusal holds the test writes nothing.
func TestAStoreTestMarksAFullDisk(t *testing.T) {
	ctx := context.Background()
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	d := useStoreDisk(srv, node)
	probe := srv.nf.LocalDnProbePath(testCluster, testDn)
	ok, bad := pb.ResStatus_RES_STATUS_OK, pb.ResStatus_RES_STATUS_ERROR
	readRow := func() *pb.ResInfo {
		readDnNow(ctx, srv)
		return dnRound(srv, "round").GetDnInfo().GetStoreInfo()
	}

	srv.testStore(ctx)
	if n := d.count(probe); n != 1 {
		t.Fatalf("%d test writes on a healthy store, want 1", n)
	}
	node.mu.Lock()
	_, left := node.protos[probe]
	node.mu.Unlock()
	if left {
		t.Fatal("the store test left its file behind")
	}
	assertStoreRow(t, "a read after a test that held", readRow(), ok, "")

	d.fail(fmt.Errorf("write %s: %s", probe, noSpace))
	srv.testStore(ctx)
	// While the refusal holds the test writes nothing: the read's probe
	// writes on every read then.
	before := d.count(probe)
	srv.testStore(ctx)
	if n := d.count(probe); n != before {
		t.Fatalf("the store test wrote %d times while a refusal held",
			n-before)
	}
	assertStoreRow(t, "a read after a refused test", readRow(), bad, noSpace)

	d.fail(nil)
	assertStoreRow(t, "a read once the disk takes writes", readRow(), ok, "")
}

// TestTheStoreTestLoopRuns: the loop the startup pass starts tests the store
// on its own, with no syncup or read to bring it, and the join stops it.
func TestTheStoreTestLoopRuns(t *testing.T) {
	node := newFakeNode()
	node.devSize[testDisk] = 512 << 20
	node.devNo[testDisk] = "253:0"
	srv := NewDnAgentServer(node.osClient(),
		common.NewNameFmt(common.DefaultLocalStorPrefix),
		common.DefaultLocalStorPrefix, testDisk, testTrConf(),
		common.NvmetPortId, 0)
	srv.fenceWait = 0
	srv.roundReads = false
	srv.storeTestInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := srv.SyncupDn(ctx, dnReq(1)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	probe := "writeproto " + srv.nf.LocalDnProbePath(testCluster, testDn)
	deadline := time.Now().Add(5 * time.Second)
	for !node.hasCall(probe) {
		if time.Now().After(deadline) {
			t.Fatal("the store test loop never wrote")
		}
		time.Sleep(time.Millisecond)
	}
	srv.WaitBackground()
	node.Reset()
	time.Sleep(20 * time.Millisecond)
	if node.hasCall(probe) {
		t.Fatal("the store test loop wrote after the join")
	}
}
