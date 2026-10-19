package etcdutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// stopTestEtcd stops the package's etcd with SIGSTOP until the test ends:
// its process stays alive and its sockets stay open, but it answers nothing,
// as a member does that died without a TCP reset. No test of the package
// runs in parallel with another, so no other test sees it stopped.
//
// It returns only once every thread of etcd is stopped. The kernel hands
// the stop to etcd's main thread, which stops the others when it next runs,
// and a thread waiting on a disk write runs only when the write ends. Until
// then etcd's other threads go on serving: a put sent at once can land, and
// any frame etcd sends after the put starts counts as a read, so the
// client's keepalive ping goes out a keepalive time later, past the put's
// deadline.
func stopTestEtcd(t *testing.T) {
	t.Helper()
	if err := testEtcdProc.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("stop etcd: %v", err)
	}
	t.Cleanup(func() {
		if err := testEtcdProc.Signal(syscall.SIGCONT); err != nil {
			t.Errorf("continue etcd: %v", err)
		}
	})
	deadline := time.Now().Add(etcdStopWait)
	for !procStopped(t, testEtcdProc.Pid) {
		if time.Now().After(deadline) {
			t.Fatalf("etcd's threads did not all stop within %v",
				etcdStopWait)
		}
		time.Sleep(time.Millisecond)
	}
}

// etcdStopWait bounds stopTestEtcd's wait for the stop: a disk write of a
// loaded test machine ends well within it.
const etcdStopWait = 20 * time.Second

// procStopped reports whether every thread of the process is in the stopped
// state, read from /proc. The state is the field after the command name,
// which stands in parentheses and may itself hold spaces or parentheses.
func procStopped(t *testing.T, pid int) bool {
	t.Helper()
	dir := fmt.Sprintf("/proc/%d/task", pid)
	tasks, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("list etcd's threads: %v", err)
	}
	for _, task := range tasks {
		stat, err := os.ReadFile(filepath.Join(dir, task.Name(), "stat"))
		if errors.Is(err, fs.ErrNotExist) {
			// The thread exited after the listing.
			continue
		}
		if err != nil {
			t.Fatalf("read etcd's thread state: %v", err)
		}
		end := bytes.LastIndexByte(stat, ')')
		if end < 0 || end+2 >= len(stat) {
			t.Fatalf("parse etcd's thread state %q", stat)
		}
		if state := stat[end+2]; state != 'T' && state != 't' {
			return false
		}
	}
	return true
}

// keepaliveIdle is how long the keepalive test leaves its connection idle
// before etcd stops: a second under the keepalive time, so the client's ping
// goes out about a second after the put starts.
const keepaliveIdle = common.DefaultEtcdKeepaliveTime*time.Second - time.Second

// keepaliveFailBound is how soon after its start the keepalive test's put
// must fail: the rest of the keepalive time after keepaliveIdle, the
// keepalive timeout, and a margin, well under the operation deadline of EU5
// at which a client with no keepalive fails the same put.
const keepaliveFailBound = 4 * time.Second

// TestKeepaliveFailsAPutOnAStoppedEtcd: once etcd stops answering, a put
// fails when the client's keepalive ping goes unanswered, with an error that
// names the lost connection, and not at the operation deadline of EU5
// (EU1). The client runs with the production keepalive time, which is
// gRPC's floor, and a keepalive timeout of one second; the connection's last
// read is the put before the idle wait, so the ping goes out at the
// keepalive time after it and the put fails about two seconds after it
// starts.
func TestKeepaliveFailsAPutOnAStoppedEtcd(t *testing.T) {
	tmo := prodTimeouts()
	tmo.keepaliveTimeout = time.Second
	cli, err := newClient(context.Background(), []string{testEndpoint},
		time.Second, tmo)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	ctx := context.Background()
	key := testPrefix(t) + "k"
	// A put that lands first, so the client holds a connection etcd answered
	// on when it stops.
	if err := cli.Put(ctx, key, &pb.WorkerReg{Epoch: 1}); err != nil {
		t.Fatalf("Put before the stop: %v", err)
	}
	time.Sleep(keepaliveIdle)
	stopTestEtcd(t)

	start := time.Now()
	err = cli.Put(ctx, key, &pb.WorkerReg{Epoch: 2})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("a put on a stopped etcd landed")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the put ran to its deadline after %v: %v", elapsed, err)
	}
	if code := status.Code(err); code != codes.Unavailable {
		t.Fatalf("the put failed with code %v, want %v: %v", code,
			codes.Unavailable, err)
	}
	if !strings.Contains(err.Error(), "keepalive") {
		t.Fatalf("the error does not name the unanswered keepalive: %v", err)
	}
	if elapsed > keepaliveFailBound {
		t.Fatalf("the put failed after %v, want at most %v", elapsed,
			keepaliveFailBound)
	}
}
