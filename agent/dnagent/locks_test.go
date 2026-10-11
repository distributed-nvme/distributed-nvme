package dnagent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// 10. Lock mapping smoke (DN1): a SyncupSide stuck in a slow command holds
// the node read lock and its own object lock — so node-scoped reads and other
// sides run on, and only the same side waits.
func TestLockMapping(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()

	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide, testSide2)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}

	gate := make(chan struct{})
	slow := "cmd dmsetup create " +
		nf.DnSideName(testCluster, testDn, testSp, testSide)
	node.mu.Lock()
	node.gate[slow] = gate
	node.mu.Unlock()

	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		_, _ = srv.SyncupSide(ctx, unprovisionedSideReq(1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE))
	}()
	if !waitFor(t, 2*time.Second, func() bool { return node.hasCall(slow) }) {
		t.Fatal("the slow command never started")
	}

	// A node-scoped CheckDn round only needs the node read lock.
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.checkDnRound(ctx, &pb.CheckDnRequest{
			ClusterId: testCluster, DnId: testDn, ShowInfo: true,
		}, nil)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a CheckDn round was blocked by a busy side")
	}

	// A different side takes a different object lock.
	otherDone := make(chan struct{})
	go func() {
		defer close(otherDone)
		_, _ = srv.SyncupSide(ctx, unprovisionedSideReq(1, testSide2, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE))
	}()
	select {
	case <-otherDone:
	case <-time.After(2 * time.Second):
		t.Fatal("a SyncupSide for another side was blocked")
	}

	// The same side must wait.
	sameDone := make(chan struct{})
	go func() {
		defer close(sameDone)
		_, _ = srv.SyncupSide(ctx, unprovisionedSideReq(1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE))
	}()
	select {
	case <-sameDone:
		t.Fatal("a second SyncupSide for the same side was not serialized")
	case <-time.After(150 * time.Millisecond):
	}

	close(gate)
	for _, ch := range []chan struct{}{blocked, sameDone} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("a SyncupSide never finished after the gate opened")
		}
	}
}

// SyncupDn takes the node write lock, so it waits for a busy side (SH10).
func TestSyncupDnWaitsForBusySide(t *testing.T) {
	srv, node := newTestServer(t)
	nf := common.NewNameFmt(common.DefaultLocalStorPrefix)
	ctx := context.Background()

	if _, err := srv.SyncupDn(ctx, dnReq(1, testSide)); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	gate := make(chan struct{})
	slow := "cmd dmsetup create " +
		nf.DnSideName(testCluster, testDn, testSp, testSide)
	node.mu.Lock()
	node.gate[slow] = gate
	node.mu.Unlock()

	sideDone := make(chan struct{})
	go func() {
		defer close(sideDone)
		_, _ = srv.SyncupSide(ctx, unprovisionedSideReq(1, testSide, testCn0, nil,
			pb.SpLevel_SP_LEVEL_READWRITE))
	}()
	if !waitFor(t, 2*time.Second, func() bool { return node.hasCall(slow) }) {
		t.Fatal("the slow command never started")
	}

	dnDone := make(chan struct{})
	go func() {
		defer close(dnDone)
		_, _ = srv.SyncupDn(ctx, dnReq(2, testSide))
	}()
	select {
	case <-dnDone:
		t.Fatal("SyncupDn ran while a side was converging")
	case <-time.After(150 * time.Millisecond):
	}
	close(gate)
	for _, ch := range []chan struct{}{sideDone, dnDone} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("a call never finished after the gate opened")
		}
	}
}

// TestAnotherSidesPassReadsARequestBeingReplaced: a SyncupSide replaces its
// side's request under that side's object lock, and other passes read it
// under locks that do not order them with that one: another side's CheckSide
// round builds its claims from every side's request under the node read lock
// and its own object lock, and a CheckDn round does the same under the node
// read lock alone. The replacement must therefore be safe on its own; under
// the race detector, a plain assignment fails this test with a DATA RACE on
// the request field.
func TestAnotherSidesPassReadsARequestBeingReplaced(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()
	// The second side sits in another leg: two sides of one leg on one DN
	// would share the leg's per-CN exports.
	side2Ptr := &pb.SidePointer{
		SpId: testSp, LegId: testLeg + 1, SideId: testSide2}
	dn := dnReq(1, testSide)
	dn.SidePointerList = append(dn.SidePointerList, side2Ptr)
	if _, err := srv.SyncupDn(ctx, dn); err != nil {
		t.Fatalf("SyncupDn: %v", err)
	}
	syncupSideTwoPhase(t, srv, sideReq(1, testSide, testCn0,
		[]uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
	side2 := sideReq(1, testSide2, testCn0, []uint64{testCn1},
		pb.SpLevel_SP_LEVEL_READWRITE)
	side2.SidePointer = side2Ptr
	syncupSideTwoPhase(t, srv, side2)

	const rounds = 300
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for revision := uint64(2); revision < 2+rounds; revision++ {
			reply, err := srv.SyncupSide(ctx, sideReq(revision, testSide,
				testCn0, []uint64{testCn1}, pb.SpLevel_SP_LEVEL_READWRITE))
			if err != nil || reply.GetAgentReply().GetCode() != 0 {
				t.Errorf("SyncupSide at revision %d: %v %v", revision,
					reply.GetAgentReply(), err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			srv.checkSideRound(ctx, &pb.CheckSideRequest{
				ClusterId: testCluster, DnId: testDn,
				SidePointer: side2Ptr, Revision: 1,
			}, nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			srv.checkDnRound(ctx, &pb.CheckDnRequest{
				ClusterId: testCluster, DnId: testDn, Revision: 1,
			}, nil)
		}
	}()
	wg.Wait()

	reply, _ := srv.checkSideRound(ctx, &pb.CheckSideRequest{
		ClusterId: testCluster, DnId: testDn,
		SidePointer: sidePtr(testSide), Revision: 1 + rounds,
	}, nil)
	if got := reply.GetRevision(); got != 1+rounds {
		t.Errorf("the replaced side reports revision %d, want %d",
			got, 1+rounds)
	}
}

// A request refused for a side the agent does not hold creates no lock entry
// for it (dnagent.md, Concurrency — `locks.go`): only the teardown of a held
// object drops an entry, so one a refusal created would stay for the life of
// the process. Each call is checked twice: the set holds as many entries
// after it as before, and the call does not wait for an entry another call
// holds. The admitted request at the end creates its side's entry, which is
// what the count can see.
func TestARefusedRequestLeavesNoLockEntry(t *testing.T) {
	ctx := context.Background()
	key := sideKey(testCluster, testDn, testSp, testSide)
	for _, tc := range []struct {
		name string
		// listed says whether the DN's list names the side.
		listed bool
		call   func(srv *DnAgentServer) uint32
		want   uint32
		// entries is how many entries the call adds.
		entries int
	}{
		{"a SyncupSide for a pointer the dn does not list", false,
			func(srv *DnAgentServer) uint32 {
				reply, _ := srv.SyncupSide(ctx, sideReq(1, testSide, testCn0,
					nil, pb.SpLevel_SP_LEVEL_READWRITE))
				return reply.GetAgentReply().GetCode()
			}, common.ReplyCodeUnknownObject, 0},
		{"a SyncupSide whose side conf DN8 refuses, for a side never held",
			true, func(srv *DnAgentServer) uint32 {
				req := unprovisionedSideReq(1, testSide, testCn0, nil,
					pb.SpLevel_SP_LEVEL_READWRITE)
				req.SideConf.ZeroBytes = 0
				reply, _ := srv.SyncupSide(ctx, req)
				return reply.GetAgentReply().GetCode()
			}, common.ReplyCodeInvalidConf, 0},
		{"a PushMigrBitmap for a side never held", true,
			func(srv *DnAgentServer) uint32 {
				reply, _ := srv.PushMigrBitmap(ctx,
					pushReq(0, testMigrId, []byte{0xff}))
				return reply.GetAgentReply().GetCode()
			}, common.ReplyCodeUnknownObject, 0},
		{"a GetSideInfo for a side never held", true,
			func(srv *DnAgentServer) uint32 {
				reply, _ := srv.GetSideInfo(ctx, &pb.GetSideInfoRequest{
					ClusterId: testCluster, DnId: testDn,
					SidePointer: sidePtr(testSide),
				})
				return reply.GetAgentReply().GetCode()
			}, common.ReplyCodeUnknownObject, 0},
		{"a CheckSide round for a side never held", true,
			func(srv *DnAgentServer) uint32 {
				reply, _ := srv.checkSideRound(ctx, &pb.CheckSideRequest{
					ClusterId: testCluster, DnId: testDn,
					SidePointer: sidePtr(testSide), ShowInfo: true,
				}, nil)
				return reply.GetAgentReply().GetCode()
			}, common.ReplyCodeUnknownObject, 0},
		{"an admitted SyncupSide", true, func(srv *DnAgentServer) uint32 {
			reply, _ := srv.SyncupSide(ctx, unprovisionedSideReq(1,
				testSide, testCn0, nil, pb.SpLevel_SP_LEVEL_READWRITE))
			return reply.GetAgentReply().GetCode()
		}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newServer := func() *DnAgentServer {
				srv, _ := newTestServer(t)
				dn := dnReq(1)
				if tc.listed {
					dn = dnReq(1, testSide)
				}
				if _, err := srv.SyncupDn(ctx, dn); err != nil {
					t.Fatalf("SyncupDn: %v", err)
				}
				return srv
			}

			srv := newServer()
			before := srv.locks.ObjCount()
			if code := tc.call(srv); code != tc.want {
				t.Fatalf("code %d, want %d", code, tc.want)
			}
			if got := srv.locks.ObjCount(); got != before+tc.entries {
				t.Errorf("%d lock entries after the call, want %d",
					got, before+tc.entries)
			}
			if tc.entries != 0 {
				return
			}

			srv = newServer()
			lock := srv.locks.Obj(key)
			lock.Lock()
			defer lock.Unlock()
			got := make(chan uint32, 1)
			go func() { got <- tc.call(srv) }()
			select {
			case code := <-got:
				if code != tc.want {
					t.Errorf("code %d, want %d", code, tc.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the refusal waited for the side's object lock")
			}
		})
	}
}

// A background attempt scheduled for a side that a drop takes away before
// the attempt gets the node read lock — a fence callback or a retry pass
// racing the side's pointer leaving its DN's list — creates no lock entry:
// the attempt checks the side under the node read lock, which the drop's
// write lock excludes, before it takes the object lock (reconvergeSide).
func TestABackgroundAttemptOnADroppedSideLeavesNoLockEntry(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()
	syncupBoth(t, srv, 1, testSide)
	key := sideKey(testCluster, testDn, testSp, testSide)
	st := srv.getSide(key)

	srv.locks.Node().Lock()
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		close(started)
		srv.reconvergeSide(ctx, key, st)
		close(done)
	}()
	<-started
	// The attempt reaches the node lock and waits behind the write lock.
	time.Sleep(50 * time.Millisecond)
	srv.dropSideState(ctx, key, st)
	before := srv.locks.ObjCount()
	srv.locks.Node().Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the attempt never returned")
	}
	if got := srv.locks.ObjCount(); got != before {
		t.Errorf("%d lock entries after the attempt, want %d", got, before)
	}
}

// A zeroing batch takes its side's lock entry only for a side the agent
// holds, which it checks under the node read lock (dnagent.md, Concurrency —
// `locks.go`): a take for a side state the agent does not hold, or has
// replaced, reports false and creates no entry, and a take for the held side
// gets both locks.
func TestAZeroingLockTakeLeavesNoEntryForASideNotHeld(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()
	key := sideKey(testCluster, testDn, testSp, testSide)

	before := srv.locks.ObjCount()
	if release, ok := srv.zeroAcquire(ctx, key, &sideState{}); ok {
		release()
		t.Fatal("the take succeeded for a side the agent does not hold")
	}
	if got := srv.locks.ObjCount(); got != before {
		t.Errorf("%d lock entries after the take, want %d", got, before)
	}

	syncupBoth(t, srv, 1, testSide)
	st := srv.getSide(key)
	if release, ok := srv.zeroAcquire(
		ctx, key, newSideState(st.req.Load())); ok {
		release()
		t.Error("the take succeeded for a side state the agent does not hold")
	}
	release, ok := srv.zeroAcquire(ctx, key, st)
	if !ok {
		t.Fatal("the take failed for the held side")
	}
	release()
}
