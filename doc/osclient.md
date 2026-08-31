# osclient.md — OsClient Specification (dnv)

Status: **normative**. Read `log.md` first — this document reuses its helpers
(`PbToLogValue`, `TruncForLog`, `LogStrDataLimit`) and follows its rules
(one Info record per operation, typed attributes, ctx propagation).

Required background: `architecture.md` §7 (command timeouts), §9.1 (agent local
store, temp+fsync+rename), §9.4 and Appendix A (the commands agents run),
`constants.go`, `name_fmt.go` (the `Local*Path` files that go through
`ReadProto`/`WriteProto`).

---

## 1. Scope and placement

* Files: `common/osclient.go` (interface + `LimitedOsClient`),
  `common/osclient_fake.go` (test double), `common/osclient_test.go`.
* Package: `common`, alongside `constants.go`/`name_fmt.go`.
* Consumers: primarily `dnv-agent` (dn/cn) for `dmsetup`/LVM/`mdadm`/`nvme`/
  nvmet-configfs work and for persisting the `Local*Path` protobuf state files;
  `dnvctl`'s copier may use it too. **All** OS command execution and disk file
  I/O in the codebase goes through an `OsClient` — never call `os/exec` or
  `os.ReadFile`/`os.WriteFile` directly outside this file. This is what makes
  the logging rules of `log.md` R8.1/R8.2 enforceable and makes every consumer
  unit-testable via `FakeOsClient`.
* Dependencies: `golang.org/x/sync/semaphore`, `google.golang.org/protobuf`.
  Go ≥ 1.20 for `exec.Cmd.Cancel`/`WaitDelay`; the module itself pins
  `go 1.26.5` in `go.mod`.

## 2. Interface (normative, verbatim)

```go
package common

import (
	"context"

	"google.golang.org/protobuf/proto"
)

// OsClient defines high-level primitives for executing OS commands,
// performing disk file I/O, and handling Protobuf binary serialization with
// context support.
type OsClient interface {

	// RunCommand executes an operating system binary with the given
	// command-line arguments and optional stdin data.
	//
	// Inputs:
	//   - ctx: Controls command cancellation or execution timeout
	//     (e.g., context.WithTimeout).
	//   - name: The executable binary name or full path
	//     (e.g., "git", "grep", "/bin/sh").
	//   - args: Command-line parameters passed to the executable.
	//   - stdinInput: String payload passed via standard input stream
	//     (pass empty string "" if none).
	//
	// Outputs:
	//   - stdout: Captured standard output stream as a string.
	//   - stderr: Captured standard error stream as a string.
	//   - exitCode: Process exit status code (0 for success, non-zero for
	//     error, -1 if execution failed to start).
	//   - err: Go error interface indicating non-zero exit code or execution
	//     failure.
	RunCommand(ctx context.Context, name string, args []string, stdinInput string) (stdout string, stderr string, exitCode int, err error)

	// ReadFile loads the entire contents of a file specified by path into
	// memory.
	//
	// Outputs:
	//   - data: String content of the file.
	//   - err: nil on success, non-nil error if context is canceled, file is
	//     missing, or I/O fails.
	ReadFile(ctx context.Context, path string) (data string, err error)

	// WriteFile creates or overwrites a file at the specified path with
	// string data using default file permissions.
	WriteFile(ctx context.Context, path string, data string) (err error)

	// ReadProto loads a raw Protobuf binary file from disk and deserializes
	// it into a target message struct (target must be a non-nil pointer,
	// e.g. &pb.SyncupDnRequest{}).
	ReadProto(ctx context.Context, path string, target proto.Message) (err error)

	// WriteProto serializes a Protobuf message into wire binary format and
	// writes it directly to disk.
	WriteProto(ctx context.Context, path string, msg proto.Message) (err error)
}
```

(The full doc comments from the requirement are kept in the source; they are
abbreviated above for readability. Signatures are exact and MUST NOT change.)

## 3. Constant to add to `constants.go`

`DefaultOsClientLimit` does **not** exist yet in `constants.go`; add it to the
existing `const` block:

```go
	// Default cap on the number of in-flight OsClient operations
	// (commands + file I/O + proto I/O combined). See osclient.md.
	DefaultOsClientLimit = 32
```

Rationale: during convergence an agent fans out many `dmsetup`/`lvm`/`mdadm`/
`nvme` invocations plus state-file writes; 32 bounds fork and disk pressure
while leaving ample parallelism.

## 4. `LimitedOsClient` — normative behavior

### 4.1 Construction and concurrency limit

* `func NewLimitedOsClient(limit int64) *LimitedOsClient` — `limit <= 0` means
  "use `DefaultOsClientLimit`".
* The limit caps the **sum of in-flight calls across all five methods**. Use a
  single `semaphore.Weighted(limit)`. Every public method first does
  `sem.Acquire(ctx, 1)` (blocking, ctx-aware: a canceled/expired ctx returns
  `ctx.Err()` without performing the operation and without logging an
  operation record), and `defer sem.Release(1)`.
* `LimitedOsClient` is safe for concurrent use and holds no other mutable
  state. One instance per process is the expected usage.
* `var _ OsClient = (*LimitedOsClient)(nil)` compile-time assertion.

### 4.2 RunCommand

* Use `exec.CommandContext(ctx, name, args...)`. Set `cmd.Stdin =
  strings.NewReader(stdinInput)` only when `stdinInput != ""`. Capture stdout
  and stderr into separate `bytes.Buffer`s.
* Timeout / kill semantics (implements `architecture.md` §7): the **caller**
  owns the deadline — agents wrap the ctx with
  `context.WithTimeout(ctx, CmdSoftTimeout*time.Second)` before calling.
  `LimitedOsClient` adds no deadline of its own, but configures:
  * `cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }`
    — SIGTERM when the ctx fires (the soft timeout);
  * `cmd.WaitDelay = (CmdHardTimeout - CmdSoftTimeout) * time.Second` — if the
    process ignores SIGTERM, it is SIGKILLed after this grace, i.e. at the
    hard timeout relative to the soft one. `WaitDelay` also prevents `Wait`
    from hanging on inherited pipes.
* Exit-code mapping: `err == nil` → 0; `*exec.ExitError` →
  `ExitError.ExitCode()` (note: a signal-killed process reports `-1` here,
  with a non-nil error — acceptable); any other error (start failure, ctx
  canceled before start) → `-1`.
* Return `err` exactly as produced (non-nil for non-zero exit, signal death,
  start failure, or ctx cancellation). Do not wrap stderr into the error; the
  caller already receives stderr separately.

### 4.3 ReadFile / WriteFile

* Check `ctx.Err()` after acquiring the semaphore and return it if non-nil
  (plain file I/O on local disks is not further cancelable; this is
  best-effort cancellation and is sufficient).
* `ReadFile`: `os.ReadFile(path)`, return `string(data)`.
* `WriteFile`: **atomic replace** — write to a temp file in the same
  directory, `Sync`, `Close`, `Chmod(0o644)` ("default file permissions"),
  then `os.Rename` onto `path`. This makes the agents' §9.1 requirement
  ("temp file in the same dir, fsync, rename") automatic for every state
  file, and is harmless for all other writes.

### 4.4 ReadProto / WriteProto

* `ReadProto`: read the file bytes (`os.ReadFile`), then
  `proto.Unmarshal(data, target)`.
* `WriteProto`: `proto.Marshal(msg)`, then the same atomic replace as
  `WriteFile`.
* These are the methods the agents use for the `Local*Path` files of
  `architecture.md` §4.6/§9.1 (last applied `Syncup*Request`s and received
  `Push*BitmapRequest` chunks).

### 4.5 Logging (implements `log.md` §5.1)

One `slog.InfoContext(ctx, ...)` record per call, emitted on completion, with
the exact `msg` strings and attributes below. Append
`slog.String("error", err.Error())` only when `err != nil`.

| method | msg | attrs |
|---|---|---|
| RunCommand | `os command` | `cmd`, `args` (`slog.Any`), `stdin`, `stdout`, `stderr`, `exit_code`, `error?` |
| ReadFile | `os read file` | `path`, `size` (= `len(data)`), `data` (= `TruncForLog(data)`), `error?` |
| WriteFile | `os write file` | `path`, `size`, `data` (= `TruncForLog(data)`), `error?` |
| ReadProto | `os read proto` | `path`, `size` (= serialized length read), `data` (`slog.Any(PbToLogValue(target))`), `error?` |
| WriteProto | `os write proto` | `path`, `size` (= serialized length), `data` (`slog.Any(PbToLogValue(msg))`), `error?` |

Notes:

* Command stdin/stdout/stderr are logged in full (no truncation) — the
  128-character `TruncForLog` rule applies to file string data only.
* `PbToLogValue` already reduces embedded `bytes` fields (e.g. the `bitmap` of
  a persisted `PushMigrBitmapRequest`) to `"<N bytes>"`, so large payloads
  never reach the log.
* A semaphore-acquire failure (ctx canceled while waiting) logs nothing — the
  operation never happened.

## 5. Reference implementation — `common/osclient.go` (core, complete)

```go
package common

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/semaphore"
	"google.golang.org/protobuf/proto"
)

// LimitedOsClient is the concrete OsClient. It caps the total number of
// in-flight operations (all methods combined) at the limit given to
// NewLimitedOsClient, and logs every operation per log.md / osclient.md.
type LimitedOsClient struct {
	sem *semaphore.Weighted
}

var _ OsClient = (*LimitedOsClient)(nil)

// NewLimitedOsClient creates a LimitedOsClient. limit <= 0 selects
// DefaultOsClientLimit.
func NewLimitedOsClient(limit int64) *LimitedOsClient {
	if limit <= 0 {
		limit = DefaultOsClientLimit
	}
	return &LimitedOsClient{sem: semaphore.NewWeighted(limit)}
}

func appendErr(attrs []any, err error) []any {
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	return attrs
}

func (c *LimitedOsClient) RunCommand(
	ctx context.Context,
	name string,
	args []string,
	stdinInput string,
) (string, string, int, error) {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return "", "", -1, err
	}
	defer c.sem.Release(1)

	cmd := exec.CommandContext(ctx, name, args...)
	if stdinInput != "" {
		cmd.Stdin = strings.NewReader(stdinInput)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// architecture.md §7: SIGTERM at the (caller-set) soft timeout,
	// SIGKILL at the hard timeout.
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = time.Duration(CmdHardTimeout-CmdSoftTimeout) * time.Second

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	attrs := []any{
		slog.String("cmd", name),
		slog.Any("args", args),
		slog.String("stdin", stdinInput),
		slog.String("stdout", stdout.String()),
		slog.String("stderr", stderr.String()),
		slog.Int("exit_code", exitCode),
	}
	slog.InfoContext(ctx, "os command", appendErr(attrs, err)...)

	return stdout.String(), stderr.String(), exitCode, err
}

func (c *LimitedOsClient) ReadFile(
	ctx context.Context,
	path string,
) (string, error) {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return "", err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return "", err
	}

	rawData, err := os.ReadFile(path)
	data := string(rawData)

	attrs := []any{
		slog.String("path", path),
		slog.Int("size", len(rawData)),
		slog.String("data", TruncForLog(data)),
	}
	slog.InfoContext(ctx, "os read file", appendErr(attrs, err)...)

	return data, err
}

func (c *LimitedOsClient) WriteFile(
	ctx context.Context,
	path string,
	data string,
) error {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return err
	}

	err := atomicWrite(path, []byte(data))

	attrs := []any{
		slog.String("path", path),
		slog.Int("size", len(data)),
		slog.String("data", TruncForLog(data)),
	}
	slog.InfoContext(ctx, "os write file", appendErr(attrs, err)...)

	return err
}

func (c *LimitedOsClient) ReadProto(
	ctx context.Context,
	path string,
	target proto.Message,
) error {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return err
	}

	rawData, err := os.ReadFile(path)
	if err == nil {
		err = proto.Unmarshal(rawData, target)
	}

	attrs := []any{
		slog.String("path", path),
		slog.Int("size", len(rawData)),
		slog.Any("data", PbToLogValue(target)),
	}
	slog.InfoContext(ctx, "os read proto", appendErr(attrs, err)...)

	return err
}

func (c *LimitedOsClient) WriteProto(
	ctx context.Context,
	path string,
	msg proto.Message,
) error {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return err
	}

	rawData, err := proto.Marshal(msg)
	if err == nil {
		err = atomicWrite(path, rawData)
	}

	attrs := []any{
		slog.String("path", path),
		slog.Int("size", len(rawData)),
		slog.Any("data", PbToLogValue(msg)),
	}
	slog.InfoContext(ctx, "os write proto", appendErr(attrs, err)...)

	return err
}

// atomicWrite implements the temp-file + fsync + rename protocol of
// architecture.md §9.1: readers never observe a partial file.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// No-op after a successful rename (ENOENT is ignored); cleans up on
	// every failure path.
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
```

## 6. Test double — `common/osclient_fake.go` (complete)

Exported (not `_test.go`) so agent/worker/gateway tests in other packages can
reuse it. Unset function fields default to success.

```go
package common

import (
	"context"

	"google.golang.org/protobuf/proto"
)

// FakeOsClient is a configurable OsClient test double: set only the function
// fields your test needs; unset fields succeed with zero values.
type FakeOsClient struct {
	RunCommandFn func(ctx context.Context, name string, args []string, stdinInput string) (string, string, int, error)
	ReadFileFn   func(ctx context.Context, path string) (string, error)
	WriteFileFn  func(ctx context.Context, path string, data string) error
	ReadProtoFn  func(ctx context.Context, path string, target proto.Message) error
	WriteProtoFn func(ctx context.Context, path string, msg proto.Message) error
}

var _ OsClient = (*FakeOsClient)(nil)

func (f *FakeOsClient) RunCommand(ctx context.Context, name string, args []string, stdinInput string) (string, string, int, error) {
	if f.RunCommandFn != nil {
		return f.RunCommandFn(ctx, name, args, stdinInput)
	}
	return "", "", 0, nil
}

func (f *FakeOsClient) ReadFile(ctx context.Context, path string) (string, error) {
	if f.ReadFileFn != nil {
		return f.ReadFileFn(ctx, path)
	}
	return "", nil
}

func (f *FakeOsClient) WriteFile(ctx context.Context, path string, data string) error {
	if f.WriteFileFn != nil {
		return f.WriteFileFn(ctx, path, data)
	}
	return nil
}

func (f *FakeOsClient) ReadProto(ctx context.Context, path string, target proto.Message) error {
	if f.ReadProtoFn != nil {
		return f.ReadProtoFn(ctx, path, target)
	}
	return nil
}

func (f *FakeOsClient) WriteProto(ctx context.Context, path string, msg proto.Message) error {
	if f.WriteProtoFn != nil {
		return f.WriteProtoFn(ctx, path, msg)
	}
	return nil
}
```

## 7. Example log output

```json
{"time":"2026-08-28T10:00:01.000Z","level":"INFO","msg":"os command","cmd":"lvcreate","args":["--addtag","not_trimmed","--name","0000000000000011-0000000000000016","--extents","10","dnv-dn-ebada5168620c5fe-0000000000000003"],"stdin":"","stdout":"  Logical volume \"...\" created.\n","stderr":"","exit_code":0,"trace_id":"a1b2c3d4e5f60718"}
{"time":"2026-08-28T10:00:01.050Z","level":"INFO","msg":"os write proto","path":"/var/tmp/side-ebada5168620c5fe-0000000000000003-0000000000000011-0000000000000016","size":34,"data":{"cluster_id":16981786240730056190,"dn_id":3,"side_pointer":{"sp_id":17,"leg_id":21,"side_id":22},"revision":9,"side_conf":{"ext_cnt":10,"cntlid_slot":1,"primary_cn_id":5,"standby_id_list":[6]}},"trace_id":"a1b2c3d4e5f60718"}
```

## 8. Tests and acceptance checklist

Unit tests (`common/osclient_test.go`; Linux assumed — `sh`, `sleep`, `cat`
available):

1. **Exit codes**: `RunCommand(ctx, "sh", []string{"-c", "exit 0"}, "")` →
   `(…, 0, nil)`; `"exit 3"` → exit code 3 and non-nil error; a nonexistent
   binary → exit code -1 and non-nil error.
2. **Stdin/stdout/stderr**: `RunCommand(ctx, "cat", nil, "hello")` → stdout
   `"hello"`; `sh -c "echo out; echo err 1>&2"` captures both streams
   separately.
3. **Soft/hard timeout**: with `ctx = context.WithTimeout(…,
   CmdSoftTimeout*time.Second)` shortened for test speed (e.g. 100 ms), a
   `sleep 10` returns promptly after cancellation with a non-nil error
   (SIGTERM path); a `sh -c 'trap "" TERM; sleep 10'` returns within
   `WaitDelay` (SIGKILL path — may be shortened by making the delay small in
   a test-local client if needed, otherwise mark as a slow test).
4. **In-flight limit**: `NewLimitedOsClient(2)`; start three concurrent
   `sleep 0.2` commands; total wall time ≥ 0.4 s (the third waited for a
   slot). Also: with the semaphore fully held, a call with an
   already-canceled ctx returns `context.Canceled` immediately.
5. **File round-trip**: `WriteFile` then `ReadFile` returns identical data;
   overwriting an existing file replaces it; the directory contains no
   leftover `*.tmp-*` files afterwards.
6. **Proto round-trip**: `WriteProto` a populated `SyncupSideRequest` (or any
   generated message), `ReadProto` into a fresh instance,
   `proto.Equal` holds.
7. **Log records**: swap in a captured handler (as in `log.md` §7), run one
   call of each method, assert the `msg` strings and required attrs of §4.5,
   assert file `data` is truncated at `LogStrDataLimit` characters for a long
   string, and assert a proto containing a `bytes` field logs `"<N bytes>"`.

Acceptance: `go vet ./common/...` and `go test ./common/...` pass;
`DefaultOsClientLimit` exists in `constants.go`; repo-wide grep shows no
`os/exec` usage outside `common/osclient.go`.
