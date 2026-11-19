package common

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Shared test plumbing: a capturing logger with the production handler chain
// (TraceIdHandler over a JSONHandler), used by log_test, osclient_test and
// interceptor_test (log.md R2; osclient.md, Logging; grpc.md L2 to L4).
// ---------------------------------------------------------------------------

// syncBuffer is a bytes.Buffer safe for the concurrent writes produced by
// several goroutines logging at once.
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
	buf    *syncBuffer
	logger *slog.Logger
}

// records decodes every captured JSON line.
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

// withMsg returns every captured record whose "msg" equals msg.
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

// onlyMsg returns the single captured record with the given msg, failing if
// there is not exactly one.
func (c *logCapture) onlyMsg(t *testing.T, msg string) map[string]any {
	t.Helper()
	recs := c.withMsg(t, msg)
	if len(recs) != 1 {
		t.Fatalf("want exactly 1 %q record, got %d\n%s",
			msg, len(recs), c.buf.String())
	}
	return recs[0]
}

// msgs returns the "msg" of every captured record, in order.
func (c *logCapture) msgs(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, rec := range c.records(t) {
		msg, _ := rec["msg"].(string)
		out = append(out, msg)
	}
	return out
}

// captureLogs installs a capturing logger as the process default for the
// duration of the test, using the same handler chain as init() (log.md R2),
// and sets the process log level to Debug for that time, so the capture
// holds every record a daemon at --log-level debug writes; a test of the
// level itself sets the level it needs after the capture (atLogLevel).
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	atLogLevel(t, slog.LevelDebug)
	buf := &syncBuffer{}
	handler := &TraceIdHandler{
		Handler: slog.NewJSONHandler(
			buf,
			&slog.HandlerOptions{Level: logLevel},
		),
	}
	logger := slog.New(handler)
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logCapture{buf: buf, logger: logger}
}

// ---------------------------------------------------------------------------
// Trace id (log.md R5, R12)
// ---------------------------------------------------------------------------

func TestTraceIdHandlerInjectsTraceId(t *testing.T) {
	capture := captureLogs(t)

	ctx := WithTraceId(context.Background(), "a1b2c3d4e5f60718")
	slog.InfoContext(ctx, "with trace")
	slog.InfoContext(context.Background(), "without trace")

	withTrace := capture.onlyMsg(t, "with trace")
	if got := withTrace[TraceIdLogKey]; got != "a1b2c3d4e5f60718" {
		t.Errorf("trace_id = %v, want a1b2c3d4e5f60718", got)
	}
	withoutTrace := capture.onlyMsg(t, "without trace")
	if _, ok := withoutTrace[TraceIdLogKey]; ok {
		t.Errorf("record without ctx trace id carries %s: %v",
			TraceIdLogKey, withoutTrace)
	}
}

func TestTraceIdFromCtx(t *testing.T) {
	if _, ok := TraceIdFromCtx(nil); ok { //nolint:staticcheck // nil ctx is explicitly supported
		t.Error("TraceIdFromCtx(nil) reported a trace id")
	}
	if _, ok := TraceIdFromCtx(context.Background()); ok {
		t.Error("bare ctx reported a trace id")
	}
	if _, ok := TraceIdFromCtx(WithTraceId(context.Background(), "")); ok {
		t.Error("empty trace id reported as present")
	}
	traceId, ok := TraceIdFromCtx(WithTraceId(context.Background(), "t-123"))
	if !ok || traceId != "t-123" {
		t.Errorf("TraceIdFromCtx = (%q, %v), want (\"t-123\", true)", traceId, ok)
	}
}

// R12: derived loggers must keep injecting the trace id.
func TestDerivedLoggersKeepTraceId(t *testing.T) {
	capture := captureLogs(t)
	ctx := WithTraceId(context.Background(), "derived-id")

	capture.logger.With(slog.String("extra", "attr")).
		InfoContext(ctx, "with attrs")
	capture.logger.WithGroup("grp").
		InfoContext(ctx, "with group", slog.String("inner", "v"))
	capture.logger.With(slog.String("a", "1")).WithGroup("g").
		With(slog.String("b", "2")).InfoContext(ctx, "with both")

	// With(...) keeps the attribute at the top level; under WithGroup(name)
	// the handler's attrs land inside that group, as slog prescribes — what
	// matters for R12 is that the trace id is still injected.
	if got := capture.onlyMsg(t, "with attrs")[TraceIdLogKey]; got != "derived-id" {
		t.Errorf("with attrs: trace_id = %v, want derived-id", got)
	}
	for msg, group := range map[string]string{"with group": "grp", "with both": "g"} {
		rec := capture.onlyMsg(t, msg)
		grouped, ok := rec[group].(map[string]any)
		if !ok {
			t.Fatalf("%s: group %q missing from record %v", msg, group, rec)
		}
		if got := grouped[TraceIdLogKey]; got != "derived-id" {
			t.Errorf("%s: %s.trace_id = %v, want derived-id (record %v)",
				msg, group, got, rec)
		}
	}
}

func TestNewTraceId(t *testing.T) {
	first := NewTraceId()
	if len(first) != 16 {
		t.Errorf("NewTraceId() = %q, want 16 hex chars", first)
	}
	if first == NewTraceId() {
		t.Error("NewTraceId() returned the same id twice")
	}
}

// ---------------------------------------------------------------------------
// TruncForLog (log.md R11)
// ---------------------------------------------------------------------------

func TestTruncForLog(t *testing.T) {
	short := "a short line of file data"
	if got := TruncForLog(short); got != short {
		t.Errorf("short string mangled: %q", got)
	}

	exact := strings.Repeat("x", LogStrDataLimit)
	if got := TruncForLog(exact); got != exact {
		t.Errorf("string of exactly the limit was truncated: %q", got)
	}

	long := strings.Repeat("y", LogStrDataLimit+10)
	got := TruncForLog(long)
	wantPrefix := strings.Repeat("y", LogStrDataLimit) + "..."
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("long string not truncated at %d chars: %q",
			LogStrDataLimit, got)
	}
	if !strings.HasSuffix(got, "(138 chars total)") {
		t.Errorf("missing total-length suffix: %q", got)
	}

	// Multi-byte runes are counted as characters, not bytes.
	runes := strings.Repeat("世", LogStrDataLimit+1)
	got = TruncForLog(runes)
	if !strings.HasSuffix(got, "(129 chars total)") {
		t.Errorf("rune counting is wrong: %q", got)
	}
	if n := len([]rune(strings.TrimSuffix(got, "...(129 chars total)"))); n != LogStrDataLimit {
		t.Errorf("kept %d runes, want %d", n, LogStrDataLimit)
	}

	binary := string([]byte{0x00, 0xff, 0xfe, 0x41})
	if got := TruncForLog(binary); got != "<binary 4 bytes>" {
		t.Errorf("TruncForLog(binary) = %q, want <binary 4 bytes>", got)
	}
}

// truncByRunes is the reference TruncForLog must equal, written the plain
// way: the whole string as runes, the first LogStrDataLimit of them kept.
func truncByRunes(s string) string {
	if !utf8.ValidString(s) {
		return fmt.Sprintf("<binary %d bytes>", len(s))
	}
	runes := []rune(s)
	if len(runes) <= LogStrDataLimit {
		return s
	}
	return fmt.Sprintf("%s...(%d chars total)",
		string(runes[:LogStrDataLimit]), len(runes))
}

// TestTruncForLogCopiesOnlyWhatItKeeps holds TruncForLog to the cost of
// what it keeps: a file read logs the file's data through it (log.md R11),
// and the cn agent reads a thin_dump document of tens of MB that way, so a
// copy of the whole input per call is a large allocation on every read. The
// output is the reference's, byte for byte, on each kind of input.
func TestTruncForLogCopiesOnlyWhatItKeeps(t *testing.T) {
	limit := LogStrDataLimit
	mixed := strings.Repeat("a世", limit)
	for name, s := range map[string]string{
		"empty":              "",
		"ascii at the limit": strings.Repeat("x", limit),
		"ascii one over":     strings.Repeat("x", limit+1),
		"ascii 1 MiB":        strings.Repeat("x", 1<<20),
		"wide one over":      strings.Repeat("世", limit+1),
		"wide 300":           strings.Repeat("世", 300),
		"mixed":              mixed,
		"mixed cut inside":   "z" + mixed,
		"binary":             string([]byte{0x00, 0xff, 0xfe, 0x41}),
		"binary at the end":  strings.Repeat("x", 300) + "\xff",
	} {
		if got, want := TruncForLog(s), truncByRunes(s); got != want {
			t.Errorf("%s: TruncForLog = %q, want %q", name, got, want)
		}
	}

	// TotalAlloc counts every goroutine's allocations, so the bound sits
	// far from both sides: a quarter of the input, against a copy of four
	// bytes per character above it and a kept part of 128 characters below.
	big := strings.Repeat("y", 4<<20)
	_ = TruncForLog(big)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	const calls = 4
	for i := 0; i < calls; i++ {
		_ = TruncForLog(big)
	}
	runtime.ReadMemStats(&after)
	perCall := (after.TotalAlloc - before.TotalAlloc) / calls
	if perCall > uint64(len(big)/4) {
		t.Errorf("TruncForLog of %d bytes allocated %d bytes a call, want "+
			"at most %d", len(big), perCall, len(big)/4)
	}
}

// ---------------------------------------------------------------------------
// PbToLogValue (log.md R10)
// ---------------------------------------------------------------------------

func TestPbToLogValueBytesAndNesting(t *testing.T) {
	req := &pb.PushMigrBitmapRequest{
		ClusterId: 16981786240730056190,
		DnId:      3,
		SidePointer: &pb.SidePointer{
			SpId:   17,
			LegId:  21,
			SideId: 22,
		},
		MigrId: 30,
		BmIdx:  0,
		Bitmap: []byte{0x01, 0x02, 0x03, 0x04},
	}

	value, ok := PbToLogValue(req).(map[string]any)
	if !ok {
		t.Fatalf("PbToLogValue returned %T, want map[string]any", PbToLogValue(req))
	}
	if got := value["bitmap"]; got != "<4 bytes>" {
		t.Errorf("bitmap = %v, want <4 bytes>", got)
	}
	sidePointer, ok := value["side_pointer"].(map[string]any)
	if !ok {
		t.Fatalf("side_pointer = %T, want a nested map", value["side_pointer"])
	}
	if sidePointer["sp_id"] != uint64(17) || sidePointer["side_id"] != uint64(22) {
		t.Errorf("side_pointer rendered as %v", sidePointer)
	}
	if value["cluster_id"] != uint64(16981786240730056190) {
		t.Errorf("cluster_id = %v (%T), want the exact uint64",
			value["cluster_id"], value["cluster_id"])
	}
	// bm_idx is the proto3 zero value, so it is not populated.
	if _, ok := value["bm_idx"]; ok {
		t.Errorf("unpopulated field bm_idx appears: %v", value)
	}

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("rendered value does not marshal as JSON: %v", err)
	}
	if strings.Contains(string(raw), "AQIDBA") { // base64 of the payload
		t.Errorf("raw bitmap payload leaked into JSON: %s", raw)
	}
	// encoding/json escapes < and > as \u003c / \u003e.
	if !strings.Contains(string(raw), `4 bytes`) {
		t.Errorf("JSON is missing the redacted bitmap: %s", raw)
	}
}

func TestPbToLogValueEnumsListsMaps(t *testing.T) {
	// Enum by name + repeated scalar + repeated message.
	side := &pb.SyncupSideRequest{
		SideConf: &pb.SyncupSideRequest_SideConf{
			SpLevel:       pb.SpLevel_SP_LEVEL_READONLY,
			StandbyIdList: []uint64{5, 6, 7},
		},
	}
	sideValue := PbToLogValue(side).(map[string]any)
	sideConf, ok := sideValue["side_conf"].(map[string]any)
	if !ok {
		t.Fatalf("side_conf rendered as %T", sideValue["side_conf"])
	}
	if got := sideConf["sp_level"]; got != "SP_LEVEL_READONLY" {
		t.Errorf("sp_level = %v, want SP_LEVEL_READONLY", got)
	}
	list, ok := sideConf["standby_id_list"].([]any)
	if !ok || len(list) != 3 || list[0] != uint64(5) {
		t.Errorf("standby_id_list rendered as %v (%T)",
			sideConf["standby_id_list"], sideConf["standby_id_list"])
	}

	dn := &pb.SyncupDnRequest{
		SidePointerList: []*pb.SidePointer{
			{SpId: 1, SideId: 2},
			{SpId: 3, SideId: 4},
		},
	}
	dnValue := PbToLogValue(dn).(map[string]any)
	pointers, ok := dnValue["side_pointer_list"].([]any)
	if !ok || len(pointers) != 2 {
		t.Fatalf("side_pointer_list rendered as %v", dnValue["side_pointer_list"])
	}
	first, ok := pointers[0].(map[string]any)
	if !ok || first["sp_id"] != uint64(1) {
		t.Errorf("repeated message element rendered as %v", pointers[0])
	}

	// Map with a non-string key and a message value.
	info := &pb.SideInfo{
		CnIdToDmError: map[uint64]*pb.ResInfo{
			42: {
				ResName: "dnv-x-0-y",
				Status:  pb.ResStatus_RES_STATUS_ERROR,
				Details: "boom",
			},
		},
	}
	infoValue := PbToLogValue(info).(map[string]any)
	errMap, ok := infoValue["cn_id_to_dm_error"].(map[string]any)
	if !ok {
		t.Fatalf("map field rendered as %T", infoValue["cn_id_to_dm_error"])
	}
	entry, ok := errMap["42"].(map[string]any)
	if !ok {
		t.Fatalf("map key was not stringified: %v", errMap)
	}
	if entry["status"] != "RES_STATUS_ERROR" || entry["details"] != "boom" {
		t.Errorf("map value rendered as %v", entry)
	}

	for name, value := range map[string]any{
		"side": sideValue, "dn": dnValue, "info": infoValue,
	} {
		if _, err := json.Marshal(value); err != nil {
			t.Errorf("%s value does not marshal as JSON: %v", name, err)
		}
	}
}

func TestPbToLogValueNil(t *testing.T) {
	if got := PbToLogValue(nil); got != nil {
		t.Errorf("PbToLogValue(nil) = %v, want nil", got)
	}
	var typedNil *pb.SidePointer
	if got := PbToLogValue(typedNil); got != nil {
		t.Errorf("PbToLogValue(typed nil) = %v, want nil", got)
	}
}

// ---------------------------------------------------------------------------
// Levels (log.md R6)
// ---------------------------------------------------------------------------

func TestSetLogLevelSuppressesInfo(t *testing.T) {
	capture := captureLogs(t)
	t.Cleanup(func() { SetLogLevel(slog.LevelInfo) })

	ctx := context.Background()
	slog.InfoContext(ctx, "info before")

	SetLogLevel(slog.LevelWarn) // what dnvctl's main() does
	slog.InfoContext(ctx, "info after")
	slog.WarnContext(ctx, "warn after")

	got := capture.msgs(t)
	want := []string{"info before", "warn after"}
	if len(got) != len(want) {
		t.Fatalf("captured %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("captured %v, want %v", got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// The stream init() chose (log.md R2, R3)
// ---------------------------------------------------------------------------
//
// Neither a bytes.Buffer nor a swap of the os.Stdout/os.Stderr package
// variables can observe the handler init() built: slog.NewJSONHandler holds
// the *os.File it was given at package-init time and writes to that file
// descriptor forever. Short of dup2-ing over that descriptor, the only
// faithful observer is a second process, so this pin re-runs the test binary
// in the child mode below — the same idiom etcdutil's one-record-per-line test
// uses.

// stderrChildEnv puts the re-executed test binary into the child mode of
// TestDefaultLoggerWritesStderr.
const stderrChildEnv = "DNV_COMMON_STDERR_CHILD"

// TestMain runs that child when the environment selects it. The child exits
// BEFORE m.Run, which is what keeps the framework's own "PASS"/"ok" lines off
// the stdout the parent asserts is empty.
func TestMain(m *testing.M) {
	if os.Getenv(stderrChildEnv) == "1" {
		runStderrChild()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runStderrChild logs through the process default logger — the one init()
// installed, untouched — at the level dnvctl's main() sets.
func runStderrChild() {
	SetLogLevel(slog.LevelWarn) // what dnvctl's main() does
	slog.Info("info record")
	slog.WarnContext(
		WithTraceId(context.Background(), "trace-abc"),
		"warn record",
	)
}

// TestDefaultLoggerWritesStderr is R2 and R3 together: the chain common's
// init() installs writes JSON to STDERR, honors SetLogLevel, keeps the trace
// id attribute, and leaves stdout — the payload channel — completely empty.
func TestDefaultLoggerWritesStderr(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestDefaultLoggerWritesStderr")
	cmd.Env = append(os.Environ(), stderrChildEnv+"=1")
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("child: %v (stderr %q)", err, errBuf.String())
	}

	if outBuf.Len() != 0 {
		t.Errorf("the default logger wrote %q to stdout, which is the "+
			"payload channel (R2)", outBuf.String())
	}
	stderr := errBuf.String()
	if strings.Contains(stderr, "info record") {
		t.Errorf("an Info record survived SetLogLevel(Warn): %q", stderr)
	}
	if !strings.Contains(stderr, "warn record") {
		t.Errorf("stderr = %q, want the Warn record", stderr)
	}
	// TraceIdHandler must wrap the JSON handler, or a record would be
	// unjoinable to the rest of its invocation.
	if !strings.Contains(stderr, `"trace_id":"trace-abc"`) {
		t.Errorf("stderr = %q, want the trace id attribute", stderr)
	}
	// R2's shape: one JSON record per line, nothing else on the stream.
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		if line == "" {
			continue
		}
		rec := make(map[string]any)
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("non-JSON line on stderr: %q", line)
		}
	}
}

// ---------------------------------------------------------------------------
// The log level and its change by signal (log.md R6, R9, R13)
// ---------------------------------------------------------------------------

// atLogLevel sets the process log level for the duration of the test and puts
// back the level it found.
func atLogLevel(t *testing.T, level slog.Level) {
	t.Helper()
	prev := logLevel.Level()
	logLevel.Set(level)
	t.Cleanup(func() { logLevel.Set(prev) })
}

// R6: --log-level takes debug, info and warn, in any case, and nothing else;
// the refusal names the three words and the value as given.
func TestParseLogLevel(t *testing.T) {
	for raw, want := range map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"DEBUG":   slog.LevelDebug,
		"Warn":    slog.LevelWarn,
		" info\n": slog.LevelInfo,
	} {
		got, err := ParseLogLevel(raw)
		if err != nil || got != want {
			t.Errorf("ParseLogLevel(%q) = %v, %v, want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"error", "", "trace", "info+1", "warning",
		"-4", "0"} {
		_, err := ParseLogLevel(raw)
		if err == nil {
			t.Errorf("ParseLogLevel(%q) was accepted", raw)
			continue
		}
		want := fmt.Sprintf("--log-level must be debug, info or warn, got %q", raw)
		if err.Error() != want {
			t.Errorf("ParseLogLevel(%q) = %q, want %q", raw, err, want)
		}
	}
}

// R6: each level's word reads back as the word the flag takes.
func TestLogLevelName(t *testing.T) {
	for _, word := range []string{LogLevelDebug, LogLevelInfo, LogLevelWarn} {
		level, err := ParseLogLevel(word)
		if err != nil {
			t.Fatalf("ParseLogLevel(%q): %v", word, err)
		}
		if got := LogLevelName(level); got != word {
			t.Errorf("LogLevelName(%v) = %q, want %q", level, got, word)
		}
	}
}

// R9: a read that succeeded is Debug, one that failed Info.
func TestReadLevel(t *testing.T) {
	if got := ReadLevel(nil); got != slog.LevelDebug {
		t.Errorf("ReadLevel(nil) = %v, want DEBUG", got)
	}
	if got := ReadLevel(errors.New("boom")); got != slog.LevelInfo {
		t.Errorf("ReadLevel(err) = %v, want INFO", got)
	}
}

// R9: LogEnabled answers what the process default logger would write, so a
// logging point can skip building a record the level drops.
func TestLogEnabled(t *testing.T) {
	captureLogs(t)
	ctx := context.Background()
	atLogLevel(t, slog.LevelInfo)
	if LogEnabled(ctx, slog.LevelDebug) || !LogEnabled(ctx, slog.LevelInfo) {
		t.Errorf("at Info: debug enabled %v, info enabled %v, want false, true",
			LogEnabled(ctx, slog.LevelDebug), LogEnabled(ctx, slog.LevelInfo))
	}
	SetLogLevel(slog.LevelDebug)
	if !LogEnabled(ctx, slog.LevelDebug) {
		t.Error("at Debug, a debug record is not enabled")
	}
	if got := LogLevel(); got != slog.LevelDebug {
		t.Errorf("LogLevel() = %v after SetLogLevel(Debug)", got)
	}
}

// R13: SIGUSR1 sets Debug, SIGUSR2 the start level, both outright; every
// signal logs one "log level changed" record at WARN with the new level, the
// old one and the signal; the watch ends with its ctx.
func TestWatchLogLevelSignals(t *testing.T) {
	capture := captureLogs(t)
	atLogLevel(t, slog.LevelInfo)
	ctx, cancel := context.WithCancel(
		WithTraceId(context.Background(), "watch-trace"))
	sigCh := make(chan os.Signal)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchLogLevel(ctx, slog.LevelInfo, sigCh)
	}()

	steps := []struct {
		sig       os.Signal
		want      slog.Level
		wantWords [2]string // log_level, from
	}{
		{syscall.SIGUSR1, slog.LevelDebug, [2]string{"debug", "info"}},
		{syscall.SIGUSR1, slog.LevelDebug, [2]string{"debug", "debug"}},
		{syscall.SIGUSR2, slog.LevelInfo, [2]string{"info", "debug"}},
		{syscall.SIGUSR2, slog.LevelInfo, [2]string{"info", "info"}},
	}
	for i, step := range steps {
		sigCh <- step.sig
		waitFor(t, func() bool {
			return len(capture.withMsg(t, MsgLogLevelChanged)) == i+1
		})
		if got := LogLevel(); got != step.want {
			t.Errorf("step %d (%v): level %v, want %v", i, step.sig, got,
				step.want)
		}
		rec := capture.withMsg(t, MsgLogLevelChanged)[i]
		if rec["level"] != "WARN" || rec["log_level"] != step.wantWords[0] ||
			rec["from"] != step.wantWords[1] ||
			rec["signal"] != step.sig.String() ||
			rec[TraceIdLogKey] != "watch-trace" {
			t.Errorf("step %d: record %v, want WARN, log_level %s, from %s, "+
				"signal %s, the ctx's trace id", i, rec, step.wantWords[0],
				step.wantWords[1], step.sig)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not end with its ctx")
	}
}

// R13: a daemon started at Warn goes back to Warn, not to Info, on SIGUSR2.
func TestWatchLogLevelGoesBackToTheStartLevel(t *testing.T) {
	captureLogs(t)
	atLogLevel(t, slog.LevelWarn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal)
	go watchLogLevel(ctx, slog.LevelWarn, sigCh)
	sigCh <- syscall.SIGUSR1
	waitFor(t, func() bool { return LogLevel() == slog.LevelDebug })
	sigCh <- syscall.SIGUSR2
	waitFor(t, func() bool { return LogLevel() == slog.LevelWarn })
}

// R13 with real signals: WatchLogLevel takes SIGUSR1 and SIGUSR2 sent to the
// process, and once stopped a signal changes nothing and ends nothing (the Go
// runtime catches both and takes no action when no channel is notified).
func TestWatchLogLevelRealSignal(t *testing.T) {
	capture := captureLogs(t)
	atLogLevel(t, slog.LevelInfo)
	stop := WatchLogLevel(context.Background(), slog.LevelInfo)
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatalf("kill USR1: %v", err)
	}
	waitFor(t, func() bool { return LogLevel() == slog.LevelDebug })
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR2); err != nil {
		t.Fatalf("kill USR2: %v", err)
	}
	waitFor(t, func() bool {
		return len(capture.withMsg(t, MsgLogLevelChanged)) == 2
	})
	if got := LogLevel(); got != slog.LevelInfo {
		t.Errorf("after USR2: level %v, want INFO", got)
	}
	stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
		t.Fatalf("kill USR1 after stop: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := LogLevel(); got != slog.LevelInfo {
		t.Errorf("a USR1 after stop set the level to %v", got)
	}
	if got := len(capture.withMsg(t, MsgLogLevelChanged)); got != 2 {
		t.Errorf("%d change records after stop, want 2", got)
	}
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
