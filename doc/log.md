# log.md — Logging Specification (dnv)

Status: **normative**. This document implements the "log" requirement for the dnv
project (see `architecture.md` for system context). It is the base document for
`osclient.md` and `grpc.md`, which both reuse the helpers defined here. Read those
two documents after this one.

Required background files: `architecture.md` (§1 binaries, §5 etcd model, §7
timeouts), `constants.go`, `name_fmt.go`, `schema.proto`.

---

## 1. Scope and placement

* All logging code is shared and lives in the existing shared package `common`
  (the package that already contains `constants.go` and `name_fmt.go`), in a new
  file `common/log.go`.
* Every dnv binary (`dnv-gateway`, `dnv-worker`, `dnv-agent`, `dnv-cdc`, `dnvctl`)
  imports `common`, so the `init()` below installs the default logger before any
  `main()` runs.
* Go ≥ 1.24 is required (`log/slog` needs only 1.21, but `NewTraceId` relies on the
  Go 1.24 guarantee that `crypto/rand.Read` never fails). Dependency:
  `google.golang.org/protobuf` (already required by the schema).

## 2. Normative requirements

R1. Use the standard library `log/slog` only. No third-party logging libraries.
    `fmt.Print*` is never used for operational logging (dnvctl printing command
    *results* to the user is CLI output, not logging, and is exempt).

R2. The handler chain is: `TraceIdHandler` (defined below) wrapping
    `slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})`.
    All logs go to **stdout** as JSON. All handler options stay at their
    defaults except `Level` — see R6. (`Level` is the one deliberate deviation
    from "nil options"; it is required to satisfy the per-binary level rule.)

R3. `common`'s `init()` calls `slog.SetDefault(...)` with that chain. The dnv
    binaries of §1 do not build their own loggers. R2 and R3 bind those five
    binaries only: the `integtest/` drivers are not dnv binaries, and three of
    them re-install the same handler chain over `os.Stderr` because their
    stdout carries the command result the suites parse — the carve-out is
    recorded in `grpc.md` §6.

R4. Every log call uses the context-aware form — `slog.InfoContext`,
    `slog.WarnContext`, `slog.ErrorContext` — and passes the request-scoped
    `ctx`. Use `context.Background()` explicitly only when no operation context
    exists. **Always propagate the request ctx down** into `OsClient` calls,
    etcd calls and gRPC calls; the trace id rides on it (see R5 and `grpc.md`).

R5. The trace id is carried in the context under an unexported key and injected
    into every record as attribute `trace_id` by `TraceIdHandler`. Access only
    through the exported helpers `WithTraceId` / `TraceIdFromCtx` (§3). The gRPC
    interceptors (`grpc.md`) move the trace id between context and gRPC
    metadata; nothing else touches it. On a logger derived with `WithGroup` the
    attribute is nested inside that group rather than at the top level — see
    R12 — so a consumer that greps `trace_id` positionally must account for it.

R6. Log levels per binary:

    | binary | level |
    |---|---|
    | dnv-gateway | Info |
    | dnv-worker | Info |
    | dnv-agent (dn and cn) | Info |
    | dnv-cdc | Info |
    | dnvctl | Warn |

    Implemented with a package-level `slog.LevelVar` (zero value = Info) plus
    `common.SetLogLevel(l slog.Level)`. The four daemons do nothing (default is
    Info). `dnvctl` calls `common.SetLogLevel(slog.LevelWarn)` as the first
    statement of `main()`. Because all the R8 logging points are Info, they are
    automatically silenced in dnvctl — this is intended.

R7. Attributes are always built with the strongly-typed constructors:
    `slog.String`, `slog.Int`, `slog.Int64`, `slog.Uint64`, `slog.Bool`,
    `slog.Duration`, `slog.Time`, `slog.Any`. Never pass loose alternating
    `key, value` arguments. Use `slog.Any` only for slices, maps and the
    `PbToLogValue` output.

R8. The following events MUST be logged at **Info** level, with the exact `msg`
    strings and required attributes listed in §5:
    1. every OS command execution (stdin, stdout, stderr, exit code) —
       implemented once inside `LimitedOsClient` (`osclient.md`);
    2. every file read/write (full path; string data truncated to the first
       128 characters; non-UTF-8 data logged as size only) — implemented once
       inside `LimitedOsClient`;
    3. every gRPC client/server request/reply message (protobuf rendered as
       JSON; `bytes` fields rendered as their size only) — implemented once in
       the interceptors (`grpc.md`);
    4. every etcd read/write (human-readable key, value decoded from protobuf
       and rendered as JSON — never raw protobuf binary) — implemented in a
       small set of central etcd helper functions (§5.3).

R9. Each logging point emits exactly **one** record per operation, on
    completion, so the record can carry the outcome (exit code, error). When the
    operation failed, the same Info record additionally carries
    `slog.String("error", err.Error())`. These logging points do not use other
    levels; failures still reach the caller through the returned error.

R10. Protobuf messages are rendered for logging exclusively through
     `PbToLogValue` (§4), which replaces every `bytes` field, at any nesting
     depth (singular, repeated, map values), with the string `"<N bytes>"`.
     Raw payload bytes never appear in logs. This single helper satisfies both
     the gRPC bytes rule and the etcd human-readable rule.

R11. String data written to / read from files is truncated for logging with
     `TruncForLog` (§4): first `LogStrDataLimit` (= 128) characters, with a
     `...(N chars total)` suffix when truncated; non-UTF-8 content is replaced
     by `"<binary N bytes>"` (this is the "the code should decide" resolution
     for bytes data). Protobuf files are the exception: `ReadProto`/`WriteProto`
     log the decoded message via `PbToLogValue` instead (see `osclient.md`).

R12. `TraceIdHandler` MUST override `WithAttrs` and `WithGroup` to re-wrap the
     returned inner handler. (Embedding alone forwards these calls to the inner
     handler and returns the *inner* type, so `logger.With(...)` would silently
     drop trace-id injection. This is a bug in the naive example; do not
     reproduce it.) Injection happens in `Handle` via `r.AddAttrs`, i.e. as a
     record attribute rather than a handler attribute, so on a logger derived
     with `WithGroup(name)` the `trace_id` lands **inside** that group
     (`{"grp":{"inner":"v","trace_id":"…"}}`) exactly as any other record
     attribute does. That is slog's own grouping rule, not a defect, and
     `TestDerivedLoggersKeepTraceId` pins it. dnv's own logging points never
     open a group, so where a record carries `trace_id` it is at the top
     level.

## 3. Constants to add to `constants.go`

Add to the existing `const` block in `common/constants.go`:

```go
	// Maximum number of characters of string file data included in a log
	// record (see log.md R11).
	LogStrDataLimit = 128
```

(`osclient.md` adds one more constant, `DefaultOsClientLimit`.)

## 4. Reference implementation — `common/log.go` (complete file)

```go
package common

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ---------------------------------------------------------------------------
// Trace id plumbing (log.md R5)
// ---------------------------------------------------------------------------

type contextKey string

const traceIdCtxKey contextKey = "trace_id"

// TraceIdLogKey is the JSON attribute name under which the trace id appears
// in every log record.
const TraceIdLogKey = "trace_id"

// TraceIdMetadataKey is the gRPC metadata key used to propagate the trace id
// between processes (see grpc.md).
const TraceIdMetadataKey = "trace_id"

// WithTraceId returns a ctx carrying the given trace id.
func WithTraceId(ctx context.Context, traceId string) context.Context {
	return context.WithValue(ctx, traceIdCtxKey, traceId)
}

// TraceIdFromCtx extracts the trace id from ctx, if present and non-empty.
func TraceIdFromCtx(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	traceId, ok := ctx.Value(traceIdCtxKey).(string)
	if !ok || traceId == "" {
		return "", false
	}
	return traceId, true
}

// NewTraceId mints a new trace id. Entry points create one (dnvctl per CLI
// invocation, dnv-worker per sync/health round, dnv-gateway for a request
// that arrived without one); the interceptors never invent one (grpc.md T4).
func NewTraceId() string {
	var b [8]byte
	// crypto/rand.Read never returns an error (it panics on failure since
	// Go 1.24), so the result is always a full 8 random bytes.
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// Handler (log.md R2, R12)
// ---------------------------------------------------------------------------

// TraceIdHandler wraps another slog.Handler and adds the trace_id attribute
// from the context to every record.
type TraceIdHandler struct {
	slog.Handler
}

func (h *TraceIdHandler) Handle(ctx context.Context, r slog.Record) error {
	if traceId, ok := TraceIdFromCtx(ctx); ok {
		r.AddAttrs(slog.String(TraceIdLogKey, traceId))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs / WithGroup must re-wrap so that derived loggers
// (logger.With(...), logger.WithGroup(...)) keep trace-id injection (R12).
func (h *TraceIdHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TraceIdHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *TraceIdHandler) WithGroup(name string) slog.Handler {
	return &TraceIdHandler{Handler: h.Handler.WithGroup(name)}
}

// logLevel's zero value is slog.LevelInfo, the level used by dnv-gateway,
// dnv-worker, dnv-agent and dnv-cdc (log.md R6).
var logLevel = new(slog.LevelVar)

// SetLogLevel changes the process-wide log level. dnvctl calls
// SetLogLevel(slog.LevelWarn) as the first statement of main(); the daemons
// do not call it.
func SetLogLevel(l slog.Level) {
	logLevel.Set(l)
}

func init() {
	baseHandler := slog.NewJSONHandler(
		os.Stdout,
		&slog.HandlerOptions{Level: logLevel},
	)
	slog.SetDefault(slog.New(&TraceIdHandler{Handler: baseHandler}))
}

// ---------------------------------------------------------------------------
// Shared logging helpers (log.md R10, R11)
// ---------------------------------------------------------------------------

// TruncForLog prepares string file data for logging: valid UTF-8 is truncated
// to the first LogStrDataLimit characters; anything else is reduced to its
// size (log.md R11).
func TruncForLog(s string) string {
	if !utf8.ValidString(s) {
		return fmt.Sprintf("<binary %d bytes>", len(s))
	}
	runes := []rune(s)
	if len(runes) <= LogStrDataLimit {
		return s
	}
	return fmt.Sprintf(
		"%s...(%d chars total)",
		string(runes[:LogStrDataLimit]),
		len(runes),
	)
}

// PbToLogValue renders a protobuf message as a JSON-marshalable value for
// logging (use as slog.Any("data", PbToLogValue(msg))). Every bytes field, at
// any nesting depth, is replaced by the string "<N bytes>" so raw payloads
// never reach the logs (log.md R10). Enum values are rendered by name. Only
// populated fields appear.
func PbToLogValue(msg proto.Message) any {
	if msg == nil {
		return nil
	}
	refMsg := msg.ProtoReflect()
	if !refMsg.IsValid() {
		return nil
	}
	return pbMsgToAny(refMsg)
}

func pbMsgToAny(m protoreflect.Message) map[string]any {
	out := make(map[string]any)
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		out[string(fd.Name())] = pbFieldToAny(fd, v)
		return true
	})
	return out
}

func pbFieldToAny(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	switch {
	case fd.IsMap():
		mp := make(map[string]any)
		valFd := fd.MapValue()
		v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
			// MapKey.String() renders non-string keys too.
			mp[k.String()] = pbScalarToAny(valFd, mv)
			return true
		})
		return mp
	case fd.IsList():
		lst := v.List()
		arr := make([]any, 0, lst.Len())
		for i := 0; i < lst.Len(); i++ {
			arr = append(arr, pbScalarToAny(fd, lst.Get(i)))
		}
		return arr
	default:
		return pbScalarToAny(fd, v)
	}
}

func pbScalarToAny(fd protoreflect.FieldDescriptor, v protoreflect.Value) any {
	switch fd.Kind() {
	case protoreflect.BytesKind:
		return fmt.Sprintf("<%d bytes>", len(v.Bytes()))
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return pbMsgToAny(v.Message())
	case protoreflect.EnumKind:
		if ev := fd.Enum().Values().ByNumber(v.Enum()); ev != nil {
			return string(ev.Name())
		}
		return int32(v.Enum())
	default:
		// Bool, ints, uints, floats, string: native Go value; the JSON
		// handler marshals uint64 exactly (no precision loss).
		return v.Interface()
	}
}
```

## 5. Logging obligations by subsystem

Attribute names and `msg` strings below are normative (grep-ability). `error?`
means the attribute is present only when the operation returned a non-nil
error.

### 5.1 OS commands and file I/O — implemented in `LimitedOsClient`

See `osclient.md` §5 for the code. Summary of the records it must emit:

| event | msg | required attrs |
|---|---|---|
| command run | `os command` | `cmd` (string), `args` (`slog.Any`, `[]string`), `stdin` (string), `stdout` (string), `stderr` (string), `exit_code` (int), `error?` |
| file read | `os read file` | `path`, `size` (bytes read), `data` (`TruncForLog`), `error?` |
| file write | `os write file` | `path`, `size` (bytes written), `data` (`TruncForLog`), `error?` |
| direct file write | `os write file direct` | `path`, `size` (bytes written), `data` (`TruncForLog`), `error?` |
| raw block read | `os read block` | `path`, `offset`, `length`, `error?` — **never** `data` |
| raw block write | `os write block` | `path`, `offset`, `length` (bytes written), `error?` — **never** `data` |
| proto read | `os read proto` | `path`, `size` (serialized bytes), `data` (`slog.Any` of `PbToLogValue(target)`), `error?` |
| proto write | `os write proto` | `path`, `size` (serialized bytes), `data` (`slog.Any` of `PbToLogValue(msg)`), `error?` |

Command stdin/stdout/stderr are logged in full (the 128-character truncation
applies to file data only, per the requirement). The file `data` rule follows
R11. The two raw-block records — the DN's on-disk metadata path ([D13]) —
deliberately carry no `data` attribute at all: the blocks are large, opaque,
and may hold arbitrary tenant bytes.

Two block-IO records are emitted **outside** `LimitedOsClient`, by the cn
agent's `directLegProbeIO` (`agent/cnagent`), because the CN11 leg health
probers deliberately bypass the OsClient semaphore (`osclient.md` §4.5.1,
`update_01.md` U2). Their `msg` strings are normative here like every other:

| event | msg | required attrs |
|---|---|---|
| probe block write | `probe write block` | `path`, `offset`, `length` (bytes written), `error?` — **never** `data` |
| probe block direct read | `probe read block direct` | `path`, `offset`, `length`, `error?` — **never** `data` |

They are `probe …` and not `os …` on purpose: `cnagent_integtest.md` §9's
mutation grep names `os write block` explicitly, and the continuous health
probes must fall out of that list by construction rather than by a path-based
exemption. Everything else about them — one Info record per operation, emitted
on completion, under the caller's trace id, with `error?` only on failure —
follows the same rules as the table above.

### 5.2 gRPC — implemented in the interceptors

See `grpc.md` §2 and §3. Every request and reply message of the dnv-internal
services (`Gateway`, `DiskNodeAgent`, `ControllerNodeAgent`) is logged with
attrs `method` (full method string) and `data` (`PbToLogValue`), so e.g. a
`PushMigrBitmapRequest` logs its `bitmap` as `"<131072 bytes>"` instead of the
payload.

### 5.3 etcd — implemented in central helpers

etcd keys in dnv are already human-readable space-joined strings
(`architecture.md` §5.1); values are binary-serialized protobuf messages
(§5.3). Requirements:

* All etcd access (plain client and `clientv3/concurrency` STM alike) goes
  through a small set of central helpers in one file (suggested
  `common/etcdutil.go` or the gateway's kv layer), which do the proto
  (un)marshal **and** the logging. Direct `clientv3`/STM `Get`/`Put` calls with
  ad-hoc marshaling elsewhere are forbidden — otherwise the rule cannot be
  enforced.
* Records (all Info, via `slog.InfoContext(ctx, ...)`):

| event | msg | required attrs |
|---|---|---|
| point read | `etcd get` | `key` (string), `found` (bool), `value` (`PbToLogValue` of the decoded message; omit when not found), `error?` |
| write | `etcd put` | `key`, `value` (`PbToLogValue` of the message being stored), `error?` |
| delete | `etcd delete` | `key`, `error?` |
| range scan | `etcd range` | `prefix` (string), `count` (int, keys returned), `error?` — do not dump every value of a range; individual keys of interest are then read/logged via `etcd get` semantics |
| watch event | `etcd watch event` | `key`, `type` (`put`/`delete`), `value` (`PbToLogValue`, puts only), `error?` |

* Values are always logged decoded (`PbToLogValue`), never as raw/base64
  protobuf bytes.
* A put whose value fails to unmarshal still emits its `etcd watch event`
  record, carrying `error` and **no** `value` — there is no decoded message
  to render. The unmarshal error then ends that watch *generation*, not the
  watch: the error names the key itself, the consumer logs it on the way out
  — each of the four `WatchTyped` consumers with its own record, so grep for
  all of `cluster conf watch restarting`, `cdc watch restarting`,
  `rev watch restarting` and `worker reg watch restarting` — and its outer
  loop rescans and re-opens the watch, so this record is not the only trace
  of which key broke it.
* Inside an STM, reads/writes may be re-executed on transaction retry; each
  attempt logs, so duplicate records for retried transactions are expected and
  acceptable.

## 6. Example output

```json
{"time":"2026-08-28T10:00:00.000Z","level":"INFO","msg":"os command","cmd":"dmsetup","args":["create","dnv-...-5-...","--table","0 2097152 error"],"stdin":"","stdout":"","stderr":"","exit_code":0,"trace_id":"a1b2c3d4e5f60718"}
{"time":"2026-08-28T10:00:00.010Z","level":"INFO","msg":"etcd put","key":"dnv sp_rev 04 ebada5168620c5fe 0000000000000011","value":{"sp_name":"pool1","revision":7},"trace_id":"a1b2c3d4e5f60718"}
{"time":"2026-08-28T10:00:00.020Z","level":"INFO","msg":"grpc client request","method":"/DiskNodeAgent/PushMigrBitmap","data":{"cluster_id":16981786240730056190,"dn_id":3,"side_pointer":{"sp_id":17,"leg_id":21,"side_id":22},"revision":9,"migr_id":30,"bm_idx":1,"bitmap":"<131072 bytes>"},"trace_id":"a1b2c3d4e5f60718"}
```

## 7. Tests and acceptance checklist

Unit tests (`common/log_test.go`):

1. `TraceIdHandler` injects `trace_id` when the ctx carries one, and not
   otherwise (capture output with a `JSONHandler` over a `bytes.Buffer`).
2. `logger.With(...)` / `logger.WithGroup(...)` derived loggers still inject
   `trace_id` (verifies R12).
3. `TruncForLog`: short string passes through; long string truncated at 128
   characters with the suffix; invalid UTF-8 → `<binary N bytes>`.
4. `PbToLogValue` on a `PushMigrBitmapRequest` with a 4-byte bitmap: the
   rendered map has `"bitmap": "<4 bytes>"`, nested `side_pointer` rendered as
   a map, and the whole value marshals with `encoding/json` without error.
5. `PbToLogValue` renders enum fields by name (e.g. `SpLevel` →
   `"SP_LEVEL_READONLY"`; a zero-valued enum such as `SP_LEVEL_READWRITE` is
   omitted like any proto3 zero), repeated fields as arrays, and map fields
   with stringified keys.
6. `SetLogLevel(slog.LevelWarn)` suppresses Info records.

Acceptance: `go build ./...` and `go test ./common/...` pass; every binary's
`main` package imports `common` (directly or transitively); `dnvctl`'s `main()`
starts with `common.SetLogLevel(slog.LevelWarn)`; a grep for `log.Print`,
`logrus`, `zap`, `zerolog` over the Go sources alone (`--include=*.go`) hits
exactly three lines, all in `etcdutil/etcdutil.go` — the `go.uber.org/zap`
import, the comment explaining it, and the `zap.NewNop()` it passes as the
etcd client's `Logger` — and nothing anywhere else. That logger is being
*silenced*, not used, precisely so stdout carries one JSON stream and it is
dnv's, which is R2; `TestStdoutStaysOneJsonRecordPerLine`
(`etcdutil/etcdutil_test.go`) pins it by re-running the test binary as a child
that drives the etcd client and asserting every line the child writes to
stdout parses as one JSON record, dnv's own `etcd get` among them. No dnv
record comes from anything but `log/slog`.

## 8. Amendments applied to this document

Recorded for traceability; the edits are already applied. Appended rather than
inserted because §2, §3, §5.1 and §5.3 are cited by number from `osclient.md`,
`grpc.md` and `layout.md`.

* `update_01.md` U2 — §5.1 gained the two prober-emitted records
  `probe write block` and `probe read block direct`, in their own table. They
  are emitted by `agent/cnagent`'s `directLegProbeIO`, **not** by an `OsClient`
  method: the CN11 leg health probers call `common.WriteBlockAt` /
  `common.ReadBlockDirectAt` directly, outside the semaphore
  (`osclient.md` §4.5.1). `ReadBlockDirect` left the `OsClient` interface in
  the same change; §5.1 never had a row for it, so nothing was removed, and the
  `os read block` / `os write block` rows are unchanged.
