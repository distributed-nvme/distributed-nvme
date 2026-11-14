package cnagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/agent"
)

// A resume keeps the live table the agent compared (dnagent.md, OS wrappers
// — `dm.go`, `nvmet.go`, `nvmehost.go`; cnagent.md CN16). A park whose
// reload stopped after its load leaves the ns-dev suspended on the raid0
// with the td's dm-error waiting. The revision after it unparks the
// namespace, which wants the raid0: the converge finds the live table as it
// wants it and resumes the device, and the resume must not make the waiting
// dm-error live, which would answer IO errors on a namespace the same pass
// moves to optimized.
func TestAResumeKeepsTheLiveTableAfterAStoppedReload(t *testing.T) {
	srv, node := newTestServer(t)
	srv.retryInterval = time.Hour
	ctx := context.Background()
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	nsDev := nsDevName(srv, testNs)
	raid0No := node.devNo["/dev/mapper/"+raid0Name(srv, testTd)]
	onRaid0 := agent.LinearTable(testTdSize/512, raid0No, 0)

	// Both of the pass's parks stop after their load: the sweep's pre-step
	// and the build's own step.
	node.failCmdAlways["dmsetup resume "+nsDev] = "refused"
	if _, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 3, primary: true, suspended: true})); err != nil {
		t.Fatalf("park: %v", err)
	}
	delete(node.failCmdAlways, "dmsetup resume "+nsDev)
	if dev := node.dms[nsDev]; dev.waiting == nil || !dev.suspended ||
		dev.table != onRaid0 {
		t.Fatalf("fixture is wrong: the park's reload did not stop after "+
			"its load (waiting %v, suspended %v, table %q):\n%s",
			dev.waiting != nil, dev.suspended, dev.table,
			strings.Join(node.callsMatching(nsDev), "\n"))
	}

	if _, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{
		revision: 4, primary: true})); err != nil {
		t.Fatalf("unpark: %v", err)
	}
	dev := node.dms[nsDev]
	if dev.table != onRaid0 {
		t.Errorf("the unparked ns-dev's live table is %q, want %q",
			dev.table, onRaid0)
	}
	if dev.waiting != nil || dev.suspended {
		t.Errorf("the ns-dev is suspended %v, a table waits %v",
			dev.suspended, dev.waiting != nil)
	}
}
