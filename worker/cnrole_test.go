package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/model"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

const testCnAddr = "cn0:9620"

// cnTestConf is the CnConf a SyncupCn is built from (§8.3).
func cnTestConf() *pb.CnConf {
	return &pb.CnConf{
		CnId:      testCnId,
		ShardCode: testShard,
		CntlrPtrList: []*pb.CntlrPointer{
			{SpId: 1, CntlrId: 2},
			{SpId: 3, CntlrId: 4},
		},
		TotalExtCnt: 50,
		FreeExtCnt:  20,
	}
}

// startCn starts a cn revision worker against the harness fleet.
func (h *revHarness) startCn(addrPort string, revision uint64) *revWorker {
	h.t.Helper()
	params := revWorkerParams{
		deps:    h.deps,
		role:    common.WorkerRoleCn,
		shard:   testShard,
		cid:     testCid,
		id:      testCnId,
		seed:    seedOf(1),
		desired: desiredState{revision: revision, handle: addrPort},
	}
	w := startRevWorker(params, func(host *revWorker) objDriver {
		return newCnDriver(params, host)
	})
	h.t.Cleanup(w.stop)
	return w
}

// TestCnSyncupRequestGolden pins the SyncupCn request of §8.3: the CnConf's
// cntlr pointer list verbatim and the cluster's qos_ratio. qos_ratio is not
// defaultable at all — CreateCluster stores whatever it was given, including
// nothing (§7) — so an absent one stays absent here rather than becoming an
// empty message.
func TestCnSyncupRequestGolden(t *testing.T) {
	conf := cnTestConf()
	qos := &pb.QosRatio{Strict: true, BytesPerIops: 8192, BytesPerBps: 16}
	cc := testClusterConf(func(cc *pb.ClusterConf) { cc.QosRatio = qos })
	got := cnSyncupRequest(testCid, testCnId, 21, conf, cc)
	want := &pb.SyncupCnRequest{
		ClusterId:        testCid,
		CnId:             testCnId,
		Revision:         21,
		CntlrPointerList: conf.GetCntlrPtrList(),
		QosRatio:         qos,
	}
	if !proto.Equal(got, want) {
		t.Fatalf("request =\n%v\nwant\n%v", got, want)
	}

	// No qos_ratio stored: none is sent.
	cc = testClusterConf()
	if got := cnSyncupRequest(testCid, testCnId, 21, conf, cc); got.GetQosRatio() != nil {
		t.Fatalf("qos_ratio = %v, want nil", got.GetQosRatio())
	}
}

// TestCnCheckRequestGolden pins the CheckCn round request (RW4 step 2).
func TestCnCheckRequestGolden(t *testing.T) {
	got := cnCheckRequest(testCid, testCnId, 9, false)
	want := &pb.CheckCnRequest{
		ClusterId: testCid,
		CnId:      testCnId,
		Revision:  9,
		ShowInfo:  false,
	}
	if !proto.Equal(got, want) {
		t.Fatalf("request = %v, want %v", got, want)
	}
}

// TestCnRoleSyncupReadsCnConf checks §8.3 end to end.
func TestCnRoleSyncupReadsCnConf(t *testing.T) {
	h := newRevHarness(t)
	qos := &pb.QosRatio{Strict: true, BytesPerIops: 8192}
	stub := &stubCnAgent{
		checkReply: func(req *pb.CheckCnRequest) *pb.CheckCnReply {
			return &pb.CheckCnReply{Revision: 0}
		},
	}
	h.fleet.addCn(t, testCnAddr, stub)
	h.setClusterConf(testCid, testClusterConf(func(cc *pb.ClusterConf) {
		cc.QosRatio = qos
	}))
	h.store.seed(t, model.CnConfKey(testCid, testCnAddr), cnTestConf())
	h.startCn(testCnAddr, 21)

	waitFor(t, "syncup", func() bool { return len(stub.syncups()) >= 1 })
	got := stub.syncups()[0]
	want := &pb.SyncupCnRequest{
		ClusterId:        testCid,
		CnId:             testCnId,
		Revision:         21,
		CntlrPointerList: cnTestConf().GetCntlrPtrList(),
		QosRatio:         qos,
	}
	if !proto.Equal(got, want) {
		t.Fatalf("request =\n%v\nwant\n%v", got, want)
	}
}

// TestCnRoleMissingCnConfSkipsSyncup is the cn mirror of RW13's skip.
func TestCnRoleMissingCnConfSkipsSyncup(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubCnAgent{
		checkReply: func(req *pb.CheckCnRequest) *pb.CheckCnReply {
			return &pb.CheckCnReply{Revision: 0}
		},
	}
	h.fleet.addCn(t, testCnAddr, stub)
	h.defaultConf()
	h.startCn(testCnAddr, 2)

	waitFor(t, "cn conf missing", func() bool {
		return len(h.logs.withMsg("cn conf missing")) >= 1
	})
	if got := len(stub.syncups()); got != 0 {
		t.Fatalf("%d syncups without a CnConf", got)
	}
}

// TestCnRoleUsesCnInterval checks RW9: the cn round period comes from
// health_check_conf.cn_interval, not from the dn one.
func TestCnRoleUsesCnInterval(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubCnAgent{
		checkReply: func(req *pb.CheckCnRequest) *pb.CheckCnReply {
			return &pb.CheckCnReply{Revision: req.GetRevision()}
		},
	}
	h.fleet.addCn(t, testCnAddr, stub)
	h.setClusterConf(testCid, testClusterConf(func(cc *pb.ClusterConf) {
		cc.HealthCheckConf.DnInterval = 1
		cc.HealthCheckConf.CnInterval = 30
	}))
	h.store.seed(t, model.CnConfKey(testCid, testCnAddr), cnTestConf())
	h.startCn(testCnAddr, 1)

	waitFor(t, "first round", func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return len(stub.checkReqs) >= 1
	})
	// A dn-sized step is not enough for the next cn round.
	h.clk.advance(2 * time.Second)
	time.Sleep(10 * time.Millisecond)
	stub.mu.Lock()
	afterShort := len(stub.checkReqs)
	stub.mu.Unlock()
	if afterShort != 1 {
		t.Fatalf("%d rounds after 2 s, want 1 with a 30 s interval",
			afterShort)
	}
	h.advanceUntil("second round", 30*time.Second, func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return len(stub.checkReqs) >= 2
	})
}

// TestCnRoleHealthFromInfo checks the HL1 cn row through the real loop.
func TestCnRoleHealthFromInfo(t *testing.T) {
	h := newRevHarness(t)
	stub := &stubCnAgent{
		checkReply: func(req *pb.CheckCnRequest) *pb.CheckCnReply {
			return &pb.CheckCnReply{
				Revision: req.GetRevision(),
				CnInfo:   &pb.CnInfo{TmpfsInfo: resErr("tmpfs", "no space")},
			}
		},
	}
	h.fleet.addCn(t, testCnAddr, stub)
	h.defaultConf()
	h.store.seed(t, model.CnConfKey(testCid, testCnAddr), cnTestConf())
	h.startCn(testCnAddr, 1)

	waitFor(t, "unhealthy", func() bool { return len(h.hw.all()) == 1 })
	write := h.hw.all()[0]
	if write.record != healthRecordCn || write.addr != testCnAddr ||
		write.epoch == 0 {
		t.Fatalf("health write = %+v", write)
	}
	recs := h.logs.withMsg(msgHealthChanged)
	if len(recs) != 1 || recs[0]["res_name"] != "tmpfs" {
		t.Fatalf("health changed = %v", recs)
	}
}

// TestCnDriverInfoIsRaceFree is TestDnDriverInfoIsRaceFree for a CN: fold runs
// on the stream's pump goroutine while observe and unreachable run on the
// loop's, and RW4 step 4 lets them overlap because dropStream does not join
// the pump (HL1, §9.5).
func TestCnDriverInfoIsRaceFree(t *testing.T) {
	h := newRevHarness(t)
	h.defaultConf()
	params := revWorkerParams{
		deps:    h.deps,
		role:    common.WorkerRoleCn,
		shard:   testShard,
		cid:     testCid,
		id:      testCnId,
		seed:    seedOf(1),
		desired: desiredState{revision: 1, handle: testCnAddr},
	}
	d := newCnDriver(params, nil).(*cnDriver)
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			d.fold(&pb.AgentReply{}, uint64(i), &pb.CnInfo{
				TmpfsInfo: resErr("tmpfs", "no space"),
				PortInfo:  resOk("port"),
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
