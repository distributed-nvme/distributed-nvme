package model

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

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A real etcd server, shared by every test that needs one (MD9, EU7)
// ---------------------------------------------------------------------------
//
// The same shape as etcdutil_test.go's helper, deliberately duplicated rather
// than factored into a package of its own: it is a test fixture, and a shared
// one would put a third package between model and etcdutil for no gain.
// Everything here goes through etcdutil, so model's tests import no etcd
// client either (layout.md §3).

// testEndpoint is the client URL of the etcd started by TestMain, empty when
// no etcd binary was found.
var testEndpoint string

// findEtcdBin returns the etcd binary named by ETCD_BIN, else the one on
// PATH, else "" (EU7: no dependency on the etcd server module).
func findEtcdBin() string {
	if bin := os.Getenv("ETCD_BIN"); bin != "" {
		info, err := os.Stat(bin)
		if err != nil || info.IsDir() {
			// A misconfigured ETCD_BIN must not look like "no etcd
			// available": silently skipping would hide the whole
			// etcd-backed suite from a run that asked for it (EU7).
			fmt.Fprintf(
				os.Stderr,
				"ETCD_BIN=%q is not a readable file\n",
				bin,
			)
			os.Exit(1)
		}
		return bin
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
	dir, err := os.MkdirTemp("", "dnv-model-")
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
	name := "dnv-model-test"
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
	cmd.Stdout = nil
	cmd.Stderr = nil
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
	cli, err := etcdutil.New(
		context.Background(),
		[]string{endpoint},
		time.Second,
	)
	if err != nil {
		return err
	}
	defer cli.Close()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, lastErr = cli.Get(ctx, "dnv-model-probe", &pb.SpName{})
		cancel()
		if lastErr == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("etcd did not come up: %w", lastErr)
}

func TestMain(m *testing.M) {
	// model logs nothing of its own, but every etcdutil call it makes emits
	// a log.md §5.3 record. The tests assert behavior, not logs, so the
	// records go nowhere and keep the test output readable.
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
		context.Background(),
		[]string{testEndpoint},
		time.Second,
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

// testCid gives each test its own cluster id, so that the shared etcd needs no
// cleanup between tests: two clusters never share a key (§5.2).
func testCid(t *testing.T) uint64 {
	t.Helper()
	// The creation epoch is a per-invocation counter, not 0: ClusterId folds
	// it in (§5.2), so every call — including the second and third iteration
	// of the same test under `go test -count=3`, and two tests running in
	// parallel — gets its own key space. Deriving it from t.Name() alone made
	// a test that writes keys and does not delete them see its previous
	// iteration's leftovers (TestLoadSpRevAdvances did exactly that).
	return ClusterId(t.Name(), testCidSeq.Add(1))
}

// testCidSeq numbers the key spaces testCid hands out.
var testCidSeq atomic.Uint64

// mustPut writes one message or fails the test.
func mustPut(
	t *testing.T,
	cli *etcdutil.Client,
	key string,
	msg proto.Message,
) {
	t.Helper()
	if err := cli.Put(context.Background(), key, msg); err != nil {
		t.Fatalf("Put %s: %v", key, err)
	}
}
