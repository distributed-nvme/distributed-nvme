package worker

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A real etcd server for the etcd-backed tests (EU7, MD9)
// ---------------------------------------------------------------------------
//
// The same shape as etcdutil_test.go's and model/etcdenv_test.go's helpers,
// deliberately duplicated rather than factored into a package of its own: it
// is a test fixture, and a shared one would put another package between
// worker and etcdutil for no gain. Everything goes through etcdutil, so
// worker's tests import no etcd client either (layout.md, Dependency rules).

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
		// dnv-worker.md, Integration test plan: dnv requires
		// --max-txn-ops=common.EtcdMaxTxnOps of every etcd it runs against; the
		// server's own default is below the transactions that need the
		// flag — among them CreateStoragePool's maximum shape,
		// which is what SIZES the requirement (gateway.md, Constants this
		// document owns), the sp drain's maximum-shape D2 batch (SPD13; SPD14
		// is the tripwire pair that keeps it so), one created-flip
		// transaction (RW19, MaxFlipCreatedPerTxn) and ReplaceCntlr at the
		// ceilings, which reads every clone of the SP (MD6).
		"--max-txn-ops", strconv.Itoa(common.EtcdMaxTxnOps),
		// dnv also requires --max-request-bytes=EtcdMaxRequestBytes
		// (dnv-worker.md, Constants this document owns): the server's own
		// default is below the provisioned flip (RW18) over every slice of a
		// pool at the widest shape, which model's
		// TestFlipProvisionedAtTheSliceCeiling commits.
		"--max-request-bytes", strconv.Itoa(common.EtcdMaxRequestBytes),
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
