package model

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/etcdutil"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A real etcd server, shared by the tests that need one (MD9, EU7); a test
// that writes tens of MiB starts one of its own (newPrivateTestClient)
// ---------------------------------------------------------------------------
//
// The same shape as etcdutil_test.go's helper, deliberately duplicated rather
// than factored into a package of its own: it is a test fixture, and a shared
// one would put a third package between model and etcdutil for no gain.
// Everything here goes through etcdutil, so model's tests import no etcd
// client either (layout.md, Dependency rules).

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

// etcdDirPrefix starts the name of every data directory startEtcd makes in
// the temporary directory, and the pid of the test binary follows it, so
// that reclaimEtcdDirs can tell the directory of a binary that died from one
// of a binary that runs.
const etcdDirPrefix = "dnv-model-"

// etcdStartTries is how many etcds startEtcd starts before it gives up.
const etcdStartTries = 3

// etcdStderrCap is how much of what etcd writes to its standard error a
// start keeps for its error.
const etcdStderrCap = 8 << 10

// startEtcd runs a single-node etcd on free localhost ports with a temporary
// data dir and waits until it serves. A port is free when freePort picks it
// and taken only when etcd binds it, so another process can take it in
// between. An etcd that cannot bind its ports exits at start, before it
// writes its WAL; that start then fails, instead of handing back an endpoint
// another process's etcd serves, and startEtcd starts another, up to
// etcdStartTries in all.
func startEtcd(bin string) (string, func(), error) {
	var errs []error
	for range etcdStartTries {
		endpoint, stop, err := startEtcdOnce(bin)
		if err == nil {
			return endpoint, stop, nil
		}
		errs = append(errs, err)
	}
	return "", nil, errors.Join(errs...)
}

// startEtcdOnce is one start of startEtcd.
func startEtcdOnce(bin string) (string, func(), error) {
	dir, err := os.MkdirTemp(
		"", fmt.Sprintf("%s%d-", etcdDirPrefix, os.Getpid()),
	)
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
		// architecture.md, Components: invocation reference: dnv requires
		// --max-txn-ops=common.EtcdMaxTxnOps of every etcd it runs against;
		// the server's own default is below the compare counts
		// CreateStoragePool's maximum shape, which is what SIZES the
		// requirement (gateway.md, Constants this document owns), and one
		// maximum-shape sp-drain batch (SPD13) reach — and THIS package
		// commits that batch, in TestDrainSpSliceAtTheCeiling (SPD14), as it
		// commits the created flip's transactions (RW19) in
		// TestFlipCreatedAtTheTdCeiling and a maximum-shape ReplaceCntlr,
		// which reads every clone of the SP (dnv-worker.md MD6), in
		// TestReplaceCntlrAtTheCeiling.
		"--max-txn-ops", strconv.Itoa(common.EtcdMaxTxnOps),
		// dnv also requires --max-request-bytes=EtcdMaxRequestBytes
		// (dnv-worker.md, Constants this document owns): the server's own
		// default is below the provisioned flip over every slice of a pool
		// at the widest shape, which SIZES that requirement and which THIS
		// package commits in TestFlipProvisionedAtTheSliceCeiling.
		"--max-request-bytes", strconv.Itoa(common.EtcdMaxRequestBytes),
		"--log-level", "error",
		"--log-outputs", "stderr",
	)
	// etcd's output reaches no stream of the test binary: its standard
	// output goes nowhere, and its standard error only into the error of a
	// start that fails.
	stderr := &etcdStderr{}
	cmd.Stderr = stderr
	// The kernel kills etcd when the test binary dies, however it dies: a
	// kill, the panic of the test timeout or a lost pipe, none of which runs
	// a cleanup. It sends the signal when the thread that started etcd
	// exits; the Go runtime ends a thread only when a goroutine locked to it
	// exits without unlocking it, and no goroutine of this binary does, so
	// that thread lives as long as the binary.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		cleanupDir()
		return "", nil, err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	stop := func() {
		_ = cmd.Process.Kill()
		<-exited
		cleanupDir()
	}
	endpoint := fmt.Sprintf("127.0.0.1:%d", clientPort)
	if err := waitForEtcd(endpoint, exited); err != nil {
		stop()
		return "", nil, stderr.wrap(err)
	}
	// The endpoint answers. It is this etcd's only if this etcd bound both its
	// ports, which it has once it wrote its WAL and while it still runs.
	_, err = os.Stat(filepath.Join(dir, "data", "member", "wal"))
	select {
	case <-exited:
		err = errors.New("it exited")
	default:
	}
	if err != nil {
		stop()
		return "", nil, stderr.wrap(fmt.Errorf(
			"etcd at %s is not the one started: %w", endpoint, err))
	}
	return endpoint, stop, nil
}

// waitForEtcd polls the server until one point read succeeds, and gives up
// once the etcd process has exited.
func waitForEtcd(endpoint string, exited <-chan struct{}) error {
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
		select {
		case <-exited:
			return errors.New("etcd exited before it served")
		default:
		}
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

// etcdStderr keeps the first etcdStderrCap bytes etcd writes to its standard
// error. It takes every write whole, so etcd never sees a write to its
// standard error fail.
type etcdStderr struct {
	mu  sync.Mutex
	buf []byte
}

func (s *etcdStderr) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if room := etcdStderrCap - len(s.buf); room > 0 {
		s.buf = append(s.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// wrap returns err with what etcd wrote to its standard error, if anything.
func (s *etcdStderr) wrap(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) == 0 {
		return err
	}
	return fmt.Errorf("%w (etcd stderr: %q)", err, s.buf)
}

// reclaimEtcdDirs removes the data directories startEtcd made for a test
// binary that no longer runs: a binary that was killed, or whose test timed
// out, removes none of its own. The pid in a directory's name says whose it
// is; the directories of a binary that runs, this one's or a sibling's, stay,
// and so does every name of another shape.
func reclaimEtcdDirs() {
	tmp := os.TempDir()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	for _, entry := range entries {
		rest, ok := strings.CutPrefix(entry.Name(), etcdDirPrefix)
		if !ok || !entry.IsDir() {
			continue
		}
		pidText, _, ok := strings.Cut(rest, "-")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil || pid <= 0 || pidRuns(pid) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(tmp, entry.Name()))
	}
}

// pidRuns reports whether a process with the pid exists: every answer of the
// kernel but "no such process", a refusal included, counts as one.
func pidRuns(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func TestMain(m *testing.M) {
	// model logs nothing of its own, but every etcdutil call it makes emits
	// a record of log.md, etcd. The tests assert behavior, not logs, so the
	// records go nowhere and keep the test output readable.
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if os.Getenv(etcdChildEnv) == "1" {
		runEtcdChild()
	}
	bin := findEtcdBin()
	reclaimEtcdDirs()
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

// newTestClient returns a Client against the shared etcd (EU7).
func newTestClient(t *testing.T) *etcdutil.Client {
	t.Helper()
	return newClientAt(t, testEndpoint)
}

// newPrivateTestClient starts an etcd of the test's own, which the test's
// end stops and whose data directory it removes, and returns a Client
// against it. A test that writes tens of MiB uses it: the shared etcd keeps
// every value a test writes and its history, so such a test repeated under
// `go test -count=N` would fill etcd's storage quota, past which etcd
// refuses every write of every test that runs after it.
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

// testCid gives each test its own cluster id, so that the shared etcd needs no
// cleanup between tests: two clusters never share a key (architecture.md,
// cluster_id derivation). Nothing deletes what a test writes there, so a test
// that writes tens of MiB runs on an etcd of its own (newPrivateTestClient).
func testCid(t *testing.T) uint64 {
	t.Helper()
	// The creation epoch is a per-invocation counter, not 0: ClusterId folds
	// it in (architecture.md, cluster_id derivation), so every call —
	// including the second and third iteration
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

// etcdChildEnv puts the re-executed test binary into the child mode of
// TestEtcdDiesWithTheTestBinary.
const etcdChildEnv = "DNV_MODEL_ETCD_CHILD"

// etcdChildReady is the line the child prints once its etcd serves.
const etcdChildReady = "etcd ready"

// runEtcdChild is the child mode of TestEtcdDiesWithTheTestBinary: it starts
// an etcd, prints etcdChildReady, and sleeps until it is killed. It never
// returns.
func runEtcdChild() {
	if _, _, err := startEtcd(findEtcdBin()); err != nil {
		fmt.Fprintf(os.Stderr, "cannot start etcd: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(etcdChildReady)
	time.Sleep(time.Hour)
	os.Exit(1)
}

// etcdPidsOf returns the pids of the processes whose arguments name dataDir,
// the etcd that serves from it. A process that has exited and not yet been
// reaped has no arguments left, so it is not among them.
func etcdPidsOf(t *testing.T, dataDir string) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if slices.Contains(strings.Split(string(raw), "\x00"), dataDir) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// TestEtcdDiesWithTheTestBinary: the etcd a launcher started dies with the
// test binary that started it, even when that binary is killed and runs no
// cleanup, and the next binary's TestMain removes the data directory the
// killed one left, while the directory of a binary that runs, this one's,
// stays. The first child is this test binary run again in etcdChildEnv's
// mode, killed with SIGKILL once its etcd serves; the second runs no test,
// so it does no more than TestMain does.
func TestEtcdDiesWithTheTestBinary(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), etcdChildEnv+"=1")
	// A run that fails before it kills the child leaves no child behind.
	child.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	child.Stderr = os.Stderr
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatalf("child stdout: %v", err)
	}
	if err := child.Start(); err != nil {
		t.Fatalf("start the child: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		ready <- strings.TrimSpace(line)
	}()
	select {
	case line := <-ready:
		if line != etcdChildReady {
			t.Fatalf("the child printed %q, want %q", line, etcdChildReady)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("the child's etcd did not come up")
	}
	dirs, err := filepath.Glob(filepath.Join(os.TempDir(),
		fmt.Sprintf("%s%d-*", etcdDirPrefix, child.Process.Pid)))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("the child's data directories: %v (err %v), want one",
			dirs, err)
	}
	t.Cleanup(func() { os.RemoveAll(dirs[0]) })
	dataDir := filepath.Join(dirs[0], "data")
	pids := etcdPidsOf(t, dataDir)
	if len(pids) != 1 {
		t.Fatalf("processes serving from %s: %v, want one etcd", dataDir, pids)
	}
	t.Cleanup(func() {
		if len(etcdPidsOf(t, dataDir)) > 0 {
			_ = syscall.Kill(pids[0], syscall.SIGKILL)
		}
	})

	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill the child: %v", err)
	}
	_ = child.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for len(etcdPidsOf(t, dataDir)) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("etcd %d outlived the test binary that started it",
				pids[0])
		}
		time.Sleep(50 * time.Millisecond)
	}

	next := exec.Command(os.Args[0], "-test.run=^$")
	next.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if output, err := next.CombinedOutput(); err != nil {
		t.Fatalf("the next test binary: %v\n%s", err, output)
	}
	if _, err := os.Stat(dirs[0]); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the killed binary's data directory %s is still there "+
			"after the next binary started (stat: %v)", dirs[0], err)
	}
	own, err := filepath.Glob(filepath.Join(os.TempDir(),
		fmt.Sprintf("%s%d-*", etcdDirPrefix, os.Getpid())))
	if err != nil || len(own) == 0 {
		t.Fatalf("this binary's data directory went with the reclaim "+
			"(err %v)", err)
	}
}
