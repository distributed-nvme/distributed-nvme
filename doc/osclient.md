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

* Files: `common/osclient.go` (interface + `LimitedOsClient` + the exported raw
  block helpers `WriteBlockAt`/`ReadBlockDirectAt` of §4.5.1),
  `common/osclient_fake.go` (test double), `common/osclient_test.go`.
* Package: `common`, alongside `constants.go`/`name_fmt.go`.
* Consumers: primarily `dnv-agent` (dn/cn) for `dmsetup`/`mdadm`/`nvme`/
  nvmet-configfs work and for persisting the `Local*Path` protobuf state files;
  `dnvctl`'s copier may use it too. **All** OS command execution and disk file
  I/O in the codebase goes through an `OsClient` — never call `os/exec` or
  `os.ReadFile`/`os.WriteFile` directly outside this file. This is what makes
  the logging rules of `log.md` R8.1/R8.2 enforceable and makes every consumer
  unit-testable via `FakeOsClient`. There is exactly **one** sanctioned
  exception, recorded in §4.5.1: the CN11 leg-health probers call the
  package-level helpers `common.WriteBlockAt` / `common.ReadBlockDirectAt`
  directly — outside the semaphore, never under a lock — and log their own
  records (`update_01.md` U2).
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

	// WriteFileDirect writes data straight into the file at path with a
	// plain open/truncate/write/close — no temp file, no rename. It exists
	// for kernel virtual filesystems (nvmet configfs, sysfs), where the
	// atomic-replace protocol of WriteFile is impossible: configfs forbids
	// creating arbitrary files, so a temp file + rename can never succeed
	// there. Use WriteFile for every regular-file write; use
	// WriteFileDirect only for virtual-filesystem attribute writes
	// (dnagent.md SH18).
	WriteFileDirect(ctx context.Context, path string, data string) (err error)

	// ReadBlock reads exactly length bytes at byte offset from a block
	// device (or regular file). A short read is an error. Buffered IO —
	// callers use it only for regions no dm table references.
	ReadBlock(ctx context.Context, path string, offset uint64, length uint64) (data []byte, err error)

	// WriteBlock writes data at byte offset and fdatasyncs the file
	// descriptor before returning.
	WriteBlock(ctx context.Context, path string, offset uint64, data []byte) (err error)

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
abbreviated above for readability. Signatures are exact and MUST NOT change.
The *method set* has changed twice by recorded amendment: `WriteFileDirect` was
added by `dnagent.md` §5, and `ReadBlockDirect` was **removed** by
`update_01.md` U2 — its only caller, the CN11 leg prober, now calls the
package-level helper of §4.5.1 outside the semaphore. §9 records both.)

## 3. Constant to add to `constants.go`

`DefaultOsClientLimit` does **not** exist yet in `constants.go`; add it to the
existing `const` block:

```go
	// Default cap on the number of in-flight OsClient operations
	// (commands + file I/O + proto I/O combined). See osclient.md.
	DefaultOsClientLimit = 32
```

Rationale: during convergence an agent fans out many `dmsetup`/`mdadm`/`nvme`/
`blkdiscard` invocations plus state-file writes; 32 bounds fork and disk
pressure while leaving ample parallelism.

## 4. `LimitedOsClient` — normative behavior

### 4.1 Construction and concurrency limit

* `func NewLimitedOsClient(limit int64) *LimitedOsClient` — `limit <= 0` means
  "use `DefaultOsClientLimit`".
* The limit caps the **sum of in-flight calls across all eight interface
  methods**. It deliberately does **not** cover the §4.5.1 package-level
  helpers: probe IO must never consume a slot. Use a single
  `semaphore.Weighted(limit)`. Every public method first does
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

### 4.3 ReadFile / WriteFile / WriteFileDirect

* Check `ctx.Err()` after acquiring the semaphore and return it if non-nil
  (plain file I/O on local disks is not further cancelable; this is
  best-effort cancellation and is sufficient).
* `ReadFile`: `os.ReadFile(path)`, return `string(data)`.
* `WriteFile`: **atomic replace** — write to a temp file in the same
  directory, `Sync`, `Close`, `Chmod(0o644)` ("default file permissions"),
  then `os.Rename` onto `path`. This makes the agents' §9.1 requirement
  ("temp file in the same dir, fsync, rename") automatic for every state
  file, and is the right default for every regular-file write.
* `WriteFileDirect`: plain in-place write — `os.WriteFile(path,
  []byte(data), 0o644)`, no temp file, no fsync, no rename. It exists for
  kernel virtual filesystems (nvmet configfs, sysfs), where the atomic
  replace is impossible: configfs forbids creating arbitrary files, so
  `WriteFile` can never succeed there. Use it **only** for such attribute
  writes; the agents' §9.1 state files MUST keep using
  `WriteFile`/`WriteProto` (`dnagent.md` SH18).

### 4.4 ReadProto / WriteProto

* `ReadProto`: read the file bytes (`os.ReadFile`), then
  `proto.Unmarshal(data, target)`.
* `WriteProto`: `proto.Marshal(msg)`, then the same atomic replace as
  `WriteFile`.
* These are the methods the agents use for the `Local*Path` files of
  `architecture.md` §4.6/§9.1 (last applied `Syncup*Request`s and received
  `Push*BitmapRequest` chunks).

### 4.5 ReadBlock / WriteBlock

The raw-device metadata path of `architecture.md` [D13]: the dn agent reads
and writes its own on-disk format (header block, A/B volume-table slots,
dm-clone metadata slots) directly on the `--disk` device through these two
methods.

* Check `ctx.Err()` after acquiring the semaphore, exactly like §4.3.
* `ReadBlock`: `os.OpenFile(path, os.O_RDONLY, 0)`, read exactly `length`
  bytes at `offset` (`io.ReadFull` over an `io.SectionReader`), close.
  **A short read is an error** — the method never returns a partially filled
  buffer, so a caller can trust `len(data) == length` whenever `err == nil`.
  A region inside the device that was never written reads as zeros; that is
  not an error.
* `WriteBlock`: `os.OpenFile(path, os.O_WRONLY, 0)`, `WriteAt(data, offset)`,
  `Sync()` (the fdatasync the caller's crash protocol depends on), close. It
  never *creates* a file (no `O_CREATE`); the intended target is a block
  device, which always exists at full size. Against a regular file — tests —
  `WriteAt` past the end extends it, as `pwrite` does. The body is the
  package-level helper `WriteBlockAt` (§4.5.1); `WriteBlock` adds only the
  semaphore slot and the `os write block` record.
* **Buffered IO, never `O_DIRECT`.** Durability comes from the `Sync()`. The
  regions the DN agent reads back this way — the header block and the two
  volume-table slots — are never part of any dm table, so no dm path can
  write bytes underneath a cached read. The clone-metadata area *is* mapped,
  by each migration's wrapper dm-linear, but the agent only ever **writes**
  there (the 8 KiB zeroing of a freshly allocated slot), only before that
  slot's wrapper exists, and always followed by the `Sync()` — so the bytes
  reach the device before anything can map them, and no read of that area is
  ever served from the page cache.
* **Never shell out to `dd` for this.** The lab VMs ship uutils dd 0.8.0,
  whose `iflag=`/`oflag=direct` silently misbehave — false failures and
  dropped writes (`dnagent_integtest.md` §4).
* The payload is **never** logged: the records carry `path`, `offset` and
  `length` only (§4.6). Metadata blocks are large and uninteresting in a log,
  and a device region may hold arbitrary tenant bytes.

### 4.5.1 Exported raw helpers and the probe-IO carve-out

`update_01.md` U2 removed `ReadBlockDirect` from the `OsClient` interface — the
CN11 leg-health prober was its only caller, and a prober must never hold a
semaphore slot (below). The two raw bodies are exported from
`common/osclient.go` as plain package-level functions instead. They take no
`ctx`, acquire **no** semaphore slot, and log **nothing**:

```go
// WriteBlockAt pwrites data at byte offset and fdatasyncs the file
// descriptor before returning. Buffered IO; a fresh fd per call, never a
// cached one; never O_CREATE (the target is a block device that already
// exists at full size).
func WriteBlockAt(path string, offset uint64, data []byte) error

// ReadBlockDirectAt preads exactly length bytes at byte offset with
// O_RDONLY|O_DIRECT into a 4096-aligned bounce buffer (allocate
// length+4096, slice to the first aligned boundary, copy out), then
// closes. offset and length MUST be multiples of 4096 and length != 0 —
// checked **before** the open, so a misaligned caller never touches the
// device. A short read is an error, exactly like ReadBlock. A fresh fd per
// call.
func ReadBlockDirectAt(path string, offset uint64, length uint64) ([]byte, error)
```

4096 is the alignment because O_DIRECT alignment is per-device, 4096 satisfies
every device dnv touches, and it is the probe's natural unit
(`LegHealthBlockSize`). `LimitedOsClient.WriteBlock` is a thin wrapper over
`WriteBlockAt` (semaphore + the `os write block` record of §4.6);
`ReadBlockDirectAt` has no `OsClient` wrapper at all. The buffered `readBlockAt`
behind `ReadBlock` stays **unexported** — nothing outside the `OsClient` may
bypass it for metadata IO ([D13]).

**The recorded carve-out.** Probe IO — and only probe IO — is the one
sanctioned direct-syscall path in dnv:

* *What it is.* The §3.6 / [D6] leg health probe (`cnagent.md` CN11, §2.2):
  write a block through the leg wrapper with `WriteBlockAt`, read it back with
  `ReadBlockDirectAt`. The read must bypass the page cache — a buffered read of
  a just-written block would be answered from cache and observe no device IO at
  all, making the read-back vacuous. The write half needs no direct twin: its
  `fdatasync` already forces the data to the device and surfaces the IO error.
* *It may block indefinitely, by design.* A leg with no serving path
  (`ctrl_loss_tmo = -1`) queues IO forever, so the calling goroutine sits in
  uninterruptible D state until that IO is errored — which happens only when the
  agent disconnects the path (`cnagent.md` CN21). This is the feature working: a
  hung probe is how a silently stalled target is detected. Two consequences the
  implementer must not design around: **cancelling the prober's context does not
  unblock a syscall already in flight** (the helpers take no ctx; the prober's
  ctx is checked once before the call and otherwise only carries the CN2
  per-attempt trace id into the records below), and **nothing ever waits for a
  prober to finish** — cancel and move on, never cancel-and-wait. (Contrast
  `update_01.md` U4's dn zeroing goroutines, which *are* waited for: their
  `blkdiscard` is a killable child process, not a blocked syscall.)
* *It must never run under a lock.* CN1 already keeps the probers out of the
  lock hierarchy; a wedged probe under the node or object lock would freeze
  every converge and Check round on the node.
* *It must never run through the semaphore.* `LimitedOsClient` is a
  `DefaultOsClientLimit` = 32 slot semaphore held across the blocking syscall.
  One dead or partitioned DN can back ≥ 32 legs on a CN (`MaxSideCntPerDn` =
  1024); 32 wedged probes then starve **every** OS operation on the node —
  including the `nvme disconnect` that is the only documented release mechanism
  for those very probes, and the mdadm/dm commands the §10.4 self-healing
  needs. Deadlock by construction; hence the helpers, not a second OsClient and
  not a probe budget (`update_01.md` §8 rejects both).
* *Who may call them.* Only the cn agent's lock-free prober goroutines
  (`cnagent.md` CN1/CN11), through the small fakeable probe-IO dependency U2
  defines alongside `healthcheck.go`. These two helpers are the only block-IO
  syscalls any package outside `common` may call; every other caller — the dn
  `diskmeta.go` header/volume-table path above all — keeps using the `OsClient`
  methods of §4.5.
* *Records.* Because the helpers log nothing, the prober emits its own two
  records, `probe write block` and `probe read block direct` (§4.6). They are
  deliberately **not** `os …` messages.

Everything §4.5 says about payload logging (`path`, `offset` and `length` only,
never `data` — a device region may hold arbitrary tenant bytes) and about never
shelling out to `dd` (uutils dd 0.8.0's broken `iflag=`/`oflag=direct`,
`dnagent_integtest.md` §4) applies to the helpers and to the prober's records
unchanged.

### 4.6 Logging (implements `log.md` §5.1)

One `slog.InfoContext(ctx, ...)` record per call, emitted on completion, with
the exact `msg` strings and attributes below. Append
`slog.String("error", err.Error())` only when `err != nil`.

| OsClient method | msg | attrs |
|---|---|---|
| RunCommand | `os command` | `cmd`, `args` (`slog.Any`), `stdin`, `stdout`, `stderr`, `exit_code`, `error?` |
| ReadFile | `os read file` | `path`, `size` (= `len(data)`), `data` (= `TruncForLog(data)`), `error?` |
| WriteFile | `os write file` | `path`, `size`, `data` (= `TruncForLog(data)`), `error?` |
| WriteFileDirect | `os write file direct` | `path`, `size`, `data` (= `TruncForLog(data)`), `error?` |
| ReadBlock | `os read block` | `path`, `offset`, `length`, `error?` — never `data` |
| WriteBlock | `os write block` | `path`, `offset`, `length` (= `len(data)`), `error?` — never `data` |
| ReadProto | `os read proto` | `path`, `size` (= serialized length read), `data` (`slog.Any(PbToLogValue(target))`), `error?` |
| WriteProto | `os write proto` | `path`, `size` (= serialized length), `data` (`slog.Any(PbToLogValue(msg))`), `error?` |

The CN11 leg probers do **not** call an `OsClient` (§4.5.1). They call the
package-level helpers and emit these two records themselves, one per probe
half, under the attempt's fresh trace id — same attributes, same "never `data`"
rule:

| emitter | msg | attrs |
|---|---|---|
| prober write half (`common.WriteBlockAt`) | `probe write block` | `path`, `offset`, `length` (= `len(data)`), `error?` — never `data` |
| prober read half (`common.ReadBlockDirectAt`) | `probe read block direct` | `path`, `offset`, `length`, `error?` — never `data` |

The `probe …` prefix is load-bearing: `cnagent_integtest.md` §9's `mutations()`
grepped for `os write block` **and** `os read block direct` records, and needed
a `-9-` path exemption to tolerate the continuous health probes. With these
messages the probe records fall out of that grep **by construction**, so the
exemption is deleted and the grep list keeps `os write block` alone — no agent
emits `os read block direct` any more (`update_01.md` U2-T5). `os write block`
survives unchanged as the
dn agent's `diskmeta.go` record, so a `probe …` record and an `os …` record can
never be confused for one another.

Notes:

* Command stdin/stdout/stderr are logged in full (no truncation) — the
  128-character `TruncForLog` rule applies to file string data only.
* `PbToLogValue` already reduces embedded `bytes` fields (e.g. the `bitmap` of
  a persisted `PushMigrBitmapRequest`) to `"<N bytes>"`, so large payloads
  never reach the log.
* A semaphore-acquire failure (ctx canceled while waiting) logs nothing — the
  operation never happened.
* The `probe …` records carry no semaphore semantics: a prober never waits for
  a slot. The same "never happened, never logged" rule still applies to it,
  through the one `ctx.Err()` check the prober makes *before* calling a helper
  — an attempt cancelled there emits no record at all.

## 5. Reference implementation — `common/osclient.go` (core, complete)

```go
package common

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

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

func (c *LimitedOsClient) WriteFileDirect(
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

	err := os.WriteFile(path, []byte(data), 0o644)

	attrs := []any{
		slog.String("path", path),
		slog.Int("size", len(data)),
		slog.String("data", TruncForLog(data)),
	}
	slog.InfoContext(ctx, "os write file direct", appendErr(attrs, err)...)

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

func (c *LimitedOsClient) ReadBlock(
	ctx context.Context,
	path string,
	offset uint64,
	length uint64,
) ([]byte, error) {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, err := readBlockAt(path, offset, length)

	attrs := []any{
		slog.String("path", path),
		slog.Uint64("offset", offset),
		slog.Uint64("length", length),
	}
	slog.InfoContext(ctx, "os read block", appendErr(attrs, err)...)

	return data, err
}

func (c *LimitedOsClient) WriteBlock(
	ctx context.Context,
	path string,
	offset uint64,
	data []byte,
) error {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return err
	}
	defer c.sem.Release(1)
	if err := ctx.Err(); err != nil {
		return err
	}

	err := WriteBlockAt(path, offset, data)

	attrs := []any{
		slog.String("path", path),
		slog.Uint64("offset", offset),
		slog.Int("length", len(data)),
	}
	slog.InfoContext(ctx, "os write block", appendErr(attrs, err)...)

	return err
}

// ReadBlockDirectAt is the §4.5.1 O_DIRECT read: pread into a 4096-aligned
// buffer so the read is served by the device, not the page cache (the leg
// health probe's read-back would otherwise observe nothing). It is NOT an
// OsClient method: it takes no semaphore slot and logs nothing, because it may
// block for as long as the device queues IO. Its only caller is the cn agent's
// lock-free prober, which logs `probe read block direct` itself.
func ReadBlockDirectAt(path string, offset uint64, length uint64) ([]byte, error) {
	const align = 4096
	if offset%align != 0 || length%align != 0 || length == 0 {
		return nil, fmt.Errorf(
			"read block direct: %s offset=%d length=%d not %d-aligned",
			path, offset, length, align)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer syscall.Close(fd)
	raw := make([]byte, length+align)
	shift := align - (uint64(uintptr(unsafe.Pointer(&raw[0]))) % align)
	if shift == align {
		shift = 0
	}
	buf := raw[shift : shift+length]
	n, err := syscall.Pread(fd, buf, int64(offset))
	if err != nil {
		return nil, &os.PathError{Op: "pread", Path: path, Err: err}
	}
	if uint64(n) != length {
		return nil, fmt.Errorf(
			"short read: %s offset=%d length=%d got=%d",
			path, offset, length, n)
	}
	data := make([]byte, length)
	copy(data, buf)
	return data, nil
}

// readBlockAt is the §4.5 buffered raw-device read behind ReadBlock, and stays
// unexported. WriteBlockAt is the buffered pwrite + fdatasync behind
// WriteBlock, and is exported because the §4.5.1 probe write half calls it
// directly. Neither uses O_DIRECT, and neither ever shells out to dd.
func readBlockAt(path string, offset uint64, length uint64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Bound-check before allocating: os.Stat reports 0 for a block device,
	// so the size comes from a seek to the end.
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if offset+length > uint64(size) || offset+length < offset {
		return nil, fmt.Errorf(
			"short read: %s offset=%d length=%d size=%d",
			path, offset, length, size)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(io.NewSectionReader(
		f, int64(offset), int64(length)), data); err != nil {
		return nil, err
	}
	return data, nil
}

func WriteBlockAt(path string, offset uint64, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(data, int64(offset)); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
```

## 6. Test double — `common/osclient_fake.go` (complete)

Exported (not `_test.go`) so agent/worker/gateway tests in other packages can
reuse it. Unset function fields default to success. There is no
`ReadBlockDirectFn`: the probe read left the interface with `update_01.md` U2,
and probe IO is faked through the cn agent's own probe-IO dependency
(`cnagent.md` §4.2 / §6 test 15), not through this double.

```go
package common

import (
	"context"

	"google.golang.org/protobuf/proto"
)

// FakeOsClient is a configurable OsClient test double: set only the function
// fields your test needs; unset fields succeed with zero values.
type FakeOsClient struct {
	RunCommandFn      func(ctx context.Context, name string, args []string, stdinInput string) (string, string, int, error)
	ReadFileFn        func(ctx context.Context, path string) (string, error)
	WriteFileFn       func(ctx context.Context, path string, data string) error
	WriteFileDirectFn func(ctx context.Context, path string, data string) error
	ReadBlockFn       func(ctx context.Context, path string, offset uint64, length uint64) ([]byte, error)
	WriteBlockFn      func(ctx context.Context, path string, offset uint64, data []byte) error
	ReadProtoFn       func(ctx context.Context, path string, target proto.Message) error
	WriteProtoFn      func(ctx context.Context, path string, msg proto.Message) error
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

func (f *FakeOsClient) WriteFileDirect(ctx context.Context, path string, data string) error {
	if f.WriteFileDirectFn != nil {
		return f.WriteFileDirectFn(ctx, path, data)
	}
	return nil
}

func (f *FakeOsClient) ReadBlock(ctx context.Context, path string, offset uint64, length uint64) ([]byte, error) {
	if f.ReadBlockFn != nil {
		return f.ReadBlockFn(ctx, path, offset, length)
	}
	return nil, nil
}

func (f *FakeOsClient) WriteBlock(ctx context.Context, path string, offset uint64, data []byte) error {
	if f.WriteBlockFn != nil {
		return f.WriteBlockFn(ctx, path, offset, data)
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
{"time":"2026-08-28T10:00:01.000Z","level":"INFO","msg":"os command","cmd":"dmsetup","args":["create","dnv-ebada5168620c5fe-0000000000000003-4-0000000000000011-0000000000000016"],"stdin":"0 20480 linear 253:0 524288\n","stdout":"","stderr":"","exit_code":0,"trace_id":"a1b2c3d4e5f60718"}
{"time":"2026-08-28T10:00:01.050Z","level":"INFO","msg":"os write proto","path":"/var/tmp/side-ebada5168620c5fe-0000000000000003-0000000000000011-0000000000000016","size":34,"data":{"cluster_id":16981786240730056190,"dn_id":3,"side_pointer":{"sp_id":17,"leg_id":21,"side_id":22},"revision":9,"side_conf":{"ext_cnt":10,"cntlid_slot":1,"primary_cn_id":5,"standby_id_list":[6]}},"trace_id":"a1b2c3d4e5f60718"}
```

```json
{"time":"2026-08-28T10:00:02.100Z","level":"INFO","msg":"os read block","path":"/dev/loop0","offset":0,"length":4096,"trace_id":"a1b2c3d4e5f60718"}
{"time":"2026-08-28T10:00:02.140Z","level":"INFO","msg":"os write block","path":"/dev/loop0","offset":4194304,"length":4096,"trace_id":"a1b2c3d4e5f60718"}
```

One CN11 probe attempt against a kind-`9` leg wrapper — emitted by the prober
itself, not by an `OsClient` (§4.5.1). Both halves carry the *same* trace id,
the fresh per-attempt id of `cnagent.md` CN2:

```json
{"time":"2026-08-28T10:00:03.010Z","level":"INFO","msg":"probe write block","path":"/dev/mapper/dnv-ebada5168620c5fe-0000000000000005-9-0000000000000011-0000000000000015","offset":0,"length":4096,"trace_id":"77f0c2b9a1d3e408"}
{"time":"2026-08-28T10:00:03.014Z","level":"INFO","msg":"probe read block direct","path":"/dev/mapper/dnv-ebada5168620c5fe-0000000000000005-9-0000000000000011-0000000000000015","offset":0,"length":4096,"trace_id":"77f0c2b9a1d3e408"}
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
   call of each method, assert the `msg` strings and required attrs of §4.6,
   assert file `data` is truncated at `LogStrDataLimit` characters for a long
   string, and assert a proto containing a `bytes` field logs `"<N bytes>"`.
8. **Direct write**: `WriteFileDirect` creates a missing file and overwrites
   an existing one in place, leaving no `*.tmp-*` file behind; its log
   record uses msg `os write file direct`.
9. **Block round-trip**: against a pre-sized backing file, `WriteBlock` at
   two offsets then `ReadBlock` returns the same bytes; overwriting part of
   an existing region replaces exactly that region and leaves its
   neighbours (and the header at offset 0) untouched; an untouched region
   reads as zeros. A short read — `length` past the end of the file, or an
   `offset` past it — is an **error**, rejected *before* the buffer is
   allocated so a nonsense length cannot become an OOM; a missing path fails
   on either method. The `os read block` / `os write block` records carry
   `path`/`offset`/`length` and **no** `data` attribute. The fake dispatches
   both methods and returns zero values when the fn fields are unset.
10. **Direct read helper** (`common.ReadBlockDirectAt` — no `OsClient`
    involved): against a pre-sized backing file, `WriteBlock` then
    `ReadBlockDirectAt` at an aligned offset returns the same bytes (tmpfs
    rejects O_DIRECT — run against a file on a real filesystem, and skip
    with a diagnostic if the open fails with `EINVAL`); a misaligned
    `offset` or `length` (or `length == 0`) is rejected **before** the
    open, so a missing path is irrelevant to the rejection; a read past the
    end is a short read, i.e. an error. The helper emits **no** log record —
    assert the captured handler is empty after the call (`os read block
    direct` no longer exists anywhere in `common`). The `OsClient` interface,
    `LimitedOsClient` and `FakeOsClient` no longer carry
    `ReadBlockDirect`/`ReadBlockDirectFn` at all, so there is no
    interface-level dispatch test for it.
11. **Exported write helper**: `common.WriteBlockAt` is exercised by item 9
    through `WriteBlock`; assert additionally that calling it directly writes
    and fdatasyncs without emitting any record (the `os write block` record
    belongs to the `OsClient` wrapper alone).

Acceptance: `go vet ./common/...` and `go test ./common/...` pass;
`DefaultOsClientLimit` exists in `constants.go`; repo-wide grep shows no
`os/exec` usage outside `common/osclient.go`;
`grep -rn "ReadBlockDirect" common/` hits only the exported helper
`ReadBlockDirectAt` (and its test) — the `OsClient` interface,
`LimitedOsClient` and `FakeOsClient` no longer carry the method
(`update_01.md` §7 item 3); `grep -rn "os read block direct" .` finds nothing
outside historical documents.

## 9. Amendments applied to this document

Recorded for traceability; the edits are already applied. Unlike the
"amendments applied to companion documents" sections of `dnagent.md` §5 and
`cnagent.md` §5, this one records edits made **to this document**.

* `update_01.md` U2 (U2-T4) — **`ReadBlockDirect` removed** from the `OsClient`
  interface (§2), from `LimitedOsClient` (§5) and from `FakeOsClient` (§6). Its
  two raw bodies are exported instead as the package-level helpers
  `common.WriteBlockAt` / `common.ReadBlockDirectAt`, which take no `ctx`, no
  semaphore slot and log nothing. §4.5.1 was rewritten from a method
  specification into the **probe-IO carve-out**: probe IO is the one sanctioned
  direct-syscall path in dnv, it may block indefinitely by design, and it must
  never run under a lock or through the semaphore (a wedged probe would
  otherwise pin one of the 32 `DefaultOsClientLimit` slots, and ≥ 32 of them
  starve the node — including the `nvme disconnect` that releases them). §1's
  "never call `os/exec` or `os.ReadFile`/`os.WriteFile` directly" absolute
  gained that same carve-out, and §4.1's slot count now excludes the helpers.
  §4.6 lost the `ReadBlockDirect` row and gained the prober-emitted
  `probe write block` / `probe read block direct` records; §7 shows a probe
  attempt; §8 test 10 was retargeted at the helper and test 11 added. The
  `os read block` / `os write block` rows are deliberately **unchanged** —
  `WriteBlock` is still the dn `diskmeta.go` path and
  `integtest/dnagent_test.sh` greps that record. §4.6's note on the
  `mutations()` grep is past tense about `os read block direct`: with the msg
  emitted by nothing, `integtest/cnagent_test.sh` drops it from that `jq`
  clause too, which is what makes §8's `grep -rn "os read block direct" .`
  acceptance line true outside this and the other narrative documents.
* `update_01.md` U3 — LVM is gone from the whole repo (the CN clone-metadata
  arena became a slot allocator over one loop device, `architecture.md` [D14]),
  so §1's consumer list and §3's rationale no longer name it.
* Earlier amendments, recorded in their originating documents: `dnagent.md` §5
  added `WriteFileDirect` (`os write file direct`); `cnagent.md` §5 added
  `ReadBlockDirect`, which U2 above has now removed again.
