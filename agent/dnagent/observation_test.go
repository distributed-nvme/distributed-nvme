package dnagent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/agent"
	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Observations, the background read and converge marks (SH28-SH30)
// ---------------------------------------------------------------------------

// readDnOf runs the background read (refreshDn) of the DN dnId of cluster
// clusterId to its end, as a round would start it, when the agent holds the
// DN, so the next round or info read of it answers what the node holds now
// (SH29). ctx is the read's: its trace id is the observation's.
func readDnOf(
	ctx context.Context,
	srv *DnAgentServer,
	clusterId uint64,
	dnId uint64,
) {
	key := dnKey(clusterId, dnId)
	if st := srv.getDn(key); st != nil {
		srv.refreshDn(ctx, key, st)
	}
}

// readSideOf is readDnOf for the side ptr names on the DN dnId of cluster
// clusterId (refreshSide).
func readSideOf(
	ctx context.Context,
	srv *DnAgentServer,
	clusterId uint64,
	dnId uint64,
	ptr *pb.SidePointer,
) {
	key := sideKey(clusterId, dnId, ptr.GetSpId(), ptr.GetSideId())
	if st := srv.getSide(key); st != nil {
		srv.refreshSide(ctx, key, st)
	}
}

// readDnNow is readDnOf for the test DN.
func readDnNow(ctx context.Context, srv *DnAgentServer) {
	readDnOf(ctx, srv, testCluster, testDn)
}

// readSideNow is readSideOf for the test DN's side ptr names.
func readSideNow(
	ctx context.Context,
	srv *DnAgentServer,
	ptr *pb.SidePointer,
) {
	readSideOf(ctx, srv, testCluster, testDn, ptr)
}

// checkDnNow is a CheckDn round that answers what the node holds now: the
// read of the DN req names runs to its end under the round's ctx, and then
// the round answers from the observation it stored (SH28, SH29). A test that
// expects a round to show a change made since the DN's last converge, or
// that compares a round with a converge's reply, runs its rounds this way.
func checkDnNow(
	srv *DnAgentServer,
	ctx context.Context,
	req *pb.CheckDnRequest,
	lastSent *pb.DnInfo,
) (*pb.CheckDnReply, *pb.DnInfo) {
	readDnOf(ctx, srv, req.GetClusterId(), req.GetDnId())
	return srv.checkDnRound(ctx, req, lastSent)
}

// checkSideNow is checkDnNow for a CheckSide round.
func checkSideNow(
	srv *DnAgentServer,
	ctx context.Context,
	req *pb.CheckSideRequest,
	lastSent *pb.SideInfo,
) (*pb.CheckSideReply, *pb.SideInfo) {
	readSideOf(ctx, srv, req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer())
	return srv.checkSideRound(ctx, req, lastSent)
}

// dnInfoNow is checkDnNow for GetDnInfo.
func dnInfoNow(
	srv *DnAgentServer,
	ctx context.Context,
	req *pb.GetDnInfoRequest,
) (*pb.GetDnInfoReply, error) {
	readDnOf(ctx, srv, req.GetClusterId(), req.GetDnId())
	return srv.GetDnInfo(ctx, req)
}

// sideInfoNow is checkDnNow for GetSideInfo.
func sideInfoNow(
	srv *DnAgentServer,
	ctx context.Context,
	req *pb.GetSideInfoRequest,
) (*pb.GetSideInfoReply, error) {
	readSideOf(ctx, srv, req.GetClusterId(), req.GetDnId(),
		req.GetSidePointer())
	return srv.GetSideInfo(ctx, req)
}

// dnObs is the test DN's latest observation.
func dnObs(t *testing.T, srv *DnAgentServer) agent.Observation[*pb.DnInfo] {
	t.Helper()
	st := srv.getDn(dnKey(testCluster, testDn))
	if st == nil {
		t.Fatal("the agent does not hold the dn")
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return st.obs.LatestLocked()
}

// sideObs is the fixture side's latest observation.
func sideObs(t *testing.T, srv *DnAgentServer) agent.Observation[*pb.SideInfo] {
	t.Helper()
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	if st == nil {
		t.Fatal("the agent does not hold the side")
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return st.obs.LatestLocked()
}

// sideReading reports whether a read of the fixture side runs.
func sideReading(srv *DnAgentServer) bool {
	st := srv.getSide(sideKey(testCluster, testDn, testSp, testSide))
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return st.obs.ReadingLocked()
}

// dnReading reports whether a read of the test DN runs.
func dnReading(srv *DnAgentServer) bool {
	st := srv.getDn(dnKey(testCluster, testDn))
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return st.obs.ReadingLocked()
}

// traced is a ctx carrying traceId, a round's or a syncup's.
func traced(traceId string) context.Context {
	return common.WithTraceId(context.Background(), traceId)
}

// dnRound runs one CheckDn round of the test DN under traceId.
func dnRound(srv *DnAgentServer, traceId string) *pb.CheckDnReply {
	reply, _ := srv.checkDnRound(traced(traceId), &pb.CheckDnRequest{
		ClusterId: testCluster, DnId: testDn, ShowInfo: true,
	}, nil)
	return reply
}

// sideRound runs one CheckSide round of the fixture side under traceId.
func sideRound(srv *DnAgentServer, traceId string) *pb.CheckSideReply {
	reply, _ := srv.checkSideRound(traced(traceId), &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn,
		SidePointer: sidePtr(testSide), ShowInfo: true,
	}, nil)
	return reply
}

// softGate parks every command whose line contains key until the returned
// release, or until the command's ctx is done, as a child the agent kills on
// a cancel does.
func softGate(t *testing.T, node *fakeNode, key string) func() {
	t.Helper()
	gate := make(chan struct{})
	node.mu.Lock()
	node.gate[key] = gate
	node.mu.Unlock()
	released := false
	release := func() {
		if !released {
			released = true
			close(gate)
		}
	}
	t.Cleanup(release)
	return release
}

// joinReads waits for every read the rounds started to end.
func joinReads(t *testing.T, srv *DnAgentServer) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		srv.reads.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a read never ended")
	}
}

// within fails the test unless call returns within d.
func within(t *testing.T, d time.Duration, what string, call func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		call()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
	}
}

// TestRoundReadsDefault pins the production constructor: rounds start their
// reads (SH29), and the marks a round reads exist.
func TestRoundReadsDefault(t *testing.T) {
	srv := NewDnAgentServer(newFakeNode().osClient(),
		common.NewNameFmt(common.DefaultLocalStorPrefix),
		common.DefaultLocalStorPrefix, testDisk, testTrConf(),
		common.NvmetPortId)
	if !srv.roundReads {
		t.Error("NewDnAgentServer leaves round reads off")
	}
	if srv.marks == nil {
		t.Error("NewDnAgentServer builds no converge marks")
	}
}

// Every converge end stores the object's observation before its locks go
// (SH28): a syncup's, the startup reconcile's, a DN13 retry attempt's and the
// DN12 fence timer's. It carries the converge's own info, code and revision,
// a time and the converge's trace id; a syncup's reply stamps it with no
// converge start, beside the time the request was saved.
func TestEveryConvergeEndStoresAnObservation(t *testing.T) {
	t.Run("SyncupDn", func(t *testing.T) {
		srv, _ := newTestServer(t)
		before := time.Now()
		reply, err := srv.SyncupDn(traced("dn-syncup"), dnReq(1, testSide))
		if err != nil || reply.GetAgentReply().GetCode() != 0 {
			t.Fatalf("SyncupDn: %v %v", reply.GetAgentReply(), err)
		}
		obs := dnObs(t, srv)
		if obs.Revision != 1 || obs.TraceId != "dn-syncup" ||
			!proto.Equal(obs.Info, reply.GetDnInfo()) ||
			!proto.Equal(obs.Reply, reply.GetAgentReply()) {
			t.Errorf("observation = rev %d, trace %q, info %v, reply %v; "+
				"want the converge's", obs.Revision, obs.TraceId, obs.Info,
				obs.Reply)
		}
		if obs.Time.Before(before) {
			t.Errorf("observation time %v is before the call", obs.Time)
		}
		stamp := reply.GetObsStamp()
		if !stamp.GetObsTime().AsTime().Equal(obs.Time) ||
			stamp.GetObsTraceId() != "dn-syncup" ||
			stamp.GetConvergeStart() != nil || stamp.GetAnswerTime() == nil {
			t.Errorf("reply stamp = %v, want the observation's with no "+
				"converge start", stamp)
		}
		if reply.GetSaveTime() == nil ||
			reply.GetSaveTime().AsTime().Before(before) {
			t.Errorf("save time = %v, want the save's", reply.GetSaveTime())
		}
	})

	t.Run("SyncupSide", func(t *testing.T) {
		srv, _ := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		reply, err := srv.SyncupSide(traced("side-syncup"), failoverReq(2))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		obs := sideObs(t, srv)
		if obs.Revision != 2 || obs.TraceId != "side-syncup" ||
			!proto.Equal(obs.Info, reply.GetSideInfo()) ||
			!proto.Equal(obs.Reply, reply.GetAgentReply()) {
			t.Errorf("observation = rev %d, trace %q; want the converge's "+
				"at revision 2", obs.Revision, obs.TraceId)
		}
		stamp := reply.GetObsStamp()
		if !stamp.GetObsTime().AsTime().Equal(obs.Time) ||
			stamp.GetConvergeStart() != nil || reply.GetSaveTime() == nil {
			t.Errorf("reply stamp %v, save time %v; want the observation's "+
				"and the save's", stamp, reply.GetSaveTime())
		}
	})

	t.Run("a SyncupSide whose save fails", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		path := srv.nf.LocalSidePath(testCluster, testDn, testSp, testSide)
		oc := node.osClient()
		write := oc.WriteProtoFn
		oc.WriteProtoFn = func(
			ctx context.Context, p string, msg proto.Message,
		) error {
			if p == path {
				return errors.New("write " + p + ": no space left on device")
			}
			return write(ctx, p, msg)
		}
		srv.store = agent.NewStore(oc, common.DefaultLocalStorPrefix)
		reply, err := srv.SyncupSide(traced("side-syncup"), failoverReq(2))
		if err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if reply.GetSaveTime() != nil {
			t.Errorf("save time %v for a save that failed", reply.GetSaveTime())
		}
		if obs := sideObs(t, srv); obs.Revision != 2 {
			t.Errorf("observation revision %d, want the converged 2",
				obs.Revision)
		}
	})

	t.Run("the startup reconcile", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		stopTestServer(t, srv)
		restarted := NewDnAgentServer(node.osClient(),
			common.NewNameFmt(common.DefaultLocalStorPrefix),
			common.DefaultLocalStorPrefix, testDisk, testTrConf(),
			common.NvmetPortId)
		restarted.roundReads = false
		rootCtx, cancel := context.WithCancel(traced("startup"))
		t.Cleanup(func() {
			cancel()
			restarted.WaitBackground()
		})
		if err := restarted.Reconcile(rootCtx); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		dn := dnObs(t, restarted)
		if dn.Revision != 1 || dn.TraceId != "startup" || dn.Time.IsZero() ||
			dn.Reply.GetCode() != 0 || dn.Info.GetMetaInfo().GetStatus() !=
			pb.ResStatus_RES_STATUS_OK {
			t.Errorf("dn observation = rev %d, trace %q, reply %v, info %v; "+
				"want the reconcile's", dn.Revision, dn.TraceId, dn.Reply,
				dn.Info)
		}
		side := sideObs(t, restarted)
		if side.Revision != 1 || side.TraceId != "startup" ||
			side.Time.IsZero() || side.Info.GetSideDevInfo().GetStatus() !=
			pb.ResStatus_RES_STATUS_OK {
			t.Errorf("side observation = rev %d, trace %q, info %v; want "+
				"the reconcile's", side.Revision, side.TraceId, side.Info)
		}
	})

	t.Run("a DN13 retry attempt", func(t *testing.T) {
		srv, node := newTestServer(t)
		names := newMigrDstNames()
		srv.migrRetryInterval = 5 * time.Millisecond
		syncupBoth(t, srv, 1, testSide)
		setFailAlways(node, "dmsetup create "+names.clone, "refused")
		if _, err := srv.SyncupSide(traced("side-syncup"),
			migrDstReq(2, pb.SpLevel_SP_LEVEL_READWRITE)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if !migrRetrying(srv) {
			t.Fatal("fixture is wrong: no retry registered")
		}
		waitUntil(t, "a retry attempt to store an observation", func() bool {
			obs := sideObs(t, srv)
			return obs.TraceId != "side-syncup" && obs.Revision == 2
		})
		// The attempt that ends the retry stores what it built.
		clearFailAlways(node, "dmsetup create "+names.clone)
		waitUntil(t, "the retry to end with the dm-clone observed",
			func() bool {
				obs := sideObs(t, srv)
				return !migrRetrying(srv) && obs.Info.GetMigrDstInfo().
					GetDmCloneInfo().GetStatus() ==
					pb.ResStatus_RES_STATUS_OK
			})
	})

	t.Run("the DN12 fence timer", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		lin := srv.nf.DnLinearName(testCluster, testDn, testSp, testSide,
			testCn0)
		srv.fenceWait = 30 * time.Millisecond
		if _, err := srv.SyncupSide(traced("side-syncup"),
			migrSrcReq(2)); err != nil {
			t.Fatalf("SyncupSide: %v", err)
		}
		if !dmSuspended(node, lin) {
			t.Fatal("fixture is wrong: the cutover suspended nothing")
		}
		waitUntil(t, "the fence timer's converge to store an observation",
			func() bool {
				obs := sideObs(t, srv)
				return obs.TraceId != "side-syncup" && obs.Revision == 2
			})
		if dmSuspended(node, lin) {
			t.Error("the observation was stored before the window ended")
		}
	})
}

// A converge whose call was cut off stores nothing (SH28): every command
// after the cut fails at once, so its rows tell of the cut. The object keeps
// the observation it had, and a new object stays with none: its rounds
// answer with no observation time (HL7) and its info reads with no info.
func TestACutOffConvergeStoresNothing(t *testing.T) {
	t.Run("a held side", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		before := sideObs(t, srv)
		oldLin := srv.nf.DnLinearName(testCluster, testDn, testSp, testSide,
			testCn0)
		node.Reset()
		cutOffSyncupSide(t, srv, node, failoverReq(2),
			"dmsetup reload "+oldLin)
		after := sideObs(t, srv)
		if !after.Time.Equal(before.Time) || after.Revision != 1 {
			t.Errorf("the cut-off converge stored an observation: rev %d "+
				"at %v, want rev 1 at %v", after.Revision, after.Time,
				before.Time)
		}
	})

	t.Run("a new dn", func(t *testing.T) {
		srv, _ := newTestServer(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		reply, err := srv.SyncupDn(ctx, dnReq(1, testSide))
		if err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		if reply.GetObsStamp().GetObsTime() != nil {
			t.Errorf("the cut-off reply stamps an observation: %v",
				reply.GetObsStamp())
		}
		if srv.getDn(dnKey(testCluster, testDn)) == nil {
			t.Fatal("fixture is wrong: the agent does not hold the dn")
		}
		round := dnRound(srv, "round")
		stamp := round.GetObsStamp()
		if stamp.GetObsTime() != nil || stamp.GetAnswerTime() == nil ||
			stamp.GetConvergeStart() != nil {
			t.Errorf("round stamp = %v, want an answer time alone", stamp)
		}
		if round.GetAgentReply().GetCode() != 0 || round.GetRevision() != 0 ||
			round.GetDnInfo() != nil {
			t.Errorf("round = code %d, rev %d, info %v; want no observation",
				round.GetAgentReply().GetCode(), round.GetRevision(),
				round.GetDnInfo())
		}
		info, err := srv.GetDnInfo(context.Background(),
			&pb.GetDnInfoRequest{ClusterId: testCluster, DnId: testDn})
		if err != nil {
			t.Fatalf("GetDnInfo: %v", err)
		}
		if info.GetObsStamp().GetObsTime() != nil || info.GetDnInfo() != nil {
			t.Errorf("info read = %v, want no observation", info)
		}
	})
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

// A read or a converge whose ctx is cancelled while it runs makes each row
// after the cut from a call that failed on it, and stores no observation
// (SH28, SH29); those rows move no epoch either (SH14), so the next read
// reports every row the node never changed with the epoch it had before the
// cut, and the observation equals the one before it (architecture.md,
// Live-state reporting; agent.ResTracker). A side's passes bind their ctx to
// its tracker under its object lock; a DN's read and converge each write
// through a view of their own.
func TestACutOffPassMovesNoEpoch(t *testing.T) {
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	sideK := sideKey(testCluster, testDn, testSp, testSide)
	dnK := dnKey(testCluster, testDn)
	sideDev := nf.DnSideName(testCluster, testDn, testSp, testSide)
	readSide := func(t *testing.T, srv *DnAgentServer) proto.Message {
		readSideNow(context.Background(), srv, sidePtr(testSide))
		return sideObs(t, srv).Info
	}
	readDn := func(t *testing.T, srv *DnAgentServer) proto.Message {
		readDnNow(context.Background(), srv)
		return dnObs(t, srv).Info
	}
	cases := []struct {
		name string
		// cut is the line of the call whose run cancels the pass.
		cut  string
		pass func(t *testing.T, ctx context.Context, srv *DnAgentServer)
		read func(t *testing.T, srv *DnAgentServer) proto.Message
	}{
		{"a side read", "-o attr " + sideDev,
			func(t *testing.T, ctx context.Context, srv *DnAgentServer) {
				srv.refreshSide(ctx, sideK, srv.getSide(sideK))
			}, readSide},
		{"a dn read", "devno " + testDisk,
			func(t *testing.T, ctx context.Context, srv *DnAgentServer) {
				srv.refreshDn(ctx, dnK, srv.getDn(dnK))
			}, readDn},
		{"a SyncupSide", "cmd dmsetup ls",
			func(t *testing.T, ctx context.Context, srv *DnAgentServer) {
				reply, err := srv.SyncupSide(ctx, sideReq(2, testSide,
					testCn0, []uint64{testCn1},
					pb.SpLevel_SP_LEVEL_READWRITE))
				if err != nil {
					t.Fatalf("SyncupSide: %v", err)
				}
				if got := reply.GetSideInfo().GetSideDevInfo().
					GetStatus(); got != pb.ResStatus_RES_STATUS_ERROR {
					t.Fatalf("fixture is wrong: the cut-off converge's "+
						"side device row is %v", got)
				}
			}, readSide},
		{"a SyncupDn", "devno " + testDisk,
			func(t *testing.T, ctx context.Context, srv *DnAgentServer) {
				reply, err := srv.SyncupDn(ctx, dnReq(2, testSide))
				if err != nil {
					t.Fatalf("SyncupDn: %v", err)
				}
				if got := reply.GetDnInfo().GetDiskInfo().
					GetStatus(); got != pb.ResStatus_RES_STATUS_ERROR {
					t.Fatalf("fixture is wrong: the cut-off converge's "+
						"disk row is %v", got)
				}
			}, readDn},
	}
	// Every fixture's rows first, then one wait for the next second for all
	// of them.
	type fixture struct {
		srv    *DnAgentServer
		node   *fakeNode
		before proto.Message
	}
	fixtures := make([]fixture, len(cases))
	for i, tc := range cases {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		fixtures[i] = fixture{srv, node, proto.Clone(tc.read(t, srv))}
	}
	nextSecond()
	for i, tc := range cases {
		srv, node, before := fixtures[i].srv, fixtures[i].node,
			fixtures[i].before
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(traced("cut"))
			defer cancel()
			node.mu.Lock()
			node.cancelCmd[tc.cut] = cancel
			node.mu.Unlock()
			tc.pass(t, ctx, srv)
			if ctx.Err() == nil {
				t.Fatalf("fixture is wrong: the pass never ran %q", tc.cut)
			}
			if after := tc.read(t, srv); !proto.Equal(after, before) {
				t.Errorf("the read after the cut reports\n%v\nwant, as "+
					"before it,\n%v", after, before)
			}
		})
	}
}

// A round of an object the agent does not hold answers with no observation,
// only the agent's clock (SH25); so does an info read.
func TestAnUnknownObjectAnswersAnAnswerTimeAlone(t *testing.T) {
	srv, node := newTestServer(t)
	clock := withDnClock(srv, node)
	for what, stamp := range map[string]*pb.ObsStamp{
		"CheckDn":   dnRound(srv, "r").GetObsStamp(),
		"CheckSide": sideRound(srv, "r").GetObsStamp(),
	} {
		if stamp.GetObsTime() != nil || stamp.GetConvergeStart() != nil ||
			stamp.GetAnswerTime() == nil ||
			!stamp.GetAnswerTime().AsTime().Equal(clock.Now()) {
			t.Errorf("%s stamp = %v, want the answer time %v alone", what,
				stamp, clock.Now())
		}
	}
	side, _ := srv.GetSideInfo(context.Background(), &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	})
	if side.GetAgentReply().GetCode() != common.ReplyCodeUnknownObject ||
		side.GetObsStamp().GetObsTime() != nil ||
		side.GetObsStamp().GetAnswerTime() == nil {
		t.Errorf("GetSideInfo = %v, want unknown with an answer time alone",
			side)
	}
}

// A round and an info read answer from the observation alone and take no
// lock (SH25, SH28): while a converge holds the object's lock, or a SyncupDn
// the node write lock, they answer at once with the observation from before
// the converge — its revision, time and trace id — the agent's clock as they
// answer, and the start of the converge in flight. Once it ends they answer
// its observation, with no converge start.
func TestARoundAnswersFromMemoryWhileAConvergeRuns(t *testing.T) {
	t.Run("a SyncupSide", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		clock := withDnClock(srv, node)
		clock.mu.Lock()
		clock.now = time.Now().Add(time.Hour)
		clock.mu.Unlock()
		before := sideObs(t, srv)
		start := clock.Now()

		node.Reset()
		node.blockCmd("dmsetup ls")
		t.Cleanup(func() { node.releaseCmd("dmsetup ls") })
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = srv.SyncupSide(traced("side-syncup"), failoverReq(2))
		}()
		waitUntil(t, "the converge to hold the side's lock",
			func() bool { return node.hasCall("cmd dmsetup ls") })
		_ = clock.Sleep(context.Background(), time.Minute)

		var round *pb.CheckSideReply
		within(t, 2*time.Second, "a CheckSide round", func() {
			round = sideRound(srv, "round")
		})
		var info *pb.GetSideInfoReply
		within(t, 2*time.Second, "GetSideInfo", func() {
			info, _ = srv.GetSideInfo(context.Background(),
				&pb.GetSideInfoRequest{ClusterId: testCluster, DnId: testDn,
					SidePointer: sidePtr(testSide)})
		})
		for what, got := range map[string]struct {
			revision uint64
			stamp    *pb.ObsStamp
			info     *pb.SideInfo
		}{
			"round":     {round.GetRevision(), round.GetObsStamp(), round.GetSideInfo()},
			"info read": {info.GetRevision(), info.GetObsStamp(), info.GetSideInfo()},
		} {
			if got.revision != 1 || !proto.Equal(got.info, before.Info) {
				t.Errorf("%s = rev %d; want the observation before the "+
					"converge, at revision 1", what, got.revision)
			}
			if !got.stamp.GetObsTime().AsTime().Equal(before.Time) ||
				got.stamp.GetObsTraceId() != before.TraceId ||
				!got.stamp.GetConvergeStart().AsTime().Equal(start) ||
				!got.stamp.GetAnswerTime().AsTime().Equal(clock.Now()) {
				t.Errorf("%s stamp = %v; want obs time %v, trace %q, "+
					"converge start %v, answer time %v", what, got.stamp,
					before.Time, before.TraceId, start, clock.Now())
			}
		}

		node.releaseCmd("dmsetup ls")
		<-done
		round = sideRound(srv, "round")
		if round.GetRevision() != 2 ||
			round.GetObsStamp().GetObsTraceId() != "side-syncup" ||
			!round.GetObsStamp().GetObsTime().AsTime().After(before.Time) ||
			round.GetObsStamp().GetConvergeStart() != nil {
			t.Errorf("round after the converge = rev %d, stamp %v; want the "+
				"converge's observation and no converge start",
				round.GetRevision(), round.GetObsStamp())
		}
	})

	t.Run("a SyncupDn", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		dnBefore, sideBefore := dnObs(t, srv), sideObs(t, srv)
		node.Reset()
		node.blockCmd("dmsetup ls")
		t.Cleanup(func() { node.releaseCmd("dmsetup ls") })
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = srv.SyncupDn(traced("dn-syncup"), dnReq(2, testSide))
		}()
		waitUntil(t, "the SyncupDn to hold the node write lock",
			func() bool { return node.hasCall("cmd dmsetup ls") })

		within(t, 2*time.Second, "rounds and info reads", func() {
			dn := dnRound(srv, "round")
			if dn.GetRevision() != 1 ||
				!dn.GetObsStamp().GetObsTime().AsTime().Equal(dnBefore.Time) ||
				dn.GetObsStamp().GetConvergeStart() == nil {
				t.Errorf("CheckDn = rev %d, stamp %v; want the observation "+
					"before the converge and its start", dn.GetRevision(),
					dn.GetObsStamp())
			}
			side := sideRound(srv, "round")
			if side.GetRevision() != 1 ||
				!side.GetObsStamp().GetObsTime().AsTime().
					Equal(sideBefore.Time) ||
				side.GetObsStamp().GetConvergeStart() != nil {
				t.Errorf("CheckSide = rev %d, stamp %v; want the side's "+
					"observation, no converge of the side in flight",
					side.GetRevision(), side.GetObsStamp())
			}
			info, err := srv.GetDnInfo(context.Background(),
				&pb.GetDnInfoRequest{ClusterId: testCluster, DnId: testDn})
			if err != nil {
				t.Errorf("GetDnInfo: %v", err)
			}
			if info.GetRevision() != 1 ||
				!info.GetObsStamp().GetObsTime().AsTime().
					Equal(dnBefore.Time) ||
				info.GetObsStamp().GetConvergeStart() == nil {
				t.Errorf("GetDnInfo = rev %d, stamp %v; want the "+
					"observation before the converge and its start",
					info.GetRevision(), info.GetObsStamp())
			}
		})
		node.releaseCmd("dmsetup ls")
		<-done
		if got := dnRound(srv, "round"); got.GetRevision() != 2 ||
			got.GetObsStamp().GetConvergeStart() != nil {
			t.Errorf("CheckDn after the converge = rev %d, stamp %v",
				got.GetRevision(), got.GetObsStamp())
		}
	})
}

// A check round starts a read of its object when none runs and no converge
// of it, nor a SyncupDn, is in flight, and the server still starts
// background work (SH29, SH30). The read runs under the round's trace id,
// outlives the round, and stores what it read for the next round; an info
// read starts none.
func TestARoundStartsOneRead(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	srv.roundReads = true
	key := sideKey(testCluster, testDn, testSp, testSide)

	// An info read starts nothing.
	node.Reset()
	if _, err := srv.GetSideInfo(context.Background(), &pb.GetSideInfoRequest{
		ClusterId: testCluster, DnId: testDn, SidePointer: sidePtr(testSide),
	}); err != nil {
		t.Fatalf("GetSideInfo: %v", err)
	}
	if _, err := srv.GetDnInfo(context.Background(), &pb.GetDnInfoRequest{
		ClusterId: testCluster, DnId: testDn,
	}); err != nil {
		t.Fatalf("GetDnInfo: %v", err)
	}
	joinReads(t, srv)
	if calls := node.Calls(); len(calls) != 0 || sideReading(srv) {
		t.Fatalf("an info read started a read: %q", calls)
	}

	// One read, and none beside it while it runs.
	node.blockCmd("dmsetup ls")
	t.Cleanup(func() { node.releaseCmd("dmsetup ls") })
	sideRound(srv, "round-1")
	waitUntil(t, "the read to start", func() bool {
		return len(node.Traced("round-1")) > 0 &&
			node.hasCall("cmd dmsetup ls")
	})
	for _, id := range []string{"round-2", "round-3"} {
		sideRound(srv, id)
	}
	node.releaseCmd("dmsetup ls")
	joinReads(t, srv)
	for _, id := range []string{"round-2", "round-3"} {
		if got := node.Traced(id); len(got) != 0 {
			t.Errorf("%s started a second read: %q", id, got)
		}
	}
	obs := sideObs(t, srv)
	if obs.TraceId != "round-1" || obs.Revision != 1 {
		t.Errorf("the read stored trace %q at rev %d, want round-1's at 1",
			obs.TraceId, obs.Revision)
	}
	if got := sideRound(srv, "round-4").GetObsStamp().
		GetObsTraceId(); got != "round-1" {
		t.Errorf("the next round answered %q's observation, want round-1's",
			got)
	}
	joinReads(t, srv)

	// No read while a SyncupSide of the side is in flight, nor a SyncupDn,
	// nor while a background attempt's mark of the side stands.
	inFlight := func(what string, call func(), rounds ...func()) {
		t.Helper()
		node.Reset()
		node.blockCmd("dmsetup ls")
		done := make(chan struct{})
		go func() {
			defer close(done)
			call()
		}()
		waitUntil(t, what+" to reach its listing", func() bool {
			return node.hasCall("cmd dmsetup ls")
		})
		for _, round := range rounds {
			round()
		}
		node.releaseCmd("dmsetup ls")
		<-done
		joinReads(t, srv)
	}
	inFlight("a SyncupSide", func() {
		_, _ = srv.SyncupSide(traced("side-syncup"), failoverReq(2))
	}, func() { sideRound(srv, "round-5") })
	inFlight("a SyncupDn", func() {
		_, _ = srv.SyncupDn(traced("dn-syncup"), dnReq(2, testSide))
	}, func() { sideRound(srv, "round-6") },
		func() { dnRound(srv, "round-7") })
	srv.beginSideConverge(key)
	sideRound(srv, "round-8")
	srv.endConverge(key)
	joinReads(t, srv)
	for _, id := range []string{"round-5", "round-6", "round-7", "round-8"} {
		if got := node.Traced(id); len(got) != 0 {
			t.Errorf("%s started a read under a converge mark: %q", id, got)
		}
	}

	// A CheckDn round reads the DN alike.
	dnRound(srv, "dn-round")
	joinReads(t, srv)
	if got := dnObs(t, srv); got.TraceId != "dn-round" || got.Revision != 2 {
		t.Errorf("the dn read stored trace %q at rev %d, want dn-round's "+
			"at 2", got.TraceId, got.Revision)
	}

	// And none once the join has begun.
	srv.WaitBackground()
	sideRound(srv, "round-9")
	dnRound(srv, "round-10")
	joinReads(t, srv)
	for _, id := range []string{"round-9", "round-10"} {
		if got := node.Traced(id); len(got) != 0 {
			t.Errorf("%s started a read after the join: %q", id, got)
		}
	}
}

// A read whose listing did not answer still stores what it read, the
// verdict saying so (SH29).
func TestAReadWhoseListingFailedStillWrites(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	dnBefore, sideBefore := dnObs(t, srv), sideObs(t, srv)
	setFailAlways(node, "dmsetup ls", "dmsetup ls failed")
	readDnNow(traced("dn-read"), srv)
	readSideNow(traced("side-read"), srv, sidePtr(testSide))
	dn, side := dnObs(t, srv), sideObs(t, srv)
	if dn.TraceId != "dn-read" || !dn.Time.After(dnBefore.Time) ||
		dn.Reply.GetCode() != common.ReplyCodeLeftover {
		t.Errorf("dn observation = trace %q, reply %v; want the read's, "+
			"not clean", dn.TraceId, dn.Reply)
	}
	if side.TraceId != "side-read" || !side.Time.After(sideBefore.Time) ||
		side.Reply.GetCode() != common.ReplyCodeLeftover {
		t.Errorf("side observation = trace %q, reply %v; want the read's, "+
			"not clean", side.TraceId, side.Reply)
	}
}

// A syncup cancels the object's running read before it takes a lock (SH30),
// so it never waits behind one; the cancelled read stores nothing. A read
// cancelled while it waits for the side's lock runs nothing once it gets the
// lock. A syncup DN8's gates refuse on the held request cancels nothing: that
// is TestARefusedSyncupCancelsNoRead.
func TestASyncupCancelsTheRead(t *testing.T) {
	// admittedReq is a SyncupSide DN8's gates admit that asks for what the
	// side already holds, so its converge changes nothing on the node.
	admittedReq := func() *pb.SyncupSideRequest {
		return sideReq(2, testSide, testCn0, []uint64{testCn1},
			pb.SpLevel_SP_LEVEL_READWRITE)
	}
	// syncupDone waits for the syncup and checks it was admitted, and that
	// the side's observation is now the one its converge stored.
	syncupDone := func(t *testing.T, srv *DnAgentServer,
		done <-chan *pb.SyncupSideReply) {
		t.Helper()
		select {
		case reply := <-done:
			if got := reply.GetAgentReply().GetCode(); got != 0 {
				t.Fatalf("fixture is wrong: the syncup answered code %d: %v",
					got, reply.GetAgentReply())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the syncup never returned")
		}
		if got := sideObs(t, srv); got.TraceId != "syncup" ||
			got.Revision != 2 {
			t.Errorf("observation = trace %q at rev %d, want the syncup's",
				got.TraceId, got.Revision)
		}
	}

	// The syncup's own converge parks in the same listing as the read, so
	// the read can only have ended on the syncup's cancel, and the syncup
	// reaches its listing, under the side's lock, while the gate is still
	// closed.
	t.Run("a read in a command", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		srv.roundReads = true
		before := sideObs(t, srv)
		node.Reset()
		release := softGate(t, node, "dmsetup ls")
		sideRound(srv, "round")
		waitUntil(t, "the read to hold the side's lock", func() bool {
			return len(node.Traced("round")) > 0 &&
				node.hasCall("cmd dmsetup ls")
		})
		done := make(chan *pb.SyncupSideReply, 1)
		go func() {
			reply, _ := srv.SyncupSide(traced("syncup"), admittedReq())
			done <- reply
		}()
		joinReads(t, srv)
		waitUntil(t, "the syncup to list under the side's lock", func() bool {
			return contains(node.Traced("syncup"), "cmd dmsetup ls")
		})
		after := sideObs(t, srv)
		if !after.Time.Equal(before.Time) || after.TraceId != before.TraceId {
			t.Errorf("the cancelled read stored trace %q at %v, want the "+
				"observation before it", after.TraceId, after.Time)
		}
		if sideReading(srv) {
			t.Error("the cancelled read still counts as running")
		}
		release()
		syncupDone(t, srv, done)
	})

	// The read checks its ctx again once it holds the side's lock (SH29): a
	// read the syncup cancelled while it waited for that lock behind a
	// holder that cancels nothing, such as a PushMigrBitmap, runs no probe
	// and no verdict on the cancelled ctx. The fake refuses a cancelled
	// command before it records it, so the probe and the verdict show only
	// in the records they log under the round's trace id.
	t.Run("a read waiting for the side's lock", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		srv.roundReads = true
		key := sideKey(testCluster, testDn, testSp, testSide)
		node.Reset()
		capture := captureLogs(t)
		lock := srv.locks.Obj(key)
		lock.Lock()
		locked := true
		t.Cleanup(func() {
			if locked {
				lock.Unlock()
			}
		})
		sideRound(srv, "round")
		// The read holds the node read lock once a try of the write lock
		// fails, which leaves no writer waiting; it then checks the side and
		// parks on the side's lock.
		waitUntil(t, "the read to take the node read lock", func() bool {
			if srv.locks.Node().TryLock() {
				srv.locks.Node().Unlock()
				return false
			}
			return true
		})
		time.Sleep(50 * time.Millisecond)
		done := make(chan *pb.SyncupSideReply, 1)
		go func() {
			reply, _ := srv.SyncupSide(traced("syncup"), admittedReq())
			done <- reply
		}()
		waitUntil(t, "the syncup to mark its converge", func() bool {
			srv.mu.Lock()
			defer srv.mu.Unlock()
			return !srv.marks.SinceLocked(key).IsZero()
		})
		locked = false
		lock.Unlock()
		syncupDone(t, srv, done)
		joinReads(t, srv)
		if got := node.Traced("round"); len(got) != 0 {
			t.Errorf("the cancelled read ran %q once it held the side's lock",
				got)
		}
		if got := capture.traceRecords(t, "round"); len(got) != 0 {
			t.Errorf("the cancelled read logged %v once it held the side's "+
				"lock", got)
		}
		if got := sideObs(t, srv); got.TraceId != "syncup" {
			t.Errorf("the observation is %q's, want the syncup's",
				got.TraceId)
		}
		if sideReading(srv) {
			t.Error("the cancelled read still counts as running")
		}
	})
}

// A SyncupDn cancels every read of the agent before it waits for the node
// write lock, which each of them holds the read lock of (SH30).
func TestASyncupDnCancelsEveryRead(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	srv.roundReads = true
	sideBefore := sideObs(t, srv)
	node.Reset()
	release := softGate(t, node, "dmsetup ls")
	dnRound(srv, "dn-round")
	sideRound(srv, "side-round")
	waitUntil(t, "both reads to park in their listing", func() bool {
		return len(node.callsMatching("cmd dmsetup ls")) == 2
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = srv.SyncupDn(traced("dn-syncup"), dnReq(2, testSide))
	}()
	// The reads end on their cancel while the SyncupDn waits for its lock;
	// its own sweep lists only once the gate opens.
	joinReads(t, srv)
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the SyncupDn never returned")
	}
	if got := dnObs(t, srv); got.TraceId != "dn-syncup" || got.Revision != 2 {
		t.Errorf("dn observation = trace %q at rev %d, want the SyncupDn's",
			got.TraceId, got.Revision)
	}
	if got := sideObs(t, srv); !got.Time.Equal(sideBefore.Time) {
		t.Errorf("the cancelled side read stored trace %q", got.TraceId)
	}
}

// A SyncupDn at the revision the agent holds for the DN — the re-send the
// worker issues every round while a DN leftover lasts (dnv-worker.md RW4) —
// marks the DN and cancels the DN's own read alone (SH30): it cancels no
// side's read and holds no side's read off, so a side with no fault keeps a
// fresh observation however long the leftover lasts (dnv-worker.md HL7). Its
// write lock waits for the side's read in flight, and a side round while it
// holds that lock starts a read that runs once the syncup ends.
func TestAReSentSyncupDnLetsSideReadsEnd(t *testing.T) {
	sideK := sideKey(testCluster, testDn, testSp, testSide)
	dnK := dnKey(testCluster, testDn)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	gone := nf.DnSideName(testCluster, testDn, testSp, testSide2)
	// leftover builds a DN whose second side left the list while the
	// removal of its device keeps failing, so every SyncupDn at the held
	// revision answers a leftover, and returns the re-send.
	leftover := func(t *testing.T) (*DnAgentServer, *fakeNode,
		func(traceId string) *pb.SyncupDnReply) {
		t.Helper()
		srv, node := newTestServer(t)
		ctx := context.Background()
		_, err := srv.SyncupDn(ctx, dnReq(1, testSide, testSide2))
		if err != nil {
			t.Fatalf("SyncupDn: %v", err)
		}
		for _, side := range []uint64{testSide, testSide2} {
			syncupSideTwoPhase(t, srv, sideReq(1, side, testCn0,
				[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
		}
		setFailAlways(node, "dmsetup remove "+gone, "device is busy")
		resend := func(traceId string) *pb.SyncupDnReply {
			reply, _ := srv.SyncupDn(traced(traceId), dnReq(2, testSide))
			return reply
		}
		if got := resend("syncup-0").GetAgentReply().GetCode(); got !=
			common.ReplyCodeLeftover {
			t.Fatalf("fixture is wrong: the SyncupDn answered code %d, "+
				"want a leftover", got)
		}
		srv.roundReads = true
		return srv, node, resend
	}
	resendDone := func(t *testing.T, done <-chan *pb.SyncupDnReply) {
		t.Helper()
		select {
		case reply := <-done:
			if got := reply.GetAgentReply().GetCode(); got !=
				common.ReplyCodeLeftover {
				t.Fatalf("fixture is wrong: the re-send answered code %d, "+
					"want a leftover", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the re-sent SyncupDn never returned")
		}
	}

	// Round after round, a re-send lands while the side's read is in its
	// listing: the read runs on and stores, so the side's observation keeps
	// moving forward, while the DN's read is cancelled and the DN's
	// observation is each re-send's own.
	t.Run("a read in flight runs on", func(t *testing.T) {
		srv, node, resend := leftover(t)
		prev := sideObs(t, srv)
		for i := 1; i <= 3; i++ {
			sideId := fmt.Sprintf("side-round-%d", i)
			syncupId := fmt.Sprintf("syncup-%d", i)
			node.Reset()
			release := softGate(t, node, "dmsetup ls")
			dnRound(srv, fmt.Sprintf("dn-round-%d", i))
			sideRound(srv, sideId)
			waitUntil(t, "both reads to park in their listing", func() bool {
				return len(node.callsMatching("cmd dmsetup ls")) == 2
			})
			done := make(chan *pb.SyncupDnReply, 1)
			go func() { done <- resend(syncupId) }()
			// Within a second: a read parked in a command ends on its own
			// once the command's soft timeout cuts it.
			for deadline := time.Now().Add(time.Second); dnReading(srv); {
				if time.Now().After(deadline) {
					t.Fatalf("round %d: the re-send did not cancel the "+
						"DN's read", i)
				}
				time.Sleep(5 * time.Millisecond)
			}
			srv.mu.Lock()
			dnSince := srv.marks.SinceLocked(dnK)
			sideQuiet := srv.marks.QuietLocked(sideK)
			srv.mu.Unlock()
			if dnSince.IsZero() || !sideQuiet {
				t.Errorf("round %d: dn marked %v, side quiet %v; want the "+
					"DN's mark alone", i, !dnSince.IsZero(), sideQuiet)
			}
			readsDone := make(chan struct{})
			go func() {
				srv.reads.Wait()
				close(readsDone)
			}()
			select {
			case <-readsDone:
				t.Fatalf("round %d: the re-sent SyncupDn cancelled the "+
					"side's read", i)
			case <-time.After(150 * time.Millisecond):
			}
			release()
			resendDone(t, done)
			<-readsDone
			got := sideObs(t, srv)
			if got.TraceId != sideId || !got.Time.After(prev.Time) {
				t.Errorf("round %d: side observation = trace %q at %v, "+
					"want the read %q after %v", i, got.TraceId, got.Time,
					sideId, prev.Time)
			}
			prev = got
			if got := dnObs(t, srv).TraceId; got != syncupId {
				t.Errorf("round %d: dn observation is %q's, want the "+
					"re-send's", i, got)
			}
		}
	})

	// A side round while the re-send holds the write lock starts a read:
	// no node mark holds it off, and the read waits for the node read lock
	// until the syncup ends, then stores.
	t.Run("a round meanwhile starts a read", func(t *testing.T) {
		srv, node, resend := leftover(t)
		node.Reset()
		release := softGate(t, node, "-o attr "+gone)
		done := make(chan *pb.SyncupDnReply, 1)
		go func() { done <- resend("syncup-1") }()
		waitUntil(t, "the re-send to probe under the write lock", func() bool {
			return node.hasCall("-o attr " + gone)
		})
		sideRound(srv, "side-round")
		if !sideReading(srv) {
			t.Fatal("a side round under the re-send started no read")
		}
		if got := node.Traced("side-round"); len(got) != 0 {
			t.Errorf("the read ran %q under the syncup's write lock", got)
		}
		release()
		resendDone(t, done)
		joinReads(t, srv)
		if got := sideObs(t, srv).TraceId; got != "side-round" {
			t.Errorf("side observation is %q's, want the read's", got)
		}
	})
}

// A syncup the gates refuse on the request the agent holds for its object —
// the revision gate (SH8), DN8's side conf gate, DN4's conf gate — marks
// nothing and cancels no read (SH30): the worker re-sends such a request
// every round, and were it to cancel each round's read, no read would ever
// end and the observation would go stale (dnv-worker.md HL7). The read runs
// on to its end and stores its observation, the gates refuse the request
// under its locks as before, and a round after it starts a read. A mark
// another converge of the object holds outlives the refused syncup.
func TestARefusedSyncupCancelsNoRead(t *testing.T) {
	sideK := sideKey(testCluster, testDn, testSp, testSide)
	dnK := dnKey(testCluster, testDn)
	staleSide := func(srv *DnAgentServer) *pb.AgentReply {
		reply, _ := srv.SyncupSide(traced("syncup"), sideReq(1, testSide,
			testCn0, []uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
		return reply.GetAgentReply()
	}
	staleDn := func(srv *DnAgentServer) *pb.AgentReply {
		reply, _ := srv.SyncupDn(traced("syncup"), dnReq(1, testSide))
		return reply.GetAgentReply()
	}
	// badConfSide is a newer SyncupSide whose length to zero DN8's side
	// conf gate refuses, as a bad stored conf would bring every round.
	badConfSide := func(srv *DnAgentServer) *pb.AgentReply {
		req := sideReq(3, testSide, testCn0, []uint64{testCn1},
			pb.SpLevel_SP_LEVEL_READWRITE)
		req.SideConf.ZeroBytes = 0
		reply, _ := srv.SyncupSide(traced("syncup"), req)
		return reply.GetAgentReply()
	}
	// badConfDn is a newer SyncupDn whose zero extent size DN4's conf gate
	// refuses.
	badConfDn := func(srv *DnAgentServer) *pb.AgentReply {
		req := dnReq(3, testSide)
		req.ExtentSize = 0
		reply, _ := srv.SyncupDn(traced("syncup"), req)
		return reply.GetAgentReply()
	}
	// quiet checks that none of keys has a mark and no node mark stands.
	quiet := func(t *testing.T, srv *DnAgentServer, keys ...string) {
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

	sideRounds := map[string]func(*DnAgentServer, string){
		"side-round": func(srv *DnAgentServer, id string) {
			sideRound(srv, id)
		},
	}
	sideStored := func(t *testing.T, srv *DnAgentServer) map[string]string {
		return map[string]string{"side-round": sideObs(t, srv).TraceId}
	}
	nodeRounds := map[string]func(*DnAgentServer, string){
		"side-round": func(srv *DnAgentServer, id string) {
			sideRound(srv, id)
		},
		"dn-round": func(srv *DnAgentServer, id string) {
			dnRound(srv, id)
		},
	}
	nodeStored := func(t *testing.T, srv *DnAgentServer) map[string]string {
		return map[string]string{
			"side-round": sideObs(t, srv).TraceId,
			"dn-round":   dnObs(t, srv).TraceId,
		}
	}
	for _, tc := range []struct {
		name  string
		stale func(srv *DnAgentServer) *pb.AgentReply
		// code is the refusal's.
		code uint32
		// rounds are the reads that run beside the syncup, by trace id.
		rounds map[string]func(srv *DnAgentServer, traceId string)
		// stored is the trace id each object's observation holds.
		stored func(t *testing.T, srv *DnAgentServer) map[string]string
	}{
		{"SyncupSide", staleSide, common.ReplyCodeStaleRevision,
			sideRounds, sideStored},
		{"SyncupSide side conf", badConfSide, common.ReplyCodeInvalidConf,
			sideRounds, sideStored},
		{"SyncupDn", staleDn, common.ReplyCodeStaleRevision,
			nodeRounds, nodeStored},
		{"SyncupDn conf", badConfDn, common.ReplyCodeInvalidConf,
			nodeRounds, nodeStored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, 2, testSide)
			srv.roundReads = true
			node.Reset()
			release := softGate(t, node, "dmsetup ls")
			for id, round := range tc.rounds {
				round(srv, id)
			}
			waitUntil(t, "the reads to park in their listing", func() bool {
				return len(node.callsMatching("cmd dmsetup ls")) ==
					len(tc.rounds)
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
			quiet(t, srv, sideK, dnK)
			release()
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
			sideRound(srv, "after")
			joinReads(t, srv)
			if got := sideObs(t, srv).TraceId; got != "after" {
				t.Errorf("the round after the refused syncup stored no read: "+
					"the observation is %q's", got)
			}
		})
	}

	// The refused syncup ends no mark it did not make: a background
	// attempt's mark of the side, and an admitted SyncupDn's marks of the DN
	// and the node, stand until their own converge ends them.
	t.Run("another converge's mark", func(t *testing.T) {
		srv, _ := newTestServer(t)
		syncupBoth(t, srv, 2, testSide)

		srv.beginSideConverge(sideK)
		within(t, 2*time.Second, "a stale SyncupSide", func() {
			if got := staleSide(srv); got.GetCode() !=
				common.ReplyCodeStaleRevision {
				t.Errorf("fixture is wrong: the syncup was not refused "+
					"as stale: %v", got)
			}
		})
		srv.mu.Lock()
		sideSince := srv.marks.SinceLocked(sideK)
		srv.mu.Unlock()
		if sideSince.IsZero() {
			t.Error("the stale SyncupSide ended the attempt's mark")
		}
		srv.endConverge(sideK)

		end := srv.beginDnSyncup(dnK, dnReq(3, testSide))
		if end == nil {
			t.Fatal("fixture is wrong: an admitted revision was not marked")
		}
		within(t, 2*time.Second, "a stale SyncupDn", func() {
			if got := staleDn(srv); got.GetCode() !=
				common.ReplyCodeStaleRevision {
				t.Errorf("fixture is wrong: the syncup was not refused "+
					"as stale: %v", got)
			}
		})
		srv.mu.Lock()
		dnSince := srv.marks.SinceLocked(dnK)
		sideQuiet := srv.marks.QuietLocked(sideK)
		srv.mu.Unlock()
		if dnSince.IsZero() || sideQuiet {
			t.Errorf("the stale SyncupDn ended the admitted one's marks: "+
				"dn since %v, side quiet %v", dnSince, sideQuiet)
		}
		end()
		quiet(t, srv, sideK, dnK)
	})
}

// The teardown of a side's state cancels its read and waits for nothing
// (SH30), and a read of a side dropped before it found the side leaves no
// lock entry and stores nothing.
func TestDroppingASideCancelsItsRead(t *testing.T) {
	t.Run("a read in a command", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		srv.roundReads = true
		key := sideKey(testCluster, testDn, testSp, testSide)
		node.Reset()
		softGate(t, node, "dmsetup ls")
		sideRound(srv, "round")
		waitUntil(t, "the read to park in its listing", func() bool {
			return node.hasCall("cmd dmsetup ls")
		})
		st := srv.getSide(key)
		within(t, 2*time.Second, "the drop", func() { srv.dropSide(key) })
		joinReads(t, srv)
		srv.mu.Lock()
		obs, reading := st.obs.LatestLocked(), st.obs.ReadingLocked()
		srv.mu.Unlock()
		if obs.TraceId == "round" || reading {
			t.Errorf("the dropped side's read stored %q or still runs (%v)",
				obs.TraceId, reading)
		}
	})

	t.Run("a read waiting for the node lock", func(t *testing.T) {
		srv, node := newTestServer(t)
		ctx := context.Background()
		syncupBoth(t, srv, 1, testSide)
		srv.roundReads = true
		key := sideKey(testCluster, testDn, testSp, testSide)
		st := srv.getSide(key)
		node.Reset()
		srv.locks.Node().Lock()
		within(t, 2*time.Second, "a round under the node write lock",
			func() { sideRound(srv, "round") })
		srv.dropSideState(ctx, key, st)
		before := srv.locks.ObjCount()
		srv.locks.Node().Unlock()
		joinReads(t, srv)
		if got := srv.locks.ObjCount(); got != before {
			t.Errorf("%d lock entries after the read, want %d", got, before)
		}
		if got := node.Traced("round"); len(got) != 0 {
			t.Errorf("the read of a dropped side ran %q", got)
		}
	})
}

// The dn's join waits for a running read (SH27, SH29): a read runs child
// processes, which must not outlive the agent.
func TestTheJoinWaitsForARead(t *testing.T) {
	srv, node := newTestServer(t)
	syncupBoth(t, srv, 1, testSide)
	srv.roundReads = true
	node.Reset()
	node.blockCmd("dmsetup ls")
	t.Cleanup(func() { node.releaseCmd("dmsetup ls") })
	sideRound(srv, "round")
	waitUntil(t, "the read to park in its listing", func() bool {
		return node.hasCall("cmd dmsetup ls")
	})
	joined := make(chan struct{})
	go func() {
		srv.WaitBackground()
		close(joined)
	}()
	select {
	case <-joined:
		t.Fatal("the join did not wait for the read")
	case <-time.After(150 * time.Millisecond):
	}
	node.releaseCmd("dmsetup ls")
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("the join never returned once the read was let go")
	}
}

// A background converge attempt (DN12, DN13) marks its converge once it has
// found the side, before it waits for the side's lock, and cancels no read
// (SH30): a round meanwhile reports the converge's start and starts no read,
// and a read the attempt finds running runs on to its end.
func TestABackgroundAttemptMarksItsConverge(t *testing.T) {
	t.Run("the mark", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		srv.roundReads = true
		key := sideKey(testCluster, testDn, testSp, testSide)
		st := srv.getSide(key)
		lock := srv.locks.Obj(key)
		lock.Lock()
		done := make(chan struct{})
		go func() {
			defer close(done)
			srv.reconvergeSide(srv.rootCtx, key, st)
		}()
		var since time.Time
		waitUntil(t, "the attempt to mark its converge", func() bool {
			srv.mu.Lock()
			defer srv.mu.Unlock()
			since = srv.marks.SinceLocked(key)
			return !since.IsZero()
		})
		stamp := sideRound(srv, "round").GetObsStamp()
		if !stamp.GetConvergeStart().AsTime().Equal(since) {
			t.Errorf("round stamp = %v, want the attempt's start %v", stamp,
				since)
		}
		lock.Unlock()
		<-done
		joinReads(t, srv)
		if got := node.Traced("round"); len(got) != 0 {
			t.Errorf("a round under the attempt's mark started a read: %q",
				got)
		}
		srv.mu.Lock()
		left := srv.marks.SinceLocked(key)
		srv.mu.Unlock()
		if !left.IsZero() {
			t.Error("the attempt's mark outlived it")
		}
	})

	t.Run("no cancel", func(t *testing.T) {
		srv, node := newTestServer(t)
		syncupBoth(t, srv, 1, testSide)
		srv.roundReads = true
		key := sideKey(testCluster, testDn, testSp, testSide)
		st := srv.getSide(key)
		node.Reset()
		release := softGate(t, node, "dmsetup ls")
		sideRound(srv, "round")
		waitUntil(t, "the read to park in its listing", func() bool {
			return node.hasCall("cmd dmsetup ls")
		})
		done := make(chan struct{})
		go func() {
			defer close(done)
			srv.reconvergeSide(srv.rootCtx, key, st)
		}()
		readsDone := make(chan struct{})
		go func() {
			srv.reads.Wait()
			close(readsDone)
		}()
		select {
		case <-readsDone:
			t.Fatal("the attempt cancelled the read")
		case <-time.After(150 * time.Millisecond):
		}
		release()
		<-readsDone
		<-done
		if got := sideObs(t, srv); got.TraceId == "round" || got.TraceId == "" {
			t.Errorf("the last observation is %q's, want the attempt's",
				got.TraceId)
		}
	})
}

// A syncup marks its converge as it arrives, before it waits for its locks
// (SH30): while it waits, a round reports its start and starts no read that
// the lock could go to first. A SyncupDn marks the node too, so no round of
// any object of the agent starts one.
func TestASyncupMarksBeforeItsLocks(t *testing.T) {
	for _, tc := range []struct {
		name string
		hold func(srv *DnAgentServer) func()
		call func(srv *DnAgentServer)
		key  string
	}{
		{"SyncupSide", func(srv *DnAgentServer) func() {
			lock := srv.locks.Obj(sideKey(testCluster, testDn, testSp,
				testSide))
			lock.Lock()
			return lock.Unlock
		}, func(srv *DnAgentServer) {
			_, _ = srv.SyncupSide(traced("syncup"), failoverReq(2))
		}, sideKey(testCluster, testDn, testSp, testSide)},
		{"SyncupDn", func(srv *DnAgentServer) func() {
			srv.locks.Node().RLock()
			return srv.locks.Node().RUnlock
		}, func(srv *DnAgentServer) {
			_, _ = srv.SyncupDn(traced("syncup"), dnReq(2, testSide))
		}, dnKey(testCluster, testDn)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, node := newTestServer(t)
			syncupBoth(t, srv, 1, testSide)
			srv.roundReads = true
			unlock := tc.hold(srv)
			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.call(srv)
			}()
			var since time.Time
			waitUntil(t, "the syncup to mark its converge", func() bool {
				srv.mu.Lock()
				defer srv.mu.Unlock()
				since = srv.marks.SinceLocked(tc.key)
				return !since.IsZero()
			})
			stamp := sideRound(srv, "side-round").GetObsStamp()
			if tc.name == "SyncupSide" &&
				!stamp.GetConvergeStart().AsTime().Equal(since) {
				t.Errorf("round stamp = %v, want the syncup's start %v",
					stamp, since)
			}
			dnRound(srv, "dn-round")
			unlock()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the syncup never returned")
			}
			joinReads(t, srv)
			for _, id := range []string{"side-round", "dn-round"} {
				if tc.name == "SyncupSide" && id == "dn-round" {
					continue
				}
				if got := node.Traced(id); len(got) != 0 {
					t.Errorf("%s started a read while the syncup waited "+
						"for its lock: %q", id, got)
				}
			}
		})
	}
}
