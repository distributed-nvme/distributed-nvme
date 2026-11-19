package etcdutil

import (
	"context"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// log.md, etcd: a get, a range and a watch event that succeed are reads at
// DEBUG; a put and a delete, plain or inside an STM, stay at INFO.
func TestRecordLevels(t *testing.T) {
	cli := newTestClient(t)
	ctx := common.WithTraceId(context.Background(), common.NewTraceId())
	prefix := testPrefix(t)
	key := prefix + "reg"
	_, rev, err := cli.Range(ctx, prefix)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	capture := captureLogs(t)
	eventCh, _ := cli.WatchTyped(wctx, prefix, rev+1, newWorkerReg)

	if err := cli.Put(ctx, key, &pb.WorkerReg{Epoch: 7}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := cli.Get(ctx, key, &pb.WorkerReg{}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := cli.Get(ctx, prefix+"absent", &pb.WorkerReg{}); err != nil {
		t.Fatalf("Get(absent): %v", err)
	}
	if _, _, err := cli.Range(ctx, prefix); err != nil {
		t.Fatalf("Range: %v", err)
	}
	if err := cli.RunSTM(ctx, func(s STM) error {
		s.Get(key, &pb.WorkerReg{})
		s.Put(prefix+"stm", &pb.WorkerReg{Epoch: 8})
		return nil
	}); err != nil {
		t.Fatalf("RunSTM: %v", err)
	}
	if err := cli.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for i := 0; i < 3; i++ {
		recvEvent(t, eventCh)
	}
	for _, rec := range capture.records(t) {
		want := ""
		switch rec["msg"] {
		case msgGet, msgRange, msgWatchEvent:
			want = "DEBUG"
		case msgPut, msgDelete:
			want = "INFO"
		default:
			continue
		}
		if rec["level"] != want {
			t.Errorf("%v at %v, want %s: %v", rec["msg"], rec["level"], want,
				rec)
		}
	}
	for _, msg := range []string{msgGet, msgRange, msgWatchEvent, msgPut,
		msgDelete} {
		if len(capture.withMsg(t, msg)) == 0 {
			t.Errorf("no %q record", msg)
		}
	}
}

// log.md, etcd: a read that failed is at INFO with its error, so the
// default level shows it.
func TestFailedReadIsInfo(t *testing.T) {
	cli := deadClient(t)
	capture := captureLogs(t)
	key := testPrefix(t) + "k"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := cli.Get(ctx, key, &pb.WorkerReg{}); err == nil {
		t.Fatal("want an error from a dead endpoint")
	}
	if _, _, err := cli.Range(ctx, testPrefix(t)); err == nil {
		t.Fatal("want an error from a dead endpoint")
	}
	for _, msg := range []string{msgGet, msgRange} {
		recs := capture.withMsg(t, msg)
		if len(recs) != 1 || recs[0]["level"] != "INFO" ||
			recs[0]["error"] == nil {
			t.Errorf("%s records %v, want one at INFO with an error", msg, recs)
		}
	}
}
