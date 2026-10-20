package cnagent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// The observations of dnagent.md SH28 to SH30 on the cn: every converge end
// stores one, a round and an info read answer from it alone, a round starts
// one background read of its object while no read and no converge of it run,
// and a syncup cancels a read before it waits for a lock. The tests that
// start reads turn the server's round reads back on (newCnServerOnPort turns
// them off) and join them (srv.reads) before they look at the node.

// obsBound is how long a call that must not wait for a lock or a read is
// given before the test calls it a wait.
const obsBound = 2 * time.Second

// metaLegAttr is the command a converge of the primary=false fixture runs
// first on its meta leg's wrapper, where the tests park a converge.
func metaLegAttr(srv *CnAgentServer) string {
	return "cmd dmsetup info --columns --noheadings -o attr " +
		legName(srv, testMetaLeg)
}

func fixtureCntlrState(t *testing.T, srv *CnAgentServer) *cntlrState {
	t.Helper()
	st := srv.getCntlr(cntlrKey(testCluster, testCn, testSp, testCntlr))
	if st == nil {
		t.Fatalf("the fixture cntlr is not known")
	}
	return st
}

func fixtureCnState(t *testing.T, srv *CnAgentServer) *cnState {
	t.Helper()
	st := srv.getCn(cnKey(testCluster, testCn))
	if st == nil {
		t.Fatalf("the fixture cn is not known")
	}
	return st
}

// latestCntlrObs and latestCnObs are the fixture objects' stored
// observations, read under srv.mu as a round reads them.
func latestCntlrObs(t *testing.T, srv *CnAgentServer) cntlrObs {
	t.Helper()
	st := fixtureCntlrState(t, srv)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return st.obs.LatestLocked()
}

func latestCnObs(t *testing.T, srv *CnAgentServer) cnObs {
	t.Helper()
	st := fixtureCnState(t, srv)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return st.obs.LatestLocked()
}

// cntlrReadRuns and cnReadRuns say whether a read of the fixture object runs.
func cntlrReadRuns(t *testing.T, srv *CnAgentServer) bool {
	t.Helper()
	st := fixtureCntlrState(t, srv)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return st.obs.ReadingLocked()
}

func cnReadRuns(t *testing.T, srv *CnAgentServer) bool {
	t.Helper()
	st := fixtureCnState(t, srv)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return st.obs.ReadingLocked()
}

// joinReads waits for every read a round started to end.
func joinReads(t *testing.T, srv *CnAgentServer) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		srv.reads.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a background read never ended")
	}
}

// within runs f off the test goroutine and fails the test when f has not
// returned within obsBound.
func within(t *testing.T, label string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(obsBound):
		t.Fatalf("%s waited", label)
	}
}

// openGate parks every command whose line contains key until the func it
// returns is called; the test's cleanup calls it too, so a test that failed
// early leaves no goroutine parked.
func openGate(t *testing.T, node *fakeNode, key string) func() {
	t.Helper()
	gate := make(chan struct{})
	node.mu.Lock()
	node.gate[key] = gate
	node.mu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return release
}

// dropGate stops the gate of key parking new commands; those it parks
// already stay parked until it is released.
func dropGate(node *fakeNode, key string) {
	node.mu.Lock()
	defer node.mu.Unlock()
	delete(node.gate, key)
}

func traced(traceId string) context.Context {
	return common.WithTraceId(context.Background(), traceId)
}

// roundCntlr and roundCn are one round of the fixture objects with
// show_info set, under traceId.
func roundCntlr(
	srv *CnAgentServer,
	traceId string,
	revision uint64,
) *pb.CheckCntlrReply {
	reply, _ := srv.checkCntlrRound(traced(traceId), &pb.CheckCntlrRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr(),
		Revision: revision, ShowInfo: true,
	}, nil)
	return reply
}

func roundCn(
	srv *CnAgentServer,
	traceId string,
	revision uint64,
) *pb.CheckCnReply {
	reply, _ := srv.checkCnRound(traced(traceId), &pb.CheckCnRequest{
		ClusterId: testCluster, CnId: testCn, Revision: revision,
		ShowInfo: true,
	}, nil)
	return reply
}

func cntlrInfoNow(t *testing.T, srv *CnAgentServer) *pb.GetCntlrInfoReply {
	t.Helper()
	reply, err := srv.GetCntlrInfo(context.Background(),
		&pb.GetCntlrInfoRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr()})
	if err != nil {
		t.Fatalf("GetCntlrInfo: %v", err)
	}
	return reply
}

func cnInfoNow(t *testing.T, srv *CnAgentServer) *pb.GetCnInfoReply {
	t.Helper()
	reply, err := srv.GetCnInfo(context.Background(),
		&pb.GetCnInfoRequest{ClusterId: testCluster, CnId: testCn})
	if err != nil {
		t.Fatalf("GetCnInfo: %v", err)
	}
	return reply
}

// stampTime is a stamp's time as the agent stored it, zero when unset.
func stampTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

// assertStamp checks a reply's stamp: the observation's time and trace id,
// the answer time and the converge start (zero: unset).
func assertStamp(
	t *testing.T,
	label string,
	stamp *pb.ObsStamp,
	obsTime time.Time,
	traceId string,
	answer time.Time,
	convergeStart time.Time,
) {
	t.Helper()
	if got := stampTime(stamp.GetObsTime()); !got.Equal(obsTime) {
		t.Errorf("%s: observation time %v, want %v", label, got, obsTime)
	}
	if got := stamp.GetObsTraceId(); got != traceId {
		t.Errorf("%s: observation trace id %q, want %q", label, got, traceId)
	}
	if got := stampTime(stamp.GetAnswerTime()); !got.Equal(answer) {
		t.Errorf("%s: answer time %v, want %v", label, got, answer)
	}
	if got := stampTime(stamp.GetConvergeStart()); !got.Equal(convergeStart) {
		t.Errorf("%s: converge start %v, want %v", label, got, convergeStart)
	}
}

// TestRoundReadsDefault pins the production constructor: rounds start their
// reads (dnagent.md SH29), and the marks a round reads exist. Every other
// test builds its server through newCnServerOnPort, which turns the reads
// off, so only this test sees what NewCnAgentServer leaves.
func TestRoundReadsDefault(t *testing.T) {
	srv := NewCnAgentServer(newFakeNode().osClient(),
		common.NewNameFmt(common.DefaultLocalStorPrefix),
		common.DefaultLocalStorPrefix, testCapacity, testTrConf(),
		common.NvmetPortId)
	if !srv.roundReads {
		t.Error("NewCnAgentServer leaves round reads off")
	}
	if srv.marks == nil {
		t.Error("NewCnAgentServer builds no converge marks")
	}
}

// TestEveryConvergeEndStoresAnObservation: a SyncupCn's and a SyncupCntlr's
// converge, a retry attempt's and the startup reconcile's each store the
// object's observation before their locks go (dnagent.md SH28): the rows and
// the code the converge's reply carries, the revision it converged, the
// agent's clock at its end and the trace id of its call. A syncup's reply
// carries that observation's stamp with no converge start, and the time it
// saved the request; the round after it answers from that observation and
// reads nothing of the node.
func TestEveryConvergeEndStoresAnObservation(t *testing.T) {
	t.Run("a SyncupCn and a SyncupCntlr", func(t *testing.T) {
		srv, node := newTestServer(t)
		clock := withClock(srv)
		ctx := traced("syncup")
		cnReply, err := srv.SyncupCn(ctx, cnReq(2, true))
		if err != nil || cnReply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("SyncupCn: %v %v", err, cnReply.GetAgentReply())
		}
		cntlrReply, err := srv.SyncupCntlr(ctx,
			cntlrReq(reqOpts{revision: 2, primary: true}))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		at := clock.Now()

		cn := latestCnObs(t, srv)
		if cn.Revision != 2 || !cn.Time.Equal(at) || cn.TraceId != "syncup" ||
			!proto.Equal(cn.Info, cnReply.GetCnInfo()) ||
			!proto.Equal(cn.Reply, cnReply.GetAgentReply()) {
			t.Errorf("the SyncupCn stored %+v", cn)
		}
		assertStamp(t, "the SyncupCn reply", cnReply.GetObsStamp(),
			at, "syncup", at, time.Time{})
		if got := stampTime(cnReply.GetSaveTime()); !got.Equal(at) {
			t.Errorf("the SyncupCn reply's save time %v, want %v", got, at)
		}
		cntlr := latestCntlrObs(t, srv)
		if cntlr.Revision != 2 || !cntlr.Time.Equal(at) ||
			cntlr.TraceId != "syncup" ||
			!proto.Equal(cntlr.Info, cntlrReply.GetCntlrInfo()) ||
			!proto.Equal(cntlr.Reply, cntlrReply.GetAgentReply()) {
			t.Errorf("the SyncupCntlr stored %+v", cntlr)
		}
		assertStamp(t, "the SyncupCntlr reply", cntlrReply.GetObsStamp(),
			at, "syncup", at, time.Time{})
		if got := stampTime(cntlrReply.GetSaveTime()); !got.Equal(at) {
			t.Errorf("the SyncupCntlr reply's save time %v, want %v", got, at)
		}

		clock.advance(time.Second)
		node.Reset()
		cnRound := roundCn(srv, "round", 2)
		if cnRound.GetRevision() != 2 ||
			!proto.Equal(cnRound.GetCnInfo(), cnReply.GetCnInfo()) ||
			cnRound.GetAgentReply().GetCode() !=
				cnReply.GetAgentReply().GetCode() {
			t.Errorf("the CheckCn round after the syncup answered %v", cnRound)
		}
		assertStamp(t, "the CheckCn round", cnRound.GetObsStamp(),
			at, "syncup", clock.Now(), time.Time{})
		cntlrRound := roundCntlr(srv, "round", 2)
		if cntlrRound.GetRevision() != 2 ||
			!proto.Equal(cntlrRound.GetCntlrInfo(),
				cntlrReply.GetCntlrInfo()) ||
			cntlrRound.GetAgentReply().GetCode() !=
				cntlrReply.GetAgentReply().GetCode() {
			t.Errorf("the CheckCntlr round after the syncup answered %v",
				cntlrRound)
		}
		assertStamp(t, "the CheckCntlr round", cntlrRound.GetObsStamp(),
			at, "syncup", clock.Now(), time.Time{})
		if calls := node.Calls(); len(calls) != 0 {
			t.Errorf("the rounds read the node: %q", calls)
		}
	})

	t.Run("a retry attempt", func(t *testing.T) {
		srv, _ := newTestServer(t)
		clock := withClock(srv)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
		syncup := latestCntlrObs(t, srv)
		clock.advance(time.Second)
		retryAttempt(t, srv)
		got := latestCntlrObs(t, srv)
		if got.Revision != 2 || !got.Time.Equal(clock.Now()) ||
			got.Info == nil || got.TraceId == "" ||
			got.TraceId == syncup.TraceId {
			t.Errorf("the attempt stored %+v after the syncup's %+v",
				got, syncup)
		}
		assertStamp(t, "the round after the attempt",
			roundCntlr(srv, "round", 2).GetObsStamp(),
			got.Time, got.TraceId, clock.Now(), time.Time{})
	})

	t.Run("the startup reconcile", func(t *testing.T) {
		srv, _ := newTestServer(t)
		clock := withClock(srv)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
		clock.advance(time.Second)
		if err := srv.Reconcile(
			common.WithTraceId(t.Context(), "startup")); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		cn := latestCnObs(t, srv)
		if cn.Revision != 2 || !cn.Time.Equal(clock.Now()) ||
			cn.TraceId != "startup" || cn.Info == nil {
			t.Errorf("the reconcile stored the cn's %+v", cn)
		}
		cntlr := latestCntlrObs(t, srv)
		if cntlr.Revision != 2 || !cntlr.Time.Equal(clock.Now()) ||
			cntlr.TraceId != "startup" || cntlr.Info == nil {
			t.Errorf("the reconcile stored the cntlr's %+v", cntlr)
		}
	})
}

// TestACutOffConvergeStoresNothing: a syncup whose call is cut off in the
// middle of its converge stores no observation (dnagent.md SH28): every
// command after the cut fails at once, so its rows would tell of the cut. The
// object keeps the observation it had, and the reply's stamp is that one.
func TestACutOffConvergeStoresNothing(t *testing.T) {
	t.Run("a SyncupCntlr", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: false})
		before := latestCntlrObs(t, srv)
		ctx, cancel := context.WithCancel(traced("cut"))
		defer cancel()
		node.mu.Lock()
		node.cancelCmd[metaLegAttr(srv)] = cancel
		node.mu.Unlock()
		reply, err := srv.SyncupCntlr(ctx,
			cntlrReq(reqOpts{revision: 3, primary: false}))
		if err != nil {
			t.Fatalf("SyncupCntlr: %v", err)
		}
		if !node.hasCall(metaLegAttr(srv)) {
			t.Fatalf("the converge never reached the cut")
		}
		if got := latestCntlrObs(t, srv); !got.Time.Equal(before.Time) ||
			got.TraceId != before.TraceId || got.Revision != 2 {
			t.Errorf("the cut-off syncup stored %+v over %+v", got, before)
		}
		if got := reply.GetObsStamp().GetObsTraceId(); got != before.TraceId {
			t.Errorf("the cut-off reply's stamp names %q, want %q",
				got, before.TraceId)
		}
	})

	t.Run("a SyncupCn", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: false})
		before := latestCnObs(t, srv)
		ctx, cancel := context.WithCancel(traced("cut"))
		defer cancel()
		node.mu.Lock()
		node.cancelCmd[portProbeLine] = cancel
		node.mu.Unlock()
		if _, err := srv.SyncupCn(ctx, cnReq(3, true)); err != nil {
			t.Fatalf("SyncupCn: %v", err)
		}
		if got := latestCnObs(t, srv); !got.Time.Equal(before.Time) ||
			got.TraceId != before.TraceId || got.Revision != 2 {
			t.Errorf("the cut-off syncup stored %+v over %+v", got, before)
		}
	})
}

// TestAnObjectWithNoObservationAnswersAZeroTime: a cntlr the agent holds but
// has no observation of yet — its first syncup was cut off — is answered by a
// round and an info read with no observation time, revision zero, a clean
// code and no info, and an unknown one with the refusal; both with the
// agent's answer time (dnagent.md SH25, SH28).
func TestAnObjectWithNoObservationAnswersAZeroTime(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.retryInterval = time.Hour
	clock := withClock(srv)
	cnSyncup(t, srv, 1, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := srv.SyncupCntlr(ctx,
		cntlrReq(reqOpts{revision: 2, primary: true})); err != nil {
		t.Fatalf("SyncupCntlr: %v", err)
	}
	fixtureCntlrState(t, srv)

	round := roundCntlr(srv, "round", 2)
	if round.GetAgentReply().GetCode() != 0 || round.GetRevision() != 0 ||
		round.GetCntlrInfo() != nil {
		t.Errorf("a cntlr with no observation answered %v", round)
	}
	assertStamp(t, "the round", round.GetObsStamp(),
		time.Time{}, "", clock.Now(), time.Time{})
	info := cntlrInfoNow(t, srv)
	if info.GetAgentReply().GetCode() != 0 || info.GetRevision() != 0 ||
		info.GetCntlrInfo() != nil {
		t.Errorf("an info read of a cntlr with no observation answered %v",
			info)
	}
	assertStamp(t, "the info read", info.GetObsStamp(),
		time.Time{}, "", clock.Now(), time.Time{})

	unknown, _ := srv.checkCntlrRound(context.Background(),
		&pb.CheckCntlrRequest{ClusterId: testCluster, CnId: testCn,
			CntlrPointer: cntlrPtr2(), Revision: 2}, nil)
	if unknown.GetAgentReply().GetCode() != common.ReplyCodeUnknownObject {
		t.Errorf("an unknown cntlr answered %v", unknown.GetAgentReply())
	}
	assertStamp(t, "the unknown cntlr's round", unknown.GetObsStamp(),
		time.Time{}, "", clock.Now(), time.Time{})
}

// TestARoundBesideAConvergeAnswersFromMemory: while a converge of the object
// runs — a SyncupCntlr, a retry attempt, a SyncupCn — and holds its locks, a
// round and an info read answer at once from the observation from before it,
// at that observation's revision, with the converge's start, and read
// nothing; and no round starts a read until the converge's mark ends
// (dnagent.md SH28, SH29, SH30). Once the converge has ended, the info read
// answers from its observation, at its revision, with no converge start.
func TestARoundBesideAConvergeAnswersFromMemory(t *testing.T) {
	for _, tc := range []struct {
		name string
		// gate is the command the converge is parked in.
		gate func(srv *CnAgentServer) string
		// converge runs the converge, and the revision it converges.
		converge func(t *testing.T, srv *CnAgentServer) uint64
	}{
		{"a SyncupCntlr", metaLegAttr,
			func(t *testing.T, srv *CnAgentServer) uint64 {
				if _, err := srv.SyncupCntlr(traced("syncup"),
					cntlrReq(reqOpts{revision: 3})); err != nil {
					t.Errorf("SyncupCntlr: %v", err)
				}
				return 3
			}},
		{"a retry attempt", metaLegAttr,
			func(t *testing.T, srv *CnAgentServer) uint64 {
				srv.reconvergeCntlr(srv.rootCtx,
					cntlrKey(testCluster, testCn, testSp, testCntlr),
					srv.getCntlr(cntlrKey(
						testCluster, testCn, testSp, testCntlr)))
				return 2
			}},
		{"a SyncupCn", func(*CnAgentServer) string { return portProbeLine },
			func(t *testing.T, srv *CnAgentServer) uint64 {
				if _, err := srv.SyncupCn(traced("syncup"),
					cnReq(3, true)); err != nil {
					t.Errorf("SyncupCn: %v", err)
				}
				return 3
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			clock := withClock(srv)
			syncupBoth(t, srv, reqOpts{revision: 2})
			srv.roundReads = true
			cnBefore, cntlrBefore := latestCnObs(t, srv),
				latestCntlrObs(t, srv)
			nodeLevel := tc.name == "a SyncupCn"

			release := openGate(t, node, tc.gate(srv))
			clock.advance(time.Second)
			start := clock.Now()
			node.Reset()
			done := make(chan uint64, 1)
			go func() { done <- tc.converge(t, srv) }()
			waitFor(t, func() bool { return node.hasCall(tc.gate(srv)) })
			clock.advance(time.Second)
			node.Reset()

			cntlrStart, cnStart := start, time.Time{}
			if nodeLevel {
				cntlrStart, cnStart = time.Time{}, start
			}
			within(t, "a CheckCntlr round", func() {
				reply := roundCntlr(srv, "round", 2)
				if reply.GetRevision() != 2 ||
					!proto.Equal(reply.GetCntlrInfo(), cntlrBefore.Info) {
					t.Errorf("the CheckCntlr round answered %v", reply)
				}
				assertStamp(t, "the CheckCntlr round", reply.GetObsStamp(),
					cntlrBefore.Time, cntlrBefore.TraceId, clock.Now(),
					cntlrStart)
			})
			within(t, "a GetCntlrInfo", func() {
				reply := cntlrInfoNow(t, srv)
				if reply.GetRevision() != 2 ||
					!proto.Equal(reply.GetCntlrInfo(), cntlrBefore.Info) {
					t.Errorf("the GetCntlrInfo answered %v", reply)
				}
				assertStamp(t, "the GetCntlrInfo", reply.GetObsStamp(),
					cntlrBefore.Time, cntlrBefore.TraceId, clock.Now(),
					cntlrStart)
			})
			if calls := node.Calls(); len(calls) != 0 {
				t.Errorf("the cntlr's answers beside a converge read the "+
					"node: %q", calls)
			}
			// A CheckCn round beside a cntlr's converge starts the CN's
			// read, which a cntlr's converge, under the node read lock,
			// leaves room for; beside a SyncupCn it starts none.
			within(t, "a CheckCn round", func() {
				reply := roundCn(srv, "round", 2)
				if reply.GetRevision() != 2 ||
					!proto.Equal(reply.GetCnInfo(), cnBefore.Info) {
					t.Errorf("the CheckCn round answered %v", reply)
				}
				assertStamp(t, "the CheckCn round", reply.GetObsStamp(),
					cnBefore.Time, cnBefore.TraceId, clock.Now(), cnStart)
			})
			within(t, "a GetCnInfo", func() {
				reply := cnInfoNow(t, srv)
				if reply.GetRevision() != 2 ||
					!proto.Equal(reply.GetCnInfo(), cnBefore.Info) {
					t.Errorf("the GetCnInfo answered %v", reply)
				}
				assertStamp(t, "the GetCnInfo", reply.GetObsStamp(),
					cnBefore.Time, cnBefore.TraceId, clock.Now(), cnStart)
			})
			if cntlrReadRuns(t, srv) || nodeLevel && cnReadRuns(t, srv) {
				t.Errorf("a round started a read beside the converge")
			}

			release()
			var revision uint64
			select {
			case revision = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the converge never ended")
			}
			joinReads(t, srv)
			if got := countCalls(node, dmTableListLine); got != 0 {
				t.Errorf("a cntlr read ran beside the converge: %d readings",
					got)
			}
			if nodeLevel {
				reply := cnInfoNow(t, srv)
				if reply.GetRevision() != revision {
					t.Errorf("the GetCnInfo after the syncup answered "+
						"revision %d, want %d", reply.GetRevision(), revision)
				}
				assertStamp(t, "the GetCnInfo after the syncup",
					reply.GetObsStamp(), clock.Now(), "syncup", clock.Now(),
					time.Time{})
				return
			}
			after := latestCntlrObs(t, srv)
			reply := cntlrInfoNow(t, srv)
			if reply.GetRevision() != revision ||
				!after.Time.After(cntlrBefore.Time) {
				t.Errorf("the GetCntlrInfo after the converge answered "+
					"revision %d at %v, want %d after %v",
					reply.GetRevision(), after.Time, revision,
					cntlrBefore.Time)
			}
			assertStamp(t, "the GetCntlrInfo after the converge",
				reply.GetObsStamp(), after.Time, after.TraceId, clock.Now(),
				time.Time{})
		})
	}
}

// TestARoundStartsOneRead: a round starts the object's background read under
// its own trace id, and no other round starts a second one while it runs; the
// read stores its observation when it ends, and the next round answers from
// it and starts the next read (dnagent.md SH29).
func TestARoundStartsOneRead(t *testing.T) {
	t.Run("a cntlr", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2})
		syncup := latestCntlrObs(t, srv)
		srv.roundReads = true
		release := openGate(t, node, dmTableListLine)
		node.Reset()

		first := roundCntlr(srv, "round-1", 2)
		if first.GetObsStamp().GetObsTraceId() != syncup.TraceId {
			t.Errorf("the first round answered %v, not the syncup's",
				first.GetObsStamp())
		}
		waitFor(t, func() bool { return node.hasCall(dmTableListLine) })
		roundCntlr(srv, "round-2", 2)
		cntlrInfoNow(t, srv)
		roundCntlr(srv, "round-3", 2)
		if !cntlrReadRuns(t, srv) {
			t.Errorf("no read runs after the round")
		}
		release()
		joinReads(t, srv)
		if got := countCalls(node, dmTableListLine); got != 1 {
			t.Errorf("%d readings for three rounds beside one read, want 1",
				got)
		}
		read := latestCntlrObs(t, srv)
		if read.TraceId != "round-1" || read.Revision != 2 ||
			!read.Time.After(syncup.Time) {
			t.Errorf("the read stored %+v after the syncup's %+v",
				read, syncup)
		}

		next := roundCntlr(srv, "round-4", 2)
		if next.GetObsStamp().GetObsTraceId() != "round-1" {
			t.Errorf("the next round answered %v, not the read's",
				next.GetObsStamp())
		}
		joinReads(t, srv)
		if got := countCalls(node, dmTableListLine); got != 2 {
			t.Errorf("%d readings after the next round, want 2", got)
		}
		if got := latestCntlrObs(t, srv).TraceId; got != "round-4" {
			t.Errorf("the next read stored under %q, want round-4", got)
		}
	})

	t.Run("a cn", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2})
		syncup := latestCnObs(t, srv)
		srv.roundReads = true
		// The port probe's first read, once per read.
		probe := portProbeLine
		release := openGate(t, node, probe)
		node.Reset()

		roundCn(srv, "round-1", 2)
		waitFor(t, func() bool { return node.hasCall(probe) })
		roundCn(srv, "round-2", 2)
		cnInfoNow(t, srv)
		if !cnReadRuns(t, srv) {
			t.Errorf("no read runs after the round")
		}
		release()
		joinReads(t, srv)
		if got := len(node.callsMatching(probe)); got != 1 {
			t.Errorf("%d port probes for two rounds beside one read, "+
				"want 1", got)
		}
		read := latestCnObs(t, srv)
		if read.TraceId != "round-1" || read.Revision != 2 ||
			!read.Time.After(syncup.Time) {
			t.Errorf("the read stored %+v after the syncup's %+v",
				read, syncup)
		}
	})
}

// TestAnInfoReadStartsNoRead: GetCnInfo and GetCntlrInfo answer from the
// observation and start no read, whatever the rounds do (dnagent.md SH28).
func TestAnInfoReadStartsNoRead(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2})
	srv.roundReads = true
	node.Reset()
	cnInfoNow(t, srv)
	cntlrInfoNow(t, srv)
	if cnReadRuns(t, srv) || cntlrReadRuns(t, srv) {
		t.Errorf("an info read started a read")
	}
	joinReads(t, srv)
	if calls := node.Calls(); len(calls) != 0 {
		t.Errorf("the info reads read the node: %q", calls)
	}
}

// TestASyncupCancelsTheReadBeforeItsLock: a SyncupCntlr that arrives while
// the cntlr's read holds the object lock, parked in a command, cancels the
// read before it waits for the lock, so it does not wait behind the read;
// the cancelled read stores nothing, and the object's observation is the
// syncup's (dnagent.md SH29, SH30).
func TestASyncupCancelsTheReadBeforeItsLock(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2})
	srv.roundReads = true
	release := openGate(t, node, dmTableListLine)
	node.Reset()
	roundCntlr(srv, "round", 2)
	waitFor(t, func() bool { return node.hasCall(dmTableListLine) })

	within(t, "a SyncupCntlr behind a read", func() {
		reply, err := srv.SyncupCntlr(traced("syncup"),
			cntlrReq(reqOpts{revision: 3}))
		if err != nil || reply.GetAgentReply().GetCode() != 0 {
			t.Errorf("SyncupCntlr: %v %v", err, reply.GetAgentReply())
		}
	})
	release()
	joinReads(t, srv)
	if got := latestCntlrObs(t, srv); got.TraceId != "syncup" ||
		got.Revision != 3 {
		t.Errorf("the observation after the syncup is %+v", got)
	}
}

// TestACancelledReadStoresNothing: a read cancelled while it runs — here in a
// command that ignores the cancel, as a child in an uninterruptible wait
// does — stores nothing when it ends, and the object's observation is the
// one from before it; the read ends all the same, so the next round starts
// the next one (dnagent.md SH29).
func TestACancelledReadStoresNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		block   string
		round   func(srv *CnAgentServer, traceId string)
		latest  func(t *testing.T, srv *CnAgentServer) (time.Time, string)
		cancel  func(t *testing.T, srv *CnAgentServer)
		reading func(t *testing.T, srv *CnAgentServer) bool
	}{
		{"a cntlr", dmInfoListLine,
			func(srv *CnAgentServer, traceId string) {
				roundCntlr(srv, traceId, 2)
			},
			func(t *testing.T, srv *CnAgentServer) (time.Time, string) {
				obs := latestCntlrObs(t, srv)
				return obs.Time, obs.TraceId
			},
			func(t *testing.T, srv *CnAgentServer) {
				st := fixtureCntlrState(t, srv)
				srv.mu.Lock()
				defer srv.mu.Unlock()
				st.obs.CancelReadLocked()
			},
			cntlrReadRuns},
		{"a cn", portProbeLine,
			func(srv *CnAgentServer, traceId string) {
				roundCn(srv, traceId, 2)
			},
			func(t *testing.T, srv *CnAgentServer) (time.Time, string) {
				obs := latestCnObs(t, srv)
				return obs.Time, obs.TraceId
			},
			func(t *testing.T, srv *CnAgentServer) {
				st := fixtureCnState(t, srv)
				srv.mu.Lock()
				defer srv.mu.Unlock()
				st.obs.CancelReadLocked()
			},
			cnReadRuns},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2})
			beforeTime, beforeTrace := tc.latest(t, srv)
			srv.roundReads = true
			node.blockCmd(tc.block)
			t.Cleanup(func() { node.releaseCmd(tc.block) })
			node.Reset()
			tc.round(srv, "round")
			waitFor(t, func() bool { return node.hasCall(tc.block) })
			tc.cancel(t, srv)
			node.releaseCmd(tc.block)
			joinReads(t, srv)
			if gotTime, gotTrace := tc.latest(t, srv); !gotTime.Equal(
				beforeTime) || gotTrace != beforeTrace {
				t.Errorf("the cancelled read stored %v %q over %v %q",
					gotTime, gotTrace, beforeTime, beforeTrace)
			}
			if tc.reading(t, srv) {
				t.Fatalf("the cancelled read never ended")
			}
			tc.round(srv, "again")
			joinReads(t, srv)
			if _, got := tc.latest(t, srv); got != "again" {
				t.Errorf("the round after the cancelled read stored under "+
					"%q, want again", got)
			}
		})
	}
}

// nextSecond waits until the wall clock's unix second moves on, so a status
// change from here on gets an epoch later than every epoch made before: the
// trackers count epochs in seconds of the wall clock, not of the server's
// clock seam.
func nextSecond() {
	start := time.Now().Unix()
	for time.Now().Unix() == start {
		time.Sleep(10 * time.Millisecond)
	}
}

// TestACutOffPassMovesNoEpoch: a read or a converge whose ctx is cancelled
// while it runs makes each row after the cut from a command that failed on
// it, and stores no observation (dnagent.md SH28, SH29); those rows move no
// epoch either, so the next read reports every row the node never changed
// with the epoch it had before the cut, and the observation equals the one
// before it (architecture.md, Live-state reporting; agent.ResTracker).
func TestACutOffPassMovesNoEpoch(t *testing.T) {
	cntlrK := cntlrKey(testCluster, testCn, testSp, testCntlr)
	cnK := cnKey(testCluster, testCn)
	cases := []struct {
		name string
		// cut is the line of the command whose run cancels the pass.
		cut  func(srv *CnAgentServer) string
		pass func(t *testing.T, ctx context.Context, srv *CnAgentServer)
		read func(t *testing.T, srv *CnAgentServer) proto.Message
	}{
		{"a cntlr read",
			func(*CnAgentServer) string { return dmInfoListLine },
			func(t *testing.T, ctx context.Context, srv *CnAgentServer) {
				srv.refreshCntlr(ctx, cntlrK, fixtureCntlrState(t, srv))
			},
			func(t *testing.T, srv *CnAgentServer) proto.Message {
				refreshTestCntlr(t, srv)
				return latestCntlrObs(t, srv).Info
			}},
		{"a cn read",
			func(*CnAgentServer) string { return portProbeLine },
			func(t *testing.T, ctx context.Context, srv *CnAgentServer) {
				srv.refreshCn(ctx, cnK, fixtureCnState(t, srv))
			},
			func(t *testing.T, srv *CnAgentServer) proto.Message {
				refreshTestCn(t, srv)
				return latestCnObs(t, srv).Info
			}},
		{"a SyncupCntlr", metaLegAttr,
			func(t *testing.T, ctx context.Context, srv *CnAgentServer) {
				reply, err := srv.SyncupCntlr(ctx,
					cntlrReq(reqOpts{revision: 3, primary: false}))
				if err != nil {
					t.Fatalf("SyncupCntlr: %v", err)
				}
				if len(errorRows(reply.GetCntlrInfo())) == 0 {
					t.Fatalf("fixture is wrong: the cut-off converge made "+
						"no error row: %v", reply.GetCntlrInfo())
				}
			},
			func(t *testing.T, srv *CnAgentServer) proto.Message {
				refreshTestCntlr(t, srv)
				return latestCntlrObs(t, srv).Info
			}},
		{"a SyncupCn",
			func(*CnAgentServer) string { return portProbeLine },
			func(t *testing.T, ctx context.Context, srv *CnAgentServer) {
				reply, err := srv.SyncupCn(ctx, cnReq(3, true))
				if err != nil {
					t.Fatalf("SyncupCn: %v", err)
				}
				if got := reply.GetCnInfo().GetPortInfo().GetStatus(); got !=
					pb.ResStatus_RES_STATUS_ERROR {
					t.Fatalf("fixture is wrong: the cut-off converge's "+
						"port row is %v", got)
				}
			},
			func(t *testing.T, srv *CnAgentServer) proto.Message {
				refreshTestCn(t, srv)
				return latestCnObs(t, srv).Info
			}},
	}
	// Every fixture's rows first, then one wait for the next second for all
	// of them.
	type fixture struct {
		srv    *CnAgentServer
		node   *fakeNode
		before proto.Message
	}
	fixtures := make([]fixture, len(cases))
	for i, tc := range cases {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2, primary: false})
		fixtures[i] = fixture{srv, node, proto.Clone(tc.read(t, srv))}
	}
	nextSecond()
	for i, tc := range cases {
		srv, node, before := fixtures[i].srv, fixtures[i].node,
			fixtures[i].before
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(traced("cut"))
			defer cancel()
			cut := tc.cut(srv)
			node.mu.Lock()
			node.cancelCmd[cut] = cancel
			node.mu.Unlock()
			tc.pass(t, ctx, srv)
			if ctx.Err() == nil {
				t.Fatalf("fixture is wrong: the pass never ran %q", cut)
			}
			if after := tc.read(t, srv); !proto.Equal(after, before) {
				t.Errorf("the read after the cut reports\n%v\nwant, as "+
					"before it,\n%v", after, before)
			}
		})
	}
}

// TestANodeSyncupCancelsEveryRead: a SyncupCn newer than the CN's held
// request cancels the read of its CN and that of every cntlr before it waits
// for the node write lock, which waits for each of them (dnagent.md SH30):
// it does not wait behind either, the cntlr's cancelled read stores nothing,
// and the CN's observation is the syncup's.
func TestANodeSyncupCancelsEveryRead(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2})
	cntlrBefore := latestCntlrObs(t, srv)
	srv.roundReads = true
	releaseCntlr := openGate(t, node, dmTableListLine)
	releaseCn := openGate(t, node, portProbeLine)
	node.Reset()
	roundCn(srv, "cn-round", 2)
	roundCntlr(srv, "cntlr-round", 2)
	waitFor(t, func() bool {
		return node.hasCall(portProbeLine) && node.hasCall(dmTableListLine)
	})
	// The syncup's own converge probes the port too; only the read stays
	// parked in it.
	dropGate(node, portProbeLine)

	within(t, "a SyncupCn behind two reads", func() {
		reply, err := srv.SyncupCn(traced("syncup"), cnReq(3, true))
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
	if got := latestCntlrObs(t, srv); !got.Time.Equal(cntlrBefore.Time) ||
		got.TraceId != cntlrBefore.TraceId {
		t.Errorf("the cancelled cntlr read stored %+v over %+v",
			got, cntlrBefore)
	}
}

// TestARefusedSyncupCancelsNoRead: a SyncupCntlr that one of CN8's gates
// refuses on the request the agent holds for the cntlr — the revision gate,
// the pointer gate or the conf gate — and a SyncupCn the revision gate
// refuses on the CN's held request mark nothing and cancel no read
// (dnagent.md SH30): the worker re-sends such a request every round, and
// were it to cancel each round's read, no read would ever end and the
// observation would go stale (dnv-worker.md HL7). The read runs on to its
// end and stores its observation, the gates refuse the request under its
// locks as before, and a round after it starts a read. A mark another
// converge of the object holds outlives the refused syncup.
func TestARefusedSyncupCancelsNoRead(t *testing.T) {
	cntlrK := cntlrKey(testCluster, testCn, testSp, testCntlr)
	cnK := cnKey(testCluster, testCn)
	staleCntlr := func(srv *CnAgentServer) *pb.AgentReply {
		reply, _ := srv.SyncupCntlr(traced("syncup"),
			cntlrReq(reqOpts{revision: 1}))
		return reply.GetAgentReply()
	}
	// pointerCntlr is a newer SyncupCntlr of the held cntlr while the CN's
	// list lacks its pointer, as the list stands inside a SyncupCn that
	// removes the pointer, between the store of its request and the drop of
	// the cntlr (syncupCn). The list comes back once the syncup has
	// answered.
	pointerCntlr := func(srv *CnAgentServer) *pb.AgentReply {
		st := srv.getCn(cnK)
		held := st.loadReq()
		st.storeReq(cnReq(held.GetRevision(), false))
		defer st.storeReq(held)
		reply, _ := srv.SyncupCntlr(traced("syncup"),
			cntlrReq(reqOpts{revision: 3}))
		return reply.GetAgentReply()
	}
	// confCntlr is a newer SyncupCntlr whose conf CN8's conf gate refuses,
	// as a bad stored conf would bring every round.
	confCntlr := func(srv *CnAgentServer) *pb.AgentReply {
		reply, _ := srv.SyncupCntlr(traced("syncup"), zeroConfReq(
			reqOpts{revision: 3}, func(conf *pb.BdevConf) {
				conf.DmPoolConf.DataBlockSize = 0
			}))
		return reply.GetAgentReply()
	}
	staleCn := func(srv *CnAgentServer) *pb.AgentReply {
		reply, _ := srv.SyncupCn(traced("syncup"), cnReq(1, true))
		return reply.GetAgentReply()
	}
	// quiet checks that none of keys has a mark and no node mark stands.
	quiet := func(t *testing.T, srv *CnAgentServer, keys ...string) {
		t.Helper()
		srv.mu.Lock()
		defer srv.mu.Unlock()
		for _, key := range keys {
			if !srv.marks.QuietLocked(key) {
				t.Errorf("the refused syncup left a mark: %s quiet = false",
					key)
			}
		}
	}
	cntlrRound := func(srv *CnAgentServer, id string) {
		roundCntlr(srv, id, 2)
	}
	cnRound := func(srv *CnAgentServer, id string) { roundCn(srv, id, 2) }
	cntlrRounds := map[string]func(*CnAgentServer, string){
		"cntlr-round": cntlrRound,
	}
	cntlrStored := func(t *testing.T, srv *CnAgentServer) map[string]string {
		return map[string]string{
			"cntlr-round": latestCntlrObs(t, srv).TraceId,
		}
	}

	for _, tc := range []struct {
		name  string
		stale func(srv *CnAgentServer) *pb.AgentReply
		// code is the refusal's.
		code uint32
		// gates park the reads that run beside the syncup, one command
		// line per read.
		gates []string
		// rounds start those reads, by trace id.
		rounds map[string]func(srv *CnAgentServer, traceId string)
		// stored is the trace id each object's observation holds.
		stored func(t *testing.T, srv *CnAgentServer) map[string]string
	}{
		{"SyncupCntlr", staleCntlr, common.ReplyCodeStaleRevision,
			[]string{dmTableListLine}, cntlrRounds, cntlrStored},
		{"SyncupCntlr pointer", pointerCntlr, common.ReplyCodeUnknownObject,
			[]string{dmTableListLine}, cntlrRounds, cntlrStored},
		{"SyncupCntlr conf", confCntlr, common.ReplyCodeInvalidConf,
			[]string{dmTableListLine}, cntlrRounds, cntlrStored},
		{"SyncupCn", staleCn, common.ReplyCodeStaleRevision,
			[]string{dmTableListLine, portProbeLine},
			map[string]func(*CnAgentServer, string){
				"cntlr-round": cntlrRound,
				"cn-round":    cnRound,
			},
			func(t *testing.T, srv *CnAgentServer) map[string]string {
				return map[string]string{
					"cntlr-round": latestCntlrObs(t, srv).TraceId,
					"cn-round":    latestCnObs(t, srv).TraceId,
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, reqOpts{revision: 2})
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
			done := make(chan *pb.AgentReply, 1)
			go func() { done <- tc.stale(srv) }()
			readsDone := make(chan struct{})
			go func() {
				srv.reads.Wait()
				close(readsDone)
			}()
			select {
			case <-readsDone:
				t.Fatal("the refused syncup cancelled a read")
			case <-time.After(150 * time.Millisecond):
			}
			quiet(t, srv, cntlrK, cnK)
			for _, release := range releases {
				release()
			}
			select {
			case reply := <-done:
				if reply.GetCode() != tc.code {
					t.Fatalf("fixture is wrong: the syncup was not refused "+
						"with code %d: %v", tc.code, reply)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the refused syncup never returned")
			}
			joinReads(t, srv)
			// Every Wait returns before the next round's read joins the
			// group.
			<-readsDone
			for id, got := range tc.stored(t, srv) {
				if got != id {
					t.Errorf("the observation is %q's, want the read %q "+
						"stored", got, id)
				}
			}
			cntlrRound(srv, "after")
			joinReads(t, srv)
			if got := latestCntlrObs(t, srv).TraceId; got != "after" {
				t.Errorf("the round after the refused syncup stored no read: "+
					"the observation is %q's", got)
			}
		})
	}

	// The refused syncup ends no mark it did not make: a retry attempt's
	// mark of the cntlr, and an admitted SyncupCn's marks of the CN and the
	// node, stand until their own converge ends them.
	t.Run("another converge's mark", func(t *testing.T) {
		srv, _ := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2})

		endAttempt := srv.markConverge(cntlrK)
		within(t, "a stale SyncupCntlr", func() {
			if got := staleCntlr(srv); got.GetCode() !=
				common.ReplyCodeStaleRevision {
				t.Errorf("fixture is wrong: the syncup was not refused "+
					"as stale: %v", got)
			}
		})
		srv.mu.Lock()
		cntlrSince := srv.marks.SinceLocked(cntlrK)
		srv.mu.Unlock()
		if cntlrSince.IsZero() {
			t.Error("the stale SyncupCntlr ended the attempt's mark")
		}
		endAttempt()

		endSyncup := srv.markNodeSyncup(cnK, 3)
		within(t, "a stale SyncupCn", func() {
			if got := staleCn(srv); got.GetCode() !=
				common.ReplyCodeStaleRevision {
				t.Errorf("fixture is wrong: the syncup was not refused "+
					"as stale: %v", got)
			}
		})
		srv.mu.Lock()
		cnSince := srv.marks.SinceLocked(cnK)
		cntlrQuiet := srv.marks.QuietLocked(cntlrK)
		srv.mu.Unlock()
		if cnSince.IsZero() || cntlrQuiet {
			t.Errorf("the stale SyncupCn ended the admitted one's marks: "+
				"cn since %v, cntlr quiet %v", cnSince, cntlrQuiet)
		}
		endSyncup()
		quiet(t, srv, cntlrK, cnK)
	})
}

// TestAReSentSyncupCnLetsCntlrReadsEnd: a SyncupCn at the revision the agent
// holds for the CN — the re-send the worker issues every round while a
// CN-level leftover lasts (dnv-worker.md RW4) — marks the CN and cancels the
// CN's own read alone (dnagent.md SH30): it cancels no cntlr's read and holds
// no cntlr's read off, so a cntlr with no fault keeps a fresh observation
// however long the leftover lasts (dnv-worker.md HL7). Its write lock waits
// for the cntlr's read in flight, and a cntlr round while it holds that lock
// starts a read that runs once the syncup ends.
func TestAReSentSyncupCnLetsCntlrReadsEnd(t *testing.T) {
	cntlrK := cntlrKey(testCluster, testCn, testSp, testCntlr)
	cnK := cnKey(testCluster, testCn)
	// leftover builds a CN whose node holds a stray ns-dev of an sp no cntlr
	// names while its removal keeps failing, so every SyncupCn at the held
	// revision answers a leftover, and returns the re-send.
	leftover := func(t *testing.T) (*CnAgentServer, *fakeNode,
		func(traceId string) *pb.SyncupCnReply) {
		t.Helper()
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2})
		stray := srv.nf.CnNsDevName(testCluster, testCn, cnSweepStrandedSp,
			testNs)
		seedStrayDm(node, stray, "253:200")
		node.mu.Lock()
		node.failCmdAlways["dmsetup remove "+stray] = "device is busy"
		node.mu.Unlock()
		resend := func(traceId string) *pb.SyncupCnReply {
			reply, _ := srv.SyncupCn(traced(traceId), cnReq(2, true))
			return reply
		}
		if got := resend("syncup-0").GetAgentReply().GetCode(); got !=
			common.ReplyCodeLeftover {
			t.Fatalf("fixture is wrong: the SyncupCn answered code %d, "+
				"want a leftover", got)
		}
		srv.roundReads = true
		return srv, node, resend
	}
	resendDone := func(t *testing.T, done <-chan *pb.SyncupCnReply) {
		t.Helper()
		select {
		case reply := <-done:
			if got := reply.GetAgentReply().GetCode(); got !=
				common.ReplyCodeLeftover {
				t.Fatalf("fixture is wrong: the re-send answered code %d, "+
					"want a leftover", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the re-sent SyncupCn never returned")
		}
	}

	// Round after round, a re-send lands while the cntlr's read is in its
	// table listing: the read runs on and stores, so the cntlr's observation
	// keeps moving forward, while the CN's read is cancelled and the CN's
	// observation is each re-send's own.
	t.Run("a read in flight runs on", func(t *testing.T) {
		srv, node, resend := leftover(t)
		prev := latestCntlrObs(t, srv)
		for i := 1; i <= 3; i++ {
			cntlrId := fmt.Sprintf("cntlr-round-%d", i)
			syncupId := fmt.Sprintf("syncup-%d", i)
			node.Reset()
			releaseCntlr := openGate(t, node, dmTableListLine)
			releaseCn := openGate(t, node, portProbeLine)
			roundCn(srv, fmt.Sprintf("cn-round-%d", i), 2)
			roundCntlr(srv, cntlrId, 2)
			waitFor(t, func() bool {
				return node.hasCall(portProbeLine) &&
					node.hasCall(dmTableListLine)
			})
			// The re-send's own converge probes the port too; only the
			// CN's read stays parked in it.
			dropGate(node, portProbeLine)
			done := make(chan *pb.SyncupCnReply, 1)
			go func() { done <- resend(syncupId) }()
			waitFor(t, func() bool { return !cnReadRuns(t, srv) })
			srv.mu.Lock()
			cnSince := srv.marks.SinceLocked(cnK)
			cntlrQuiet := srv.marks.QuietLocked(cntlrK)
			srv.mu.Unlock()
			if cnSince.IsZero() || !cntlrQuiet {
				t.Errorf("round %d: cn marked %v, cntlr quiet %v; want the "+
					"CN's mark alone", i, !cnSince.IsZero(), cntlrQuiet)
			}
			readsDone := make(chan struct{})
			go func() {
				srv.reads.Wait()
				close(readsDone)
			}()
			select {
			case <-readsDone:
				t.Fatalf("round %d: the re-sent SyncupCn cancelled the "+
					"cntlr's read", i)
			case <-time.After(150 * time.Millisecond):
			}
			releaseCntlr()
			releaseCn()
			resendDone(t, done)
			<-readsDone
			got := latestCntlrObs(t, srv)
			if got.TraceId != cntlrId || !got.Time.After(prev.Time) {
				t.Errorf("round %d: cntlr observation = trace %q at %v, "+
					"want the read %q after %v", i, got.TraceId, got.Time,
					cntlrId, prev.Time)
			}
			prev = got
			if got := latestCnObs(t, srv).TraceId; got != syncupId {
				t.Errorf("round %d: cn observation is %q's, want the "+
					"re-send's", i, got)
			}
		}
	})

	// A cntlr round while the re-send holds the write lock starts a read:
	// no node mark holds it off, and the read waits for the node read lock
	// until the syncup ends, then stores.
	t.Run("a round meanwhile starts a read", func(t *testing.T) {
		srv, node, resend := leftover(t)
		node.Reset()
		release := openGate(t, node, portProbeLine)
		done := make(chan *pb.SyncupCnReply, 1)
		go func() { done <- resend("syncup-1") }()
		waitFor(t, func() bool { return node.hasCall(portProbeLine) })
		roundCntlr(srv, "cntlr-round", 2)
		if !cntlrReadRuns(t, srv) {
			t.Fatal("a cntlr round under the re-send started no read")
		}
		if got := node.Traced("cntlr-round"); len(got) != 0 {
			t.Errorf("the read ran %q under the syncup's write lock", got)
		}
		release()
		resendDone(t, done)
		joinReads(t, srv)
		if got := latestCntlrObs(t, srv).TraceId; got != "cntlr-round" {
			t.Errorf("cntlr observation is %q's, want the read's", got)
		}
	})
}

// TestAReadWhoseListingDidNotAnswerStillStores: a read whose listing did not
// answer stores what it found, its verdict naming the failure, as a round
// once reported it (dnagent.md SH29).
func TestAReadWhoseListingDidNotAnswerStillStores(t *testing.T) {
	srv, node := newTestServer(t)
	clock := withClock(srv)
	syncupBoth(t, srv, reqOpts{revision: 2, primary: true})
	clock.advance(time.Second)

	node.mu.Lock()
	node.killCmdAlways[dmInfoListLine] = true
	node.killCmdAlways["cmd dmsetup ls"] = true
	node.mu.Unlock()
	refreshTestCntlr(t, srv)
	refreshTestCn(t, srv)
	cntlr := latestCntlrObs(t, srv)
	if !cntlr.Time.Equal(clock.Now()) || cntlr.Revision != 2 ||
		cntlr.Info == nil || cntlr.Reply.GetCode() == 0 {
		t.Errorf("a read whose dm listing did not answer stored %+v", cntlr)
	}
	cn := latestCnObs(t, srv)
	if !cn.Time.Equal(clock.Now()) || cn.Revision != 2 || cn.Info == nil ||
		cn.Reply.GetCode() == 0 {
		t.Errorf("a read whose dm listing did not answer stored %+v", cn)
	}
}

// TestDroppingACntlrCancelsItsRead: the drop of a cntlr's state cancels its
// running read and waits for nothing, and a read started for a cntlr that is
// dropped before it gets the node read lock reads nothing and leaves no lock
// entry (dnagent.md SH29, SH30; SH11).
func TestDroppingACntlrCancelsItsRead(t *testing.T) {
	t.Run("the drop cancels the read", func(t *testing.T) {
		srv, _ := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2})
		key := cntlrKey(testCluster, testCn, testSp, testCntlr)
		st := fixtureCntlrState(t, srv)
		srv.mu.Lock()
		readCtx := st.obs.BeginReadLocked(context.Background())
		srv.mu.Unlock()
		srv.locks.Node().Lock()
		srv.dropCntlrState(context.Background(), key, st)
		srv.locks.Node().Unlock()
		if readCtx.Err() == nil {
			t.Errorf("the drop left the cntlr's read running")
		}
		srv.mu.Lock()
		st.obs.EndReadLocked(readCtx)
		srv.mu.Unlock()
	})

	t.Run("a read of a dropped cntlr", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, reqOpts{revision: 2})
		key := cntlrKey(testCluster, testCn, testSp, testCntlr)
		st := fixtureCntlrState(t, srv)
		before := latestCntlrObs(t, srv)
		srv.roundReads = true

		srv.locks.Node().Lock()
		within(t, "a round beside the node write lock", func() {
			roundCntlr(srv, "round", 2)
		})
		// The read reaches the node lock and waits behind the write lock.
		time.Sleep(50 * time.Millisecond)
		srv.dropCntlrState(context.Background(), key, st)
		entries := srv.locks.ObjCount()
		node.Reset()
		srv.locks.Node().Unlock()
		joinReads(t, srv)
		if got := srv.locks.ObjCount(); got != entries {
			t.Errorf("%d lock entries after the read, want %d", got, entries)
		}
		if calls := node.Calls(); len(calls) != 0 {
			t.Errorf("the read of a dropped cntlr ran %q", calls)
		}
		srv.mu.Lock()
		got, reading := st.obs.LatestLocked(), st.obs.ReadingLocked()
		srv.mu.Unlock()
		if !got.Time.Equal(before.Time) || reading {
			t.Errorf("the read of a dropped cntlr stored %+v (running %v)",
				got, reading)
		}
	})
}

// TestAReadEndsWithTheAgent: a read hangs off rootCtx, not off the stream of
// the round that started it, so once the agent's ctx is cancelled a read
// reads nothing and stores nothing; the cn joins none of its reads
// (dnagent.md SH29).
func TestAReadEndsWithTheAgent(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, reqOpts{revision: 2})
	before := latestCntlrObs(t, srv)
	rootCtx, cancel := context.WithCancel(context.Background())
	cancel()
	srv.rootCtx = rootCtx
	srv.roundReads = true
	node.Reset()

	roundCntlr(srv, "round", 2)
	joinReads(t, srv)
	if calls := node.Calls(); len(calls) != 0 {
		t.Errorf("a read on a cancelled agent ran %q", calls)
	}
	if got := latestCntlrObs(t, srv); !got.Time.Equal(before.Time) {
		t.Errorf("a read on a cancelled agent stored %+v", got)
	}

	// A round's stream ending does not end its read.
	srv.rootCtx = t.Context()
	streamCtx, endStream := context.WithCancel(traced("stream"))
	srv.checkCntlrRound(streamCtx, &pb.CheckCntlrRequest{
		ClusterId: testCluster, CnId: testCn, CntlrPointer: cntlrPtr(),
		Revision: 2,
	}, nil)
	endStream()
	joinReads(t, srv)
	if got := latestCntlrObs(t, srv).TraceId; got != "stream" {
		t.Errorf("the read of an ended stream stored under %q, want stream",
			got)
	}
}
