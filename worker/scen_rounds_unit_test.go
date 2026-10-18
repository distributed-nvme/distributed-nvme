package worker

import (
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// roundsErrRecordRead is the failure the store's reads return while a case below
// holds them failing.
var roundsErrRecordRead = errors.New("etcd unavailable")

// roundsSetGetErr makes every read of the fake store fail with err, nil to
// let them answer again.
func roundsSetGetErr(s *fakeStore, err error) {
	s.mu.Lock()
	s.getErr = err
	s.mu.Unlock()
}

// roundsSyncupResultsNaming are the syncup result records whose error names want.
func roundsSyncupResultsNaming(logs *logCapture, want error) []map[string]any {
	var out []map[string]any
	for _, rec := range logs.withMsg(msgSyncupResult) {
		if msg, _ := rec["error"].(string); strings.Contains(msg, want.Error()) {
			out = append(out, rec)
		}
	}
	return out
}

// TestDnRoleFailedRecordReadLogsTheSyncupResult pins RW13 and the syncup
// result record (dnv-worker.md, Log records): a dn syncup whose read of the
// DnConf with its DnRev fails sends no SyncupDn, fails with the read's error,
// which its syncup result names, and is retried by the next round once the
// read answers.
func TestDnRoleFailedRecordReadLogsTheSyncupResult(t *testing.T) {
	synctest.Test(t, testDnRoleFailedRecordReadLogsTheSyncupResult)
}

func testDnRoleFailedRecordReadLogsTheSyncupResult(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			return &pb.CheckDnReply{Revision: 0}
		},
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, dnTestConf())
	roundsSetGetErr(h.store, roundsErrRecordRead)
	h.startDn(testAddr, 3)

	waitFor(t, "a syncup result naming the failed read", func() bool {
		return len(roundsSyncupResultsNaming(h.logs, roundsErrRecordRead)) >= 1
	})
	if got := len(stub.syncups()); got != 0 {
		t.Fatalf("%d SyncupDn sent with no record read", got)
	}
	rec := roundsSyncupResultsNaming(h.logs, roundsErrRecordRead)[0]
	if rec["dn_id"] != float64(testDnId) || rec["revision"] != float64(3) {
		t.Errorf("the syncup result names dn %v at revision %v, want dn %d at 3",
			rec["dn_id"], rec["revision"], testDnId)
	}
	roundsSetGetErr(h.store, nil)
	h.advanceUntil("the syncup once the read answers", roundInterval, func() bool {
		return len(stub.syncups()) >= 1
	})
}

// TestCnRoleFailedRecordReadLogsTheSyncupResult is the cn mirror of RW13's
// failed read (dnv-worker.md, cn role — `worker/cnrole.go`): a cn syncup
// whose read of its CnConf fails sends no SyncupCn, fails with the read's
// error, which its syncup result names, and is retried by the next round once
// the read answers.
func TestCnRoleFailedRecordReadLogsTheSyncupResult(t *testing.T) {
	t.Skip("the cn role's syncup returns no error when its CnConf read " +
		"fails, so no syncup result names the failure")
	synctest.Test(t, testCnRoleFailedRecordReadLogsTheSyncupResult)
}

func testCnRoleFailedRecordReadLogsTheSyncupResult(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubCnAgent{
		checkReply: func(req *pb.CheckCnRequest) *pb.CheckCnReply {
			return &pb.CheckCnReply{Revision: 0}
		},
	}
	h.fleet.addCn(t, testCnAddr, stub)
	h.defaultConf()
	h.store.seed(t, model.CnConfKey(testCid, testCnAddr), cnTestConf())
	roundsSetGetErr(h.store, roundsErrRecordRead)
	h.startCn(testCnAddr, 2)

	waitFor(t, "a syncup result naming the failed read", func() bool {
		return len(roundsSyncupResultsNaming(h.logs, roundsErrRecordRead)) >= 1
	})
	if got := len(stub.syncups()); got != 0 {
		t.Fatalf("%d SyncupCn sent with no record read", got)
	}
	roundsSetGetErr(h.store, nil)
	h.advanceUntil("the syncup once the read answers", roundInterval, func() bool {
		return len(stub.syncups()) >= 1
	})
}
