package dnagent

import (
	"context"
	"errors"
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
// A request whose save fails is refused (SH5)
// ---------------------------------------------------------------------------

// noSpace is the error text of a save to a full store disk.
const noSpace = "no space left on device"

// saveFault makes the saves of the local store to some paths fail, as a full
// store disk does, and records each save it refused.
type saveFault struct {
	mu       sync.Mutex
	attempts []string
}

// failSaves puts a store on srv whose saves to every path match accepts fail
// with noSpace; every other write goes to the fake node as before. The store
// is a new one, so its latest save has not failed until one of its own does.
func failSaves(
	srv *DnAgentServer,
	node *fakeNode,
	match func(path string) bool,
) *saveFault {
	fault := &saveFault{}
	oc := node.osClient()
	write := oc.WriteProtoFn
	oc.WriteProtoFn = func(
		ctx context.Context, p string, msg proto.Message,
	) error {
		if match(p) {
			fault.mu.Lock()
			fault.attempts = append(fault.attempts, p)
			fault.mu.Unlock()
			return errors.New("write " + p + ": " + noSpace)
		}
		return write(ctx, p, msg)
	}
	srv.store = agent.NewStore(oc, common.DefaultLocalStorPrefix)
	return fault
}

// Attempts lists the paths of the saves the fault refused, in order.
func (f *saveFault) Attempts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.attempts...)
}

// onlyPath matches path alone.
func onlyPath(path string) func(string) bool {
	return func(p string) bool { return p == path }
}

// everyPath matches every path: the whole store disk is full.
func everyPath(string) bool { return true }

// storedDnRevision is the revision of the test DN's request in the local
// store, read from the fake node itself; ok is false when no dn file exists.
func storedDnRevision(
	t *testing.T,
	srv *DnAgentServer,
	node *fakeNode,
) (uint64, bool) {
	t.Helper()
	path := srv.nf.LocalDnPath(testCluster, testDn)
	node.mu.Lock()
	raw, ok := node.protos[path]
	node.mu.Unlock()
	if !ok {
		return 0, false
	}
	req := &pb.SyncupDnRequest{}
	if err := proto.Unmarshal(raw, req); err != nil {
		t.Fatalf("the dn file does not decode: %v", err)
	}
	return req.GetRevision(), true
}

// assertSaveFailed checks a refusal for a failed save: ReplyCodeSaveFailed,
// the revision the agent held before the call, and the save's error in the
// details.
func assertSaveFailed(
	t *testing.T,
	reply *pb.AgentReply,
	revision uint64,
	held uint64,
) {
	t.Helper()
	if reply.GetCode() != common.ReplyCodeSaveFailed {
		t.Errorf("reply code %d (%q), want ReplyCodeSaveFailed",
			reply.GetCode(), reply.GetDetails())
	}
	if revision != held {
		t.Errorf("reply revision %d, want the held %d", revision, held)
	}
	if !strings.Contains(reply.GetDetails(), noSpace) {
		t.Errorf("reply details %q do not name the save's error",
			reply.GetDetails())
	}
}

// assertNoMutation fails the test when the node recorded a call that
// changes it since its last Reset. A save the fault refused is not
// recorded.
func assertNoMutation(t *testing.T, node *fakeNode, what string) {
	t.Helper()
	if muts := node.Mutations(); len(muts) != 0 {
		t.Errorf("%s changed the node:\n%s", what, strings.Join(muts, "\n"))
	}
}

// legB is the leg of the second side of the tests below, so that its
// exports are its own and not the fixture side's.
const legB = testLeg + 1

// A SyncupDn whose save fails is refused (SH5, DN4): the reply carries
// ReplyCodeSaveFailed at the revision the agent held, zero when it held
// none, and no rows. The request does not become the desired state: the dn
// file and the held request keep the last accepted revision, the round
// answers it from the observation before the call, and a side the refused
// request named stays unknown (DN8), so nothing is built for it. A DN the
// agent did not hold stays unknown, with nothing built on the node.
func TestASyncupDnWhoseSaveFailsIsRefused(t *testing.T) {
	ctx := context.Background()
	t.Run("a held dn", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		dnPath := srv.nf.LocalDnPath(testCluster, testDn)
		fault := failSaves(srv, node, onlyPath(dnPath))
		before := dnObs(t, srv)
		ptrB := sweepSidePtr(legB, testSide2)

		node.Reset()
		reply, err := srv.SyncupDn(traced("syncup"),
			sweepDnReq(2, sidePtr(testSide), ptrB))
		if err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		assertSaveFailed(t, reply.GetAgentReply(), reply.GetRevision(), 1)
		if reply.GetDnInfo() != nil || reply.GetObsStamp() != nil ||
			reply.GetSaveTime() != nil {
			t.Errorf("the refusal carries info %v, stamp %v, save time %v; "+
				"want none", reply.GetDnInfo(), reply.GetObsStamp(),
				reply.GetSaveTime())
		}
		if got := fault.Attempts(); len(got) != 1 || got[0] != dnPath {
			t.Errorf("save attempts %v, want the one of %s", got, dnPath)
		}
		assertNoMutation(t, node, "the refused SyncupDn")
		if got, ok := storedDnRevision(t, srv, node); !ok || got != 1 {
			t.Errorf("the dn file holds revision %d (present %v), want 1",
				got, ok)
		}
		if got := srv.getDn(dnKey(testCluster, testDn)).req.Load().
			GetRevision(); got != 1 {
			t.Errorf("the agent holds revision %d, want 1", got)
		}
		if got := dnObs(t, srv); !got.Time.Equal(before.Time) ||
			got.TraceId != before.TraceId {
			t.Errorf("the refused SyncupDn stored an observation: trace "+
				"%q at rev %d", got.TraceId, got.Revision)
		}
		if got := dnRound(srv, "round"); got.GetRevision() != 1 ||
			got.GetAgentReply().GetCode() != 0 {
			t.Errorf("the round answers code %d at revision %d, want 0 "+
				"at 1", got.GetAgentReply().GetCode(), got.GetRevision())
		}

		node.Reset()
		sideReply, err := srv.SyncupSide(ctx, func() *pb.SyncupSideRequest {
			req := sweepSideReq(2, ptrB)
			req.SideConf.Provisioned = false
			return req
		}())
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if got := sideReply.GetAgentReply().GetCode(); got !=
			common.ReplyCodeUnknownObject {
			t.Errorf("the new side's SyncupSide answered code %d (%q), "+
				"want unknown object", got,
				sideReply.GetAgentReply().GetDetails())
		}
		assertNoMutation(t, node, "the new side's SyncupSide")
		if srv.getSide(sideKey(testCluster, testDn, testSp,
			testSide2)) != nil {
			t.Error("the agent holds the new side")
		}
	})

	// A refused request that leaves a held side out of the list drops
	// nothing (DN6): the agent still holds the side, its side file and its
	// lock entry stay, and nothing is removed from the node.
	t.Run("a request that leaves a held side out", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		key := sideKey(testCluster, testDn, testSp, testSide)
		sidePath := srv.nf.LocalSidePath(
			testCluster, testDn, testSp, testSide)
		failSaves(srv, node, onlyPath(srv.nf.LocalDnPath(
			testCluster, testDn)))
		entries := srv.locks.ObjCount()

		node.Reset()
		reply, err := srv.SyncupDn(ctx, dnReq(2))
		if err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		assertSaveFailed(t, reply.GetAgentReply(), reply.GetRevision(), 1)
		assertNoMutation(t, node, "the refused SyncupDn")
		if srv.getSide(key) == nil {
			t.Error("the refused SyncupDn dropped the side")
		}
		node.mu.Lock()
		_, stored := node.protos[sidePath]
		node.mu.Unlock()
		if !stored {
			t.Error("the refused SyncupDn removed the side file")
		}
		if got := srv.locks.ObjCount(); got != entries {
			t.Errorf("%d lock entries after the refused SyncupDn, want %d",
				got, entries)
		}
	})

	// A refused request over a leftover runs no sweep (DN6): the side
	// device a failed removal left on the node stays, and nothing is
	// removed or retried.
	t.Run("a held dn with a leftover", func(t *testing.T) {
		srv, node := newTestServer(t)
		if _, err := srv.SyncupDn(ctx,
			dnReq(1, testSide, testSide2)); err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		for _, side := range []uint64{testSide, testSide2} {
			syncupSideTwoPhase(t, srv, sideReq(1, side, testCn0,
				[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
		}
		gone := srv.nf.DnSideName(testCluster, testDn, testSp, testSide2)
		setFailAlways(node, "dmsetup remove "+gone, "device is busy")
		if reply, err := srv.SyncupDn(ctx, dnReq(2, testSide)); err != nil ||
			reply.GetAgentReply().GetCode() != common.ReplyCodeLeftover {
			t.Fatalf("fixture is wrong: the SyncupDn that drops side 2 "+
				"left no leftover: %v %v", reply.GetAgentReply(), err)
		}
		failSaves(srv, node, onlyPath(srv.nf.LocalDnPath(
			testCluster, testDn)))

		node.Reset()
		reply, err := srv.SyncupDn(ctx, dnReq(3, testSide))
		if err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		assertSaveFailed(t, reply.GetAgentReply(), reply.GetRevision(), 2)
		assertNoMutation(t, node, "the refused SyncupDn")
		if !dmPresent(node, gone) {
			t.Errorf("the leftover side device %s is gone", gone)
		}
	})

	t.Run("a dn the agent does not hold", func(t *testing.T) {
		srv, node := newTestServer(t)
		dnPath := srv.nf.LocalDnPath(testCluster, testDn)
		fault := failSaves(srv, node, onlyPath(dnPath))

		node.Reset()
		reply, err := srv.SyncupDn(ctx, dnReq(1, testSide))
		if err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		assertSaveFailed(t, reply.GetAgentReply(), reply.GetRevision(), 0)
		if reply.GetDnInfo() != nil || reply.GetObsStamp() != nil ||
			reply.GetSaveTime() != nil {
			t.Errorf("the refusal carries info %v, stamp %v, save time %v; "+
				"want none", reply.GetDnInfo(), reply.GetObsStamp(),
				reply.GetSaveTime())
		}
		if got := fault.Attempts(); len(got) != 1 {
			t.Errorf("save attempts %v, want one", got)
		}
		assertNoMutation(t, node, "the refused SyncupDn")
		if _, ok := storedDnRevision(t, srv, node); ok {
			t.Error("a dn file exists after the refused SyncupDn")
		}
		if srv.getDn(dnKey(testCluster, testDn)) != nil {
			t.Error("the agent holds the dn after the refused SyncupDn")
		}
		if got := dnRound(srv, "round").GetAgentReply().GetCode(); got !=
			common.ReplyCodeUnknownObject {
			t.Errorf("the round answers code %d, want unknown object", got)
		}
	})
}

// A SyncupSide whose save fails is refused (SH5, DN8, DN14): the reply
// carries ReplyCodeSaveFailed at the revision the agent held, zero when it
// held none, and no rows. No converge runs, so the node is left as the last
// accepted request left it, the side file and the held request keep that
// request, and the observation is the one before the call. A side the agent
// did not hold stays unknown, with nothing allocated or built for it.
func TestASyncupSideWhoseSaveFailsIsRefused(t *testing.T) {
	ctx := context.Background()
	key := sideKey(testCluster, testDn, testSp, testSide)
	t.Run("a held side", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		sidePath := srv.nf.LocalSidePath(
			testCluster, testDn, testSp, testSide)
		fault := failSaves(srv, node, onlyPath(sidePath))
		before := sideObs(t, srv)

		node.Reset()
		reply, err := srv.SyncupSide(traced("syncup"), failoverReq(2))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		assertSaveFailed(t, reply.GetAgentReply(), reply.GetRevision(), 1)
		if reply.GetSideInfo() != nil || reply.GetBmInfo() != nil ||
			reply.GetObsStamp() != nil || reply.GetSaveTime() != nil {
			t.Errorf("the refusal carries info %v, bm info %v, stamp %v, "+
				"save time %v; want none", reply.GetSideInfo(),
				reply.GetBmInfo(), reply.GetObsStamp(), reply.GetSaveTime())
		}
		if got := fault.Attempts(); len(got) != 1 || got[0] != sidePath {
			t.Errorf("save attempts %v, want the one of %s", got, sidePath)
		}
		assertNoMutation(t, node, "the refused SyncupSide")
		if got := storedSideRevision(t, srv, node); got != 1 {
			t.Errorf("the side file holds revision %d, want 1", got)
		}
		if got := srv.getSide(key).req.Load().GetRevision(); got != 1 {
			t.Errorf("the agent holds revision %d, want 1", got)
		}
		if got := sideObs(t, srv); got.Revision != 1 ||
			!got.Time.Equal(before.Time) || got.TraceId != before.TraceId {
			t.Errorf("the observation is trace %q at rev %d, want the one "+
				"before the refused SyncupSide", got.TraceId, got.Revision)
		}
		if got := sideRound(srv, "round").GetRevision(); got != 1 {
			t.Errorf("the round answers revision %d, want 1", got)
		}
	})

	t.Run("a side the agent does not hold", func(t *testing.T) {
		srv, node := newTestServer(t)
		if reply, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil ||
			reply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("SyncupDn: %v %v", reply.GetAgentReply(), err)
		}
		sidePath := srv.nf.LocalSidePath(
			testCluster, testDn, testSp, testSide)
		fault := failSaves(srv, node, onlyPath(sidePath))

		node.Reset()
		reply, err := srv.SyncupSide(ctx, unprovisionedSideReq(1, testSide,
			testCn0, []uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		assertSaveFailed(t, reply.GetAgentReply(), reply.GetRevision(), 0)
		if reply.GetSideInfo() != nil || reply.GetObsStamp() != nil ||
			reply.GetSaveTime() != nil {
			t.Errorf("the refusal carries info %v, stamp %v, save time %v; "+
				"want none", reply.GetSideInfo(), reply.GetObsStamp(),
				reply.GetSaveTime())
		}
		if got := fault.Attempts(); len(got) != 1 {
			t.Errorf("save attempts %v, want one", got)
		}
		assertNoMutation(t, node, "the refused SyncupSide")
		if srv.getSide(key) != nil {
			t.Error("the agent holds the side after the refused SyncupSide")
		}
		if _, ok, err := srv.meta.LookupSide(
			ctx, testSp, testSide); err != nil || ok {
			t.Errorf("the side has a record after the refused SyncupSide "+
				"(%v, %v)", ok, err)
		}
		node.mu.Lock()
		_, stored := node.protos[sidePath]
		node.mu.Unlock()
		if stored {
			t.Error("a side file exists after the refused SyncupSide")
		}
		if got := sideRound(srv, "round").GetAgentReply().GetCode(); got !=
			common.ReplyCodeUnknownObject {
			t.Errorf("the round answers code %d, want unknown object", got)
		}
	})
}

// A SyncupDn whose save fails cannot lose a live side to the next restart
// (SH5, DN2, DN6). The dn file holds the side list of the last request the
// agent accepted, and DN2 reads it as the authoritative list, so a side the
// agent built for a SyncupDn whose save failed would leave the list at the
// restart, and the sweep would remove its devices and free its record. The
// refused SyncupDn keeps the side unknown, so its own SyncupSide builds
// nothing; every side the agent reported built before the restart keeps its
// devices and its record after it.
func TestAFailedDnSaveLosesNoSideOnARestart(t *testing.T) {
	srv, node := newTestServer(t)
	nf := srv.nf
	ctx := context.Background()
	// built names the sides the agent reported built: an accepted reply
	// whose side device row reads OK.
	built := map[uint64]*pb.SidePointer{}
	reportsBuilt := func(reply *pb.SyncupSideReply) bool {
		return reply.GetAgentReply().GetCode() == 0 &&
			reply.GetSideInfo().GetSideDevInfo().GetStatus() ==
				pb.ResStatus_RES_STATUS_OK
	}

	// Side A: saved and provisioned.
	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if reply := syncupSideTwoPhase(t, srv, sideReq(1, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE)); reportsBuilt(
		reply) {
		built[testSide] = sidePtr(testSide)
	} else {
		t.Fatalf("fixture is wrong: side A is not built: %v",
			reply.GetSideInfo())
	}

	// A SyncupDn adds side B, and its save fails.
	ptrB := sweepSidePtr(legB, testSide2)
	failSaves(srv, node, onlyPath(nf.LocalDnPath(testCluster, testDn)))
	dnReply, err := srv.SyncupDn(ctx, sweepDnReq(2, sidePtr(testSide), ptrB))
	if err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	if got := dnReply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeSaveFailed {
		t.Errorf("the SyncupDn whose save failed answered code %d, want "+
			"ReplyCodeSaveFailed", got)
	}

	// B's own SyncupSide follows, as the worker sends it: provisioning
	// first, then the provisioned request once its zeroing is done.
	reqB := sweepSideReq(2, ptrB)
	phase1 := proto.Clone(reqB).(*pb.SyncupSideRequest)
	phase1.SideConf.Provisioned = false
	reply, err := srv.SyncupSide(ctx, phase1)
	if err != nil {
		t.Fatalf("SyncupSide: %v", err)
	}
	if got := reply.GetAgentReply().GetCode(); got !=
		common.ReplyCodeUnknownObject {
		t.Errorf("side B's SyncupSide answered code %d, want unknown object",
			got)
	}
	if reply.GetAgentReply().GetCode() == 0 {
		waitZeroed(t, srv, testSide2)
		if reply, err = srv.SyncupSide(ctx, reqB); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if reportsBuilt(reply) {
			built[testSide2] = ptrB
		}
	}

	// The kernel state and the local store survive; the process does not.
	stopTestServer(t, srv)
	node.Reset()
	restarted := startTestServer(t, node)

	for sideId, ptr := range built {
		sideDev := nf.DnSideName(testCluster, testDn, testSp, sideId)
		if !dmPresent(node, sideDev) {
			t.Errorf("side %#x, reported built, lost its side device %s "+
				"in the restart", sideId, sideDev)
		}
		for _, cnId := range []uint64{testCn0, testCn1} {
			lin := nf.DnLinearName(testCluster, testDn, testSp, sideId, cnId)
			if !dmPresent(node, lin) {
				t.Errorf("side %#x, reported built, lost its dm-linear %s "+
					"in the restart", sideId, lin)
			}
			nqn := nf.SideToCnNqn(testCluster, testSp, ptr.GetLegId(), cnId)
			if !subsysPresent(node, nqn) {
				t.Errorf("side %#x, reported built, lost its export %s in "+
					"the restart", sideId, nqn)
			}
		}
		if _, ok, err := restarted.meta.LookupSide(
			ctx, testSp, sideId); err != nil || !ok {
			t.Errorf("side %#x, reported built, lost its record in the "+
				"restart (%v, %v)", sideId, ok, err)
		}
		if restarted.getSide(sideKey(testCluster, testDn, testSp,
			sideId)) == nil {
			t.Errorf("the restarted agent does not hold side %#x, "+
				"reported built", sideId)
		}
	}
}

// While the store's latest save failed, as SaveFailing (agent/store.go)
// reports, a syncup the gates admit marks its own object alone and cancels
// no read (SH30), its object's own included. The worker re-sends a request
// the agent refused for a failed save on every round at a newer revision,
// and at the held revision while the object's rounds answer a non-zero code
// (SH5), and nothing knows before the save that it will fail; were each
// re-send to cancel the reads, or a SyncupDn's to mark the node, the
// observations of objects with no fault would go too old (dnv-worker.md
// HL7). The syncup waits for the running reads, which store their
// observations, and is then refused for its own failed save.
func TestASyncupWhileSavesFailCancelsNoRead(t *testing.T) {
	sideK := sideKey(testCluster, testDn, testSp, testSide)
	dnK := dnKey(testCluster, testDn)
	// watchReads returns what is closed once every read the rounds started
	// has ended. It starts before the syncup does, so that a read the
	// syncup cancels is seen even when the syncup ends, and its mark with
	// it, before markedWhileReadsRun looks.
	watchReads := func(srv *DnAgentServer) <-chan struct{} {
		readsDone := make(chan struct{})
		go func() {
			srv.reads.Wait()
			close(readsDone)
		}()
		return readsDone
	}
	// markedWhileReadsRun waits until the syncup marks key and then 150 ms
	// more, and fails the test as soon as a read ends in that time.
	markedWhileReadsRun := func(
		t *testing.T,
		srv *DnAgentServer,
		key string,
		readsDone <-chan struct{},
	) {
		t.Helper()
		marked := func() bool {
			srv.mu.Lock()
			defer srv.mu.Unlock()
			return !srv.marks.SinceLocked(key).IsZero()
		}
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		deadline := time.After(5 * time.Second)
		for !marked() {
			select {
			case <-readsDone:
				t.Fatal("the syncup cancelled a read while saves fail")
			case <-deadline:
				t.Fatal("timed out waiting for the syncup to mark its object")
			case <-tick.C:
			}
		}
		select {
		case <-readsDone:
			t.Fatal("the syncup cancelled a read while saves fail")
		case <-time.After(150 * time.Millisecond):
		}
	}

	// The DN's read and the side's read run while a SyncupDn arrives, at a
	// newer revision or at the held one: both run on, and only the DN is
	// marked.
	for _, tc := range []struct {
		name     string
		revision uint64
	}{
		{"SyncupDn", 3},
		{"SyncupDn at the held revision", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, 1, testSide)
			failSaves(srv, node, everyPath)
			if _, err := srv.SyncupDn(traced("syncup-0"),
				dnReq(2, testSide)); err != nil {
				t.Fatalf("SyncupDn: %v", err)
			}
			if !srv.store.SaveFailing() {
				t.Fatal("fixture is wrong: the store's latest save did " +
					"not fail")
			}
			srv.roundReads = true
			node.Reset()
			release := softGate(t, node, "dmsetup ls")
			dnRound(srv, "dn-round")
			sideRound(srv, "side-round")
			waitUntil(t, "both reads to park in their listing", func() bool {
				return len(node.callsMatching("cmd dmsetup ls")) == 2
			})
			readsDone := watchReads(srv)
			done := make(chan *pb.SyncupDnReply, 1)
			go func() {
				reply, _ := srv.SyncupDn(traced("syncup-1"),
					dnReq(tc.revision, testSide))
				done <- reply
			}()
			markedWhileReadsRun(t, srv, dnK, readsDone)
			if !dnReading(srv) || !sideReading(srv) {
				t.Errorf("dn reading %v, side reading %v; want both",
					dnReading(srv), sideReading(srv))
			}
			srv.mu.Lock()
			sideQuiet := srv.marks.QuietLocked(sideK)
			srv.mu.Unlock()
			if !sideQuiet {
				t.Error("the SyncupDn marked the node or the side")
			}
			release()
			select {
			case reply := <-done:
				assertSaveFailed(t, reply.GetAgentReply(),
					reply.GetRevision(), 1)
			case <-time.After(5 * time.Second):
				t.Fatal("the SyncupDn never returned")
			}
			joinReads(t, srv)
			<-readsDone
			if got := dnObs(t, srv).TraceId; got != "dn-round" {
				t.Errorf("the dn observation is %q's, want the read's", got)
			}
			if got := sideObs(t, srv).TraceId; got != "side-round" {
				t.Errorf("the side observation is %q's, want the read's",
					got)
			}
			srv.mu.Lock()
			quiet := srv.marks.QuietLocked(dnK) &&
				srv.marks.QuietLocked(sideK)
			srv.mu.Unlock()
			if !quiet {
				t.Error("the refused SyncupDn left a mark")
			}
		})
	}

	// The side's read runs while a SyncupSide at a newer revision arrives:
	// it runs on and stores its observation.
	t.Run("SyncupSide", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		failSaves(srv, node, everyPath)
		if _, err := srv.SyncupSide(traced("syncup-0"),
			failoverReq(2)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if !srv.store.SaveFailing() {
			t.Fatal("fixture is wrong: the store's latest save did not fail")
		}
		srv.roundReads = true
		node.Reset()
		release := softGate(t, node, "dmsetup ls")
		sideRound(srv, "side-round")
		waitUntil(t, "the read to park in its listing", func() bool {
			return len(node.callsMatching("cmd dmsetup ls")) == 1
		})
		readsDone := watchReads(srv)
		done := make(chan *pb.SyncupSideReply, 1)
		go func() {
			reply, _ := srv.SyncupSide(traced("syncup-1"), sideReq(3,
				testSide, testCn0, []uint64{testCn1},
				pb.SpLevel_SP_LEVEL_READWRITE))
			done <- reply
		}()
		markedWhileReadsRun(t, srv, sideK, readsDone)
		if !sideReading(srv) {
			t.Error("the side's read does not run")
		}
		release()
		select {
		case reply := <-done:
			assertSaveFailed(t, reply.GetAgentReply(), reply.GetRevision(), 1)
		case <-time.After(5 * time.Second):
			t.Fatal("the SyncupSide never returned")
		}
		joinReads(t, srv)
		<-readsDone
		if got := sideObs(t, srv).TraceId; got != "side-round" {
			t.Errorf("the side observation is %q's, want the read's", got)
		}
		srv.mu.Lock()
		quiet := srv.marks.QuietLocked(sideK)
		srv.mu.Unlock()
		if !quiet {
			t.Error("the refused SyncupSide left a mark")
		}
	})
}
