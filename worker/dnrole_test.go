package worker

import (
	"context"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// dnTestConf is the DnConf a SyncupDn is built from (RW13).
func dnTestConf() *pb.DnConf {
	return &pb.DnConf{
		DnId:      testDnId,
		ShardCode: testShard,
		SidePtrList: []*pb.SidePointer{
			{SpId: 1, LegId: 2, SideId: 3},
			{SpId: 1, LegId: 4, SideId: 5},
		},
		TotalExtCnt: 100,
		FreeExtCnt:  90,
	}
}

// TestDnSyncupRequestGolden pins the SyncupDn request of RW13: the DnConf's
// side pointer list verbatim and the RESOLVED dn_bin_conf's extent_size.
func TestDnSyncupRequestGolden(t *testing.T) {
	conf := dnTestConf()
	// An empty ClusterConf resolves to the defaults (RW21).
	cc := model.ResolveClusterConf(&pb.ClusterConf{CreationEpoch: 1})
	got := dnSyncupRequest(testCid, testDnId, 42, conf, cc)
	want := &pb.SyncupDnRequest{
		ClusterId:       testCid,
		DnId:            testDnId,
		Revision:        42,
		SidePointerList: conf.GetSidePtrList(),
		ExtentSize:      common.DefaultDnExtSize,
	}
	if !proto.Equal(got, want) {
		t.Fatalf("request =\n%v\nwant\n%v", got, want)
	}

	// A configured extent size wins over the default.
	cc = model.ResolveClusterConf(&pb.ClusterConf{
		CreationEpoch: 1,
		DnBinConf:     &pb.DnBinConf{ExtentSize: 4 * 1024 * 1024 * 1024},
	})
	got = dnSyncupRequest(testCid, testDnId, 42, conf, cc)
	if got.GetExtentSize() != 4*1024*1024*1024 {
		t.Fatalf("extent_size = %d", got.GetExtentSize())
	}
}

// TestDnCheckRequestGolden pins the CheckDn round request (RW4 step 2).
func TestDnCheckRequestGolden(t *testing.T) {
	got := dnCheckRequest(testCid, testDnId, 7, false)
	want := &pb.CheckDnRequest{
		ClusterId: testCid,
		DnId:      testDnId,
		Revision:  7,
		ShowInfo:  false,
	}
	if !proto.Equal(got, want) {
		t.Fatalf("request = %v, want %v", got, want)
	}
}

// TestDnRoleSyncupReadsDnConfPerSyncup checks RW13 end to end: the request the
// agent receives carries the DnConf's pointer list and the cluster's extent
// size.
func TestDnRoleSyncupReadsDnConfPerSyncup(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			// A revision mismatch forces the syncup of RW4 step 5.
			return &pb.CheckDnReply{Revision: 0}
		},
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, dnTestConf())
	h.startDn(testAddr, 42)

	waitFor(t, "syncup", func() bool { return len(stub.syncups()) >= 1 })
	got := stub.syncups()[0]
	want := &pb.SyncupDnRequest{
		ClusterId:       testCid,
		DnId:            testDnId,
		Revision:        42,
		SidePointerList: dnTestConf().GetSidePtrList(),
		ExtentSize:      common.DefaultDnExtSize,
	}
	if !proto.Equal(got, want) {
		t.Fatalf("request =\n%v\nwant\n%v", got, want)
	}
}

// TestDnRoleMissingDnConfSkipsSyncup checks RW13: a missing DnConf is logged
// and skipped, and the round retries next time.
func TestDnRoleMissingDnConfSkipsSyncup(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubDnAgent{
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			return &pb.CheckDnReply{Revision: 0}
		},
	}
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	// No DnConf is seeded.
	h.startDn(testAddr, 3)

	waitFor(t, "dn conf missing", func() bool {
		return len(h.logs.withMsg("dn conf missing")) >= 1
	})
	if got := len(stub.syncups()); got != 0 {
		t.Fatalf("%d syncups without a DnConf", got)
	}
	// Nothing was reported as a syncup result: no SyncupDn was issued.
	if got := len(h.logs.withMsg(msgSyncupResult)); got != 0 {
		t.Fatalf("%d syncup result records without a SyncupDn", got)
	}
	// It converges as soon as the record shows up.
	h.seedDnConf(testAddr, dnTestConf())
	h.advanceUntil("syncup after the conf appears", roundInterval, func() bool {
		return len(stub.syncups()) >= 1
	})
}

// TestDnRoleEndpointChangeRestartsAtNewAddress checks RW13/§10.2: a put whose
// only change is addr_port re-syncs the node at its NEW endpoint — the loop
// drops the stream and the connection reference and continues there, and no
// delete ever reaches the agent.
func TestDnRoleEndpointChangeRestartsAtNewAddress(t *testing.T) {
	h := newRevHarness(t)
	const movedAddr = "dn9:9520"
	echo := func(req *pb.CheckDnRequest) *pb.CheckDnReply {
		return &pb.CheckDnReply{Revision: req.GetRevision()}
	}
	oldStub := &stubDnAgent{checkReply: echo}
	newStub := &stubDnAgent{checkReply: echo}
	h.fleet.addDn(t, testAddr, oldStub)
	h.fleet.addDn(t, movedAddr, newStub)
	h.defaultConf()
	h.seedDnConf(testAddr, dnTestConf())
	movedConf := dnTestConf()
	h.store.seed(t, model.DnConfKey(testCid, movedAddr), movedConf)
	w := h.startDn(testAddr, 5)

	waitFor(t, "first check", func() bool { return oldStub.checkCount() >= 1 })
	if refs := h.deps.conns.refs(testAddr); refs != 1 {
		t.Fatalf("old endpoint refs = %d, want 1", refs)
	}

	// Only the endpoint changes; the revision stays.
	w.update(desiredState{revision: 5, handle: movedAddr})
	waitFor(t, "syncup at the new endpoint", func() bool {
		return len(newStub.syncups()) >= 1
	})
	if got := newStub.syncups()[0].GetRevision(); got != 5 {
		t.Fatalf("re-sync revision = %d, want the unchanged 5", got)
	}
	waitFor(t, "connection moved", func() bool {
		return h.deps.conns.refs(testAddr) == 0 &&
			h.deps.conns.refs(movedAddr) == 1
	})
	h.advanceUntil("rounds at the new endpoint", roundInterval, func() bool {
		return newStub.checkCount() >= 1
	})
	before := oldStub.checkCount()
	h.advanceUntil("another round at the new endpoint", roundInterval,
		func() bool { return newStub.checkCount() >= 2 })
	if oldStub.checkCount() != before {
		t.Fatalf("the old endpoint is still being checked")
	}
}

// TestDnRoleHealthFromInfo checks HL1/HL4/HL5 through the real loop: an ERROR
// row in a Check reply's DnInfo sets err_epoch, and a later clean reply — even
// one carrying no info at all — clears it.
func TestDnRoleHealthFromInfo(t *testing.T) {
	h := newRevHarness(t)
	bad := true
	stub := &stubDnAgent{}
	stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
		reply := &pb.CheckDnReply{Revision: req.GetRevision()}
		if bad {
			reply.DnInfo = &pb.DnInfo{DiskInfo: resErr("disk", "io error")}
		}
		return reply
	})
	h.fleet.addDn(t, testAddr, stub)
	h.defaultConf()
	h.seedDnConf(testAddr, dnTestConf())
	h.startDn(testAddr, 1)

	waitFor(t, "unhealthy", func() bool { return len(h.hw.all()) == 1 })
	if h.hw.all()[0].epoch == 0 {
		t.Fatalf("first write = %+v, want a set", h.hw.all()[0])
	}
	// A reply with neither info nor error is a clean round only once the
	// latest known info is clean again (HL5), so the agent reports the
	// recovery first.
	bad = false
	stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
		return &pb.CheckDnReply{
			Revision: req.GetRevision(),
			DnInfo:   &pb.DnInfo{DiskInfo: resOk("disk")},
		}
	})
	h.advanceUntil("recovered", roundInterval, func() bool {
		return len(h.hw.all()) == 2
	})
	if h.hw.all()[1].epoch != 0 {
		t.Fatalf("second write = %+v, want a clear", h.hw.all()[1])
	}
	// Further clean rounds write nothing (HL3).
	stub.setCheckReply(func(req *pb.CheckDnRequest) *pb.CheckDnReply {
		return &pb.CheckDnReply{Revision: req.GetRevision()}
	})
	h.advanceUntil("more rounds", roundInterval, func() bool {
		return stub.checkCount() >= 4
	})
	if got := len(h.hw.all()); got != 2 {
		t.Fatalf("%d health writes, want 2", got)
	}
}

// TestDnDriverInfoIsRaceFree checks HL1/HL5 under -race. A CheckDn reply is
// decoded by fold on the STREAM'S PUMP goroutine (dnCheckStream.recv) while
// the loop goroutine reads the same lastInfo in observe and rewrites its rows
// in place in unreachable (§9.5). The two overlap on RW4 step 4: recv returns
// errRoundTimeout while a late reply is already inside the pump's recv, and
// fail() then drops the stream — which does NOT join the pump — and calls
// unreachable at once. Unguarded, a just-stored fresh DnInfo is overwritten
// with RES_STATUS_UNKNOWN or the marking lands on a message being discarded;
// either way the round's ERROR rows are lost, HL1 never sets err_epoch and the
// node stays in the §5.6 capacity index as an allocation candidate.
func TestDnDriverInfoIsRaceFree(t *testing.T) {
	h := newRevHarness(t)
	h.defaultConf()
	params := revWorkerParams{
		deps:    h.deps,
		role:    common.WorkerRoleDn,
		shard:   testShard,
		cid:     testCid,
		id:      testDnId,
		seed:    seedOf(1),
		desired: desiredState{revision: 1, handle: testAddr},
	}
	// A DN driver never calls back into its host (unlike the sp children), so
	// the loop it belongs to is not needed to exercise the two goroutines.
	d := newDnDriver(params, nil).(*dnDriver)
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			d.fold(&pb.AgentReply{}, uint64(i), &pb.DnInfo{
				DiskInfo: resErr("disk", "io error"),
				MetaInfo: resOk("meta"),
			})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			d.observe(ctx, &replyState{revision: uint64(i)})
			d.unreachable(ctx)
		}
	}()
	wg.Wait()
}

// TestDnRoleEndpointChangeDuringRoundMovesConnection checks RW13 and RW7 on
// the mid-round path of RW6: a put whose only change is addr_port, delivered
// while the round is still waiting for its reply, re-syncs the node at the NEW
// endpoint at once, hands the old endpoint's connection reference back, and
// reaches no health verdict — a moved node was never unreachable (HL1). No
// delete ever reaches either agent (§10.2, [D10]).
func TestDnRoleEndpointChangeDuringRoundMovesConnection(t *testing.T) {
	h := newRevHarness(t)
	const movedAddr = "dn9:9520"
	// The old agent never answers: only a desired change can end this round.
	oldStub := &stubDnAgent{
		checkReply: func(*pb.CheckDnRequest) *pb.CheckDnReply { return nil },
	}
	newStub := &stubDnAgent{
		checkReply: func(req *pb.CheckDnRequest) *pb.CheckDnReply {
			return &pb.CheckDnReply{Revision: req.GetRevision()}
		},
	}
	h.fleet.addDn(t, testAddr, oldStub)
	h.fleet.addDn(t, movedAddr, newStub)
	h.defaultConf()
	h.seedDnConf(testAddr, dnTestConf())
	h.store.seed(t, model.DnConfKey(testCid, movedAddr), dnTestConf())
	w := h.startDn(testAddr, 5)

	waitFor(t, "first check", func() bool { return oldStub.checkCount() >= 1 })
	if refs := h.deps.conns.refs(testAddr); refs != 1 {
		t.Fatalf("old endpoint refs = %d, want 1", refs)
	}
	// Only the endpoint changes; the revision stays.
	w.update(desiredState{revision: 5, handle: movedAddr})
	waitFor(t, "syncup at the new endpoint", func() bool {
		return len(newStub.syncups()) >= 1
	})
	if got := newStub.syncups()[0].GetRevision(); got != 5 {
		t.Fatalf("re-sync revision = %d, want the unchanged 5", got)
	}
	waitFor(t, "connection moved", func() bool {
		return h.deps.conns.refs(testAddr) == 0 &&
			h.deps.conns.refs(movedAddr) == 1
	})
	// The RW6 syncup's own reply is folded into health like a Check reply's
	// (HL4), so a clean clear is expected — but nothing may set an epoch: the
	// abandoned round is not a missed reply (HL1).
	for _, write := range h.hw.all() {
		if write.epoch != 0 {
			t.Fatalf("a moved node was reported unhealthy: %+v", write)
		}
	}
	// The next round runs at the new endpoint, and the old one is never
	// spoken to again.
	before := oldStub.checkCount()
	h.advanceUntil("round at the new endpoint", roundInterval, func() bool {
		return newStub.checkCount() >= 1
	})
	if oldStub.checkCount() != before {
		t.Fatalf("the old endpoint is still being checked")
	}
	if got := len(oldStub.syncups()); got != 0 {
		t.Fatalf("%d syncups at the old endpoint", got)
	}
}
