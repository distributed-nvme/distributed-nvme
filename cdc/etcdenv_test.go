package cdc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A real etcd server, shared by the tests that need one (EU7); a test that
// writes tens of MiB starts one of its own (newPrivateTestClient)
// ---------------------------------------------------------------------------
//
// The same shape as model/etcdenv_test.go's helper, deliberately duplicated
// rather than factored into a package of its own: it is a test fixture, and a
// shared one would put another package between cdc and etcdutil for no gain.
// Everything goes through etcdutil, so the cdc's tests import no etcd client
// either (layout.md, Dependency rules). TestMain starts the server for every
// test binary of the package, so the tests that need no etcd need one too:
// a run without one fails before any test runs (EU7).

// testEndpoint is the client address of the etcd TestMain starts before any
// test runs.
var testEndpoint string

// findEtcdBin returns the etcd binary ETCD_BIN names, or the one on PATH when
// ETCD_BIN is empty or unset. When ETCD_BIN names no readable file, or is
// empty with no etcd on PATH, it exits the test binary with failure before any
// test runs: a run that skipped the etcd-backed tests would print ok while
// testing nothing (dnv-worker.md EU7).
func findEtcdBin() string {
	if bin := os.Getenv("ETCD_BIN"); bin != "" {
		info, err := os.Stat(bin)
		if err != nil || info.IsDir() {
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
		fmt.Fprintln(os.Stderr, noEtcdMsg)
		os.Exit(1)
	}
	return bin
}

// noEtcdMsg is what a test binary prints when it finds no etcd binary.
const noEtcdMsg = "no etcd binary: set ETCD_BIN or put etcd on PATH; " +
	"`make test` also finds the one `make fetch-etcd` fetches into " +
	"integtest/bin/cache"

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
	dir, err := os.MkdirTemp("", "dnv-cdc-")
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
	name := "dnv-cdc-test"
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
		// architecture.md, Components: invocation reference: dnv requires
		// --max-txn-ops=common.EtcdMaxTxnOps of every etcd it runs against,
		// and THIS package's tests write their cdc entries in transactions
		// of that many puts (plantEnv.put).
		"--max-txn-ops", strconv.Itoa(common.EtcdMaxTxnOps),
		// dnv also requires --max-request-bytes=EtcdMaxRequestBytes
		// (dnv-worker.md, Constants this document owns), the cap etcdutil's
		// client sends under.
		"--max-request-bytes", strconv.Itoa(common.EtcdMaxRequestBytes),
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
		_, lastErr = cli.Get(ctx, "dnv-cdc-probe", &pb.CdcEntry{})
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
	// rest of the package's log volume, the records etcdutil emits for every
	// scan, decode and watch event included, goes nowhere.
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	endpoint, stop, err := startEtcd(findEtcdBin())
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot start etcd: %v\n", err)
		os.Exit(1)
	}
	testEndpoint = endpoint
	code := m.Run()
	stop()
	os.Exit(code)
}

// newTestClient returns a Client against the shared etcd (EU7).
func newTestClient(t *testing.T) *etcdutil.Client {
	t.Helper()
	return newClientAt(t, testEndpoint)
}

// newPrivateTestClient starts an etcd of the test's own, which the test's
// end stops and whose data directory it removes, and returns a Client
// against it. A test that writes tens of MiB uses it: the shared etcd keeps
// every value a test writes and its history, so such a test repeated under
// `go test -count=N` would grow it run after run.
func newPrivateTestClient(t *testing.T) *etcdutil.Client {
	t.Helper()
	endpoint, stop, err := startEtcd(findEtcdBin())
	if err != nil {
		t.Fatalf("start the test's own etcd: %v", err)
	}
	t.Cleanup(stop)
	return newClientAt(t, endpoint)
}

// newClientAt returns a Client against the etcd at endpoint, closed when the
// test ends.
func newClientAt(t *testing.T, endpoint string) *etcdutil.Client {
	t.Helper()
	cli, err := etcdutil.New(
		context.Background(),
		[]string{endpoint},
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

// TestNoEtcdFailsTheRun pins dnv-worker.md EU7's rule for a missing binary:
// the test binary, run again with no ETCD_BIN and a PATH that holds no etcd,
// or with an ETCD_BIN that names no file, exits with failure before any test
// runs and says why, instead of skipping the etcd-backed tests and printing
// ok. The child runs no test (-test.run=^$), so a TestMain that went on
// without an etcd would end it with exit status 0.
func TestNoEtcdFailsTheRun(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "etcd")
	dir := t.TempDir()
	unusable := func(bin string) string {
		return fmt.Sprintf("ETCD_BIN=%q is not a readable file", bin)
	}
	for _, tt := range []struct {
		name    string
		etcdBin string
		want    string
	}{
		{"no binary", "", noEtcdMsg},
		{"ETCD_BIN names no file", missing, unusable(missing)},
		{"ETCD_BIN names a directory", dir, unusable(dir)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := []string{"PATH=" + t.TempDir()}
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(kv, "PATH=") &&
					!strings.HasPrefix(kv, "ETCD_BIN=") {
					env = append(env, kv)
				}
			}
			if tt.etcdBin != "" {
				env = append(env, "ETCD_BIN="+tt.etcdBin)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = env
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("a run without etcd ended with %v, want exit "+
					"status 1 (stderr %q)", err, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("stderr %q does not say %q", stderr.String(), tt.want)
			}
		})
	}
}
