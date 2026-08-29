package common

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/distributed-nvme/distributed-nvme/pb"
)

// ---------------------------------------------------------------------------
// Shared test plumbing: a capturing logger with the production handler chain
// (TraceIdHandler over a JSONHandler), used by log_test, osclient_test and
// interceptor_test (log.md §7, osclient.md §8.7, grpc.md §6.4).
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
// duration of the test, using the same handler chain as init() (log.md R2).
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
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
// Trace id (log.md §7.1, §7.2)
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
// TruncForLog (log.md §7.3, R11)
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

// ---------------------------------------------------------------------------
// PbToLogValue (log.md §7.4, §7.5, R10)
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
		Revision: 9,
		MigrId:   30,
		BmIdx:    0,
		Bitmap:   []byte{0x01, 0x02, 0x03, 0x04},
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
		SpLevel:       pb.SpLevel_SP_LEVEL_READONLY,
		StandbyIdList: []uint64{5, 6, 7},
	}
	sideValue := PbToLogValue(side).(map[string]any)
	if got := sideValue["sp_level"]; got != "SP_LEVEL_READONLY" {
		t.Errorf("sp_level = %v, want SP_LEVEL_READONLY", got)
	}
	list, ok := sideValue["standby_id_list"].([]any)
	if !ok || len(list) != 3 || list[0] != uint64(5) {
		t.Errorf("standby_id_list rendered as %v (%T)",
			sideValue["standby_id_list"], sideValue["standby_id_list"])
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
		Revision: 9,
		CntlrIdToDmError: map[uint64]*pb.ResInfo{
			42: {
				ResName: "dnv-x-0-y",
				Status:  pb.ResStatus_RES_STATUS_ERROR,
				Details: "boom",
			},
		},
	}
	infoValue := PbToLogValue(info).(map[string]any)
	errMap, ok := infoValue["cntlr_id_to_dm_error"].(map[string]any)
	if !ok {
		t.Fatalf("map field rendered as %T", infoValue["cntlr_id_to_dm_error"])
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
// Levels (log.md §7.6, R6)
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
