package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A real etcd server for the etcd-backed tests (EU7, §13)
// ---------------------------------------------------------------------------
//
// The same shape as etcdutil_test.go's and model/etcdenv_test.go's helpers,
// deliberately duplicated rather than factored into a package of its own: it
// is a test fixture, and a shared one would put another package between
// worker and etcdutil for no gain. Everything goes through etcdutil, so
// worker's tests import no etcd client either (layout.md §3).

// testEndpoint is the client URL of the etcd started by TestMain, empty when
// no etcd binary was found.
var testEndpoint string

// findEtcdBin returns the etcd binary named by ETCD_BIN, else the one on
// PATH, else "" (EU7).
func findEtcdBin() string {
	if bin := os.Getenv("ETCD_BIN"); bin != "" {
		if info, err := os.Stat(bin); err == nil && !info.IsDir() {
			return bin
		}
		return ""
	}
	bin, err := exec.LookPath("etcd")
	if err != nil {
		return ""
	}
	return bin
}

// freePort returns a currently free localhost port.
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// startEtcd runs a single-node etcd on free localhost ports with a temporary
// data dir and waits until it serves.
func startEtcd(bin string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "dnv-worker-")
	if err != nil {
		return "", nil, err
	}
	cleanupDir := func() { os.RemoveAll(dir) }
	clientPort, err := freePort()
	if err != nil {
		cleanupDir()
		return "", nil, err
	}
	peerPort, err := freePort()
	if err != nil {
		cleanupDir()
		return "", nil, err
	}
	clientUrl := fmt.Sprintf("http://127.0.0.1:%d", clientPort)
	peerUrl := fmt.Sprintf("http://127.0.0.1:%d", peerPort)
	name := "dnv-worker-test"
	cmd := exec.Command(
		bin,
		"--name", name,
		"--data-dir", filepath.Join(dir, "data"),
		"--listen-client-urls", clientUrl,
		"--advertise-client-urls", clientUrl,
		"--listen-peer-urls", peerUrl,
		"--initial-advertise-peer-urls", peerUrl,
		"--initial-cluster", name+"="+peerUrl,
		"--initial-cluster-token", name,
		"--log-level", "error",
		"--log-outputs", "stderr",
	)
	if err := cmd.Start(); err != nil {
		cleanupDir()
		return "", nil, err
	}
	stop := func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		cleanupDir()
	}
	endpoint := fmt.Sprintf("127.0.0.1:%d", clientPort)
	if err := waitForEtcd(endpoint); err != nil {
		stop()
		return "", nil, err
	}
	return endpoint, stop, nil
}

// waitForEtcd polls the server until one point read succeeds.
func waitForEtcd(endpoint string) error {
	cli, err := etcdutil.New(context.Background(), []string{endpoint}, time.Second)
	if err != nil {
		return err
	}
	defer cli.Close()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, lastErr = cli.Get(ctx, "dnv-worker-probe", &pb.WorkerReg{})
		cancel()
		if lastErr == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("etcd did not come up: %w", lastErr)
}

func TestMain(m *testing.M) {
	// Tests that assert on records install their own capture handler; the
	// rest of the package's log volume goes nowhere.
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if bin := findEtcdBin(); bin != "" {
		endpoint, stop, err := startEtcd(bin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot start etcd: %v\n", err)
			os.Exit(1)
		}
		testEndpoint = endpoint
		code := m.Run()
		stop()
		os.Exit(code)
	}
	// Loud, not silent: without a binary every etcd-backed test t.Skip()s and
	// the package still prints "ok", so a regression in the STM helpers, the
	// MD6 ops or the allocator would sail through a plain `go test ./...`.
	// This banner is the only thing that distinguishes that run from a real
	// one (EU7/MD9 allow the skip; they do not allow it to be invisible).
	fmt.Fprintln(
		os.Stderr,
		"SKIPPING every etcd-backed test: no etcd binary "+
			"(set ETCD_BIN or put etcd on PATH)",
	)
	os.Exit(m.Run())
}

// newTestClient returns a Client against the shared etcd, or skips (EU7).
func newTestClient(t *testing.T) *etcdutil.Client {
	t.Helper()
	if testEndpoint == "" {
		t.Skip("no etcd binary (set ETCD_BIN or put etcd on PATH)")
	}
	cli, err := etcdutil.New(
		context.Background(), []string{testEndpoint}, time.Second,
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := cli.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return cli
}

// nextTestCid gives each etcd-backed test its own cluster id, so the shared
// etcd needs no cleanup between tests.
var nextTestCid atomic.Uint64

func testClusterId() uint64 {
	return 0x7000000000000000 | nextTestCid.Add(1)
}
