package cnagent

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A request whose save fails is refused (CN7, CN8; dnagent.md SH5, SH30)
//
// A SyncupCn or a SyncupCntlr the gates admit is saved before it becomes the
// desired state. When the save fails the request is refused with
// common.ReplyCodeSaveFailed at the revision the agent still holds, and
// nothing of it is kept or converged, so the files hold the last request the
// agent accepted and a restart undoes nothing. The one exception, a save
// whose rename held and whose directory fsync alone failed, leaves the
// refused request in its file, which a restart converges and the worker
// wants anyway; the tests here fail the whole write, as a full store disk
// does.
// ---------------------------------------------------------------------------

// noSpace is the error text of the save failure the tests inject, the one a
// full store disk gives.
const noSpace = "no space left on device"

// saveFault fails the store's writes of the paths it holds, as a full or
// read-only store disk fails them: the write changes no file and records
// "writeproto <path> failed" among the node's calls, which Mutations counts.
type saveFault struct {
	mu    sync.Mutex
	paths map[string]bool
}

// failSaves gives srv a store over node whose writes of paths fail
// (saveFault). The store is a new one, so no save of it has failed yet.
func failSaves(srv *CnAgentServer, node *fakeNode, paths ...string) *saveFault {
	fault := &saveFault{paths: make(map[string]bool)}
	for _, path := range paths {
		fault.paths[path] = true
	}
	oc := node.osClient()
	write := oc.WriteProtoFn
	oc.WriteProtoFn = func(
		ctx context.Context, path string, msg proto.Message,
	) error {
		fault.mu.Lock()
		failing := fault.paths[path]
		fault.mu.Unlock()
		if !failing {
			return write(ctx, path, msg)
		}
		node.mu.Lock()
		node.record("writeproto %s failed", path)
		node.mu.Unlock()
		return fmt.Errorf("write %s: %s", path, noSpace)
	}
	srv.store = agent.NewStore(oc, common.DefaultLocalStorPrefix)
	return fault
}

// failedWrite is the call line a failed write of path leaves (saveFault).
func failedWrite(path string) string {
	return "writeproto " + path + " failed"
}

// assertOnlyFailedWrite checks that the node saw no call but the failed
// write of path, read-only ones included: the gates read nothing of the
// node, so a refused call that converged, swept or dropped anything, even a
// pass with nothing left to change that only probes, shows more.
func assertOnlyFailedWrite(t *testing.T, node *fakeNode, path string) {
	t.Helper()
	calls := node.Calls()
	if len(calls) != 1 || calls[0] != failedWrite(path) {
		t.Errorf("want the failed write of %s and no other call, got:\n%s",
			path, strings.Join(calls, "\n"))
	}
}

// assertSaveRefused checks a refusal for a failed save: the code, the
// revision the agent held, and details that carry the save's error.
func assertSaveRefused(
	t *testing.T,
	label string,
	reply *pb.AgentReply,
	revision uint64,
	held uint64,
) {
	t.Helper()
	if reply.GetCode() != common.ReplyCodeSaveFailed {
		t.Errorf("%s: code %d (%q), want %d, a refusal for the failed save",
			label, reply.GetCode(), reply.GetDetails(),
			common.ReplyCodeSaveFailed)
	} else if !strings.Contains(reply.GetDetails(), noSpace) {
		t.Errorf("%s: details %q do not carry the save's error",
			label, reply.GetDetails())
	}
	if revision != held {
		t.Errorf("%s: revision %d, want %d, the one the agent holds",
			label, revision, held)
	}
}

// storedCnRevision is the revision of the fixture CN's request in the local
// store, read from the fake node itself.
func storedCnRevision(t *testing.T, srv *CnAgentServer, node *fakeNode) uint64 {
	t.Helper()
	path := srv.nf.LocalCnPath(testCluster, testCn)
	node.mu.Lock()
	raw, ok := node.protos[path]
	node.mu.Unlock()
	if !ok {
		t.Fatalf("no cn file at %s", path)
	}
	req := &pb.SyncupCnRequest{}
	if err := proto.Unmarshal(raw, req); err != nil {
		t.Fatalf("the cn file does not decode: %v", err)
	}
	return req.GetRevision()
}

// storeFileExists reports whether the local store holds a file at path.
func storeFileExists(node *fakeNode, path string) bool {
	node.mu.Lock()
	defer node.mu.Unlock()
	_, ok := node.protos[path]
	return ok
}

// cntlrPathOf is the store file of the fixture CN's cntlr ptr.
func cntlrPathOf(srv *CnAgentServer, ptr *pb.CntlrPointer) string {
	return srv.nf.LocalCntlrPath(testCluster, testCn, ptr.GetSpId(),
		ptr.GetCntlrId())
}

// spDms are the names of the node's dm devices of sp, sorted: every dm name
// of a cntlr carries its sp id (common.NameFmt).
func spDms(node *fakeNode, spId uint64) []string {
	tag := fmt.Sprintf("-%016x-", spId)
	node.mu.Lock()
	defer node.mu.Unlock()
	var out []string
	for name := range node.dms {
		if strings.Contains(name, tag) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// primaryCntlrReq2 is cntlrReq2's cntlr, of the second SP, as a primary, so
// its converge builds its pool, its thin volume and its namespace device.
func primaryCntlrReq2(revision uint64) *pb.SyncupCntlrRequest {
	req := cntlrReq2(revision)
	req.Cntlr.Primary = true
	return req
}

// TestASyncupCnWhoseSaveFailsIsRefused: a SyncupCn the revision gate admits
// whose cn file write fails is refused with ReplyCodeSaveFailed at the
// revision the agent holds for the CN, zero for a CN it does not hold, the
// save's error in its details and no row, stamp or save time (CN7;
// dnagent.md SH5). Its request does not become the CN's desired state:
// nothing converges or sweeps, the file keeps the request before it, the
// CN's round answers that request's revision from the observation before
// it, and a cntlr whose pointer only the refused list names stays unknown
// (CN8), so no cntlr is built that a restart over the stored list would
// remove. A refused list that drops a held cntlr drops nothing: the cntlr,
// its file, its devices and its lock entry stay. A CN the agent does not
// hold is not registered by the refused request: its round answers unknown
// object, so the worker sends the SyncupCn again.
func TestASyncupCnWhoseSaveFailsIsRefused(t *testing.T) {
	ctx := context.Background()
	t.Run("a held CN", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 1})
		cnPath := srv.nf.LocalCnPath(testCluster, testCn)
		obsBefore := latestCnObs(t, srv)
		failSaves(srv, node, cnPath)
		node.Reset()

		reply, err := srv.SyncupCn(ctx, cnReq2(2))
		if err != nil {
			t.Fatalf("SyncupCn: %v", err)
		}
		assertSaveRefused(t, "SyncupCn", reply.GetAgentReply(),
			reply.GetRevision(), 1)
		if reply.GetCnInfo() != nil || reply.GetObsStamp() != nil ||
			reply.GetSaveTime() != nil {
			t.Errorf("the refusal carries a converge's reply: info %v, "+
				"stamp %v, save time %v", reply.GetCnInfo(),
				reply.GetObsStamp(), reply.GetSaveTime())
		}
		assertOnlyFailedWrite(t, node, cnPath)
		if got := storedCnRevision(t, srv, node); got != 1 {
			t.Errorf("the cn file holds revision %d, want 1", got)
		}
		if got := fixtureCnState(t, srv).loadReq().GetRevision(); got != 1 {
			t.Errorf("the CN's held request is at revision %d, want 1", got)
		}
		if got := latestCnObs(t, srv); !got.Time.Equal(obsBefore.Time) ||
			got.TraceId != obsBefore.TraceId {
			t.Errorf("the refused SyncupCn stored an observation: %+v "+
				"over %+v", got, obsBefore)
		}
		if got := roundCn(srv, "round", 2).GetRevision(); got != 1 {
			t.Errorf("the CN's round answers revision %d, want 1", got)
		}

		node.Reset()
		xReply, err := srv.SyncupCntlr(ctx, primaryCntlrReq2(1))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		if got := xReply.GetAgentReply().GetCode(); got !=
			common.ReplyCodeUnknownObject {
			t.Errorf("the SyncupCntlr of the cntlr only the refused list "+
				"names answered code %d (%q), want unknown object", got,
				xReply.GetAgentReply().GetDetails())
		}
		if calls := node.Calls(); len(calls) != 0 {
			t.Errorf("the cntlr only the refused list names was built:\n%s",
				strings.Join(calls, "\n"))
		}
		if srv.getCntlr(cntlrKey(testCluster, testCn, testSp2,
			testCntlr2)) != nil {
			t.Error("the agent holds the cntlr only the refused list names")
		}
	})
	t.Run("a list that drops a held cntlr", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 1, primary: true})
		cnPath := srv.nf.LocalCnPath(testCluster, testCn)
		cntlrK := cntlrKey(testCluster, testCn, testSp, testCntlr)
		st := srv.getCntlr(cntlrK)
		dms := spDms(node, testSp)
		if len(dms) == 0 {
			t.Fatal("fixture is wrong: the cntlr has no dm device")
		}
		entries := srv.locks.ObjCount()
		failSaves(srv, node, cnPath)
		node.Reset()

		reply, err := srv.SyncupCn(ctx, cnReq(2, false))
		if err != nil {
			t.Fatalf("SyncupCn: %v", err)
		}
		assertSaveRefused(t, "SyncupCn", reply.GetAgentReply(),
			reply.GetRevision(), 1)
		assertOnlyFailedWrite(t, node, cnPath)
		if got := srv.getCntlr(cntlrK); got == nil || got != st {
			t.Error("the refused SyncupCn dropped the held cntlr")
		}
		if !storeFileExists(node, cntlrPathOf(srv, cntlrPtr())) {
			t.Error("the refused SyncupCn removed the cntlr file")
		}
		if got := spDms(node, testSp); !slices.Equal(got, dms) {
			t.Errorf("the cntlr's dm devices went from %v to %v", dms, got)
		}
		if got := srv.locks.ObjCount(); got != entries {
			t.Errorf("%d lock entries after the refused SyncupCn, want %d",
				got, entries)
		}
		if !pointerKnown(fixtureCnState(t, srv).loadReq(), cntlrPtr()) {
			t.Error("the CN's held list lost the cntlr's pointer")
		}
	})
	t.Run("a CN the agent does not hold", func(t *testing.T) {
		srv, node := newTestServer(t)
		cnPath := srv.nf.LocalCnPath(testCluster, testCn)
		failSaves(srv, node, cnPath)
		node.Reset()

		reply, err := srv.SyncupCn(ctx, cnReq(1, true))
		if err != nil {
			t.Fatalf("SyncupCn: %v", err)
		}
		assertSaveRefused(t, "SyncupCn", reply.GetAgentReply(),
			reply.GetRevision(), 0)
		assertOnlyFailedWrite(t, node, cnPath)
		if storeFileExists(node, cnPath) {
			t.Error("a cn file exists")
		}
		if srv.getCn(cnKey(testCluster, testCn)) != nil {
			t.Error("the refused SyncupCn registered the CN")
		}
		if got := roundCn(srv, "round", 1).GetAgentReply().GetCode(); got !=
			common.ReplyCodeUnknownObject {
			t.Errorf("the CN's round answers code %d, want unknown object",
				got)
		}
	})
}

// TestASyncupCntlrWhoseSaveFailsIsRefused: a SyncupCntlr CN8's gates admit
// whose cntlr file write fails is refused with ReplyCodeSaveFailed at the
// revision the agent holds for the cntlr, zero for a cntlr it does not hold,
// with no row, bitmap info, stamp or save time (CN8, CN20; dnagent.md SH5):
// the held cntlr carries a clone, so a reply built from its state would
// carry bitmap info.
// Nothing converges, the file keeps the request before it, the observation
// and the round stay at that request's revision, and the held request is
// the same one. The cntlr here is loaded by a restart, so its request did
// not come through a SyncupCntlr of this process: a refused request does not
// make it one, since the thin-id sweep (CN14) and the forced-start marks
// (CN12) act only on a request that counts as delivered (CN8,
// cntlrState.reqFromRpc). A cntlr the agent does not hold is not registered
// by the refused request: its round answers unknown object.
func TestASyncupCntlrWhoseSaveFailsIsRefused(t *testing.T) {
	ctx := context.Background()
	t.Run("a held cntlr", func(t *testing.T) {
		clones := []*pb.Clone{cloneOf()}
		first, node := newTestServer(t)
		syncupBoth(t, first, reqOpts{revision: 1, clones: clones})
		srv := newCnServer(node)
		reconcileForTest(t, srv)
		st := fixtureCntlrState(t, srv)
		held := st.loadReq()
		if st.reqFromRpc {
			t.Fatal("fixture is wrong: the request the restart loaded " +
				"counts as delivered")
		}
		if len(srv.bitmapInfoList(st)) == 0 {
			t.Fatal("fixture is wrong: the held cntlr has no bitmap info")
		}
		obsBefore := latestCntlrObs(t, srv)
		path := cntlrPathOf(srv, cntlrPtr())
		failSaves(srv, node, path)
		node.Reset()

		reply, err := srv.SyncupCntlr(ctx,
			cntlrReq(reqOpts{revision: 2, primary: true, clones: clones}))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		assertSaveRefused(t, "SyncupCntlr", reply.GetAgentReply(),
			reply.GetRevision(), 1)
		if reply.GetCntlrInfo() != nil || len(reply.GetBmInfoList()) != 0 ||
			reply.GetObsStamp() != nil || reply.GetSaveTime() != nil {
			t.Errorf("the refusal carries a converge's reply: info %v, "+
				"bm info %v, stamp %v, save time %v", reply.GetCntlrInfo(),
				reply.GetBmInfoList(), reply.GetObsStamp(),
				reply.GetSaveTime())
		}
		assertOnlyFailedWrite(t, node, path)
		if got := storedCntlrRevision(t, srv, node); got != 1 {
			t.Errorf("the cntlr file holds revision %d, want 1", got)
		}
		if got := latestCntlrObs(t, srv); got.Revision != 1 ||
			!got.Time.Equal(obsBefore.Time) ||
			got.TraceId != obsBefore.TraceId {
			t.Errorf("the refused SyncupCntlr stored an observation: %+v "+
				"over %+v", got, obsBefore)
		}
		if got := roundCntlr(srv, "round", 2).GetRevision(); got != 1 {
			t.Errorf("the cntlr's round answers revision %d, want 1", got)
		}
		if cur := fixtureCntlrState(t, srv); cur != st {
			t.Fatal("the refused SyncupCntlr replaced the cntlr's state")
		}
		if st.loadReq() != held {
			t.Errorf("the cntlr's held request is the refused one, at "+
				"revision %d", st.loadReq().GetRevision())
		}
		if st.reqFromRpc {
			t.Error("after the refused request, the held request counts " +
				"as delivered")
		}
	})
	t.Run("a cntlr the agent does not hold", func(t *testing.T) {
		srv, node := newTestServer(t)
		cnSyncup(t, srv, 1, true)
		path := cntlrPathOf(srv, cntlrPtr())
		failSaves(srv, node, path)
		node.Reset()

		reply, err := srv.SyncupCntlr(ctx,
			cntlrReq(reqOpts{revision: 1, primary: true}))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		assertSaveRefused(t, "SyncupCntlr", reply.GetAgentReply(),
			reply.GetRevision(), 0)
		assertOnlyFailedWrite(t, node, path)
		if storeFileExists(node, path) {
			t.Error("a cntlr file exists")
		}
		if srv.getCntlr(cntlrKey(testCluster, testCn, testSp,
			testCntlr)) != nil {
			t.Error("the refused SyncupCntlr registered the cntlr")
		}
		if got := roundCntlr(srv, "round", 1).GetAgentReply().
			GetCode(); got != common.ReplyCodeUnknownObject {
			t.Errorf("the cntlr's round answers code %d, want unknown "+
				"object", got)
		}
	})
}

// TestARestartAfterARefusedCnSaveRemovesNoBuiltCntlr: a restart removes no
// cntlr the agent reported built before it. Cntlr A is saved and built; a
// SyncupCn that adds cntlr X fails its save and X's SyncupCntlr follows.
// Were the refused list kept in memory while the file holds the one before
// it, X's SyncupCntlr would pass the pointer gate (CN8) and build X, and the
// startup reconcile over the stored list would forget X and its sweep remove
// its devices (CN2, CN7, CN21): its hosts would lose IO. The SyncupCn is
// refused for its save (dnagent.md SH5) and X's SyncupCntlr is answered
// unknown object, so X is never built, the worker sends both again, and
// every cntlr reported built keeps its devices and its file over the
// restart.
func TestARestartAfterARefusedCnSaveRemovesNoBuiltCntlr(t *testing.T) {
	ctx := context.Background()
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	failSaves(srv, node, srv.nf.LocalCnPath(testCluster, testCn))

	cnReply, err := srv.SyncupCn(ctx, cnReq2(3))
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	xReply, err := srv.SyncupCntlr(ctx, primaryCntlrReq2(2))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}

	// built is every cntlr the agent reported built: A, whose SyncupCntlr
	// syncupBoth accepted, and X if the agent accepted its SyncupCntlr.
	type builtCntlr struct {
		name string
		ptr  *pb.CntlrPointer
		dms  []string
	}
	built := []builtCntlr{{name: "A", ptr: cntlrPtr()}}
	if xReply.GetAgentReply().GetCode() == 0 &&
		xReply.GetRevision() == 2 {
		built = append(built, builtCntlr{name: "X", ptr: cntlrPtr2()})
	}
	for i := range built {
		built[i].dms = spDms(node, built[i].ptr.GetSpId())
		if len(built[i].dms) == 0 {
			t.Fatalf("fixture is wrong: cntlr %s was reported built and "+
				"has no dm device", built[i].name)
		}
	}

	// The process exits: its retries and probers stop. The kernel state and
	// the local store survive.
	for _, key := range srv.allCntlrKeys() {
		st := srv.getCntlr(key)
		srv.stopConnectRetry(st)
		srv.stopLegProbers(st, nil)
	}
	node.Reset()
	fresh := newCnServer(node)
	reconcileForTest(t, fresh)

	for _, c := range built {
		after := map[string]bool{}
		for _, name := range spDms(node, c.ptr.GetSpId()) {
			after[name] = true
		}
		for _, name := range c.dms {
			if !after[name] {
				t.Errorf("the restart removed %s of cntlr %s, which the "+
					"agent reported built", name, c.name)
			}
		}
		if !storeFileExists(node, cntlrPathOf(fresh, c.ptr)) {
			t.Errorf("the restart removed the file of cntlr %s, which the "+
				"agent reported built", c.name)
		}
	}
	if got := cnReply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeSaveFailed {
		t.Errorf("the SyncupCn whose save failed answered code %d, want %d",
			got, common.ReplyCodeSaveFailed)
	}
	if got := xReply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeUnknownObject {
		t.Errorf("the SyncupCntlr of X answered code %d, want unknown object",
			got)
	}
}

// TestALockEntryOfARefusedCntlrSaveGoesWithItsPointer: a SyncupCntlr CN8's
// gates admit for a cntlr the agent does not hold takes the cntlr's object
// lock before its save (dnagent.md, Concurrency — `locks.go`). When the save
// fails no teardown runs for the cntlr, which the agent never held, so the
// entry stays and the request sent again reuses it; the CN7 pointer diff
// drops it once the pointer leaves the CN's list (dropRemovedCntlrs), so
// refused saves do not grow the lock set for the life of the process.
func TestALockEntryOfARefusedCntlrSaveGoesWithItsPointer(t *testing.T) {
	ctx := context.Background()
	srv, node := newTestServer(t)
	cnSyncup(t, srv, 1, true)
	entries := srv.locks.ObjCount()
	failSaves(srv, node, cntlrPathOf(srv, cntlrPtr()))
	for _, send := range []string{"first", "second"} {
		reply, err := srv.SyncupCntlr(ctx, cntlrReq(reqOpts{revision: 1}))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		if got := reply.GetAgentReply().GetCode(); got !=
			common.ReplyCodeSaveFailed {
			t.Fatalf("fixture is wrong: the %s SyncupCntlr answered code "+
				"%d, want a refusal for its failed save", send, got)
		}
		if got := srv.locks.ObjCount(); got != entries+1 {
			t.Errorf("%d lock entries after the %s refused save, want %d",
				got, send, entries+1)
		}
	}

	cnSyncup(t, srv, 2, false)
	if got := srv.locks.ObjCount(); got != entries {
		t.Errorf("%d lock entries after the cntlr's pointer left the CN's "+
			"list, want %d", got, entries)
	}
}

// TestASaveThatHoldsEndsTheFailedSaveRule: the first save that succeeds ends
// the rule of a failed save (agent.Store.SaveFailing; dnagent.md SH30). Once
// the SyncupCn refused for its save is saved when sent again, a newer
// SyncupCn marks the node and cancels every read once more, as
// TestANodeSyncupCancelsEveryRead pins: it waits behind neither read.
func TestASaveThatHoldsEndsTheFailedSaveRule(t *testing.T) {
	ctx := context.Background()
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2})
	fault := failSaves(srv, node, srv.nf.LocalCnPath(testCluster, testCn))
	reply, err := srv.SyncupCn(ctx, cnReq(3, true))
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != common.ReplyCodeSaveFailed ||
		!srv.store.SaveFailing() {
		t.Fatalf("fixture is wrong: the SyncupCn was not refused for its "+
			"save: %v", reply.GetAgentReply())
	}

	fault.mu.Lock()
	clear(fault.paths)
	fault.mu.Unlock()
	reply, err = srv.SyncupCn(ctx, cnReq(3, true))
	if err != nil {
		t.Fatalf("SyncupCn: %v", err)
	}
	if reply.GetAgentReply().GetCode() != 0 || reply.GetRevision() != 3 {
		t.Fatalf("the SyncupCn sent again over a store that writes was "+
			"not accepted: %v at revision %d", reply.GetAgentReply(),
			reply.GetRevision())
	}
	if srv.store.SaveFailing() {
		t.Error("a save that held left the rule of a failed save in force")
	}

	srv.roundReads = true
	releaseCntlr := openGate(t, node, dmTableListLine)
	releaseCn := openGate(t, node, portProbeLine)
	node.Reset()
	roundCn(srv, "cn-round", 3)
	roundCntlr(srv, "cntlr-round", 2)
	waitFor(t, func() bool {
		return node.hasCall(portProbeLine) && node.hasCall(dmTableListLine)
	})
	// The syncup's own converge probes the port too; only the read stays
	// parked in it.
	dropGate(node, portProbeLine)
	within(t, "a newer SyncupCn behind two reads", func() {
		reply, err := srv.SyncupCn(traced("syncup"), cnReq(4, true))
		if err != nil || reply.GetAgentReply().GetCode() != 0 {
			t.Errorf("SyncupCn: %v %v", err, reply.GetAgentReply())
		}
	})
	releaseCn()
	releaseCntlr()
	joinReads(t, srv)
	if got := latestCnObs(t, srv).TraceId; got != "syncup" {
		t.Errorf("the cn's observation is %q's, want the syncup's", got)
	}
}

// TestASyncupWhileSavesFailCancelsNoRead: while the store's latest save
// failed, a SyncupCn or a SyncupCntlr the gates admit marks its own object
// alone and cancels no read, not even its object's own (dnagent.md SH30):
// a SyncupCn newer than the CN's held request marks no node. The worker
// re-sends every round a request the agent refused for a failed save, and
// nothing knows before the save that it will be refused, so a cancel on each
// re-send would stop each round's reads: the observations of objects with no
// fault would go too old and their health epochs on and off (dnv-worker.md
// HL7). A SyncupCn at the held revision cancels not even the CN's own read,
// since its save is refused too and stores no observation in its place. The
// syncup waits for the reads instead; they run on to their end and store,
// and the syncup, whose save fails again, is refused and ends its mark, so a
// round after it starts a read of each object. The latest failed save is the
// same request's, refused a round before, or another object's.
func TestASyncupWhileSavesFailCancelsNoRead(t *testing.T) {
	cntlrK := cntlrKey(testCluster, testCn, testSp, testCntlr)
	cnK := cnKey(testCluster, testCn)
	sendCn := func(srv *CnAgentServer, traceId string) *pb.AgentReply {
		reply, _ := srv.SyncupCn(traced(traceId), cnReq(3, true))
		return reply.GetAgentReply()
	}
	// sendHeldCn is a SyncupCn at the revision the agent holds for the CN.
	sendHeldCn := func(srv *CnAgentServer, traceId string) *pb.AgentReply {
		reply, _ := srv.SyncupCn(traced(traceId), cnReq(2, true))
		return reply.GetAgentReply()
	}
	sendCntlr := func(srv *CnAgentServer, traceId string) *pb.AgentReply {
		reply, _ := srv.SyncupCntlr(traced(traceId),
			cntlrReq(reqOpts{revision: 3}))
		return reply.GetAgentReply()
	}
	bothReads := []string{dmTableListLine, portProbeLine}
	bothRounds := map[string]func(*CnAgentServer, string){
		"cntlr-round": func(srv *CnAgentServer, id string) {
			roundCntlr(srv, id, 2)
		},
		"cn-round": func(srv *CnAgentServer, id string) { roundCn(srv, id, 2) },
	}
	bothStored := func(t *testing.T, srv *CnAgentServer) map[string]string {
		return map[string]string{
			"cntlr-round": latestCntlrObs(t, srv).TraceId,
			"cn-round":    latestCnObs(t, srv).TraceId,
		}
	}
	cntlrRounds := map[string]func(*CnAgentServer, string){
		"cntlr-round": bothRounds["cntlr-round"],
	}
	cntlrStored := func(t *testing.T, srv *CnAgentServer) map[string]string {
		return map[string]string{
			"cntlr-round": latestCntlrObs(t, srv).TraceId,
		}
	}

	for _, tc := range []struct {
		name string
		// prime is the failed save before the reads start.
		prime  func(srv *CnAgentServer, traceId string) *pb.AgentReply
		syncup func(srv *CnAgentServer, traceId string) *pb.AgentReply
		// own is the key the syncup marks, other one it must leave quiet.
		own, other string
		// gates park the reads that run beside the syncup, one command line
		// per read, which rounds start, by trace id.
		gates  []string
		rounds map[string]func(srv *CnAgentServer, traceId string)
		// stored is the trace id each object's observation holds.
		stored func(t *testing.T, srv *CnAgentServer) map[string]string
	}{
		{"SyncupCn sent again", sendCn, sendCn, cnK, cntlrK,
			bothReads, bothRounds, bothStored},
		{"SyncupCn after a cntlr's failed save", sendCntlr, sendCn, cnK,
			cntlrK, bothReads, bothRounds, bothStored},
		{"SyncupCn at the held revision", sendCn, sendHeldCn, cnK, cntlrK,
			bothReads, bothRounds, bothStored},
		{"SyncupCntlr sent again", sendCntlr, sendCntlr, cntlrK, cnK,
			[]string{dmTableListLine}, cntlrRounds, cntlrStored},
		{"SyncupCntlr after the CN's failed save", sendCn, sendCntlr, cntlrK,
			cnK, []string{dmTableListLine}, cntlrRounds, cntlrStored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2})
			failSaves(srv, node, srv.nf.LocalCnPath(testCluster, testCn),
				cntlrPathOf(srv, cntlrPtr()))
			tc.prime(srv, "prime")
			if !srv.store.SaveFailing() {
				t.Fatal("fixture is wrong: the store's latest save did " +
					"not fail")
			}

			srv.roundReads = true
			var releases []func()
			for _, gate := range tc.gates {
				releases = append(releases, openGate(t, node, gate))
			}
			node.Reset()
			for id, round := range tc.rounds {
				round(srv, id)
			}
			waitFor(t, func() bool {
				for _, gate := range tc.gates {
					if !node.hasCall(gate) {
						return false
					}
				}
				return true
			})
			// The gates stay: a converge of the syncup that ran would park
			// in them too and keep its marks while the test looks.

			done := make(chan *pb.AgentReply, 1)
			returned := make(chan struct{})
			go func() {
				defer close(returned)
				done <- tc.syncup(srv, "syncup")
			}()
			// The mark and the cancels are one hold of mu, so once the
			// mark stands any cancel has been made; a cancelled read ends
			// at its parked command.
			waitFor(t, func() bool {
				select {
				case <-returned:
					return true
				default:
				}
				srv.mu.Lock()
				defer srv.mu.Unlock()
				return !srv.marks.SinceLocked(tc.own).IsZero()
			})
			time.Sleep(150 * time.Millisecond)
			if _, ok := tc.rounds["cntlr-round"]; ok &&
				!cntlrReadRuns(t, srv) {
				t.Error("the syncup cancelled the cntlr's read")
			}
			if _, ok := tc.rounds["cn-round"]; ok && !cnReadRuns(t, srv) {
				t.Error("the syncup cancelled the CN's read")
			}
			srv.mu.Lock()
			otherQuiet := srv.marks.QuietLocked(tc.other)
			srv.mu.Unlock()
			if !otherQuiet {
				t.Error("the syncup marked the node")
			}

			for _, release := range releases {
				release()
			}
			select {
			case reply := <-done:
				if reply.GetCode() != common.ReplyCodeSaveFailed {
					t.Errorf("the syncup answered code %d (%q), want a "+
						"refusal for its failed save", reply.GetCode(),
						reply.GetDetails())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the syncup never returned")
			}
			joinReads(t, srv)
			for id, got := range tc.stored(t, srv) {
				if got != id {
					t.Errorf("the observation is %q's, want the read %q "+
						"stored", got, id)
				}
			}
			srv.mu.Lock()
			ownQuiet := srv.marks.QuietLocked(tc.own)
			otherQuiet = srv.marks.QuietLocked(tc.other)
			srv.mu.Unlock()
			if !ownQuiet || !otherQuiet {
				t.Errorf("the refused syncup left a mark: own quiet %v, "+
					"other quiet %v", ownQuiet, otherQuiet)
			}

			// A round after the refused syncup starts a read of each object.
			for id, round := range tc.rounds {
				round(srv, id+"-after")
			}
			joinReads(t, srv)
			for id, got := range tc.stored(t, srv) {
				if got != id+"-after" {
					t.Errorf("the round after the refused syncup stored no "+
						"read: the observation is %q's, want %q's", got,
						id+"-after")
				}
			}
		})
	}
}

// TestARefusedNewerCntlrSaveTakesBackTheTrustInTheHeldRequest: a SyncupCntlr
// newer than the request the agent holds for the cntlr, refused for a failed
// save (dnagent.md SH5), makes the held request count as one the startup
// reconcile loaded (cntlrState.reqFromRpc; CN8, CN12) until a SyncupCntlr
// whose save holds arrives. The held request carries a forced-start mark
// that a SyncupCntlr of this process delivered while the group's array ran,
// so the mark waits (grpPlan.markWaits), and the newer request is the sp
// worker's clear of it (dnv-worker.md AR13). Were the held request still
// trusted, a CN10 retry attempt that reads the array broken would stop it,
// zero the other member, which can hold newer data, and start the array
// from the marked leg. The attempt acts on no mark instead
// (dropForcedStartMarks), and the cntlr's read names the mark (storedMarks;
// CN30), so the worker sends the clear again. A refused request at the held
// revision leaves the trust as it is, and the attempt acts on the mark: a
// clear raises the revision, so a request at the held revision clears no
// mark. A request whose save holds gives the trust back: the attempt over
// the broken array starts it from the marked leg, and the read names no
// mark.
func TestARefusedNewerCntlrSaveTakesBackTheTrustInTheHeldRequest(t *testing.T) {
	// marked delivers the marked request at revision 4 over the running
	// array, where the mark waits, and fails the cntlr file's saves from
	// then on.
	marked := func(t *testing.T) (*repFix, *cntlrState, *saveFault) {
		t.Helper()
		f := newRepFix(t, allLegs...)
		f.syncup(t, quietLevel(3))
		f.node.Reset()
		f.syncup(t, withMark(quietLevel(4), testDataLeg2))
		f.assertMarkLeftWaiting(t, f.leg2)
		st := fixtureCntlrState(t, f.srv)
		if !st.reqFromRpc {
			t.Fatal("fixture is wrong: the marked request a SyncupCntlr " +
				"delivered does not count as delivered")
		}
		fault := failSaves(f.srv, f.node, cntlrPathOf(f.srv, cntlrPtr()))
		return f, st, fault
	}
	// refused sends req, whose save fails, and checks its refusal at
	// revision 4 with the marked request still held.
	refused := func(
		t *testing.T,
		f *repFix,
		st *cntlrState,
		req *pb.SyncupCntlrRequest,
	) {
		t.Helper()
		held := st.loadReq()
		reply, err := f.srv.SyncupCntlr(f.ctx, req)
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		assertSaveRefused(t, "SyncupCntlr", reply.GetAgentReply(),
			reply.GetRevision(), 4)
		if cur := fixtureCntlrState(t, f.srv); cur != st ||
			st.loadReq() != held {
			t.Fatalf("the refused SyncupCntlr replaced the held request, "+
				"which is at revision %d", cur.loadReq().GetRevision())
		}
	}
	// breakAndRetry has md read the group's array broken and runs one CN10
	// retry attempt of the held request. It returns the array's md node,
	// the one the forced start stops.
	breakAndRetry := func(t *testing.T, f *repFix) string {
		t.Helper()
		f.node.setArrayState(f.mdDev, "broken")
		mdNode := f.node.mdNode(f.mdDev)
		f.node.Reset()
		retryAttempt(t, f.srv)
		return mdNode
	}
	// assertForcedStart checks that the attempt stopped the array, zeroed
	// leg 1, started the array from leg 2 and added leg 1 back.
	assertForcedStart := func(t *testing.T, f *repFix, mdNode string) {
		t.Helper()
		assertOrder(t, f.node,
			"cmd mdadm --stop "+mdNode,
			zeroOf(f.leg1),
			f.forced(f.leg2, mdUpdateUuid),
			f.add(f.leg1))
	}
	const namedMark = "forced start of group"

	t.Run("a newer request", func(t *testing.T) {
		f, st, _ := marked(t)
		refused(t, f, st, quietLevel(5))
		if st.reqFromRpc {
			t.Error("the held marked request still counts as delivered " +
				"after its clear was refused for a failed save")
		}
		breakAndRetry(t, f)
		var acted []string
		for _, call := range []string{"cmd mdadm --stop ",
			"--zero-superblock", examineOf(f.leg2),
			"cmd mdadm --assemble"} {
			if f.node.hasCall(call) {
				acted = append(acted, call)
			}
		}
		if len(acted) != 0 {
			t.Errorf("the retry attempt acted on the mark whose clear was "+
				"refused, with %q:\n%s", acted,
				strings.Join(f.node.Calls(), "\n"))
		}
		if _, code := f.read(t, 4); !strings.Contains(code.GetDetails(),
			namedMark) {
			t.Errorf("the read's verdict is code %d (%q), want the held "+
				"mark named", code.GetCode(), code.GetDetails())
		}
	})
	t.Run("a request at the held revision", func(t *testing.T) {
		f, st, _ := marked(t)
		refused(t, f, st, withMark(quietLevel(4), testDataLeg2))
		if !st.reqFromRpc {
			t.Error("a refused request at the held revision took the " +
				"trust from the held request")
		}
		assertForcedStart(t, f, breakAndRetry(t, f))
	})
	t.Run("a request whose save holds", func(t *testing.T) {
		f, st, fault := marked(t)
		// The request after the operator's keeps the mark set at
		// revision 4.
		req := quietLevel(5)
		dataGrpOf(req).ForcedStart = &pb.ForcedStart{
			LegId: testDataLeg2, Revision: 4}
		refused(t, f, st, req)
		fault.mu.Lock()
		clear(fault.paths)
		fault.mu.Unlock()
		f.node.Reset()
		f.syncup(t, req)
		f.assertMarkLeftWaiting(t, f.leg2)
		if got := st.loadReq().GetRevision(); got != 5 {
			t.Fatalf("the held request is at revision %d, want 5", got)
		}
		if !st.reqFromRpc {
			t.Error("the request whose save held does not count as " +
				"delivered")
		}
		assertForcedStart(t, f, breakAndRetry(t, f))
		if _, code := f.read(t, 5); strings.Contains(code.GetDetails(),
			namedMark) {
			t.Errorf("the read after the request whose save held names "+
				"the mark: code %d (%q)", code.GetCode(), code.GetDetails())
		}
	})
}

// TestARefusedNewerCntlrSaveStopsTheThinIdSweep: the other effect of the rule
// the test above pins. CN14's activation sweep deletes from a slice's pool
// every thin id the held request's td_list does not name, and runs only
// under a request that counts as delivered (cntlrState.reqFromRpc; CN8). A
// SyncupCntlr newer than the held request, refused for a failed save
// (dnagent.md SH5), takes that trust back, so no converge sweeps until a
// SyncupCntlr whose save holds arrives: the refused request may drop a td the
// held one names, or add one whose id the held one does not, and the held
// td_list is then no longer the newest this cntlr accepted. The sweep is
// owed here because the dump of the converge that created the pool failed,
// which leaves the slice armed. The request is then sent again and its save
// holds, and its converge deletes the stray id once.
func TestARefusedNewerCntlrSaveStopsTheThinIdSweep(t *testing.T) {
	srv, node := newTestServer(t)
	pool := poolName(srv)
	node.holdThinIds(pool, sweepLiveDevId, sweepStrayDevId)
	scriptDump(srv, node, sweepDump(sweepLiveDevId, sweepStrayDevId))
	node.failCmd["cmd thin_dump"] = "thin_dump: metadata device is busy"
	tds := []*pb.ThinDevice{createdTd(testTd, sweepLiveDevId)}
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true, tds: tds})
	st := fixtureCntlrState(t, srv)
	if !st.reqFromRpc || !sweepPending(srv, testSlice) ||
		!node.thinPools[pool][sweepStrayDevId] {
		t.Fatalf("fixture is wrong: delivered %v, sweep owed %v, stray "+
			"held %v; want all three", st.reqFromRpc,
			sweepPending(srv, testSlice), node.thinPools[pool][sweepStrayDevId])
	}
	delete(node.failCmd, "cmd thin_dump")

	path := cntlrPathOf(srv, cntlrPtr())
	fault := failSaves(srv, node, path)
	node.Reset()
	reply, err := srv.SyncupCntlr(context.Background(),
		cntlrReq(reqOpts{revision: 3, primary: true, tds: tds}))
	if err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	assertSaveRefused(t, "SyncupCntlr", reply.GetAgentReply(),
		reply.GetRevision(), 2)
	assertOnlyFailedWrite(t, node, path)
	if st.reqFromRpc {
		t.Error("the held request still counts as delivered after a " +
			"newer request was refused for a failed save")
	}

	// Two CN10 retry attempts converge the held request; neither sweeps.
	retryAttempt(t, srv)
	retryAttempt(t, srv)
	for _, fragment := range []string{
		"cmd dmsetup message " + pool + " 0 reserve_metadata_snap",
		"cmd thin_dump",
		"cmd dmsetup message " + pool + " 0 delete ",
	} {
		if n := callCnt(node, fragment); n != 0 {
			t.Errorf("%d %q calls under the held request after the "+
				"refusal, want none\ncalls:\n%s", n, fragment,
				strings.Join(node.Calls(), "\n"))
		}
	}
	if !node.thinPools[pool][sweepStrayDevId] {
		t.Error("the stray id left the pool under the held request")
	}
	if !sweepPending(srv, testSlice) {
		t.Error("the converges under the held request disarmed the slice")
	}

	// The request again, and its save holds: its converge sweeps once.
	fault.mu.Lock()
	clear(fault.paths)
	fault.mu.Unlock()
	node.Reset()
	syncupCntlrAt(t, srv, reqOpts{revision: 3, primary: true, tds: tds})
	if !st.reqFromRpc {
		t.Error("the request whose save held does not count as delivered")
	}
	if n := callCnt(node, deleteMsg(pool, sweepStrayDevId)); n != 1 {
		t.Errorf("%d deletes of the stray id, want exactly 1\ncalls:\n%s",
			n, strings.Join(node.Calls(), "\n"))
	}
	if n := callCnt(node, deleteMsg(pool, sweepLiveDevId)); n != 0 {
		t.Errorf("the sweep deleted the live td's id (%d messages)", n)
	}
	if node.thinPools[pool][sweepStrayDevId] {
		t.Error("the stray id is still in the pool after the sweep")
	}
	if sweepPending(srv, testSlice) {
		t.Error("the completed sweep left the slice armed")
	}

	// A further retry attempt under the delivered request sweeps nothing.
	node.Reset()
	retryAttempt(t, srv)
	if n := callCnt(node, "cmd thin_dump"); n != 0 {
		t.Errorf("%d dumps after the sweep completed, want none", n)
	}
}
