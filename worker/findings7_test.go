package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// otherPool is the fixture SP's state at revision, its name now holding
// another pool's record, as after a delete and a create under the name.
func otherPool(revision uint64) *model.SpState {
	next := spFixture()
	next.SpRevision = revision
	next.Conf = proto.Clone(next.Conf).(*pb.SpConf)
	next.Conf.SpId = testSpId + 1
	return next
}

// TestAPoolOfAnotherIdIsGone pins RW14's load of the coordinator's pool by
// its name: a record of another sp_id is the coordinator's pool gone, so
// the fan-out sends the new pool's state to no agent, logging the pool as
// being deleted, and the reaction pass's load reads it as gone too.
func TestAPoolOfAnotherIdIsGone(t *testing.T) {
	t.Run("the fan-out", func(t *testing.T) {
		h := newSpHarness(t)
		h.addFixtureAgents()
		w := h.start()
		syncedAtN(t, h)
		const bump = testSpRev + 1
		h.ops.setState(otherPool(bump))
		w.update(desiredState{revision: bump, handle: testSpName})
		waitFor(t, "the pool read as being deleted", func() bool {
			return len(h.logs.withMsg(msgSpDeleting)) > 0
		})
		time.Sleep(50 * time.Millisecond)
		for _, addr := range []string{spCnA, spCnB, spCnC} {
			if cntlrSentAt(h.cntlrs[addr], bump) {
				t.Fatalf("cntlr at %s was sent the other pool's state", addr)
			}
		}
	})
	t.Run("the pass's load", func(t *testing.T) {
		captureLogs(t)
		d := newTestDeps(
			testConfig(common.WorkerRoleSp), newFakeStore(), newFakeClock(),
		)
		w := spTestWorker(d)
		w.ops = &fakeSpOps{state: otherPool(testSpRev)}
		if _, ok := w.loadPassState(context.Background()); ok {
			t.Fatalf("the pass loaded another pool's state")
		}
	})
}

// TestANodeRecordOfAnotherIdIsMissing pins RW13 and HL1 for a loop whose
// node was deleted and whose address another node took: the record at the
// address names another id, so the dn and cn loops send no syncup, logging
// the record missing, and the health monitors' load reads it as missing.
func TestANodeRecordOfAnotherIdIsMissing(t *testing.T) {
	t.Run("the dn loop", func(t *testing.T) {
		h := newRevHarness(t)
		stub := &stubDnAgent{
			checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
				return &pb.CheckDnReply{Revision: 0}
			},
		}
		h.fleet.addDn(t, testAddr, stub)
		h.defaultConf()
		other := dnTestConf()
		other.DnId = testDnId + 1
		h.seedDnConf(testAddr, other)
		h.startDn(testAddr, 3)
		waitFor(t, "dn conf missing", func() bool {
			return len(h.logs.withMsg("dn conf missing")) >= 1
		})
		if got := len(stub.syncups()); got != 0 {
			t.Fatalf("%d syncups over another node's record", got)
		}
	})
	t.Run("the cn loop", func(t *testing.T) {
		h := newRevHarness(t)
		stub := &stubCnAgent{
			checkReply: func(req *pb.CheckCnRequest) *pb.CheckCnReply {
				return &pb.CheckCnReply{Revision: 0}
			},
		}
		h.fleet.addCn(t, testCnAddr, stub)
		h.defaultConf()
		other := cnTestConf()
		other.CnId = testCnId + 1
		h.store.seed(t, model.CnConfKey(testCid, testCnAddr), other)
		h.startCn(testCnAddr, 2)
		waitFor(t, "cn conf missing", func() bool {
			return len(h.logs.withMsg("cn conf missing")) >= 1
		})
		if got := len(stub.syncups()); got != 0 {
			t.Fatalf("%d syncups over another node's record", got)
		}
	})
	t.Run("the monitors' load", func(t *testing.T) {
		captureLogs(t)
		store := newFakeStore()
		d := newTestDeps(testConfig(common.WorkerRoleDn), store,
			newFakeClock())
		otherDn := dnTestConf()
		otherDn.DnId = testDnId + 1
		store.seed(t, model.DnConfKey(testCid, testAddr), otherDn)
		otherCn := cnTestConf()
		otherCn.CnId = testCnId + 1
		store.seed(t, model.CnConfKey(testCid, testCnAddr), otherCn)
		ctx := context.Background()
		dnMon := newDnMonitor(d, testCid, testDnId,
			func() string { return testAddr }, nil)
		if _, found, err := dnMon.load(ctx); err != nil || found {
			t.Fatalf("the dn monitor's load: found %v, err %v", found, err)
		}
		cnMon := newCnMonitor(d, testCid, testCnId,
			func() string { return testCnAddr })
		if _, found, err := cnMon.load(ctx); err != nil || found {
			t.Fatalf("the cn monitor's load: found %v, err %v", found, err)
		}
	})
}

// TestShardRescanKeepsAKeyWithAMalformedValue pins SW4: a rescan that finds
// a known key whose value does not decode keeps the key's running loop, its
// desired state unchanged, as a present key; a key the rescan does not find
// is still stopped.
func TestShardRescanKeepsAKeyWithAMalformedValue(t *testing.T) {
	h := newShardHarness(t)
	badKey := model.DnRevKey(testShard, 1, 10)
	goneKey := model.DnRevKey(testShard, 1, 11)
	h.store.seed(t, badKey, &pb.DnRev{AddrPort: "dn0:9520", Revision: 7})
	h.store.seed(t, goneKey, &pb.DnRev{AddrPort: "dn1:9520", Revision: 1})
	h.start()
	waitFor(t, "two workers", func() bool { return h.rec.startCount() == 2 })
	kept := h.rec.get(1, 10)
	gone := h.rec.get(1, 11)

	// One byte that starts a varint and never ends it does not decode.
	h.store.mu.Lock()
	h.store.kvs[badKey] = []byte{0xff}
	delete(h.store.kvs, goneKey)
	h.store.mu.Unlock()
	h.store.breakWatches(errors.New("watch closed"))

	waitFor(t, "the rescan stops the gone key's worker", func() bool {
		_, stopped := gone.snapshot()
		return stopped == 1
	})
	waitFor(t, "watch restarted", func() bool { return h.store.watchCount() > 0 })
	if updates, stopped := kept.snapshot(); stopped != 0 || len(updates) != 0 {
		t.Fatalf("the malformed key's worker: %d updates, stopped %d; want "+
			"it kept as it was", len(updates), stopped)
	}
}

// TestASyncupReplyAfterTheStopIsNotJudged pins RW11's graceful stop: the
// in-flight SyncupDn finishes and its result is logged, but its reply,
// which carries an ERROR row, is judged no further once the loop has been
// stopped, so no health write runs under the stop's cancelled context.
func TestASyncupReplyAfterTheStopIsNotJudged(t *testing.T) {
	h := newRevHarness(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	stub := &stubDnAgent{
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			return &pb.CheckDnReply{Revision: req.GetRevision()}
		},
		syncupReply: func(req *pb.SyncupDnRequest) (*pb.SyncupDnReply, error) {
			entered <- struct{}{}
			<-release
			return &pb.SyncupDnReply{
				Revision: req.GetRevision(),
				DnInfo: &pb.DnInfo{DiskInfo: &pb.ResInfo{
					Status:  pb.ResStatus_RES_STATUS_ERROR,
					Details: "disk gone",
				}},
			}, nil
		},
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	w := h.startDn(testAddr, 1)
	waitFor(t, "first check", func() bool { return stub.checkCount() >= 1 })
	// The first round's clean verdict, which may land after its check.
	waitFor(t, "first verdict", func() bool { return len(h.hw.all()) >= 1 })
	writes := len(h.hw.all())
	w.update(desiredState{revision: 2, handle: testAddr})
	<-entered
	stopped := make(chan struct{})
	go func() {
		w.stop()
		close(stopped)
	}()
	waitFor(t, "the stop", func() bool { return w.ctx.Err() != nil })
	close(release)
	<-stopped
	if got := len(h.logs.withMsg(msgSyncupResult)); got == 0 {
		t.Fatalf("the in-flight syncup produced no result record")
	}
	for _, write := range h.hw.all()[writes:] {
		if write.record == healthRecordDn {
			t.Fatalf("a reply after the stop wrote health: %+v", write)
		}
	}
}

// TestAnUnchangedDesiredStateKeepsTheRound pins RW3 inside a round: a
// desired state equal to the one the loop holds, as a parent hands every
// child on a tick, does not abandon the round waiting for its reply, so a
// frozen agent's round still runs to its timeout; its stream stays open and
// no syncup is sent.
func TestAnUnchangedDesiredStateKeepsTheRound(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{
		checkReply: func(*pb.CheckDnRequest) *pb.CheckDnReply { return nil },
		holdRound:  true,
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, &pb.DnConf{DnId: testDnId})
	w := h.startDn(testAddr, 1)
	waitFor(t, "first check", func() bool { return stub.checkCount() >= 1 })
	waitFor(t, "the round's stream", func() bool {
		return stub.lastStreamCtx() != nil
	})
	sctx := stub.lastStreamCtx()
	for range 3 {
		w.update(desiredState{revision: 1, handle: testAddr})
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if sctx.Err() != nil {
		t.Fatalf("an unchanged desired state abandoned the round")
	}
	if got := len(stub.syncups()); got != 0 {
		t.Fatalf("%d syncups for an unchanged desired state", got)
	}
}
