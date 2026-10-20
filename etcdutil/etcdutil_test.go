package etcdutil

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/proto"

	"github.com/distributed-nvme/distributed-nvme/common"
	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// A real etcd server, shared by every test that needs one (EU7)
// ---------------------------------------------------------------------------

// stderrChildEnv puts the re-executed test binary into the child mode of
// TestStderrStaysOneJsonRecordPerLine.
const stderrChildEnv = "DNV_ETCDUTIL_STDERR_CHILD"

// testEndpoint is the client address of the etcd TestMain starts before any
// test runs, and testEtcdProc its process, which stopTestEtcd stops.
var (
	testEndpoint string
	testEtcdProc *os.Process
)

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
const etcdDirPrefix = "dnv-etcdutil-"

// etcdStartTries is how many etcds startEtcd starts before it gives up.
const etcdStartTries = 3

// etcdStderrCap is how much of what etcd writes to its standard error a
// start keeps for its error.
const etcdStderrCap = 8 << 10

// startEtcd runs a single-node etcd on free localhost ports with a temporary
// data dir, waits until it serves, and returns its endpoint, its process and
// the function that stops it. A port is free when freePort picks it and
// taken only when etcd binds it, so another process can take it in between.
// An etcd that cannot bind its ports exits at start, before it writes its
// WAL; that start then fails, instead of handing back an endpoint another
// process's etcd serves, and startEtcd starts another, up to etcdStartTries
// in all.
func startEtcd(bin string) (string, *os.Process, func(), error) {
	var errs []error
	for range etcdStartTries {
		endpoint, proc, stop, err := startEtcdOnce(bin)
		if err == nil {
			return endpoint, proc, stop, nil
		}
		errs = append(errs, err)
	}
	return "", nil, nil, errors.Join(errs...)
}

// progressNotifyFlag returns the name under which the etcd at bin takes its
// progress notification interval, read from its help text. etcd 3.6 renamed
// the flag and marks the old name for removal, while 3.5, which the
// deployment allows (architecture.md, Components: invocation reference),
// knows only the old name, and an etcd stops at a flag it does not know.
func progressNotifyFlag(bin string) (string, error) {
	cmd := exec.Command(bin, "--help")
	stderr := &etcdStderr{}
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		return "", stderr.wrap(fmt.Errorf("%s --help: %w", bin, err))
	}
	const flag = "--watch-progress-notify-interval"
	if bytes.Contains(out, []byte("\n  "+flag+" ")) {
		return flag, nil
	}
	return "--experimental-watch-progress-notify-interval", nil
}

// startEtcdOnce is one start of startEtcd.
func startEtcdOnce(bin string) (string, *os.Process, func(), error) {
	notifyFlag, err := progressNotifyFlag(bin)
	if err != nil {
		return "", nil, nil, err
	}
	dir, err := os.MkdirTemp(
		"", fmt.Sprintf("%s%d-", etcdDirPrefix, os.Getpid()),
	)
	if err != nil {
		return "", nil, nil, err
	}
	cleanupDir := func() { os.RemoveAll(dir) }
	clientPort, err := freePort()
	if err != nil {
		cleanupDir()
		return "", nil, nil, err
	}
	peerPort, err := freePort()
	if err != nil {
		cleanupDir()
		return "", nil, nil, err
	}
	clientUrl := fmt.Sprintf("http://127.0.0.1:%d", clientPort)
	peerUrl := fmt.Sprintf("http://127.0.0.1:%d", peerPort)
	name := "dnv-etcdutil-test"
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
		// --max-txn-ops=EtcdMaxTxnOps of every etcd it runs against; the
		// server's own default is below the compare counts CreateStoragePool's
		// maximum shape, which is what SIZES the requirement (gateway.md,
		// Constants this document owns), the sp drain's maximum-shape D2 batch
		// (SPD13), one created-flip transaction (RW19, MaxFlipCreatedPerTxn)
		// and a clone flip over every clone of the SP (MD6) reach;
		// gateway/txnbudget_test.go derives the counts, and
		// TestCreateStoragePoolAtTheCeiling, TestDrainSpSliceAtTheCeiling,
		// TestFlipCreatedAtTheTdCeiling and
		// TestFlipCloneProvisionedAtTheCloneCeiling commit them.
		"--max-txn-ops", strconv.Itoa(common.EtcdMaxTxnOps),
		// dnv also requires --max-request-bytes=EtcdMaxRequestBytes of every
		// etcd it runs against (dnv-worker.md, Constants this document
		// owns): the server's own default is below the provisioned flip over
		// every slice of a pool at the widest shape, and below the put
		// TestPutAboveTheClientDefault sends.
		"--max-request-bytes", strconv.Itoa(common.EtcdMaxRequestBytes),
		// A progress notification every second instead of every ten
		// minutes, so that a test can watch the notifications keep a quiet
		// watch open within seconds (watchsilence_test.go).
		notifyFlag, watchProgressInterval.String(),
		"--log-level", "error",
		"--log-outputs", "stderr",
	)
	// The suite parses stderr as JSON records: etcd's output reaches no
	// stream of the test binary. Its standard output goes nowhere, and its
	// standard error only into the error of a start that fails.
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
		return "", nil, nil, err
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
		return "", nil, nil, stderr.wrap(err)
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
		return "", nil, nil, stderr.wrap(fmt.Errorf(
			"etcd at %s is not the one started: %w", endpoint, err))
	}
	return endpoint, cmd.Process, stop, nil
}

// waitForEtcd polls the server until one point read succeeds, and gives up
// once the etcd process has exited.
func waitForEtcd(endpoint string, exited <-chan struct{}) error {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{endpoint},
		DialTimeout: time.Second,
	})
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
		_, lastErr = cli.Get(ctx, "dnv-etcdutil-probe")
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
	if os.Getenv(stderrChildEnv) == "1" {
		// The child of TestStderrStaysOneJsonRecordPerLine: it must not
		// start an etcd, and must print nothing but our own JSON records.
		// It exits before m.Run, which is what keeps the framework's own
		// "PASS"/"ok" lines off the stdout the parent asserts is empty.
		runStderrChild()
		os.Exit(0)
	}
	if os.Getenv(etcdChildEnv) == "1" {
		runEtcdChild()
	}
	bin := findEtcdBin()
	reclaimEtcdDirs()
	endpoint, proc, stop, err := startEtcd(bin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot start etcd: %v\n", err)
		os.Exit(1)
	}
	testEndpoint, testEtcdProc = endpoint, proc
	code := m.Run()
	stop()
	os.Exit(code)
}

// newTestClient returns a Client against the shared etcd (EU7).
func newTestClient(t *testing.T) *Client {
	t.Helper()
	cli, err := New(
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

// testPrefix is a per-test key prefix, space-terminated like every dnv prefix
// (architecture.md, Key grammar).
func testPrefix(t *testing.T) string {
	t.Helper()
	// A per-INVOCATION counter, not just t.Name(): these tests write keys and
	// do not delete them, so a prefix keyed on the name alone made the second
	// and third iteration of `go test -count=3` observe the first one's
	// leftovers (TestRunSTMReadWriteAndDelete failed with "keyB must not
	// exist yet"). The counter also keeps two parallel tests apart.
	return fmt.Sprintf("dnv-test %s %d ", t.Name(), testPrefixSeq.Add(1))
}

// testPrefixSeq numbers the key spaces testPrefix hands out.
var testPrefixSeq atomic.Uint64

// ---------------------------------------------------------------------------
// Captured logs (as in common/log_test.go)
// ---------------------------------------------------------------------------

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type logCapture struct {
	buf *syncBuffer
}

func (c *logCapture) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(c.buf.String()), "\n") {
		if line == "" {
			continue
		}
		rec := make(map[string]any)
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("captured line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// withMsgKey returns every captured record with the given msg and key attr.
func (c *logCapture) withMsgKey(t *testing.T, msg, key string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range c.records(t) {
		if rec["msg"] == msg && rec["key"] == key {
			out = append(out, rec)
		}
	}
	return out
}

// onlyMsgKey returns the single record with the given msg and key.
func (c *logCapture) onlyMsgKey(t *testing.T, msg, key string) map[string]any {
	t.Helper()
	recs := c.withMsgKey(t, msg, key)
	if len(recs) != 1 {
		t.Fatalf("want exactly 1 %q record for key %q, got %d\n%s",
			msg, key, len(recs), c.buf.String())
	}
	return recs[0]
}

// withMsg returns every captured record with the given msg.
func (c *logCapture) withMsg(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range c.records(t) {
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// captureLogs installs a capturing logger with the production handler chain
// for the duration of the test.
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	buf := &syncBuffer{}
	handler := &common.TraceIdHandler{
		Handler: slog.NewJSONHandler(buf, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}),
	}
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logCapture{buf: buf}
}

// wantEpoch asserts that a logged "value" attribute is a WorkerReg with the
// given epoch.
func wantEpoch(t *testing.T, rec map[string]any, epoch float64) {
	t.Helper()
	value, ok := rec["value"].(map[string]any)
	if !ok {
		t.Fatalf("record %v has no decoded value attribute", rec)
	}
	if epoch == 0 {
		if len(value) != 0 {
			t.Fatalf("want empty value, got %v", value)
		}
		return
	}
	if value["epoch"] != epoch {
		t.Fatalf("want epoch %v, got %v (record %v)", epoch, value["epoch"], rec)
	}
}

func newWorkerReg() proto.Message { return &pb.WorkerReg{} }

// ---------------------------------------------------------------------------
// EU2 — point operations and their records
// ---------------------------------------------------------------------------

func TestGetPutDeleteRecords(t *testing.T) {
	cli := newTestClient(t)
	ctx := common.WithTraceId(context.Background(), common.NewTraceId())
	key := testPrefix(t) + "reg"
	capture := captureLogs(t)

	if err := cli.Put(ctx, key, &pb.WorkerReg{Epoch: 7}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	putRec := capture.onlyMsgKey(t, "etcd put", key)
	wantEpoch(t, putRec, 7)
	if _, ok := putRec["error"]; ok {
		t.Fatalf("successful put logged an error: %v", putRec)
	}
	if putRec["trace_id"] == nil {
		t.Fatalf("put record has no trace_id: %v", putRec)
	}

	reg := &pb.WorkerReg{}
	found, err := cli.Get(ctx, key, reg)
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if reg.GetEpoch() != 7 {
		t.Fatalf("want epoch 7, got %d", reg.GetEpoch())
	}
	getRec := capture.onlyMsgKey(t, "etcd get", key)
	if getRec["found"] != true {
		t.Fatalf("want found=true, got %v", getRec["found"])
	}
	wantEpoch(t, getRec, 7)

	missing := testPrefix(t) + "absent"
	other := &pb.WorkerReg{Epoch: 42}
	found, err = cli.Get(ctx, missing, other)
	if err != nil || found {
		t.Fatalf("Get(absent): found=%v err=%v", found, err)
	}
	if other.GetEpoch() != 42 {
		t.Fatalf("msg must be untouched when not found, got %d", other.GetEpoch())
	}
	missRec := capture.onlyMsgKey(t, "etcd get", missing)
	if missRec["found"] != false {
		t.Fatalf("want found=false, got %v", missRec["found"])
	}
	if _, ok := missRec["value"]; ok {
		t.Fatalf("a not-found get must not log a value: %v", missRec)
	}

	if err := cli.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	delRec := capture.onlyMsgKey(t, "etcd delete", key)
	if _, ok := delRec["error"]; ok {
		t.Fatalf("successful delete logged an error: %v", delRec)
	}
	// Deleting an absent key is not an error (EU2).
	if err := cli.Delete(ctx, key); err != nil {
		t.Fatalf("Delete(absent): %v", err)
	}
	found, err = cli.Get(ctx, key, reg)
	if err != nil || found {
		t.Fatalf("Get after delete: found=%v err=%v", found, err)
	}
}

// An all-default message marshals to zero bytes; the key still exists.
func TestGetEmptyMessageIsFound(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	key := testPrefix(t) + "empty"
	capture := captureLogs(t)

	if err := cli.Put(ctx, key, &pb.WorkerReg{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	reg := &pb.WorkerReg{}
	found, err := cli.Get(ctx, key, reg)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatalf("an empty stored message must still be found")
	}
	rec := capture.onlyMsgKey(t, "etcd get", key)
	wantEpoch(t, rec, 0)
}

func TestGetUnmarshalErrorIsWrapped(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	key := testPrefix(t) + "garbage"
	// Write bytes no message can decode, bypassing Put on purpose.
	if _, err := cli.cli.Put(ctx, key, "\xff\xff\xff\xff"); err != nil {
		t.Fatalf("raw put: %v", err)
	}
	capture := captureLogs(t)
	found, err := cli.Get(ctx, key, &pb.WorkerReg{})
	if err == nil {
		t.Fatalf("want an unmarshal error, got found=%v", found)
	}
	if !strings.Contains(err.Error(), key) {
		t.Fatalf("error must name the key: %v", err)
	}
	rec := capture.onlyMsgKey(t, "etcd get", key)
	if _, ok := rec["error"]; !ok {
		t.Fatalf("want an error attribute: %v", rec)
	}
}

// ---------------------------------------------------------------------------
// EU2 — range scans
// ---------------------------------------------------------------------------

func TestRangeOrderRevAndDecode(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	prefix := testPrefix(t)
	for i, epoch := range []uint64{10, 20, 30} {
		key := fmt.Sprintf("%s%02d", prefix, i)
		if err := cli.Put(ctx, key, &pb.WorkerReg{Epoch: epoch}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	capture := captureLogs(t)

	kvs, rev, err := cli.Range(ctx, prefix)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(kvs) != 3 {
		t.Fatalf("want 3 keys, got %d", len(kvs))
	}
	for i, kv := range kvs {
		want := fmt.Sprintf("%s%02d", prefix, i)
		if kv.Key != want {
			t.Fatalf("key %d: want %q, got %q", i, want, kv.Key)
		}
	}
	if rev <= 0 {
		t.Fatalf("want a positive store revision, got %d", rev)
	}
	rangeRecs := capture.withMsg(t, "etcd range")
	if len(rangeRecs) != 1 {
		t.Fatalf("want 1 range record, got %d", len(rangeRecs))
	}
	if rangeRecs[0]["prefix"] != prefix {
		t.Fatalf("want prefix %q, got %v", prefix, rangeRecs[0]["prefix"])
	}
	if rangeRecs[0]["count"] != float64(3) {
		t.Fatalf("want count 3, got %v", rangeRecs[0]["count"])
	}
	if _, ok := rangeRecs[0]["value"]; ok {
		t.Fatalf("a range record must never carry values: %v", rangeRecs[0])
	}

	// Every value a caller actually reads is logged individually (EU2).
	for i, kv := range kvs {
		reg := &pb.WorkerReg{}
		if err := cli.Decode(ctx, kv, reg); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if reg.GetEpoch() != uint64(10*(i+1)) {
			t.Fatalf("kv %d: got epoch %d", i, reg.GetEpoch())
		}
		rec := capture.onlyMsgKey(t, "etcd get", kv.Key)
		if rec["found"] != true {
			t.Fatalf("Decode must log found=true: %v", rec)
		}
		wantEpoch(t, rec, float64(10*(i+1)))
	}
}

// TestRangeModRev checks that Range and RangeDesc hand back each key's
// mod_revision beside its value: a rescan tells a key re-put since the last
// scan from one merely still there by that number alone (VW3).
func TestRangeModRev(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	prefix := testPrefix(t)
	keys := []string{prefix + "a", prefix + "b", prefix + "c"}
	for _, key := range keys {
		if err := cli.Put(ctx, key, &pb.WorkerReg{Epoch: 1}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// Rewrite the middle key: its mod revision must become the largest.
	if err := cli.Put(ctx, keys[1], &pb.WorkerReg{Epoch: 2}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	asc, rev, err := cli.Range(ctx, prefix)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	desc, _, err := cli.RangeDesc(ctx, prefix, 0)
	if err != nil {
		t.Fatalf("RangeDesc: %v", err)
	}
	if len(asc) != 3 || len(desc) != 3 {
		t.Fatalf("want 3 keys each way, got %d and %d", len(asc), len(desc))
	}
	for i, kv := range asc {
		if kv.ModRev <= 0 {
			t.Fatalf("Range: key %q has no mod revision", kv.Key)
		}
		if kv.ModRev > rev {
			t.Fatalf("Range: mod revision %d beyond store revision %d",
				kv.ModRev, rev)
		}
		if mirror := desc[len(desc)-1-i]; mirror.Key != kv.Key ||
			mirror.ModRev != kv.ModRev {
			t.Fatalf("RangeDesc: %q at mod revision %d, Range: %q at %d",
				mirror.Key, mirror.ModRev, kv.Key, kv.ModRev)
		}
	}
	if asc[1].ModRev <= asc[0].ModRev || asc[1].ModRev <= asc[2].ModRev {
		t.Fatalf("rewritten key must carry the largest mod revision: %v", asc)
	}
}

func TestRangeKeysModRev(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	prefix := testPrefix(t)
	keys := []string{prefix + "a", prefix + "b", prefix + "c"}
	for _, key := range keys {
		if err := cli.Put(ctx, key, &pb.WorkerReg{Epoch: 1}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// Rewrite the middle key: its mod revision must become the largest.
	if err := cli.Put(ctx, keys[1], &pb.WorkerReg{Epoch: 2}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	capture := captureLogs(t)

	got, rev, err := cli.RangeKeys(ctx, prefix)
	if err != nil {
		t.Fatalf("RangeKeys: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 keys, got %d", len(got))
	}
	for i, keyRev := range got {
		if keyRev.Key != keys[i] {
			t.Fatalf("key %d: want %q, got %q", i, keys[i], keyRev.Key)
		}
		if keyRev.ModRev <= 0 {
			t.Fatalf("key %q has no mod revision", keyRev.Key)
		}
		if keyRev.ModRev > rev {
			t.Fatalf("mod revision %d beyond store revision %d",
				keyRev.ModRev, rev)
		}
	}
	if got[1].ModRev <= got[0].ModRev || got[1].ModRev <= got[2].ModRev {
		t.Fatalf("rewritten key must carry the largest mod revision: %v", got)
	}
	recs := capture.withMsg(t, "etcd range")
	if len(recs) != 1 || recs[0]["count"] != float64(3) {
		t.Fatalf("want one range record with count 3, got %v", recs)
	}
}

func TestRangeDescOrderAndLimit(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	prefix := testPrefix(t)
	// Capacity keys embed the free extent count, so descending key order is
	// "largest free first" (architecture.md, Finding DN candidates).
	for _, free := range []uint64{1, 5, 9} {
		key := prefix + fmt.Sprintf(common.FreeSpaceFmt, free)
		if err := cli.Put(ctx, key, &pb.WorkerReg{Epoch: free}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	capture := captureLogs(t)

	kvs, rev, err := cli.RangeDesc(ctx, prefix, 0)
	if err != nil {
		t.Fatalf("RangeDesc: %v", err)
	}
	if rev <= 0 {
		t.Fatalf("want a positive store revision, got %d", rev)
	}
	wantKeys := []string{
		prefix + fmt.Sprintf(common.FreeSpaceFmt, uint64(9)),
		prefix + fmt.Sprintf(common.FreeSpaceFmt, uint64(5)),
		prefix + fmt.Sprintf(common.FreeSpaceFmt, uint64(1)),
	}
	if len(kvs) != 3 {
		t.Fatalf("want 3 keys, got %d", len(kvs))
	}
	for i, kv := range kvs {
		if kv.Key != wantKeys[i] {
			t.Fatalf("key %d: want %q, got %q", i, wantKeys[i], kv.Key)
		}
	}

	bounded, _, err := cli.RangeDesc(ctx, prefix, 2)
	if err != nil {
		t.Fatalf("RangeDesc(limit): %v", err)
	}
	if len(bounded) != 2 {
		t.Fatalf("want 2 keys, got %d", len(bounded))
	}
	for i, kv := range bounded {
		if kv.Key != wantKeys[i] {
			t.Fatalf("bounded key %d: want %q, got %q", i, wantKeys[i], kv.Key)
		}
	}
	recs := capture.withMsg(t, "etcd range")
	if len(recs) != 2 {
		t.Fatalf("want 2 range records, got %d", len(recs))
	}
	if recs[1]["count"] != float64(2) {
		t.Fatalf("want count 2 for the bounded scan, got %v", recs[1]["count"])
	}
}

// ---------------------------------------------------------------------------
// EU3 — watch
// ---------------------------------------------------------------------------

func recvEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case event, ok := <-ch:
		if !ok {
			t.Fatalf("event channel closed early")
		}
		return event
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for a watch event")
		return Event{}
	}
}

func TestWatchTypedOrderingAndCtxEnd(t *testing.T) {
	cli := newTestClient(t)
	base := context.Background()
	prefix := testPrefix(t)
	_, rev, err := cli.Range(base, prefix)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	capture := captureLogs(t)

	ctx, cancel := context.WithCancel(base)
	defer cancel()
	eventCh, errCh := cli.WatchTyped(ctx, prefix, rev+1, newWorkerReg)

	keyA, keyB := prefix+"a", prefix+"b"
	if err := cli.Put(base, keyA, &pb.WorkerReg{Epoch: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := cli.Put(base, keyB, &pb.WorkerReg{Epoch: 2}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := cli.Delete(base, keyA); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	first := recvEvent(t, eventCh)
	if first.Type != EventPut || first.Key != keyA {
		t.Fatalf("first event: %+v", first)
	}
	reg, ok := first.Msg.(*pb.WorkerReg)
	if !ok || reg.GetEpoch() != 1 {
		t.Fatalf("first event msg: %+v", first.Msg)
	}
	second := recvEvent(t, eventCh)
	if second.Type != EventPut || second.Key != keyB {
		t.Fatalf("second event: %+v", second)
	}
	if second.Rev <= first.Rev {
		t.Fatalf("revisions must increase: %d then %d", first.Rev, second.Rev)
	}
	third := recvEvent(t, eventCh)
	if third.Type != EventDelete || third.Key != keyA {
		t.Fatalf("third event: %+v", third)
	}
	if third.Msg != nil {
		t.Fatalf("a delete event carries no message: %+v", third.Msg)
	}
	// A put event's Rev is the number a scan reports as the key's ModRev,
	// which VW3's rescan compares; a delete event's is the deletion's own.
	kvs, scanRev, err := cli.Range(base, prefix)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(kvs) != 1 || kvs[0].Key != keyB || kvs[0].ModRev != second.Rev {
		t.Fatalf("scan after the events: %+v, want only %q at mod "+
			"revision %d", kvs, keyB, second.Rev)
	}
	if third.Rev <= second.Rev || third.Rev > scanRev {
		t.Fatalf("delete event at revision %d, want after the put's %d "+
			"and at most the store's %d", third.Rev, second.Rev, scanRev)
	}

	putRec := capture.onlyMsgKey(t, "etcd watch event", keyB)
	if putRec["type"] != "put" {
		t.Fatalf("want type put, got %v", putRec["type"])
	}
	wantEpoch(t, putRec, 2)
	delRecs := capture.withMsgKey(t, "etcd watch event", keyA)
	if len(delRecs) != 2 || delRecs[1]["type"] != "delete" {
		t.Fatalf("want a put then a delete record for %q: %v", keyA, delRecs)
	}
	if _, ok := delRecs[1]["value"]; ok {
		t.Fatalf("a delete watch record must not carry a value: %v", delRecs[1])
	}

	// When ctx ends both channels close without an error (EU3).
	cancel()
	select {
	case err, ok := <-errCh:
		if ok {
			t.Fatalf("want no error on ctx end, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("error channel did not close after ctx end")
	}
	select {
	case _, ok := <-eventCh:
		if ok {
			t.Fatalf("event channel delivered after ctx end")
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("event channel did not close after ctx end")
	}
}

// ---------------------------------------------------------------------------
// EU4 — STM
// ---------------------------------------------------------------------------

func TestRunSTMReadWriteAndDelete(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	prefix := testPrefix(t)
	keyA, keyB := prefix+"a", prefix+"b"
	if err := cli.Put(ctx, keyA, &pb.WorkerReg{Epoch: 3}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	capture := captureLogs(t)

	err := cli.RunSTM(ctx, func(s STM) error {
		reg := &pb.WorkerReg{}
		if !s.Get(keyA, reg) {
			return errors.New("keyA must exist")
		}
		if s.Rev(keyA) <= 0 {
			return errors.New("keyA must have a mod revision")
		}
		if s.Rev(keyB) != 0 {
			return errors.New("keyB must not exist yet")
		}
		s.Put(keyB, &pb.WorkerReg{Epoch: reg.GetEpoch() + 1})
		// A key written in this transaction reads back through the same
		// view, even when its message is all-default.
		back := &pb.WorkerReg{}
		if !s.Get(keyB, back) || back.GetEpoch() != 4 {
			return fmt.Errorf("read-back of keyB: %v", back)
		}
		s.Del(keyA)
		if s.Get(keyA, &pb.WorkerReg{}) {
			return errors.New("keyA must read as deleted")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunSTM: %v", err)
	}

	if found, err := cli.Get(ctx, keyA, &pb.WorkerReg{}); err != nil || found {
		t.Fatalf("keyA after commit: found=%v err=%v", found, err)
	}
	reg := &pb.WorkerReg{}
	if found, err := cli.Get(ctx, keyB, reg); err != nil || !found {
		t.Fatalf("keyB after commit: found=%v err=%v", found, err)
	}
	if reg.GetEpoch() != 4 {
		t.Fatalf("want epoch 4, got %d", reg.GetEpoch())
	}
	// Each STM operation logged its own record (EU4).
	if recs := capture.withMsgKey(t, "etcd put", keyB); len(recs) != 1 {
		t.Fatalf("want 1 stm put record, got %d", len(recs))
	}
	if recs := capture.withMsgKey(t, "etcd delete", keyA); len(recs) != 1 {
		t.Fatalf("want 1 stm delete record, got %d", len(recs))
	}
}

func TestRunSTMRetriesOnConflict(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	key := testPrefix(t) + "counter"
	if err := cli.Put(ctx, key, &pb.WorkerReg{Epoch: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	attempts := 0
	err := cli.RunSTM(ctx, func(s STM) error {
		attempts++
		reg := &pb.WorkerReg{}
		if !s.Get(key, reg) {
			return errors.New("key must exist")
		}
		if attempts == 1 {
			// A concurrent writer invalidates this attempt's read set.
			if err := cli.Put(ctx, key, &pb.WorkerReg{Epoch: 99}); err != nil {
				return err
			}
		}
		s.Put(key, &pb.WorkerReg{Epoch: reg.GetEpoch() + 1})
		return nil
	})
	if err != nil {
		t.Fatalf("RunSTM: %v", err)
	}
	if attempts < 2 {
		t.Fatalf("want at least 2 attempts, got %d", attempts)
	}
	reg := &pb.WorkerReg{}
	if _, err := cli.Get(ctx, key, reg); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if reg.GetEpoch() != 100 {
		t.Fatalf("want the retry to see 99 and store 100, got %d",
			reg.GetEpoch())
	}
}

// precondErr mimics model.ErrPrecondition (MD7): an ordinary error type whose
// Unwrap is the etcdutil abort sentinel.
type precondErr struct {
	reason string
}

func (e *precondErr) Error() string { return "precondition: " + e.reason }

func (e *precondErr) Unwrap() error { return ErrNoCommit }

func TestRunSTMErrNoCommitAbortsWithoutCommitOrRetry(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	prefix := testPrefix(t)
	keyA, keyB := prefix+"a", prefix+"b"
	if err := cli.Put(ctx, keyA, &pb.WorkerReg{Epoch: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	abort := &precondErr{reason: "candidate changed"}
	attempts := 0
	err := cli.RunSTM(ctx, func(s STM) error {
		attempts++
		s.Put(keyB, &pb.WorkerReg{Epoch: 2})
		s.Del(keyA)
		return abort
	})
	if err != abort {
		t.Fatalf("RunSTM must return the callback error unchanged, got %v", err)
	}
	if !errors.Is(err, ErrNoCommit) {
		t.Fatalf("want errors.Is(err, ErrNoCommit)")
	}
	var typed *precondErr
	if !errors.As(err, &typed) {
		t.Fatalf("want the caller's own type back")
	}
	if attempts != 1 {
		t.Fatalf("an abort must not be retried, got %d attempts", attempts)
	}
	// Nothing was committed.
	if found, err := cli.Get(ctx, keyB, &pb.WorkerReg{}); err != nil || found {
		t.Fatalf("keyB must not exist: found=%v err=%v", found, err)
	}
	if found, err := cli.Get(ctx, keyA, &pb.WorkerReg{}); err != nil || !found {
		t.Fatalf("keyA must survive: found=%v err=%v", found, err)
	}
}

func TestRunSTMCallbackErrorAbortsWithoutCommit(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	key := testPrefix(t) + "k"
	plain := errors.New("plain failure")
	err := cli.RunSTM(ctx, func(s STM) error {
		s.Put(key, &pb.WorkerReg{Epoch: 1})
		return plain
	})
	if err != plain {
		t.Fatalf("want the callback error unchanged, got %v", err)
	}
	if found, err := cli.Get(ctx, key, &pb.WorkerReg{}); err != nil || found {
		t.Fatalf("nothing must be committed: found=%v err=%v", found, err)
	}
}

func TestSnapshotReadsOneRevision(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	prefix := testPrefix(t)
	keyA, keyB := prefix+"a", prefix+"b"
	if err := cli.Put(ctx, keyA, &pb.WorkerReg{Epoch: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := cli.Put(ctx, keyB, &pb.WorkerReg{Epoch: 2}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	runs := 0
	var gotA, gotB uint64
	err := cli.Snapshot(ctx, func(s STM) error {
		runs++
		regA := &pb.WorkerReg{}
		if !s.Get(keyA, regA) {
			return errors.New("keyA must exist")
		}
		gotA = regA.GetEpoch()
		if runs == 1 {
			// Both keys are rewritten between the two reads.
			if err := cli.Put(ctx, keyA, &pb.WorkerReg{Epoch: 11}); err != nil {
				return err
			}
			if err := cli.Put(ctx, keyB, &pb.WorkerReg{Epoch: 22}); err != nil {
				return err
			}
		}
		regB := &pb.WorkerReg{}
		if !s.Get(keyB, regB) {
			return errors.New("keyB must exist")
		}
		gotB = regB.GetEpoch()
		return nil
	})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if runs != 1 {
		t.Fatalf("a snapshot must not retry, ran %d times", runs)
	}
	if gotA != 1 || gotB != 2 {
		t.Fatalf("all reads must be served at one revision, got %d and %d",
			gotA, gotB)
	}
}

func TestSnapshotRejectsWrites(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	prefix := testPrefix(t)
	keyA, keyB := prefix+"a", prefix+"b"
	if err := cli.Put(ctx, keyA, &pb.WorkerReg{Epoch: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	err := cli.Snapshot(ctx, func(s STM) error {
		s.Put(keyB, &pb.WorkerReg{Epoch: 2})
		return nil
	})
	if err == nil {
		t.Fatalf("a write inside Snapshot must fail")
	}
	if found, err := cli.Get(ctx, keyB, &pb.WorkerReg{}); err != nil || found {
		t.Fatalf("nothing must be written: found=%v err=%v", found, err)
	}
	err = cli.Snapshot(ctx, func(s STM) error {
		s.Del(keyA)
		return nil
	})
	if err == nil {
		t.Fatalf("a delete inside Snapshot must fail")
	}
	if found, err := cli.Get(ctx, keyA, &pb.WorkerReg{}); err != nil || !found {
		t.Fatalf("keyA must survive: found=%v err=%v", found, err)
	}
}

func TestSnapshotPropagatesCallbackError(t *testing.T) {
	cli := newTestClient(t)
	ctx := context.Background()
	abort := &precondErr{reason: "not loaded"}
	err := cli.Snapshot(ctx, func(s STM) error {
		s.Get(testPrefix(t)+"a", &pb.WorkerReg{})
		return abort
	})
	if err != abort {
		t.Fatalf("want the callback error unchanged, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// EU5/EU6 — timeouts and wrapped errors, no etcd needed
// ---------------------------------------------------------------------------

// deadClient dials an endpoint nothing listens on.
func deadClient(t *testing.T) *Client {
	t.Helper()
	cli, err := New(
		context.Background(),
		[]string{"127.0.0.1:1"},
		100*time.Millisecond,
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func TestPlainOpErrorsAreWrappedAndLogged(t *testing.T) {
	cli := deadClient(t)
	capture := captureLogs(t)
	key := testPrefix(t) + "k"

	// A caller ctx shorter than DefaultEtcdOpTimeout wins (EU5).
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := cli.Get(ctx, key, &pb.WorkerReg{}); err == nil {
		t.Fatalf("want an error from a dead endpoint")
	} else if !strings.Contains(err.Error(), "etcd get") {
		t.Fatalf("error must be wrapped by the helper: %v", err)
	}
	if elapsed := time.Since(start); elapsed > common.DefaultEtcdOpTimeout*time.Second {
		t.Fatalf("the shorter caller ctx must win, waited %v", elapsed)
	}
	rec := capture.onlyMsgKey(t, "etcd get", key)
	if rec["found"] != false {
		t.Fatalf("want found=false, got %v", rec["found"])
	}
	if _, ok := rec["error"]; !ok {
		t.Fatalf("want an error attribute: %v", rec)
	}

	if err := cli.Put(ctx, key, &pb.WorkerReg{Epoch: 1}); err == nil {
		t.Fatalf("want an error from a dead endpoint")
	}
	if _, ok := capture.onlyMsgKey(t, "etcd put", key)["error"]; !ok {
		t.Fatalf("the put record must carry the error")
	}
	if _, _, err := cli.Range(ctx, testPrefix(t)); err == nil {
		t.Fatalf("want an error from a dead endpoint")
	}
	recs := capture.withMsg(t, "etcd range")
	if len(recs) != 1 {
		t.Fatalf("want 1 range record, got %d", len(recs))
	}
	if recs[0]["count"] != float64(0) {
		t.Fatalf("want count 0, got %v", recs[0]["count"])
	}
	if _, ok := recs[0]["error"]; !ok {
		t.Fatalf("the range record must carry the error: %v", recs[0])
	}
	if err := cli.RunSTM(ctx, func(s STM) error {
		s.Get(key, &pb.WorkerReg{})
		return nil
	}); err == nil {
		t.Fatalf("want an error from a dead endpoint")
	} else if !strings.Contains(err.Error(), "etcd stm") {
		t.Fatalf("stm client errors must be wrapped: %v", err)
	}
}

// ---------------------------------------------------------------------------
// EU1 — the etcd client must not write anything of its own (log.md R2)
// ---------------------------------------------------------------------------

// runStderrChild is the child mode of the test below: it exercises a client
// against a dead endpoint with the process default logger, so that everything
// reaching stderr is either our own JSON record or a bug — and stdout, the
// payload channel, stays empty.
func runStderrChild() {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cli, err := New(ctx, []string{"127.0.0.1:1"}, 100*time.Millisecond)
	if err != nil {
		return
	}
	_, _ = cli.Get(ctx, "dnv child probe", &pb.WorkerReg{})
	_ = cli.Put(ctx, "dnv child probe", &pb.WorkerReg{Epoch: 1})
	_, _, _ = cli.Range(ctx, "dnv child ")
	_ = cli.RunSTM(ctx, func(s STM) error {
		s.Get("dnv child probe", &pb.WorkerReg{})
		return nil
	})
	_ = cli.Close()
}

func TestStderrStaysOneJsonRecordPerLine(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestStderrStaysOneJsonRecordPerLine")
	cmd.Env = append(os.Environ(), stderrChildEnv+"=1")
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("child: %v (stderr %q)", err, errBuf.String())
	}
	if outBuf.Len() != 0 {
		t.Fatalf("the child wrote %q to stdout, which is the payload "+
			"channel and must stay empty", outBuf.String())
	}
	out := errBuf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	sawGet := false
	for _, line := range lines {
		if line == "" {
			continue
		}
		rec := make(map[string]any)
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("non-JSON line on stderr: %q", line)
		}
		if rec["msg"] == "etcd get" {
			sawGet = true
		}
	}
	if !sawGet {
		t.Fatalf("the child logged no etcd get record:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// EU3 — compaction. Last, because compaction is store-wide.
// ---------------------------------------------------------------------------

func TestWatchTypedReportsCompactedOnce(t *testing.T) {
	cli := newTestClient(t)
	base := context.Background()
	prefix := testPrefix(t)
	for i := range 3 {
		key := fmt.Sprintf("%s%02d", prefix, i)
		if err := cli.Put(base, key, &pb.WorkerReg{Epoch: 1}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	_, rev, err := cli.Range(base, prefix)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	ctx, cancel := context.WithTimeout(base, 30*time.Second)
	defer cancel()
	if _, err := cli.cli.Compact(
		ctx, rev, clientv3.WithCompactPhysical(),
	); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Watching from before the compaction revision is refused by the server.
	eventCh, errCh := cli.WatchTyped(ctx, prefix, 1, newWorkerReg)
	select {
	case err, ok := <-errCh:
		if !ok || err == nil {
			t.Fatalf("want one error, got ok=%v err=%v", ok, err)
		}
		if !IsCompacted(err) {
			t.Fatalf("want an ErrCompacted report, got %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("no compaction error reported")
	}
	// Both channels close, and the error is reported once (EU3).
	select {
	case err, ok := <-errCh:
		if ok {
			t.Fatalf("want the error channel closed, got another error %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("error channel did not close")
	}
	select {
	case _, ok := <-eventCh:
		if ok {
			t.Fatalf("want the event channel closed")
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("event channel did not close")
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
const etcdChildEnv = "DNV_ETCDUTIL_ETCD_CHILD"

// etcdChildReady is the line the child prints once its etcd serves.
const etcdChildReady = "etcd ready"

// runEtcdChild is the child mode of TestEtcdDiesWithTheTestBinary: it starts
// an etcd, prints etcdChildReady, and sleeps until it is killed. It never
// returns.
func runEtcdChild() {
	if _, _, _, err := startEtcd(findEtcdBin()); err != nil {
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
